package harness

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The capture-availability contract regressions: SnapshotSession and
// ReadOperation must decide one Session's availability inside the same
// coordinator-mutex hold that copies their returned state, so a corruption
// marker that a real transition sticks on a warm coordinator between a
// reader's coordinatorFor lookup and its capture refuses that capture.
//
// The window's real producer is Fork: its store-wide transaction runs outside
// the source coordinator's c.mu and fails on a corrupted durable session
// register (decodeSessionRegister -> corruptSession, fork.go), so markCorrupt
// sticks the warm source's corruption marker under h.mu without holding c.mu.
// No test injects a marker directly on these rows.
//
// The rendezvous is deterministic, with no sleeps and no production changes:
// the shared prepareStub gate parks Fork before its transaction; the row then
// holds c.mu and starts a named reader goroutine; a goroutine-stack barrier
// waits until the reader's stack shows it parked in goneErr on that mutex —
// the state that proves its corru sample (read before the goneErr call, in
// program order) already returned nil. Releasing the Fork gate lets the
// transaction fail and the real marker land while the reader is parked; only
// then is c.mu released and the reader's capture proceeds, strictly after the
// mark. Every wait is bounded by the shared barrier deadline, and the
// rendezvous registers one failure-safe release — the gate, then the
// test-held mutex, then a bounded join of the parked reader and Fork — before
// any wait starts, so no exit path, including a mid-rendezvous t.Fatal with
// the mutex held or the gate armed, can strand them.

const captureBarrierWait = 5 * time.Second

// corruptRegisterLifecyclePayload rewrites one session register payload with
// the same invalid lifecycle value the graph_test sessionRawOverride fixture
// uses, so decodeSessionRegister rejects it.
func corruptRegisterLifecyclePayload(t *testing.T, regPayload json.RawMessage) json.RawMessage {
	t.Helper()
	state, err := decodePayloadObject(mustState(t, regPayload))
	mustEncode(t, err)
	state["lifecycle"] = json.RawMessage(`"stopping"`)
	patchedState, err := json.Marshal(state)
	mustEncode(t, err)
	obj := wireObject(t, regPayload)
	obj["state"] = patchedState
	patched, err := json.Marshal(obj)
	mustEncode(t, err)
	return patched
}

// foreignCorruptRegister corrupts the durable session register in place
// through a plain test transaction, the way an external writer would.
func foreignCorruptRegister(t *testing.T, store *graphStorage, sessionID string) {
	t.Helper()
	err := store.Transact(context.Background(), func(tx Transaction) error {
		key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		patched := corruptRegisterLifecyclePayload(t, reg.Payload)
		_, err = tx.ReplaceRegister(key, reg.Revision, patched)
		return err
	})
	if err != nil {
		t.Fatalf("foreign corrupt register write: %v", err)
	}
}

// warmCorruptFixture builds one warm Session whose cached graph carries a
// settled operation admitted and settled through the real production paths,
// one valid sibling Session, and a durable session register corrupted by a
// foreign test write so the real Fork transaction fails on it. It returns the
// harness, the store, the preparation stub, the subject's cached coordinator,
// and the sibling's identity.
func warmCorruptFixture(t *testing.T) (*Harness, *graphStorage, *prepareStub, *coordinator, string) {
	t.Helper()
	store := freshSessionStore(t)
	stub := newPrepareStub(validPrepared())
	h := newTestHarness(t, store, stub.prepare)
	ctx := context.Background()
	if _, err := h.ReadSessionHeader(ctx, testSessionID); err != nil {
		t.Fatalf("warm ReadSessionHeader: %v", err)
	}
	sibling := createSession(t, h)
	c := cachedCoordinator(t, h, testSessionID)
	mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello"))
	if _, err := h.commitEffectResult(ctx, c, testOpID, nil, modelResult{terminal: OperationSuccess}); err != nil {
		t.Fatalf("terminal settlement: %v", err)
	}
	if header, err := h.ReadSessionHeader(ctx, testSessionID); err != nil || header.CurrentOperationID != "" {
		t.Fatalf("post-settlement header = %+v (%v), want an idle Session", header, err)
	}
	foreignCorruptRegister(t, store, testSessionID)
	return h, store, stub, c, sibling
}

