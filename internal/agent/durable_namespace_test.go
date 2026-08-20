package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

type stagedPublicationEvent struct {
	kind string
	path string
}

func stagedSessionPath(path string) bool {
	return strings.Contains(filepath.Clean(path), string(filepath.Separator)+".staging"+string(filepath.Separator)+"sessions"+string(filepath.Separator))
}

func stagingSessionsNonce(path string) bool {
	return filepath.Base(filepath.Dir(path)) == "sessions" && filepath.Base(filepath.Dir(filepath.Dir(path))) == ".staging"
}

func candidateTreeSyncOrder(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read candidate tree %q: %v", root, err)
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, candidateTreeSyncOrder(t, filepath.Join(root, entry.Name()))...)
		}
	}
	return append(out, root)
}

func assertCandidatePublicationOrder(t *testing.T, events []stagedPublicationEvent, candidateDir, finalRoot string, wantTree []string) {
	t.Helper()
	if candidateDir == "" {
		t.Fatal("candidate directory was not observed through the real producer")
	}
	var metaWrites []int
	for i, event := range events {
		if event.kind == "meta" {
			metaWrites = append(metaWrites, i)
		}
	}
	if len(metaWrites) != 2 {
		t.Fatalf("candidate meta writes = %d, want initial metadata plus final SetModel", len(metaWrites))
	}
	stagingRoot := filepath.Dir(candidateDir)
	treeStart := -1
	for i := range events {
		if events[i].kind != "dir" || events[i].path != wantTree[0] || i+len(wantTree) > len(events) {
			continue
		}
		match := true
		for j, want := range wantTree {
			if events[i+j].kind != "dir" || events[i+j].path != want {
				match = false
				break
			}
		}
		if match {
			treeStart = i
			break
		}
	}
	if treeStart < 0 {
		t.Fatalf("candidate tree was not synced in leaf-before-parent order; events=%v want=%v", events, wantTree)
	}
	for _, index := range metaWrites {
		if index >= treeStart {
			t.Fatalf("candidate metadata write at event %d followed candidate-tree sync at %d; events=%v", index, treeStart, events)
		}
	}
	afterTree := treeStart + len(wantTree)
	wantTail := []string{stagingRoot, finalRoot}
	if afterTree+len(wantTail) > len(events) {
		t.Fatalf("publication ended before parent syncs; events=%v", events)
	}
	for i, want := range wantTail {
		got := events[afterTree+i]
		if got.kind != "dir" || got.path != want {
			t.Fatalf("publication parent sync %d = %+v, want %q; events=%v", i, got, want, events)
		}
	}
}

func TestArchiveCommittedSyncErrorTearsDownLiveSession(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatal(err)
	}
	id := a.SessionCurrent().ID
	root := a.store.Root()
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == filepath.Join(root, id) {
			return errors.New("injected archive sync failure")
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	err := a.SessionArchive(id)
	var committed *snapshot.CommittedMutationError
	if !errors.As(err, &committed) {
		t.Fatalf("archive error = %v, want committed error", err)
	}
	if a.store.Active() {
		t.Fatal("archived current store remained active after committed error")
	}
	if a.currentSessionID != "" {
		t.Fatalf("current session after committed archive error = %q, want empty", a.currentSessionID)
	}
	if _, err := os.Stat(filepath.Join(root, id, "meta.json")); err != nil {
		t.Fatalf("archived metadata missing: %v", err)
	}
}

