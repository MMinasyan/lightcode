package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func TestOwnerStartWritesLockAndClientAttaches(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{})
	lf, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("server did not shut down")
		}
	})

	if lf.Port == 0 || lf.Token == "" || lf.PID != os.Getpid() {
		t.Fatalf("lockfile = %+v, want port/token/current pid", lf)
	}
	got, err := Read(home)
	if err != nil {
		t.Fatalf("Read owner lock: %v", err)
	}
	if got != lf {
		t.Fatalf("owner lock = %+v, want %+v", got, lf)
	}

	client := NewClient(lf, a.ProjectRoot())
	id, err := client.NewSession("", "primary")
	if err != nil {
		t.Fatalf("client NewSession: %v", err)
	}
	summary, err := client.SessionSummaryForSession(id)
	if err != nil {
		t.Fatalf("client SessionSummaryForSession: %v", err)
	}
	if summary.ID != id {
		t.Fatalf("summary id = %q, want %q", summary.ID, id)
	}
}

func TestOwnerStartConcurrentCreatesOneLock(t *testing.T) {
	home := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu       sync.Mutex
		servers  []*Server
		started  []<-chan error
		success  int
		failures int
	)
	for i := 0; i < 2; i++ {
		servers = append(servers, New(newServerTestAgent(t), Config{}))
	}
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv *Server) {
			defer wg.Done()
			_, done, err := srv.Start(ctx, home)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures++
				return
			}
			success++
			started = append(started, done)
		}(srv)
	}
	wg.Wait()
	if success != 1 || failures != 1 {
		t.Fatalf("starts success/failure = %d/%d, want 1/1", success, failures)
	}
	if _, err := Read(home); err != nil {
		t.Fatalf("Read owner lock: %v", err)
	}
	cancel()
	for _, done := range started {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("server did not shut down")
		}
	}
}

func TestAdapterClientReceivesOwnerEvents(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{})
	lf, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("server did not shut down")
		}
	})

	client := NewClient(lf, a.ProjectRoot())
	if client.http.Timeout != 0 {
		t.Fatalf("adapter SSE client timeout = %v, want none", client.http.Timeout)
	}
	events := make(chan agent.Event, 1)
	client.SetEventHandler(func(ev agent.Event) {
		events <- ev
	})
	client.Init(ctx)

	want := agent.Event{Kind: agent.EventWarning, SessionID: "session-a", Warnings: []agent.PromptWarning{{Kind: "test", Message: "hello"}}}
	deadline := time.After(2 * time.Second)
	for {
		srv.handleEvent(want)
		select {
		case got := <-events:
			if got.Kind != want.Kind || got.SessionID != want.SessionID || len(got.Warnings) != 1 || got.Warnings[0].Message != "hello" {
				t.Fatalf("event = %+v, want %+v", got, want)
			}
			return
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for adapter event")
		}
	}
}

func TestAdapterClientBindsAttachProject(t *testing.T) {
	home := t.TempDir()
	projectA := filepath.Join(home, "project-a")
	projectB := filepath.Join(home, "project-b")
	if err := os.MkdirAll(projectA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectB, 0o700); err != nil {
		t.Fatal(err)
	}
	a := newServerTestAgentWithHomeRoot(t, home, projectA, "http://127.0.0.1:9/v1", false, "test-model")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{})
	lf, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("server did not shut down")
		}
	})

	clientA := NewClient(lf, projectA)
	sessionA, err := clientA.NewSession("", "primary")
	if err != nil {
		t.Fatalf("clientA NewSession: %v", err)
	}
	clientB := NewClient(lf, projectB)
	if list, err := clientB.SessionList("active"); err != nil {
		t.Fatalf("clientB SessionList: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("clientB SessionList before new = %v, want none", list)
	}
	sessionB, err := clientB.NewSession("", "primary")
	if err != nil {
		t.Fatalf("clientB NewSession: %v", err)
	}
	if sessionB == sessionA {
		t.Fatalf("clientB reused project A session %q", sessionB)
	}
	summaryB, err := clientB.SessionSummaryForSession(sessionB)
	if err != nil {
		t.Fatalf("clientB SessionSummaryForSession: %v", err)
	}
	if summaryB.ProjectPath != projectB {
		t.Fatalf("clientB session project = %q, want %q", summaryB.ProjectPath, projectB)
	}
	if list, err := clientB.SessionList("active"); err != nil {
		t.Fatalf("clientB SessionList after new: %v", err)
	} else if len(list) != 1 || list[0].ID != sessionB {
		t.Fatalf("clientB SessionList after new = %v, want only %s", list, sessionB)
	}
	if current := clientB.ProjectCurrent(); current.Path != projectB {
		t.Fatalf("clientB ProjectCurrent path = %q, want %q", current.Path, projectB)
	}
}

func TestOwnerStartRemovesMalformedLock(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(Path(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(home), []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{})
	_, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start with malformed lock: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("server shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("server did not shut down")
		}
	})
	if lf, err := Read(home); err != nil {
		t.Fatalf("Read replacement owner lock: %v", err)
	} else if lf.Token == "" || lf.Port == 0 {
		t.Fatalf("replacement owner lock = %+v, want token and port", lf)
	}
}

func TestOwnerDetachWithoutLiveSessionStopsOwner(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{ExitOnLastDetach: true})
	_, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	lease, err := srv.AttachAdapter()
	if err != nil {
		t.Fatalf("AttachAdapter: %v", err)
	}
	srv.DetachAdapter(lease)
	waitOwnerDone(t, done)
	if _, err := Read(home); !os.IsNotExist(err) {
		t.Fatalf("owner lock after last detach = %v, want removed", err)
	}
}

func TestAdapterClientDetachWithoutLiveSessionStopsOwner(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{ExitOnLastDetach: true})
	lf, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	client := NewClient(lf, a.ProjectRoot())
	if err := client.AttachAdapter(context.Background()); err != nil {
		t.Fatalf("AttachAdapter: %v", err)
	}
	if err := client.DetachAdapter(context.Background()); err != nil {
		t.Fatalf("DetachAdapter: %v", err)
	}
	waitOwnerDone(t, done)
	if _, err := Read(home); !os.IsNotExist(err) {
		t.Fatalf("owner lock after adapter RPC detach = %v, want removed", err)
	}
}

