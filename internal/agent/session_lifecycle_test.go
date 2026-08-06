package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// TestSessionLifecycleTransactionContract asserts that a failed session
// creation leaves nothing behind: when newSession fails, the sessions root
// holds no session directory, no session is registered or selected, and no
// staging tree survives.
//
// It does not discriminate the ordering of the fallible steps relative to the
// durable commit, and would pass against an implementation that persists the
// model and reads the session meta after publishing. A failure cannot be
// injected between the prepare and publish calls, because the staged
// candidate is created inside newSession under a staging directory whose name
// is not known to the caller, and reaching it would require production
// surface that exists only for testing.
func TestSessionLifecycleTransactionContract(t *testing.T) {
	t.Run("shape=A/case=new_staging_partial_failure", func(t *testing.T) {
		// The failure is injected at the durable commit, not at the pre-commit
		// fallible steps (persisting the model, reading the session meta):
		// those steps run against a staging directory that
		// PrepareStagedNewSession creates fresh inside the call with random
		// nonce and session ids, so no filesystem state arranged beforehand
		// can fail exactly those steps through the exported surface — any
		// state that breaks them breaks preparation first, and the agent
		// exposes no injection seam for them. The deepest fallible step that
		// is externally determinable is the commit itself: make the sessions
		// root unwritable so the atomic rename cannot happen. The assertions
		// therefore cover only what that failure leaves behind — nothing in
		// the sessions root, nothing registered or selected, and no staging
		// tree.
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		home := t.TempDir()
		projectRoot := t.TempDir()
		a := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		if err := os.Chmod(sessionsRoot, 0o500); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(sessionsRoot, 0o700) }()

		emitCalled := false
		if _, err := a.NewSessionWithBoundary(proj.ID, "primary", func(HydrationState) { emitCalled = true }); err == nil {
			t.Fatal("NewSessionWithBoundary should fail when the sessions root is unwritable")
		}

		// Nothing published: the sessions root holds no session directory, and
		// listing sees no partially created session.
		entries, err := os.ReadDir(sessionsRoot)
		if err != nil {
			t.Fatalf("read sessions root: %v", err)
		}
		for _, e := range entries {
			if e.IsDir() {
				t.Fatalf("session directory %q left in sessions root after failed creation", e.Name())
			}
		}
		list, err := snapshot.List(sessionsRoot, proj.Path, snapshot.StateActive)
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("failed creation left listed sessions: %#v", list)
		}

		// The staged candidate is gone too, and the boundary capture — the
		// in-commit publish step — never ran.
		if staging, _ := os.ReadDir(filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions")); len(staging) != 0 {
			t.Fatalf("staging left uncleaned: %v", staging)
		}
		if emitCalled {
			t.Fatal("boundary emit ran for a session that was never published")
		}

		// Nothing registered or selected in memory.
		a.ensureRuntime().mu.Lock()
		registered := len(a.sessions)
		current := a.currentSessionID
		a.ensureRuntime().mu.Unlock()
		if registered != 0 {
			t.Fatalf("failed creation registered %d live sessions", registered)
		}
		if current != "" {
			t.Fatalf("failed creation selected session %q", current)
		}
		if a.SessionCurrent().ID != "" {
			t.Fatalf("SessionCurrent = %q, want none", a.SessionCurrent().ID)
		}

		// The failed transaction left the namespace reusable: once the root is
		// writable again, a fresh creation succeeds and is selected, and the
		// published session is correctly claimed — a second claim on it is
		// refused.
		if err := os.Chmod(sessionsRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		id, err := a.NewSession(proj.ID, "primary")
		if err != nil {
			t.Fatalf("creation after failed transaction: %v", err)
		}
		if a.SessionCurrent().ID != id {
			t.Fatalf("SessionCurrent = %q, want the new session %q", a.SessionCurrent().ID, id)
		}
		claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, id)
		if err != nil {
			t.Fatalf("claim check: %v", err)
		}
		if ok {
			_ = claim.Release()
			t.Fatal("published session is not claimed by the live unit")
		}
	})

	// shape=B is the removal shape: reserve (transitioning, claim held), durable
	// commit, then release. A durable mutation failing midway must leave the
	// unit live, claimed, selected and queued — for archive and delete, and for
	// the current and non-current cases. The failure is injected at the durable
	// commit through the exported surface: ArchiveSession writes the new
	// meta.json atomically inside the session directory, so an unwritable
	// session dir fails exactly the write; DeleteSession's commit is the atomic
	// rename out of the sessions root, so an unwritable sessions root fails
	// exactly the rename. There is no injection seam below the exported
	// surface, and the queue is volatile runtime state with no exported
	// mutation surface (Submit consumes it immediately when idle), so it is
	// seeded directly as the queue tests do.
	t.Run("shape=B/case=current_remove_no_session_no_fallback_no_postcapture", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		home := t.TempDir()
		projectRoot := t.TempDir()
		a := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		id, err := a.NewSession(proj.ID, "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		seedUnitQueue(t, a, id, "queued draft")

		// Archive fails at the durable commit when the session dir is
		// unwritable (the atomic write of meta.json happens inside it).
		if err := os.Chmod(filepath.Join(sessionsRoot, id), 0o500); err != nil {
			t.Fatal(err)
		}
		if err := a.SessionArchive(id); err == nil {
			t.Fatal("SessionArchive unexpectedly succeeded with an unwritable session dir")
		}
		if err := os.Chmod(filepath.Join(sessionsRoot, id), 0o700); err != nil {
			t.Fatal(err)
		}
		assertCurrentRemovalFailureIntact(t, a, id, proj)
		assertSessionListedActive(t, sessionsRoot, proj.Path, id)

		// Delete fails at the durable commit when the sessions root is
		// unwritable (the atomic rename out of it fails).
		if err := os.Chmod(sessionsRoot, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := a.SessionDelete(id); err == nil {
			t.Fatal("SessionDelete unexpectedly succeeded with an unwritable sessions root")
		}
		if err := os.Chmod(sessionsRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		assertCurrentRemovalFailureIntact(t, a, id, proj)
		assertSessionListedActive(t, sessionsRoot, proj.Path, id)

		// With the durable commit unblocked, removing the current session
		// publishes the deterministic no-session state: nothing selected, no
		// fallback to another session, and no complete-state capture after the
		// commit (the removal path has no capture seam — the probe never
		// fires).
		a.captureProbe = func(int) error {
			t.Fatal("removal performed a complete-state capture after its durable commit")
			return nil
		}
		if err := a.SessionDelete(id); err != nil {
			a.captureProbe = nil
			t.Fatalf("SessionDelete after restoring permissions: %v", err)
		}
		a.captureProbe = nil
		if current := a.SessionCurrent().ID; current != "" {
			t.Fatalf("SessionCurrent after removing current session = %q, want no session", current)
		}
		a.ensureRuntime().mu.Lock()
		current := a.currentSessionID
		registered := len(a.sessions)
		a.ensureRuntime().mu.Unlock()
		if current != "" {
			t.Fatalf("currentSessionID after removing current session = %q, want empty", current)
		}
		if registered != 0 {
			t.Fatalf("removed session still registered: %d live sessions", registered)
		}
		if list, err := snapshot.List(sessionsRoot, proj.Path, snapshot.StateActive); err != nil {
			t.Fatalf("list sessions after removal: %v", err)
		} else if len(list) != 0 {
			t.Fatalf("removed session still listed as active: %#v", list)
		}
		// The removal released the claim: a fresh acquisition succeeds.
		claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, id)
		if err != nil {
			t.Fatalf("claim check: %v", err)
		}
		if !ok {
			t.Fatal("claim still held after the session was removed")
		}
		_ = claim.Release()
	})

	t.Run("shape=B/case=noncurrent_remove_unchanged", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		home := t.TempDir()
		projectRoot := t.TempDir()
		a := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		firstID, err := a.NewSession(proj.ID, "primary")
		if err != nil {
			t.Fatalf("NewSession first: %v", err)
		}
		secondID, err := a.NewSession(proj.ID, "primary")
		if err != nil {
			t.Fatalf("NewSession second: %v", err)
		}
		if current := a.SessionCurrent().ID; current != secondID {
			t.Fatalf("current session = %q, want %q", current, secondID)
		}
		seedUnitQueue(t, a, firstID, "queued draft")

		// Archive fails at the durable commit when the target session dir is
		// unwritable.
		if err := os.Chmod(filepath.Join(sessionsRoot, firstID), 0o500); err != nil {
			t.Fatal(err)
		}
		if err := a.SessionArchive(firstID); err == nil {
			t.Fatal("SessionArchive unexpectedly succeeded with an unwritable session dir")
		}
		if err := os.Chmod(filepath.Join(sessionsRoot, firstID), 0o700); err != nil {
			t.Fatal(err)
		}
		assertNonCurrentRemovalFailureIntact(t, a, firstID, secondID, proj)
		assertSessionListedActive(t, sessionsRoot, proj.Path, firstID)

		// Delete fails at the durable commit when the sessions root is
		// unwritable.
		if err := os.Chmod(sessionsRoot, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := a.SessionDelete(firstID); err == nil {
			t.Fatal("SessionDelete unexpectedly succeeded with an unwritable sessions root")
		}
		if err := os.Chmod(sessionsRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		assertNonCurrentRemovalFailureIntact(t, a, firstID, secondID, proj)
		assertSessionListedActive(t, sessionsRoot, proj.Path, firstID)

		// A successful removal of the non-current session leaves the current
		// selection unchanged.
		if err := a.SessionArchive(firstID); err != nil {
			t.Fatalf("SessionArchive after restoring permissions: %v", err)
		}
		if current := a.SessionCurrent().ID; current != secondID {
			t.Fatalf("current session after removing non-current session = %q, want %q", current, secondID)
		}
		a.ensureRuntime().mu.Lock()
		_, stillLive := a.sessions[firstID]
		a.ensureRuntime().mu.Unlock()
		if stillLive {
			t.Fatal("removed non-current session still registered")
		}
		// The removal released the target's claim: a fresh acquisition succeeds.
		claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, firstID)
		if err != nil {
			t.Fatalf("claim check: %v", err)
		}
		if !ok {
			t.Fatal("claim still held after the session was removed")
		}
		_ = claim.Release()
		if list, err := snapshot.List(sessionsRoot, proj.Path, snapshot.StateArchived); err != nil {
			t.Fatalf("list archived sessions: %v", err)
		} else if len(list) != 1 || list[0].ID != firstID {
			t.Fatalf("archived sessions = %#v, want only %q", list, firstID)
		}
	})

	// shape=C is the revert shape: reserve (transitioning), durable mutation,
	// release. A revert failing midway must leave the unit live and consistent
	// afterwards — live, claimed, selected, its loop reloaded to match the
	// truncated history, and the transitioning reservation released so the unit
	// is driveable again. The failure is injected at a specific turn directory
	// through filesystem permissions: RevertHistory removes turn dirs strictly
	// above the target, so an unwritable turn dir fails exactly its removal,
	// midway through the walk. The subtests assert only that post-failure
	// state: whether the reservation is held across the durable call is not
	// observable without production surface that exists only for testing.
	t.Run("shape=C/case=current_revert_failure_releases_reservation", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		home := t.TempDir()
		projectRoot := t.TempDir()
		a := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		id, err := a.NewSession(proj.ID, "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		loopContents := []string{"turn one", "turn two", "turn three", "turn four"}
		for _, content := range loopContents {
			if _, err := a.AppendUserMessageToSession(id, content); err != nil {
				t.Fatalf("append user turn: %v", err)
			}
		}
		// Revert to before turn 3 (target 2) removes turns 4 and 3; block the
		// removal of turn 3 so the walk removes turn 4, fails at turn 3, and stops.
		blockedTurn := filepath.Join(sessionsRoot, id, "turns", "3")
		if err := os.Chmod(blockedTurn, 0o555); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(blockedTurn, 0o700) }()

		// The reservation must be released again after the failure, but whether
		// it was held across the durable call is not observable here.
		result, err := a.ApplyTurnActionForSession(id, 3, TurnActionRevertHistory, false)
		if err == nil {
			t.Fatal("revert_history succeeded despite the blocked removal; the failure was swallowed")
		}
		if !strings.Contains(err.Error(), "revert history turn 3") {
			t.Fatalf("revert error = %q, want it to name turn 3 where the walk stopped", err.Error())
		}
		// The populated result reports what happened alongside the error.
		if result.Action != TurnActionRevertHistory || result.Turn != 3 || result.TargetTurn != 2 || !result.SessionChanged {
			t.Fatalf("populated result = %#v, want revert_history to turn 2 with session change", result)
		}
		if result.Session.ID != id {
			t.Fatalf("result session = %q, want %q", result.Session.ID, id)
		}
		if got := userContents(result.Messages); !equalStrings(got, loopContents[:3]) {
			t.Fatalf("result messages = %q, want the surviving turns 1..3 %q", got, loopContents[:3])
		}
		// Afterwards the unit is live, claimed, selected, consistent and the
		// reservation is released.
		assertCurrentRevertFailureIntact(t, a, id, proj, loopContents)
	})

	t.Run("shape=C/case=noncurrent_revert_failure_releases_reservation", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not block writes as root")
		}
		home := t.TempDir()
		projectRoot := t.TempDir()
		a := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		sessionsRoot := a.projects.SessionsRoot(proj.ID)
		targetID, err := a.NewSession(proj.ID, "primary")
		if err != nil {
			t.Fatalf("NewSession target: %v", err)
		}
		secondID, err := a.NewSession(proj.ID, "primary")
		if err != nil {
			t.Fatalf("NewSession second: %v", err)
		}
		if current := a.SessionCurrent().ID; current != secondID {
			t.Fatalf("current session = %q, want %q", current, secondID)
		}
		loopContents := []string{"turn one", "turn two", "turn three", "turn four"}
		for _, content := range loopContents {
			if _, err := a.AppendUserMessageToSession(targetID, content); err != nil {
				t.Fatalf("append user turn: %v", err)
			}
		}
		blockedTurn := filepath.Join(sessionsRoot, targetID, "turns", "3")
		if err := os.Chmod(blockedTurn, 0o555); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(blockedTurn, 0o700) }()

		result, err := a.ApplyTurnActionForSession(targetID, 3, TurnActionRevertHistory, false)
		if err == nil {
			t.Fatal("revert_history succeeded despite the blocked removal; the failure was swallowed")
		}
		if !strings.Contains(err.Error(), "revert history turn 3") {
			t.Fatalf("revert error = %q, want it to name turn 3 where the walk stopped", err.Error())
		}
		if result.Action != TurnActionRevertHistory || result.Turn != 3 || result.TargetTurn != 2 || !result.SessionChanged {
			t.Fatalf("populated result = %#v, want revert_history to turn 2 with session change", result)
		}
		if result.Session.ID != targetID {
			t.Fatalf("result session = %q, want %q", result.Session.ID, targetID)
		}
		if got := userContents(result.Messages); !equalStrings(got, loopContents[:3]) {
			t.Fatalf("result messages = %q, want the surviving turns 1..3 %q", got, loopContents[:3])
		}
		// Afterwards the target is live, claimed, consistent, the reservation is
		// released, and the current selection is unchanged. Whether the
		// reservation was held across the durable call is not observable here.
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[targetID]
		if unit == nil {
			rt.mu.Unlock()
			t.Fatal("failed revert evicted the non-current session from the live map")
		}
		transitioning := unit.transitioning
		queueLen := len(unit.queue)
		loopMsgs := len(unit.lp.Messages())
		rt.mu.Unlock()
		if unit.store == nil || !unit.store.Active() || unit.store.SessionID() != targetID {
			t.Fatal("failed revert detached the target session's store")
		}
		if transitioning {
			t.Fatal("transitioning reservation not released after failed revert")
		}
		if queueLen != 0 {
			t.Fatalf("queue length after failed revert = %d, want 0", queueLen)
		}
		if loopMsgs != len(loopContents) {
			t.Fatalf("loop message count = %d, want %d (system prompt + %d surviving user turns; the loop was reloaded to match disk)", loopMsgs, len(loopContents), len(loopContents)-1)
		}
		msgs, err := a.SessionMessagesFor(targetID)
		if err != nil {
			t.Fatalf("messages for %q: %v", targetID, err)
		}
		if got := userContents(msgs); !equalStrings(got, loopContents[:3]) {
			t.Fatalf("durable messages after failed revert = %q, want the surviving turns 1..3 %q", got, loopContents[:3])
		}
		if err := unitMutableLocked(unit); err != nil {
			t.Fatalf("target unit not driveable after failed revert: %v", err)
		}
		if got := a.SessionCurrent().ID; got != secondID {
			t.Fatalf("SessionCurrent = %q, want unchanged %q", got, secondID)
		}
		// The live store still holds the target's claim.
		claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, targetID)
		if err != nil {
			t.Fatalf("claim check: %v", err)
		}
		if ok {
			_ = claim.Release()
			t.Fatal("claim released by the failed revert; another process could drive the session")
		}
	})
}

