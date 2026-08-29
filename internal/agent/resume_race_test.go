package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/snapshot"
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
	writeAgentsTestConfig(t, configPath, `{"primary": {"model": "test/test-model"}}`)
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

// TestResumeSkipsArchivedCandidateUnderClaim proves startup resume revalidates
// the candidate's durable state under the claim LoadSession takes: a session
// listed active by the pre-claim enumeration but archived before the load is
// detached and skipped under its claim, so a stale listing can never register
// archived identity live, and the skip releases the claim.
func TestResumeSkipsArchivedCandidateUnderClaim(t *testing.T) {
	t.Setenv("LIGHTCODE_RESUME_KEY", "test-key")
	home, projectRoot := t.TempDir(), t.TempDir()
	// Create a persisted session with a complete turn.
	a1 := newResumeRaceAgent(t, home, projectRoot)
	proj, err := a1.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	sessionsRoot := a1.projects.SessionsRoot(proj.ID)
	if err := a1.store.AttachSessionsRoot(sessionsRoot, a1.projects.Root(), proj.ID); err != nil {
		t.Fatalf("attach sessions root: %v", err)
	}
	if err := a1.store.BeginNewSession(projectRoot); err != nil {
		t.Fatalf("begin session: %v", err)
	}
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
	sessionID := a1.store.SessionID()
	if sessionID == "" {
		t.Fatal("no session id after begin")
	}
	if _, err := a1.store.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}

	// A fresh owner resumes with the candidate archived between the pre-claim
	// listing and the claim-held load: the listing still names it active, and
	// the archive lands before LoadSession takes its claim — the
	// archived-after-list race the claim-held revalidation exists for.
	a2 := newResumeRaceAgent(t, home, projectRoot)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	firstHook := true
	parked := make(chan struct{})
	release := make(chan struct{})
	var parkOnce sync.Once
	a2.durableReadHook = func() {
		if firstHook {
			firstHook = false
			return
		}
		parkOnce.Do(func() { close(parked) })
		<-release
	}
	defer func() { a2.durableReadHook = nil }()
	initDone := make(chan struct{})
	go func() {
		a2.Init(ctx2)
		close(initDone)
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("resume never parked between the listing and the candidate load")
	}
	if err := snapshot.ArchiveSession(sessionsRoot, sessionID); err != nil {
		t.Fatalf("archive session: %v", err)
	}
	close(release)
	select {
	case <-initDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Init did not complete after the release")
	}
	if cur := a2.SessionCurrent().ID; cur != "" {
		t.Fatalf("resume registered the archived session %q as current", cur)
	}
	// The skip released the claim: a fresh store can load the session.
	fresh, err := snapshot.NewForSessionsRoot(sessionsRoot, a2.projects.Root(), proj.ID)
	if err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	if err := fresh.LoadSession(sessionID); err != nil {
		t.Fatalf("archived candidate claim still held after the skip: %v", err)
	}
	fresh.Detach()
}

// TestResumeSkipsCorruptNewestCandidateAndContinues proves resume's
// claim-held metadata revalidation continues past a corrupt newest candidate:
// with the newest candidate's meta corrupted while parked immediately before
// its claim-held Meta() read, the resume detaches it (releasing the claim),
// does not register it, continues to and resumes the older healthy candidate,
// sets current/routing exactly to it, and a fresh store can claim the corrupt
// candidate once valid metadata is restored.
func TestResumeSkipsCorruptNewestCandidateAndContinues(t *testing.T) {
	t.Setenv("LIGHTCODE_RESUME_KEY", "test-key")
	home, projectRoot := t.TempDir(), t.TempDir()
	// Seed two root candidates with explicitly distinct persisted LastActivity
	// seconds: the older (1000) and the newer (2000), so the resume scans the
	// newer first.
	a1 := newResumeRaceAgent(t, home, projectRoot)
	proj, err := a1.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	sessionsRoot := a1.projects.SessionsRoot(proj.ID)
	if err := a1.store.AttachSessionsRoot(sessionsRoot, a1.projects.Root(), proj.ID); err != nil {
		t.Fatalf("attach sessions root: %v", err)
	}
	seedResumeCandidate := func(lastActivity int64) string {
		t.Helper()
		if err := a1.store.BeginNewSession(projectRoot); err != nil {
			t.Fatalf("begin session: %v", err)
		}
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
		if err := a1.store.SetLastActivity(lastActivity); err != nil {
			t.Fatalf("set last activity: %v", err)
		}
		id := a1.store.SessionID()
		if _, err := a1.store.Close(); err != nil {
			t.Fatalf("close session: %v", err)
		}
		return id
	}
	// Distinct recent LastActivity seconds (within the archive window so the
	// init sweep never archives them): the older resumes last.
	olderID := seedResumeCandidate(time.Now().Unix() - 7200)
	newerID := seedResumeCandidate(time.Now().Unix() - 3600)

	// A fresh owner resumes. The durableReadHook parks immediately before the
	// newest candidate's claim-held Meta() read (the resume fires: listing=1,
	// newest LoadSession=2, newest Meta=3, ...); the newest meta is corrupted
	// while parked.
	a2 := newResumeRaceAgent(t, home, projectRoot)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	parked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fire := 0
	a2.durableReadHook = func() {
		fire++
		if fire == 3 {
			once.Do(func() { close(parked) })
			<-release
		}
	}
	initDone := make(chan struct{})
	go func() {
		a2.Init(ctx2)
		close(initDone)
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("resume never parked before the newest candidate's claim-held Meta read")
	}
	newestMeta := filepath.Join(sessionsRoot, newerID, "meta.json")
	if err := os.WriteFile(newestMeta, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-initDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Init did not complete after the release")
	}

	// The corrupt newest candidate was skipped: current/routing point exactly
	// at the older healthy candidate.
	if cur := a2.SessionCurrent().ID; cur != olderID {
		t.Fatalf("SessionCurrent = %q, want the older healthy candidate %q", cur, olderID)
	}
	rt := a2.ensureRuntime()
	rt.mu.Lock()
	current := a2.currentSessionID
	_, newerRegistered := a2.sessions[newerID]
	_, olderRegistered := a2.sessions[olderID]
	rt.mu.Unlock()
	if current != olderID {
		t.Fatalf("currentSessionID = %q, want %q", current, olderID)
	}
	if newerRegistered {
		t.Fatal("corrupt newest candidate was registered live")
	}
	if !olderRegistered {
		t.Fatal("older healthy candidate was not registered")
	}
	// The corrupt candidate's claim was released: once valid metadata is
	// restored, a fresh store can claim it.
	if err := os.WriteFile(newestMeta, []byte(`{"id":`+strconv.Quote(newerID)+`,"state":"active"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, err := snapshot.NewForSessionsRoot(sessionsRoot, a2.projects.Root(), proj.ID)
	if err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	if err := fresh.LoadSession(newerID); err != nil {
		t.Fatalf("corrupt candidate claim still held after the skip: %v", err)
	}
	fresh.Detach()
}
