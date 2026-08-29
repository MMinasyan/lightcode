package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// TestDurableIOAgainstOwnerLocks asserts the position of each site's durable
// I/O relative to its owner lock under the current arrangement: the fork's
// staged copy and every candidate read run outside runtime.mu (only the
// source admission snapshot and the registration/boundary section hold it),
// resume's listing/meta/history loads run under lifecycleMu, the tokens file
// read runs outside the unit's tokensMu, and the tokens file write runs
// inside it.
//
// The assertion needs durableReadHook because lock ownership is unobservable
// from outside the locked code. The hook fires at the exact moment each durable
// I/O is about to run, and each scenario's closure probes the site's owner lock
// with TryLock: a lock held by the current goroutine cannot be acquired, so the
// probe reads true exactly when the I/O would run under the lock, while an
// uncontended lock is acquired and immediately released, so a false reading
// means the I/O is running outside it.
func TestDurableIOAgainstOwnerLocks(t *testing.T) {
	t.Run("fork_tree_copy_outside_runtime_lock", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "one")
		sourceID := a.SessionCurrent().ID
		obs := probeOwnerLock(a, &a.ensureRuntime().mu)
		defer func() { obs() }()
		if err := a.ForkSessionForSession(sourceID, 1); err != nil {
			t.Fatalf("fork: %v", err)
		}
		// The staged copy and every candidate read run outside runtime.mu; the
		// lock is held only for the admission snapshot and the final
		// registration/boundary section, neither of which performs durable I/O.
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable fork I/O observed")
		}
		for i, held := range got {
			if held {
				t.Fatalf("fork durable I/O %d ran while runtime.mu was held", i+1)
			}
		}
	})

	t.Run("fork_action_copy_outside_runtime_lock", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		clicked := appendUserTurn(t, a, "one")
		sourceID := a.SessionCurrent().ID
		obs := probeOwnerLock(a, &a.ensureRuntime().mu)
		defer func() { obs() }()
		if _, err := a.ApplyTurnActionForSession(sourceID, clicked, TurnActionFork, false); err != nil {
			t.Fatalf("fork action: %v", err)
		}
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable fork I/O observed")
		}
		for i, held := range got {
			if held {
				t.Fatalf("fork durable I/O %d ran while runtime.mu was held", i+1)
			}
		}
	})

	t.Run("resume_listing_and_loading_under_lifecycle_lock", func(t *testing.T) {
		t.Setenv("LIGHTCODE_RESUME_KEY", "test-key")
		home, projectRoot := t.TempDir(), t.TempDir()
		a := newResumeRaceAgent(t, home, projectRoot)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatalf("ensure project: %v", err)
		}
		if err := a.store.AttachSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID); err != nil {
			t.Fatalf("attach sessions root: %v", err)
		}
		if err := a.store.BeginNewSession(projectRoot); err != nil {
			t.Fatalf("begin session: %v", err)
		}
		// A session is only persisted (and thus resumable) once it carries a
		// completed turn.
		raw, err := json.Marshal(message.NewText(message.RoleUser, "hello"))
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		if err := a.store.AppendMessage(1, raw); err != nil {
			t.Fatalf("append message: %v", err)
		}
		if err := a.store.MarkTurnComplete(1); err != nil {
			t.Fatalf("mark turn complete: %v", err)
		}
		if _, err := a.store.Close(); err != nil {
			t.Fatalf("close session: %v", err)
		}

		obs := probeOwnerLock(a, &a.ensureRuntime().lifecycleMu)
		defer func() { obs() }()
		if _, err := a.resumeMostRecent(); err != nil {
			t.Fatalf("resume: %v", err)
		}
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable resume I/O observed")
		}
		for i, held := range got {
			if !held {
				t.Fatalf("resume durable I/O %d ran outside lifecycleMu", i)
			}
		}
	})

	t.Run("tokens_read_outside_tokens_mutex", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		unit := a.session
		writeTokenFile(t, unit, TokenEntry{Provider: "test", Model: "test-model", Input: 11, Output: 7, Known: true})
		obs := probeOwnerLock(a, &unit.tokensMu)
		defer func() { obs() }()
		a.loadTokensFromDiskForSession(unit)
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable tokens read observed")
		}
		for i, held := range got {
			if held {
				t.Fatalf("tokens file read %d ran while tokensMu was held", i)
			}
		}
		report, err := a.TokenUsageForSession(unit.store.SessionID())
		if err != nil {
			t.Fatalf("TokenUsageForSession: %v", err)
		}
		if report.Total.Input != 11 || report.Total.Output != 7 {
			t.Fatalf("loaded token report = %#v, want 11/7", report.Total)
		}
	})

	t.Run("tokens_write_under_tokens_mutex", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		unit := a.session
		obs := probeOwnerLock(a, &unit.tokensMu)
		defer func() { obs() }()
		ref := coremodel.ModelRef{Provider: "test", Model: "test-model"}
		a.recordUsageForSession(unit, loop.Event{Model: ref.Model, ModelRef: ref, UsageKnown: true, Cache: 1, Input: 2, Output: 3})
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable tokens write observed")
		}
		for i, held := range got {
			if !held {
				t.Fatalf("tokens file write %d ran outside tokensMu", i)
			}
		}
		// The write landed inside the section and carries the recorded entry.
		data, err := os.ReadFile(filepath.Join(unit.store.Dir(), tokensFileName))
		if err != nil {
			t.Fatalf("read persisted tokens: %v", err)
		}
		var entries []TokenEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			t.Fatalf("unmarshal persisted tokens: %v", err)
		}
		if len(entries) != 1 || entries[0].Input != 2 || entries[0].Output != 3 || entries[0].Cache != 1 {
			t.Fatalf("persisted entries = %#v, want one cache 1 / input 2 / output 3 entry", entries)
		}
	})
	t.Run("new_session_all_durable_io_outside_runtime_lock", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		obs := probeOwnerLock(a, &a.ensureRuntime().mu)
		defer func() { obs() }()
		if _, err := a.NewSessionWithBoundary(proj.ID, "primary", func(_ HydrationState, _ error) {}); err != nil {
			t.Fatalf("NewSessionWithBoundary: %v", err)
		}
		// The newSession durable fires — the prompt/rules read, the prepared
		// SetModel and Meta writes/reads, and the tokens read — all run
		// outside runtime.mu; the lock is held only for the admission checks,
		// the config/model snapshot, and the final registration/boundary
		// section, none of which performs durable I/O.
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable newSession I/O observed")
		}
		for i, held := range got {
			if held {
				t.Fatalf("newSession durable I/O %d ran while runtime.mu was held", i+1)
			}
		}
	})

	// The persisted open (reactivation) path follows the same split as the
	// fork: the second short hold covers only the locked reload/config work
	// and the root-unit snapshot, every durable read — the session load,
	// metadata, prompt/rules assembly, prebuilt boundary prefix, and tokens —
	// runs with rt.mu free, and the final hold covers only registration and
	// the in-commit boundary publication. The boundary emit is the observable
	// of that final hold: it runs inside captureUnderLocksRTHeld, so probing
	// rt.mu from it reads held exactly when the caller's final hold exists.
	// Removing the final hold (the mutation this subtest gates) makes the emit
	// probe read free and fails the assertion.
	t.Run("reactivation_durable_io_outside_runtime_lock_boundary_under_it", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "kept")
		archivedID := a.SessionCurrent().ID
		if archivedID == "" {
			t.Fatal("no session to archive")
		}
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if err := a.SessionArchive(archivedID); err != nil {
			t.Fatalf("SessionArchive: %v", err)
		}

		rt := a.ensureRuntime()
		obs := probeOwnerLock(a, &rt.mu)
		defer func() { obs() }()
		var boundaryUnderLock *bool
		boundaryCount := 0
		if _, err := a.OpenSessionWithBoundary(archivedID, func(hs HydrationState) {
			boundaryCount++
			held := !rt.mu.TryLock()
			if !held {
				rt.mu.Unlock()
			}
			boundaryUnderLock = &held
			if hs.Session.ID != archivedID {
				t.Fatalf("boundary session = %q, want %q", hs.Session.ID, archivedID)
			}
			if hs.Session.State != snapshot.StateActive {
				t.Fatalf("boundary state = %q, want active", hs.Session.State)
			}
			if c := userContents(hs.Messages); !equalStrings(c, []string{"kept"}) {
				t.Fatalf("boundary messages = %q, want [kept]", c)
			}
		}); err != nil {
			t.Fatalf("OpenSessionWithBoundary: %v", err)
		}
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable reactivation I/O observed")
		}
		for i, held := range got {
			if held {
				t.Fatalf("reactivation durable I/O %d ran while runtime.mu was held", i+1)
			}
		}
		if boundaryCount != 1 {
			t.Fatalf("boundary emitted %d times, want exactly 1", boundaryCount)
		}
		if boundaryUnderLock == nil || !*boundaryUnderLock {
			t.Fatal("boundary publication ran outside runtime.mu, want the final registration/boundary hold")
		}
	})

}