// seedUnitQueue plants a queue item on a live unit. The volatile queue has no
// exported injection surface: Submit appends only while a turn is running and
// the drainer then consumes it, so a deterministic queued state can only be
// arranged through the field, as the queue tests do.
func seedUnitQueue(t *testing.T, a *Agent, id, content string) {
	t.Helper()
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	unit := a.sessions[id]
	if unit == nil && a.currentSessionID == id {
		unit = a.ensureRuntime().sessionLocked()
	}
	if unit == nil {
		t.Fatal("seeding queue for a session that is not live")
	}
	unit.queue = []QueuedItem{{ID: "q-1", Content: content}}
	unit.queueVersion = 1
}

// assertCurrentRemovalFailureIntact asserts that a failed current-session
// removal left the unit live, claimed, selected and queued, with the
// transitioning reservation released.
func assertCurrentRemovalFailureIntact(t *testing.T, a *Agent, id string, proj *project.Project) {
	t.Helper()
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit := a.sessions[id]
	if unit == nil {
		rt.mu.Unlock()
		t.Fatal("failed removal evicted the current session from the live map")
	}
	queueLen := len(unit.queue)
	transitioning := unit.transitioning
	current := a.currentSessionID
	rt.mu.Unlock()
	if unit.store == nil || !unit.store.Active() {
		t.Fatal("failed removal detached the current session's store")
	}
	if unit.store.SessionID() != id {
		t.Fatalf("store session id = %q, want %q", unit.store.SessionID(), id)
	}
	if current != id {
		t.Fatalf("currentSessionID = %q, want %q", current, id)
	}
	if got := a.SessionCurrent().ID; got != id {
		t.Fatalf("SessionCurrent = %q, want %q", got, id)
	}
	if queueLen != 1 {
		t.Fatalf("queue length after failed removal = %d, want 1", queueLen)
	}
	if transitioning {
		t.Fatal("transitioning reservation not released after failed removal")
	}
	// The live store still holds the session's claim.
	claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, id)
	if err != nil {
		t.Fatalf("claim check: %v", err)
	}
	if ok {
		_ = claim.Release()
		t.Fatal("claim released by the failed removal; another process could drive the session")
	}
}

