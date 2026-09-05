package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// newEffectHarness admits one running Operation ("op-1") with the given
// prepared model function and returns the pieces the effect fixtures need.
func newEffectHarness(t *testing.T, modelFn agent.ModelEffect) (*Harness, *graphStorage, *coordinator, string) {
	t.Helper()
	store := emptyStore(t)
	prepared := validPrepared()
	if modelFn != nil {
		prepared.Model = modelFn
	}
	h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return prepared, nil
	})
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, disposition := mustAdmitWithoutExecution(t, h, session.Identity.SessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
		t.Fatalf("disposition %q, want admitted", disposition)
	}
	c, err := h.coordinatorFor(context.Background(), session.Identity.SessionID)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return h, store, c, session.Identity.SessionID
}

// modelReturning ignores the assembly callback and returns one fixed
// settlement, the shape of a prepared model function that never assembles.
func modelReturning(set agent.ModelSettlement) agent.ModelEffect {
	return func(context.Context, model.Request, agent.AssemblyCallback) (agent.ModelSettlement, error) {
		return set, nil
	}
}

// modelAssemblingOnce invokes the assembly callback exactly once over a fake
// completed stream, ignoring its outcome, and returns one fixed settlement.
func modelAssemblingOnce(set agent.ModelSettlement) agent.ModelEffect {
	return func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		if err := assembleCompleted(assemble); err != nil {
			return agent.ModelSettlement{}, err
		}
		return set, nil
	}
}

// invokeModelEffect drives one model effect with a validated request.
func invokeModelEffect(t *testing.T, me agent.ModelEffect, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
	t.Helper()
	req, err := model.NewRequest(model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("hello")}},
		Tools:    []model.ToolDefinition{testToolDefinition()},
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return me(context.Background(), req, assemble)
}

// completedOutputWith builds one valid completed output of the expected model.
func completedOutputWith(calls ...model.ToolCall) *model.Output {
	return &model.Output{
		Status: model.OutputCompleted,
		Source: testModelRef(),
		Message: &model.Message{
			Role:      model.RoleAssistant,
			Source:    testModelRef(),
			Content:   admissionContent("done"),
			ToolCalls: calls,
		},
		Usage: &model.Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2},
	}
}

// erroredOutputWith builds one valid errored output retaining a text payload.
func erroredOutputWith() *model.Output {
	return &model.Output{
		Status: model.OutputErrored,
		Source: testModelRef(),
		Message: &model.Message{
			Role:    model.RoleAssistant,
			Source:  testModelRef(),
			Content: admissionContent("partial"),
		},
		Detail: "provider failure",
		Usage:  &model.Usage{InputTokens: 5, OutputTokens: 7},
	}
}

func testToolCall(id string) model.ToolCall {
	return model.ToolCall{ID: id, Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}
}

// requireTerminalFailure asserts the durable shape of one model-effect
// protocol failure: the Operation settled as failure, no model result entry
// was published, and the settlement detail is the error text.
func requireTerminalFailure(t *testing.T, h *Harness, store *graphStorage, sessionID string, wantErr error) {
	t.Helper()
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-failure graph: %v", err)
	}
	for _, entry := range graph.Entries {
		if entry.Assistant != nil {
			t.Fatalf("assistant entry %s published after a callback-protocol failure", entry.Envelope.ID)
		}
		if entry.Signal != nil {
			t.Fatalf("signal entry %s published after a callback-protocol failure", entry.Envelope.ID)
		}
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationFailure {
		t.Fatalf("operation status = %s, want failure", rec.State.Status)
	}
	if rec.State.ActiveEffect != nil {
		t.Fatalf("settled operation keeps an active effect %+v", rec.State.ActiveEffect)
	}
	if rec.State.Terminal == nil || rec.State.Terminal.Detail != wantErr.Error() {
		t.Fatalf("terminal detail = %+v, want the protocol error text %q", rec.State.Terminal, wantErr.Error())
	}
	session, err := h.ReadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if session.State.CurrentOperationID != "" {
		t.Fatalf("current operation %q survived a terminal settlement", session.State.CurrentOperationID)
	}
}

// TestModelEffectCallbackProtocolFailures proves the private assembly-callback
// discipline: a protocol failure behind a settlement is fatal, publishes no
// model result, and settles the Operation as failure through the common
// terminal helper.
func TestModelEffectCallbackProtocolFailures(t *testing.T) {
	cases := []struct {
		name     string
		modelFn  agent.ModelEffect
		assemble agent.AssemblyCallback
	}{
		{
			name:    "output settlement with no assembly callback call",
			modelFn: modelReturning(agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}),
			assemble: func(model.ModelRef, model.Stream) (model.Output, error) {
				return model.Output{}, errors.New("assembly callback must not run")
			},
		},
		{
			name:     "outputless settlement after one assembly callback call",
			modelFn:  modelAssemblingOnce(agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "model failure"}),
			assemble: func(model.ModelRef, model.Stream) (model.Output, error) { return model.Output{}, nil },
		},
		{
			name: "assembly callback error stays fatal when the prepared function ignores it",
			modelFn: func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
				_, _ = assemble(testModelRef(), nil) // the callback failed; the settlement below ignores that
				return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, nil
			},
			assemble: func(model.ModelRef, model.Stream) (model.Output, error) {
				return model.Output{}, errors.New("assembler broke")
			},
		},
		{
			name:     "settlement fails settlement validation",
			modelFn:  modelReturning(agent.ModelSettlement{Disposition: agent.DispoFailure, Output: completedOutputWith()}),
			assemble: func(model.ModelRef, model.Stream) (model.Output, error) { return model.Output{}, nil },
		},
		{
			name: "repeated assembly callback behind an output settlement",
			modelFn: func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
				_, _ = assemble(testModelRef(), nil)
				_, _ = assemble(testModelRef(), nil)
				return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, nil
			},
			assemble: func(model.ModelRef, model.Stream) (model.Output, error) { return model.Output{}, nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, c, sessionID := newEffectHarness(t, tc.modelFn)
			me := h.modelEffect(c, testOpID, tc.modelFn)
			set, err := invokeModelEffect(t, me, tc.assemble)
			if err == nil {
				t.Fatalf("model effect settled %+v, want a protocol error", set)
			}
			requireTerminalFailure(t, h, store, sessionID, err)
		})
	}
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
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		reg, err := store.ReadRegister(ctx, RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID})
		if err != nil {
			return agent.ModelSettlement{}, err
		}
		op, err := decodeOperationRegister(reg)
		if err != nil {
			return agent.ModelSettlement{}, err
		}
		if op.State.ActiveEffect == nil || op.State.ActiveEffect.Kind != EffectModel {
			return agent.ModelSettlement{}, errors.New("no model effect intent is durable while the effect runs")
		}
		reservedID = op.State.ActiveEffect.ResultEntryID
		if _, err := assemble(testModelRef(), nil); err != nil {
			return agent.ModelSettlement{}, err
		}
		return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith(testToolCall("call-1"), testToolCall("call-2"))}, nil
	}
	h, st, c, sid := newEffectHarness(t, modelFn)
	store, sessionID = st, sid

	calls := 0
	me := h.modelEffect(c, testOpID, modelFn)
	set, err := invokeModelEffect(t, me, func(model.ModelRef, model.Stream) (model.Output, error) {
		calls++
		return model.Output{Status: model.OutputCompleted, Source: testModelRef()}, nil
	})
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if set.Disposition != agent.DispoReady {
		t.Fatalf("settlement disposition %q, want ready", set.Disposition)
	}
	if calls != 1 {
		t.Fatalf("assembly callback calls = %d, want exactly one", calls)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-ready graph: %v", err)
	}
	var assistant *assistantEntry
	for i := range graph.Entries {
		if graph.Entries[i].Assistant != nil {
			assistant = graph.Entries[i].Assistant
		}
	}
	if assistant == nil {
		t.Fatalf("ready settlement published no assistant entry")
	}
	if assistant.EntryID != reservedID {
		t.Fatalf("assistant entry %s does not consume the reserved identity %s", assistant.EntryID, reservedID)
	}
	if assistant.Status != model.OutputCompleted || len(assistant.ToolCalls) != 2 {
		t.Fatalf("assistant entry = %+v, want completed output with two calls", assistant)
	}
	if assistant.Usage == nil || *assistant.Usage != (UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}) {
		t.Fatalf("assistant usage = %+v, want the reported counts", assistant.Usage)
	}
	for i, call := range assistant.ToolCalls {
		if call.Ordinal != int64(i) || call.ID != fmt.Sprintf("call-%d", i+1) || call.ResultEntryID == "" {
			t.Fatalf("tool_calls[%d] = %+v, want ordered reserved call", i, call)
		}
		if raw, derr := base64.StdEncoding.DecodeString(call.ArgumentsBase64); derr != nil || string(raw) != `{"x":1}` {
			t.Fatalf("tool_calls[%d] arguments = %q (%v), want the exact raw bytes", i, raw, derr)
		}
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect != nil {
		t.Fatalf("operation state = %+v, want running with the effect cleared", rec.State)
	}
	if len(rec.State.PendingToolCalls) != 2 {
		t.Fatalf("pending calls = %+v, want the two published calls", rec.State.PendingToolCalls)
	}
	for i, call := range rec.State.PendingToolCalls {
		if call.CallID != assistant.ToolCalls[i].ID || call.ResultEntryID != assistant.ToolCalls[i].ResultEntryID ||
			call.AssistantEntry.EntryID != assistant.EntryID {
			t.Fatalf("pending call %d = %+v, want the assistant's reservation", i, call)
		}
	}
	want := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}}}}
	if !usageTotalsEqual(rec.State.Usage, want) {
		t.Fatalf("operation usage = %+v, want %+v", rec.State.Usage, want)
	}
	session, err := h.ReadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if session.State.CurrentOperationID != testOpID {
		t.Fatalf("current operation %q, want the still-running %q", session.State.CurrentOperationID, testOpID)
	}
	if !usageTotalsEqual(session.State.Usage, want) {
		t.Fatalf("session usage = %+v, want %+v", session.State.Usage, want)
	}

	// The second context projection reads the entry the first effect committed.
	source := h.contextSource(c, testOpID)
	msgs, err := source(context.Background())
	if err != nil {
		t.Fatalf("second context projection: %v", err)
	}
	if len(msgs) != 3 { // system prompt, admitted input, committed assistant
		t.Fatalf("second projection carried %d messages, want 3", len(msgs))
	}
	last := msgs[len(msgs)-1]
	if last.Role != model.RoleAssistant || last.Source != testModelRef() || last.TextContent() != "done" || len(last.ToolCalls) != 2 {
		t.Fatalf("second projection's last message = %+v, want the committed assistant", last)
	}
	if string(last.ToolCalls[0].Arguments) != `{"x":1}` {
		t.Fatalf("projected call arguments = %s, want the exact raw bytes", last.ToolCalls[0].Arguments)
	}
}

