// External-package public Harness suite: every fixture drives the landed
// public API only — New, CreateSession, Submit, ReadSession, ReadOperation,
// ChangeAgentType, and Wait — over both storage implementations, covering the
// preparation, admission, idempotency, context, effect, settlement, usage,
// terminal, coordination, and lifetime rows through public operations.
package harness_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/model"
)

var publicModelRef = model.ModelRef{Provider: "prov", Model: "gpt-x"}

// publicCapture is the fixed prepared capture of the suite.
func publicCapture() harness.ExecutionCapture {
	return harness.ExecutionCapture{
		ConfigurationRevision: "rev-1",
		Model:                 publicModelRef,
		SystemPrompt:          "system",
		Tools: []model.ToolDefinition{{
			Name:        "echo",
			Description: "echoes",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	}
}

// publicStream is one fake accepted model stream yielding a completed text
// response, for the suite's real assembly callback.
type publicStream struct{ i int }

func (s *publicStream) Recv() (model.StreamDelta, error) {
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

func (s *publicStream) Close() error { return nil }

// scriptModel is the scripted model effect of the suite: it records every
// projected request, signals each arrival, optionally parks every invocation
// until its gate closes, assembles once behind every output-bearing
// settlement, and answers from a scripted settlement list.
type scriptModel struct {
	mu          sync.Mutex
	requests    []model.Request
	arrived     chan struct{}
	gate        chan struct{}
	settlements []agent.ModelSettlement
}

func newScriptModel(settlements ...agent.ModelSettlement) *scriptModel {
	return &scriptModel{arrived: make(chan struct{}, 16), settlements: settlements}
}

func (s *scriptModel) effect(_ context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	gate := s.gate
	s.mu.Unlock()
	s.arrived <- struct{}{}
	if gate != nil {
		<-gate
	}
	var set agent.ModelSettlement
	if len(s.settlements) > 0 {
		set = s.settlements[0]
		s.settlements = s.settlements[1:]
	} else {
		set = agent.ModelSettlement{Disposition: agent.DispoReady, Output: publicCompleted(nil)}
	}
	if set.Output != nil { // an output-bearing settlement requires exactly one assembly
		if _, err := assemble(publicModelRef, &publicStream{}); err != nil {
			return agent.ModelSettlement{}, err
		}
	}
	return set, nil
}

func (s *scriptModel) seen() []model.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Request(nil), s.requests...)
}

// texts returns one request's projected message texts.
func texts(req model.Request) []string {
	out := make([]string, 0, len(req.Messages))
	for _, msg := range req.Messages {
		out = append(out, msg.TextContent())
	}
	return out
}

func (s *scriptModel) lastTexts() []string {
	reqs := s.seen()
	return texts(reqs[len(reqs)-1])
}

// releaseGate unblocks every parked invocation and stops parking later ones.
func (s *scriptModel) releaseGate() {
	s.mu.Lock()
	gate := s.gate
	s.gate = nil
	s.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func publicCompleted(usage *model.Usage) *model.Output {
	return &model.Output{
		Status: model.OutputCompleted,
		Source: publicModelRef,
		Message: &model.Message{
			Role:    model.RoleAssistant,
			Source:  publicModelRef,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "done"}},
		},
		Usage: usage,
	}
}

// publicFixture wires one Harness over the given store with a scripted
// preparation callback: every admission receives the fixture's prepared
// execution (or the per-call prepare hook's result) and signals the
// preparation arrival. The model effect defaults to the script's own effect.
type publicFixture struct {
	h       *harness.Harness
	store   harness.Storage
	cancel  context.CancelFunc
	prepare chan struct{} // signaled per preparation call

	prepMu sync.Mutex
	preps  []harness.PreparationRequest

	model *scriptModel

	// prepareHook, when non-nil, answers one preparation call with its own
	// result or error; the call index counts from zero.
	prepareHook func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error)
}

