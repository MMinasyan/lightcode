package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// The preparation-reservation ownership rows: every discard path releases the
// transferred reservation exactly once, every adoption path releases it exactly
// once after the opened execution's Close, and a release failure is retained
// through the Wait channel without rewriting a committed terminal.

// reservationCounters is a fixture Release: it counts every call and answers
// with the fixed error when one is configured.
type reservationCounters struct {
	releases atomic.Int64
	err      error
}

func (r *reservationCounters) release() error {
	r.releases.Add(1)
	return r.err
}

// releasingHarness builds one cancelable Harness whose preparation answers every
// call with the given prepared execution.
func releasingHarness(t *testing.T, store Storage, prepared PreparedExecution) (*Harness, context.CancelFunc) {
	t.Helper()
	h, cancel := newCancelableHarness(t, store, prepared, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return prepared, nil
	})
	return h, cancel
}

// admitWithoutInstall runs one reserved admission and returns the adopted
// prepared value without releasing it — the fixture shape for driving the
// execution directly on it.
func admitWithoutInstall(t *testing.T, h *Harness, sessionID, operationID string, content []model.ContentPart) (*coordinator, PreparedExecution) {
	t.Helper()
	c := leaseCoordinator(t, h, sessionID)
	release, err := h.reserve(context.Background(), c)
	testassert.NoError(t, err)
	defer release()
	rec, prepared, disposition, err := h.admitReserved(context.Background(), c, admissionRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Kind:        RequestKindMessage,
		Origin:      InputOriginUser,
		Content:     content,
	})
	testassert.NoError(t, err)
	testassert.Require(t, disposition == DispositionAdmitted && prepared != nil, "disposition %q (prepared %v), want a new admission", disposition, prepared != nil)
	_ = rec
	return c, *prepared
}

