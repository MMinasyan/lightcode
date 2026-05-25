package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	lcconfig "github.com/MMinasyan/lightcode/internal/config"
)

func TestAuthMiddleware(t *testing.T) {
	s := &Server{token: "secret"}
	called := false
	handler := s.auth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		jsonResp(w, http.StatusOK, map[string]any{"ok": true})
	})

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/v1/cancel", nil))
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("no auth code/called = %d/%v, want 401/false", rec.Code, called)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cancel", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("bad auth code/called = %d/%v, want 401/false", rec.Code, called)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret")
	handler(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("good auth code/called = %d/%v, want 200/true", rec.Code, called)
	}
}

func TestSSEHubSubscribeBroadcastUnsubscribe(t *testing.T) {
	hub := newSSEHub()
	ch1, unsub1 := hub.subscribe()
	ch2, unsub2 := hub.subscribe()
	hub.broadcast("message_chunk", map[string]any{"content": "hi"})
	for i, ch := range []<-chan []byte{ch1, ch2} {
		select {
		case msg := <-ch:
			text := string(msg)
			if !strings.Contains(text, "event: message_chunk") || !strings.Contains(text, `"content":"hi"`) {
				t.Fatalf("client %d message = %q", i, text)
			}
		default:
			t.Fatalf("client %d did not receive broadcast", i)
		}
	}
	unsub1()
	select {
	case _, ok := <-ch1:
		if ok {
			t.Fatal("unsubscribed channel still open")
		}
	default:
		t.Fatal("unsubscribed channel was not closed")
	}
	unsub2()
}

func TestSSEHubDropsForFullClientWithoutBlocking(t *testing.T) {
	hub := newSSEHub()
	ch, unsub := hub.subscribe()
	defer unsub()
	for i := 0; i < 64; i++ {
		hub.broadcast("event", map[string]any{"n": i})
	}
	hub.broadcast("event", map[string]any{"n": "dropped-if-full"})
	count := 0
	for len(ch) > 0 {
		<-ch
		count++
	}
	if count != 64 {
		t.Fatalf("buffered messages = %d, want 64", count)
	}
}

func TestHandleEventBroadcastsAndSkipsSubagents(t *testing.T) {
	hub := newSSEHub()
	ch, unsub := hub.subscribe()
	defer unsub()
	s := &Server{hub: hub}
	s.handleEvent(agent.Event{Kind: agent.EventTextDelta, Result: "hello"})
	select {
	case msg := <-ch:
		if !strings.Contains(string(msg), "event: message_chunk") || !strings.Contains(string(msg), `"content":"hello"`) {
			t.Fatalf("message event = %q", msg)
		}
	default:
		t.Fatal("message event not broadcast")
	}
	s.handleEvent(agent.Event{
		Kind:    agent.EventBackgroundProcessComplete,
		Result:  "done",
		IsError: false,
		BackgroundProcess: &agent.BackgroundProcessDisplay{
			ID:       "bg-1",
			Command:  "printf done",
			Reason:   "completed",
			ExitCode: 0,
			Output:   "done",
		},
	})
	select {
	case msg := <-ch:
		if !strings.Contains(string(msg), "event: background_process_complete") || !strings.Contains(string(msg), `"id":"bg-1"`) || !strings.Contains(string(msg), `"output":"done"`) {
			t.Fatalf("background event = %q", msg)
		}
	default:
		t.Fatal("background event not broadcast")
	}
	s.handleEvent(agent.Event{Kind: agent.EventTextDelta, Result: "skip", SubagentSessionID: "sub"})
	select {
	case msg := <-ch:
		t.Fatalf("subagent event was broadcast: %q", msg)
	default:
	}
}

