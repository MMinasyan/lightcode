package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/process"
	"github.com/MMinasyan/lightcode/internal/prompt"
	"github.com/MMinasyan/lightcode/internal/tool"
)

// transcriptRow is the projection used by both projectEvents and
// projectDisplay so the two sequences can be compared with reflect.DeepEqual.
type transcriptRow struct {
	Type       string
	Content    string
	Turn       int
	ID         string
	Name       string
	Args       string
	Done       bool
	Success    bool
	Result     string
	BGID       string
	BGCommand  string
	BGReason   string
	BGOutput   string
	BGExitCode int
}

// projectEvents folds an event stream into transcript rows using the rules
// defined in the plan: TextDelta chunks coalesce into one assistant row per
// contiguous span; ToolCallStart + matching ToolCallEnd collapse into one tool
// row at the start position; UserMessageDisplay / GenericSystemSignal /
// BackgroundProcessComplete map 1:1; lifecycle/metadata events never produce a
// row.
func projectEvents(events []Event) []transcriptRow {
	var rows []transcriptRow
	var assistant []byte
	var currentTurn int
	flush := func() {
		if len(assistant) == 0 {
			return
		}
		rows = append(rows, transcriptRow{Type: "assistant", Content: string(assistant), Turn: currentTurn})
		assistant = nil
	}
	for _, ev := range events {
		switch ev.Kind {
		case EventTurnStart:
			flush()
			currentTurn = ev.Turn
		case EventTurnEnd:
			flush()
		case EventTextDelta:
			assistant = append(assistant, ev.Result...)
		case EventToolCallStart:
			flush()
			rows = append(rows, transcriptRow{Type: "tool", ID: ev.ToolCallID, Name: ev.ToolName, Args: ev.Args})
		case EventToolCallEnd:
			// Last-end-wins: a staged edit emits a second ToolCallEnd (the real
			// result, at flush) after its stage-time "Staged." end; the later end
			// overwrites the row (no !Done guard), matching the live UIs and the
			// reload <staged-flush> overlay.
			for i := len(rows) - 1; i >= 0; i-- {
				if rows[i].Type == "tool" && rows[i].ID == ev.ToolCallID {
					rows[i].Done = true
					rows[i].Success = !ev.IsError
					rows[i].Result = ev.Result
					if ev.ToolName != "" {
						rows[i].Name = ev.ToolName
					}
					if ev.Args != "" {
						rows[i].Args = ev.Args
					}
					break
				}
			}
		case EventUserMessageDisplay:
			flush()
			rows = append(rows, transcriptRow{Type: "user", Content: ev.Result, Turn: ev.Turn})
		case EventGenericSystemSignal:
			flush()
			rows = append(rows, transcriptRow{Type: "system", Content: "System: " + ev.Result})
		case EventBackgroundProcessComplete:
			flush()
			row := transcriptRow{Type: "background_process", Done: true, Success: !ev.IsError, Result: ev.Result}
			if ev.BackgroundProcess != nil {
				row.BGID = ev.BackgroundProcess.ID
				row.BGCommand = ev.BackgroundProcess.Command
				row.BGReason = ev.BackgroundProcess.Reason
				row.BGOutput = ev.BackgroundProcess.Output
				row.BGExitCode = ev.BackgroundProcess.ExitCode
				row.ID = ev.BackgroundProcess.ID
			}
			rows = append(rows, row)
		}
	}
	flush()
	return rows
}

// projectDisplay folds a SessionMessages() output into transcript rows.
func projectDisplay(msgs []DisplayMessage) []transcriptRow {
	var rows []transcriptRow
	for _, m := range msgs {
		switch m.Type {
		case "user":
			rows = append(rows, transcriptRow{Type: "user", Content: m.Content, Turn: m.Turn})
		case "assistant":
			rows = append(rows, transcriptRow{Type: "assistant", Content: m.Content, Turn: m.Turn})
		case "tool":
			rows = append(rows, transcriptRow{Type: "tool", ID: m.ID, Name: m.Name, Args: m.Args, Done: m.Done, Success: m.Success, Result: m.Result})
		case "system":
			rows = append(rows, transcriptRow{Type: "system", Content: m.Content})
		case "background_process":
			row := transcriptRow{Type: "background_process", Done: m.Done, Success: m.Success, Result: m.Result, ID: m.ID}
			if m.BackgroundProcess != nil {
				row.BGID = m.BackgroundProcess.ID
				row.BGCommand = m.BackgroundProcess.Command
				row.BGReason = m.BackgroundProcess.Reason
				row.BGOutput = m.BackgroundProcess.Output
				row.BGExitCode = m.BackgroundProcess.ExitCode
			}
			rows = append(rows, row)
		}
	}
	return rows
}

type eventCapture struct {
	mu     sync.Mutex
	events []Event
}

func (c *eventCapture) handler(ev Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *eventCapture) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

