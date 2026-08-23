package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/tool"
)

// seedCompleteTurns persists n complete turns, one user message each, through
// the store and the loop.
func seedCompleteTurns(t *testing.T, a *Agent, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		appendUserTurn(t, a, fmt.Sprintf("turn %d", i))
	}
}

// blockTurnDir makes one turn directory's removal fail: an unwritable
// directory blocks os.RemoveAll exactly there, so the descending history walk
// stops at it.
func blockTurnDir(t *testing.T, a *Agent, turn int) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	blocked := filepath.Join(a.store.Dir(), "turns", strconv.Itoa(turn))
	if err := os.Chmod(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
}

// blockTurnMessages makes one surviving turn's messages.jsonl unreadable: the
// post-walk reload derives the loop from disk, and the first disk read of a
// surviving turn is its messages file, so the reload fails exactly there
// while the walk itself — which only removes turn directories — proceeds
// normally. The caller restores the permission before any later assertion
// reads the surviving turns.
func blockTurnMessages(t *testing.T, a *Agent, turn int) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not block reads as root")
	}
	blocked := filepath.Join(a.store.Dir(), "turns", strconv.Itoa(turn), "messages.jsonl")
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o600) })
}

func loopUserContents(msgs []message.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role != message.RoleUser {
			continue
		}
		var text string
		for _, p := range m.Content {
			text += p.Text
		}
		out = append(out, text)
	}
	return out
}

// TestRevertPartialWalkReloadsLoopAndPublishesBoundary covers the partial-walk
// outcome with a reload that succeeds: the walk removed turns 8-10 and stopped
// at turn 7, so the loop must be re-derived from disk (turns 1-7), the error
// must name turn 7, and the boundary must be published over the reconciled
// state — today the loop keeps all ten turns and nothing is published.
func TestRevertPartialWalkReloadsLoopAndPublishesBoundary(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	id := a.SessionCurrent().ID
	blockTurnDir(t, a, 7)

	var got []HydrationState
	result, err := a.ApplyTurnActionForSessionWithBoundary(id, 6, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert, _ string, _ *snapshot.CommittedMutationError, _ *string) {
		got = append(got, hs)
	})
	if err == nil {
		t.Fatal("partial walk reported success")
	}
	if !strings.Contains(err.Error(), "turn 7") {
		t.Fatalf("revert error = %q, want it to name turn 7 where removal stopped", err.Error())
	}
	// The loop matches disk: turns 8-10 are gone from both.
	if c := loopUserContents(a.lp.Messages()); !equalStrings(c, []string{"turn 1", "turn 2", "turn 3", "turn 4", "turn 5", "turn 6", "turn 7"}) {
		t.Fatalf("loop history after partial revert = %q, want turns 1-7 (the loop must equal disk)", c)
	}
	// The unit stays live: only a failed reload evicts.
	if _, err := a.SessionSummaryForSession(id); err != nil {
		t.Fatalf("unit was evicted after a reconciled partial walk: %v", err)
	}
	// The boundary is published exactly once, over the reconciled history.
	if len(got) != 1 {
		t.Fatalf("boundary emitted %d times, want exactly 1", len(got))
	}
	if c := userContents(got[0].Messages); !equalStrings(c, []string{"turn 1", "turn 2", "turn 3", "turn 4", "turn 5", "turn 6", "turn 7"}) {
		t.Fatalf("boundary messages = %q, want turns 1-7", c)
	}
	// The reconciled failure rides the result as a warning.
	if result.Warning != err.Error() {
		t.Fatalf("result.Warning = %q, want the walk error %q", result.Warning, err.Error())
	}
	// The coordinator mutation committed exactly once: the rewrite epoch
	// advanced, and the committed bound was lowered to the stop turn (this
	// unit never committed a turn, so the bound stays 0 — the epoch advance is
	// what makes a racing live capture re-read the truncated prefix).
	tr := a.transcriptForSessionID(id)
	if tr == nil {
		t.Fatal("no live coordinator for the session")
	}
	tr.seqMu.Lock()
	rev := tr.revisionLocked()
	tr.seqMu.Unlock()
	if rev.committedTurn != 0 {
		t.Fatalf("committedTurn = %d, want 0 (nothing was committed on this unit)", rev.committedTurn)
	}
	if rev.rewriteEpoch != 1 {
		t.Fatalf("rewriteEpoch = %d, want exactly 1 after the partial walk", rev.rewriteEpoch)
	}
}

