package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	lcconfig "github.com/MMinasyan/lightcode/internal/config"
)

// TestACPOutputIsWrittenOnlyByDrainer proves the single output drainer is the only
// path that writes the ACP stream, so protocol order is the drainer's write order.
func TestACPOutputIsWrittenOnlyByDrainer(t *testing.T) {
	src, err := os.ReadFile("acp.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	decl := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)\(`)
	locs := decl.FindAllStringSubmatchIndex(s, -1)
	for i, loc := range locs {
		name := s[loc[2]:loc[3]]
		end := len(s)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if name != "runOutputDrainer" && strings.Contains(s[loc[1]:end], "r.out.Write") {
			t.Fatalf("r.out.Write is called in %s; only runOutputDrainer may write the ACP stream", name)
		}
	}
}

// TestACPShutdownJoinsOwnerBeforeClosingOutput proves Run joins the owner before
// closing the output, so a turn's terminal events (e.g. turn_end) emitted while the
// owner drains on shutdown are still admitted and delivered rather than dropped.
// The source order is what lets them be admitted at all — a frame enqueued after
// closeOutput is rejected by the closed gate — so the behavioral half forces a
// shutdown-produced frame to be queued when close runs and asserts it is still
// written.
func TestACPShutdownJoinsOwnerBeforeClosingOutput(t *testing.T) {
	src, err := os.ReadFile("acp.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	shut := strings.Index(s, "r.owner.ShutdownOwner()")
	closeOut := strings.Index(s, "r.closeOutput()")
	if shut < 0 || closeOut < 0 || shut > closeOut {
		t.Fatal("Run must join the owner (ShutdownOwner) before closing output so terminal events are delivered on shutdown")
	}

	// The behavioral half: with the drainer held inside one write, a frame the
	// owner's shutdown enqueues (a terminal event) sits queued when closeOutput
	// runs; close must write it, not discard it.
	orig := acpOutputJoinTimeout
	acpOutputJoinTimeout = 50 * time.Millisecond
	defer func() { acpOutputJoinTimeout = orig }()

	out := &blockingACPWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(out.release) }) }
	t.Cleanup(release)

	r := &Runner{out: out}
	r.startOutput()

	r.enqueue([]byte("in-flight\n"))
	select {
	case <-out.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("drainer did not start writing the in-flight frame")
	}

	// The owner drains on shutdown here; the drainer is busy, so the terminal
	// event queues behind the in-flight frame and is still queued when
	// closeOutput runs.
	r.enqueue([]byte("turn_end\n"))
	r.closeOutput() // returns at the join bound; the drainer is still blocked

	release() // the drainer writes the in-flight frame, then the backlog
	select {
	case <-r.outDone:
	case <-time.After(5 * time.Second):
		t.Fatal("drainer did not exit after the backlog drained")
	}
	if got := out.String(); got != "in-flight\nturn_end\n" {
		t.Fatalf("output after close = %q, want the shutdown-produced frame written after the in-flight one", got)
	}
}

// TestACPShutdownAbandonedReturnsError pins Run's teardown fold: when the
// owner reports that shutdown abandoned in-flight work, Run must fold a
// non-nil error into its returned error so a script driving this process
// detects the abandonment from the exit code. Exception, recorded per the
// contract-test rule: Run cannot be driven against a stub owner in a test.
// The owner field is a concrete *agent.Agent — typed for the concrete-only
// surface (ShutdownOwner), not the AdapterService interface — so a stub
// cannot be substituted without changing the field's type to an interface, a
// production change this test must not force. An abandoned shutdown would
// otherwise need the agent-internal coordinator park that
// TestOwnerShutdownContractMatrix drives (join=timeout), which this package
// cannot reach. The fold is therefore pinned structurally against that
// behavioral evidence.
func TestACPShutdownAbandonedReturnsError(t *testing.T) {
	src, err := os.ReadFile("acp.go")
	if err != nil {
		t.Fatalf("read acp.go: %v", err)
	}
	body, ok := extractACPFunctionBody(string(src), "func (r *Runner) Run(")
	if !ok {
		t.Fatal("Run not found")
	}
	// The whole shape is one structure, not separate facts: the fold is
	// guarded by the abandoned outcome, the joined error is assigned into
	// err, and the same err is returned later in the body. An inverted guard,
	// an unconditional fold, or a fold that builds the joined error and
	// discards it would each fail this one pattern while still containing the
	// guard, the join call, the message and the return somewhere in the
	// function.
	guardedFoldIntoReturned := regexp.MustCompile(`!r\.owner\.ShutdownOwner\(\)\s*\{\s*err\s*=\s*errors\.Join\(\s*err\s*,\s*fmt\.Errorf\(\s*"owner shutdown abandoned in-flight work"\s*\)\s*\)\s*\}[\s\S]*?return\s+err\b`)
	if !guardedFoldIntoReturned.MatchString(body) {
		t.Fatal("Run must fold the abandoned outcome into the error it returns as one guarded structure: `if !r.owner.ShutdownOwner() { err = errors.Join(err, fmt.Errorf(\"owner shutdown abandoned in-flight work\")) }` and later `return err`")
	}
}

// extractACPFunctionBody returns the brace-delimited body of the first function
// whose definition line starts with prefix. It does not understand strings or
// comments containing braces, so callers should pass production code only.
func extractACPFunctionBody(source, prefix string) (string, bool) {
	idx := strings.Index(source, prefix)
	if idx < 0 {
		return "", false
	}
	rest := source[idx:]
	open := strings.Index(rest, "{")
	if open < 0 {
		return "", false
	}
	depth := 1
	for i := open + 1; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[open+1 : i], true
			}
		}
	}
	return "", false
}

// TestACPOutputDrainsInOrder proves the drainer writes queued frames in FIFO
// order. Two assertions, neither substituting for the other: with the output open
// and never closed during the assertion, every queued frame is written in order —
// the drain is live, not deferred to close; and frames still queued when
// closeOutput runs are written after close rather than discarded — closing
// refuses new frames but does not discard the backlog.
func TestACPOutputDrainsInOrder(t *testing.T) {
	orig := acpOutputJoinTimeout
	acpOutputJoinTimeout = 50 * time.Millisecond
	defer func() { acpOutputJoinTimeout = orig }()

	t.Run("open_stream=frames_written_while_output_open", func(t *testing.T) {
		const n = 50
		out := &blockingACPWriter{entered: make(chan struct{}), release: make(chan struct{})}
		close(out.release) // writes never block: this assertion drives an open, unblocked stream
		r := &Runner{out: out}
		r.startOutput()
		var want string
		for i := 0; i < n; i++ {
			r.enqueue([]byte("f" + strconv.Itoa(i) + "\n"))
			want += "f" + strconv.Itoa(i) + "\n"
		}
		// The whole queue drains while the output is still open; closeOutput is
		// never called during the assertion. An implementation that writes only
		// up to a held frame and defers the rest to close never reaches the full
		// content and fails here.
		deadline := time.Now().Add(2 * time.Second)
		for out.String() != want {
			if time.Now().After(deadline) {
				t.Fatalf("open stream wrote %d of %d frames; the drain must happen while the output is open, not at close", strings.Count(out.String(), "\n"), n)
			}
			time.Sleep(time.Millisecond)
		}
		if got := out.String(); got != want {
			t.Fatalf("open stream output = %q, want %q", got, want)
		}
		r.mu.Lock()
		closed := r.outClosed
		r.mu.Unlock()
		if closed {
			t.Fatal("the output was closed during the open-stream assertion")
		}
		// The assertion ran with the output open; close only to join the drainer.
		r.closeOutput()
		select {
		case <-r.outDone:
		case <-time.After(2 * time.Second):
			t.Fatal("drainer did not exit after close")
		}
	})

	t.Run("close_with_backlog=frames_written_after_close", func(t *testing.T) {
		const n = 50
		out := &blockingACPWriter{entered: make(chan struct{}), release: make(chan struct{})}
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(out.release) }) }
		t.Cleanup(release)

		r := &Runner{out: out}
		r.startOutput()
		r.enqueue([]byte("f0\n"))
		select {
		case <-out.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("drainer did not start writing the first frame")
		}
		for i := 1; i < n; i++ {
			r.enqueue([]byte("f" + strconv.Itoa(i) + "\n"))
		}
		// Close while 49 frames are still queued: they are the backlog close must
		// drain rather than discard.
		r.closeOutput()
		release()
		select {
		case <-r.outDone:
		case <-time.After(5 * time.Second):
			t.Fatal("drainer did not exit after the backlog drained")
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != n {
			t.Fatalf("got %d lines, want %d: %q", len(lines), n, out.String())
		}
		for i, line := range lines {
			if want := "f" + strconv.Itoa(i); line != want {
				t.Fatalf("line %d = %q, want %q", i, line, want)
			}
		}
	})
}

// TestACPOutputCloseDrainsBacklog proves closing delivery on the protocol host
// writes frames already queued before the drainer exits: the client process is
// still reading the output pipe at close, and the queued frames are the terminal
// events shutdown just produced. The backlog is forced, not raced — the drainer
// is held inside one write while the rest queue behind it. Its nearest forbidden
// sibling: a drainer blocked inside one write is still abandoned at the host's
// existing join bound rather than waited for.
func TestACPOutputCloseDrainsBacklog(t *testing.T) {
	t.Run("queued_frames=written_after_close", func(t *testing.T) {
		orig := acpOutputJoinTimeout
		acpOutputJoinTimeout = 50 * time.Millisecond
		defer func() { acpOutputJoinTimeout = orig }()

		out := &blockingACPWriter{entered: make(chan struct{}), release: make(chan struct{})}
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(out.release) }) }
		t.Cleanup(release)

		r := &Runner{out: out}
		r.startOutput()

		// Frame A is dequeued and blocked inside its write...
		r.enqueue([]byte("A\n"))
		select {
		case <-out.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("drainer did not start writing frame A")
		}
		// ...while B and C sit queued. Close must still write them.
		r.enqueue([]byte("B\n"))
		r.enqueue([]byte("C\n"))
		r.closeOutput()

		release() // the drainer writes A, then the backlog
		select {
		case <-r.outDone:
		case <-time.After(5 * time.Second):
			t.Fatal("drainer did not exit after the backlog drained")
		}
		if got := out.String(); got != "A\nB\nC\n" {
			t.Fatalf("output after close = %q, want the queued backlog written: %q", got, "A\nB\nC\n")
		}
	})

	t.Run("blocked_write=abandoned_at_existing_bound", func(t *testing.T) {
		orig := acpOutputJoinTimeout
		acpOutputJoinTimeout = 100 * time.Millisecond
		defer func() { acpOutputJoinTimeout = orig }()

		out := &blockingACPWriter{entered: make(chan struct{}), release: make(chan struct{})}
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(out.release) }) }
		t.Cleanup(release)

		r := &Runner{out: out}
		r.startOutput()

		r.enqueue([]byte("A\n"))
		select {
		case <-out.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("drainer did not start writing frame A")
		}

		// The write never completes; close must give up at the existing join
		// bound rather than wait for the drainer.
		start := time.Now()
		r.closeOutput()
		elapsed := time.Since(start)
		if elapsed < acpOutputJoinTimeout/2 || elapsed > 2*time.Second {
			t.Fatalf("closeOutput returned in %v; it must abandon the blocked drainer at the join bound (%v)", elapsed, acpOutputJoinTimeout)
		}

		release() // let the abandoned drainer finish and exit
		select {
		case <-r.outDone:
		case <-time.After(5 * time.Second):
			t.Fatal("drainer did not exit after the blocked write was released")
		}
	})
}

// TestACPOwnsConcreteAgentLifecycle proves the runner initializes the concrete
// owner it constructs, establishes a current session, and joins the owner cleanly
// on stdin EOF without hanging.
func TestACPOwnsConcreteAgentLifecycle(t *testing.T) {
	ag := newACPTestAgent(t)
	var out bytes.Buffer
	r := &Runner{agent: ag, owner: ag, in: strings.NewReader(""), out: &out}

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run hung joining the owner after stdin EOF")
	}
}

// blockingReader blocks every Read until release is closed, modeling stdin with no
// input so a Scan stays blocked until shutdown abandons it.
type blockingReader struct{ release <-chan struct{} }

func (b blockingReader) Read(p []byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

// blockingACPWriter signals when the drainer first enters a Write and blocks
// every Write until release closes, so a test can hold the drainer mid-stream
// and force a deterministic backlog at close.
type blockingACPWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (w *blockingACPWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *blockingACPWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestACPStdinReadByOneScanner proves only scanLoop reads r.in, so there is no
// second stdin reader competing for input lines.
func TestACPStdinReadByOneScanner(t *testing.T) {
	src, err := os.ReadFile("acp.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	decl := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)\(`)
	locs := decl.FindAllStringSubmatchIndex(s, -1)
	for i, loc := range locs {
		name := s[loc[2]:loc[3]]
		end := len(s)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if name != "scanLoop" && strings.Contains(s[loc[1]:end], "r.in") {
			t.Fatalf("r.in is read in %s; only scanLoop may read the ACP stdin stream", name)
		}
	}
}

// TestACPCloseRejectsLaterAdmission proves a line read after shutdown closed the
// dispatch gate is not admitted, so it is never parsed or dispatched.
func TestACPCloseRejectsLaterAdmission(t *testing.T) {
	r := &Runner{}
	r.closeDispatch()
	if r.admitDispatch() {
		r.dispatchWG.Done()
		t.Fatal("a line read after dispatch close must not be admitted")
	}
}

// TestACPArchiveDeleteCloseRaceRefuses proves an archive/delete request
// admitted by the ACP host before close but entering the owner after close is
// refused at the owner admission boundary: the dispatch was admitted while the
// owner was still open, but the owner had already published closed by the time
// the handler reached the owner, so the removal must refuse with the
// owner-closed error instead of taking a claim and mutating durably.
func TestACPArchiveDeleteCloseRaceRefuses(t *testing.T) {
	cases := []struct {
		name   string
		handle func(*Runner, Request)
	}{
		{name: "archive", handle: func(r *Runner, req Request) { r.handleSessionArchive(req) }},
		{name: "delete", handle: func(r *Runner, req Request) { r.handleSessionDelete(req) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newACPTestAgent(t)
			id, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			var out bytes.Buffer
			r := &Runner{agent: a, owner: a, out: &out}
			// The archive/delete line is admitted by the host before close.
			if !r.admitDispatch() {
				t.Fatal("dispatch admission refused before close")
			}
			defer r.dispatchWG.Done()

			// The owner publishes close before the admitted dispatch reaches it.
			if !a.ShutdownOwner() {
				t.Fatal("clean shutdown reported abandoned in-flight work")
			}

			req := Request{JSONRPC: "2.0", ID: tc.name, Params: json.RawMessage(`{"id":"` + id + `"}`)}
			tc.handle(r, req)

			lines := drainedLines(t, r, &out, 1)
			var resp Response
			if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
				t.Fatalf("response json: %v", err)
			}
			if resp.Error == nil || !strings.Contains(resp.Error.Message, "owner is shutting down") {
				t.Fatalf("admitted-after-close %s response = %+v, want the owner-closed error", tc.name, resp)
			}
			if resp.Result != nil {
				t.Fatalf("%s response carried a result: %#v", tc.name, resp.Result)
			}
		})
	}
}

// TestACPShutdownJoinsAdmittedDispatchResponse proves shutdown waits for an
// in-flight dispatch and that the dispatch's response is enqueued before shutdown
// completes.
func TestACPShutdownJoinsAdmittedDispatchResponse(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	if !r.admitDispatch() {
		t.Fatal("admitDispatch must succeed before dispatch close")
	}

	teardownDone := make(chan struct{})
	go func() {
		r.closeDispatch()
		r.dispatchWG.Wait()
		close(teardownDone)
	}()

	select {
	case <-teardownDone:
		t.Fatal("shutdown joined before the admitted dispatch finished")
	case <-time.After(50 * time.Millisecond):
	}

	r.processLine(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	r.dispatchWG.Done()

	select {
	case <-teardownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not complete after the admitted dispatch finished")
	}

	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error != nil || resp.Result == nil {
		t.Fatalf("admitted dispatch response = %+v, want a success result", resp)
	}
}

// TestACPShutdownJoinsAdmittedDispatchParseError proves a malformed admitted line
// still enqueues its parse-error response before shutdown completes.
func TestACPShutdownJoinsAdmittedDispatchParseError(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	if !r.admitDispatch() {
		t.Fatal("admitDispatch must succeed before dispatch close")
	}

	teardownDone := make(chan struct{})
	go func() {
		r.closeDispatch()
		r.dispatchWG.Wait()
		close(teardownDone)
	}()

	select {
	case <-teardownDone:
		t.Fatal("shutdown joined before the admitted parse-error finished")
	case <-time.After(50 * time.Millisecond):
	}

	r.processLine(context.Background(), []byte(`{not json`))
	r.dispatchWG.Done()

	select {
	case <-teardownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not complete after the admitted parse-error finished")
	}

	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32700 {
		t.Fatalf("admitted parse-error response = %+v, want code -32700", resp)
	}
}

// TestACPShutdownReturnsWithScanBlocked proves shutdown tears down and Run returns
// even while a Scan is blocked on stdin; the blocked reader is abandoned.
func TestACPShutdownReturnsWithScanBlocked(t *testing.T) {
	ag := newACPTestAgent(t)
	release := make(chan struct{})
	var out bytes.Buffer
	r := &Runner{agent: ag, owner: ag, in: blockingReader{release: release}, out: &out}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	time.Sleep(100 * time.Millisecond) // let Run reach the blocked Scan
	cancel()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return while a Scan was blocked on stdin")
	}
	close(release) // release the abandoned reader goroutine
}

func TestDispatchInitializeAndUnknownMethod(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: float64(2), Method: "missing/method"})

	lines := drainedLines(t, r, &out, 2)
	var initResp Response
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("initialize response json: %v", err)
	}
	if initResp.JSONRPC != "2.0" || initResp.Error != nil {
		t.Fatalf("initialize response = %+v", initResp)
	}
	result, ok := initResp.Result.(map[string]any)
	if !ok || result["protocolVersion"].(float64) != 1 {
		t.Fatalf("initialize result = %#v", initResp.Result)
	}

	var errResp Response
	if err := json.Unmarshal([]byte(lines[1]), &errResp); err != nil {
		t.Fatalf("unknown response json: %v", err)
	}
	if errResp.Error == nil || errResp.Error.Code != -32601 || !strings.Contains(errResp.Error.Message, "missing/method") {
		t.Fatalf("unknown method response = %+v", errResp)
	}
}

func TestWireHelpers(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.respond("id-1", map[string]any{"ok": true})
	r.respondError("id-2", -32602, "bad params")
	r.sendNotification(Notification{JSONRPC: "2.0", Method: "agent/test", Params: map[string]any{"x": 1}}, "")

	lines := drainedLines(t, r, &out, 3)
	if !strings.Contains(lines[0], `"jsonrpc":"2.0"`) || !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("response is not newline-terminated JSON-RPC: %q", out.String())
	}
	var resp Response
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("error response json: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 || resp.Error.Message != "bad params" {
		t.Fatalf("error response = %+v", resp)
	}
	var notif Notification
	if err := json.Unmarshal([]byte(lines[2]), &notif); err != nil {
		t.Fatalf("notification json: %v", err)
	}
	if notif.Method != "agent/test" {
		t.Fatalf("notification = %+v", notif)
	}
}

