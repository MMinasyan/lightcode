// External-package public Harness suite: every fixture drives the landed
// public API only — New, CreateSession, Submit, ReadSession, ReadOperation,
// ChangeAgentType, ReopenSession, ArchiveSession, DeleteSession, Sweep, and
// Wait — over both storage implementations, covering the preparation,
// admission, idempotency, context, effect, settlement, usage, terminal,
// coordination, lifetime, and lifecycle rows through public operations.
package harness_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
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
// with changed content and in both modes — reuse in another Session is
// invalid, and deleting the owning Session releases its Operation-ID storage
// constraint so the same ID is admitted in another Session afterwards.
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

		t.Run("reuse after delete", func(t *testing.T) {
			script := newScriptModel(
				agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "source turn settled"}, // model-originated terminal before the drain
				agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "reused turn settled"}, // durable regardless of later cancellation
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
				if call == 1 { // the drained queued item's preparation fails: the attempt is final for it
					return harness.PreparedExecution{}, fmt.Errorf("preparation broke")
				}
				return prepared, nil
			}
			source := createSession(t, f.h)

			if _, err := submit(t, f.h, source, "reuse-1", harness.MessageModeRegular, "first"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare      // the first admission prepared
			<-script.arrived // parked at the model boundary: reuse-1 is running
			if _, err := submit(t, f.h, source, "reuse-x", harness.MessageModeQueued, "queued"); err != nil {
				t.Fatalf("queued submit: %v", err)
			}
			script.releaseGate()
			<-f.prepare // barrier: the drain reached the queued item's preparation — reuse-1's terminal committed

			// the failed preparation is final for the item and the drain retires, leaving
			// the source Session idle; ArchiveSession's waitIdle joins that retirement
			if _, err := f.h.ArchiveSession(context.Background(), source); err != nil {
				t.Fatalf("archive: %v", err)
			}
			if err := f.h.DeleteSession(context.Background(), source); err != nil {
				t.Fatalf("delete: %v", err)
			}

			other := createSession(t, f.h)
			res, err := submit(t, f.h, other, "reuse-1", harness.MessageModeRegular, "reused")
			if err != nil || res.Disposition != harness.DispositionAdmitted || res.Operation == nil {
				t.Fatalf("reuse after delete = %+v err %v, want admitted in another session", res, err)
			}
			<-script.arrived // the reused ID's Operation reached its model boundary
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			rec, err := f.h.ReadOperation(context.Background(), other, "reuse-1")
			if err != nil || rec.State.Status != harness.OperationFailure ||
				rec.State.Terminal == nil || rec.State.Terminal.Detail != "reused turn settled" {
				t.Fatalf("reused operation = %+v err %v, want the model-originated terminal in the other session", rec, err)
			}
		})
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

// sessionRegister returns the raw durable Session register of one Session.
func sessionRegister(t *testing.T, store harness.Storage, sessionID string) harness.Register {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	if err != nil {
		t.Fatalf("read session register: %v", err)
	}
	return reg
}

// TestPublicSessionLifecycle proves the lifecycle row through public
// operations: the reopen/archive/delete state table with its timestamps and
// no-write successes, rejected preconditions leaving the durable state — and
// the process-local buffers — unchanged, post-delete absence for holders of
// the materialized coordinator and later materialization, and the resulting
// durable state at every transition.
func TestPublicSessionLifecycle(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()

		t.Run("state table, timestamps, and no-write successes", func(t *testing.T) {
			f := newPublicFixture(t, store, newScriptModel(), nil)
			defer f.close()
			session := createSession(t, f.h)
			first, err := f.h.ReadSession(ctx, session)
			if err != nil || first.State.Lifecycle != harness.LifecycleOpen || first.Revision != 1 {
				t.Fatalf("created session = %+v err %v, want open at revision 1", first, err)
			}
			originalActivity := first.State.LastActivity

			reopened, err := f.h.ReopenSession(ctx, session)
			if err != nil || reopened.State.Lifecycle != harness.LifecycleOpen || reopened.State.ArchivedAt != nil ||
				reopened.Revision != first.Revision || !reopened.State.LastActivity.Equal(originalActivity) {
				t.Fatalf("reopen of open = %+v err %v, want the no-write success", reopened, err)
			}

			archived, err := f.h.ArchiveSession(ctx, session)
			if err != nil || archived.State.Lifecycle != harness.LifecycleArchived || archived.Revision != first.Revision+1 {
				t.Fatalf("archive of open = %+v err %v, want archived at the next revision", archived, err)
			}
			if reg := sessionRegister(t, store, session); reg.Revision != archived.Revision {
				t.Fatalf("durable register revision %d disagrees with the returned record's %d", reg.Revision, archived.Revision)
			}
			if archived.State.ArchivedAt == nil || archived.State.ArchivedAt.IsZero() ||
				archived.State.ArchivedAt.Location() != time.UTC {
				t.Fatalf("archived_at = %v, want one sampled UTC time", archived.State.ArchivedAt)
			}
			if !archived.State.LastActivity.Equal(originalActivity) {
				t.Fatalf("archive moved last activity to %v, want it unchanged at %v", archived.State.LastActivity, originalActivity)
			}

			again, err := f.h.ArchiveSession(ctx, session)
			if err != nil || again.Revision != archived.Revision || !again.State.ArchivedAt.Equal(*archived.State.ArchivedAt) {
				t.Fatalf("archive of archived = %+v err %v, want the no-write success", again, err)
			}

			back, err := f.h.ReopenSession(ctx, session)
			if err != nil || back.State.Lifecycle != harness.LifecycleOpen || back.State.ArchivedAt != nil ||
				back.Revision != archived.Revision+1 {
				t.Fatalf("reopen of archived = %+v err %v, want open with the archived time cleared", back, err)
			}
			if reg := sessionRegister(t, store, session); reg.Revision != back.Revision {
				t.Fatalf("durable register revision %d disagrees with the returned record's %d", reg.Revision, back.Revision)
			}
			if !back.State.LastActivity.After(originalActivity) {
				t.Fatalf("reopen left last activity at %v, want it advanced past %v", back.State.LastActivity, originalActivity)
			}
			if back.Identity != first.Identity || back.State.CurrentAgentType != "coder" {
				t.Fatalf("reopen changed identity or Agent type: %+v", back)
			}

			againOpen, err := f.h.ReopenSession(ctx, session)
			if err != nil || againOpen.Revision != back.Revision || !againOpen.State.LastActivity.Equal(back.State.LastActivity) {
				t.Fatalf("second reopen = %+v err %v, want the no-write success", againOpen, err)
			}
		})

		t.Run("archive rejects a running session without losing its buffers", func(t *testing.T) {
			script := newScriptModel()
			script.gate = make(chan struct{})
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			session := createSession(t, f.h)
			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-f.prepare      // the first admission prepared
			<-script.arrived // parked at the model boundary: the Session is running
			if _, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1"); err != nil {
				t.Fatalf("queued submit: %v", err)
			}
			before := sessionRegister(t, store, session)

			if _, err := f.h.ArchiveSession(ctx, session); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("archive of a running session = err %v, want ErrInvalid", err)
			}
			after := sessionRegister(t, store, session)
			if after.Revision != before.Revision || !bytes.Equal(after.Payload, before.Payload) {
				t.Fatalf("rejected archive changed the durable register (revision %d -> %d)", before.Revision, after.Revision)
			}

			script.releaseGate()
			<-f.prepare      // the rejected archive kept the buffered item: the drain admitted it
			<-script.arrived // the admitted item reached its model boundary: its admission committed
			if err := converge(t, f); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			drained, err := f.h.ReadOperation(ctx, session, "op-2")
			if err != nil || drained.State.Terminal == nil {
				t.Fatalf("buffered item after rejected archive = %+v err %v, want it delivered and settled", drained, err)
			}
		})

		t.Run("delete requires archived and invalidates the coordinator", func(t *testing.T) {
			f := newPublicFixture(t, store, newScriptModel(), nil)
			defer f.close()
			session := createSession(t, f.h)
			before := sessionRegister(t, store, session)

			if err := f.h.DeleteSession(ctx, session); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("delete of an open session = err %v, want ErrInvalid", err)
			}
			after := sessionRegister(t, store, session)
			if after.Revision != before.Revision || !bytes.Equal(after.Payload, before.Payload) {
				t.Fatalf("rejected delete changed the durable register (revision %d -> %d)", before.Revision, after.Revision)
			}

			if _, err := f.h.ArchiveSession(ctx, session); err != nil {
				t.Fatalf("archive: %v", err)
			}
			if err := f.h.DeleteSession(ctx, session); err != nil {
				t.Fatalf("delete of archived: %v", err)
			}

			key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession}
			if _, err := store.ReadRegister(ctx, key); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("deleted session register = err %v, want ErrNotFound", err)
			}
			ids, err := store.ListSessionIDs(ctx)
			if err != nil {
				t.Fatalf("list sessions: %v", err)
			}
			for _, id := range ids { // deletion is absence: no tombstone, no listing
				if id == session {
					t.Fatalf("deleted session still listed")
				}
			}

			// holders of the materialized coordinator and later materialization both get ErrNotFound
			if _, err := f.h.ReadSession(ctx, session); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("read after delete = err %v, want ErrNotFound, not a stale record", err)
			}
			if _, err := f.h.ReadOperation(ctx, session, "op-1"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("operation read after delete = err %v, want ErrNotFound", err)
			}
			if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "late"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("submit after delete = err %v, want ErrNotFound", err)
			}
			if _, err := f.h.ChangeAgentType(ctx, session, "reviewer"); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("agent-type change after delete = err %v, want ErrNotFound", err)
			}
			if _, err := f.h.ReopenSession(ctx, session); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("reopen after delete = err %v, want ErrNotFound", err)
			}
			if _, err := f.h.ArchiveSession(ctx, session); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("archive after delete = err %v, want ErrNotFound", err)
			}
			if err := f.h.DeleteSession(ctx, session); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("second delete = err %v, want ErrNotFound", err)
			}
		})

		t.Run("unknown and malformed identities", func(t *testing.T) {
			f := newPublicFixture(t, store, newScriptModel(), nil)
			defer f.close()
			unknown := "0123456789abcdef0123456789abcdef" // valid hex identity, absent Session
			if _, err := f.h.ReopenSession(ctx, unknown); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("reopen of unknown = err %v, want ErrNotFound", err)
			}
			if _, err := f.h.ArchiveSession(ctx, unknown); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("archive of unknown = err %v, want ErrNotFound", err)
			}
			if err := f.h.DeleteSession(ctx, unknown); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("delete of unknown = err %v, want ErrNotFound", err)
			}
			malformed := "not-a-session-id"
			if _, err := f.h.ReopenSession(ctx, malformed); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("reopen of malformed = err %v, want ErrInvalid", err)
			}
			if _, err := f.h.ArchiveSession(ctx, malformed); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("archive of malformed = err %v, want ErrInvalid", err)
			}
			if err := f.h.DeleteSession(ctx, malformed); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("delete of malformed = err %v, want ErrInvalid", err)
			}
		})
	})
}

// TestPublicSweepArchiveBoundary proves the sweep archive transition through
// public operations: the exact boundary (now-last_activity == ArchiveAfter)
// leaves the Session unchanged, one nanosecond past it archives at the
// explicit sweep time without touching last activity, and the durable state
// carries the stamped revision.
func TestPublicSweepArchiveBoundary(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		f := newPublicFixture(t, store, newScriptModel(), nil)
		defer f.close()
		ctx := context.Background()
		session := createSession(t, f.h)
		first, err := f.h.ReadSession(ctx, session)
		if err != nil {
			t.Fatalf("read created session: %v", err)
		}
		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}

		boundary := first.State.LastActivity.Add(policy.ArchiveAfter)
		before := sessionRegister(t, store, session)
		if err := f.h.Sweep(ctx, policy, boundary); err != nil {
			t.Fatalf("sweep at the boundary: %v", err)
		}
		still, err := f.h.ReadSession(ctx, session)
		if err != nil || still.State.Lifecycle != harness.LifecycleOpen || still.Revision != first.Revision {
			t.Fatalf("boundary sweep = %+v err %v, want the open Session unchanged", still, err)
		}
		after := sessionRegister(t, store, session)
		if after.Revision != before.Revision || !bytes.Equal(after.Payload, before.Payload) {
			t.Fatalf("no-op boundary sweep changed the durable register (revision %d -> %d)", before.Revision, after.Revision)
		}

		past := boundary.Add(time.Nanosecond)
		if err := f.h.Sweep(ctx, policy, past); err != nil {
			t.Fatalf("sweep past the boundary: %v", err)
		}
		archived, err := f.h.ReadSession(ctx, session)
		if err != nil || archived.State.Lifecycle != harness.LifecycleArchived || archived.Revision != first.Revision+1 {
			t.Fatalf("past-boundary sweep = %+v err %v, want archived at the next revision", archived, err)
		}
		if reg := sessionRegister(t, store, session); reg.Revision != archived.Revision {
			t.Fatalf("durable register revision %d disagrees with the swept record's %d", reg.Revision, archived.Revision)
		}
		if archived.State.ArchivedAt == nil || !archived.State.ArchivedAt.Equal(past) {
			t.Fatalf("sweep archive stamped %v, want the explicit sweep time %v", archived.State.ArchivedAt, past)
		}
		if !archived.State.LastActivity.Equal(first.State.LastActivity) {
			t.Fatalf("sweep archive moved last activity to %v, want it unchanged", archived.State.LastActivity)
		}
	})
}

