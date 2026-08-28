package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// App.ReadFileContent (the Wails-bound surface) is a passthrough to
// agent.ReadFileContent. The Wails adapter must propagate the agent's
// boundary refusal.
func TestPR11Closure_AppReadFileContentPropagatesViewerBoundaryRefusal(t *testing.T) {
	svc := newAppTestAgent(t)
	app := newTestApp(svc)

	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	app.seedPresented(svc.SessionCurrent().ID)
	result, err := app.ReadFileContent(svc.SessionCurrent().ID, outsideSecret)
	if err == nil {
		t.Fatalf("App.ReadFileContent(%q) succeeded with content %q; want boundary refusal propagated from agent", outsideSecret, result.Content)
	}
	if strings.Contains(result.Content, "outside-secret") {
		t.Fatalf("App.ReadFileContent(%q) leaked outside content despite returning error", outsideSecret)
	}
}

func TestWailsStaleCurrent(t *testing.T) {
	svc := newAppTestAgent(t)
	if _, err := svc.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := svc.AppendUserMessage("gone"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	id := svc.SessionCurrent().ID
	if id == "" {
		t.Fatal("missing session id")
	}

	app := newTestApp(svc)
	app.setCurrentSessionID(id)
	if err := svc.SessionDelete(id); err != nil {
		t.Fatalf("SessionDelete: %v", err)
	}

	if got := app.SessionCurrent().ID; got != "" {
		t.Fatalf("stale current id = %q, want empty", got)
	}
	app.setCurrentSessionID(id)
	if got := app.SessionMessages(); len(got) != 0 {
		t.Fatalf("stale messages = %#v, want empty", got)
	}
	app.setCurrentSessionID(id)
	if _, err := app.Submit("hello"); err == nil {
		t.Fatal("stale submit succeeded")
	}
}

// TestWailsCurrentModelPropagatesOwnerAndRoutingErrors proves App.CurrentModel
// returns the real owner and routing errors instead of suppressing them into a
// zero model: a deleted/missing owner session and a closed routing (navMu won
// by close) must surface as errors so the settings refresh can gate on them.
func TestWailsCurrentModelPropagatesOwnerAndRoutingErrors(t *testing.T) {
	// Missing/deleted owner session: routing current points at a session the
	// owner no longer serves, so the owner error must propagate.
	svc := newAppTestAgent(t)
	if _, err := svc.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := svc.SessionCurrent().ID
	if id == "" {
		t.Fatal("missing session id")
	}
	app := newTestApp(svc)
	app.setCurrentSessionID(id)
	if err := svc.SessionDelete(id); err != nil {
		t.Fatalf("SessionDelete: %v", err)
	}
	if _, err := app.CurrentModel(""); err == nil {
		t.Fatal("CurrentModel for a deleted owner session returned no error; want the owner error propagated")
	}

	// Closed routing: close won navMu, so boundedSessionIDLocked rejects.
	app2 := newTestApp(svc)
	app2.navClosed = true
	if _, err := app2.CurrentModel(""); !errors.Is(err, errAdapterClosed) {
		t.Fatalf("CurrentModel with closed routing = %v, want errAdapterClosed", err)
	}
}

// currentModelSpy is a deterministic AdapterService spy recording every
// CurrentModelForSession call, so a superseded CurrentModel can be proven to
// skip the owner read and a matched read can be counted exactly.
type currentModelSpy struct {
	agent.AdapterService
	byID   map[string]agent.ModelInfo
	errs   map[string]error
	called []string
}

func (s *currentModelSpy) CurrentModelForSession(id string) (agent.ModelInfo, error) {
	s.called = append(s.called, id)
	if err := s.errs[id]; err != nil {
		return agent.ModelInfo{}, err
	}
	return s.byID[id], nil
}

// TestWailsCurrentModelSupersededRouteMismatch proves App.CurrentModel marks a
// nonempty expected session that no longer matches routing as superseded without
// performing an owner model read. It drives the real no-drainer production
// seams: presentation is seeded to A, routing commits to B, and B's navigation
// boundary is enqueued without a drainer, so it stays queued and presentation
// stays A. A settings refresh captured on A must not apply B's model to A, while
// a matched expectation reads B's model exactly once. Matched owner errors and
// empty expectations keep their mount-time siblings.
func TestWailsCurrentModelSupersededRouteMismatch(t *testing.T) {
	t.Run("no_drainer_route_presentation", func(t *testing.T) {
		spy := &currentModelSpy{
			byID: map[string]agent.ModelInfo{
				"A": {Ref: "prov/a", Provider: "prov", Model: "a", DisplayName: "Model A"},
				"B": {Ref: "prov/b", Provider: "prov", Model: "b", DisplayName: "Model B"},
			},
		}
		app := &App{svc: spy}

		// Seed backend presentation as A.
		app.seedPresented("A")
		// Commit routing to B.
		app.setCurrentSessionID("B")
		// Enqueue B's navigation/frameAdvance boundary without starting a drainer.
		app.enqueueBoundary("navigation", map[string]any{"session": map[string]any{"id": "B"}}, "", "B")

		// The boundary remains queued and presentation remains A: no drainer ran.
		app.deliveryMu.Lock()
		queuedCount := len(app.deliveryFrames)
		var queuedKind frameKind
		var queuedSession string
		presented := app.presented
		if queuedCount == 1 {
			queuedKind = app.deliveryFrames[0].kind
			queuedSession = app.deliveryFrames[0].sessionID
		}
		app.deliveryMu.Unlock()
		if queuedCount != 1 {
			t.Fatalf("queued boundary count = %d, want 1 (no drainer started)", queuedCount)
		}
		if queuedKind != frameAdvance || queuedSession != "B" {
			t.Fatalf("queued boundary kind/session = %v/%q, want frameAdvance/B", queuedKind, queuedSession)
		}
		if presented != "A" {
			t.Fatalf("presentation advanced to %q while the boundary stayed queued, want A", presented)
		}

		// Expected A while routing is B: superseded, with no owner model read.
		res, err := app.CurrentModel("A")
		if err != nil {
			t.Fatalf("CurrentModel(expected A) while routing B = %v, want nil superseded", err)
		}
		if !res.Superseded {
			t.Fatal("CurrentModel(expected A) while routing B must report Superseded")
		}
		if len(spy.called) != 0 {
			t.Fatalf("superseded CurrentModel must not call CurrentModelForSession; got %v", spy.called)
		}

		// Matched expected B reads B's model exactly once.
		matched, err := app.CurrentModel("B")
		if err != nil {
			t.Fatalf("CurrentModel(expected B) = %v, want B's model", err)
		}
		if matched.Superseded {
			t.Fatal("matched expected B must not be superseded")
		}
		if matched.Model.Ref != "prov/b" {
			t.Fatalf("matched expected B model = %q, want prov/b", matched.Model.Ref)
		}
		if len(spy.called) != 1 || spy.called[0] != "B" {
			t.Fatalf("matched expected B owner reads = %v, want exactly ['B']", spy.called)
		}
	})

	t.Run("matched_owner_error", func(t *testing.T) {
		spy := &currentModelSpy{errs: map[string]error{"B": errors.New("owner unavailable")}}
		app := &App{svc: spy}
		app.setCurrentSessionID("B")
		if _, err := app.CurrentModel("B"); err == nil {
			t.Fatal("matched owner error must propagate")
		}
		if len(spy.called) != 1 || spy.called[0] != "B" {
			t.Fatalf("matched owner-error owner reads = %v, want exactly ['B']", spy.called)
		}
	})

	t.Run("empty_expectation", func(t *testing.T) {
		spy := &currentModelSpy{
			byID: map[string]agent.ModelInfo{
				"B": {Ref: "prov/b", Provider: "prov", Model: "b", DisplayName: "Model B"},
			},
		}
		app := &App{svc: spy}
		app.setCurrentSessionID("B")
		res, err := app.CurrentModel("")
		if err != nil {
			t.Fatalf("CurrentModel(\"\") = %v, want B's model", err)
		}
		if res.Superseded {
			t.Fatal("empty expectation must not be superseded; it preserves mount-time routing")
		}
		if res.Model.Ref != "prov/b" {
			t.Fatalf("empty expectation model = %q, want prov/b", res.Model.Ref)
		}
		if len(spy.called) != 1 || spy.called[0] != "B" {
			t.Fatalf("empty expectation owner reads = %v, want exactly ['B']", spy.called)
		}
	})
}

type readFileContentSpy struct {
	agent.AdapterService
	roots        map[string]string
	content      string
	readErr      error
	readStarted  chan struct{}
	readRelease  chan struct{}
	readStartOne sync.Once
	mu           sync.Mutex
	projectCalls []string
	readCalls    []string
}

func (s *readFileContentSpy) ProjectPathForSession(sessionID string) (string, error) {
	s.mu.Lock()
	s.projectCalls = append(s.projectCalls, sessionID)
	s.mu.Unlock()
	return s.roots[sessionID], nil
}

func (s *readFileContentSpy) ReadFileContentForProjectPath(root, path string) (string, error) {
	s.mu.Lock()
	s.readCalls = append(s.readCalls, root+":"+path)
	s.mu.Unlock()
	if s.readStarted != nil {
		s.readStartOne.Do(func() { close(s.readStarted) })
	}
	if s.readRelease != nil {
		<-s.readRelease
	}
	return s.content, s.readErr
}

func (s *readFileContentSpy) calls() (projects, reads []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.projectCalls...), append([]string(nil), s.readCalls...)
}