func TestHandleEventNotifications(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, Result: "hello"})
	r.handleEvent(agent.Event{Kind: agent.EventToolCallStart, ToolCallID: "tc1", ToolName: "read_file", Args: `{"path":"x"}`})
	r.handleEvent(agent.Event{Kind: agent.EventToolCallEnd, ToolCallID: "tc1", ToolName: "read_file", Args: `{"path":"x"}`, Result: "done"})
	r.handleEvent(agent.Event{
		Kind:   agent.EventBackgroundProcessComplete,
		Result: "done",
		BackgroundProcess: &agent.BackgroundProcessDisplay{
			ID:       "bg-1",
			Command:  "printf done",
			Reason:   "completed",
			ExitCode: 0,
			Output:   "done",
		},
	})
	r.handleEvent(agent.Event{Kind: agent.EventWarning, Warnings: []agent.PromptWarning{{Kind: "catalog_discovery_failure", Message: "test: failed"}}})
	r.handleEvent(agent.Event{Kind: agent.EventWarning})
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, Result: "skip", SubagentSessionID: "sub"})

	lines := drainedLines(t, r, &out, 6)
	wantMethods := []string{"agent/message_chunk", "agent/tool_start", "agent/tool_result", "agent/background_process_complete", "agent/warnings", "agent/warnings"}
	for i, want := range wantMethods {
		var got Notification
		if err := json.Unmarshal([]byte(lines[i]), &got); err != nil {
			t.Fatalf("notification[%d] json: %v", i, err)
		}
		if got.Method != want {
			t.Fatalf("notification[%d].Method = %q, want %q", i, got.Method, want)
		}
		if got.Method == "agent/warnings" && i == 4 {
			data, err := json.Marshal(got.Params)
			if err != nil {
				t.Fatalf("warning params marshal: %v", err)
			}
			var warnings []agent.PromptWarning
			if err := json.Unmarshal(data, &warnings); err != nil {
				t.Fatalf("warning params json: %v", err)
			}
			if len(warnings) != 1 || warnings[0].Kind != "catalog_discovery_failure" || warnings[0].Message != "test: failed" {
				t.Fatalf("warning params = %#v, want kind/message", warnings)
			}
		}
		if got.Method == "agent/tool_result" {
			params, ok := got.Params.(map[string]any)
			if !ok || params["args"] != `{"path":"x"}` {
				t.Fatalf("tool_result params = %#v, want args", got.Params)
			}
		}
		if got.Method == "agent/warnings" && i == 5 {
			data, err := json.Marshal(got.Params)
			if err != nil {
				t.Fatalf("empty warning params marshal: %v", err)
			}
			if string(data) != "[]" {
				t.Fatalf("empty warning params = %s, want []", data)
			}
		}
	}
}

func TestHandleEventCarriesSequence(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, Seq: 5, Result: "hi"})
	r.handleEvent(agent.Event{Kind: agent.EventToolCallStart, Seq: 6, ToolCallID: "tc1", ToolName: "read_file"})
	r.handleEvent(agent.Event{Kind: agent.EventToolCallEnd, Seq: 7, ToolCallID: "tc1", ToolName: "read_file", Result: "done"})
	r.handleEvent(agent.Event{Kind: agent.EventUserMessageDisplay, Seq: 8, Turn: 1, Result: "u"})
	r.handleEvent(agent.Event{Kind: agent.EventGenericSystemSignal, Seq: 9, Result: "s"})
	r.handleEvent(agent.Event{Kind: agent.EventError, Seq: 10, Error: "boom", Turn: 1})
	r.handleEvent(agent.Event{
		Kind: agent.EventBackgroundProcessComplete, Seq: 11,
		BackgroundProcess: &agent.BackgroundProcessDisplay{ID: "bg-1", Command: "x", Reason: "completed"},
	})

	lines := drainedLines(t, r, &out, 7)
	// Every transcript row carries its sequence so the client can gate live
	// items against the navigation boundary high-water. tool_result is the
	// exception: it updates the id-keyed row started by tool_start.
	wantSeq := map[string]float64{
		"agent/message_chunk":               5,
		"agent/tool_start":                  6,
		"agent/user_message":                8,
		"agent/system_signal":               9,
		"agent/error":                       10,
		"agent/background_process_complete": 11,
	}
	for i, line := range lines {
		var got Notification
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("notification[%d] json: %v", i, err)
		}
		params, ok := got.Params.(map[string]any)
		if !ok {
			t.Fatalf("notification[%d] %s params not object: %#v", i, got.Method, got.Params)
		}
		if want, seqBearing := wantSeq[got.Method]; seqBearing {
			if params["seq"] != want {
				t.Fatalf("%s seq = %#v, want %v", got.Method, params["seq"], want)
			}
		}
		if got.Method == "agent/tool_result" {
			if _, present := params["seq"]; present {
				t.Fatalf("tool_result must not carry seq (id-keyed update), got %#v", params["seq"])
			}
		}
	}
}

// TestHandleEventErrorOmitsSequenceWhenUnsequenced verifies the agent/error
// notification carries a seq field only when the error event was sequenced by a
// transcript. A sessionless error is emitted directly and never sequenced
// (Seq 0); its notification must omit the field, because a client gate that
// reads the field's absence as "unsequenced" would reject a zero-stamped seq
// against every snapshot high-water and the user would see nothing.
func TestHandleEventErrorOmitsSequenceWhenUnsequenced(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.handleEvent(agent.Event{Kind: agent.EventError, Error: "sessionless", Turn: 1})
	r.handleEvent(agent.Event{Kind: agent.EventError, Seq: 5, Error: "sequenced", Turn: 2})

	lines := drainedLines(t, r, &out, 2)
	for i, line := range lines {
		var got Notification
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("notification[%d] json: %v", i, err)
		}
		if got.Method != "agent/error" {
			t.Fatalf("notification[%d] method = %q, want agent/error", i, got.Method)
		}
		params, ok := got.Params.(map[string]any)
		if !ok {
			t.Fatalf("notification[%d] %s params not object: %#v", i, got.Method, got.Params)
		}
		if i == 0 {
			if _, present := params["seq"]; present {
				t.Fatalf("sessionless error notification carries seq %#v; the field must be omitted", params["seq"])
			}
			if params["message"] != "sessionless" || params["turn"] != float64(1) {
				t.Fatalf("sessionless error params = %#v, want message %q and turn 1", params, "sessionless")
			}
		} else {
			if params["seq"] != float64(5) {
				t.Fatalf("sequenced error notification seq = %#v, want 5", params["seq"])
			}
			if params["message"] != "sequenced" || params["turn"] != float64(2) {
				t.Fatalf("sequenced error params = %#v, want message %q and turn 2", params, "sequenced")
			}
		}
	}
}

// TestACPUsageAppliesCumulativeWithoutOwnerQuery proves the usage callback applies
// the event's absolute cumulative report as a replacement and never queries the
// owner: the runner has no agent, so any owner query would panic.
func TestACPUsageAppliesCumulativeWithoutOwnerQuery(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.seedPresented("s")
	report := agent.TokenReport{ContextUsed: 1234, ContextWindow: 8000}
	r.handleEvent(agent.Event{Kind: agent.EventUsage, SessionID: "s", CumulativeTokens: &report})

	lines := drainedLines(t, r, &out, 1)
	assertACPNotificationMethod(t, lines[0], "agent/usage")
	var notif Notification
	if err := json.Unmarshal([]byte(lines[0]), &notif); err != nil {
		t.Fatalf("usage notification json: %v", err)
	}
	data, err := json.Marshal(notif.Params)
	if err != nil {
		t.Fatalf("usage params marshal: %v", err)
	}
	var got agent.TokenReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("usage params json: %v", err)
	}
	if got.ContextUsed != 1234 || got.ContextWindow != 8000 {
		t.Fatalf("usage = %#v, want the event's cumulative report applied without a query", got)
	}
}

// TestHandleEventFiltersSession proves the drainer writes a session-tagged event
// only while its session is presentation-current: a wrong-session event is dropped,
// the presentation-current session's event is written, and a boundary that detaches
// presentation current drops the session's subsequent events.
func TestHandleEventFiltersSession(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.seedPresented("session-a")

	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: "session-b", Result: "skip"})
	r.drainForTest()
	if out.Len() != 0 {
		t.Fatalf("wrong-session event was emitted: %q", out.String())
	}

	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: "session-a", Result: "keep"})
	lines := drainedLines(t, r, &out, 1)
	assertACPNotificationMethod(t, lines[0], "agent/message_chunk")

	out.Reset()
	// Detach presentation current through a boundary; the session's events now drop.
	r.enqueueFrame(outFrame{kind: frameAdvance, sessionID: ""})
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: "session-a", Result: "skip"})
	r.drainForTest()
	if out.Len() != 0 {
		t.Fatalf("event after detach was emitted: %q", out.String())
	}
}

func TestHandleEventNotifiesUserMessageAndSystemSignal(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}

	r.handleEvent(agent.Event{Kind: agent.EventUserMessageDisplay, Turn: 4, Result: "hello"})
	r.handleEvent(agent.Event{Kind: agent.EventGenericSystemSignal, Turn: 4, Result: "Model switched to x/y"})

	lines := drainedLines(t, r, &out, 2)
	var um Notification
	if err := json.Unmarshal([]byte(lines[0]), &um); err != nil {
		t.Fatalf("user_message json: %v", err)
	}
	if um.Method != "agent/user_message" {
		t.Fatalf("user_message method = %q", um.Method)
	}
	umData, _ := json.Marshal(um.Params)
	if !strings.Contains(string(umData), `"content":"hello"`) || !strings.Contains(string(umData), `"turn":4`) {
		t.Fatalf("user_message params = %s", umData)
	}

	var ss Notification
	if err := json.Unmarshal([]byte(lines[1]), &ss); err != nil {
		t.Fatalf("system_signal json: %v", err)
	}
	if ss.Method != "agent/system_signal" {
		t.Fatalf("system_signal method = %q", ss.Method)
	}
	ssData, _ := json.Marshal(ss.Params)
	if !strings.Contains(string(ssData), `"content":"System: Model switched to x/y"`) {
		t.Fatalf("system_signal params = %s", ssData)
	}
}

func TestHandleEventNotifiesQueueChanged(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}

	r.handleEvent(agent.Event{
		Kind:         agent.EventQueueChanged,
		Queue:        []agent.QueuedItem{{ID: "q-1", Content: "hi"}},
		QueueVersion: 2,
	})
	r.handleEvent(agent.Event{
		Kind:         agent.EventQueueChanged,
		QueueVersion: 3,
	})
	lines := drainedLines(t, r, &out, 2)
	var qc Notification
	if err := json.Unmarshal([]byte(lines[0]), &qc); err != nil {
		t.Fatalf("queue_changed json: %v", err)
	}
	if qc.Method != "agent/queue_changed" {
		t.Fatalf("queue_changed method = %q", qc.Method)
	}
	data, _ := json.Marshal(qc.Params)
	if !strings.Contains(string(data), `"version":2`) || !strings.Contains(string(data), `"content":"hi"`) {
		t.Fatalf("queue_changed params = %s", data)
	}
	var empty Notification
	if err := json.Unmarshal([]byte(lines[1]), &empty); err != nil {
		t.Fatalf("empty queue_changed json: %v", err)
	}
	emptyData, _ := json.Marshal(empty.Params)
	if !strings.Contains(string(emptyData), `"version":3`) || !strings.Contains(string(emptyData), `"items":[]`) {
		t.Fatalf("empty queue_changed params = %s", emptyData)
	}
}

func TestHandleEventTurnEndIncludesCancelled(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}

	r.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Turn: 3, Cancelled: true})
	r.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Turn: 5, Cancelled: false})

	lines := drainedLines(t, r, &out, 2)
	for i, expectCancelled := range []bool{true, false} {
		var n Notification
		if err := json.Unmarshal([]byte(lines[i]), &n); err != nil {
			t.Fatalf("turn_end[%d] json: %v", i, err)
		}
		if n.Method != "agent/turn_end" {
			t.Fatalf("turn_end[%d] method = %q", i, n.Method)
		}
		data, _ := json.Marshal(n.Params)
		want := `"cancelled":false`
		if expectCancelled {
			want = `"cancelled":true`
		}
		if !strings.Contains(string(data), want) {
			t.Fatalf("turn_end[%d] params = %s, want %s", i, data, want)
		}
	}
}

func TestHandleEventCompactionEndPushesSessionChanged(t *testing.T) {
	var out bytes.Buffer
	a := newACPTestAgent(t)
	_ = appendACPUserTurn(t, a, "seed")
	r := &Runner{agent: a, out: &out}
	sessionID := a.SessionCurrent().ID
	r.setCurrentSessionID(sessionID)
	r.seedPresented(sessionID)

	// compaction_end alone notifies only compaction_end; the replacement transcript
	// arrives as the separate rewrite boundary.
	r.handleEvent(agent.Event{Kind: agent.EventCompactionEnd, SessionID: sessionID})
	lines := drainedLines(t, r, &out, 1)
	assertACPNotificationMethod(t, lines[0], "agent/compaction_end")

	out.Reset()
	payload, err := a.SessionPayloadForSession(sessionID)
	if err != nil {
		t.Fatalf("SessionPayloadForSession: %v", err)
	}
	r.handleEvent(agent.Event{Kind: agent.EventSessionRewrite, SessionID: sessionID, RewritePayload: &payload})
	lines = drainedLines(t, r, &out, 1)
	assertACPNotificationMethod(t, lines[0], "agent/session_resync")
}