// TestPublicSweepDeleteBoundary proves the sweep delete transition through
// public operations: the exact boundary (now-archived_at == DeleteAfterArchive)
// keeps the Session, one nanosecond past it deletes it — absence in storage
// and ErrNotFound for holders of the materialized coordinator and later
// materialization.
func TestPublicSweepDeleteBoundary(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		f := newPublicFixture(t, store, newScriptModel(), nil)
		defer f.close()
		ctx := context.Background()
		session := createSession(t, f.h)
		archived, err := f.h.ArchiveSession(ctx, session)
		if err != nil || archived.State.ArchivedAt == nil {
			t.Fatalf("archive = %+v err %v", archived, err)
		}
		policy := harness.SweepPolicy{DeleteAfterArchive: 12 * time.Hour}

		boundary := archived.State.ArchivedAt.Add(policy.DeleteAfterArchive)
		before := sessionRegister(t, store, session)
		if err := f.h.Sweep(ctx, policy, boundary); err != nil {
			t.Fatalf("sweep at the delete boundary: %v", err)
		}
		if _, err := f.h.ReadSession(ctx, session); err != nil {
			t.Fatalf("delete-boundary sweep removed the session: %v", err)
		}
		after := sessionRegister(t, store, session)
		if after.Revision != before.Revision || !bytes.Equal(after.Payload, before.Payload) {
			t.Fatalf("no-op delete-boundary sweep changed the durable register (revision %d -> %d)", before.Revision, after.Revision)
		}

		if err := f.h.Sweep(ctx, policy, boundary.Add(time.Nanosecond)); err != nil {
			t.Fatalf("sweep past the delete boundary: %v", err)
		}
		key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession}
		if _, err := store.ReadRegister(ctx, key); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("sweep-deleted session register = err %v, want ErrNotFound", err)
		}
		ids, err := store.ListSessionIDs(ctx)
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		for _, id := range ids {
			if id == session {
				t.Fatalf("sweep-deleted session still listed")
			}
		}
		if _, err := f.h.ReadSession(ctx, session); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("read after sweep delete = err %v, want ErrNotFound, not a stale record", err)
		}
	})
}

// TestPublicSweepDisabledThresholds proves that a nonpositive threshold
// disables only its corresponding transition: both disabled sweeps nothing,
// and each single-disabled sweep performs only the other transition.
func TestPublicSweepDisabledThresholds(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		farPast := func(activity time.Time) time.Time { return activity.Add(100 * time.Hour) }

		t.Run("both disabled sweeps nothing", func(t *testing.T) {
			f := newPublicFixture(t, store, newScriptModel(), nil)
			defer f.close()
			open := createSession(t, f.h)
			archivedID := createSession(t, f.h)
			if _, err := f.h.ArchiveSession(ctx, archivedID); err != nil {
				t.Fatalf("archive: %v", err)
			}
			beforeOpen, err := f.h.ReadSession(ctx, open)
			if err != nil {
				t.Fatalf("read open: %v", err)
			}
			beforeArchived, err := f.h.ReadSession(ctx, archivedID)
			if err != nil {
				t.Fatalf("read archived: %v", err)
			}
			if err := f.h.Sweep(ctx, harness.SweepPolicy{}, farPast(beforeOpen.State.LastActivity)); err != nil {
				t.Fatalf("sweep with disabled thresholds: %v", err)
			}
			afterOpen, err := f.h.ReadSession(ctx, open)
			if err != nil || afterOpen.Revision != beforeOpen.Revision || afterOpen.State.Lifecycle != harness.LifecycleOpen {
				t.Fatalf("disabled sweep touched the open session: %+v err %v", afterOpen, err)
			}
			afterArchived, err := f.h.ReadSession(ctx, archivedID)
			if err != nil || afterArchived.Revision != beforeArchived.Revision || afterArchived.State.Lifecycle != harness.LifecycleArchived {
				t.Fatalf("disabled sweep touched the archived session: %+v err %v", afterArchived, err)
			}
		})

		t.Run("delete disabled archives but does not delete", func(t *testing.T) {
			f := newPublicFixture(t, store, newScriptModel(), nil)
			defer f.close()
			open := createSession(t, f.h)
			archivedID := createSession(t, f.h)
			if _, err := f.h.ArchiveSession(ctx, archivedID); err != nil {
				t.Fatalf("archive: %v", err)
			}
			beforeOpen, err := f.h.ReadSession(ctx, open)
			if err != nil {
				t.Fatalf("read open: %v", err)
			}
			policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour}
			if err := f.h.Sweep(ctx, policy, farPast(beforeOpen.State.LastActivity)); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			sweptOpen, err := f.h.ReadSession(ctx, open)
			if err != nil || sweptOpen.State.Lifecycle != harness.LifecycleArchived {
				t.Fatalf("enabled archive transition = %+v err %v, want archived", sweptOpen, err)
			}
			if _, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: archivedID, Kind: harness.RegisterSession}); err != nil {
				t.Fatalf("disabled delete removed the archived session: %v", err)
			}
			if _, err := f.h.ReadSession(ctx, archivedID); err != nil {
				t.Fatalf("archived sibling after sweep = err %v, want it still present", err)
			}
		})

		t.Run("archive disabled deletes but does not archive", func(t *testing.T) {
			f := newPublicFixture(t, store, newScriptModel(), nil)
			defer f.close()
			open := createSession(t, f.h)
			archivedID := createSession(t, f.h)
			if _, err := f.h.ArchiveSession(ctx, archivedID); err != nil {
				t.Fatalf("archive: %v", err)
			}
			beforeOpen, err := f.h.ReadSession(ctx, open)
			if err != nil {
				t.Fatalf("read open: %v", err)
			}
			policy := harness.SweepPolicy{DeleteAfterArchive: 12 * time.Hour}
			now := beforeOpen.State.LastActivity.Add(100 * time.Hour)
			if err := f.h.Sweep(ctx, policy, now); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			stillOpen, err := f.h.ReadSession(ctx, open)
			if err != nil || stillOpen.State.Lifecycle != harness.LifecycleOpen {
				t.Fatalf("disabled archive transition = %+v err %v, want the session still open", stillOpen, err)
			}
			key := harness.RegisterKey{SessionID: archivedID, Kind: harness.RegisterSession}
			if _, err := store.ReadRegister(ctx, key); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("enabled delete transition left the archived session: err %v", err)
			}
		})
	})
}

// TestPublicSweepRunningLeftUnchanged proves the pending/running sibling axis
// through public operations: a Session running an Operation with a buffered
// queued message is left unchanged by an otherwise fully eligible sweep, and
// its buffered item still drains after the release.
func TestPublicSweepRunningLeftUnchanged(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		ctx := context.Background()
		session := createSession(t, f.h)
		first, err := f.h.ReadSession(ctx, session)
		if err != nil {
			t.Fatalf("read created session: %v", err)
		}

		if _, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-f.prepare      // the first admission prepared
		<-script.arrived // parked at the model boundary: the Session is running
		if _, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1"); err != nil {
			t.Fatalf("queued submit: %v", err)
		}
		active, err := f.h.ReadSession(ctx, session) // the admission's own write is the sweep's baseline
		if err != nil {
			t.Fatalf("read active session: %v", err)
		}
		before := sessionRegister(t, store, session)

		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		if err := f.h.Sweep(ctx, policy, first.State.LastActivity.Add(100*time.Hour)); err != nil {
			t.Fatalf("sweep of a running session: %v", err)
		}
		still, err := f.h.ReadSession(ctx, session)
		if err != nil || still.State.Lifecycle != harness.LifecycleOpen || still.Revision != active.Revision {
			t.Fatalf("sweep touched the running session: %+v err %v, want it unchanged", still, err)
		}
		after := sessionRegister(t, store, session)
		if after.Revision != before.Revision || !bytes.Equal(after.Payload, before.Payload) {
			t.Fatalf("sweep of a running session changed the durable register (revision %d -> %d)", before.Revision, after.Revision)
		}

		script.releaseGate()
		<-f.prepare      // the buffered item was not lost by the sweep: the drain admitted it
		<-script.arrived // the admitted item reached its model boundary: its admission committed
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		drained, err := f.h.ReadOperation(ctx, session, "op-2")
		if err != nil || drained.State.Terminal == nil {
			t.Fatalf("buffered item after sweep = %+v err %v, want it delivered and settled", drained, err)
		}
	})
}

// TestPublicSweepCorruptSibling proves the corruption axis of the lifecycle
// row through public operations: a Session whose register is corrupt in
// storage is left unchanged by Sweep without stopping the pass, its valid
// sibling is still swept, and the corrupt Session becomes unavailable in the
// Harness instance while staying present in storage.
func TestPublicSweepCorruptSibling(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		f := newPublicFixture(t, store, newScriptModel(), nil)
		defer f.close()
		ctx := context.Background()
		corrupt := createSession(t, f.h)
		valid := createSession(t, f.h)

		// corrupt the register directly through the storage contract: syntactically
		// valid JSON whose state section violates the closed lifecycle enum
		reg := sessionRegister(t, store, corrupt)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(reg.Payload, &obj); err != nil {
			t.Fatalf("unmarshal session payload: %v", err)
		}
		var state map[string]json.RawMessage
		if err := json.Unmarshal(obj["state"], &state); err != nil {
			t.Fatalf("unmarshal state section: %v", err)
		}
		state["lifecycle"] = json.RawMessage(`"bogus"`)
		newState, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("marshal state section: %v", err)
		}
		obj["state"] = newState
		bad, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal session payload: %v", err)
		}
		key := harness.RegisterKey{SessionID: corrupt, Kind: harness.RegisterSession}
		if err := store.Transact(ctx, func(tx harness.Transaction) error {
			_, err := tx.ReplaceRegister(key, reg.Revision, bad)
			return err
		}); err != nil {
			t.Fatalf("corrupt the register: %v", err)
		}

		validBefore, err := f.h.ReadSession(ctx, valid)
		if err != nil {
			t.Fatalf("read valid sibling: %v", err)
		}
		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		if err := f.h.Sweep(ctx, policy, validBefore.State.LastActivity.Add(100*time.Hour)); err != nil {
			t.Fatalf("sweep with a corrupt sibling = %v, want the corruption left in place and the pass completed", err)
		}

		untouched := sessionRegister(t, store, corrupt) // the corruption write itself took revision reg.Revision+1
		if !bytes.Equal(untouched.Payload, bad) || untouched.Revision != reg.Revision+1 {
			t.Fatalf("sweep changed the corrupt register (revision %d -> %d)", reg.Revision+1, untouched.Revision)
		}
		swept, err := f.h.ReadSession(ctx, valid)
		if err != nil || swept.State.Lifecycle != harness.LifecycleArchived {
			t.Fatalf("valid sibling after sweep = %+v err %v, want it archived", swept, err)
		}
		if _, err := f.h.ReadSession(ctx, corrupt); !errors.Is(err, harness.ErrCorrupt) {
			t.Fatalf("corrupt session after sweep = err %v, want the corruption error", err)
		}
	})
}

// listCountingStore wraps one store and counts ListSessionIDs calls; when
// listed is set it also signals (non-blocking) each call.
type listCountingStore struct {
	harness.Storage
	lists  int
	listed chan struct{}
}

func (s *listCountingStore) ListSessionIDs(ctx context.Context) ([]string, error) {
	s.lists++
	if s.listed != nil {
		select {
		case s.listed <- struct{}{}:
		default:
		}
	}
	return s.Storage.ListSessionIDs(ctx)
}

// gatedEntryStore wraps one store and parks exactly one Session's first entry
// read: it performs the real pre-deletion read first, captures its value,
// signals, and parks until released — after which it returns the captured
// value instead of re-reading, so a materialization parked across a deletion
// completes its validation on fully pre-deletion data. Later entry reads of
// the Session pass through.
type gatedEntryStore struct {
	harness.Storage
	mu       sync.Mutex
	session  string
	parked   bool
	captured []harness.Entry
	started  chan struct{}
	release  chan struct{}
}

func (s *gatedEntryStore) ReadEntries(ctx context.Context, sessionID string, after int64) ([]harness.Entry, error) {
	if sessionID == s.session && s.release != nil {
		s.mu.Lock()
		first := !s.parked
		s.parked = true
		s.mu.Unlock()
		if first { // only the first materialization parks; concurrent transitions pass through
			captured, err := s.Storage.ReadEntries(ctx, sessionID, after) // the real pre-deletion read
			select {
			case s.started <- struct{}{}:
			default:
			}
			<-s.release
			return captured, err // validation completes on the captured pre-deletion value
		}
	}
	return s.Storage.ReadEntries(ctx, sessionID, after)
}