func TestWailsReadFileContentPresentationOwnership(t *testing.T) {
	newSpy := func() *readFileContentSpy {
		return &readFileContentSpy{
			AdapterService: nil,
			roots:          map[string]string{"A": "/project-a", "B": "/project-b"},
			content:        "file contents",
		}
	}

	t.Run("same_presentation_success_and_error", func(t *testing.T) {
		spy := newSpy()
		app := &App{svc: spy}
		app.seedPresented("A")

		got, err := app.ReadFileContent("A", "main.go")
		if err != nil || got.Superseded || got.Content != "file contents" {
			t.Fatalf("same-presentation read = %#v, %v; want content", got, err)
		}
		spy.readErr = errors.New("read failed")
		got, err = app.ReadFileContent("A", "main.go")
		if err == nil || got.Superseded {
			t.Fatalf("same-presentation error = %#v, %v; want ordinary read error", got, err)
		}
		projects, reads := spy.calls()
		if !reflect.DeepEqual(projects, []string{"A", "A"}) || !reflect.DeepEqual(reads, []string{"/project-a:main.go", "/project-a:main.go"}) {
			t.Fatalf("ownership calls = projects %v reads %v; want A and project-a", projects, reads)
		}
	})

	t.Run("forged_expected_and_detach_drop_before_io", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			expected  string
			presented string
		}{
			{name: "forged", expected: "B", presented: "A"},
			{name: "detach", expected: "A", presented: ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				spy := newSpy()
				app := &App{svc: spy}
				app.seedPresented(tc.presented)
				got, err := app.ReadFileContent(tc.expected, "main.go")
				if err != nil || !got.Superseded {
					t.Fatalf("pre-read mismatch = %#v, %v; want superseded", got, err)
				}
				projects, reads := spy.calls()
				if len(projects) != 0 || len(reads) != 0 {
					t.Fatalf("superseded read performed I/O: projects %v reads %v", projects, reads)
				}
			})
		}
	})

	t.Run("routing_B_while_presenting_A_reads_A", func(t *testing.T) {
		spy := newSpy()
		app := &App{svc: spy}
		app.seedPresented("A")
		app.setCurrentSessionID("B")

		got, err := app.ReadFileContent("A", "main.go")
		if err != nil || got.Superseded {
			t.Fatalf("routing-B read = %#v, %v; want A content", got, err)
		}
		projects, reads := spy.calls()
		if !reflect.DeepEqual(projects, []string{"A"}) || !reflect.DeepEqual(reads, []string{"/project-a:main.go"}) {
			t.Fatalf("routing-B ownership calls = projects %v reads %v; want A/project-a", projects, reads)
		}
	})

	for _, tc := range []struct {
		name       string
		readErr    error
		change     func(*App)
		wantClosed bool
	}{
		{name: "A_to_B_success", change: func(app *App) { app.seedPresented("B") }},
		{name: "A_to_B_error", readErr: errors.New("read failed"), change: func(app *App) { app.seedPresented("B") }},
		{name: "detach_error", readErr: errors.New("read failed"), change: func(app *App) { app.seedPresented("") }},
		{name: "close_error", readErr: errors.New("read failed"), change: func(app *App) { app.closeDelivery() }, wantClosed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newSpy()
			spy.readErr = tc.readErr
			spy.readStarted = make(chan struct{})
			spy.readRelease = make(chan struct{})
			app := &App{svc: spy}
			app.seedPresented("A")
			resultCh := make(chan struct {
				result ReadFileContentResult
				err    error
			}, 1)
			go func() {
				result, err := app.ReadFileContent("A", "main.go")
				resultCh <- struct {
					result ReadFileContentResult
					err    error
				}{result, err}
			}()
			select {
			case <-spy.readStarted:
			case <-time.After(time.Second):
				t.Fatal("read did not reach the viewer boundary")
			}
			tc.change(app)
			close(spy.readRelease)
			out := <-resultCh
			if out.err != nil || !out.result.Superseded || out.result.Content != "" {
				t.Fatalf("stale read = %#v, %v; want superseded without content", out.result, out.err)
			}
			if tc.wantClosed {
				app.closeDelivery()
			}
		})
	}

	t.Run("stalled_read_does_not_block_delivery_or_close", func(t *testing.T) {
		spy := newSpy()
		spy.readStarted = make(chan struct{})
		spy.readRelease = make(chan struct{})
		app := &App{svc: spy}
		app.seedPresented("A")
		resultCh := make(chan ReadFileContentResult, 1)
		go func() {
			result, _ := app.ReadFileContent("A", "main.go")
			resultCh <- result
		}()
		select {
		case <-spy.readStarted:
		case <-time.After(time.Second):
			t.Fatal("read did not reach the viewer boundary")
		}

		enqueued := make(chan struct{})
		go func() {
			app.enqueueFrame(deliveryFrame{name: "probe"})
			close(enqueued)
		}()
		select {
		case <-enqueued:
		case <-time.After(time.Second):
			t.Fatal("delivery enqueue blocked behind viewer I/O")
		}
		closed := make(chan struct{})
		go func() {
			app.closeDelivery()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("delivery close blocked behind viewer I/O")
		}
		close(spy.readRelease)
		if result := <-resultCh; !result.Superseded {
			t.Fatalf("read after close = %#v; want superseded", result)
		}
	})
}

