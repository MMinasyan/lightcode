package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	_, disposition, err := h.admit(context.Background(), admissionRequest{
		SessionID:   session.Identity.SessionID,
		OperationID: testOpID,
		Origin:      InputOriginUser,
		Content:     admissionContent("hello"),
	})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if disposition != DispositionAdmitted {
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

// modelAssemblingOnce invokes the assembly callback exactly once, ignoring its
// outcome, and returns one fixed settlement.
func modelAssemblingOnce(set agent.ModelSettlement) agent.ModelEffect {
	return func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		_, _ = assemble(testModelRef(), nil)
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
// Operation-owned input entry and advances last activity to its commit time.
func TestSteeringInputHelper(t *testing.T) {
	h, store, c, sessionID := newEffectHarness(t, nil)
	before, err := h.ReadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if err := h.commitSteeringInput(context.Background(), c, testOpID, admissionContent("steering")); err != nil {
		t.Fatalf("commitSteeringInput: %v", err)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("post-steering graph: %v", err)
	}
	var input *inputEntry
	for i := range graph.Entries {
		if graph.Entries[i].Input != nil && graph.Entries[i].Input.Origin == InputOriginUser && graph.Entries[i].Envelope.OperationID == testOpID && graph.Entries[i].Input.Content[0].Text == "steering" {
			input = graph.Entries[i].Input
		}
	}
	if input == nil {
		t.Fatalf("steering input entry not committed as Operation-owned user input")
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
		if err := h.commitSteeringInput(context.Background(), c, testOpID, admissionContent("steering")); !errors.Is(err, ErrConflict) {
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
		if err := h.commitSteeringInput(context.Background(), c, testOpID, admissionContent("steering")); !errors.Is(err, ErrInvalid) {
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
		if err := h.commitSteeringInput(context.Background(), c, testOpID, admissionContent("steering")); !errors.Is(err, ErrInvalid) {
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
		if err := h.commitSteeringInput(context.Background(), c, testOpID, admissionContent("steering")); !errors.Is(err, ErrInvalid) {
			t.Fatalf("steering over a foreign archive = %v, want ErrInvalid", err)
		}
	})
}
