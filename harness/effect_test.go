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
	"github.com/MMinasyan/lightcode/model"
)

// newEffectHarness admits one running Operation ("op-1") with the given
// prepared physical model request function and returns the pieces the effect
// fixtures need.
func newEffectHarness(t *testing.T, modelFn func(context.Context, model.Request) (model.Stream, error)) (*Harness, *graphStorage, *coordinator, string) {
	t.Helper()
	store := emptyStore(t)
	prepared := modelPrepared(modelFn)
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

// invokeModelEffect drives one model effect with a validated request; a nil
// assemble callback runs the real Agent assembly.
func invokeModelEffect(t *testing.T, me agent.ModelEffect, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
	t.Helper()
	req, err := model.NewRequest(model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("hello")}},
		Tools:    []model.ToolDefinition{testToolDefinition()},
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if assemble == nil {
		assemble = func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			return agent.Assemble(context.Background(), source, stream)
		}
	}
	return me(context.Background(), req, assemble)
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
var fixturePermission = []PermissionRequest{{Permission: permissionCommandRun, Target: "fixture"}}

// effectExecution supplies the normalizer every opened execution requires.
func effectExecution(modelFn func(context.Context, model.Request) (model.Stream, error), toolFn func(context.Context, model.ToolCall) PreparedTool) Execution {
	return Execution{Model: modelFn, Tool: toolFn, NormalizeTool: objectNormalize}
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
	me := h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture())
	set, err := invokeModelEffect(t, me, func(source model.ModelRef, stream model.Stream) (model.Output, error) {
		calls++
		return agent.Assemble(context.Background(), source, stream)
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
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return erroredTurnStream(), nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil)
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
		set, err := invokeModelEffectCtx(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), ctx, nil)
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
		if rec.State.Status != OperationInterruption || rec.State.Terminal == nil || rec.State.Terminal.Detail != "stream interrupted: context canceled" {
			t.Fatalf("operation state = %+v, want terminal interruption with the assembly's detail", rec.State)
		}
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
		set, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil)
		if err != nil {
			t.Fatalf("model effect: %v", err)
		}
		if set.Disposition != agent.DispoFailure || set.Detail != "model failure" {
			t.Fatalf("settlement = %+v, want the committed failure with its diagnostic", set)
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

	t.Run("cancellation before an attempt settles without output", func(t *testing.T) {
		attempts := 0
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			attempts++
			return nil, nil
		}
		h, store, c, sessionID := newEffectHarness(t, modelFn)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the execution context dies before any attempt could start
		me := h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture())
		set, err := invokeModelEffectCtx(t, me, ctx, nil)
		if err != nil {
			t.Fatalf("model effect: %v", err)
		}
		if set.Disposition != agent.DispoInterruption || set.Detail != executionInterruptedDetail {
			t.Fatalf("settlement = %+v, want the committed interruption settlement", set)
		}
		if attempts != 0 {
			t.Fatalf("physical attempts = %d, want zero after observed cancellation", attempts)
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
	me := h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture())
	if _, err := invokeModelEffect(t, me, nil); err != nil {
		t.Fatalf("first effect: %v", err)
	}
	_, err := invokeModelEffect(t, me, nil)
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
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return failStream(errors.New("empty failure"), usageDelta(model.Usage{InputTokens: 4, CachedInputTokens: 2, OutputTokens: 6})), nil
	}
	h, store, c, sessionID := newEffectHarness(t, modelFn)
	if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
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
			if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err == nil {
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
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}
		h, store, c, sessionID := newEffectHarness(t, modelFn)
		foreignEffectRace(t, store, sessionID, testOpID, "foreign")
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); !errors.Is(err, ErrConflict) {
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
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			foreignEffectRace(t, store, sessionID, testOpID, "foreign") // between the committed intent and the result transaction
			return completedTurnStream(), nil
		}
		h, st, c, sid := newEffectHarness(t, modelFn)
		store, sessionID = st, sid
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); !errors.Is(err, ErrConflict) {
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
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return nil, errors.New("model failure")
		}
		h, store, c, sessionID := newEffectHarness(t, modelFn)
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
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
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return nil, errors.New("model failure")
		}
		h, store, c, sessionID := newEffectHarness(t, modelFn)
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
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
		modelFn := func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}
		me := h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture())
		if _, err := invokeModelEffect(t, me, nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("intent over a foreign terminal operation = %v, want ErrInvalid", err)
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
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); !errors.Is(err, ErrInvalid) {
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
	store := emptyStore(t)
	prepared := PreparedExecution{Capture: testCapture(), Open: open}
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

// newExecutionHarness admits one running Operation ("op-1") under a cancelable
// Harness context with both prepared effect functions, returning the pieces
// the execution fixtures need.
func newExecutionHarness(t *testing.T, modelFn func(context.Context, model.Request) (model.Stream, error), toolFn func(context.Context, model.ToolCall) PreparedTool) (*Harness, *graphStorage, *coordinator, string, PreparedExecution, context.CancelFunc) {
	t.Helper()
	if toolFn == nil {
		t.Fatalf("execution fixtures require a prepared tool function")
	}
	return newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
		return effectExecution(modelFn, toolFn), nil
	})
}