func TestWailsNewSetsCurrent(t *testing.T) {
	svc := newAppTestAgent(t)
	app := newTestApp(svc)
	if err := app.SessionNew(); err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	if got := app.SessionCurrent().ID; got == "" {
		t.Fatal("new session did not set current")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app.ctx = ctx
	if _, err := app.Submit("hello"); err != nil && strings.Contains(err.Error(), "no current session") {
		t.Fatalf("submit after new = %v", err)
	}
}

func TestWailsClearRemovedCurrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*App, string) error
	}{
		{name: "archive", run: func(app *App, id string) error { return app.SessionArchive(id) }},
		{name: "delete", run: func(app *App, id string) error { return app.SessionDelete(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newAppTestAgent(t)
			firstID, err := svc.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession first: %v", err)
			}
			if _, err := svc.AppendUserMessageToSession(firstID, "first"); err != nil {
				t.Fatalf("append first: %v", err)
			}
			secondID, err := svc.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession second: %v", err)
			}
			if _, err := svc.AppendUserMessageToSession(secondID, "second"); err != nil {
				t.Fatalf("append second: %v", err)
			}

			app := newTestApp(svc)
			app.setCurrentSessionID(firstID)
			if err := tc.run(app, firstID); err != nil {
				t.Fatalf("%s first: %v", tc.name, err)
			}
			if got := app.SessionCurrent().ID; got != "" {
				t.Fatalf("current after %s = %q, want empty", tc.name, got)
			}
			if _, err := app.SnapshotList(); err == nil {
				t.Fatalf("%s snapshot list succeeded with no current session", tc.name)
			}
			if err := app.CompactNow(); err == nil {
				t.Fatalf("%s compact succeeded with no current session", tc.name)
			}
			app.deliveryMu.Lock()
			delivered := app.presentAcceptsLocked(deliveryFrame{name: "token", sessionID: firstID})
			app.deliveryMu.Unlock()
			if delivered {
				t.Fatalf("%s left old session event visible", tc.name)
			}
			if current := svc.SessionCurrent().ID; current != secondID {
				t.Fatalf("backend current after %s = %q, want %q", tc.name, current, secondID)
			}
		})
	}
}

