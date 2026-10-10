package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// steeringInputOwner returns the owning Operation and origin of one committed
// input entry carrying the given text, read from the validated graph.
func steeringInputOwner(t *testing.T, store Storage, sessionID, text string) (string, InputOrigin) {
	t.Helper()
	graph, err := validateSessionGraph(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("session graph: %v", err)
	}
	for _, entry := range graph.Entries {
		if entry.Input != nil && len(entry.Input.Content) > 0 && entry.Input.Content[0].Text == text {
			return entry.Envelope.OperationID, entry.Input.Origin
		}
	}
	t.Fatalf("session %q carries no committed input %q", sessionID, text)
	return "", ""
}

// awaitRunRetired polls one coordinator until its run slot and reservation are
// both clear, the deterministic post-drain barrier for sessions whose
// Operation stays durably running.
func awaitRunRetired(t *testing.T, h *Harness, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	c := cachedCoordinator(t, h, sessionID)
	for {
		c.mu.Lock()
		busy := c.run != nil || c.reserved != nil
		c.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s never retired its run and reservation", sessionID)
		}
		time.Sleep(time.Millisecond)
	}
}

// nextSettlementBounded waits for one committed operation-settlement entry,
// failing the test instead of hanging when the expected settlement never
// commits under a broken contract.
func nextSettlementBounded(t *testing.T, w *settleWatch, what string) {
	t.Helper()
	select {
	case <-w.seen:
	case <-time.After(10 * time.Second):
		t.Fatalf("settlement never committed: %s", what)
	}
}

// immediateToolPlan is the fixtures' successful tool plan: allowed declaration
// plus one immediate success result.
func immediateToolPlan(_ context.Context, call model.ToolCall) PreparedTool {
	return PreparedTool{
		Permissions: fixturePermission,
		Immediate:   &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ok"}},
	}
}

// deniedToolPlan is a plan the permission boundary denies: a file target
// without a canonical workspace root never passes the fixed evaluator.
func deniedToolPlan(_ context.Context, call model.ToolCall) PreparedTool {
	return PreparedTool{
		Permissions: []PermissionRequest{{Permission: permissionFileRead, Target: "/data/secret"}},
		Immediate:   &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "never"}},
	}
}

// interruptedToolPlan settles its call with the ordinary interrupted result.
func interruptedToolPlan(_ context.Context, call model.ToolCall) PreparedTool {
	return PreparedTool{
		Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultInterrupted, Content: interruptedToolResultContent}},
	}
}

// successToolExecution builds the fixture execution whose model function and
// tool plans both settle normally.
func successToolExecution(modelFn func(context.Context, model.Request) (model.Stream, error), toolFn func(context.Context, model.ToolCall) PreparedTool) PreparedExecution {
	return preparedExecuting(Execution{
		Model:         modelFn,
		CompactModel:  modelFn,
		Tool:          toolFn,
		NormalizeTool: objectNormalize,
	})
}