func newPublicFixture(t *testing.T, store harness.Storage, script *scriptModel, modelFn agent.ModelEffect) *publicFixture {
	t.Helper()
	if modelFn == nil {
		modelFn = script.effect
	}
	f := &publicFixture{store: store, cancel: func() {}, prepare: make(chan struct{}, 16), model: script}
	prepared := harness.PreparedExecution{
		Capture: publicCapture(),
		Model:   modelFn,
		Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
			return harness.PreparedTool{Immediate: &model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"}}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	h, err := harness.New(ctx, harness.Dependencies{Storage: store, Prepare: func(_ context.Context, req harness.PreparationRequest) (harness.PreparedExecution, error) {
		f.prepMu.Lock()
		call := len(f.preps)
		f.preps = append(f.preps, req)
		hook := f.prepareHook
		f.prepMu.Unlock()
		f.prepare <- struct{}{}
		if hook != nil {
			return hook(call, req)
		}
		return prepared, nil
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.h = h
	return f
}

func (f *publicFixture) preparationCalls() []harness.PreparationRequest {
	f.prepMu.Lock()
	defer f.prepMu.Unlock()
	return append([]harness.PreparationRequest(nil), f.preps...)
}

func (f *publicFixture) close() { f.cancel() }

func createSession(t *testing.T, h *harness.Harness) string {
	t.Helper()
	session, err := h.CreateSession(context.Background(), harness.CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return session.Identity.SessionID
}

func submit(t *testing.T, h *harness.Harness, sessionID, operationID string, mode harness.MessageMode, text string) (harness.SubmitResult, error) {
	t.Helper()
	return h.Submit(context.Background(), harness.SubmitRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Origin:      harness.InputOriginUser,
		Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
		Mode:        mode,
	})
}

// converge cancels the Harness context and joins every in-flight execution
// through Wait: the deterministic end-of-fixture barrier.
func converge(t *testing.T, f *publicFixture) error {
	t.Helper()
	f.cancel()
	return f.h.Wait(context.Background())
}

// eachStore runs one fixture against memory and a temporary SQLite store.
func eachStore(t *testing.T, run func(t *testing.T, store harness.Storage)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		run(t, storage.NewMemory())
	})
	t.Run("sqlite", func(t *testing.T) {
		store, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "lightcode.db"))
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		defer store.Close()
		run(t, store)
	})
}

// TestPublicTurnLifecycle proves the preparation, admission, context, effect,
// settlement, usage, and terminal rows through public operations: one
// preparation per admission with the owned Session view, the published running
// record, the projected context at the model boundary, one committed turn with
// usage on both totals, the success terminal, and the queued item the
// post-terminal drain admits — which is itself the proof that the drain
// publishes only after the terminal commit.
func TestPublicTurnLifecycle(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel(
			agent.ModelSettlement{Disposition: agent.DispoReady, Output: publicCompleted(&model.Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2})},
			agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "second turn failed"},
		)
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)

		res, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
		if err != nil || res.Disposition != harness.DispositionAdmitted || res.Operation == nil {
			t.Fatalf("first submit = %+v err %v, want admitted", res, err)
		}
		rec := res.Operation
		if rec.State.Status != harness.OperationRunning || rec.Admission.AgentType != "coder" ||
			rec.Admission.Execution.ConfigurationRevision != "rev-1" || rec.Admission.Execution.Model != publicModelRef {
			t.Fatalf("admitted record = %+v", rec)
		}
		<-script.arrived // parked at the first model boundary: the Session is active
		if got := texts(script.seen()[0]); got[0] != "system" || got[1] != "hello" || len(got) != 2 {
			t.Fatalf("first projection = %v, want the captured system prompt and the admitted input", got)
		}
		if queued, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1"); err != nil ||
			queued.Disposition != harness.DispositionQueued || queued.Operation != nil {
			t.Fatalf("active queued submit = %+v err %v, want queued", queued, err)
		}

		script.releaseGate()
		<-f.prepare // the drain admitted the queued item: op-1's terminal committed
		<-script.arrived
		if got := texts(script.seen()[1]); got[0] != "system" || got[1] != "hello" || got[2] != "done" || got[3] != "queued-1" {
			t.Fatalf("second projection = %v, want the first turn's committed history before the queued input", got)
		}
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		first, err := f.h.ReadOperation(context.Background(), session, "op-1")
		if err != nil || first.State.Status != harness.OperationSuccess || first.State.Terminal == nil {
			t.Fatalf("first operation = %+v err %v, want terminal success", first, err)
		}
		want := harness.UsageTotals{ByModel: []harness.ModelUsage{{
			Model: publicModelRef,
			Usage: harness.UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2},
		}}}
		if len(first.State.Usage.ByModel) != 1 || first.State.Usage.ByModel[0] != want.ByModel[0] {
			t.Fatalf("operation usage = %+v, want the reported counts", first.State.Usage)
		}
		sessionRec, err := f.h.ReadSession(context.Background(), session)
		if err != nil || sessionRec.State.CurrentOperationID != "" {
			t.Fatalf("session after convergence = %+v err %v", sessionRec, err)
		}
		if len(sessionRec.State.Usage.ByModel) != 1 || sessionRec.State.Usage.ByModel[0] != want.ByModel[0] {
			t.Fatalf("session usage = %+v, want the same totals", sessionRec.State.Usage)
		}
		second, err := f.h.ReadOperation(context.Background(), session, "op-2")
		if err != nil || second.State.Status != harness.OperationFailure ||
			second.State.Terminal == nil || second.State.Terminal.Detail != "second turn failed" {
			t.Fatalf("drained operation = %+v err %v, want terminal failure with its detail", second, err)
		}
		calls := f.preparationCalls()
		if len(calls) != 2 || calls[0].Session.AgentType != "coder" || calls[1].Session.AgentType != "coder" {
			t.Fatalf("preparation calls = %+v, want one per admission with the Session type", calls)
		}
	})
}

