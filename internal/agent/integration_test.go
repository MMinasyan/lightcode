//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
)

type integrationRequest struct {
	Model    string           `json:"model"`
	Stream   bool             `json:"stream"`
	Messages []map[string]any `json:"messages"`
	Tools    []map[string]any `json:"tools"`
}

type integrationEventLog struct {
	ch chan Event
}

func newIntegrationEventLog() *integrationEventLog {
	return &integrationEventLog{ch: make(chan Event, 256)}
}

func (l *integrationEventLog) collect(ev Event) {
	select {
	case l.ch <- ev:
	default:
	}
}

func (l *integrationEventLog) waitFor(t *testing.T, kind EventKind) Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-l.ch:
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event kind %d", kind)
		}
	}
}

func (l *integrationEventLog) waitForMatching(t *testing.T, match func(Event) bool) Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-l.ch:
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for matching event")
		}
	}
}

func newIntegrationAgent(t *testing.T, baseURL string, options ...func(*integrationAgentOptions)) *Agent {
	t.Helper()
	opts := integrationAgentOptions{compactionEnabled: false, thresholdPct: 0.90}
	for _, opt := range options {
		opt(&opts)
	}
	a := newIntegrationAgentWithRoots(t, t.TempDir(), t.TempDir(), baseURL, opts)
	return a
}

type integrationAgentOptions struct {
	compactionEnabled bool
	thresholdPct      float64
}

func withIntegrationCompaction(threshold float64) func(*integrationAgentOptions) {
	return func(opts *integrationAgentOptions) {
		opts.compactionEnabled = true
		opts.thresholdPct = threshold
	}
}

func newIntegrationAgentWithRoots(t *testing.T, home, projectRoot, baseURL string, opts integrationAgentOptions) *Agent {
	t.Helper()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	configPath := filepath.Join(lightcodeDir, "config.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": %q, "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 2000, "max_output_tokens": 256, "usage_in_stream": true },
        "alt-model": { "name": "Alt Model", "context_window": 2000, "max_output_tokens": 256, "usage_in_stream": true }
      }
    }
  },
  "default_model": "test/test-model",
  "compaction": { "enabled": %t, "threshold_pct": %.4f }
}`, baseURL, opts.compactionEnabled, opts.thresholdPct)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAgentsTestConfig(t, configPath, `{"primary": {"model": "test/test-model"}}`)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home, NewMemoryEmbedder: disabledMemoryEmbedder})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

func startIntegrationAgent(t *testing.T, a *Agent) (*integrationEventLog, context.Context) {
	t.Helper()
	log := newIntegrationEventLog()
	a.SetEventHandler(log.collect)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	return log, ctx
}

func TestIntegrationDefaultMemoryEmbedder(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": { "test-model": { "name": "Test Model", "context_window": 2000, "max_output_tokens": 256 } }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAgentsTestConfig(t, configPath, `{"primary": {"model": "test/test-model"}}`)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if !a.ShutdownOwner() {
			t.Error("default embedder agent shutdown reported abandoned work")
		}
	})
	a.Init(ctx)
	if a.embedder == nil {
		t.Fatal("default memory embedder is nil")
	}
	vector, err := a.embedder.Embed("short integration text")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vector) != 384 {
		t.Fatalf("vector length = %d, want 384", len(vector))
	}
	var sumSquares float64
	for i, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			t.Fatalf("vector[%d] = %v, want finite value", i, value)
		}
		sumSquares += float64(value) * float64(value)
	}
	if norm := math.Sqrt(sumSquares); math.Abs(norm-1) > 1e-4 {
		t.Fatalf("vector norm = %f, want normalized vector", norm)
	}
}

func readIntegrationRequest(t *testing.T, r *http.Request) integrationRequest {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
		t.Fatalf("request = %s %s, want POST /chat/completions", r.Method, r.URL.Path)
	}
	var req integrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

func writeSSE(w http.ResponseWriter, payloads ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, payload := range payloads {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func textChunk(id, model, content string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, id, model, content)
}

func stopChunk(id, model string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, id, model)
}

func usageChunk(id, model string, total int) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":5,"total_tokens":%d}}`, id, model, total-5, total)
}