func newEventOrderAgent(t *testing.T, baseURL string) *Agent {
	t.Helper()
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test")
	configPath := filepath.Join(lightcodeDir, "config.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": %q, "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "test/test-model"
}`, baseURL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAgentsTestConfig(t, configPath, `{"primary": {"model": "test/test-model"}}`)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func startEventOrderAgent(t *testing.T, a *Agent, cap *eventCapture) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.SetEventHandler(cap.handler)
	a.Init(ctx)
	return ctx
}

func waitUntilEventOrderAgentIdle(t *testing.T, a *Agent) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !a.Busy() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("agent did not become idle")
}

// waitUntilFullyDrained waits until the agent is idle AND its backend queue is
// empty, sustained briefly so a pending auto-drain cannot still be about to
// start a turn. Used by queued scenarios where Submit enqueues and the backend
// drains across multiple turns.
func waitUntilFullyDrained(t *testing.T, a *Agent) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	stable := 0
	for time.Now().Before(deadline) {
		if !a.Busy() && len(a.QueueSnapshot().Items) == 0 {
			stable++
			if stable >= 5 {
				return
			}
		} else {
			stable = 0
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("agent did not fully drain")
}

func waitUntilEventOrderTurnEndCount(t *testing.T, cap *eventCapture, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnEnd {
				count++
			}
		}
		if count >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("did not observe %d turn-end events in time", want)
}

// chatResponse writes a minimal SSE stream that emits a single assistant text
// chunk and a [DONE] terminator.
func writeTextResponse(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`+"\n\n", content)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func writePendingEditToolCallResponse(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "text/event-stream")
	args := `{"path":"file.txt","old_string":"a","new_string":"b","pending":true}`
	fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":"edit_file","arguments":%q}}]},"finish_reason":"tool_calls"}]}`+"\n\n", id, args)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// writeHangingResponse holds the connection until ctx is cancelled to simulate
// a cancellable mid-stream model call.
func writeHangingResponse(w http.ResponseWriter, ctx context.Context) {
	w.Header().Set("Content-Type", "text/event-stream")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	<-ctx.Done()
}

func TestEventOrderEqualsMessagesForFrontend(t *testing.T) {
	t.Run("direct_submit_no_signal", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTextResponse(w, "hello back")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.Submit(ctx, "hello"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		assertProjectionsMatch(t, a, cap)
	})

	t.Run("direct_submit_with_passive_signal", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTextResponse(w, "done")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if err := a.ensureSession(); err != nil {
			t.Fatalf("ensureSession: %v", err)
		}
		a.lp.AddPendingSignal(loop.PendingSignal{Payload: "Model switched to test/test-model", Persist: true})
		if _, err := a.Submit(ctx, "after switch"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		assertProjectionsMatch(t, a, cap)
	})

	t.Run("queued_submit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTextResponse(w, "queued response")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		for _, m := range []string{"first", "second", "third"} {
			if _, err := a.Submit(ctx, m); err != nil {
				t.Fatalf("Submit: %v", err)
			}
		}
		waitUntilFullyDrained(t, a)
		assertProjectionsMatch(t, a, cap)
	})

	t.Run("queued_submit_with_passive_signal", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTextResponse(w, "ok")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if err := a.ensureSession(); err != nil {
			t.Fatalf("ensureSession: %v", err)
		}
		a.lp.AddPendingSignal(loop.PendingSignal{Payload: "Model switched to test/test-model", Persist: true})
		for _, m := range []string{"alpha", "beta"} {
			if _, err := a.Submit(ctx, m); err != nil {
				t.Fatalf("Submit: %v", err)
			}
		}
		waitUntilFullyDrained(t, a)
		assertProjectionsMatch(t, a, cap)
	})

	t.Run("queued_submit_with_background_process_signal", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTextResponse(w, "after bg")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if err := a.ensureSession(); err != nil {
			t.Fatalf("ensureSession: %v", err)
		}
		bg := &loop.BackgroundProcessDisplay{
			ID:       "bg-1",
			Command:  "printf done",
			Reason:   string(process.ExitReasonCompleted),
			ExitCode: 0,
			Output:   "done",
		}
		a.lp.AddPendingSignal(loop.PendingSignal{
			Payload:           backgroundTerminalPayload(process.ExitEvent{ID: "bg-1", Command: "printf done", Reason: process.ExitReasonCompleted, ExitCode: 0}, "done"),
			Persist:           true,
			BackgroundProcess: bg,
		})
		for _, m := range []string{"first", "next"} {
			if _, err := a.Submit(ctx, m); err != nil {
				t.Fatalf("Submit: %v", err)
			}
		}
		waitUntilFullyDrained(t, a)
		assertProjectionsMatch(t, a, cap)
	})

	t.Run("warning_before_drain_is_not_in_transcript", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTextResponse(w, "post warning")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		a.addWarning("test", promptWarning("test_kind", "test warning"))
		if _, err := a.Submit(ctx, "hi"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		assertProjectionsMatch(t, a, cap)
	})

	t.Run("cancel_during_streaming", func(t *testing.T) {
		ctxServer, cancelServer := context.WithCancel(context.Background())
		defer cancelServer()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeHangingResponse(w, ctxServer)
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.Submit(ctx, "long task"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		// Give the request a moment to reach the server, then cancel.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if a.Busy() {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		_ = a.Cancel()
		cancelServer()
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		waitUntilEventOrderAgentIdle(t, a)
		assertProjectionsMatch(t, a, cap)
	})

	t.Run("submit_while_busy_enqueues_then_drains", func(t *testing.T) {
		var holdReq atomic.Int32
		holdReq.Store(1)
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if holdReq.Swap(0) == 1 {
				<-release // hold the first turn open while we enqueue
			}
			writeTextResponse(w, "answer")
		}))
		defer server.Close()
		defer close(release)

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)

		// "first" starts a turn that hangs on the server.
		r1, err := a.Submit(ctx, "first")
		if err != nil {
			t.Fatalf("Submit first: %v", err)
		}
		if !r1.Started {
			t.Fatalf("first submit should start a turn: %#v", r1)
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if a.Busy() {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		// "second" arrives while busy: it must enqueue (not start, not error).
		r2, err := a.Submit(ctx, "second")
		if err != nil {
			t.Fatalf("Submit second while busy: %v", err)
		}
		if r2.Started || len(r2.Queue) != 1 {
			t.Fatalf("second submit while busy should enqueue: %#v", r2)
		}
		// Release the first turn; the backend then auto-drains "second".
		release <- struct{}{}
		waitUntilFullyDrained(t, a)
		assertProjectionsMatch(t, a, cap)
	})

	t.Run("staged_edit", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reqs.Add(1) == 1 {
				writePendingEditToolCallResponse(w, "call_1")
				return
			}
			writeTextResponse(w, "done")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		a.lp.SetPendingExecutor(fakeAgentPendingExecutor{results: map[string]tool.BatchResult{
			"call_1": {Success: true, Result: "Edited file.txt (1 replacement, lines 1-1)."},
		}})
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.Submit(ctx, "stage edit"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		assertProjectionsMatch(t, a, cap)
	})

	t.Run("failed_staged_edit", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reqs.Add(1) == 1 {
				writePendingEditToolCallResponse(w, "call_1")
				return
			}
			writeTextResponse(w, "done")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		a.lp.SetPendingExecutor(fakeAgentPendingExecutor{results: map[string]tool.BatchResult{
			"call_1": {Success: false, Error: "error: no match"},
		}})
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.Submit(ctx, "stage edit"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		assertProjectionsMatch(t, a, cap)
	})
}