// TestPublicSteeringContinuation proves the coordination row through public
// operations: regular input submitted while an Operation is active enters the
// steering buffer, and the same Operation continues across the model boundary
// with the steering projected before the next request.
func TestPublicSteeringContinuation(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)

		if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived // parked at the first model boundary
		if steered, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "steer"); err != nil ||
			steered.Disposition != harness.DispositionSteering || steered.Operation != nil {
			t.Fatalf("steering submit = %+v err %v, want steering", steered, err)
		}
		if queued, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "queued-1"); err != nil ||
			queued.Disposition != harness.DispositionQueued {
			t.Fatalf("queued submit = %+v err %v, want queued", queued, err)
		}
		script.releaseGate()
		<-script.arrived
		if got := texts(script.seen()[1]); got[0] != "system" || got[1] != "hello" || got[2] != "done" || got[3] != "steer" {
			t.Fatalf("continuation projection = %v, want the drained steering in the same Operation", got)
		}
		<-f.prepare // the drain admitted the queued item: op-1's terminal committed
		<-script.arrived
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
		if err != nil || rec.State.Status != harness.OperationSuccess {
			t.Fatalf("steered operation = %+v err %v, want success", rec, err)
		}
	})
}

// TestPublicIdempotency proves the idempotency row through public operations:
// an admitted identity resolves to the first Operation before routing — even
// with changed content and in both modes — and reuse in another Session is
// invalid.
func TestPublicIdempotency(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)

		first, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "first")
		if err != nil || first.Disposition != harness.DispositionAdmitted || first.Operation == nil {
			t.Fatalf("first submit = %+v err %v, want admitted", first, err)
		}
		<-script.arrived // the Operation stays active

		retry, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "changed")
		if err != nil || retry.Disposition != harness.DispositionExisting || retry.Operation == nil ||
			retry.Operation.Admission.AdmittedEntry != first.Operation.Admission.AdmittedEntry {
			t.Fatalf("active retry = %+v err %v, want the first record", retry, err)
		}
		queuedRetry, err := submit(t, f.h, session, "op-1", harness.MessageModeQueued, "changed-again")
		if err != nil || queuedRetry.Disposition != harness.DispositionExisting || queuedRetry.Operation == nil {
			t.Fatalf("queued retry = %+v err %v, want existing before routing", queuedRetry, err)
		}
		other := createSession(t, f.h)
		if _, err := submit(t, f.h, other, "op-1", harness.MessageModeRegular, "reused"); err == nil {
			t.Fatalf("cross-session reuse succeeded, want an error")
		}

		script.releaseGate()
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if _, err := f.h.ReadOperation(context.Background(), other, "op-1"); err == nil {
			t.Fatalf("cross-session reuse published an operation, want nothing")
		}
	})
}

