package agent

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// TestOwnerGlobalSessionIDResolverRejectsAmbiguity proves a bare session id
// that exists in more than one project is rejected by every adapter-facing
// route that resolves ids against disk: the id is ambiguous, so no route may
// silently pick one project over another.
func TestOwnerGlobalSessionIDResolverRejectsAmbiguity(t *testing.T) {
	routes := []struct {
		name string
		run  func(*persistedAmbiguousSessionFixture) error
	}{
		{
			name: "OpenSession",
			run: func(f *persistedAmbiguousSessionFixture) error {
				_, err := f.agent.OpenSession(f.id)
				return err
			},
		},
		{
			name: "OpenSessionWithBoundary",
			run: func(f *persistedAmbiguousSessionFixture) error {
				boundary := 0
				_, err := f.agent.OpenSessionWithBoundary(f.id, func(HydrationState) { boundary++ })
				if boundary != 0 {
					return fmt.Errorf("boundary count = %d", boundary)
				}
				return err
			},
		},
		{
			name: "SessionMessagesFor",
			run: func(f *persistedAmbiguousSessionFixture) error {
				_, err := f.agent.SessionMessagesFor(f.id)
				return err
			},
		},
		{
			name: "HydrateSession",
			run: func(f *persistedAmbiguousSessionFixture) error {
				_, err := f.agent.HydrateSession(f.id)
				return err
			},
		},
		{
			name: "SessionSummaryForSessionOrPersisted",
			run: func(f *persistedAmbiguousSessionFixture) error {
				_, err := f.agent.SessionSummaryForSessionOrPersisted(f.id)
				return err
			},
		},
		{
			name: "sessionsRootForSession",
			run: func(f *persistedAmbiguousSessionFixture) error {
				_, err := f.agent.sessionsRootForSession(f.id)
				return err
			},
		},
		{
			name: "SessionArchive",
			run:  func(f *persistedAmbiguousSessionFixture) error { return f.agent.SessionArchive(f.id) },
		},
		{
			name: "SessionDelete",
			run:  func(f *persistedAmbiguousSessionFixture) error { return f.agent.SessionDelete(f.id) },
		},
	}
	for _, route := range routes {
		route := route
		t.Run(route.name, func(t *testing.T) {
			f := newPersistedAmbiguousSessionFixture(t)
			err := route.run(&f)
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("route error = %v, want ambiguity rejection", err)
			}
			f.assertUntouched(t)
		})
	}

	for _, route := range []struct {
		name string
		run  func(*persistedAmbiguousSessionFixture, string) error
	}{
		{
			name: "RespondPermissionForSession",
			run: func(f *persistedAmbiguousSessionFixture, requestID string) error {
				return f.agent.RespondPermissionForSession(f.id, requestID, true)
			},
		},
		{
			name: "RespondPermissionActionForSession",
			run: func(f *persistedAmbiguousSessionFixture, requestID string) error {
				return f.agent.RespondPermissionActionForSession(f.id, requestID, "allow")
			},
		},
		{
			name: "SaveProjectPermissionForSession",
			run: func(f *persistedAmbiguousSessionFixture, requestID string) error {
				return f.agent.SaveProjectPermissionForSession(f.id, requestID, []string{"run_command(marker)"})
			},
		},
	} {
		route := route
		t.Run(route.name, func(t *testing.T) {
			f := newPersistedAmbiguousSessionFixture(t)
			requestID, gateDone := pendingPermissionRequest(t, f.agent, f.id, f.first.ID)
			err := route.run(&f, requestID)
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("route error = %v, want ambiguity rejection", err)
			}
			if got := f.agent.gate.PendingForSession(f.id); len(got) != 1 {
				t.Fatalf("pending permission requests = %d, want 1", len(got))
			}
			f.assertUntouched(t)
			gateDone()
		})
	}
}

type persistedAmbiguousSessionFixture struct {
	agent      *Agent
	id         string
	first      *project.Project
	second     *project.Project
	firstMeta  []byte
	secondMeta []byte
}

func newPersistedAmbiguousSessionFixture(t *testing.T) persistedAmbiguousSessionFixture {
	t.Helper()
	a := newCatalogBackedTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)

	first, err := a.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure first project: %v", err)
	}
	second, err := project.EnsureForPath(a.projects.Root(), t.TempDir())
	if err != nil {
		t.Fatalf("ensure second project: %v", err)
	}
	const sharedID = "d00dcafe"
	metas := [][]byte{
		[]byte(`{"id":"` + sharedID + `","state":"active","project_path":"` + first.Path + `","last_activity":111}` + "\n"),
		[]byte(`{"id":"` + sharedID + `","state":"active","project_path":"` + second.Path + `","last_activity":222}` + "\n"),
	}
	for i, proj := range []*project.Project{first, second} {
		dir := filepath.Join(a.projects.SessionsRoot(proj.ID), sharedID)
		if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "turns"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), metas[i], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return persistedAmbiguousSessionFixture{
		agent:      a,
		id:         sharedID,
		first:      first,
		second:     second,
		firstMeta:  metas[0],
		secondMeta: metas[1],
	}
}

