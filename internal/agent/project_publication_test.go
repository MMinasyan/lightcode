package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/config"
	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

func projectTestAgent(t *testing.T) (*Agent, *project.Resolver, string) {
	t.Helper()
	home := t.TempDir()
	root := t.TempDir()
	resolver, err := project.NewResolver(home, root)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{projects: resolver, projectRoot: root}
	a.rt = newRuntime(a, runtimeOptions{WorkspaceRoot: root})
	a.session = &session{rt: a.rt}
	return a, resolver, root
}

func projectIdentityLock(root, path string) string {
	abs, _ := filepath.Abs(path)
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return filepath.Join(root, ".locks", "identity", hex.EncodeToString(sum[:])+".lock")
}

func TestPresentProjectReadDoesNotWaitForIdentityLock(t *testing.T) {
	a, resolver, root := projectTestAgent(t)
	if _, err := resolver.Ensure(); err != nil {
		t.Fatal(err)
	}
	hold, err := atomicfs.Acquire(projectIdentityLock(resolver.Root(), root))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()

	done := make(chan error, 1)
	go func() {
		_, err := a.ProjectCurrentForPath(root)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("present project read under identity contention: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("present project read waited for the identity lock")
	}
}

func TestAbsentProjectReadReportsBusyWithoutPublication(t *testing.T) {
	a, resolver, root := projectTestAgent(t)
	hold, err := atomicfs.Acquire(projectIdentityLock(resolver.Root(), root))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()

	_, err = a.ProjectCurrentForPath(root)
	if !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("absent project read = %v, want ErrProjectBusy", err)
	}
	entries, err := os.ReadDir(resolver.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("busy project read published %q", entry.Name())
		}
	}
}

func TestWriterProjectAdmissionReportsBusyWithoutPublication(t *testing.T) {
	a, resolver, root := projectTestAgent(t)
	hold, err := atomicfs.Acquire(projectIdentityLock(resolver.Root(), root))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()

	_, err = a.resolveProjectForPath(root, true)
	if !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("project writer under identity contention = %v, want ErrProjectBusy", err)
	}
	entries, err := os.ReadDir(resolver.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("busy project writer published %q", entry.Name())
		}
	}
}

func TestExistingProjectWriterStillRequiresIdentityAdmission(t *testing.T) {
	a, resolver, root := projectTestAgent(t)
	if _, err := resolver.Ensure(); err != nil {
		t.Fatal(err)
	}
	hold, err := atomicfs.Acquire(projectIdentityLock(resolver.Root(), root))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	if _, err := a.resolveProjectForPath(root, true); !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("existing project writer under identity contention = %v, want ErrProjectBusy", err)
	}
}

func TestProjectWriterAfterOwnerClosePublishesNothing(t *testing.T) {
	a, resolver, root := projectTestAgent(t)
	parked, release := parkAtLifecycleAdmission(t, a)
	done := make(chan error, 1)
	go func() {
		_, err := a.resolveProjectForPath(root, true)
		done <- err
	}()
	select {
	case <-parked:
	case <-time.After(time.Second):
		t.Fatal("project writer did not reach lifecycle admission")
	}
	if !a.ShutdownOwner() {
		t.Fatal("shutdown reported abandoned work")
	}
	release()
	if err := <-done; !errors.Is(err, errOwnerClosed) {
		t.Fatalf("project writer after close = %v, want errOwnerClosed", err)
	}
	entries, err := os.ReadDir(resolver.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("close-first project writer published %q", entry.Name())
		}
	}
}