func TestNewSessionAdoptsDestinationOnCommittedPublicationError(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	proj, err := a.projects.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	root := a.projects.SessionsRoot(proj.ID)
	var emitted bool
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == root {
			return errors.New("injected publication parent sync failure")
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	id, err := a.NewSessionWithBoundary(proj.ID, "primary", func(_ HydrationState, cbErr error) {
		emitted = true
		var committed *snapshot.CommittedMutationError
		if !errors.As(cbErr, &committed) {
			t.Errorf("boundary error = %v, want committed error", cbErr)
		}
	})
	var committed *snapshot.CommittedMutationError
	if !errors.As(err, &committed) {
		t.Fatalf("new session error = %v, want committed error", err)
	}
	if id == "" || !emitted {
		t.Fatalf("new session committed result = id %q emitted %v, want destination and boundary", id, emitted)
	}
	if a.currentSessionID != id || !a.store.Active() {
		t.Fatalf("adopted session = %q active=%v, want %q/true", a.currentSessionID, a.store.Active(), id)
	}
}

func TestForkAdoptsDestinationOnCommittedPublicationError(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, "fork me")
	project, err := a.projects.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	sourceID := a.SessionCurrent().ID
	sessionsRoot := a.projects.SessionsRoot(project.ID)
	var emitted bool
	var committedBoundary bool
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == sessionsRoot {
			return errors.New("injected fork publication parent sync failure")
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	result, err := a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, false, func(state HydrationState, _ []snapshot.SkippedRevert, _ string, callbackErr *snapshot.CommittedMutationError, _ *string) {
		emitted = true
		committedBoundary = callbackErr != nil && state.Session.ID != "" && state.Session.ID != sourceID
	})
	var committed *snapshot.CommittedMutationError
	if !errors.As(err, &committed) {
		t.Fatalf("fork error = %v, want committed error", err)
	}
	if result.Session.ID == "" || result.Session.ID == sourceID || !emitted || !committedBoundary {
		t.Fatalf("fork committed result = session %q emitted=%v boundary=%v, want adopted destination", result.Session.ID, emitted, committedBoundary)
	}
	if a.currentSessionID != result.Session.ID {
		t.Fatalf("current session after committed fork = %q, want %q", a.currentSessionID, result.Session.ID)
	}
	if _, err := os.Stat(filepath.Join(sessionsRoot, result.Session.ID, "meta.json")); err != nil {
		t.Fatalf("published fork metadata missing: %v", err)
	}
}