// seedArchivedSession inserts one archived root Session register directly
// through the storage contract, before the live Harness under test is
// constructed, so its first operation on that Session is a materialization.
func seedArchivedSession(t *testing.T, store harness.Storage) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("random session id: %v", err)
	}
	id := hex.EncodeToString(buf[:])
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload := fmt.Sprintf(
		`{"identity":{"session_id":%q,"workspace":"/tmp/works","created_at":%q},`+
			`"state":{"lifecycle":"archived","archived_at":%q,"current_agent_type":"coder",`+
			`"usage":{"by_model":[]},"last_activity":%q}}`, id, now, now, now)
	err := store.Transact(context.Background(), func(tx harness.Transaction) error {
		_, err := tx.InsertRegister(harness.RegisterDraft{
			Key:     harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession},
			Payload: json.RawMessage(payload),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed archived session: %v", err)
	}
	return id
}

// TestPublicSweepZeroTime proves the sweep's explicit-time precondition: a
// zero time is rejected with ErrInvalid before any storage read.
func TestPublicSweepZeroTime(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		counting := &listCountingStore{Storage: store}
		f := newPublicFixture(t, counting, newScriptModel(), nil)
		defer f.close()
		createSession(t, f.h) // a listed Session exists: any storage read would be observable

		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		if err := f.h.Sweep(context.Background(), policy, time.Time{}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("sweep with a zero time = err %v, want ErrInvalid", err)
		}
		if counting.lists != 0 {
			t.Fatalf("zero-time sweep performed %d ListSessionIDs reads, want none before the rejection", counting.lists)
		}
	})
}

// TestPublicSweepWaitsForBlockedPreparation proves the sweep's transition
// serialization through public operations: a Sweep that reaches a Session with
// an in-flight preparation (reservation held) waits for it to resolve instead
// of transitioning the Session under it — the admission commits, the running
// Session is left unchanged, and the sweep returns nil.
func TestPublicSweepWaitsForBlockedPreparation(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		counting := &listCountingStore{Storage: store, listed: make(chan struct{}, 1)}
		script := newScriptModel()
		gate := make(chan struct{})
		script.gate = gate
		f := newPublicFixture(t, counting, script, nil)
		defer f.close()
		ctx := context.Background()
		session := createSession(t, f.h) // the only Session: after enumeration the sweep is parked on it
		first, err := f.h.ReadSession(ctx, session)
		if err != nil {
			t.Fatalf("read created session: %v", err)
		}

		prepStarted := make(chan struct{}, 1)
		releasePrep := make(chan struct{})
		prepared := harness.PreparedExecution{
			Capture: publicCapture(),
			Model:   script.effect,
			Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
				return harness.PreparedTool{Immediate: &model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"}}
			},
		}
		f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
			if call == 0 { // the first admission's preparation is blocked while the sweep waits
				select {
				case prepStarted <- struct{}{}:
				default:
				}
				<-releasePrep
			}
			return prepared, nil
		}

		type submitOutcome struct {
			res harness.SubmitResult
			err error
		}
		submitDone := make(chan submitOutcome, 1)
		go func() { // the submit blocks inside the preparation: it runs off the test goroutine
			res, err := f.h.Submit(ctx, harness.SubmitRequest{
				SessionID:   session,
				OperationID: "op-1",
				Origin:      harness.InputOriginUser,
				Content:     []model.ContentPart{{Kind: model.PartText, Text: "hello"}},
				Mode:        harness.MessageModeRegular,
			})
			submitDone <- submitOutcome{res: res, err: err}
		}()
		<-prepStarted // the admission holds its reservation inside the blocked preparation

		sweepDone := make(chan error, 1)
		policy := harness.SweepPolicy{ArchiveAfter: time.Hour}
		go func() { sweepDone <- f.h.Sweep(ctx, policy, first.State.LastActivity.Add(100*time.Hour)) }()
		<-counting.listed // the sweep enumerated and is waiting for the idle reservation

		close(releasePrep) // the preparation completes: the admission commits and the Session becomes running
		out := <-submitDone
		if out.err != nil || out.res.Disposition != harness.DispositionAdmitted {
			t.Fatalf("first submit = %+v err %v, want admitted", out.res, out.err)
		}
		if err := <-sweepDone; err != nil {
			t.Fatalf("sweep over a blocked preparation = %v, want it to wait and then skip the running Session", err)
		}
		still, err := f.h.ReadSession(ctx, session)
		if err != nil || still.State.Lifecycle != harness.LifecycleOpen || still.State.CurrentOperationID != "op-1" {
			t.Fatalf("sweep transitioned the Session under a blocked preparation: %+v err %v", still, err)
		}
		if _, err := f.h.ReadOperation(ctx, session, "op-1"); err != nil {
			t.Fatalf("blocked-preparation admission = err %v, want it admitted after the sweep waited", err)
		}

		script.releaseGate()
		if err := converge(t, f); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// TestPublicDeleteVsMaterialization proves the delete-vs-materialization
// contract through public operations on one live Harness: an archived Session
// is seeded before the Harness exists; its first materialization captures its
// valid pre-deletion register and entry state, then parks while a concurrent
// DeleteSession materializes and deletes the same Session; released, the first
// materialization completes validation on the captured pre-deletion data,
// finds the registry generation changed, retries, and resolves to ErrNotFound
// — never installing or serving a stale coordinator.
func TestPublicDeleteVsMaterialization(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		session := seedArchivedSession(t, store) // seeded before the live Harness is constructed
		gated := &gatedEntryStore{Storage: store, session: session, started: make(chan struct{}, 1), release: make(chan struct{})}
		f := newPublicFixture(t, gated, newScriptModel(), nil)
		defer f.close()
		ctx := context.Background()

		readDone := make(chan error, 1)
		go func() { _, err := f.h.ReadSession(ctx, session); readDone <- err }() // the first materialization
		<-gated.started                                                          // parked after capturing its valid pre-deletion state

		if err := f.h.DeleteSession(ctx, session); err != nil { // materializes and deletes the same Session
			t.Fatalf("delete of the seeded session: %v", err)
		}

		close(gated.release) // validation now completes on the captured pre-deletion value
		if err := <-readDone; !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("materialization across a deletion = err %v, want ErrNotFound", err)
		}
		if _, err := f.h.ReadSession(ctx, session); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("later materialization installed a stale coordinator: err %v, want ErrNotFound", err)
		}
	})
}

// errInjectedRollback is the sentinel the rollback probe returns from the
// transaction callback after performing its real mutation.
var errInjectedRollback = errors.New("harness_test: injected post-mutation transaction failure")

// rollbackProbeStore wraps one store to inject one post-mutation transaction
// failure: while armed, the next ReplaceRegister (or DeleteSession) call
// inside a transaction performs its real mutation and then returns the
// sentinel, so the surrounding transaction rolls back after mutating.
type rollbackProbeStore struct {
	harness.Storage
	failReplace bool
	failDelete  bool
}

func (s *rollbackProbeStore) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	return s.Storage.Transact(ctx, func(tx harness.Transaction) error {
		return fn(&rollbackProbeTransaction{Transaction: tx, probe: s})
	})
}

// rollbackProbeTransaction forwards every transaction call and fails the armed
// mutation point only after performing its real effect.
type rollbackProbeTransaction struct {
	harness.Transaction
	probe *rollbackProbeStore
}

func (t *rollbackProbeTransaction) ReplaceRegister(key harness.RegisterKey, expectedRevision int64, payload json.RawMessage) (harness.Register, error) {
	reg, err := t.Transaction.ReplaceRegister(key, expectedRevision, payload)
	if err == nil && t.probe.failReplace {
		t.probe.failReplace = false
		return harness.Register{}, errInjectedRollback
	}
	return reg, err
}

func (t *rollbackProbeTransaction) DeleteSession(sessionID string) error {
	err := t.Transaction.DeleteSession(sessionID)
	if err == nil && t.probe.failDelete {
		t.probe.failDelete = false
		return errInjectedRollback
	}
	return err
}

// assertRollback verifies that a failed lifecycle transaction left the durable
// register byte-identical at the same revision and the coordinator's cached
// view still returns the pre-attempt record.
func assertRollback(t *testing.T, store harness.Storage, h *harness.Harness, session string, before harness.Register, want harness.SessionRecord) {
	t.Helper()
	after := sessionRegister(t, store, session)
	if after.Revision != before.Revision || !bytes.Equal(after.Payload, before.Payload) {
		t.Fatalf("failed transition changed the durable register (revision %d -> %d)", before.Revision, after.Revision)
	}
	got, err := h.ReadSession(context.Background(), session)
	if err != nil {
		t.Fatalf("read after failed transition: %v", err)
	}
	if got.Revision != want.Revision || got.State.Lifecycle != want.State.Lifecycle ||
		!got.State.LastActivity.Equal(want.State.LastActivity) ||
		got.State.CurrentAgentType != want.State.CurrentAgentType {
		t.Fatalf("cached view after failed transition = %+v, want the pre-attempt record", got)
	}
	if (got.State.ArchivedAt == nil) != (want.State.ArchivedAt == nil) ||
		(got.State.ArchivedAt != nil && !got.State.ArchivedAt.Equal(*want.State.ArchivedAt)) {
		t.Fatalf("cached view archived_at after failed transition = %v, want %v", got.State.ArchivedAt, want.State.ArchivedAt)
	}
}

// TestPublicLifecycleRollbackOnTransactionFailure proves the lifecycle row's
// rollback axis with real post-mutation transaction failures on both stores:
// each transition performs its own mutation and then the transaction fails, so
// storage must roll back — the durable register stays byte-identical at the
// same revision, the coordinator's cached view is preserved, the error passes
// through unclassified (and stops a sweep pass), and the next attempt of the
// same transition succeeds. Every case runs on its own fresh memory or
// temporary SQLite store, so an injected failure necessarily hits the case's
// target Session, never an older eligible sibling.
func TestPublicLifecycleRollbackOnTransactionFailure(t *testing.T) {
	variants := []struct {
		name string
		open func(t *testing.T) harness.Storage
	}{
		{name: "memory", open: func(t *testing.T) harness.Storage { return storage.NewMemory() }},
		{name: "sqlite", open: func(t *testing.T) harness.Storage {
			store, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "lightcode.db"))
			if err != nil {
				t.Fatalf("OpenSQLite: %v", err)
			}
			t.Cleanup(func() { store.Close() })
			return store
		}},
	}
	cases := []struct {
		name string
		run  func(t *testing.T, store harness.Storage)
	}{
		{"reopen", rollbackReopenCase},
		{"archive", rollbackArchiveCase},
		{"delete", rollbackDeleteCase},
		{"sweep archive", rollbackSweepArchiveCase},
		{"sweep delete", rollbackSweepDeleteCase},
	}
	for _, variant := range variants {
		variant := variant
		for _, c := range cases {
			c := c
			t.Run(variant.name+"/"+c.name, func(t *testing.T) { c.run(t, variant.open(t)) })
		}
	}
}

// rollbackReopenCase proves Reopen's post-mutation rollback on one isolated store.
func rollbackReopenCase(t *testing.T, store harness.Storage) {
	probe := &rollbackProbeStore{Storage: store}
	f := newPublicFixture(t, probe, newScriptModel(), nil)
	defer f.close()
	ctx := context.Background()
	session := createSession(t, f.h)
	archived, err := f.h.ArchiveSession(ctx, session)
	if err != nil {
		t.Fatalf("seed archive: %v", err)
	}
	before := sessionRegister(t, store, session)

	probe.failReplace = true
	if _, err := f.h.ReopenSession(ctx, session); !errors.Is(err, errInjectedRollback) {
		t.Fatalf("reopen with injected post-mutation failure = err %v, want the injected rollback", err)
	}
	assertRollback(t, store, f.h, session, before, archived)

	reopened, err := f.h.ReopenSession(ctx, session)
	if err != nil || reopened.State.Lifecycle != harness.LifecycleOpen || reopened.State.ArchivedAt != nil ||
		reopened.Revision != before.Revision+1 {
		t.Fatalf("reopen after rollback = %+v err %v, want open at the next revision", reopened, err)
	}
}

// rollbackArchiveCase proves Archive's post-mutation rollback on one isolated store.
func rollbackArchiveCase(t *testing.T, store harness.Storage) {
	probe := &rollbackProbeStore{Storage: store}
	f := newPublicFixture(t, probe, newScriptModel(), nil)
	defer f.close()
	ctx := context.Background()
	session := createSession(t, f.h)
	first, err := f.h.ReadSession(ctx, session)
	if err != nil {
		t.Fatalf("read created session: %v", err)
	}
	before := sessionRegister(t, store, session)

	probe.failReplace = true
	if _, err := f.h.ArchiveSession(ctx, session); !errors.Is(err, errInjectedRollback) {
		t.Fatalf("archive with injected post-mutation failure = err %v, want the injected rollback", err)
	}
	assertRollback(t, store, f.h, session, before, first)

	archived, err := f.h.ArchiveSession(ctx, session)
	if err != nil || archived.State.Lifecycle != harness.LifecycleArchived || archived.State.ArchivedAt == nil ||
		archived.Revision != before.Revision+1 {
		t.Fatalf("archive after rollback = %+v err %v, want archived at the next revision", archived, err)
	}
}