// TestSteeringFirstRequestAndMixedFIFO proves the delivered-message
// admission contract through public
// operations: every delivered head owns its caller-ID Operation with exactly
// one first receiving request, steering FIFO precedes queued, the handoff
// happens only after a complete tool batch — including input that arrives
// after the last tool callback — while an interrupted tool result without
// context cancellation keeps the real interruption, fixed cap exhaustion stays
// a real failure, tool call IDs recur across successor Operations, and
// same-Operation duplication fails.
func TestSteeringFirstRequestAndMixedFIFO(t *testing.T) {
	t.Run("mixed heads in FIFO order with one first request per head", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		prepared := modelPrepared(script.model)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived // parked at the first model boundary
		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1"); err != nil {
			t.Fatalf("steering submit 1: %v", err)
		}
		if _, err := submitText(t, h, session, "op-3", MessageModeRegular, "s2"); err != nil {
			t.Fatalf("steering submit 2: %v", err)
		}
		if _, err := submitText(t, h, session, "op-4", MessageModeQueued, "q1"); err != nil {
			t.Fatalf("queued submit: %v", err)
		}
		script.releaseGate()

		for _, op := range []string{"op-1", "op-2", "op-3", "op-4"} { // four split Operations settle in FIFO order
			nextSettlementBounded(t, watch, "the expected terminal settlement")
			if rec := settledOperation(t, store, session, op); rec.State.Status != OperationSuccess {
				t.Fatalf("operation %q settled %q, want success", op, rec.State.Status)
			}
		}
		reqs := script.seen()
		if len(reqs) != 4 {
			t.Fatalf("model requests = %d, want one per delivered head", len(reqs))
		}
		want := []string{
			"system|hello",
			"system|hello|done|s1",
			"system|hello|done|s1|done|s2",
			"system|hello|done|s1|done|s2|done|q1",
		}
		for i, wantTexts := range want {
			if got := strings.Join(textsOf(reqs[i]), "|"); got != wantTexts {
				t.Fatalf("request %d projection = %q, want %q", i, got, wantTexts)
			}
		}
		if got := strings.Join(entryTexts(t, store, session), ","); got != "hello,s1,s2,q1" {
			t.Fatalf("committed inputs = %q, want the heads admitted in FIFO order", got)
		}
		if owner, _ := steeringInputOwner(t, store, session, "s1"); owner != "op-2" {
			t.Fatalf("steering input s1 owned by %q, want its own Operation op-2", owner)
		}
		if owner, _ := steeringInputOwner(t, store, session, "q1"); owner != "op-4" {
			t.Fatalf("queued input q1 owned by %q, want its own Operation op-4", owner)
		}
	})

	t.Run("first request protection for steering buffered before the run starts", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		modelGate := make(chan struct{})
		script.gate = modelGate
		openerGate := make(chan struct{})
		prepared := PreparedExecution{
			Capture: testCapture(),
			Open: func(context.Context, OperationAdmission) (Execution, error) {
				<-openerGate // parked between the admission commit and the first context call
				exec := validExecution()
				exec.Model = script.model
				return exec, nil
			},
		}
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		// op-1 is durably admitted and its opener is parked: the steering
		// lands before the run's first context call
		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		close(openerGate)
		receiveBounded(t, script.arrived, "op-1's first model request")
		close(modelGate)

		nextSettlementBounded(t, watch, "op-1 settles success at the boundary after its first request")
		nextSettlementBounded(t, watch, "the drain admits s1 as its own Operation")
		if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationSuccess {
			t.Fatalf("op-1 settled %q, want success", rec.State.Status)
		}
		if rec := settledOperation(t, store, session, "op-2"); rec.State.Status != OperationSuccess {
			t.Fatalf("op-2 settled %q, want success", rec.State.Status)
		}
		reqs := script.seen()
		if len(reqs) != 2 {
			t.Fatalf("model requests = %d, want two", len(reqs))
		}
		if got := strings.Join(textsOf(reqs[0]), "|"); got != "system|hello" {
			t.Fatalf("first request = %q, want the admitted input only: waiting steering never preempts the first receiving request", got)
		}
		if got := strings.Join(textsOf(reqs[1]), "|"); got != "system|hello|done|s1" {
			t.Fatalf("successor request = %q, want the steering as the successor's first receiving request", got)
		}
	})

	t.Run("all origins deliver their own admitted input", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		prepared := modelPrepared(script.model)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived
		for _, head := range []struct {
			id     string
			origin InputOrigin
			text   string
		}{
			{"op-u", InputOriginUser, "u1"},
			{"op-r", InputOriginRuntime, "r1"},
			{"op-p", InputOriginPlugin, "p1"},
		} {
			if _, err := h.Submit(context.Background(), SubmitRequest{
				SessionID: session, OperationID: head.id, Origin: head.origin,
				Content: admissionContent(head.text), Mode: MessageModeRegular,
			}); err != nil {
				t.Fatalf("steering submit %s: %v", head.id, err)
			}
		}
		script.releaseGate()

		for _, op := range []string{"op-1", "op-u", "op-r", "op-p"} {
			nextSettlementBounded(t, watch, "the expected terminal settlement")
			if rec := settledOperation(t, store, session, op); rec.State.Status != OperationSuccess {
				t.Fatalf("operation %q settled %q, want success", op, rec.State.Status)
			}
		}
		for _, head := range []struct {
			id     string
			origin InputOrigin
			text   string
		}{
			{"op-u", InputOriginUser, "u1"},
			{"op-r", InputOriginRuntime, "r1"},
			{"op-p", InputOriginPlugin, "p1"},
		} {
			owner, origin := steeringInputOwner(t, store, session, head.text)
			if owner != head.id || origin != head.origin {
				t.Fatalf("input %q = (%q owner, %q origin), want its own Operation %q with the submitted origin", head.text, owner, origin, head.id)
			}
		}
	})

	t.Run("two-call batch ordering keeps every result before the successor admissions", func(t *testing.T) {
		store := emptyStore(t)
		// the batch-ordering row: a full batch of two calls with steering
		// waiting through it — the after-callback timing oracle is
		// TestSteeringLateSubmitAfterEffectCallbacks; this row owns only the
		// result-ordering proof
		script := newModelScript(turn(testToolCall("call-1"), testToolCall("call-2")))
		var (
			mu             sync.Mutex
			batchSubmitted bool
			late           SubmitResult
			lateErr        error
			h              *Harness
			session        string
		)
		toolFn := func(_ context.Context, call model.ToolCall) PreparedTool {
			mu.Lock()
			first := !batchSubmitted
			batchSubmitted = true
			mu.Unlock()
			if first {
				if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer-batch"); err != nil {
					t.Errorf("mid-batch submit: %v", err)
				}
			}
			return immediateToolPlan(context.Background(), call)
		}
		prepared := successToolExecution(script.model, toolFn)
		created, cancel := newCancelableHarness(t, store, prepared, nil)
		h = created
		defer cancel()
		session = createSession(t, h)
		// The finished-call fact fires after the real tool callback returned
		// and its result committed: the late head lands inside that window,
		// before the subsequent context call.
		var lateOnce sync.Once
		h.deps.Observe = func(fact HarnessFact) {
			if fact.Kind != FactToolFinished || fact.CallID != "call-2" {
				return
			}
			lateOnce.Do(func() {
				late, lateErr = h.Submit(context.Background(), SubmitRequest{
					SessionID: session, OperationID: "op-3", Origin: InputOriginUser,
					Content: admissionContent("steer-late"), Mode: MessageModeRegular,
				})
			})
		}
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		for _, op := range []string{"op-1", "op-2", "op-3"} {
			nextSettlementBounded(t, watch, "the split Operations' settlements")
			if rec := settledOperation(t, store, session, op); rec.State.Status != OperationSuccess {
				t.Fatalf("operation %q settled %q (%q), want success", op, rec.State.Status, rec.State.Terminal.Detail)
			}
		}
		if lateErr != nil || late.Disposition != DispositionSteering || late.Operation != nil {
			t.Fatalf("late submit = %+v err %v, want buffered steering with no invented Operation", late, lateErr)
		}

		reqs := script.seen()
		// Exactly one predecessor request: a decision moved earlier would send
		// a continuation request without the late head.
		if len(reqs) != 3 {
			t.Fatalf("model requests = %d, want one predecessor request and one per delivered head", len(reqs))
		}
		if got := strings.Join(textsOf(reqs[0]), "|"); got != "system|hello" {
			t.Fatalf("predecessor request = %q, want the admitted input only", got)
		}
		if got := strings.Join(textsOf(reqs[1]), "|"); got != "system|hello|done|ok|ok|steer-batch" {
			t.Fatalf("first successor request = %q, want the settled batch before the waiting head", got)
		}
		if got := strings.Join(textsOf(reqs[2]), "|"); got != "system|hello|done|ok|ok|steer-batch|done|steer-late" {
			t.Fatalf("second successor request = %q, want the late head after the settled batch history", got)
		}
		// Every batch result precedes every successor admission.
		graph, err := validateFixture(t, store, session)
		if err != nil {
			t.Fatalf("graph: %v", err)
		}
		lastResult, firstAdmission := -1, len(graph.Entries)
		for i, entry := range graph.Entries {
			switch {
			case entry.ToolResult != nil:
				lastResult = i
			case entry.Input != nil && len(entry.Input.Content) > 0 &&
				(entry.Input.Content[0].Text == "steer-batch" || entry.Input.Content[0].Text == "steer-late"):
				if i < firstAdmission {
					firstAdmission = i
				}
			}
		}
		if lastResult < 0 || firstAdmission >= len(graph.Entries) || lastResult >= firstAdmission {
			t.Fatalf("entry order = results up to %d, admissions from %d, want every result before every successor admission", lastResult, firstAdmission)
		}
		if owner, _ := steeringInputOwner(t, store, session, "steer-batch"); owner != "op-2" {
			t.Fatalf("mid-batch input owned by %q, want op-2", owner)
		}
		if owner, _ := steeringInputOwner(t, store, session, "steer-late"); owner != "op-3" {
			t.Fatalf("late input owned by %q, want op-3: input arriving before the context call is never committed in place", owner)
		}
	})

	outputShapes := []struct {
		name       string
		attempts   []modelAttempt
		toolFn     func(context.Context, model.ToolCall) PreparedTool
		wantOp1    OperationState
		wantSignal bool // the errored partial retains its committed continuation signal
	}{
		{"ready no-call output", []modelAttempt{turn()}, immediateToolPlan, OperationSuccess, false},
		{"full tool batch", []modelAttempt{turn(testToolCall("call-1"))}, immediateToolPlan, OperationSuccess, false},
		{"errored partial output", []modelAttempt{partialTurn()}, immediateToolPlan, OperationSuccess, true},
		{"tool error result", []modelAttempt{turn(testToolCall("call-1"))}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }, OperationSuccess, false},
		{"denied tool result", []modelAttempt{turn(testToolCall("call-1"))}, deniedToolPlan, OperationSuccess, false},
		{"interrupted tool result", []modelAttempt{turn(testToolCall("call-1"))}, interruptedToolPlan, OperationInterruption, false},
	}
	for _, tc := range outputShapes {
		t.Run(tc.name, func(t *testing.T) {
			store := emptyStore(t)
			script := newModelScript(tc.attempts...)
			gate := make(chan struct{})
			script.gate = gate
			prepared := successToolExecution(script.model, tc.toolFn)
			h, cancel := newCancelableHarness(t, store, prepared, nil)
			defer cancel()
			session := createSession(t, h)
			watch := watchSettlements(store)

			if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-script.arrived
			if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer"); err != nil {
				t.Fatalf("steering submit: %v", err)
			}
			script.releaseGate()

			nextSettlementBounded(t, watch, "the expected terminal settlement")
			if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != tc.wantOp1 {
				t.Fatalf("op-1 settled %q, want %q", rec.State.Status, tc.wantOp1)
			}
			nextSettlementBounded(t, watch, "the expected terminal settlement")
			if rec := settledOperation(t, store, session, "op-2"); rec.State.Status != OperationSuccess {
				t.Fatalf("op-2 settled %q, want the steering delivered through ordinary admission", rec.State.Status)
			}
			if owner, _ := steeringInputOwner(t, store, session, "steer"); owner != "op-2" {
				t.Fatalf("steering input owned by %q, want op-2", owner)
			}
			graph, err := validateFixture(t, store, session)
			if err != nil {
				t.Fatalf("graph: %v", err)
			}
			signals := 0
			for _, entry := range graph.Entries {
				if entry.Signal != nil && entry.Signal.Signal == SignalModelFailureContinuation && entry.Envelope.OperationID == "op-1" {
					signals++
				}
			}
			if tc.wantSignal && signals != 1 {
				t.Fatalf("continuation signals = %d, want the errored partial's committed signal retained", signals)
			}
			if !tc.wantSignal && signals != 0 {
				t.Fatalf("continuation signals = %d, want none", signals)
			}
			if tc.wantOp1 == OperationInterruption { // a real interruption signal, never a fabricated one for the handoff
				var interrupted bool
				for _, entry := range graph.Entries {
					if entry.Signal != nil && entry.Signal.Signal == SignalInterruption && entry.Envelope.OperationID == "op-1" {
						interrupted = true
					}
				}
				if !interrupted {
					t.Fatalf("interrupted tool result left no real interruption signal")
				}
			}
		})
	}

	t.Run("fixed cap exhaustion settles failure before any handoff", func(t *testing.T) {
		store := emptyStore(t)
		var (
			mu                 sync.Mutex
			predecessorEffects int
			successorEffects   int
		)
		arrived25 := make(chan struct{})
		gate25 := make(chan struct{})
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			mu.Lock()
			if predecessorEffects < 25 {
				predecessorEffects++
				n := predecessorEffects
				mu.Unlock()
				if n == 25 {
					close(arrived25)
					<-gate25 // the predecessor's final effect is gated before the steering submits
				}
				return erroredTurnStream(), nil // every predecessor effect continues: the fixed cap is its only end
			}
			successorEffects++
			mu.Unlock()
			return completedTurnStream(), nil
		}
		prepared := modelPrepared(modelFn)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		receiveBounded(t, arrived25, "the predecessor's gated effect 25")
		steered, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1") // regular steering waits through the cap boundary
		if err != nil || steered.Disposition != DispositionSteering {
			t.Fatalf("steering submit = %+v err %v, want steering at the gated effect", steered, err)
		}
		close(gate25)

		nextSettlementBounded(t, watch, "the cap failure settlement")
		rec := settledOperation(t, store, session, "op-1")
		if rec.State.Status != OperationFailure || !strings.Contains(rec.State.Terminal.Detail, "25 model effects") {
			t.Fatalf("op-1 = %q %q, want the fixed cap failure settled before any handoff", rec.State.Status, rec.State.Terminal.Detail)
		}
		mu.Lock()
		predecessor := predecessorEffects
		mu.Unlock()
		if predecessor != 25 {
			t.Fatalf("predecessor effects = %d, want exactly the fixed 25 counted independently", predecessor)
		}
		nextSettlementBounded(t, watch, "the steering head's settlement")
		if rec := settledOperation(t, store, session, "op-2"); rec.State.Status != OperationSuccess {
			t.Fatalf("op-2 settled %q, want the steering admitted after the cap failure", rec.State.Status)
		}
		mu.Lock()
		successor := successorEffects
		mu.Unlock()
		if successor != 1 {
			t.Fatalf("successor effects = %d, want its single completed turn", successor)
		}
		if owner, _ := steeringInputOwner(t, store, session, "s1"); owner != "op-2" {
			t.Fatalf("steering input owned by %q, want op-2", owner)
		}
	})

	t.Run("recurring call id across successor operations", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript(turn(testToolCall("call-1")), turn(testToolCall("call-1")))
		gate := make(chan struct{})
		script.gate = gate
		prepared := successToolExecution(script.model, immediateToolPlan)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		receiveBounded(t, script.arrived, "op-1's first model request")
		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		script.releaseGate()

		for _, op := range []string{"op-1", "op-2"} {
			nextSettlementBounded(t, watch, "the expected terminal settlement")
			if rec := settledOperation(t, store, session, op); rec.State.Status != OperationSuccess {
				t.Fatalf("operation %q settled %q (%q), want the recurring call id to succeed across Operations", op, rec.State.Status, rec.State.Terminal.Detail)
			}
		}
	})

	t.Run("same-operation call id duplication fails", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript(turn(testToolCall("call-1")), turn(testToolCall("call-1")))
		prepared := successToolExecution(script.model, immediateToolPlan)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		nextSettlementBounded(t, watch, "the expected terminal settlement")
		rec := settledOperation(t, store, session, "op-1")
		if rec.State.Status != OperationFailure || !strings.Contains(rec.State.Terminal.Detail, "repeats tool call id") {
			t.Fatalf("op-1 = %q %q, want the same-Operation duplication failure", rec.State.Status, rec.State.Terminal.Detail)
		}
	})
}