func TestPreparationReservationReleasesOnDiscards(t *testing.T) {
	t.Run("invalid capture discards with release", func(t *testing.T) {
		counters := &reservationCounters{}
		opens := 0
		prepared := PreparedExecution{
			Capture: ExecutionCapture{}, // an invalid capture: the Harness never adopts it
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				opens++
				return validExecution(), nil
			},
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, freshSessionStore(t), prepared)
		defer cancel()
		_, err := submitText(t, h, testSessionID, "op-1", MessageModeRegular, "hello")
		testassert.Require(t, errors.Is(err, ErrInvalid), "Submit = %v, want the invalid-capture rejection", err)
		testassert.Equal(t, counters.releases.Load(), int64(1))
		testassert.Equal(t, opens, 0)
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
		}
	})

	t.Run("nil opener discards with release", func(t *testing.T) {
		counters := &reservationCounters{}
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open:    nil,
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, freshSessionStore(t), prepared)
		defer cancel()
		_, err := submitText(t, h, testSessionID, "op-1", MessageModeRegular, "hello")
		testassert.Require(t, errors.Is(err, ErrInvalid), "Submit = %v, want the nil-opener rejection", err)
		testassert.Equal(t, counters.releases.Load(), int64(1))
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
		}
	})

	t.Run("cancellation after preparation discards with release", func(t *testing.T) {
		store := freshSessionStore(t)
		counters := &reservationCounters{}
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open:    func(context.Context, OperationAdmission) (Execution, error) { return validExecution(), nil },
			Release: counters.release,
		}
		callerCtx, callerCancel := context.WithCancel(context.Background())
		defer callerCancel()
		h, cancel := newCancelableHarness(t, store, prepared, func(context.Context, PreparationRequest) (PreparedExecution, error) {
			callerCancel() // the caller dies while this preparation is completing
			return prepared, nil
		})
		defer cancel()
		_, err := h.Submit(callerCtx, SubmitRequest{
			SessionID:   testSessionID,
			OperationID: "op-1",
			Origin:      InputOriginUser,
			Content:     admissionContent("hello"),
			Mode:        MessageModeRegular,
		})
		testassert.Require(t, errors.Is(err, context.Canceled), "Submit = %v, want the post-Prepare cancellation", err)
		testassert.Equal(t, counters.releases.Load(), int64(1))
		{
			_, regs := storedSessionState(store, testSessionID)
			testassert.Equal(t, len(regs), 1) // only the Session register: nothing published
		}
	})

	t.Run("harness shutdown after successful preparation discards with release", func(t *testing.T) {
		store := freshSessionStore(t)
		counters := &reservationCounters{}
		opens := 0
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				opens++
				return validExecution(), nil
			},
			Release: counters.release,
		}
		arrived := make(chan struct{}, 1)
		gate := make(chan struct{})
		hctx, cancel := context.WithCancel(context.Background())
		h, err := New(hctx, Dependencies{Storage: store, Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) {
			arrived <- struct{}{}
			<-gate // the preparation only completes after the Harness shutdown lands
			return prepared, nil
		}})
		testassert.NoError(t, err)
		submitted := make(chan error, 1)
		go func() {
			_, err := submitText(t, h, testSessionID, "op-1", MessageModeRegular, "hello")
			submitted <- err
		}()
		<-arrived
		cancel() // the Harness context dies while the returned preparation is being validated
		close(gate)
		err = <-submitted
		testassert.Require(t, errors.Is(err, context.Canceled), "Submit = %v, want the harness-shutdown discard", err)
		testassert.Equal(t, counters.releases.Load(), int64(1))
		testassert.Equal(t, opens, 0)
		{
			_, regs := storedSessionState(store, testSessionID)
			testassert.Equal(t, len(regs), 1) // only the Session register: nothing published
		}
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
		}
	})

	t.Run("duplicate identity resolving after publication discards with release", func(t *testing.T) {
		store := freshSessionStore(t)
		counters := &reservationCounters{}
		opens := 0
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				opens++
				return validExecution(), nil
			},
			Release: counters.release,
		}
		h, cancel := newCancelableHarness(t, store, prepared, func(context.Context, PreparationRequest) (PreparedExecution, error) {
			// While this admission prepares outside the lock, another producer
			// durably admits the same identity: publication resolves it as
			// existing and this prepared value is discarded.
			seed := validSessionRecord()
			seed.State.CurrentOperationID = testOpID
			seed.Revision += 1
			seedSessionRaw, err := encodeSessionRegister(seed)
			testassert.NoError(t, err)
			input := validInputEntry(testOpID)
			input.EntryID = hexID(1)
			inputRaw, err := encodeInputEntry(input)
			testassert.NoError(t, err)
			op := validOperationRecord()
			opRaw, err := encodeOperationRegister(op)
			testassert.NoError(t, err)
			err = store.Transact(context.Background(), func(tx Transaction) error {
				if _, err := tx.InsertEntry(EntryDraft{SessionID: testSessionID, ID: hexID(1), OperationID: testOpID, Kind: EntryInput, Payload: inputRaw}); err != nil {
					return err
				}
				if _, err := tx.InsertRegister(RegisterDraft{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: opRaw}); err != nil {
					return err
				}
				reg, err := tx.ReadRegister(RegisterKey{SessionID: testSessionID, Kind: RegisterSession})
				if err != nil {
					return err
				}
				_, err = tx.ReplaceRegister(reg.Key, reg.Revision, seedSessionRaw)
				return err
			})
			testassert.NoError(t, err)
			return prepared, nil
		})
		defer cancel()
		rec, preparedOut, disposition, err := func() (OperationRecord, *PreparedExecution, SubmitDisposition, error) {
			c := leaseCoordinator(t, h, testSessionID)
			release, err := h.reserve(context.Background(), c)
			testassert.NoError(t, err)
			defer release()
			return h.admitReserved(context.Background(), c, admissionRequest{
				SessionID:   testSessionID,
				OperationID: testOpID,
				Kind:        RequestKindMessage,
				Origin:      InputOriginUser,
				Content:     admissionContent("hello"),
			})
		}()
		testassert.NoError(t, err)
		testassert.Equal(t, disposition, DispositionExisting)
		testassert.Require(t, preparedOut == nil, "existing resolution carried a prepared execution to adopt: %v", preparedOut)
		testassert.Require(t, rec.Admission.OperationID == testOpID, "existing record = %+v, want the seeded identity", rec.Admission)
		testassert.Equal(t, counters.releases.Load(), int64(1))
		testassert.Equal(t, opens, 0)
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
		}
	})

	t.Run("publication storage failure discards with release", func(t *testing.T) {
		store := freshSessionStore(t)
		counters := &reservationCounters{}
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open:    func(context.Context, OperationAdmission) (Execution, error) { return validExecution(), nil },
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, store, prepared)
		defer cancel()
		store.txHook = func(step string) error {
			if step == "insert_entry" {
				return fmt.Errorf("%w: admission publication down", ErrStorage)
			}
			return nil
		}
		_, err := submitText(t, h, testSessionID, "op-1", MessageModeRegular, "hello")
		testassert.Require(t, errors.Is(err, ErrStorage), "Submit = %v, want the storage failure", err)
		testassert.Equal(t, counters.releases.Load(), int64(1))
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err) // the storage failure belongs to the admission latch, not the release
		}
	})

	t.Run("failed fork discards with release", func(t *testing.T) {
		store := freshSessionStore(t)
		settled := newPrepareStub(validPrepared())
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, settled.prepare)
		defer cancel()
		{
			_, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		c := cachedCoordinator(t, h, testSessionID)
		{
			_, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationSuccess})
			testassert.NoError(t, err) // the idle source the fork reads
		}
		store.mu.Lock()
		var boundary string
		for _, entry := range store.entries[testSessionID] {
			if entry.Kind == EntryInput {
				boundary = entry.ID
			}
		}
		store.mu.Unlock()
		testassert.Require(t, boundary != "", "the source graph carries no input entry")
		counters := &reservationCounters{}
		opens := 0
		h.deps.Prepare = func(context.Context, PreparationRequest) (PreparedExecution, error) {
			return PreparedExecution{
				Capture: testCapture(),
				Open: func(context.Context, OperationAdmission) (Execution, error) {
					opens++
					return validExecution(), nil
				},
				Release: counters.release,
			}, nil
		}
		store.txHook = func(step string) error {
			if step == "insert_entry" {
				return fmt.Errorf("%w: fork transaction down", ErrStorage)
			}
			return nil
		}
		_, err := h.Fork(context.Background(), ForkRequest{
			SourceSessionID: testSessionID,
			BoundaryEntryID: boundary,
			OperationID:     "fork-down",
			Content:         admissionContent("forked"),
		})
		testassert.Require(t, errors.Is(err, ErrStorage), "Fork = %v, want the storage failure", err)
		testassert.Equal(t, counters.releases.Load(), int64(1))
		testassert.Equal(t, opens, 0)
	})

	t.Run("cap-rejected child launch discards with release", func(t *testing.T) {
		counters := &reservationCounters{}
		opens := 0
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				opens++
				return validExecution(), nil
			},
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, freshSessionStore(t), prepared)
		defer cancel()
		c := leaseCoordinator(t, h, testSessionID)
		{
			_, err := admitLocked(h, c, memberChild, "existing-child", 0)
			testassert.NoError(t, err)
		}
		req := launchRequest()
		req.MaxConcurrent = 1
		_, err := h.LaunchChildSession(context.Background(), req)
		testassert.Require(t, err != nil, "capped launch = nil, want the cap rejection")
		testassert.Equal(t, counters.releases.Load(), int64(1))
		testassert.Equal(t, opens, 0)
	})

	t.Run("launch transaction failure discards with release", func(t *testing.T) {
		store := freshSessionStore(t)
		counters := &reservationCounters{}
		opens := 0
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				opens++
				return validExecution(), nil
			},
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, store, prepared)
		defer cancel()
		store.txHook = func(step string) error {
			if step == "insert_entry" {
				return fmt.Errorf("%w: launch transaction down", ErrStorage)
			}
			return nil
		}
		_, err := h.LaunchChildSession(context.Background(), launchRequest())
		store.txHook = nil
		testassert.Require(t, errors.Is(err, ErrStorage), "launch = %v, want the launch-transaction failure", err)
		testassert.Equal(t, counters.releases.Load(), int64(1))
		testassert.Equal(t, opens, 0) // no adoption: the child execution never started
		c := cachedCoordinator(t, h, testSessionID)
		c.mu.Lock()
		live := c.group != nil && len(c.group.members) > 0
		c.mu.Unlock()
		testassert.Require(t, !live, "the failed launch left the parent member live")
	})

	t.Run("rejected handoff child launch discards with release", func(t *testing.T) {
		store := freshSessionStore(t)
		counters := &reservationCounters{}
		opens := 0
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				opens++
				return validExecution(), nil
			},
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, store, prepared)
		defer cancel()
		c := leaseCoordinator(t, h, testSessionID)
		store.txHook = func(string) error { // the group closes between the launch commit and the handoff
			c.mu.Lock()
			c.bgState = bgStopping
			c.mu.Unlock()
			return nil
		}
		_, err := h.LaunchChildSession(context.Background(), launchRequest())
		store.txHook = nil
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "closed or stopping"), "rejected handoff launch = %v, want the closed-or-stopping rejection", err)
		testassert.Equal(t, counters.releases.Load(), int64(1))
		testassert.Equal(t, opens, 0) // no adoption: the rejected launch never starts the child execution
	})

	t.Run("failed child materialization discards with release", func(t *testing.T) {
		store := freshSessionStore(t)
		counters := &reservationCounters{}
		opens := 0
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				opens++
				return validExecution(), nil
			},
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, store, prepared)
		defer cancel()
		// Armed on the launch transaction's entry insert: the committed child's
		// first graph read fails, so the launch never installs the execution.
		store.entryHook = func(draft EntryDraft) error {
			if draft.Kind == EntryInput {
				store.registersErr = fmt.Errorf("%w: child graph load down", ErrStorage)
			}
			return nil
		}
		_, err := h.LaunchChildSession(context.Background(), launchRequest())
		store.registersErr = nil
		testassert.Require(t, errors.Is(err, ErrStorage), "launch = %v, want the materialization read failure", err)
		testassert.Equal(t, counters.releases.Load(), int64(1))
		testassert.Equal(t, opens, 0) // no adoption: the child execution never started
		childID := ""
		store.mu.Lock()
		for id := range store.registers {
			if id != testSessionID {
				childID = id
			}
		}
		store.mu.Unlock()
		testassert.Require(t, childID != "", "the committed child register is missing") // the durable child stays for recovery
	})

	t.Run("dropped buffer delivery discards with release", func(t *testing.T) {
		store := freshSessionStore(t)
		counters := &reservationCounters{}
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open:    func(context.Context, OperationAdmission) (Execution, error) { return validExecution(), nil },
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, store, prepared)
		defer cancel()
		{
			_, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		c := cachedCoordinator(t, h, testSessionID)
		{
			_, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationSuccess})
			testassert.NoError(t, err) // the committed terminal the drain publishes after
		}
		item := &pendingMessage{operationID: "drain-down", origin: InputOriginUser, content: admissionContent("buffered")}
		c.mu.Lock()
		c.steering = append(c.steering, item)
		c.mu.Unlock()
		testassert.Equal(t, counters.releases.Load(), int64(1)) // the fixture admission it never installed already released
		store.txHook = func(step string) error {
			if step == "insert_entry" {
				return fmt.Errorf("%w: drain admission down", ErrStorage)
			}
			return nil
		}
		release, err := h.reserve(context.Background(), c)
		testassert.NoError(t, err)
		h.drainBuffers(c, &activeExecution{done: make(chan struct{}), execCtx: context.Background(), cancel: func() {}})
		release()
		testassert.Equal(t, counters.releases.Load(), int64(2)) // the failed delivery released the prepared value it never adopted
		c.mu.Lock()
		buffered := len(c.steering) + len(c.queued)
		c.mu.Unlock()
		testassert.Equal(t, buffered, 0) // one failed attempt is final for the item
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
		}
	})
}