// assertCurrentRevertFailureIntact asserts that a failed current-session
// revert left the unit live, claimed, selected and consistent: the durable
// history is truncated only as far as the walk reached (all but the last
// contents), the loop was reloaded to match it, and the transitioning
// reservation is released so the unit is driveable again.
func assertCurrentRevertFailureIntact(t *testing.T, a *Agent, id string, proj *project.Project, loopContents []string) {
	t.Helper()
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit := a.sessions[id]
	if unit == nil {
		rt.mu.Unlock()
		t.Fatal("failed revert evicted the current session from the live map")
	}
	transitioning := unit.transitioning
	queueLen := len(unit.queue)
	loopMsgs := len(unit.lp.Messages())
	current := a.currentSessionID
	rt.mu.Unlock()
	if unit.store == nil || !unit.store.Active() {
		t.Fatal("failed revert detached the current session's store")
	}
	if unit.store.SessionID() != id {
		t.Fatalf("store session id = %q, want %q", unit.store.SessionID(), id)
	}
	if current != id {
		t.Fatalf("currentSessionID = %q, want %q", current, id)
	}
	if got := a.SessionCurrent().ID; got != id {
		t.Fatalf("SessionCurrent = %q, want %q", got, id)
	}
	if transitioning {
		t.Fatal("transitioning reservation not released after failed revert")
	}
	if queueLen != 0 {
		t.Fatalf("queue length after failed revert = %d, want 0", queueLen)
	}
	if loopMsgs != len(loopContents) {
		t.Fatalf("loop message count = %d, want %d (system prompt + %d surviving user turns; the loop was reloaded to match disk)", loopMsgs, len(loopContents), len(loopContents)-1)
	}
	msgs, err := a.SessionMessagesFor(id)
	if err != nil {
		t.Fatalf("messages for %q: %v", id, err)
	}
	if got := userContents(msgs); !equalStrings(got, loopContents[:len(loopContents)-1]) {
		t.Fatalf("durable messages after failed revert = %q, want the surviving turns 1..%d %q", got, len(loopContents)-1, loopContents[:len(loopContents)-1])
	}
	if err := unitMutableLocked(unit); err != nil {
		t.Fatalf("unit not driveable after failed revert: %v", err)
	}
	// The live store still holds the session's claim.
	claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, id)
	if err != nil {
		t.Fatalf("claim check: %v", err)
	}
	if ok {
		_ = claim.Release()
		t.Fatal("claim released by the failed revert; another process could drive the session")
	}
}