func TestRevertHighestTurnPartialMutationUsesExplicitOutcome(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 5)
	id := a.SessionCurrent().ID
	tr := a.transcriptForSessionID(id)
	if tr == nil {
		t.Fatal("no live coordinator")
	}
	a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: "surviving error", Turn: 4})
	a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: "removed error", Turn: 5})
	injected := errors.New("injected highest-turn partial failure")
	snapshot.RemoveHistoryTurnFunc = func(path string) error {
		if filepath.Base(path) == "5" {
			if err := os.Remove(filepath.Join(path, "messages.jsonl")); err != nil {
				return err
			}
		}
		return injected
	}
	t.Cleanup(func() { snapshot.RemoveHistoryTurnFunc = nil })

	var boundaries int
	result, err := a.ApplyTurnActionForSessionWithBoundary(id, 3, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert, warning string, _ *snapshot.CommittedMutationError, _ *string) {
		boundaries++
		if hs.Session.ID == "" || warning == "" {
			t.Errorf("boundary = %#v warning=%q, want reconciled session and warning", hs, warning)
		}
	})
	if !errors.Is(err, injected) {
		t.Fatalf("revert error = %v, want injected failure", err)
	}
	if result.Warning == "" || boundaries != 1 {
		t.Fatalf("result warning=%q boundaries=%d, want one reconciled boundary", result.Warning, boundaries)
	}
	if got := loopUserContents(a.lp.Messages()); !equalStrings(got, []string{"turn 1", "turn 2", "turn 3", "turn 4"}) {
		t.Fatalf("loop after highest-turn partial mutation = %q, want turns 1-4", got)
	}
	if got := a.store.CurrentTurn(); got != 5 {
		t.Fatalf("operational current turn = %d, want 5", got)
	}
	if got := retainedErrorTurns(t, a, id); !reflect.DeepEqual(got, []int{4}) {
		t.Fatalf("retained errors after visible history lowered = %v, want [4]", got)
	}
}

func TestDirectRevertHighestTurnPartialMutationReconcilesWithoutCallback(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 5)
	id := a.SessionCurrent().ID
	tr := a.transcriptForSessionID(id)
	if tr == nil {
		t.Fatal("no live coordinator")
	}
	a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: "surviving error", Turn: 4})
	a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: "removed error", Turn: 5})
	seedQueue(t, a, 7, "stale after direct partial revert")
	tracked := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("tracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.fileTracker.TrackIdentity(tracked, 0, 0, tool.FileIdentity{Valid: true})
	if !agentTrackerHasRead(a, tracked) {
		t.Fatal("setup: tracker missing read state")
	}
	injected := errors.New("injected direct highest-turn partial failure")
	snapshot.RemoveHistoryTurnFunc = func(path string) error {
		if filepath.Base(path) == "5" {
			if err := os.Remove(filepath.Join(path, "messages.jsonl")); err != nil {
				return err
			}
		}
		return injected
	}
	t.Cleanup(func() { snapshot.RemoveHistoryTurnFunc = nil })

	result, err := a.ApplyTurnActionForSession(id, 3, TurnActionRevertHistory, false)
	if !errors.Is(err, injected) || result.Warning == "" {
		t.Fatalf("result = %+v err=%v, want reconciled warning and injected error", result, err)
	}
	if got := loopUserContents(a.lp.Messages()); !equalStrings(got, []string{"turn 1", "turn 2", "turn 3", "turn 4"}) {
		t.Fatalf("loop after direct partial revert = %q, want turns 1-4", got)
	}
	queue := a.QueueSnapshot()
	if len(queue.Items) != 0 || queue.Items == nil || queue.Version <= 7 {
		t.Fatalf("queue after direct partial revert = %#v, want empty version > 7", queue)
	}
	if agentTrackerHasRead(a, tracked) {
		t.Fatal("tracker not reset by direct partial revert")
	}
	if got := retainedErrorTurns(t, a, id); !reflect.DeepEqual(got, []int{4}) {
		t.Fatalf("retained errors after direct partial revert = %v, want [4]", got)
	}
	tr.seqMu.Lock()
	epoch := tr.rewriteEpoch
	tr.seqMu.Unlock()
	if epoch != 1 {
		t.Fatalf("rewrite epoch after direct partial revert = %d, want 1", epoch)
	}
}

func TestRevertUnknownPostStateEvictsOwner(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 3)
	id := a.SessionCurrent().ID
	injected := errors.New("injected unreadable post-state")
	snapshot.RemoveHistoryTurnFunc = func(path string) error {
		if err := os.Remove(filepath.Join(path, "complete")); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(path, "messages.jsonl")); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(path, "messages.jsonl"), 0o700); err != nil {
			return err
		}
		return injected
	}
	t.Cleanup(func() { snapshot.RemoveHistoryTurnFunc = nil })

	var boundaries []HydrationState
	_, err := a.ApplyTurnActionForSessionWithBoundary(id, 3, TurnActionRevertHistory, false, func(state HydrationState, _ []snapshot.SkippedRevert, _ string, _ *snapshot.CommittedMutationError, _ *string) {
		boundaries = append(boundaries, state)
	})
	if !errors.Is(err, injected) {
		t.Fatalf("revert error = %v, want injected unreadable post-state", err)
	}
	assertEvicted(t, a, id, boundaries)
}