func TestHandleEventActiveCompactionDefersSessionChangedUntilTurnEnd(t *testing.T) {
	a := newACPTestAgent(t)
	_ = appendACPUserTurn(t, a, "complete before compaction")
	turn := a.Store().BeginTurn()
	for _, raw := range []string{
		`{"role":"user","content":"active prompt"}`,
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}`,
		`{"role":"tool","tool_call_id":"call_1","name":"read_file","content":"ok"}`,
	} {
		if err := a.Store().AppendMessage(turn, []byte(raw)); err != nil {
			t.Fatalf("AppendMessage active: %v", err)
		}
	}
	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	sessionID := a.SessionCurrent().ID
	r.setCurrentSessionID(sessionID)
	r.seedPresented(sessionID)

	r.handleEvent(agent.Event{Kind: agent.EventCompactionEnd, SessionID: sessionID})
	lines := drainedLines(t, r, &out, 1)
	assertACPNotificationMethod(t, lines[0], "agent/compaction_end")

	if err := a.Store().MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete active: %v", err)
	}
	r.drainForTest()
	out.Reset()
	// The replacement is published as the rewrite boundary at compaction success,
	// not deferred onto turn_end; turn_end itself carries no resync.
	payload, err := a.SessionPayloadForSession(sessionID)
	if err != nil {
		t.Fatalf("SessionPayloadForSession: %v", err)
	}
	r.handleEvent(agent.Event{Kind: agent.EventSessionRewrite, SessionID: sessionID, RewritePayload: &payload})
	lines = drainedLines(t, r, &out, 1)
	assertACPNotificationMethod(t, lines[0], "agent/session_resync")
	if !strings.Contains(lines[0], "active prompt") {
		t.Fatalf("session_resync omitted completed active turn: %s", lines[0])
	}

	out.Reset()
	r.handleEvent(agent.Event{Kind: agent.EventTurnEnd, SessionID: sessionID, Turn: turn})
	lines = drainedLines(t, r, &out, 1)
	assertACPNotificationMethod(t, lines[0], "agent/turn_end")
}

func TestDispatchWarningsCurrentReturnsCurrentWarningSnapshot(t *testing.T) {
	a := newACPWarningTestAgent(t)
	if len(a.CurrentWarnings()) == 0 {
		t.Fatal("warning test agent has empty startup warning snapshot")
	}
	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}

	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "warnings", Method: "warnings/current"})

	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("warnings/current error = %+v", resp.Error)
	}
	warnings := promptWarningsFromResponse(t, resp)
	if !hasPromptWarningKind(warnings, "catalog_discovery_failure") {
		t.Fatalf("warnings = %#v, want catalog_discovery_failure", warnings)
	}
}

func TestDispatchWarningsCurrentReturnsEmptyArrayForNoWarnings(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{agent: newACPTestAgent(t), out: &out}

	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "warnings", Method: "warnings/current"})

	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("empty warnings result = %s, want []", data)
	}
}

func TestPermissionRespondMissingAction(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{out: &out}
	r.handlePermissionRespond(Request{JSONRPC: "2.0", ID: "p", Params: json.RawMessage(`{"id":"perm"}`)})
	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 || resp.Error.Message != "missing permission action" {
		t.Fatalf("permission missing-action response = %+v", resp)
	}
}

func TestHandleTurnActionACPRevertCodeReturnsResultWithoutSessionChanged(t *testing.T) {
	a := newACPTestAgent(t)
	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}

	_ = appendACPUserTurn(t, a, "first")
	r.setCurrentSessionID(a.SessionCurrent().ID)
	path := filepath.Join(a.ProjectRoot(), "created.txt")
	clickedTurn := appendACPUserTurnWithSnapshot(t, a, "create file", path, "created\n")

	r.handleRevertCode(Request{
		JSONRPC: "2.0",
		ID:      "revert-code",
		Params:  json.RawMessage(`{"turn":` + itoa(clickedTurn) + `,"alsoRevertCode":true}`),
	})

	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	result := turnActionResultFromResponse(t, resp)
	if resp.Error != nil || result.Action != agent.TurnActionRevertCode || result.Turn != clickedTurn || result.TargetTurn != clickedTurn-1 || result.SessionChanged {
		t.Fatalf("response/result = %+v %#v, want revert_code result without session change", resp, result)
	}
	if len(result.Messages) != 0 {
		t.Fatalf("revert_code messages len = %d, want no hydrated messages", len(result.Messages))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file still exists after revert code; stat err=%v", err)
	}
}

func TestHandleTurnActionACPRevertHistoryPropagatesAlsoRevertCode(t *testing.T) {
	a := newACPTestAgent(t)
	var out bytes.Buffer
	r := &Runner{agent: a, owner: a, out: &out}

	_ = appendACPUserTurn(t, a, "first")
	r.setCurrentSessionID(a.SessionCurrent().ID)
	path := filepath.Join(a.ProjectRoot(), "created.txt")
	clickedTurn := appendACPUserTurnWithSnapshot(t, a, "create file", path, "created\n")
	_ = appendACPUserTurn(t, a, "after")

	r.handleRevertHistory(Request{
		JSONRPC: "2.0",
		ID:      "revert-history",
		Params:  json.RawMessage(`{"turn":` + itoa(clickedTurn) + `,"alsoRevertCode":true}`),
	})

	lines := drainedLines(t, r, &out, 2)
	assertACPNotificationMethod(t, lines[0], "agent/session_changed")
	var resp Response
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	result := turnActionResultFromResponse(t, resp)
	if resp.Error != nil || result.Action != agent.TurnActionRevertHistory || result.TargetTurn != clickedTurn-1 || !result.SessionChanged || result.Prefill != "create file" {
		t.Fatalf("response/result = %+v %#v, want revert_history result", resp, result)
	}
	if got := acpUserMessageContents(result.Messages); !equalStringSlices(got, []string{"first"}) {
		t.Fatalf("result messages = %q, want truncated history", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file still exists after history+code revert; stat err=%v", err)
	}
}

func TestHandleTurnActionACPForkReturnsResultAndSessionChanged(t *testing.T) {
	a := newACPTestAgent(t)
	var out bytes.Buffer
	r := &Runner{agent: a, owner: a, out: &out}

	_ = appendACPUserTurn(t, a, "first")
	r.setCurrentSessionID(a.SessionCurrent().ID)
	clickedTurn := appendACPUserTurn(t, a, "fork point")
	_ = appendACPUserTurn(t, a, "after")
	beforeID := a.SessionCurrent().ID

	r.handleSessionFork(Request{
		JSONRPC: "2.0",
		ID:      "fork",
		Params:  json.RawMessage(`{"turn":` + itoa(clickedTurn) + `}`),
	})

	lines := drainedLines(t, r, &out, 2)
	assertACPNotificationMethod(t, lines[0], "agent/session_changed")
	var resp Response
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	result := turnActionResultFromResponse(t, resp)
	if resp.Error != nil || result.Action != agent.TurnActionFork || result.TargetTurn != clickedTurn || !result.SessionChanged || result.Session.ID == "" || result.Session.ID == beforeID {
		t.Fatalf("response/result = %+v %#v, want fork result with new session", resp, result)
	}
	if got := acpUserMessageContents(result.Messages); !equalStringSlices(got, []string{"first", "fork point"}) {
		t.Fatalf("fork messages = %q, want selected turn included", got)
	}
}

// TestHandleTurnActionACPForkWarningOnFailedCodeRevert proves the protocol
// response for a fork whose best-effort code revert failed still returns the
// fork's result — success — and carries the failed revert as the result's
// warning: the host must not turn the published fork into an error response.
func TestHandleTurnActionACPForkWarningOnFailedCodeRevert(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	a := newACPTestAgent(t)
	var out bytes.Buffer
	r := &Runner{agent: a, owner: a, out: &out}

	_ = appendACPUserTurn(t, a, "first")
	r.setCurrentSessionID(a.SessionCurrent().ID)
	clickedTurn := appendACPUserTurn(t, a, "fork point")
	sub := filepath.Join(a.ProjectRoot(), "sub")
	path := filepath.Join(sub, "created-after-fork.txt")
	_ = appendACPUserTurnWithSnapshot(t, a, "create after fork", path, "later\n")
	beforeID := a.SessionCurrent().ID
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(sub, 0o700) }()

	r.handleSessionFork(Request{
		JSONRPC: "2.0",
		ID:      "fork",
		Params:  json.RawMessage(`{"turn":` + itoa(clickedTurn) + `,"alsoRevertCode":true}`),
	})

	lines := drainedLines(t, r, &out, 2)
	assertACPNotificationMethod(t, lines[0], "agent/session_changed")
	var resp Response
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	result := turnActionResultFromResponse(t, resp)
	if resp.Error != nil {
		t.Fatalf("a failed best-effort code revert must not fail the fork response: %+v", resp.Error)
	}
	if result.Action != agent.TurnActionFork || result.Session.ID == "" || result.Session.ID == beforeID {
		t.Fatalf("response/result = %+v %#v, want successful fork result with new session", resp, result)
	}
	if result.Warning == "" || !strings.Contains(result.Warning, "code revert failed") {
		t.Fatalf("fork response must carry the failed code revert warning, got %q", result.Warning)
	}
}

// TestACPOrderedDelivery proves that every boundary-producing lifecycle
// operation enqueues its session boundary before its success response (the nearest
// forbidden sibling is response-before-boundary), and that a preparation/mutation
// failure enqueues only its error response and no boundary. revert_code is the one
// lifecycle op that emits no boundary by design, so its success is response-only.
func TestACPOrderedDelivery(t *testing.T) {
	cases := []struct {
		name          string
		emitsBoundary bool
		success       func(t *testing.T) (*Runner, *bytes.Buffer, func())
		fail          func(t *testing.T) (*Runner, *bytes.Buffer, func())
	}{
		{
			name:          "session_new",
			emitsBoundary: true,
			success: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				id, err := a.NewSession("", "primary")
				if err != nil {
					t.Fatalf("NewSession: %v", err)
				}
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				r.setCurrentSessionID(id)
				req := Request{JSONRPC: "2.0", ID: "new", Method: "session/new"}
				return r, out, func() { r.handleSessionNew(req) }
			},
		},
		{
			name:          "session_switch",
			emitsBoundary: true,
			success: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				first, err := a.NewSession("", "primary")
				if err != nil {
					t.Fatalf("NewSession first: %v", err)
				}
				if _, err := a.AppendUserMessageToSession(first, "first"); err != nil {
					t.Fatalf("append first: %v", err)
				}
				second, err := a.NewSession("", "primary")
				if err != nil {
					t.Fatalf("NewSession second: %v", err)
				}
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				r.setCurrentSessionID(second)
				req := Request{JSONRPC: "2.0", ID: "switch", Params: json.RawMessage(`{"id":"` + first + `"}`)}
				return r, out, func() { r.handleSessionSwitch(req) }
			},
			fail: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				id, err := a.NewSession("", "primary")
				if err != nil {
					t.Fatalf("NewSession: %v", err)
				}
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				r.setCurrentSessionID(id)
				req := Request{JSONRPC: "2.0", ID: "switch", Params: json.RawMessage(`{"id":"does-not-exist"}`)}
				return r, out, func() { r.handleSessionSwitch(req) }
			},
		},
		{
			name:          "session_archive",
			emitsBoundary: true,
			success: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				if _, err := a.NewSession("", "primary"); err != nil {
					t.Fatalf("NewSession keep: %v", err)
				}
				id, err := a.NewSession("", "primary")
				if err != nil {
					t.Fatalf("NewSession target: %v", err)
				}
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				r.setCurrentSessionID(id)
				req := Request{JSONRPC: "2.0", ID: "archive", Params: json.RawMessage(`{"id":"` + id + `"}`)}
				return r, out, func() { r.handleSessionArchive(req) }
			},
			fail: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				id, err := a.NewSession("", "primary")
				if err != nil {
					t.Fatalf("NewSession: %v", err)
				}
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				r.setCurrentSessionID(id)
				req := Request{JSONRPC: "2.0", ID: "archive", Params: json.RawMessage(`{"id":"does-not-exist"}`)}
				return r, out, func() { r.handleSessionArchive(req) }
			},
		},
		{
			name:          "session_delete",
			emitsBoundary: true,
			success: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				if _, err := a.NewSession("", "primary"); err != nil {
					t.Fatalf("NewSession keep: %v", err)
				}
				id, err := a.NewSession("", "primary")
				if err != nil {
					t.Fatalf("NewSession target: %v", err)
				}
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				r.setCurrentSessionID(id)
				req := Request{JSONRPC: "2.0", ID: "delete", Params: json.RawMessage(`{"id":"` + id + `"}`)}
				return r, out, func() { r.handleSessionDelete(req) }
			},
			fail: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				id, err := a.NewSession("", "primary")
				if err != nil {
					t.Fatalf("NewSession: %v", err)
				}
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				r.setCurrentSessionID(id)
				req := Request{JSONRPC: "2.0", ID: "delete", Params: json.RawMessage(`{"id":"does-not-exist"}`)}
				return r, out, func() { r.handleSessionDelete(req) }
			},
		},
		{
			name:          "session_fork",
			emitsBoundary: true,
			success: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				appendACPUserTurn(t, a, "first")
				r.setCurrentSessionID(a.SessionCurrent().ID)
				clicked := appendACPUserTurn(t, a, "fork point")
				appendACPUserTurn(t, a, "after")
				req := Request{JSONRPC: "2.0", ID: "fork", Params: json.RawMessage(`{"turn":` + itoa(clicked) + `}`)}
				return r, out, func() { r.handleSessionFork(req) }
			},
			fail: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				req := Request{JSONRPC: "2.0", ID: "fork", Params: json.RawMessage(`{"session_id":"does-not-exist","turn":1}`)}
				return r, out, func() { r.handleSessionFork(req) }
			},
		},
		{
			name:          "session_revert_code",
			emitsBoundary: false,
			success: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				appendACPUserTurn(t, a, "first")
				r.setCurrentSessionID(a.SessionCurrent().ID)
				path := filepath.Join(a.ProjectRoot(), "created.txt")
				clicked := appendACPUserTurnWithSnapshot(t, a, "create file", path, "created\n")
				req := Request{JSONRPC: "2.0", ID: "revert-code", Params: json.RawMessage(`{"turn":` + itoa(clicked) + `,"alsoRevertCode":true}`)}
				return r, out, func() { r.handleRevertCode(req) }
			},
			fail: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				req := Request{JSONRPC: "2.0", ID: "revert-code", Params: json.RawMessage(`{"session_id":"does-not-exist","turn":1,"alsoRevertCode":true}`)}
				return r, out, func() { r.handleRevertCode(req) }
			},
		},
		{
			name:          "session_revert_history",
			emitsBoundary: true,
			success: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				appendACPUserTurn(t, a, "first")
				r.setCurrentSessionID(a.SessionCurrent().ID)
				path := filepath.Join(a.ProjectRoot(), "created.txt")
				clicked := appendACPUserTurnWithSnapshot(t, a, "create file", path, "created\n")
				appendACPUserTurn(t, a, "after")
				req := Request{JSONRPC: "2.0", ID: "revert-history", Params: json.RawMessage(`{"turn":` + itoa(clicked) + `,"alsoRevertCode":true}`)}
				return r, out, func() { r.handleRevertHistory(req) }
			},
			fail: func(t *testing.T) (*Runner, *bytes.Buffer, func()) {
				a := newACPTestAgent(t)
				out := new(bytes.Buffer)
				r := &Runner{agent: a, owner: a, out: out}
				req := Request{JSONRPC: "2.0", ID: "revert-history", Params: json.RawMessage(`{"session_id":"does-not-exist","turn":1,"alsoRevertCode":true}`)}
				return r, out, func() { r.handleRevertHistory(req) }
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/success_boundary_before_response", func(t *testing.T) {
			r, out, invoke := tc.success(t)
			invoke()
			if tc.emitsBoundary {
				lines := drainedLines(t, r, out, 2)
				assertACPNotificationMethod(t, lines[0], "agent/session_changed")
				assertACPSuccessResponse(t, lines[1])
			} else {
				lines := drainedLines(t, r, out, 1)
				assertACPSuccessResponse(t, lines[0])
			}
		})
		if tc.fail != nil {
			t.Run(tc.name+"/failure_error_only_no_boundary", func(t *testing.T) {
				r, out, invoke := tc.fail(t)
				invoke()
				lines := drainedLines(t, r, out, 1)
				assertACPErrorResponse(t, lines[0])
			})
		}
	}
}

// TestACPOrderedDeliveryContract is the session/prompt ordered-delivery
// contract. The prompt's implicit switch boundary is invisible on the wire (an
// advance frame with no payload), so it cannot be a row in the table-shaped
// TestACPOrderedDelivery, whose success cells assert a visible boundary before
// the response. Ordering is asserted through the boundary's effect: the
// destination's first frames are delivered, and a submit that fails leaves
// routing and presentation on the previous session.
func TestACPOrderedDeliveryContract(t *testing.T) {
	t.Run("session/prompt=success_advance_ahead_of_first_frames", func(t *testing.T) {
		a := newACPTestAgent(t)
		firstID, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession first: %v", err)
		}
		if _, err := a.AppendUserMessageToSession(firstID, "first"); err != nil {
			t.Fatalf("append first: %v", err)
		}
		secondID, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession second: %v", err)
		}
		if _, err := a.AppendUserMessageToSession(secondID, "second"); err != nil {
			t.Fatalf("append second: %v", err)
		}

		out := new(bytes.Buffer)
		r := &Runner{agent: a, owner: a, out: out}
		a.SetEventHandler(r.handleEvent)
		r.setCurrentSessionID(firstID)
		r.seedPresented(firstID)

		// A live context admits the submit against the dead provider. The turn's
		// first frame (turn_start) is enqueued synchronously inside the submit,
		// ahead of the response; the turn itself fails fast in its goroutine and
		// parks in the flush round-trip (whose consumer only runs after Init), so
		// no further frames follow the response.
		r.handleSessionPrompt(context.Background(), Request{
			JSONRPC: "2.0",
			ID:      "prompt",
			Params:  json.RawMessage(`{"session_id":"` + secondID + `","content":"hi"}`),
		})
		r.drainForTest()
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")

		// The invisible advance is adopted before the first written frame, so the
		// destination's first frame — turn_start, emitted synchronously inside the
		// submit — reaches the client instead of being filtered out, and it is
		// delivered before the response. A commit-after-return ordering drops it.
		turnStartIdx, responseIdx := -1, -1
		for i, line := range lines {
			var frame map[string]any
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				t.Fatalf("frame json: %v", err)
			}
			if m, _ := frame["method"].(string); m == "agent/turn_start" {
				turnStartIdx = i
			}
			if _, ok := frame["id"]; ok {
				responseIdx = i
			}
		}
		if turnStartIdx < 0 {
			t.Fatalf("no turn_start frame reached the client; frames: %q", out.String())
		}
		if responseIdx < 0 {
			t.Fatalf("no success response among frames: %q", out.String())
		}
		if turnStartIdx > responseIdx {
			t.Fatalf("turn_start frame (index %d) delivered after the response (index %d): %q", turnStartIdx, responseIdx, out.String())
		}
		if got := r.currentSessionSummary().ID; got != secondID {
			t.Fatalf("routing current = %q, want %q", got, secondID)
		}
		r.mu.Lock()
		presented := r.presented
		r.mu.Unlock()
		if presented != secondID {
			t.Fatalf("presentation current = %q, want %q", presented, secondID)
		}
	})

	t.Run("session/prompt=failure_routing_and_presentation_unchanged", func(t *testing.T) {
		a := newACPTestAgent(t)
		firstID, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession first: %v", err)
		}
		secondID, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession second: %v", err)
		}

		out := new(bytes.Buffer)
		r := &Runner{agent: a, owner: a, out: out}
		r.setCurrentSessionID(firstID)
		r.seedPresented(firstID)

		// A closed runtime is the submit-side failure the id pre-check cannot
		// see: the session is still resolvable, but rt.submit rejects admission.
		a.ShutdownOwner()

		r.handleSessionPrompt(context.Background(), Request{
			JSONRPC: "2.0",
			ID:      "prompt",
			Params:  json.RawMessage(`{"session_id":"` + secondID + `","content":"hi"}`),
		})
		lines := drainedLines(t, r, out, 1)
		assertACPErrorResponse(t, lines[0])
		// The view's routing id is asserted directly: it is the routing state
		// the failed submit must leave unchanged, and it survives the clean
		// shutdown, whose store detach makes a summary lookup fail and would
		// clear the id on that error.
		got, err := r.currentSession()
		if err != nil {
			t.Fatalf("current session: %v", err)
		}
		if got != firstID {
			t.Fatalf("routing current = %q, want unchanged %q", got, firstID)
		}
		r.mu.Lock()
		presented := r.presented
		r.mu.Unlock()
		if presented != firstID {
			t.Fatalf("presentation current = %q, want unchanged %q", presented, firstID)
		}
	})

	t.Run("session/prompt=success_queued_advance_ahead_of_queue_changed", func(t *testing.T) {
		a := newACPTestAgent(t)
		firstID, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession first: %v", err)
		}
		secondID, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession second: %v", err)
		}

		// Make secondID busy with a real turn against the dead provider. The turn
		// goroutine parks in the flush round-trip (whose consumer only runs after
		// Init), so the unit stays busy and the next submit is queued, not
		// started.
		res, err := a.SubmitToSession(context.Background(), secondID, "first")
		if err != nil {
			t.Fatalf("SubmitToSession busy turn: %v", err)
		}
		if !res.Started {
			t.Fatalf("busy turn was queued instead of started: %+v", res)
		}

		out := new(bytes.Buffer)
		r := &Runner{agent: a, owner: a, out: out}
		a.SetEventHandler(r.handleEvent)
		r.setCurrentSessionID(firstID)
		r.seedPresented(firstID)

		// The targeted session is busy, so the submit queues; admission becomes
		// certain at the queue append, where the implicit switch commits routing
		// and advances presentation ahead of the queue-changed event for this
		// submit.
		r.handleSessionPrompt(context.Background(), Request{
			JSONRPC: "2.0",
			ID:      "prompt",
			Params:  json.RawMessage(`{"session_id":"` + secondID + `","content":"queued"}`),
		})
		r.drainForTest()
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")

		queueChangedIdx, responseIdx := -1, -1
		for i, line := range lines {
			var frame map[string]any
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				t.Fatalf("frame json: %v", err)
			}
			if m, _ := frame["method"].(string); m == "agent/queue_changed" {
				queueChangedIdx = i
			}
			if _, ok := frame["id"]; ok {
				responseIdx = i
			}
		}
		// The advance is adopted before the first written frame, so the
		// queue-changed event for this submit reaches the client instead of being
		// filtered out, and it is delivered before the response. An admitted
		// callback fired after the append drops it.
		if queueChangedIdx < 0 {
			t.Fatalf("no queue_changed frame reached the client; frames: %q", out.String())
		}
		if !strings.Contains(lines[queueChangedIdx], `"queued"`) {
			t.Fatalf("queue_changed frame does not carry this submit's item: %q", lines[queueChangedIdx])
		}
		if responseIdx < 0 {
			t.Fatalf("no success response among frames: %q", out.String())
		}
		if queueChangedIdx > responseIdx {
			t.Fatalf("queue_changed frame (index %d) delivered after the response (index %d): %q", queueChangedIdx, responseIdx, out.String())
		}
		var resp Response
		if err := json.Unmarshal([]byte(lines[responseIdx]), &resp); err != nil {
			t.Fatalf("response json: %v", err)
		}
		if resp.Error != nil {
			t.Fatalf("queued prompt response error = %+v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("queued prompt result not an object: %#v", resp.Result)
		}
		if started, _ := result["started"].(bool); started {
			t.Fatalf("queued prompt response started = true, want queued (false)")
		}
		if got := r.currentSessionSummary().ID; got != secondID {
			t.Fatalf("routing current = %q, want %q", got, secondID)
		}
		r.mu.Lock()
		presented := r.presented
		r.mu.Unlock()
		if presented != secondID {
			t.Fatalf("presentation current = %q, want %q", presented, secondID)
		}
	})

	t.Run("permission_resolved=cancel_forwarded_before_turn_end", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				writeACPSSE(w,
					acpToolCallChunk("perm-cancel", "test-model", "call_read", "read_file", `{"path":"x.txt"}`),
					acpStopChunk("perm-cancel", "test-model"),
					"[DONE]")
				return
			}
			// A cancelled turn makes no second model call; serve a plain
			// completion so a stray call cannot hang the test.
			writeACPSSE(w,
				acpTextChunk("perm-cancel-2", "test-model", "done"),
				acpStopChunk("perm-cancel-2", "test-model"),
				"[DONE]")
		}))
		t.Cleanup(server.Close)

		a := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
		// The ACP test config declares no agents.json, so the primary agent type
		// has no model; bootstrap it through the adapter-facing method.
		if err := a.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		out := new(bytes.Buffer)
		r := &Runner{agent: a, owner: a, out: out}
		a.SetEventHandler(r.handleEvent)
		sessionID := a.Init(ctx)
		if sessionID == "" {
			// Nothing to resume on a fresh agent: create, as Run does.
			var createErr error
			sessionID, createErr = a.NewSession("", "primary")
			if createErr != nil {
				t.Fatalf("NewSession: %v", createErr)
			}
		}
		r.setCurrentSessionID(sessionID)
		r.seedPresented(sessionID)

		res, err := a.SubmitToSession(ctx, sessionID, "read the file")
		if err != nil {
			t.Fatalf("SubmitToSession: %v", err)
		}
		if !res.Started {
			t.Fatalf("submit was queued, want started: %+v", res)
		}

		reqParams := waitForACPMethod(t, r, out, "agent/permission_request")
		reqID := acpParamString(t, reqParams, "id")
		if reqID == "" {
			t.Fatalf("permission_request notification carries no id: %#v", reqParams)
		}

		// Cancel while the prompt is pending: the forwarded resolution must clear
		// the client's mirror before the turn-end notification arrives.
		r.handleSessionCancel(Request{JSONRPC: "2.0", ID: "cancel", Params: json.RawMessage(`{}`)})

		waitForACPMethod(t, r, out, "agent/turn_end")
		r.drainForTest()
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		resolvedIdx, endIdx := -1, -1
		var resID, resSession string
		for i, line := range lines {
			var n Notification
			if err := json.Unmarshal([]byte(line), &n); err != nil {
				continue
			}
			switch n.Method {
			case "agent/permission_resolved":
				resolvedIdx = i
				params, _ := n.Params.(map[string]any)
				resID, _ = params["id"].(string)
				resSession, _ = params["sessionId"].(string)
			case "agent/turn_end":
				endIdx = i
			}
		}
		if resolvedIdx < 0 || endIdx < 0 {
			t.Fatalf("missing notifications: permission_resolved=%d turn_end=%d; output: %q", resolvedIdx, endIdx, out.String())
		}
		if resID != reqID {
			t.Fatalf("permission_resolved id = %q, want the pending request's id %q", resID, reqID)
		}
		if resSession != sessionID {
			t.Fatalf("permission_resolved sessionId = %q, want %q", resSession, sessionID)
		}
		if resolvedIdx > endIdx {
			t.Fatalf("permission_resolved (index %d) delivered after turn_end (index %d)", resolvedIdx, endIdx)
		}
	})

	t.Run("permission_respond=never_pending_is_reported", func(t *testing.T) {
		a := newACPTestAgentWithProvider(t, "http://127.0.0.1:9/v1", false)
		if err := a.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		sessionID, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		out := new(bytes.Buffer)
		r := &Runner{agent: a, owner: a, out: out}
		a.SetEventHandler(r.handleEvent)
		r.setCurrentSessionID(sessionID)
		r.seedPresented(sessionID)

		// The protocol client is third-party and may send anything: an unknown
		// request is real information and is reported, not swallowed.
		r.handlePermissionRespond(Request{
			JSONRPC: "2.0",
			ID:      "resp",
			Params:  json.RawMessage(`{"id":"bogus-id","action":"deny"}`),
		})
		r.handlePermissionSave(Request{
			JSONRPC: "2.0",
			ID:      "save",
			Params:  json.RawMessage(`{"id":"bogus-id","patterns":["/tmp/*"]}`),
		})
		r.drainForTest()
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		for _, wantID := range []string{"resp", "save"} {
			var respLine string
			for _, line := range lines {
				var resp Response
				if err := json.Unmarshal([]byte(line), &resp); err != nil {
					continue
				}
				if s, _ := resp.ID.(string); s == wantID {
					respLine = line
				}
			}
			if respLine == "" {
				t.Fatalf("no response for %q among frames: %q", wantID, out.String())
			}
			assertACPErrorResponse(t, respLine)
		}
	})
}

// acpPermissionPendingRunner wires an ACP runner whose first turn asks a
// read_file permission request, submits a message, and waits until the request
// is pending. It returns the session id, the output buffer, and the runner.
func acpPermissionPendingRunner(t *testing.T) (string, *bytes.Buffer, *Runner) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeACPSSE(w,
				acpToolCallChunk("perm-ask", "test-model", "call_read", "read_file", `{"path":"x.txt"}`),
				acpStopChunk("perm-ask", "test-model"),
				"[DONE]")
			return
		}
		// A cancelled turn makes no second model call; serve a plain
		// completion so a stray call cannot hang the test.
		writeACPSSE(w,
			acpTextChunk("perm-ask-2", "test-model", "done"),
			acpStopChunk("perm-ask-2", "test-model"),
			"[DONE]")
	}))
	t.Cleanup(server.Close)

	a := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
	// The ACP test config declares no agents.json, so the primary agent type
	// has no model; bootstrap it through the adapter-facing method.
	if err := a.SetDefaultModel("test/test-model"); err != nil {
		t.Fatalf("SetDefaultModel: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	out := new(bytes.Buffer)
	r := &Runner{agent: a, owner: a, out: out}
	a.SetEventHandler(r.handleEvent)
	sessionID := a.Init(ctx)
	if sessionID == "" {
		// Nothing to resume on a fresh agent: create, as Run does.
		var createErr error
		sessionID, createErr = a.NewSession("", "primary")
		if createErr != nil {
			t.Fatalf("NewSession: %v", createErr)
		}
	}
	r.setCurrentSessionID(sessionID)
	r.seedPresented(sessionID)

	res, err := a.SubmitToSession(ctx, sessionID, "read the file")
	if err != nil {
		t.Fatalf("SubmitToSession: %v", err)
	}
	if !res.Started {
		t.Fatalf("submit was queued, want started: %+v", res)
	}

	reqParams := waitForACPMethod(t, r, out, "agent/permission_request")
	if id := acpParamString(t, reqParams, "id"); id == "" {
		t.Fatalf("permission_request notification carries no id: %#v", reqParams)
	}
	return sessionID, out, r
}

// TestACPStalledOutputPreservesBoundaryOrder proves that with the output drainer
// stalled, the FIFO still delivers a queued source event, then the A->B boundary,
// then the switch response, then a destination event in that exact order — and that
// routing current commits B (so a current-target request routes to B) before the
// response is drained.
func TestACPStalledOutputPreservesBoundaryOrder(t *testing.T) {
	a := newACPTestAgent(t)
	aID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession A: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(aID, "a-msg"); err != nil {
		t.Fatalf("append A: %v", err)
	}
	bID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession B: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(bID, "b-msg"); err != nil {
		t.Fatalf("append B: %v", err)
	}

	out := new(bytes.Buffer)
	r := &Runner{agent: a, owner: a, out: out}
	r.setCurrentSessionID(aID)
	r.seedPresented(aID)
	// Output is stalled: the drainer is never started, so every frame accumulates in
	// the FIFO and is delivered only when the test drains it.

	// A source (A) event is queued before the A->B boundary.
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: aID, Seq: 1, Result: "a-event"})
	// The switch commits routing current to B and enqueues its boundary then response.
	r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw", Params: json.RawMessage(`{"id":"` + bID + `"}`)})
	if got := r.currentSessionSummary().ID; got != bID {
		t.Fatalf("routing current = %q, want B %q while output stalled", got, bID)
	}
	// A following current-target request routes to B while output remains stalled.
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "cur", Method: "session/current"})
	// A destination (B) event is queued after the boundary.
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: bID, Seq: 2, Result: "b-event"})

	// Resume output: the FIFO delivers in enqueue order.
	lines := drainedLines(t, r, out, 5)
	if c := acpChunkContent(t, lines[0]); c != "a-event" {
		t.Fatalf("frame 0 = %q, want the source event before the boundary", c)
	}
	assertACPNotificationMethod(t, lines[1], "agent/session_changed")
	assertACPSuccessResponse(t, lines[2])
	if got := acpSessionSummaryFromResponse(t, lines[3]).ID; got != bID {
		t.Fatalf("session/current response = %q, want B %q", got, bID)
	}
	if c := acpChunkContent(t, lines[4]); c != "b-event" {
		t.Fatalf("frame 4 = %q, want the destination event after the boundary", c)
	}
}

func TestHandleTurnActionACPInvalidParams(t *testing.T) {
	var out bytes.Buffer
	r := &Runner{agent: newACPTestAgent(t), out: &out}

	r.handleRevertHistory(Request{JSONRPC: "2.0", ID: "bad", Params: json.RawMessage(`{`)})

	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 || resp.Error.Message != "invalid params" {
		t.Fatalf("invalid params response = %+v, want -32602 invalid params", resp)
	}
}

func TestHandleSessionMessagesByIDDoesNotSwitchCurrentSession(t *testing.T) {
	a := newACPTestAgent(t)
	firstTurn := appendACPUserTurn(t, a, "first session")
	firstID := a.SessionCurrent().ID
	if firstID == "" || firstTurn == 0 {
		t.Fatalf("first session id/turn = %q/%d", firstID, firstTurn)
	}
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	appendACPUserTurn(t, a, "second session")
	currentID := a.SessionCurrent().ID
	if currentID == "" || currentID == firstID {
		t.Fatalf("current session id = %q, first = %q", currentID, firstID)
	}

	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(currentID)
	r.handleSessionMessages(Request{
		JSONRPC: "2.0",
		ID:      "messages-by-id",
		Params:  json.RawMessage(`{"id":"` + firstID + `"}`),
	})

	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("session/messages by id error = %+v", resp.Error)
	}
	msgs := displayMessagesFromResponse(t, resp)
	if got := acpUserMessageContents(msgs); !equalStringSlices(got, []string{"first session"}) {
		t.Fatalf("messages for first session = %q", got)
	}
	if got := a.SessionCurrent().ID; got != currentID {
		t.Fatalf("SessionMessagesFor switched current session to %q, want %q", got, currentID)
	}

	r.drainForTest()
	out.Reset()
	r.handleSessionMessages(Request{JSONRPC: "2.0", ID: "current-messages"})
	lines = drainedLines(t, r, &out, 1)
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("current response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("session/messages current error = %+v", resp.Error)
	}
	msgs = displayMessagesFromResponse(t, resp)
	if got := acpUserMessageContents(msgs); !equalStringSlices(got, []string{"second session"}) {
		t.Fatalf("current messages = %q", got)
	}
}

// TestSessionMessagesPointLookupHasNoCursor proves the session/messages point
// lookup keeps its bare-array response shape: the result is a message array,
// not a hydration-state object, so it carries no cursor — the point lookup is
// the unbounded complete-history exception to the committed-turn bound.
func TestSessionMessagesPointLookupHasNoCursor(t *testing.T) {
	a := newACPTestAgent(t)
	appendACPUserTurn(t, a, "point lookup")
	id := a.SessionCurrent().ID
	if id == "" {
		t.Fatal("missing session id")
	}

	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(id)
	r.handleSessionMessages(Request{
		JSONRPC: "2.0",
		ID:      "point-lookup",
		Params:  json.RawMessage(`{"id":"` + id + `"}`),
	})

	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("session/messages error = %+v", resp.Error)
	}
	msgs := displayMessagesFromResponse(t, resp)
	if got := acpUserMessageContents(msgs); !equalStringSlices(got, []string{"point lookup"}) {
		t.Fatalf("point lookup messages = %q", got)
	}
	// The result is a bare array: it decodes into the message list alone and
	// cannot decode into an object carrying a cursor.
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var shape struct {
		Cursor json.RawMessage `json:"cursor"`
	}
	if err := json.Unmarshal(data, &shape); err == nil {
		t.Fatal("point lookup result decodes as an object with a cursor, want a bare message array (no cursor)")
	}
}

func TestACPPromptSelectsSession(t *testing.T) {
	a := newACPTestAgent(t)
	_ = appendACPUserTurn(t, a, "first")
	firstID := a.SessionCurrent().ID
	if firstID == "" {
		t.Fatal("missing first session")
	}
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	_ = appendACPUserTurn(t, a, "second")
	secondID := a.SessionCurrent().ID
	if secondID == "" || secondID == firstID {
		t.Fatalf("second session id = %q, first = %q", secondID, firstID)
	}

	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(firstID)
	// A live context admits the submit, so the implicit switch commits inside
	// admission; a cancelled context would fail the submit before the switch and
	// the test would pass without exercising admission.
	r.handleSessionPrompt(context.Background(), Request{
		JSONRPC: "2.0",
		ID:      "prompt",
		Params:  json.RawMessage(`{"session_id":"` + secondID + `","content":"hello"}`),
	})
	if got := r.currentSessionSummary().ID; got != secondID {
		t.Fatalf("current session = %q, want %q", got, secondID)
	}
}

// TestACPPromptSwitchAdvancesPresentation proves prompting an explicit different
// session advances presentation current, so the prompted session's turn events reach
// the client (the switch pushes no client-visible boundary), and a late event for the
// previous session is dropped.
func TestACPPromptSwitchAdvancesPresentation(t *testing.T) {
	a := newACPTestAgent(t)
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(firstID, "first"); err != nil {
		t.Fatalf("append first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(secondID, "second"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	var out bytes.Buffer
	r := &Runner{agent: a, owner: a, out: &out}
	r.setCurrentSessionID(firstID)
	r.seedPresented(firstID)

	// A live context admits the submit, so the implicit switch advances
	// presentation current inside admission, ahead of any turn frames; a
	// cancelled context would fail the submit before the switch and the test
	// would pass without exercising admission.
	r.handleSessionPrompt(context.Background(), Request{
		JSONRPC: "2.0",
		ID:      "prompt",
		Params:  json.RawMessage(`{"session_id":"` + secondID + `","content":"hi"}`),
	})
	r.drainForTest()
	out.Reset()

	// A live event for the prompted session now reaches the client.
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: secondID, Result: "second-event"})
	lines := drainedLines(t, r, &out, 1)
	assertACPNotificationMethod(t, lines[0], "agent/message_chunk")

	// A late event for the previous session is dropped.
	out.Reset()
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: firstID, Result: "late-first"})
	r.drainForTest()
	if out.Len() != 0 {
		t.Fatalf("late event for the previous session was emitted: %q", out.String())
	}
}

func TestACPNewSetsCurrent(t *testing.T) {
	a := newACPTestAgent(t)
	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.handleSessionNew(Request{JSONRPC: "2.0", ID: "new"})
	if got := r.currentSessionSummary().ID; got == "" {
		t.Fatal("new session did not set current")
	}

	r.drainForTest()
	out.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.handleSessionPrompt(ctx, Request{
		JSONRPC: "2.0",
		ID:      "prompt",
		Params:  json.RawMessage(`{"content":"hello"}`),
	})
	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("prompt response json: %v", err)
	}
	if resp.Error != nil && strings.Contains(resp.Error.Message, "no current session") {
		t.Fatalf("prompt after new = %+v", resp.Error)
	}
}

// TestACPExplicitSwitchToContendedSessionIsReadOnly proves session/switch over
// a session another process drives opens it read-only: the switch succeeds,
// the session_changed notification carries the read-only hydration state with
// the session's own identity and durable transcript, session/prompt refuses
// with the contention message on both the explicit-id and implicit-current
// paths, compact and snapshot/list name the contention instead of "no current
// session", and a switch to a live session clears the marker and admits a
// prompt.
func TestACPExplicitSwitchToContendedSessionIsReadOnly(t *testing.T) {
	first, second := newACPTestAgentPair(t)
	heldID, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession held: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(heldID, "durable from the driving owner"); err != nil {
		t.Fatalf("append durable message: %v", err)
	}
	startupID, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}

	var out bytes.Buffer
	r := &Runner{agent: second, owner: second, out: &out}
	r.setCurrentSessionID(startupID)
	r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw", Params: json.RawMessage(`{"id":"` + heldID + `"}`)})

	lines := drainedLines(t, r, &out, 2)
	var notif Notification
	if err := json.Unmarshal([]byte(lines[0]), &notif); err != nil {
		t.Fatalf("notification json: %v", err)
	}
	if notif.Method != "agent/session_changed" {
		t.Fatalf("notification method = %q, want session_changed", notif.Method)
	}
	state := hydrationStateFromParams(t, notif.Params)
	if !state.ReadOnly {
		t.Fatal("read-only switch notification is not marked read-only")
	}
	if state.Session.ID != heldID {
		t.Fatalf("notification session = %q, want %q", state.Session.ID, heldID)
	}
	if got := acpUserMessageContents(state.Messages); !equalStringSlices(got, []string{"durable from the driving owner"}) {
		t.Fatalf("notification messages = %#v, want the durable transcript", got)
	}
	var resp Response
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("switch response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("switch response error = %+v, want a read-only open", resp.Error)
	}
	if got := r.currentSessionSummary().ID; got != heldID {
		t.Fatalf("runner current = %q, want the held session %q", got, heldID)
	}
	if got := r.sv().LiveCurrent(); got != "" {
		t.Fatalf("read-only session reports live: %q", got)
	}
	if !r.sv().IsReadOnly(heldID) {
		t.Fatal("held session not marked read-only")
	}

	// A prompt naming the read-only session refuses at its own precheck, which
	// never reaches the owner, with the contention message.
	out.Reset()
	r.handleSessionPrompt(context.Background(), Request{
		JSONRPC: "2.0",
		ID:      "prompt",
		Params:  json.RawMessage(`{"session_id":"` + heldID + `","content":"hi"}`),
	})
	lines = drainedLines(t, r, &out, 1)
	var promptResp Response
	if err := json.Unmarshal([]byte(lines[0]), &promptResp); err != nil {
		t.Fatalf("prompt response json: %v", err)
	}
	if promptResp.Error == nil || !strings.Contains(promptResp.Error.Message, "driven by another process") {
		t.Fatalf("explicit-id prompt over the read-only session = %+v, want the contention message", promptResp.Error)
	}

	// A prompt to the routing current (the read-only session) translates the
	// owner's refusal the same way.
	out.Reset()
	r.handleSessionPrompt(context.Background(), Request{
		JSONRPC: "2.0",
		ID:      "prompt",
		Params:  json.RawMessage(`{"content":"hi"}`),
	})
	lines = drainedLines(t, r, &out, 1)
	var implicitResp Response
	if err := json.Unmarshal([]byte(lines[0]), &implicitResp); err != nil {
		t.Fatalf("prompt response json: %v", err)
	}
	if implicitResp.Error == nil || !strings.Contains(implicitResp.Error.Message, "driven by another process") {
		t.Fatalf("implicit prompt over the read-only session = %+v, want the contention message", implicitResp.Error)
	}

	// Compact and snapshot/list name the contention instead of "no current
	// session", keeping the selection.
	out.Reset()
	r.handleCompact(context.Background(), Request{JSONRPC: "2.0", ID: "compact"})
	lines = drainedLines(t, r, &out, 1)
	var compactResp Response
	if err := json.Unmarshal([]byte(lines[0]), &compactResp); err != nil {
		t.Fatalf("compact response json: %v", err)
	}
	if compactResp.Error == nil || !strings.Contains(compactResp.Error.Message, "driven by another process") {
		t.Fatalf("compact over the read-only session = %+v, want the contention message", compactResp.Error)
	}

	// An explicit-id compact names the contention too, instead of the owner's
	// "unknown session" answer for a session this connection holds read-only.
	out.Reset()
	r.handleCompact(context.Background(), Request{
		JSONRPC: "2.0",
		ID:      "compact",
		Params:  json.RawMessage(`{"session_id":"` + heldID + `"}`),
	})
	lines = drainedLines(t, r, &out, 1)
	var explicitCompactResp Response
	if err := json.Unmarshal([]byte(lines[0]), &explicitCompactResp); err != nil {
		t.Fatalf("explicit-id compact response json: %v", err)
	}
	if explicitCompactResp.Error == nil || !strings.Contains(explicitCompactResp.Error.Message, "driven by another process") {
		t.Fatalf("explicit-id compact over the read-only session = %+v, want the contention message", explicitCompactResp.Error)
	}

	out.Reset()
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "snaps", Method: "snapshot/list"})
	lines = drainedLines(t, r, &out, 1)
	var snapsResp Response
	if err := json.Unmarshal([]byte(lines[0]), &snapsResp); err != nil {
		t.Fatalf("snapshot/list response json: %v", err)
	}
	if snapsResp.Error == nil || !strings.Contains(snapsResp.Error.Message, "driven by another process") {
		t.Fatalf("snapshot/list over the read-only session = %+v, want the contention message", snapsResp.Error)
	}
	if got := r.sv().Current(); got != heldID {
		t.Fatalf("selection after failed compact/snapshot = %q, want the read-only session kept %q", got, heldID)
	}

	// Switching to a live session clears the marker and admits a prompt.
	liveID, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession live: %v", err)
	}
	out.Reset()
	r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw2", Params: json.RawMessage(`{"id":"` + liveID + `"}`)})
	lines = drainedLines(t, r, &out, 2)
	var liveResp Response
	if err := json.Unmarshal([]byte(lines[1]), &liveResp); err != nil {
		t.Fatalf("switch response json: %v", err)
	}
	if liveResp.Error != nil {
		t.Fatalf("switch to live response error = %+v", liveResp.Error)
	}
	if r.sv().IsReadOnly(heldID) {
		t.Fatal("read-only marker survived the switch to a live session")
	}
	if got := r.sv().LiveCurrent(); got != liveID {
		t.Fatalf("live current after switch = %q, want %q", got, liveID)
	}
}

// TestACPReadOnlySwitchHydrationFailureLeavesRoutingUnchanged proves a
// read-only switch whose durable view cannot be read commits nothing: the
// protocol path keeps the previous session and the routing project.
func TestACPReadOnlySwitchHydrationFailureLeavesRoutingUnchanged(t *testing.T) {
	first, second := newACPTestAgentPair(t)
	heldID, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession held: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(heldID, "durable from the driving owner"); err != nil {
		t.Fatalf("append durable message: %v", err)
	}
	startupID, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}
	// A held session whose durable history cannot be read: the open still fails
	// as contention (the claim is acquired before any file read), and the
	// read-only hydration then fails on the corrupt compaction record.
	proj, err := first.ProjectCurrentForPath(first.ProjectRoot())
	if err != nil {
		t.Fatalf("project for held session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(first.Projects().SessionsRoot(proj.ID), heldID, "compaction.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("corrupt held session compaction: %v", err)
	}

	var out bytes.Buffer
	r := &Runner{agent: second, owner: second, out: &out}
	r.setCurrentSessionID(startupID)
	r.routeProjectPath = second.ProjectRoot()
	r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw", Params: json.RawMessage(`{"id":"` + heldID + `"}`)})
	lines := drainedLines(t, r, &out, 1)
	var resp Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("switch response json: %v", err)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "compaction.json") {
		t.Fatalf("switch over a held session with unreadable history = %+v, want the hydration failure", resp.Error)
	}
	if got := r.sv().Current(); got != startupID {
		t.Fatalf("routing current after failed read-only switch = %q, want unchanged %q", got, startupID)
	}
	if got := r.routeProjectPath; got != second.ProjectRoot() {
		t.Fatalf("routing project after failed read-only switch = %q, want unchanged %q", got, second.ProjectRoot())
	}
	if r.sv().IsReadOnly(heldID) {
		t.Fatal("read-only marker set for a switch whose presentation failed")
	}
}

// TestACPReopenAfterHolderReleasesIsLive proves a session switched read-only
// becomes live again when it is switched again after the driving process
// releases it: the read-only marker does not survive a successful live commit
// of the same id, so a prompt is admitted and no contention is reported.
func TestACPReopenAfterHolderReleasesIsLive(t *testing.T) {
	first, second := newACPTestAgentPair(t)
	heldID, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession held: %v", err)
	}
	startupID, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}
	var out bytes.Buffer
	r := &Runner{agent: second, owner: second, out: &out}
	r.setCurrentSessionID(startupID)
	r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw", Params: json.RawMessage(`{"id":"` + heldID + `"}`)})
	lines := drainedLines(t, r, &out, 2)
	var resp Response
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("switch response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("read-only switch response error = %+v", resp.Error)
	}
	if !r.sv().IsReadOnly(heldID) {
		t.Fatal("held session not marked read-only")
	}

	// The driving process releases the session; switching again must succeed
	// live.
	if err := first.SessionArchive(heldID); err != nil {
		t.Fatalf("SessionArchive (release): %v", err)
	}
	out.Reset()
	r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw2", Params: json.RawMessage(`{"id":"` + heldID + `"}`)})
	lines = drainedLines(t, r, &out, 2)
	var liveResp Response
	if err := json.Unmarshal([]byte(lines[1]), &liveResp); err != nil {
		t.Fatalf("switch response json: %v", err)
	}
	if liveResp.Error != nil {
		t.Fatalf("reopen switch response error = %+v, want a live open", liveResp.Error)
	}
	if r.sv().IsReadOnly(heldID) {
		t.Fatal("read-only marker survived the live reopen of the same session")
	}
	if got := r.sv().LiveCurrent(); got != heldID {
		t.Fatalf("live current after reopen = %q, want %q", got, heldID)
	}

	// A prompt to the reopened session is admitted; no contention is reported.
	out.Reset()
	r.handleSessionPrompt(context.Background(), Request{
		JSONRPC: "2.0",
		ID:      "prompt",
		Params:  json.RawMessage(`{"content":"hi"}`),
	})
	lines = drainedLines(t, r, &out, 1)
	var promptResp Response
	if err := json.Unmarshal([]byte(lines[0]), &promptResp); err != nil {
		t.Fatalf("prompt response json: %v", err)
	}
	if promptResp.Error != nil {
		t.Fatalf("prompt after reopen = %+v, want admission without contention", promptResp.Error)
	}
}

func TestACPSwitchKeepsCurrent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeACPSSE(w,
			acpTextChunk("switch-turn", "test-model", "ok"),
			acpStopChunk("switch-turn", "test-model"),
			"[DONE]")
	}))
	t.Cleanup(server.Close)

	a := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
	// The ACP test config declares no agents.json, so the primary agent type
	// has no model; bootstrap it through the adapter-facing method.
	if err := a.SetDefaultModel("test/test-model"); err != nil {
		t.Fatalf("SetDefaultModel: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_ = a.Init(ctx)

	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	var out bytes.Buffer
	r := &Runner{agent: a, owner: a, out: &out}
	a.SetEventHandler(r.handleEvent)
	r.seedPresented(firstID)
	// Run a real turn so the coordinator commits it: a marked-but-uncommitted
	// turn stays below the bounded switch boundary. The commit runs before the
	// turn_end event is emitted, so observing the notification makes the
	// switch's bounded read deterministic.
	if res, err := a.SubmitToSession(ctx, firstID, "first"); err != nil || !res.Started {
		t.Fatalf("SubmitToSession first = %+v, %v; want started", res, err)
	}
	waitForACPMethod(t, r, &out, "agent/turn_end")
	out.Reset()

	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(secondID, "second"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	r.setCurrentSessionID(secondID)
	r.handleSessionSwitch(Request{
		JSONRPC: "2.0",
		ID:      "switch",
		Params:  json.RawMessage(`{"id":"` + firstID + `"}`),
	})

	lines := drainedLines(t, r, &out, 2)
	var notif Notification
	if err := json.Unmarshal([]byte(lines[0]), &notif); err != nil {
		t.Fatalf("notification json: %v", err)
	}
	if notif.Method != "agent/session_changed" {
		t.Fatalf("notification method = %q, want session_changed", notif.Method)
	}
	payload := sessionPayloadFromParams(t, notif.Params)
	if payload.Session.ID != firstID {
		t.Fatalf("payload session = %q, want %q", payload.Session.ID, firstID)
	}
	if got := acpUserMessageContents(payload.Messages); !equalStringSlices(got, []string{"first"}) {
		t.Fatalf("payload messages = %#v, want first", got)
	}
	var resp Response
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("switch response error = %+v", resp.Error)
	}
	if got := r.currentSessionSummary().ID; got != firstID {
		t.Fatalf("runner current = %q, want %q", got, firstID)
	}
	if got := a.SessionCurrent().ID; got != secondID {
		t.Fatalf("backend current = %q, want %q", got, secondID)
	}
}

// TestACPSessionChangedCarriesResolvedModel proves the serialized
// agent/session_changed notification carries the destination session's model as
// the resolved object — identifier plus display name — not a bare
// provider/model string. The protocol host forwards the same captured state the
// desktop adapter hydrates from, so a third-party client can render the
// selector from the notification alone; decoding the wire params into the full
// hydration state must yield the resolved model.
func TestACPSessionChangedCarriesResolvedModel(t *testing.T) {
	a := newACPTestAgent(t)
	// The ACP test config declares no agents.json, so the primary agent type
	// has no model; bootstrap it through the adapter-facing method.
	if err := a.SetDefaultModel("test/test-model"); err != nil {
		t.Fatalf("SetDefaultModel: %v", err)
	}
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}

	var out bytes.Buffer
	r := &Runner{agent: a, owner: a, out: &out}
	r.setCurrentSessionID(secondID)
	r.handleSessionSwitch(Request{
		JSONRPC: "2.0",
		ID:      "switch",
		Params:  json.RawMessage(`{"id":"` + firstID + `"}`),
	})

	lines := drainedLines(t, r, &out, 2)
	var notif Notification
	if err := json.Unmarshal([]byte(lines[0]), &notif); err != nil {
		t.Fatalf("notification json: %v", err)
	}
	if notif.Method != "agent/session_changed" {
		t.Fatalf("notification method = %q, want session_changed", notif.Method)
	}
	state := hydrationStateFromParams(t, notif.Params)
	if state.Session.ID != firstID {
		t.Fatalf("notification session = %q, want %q", state.Session.ID, firstID)
	}
	if state.Model.Ref != "test/test-model" || state.Model.Provider != "test" || state.Model.Model != "test-model" ||
		state.Model.DisplayName != "Test Model" || state.Model.ContextWindow != 8192 {
		t.Fatalf("notification model = %+v, want resolved test/test-model (Test Model)", state.Model)
	}
}

func TestACPStaleCurrent(t *testing.T) {
	a := newACPTestAgent(t)
	_ = appendACPUserTurn(t, a, "gone")
	id := a.SessionCurrent().ID
	if id == "" {
		t.Fatal("missing session id")
	}
	if err := a.SessionDelete(id); err != nil {
		t.Fatalf("SessionDelete: %v", err)
	}

	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(id)
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "current", Method: "session/current"})
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "messages", Method: "session/messages"})

	lines := drainedLines(t, r, &out, 2)
	var current Response
	if err := json.Unmarshal([]byte(lines[0]), &current); err != nil {
		t.Fatalf("current json: %v", err)
	}
	data, err := json.Marshal(current.Result)
	if err != nil {
		t.Fatalf("current marshal: %v", err)
	}
	var summary agent.SessionSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("current result json: %v", err)
	}
	if summary.ID != "" {
		t.Fatalf("stale current id = %q, want empty", summary.ID)
	}

	var messages Response
	if err := json.Unmarshal([]byte(lines[1]), &messages); err != nil {
		t.Fatalf("messages json: %v", err)
	}
	if messages.Error == nil {
		t.Fatalf("stale current messages response = %+v, want error", messages)
	}
}

func TestACPStaleEvent(t *testing.T) {
	a := newACPTestAgent(t)
	_ = appendACPUserTurn(t, a, "gone")
	id := a.SessionCurrent().ID
	if id == "" {
		t.Fatal("missing session id")
	}
	if err := a.SessionDelete(id); err != nil {
		t.Fatalf("SessionDelete: %v", err)
	}

	var out bytes.Buffer
	r := &Runner{agent: a, out: &out}
	r.setCurrentSessionID(id)
	r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: id, Result: "skip"})
	r.drainForTest()
	if out.Len() != 0 {
		t.Fatalf("stale event was emitted: %q", out.String())
	}
	if got := r.currentSessionSummary().ID; got != "" {
		t.Fatalf("stale current id = %q, want empty", got)
	}
}

// TestACPProjectCurrentFollowsCrossProjectSwitch proves project/current resolves
// the routing-current session's project — not the owner/startup project — so an
// A->B cross-project session switch makes project/current report B's project.
func TestACPProjectCurrentFollowsCrossProjectSwitch(t *testing.T) {
	a := newACPTestAgent(t)
	startupID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}
	otherRoot := t.TempDir()
	otherID, err := a.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath: %v", err)
	}
	wantStartup, err := a.ProjectCurrentForPath(a.ProjectRoot())
	if err != nil {
		t.Fatalf("startup project: %v", err)
	}
	wantOther, err := a.ProjectCurrentForPath(otherRoot)
	if err != nil {
		t.Fatalf("other project: %v", err)
	}
	if wantStartup.ID == wantOther.ID {
		t.Fatal("test setup: the two sessions must be in different projects")
	}

	var out bytes.Buffer
	r := &Runner{agent: a, owner: a, out: &out}
	r.setCurrentSessionID(startupID)

	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "p1", Method: "project/current"})
	if got := acpProjectFromResponse(t, drainedLines(t, r, &out, 1)[0]); got.ID != wantStartup.ID {
		t.Fatalf("project/current before switch id = %q, want startup %q", got.ID, wantStartup.ID)
	}
	out.Reset()

	r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw", Params: json.RawMessage(`{"id":"` + otherID + `"}`)})
	r.drainForTest()
	out.Reset()

	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "p2", Method: "project/current"})
	if got := acpProjectFromResponse(t, drainedLines(t, r, &out, 1)[0]); got.ID != wantOther.ID {
		t.Fatalf("project/current after cross-project switch id = %q, want %q (startup was %q)", got.ID, wantOther.ID, wantStartup.ID)
	}
}

// TestAdapterExplicitSessionTargetingContract proves the project-implicit ACP
// routes resolve to the connection's current session's project after a
// cross-project switch: project/current reports it, session/new creates in it,
// session/list lists it, and file/read enforces its root as the viewer sandbox.
// With no session selected, the three operation routes fall back to the
// owner-startup project (the same fallback Run's bootstrap uses), while
// project/current alone errors with -32000 — a pure query has nothing to report
// where creating, listing, and reading stay meaningful against the owner project.
func TestAdapterExplicitSessionTargetingContract(t *testing.T) {
	newSwitchedRunner := func(t *testing.T) (*agent.Agent, *Runner, *bytes.Buffer, string, string) {
		t.Helper()
		a := newACPTestAgent(t)
		startupID, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession startup: %v", err)
		}
		otherRoot := t.TempDir()
		otherID, err := a.NewSessionForProjectPath(otherRoot, "primary")
		if err != nil {
			t.Fatalf("NewSessionForProjectPath: %v", err)
		}
		out := new(bytes.Buffer)
		r := &Runner{agent: a, owner: a, out: out}
		r.setCurrentSessionID(startupID)
		r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw", Params: json.RawMessage(`{"id":"` + otherID + `"}`)})
		r.drainForTest()
		out.Reset()
		return a, r, out, otherRoot, otherID
	}

	t.Run("project_current=cross_project_session_switch_reports_B", func(t *testing.T) {
		a, r, out, otherRoot, _ := newSwitchedRunner(t)
		wantOther, err := a.ProjectCurrentForPath(otherRoot)
		if err != nil {
			t.Fatalf("other project: %v", err)
		}
		r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "p", Method: "project/current"})
		if got := acpProjectFromResponse(t, drainedLines(t, r, out, 1)[0]); got.ID != wantOther.ID {
			t.Fatalf("project/current after cross-project switch = %q, want %q", got.ID, wantOther.ID)
		}
	})

	t.Run("project_current=no_session_returns_error", func(t *testing.T) {
		a := newACPTestAgent(t)
		out := new(bytes.Buffer)
		r := &Runner{agent: a, owner: a, out: out}
		r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "p", Method: "project/current"})
		line := drainedLines(t, r, out, 1)[0]
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("response json: %v", err)
		}
		if resp.Error == nil {
			t.Fatalf("project/current with no session = %+v, want -32000 error", resp.Result)
		}
		if resp.Error.Code != -32000 || !strings.Contains(resp.Error.Message, "no current session") {
			t.Fatalf("project/current error = %+v, want code -32000 message containing %q", resp.Error, "no current session")
		}
	})

	t.Run("acp_session_new=creates_in_selected_project", func(t *testing.T) {
		_, r, out, otherRoot, _ := newSwitchedRunner(t)
		r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "new", Method: "session/new"})
		lines := drainedLines(t, r, out, 2)
		assertACPNotificationMethod(t, lines[0], "agent/session_changed")
		summary := acpSessionSummaryFromResponse(t, lines[1])
		if summary.ID == "" {
			t.Fatal("session/new returned no session")
		}
		if summary.ProjectPath != otherRoot {
			t.Fatalf("session/new created in project %q, want selected project %q", summary.ProjectPath, otherRoot)
		}
	})

	t.Run("acp_session_list=lists_selected_project", func(t *testing.T) {
		a, r, out, otherRoot, _ := newSwitchedRunner(t)
		r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "list", Method: "session/list"})
		line := drainedLines(t, r, out, 1)[0]
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("response json: %v", err)
		}
		if resp.Error != nil {
			t.Fatalf("session/list response error: %+v", resp.Error)
		}
		data, err := json.Marshal(resp.Result)
		if err != nil {
			t.Fatalf("session/list result marshal: %v", err)
		}
		var got []agent.SessionSummary
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("session/list result json: %v", err)
		}
		want, err := a.SessionListForProjectPath(otherRoot, "active")
		if err != nil {
			t.Fatalf("SessionListForProjectPath: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("session/list returned %d sessions, want %d for selected project", len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID {
				t.Fatalf("session/list[%d] = %q, want selected project session %q", i, got[i].ID, want[i].ID)
			}
		}
	})

	t.Run("acp_file_read=allows_inside_selected_root", func(t *testing.T) {
		_, r, out, otherRoot, _ := newSwitchedRunner(t)
		inside := filepath.Join(otherRoot, "readme.txt")
		if err := os.WriteFile(inside, []byte("b-content"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.dispatch(context.Background(), Request{
			JSONRPC: "2.0",
			ID:      "read",
			Method:  "file/read",
			Params:  json.RawMessage(`{"path":` + strconv.Quote(inside) + `}`),
		})
		line := drainedLines(t, r, out, 1)[0]
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("response json: %v", err)
		}
		if resp.Error != nil {
			t.Fatalf("file/read inside selected root errored: %+v", resp.Error)
		}
		data, err := json.Marshal(resp.Result)
		if err != nil {
			t.Fatalf("file/read result marshal: %v", err)
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("file/read result json: %v", err)
		}
		if payload.Content != "b-content" {
			t.Fatalf("file/read inside selected root = %q, want %q", payload.Content, "b-content")
		}
	})

	t.Run("acp_file_read=refuses_outside_selected_root", func(t *testing.T) {
		a, r, out, _, _ := newSwitchedRunner(t)
		outside := filepath.Join(a.ProjectRoot(), "old.txt")
		if err := os.WriteFile(outside, []byte("old-content"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.dispatch(context.Background(), Request{
			JSONRPC: "2.0",
			ID:      "read",
			Method:  "file/read",
			Params:  json.RawMessage(`{"path":` + strconv.Quote(outside) + `}`),
		})
		line := drainedLines(t, r, out, 1)[0]
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("response json: %v", err)
		}
		if resp.Error == nil {
			t.Fatalf("file/read outside selected root succeeded with %+v, want boundary refusal", resp.Result)
		}
	})

	t.Run("acp_no_session=falls_back_to_owner_project", func(t *testing.T) {
		a := newACPTestAgent(t)
		out := new(bytes.Buffer)
		r := &Runner{agent: a, owner: a, out: out}
		ownerRoot := a.ProjectRoot()

		r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "list", Method: "session/list"})
		line := drainedLines(t, r, out, 1)[0]
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("list response json: %v", err)
		}
		if resp.Error != nil {
			t.Fatalf("session/list with no session errored: %+v", resp.Error)
		}
		data, err := json.Marshal(resp.Result)
		if err != nil {
			t.Fatalf("session/list result marshal: %v", err)
		}
		var got []agent.SessionSummary
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("session/list result json: %v", err)
		}
		want, err := a.SessionListForProjectPath(ownerRoot, "active")
		if err != nil {
			t.Fatalf("SessionListForProjectPath: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("session/list with no session returned %d sessions, want owner project's %d", len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID {
				t.Fatalf("session/list[%d] = %q, want owner project session %q", i, got[i].ID, want[i].ID)
			}
		}
		out.Reset()

		inside := filepath.Join(ownerRoot, "readme.txt")
		if err := os.WriteFile(inside, []byte("owner-content"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.dispatch(context.Background(), Request{
			JSONRPC: "2.0",
			ID:      "read",
			Method:  "file/read",
			Params:  json.RawMessage(`{"path":` + strconv.Quote(inside) + `}`),
		})
		line = drainedLines(t, r, out, 1)[0]
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("read response json: %v", err)
		}
		if resp.Error != nil {
			t.Fatalf("file/read inside owner root with no session errored: %+v", resp.Error)
		}
		out.Reset()

		r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "new", Method: "session/new"})
		lines := drainedLines(t, r, out, 2)
		assertACPNotificationMethod(t, lines[0], "agent/session_changed")
		summary := acpSessionSummaryFromResponse(t, lines[1])
		if summary.ID == "" {
			t.Fatal("session/new returned no session")
		}
		if summary.ProjectPath != ownerRoot {
			t.Fatalf("session/new with no session created in project %q, want owner project %q", summary.ProjectPath, ownerRoot)
		}
	})
}

// TestACPProjectRoutesKeepSessionProjectAfterRemoval proves the project-scoped
// routes keep answering for the project of the session they were routing to
// after that session is removed — archived or deleted, the two removal shapes
// that clear routing current through separate calls: the routing project is
// committed with the session id on every set and deliberately survives the
// current-session removal, so session/new, session/list and file/read do not
// fall back to the startup project.
func TestACPProjectRoutesKeepSessionProjectAfterRemoval(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Runner, Request)
	}{
		{name: "archive", run: func(r *Runner, req Request) { r.handleSessionArchive(req) }},
		{name: "delete", run: func(r *Runner, req Request) { r.handleSessionDelete(req) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newACPTestAgent(t)
			startupID, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession startup: %v", err)
			}
			otherRoot := t.TempDir()
			otherID, err := a.NewSessionForProjectPath(otherRoot, "primary")
			if err != nil {
				t.Fatalf("NewSessionForProjectPath: %v", err)
			}
			keptID, err := a.NewSessionForProjectPath(otherRoot, "primary")
			if err != nil {
				t.Fatalf("NewSessionForProjectPath kept: %v", err)
			}
			if startupID == otherID || otherID == keptID {
				t.Fatal("test setup: sessions must be distinct")
			}
			readme := filepath.Join(otherRoot, "readme.txt")
			if err := os.WriteFile(readme, []byte("b-content"), 0o600); err != nil {
				t.Fatal(err)
			}

			out := new(bytes.Buffer)
			r := &Runner{agent: a, owner: a, out: out}
			r.setCurrentSessionID(startupID)

			// Route to the other-project session, then remove it: routing
			// current clears while the routing project stays the other project.
			r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw", Params: json.RawMessage(`{"id":"` + otherID + `"}`)})
			r.drainForTest()
			out.Reset()
			tc.run(r, Request{JSONRPC: "2.0", ID: "rem", Params: json.RawMessage(`{"id":"` + otherID + `"}`)})
			r.drainForTest()
			out.Reset()
			if got, err := r.currentSession(); err == nil {
				t.Fatalf("routing current after %s = %q, want cleared", tc.name, got)
			}

			// session/list still lists the other project, not the startup one.
			r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "list", Method: "session/list"})
			gotList := acpSessionListFromResponse(t, drainedLines(t, r, out, 1)[0])
			wantList, err := a.SessionListForProjectPath(otherRoot, "active")
			if err != nil {
				t.Fatalf("SessionListForProjectPath: %v", err)
			}
			if len(gotList) != len(wantList) {
				t.Fatalf("session/list returned %d sessions, want the routed project's %d: %#v", len(gotList), len(wantList), gotList)
			}
			for i := range wantList {
				if gotList[i].ID != wantList[i].ID {
					t.Fatalf("session/list[%d] = %q, want routed project session %q", i, gotList[i].ID, wantList[i].ID)
				}
			}
			if len(gotList) != 1 || gotList[0].ID != keptID {
				t.Fatalf("session/list = %#v, want the kept routed-project session %q", gotList, keptID)
			}
			out.Reset()

			// session/new still creates in the other project.
			r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "new", Method: "session/new"})
			lines := drainedLines(t, r, out, 2)
			assertACPNotificationMethod(t, lines[0], "agent/session_changed")
			summary := acpSessionSummaryFromResponse(t, lines[1])
			if summary.ID == "" {
				t.Fatal("session/new returned no session")
			}
			if summary.ProjectPath != otherRoot {
				t.Fatalf("session/new created in project %q, want routed project %q", summary.ProjectPath, otherRoot)
			}
			out.Reset()

			// file/read still reads from the other project.
			if got := acpFileReadContent(t, r, out, readme); got != "b-content" {
				t.Fatalf("file/read after %s = %q, want %q", tc.name, got, "b-content")
			}
		})
	}
}

// TestACPProjectRoutesFollowExplicitPromptTarget proves the project-scoped
// routes answer for the project of a session named explicitly in
// session/prompt: the implicit switch inside submit admission commits the
// routing project alongside the id, so a client that prompts another
// project's session without ever calling session/switch has its subsequent
// session/new, session/list and file/read scoped there.
func TestACPProjectRoutesFollowExplicitPromptTarget(t *testing.T) {
	a := newACPTestAgent(t)
	startupID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}
	otherRoot := t.TempDir()
	otherID, err := a.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath: %v", err)
	}
	readme := filepath.Join(otherRoot, "readme.txt")
	if err := os.WriteFile(readme, []byte("b-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := new(bytes.Buffer)
	r := &Runner{agent: a, owner: a, out: out}
	a.SetEventHandler(r.handleEvent)
	r.setCurrentSessionID(startupID)

	// A live context admits the submit, so the implicit switch commits routing
	// current and the routing project inside admission. The turn's own frames
	// (turn_start, warnings) are enqueued by the failed turn's goroutine on no
	// fixed schedule, so the route assertions below read the response frame by
	// id and tolerate whatever else is on the wire around it.
	r.handleSessionPrompt(context.Background(), Request{
		JSONRPC: "2.0",
		ID:      "prompt",
		Params:  json.RawMessage(`{"session_id":"` + otherID + `","content":"hi"}`),
	})
	r.drainForTest()
	out.Reset()
	if got := r.currentSessionSummary().ID; got != otherID {
		t.Fatalf("routing current = %q, want %q", got, otherID)
	}

	// session/list answers for the prompted session's project.
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "list", Method: "session/list"})
	gotList := acpSessionListFromResponse(t, responseLineForID(t, r, out, "list"))
	wantList, err := a.SessionListForProjectPath(otherRoot, "active")
	if err != nil {
		t.Fatalf("SessionListForProjectPath: %v", err)
	}
	if len(gotList) != len(wantList) {
		t.Fatalf("session/list returned %d sessions, want the prompted project's %d: %#v", len(gotList), len(wantList), gotList)
	}
	for i := range wantList {
		if gotList[i].ID != wantList[i].ID {
			t.Fatalf("session/list[%d] = %q, want prompted project session %q", i, gotList[i].ID, wantList[i].ID)
		}
	}
	if len(gotList) != 1 || gotList[0].ID != otherID {
		t.Fatalf("session/list = %#v, want the prompted session %q", gotList, otherID)
	}
	out.Reset()

	// session/new creates in the prompted session's project.
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "new", Method: "session/new"})
	summary := acpSessionSummaryFromResponse(t, responseLineForID(t, r, out, "new"))
	if summary.ID == "" {
		t.Fatal("session/new returned no session")
	}
	if summary.ProjectPath != otherRoot {
		t.Fatalf("session/new created in project %q, want prompted project %q", summary.ProjectPath, otherRoot)
	}
	out.Reset()

	// file/read reads from the prompted session's project.
	if got := acpFileReadContent(t, r, out, readme); got != "b-content" {
		t.Fatalf("file/read after prompt = %q, want %q", got, "b-content")
	}
}

// TestACPProjectRoutesKeepSessionProjectAfterEviction proves the routing
// project survives a current-session eviction: a history revert whose
// post-walk reload fails evicts the session and clears the current id, and the
// empty-state boundary the eviction publishes through the turn-action callback
// must clear the id without clearing the project the session was in, so the
// project-scoped routes keep answering for it.
func TestACPProjectRoutesKeepSessionProjectAfterEviction(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not block reads as root")
	}
	a, home := newACPTestAgentEnv(t, "http://127.0.0.1:9/v1", false)
	startupID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}
	otherRoot := t.TempDir()
	otherID, err := a.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath: %v", err)
	}
	for i := 1; i <= 10; i++ {
		if _, err := a.AppendUserMessageToSession(otherID, fmt.Sprintf("turn %d", i)); err != nil {
			t.Fatalf("append turn %d: %v", i, err)
		}
	}
	readme := filepath.Join(otherRoot, "readme.txt")
	if err := os.WriteFile(readme, []byte("b-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Route to the other-project session first: opening re-hydrates the loop
	// from disk, so the blocking below must come after it. Then make the
	// reload fail after the walk ran: the walk stops at the blocked turn 7
	// directory, and the reload then fails reading the surviving turn 7's
	// messages file.
	out := new(bytes.Buffer)
	r := &Runner{agent: a, owner: a, out: out}
	r.setCurrentSessionID(startupID)
	r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw", Params: json.RawMessage(`{"id":"` + otherID + `"}`)})
	r.drainForTest()
	out.Reset()

	proj, err := a.ProjectCurrentForPath(otherRoot)
	if err != nil {
		t.Fatalf("ProjectCurrentForPath: %v", err)
	}
	sessionDir := filepath.Join(home, ".lightcode", "projects", proj.ID, "sessions", otherID)
	blockedDir := filepath.Join(sessionDir, "turns", "7")
	if err := os.Chmod(blockedDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blockedDir, 0o700) })
	blockedMessages := filepath.Join(blockedDir, "messages.jsonl")
	if err := os.Chmod(blockedMessages, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blockedMessages, 0o600) })

	// The revert walks turns 10..7, stops at the blocked 7, and the reload
	// failure evicts the session; the eviction boundary carries no session and
	// clears the current id. The routing project must survive the empty-state
	// boundary the callback receives.
	r.handleRevertHistory(Request{JSONRPC: "2.0", ID: "rv", Params: json.RawMessage(`{"turn":6}`)})
	r.drainForTest()
	out.Reset()
	if got, err := r.currentSession(); err == nil {
		t.Fatalf("routing current after eviction = %q, want cleared", got)
	}

	// session/list still lists the other project, not the startup one.
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "list", Method: "session/list"})
	gotList := acpSessionListFromResponse(t, drainedLines(t, r, out, 1)[0])
	wantList, err := a.SessionListForProjectPath(otherRoot, "active")
	if err != nil {
		t.Fatalf("SessionListForProjectPath: %v", err)
	}
	if len(gotList) != len(wantList) {
		t.Fatalf("session/list returned %d sessions, want the routed project's %d: %#v", len(gotList), len(wantList), gotList)
	}
	for i := range wantList {
		if gotList[i].ID != wantList[i].ID {
			t.Fatalf("session/list[%d] = %q, want routed project session %q", i, gotList[i].ID, wantList[i].ID)
		}
	}
	if len(gotList) != 1 || gotList[0].ID != otherID {
		t.Fatalf("session/list = %#v, want the evicted routed-project session %q", gotList, otherID)
	}
	out.Reset()

	// session/new still creates in the other project.
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "new", Method: "session/new"})
	lines := drainedLines(t, r, out, 2)
	assertACPNotificationMethod(t, lines[0], "agent/session_changed")
	summary := acpSessionSummaryFromResponse(t, lines[1])
	if summary.ID == "" {
		t.Fatal("session/new returned no session")
	}
	if summary.ProjectPath != otherRoot {
		t.Fatalf("session/new created in project %q, want routed project %q", summary.ProjectPath, otherRoot)
	}
	out.Reset()

	// file/read still reads from the other project.
	if got := acpFileReadContent(t, r, out, readme); got != "b-content" {
		t.Fatalf("file/read after eviction = %q, want %q", got, "b-content")
	}
}

// TestACPRefusedPromptKeepsRoutingProject proves the explicit-id precheck does
// not move the routing project: the project is committed with the id inside
// submit admission, so a prompt whose submit is refused admission leaves both
// the current id and the routing project on the previous session.
func TestACPRefusedPromptKeepsRoutingProject(t *testing.T) {
	a := newACPTestAgent(t)
	startupID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}
	otherRoot := t.TempDir()
	otherID, err := a.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath: %v", err)
	}

	// Park a turn on the target: without Init the failed provider call parks
	// in the flush round-trip, so the unit stays busy and the prompt below is
	// refused at the queue admission on the cancelled context, after the
	// precheck has already resolved the session.
	res, err := a.SubmitToSession(context.Background(), otherID, "park")
	if err != nil {
		t.Fatalf("SubmitToSession park: %v", err)
	}
	if !res.Started {
		t.Fatalf("park submit was queued instead of started: %+v", res)
	}

	out := new(bytes.Buffer)
	r := &Runner{agent: a, owner: a, out: out}
	r.setCurrentSessionID(startupID)
	r.handleSessionSwitch(Request{JSONRPC: "2.0", ID: "sw", Params: json.RawMessage(`{"id":"` + startupID + `"}`)})
	r.drainForTest()
	out.Reset()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.handleSessionPrompt(ctx, Request{
		JSONRPC: "2.0",
		ID:      "prompt",
		Params:  json.RawMessage(`{"session_id":"` + otherID + `","content":"hi"}`),
	})
	var resp Response
	if err := json.Unmarshal([]byte(responseLineForID(t, r, out, "prompt")), &resp); err != nil {
		t.Fatalf("prompt response json: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("refused prompt succeeded with %+v, want an error", resp.Result)
	}

	// Neither the current id nor the routing project moved.
	if got, err := r.currentSession(); err != nil || got != startupID {
		t.Fatalf("routing current = %q (err %v), want unchanged %q", got, err, startupID)
	}
	if got := r.currentProjectPath(); got != a.ProjectRoot() {
		t.Fatalf("routing project after refused prompt = %q, want unchanged %q", got, a.ProjectRoot())
	}
	out.Reset()

	// The project-scoped routes still answer for the previous project.
	r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "list", Method: "session/list"})
	gotList := acpSessionListFromResponse(t, responseLineForID(t, r, out, "list"))
	wantList, err := a.SessionListForProjectPath(a.ProjectRoot(), "active")
	if err != nil {
		t.Fatalf("SessionListForProjectPath: %v", err)
	}
	if len(gotList) != len(wantList) {
		t.Fatalf("session/list after refused prompt returned %d sessions, want the previous project's %d: %#v", len(gotList), len(wantList), gotList)
	}
	for i := range wantList {
		if gotList[i].ID != wantList[i].ID {
			t.Fatalf("session/list[%d] after refused prompt = %q, want previous project session %q", i, gotList[i].ID, wantList[i].ID)
		}
	}
}

func TestACPClearRemovedCurrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Runner, Request)
	}{
		{name: "archive", run: func(r *Runner, req Request) { r.handleSessionArchive(req) }},
		{name: "delete", run: func(r *Runner, req Request) { r.handleSessionDelete(req) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newACPTestAgent(t)
			firstID, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession first: %v", err)
			}
			if _, err := a.AppendUserMessageToSession(firstID, "first"); err != nil {
				t.Fatalf("append first: %v", err)
			}
			secondID, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession second: %v", err)
			}
			if _, err := a.AppendUserMessageToSession(secondID, "second"); err != nil {
				t.Fatalf("append second: %v", err)
			}

			var out bytes.Buffer
			r := &Runner{agent: a, out: &out}
			r.setCurrentSessionID(firstID)
			tc.run(r, Request{
				JSONRPC: "2.0",
				ID:      tc.name,
				Params:  json.RawMessage(`{"id":"` + firstID + `"}`),
			})
			if got := r.currentSessionSummary().ID; got != "" {
				t.Fatalf("current after %s = %q, want empty", tc.name, got)
			}
			r.drainForTest()
			out.Reset()
			r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "snapshots", Method: "snapshot/list"})
			lines := drainedLines(t, r, &out, 1)
			var snapshots Response
			if err := json.Unmarshal([]byte(lines[0]), &snapshots); err != nil {
				t.Fatalf("snapshot response json: %v", err)
			}
			if snapshots.Error == nil {
				t.Fatalf("%s snapshot/list response = %+v, want error", tc.name, snapshots)
			}
			r.drainForTest()
			out.Reset()
			r.dispatch(context.Background(), Request{JSONRPC: "2.0", ID: "compact", Method: "compact"})
			lines = drainedLines(t, r, &out, 1)
			var compact Response
			if err := json.Unmarshal([]byte(lines[0]), &compact); err != nil {
				t.Fatalf("compact response json: %v", err)
			}
			if compact.Error == nil {
				t.Fatalf("%s compact response = %+v, want error", tc.name, compact)
			}
			r.drainForTest()
			out.Reset()
			r.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: firstID, Result: "skip"})
			r.drainForTest()
			if strings.Contains(out.String(), "skip") {
				t.Fatalf("%s left old session event visible: %q", tc.name, out.String())
			}
			if current := a.SessionCurrent().ID; current != secondID {
				t.Fatalf("backend current after %s = %q, want %q", tc.name, current, secondID)
			}
		})
	}
}

func TestACPHandlersUseSharedTurnActionContract(t *testing.T) {
	src := mustReadACPSource(t)
	helper := extractSourceFunc(t, src, "func (r *Runner) handleTurnAction(")
	if !strings.Contains(helper, ".ApplyTurnActionForSessionWithBoundary(") {
		t.Fatal("handleTurnAction must call ApplyTurnActionForSessionWithBoundary")
	}
	for _, forbidden := range []string{".ForkSession(", ".RevertCode(", ".RevertHistory("} {
		if strings.Contains(helper, forbidden) {
			t.Fatalf("handleTurnAction must not call low-level %s", forbidden)
		}
	}

	wrappers := map[string]string{
		"func (r *Runner) handleSessionFork(":   "r.handleTurnAction(req, agent.TurnActionFork)",
		"func (r *Runner) handleRevertCode(":    "r.handleTurnAction(req, agent.TurnActionRevertCode)",
		"func (r *Runner) handleRevertHistory(": "r.handleTurnAction(req, agent.TurnActionRevertHistory)",
	}
	for signature, want := range wrappers {
		body := extractSourceFunc(t, src, signature)
		if !strings.Contains(body, want) {
			t.Fatalf("%s must delegate with %q; body:\n%s", signature, want, body)
		}
	}

	compact := extractSourceFunc(t, src, "func (r *Runner) handleCompact(")
	if strings.Contains(compact, "pushSessionChanged(") {
		t.Fatalf("handleCompact must leave session refresh to EventCompactionEnd; body:\n%s", compact)
	}
}

func responseLines(t *testing.T, output string, want int) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != want {
		t.Fatalf("output lines = %d, want %d: %q", len(lines), want, output)
	}
	return lines
}

// drainForTest synchronously writes the runner's queued output frames to r.out, so
// a test can read the output the sole drainer would otherwise write asynchronously.
func (r *Runner) drainForTest() {
	for {
		r.mu.Lock()
		if len(r.outFrames) == 0 {
			r.mu.Unlock()
			return
		}
		f := r.outFrames[0]
		r.outFrames = r.outFrames[1:]
		write := r.presentAcceptsLocked(f)
		r.mu.Unlock()
		if write && f.data != nil {
			_, _ = r.out.Write(f.data)
		}
	}
}

func drainedLines(t *testing.T, r *Runner, out *bytes.Buffer, want int) []string {
	t.Helper()
	r.drainForTest()
	return responseLines(t, out.String(), want)
}

func newACPTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	return newACPTestAgentWithProvider(t, "http://127.0.0.1:9/v1", false)
}

func newACPWarningTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	a := newACPTestAgentWithProvider(t, server.URL+"/v1", true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	return a
}

func newACPTestAgentWithProvider(t *testing.T, baseURL string, discovery bool) *agent.Agent {
	t.Helper()
	a, _ := newACPTestAgentEnv(t, baseURL, discovery)
	return a
}

// newACPTestAgentEnv is newACPTestAgentWithProvider that also returns the
// storage home directory the agent was built with, for tests that must reach
// the session files on disk.
func newACPTestAgentEnv(t *testing.T, baseURL string, discovery bool) (*agent.Agent, string) {
	t.Helper()
	home := t.TempDir()
	a := newACPTestAgentAtHome(t, baseURL, discovery, home)
	return a, home
}

// newACPTestAgentAtHome builds an owner over the given home, so several owners
// can share one home for cross-process claim testing.
func newACPTestAgentAtHome(t *testing.T, baseURL string, discovery bool, home string) *agent.Agent {
	t.Helper()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "`+baseURL+`", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": `+strconv.FormatBool(discovery)+`,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The agents config gives every session a resolvable live model, which the
	// fork now requires of a driveable source (the persisted-model fallback is
	// gone).
	if err := os.WriteFile(filepath.Join(lightcodeDir, "agents.json"), []byte(`{"primary": {"model": "test/test-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := lcconfig.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := agent.New(agent.Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

// newACPTestAgentPair builds two owners over the same home with distinct
// project roots, so one owner's live sessions hold their claims against the
// other.
func newACPTestAgentPair(t *testing.T) (*agent.Agent, *agent.Agent) {
	t.Helper()
	home := t.TempDir()
	first := newACPTestAgentAtHome(t, "http://127.0.0.1:9/v1", false, home)
	second := newACPTestAgentAtHome(t, "http://127.0.0.1:9/v1", false, home)
	return first, second
}

func appendACPUserTurn(t *testing.T, a *agent.Agent, content string) int {
	t.Helper()
	// The removed ensureSession creating branch used to open a session on first
	// use; bootstrap through the real creation entry when none exists yet.
	if !a.Store().Active() {
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
	}
	turn, err := a.AppendUserMessage(content)
	if err != nil {
		t.Fatalf("AppendUserMessage: %v", err)
	}
	return turn
}

func appendACPUserTurnWithSnapshot(t *testing.T, a *agent.Agent, content, path, after string) int {
	t.Helper()
	turn := appendACPUserTurn(t, a, content)
	entryID, _, err := a.Store().SnapshotResolvedEntry(turn, path, path)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatalf("write changed file: %v", err)
	}
	if err := a.Store().RecordSnapshotContent(turn, entryID, []byte(after)); err != nil {
		t.Fatalf("RecordSnapshotContent: %v", err)
	}
	return turn
}

func assertACPSuccessResponse(t *testing.T, line string) {
	t.Helper()
	var resp Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	// A JSON-RPC response echoes the request id; a notification carries none. This
	// rejects a stray boundary notification standing in for the operation response.
	if resp.ID == nil {
		t.Fatalf("expected a JSON-RPC response with an id, got a frame without one: %s", line)
	}
	if resp.Error != nil {
		t.Fatalf("expected success response, got error %+v", resp.Error)
	}
}

func assertACPErrorResponse(t *testing.T, line string) {
	t.Helper()
	var resp Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error response, got success %+v", resp.Result)
	}
}

func acpSessionSummaryFromResponse(t *testing.T, line string) agent.SessionSummary {
	t.Helper()
	var resp Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("session response error: %+v", resp.Error)
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("session result marshal: %v", err)
	}
	var summary agent.SessionSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("session result json: %v", err)
	}
	return summary
}

