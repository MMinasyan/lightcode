package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
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

	{
		_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-started // parked inside the blocked model attempt
	{
		err := h.Interrupt(context.Background(), session)
		testassert.NoError(t, err)
	}
	watch.next()
	rec := settledOperation(t, store, session, "op-1")
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "operation state = %+v, want terminal interruption with the contract detail", rec.State)
	requireSessionCleared(t, h, session)
	{
		_, err := validateFixture(t, store, session)
		testassert.NoError(t, err)
	}
}

// TestInterruptBlockedForegroundToolSettlesRealResult proves the blocked-tool
// row: interrupting a channel-blocked foreground tool cancels it, commits the
// tool's own interrupted result, stops the batch so the later unstarted call
// never begins, and settles the Operation as terminal interruption; the
// steering submitted while the tool was blocked later enters its own admission
// and its own model request.
func TestInterruptBlockedForegroundToolSettlesRealResult(t *testing.T) {
	store := emptyStore(t)
	var (
		mu       sync.Mutex
		call2Ran bool
		calls    int
		requests []model.Request
		started  = make(chan struct{})
	)
	modelFn := func(ctx context.Context, req model.Request) (model.Stream, error) {
		mu.Lock()
		calls++
		n := calls
		requests = append(requests, req)
		mu.Unlock()
		if n == 1 { // the blocked Operation's tool-call turn
			return completedTurnStream(testToolCall("call-1"), testToolCall("call-2")), nil
		}
		return completedTurnStream(), nil // the steering admission's own turn
	}
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

	{
		_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-started // the foreground tool is parked in its executor
	{
		_, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer")
		testassert.NoError(t, err)
	}
	{
		err := h.Interrupt(context.Background(), session)
		testassert.NoError(t, err)
	}
	watch.next()
	rec := settledOperation(t, store, session, "op-1")
	testassert.Equal(t, rec.State.Status, OperationInterruption)
	mu.Lock()
	ran := call2Ran
	mu.Unlock()
	testassert.Require(t, !ran, "the later unstarted call began after the interrupt")
	graph, err := validateFixture(t, store, session)
	testassert.NoError(t, err)
	var call1 *toolResultEntry
	for i := range graph.Entries {
		if tr := graph.Entries[i].ToolResult; tr != nil && tr.ToolCallID == "call-1" {
			call1 = tr
		}
	}
	testassert.Require(t, call1 != nil && call1.Status == model.ResultInterrupted && call1.Content == "tool canceled", "call-1 result = %+v, want the tool's real interrupted result", call1)

	// The steering buffered while the tool was blocked drains after the
	// interrupted Operation's terminal into its own admission, whose own
	// model request carries it: exactly two model effects in total.
	watch.next()
	{
		rec := settledOperation(t, store, session, "op-2")
		testassert.Equal(t, rec.State.Status, OperationSuccess)
	}
	mu.Lock()
	calls, reqs := calls, append([]model.Request(nil), requests...)
	mu.Unlock()
	testassert.Equal(t, calls, 2)
	{
		got := strings.Join(textsOf(reqs[1]), "|")
		testassert.Require(t, strings.Contains(got, "steer"), "the steering admission's request = %q, want the steering text at its own model boundary", got)
	}
	{
		got := strings.Join(entryTexts(t, store, session), ",")
		testassert.Equal(t, got, "hello,steer")
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
		// the durable Operation runs; no execution is installed yet
		{
			err := h.Interrupt(context.Background(), sessionID)
			testassert.NoError(t, err)
		}
		{
			err := executeDirect(h, c, h.ctx, testOpID, prepared)
			testassert.Require(t, errors.Is(err, context.Canceled), "execute = %v, want the entry interruption", err)
		}
		testassert.Equal(t, opens, 0)
		c.mu.Lock()
		consumed := c.interruptOp == ""
		c.mu.Unlock()
		testassert.Require(t, consumed, "the interrupt marker survived its consumption")
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "operation state = %+v, want terminal interruption with the contract detail", rec.State)
		requireSessionCleared(t, h, sessionID)
		{
			_, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
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
		// canceling the retiring predecessor is the harmless no-op
		{
			err := h.Interrupt(context.Background(), sessionID)
			testassert.NoError(t, err)
		}
		select {
		case <-retired.done:
		default:
			t.Fatalf("the retiring predecessor's done channel was reopened")
		}
		{
			err := executeDirect(h, c, h.ctx, testOpID, prepared)
			testassert.Require(t, errors.Is(err, context.Canceled), "execute = %v, want the entry interruption", err)
		}
		testassert.Equal(t, opens, 0)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "operation state = %+v, want terminal interruption with the contract detail", rec.State)
		requireSessionCleared(t, h, sessionID)
		{
			_, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("a marker for another operation is inert", func(t *testing.T) {
		opens := 0
		h, store, c, sessionID, prepared, cancel := newOpenerHarness(t, opener(&opens))
		defer cancel()
		c.mu.Lock()
		c.interruptOp = "op-other"
		c.mu.Unlock()
		{
			err := executeDirect(h, c, h.ctx, testOpID, prepared)
			testassert.NoError(t, err)
		}
		testassert.Equal(t, opens, 1)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Equal(t, rec.State.Status, OperationSuccess)
		c.mu.Lock()
		marker := c.interruptOp
		c.mu.Unlock()
		testassert.Equal(t, marker, "op-other")
		{
			_, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
		}
	})
}

// TestInterruptSteeringPreservesBufferOrder proves the buffer row: steering
// waiting when the interrupt lands stays buffered and unpopped — nothing is
// committed in place — the interrupted Operation settles with the contract
// detail, and the post-terminal drain admits the buffered heads in FIFO
// order, steering before queued input.
func TestInterruptSteeringPreservesBufferOrder(t *testing.T) {
	store := emptyStore(t)
	var (
		mu          sync.Mutex
		toolCalls   int
		modelCalls  int
		toolArrived = make(chan struct{})
		toolGate    = make(chan struct{})
	)
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		mu.Lock()
		modelCalls++
		firstModel := modelCalls == 1
		mu.Unlock()
		if firstModel {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		return completedTurnStream(), nil
	}
	toolFn := func(ctx context.Context, call model.ToolCall) PreparedTool {
		mu.Lock()
		toolCalls++
		first := toolCalls == 1
		mu.Unlock()
		if !first {
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ok"}}}
		}
		toolArrived <- struct{}{} // the first batch parks mid-execution: the pre-boundary window
		select {
		case <-toolGate:
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ok"}}}
		case <-ctx.Done(): // the interrupt cancels the execution context inside the batch
			return PreparedTool{Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultInterrupted, Content: interruptedToolResultContent}}}
		}
	}
	prepared := preparedExecuting(effectExecution(modelFn, toolFn))
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)

	{
		_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	receiveBounded(t, toolArrived, "the first tool batch") // op-1 parks inside its tool batch, before any conversation boundary
	{
		_, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1")
		testassert.NoError(t, err)
	}
	{
		_, err := submitText(t, h, session, "op-4", MessageModeQueued, "q1")
		testassert.NoError(t, err)
	}
	{
		err := h.Interrupt(context.Background(), session)
		testassert.NoError(t, err)
	}
	close(toolGate)

	receiveBounded(t, watch.seen, "op-1's interrupted terminal") // op-1 settles as the interrupted Operation
	{
		rec := settledOperation(t, store, session, "op-1")
		testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "op-1 state = %+v, want terminal interruption with the contract detail", rec.State)
	}
	receiveBounded(t, watch.seen, "the steering head's settlement") // the buffered steering drains through ordinary admission
	if rec := settledOperation(t, store, session, "op-2"); rec.State.Status != OperationSuccess {
		t.Fatalf("op-2 status = %s (%s), want the preserved steering admitted and successful", rec.State.Status, rec.State.Terminal.Detail)
	}
	receiveBounded(t, watch.seen, "the queued head's settlement") // the queued input drains next
	{
		rec := settledOperation(t, store, session, "op-4")
		testassert.Equal(t, rec.State.Status, OperationSuccess)
	}
	awaitHarnessQuiet(t, h, session) // raw graph readers await Session convergence after the successors' terminals
	{
		got := strings.Join(entryTexts(t, store, session), ",")
		testassert.Equal(t, got, "hello,s1,q1")
	}
	{
		owner, _ := steeringInputOwner(t, store, session, "s1")
		testassert.Equal(t, owner, "op-2")
	}
	requireSessionCleared(t, h, session)
	{
		_, err := validateFixture(t, store, session)
		testassert.NoError(t, err)
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
	{
		err := h.StartJob(context.Background(), session, jobFixtureID, spawnRecord(&first, nil))
		testassert.NoError(t, err)
	}
	{
		_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-started
	{
		err := h.Interrupt(context.Background(), session)
		testassert.NoError(t, err)
	}
	watch.next()
	{
		rec := settledOperation(t, store, session, "op-1")
		testassert.Equal(t, rec.State.Status, OperationInterruption)
	}

	var second []string
	{
		err := h.StartJob(context.Background(), session, "feedface", spawnRecord(&second, nil))
		testassert.NoError(t, err)
	}
	c := leaseCoordinator(t, h, session)
	c.mu.Lock()
	members := len(c.group.members)
	_, firstLive := c.group.members[first[0]]
	_, secondLive := c.group.members[second[0]]
	c.mu.Unlock()
	testassert.Require(t, members == 2 && firstLive && secondLive, "group members = %d (first %v, second %v), want both members live after the interrupt", members, firstLive, secondLive)
}

// TestInterruptIdleAndUnknown proves the idle row: an idle Session returns
// nil, and an unknown Session's resolution error propagates.
func TestInterruptIdleAndUnknown(t *testing.T) {
	h := newTestHarness(t, emptyStore(t), nil)
	session := createSession(t, h)
	{
		err := h.Interrupt(context.Background(), session)
		testassert.NoError(t, err)
	}
	{
		err := h.Interrupt(context.Background(), "ffffffffffffffffffffffffffffffff")
		testassert.Require(t, errors.Is(err, ErrNotFound), "Interrupt on an unknown Session = %v, want ErrNotFound", err)
	}
}
