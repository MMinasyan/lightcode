package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// TestInterruptBlockedModelAttemptSettlesContractInterruption proves the
// blocked-model row: interrupting a running execution whose physical model
// attempt is parked cancels the attempt and settles the durable Operation as
// terminal interruption with the contract detail.
func TestInterruptBlockedModelAttemptSettlesContractInterruption(t *testing.T) {
	store := emptyStore(t)
	started := make(chan struct{})
	modelFn := func(ctx context.Context, req model.Request) (model.Stream, error) {
		started <- struct{}{}
		<-ctx.Done() // the physical attempt parks until the interrupt cancels the execution context
		return nil, ctx.Err()
	}
	prepared := modelPrepared(modelFn)
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)

	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	<-started // parked inside the blocked model attempt
	if err := h.Interrupt(context.Background(), session); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	watch.next()
	rec := settledOperation(t, store, session, "op-1")
	if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
		t.Fatalf("operation state = %+v, want terminal interruption with the contract detail", rec.State)
	}
	requireSessionCleared(t, h, session)
	if _, err := validateFixture(t, store, session); err != nil {
		t.Fatalf("graph after the interrupt: %v", err)
	}
}

// TestInterruptBlockedForegroundToolSettlesRealResult proves the blocked-tool
// row: interrupting a channel-blocked foreground tool cancels it, commits the
// tool's own interrupted result, stops the batch so the later unstarted call
// never begins, and settles the Operation as terminal interruption.
func TestInterruptBlockedForegroundToolSettlesRealResult(t *testing.T) {
	store := emptyStore(t)
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(testToolCall("call-1"), testToolCall("call-2")), nil
	}
	var (
		mu       sync.Mutex
		call2Ran bool
		started  = make(chan struct{})
	)
	toolFn := func(ctx context.Context, call model.ToolCall) PreparedTool {
		if call.ID == "call-1" {
			return PreparedTool{Permissions: fixturePermission, Execute: func(ctx context.Context) ToolOutcome {
				started <- struct{}{}
				<-ctx.Done() // the foreground tool parks until the interrupt cancels its context
				return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultInterrupted, Content: "tool canceled"}}
			}}
		}
		return PreparedTool{Permissions: fixturePermission, Execute: func(ctx context.Context) ToolOutcome {
			mu.Lock()
			call2Ran = true
			mu.Unlock()
			return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "must not run"}}
		}}
	}
	prepared := preparedExecuting(effectExecution(modelFn, toolFn))
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)

	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	<-started // the foreground tool is parked in its executor
	if err := h.Interrupt(context.Background(), session); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	watch.next()
	rec := settledOperation(t, store, session, "op-1")
	if rec.State.Status != OperationInterruption {
		t.Fatalf("operation status = %s, want interruption", rec.State.Status)
	}
	mu.Lock()
	ran := call2Ran
	mu.Unlock()
	if ran {
		t.Fatalf("the later unstarted call began after the interrupt")
	}
	graph, err := validateFixture(t, store, session)
	if err != nil {
		t.Fatalf("graph after the interrupt: %v", err)
	}
	var call1 *toolResultEntry
	for i := range graph.Entries {
		if tr := graph.Entries[i].ToolResult; tr != nil && tr.ToolCallID == "call-1" {
			call1 = tr
		}
	}
	if call1 == nil || call1.Status != model.ResultInterrupted || call1.Content != "tool canceled" {
		t.Fatalf("call-1 result = %+v, want the tool's real interrupted result", call1)
	}
	requireSessionCleared(t, h, session)
}