// TestPublicAgentTypeChange proves the Agent-type row through public
// operations: the change during a running Operation succeeds without touching
// the active capture, and the drained queued admission resolves the
// Session's then-current type.
func TestPublicAgentTypeChange(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel(
			agent.ModelSettlement{Disposition: agent.DispoReady, Output: publicCompleted(nil)},    // op-1 succeeds
			agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "drained turn failed"}, // op-2 fails
		)
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)

		if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived
		if _, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1"); err != nil {
			t.Fatalf("queued submit: %v", err)
		}
		changed, err := f.h.ChangeAgentType(context.Background(), session, "reviewer")
		if err != nil || changed.State.CurrentAgentType != "reviewer" {
			t.Fatalf("ChangeAgentType = %+v err %v, want success during the blocked effect", changed, err)
		}
		script.releaseGate()

		<-f.prepare // the drained admission prepared again
		<-script.arrived
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		calls := f.preparationCalls()
		if len(calls) != 2 || calls[0].Session.AgentType != "coder" || calls[1].Session.AgentType != "reviewer" {
			t.Fatalf("preparation agent types = %+v, want the capture-time value then the changed one", calls)
		}
		first, err := f.h.ReadOperation(context.Background(), session, "op-1")
		if err != nil || first.Admission.AgentType != "coder" || first.State.Status != harness.OperationSuccess {
			t.Fatalf("first operation = %+v err %v, want the immutable capture and success", first, err)
		}
	})
}

// TestPublicHarnessLifetime proves the lifetime row through public operations:
// Wait begun before cancellation returns only after the blocked execution and
// both buffers have converged, cancellation closes admission, and Harness loss
// discards the steering and queued buffers without delivery attempts.
func TestPublicHarnessLifetime(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		session := createSession(t, f.h)

		if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived // parked inside the model effect
		if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "steer"); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		if _, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "queued-1"); err != nil {
			t.Fatalf("queued submit: %v", err)
		}

		waited := make(chan error, 1)
		go func() { waited <- f.h.Wait(context.Background()) }() // Wait begins before cancellation
		f.cancel()                                               // Harness loss closes admission and ends the run
		script.releaseGate()
		if err := <-waited; err != nil {
			t.Fatalf("Wait: %v", err)
		}

		if _, err := submit(t, f.h, session, "op-4", harness.MessageModeRegular, "late"); err == nil {
			t.Fatalf("submit after cancellation succeeded, want admission closed")
		}
		rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
		if err != nil || rec.State.Status != harness.OperationInterruption {
			t.Fatalf("converged operation = %+v err %v, want interruption", rec, err)
		}
		for _, id := range []string{"op-2", "op-3"} {
			if _, err := f.h.ReadOperation(context.Background(), session, id); err == nil {
				t.Fatalf("buffered item %q became an operation, want the buffers discarded", id)
			}
		}
		calls := f.preparationCalls()
		if len(calls) != 1 {
			t.Fatalf("preparation calls = %d, want no buffered delivery attempts after Harness loss", len(calls))
		}
	})
}

// TestPublicWaitOwnContextCancelsOnlyTheWait proves a canceled wait context
// ends the wait with its own error while the Harness stays live.
func TestPublicWaitOwnContextCancelsOnlyTheWait(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		f := newPublicFixture(t, store, newScriptModel(), nil)
		defer f.close()
		waitCtx, cancelWait := context.WithCancel(context.Background())
		cancelWait()
		done := make(chan error, 1)
		go func() { done <- f.h.Wait(waitCtx) }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("Wait over a canceled wait context returned nil, want its error")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Wait ignored its own context cancellation")
		}
	})
}