// TestModelEffectContinueRemainsRunning proves the continuation transition:
// the retained partial assistant commits with the fixed continuation signal
// and the Operation stays running.
func TestModelEffectContinueRemainsRunning(t *testing.T) {
	modelFn := modelAssemblingOnce(agent.ModelSettlement{Disposition: agent.DispoContinue, Output: erroredOutputWith()})
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), func(model.ModelRef, model.Stream) (model.Output, error) {
		return model.Output{}, nil
	})
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if set.Disposition != agent.DispoContinue {
		t.Fatalf("settlement disposition %q, want continue", set.Disposition)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-continue graph: %v", err)
	}
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
	if assistant == nil || assistant.Status != model.OutputErrored || len(assistant.Content) != 1 || assistant.Content[0].Text != "partial" {
		t.Fatalf("assistant entry = %+v, want the retained errored partial", assistant)
	}
	if signal == nil || signal.Signal != SignalModelFailureContinuation || signal.Content != signalModelFailureContinuationContent {
		t.Fatalf("signal entry = %+v, want the fixed continuation signal", signal)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect != nil || len(rec.State.PendingToolCalls) != 0 {
		t.Fatalf("operation state = %+v, want running with the effect cleared and no pending calls", rec.State)
	}
	want := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 5, OutputTokens: 7}}}}
	if !usageTotalsEqual(rec.State.Usage, want) {
		t.Fatalf("operation usage = %+v, want %+v", rec.State.Usage, want)
	}
	session, err := h.ReadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if session.State.CurrentOperationID != testOpID || !usageTotalsEqual(session.State.Usage, want) {
		t.Fatalf("session state = %+v, want the running operation and updated usage", session.State)
	}
}

// TestModelEffectTerminalSettlement proves the model-originated failure and
// interruption dispositions: each terminally settles in the one result
// transaction, preserving any produced assistant payload, interrupting every
// unstarted call of a completed output, and consuming the effect's reserved
// identity when no assistant payload exists.
func TestModelEffectTerminalSettlement(t *testing.T) {
	t.Run("failure retains the partial assistant", func(t *testing.T) {
		var (
			store      *graphStorage
			sessionID  string
			reservedID string
		)
		modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
			reservedID = readReservedIntent(t, store, sessionID)
			_, _ = assemble(testModelRef(), nil)
			return agent.ModelSettlement{Disposition: agent.DispoFailure, Output: erroredOutputWith(), Detail: "model failure"}, nil
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble)
		if err != nil {
			t.Fatalf("model effect: %v", err)
		}
		if set.Disposition != agent.DispoFailure {
			t.Fatalf("settlement disposition %q, want failure", set.Disposition)
		}
		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("post-failure graph: %v", err)
		}
		var assistant *assistantEntry
		for i := range graph.Entries {
			if graph.Entries[i].Assistant != nil {
				assistant = graph.Entries[i].Assistant
			}
		}
		if assistant == nil || assistant.EntryID != reservedID {
			t.Fatalf("assistant entry %v does not consume the reserved identity %s", assistant, reservedID)
		}
		requireSessionCleared(t, h, sessionID)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.State.Status != OperationFailure || rec.State.Terminal == nil || rec.State.Terminal.Detail != "model failure" {
			t.Fatalf("operation state = %+v, want terminal failure with the settlement detail", rec.State)
		}
	})

	t.Run("failure without an assistant payload settles under the reserved identity", func(t *testing.T) {
		var (
			store      *graphStorage
			sessionID  string
			reservedID string
		)
		modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
			reservedID = readReservedIntent(t, store, sessionID)
			return agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "model failure"}, nil
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble); err != nil {
			t.Fatalf("model effect: %v", err)
		}
		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("post-failure graph: %v", err)
		}
		for _, entry := range graph.Entries {
			if entry.Assistant != nil {
				t.Fatalf("assistant entry %s published for a payload-less failure", entry.Envelope.ID)
			}
			if entry.Settlement != nil && entry.Envelope.ID != reservedID {
				t.Fatalf("settlement entry %s does not consume the reserved identity %s", entry.Envelope.ID, reservedID)
			}
		}
		requireSessionCleared(t, h, sessionID)
	})

	t.Run("interruption of a completed output with calls", func(t *testing.T) {
		var (
			store      *graphStorage
			sessionID  string
			reservedID string
		)
		modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
			reservedID = readReservedIntent(t, store, sessionID)
			_, _ = assemble(testModelRef(), nil)
			return agent.ModelSettlement{Disposition: agent.DispoInterruption, Output: completedOutputWith(testToolCall("call-1"), testToolCall("call-2")), Detail: "stopped mid-run"}, nil
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble)
		if err != nil {
			t.Fatalf("model effect: %v", err)
		}
		if set.Disposition != agent.DispoInterruption {
			t.Fatalf("settlement disposition %q, want interruption", set.Disposition)
		}
		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("post-interruption graph: %v", err)
		}
		var assistant *assistantEntry
		results, signals, settlements := 0, 0, 0
		for i := range graph.Entries {
			entry := graph.Entries[i]
			switch {
			case entry.Assistant != nil:
				assistant = entry.Assistant
			case entry.ToolResult != nil:
				results++
				if entry.ToolResult.Status != model.ResultInterrupted || entry.ToolResult.Content != interruptedToolResultContent {
					t.Fatalf("tool result = %+v, want the fixed interrupted result", entry.ToolResult)
				}
			case entry.Signal != nil:
				signals++
				if entry.Signal.Signal != SignalInterruption || entry.Signal.Content != signalInterruptionContent {
					t.Fatalf("signal = %+v, want the fixed interruption signal", entry.Signal)
				}
			case entry.Settlement != nil:
				settlements++
				if entry.Envelope.ID == reservedID {
					t.Fatalf("settlement entry %s reused the identity consumed by the assistant", entry.Envelope.ID)
				}
			}
		}
		if assistant == nil || assistant.EntryID != reservedID || len(assistant.ToolCalls) != 2 {
			t.Fatalf("assistant entry %v does not consume %s with two calls", assistant, reservedID)
		}
		if results != 2 || signals != 1 || settlements != 1 {
			t.Fatalf("committed %d interrupted results, %d signals, %d settlements, want 2/1/1", results, signals, settlements)
		}
		requireSessionCleared(t, h, sessionID)
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != "stopped mid-run" {
			t.Fatalf("operation state = %+v, want terminal interruption", rec.State)
		}
		if len(rec.State.PendingToolCalls) != 0 || rec.State.ActiveEffect != nil {
			t.Fatalf("operation state = %+v, want no pending calls or active effect", rec.State)
		}
	})

	t.Run("interruption without output settles under the reserved identity", func(t *testing.T) {
		var (
			store      *graphStorage
			sessionID  string
			reservedID string
		)
		modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
			reservedID = readReservedIntent(t, store, sessionID)
			return agent.ModelSettlement{Disposition: agent.DispoInterruption, Detail: "stopped"}, nil
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble); err != nil {
			t.Fatalf("model effect: %v", err)
		}
		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("post-interruption graph: %v", err)
		}
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
				if entry.Envelope.ID != reservedID {
					t.Fatalf("settlement entry %s does not consume the reserved identity %s", entry.Envelope.ID, reservedID)
				}
			}
		}
		if signals != 1 || settlements != 1 {
			t.Fatalf("committed %d signals and %d settlements, want one of each", signals, settlements)
		}
		requireSessionCleared(t, h, sessionID)
	})
}