// TestSteeringLateSubmitAfterEffectCallbacks drives the actual context and
// effect functions over one admitted run: the first context call is consumed,
// the real model and tool effect callbacks finish, no steering exists at any
// point before them, and the Submit lands strictly after those functions
// returned — then the actual next context boundary performs the handoff. It
// asserts the boundary's cancellation-shaped return (the handoff), the
// predecessor's silent success, that no second predecessor request is ever
// projected or sent, and that the head stays buffered for the shared drain.
func TestSteeringLateSubmitAfterEffectCallbacks(t *testing.T) {
	var (
		mu         sync.Mutex
		modelCalls int
	)
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		mu.Lock()
		modelCalls++
		first := modelCalls == 1
		mu.Unlock()
		if first {
			return completedTurnStream(testToolCall("call-1")), nil // the predecessor's one call-bearing request
		}
		return completedTurnStream(), nil // the successor completes plainly
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)

	// the admitted-run fixture: the installed execution the handoff recheck
	// requires, over the real admitted Operation
	execCtx := context.Background()
	run := &activeExecution{done: make(chan struct{}), execCtx: execCtx, cancel: func() {}}
	c.mu.Lock()
	c.run = run
	c.mu.Unlock()

	exec := effectExecution(modelFn, immediateToolPlan)
	source := h.contextSource(c, run, testOpID)
	first, err := source(execCtx) // the first context call is consumed by the admitted input
	if err != nil {
		t.Fatalf("first context call: %v", err)
	}
	if got := len(first); got != 2 {
		t.Fatalf("first projection = %d messages, want the system prompt and the admitted input", got)
	}

	set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, exec, testCapture()), nil) // the real model effect callback
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if set.Disposition != agent.DispoReady || set.Output == nil || len(set.Output.Message.ToolCalls) != 1 {
		t.Fatalf("model settlement = %+v, want the completed call-bearing output", set)
	}
	res, err := h.toolEffect(c, testOpID, exec, testCapture())(execCtx, testToolCall("call-1")) // the real tool effect callback
	if err != nil {
		t.Fatalf("tool effect: %v", err)
	}
	if res.Status != model.ResultSuccess {
		t.Fatalf("tool result = %+v, want success", res)
	}

	// every real effect function has returned and no steering existed before
	// any of them finished
	c.mu.Lock()
	waiting := len(c.steering)
	c.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("steering before the late submit = %d items, want none", waiting)
	}

	// the Submit lands strictly after the model and tool effect functions
	// returned, before the subsequent context call
	steered, err := h.Submit(execCtx, SubmitRequest{
		SessionID: sessionID, OperationID: "op-2", Origin: InputOriginUser,
		Content: admissionContent("steer-late"), Mode: MessageModeRegular,
	})
	if err != nil || steered.Disposition != DispositionSteering || steered.Operation != nil {
		t.Fatalf("late submit = %+v err %v, want buffered steering", steered, err)
	}

	second, err := source(execCtx) // the actual next context boundary
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("next context call = (%v, %v), want the handoff's cancellation-shaped return", second, err)
	}
	if second != nil {
		t.Fatalf("next context call projected %v, want no extra predecessor request", second)
	}
	if rec := settledOperation(t, store, sessionID, testOpID); rec.State.Status != OperationSuccess ||
		rec.State.Terminal == nil || rec.State.Terminal.Detail != "" {
		t.Fatalf("predecessor = %+v, want the handoff's silent success", rec.State)
	}
	mu.Lock()
	got := modelCalls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("predecessor model calls = %d, want exactly the one finished request with no extra predecessor request", got)
	}
	c.mu.Lock()
	buffered := len(c.steering)
	c.mu.Unlock()
	if buffered != 1 {
		t.Fatalf("steering after the handoff = %d items, want the head buffered until the shared drain adopts it", buffered)
	}
	h.drainBuffers(c, run) // the shared drain adopts the buffered head under its own identity
	awaitOperationTerminal(t, store, sessionID, "op-2", OperationSuccess)
	if owner, _ := steeringInputOwner(t, store, sessionID, "steer-late"); owner != "op-2" {
		t.Fatalf("late input owned by %q, want its own successor Operation", owner)
	}
}

