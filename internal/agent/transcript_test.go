package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/prompt"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

func feedTranscriptEvents(tr *transcript, events []Event) {
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	for _, ev := range events {
		tr.appendEventLocked(ev)
	}
}

// newLiveCatalogBackedTestAgent returns an agent whose current session is a
// registered live session, so coordinator lookups resolve through the runtime's
// transcriptState registry. Coordinator tests that operate on the current
// session use this rather than reaching a coordinator field on the unit.
func newLiveCatalogBackedTestAgent(t *testing.T) *Agent {
	t.Helper()
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return a
}

func transcriptTailRows(tr *transcript) []transcriptRow {
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	return projectDisplay(tr.tailMessagesLocked())
}

// TestTranscriptCoordinatorProjectionMatchesEventFold verifies the coordinator's
// incremental retained tail is byte-identical to the batch event projection: text
// deltas coalesce per contiguous span, tool start/end collapse to one row, and
// user/system/background rows map one to one. projectEvents is the reference fold
// already asserted equal to the durable display projection elsewhere.
func TestTranscriptCoordinatorProjectionMatchesEventFold(t *testing.T) {
	events := []Event{
		{Kind: EventTurnStart, Turn: 1},
		{Kind: EventTextDelta, Result: "Hello "},
		{Kind: EventTextDelta, Result: "world"},
		{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "read_file", Args: `{"path":"a"}`},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "ok"},
		{Kind: EventTextDelta, Result: "after tool"},
		{Kind: EventGenericSystemSignal, Result: "a signal"},
		{Kind: EventBackgroundProcessComplete, Result: "bg done", BackgroundProcess: &BackgroundProcessDisplay{
			ID: "bg1", Command: "sleep 1", Reason: "completed", Output: "out", ExitCode: 0,
		}},
		{Kind: EventTurnEnd, Turn: 1},
		{Kind: EventUserMessageDisplay, Result: "next question", Turn: 2},
		{Kind: EventTurnStart, Turn: 2},
		{Kind: EventTextDelta, Result: "reply"},
		{Kind: EventTurnEnd, Turn: 2},
	}
	tr := newTranscript()
	feedTranscriptEvents(tr, events)

	got := transcriptTailRows(tr)
	want := projectEvents(events)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coordinator tail projection != event fold\n got: %#v\nwant: %#v", got, want)
	}
}

// TestTranscriptCoordinatorPerDeltaSequence verifies each streamed text delta
// advances the sequence and returns it, while the coalesced tail row carries its
// latest delta's sequence — so a delta delivered after a capture gates as new even
// though the tail still holds one coalesced row.
func TestTranscriptCoordinatorPerDeltaSequence(t *testing.T) {
	tr := newTranscript()
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()

	tr.appendEventLocked(Event{Kind: EventTurnStart, Turn: 1})
	s1 := tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "a"})
	s2 := tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "b"})
	s3 := tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "c"})

	if s1 == 0 || s2 != s1+1 || s3 != s2+1 {
		t.Fatalf("delta sequences = %d,%d,%d, want three consecutive nonzero", s1, s2, s3)
	}
	if len(tr.tail) != 1 {
		t.Fatalf("tail rows = %d, want 1 coalesced row", len(tr.tail))
	}
	if tr.tail[0].msg.Content != "abc" {
		t.Fatalf("coalesced content = %q, want %q", tr.tail[0].msg.Content, "abc")
	}
	if tr.tail[0].seq != s3 {
		t.Fatalf("coalesced row seq = %d, want last delta seq %d", tr.tail[0].seq, s3)
	}
}

// TestTranscriptCoordinatorToolEndKeepsRowSequence verifies a tool end updates its
// row in place and returns that row's original sequence, so an id-keyed result
// update is never re-sequenced and tail sequences stay monotonic even when a tool
// completes after a later row was appended.
func TestTranscriptCoordinatorToolEndKeepsRowSequence(t *testing.T) {
	tr := newTranscript()
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()

	startSeq := tr.appendEventLocked(Event{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "read_file"})
	userSeq := tr.appendEventLocked(Event{Kind: EventUserMessageDisplay, Result: "q", Turn: 1})
	endSeq := tr.appendEventLocked(Event{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "ok"})

	if endSeq != startSeq {
		t.Fatalf("tool end seq = %d, want the tool row's start seq %d", endSeq, startSeq)
	}
	if userSeq <= startSeq {
		t.Fatalf("later row seq %d must exceed the tool row seq %d", userSeq, startSeq)
	}
	if len(tr.tail) != 2 || tr.tail[0].seq != startSeq || !tr.tail[0].msg.Done {
		t.Fatalf("tool row = %#v, want seq %d done in place", tr.tail[0], startSeq)
	}
}

// TestTranscriptCoordinatorStagedToolLastEndWins verifies a second tool end for the
// same call overwrites the first in place, without adding a row or a sequence.
func TestTranscriptCoordinatorStagedToolLastEndWins(t *testing.T) {
	tr := newTranscript()
	feedTranscriptEvents(tr, []Event{
		{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "apply_patch"},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "Staged."},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "Applied.", ToolName: "apply_patch"},
	})
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	if len(tr.tail) != 1 {
		t.Fatalf("tail rows = %d, want 1 (one tool row)", len(tr.tail))
	}
	if got := tr.tail[0].msg.Result; got != "Applied." {
		t.Fatalf("tool result = %q, want last end %q", got, "Applied.")
	}
	if tr.nextSeq != 2 {
		t.Fatalf("nextSeq = %d, want 2 (one sequence assigned)", tr.nextSeq)
	}
}

// TestTranscriptCoordinatorToolMetadataAndSubagentLinks verifies a successful tool
// end carries its display metadata and derived subagent-session links into the
// live tail (matching the durable projection), and a failing final end clears them.
func TestTranscriptCoordinatorToolMetadataAndSubagentLinks(t *testing.T) {
	meta := map[string]any{
		"edit_preview_files":   []any{"a.go"},
		"subagent_session_ids": []SubagentSessionLink{{Index: 0, SessionID: "child-1"}},
	}
	tr := newTranscript()
	feedTranscriptEvents(tr, []Event{
		{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "task"},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "done", Metadata: meta},
	})
	tr.seqMu.Lock()
	row := tr.tail[0].msg
	tr.seqMu.Unlock()
	if !reflect.DeepEqual(row.Metadata, meta) {
		t.Fatalf("tool metadata not preserved: %#v", row.Metadata)
	}
	if want := []SubagentSessionLink{{Index: 0, SessionID: "child-1"}}; !reflect.DeepEqual(row.SubagentSessionIDs, want) {
		t.Fatalf("subagent links = %#v, want %#v", row.SubagentSessionIDs, want)
	}

	// A failing final end drops metadata and links: the durable projection keeps
	// them only for a successful result, so the last-end-wins tail must too.
	tr2 := newTranscript()
	feedTranscriptEvents(tr2, []Event{
		{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "task"},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "staged", Metadata: meta},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "boom", IsError: true},
	})
	tr2.seqMu.Lock()
	row2 := tr2.tail[0].msg
	tr2.seqMu.Unlock()
	if row2.Metadata != nil || row2.SubagentSessionIDs != nil {
		t.Fatalf("failed end kept metadata/links: meta=%#v links=%#v", row2.Metadata, row2.SubagentSessionIDs)
	}
}

// TestTranscriptRegistryCursorsAdvanceIndependently verifies the step-12
// registry property: each live session's coordinator lives in its own registry
// entry with its own seqMu, so feeding or committing one session never moves
// another's {nextSeq, committedSeq, committedTurn}, and a feed to one session
// never blocks on another session's coordinator lock. Mutation check: key the
// registry so both sessions share one entry; this test must fail.
func TestTranscriptRegistryCursorsAdvanceIndependently(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}

	trA := a.transcriptForSessionID(firstID)
	trB := a.transcriptForSessionID(secondID)
	if trA == nil || trB == nil {
		t.Fatal("live sessions have no registry entries")
	}
	if trA == trB {
		t.Fatal("live sessions share one coordinator")
	}

	nextSeq := func(tr *transcript) int {
		tr.seqMu.Lock()
		defer tr.seqMu.Unlock()
		return tr.nextSeq
	}

	// Interleaved feed: A's rows advance A only; B's cursor stays put. Turn
	// start consumes no sequence; each text delta consumes one.
	seqA0, seqB0 := nextSeq(trA), nextSeq(trB)
	feedTranscript(trA, Event{Kind: EventTurnStart, Turn: 1})
	feedTranscript(trA, Event{Kind: EventTextDelta, Result: "a1"})
	feedTranscript(trA, Event{Kind: EventTextDelta, Result: "a2"})
	if got := nextSeq(trA); got != seqA0+2 {
		t.Fatalf("session A nextSeq = %d, want %d", got, seqA0+2)
	}
	if got := nextSeq(trB); got != seqB0 {
		t.Fatalf("feeding session A advanced session B's cursor: %d -> %d", seqB0, got)
	}

	feedTranscript(trB, Event{Kind: EventTurnStart, Turn: 1})
	feedTranscript(trB, Event{Kind: EventTextDelta, Result: "b1"})
	if got := nextSeq(trB); got != seqB0+1 {
		t.Fatalf("session B nextSeq = %d, want %d", got, seqB0+1)
	}
	if got := nextSeq(trA); got != seqA0+2 {
		t.Fatalf("feeding session B advanced session A's cursor: %d -> %d", seqA0+2, got)
	}

	// Commits are per-session too: committing A's turn moves only A's markers.
	trA.seqMu.Lock()
	trA.commitLocked(1)
	revA := trA.revisionLocked()
	trA.seqMu.Unlock()
	trB.seqMu.Lock()
	revB := trB.revisionLocked()
	trB.seqMu.Unlock()
	if revA.committedTurn != 1 || revA.committedSeq == 0 {
		t.Fatalf("session A revision after commit = %+v, want committedTurn 1 with a nonzero committedSeq", revA)
	}
	if revB != (captureRevision{}) {
		t.Fatalf("committing session A advanced session B's committed markers: %+v", revB)
	}

	// A feed to B must not need A's lock: holding A's coordinator lock does not
	// stall B's feed, which is exactly what a registry-wide hold would do.
	trA.seqMu.Lock()
	fedB := make(chan int, 1)
	go func() {
		fedB <- feedTranscript(trB, Event{Kind: EventTextDelta, Result: "b-concurrent"})
	}()
	select {
	case <-fedB:
	case <-time.After(2 * time.Second):
		t.Fatal("feed to session B blocked on session A's coordinator lock: one lock guards both sessions' feeds")
	}
	trA.seqMu.Unlock()
}

// TestCaptureStateForSelectionRevalidation verifies the live-selection capture
// revalidates the coordinator revision across a fixed three attempts: a stable
// revision succeeds on the first read, a commit or rewrite during the read is
// retried once and then succeeds, three consecutive changes exhaust the attempts
// with a retryable error, and a durable read error returns immediately with no
// further attempt.
func TestCaptureStateForSelectionRevalidation(t *testing.T) {
	bumpCommit := func(tr *transcript) {
		tr.seqMu.Lock()
		tr.commitLocked(tr.committedTurn + 1)
		tr.seqMu.Unlock()
	}

	t.Run("stable_first", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		attempts := 0
		a.captureProbe = func(int) error { attempts++; return nil }
		if _, err := a.captureStateForSelection(a.session, nil); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (no retry)", attempts)
		}
	})

	t.Run("normal_commit_then_success", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		tr := a.transcriptForSessionID(sessionIDOf(a.session))
		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			if attempt == 1 {
				bumpCommit(tr)
			}
			return nil
		}
		if _, err := a.captureStateForSelection(a.session, nil); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2 (one retry then success)", attempts)
		}
	})

	t.Run("rewrite_then_success", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			if attempt == 1 {
				// Drive the production compaction rewrite path: it advances the
				// epoch, so the racing capture must revalidate and retry rather
				// than publish the pre-compaction prefix. Boundary 0 compacts
				// nothing and no errors are retained, so the pruning is a no-op.
				a.publishCompactionRewrite(unit, sessionIDOf(unit), unit.projectID, 0, SessionSummary{}, nil)
			}
			return nil
		}
		if _, err := a.captureStateForSelection(a.session, nil); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2 (production compaction bumped the epoch once)", attempts)
		}
	})

	t.Run("three_mismatches", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		tr := a.transcriptForSessionID(sessionIDOf(a.session))
		attempts := 0
		a.captureProbe = func(int) error {
			attempts++
			bumpCommit(tr)
			return nil
		}
		_, err := a.captureStateForSelection(a.session, nil)
		if !errors.Is(err, errCaptureRevisionChanged) {
			t.Fatalf("err = %v, want errCaptureRevisionChanged", err)
		}
		if attempts != 3 {
			t.Fatalf("attempts = %d, want 3", attempts)
		}
	})

	t.Run("read_error_attempt_1", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		sentinel := errors.New("read boom")
		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			if attempt == 1 {
				return sentinel
			}
			return nil
		}
		_, err := a.captureStateForSelection(a.session, nil)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel", err)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (no later attempt)", attempts)
		}
	})

	t.Run("read_error_attempt_2_after_mismatch", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		tr := a.transcriptForSessionID(sessionIDOf(a.session))
		sentinel := errors.New("read boom")
		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			switch attempt {
			case 1:
				bumpCommit(tr)
			case 2:
				return sentinel
			}
			return nil
		}
		_, err := a.captureStateForSelection(a.session, nil)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel", err)
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2", attempts)
		}
	})
}