// rollbackDeleteCase proves Delete's post-mutation rollback on one isolated store.
func rollbackDeleteCase(t *testing.T, store harness.Storage) {
	probe := &rollbackProbeStore{Storage: store}
	f := newPublicFixture(t, probe, newScriptModel(), nil)
	defer f.close()
	ctx := context.Background()
	session := createSession(t, f.h)
	archived, err := f.h.ArchiveSession(ctx, session)
	if err != nil {
		t.Fatalf("seed archive: %v", err)
	}
	before := sessionRegister(t, store, session)

	probe.failDelete = true
	if err := f.h.DeleteSession(ctx, session); !errors.Is(err, errInjectedRollback) {
		t.Fatalf("delete with injected post-mutation failure = err %v, want the injected rollback", err)
	}
	assertRollback(t, store, f.h, session, before, archived)
	ids, err := store.ListSessionIDs(ctx)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	found := false
	for _, id := range ids { // the rolled-back deletion is absence-free: the Session is still listed
		if id == session {
			found = true
		}
	}
	if !found {
		t.Fatalf("rolled-back delete removed the session from the listing")
	}

	if err := f.h.DeleteSession(ctx, session); err != nil {
		t.Fatalf("delete after rollback: %v", err)
	}
	key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession}
	if _, err := store.ReadRegister(ctx, key); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("register after delete = err %v, want ErrNotFound", err)
	}
}

// rollbackSweepArchiveCase proves the sweep archive transition's post-mutation
// rollback on one isolated store.
func rollbackSweepArchiveCase(t *testing.T, store harness.Storage) {
	probe := &rollbackProbeStore{Storage: store}
	f := newPublicFixture(t, probe, newScriptModel(), nil)
	defer f.close()
	ctx := context.Background()
	session := createSession(t, f.h)
	first, err := f.h.ReadSession(ctx, session)
	if err != nil {
		t.Fatalf("read created session: %v", err)
	}
	before := sessionRegister(t, store, session)

	policy := harness.SweepPolicy{ArchiveAfter: time.Hour}
	now := first.State.LastActivity.Add(100 * time.Hour)
	probe.failReplace = true
	if err := f.h.Sweep(ctx, policy, now); !errors.Is(err, errInjectedRollback) {
		t.Fatalf("sweep with injected post-mutation failure = err %v, want the pass to stop and return it", err)
	}
	assertRollback(t, store, f.h, session, before, first)

	if err := f.h.Sweep(ctx, policy, now); err != nil {
		t.Fatalf("sweep after rollback: %v", err)
	}
	archived, err := f.h.ReadSession(ctx, session)
	if err != nil || archived.State.Lifecycle != harness.LifecycleArchived || archived.Revision != before.Revision+1 ||
		archived.State.ArchivedAt == nil || !archived.State.ArchivedAt.Equal(now) {
		t.Fatalf("sweep after rollback = %+v err %v, want archived at the sweep time", archived, err)
	}
}

// rollbackSweepDeleteCase proves the sweep delete transition's post-mutation
// rollback on one isolated store.
func rollbackSweepDeleteCase(t *testing.T, store harness.Storage) {
	probe := &rollbackProbeStore{Storage: store}
	f := newPublicFixture(t, probe, newScriptModel(), nil)
	defer f.close()
	ctx := context.Background()
	session := createSession(t, f.h)
	archived, err := f.h.ArchiveSession(ctx, session)
	if err != nil || archived.State.ArchivedAt == nil {
		t.Fatalf("seed archive = %+v err %v", archived, err)
	}
	before := sessionRegister(t, store, session)

	policy := harness.SweepPolicy{DeleteAfterArchive: time.Hour}
	now := archived.State.ArchivedAt.Add(100 * time.Hour)
	probe.failDelete = true
	if err := f.h.Sweep(ctx, policy, now); !errors.Is(err, errInjectedRollback) {
		t.Fatalf("sweep with injected post-mutation failure = err %v, want the pass to stop and return it", err)
	}
	assertRollback(t, store, f.h, session, before, archived)

	if err := f.h.Sweep(ctx, policy, now); err != nil {
		t.Fatalf("sweep after rollback: %v", err)
	}
	key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession}
	if _, err := store.ReadRegister(ctx, key); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("register after swept delete = err %v, want ErrNotFound", err)
	}
}

// TestPublicSweepMalformedRowSibling proves that a malformed Session identity
// returned by storage is a corrupt row for Sweep: it is left unchanged and the
// pass continues to its valid siblings.
func TestPublicSweepMalformedRowSibling(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		const malformed = "corrupt-id" // non-empty (storage accepts it), not the durable 32-hex shape
		if err := store.Transact(ctx, func(tx harness.Transaction) error {
			_, err := tx.InsertRegister(harness.RegisterDraft{
				Key:     harness.RegisterKey{SessionID: malformed, Kind: harness.RegisterSession},
				Payload: json.RawMessage(`{}`),
			})
			return err
		}); err != nil {
			t.Fatalf("plant the malformed row: %v", err)
		}

		f := newPublicFixture(t, store, newScriptModel(), nil)
		defer f.close()
		valid := createSession(t, f.h)
		first, err := f.h.ReadSession(ctx, valid)
		if err != nil {
			t.Fatalf("read the valid sibling: %v", err)
		}

		policy := harness.SweepPolicy{ArchiveAfter: time.Hour}
		if err := f.h.Sweep(ctx, policy, first.State.LastActivity.Add(100*time.Hour)); err != nil {
			t.Fatalf("sweep with a malformed row = %v, want the row left unchanged and the pass completed", err)
		}

		swept, err := f.h.ReadSession(ctx, valid)
		if err != nil || swept.State.Lifecycle != harness.LifecycleArchived {
			t.Fatalf("valid sibling after sweep = %+v err %v, want it archived", swept, err)
		}
		key := harness.RegisterKey{SessionID: malformed, Kind: harness.RegisterSession}
		if _, err := store.ReadRegister(ctx, key); err != nil {
			t.Fatalf("malformed row after sweep = err %v, want it left unchanged", err)
		}
		ids, err := store.ListSessionIDs(ctx)
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		listed := false
		for _, id := range ids {
			if id == malformed {
				listed = true
			}
		}
		if !listed {
			t.Fatalf("malformed row disappeared from the listing after the sweep")
		}
	})
}

// TestPublicWaitConvergesWithCorruptingTransition proves there is no lock
// inversion between Wait's scans and a lifecycle transition that discovers
// corruption under the coordinator mutex (markCorrupt takes h.mu while holding
// c.mu): with the register corrupted in storage after materialization,
// ArchiveSession fails with the corruption error while a concurrent Wait over
// the canceled Harness converges, and the Session stays unavailable.
func TestPublicWaitConvergesWithCorruptingTransition(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		f := newPublicFixture(t, store, newScriptModel(), nil)
		session := createSession(t, f.h)
		ctx := context.Background()

		// corrupt the register directly through storage after materialization:
		// the transition discovers it in-transaction under c.mu
		reg := sessionRegister(t, store, session)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(reg.Payload, &obj); err != nil {
			t.Fatalf("unmarshal session payload: %v", err)
		}
		var state map[string]json.RawMessage
		if err := json.Unmarshal(obj["state"], &state); err != nil {
			t.Fatalf("unmarshal state section: %v", err)
		}
		state["lifecycle"] = json.RawMessage(`"bogus"`)
		newState, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("marshal state section: %v", err)
		}
		obj["state"] = newState
		bad, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal session payload: %v", err)
		}
		key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession}
		if err := store.Transact(ctx, func(tx harness.Transaction) error {
			_, err := tx.ReplaceRegister(key, reg.Revision, bad)
			return err
		}); err != nil {
			t.Fatalf("corrupt the register: %v", err)
		}

		f.cancel() // Harness loss before both calls
		waited := make(chan error, 1)
		go func() { waited <- f.h.Wait(ctx) }() // Wait begins while the transition is in flight

		if _, err := f.h.ArchiveSession(ctx, session); !errors.Is(err, harness.ErrCorrupt) {
			t.Fatalf("archive over a corrupted register = err %v, want the corruption error", err)
		}
		if err := <-waited; err != nil {
			t.Fatalf("Wait converging with the corrupting transition = %v, want nil", err)
		}
		if _, err := f.h.ReadSession(ctx, session); !errors.Is(err, harness.ErrCorrupt) {
			t.Fatalf("corrupt session after convergence = err %v, want the sticky corruption error", err)
		}
	})
}

// recoveredInterruptionDetail is the exact terminal detail Recover settles on
// every running Operation it repairs.
const recoveredInterruptionDetail = "Operation interrupted by Runtime loss."

// recoverOpState mirrors the durable operation-register state section the
// recovery fixtures observe while an effect runs and after settlement; the
// payload stays opaque JSON on the public storage contract.
type recoverOpState struct {
	Status       string `json:"status"`
	ActiveEffect *struct {
		Kind          string `json:"kind"`
		ResultEntryID string `json:"result_entry_id"`
		ToolCallID    string `json:"tool_call_id"`
	} `json:"active_effect"`
	PendingToolCalls []struct {
		CallID        string `json:"call_id"`
		ResultEntryID string `json:"result_entry_id"`
	} `json:"pending_tool_calls"`
	Terminal *struct {
		SettlementEntry struct {
			SessionID string `json:"session_id"`
			EntryID   string `json:"entry_id"`
		} `json:"settlement_entry"`
		Detail string `json:"detail"`
	} `json:"terminal"`
}

func recoverOpStateAt(t *testing.T, store harness.Storage, sessionID, operationID string) recoverOpState {
	t.Helper()
	reg := operationRegister(t, store, sessionID, operationID)
	var wire struct {
		State recoverOpState `json:"state"`
	}
	if err := json.Unmarshal(reg.Payload, &wire); err != nil {
		t.Fatalf("decode operation register %q: %v", operationID, err)
	}
	return wire.State
}

// operationRegister returns the raw durable Operation register of one
// Operation.
func operationRegister(t *testing.T, store harness.Storage, sessionID, operationID string) harness.Register {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID})
	if err != nil {
		t.Fatalf("read operation register %q: %v", operationID, err)
	}
	return reg
}

// recoverSessionCurrent returns the Session register's current Operation ID.
func recoverSessionCurrent(t *testing.T, store harness.Storage, sessionID string) string {
	t.Helper()
	reg := sessionRegister(t, store, sessionID)
	var wire struct {
		State struct {
			CurrentOperationID string `json:"current_operation_id"`
		} `json:"state"`
	}
	if err := json.Unmarshal(reg.Payload, &wire); err != nil {
		t.Fatalf("decode session register: %v", err)
	}
	return wire.State.CurrentOperationID
}

// recoverEntries lists one Session's committed entries in sequence order.
func recoverEntries(t *testing.T, store harness.Storage, sessionID string) []harness.Entry {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("read entries of session %q: %v", sessionID, err)
	}
	return entries
}

// newRawSessionID returns one fresh durable 32-hex identity for raw seeding.
func newRawSessionID(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("random id: %v", err)
	}
	return hex.EncodeToString(buf[:])
}

// insertRawRegister inserts one register payload directly through the storage
// contract. The addressed Session must already exist for Operation registers
// and entries.
func insertRawRegister(t *testing.T, store harness.Storage, key harness.RegisterKey, payload string) {
	t.Helper()
	if err := store.Transact(context.Background(), func(tx harness.Transaction) error {
		_, err := tx.InsertRegister(harness.RegisterDraft{Key: key, Payload: json.RawMessage(payload)})
		return err
	}); err != nil {
		t.Fatalf("insert register %v: %v", key, err)
	}
}

// insertRawEntry inserts one entry payload directly through the storage
// contract. The addressed Session must already exist.
func insertRawEntry(t *testing.T, store harness.Storage, sessionID, id, operationID string, kind harness.EntryKind, payload string) {
	t.Helper()
	if err := store.Transact(context.Background(), func(tx harness.Transaction) error {
		_, err := tx.InsertEntry(harness.EntryDraft{SessionID: sessionID, ID: id, OperationID: operationID, Kind: kind, Payload: json.RawMessage(payload)})
		return err
	}); err != nil {
		t.Fatalf("insert entry %s: %v", id, err)
	}
}

// rawToolDefinition is the one tool definition every recovery fixture captures.
const rawToolDefinition = `{"name":"echo","description":"echoes","parameters":{"type":"object"}}`

// rawSessionRegister builds one open root Session register payload with empty
// usage; currentOp is omitted when empty.
func rawSessionRegister(sessionID, currentOp string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	current := ""
	if currentOp != "" {
		current = fmt.Sprintf(`,"current_operation_id":%q`, currentOp)
	}
	return fmt.Sprintf(
		`{"identity":{"session_id":%q,"workspace":"/tmp/works","created_at":%q},`+
			`"state":{"lifecycle":"open","current_agent_type":"coder"%s,"usage":{"by_model":[]},"last_activity":%q}}`,
		sessionID, now, current, now)
}