func toolCallChunk(id, model, callID, name, arguments string) string {
	argsJSON, _ := json.Marshal(arguments)
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":null}]}`, id, model, callID, name, argsJSON)
}

func chatResponse(content string) string {
	return fmt.Sprintf(`{"id":"summary","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, content)
}

func messageContent(req integrationRequest) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		if content, ok := msg["content"].(string); ok {
			b.WriteString(content)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func assertAssistantMessageContains(t *testing.T, a *Agent, want string) {
	t.Helper()
	for _, msg := range a.SessionMessages() {
		if msg.Type == "assistant" && strings.Contains(msg.Content, want) {
			return
		}
	}
	t.Fatalf("assistant message containing %q not found in %#v", want, a.SessionMessages())
}

func assertToolResultContains(t *testing.T, a *Agent, want string) {
	t.Helper()
	for _, msg := range a.SessionMessages() {
		if msg.Type == "tool" && strings.Contains(msg.Result, want) {
			return
		}
	}
	t.Fatalf("tool result containing %q not found in %#v", want, a.SessionMessages())
}

func TestIntegrationHappyPathTurn(t *testing.T) {
	var saw atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readIntegrationRequest(t, r)
		if req.Model != "test-model" || !req.Stream || len(req.Tools) == 0 {
			t.Fatalf("request model/stream/tools = %q/%v/%d", req.Model, req.Stream, len(req.Tools))
		}
		saw.Store(true)
		writeSSE(w, textChunk("happy", "test-model", "hello "), textChunk("happy", "test-model", "world"), stopChunk("happy", "test-model"), "[DONE]")
	}))
	t.Cleanup(server.Close)

	a := newIntegrationAgent(t, server.URL)
	log, ctx := startIntegrationAgent(t, a)
	res, err := a.Submit(ctx, "say hello")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ev := log.waitFor(t, EventTurnEnd)
	if ev.Turn != res.Turn || ev.Cancelled {
		t.Fatalf("turn end = %+v, want turn %d not cancelled", ev, res.Turn)
	}
	if !saw.Load() {
		t.Fatal("mock provider was not called")
	}
	assertAssistantMessageContains(t, a, "hello world")
}

func TestIntegrationToolCallPermissionAllowContinues(t *testing.T) {
	filePath := "allowed.txt"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readIntegrationRequest(t, r)
		switch calls.Add(1) {
		case 1:
			args := fmt.Sprintf(`{"path":%q}`, filePath)
			writeSSE(w, toolCallChunk("allow-1", "test-model", "call_read", "read_file", args), stopChunk("allow-1", "test-model"), "[DONE]")
		case 2:
			writeSSE(w, textChunk("allow-2", "test-model", "read complete"), stopChunk("allow-2", "test-model"), "[DONE]")
		default:
			t.Fatalf("unexpected provider call %d", calls.Load())
		}
	}))
	t.Cleanup(server.Close)

	a := newIntegrationAgent(t, server.URL)
	if err := os.WriteFile(filepath.Join(a.projectRoot, filePath), []byte("file contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := newIntegrationEventLog()
	a.SetEventHandler(func(ev Event) {
		log.collect(ev)
		if ev.Kind == EventPermissionRequest && ev.PermReq != nil {
			if ev.PermReq.ToolName != "read_file" {
				t.Errorf("permission tool = %q, want read_file", ev.PermReq.ToolName)
			}
			// The request event is delivered under the gate mutex, so respond off the
			// callback goroutine (as a real adapter does) rather than re-entering it.
			id := ev.PermReq.ID
			go func() { _ = a.RespondPermission(id, true) }()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	if _, err := a.Submit(ctx, "read the file"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if ev := log.waitFor(t, EventTurnEnd); ev.Cancelled {
		t.Fatalf("turn cancelled: %+v", ev)
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", calls.Load())
	}
	assertAssistantMessageContains(t, a, "read complete")
}

func TestIntegrationToolCallPermissionDenyCompletes(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readIntegrationRequest(t, r)
		switch calls.Add(1) {
		case 1:
			writeSSE(w, toolCallChunk("deny-1", "test-model", "call_read", "read_file", `{"path":"denied.txt"}`), stopChunk("deny-1", "test-model"), "[DONE]")
		case 2:
			writeSSE(w, textChunk("deny-2", "test-model", "denied by user handled"), stopChunk("deny-2", "test-model"), "[DONE]")
		default:
			t.Fatalf("unexpected provider call %d", calls.Load())
		}
	}))
	t.Cleanup(server.Close)

	a := newIntegrationAgent(t, server.URL)
	log := newIntegrationEventLog()
	a.SetEventHandler(func(ev Event) {
		log.collect(ev)
		if ev.Kind == EventPermissionRequest && ev.PermReq != nil {
			// The request event is delivered under the gate mutex, so respond off the
			// callback goroutine (as a real adapter does) rather than re-entering it.
			id := ev.PermReq.ID
			go func() { _ = a.RespondPermission(id, false) }()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	if _, err := a.Submit(ctx, "try denied read"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if ev := log.waitFor(t, EventTurnEnd); ev.Cancelled {
		t.Fatalf("turn cancelled: %+v", ev)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want deny to finish without second model call", calls.Load())
	}
	assertToolResultContains(t, a, "denied by user")
}

func TestIntegrationCancelMidStreamThenNextTurnStartsCleanly(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readIntegrationRequest(t, r)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: "+textChunk("cancel", "test-model", "partial")+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			return
		}
		writeSSE(w, textChunk("next", "test-model", "clean next turn"), stopChunk("next", "test-model"), "[DONE]")
	}))
	t.Cleanup(server.Close)

	a := newIntegrationAgent(t, server.URL)
	log, ctx := startIntegrationAgent(t, a)
	if _, err := a.Submit(ctx, "start slow stream"); err != nil {
		t.Fatalf("Submit slow: %v", err)
	}
	log.waitForMatching(t, func(ev Event) bool { return ev.Kind == EventTextDelta && ev.Result == "partial" })
	if err := a.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if ev := log.waitFor(t, EventTurnEnd); !ev.Cancelled {
		t.Fatalf("turn end = %+v, want cancelled", ev)
	}
	if _, err := a.Submit(ctx, "next"); err != nil {
		t.Fatalf("Submit next: %v", err)
	}
	if ev := log.waitFor(t, EventTurnEnd); ev.Cancelled {
		t.Fatalf("next turn cancelled: %+v", ev)
	}
	assertAssistantMessageContains(t, a, "clean next turn")
}

func TestIntegrationForkSessionContinuesConversation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readIntegrationRequest(t, r)
		call := calls.Add(1)
		writeSSE(w, textChunk(fmt.Sprintf("fork-%d", call), "test-model", fmt.Sprintf("answer %d", call)), stopChunk(fmt.Sprintf("fork-%d", call), "test-model"), "[DONE]")
	}))
	t.Cleanup(server.Close)

	a := newIntegrationAgent(t, server.URL)
	log, ctx := startIntegrationAgent(t, a)
	res, err := a.Submit(ctx, "before fork")
	if err != nil {
		t.Fatalf("Submit before: %v", err)
	}
	log.waitFor(t, EventTurnEnd)
	before := a.SessionCurrent().ID
	result, err := a.ApplyTurnActionForSession(before, res.Turn, TurnActionFork, false)
	if err != nil {
		t.Fatalf("ApplyTurnActionForSession fork: %v", err)
	}
	if result.Session.ID == "" || result.Session.ID == before {
		t.Fatalf("fork session ID = %q, before %q", result.Session.ID, before)
	}
	if _, err := a.Submit(ctx, "after fork"); err != nil {
		t.Fatalf("Submit after: %v", err)
	}
	log.waitFor(t, EventTurnEnd)
	assertAssistantMessageContains(t, a, "answer 2")
}

func TestIntegrationCompactionTriggerSavesSummary(t *testing.T) {
	var sawSummary atomic.Bool
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readIntegrationRequest(t, r)
		if len(req.Tools) == 0 {
			sawSummary.Store(true)
			writeSSE(w, textChunk("compact-summary", "test-model", "compact summary"), stopChunk("compact-summary", "test-model"), "[DONE]")
			return
		}
		call := calls.Add(1)
		writeSSE(w, textChunk(fmt.Sprintf("compact-%d", call), "test-model", fmt.Sprintf("turn %d", call)), usageChunk(fmt.Sprintf("compact-%d", call), "test-model", 1900), stopChunk(fmt.Sprintf("compact-%d", call), "test-model"), "[DONE]")
	}))
	t.Cleanup(server.Close)

	a := newIntegrationAgent(t, server.URL, withIntegrationCompaction(0.50))
	log, ctx := startIntegrationAgent(t, a)
	if _, err := a.Submit(ctx, "first"); err != nil {
		t.Fatalf("Submit first: %v", err)
	}
	log.waitFor(t, EventTurnEnd)
	if _, err := a.Submit(ctx, "second triggers compaction"); err != nil {
		t.Fatalf("Submit second: %v", err)
	}
	log.waitFor(t, EventCompactionStart)
	log.waitFor(t, EventCompactionEnd)
	log.waitFor(t, EventTurnEnd)
	if !sawSummary.Load() {
		t.Fatal("summarizer non-streaming request was not observed")
	}
	data, err := os.ReadFile(filepath.Join(a.store.Dir(), "compaction.json"))
	if err != nil {
		t.Fatalf("read compaction.json: %v", err)
	}
	if !strings.Contains(string(data), "compact summary") {
		t.Fatalf("compaction.json = %s, want compact summary", data)
	}
}

func TestIntegrationModelSwitchStripsPriorExtraFields(t *testing.T) {
	var secondBody string
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		encoded, _ := json.Marshal(raw)
		call := calls.Add(1)
		if call == 2 {
			secondBody = string(encoded)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			writeSSE(w, `{"id":"extra-1","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"extra-bearing","reasoning_content":"secret-state"},"finish_reason":null}]}`, stopChunk("extra-1", "test-model"), "[DONE]")
			return
		}
		writeSSE(w, textChunk("extra-2", "alt-model", "switched"), stopChunk("extra-2", "alt-model"), "[DONE]")
	}))
	t.Cleanup(server.Close)

	a := newIntegrationAgent(t, server.URL)
	log, ctx := startIntegrationAgent(t, a)
	if _, err := a.Submit(ctx, "first"); err != nil {
		t.Fatalf("Submit first: %v", err)
	}
	log.waitFor(t, EventTurnEnd)
	if err := a.SwitchModel("test/alt-model"); err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	if _, err := a.Submit(ctx, "after switch"); err != nil {
		t.Fatalf("Submit after switch: %v", err)
	}
	log.waitFor(t, EventTurnEnd)
	if strings.Contains(secondBody, "secret-state") || strings.Contains(secondBody, "reasoning_content") {
		t.Fatalf("switched-model request replayed source-model extra field: %s", secondBody)
	}
}