func acpSessionListFromResponse(t *testing.T, line string) []agent.SessionSummary {
	t.Helper()
	var resp Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("session/list response error: %+v", resp.Error)
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("session/list result marshal: %v", err)
	}
	var list []agent.SessionSummary
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("session/list result json: %v", err)
	}
	return list
}

// responseLineForID drains the runner's output and returns the response frame
// carrying id, tolerating any notifications a turn's goroutine enqueues around
// it on no fixed schedule.
func responseLineForID(t *testing.T, r *Runner, out *bytes.Buffer, id string) string {
	t.Helper()
	r.drainForTest()
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("frame json: %v", err)
		}
		if resp.ID == id {
			return line
		}
	}
	t.Fatalf("no response with id %q among frames: %q", id, out.String())
	return ""
}

// acpFileReadContent dispatches file/read for path and returns the content,
// failing the test on any error response.
func acpFileReadContent(t *testing.T, r *Runner, out *bytes.Buffer, path string) string {
	t.Helper()
	r.dispatch(context.Background(), Request{
		JSONRPC: "2.0",
		ID:      "read",
		Method:  "file/read",
		Params:  json.RawMessage(`{"path":` + strconv.Quote(path) + `}`),
	})
	line := responseLineForID(t, r, out, "read")
	var resp Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("file/read %q errored: %+v", path, resp.Error)
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("file/read result marshal: %v", err)
	}
	var payload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("file/read result json: %v", err)
	}
	return payload.Content
}