// TestSteeringHandoffReservationAndCleanup proves the handoff reservation
// and cleanup contract: the handoff
// settles a silent success with exact ownership and usage, holds the Session
// reservation through the parked settlement until the retiring execution's
// cleanup and the shared drain install a successor — never scoping a successor
// before the predecessor's Close — and neither Fork nor Archive overtakes a
// selected handoff; a cleanup failure stays in Wait without rewriting success,
// and one delivery cycle acquires the reservation exactly once.
func TestSteeringHandoffReservationAndCleanup(t *testing.T) {
	t.Run("silent success with exact ownership and usage", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		prepared := modelPrepared(script.model)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived
		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		script.releaseGate()

		nextSettlementBounded(t, watch, "the expected terminal settlement")
		nextSettlementBounded(t, watch, "the expected terminal settlement")
		rec := settledOperation(t, store, session, "op-1")
		if rec.State.Status != OperationSuccess || rec.State.Terminal == nil || rec.State.Terminal.Detail != "" {
			t.Fatalf("op-1 terminal = %+v, want quiet success with empty detail", rec.State)
		}
		want := UsageTotals{ByModel: []ModelUsage{{Model: testCapture().Model, Usage: UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}}}}
		if len(rec.State.Usage.ByModel) != 1 || rec.State.Usage.ByModel[0] != want.ByModel[0] {
			t.Fatalf("op-1 usage = %+v, want exactly the one model turn's counts with no handoff contribution", rec.State.Usage)
		}
		graph, err := validateFixture(t, store, session)
		if err != nil {
			t.Fatalf("graph: %v", err)
		}
		var (
			op1Inputs      int
			op1Assistants  int
			op1Settlements int
			signals        int
		)
		for _, entry := range graph.Entries {
			switch {
			case entry.Input != nil && entry.Envelope.OperationID == "op-1":
				op1Inputs++
			case entry.Assistant != nil && entry.Envelope.OperationID == "op-1":
				op1Assistants++
			case entry.Settlement != nil && entry.Envelope.OperationID == "op-1":
				op1Settlements++
			case entry.Signal != nil:
				signals++
			}
		}
		if op1Inputs != 1 || op1Assistants != 1 || op1Settlements != 1 {
			t.Fatalf("op-1 owns inputs=%d assistants=%d settlements=%d, want exactly its admitted input, one assistant and one settlement", op1Inputs, op1Assistants, op1Settlements)
		}
		if signals != 0 {
			t.Fatalf("session carries %d signal entries, want the quiet handoff to emit none", signals)
		}
		if owner, _ := steeringInputOwner(t, store, session, "s1"); owner != "op-2" {
			t.Fatalf("steering input owned by %q, want op-2", owner)
		}
	})

	t.Run("parked settlement holds the coordinator and blocks overtaking transitions", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		var (
			mu    sync.Mutex
			order []string
		)
		note := func(step string) {
			mu.Lock()
			order = append(order, step)
			mu.Unlock()
		}
		exec := validExecution()
		exec.Model = script.model
		exec.Close = func() error {
			note("close")
			return nil
		}
		prepared := preparedExecuting(exec)
		stub := newPrepareStub(prepared)
		h, cancel := newCancelableHarness(t, store, PreparedExecution{}, func(ctx context.Context, req PreparationRequest) (PreparedExecution, error) {
			note("prepare")
			return stub.prepare(ctx, req)
		})
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		receiveBounded(t, stub.arrived, "preparation arrival") // op-1's admission preparation
		receiveBounded(t, script.arrived, "op-1's first model request")
		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}

		parked := make(chan struct{})
		release := make(chan struct{})
		prior := store.entryHook // the settlement watcher stays chained through the park
		store.entryHook = func(draft EntryDraft) error {
			if draft.Kind == EntryOperationSettlement && draft.OperationID == "op-1" {
				store.entryHook = prior // park exactly the handoff's own settlement
				close(parked)
				<-release
			}
			if prior != nil {
				return prior(draft)
			}
			return nil
		}
		prepareGate := make(chan struct{})
		stub.gate = prepareGate // the successor's admission preparation parks: the handoff window stays open
		script.releaseGate()
		receiveBounded(t, parked, "the parked settlement") // the handoff's settlement transaction is parked under the coordinator hold

		c := cachedCoordinator(t, h, session)
		if c.mu.TryLock() { // the recheck and the quiet success publication share one coordinator hold
			c.mu.Unlock()
			t.Fatalf("the parked handoff settlement does not hold the coordinator mutex")
		}
		close(release)
		receiveBounded(t, stub.arrived, "the successor's admission preparation") // the drain holds the carried reservation mid-admission

		// The handoff window: the predecessor is durably settled while the
		// selected head is still pending and no successor is scoped yet.
		snap, err := h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("snapshot in the handoff window: %v", err)
		}
		if snap.Session.State.CurrentOperationID != "" {
			t.Fatalf("current operation = %q, want the settled predecessor cleared", snap.Session.State.CurrentOperationID)
		}
		if len(snap.Steering) != 1 || snap.Steering[0].OperationID != "op-2" {
			t.Fatalf("handoff-window steering = %+v, want the selected head pending until adoption", snap.Steering)
		}
		for _, op := range snap.Operations {
			if op.Admission.OperationID == "op-2" {
				t.Fatalf("the successor Operation was scoped before the predecessor finished")
			}
		}
		// Each overtaking transition gets its own live context and provably
		// reaches the reservation wait: a fresh deadline cannot fire early, so
		// a DeadlineExceeded arriving no sooner than that deadline proves the
		// wait, while an already-expired shared context would return at once.
		const waitBound = 100 * time.Millisecond
		archiveCtx, archiveCancel := context.WithTimeout(context.Background(), waitBound)
		defer archiveCancel()
		archiveStart := time.Now()
		if _, err := h.ArchiveSession(archiveCtx, session); !errors.Is(err, context.DeadlineExceeded) || time.Since(archiveStart) < waitBound {
			t.Fatalf("archive during the selected handoff = %v after %v, want it blocked on the held reservation until its own deadline", err, time.Since(archiveStart))
		}
		boundary := settledOperation(t, store, session, "op-1").Admission.AdmittedEntry.EntryID
		forkCtx, forkCancel := context.WithTimeout(context.Background(), waitBound)
		defer forkCancel()
		forkStart := time.Now()
		if _, err := h.Fork(forkCtx, ForkRequest{SourceSessionID: session, BoundaryEntryID: boundary, OperationID: "fork-1", Content: admissionContent("f")}); !errors.Is(err, context.DeadlineExceeded) || time.Since(forkStart) < waitBound {
			t.Fatalf("fork during the selected handoff = %v after %v, want it blocked on the held reservation until its own deadline", err, time.Since(forkStart))
		}

		close(prepareGate)
		for _, op := range []string{"op-1", "op-2"} {
			nextSettlementBounded(t, watch, "the expected terminal settlement")
			if rec := settledOperation(t, store, session, op); rec.State.Status != OperationSuccess {
				t.Fatalf("operation %q settled %q, want success", op, rec.State.Status)
			}
		}
		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		var closeIdx = -1
		for i, step := range got {
			if step == "close" {
				closeIdx = i // the predecessor's cleanup
				break
			}
		}
		prepareIdx := -1
		if closeIdx >= 0 {
			for i := closeIdx + 1; i < len(got); i++ {
				if got[i] == "prepare" { // the successor's admission preparation
					prepareIdx = i
					break
				}
			}
		}
		if closeIdx < 0 || prepareIdx < 0 {
			t.Fatalf("scope order = %v, want the predecessor's Close before the successor's preparation", got)
		}
	})

	t.Run("cleanup failure stays in Wait without rewriting success", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		exec := validExecution()
		exec.Model = script.model
		exec.Close = func() error { return errors.New("cleanup broke") }
		prepared := preparedExecuting(exec)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived
		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		script.releaseGate()

		nextSettlementBounded(t, watch, "the expected terminal settlement")
		nextSettlementBounded(t, watch, "the expected terminal settlement")
		if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationSuccess {
			t.Fatalf("op-1 settled %q, want success", rec.State.Status)
		}
		if rec := settledOperation(t, store, session, "op-2"); rec.State.Status != OperationSuccess {
			t.Fatalf("op-2 settled %q, want success", rec.State.Status)
		}
		cancel()
		if err := h.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "cleanup broke") {
			t.Fatalf("Wait = %v, want the retained cleanup failure", err)
		}
		if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationSuccess {
			t.Fatalf("op-1 settled %q after the cleanup failure, want success preserved", rec.State.Status)
		}
	})

	t.Run("one delivery cycle acquires the reservation exactly once", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		prepared := modelPrepared(script.model)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)
		col := observeHarness(h)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		receiveBounded(t, script.arrived, "op-1's first model request")
		col.reset()
		base := snapshotPair(t, h, session)
		if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		wantPairAt(t, h, session, base.DurableRevision, base.LocalRevision+3) // reserve, enqueue, release

		col.reset()
		base = snapshotPair(t, h, session)
		script.releaseGate()
		nextSettlementBounded(t, watch, "the handoff's quiet success settlement")
		nextSettlementBounded(t, watch, "the successor's terminal")
		awaitHarnessQuiet(t, h, session)
		// The delivery cycle's publications in order: op-1's turn result
		// (durable), the handoff reservation (local), the handoff settlement
		// (durable), the successor admission (durable), the installation
		// (local), the one carried release (local), op-2's turn result and
		// terminal (durable), and op-2's drain reservation, retirement and
		// release (local). A reacquired reservation would add a seventh local
		// publication before the admission.
		wantPairAt(t, h, session, base.DurableRevision+5, base.LocalRevision+6)
		if facts := col.invalidations(); len(facts) != 11 {
			t.Fatalf("delivery facts = %d, want the eleven publications of one handoff cycle with a single reservation: %+v", len(facts), facts)
		}
	})
}

