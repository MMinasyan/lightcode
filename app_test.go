package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/config"
)

// App.ReadFileContent (the Wails-bound surface) is a passthrough to
// agent.ReadFileContent. The Wails adapter must propagate the agent's
// boundary refusal.
func TestPR11Closure_AppReadFileContentPropagatesViewerBoundaryRefusal(t *testing.T) {
	app := &App{svc: newAppTestAgent(t)}

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
	if _, err := svc.AppendUserMessage("gone"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	id := svc.SessionCurrent().ID
	if id == "" {
		t.Fatal("missing session id")
	}

	app := &App{svc: svc}
	app.setCurrentSessionID(id)
	if _, err := svc.SessionDelete(id); err != nil {
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
	app := &App{svc: svc}
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

			app := &App{svc: svc}
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
			if app.acceptsEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: firstID, Result: "skip"}) {
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

	app := &App{svc: svc}
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

	app := &App{svc: svc}
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

func TestWailsSubagentFilter(t *testing.T) {
	svc := newAppTestAgent(t)
	if _, err := svc.AppendUserMessage("root"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	root := svc.SessionCurrent().ID
	if root == "" {
		t.Fatal("missing root session")
	}
	app := &App{svc: svc}

	app.setCurrentSessionID("")
	if app.acceptsEvent(agent.Event{Kind: agent.EventSubagentStart, SessionID: "child", SubagentSessionID: "child", ParentSessionID: root}) {
		t.Fatal("empty current accepted child event")
	}

	app.setCurrentSessionID(root)
	if app.acceptsEvent(agent.Event{Kind: agent.EventSubagentStart, SessionID: "child", SubagentSessionID: "child", ParentSessionID: "other"}) {
		t.Fatal("wrong parent accepted child event")
	}
	if !app.acceptsEvent(agent.Event{Kind: agent.EventSubagentStart, SessionID: "child", SubagentSessionID: "child", ParentSessionID: root}) {
		t.Fatal("matching parent child start rejected")
	}
	if !app.acceptsEvent(agent.Event{Kind: agent.EventTextDelta, SessionID: "child", SubagentSessionID: "child", Result: "ok"}) {
		t.Fatal("subscribed child event rejected")
	}

	oldRoot := app.liveCurrentSessionID()
	if err := svc.SessionNew(); err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	if _, err := svc.AppendUserMessage("next"); err != nil {
		t.Fatalf("seed next session: %v", err)
	}
	next := svc.SessionCurrent().ID
	if next == "" || next == oldRoot {
		t.Fatalf("new session id = %q, old = %q", next, oldRoot)
	}
	app.setCurrentSessionID(next)
	if app.acceptsSubagentEventForCurrent(oldRoot, agent.Event{Kind: agent.EventSubagentStart, SessionID: "old-child", SubagentSessionID: "old-child", ParentSessionID: oldRoot}) {
		t.Fatal("stale current snapshot accepted child event")
	}
}

func newAppTestAgent(t *testing.T) *agent.Agent {
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
}`, "http://127.0.0.1:9/v1")
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