func acpChunkContent(t *testing.T, line string) string {
	t.Helper()
	var notif Notification
	if err := json.Unmarshal([]byte(line), &notif); err != nil {
		t.Fatalf("chunk notification json: %v", err)
	}
	params, ok := notif.Params.(map[string]any)
	if !ok {
		t.Fatalf("chunk params not object: %#v", notif.Params)
	}
	content, _ := params["content"].(string)
	return content
}

func acpProjectFromResponse(t *testing.T, line string) agent.ProjectSummary {
	t.Helper()
	var resp Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("project response json: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("project response error: %+v", resp.Error)
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("project result marshal: %v", err)
	}
	var proj agent.ProjectSummary
	if err := json.Unmarshal(data, &proj); err != nil {
		t.Fatalf("project result json: %v", err)
	}
	return proj
}

func assertACPNotificationMethod(t *testing.T, line, method string) {
	t.Helper()
	var notif Notification
	if err := json.Unmarshal([]byte(line), &notif); err != nil {
		t.Fatalf("notification json: %v", err)
	}
	if notif.Method != method {
		t.Fatalf("notification method = %q, want %q", notif.Method, method)
	}
}

func turnActionResultFromResponse(t *testing.T, resp Response) agent.TurnActionResult {
	t.Helper()
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var result agent.TurnActionResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal turn action result: %v", err)
	}
	return result
}