// TestCompleteLiveSessionStateContract verifies the hydration capture shape:
// the revalidating three-attempt read with the durable half bounded by the
// coordinator's committed turn. A compaction or commit landing between the
// durable read and the locked read forces a retry rather than a torn snapshot;
// exhausting the attempts surfaces an error rather than an empty session; and
// a turn whose completion marker is durable while its commit has not run stays
// below the bound, so it is carried by the retained tail alone and renders
// exactly once with an unraised cursor.
//
// Exception, named per the contract-test rule: the bounded decision and the
// revalidation live in captureUnderLocksRTHeld, which is unexported and not
// adapter-facing. The tests enter through the exported HydrateSession, the
// production hydration entry point, and observe the capture through the
// exported HydrationState plus the existing captureProbe and durableReadHook
// test seams.
func TestCompleteLiveSessionStateContract(t *testing.T) {
	bumpCommit := func(tr *transcript) {
		tr.seqMu.Lock()
		tr.commitLocked(tr.committedTurn + 1)
		tr.seqMu.Unlock()
	}

	// A compaction landing between the durable read and the locked revalidation
	// must force a retry rather than publish the torn snapshot: the first read
	// returns the pre-compaction prefix (both turns), the compaction persists a
	// boundary record and advances the rewrite epoch, and the retry's read
	// returns the post-compaction prefix (only the turn after the boundary).
	// Publishing attempt 1's prefix with the post-compaction cursor is the tear;
	// asserting the pre-compaction turn is absent is what catches it. The
	// compaction is driven through the production path: the persisted boundary
	// record plus the rewrite boundary.
	t.Run("shape=B/capture_retry=compaction_during_read", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)
		for _, content := range []string{"pre-compaction", "post-compaction"} {
			turn := appendUserTurn(t, a, content)
			// Commit each turn in the coordinator so the bounded durable read
			// covers it: a marked-but-uncommitted turn stays below the bound.
			feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		}

		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			if attempt == 1 {
				if err := unit.store.SaveCompaction(snapshot.CompactionRecord{BoundaryTurn: 1, Summary: "rewritten"}); err != nil {
					return err
				}
				a.publishCompactionRewrite(unit, id, unit.projectID, 1, SessionSummary{}, nil)
			}
			return nil
		}
		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2 (the compaction in the read-to-lock window forced one retry)", attempts)
		}
		if got := countHydrationContent(hs, "pre-compaction"); got != 0 {
			t.Fatalf("hydrated %q %d times, want 0 (the torn pre-compaction prefix was published)", "pre-compaction", got)
		}
		if got := countHydrationContent(hs, "post-compaction"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (the retry re-read the post-compaction prefix)", "post-compaction", got)
		}
	})

	// A turn committing during the hydration must not be omitted: the first
	// durable read predates the commit, the revision change forces a retry, and
	// the retry's read returns the turn.
	t.Run("shape=B/capture_retry=commit_during_hydration", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)

		turn := unit.store.BeginTurn()
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "commit during hydration"})
		data, err := json.Marshal(message.NewText(message.RoleUser, "commit during hydration"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := unit.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}

		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			if attempt == 1 {
				feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: id, ProjectID: unit.projectID, Turn: turn})
			}
			return nil
		}
		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2 (the commit during the read forced one retry)", attempts)
		}
		if got := countHydrationContent(hs, "commit during hydration"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (the turn committing during hydration is not omitted)", "commit during hydration", got)
		}
	})

	// Exhausting the three attempts surfaces an error rather than an empty
	// session: every adapter applies a zero HydrationState as "no session".
	t.Run("shape=B/capture_read_error=attempts_exhausted", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		tr := a.transcriptForSessionID(sessionIDOf(a.session))
		attempts := 0
		a.captureProbe = func(int) error {
			attempts++
			bumpCommit(tr)
			return nil
		}
		_, err := a.HydrateSession(sessionIDOf(a.session))
		if !errors.Is(err, errCaptureRevisionChanged) {
			t.Fatalf("err = %v, want errCaptureRevisionChanged (the exhausted hydration surfaces an error, not an empty session)", err)
		}
		if attempts != 3 {
			t.Fatalf("attempts = %d, want 3", attempts)
		}
	})

	// The three-attempt bound is a bound on durable reads: one read per
	// attempt, never a fourth. The durableReadHook fires immediately before
	// each capture durable read, and hydration performs no other hook-firing
	// durable read.
	t.Run("shape=B/capture_read_error=at_most_three_durable_reads", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		tr := a.transcriptForSessionID(sessionIDOf(a.session))
		var reads atomic.Int32
		a.durableReadHook = func() { reads.Add(1) }
		defer func() { a.durableReadHook = nil }()
		a.captureProbe = func(int) error {
			bumpCommit(tr)
			return nil
		}
		if _, err := a.HydrateSession(sessionIDOf(a.session)); !errors.Is(err, errCaptureRevisionChanged) {
			t.Fatalf("err = %v, want errCaptureRevisionChanged", err)
		}
		if got := reads.Load(); got != 3 {
			t.Fatalf("durable reads = %d, want exactly 3 (one per attempt, never a fourth)", got)
		}
	})

	// The duplicate window: a turn whose completion marker is durable while its
	// commit has not run is above the committed bound, so the durable half
	// stops before it and the retained tail carries it alone. The fixture
	// builds the window the way a root turn reaches it: rows fed into the
	// tail, the marker on disk, no commit.
	t.Run("shape=B/durable_tail_disjoint=root_turn", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)

		turn := unit.store.BeginTurn()
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		seqUser := feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "root window"})
		seqReply := feedTranscript(tr, Event{Kind: EventTextDelta, SessionID: id, Result: "root reply"})
		for _, m := range []message.Message{
			message.NewText(message.RoleUser, "root window"),
			message.NewText(message.RoleAssistant, "root reply"),
		} {
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := unit.store.AppendMessage(turn, data); err != nil {
				t.Fatalf("AppendMessage: %v", err)
			}
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}

		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		for _, content := range []string{"root window", "root reply"} {
			if got := countHydrationContent(hs, content); got != 1 {
				t.Fatalf("hydrated %q %d times, want exactly once (the marked turn renders from the retained tail alone)", content, got)
			}
		}
		if len(hs.Messages) != 0 {
			t.Fatalf("hydration has %d durable rows, want 0 (the marked turn stays below the committed bound)", len(hs.Messages))
		}
		if seqUser <= 0 || seqReply <= seqUser {
			t.Fatalf("fixture sequences = user %d, reply %d, want reply > user > 0", seqUser, seqReply)
		}
		// The cursor is not raised over the tail: the tail rows themselves seed
		// the frontend's high-water, so the coordinator's own committedSeq is
		// the honest bound.
		if hs.Cursor.CommittedSeq != 0 {
			t.Fatalf("cursor committedSeq = %d, want 0 (the uncommitted bound is not raised)", hs.Cursor.CommittedSeq)
		}
	})

	// The same window as a queued preseed reaches it: the drain feeds the turn
	// start (so curTurn names the persisted turn), the user row lands in the
	// tail, the marker is durable, and the commit has not run.
	t.Run("shape=B/durable_tail_disjoint=queued_preseed", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)

		turn := unit.store.BeginTurn()
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "preseed window"})
		data, err := json.Marshal(message.NewText(message.RoleUser, "preseed window"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := unit.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}

		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if got := countHydrationContent(hs, "preseed window"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (the marked preseed turn renders from the retained tail alone)", "preseed window", got)
		}
		if len(hs.Messages) != 0 {
			t.Fatalf("hydration has %d durable rows, want 0 (the marked preseed turn stays below the committed bound)", len(hs.Messages))
		}
		if hs.Cursor.CommittedSeq != 0 {
			t.Fatalf("cursor committedSeq = %d, want 0 (the uncommitted bound is not raised)", hs.Cursor.CommittedSeq)
		}
	})

	// A session reattached in a fresh process with prior durable turns must
	// show full history: registration adopts the store's highest complete
	// turn, so the bounded read covers the prior turns — and the window turn
	// (marked, uncommitted) must render exactly once from the retained tail.
	t.Run("shape=B/durable_tail_disjoint=reattached_session", func(t *testing.T) {
		home := t.TempDir()
		projectRoot := t.TempDir()

		first := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
		if _, err := first.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		id := first.SessionCurrent().ID
		if id == "" {
			t.Fatal("no current session after NewSession")
		}
		for _, content := range []string{"prior one", "prior two", "prior three"} {
			appendUserTurn(t, first, content)
		}
		// Release the first owner's per-session claim so a fresh owner can
		// reopen the session.
		if _, err := first.store.Close(); err != nil {
			t.Fatalf("release first owner claim: %v", err)
		}

		second := newCatalogBackedTestAgentForRoot(t, home, projectRoot)
		if _, err := second.OpenSession(id); err != nil {
			t.Fatalf("OpenSession: %v", err)
		}
		rt := second.ensureRuntime()
		rt.mu.Lock()
		unit := second.sessions[id]
		rt.mu.Unlock()
		if unit == nil {
			t.Fatal("session not live in the fresh owner")
		}
		tr := second.transcriptForSessionID(id)

		turn := unit.store.BeginTurn()
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "window row"})
		data, err := json.Marshal(message.NewText(message.RoleUser, "window row"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := unit.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}

		hs, err := second.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		for _, content := range []string{"prior one", "prior two", "prior three", "window row"} {
			if got := countHydrationContent(hs, content); got != 1 {
				t.Fatalf("hydrated %q %d times, want exactly once (prior turns from the adopted bound, the window turn from the tail)", content, got)
			}
		}
		if hs.Cursor.CommittedTurn != 3 {
			t.Fatalf("cursor committedTurn = %d, want 3 (registration adopted the highest complete turn)", hs.Cursor.CommittedTurn)
		}
		if hs.Cursor.CommittedSeq != 0 {
			t.Fatalf("cursor committedSeq = %d, want 0 (a fresh coordinator has committed nothing)", hs.Cursor.CommittedSeq)
		}
	})

	// A turn whose only content is a system signal renders rows carrying no
	// Turn. The durable bound is applied to the raw turn records, not the
	// rendered rows, so the marked turn stays below the bound and the signal
	// renders exactly once from the retained tail.
	t.Run("shape=B/durable_tail_disjoint=no_titled_row", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)

		turn := unit.store.BeginTurn()
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventGenericSystemSignal, SessionID: id, Result: "background finished"})
		data, err := json.Marshal(loop.NewSystemSignalMessage("background finished"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := unit.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}

		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if got := countHydrationContent(hs, "System: background finished"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (a turn rendering no titled row still renders once from the tail)", "System: background finished", got)
		}
		if hs.Cursor.CommittedSeq != 0 {
			t.Fatalf("cursor committedSeq = %d, want 0 (the uncommitted bound is not raised)", hs.Cursor.CommittedSeq)
		}
	})

	// The lower bound: a row appended after the commit carries the same curTurn
	// but is not durable, so it stays in the retained tail. The committed turn
	// renders from the durable half and the post-commit row from the tail —
	// each exactly once, because the bound never drops a row that is not
	// durably covered.
	t.Run("shape=B/durable_tail_disjoint=post_commit_tail_kept", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)

		turn := unit.store.BeginTurn()
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "committed row"})
		data, err := json.Marshal(message.NewText(message.RoleUser, "committed row"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := unit.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}
		feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventBackgroundProcessComplete, SessionID: id, Result: "bg done", BackgroundProcess: &BackgroundProcessDisplay{
			ID: "bg1", Command: "sleep 1", Reason: "completed", Output: "out", ExitCode: 0,
		}})

		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if got := countHydrationContent(hs, "committed row"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (committed turns render from the durable half only)", "committed row", got)
		}
		// Background-process rows carry their text in Result, not Content, so
		// count the tail directly.
		bgCount := 0
		for _, r := range hs.Tail {
			if r.Message.Result == "bg done" {
				bgCount++
			}
		}
		if bgCount != 1 {
			t.Fatalf("hydrated background row %d times, want exactly once (a row appended after the commit is kept in the tail)", bgCount)
		}
	})

	// A child's transcript hydrates without duplication once per exit path.
	// Every subagent outcome funnels through the single exit closure that
	// closes the forwarding channel, joins the forwarder, waits for the loop
	// drainer to acknowledge the flush, commits the child's turn, and only then
	// removes its registry entry — so by the time the parent turn ends (which
	// the task tool's completion precedes), the child is committed and its
	// entry is gone. The hydration then takes the completed-child route and
	// must return the full durable transcript, not an error and not an empty
	// view, with every row exactly once.
	t.Run("shape=B/child_exit_path=normal", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch calls.Add(1) {
			case 1:
				writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"child normal","subagent_type":"explore"}]}`)
			case 2:
				writeTextResponse(w, "CHILD_NORMAL_DONE")
			case 3:
				writeTextResponse(w, "PARENT_DONE")
			default:
				t.Fatalf("unexpected provider call")
			}
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.Submit(ctx, "delegate normal"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		childID := findSubagentStart(t, cap).SubagentSessionID

		if tr := a.transcriptForSessionID(childID); tr != nil {
			t.Fatal("child registry entry still present after the run finished")
		}
		hs, err := a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(completed child): %v", err)
		}
		if len(hs.Messages) == 0 {
			t.Fatal("completed child hydration returned an empty transcript")
		}
		for _, content := range []string{"child normal", "CHILD_NORMAL_DONE"} {
			if got := countHydrationContent(hs, content); got != 1 {
				t.Fatalf("hydrated %q %d times, want exactly once (child rows must not duplicate across exit paths)", content, got)
			}
		}
		if len(hs.Tail) != 0 || len(hs.Errors) != 0 {
			t.Fatalf("completed child hydration has live rows: tail=%d errors=%d", len(hs.Tail), len(hs.Errors))
		}
	})

	// Denied: the child's tool is denied, the loop returns "Tool denied by
	// user.", and the exit closure still commits the child's turn.
	t.Run("shape=B/child_exit_path=denied", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch calls.Add(1) {
			case 1:
				writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"child denied","subagent_type":"explore"}]}`)
			case 2:
				writeTaskToolCallResponse(w, "call_read", "read_file", `{"path":"target.txt"}`)
			case 3:
				writeTextResponse(w, "PARENT_DONE")
			default:
				t.Fatalf("unexpected provider call")
			}
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		// The live unit's permission policy captures a.cfg at build time, so
		// the deny rule must land on the config the policy holds.
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		a.cfg.Permissions.Deny = []string{"read_file(/**)"}
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.Submit(ctx, "delegate denied"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		childID := findSubagentStart(t, cap).SubagentSessionID

		hs, err := a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(denied child): %v", err)
		}
		if got := countHydrationContent(hs, "child denied"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (a denied child still commits its turn)", "child denied", got)
		}
		if len(hs.Messages) == 0 {
			t.Fatal("denied child hydration returned an empty transcript")
		}
		if len(hs.Tail) != 0 {
			t.Fatalf("denied child hydration has %d live rows, want none", len(hs.Tail))
		}
	})

	// Errored: the child's provider call fails, the loop returns an error, and
	// the exit closure still commits whatever the turn persisted.
	t.Run("shape=B/child_exit_path=errored", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch calls.Add(1) {
			case 1:
				writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"child errored","subagent_type":"explore"}]}`)
			case 2:
				// A non-retryable status so the child's loop fails on the
				// first attempt instead of retrying.
				http.Error(w, "boom", http.StatusBadRequest)
			case 3:
				writeTextResponse(w, "PARENT_DONE")
			default:
				t.Fatalf("unexpected provider call")
			}
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.Submit(ctx, "delegate errored"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		childID := findSubagentStart(t, cap).SubagentSessionID

		hs, err := a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(errored child): %v", err)
		}
		if got := countHydrationContent(hs, "child errored"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (an errored child still commits its turn)", "child errored", got)
		}
		if len(hs.Messages) == 0 {
			t.Fatal("errored child hydration returned an empty transcript")
		}
		if len(hs.Tail) != 0 {
			t.Fatalf("errored child hydration has %d live rows, want none", len(hs.Tail))
		}
	})

	// Cancelled: an ordinary Stop/Escape cancels the parent turn, which is the
	// child's turn context. The child returns cleanly — cancellation is not an
	// error — and the exit closure still commits; the interrupted signal is
	// part of the durable turn.
	t.Run("shape=B/child_exit_path=cancelled", func(t *testing.T) {
		childCallStarted := make(chan struct{})
		release := make(chan struct{})
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch calls.Add(1) {
			case 1:
				writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"child cancelled","subagent_type":"explore"}]}`)
			case 2:
				close(childCallStarted)
				select {
				case <-release:
				case <-r.Context().Done():
				}
				// The client cancelling a request with a body does not
				// necessarily reach this handler, and writing to an abandoned
				// connection can block forever; the response is irrelevant —
				// the child was cancelled — so return silently.
			case 3:
				writeTextResponse(w, "PARENT_DONE")
			default:
				t.Fatalf("unexpected provider call")
			}
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.Submit(ctx, "delegate cancelled"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		<-childCallStarted
		parentID := a.SessionCurrent().ID
		if err := a.CancelSession(parentID); err != nil {
			t.Fatalf("CancelSession: %v", err)
		}
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		// The parent turn end fires only after the task tool returned, which
		// is after the child's exit closure committed; release the blocked
		// handler so the server can close.
		close(release)
		childID := findSubagentStart(t, cap).SubagentSessionID

		hs, err := a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(cancelled child): %v", err)
		}
		if got := countHydrationContent(hs, "child cancelled"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (a cancelled child still commits its turn)", "child cancelled", got)
		}
		if got := countHydrationContent(hs, "System: Request interrupted by user"); got != 1 {
			t.Fatalf("hydrated interrupted signal %d times, want exactly once", got)
		}
		if len(hs.Tail) != 0 {
			t.Fatalf("cancelled child hydration has %d live rows, want none", len(hs.Tail))
		}
	})

	// A live child hydrates from its registry entry: the durable prefix read
	// through the entry's store plus the retained tail and cursor — the same
	// revalidating shape a root uses. After the run finishes and the entry is
	// removed, the same id resolves from its durable store alone.
	t.Run("shape=B/child=live_then_completed", func(t *testing.T) {
		childCallStarted := make(chan struct{})
		release := make(chan struct{})
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch calls.Add(1) {
			case 1:
				writeTaskToolCallResponse(w, "call_task", "task", `{"tasks":[{"prompt":"child live","subagent_type":"explore"}]}`)
			case 2:
				close(childCallStarted)
				select {
				case <-release:
				case <-r.Context().Done():
				}
				writeTextResponse(w, "CHILD_LIVE_DONE")
			case 3:
				writeTextResponse(w, "PARENT_DONE")
			default:
				t.Fatalf("unexpected provider call")
			}
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		if _, err := a.Submit(ctx, "delegate live"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		var childID string
		for {
			for _, ev := range cap.snapshot() {
				if ev.Kind == EventSubagentStart {
					childID = ev.SubagentSessionID
				}
			}
			if childID != "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("missing subagent start event: %#v", cap.snapshot())
			}
			time.Sleep(5 * time.Millisecond)
		}
		// The child's user-display row is sequenced into its coordinator's
		// tail in the same section that delivers the event, so observing the
		// event means the tail holds the row when the hydration reads it.
		deadline = time.Now().Add(5 * time.Second)
		for {
			found := false
			for _, ev := range cap.snapshot() {
				if ev.Kind == EventUserMessageDisplay && ev.SubagentSessionID == childID && ev.Result == "child live" {
					found = true
				}
			}
			if found {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("did not observe the child user display in time: %#v", cap.snapshot())
			}
			time.Sleep(5 * time.Millisecond)
		}

		hs, err := a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(live child): %v", err)
		}
		if got := countHydrationContent(hs, "child live"); got != 1 {
			t.Fatalf("hydrated live child %q %d times, want exactly once (the live route returns the tail row once)", "child live", got)
		}
		if len(hs.Messages) != 0 {
			t.Fatalf("live child hydration has %d durable rows, want none (the turn is not complete yet)", len(hs.Messages))
		}
		if len(hs.Tail) == 0 {
			t.Fatal("live child hydration has no retained tail")
		}
		if hs.Cursor.CommittedTurn != 0 {
			t.Fatalf("live child cursor committedTurn = %d, want 0 (nothing committed yet)", hs.Cursor.CommittedTurn)
		}

		close(release)
		waitUntilEventOrderTurnEndCount(t, cap, 1)
		if tr := a.transcriptForSessionID(childID); tr != nil {
			t.Fatal("child registry entry still present after the run finished")
		}
		hs, err = a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(completed child): %v", err)
		}
		for _, content := range []string{"child live", "CHILD_LIVE_DONE"} {
			if got := countHydrationContent(hs, content); got != 1 {
				t.Fatalf("hydrated %q %d times, want exactly once (the completed route returns the durable turn once)", content, got)
			}
		}
		if len(hs.Tail) != 0 {
			t.Fatalf("completed child hydration has %d live rows, want none", len(hs.Tail))
		}
	})
}