// probeOwnerLock installs a durableReadHook that records, for every fire,
// whether m was held at that moment, and returns a func that restores the hook
// and reports the recorded observations in fire order.
func probeOwnerLock(a *Agent, m *sync.Mutex) func() []bool {
	var obs []bool
	a.durableReadHook = func() {
		if !m.TryLock() {
			obs = append(obs, true)
		} else {
			m.Unlock()
			obs = append(obs, false)
		}
	}
	return func() []bool {
		a.durableReadHook = nil
		return obs
	}
}

// TestPreseedRearmWakeSentAfterRuntimeUnlock is a structural regression for
// the deferred preseed claim cleanup's lock order: the rearm wake must be sent
// only after rt.mu is released, so a wake can never be delivered while the
// runtime mutex is held. A structural assertion is used because the
// nonblocking channel wake has no observable lock-ownership effect — the
// receiver merely finds a token, and the send's lock context cannot be
// observed without production instrumentation — and adding a runtime hook to
// observe the send would add the production mechanism the design forbids. The
// test extracts the deferred cleanup body from agent.go's source and requires
// the runtime-mutex unlock to precede the nudge call within it.
func TestPreseedRearmWakeSentAfterRuntimeUnlock(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}
	const marker = "// Rearm retained work on a live owner"
	start := bytes.Index(src, []byte(marker))
	if start < 0 {
		t.Fatalf("deferred preseed cleanup marker %q not found in agent.go", marker)
	}
	body := src[start:]
	unlockAt := bytes.Index(body, []byte("rt.mu.Unlock()"))
	nudgeAt := bytes.Index(body, []byte("rt.nudgeQueueDrainer()"))
	if unlockAt < 0 {
		t.Fatalf("no rt.mu.Unlock() after the preseed cleanup marker")
	}
	if nudgeAt < 0 {
		t.Fatalf("no nudgeQueueDrainer() after the preseed cleanup marker")
	}
	if unlockAt > nudgeAt {
		t.Fatalf("the preseed rearm nudge is sent while rt.mu is still held (unlock at byte %d, nudge at byte %d in the deferred cleanup body)", unlockAt, nudgeAt)
	}
	t.Run("new_session_all_durable_io_outside_runtime_lock", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		obs := probeOwnerLock(a, &a.ensureRuntime().mu)
		defer func() { obs() }()
		if _, err := a.NewSessionWithBoundary(proj.ID, "primary", func(_ HydrationState, _ error) {}); err != nil {
			t.Fatalf("NewSessionWithBoundary: %v", err)
		}
		// The newSession durable fires — the prompt/rules read, the prepared
		// SetModel and Meta writes/reads, and the tokens read — all run
		// outside runtime.mu; the lock is held only for the admission checks,
		// the config/model snapshot, and the final registration/boundary
		// section, none of which performs durable I/O.
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable newSession I/O observed")
		}
		for i, held := range got {
			if held {
				t.Fatalf("newSession durable I/O %d ran while runtime.mu was held", i+1)
			}
		}
	})

}