// TestPublicBufferedItemFailure proves the buffer-lifetime row through public
// operations: a failed delivery attempt — failed preparation or a failed
// delivered Operation — is final for the item, and the next buffered message
// proceeds without retention or retry.
func TestPublicBufferedItemFailure(t *testing.T) {
	t.Run("failed preparation drops the item and proceeds", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(
				agent.ModelSettlement{Disposition: agent.DispoReady, Output: publicCompleted(nil)},  // op-1 succeeds
				agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "next item settled"}, // q2 settles through its own terminal
			)
			gate := make(chan struct{})
			script.gate = gate
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			prepared := harness.PreparedExecution{
				Capture: publicCapture(),
				Model:   script.effect,
				Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
					return harness.PreparedTool{Immediate: &model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"}}
				},
			}
			f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
				if call == 1 { // the first drained queued item's preparation fails
					return harness.PreparedExecution{}, fmt.Errorf("preparation broke")
				}
				return prepared, nil
			}
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare      // the first admission prepared
			<-script.arrived // parked: the Session is active
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "q1"); err != nil {
				t.Fatalf("queued submit 1: %v", err)
			}
			if _, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "q2"); err != nil {
				t.Fatalf("queued submit 2: %v", err)
			}
			script.releaseGate()

			<-f.prepare // the failed item was dropped and the next one admitted
			<-script.arrived
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if _, err := f.h.ReadOperation(context.Background(), session, "op-2"); err == nil {
				t.Fatalf("failed item's operation exists, want nothing retained")
			}
			rec, err := f.h.ReadOperation(context.Background(), session, "op-3")
			if err != nil || rec.State.Status != harness.OperationFailure ||
				rec.State.Terminal == nil || rec.State.Terminal.Detail != "next item settled" {
				t.Fatalf("next buffered message = %+v err %v, want admitted and settled", rec, err)
			}
		})
	})

	t.Run("failed delivered operation proceeds to the next item", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(
				agent.ModelSettlement{Disposition: agent.DispoReady, Output: publicCompleted(nil)},   // op-1 succeeds
				agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "queued turn failed"}, // q1 fails
				agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "next item settled"},  // q2 settles through its own terminal
			)
			gate := make(chan struct{})
			script.gate = gate
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			session := createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare      // the first admission prepared
			<-script.arrived // parked: the Session is active
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "q1"); err != nil {
				t.Fatalf("queued submit 1: %v", err)
			}
			if _, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "q2"); err != nil {
				t.Fatalf("queued submit 2: %v", err)
			}
			script.releaseGate()

			<-f.prepare // the drain admitted q1 over op-1's terminal
			<-script.arrived
			<-f.prepare // q1's failed terminal let the drain admit the next item
			<-script.arrived
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			failed, err := f.h.ReadOperation(context.Background(), session, "op-2")
			if err != nil || failed.State.Status != harness.OperationFailure ||
				failed.State.Terminal == nil || failed.State.Terminal.Detail != "queued turn failed" {
				t.Fatalf("failed delivered operation = %+v err %v, want terminal failure", failed, err)
			}
			rec, err := f.h.ReadOperation(context.Background(), session, "op-3")
			if err != nil || rec.State.Status != harness.OperationFailure ||
				rec.State.Terminal == nil || rec.State.Terminal.Detail != "next item settled" {
				t.Fatalf("next buffered message = %+v err %v, want admitted and settled", rec, err)
			}
		})
	})
}

// TestPublicFinalBoundarySerialization proves the coordinator linearizes the
// final model-result boundary against submissions through public operations:
// input submitted inside the model callback while the Operation is still
// current is never stranded — regular input continues the Operation, queued
// input drains after the terminal commit.
func TestPublicFinalBoundarySerialization(t *testing.T) {
	t.Run("regular input at the boundary continues the operation", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(
				agent.ModelSettlement{Disposition: agent.DispoReady, Output: publicCompleted(nil)}, // the boundary result commits; steering continues it
				agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "boundary settled"}, // model-originated terminal
			)
			var (
				f       *publicFixture
				session string
				first   = true
			)
			inner := script.effect
			boundary := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
				if first { // submit at the model-result boundary, before the settlement returns
					first = false
					if _, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "at-boundary"); err != nil {
						t.Errorf("boundary submit: %v", err)
					}
				}
				return inner(ctx, req, assemble)
			}
			f = newPublicFixture(t, store, script, boundary)
			defer f.close()
			session = createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-script.arrived
			<-script.arrived // the boundary steering continued the same Operation
			if got := texts(script.seen()[1]); got[0] != "system" || got[1] != "hello" || got[2] != "done" || got[3] != "at-boundary" {
				t.Fatalf("continuation projection = %v, want the boundary steering in the same Operation", got)
			}
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
			if err != nil || rec.State.Status != harness.OperationFailure ||
				rec.State.Terminal == nil || rec.State.Terminal.Detail != "boundary settled" {
				t.Fatalf("operation = %+v err %v, want the boundary steering to continue it to its terminal", rec, err)
			}
			if _, err := f.h.ReadOperation(context.Background(), session, "op-2"); err == nil {
				t.Fatalf("boundary steering became an Operation, want it owned by the running one")
			}
		})
	})

	t.Run("queued input at the boundary drains after the terminal", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(
				agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "first settled"},        // model-originated terminal before the drain
				agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "boundary turn failed"}, // the drained item's own terminal
			)
			var (
				f       *publicFixture
				session string
				first   = true
			)
			inner := script.effect
			boundary := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
				if first { // defer one queued message at the final model-result boundary
					first = false
					if _, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "at-boundary"); err != nil {
						t.Errorf("boundary submit: %v", err)
					}
				}
				return inner(ctx, req, assemble)
			}
			f = newPublicFixture(t, store, script, boundary)
			defer f.close()
			session = createSession(t, f.h)

			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-script.arrived
			<-f.prepare // the drain admitted the boundary item after the terminal commit
			<-script.arrived
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			firstRec, err := f.h.ReadOperation(context.Background(), session, "op-1")
			if err != nil || firstRec.State.Status != harness.OperationFailure ||
				firstRec.State.Terminal == nil || firstRec.State.Terminal.Detail != "first settled" {
				t.Fatalf("first operation = %+v err %v, want its terminal before the drain", firstRec, err)
			}
			rec, err := f.h.ReadOperation(context.Background(), session, "op-2")
			if err != nil || rec.State.Status != harness.OperationFailure {
				t.Fatalf("drained operation = %+v err %v, want the boundary queued input admitted after the terminal", rec, err)
			}
		})
	})
}