// TestLiveHydrationUsesCommittedTurn verifies the coordinator's committedTurn
// is the sole durable visibility bound for a live cursor-bearing hydration: a
// turn whose completion marker is durable while its commit has not run appears
// only through the retained tail/live events, never through the durable half;
// after the coordinator commits, the bounded read covers it exactly once; a
// fresh coordinator registration adopts the store's highest complete turn
// (never its current, possibly in-flight, turn); and the three-attempt
// revision revalidation never issues a fourth durable read.
//
// Exception, named per the contract-test rule: the bounded read and the
// registration seeding are unexported; the tests enter through the exported
// HydrateSession and observe the exported HydrationState plus the existing
// captureProbe and durableReadHook test seams.
func TestLiveHydrationUsesCommittedTurn(t *testing.T) {
	bumpCommit := func(tr *transcript) {
		tr.seqMu.Lock()
		tr.commitLocked(tr.committedTurn + 1)
		tr.seqMu.Unlock()
	}

	// Zero-tail window: the marker is durable, the commit has not run, and no
	// event has been dispatched yet, so the retained tail is empty. The turn
	// must stay invisible until the coordinator commits; the dispatched
	// turn-end then commits it and the bounded read returns it exactly once.
	// The unbounded-helper mutation makes the pre-commit read see the turn on
	// disk and fails this subtest.
	t.Run("marked_turn_zero_tail_invisible_until_commit", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)

		turn := unit.store.BeginTurn()
		data, err := json.Marshal(message.NewText(message.RoleUser, "zero tail"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := unit.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}

		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if got := countHydrationContent(hs, "zero tail"); got != 0 {
			t.Fatalf("pre-commit hydration returns %q %d times, want 0 (a marked turn is invisible until the coordinator commits)", "zero tail", got)
		}

		// The drain dispatches the turn's events and commits it.
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "zero tail"})
		feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: id, ProjectID: unit.projectID, Turn: turn})

		hs, err = a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession after commit: %v", err)
		}
		if got := countHydrationContent(hs, "zero tail"); got != 1 {
			t.Fatalf("post-commit hydration returns %q %d times, want exactly once (the committed turn is durable through the bound)", "zero tail", got)
		}
		if len(hs.Tail) != 0 {
			t.Fatalf("post-commit hydration has %d tail rows, want none (the commit cleared the tail)", len(hs.Tail))
		}
		if hs.Cursor.CommittedTurn != turn {
			t.Fatalf("cursor committedTurn = %d, want %d", hs.Cursor.CommittedTurn, turn)
		}
		if hs.Cursor.CommittedSeq == 0 {
			t.Fatal("cursor committedSeq = 0, want the committed row's sequence (committedSeq suppresses replay)")
		}
	})

	// Partial-tail window: the marked turn's rows are also in the retained
	// tail. The bounded read stops before the turn, so the snapshot renders
	// the rows from the tail alone — never from the durable half — exactly
	// once. The unbounded-helper mutation duplicates the rows (durable half
	// plus tail) and fails this subtest.
	t.Run("marked_turn_partial_tail_renders_once", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)

		turn := unit.store.BeginTurn()
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "partial tail"})
		data, err := json.Marshal(message.NewText(message.RoleUser, "partial tail"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := unit.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}

		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if len(hs.Messages) != 0 {
			t.Fatalf("hydration has %d durable rows, want 0 (the marked turn stays below the committed bound)", len(hs.Messages))
		}
		if got := countHydrationContent(hs, "partial tail"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (the tail carries the uncommitted row)", "partial tail", got)
		}
	})

	// Registration during a begun incomplete turn: the fresh coordinator must
	// adopt the store's highest COMPLETE turn (zero here), never the current
	// in-flight turn. Seeding from CurrentTurn would adopt the begun turn and
	// expose its durably marked rows through the bounded read too early — the
	// mutation this subtest catches.
	t.Run("registration_adopts_highest_complete_turn", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)

		// Begin an incomplete turn, then re-register the live id while it is
		// begun but has no completion marker: the fresh coordinator must adopt
		// the complete-turn bound (0), not the current (begun) turn (1).
		turn := unit.store.BeginTurn()
		rt := a.ensureRuntime()
		rt.unregisterTranscript(id)
		rt.registerTranscript(id, unit.store)
		tr := a.transcriptForSessionID(id)

		// Build the marked-uncommitted window after the registration: rows in
		// the tail, the marker on disk, the commit not run.
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "begun turn"})
		data, err := json.Marshal(message.NewText(message.RoleUser, "begun turn"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := unit.store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}

		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if hs.Cursor.CommittedTurn != 0 {
			t.Fatalf("seeded committedTurn = %d, want 0 (a begun incomplete turn must not be adopted as committed)", hs.Cursor.CommittedTurn)
		}
		if len(hs.Messages) != 0 {
			t.Fatalf("hydration has %d durable rows, want 0 (the begun turn must not be exposed through the durable half before its commit)", len(hs.Messages))
		}
		if got := countHydrationContent(hs, "begun turn"); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (the tail carries the uncommitted row)", "begun turn", got)
		}
	})

	// The three-attempt bound is a bound on durable reads: a revision change
	// on every attempt exhausts the retries with a typed failure and never
	// issues a fourth read.
	t.Run("revision_change_all_three_attempts_no_fourth_read", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		tr := a.transcriptForSessionID(sessionIDOf(a.session))
		var reads atomic.Int32
		a.durableReadHook = func() { reads.Add(1) }
		defer func() { a.durableReadHook = nil }()
		a.captureProbe = func(int) error {
			bumpCommit(tr)
			return nil
		}
		if _, err := a.HydrateSession(sessionIDOf(a.session)); !errors.Is(err, errCaptureRevisionChanged) {
			t.Fatalf("err = %v, want errCaptureRevisionChanged", err)
		}
		if got := reads.Load(); got != 3 {
			t.Fatalf("durable reads = %d, want exactly 3 (one per attempt, never a fourth)", got)
		}
	})
}