// TestModelEffectRepeatedCallIDSettlesFailure proves one Operation never
// publishes one tool call identity twice: a second effect repeating an
// already-published call ID is a protocol violation that settles terminal
// failure instead of committing a corrupt graph.
func TestModelEffectRepeatedCallIDSettlesFailure(t *testing.T) {
	settlements := []agent.ModelSettlement{
		{Disposition: agent.DispoReady, Output: completedOutputWith(testToolCall("call-1"))},
		{Disposition: agent.DispoReady, Output: completedOutputWith(testToolCall("call-1"))},
	}
	turns := 0
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		_, _ = assemble(testModelRef(), nil)
		set := settlements[turns]
		turns++
		return set, nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	me := h.modelEffect(c, testOpID, modelFn)
	if _, err := invokeModelEffect(t, me, noopAssemble); err != nil {
		t.Fatalf("first effect: %v", err)
	}
	_, err := invokeModelEffect(t, me, noopAssemble)
	if err == nil {
		t.Fatalf("second effect settled, want the repeated-call protocol violation")
	}
	wantErr := err
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the repeated-call failure: %v", err)
	}
	assistants := 0
	for _, entry := range graph.Entries {
		if entry.Assistant != nil {
			assistants++
		}
	}
	if assistants != 1 {
		t.Fatalf("%d assistant entries after the repeated-call failure, want only turn 1's", assistants)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationFailure || rec.State.Terminal == nil || rec.State.Terminal.Detail != wantErr.Error() {
		t.Fatalf("operation state = %+v, want terminal failure with the protocol error detail", rec.State)
	}
	requireSessionCleared(t, h, sessionID)
}

// TestModelEffectTerminalNoOutputUsageOnSettlement proves a terminal output
// that reported usage without an eligible payload lands that usage on the
// settlement entry and both totals.
func TestModelEffectTerminalNoOutputUsageOnSettlement(t *testing.T) {
	payloadless := &model.Output{
		Status: model.OutputErrored,
		Source: testModelRef(),
		Detail: "empty failure",
		Usage:  &model.Usage{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 6},
	}
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		_, _ = assemble(testModelRef(), nil)
		return agent.ModelSettlement{Disposition: agent.DispoFailure, Output: payloadless, Detail: "model failure"}, nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble); err != nil {
		t.Fatalf("model effect: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-failure graph: %v", err)
	}
	for _, entry := range graph.Entries {
		if entry.Settlement == nil {
			continue
		}
		if entry.Settlement.Model == nil || *entry.Settlement.Model != testModelRef() {
			t.Fatalf("settlement model = %+v, want the expected identity beside its usage", entry.Settlement.Model)
		}
		if entry.Settlement.Usage == nil || *entry.Settlement.Usage != (UsageCount{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 6}) {
			t.Fatalf("settlement usage = %+v, want the reported no-output counts", entry.Settlement.Usage)
		}
	}
	want := UsageTotals{ByModel: []ModelUsage{{Model: testModelRef(), Usage: UsageCount{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 6}}}}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if !usageTotalsEqual(rec.State.Usage, want) {
		t.Fatalf("operation usage = %+v, want %+v", rec.State.Usage, want)
	}
	session, err := h.ReadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if !usageTotalsEqual(session.State.Usage, want) {
		t.Fatalf("session usage = %+v, want %+v", session.State.Usage, want)
	}
}

// noopAssemble is an assembly callback that must never run; only the
// outputless, call-free settlements use it.
func noopAssemble(model.ModelRef, model.Stream) (model.Output, error) {
	return model.Output{}, nil
}

// readReservedIntent reads the active model effect's reserved result identity
// from durable state. It runs inside a prepared model function, between the
// committed intent and the result transaction.
func readReservedIntent(t *testing.T, store *graphStorage, sessionID string) string {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: testOpID})
	if err != nil {
		t.Fatalf("read operation register mid-effect: %v", err)
	}
	op, err := decodeOperationRegister(reg)
	if err != nil {
		t.Fatalf("decode operation register mid-effect: %v", err)
	}
	if op.State.ActiveEffect == nil || op.State.ActiveEffect.Kind != EffectModel {
		t.Fatalf("no model effect intent is durable while the effect runs: %+v", op.State)
	}
	return op.State.ActiveEffect.ResultEntryID
}

// requireSessionCleared asserts the Session register no longer names a
// current Operation after its terminal settlement.
func requireSessionCleared(t *testing.T, h *Harness, sessionID string) {
	t.Helper()
	session, err := h.ReadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if session.State.CurrentOperationID != "" {
		t.Fatalf("current operation %q survived a terminal settlement", session.State.CurrentOperationID)
	}
}