func TestOwnerActivityHelperSkipsForeignMetadataLock(t *testing.T) {
	a, resolver, root := projectTestAgent(t)
	p, err := resolver.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	store, err := snapshotStoreForProjectTest(resolver, p, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Detach()
	hold, err := atomicfs.Acquire(filepath.Join(resolver.Root(), p.ID, ".locks", "meta.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()

	done := make(chan struct{})
	go func() {
		a.rt.touchProjectActivityBeforeRun(store)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("owner activity helper waited for the metadata lock")
	}
}

func TestOwnerActivityHelperCompletesBeforeShutdownPublishesClosed(t *testing.T) {
	a, resolver, root := projectTestAgent(t)
	p, err := resolver.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(resolver.Root(), p.ID, "meta.json")
	p.LastActivity = 1
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := snapshotStoreForProjectTest(resolver, p, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Detach()

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	atomicfs.SyncFileFunc = func(f *os.File) error {
		once.Do(func() { close(entered) })
		<-release
		return f.Sync()
	}
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil; unblock() })

	helperDone := make(chan struct{})
	go func() {
		a.rt.touchProjectActivityBeforeRun(store)
		close(helperDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner activity helper did not enter the project write")
	}
	shutdownDone := make(chan bool, 1)
	go func() { shutdownDone <- a.ShutdownOwner() }()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown published closed before the admitted activity write completed")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case <-helperDone:
	case <-time.After(time.Second):
		t.Fatal("activity helper did not finish after release")
	}
	if !<-shutdownDone {
		t.Fatal("shutdown reported abandoned work")
	}
}

func TestOwnerActivityHelperSkipsAfterShutdown(t *testing.T) {
	a, resolver, root := projectTestAgent(t)
	p, err := resolver.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	p.LastActivity = 1
	meta, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resolver.Root(), p.ID, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := snapshotStoreForProjectTest(resolver, p, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Detach()
	if !a.ShutdownOwner() {
		t.Fatal("shutdown reported abandoned work")
	}
	called := make(chan struct{}, 1)
	atomicfs.SyncFileFunc = func(*os.File) error {
		called <- struct{}{}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil })
	a.rt.touchProjectActivityBeforeRun(store)
	select {
	case <-called:
		t.Fatal("activity helper wrote project metadata after shutdown")
	default:
	}
}

func TestNewSessionExplicitIDReSyncsExistingProjectBeforePublish(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	p, err := a.projects.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	failOnce := true
	atomicfs.SyncDirFunc = func(string) error {
		if failOnce {
			failOnce = false
			return errors.New("injected project sync failure")
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
	if _, err := a.NewSession(p.ID, "primary"); err == nil || !strings.Contains(err.Error(), "injected project sync failure") {
		t.Fatalf("first explicit-ID session = %v, want project sync failure", err)
	}
	if sessions, err := a.SessionListForProjectPath(p.Path, "active"); err != nil {
		t.Fatal(err)
	} else if len(sessions) != 0 {
		t.Fatalf("failed explicit-ID publication left %d sessions", len(sessions))
	}
	if _, err := a.NewSession(p.ID, "primary"); err != nil {
		t.Fatalf("explicit-ID retry: %v", err)
	}
}

func TestNewSessionExplicitIDAcceptsForeignPersistedID(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	p, err := a.projects.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(a.projects.Root(), p.ID, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored project.Project
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	stored.ID = "foreign-persisted-id"
	data, err = json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewSession(p.ID, "primary"); err != nil {
		t.Fatalf("explicit-ID creation with foreign persisted id: %v", err)
	}
}

func TestNewSessionExplicitIDRejectsForeignStoredPathWithoutPublication(t *testing.T) {
	for _, existingDestination := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing_destination=%t", existingDestination), func(t *testing.T) {
			a := newCatalogBackedTestAgent(t)
			requested, err := a.projects.Ensure()
			if err != nil {
				t.Fatal(err)
			}
			foreignPath := filepath.Join(t.TempDir(), "foreign-path")
			if existingDestination {
				if err := os.MkdirAll(foreignPath, 0o700); err != nil {
					t.Fatal(err)
				}
				if _, err := project.EnsureForPath(a.projects.Root(), foreignPath); err != nil {
					t.Fatal(err)
				}
			}
			metaPath := filepath.Join(a.projects.Root(), requested.ID, "meta.json")
			data, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatal(err)
			}
			var stored project.Project
			if err := json.Unmarshal(data, &stored); err != nil {
				t.Fatal(err)
			}
			stored.Path = foreignPath
			data, err = json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(metaPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.NewSession(requested.ID, "primary"); err == nil || (!strings.Contains(err.Error(), "stored path") && !strings.Contains(err.Error(), "resolved directory identity")) {
				t.Fatalf("corrupt explicit-ID creation = %v, want stored-path corruption error", err)
			}
			entries, err := os.ReadDir(filepath.Join(a.projects.Root(), requested.ID, "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("corrupt requested project published %d sessions", len(entries))
			}
			if existingDestination {
				foreign, err := project.FindByPath(a.projects.Root(), foreignPath)
				if err != nil {
					t.Fatal(err)
				}
				entries, err := os.ReadDir(filepath.Join(a.projects.Root(), foreign.ID, "sessions"))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("existing foreign destination published %d sessions", len(entries))
				}
			}
		})
	}
}

func TestNewSessionWriterWinsAdmissionBeforeClose(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	atomicfs.SyncFileFunc = func(f *os.File) error {
		once.Do(func() { close(entered) })
		<-release
		return f.Sync()
	}
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil; unblock() })
	created := make(chan string, 1)
	go func() {
		id, _ := a.NewSessionForProjectPath(filepath.Join(t.TempDir(), "writer"), "primary")
		created <- id
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not enter publication")
	}
	shutdown := make(chan bool, 1)
	go func() { shutdown <- a.ShutdownOwner() }()
	select {
	case <-shutdown:
		t.Fatal("shutdown completed before admitted writer")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	id := <-created
	if id == "" {
		t.Fatal("admitted writer did not publish a session")
	}
	if !<-shutdown {
		t.Fatal("shutdown after admitted writer reported abandoned work")
	}
}

func TestNewSessionExplicitIDCloseFirstPublishesNothing(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	p, err := a.projects.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if !a.ShutdownOwner() {
		t.Fatal("shutdown reported abandoned work")
	}
	if _, err := a.NewSession(p.ID, "primary"); !errors.Is(err, errOwnerClosed) {
		t.Fatalf("explicit-ID session after close = %v, want errOwnerClosed", err)
	}
	if sessions, err := a.SessionListForProjectPath(p.Path, "active"); err == nil && len(sessions) != 0 {
		t.Fatalf("close-first explicit-ID session published %d sessions", len(sessions))
	}
}

func TestArchivedSessionReactivationSkipsForeignMetaLock(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SessionArchive(id); err != nil {
		t.Fatal(err)
	}
	p, err := a.projects.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := atomicfs.Acquire(filepath.Join(a.projects.Root(), p.ID, ".locks", "meta.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	done := make(chan error, 1)
	go func() {
		_, err := a.OpenSession(id)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reactivation under project meta contention: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reactivation waited for the project metadata lock")
	}
	if !a.ShutdownOwner() {
		t.Fatal("shutdown after reactivation reported abandoned work")
	}
}

func TestAgentNewDiscoveryContentionThenReloadRecovery(t *testing.T) {
	foreignLockHolderChild()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"fresh-model","name":"Fresh","context_window":4096}]}`))
	}))
	defer server.Close()
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_DISCOVERY_KEY", "secret")
	configPath := filepath.Join(lightcodeDir, "config.json")
	configJSON := fmt.Sprintf(`{"providers":{"disc":{"name":"Discovery","transport":{"base_url":%q,"api_key_env":"LIGHTCODE_DISCOVERY_KEY"},"discovery":true,"models":{"seed":{"context_window":4096},"fresh-model":{"context_window":0}}}}}`, server.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAgentsTestConfig(t, configPath, `{"primary":{"model":"disc/seed"}}`)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(home, ".lightcode", "cache", "discovery", ".locks", "disc.lock")
	release := startForeignLockHolder(t, "TestAgentNewDiscoveryContentionThenReloadRecovery", lockPath)
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		release()
		t.Fatal(err)
	}
	if len(a.pendingCatalogWarnings) == 0 {
		t.Fatal("discovery contention returned no startup warning")
	}
	release()
	if err := a.Reload(); err != nil {
		t.Fatalf("reload after discovery holder release: %v", err)
	}
	if _, ok := a.catalog.Providers["disc"].Models["fresh-model"]; !ok {
		t.Fatalf("reloaded discovery catalog = %#v, want fresh-model", a.catalog.Providers["disc"].Models)
	}
}

func TestEveryLoopRunPathUnderForeignProjectMetaLock(t *testing.T) {
	foreignLockHolderChild()
	for _, path := range []string{"immediate", "queued", "signal", "child", "compact"} {
		t.Run(path, func(t *testing.T) {
			blocked := path == "queued"
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				_ = json.NewDecoder(r.Body).Decode(&req)
				stream, _ := req["stream"].(bool)
				mu.Lock()
				calls++
				first := calls == 1
				mu.Unlock()
				if blocked && first {
					entered <- struct{}{}
					<-release
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", fmt.Sprintf(`{"id":"test","object":"chat.completion.chunk","choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"summary"}}]}`))
			}))
			defer server.Close()
			a := newEventOrderAgent(t, server.URL)
			cap := &eventCapture{}
			ctx := startEventOrderAgent(t, a, cap)
			id, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatal(err)
			}
			p, err := a.projects.Ensure()
			if err != nil {
				t.Fatal(err)
			}
			p.LastActivity = 1
			meta, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(a.projects.Root(), p.ID, "meta.json"), meta, 0o600); err != nil {
				t.Fatal(err)
			}
			releaseHolder := startForeignLockHolder(t, "TestEveryLoopRunPathUnderForeignProjectMetaLock", filepath.Join(a.projects.Root(), p.ID, ".locks", "meta.lock"))
			defer releaseHolder()

			switch path {
			case "immediate":
				if _, err := a.SubmitToSession(ctx, id, "immediate"); err != nil {
					t.Fatal(err)
				}
				waitUntilEventOrderAgentIdle(t, a)
			case "queued":
				if _, err := a.SubmitToSession(ctx, id, "first"); err != nil {
					t.Fatal(err)
				}
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("queued root turn did not enter the model")
				}
				if _, err := a.SubmitToSession(ctx, id, "queued"); err != nil {
					t.Fatal(err)
				}
				close(release)
				waitUntilFullyDrained(t, a)
			case "signal":
				a.session.lp.AddPendingSignal(loop.PendingSignal{Wake: true, Persist: true, Payload: "signal"})
				a.ensureRuntime().nudgeSignalScheduler()
				waitForActivityTurnEnd(t, cap, id)
				waitUntilEventOrderAgentIdle(t, a)
			case "child":
				parentTurn := a.store.BeginTurn()
				defer a.store.MarkTurnComplete(parentTurn)
				result := a.taskToolInst.runSubagent(ctx, 0, taskDef{Prompt: "child", SubagentType: "secondary"}, "")
				if result.err != nil {
					t.Fatalf("child run: %v", result.err)
				}
			case "compact":
				if _, err := a.AppendUserMessageToSession(id, "one"); err != nil {
					t.Fatal(err)
				}
				if _, err := a.AppendUserMessageToSession(id, "two"); err != nil {
					t.Fatal(err)
				}
				if err := a.CompactNowForSession(ctx, id); err != nil {
					t.Fatalf("compact run: %v", err)
				}
			}
			releaseHolder()

			found, err := project.FindByPath(a.projects.Root(), p.Path)
			if err != nil {
				t.Fatal(err)
			}
			if found.LastActivity != 1 {
				t.Fatalf("%s path touched project after the foreign lock was released: %d", path, found.LastActivity)
			}
		})
	}
}