// TestLiveChildHydrationUsesCommittedTurn extends the committed-turn contract
// to the live child route across the mark-commit-unregister lifecycle. The
// child's turn is durably marked while the coordinator has not committed (no
// EventTurnEnd) and the registry entry is still live: before the commit the
// durable rows stay excluded (zero-tail renders nothing, partial-tail renders
// once from the retained tail); after the coordinator commits while the entry
// is still registered the durable rows render once with a suppression cursor
// (committedSeq covers the row, so replayed frames gate); after unregister the
// completed-child point lookup remains complete. No production seam is added:
// the tests enter through the exported HydrateSession with the child registry
// entry and coordinator fed through the production registration and the shared
// feed helper, exactly as the subagent run does.
func TestLiveChildHydrationUsesCommittedTurn(t *testing.T) {
	// newLiveChildFixture builds a live child over the agent's project: a
	// fresh child store with a registered transcript entry, mirroring the
	// subagent run's registration. The cleanup unregisters the entry and
	// detaches the store.
	newLiveChildFixture := func(t *testing.T) (*Agent, *snapshot.Store, string, *transcript, func()) {
		t.Helper()
		a := newLiveCatalogBackedTestAgent(t)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatalf("ensure project: %v", err)
		}
		childStore, err := snapshot.NewForSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID)
		if err != nil {
			t.Fatalf("child store: %v", err)
		}
		if err := childStore.BeginChildSession(a.projectRoot, "parent-1"); err != nil {
			t.Fatalf("BeginChildSession: %v", err)
		}
		childID := childStore.SessionID()
		rt := a.ensureRuntime()
		rt.registerTranscript(childID, childStore)
		cleanup := func() {
			rt.unregisterTranscript(childID)
			childStore.Detach()
		}
		return a, childStore, childID, a.transcriptForSessionID(childID), cleanup
	}

	persistChildTurn := func(t *testing.T, store *snapshot.Store, tr *transcript, childID string, content string) int {
		t.Helper()
		turn := store.BeginTurn()
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: childID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: childID, Turn: turn, Result: content})
		data, err := json.Marshal(message.NewText(message.RoleUser, content))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}
		return turn
	}

	// Zero-tail: the marker is durable, the commit has not run, and no event
	// has been dispatched — the live child hydration must exclude the turn
	// entirely until the coordinator commits.
	t.Run("zero_tail_excluded_before_commit", func(t *testing.T) {
		a, store, childID, _, cleanup := newLiveChildFixture(t)
		defer cleanup()
		const content = "child zero tail"
		turn := store.BeginTurn()
		data, err := json.Marshal(message.NewText(message.RoleUser, content))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := store.AppendMessage(turn, data); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}

		hs, err := a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(live child): %v", err)
		}
		if len(hs.Messages) != 0 {
			t.Fatalf("live child hydration has %d durable rows, want 0 (the marked turn stays below the committed bound)", len(hs.Messages))
		}
		if len(hs.Tail) != 0 {
			t.Fatalf("live child hydration has %d tail rows, want 0 (no event dispatched yet)", len(hs.Tail))
		}
		if got := countHydrationContent(hs, content); got != 0 {
			t.Fatalf("hydrated %q %d times, want 0 before the coordinator commit", content, got)
		}
		if hs.Cursor.CommittedTurn != 0 {
			t.Fatalf("cursor committedTurn = %d, want 0 (nothing committed yet)", hs.Cursor.CommittedTurn)
		}
	})

	// Partial-tail: the same marked turn has its row in the retained tail, so
	// the live child hydration renders it exactly once from the tail while the
	// durable half stays excluded.
	t.Run("partial_tail_renders_once_before_commit", func(t *testing.T) {
		a, store, childID, tr, cleanup := newLiveChildFixture(t)
		defer cleanup()
		const content = "child partial tail"
		persistChildTurn(t, store, tr, childID, content)

		hs, err := a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(live child): %v", err)
		}
		if len(hs.Messages) != 0 {
			t.Fatalf("live child hydration has %d durable rows, want 0 (the marked turn stays below the committed bound)", len(hs.Messages))
		}
		if got := countHydrationContent(hs, content); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (the tail carries the uncommitted row)", content, got)
		}
		if len(hs.Tail) != 1 {
			t.Fatalf("live child hydration has %d tail rows, want 1", len(hs.Tail))
		}
		if hs.Cursor.CommittedSeq != 0 {
			t.Fatalf("cursor committedSeq = %d, want 0 (the uncommitted bound is not raised)", hs.Cursor.CommittedSeq)
		}
	})

	// Commit while the entry is still registered: the coordinator's turn-end
	// lands, the bounded read covers the turn, and the suppression cursor
	// (committedSeq = the row's sequence) gates any replayed frame.
	t.Run("committed_while_registered_renders_once_with_suppression_cursor", func(t *testing.T) {
		a, store, childID, tr, cleanup := newLiveChildFixture(t)
		defer cleanup()
		const content = "child committed row"
		turn := persistChildTurn(t, store, tr, childID, content)
		feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: childID, Turn: turn})

		hs, err := a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(live child): %v", err)
		}
		if got := countHydrationContent(hs, content); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (the committed turn is durable through the bound)", content, got)
		}
		if len(hs.Tail) != 0 {
			t.Fatalf("live child hydration has %d tail rows, want none (the commit cleared the tail)", len(hs.Tail))
		}
		if hs.Cursor.CommittedTurn != turn {
			t.Fatalf("cursor committedTurn = %d, want %d", hs.Cursor.CommittedTurn, turn)
		}
		if hs.Cursor.CommittedSeq != 1 {
			t.Fatalf("cursor committedSeq = %d, want 1 (the row's sequence, so replay suppresses)", hs.Cursor.CommittedSeq)
		}
	})

	// After unregister, the completed-child route is the unbounded point
	// lookup: the durable turn renders once and no cursor is carried.
	t.Run("completed_point_lookup_remains_complete_after_unregister", func(t *testing.T) {
		a, store, childID, tr, cleanup := newLiveChildFixture(t)
		defer cleanup()
		const content = "child completed row"
		turn := persistChildTurn(t, store, tr, childID, content)
		feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: childID, Turn: turn})
		a.ensureRuntime().unregisterTranscript(childID)

		hs, err := a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(completed child): %v", err)
		}
		if got := countHydrationContent(hs, content); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (the completed point lookup reads every complete turn)", content, got)
		}
		if len(hs.Tail) != 0 {
			t.Fatalf("completed child hydration has %d live rows, want none", len(hs.Tail))
		}
		if hs.Cursor != (HydrationCursor{}) {
			t.Fatalf("completed child cursor = %+v, want none (the point lookup carries no cursor)", hs.Cursor)
		}
	})

	// The child capture probe fires in the same read-to-revalidation window a
	// root capture's does: a commit injected there forces one retry, and the
	// retry's bounded read returns the committed turn exactly once with the
	// correct cursor. Removing the child probe (or the retry) makes this
	// subtest fail — the mutation the probe's existence gates.
	t.Run("probe_window_commit_forces_one_retry", func(t *testing.T) {
		a, store, childID, tr, cleanup := newLiveChildFixture(t)
		defer cleanup()
		const content = "child commit window"
		turn := persistChildTurn(t, store, tr, childID, content)

		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			if attempt == 1 {
				feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: childID, Turn: turn})
			}
			return nil
		}
		defer func() { a.captureProbe = nil }()

		hs, err := a.HydrateSession(childID)
		if err != nil {
			t.Fatalf("HydrateSession(live child): %v", err)
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2 (the commit in the read-to-revalidation window forced one retry)", attempts)
		}
		if got := countHydrationContent(hs, content); got != 1 {
			t.Fatalf("hydrated %q %d times, want exactly once (the committed turn is durable through the bound)", content, got)
		}
		if len(hs.Tail) != 0 {
			t.Fatalf("hydration has %d tail rows, want none (the commit cleared the tail)", len(hs.Tail))
		}
		if hs.Cursor.CommittedTurn != turn {
			t.Fatalf("cursor committedTurn = %d, want %d", hs.Cursor.CommittedTurn, turn)
		}
		if hs.Cursor.CommittedSeq != 1 {
			t.Fatalf("cursor committedSeq = %d, want 1 (the row's sequence, so replay suppresses)", hs.Cursor.CommittedSeq)
		}
	})

	// A revision change on every attempt exhausts the three-attempt bound with
	// a typed failure: exactly three reads/attempts (the probe fires once per
	// durable read), never a fourth, and no partial state is published.
	t.Run("probe_revision_change_all_three_attempts", func(t *testing.T) {
		a, store, childID, tr, cleanup := newLiveChildFixture(t)
		defer cleanup()
		persistChildTurn(t, store, tr, childID, "child churn")

		attempts := 0
		a.captureProbe = func(int) error {
			attempts++
			tr.seqMu.Lock()
			tr.commitLocked(tr.committedTurn + 1)
			tr.seqMu.Unlock()
			return nil
		}
		defer func() { a.captureProbe = nil }()

		hs, err := a.HydrateSession(childID)
		if !errors.Is(err, errCaptureRevisionChanged) {
			t.Fatalf("err = %v, want errCaptureRevisionChanged (the exhausted hydration surfaces an error, never partial state)", err)
		}
		if attempts != 3 {
			t.Fatalf("attempts = %d, want exactly 3 (one durable read per attempt, never a fourth)", attempts)
		}
		if hs.Session.ID != "" || len(hs.Messages) != 0 || len(hs.Tail) != 0 {
			t.Fatalf("hydration returned partial state %+v alongside the error", hs)
		}
	})

	// A probe error propagates immediately, with no further attempt — the
	// same disposition the root capture applies.
	t.Run("probe_error_propagates_without_retry", func(t *testing.T) {
		a, store, childID, tr, cleanup := newLiveChildFixture(t)
		defer cleanup()
		persistChildTurn(t, store, tr, childID, "child probe error")

		sentinel := errors.New("child probe boom")
		attempts := 0
		a.captureProbe = func(attempt int) error {
			attempts++
			if attempt == 1 {
				return sentinel
			}
			return nil
		}
		defer func() { a.captureProbe = nil }()

		if _, err := a.HydrateSession(childID); !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want the probe's sentinel (probe errors propagate)", err)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (a probe error returns immediately, no later attempt)", attempts)
		}
	})
}

// TestCaptureStateInvokesBoundaryWithBuiltState verifies the capture invokes the
// boundary callback with the immutable state it built, so an adapter can append
// its boundary while the capture still holds the locks.
func TestCaptureStateInvokesBoundaryWithBuiltState(t *testing.T) {
	a := newLiveCatalogBackedTestAgent(t)
	unit := a.session

	tr := a.transcriptForSessionID(sessionIDOf(unit))
	tr.seqMu.Lock()
	tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "hi"})
	tr.seqMu.Unlock()

	invoked := 0
	var seen completeState
	returned, err := a.captureStateForSelection(unit, func(st completeState) {
		invoked++
		seen = st
	})
	if err != nil {
		t.Fatalf("captureStateForSelection: %v", err)
	}
	if invoked != 1 {
		t.Fatalf("boundary invoked %d times, want 1", invoked)
	}
	if len(seen.transcript.tail) != 1 || seen.transcript.tail[0].msg.Content != "hi" {
		t.Fatalf("boundary state tail = %#v", seen.transcript.tail)
	}
	if len(returned.transcript.tail) != len(seen.transcript.tail) {
		t.Fatal("boundary state differs from the returned state")
	}
}

// TestCaptureStateCapturesPendingPermissions verifies the capture reads the
// session's pending permission requests under the gate lock.
func TestCaptureStateCapturesPendingPermissions(t *testing.T) {
	a := newLiveCatalogBackedTestAgent(t)
	unit := a.session
	sessionID := sessionIDOf(unit)

	permCtx, permCancel := context.WithCancel(context.Background())
	defer permCancel()
	go a.gate.AskRequest(permCtx, permission.Request{SessionID: sessionID, ToolName: "write_file", Arg: "x"})

	deadline := time.Now().Add(2 * time.Second)
	for len(a.gate.PendingForSession(sessionID)) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	st, err := a.captureStateForSelection(unit, nil)
	if err != nil {
		t.Fatalf("captureStateForSelection: %v", err)
	}
	a.gate.CancelAll()

	if len(st.permissions) != 1 || st.permissions[0].ToolName != "write_file" || st.permissions[0].SessionID != sessionID {
		t.Fatalf("captured permissions = %#v, want one write_file for this session", st.permissions)
	}
}

// TestCaptureStateReadsAllLiveClasses verifies the full capture reads every live
// class — transcript, activity, model, tokens, queue, warnings — as one set, by
// seeding a distinctive value in each under its lock and asserting all of them.
func TestCaptureStateReadsAllLiveClasses(t *testing.T) {
	a := newLiveCatalogBackedTestAgent(t)
	unit := a.session
	rt := a.ensureRuntime()

	rt.mu.Lock()
	unit.busy = true
	unit.compacting = true
	unit.currentRef = coremodel.ModelRef{Provider: "p", Model: "m"}
	unit.queue = []QueuedItem{{ID: "q1", Content: "queued"}}
	unit.queueVersion = 7
	rt.mu.Unlock()

	unit.tokensMu.Lock()
	unit.tokens = map[string]*TokenEntry{"p/m": {Provider: "p", Model: "m", Input: 11, Known: true}}
	unit.tokensMu.Unlock()

	tr := a.transcriptForSessionID(sessionIDOf(unit))
	tr.seqMu.Lock()
	tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "hi"})
	tr.seqMu.Unlock()

	a.setWarningGroup("protocol", []prompt.Warning{{Kind: "k", Message: "m"}})

	st, err := a.captureStateForSelection(unit, nil)
	if err != nil {
		t.Fatalf("captureStateForSelection: %v", err)
	}
	if len(st.transcript.tail) != 1 || st.transcript.tail[0].msg.Content != "hi" {
		t.Fatalf("captured transcript tail = %#v", st.transcript.tail)
	}
	if !st.busy {
		t.Fatal("busy not captured")
	}
	if !st.compacting {
		t.Fatal("compacting not captured")
	}
	if st.model.Ref != "p/m" || st.model.Provider != "p" || st.model.Model != "m" {
		t.Fatalf("model = %+v, want resolved p/m", st.model)
	}
	if st.tokens.Total.Input != 11 {
		t.Fatalf("tokens total input = %d, want 11", st.tokens.Total.Input)
	}
	if len(st.queue.Items) != 1 || st.queue.Items[0].ID != "q1" || st.queue.Version != 7 {
		t.Fatalf("queue = %+v, want one item q1 version 7", st.queue)
	}
	found := false
	for _, w := range st.warnings {
		if w.Kind == "k" && w.Message == "m" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warning not captured: %#v", st.warnings)
	}
}

// TestTranscriptCoordinatorSessionErrorRetention verifies session-tagged errors are
// retained as sequenced display rows, kept across ordinary commits, and disposed
// per operation: history revert drops errors above its target turn and compaction
// drops errors through its replaced range.
func TestTranscriptCoordinatorSessionErrorRetention(t *testing.T) {
	tr := newTranscript()

	tr.seqMu.Lock()
	tr.appendEventLocked(Event{Kind: EventTurnStart, Turn: 1})
	tr.appendEventLocked(Event{Kind: EventTextDelta, Result: "hello"})
	tr.appendErrorLocked(Event{Kind: EventError, Error: "boom", Turn: 1})
	tr.appendEventLocked(Event{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "x"})

	// Rows and the error share one strictly increasing sequence.
	rowSeq1 := tr.tail[0].seq
	errSeq := tr.retainedErrors[0].seq
	rowSeq2 := tr.tail[1].seq
	if !(rowSeq1 < errSeq && errSeq < rowSeq2) {
		t.Fatalf("error not sequenced between rows: row=%d err=%d row=%d", rowSeq1, errSeq, rowSeq2)
	}
	e := tr.retainedErrors[0]
	if e.msg.Type != "error" || e.msg.Content != "boom" || e.turn != 1 {
		t.Fatalf("retained error = %+v", e)
	}

	// Ordinary commit preserves the error: completed messages do not represent it.
	tr.commitLocked(1)
	if len(tr.retainedErrors) != 1 {
		t.Fatalf("commit pruned retained error: %#v", tr.retainedErrors)
	}
	tr.seqMu.Unlock()

	// History revert to turn 1 drops errors above turn 1, keeps turn 1.
	tr.seqMu.Lock()
	tr.appendErrorLocked(Event{Kind: EventError, Error: "later", Turn: 2})
	tr.dropErrorsAboveTurnLocked(1)
	if len(tr.retainedErrors) != 1 || tr.retainedErrors[0].turn != 1 {
		t.Fatalf("history-revert disposition wrong: %#v", tr.retainedErrors)
	}
	tr.seqMu.Unlock()

	// Compaction through turn 1 removes the turn-1 error.
	tr.seqMu.Lock()
	tr.dropErrorsThroughTurnLocked(1)
	if len(tr.retainedErrors) != 0 {
		t.Fatalf("compaction disposition kept turn-1 error: %#v", tr.retainedErrors)
	}
	tr.seqMu.Unlock()
}

// TestSessionErrorRetentionContract verifies the compaction rewrite prunes
// retained errors whose turns the compacted record replaces while keeping an
// error that carries no turn attribution: an unattributed error has no merge
// key, so it belongs to no compacted range and keeps its position after all
// committed rows.
//
// Exception, named per the contract-test rule: the pruning runs inside
// publishCompactionRewrite, which is unexported and not adapter-facing. The
// exported route CompactNowForSession reaches the same boundary only after a
// live multi-turn session, a model server, and a summarizer round-trip — none
// of which participate in the pruning contract, and that full route is already
// exercised end-to-end by TestCompactionIndexesSelectedSessionProject.
func TestSessionErrorRetentionContract(t *testing.T) {
	t.Run("attribution=compacted_away", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		tr := a.transcriptForSessionID(sessionIDOf(unit))
		a.feedAndEmit(tr, Event{Kind: EventError, SessionID: sessionIDOf(unit), Error: "boom", Turn: 1})
		a.publishCompactionRewrite(unit, sessionIDOf(unit), unit.projectID, 1, SessionSummary{}, nil)
		tr.seqMu.Lock()
		defer tr.seqMu.Unlock()
		if len(tr.retainedErrors) != 0 {
			t.Fatalf("retained errors = %#v, want the turn-1 error pruned by the compaction rewrite", tr.retainedErrors)
		}
	})

	t.Run("attribution=none", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		tr := a.transcriptForSessionID(sessionIDOf(unit))
		a.feedAndEmit(tr, Event{Kind: EventError, SessionID: sessionIDOf(unit), Error: "boom"})
		a.publishCompactionRewrite(unit, sessionIDOf(unit), unit.projectID, 1, SessionSummary{}, nil)
		tr.seqMu.Lock()
		defer tr.seqMu.Unlock()
		if len(tr.retainedErrors) != 1 || tr.retainedErrors[0].turn != 0 {
			t.Fatalf("retained errors = %#v, want the unattributed error kept", tr.retainedErrors)
		}
	})

	t.Run("attribution=after_boundary", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		tr := a.transcriptForSessionID(sessionIDOf(unit))
		a.feedAndEmit(tr, Event{Kind: EventError, SessionID: sessionIDOf(unit), Error: "boom", Turn: 2})
		a.publishCompactionRewrite(unit, sessionIDOf(unit), unit.projectID, 1, SessionSummary{}, nil)
		tr.seqMu.Lock()
		defer tr.seqMu.Unlock()
		if len(tr.retainedErrors) != 1 || tr.retainedErrors[0].turn != 2 {
			t.Fatalf("retained errors = %#v, want the turn-2 error kept", tr.retainedErrors)
		}
	})
}