func TestRevertTurnSyncFailureReconcilesOwner(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	cap := &eventCapture{}
	a.SetEventHandler(cap.handler)
	seedCompleteTurns(t, a, 3)
	id := a.SessionCurrent().ID
	tr := a.transcriptForSessionID(id)
	if tr == nil {
		t.Fatal("no live coordinator for the session")
	}
	for _, turn := range []int{1, 2, 3} {
		a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: fmt.Sprintf("error at %d", turn), Turn: turn})
	}
	seedQueue(t, a, 4, "stale after turn removal")
	turnsDir := filepath.Join(a.store.Dir(), "turns")
	injected := errors.New("injected turns directory sync failure")
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == turnsDir {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	var boundary []HydrationState
	var boundaryWarning string
	var boundaryErr *snapshot.CommittedMutationError
	result, err := a.ApplyTurnActionForSessionWithBoundary(id, 2, TurnActionRevertHistory, false, func(state HydrationState, _ []snapshot.SkippedRevert, warning string, committed *snapshot.CommittedMutationError, _ *string) {
		boundary = append(boundary, state)
		boundaryWarning = warning
		boundaryErr = committed
	})
	var committed *snapshot.CommittedMutationError
	if !errors.As(err, &committed) || !errors.Is(err, injected) {
		t.Fatalf("revert error = %v, want the committed turns-dir sync error", err)
	}
	if result.Session.ID != id || result.TargetTurn != 1 || result.Warning != err.Error() {
		t.Fatalf("result = session:%q target:%d warning:%q, want surviving session, target 1, and typed error warning", result.Session.ID, result.TargetTurn, result.Warning)
	}
	if !a.store.Active() || a.currentSessionID != id {
		t.Fatalf("owner after committed turn sync failure = active:%v current:%q, want retained live unit", a.store.Active(), a.currentSessionID)
	}
	a.ensureRuntime().mu.Lock()
	_, live := a.sessions[id]
	a.ensureRuntime().mu.Unlock()
	if !live || a.transcriptForSessionID(id) == nil {
		t.Fatalf("owner reconciliation lost live unit or transcript: live=%v transcript=%v", live, a.transcriptForSessionID(id) != nil)
	}
	if got := a.store.CurrentTurn(); got != 2 {
		t.Fatalf("current turn after turn removal sync failure = %d, want surviving bound 2", got)
	}
	if got := loopUserContents(a.lp.Messages()); !equalStrings(got, []string{"turn 1", "turn 2"}) {
		t.Fatalf("reloaded loop history = %q, want surviving turns 1-2", got)
	}
	assertQueueClearedAfterVersion(t, a, cap, 4)
	if got := retainedErrorTurns(t, a, id); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("retained error turns = %v, want surviving turns [1 2]", got)
	}
	tr.seqMu.Lock()
	epoch := tr.rewriteEpoch
	tr.seqMu.Unlock()
	if epoch != 1 {
		t.Fatalf("transcript rewrite epoch = %d, want 1 after reconciliation", epoch)
	}
	if len(boundary) != 1 || boundary[0].Session.ID != id || !equalStrings(userContents(boundary[0].Messages), []string{"turn 1", "turn 2"}) {
		t.Fatalf("boundary = %#v, want one surviving-history state", boundary)
	}
	if boundaryErr == nil || boundaryWarning != err.Error() {
		t.Fatalf("boundary error/warning = %v/%q, want typed error and the same warning", boundaryErr, boundaryWarning)
	}
}

// assertEvicted verifies the failed-reload outcome: the unit is out of the
// live map, its transcript is unregistered, its claim is released, the empty
// HydrationState was delivered, and no turn is admitted afterwards.
func assertEvicted(t *testing.T, a *Agent, id string, got []HydrationState) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("empty-state boundary emitted %d times, want exactly 1", len(got))
	}
	if got[0].Session.ID != "" || len(got[0].Messages) != 0 || len(got[0].Tail) != 0 || len(got[0].Errors) != 0 {
		t.Fatalf("eviction boundary = %#v, want the empty HydrationState", got[0])
	}
	if a.store.Active() {
		t.Fatal("store still active after eviction")
	}
	if a.currentSessionID != "" {
		t.Fatalf("currentSessionID = %q after eviction, want empty", a.currentSessionID)
	}
	if a.sessions[id] != nil {
		t.Fatal("evicted unit is still in the live map")
	}
	if a.transcriptForSessionID(id) != nil {
		t.Fatal("evicted unit's transcript is still registered")
	}
	if cur := a.SessionCurrent().ID; cur != "" {
		t.Fatalf("SessionCurrent = %q after eviction, want no current session", cur)
	}
	if _, err := a.resolveLiveSession(id); err == nil {
		t.Fatal("evicted id still resolves as a live session")
	}
	// No turn is admitted afterwards: the unit is gone from the live map.
	if _, err := a.ApplyTurnActionForSession(id, 2, TurnActionRevertHistory, false); err == nil {
		t.Fatal("a turn action was admitted on an evicted session")
	}
	// The claim was released: a fresh store with the same project context can
	// load the session.
	proj, err := a.projects.Current()
	if err != nil {
		t.Fatalf("current project: %v", err)
	}
	fresh, err := snapshot.NewForSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID)
	if err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	if err := fresh.LoadSession(id); err != nil {
		t.Fatalf("session claim still held after eviction: %v", err)
	}
	fresh.Detach()
}