// assertNonCurrentRemovalFailureIntact asserts that a failed non-current
// removal left the target unit live, claimed and queued, the current selection
// unchanged, and the transitioning reservation released.
func assertNonCurrentRemovalFailureIntact(t *testing.T, a *Agent, targetID, currentID string, proj *project.Project) {
	t.Helper()
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit := a.sessions[targetID]
	if unit == nil {
		rt.mu.Unlock()
		t.Fatal("failed removal evicted the non-current session from the live map")
	}
	queueLen := len(unit.queue)
	transitioning := unit.transitioning
	rt.mu.Unlock()
	if unit.store == nil || !unit.store.Active() || unit.store.SessionID() != targetID {
		t.Fatal("failed removal detached the non-current session's store")
	}
	if queueLen != 1 {
		t.Fatalf("queue length after failed removal = %d, want 1", queueLen)
	}
	if transitioning {
		t.Fatal("transitioning reservation not released after failed removal")
	}
	if got := a.SessionCurrent().ID; got != currentID {
		t.Fatalf("SessionCurrent = %q, want unchanged %q", got, currentID)
	}
	// The live store still holds the target's claim.
	claim, ok, err := snapshot.AcquireSessionClaim(a.projects.Root(), proj.ID, targetID)
	if err != nil {
		t.Fatalf("claim check: %v", err)
	}
	if ok {
		_ = claim.Release()
		t.Fatal("claim released by the failed removal; another process could drive the session")
	}
}