// TestModelEffectPublicationFailureLeavesIntentState proves the result
// transaction's atomicity: an injected failure at any producer step rolls the
// whole settlement back, leaving the committed running/intent state for
// recovery and publishing nothing.
func TestModelEffectPublicationFailureLeavesIntentState(t *testing.T) {
	for _, fail := range []struct {
		step string
		nth  int
	}{
		{"insert_entry", 1},     // the assistant entry
		{"insert_entry", 3},     // the second interrupted tool result
		{"insert_entry", 5},     // the settlement entry
		{"replace_register", 2}, // the result transaction's Operation register
		{"replace_register", 3}, // the result transaction's Session register
	} {
		t.Run(fmt.Sprintf("publication failure at %s #%d", fail.step, fail.nth), func(t *testing.T) {
			modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
				_, _ = assemble(testModelRef(), nil)
				return agent.ModelSettlement{Disposition: agent.DispoInterruption, Output: completedOutputWith(testToolCall("call-1"), testToolCall("call-2")), Detail: "stopped"}, nil
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
			if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble); err == nil {
				t.Fatalf("model effect succeeded past an injected %s failure", fail.step)
			}
			graph, err := validateFixture(t, store, sessionID)
			if err != nil {
				t.Fatalf("graph after rollback: %v", err)
			}
			if len(graph.Entries) != 1 { // only the admitted input entry
				t.Fatalf("%d entries committed after rollback, want only the admitted input", len(graph.Entries))
			}
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			if err != nil {
				t.Fatalf("ReadOperation: %v", err)
			}
			if rec.State.Status != OperationRunning || rec.State.ActiveEffect == nil {
				t.Fatalf("operation state = %+v, want the committed running/intent state", rec.State)
			}
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

// TestContextSourceProjectsFullHistory proves the one kind-to-message mapping
// over the full committed history plus the captured system prompt.
func TestContextSourceProjectsFullHistory(t *testing.T) {
	store := projectionTestGraph().storage(t)
	h := newTestHarness(t, store, nil)
	c, err := h.coordinatorFor(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	msgs, err := h.contextSource(c, testOpID)(context.Background())
	if err != nil {
		t.Fatalf("context projection: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("projection carried %d messages, want 5", len(msgs))
	}
	if msgs[0].Role != model.RoleSystem || msgs[0].TextContent() != "system" {
		t.Fatalf("first message = %+v, want the captured system prompt", msgs[0])
	}
	if msgs[1].Role != model.RoleUser || msgs[1].TextContent() != "hello" {
		t.Fatalf("input message = %+v, want the admitted user input", msgs[1])
	}
	assistant := msgs[2]
	if assistant.Role != model.RoleAssistant || assistant.Source != testModelRef() || assistant.TextContent() != "hi" {
		t.Fatalf("assistant message = %+v, want the committed assistant payload", assistant)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call-1" || string(assistant.ToolCalls[0].Arguments) != `{"x":1}` {
		t.Fatalf("assistant call = %+v, want the call with its exact raw argument bytes", assistant.ToolCalls)
	}
	if msgs[3].Role != model.RoleTool || msgs[3].ToolCallID != "call-1" || msgs[3].TextContent() != "done" {
		t.Fatalf("tool message = %+v, want the tool result under its call identity", msgs[3])
	}
	want := "<system-signal>" + signalInterruptionContent + "</system-signal>"
	if msgs[4].Role != model.RoleUser || msgs[4].TextContent() != want {
		t.Fatalf("signal message = %+v, want the wrapped signal text %q", msgs[4], want)
	}
}

// TestSignalProjectionEscaping proves the signal wrapper escapes &, < and >
// in the signal content.
func TestSignalProjectionEscaping(t *testing.T) {
	got := signalProjectedText(`a & b < c > d`)
	if got != "<system-signal>a &amp; b &lt; c &gt; d</system-signal>" {
		t.Fatalf("signalProjectedText = %q, want the escaped wrapper", got)
	}
}

// TestSteeringInputHelper proves the steering-input producer commits one
// Operation-owned input entry, preserves the item's own submission origin,
// and advances last activity to its commit time.
func TestSteeringInputHelper(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	before, err := h.ReadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if err := h.commitSteeringInput(context.Background(), c, testOpID, InputOriginUser, admissionContent("steering")); err != nil {
		t.Fatalf("commitSteeringInput: %v", err)
	}
	if err := h.commitSteeringInput(context.Background(), c, testOpID, InputOriginRuntime, admissionContent("steered-by-runtime")); err != nil {
		t.Fatalf("commitSteeringInput with a non-user origin: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-steering graph: %v", err)
	}
	var user, runtime *inputEntry
	for i := range graph.Entries {
		if graph.Entries[i].Input == nil || graph.Entries[i].Envelope.OperationID != testOpID {
			continue
		}
		switch graph.Entries[i].Input.Content[0].Text {
		case "steering":
			user = graph.Entries[i].Input
		case "steered-by-runtime":
			runtime = graph.Entries[i].Input
		}
	}
	if user == nil || user.Origin != InputOriginUser {
		t.Fatalf("user steering entry not committed with its own origin: %+v", user)
	}
	if runtime == nil || runtime.Origin != InputOriginRuntime {
		t.Fatalf("runtime steering entry did not preserve its submission origin: %+v", runtime)
	}
	after, err := h.ReadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if after.Identity.SessionID != sessionID {
		t.Fatalf("session identity changed")
	}
	if !after.State.LastActivity.After(before.State.LastActivity) {
		t.Fatalf("last activity = %v, want it advanced to the steering commit time", after.State.LastActivity)
	}
	if after.State.CurrentOperationID != testOpID {
		t.Fatalf("current operation = %q, want the running operation preserved", after.State.CurrentOperationID)
	}
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
	if err != nil {
		t.Fatalf("foreign race write: %v", err)
	}
}

// TestEffectTransactionsRematerializeOnRevisionRace proves every effect
// transaction's revision guard mirrors the landed ChangeAgentType pattern: the
// race error returns and the coordinator rematerializes, so the next read
// observes the foreign state instead of the stale view.
func TestEffectTransactionsRematerializeOnRevisionRace(t *testing.T) {
	t.Run("intent transaction", func(t *testing.T) {
		modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
			_, _ = assemble(testModelRef(), nil)
			return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, nil
		}
		h, store, c, sessionID := newEffectHarness(t, modelFn)
		foreignEffectRace(t, store, sessionID, testOpID, "foreign")
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble); !errors.Is(err, ErrConflict) {
			t.Fatalf("intent over a foreign revision = %v, want the revision-race conflict", err)
		}
		if session, err := h.ReadSession(context.Background(), sessionID); err != nil || session.State.CurrentAgentType != "foreign" {
			t.Fatalf("session after the race = %+v (%v), want the foreign agent type", session, err)
		}
	})
	t.Run("result transaction", func(t *testing.T) {
		var (
			store     *graphStorage
			sessionID string
		)
		modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
			foreignEffectRace(t, store, sessionID, testOpID, "foreign") // between the committed intent and the result transaction
			_, _ = assemble(testModelRef(), nil)
			return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, nil
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble); !errors.Is(err, ErrConflict) {
			t.Fatalf("result over a foreign revision = %v, want the revision-race conflict", err)
		}
		if session, err := h.ReadSession(context.Background(), sessionID); err != nil || session.State.CurrentAgentType != "foreign" {
			t.Fatalf("session after the race = %+v (%v), want the foreign agent type", session, err)
		}
	})
	t.Run("steering transaction", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		foreignEffectRace(t, store, sessionID, testOpID, "foreign")
		if err := h.commitSteeringInput(context.Background(), c, testOpID, InputOriginUser, admissionContent("steering")); !errors.Is(err, ErrConflict) {
			t.Fatalf("steering over a foreign revision = %v, want the revision-race conflict", err)
		}
		if session, err := h.ReadSession(context.Background(), sessionID); err != nil || session.State.CurrentAgentType != "foreign" {
			t.Fatalf("session after the race = %+v (%v), want the foreign agent type", session, err)
		}
	})
}

// TestModelEffectCompletedContinueSettlesFailure proves the plan's rule that
// only the wrapper's own steering decision may produce a completed-output
// continue: a prepared callback returning that combination settles terminal
// failure and publishes no model result.
func TestModelEffectCompletedContinueSettlesFailure(t *testing.T) {
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		_, _ = assemble(testModelRef(), nil)
		return agent.ModelSettlement{Disposition: agent.DispoContinue, Output: completedOutputWith()}, nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	_, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble)
	if err == nil {
		t.Fatalf("completed-output continue settled, want a protocol error")
	}
	requireTerminalFailure(t, h, store, sessionID, err)
}