// drainPreparations consumes every buffered preparation arrival, so the next
// one belongs to the gated producer under test.
func drainPreparations(stub *prepareStub) {
	for {
		select {
		case <-stub.arrived:
			continue
		default:
		}
		break
	}
}

// waitForParkedReader blocks until exactly one goroutine's stack section shows
// the named reader parked on the coordinator mutex inside goneErr: the section
// contains the reader's marker function, the coordinatorFor and goneErr frames,
// and the [sync.Mutex.Lock] park state. Parking inside goneErr proves the
// reader's corru sample — read before the goneErr call in program order —
// already returned nil, while the capture has not started. It fails the test
// with the dump when the state is not reached within the bounded wait.
func waitForParkedReader(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(captureBarrierWait)
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		dump := string(buf[:n])
		matches := 0
		for _, section := range strings.Split(dump, "\n\n") {
			if !strings.Contains(section, marker) ||
				!strings.Contains(section, "coordinatorFor") ||
				!strings.Contains(section, "goneErr") {
				continue
			}
			header := section
			if lines := strings.SplitN(section, "\n", 2); len(lines) > 0 {
				header = lines[0]
			}
			if strings.Contains(header, "[sync.Mutex.Lock]") {
				matches++
			}
		}
		if matches == 1 {
			return
		}
		if matches > 1 {
			t.Fatalf("stack barrier %s: %d goroutines matched, want exactly 1", marker, matches)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("stack barrier %s: the reader never parked in goneErr within %s; dump:\n%s", marker, captureBarrierWait, dump)
		}
		runtime.Gosched()
	}
}

// awaitCorruptMark waits (bounded, yielding the processor while waiting for
// this explicit event) until the warm coordinator carries its sticky
// corruption marker, and reports whether it landed.
func awaitCorruptMark(h *Harness, c *coordinator) (error, bool) {
	deadline := time.Now().Add(captureBarrierWait)
	for {
		h.mu.Lock()
		corru := c.corru
		h.mu.Unlock()
		if corru != nil {
			return corru, true
		}
		if !time.Now().Before(deadline) {
			return nil, false
		}
		runtime.Gosched()
	}
}

// receiveBounded waits for one rendezvous outcome channel with the shared
// barrier deadline, so no completion wait can hang a row.
func receiveBounded[T any](t *testing.T, from <-chan T, what string) T {
	t.Helper()
	timer := time.NewTimer(captureBarrierWait)
	defer timer.Stop()
	select {
	case v := <-from:
		return v
	case <-timer.C:
		var zero T
		t.Fatalf("%s within %s", what, captureBarrierWait)
		return zero
	}
}

// joinBounded waits for the rendezvous goroutines with the shared barrier
// deadline. A timeout means a released goroutine still runs, so the row fails
// rather than hanging; its helper waiter dies with the test process.
func joinBounded(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(captureBarrierWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatalf("the rendezvous goroutines did not join within %s", captureBarrierWait)
	}
}

// snapshotResult is one SnapshotSession reader goroutine's outcome.
type snapshotResult struct {
	snap SessionSnapshot
	err  error
}

// snapshotReaderMarker is the named SnapshotSession reader goroutine root.
func snapshotReaderMarker(ctx context.Context, h *Harness, sessionID string, out chan<- snapshotResult) {
	snap, err := h.SnapshotSession(ctx, sessionID)
	out <- snapshotResult{snap: snap, err: err}
}

// operationResult is one ReadOperation reader goroutine's outcome.
type operationResult struct {
	rec OperationRecord
	err error
}

// operationReaderMarker is the named ReadOperation reader goroutine root.
func operationReaderMarker(ctx context.Context, h *Harness, sessionID, operationID string, out chan<- operationResult) {
	rec, err := h.ReadOperation(ctx, sessionID, operationID)
	out <- operationResult{rec: rec, err: err}
}

// forkMarker is the named Fork goroutine root; its deferred reservation
// release parks on the test-held c.mu once the mark landed.
func forkMarker(ctx context.Context, h *Harness, req ForkRequest, out chan<- error) {
	_, err := h.Fork(ctx, req)
	out <- err
}