func TestServeNoExitOnLastDetach(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(a, Config{ExitOnLastDetach: false})
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx, home)
	}()
	waitOwnerLock(t, home)
	waitAdapterCount(t, srv, 0)
	lf, err := Read(home)
	if err != nil {
		t.Fatalf("Read owner lock: %v", err)
	}
	client := NewClient(lf, a.ProjectRoot())
	if err := client.AttachAdapter(context.Background()); err != nil {
		t.Fatalf("AttachAdapter: %v", err)
	}
	if err := client.DetachAdapter(context.Background()); err != nil {
		t.Fatalf("DetachAdapter: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("serve exited after last adapter detach with ExitOnLastDetach=false: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	waitOwnerDone(t, done)
}

func TestOwnerDetachWithLiveSessionKeepsOwner(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{})
	_, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			srv.RequestShutdown()
			waitOwnerDone(t, done)
		}
	})
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	lease, err := srv.AttachAdapter()
	if err != nil {
		t.Fatalf("AttachAdapter: %v", err)
	}
	srv.DetachAdapter(lease)
	select {
	case err := <-done:
		t.Fatalf("owner exited after live-session detach: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := Read(home); err != nil {
		t.Fatalf("owner lock after live-session detach: %v", err)
	}
}

func TestAdapterDetachRequiresLease(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{ExitOnLastDetach: true})
	lf, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			srv.RequestShutdown()
			waitOwnerDone(t, done)
		}
	})
	client := NewClient(lf, a.ProjectRoot())
	if err := client.AttachAdapter(context.Background()); err != nil {
		t.Fatalf("AttachAdapter: %v", err)
	}
	rec := httptest.NewRecorder()
	body := `{"method":"DetachAdapter","params":{"adapter_id":"stale"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/adapter/rpc", strings.NewReader(body))
	srv.handleAdapterRPC(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale detach status = %d, body = %s; want 409", rec.Code, rec.Body.String())
	}
	waitAdapterCount(t, srv, 1)
	select {
	case err := <-done:
		t.Fatalf("owner exited after stale detach: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := client.DetachAdapter(context.Background()); err != nil {
		t.Fatalf("valid DetachAdapter: %v", err)
	}
	waitOwnerDone(t, done)
	cleaned = true
}

func TestOwnerShutdownRequestRemovesLock(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{})
	lf, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	client := NewClient(lf, a.ProjectRoot())
	if err := client.RequestShutdown(context.Background()); err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	waitOwnerDone(t, done)
	if _, err := Read(home); !os.IsNotExist(err) {
		t.Fatalf("owner lock after shutdown request = %v, want removed", err)
	}
}

func TestHTTPRequiresSessionID(t *testing.T) {
	s := &Server{agent: newServerTestAgent(t), hub: newSSEHub()}
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		call   func(http.ResponseWriter, *http.Request)
	}{
		{name: "prompt", method: http.MethodPost, path: "/v1/prompt", body: `{"content":"hi"}`, call: s.handlePrompt},
		{name: "current", method: http.MethodGet, path: "/v1/session", call: s.handleSessionCurrent},
		{name: "queue", method: http.MethodGet, path: "/v1/queue", call: s.handleQueue},
		{name: "cancel", method: http.MethodPost, path: "/v1/cancel", call: s.handleCancel},
		{name: "compact", method: http.MethodPost, path: "/v1/compact", call: s.handleCompact},
		{name: "snapshots", method: http.MethodGet, path: "/v1/snapshots", call: s.handleSnapshots},
		{name: "model", method: http.MethodGet, path: "/v1/model", call: s.handleModelCurrent},
		{name: "tokens", method: http.MethodGet, path: "/v1/tokens", call: s.handleTokens},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			tc.call(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s; want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHTTPCurrentByID(t *testing.T) {
	a := newServerTestAgent(t)
	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s := &Server{agent: a, hub: newSSEHub()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/session?session_id="+id, nil)
	s.handleSessionCurrent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; want 200", rec.Code, rec.Body.String())
	}
	var summary agent.SessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.ID != id {
		t.Fatalf("session id = %q, want %q", summary.ID, id)
	}
}

func TestHTTPReadsByID(t *testing.T) {
	a := newServerTestAgent(t)
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if err := a.SwitchModelForSession(firstID, "test/alt-model"); err != nil {
		t.Fatalf("SwitchModelForSession first: %v", err)
	}
	s := &Server{agent: a, hub: newSSEHub()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/model?session_id="+firstID, nil)
	s.handleModelCurrent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("model status = %d, body = %s; want 200", rec.Code, rec.Body.String())
	}
	var firstModel agent.ModelInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &firstModel); err != nil {
		t.Fatalf("decode first model: %v", err)
	}
	if firstModel.Ref != "test/alt-model" || firstModel.ContextWindow != 4096 {
		t.Fatalf("first model = %#v, want alt model", firstModel)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/tokens?session_id="+firstID, nil)
	s.handleTokens(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tokens status = %d, body = %s; want 200", rec.Code, rec.Body.String())
	}
	var firstTokens agent.TokenReport
	if err := json.Unmarshal(rec.Body.Bytes(), &firstTokens); err != nil {
		t.Fatalf("decode first tokens: %v", err)
	}
	if firstTokens.ContextWindow != 4096 {
		t.Fatalf("first tokens context window = %d, want 4096", firstTokens.ContextWindow)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/model?session_id="+secondID, nil)
	s.handleModelCurrent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second model status = %d, body = %s; want 200", rec.Code, rec.Body.String())
	}
	var secondModel agent.ModelInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &secondModel); err != nil {
		t.Fatalf("decode second model: %v", err)
	}
	if secondModel.Ref != "test/test-model" || secondModel.ContextWindow != 8192 {
		t.Fatalf("second model = %#v, want test model", secondModel)
	}
	if current := a.SessionCurrent().ID; current != secondID {
		t.Fatalf("backend current = %q, want %q", current, secondID)
	}
}

func TestHTTPSwitchRoutesByID(t *testing.T) {
	a := newServerTestAgent(t)
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
	s := &Server{agent: a, hub: newSSEHub()}
	firstCh, unsubFirst := s.hub.subscribe(firstID)
	defer unsubFirst()
	secondCh, unsubSecond := s.hub.subscribe(secondID)
	defer unsubSecond()
	globalCh, unsubGlobal := s.hub.subscribe("")
	defer unsubGlobal()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/session/switch", strings.NewReader(`{"id":`+strconv.Quote(firstID)+`}`))
	s.handleSessionSwitch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; want 200", rec.Code, rec.Body.String())
	}
	var summary agent.SessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if summary.ID != firstID {
		t.Fatalf("response session = %q, want %q", summary.ID, firstID)
	}
	var payload agent.SessionPayload
	readSSEPayload(t, firstCh, "session_changed", &payload)
	if payload.Session.ID != firstID {
		t.Fatalf("payload session = %q, want %q", payload.Session.ID, firstID)
	}
	if got := userMessageContents(payload.Messages); !equalStringSlices(got, []string{"first"}) {
		t.Fatalf("payload messages = %#v, want first", got)
	}
	assertNoSSEMessage(t, secondCh)
	assertNoSSEMessage(t, globalCh)
	if current := a.SessionCurrent().ID; current != secondID {
		t.Fatalf("backend current = %q, want %q", current, secondID)
	}
}

func TestHTTPRemoveRoutesByID(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		call func(*Server, http.ResponseWriter, *http.Request)
	}{
		{name: "archive", path: "/v1/session/archive", call: (*Server).handleSessionArchive},
		{name: "delete", path: "/v1/session/delete", call: (*Server).handleSessionDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newServerTestAgent(t)
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
			s := &Server{agent: a, hub: newSSEHub()}
			firstCh, unsubFirst := s.hub.subscribe(firstID)
			defer unsubFirst()
			secondCh, unsubSecond := s.hub.subscribe(secondID)
			defer unsubSecond()
			globalCh, unsubGlobal := s.hub.subscribe("")
			defer unsubGlobal()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"id":`+strconv.Quote(firstID)+`}`))
			tc.call(s, rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s; want 200", rec.Code, rec.Body.String())
			}
			var payload agent.SessionPayload
			readSSEPayload(t, firstCh, "session_changed", &payload)
			if payload.Session.ID != "" || len(payload.Messages) != 0 {
				t.Fatalf("removed-session payload = %#v, want empty", payload)
			}
			assertNoSSEMessage(t, secondCh)
			assertNoSSEMessage(t, globalCh)
			if current := a.SessionCurrent().ID; current != secondID {
				t.Fatalf("backend current = %q, want %q", current, secondID)
			}
		})
	}
}