// archiveSettledSession moves one open Session with no running Operation to
// archived consistently across the coordinator view and storage: a genuinely
// archived Session with a running Operation is graph-invalid and cannot be
// produced through storage, so the fixture writes both sides at one revision.
func archiveSettledSession(t *testing.T, c *coordinator, store *graphStorage, sessionID string) {
	t.Helper()
	stamped := testTime
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
		state.Lifecycle = LifecycleArchived
		state.ArchivedAt = &stamped
		archived := SessionRecord{Identity: current.Identity, State: state}
		payload, err := encodeSessionRegister(archived)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
		if err != nil {
			return err
		}
		archived.Revision = replaced.Revision
		c.mu.Lock()
		c.graph.Session = archived
		c.mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("archive fixture: %v", err)
	}
}

// TestSteeringInputPreconditions proves the steering transition's
// in-transaction preconditions: an open Session whose current Operation is the
// steering target.
func TestSteeringInputPreconditions(t *testing.T) {
	t.Run("steering after terminal settlement", func(t *testing.T) {
		modelFn := modelReturning(agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "model failure"})
		h, store, c, sessionID := newEffectHarness(t, modelFn)
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble); err != nil {
			t.Fatalf("terminal effect: %v", err)
		}
		before, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("graph before steering: %v", err)
		}
		if err := h.commitSteeringInput(context.Background(), c, testOpID, InputOriginUser, admissionContent("steering")); !errors.Is(err, ErrInvalid) {
			t.Fatalf("steering after terminal settlement = %v, want ErrInvalid", err)
		}
		after, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("graph after refused steering: %v", err)
		}
		if len(after.Entries) != len(before.Entries) {
			t.Fatalf("%d entries after refused steering, want the unchanged %d", len(after.Entries), len(before.Entries))
		}
	})
	t.Run("steering on an archived session", func(t *testing.T) {
		modelFn := modelReturning(agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "model failure"})
		h, store, c, sessionID := newEffectHarness(t, modelFn)
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble); err != nil {
			t.Fatalf("terminal effect: %v", err)
		}
		archiveSettledSession(t, c, store, sessionID)
		if err := h.commitSteeringInput(context.Background(), c, testOpID, InputOriginUser, admissionContent("steering")); !errors.Is(err, ErrInvalid) {
			t.Fatalf("steering on an archived session = %v, want ErrInvalid", err)
		}
		after, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("graph after refused steering: %v", err)
		}
		for _, entry := range after.Entries {
			if entry.Input != nil && entry.Input.Content[0].Text == "steering" {
				t.Fatalf("steering entry %s committed on an archived session", entry.Envelope.ID)
			}
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
		op.State.PendingToolCalls = []PendingToolCall{}
		op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: sessionID, EntryID: hexID(9)}, Detail: "foreign"}
		payload, err := encodeOperationRegister(op)
		if err != nil {
			return err
		}
		_, err = tx.ReplaceRegister(key, reg.Revision, payload)
		return err
	})
	if err != nil {
		t.Fatalf("foreign terminal settle: %v", err)
	}
}

// foreignArchive wraps the landed foreign-archive writer in one transaction.
func foreignArchive(t *testing.T, store *graphStorage, sessionID string) {
	t.Helper()
	if err := store.Transact(context.Background(), func(tx Transaction) error {
		return foreignArchiveChange(tx, sessionID)
	}); err != nil {
		t.Fatalf("foreign archive: %v", err)
	}
}

// TestEffectTransactionsPreconditionsOutrankRevisionRace proves the mismatch
// path of every effect transaction mirrors publishAdmission: a freshly decoded
// register showing the semantic precondition violated returns ErrInvalid; only
// a clean mismatch returns the revision-race conflict.
func TestEffectTransactionsPreconditionsOutrankRevisionRace(t *testing.T) {
	t.Run("intent over a foreign terminal operation", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		foreignTerminalSettle(t, store, sessionID, testOpID)
		me := h.modelEffect(c, testOpID, modelReturning(agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}))
		if _, err := invokeModelEffect(t, me, noopAssemble); !errors.Is(err, ErrInvalid) {
			t.Fatalf("intent over a foreign terminal operation = %v, want ErrInvalid", err)
		}
	})
	t.Run("result over a foreign terminal operation", func(t *testing.T) {
		var (
			store     *graphStorage
			sessionID string
		)
		modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
			foreignTerminalSettle(t, store, sessionID, testOpID) // between the committed intent and the result transaction
			_, _ = assemble(testModelRef(), nil)
			return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, nil
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), noopAssemble); !errors.Is(err, ErrInvalid) {
			t.Fatalf("result over a foreign terminal operation = %v, want ErrInvalid", err)
		}
	})
	t.Run("steering over a foreign archive", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		foreignArchive(t, store, sessionID)
		if err := h.commitSteeringInput(context.Background(), c, testOpID, InputOriginUser, admissionContent("steering")); !errors.Is(err, ErrInvalid) {
			t.Fatalf("steering over a foreign archive = %v, want ErrInvalid", err)
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
	return PreparedTool{Immediate: &model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}
}

func (s *toolSpy) dispatched() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.order...)
}

// newExecutionHarness admits one running Operation ("op-1") under a cancelable
// Harness context with both prepared effect functions, returning the pieces
// the execution fixtures need.
func newExecutionHarness(t *testing.T, modelFn agent.ModelEffect, toolFn func(context.Context, model.ToolCall) PreparedTool) (*Harness, *graphStorage, *coordinator, string, PreparedExecution, context.CancelFunc) {
	t.Helper()
	if toolFn == nil {
		t.Fatalf("execution fixtures require a prepared tool function")
	}
	store := emptyStore(t)
	prepared := PreparedExecution{Capture: testCapture(), Model: modelFn, Tool: toolFn}
	hctx, cancel := context.WithCancel(context.Background())
	h, err := New(hctx, Dependencies{Storage: store, Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return prepared, nil
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, disposition := mustAdmitWithoutExecution(t, h, session.Identity.SessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
		t.Fatalf("disposition %q, want admitted", disposition)
	}
	c, err := h.coordinatorFor(context.Background(), session.Identity.SessionID)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return h, store, c, session.Identity.SessionID, prepared, cancel
}

// invokeModelEffectCtx drives one model effect with an explicit context.
func invokeModelEffectCtx(t *testing.T, me agent.ModelEffect, ctx context.Context, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
	t.Helper()
	req, err := model.NewRequest(model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("hello")}},
		Tools:    []model.ToolDefinition{testToolDefinition()},
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return me(ctx, req, assemble)
}

// TestModelEffectGateSkipsCallbackOnCancellation proves execution cancellation
// stops new callbacks: a context dying between the committed intent and the
// prepared callback settles the Operation as terminal interruption without
// ever starting the callback.
func TestModelEffectGateSkipsCallbackOnCancellation(t *testing.T) {
	invoked := 0
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		invoked++
		_, _ = assemble(testModelRef(), nil)
		return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	ctx, cancel := context.WithCancel(context.Background())
	store.txHook = func(step string) error {
		if step == "replace_register" {
			cancel() // the execution context dies between the committed intent and the callback
		}
		return nil
	}
	me := h.modelEffect(c, testOpID, modelFn)
	set, err := invokeModelEffectCtx(t, me, ctx, func(model.ModelRef, model.Stream) (model.Output, error) {
		return model.Output{}, nil
	})
	if err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if invoked != 0 {
		t.Fatalf("prepared callback invoked %d times after cancellation, want zero", invoked)
	}
	if set.Disposition != agent.DispoInterruption || set.Detail != executionInterruptedDetail {
		t.Fatalf("settlement = %+v, want the committed interruption settlement", set)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
		t.Fatalf("operation state = %+v, want terminal interruption", rec.State)
	}
	requireSessionCleared(t, h, sessionID)
	if _, err := validateFixture(t, store, sessionID); err != nil {
		t.Fatalf("graph after the gate: %v", err)
	}
}

// TestExecuteSuccessSettlesOuterTerminal proves the private agent.Run
// composition and the outer terminal settlement: a clean run settles success
// through the common terminal helper with no detail.
func TestExecuteSuccessSettlesOuterTerminal(t *testing.T) {
	modelFn := modelAssemblingOnce(agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()})
	spy := &toolSpy{}
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, spy.tool)
	if err := h.execute(c, testOpID, prepared); err != nil {
		t.Fatalf("execute: %v", err)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationSuccess || rec.State.Terminal == nil || rec.State.Terminal.Detail != "" {
		t.Fatalf("operation state = %+v, want terminal success without detail", rec.State)
	}
	requireSessionCleared(t, h, sessionID)
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after success: %v", err)
	}
	settlements := 0
	for _, entry := range graph.Entries {
		if entry.Settlement != nil {
			settlements++
			if entry.Settlement.Status != OperationSuccess || entry.Settlement.Detail != "" {
				t.Fatalf("settlement = %+v, want success without detail", entry.Settlement)
			}
		}
	}
	if settlements != 1 {
		t.Fatalf("%d settlement entries, want exactly one", settlements)
	}
}