// assertCaptureCorruption asserts one reader outcome is the typed corruption
// error of the marked Session, not captured state.
func assertCaptureCorruption(t *testing.T, err error, sessionID, read string) {
	t.Helper()
	if !isCorruption(err) {
		t.Fatalf("%s after the mark = %v, want the typed corruption error", read, err)
	}
	var corrupt *CorruptionError
	if !errors.As(err, &corrupt) || corrupt.SessionID != sessionID {
		t.Fatalf("%s error = %v, want the typed CorruptionError naming %q", read, err, sessionID)
	}
}

// runMarkWindow performs the shared deterministic window rendezvous for one
// reader goroutine against the warm source of one real Fork transition: it
// arms the preparation gate, starts Fork and parks it in preparation, holds
// the coordinator mutex, starts the named reader and waits until it is
// provably past its corruption-marker sample (parked in goneErr), releases
// the gate so Fork's own register-decode corruption path sticks the marker on
// the warm source, awaits the marker, and releases the mutex so the reader's
// capture proceeds strictly after the mark. One deferred failure-safe release
// — the gate, then the test-held mutex, then a bounded join of the reader and
// Fork — is registered before any wait starts, so no exit path can strand the
// parked reader or the gated Fork; the join also guarantees both outcome
// channels carry their values before the rendezvous returns. The sticky
// marker must be the Fork transition's own register corruption naming the
// source Session.
func runMarkWindow[T any](t *testing.T, ctx context.Context, h *Harness, stub *prepareStub, c *coordinator,
	req ForkRequest, readerMarker string, startReader func(done chan<- T)) (reader chan T, forkDone chan error) {
	t.Helper()

	drainPreparations(stub)
	gate := make(chan struct{})
	releaseGate := sync.OnceFunc(func() { close(gate) })
	stub.gate = gate

	var joins sync.WaitGroup
	held := false
	defer func() { // failure-safe release on every exit: gate, then lock, then join
		releaseGate()
		if held {
			held = false
			c.mu.Unlock()
		}
		joinBounded(t, &joins)
	}()

	forkDone = make(chan error, 1)
	joins.Add(1)
	go func() { defer joins.Done(); forkMarker(ctx, h, req, forkDone) }()
	select {
	case <-stub.arrived: // Fork holds the source reservation and parks in preparation
	case <-time.After(captureBarrierWait):
		t.Fatalf("the gated Fork preparation never arrived within %s", captureBarrierWait)
	}

	c.mu.Lock() // the lock rendezvous: the reader parks inside coordinatorFor
	held = true

	reader = make(chan T, 1)
	joins.Add(1)
	go func() { defer joins.Done(); startReader(reader) }()
	waitForParkedReader(t, readerMarker) // parked in goneErr: the corru sample (nil) is provably complete

	releaseGate() // the transaction fails on the corrupt register and the real mark lands, with no c.mu held

	marker, landed := awaitCorruptMark(h, c)
	held = false
	c.mu.Unlock() // the reader's capture proceeds, strictly after the mark
	if !landed {
		t.Fatalf("the real Fork transition's corruption marker did not land within %s", captureBarrierWait)
	}
	var corrupt *CorruptionError
	if !errors.As(marker, &corrupt) || corrupt.SessionID != req.SourceSessionID ||
		!strings.Contains(corrupt.Detail, "session register") {
		t.Fatalf("sticky marker = %v, want the Fork transition's register corruption naming %q", marker, req.SourceSessionID)
	}
	return reader, forkDone
}