// TestSteeringRollingCompactionExcludesArrivals drives the estimator-triggered
// rolling compaction at the real model effect over an admitted run: steering
// is pending before the first checkpoint and more arrives during the frozen
// orchestration. It asserts no compact piece performs a handoff, no
// intermediate durable summary exists between the pieces, the frozen piece
// inputs exclude both arrivals, the successor's first request projects only
// the summary, and the nonzero compact usage is counted exactly once.
func TestSteeringRollingCompactionExcludesArrivals(t *testing.T) {
	store := emptyStore(t)
	var (
		mu               sync.Mutex
		conversationReqs []model.Request
		pieceReqs        []model.Request
	)
	modelFn := func(_ context.Context, req model.Request) (model.Stream, error) {
		mu.Lock()
		conversationReqs = append(conversationReqs, req)
		mu.Unlock()
		return completedTurnStream(), nil
	}
	prepared := modelPrepared(modelFn)
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	sessionID := createSession(t, h)
	c := cachedCoordinator(t, h, sessionID)
	// the two-piece driver's exact message sizes become one settled input and
	// one admitted input, so the real projected conversation packs into the
	// same rolling two pieces
	pieces := compactTwoPieceSnapshot(t)
	if _, disposition := mustAdmitWithoutExecution(t, h, sessionID, "op-prev", []model.ContentPart{{Kind: model.PartText, Text: pieces[0].TextContent()}}); disposition != DispositionAdmitted {
		t.Fatalf("disposition %q, want the first message admitted", disposition)
	}
	if err := h.settleAgentTerminal(c, "op-prev", agent.TerminalResult{Status: agent.TerminalSuccess}, nil); err != nil {
		t.Fatalf("settle the first Operation: %v", err)
	}
	operationID := testOpID // the trigger fixture projects the current Operation
	if _, disposition := mustAdmitWithoutExecution(t, h, sessionID, operationID, []model.ContentPart{{Kind: model.PartText, Text: pieces[1].TextContent()}}); disposition != DispositionAdmitted {
		t.Fatalf("disposition %q, want the second message admitted under its own Operation", disposition)
	}
	execCtx := context.Background()
	run := &activeExecution{done: make(chan struct{}), execCtx: execCtx, cancel: func() {}}
	c.mu.Lock()
	c.run = run
	c.mu.Unlock()

	// steering pending before the first checkpoint
	if _, err := h.Submit(execCtx, SubmitRequest{
		SessionID: sessionID, OperationID: "op-2", Origin: InputOriginUser,
		Content: admissionContent("s1"), Mode: MessageModeRegular,
	}); err != nil {
		t.Fatalf("pre-checkpoint steering submit: %v", err)
	}

	source := h.contextSource(c, run, operationID)
	if _, err := source(execCtx); err != nil {
		t.Fatalf("first context call: %v", err)
	}
	req := projectedTriggerRequest(t, h, c)
	capture := triggerCapture(t, req, true) // the request overflows the window: the checkpoint fires
	compactRef := testCompactCapture().Model
	exec := Execution{
		Model: func(_ context.Context, r model.Request) (model.Stream, error) {
			mu.Lock()
			conversationReqs = append(conversationReqs, r)
			mu.Unlock()
			return completedTurnStream(), nil
		},
		CompactModel: func(_ context.Context, r model.Request) (model.Stream, error) {
			mu.Lock()
			pieceReqs = append(pieceReqs, r)
			piece := len(pieceReqs)
			mu.Unlock()
			if piece == 1 {
				// steering arriving during the frozen orchestration
				if _, err := h.Submit(execCtx, SubmitRequest{
					SessionID: sessionID, OperationID: "op-3", Origin: InputOriginUser,
					Content: admissionContent("s2"), Mode: MessageModeRegular,
				}); err != nil {
					return nil, err
				}
				return summaryStream("piece-one", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			}
			// no intermediate durable summary exists between the pieces
			entries, err := store.ReadEntries(execCtx, sessionID, 0)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.Kind == EntryCompaction {
					return nil, errors.New("an intermediate durable summary exists between the pieces")
				}
			}
			return summaryStream("piece-two-summary", model.Usage{InputTokens: 4, OutputTokens: 2}), nil
		},
		NormalizeTool: objectNormalize,
	}
	set, err := invokeTriggerEffect(t, h.modelEffect(c, operationID, exec, capture), req)
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if set.Disposition != agent.DispoReady {
		t.Fatalf("settlement = %+v, want the ready turn after the checkpoint", set.Disposition)
	}

	// the frozen piece inputs exclude both arrivals and roll through the
	// previous summary
	mu.Lock()
	pieceRequests := append([]model.Request(nil), pieceReqs...)
	mu.Unlock()
	if len(pieceRequests) != 2 {
		t.Fatalf("compact pieces = %d, want the rolling two", len(pieceRequests))
	}
	for i, piece := range pieceRequests {
		for _, msg := range piece.Messages {
			if strings.Contains(msg.TextContent(), "s1") || strings.Contains(msg.TextContent(), "s2") {
				t.Fatalf("piece %d carried a steering arrival: %+v", i, piece.Messages)
			}
		}
		if i == 0 && strings.Contains(textsOf(piece)[len(textsOf(piece))-1], "Previous summary:") {
			t.Fatalf("piece 1 carried a previous-summary wrapper: %v", textsOf(piece))
		}
		if i == 1 && !strings.Contains(textsOf(piece)[len(textsOf(piece))-1], "Previous summary:\npiece-one") {
			t.Fatalf("piece 2 omitted the previous summary: %v", textsOf(piece))
		}
	}

	// no piece handoff: the predecessor is still running and both heads are
	// still buffered; exactly one durable summary exists
	c.mu.Lock()
	running := c.graph.Session.State.CurrentOperationID == operationID
	buffered := len(c.steering)
	c.mu.Unlock()
	if !running || buffered != 2 {
		t.Fatalf("after the pieces: running=%v buffered=%d, want the running predecessor with both heads buffered", running, buffered)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the checkpoint: %v", err)
	}
	compactions := 0
	for _, entry := range graph.Entries {
		if entry.Compaction == nil {
			continue
		}
		compactions++
		if entry.Compaction.Summary != "piece-two-summary" {
			t.Fatalf("summary = %q, want the final piece's", entry.Compaction.Summary)
		}
		if entry.Compaction.Usage == nil || entry.Compaction.Usage.InputTokens != 5 || entry.Compaction.Usage.OutputTokens != 3 {
			t.Fatalf("compaction usage = %+v, want the nonzero pieces' total counted once", entry.Compaction.Usage)
		}
	}
	if compactions != 1 {
		t.Fatalf("compaction entries = %d, want exactly one with no intermediate summary", compactions)
	}
	opRec := settledOperation(t, store, sessionID, operationID)
	compactUsage := UsageTotals{ByModel: []ModelUsage{{Model: compactRef, Usage: UsageCount{InputTokens: 5, OutputTokens: 3}}}}
	if len(opRec.State.Usage.ByModel) != 2 { // the conversation turn and the compact total
		t.Fatalf("predecessor usage = %+v, want the conversation and compact contributions", opRec.State.Usage)
	}
	foundCompact := false
	for _, model := range opRec.State.Usage.ByModel {
		if model.Model == compactRef && model.Usage == compactUsage.ByModel[0].Usage {
			foundCompact = true
		}
	}
	if !foundCompact {
		t.Fatalf("predecessor usage = %+v, want the compact total counted exactly once", opRec.State.Usage)
	}

	// the boundary handoff and the shared drain deliver the heads in order
	if _, err := source(execCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("next context call = %v, want the handoff's cancellation", err)
	}
	if rec := settledOperation(t, store, sessionID, operationID); rec.State.Status != OperationSuccess {
		t.Fatalf("predecessor settled %q, want the handoff's silent success", rec.State.Status)
	}
	h.drainBuffers(c, run)
	awaitOperationTerminal(t, store, sessionID, "op-2", OperationSuccess)
	awaitOperationTerminal(t, store, sessionID, "op-3", OperationSuccess)

	// the successor's first request is summary-only: the summary message and
	// its own admitted input, with the compacted history replaced
	mu.Lock()
	requests := append([]model.Request(nil), conversationReqs...)
	mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("conversation requests = %d, want the predecessor's and one per successor", len(requests))
	}
	successor := textsOf(requests[1])
	// summary-only checkpoint projection: the summary replaces the covered
	// inputs; the post-checkpoint turn output and the successor's own input
	// remain
	if len(successor) != 4 || !strings.Contains(successor[1], "piece-two-summary") || successor[2] != "done" || successor[3] != "s1" {
		t.Fatalf("successor projection = %v, want [system, summary, post-checkpoint turn, s1]", successor)
	}
	for _, covered := range []string{pieces[0].TextContent(), pieces[1].TextContent()} {
		for _, msg := range successor {
			if msg == covered {
				t.Fatalf("successor projection carried the compacted-away input %q: %v", covered, successor)
			}
		}
	}
	if owner, _ := steeringInputOwner(t, store, sessionID, "s1"); owner != "op-2" {
		t.Fatalf("s1 owned by %q, want op-2", owner)
	}
	if owner, _ := steeringInputOwner(t, store, sessionID, "s2"); owner != "op-3" {
		t.Fatalf("s2 owned by %q, want op-3", owner)
	}
}

