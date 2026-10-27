package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// executeDirect drives one admitted Operation's private execution and
// completion directly, without installing a run slot — the direct effect-test
// shape over the two actual production functions with a constructed run.
func executeDirect(h *Harness, c *coordinator, execCtx context.Context, operationID string, prepared PreparedExecution) error {
	run := &activeExecution{execCtx: execCtx, cancel: func() {}}
	res, closeInvocation, closeExecution, err := h.execute(c, operationID, prepared, execCtx)
	return h.finishExecution(c, run, operationID, prepared, res, err, closeInvocation, closeExecution)
}

// newAdmittedHarness admits one running Operation ("op-1") over a fresh empty
// store under the given Harness context and prepared execution: the shared
// admission construction of the effect and opener fixtures.
func newAdmittedHarness(t *testing.T, hctx context.Context, prepared PreparedExecution) (*Harness, *graphStorage, *coordinator, string) {
	t.Helper()
	store := emptyStore(t)
	h, err := New(hctx, Dependencies{Storage: store, Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return prepared, nil
	}})
	testassert.NoError(t, err)
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	{
		_, disposition := mustAdmitWithoutExecution(t, h, session.Identity.SessionID, testOpID, admissionContent("hello"))
		testassert.Equal(t, disposition, DispositionAdmitted)
	}
	c := leaseCoordinator(t, h, session.Identity.SessionID)
	return h, store, c, session.Identity.SessionID
}

// newEffectHarness admits one running Operation ("op-1") with the given
// prepared physical model request function and returns the pieces the effect
// fixtures need.
func newEffectHarness(t *testing.T, modelFn func(context.Context, model.Request) (model.Stream, error)) (*Harness, *graphStorage, *coordinator, string) {
	t.Helper()
	return newAdmittedHarness(t, context.Background(), modelPrepared(modelFn))
}

// defaultModelRequest builds the fixture conversation request the default
// invocation callers drive.
func defaultModelRequest(t *testing.T) model.Request {
	t.Helper()
	req, err := model.NewRequest(model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("hello")}},
		Tools:    []model.ToolDefinition{testToolDefinition()},
	})
	testassert.NoError(t, err)
	return req
}

// invokeModelEffect drives one model effect with the given validated request
// under a background context; a nil assemble callback runs the real Agent
// assembly.
func invokeModelEffect(t *testing.T, me agent.ModelEffect, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
	t.Helper()
	return invokeModelEffectCtx(t, me, context.Background(), req, assemble)
}

// fixtureProjection wraps one Operation's pipeline request source with the
// test capture and a frozen round, exposing only the projected messages for
// fixtures that read the boundary directly. The source is built once, so its
// steering-boundary selection across calls is the production behavior.
func fixtureProjection(h *Harness, c *coordinator) func(context.Context) ([]model.Message, error) {
	source := h.requestSource(c, testOpID, testCapture(), Execution{}, InvocationExecution{}, fixtureRound(h, c))
	return func(ctx context.Context) ([]model.Message, error) {
		req, err := source(ctx)
		if err != nil {
			return nil, err
		}
		return req.Messages, nil
	}
}

// fixtureRound freezes the current durable conversation state into a
// conversationRound through one run of the pipeline request source: the
// frozen-request shape the direct-drive fixtures dispatch under.
func fixtureRound(h *Harness, c *coordinator) *conversationRound {
	round := &conversationRound{}
	if _, err := h.requestSource(c, testOpID, testCapture(), Execution{}, InvocationExecution{}, round)(context.Background()); err != nil {
		panic(err)
	}
	return round
}

func testToolCall(id string) model.ToolCall {
	return model.ToolCall{ID: id, Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}
}

// objectNormalize is the fixtures' argument normalizer: it accepts exactly
// one non-null JSON object and returns its compact encoding.
func objectNormalize(call model.ToolCall) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &obj); err != nil || obj == nil {
		return nil, errors.New("arguments must be one non-null JSON object")
	}
	return json.Marshal(obj)
}

// fixturePermission is one built-in-allowed declaration: command.run allows
// any target, so fixture plans reach their intended executor or immediate
// result through the permission boundary.
var fixturePermission = []PermissionRequest{{Permission: PermissionCommandRun, Target: "fixture"}}

// invocationExecution supplies the fixtures' opened invocation: the given
// model and tool callbacks over the required pure normalizer.
func invocationExecution(modelFn func(context.Context, model.Request) (model.Stream, error), toolFn func(context.Context, model.ToolCall) PreparedTool) InvocationExecution {
	return InvocationExecution{Model: modelFn, Tool: toolFn, NormalizeTool: objectNormalize}
}

// effectExecution supplies the normalizer every opened invocation requires,
// answering every Engine request with the same model and tool callbacks; a
// nil tool callback answers with the empty plan.
func effectExecution(modelFn func(context.Context, model.Request) (model.Stream, error), toolFn func(context.Context, model.ToolCall) PreparedTool) Execution {
	if toolFn == nil {
		toolFn = func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }
	}
	return Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		return invocationExecution(modelFn, toolFn), nil
	}}
}

// purposeExecution returns the effect-driven fixtures' execution: its opener
// answers conversation Engine requests with the conversation callback and
// maintenance requests with the maintenance callback, both over an empty-plan
// tool function and the required normalizer.
func purposeExecution(conversation, maintenance func(context.Context, model.Request) (model.Stream, error)) Execution {
	return Execution{OpenInvocation: func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
		modelFn := conversation
		if request.Purpose == InvocationPurposeMaintenance {
			modelFn = maintenance
		}
		return invocationExecution(modelFn, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }), nil
	}}
}

// openedConversation opens the given execution's conversation invocation with
// the fixture model — the opened shape the direct effect fixtures drive.
func openedConversation(t *testing.T, exec Execution) InvocationExecution {
	t.Helper()
	inv, err := exec.OpenInvocation(context.Background(), InvocationRequest{
		ID:      "inv-conversation",
		Purpose: InvocationPurposeConversation,
		Model:   testCapture().Model,
	})
	testassert.NoError(t, err)
	return inv
}

// scriptStream is one fake accepted model stream: it yields the scripted
// deltas in order, then either its read failure or io.EOF, counting Close
// calls so tests pin exactly-once close of the accepted stream. A non-nil
// onExhausted hook runs on the read after the last delta, before the EOF or
// read-failure report.
type scriptStream struct {
	deltas      []model.StreamDelta
	err         error // non-EOF read failure yielded after the deltas when non-nil
	i           int
	closes      int
	onExhausted func()
}

func (s *scriptStream) Recv() (model.StreamDelta, error) {
	if s.i >= len(s.deltas) {
		if s.onExhausted != nil {
			s.onExhausted()
		}
		if s.err != nil {
			s.i++
			return model.StreamDelta{}, s.err
		}
		return model.StreamDelta{}, io.EOF
	}
	d := s.deltas[s.i]
	s.i++
	return d, nil
}

func (s *scriptStream) Close() error { s.closes++; return nil }

// streamOf builds one accepted stream yielding the deltas then EOF.
func streamOf(deltas ...model.StreamDelta) *scriptStream {
	return &scriptStream{deltas: deltas}
}

// failStream builds one accepted stream yielding the deltas then the read
// failure; assembly classifies the failed read.
func failStream(err error, deltas ...model.StreamDelta) *scriptStream {
	return &scriptStream{deltas: deltas, err: err}
}

// turnDelta builds one assistant delta carrying the given text and tool calls
// plus their finish reason, the one content delta of the fixture turns.
func turnDelta(finish, text string, calls ...model.ToolCall) model.StreamDelta {
	d := model.StreamDelta{
		HasChoice: true,
		Role:      "assistant",
		ContentFragments: []model.ContentFragment{{
			Position: 0,
			Kind:     model.PartText,
			Text:     text,
		}},
		FinishReason: finish,
	}
	for i, call := range calls {
		position := i
		d.ToolFragments = append(d.ToolFragments, model.ToolCallFragment{
			Position:         &position,
			ID:               call.ID,
			Name:             call.Name,
			ArgumentFragment: string(call.Arguments),
		})
	}
	return d
}

// usageDelta carries only trailing usage.
func usageDelta(u model.Usage) model.StreamDelta {
	return model.StreamDelta{Usage: &u}
}

// completedTurnStream assembles to the fixture completed output: text "done"
// plus the given calls, with the fixture usage reported.
func completedTurnStream(calls ...model.ToolCall) *scriptStream {
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	return streamOf(turnDelta(finish, "done", calls...), usageDelta(model.Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}))
}

// erroredTurnStream assembles to the fixture errored partial: text "partial"
// with the fixture usage, then the read failure — an accepted stream whose
// assembly errors while retaining the partial.
func erroredTurnStream() *scriptStream {
	return failStream(errors.New("provider failure"),
		turnDelta("", "partial"), usageDelta(model.Usage{InputTokens: 5, OutputTokens: 7}))
}

// TestModelEffectReadyRemainsRunning proves the ready transition: the
// committed assistant consumes the effect's reserved identity, completed calls
// receive their reservations, usage lands on the entry and both totals, and
// the Operation stays running with the effect cleared.
func TestModelEffectReadyRemainsRunning(t *testing.T) {
	var (
		store      *graphStorage
		sessionID  string
		reservedID string
	)
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID})
		if err != nil {
			return nil, err
		}
		op, err := decodeOperationRegister(reg)
		if err != nil {
			return nil, err
		}
		if op.State.ActiveEffect == nil || op.State.ActiveEffect.Kind != EffectModel {
			return nil, errors.New("no model effect intent is durable while the effect runs")
		}
		reservedID = op.State.ActiveEffect.ResultEntryID
		return completedTurnStream(testToolCall("call-1"), testToolCall("call-2")), nil
	}
	h, st, c, sid := newEffectHarness(t, modelFn)
	store, sessionID = st, sid

	calls := 0
	me := h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c))
	set, err := invokeModelEffect(t, me, defaultModelRequest(t), func(source model.ModelRef, stream model.Stream) (model.Output, error) {
		calls++
		return agent.Assemble(context.Background(), source, stream)
	})
	testassert.NoError(t, err)
	testassert.Equal(t, set.Disposition, agent.DispoReady)
	testassert.Equal(t, calls, 1)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var assistant *assistantEntry
	for i := range graph.Entries {
		if graph.Entries[i].Assistant != nil {
			assistant = graph.Entries[i].Assistant
		}
	}
	testassert.Require(t, assistant != nil, "ready settlement published no assistant entry")
	testassert.Require(t, assistant.EntryID == reservedID && (assistant.Status == model.OutputCompleted && len(assistant.ToolCalls) == 2), "got %v, want %v; assistant entry = %+v, want completed output with two calls", assistant.EntryID, reservedID, assistant)
	testassert.Require(t, assistant.Usage != nil && *assistant.Usage == (UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}), "assistant usage = %+v, want the reported counts", assistant.Usage)
	for i, call := range assistant.ToolCalls {
		testassert.Require(t, call.Ordinal == int64(i) && call.ID == fmt.Sprintf("call-%d", i+1) && call.ResultEntryID != "", "tool_calls[%d] = %+v, want ordered reserved call", i, call)
		{
			raw, derr := base64.StdEncoding.DecodeString(call.ArgumentsBase64)
			testassert.Require(t, derr == nil && string(raw) == `{"x":1}`, "tool_calls[%d] arguments = %q (%v), want the exact raw bytes", i, raw, derr)
		}
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 2, "operation state = %+v, want running with the effect cleared; pending calls = %+v, want the two published calls", rec.State, rec.State.PendingToolCalls)
	for i, call := range rec.State.PendingToolCalls {
		testassert.Require(t, call.CallID == assistant.ToolCalls[i].ID && call.ResultEntryID == assistant.ToolCalls[i].ResultEntryID && call.AssistantEntry.EntryID == assistant.EntryID, "pending call %d = %+v, want the assistant's reservation", i, call)
	}
	want := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}}}}
	testassert.Require(t, usageTotalsEqual(rec.State.Usage, want), "operation usage = %+v, want %+v", rec.State.Usage, want)
	view, err := h.ReadSessionView(context.Background(), sessionID)
	testassert.NoError(t, err)
	testassert.Require(t, view.Header.CurrentOperationID == testOpID && usageTotalsEqual(view.TotalUsage, want), "got %v, want %v; session usage = %+v, want %+v", view.Header.CurrentOperationID, testOpID, view.TotalUsage, want)

	// The second context projection reads the entry the first effect committed.
	source := fixtureProjection(h, c)
	msgs, err := source(context.Background())
	testassert.NoError(t, err)
	// system prompt, admitted input, committed assistant
	testassert.Equal(t, len(msgs), 3)
	last := msgs[len(msgs)-1]
	testassert.Require(t, last.Role == model.RoleAssistant && last.Source == testModelRef() && last.TextContent() == "done" && len(last.ToolCalls) == 2, "second projection's last message = %+v, want the committed assistant", last)
	testassert.Require(t, string(last.ToolCalls[0].Arguments) == `{"x":1}`, "projected call arguments = %s, want the exact raw bytes", last.ToolCalls[0].Arguments)
}

// TestModelEffectContinueRemainsRunning proves the continuation transition:
// the retained partial assistant commits with the fixed continuation signal
// and the Operation stays running.
func TestModelEffectContinueRemainsRunning(t *testing.T) {
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return erroredTurnStream(), nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
	testassert.NoError(t, err)
	testassert.Equal(t, set.Disposition, agent.DispoContinue)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var assistant *assistantEntry
	var signal *signalEntry
	for i := range graph.Entries {
		if graph.Entries[i].Assistant != nil {
			assistant = graph.Entries[i].Assistant
		}
		if graph.Entries[i].Signal != nil {
			signal = graph.Entries[i].Signal
		}
	}
	testassert.Require(t, assistant != nil && assistant.Status == model.OutputErrored && len(assistant.Content) == 1 && assistant.Content[0].Text == "partial", "assistant entry = %+v, want the retained errored partial", assistant)
	testassert.Require(t, signal != nil && signal.Signal == SignalModelFailureContinuation && signal.Content == signalModelFailureContinuationContent, "signal entry = %+v, want the fixed continuation signal", signal)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 0, "operation state = %+v, want running with the effect cleared and no pending calls", rec.State)
	want := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 5, OutputTokens: 7}}}}
	testassert.Require(t, usageTotalsEqual(rec.State.Usage, want), "operation usage = %+v, want %+v", rec.State.Usage, want)
	view, err := h.ReadSessionView(context.Background(), sessionID)
	testassert.NoError(t, err)
	testassert.Require(t, view.Header.CurrentOperationID == testOpID && usageTotalsEqual(view.TotalUsage, want), "session view = %+v, want the running operation and updated usage", view)
}

