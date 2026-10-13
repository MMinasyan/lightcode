//go:build linux

package desktopclient

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
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/acpclient"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/cliclient"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/protocol"
	"github.com/MMinasyan/lightcode/runtime"
	"golang.org/x/sys/unix"
)

// Each wave uses actual native tools. Opening the FIFO writer rendezvous with
// the shell's read; the two model channels park the root and child requests.
type nativeWave struct {
	fifo                      string
	rootArrived, childArrived chan struct{}
	release                   chan struct{}
	once                      sync.Once
}

func (w *nativeWave) unblock() { w.once.Do(func() { close(w.release) }) }

type nativeModel struct {
	waves           []*nativeWave
	mu              sync.Mutex
	calls, canceled int
}

func (m *nativeModel) serve(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	var wave *nativeWave
	// The native task prompt and root input carry the wave, not a synthetic
	// Session router or execution-preparation replacement.
	for i := len(request.Messages) - 1; i >= 0; i-- {
		msg := request.Messages[i]
		if msg.Role == "user" && strings.Contains(string(msg.Content), "native wave ") {
			for n, candidate := range m.waves {
				if strings.Contains(string(msg.Content), fmt.Sprintf("native wave %d", n)) {
					wave = candidate
					break
				}
			}
			break
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	last := request.Messages[len(request.Messages)-1]
	if wave != nil && request.Model == "root" && last.Role == "user" && strings.Contains(string(last.Content), "native wave ") {
		args, _ := json.Marshal(map[string]any{"command": "IFS= read -r signal < '" + wave.fifo + "'; printf 'native job done'", "background": true})
		n := 0
		for m.waves[n] != wave {
			n++
		}
		taskArgs, _ := json.Marshal(map[string]string{"prompt": fmt.Sprintf("child native wave %d", n), "subagent_type": "secondary"})
		calls := []map[string]any{
			{"index": 0, "id": "job-call", "type": "function", "function": map[string]string{"name": "run_command", "arguments": string(args)}},
			{"index": 1, "id": "child-call", "type": "function", "function": map[string]string{"name": "task", "arguments": string(taskArgs)}},
		}
		delta, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant", "tool_calls": calls}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", delta)
		return
	}
	if wave != nil {
		arrived := wave.rootArrived
		if request.Model == "child" {
			arrived = wave.childArrived
		}
		select {
		case <-arrived:
		default:
			close(arrived)
		}
		// Flush real progress before parking; only fresh owner reads may
		// describe any assistant item committed during shutdown.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"parked native activity\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-wave.release:
		case <-r.Context().Done():
			m.mu.Lock()
			m.canceled++
			m.mu.Unlock()
			return
		}
	}
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"native finished\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

// pipeTerminal runs the actual input, writer, reader and repair owners. It
// collects the sole CLI writer's output; no PTY, screen model or private CLI
// state is used to stand in for a host operation.
type pipeTerminal struct {
	in      *os.File
	done    chan error
	mu      sync.Mutex
	text    bytes.Buffer
	written chan struct{}
}

func (p *pipeTerminal) Write(data []byte) (int, error) {
	p.mu.Lock()
	n, err := p.text.Write(data)
	p.mu.Unlock()
	select {
	case p.written <- struct{}{}:
	default:
	}
	return n, err
}

func startPipeTerminal(t *testing.T, opts clienthost.Options, workspace string) *pipeTerminal {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &pipeTerminal{in: inW, done: make(chan error, 1), written: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	cli := cliclient.New(cliclient.Options{Startup: opts, Workspace: workspace})
	go func() {
		p.done <- cli.Run(ctx, inR, p)
	}()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		_ = inR.Close()
	})
	return p
}

func (p *pipeTerminal) send(t *testing.T, text string) {
	t.Helper()
	if _, err := io.WriteString(p.in, text+"\r"); err != nil {
		t.Fatal(err)
	}
}

