package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/engine/message"
)

// newResumeRaceAgent builds an agent rooted at the shared home/projectRoot with a
// test provider exposing test-model (the default) and alt-model.
func newResumeRaceAgent(t *testing.T, home, projectRoot string) *Agent {
	t.Helper()
	configPath := filepath.Join(home, ".lightcode", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_RESUME_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192 },
        "alt-model": { "name": "Alt Model", "context_window": 4096 }
      }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

// TestResumeRestoresModelUnderLock exercises session resume with the runtime's
// reader goroutines active and a concurrent CurrentModel() reader, so that
// `go test -race` proves restoreModelFromSession's currentRef / client writes are
// synchronized under rt.mu. Before the fix (the restore ran outside rt.mu) the
// race detector flagged the write against the locked readers.
func TestResumeRestoresModelUnderLock(t *testing.T) {
	t.Setenv("LIGHTCODE_RESUME_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	home, projectRoot := t.TempDir(), t.TempDir()

	// Persist a session pinned to alt-model (distinct from the default test-model)
	// so a successful resume is observable. Sessions are created lazily on the
	// first turn, so build the fixture directly via the store/project primitives
	// the agent itself uses (AttachSessionsRoot -> BeginNewSession -> SetModel).
	a1 := newResumeRaceAgent(t, home, projectRoot)
	proj, err := a1.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if err := a1.store.AttachSessionsRoot(a1.projects.SessionsRoot(proj.ID), a1.projects.Root(), proj.ID); err != nil {
		t.Fatalf("attach sessions root: %v", err)
	}
	if err := a1.store.BeginNewSession(projectRoot); err != nil {
		t.Fatalf("begin session: %v", err)
	}
	if err := a1.store.SetModel("test", "alt-model"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	// A session is only persisted (and thus resumable) once it carries a completed
	// turn, so append one message and mark the turn complete before closing.
	raw, err := json.Marshal(message.NewText(message.RoleUser, "hello"))
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if err := a1.store.AppendMessage(1, raw); err != nil {
		t.Fatalf("append message: %v", err)
	}
	if err := a1.store.MarkTurnComplete(1); err != nil {
		t.Fatalf("mark turn complete: %v", err)
	}
	if _, err := a1.store.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}

	// Agent 2 resumes that session while a goroutine hammers the locked reader and
	// Init's own reader goroutines run.
	a2 := newResumeRaceAgent(t, home, projectRoot)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	ready := make(chan struct{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(ready) // the reader is scheduled and about to loop before resume runs
		for {
			select {
			case <-stop:
				return
			default:
				_ = a2.CurrentModel() // locked read of currentRef, racing the restore
			}
		}
	}()
	<-ready
	a2.Init(ctx2) // starts reader goroutines AND resumes (restore) concurrently
	close(stop)
	wg.Wait()

	if got := a2.CurrentModel().Model; got != "alt-model" {
		t.Fatalf("resumed model = %q, want alt-model", got)
	}
}