func (f persistedAmbiguousSessionFixture) assertUntouched(t *testing.T) {
	t.Helper()
	for _, item := range []struct {
		project *project.Project
		want    []byte
	}{
		{project: f.first, want: f.firstMeta},
		{project: f.second, want: f.secondMeta},
	} {
		got, err := os.ReadFile(filepath.Join(f.agent.projects.SessionsRoot(item.project.ID), f.id, "meta.json"))
		if err != nil {
			t.Fatalf("read metadata for project %s: %v", item.project.ID, err)
		}
		if string(got) != string(item.want) {
			t.Fatalf("metadata for project %s changed: got %q want %q", item.project.ID, got, item.want)
		}
		claim, ok, err := snapshot.AcquireSessionClaim(f.agent.projects.Root(), item.project.ID, f.id)
		if err != nil || !ok {
			t.Fatalf("claim for project %s after route = %v, %v; want available", item.project.ID, ok, err)
		}
		if err := claim.Release(); err != nil {
			t.Fatalf("release claim for project %s: %v", item.project.ID, err)
		}
	}
	rt := f.agent.ensureRuntime()
	rt.mu.Lock()
	unit := f.agent.sessions[f.id]
	rt.mu.Unlock()
	if unit != nil {
		t.Fatal("ambiguous persisted id was registered as live")
	}
	for _, proj := range []*project.Project{f.first, f.second} {
		rules, err := permission.LoadLocal(f.agent.projects.Root(), proj.ID)
		if err != nil {
			t.Fatalf("load project %s permissions: %v", proj.ID, err)
		}
		if len(rules.Allow) != 0 {
			t.Fatalf("project %s permissions = %#v, want empty", proj.ID, rules.Allow)
		}
	}
}

type ambiguousSessionFixture struct {
	agent      *Agent
	id         string
	first      *project.Project
	second     *project.Project
	firstMeta  []byte
	secondMeta []byte
	cancelled  *int
}