func TestHandleTurnActionRevertCodeReturnsResultWithoutSessionChanged(t *testing.T) {
	a := newServerTestAgent(t)
	s := &Server{agent: a, hub: newSSEHub()}
	ch, unsub := s.hub.subscribe()
	defer unsub()

	_ = appendServerUserTurn(t, a, "first")
	path := filepath.Join(a.ProjectRoot(), "created.txt")
	clickedTurn := appendServerUserTurnWithSnapshot(t, a, "create file", path, "created\n")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/revert/code", strings.NewReader(`{"turn":`+itoa(clickedTurn)+`}`))
	s.handleRevertCode(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var result agent.TurnActionResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Action != agent.TurnActionRevertCode || result.Turn != clickedTurn || result.TargetTurn != clickedTurn-1 || result.SessionChanged {
		t.Fatalf("result = %#v, want revert_code clicked-turn result without session change", result)
	}
	if len(result.Messages) != 0 {
		t.Fatalf("revert_code messages len = %d, want no hydrated messages", len(result.Messages))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file still exists after revert code; stat err=%v", err)
	}
	assertNoSSEMessage(t, ch)
}

func TestHandleTurnActionRevertHistoryReturnsResultAndSessionChanged(t *testing.T) {
	a := newServerTestAgent(t)
	s := &Server{agent: a, hub: newSSEHub()}
	ch, unsub := s.hub.subscribe()
	defer unsub()

	_ = appendServerUserTurn(t, a, "first")
	path := filepath.Join(a.ProjectRoot(), "created.txt")
	clickedTurn := appendServerUserTurnWithSnapshot(t, a, "create file", path, "created\n")
	_ = appendServerUserTurn(t, a, "after")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/revert/history", strings.NewReader(`{"turn":`+itoa(clickedTurn)+`,"alsoRevertCode":true}`))
	s.handleRevertHistory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var result agent.TurnActionResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Action != agent.TurnActionRevertHistory || result.Turn != clickedTurn || result.TargetTurn != clickedTurn-1 || !result.SessionChanged || result.Prefill != "create file" {
		t.Fatalf("result = %#v, want revert_history clicked-turn result", result)
	}
	if got := userMessageContents(result.Messages); !equalStringSlices(got, []string{"first"}) {
		t.Fatalf("result messages = %q, want truncated history", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file still exists after history+code revert; stat err=%v", err)
	}
	assertSSEEvent(t, ch, "session_changed")
}

func TestHandleTurnActionForkReturnsResultAndSessionChanged(t *testing.T) {
	a := newServerTestAgent(t)
	s := &Server{agent: a, hub: newSSEHub()}
	ch, unsub := s.hub.subscribe()
	defer unsub()

	_ = appendServerUserTurn(t, a, "first")
	clickedTurn := appendServerUserTurn(t, a, "fork point")
	_ = appendServerUserTurn(t, a, "after")
	beforeID := a.SessionCurrent().ID

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/session/fork", strings.NewReader(`{"turn":`+itoa(clickedTurn)+`}`))
	s.handleSessionFork(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var result agent.TurnActionResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Action != agent.TurnActionFork || result.TargetTurn != clickedTurn || !result.SessionChanged || result.Session.ID == "" || result.Session.ID == beforeID {
		t.Fatalf("result = %#v, want fork result with new session", result)
	}
	if got := userMessageContents(result.Messages); !equalStringSlices(got, []string{"first", "fork point"}) {
		t.Fatalf("fork messages = %q, want selected turn included", got)
	}
	assertSSEEvent(t, ch, "session_changed")
}

func TestHandleTurnActionForkPropagatesAlsoRevertCode(t *testing.T) {
	a := newServerTestAgent(t)
	s := &Server{agent: a, hub: newSSEHub()}

	_ = appendServerUserTurn(t, a, "first")
	clickedTurn := appendServerUserTurn(t, a, "fork point")
	path := filepath.Join(a.ProjectRoot(), "created-after-fork.txt")
	_ = appendServerUserTurnWithSnapshot(t, a, "create after fork", path, "later\n")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/session/fork", strings.NewReader(`{"turn":`+itoa(clickedTurn)+`,"alsoRevertCode":true}`))
	s.handleSessionFork(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later file still exists after fork+code revert; stat err=%v", err)
	}
}

func TestHandleTurnActionInvalidJSON(t *testing.T) {
	s := &Server{agent: newServerTestAgent(t), hub: newSSEHub()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/revert/history", strings.NewReader(`{`))
	s.handleRevertHistory(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error":"invalid body"`) {
		t.Fatalf("invalid JSON response = %d %s, want 400 invalid body", rec.Code, rec.Body.String())
	}
}

func TestHTTPHandlersUseSharedTurnActionContract(t *testing.T) {
	src := mustReadServerSource(t)
	helper := extractSourceFunc(t, src, "func (s *Server) handleTurnAction(")
	if !strings.Contains(helper, ".ApplyTurnAction(") {
		t.Fatal("handleTurnAction must call ApplyTurnAction")
	}
	for _, forbidden := range []string{".ForkSession(", ".RevertCode(", ".RevertHistory("} {
		if strings.Contains(helper, forbidden) {
			t.Fatalf("handleTurnAction must not call low-level %s", forbidden)
		}
	}

	wrappers := map[string]string{
		"func (s *Server) handleSessionFork(":   "s.handleTurnAction(w, r, agent.TurnActionFork, http.StatusInternalServerError)",
		"func (s *Server) handleRevertCode(":    "s.handleTurnAction(w, r, agent.TurnActionRevertCode, http.StatusConflict)",
		"func (s *Server) handleRevertHistory(": "s.handleTurnAction(w, r, agent.TurnActionRevertHistory, http.StatusConflict)",
	}
	for signature, want := range wrappers {
		body := extractSourceFunc(t, src, signature)
		if !strings.Contains(body, want) {
			t.Fatalf("%s must delegate with %q; body:\n%s", signature, want, body)
		}
	}
}

func TestJSONHelpers(t *testing.T) {
	rec := httptest.NewRecorder()
	jsonResp(rec, http.StatusAccepted, map[string]any{"ok": true})
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"ok":true`) || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("jsonResp code/header/body = %d/%q/%q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	rec = httptest.NewRecorder()
	jsonError(rec, "bad", http.StatusBadRequest)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error":"bad"`) {
		t.Fatalf("jsonError code/body = %d/%q", rec.Code, rec.Body.String())
	}
}

func newServerTestAgent(t *testing.T) *agent.Agent {
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
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
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

func appendServerUserTurn(t *testing.T, a *agent.Agent, content string) int {
	t.Helper()
	turn, err := a.AppendUserMessage(content)
	if err != nil {
		t.Fatalf("AppendUserMessage: %v", err)
	}
	return turn
}

func appendServerUserTurnWithSnapshot(t *testing.T, a *agent.Agent, content, path, after string) int {
	t.Helper()
	turn := appendServerUserTurn(t, a, content)
	if err := a.Store().Snapshot(turn, path); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatalf("write changed file: %v", err)
	}
	return turn
}

func assertSSEEvent(t *testing.T, ch <-chan []byte, name string) {
	t.Helper()
	select {
	case msg := <-ch:
		if !strings.Contains(string(msg), "event: "+name) {
			t.Fatalf("SSE message = %q, want event %q", msg, name)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for SSE event %q", name)
	}
}

func assertNoSSEMessage(t *testing.T, ch <-chan []byte) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unexpected SSE message: %q", msg)
	default:
	}
}

func userMessageContents(messages []agent.DisplayMessage) []string {
	var out []string
	for _, msg := range messages {
		if msg.Type == "user" {
			out = append(out, msg.Content)
		}
	}
	return out
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

func mustReadServerSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
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