// TestCompactionRewriteCarriesRetainedErrors verifies the compaction rewrite
// payload carries the committed prefix followed by the retained tail and the
// surviving retained errors merged by their shared display sequence — the same
// ordering the desktop snapshot applies to those two live classes — so an error
// that survives the compaction disposition stays on screen after the rewrite
// instead of vanishing until a full hydration. The turn-1 error is compacted
// away; the unattributed (turn 0) and turn-2 errors survive and take their
// sequenced place among the tail rows.
func TestCompactionRewriteCarriesRetainedErrors(t *testing.T) {
	a := newLiveCatalogBackedTestAgent(t)
	unit := a.session
	id := sessionIDOf(unit)
	tr := a.transcriptForSessionID(id)

	var rewrite Event
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventSessionRewrite {
			rewrite = ev
		}
	})

	a.feedAndEmit(tr, Event{Kind: EventTurnStart, Turn: 1})
	a.feedAndEmit(tr, Event{Kind: EventTextDelta, Result: "hello"})
	a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: "compacted away", Turn: 1})
	a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: "unattributed"})
	a.feedAndEmit(tr, Event{Kind: EventError, SessionID: id, Error: "after boundary", Turn: 2})
	a.feedAndEmit(tr, Event{Kind: EventUserMessageDisplay, Result: "next", Turn: 2})

	a.publishCompactionRewrite(unit, id, unit.projectID, 1, SessionSummary{}, []DisplayMessage{
		{Type: "user", Content: "committed", Turn: 1},
	})
	if rewrite.RewritePayload == nil {
		t.Fatal("rewrite boundary not emitted")
	}

	var contents []string
	for _, m := range rewrite.RewritePayload.Messages {
		contents = append(contents, m.Content)
	}
	want := []string{"committed", "hello", "unattributed", "after boundary", "next"}
	if !reflect.DeepEqual(contents, want) {
		t.Fatalf("rewrite messages = %v, want %v", contents, want)
	}
}

// TestHydrationAssistantSpanFact verifies the hydration state publishes whether
// the last row is an open assistant span — the fact the desktop root view uses
// to continue a turn that was streaming when the snapshot was captured — and
// that the fact is computed from the rows the snapshot actually carries rather
// than copied from the coordinator's running flag: a capture whose last row is
// a completed durable answer must publish false even while the coordinator's
// span is open.
func TestHydrationAssistantSpanFact(t *testing.T) {
	// The span is open while a text delta streamed and no boundary closed it.
	t.Run("span=open", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: 1})
		feedTranscript(tr, Event{Kind: EventTextDelta, SessionID: id, Result: "streaming"})
		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if !hs.AssistantOpen {
			t.Fatal("assistantOpen = false, want true while the assistant span is open")
		}
	})

	// Any boundary that closes the span reads false: a tool start after the
	// text row closes it even though the tail still holds the assistant row.
	t.Run("span=closed_by_tool_start", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: 1})
		feedTranscript(tr, Event{Kind: EventTextDelta, SessionID: id, Result: "streaming"})
		feedTranscript(tr, Event{Kind: EventToolCallStart, SessionID: id, ToolCallID: "t1", ToolName: "x"})
		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if hs.AssistantOpen {
			t.Fatal("assistantOpen = true, want false after the boundary that closed the span")
		}
	})

	// A completed turn that committed in the coordinator leaves no live rows:
	// the snapshot's last row is the durable assistant answer, which nothing
	// is streaming into.
	t.Run("span=last_row_completed_answer", func(t *testing.T) {
		a := newLiveCatalogBackedTestAgent(t)
		unit := a.session
		id := sessionIDOf(unit)
		tr := a.transcriptForSessionID(id)

		turn := unit.store.BeginTurn()
		feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
		feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "question"})
		feedTranscript(tr, Event{Kind: EventTextDelta, SessionID: id, Result: "answer"})
		for _, m := range []message.Message{
			message.NewText(message.RoleUser, "question"),
			message.NewText(message.RoleAssistant, "answer"),
		} {
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := unit.store.AppendMessage(turn, data); err != nil {
				t.Fatalf("AppendMessage: %v", err)
			}
		}
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}
		feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: id, ProjectID: unit.projectID, Turn: turn})

		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession: %v", err)
		}
		if len(hs.Messages) == 0 || hs.Messages[len(hs.Messages)-1].Type != "assistant" {
			t.Fatalf("fixture: snapshot must end with the completed assistant answer, got %+v", hs.Messages)
		}
		if hs.AssistantOpen {
			t.Fatal("assistantOpen = true, want false when the last row is a completed durable answer")
		}
	})
}

// TestResyncPayloadCarriesAssistantSpanFact verifies the compaction rewrite
// payload publishes the same open-assistant-span fact the hydration state
// carries, computed from the rows the payload carries, so the desktop view
// continues a turn that was streaming when the rewrite boundary landed.
func TestResyncPayloadCarriesAssistantSpanFact(t *testing.T) {
	a := newLiveCatalogBackedTestAgent(t)
	unit := a.session
	id := sessionIDOf(unit)
	tr := a.transcriptForSessionID(id)

	var rewrite Event
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventSessionRewrite {
			rewrite = ev
		}
	})

	a.feedAndEmit(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: 1})
	a.feedAndEmit(tr, Event{Kind: EventTextDelta, SessionID: id, Result: "streaming"})
	a.publishCompactionRewrite(unit, id, unit.projectID, 1, SessionSummary{}, nil)
	if rewrite.RewritePayload == nil {
		t.Fatal("rewrite boundary not emitted")
	}
	if !rewrite.RewritePayload.AssistantOpen {
		t.Fatal("resync assistantOpen = false, want true while the assistant span is open")
	}

	// A boundary that closes the span is carried as false.
	a.feedAndEmit(tr, Event{Kind: EventToolCallStart, SessionID: id, ProjectID: unit.projectID, ToolCallID: "t1", ToolName: "x"})
	rewrite = Event{}
	a.publishCompactionRewrite(unit, id, unit.projectID, 1, SessionSummary{}, nil)
	if rewrite.RewritePayload == nil {
		t.Fatal("rewrite boundary not emitted")
	}
	if rewrite.RewritePayload.AssistantOpen {
		t.Fatal("resync assistantOpen = true, want false after the boundary that closed the span")
	}
}

// TestTranscriptCoordinatorCommit verifies the commit cursor partitions
// state exactly: a committed turn clears the retained tail and advances the
// committed markers to the sequence high-water, later preseeds keep sequence
// monotonic.
func TestTranscriptCoordinatorCommit(t *testing.T) {
	tr := newTranscript()

	// Turn 1 streams two rows, then commits.
	feedTranscriptEvents(tr, []Event{
		{Kind: EventTurnStart, Turn: 1},
		{Kind: EventTextDelta, Result: "a"},
		{Kind: EventToolCallStart, ToolCallID: "t1", ToolName: "x"},
		{Kind: EventToolCallEnd, ToolCallID: "t1", Result: "ok"},
		{Kind: EventTurnEnd, Turn: 1},
	})
	tr.seqMu.Lock()
	high1 := tr.nextSeq - 1
	tr.commitLocked(1)
	rev1 := tr.revisionLocked()
	tailAfter1 := tr.tailMessagesLocked()
	tr.seqMu.Unlock()

	if high1 != 2 {
		t.Fatalf("turn 1 high-water seq = %d, want 2", high1)
	}
	if len(tailAfter1) != 0 {
		t.Fatalf("tail after commit = %d rows, want 0", len(tailAfter1))
	}
	if rev1 != (captureRevision{committedTurn: 1, committedSeq: 2, rewriteEpoch: 0}) {
		t.Fatalf("revision after commit 1 = %+v", rev1)
	}

	// Preseed of turn 2: its rows must take sequences strictly above committedSeq.
	feedTranscriptEvents(tr, []Event{
		{Kind: EventUserMessageDisplay, Result: "q2", Turn: 2},
		{Kind: EventTurnStart, Turn: 2},
		{Kind: EventTextDelta, Result: "b"},
		{Kind: EventTurnEnd, Turn: 2},
	})
	tr.seqMu.Lock()
	firstSeq2 := tr.tail[0].seq
	high2 := tr.nextSeq - 1
	tr.commitLocked(2)
	rev2 := tr.revisionLocked()
	tr.seqMu.Unlock()

	if firstSeq2 <= rev1.committedSeq {
		t.Fatalf("turn 2 first row seq = %d, must exceed committedSeq %d", firstSeq2, rev1.committedSeq)
	}
	if rev2 != (captureRevision{committedTurn: 2, committedSeq: high2, rewriteEpoch: 0}) {
		t.Fatalf("revision after commit 2 = %+v (high2=%d)", rev2, high2)
	}
}

// commitState is the coordinator snapshot a turn-end event handler records at
// the moment EventTurnEnd is delivered.
type commitState struct {
	seen          bool
	turn          int
	committedTurn int
	committedSeq  int
	nextSeq       int
	tailLen       int
}