func TestForkCommittedPublicationAlsoRevertsSourceCode(t *testing.T) {
	t.Run("preserves_restored_and_skipped_results", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		restoredPath := filepath.Join(a.projectRoot, "restored.txt")
		skippedPath := filepath.Join(a.projectRoot, "skipped.txt")
		if err := os.WriteFile(restoredPath, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(skippedPath, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		appendUserTurn(t, a, "fork source")
		appendUserTurnWithSnapshot(t, a, "restore later", restoredPath, "after")
		appendUserTurnWithSnapshot(t, a, "skip later", skippedPath, "after")
		if err := os.WriteFile(skippedPath, []byte("external"), 0o600); err != nil {
			t.Fatal(err)
		}
		project, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(project.ID)
		parentSyncErr := errors.New("injected committed fork parent sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == sessionsRoot {
				return parentSyncErr
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

		var boundaryState HydrationState
		var boundarySkipped []snapshot.SkippedRevert
		var boundaryWarning string
		var boundaryErr *snapshot.CommittedMutationError
		result, err := a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, true, func(state HydrationState, skipped []snapshot.SkippedRevert, warning string, committed *snapshot.CommittedMutationError, _ *string) {
			boundaryState = state
			boundarySkipped = skipped
			boundaryWarning = warning
			boundaryErr = committed
		})
		var committed *snapshot.CommittedMutationError
		if !errors.As(err, &committed) || !errors.Is(err, parentSyncErr) {
			t.Fatalf("fork error = %v, want the committed parent-sync error", err)
		}
		if result.Session.ID == "" || result.Session.ID == sourceID {
			t.Fatalf("fork result session = %q, want the adopted destination", result.Session.ID)
		}
		if len(result.RestoredFiles) != 1 || result.RestoredFiles[0] != restoredPath {
			t.Fatalf("restored files = %v, want [%s]", result.RestoredFiles, restoredPath)
		}
		if len(result.SkippedFiles) != 1 || result.SkippedFiles[0].Path != skippedPath {
			t.Fatalf("skipped files = %+v, want [%s]", result.SkippedFiles, skippedPath)
		}
		if boundaryState.Session.ID != result.Session.ID || boundaryErr == nil {
			t.Fatalf("boundary session/error = %q/%v, want destination and typed error", boundaryState.Session.ID, boundaryErr)
		}
		if boundaryWarning != "" || len(boundarySkipped) != 1 || boundarySkipped[0].Path != skippedPath {
			t.Fatalf("boundary code outcome = skipped:%+v warning:%q, want the successful source result", boundarySkipped, boundaryWarning)
		}
		if got, err := os.ReadFile(restoredPath); err != nil || string(got) != "before" {
			t.Fatalf("source restored file = %q, %v; want before", got, err)
		}
		if got, err := os.ReadFile(skippedPath); err != nil || string(got) != "external" {
			t.Fatalf("source skipped file = %q, %v; want external", got, err)
		}
	})

	t.Run("source_revert_failure_is_warning_only", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		path := filepath.Join(a.projectRoot, "broken-restore.txt")
		if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		appendUserTurn(t, a, "fork source")
		appendUserTurnWithSnapshot(t, a, "break later", path, "after")
		entries, err := os.ReadDir(filepath.Join(a.store.Dir(), "snapshots", "2"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("snapshot entries = %v, %v; want one entry", entries, err)
		}
		if err := os.Remove(filepath.Join(a.store.Dir(), "snapshots", "2", entries[0].Name(), "original")); err != nil {
			t.Fatal(err)
		}
		project, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sourceID := a.SessionCurrent().ID
		sessionsRoot := a.projects.SessionsRoot(project.ID)
		parentSyncErr := errors.New("injected committed fork parent sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == sessionsRoot {
				return parentSyncErr
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

		var boundaryWarning string
		var boundaryErr *snapshot.CommittedMutationError
		result, err := a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, true, func(_ HydrationState, _ []snapshot.SkippedRevert, warning string, committed *snapshot.CommittedMutationError, _ *string) {
			boundaryWarning = warning
			boundaryErr = committed
		})
		var committed *snapshot.CommittedMutationError
		if !errors.As(err, &committed) || !errors.Is(err, parentSyncErr) {
			t.Fatalf("fork error = %v, want only the committed parent-sync error", err)
		}
		if strings.Contains(err.Error(), "code revert failed") {
			t.Fatalf("fork error = %v, source-revert warning replaced or joined the committed error", err)
		}
		if result.Warning == "" || !strings.Contains(result.Warning, "code revert failed") {
			t.Fatalf("result warning = %q, want the source-revert failure", result.Warning)
		}
		if boundaryErr == nil || boundaryWarning != result.Warning {
			t.Fatalf("boundary warning/error = %q/%v, want prepared warning and committed error", boundaryWarning, boundaryErr)
		}
	})
}

func TestRealStagedNewAndForkSyncOrdering(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		var candidateDir string
		var wantTree []string
		installNewSessionCandidateHook(a, a.projects.SessionsRoot(proj.ID), 1, func(dir, _ string) {
			candidateDir = dir
			nested := filepath.Join(dir, "snapshots", "1", "entry")
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(filepath.Join(nested, "original"), []byte("candidate"), 0o600); err != nil {
				t.Error(err)
				return
			}
			wantTree = candidateTreeSyncOrder(t, dir)
		})
		var events []stagedPublicationEvent
		atomicfs.SyncFileFunc = func(file *os.File) error {
			if stagedSessionPath(file.Name()) && strings.HasPrefix(filepath.Base(file.Name()), "meta.json.tmp-") {
				candidateDir = filepath.Dir(file.Name())
				events = append(events, stagedPublicationEvent{kind: "meta", path: candidateDir})
			}
			return nil
		}
		atomicfs.SyncDirFunc = func(dir string) error {
			events = append(events, stagedPublicationEvent{kind: "dir", path: dir})
			return nil
		}
		t.Cleanup(func() {
			atomicfs.SyncFileFunc = nil
			atomicfs.SyncDirFunc = nil
		})

		id, err := a.NewSession(proj.ID, "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if id == "" {
			t.Fatal("NewSession returned an empty id")
		}
		assertCandidatePublicationOrder(t, events, candidateDir, a.projects.SessionsRoot(proj.ID), wantTree)
	})

	t.Run("fork", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		path := filepath.Join(a.projectRoot, "fork-source.txt")
		appendUserTurnWithSnapshot(t, a, "fork source", path, "source")
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		var candidateDir string
		var wantTree []string
		installForkCandidateHook(a, sessionsRoot, 1, func(dir, _ string) {
			candidateDir = dir
			wantTree = candidateTreeSyncOrder(t, dir)
		})
		var events []stagedPublicationEvent
		atomicfs.SyncFileFunc = func(file *os.File) error {
			if stagedSessionPath(file.Name()) && strings.HasPrefix(filepath.Base(file.Name()), "meta.json.tmp-") {
				candidateDir = filepath.Dir(file.Name())
				events = append(events, stagedPublicationEvent{kind: "meta", path: candidateDir})
			}
			return nil
		}
		atomicfs.SyncDirFunc = func(dir string) error {
			events = append(events, stagedPublicationEvent{kind: "dir", path: dir})
			return nil
		}
		t.Cleanup(func() {
			atomicfs.SyncFileFunc = nil
			atomicfs.SyncDirFunc = nil
		})

		result, err := a.ApplyTurnActionForSession(a.SessionCurrent().ID, 1, TurnActionFork, false)
		if err != nil {
			t.Fatalf("fork: %v", err)
		}
		if result.Session.ID == "" {
			t.Fatal("fork returned an empty destination id")
		}
		assertCandidatePublicationOrder(t, events, candidateDir, sessionsRoot, wantTree)
	})
}