// TestLiveRegistrationRequiresPreRegisteredCursor proves the locked live
// registration never first-registers: the unit's transcript coordinator must
// already exist, because every first-registration path registers outside
// rt.mu. A missing cursor is refused and the locked path performs no store
// read and no insert — a first registration under rt.mu is impossible.
func TestLiveRegistrationRequiresPreRegisteredCursor(t *testing.T) {
	t.Run("missing_cursor_refused_without_store_io", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		rt := a.ensureRuntime()
		rt.unregisterTranscript(id)
		rt.mu.Lock()
		err := a.registerLiveSessionLocked(unit)
		rt.mu.Unlock()
		if err == nil || !strings.Contains(err.Error(), id) {
			t.Fatalf("registerLiveSessionLocked = %v, want a missing-cursor refusal naming %q", err, id)
		}
		// The refused locked registration must not have re-created the cursor:
		// that insert would have read the store under rt.mu.
		if tr := a.transcriptForSessionID(id); tr != nil {
			t.Fatal("refused locked registration re-created the transcript cursor")
		}
	})

	t.Run("pre_registered_cursor_accepted_and_preserved", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		rt := a.ensureRuntime()
		rt.unregisterTranscript(id)
		// The first-registration path: outside rt.mu.
		rt.registerTranscript(id, unit.store)
		tr := a.transcriptForSessionID(id)
		rt.mu.Lock()
		err := a.registerLiveSessionLocked(unit)
		rt.mu.Unlock()
		if err != nil {
			t.Fatalf("registerLiveSessionLocked with a pre-registered cursor: %v", err)
		}
		if got := a.transcriptForSessionID(id); got != tr {
			t.Fatal("locked registration replaced the pre-registered coordinator")
		}
	})
}