// TestRevertPartialWalkReloadFailureEvictsUnit covers the partial-walk
// outcome with a reload that fails: the unit must be evicted rather than
// released back over a loop that no longer matches disk, and the returned
// error stays the walk's.
func TestRevertPartialWalkReloadFailureEvictsUnit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	id := a.SessionCurrent().ID
	// The reload must fail after the walk ran: the walk stops at the blocked
	// turn 7 directory, and the reload then fails reading the surviving
	// turn 7's messages file. The session dir is captured before the revert:
	// the failed reload evicts the unit and detaches the store, so
	// store.Dir() is empty afterwards.
	sessionDir := a.store.Dir()
	blockTurnMessages(t, a, 7)
	blockTurnDir(t, a, 7)

	var got []HydrationState
	var evictWarning string
	_, err := a.ApplyTurnActionForSessionWithBoundary(id, 6, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert, warning string, _ *snapshot.CommittedMutationError, _ *string) {
		got = append(got, hs)
		evictWarning = warning
	})
	// Restore the messages file before any assertion reads the surviving
	// turns, so a later read cannot fail for the injected reason.
	if err := os.Chmod(filepath.Join(sessionDir, "turns", "7", "messages.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("partial walk with failed reload reported success")
	}
	if !strings.Contains(err.Error(), "turn 7") {
		t.Fatalf("revert error = %q, want the walk's error naming turn 7", err.Error())
	}
	// The reload error is joined onto the walk error, never substituted for
	// it, so neither cause is lost.
	if !strings.Contains(err.Error(), "messages.jsonl") {
		t.Fatalf("revert error = %q, want the reload's cause joined onto the walk error", err.Error())
	}
	// The eviction boundary is the empty state carrying the error, so the
	// adapter learns the session was evicted because of it.
	if evictWarning == "" {
		t.Fatal("eviction boundary carries no error warning")
	}
	assertEvicted(t, a, id, got)
}

// TestRevertCompleteWalkReloadFailureEvictsUnit covers the complete-walk
// outcome with a reload that fails: the same eviction, with the reload's error
// returned. It fails against any fix applied only to the walk's error branch.
func TestRevertCompleteWalkReloadFailureEvictsUnit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	id := a.SessionCurrent().ID
	// The reload must fail after the walk ran: nothing blocks a removal, so
	// the walk completes, and the reload then fails reading the surviving
	// turn 5's messages file. The session dir is captured before the revert:
	// the failed reload evicts the unit and detaches the store, so
	// store.Dir() is empty afterwards.
	sessionDir := a.store.Dir()
	blockTurnMessages(t, a, 5)

	var got []HydrationState
	_, err := a.ApplyTurnActionForSessionWithBoundary(id, 6, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert, _ string, _ *snapshot.CommittedMutationError, _ *string) {
		got = append(got, hs)
	})
	// Restore the messages file before any assertion reads the surviving
	// turns: the turns-on-disk assertion below loads them.
	if err := os.Chmod(filepath.Join(sessionDir, "turns", "5", "messages.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("complete walk with failed reload reported success")
	}
	if strings.Contains(err.Error(), "turn 7") {
		t.Fatalf("revert error = %q, want the reload error, not the walk's", err.Error())
	}
	assertEvicted(t, a, id, got)
	// The complete walk removed turns 6-10; only 1-5 survive on disk.
	if got := loadedTurnNumbers(t, a, id); !reflect.DeepEqual(got, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("turns on disk after eviction = %v, want [1 2 3 4 5]", got)
	}
}

func loadedTurnNumbers(t *testing.T, a *Agent, id string) []int {
	t.Helper()
	proj, err := a.projects.Current()
	if err != nil {
		t.Fatalf("current project: %v", err)
	}
	store, err := snapshot.NewForSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID)
	if err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	if err := store.LoadSession(id); err != nil {
		t.Fatalf("load evicted session: %v", err)
	}
	defer store.Detach()
	turns, err := store.LoadCompleteTurns()
	if err != nil {
		t.Fatalf("load complete turns: %v", err)
	}
	var out []int
	for _, t := range turns {
		out = append(out, t.Turn)
	}
	return out
}

// TestRevertPartialWalkClearsQueue covers the queue rule on a partial walk:
// at least one turn was removed, so queued input written against the removed
// turns is cleared.
func TestRevertPartialWalkClearsQueue(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	seedQueue(t, a, 7, "stale after partial revert")
	blockTurnDir(t, a, 7)

	if _, err := a.ApplyTurnActionForSession(a.SessionCurrent().ID, 6, TurnActionRevertHistory, false); err == nil {
		t.Fatal("partial walk reported success")
	}
	got := a.QueueSnapshot()
	if len(got.Items) != 0 || got.Items == nil {
		t.Fatalf("queue after partial revert = %#v, want empty: turns were removed, so queued input no longer applies", got.Items)
	}
	if got.Version <= 7 {
		t.Fatalf("queue version = %d, want > 7", got.Version)
	}
	// The coordinator mutation committed alongside the queue clear: the
	// rewrite epoch advanced exactly once.
	tr := a.transcriptForSessionID(a.SessionCurrent().ID)
	if tr == nil {
		t.Fatal("no live coordinator for the session")
	}
	tr.seqMu.Lock()
	epoch := tr.rewriteEpoch
	tr.seqMu.Unlock()
	if epoch != 1 {
		t.Fatalf("rewriteEpoch = %d, want exactly 1 after the partial walk", epoch)
	}
}