func TestSSEHubSubscribeBroadcastUnsubscribe(t *testing.T) {
	hub := newSSEHub()
	ch1, unsub1 := hub.subscribe("")
	ch2, unsub2 := hub.subscribe("")
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

func TestSSEHubSessionFilter(t *testing.T) {
	hub := newSSEHub()
	sessionA, unsubA := hub.subscribe("session-a")
	defer unsubA()
	sessionB, unsubB := hub.subscribe("session-b")
	defer unsubB()
	global, unsubGlobal := hub.subscribe("")
	defer unsubGlobal()

	hub.broadcastForSession("session-a", "message_chunk", map[string]any{"content": "owned"})
	assertSSEEvent(t, sessionA, "message_chunk")
	assertNoSSEMessage(t, sessionB)
	assertNoSSEMessage(t, global)

	hub.broadcast("warnings", []agent.PromptWarning{{Kind: "test", Message: "global"}})
	assertSSEEvent(t, sessionA, "warnings")
	assertSSEEvent(t, sessionB, "warnings")
	assertSSEEvent(t, global, "warnings")
}

func TestSSEHubDropsForFullClientWithoutBlocking(t *testing.T) {
	hub := newSSEHub()
	ch, unsub := hub.subscribe("")
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

func TestSSEHubBroadcastDoesNotHoldHubLockDuringFanout(t *testing.T) {
	hub := newSSEHub()
	_, unsub := hub.subscribe("")
	defer unsub()

	hub.mu.Lock()
	client := hub.clients[0]
	hub.mu.Unlock()

	client.mu.Lock()
	broadcastDone := make(chan struct{})
	go func() {
		hub.broadcast("event", map[string]any{"n": 1})
		close(broadcastDone)
	}()

	time.Sleep(10 * time.Millisecond)
	select {
	case <-broadcastDone:
		client.mu.Unlock()
		t.Fatal("broadcast finished while client lock was held")
	default:
	}

	subscribeDone := make(chan func(), 1)
	go func() {
		_, unsubscribe := hub.subscribe("")
		subscribeDone <- unsubscribe
	}()

	select {
	case unsubscribe := <-subscribeDone:
		unsubscribe()
	case <-time.After(200 * time.Millisecond):
		client.mu.Unlock()
		t.Fatal("subscribe blocked while broadcast fan-out was blocked on a client")
	}

	client.mu.Unlock()
	select {
	case <-broadcastDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("broadcast did not finish after client lock was released")
	}
}

func TestSSEHubConcurrentBroadcastUnsubscribeNoPanic(t *testing.T) {
	for i := 0; i < 100; i++ {
		hub := newSSEHub()
		_, unsubscribe := hub.subscribe("")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				hub.broadcast("event", map[string]any{"n": j})
			}
		}()
		go func() {
			defer wg.Done()
			unsubscribe()
		}()
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("broadcast/unsubscribe deadlocked")
		}
	}
}

func TestSSEDisconnectCleanup(t *testing.T) {
	s := &Server{
		agent:             &agent.Agent{},
		hub:               newSSEHub(),
		permTimers:        make(map[string]*time.Timer),
		permTimerSessions: make(map[string]string),
	}
	addTimer := func(id string, sessionID string) {
		timer := time.AfterFunc(time.Hour, func() {})
		t.Cleanup(func() {
			timer.Stop()
		})
		s.permTimers[id] = timer
		s.permTimerSessions[id] = sessionID
	}
	addTimer("req-a", "session-a")
	addTimer("req-b", "session-b")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil).WithContext(ctx)
	s.handleSSE(httptest.NewRecorder(), req)
	if _, ok := s.permTimers["req-a"]; !ok {
		t.Fatal("SSE without session cleaned permission timer")
	}

	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	req = httptest.NewRequest(http.MethodGet, "/v1/events?session_id=session-a", nil).WithContext(ctx)
	s.handleSSE(httptest.NewRecorder(), req)
	if _, ok := s.permTimers["req-a"]; ok {
		t.Fatal("session timer remained after matching SSE disconnect")
	}
	if _, ok := s.permTimers["req-b"]; !ok {
		t.Fatal("other session timer was cleaned")
	}
}