func TestIntegrationCrashRecoveryLoadsCleanHistory(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readIntegrationRequest(t, r)
		call := calls.Add(1)
		writeSSE(w, textChunk(fmt.Sprintf("recover-%d", call), "test-model", fmt.Sprintf("persisted %d", call)), stopChunk(fmt.Sprintf("recover-%d", call), "test-model"), "[DONE]")
	}))
	t.Cleanup(server.Close)
	opts := integrationAgentOptions{compactionEnabled: false, thresholdPct: 0.90}

	a1 := newIntegrationAgentWithRoots(t, home, projectRoot, server.URL, opts)
	log1, ctx1 := startIntegrationAgent(t, a1)
	if _, err := a1.Submit(ctx1, "persist before restart"); err != nil {
		t.Fatalf("Submit first agent: %v", err)
	}
	log1.waitFor(t, EventTurnEnd)
	firstSession := a1.SessionCurrent().ID

	// A crash ends the owner process, which releases its per-session flock claim
	// via the OS. Simulate that release in-process (the same one process cannot
	// otherwise drop a flock it still holds) so a fresh owner can reclaim and
	// recover the durable session persisted on disk.
	if _, err := a1.store.Close(); err != nil {
		t.Fatalf("release first agent claim: %v", err)
	}

	a2 := newIntegrationAgentWithRoots(t, home, projectRoot, server.URL, opts)
	log2, ctx2 := startIntegrationAgent(t, a2)
	if got := a2.SessionCurrent().ID; got != firstSession {
		t.Fatalf("resumed session = %q, want %q", got, firstSession)
	}
	assertAssistantMessageContains(t, a2, "persisted 1")
	if _, err := a2.Submit(ctx2, "continue after restart"); err != nil {
		t.Fatalf("Submit second agent: %v", err)
	}
	log2.waitFor(t, EventTurnEnd)
	assertAssistantMessageContains(t, a2, "persisted 2")
}