func TestWailsForkCurrent(t *testing.T) {
	svc := newAppTestAgent(t)
	firstID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	turn, err := svc.AppendUserMessageToSession(firstID, "first")
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	secondID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := svc.AppendUserMessageToSession(secondID, "second"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	app := newTestApp(svc)
	app.setCurrentSessionID(firstID)
	if err := app.ForkSession(turn); err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	forkID := app.SessionCurrent().ID
	if forkID == "" || forkID == firstID || forkID == secondID {
		t.Fatalf("fork current = %q, want new id distinct from %q/%q", forkID, firstID, secondID)
	}
	if got := svc.SessionCurrent().ID; got != secondID {
		t.Fatalf("backend current = %q, want %q", got, secondID)
	}
}

// TestWailsRevertHistoryTruncatesLikeApplyTurnAction proves the bound
// App.RevertHistory route and the adapter-neutral App.ApplyTurnAction route are
// the same revert implementation: given the same turn argument, both must leave
// the same durable history. The bound route previously truncated one turn
// higher than the turn-action route.
func TestWailsRevertHistoryTruncatesLikeApplyTurnAction(t *testing.T) {
	svc := newAppTestAgent(t)
	app := newTestApp(svc)

	seed := func() (string, int) {
		id, err := svc.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		var last int
		for _, content := range []string{"one", "two", "three", "four"} {
			last, err = svc.AppendUserMessageToSession(id, content)
			if err != nil {
				t.Fatalf("append %q: %v", content, err)
			}
		}
		return id, last
	}

	boundID, boundTurn := seed()
	actionID, actionTurn := seed()

	app.setCurrentSessionID(boundID)
	if err := app.RevertHistory(boundTurn); err != nil {
		t.Fatalf("App.RevertHistory(%d): %v", boundTurn, err)
	}
	app.setCurrentSessionID(actionID)
	if _, err := app.ApplyTurnAction(actionTurn, agent.TurnActionRevertHistory, false); err != nil {
		t.Fatalf("App.ApplyTurnAction(%d, revert_history): %v", actionTurn, err)
	}

	bound, err := svc.SessionMessagesFor(boundID)
	if err != nil {
		t.Fatalf("SessionMessagesFor(bound): %v", err)
	}
	action, err := svc.SessionMessagesFor(actionID)
	if err != nil {
		t.Fatalf("SessionMessagesFor(action): %v", err)
	}
	got, want := userContents(bound), userContents(action)
	if !equalStrings(got, want) {
		t.Fatalf("App.RevertHistory(%d) left %q, App.ApplyTurnAction(%d) left %q; both must truncate to the same turn", boundTurn, got, actionTurn, want)
	}
	// The surviving semantic is turn-1: the given turn is the first one removed.
	if expected := []string{"one", "two", "three"}; !equalStrings(want, expected) {
		t.Fatalf("both routes left %q, want %q (given turn %d is the first removed)", want, expected, actionTurn)
	}
}

func TestWailsSwitchKeepsCurrent(t *testing.T) {
	svc := newAppTestAgent(t)
	firstID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	if _, err := svc.AppendUserMessageToSession(firstID, "first"); err != nil {
		t.Fatalf("append first: %v", err)
	}
	secondID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := svc.AppendUserMessageToSession(secondID, "second"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	app := newTestApp(svc)
	app.setCurrentSessionID(secondID)
	if err := app.SessionSwitch(firstID); err != nil {
		t.Fatalf("SessionSwitch: %v", err)
	}
	if got := app.SessionCurrent().ID; got != firstID {
		t.Fatalf("app current = %q, want %q", got, firstID)
	}
	if got := svc.SessionCurrent().ID; got != secondID {
		t.Fatalf("backend current = %q, want %q", got, secondID)
	}
	if got := userContents(app.SessionMessages()); !equalStrings(got, []string{"first"}) {
		t.Fatalf("app messages = %#v, want first", got)
	}
}

// TestWailsSubagentFilter drives the drain-side subagent gating: a subagent frame
// reaches the frontend only for a child registered under the presentation-current
// root, and a boundary that advances presentation current clears the child set.
func TestWailsSubagentFilter(t *testing.T) {
	app := &App{}
	accept := func(f deliveryFrame) bool {
		app.deliveryMu.Lock()
		defer app.deliveryMu.Unlock()
		return app.presentAcceptsLocked(f)
	}
	sub := func(parent, child string) deliveryFrame {
		return deliveryFrame{name: "subagent_token", kind: frameSubagent, parent: parent, child: child}
	}
	advance := func(id string) { accept(deliveryFrame{name: "navigation", kind: frameAdvance, sessionID: id}) }

	// No presentation current rejects any subagent event.
	if accept(sub("root", "child")) {
		t.Fatal("empty presented accepted child event")
	}

	advance("root")
	// Wrong parent with an unregistered child rejects.
	if accept(sub("other", "child")) {
		t.Fatal("wrong parent accepted child event")
	}
	// A direct child of the presented root registers and is accepted.
	if !accept(sub("root", "child")) {
		t.Fatal("matching parent child start rejected")
	}
	// A later event from the registered child is accepted via the set.
	if !accept(sub("", "child")) {
		t.Fatal("subscribed child event rejected")
	}

	// Advancing presentation current to a new root clears the child set, so the old
	// child is rejected.
	advance("next")
	if accept(sub("", "child")) {
		t.Fatal("stale child accepted after boundary advanced presentation current")
	}
}

func newAppTestAgent(t *testing.T) *agent.Agent {
	return newAppTestAgentAt(t, "http://127.0.0.1:9/v1")
}

func newAppTestAgentAt(t *testing.T, baseURL string) *agent.Agent {
	t.Helper()
	return newAppTestAgentAtHome(t, baseURL, t.TempDir(), t.TempDir())
}

// newAppTestAgentAtHome builds an owner over the given home and project root,
// so several owners can share one home for cross-process claim testing.
func newAppTestAgentAtHome(t *testing.T, baseURL, home, projectRoot string) *agent.Agent {
	t.Helper()

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
  }
}`, baseURL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lightcodeDir, "agents.json"), []byte(`{"primary": {"model": "test/test-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newTestApp(svc *agent.Agent) *App {
	return &App{svc: svc, routeProjectPath: svc.ProjectRoot()}
}

func startAppTestAgent(t *testing.T, a *agent.Agent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a.Init(ctx)
	t.Cleanup(func() {
		cancel()
		if !a.ShutdownOwner() {
			t.Error("Wails test agent shutdown reported abandoned work")
		}
	})
}

// newAppTestAgentPair builds two owners over the same home with distinct
// project roots, so one owner's live sessions hold their claims against the
// other.
func newAppTestAgentPair(t *testing.T) (*agent.Agent, *agent.Agent) {
	t.Helper()
	home := t.TempDir()
	first := newAppTestAgentAtHome(t, "http://127.0.0.1:9/v1", home, t.TempDir())
	second := newAppTestAgentAtHome(t, "http://127.0.0.1:9/v1", home, t.TempDir())
	t.Cleanup(func() {
		runtime.KeepAlive(first)
		runtime.KeepAlive(second)
	})
	return first, second
}

// stampSessionActivity rewrites a session's persisted last activity so the
// active-session listing order is deterministic instead of same-second ties.
func stampSessionActivity(t *testing.T, svc *agent.Agent, projectPath, id string, lastActivity int64) {
	t.Helper()
	proj, err := svc.ProjectCurrentForPath(projectPath)
	if err != nil {
		t.Fatalf("project for %s: %v", projectPath, err)
	}
	metaPath := filepath.Join(svc.Projects().SessionsRoot(proj.ID), id, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var meta snapshot.SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	meta.LastActivity = lastActivity
	out, err := json.Marshal(&meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(metaPath, out, 0o600); err != nil {
		t.Fatalf("rewrite meta: %v", err)
	}
}

// listingFailSvc embeds a real owner but fails the active-session listing, so
// a caller that reads the listing result must surface the error instead of
// silently creating a session.
type listingFailSvc struct {
	agent.AdapterService
	listErr error
}

func (f *listingFailSvc) SessionListForProjectPath(projectPath, state string) ([]agent.SessionSummary, error) {
	return nil, f.listErr
}

func userContents(messages []agent.DisplayMessage) []string {
	var out []string
	for _, msg := range messages {
		if msg.Type == "user" {
			out = append(out, msg.Content)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
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

// TestAdapterExplicitSessionTargetingContract proves a cross-project session
// switch commits the destination project on the Wails host, so every later
// project-scoped route resolves to it rather than the previous project.
func TestAdapterExplicitSessionTargetingContract(t *testing.T) {
	t.Run("project_current=cross_project_session_switch_reports_B", func(t *testing.T) {
		svc := newAppTestAgent(t)
		startupID, err := svc.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession startup: %v", err)
		}
		otherRoot := t.TempDir()
		otherID, err := svc.NewSessionForProjectPath(otherRoot, "primary")
		if err != nil {
			t.Fatalf("NewSessionForProjectPath: %v", err)
		}
		wantOther, err := svc.ProjectCurrentForPath(otherRoot)
		if err != nil {
			t.Fatalf("other project: %v", err)
		}
		if wantOther.ID == "" {
			t.Fatal("test setup: destination project record missing")
		}
		app := newTestApp(svc)
		app.setCurrentSessionID(startupID)
		if err := app.SessionSwitch(otherID); err != nil {
			t.Fatalf("SessionSwitch: %v", err)
		}
		gotProject, err := app.ProjectCurrent()
		if err != nil {
			t.Fatalf("ProjectCurrent: %v", err)
		}
		if got := gotProject.ID; got != wantOther.ID {
			t.Fatalf("ProjectCurrent after cross-project switch = %q, want %q", got, wantOther.ID)
		}
	})

	t.Run("project_list=cross_project_switch_keeps_list", func(t *testing.T) {
		svc := newAppTestAgent(t)
		startupID, err := svc.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession startup: %v", err)
		}
		otherRoot := t.TempDir()
		otherID, err := svc.NewSessionForProjectPath(otherRoot, "primary")
		if err != nil {
			t.Fatalf("NewSessionForProjectPath: %v", err)
		}
		app := newTestApp(svc)
		app.setCurrentSessionID(startupID)
		if err := app.SessionSwitch(otherID); err != nil {
			t.Fatalf("SessionSwitch: %v", err)
		}
		sessions, err := app.SessionList("active")
		if err != nil {
			t.Fatalf("SessionList: %v", err)
		}
		if len(sessions) != 1 || sessions[0].ID != otherID {
			t.Fatalf("SessionList after cross-project switch = %#v, want only %q", sessions, otherID)
		}
	})
}

// TestWailsSessionSwitchAppliesCrossProjectTitle proves a session switch into
// another project applies the destination project's native window title through
// the consumed navigation boundary, exactly as a project switch does: the
// boundary carries "Lightcode — <basename>" computed from the destination
// session's project path, so the window does not stay on the previous project.
func TestWailsSessionSwitchAppliesCrossProjectTitle(t *testing.T) {
	svc := newAppTestAgent(t)
	startupID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}
	otherRoot := t.TempDir()
	otherID, err := svc.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath: %v", err)
	}

	app := newTestApp(svc)
	app.agent = svc
	var mu sync.Mutex
	var titles []string
	app.emitFn = func(string, any) {}
	app.titleFn = func(title string) {
		mu.Lock()
		titles = append(titles, title)
		mu.Unlock()
	}
	app.startDelivery()
	defer app.closeDelivery()
	app.setCurrentSessionID(startupID)

	if err := app.SessionSwitch(otherID); err != nil {
		t.Fatalf("SessionSwitch: %v", err)
	}

	want := "Lightcode — " + filepath.Base(otherRoot)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := append([]string(nil), titles...)
		mu.Unlock()
		if len(got) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no window title applied after a cross-project SessionSwitch; the navigation boundary must carry the destination project's title")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(titles) != 1 || titles[0] != want {
		t.Fatalf("titles after SessionSwitch = %v, want [%s]", titles, want)
	}
}

// TestSessionSwitchUnreadableMetaStillRoutesDestination proves a session switch
// routes to the destination project even when the destination's metadata is
// unreadable: the owner resolves the unit against its project and carries that
// project in the boundary summary, so the adapter commits the route instead of
// losing it.
func TestSessionSwitchUnreadableMetaStillRoutesDestination(t *testing.T) {
	svc := newAppTestAgent(t)
	startupID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}
	otherRoot := t.TempDir()
	otherID, err := svc.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath: %v", err)
	}
	proj, err := svc.ProjectCurrentForPath(otherRoot)
	if err != nil {
		t.Fatalf("destination project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(svc.Projects().SessionsRoot(proj.ID), otherID, "meta.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt destination meta: %v", err)
	}

	app := newTestApp(svc)
	app.setCurrentSessionID(startupID)
	if err := app.SessionSwitch(otherID); err != nil {
		t.Fatalf("SessionSwitch: %v", err)
	}
	if got := app.routeProjectPath; got != otherRoot {
		t.Fatalf("route after switch with unreadable meta = %q, want destination %q", got, otherRoot)
	}
	gotProject, err := app.ProjectCurrent()
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	if got := gotProject.ID; got != proj.ID {
		t.Fatalf("ProjectCurrent after switch = %q, want destination project %q", got, proj.ID)
	}
}

// TestSessionSwitchArchivedSessionRoutesDestination proves the reactivation
// boundary carries the destination project too: switching to an archived session
// in another project whose metadata records no project path still routes to that
// project, because the boundary builds from the same authoritative project as the
// live-selection path.
func TestSessionSwitchArchivedSessionRoutesDestination(t *testing.T) {
	svc := newAppTestAgent(t)
	startupID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}
	otherRoot := t.TempDir()
	otherID, err := svc.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath: %v", err)
	}
	// The destination must be a persisted archived session, not a live unit, so
	// the switch takes the reactivation branch; a complete turn keeps it from
	// being discarded as an empty session when it closes.
	if _, err := svc.AppendUserMessageToSession(otherID, "seed"); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	if err := svc.SessionArchive(otherID); err != nil {
		t.Fatalf("SessionArchive: %v", err)
	}
	proj, err := svc.ProjectCurrentForPath(otherRoot)
	if err != nil {
		t.Fatalf("destination project: %v", err)
	}
	metaPath := filepath.Join(svc.Projects().SessionsRoot(proj.ID), otherID, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read destination meta: %v", err)
	}
	var meta snapshot.SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal destination meta: %v", err)
	}
	meta.ProjectPath = ""
	out, err := json.Marshal(&meta)
	if err != nil {
		t.Fatalf("marshal destination meta: %v", err)
	}
	if err := os.WriteFile(metaPath, out, 0o600); err != nil {
		t.Fatalf("rewrite destination meta: %v", err)
	}

	app := newTestApp(svc)
	app.setCurrentSessionID(startupID)
	if err := app.SessionSwitch(otherID); err != nil {
		t.Fatalf("SessionSwitch: %v", err)
	}
	if got := app.routeProjectPath; got != otherRoot {
		t.Fatalf("route after switch to archived session with empty meta project = %q, want destination %q", got, otherRoot)
	}
	gotProject, err := app.ProjectCurrent()
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	if got := gotProject.ID; got != proj.ID {
		t.Fatalf("ProjectCurrent after switch = %q, want destination project %q", got, proj.ID)
	}
}