// TestRevertFirstRemovalFailureKeepsQueue covers the queue rule when the walk
// removed nothing: the first removal failed, so the queued input is kept. The
// same failure is a zero-history-mutation failure — no compaction record was
// removed and no turn was removed — so it is returned as a precommit failure
// with no boundary, no coordinator mutation, and no tracker reset, and the
// loop and disk stay exactly as they were.
func TestRevertFirstRemovalFailureKeepsQueue(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	seedQueue(t, a, 7, "stale after failed first removal")
	blockTurnDir(t, a, 10)
	id := a.SessionCurrent().ID
	// Track one read so a tracker reset is observable.
	path := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.fileTracker.TrackIdentity(path, 0, 0, tool.FileIdentity{Valid: true})
	if !agentTrackerHasRead(a, path) {
		t.Fatal("setup: tracker missing read state")
	}

	var boundaries int
	var warnings []string
	_, err := a.ApplyTurnActionForSessionWithBoundary(id, 6, TurnActionRevertHistory, false, func(_ HydrationState, _ []snapshot.SkippedRevert, warning string, _ *snapshot.CommittedMutationError, _ *string) {
		boundaries++
		warnings = append(warnings, warning)
	})
	if err == nil {
		t.Fatal("first-removal failure reported success")
	}
	if !strings.Contains(err.Error(), "turn 10") {
		t.Fatalf("revert error = %q, want it to name turn 10 where removal stopped", err.Error())
	}
	got := a.QueueSnapshot()
	if len(got.Items) != 1 || got.Items[0].Content != "stale after failed first removal" {
		t.Fatalf("queue after first-removal failure = %#v, want the queued item kept", got.Items)
	}
	if got.Version != 7 {
		t.Fatalf("queue version = %d, want unchanged 7", got.Version)
	}
	// Zero-history-mutation failure: nothing was durably removed, so the error
	// is the whole outcome — no boundary, no coordinator mutation, no tracker
	// reset, and the loop and disk stay exactly as they were.
	if boundaries != 0 {
		t.Fatalf("boundaries emitted = %d, want 0 for a zero-history-mutation failure (%v)", boundaries, warnings)
	}
	tr := a.transcriptForSessionID(id)
	if tr == nil {
		t.Fatal("no live coordinator for the session")
	}
	tr.seqMu.Lock()
	epoch := tr.rewriteEpoch
	tr.seqMu.Unlock()
	if epoch != 0 {
		t.Fatalf("rewriteEpoch = %d, want 0 (no coordinator mutation)", epoch)
	}
	if !agentTrackerHasRead(a, path) {
		t.Fatal("tracker reset by a zero-history-mutation failure")
	}
	if c := loopUserContents(a.lp.Messages()); !equalStrings(c, []string{"turn 1", "turn 2", "turn 3", "turn 4", "turn 5", "turn 6", "turn 7", "turn 8", "turn 9", "turn 10"}) {
		t.Fatalf("loop history changed by a zero-history-mutation failure: %q", c)
	}
	turns, err := a.store.LoadCompleteTurns()
	if err != nil {
		t.Fatalf("load complete turns: %v", err)
	}
	var numbers []int
	for _, t := range turns {
		numbers = append(numbers, t.Turn)
	}
	if !reflect.DeepEqual(numbers, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Fatalf("turns on disk changed by a zero-history-mutation failure: %v", numbers)
	}
}

// retainedErrorTurns returns the turns of the live coordinator's retained
// errors, in sequence order, for assertions after a revert.
func retainedErrorTurns(t *testing.T, a *Agent, id string) []int {
	t.Helper()
	tr := a.transcriptForSessionID(id)
	if tr == nil {
		t.Fatal("no live coordinator for the session")
	}
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	var turns []int
	for _, e := range tr.retainedErrors {
		turns = append(turns, e.turn)
	}
	return turns
}

// TestRevertPrunesRetainedErrorsAboveSurvivingTurn asserts the history-revert
// disposition of retained errors on a successful walk: errors tagged to turns
// the revert removed point at history that is gone and are pruned, errors at
// or below the surviving turn stay.
func TestRevertPrunesRetainedErrorsAboveSurvivingTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	id := a.SessionCurrent().ID
	tr := a.transcriptForSessionID(id)
	if tr == nil {
		t.Fatal("no live coordinator for the current session")
	}
	a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: "kept below", Turn: 4})
	a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: "kept at target", Turn: 5})
	a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: "points at removed history", Turn: 6})

	if _, err := a.ApplyTurnActionForSession(id, 6, TurnActionRevertHistory, false); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if turns := retainedErrorTurns(t, a, id); !reflect.DeepEqual(turns, []int{4, 5}) {
		t.Fatalf("retained error turns after revert = %v, want [4 5]: errors above the surviving turn must be pruned", turns)
	}
}