// TestToolEffectPlansAndOutcomes proves the prepared-tool contract at the
// effect boundary: an immediate plan commits its ordinary terminal result
// without an effect intent, an executor-backed plan commits intent then one
// validated outcome, and an invalid plan or normalized-argument shape maps to
// the fixed validation-error result for the original call.
func TestToolEffectPlansAndOutcomes(t *testing.T) {
	publishCalls := func(t *testing.T, h *Harness, c *coordinator, sessionID string) {
		t.Helper()
		modelFn := modelAssemblingOnce(agent.ModelSettlement{
			Disposition: agent.DispoReady,
			Output:      completedOutputWith(testToolCall("call-1")),
		})
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), func(model.ModelRef, model.Stream) (model.Output, error) {
			return model.Output{}, nil
		}); err != nil {
			t.Fatalf("model effect: %v", err)
		}
	}
	success := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Immediate: &model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}
	}
	cases := []struct {
		name         string
		plan         func(_ context.Context, call model.ToolCall) PreparedTool
		want         model.ToolResult
		wantReplaces int
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
				return PreparedTool{Execute: func(context.Context) model.ToolResult {
					return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}
				}}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"},
			wantReplaces: 3, // intent + Operation + Session
		},
		{
			name:         "plan without immediate result or executor maps to the validation error",
			plan:         func(_ context.Context, call model.ToolCall) PreparedTool { return PreparedTool{} },
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantReplaces: 2,
		},
		{
			name: "invalid normalized arguments map to the validation error",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Immediate: success(context.Background(), call).Immediate, NormalizedArguments: json.RawMessage("{broken")}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantReplaces: 2,
		},
		{
			name: "returned outcome answering another call maps to the validation error",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Execute: func(context.Context) model.ToolResult {
					return model.ToolResult{CallID: "other-call", Status: model.ResultSuccess, Content: "ran"}
				}}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultError, Content: invalidToolResultContent},
			wantReplaces: 3,
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
			got, err := h.toolEffect(c, testOpID, tc.plan)(context.Background(), testToolCall("call-1"))
			if err != nil {
				t.Fatalf("tool effect: %v", err)
			}
			if got != tc.want {
				t.Fatalf("committed result = %+v, want %+v", got, tc.want)
			}
			if replaces != tc.wantReplaces {
				t.Fatalf("%d register replacements, want %d", replaces, tc.wantReplaces)
			}
			graph, err := validateFixture(t, store, sessionID)
			if err != nil {
				t.Fatalf("graph after the tool effect: %v", err)
			}
			results := 0
			for _, entry := range graph.Entries {
				if entry.ToolResult != nil {
					results++
					if entry.ToolResult.ToolCallID != "call-1" || entry.ToolResult.Status != tc.want.Status || entry.ToolResult.Content != tc.want.Content {
						t.Fatalf("tool result = %+v, want %+v", entry.ToolResult, tc.want)
					}
				}
			}
			if results != 1 {
				t.Fatalf("%d tool result entries, want exactly one", results)
			}
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			if err != nil {
				t.Fatalf("ReadOperation: %v", err)
			}
			if rec.State.Status != OperationRunning || rec.State.ActiveEffect != nil || len(rec.State.PendingToolCalls) != 0 {
				t.Fatalf("operation state = %+v, want running with the effect cleared and the call resolved", rec.State)
			}
		})
	}
}

// TestExecuteOrderedBatchSettlesExactlyOnce proves the ordered batch: calls
// dispatch in assembled order, every call receives exactly one terminal
// result, and the run continues to the outer success settlement.
func TestExecuteOrderedBatchSettlesExactlyOnce(t *testing.T) {
	turn := 0
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		if err := assembleCompleted(assemble); err != nil {
			return agent.ModelSettlement{}, err
		}
		turn++
		if turn == 1 {
			return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith(testToolCall("call-1"), testToolCall("call-2"))}, nil
		}
		return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, nil
	}
	spy := &toolSpy{}
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, spy.tool)
	if err := h.execute(c, testOpID, prepared); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := spy.dispatched(); len(got) != 2 || got[0] != "call-1" || got[1] != "call-2" {
		t.Fatalf("dispatch order = %v, want [call-1 call-2]", got)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the batch: %v", err)
	}
	results := map[string]int{}
	var lastSeq int64
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			results[entry.ToolResult.ToolCallID]++
			if entry.Envelope.Sequence <= lastSeq {
				t.Fatalf("tool result %s committed out of order", entry.Envelope.ID)
			}
		}
		lastSeq = entry.Envelope.Sequence
	}
	if results["call-1"] != 1 || results["call-2"] != 1 {
		t.Fatalf("result counts = %v, want exactly one per call", results)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationSuccess || len(rec.State.PendingToolCalls) != 0 {
		t.Fatalf("operation state = %+v, want terminal success with every call resolved", rec.State)
	}
}

// TestToolEffectRealOutcomeWinsCancellationRace proves a returned real outcome
// publishes even when the execution context died during the execution.
func TestToolEffectRealOutcomeWinsCancellationRace(t *testing.T) {
	var cancel context.CancelFunc
	turn := 0
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		if err := assembleCompleted(assemble); err != nil {
			return agent.ModelSettlement{}, err
		}
		turn++
		if turn == 1 {
			return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith(testToolCall("call-1"))}, nil
		}
		return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, nil
	}
	toolFn := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Execute: func(ctx context.Context) model.ToolResult {
			cancel() // the execution context dies during the concrete execution
			return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}
		}}
	}
	h, store, c, sessionID, prepared, harnessCancel := newExecutionHarness(t, modelFn, toolFn)
	cancel = harnessCancel
	if err := h.execute(c, testOpID, prepared); err != nil {
		t.Fatalf("execute: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the race: %v", err)
	}
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil && entry.ToolResult.ToolCallID == "call-1" {
			if entry.ToolResult.Status != model.ResultSuccess || entry.ToolResult.Content != "ran" {
				t.Fatalf("tool result = %+v, want the real outcome", entry.ToolResult)
			}
		}
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationInterruption {
		t.Fatalf("operation status = %s, want the outer interruption after the real outcome", rec.State.Status)
	}
}