// TestSessionSwitchArchivedStaleMetaRoutesActualProject proves the reactivation
// boundary routes to the project the session actually lives in, not the project
// its persisted metadata names: the unit is resolved from the session's real
// directory, and that project is authoritative for the boundary even when the
// metadata's path is nonempty and stale.
func TestSessionSwitchArchivedStaleMetaRoutesActualProject(t *testing.T) {
	svc := newAppTestAgent(t)
	startupID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession startup: %v", err)
	}
	otherRoot := t.TempDir()
	otherID, err := svc.NewSessionForProjectPath(otherRoot, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath: %v", err)
	}
	// The destination must be a persisted archived session, not a live unit, so
	// the switch takes the reactivation branch; a complete turn keeps it from
	// being discarded as an empty session when it closes.
	if _, err := svc.AppendUserMessageToSession(otherID, "seed"); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	if err := svc.SessionArchive(otherID); err != nil {
		t.Fatalf("SessionArchive: %v", err)
	}
	proj, err := svc.ProjectCurrentForPath(otherRoot)
	if err != nil {
		t.Fatalf("destination project: %v", err)
	}
	// The persisted metadata names a different project than the one the session
	// physically lives under.
	staleRoot := t.TempDir()
	metaPath := filepath.Join(svc.Projects().SessionsRoot(proj.ID), otherID, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read destination meta: %v", err)
	}
	var meta snapshot.SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal destination meta: %v", err)
	}
	meta.ProjectPath = staleRoot
	out, err := json.Marshal(&meta)
	if err != nil {
		t.Fatalf("marshal destination meta: %v", err)
	}
	if err := os.WriteFile(metaPath, out, 0o600); err != nil {
		t.Fatalf("rewrite destination meta: %v", err)
	}

	app := newTestApp(svc)
	app.setCurrentSessionID(startupID)
	if err := app.SessionSwitch(otherID); err != nil {
		t.Fatalf("SessionSwitch: %v", err)
	}
	if got := app.routeProjectPath; got != otherRoot {
		t.Fatalf("route after switch to archived session with stale meta project = %q, want the project it lives in %q", got, otherRoot)
	}
	gotProject, err := app.ProjectCurrent()
	if err != nil {
		t.Fatalf("ProjectCurrent: %v", err)
	}
	if got := gotProject.ID; got != proj.ID {
		t.Fatalf("ProjectCurrent after switch = %q, want the project it lives in %q", got, proj.ID)
	}
}