func TestTwoLiveSessionsRunTurnsConcurrently(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		for {
			max := maxInFlight.Load()
			if current <= max || maxInFlight.CompareAndSwap(max, current) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		inFlight.Add(-1)
		writeTextResponse(w, "done")
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(release) })

	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}

	if _, err := a.SubmitToSession(ctx, firstID, "first"); err != nil {
		t.Fatalf("SubmitToSession first: %v", err)
	}
	if _, err := a.SubmitToSession(ctx, secondID, "second"); err != nil {
		t.Fatalf("SubmitToSession second: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d requests entered provider; max in flight = %d", i, maxInFlight.Load())
		}
	}
	if maxInFlight.Load() < 2 {
		t.Fatalf("max concurrent provider requests = %d, want at least 2", maxInFlight.Load())
	}
	releaseOnce.Do(func() { close(release) })
	waitUntilEventOrderTurnEndCount(t, cap, 2)
}

type fakeAgentPendingExecutor struct {
	results map[string]tool.BatchResult
}

func (f fakeAgentPendingExecutor) ExecutePending(_ context.Context, staged []tool.StagedCall) []tool.BatchResult {
	out := make([]tool.BatchResult, 0, len(staged))
	for _, call := range staged {
		if res, ok := f.results[call.ToolCallID]; ok {
			res.ToolName = call.ToolName
			res.ToolCallID = call.ToolCallID
			out = append(out, res)
			continue
		}
		out = append(out, tool.BatchResult{
			ToolName:   call.ToolName,
			ToolCallID: call.ToolCallID,
			Success:    true,
			Result:     "applied " + call.ToolCallID,
		})
	}
	return out
}

func assertProjectionsMatch(t *testing.T, a *Agent, cap *eventCapture) {
	t.Helper()
	eventRows := projectEvents(cap.snapshot())
	displayRows := projectDisplay(a.SessionMessages())
	if !reflect.DeepEqual(eventRows, displayRows) {
		t.Fatalf("transcript projection mismatch\nevents:  %s\ndisplay: %s", formatRows(eventRows), formatRows(displayRows))
	}
}

func formatRows(rows []transcriptRow) string {
	b, _ := json.MarshalIndent(rows, "", "  ")
	return string(b)
}

func promptWarning(kind, message string) prompt.Warning {
	return prompt.Warning{Kind: kind, Message: message}
}