func TestRealStagedCandidateTreePrecommitFailureCleansUp(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		var candidateID, failDir string
		var emitted bool
		installNewSessionCandidateHook(a, sessionsRoot, 1, func(dir, id string) {
			candidateID = id
			failDir = filepath.Join(dir, "snapshots", "1", "entry")
			if err := os.MkdirAll(failDir, 0o700); err != nil {
				t.Error(err)
			}
		})
		injected := errors.New("injected staged new candidate-tree sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == failDir {
				return injected
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

		_, err = a.NewSessionWithBoundary(proj.ID, "primary", func(_ HydrationState, _ error) { emitted = true })
		if !errors.Is(err, injected) {
			t.Fatalf("new precommit error = %v, want injected candidate-tree error", err)
		}
		if errors.As(err, new(*snapshot.CommittedMutationError)) {
			t.Fatalf("new precommit error = %v, want no committed classification", err)
		}
		assertNewSessionFailureState(t, a, proj.ID, proj.Path, sessionsRoot, stagingParent, candidateID, &emitted)
	})

	t.Run("fork", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		path := filepath.Join(a.projectRoot, "fork-source.txt")
		appendUserTurnWithSnapshot(t, a, "fork source", path, "source")
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		stagingParent := filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")
		var candidateID, failDir string
		var emitted bool
		installForkCandidateHook(a, sessionsRoot, 1, func(dir, id string) {
			candidateID = id
			failDir = filepath.Join(dir, "snapshots", "1")
		})
		injected := errors.New("injected staged fork candidate-tree sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if dir == failDir {
				return injected
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

		sourceID := a.SessionCurrent().ID
		_, err = a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, false, func(HydrationState, []snapshot.SkippedRevert, string, *snapshot.CommittedMutationError, *string) {
			emitted = true
		})
		if !errors.Is(err, injected) {
			t.Fatalf("fork precommit error = %v, want injected candidate-tree error", err)
		}
		if errors.As(err, new(*snapshot.CommittedMutationError)) {
			t.Fatalf("fork precommit error = %v, want no committed classification", err)
		}
		assertForkStagedFailureInvariants(t, a, sourceID, proj.ID, &emitted, stagingParent)
		claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, candidateID)
		if err != nil || !ok {
			t.Fatalf("candidate claim after failed fork = ok:%v err:%v, want released", ok, err)
		}
		_ = claim.Release()
	})
}