// TestModelEffectTerminalSettlement proves the model-derived failure and
// interruption settlements: each terminally settles in the one result
// transaction, preserving any produced assistant payload and consuming the
// effect's reserved identity when no assistant payload exists.
func TestModelEffectTerminalSettlement(t *testing.T) {
	t.Run("interruption retains the partial assistant", func(t *testing.T) {
		var (
			store      *graphStorage
			sessionID  string
			reservedID string
		)
		ctx, cancel := context.WithCancel(context.Background())
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			reservedID = readReservedIntent(t, store, sessionID)
			// the read failure is observed under the canceled execution
			// context after the partial arrived: assembly classifies the
			// accepted stream's output as interrupted, preserving the partial
			s := failStream(errors.New("model failure"), turnDelta("", "partial"))
			s.onExhausted = cancel
			return s, nil
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		set, err := invokeModelEffectCtx(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), ctx, defaultModelRequest(t), nil)
		testassert.NoError(t, err)
		testassert.Equal(t, set.Disposition, agent.DispoInterruption)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		var assistant *assistantEntry
		for i := range graph.Entries {
			if graph.Entries[i].Assistant != nil {
				assistant = graph.Entries[i].Assistant
			}
		}
		testassert.Require(t, assistant != nil && assistant.EntryID == reservedID, "assistant entry %v does not consume the reserved identity %s", assistant, reservedID)
		requireSessionCleared(t, h, sessionID)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == "stream interrupted: context canceled", "operation state = %+v, want terminal interruption with the assembly's detail", rec.State)
	})

	t.Run("nonretryable invocation failure settles under the reserved identity without an assistant payload", func(t *testing.T) {
		var (
			store      *graphStorage
			sessionID  string
			reservedID string
		)
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			reservedID = readReservedIntent(t, store, sessionID)
			return nil, errors.New("model failure")
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
		testassert.Require(t, set.Disposition == agent.DispoFailure && set.Detail == "model failure", "settlement = %+v, want the committed failure with its diagnostic", set)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		for _, entry := range graph.Entries {
			testassert.Require(t, entry.Assistant == nil && (entry.Settlement == nil || entry.Envelope.ID == reservedID), "assistant entry %s published for a payload-less failure; settlement entry %s does not consume the reserved identity %s", entry.Envelope.ID, entry.Envelope.ID, reservedID)
		}
		requireSessionCleared(t, h, sessionID)
	})

	t.Run("cancellation before an attempt settles without output", func(t *testing.T) {
		attempts := 0
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			attempts++
			return nil, nil
		}
		h, store, c, sessionID := newEffectHarness(t, modelFn)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the execution context dies before any attempt could start
		me := h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c))
		set, err := invokeModelEffectCtx(t, me, ctx, defaultModelRequest(t), nil)
		testassert.NoError(t, err)
		testassert.Require(t, set.Disposition == agent.DispoInterruption && set.Detail == executionInterruptedDetail, "settlement = %+v, want the committed interruption settlement", set)
		testassert.Equal(t, attempts, 0)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		signals, settlements := 0, 0
		for _, entry := range graph.Entries {
			switch {
			case entry.Assistant != nil:
				t.Fatalf("assistant entry %s published for an outputless interruption", entry.Envelope.ID)
			case entry.ToolResult != nil:
				t.Fatalf("tool result %s published for an outputless interruption", entry.Envelope.ID)
			case entry.Signal != nil:
				signals++
			case entry.Settlement != nil:
				settlements++
			}
		}
		testassert.Require(t, signals == 1 && settlements == 1, "committed %d signals and %d settlements, want one of each", signals, settlements)
		requireSessionCleared(t, h, sessionID)
	})
}

// TestModelEffectRepeatedCallIDSettlesFailure proves one Operation never
// publishes one tool call identity twice: a second effect repeating an
// already-published call ID is a protocol violation that settles terminal
// failure instead of committing a corrupt graph.
func TestModelEffectRepeatedCallIDSettlesFailure(t *testing.T) {
	turns := []model.Stream{
		completedTurnStream(testToolCall("call-1")),
		completedTurnStream(testToolCall("call-1")),
	}
	turn := 0
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		s := turns[turn]
		turn++
		return s, nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	me := h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c))
	{
		_, err := invokeModelEffect(t, me, defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
	_, err := invokeModelEffect(t, me, defaultModelRequest(t), nil)
	testassert.Require(t, err != nil, "second effect settled, want the repeated-call protocol violation")
	wantErr := err
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	assistants := 0
	for _, entry := range graph.Entries {
		if entry.Assistant != nil {
			assistants++
		}
	}
	testassert.Equal(t, assistants, 1)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == wantErr.Error(), "operation state = %+v, want terminal failure with the protocol error detail", rec.State)
	requireSessionCleared(t, h, sessionID)
}

// TestModelEffectTerminalNoOutputUsageOnSettlement proves a terminal output
// that reported usage without an eligible payload lands that usage on the
// settlement entry and both totals.
func TestModelEffectTerminalNoOutputUsageOnSettlement(t *testing.T) {
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return failStream(errors.New("empty failure"), usageDelta(model.Usage{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 6})), nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	{
		_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		if entry.Settlement == nil {
			continue
		}
		testassert.Require(t, entry.Settlement.Model != nil && *entry.Settlement.Model == testModelRef(), "settlement model = %+v, want the expected identity beside its usage", entry.Settlement.Model)
		testassert.Require(t, entry.Settlement.Usage != nil && *entry.Settlement.Usage == (UsageCount{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 6}) && !entry.Settlement.CompactionUsage, "settlement usage = %+v, want the reported no-output counts; settlement compaction_usage = true, want the conversation usage unmarked", entry.Settlement.Usage)
	}
	want := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 6}}}}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, usageTotalsEqual(rec.State.Usage, want), "operation usage = %+v, want %+v", rec.State.Usage, want)
	view, err := h.ReadSessionView(context.Background(), sessionID)
	testassert.NoError(t, err)
	testassert.Require(t, usageTotalsEqual(view.TotalUsage, want), "session usage = %+v, want %+v", view.TotalUsage, want)
	{
		measured := view.History.LatestContextUsage()
		testassert.Require(t, measured != nil && *measured == (UsageCount{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 6}), "latest context measurement = %+v, want the conversation no-output usage eligible", measured)
	}
}

// readReservedIntent reads the active model effect's reserved result identity
// from durable state. It runs inside a prepared model function, between the
// committed intent and the result transaction.
func readReservedIntent(t *testing.T, store *graphStorage, sessionID string) string {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID})
	testassert.NoError(t, err)
	op, err := decodeOperationRegister(reg)
	testassert.NoError(t, err)
	testassert.Require(t, op.State.ActiveEffect != nil && op.State.ActiveEffect.Kind == EffectModel, "no model effect intent is durable while the effect runs: %+v", op.State)
	return op.State.ActiveEffect.ResultEntryID
}

// requireSessionCleared asserts the Session register no longer names a
// current Operation after its terminal settlement.
func requireSessionCleared(t *testing.T, h *Harness, sessionID string) {
	t.Helper()
	session, err := h.ReadSessionHeader(context.Background(), sessionID)
	testassert.NoError(t, err)
	testassert.Equal(t, session.CurrentOperationID, "")
}

// TestModelEffectPublicationFailureLeavesIntentState proves the model result
// transaction's atomicity: an injected failure at any producer step rolls the
// whole settlement back, leaving the committed running/intent state for
// recovery and publishing nothing.
func TestModelEffectPublicationFailureLeavesIntentState(t *testing.T) {
	for _, fail := range []struct {
		step string
		nth  int
	}{
		{"insert_entry", 1},     // the assistant entry
		{"replace_register", 2}, // the result transaction's Operation register
		{"replace_register", 3}, // the result transaction's Session register
	} {
		t.Run(fmt.Sprintf("publication failure at %s #%d", fail.step, fail.nth), func(t *testing.T) {
			modelFn := func(context.Context, model.Request) (model.Stream, error) {
				return completedTurnStream(testToolCall("call-1"), testToolCall("call-2")), nil
			}
			h, store, c, sessionID := newEffectHarness(t, modelFn)
			count := 0
			store.txHook = func(step string) error {
				if step != fail.step {
					return nil
				}
				count++
				if count == fail.nth {
					return errors.New("injected publication failure")
				}
				return nil
			}
			{
				_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
				testassert.Require(t, err != nil, "model effect succeeded past an injected %s failure", fail.step)
			}
			graph, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
			// only the admitted input entry
			testassert.Equal(t, len(graph.Entries), 1)
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, err)
			testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect != nil, "operation state = %+v, want the committed running/intent state", rec.State)
		})
	}
}

// projectionTestGraph builds one terminal Operation whose history carries
// every Phase 3 entry kind: input, assistant with a call, its tool result, a
// signal, and the settlement.
func projectionTestGraph() *testGraph {
	fixture := validTestGraph()
	session := &fixture.session
	session.State.CurrentOperationID = ""
	op := &fixture.ops[0]
	stamped := testTime
	op.State.Status = OperationSuccess
	op.State.SettledAt = &stamped
	op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(4)}}
	op.State.PendingToolCalls = []PendingToolCall{}

	assistant := validAssistantEntry(testOpID)
	assistant.EntryID = hexID(2)
	call := validToolCallRecord()
	call.ResultEntryID = hexID(5)
	assistant.ToolCalls = []toolCallRecord{call}
	fixture.entries[1] = testEntry{env: Entry{SessionID: testSessionID, ID: hexID(2), OperationID: testOpID, Kind: EntryAssistant, Sequence: 2, CommittedAt: testTime}, assistant: &assistant}

	result := validToolResultEntry(testOpID)
	result.EntryID = hexID(5)
	result.AssistantEntry = EntryRef{SessionID: testSessionID, EntryID: hexID(2)}
	result.ToolCallID = "call-1"
	result.Status = model.ResultSuccess
	result.Content = "done"
	toolResult := testEntry{env: Entry{SessionID: testSessionID, ID: hexID(5), OperationID: testOpID, Kind: EntryToolResult, Sequence: 3, CommittedAt: testTime}, toolResult: &result}

	signal := validSignalEntry(testOpID)
	signal.EntryID = hexID(3)
	signalEntry := testEntry{env: Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntrySignal, Sequence: 4, CommittedAt: testTime}, signal: &signal}

	settlement := operationSettlementEntry{SessionID: testSessionID, EntryID: hexID(4), OperationID: testOpID, Status: OperationSuccess}
	settlementEntry := testEntry{env: Entry{SessionID: testSessionID, ID: hexID(4), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 5, CommittedAt: testTime}, settlement: &settlement}

	fixture.entries = append(fixture.entries, toolResult, signalEntry, settlementEntry)
	return fixture
}

// TestRequestSourceProjectsFullHistory proves the one kind-to-message mapping
// over the full committed history plus the captured system prompt.
func TestRequestSourceProjectsFullHistory(t *testing.T) {
	store := projectionTestGraph().storage(t)
	h := newTestHarness(t, store, nil)
	c := leaseCoordinator(t, h, testSessionID)
	msgs, err := fixtureProjection(h, c)(context.Background())
	testassert.NoError(t, err)
	testassert.Equal(t, len(msgs), 5)
	testassert.Require(t, msgs[0].Role == model.RoleSystem && msgs[0].TextContent() == "system", "first message = %+v, want the captured system prompt", msgs[0])
	testassert.Require(t, msgs[1].Role == model.RoleUser && msgs[1].TextContent() == "hello", "input message = %+v, want the admitted user input", msgs[1])
	assistant := msgs[2]
	testassert.Require(t, assistant.Role == model.RoleAssistant && assistant.Source == testModelRef() && assistant.TextContent() == "hi" && (len(assistant.ToolCalls) == 1 && assistant.ToolCalls[0].ID == "call-1" && string(assistant.ToolCalls[0].Arguments) == `{"x":1}`), "assistant message = %+v, want the committed assistant payload; assistant call = %+v, want the call with its exact raw argument bytes", assistant, assistant.ToolCalls)
	testassert.Require(t, msgs[3].Role == model.RoleTool && msgs[3].ToolCallID == "call-1" && msgs[3].TextContent() == "done", "tool message = %+v, want the tool result under its call identity", msgs[3])
	want := "<system-signal>" + signalInterruptionContent + "</system-signal>"
	testassert.Require(t, msgs[4].Role == model.RoleUser && msgs[4].TextContent() == want, "signal message = %+v, want the wrapped signal text %q", msgs[4], want)

	// The two live boundary-selection cases share the fixture pieces: one
	// immediate tool plan and the real model/tool effect drivers, whose
	// callbacks complete before any late arrival.
	plan := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ok"}}}
	}
	runEffects := func(t *testing.T, h *Harness, c *coordinator, modelFn func(context.Context, model.Request) (model.Stream, error)) {
		t.Helper()
		exec := effectExecution(modelFn, plan)
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, exec, invocationExecution(modelFn, plan), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.NoError(t, err)
		}
		{
			_, err := h.toolEffect(c, testOpID, exec, invocationExecution(modelFn, plan), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
			testassert.NoError(t, err)
		}
	}

	t.Run("first projection ignores early pending steering", func(t *testing.T) {
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		h, _, c, sessionID := newEffectHarness(t, modelFn)
		{
			_, err := submitText(t, h, sessionID, "op-2", MessageModeRegular, "s1")
			testassert.NoError(t, err)
		}
		first, err := fixtureProjection(h, c)(context.Background())
		testassert.NoError(t, err)
		testassert.Require(t, len(first) == 2 && first[1].TextContent() == "hello", "first projection = %+v, want the admitted input only despite the pending head", first)
	})

	t.Run("late arrival selects the boundary at the next call", func(t *testing.T) {
		modelCalls := 0
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			modelCalls++
			return completedTurnStream(testToolCall("call-1")), nil
		}
		h, store, c, sessionID := newEffectHarness(t, modelFn)
		liveCtx, liveCancel := context.WithCancel(context.Background())
		defer liveCancel()
		run := &activeExecution{done: make(chan struct{}), execCtx: liveCtx, cancel: func() {}}
		c.mu.Lock()
		c.run = run // the live execution whose context selection must leave alone
		c.mu.Unlock()
		source := fixtureProjection(h, c)
		first, err := source(context.Background()) // steering is empty through the first projection
		testassert.Require(t, err == nil && len(first) == 2 && first[1].TextContent() == "hello", "first projection = (%+v, %v), want the admitted input with empty steering", first, err)
		runEffects(t, h, c, modelFn) // the real model and tool callbacks return before the head arrives
		{
			_, err := submitText(t, h, sessionID, "op-2", MessageModeRegular, "s1")
			testassert.NoError(t, err)
		}
		before := storedEntryCount(store, sessionID)
		msgs, err := source(context.Background())
		// exact identity: the private token, never a text or cancellation class
		testassert.Require(t, err == errSteeringBoundary, "boundary selection = (%v, %v), want the private boundary token verbatim", msgs, err)
		testassert.Require(t, msgs == nil, "boundary selection projected %v, want no messages", msgs)
		{
			got := storedEntryCount(store, sessionID)
			testassert.Require(t, got == before, "boundary selection wrote %d entries, want no storage", got-before)
		}
		testassert.Equal(t, modelCalls, 1)
		c.mu.Lock()
		buffered := len(c.steering)
		c.mu.Unlock()
		testassert.Equal(t, buffered, 1)
		if run.execCtx.Err() != nil {
			t.Fatalf("execution context = %v, want it unchanged and live after selection", run.execCtx.Err())
		}
	})
}

