package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// App.ReadFileContent (the Wails-bound surface) is a passthrough to
// agent.ReadFileContent. The Wails adapter must propagate the agent's
// boundary refusal.
func TestPR11Closure_AppReadFileContentPropagatesViewerBoundaryRefusal(t *testing.T) {
	app := newTestApp(newAppTestAgent(t))

	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	content, err := app.ReadFileContent(outsideSecret)
	if err == nil {
		t.Fatalf("App.ReadFileContent(%q) succeeded with content %q; want boundary refusal propagated from agent", outsideSecret, content)
	}
	if strings.Contains(content, "outside-secret") {
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
		if got := app.ProjectCurrent().ID; got != wantOther.ID {
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
	if got := app.ProjectCurrent().ID; got != proj.ID {
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
	if got := app.ProjectCurrent().ID; got != proj.ID {
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
	if got := app.ProjectCurrent().ID; got != proj.ID {
		t.Fatalf("ProjectCurrent after switch = %q, want the project it lives in %q", got, proj.ID)
	}
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