// TestTranscriptCommitContractMatrix verifies the turn-end commit ordering
// contract: the drainer flush and the coordinator commit run after the turn's
// loop returns and strictly before EventTurnEnd is emitted. With the commit
// inside the runtime.mu section that clears busy, a submit that observes busy
// cleared and claims the unit cannot feed the next turn's rows into a tail the
// previous turn's commit then wipes.
func TestTranscriptCommitContractMatrix(t *testing.T) {
	// The event handler observes the coordinator at the instant turn_end is
	// delivered. The commit must already have run: committedTurn must equal
	// the delivered turn, the retained tail must be empty, and the committed
	// sequence must be the high-water. With the commit after the emit (the
	// pre-fix order) the handler sees the turn's rows still uncommitted.
	t.Run("order=commit_before_end_emit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTextResponse(w, "hello back")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		ctx := startEventOrderAgent(t, a, &eventCapture{})
		// Swap in the observing handler after Init: startEventOrderAgent
		// installs the capture handler, and this one records the coordinator
		// state at every turn_end delivery. The handler must take seqMu only
		// for turn_end — row events are emitted from feedAndEmit, which
		// already holds seqMu.
		var stateMu sync.Mutex
		var atEnd commitState
		a.SetEventHandler(func(ev Event) {
			if ev.Kind != EventTurnEnd {
				return
			}
			tr := a.transcriptForSessionID(ev.SessionID)
			if tr == nil {
				return
			}
			tr.seqMu.Lock()
			st := commitState{
				seen:          true,
				turn:          ev.Turn,
				committedTurn: tr.committedTurn,
				committedSeq:  tr.committedSeq,
				nextSeq:       tr.nextSeq,
				tailLen:       len(tr.tail),
			}
			tr.seqMu.Unlock()
			stateMu.Lock()
			atEnd = st
			stateMu.Unlock()
		})
		if _, err := a.Submit(ctx, "hello"); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitUntilEventOrderAgentIdle(t, a)

		stateMu.Lock()
		st := atEnd
		stateMu.Unlock()
		if !st.seen {
			t.Fatal("no turn_end delivered")
		}
		if st.committedTurn != st.turn {
			t.Fatalf("at turn_end delivery committedTurn = %d, want the delivered turn %d (the commit ran after the emit)", st.committedTurn, st.turn)
		}
		if st.tailLen != 0 {
			t.Fatalf("at turn_end delivery retained tail = %d rows, want 0 (the commit ran after the emit)", st.tailLen)
		}
		if st.committedSeq != st.nextSeq-1 {
			t.Fatalf("at turn_end delivery committedSeq = %d, want high-water %d (the commit ran after the emit)", st.committedSeq, st.nextSeq-1)
		}
	})

	// A submit racing a turn end must not lose the next turn's rows. The
	// pre-fix window: the turn-end path cleared busy and released runtime.mu
	// before feeding the end event, so a concurrent submit could observe busy
	// false, claim, and launch turn N+1 whose rows enter the tail before turn
	// N's late commit set t.tail = nil, erasing them.
	//
	// The subtest parks turn N's commit feed by holding seqMu (the only
	// parkable point on that path), releases turn N's model gate, and submits
	// turn N+1 while the commit is still pending:
	//
	//   - Pre-fix the feed is post-unlock, so the submit claims, launches, and
	//     its turn_start is delivered (and turn N's turn_end was already
	//     delivered before the feed) while the commit is still pending — the
	//     exact window the defect is built on. The assertion below fails.
	//   - Now the commit runs immediately before the busy-clear section while
	//     the unit is still busy, so the racing submit cannot claim: a busy
	//     unit enqueues, no turn_start(N+1) is delivered before the release,
	//     and the drained turn N+1 feeds its rows after the commit and
	//     survives.
	t.Run("race=submit_vs_turn_end", func(t *testing.T) {
		releaseRun := make(chan struct{})
		releaseN1 := make(chan struct{})
		reqNSeen := make(chan struct{}, 1)
		reqN1Seen := make(chan struct{}, 1)
		var runOnce, n1Once sync.Once
		closeRun := func() { runOnce.Do(func() { close(releaseRun) }) }
		closeN1 := func() { n1Once.Do(func() { close(releaseN1) }) }
		t.Cleanup(closeRun)
		t.Cleanup(closeN1)

		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Payload-only response: no TextDelta reaches the loop event queue
			// while seqMu is held, so the drainer stays idle and the flush ack
			// completes instead of deadlocking on the parked commit feed.
			gate := releaseN1
			seen := reqN1Seen
			if reqs.Add(1) == 1 {
				gate = releaseRun
				seen = reqNSeen
			}
			select {
			case seen <- struct{}{}:
			default:
			}
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","reasoning":"stall"},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		t.Cleanup(server.Close)

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		// Turn N: submit and park its Run in the model request.
		resN, err := a.SubmitToSession(ctx, id, "turn N")
		if err != nil {
			t.Fatalf("SubmitToSession N: %v", err)
		}
		if !resN.Started {
			t.Fatalf("turn N enqueued instead of starting")
		}
		waitForSignal(t, reqNSeen, "turn N model request")

		// Park turn N's commit feed: hold seqMu, then release Run(N), so the
		// turn-end path runs through the flush and parks at the commit feed,
		// before the busy-clear section, with the deferred cleanup pending.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		seqMuHeld := true
		defer func() {
			if seqMuHeld {
				tr.seqMu.Unlock()
			}
		}()

		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()

		closeRun()
		// Wait until turn N's loop has returned and its turn-end path is in
		// flight instead of a fixed sleep: Run's deferred MarkTurnComplete
		// writes the durable complete marker, and the held seqMu parks the
		// commit feed from there on, so the racing submit lands while the
		// commit is still pending and the unit is still busy — not while the
		// turn is still running.
		completeMarker := filepath.Join(unit.store.Dir(), "turns", strconv.Itoa(resN.Turn), "complete")
		parkDeadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(parkDeadline) {
			if _, err := os.Stat(completeMarker); err == nil {
				break
			} else if !os.IsNotExist(err) {
				t.Fatalf("stat complete marker: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
		}
		if _, err := os.Stat(completeMarker); err != nil {
			t.Fatal("turn N did not reach its complete marker before the racing submit")
		}

		// Submit turn N+1 racing the turn end while the commit is pending,
		// and wait for the submit to complete while the commit is still
		// parked (seqMu is still held here): the unit is still busy, so a
		// busy unit must enqueue rather than claim.
		var resN1 SubmitResult
		var errN1 error
		var subWg sync.WaitGroup
		subWg.Add(1)
		go func() {
			defer subWg.Done()
			resN1, errN1 = a.SubmitToSession(ctx, id, "turn N+1")
		}()
		subWg.Wait()
		if errN1 != nil {
			t.Fatalf("SubmitToSession N+1: %v", errN1)
		}
		if resN1.Started {
			t.Fatalf("the racing submit started turn %d while turn %d's commit was still pending; a busy unit must enqueue, not claim", resN1.Turn, resN.Turn)
		}

		// While turn N's commit is still pending (seqMu is still held here),
		// the racing submit must not have claimed: no turn_start(N+1) may be
		// delivered, and turn N's turn_end may not precede the commit.
		startedNext, endedN := false, false
		for _, ev := range cap.snapshot() {
			switch {
			case ev.Kind == EventTurnStart && ev.Turn == resN.Turn+1:
				startedNext = true
			case ev.Kind == EventTurnEnd && ev.Turn == resN.Turn:
				endedN = true
			}
		}
		if startedNext {
			t.Fatalf("the racing submit claimed and launched turn %d while turn %d's commit was still pending — the window that wipes the next turn's rows", resN.Turn+1, resN.Turn)
		}
		if endedN {
			t.Fatalf("turn %d's turn_end was delivered before its commit ran", resN.Turn)
		}

		// Release the stall: turn N's commit completes and its end event is
		// emitted, then the drainer launches the enqueued turn N+1, which
		// runs to the second gate.
		tr.seqMu.Unlock()
		seqMuHeld = false
		waitForSignal(t, reqN1Seen, "turn N+1 model request")

		// The next turn's rows must be intact: its user row is in the retained
		// tail while the turn is live (the loop emits the user display before
		// the model request).
		userRow := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && !userRow {
			tr.seqMu.Lock()
			for _, row := range tr.tail {
				if row.msg.Type == "user" && row.msg.Content == "turn N+1" {
					userRow = true
					break
				}
			}
			tr.seqMu.Unlock()
			if !userRow {
				time.Sleep(2 * time.Millisecond)
			}
		}
		if !userRow {
			t.Fatal("the next turn's user row is missing from the retained tail: turn N's commit wiped it")
		}

		closeN1()
		waitSessionDrained(t, a, id)

		// The drained turn advanced the committed marker: turn N+1's rows
		// were committed, not wiped by turn N's commit.
		tr.seqMu.Lock()
		committedTurn := tr.committedTurn
		tr.seqMu.Unlock()
		if committedTurn != resN.Turn+1 {
			t.Fatalf("committedTurn = %d, want the drained turn %d", committedTurn, resN.Turn+1)
		}
	})

	// A queued preseed completes its turn durably and commits it before the
	// final queued item launches. While the launched turn is still live (its
	// model request parked), the coordinator must already have committed the
	// preseed's turn, and a hydration taken then must return the preseed
	// exactly once — from the durable half alone, with the retained tail no
	// longer carrying it. The pre-fix drain preseeded under rt.mu with no feed
	// and no commit, so the preseed's row stayed in the retained tail while its
	// turn was already durable: a mid-drain hydration returned it from both
	// halves and rendered it twice.
	t.Run("preseed=commits_before_launch", func(t *testing.T) {
		release := make(chan struct{})
		reqSeen := make(chan struct{}, 1)
		var reqSeenOnce, runOnce sync.Once
		closeReqSeen := func() { reqSeenOnce.Do(func() { close(reqSeen) }) }
		closeRun := func() { runOnce.Do(func() { close(release) }) }
		// Register the server close before closeRun: cleanup runs LIFO, so a
		// parked handler is released before the server is allowed to close.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			closeReqSeen()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			writeTextResponse(w, "ok")
		}))
		t.Cleanup(func() { server.Close() })
		t.Cleanup(closeRun)

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		rt.sessionLocked().queue = []QueuedItem{
			{ID: "q-1", Content: "preseed me"},
			{ID: "q-2", Content: "launch me"},
		}
		rt.sessionLocked().queueSeq = 2
		rt.sessionLocked().queueVersion = 1
		rt.mu.Unlock()

		go rt.tryDrainQueue(ctx)
		waitForSignal(t, reqSeen, "launched turn model request")

		// The preseed's turn must already be committed while the launched turn
		// is still live: the pre-fix drain fed nothing, so committedTurn is 0
		// here.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		committedTurn := tr.committedTurn
		tr.seqMu.Unlock()
		if committedTurn != 1 {
			t.Fatalf("committedTurn = %d while the launched turn is live, want 1 (the preseed commits before the launch)", committedTurn)
		}

		// A hydration taken mid-drain returns the preseed exactly once. The
		// pre-fix drain left the preseed's row in the retained tail while its
		// turn was already durable, so the capture returned it twice.
		hs, err := a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession mid-drain: %v", err)
		}
		if got := countHydrationContent(hs, "preseed me"); got != 1 {
			t.Fatalf("mid-drain hydration returns the preseed %d times, want exactly once (durable + tail)", got)
		}

		closeRun()
		waitSessionDrained(t, a, id)

		hs, err = a.HydrateSession(id)
		if err != nil {
			t.Fatalf("HydrateSession after drain: %v", err)
		}
		for _, content := range []string{"preseed me", "launch me"} {
			if got := countHydrationContent(hs, content); got != 1 {
				t.Fatalf("post-drain hydration returns %q %d times, want exactly once", content, got)
			}
		}
		var livePreseed, liveLaunch int
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventUserMessageDisplay {
				switch ev.Result {
				case "preseed me":
					livePreseed++
				case "launch me":
					liveLaunch++
				}
			}
		}
		if livePreseed != 1 || liveLaunch != 1 {
			t.Fatalf("live user displays: preseed=%d launch=%d, want 1 each", livePreseed, liveLaunch)
		}
	})

	// A preseed whose durable marker write fails must surface the failure
	// through the transcript (sequenced, so a hydration or reconnect sees it),
	// launch no further turn, requeue the untouched remainder — and only that,
	// with its original ids — ahead of anything submitted meanwhile, and leave
	// the unit drainable. Then, with the fault cleared, draining again must
	// render the failed message exactly once in the live stream and exactly
	// once in loop history: not once per attempt.
	//
	// The marker fault is armed with the drain parked at the preseed's turn
	// start feed: the drain has already called BeginTurn (so the turn dir and
	// Store.CurrentTurn() are known) but not yet MarkTurnComplete, and the
	// parked feed holds seqMu so the test controls the release. A directory
	// named complete at that turn's marker path makes the marker write fail
	// with EISDIR on any filesystem, regardless of root. Arming before the
	// drain cannot target a preseed: creating turns/<n> feeds the store's
	// nextTurnLocked scan, so every preseed takes max+1 and skips the armed
	// dir.
	t.Run("preseed=marker_failure_aborts_and_requeues", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			writeTextResponse(w, "ok")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()

		rt.mu.Lock()
		unit.queue = []QueuedItem{
			{ID: "q-1", Content: "fail me"},
			{ID: "q-2", Content: "survivor"},
		}
		unit.queueSeq = 2
		unit.queueVersion = 1
		rt.mu.Unlock()

		// Park the drain at the first preseed's turn start feed: the preseed
		// loop has called BeginTurn (turns/<next> exists) and is waiting on the
		// coordinator lock the test holds. The park is what makes both the
		// arming and the mid-drain append deterministic.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		seqMuHeld := true
		defer func() {
			if seqMuHeld {
				tr.seqMu.Unlock()
			}
		}()

		drained := make(chan struct{})
		go func() {
			defer close(drained)
			rt.tryDrainQueue(ctx)
		}()
		// Wait until the drain has begun the first preseed turn instead of a
		// fixed sleep: the held seqMu parks the drain at the turn-start feed
		// once BeginTurn ran, so the arming below targets exactly that turn.
		// The check after the loop reports a drain that never got there.
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) && unit.store.CurrentTurn() == 0 {
			time.Sleep(2 * time.Millisecond)
		}

		// Arm the marker fault at the turn the parked drain just began. Then
		// drive the concurrent message through the real submit path: the drain
		// holds the claim (busy), so the submit enqueues, and the requeue must
		// prepend the remainder ahead of it. The submit nudges the drainer.
		failTurn := unit.store.CurrentTurn()
		if failTurn == 0 {
			t.Fatalf("drain did not begin a preseed turn before parking")
		}
		markerPath := filepath.Join(unit.store.Dir(), "turns", strconv.Itoa(failTurn), "complete")
		if err := os.MkdirAll(markerPath, 0o700); err != nil {
			t.Logf("armed marker fault failed (pre-fix drain does not park at the feed): %v", err)
		}
		clearFault := func() {
			if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
				t.Fatalf("clear marker fault: %v", err)
			}
		}
		t.Cleanup(clearFault)

		// Install the drain-pass hook before the submit that nudges the
		// drainer. The settle below waits on it: once the background drainer
		// has completed the pass its wake triggered, it is back waiting on
		// the wake and cannot re-drain until the test nudges it again. The
		// hook is one-shot, so the later explicit nudge's pass cannot signal
		// into the same channel.
		passDone := make(chan struct{})
		var passOnce sync.Once
		rt.queueDrainPassHook = func() { passOnce.Do(func() { close(passDone) }) }

		res, err := a.SubmitToSession(ctx, id, "meanwhile")
		if err != nil {
			t.Fatalf("SubmitToSession meanwhile: %v", err)
		}
		if res.Started {
			t.Fatal("submit during the drain claimed the unit")
		}
		if len(res.Queue) != 1 || res.Queue[0].Content != "meanwhile" {
			t.Fatalf("submit snapshot = %#v, want one enqueued meanwhile item", res.Queue)
		}
		// Wait until the background drainer has completed the pass the
		// submit's wake triggered while the drain is still parked and busy.
		// The wake token alone proves only that the receive happened: a
		// drainer descheduled between consuming the token and attempting its
		// claim would find the unit drainable once the abort clears busy and
		// re-drain the requeued remainder, which the case requires not to
		// happen until the explicit nudge below. The hook fires only after
		// the pass returns — whether it took the claim or found the unit busy
		// — so no launch can follow the marker failure until the test nudges
		// the drainer again.
		select {
		case <-passDone:
		case <-time.After(15 * time.Second):
			t.Fatal("background drainer did not finish its pass while the drain was parked")
		}

		tr.seqMu.Unlock()
		seqMuHeld = false
		select {
		case <-drained:
		case <-time.After(15 * time.Second):
			t.Fatal("drain did not return after the marker failure")
		}

		// The failure is sequenced into the coordinator — not just delivered
		// live — so a hydration or reconnect sees it.
		tr.seqMu.Lock()
		errRows := append([]errorRow(nil), tr.retainedErrors...)
		tr.seqMu.Unlock()
		if len(errRows) != 1 {
			t.Fatalf("retained errors = %d, want exactly 1 sequenced failure", len(errRows))
		}
		var liveErr bool
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventError {
				liveErr = true
			}
		}
		if !liveErr {
			t.Fatal("marker failure was not delivered live")
		}

		// The abort's exit disposition re-arms the retained work on the live
		// owner: the drainer re-drains the requeued remainder on its own — the
		// fault is armed only at the failed turn, so the re-drain succeeds and
		// launches the final item. No manual nudge is required.
		rearmDeadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(rearmDeadline) && reqs.Load() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		if got := reqs.Load(); got != 1 {
			t.Fatalf("retained work never re-drained after the marker-failure abort (model requests = %d, want 1)", got)
		}
		// The requeue event carried the real remainder — the failed item is
		// not requeued, and the item appended meanwhile is prepended behind
		// the survivors — with the original ids preserved.
		assertQueueChangedPayloadPresent(t, cap, []QueuedItem{{ID: "q-2", Content: "survivor"}, {ID: "q-3", Content: "meanwhile"}})

		// Exactly one model turn launched after the abort — the rearm's final
		// item — and the failed message rendered exactly once in the live
		// stream and once in loop history, not once per attempt.
		if got := reqs.Load(); got != 1 {
			t.Fatalf("model requests after the rearm = %d, want exactly 1 (the retained final item launched once)", got)
		}
		rt.mu.Lock()
		busy := unit.busy
		turnCtxSet := unit.turnCtx != nil
		turnCancelSet := unit.turnCancel != nil
		rt.mu.Unlock()
		if busy {
			t.Fatal("unit still busy after the rearm completed")
		}
		if turnCtxSet || turnCancelSet {
			t.Fatal("rearm completed but left the per-turn context installed")
		}
		var liveFail, liveSurvivor, liveMeanwhile int
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventUserMessageDisplay {
				switch ev.Result {
				case "fail me":
					liveFail++
				case "survivor":
					liveSurvivor++
				case "meanwhile":
					liveMeanwhile++
				}
			}
		}
		if liveFail != 1 || liveSurvivor != 1 || liveMeanwhile != 1 {
			t.Fatalf("live user displays: fail=%d survivor=%d meanwhile=%d, want 1 each", liveFail, liveSurvivor, liveMeanwhile)
		}
		var histFail, histSurvivor int
		for _, m := range unit.lp.Messages() {
			if m.Role != message.RoleUser {
				continue
			}
			switch m.TextContent() {
			case "fail me":
				histFail++
			case "survivor":
				histSurvivor++
			}
		}
		if histFail != 1 {
			t.Fatalf("loop history has %q %d times, want exactly once", "fail me", histFail)
		}
		if histSurvivor != 1 {
			t.Fatalf("loop history has %q %d times, want exactly once", "survivor", histSurvivor)
		}
	})

	// Admission can close while the preseed runs. The section that reacquires
	// the runtime mutex before the final launch must re-check it: a shutdown
	// arriving during the preseed aborts the drain the way the marker-failure
	// path aborts — the untouched remainder goes back on the queue, no turn is
	// launched, and the per-iteration cleanup clears busy and releases the
	// count. The pre-fix drain installed nothing to re-check, so it launched
	// the final item after shutdown and emitted its turn_start.
	t.Run("preseed=shutdown_aborts_before_launch", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			writeTextResponse(w, "ok")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()
		rt.mu.Lock()
		unit.queue = []QueuedItem{
			{ID: "q-1", Content: "preseed"},
			{ID: "q-2", Content: "launch"},
		}
		unit.queueSeq = 2
		unit.queueVersion = 1
		rt.mu.Unlock()

		// Park the drain at the first preseed's turn start feed, then start
		// the shutdown. ShutdownOwner sets admission closed, cancels the
		// claim-time turn context, and joins turnWG — which the parked drain
		// holds until it aborts.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		seqMuHeld := true
		defer func() {
			if seqMuHeld {
				tr.seqMu.Unlock()
			}
		}()

		drained := make(chan struct{})
		go func() {
			defer close(drained)
			rt.tryDrainQueue(ctx)
		}()
		// Wait until the drain has claimed the unit and begun the first
		// preseed turn instead of a fixed sleep: the claim installed the turn
		// context and the held seqMu parks the drain at the turn-start feed,
		// so the shutdown below cancels a live claim-time context and the
		// cancellation poll that follows cannot time out against an
		// unclaimed unit.
		parkDeadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(parkDeadline) && unit.store.CurrentTurn() == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		if unit.store.CurrentTurn() == 0 {
			t.Fatal("drain did not begin a preseed turn before the shutdown")
		}

		shutdownDone := make(chan struct{})
		go func() {
			defer close(shutdownDone)
			a.ShutdownOwner()
		}()

		// Wait until the shutdown has cancelled the claim-time turn context —
		// only possible because the context is installed at claim. Releasing
		// before that would let the drain finish its preseed unhindered.
		cancelled := false
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			rt.mu.Lock()
			tc := unit.turnCtx
			rt.mu.Unlock()
			if tc != nil && tc.Err() != nil {
				cancelled = true
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !cancelled {
			t.Fatal("shutdown did not cancel the claimed-but-not-launched turn context")
		}

		tr.seqMu.Unlock()
		seqMuHeld = false
		select {
		case <-drained:
		case <-time.After(15 * time.Second):
			t.Fatal("drain did not return after shutdown")
		}
		select {
		case <-shutdownDone:
		case <-time.After(15 * time.Second):
			t.Fatal("ShutdownOwner did not complete")
		}

		// No turn launched after shutdown.
		if got := reqs.Load(); got != 0 {
			t.Fatalf("a model turn launched after shutdown (%d requests)", got)
		}
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				t.Fatalf("a turn_start was delivered after shutdown: %#v", ev)
			}
		}

		// The untouched remainder (the final item) is back on the queue with
		// its original id, and the requeue event carries it. The queue is
		// unit-local in-memory state with no durable counterpart, so it is read
		// from the unit directly (named exception): the adapter-facing
		// QueueSnapshotForSession requires a live store, and a clean shutdown
		// deliberately detaches stores, so it cannot resolve the session
		// afterwards. rt.queueSnapshotLocked is the same helper the adapter
		// method calls.
		rt.mu.Lock()
		q := rt.queueSnapshotLocked(unit)
		rt.mu.Unlock()
		if len(q.Items) != 1 || q.Items[0].Content != "launch" || q.Items[0].ID != "q-2" {
			t.Fatalf("queue after shutdown abort = %#v, want [launch(q-2)]", q.Items)
		}
		assertQueueChangedPayloadForVersion(t, cap, q.Version, q.Items)

		// The abort leaves the same residue as the other abort paths: the
		// per-turn context is cleared, not left installed-cancelled.
		rt.mu.Lock()
		turnCtxSet := unit.turnCtx != nil
		turnCancelSet := unit.turnCancel != nil
		rt.mu.Unlock()
		if turnCtxSet || turnCancelSet {
			t.Fatal("shutdown abort left the per-turn context installed")
		}
	})

	// The unit is busy from the moment it is claimed, so a cancel arriving
	// during the preseed must find the turn's cancel already installed and
	// must be honoured: no turn is launched afterwards, and the untouched
	// remainder stays on the queue. The pre-fix drain installed the cancel
	// only immediately before launchTurn, so the preseed window was not
	// cancellable and the launch went ahead.
	t.Run("preseed=cancel_aborts_before_launch", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			writeTextResponse(w, "ok")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()
		rt.mu.Lock()
		unit.queue = []QueuedItem{
			{ID: "q-1", Content: "cancel me"},
			{ID: "q-2", Content: "launch"},
		}
		unit.queueSeq = 2
		unit.queueVersion = 1
		rt.mu.Unlock()

		// Park the drain at the first preseed's turn start feed, then cancel
		// the session. The cancel must succeed and actually cancel the
		// claim-time context, so the drain aborts instead of launching.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		seqMuHeld := true
		defer func() {
			if seqMuHeld {
				tr.seqMu.Unlock()
			}
		}()

		drained := make(chan struct{})
		go func() {
			defer close(drained)
			rt.tryDrainQueue(ctx)
		}()
		// Wait until the drain has claimed the unit and begun the first
		// preseed turn instead of a fixed sleep: the claim installed the turn
		// context, and the held seqMu parks the drain at the turn-start feed,
		// so the cancel below finds a live claim-time cancel and aborts the
		// drain instead of silently no-op'ing against an unclaimed unit.
		parkDeadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(parkDeadline) && unit.store.CurrentTurn() == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		if unit.store.CurrentTurn() == 0 {
			t.Fatal("drain did not begin a preseed turn before the cancel")
		}

		if err := a.CancelSession(id); err != nil {
			t.Fatalf("CancelSession: %v", err)
		}
		tr.seqMu.Unlock()
		seqMuHeld = false
		select {
		case <-drained:
		case <-time.After(15 * time.Second):
			t.Fatal("drain did not return after the cancel")
		}

		// The cancelled attempt itself launched nothing: no model request and
		// no turn_start existed before the abort's requeue. The requeue event
		// carried the real remainder with its original id.
		if got := reqs.Load(); got != 0 {
			t.Fatalf("a model turn launched by the cancelled attempt (%d requests)", got)
		}
		assertQueueChangedPayloadPresent(t, cap, []QueuedItem{{ID: "q-2", Content: "launch"}})

		// The abort's exit disposition re-arms the retained work on the live
		// owner: the requeued remainder is re-drained and launched without any
		// manual nudge.
		rearmDeadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(rearmDeadline) && reqs.Load() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		if got := reqs.Load(); got != 1 {
			t.Fatalf("retained work never re-drained after the cancel abort (model requests = %d, want 1)", got)
		}
		// The rearm's launch delivered the turn_start after the requeue; the
		// cancelled attempt delivered none.
		evs := cap.snapshot()
		requeueAt := -1
		firstTurnStart := -1
		for i, ev := range evs {
			if ev.Kind == EventQueueChanged && requeueAt < 0 && reflect.DeepEqual(ev.Queue, []QueuedItem{{ID: "q-2", Content: "launch"}}) {
				requeueAt = i
			}
			if ev.Kind == EventTurnStart && firstTurnStart < 0 {
				firstTurnStart = i
			}
		}
		if requeueAt < 0 {
			t.Fatalf("no requeue event carrying [launch(q-2)] in %#v", evs)
		}
		if firstTurnStart >= 0 && requeueAt > firstTurnStart {
			t.Fatal("a turn_start preceded the abort's requeue: the cancelled attempt launched")
		}

		// The rearm completed: the session is idle again, and the abort left
		// the same residue as the other abort paths — the per-turn context is
		// cleared, not left installed-cancelled.
		waitSessionDrained(t, a, id)
		rt.mu.Lock()
		turnCtxSet := unit.turnCtx != nil
		turnCancelSet := unit.turnCancel != nil
		rt.mu.Unlock()
		if turnCtxSet || turnCancelSet {
			t.Fatal("cancel abort left the per-turn context installed")
		}
	})

	// The drain's re-validation is not atomic with the launch: a cancel can
	// land after the revalidation passes and the runtime mutex is released,
	// but before launchTurn begins. launchTurn must reject such a handoff at
	// its receiving end, before it creates or emits anything, and unwind the
	// claim — the wait-group count and the busy flag — so the session is not
	// wedged. The handoff is driven directly, the same way
	// TestDeferredCleanupGuardContract drives launchTurn: the unit is
	// arranged as a claimed-but-not-yet-launched turn whose revalidation
	// already passed, the context is cancelled, and the call must be refused.
	// Without the rejection launchTurn accepts it: it creates a turn, emits
	// its turn_start, and returns a nonzero turn.
	//
	// The rejection's second half is the caller's: the final item was already
	// removed from the queue when the handoff is refused, so the drain must
	// put it back — the same requeue the other abort paths perform — and a
	// later drain must launch it exactly once. That part is driven through
	// the real drain with a deterministically refused launch
	// (unit.lp = nil makes launchTurn return 0 before creating anything), so
	// the closure's requeue-on-refusal is what is exercised.
	t.Run("preseed=launch_rejects_cancelled_handoff", func(t *testing.T) {
		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			writeTextResponse(w, "ok")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		cap := &eventCapture{}
		ctx := startEventOrderAgent(t, a, cap)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()
		before := unit.store.CurrentTurn()

		// Arrange the claimed-but-not-yet-launched claim the drain leaves
		// behind when it hands off: busy set, per-turn context installed, the
		// preseed-phase wait-group count held.
		turnCtx, cancel := context.WithCancel(ctx)
		rt.mu.Lock()
		unit.busy = true
		unit.turnCtx = turnCtx
		unit.turnCancel = cancel
		rt.mu.Unlock()
		rt.turnWG.Add(1)

		// The cancel lands after the revalidation passed and before the
		// launch begins.
		cancel()
		if launched := rt.launchTurn(unit, turnCtx, cancel, []string{"late cancel"}); launched != 0 {
			t.Fatalf("launchTurn accepted a cancelled handoff, returned turn %d", launched)
		}

		// Nothing was created or emitted.
		if got := unit.store.CurrentTurn(); got != before {
			t.Fatalf("CurrentTurn = %d after the rejected handoff, want unchanged %d", got, before)
		}
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				t.Fatalf("a turn_start was delivered for a cancelled handoff: %#v", ev)
			}
		}
		if got := reqs.Load(); got != 0 {
			t.Fatalf("model request after rejected handoff (%d)", got)
		}

		// The claim was unwound: busy and the per-turn context are cleared,
		// and the wait-group count is released.
		rt.mu.Lock()
		busy := unit.busy
		turnCtxSet := unit.turnCtx != nil
		turnCancelSet := unit.turnCancel != nil
		rt.mu.Unlock()
		if busy {
			t.Fatal("unit still busy after the rejected handoff")
		}
		if turnCtxSet || turnCancelSet {
			t.Fatal("unit still holds the per-turn context after the rejected handoff")
		}
		wgDone := make(chan struct{})
		go func() {
			rt.turnWG.Wait()
			close(wgDone)
		}()
		select {
		case <-wgDone:
		case <-time.After(2 * time.Second):
			t.Fatal("rejected handoff did not release the wait-group count")
		}

		// The session stays usable: a fresh submit claims, launches, and
		// drains.
		if _, err := a.Submit(ctx, "after cancel"); err != nil {
			t.Fatalf("Submit after rejected handoff: %v", err)
		}
		waitSessionDrained(t, a, id)
		if got := reqs.Load(); got != 1 {
			t.Fatalf("model requests = %d, want exactly 1 (the follow-up turn)", got)
		}

		// Second half: a launch refused through the real drain must not drop
		// the final item. Seed the queue, make launchTurn refuse the handoff
		// deterministically (lp nil returns 0 before anything is created),
		// and drain. The rearm keeps re-draining while lp is nil — each
		// attempt creates nothing and requeues — and the moment lp is
		// restored the rearm launches the item exactly once.
		before2 := unit.store.CurrentTurn()
		turnStartsBefore := 0
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				turnStartsBefore++
			}
		}
		savedLP := unit.lp
		rt.mu.Lock()
		unit.lp = nil
		unit.queue = []QueuedItem{{ID: "q-1", Content: "late cancel"}}
		unit.queueSeq = 1
		unit.queueVersion = 0
		rt.mu.Unlock()
		rt.tryDrainQueue(ctx)

		// No turn was created or emitted by the refused launch.
		if got := unit.store.CurrentTurn(); got != before2 {
			t.Fatalf("CurrentTurn = %d after the refused launch, want unchanged %d", got, before2)
		}
		turnStartsAfter := 0
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventTurnStart {
				turnStartsAfter++
			}
		}
		if turnStartsAfter != turnStartsBefore {
			t.Fatalf("turn_start count changed across the refused launch: %d -> %d", turnStartsBefore, turnStartsAfter)
		}
		if got := reqs.Load(); got != 1 {
			t.Fatalf("model requests after the refused launch = %d, want still 1", got)
		}

		// The requeue event carried the final item with its original id — the
		// refused launch put it back the same way the other abort paths do.
		assertQueueChangedPayloadPresent(t, cap, []QueuedItem{{ID: "q-1", Content: "late cancel"}})

		// The abort's exit disposition re-arms the retained work on the live
		// owner: while lp is nil every rearm attempt is refused again and
		// requeues, and the moment lp is restored the rearm launches the item
		// exactly once — no manual nudge required.
		rt.mu.Lock()
		unit.lp = savedLP
		rt.mu.Unlock()
		rearmDeadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(rearmDeadline) && reqs.Load() != 2 {
			time.Sleep(5 * time.Millisecond)
		}
		if got := reqs.Load(); got != 2 {
			t.Fatalf("retained work never re-drained after the refused launch (model requests = %d, want 2)", got)
		}
		waitSessionDrained(t, a, id)
		var liveLate int
		for _, ev := range cap.snapshot() {
			if ev.Kind == EventUserMessageDisplay && ev.Result == "late cancel" {
				liveLate++
			}
		}
		if liveLate != 1 {
			t.Fatalf("live user displays of %q = %d, want exactly once", "late cancel", liveLate)
		}
	})
}