// TestSignalProjectionEscaping proves the signal wrapper escapes &, < and >
// in the signal content.
func TestSignalProjectionEscaping(t *testing.T) {
	got := signalProjectedText(`a & b < c > d`)
	testassert.Equal(t, got, "<system-signal>a &amp; b &lt; c &gt; d</system-signal>")
}

// foreignEffectRace writes one foreign state change under the coordinator's
// view: the Session register's agent type and the Operation register's
// revision both move, so every effect transaction's revision guard fires and
// the next read observes the foreign state.
func foreignEffectRace(t *testing.T, store *graphStorage, sessionID, operationID, agentType string) {
	t.Helper()
	err := store.Transact(context.Background(), func(tx Transaction) error {
		key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		current, err := decodeSessionRegister(reg)
		if err != nil {
			return err
		}
		state := current.State
		state.CurrentAgentType = agentType
		payload, err := encodeSessionRegister(SessionRecord{Identity: current.Identity, State: state})
		if err != nil {
			return err
		}
		if _, err := tx.ReplaceRegister(key, reg.Revision, payload); err != nil {
			return err
		}
		opKey := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID}
		oreg, err := tx.ReadRegister(opKey)
		if err != nil {
			return err
		}
		op, err := decodeOperationRegister(oreg)
		if err != nil {
			return err
		}
		opPayload, err := encodeOperationRegister(op)
		if err != nil {
			return err
		}
		_, err = tx.ReplaceRegister(opKey, oreg.Revision, opPayload)
		return err
	})
	testassert.NoError(t, err)
}

// TestEffectTransactionsRematerializeOnRevisionRace proves every effect
// transaction's revision guard mirrors the landed ChangeAgentType pattern: the
// race error returns and the coordinator rematerializes, so the next read
// observes the foreign state instead of the stale view.
func TestEffectTransactionsRematerializeOnRevisionRace(t *testing.T) {
	t.Run("intent transaction", func(t *testing.T) {
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}
		h, store, c, sessionID := newEffectHarness(t, modelFn)
		foreignEffectRace(t, store, sessionID, testOpID, "foreign")
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.Require(t, errors.Is(err, ErrConflict), "intent over a foreign revision = %v, want the revision-race conflict", err)
		}
		{
			session, err := h.ReadSessionHeader(context.Background(), sessionID)
			testassert.Require(t, err == nil && session.CurrentAgentType == "foreign", "session after the race = %+v (%v), want the foreign agent type", session, err)
		}
	})
	t.Run("result transaction", func(t *testing.T) {
		var (
			store     *graphStorage
			sessionID string
		)
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			foreignEffectRace(t, store, sessionID, testOpID, "foreign") // between the committed intent and the result transaction
			return completedTurnStream(), nil
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.Require(t, errors.Is(err, ErrConflict), "result over a foreign revision = %v, want the revision-race conflict", err)
		}
		{
			session, err := h.ReadSessionHeader(context.Background(), sessionID)
			testassert.Require(t, err == nil && session.CurrentAgentType == "foreign", "session after the race = %+v (%v), want the foreign agent type", session, err)
		}
	})
}

// foreignTerminalSettle writes one foreign terminal settlement directly into
// the Operation register, as a foreign writer would; the Session register does
// not move.
func foreignTerminalSettle(t *testing.T, store *graphStorage, sessionID, operationID string) {
	t.Helper()
	err := store.Transact(context.Background(), func(tx Transaction) error {
		key := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		op, err := decodeOperationRegister(reg)
		if err != nil {
			return err
		}
		stamped := testTime
		op.State.Status = OperationFailure
		op.State.SettledAt = &stamped
		op.State.ActiveEffect = nil
		op.State.Request = nil
		op.State.PendingToolCalls = []PendingToolCall{}
		op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: sessionID, EntryID: hexID(9)}, Detail: "foreign"}
		payload, err := encodeOperationRegister(op)
		if err != nil {
			return err
		}
		_, err = tx.ReplaceRegister(key, reg.Revision, payload)
		return err
	})
	testassert.NoError(t, err)
}

// TestEffectTransactionsPreconditionsOutrankRevisionRace proves the mismatch
// path of every effect transaction mirrors publishAdmission: a freshly decoded
// register showing the semantic precondition violated returns ErrInvalid; only
// a clean mismatch returns the revision-race conflict.
func TestEffectTransactionsPreconditionsOutrankRevisionRace(t *testing.T) {
	t.Run("intent over a foreign terminal operation", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		foreignTerminalSettle(t, store, sessionID, testOpID)
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}
		me := h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c))
		{
			_, err := invokeModelEffect(t, me, defaultModelRequest(t), nil)
			testassert.Require(t, errors.Is(err, ErrInvalid), "intent over a foreign terminal operation = %v, want ErrInvalid", err)
		}
	})
	t.Run("result over a foreign terminal operation", func(t *testing.T) {
		var (
			store     *graphStorage
			sessionID string
		)
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			foreignTerminalSettle(t, store, sessionID, testOpID) // between the committed intent and the result transaction
			return completedTurnStream(), nil
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.Require(t, errors.Is(err, ErrInvalid), "result over a foreign terminal operation = %v, want ErrInvalid", err)
		}
	})
}

// toolSpy records the dispatch order of one execution's tool calls and
// answers every preparation with the configured plan.
type toolSpy struct {
	mu    sync.Mutex
	order []string
	plan  func(_ context.Context, call model.ToolCall) PreparedTool
}

func (s *toolSpy) tool(_ context.Context, call model.ToolCall) PreparedTool {
	s.mu.Lock()
	s.order = append(s.order, call.ID)
	s.mu.Unlock()
	if s.plan != nil {
		return s.plan(context.Background(), call)
	}
	return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
}

func (s *toolSpy) dispatched() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.order...)
}

// newOpenerHarness admits one running Operation ("op-1") under a cancelable
// Harness context whose preparation returns the given opener, returning the
// pieces the execution fixtures need.
func newOpenerHarness(t *testing.T, open func(context.Context, OperationAdmission) (Execution, error)) (*Harness, *graphStorage, *coordinator, string, PreparedExecution, context.CancelFunc) {
	t.Helper()
	prepared := PreparedExecution{Capture: testCapture(), Open: open}
	hctx, cancel := context.WithCancel(context.Background())
	h, store, c, sessionID := newAdmittedHarness(t, hctx, prepared)
	return h, store, c, sessionID, prepared, cancel
}

// newExecutionHarness admits one running Operation ("op-1") under a cancelable
// Harness context with both prepared effect functions, returning the pieces
// the execution fixtures need.
func newExecutionHarness(t *testing.T, modelFn func(context.Context, model.Request) (model.Stream, error), toolFn func(context.Context, model.ToolCall) PreparedTool) (*Harness, *graphStorage, *coordinator, string, PreparedExecution, context.CancelFunc) {
	t.Helper()
	testassert.Require(t, toolFn != nil, "execution fixtures require a prepared tool function")
	return newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
		return effectExecution(modelFn, toolFn), nil
	})
}

// invokeModelEffectCtx drives one model effect with the given validated
// request and an explicit context; a nil assemble callback runs the real Agent
// assembly under that context.
func invokeModelEffectCtx(t *testing.T, me agent.ModelEffect, ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
	t.Helper()
	if assemble == nil {
		assemble = func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			return agent.Assemble(ctx, source, stream)
		}
	}
	return me(ctx, req, assemble)
}

// TestModelEffectGateSkipsCallbackOnCancellation proves execution cancellation
// stops new callbacks: a context dying between the committed intent and the
// physical request settles the Operation as terminal interruption without
// ever starting the callback.
func TestModelEffectGateSkipsCallbackOnCancellation(t *testing.T) {
	invoked := 0
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		invoked++
		return completedTurnStream(), nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	ctx, cancel := context.WithCancel(context.Background())
	store.txHook = func(step string) error {
		if step == "replace_register" {
			cancel() // the execution context dies between the committed intent and the callback
		}
		return nil
	}
	me := h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c))
	set, err := invokeModelEffectCtx(t, me, ctx, defaultModelRequest(t), nil)
	testassert.NoError(t, err)
	testassert.Equal(t, invoked, 0)
	testassert.Require(t, set.Disposition == agent.DispoInterruption && set.Detail == executionInterruptedDetail, "settlement = %+v, want the committed interruption settlement", set)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "operation state = %+v, want terminal interruption", rec.State)
	requireSessionCleared(t, h, sessionID)
	{
		_, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
	}
}

// TestExecuteSuccessSettlesOuterTerminal proves the private agent.Run
// composition and the outer terminal settlement: a clean run settles success
// through the common terminal helper with no detail.
func TestExecuteSuccessSettlesOuterTerminal(t *testing.T) {
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil
	}
	spy := &toolSpy{}
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, spy.tool)
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.NoError(t, err)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationSuccess && rec.State.Terminal != nil && rec.State.Terminal.Detail == "", "operation state = %+v, want terminal success without detail", rec.State)
	requireSessionCleared(t, h, sessionID)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	settlements := 0
	for _, entry := range graph.Entries {
		if entry.Settlement != nil {
			settlements++
			testassert.Require(t, entry.Settlement.Status == OperationSuccess && entry.Settlement.Detail == "", "settlement = %+v, want success without detail", entry.Settlement)
		}
	}
	testassert.Equal(t, settlements, 1)
}

// TestExecuteOpensOnceWithCommittedAdmission proves the opener row: execute
// invokes the preparation's opener exactly once on the execution context with
// the owned committed admission carrying the prepared capture, then runs the
// Agent over the opened effects.
func TestExecuteOpensOnceWithCommittedAdmission(t *testing.T) {
	type openRecord struct {
		ctx context.Context
		adm OperationAdmission
	}
	var opens []openRecord
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil
	}
	spy := &toolSpy{}
	h, _, c, sessionID, prepared, _ := newOpenerHarness(t, func(ctx context.Context, adm OperationAdmission) (Execution, error) {
		opens = append(opens, openRecord{ctx: ctx, adm: adm})
		return effectExecution(modelFn, spy.tool), nil
	})
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.NoError(t, err)
	}
	testassert.Equal(t, len(opens), 1)
	testassert.Equal(t, opens[0].ctx, h.ctx)
	adm := opens[0].adm
	testassert.Require(t, adm.SessionID == sessionID && adm.OperationID == testOpID && adm.AgentType == "coder" && reflect.DeepEqual(adm.Execution, testCapture()), "opener admission = %+v, want the owned committed admission; opener capture = %+v, want the prepared capture", adm, adm.Execution)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Equal(t, rec.State.Status, OperationSuccess)
}

// TestExecuteCanceledBeforeOpenSkipsOpener proves the canceled-execution row:
// an execution whose context is already lost never starts the opener and
// settles the terminal interruption.
func TestExecuteCanceledBeforeOpenSkipsOpener(t *testing.T) {
	opens := 0
	h, store, c, sessionID, prepared, cancel := newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
		opens++
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}
		return effectExecution(modelFn, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }), nil
	})
	cancel() // the execution context is lost before the opener could start
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.Require(t, err == nil, "execute = %v, want the entry interruption settled with no run error", err)
	}
	testassert.Equal(t, opens, 0)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "operation state = %+v, want terminal interruption", rec.State)
	requireSessionCleared(t, h, sessionID)
	{
		_, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
	}
}

// TestExecuteOpenerErrorSettlesOrdinaryTerminals proves the opening-error
// rows: an ordinary opener error settles failure with the error's own text
// and a storage-class opener error retains the running state for recovery —
// in both cases the Agent never runs, and the opener owned any provisional
// cleanup, not the Harness.
func TestExecuteOpenerErrorSettlesOrdinaryTerminals(t *testing.T) {
	t.Run("ordinary opener error settles failure", func(t *testing.T) {
		openErr := errors.New("open broke")
		modelRuns := 0
		h, store, c, sessionID, prepared, _ := newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
			return effectExecution(func(context.Context, model.Request) (model.Stream, error) {
				modelRuns++
				return nil, nil
			}, nil), openErr
		})
		{
			err := executeDirect(h, c, h.ctx, testOpID, prepared)
			testassert.Equal(t, err, openErr)
		}
		testassert.Equal(t, modelRuns, 0)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == "open broke", "operation state = %+v, want terminal failure with the opener error text", rec.State)
		requireSessionCleared(t, h, sessionID)
		{
			_, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
		}
	})

	t.Run("storage opener error retains running state", func(t *testing.T) {
		storageErr := fmt.Errorf("opener storage failure: %w", ErrStorage)
		h, _, c, sessionID, prepared, _ := newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
			return Execution{}, storageErr
		})
		{
			err := executeDirect(h, c, h.ctx, testOpID, prepared)
			testassert.Equal(t, err, storageErr)
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Equal(t, rec.State.Status, OperationRunning)
		session, err := h.ReadSessionHeader(context.Background(), sessionID)
		testassert.NoError(t, err)
		testassert.Equal(t, session.CurrentOperationID, testOpID)
	})
}

