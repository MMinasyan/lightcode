package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/agent"
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