func TestAdoptedReservationReleasesOnceAfterClose(t *testing.T) {
	t.Run("skipped Open releases the adopted reservation", func(t *testing.T) {
		counters := &reservationCounters{}
		opens := 0
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				opens++
				return validExecution(), nil
			},
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, freshSessionStore(t), prepared)
		c, adopted := admitWithoutInstall(t, h, testSessionID, "op-1", admissionContent("hello"))
		cancel() // the execution context is lost before the opener could start
		{
			err := executeDirect(h, c, h.ctx, "op-1", adopted)
			testassert.Require(t, errors.Is(err, context.Canceled), "executeDirect = %v, want the context error", err)
		}
		testassert.Equal(t, opens, 0)
		testassert.Equal(t, counters.releases.Load(), int64(1)) // skipped Open still releases the adopted reservation
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
		}
	})

	t.Run("failed Open releases the adopted reservation once", func(t *testing.T) {
		counters := &reservationCounters{}
		openErr := errors.New("open broke")
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				return Execution{}, openErr
			},
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, freshSessionStore(t), prepared)
		defer cancel()
		c, adopted := admitWithoutInstall(t, h, testSessionID, "op-1", admissionContent("hello"))
		{
			err := executeDirect(h, c, h.ctx, "op-1", adopted)
			testassert.Equal(t, err, openErr)
		}
		testassert.Equal(t, counters.releases.Load(), int64(1))
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.NoError(t, err)
		}
	})

	t.Run("adoption releases exactly once, after the execution Close", func(t *testing.T) {
		events := make(chan string, 8)
		counters := &reservationCounters{}
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				return Execution{
					OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
						return invocationExecution(func(context.Context, model.Request) (model.Stream, error) {
							return completedTurnStream(), nil
						}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }), nil
					},
					Close: func() error {
						events <- "execution-close"
						return nil
					},
				}, nil
			},
			Release: func() error {
				events <- "release"
				return counters.release()
			},
		}
		h, cancel := releasingHarness(t, freshSessionStore(t), prepared)
		defer cancel()
		{
			_, err := submitText(t, h, testSessionID, "op-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		for _, want := range []string{"execution-close", "release"} { // the reservation releases after the Close it follows
			select {
			case got := <-events:
				testassert.Equal(t, got, want)
			case <-time.After(30 * time.Second):
				t.Fatalf("timed out waiting for lifetime event %q", want)
			}
		}
		testassert.Equal(t, counters.releases.Load(), int64(1))
		{
			rec, err := h.ReadOperation(context.Background(), testSessionID, "op-1")
			testassert.NoError(t, err)
			testassert.Equal(t, rec.State.Status, OperationSuccess)
		}
	})
}