// TestExecuteInvalidOpenedExecutionClosesBeforeRejection proves the
// invalid-success rows: a successful Open with a nil invocation opener has
// its execution Close invoked exactly once inline before the rejection, and a
// successful Open whose opened conversation invocation has a nil Model, Tool,
// or NormalizeTool has the invocation's non-nil Close invoked exactly once
// before the rejection and the execution's Close once through completion —
// while the Operation is still running — never runs the Agent, and settles
// failure through the ordinary terminal settlement.
func TestExecuteInvalidOpenedExecutionClosesBeforeRejection(t *testing.T) {
	for _, name := range []string{"nil opener", "nil model", "nil tool", "nil normalization"} {
		t.Run(name, func(t *testing.T) {
			var h *Harness
			invCloses, execCloses := 0, 0
			var statusDuringClose OperationState
			modelRuns := 0
			open := func(_ context.Context, adm OperationAdmission) (Execution, error) {
				exec := effectExecution(func(context.Context, model.Request) (model.Stream, error) {
					modelRuns++
					return completedTurnStream(), nil
				}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} })
				if name == "nil opener" {
					exec.OpenInvocation = nil
				} else {
					exec.OpenInvocation = func(_ context.Context, request InvocationRequest) (InvocationExecution, error) {
						inv := invocationExecution(func(context.Context, model.Request) (model.Stream, error) {
							modelRuns++
							return completedTurnStream(), nil
						}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} })
						switch name {
						case "nil model":
							inv.Model = nil
						case "nil tool":
							inv.Tool = nil
						case "nil normalization":
							inv.NormalizeTool = nil
						}
						inv.Close = func() error {
							invCloses++
							rec, err := h.ReadOperation(context.Background(), adm.SessionID, adm.OperationID)
							testassert.Check(t, err == nil, "read operation from the rejection closer: %v", err)
							statusDuringClose = rec.State.Status
							return nil
						}
						return inv, nil
					}
				}
				exec.Close = func() error { execCloses++; return nil }
				return exec, nil
			}
			var store *graphStorage
			var c *coordinator
			var sessionID string
			var prepared PreparedExecution
			h, store, c, sessionID, prepared, _ = newOpenerHarness(t, open)
			err := executeDirect(h, c, h.ctx, testOpID, prepared)
			testassert.Require(t, errors.Is(err, ErrInvalid), "execute = %v, want the invalid-execution rejection", err)
			testassert.Equal(t, modelRuns, 0)
			if name == "nil opener" {
				testassert.Equal(t, execCloses, 1) // only the execution closes; no invocation was ever opened
			} else {
				testassert.Equal(t, invCloses, 1)
				testassert.Equal(t, execCloses, 1)
				testassert.Equal(t, statusDuringClose, OperationRunning)
			}
			settled, readErr := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, readErr)
			testassert.Require(t, settled.State.Status == OperationFailure && settled.State.Terminal != nil && settled.State.Terminal.Detail == err.Error(), "operation state = %+v, want terminal failure with the rejection text %q", settled.State, err)
			requireSessionCleared(t, h, sessionID)
			{
				_, err := validateFixture(t, store, sessionID)
				testassert.NoError(t, err)
			}
		})
	}
}

// TestToolEffectPlansAndOutcomes proves the prepared-tool contract at the
// effect boundary: an immediate plan commits its ordinary terminal result
// without an effect intent, an executor-backed plan commits intent then one
// validated outcome, an invalid plan shape maps to the fixed validation-error
// result for the original call, and an unauthorized effect plan — missing or
// malformed declarations — settles the fixed denial without ever running its
// executor. Only effect-producing plans need declarations; an immediate
// error commits without any.
func TestToolEffectPlansAndOutcomes(t *testing.T) {
	publishCalls := func(t *testing.T, h *Harness, c *coordinator, sessionID string) {
		t.Helper()
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.NoError(t, err)
		}
	}
	success := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
	}
	cases := []struct {
		name           string
		plan           func(_ context.Context, call model.ToolCall) PreparedTool
		want           model.ToolResult
		wantReplaces   int
		wantExecutions int64 // how often the executor body ran
	}{
		{
			name:         "immediate plan commits without an effect intent",
			plan:         success,
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"},
			wantReplaces: 2, // Operation + Session registers only: no intent
		},
		{
			name: "executor plan commits intent then one validated outcome",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
				}}
			},
			want:           model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"},
			wantReplaces:   3, // intent + Operation + Session
			wantExecutions: 1,
		},
		{
			name:         "plan without immediate result or executor maps to the validation error",
			plan:         func(_ context.Context, call model.ToolCall) PreparedTool { return PreparedTool{} },
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantReplaces: 2,
		},
		{
			name: "shape-invalid plan stays the validation error even with allowed declarations",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantReplaces: 2,
		},
		{
			name: "executor without declarations is denied and never executes",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
				}}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: PermissionDeniedContent},
			wantReplaces: 2, // denial settles through the intent-free transition
		},
		{
			name: "immediate success without declarations is denied",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: PermissionDeniedContent},
			wantReplaces: 2,
		},
		{
			name: "immediate error needs no declaration and still commits",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "boom"}}}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: "boom"},
			wantReplaces: 2,
		},
		{
			name: "immediate interrupted needs no declaration and still commits",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultInterrupted, Content: interruptedToolResultContent}}}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent},
			wantReplaces: 2,
		},
		{
			name: "immediate denied needs no declaration and still commits",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultDenied, Content: "denied"}}}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: "denied"},
			wantReplaces: 2,
		},
		{
			name: "empty permission or target pair is denied",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions: []PermissionRequest{{Permission: PermissionCommandRun, Target: "fixture"}, {Permission: "", Target: "fixture"}},
					Execute: func(context.Context) ToolOutcome {
						return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
					},
				}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: PermissionDeniedContent},
			wantReplaces: 2,
		},
		{
			name: "one built-in-denied pair denies the whole call",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions: []PermissionRequest{
						{Permission: PermissionCommandRun, Target: "fixture"},
						{Permission: PermissionFileWrite, Target: "/w/sub/.env"},
					},
					CanonicalWorkspace: "/w",
					Execute: func(context.Context) ToolOutcome {
						return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
					},
				}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: PermissionDeniedContent},
			wantReplaces: 2,
		},
		{
			name: "returned outcome answering another call maps to the validation error",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: "other-call", Status: model.ResultSuccess, Content: "ran"}}
				}}
			},
			want:           model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantReplaces:   3,
			wantExecutions: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, c, sessionID := newEffectHarness(t, nil)
			publishCalls(t, h, c, sessionID)
			replaces := 0
			store.txHook = func(step string) error {
				if step == "replace_register" {
					replaces++
				}
				return nil
			}
			var executions int64
			plan := func(ctx context.Context, call model.ToolCall) PreparedTool {
				inner := tc.plan(ctx, call)
				if inner.Execute == nil {
					return inner
				}
				original := inner.Execute
				inner.Execute = func(ctx context.Context) ToolOutcome {
					executions++
					return original(ctx)
				}
				return inner
			}
			got, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), invocationExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
			testassert.NoError(t, err)
			testassert.Equal(t, got, tc.want)
			testassert.Equal(t, executions, tc.wantExecutions)
			testassert.Equal(t, replaces, tc.wantReplaces)
			graph, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
			results := 0
			for _, entry := range graph.Entries {
				if entry.ToolResult != nil {
					results++
					testassert.Require(t, entry.ToolResult.ToolCallID == "call-1" && entry.ToolResult.Status == tc.want.Status && entry.ToolResult.Content == tc.want.Content, "tool result = %+v, want %+v", entry.ToolResult, tc.want)
				}
			}
			testassert.Equal(t, results, 1)
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			testassert.NoError(t, err)
			testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && len(rec.State.PendingToolCalls) == 0, "operation state = %+v, want running with the effect cleared and the call resolved", rec.State)
		})
	}
}