func TestHandleEventBroadcastsAndSkipsSubagents(t *testing.T) {
	hub := newSSEHub()
	ch, unsub := hub.subscribe("")
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
	s.handleEvent(agent.Event{Kind: agent.EventToolCallEnd, ToolCallID: "tc1", ToolName: "read_file", Args: `{"path":"x"}`, Result: "done"})
	select {
	case msg := <-ch:
		text := string(msg)
		if !strings.Contains(text, "event: tool_result") || !strings.Contains(text, `"args":"{\"path\":\"x\"}"`) {
			t.Fatalf("tool_result event = %q", msg)
		}
	default:
		t.Fatal("tool_result event not broadcast")
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

	warning := agent.PromptWarning{Kind: "catalog_discovery_failure", Message: "test: failed"}
	s.handleEvent(agent.Event{Kind: agent.EventWarning, Warnings: []agent.PromptWarning{warning}})
	select {
	case msg := <-ch:
		text := string(msg)
		if !strings.Contains(text, "event: warnings") || !strings.Contains(text, `"kind":"catalog_discovery_failure"`) || !strings.Contains(text, `"message":"test: failed"`) {
			t.Fatalf("warning event = %q", msg)
		}
	default:
		t.Fatal("warning event not broadcast")
	}
	s.handleEvent(agent.Event{Kind: agent.EventWarning})
	select {
	case msg := <-ch:
		if !strings.Contains(string(msg), "event: warnings") || !strings.Contains(string(msg), "data: []") {
			t.Fatalf("empty warning event = %q, want empty array", msg)
		}
	default:
		t.Fatal("empty warning event not broadcast")
	}
}

func TestHandleEventSessionFilter(t *testing.T) {
	hub := newSSEHub()
	sessionA, unsubA := hub.subscribe("session-a")
	defer unsubA()
	sessionB, unsubB := hub.subscribe("session-b")
	defer unsubB()
	s := &Server{hub: hub}

	s.handleEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: "session-a", Result: "hello"})
	assertSSEEvent(t, sessionA, "message_chunk")
	assertNoSSEMessage(t, sessionB)
}

func TestHandleEventBroadcastsUserMessageAndSystemSignal(t *testing.T) {
	hub := newSSEHub()
	ch, unsub := hub.subscribe("")
	defer unsub()
	s := &Server{hub: hub}

	s.handleEvent(agent.Event{Kind: agent.EventUserMessageDisplay, Turn: 4, Result: "hello"})
	select {
	case msg := <-ch:
		text := string(msg)
		if !strings.Contains(text, "event: user_message") || !strings.Contains(text, `"content":"hello"`) || !strings.Contains(text, `"turn":4`) {
			t.Fatalf("user_message event = %q", msg)
		}
	default:
		t.Fatal("user_message event not broadcast")
	}

	s.handleEvent(agent.Event{Kind: agent.EventGenericSystemSignal, Turn: 4, Result: "Model switched to x/y"})
	select {
	case msg := <-ch:
		text := string(msg)
		if !strings.Contains(text, "event: system_signal") || !strings.Contains(text, `"content":"System: Model switched to x/y"`) {
			t.Fatalf("system_signal event = %q", msg)
		}
	default:
		t.Fatal("system_signal event not broadcast")
	}
}

func TestHandleEventQueueChangedBroadcasts(t *testing.T) {
	hub := newSSEHub()
	ch, unsub := hub.subscribe("")
	defer unsub()
	s := &Server{hub: hub}

	s.handleEvent(agent.Event{
		Kind:         agent.EventQueueChanged,
		Queue:        []agent.QueuedItem{{ID: "q-1", Content: "hi"}},
		QueueVersion: 3,
	})
	select {
	case msg := <-ch:
		text := string(msg)
		if !strings.Contains(text, "event: queue_changed") || !strings.Contains(text, `"version":3`) || !strings.Contains(text, `"content":"hi"`) {
			t.Fatalf("queue_changed event = %q", msg)
		}
	default:
		t.Fatal("queue_changed event not broadcast")
	}

	s.handleEvent(agent.Event{Kind: agent.EventQueueChanged, QueueVersion: 4})
	select {
	case msg := <-ch:
		text := string(msg)
		if !strings.Contains(text, "event: queue_changed") || !strings.Contains(text, `"version":4`) || !strings.Contains(text, `"items":[]`) {
			t.Fatalf("empty queue_changed event = %q", msg)
		}
	default:
		t.Fatal("empty queue_changed event not broadcast")
	}
}

func TestHandleEventTurnEndIncludesCancelled(t *testing.T) {
	hub := newSSEHub()
	ch, unsub := hub.subscribe("")
	defer unsub()
	s := &Server{hub: hub}

	s.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Turn: 3, Cancelled: true})
	select {
	case msg := <-ch:
		text := string(msg)
		if !strings.Contains(text, "event: turn_end") || !strings.Contains(text, `"cancelled":true`) || !strings.Contains(text, `"turn":3`) {
			t.Fatalf("turn_end event = %q", msg)
		}
	default:
		t.Fatal("turn_end event not broadcast")
	}

	s.handleEvent(agent.Event{Kind: agent.EventTurnEnd, Turn: 5, Cancelled: false})
	select {
	case msg := <-ch:
		text := string(msg)
		if !strings.Contains(text, "event: turn_end") || !strings.Contains(text, `"cancelled":false`) || !strings.Contains(text, `"turn":5`) {
			t.Fatalf("turn_end non-cancelled event = %q", msg)
		}
	default:
		t.Fatal("turn_end event not broadcast")
	}
}