// invokeModelEffectCtx drives one model effect with an explicit context; a nil
// assemble callback runs the real Agent assembly.
func invokeModelEffectCtx(t *testing.T, me agent.ModelEffect, ctx context.Context, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
	t.Helper()
	req, err := model.NewRequest(model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: admissionContent("hello")}},
		Tools:    []model.ToolDefinition{testToolDefinition()},
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
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
	me := h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture())
	set, err := invokeModelEffectCtx(t, me, ctx, nil)
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
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(), nil
	}
	spy := &toolSpy{}
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, spy.tool)
	if err := h.execute(c, testOpID, prepared, h.ctx); err != nil {
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
	if err := h.execute(c, testOpID, prepared, h.ctx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(opens) != 1 {
		t.Fatalf("%d opener calls, want exactly one per execution", len(opens))
	}
	if opens[0].ctx != h.ctx {
		t.Fatalf("opener ran on a foreign context, want the execution context")
	}
	adm := opens[0].adm
	if adm.SessionID != sessionID || adm.OperationID != testOpID || adm.AgentType != "coder" {
		t.Fatalf("opener admission = %+v, want the owned committed admission", adm)
	}
	if !reflect.DeepEqual(adm.Execution, testCapture()) {
		t.Fatalf("opener capture = %+v, want the prepared capture", adm.Execution)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationSuccess {
		t.Fatalf("operation status = %s, want the Agent run over the opened effects", rec.State.Status)
	}
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
	if err := h.execute(c, testOpID, prepared, h.ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("execute = %v, want the context error", err)
	}
	if opens != 0 {
		t.Fatalf("opener calls = %d, want a canceled execution to skip the unstarted opener", opens)
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
		t.Fatalf("graph after the skipped opener: %v", err)
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
			return Execution{
				Model: func(context.Context, model.Request) (model.Stream, error) {
					modelRuns++
					return nil, nil
				},
				Tool: func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
			}, openErr
		})
		if err := h.execute(c, testOpID, prepared, h.ctx); err != openErr {
			t.Fatalf("execute = %v, want the exact opener error", err)
		}
		if modelRuns != 0 {
			t.Fatalf("model effect ran %d times after a failed open, want zero", modelRuns)
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.State.Status != OperationFailure || rec.State.Terminal == nil || rec.State.Terminal.Detail != "open broke" {
			t.Fatalf("operation state = %+v, want terminal failure with the opener error text", rec.State)
		}
		requireSessionCleared(t, h, sessionID)
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph after the opening failure: %v", err)
		}
	})

	t.Run("storage opener error retains running state", func(t *testing.T) {
		storageErr := fmt.Errorf("opener storage failure: %w", ErrStorage)
		h, _, c, sessionID, prepared, _ := newOpenerHarness(t, func(context.Context, OperationAdmission) (Execution, error) {
			return Execution{}, storageErr
		})
		if err := h.execute(c, testOpID, prepared, h.ctx); err != storageErr {
			t.Fatalf("execute = %v, want the exact storage-class error", err)
		}
		rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.State.Status != OperationRunning {
			t.Fatalf("operation status = %s, want the committed running state preserved for recovery", rec.State.Status)
		}
		session, err := h.ReadSession(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if session.State.CurrentOperationID != testOpID {
			t.Fatalf("session current operation = %q, want the running Operation still current", session.State.CurrentOperationID)
		}
	})
}