func displayMessagesFromResponse(t *testing.T, resp Response) []agent.DisplayMessage {
	t.Helper()
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var messages []agent.DisplayMessage
	if err := json.Unmarshal(data, &messages); err != nil {
		t.Fatalf("unmarshal display messages: %v", err)
	}
	return messages
}

func sessionPayloadFromParams(t *testing.T, params any) agent.SessionPayload {
	t.Helper()
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var payload agent.SessionPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal session payload: %v", err)
	}
	return payload
}

// hydrationStateFromParams decodes a notification's wire params into the full
// captured state so tests can assert classes SessionPayload discards (model,
// queue, warnings, permissions).
func hydrationStateFromParams(t *testing.T, params any) agent.HydrationState {
	t.Helper()
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var state agent.HydrationState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal hydration state: %v", err)
	}
	return state
}

func promptWarningsFromResponse(t *testing.T, resp Response) []agent.PromptWarning {
	t.Helper()
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var warnings []agent.PromptWarning
	if err := json.Unmarshal(data, &warnings); err != nil {
		t.Fatalf("unmarshal warnings: %v", err)
	}
	return warnings
}

func acpUserMessageContents(messages []agent.DisplayMessage) []string {
	var out []string
	for _, msg := range messages {
		if msg.Type == "user" {
			out = append(out, msg.Content)
		}
	}
	return out
}