// TestPublicSessionsExecuteConcurrently proves the coordination row's
// concurrency axis through public operations: a second Session reaches its
// model boundary and settles while the first Session's execution stays parked
// — no global execution lane.
func TestPublicSessionsExecuteConcurrently(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		parked := newScriptModel(
			agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "first session settled"}, // model-originated terminal: durable regardless of later cancellation
		)
		gate := make(chan struct{})
		parked.gate = gate
		finished := newScriptModel(
			agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "second session settled"}, // model-originated terminal: durable regardless of later cancellation
		)
		parkedEffect, finishedEffect := parked.effect, finished.effect
		route := func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
			got := texts(req)
			if got[len(got)-1] == "first" {
				return parkedEffect(ctx, req, assemble)
			}
			return finishedEffect(ctx, req, assemble)
		}
		f := newPublicFixture(t, store, parked, route)
		defer f.close()
		sessionA := createSession(t, f.h)
		sessionB := createSession(t, f.h)

		if _, err := submit(t, f.h, sessionA, "op-a", harness.MessageModeRegular, "first"); err != nil {
			t.Fatalf("first session submit: %v", err)
		}
		<-f.prepare      // the first admission prepared
		<-parked.arrived // the first Session is parked at its model boundary

		if _, err := submit(t, f.h, sessionB, "op-b", harness.MessageModeRegular, "second"); err != nil {
			t.Fatalf("second session submit: %v", err)
		}
		<-f.prepare          // the second Session was admitted while the first is parked
		<-finished.arrived   // and reached its model boundary before the first one resumed
		parked.releaseGate() // the first Session settles through its own model-originated terminal
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		doneRec, err := f.h.ReadOperation(context.Background(), sessionB, "op-b")
		if err != nil || doneRec.State.Status != harness.OperationFailure ||
			doneRec.State.Terminal == nil || doneRec.State.Terminal.Detail != "second session settled" {
			t.Fatalf("second session = %+v err %v, want it settled while the first was parked", doneRec, err)
		}
		rec, err := f.h.ReadOperation(context.Background(), sessionA, "op-a")
		if err != nil || rec.State.Status != harness.OperationFailure ||
			rec.State.Terminal == nil || rec.State.Terminal.Detail != "first session settled" {
			t.Fatalf("first session = %+v err %v, want its model-originated terminal after its release", rec, err)
		}
	})
}

// opRegisterWire mirrors the durable operation-register members the suite
// observes while an effect runs; the payload stays opaque JSON on the public
// storage contract.
type opRegisterWire struct {
	State struct {
		ActiveEffect *struct {
			Kind          string `json:"kind"`
			ResultEntryID string `json:"result_entry_id"`
			ToolCallID    string `json:"tool_call_id"`
		} `json:"active_effect"`
	} `json:"state"`
}