// TestWailsExplicitOpenOfContendedSessionIsReadOnly proves an explicit switch
// to a session another process drives opens it read-only: routing current
// commits the session with its own identity, the durable transcript is
// presented, a turn refuses with the contention message instead of "unknown
// session", compaction keeps the selection and names the contention instead of
// answering "no current session", and a later switch to a live session clears
// the marker and admits a turn.
func TestWailsExplicitOpenOfContendedSessionIsReadOnly(t *testing.T) {
	first, second := newAppTestAgentPair(t)
	startAppTestAgent(t, second)
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

	app := newTestApp(second)
	app.agent = second
	app.ctx = context.Background()
	app.setCurrentSessionID(startupID)
	if err := app.SessionSwitch(heldID); err != nil {
		t.Fatalf("SessionSwitch over a held session = %v, want a read-only open", err)
	}
	if got := app.currentSessionID(); got != heldID {
		t.Fatalf("routing current = %q, want the held session %q", got, heldID)
	}
	if got := app.SessionCurrent().ID; got != heldID {
		t.Fatalf("SessionCurrent = %q, want the held session %q", got, heldID)
	}
	if got := userContents(app.SessionMessages()); !equalStrings(got, []string{"durable from the driving owner"}) {
		t.Fatalf("read-only transcript = %#v, want the durable messages", got)
	}

	// Not live here: a turn refuses with the contention message.
	if _, err := app.Submit("hi"); err == nil || !strings.Contains(err.Error(), "driven by another process") {
		t.Fatalf("submit over the read-only session = %v, want the contention message", err)
	}
	// Compaction keeps the selection and names the contention.
	if err := app.CompactNow(); err == nil || !strings.Contains(err.Error(), "driven by another process") {
		t.Fatalf("CompactNow over the read-only session = %v, want the contention message", err)
	}
	if got := app.currentSessionID(); got != heldID {
		t.Fatalf("routing current after failed compact = %q, want the read-only session kept %q", got, heldID)
	}

	// Switching to a live session clears the marker and admits a turn.
	liveID, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession live: %v", err)
	}
	if err := app.SessionSwitch(liveID); err != nil {
		t.Fatalf("SessionSwitch to a live session: %v", err)
	}
	if got := app.currentSessionID(); got != liveID {
		t.Fatalf("routing current after live switch = %q, want %q", got, liveID)
	}
	app.ctx = context.Background()
	if _, err := app.Submit("hi"); err != nil {
		t.Fatalf("submit after switching to a live session = %v, want admission", err)
	}
	runtime.KeepAlive(first)
}