func newLiveAmbiguousSessionFixture(t *testing.T) ambiguousSessionFixture {
	t.Helper()
	a := newCatalogBackedTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)

	first, err := a.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure first project: %v", err)
	}
	second, err := project.EnsureForPath(a.projects.Root(), t.TempDir())
	if err != nil {
		t.Fatalf("ensure second project: %v", err)
	}
	id, err := a.NewSession(first.ID, "primary")
	if err != nil {
		t.Fatalf("new live session: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(id, "live-marker"); err != nil {
		t.Fatalf("append live marker: %v", err)
	}
	cancelled := 0
	a.ensureRuntime().mu.Lock()
	unit := a.sessions[id]
	unit.queue = []QueuedItem{{ID: "queue-marker", Content: "queued-marker"}}
	unit.queueVersion = 1
	unit.turnCancel = func() { cancelled++ }
	a.ensureRuntime().mu.Unlock()

	secondDir := filepath.Join(a.projects.SessionsRoot(second.ID), id)
	if err := os.MkdirAll(filepath.Join(secondDir, "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(secondDir, "turns"), 0o700); err != nil {
		t.Fatal(err)
	}
	secondMeta := []byte(`{"id":"` + id + `","state":"active","project_path":"` + second.Path + `","last_activity":222}` + "\n")
	if err := os.WriteFile(filepath.Join(secondDir, "meta.json"), secondMeta, 0o600); err != nil {
		t.Fatal(err)
	}

	firstDir := filepath.Join(a.projects.SessionsRoot(first.ID), id)
	firstMeta, err := os.ReadFile(filepath.Join(firstDir, "meta.json"))
	if err != nil {
		t.Fatalf("read live metadata: %v", err)
	}
	return ambiguousSessionFixture{
		agent:      a,
		id:         id,
		first:      first,
		second:     second,
		firstMeta:  firstMeta,
		secondMeta: secondMeta,
		cancelled:  &cancelled,
	}
}

func (f ambiguousSessionFixture) assertDurableMarkers(t *testing.T) {
	t.Helper()
	firstMeta, err := os.ReadFile(filepath.Join(f.agent.projects.SessionsRoot(f.first.ID), f.id, "meta.json"))
	if err != nil {
		t.Fatalf("read live metadata after route: %v", err)
	}
	if string(firstMeta) != string(f.firstMeta) {
		t.Fatalf("live metadata changed: got %q want %q", firstMeta, f.firstMeta)
	}
	secondMeta, err := os.ReadFile(filepath.Join(f.agent.projects.SessionsRoot(f.second.ID), f.id, "meta.json"))
	if err != nil {
		t.Fatalf("read persisted metadata after route: %v", err)
	}
	if string(secondMeta) != string(f.secondMeta) {
		t.Fatalf("persisted metadata changed: got %q want %q", secondMeta, f.secondMeta)
	}
	claim, ok, err := snapshot.AcquireSessionClaim(f.agent.projects.Root(), f.second.ID, f.id)
	if err != nil || !ok {
		t.Fatalf("persisted session claim after route = %v, %v; want available", ok, err)
	}
	if err := claim.Release(); err != nil {
		t.Fatalf("release persisted session claim: %v", err)
	}
}

func (f ambiguousSessionFixture) assertLiveUnitUntouched(t *testing.T) {
	t.Helper()
	f.agent.ensureRuntime().mu.Lock()
	unit := f.agent.sessions[f.id]
	f.agent.ensureRuntime().mu.Unlock()
	if unit == nil {
		t.Fatal("live session was evicted")
	}
	if unit.store == nil || !unit.store.Active() {
		t.Fatal("live session store was detached")
	}
	if unit.transitioning {
		t.Fatal("live session was left transitioning")
	}
	if len(unit.queue) != 1 || unit.queue[0].Content != "queued-marker" {
		t.Fatalf("live queue after route = %#v, want queued marker", unit.queue)
	}
	if *f.cancelled != 0 {
		t.Fatalf("live session cancellation count = %d, want 0", *f.cancelled)
	}
	got, err := f.agent.messagesForFrontendForStore(unit.store, f.id)
	if err != nil || !strings.Contains(displayText(got), "live-marker") {
		t.Fatalf("live marker after route = %q, err=%v", displayText(got), err)
	}
}

func displayText(messages []DisplayMessage) string {
	var b strings.Builder
	for _, message := range messages {
		b.WriteString(message.Content)
	}
	return b.String()
}

func TestLiveAndPersistedSessionIDAmbiguityStopsEveryExternalRoute(t *testing.T) {
	routes := []struct {
		name string
		run  func(*ambiguousSessionFixture) error
	}{
		{
			name: "open",
			run: func(f *ambiguousSessionFixture) error {
				_, err := f.agent.OpenSession(f.id)
				return err
			},
		},
		{
			name: "open_with_boundary",
			run: func(f *ambiguousSessionFixture) error {
				boundary := 0
				_, err := f.agent.OpenSessionWithBoundary(f.id, func(HydrationState) { boundary++ })
				if boundary != 0 {
					return fmt.Errorf("boundary count = %d", boundary)
				}
				return err
			},
		},
		{
			name: "messages",
			run: func(f *ambiguousSessionFixture) error {
				_, err := f.agent.SessionMessagesFor(f.id)
				return err
			},
		},
		{
			name: "sessions_root",
			run: func(f *ambiguousSessionFixture) error {
				_, err := f.agent.sessionsRootForSession(f.id)
				return err
			},
		},
		{
			name: "hydrate",
			run: func(f *ambiguousSessionFixture) error {
				_, err := f.agent.HydrateSession(f.id)
				return err
			},
		},
		{
			name: "summary",
			run: func(f *ambiguousSessionFixture) error {
				_, err := f.agent.SessionSummaryForSessionOrPersisted(f.id)
				return err
			},
		},
		{
			name: "archive",
			run:  func(f *ambiguousSessionFixture) error { return f.agent.SessionArchive(f.id) },
		},
		{
			name: "delete",
			run:  func(f *ambiguousSessionFixture) error { return f.agent.SessionDelete(f.id) },
		},
	}

	for _, route := range routes {
		route := route
		t.Run(route.name, func(t *testing.T) {
			f := newLiveAmbiguousSessionFixture(t)
			err := route.run(&f)
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("route error = %v, want ambiguity rejection", err)
			}
			f.assertDurableMarkers(t)
			f.assertLiveUnitUntouched(t)
		})
	}

	for _, route := range []struct {
		name string
		run  func(*ambiguousSessionFixture, string) error
	}{
		{
			name: "permission_response",
			run: func(f *ambiguousSessionFixture, requestID string) error {
				return f.agent.RespondPermissionForSession(f.id, requestID, true)
			},
		},
		{
			name: "permission_action",
			run: func(f *ambiguousSessionFixture, requestID string) error {
				return f.agent.RespondPermissionActionForSession(f.id, requestID, "allow")
			},
		},
		{
			name: "project_permission_save",
			run: func(f *ambiguousSessionFixture, requestID string) error {
				return f.agent.SaveProjectPermissionForSession(f.id, requestID, []string{"run_command(marker)"})
			},
		},
	} {
		route := route
		t.Run(route.name, func(t *testing.T) {
			f := newLiveAmbiguousSessionFixture(t)
			requestID, gateDone := pendingPermissionRequest(t, f.agent, f.id, f.first.ID)
			err := route.run(&f, requestID)
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("route error = %v, want ambiguity rejection", err)
			}
			if got := f.agent.gate.PendingForSession(f.id); len(got) != 1 {
				t.Fatalf("pending permission requests = %d, want 1", len(got))
			}
			f.assertDurableMarkers(t)
			f.assertLiveUnitUntouched(t)
			if rules, err := permission.LoadLocal(f.agent.projects.Root(), f.first.ID); err != nil {
				t.Fatalf("load first project permissions: %v", err)
			} else if len(rules.Allow) != 0 {
				t.Fatalf("first project permissions = %#v, want empty", rules.Allow)
			}
			gateDone()
		})
	}
}