// TestTurnBegunAfterRevertRendersOnceThroughHydration proves the committed
// bound against a reused turn number: after a combined code+history revert the
// coordinator's committedTurn is lowered to the surviving turn, so a turn
// begun after the revert that reissued an old number would compare a stale
// bound against a reused number. The bounded durable read stops at the
// surviving committed turn, the fresh turn's rows stay in the retained tail,
// and the hydration capture renders the turn exactly once.
func TestTurnBegunAfterRevertRendersOnceThroughHydration(t *testing.T) {
	a := newLiveCatalogBackedTestAgent(t)
	id := a.SessionCurrent().ID

	// Ten complete turns through the store and the coordinator: the durable
	// side records turns 1..10 and the coordinator commits each one.
	for i := 1; i <= 10; i++ {
		turn := a.store.BeginTurn()
		a.lp.AppendUserMessage(turn, fmt.Sprintf("turn %d", i))
		if err := a.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete(%d): %v", turn, err)
		}
		feedTranscript(a.transcriptForSessionID(id), Event{Kind: EventTurnStart, Turn: turn})
		feedTranscript(a.transcriptForSessionID(id), Event{Kind: EventTurnEnd, Turn: turn})
	}

	if _, err := a.ApplyTurnActionForSession(id, 6, TurnActionRevertHistory, true); err != nil {
		t.Fatalf("ApplyTurnActionForSession revert_history: %v", err)
	}

	// Begin and persist the next turn, feeding its rows into the coordinator
	// without a turn end: the completion marker lands on disk while the commit
	// has not run — the window the committed bound exists for.
	fresh := a.store.BeginTurn()
	const content = "fresh turn"
	a.lp.AppendUserMessage(fresh, content)
	if err := a.store.MarkTurnComplete(fresh); err != nil {
		t.Fatalf("MarkTurnComplete(%d): %v", fresh, err)
	}
	tr := a.transcriptForSessionID(id)
	feedTranscript(tr, Event{Kind: EventTurnStart, Turn: fresh})
	feedTranscript(tr, Event{Kind: EventUserMessageDisplay, Result: content, Turn: fresh})

	hs, err := a.HydrateSession(id)
	if err != nil {
		t.Fatalf("HydrateSession: %v", err)
	}
	if got := countHydrationContent(hs, content); got != 1 {
		t.Fatalf("fresh turn rendered %d times through hydration, want exactly once (durable messages = %d, tail rows = %d)", got, len(hs.Messages), len(hs.Tail))
	}
}