// TestRevertPartialWalkPrunesToStoppedTurn asserts the prune point on a
// partial walk: the walk stopped at turn 7, so turns 6 and 7 still exist on
// disk and errors tagged to them survive; only 8-10 go. Pruning to the
// requested target (5) would drop 6 and 7 with history that is still there.
func TestRevertPartialWalkPrunesToStoppedTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	id := a.SessionCurrent().ID
	tr := a.transcriptForSessionID(id)
	if tr == nil {
		t.Fatal("no live coordinator for the current session")
	}
	for _, turn := range []int{5, 6, 7, 8, 9, 10} {
		a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: fmt.Sprintf("error at %d", turn), Turn: turn})
	}
	blockTurnDir(t, a, 7)

	if _, err := a.ApplyTurnActionForSession(id, 6, TurnActionRevertHistory, false); err == nil {
		t.Fatal("partial walk reported success")
	}
	if turns := retainedErrorTurns(t, a, id); !reflect.DeepEqual(turns, []int{5, 6, 7}) {
		t.Fatalf("retained error turns after partial walk = %v, want [5 6 7]: errors tagged to surviving turns must stay", turns)
	}
	// The coordinator mutation committed exactly once alongside the prune: the
	// rewrite epoch advanced even though the committed bound did not change
	// (nothing was committed on this unit), so a racing live capture still
	// re-reads the truncated durable prefix.
	tr.seqMu.Lock()
	epoch := tr.rewriteEpoch
	committedTurn := tr.committedTurn
	tr.seqMu.Unlock()
	if epoch != 1 {
		t.Fatalf("rewriteEpoch = %d, want exactly 1 after the partial walk", epoch)
	}
	if committedTurn != 0 {
		t.Fatalf("committedTurn = %d, want 0 (the bound is never raised)", committedTurn)
	}
}

// TestRevertBelowCompactionBoundaryDropsRecordAndRendersFullHistory asserts
// that reverting below a compaction boundary removes the record before the
// walk: the reload then loads the surviving turns in full instead of dropping
// them behind the boundary the summary names.
func TestRevertBelowCompactionBoundaryDropsRecordAndRendersFullHistory(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	id := a.SessionCurrent().ID
	if err := a.store.SaveCompaction(snapshot.CompactionRecord{BoundaryTurn: 10, Summary: "summary of turns 1-10"}); err != nil {
		t.Fatalf("SaveCompaction: %v", err)
	}

	var got []HydrationState
	if _, err := a.ApplyTurnActionForSessionWithBoundary(id, 6, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert, _ string, _ *snapshot.CommittedMutationError, _ *string) {
		got = append(got, hs)
	}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	rec, err := a.store.LoadCompaction()
	if err != nil {
		t.Fatalf("LoadCompaction: %v", err)
	}
	if rec != nil {
		t.Fatalf("compaction record survived a revert below its boundary: %+v", rec)
	}
	if c := loopUserContents(a.lp.Messages()); !equalStrings(c, []string{"turn 1", "turn 2", "turn 3", "turn 4", "turn 5"}) {
		t.Fatalf("loop history = %q, want turns 1-5 in full with no summary", c)
	}
	if len(got) != 1 {
		t.Fatalf("boundary emitted %d times, want exactly 1", len(got))
	}
	if c := userContents(got[0].Messages); !equalStrings(c, []string{"turn 1", "turn 2", "turn 3", "turn 4", "turn 5"}) {
		t.Fatalf("boundary messages = %q, want turns 1-5", c)
	}
}

// TestRevertCompactionRecordSyncFailurePublishesFullTurns pins the
// sync-failure outcome at the reload: the unlink succeeded, so the record is
// gone and the reload publishes all ten turns with no summary, while the
// error still fails the revert with no turn removed. The record removal is a
// durable history mutation (historyChanged), so the queued input is
// invalidated even though no turn was removed: cleared, version advanced, the
// matching queue-changed event emitted, and nothing retained to rearm.
func TestRevertCompactionRecordSyncFailurePublishesFullTurns(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	cap := &eventCapture{}
	a.SetEventHandler(cap.handler)
	seedCompleteTurns(t, a, 10)
	id := a.SessionCurrent().ID
	seedQueue(t, a, 7, "stale after record unlink")
	if err := a.store.SaveCompaction(snapshot.CompactionRecord{BoundaryTurn: 10, Summary: "summary of turns 1-10"}); err != nil {
		t.Fatalf("SaveCompaction: %v", err)
	}
	atomicfs.SyncDirFunc = func(string) error { return fmt.Errorf("injected sync failure") }
	defer func() { atomicfs.SyncDirFunc = nil }()

	var got []HydrationState
	_, err := a.ApplyTurnActionForSessionWithBoundary(id, 6, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert, _ string, _ *snapshot.CommittedMutationError, _ *string) {
		got = append(got, hs)
	})
	if err == nil {
		t.Fatal("failed directory sync reported success")
	}
	if _, err := os.Stat(filepath.Join(a.store.Dir(), "compaction.json")); !os.IsNotExist(err) {
		t.Fatalf("compaction.json = %v, want it gone: the unlink succeeded", err)
	}
	turns, err := a.store.LoadCompleteTurns()
	if err != nil {
		t.Fatalf("load complete turns: %v", err)
	}
	var numbers []int
	for _, tr := range turns {
		numbers = append(numbers, tr.Turn)
	}
	if !reflect.DeepEqual(numbers, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Fatalf("turns on disk after failed sync = %v, want [1..10] intact: no turn may be removed on top of an un-durable unlink", numbers)
	}
	if len(got) != 1 {
		t.Fatalf("boundary emitted %d times, want exactly 1", len(got))
	}
	if c := userContents(got[0].Messages); !equalStrings(c, []string{"turn 1", "turn 2", "turn 3", "turn 4", "turn 5", "turn 6", "turn 7", "turn 8", "turn 9", "turn 10"}) {
		t.Fatalf("boundary messages = %q, want turns 1-10 in full with no summary", c)
	}
	// The record removal invalidated the queue: cleared, version advanced,
	// the matching queue-changed event emitted, and nothing retained to
	// rearm.
	q := a.QueueSnapshot()
	if len(q.Items) != 0 || q.Items == nil {
		t.Fatalf("queue after record-unlink sync failure = %#v, want empty", q.Items)
	}
	if q.Version <= 7 {
		t.Fatalf("queue version = %d, want > 7", q.Version)
	}
	assertQueueChangedPayloadForVersion(t, cap, q.Version, q.Items)
}