// rawAdmission builds the immutable admission section every recovery fixture
// seeds, admitting the given input entry.
func rawAdmission(sessionID, operationID, entryID string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf(
		`{"session_id":%q,"operation_id":%q,"request_kind":"message",`+
			`"admitted_entry":{"session_id":%q,"entry_id":%q},"agent_type":"coder",`+
			`"execution":{"configuration_revision":"rev-1","model":{"provider":"prov","model":"gpt-x"},`+
			`"system_prompt":"system","tools":[%s]},"admitted_at":%q}`,
		sessionID, operationID, sessionID, entryID, rawToolDefinition, now)
}

// rawInputEntryPayload builds one admitted user input entry payload.
func rawInputEntryPayload(sessionID, entryID, operationID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"origin":"user","content":[{"kind":"text","text":"hello"}]}`,
		sessionID, entryID, operationID)
}

// rawAssistantEntryPayload builds one completed assistant entry publishing the
// given (call ID, reserved result ID) pairs in publication order.
func rawAssistantEntryPayload(sessionID, entryID, operationID string, calls [][2]string) string {
	parts := make([]string, 0, len(calls))
	args := base64.StdEncoding.EncodeToString([]byte(`{"x":1}`))
	for i, call := range calls {
		parts = append(parts, fmt.Sprintf(
			`{"id":%q,"ordinal":%d,"name":"echo","arguments_base64":%q,"result_entry_id":%q}`,
			call[0], i, args, call[1]))
	}
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"status":"completed",`+
			`"source":{"provider":"prov","model":"gpt-x"},"content":[{"kind":"text","text":"done"}],"tool_calls":[%s]}`,
		sessionID, entryID, operationID, strings.Join(parts, ","))
}

// seedRunningOperationShape inserts one quiescent open Session with one
// running Operation of the given shape directly through the storage contract,
// before any live Harness over the store exists: the durable state a process
// loss leaves behind. activeEffect is "", "model", or "tool"; calls are the
// published tool-call IDs — a model effect admits no pending calls, a tool
// effect requires at least one. Operation IDs are globally unique in storage,
// so two seeded graphs on one store need distinct ones. It returns the
// Session ID, the active model effect's reserved identity when present, and
// the pending reservations in order.
func seedRunningOperationShape(t *testing.T, store harness.Storage, activeEffect, operationID string, calls []string) (sessionID, modelReserved string, pendingResultIDs []string) {
	t.Helper()
	if activeEffect == "model" && len(calls) > 0 {
		t.Fatalf("a model effect admits no pending calls")
	}
	if activeEffect == "tool" && len(calls) == 0 {
		t.Fatalf("a tool effect requires a first pending call")
	}
	sessionID = newRawSessionID(t)
	entryID, assistantID := newRawSessionID(t), newRawSessionID(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	reserved := make([]string, len(calls))
	for i := range calls {
		reserved[i] = newRawSessionID(t)
	}
	activeEffectJSON := ""
	switch activeEffect {
	case "model":
		modelReserved = newRawSessionID(t)
		activeEffectJSON = fmt.Sprintf(`,"active_effect":{"kind":"model","result_entry_id":%q}`, modelReserved)
	case "tool":
		activeEffectJSON = fmt.Sprintf(`,"active_effect":{"kind":"tool","result_entry_id":%q,"tool_call_id":%q}`, reserved[0], calls[0])
	}
	pendingJSON := `,"pending_tool_calls":[]` // required member; empty arrays are non-null
	if len(calls) > 0 {
		pairs := make([][2]string, len(calls))
		for i := range calls {
			pairs[i] = [2]string{calls[i], reserved[i]}
		}
		parts := make([]string, 0, len(pairs))
		for _, pair := range pairs {
			parts = append(parts, fmt.Sprintf(
				`{"assistant_entry":{"session_id":%q,"entry_id":%q},"call_id":%q,"result_entry_id":%q}`,
				sessionID, assistantID, pair[0], pair[1]))
		}
		pendingJSON = fmt.Sprintf(`,"pending_tool_calls":[%s]`, strings.Join(parts, ","))
	}
	operationPayload := fmt.Sprintf(
		`{"admission":%s,"state":{"status":"running","started_at":%q%s%s,"usage":{"by_model":[]}}}`,
		rawAdmission(sessionID, operationID, entryID), now, activeEffectJSON, pendingJSON)

	insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}, rawSessionRegister(sessionID, operationID))
	insertRawEntry(t, store, sessionID, entryID, operationID, harness.EntryInput, rawInputEntryPayload(sessionID, entryID, operationID))
	if len(calls) > 0 {
		pairs := make([][2]string, len(calls))
		for i := range calls {
			pairs[i] = [2]string{calls[i], reserved[i]}
		}
		insertRawEntry(t, store, sessionID, assistantID, operationID, harness.EntryAssistant, rawAssistantEntryPayload(sessionID, assistantID, operationID, pairs))
	}
	insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID}, operationPayload)
	return sessionID, modelReserved, reserved
}

// sessionSnapshot captures every register and entry of one Session for
// byte-identity comparison.
type sessionSnapshot struct {
	registers []harness.Register
	entries   []harness.Entry
}

func snapshotSession(t *testing.T, store harness.Storage, sessionID string) sessionSnapshot {
	t.Helper()
	registers, err := store.ReadRegisters(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read registers of %q: %v", sessionID, err)
	}
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("read entries of %q: %v", sessionID, err)
	}
	return sessionSnapshot{registers: registers, entries: entries}
}

// assertSessionUnchanged verifies that recovery left one Session's complete
// durable state byte-identical at the same revisions: no repair, no
// truncation, no rewrite.
func assertSessionUnchanged(t *testing.T, store harness.Storage, sessionID string, before sessionSnapshot) {
	t.Helper()
	after := snapshotSession(t, store, sessionID)
	if len(after.registers) != len(before.registers) || len(after.entries) != len(before.entries) {
		t.Fatalf("recovery changed the record count of corrupt session %q (registers %d -> %d, entries %d -> %d)",
			sessionID, len(before.registers), len(after.registers), len(before.entries), len(after.entries))
	}
	for i := range before.registers {
		b, a := before.registers[i], after.registers[i]
		if b.Key != a.Key || b.Revision != a.Revision || !bytes.Equal(b.Payload, a.Payload) {
			t.Fatalf("recovery changed register %v of corrupt session %q (revision %d -> %d)", b.Key, sessionID, b.Revision, a.Revision)
		}
	}
	for i := range before.entries {
		b, a := before.entries[i], after.entries[i]
		if b.ID != a.ID || b.Kind != a.Kind || b.Sequence != a.Sequence || !bytes.Equal(b.Payload, a.Payload) {
			t.Fatalf("recovery changed entry %s of corrupt session %q", b.ID, sessionID)
		}
	}
}

// harnessOver constructs one live Harness over an already-recovered store for
// materialization verification only; its preparation callback must never run.
func harnessOver(t *testing.T, store harness.Storage) *harness.Harness {
	t.Helper()
	h, err := harness.New(context.Background(), harness.Dependencies{Storage: store, Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
		return harness.PreparedExecution{}, errors.New("the recovery suite never prepares")
	}})
	if err != nil {
		t.Fatalf("New after recover: %v", err)
	}
	return h
}

// assertRecoveredOperation verifies the complete recovery outcome on one
// Session's durable state: the terminal interruption with the exact detail,
// no active or pending effects, the cleared Session current Operation, and
// the exact committed tail — one interrupted tool result under each reserved
// identity in order, exactly one signal, and the one settlement entry under
// wantSettlementID (or a fresh identity when it is empty). Every pre-recovery
// entry stays byte-identical: no replay, repair, or truncation.
func assertRecoveredOperation(t *testing.T, store harness.Storage, sessionID, operationID string, preEntries []harness.Entry, wantSettlementID string, wantInterruptedResultIDs []string) {
	t.Helper()
	state := recoverOpStateAt(t, store, sessionID, operationID)
	if state.Status != "interruption" || state.ActiveEffect != nil || len(state.PendingToolCalls) != 0 || state.Terminal == nil {
		t.Fatalf("recovered operation = %+v, want terminal interruption with no active or pending effects", state)
	}
	if state.Terminal.Detail != recoveredInterruptionDetail {
		t.Fatalf("recovered detail = %q, want the exact recovery detail %q", state.Terminal.Detail, recoveredInterruptionDetail)
	}
	settlementID := state.Terminal.SettlementEntry.EntryID
	if wantSettlementID != "" {
		if settlementID != wantSettlementID {
			t.Fatalf("settlement entry = %s, want the reserved identity %s", settlementID, wantSettlementID)
		}
	} else {
		seen := map[string]bool{}
		for _, e := range preEntries {
			seen[e.ID] = true
		}
		for _, id := range wantInterruptedResultIDs {
			seen[id] = true
		}
		if len(settlementID) != 32 || seen[settlementID] {
			t.Fatalf("settlement entry = %q, want a fresh identity unused by the pre-recovery state", settlementID)
		}
	}
	if current := recoverSessionCurrent(t, store, sessionID); current != "" {
		t.Fatalf("session current operation = %q, want it cleared after recovery", current)
	}
	wantTail := make([]harness.EntryKind, 0, len(wantInterruptedResultIDs)+2)
	for range wantInterruptedResultIDs {
		wantTail = append(wantTail, harness.EntryToolResult)
	}
	wantTail = append(wantTail, harness.EntrySignal, harness.EntryOperationSettlement)
	entries := recoverEntries(t, store, sessionID)
	if len(entries) != len(preEntries)+len(wantTail) {
		t.Fatalf("recovered entries = %d, want the pre-recovery %d plus the %d recovery commits", len(entries), len(preEntries), len(wantTail))
	}
	for i, want := range preEntries {
		if entries[i].ID != want.ID || entries[i].Kind != want.Kind || !bytes.Equal(entries[i].Payload, want.Payload) {
			t.Fatalf("pre-recovery entry %d = %s/%s, want %s/%s unchanged", i, entries[i].ID, entries[i].Kind, want.ID, want.Kind)
		}
	}
	for i, kind := range wantTail {
		entry := entries[len(preEntries)+i]
		if entry.Kind != kind {
			t.Fatalf("recovery commit %d is a %s entry, want %s", i, entry.Kind, kind)
		}
		switch kind {
		case harness.EntryToolResult:
			if entry.ID != wantInterruptedResultIDs[i] {
				t.Fatalf("interrupted result %d = %s, want the reserved identity %s", i, entry.ID, wantInterruptedResultIDs[i])
			}
		case harness.EntryOperationSettlement:
			if entry.ID != settlementID {
				t.Fatalf("settlement entry %s disagrees with the operation's terminal reference %s", entry.ID, settlementID)
			}
			var wire struct {
				Status string `json:"status"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(entry.Payload, &wire); err != nil {
				t.Fatalf("decode settlement entry: %v", err)
			}
			if wire.Status != "interruption" || wire.Detail != recoveredInterruptionDetail {
				t.Fatalf("settlement payload = %+v, want the terminal interruption with the recovery detail", wire)
			}
		}
	}
}

// assertRecoverIdempotent runs Recover once more and requires it to find no
// running Operation and write nothing: both registers byte-identical at the
// same revision and the entry list unchanged.
func assertRecoverIdempotent(t *testing.T, store harness.Storage, sessionID, operationID string) {
	t.Helper()
	opBefore := operationRegister(t, store, sessionID, operationID)
	sessBefore := sessionRegister(t, store, sessionID)
	entriesBefore := recoverEntries(t, store, sessionID)
	if err := harness.Recover(context.Background(), store); err != nil {
		t.Fatalf("second recover: %v", err)
	}
	opAfter := operationRegister(t, store, sessionID, operationID)
	sessAfter := sessionRegister(t, store, sessionID)
	if opAfter.Revision != opBefore.Revision || !bytes.Equal(opAfter.Payload, opBefore.Payload) {
		t.Fatalf("second recover changed the operation register (revision %d -> %d)", opBefore.Revision, opAfter.Revision)
	}
	if sessAfter.Revision != sessBefore.Revision || !bytes.Equal(sessAfter.Payload, sessBefore.Payload) {
		t.Fatalf("second recover changed the session register (revision %d -> %d)", sessBefore.Revision, sessAfter.Revision)
	}
	if got := len(recoverEntries(t, store, sessionID)); got != len(entriesBefore) {
		t.Fatalf("second recover committed %d entries, want none", got-len(entriesBefore))
	}
}

// TestPublicRecoverRunningOperationShapes proves the recovery row: every
// running effect/pending shape — a quiet Operation with no calls, an active
// model effect, an active tool effect, and a quiet Operation with pending
// calls — is seeded directly into storage, settled by Recover (which runs
// before any Harness exists) in one transaction as terminal interruption
// through the regular result/signal/terminal helpers, with the settlement
// consuming an active model effect's reserved identity or a fresh identity
// otherwise; a second run writes nothing, and one Harness constructed only
// afterwards materializes the repaired state.
func TestPublicRecoverRunningOperationShapes(t *testing.T) {
	shapes := []struct {
		name         string
		activeEffect string // "", "model", or "tool"
		calls        int    // published tool calls of the seeded shape
	}{
		{"quiet running operation without calls", "", 0},
		{"active model effect settles under its reserved identity", "model", 0},
		{"active tool effect interrupts its pending call", "tool", 1},
		{"quiet running operation with pending calls", "", 2},
	}
	for _, shape := range shapes {
		shape := shape
		t.Run(shape.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, store harness.Storage) {
				ctx := context.Background()
				calls := make([]string, shape.calls)
				for i := range calls {
					calls[i] = fmt.Sprintf("call-%d", i+1)
				}
				sessionID, modelReserved, pendingResultIDs := seedRunningOperationShape(t, store, shape.activeEffect, "op-1", calls)

				state := recoverOpStateAt(t, store, sessionID, "op-1")
				if state.Status != "running" || len(state.PendingToolCalls) != shape.calls {
					t.Fatalf("seeded operation = %+v, want running with %d pending call(s)", state, shape.calls)
				}
				switch shape.activeEffect {
				case "model":
					if state.ActiveEffect == nil || state.ActiveEffect.Kind != "model" || state.ActiveEffect.ResultEntryID != modelReserved {
						t.Fatalf("seeded operation = %+v, want the committed model effect intent", state)
					}
				case "tool":
					if state.ActiveEffect == nil || state.ActiveEffect.Kind != "tool" || state.ActiveEffect.ToolCallID != calls[0] ||
						state.ActiveEffect.ResultEntryID != pendingResultIDs[0] {
						t.Fatalf("seeded operation = %+v, want the committed tool effect intent on %s", state, calls[0])
					}
				default:
					if state.ActiveEffect != nil {
						t.Fatalf("seeded operation = %+v, want no active effect", state)
					}
				}

				pre := recoverEntries(t, store, sessionID)
				if err := harness.Recover(ctx, store); err != nil { // before any live Harness over the store
					t.Fatalf("recover: %v", err)
				}
				assertRecoveredOperation(t, store, sessionID, "op-1", pre, modelReserved, pendingResultIDs)
				assertRecoverIdempotent(t, store, sessionID, "op-1")

				h := harnessOver(t, store) // one Harness after recovery: materialization verification only
				sess, err := h.ReadSession(ctx, sessionID)
				if err != nil || sess.State.CurrentOperationID != "" {
					t.Fatalf("repaired session = %+v err %v, want open with the current Operation cleared", sess, err)
				}
				rec, err := h.ReadOperation(ctx, sessionID, "op-1")
				if err != nil || rec.State.Status != harness.OperationInterruption || rec.State.Terminal == nil ||
					rec.State.Terminal.Detail != recoveredInterruptionDetail {
					t.Fatalf("repaired operation = %+v err %v, want the terminal interruption with the recovery detail", rec, err)
				}
			})
		})
	}
}

// corruptSiblingStore wraps one store and serves exactly one Session's
// registers and entries from a mutated deep-copied read snapshot, delegating
// sibling reads, the session listing, and all transactions to the real store.
// Serving corruptions that real stores reject on insertion — duplicate
// identities, malformed JSON — is the point: recovery must classify them from
// reads alone and never write anything back.
type corruptSiblingStore struct {
	harness.Storage
	session   string
	registers []harness.Register
	entries   []harness.Entry
}

func (s *corruptSiblingStore) ReadRegisters(ctx context.Context, sessionID string) ([]harness.Register, error) {
	if sessionID == s.session {
		return append([]harness.Register(nil), s.registers...), nil
	}
	return s.Storage.ReadRegisters(ctx, sessionID)
}

func (s *corruptSiblingStore) ReadEntries(ctx context.Context, sessionID string, after int64) ([]harness.Entry, error) {
	if sessionID == s.session {
		var out []harness.Entry
		for _, entry := range s.entries {
			if entry.Sequence > after {
				out = append(out, entry)
			}
		}
		return out, nil
	}
	return s.Storage.ReadEntries(ctx, sessionID, after)
}

// Distinct durable identities no seeded graph ever generates: the "other"
// references the corruption rows point at missing records.
const (
	corruptSiblingOtherSession = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	corruptSiblingOtherEntry   = "dddddddddddddddddddddddddddddddd"
)

// rawToolResultPayload builds one settled successful tool-result entry payload
// answering the given assistant entry's call under the reserved identity.
func rawToolResultPayload(sessionID, entryID, operationID, assistantEntryID, callID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"assistant_entry":{"session_id":%q,"entry_id":%q},"tool_call_id":%q,"status":"success","content":"done"}`,
		sessionID, entryID, operationID, sessionID, assistantEntryID, callID)
}

// rawSignalPayload builds one owned interruption signal entry payload related
// to its owning Operation.
func rawSignalPayload(sessionID, entryID, operationID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"signal":"interruption","related_operation":{"session_id":%q,"operation_id":%q},"content":"Operation interrupted."}`,
		sessionID, entryID, operationID, sessionID, operationID)
}

// rawSettlementPayload builds one successful settlement entry payload.
func rawSettlementPayload(sessionID, entryID, operationID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"status":"success"}`,
		sessionID, entryID, operationID)
}