func pendingPermissionRequest(t *testing.T, a *Agent, sessionID, projectID string) (string, func()) {
	t.Helper()
	ids := make(chan string, 1)
	gate := permission.NewGate(func(_ context.Context, req permission.Request) { ids <- req.ID })
	a.gate = gate
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan permission.ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(ctx, permission.Request{SessionID: sessionID, ProjectID: projectID, ToolName: "run_command", Arg: "marker"})
	}()
	id := <-ids
	return id, func() {
		gate.CancelAll()
		cancel()
		<-result
	}
}

func TestSelectedLiveCancellationDoesNotResolveDiskAmbiguity(t *testing.T) {
	f := newLiveAmbiguousSessionFixture(t)
	f.agent.ensureRuntime().mu.Lock()
	unit := f.agent.sessions[f.id]
	unit.busy = true
	cancelled := false
	unit.turnCancel = func() { cancelled = true }
	f.agent.ensureRuntime().mu.Unlock()

	got, err := f.agent.CancelBusySession(f.id)
	if err != nil || !got {
		t.Fatalf("CancelBusySession = %v, %v; want true, nil", got, err)
	}
	if !cancelled {
		t.Fatal("selected live session was not cancelled")
	}
}

func TestSessionPointerHelpersStayLockLocal(t *testing.T) {
	f := newLiveAmbiguousSessionFixture(t)
	rt := f.agent.ensureRuntime()
	rt.mu.Lock()
	unit, err := f.agent.liveSessionLocked(f.id)
	rt.mu.Unlock()
	if err != nil || unit == nil {
		t.Fatalf("liveSessionLocked = %v, %v", unit, err)
	}
	root, ok := f.agent.liveSessionsRoot(f.id)
	if !ok || root != f.agent.projects.SessionsRoot(f.first.ID) {
		t.Fatalf("liveSessionsRoot = %q, %v", root, ok)
	}
}

func TestSessionIdentityRoutesHaveOneLivePointerAuthority(t *testing.T) {
	data, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "agent.go", data, 0)
	if err != nil {
		t.Fatalf("parse agent.go: %v", err)
	}

	allowedLiveCalls := map[string]bool{
		"resolveLiveSession":            true,
		"resolveUnambiguousLiveSession": true,
		"liveSessionLockedForOperation": true,
	}
	allowedMapFunctions := map[string]bool{
		"registerLiveSessionLocked": true,
		"liveUnitLocked":            true,
		"liveSessionLocked":         true,
		"CancelBusySession":         true,
	}

	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				selector, ok := node.Fun.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "liveSessionLocked" && !allowedLiveCalls[name] {
					t.Errorf("liveSessionLocked called from external function %s", name)
				}
				if ok && selector.Sel.Name == "liveSessionsRoot" {
					t.Errorf("liveSessionsRoot called from function %s", name)
				}
			case *ast.IndexExpr:
				selector, ok := node.X.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "sessions" && !allowedMapFunctions[name] {
					t.Errorf("direct sessions map access from external function %s", name)
				}
			}
			return true
		})
	}

	cancel, ok := extractFunctionBody(string(data), "func (a *Agent) CancelBusySession(")
	if !ok {
		t.Fatal("CancelBusySession not found")
	}
	if strings.Contains(cancel, "resolveUnambiguousLiveSession") || strings.Contains(cancel, "projectForExistingSession") || strings.Contains(cancel, "liveSessionLocked") {
		t.Fatal("CancelBusySession must remain a direct selected-live exception")
	}
}