// TestCombinedRevertNoOpCodePhaseFirstRemovalFailureIsPrecommit: a prior
// code-only revert already removed the only snapshot turn above the target,
// so the combined revert's code phase restores nothing (codeChanged=false);
// the first history deletion failure then leaves the operation with no
// durable mutation and returns as a precommit failure — no boundary, no epoch
// advance, no tracker reset.
func TestCombinedRevertNoOpCodePhaseFirstRemovalFailureIsPrecommit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	path := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendUserTurnWithSnapshot(t, a, "modify", path, "v2")
	id := a.SessionCurrent().ID
	// The prior code-only revert removes the only snapshot turn above the
	// combined target.
	if _, err := a.ApplyTurnActionForSession(id, 11, TurnActionRevertCode, false); err != nil {
		t.Fatalf("prior code revert: %v", err)
	}
	a.fileTracker.TrackIdentity(path, 0, 0, tool.FileIdentity{Valid: true})
	if !agentTrackerHasRead(a, path) {
		t.Fatal("setup: tracker missing read state")
	}
	blockTurnDir(t, a, 11)

	var boundaries int
	result, err := a.ApplyTurnActionForSessionWithBoundary(id, 11, TurnActionRevertHistory, true, func(_ HydrationState, _ []snapshot.SkippedRevert, _ string, _ *snapshot.CommittedMutationError, _ *string) {
		boundaries++
	})
	if err == nil {
		t.Fatal("first history deletion failure reported success")
	}
	if !strings.Contains(err.Error(), "turn 11") {
		t.Fatalf("revert error = %q, want it to name turn 11 where removal stopped", err.Error())
	}
	if len(result.RestoredFiles) != 0 || len(result.SkippedFiles) != 0 {
		t.Fatalf("combined result = restored %v skipped %v, want empty (no-op code phase)", result.RestoredFiles, result.SkippedFiles)
	}
	if boundaries != 0 {
		t.Fatalf("boundaries emitted = %d, want 0 (no-op code phase, no history mutation)", boundaries)
	}
	tr := a.transcriptForSessionID(id)
	tr.seqMu.Lock()
	epoch := tr.rewriteEpoch
	tr.seqMu.Unlock()
	if epoch != 0 {
		t.Fatalf("rewriteEpoch = %d, want 0", epoch)
	}
	if !agentTrackerHasRead(a, path) {
		t.Fatal("tracker reset by a zero-mutation combined revert")
	}
}

// TestCombinedRevertSkippedCodePhaseFirstRemovalFailureIsPrecommit: the code
// phase skips every file (the current content diverged from the recorded
// post-write identity), so no file was mutated (codeChanged=false); the first
// history deletion failure returns as a precommit failure carrying the exact
// skips, with no boundary, no epoch advance, and no tracker reset.
func TestCombinedRevertSkippedCodePhaseFirstRemovalFailureIsPrecommit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	path := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendUserTurnWithSnapshot(t, a, "modify", path, "v2")
	// Divergence: the file changed since the recorded post-write identity, so
	// the code phase skips it.
	if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := a.SessionCurrent().ID
	a.fileTracker.TrackIdentity(path, 0, 0, tool.FileIdentity{Valid: true})
	if !agentTrackerHasRead(a, path) {
		t.Fatal("setup: tracker missing read state")
	}
	blockTurnDir(t, a, 11)

	var boundaries int
	result, err := a.ApplyTurnActionForSessionWithBoundary(id, 11, TurnActionRevertHistory, true, func(_ HydrationState, _ []snapshot.SkippedRevert, _ string, _ *snapshot.CommittedMutationError, _ *string) {
		boundaries++
	})
	if err == nil {
		t.Fatal("first history deletion failure reported success")
	}
	if len(result.RestoredFiles) != 0 {
		t.Fatalf("RestoredFiles = %v, want none", result.RestoredFiles)
	}
	if len(result.SkippedFiles) != 1 || result.SkippedFiles[0].Path != path {
		t.Fatalf("SkippedFiles = %+v, want exactly the diverged file", result.SkippedFiles)
	}
	if boundaries != 0 {
		t.Fatalf("boundaries emitted = %d, want 0 (skipped-only code phase, no history mutation)", boundaries)
	}
	tr := a.transcriptForSessionID(id)
	tr.seqMu.Lock()
	epoch := tr.rewriteEpoch
	tr.seqMu.Unlock()
	if epoch != 0 {
		t.Fatalf("rewriteEpoch = %d, want 0", epoch)
	}
	if !agentTrackerHasRead(a, path) {
		t.Fatal("tracker reset by a zero-mutation combined revert")
	}
}