func TestRealStagedPublicationRetainsDestinationOnStagingParentSyncFailure(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		var failed bool
		injected := errors.New("injected staged new source-parent sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if stagingSessionsNonce(dir) {
				failed = true
				return injected
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
		var emitted bool
		id, err := a.NewSessionWithBoundary(proj.ID, "primary", func(state HydrationState, callbackErr error) {
			emitted = true
			if state.Session.ID == "" || !errors.As(callbackErr, new(*snapshot.CommittedMutationError)) {
				t.Errorf("new committed boundary = session:%q error:%v, want destination and typed error", state.Session.ID, callbackErr)
			}
		})
		if !failed {
			t.Fatal("staging source-parent sync failure was not exercised")
		}
		var committed *snapshot.CommittedMutationError
		if !errors.As(err, &committed) {
			t.Fatalf("new source-parent sync error = %v, want committed error", err)
		}
		if id == "" || !emitted || a.currentSessionID != id || !a.store.Active() {
			t.Fatalf("new committed result = id:%q emitted:%v current:%q active:%v, want retained destination", id, emitted, a.currentSessionID, a.store.Active())
		}
		if _, err := os.Stat(filepath.Join(a.projects.SessionsRoot(proj.ID), id, "meta.json")); err != nil {
			t.Fatalf("published new metadata: %v", err)
		}
		claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			_ = claim.Release()
			t.Fatal("new committed destination claim was released")
		}
	})

	t.Run("fork", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		path := filepath.Join(a.projectRoot, "fork-source.txt")
		appendUserTurnWithSnapshot(t, a, "fork source", path, "source")
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		var failed bool
		injected := errors.New("injected staged fork source-parent sync failure")
		atomicfs.SyncDirFunc = func(dir string) error {
			if stagingSessionsNonce(dir) {
				failed = true
				return injected
			}
			return nil
		}
		t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
		sourceID := a.SessionCurrent().ID
		var emitted bool
		var boundaryID string
		result, err := a.ApplyTurnActionForSessionWithBoundary(sourceID, 1, TurnActionFork, false, func(state HydrationState, _ []snapshot.SkippedRevert, _ string, callbackErr *snapshot.CommittedMutationError, _ *string) {
			emitted = true
			boundaryID = state.Session.ID
			if callbackErr == nil {
				t.Error("fork committed boundary omitted the typed error")
			}
		})
		if !failed {
			t.Fatal("staging source-parent sync failure was not exercised")
		}
		var committed *snapshot.CommittedMutationError
		if !errors.As(err, &committed) {
			t.Fatalf("fork source-parent sync error = %v, want committed error", err)
		}
		if result.Session.ID == "" || result.Session.ID == sourceID || !emitted || boundaryID != result.Session.ID || a.currentSessionID != result.Session.ID {
			t.Fatalf("fork committed result = session:%q boundary:%q source:%q emitted:%v current:%q, want retained destination", result.Session.ID, boundaryID, sourceID, emitted, a.currentSessionID)
		}
		if _, err := os.Stat(filepath.Join(sessionsRoot, result.Session.ID, "meta.json")); err != nil {
			t.Fatalf("published fork metadata: %v", err)
		}
		claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, result.Session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			_ = claim.Release()
			t.Fatal("fork committed destination claim was released")
		}
	})
}

func TestCommittedNonCurrentRemovalReleasesTransitionReservation(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	targetID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatal(err)
	}

	err = a.removeSession(targetID, func(string, string) error {
		rt := a.ensureRuntime()
		rt.mu.Lock()
		a.sessions[targetID].busy = true
		rt.mu.Unlock()
		return &snapshot.CommittedMutationError{Err: errors.New("injected committed removal failure")}
	})
	var committed *snapshot.CommittedMutationError
	if !errors.As(err, &committed) {
		t.Fatalf("committed removal error = %v, want typed error", err)
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit := a.sessions[targetID]
	transitioning := unit != nil && unit.transitioning
	if unit != nil {
		unit.busy = false
	}
	rt.mu.Unlock()
	if transitioning {
		t.Fatal("non-current committed removal retained the transition reservation")
	}
}