// rawRunningOperationPayload builds one quiet running Operation register
// payload (no active effect, empty pending list) admitting the given entry.
func rawRunningOperationPayload(sessionID, operationID, admittedEntryID string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf(
		`{"admission":%s,"state":{"status":"running","started_at":%q,"pending_tool_calls":[],"usage":{"by_model":[]}}}`,
		rawAdmission(sessionID, operationID, admittedEntryID), now)
}

// rawSettledOperationPayload builds one successfully settled Operation
// register payload whose terminal names the given settlement entry.
func rawSettledOperationPayload(sessionID, operationID, admittedEntryID, settlementID string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf(
		`{"admission":%s,"state":{"status":"success","started_at":%q,"settled_at":%q,"pending_tool_calls":[],"usage":{"by_model":[]},`+
			`"terminal":{"settlement_entry":{"session_id":%q,"entry_id":%q}}}}`,
		rawAdmission(sessionID, operationID, admittedEntryID), now, now, sessionID, settlementID)
}

// rawCopiedInputPayload builds one operationless copied input entry payload.
func rawCopiedInputPayload(sessionID, entryID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"origin":"user","content":[{"kind":"text","text":"hello"}]}`,
		sessionID, entryID)
}

// rawCopiedAssistantPayload builds one operationless copied assistant entry
// payload publishing the given (call ID, reserved result ID) pairs.
func rawCopiedAssistantPayload(sessionID, entryID string, calls [][2]string) string {
	parts := make([]string, 0, len(calls))
	args := base64.StdEncoding.EncodeToString([]byte(`{"x":1}`))
	for i, call := range calls {
		parts = append(parts, fmt.Sprintf(
			`{"id":%q,"ordinal":%d,"name":"echo","arguments_base64":%q,"result_entry_id":%q}`,
			call[0], i, args, call[1]))
	}
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"status":"completed",`+
			`"source":{"provider":"prov","model":"gpt-x"},"content":[{"kind":"text","text":"done"}],"tool_calls":[%s]}`,
		sessionID, entryID, strings.Join(parts, ","))
}

// rawCopiedResultPayload builds one operationless copied tool-result entry
// payload answering the given copied assistant entry's call.
func rawCopiedResultPayload(sessionID, entryID, assistantEntryID, callID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"assistant_entry":{"session_id":%q,"entry_id":%q},"tool_call_id":%q,"status":"success","content":"done"}`,
		sessionID, entryID, sessionID, assistantEntryID, callID)
}

// seedCorruptSiblingRunning seeds the plain valid target graph: an open
// Session whose running Operation owns one admitted input entry.
func seedCorruptSiblingRunning(t *testing.T, store harness.Storage) string {
	t.Helper()
	sessionID, _, _ := seedRunningOperationShape(t, store, "", "op-1", nil)
	return sessionID
}

// seedCorruptSiblingPendingCall seeds the valid target graph with one
// published call left pending: an assistant entry reserving one result
// identity named by the Operation's pending list.
func seedCorruptSiblingPendingCall(t *testing.T, store harness.Storage) string {
	t.Helper()
	sessionID, _, _ := seedRunningOperationShape(t, store, "", "op-1", []string{"call-1"})
	return sessionID
}

// seedCorruptSiblingWithResult seeds the valid target graph whose single
// published call carries its terminal tool result under the reservation.
func seedCorruptSiblingWithResult(t *testing.T, store harness.Storage) string {
	t.Helper()
	sessionID, entryID, assistantID, resultID := newRawSessionID(t), newRawSessionID(t), newRawSessionID(t), newRawSessionID(t)
	insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}, rawSessionRegister(sessionID, "op-1"))
	insertRawEntry(t, store, sessionID, entryID, "op-1", harness.EntryInput, rawInputEntryPayload(sessionID, entryID, "op-1"))
	insertRawEntry(t, store, sessionID, assistantID, "op-1", harness.EntryAssistant, rawAssistantEntryPayload(sessionID, assistantID, "op-1", [][2]string{{"call-1", resultID}}))
	insertRawEntry(t, store, sessionID, resultID, "op-1", harness.EntryToolResult, rawToolResultPayload(sessionID, resultID, "op-1", assistantID, "call-1"))
	insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "op-1"}, rawRunningOperationPayload(sessionID, "op-1", entryID))
	return sessionID
}

// seedCorruptSiblingWithSignal seeds the valid target graph carrying one owned
// interruption signal related to its own running Operation.
func seedCorruptSiblingWithSignal(t *testing.T, store harness.Storage) string {
	t.Helper()
	sessionID, entryID, signalID := newRawSessionID(t), newRawSessionID(t), newRawSessionID(t)
	insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}, rawSessionRegister(sessionID, "op-1"))
	insertRawEntry(t, store, sessionID, entryID, "op-1", harness.EntryInput, rawInputEntryPayload(sessionID, entryID, "op-1"))
	insertRawEntry(t, store, sessionID, signalID, "op-1", harness.EntrySignal, rawSignalPayload(sessionID, signalID, "op-1"))
	insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "op-1"}, rawRunningOperationPayload(sessionID, "op-1", entryID))
	return sessionID
}

// seedCorruptSiblingSettled seeds the valid target graph of a successfully
// settled Operation: no current Operation, one admitted input, the terminal
// register, and its one settlement entry.
func seedCorruptSiblingSettled(t *testing.T, store harness.Storage) string {
	t.Helper()
	sessionID, entryID, settlementID := newRawSessionID(t), newRawSessionID(t), newRawSessionID(t)
	insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}, rawSessionRegister(sessionID, ""))
	insertRawEntry(t, store, sessionID, entryID, "op-1", harness.EntryInput, rawInputEntryPayload(sessionID, entryID, "op-1"))
	insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "op-1"}, rawSettledOperationPayload(sessionID, "op-1", entryID, settlementID))
	insertRawEntry(t, store, sessionID, settlementID, "op-1", harness.EntryOperationSettlement, rawSettlementPayload(sessionID, settlementID, "op-1"))
	return sessionID
}

// snapSessionID returns the seeded graph's Session identity.
func snapSessionID(snap *sessionSnapshot) string {
	if len(snap.entries) > 0 {
		return snap.entries[0].SessionID
	}
	return snap.registers[0].Key.SessionID
}

// snapRegisterOf returns the snapshot register of one kind (and Operation
// identity for Operation registers).
func snapRegisterOf(snap *sessionSnapshot, kind harness.RegisterKind, operationID string) *harness.Register {
	for i := range snap.registers {
		reg := &snap.registers[i]
		if reg.Key.Kind == kind && (kind != harness.RegisterOperation || reg.Key.OperationID == operationID) {
			return reg
		}
	}
	return nil
}

// snapEntryOf returns the snapshot's first entry of one kind.
func snapEntryOf(snap *sessionSnapshot, kind harness.EntryKind) *harness.Entry {
	for i := range snap.entries {
		if snap.entries[i].Kind == kind {
			return &snap.entries[i]
		}
	}
	return nil
}

// snapDropEntry removes the snapshot's first entry of one kind.
func snapDropEntry(snap *sessionSnapshot, kind harness.EntryKind) {
	for i := range snap.entries {
		if snap.entries[i].Kind == kind {
			snap.entries = append(snap.entries[:i], snap.entries[i+1:]...)
			return
		}
	}
}

// snapDropSessionRegister removes the snapshot's Session register.
func snapDropSessionRegister(snap *sessionSnapshot) {
	for i := range snap.registers {
		if snap.registers[i].Key.Kind == harness.RegisterSession {
			snap.registers = append(snap.registers[:i], snap.registers[i+1:]...)
			return
		}
	}
}

// snapAppendEntry appends one entry envelope to the snapshot after the last
// committed sequence.
func snapAppendEntry(snap *sessionSnapshot, sessionID, entryID, operationID string, kind harness.EntryKind, payload string) {
	var highest int64
	for _, entry := range snap.entries {
		if entry.Sequence > highest {
			highest = entry.Sequence
		}
	}
	snap.entries = append(snap.entries, harness.Entry{
		SessionID: sessionID, ID: entryID, Sequence: highest + 1,
		OperationID: operationID, Kind: kind, Payload: json.RawMessage(payload),
	})
}

// snapAppendOperation appends one Operation register to the snapshot.
func snapAppendOperation(snap *sessionSnapshot, sessionID, operationID, payload string) {
	snap.registers = append(snap.registers, harness.Register{
		Key:      harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID},
		Revision: 1, Payload: json.RawMessage(payload),
	})
}

// editPayloadObject returns one JSON object payload with its members edited.
func editPayloadObject(t *testing.T, raw json.RawMessage, edit func(map[string]json.RawMessage)) json.RawMessage {
	t.Helper()
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	edit(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal edited payload: %v", err)
	}
	return out
}

// editStateObject returns one register payload whose state member is edited.
func editStateObject(t *testing.T, raw json.RawMessage, edit func(map[string]json.RawMessage)) json.RawMessage {
	t.Helper()
	return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
		obj["state"] = editPayloadObject(t, obj["state"], edit)
	})
}

// editOperationAdmission returns one Operation register payload whose
// admission member is edited.
func editOperationAdmission(t *testing.T, raw json.RawMessage, edit func(map[string]json.RawMessage)) json.RawMessage {
	t.Helper()
	return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
		obj["admission"] = editPayloadObject(t, obj["admission"], edit)
	})
}

// editJSONArray returns one JSON array of objects with its items edited.
func editJSONArray(t *testing.T, raw json.RawMessage, edit func([]map[string]json.RawMessage)) json.RawMessage {
	t.Helper()
	items := []map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("payload is not a JSON array of objects: %v", err)
	}
	edit(items)
	out, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal edited array: %v", err)
	}
	return out
}

// snapReservedResultID returns the one published call's reserved result
// identity from the snapshot's assistant payload.
func snapReservedResultID(t *testing.T, snap *sessionSnapshot) string {
	t.Helper()
	assistant := snapEntryOf(snap, harness.EntryAssistant)
	var wire struct {
		ToolCalls []struct {
			ResultEntryID string `json:"result_entry_id"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(assistant.Payload, &wire); err != nil || len(wire.ToolCalls) != 1 {
		t.Fatalf("assistant payload has no single published call: %v", err)
	}
	return wire.ToolCalls[0].ResultEntryID
}