// TestCombinedRevertRestoredCodePhaseFirstRemovalFailureReconciles: the code
// phase actually restored a file (codeChanged=true), so the first history
// deletion failure keeps the reconciled treatment — tracker reset, epoch
// advance, warning, and boundary.
func TestCombinedRevertRestoredCodePhaseFirstRemovalFailureReconciles(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	seedCompleteTurns(t, a, 10)
	path := filepath.Join(a.projectRoot, "tracked.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendUserTurnWithSnapshot(t, a, "modify", path, "v2")
	id := a.SessionCurrent().ID
	a.fileTracker.TrackIdentity(path, 0, 0, tool.FileIdentity{Valid: true})
	if !agentTrackerHasRead(a, path) {
		t.Fatal("setup: tracker missing read state")
	}
	blockTurnDir(t, a, 11)

	var boundaries int
	var warning string
	result, err := a.ApplyTurnActionForSessionWithBoundary(id, 11, TurnActionRevertHistory, true, func(_ HydrationState, _ []snapshot.SkippedRevert, w string, _ *snapshot.CommittedMutationError, _ *string) {
		boundaries++
		warning = w
	})
	if err == nil {
		t.Fatal("first history deletion failure reported success")
	}
	if len(result.RestoredFiles) != 1 || result.RestoredFiles[0] != path {
		t.Fatalf("RestoredFiles = %v, want exactly the restored file", result.RestoredFiles)
	}
	if boundaries != 1 {
		t.Fatalf("boundaries emitted = %d, want 1 (reconciled)", boundaries)
	}
	if warning == "" {
		t.Fatal("reconciled boundary carries no walk warning")
	}
	tr := a.transcriptForSessionID(id)
	tr.seqMu.Lock()
	epoch := tr.rewriteEpoch
	tr.seqMu.Unlock()
	if epoch != 1 {
		t.Fatalf("rewriteEpoch = %d, want 1", epoch)
	}
	if agentTrackerHasRead(a, path) {
		t.Fatal("tracker not reset by the reconciled combined revert")
	}
}

// TestRevertWalkFailureBelowCompactionBoundaryRendersSurvivors asserts the
// record is gone and the survivors render in full whichever removal fails:
// the record's removal precedes the walk, so no summary survives even when
// the walk stops early or removes nothing.
func TestRevertWalkFailureBelowCompactionBoundaryRendersSurvivors(t *testing.T) {
	for _, failTurn := range []int{7, 10} {
		t.Run(fmt.Sprintf("fail_at_%d", failTurn), func(t *testing.T) {
			a := newCatalogBackedTestAgent(t)
			seedCompleteTurns(t, a, 10)
			id := a.SessionCurrent().ID
			if err := a.store.SaveCompaction(snapshot.CompactionRecord{BoundaryTurn: 10, Summary: "summary of turns 1-10"}); err != nil {
				t.Fatalf("SaveCompaction: %v", err)
			}
			blockTurnDir(t, a, failTurn)

			var got []HydrationState
			_, err := a.ApplyTurnActionForSessionWithBoundary(id, 6, TurnActionRevertHistory, false, func(hs HydrationState, _ []snapshot.SkippedRevert, _ string, _ *snapshot.CommittedMutationError, _ *string) {
				got = append(got, hs)
			})
			if err == nil {
				t.Fatal("blocked removal reported success")
			}
			if !strings.Contains(err.Error(), strconv.Itoa(failTurn)) {
				t.Fatalf("revert error = %q, want it to name turn %d where removal stopped", err.Error(), failTurn)
			}
			if _, err := os.Stat(filepath.Join(a.store.Dir(), "compaction.json")); !os.IsNotExist(err) {
				t.Fatalf("compaction.json = %v, want it gone: the record's removal precedes the walk", err)
			}
			if len(got) != 1 {
				t.Fatalf("boundary emitted %d times, want exactly 1", len(got))
			}
			var want []string
			for i := 1; i <= failTurn; i++ {
				want = append(want, fmt.Sprintf("turn %d", i))
			}
			if c := userContents(got[0].Messages); !equalStrings(c, want) {
				t.Fatalf("boundary messages = %q, want turns 1-%d in full with no summary", c, failTurn)
			}
		})
	}
}