// TestToolOriginatedInterruptionSettlesUnstartedCalls proves the reused
// interrupted-result settlement: an executor returning the interrupted result
// stops the batch and every remaining unstarted call receives the ordinary
// interrupted result through the common terminal helper.
func TestToolOriginatedInterruptionSettlesUnstartedCalls(t *testing.T) {
	turn := 0
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		if err := assembleCompleted(assemble); err != nil {
			return agent.ModelSettlement{}, err
		}
		turn++
		if turn == 1 {
			return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith(testToolCall("call-1"), testToolCall("call-2"))}, nil
		}
		return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, nil
	}
	toolFn := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Execute: func(context.Context) model.ToolResult {
			return model.ToolResult{CallID: call.ID, Status: model.ResultInterrupted, Content: "stopped by the tool"}
		}}
	}
	spy := &toolSpy{plan: toolFn}
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, spy.tool)
	if err := h.execute(c, testOpID, prepared); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := spy.dispatched(); len(got) != 1 || got[0] != "call-1" {
		t.Fatalf("dispatch order = %v, want the batch stopped after call-1", got)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the interruption: %v", err)
	}
	byCall := map[string]toolResultEntry{}
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			byCall[entry.ToolResult.ToolCallID] = *entry.ToolResult
		}
	}
	if got := byCall["call-1"]; got.Status != model.ResultInterrupted || got.Content != "stopped by the tool" {
		t.Fatalf("call-1 result = %+v, want the executor's real interrupted result", got)
	}
	if got := byCall["call-2"]; got.Status != model.ResultInterrupted || got.Content != interruptedToolResultContent {
		t.Fatalf("call-2 result = %+v, want the fixed interrupted-before-execution result", got)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != "agent interrupted" {
		t.Fatalf("operation state = %+v, want terminal interruption with the Agent's detail", rec.State)
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
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		if err := assembleCompleted(assemble); err != nil {
			return agent.ModelSettlement{}, err
		}
		turn++
		if turn == 2 {
			cancel() // the execution context dies between the first and second effect
		}
		return agent.ModelSettlement{Disposition: agent.DispoContinue, Output: erroredOutputWith()}, nil
	}
	h, store, c, sessionID, prepared, harnessCancel := newExecutionHarness(t, modelFn, func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Immediate: &model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}
	})
	cancel = harnessCancel
	if err := h.execute(c, testOpID, prepared); err != nil {
		t.Fatalf("execute: %v", err)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != "agent interrupted" {
		t.Fatalf("operation state = %+v, want terminal interruption with the Agent's detail", rec.State)
	}
	requireSessionCleared(t, h, sessionID)
	if _, err := validateFixture(t, store, sessionID); err != nil {
		t.Fatalf("graph after the cancellation: %v", err)
	}
}

// TestExecuteCapSettlesFailure proves the model-effect cap exhausts through
// the outer path: the Operation settles failure with the Agent's cap detail
// after the last settled continuation.
func TestExecuteCapSettlesFailure(t *testing.T) {
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		if err := assembleCompleted(assemble); err != nil {
			return agent.ModelSettlement{}, err
		}
		return agent.ModelSettlement{Disposition: agent.DispoContinue, Output: erroredOutputWith()}, nil
	}
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Immediate: &model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}
	})
	if err := h.execute(c, testOpID, prepared); err != nil {
		t.Fatalf("execute: %v", err)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationFailure || rec.State.Terminal == nil || rec.State.Terminal.Detail != "agent exceeded 25 model effects" {
		t.Fatalf("operation state = %+v, want terminal failure at the cap", rec.State)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph at the cap: %v", err)
	}
	assistants, signals := 0, 0
	for _, entry := range graph.Entries {
		if entry.Assistant != nil {
			assistants++
		}
		if entry.Signal != nil {
			signals++
		}
	}
	if assistants != 25 || signals != 25 {
		t.Fatalf("%d assistants and %d signals committed, want 25 of each", assistants, signals)
	}
	requireSessionCleared(t, h, sessionID)
}

// completedStream is a fake accepted model stream yielding one completed text
// response, for composition fixtures driving the real Agent assembly callback.
type completedStream struct {
	i int
}

func (s *completedStream) Recv() (model.StreamDelta, error) {
	if s.i > 0 {
		return model.StreamDelta{}, io.EOF
	}
	s.i++
	return model.StreamDelta{
		HasChoice:        true,
		Role:             "assistant",
		ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "done"}},
		FinishReason:     "stop",
	}, nil
}

func (s *completedStream) Close() error { return nil }

// assembleCompleted runs the real assembly callback over one fake completed
// stream; every output-bearing settlement of the composition fixtures needs
// exactly one successful assembly behind it.
func assembleCompleted(assemble agent.AssemblyCallback) error {
	_, err := assemble(testModelRef(), &completedStream{})
	return err
}

// TestToolResultAdoptsIntoView proves a committed tool result adopts into the
// coordinator view: the next contextSource projection carries it as its
// model.RoleTool message, and a second tool effect in the same Operation sees
// the first one committed.
func TestToolResultAdoptsIntoView(t *testing.T) {
	h, _, c, sessionID := newEffectHarness(t, nil)
	publishCalls(t, h, c, sessionID, testToolCall("call-1"), testToolCall("call-2"))
	source := h.contextSource(c, testOpID)
	immediate := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Immediate: &model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran " + call.ID}}
	}
	if _, err := h.toolEffect(c, testOpID, immediate)(context.Background(), testToolCall("call-1")); err != nil {
		t.Fatalf("first tool effect: %v", err)
	}
	msgs, err := source(context.Background())
	if err != nil {
		t.Fatalf("projection after the first tool result: %v", err)
	}
	if len(msgs) != 4 { // system, input, assistant, tool result
		t.Fatalf("projection carried %d messages, want the committed tool result included (4)", len(msgs))
	}
	tool := msgs[len(msgs)-1]
	if tool.Role != model.RoleTool || tool.ToolCallID != "call-1" || tool.TextContent() != "ran call-1" {
		t.Fatalf("projected tool message = %+v, want the committed call-1 result", tool)
	}
	if _, err := h.toolEffect(c, testOpID, immediate)(context.Background(), testToolCall("call-2")); err != nil {
		t.Fatalf("second tool effect: %v", err)
	}
	msgs, err = source(context.Background())
	if err != nil {
		t.Fatalf("projection after the second tool result: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("projection carried %d messages, want both tool results (5)", len(msgs))
	}
	if tool = msgs[len(msgs)-1]; tool.ToolCallID != "call-2" || tool.TextContent() != "ran call-2" {
		t.Fatalf("projected tool message = %+v, want the committed call-2 result", tool)
	}
}

// publishCalls drives one ready model effect publishing the given calls as
// pending, for direct tool-effect fixtures.
func publishCalls(t *testing.T, h *Harness, c *coordinator, sessionID string, calls ...model.ToolCall) {
	t.Helper()
	modelFn := modelAssemblingOnce(agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith(calls...)})
	if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), func(model.ModelRef, model.Stream) (model.Output, error) {
		return model.Output{}, nil
	}); err != nil {
		t.Fatalf("model effect: %v", err)
	}
}