func waitForActivityTurnEnd(t *testing.T, cap *eventCapture, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnEnd && ev.SessionID == sessionID {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("activity path did not publish turn_end")
}

func snapshotStoreForProjectTest(resolver *project.Resolver, p *project.Project, root string) (*snapshot.Store, error) {
	store, err := snapshot.NewForSessionsRoot(resolver.SessionsRoot(p.ID), resolver.Root(), p.ID)
	if err != nil {
		return nil, err
	}
	if err := store.BeginNewSession(root); err != nil {
		return nil, err
	}
	return store, nil
}

func TestOwnerLoopRunsHaveActivityAdmission(t *testing.T) {
	for path, names := range map[string][]string{
		"internal/agent/agent.go": {"s.unit.rt.touchProjectActivityBeforeRun(s.unit.store)", "rt.touchProjectActivityBeforeRun(unit.store)"},
		"internal/agent/task.go":  {"t.rt.touchProjectActivityBeforeRun(childStore)"},
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		for _, name := range names {
			if !strings.Contains(src, name) {
				t.Errorf("%s is missing %s", path, name)
			}
		}
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "agent", "runtime.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	body, ok := extractFunctionBody(src, "func (rt *runtime) touchProjectActivityBeforeRun(")
	if !ok {
		t.Fatal("activity helper is missing")
	}
	if strings.Index(body, "rt.mu.Unlock()") > strings.Index(body, "store.TryTouchProjectActivity()") {
		t.Fatal("activity helper performs filesystem work before releasing runtime.mu")
	}
}