// TestCorruptMarkBetweenLookupAndCapture is the window regression: a reader
// whose coordinatorFor lookup passed before the real Fork transition stuck the
// corruption marker on the warm source must still refuse its capture, because
// the availability decision runs inside the capture hold. Both reads share the
// window; each subtest also proves the pre-marker nearest valid sibling — the
// same warm coordinator captures its complete state and the copy is owned.
func TestCorruptMarkBetweenLookupAndCapture(t *testing.T) {
	t.Run("SnapshotSession refuses a capture after the mark", func(t *testing.T) {
		ctx := context.Background()
		h, _, stub, c, _ := warmCorruptFixture(t)

		snap, err := h.SnapshotSession(ctx, testSessionID)
		if err != nil {
			t.Fatalf("pre-marker SnapshotSession: %v", err)
		}
		if len(snap.Operations) != 1 || snap.Operations[0].Admission.OperationID != testOpID ||
			snap.Operations[0].State.Status != OperationSuccess {
			t.Fatalf("pre-marker operations = %+v, want the settled %s", snap.Operations, testOpID)
		}
		if snap.ExecutionBusy {
			t.Fatalf("pre-marker snapshot = busy, want the idle post-settlement state")
		}
		// The captured copy is owned: mutating it cannot reach the coordinator.
		snap.Operations[0].State.Status = OperationRunning
		if snap.Facts[0].Input != nil {
			snap.Facts[0].Input.Content[0].Text = "mutated"
		}
		again, err := h.SnapshotSession(ctx, testSessionID)
		if err != nil {
			t.Fatalf("pre-marker reread: %v", err)
		}
		if again.Operations[0].State.Status != OperationSuccess ||
			(snap.Facts[0].Input != nil && again.Facts[0].Input.Content[0].Text == "mutated") {
			t.Fatalf("a mutation of the returned snapshot reached the coordinator: %+v", again)
		}

		reader, forkDone := runMarkWindow(t, ctx, h, stub, c, ForkRequest{
			SourceSessionID: testSessionID,
			BoundaryEntryID: hexID(1),
			OperationID:     "op-fork-1",
			Content:         admissionContent("fork"),
		}, "snapshotReaderMarker", func(done chan<- snapshotResult) {
			snapshotReaderMarker(ctx, h, testSessionID, done)
		})

		result := receiveBounded(t, reader, "the SnapshotSession reader never completed")
		assertCaptureCorruption(t, result.err, testSessionID, "SnapshotSession")
		forkErr := receiveBounded(t, forkDone, "Fork never completed")
		if !isCorruption(forkErr) {
			t.Fatalf("Fork = %v, want its own register-decode corruption", forkErr)
		}
	})

	t.Run("ReadOperation refuses a capture after the mark", func(t *testing.T) {
		ctx := context.Background()
		h, _, stub, c, _ := warmCorruptFixture(t)

		rec, err := h.ReadOperation(ctx, testSessionID, testOpID)
		if err != nil {
			t.Fatalf("pre-marker ReadOperation: %v", err)
		}
		if rec.Admission.OperationID != testOpID || rec.State.Status != OperationSuccess {
			t.Fatalf("pre-marker record = %+v, want the settled %s", rec, testOpID)
		}
		rec.State.Status = OperationFailure
		reread, err := h.ReadOperation(ctx, testSessionID, testOpID)
		if err != nil {
			t.Fatalf("pre-marker reread: %v", err)
		}
		if reread.State.Status != OperationSuccess {
			t.Fatalf("a mutation of the returned record reached the coordinator: %+v", reread)
		}

		reader, forkDone := runMarkWindow(t, ctx, h, stub, c, ForkRequest{
			SourceSessionID: testSessionID,
			BoundaryEntryID: hexID(1),
			OperationID:     "op-fork-2",
			Content:         admissionContent("fork"),
		}, "operationReaderMarker", func(done chan<- operationResult) {
			operationReaderMarker(ctx, h, testSessionID, testOpID, done)
		})

		result := receiveBounded(t, reader, "the ReadOperation reader never completed")
		assertCaptureCorruption(t, result.err, testSessionID, "ReadOperation")
		forkErr := receiveBounded(t, forkDone, "Fork never completed")
		if !isCorruption(forkErr) {
			t.Fatalf("Fork = %v, want its own register-decode corruption", forkErr)
		}
	})
}