func hasPromptWarningKind(warnings []agent.PromptWarning, kind string) bool {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return true
		}
	}
	return false
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func mustReadACPSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("acp.go")
	if err != nil {
		t.Fatalf("read acp.go: %v", err)
	}
	return string(data)
}

func extractSourceFunc(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("missing function signature %q", signature)
	}
	brace := strings.IndexByte(src[start:], '{')
	if brace < 0 {
		t.Fatalf("missing body for signature %q", signature)
	}
	depth := 0
	for i := start + brace; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("unterminated function %q", signature)
	return ""
}

// waitForACPMethod drains until a notification with the given method is present
// in the output and returns its params.
func waitForACPMethod(t *testing.T, r *Runner, out *bytes.Buffer, method string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.drainForTest()
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var n Notification
			if err := json.Unmarshal([]byte(line), &n); err != nil || n.Method != method {
				continue
			}
			params, _ := n.Params.(map[string]any)
			return params
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for ACP notification %q; output: %q", method, out.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func acpParamString(t *testing.T, params map[string]any, key string) string {
	t.Helper()
	s, _ := params[key].(string)
	return s
}

func writeACPSSE(w http.ResponseWriter, payloads ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, payload := range payloads {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func acpTextChunk(id, model, content string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, id, model, content)
}

func acpStopChunk(id, model string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, id, model)
}

func acpToolCallChunk(id, model, callID, name, arguments string) string {
	argsJSON, _ := json.Marshal(arguments)
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":null}]}`, id, model, callID, name, argsJSON)
}