// TestPublicOrderedToolCallsSettle proves the effect, tool-settlement,
// context, and terminal rows through public operations: a Submit auto-started
// turn dispatches its completed calls in order, an executor-backed call
// publishes its tool intent before execution and one result after it, an
// immediate call settles without an intent, the next model request projects
// both committed results, and the Operation settles success — proven by the
// queued item the post-terminal drain admits.
func TestPublicOrderedToolCallsSettle(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel(
			agent.ModelSettlement{Disposition: agent.DispoReady, Output: publicCompletedWithCalls("call-1", "call-2")}, // the turn publishes two ordered calls
			agent.ModelSettlement{Disposition: agent.DispoReady, Output: publicCompleted(nil)},                         // the turn completes after the results
			agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "drained turn settled"},                     // the drained item's own terminal
		)
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		var (
			toolMu    sync.Mutex
			toolOrder []string
			session   string
		)
		prepared := harness.PreparedExecution{
			Capture: publicCapture(),
			Model:   script.effect,
			Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
				toolMu.Lock()
				toolOrder = append(toolOrder, call.ID)
				toolMu.Unlock()
				if call.ID == "call-1" { // executor-backed: intent before execution, one result after
					return harness.PreparedTool{Execute: func(context.Context) model.ToolResult {
						reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: session, Kind: harness.RegisterOperation, OperationID: "op-1"})
						if err != nil {
							t.Errorf("read operation register during execution: %v", err)
							return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call-1"}
						}
						var wire opRegisterWire
						if err := json.Unmarshal(reg.Payload, &wire); err != nil {
							t.Errorf("decode operation register during execution: %v", err)
							return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call-1"}
						}
						if wire.State.ActiveEffect == nil || wire.State.ActiveEffect.Kind != "tool" ||
							wire.State.ActiveEffect.ToolCallID != "call-1" || wire.State.ActiveEffect.ResultEntryID == "" {
							t.Errorf("tool intent during execution = %+v, want the committed call-1 intent with a reserved result", wire.State.ActiveEffect)
						}
						return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call-1"}
					}}
				}
				return harness.PreparedTool{Immediate: &model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "immediate call-2"}} // no effect intent
			},
		}
		f.prepareHook = func(int, harness.PreparationRequest) (harness.PreparedExecution, error) { return prepared, nil }

		session = createSession(t, f.h)
		res, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
		if err != nil || res.Disposition != harness.DispositionAdmitted {
			t.Fatalf("first submit = %+v err %v, want the auto-started admission", res, err)
		}
		<-script.arrived // the first model boundary
		if got := texts(script.seen()[0]); got[0] != "system" || got[1] != "hello" || len(got) != 2 {
			t.Fatalf("first projection = %v, want the system prompt and the admitted input", got)
		}
		if queued, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1"); err != nil || queued.Disposition != harness.DispositionQueued {
			t.Fatalf("queued submit = %+v err %v, want queued", queued, err)
		}

		<-script.arrived // the second request: both committed results project before it
		if got := texts(script.seen()[1]); len(got) != 5 || got[0] != "system" || got[1] != "hello" || got[2] != "done" ||
			got[3] != "ran call-1" || got[4] != "immediate call-2" {
			t.Fatalf("subsequent projection = %v, want both tool results in dispatch order", got)
		}
		<-f.prepare // the drain admitted the queued item: op-1's terminal success committed
		<-script.arrived
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
		if err != nil || rec.State.Status != harness.OperationSuccess || len(rec.State.PendingToolCalls) != 0 {
			t.Fatalf("tool turn = %+v err %v, want terminal success with every call settled", rec, err)
		}
		toolMu.Lock()
		defer toolMu.Unlock()
		if len(toolOrder) != 2 || toolOrder[0] != "call-1" || toolOrder[1] != "call-2" {
			t.Fatalf("dispatch order = %v, want the completed calls in published order", toolOrder)
		}
	})
}

// publicCompletedWithCalls builds one valid completed output carrying the
// given ordered tool calls.
func publicCompletedWithCalls(calls ...string) *model.Output {
	toolCalls := make([]model.ToolCall, 0, len(calls))
	for _, id := range calls {
		toolCalls = append(toolCalls, model.ToolCall{ID: id, Name: "echo", Arguments: json.RawMessage(`{"x":1}`)})
	}
	return &model.Output{
		Status: model.OutputCompleted,
		Source: publicModelRef,
		Message: &model.Message{
			Role:      model.RoleAssistant,
			Source:    publicModelRef,
			Content:   []model.ContentPart{{Kind: model.PartText, Text: "done"}},
			ToolCalls: toolCalls,
		},
	}
}