func TestReservationReleaseFailureIsRetained(t *testing.T) {
	broke := errors.New("reservation release broke")
	t.Run("a discard release failure is retained through Wait", func(t *testing.T) {
		counters := &reservationCounters{err: broke}
		prepared := PreparedExecution{
			Capture: ExecutionCapture{}, // invalid: the admission discards the prepared value
			Open:    func(context.Context, OperationAdmission) (Execution, error) { return validExecution(), nil },
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, freshSessionStore(t), prepared)
		defer cancel()
		_, err := submitText(t, h, testSessionID, "op-1", MessageModeRegular, "hello")
		testassert.Require(t, errors.Is(err, ErrInvalid), "Submit = %v, want the invalid-capture rejection", err)
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.Require(t, err != nil && errors.Is(err, broke), "Wait = %v, want the retained release failure", err)
		}
	})

	t.Run("an adopted release failure is retained without rewriting the terminal", func(t *testing.T) {
		counters := &reservationCounters{err: broke}
		store := freshSessionStore(t)
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				return effectExecution(func(context.Context, model.Request) (model.Stream, error) {
					return completedTurnStream(), nil
				}, nil), nil
			},
			Release: counters.release,
		}
		h, cancel := releasingHarness(t, store, prepared)
		defer cancel()
		{
			_, err := submitText(t, h, testSessionID, "op-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		{
			rec := awaitOperationTerminal(t, store, testSessionID, "op-1", OperationSuccess)
			testassert.Require(t, rec.State.Status == OperationSuccess, "settled operation = %+v, want the committed success kept", rec.State)
		}
		cancel()
		{
			err := h.Wait(context.Background())
			testassert.Require(t, err != nil && errors.Is(err, broke), "Wait = %v, want the retained release failure over the committed terminal", err)
		}
		testassert.Equal(t, counters.releases.Load(), int64(1))
	})
}