// TestCaptureAvailabilityFamily pins the surrounding semantic rows of the
// shared in-hold availability decision: a gone Session answers not-found from
// both reads, a valid sibling stays fully readable and listed around the marked
// subject, a warm-marked Session is unavailable to the whole read family, and
// a cold corrupt register refuses a fresh harness through validation.
func TestCaptureAvailabilityFamily(t *testing.T) {
	t.Run("gone session answers not-found from both reads", func(t *testing.T) {
		ctx := context.Background()
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(validPrepared()).prepare)
		if _, err := h.ReadSessionHeader(ctx, testSessionID); err != nil {
			t.Fatalf("warm ReadSessionHeader: %v", err)
		}
		if _, err := h.ArchiveSession(ctx, testSessionID); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if err := h.DeleteSession(ctx, testSessionID); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		if _, err := h.SnapshotSession(ctx, testSessionID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SnapshotSession after deletion = %v, want the not-found class", err)
		}
		if _, err := h.ReadOperation(ctx, testSessionID, testOpID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ReadOperation after deletion = %v, want the not-found class", err)
		}
	})

	t.Run("valid sibling stays readable and listed around the marked subject", func(t *testing.T) {
		ctx := context.Background()
		h, _, _, _, sibling := warmCorruptFixture(t)

		// The same real transition installs the marker on the warm source.
		forkDone := make(chan error, 1)
		go forkMarker(ctx, h, ForkRequest{
			SourceSessionID: testSessionID,
			BoundaryEntryID: hexID(1),
			OperationID:     "op-fork-sibling",
			Content:         admissionContent("fork"),
		}, forkDone)
		if err := receiveBounded(t, forkDone, "Fork never completed"); !isCorruption(err) {
			t.Fatalf("Fork over the corrupt register = %v, want its register-decode corruption", err)
		}

		listed, err := h.ListSessions(ctx)
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		if len(listed) != 1 || listed[0].Identity.SessionID != sibling {
			t.Fatalf("ListSessions = %+v, want exactly the valid sibling %q", listed, sibling)
		}
		if _, err := h.SnapshotSession(ctx, sibling); err != nil {
			t.Fatalf("sibling SnapshotSession = %v, want the valid read", err)
		}
		if _, err := h.ReadSessionHeader(ctx, sibling); err != nil {
			t.Fatalf("sibling ReadSessionHeader = %v, want the valid read", err)
		}
		// The sibling's own absent Operation answers its not-found class, not
		// the subject's corruption: the marker never crosses coordinators.
		if _, err := h.ReadOperation(ctx, sibling, "op-absent"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("sibling ReadOperation = %v, want the sibling's own not-found class", err)
		}
	})

	t.Run("warm-marked session is unavailable to the whole read family", func(t *testing.T) {
		ctx := context.Background()
		h, _, _, _, _ := warmCorruptFixture(t)

		forkDone := make(chan error, 1)
		go forkMarker(ctx, h, ForkRequest{
			SourceSessionID: testSessionID,
			BoundaryEntryID: hexID(1),
			OperationID:     "op-fork-family",
			Content:         admissionContent("fork"),
		}, forkDone)
		if err := receiveBounded(t, forkDone, "Fork never completed"); !isCorruption(err) {
			t.Fatalf("Fork over the corrupt register = %v, want its register-decode corruption", err)
		}

		if _, err := h.SnapshotSession(ctx, testSessionID); !isCorruption(err) {
			t.Fatalf("fresh SnapshotSession = %v, want the corruption class", err)
		}
		if _, err := h.ReadOperation(ctx, testSessionID, testOpID); !isCorruption(err) {
			t.Fatalf("fresh ReadOperation = %v, want the corruption class", err)
		}
		if _, err := h.ReadSessionHeader(ctx, testSessionID); !isCorruption(err) {
			t.Fatalf("ReadSessionHeader = %v, want the corruption class", err)
		}
		if _, _, ok := h.ReadObservation(testSessionID); ok {
			t.Fatal("ReadObservation reported a corrupt Session available")
		}
	})

	t.Run("cold corrupt register refuses a fresh harness through validation", func(t *testing.T) {
		ctx := context.Background()
		_, store, _, _, _ := warmCorruptFixture(t)
		cold := newTestHarness(t, store, nil)

		if _, err := cold.SnapshotSession(ctx, testSessionID); !isCorruption(err) {
			t.Fatalf("cold SnapshotSession = %v, want the corruption class", err)
		}
		if _, err := cold.ReadOperation(ctx, testSessionID, testOpID); !isCorruption(err) {
			t.Fatalf("cold ReadOperation = %v, want the corruption class", err)
		}
	})
}