// TestAcceptedWorkOutlivesHostContext is the accepted-work lifetime contract:
// once the owner accepts work, that work's lifetime is the owner's; a host
// context may trigger shutdown but never severs accepted work. The matrix axes
// are cancellation source (host signal, caller context, owner shutdown,
// explicit per-session cancel) by admission state (pre-admission,
// post-admission) by work kind (direct submit, queued item, compaction, child
// turn). The nearest forbidden siblings: a cancelled caller context is still
// rejected before admission, a cancelled caller context does not sever
// already-accepted work, and owner shutdown and per-session cancel do end it.
// The protocol host's signal path is driven specifically — that is the
// reachable production case.
func TestAcceptedWorkOutlivesHostContext(t *testing.T) {
	// The host signal path: the protocol host passes its signal context into
	// the submit path, and a signal must end the accepted turn only through the
	// owner's joined shutdown — its terminal event delivered and its message
	// persisted — never by severing the turn directly.
	t.Run("source=host_signal/state=post_admission/work=direct_submit", func(t *testing.T) {
		reqSeen := make(chan struct{}, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case reqSeen <- struct{}{}:
			default:
			}
			// Hold the model call open until the owner cancels the turn; the
			// test-controlled release unblocks the handler so the server can
			// close even when the turn ends without cancelling the request.
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(func() { closeRelease(); server.Close() })

		ag := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
		// The ACP test config declares no agents.json, so the primary agent
		// type has no model; bootstrap it through the adapter-facing method.
		if err := ag.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		var out bytes.Buffer
		r := &Runner{
			agent: ag,
			owner: ag,
			in:    &onePromptThenBlockReader{line: []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"session_id":"","content":"hi"}}` + "\n")},
			out:   &out,
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		t.Cleanup(cancel)

		waitForACPRequest(t, reqSeen, "prompt turn model request")
		// Capture the session id before the signal: after the owner shutdown
		// the session store is detached, so the routing current can no longer
		// be resolved into a summary. The view's routing id itself survives.
		id, err := r.currentSession()
		if err != nil {
			t.Fatalf("current session before signal: %v", err)
		}
		cancel() // the host's signal: cancels the Run context, triggering owner shutdown

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("Run did not return after the signal")
		}

		// The accepted turn was not severed: its terminal event was delivered
		// and its message persisted, even though the host context is gone.
		params := waitForACPMethod(t, r, &out, "agent/turn_end")
		if cancelled, _ := params["cancelled"].(bool); !cancelled {
			t.Fatalf("turn_end cancelled = %v, want true (the signal ended the turn through the owner)", cancelled)
		}
		assertACPDurableContent(t, ag, id, "hi")
	})

	// The forbidden sibling on the host path: a prompt submitted with an
	// already-cancelled context is refused before admission.
	t.Run("source=host_signal/state=pre_admission/work=direct_submit", func(t *testing.T) {
		ag := newACPTestAgent(t)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); ag.ShutdownOwner() })
		ag.Init(ctx)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		cancelled, cancelCtx := context.WithCancel(context.Background())
		cancelCtx()
		var out bytes.Buffer
		r := &Runner{agent: ag, owner: ag, out: &out}
		r.handleSessionPrompt(cancelled, Request{
			JSONRPC: "2.0",
			ID:      "prompt",
			Params:  json.RawMessage(`{"session_id":"` + id + `","content":"hi"}`),
		})
		lines := drainedLines(t, r, &out, 1)
		assertACPErrorResponse(t, lines[0])
		if ag.Busy() {
			t.Fatal("a cancelled-context prompt started a turn; admission must refuse it")
		}
	})

	// The forbidden sibling on the queued path: a submit with an
	// already-cancelled context is refused before the item is admitted to the
	// queue, leaving the queue unchanged.
	t.Run("source=host_signal/state=pre_admission/work=queued_item", func(t *testing.T) {
		reqSeen := make(chan struct{}, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case reqSeen <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			writeACPSSE(w, acpTextChunk("r", "test-model", "ok"), acpStopChunk("r", "test-model"), "[DONE]")
		}))
		t.Cleanup(func() { closeRelease(); server.Close() })

		ag := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
		if err := ag.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); ag.ShutdownOwner() })
		ag.Init(ctx)
		cap := &acpEventCapture{}
		ag.SetEventHandler(cap.handler)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		// The unit is busy with a live turn, so a submit would enqueue.
		res, err := ag.SubmitToSession(ctx, id, "first")
		if err != nil {
			t.Fatalf("SubmitToSession first: %v", err)
		}
		if !res.Started {
			t.Fatalf("first turn enqueued instead of started: %+v", res)
		}
		<-reqSeen

		// An already-cancelled caller is refused before the item is admitted
		// to the queue.
		cancelled, cancelCtx := context.WithCancel(context.Background())
		cancelCtx()
		if _, err := ag.SubmitToSession(cancelled, id, "queued"); !errors.Is(err, context.Canceled) {
			t.Fatalf("SubmitToSession with a cancelled context while busy = %v, want context.Canceled (admission must refuse before the item is queued)", err)
		}
		q, err := ag.QueueSnapshotForSession(id)
		if err != nil {
			t.Fatalf("QueueSnapshotForSession: %v", err)
		}
		if len(q.Items) != 0 {
			t.Fatalf("queue after the refused submit = %d items, want none", len(q.Items))
		}
		for _, ev := range cap.snapshot() {
			if ev.Kind == agent.EventTurnStart && ev.Turn == res.Turn+1 {
				t.Fatalf("a turn_start for a second turn was delivered after the refused submit: %#v", ev)
			}
		}
	})

	// The forbidden sibling on the compaction path: a compaction requested
	// with an already-cancelled context is refused before the session is
	// marked busy.
	t.Run("source=host_signal/state=pre_admission/work=compaction", func(t *testing.T) {
		ag := newACPTestAgent(t)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); ag.ShutdownOwner() })
		ag.Init(ctx)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if _, err := ag.AppendUserMessageToSession(id, "compact me"); err != nil {
			t.Fatalf("AppendUserMessageToSession: %v", err)
		}

		cancelled, cancelCtx := context.WithCancel(context.Background())
		cancelCtx()
		if err := ag.CompactNowForSession(cancelled, id); !errors.Is(err, context.Canceled) {
			t.Fatalf("CompactNowForSession with a cancelled context = %v, want context.Canceled (admission must refuse before marking the session busy)", err)
		}
		if ag.Busy() {
			t.Fatal("the refused compaction marked the session busy")
		}
	})

	// A nil caller context is not a cancelled one: the queued branch must
	// treat it as the pre-existing behaviour did — enqueue on a busy unit,
	// never panic on it and never refuse it.
	t.Run("source=nil_context/state=pre_admission/work=queued_item", func(t *testing.T) {
		reqSeen := make(chan struct{}, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case reqSeen <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			writeACPSSE(w, acpTextChunk("r", "test-model", "ok"), acpStopChunk("r", "test-model"), "[DONE]")
		}))
		t.Cleanup(func() { closeRelease(); server.Close() })

		ag := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
		if err := ag.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); ag.ShutdownOwner() })
		ag.Init(ctx)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		// The unit is busy with a live turn, so a submit takes the queued
		// branch.
		res, err := ag.SubmitToSession(ctx, id, "first")
		if err != nil {
			t.Fatalf("SubmitToSession first: %v", err)
		}
		if !res.Started {
			t.Fatalf("first turn enqueued instead of started: %+v", res)
		}
		<-reqSeen

		// A nil context must be admitted like any live one: the item is
		// enqueued instead of panicking or being refused.
		resN, err := ag.SubmitToSession(nil, id, "queued")
		if err != nil {
			t.Fatalf("SubmitToSession(nil) while busy: %v", err)
		}
		if resN.Started {
			t.Fatalf("SubmitToSession(nil) started a turn instead of enqueueing: %+v", resN)
		}
		q, err := ag.QueueSnapshotForSession(id)
		if err != nil {
			t.Fatalf("QueueSnapshotForSession: %v", err)
		}
		if len(q.Items) != 1 || q.Items[0].Content != "queued" {
			t.Fatalf("queue after the nil-context submit = %#v, want the queued item", q.Items)
		}
	})

	// A nil caller context is not a cancelled one: the admission check must
	// treat it as the pre-existing normalisation did — admit the compaction,
	// never panic on it and never refuse it.
	t.Run("source=nil_context/state=pre_admission/work=compaction", func(t *testing.T) {
		reqSeen := make(chan struct{}, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case reqSeen <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			writeACPSSE(w, acpTextChunk("s", "test-model", "compact summary"), acpStopChunk("s", "test-model"), "[DONE]")
		}))
		t.Cleanup(func() { closeRelease(); server.Close() })

		ag := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
		if err := ag.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); ag.ShutdownOwner() })
		ag.Init(ctx)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if _, err := ag.AppendUserMessageToSession(id, "compact me"); err != nil {
			t.Fatalf("AppendUserMessageToSession: %v", err)
		}

		// A nil context must be admitted like any live one: the compaction
		// runs to completion instead of panicking or being refused.
		compactDone := make(chan error, 1)
		go func() { compactDone <- ag.CompactNowForSession(nil, id) }()
		<-reqSeen // the summarizer call is in flight: the compaction was admitted
		closeRelease()
		select {
		case err := <-compactDone:
			if err != nil {
				t.Fatalf("CompactNowForSession(nil): %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("compaction with a nil context did not complete")
		}
	})

	// A cancelled caller context does not sever an accepted direct submit: the
	// turn's context derives from the owner, so the in-flight model call
	// survives the caller's cancellation and the turn completes.
	t.Run("source=caller/state=post_admission/work=direct_submit", func(t *testing.T) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		reqSeen := make(chan struct{}, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case reqSeen <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			writeACPSSE(w, acpTextChunk("r1", "test-model", "hello back"), acpStopChunk("r1", "test-model"), "[DONE]")
		}))
		t.Cleanup(func() { closeRelease(); server.Close() })

		ag := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
		if err := ag.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); ag.ShutdownOwner() })
		ag.Init(ctx)
		cap := &acpEventCapture{}
		ag.SetEventHandler(cap.handler)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		submitCtx, submitCancel := context.WithCancel(context.Background())
		res, err := ag.SubmitToSession(submitCtx, id, "hi")
		if err != nil {
			t.Fatalf("SubmitToSession: %v", err)
		}
		if !res.Started {
			t.Fatalf("turn enqueued instead of started: %+v", res)
		}
		<-reqSeen // the model call is in flight
		submitCancel()
		closeRelease() // the turn completes despite the caller's cancellation

		// The turn continues to completion despite the caller's cancellation.
		endEv := waitForACPTurnEnd(t, cap, id, 1)
		if endEv.Cancelled {
			t.Fatalf("turn_end cancelled = true after only the caller context was cancelled; the accepted turn must not be severed")
		}
		assertACPHydratedContent(t, ag, id, "hi")
		assertACPHydratedContent(t, ag, id, "hello back")
	})

	// A queued item whose submitter's context is cancelled is still drained:
	// the drainer launches it on the owner's lifetime.
	t.Run("source=caller/state=post_admission/work=queued_item", func(t *testing.T) {
		releaseA := make(chan struct{})
		releaseB := make(chan struct{})
		var releaseAOnce, releaseBOnce sync.Once
		closeA := func() { releaseAOnce.Do(func() { close(releaseA) }) }
		closeB := func() { releaseBOnce.Do(func() { close(releaseB) }) }
		reqSeen := make(chan struct{}, 2)
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case reqSeen <- struct{}{}:
			default:
			}
			switch calls.Add(1) {
			case 1:
				select {
				case <-releaseA:
				case <-r.Context().Done():
				}
			case 2:
				select {
				case <-releaseB:
				case <-r.Context().Done():
				}
			}
			writeACPSSE(w, acpTextChunk("r", "test-model", "ok"), acpStopChunk("r", "test-model"), "[DONE]")
		}))
		t.Cleanup(func() { closeA(); closeB(); server.Close() })

		ag := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
		if err := ag.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); ag.ShutdownOwner() })
		ag.Init(ctx)
		cap := &acpEventCapture{}
		ag.SetEventHandler(cap.handler)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		resA, err := ag.SubmitToSession(ctx, id, "first")
		if err != nil {
			t.Fatalf("SubmitToSession first: %v", err)
		}
		if !resA.Started {
			t.Fatalf("first turn enqueued instead of started: %+v", resA)
		}
		<-reqSeen

		// The second submit is accepted as a queued item under a cancellable
		// caller context; cancelling that context must not drop the item.
		submitBCtx, submitBCancel := context.WithCancel(context.Background())
		resB, err := ag.SubmitToSession(submitBCtx, id, "queued")
		if err != nil {
			t.Fatalf("SubmitToSession queued: %v", err)
		}
		if resB.Started {
			t.Fatalf("second turn started instead of queued: %+v", resB)
		}
		submitBCancel()

		closeA() // turn A completes; the drainer launches the queued item
		<-reqSeen
		closeB() // turn B completes
		waitForACPTurnEnd(t, cap, id, 2)
		assertACPHydratedContent(t, ag, id, "first")
		assertACPHydratedContent(t, ag, id, "queued")
	})

	// A compaction accepted under a caller context survives that context's
	// cancellation: its context derives from the owner.
	t.Run("source=caller/state=post_admission/work=compaction", func(t *testing.T) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		reqSeen := make(chan struct{}, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case reqSeen <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			writeACPSSE(w, acpTextChunk("s", "test-model", "compact summary"), acpStopChunk("s", "test-model"), "[DONE]")
		}))
		t.Cleanup(func() { closeRelease(); server.Close() })

		ag := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
		if err := ag.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); ag.ShutdownOwner() })
		ag.Init(ctx)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if _, err := ag.AppendUserMessageToSession(id, "compact me"); err != nil {
			t.Fatalf("AppendUserMessageToSession: %v", err)
		}

		compactCtx, compactCancel := context.WithCancel(context.Background())
		compactDone := make(chan error, 1)
		go func() { compactDone <- ag.CompactNowForSession(compactCtx, id) }()
		<-reqSeen // the summarizer call is in flight
		compactCancel()
		closeRelease() // the compaction completes despite the caller's cancellation

		// The compaction was accepted and must complete despite the caller's
		// cancellation.
		select {
		case err := <-compactDone:
			if err != nil {
				t.Fatalf("CompactNowForSession: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("compaction did not complete after the caller context was cancelled")
		}
	})

	// A child turn (subagent run) accepted inside a parent turn survives the
	// caller's cancellation: the child's lifetime derives from the owner through
	// the parent's accepted turn.
	t.Run("source=caller/state=post_admission/work=child_turn", func(t *testing.T) {
		releaseChild := make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(releaseChild) }) }
		childReqSeen := make(chan struct{}, 1)
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch calls.Add(1) {
			case 1:
				writeACPSSE(w, acpToolCallChunk("p1", "test-model", "call_task", "task", `{"tasks":[{"prompt":"child work","subagent_type":"explore"}]}`), acpStopChunk("p1", "test-model"), "[DONE]")
			case 2:
				select {
				case childReqSeen <- struct{}{}:
				default:
				}
				select {
				case <-releaseChild:
				case <-r.Context().Done():
				}
				writeACPSSE(w, acpTextChunk("c1", "test-model", "CHILD_DONE"), acpStopChunk("c1", "test-model"), "[DONE]")
			case 3:
				writeACPSSE(w, acpTextChunk("p2", "test-model", "PARENT_DONE"), acpStopChunk("p2", "test-model"), "[DONE]")
			default:
				t.Fatalf("unexpected provider call %d", calls.Load())
			}
		}))
		t.Cleanup(func() { closeRelease(); server.Close() })

		ag := newACPTaskAgent(t, server.URL+"/v1")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); ag.ShutdownOwner() })
		ag.Init(ctx)
		cap := &acpEventCapture{}
		ag.SetEventHandler(cap.handler)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		submitCtx, submitCancel := context.WithCancel(context.Background())
		if _, err := ag.SubmitToSession(submitCtx, id, "delegate"); err != nil {
			t.Fatalf("SubmitToSession: %v", err)
		}
		<-childReqSeen // the child's model call is in flight
		submitCancel()

		// The child completes and the parent turn finishes normally.
		closeRelease()
		waitForACPTurnEnd(t, cap, id, 1)
		childID := acpSubagentSessionID(t, cap)
		if childID == "" {
			t.Fatal("no subagent session started")
		}
		assertACPHydratedContent(t, ag, childID, "child work")
		assertACPHydratedContent(t, ag, childID, "CHILD_DONE")
	})

	// Owner shutdown ends an accepted turn by cancelling and joining it: the
	// terminal event is delivered and the message persisted.
	t.Run("source=owner_shutdown/state=post_admission/work=direct_submit", func(t *testing.T) {
		reqSeen := make(chan struct{}, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case reqSeen <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(func() { closeRelease(); server.Close() })

		ag := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
		if err := ag.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel() })
		ag.Init(ctx)
		cap := &acpEventCapture{}
		ag.SetEventHandler(cap.handler)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		res, err := ag.SubmitToSession(ctx, id, "hi")
		if err != nil {
			t.Fatalf("SubmitToSession: %v", err)
		}
		if !res.Started {
			t.Fatalf("turn enqueued instead of started: %+v", res)
		}
		<-reqSeen

		done := make(chan struct{})
		go func() { ag.ShutdownOwner(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("ShutdownOwner did not join the in-flight turn")
		}
		endEv := waitForACPTurnEnd(t, cap, id, 1)
		if !endEv.Cancelled {
			t.Fatalf("turn_end cancelled = false after owner shutdown, want true")
		}
		// The message persisted through the clean shutdown: the live store is
		// detached once every turn finished, so the persistence fact is read
		// deliberately through the durable path.
		assertACPDurableContent(t, ag, id, "hi")
	})

	// An explicit per-session cancel still ends exactly one turn: the terminal
	// event is delivered and the message persisted.
	t.Run("source=per_session_cancel/state=post_admission/work=direct_submit", func(t *testing.T) {
		reqSeen := make(chan struct{}, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case reqSeen <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(func() { closeRelease(); server.Close() })

		ag := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
		if err := ag.SetDefaultModel("test/test-model"); err != nil {
			t.Fatalf("SetDefaultModel: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); ag.ShutdownOwner() })
		ag.Init(ctx)
		cap := &acpEventCapture{}
		ag.SetEventHandler(cap.handler)
		id, err := ag.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		res, err := ag.SubmitToSession(ctx, id, "hi")
		if err != nil {
			t.Fatalf("SubmitToSession: %v", err)
		}
		if !res.Started {
			t.Fatalf("turn enqueued instead of started: %+v", res)
		}
		<-reqSeen

		if err := ag.CancelSession(id); err != nil {
			t.Fatalf("CancelSession: %v", err)
		}
		endEv := waitForACPTurnEnd(t, cap, id, 1)
		if !endEv.Cancelled {
			t.Fatalf("turn_end cancelled = false after per-session cancel, want true")
		}
		assertACPHydratedContent(t, ag, id, "hi")
	})
}

// TestACPShutdownReachesBlockedCompaction proves Run's teardown cancels the
// work an admitted dispatch is blocked on before it joins that dispatch. A
// compaction's summarizer call derives its context from the owner, so only the
// owner's shutdown can cancel it, and the provider client carries no timeout —
// behind the dispatch join that cancellation is unreachable and teardown hangs
// forever. The cell has to be a subprocess receiving a real SIGTERM: cancelling
// the context passed to Run also cancels the context passed to Agent.Init,
// whose watcher runs ShutdownOwner immediately and cancels the compaction
// before the join can block — the non-deadlocking sibling, which production
// never reaches because the ACP host runs with context.Background and SIGTERM
// cancels only the internal signal context.
func TestACPShutdownReachesBlockedCompaction(t *testing.T) {
	if os.Getenv(acpSignalChildEnv) == "1" {
		runACPSignalShutdownChild(t)
		return
	}

	// The parent half: spawn the child, wait until its admitted compaction is
	// blocked in the summarizer call, then deliver a real SIGTERM and require
	// the child to exit. Against the join-first teardown the child hangs behind
	// the dispatch join and this times out.
	cmd := exec.Command(os.Args[0], "-test.run=^TestACPShutdownReachesBlockedCompaction$")
	cmd.Env = append(os.Environ(), acpSignalChildEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), acpSignalBlockedMarker) {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("child never reported the blocked compaction")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("signal child: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("child exited with an error after SIGTERM: %v", err)
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-waited
		t.Fatal("child did not exit after SIGTERM: teardown hung behind the admitted compaction")
	}
}

// runACPSignalShutdownChild is the child half of
// TestACPShutdownReachesBlockedCompaction. It drives the production teardown
// shape — Runner.Run with a Background host context, so only the internal
// signal context is ever cancelled — with an admitted compaction blocked in the
// summarizer call, reports readiness once the call is in flight, and returns
// when the SIGTERM-triggered teardown completes.
func runACPSignalShutdownChild(t *testing.T) {
	reqSeen := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reqSeen <- struct{}{}:
		default:
		}
		// Hold the summarizer call open. The client side of the call is what
		// the owner's shutdown cancels; the release is closed once the child's
		// teardown completes, so the server itself never outlives the test.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	ag := newACPTestAgentWithProvider(t, server.URL+"/v1", false)
	if err := ag.SetDefaultModel("test/test-model"); err != nil {
		t.Fatalf("SetDefaultModel: %v", err)
	}
	ag.Init(context.Background())
	id, err := ag.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := ag.AppendUserMessageToSession(id, "compact me"); err != nil {
		t.Fatalf("AppendUserMessageToSession: %v", err)
	}

	line := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"compact","params":{"session_id":%q}}`+"\n", id)
	r := &Runner{
		agent: ag,
		owner: ag,
		in:    &onePromptThenBlockReader{line: []byte(line)},
		out:   io.Discard,
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()

	select {
	case <-reqSeen:
	case <-time.After(15 * time.Second):
		t.Fatal("compaction was never admitted to the summarizer call")
	}
	fmt.Println(acpSignalBlockedMarker)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// onePromptThenBlockReader yields exactly one line and then blocks every
// further Read, so Run reaches teardown only through context cancellation.
type onePromptThenBlockReader struct {
	line []byte
	sent bool
}

// acpSignalChildEnv gates the child half of
// TestACPShutdownReachesBlockedCompaction, and acpSignalBlockedMarker is the
// line the child prints once its admitted compaction is blocked in the
// summarizer call.
const (
	acpSignalChildEnv      = "LIGHTCODE_ACP_SIGNAL_CHILD"
	acpSignalBlockedMarker = "LIGHTCODE_ACP_SIGNAL_CHILD_BLOCKED"
)

func (r *onePromptThenBlockReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.line), nil
	}
	select {}
}

// newACPTaskAgent builds an ACP test agent whose agents config overrides the
// primary model so subagent turns resolve to the test provider.
func newACPTaskAgent(t *testing.T, baseURL string) *agent.Agent {
	t.Helper()
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "`+baseURL+`", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lightcodeDir, "agents.json"), []byte(`{"primary": {"model": "test/test-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := lcconfig.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := agent.New(agent.Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

// acpEventCapture records agent events for the accepted-work contract.
type acpEventCapture struct {
	mu     sync.Mutex
	events []agent.Event
}

func (c *acpEventCapture) handler(ev agent.Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *acpEventCapture) snapshot() []agent.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]agent.Event(nil), c.events...)
}

// waitForACPTurnEnd waits until the want-th turn_end for sessionID is
// delivered and returns it.
func waitForACPTurnEnd(t *testing.T, cap *acpEventCapture, sessionID string, want int) agent.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		seen := 0
		for _, ev := range cap.snapshot() {
			if ev.Kind == agent.EventTurnEnd && ev.SessionID == sessionID {
				seen++
				if seen == want {
					return ev
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for turn_end %d of session %q; events: %#v", want, sessionID, cap.snapshot())
	return agent.Event{}
}

// acpSubagentSessionID returns the first subagent session id recorded in the
// capture, or "" when none was started.
func acpSubagentSessionID(t *testing.T, cap *acpEventCapture) string {
	t.Helper()
	for _, ev := range cap.snapshot() {
		if ev.Kind == agent.EventSubagentStart && ev.SubagentSessionID != "" {
			return ev.SubagentSessionID
		}
	}
	return ""
}

// assertACPHydratedContent fails unless sessionID's durable history contains a
// message with the given content.
func assertACPHydratedContent(t *testing.T, a *agent.Agent, sessionID, content string) {
	t.Helper()
	hs, err := a.HydrateSession(sessionID)
	if err != nil {
		t.Fatalf("HydrateSession(%q): %v", sessionID, err)
	}
	for _, m := range hs.Messages {
		if m.Content == content {
			return
		}
	}
	t.Fatalf("durable history for %q lacks %q: %#v", sessionID, content, hs.Messages)
}

// assertACPDurableContent fails unless sessionID's durable history contains a
// message with the given content. It is the deliberate post-shutdown read:
// SessionMessagesFor's non-live branch resolves the session's project and
// reads it read-only, which is the read that survives a clean owner shutdown
// (the live store is detached once every turn has finished, so the live
// resolution and the hydration fallback are both unavailable by design).
func assertACPDurableContent(t *testing.T, a *agent.Agent, sessionID, content string) {
	t.Helper()
	msgs, err := a.SessionMessagesFor(sessionID)
	if err != nil {
		t.Fatalf("SessionMessagesFor(%q): %v", sessionID, err)
	}
	for _, m := range msgs {
		if m.Content == content {
			return
		}
	}
	t.Fatalf("durable history for %q lacks %q: %#v", sessionID, content, msgs)
}

// waitForACPRequest waits for a request-signal on ch; the server-side helper
// for the accepted-work contract.
func waitForACPRequest(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