// TestMessageOperationInputOwnership proves the durable input-ownership
// rule: an
// Operation-owned input must equal its owning message Operation's admitted
// entry — extra owned inputs in running or terminal message Operations and
// any input owned by a compact Operation are corruption, copied prefix
// inputs omit ownership and stay valid, and reads return the corruption
// rather than treating an extra input as an admitted rewind boundary.
func TestMessageOperationInputOwnership(t *testing.T) {
	terminalGraph := func() *testGraph {
		fixture := validTestGraph()
		fixture.session.State.CurrentOperationID = ""
		stamped := testTime
		op := &fixture.ops[0]
		op.State.Status = OperationSuccess
		op.State.SettledAt = &stamped
		op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
		settlement := validSettlementEntry()
		settlement.EntryID = hexID(3)
		settlement.Status = OperationSuccess
		fixture.entries = append(fixture.entries, testEntry{
			env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime},
			settlement: &settlement,
		})
		return fixture
	}
	steeringEntry := func(entryID, owner string, sequence int64) testEntry {
		input := validInputEntry(owner)
		input.EntryID = entryID
		input.Content = []model.ContentPart{{Kind: model.PartText, Text: "steer"}}
		return testEntry{
			env:   Entry{SessionID: testSessionID, ID: entryID, OperationID: owner, Kind: EntryInput, Sequence: sequence, CommittedAt: testTime},
			input: &input,
		}
	}

	t.Run("running message operation with its admitted input stays valid", func(t *testing.T) {
		if _, err := validateFixture(t, validTestGraph().storage(t), testSessionID); err != nil {
			t.Fatalf("valid running graph rejected: %v", err)
		}
	})
	t.Run("terminal message operation with its admitted input stays valid", func(t *testing.T) {
		if _, err := validateFixture(t, terminalGraph().storage(t), testSessionID); err != nil {
			t.Fatalf("valid terminal graph rejected: %v", err)
		}
	})
	t.Run("extra owned input in a running message operation is corrupt", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.entries = append(fixture.entries, steeringEntry(hexID(4), testOpID, 4))
		if err := wantCorruption(t, validateFixtureErr(t, fixture)); err == nil {
			t.Fatalf("an extra Operation-owned input in a running Operation stayed valid")
		}
	})
	t.Run("extra owned input in a terminal message operation is corrupt", func(t *testing.T) {
		fixture := terminalGraph()
		fixture.entries = append(fixture.entries, steeringEntry(hexID(4), testOpID, 4))
		if err := wantCorruption(t, validateFixtureErr(t, fixture)); err == nil {
			t.Fatalf("an extra Operation-owned input in a terminal Operation stayed valid")
		}
	})
	t.Run("compact operation without input stays valid", func(t *testing.T) {
		fixture := terminalGraph()
		fixture.ops[0].Admission.RequestKind = RequestKindCompact
		fixture.ops[0].Admission.AdmittedEntry = EntryRef{}
		fixture.entries = fixture.entries[1:] // drop the admitted input: a compact Operation owns none
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("valid compact graph rejected: %v", err)
		}
	})
	t.Run("compact-owned input is corrupt", func(t *testing.T) {
		fixture := terminalGraph()
		fixture.ops[0].Admission.RequestKind = RequestKindCompact
		fixture.ops[0].Admission.AdmittedEntry = EntryRef{}
		fixture.entries = append(fixture.entries, steeringEntry(hexID(4), testOpID, 4))
		if err := wantCorruption(t, validateFixtureErr(t, fixture)); err == nil {
			t.Fatalf("a compact-owned input stayed valid")
		}
	})
	t.Run("copied operationless prefix stays valid and forged ownership is corrupt", func(t *testing.T) {
		fixture := terminalGraph()
		fixture.session.Identity.SourceSessionID = "000000000000000000000000000000aa" // the fork shape
		fixture.session.Identity.SourceBoundaryEntryID = hexID(1)
		for i := range fixture.entries { // the copied prefix precedes every owned entry
			fixture.entries[i].env.Sequence++
		}
		copied := steeringEntry(hexID(0), "", 1)
		fixture.entries = append([]testEntry{copied}, fixture.entries...)
		if _, err := validateFixture(t, fixture.storage(t), testSessionID); err != nil {
			t.Fatalf("valid copied prefix rejected: %v", err)
		}

		forged := steeringEntry(hexID(0), testOpID, 1)
		fixture.entries[0] = forged
		err := wantCorruption(t, validateFixtureErr(t, fixture))
		if err == nil || !strings.Contains(err.Detail, "admitted input") {
			t.Fatalf("forged ownership error = %v, want the admitted-input ownership corruption", err)
		}
	})
	t.Run("reads return the corruption instead of an admitted rewind boundary", func(t *testing.T) {
		store := emptyStore(t)
		prepare := func(context.Context, PreparationRequest) (PreparedExecution, error) { return validPrepared(), nil }
		h := newTestHarness(t, store, prepare)
		session := createSession(t, h)
		mustAdmitWithoutExecution(t, h, session, "op-1", admissionContent("hello"))
		entryID, err := newHexID()
		if err != nil {
			t.Fatalf("entry id: %v", err)
		}
		payload, err := encodeInputEntry(inputEntry{
			SessionID: session, EntryID: entryID, OperationID: "op-1",
			Origin: InputOriginUser, Content: admissionContent("extra"),
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if err := store.Transact(context.Background(), func(tx Transaction) error {
			_, err := tx.InsertEntry(EntryDraft{SessionID: session, ID: entryID, OperationID: "op-1", Kind: EntryInput, Payload: payload})
			return err
		}); err != nil {
			t.Fatalf("inject the extra owned input: %v", err)
		}

		fresh := newTestHarness(t, store, prepare) // a new reader revalidates the persisted graph
		if _, err := fresh.SnapshotSession(context.Background(), session); !isCorruption(err) {
			t.Fatalf("SnapshotSession = %v, want the typed corruption", err)
		}
		if _, _, err := fresh.ReadSessionHistory(context.Background(), session); !isCorruption(err) {
			t.Fatalf("ReadSessionHistory = %v, want the typed corruption", err)
		}
		if err := Recover(context.Background(), store); err != nil {
			t.Fatalf("Recover = %v, want the corrupt Session left unchanged", err)
		}
	})
}

// validateFixtureErr validates one fixture graph and returns its error.
func validateFixtureErr(t *testing.T, fixture *testGraph) error {
	t.Helper()
	_, err := validateFixture(t, fixture.storage(t), testSessionID)
	return err
}