// TestLockedLiveRegistrationPerformsNoStoreRead is a structural regression
// for the locked registration's I/O rule: registerLiveSessionLocked and
// liveSessionLocked are both called under rt.mu, so neither may call
// registerTranscript or read the store's complete turn — the coordinator is
// only ever first-registered outside rt.mu, and the locked registration only
// verifies the pre-registered cursor. A structural assertion is used because
// the store read is unobservable from outside the coordinator; the same
// pattern the preseed-rearm lock-order regression uses.
func TestLockedLiveRegistrationPerformsNoStoreRead(t *testing.T) {
	src, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}
	text := string(src)
	for _, marker := range []string{
		"func (a *Agent) registerLiveSessionLocked",
		"func (a *Agent) liveSessionLocked",
	} {
		start := strings.Index(text, marker)
		if start < 0 {
			t.Fatalf("%s not found in agent.go", marker)
		}
		end := strings.Index(text[start:], "\nfunc ")
		if end < 0 {
			t.Fatalf("%s body end not found", marker)
		}
		body := text[start : start+end]
		if strings.Contains(body, "registerTranscript(") {
			t.Fatalf("%s calls registerTranscript under rt.mu", marker)
		}
		if strings.Contains(body, "HighestCompleteTurn(") {
			t.Fatalf("%s reads the store's complete turn under rt.mu", marker)
		}
	}
}

// TestAllProductionRegistrationsOutsideRuntimeLock is a structural regression
// for the registration placement invariant: every production
// registerTranscript call (agent.go, task.go) must sit outside any rt.mu
// locked region, because the registration reads the store's complete turn.
// The scan tracks the lexical lock depth across both lock spellings; the
// locked live-registration functions are additionally pinned by
// TestLockedLiveRegistrationPerformsNoStoreRead.
func TestAllProductionRegistrationsOutsideRuntimeLock(t *testing.T) {
	for _, file := range []string{"agent.go", "task.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		depth := 0
		for i, line := range strings.Split(string(src), "\n") {
			switch {
			case strings.Contains(line, "rt.mu.Lock()"):
				depth++
			case strings.Contains(line, "a.ensureRuntime().mu.Lock()"):
				depth++
			case strings.Contains(line, "rt.mu.Unlock()"):
				depth--
			case strings.Contains(line, "a.ensureRuntime().mu.Unlock()"):
				depth--
			}
			if depth < 0 {
				depth = 0
			}
			if strings.Contains(line, "registerTranscript(") && !strings.Contains(line, "unregister") && depth > 0 {
				t.Fatalf("%s:%d: registerTranscript call runs inside an rt.mu locked region", file, i+1)
			}
		}
	}
}

