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
	"github.com/MMinasyan/lightcode/internal/loop"
	"github.com/MMinasyan/lightcode/internal/process"
	"github.com/MMinasyan/lightcode/internal/prompt"
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
			for i := len(rows) - 1; i >= 0; i-- {
				if rows[i].Type == "tool" && rows[i].ID == ev.ToolCallID && !rows[i].Done {
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
		if _, err := a.SendPrompt(ctx, "hello"); err != nil {
			t.Fatalf("SendPrompt: %v", err)
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
		if _, err := a.SendPrompt(ctx, "after switch"); err != nil {
			t.Fatalf("SendPrompt: %v", err)
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
		if _, err := a.SendQueuedMessages(ctx, []string{"first", "second", "third"}); err != nil {
			t.Fatalf("SendQueuedMessages: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
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
		if _, err := a.SendQueuedMessages(ctx, []string{"alpha", "beta"}); err != nil {
			t.Fatalf("SendQueuedMessages: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
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
		if _, err := a.SendQueuedMessages(ctx, []string{"first", "next"}); err != nil {
			t.Fatalf("SendQueuedMessages: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
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
		if _, err := a.SendPrompt(ctx, "hi"); err != nil {
			t.Fatalf("SendPrompt: %v", err)
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
		if _, err := a.SendPrompt(ctx, "long task"); err != nil {
			t.Fatalf("SendPrompt: %v", err)
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

	t.Run("busy_retry_no_transcript_on_rejected_attempt", func(t *testing.T) {
		var holdReq atomic.Int32
		holdReq.Store(1)
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if holdReq.Load() == 1 {
				holdReq.Store(0)
				<-release
			}
			writeTextResponse(w, "answer")
		}))
		defer server.Close()
		defer close(release)

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)

		if _, err := a.SendPrompt(ctx, "first"); err != nil {
			t.Fatalf("SendPrompt: %v", err)
		}
		// Wait for the agent to be busy on the first turn.
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if a.Busy() {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		// Second submission must be rejected as busy.
		if _, err := a.SendQueuedMessages(ctx, []string{"rejected attempt"}); err == nil {
			t.Fatal("SendQueuedMessages while busy must error")
		}
		// Release the first turn and retry the second.
		release <- struct{}{}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		waitUntilEventOrderAgentIdle(t, a)
		if _, err := a.SendQueuedMessages(ctx, []string{"rejected attempt"}); err != nil {
			t.Fatalf("retry SendQueuedMessages: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 2)
		waitUntilEventOrderAgentIdle(t, a)
		assertProjectionsMatch(t, a, cap)
	})
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