func TestIntegrationSubagentSpawnTagsEventsAndReturnsResult(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readIntegrationRequest(t, r)
		call := calls.Add(1)
		switch call {
		case 1:
			args := `{"tasks":[{"prompt":"inspect with subagent","subagent_type":"explore"}]}`
			writeSSE(w, toolCallChunk("task-1", "test-model", "call_task", "task", args), stopChunk("task-1", "test-model"), "[DONE]")
		case 2:
			if !strings.Contains(messageContent(req), "inspect with subagent") {
				t.Fatalf("subagent request messages = %#v", req.Messages)
			}
			writeSSE(w, textChunk("task-sub", "test-model", "subagent result"), stopChunk("task-sub", "test-model"), "[DONE]")
		case 3:
			writeSSE(w, textChunk("task-2", "test-model", "parent got subagent result"), stopChunk("task-2", "test-model"), "[DONE]")
		default:
			t.Fatalf("unexpected provider call %d", call)
		}
	}))
	t.Cleanup(server.Close)

	a := newIntegrationAgent(t, server.URL)
	log, ctx := startIntegrationAgent(t, a)
	if _, err := a.Submit(ctx, "delegate"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	subEv := log.waitFor(t, EventSubagentStart)
	if subEv.SubagentSessionID == "" || subEv.TaskIndex != 0 || subEv.ToolCallID != "call_task" {
		t.Fatalf("subagent event = %+v", subEv)
	}
	if ev := log.waitFor(t, EventTurnEnd); ev.Cancelled {
		t.Fatalf("turn cancelled: %+v", ev)
	}
	assertAssistantMessageContains(t, a, "parent got subagent result")
}