// TestSettleAgentTerminalClassifiesRunError proves the outer settlement
// classifies the run error: a storage-class error performs no compensating
// write and leaves the committed running state for recovery; an
// execution-context error is between-effect cancellation settling the fixed
// interruption detail; anything else settles failure with the error's text.
func TestSettleAgentTerminalClassifiesRunError(t *testing.T) {
	t.Run("storage failure leaves the running state for recovery", func(t *testing.T) {
		modelFn := modelAssemblingOnce(agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith(testToolCall("call-1"))})
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
		err := h.execute(c, testOpID, prepared)
		if !errors.Is(err, ErrStorage) {
			t.Fatalf("execute = %v, want the injected storage failure", err)
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.State.Status != OperationRunning {
			t.Fatalf("operation status = %s, want the committed running state left for recovery", rec.State.Status)
		}
		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("graph after the storage failure: %v", err)
		}
		for _, entry := range graph.Entries {
			if entry.Settlement != nil {
				t.Fatalf("settlement entry %s published over a storage failure", entry.Envelope.ID)
			}
		}
	})
	t.Run("execution-context error settles the fixed interruption detail", func(t *testing.T) {
		for _, runErr := range []error{context.Canceled, context.DeadlineExceeded} {
			h, store, c, sessionID := newEffectHarness(t, nil)
			publishCalls(t, h, c, sessionID, testToolCall("call-1"))
			if err := h.settleAgentTerminal(c, testOpID, agent.TerminalResult{}, runErr); !errors.Is(err, runErr) {
				t.Fatalf("settleAgentTerminal(%v) = %v, want the run error back", runErr, err)
			}
			rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
			if err != nil {
				t.Fatalf("ReadOperation: %v", err)
			}
			if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
				t.Fatalf("operation state = %+v, want terminal interruption with the fixed detail", rec.State)
			}
			if _, err := validateFixture(t, store, sessionID); err != nil {
				t.Fatalf("graph after the interruption: %v", err)
			}
		}
	})
	t.Run("non-storage protocol error still settles failure", func(t *testing.T) {
		h, _, c, sessionID := newEffectHarness(t, nil)
		runErr := errors.New("context source broke")
		if err := h.settleAgentTerminal(c, testOpID, agent.TerminalResult{}, runErr); !errors.Is(err, runErr) {
			t.Fatalf("settleAgentTerminal = %v, want the run error back", err)
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.State.Status != OperationFailure || rec.State.Terminal == nil || rec.State.Terminal.Detail != runErr.Error() {
			t.Fatalf("operation state = %+v, want terminal failure with the error text", rec.State)
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
	got, err := h.toolEffect(c, testOpID, spy.tool)(ctx, testToolCall("call-1"))
	if err != nil {
		t.Fatalf("tool effect: %v", err)
	}
	if dispatched := spy.dispatched(); len(dispatched) != 0 {
		t.Fatalf("prepared invoked for %v, want zero preparations", dispatched)
	}
	if got != (model.ToolResult{CallID: "call-1", Status: model.ResultInterrupted, Content: interruptedToolResultContent}) {
		t.Fatalf("committed result = %+v, want the interrupted-before-execution result", got)
	}
	if replaces != 2 { // Operation + Session registers only: no effect intent
		t.Fatalf("%d register replacements, want the intent-free transition (2)", replaces)
	}
	// The run settles interruption and the terminal helper interrupts the
	// remaining unstarted call.
	if err := h.settleAgentTerminal(c, testOpID, agent.TerminalResult{Status: agent.TerminalInterruption, Detail: executionInterruptedDetail}, nil); err != nil {
		t.Fatalf("outer settlement: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the interruption: %v", err)
	}
	byCall := map[string]toolResultEntry{}
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			byCall[entry.ToolResult.ToolCallID] = *entry.ToolResult
		}
	}
	if got := byCall["call-2"]; got.Status != model.ResultInterrupted || got.Content != interruptedToolResultContent {
		t.Fatalf("call-2 result = %+v, want the terminal helper's interrupted result", got)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
		t.Fatalf("operation state = %+v, want terminal interruption", rec.State)
	}
	requireSessionCleared(t, h, sessionID)
}

// TestAssistantEntryPreservesRawArgumentBytes proves the raw-argument row:
// the assistant record stores each call's exact raw argument bytes and never
// fabricates normalized arguments from JSON syntax — valid JSON alone does
// not become a normalized value, and malformed, non-UTF-8 bytes are neither
// altered nor lost; the entry round-trips through the codecs.
func TestAssistantEntryPreservesRawArgumentBytes(t *testing.T) {
	valid := json.RawMessage(` {"x": 1} `)      // valid JSON whose bytes are not canonical
	malformed := json.RawMessage("\xff{broken") // invalid JSON and invalid UTF-8
	modelFn := modelAssemblingOnce(agent.ModelSettlement{
		Disposition: agent.DispoReady,
		Output: completedOutputWith(
			model.ToolCall{ID: "call-1", Name: "echo", Arguments: valid},
			model.ToolCall{ID: "call-2", Name: "echo", Arguments: malformed},
		),
	})
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, modelFn), func(model.ModelRef, model.Stream) (model.Output, error) {
		return model.Output{}, nil
	}); err != nil {
		t.Fatalf("model effect: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the model effect: %v", err)
	}
	for _, entry := range graph.Entries {
		if entry.Assistant == nil || len(entry.Assistant.ToolCalls) != 2 {
			continue
		}
		first, second := entry.Assistant.ToolCalls[0], entry.Assistant.ToolCalls[1]
		if raw, err := base64.StdEncoding.DecodeString(first.ArgumentsBase64); err != nil || string(raw) != string(valid) {
			t.Fatalf("call-1 raw arguments = %q err %v, want the exact caller bytes", raw, err)
		}
		if len(first.NormalizedArguments) != 0 {
			t.Fatalf("call-1 normalized arguments = %q, want none fabricated from valid JSON", first.NormalizedArguments)
		}
		if raw, err := base64.StdEncoding.DecodeString(second.ArgumentsBase64); err != nil || string(raw) != string(malformed) {
			t.Fatalf("call-2 raw arguments = %q err %v, want the exact malformed bytes preserved", raw, err)
		}
		if len(second.NormalizedArguments) != 0 {
			t.Fatalf("call-2 normalized arguments = %q, want the field absent for malformed bytes", second.NormalizedArguments)
		}
		return
	}
	t.Fatalf("no assistant entry with two calls committed")
}

// TestModelEffectIntentCancellationSettlesInterruption proves an intent
// transaction aborted by cancellation settles the cancellation outcome: the
// run ends in the fixed terminal interruption, never in a boundary violation.
func TestModelEffectIntentCancellationSettlesInterruption(t *testing.T) {
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		if err := assembleCompleted(assemble); err != nil {
			return agent.ModelSettlement{}, err
		}
		return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, nil
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
	if err := h.execute(c, testOpID, prepared); err != nil {
		t.Fatalf("execute: %v", err)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
		t.Fatalf("operation state = %+v, want terminal interruption with the fixed detail", rec.State)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the cancellation: %v", err)
	}
	for _, entry := range graph.Entries {
		if entry.Assistant != nil {
			t.Fatalf("assistant entry %s published after an aborted intent", entry.Envelope.ID)
		}
	}
	requireSessionCleared(t, h, sessionID)
}

// TestToolEffectIntentCancellationSettlesInterrupted proves a tool intent
// transaction aborted by cancellation settles the call's
// interrupted-before-execution result: the batch stops and the terminal helper
// interrupts every remaining unstarted call.
func TestToolEffectIntentCancellationSettlesInterrupted(t *testing.T) {
	modelFn := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		if err := assembleCompleted(assemble); err != nil {
			return agent.ModelSettlement{}, err
		}
		return agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith(testToolCall("call-1"), testToolCall("call-2"))}, nil
	}
	toolFn := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Execute: func(context.Context) model.ToolResult {
			return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}
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
	if err := h.execute(c, testOpID, prepared); err != nil {
		t.Fatalf("execute: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the cancellation: %v", err)
	}
	byCall := map[string]toolResultEntry{}
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			byCall[entry.ToolResult.ToolCallID] = *entry.ToolResult
		}
	}
	if got := byCall["call-1"]; got.Status != model.ResultInterrupted || got.Content != interruptedToolResultContent {
		t.Fatalf("call-1 result = %+v, want the interrupted-before-execution result", got)
	}
	if got := byCall["call-2"]; got.Status != model.ResultInterrupted || got.Content != interruptedToolResultContent {
		t.Fatalf("call-2 result = %+v, want the terminal helper's interrupted result", got)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != executionInterruptedDetail {
		t.Fatalf("operation state = %+v, want terminal interruption with the fixed detail", rec.State)
	}
	requireSessionCleared(t, h, sessionID)
}