// TestWailsReopenAfterHolderReleasesIsLive proves a session opened read-only
// becomes live again when it is reopened after the other process releases it:
// the read-only marker does not survive a successful live commit of the same
// id, so a turn is admitted and no contention is reported.
func TestWailsReopenAfterHolderReleasesIsLive(t *testing.T) {
	first, second := newAppTestAgentPair(t)
	startAppTestAgent(t, second)
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

	app := newTestApp(second)
	app.agent = second
	app.setCurrentSessionID(startupID)
	if err := app.SessionSwitch(heldID); err != nil {
		t.Fatalf("SessionSwitch over a held session = %v, want a read-only open", err)
	}
	if _, err := app.Submit("hi"); err == nil || !strings.Contains(err.Error(), "driven by another process") {
		t.Fatalf("submit over the read-only session = %v, want the contention message", err)
	}

	// The driving process releases the session; reopening it must succeed live.
	if err := first.SessionArchive(heldID); err != nil {
		t.Fatalf("SessionArchive (release): %v", err)
	}
	if err := app.SessionSwitch(heldID); err != nil {
		t.Fatalf("SessionSwitch after the holder released = %v, want a live open", err)
	}
	if got := app.liveCurrentSessionID(); got != heldID {
		t.Fatalf("live current after reopen = %q, want %q", got, heldID)
	}
	if app.routeReadOnlyNames(heldID) {
		t.Fatal("read-only marker survived the live reopen of the same session")
	}
	app.ctx = context.Background()
	if _, err := app.Submit("hi"); err != nil {
		t.Fatalf("submit after reopen = %v, want admission without contention", err)
	}
}

// TestWailsReadOnlyOpenHydrationFailureLeavesRoutingUnchanged proves a
// read-only open whose durable view cannot be read commits nothing: the
// previous session and the routing project stay, so a failed presentation
// never advances routing.
func TestWailsReadOnlyOpenHydrationFailureLeavesRoutingUnchanged(t *testing.T) {
	first, second := newAppTestAgentPair(t)
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

	app := newTestApp(second)
	app.agent = second
	app.setCurrentSessionID(startupID)
	if err := app.SessionSwitch(heldID); err == nil || !strings.Contains(err.Error(), "compaction.json") {
		t.Fatalf("SessionSwitch over a held session with unreadable history = %v, want the hydration failure", err)
	}
	if got := app.currentSessionID(); got != startupID {
		t.Fatalf("routing current after failed read-only open = %q, want unchanged %q", got, startupID)
	}
	if got := app.routeProjectPath; got != second.ProjectRoot() {
		t.Fatalf("routing project after failed read-only open = %q, want unchanged %q", got, second.ProjectRoot())
	}
	if app.routeReadOnlyNames(heldID) {
		t.Fatal("read-only marker set for an open whose presentation failed")
	}
	runtime.KeepAlive(first)
}

func TestProjectSwitchInPlaceKeepsOwnerAlive(t *testing.T) {
	svc := newAppTestAgent(t)
	app := newTestApp(svc)

	firstID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	if _, err := svc.AppendUserMessageToSession(firstID, "first"); err != nil {
		t.Fatalf("append first: %v", err)
	}
	app.setCurrentSessionID(firstID)

	targetDir := t.TempDir()
	if err := app.ProjectSwitch(targetDir); err != nil {
		t.Fatalf("ProjectSwitch: %v", err)
	}

	// Adapter current must point at a session in the target project.
	got := app.SessionCurrent().ID
	if got == "" {
		t.Fatal("no current session after project switch")
	}
	if got == firstID {
		t.Fatal("project switch did not change the current session")
	}

	// The old session must still be alive on the owner.
	if _, err := svc.SessionSummaryForSession(firstID); err != nil {
		t.Fatalf("old session not alive after switch: %v", err)
	}

	// ProjectName must reflect the target directory.
	if name := app.ProjectName(); name != filepath.Base(targetDir) {
		t.Fatalf("ProjectName = %q, want %q", name, filepath.Base(targetDir))
	}
}

func TestProjectSwitchNoOpSameDir(t *testing.T) {
	svc := newAppTestAgent(t)
	app := newTestApp(svc)

	firstID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	app.setCurrentSessionID(firstID)

	if err := app.ProjectSwitch(svc.ProjectRoot()); err != nil {
		t.Fatalf("ProjectSwitch same dir: %v", err)
	}
	if got := app.SessionCurrent().ID; got != firstID {
		t.Fatalf("current = %q, want %q (no-op switch should not change session)", got, firstID)
	}
}

// TestProjectSwitchSkipsContendedNewestSession proves a project navigation
// opens the newest candidate whose claim is acquirable: the newest session is
// held by another owner, so the older one opens instead of the navigation
// failing on contention.
func TestProjectSwitchSkipsContendedNewestSession(t *testing.T) {
	first, second := newAppTestAgentPair(t)
	projectPath := t.TempDir()
	olderID, err := second.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath older: %v", err)
	}
	newestID, err := first.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath newest: %v", err)
	}
	stampSessionActivity(t, second, projectPath, olderID, 1)
	stampSessionActivity(t, first, projectPath, newestID, 2)

	app := newTestApp(second)
	if err := app.ProjectSwitch(projectPath); err != nil {
		t.Fatalf("ProjectSwitch over a contended newest session: %v", err)
	}
	if got := app.SessionCurrent().ID; got != olderID {
		t.Fatalf("current after switch = %q, want the older session %q", got, olderID)
	}
	runtime.KeepAlive(first)
}