// TestReopenUsesAdmissionSnapshotModelAcrossReload proves the persisted open
// pins its model install to the admission snapshot: the open parks in the
// released section after its config snapshot (at the first durable-read seam
// fire — the adaptation-aware prompt assembly inside the unit construction,
// which runs with rt.mu free), a Reload completes a config-generation change
// against the live catalog, and the reopened private unit still uses the
// admission snapshot's persisted model/client/adaptation/prompt — the context
// window comes from the snapshotted catalog, never the reloaded one — and
// registers successfully. The window and the client are built from the same
// catalog model record, so the window check pins the client's catalog
// generation too.
func TestReopenUsesAdmissionSnapshotModelAcrossReload(t *testing.T) {
	t.Setenv("LIGHTCODE_LIFECYCLE_KEY", "test-key")
	home := t.TempDir()
	projectRoot := t.TempDir()
	configPath := writeLifecycleConfig(t, home, "test/test-model", true, "http://127.0.0.1:9/v1")

	// Seed an archived session whose persisted meta names provider "test",
	// model "alt-model", with one complete turn.
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	seed, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new seed agent: %v", err)
	}
	proj, err := seed.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	sessionsRoot := seed.projects.SessionsRoot(proj.ID)
	if err := seed.store.AttachSessionsRoot(sessionsRoot, seed.projects.Root(), proj.ID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := seed.store.BeginNewSession(projectRoot); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := seed.store.SetModel("test", "alt-model"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	raw, err := json.Marshal(message.NewText(message.RoleUser, "kept"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := seed.store.AppendMessage(1, raw); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := seed.store.MarkTurnComplete(1); err != nil {
		t.Fatalf("mark turn complete: %v", err)
	}
	id := seed.store.SessionID()
	if _, err := seed.store.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	if err := snapshot.ArchiveSession(sessionsRoot, id); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Reopen agent built on the admission config generation.
	cfg, err = config.Load(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new reopen agent: %v", err)
	}

	// Park the open in the released section at its first rt.mu-free
	// durable-read seam fire (the adaptation-aware prompt assembly inside the
	// unit construction, after the admission config snapshot), then complete
	// a config-generation change and Reload. The open's locked reload can
	// also re-assemble a prompt under rt.mu; parking there would deadlock the
	// Reload below, so only a fire with rt.mu free parks.
	rt := a.ensureRuntime()
	var parked atomic.Bool
	parkedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	a.durableReadHook = func() {
		held := !rt.mu.TryLock()
		if !held {
			rt.mu.Unlock()
		}
		if !held && parked.CompareAndSwap(false, true) {
			close(parkedCh)
			<-releaseCh
		}
	}
	defer func() { a.durableReadHook = nil }()

	var boundary []HydrationState
	done := make(chan struct{})
	var openErr error
	go func() {
		defer close(done)
		_, openErr = a.OpenSessionWithBoundary(id, func(hs HydrationState) {
			boundary = append(boundary, hs)
		})
	}()

	<-parkedCh
	// The new generation changes the provider base URL and alt-model's
	// context window (4096 -> 2048); the reopened unit must keep the
	// admission generation's model record.
	body := `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:8/v1", "api_key_env": "LIGHTCODE_LIFECYCLE_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192 },
        "alt-model": { "name": "Alt Model", "context_window": 2048 }
      }
    }
  },
  "default_model": "test/test-model"
}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	close(releaseCh)
	<-done

	if openErr != nil {
		t.Fatalf("OpenSessionWithBoundary: %v", openErr)
	}
	rt.mu.Lock()
	unit := a.sessions[id]
	rt.mu.Unlock()
	if unit == nil {
		t.Fatal("reopened session not live")
	}
	wantRef := coremodel.ModelRef{Provider: "test", Model: "alt-model"}
	if unit.currentRef != wantRef {
		t.Fatalf("currentRef = %v, want %v (the persisted model)", unit.currentRef, wantRef)
	}
	// The window comes from the admission snapshot's catalog (4096), never
	// the reloaded generation (2048): the install used snap.modelCatalog, so
	// the client is from the same snapshot generation.
	if unit.contextWindowSize != 4096 {
		t.Fatalf("contextWindowSize = %d, want 4096 (the admission snapshot's alt-model; the reloaded catalog says 2048)", unit.contextWindowSize)
	}
	if unit.lp.ActiveAdaptation() != unit.activeAdapt {
		t.Fatal("loop adaptation != unit adaptation (the single construction pass was not adaptation-aware)")
	}
	loopPrompt := unit.lp.Messages()[0].TextContent()
	if unit.installedPrompt != loopPrompt {
		t.Fatal("installedPrompt != loop prompt (the single assembly was not installed)")
	}
	if a.transcriptForSessionID(id) == nil {
		t.Fatal("reopened session registered without a transcript coordinator")
	}
	if len(boundary) != 1 {
		t.Fatalf("boundary emitted %d times, want exactly 1", len(boundary))
	}
	if boundary[0].Session.ID != id {
		t.Fatalf("boundary session = %q, want %q", boundary[0].Session.ID, id)
	}
	if boundary[0].Session.State != snapshot.StateActive {
		t.Fatalf("boundary state = %q, want active", boundary[0].Session.State)
	}
	if c := userContents(boundary[0].Messages); !equalStrings(c, []string{"kept"}) {
		t.Fatalf("boundary messages = %q, want [kept]", c)
	}
}