// TestInterruptSettlesUnstartedExecutionAtEntry proves the marker row: an
// interrupt whose target execution is not yet installed — the publish-to-
// install window, or the drain window with a retiring predecessor whose
// cancel is the harmless no-op — settles the durable Operation at execute's
// entry, and a marker for another operation is inert.
func TestInterruptSettlesUnstartedExecutionAtEntry(t *testing.T) {
	opener := func(runs *int) func(context.Context, OperationAdmission) (Execution, error) {
		return func(context.Context, OperationAdmission) (Execution, error) {
			*runs++
			return effectExecution(func(context.Context, model.Request) (model.Stream, error) {
				return completedTurnStream(), nil
			}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }), nil
		}
	}

	t.Run("publish-to-install window", func(t *testing.T) {
		opens := 0
		h, store, c, sessionID, prepared, cancel := newOpenerHarness(t, opener(&opens))
		defer cancel()
		if err := h.Interrupt(context.Background(), sessionID); err != nil { // the durable Operation runs; no execution is installed yet
			t.Fatalf("Interrupt: %v", err)
		}
		if err := h.execute(c, testOpID, prepared, h.ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("execute = %v, want the entry interruption", err)
		}
		if opens != 0 {
			t.Fatalf("opener calls = %d, want the interrupted execution to settle at entry", opens)
		}
		c.mu.Lock()
		consumed := c.interruptOp == ""
		c.mu.Unlock()
		if !consumed {
			t.Fatalf("the interrupt marker survived its consumption")
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
			t.Fatalf("operation state = %+v, want terminal interruption with the contract detail", rec.State)
		}
		requireSessionCleared(t, h, sessionID)
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph after the entry settlement: %v", err)
		}
	})

	t.Run("drain window with a retiring predecessor", func(t *testing.T) {
		opens := 0
		h, store, c, sessionID, prepared, cancel := newOpenerHarness(t, opener(&opens))
		defer cancel()
		prevCtx, prevCancel := context.WithCancel(h.ctx)
		prevCancel() // the predecessor's execution has returned; its context is already dead
		retired := &activeExecution{done: make(chan struct{}), execCtx: prevCtx, cancel: prevCancel}
		close(retired.done)
		c.mu.Lock()
		c.run = retired
		c.mu.Unlock()
		if err := h.Interrupt(context.Background(), sessionID); err != nil { // canceling the retiring predecessor is the harmless no-op
			t.Fatalf("Interrupt: %v", err)
		}
		select {
		case <-retired.done:
		default:
			t.Fatalf("the retiring predecessor's done channel was reopened")
		}
		if err := h.execute(c, testOpID, prepared, h.ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("execute = %v, want the entry interruption", err)
		}
		if opens != 0 {
			t.Fatalf("opener calls = %d, want the interrupted execution to settle at entry", opens)
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
			t.Fatalf("operation state = %+v, want terminal interruption with the contract detail", rec.State)
		}
		requireSessionCleared(t, h, sessionID)
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph after the entry settlement: %v", err)
		}
	})

	t.Run("a marker for another operation is inert", func(t *testing.T) {
		opens := 0
		h, store, c, sessionID, prepared, cancel := newOpenerHarness(t, opener(&opens))
		defer cancel()
		c.mu.Lock()
		c.interruptOp = "op-other"
		c.mu.Unlock()
		if err := h.execute(c, testOpID, prepared, h.ctx); err != nil {
			t.Fatalf("execute: %v", err)
		}
		if opens != 1 {
			t.Fatalf("opener calls = %d, want the execution to run past a foreign marker", opens)
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.State.Status != OperationSuccess {
			t.Fatalf("operation status = %s, want success past a foreign marker", rec.State.Status)
		}
		c.mu.Lock()
		marker := c.interruptOp
		c.mu.Unlock()
		if marker != "op-other" {
			t.Fatalf("foreign marker = %q, want it retained", marker)
		}
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph after the run: %v", err)
		}
	})
}