// snapMakeFork rewrites the snapshot's Session identity into a fork of an
// unrelated source Session.
func snapMakeFork(t *testing.T, snap *sessionSnapshot) {
	t.Helper()
	reg := snapRegisterOf(snap, harness.RegisterSession, "")
	reg.Payload = editPayloadObject(t, reg.Payload, func(obj map[string]json.RawMessage) {
		obj["identity"] = editPayloadObject(t, obj["identity"], func(identity map[string]json.RawMessage) {
			identity["source_session_id"] = json.RawMessage(`"` + corruptSiblingOtherSession + `"`)
			identity["source_boundary_entry_id"] = json.RawMessage(`"` + corruptSiblingOtherEntry + `"`)
		})
	})
}

// snapStripOperation rewrites the running graph into a bare copied prefix: the
// Operation register is dropped, the Session current pointer cleared, and the
// admitted input entry loses its owning Operation identity.
func snapStripOperation(t *testing.T, snap *sessionSnapshot) {
	t.Helper()
	for i := range snap.registers {
		if snap.registers[i].Key.Kind == harness.RegisterOperation {
			snap.registers = append(snap.registers[:i], snap.registers[i+1:]...)
			break
		}
	}
	reg := snapRegisterOf(snap, harness.RegisterSession, "")
	reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
		delete(state, "current_operation_id")
	})
	entry := snapEntryOf(snap, harness.EntryInput)
	entry.OperationID = ""
	entry.Payload = editPayloadObject(t, entry.Payload, func(obj map[string]json.RawMessage) {
		delete(obj, "operation_id")
	})
}

// snapCopyEntry strips the owning Operation identity from one snapshot entry.
func snapCopyEntry(t *testing.T, entry *harness.Entry) {
	t.Helper()
	entry.OperationID = ""
	entry.Payload = editPayloadObject(t, entry.Payload, func(obj map[string]json.RawMessage) {
		delete(obj, "operation_id")
	})
}

// corruptSiblingRow is one recovery corruption row: a valid base graph plus
// one named mutation applied to its deep-copied read snapshot.
type corruptSiblingRow struct {
	name   string
	seed   func(t *testing.T, store harness.Storage) string
	mutate func(t *testing.T, snap *sessionSnapshot)
}