// TestOpenedConversationInvocationOwnershipRows proves the conversation-side
// invocation row through the opened execution: a conversation invocation is
// opened exactly once per execution with the conversation purpose.
func TestOpenedConversationInvocationOwnershipRows(t *testing.T) {
	t.Run("conversation invocation opens exactly once per execution", func(t *testing.T) {
		var opens atomic.Int64
		store := freshSessionStore(t)
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				return Execution{OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
					opens.Add(1)
					testassert.Require(t, request.Purpose == InvocationPurposeConversation, "conversation request purpose = %q, want conversation", request.Purpose)
					return invocationExecution(func(context.Context, model.Request) (model.Stream, error) {
						return completedTurnStream(), nil
					}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }), nil
				}}, nil
			},
		}
		h, cancel := releasingHarness(t, store, prepared)
		defer cancel()
		{
			_, err := submitText(t, h, testSessionID, "op-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		{
			_, err := h.ReadOperation(context.Background(), testSessionID, "op-1") // the run retires through its completion owner
			testassert.NoError(t, err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for opens.Load() != 1 {
			testassert.Require(t, time.Now().Before(deadline), "conversation invocation opens = %d, want exactly one", opens.Load())
			time.Sleep(time.Millisecond)
		}
		{
			rec := awaitOperationTerminal(t, store, testSessionID, "op-1", OperationSuccess)
			testassert.Require(t, rec.State.Status == OperationSuccess, "operation = %+v, want success", rec.State)
		}
		testassert.Equal(t, opens.Load(), int64(1))
	})
}