// TestExecuteInvalidOpenedExecutionClosesBeforeRejection proves the
// invalid-success row: a successful Open with a nil Model, Tool, or
// NormalizeTool has its non-nil Close invoked exactly once before the
// rejection — while the Operation is still running — never runs the Agent,
// and settles failure through the ordinary terminal settlement.
func TestExecuteInvalidOpenedExecutionClosesBeforeRejection(t *testing.T) {
	for _, name := range []string{"nil model", "nil tool", "nil normalization"} {
		t.Run(name, func(t *testing.T) {
			var h *Harness
			closes := 0
			var statusDuringClose OperationState
			modelRuns := 0
			open := func(_ context.Context, adm OperationAdmission) (Execution, error) {
				exec := effectExecution(func(context.Context, model.Request) (model.Stream, error) {
					modelRuns++
					return completedTurnStream(), nil
				}, func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} })
				switch name {
				case "nil model":
					exec.Model = nil
				case "nil tool":
					exec.Tool = nil
				case "nil normalization":
					exec.NormalizeTool = nil
				}
				exec.Close = func() error {
					closes++
					rec, err := h.ReadOperation(context.Background(), adm.SessionID, adm.OperationID)
					if err != nil {
						t.Errorf("read operation from the rejection closer: %v", err)
					}
					statusDuringClose = rec.State.Status
					return nil
				}
				return exec, nil
			}
			var store *graphStorage
			var c *coordinator
			var sessionID string
			var prepared PreparedExecution
			h, store, c, sessionID, prepared, _ = newOpenerHarness(t, open)
			err := h.execute(c, testOpID, prepared, h.ctx)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("execute = %v, want the invalid-execution rejection", err)
			}
			if modelRuns != 0 {
				t.Fatalf("model effect ran %d times with an invalid execution, want zero: no Agent runs with invalid effects", modelRuns)
			}
			if closes != 1 {
				t.Fatalf("Close calls = %d, want exactly one for an invalid successful Execution", closes)
			}
			if statusDuringClose != OperationRunning {
				t.Fatalf("close observed status %s, want the rejection published only after the cleanup", statusDuringClose)
			}
			settled, readErr := h.ReadOperation(context.Background(), sessionID, testOpID)
			if readErr != nil {
				t.Fatalf("ReadOperation: %v", readErr)
			}
			if settled.State.Status != OperationFailure || settled.State.Terminal == nil || settled.State.Terminal.Detail != err.Error() {
				t.Fatalf("operation state = %+v, want terminal failure with the rejection text %q", settled.State, err)
			}
			requireSessionCleared(t, h, sessionID)
			if _, err := validateFixture(t, store, sessionID); err != nil {
				t.Fatalf("graph after the rejected execution: %v", err)
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
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
			t.Fatalf("model effect: %v", err)
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
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: permissionDeniedToolResultContent},
			wantReplaces: 2, // denial settles through the intent-free transition
		},
		{
			name: "immediate success without declarations is denied",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: permissionDeniedToolResultContent},
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
					Permissions: []PermissionRequest{{Permission: permissionCommandRun, Target: "fixture"}, {Permission: "", Target: "fixture"}},
					Execute: func(context.Context) ToolOutcome {
						return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
					},
				}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: permissionDeniedToolResultContent},
			wantReplaces: 2,
		},
		{
			name: "one built-in-denied pair denies the whole call",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions: []PermissionRequest{
						{Permission: permissionCommandRun, Target: "fixture"},
						{Permission: permissionFileWrite, Target: "/w/sub/.env"},
					},
					CanonicalWorkspace: "/w",
					Execute: func(context.Context) ToolOutcome {
						return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
					},
				}
			},
			want:         model.ToolResult{CallID: "call-1", Status: model.ResultDenied, Content: permissionDeniedToolResultContent},
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
			got, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-1"))
			if err != nil {
				t.Fatalf("tool effect: %v", err)
			}
			if got != tc.want {
				t.Fatalf("committed result = %+v, want %+v", got, tc.want)
			}
			if executions != tc.wantExecutions {
				t.Fatalf("executor ran %d times, want %d", executions, tc.wantExecutions)
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
			plan:        filePlan(PermissionRequest{Permission: permissionFileRead, Target: "/w/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "file.write inside the canonical Workspace is allowed without readonly",
			plan:        filePlan(PermissionRequest{Permission: permissionFileWrite, Target: "/w/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "file.read outside the canonical Workspace is denied",
			plan:        filePlan(PermissionRequest{Permission: permissionFileRead, Target: "/other/a.txt"}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "an explicit user rule allows a file.read target outside the canonical Workspace",
			policy:      ResolvePermissionPolicy(json.RawMessage(`{"rules":[{"permission":"file.read","target":"*","access":"allow"}]}`), nil),
			plan:        filePlan(PermissionRequest{Permission: permissionFileRead, Target: "/other/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "empty file target is denied",
			plan:        filePlan(PermissionRequest{Permission: permissionFileRead, Target: ""}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "relative file target is denied",
			plan:        filePlan(PermissionRequest{Permission: permissionFileRead, Target: "a.txt"}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "lexically unclean file target is denied",
			plan:        filePlan(PermissionRequest{Permission: permissionFileWrite, Target: "/w/sub/../b.txt"}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name: "empty canonical Workspace root for a file plan is denied",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions: []PermissionRequest{{Permission: permissionFileRead, Target: "/w/a.txt"}},
					Execute: func(context.Context) ToolOutcome {
						return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
					},
				}
			},
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "a relative canonical Workspace root is denied",
			plan:        filePlanRoots("w", "/w/sub", PermissionRequest{Permission: permissionFileRead, Target: "/w/a.txt"}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "a lexically unclean canonical Workspace root is denied",
			plan:        filePlanRoots("/w/../w", "/w/sub", PermissionRequest{Permission: permissionFileRead, Target: "/w/a.txt"}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "a relative canonical write root is denied",
			capture:     ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, WriteDir: "/w/sub"},
			policy:      allowAllWrites,
			plan:        filePlanRoots("/w", "sub", PermissionRequest{Permission: permissionFileWrite, Target: "/w/sub/a.txt"}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "a lexically unclean canonical write root is denied",
			capture:     ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, WriteDir: "/w/sub"},
			policy:      allowAllWrites,
			plan:        filePlanRoots("/w", "/w/sub/../sub", PermissionRequest{Permission: permissionFileWrite, Target: "/w/sub/a.txt"}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "readonly without write_dir denies file.write",
			capture:     ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, Readonly: true},
			plan:        filePlan(PermissionRequest{Permission: permissionFileWrite, Target: "/w/a.txt"}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "readonly without write_dir still allows file.read",
			capture:     ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, Readonly: true},
			plan:        filePlan(PermissionRequest{Permission: permissionFileRead, Target: "/w/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "write_dir confines file.write inside it",
			capture:     ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, WriteDir: "/w/sub"},
			plan:        filePlan(PermissionRequest{Permission: permissionFileWrite, Target: "/w/sub/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "write_dir denies file.write outside it even when policy allows",
			capture:     ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, WriteDir: "/w/sub"},
			policy:      allowAllWrites,
			plan:        filePlan(PermissionRequest{Permission: permissionFileWrite, Target: "/w/other.txt"}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:    "configured write_dir without a canonical prepared write root is denied",
			capture: ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, WriteDir: "/w/sub"},
			policy:  allowAllWrites,
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions:        []PermissionRequest{{Permission: permissionFileWrite, Target: "/w/sub/a.txt"}},
					CanonicalWorkspace: "/w",
					Execute: func(context.Context) ToolOutcome {
						return ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran"}}
					},
				}
			},
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "readonly with write_dir allows confined writes",
			capture:     ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, Readonly: true, WriteDir: "/w/sub"},
			policy:      allowAllWrites,
			plan:        filePlan(PermissionRequest{Permission: permissionFileWrite, Target: "/w/sub/a.txt"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name:        "write_dir does not lift the built-in sensitive-basename denial",
			capture:     ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, WriteDir: "/w/sub"},
			plan:        filePlan(PermissionRequest{Permission: permissionFileWrite, Target: "/w/sub/.env"}),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name:        "an explicit user rule overrides the sensitive denial inside write_dir",
			capture:     ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, WriteDir: "/w/sub"},
			policy:      allowAllWrites,
			plan:        filePlan(PermissionRequest{Permission: permissionFileWrite, Target: "/w/sub/.env"}),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name: "command-only plan needs no canonical bindings",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions: []PermissionRequest{{Permission: permissionCommandRun, Target: "git status"}},
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
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name: "multi-target patch with every pair allowed executes once",
			plan: filePlan(
				PermissionRequest{Permission: permissionFileRead, Target: "/w/src.txt"},
				PermissionRequest{Permission: permissionFileWrite, Target: "/w/dst.txt"},
			),
			wantContent: "ran", wantExecuted: true,
		},
		{
			name: "multi-target patch with a denied source denies the whole call",
			plan: filePlan(
				PermissionRequest{Permission: permissionFileRead, Target: "/other/src.txt"},
				PermissionRequest{Permission: permissionFileWrite, Target: "/w/dst.txt"},
			),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name: "multi-target patch with a denied destination denies the whole call",
			plan: filePlan(
				PermissionRequest{Permission: permissionFileRead, Target: "/w/src.txt"},
				PermissionRequest{Permission: permissionFileWrite, Target: "/other/dst.txt"},
			),
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
		},
		{
			name: "immediate success passes through the same evaluation",
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions:        []PermissionRequest{{Permission: permissionFileWrite, Target: "/w/a.txt"}},
					CanonicalWorkspace: "/w",
					Immediate:          &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}},
				}
			},
			wantContent: "done",
		},
		{
			name:    "immediate success denied by the boundary carries no result content",
			capture: ExecutionCapture{ConfigurationRevision: "rev-1", Model: testModelRef(), SystemPrompt: "system", Tools: testCapture().Tools, Readonly: true},
			plan: func(_ context.Context, call model.ToolCall) PreparedTool {
				return PreparedTool{
					Permissions:        []PermissionRequest{{Permission: permissionFileWrite, Target: "/w/a.txt"}},
					CanonicalWorkspace: "/w",
					Immediate:          &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}},
				}
			},
			wantContent: permissionDeniedToolResultContent, wantDenied: true,
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
			got, err := h.toolEffect(c, testOpID, Execution{Tool: plan, NormalizeTool: objectNormalize, Permissions: tc.policy}, capture)(context.Background(), testToolCall("call-1"))
			if err != nil {
				t.Fatalf("tool effect: %v", err)
			}
			wantStatus := model.ResultSuccess
			if tc.wantDenied {
				wantStatus = model.ResultDenied
			}
			if got.Status != wantStatus || got.Content != tc.wantContent {
				t.Fatalf("committed result = %+v, want status %s content %q", got, wantStatus, tc.wantContent)
			}
			if tc.wantExecuted != (executions == 1) || executions > 1 {
				t.Fatalf("executor ran %d times, want executed=%v", executions, tc.wantExecuted)
			}
			graph, err := validateFixture(t, store, sessionID)
			if err != nil {
				t.Fatalf("graph after the tool effect: %v", err)
			}
			for _, entry := range graph.Entries {
				if entry.ToolResult != nil {
					if tc.wantDenied && len(entry.ToolResult.Metadata) != 0 {
						t.Fatalf("denied result carries metadata %s, want none", entry.ToolResult.Metadata)
					}
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
	got, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-1"))
	if err != nil {
		t.Fatalf("denied tool effect: %v", err)
	}
	if got.Status != model.ResultDenied || got.Content != permissionDeniedToolResultContent {
		t.Fatalf("call-1 result = %+v, want the fixed denial", got)
	}
	if executions != 0 {
		t.Fatalf("concrete effects started = %d, want none for the denied call", executions)
	}
	if replaces != 2 { // Operation + Session only: denial never writes an effect intent
		t.Fatalf("%d register replacements for the denial, want the intent-free transition (2)", replaces)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.State.Status != OperationRunning || rec.State.ActiveEffect != nil {
		t.Fatalf("operation after the denial = %+v, want running with no active effect", rec.State)
	}
	if len(rec.State.PendingToolCalls) != 1 || rec.State.PendingToolCalls[0].CallID != "call-2" {
		t.Fatalf("pending calls = %+v, want the later call untouched", rec.State.PendingToolCalls)
	}
	if _, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-2")); err != nil {
		t.Fatalf("later tool effect: %v", err)
	}
	if executions != 1 {
		t.Fatalf("concrete effects started = %d, want exactly the later allowed call", executions)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after both calls: %v", err)
	}
	byCall := map[string]toolResultEntry{}
	for _, entry := range graph.Entries {
		if entry.ToolResult != nil {
			byCall[entry.ToolResult.ToolCallID] = *entry.ToolResult
		}
	}
	if len(byCall) != 2 {
		t.Fatalf("%d settled calls, want exactly one per call", len(byCall))
	}
	if got := byCall["call-1"]; got.Status != model.ResultDenied || len(got.Metadata) != 0 {
		t.Fatalf("call-1 result = %+v, want the metadata-less denial", got)
	}
	if got := byCall["call-2"]; got.Status != model.ResultSuccess || got.Content != "ran" {
		t.Fatalf("call-2 result = %+v, want the allowed execution", got)
	}
}

// TestToolEffectAdvertisementGate proves the one advertisement gate ahead of
// every preparer: a completed call whose committed record names a tool
// outside the capture set settles the ordinary unavailable-tool error with no
// preparation and no normalization, while an advertised sibling still runs.
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
		), nil
	}
	if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, Execution{Model: modelFn, NormalizeTool: normalize}, testCapture()), nil); err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if normalized != 1 {
		t.Fatalf("normalization callback ran %d times, want only the advertised call", normalized)
	}
	prepared := 0
	plan := func(_ context.Context, call model.ToolCall) PreparedTool {
		prepared++
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
	}
	got, err := h.toolEffect(c, testOpID, Execution{Tool: plan, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-1"))
	if err != nil {
		t.Fatalf("unadvertised tool effect: %v", err)
	}
	if got.Status != model.ResultError || got.Content != `Tool "ghost" is not available.` {
		t.Fatalf("unadvertised result = %+v, want the ordinary unavailable-tool error", got)
	}
	if normalized != 1 {
		t.Fatalf("normalization callback after the rejection = %d, want the gate to precede any normalization", normalized)
	}
	if prepared != 0 {
		t.Fatalf("preparations = %d, want none before the advertisement gate", prepared)
	}
	if _, err := h.toolEffect(c, testOpID, Execution{Tool: plan, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-2")); err != nil {
		t.Fatalf("advertised tool effect: %v", err)
	}
	if prepared != 1 {
		t.Fatalf("preparations = %d, want the advertised sibling to reach the preparer", prepared)
	}
	if _, err := validateFixture(t, store, sessionID); err != nil {
		t.Fatalf("graph after the gate: %v", err)
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
		if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, Execution{Model: modelFn, NormalizeTool: normalize}, testCapture()), nil); err != nil {
			t.Fatalf("model effect: %v", err)
		}
	}
	committedAssistant := func(t *testing.T, store *graphStorage, sessionID string) toolCallRecord {
		t.Helper()
		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("graph: %v", err)
		}
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
		if normalized != 1 {
			t.Fatalf("producer normalizations = %d, want one", normalized)
		}
		before := committedAssistant(t, store, sessionID)
		if string(before.NormalizedArguments) != `{"x":1}` {
			t.Fatalf("committed member = %q, want the compacted object", before.NormalizedArguments)
		}
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
		got, err := h.toolEffect(c, testOpID, Execution{Tool: plan, NormalizeTool: normalize}, testCapture())(context.Background(), forged)
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if got.Status != model.ResultSuccess || got.Content != "done" {
			t.Fatalf("result = %+v, want the committed call to execute, not a forged-name rejection", got)
		}
		if normalized != 1 {
			t.Fatalf("normalizations after the boundary = %d, want the committed value consumed without a second pass", normalized)
		}
		if seenName != "echo" {
			t.Fatalf("preparer name = %q, want the committed record's name", seenName)
		}
		if string(seen) != `{"x":1}` {
			t.Fatalf("preparer arguments = %q, want byte-identical committed bytes", seen)
		}
		after := committedAssistant(t, store, sessionID)
		if string(after.NormalizedArguments) != string(before.NormalizedArguments) {
			t.Fatalf("assistant normalized member mutated to %q", after.NormalizedArguments)
		}
		if raw, err := base64.StdEncoding.DecodeString(after.ArgumentsBase64); err != nil || string(raw) != ` {"x": 1} ` {
			t.Fatalf("assistant raw arguments mutated to %q (%v)", raw, err)
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
		if normalized != 1 {
			t.Fatalf("producer normalizations = %d, want one", normalized)
		}
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
		got, err := h.toolEffect(c, testOpID, Execution{Tool: plan, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if got.Status != model.ResultSuccess || got.Content != "ran" {
			t.Fatalf("result = %+v, want the executed outcome", got)
		}
		if replaces != 3 { // Operation + Session + the tool intent
			t.Fatalf("%d register replacements, want the intent-backed 3", replaces)
		}
		if normalized != 1 {
			t.Fatalf("normalizations = %d, want the committed value consumed through intent without a second pass", normalized)
		}
		if string(seen) != `{"x":1}` {
			t.Fatalf("preparer arguments = %q, want the committed normalized bytes, not the raw %q", seen, ` {"x": 1} `)
		}
		graph, err := validateFixture(t, store, sessionID)
		if err != nil {
			t.Fatalf("graph: %v", err)
		}
		results := 0
		for _, entry := range graph.Entries {
			if entry.ToolResult != nil {
				results++
				if entry.ToolResult.Status != model.ResultSuccess || entry.ToolResult.Content != "ran" {
					t.Fatalf("committed result = %+v, want the executor's outcome", *entry.ToolResult)
				}
			}
		}
		if results != 1 {
			t.Fatalf("%d tool results committed, want the one executor outcome", results)
		}
	})

	t.Run("invalid original re-normalizes once at the boundary for its diagnostic", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		normalized := 0
		normalize := func(call model.ToolCall) (json.RawMessage, error) {
			normalized++
			return objectNormalize(call)
		}
		publishWithNormalize(t, h, c, normalize, model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`not json`)})
		if normalized != 1 {
			t.Fatalf("producer normalizations = %d, want one failed attempt", normalized)
		}
		if len(committedAssistant(t, store, sessionID).NormalizedArguments) != 0 {
			t.Fatalf("committed member present for an invalid original")
		}
		prepared := 0
		plan := func(context.Context, model.ToolCall) PreparedTool {
			prepared++
			return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "done"}}}
		}
		got, err := h.toolEffect(c, testOpID, Execution{Tool: plan, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if normalized != 2 {
			t.Fatalf("normalizations = %d, want one more at the boundary for the diagnostic", normalized)
		}
		if got.Status != model.ResultError || got.Content != "arguments must be one non-null JSON object" {
			t.Fatalf("result = %+v, want the callback's useful validation diagnostic", got)
		}
		if prepared != 0 {
			t.Fatalf("preparations = %d, want none for an invalid original", prepared)
		}
	})

	t.Run("oversized diagnostic is bounded", func(t *testing.T) {
		h, _, c, _ := newEffectHarness(t, nil)
		diagnostic := "x" + strings.Repeat("y", 2*maxToolDiagnosticBytes)
		normalize := func(model.ToolCall) (json.RawMessage, error) {
			return nil, errors.New(diagnostic)
		}
		publishWithNormalize(t, h, c, normalize, model.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`not json`)})
		got, err := h.toolEffect(c, testOpID, Execution{Tool: func(context.Context, model.ToolCall) PreparedTool {
			t.Fatalf("preparer must not run for an invalid original")
			return PreparedTool{}
		}, NormalizeTool: normalize}, testCapture())(context.Background(), testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if got.Status != model.ResultError || len(got.Content) > maxToolDiagnosticBytes || !strings.HasPrefix(got.Content, "xyyyyy") {
			t.Fatalf("result = %+v, want the diagnostic truncated within %d bytes", got, maxToolDiagnosticBytes)
		}
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
		got, err := h.toolEffect(c, testOpID, Execution{Tool: func(context.Context, model.ToolCall) PreparedTool {
			t.Fatalf("preparer must not run for a malformed callback outcome")
			return PreparedTool{}
		}, NormalizeTool: boundaryNormalize}, testCapture())(context.Background(), testToolCall("call-1"))
		if err != nil {
			t.Fatalf("tool effect: %v", err)
		}
		if normalized != 2 {
			t.Fatalf("normalizations = %d, want one producer failure and one boundary failure", normalized)
		}
		if got.Status != model.ResultError || got.Content != invalidToolResultContent {
			t.Fatalf("result = %+v, want the fixed internal-validation error", got)
		}
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
			got, err := h.toolEffect(c, testOpID, effectExecution(nil, plan), testCapture())(context.Background(), testToolCall("call-1"))
			if err != nil {
				t.Fatalf("tool effect: %v", err)
			}
			want := model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}
			if got != want {
				t.Fatalf("agent-facing result = %+v, want only the plain Result", got)
			}
			graph, err := validateFixture(t, store, sessionID)
			if err != nil {
				t.Fatalf("graph after the tool effect: %v", err)
			}
			var result *toolResultEntry
			for i := range graph.Entries {
				if graph.Entries[i].ToolResult != nil {
					result = graph.Entries[i].ToolResult
				}
			}
			if result == nil {
				t.Fatalf("no tool result committed, want the result to survive any metadata decision")
			}
			if result.Status != model.ResultSuccess || result.Content != "ran" {
				t.Fatalf("committed result = %+v, want the settled result", result)
			}
			if tc.stored == "" {
				if len(result.Metadata) != 0 {
					t.Fatalf("committed metadata = %s, want absent", result.Metadata)
				}
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
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, spy.tool)
	if err := h.execute(c, testOpID, prepared, h.ctx); err != nil {
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
	if err := h.execute(c, testOpID, prepared, h.ctx); err != nil {
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
	if err := h.execute(c, testOpID, prepared, h.ctx); err != nil {
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
	if err := h.execute(c, testOpID, prepared, h.ctx); err != nil {
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
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return erroredTurnStream(), nil
	}
	h, store, c, sessionID, prepared, _ := newExecutionHarness(t, modelFn, func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "done"}}}
	})
	if err := h.execute(c, testOpID, prepared, h.ctx); err != nil {
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
// coordinator view: the next contextSource projection carries it as its
// model.RoleTool message, and a second tool effect in the same Operation sees
// the first one committed.
func TestToolResultAdoptsIntoView(t *testing.T) {
	h, _, c, sessionID := newEffectHarness(t, nil)
	publishCalls(t, h, c, sessionID, testToolCall("call-1"), testToolCall("call-2"))
	source := h.contextSource(c, testOpID)
	immediate := func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran " + call.ID}}}
	}
	if _, err := h.toolEffect(c, testOpID, effectExecution(nil, immediate), testCapture())(context.Background(), testToolCall("call-1")); err != nil {
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
	if _, err := h.toolEffect(c, testOpID, effectExecution(nil, immediate), testCapture())(context.Background(), testToolCall("call-2")); err != nil {
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
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		return completedTurnStream(calls...), nil
	}
	if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, effectExecution(modelFn, nil), testCapture()), nil); err != nil {
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
		err := h.execute(c, testOpID, prepared, h.ctx)
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
	got, err := h.toolEffect(c, testOpID, effectExecution(nil, spy.tool), testCapture())(ctx, testToolCall("call-1"))
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
	if _, err := invokeModelEffect(t, h.modelEffect(c, testOpID, Execution{Model: modelFn, NormalizeTool: normalize}, testCapture()), nil); err != nil {
		t.Fatalf("model effect: %v", err)
	}
	if normalized != 3 {
		t.Fatalf("normalization callback ran %d times, want exactly one per advertised completed call", normalized)
	}
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("graph after the model effect: %v", err)
	}
	for _, entry := range graph.Entries {
		if entry.Assistant == nil || len(entry.Assistant.ToolCalls) != 4 {
			continue
		}
		calls := entry.Assistant.ToolCalls
		if raw, err := base64.StdEncoding.DecodeString(calls[0].ArgumentsBase64); err != nil || string(raw) != string(valid) {
			t.Fatalf("call-1 raw arguments = %q err %v, want the exact caller bytes", raw, err)
		}
		if string(calls[0].NormalizedArguments) != `{"x":1}` {
			t.Fatalf("call-1 normalized arguments = %q, want the successful callback object", calls[0].NormalizedArguments)
		}
		if raw, err := base64.StdEncoding.DecodeString(calls[1].ArgumentsBase64); err != nil || string(raw) != string(malformed) {
			t.Fatalf("call-2 raw arguments = %q err %v, want the exact malformed bytes preserved", raw, err)
		}
		if len(calls[1].NormalizedArguments) != 0 {
			t.Fatalf("call-2 normalized arguments = %q, want the member absent for a failed normalization", calls[1].NormalizedArguments)
		}
		if len(calls[2].NormalizedArguments) != 0 {
			t.Fatalf("call-3 normalized arguments = %q, want the member absent for a malformed callback result", calls[2].NormalizedArguments)
		}
		if len(calls[3].NormalizedArguments) != 0 {
			t.Fatalf("call-4 normalized arguments = %q, want the member absent for an unadvertised name", calls[3].NormalizedArguments)
		}
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
	if err := h.execute(c, testOpID, prepared, h.ctx); err != nil {
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
	if err := h.execute(c, testOpID, prepared, h.ctx); err != nil {
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
	for call, got := range byCall {
		if len(got.Metadata) != 0 {
			t.Fatalf("%s result carries metadata %s, want none on synthetic results", call, got.Metadata)
		}
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
		if !g.replaceOperation("op-2", record("op-2", 42)) {
			t.Fatalf("replaceOperation of a carried identity = false, want true")
		}
		want := []OperationRecord{record("op-1", 1), record("op-2", 42), record("op-3", 3)}
		if !reflect.DeepEqual(g.Operations, want) {
			t.Fatalf("operations after replacement = %+v, want %+v", g.Operations, want)
		}
	})

	t.Run("missing identity is a no-op", func(t *testing.T) {
		g := &sessionGraph{Operations: []OperationRecord{record("op-1", 1), record("op-2", 2), record("op-3", 3)}}
		if g.replaceOperation("op-ghost", record("op-ghost", 9)) {
			t.Fatalf("replaceOperation of an absent identity = true, want false")
		}
		want := []OperationRecord{record("op-1", 1), record("op-2", 2), record("op-3", 3)}
		if !reflect.DeepEqual(g.Operations, want) {
			t.Fatalf("operations after a missing identity = %+v, want the untouched records %+v", g.Operations, want)
		}
	})

	t.Run("replacement allocates nothing", func(t *testing.T) {
		const count = 64
		operations := make([]OperationRecord, count) // all 64 unique records are prepared outside the measured closure
		for i := range operations {
			operations[i] = record(fmt.Sprintf("op-%02d", i), int64(i))
		}
		g := &sessionGraph{Operations: operations}
		updated := record("op-63", 999)
		if n := testing.AllocsPerRun(100, func() { g.replaceOperation("op-63", updated) }); n != 0 {
			t.Fatalf("one-record replacement over %d operations allocated %v times, want zero", count, n)
		}
	})
}