// TestProjectSwitchCreatesWhenEveryCandidateContended proves a project
// navigation with every active candidate held by another owner creates a new
// session instead of failing the navigation.
func TestProjectSwitchCreatesWhenEveryCandidateContended(t *testing.T) {
	first, second := newAppTestAgentPair(t)
	projectPath := t.TempDir()
	olderID, err := first.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath older: %v", err)
	}
	newestID, err := first.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath newest: %v", err)
	}
	stampSessionActivity(t, first, projectPath, olderID, 1)
	stampSessionActivity(t, first, projectPath, newestID, 2)

	app := newTestApp(second)
	if err := app.ProjectSwitch(projectPath); err != nil {
		t.Fatalf("ProjectSwitch with every candidate contended: %v", err)
	}
	got := app.SessionCurrent().ID
	if got == "" || got == olderID || got == newestID {
		t.Fatalf("current after switch = %q, want a newly created session", got)
	}
	runtime.KeepAlive(first)
}

// TestProjectSwitchSurfacesListingFailure proves a project navigation that
// cannot list the project's sessions reports the listing failure instead of
// silently creating a new session.
func TestProjectSwitchSurfacesListingFailure(t *testing.T) {
	_, second := newAppTestAgentPair(t)
	projectPath := t.TempDir()
	listErr := fmt.Errorf("session listing failed")
	app := &App{
		svc:              &listingFailSvc{AdapterService: second, listErr: listErr},
		routeProjectPath: second.ProjectRoot(),
	}
	err := app.ProjectSwitch(projectPath)
	if err == nil {
		t.Fatal("ProjectSwitch with a failing session listing = nil error, want the listing failure")
	}
	if !strings.Contains(err.Error(), "session listing failed") {
		t.Fatalf("ProjectSwitch error = %v, want the listing failure", err)
	}
}

func TestArchiveNonCurrentKeepsView(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*App, string) error
	}{
		{name: "archive", run: func(app *App, id string) error { return app.SessionArchive(id) }},
		{name: "delete", run: func(app *App, id string) error { return app.SessionDelete(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newAppTestAgent(t)
			firstID, err := svc.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession first: %v", err)
			}
			secondID, err := svc.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession second: %v", err)
			}
			// Seed secondID with a message so it has a complete turn and
			// won't be discarded by Store.Close when closeIfCurrent runs.
			if _, err := svc.AppendUserMessageToSession(secondID, "second"); err != nil {
				t.Fatalf("append second: %v", err)
			}

			app := newTestApp(svc)
			// After two NewSession calls, backend-current is secondID.
			// Set adapter-current to firstID (not backend-current).
			app.setCurrentSessionID(firstID)

			if backend := svc.SessionCurrent().ID; backend != secondID {
				t.Fatalf("backend current = %q, want %q (setup invariant)", backend, secondID)
			}

			// Archive/delete the backend-current (secondID), not adapter-current.
			if err := tc.run(app, secondID); err != nil {
				t.Fatalf("%s second: %v", tc.name, err)
			}

			// Adapter view must stay on firstID.
			if got := app.SessionCurrent().ID; got != firstID {
				t.Fatalf("adapter current after %s = %q, want %q", tc.name, got, firstID)
			}
		})
	}
}

// TestSessionStartupCandidateFallbackContract proves a contended startup
// candidate does not spawn a spurious session: Init returns the id of the
// session it actually resumed (skipping candidates whose claim another holder
// owns), the adapter selects exactly that session, and a new session is
// created only when nothing was resumed.
func TestSessionStartupCandidateFallbackContract(t *testing.T) {
	svc := newAppTestAgent(t)
	proj, err := svc.Projects().Ensure()
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	sessionsRoot := svc.Projects().SessionsRoot(proj.ID)
	projectsRoot := svc.Projects().Root()
	projectRoot := svc.ProjectRoot()

	seed := func(lastActivity int64) string {
		t.Helper()
		if err := svc.Store().AttachSessionsRoot(sessionsRoot, projectsRoot, proj.ID); err != nil {
			t.Fatalf("attach sessions root: %v", err)
		}
		if err := svc.Store().BeginNewSession(projectRoot); err != nil {
			t.Fatalf("begin session: %v", err)
		}
		id := svc.Store().SessionID()
		if id == "" {
			t.Fatal("seeded session id is empty")
		}
		raw, err := json.Marshal(message.NewText(message.RoleUser, "seed"))
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		if err := svc.Store().AppendMessage(1, raw); err != nil {
			t.Fatalf("append message: %v", err)
		}
		if err := svc.Store().MarkTurnComplete(1); err != nil {
			t.Fatalf("mark turn complete: %v", err)
		}
		if _, err := svc.Store().Close(); err != nil {
			t.Fatalf("close session: %v", err)
		}
		stampSessionLastActivity(t, filepath.Join(sessionsRoot, id, "meta.json"), lastActivity)
		return id
	}

	olderID := seed(time.Now().Unix() - 5)
	newerID := seed(time.Now().Unix() - 2)

	// The newest candidate is contended by a claim another holder owns; the
	// older one is free. Both are durable root sessions, so the resume scan
	// hits the contended one first.
	claim, ok, err := snapshot.AcquireSessionClaim(projectsRoot, proj.ID, newerID)
	if err != nil {
		t.Fatalf("acquire claim: %v", err)
	}
	if !ok {
		t.Fatal("test setup: newest session claim unexpectedly held")
	}
	defer claim.Release()

	app := newTestApp(svc)
	app.emitFn = func(string, any) {}
	app.titleFn = func(string) {}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.startup(ctx)
	defer app.shutdown(context.Background())

	// The adapter is selected on the session actually resumed, not on the
	// first listed (contended) one.
	if got := app.SessionCurrent().ID; got != olderID {
		t.Fatalf("adapter selected %q after startup, want the resumed session %q", got, olderID)
	}

	// No spurious session was created for the contended candidate.
	sessions, err := svc.SessionListForProjectPath(app.routeProjectPath, "active")
	if err != nil {
		t.Fatalf("list active sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("startup left %d active sessions, want exactly the two seeded", len(sessions))
	}
	for _, s := range sessions {
		if s.ID == newerID || s.ID == olderID {
			continue
		}
		t.Fatalf("spurious session %q created during startup", s.ID)
	}
}

// stampSessionLastActivity rewrites a session's meta.json with a fixed
// LastActivity so the newest-first resume scan order is deterministic.
func stampSessionLastActivity(t *testing.T, metaPath string, lastActivity int64) {
	t.Helper()
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var meta snapshot.SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	meta.LastActivity = lastActivity
	out, err := json.Marshal(&meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(metaPath, out, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
}