// TestFailedTransitionRearmsPendingWork asserts that a failed session removal
// rearms pending work: the deferred reservation release clears transitioning
// AND re-nudges the queue drainer, so an item queued before the removal drains
// on its own once the durable fault is cleared — no further submit and no
// manual nudge. The shape=B assertions above cover the precondition (unit
// live, claimed, selected, queued, transitioning cleared) but would still pass
// against an implementation that cleared the flag without rearming the
// drainer, leaving the session holding queued work that never drains; this
// test observes the resumption itself at the model boundary.
//
// The failure is injected exactly as the shape=B subtests do, through
// filesystem permissions at the durable commit: ArchiveSession writes the new
// meta.json atomically inside the session directory, so an unwritable session
// dir fails exactly the write; DeleteSession's commit is the atomic rename out
// of the sessions root, so an unwritable sessions root fails exactly the
// rename. After the fault is cleared, the only wake source left is the failed
// removal's own reservation release: nothing else in this test nudges the
// drainer or submits input, and the signal scheduler cannot start the turn
// (it only wakes sessions with empty queues).
//
// The queue is seeded directly via seedUnitQueue because the volatile queue
// has no exported mutation surface (Submit consumes it immediately when idle);
// everything else is driven through adapter-facing methods: NewSession,
// SessionArchive, SessionDelete, QueueSnapshotForSession, BusyForSession,
// SessionMessagesFor. For the archive cases the resumed drain may launch while
// the session dir is still unwritable, and message persistence is best-effort
// there, so the shared evidence is the model request itself; the delete cases
// additionally assert the content persisted as a user turn.
func TestFailedTransitionRearmsPendingWork(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	var (
		mu    sync.Mutex
		reqs  int
		users [][]string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var us []string
		for _, m := range body.Messages {
			if m.Role == "user" {
				if s, ok := m.Content.(string); ok {
					us = append(us, s)
				}
			}
		}
		mu.Lock()
		reqs++
		users = append(users, us)
		mu.Unlock()
		writeTextResponse(w, "drained")
	}))
	defer server.Close()

	// newTarget builds a started agent with one live session and seeds its
	// queue, returning the agent, the project sessions root, the project id,
	// and the session id.
	newTarget := func(t *testing.T) (*Agent, string, string, string) {
		t.Helper()
		a := newEventOrderAgent(t, server.URL+"/v1")
		_ = startEventOrderAgent(t, a, &eventCapture{})
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatal(err)
		}
		id, err := a.NewSession(proj.ID, "primary")
		if err != nil {
			t.Fatal(err)
		}
		seedUnitQueue(t, a, id, "queued draft")
		return a, a.projects.SessionsRoot(proj.ID), proj.ID, id
	}

	// startCounts captures the model server's request counter before a
	// removal, so each subtest requires exactly one NEW request — the resumed
	// drain — regardless of earlier subtests.
	startCounts := func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return reqs, len(users)
	}

	t.Run("current_delete_failure_resumes", func(t *testing.T) {
		a, sessionsRoot, _, id := newTarget(t)
		startReqs, startUsers := startCounts()
		if err := os.Chmod(sessionsRoot, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := a.SessionDelete(id); err == nil {
			t.Fatal("SessionDelete unexpectedly succeeded with an unwritable sessions root")
		}
		if err := os.Chmod(sessionsRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		assertQueuedWorkResumes(t, a, id, "queued draft", true, startReqs, startUsers, &mu, &reqs, &users)
	})

	t.Run("current_archive_failure_resumes", func(t *testing.T) {
		a, sessionsRoot, _, id := newTarget(t)
		startReqs, startUsers := startCounts()
		if err := os.Chmod(filepath.Join(sessionsRoot, id), 0o500); err != nil {
			t.Fatal(err)
		}
		if err := a.SessionArchive(id); err == nil {
			t.Fatal("SessionArchive unexpectedly succeeded with an unwritable session dir")
		}
		if err := os.Chmod(filepath.Join(sessionsRoot, id), 0o700); err != nil {
			t.Fatal(err)
		}
		assertQueuedWorkResumes(t, a, id, "queued draft", false, startReqs, startUsers, &mu, &reqs, &users)
	})

	t.Run("noncurrent_delete_failure_resumes", func(t *testing.T) {
		a, sessionsRoot, projID, targetID := newTarget(t)
		if _, err := a.NewSession(projID, "primary"); err != nil {
			t.Fatal(err)
		}
		startReqs, startUsers := startCounts()
		if err := os.Chmod(sessionsRoot, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := a.SessionDelete(targetID); err == nil {
			t.Fatal("SessionDelete unexpectedly succeeded with an unwritable sessions root")
		}
		if err := os.Chmod(sessionsRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		assertQueuedWorkResumes(t, a, targetID, "queued draft", true, startReqs, startUsers, &mu, &reqs, &users)
	})

	t.Run("noncurrent_archive_failure_resumes", func(t *testing.T) {
		a, sessionsRoot, projID, targetID := newTarget(t)
		if _, err := a.NewSession(projID, "primary"); err != nil {
			t.Fatal(err)
		}
		startReqs, startUsers := startCounts()
		if err := os.Chmod(filepath.Join(sessionsRoot, targetID), 0o500); err != nil {
			t.Fatal(err)
		}
		if err := a.SessionArchive(targetID); err == nil {
			t.Fatal("SessionArchive unexpectedly succeeded with an unwritable session dir")
		}
		if err := os.Chmod(filepath.Join(sessionsRoot, targetID), 0o700); err != nil {
			t.Fatal(err)
		}
		assertQueuedWorkResumes(t, a, targetID, "queued draft", false, startReqs, startUsers, &mu, &reqs, &users)
	})
}

// assertQueuedWorkResumes waits for the queued item to drain on its own after
// the durable fault was cleared — no further submit and no manual nudge — and
// asserts the resumption evidence: exactly one new model request (the
// startReqs-th) carrying the queued content, and an empty queue. When the
// fault left the session dir writable (delete cases), it additionally asserts
// the content persisted as a user turn.
func assertQueuedWorkResumes(t *testing.T, a *Agent, id, want string, wantPersisted bool, startReqs, startUsers int, mu *sync.Mutex, reqs *int, users *[][]string) {
	t.Helper()
	resumed := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		queue, err := a.QueueSnapshotForSession(id)
		if err != nil {
			t.Fatalf("queue snapshot for %q: %v", id, err)
		}
		busy, err := a.BusyForSession(id)
		if err != nil {
			t.Fatalf("busy for %q: %v", id, err)
		}
		mu.Lock()
		n := *reqs
		mu.Unlock()
		if n == startReqs+1 && len(queue.Items) == 0 && !busy {
			resumed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !resumed {
		mu.Lock()
		n := *reqs
		mu.Unlock()
		t.Fatalf("queued work never resumed after the failed removal: new model requests=%d, want 1 with the queue empty and the session idle", n-startReqs)
	}
	if len(*users) != startUsers+1 || !contains((*users)[len(*users)-1], want) {
		t.Fatalf("resumed drain request user contents = %#v, want %q in the single new request", (*users)[startUsers:], want)
	}
	if wantPersisted {
		msgs, err := a.SessionMessagesFor(id)
		if err != nil {
			t.Fatalf("messages for %q: %v", id, err)
		}
		if got := userContents(msgs); !contains(got, want) {
			t.Fatalf("queued content %q not persisted into history after the resumed drain; user turns=%q", want, got)
		}
	}
}

// assertSessionListedActive asserts that the session is still listed as active
// on disk after a failed removal.
func assertSessionListedActive(t *testing.T, sessionsRoot, projectPath, id string) {
	t.Helper()
	list, err := snapshot.List(sessionsRoot, projectPath, snapshot.StateActive)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	for _, info := range list {
		if info.ID == id {
			return
		}
	}
	t.Fatalf("session %q not listed as active after failed removal: %#v", id, list)
}