func TestHandleEventCompactionEndBroadcastsSessionChanged(t *testing.T) {
	a := newServerTestAgent(t)
	sessionID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s := &Server{agent: a, hub: newSSEHub()}
	ch, unsub := s.hub.subscribe(sessionID)
	defer unsub()

	s.handleEvent(agent.Event{Kind: agent.EventCompactionEnd, SessionID: sessionID, RefreshSession: true})

	assertSSEEvent(t, ch, "compaction_end")
	assertSSEEvent(t, ch, "session_changed")
	assertNoSSEMessage(t, ch)
}

func TestHandleEventActiveCompactionRefreshesSessionAfterTurnEnd(t *testing.T) {
	a := newServerTestAgent(t)
	_ = appendServerUserTurn(t, a, "complete before compaction")
	turn := a.Store().BeginTurn()
	if turn == 0 {
		t.Fatal("BeginTurn returned 0")
	}
	for _, raw := range []string{
		`{"role":"user","content":"active prompt"}`,
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}`,
		`{"role":"tool","tool_call_id":"call_1","name":"read_file","content":"ok"}`,
	} {
		if err := a.Store().AppendMessage(turn, []byte(raw)); err != nil {
			t.Fatalf("AppendMessage active: %v", err)
		}
	}
	s := &Server{agent: a, hub: newSSEHub()}
	sessionID := a.SessionCurrent().ID
	ch, unsub := s.hub.subscribe(sessionID)
	defer unsub()

	s.handleEvent(agent.Event{Kind: agent.EventCompactionEnd, SessionID: sessionID})

	assertSSEEvent(t, ch, "compaction_end")
	assertNoSSEMessage(t, ch)
	completePath := filepath.Join(a.Store().Dir(), "turns", strconv.Itoa(turn), "complete")
	if _, err := os.Stat(completePath); !os.IsNotExist(err) {
		t.Fatalf("compaction refresh mutated active turn complete marker, stat err = %v", err)
	}

	if err := a.Store().MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete active: %v", err)
	}
	s.handleEvent(agent.Event{Kind: agent.EventTurnEnd, SessionID: sessionID, Turn: turn, RefreshSession: true})
	assertSSEEvent(t, ch, "turn_end")
	select {
	case msg := <-ch:
		text := string(msg)
		if !strings.Contains(text, "event: session_changed") {
			t.Fatalf("SSE message = %q, want session_changed", msg)
		}
		if !strings.Contains(text, "active prompt") {
			t.Fatalf("session_changed after turn_end omitted completed active turn: %q", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for session_changed")
	}
}

func TestHandleTurnActionRevertCodeReturnsResultWithoutSessionChanged(t *testing.T) {
	a := newServerTestAgent(t)
	s := &Server{agent: a, hub: newSSEHub()}
	ch, unsub := s.hub.subscribe("")
	defer unsub()

	_ = appendServerUserTurn(t, a, "first")
	path := filepath.Join(a.ProjectRoot(), "created.txt")
	clickedTurn := appendServerUserTurnWithSnapshot(t, a, "create file", path, "created\n")
	sessionID := a.SessionCurrent().ID

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/revert/code", strings.NewReader(`{"session_id":`+strconv.Quote(sessionID)+`,"turn":`+itoa(clickedTurn)+`}`))
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

func TestHandleSnapshotsByID(t *testing.T) {
	a := newServerTestAgent(t)
	s := &Server{agent: a, hub: newSSEHub()}

	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	path := filepath.Join(a.ProjectRoot(), "first.txt")
	firstTurn := appendServerUserTurnWithSnapshot(t, a, "first snapshot", path, "first\n")
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if current := a.SessionCurrent().ID; current != secondID {
		t.Fatalf("current session = %q, want %q", current, secondID)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/snapshots?session_id="+firstID, nil)
	s.handleSnapshots(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var snapshots []agent.Snapshot
	if err := json.NewDecoder(rec.Body).Decode(&snapshots); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].Turn != firstTurn {
		t.Fatalf("snapshots = %#v, want first turn %d", snapshots, firstTurn)
	}
}

func TestHandleTurnActionRevertHistoryReturnsResultAndSessionChanged(t *testing.T) {
	a := newServerTestAgent(t)
	s := &Server{agent: a, hub: newSSEHub()}

	_ = appendServerUserTurn(t, a, "first")
	path := filepath.Join(a.ProjectRoot(), "created.txt")
	clickedTurn := appendServerUserTurnWithSnapshot(t, a, "create file", path, "created\n")
	_ = appendServerUserTurn(t, a, "after")
	sessionID := a.SessionCurrent().ID
	ch, unsub := s.hub.subscribe(sessionID)
	defer unsub()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/revert/history", strings.NewReader(`{"session_id":`+strconv.Quote(sessionID)+`,"turn":`+itoa(clickedTurn)+`,"alsoRevertCode":true}`))
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

	_ = appendServerUserTurn(t, a, "first")
	clickedTurn := appendServerUserTurn(t, a, "fork point")
	_ = appendServerUserTurn(t, a, "after")
	beforeID := a.SessionCurrent().ID
	ch, unsub := s.hub.subscribe(beforeID)
	defer unsub()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/session/fork", strings.NewReader(`{"session_id":`+strconv.Quote(beforeID)+`,"turn":`+itoa(clickedTurn)+`}`))
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
	var payload agent.SessionPayload
	readSSEPayload(t, ch, "session_changed", &payload)
	if payload.Session.ID != result.Session.ID {
		t.Fatalf("session_changed payload session = %q, want fork %q", payload.Session.ID, result.Session.ID)
	}
	if got := userMessageContents(payload.Messages); !equalStringSlices(got, []string{"first", "fork point"}) {
		t.Fatalf("session_changed messages = %q, want fork messages", got)
	}
}

func TestHandleTurnActionForkPropagatesAlsoRevertCode(t *testing.T) {
	a := newServerTestAgent(t)
	s := &Server{agent: a, hub: newSSEHub()}

	_ = appendServerUserTurn(t, a, "first")
	clickedTurn := appendServerUserTurn(t, a, "fork point")
	path := filepath.Join(a.ProjectRoot(), "created-after-fork.txt")
	_ = appendServerUserTurnWithSnapshot(t, a, "create after fork", path, "later\n")
	sessionID := a.SessionCurrent().ID

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/session/fork", strings.NewReader(`{"session_id":`+strconv.Quote(sessionID)+`,"turn":`+itoa(clickedTurn)+`,"alsoRevertCode":true}`))
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

func TestHandleWarningsReturnsCurrentWarningSnapshot(t *testing.T) {
	a := newServerWarningTestAgent(t)
	if len(a.CurrentWarnings()) == 0 {
		t.Fatal("warning test agent has empty startup warning snapshot")
	}
	s := &Server{agent: a, hub: newSSEHub()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/warnings", nil)
	s.handleWarnings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var warnings []agent.PromptWarning
	if err := json.NewDecoder(rec.Body).Decode(&warnings); err != nil {
		t.Fatalf("decode warnings: %v", err)
	}
	if !hasPromptWarningKind(warnings, "catalog_discovery_failure") {
		t.Fatalf("warnings = %#v, want catalog_discovery_failure", warnings)
	}
}

func TestHandleWarningsReturnsEmptyArrayForNoWarnings(t *testing.T) {
	s := &Server{agent: newServerTestAgent(t), hub: newSSEHub()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/warnings", nil)
	s.handleWarnings(rec, req)

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty warnings response = %d %q, want 200 []", rec.Code, rec.Body.String())
	}
}

func TestHandlePermissionSaveFailureKeepsTimerAndPendingRequest(t *testing.T) {
	var calls int
	var callsMu sync.Mutex
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call != 1 {
			t.Fatalf("unexpected provider call %d", call)
		}
		patch := "*** Begin Patch\n*** Add File: allowed.txt\n+ok\n*** Add File: blocked.txt\n+secret\n*** End Patch"
		args := fmt.Sprintf(`{"input":%q}`, patch)
		serverWriteSSE(w, serverToolCallChunk("save-fail-1", "gpt-5", "call_patch", "apply_patch", args), serverStopChunk("save-fail-1", "gpt-5"), "[DONE]")
	}))
	t.Cleanup(provider.Close)

	a := newServerTestAgentWithModel(t, provider.URL+"/v1", false, "gpt-5")
	s := &Server{
		agent:      a,
		hub:        newSSEHub(),
		permTimers: make(map[string]*time.Timer),
		cfg:        Config{PermissionTimeout: time.Hour},
	}
	events := make(chan agent.Event, 16)
	a.SetEventHandler(func(ev agent.Event) {
		s.handleEvent(ev)
		events <- ev
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	sessionID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	submitDone := make(chan error, 1)
	go func() {
		_, err := a.SubmitToSession(ctx, sessionID, "apply a multi-file patch")
		submitDone <- err
	}()

	permReq := waitServerPermissionRequest(t, events)
	if !permReq.DisableProjectSave {
		t.Fatal("permission request allows project save, want disabled")
	}

	rec := httptest.NewRecorder()
	body := `{"session_id":` + strconv.Quote(permReq.SessionID) + `,"id":` + strconv.Quote(permReq.ID) + `,"patterns":["apply_patch(/allowed.txt)"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/permission/save", strings.NewReader(body))
	s.handlePermissionSave(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("permission save status = %d, body = %s; want 500", rec.Code, rec.Body.String())
	}

	s.permMu.Lock()
	_, timerStillPending := s.permTimers[permReq.ID]
	s.permMu.Unlock()
	if !timerStillPending {
		t.Fatal("permission timer was cancelled after rejected project save")
	}

	s.cancelPermissionTimer(permReq.ID)
	if err := a.RespondPermissionForSession(permReq.SessionID, permReq.ID, false); err != nil {
		t.Fatalf("RespondPermission after rejected save: %v", err)
	}
	select {
	case err := <-submitDone:
		if err != nil {
			t.Fatalf("Submit after denial: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Submit stayed blocked after rejected save and explicit denial")
	}
	waitServerEventKind(t, events, agent.EventTurnEnd)
}

func TestHandlePermissionResponseFailureKeepsTimerAndPendingRequest(t *testing.T) {
	var calls int
	var callsMu sync.Mutex
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call != 1 {
			t.Fatalf("unexpected provider call %d", call)
		}
		serverWriteSSE(w, serverToolCallChunk("response-fail-1", "test-model", "call_read", "read_file", `{"path":"target.txt"}`), serverStopChunk("response-fail-1", "test-model"), "[DONE]")
	}))
	t.Cleanup(provider.Close)

	a := newServerTestAgentWithProvider(t, provider.URL+"/v1", false)
	s := &Server{
		agent:      a,
		hub:        newSSEHub(),
		permTimers: make(map[string]*time.Timer),
		cfg:        Config{PermissionTimeout: time.Hour},
	}
	events := make(chan agent.Event, 16)
	a.SetEventHandler(func(ev agent.Event) {
		s.handleEvent(ev)
		events <- ev
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	sessionID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	submitDone := make(chan error, 1)
	go func() {
		_, err := a.SubmitToSession(ctx, sessionID, "read target.txt")
		submitDone <- err
	}()

	permReq := waitServerPermissionRequest(t, events)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/permission/"+permReq.ID, strings.NewReader(`{"session_id":`+strconv.Quote(permReq.SessionID)+`}`))
	s.handlePermission(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing action status = %d, body = %s; want 400", rec.Code, rec.Body.String())
	}
	assertServerPermissionTimerPending(t, s, permReq.ID)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/permission/"+permReq.ID, strings.NewReader(`{"session_id":`+strconv.Quote(permReq.SessionID)+`,"action":"bogus"}`))
	s.handlePermission(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("invalid action status = %d, body = %s; want 404", rec.Code, rec.Body.String())
	}
	assertServerPermissionTimerPending(t, s, permReq.ID)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/permission/"+permReq.ID, strings.NewReader(`{"session_id":`+strconv.Quote(permReq.SessionID)+`,"action":"deny"}`))
	s.handlePermission(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deny status = %d, body = %s; want 200", rec.Code, rec.Body.String())
	}
	s.permMu.Lock()
	_, timerStillPending := s.permTimers[permReq.ID]
	s.permMu.Unlock()
	if timerStillPending {
		t.Fatal("permission timer remained after accepted denial")
	}
	select {
	case err := <-submitDone:
		if err != nil {
			t.Fatalf("Submit after denial: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Submit stayed blocked after invalid responses and explicit denial")
	}
	waitServerEventKind(t, events, agent.EventTurnEnd)
}

func TestHandleSessionMessagesByIDDoesNotSwitchCurrentSession(t *testing.T) {
	a := newServerTestAgent(t)
	firstTurn := appendServerUserTurn(t, a, "first session")
	firstID := a.SessionCurrent().ID
	if firstID == "" || firstTurn == 0 {
		t.Fatalf("first session id/turn = %q/%d", firstID, firstTurn)
	}
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	appendServerUserTurn(t, a, "second session")
	currentID := a.SessionCurrent().ID
	if currentID == "" || currentID == firstID {
		t.Fatalf("current session id = %q, first = %q", currentID, firstID)
	}
	s := &Server{agent: a, hub: newSSEHub()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/session/messages?id="+firstID, nil)
	s.handleSessionMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var msgs []agent.DisplayMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	if got := userMessageContents(msgs); !equalStringSlices(got, []string{"first session"}) {
		t.Fatalf("messages for first session = %q", got)
	}
	if got := a.SessionCurrent().ID; got != currentID {
		t.Fatalf("SessionMessagesFor switched current session to %q, want %q", got, currentID)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/session/messages", nil)
	s.handleSessionMessages(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing session_id status = %d, body = %s; want 400", rec.Code, rec.Body.String())
	}
}

func TestHTTPHandlersUseSharedTurnActionContract(t *testing.T) {
	src := mustReadServerSource(t)
	helper := extractSourceFunc(t, src, "func (s *Server) handleTurnAction(")
	if !strings.Contains(helper, ".ApplyTurnActionForSession(") {
		t.Fatal("handleTurnAction must call ApplyTurnActionForSession")
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
	return newServerTestAgentWithProvider(t, "http://127.0.0.1:9/v1", false)
}

func newServerWarningTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	a := newServerTestAgentWithProvider(t, server.URL+"/v1", true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	return a
}

func newServerTestAgentWithProvider(t *testing.T, baseURL string, discovery bool) *agent.Agent {
	t.Helper()
	return newServerTestAgentWithModel(t, baseURL, discovery, "test-model")
}

func newServerTestAgentWithModel(t *testing.T, baseURL string, discovery bool, modelID string) *agent.Agent {
	t.Helper()
	home := t.TempDir()
	projectRoot := t.TempDir()
	return newServerTestAgentWithHomeRoot(t, home, projectRoot, baseURL, discovery, modelID)
}

func newServerTestAgentWithHomeRoot(t *testing.T, home string, projectRoot string, baseURL string, discovery bool, modelID string) *agent.Agent {
	t.Helper()
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
        "`+modelID+`": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 },
        "alt-model": { "name": "Alt Model", "context_window": 4096, "max_output_tokens": 512 }
      }
    }
  },
  "default_model": "test/`+modelID+`"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lightcodeDir, "agents.json"), []byte(`{"primary": {"model": "test/`+modelID+`"}}`), 0o600); err != nil {
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

func waitServerPermissionRequest(t *testing.T, events <-chan agent.Event) *agent.PermissionRequest {
	t.Helper()
	for {
		select {
		case ev := <-events:
			if ev.Kind == agent.EventPermissionRequest && ev.PermReq != nil {
				return ev.PermReq
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for permission request")
		}
	}
}

func waitServerEventKind(t *testing.T, events <-chan agent.Event, kind agent.EventKind) agent.Event {
	t.Helper()
	for {
		select {
		case ev := <-events:
			if ev.Kind == kind {
				return ev
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %v", kind)
		}
	}
}

func assertServerPermissionTimerPending(t *testing.T, s *Server, id string) {
	t.Helper()
	s.permMu.Lock()
	_, ok := s.permTimers[id]
	s.permMu.Unlock()
	if !ok {
		t.Fatal("permission timer was cancelled after rejected response")
	}
}

func waitOwnerDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("owner shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not shut down")
	}
}

func waitOwnerLock(t *testing.T, home string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if _, err := Read(home); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owner lock was not written")
}

func waitAdapterCount(t *testing.T, s *Server, want int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		s.lifeMu.Lock()
		got := s.adapterCount
		s.lifeMu.Unlock()
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.lifeMu.Lock()
	got := s.adapterCount
	s.lifeMu.Unlock()
	t.Fatalf("adapter count = %d, want %d", got, want)
}

func serverWriteSSE(w http.ResponseWriter, payloads ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, payload := range payloads {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func serverToolCallChunk(id, model, callID, name, arguments string) string {
	argsJSON, _ := json.Marshal(arguments)
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":null}]}`, id, model, callID, name, argsJSON)
}

func serverStopChunk(id, model string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, id, model)
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

func readSSEPayload(t *testing.T, ch <-chan []byte, name string, out any) {
	t.Helper()
	select {
	case msg := <-ch:
		text := string(msg)
		if !strings.Contains(text, "event: "+name) {
			t.Fatalf("SSE message = %q, want event %q", msg, name)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), out); err != nil {
					t.Fatalf("decode SSE data: %v; message=%q", err, msg)
				}
				return
			}
		}
		t.Fatalf("SSE message missing data line: %q", msg)
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

func TestPermissionTimerHeadlessOnly(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{
		PermissionTimeout: 100 * time.Millisecond,
	})
	_, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		srv.RequestShutdown()
		<-done
	}()

	lease, err := srv.AttachAdapter()
	if err != nil {
		t.Fatalf("AttachAdapter: %v", err)
	}
	defer srv.DetachAdapter(lease)

	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// With an adapter attached, the timer should not arm.
	// Simulate a permission request by calling startPermissionTimer directly.
	srv.startPermissionTimer(&agent.PermissionRequest{
		ID:        "test-perm-headless",
		SessionID: "",
		ToolName:  "write_file",
	})

	time.Sleep(200 * time.Millisecond)

	// Timer should not have fired — the permission should still be respondable.
	// Verify no timer was created by checking that permTimers is empty.
	srv.permMu.Lock()
	_, hasTimer := srv.permTimers["test-perm-headless"]
	srv.permMu.Unlock()
	if hasTimer {
		t.Fatal("permission timer was armed despite attached adapter")
	}
}

func TestAttachCancelsTimers(t *testing.T) {
	var calls int
	var callsMu sync.Mutex
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 1 {
			serverWriteSSE(w, serverToolCallChunk("attach-cancel-1", "test-model", "call_read", "read_file", `{"path":"target.txt"}`), serverStopChunk("attach-cancel-1", "test-model"), "[DONE]")
			return
		}
		serverWriteSSE(w, serverStopChunk("attach-cancel-2", "test-model"), "[DONE]")
	}))
	t.Cleanup(provider.Close)

	a := newServerTestAgentWithProvider(t, provider.URL+"/v1", false)
	if err := os.WriteFile(filepath.Join(a.ProjectRoot(), "target.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := New(a, Config{PermissionTimeout: 500 * time.Millisecond})
	events := make(chan agent.Event, 16)
	a.SetEventHandler(func(ev agent.Event) {
		srv.handleEvent(ev)
		events <- ev
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	sessionID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	submitDone := make(chan error, 1)
	go func() {
		_, err := a.SubmitToSession(ctx, sessionID, "read target.txt")
		submitDone <- err
	}()

	permReq := waitServerPermissionRequest(t, events)
	assertServerPermissionTimerPending(t, srv, permReq.ID)
	lease, err := srv.AttachAdapter()
	if err != nil {
		t.Fatalf("AttachAdapter: %v", err)
	}
	t.Cleanup(func() { srv.DetachAdapter(lease) })
	time.Sleep(700 * time.Millisecond)
	srv.permMu.Lock()
	_, timerStillPending := srv.permTimers[permReq.ID]
	srv.permMu.Unlock()
	if timerStillPending {
		t.Fatal("permission timer remained after adapter attach")
	}
	if err := a.RespondPermissionForSession(permReq.SessionID, permReq.ID, true); err != nil {
		t.Fatalf("RespondPermission after attach cancelled timer: %v", err)
	}
	select {
	case err := <-submitDone:
		if err != nil {
			t.Fatalf("Submit after permission allow: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit stayed blocked after attach-cancelled timer and allow")
	}
	waitServerEventKind(t, events, agent.EventTurnEnd)
}

// TestTimerAttachRace exercises the concurrent arm/attach path. The bug is
// logical atomicity (a timer registered after attach's cancel slipped through
// the non-atomic check→register gap), not a data race, so -race will not catch
// it on pre-fix code. This stress test runs arm and attach/detach loops
// concurrently and then asserts no timer survives a final attach. It is a
// regression pin, not a deterministic fail-before — the interleaving window is
// sub-µs and cannot be forced without a production test seam.
func TestTimerAttachRace(t *testing.T) {
	a := newServerTestAgent(t)
	srv := New(a, Config{PermissionTimeout: time.Hour}) // long: we test survival, not firing

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				srv.startPermissionTimer(&agent.PermissionRequest{
					ID:       fmt.Sprintf("race-%d", i),
					ToolName: "write_file",
				})
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				lease, err := srv.AttachAdapter()
				if err != nil {
					return
				}
				srv.DetachAdapter(lease)
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	// A final attach must find zero surviving timers: under the fix, every
	// arm that passed the lease-check also registered under the same lifeMu
	// hold, so the cancel in attach reached it.
	lease, err := srv.AttachAdapter()
	if err != nil {
		t.Fatalf("final AttachAdapter: %v", err)
	}
	defer srv.DetachAdapter(lease)
	srv.permMu.Lock()
	remaining := len(srv.permTimers)
	srv.permMu.Unlock()
	if remaining > 0 {
		t.Fatalf("%d permission timers survived an adapter attach", remaining)
	}
}

func TestExitOnLastDetach(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{ExitOnLastDetach: true})
	_, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Open a session — the old condition checked LiveSessionCount and would
	// prevent exit even with no adapters. With the fix, only adapter count matters.
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	lease, err := srv.AttachAdapter()
	if err != nil {
		t.Fatalf("AttachAdapter: %v", err)
	}
	srv.DetachAdapter(lease)
	waitOwnerDone(t, done)
	if _, err := Read(home); !os.IsNotExist(err) {
		t.Fatalf("owner lock after last detach = %v, want removed", err)
	}
}

func TestTwoLeasesKeepAlive(t *testing.T) {
	home := t.TempDir()
	a := newServerTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{ExitOnLastDetach: true})
	_, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopped := false
	defer func() {
		if stopped {
			return
		}
		srv.RequestShutdown()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Log("owner did not shut down during cleanup")
		}
	}()

	first, err := srv.AttachAdapter()
	if err != nil {
		t.Fatalf("AttachAdapter first: %v", err)
	}
	second, err := srv.AttachAdapter()
	if err != nil {
		t.Fatalf("AttachAdapter second: %v", err)
	}
	if !srv.DetachAdapter(first) {
		t.Fatal("DetachAdapter first returned false")
	}
	waitAdapterCount(t, srv, 1)
	select {
	case err := <-done:
		t.Fatalf("owner exited while one lease remained: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if !srv.DetachAdapter(second) {
		t.Fatal("DetachAdapter second returned false")
	}
	waitOwnerDone(t, done)
	stopped = true
}

func TestExitMidTurn(t *testing.T) {
	entered := make(chan struct{}, 1)
	providerCancelled := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		select {
		case providerCancelled <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(provider.Close)

	home := t.TempDir()
	a := newServerTestAgentWithProvider(t, provider.URL+"/v1", false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(a, Config{ExitOnLastDetach: true})
	_, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sessionID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	lease, err := srv.AttachAdapter()
	if err != nil {
		t.Fatalf("AttachAdapter: %v", err)
	}

	if res, err := a.SubmitToSession(ctx, sessionID, "hang until owner exits"); err != nil || !res.Started {
		t.Fatalf("Submit = %#v, %v; want started turn", res, err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not start")
	}
	if !srv.DetachAdapter(lease) {
		t.Fatal("DetachAdapter returned false")
	}
	waitOwnerDone(t, done)
	select {
	case <-providerCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request was not cancelled on owner shutdown")
	}
	if _, err := Read(home); !os.IsNotExist(err) {
		t.Fatalf("owner lock after mid-turn detach = %v, want removed", err)
	}
}