func (p *pipeTerminal) mark() int { p.mu.Lock(); defer p.mu.Unlock(); return p.text.Len() }

var terminalANSI = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func (p *pipeTerminal) await(t *testing.T, offset int, match func(string) bool) string {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		p.mu.Lock()
		text := terminalANSI.ReplaceAllString(p.text.String()[offset:], "")
		p.mu.Unlock()
		if match(text) {
			return text
		}
		select {
		case <-p.written:
		case <-deadline:
			t.Fatalf("terminal did not produce expected output: %s", text)
		}
	}
}

func (p *pipeTerminal) hydrate(t *testing.T, id string) protocol.Hydration {
	t.Helper()
	offset := p.mark()
	p.send(t, `/runtime GetSessionHydration {"session_id":"`+id+`"}`)
	var h protocol.Hydration
	p.await(t, offset, func(text string) bool {
		for _, line := range strings.Split(text, "\r\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &h) == nil && h.Session.SessionId == id && h.SessionRevision.InstanceId != "" {
				return true
			}
		}
		return false
	})
	return h
}

type pipeACP struct {
	in     *io.PipeWriter
	frames chan map[string]json.RawMessage
	done   chan error
	next   int
	seen   []map[string]json.RawMessage
}

func startPipeACP(t *testing.T, opts clienthost.Options, workspace string) *pipeACP {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	p := &pipeACP{in: inW, frames: make(chan map[string]json.RawMessage, 512), done: make(chan error, 1)}
	runner := acpclient.New(acpclient.Options{Startup: opts, Workspace: workspace})
	go func() { p.done <- runner.Run(ctx, inR, outW); _ = outW.Close() }()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer close(p.frames)
		scanner := bufio.NewScanner(outR)
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for scanner.Scan() {
			var frame map[string]json.RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				t.Error(err)
				return
			}
			p.frames <- frame
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		_ = inR.Close()
		_ = outR.Close()
		_ = outW.Close()
		recvBounded(t, drained)
	})
	return p
}