// TestInterruptSteeringCommitsAndQueuedAdmits proves the buffer row: steering
// already popped and committed at the model boundary stays committed through
// the interrupted Operation's settlement, the steering submitted after that
// boundary stays unpopped for the post-terminal drain, the interrupted
// Operation settles with the contract detail, and the queued input submitted
// before the interrupt is admitted afterward.
func TestInterruptSteeringCommitsAndQueuedAdmits(t *testing.T) {
	store := emptyStore(t)
	var (
		mu      sync.Mutex
		calls   int
		gate1   = make(chan struct{}) // parks boundary 1 until the buffered inputs are submitted
		park2   = make(chan struct{}) // parks boundary 2 until the interrupt lands
		started = make(chan struct{})
		second  = make(chan struct{})
	)
	modelFn := func(ctx context.Context, req model.Request) (model.Stream, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch n {
		case 1:
			started <- struct{}{}
			<-gate1
			return completedTurnStream(), nil
		case 2: // the boundary drain has already popped and committed s1 on the Harness context
			second <- struct{}{}
			select {
			case <-park2:
				return completedTurnStream(), nil
			case <-ctx.Done(): // the interrupt cancels the execution context while the boundary is parked
				return nil, ctx.Err()
			}
		default:
			return completedTurnStream(), nil
		}
	}
	prepared := modelPrepared(modelFn)
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)

	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	<-started // parked at the first model boundary
	if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1"); err != nil {
		t.Fatalf("steering submit 1: %v", err)
	}
	if _, err := submitText(t, h, session, "op-4", MessageModeQueued, "q1"); err != nil {
		t.Fatalf("queued submit: %v", err)
	}

	close(gate1) // the first turn commits; the boundary drain pops and commits s1, then boundary 2 parks
	<-second
	if _, err := submitText(t, h, session, "op-3", MessageModeRegular, "s2"); err != nil {
		t.Fatalf("steering submit 2: %v", err)
	}
	if err := h.Interrupt(context.Background(), session); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	watch.next() // op-1 settles as the interrupted Operation
	if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationInterruption ||
		rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
		t.Fatalf("op-1 state = %+v, want terminal interruption with the contract detail", rec.State)
	}
	watch.next() // the unpopped steering drains through ordinary admission and succeeds
	if rec := settledOperation(t, store, session, "op-3"); rec.State.Status != OperationSuccess {
		t.Fatalf("op-3 status = %s, want the preserved steering admitted and successful", rec.State.Status)
	}
	watch.next() // the queued input drains next
	if rec := settledOperation(t, store, session, "op-4"); rec.State.Status != OperationSuccess {
		t.Fatalf("op-4 status = %s, want the queued input admitted and successful", rec.State.Status)
	}
	if got := strings.Join(entryTexts(t, store, session), ","); got != "hello,s1,s2,q1" {
		t.Fatalf("committed inputs = %q, want the popped steering committed and the buffers drained", got)
	}
	requireSessionCleared(t, h, session)
	if _, err := validateFixture(t, store, session); err != nil {
		t.Fatalf("graph after the drains: %v", err)
	}
}

// TestInterruptLeavesBackgroundGroupOpen proves the group row: an interrupt
// leaves the parent's background group open — its live member untouched — and
// the group still admits afterwards.
func TestInterruptLeavesBackgroundGroupOpen(t *testing.T) {
	store := emptyStore(t)
	started := make(chan struct{})
	modelFn := func(ctx context.Context, req model.Request) (model.Stream, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	prepared := modelPrepared(modelFn)
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	h.deps.Jobs = stubJobStopper{}
	session := createSession(t, h)
	watch := watchSettlements(store)

	var first []string
	if err := h.StartJob(context.Background(), session, jobFixtureID, spawnRecord(&first, nil)); err != nil {
		t.Fatalf("first StartJob: %v", err)
	}
	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	<-started
	if err := h.Interrupt(context.Background(), session); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	watch.next()
	if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationInterruption {
		t.Fatalf("op-1 status = %s, want interruption", rec.State.Status)
	}

	var second []string
	if err := h.StartJob(context.Background(), session, "feedface", spawnRecord(&second, nil)); err != nil {
		t.Fatalf("StartJob after the interrupt = %v, want the group still admitting", err)
	}
	c, err := h.coordinatorFor(context.Background(), session)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	c.mu.Lock()
	members := len(c.group.members)
	_, firstLive := c.group.members[first[0]]
	_, secondLive := c.group.members[second[0]]
	c.mu.Unlock()
	if members != 2 || !firstLive || !secondLive {
		t.Fatalf("group members = %d (first %v, second %v), want both members live after the interrupt", members, firstLive, secondLive)
	}
}

// TestInterruptIdleAndUnknown proves the idle row: an idle Session returns
// nil, and an unknown Session's resolution error propagates.
func TestInterruptIdleAndUnknown(t *testing.T) {
	h := newTestHarness(t, emptyStore(t), nil)
	session := createSession(t, h)
	if err := h.Interrupt(context.Background(), session); err != nil {
		t.Fatalf("Interrupt on an idle Session = %v, want nil", err)
	}
	if err := h.Interrupt(context.Background(), "ffffffffffffffffffffffffffffffff"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Interrupt on an unknown Session = %v, want ErrNotFound", err)
	}
}