// TestPublicRecoverCorruptSibling proves the recovery-side corruption row with
// the complete negative inventory of the durable-graph validator and both
// wire-codec rejection suites (TestGraphValidatorSurfacesCorruption,
// TestEntryPayloadRejectsInvalidWire, and TestRegisterPayloadRejectsInvalidWire):
// every row seeds one valid target graph, serves the target's reads from a
// snapshot carrying exactly one named corruption through the test-only Storage
// wrapper, and runs Recover before any Harness exists. The unmutated graph
// first materializes on a separate fresh store — never the recovery store — so
// each row's corruption is attributable to its mutation alone. Recover leaves
// the corrupt Session's backing records byte-identical without stopping the
// pass, repairs the valid running sibling seeded alongside, and a Harness
// constructed only afterwards returns ErrCorrupt for the corrupt Session while
// materializing the repaired sibling.
//
// Two validator rows have no corruption-row equivalent here: "fully absent
// session passes not-found through" asserts the not-corruption classification
// of a graph this suite cannot seed (absence is not a mutated read), and "one
// corrupt record among valid siblings" is the structure of every row.
func TestPublicRecoverCorruptSibling(t *testing.T) {
	rows := []corruptSiblingRow{
		// The durable-graph validator inventory.
		{
			name: "validator: corrupt entry payload",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				entry := snapEntryOf(snap, harness.EntryInput)
				entry.Payload = editPayloadObject(t, entry.Payload, func(obj map[string]json.RawMessage) {
					obj["bogus"] = json.RawMessage(`1`)
				})
			},
		},
		{
			name: "validator: corrupt session register payload",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterSession, "")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["lifecycle"] = json.RawMessage(`"stopping"`)
				})
			},
		},
		{
			name: "validator: two running operations",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				input := snapEntryOf(snap, harness.EntryInput)
				snapAppendOperation(snap, sessionID, "op-2", rawRunningOperationPayload(sessionID, "op-2", input.ID))
			},
		},
		{
			name: "validator: current operation missing",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterSession, "")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["current_operation_id"] = json.RawMessage(`"ghost"`)
				})
			},
		},
		{
			name: "validator: current operation not running",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["status"] = json.RawMessage(`"failure"`)
					state["settled_at"] = json.RawMessage(`"` + time.Now().UTC().Format(time.RFC3339Nano) + `"`)
					state["terminal"] = json.RawMessage(fmt.Sprintf(`{"settlement_entry":{"session_id":%q,"entry_id":%q},"detail":"boom"}`, sessionID, corruptSiblingOtherEntry))
				})
			},
		},
		{
			name: "validator: running operation unnamed by session",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterSession, "")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					delete(state, "current_operation_id")
				})
			},
		},
		{
			name: "validator: running operation in archived session",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterSession, "")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["lifecycle"] = json.RawMessage(`"archived"`)
					state["archived_at"] = json.RawMessage(`"` + time.Now().UTC().Format(time.RFC3339Nano) + `"`)
				})
			},
		},
		{
			name: "validator: admitted entry missing",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editOperationAdmission(t, reg.Payload, func(admission map[string]json.RawMessage) {
					admission["admitted_entry"] = json.RawMessage(fmt.Sprintf(`{"session_id":%q,"entry_id":%q}`, sessionID, corruptSiblingOtherEntry))
				})
			},
		},
		{
			name: "validator: admitted entry wrong kind",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				assistant := snapEntryOf(snap, harness.EntryAssistant)
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editOperationAdmission(t, reg.Payload, func(admission map[string]json.RawMessage) {
					admission["admitted_entry"] = json.RawMessage(fmt.Sprintf(`{"session_id":%q,"entry_id":%q}`, assistant.SessionID, assistant.ID))
				})
			},
		},
		{
			name: "validator: entry owned by unknown operation",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				entry := snapEntryOf(snap, harness.EntryInput)
				entry.OperationID = "ghost"
				entry.Payload = editPayloadObject(t, entry.Payload, func(obj map[string]json.RawMessage) {
					obj["operation_id"] = json.RawMessage(`"ghost"`)
				})
			},
		},
		{
			name: "validator: published call neither pending nor result",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["pending_tool_calls"] = json.RawMessage(`[]`)
				})
			},
		},
		{
			name: "validator: pending call with terminal result too",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				assistant := snapEntryOf(snap, harness.EntryAssistant)
				reserved := snapReservedResultID(t, snap)
				snapAppendEntry(snap, sessionID, reserved, "op-1", harness.EntryToolResult,
					rawToolResultPayload(sessionID, reserved, "op-1", assistant.ID, "call-1"))
			},
		},
		{
			name: "validator: second result breaks reservation identity",
			seed: seedCorruptSiblingWithResult,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				assistant := snapEntryOf(snap, harness.EntryAssistant)
				dup := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, dup, "op-1", harness.EntryToolResult,
					rawToolResultPayload(sessionID, dup, "op-1", assistant.ID, "call-1"))
			},
		},
		{
			name: "validator: result answers unpublished call",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				input := snapEntryOf(snap, harness.EntryInput)
				result := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, result, "op-1", harness.EntryToolResult,
					rawToolResultPayload(sessionID, result, "op-1", input.ID, "call-1"))
			},
		},
		{
			name: "validator: terminal operation without settlement entry",
			seed: seedCorruptSiblingSettled,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				snapDropEntry(snap, harness.EntryOperationSettlement)
			},
		},
		{
			name: "validator: settlement status disagreement",
			seed: seedCorruptSiblingSettled,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				entry := snapEntryOf(snap, harness.EntryOperationSettlement)
				entry.Payload = editPayloadObject(t, entry.Payload, func(obj map[string]json.RawMessage) {
					obj["status"] = json.RawMessage(`"failure"`)
					obj["detail"] = json.RawMessage(`"boom"`)
				})
			},
		},
		{
			name: "validator: running operation carries settlement",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				settlement := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, settlement, "op-1", harness.EntryOperationSettlement,
					rawSettlementPayload(sessionID, settlement, "op-1"))
			},
		},
		{
			name: "validator: operation usage disagrees with entries",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				entry := snapEntryOf(snap, harness.EntryAssistant)
				entry.Payload = editPayloadObject(t, entry.Payload, func(obj map[string]json.RawMessage) {
					obj["usage"] = json.RawMessage(`{"input_tokens":4,"cached_input_tokens":4,"output_tokens":4}`)
				})
			},
		},
		{
			name: "validator: session usage disagrees with entries",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				entry := snapEntryOf(snap, harness.EntryAssistant)
				entry.Payload = editPayloadObject(t, entry.Payload, func(obj map[string]json.RawMessage) {
					obj["usage"] = json.RawMessage(`{"input_tokens":4,"cached_input_tokens":4,"output_tokens":4}`)
				})
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["usage"] = json.RawMessage(`{"by_model":[{"model":{"provider":"prov","model":"gpt-x"},"usage":{"input_tokens":4,"cached_input_tokens":4,"output_tokens":4}}]}`)
				})
			},
		},
		{
			name: "validator: duplicate entry identity",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				input := snapEntryOf(snap, harness.EntryInput)
				assistant := snapEntryOf(snap, harness.EntryAssistant)
				assistant.ID = input.ID
			},
		},
		{
			name: "validator: duplicate operation identity",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				input := snapEntryOf(snap, harness.EntryInput)
				snapAppendOperation(snap, sessionID, "op-1", rawRunningOperationPayload(sessionID, "op-1", input.ID))
			},
		},
		{
			name: "validator: stored hook_result record",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				snapAppendEntry(snap, sessionID, newRawSessionID(t), "op-1", harness.EntryHookResult, `{}`)
			},
		},
		{
			name: "validator: stored compaction record",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				snapAppendEntry(snap, sessionID, newRawSessionID(t), "", harness.EntryCompaction, `{}`)
			},
		},
		{
			name: "validator: root with operationless input",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				snapCopyEntry(t, snapEntryOf(snap, harness.EntryInput))
			},
		},
		{
			name: "validator: fork with operationless entry after owned",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				snapMakeFork(t, snap)
				copied := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, copied, "", harness.EntryInput, rawCopiedInputPayload(sessionID, copied))
			},
		},
		{
			name: "validator: admitted entry owned by another operation",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				input := snapEntryOf(snap, harness.EntryInput)
				settlement := newRawSessionID(t)
				sessReg := snapRegisterOf(snap, harness.RegisterSession, "")
				sessReg.Payload = editStateObject(t, sessReg.Payload, func(state map[string]json.RawMessage) {
					state["current_operation_id"] = json.RawMessage(`"op-2"`)
				})
				op1 := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				op1.Payload = json.RawMessage(rawSettledOperationPayload(sessionID, "op-1", input.ID, settlement))
				snapAppendEntry(snap, sessionID, settlement, "op-1", harness.EntryOperationSettlement,
					rawSettlementPayload(sessionID, settlement, "op-1"))
				snapAppendOperation(snap, sessionID, "op-2", rawRunningOperationPayload(sessionID, "op-2", input.ID))
			},
		},
		{
			name: "validator: admitted entry that is a copied input",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				snapMakeFork(t, snap)
				snapCopyEntry(t, snapEntryOf(snap, harness.EntryInput))
			},
		},
		{
			name: "validator: duplicate reservation across two operations",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				reserved := snapReservedResultID(t, snap)
				input2, assistant2, settlement2 := newRawSessionID(t), newRawSessionID(t), newRawSessionID(t)
				snapAppendOperation(snap, sessionID, "op-2", rawSettledOperationPayload(sessionID, "op-2", input2, settlement2))
				snapAppendEntry(snap, sessionID, input2, "op-2", harness.EntryInput, rawInputEntryPayload(sessionID, input2, "op-2"))
				snapAppendEntry(snap, sessionID, assistant2, "op-2", harness.EntryAssistant,
					rawAssistantEntryPayload(sessionID, assistant2, "op-2", [][2]string{{"call-9", reserved}}))
				snapAppendEntry(snap, sessionID, reserved, "op-2", harness.EntryToolResult,
					rawToolResultPayload(sessionID, reserved, "op-2", assistant2, "call-9"))
				snapAppendEntry(snap, sessionID, settlement2, "op-2", harness.EntryOperationSettlement,
					rawSettlementPayload(sessionID, settlement2, "op-2"))
			},
		},
		{
			name: "validator: reservation id names a committed input entry",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				input := snapEntryOf(snap, harness.EntryInput)
				assistant := snapEntryOf(snap, harness.EntryAssistant)
				assistant.Payload = editPayloadObject(t, assistant.Payload, func(obj map[string]json.RawMessage) {
					obj["tool_calls"] = editJSONArray(t, obj["tool_calls"], func(calls []map[string]json.RawMessage) {
						calls[0]["result_entry_id"] = json.RawMessage(`"` + input.ID + `"`)
					})
				})
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["pending_tool_calls"] = editJSONArray(t, state["pending_tool_calls"], func(pending []map[string]json.RawMessage) {
						pending[0]["result_entry_id"] = json.RawMessage(`"` + input.ID + `"`)
					})
				})
			},
		},
		{
			name: "validator: copied result answering an unpublished call",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				snapMakeFork(t, snap)
				snapStripOperation(t, snap)
				assistant, result := newRawSessionID(t), newRawSessionID(t)
				snapAppendEntry(snap, sessionID, assistant, "", harness.EntryAssistant, rawCopiedAssistantPayload(sessionID, assistant, nil))
				snapAppendEntry(snap, sessionID, result, "", harness.EntryToolResult, rawCopiedResultPayload(sessionID, result, assistant, "call-1"))
			},
		},
		{
			name: "validator: copied result whose id differs from the reservation",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				snapMakeFork(t, snap)
				snapStripOperation(t, snap)
				assistant, result := newRawSessionID(t), newRawSessionID(t)
				snapAppendEntry(snap, sessionID, assistant, "", harness.EntryAssistant,
					rawCopiedAssistantPayload(sessionID, assistant, [][2]string{{"call-1", corruptSiblingOtherEntry}}))
				snapAppendEntry(snap, sessionID, result, "", harness.EntryToolResult, rawCopiedResultPayload(sessionID, result, assistant, "call-1"))
			},
		},
		{
			name: "validator: model effect reserves a committed entry",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				input := snapEntryOf(snap, harness.EntryInput)
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["active_effect"] = json.RawMessage(fmt.Sprintf(`{"kind":"model","result_entry_id":%q}`, input.ID))
				})
			},
		},
		{
			name: "validator: model effect reserves a tool call reservation",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reserved := snapReservedResultID(t, snap)
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["active_effect"] = json.RawMessage(fmt.Sprintf(`{"kind":"model","result_entry_id":%q}`, reserved))
				})
			},
		},
		{
			name: "validator: terminal op carries a second settlement entry",
			seed: seedCorruptSiblingSettled,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				second := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, second, "op-1", harness.EntryOperationSettlement,
					rawSettlementPayload(sessionID, second, "op-1"))
			},
		},
		{
			name: "validator: copied call reservation without a copied result",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				snapMakeFork(t, snap)
				snapStripOperation(t, snap)
				assistant := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, assistant, "", harness.EntryAssistant,
					rawCopiedAssistantPayload(sessionID, assistant, [][2]string{{"call-1", corruptSiblingOtherEntry}}))
			},
		},
		{
			name: "validator: register and settlement detail disagree",
			seed: seedCorruptSiblingSettled,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["status"] = json.RawMessage(`"failure"`)
					state["terminal"] = editPayloadObject(t, state["terminal"], func(terminal map[string]json.RawMessage) {
						terminal["detail"] = json.RawMessage(`"boom"`)
					})
				})
				entry := snapEntryOf(snap, harness.EntryOperationSettlement)
				entry.Payload = editPayloadObject(t, entry.Payload, func(obj map[string]json.RawMessage) {
					obj["status"] = json.RawMessage(`"failure"`)
					obj["detail"] = json.RawMessage(`"other"`)
				})
			},
		},
		{
			name: "validator: fork-copied assistant keeps usage",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				snapMakeFork(t, snap)
				snapStripOperation(t, snap)
				snap.entries = nil // the copied prefix is one assistant entry
				assistant := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, assistant, "", harness.EntryAssistant,
					string(editPayloadObject(t, json.RawMessage(rawCopiedAssistantPayload(sessionID, assistant, nil)), func(obj map[string]json.RawMessage) {
						obj["usage"] = json.RawMessage(`{"input_tokens":2,"cached_input_tokens":0,"output_tokens":0}`)
					})))
			},
		},
		{
			name: "validator: orphan entries and registers without session register",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				snapDropSessionRegister(snap)
			},
		},
	}

	// The entry-wire inventory: eight rejected payload mutations for every
	// entry kind, each on a valid graph carrying that kind.
	entryKinds := []struct {
		name      string
		seed      func(t *testing.T, store harness.Storage) string
		kind      harness.EntryKind
		container string
		wrong     string
	}{
		{name: "input", seed: seedCorruptSiblingRunning, kind: harness.EntryInput, container: "content", wrong: `{}`},
		{name: "assistant", seed: seedCorruptSiblingPendingCall, kind: harness.EntryAssistant, container: "source", wrong: `"prov/gpt-x"`},
		{name: "tool_result", seed: seedCorruptSiblingWithResult, kind: harness.EntryToolResult, container: "assistant_entry", wrong: `"x"`},
		{name: "signal", seed: seedCorruptSiblingWithSignal, kind: harness.EntrySignal, container: "related_operation", wrong: `[]`},
		{name: "operation_settlement", seed: seedCorruptSiblingSettled, kind: harness.EntryOperationSettlement, container: "usage", wrong: `0`},
	}
	wireMutations := []struct {
		name   string
		mutate func(t *testing.T, raw json.RawMessage, container, wrong string) json.RawMessage
	}{
		{"unknown key", func(t *testing.T, raw json.RawMessage, _, _ string) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["bogus"] = json.RawMessage(`1`)
			})
		}},
		{"miscased key", func(t *testing.T, raw json.RawMessage, _, _ string) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["Session_ID"] = obj["session_id"]
				delete(obj, "session_id")
			})
		}},
		{"null container", func(t *testing.T, raw json.RawMessage, container, _ string) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj[container] = json.RawMessage(`null`)
			})
		}},
		{"wrong container", func(t *testing.T, raw json.RawMessage, container, wrong string) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj[container] = json.RawMessage(wrong)
			})
		}},
		{"session mismatch", func(t *testing.T, raw json.RawMessage, _, _ string) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["session_id"] = json.RawMessage(`"` + corruptSiblingOtherSession + `"`)
			})
		}},
		{"entry mismatch", func(t *testing.T, raw json.RawMessage, _, _ string) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["entry_id"] = json.RawMessage(`"` + corruptSiblingOtherEntry + `"`)
			})
		}},
		{"operation mismatch", func(t *testing.T, raw json.RawMessage, _, _ string) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["operation_id"] = json.RawMessage(`"ghost"`)
			})
		}},
		{"trailing value", func(_ *testing.T, raw json.RawMessage, _, _ string) json.RawMessage {
			return json.RawMessage(string(raw) + ` {"x":1}`)
		}},
	}
	for _, kind := range entryKinds {
		for _, mutation := range wireMutations {
			rows = append(rows, corruptSiblingRow{
				name: "entry wire " + kind.name + ": " + mutation.name,
				seed: kind.seed,
				mutate: func(t *testing.T, snap *sessionSnapshot) {
					entry := snapEntryOf(snap, kind.kind)
					entry.Payload = mutation.mutate(t, entry.Payload, kind.container, kind.wrong)
				},
			})
		}
	}

	// The register-wire inventory: rejected session- and operation-register
	// payload mutations on the plain running graph.
	sessionWireMutations := []struct {
		name string
		edit func(t *testing.T, raw json.RawMessage) json.RawMessage
	}{
		{"unknown key", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["bogus"] = json.RawMessage(`1`)
			})
		}},
		{"miscased key", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["Identity"] = obj["identity"]
				delete(obj, "identity")
			})
		}},
		{"null identity", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["identity"] = json.RawMessage(`null`)
			})
		}},
		{"wrong identity container", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["identity"] = json.RawMessage(`[]`)
			})
		}},
		{"null state", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["state"] = json.RawMessage(`null`)
			})
		}},
		{"identity mismatch", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["identity"] = editPayloadObject(t, obj["identity"], func(identity map[string]json.RawMessage) {
					identity["session_id"] = json.RawMessage(`"` + corruptSiblingOtherSession + `"`)
				})
			})
		}},
	}
	for _, mutation := range sessionWireMutations {
		rows = append(rows, corruptSiblingRow{
			name: "session register wire: " + mutation.name,
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterSession, "")
				reg.Payload = mutation.edit(t, reg.Payload)
			},
		})
	}
	operationWireMutations := []struct {
		name string
		edit func(t *testing.T, raw json.RawMessage) json.RawMessage
	}{
		{"unknown key", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["bogus"] = json.RawMessage(`1`)
			})
		}},
		{"miscased key", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["Admission"] = obj["admission"]
				delete(obj, "admission")
			})
		}},
		{"null admission", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["admission"] = json.RawMessage(`null`)
			})
		}},
		{"wrong state container", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editPayloadObject(t, raw, func(obj map[string]json.RawMessage) {
				obj["state"] = json.RawMessage(`[]`)
			})
		}},
		{"operation mismatch", func(t *testing.T, raw json.RawMessage) json.RawMessage {
			return editOperationAdmission(t, raw, func(admission map[string]json.RawMessage) {
				admission["operation_id"] = json.RawMessage(`"ghost"`)
			})
		}},
	}
	for _, mutation := range operationWireMutations {
		rows = append(rows, corruptSiblingRow{
			name: "operation register wire: " + mutation.name,
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = mutation.edit(t, reg.Payload)
			},
		})
	}

	for _, row := range rows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, store harness.Storage) {
				ctx := context.Background()

				// The unmutated graph materializes on a separate fresh store.
				baseline := storage.NewMemory()
				baselineID := row.seed(t, baseline)
				if _, err := harnessOver(t, baseline).ReadSession(ctx, baselineID); err != nil {
					t.Fatalf("unmutated graph does not materialize: %v", err)
				}

				targetID := row.seed(t, store)
				snap := snapshotSession(t, store, targetID)
				row.mutate(t, &snap)
				siblingID, _, _ := seedRunningOperationShape(t, store, "", "op-sib", nil) // a valid quiet running sibling
				before := snapshotSession(t, store, targetID)
				pre := recoverEntries(t, store, siblingID)

				wrapped := &corruptSiblingStore{Storage: store, session: targetID, registers: snap.registers, entries: snap.entries}
				if err := harness.Recover(ctx, wrapped); err != nil {
					t.Fatalf("recover with %q corruption = %v, want the corruption left in place and the pass completed", row.name, err)
				}
				assertSessionUnchanged(t, store, targetID, before)
				assertRecoveredOperation(t, store, siblingID, "op-sib", pre, "", nil)

				h := harnessOver(t, wrapped) // constructed only after recovery: materialization verification
				if _, err := h.ReadSession(ctx, targetID); !errors.Is(err, harness.ErrCorrupt) {
					t.Fatalf("corrupt session after recovery = err %v, want the corruption error", err)
				}
				rec, err := h.ReadOperation(ctx, siblingID, "op-sib")
				if err != nil || rec.State.Status != harness.OperationInterruption || rec.State.Terminal == nil ||
					rec.State.Terminal.Detail != recoveredInterruptionDetail {
					t.Fatalf("repaired sibling after recovery = %+v err %v, want the terminal interruption with the recovery detail", rec, err)
				}
			})
		})
	}
}