func (p *pipeACP) call(t *testing.T, method string, params any, target any) {
	t.Helper()
	p.next++
	line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": p.next, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.in.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	for {
		frame := recvBounded(t, p.frames)
		if frame == nil {
			t.Fatal("ACP output closed")
		}
		p.seen = append(p.seen, frame)
		if string(frame["id"]) != fmt.Sprint(p.next) {
			continue
		}
		if raw := frame["error"]; len(raw) != 0 {
			t.Fatalf("ACP %s: %s", method, raw)
		}
		if target != nil {
			if err := json.Unmarshal(frame["result"], target); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
}

func (p *pipeACP) hydrate(t *testing.T, id string) protocol.Hydration {
	t.Helper()
	var h protocol.Hydration
	p.call(t, "session/select", map[string]string{"session_id": id}, &h)
	return h
}

func desktopHydrate(t *testing.T, host *Host, info ConnectionInfo, id string) protocol.Hydration {
	t.Helper()
	var h protocol.Hydration
	response, err := host.Request(info.Generation, "GET", "/v1/sessions/"+id+"/hydration", "")
	if err != nil || response.Status != http.StatusOK {
		t.Fatalf("desktop hydration: %+v, %v", response, err)
	}
	if err := json.Unmarshal([]byte(response.Body), &h); err != nil {
		t.Fatal(err)
	}
	if h.Session.SessionId != id || h.SessionRevision.InstanceId != info.InstanceID {
		t.Fatalf("foreign hydration: %+v", h.SessionRevision)
	}
	return h
}

func awaitNative(t *testing.T, read func() protocol.Hydration, match func(protocol.Hydration) bool) protocol.Hydration {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		h := read()
		if match(h) {
			return h
		}
		if time.Now().After(deadline) {
			raw, _ := json.Marshal(h)
			t.Fatalf("native work did not converge: %s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func nativeQuiescent(h protocol.Hydration) bool {
	if h.ActiveOperation != nil || len(h.Background) != 0 || len(h.Pending.Queued)+len(h.Pending.Steering) != 0 {
		return false
	}
	for _, op := range h.Operations {
		if op.Status != protocol.OperationStatusSuccess {
			return false
		}
	}
	return true
}

func openNativeFIFO(t *testing.T, path string) *os.File {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		// A nonblocking writer opens only once the native shell has reached
		// its FIFO read. Failed setup leaves no goroutine stuck in open(2).
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			f := os.NewFile(uintptr(fd), path)
			t.Cleanup(func() { _ = f.Close() })
			return f
		}
		if !errors.Is(err, unix.ENXIO) || time.Now().After(deadline) {
			t.Fatalf("native shell did not reach FIFO read: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCoordinatedHostsNativeWorkDepartureAndOwnerRepair(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	isolateBundledCredentials(t)
	t.Setenv("DESKTOP_TEST_KEY", "isolated-key")
	dataDir, workspace := t.TempDir(), t.TempDir()
	waves := make([]*nativeWave, 2)
	for n := range waves {
		waves[n] = &nativeWave{fifo: filepath.Join(workspace, fmt.Sprintf("job-%d.fifo", n)), rootArrived: make(chan struct{}), childArrived: make(chan struct{}), release: make(chan struct{})}
		if err := unix.Mkfifo(waves[n].fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		defer waves[n].unblock()
	}
	model := &nativeModel{waves: waves}
	modelServer := httptest.NewServer(http.HandlerFunc(model.serve))
	defer modelServer.Close()
	// Release model handlers before server cleanup even on a failed assertion.
	defer func() {
		for _, wave := range waves {
			wave.unblock()
		}
	}()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, fmt.Sprintf(`{"providers":{"prov":{"transport":{"base_url":%q,"api_key_env":"DESKTOP_TEST_KEY"},"discovery":false,"models":{"root":{"context_window":65536,"max_output_tokens":512},"child":{"context_window":65536,"max_output_tokens":512}}}}}`, modelServer.URL+"/v1"))
	writeServiceFile(t, agents.PathForConfig(configPath), `{"primary":{"model":"prov/root","system_prompt":"simple","tools":["run_command","task"],"lsp":false},"secondary":{"model":"prov/child","system_prompt":"simple","tools":[],"lsp":false}}`)
	var owner *runtime.Runtime
	openOwner := func() string {
		var err error
		owner, err = runtime.Open(context.Background(), runtime.Options{DataDir: dataDir, ConfigPath: configPath, Plugins: builtin.Plugins()})
		if err != nil {
			t.Fatal(err)
		}
		server, err := owner.OpenProtocol(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := server.PublishDiscovery(filepath.Join(dataDir, "runtime.json")); err != nil {
			t.Fatal(err)
		}
		return server.Endpoint()
	}
	oldEndpoint := openOwner()
	defer func() {
		if owner != nil {
			if err := owner.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}
	}()
	// While the test explicitly replaces its mounted owner, a self-exec that
	// runs no tests is a non-owning, immediately exiting contender. It opens no
	// state; common startup keeps probing the real publication across the gap.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts := clienthost.Options{DataDir: dataDir, ConfigPath: configPath, Executable: executable, PrefixArgs: []string{"-test.run=^$", "--"}}
	host := New(opts, workspace)
	host.startup(context.Background())
	defer host.shutdown(context.Background())
	info, err := host.Open()
	if err != nil {
		t.Fatal(err)
	}
	oldInstance := info.InstanceID
	created, err := host.Request(info.Generation, "POST", "/v1/sessions", fmt.Sprintf(`{"workspace":%q,"agent_type":"primary"}`, workspace))
	if err != nil || created.Status != http.StatusCreated {
		t.Fatalf("create root: %+v, %v", created, err)
	}
	var root protocol.Session
	if err := json.Unmarshal([]byte(created.Body), &root); err != nil {
		t.Fatal(err)
	}
	// Drain the actual native event pull, retaining deliveries and terminal loss.
	events := make(chan string, 256)
	lost := make(chan error, 1)
	go func() {
		for {
			raw, err := host.NextEvent(info.Generation)
			if err != nil {
				lost <- err
				return
			}
			events <- raw
		}
	}()
	acp := startPipeACP(t, opts, workspace)
	var initialized struct {
		Runtime protocol.Health `json:"runtime"`
	}
	acp.call(t, "initialize", map[string]any{}, &initialized)
	if initialized.Runtime.InstanceId != oldInstance {
		t.Fatal("ACP attached another owner")
	}
	terminal := startPipeTerminal(t, opts, workspace)
	terminal.await(t, 0, func(text string) bool {
		return strings.Contains(text, "Authoritative refreshed range — "+root.SessionId) && strings.Contains(text, "Future: primary / prov/root")
	})
	rootRead := func() protocol.Hydration { return desktopHydrate(t, host, info, root.SessionId) }
	var queuedID, childID, rootOperation string
	startWave := func(n int, terminal *pipeTerminal) (*os.File, protocol.Hydration, protocol.Hydration) {
		terminal.send(t, fmt.Sprintf("start native wave %d", n))
		recvBounded(t, waves[n].childArrived)
		recvBounded(t, waves[n].rootArrived)
		fifo := openNativeFIFO(t, waves[n].fifo)
		terminal.send(t, "\x14queued caller input")
		h := awaitNative(t, rootRead, func(h protocol.Hydration) bool {
			return h.ActiveOperation != nil && len(h.Background) == 2 && len(h.Pending.Queued) == 1
		})
		queuedID, rootOperation = h.Pending.Queued[0].OperationId, h.ActiveOperation.OperationId
		if len(queuedID) != 32 || queuedID == rootOperation {
			t.Fatalf("caller identity: %q, %q", queuedID, rootOperation)
		}
		childID = ""
		jobID := ""
		for _, member := range h.Background {
			if member.Kind == protocol.BackgroundMemberKindChild {
				childID = member.Id
			}
			if member.Kind == protocol.BackgroundMemberKindJob {
				jobID = member.Id
			}
		}
		if childID == "" || jobID == "" {
			t.Fatalf("native members: %+v", h.Background)
		}
		child := desktopHydrate(t, host, info, childID)
		if child.ActiveOperation == nil || child.Session.ParentSessionId == nil || *child.Session.ParentSessionId != root.SessionId {
			t.Fatal("native child not active or not owned by root")
		}
		if !reflect.DeepEqual(h, acp.hydrate(t, root.SessionId)) || !reflect.DeepEqual(child, acp.hydrate(t, childID)) {
			t.Fatal("desktop/ACP gated native facts differ")
		}
		return fifo, h, child
	}
	fifo, heldRoot, heldChild := startWave(0, terminal)
	_ = terminal.in.Close()
	if err := recvBounded(t, terminal.done); err != nil {
		t.Fatalf("terminal EOF: %v", err)
	}
	if !reflect.DeepEqual(heldRoot, rootRead()) || !reflect.DeepEqual(heldChild, desktopHydrate(t, host, info, childID)) {
		t.Fatal("terminal departure changed native work or pending caller")
	}
	if !reflect.DeepEqual(heldRoot, acp.hydrate(t, root.SessionId)) || !reflect.DeepEqual(heldChild, acp.hydrate(t, childID)) {
		t.Fatal("remaining ACP not healthy after terminal departure")
	}
	model.mu.Lock()
	canceled := model.canceled
	model.mu.Unlock()
	if canceled != 0 {
		t.Fatal("departure canceled a native model")
	}
	fresh := startPipeTerminal(t, opts, workspace)
	fresh.await(t, 0, func(text string) bool {
		return strings.Contains(text, "Authoritative refreshed range — "+root.SessionId)
	})
	if !reflect.DeepEqual(heldRoot, fresh.hydrate(t, root.SessionId)) || !reflect.DeepEqual(heldChild, fresh.hydrate(t, childID)) {
		t.Fatal("same-owner fresh terminal root/child reads differ")
	}
	// Actual streams delivered native Session hints to both observing hosts.
	seenDesktop := make(map[string]bool)
	for len(seenDesktop) != 2 {
		raw := recvBounded(t, events)
		var event protocol.Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatal(err)
		}
		changed, err := event.AsSessionChangedEvent()
		if err != nil {
			continue
		}
		scope, err := changed.Scope.AsSessionScope()
		if err == nil && changed.SessionRevision.InstanceId == oldInstance {
			if scope.SessionId == root.SessionId && changed.SessionRevision.DurableRevision == heldRoot.SessionRevision.DurableRevision {
				seenDesktop[root.SessionId] = true
			}
			if scope.SessionId == childID && changed.SessionRevision.DurableRevision == heldChild.SessionRevision.DurableRevision {
				seenDesktop[childID] = true
			}
		}
	}
	seenACP := make(map[string]bool)
	for _, frame := range acp.seen {
		if string(frame["method"]) == `"runtime/event"` {
			var event protocol.Event
			if err := json.Unmarshal(frame["params"], &event); err != nil {
				t.Fatal(err)
			}
			changed, err := event.AsSessionChangedEvent()
			if err != nil {
				continue
			}
			scope, err := changed.Scope.AsSessionScope()
			if err == nil && changed.SessionRevision.InstanceId == oldInstance && (scope.SessionId == root.SessionId || scope.SessionId == childID) {
				seenACP[scope.SessionId] = true
			}
		}
	}
	if len(seenACP) != 2 {
		t.Fatal("ACP did not forward native root and child events")
	}
	waves[0].unblock()
	if _, err := io.WriteString(fifo, "complete\n"); err != nil {
		t.Fatal(err)
	}
	_ = fifo.Close()
	// Terminal settlement is not an execution-retirement barrier. Read current
	// owner facts until the asynchronous completion/retirement tail converges,
	// rather than comparing another host with an earlier idle-looking sample.
	converged := awaitNative(t, rootRead, func(h protocol.Hydration) bool {
		completions := 0
		for _, item := range h.Conversation.Items {
			input, err := item.AsInputItem()
			if err == nil && input.Origin == protocol.InputOriginRuntime {
				completions++
			}
		}
		return completions == 2 && nativeQuiescent(h) && reflect.DeepEqual(h, fresh.hydrate(t, root.SessionId))
	})
	awaitNative(t, func() protocol.Hydration { return desktopHydrate(t, host, info, childID) }, func(h protocol.Hydration) bool {
		return nativeQuiescent(h) && reflect.DeepEqual(h, fresh.hydrate(t, childID))
	})
	admitted := false
	for _, op := range converged.Operations {
		if op.OperationId == queuedID {
			admitted = true
		}
	}
	if !admitted {
		t.Fatal("queued caller did not become its successful Operation")
	}
	jobCompleted := false
	for _, item := range converged.Conversation.Items {
		input, err := item.AsInputItem()
		if err != nil {
			continue
		}
		for _, part := range input.Content {
			text, err := part.AsTextPart()
			if err == nil && strings.Contains(text.Text, "finished: completed, exit code 0.") && strings.HasSuffix(text.Text, "Output:\nnative job done") {
				jobCompleted = true
			}
		}
	}
	if !jobCompleted {
		t.Fatal("native Job did not publish its successful process completion")
	}
	// A second native batch establishes active root/child/Job and an unadmitted
	// caller when the test owner explicitly closes. No client controls it.
	_, lossRoot, lossChild := startWave(1, fresh)
	lossQueued, lossChildID := queuedID, childID
	repairOffset := fresh.mark()
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	owner = nil
	if err := recvBounded(t, lost); err == nil {
		t.Fatal("desktop did not report old-owner loss")
	}
	newEndpoint := openOwner()
	newInfo, err := host.Open()
	if err != nil {
		t.Fatal(err)
	}
	if newInfo.InstanceID == oldInstance || newInfo.Generation == info.Generation {
		t.Fatal("new owner reused desktop identity/generation")
	}
	if _, err := host.Request(info.Generation, "GET", "/v1/health", ""); err == nil {
		t.Fatal("old generation survived owner replacement")
	}
	info = newInfo
	// Wait for the actual repair announcement; its snapshot is generated from
	// the new owner for ACP's last explicitly selected child.
	repairedSnapshot := false
	repairedConnected := false
	repairedNotice := false
	for !repairedSnapshot || !repairedConnected || !repairedNotice {
		frame := recvBounded(t, acp.frames)
		if frame == nil {
			t.Fatal("ACP ended during repair")
		}
		switch string(frame["method"]) {
		case `"runtime/connected"`:
			var health protocol.Health
			_ = json.Unmarshal(frame["params"], &health)
			repairedConnected = health.InstanceId == info.InstanceID
		case `"runtime/resync_required"`:
			repairedNotice = true
		case `"session/snapshot"`:
			var h protocol.Hydration
			_ = json.Unmarshal(frame["params"], &h)
			repairedSnapshot = h.SessionRevision.InstanceId == info.InstanceID && h.Session.SessionId == lossChildID
		}
	}
	fresh.await(t, repairOffset, func(text string) bool {
		return strings.Contains(text, "Connection repaired — authoritative reads; missed activity is not replayed.") &&
			strings.Contains(text, "Authoritative refreshed range — "+root.SessionId) &&
			strings.Contains(text, `"operation_id":"`+lossRoot.ActiveOperation.OperationId+`"`) &&
			strings.Contains(text, `"status":"interruption"`)
	})
	newRoot, newChild := rootRead(), desktopHydrate(t, host, info, lossChildID)
	for _, pair := range []struct{ before, after protocol.Hydration }{{lossRoot, newRoot}, {lossChild, newChild}} {
		h := pair.after
		if h.ActiveOperation != nil || len(h.Background) != 0 || len(h.Pending.Queued)+len(h.Pending.Steering) != 0 {
			t.Fatal("new owner resumed active work or recovered a pending buffer")
		}
		found := false
		for _, op := range h.Operations {
			if op.OperationId == lossQueued {
				t.Fatal("new owner admitted lost pending caller")
			}
			if op.OperationId == pair.before.ActiveOperation.OperationId {
				found = op.Status == protocol.OperationStatusInterruption
			}
		}
		if !found {
			t.Fatal("closed owner's active Operation did not settle interrupted")
		}
	}
	for _, id := range []string{root.SessionId, lossChildID} {
		h := desktopHydrate(t, host, info, id)
		if !reflect.DeepEqual(h, acp.hydrate(t, id)) || !reflect.DeepEqual(h, fresh.hydrate(t, id)) {
			t.Fatal("new-owner instance-qualified root/child reads differ")
		}
	}
	model.mu.Lock()
	calls := model.calls
	model.mu.Unlock()
	t.Logf("old owner %s instance %s; new owner %s instance %s; root %s child %s; discarded queued caller %s; native model requests %d", oldEndpoint, oldInstance, newEndpoint, info.InstanceID, root.SessionId, lossChildID, lossQueued, calls)
	_ = fresh.in.Close()
	if err := recvBounded(t, fresh.done); err != nil {
		t.Fatal(err)
	}
	_ = acp.in.Close()
	if err := recvBounded(t, acp.done); err != nil {
		t.Fatal(err)
	}
	host.shutdown(context.Background())
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	owner = nil
}