// countHydrationContent counts how many times content appears across a
// hydration's durable messages, retained tail rows, and retained error rows —
// the three halves the frontend concatenates. A row present in both the
// durable half and the tail counts twice.
func countHydrationContent(hs HydrationState, content string) int {
	n := 0
	for _, m := range hs.Messages {
		if m.Content == content {
			n++
		}
	}
	for _, r := range hs.Tail {
		if r.Message.Content == content {
			n++
		}
	}
	for _, e := range hs.Errors {
		if e.Message.Content == content {
			n++
		}
	}
	return n
}

// assertQueueChangedPayloadForVersion asserts the EventQueueChanged carrying
// version carries exactly items: a requeue must publish the real queue, not an
// empty snapshot, or every host would be told the queue is empty while items
// sit in it.
func assertQueueChangedPayloadForVersion(t *testing.T, cap *eventCapture, version int, items []QueuedItem) {
	t.Helper()
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventQueueChanged && ev.QueueVersion == version {
			if !reflect.DeepEqual(ev.Queue, items) {
				t.Fatalf("queue_changed v%d payload = %#v, want %#v", version, ev.Queue, items)
			}
			return
		}
	}
	t.Fatalf("no queue_changed event for version %d in %#v", version, cap.snapshot())
}

// assertQueueChangedPayloadPresent asserts at least one EventQueueChanged
// carries exactly items. It is the version-free form used where the live queue
// has already moved on (the abort's rearm re-drained it), so the requeue
// content is proven from the ordered event log instead.
func assertQueueChangedPayloadPresent(t *testing.T, cap *eventCapture, items []QueuedItem) {
	t.Helper()
	for _, ev := range cap.snapshot() {
		if ev.Kind == EventQueueChanged && reflect.DeepEqual(ev.Queue, items) {
			return
		}
	}
	t.Fatalf("no queue_changed event carrying %#v in %#v", items, cap.snapshot())
}

// TestDeferredCleanupGuardContract pins launchTurn's deferred cleanup guard
// directly, with no interleaving to force: the deferred cleanup must clear the
// unit's per-turn state only when the unit still holds that turn's context,
// and must leave a later claim's state untouched. The goroutine is driven to
// its early-return path (the owner context is already cancelled, so the
// post-admission checks fire and Run never starts) and the deferred cleanup
// runs against the unit in the exact state the test arranged; completion is
// observed through the deferred cleanup's own cancel() of the turn context,
// which runs after the guard. A cancelled caller context no longer drives
// this path: once a turn is admitted, its lifetime is the owner's.
func TestDeferredCleanupGuardContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "unreachable")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	startEventOrderAgent(t, a, &eventCapture{})
	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	unit := a.sessions[id]
	rt := a.ensureRuntime()

	// An already-cancelled owner context makes launchTurn's goroutine take its
	// early-return path, so the deferred cleanup runs without any turn work.
	// The turn context passed in is independent of the owner context, so the
	// receiving-end rejection (which refuses an already-cancelled handoff)
	// still admits the launch; the goroutine's post-admission check then fires
	// on the owner's lifetime.
	rt.ownerCancel()

	// runDeferred arranges the unit's per-turn state, launches a turn whose
	// deferred cleanup will run immediately, and waits for the deferred
	// cleanup to complete (it cancels thisCtx after the guard).
	runDeferred := func(unitBusy bool, unitCtx context.Context, unitCancel context.CancelFunc, thisCtx context.Context, thisCancel context.CancelFunc) {
		t.Helper()
		rt.mu.Lock()
		unit.busy = unitBusy
		unit.turnCtx = unitCtx
		unit.turnCancel = unitCancel
		rt.mu.Unlock()
		rt.turnWG.Add(1)
		rt.launchTurn(unit, thisCtx, thisCancel, []string{"x"})
		select {
		case <-thisCtx.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("deferred cleanup did not run")
		}
	}

	// A later claim replaced the unit's per-turn context (and busy flag)
	// before this turn's deferred cleanup ran: the guard must leave that
	// state untouched.
	t.Run("state=later_claim_untouched", func(t *testing.T) {
		thisCtx, thisCancel := context.WithCancel(context.Background())
		defer thisCancel()
		laterCtx, laterCancel := context.WithCancel(context.Background())
		defer laterCancel()
		runDeferred(true, laterCtx, laterCancel, thisCtx, thisCancel)

		rt.mu.Lock()
		busy := unit.busy
		turnCtx := unit.turnCtx
		turnCancel := unit.turnCancel
		rt.mu.Unlock()
		if !busy {
			t.Fatal("deferred cleanup cleared the later turn's busy flag")
		}
		if turnCtx != laterCtx {
			t.Fatal("deferred cleanup replaced the later turn's context")
		}
		if turnCancel == nil || reflect.ValueOf(turnCancel).Pointer() != reflect.ValueOf(laterCancel).Pointer() {
			t.Fatal("deferred cleanup replaced the later turn's cancel")
		}
	})

	// The unit still holds this turn's context: the deferred cleanup must
	// clear the claim so the unit is not stuck busy after an owner-cancelled
	// early return.
	t.Run("state=own_context_cleared", func(t *testing.T) {
		thisCtx, thisCancel := context.WithCancel(context.Background())
		defer thisCancel()
		runDeferred(true, thisCtx, thisCancel, thisCtx, thisCancel)

		rt.mu.Lock()
		busy := unit.busy
		turnCtx := unit.turnCtx
		turnCancel := unit.turnCancel
		rt.mu.Unlock()
		if busy {
			t.Fatal("deferred cleanup left the unit busy while it still held the turn's context")
		}
		if turnCtx != nil {
			t.Fatal("deferred cleanup left the unit's turn context set")
		}
		if turnCancel != nil {
			t.Fatal("deferred cleanup left the unit's turn cancel set")
		}
	})
}

// TestClaimCleanupKeepsLaterClaims pins the claim-ownership guard on the two
// cleanup sites that clear a unit's per-turn claim: the drain's deferred
// cleanup and launchTurn's receiving-end rejection. Each must unwind only the
// claim it owns and leave a claim installed afterwards untouched. The drain's
// defer is the dangerous one: launchTurn's rejection has already cleared the
// drain's own claim and released busy by the time the closure returns, so a
// concurrent submit can claim the unit again before the defer fires, and
// clearing then would drop the newer claim's gate while its loop is still
// running.
func TestClaimCleanupKeepsLaterClaims(t *testing.T) {
	// A later claim is taken while the drain is parked mid-preseed, after
	// its own claim was invalidated the way launchTurn's receiving-end
	// rejection leaves it: the claim-time context cancelled, busy cleared,
	// the per-turn fields nilled. The drain's deferred cleanup must leave
	// the later claim in place, and the owner shutdown that follows must
	// still drain its turn join. The drain is let to finish touching the
	// loop before the later claim's launch enters it — the two never touch
	// the loop concurrently, exactly as the busy gate serializes them in
	// production — while the later claim exists strictly before the deferred
	// cleanup runs. The claim and the launch are the same calls the submit
	// path makes (claimTurnLocked, launchTurn); only their atomic
	// claim-and-launch wrapper is stepped around, because the wrapper would
	// enter the loop before the parked drain has finished its preseed
	// append. The later turn is parked on the model request so its claim is
	// live when its state is asserted; the pre-fix cleanup clears it and the
	// first assertion fails.
	t.Run("drain_defer", func(t *testing.T) {
		release := make(chan struct{})
		reqSeen := make(chan struct{}, 1)
		var reqSeenOnce, runOnce sync.Once
		closeReqSeen := func() { reqSeenOnce.Do(func() { close(reqSeen) }) }
		closeRun := func() { runOnce.Do(func() { close(release) }) }
		// Register the server close before closeRun: cleanup runs LIFO, so a
		// parked handler is released before the server is allowed to close.
		// closeRun itself is registered after startEventOrderAgent so the
		// release runs before that cleanup's ShutdownOwner: a turn parked on
		// the request must be able to finish before the owner join.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			closeReqSeen()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			writeTextResponse(w, "ok")
		}))
		t.Cleanup(func() { server.Close() })

		a := newEventOrderAgent(t, server.URL+"/v1")
		ctx := startEventOrderAgent(t, a, &eventCapture{})
		t.Cleanup(closeRun)
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()

		// Two queued items: the first parks the drain mid-preseed, and the
		// second is the remainder the drain aborts over once its claim is
		// invalidated.
		rt.mu.Lock()
		unit.queue = []QueuedItem{
			{ID: "q-1", Content: "preseed"},
			{ID: "q-2", Content: "remainder"},
		}
		unit.queueSeq = 2
		unit.queueVersion = 1
		rt.mu.Unlock()

		// Park the drain at the first preseed's turn-start feed, then start
		// it.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		seqMuHeld := true
		defer func() {
			if seqMuHeld {
				tr.seqMu.Unlock()
			}
		}()

		drained := make(chan struct{})
		go func() {
			defer close(drained)
			rt.tryDrainQueue(ctx)
		}()
		// Wait until the drain has claimed the unit and begun the first
		// preseed turn instead of a fixed sleep: the claim installed the turn
		// context, and the held transcript lock parks the drain at the
		// turn-start feed.
		parkDeadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(parkDeadline) && unit.store.CurrentTurn() == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		if unit.store.CurrentTurn() == 0 {
			t.Fatal("drain did not begin a preseed turn before the claim was invalidated")
		}

		// Invalidate the drain's claim the way launchTurn's receiving-end
		// rejection does — cancel the claim-time context and clear the three
		// claim fields — then take the later claim through the same
		// claimTurnLocked the submit path uses, all in one runtime.mu
		// section. The later claim is installed strictly before the drain's
		// deferred cleanup can run, while the drain is still parked.
		rt.mu.Lock()
		unit.turnCancel()
		unit.busy = false
		unit.turnCancel = nil
		unit.turnCtx = nil
		laterCtx, laterCancel, err := rt.claimTurnLocked(ctx, unit)
		if err != nil {
			rt.mu.Unlock()
			t.Fatalf("claimTurnLocked: %v", err)
		}
		rt.mu.Unlock()

		// Let the drain finish its preseed — the only loop access this test
		// allows it — abort over the invalidated claim, and reach its
		// deferred cleanup. The cleanup runs against the live later claim
		// and must leave it in place.
		tr.seqMu.Unlock()
		seqMuHeld = false
		select {
		case <-drained:
		case <-time.After(15 * time.Second):
			t.Fatal("drain did not return")
		}

		// The later turn enters the loop only now, after the drain has
		// finished touching it; the launch is the same launchTurn the submit
		// path calls after its claim. Its model request parks, so the claim
		// is still live when its state is asserted.
		if launched := rt.launchTurn(unit, laterCtx, laterCancel, []string{"later claim"}); launched == 0 {
			t.Fatal("launchTurn refused the later claim")
		}
		select {
		case <-reqSeen:
		case <-time.After(15 * time.Second):
			t.Fatal("later turn did not reach the model")
		}

		// The drain's deferred cleanup must leave the later claim in place:
		// busy, the turn context, and the cancel, by pointer identity.
		rt.mu.Lock()
		busy := unit.busy
		turnCtx := unit.turnCtx
		turnCancel := unit.turnCancel
		rt.mu.Unlock()
		if !busy {
			t.Fatal("drain's deferred cleanup cleared the later claim's busy flag")
		}
		if turnCtx != laterCtx {
			t.Fatal("drain's deferred cleanup replaced the later claim's context")
		}
		if turnCancel == nil || reflect.ValueOf(turnCancel).Pointer() != reflect.ValueOf(laterCancel).Pointer() {
			t.Fatal("drain's deferred cleanup replaced the later claim's cancel")
		}

		// The counterpart: after that interleaving the owner shutdown's turn
		// join must drain — the deferred cleanup releases its wait-group
		// count even when it cleared nothing. An identity check wrapped
		// around the whole defer body skips the release and the join times
		// out.
		if !a.ShutdownOwner() {
			t.Fatal("ShutdownOwner reported an undrained turn join after the interleaving")
		}
	})

	// The receiving-end rejection clears its own claim synchronously while
	// busy still blocks a newer claim, so the guard at that site is
	// defensive; it must still never clear a claim it does not own. Drive
	// launchTurn directly with the unit holding a later claim and a
	// cancelled handoff context: the rejection must leave the later claim
	// untouched.
	t.Run("launch_reject", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTextResponse(w, "unreachable")
		}))
		defer server.Close()

		a := newEventOrderAgent(t, server.URL+"/v1")
		startEventOrderAgent(t, a, &eventCapture{})
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		unit := a.sessions[id]
		rt.mu.Unlock()

		laterCtx, laterCancel := context.WithCancel(context.Background())
		defer laterCancel()
		rejCtx, rejCancel := context.WithCancel(context.Background())
		defer rejCancel()
		rejCancel()

		rt.mu.Lock()
		unit.busy = true
		unit.turnCtx = laterCtx
		unit.turnCancel = laterCancel
		rt.mu.Unlock()
		rt.turnWG.Add(1)

		if launched := rt.launchTurn(unit, rejCtx, rejCancel, []string{"x"}); launched != 0 {
			t.Fatalf("launchTurn accepted a cancelled handoff, returned turn %d", launched)
		}
		rt.mu.Lock()
		busy := unit.busy
		turnCtx := unit.turnCtx
		turnCancel := unit.turnCancel
		rt.mu.Unlock()
		if !busy {
			t.Fatal("rejected handoff cleared the later claim's busy flag")
		}
		if turnCtx != laterCtx {
			t.Fatal("rejected handoff replaced the later claim's context")
		}
		if turnCancel == nil || reflect.ValueOf(turnCancel).Pointer() != reflect.ValueOf(laterCancel).Pointer() {
			t.Fatal("rejected handoff replaced the later claim's cancel")
		}
	})
}