func TestIntegrationReadOnlySubagentRunCommandUsesParentPermission(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readIntegrationRequest(t, r)
		call := calls.Add(1)
		switch call {
		case 1:
			args := `{"tasks":[{"prompt":"inspect with command","subagent_type":"explore"}]}`
			writeSSE(w, toolCallChunk("task-1", "test-model", "call_task", "task", args), stopChunk("task-1", "test-model"), "[DONE]")
		case 2:
			if !strings.Contains(messageContent(req), "inspect with command") {
				t.Fatalf("subagent request messages = %#v", req.Messages)
			}
			writeSSE(w, toolCallChunk("task-sub-1", "test-model", "call_cmd", "run_command", `{"command":"echo subagent-ok"}`), stopChunk("task-sub-1", "test-model"), "[DONE]")
		case 3:
			if !strings.Contains(messageContent(req), "subagent-ok") {
				t.Fatalf("subagent follow-up messages = %#v, want command output", req.Messages)
			}
			writeSSE(w, textChunk("task-sub-2", "test-model", "subagent command complete"), stopChunk("task-sub-2", "test-model"), "[DONE]")
		case 4:
			if !strings.Contains(messageContent(req), "subagent command complete") {
				t.Fatalf("parent follow-up messages = %#v, want subagent result", req.Messages)
			}
			writeSSE(w, textChunk("task-2", "test-model", "parent got command result"), stopChunk("task-2", "test-model"), "[DONE]")
		case 5:
			writeSSE(w, toolCallChunk("main-cmd-1", "test-model", "call_main_cmd", "run_command", `{"command":"echo subagent-ok"}`), stopChunk("main-cmd-1", "test-model"), "[DONE]")
		case 6:
			if !strings.Contains(messageContent(req), "subagent-ok") {
				t.Fatalf("main-agent follow-up messages = %#v, want command output", req.Messages)
			}
			writeSSE(w, textChunk("main-cmd-2", "test-model", "main command complete"), stopChunk("main-cmd-2", "test-model"), "[DONE]")
		default:
			t.Fatalf("unexpected provider call %d", call)
		}
	}))
	t.Cleanup(server.Close)

	a := newIntegrationAgent(t, server.URL)
	log := newIntegrationEventLog()
	var permissionRequests atomic.Int32
	a.SetEventHandler(func(ev Event) {
		log.collect(ev)
		if ev.Kind == EventPermissionRequest && ev.PermReq != nil {
			permissionRequests.Add(1)
			if ev.PermReq.ToolName == "task" {
				t.Errorf("permission tool = task, want task launch to stay unprompted")
			}
			if ev.PermReq.ToolName != "run_command" {
				t.Errorf("permission tool = %q, want run_command", ev.PermReq.ToolName)
			}
			if ev.PermReq.Arg != "echo subagent-ok" {
				t.Errorf("permission arg = %q, want command", ev.PermReq.Arg)
			}
			suggestions := a.PermissionSuggest(ev.PermReq.ToolName, ev.PermReq.Arg)
			mainSuggestions := a.PermissionSuggest("run_command", "echo subagent-ok")
			if !reflect.DeepEqual(suggestions, mainSuggestions) {
				t.Errorf("subagent suggestions = %#v, want main-agent suggestions %#v", suggestions, mainSuggestions)
			}
			if len(suggestions) == 0 || suggestions[0].Rule != "run_command(echo subagent-ok)" {
				t.Errorf("suggestions = %#v, want exact run_command rule first", suggestions)
			}
			// The request event is delivered under the gate mutex, so respond off the
			// callback goroutine (as a real adapter does) rather than re-entering it.
			id := ev.PermReq.ID
			go func() {
				if len(suggestions) == 0 {
					_ = a.RespondPermission(id, false)
					return
				}
				_ = a.SaveProjectPermission(id, []string{suggestions[0].Rule})
			}()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)

	if _, err := a.Submit(ctx, "delegate with command"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	subEv := log.waitFor(t, EventSubagentStart)
	if subEv.SubagentSessionID == "" || subEv.TaskIndex != 0 || subEv.ToolCallID != "call_task" {
		t.Fatalf("subagent event = %+v", subEv)
	}
	cmdEv := log.waitForMatching(t, func(ev Event) bool {
		return ev.Kind == EventToolCallStart && ev.SubagentSessionID != "" && ev.ToolName == "run_command"
	})
	if cmdEv.ToolCallID != "call_cmd" {
		t.Fatalf("subagent command event = %+v, want call_cmd", cmdEv)
	}
	if ev := log.waitFor(t, EventTurnEnd); ev.Cancelled {
		t.Fatalf("turn cancelled: %+v", ev)
	}
	if permissionRequests.Load() != 1 {
		t.Fatalf("permission requests = %d, want exactly one run_command request", permissionRequests.Load())
	}
	assertAssistantMessageContains(t, a, "parent got command result")

	proj, err := a.projects.Current()
	if err != nil {
		t.Fatalf("current project: %v", err)
	}
	rules, err := permission.LoadLocal(a.projects.Root(), proj.ID)
	if err != nil {
		t.Fatalf("load local permissions: %v", err)
	}
	if len(rules.Allow) != 1 || rules.Allow[0] != "run_command(echo subagent-ok)" {
		t.Fatalf("saved allow rules = %#v, want exact run_command rule", rules.Allow)
	}

	if _, err := a.Submit(ctx, "run same command in main agent"); err != nil {
		t.Fatalf("Submit main command: %v", err)
	}
	if ev := log.waitFor(t, EventTurnEnd); ev.Cancelled {
		t.Fatalf("main command turn cancelled: %+v", ev)
	}
	if permissionRequests.Load() != 1 {
		t.Fatalf("permission requests after saved main command = %d, want still 1", permissionRequests.Load())
	}
	assertAssistantMessageContains(t, a, "main command complete")
}