// TestToolEffectPermissionBoundary proves the fixed permission boundary every
// effect plan passes before any effect begins: file targets must be canonical
// and contained in the prepared canonical Workspace root under the built-in
// policy, user policy can deny what the built-ins allow, a readonly Agent
// with no write_dir denies every file.write pair, any configured write_dir
// confines file.write to the prepared canonical write directory
// independently of permission allow, non-file permissions need no canonical
// binding, and one denied pair denies the whole call.
func TestToolEffectPermissionBoundary(t *testing.T) {
	allowAllWrites := ResolvePermissionPolicy(json.RawMessage(`{"rules":[{"permission":"file.write","target":"*","access":"allow"}]}`), nil)
	filePlanRoots := func(workspace, writeDir string, permissions ...PermissionRequest) func(_ context.Context, call model.ToolCall) PreparedTool {
		return func(_ context.Context, call model.ToolCall) PreparedTool {
			return PreparedTool{
				Permissions:        permissions,
				CanonicalWorkspace: workspace,
				CanonicalWriteDir:  writeDir,
				Execute: func(context.Context) ToolOutcome {
					return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
				},
			}
		}
	}
	filePlan := func(permissions ...PermissionRequest) func(_ context.Context, call model.ToolCall) PreparedTool {
		return filePlanRoots("/w", "/w/sub", permissions...)
	}
	captureWith := func(writeDir string, readonly bool) ExecutionCapture {
		capture := testCapture()
		capture.WriteDir, capture.Readonly = writeDir, readonly
		return capture
	}
	cases := []struct {
		name         string
		capture      ExecutionCapture
		policy       PermissionPolicy
		plan         func(_ context.Context, call model.ToolCall) PreparedTool
		wantContent  string
		wantDenied   bool
		wantExecuted bool
	}{
		{
			name:        "file.read inside the canonical Workspace is allowed",
			plan:        filePlan(PermissionRequest{Permission: PermissionFileRead, Target: "/w/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "file.write inside the canonical Workspace is allowed without readonly",
			plan:        filePlan(PermissionRequest{Permission: PermissionFileWrite, Target: "/w/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "file.read outside the canonical Workspace is denied",
			plan:        filePlan(PermissionRequest{Permission: PermissionFileRead, Target: "/other/a.txt"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "an explicit user rule allows a file.read target outside the canonical Workspace",
			policy:      ResolvePermissionPolicy(json.RawMessage(`{"rules":[{"permission":"file.read","target":"*","access":"allow"}]}`), nil),
			plan:        filePlan(PermissionRequest{Permission: PermissionFileRead, Target: "/other/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "empty file target is denied",
			plan:        filePlan(PermissionRequest{Permission: PermissionFileRead, Target: ""}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "relative file target is denied",
			plan:        filePlan(PermissionRequest{Permission: PermissionFileRead, Target: "a.txt"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "lexically unclean file target is denied",
			plan:        filePlan(PermissionRequest{Permission: PermissionFileWrite, Target: "/w/sub/../b.txt"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name: "empty canonical Workspace root for a file plan is denied",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions: []PermissionRequest{{Permission: PermissionFileRead, Target: "/w/a.txt"}},
					Execute: func(context.Context) ToolOutcome {
						return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
					},
				}
			},
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "a relative canonical Workspace root is denied",
			plan:        filePlanRoots("w", "/w/sub", PermissionRequest{Permission: PermissionFileRead, Target: "/w/a.txt"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "a lexically unclean canonical Workspace root is denied",
			plan:        filePlanRoots("/w/../w", "/w/sub", PermissionRequest{Permission: PermissionFileRead, Target: "/w/a.txt"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "a relative canonical write root is denied",
			capture:     captureWith("/w/sub", false),
			policy:      allowAllWrites,
			plan:        filePlanRoots("/w", "sub", PermissionRequest{Permission: PermissionFileWrite, Target: "/w/sub/a.txt"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "a lexically unclean canonical write root is denied",
			capture:     captureWith("/w/sub", false),
			policy:      allowAllWrites,
			plan:        filePlanRoots("/w", "/w/sub/../sub", PermissionRequest{Permission: PermissionFileWrite, Target: "/w/sub/a.txt"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "readonly without write_dir denies file.write",
			capture:     captureWith("", true),
			plan:        filePlan(PermissionRequest{Permission: PermissionFileWrite, Target: "/w/a.txt"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "readonly without write_dir still allows file.read",
			capture:     captureWith("", true),
			plan:        filePlan(PermissionRequest{Permission: PermissionFileRead, Target: "/w/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "write_dir confines file.write inside it",
			capture:     captureWith("/w/sub", false),
			plan:        filePlan(PermissionRequest{Permission: PermissionFileWrite, Target: "/w/sub/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "write_dir denies file.write outside it even when policy allows",
			capture:     captureWith("/w/sub", false),
			policy:      allowAllWrites,
			plan:        filePlan(PermissionRequest{Permission: PermissionFileWrite, Target: "/w/other.txt"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:    "configured write_dir without a canonical prepared write root is denied",
			capture: captureWith("/w/sub", false),
			policy:  allowAllWrites,
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions:        []PermissionRequest{{Permission: PermissionFileWrite, Target: "/w/sub/a.txt"}},
					CanonicalWorkspace: "/w",
					Execute: func(context.Context) ToolOutcome {
						return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
					},
				}
			},
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "readonly with write_dir allows confined writes",
			capture:     captureWith("/w/sub", true),
			policy:      allowAllWrites,
			plan:        filePlan(PermissionRequest{Permission: PermissionFileWrite, Target: "/w/sub/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "write_dir does not lift the built-in sensitive-basename denial",
			capture:     captureWith("/w/sub", false),
			plan:        filePlan(PermissionRequest{Permission: PermissionFileWrite, Target: "/w/sub/.env"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name:        "an explicit user rule overrides the sensitive denial inside write_dir",
			capture:     captureWith("/w/sub", false),
			policy:      allowAllWrites,
			plan:        filePlan(PermissionRequest{Permission: PermissionFileWrite, Target: "/w/sub/.env"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name: "command-only plan needs no canonical bindings",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions: []PermissionRequest{{Permission: PermissionCommandRun, Target: "git status"}},
					Execute: func(context.Context) ToolOutcome {
						return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
					},
				}
			},
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "unknown permission namespace is denied",
			plan:        filePlan(PermissionRequest{Permission: "file.exec", Target: "*"}),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name: "multi-target patch with every pair allowed executes once",
			plan: filePlan(
				PermissionRequest{Permission: PermissionFileRead, Target: "/w/src.txt"},
				PermissionRequest{Permission: PermissionFileWrite, Target: "/w/dst.txt"},
			),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name: "multi-target patch with a denied source denies the whole call",
			plan: filePlan(
				PermissionRequest{Permission: PermissionFileRead, Target: "/other/src.txt"},
				PermissionRequest{Permission: PermissionFileWrite, Target: "/w/dst.txt"},
			),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name: "multi-target patch with a denied destination denies the whole call",
			plan: filePlan(
				PermissionRequest{Permission: PermissionFileRead, Target: "/w/src.txt"},
				PermissionRequest{Permission: PermissionFileWrite, Target: "/other/dst.txt"},
			),
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
		{
			name: "immediate success passes through the same evaluation",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions:        []PermissionRequest{{Permission: PermissionFileWrite, Target: "/w/a.txt"}},
					CanonicalWorkspace: "/w",
					Immediate:          &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}},
				}
			},
			wantContent: "done",
		},
		{
			name:    "immediate success denied by the boundary carries no result content",
			capture: captureWith("", true),
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions:        []PermissionRequest{{Permission: PermissionFileWrite, Target: "/w/a.txt"}},
					CanonicalWorkspace: "/w",
					Immediate:          &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}},
				}
			},
			wantContent: PermissionDeniedContent, wantDenied: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, c, sessionID := newEffectHarness(t, nil)
			publishCalls(t, h, c, sessionID, testToolCall("call-1"))
			capture := tc.capture
			if capture.ConfigurationRevision == "" {
				capture = testCapture()
			}
			var executions int64
			plan := func(ctx context.Context, call model.ToolCall) PreparedTool {
				inner := tc.plan(ctx, call)
				if inner.Execute == nil {
					return inner
				}
				original := inner.Execute
				inner.Execute = func(ctx context.Context) ToolOutcome {
					executions++
					return original(ctx)
				}
				return inner
			}
			got, err := h.toolEffect(c, testOpID, Execution{Permissions: tc.policy}, InvocationExecution{Tool: plan, NormalizeTool: objectNormalize}, capture)(context.Background(), testToolCall("call-1"), []string{"echo"})
			testassert.NoError(t, err)
			wantStatus := model.ResultSuccess
			if tc.wantDenied {
				wantStatus = model.ResultDenied
			}
			testassert.Require(t, got.Status == wantStatus && got.Content == tc.wantContent, "committed result = %+v, want status %s content %q", got, wantStatus, tc.wantContent)
			testassert.Require(t, tc.wantExecuted == (executions == 1) && executions <= 1, "executor ran %d times, want executed=%v", executions, tc.wantExecuted)
			graph, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
			for _, entry := range graph.Entries {
				if entry.ToolResult != nil {
					testassert.Require(t, !tc.wantDenied || len(entry.ToolResult.Metadata) == 0, "denied result carries metadata %s, want none", entry.ToolResult.Metadata)
				}
			}
		})
	}
}

// TestToolEffectDeniedSettlesOnlyItsCall proves the denial settlement scope:
// a denied executor-backed call settles exactly once through the intent-free
// transition, writes no tool active effect, runs no concrete effect, and
// leaves the Operation running for the later call, which still executes.
func TestToolEffectDeniedSettlesOnlyItsCall(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	publishCalls(t, h, c, sessionID, testToolCall("call-1"), testToolCall("call-2"))
	executions := 0
	plan := func(_ context.Context, call model.ToolCall) PreparedTool {
		if call.ID == "call-1" { // no declarations at all
			return PreparedTool{Execute: func(context.Context) ToolOutcome {
				executions++
				return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
			}}
		}
		return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
			executions++
			return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
		}}
	}
	replaces := 0
	store.txHook = func(step string) error {
		if step == "replace_register" {
			replaces++
		}
		return nil
	}
	got, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), invocationExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
	testassert.NoError(t, err)
	testassert.Require(t, got.Status == model.ResultDenied && got.Content == PermissionDeniedContent, "call-1 result = %+v, want the fixed denial", got)
	testassert.Equal(t, executions, 0)
	// Operation + Session only: denial never writes an effect intent
	testassert.Equal(t, replaces, 2)
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.State.ActiveEffect == nil && (len(rec.State.PendingToolCalls) == 1 && rec.State.PendingToolCalls[0].CallID == "call-2"), "operation after the denial = %+v, want running with no active effect; pending calls = %+v, want the later call untouched", rec.State, rec.State.PendingToolCalls)
	{
		_, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), invocationExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-2"), []string{"echo"})
		testassert.NoError(t, err)
	}
	testassert.Equal(t, executions, 1)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	byCall := map[string]toolResultEntry{}
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			byCall[entry.ToolResult.ToolCallID] = *entry.ToolResult
		}
	}
	testassert.Equal(t, len(byCall), 2)
	{
		got := byCall["call-1"]
		testassert.Require(t, got.Status == model.ResultDenied && len(got.Metadata) == 0, "call-1 result = %+v, want the metadata-less denial", got)
	}
	{
		got := byCall["call-2"]
		testassert.Require(t, got.Status == model.ResultSuccess && got.Content == "ran", "call-2 result = %+v, want the allowed execution", got)
	}
}

// TestToolEffectAdvertisementGate proves the one advertisement gate ahead of
// every preparer, consulted through the Engine-supplied per-call names of the
// producing request: a committed call whose name is absent from the names
// settles the ordinary unavailable-tool error with no preparation and no
// normalization even when a capture advertises that name — a call produced by
// an earlier request gains no retroactive authority from a later
// advertisement — a names-complete call proceeds to its plan, and a captured,
// registered name the names omit is still gated out.
func TestToolEffectAdvertisementGate(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	normalized := 0
	normalize := func(call model.ToolCall) (json.RawMessage, error) {
		normalized++
		return objectNormalize(call)
	}
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(
			model.ToolCall{ID: "call-1", Name: "ghost", Arguments: json.RawMessage(`{"x":1}`)},
			testToolCall("call-2"),
			testToolCall("call-3"),
		), nil
	}
	{
		_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, Execution{}, InvocationExecution{Model: modelFn, NormalizeTool: normalize}, testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
	testassert.Equal(t, normalized, 2)
	prepared := 0
	plan := func(_ context.Context, call model.ToolCall) PreparedTool {
		prepared++
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
	}
	// The producing request advertised only echo: the ghost call settles the
	// ordinary unavailable-tool error even under a capture that now also
	// advertises ghost — the capture grants it no retroactive authority.
	ghostCapture := testCapture()
	ghostCapture.Tools = append(ghostCapture.Tools, model.ToolDefinition{Name: "ghost", Description: "ghost tool", Parameters: json.RawMessage(`{"type":"object"}`)})
	got, err := h.toolEffect(c, testOpID, Execution{}, InvocationExecution{Tool: plan, NormalizeTool: normalize}, ghostCapture)(context.Background(), testToolCall("call-1"), []string{"echo"})
	testassert.NoError(t, err)
	testassert.Require(t, got.Status == model.ResultError && got.Content == `Tool "ghost" is not available.`, "names-absent result = %+v, want the ordinary unavailable-tool error", got)
	testassert.Equal(t, normalized, 2)
	testassert.Equal(t, prepared, 0)
	{
		_, err := h.toolEffect(c, testOpID, Execution{}, InvocationExecution{Tool: plan, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-2"), []string{"echo"})
		testassert.NoError(t, err)
	}
	testassert.Equal(t, prepared, 1)
	// A captured, registered name the names omit is gated out too.
	got, err = h.toolEffect(c, testOpID, Execution{}, InvocationExecution{Tool: plan, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-3"), nil)
	testassert.NoError(t, err)
	testassert.Require(t, got.Status == model.ResultError && got.Content == `Tool "echo" is not available.`, "names-omitted result = %+v, want the ordinary unavailable-tool error for the captured echo name", got)
	testassert.Equal(t, prepared, 1)
	{
		_, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
	}
}

// TestToolEffectConsumesCommittedNormalization proves the one normalized
// value chain: the tool boundary consumes the original assistant's committed
// normalized_arguments byte-identically with no second normalization; an
// invalid unhooked original is normalized again only to obtain its bounded
// useful validation diagnostic; a malformed callback outcome at the boundary
// stays the fixed internal-validation error; and the committed assistant
// record is never mutated afterwards.
func TestToolEffectConsumesCommittedNormalization(t *testing.T) {
	publishWithNormalize := func(t *testing.T, h *Harness, c *coordinator, normalize func(model.ToolCall) (json.RawMessage, error), calls ...model.ToolCall) {
		t.Helper()
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(calls...), nil
		}
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, Execution{}, InvocationExecution{Model: modelFn, NormalizeTool: normalize}, testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.NoError(t, err)
		}
	}
	committedAssistant := func(t *testing.T, store *graphStorage, sessionID string) toolCallRecord {
		t.Helper()
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		for _, entry := range graph.Entries {
			if entry.Assistant != nil && len(entry.Assistant.ToolCalls) > 0 {
				return entry.Assistant.ToolCalls[0]
			}
		}
		t.Fatalf("no committed assistant call")
		return toolCallRecord{}
	}

	t.Run("valid original normalizes once and the preparer sees the committed bytes", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		normalized := 0
		normalize := func(call model.ToolCall) (json.RawMessage, error) {
			normalized++
			original := append(json.RawMessage(nil), call.Arguments...)
			for i := range call.Arguments { // ownership oracle: clobber the received call's bytes in place
				call.Arguments[i] = ' '
			}
			return objectNormalize(model.ToolCall{ID: call.ID, Name: call.Name, Arguments: original})
		}
		publishWithNormalize(t, h, c, normalize, model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(` {"x": 1} `)})
		testassert.Equal(t, normalized, 1)
		before := committedAssistant(t, store, sessionID)
		testassert.Require(t, string(before.NormalizedArguments) == `{"x":1}`, "committed member = %q, want the compacted object", before.NormalizedArguments)
		var seenName string
		var seen json.RawMessage
		plan := func(_ context.Context, call model.ToolCall) PreparedTool {
			seenName = call.Name
			seen = append(json.RawMessage(nil), call.Arguments...)
			for i := range call.Arguments { // ownership oracle: clobber the received call's bytes in place
				call.Arguments[i] = ' '
			}
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
		}
		// The caller supplies a forged name and arguments that both differ from
		// the committed record; the boundary must ignore them and hand the
		// preparer the committed record's name and normalized bytes. A forged
		// unadvertised name would trip the advertisement gate if it were used.
		forged := model.ToolCall{ID: "call-1", Name: "forged", Arguments: json.RawMessage(`{"y":99}`)}
		got, err := h.toolEffect(c, testOpID, Execution{}, InvocationExecution{Tool: plan, NormalizeTool: normalize}, testCapture())(context.Background(), forged, []string{"echo"})
		testassert.NoError(t, err)
		testassert.Require(t, got.Status == model.ResultSuccess && got.Content == "done", "result = %+v, want the committed call to execute, not a forged-name rejection", got)
		testassert.Equal(t, normalized, 1)
		testassert.Equal(t, seenName, "echo")
		testassert.Require(t, string(seen) == `{"x":1}`, "preparer arguments = %q, want byte-identical committed bytes", seen)
		after := committedAssistant(t, store, sessionID)
		testassert.Require(t, string(after.NormalizedArguments) == string(before.NormalizedArguments), "assistant normalized member mutated to %q", after.NormalizedArguments)
		{
			raw, err := base64.StdEncoding.DecodeString(after.ArgumentsBase64)
			testassert.Require(t, err == nil && string(raw) == ` {"x": 1} `, "assistant raw arguments mutated to %q (%v)", raw, err)
		}
	})

	t.Run("executor intent consumes the committed bytes byte-identically", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		normalized := 0
		normalize := func(call model.ToolCall) (json.RawMessage, error) {
			normalized++
			return objectNormalize(call)
		}
		publishWithNormalize(t, h, c, normalize, model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(` {"x": 1} `)})
		testassert.Equal(t, normalized, 1)
		var seen json.RawMessage
		plan := func(_ context.Context, call model.ToolCall) PreparedTool {
			seen = append(json.RawMessage(nil), call.Arguments...)
			return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
				return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
			}}
		}
		replaces := 0
		store.txHook = func(step string) error {
			if step == "replace_register" {
				replaces++
			}
			return nil
		}
		got, err := h.toolEffect(c, testOpID, Execution{}, InvocationExecution{Tool: plan, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Require(t, got.Status == model.ResultSuccess && got.Content == "ran", "result = %+v, want the executed outcome", got)
		// Operation + Session + the tool intent
		testassert.Equal(t, replaces, 3)
		testassert.Equal(t, normalized, 1)
		testassert.Require(t, string(seen) == `{"x":1}`, "preparer arguments = %q, want the committed normalized bytes, not the raw %q", seen, ` {"x": 1} `)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		results := 0
		for _, entry := range graph.Entries {
			if entry.ToolResult != nil {
				results++
				testassert.Require(t, entry.ToolResult.Status == model.ResultSuccess && entry.ToolResult.Content == "ran", "committed result = %+v, want the executor's outcome", *entry.ToolResult)
			}
		}
		testassert.Equal(t, results, 1)
	})

	t.Run("invalid original re-normalizes once at the boundary for its diagnostic", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		normalized := 0
		normalize := func(call model.ToolCall) (json.RawMessage, error) {
			normalized++
			return objectNormalize(call)
		}
		publishWithNormalize(t, h, c, normalize, model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`not json`)})
		testassert.Equal(t, normalized, 1)
		testassert.Equal(t, len(committedAssistant(t, store, sessionID).NormalizedArguments), 0)
		prepared := 0
		plan := func(context.Context, model.ToolCall) PreparedTool {
			prepared++
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"}}}
		}
		got, err := h.toolEffect(c, testOpID, Execution{}, InvocationExecution{Tool: plan, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Equal(t, normalized, 2)
		testassert.Require(t, got.Status == model.ResultError && got.Content == "arguments must be one non-null JSON object", "result = %+v, want the callback's useful validation diagnostic", got)
		testassert.Equal(t, prepared, 0)
	})

	t.Run("committed extras reach the boundary in the decoded encoded form", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		producer := json.RawMessage(`{ "n" : [1, 1.0, 9007199254740993] }`) // raw producer form: whitespace survives until the commit encodes it
		var seenAtBoundary string
		normalize := func(call model.ToolCall) (json.RawMessage, error) {
			seenAtBoundary = string(call.Extra["form"])
			return objectNormalize(call)
		}
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			d := turnDelta("tool_calls", "done", model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{oops`)})
			d.ToolFragments[0].Extra = model.Extra{"form": producer}
			return streamOf(d, usageDelta(model.Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2})), nil
		}
		{
			_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, Execution{}, InvocationExecution{Model: modelFn, NormalizeTool: normalize}, testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
			testassert.NoError(t, err)
		}
		{
			got := string(committedAssistant(t, store, sessionID).Extra["form"])
			testassert.Equal(t, got, `{"n":[1,1.0,9007199254740993]}`)
		}
		for i := range producer { // ownership oracle: clobber the producer's buffer after the commit
			producer[i] = ' '
		}
		got, err := h.toolEffect(c, testOpID, Execution{}, InvocationExecution{Tool: func(context.Context, model.ToolCall) PreparedTool {
			t.Fatalf("preparer must not run for the invalid original")
			return PreparedTool{}
		}, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Require(t, got.Status == model.ResultError, "result = %+v, want the invalid original's validation error", got)
		testassert.Equal(t, seenAtBoundary, `{"n":[1,1.0,9007199254740993]}`)
	})

	t.Run("oversized diagnostic is bounded", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		diagnostic := "x" + strings.Repeat("y", 2*maxToolDiagnosticBytes)
		normalize := func(model.ToolCall) (json.RawMessage, error) {
			return nil, errors.New(diagnostic)
		}
		publishWithNormalize(t, h, c, normalize, model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`not json`)})
		got, err := h.toolEffect(c, testOpID, Execution{}, InvocationExecution{Tool: func(context.Context, model.ToolCall) PreparedTool {
			t.Fatalf("preparer must not run for an invalid original")
			return PreparedTool{}
		}, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Require(t, got.Status == model.ResultError && len(got.Content) <= maxToolDiagnosticBytes && strings.HasPrefix(got.Content, "xyyyyy"), "result = %+v, want the diagnostic truncated within %d bytes", got, maxToolDiagnosticBytes)
	})

	t.Run("malformed callback outcome at the boundary is the internal-validation error", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		normalized := 0
		producerNormalize := func(call model.ToolCall) (json.RawMessage, error) {
			normalized++
			return objectNormalize(call)
		}
		publishWithNormalize(t, h, c, producerNormalize, model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`not json`)})
		boundaryNormalize := func(model.ToolCall) (json.RawMessage, error) {
			normalized++
			return json.RawMessage(`[1,2]`), nil // valid JSON, not one object
		}
		got, err := h.toolEffect(c, testOpID, Execution{}, InvocationExecution{Tool: func(context.Context, model.ToolCall) PreparedTool {
			t.Fatalf("preparer must not run for a malformed callback outcome")
			return PreparedTool{}
		}, NormalizeTool: boundaryNormalize}, testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
		testassert.Equal(t, normalized, 2)
		testassert.Require(t, got.Status == model.ResultError && got.Content == invalidToolResultContent, "result = %+v, want the fixed internal-validation error", got)
	})
}

// TestToolOutcomeMetadataRules proves the tool-owned metadata boundary at the
// shared result commit: one well-formed value whose durable encoding fits the
// bound is committed verbatim while only the Result reaches the Agent, null
// and empty are treated as absent, and malformed or durable-bound-exceeding
// metadata is dropped while the result still commits. The shape matrix runs
// once through the executor transition; the immediate transition routes one
// drop-while-committing case, proving both callers reach the same shared
// boundary.
func TestToolOutcomeMetadataRules(t *testing.T) {
	cases := []struct {
		name      string
		metadata  json.RawMessage
		stored    string // expected committed metadata bytes; "" means the member stays absent
		immediate bool   // route through the immediate plan instead of the executor
	}{
		{name: "object metadata commits", metadata: json.RawMessage(`{"kind":"editpreview","files":["a.go"]}`), stored: `{"kind":"editpreview","files":["a.go"]}`},
		{name: "scalar metadata commits", metadata: json.RawMessage(`42`), stored: `42`},
		{name: "HTML characters persist in the escaped durable encoding", metadata: json.RawMessage(`"<"`), stored: `"\u003c"`},
		{name: "metadata exactly at the durable bound commits", metadata: json.RawMessage(`"` + strings.Repeat("x", maxToolMetadataBytes-2) + `"`), stored: `"` + strings.Repeat("x", maxToolMetadataBytes-2) + `"`},
		{name: "durable encoding one byte over the bound is dropped and the result commits", metadata: json.RawMessage(`"` + strings.Repeat("x", maxToolMetadataBytes-1) + `"`), stored: ""},
		// The two-representation regression: the raw value is exactly at the
		// bound, but HTML-escaping expands its durable encoding far past it,
		// so the metadata drops while the result still commits.
		{name: "raw at the bound but durable-oversized after escaping is dropped and the result commits", metadata: json.RawMessage(`"` + strings.Repeat("<", maxToolMetadataBytes-2) + `"`), stored: ""},
		{name: "malformed metadata is dropped and the result commits", metadata: json.RawMessage(`{broken`), stored: ""},
		{name: "null metadata is absent", metadata: json.RawMessage(`null`), stored: ""},
		{name: "empty metadata is absent", metadata: json.RawMessage{}, stored: ""},
		{name: "immediate plan drops malformed metadata while the result commits", metadata: json.RawMessage(`{broken`), stored: "", immediate: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, c, sessionID := newEffectHarness(t, nil)
			publishCalls(t, h, c, sessionID, testToolCall("call-1"))
			plan := func(_ context.Context, call model.ToolCall) PreparedTool {
				outcome := ToolOutcome{
					Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"},
					Metadata: tc.metadata,
				}
				if tc.immediate {
					return PreparedTool{Permissions: fixturePermission, Immediate: &outcome}
				}
				return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome { return outcome }}
			}
			got, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), invocationExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
			testassert.NoError(t, err)
			want := model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}
			testassert.Equal(t, got, want)
			graph, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
			var result *toolResultEntry
			for i := range graph.Entries {
				if graph.Entries[i].ToolResult != nil {
					result = graph.Entries[i].ToolResult
				}
			}
			testassert.Require(t, result != nil && (result.Status == model.ResultSuccess && result.Content == "ran"), "no tool result committed, want the result to survive any metadata decision; committed result = %+v, want the settled result", result)
			if tc.stored == "" {
				testassert.Require(t, len(result.Metadata) == 0, "committed metadata = %s, want absent", result.Metadata)
			} else if string(result.Metadata) != tc.stored {
				t.Fatalf("committed metadata = %q..., want %q...", result.Metadata[:min(len(result.Metadata), 20)], tc.stored[:min(len(tc.stored), 20)])
			}
		})
	}
}

// TestExecuteOrderedBatchSettlesExactlyOnce proves the ordered batch: calls
// dispatch in assembled order, every call receives exactly one terminal
// result, and the run continues to the outer success settlement.
func TestExecuteOrderedBatchSettlesExactlyOnce(t *testing.T) {
	turn := 0
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		turn++
		if turn == 1 {
			return completedTurnStream(testToolCall("call-1"), testToolCall("call-2")), nil
		}
		return completedTurnStream(), nil
	}
	spy := &toolSpy{}
	var h *Harness
	var sessionID string
	var steered bool
	toolFn := func(ctx context.Context, call model.ToolCall) PreparedTool {
		if !steered { // pending steering arrives inside the batch
			steered = true
			{
				_, err := submitText(t, h, sessionID, "op-2", MessageModeRegular, "steer-batch")
				testassert.Check(t, err == nil, "steering submit: %v", err)
			}
		}
		return spy.tool(ctx, call)
	}
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, toolFn)
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.NoError(t, err)
	}
	awaitOperationTerminal(t, store, sessionID, "op-2", OperationSuccess) // the pending head's own successor settles
	awaitHarnessQuiet(t, h, sessionID)
	{
		got := spy.dispatched()
		testassert.Require(t, len(got) == 2 && got[0] == "call-1" && got[1] == "call-2", "dispatch order = %v, want [call-1 call-2]", got)
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	results := map[string]int{}
	var lastSeq, firstAdmission int64
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			results[entry.ToolResult.ToolCallID]++
			testassert.Require(t, entry.Envelope.Sequence > lastSeq, "tool result %s committed out of order", entry.Envelope.ID)
		}
		if entry.Input != nil && entry.Input.Content[0].Text == "steer-batch" {
			firstAdmission = entry.Envelope.Sequence
		}
		lastSeq = entry.Envelope.Sequence
	}
	testassert.Require(t, results["call-1"] == 1 && results["call-2"] == 1, "result counts = %v, want exactly one per call", results)
	testassert.Require(t, firstAdmission != 0 && lastSeq != 0, "missing the successor admission or the last result entry")
	for _, entry := range graph.Entries {
		testassert.Require(t, entry.ToolResult == nil || entry.Envelope.Sequence < firstAdmission, "tool result %s committed after the successor admission", entry.Envelope.ID)
	}
	{
		owner, _ := steeringInputOwner(t, store, sessionID, "steer-batch")
		testassert.Equal(t, owner, "op-2")
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationSuccess && len(rec.State.PendingToolCalls) == 0, "operation state = %+v, want terminal success with every call resolved", rec.State)
}

// TestToolEffectRealOutcomeWinsCancellationRace proves a returned real outcome
// publishes even when the execution context died during the execution.
func TestToolEffectRealOutcomeWinsCancellationRace(t *testing.T) {
	var cancel context.CancelFunc
	turn := 0
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		turn++
		if turn == 1 {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		return completedTurnStream(), nil
	}
	toolFn := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Execute: func(ctx context.Context) ToolOutcome {
			cancel() // the execution context dies during the concrete execution
			return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
		}}
	}
	h, store, c, sessionID, prepared, harnessCancel := newExecutionHarness(t, modelFn, toolFn)
	cancel = harnessCancel
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.NoError(t, err)
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil && entry.ToolResult.ToolCallID == "call-1" {
			testassert.Require(t, entry.ToolResult.Status == model.ResultSuccess && entry.ToolResult.Content == "ran", "tool result = %+v, want the real outcome", entry.ToolResult)
		}
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Equal(t, rec.State.Status, OperationInterruption)
}

// TestToolOriginatedInterruptionSettlesUnstartedCalls proves the reused
// interrupted-result settlement: an executor returning the interrupted result
// stops the batch and every remaining unstarted call receives the ordinary
// interrupted result through the common terminal helper.
func TestToolOriginatedInterruptionSettlesUnstartedCalls(t *testing.T) {
	turn := 0
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		turn++
		if turn == 1 {
			return completedTurnStream(testToolCall("call-1"), testToolCall("call-2")), nil
		}
		return completedTurnStream(), nil
	}
	toolFn := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
			return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultInterrupted, Content: "stopped by the tool"}}
		}}
	}
	spy := &toolSpy{plan: toolFn}
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, spy.tool)
	// pending steering through the stopped batch
	{
		_, err := submitText(t, h, sessionID, "op-2", MessageModeRegular, "steer-int")
		testassert.NoError(t, err)
	}
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.NoError(t, err)
	}
	awaitOperationTerminal(t, store, sessionID, "op-2", OperationSuccess) // the pending head drains as its own Operation
	awaitHarnessQuiet(t, h, sessionID)
	{
		got := spy.dispatched()
		testassert.Require(t, len(got) == 1 && got[0] == "call-1", "dispatch order = %v, want the batch stopped after call-1", got)
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	byCall := map[string]toolResultEntry{}
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			byCall[entry.ToolResult.ToolCallID] = *entry.ToolResult
		}
	}
	{
		got := byCall["call-1"]
		testassert.Require(t, got.Status == model.ResultInterrupted && got.Content == "stopped by the tool", "call-1 result = %+v, want the executor's real interrupted result", got)
	}
	{
		got := byCall["call-2"]
		testassert.Require(t, got.Status == model.ResultInterrupted && got.Content == interruptedToolResultContent, "call-2 result = %+v, want the fixed interrupted-before-execution result", got)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == "agent interrupted", "operation state = %+v, want terminal interruption with the Agent's detail", rec.State)
	{
		owner, _ := steeringInputOwner(t, store, sessionID, "steer-int")
		testassert.Equal(t, owner, "op-2")
	}
	requireSessionCleared(t, h, sessionID)
}

// TestExecuteBetweenEffectCancellationSettlesInterruption proves the outer
// path owns between-effect cancellation: the run context dying between model
// effects settles the Operation as terminal interruption through the common
// terminal helper.
func TestExecuteBetweenEffectCancellationSettlesInterruption(t *testing.T) {
	turn := 0
	var cancel context.CancelFunc
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		turn++
		if turn == 2 {
			// the execution context dies when assembly closes the accepted
			// stream: the ready result still commits without cancellation
			// and the run's next checkpoint settles the interruption
			return &cancelOnCloseStream{scriptStream: completedTurnStream(), cancel: cancel}, nil
		}
		return erroredTurnStream(), nil
	}
	h, store, c, sessionID, prepared, harnessCancel := newExecutionHarness(t, modelFn, func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
	})
	cancel = harnessCancel
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.NoError(t, err)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == "agent interrupted", "operation state = %+v, want terminal interruption with the Agent's detail", rec.State)
	requireSessionCleared(t, h, sessionID)
	{
		_, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
	}
}

// TestExecutePast25ModelEffectsSettlesSuccessAndDrainsSteering proves a long
// Operation commits its continuations, then succeeds and drains buffered input.
func TestExecutePast25ModelEffectsSettlesSuccessAndDrainsSteering(t *testing.T) {
	var (
		mu        sync.Mutex
		modelRuns int
		arrived30 = make(chan struct{})
		gate30    = make(chan struct{})
	)
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		mu.Lock()
		modelRuns++
		n := modelRuns
		mu.Unlock()
		if n == 30 {
			close(arrived30)
			<-gate30
		}
		if n > 30 {
			return completedTurnStream(), nil // the drained successor's own turn
		}
		return erroredTurnStream(), nil
	}
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
	})
	done := make(chan error, 1)
	go func() { done <- executeDirect(h, c, h.ctx, testOpID, prepared) }()
	receiveBounded(t, arrived30, "the 30th model effect")
	{
		_, err := submitText(t, h, sessionID, "op-2", MessageModeRegular, "s1")
		testassert.NoError(t, err)
	}
	close(gate30)
	{
		err := <-done
		testassert.NoError(t, err)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationSuccess && rec.State.Terminal != nil && rec.State.Terminal.Detail == "", "operation state = %+v, want terminal success without a synthetic failure", rec.State)
	awaitOperationTerminal(t, store, sessionID, "op-2", OperationSuccess) // the pending head drains as its own Operation
	awaitHarnessQuiet(t, h, sessionID)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	assistants, signals := 0, 0
	for _, entry := range graph.Entries {
		if entry.Assistant != nil && entry.Envelope.OperationID == testOpID {
			assistants++
		}
		if entry.Signal != nil && entry.Envelope.OperationID == testOpID {
			signals++
		}
	}
	testassert.Require(t, assistants == 30 && signals == 30, "%d assistants and %d signals committed, want 30 of each", assistants, signals)
	{
		owner, _ := steeringInputOwner(t, store, sessionID, "s1")
		testassert.Equal(t, owner, "op-2")
	}
	requireSessionCleared(t, h, sessionID)
}

// cancelOnCloseStream cancels the execution context when the assembly closes
// the consumed stream: after consumption but before the result commit, whose
// transaction runs without cancellation.
type cancelOnCloseStream struct {
	*scriptStream
	cancel context.CancelFunc
}

func (s *cancelOnCloseStream) Close() error {
	s.cancel()
	return s.scriptStream.Close()
}

// TestToolResultAdoptsIntoView proves a committed tool result adopts into the
// coordinator view: the next request-source projection carries it as its
// model.RoleTool message, and a second tool effect in the same Operation sees
// the first one committed.
func TestToolResultAdoptsIntoView(t *testing.T) {
	h, _, c, sessionID := newEffectHarness(t, nil)
	publishCalls(t, h, c, sessionID, testToolCall("call-1"), testToolCall("call-2"))
	source := fixtureProjection(h, c)
	immediate := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran " + call.ID}}}
	}
	{
		_, err := h.toolEffect(c, testOpID, effectExecution(nil, immediate), invocationExecution(nil, immediate), testCapture())(context.Background(), testToolCall("call-1"), []string{"echo"})
		testassert.NoError(t, err)
	}
	msgs, err := source(context.Background())
	testassert.NoError(t, err)
	// system, input, assistant, tool result
	testassert.Equal(t, len(msgs), 4)
	tool := msgs[len(msgs)-1]
	testassert.Require(t, tool.Role == model.RoleTool && tool.ToolCallID == "call-1" && tool.TextContent() == "ran call-1", "projected tool message = %+v, want the committed call-1 result", tool)
	{
		_, err := h.toolEffect(c, testOpID, effectExecution(nil, immediate), invocationExecution(nil, immediate), testCapture())(context.Background(), testToolCall("call-2"), []string{"echo"})
		testassert.NoError(t, err)
	}
	msgs, err = source(context.Background())
	testassert.NoError(t, err)
	testassert.Equal(t, len(msgs), 5)
	tool = msgs[len(msgs)-1]
	testassert.Require(t, tool.ToolCallID == "call-2" && tool.TextContent() == "ran call-2", "projected tool message = %+v, want the committed call-2 result", tool)
}

// publishCalls drives one ready model effect publishing the given calls as
// pending, for direct tool-effect fixtures.
func publishCalls(t *testing.T, h *Harness, c *coordinator, sessionID string, calls ...model.ToolCall) {
	t.Helper()
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(calls...), nil
	}
	{
		_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), invocationExecution(modelFn, nil), testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
}

// TestSettleAgentTerminalClassifiesRunError proves the outer settlement
// classifies the run error: a storage-class error performs no compensating
// write and leaves the committed running state for recovery; an
// execution-context error is between-effect cancellation settling the fixed
// interruption detail; anything else settles failure with the error's text.
func TestSettleAgentTerminalClassifiesRunError(t *testing.T) {
	t.Run("storage failure leaves the running state for recovery", func(t *testing.T) {
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(testToolCall("call-1")), nil
		}
		spy := &toolSpy{}
		h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, spy.tool)
		replaces := 0
		store.txHook = func(step string) error {
			if step == "replace_register" {
				replaces++
				if replaces == 4 { // the tool result's Operation register
					return fmt.Errorf("%w: injected storage failure", ErrStorage)
				}
			}
			return nil
		}
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.Require(t, errors.Is(err, ErrStorage), "execute = %v, want the injected storage failure", err)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Equal(t, rec.State.Status, OperationRunning)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		for _, entry := range graph.Entries {
			testassert.Require(t, entry.Settlement == nil, "settlement entry %s published over a storage failure", entry.Envelope.ID)
		}
	})
	t.Run("typed interruption terminal settles the fixed interruption detail", func(t *testing.T) {
		// A cancellation-shaped run error never reaches this settlement: the
		// Engine converts it to the typed interruption terminal (the
		// interruption-shape rows under the effect and gate fixtures cover
		// that conversion), and the settlement consumes exactly the typed
		// result with no run error.
		h, store, c, sessionID := newEffectHarness(t, nil)
		publishCalls(t, h, c, sessionID, testToolCall("call-1"))
		{
			res := agent.TerminalResult{Status: agent.TerminalInterruption, Detail: executionInterruptedDetail}
			err := h.settleAgentTerminal(c, testOpID, res, nil, context.Background())
			testassert.NoError(t, err)
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "operation state = %+v, want terminal interruption with the fixed detail", rec.State)
		{
			_, err := validateFixture(t, store, sessionID)
			testassert.NoError(t, err)
		}
	})
	t.Run("non-storage protocol error still settles failure", func(t *testing.T) {
		h, _, c, sessionID := newEffectHarness(t, nil)
		runErr := errors.New("context source broke")
		{
			err := h.settleAgentTerminal(c, testOpID, agent.TerminalResult{}, runErr, context.Background())
			testassert.Require(t, errors.Is(err, runErr), "settleAgentTerminal = %v, want the run error back", err)
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.State.Status == OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == runErr.Error(), "operation state = %+v, want terminal failure with the error text", rec.State)
	})
	t.Run("boundary token settles the quiet success", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		{
			err := h.settleAgentTerminal(c, testOpID, agent.TerminalResult{}, errSteeringBoundary, context.Background())
			testassert.NoError(t, err)
		}
		rec := settledOperation(t, store, sessionID, testOpID)
		testassert.Require(t, rec.State.Status == OperationSuccess && rec.State.Terminal != nil && rec.State.Terminal.Detail == "", "operation state = %+v, want quiet success with empty detail", rec.State)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		for _, entry := range graph.Entries {
			testassert.Require(t, entry.Signal == nil && (entry.Assistant == nil || entry.Envelope.OperationID != testOpID), "entry %s added a signal or assistant behind the boundary token", entry.Envelope.ID)
		}
	})
	t.Run("shared success guard converts only the matching interruption", func(t *testing.T) {
		rows := []struct {
			name   string
			marker string
			cancel bool
			group  bgState
			want   OperationState
		}{
			{"matching interrupt marker becomes interruption", testOpID, false, bgOpen, OperationInterruption},
			{"canceled execution becomes interruption", "", true, bgOpen, OperationInterruption},
			{"permanently closed group becomes interruption", "", false, bgClosed, OperationInterruption},
			{"foreign interrupt marker keeps success", "op-other", false, bgOpen, OperationSuccess},
			{"root stop's stopping state keeps success", "", false, bgStopping, OperationSuccess},
		}
		for _, tc := range rows {
			t.Run(tc.name, func(t *testing.T) {
				h, store, c, sessionID := newEffectHarness(t, nil)
				c.mu.Lock()
				c.interruptOp = tc.marker
				c.bgState = tc.group
				c.mu.Unlock()
				execCtx := context.Background()
				if tc.cancel {
					canceled, cancel := context.WithCancel(context.Background())
					cancel()
					execCtx = canceled
				}
				{
					err := h.settleAgentTerminal(c, testOpID, agent.TerminalResult{Status: agent.TerminalSuccess}, nil, execCtx)
					testassert.NoError(t, err)
				}
				rec := settledOperation(t, store, sessionID, testOpID)
				testassert.Require(t, rec.State.Status == tc.want, "operation state = %+v, want %s", rec.State, tc.want)
				testassert.Require(t, tc.want != OperationInterruption || rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "terminal = %+v, want the fixed interrupted detail", rec.State.Terminal)
			})
		}
	})
	t.Run("one attempted success publication keeps its original error", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		{
			_, err := submitText(t, h, sessionID, "op-2", MessageModeRegular, "s1")
			testassert.NoError(t, err)
		}
		attempts := 0
		injected := fmt.Errorf("%w: injected publication failure", ErrStorage)
		store.txHook = func(step string) error {
			if step == "insert_entry" {
				attempts++
				return injected
			}
			return nil
		}
		run := &activeExecution{execCtx: context.Background(), cancel: func() {}}
		err := h.finishExecution(c, run, testOpID, PreparedExecution{}, &agent.TerminalResult{Status: agent.TerminalSuccess}, nil, nil, nil)
		testassert.Require(t, errors.Is(err, ErrStorage) && err.Error() == injected.Error(), "finishExecution = %v, want the original injected error", err)
		store.txHook = nil
		testassert.Equal(t, attempts, 1)
		c.mu.Lock()
		held, buffered := c.reserved != nil, len(c.steering)
		c.mu.Unlock()
		testassert.Require(t, !held, "admission reservation stayed held after the completion")
		testassert.Equal(t, buffered, 1)
		testassert.Require(t, !storedOperationExists(store, sessionID, "op-2"), "successor admitted while the current work remains")
		{
			rec := settledOperation(t, store, sessionID, testOpID)
			testassert.Require(t, rec.State.Status == OperationRunning, "operation state = %+v, want the running state left for recovery", rec.State)
		}
	})
	t.Run("acquisition cancellation skips only the drain", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			res     *agent.TerminalResult
			runErr  error
			want    OperationState
			wantSet int
		}{
			{"conversation terminal settles under a canceled acquisition", &agent.TerminalResult{Status: agent.TerminalSuccess}, nil, OperationSuccess, 1},
			{"nil-terminal outcome keeps its original run error", nil, fmt.Errorf("%w: compaction broke", ErrStorage), OperationRunning, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h, store, c, sessionID, _, harnessCancel := newExecutionHarness(t, func(context.Context, model.Request) (model.Stream, error) {
					return completedTurnStream(), nil
				}, func(_ context.Context, call model.ToolCall) PreparedTool {
					return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ok"}}}
				})
				{
					_, err := submitText(t, h, sessionID, "op-2", MessageModeRegular, "s1")
					testassert.NoError(t, err)
				}
				release, err := h.reserve(context.Background(), c) // another holder owns the reservation
				testassert.NoError(t, err)
				defer release()
				harnessCancel() // the Harness context dies: the acquisition is canceled
				closes := 0
				run := &activeExecution{execCtx: context.Background(), cancel: func() {}}
				got := h.finishExecution(c, run, testOpID, PreparedExecution{}, tc.res, tc.runErr, nil, func() error {
					closes++
					return nil
				})
				testassert.Require(t, tc.res != nil || got == tc.runErr, "finishExecution = %v, want the original run error retained", got)
				testassert.Require(t, tc.res == nil || got == nil, "finishExecution = %v, want the settled outcome, never the acquisition error", got)
				testassert.Equal(t, closes, 1)
				c.mu.Lock()
				held, buffered := c.reserved != nil, len(c.steering)
				c.mu.Unlock()
				testassert.Require(t, held, "the other holder's reservation was released")
				testassert.Equal(t, buffered, 1)
				{
					rec := settledOperation(t, store, sessionID, testOpID)
					testassert.Require(t, rec.State.Status == tc.want, "operation state = %+v, want %s", rec.State, tc.want)
				}
				graph, err := validateFixture(t, store, sessionID)
				testassert.NoError(t, err)
				settlements := 0
				for _, entry := range graph.Entries {
					if entry.Settlement != nil && entry.Envelope.OperationID == testOpID {
						settlements++
					}
				}
				testassert.Equal(t, settlements, tc.wantSet)
				h.mu.Lock()
				latched, cleanup := h.storageFailure, h.cleanupFailure
				h.mu.Unlock()
				if tc.res == nil {
					testassert.Equal(t, latched, tc.runErr)
				} else if latched != nil || cleanup != nil {
					t.Fatalf("recorded failures = (%v, %v), want the successful outcome unmodified", latched, cleanup)
				}
			})
		}
	})

	t.Run("rematerialized running state is not settled a second time", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			foreign func(t *testing.T, h *Harness, store *graphStorage, c *coordinator, sessionID string)
		}{
			{"quiet running state", func(t *testing.T, h *Harness, store *graphStorage, c *coordinator, sessionID string) {
				foreignEffectRace(t, store, sessionID, testOpID, "foreign")
			}},
			{"retained active effect", func(t *testing.T, h *Harness, store *graphStorage, c *coordinator, sessionID string) {
				{
					err := store.Transact(context.Background(), func(tx Transaction) error {
						key := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID}
						reg, err := tx.ReadRegister(key)
						if err != nil {
							return err
						}
						op, err := decodeOperationRegister(reg)
						if err != nil {
							return err
						}
						consumedBoundary := ""
						for _, entry := range store.entries[sessionID] { // the transaction callback already holds the fake's lock
							if entry.Kind == EntryInput {
								consumedBoundary = entry.ID
							}
						}
						mdl := testModelRef()
						op.State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(7), RequestID: "request-active", Model: &mdl, Purpose: InvocationPurposeConversation, Evidence: &RequestEvidence{BoundaryEntryID: consumedBoundary}}
						op.State.Request = &RunningRequest{RequestID: "request-active", BoundaryEntryID: consumedBoundary}
						payload, err := encodeOperationRegister(op)
						if err != nil {
							return err
						}
						_, err = tx.ReplaceRegister(key, reg.Revision, payload)
						return err
					})
					testassert.NoError(t, err)
				}
				{
					err := h.rematerialize(context.Background(), c, sessionID)
					testassert.NoError(t, err)
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h, store, c, sessionID := newEffectHarness(t, nil)
				tc.foreign(t, h, store, c, sessionID)
				run := &activeExecution{execCtx: context.Background(), cancel: func() {}}
				err := h.finishExecution(c, run, testOpID, PreparedExecution{}, &agent.TerminalResult{Status: agent.TerminalSuccess}, nil, nil, nil)
				testassert.Require(t, tc.name != "quiet running state" || errors.Is(err, ErrConflict), "finishExecution = %v, want the revision race's original error", err)
				testassert.Require(t, tc.name != "retained active effect" || err == nil, "finishExecution = %v, want the guarded skip's cleared value", err)
				graph, err := validateFixture(t, store, sessionID)
				testassert.NoError(t, err)
				for _, entry := range graph.Entries {
					testassert.Require(t, entry.Settlement == nil || entry.Envelope.OperationID != testOpID, "settlement entry %s published over the rematerialized running state", entry.Envelope.ID)
				}
				{
					rec := settledOperation(t, store, sessionID, testOpID)
					testassert.Require(t, rec.State.Status == OperationRunning, "operation state = %+v, want the running state left for recovery", rec.State)
				}
			})
		}
	})
	t.Run("an already committed settlement is never rewritten", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		var settlementID string
		// one coherent foreign settle
		{
			err := store.Transact(context.Background(), func(tx Transaction) error { // one coherent foreign settle
				skey := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
				sreg, err := tx.ReadRegister(skey)
				if err != nil {
					return err
				}
				session, err := decodeSessionRegister(sreg)
				if err != nil {
					return err
				}
				okey := RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID}
				oreg, err := tx.ReadRegister(okey)
				if err != nil {
					return err
				}
				op, err := decodeOperationRegister(oreg)
				if err != nil {
					return err
				}
				committed, _, _, err := commitTerminalSettlement(tx, sessionID, testOpID, session, op, sreg.Revision, oreg.Revision, OperationSuccess, "", nil, model.ModelRef{}, nil, false, nil, "")
				if err != nil {
					return err
				}
				settlementID = committed.State.Terminal.SettlementEntry.EntryID
				return nil
			})
			testassert.NoError(t, err)
		}
		{
			err := h.rematerialize(context.Background(), c, sessionID)
			testassert.NoError(t, err)
		}
		run := &activeExecution{execCtx: context.Background(), cancel: func() {}}
		{
			err := h.finishExecution(c, run, testOpID, PreparedExecution{}, &agent.TerminalResult{Status: agent.TerminalSuccess}, nil, nil, nil)
			testassert.NoError(t, err)
		}
		rec := settledOperation(t, store, sessionID, testOpID)
		testassert.Require(t, rec.State.Status == OperationSuccess && rec.State.Terminal != nil && rec.State.Terminal.SettlementEntry.EntryID == settlementID, "operation state = %+v, want the committed settlement %s preserved", rec.State, settlementID)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		for _, entry := range graph.Entries {
			testassert.Require(t, entry.Settlement == nil || entry.Envelope.OperationID != testOpID || entry.Envelope.ID == settlementID, "settlement entry %s rewrote the committed settlement", entry.Envelope.ID)
		}
	})
}

// TestToolEffectGateBeforePreparation proves execution cancellation prevents
// later preparation: a context that is already done settles the call's
// interrupted-before-execution result through the ordinary no-intent
// transition without ever invoking the prepared function.
func TestToolEffectGateBeforePreparation(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	publishCalls(t, h, c, sessionID, testToolCall("call-1"), testToolCall("call-2"))
	spy := &toolSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the execution context dies before the tool effect runs
	replaces := 0
	store.txHook = func(step string) error {
		if step == "replace_register" {
			replaces++
		}
		return nil
	}
	got, err := h.toolEffect(c, testOpID, effectExecution(nil, spy.tool), invocationExecution(nil, spy.tool), testCapture())(ctx, testToolCall("call-1"), []string{"echo"})
	testassert.NoError(t, err)
	{
		dispatched := spy.dispatched()
		testassert.Require(t, len(dispatched) == 0, "prepared invoked for %v, want zero preparations", dispatched)
	}
	testassert.Equal(t, got, (model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent}))
	// Operation + Session registers only: no effect intent
	testassert.Equal(t, replaces, 2)
	// The run settles interruption and the terminal helper interrupts the
	// remaining unstarted call.
	{
		err := h.settleAgentTerminal(c, testOpID, agent.TerminalResult{Status: agent.TerminalInterruption, Detail: executionInterruptedDetail}, nil, context.Background())
		testassert.NoError(t, err)
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	byCall := map[string]toolResultEntry{}
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			byCall[entry.ToolResult.ToolCallID] = *entry.ToolResult
		}
	}
	{
		got := byCall["call-2"]
		testassert.Require(t, got.Status == model.ResultInterrupted && got.Content == interruptedToolResultContent, "call-2 result = %+v, want the terminal helper's interrupted result", got)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "operation state = %+v, want terminal interruption", rec.State)
	requireSessionCleared(t, h, sessionID)
}

// TestAssistantEntryOriginalNormalizationCommitsAtProducer proves the
// assistant-entry normalization producer: before the entry commits, each
// advertised completed call runs through the pure callback exactly once — a
// successful owned object populates the nested call's normalized_arguments
// member while the exact raw argument bytes are always retained, a per-call
// validation failure or a malformed (non-object) callback result leaves the
// member absent, and an unadvertised name invokes no callback at all. The
// entry round-trips through the codecs.
func TestAssistantEntryOriginalNormalizationCommitsAtProducer(t *testing.T) {
	valid := json.RawMessage(` {"x": 1} `)      // valid JSON whose bytes are not compact
	malformed := json.RawMessage("\xff{broken") // invalid JSON and invalid UTF-8
	normalized := 0
	normalize := func(call model.ToolCall) (json.RawMessage, error) {
		normalized++
		if call.ID == "call-3" {
			return json.RawMessage(`[1,2]`), nil // not one JSON object
		}
		return objectNormalize(call)
	}
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(
			model.ToolCall{ID: "call-1", Name: "echo", Arguments: valid},
			model.ToolCall{ID: "call-2", Name: "echo", Arguments: malformed},
			model.ToolCall{ID: "call-3", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)},
			model.ToolCall{ID: "call-4", Name: "ghost", Arguments: json.RawMessage(`{"x":1}`)},
		), nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	{
		_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, Execution{}, InvocationExecution{Model: modelFn, NormalizeTool: normalize}, testCapture(), fixtureRound(h, c)), defaultModelRequest(t), nil)
		testassert.NoError(t, err)
	}
	testassert.Equal(t, normalized, 3)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		if entry.Assistant == nil || len(entry.Assistant.ToolCalls) != 4 {
			continue
		}
		calls := entry.Assistant.ToolCalls
		{
			raw, err := base64.StdEncoding.DecodeString(calls[0].ArgumentsBase64)
			testassert.Require(t, err == nil && string(raw) == string(valid), "call-1 raw arguments = %q err %v, want the exact caller bytes", raw, err)
		}
		testassert.Require(t, string(calls[0].NormalizedArguments) == `{"x":1}`, "call-1 normalized arguments = %q, want the successful callback object", calls[0].NormalizedArguments)
		{
			raw, err := base64.StdEncoding.DecodeString(calls[1].ArgumentsBase64)
			testassert.Require(t, err == nil && string(raw) == string(malformed), "call-2 raw arguments = %q err %v, want the exact malformed bytes preserved", raw, err)
		}
		testassert.Require(t, len(calls[1].NormalizedArguments) == 0, "call-2 normalized arguments = %q, want the member absent for a failed normalization", calls[1].NormalizedArguments)
		testassert.Require(t, len(calls[2].NormalizedArguments) == 0, "call-3 normalized arguments = %q, want the member absent for a malformed callback result", calls[2].NormalizedArguments)
		testassert.Require(t, len(calls[3].NormalizedArguments) == 0, "call-4 normalized arguments = %q, want the member absent for an unadvertised name", calls[3].NormalizedArguments)
		return
	}
	t.Fatalf("no assistant entry with four calls committed")
}

// TestModelEffectIntentCancellationSettlesInterruption proves an intent
// transaction aborted by cancellation settles the cancellation outcome: the
// run ends in the fixed terminal interruption, never in a boundary violation.
func TestModelEffectIntentCancellationSettlesInterruption(t *testing.T) {
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil
	}
	spy := &toolSpy{}
	var cancel context.CancelFunc
	h, store, c, sessionID, prepared, harnessCancel := newExecutionHarness(t, modelFn, spy.tool)
	cancel = harnessCancel
	replaces := 0
	store.txHook = func(step string) error {
		if step == "replace_register" {
			replaces++
			if replaces == 1 { // the intent transaction of the first model effect
				cancel()
				return context.Canceled
			}
		}
		return nil
	}
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.NoError(t, err)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "operation state = %+v, want terminal interruption with the fixed detail", rec.State)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		testassert.Require(t, entry.Assistant == nil, "assistant entry %s published after an aborted intent", entry.Envelope.ID)
	}
	requireSessionCleared(t, h, sessionID)
}

// TestToolEffectIntentCancellationSettlesInterrupted proves a tool intent
// transaction aborted by cancellation settles the call's
// interrupted-before-execution result: the batch stops and the terminal helper
// interrupts every remaining unstarted call. Both synthetic results carry no
// metadata member even though the executor plan would have produced
// well-formed bounded metadata.
func TestToolEffectIntentCancellationSettlesInterrupted(t *testing.T) {
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(testToolCall("call-1"), testToolCall("call-2")), nil
	}
	toolFn := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
			return ToolOutcome{
				Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"},
				Metadata: json.RawMessage(`{"preview":true}`),
			}
		}}
	}
	var cancel context.CancelFunc
	h, store, c, sessionID, prepared, harnessCancel := newExecutionHarness(t, modelFn, toolFn)
	cancel = harnessCancel
	replaces := 0
	store.txHook = func(step string) error {
		if step == "replace_register" {
			replaces++
			if replaces == 4 { // the tool effect's intent transaction
				cancel()
				return context.Canceled
			}
		}
		return nil
	}
	{
		err := executeDirect(h, c, h.ctx, testOpID, prepared)
		testassert.NoError(t, err)
	}
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	byCall := map[string]toolResultEntry{}
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			byCall[entry.ToolResult.ToolCallID] = *entry.ToolResult
		}
	}
	{
		got := byCall["call-1"]
		testassert.Require(t, got.Status == model.ResultInterrupted && got.Content == interruptedToolResultContent, "call-1 result = %+v, want the interrupted-before-execution result", got)
	}
	{
		got := byCall["call-2"]
		testassert.Require(t, got.Status == model.ResultInterrupted && got.Content == interruptedToolResultContent, "call-2 result = %+v, want the terminal helper's interrupted result", got)
	}
	for call, got := range byCall {
		testassert.Require(t, len(got.Metadata) == 0, "%s result carries metadata %s, want none on synthetic results", call, got.Metadata)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Status == OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == executionInterruptedDetail, "operation state = %+v, want terminal interruption with the fixed detail", rec.State)
	requireSessionCleared(t, h, sessionID)
}

// TestSessionGraphReplaceOperation pins the cached view's one-record state
// adoption: replaceOperation updates exactly the addressed Operation among
// several, leaves every sibling historical record untouched, is a no-op for
// an identity the view does not carry, and never allocates — the full
// operation index stays the graph validator's duplicate/reference map.
func TestSessionGraphReplaceOperation(t *testing.T) {
	record := func(id string, revision int64) OperationRecord {
		return OperationRecord{
			Admission: OperationAdmission{
				SessionID:   testSessionID,
				OperationID: id,
				RequestKind: RequestKindMessage,
				AgentType:   "coder",
			},
			State: OperationCurrentState{
				Status:    OperationRunning,
				StartedAt: testTime,
				Usage:     UsageTotals{},
			},
			Revision: revision,
		}
	}

	t.Run("replaces only the addressed record", func(t *testing.T) {
		g := &sessionGraph{Operations: []OperationRecord{record("op-1", 1), record("op-2", 2), record("op-3", 3)}}
		testassert.Require(t, g.replaceOperation("op-2", record("op-2", 42)), "replaceOperation of a carried identity = false, want true")
		want := []OperationRecord{record("op-1", 1), record("op-2", 42), record("op-3", 3)}
		testassert.Require(t, reflect.DeepEqual(g.Operations, want), "operations after replacement = %+v, want %+v", g.Operations, want)
	})

	t.Run("missing identity is a no-op", func(t *testing.T) {
		g := &sessionGraph{Operations: []OperationRecord{record("op-1", 1), record("op-2", 2), record("op-3", 3)}}
		testassert.Require(t, !g.replaceOperation("op-ghost", record("op-ghost", 9)), "replaceOperation of an absent identity = true, want false")
		want := []OperationRecord{record("op-1", 1), record("op-2", 2), record("op-3", 3)}
		testassert.Require(t, reflect.DeepEqual(g.Operations, want), "operations after a missing identity = %+v, want the untouched records %+v", g.Operations, want)
	})

	t.Run("replacement allocates nothing", func(t *testing.T) {
		const count = 64
		operations := make([]OperationRecord, count) // all 64 unique records are prepared outside the measured closure
		for i := range operations {
			operations[i] = record(fmt.Sprintf("op-%02d", i), int64(i))
		}
		g := &sessionGraph{Operations: operations}
		updated := record("op-63", 999)
		{
			n := testing.AllocsPerRun(100, func() { g.replaceOperation("op-63", updated) })
			testassert.Require(t, n == 0, "one-record replacement over %d operations allocated %v times, want zero", count, n)
		}
	})
}
