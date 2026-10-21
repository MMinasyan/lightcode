// External-package public Harness suite: every fixture drives the landed
// public API only — New, CreateSession, Submit, ReadSession, ReadOperation,
// ChangeAgentType, ReopenSession, ArchiveSession, DeleteSession, SweepSession,
// Fork, and Wait — over both storage implementations, covering the preparation,
// admission, idempotency, context, effect, settlement, usage, terminal,
// coordination, lifetime, lifecycle, fork, and package/production-isolation
// rows through public operations.
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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

var publicModelRef = model.ModelRef{Provider: "prov", Model: "gpt-x"}

// publicCapture is the fixed prepared capture of the suite.
func publicCapture() harness.ExecutionCapture {
	return harness.ExecutionCapture{
		ConfigurationRevision: "rev-1",
		Model:                 publicModelRef,
		ContextWindow:         4096,
		OutputReserve:         2048,
		SystemPrompt:          "system",
		Tools: []model.ToolDefinition{{
			Name:        "echo",
			Description: "echoes",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
		Compact: harness.CompactCapture{
			Model:        publicModelRef,
			SystemPrompt: "summarize",
		},
	}
}

// publicNormalize is the suite's required pure normalizer: it accepts exactly
// one non-null JSON object and returns its compact encoding.
func publicNormalize(call model.ToolCall) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &obj); err != nil || obj == nil {
		return nil, errors.New("arguments must be one non-null JSON object")
	}
	return json.Marshal(obj)
}

// publicExecution is the suite's ordinary opened execution: the two actual
// model callbacks, the suite's required normalizer, and the immediate
// "no tools" result plan.
func publicExecution(conversation, compact func(context.Context, model.Request) (model.Stream, error)) harness.Execution {
	return harness.Execution{
		Model:         conversation,
		CompactModel:  compact,
		NormalizeTool: publicNormalize,
		Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
			return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"}}}
		},
	}
}

// publicPermission is one built-in-allowed declaration for the suite's
// effect-producing fixture plans: command.run allows any target.
var publicPermission = []harness.PermissionRequest{{Permission: "command.run", Target: "fixture"}}

// publicTurnStream is one fake accepted model stream assembling to the
// fixture completed turn: text "done", the given ordered calls, and the
// fixture usage reported last.
func publicTurnStream(calls ...string) *publicScriptStream {
	return publicUsageTurnStream(model.Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2}, calls...)
}

// publicUsageTurnStream is publicTurnStream carrying a specific usage count.
func publicUsageTurnStream(usage model.Usage, calls ...string) *publicScriptStream {
	d := model.StreamDelta{
		HasChoice:        true,
		Role:             "assistant",
		ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "done"}},
		FinishReason:     "stop",
	}
	if len(calls) > 0 {
		d.FinishReason = "tool_calls"
		for i, id := range calls {
			position := i
			d.ToolFragments = append(d.ToolFragments, model.ToolCallFragment{
				Position:         &position,
				ID:               id,
				Name:             "echo",
				ArgumentFragment: `{"x":1}`,
			})
		}
	}
	return &publicScriptStream{deltas: []model.StreamDelta{d, {Usage: &usage}}}
}

// publicScriptStream is one fake accepted model stream: scripted deltas in
// order, then either its read failure or io.EOF. A non-nil onExhausted hook
// runs on the read after the last delta, before the report.
type publicScriptStream struct {
	deltas      []model.StreamDelta
	err         error // non-EOF read failure yielded after the deltas when non-nil
	i           int
	onExhausted func()
}

func (s *publicScriptStream) Recv() (model.StreamDelta, error) {
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

// publicPartialTurnStream is one accepted stream whose assembly errors while
// retaining the "partial" text: the fixture errored continuation turn.
func publicPartialTurnStream() *publicScriptStream {
	return &publicScriptStream{
		deltas: []model.StreamDelta{
			{HasChoice: true, Role: "assistant", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "partial"}}},
			{Usage: &model.Usage{InputTokens: 5, OutputTokens: 7}},
		},
		err: errors.New("provider failure"),
	}
}

func (s *publicScriptStream) Close() error { return nil }

// publicAttempt is one scripted physical attempt: the stream of the accepted
// attempt, or its pre-acceptance failure whose text becomes the settled
// terminal detail.
type publicAttempt struct {
	stream model.Stream
	err    error
}

// publicTurn scripts one accepted stream assembling to the fixture completed
// turn with the given calls.
func publicTurn(calls ...string) publicAttempt {
	return publicAttempt{stream: publicTurnStream(calls...)}
}

// publicFail scripts one pre-acceptance attempt failure.
func publicFail(detail string) publicAttempt {
	return publicAttempt{err: errors.New(detail)}
}

// scriptModel is the scripted physical model request of the suite: it records
// every projected request, signals each arrival, optionally parks every
// invocation until its gate closes, and answers from a scripted attempt list.
type scriptModel struct {
	mu       sync.Mutex
	requests []model.Request
	arrived  chan struct{}
	gate     chan struct{}
	attempts []publicAttempt
}

func newScriptModel(attempts ...publicAttempt) *scriptModel {
	return &scriptModel{arrived: make(chan struct{}, 16), attempts: attempts}
}

func (s *scriptModel) effect(_ context.Context, req model.Request) (model.Stream, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	gate := s.gate
	s.mu.Unlock()
	s.arrived <- struct{}{}
	if gate != nil {
		<-gate
	}
	var next publicAttempt
	if len(s.attempts) > 0 {
		next = s.attempts[0]
		s.attempts = s.attempts[1:]
	} else {
		next = publicTurn()
	}
	return next.stream, next.err
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

	// opens counts the opener invocations of the fixture's racePrepared
	// executions: a discarded prepared execution must never open.
	opens atomic.Int64

	// prepareHook, when non-nil, answers one preparation call with its own
	// result or error; the call index counts from zero.
	prepareHook func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error)
}

func newPublicFixture(t *testing.T, store harness.Storage, script *scriptModel, modelFn func(context.Context, model.Request) (model.Stream, error)) *publicFixture {
	return newPublicFixtureWithJobs(t, store, nil, script, modelFn)
}

// newPublicFixtureWithJobs is the fixture's core builder: the same scripted
// preparation over a Harness whose optional Jobs seam is installed.
func newPublicFixtureWithJobs(t *testing.T, store harness.Storage, stopper harness.JobStopper, script *scriptModel, modelFn func(context.Context, model.Request) (model.Stream, error)) *publicFixture {
	t.Helper()
	if modelFn == nil {
		modelFn = script.effect
	}
	f := &publicFixture{store: store, cancel: func() {}, prepare: make(chan struct{}, 16), model: script}
	prepared := harness.PreparedExecution{
		Capture: publicCapture(),
		Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
			return publicExecution(modelFn, modelFn), nil
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
	}, Jobs: stopper})
	testassert.NoError(t, err)
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
	testassert.NoError(t, err)
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

// awaitTerminal polls one Operation until its terminal settlement commits,
// bounding the wait: a failing attempt's settlement must not race the
// convergence cancellation the surrounding assertions rely on.
func awaitTerminal(t *testing.T, h *harness.Harness, session, operation string) harness.OperationRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := h.ReadOperation(context.Background(), session, operation)
		testassert.NoError(t, err)
		if rec.State.Terminal != nil {
			return rec
		}
		testassert.Require(t, !time.Now().After(deadline), "operation %s never settled within the wait bound", operation)
		time.Sleep(time.Millisecond)
	}
}

// eachStore runs one fixture against memory and a temporary SQLite store.
func eachStore(t *testing.T, run func(t *testing.T, store harness.Storage)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		run(t, storage.NewMemory())
	})
	t.Run("sqlite", func(t *testing.T) {
		store, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "lightcode.db"))
		testassert.NoError(t, err)
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
			publicAttempt{stream: publicUsageTurnStream(model.Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2})},
			publicFail("second turn failed"),
		)
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)

		res, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
		testassert.Require(t, err == nil && res.Disposition == harness.DispositionAdmitted && res.Operation != nil, "first submit = %+v err %v, want admitted", res, err)
		rec := res.Operation
		testassert.Require(t, rec.State.Status == harness.OperationRunning && rec.Admission.AgentType == "coder" && rec.Admission.Execution.ConfigurationRevision == "rev-1" && rec.Admission.Execution.Model == publicModelRef, "admitted record = %+v", rec)
		<-script.arrived // parked at the first model boundary: the Session is active
		{
			got := texts(script.seen()[0])
			testassert.Require(t, got[0] == "system" && got[1] == "hello" && len(got) == 2, "first projection = %v, want the captured system prompt and the admitted input", got)
		}
		{
			queued, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1")
			testassert.Require(t, err == nil && queued.Disposition == harness.DispositionQueued && queued.Operation == nil, "active queued submit = %+v err %v, want queued", queued, err)
		}

		script.releaseGate()
		<-f.prepare // the drain admitted the queued item: op-1's terminal committed
		<-script.arrived
		{
			got := texts(script.seen()[1])
			testassert.Require(t, got[0] == "system" && got[1] == "hello" && got[2] == "done" && got[3] == "queued-1", "second projection = %v, want the first turn's committed history before the queued input", got)
		}
		awaitTerminal(t, f.h, session, "op-2") // the settlement commits before the convergence cancellation
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}

		first, err := f.h.ReadOperation(context.Background(), session, "op-1")
		testassert.Require(t, err == nil && first.State.Status == harness.OperationSuccess && first.State.Terminal != nil, "first operation = %+v err %v, want terminal success", first, err)
		want := harness.UsageTotals{ByModel: []harness.ModelUsage{{
			Model: publicModelRef,
			Usage: harness.UsageCount{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2},
		}}}
		testassert.Require(t, len(first.State.Usage.ByModel) == 1 && first.State.Usage.ByModel[0] == want.ByModel[0], "operation usage = %+v, want the reported counts", first.State.Usage)
		sessionHeader, err := f.h.ReadSessionHeader(context.Background(), session)
		testassert.Require(t, err == nil && sessionHeader.CurrentOperationID == "", "session after convergence = %+v err %v", sessionHeader, err)
		sessionView, err := f.h.ReadSessionView(context.Background(), session)
		testassert.NoError(t, err)
		testassert.Require(t, len(sessionView.TotalUsage.ByModel) == 1 && sessionView.TotalUsage.ByModel[0] == want.ByModel[0], "session usage = %+v, want the same totals", sessionView.TotalUsage)
		second, err := f.h.ReadOperation(context.Background(), session, "op-2")
		testassert.Require(t, err == nil && second.State.Status == harness.OperationFailure && second.State.Terminal != nil && second.State.Terminal.Detail == "second turn failed", "drained operation = %+v err %v, want terminal failure with its detail", second, err)
		calls := f.preparationCalls()
		testassert.Require(t, len(calls) == 2 && calls[0].Session.AgentType == "coder" && calls[1].Session.AgentType == "coder", "preparation calls = %+v, want one per admission with the Session type", calls)
	})
}

// TestPublicSteeringContinuation proves the coordination row through public
// operations: regular input submitted while an Operation is active enters the
// steering buffer, the predecessor settles quiet success at the boundary, and
// the steering arrives as its own successor Operation whose first request
// projects it after the predecessor's history — with the queued head draining
// after the successor's terminal.
func TestPublicSteeringContinuation(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)

		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived // parked at the first model boundary
		{
			steered, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "steer")
			testassert.Require(t, err == nil && steered.Disposition == harness.DispositionSteering && steered.Operation == nil, "steering submit = %+v err %v, want steering", steered, err)
		}
		{
			queued, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "queued-1")
			testassert.Require(t, err == nil && queued.Disposition == harness.DispositionQueued, "queued submit = %+v err %v, want queued", queued, err)
		}
		script.releaseGate()
		<-f.prepare      // the drain admitted the steering item: op-1's quiet terminal committed
		<-script.arrived // the steering item's own Operation makes the next request
		{
			got := texts(script.seen()[1])
			testassert.Require(t, got[0] == "system" && got[1] == "hello" && got[2] == "done" && got[3] == "steer", "successor projection = %v, want the steering as its own Operation's first request", got)
		}
		awaitTerminal(t, f.h, session, "op-2")
		<-f.prepare // the queued item drains after the successor's terminal
		<-script.arrived
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
		rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
		testassert.Require(t, err == nil && rec.State.Status == harness.OperationSuccess && rec.State.Terminal != nil && rec.State.Terminal.Detail == "", "settled predecessor = %+v err %v, want quiet success with empty detail", rec, err)
		successor, err := f.h.ReadOperation(context.Background(), session, "op-2")
		testassert.Require(t, err == nil && successor.State.Status == harness.OperationSuccess, "steering operation = %+v err %v, want the successor's success", successor, err)
		for _, fact := range readHistoryFacts(t, f.h, session) {
			if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 && fact.Input.Content[0].Text == "steer" {
				testassert.Equal(t, fact.OperationID, "op-2")
			}
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
		testassert.Require(t, err == nil && first.Disposition == harness.DispositionAdmitted && first.Operation != nil, "first submit = %+v err %v, want admitted", first, err)
		<-script.arrived // the Operation stays active

		retry, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "changed")
		testassert.Require(t, err == nil && retry.Disposition == harness.DispositionExisting && retry.Operation != nil && retry.Operation.Admission.AdmittedEntry == first.Operation.Admission.AdmittedEntry, "active retry = %+v err %v, want the first record", retry, err)
		queuedRetry, err := submit(t, f.h, session, "op-1", harness.MessageModeQueued, "changed-again")
		testassert.Require(t, err == nil && queuedRetry.Disposition == harness.DispositionExisting && queuedRetry.Operation != nil, "queued retry = %+v err %v, want existing before routing", queuedRetry, err)
		other := createSession(t, f.h)
		{
			_, err := submit(t, f.h, other, "op-1", harness.MessageModeRegular, "reused")
			testassert.Require(t, err != nil, "cross-session reuse succeeded, want an error")
		}

		script.releaseGate()
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
		{
			_, err := f.h.ReadOperation(context.Background(), other, "op-1")
			testassert.Require(t, err != nil, "cross-session reuse published an operation, want nothing")
		}

		t.Run("reuse after delete", func(t *testing.T) {
			script := newScriptModel(
				publicFail("source turn settled"), // model-originated terminal before the drain
				publicFail("reused turn settled"), // durable regardless of later cancellation
			)
			gate := make(chan struct{})
			script.gate = gate
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			prepared := scriptPrepared(script)
			f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
				if call == 1 { // the drained queued item's preparation fails: the attempt is final for it
					return harness.PreparedExecution{}, fmt.Errorf("preparation broke")
				}
				return prepared, nil
			}
			source := createSession(t, f.h)

			{
				_, err := submit(t, f.h, source, "reuse-1", harness.MessageModeRegular, "first")
				testassert.NoError(t, err)
			}
			<-f.prepare      // the first admission prepared
			<-script.arrived // parked at the model boundary: reuse-1 is running
			{
				_, err := submit(t, f.h, source, "reuse-x", harness.MessageModeQueued, "queued")
				testassert.NoError(t, err)
			}
			script.releaseGate()
			<-f.prepare // barrier: the drain reached the queued item's preparation — reuse-1's terminal committed

			// the failed preparation is final for the item and the drain retires, leaving
			// the source Session idle; ArchiveSession's waitIdle joins that retirement
			{
				_, err := f.h.ArchiveSession(context.Background(), source)
				testassert.NoError(t, err)
			}
			{
				err := f.h.DeleteSession(context.Background(), source)
				testassert.NoError(t, err)
			}

			other := createSession(t, f.h)
			res, err := submit(t, f.h, other, "reuse-1", harness.MessageModeRegular, "reused")
			testassert.Require(t, err == nil && res.Disposition == harness.DispositionAdmitted && res.Operation != nil, "reuse after delete = %+v err %v, want admitted in another session", res, err)
			<-script.arrived                        // the reused ID's Operation reached its model boundary
			awaitTerminal(t, f.h, other, "reuse-1") // the settlement commits before the convergence cancellation
			{
				err := converge(t, f)
				testassert.NoError(t, err)
			}
			rec, err := f.h.ReadOperation(context.Background(), other, "reuse-1")
			testassert.Require(t, err == nil && rec.State.Status == harness.OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == "reused turn settled", "reused operation = %+v err %v, want the model-originated terminal in the other session", rec, err)
		})
	})
}

// waitContext observes one concurrent Submit's caller context: consulted
// closes the first time Done is consulted, which for a Submit is the
// reservation wait — the deterministic observation that the caller is parked
// on the Session's admission reservation.
type waitContext struct {
	context.Context
	once      sync.Once
	consulted chan struct{}
}

func (w *waitContext) Done() <-chan struct{} {
	w.once.Do(func() { close(w.consulted) })
	return w.Context.Done()
}

// scriptPrepared returns the suite's prepared execution for the script's own
// model effect.
func scriptPrepared(script *scriptModel) harness.PreparedExecution {
	return harness.PreparedExecution{
		Capture: publicCapture(),
		Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
			return publicExecution(script.effect, script.effect), nil
		},
	}
}

// gateFirstPreparation parks the first preparation call until release closes:
// the winning admission holds the Session's reservation inside its gated
// preparation while the test parks the rest of the choreography.
func gateFirstPreparation(f *publicFixture, prepared harness.PreparedExecution, release <-chan struct{}) {
	f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
		if call == 0 {
			<-release
		}
		return prepared, nil
	}
}

// asyncSubmitOutcome is one concurrent Submit's result.
type asyncSubmitOutcome struct {
	res harness.SubmitResult
	err error
}

// submitAsync runs one public Submit off the test goroutine.
func submitAsync(ctx context.Context, h *harness.Harness, sessionID, operationID string, mode harness.MessageMode, text string) <-chan asyncSubmitOutcome {
	done := make(chan asyncSubmitOutcome, 1)
	go func() {
		res, err := h.Submit(ctx, harness.SubmitRequest{
			SessionID:   sessionID,
			OperationID: operationID,
			Origin:      harness.InputOriginUser,
			Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
			Mode:        mode,
		})
		done <- asyncSubmitOutcome{res: res, err: err}
	}()
	return done
}

// TestPublicSubmitRoutesAfterReservationWinner proves the concurrent-routing
// row through public operations: while one admission is parked inside its
// gated preparation, concurrent Submits of distinct IDs wait on the Session's
// reservation — consulting their caller contexts — and after the winner's
// admission completes they route as steering and queued, without a busy
// error, a second preparation, or a second active Operation while buffered.
func TestPublicSubmitRoutesAfterReservationWinner(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		script.gate = make(chan struct{}) // the first model stays gated: the winner's Operation stays active
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)
		releasePrep := make(chan struct{})
		gateFirstPreparation(f, scriptPrepared(script), releasePrep)

		winner := submitAsync(context.Background(), f.h, session, "op-1", harness.MessageModeRegular, "first")
		<-f.prepare // the winner is parked inside its gated preparation, holding the reservation

		waitSteer := &waitContext{Context: context.Background(), consulted: make(chan struct{})}
		steerer := submitAsync(waitSteer, f.h, session, "op-2", harness.MessageModeRegular, "steer")
		<-waitSteer.consulted // parked consulting its context in the reservation wait

		waitQueued := &waitContext{Context: context.Background(), consulted: make(chan struct{})}
		queuer := submitAsync(waitQueued, f.h, session, "op-3", harness.MessageModeQueued, "queued")
		<-waitQueued.consulted

		close(releasePrep) // the winner publishes and installs its execution; its model stays gated

		out := <-winner
		testassert.Require(t, out.err == nil && out.res.Disposition == harness.DispositionAdmitted && out.res.Operation != nil, "winning submit = %+v err %v, want admitted", out.res, out.err)
		<-script.arrived // the winner is parked at its model boundary: the Session stays active

		out = <-steerer
		testassert.Require(t, out.err == nil && out.res.Disposition == harness.DispositionSteering && out.res.Operation == nil, "waiting regular submit = %+v err %v, want steering after the winner's admission", out.res, out.err)
		out = <-queuer
		testassert.Require(t, out.err == nil && out.res.Disposition == harness.DispositionQueued && out.res.Operation == nil, "waiting queued submit = %+v err %v, want queued after the winner's admission", out.res, out.err)
		{
			calls := f.preparationCalls()
			testassert.Equal(t, len(calls), 1)
		}
		{
			_, err := f.h.ReadOperation(context.Background(), session, "op-2")
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "buffered steering operation read = err %v, want ErrNotFound while buffered", err)
		}
		{
			_, err := f.h.ReadOperation(context.Background(), session, "op-3")
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "buffered queued operation read = err %v, want ErrNotFound while buffered", err)
		}
		rec, err := f.h.ReadSessionHeader(context.Background(), session)
		testassert.Require(t, err == nil && rec.CurrentOperationID == "op-1", "session while both items wait buffered = %+v err %v, want the winner as the one active Operation", rec, err)

		script.releaseGate() // every buffered item is delivered or discarded per the landed lifetime semantics
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
		{
			rec, err := f.h.ReadSessionHeader(context.Background(), session)
			testassert.Require(t, err == nil && rec.CurrentOperationID == "", "session after convergence = %+v err %v, want no operation left running", rec, err)
		}
	})
}

// TestPublicSubmitSameIDResolvesExistingUnderReservation proves the same-ID
// concurrent row: a same-ID Submit waiting on the reservation returns the
// first Operation as existing after the winner's admission, with no repeated
// preparation, no buffered duplicate, no independent execution, and the
// originally admitted content unchanged by the retry's.
func TestPublicSubmitSameIDResolvesExistingUnderReservation(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)
		releasePrep := make(chan struct{})
		gateFirstPreparation(f, scriptPrepared(script), releasePrep)

		winner := submitAsync(context.Background(), f.h, session, "op-1", harness.MessageModeRegular, "first")
		<-f.prepare // parked inside its gated preparation, holding the reservation

		waitRetry := &waitContext{Context: context.Background(), consulted: make(chan struct{})}
		retry := submitAsync(waitRetry, f.h, session, "op-1", harness.MessageModeRegular, "second")
		<-waitRetry.consulted // parked consulting its context in the reservation wait

		close(releasePrep)

		winOut := <-winner
		testassert.Require(t, winOut.err == nil && winOut.res.Disposition == harness.DispositionAdmitted && winOut.res.Operation != nil, "winning submit = %+v err %v, want admitted", winOut.res, winOut.err)
		<-script.arrived // the winner is parked at its model boundary
		out := <-retry
		testassert.Require(t, out.err == nil && out.res.Disposition == harness.DispositionExisting && out.res.Operation != nil && out.res.Operation.Admission.AdmittedEntry == winOut.res.Operation.Admission.AdmittedEntry, "same-ID retry = %+v err %v, want the first Operation as existing", out.res, out.err)
		{
			calls := f.preparationCalls()
			testassert.Equal(t, len(calls), 1)
		}
		{
			got := texts(script.seen()[0])
			testassert.Require(t, got[len(got)-1] == "first", "admitted projection = %v, want the originally admitted content, not the retry's", got)
		}
		{
			rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
			testassert.Require(t, err == nil && rec.State.Status == harness.OperationRunning, "same-ID operation = %+v err %v, want the one running Operation", rec, err)
		}
		{
			rec, err := f.h.ReadSessionHeader(context.Background(), session)
			testassert.Require(t, err == nil && rec.CurrentOperationID == "op-1", "session = %+v err %v, want the same ID still current", rec, err)
		}

		script.releaseGate() // the winner settles; no buffered duplicate exists to drain
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
		{
			calls := f.preparationCalls()
			testassert.Equal(t, len(calls), 1)
		}
		{
			rec, err := f.h.ReadSessionHeader(context.Background(), session)
			testassert.Require(t, err == nil && rec.CurrentOperationID == "", "session after convergence = %+v err %v, want no buffered duplicate delivered", rec, err)
		}
	})
}

// TestPublicSubmitCanceledWaiterPublishesNothing proves the cancellation row:
// a Submit canceled while waiting on the reservation returns a context error
// and publishes or buffers nothing, the winner's admission still completes
// afterward, and the canceled item is never delivered later.
func TestPublicSubmitCanceledWaiterPublishesNothing(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)
		releasePrep := make(chan struct{})
		gateFirstPreparation(f, scriptPrepared(script), releasePrep)

		winner := submitAsync(context.Background(), f.h, session, "op-1", harness.MessageModeRegular, "first")
		<-f.prepare // parked inside its gated preparation, holding the reservation

		waitCtx, cancelWait := context.WithCancel(context.Background())
		defer cancelWait()
		waiter := &waitContext{Context: waitCtx, consulted: make(chan struct{})}
		canceled := submitAsync(waiter, f.h, session, "op-2", harness.MessageModeRegular, "late")
		<-waiter.consulted // parked consulting its context in the reservation wait
		cancelWait()

		out := <-canceled
		testassert.Require(t, errors.Is(out.err, context.Canceled) && out.res.Operation == nil, "canceled waiter = %+v err %v, want a context error and no result", out.res, out.err)
		{
			calls := f.preparationCalls()
			testassert.Equal(t, len(calls), 1)
		}
		{
			_, err := f.h.ReadOperation(context.Background(), session, "op-2")
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "canceled operation read = err %v, want ErrNotFound: nothing published", err)
		}
		{
			rec, err := f.h.ReadSessionHeader(context.Background(), session)
			testassert.Require(t, err == nil && rec.CurrentOperationID == "", "session after the canceled waiter = %+v err %v, want nothing published", rec, err)
		}

		close(releasePrep) // the winner's publication wins after the waiter's cancellation
		out = <-winner
		testassert.Require(t, out.err == nil && out.res.Disposition == harness.DispositionAdmitted, "winner after a canceled waiter = %+v err %v, want admitted", out.res, out.err)
		<-script.arrived

		script.releaseGate()
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
		{
			calls := f.preparationCalls()
			testassert.Equal(t, len(calls), 1)
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
			publicTurn(),                      // op-1 succeeds
			publicFail("drained turn failed"), // op-2 fails
		)
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		session := createSession(t, f.h)

		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived
		{
			_, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1")
			testassert.NoError(t, err)
		}
		changed, err := f.h.ChangeAgentType(context.Background(), session, "reviewer")
		testassert.Require(t, err == nil && changed.State.CurrentAgentType == "reviewer", "ChangeAgentType = %+v err %v, want success during the blocked effect", changed, err)
		script.releaseGate()

		<-f.prepare // the drained admission prepared again
		<-script.arrived
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
		calls := f.preparationCalls()
		testassert.Require(t, len(calls) == 2 && calls[0].Session.AgentType == "coder" && calls[1].Session.AgentType == "reviewer", "preparation agent types = %+v, want the capture-time value then the changed one", calls)
		first, err := f.h.ReadOperation(context.Background(), session, "op-1")
		testassert.Require(t, err == nil && first.Admission.AgentType == "coder" && first.State.Status == harness.OperationSuccess, "first operation = %+v err %v, want the immutable capture and success", first, err)
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

		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived // parked inside the model effect
		{
			_, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "steer")
			testassert.NoError(t, err)
		}
		{
			_, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "queued-1")
			testassert.NoError(t, err)
		}

		waited := make(chan error, 1)
		go func() { waited <- f.h.Wait(context.Background()) }() // Wait begins before cancellation
		f.cancel()                                               // Harness loss closes admission and ends the run
		script.releaseGate()
		{
			err := <-waited
			testassert.NoError(t, err)
		}

		{
			_, err := submit(t, f.h, session, "op-4", harness.MessageModeRegular, "late")
			testassert.Require(t, err != nil, "submit after cancellation succeeded, want admission closed")
		}
		rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
		testassert.Require(t, err == nil && rec.State.Status == harness.OperationInterruption, "converged operation = %+v err %v, want interruption", rec, err)
		for _, id := range []string{"op-2", "op-3"} {
			{
				_, err := f.h.ReadOperation(context.Background(), session, id)
				testassert.Require(t, err != nil, "buffered item %q became an operation, want the buffers discarded", id)
			}
		}
		calls := f.preparationCalls()
		testassert.Equal(t, len(calls), 1)
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
			testassert.Require(t, err != nil, "Wait over a canceled wait context returned nil, want its error")
		case <-time.After(5 * time.Second):
			t.Fatalf("Wait ignored its own context cancellation")
		}
	})
}

// publicationCancelStore wraps one store to cancel one context at a chosen
// point of the next transaction: at its entry, before the store observes the
// call, or after a nil return, once the transaction has committed. The armed
// hook fires once and disarms itself.
type publicationCancelStore struct {
	harness.Storage
	atEntry     func()
	afterCommit func()
}

func (s *publicationCancelStore) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	if hook := s.atEntry; hook != nil {
		s.atEntry = nil
		hook()
		// the harness-combined publication context may lag the canceled parent
		// by one AfterFunc hop: wait until the context the transaction actually
		// runs on observes the cancellation
		deadline := time.Now().Add(5 * time.Second)
		for ctx.Err() == nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	err := s.Storage.Transact(ctx, fn)
	if err == nil {
		if hook := s.afterCommit; hook != nil {
			s.afterCommit = nil
			hook()
		}
	}
	return err
}

// submitRequest is one regular user-origin Submit request for the publication
// fixtures.
func submitRequest(sessionID, operationID, text string) harness.SubmitRequest {
	return harness.SubmitRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Origin:      harness.InputOriginUser,
		Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
		Mode:        harness.MessageModeRegular,
	}
}

// assertNothingAdmitted verifies the durable state of one aborted admission:
// the Session carries no entries and no Operation register.
func assertNothingAdmitted(t *testing.T, store harness.Storage, sessionID, operationID string) {
	t.Helper()
	{
		entries := recoverEntries(t, store, sessionID)
		testassert.Equal(t, len(entries), 0)
	}
	key := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID}
	{
		_, err := store.ReadRegister(context.Background(), key)
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "operation register after the aborted admission = err %v, want absence", err)
	}
}

// seedIdleSource completes one regular message on a fresh Session and leaves
// it idle, returning the source identity for the fork fixtures. The operation
// identity is globally unique across the store, so each seed passes its own.
func seedIdleSource(t *testing.T, store harness.Storage, operationID string) string {
	t.Helper()
	script := newScriptModel(publicTurn())
	f := newPublicFixture(t, store, script, nil)
	source := createSession(t, f.h)
	{
		_, err := submit(t, f.h, source, operationID, harness.MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-script.arrived
	{
		err := converge(t, f)
		testassert.NoError(t, err)
	}
	return source
}

// TestPublicPublicationContextLifetime proves the context-lifetime,
// publication-precedence, and caller-deadline rows on both stores: the
// publication transaction runs on the combined caller/Harness context, so
// cancellation of either context at the publication boundary aborts it —
// Submit publishes nothing, Fork leaves no destination, and preparation is
// never repeated — a caller deadline expiring during the gated publication
// returns DeadlineExceeded at the caller-facing boundary, and a committed
// publication wins a later caller cancellation and returns the committed
// result.
func TestPublicPublicationContextLifetime(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()

		t.Run("harness cancellation at Submit publication publishes nothing", func(t *testing.T) {
			probe := &publicationCancelStore{Storage: store}
			f := newPublicFixture(t, probe, newScriptModel(), nil)
			defer f.close()
			session := createSession(t, f.h)
			probe.atEntry = f.cancel // the Harness context dies as the publication transaction begins

			res, err := f.h.Submit(ctx, submitRequest(session, "op-a", "hello"))
			testassert.Require(t, errors.Is(err, context.Canceled), "submit whose publication lost the Harness context = %+v err %v, want a context error", res, err)
			testassert.Require(t, res.Operation == nil, "submit result = %+v, want no admitted operation", res)
			assertNothingAdmitted(t, store, session, "op-a")
			assertSessionIDs(t, store, session)
			{
				calls := f.preparationCalls()
				testassert.Equal(t, len(calls), 1)
			}
		})

		t.Run("caller cancellation at Submit publication publishes nothing", func(t *testing.T) {
			probe := &publicationCancelStore{Storage: store}
			f := newPublicFixture(t, probe, newScriptModel(), nil)
			defer f.close()
			session := createSession(t, f.h)
			caller, cancelCaller := context.WithCancel(ctx)
			defer cancelCaller()
			probe.atEntry = cancelCaller

			res, err := f.h.Submit(caller, submitRequest(session, "op-b", "hello"))
			testassert.Require(t, errors.Is(err, context.Canceled), "submit whose publication lost the caller context = %+v err %v, want a context error", res, err)
			testassert.Require(t, res.Operation == nil, "submit result = %+v, want no admitted operation", res)
			assertNothingAdmitted(t, store, session, "op-b")
			{
				calls := f.preparationCalls()
				testassert.Equal(t, len(calls), 1)
			}
		})

		t.Run("harness cancellation at Fork publication leaves no destination", func(t *testing.T) {
			source := seedIdleSource(t, store, "seed-op-1")
			boundary := forkEntryOf(t, store, source, harness.EntryInput, "seed-op-1").ID
			before, err := store.ListSessionIDs(ctx) // subtests share the store: no destination means an unchanged listing
			testassert.NoError(t, err)

			probe := &publicationCancelStore{Storage: store}
			f := newPublicFixture(t, probe, newScriptModel(), nil)
			defer f.close()
			probe.atEntry = f.cancel

			res, err := f.h.Fork(ctx, harness.ForkRequest{
				SourceSessionID: source,
				BoundaryEntryID: boundary,
				OperationID:     "fork-1",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.Require(t, errors.Is(err, context.Canceled), "fork whose publication lost the Harness context = %+v err %v, want a context error", res, err)
			after, err := store.ListSessionIDs(ctx)
			testassert.NoError(t, err)
			testassert.Require(t, len(after) == len(before), "session listing after the aborted fork = %v, want it unchanged at %v", after, before)
			for i := range after {
				testassert.Require(t, after[i] == before[i], "session listing after the aborted fork = %v, want it unchanged at %v", after, before)
			}
			{
				calls := f.preparationCalls()
				testassert.Equal(t, len(calls), 1)
			}
		})

		t.Run("caller cancellation at Fork publication leaves no destination", func(t *testing.T) {
			source := seedIdleSource(t, store, "seed-op-3")
			boundary := forkEntryOf(t, store, source, harness.EntryInput, "seed-op-3").ID
			before, err := store.ListSessionIDs(ctx) // subtests share the store: no destination means an unchanged listing
			testassert.NoError(t, err)

			probe := &publicationCancelStore{Storage: store}
			f := newPublicFixture(t, probe, newScriptModel(), nil)
			defer f.close()
			caller, cancelCaller := context.WithCancel(ctx)
			defer cancelCaller()
			probe.atEntry = cancelCaller

			res, err := f.h.Fork(caller, harness.ForkRequest{
				SourceSessionID: source,
				BoundaryEntryID: boundary,
				OperationID:     "fork-2",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.Require(t, errors.Is(err, context.Canceled), "fork whose publication lost the caller context = %+v err %v, want a context error", res, err)
			after, err := store.ListSessionIDs(ctx)
			testassert.NoError(t, err)
			testassert.Require(t, len(after) == len(before), "session listing after the aborted fork = %v, want it unchanged at %v", after, before)
			for i := range after {
				testassert.Require(t, after[i] == before[i], "session listing after the aborted fork = %v, want it unchanged at %v", after, before)
			}
			{
				calls := f.preparationCalls()
				testassert.Equal(t, len(calls), 1)
			}
		})

		t.Run("caller deadline expiring at Submit publication returns DeadlineExceeded", func(t *testing.T) {
			probe := &publicationCancelStore{Storage: store}
			f := newPublicFixture(t, probe, newScriptModel(), nil)
			defer f.close()
			session := createSession(t, f.h)
			caller, cancelCaller := context.WithTimeout(ctx, 50*time.Millisecond) // the caller's own native deadline
			defer cancelCaller()
			probe.atEntry = func() { <-caller.Done() } // publication gated until the caller deadline expires

			res, err := f.h.Submit(caller, submitRequest(session, "op-d", "hello"))
			testassert.Require(t, errors.Is(err, context.DeadlineExceeded), "submit whose publication lost the caller deadline = %+v err %v, want DeadlineExceeded", res, err)
			testassert.Require(t, res.Operation == nil, "submit result = %+v, want no admitted operation", res)
			assertNothingAdmitted(t, store, session, "op-d")
			{
				calls := f.preparationCalls()
				testassert.Equal(t, len(calls), 1)
			}
		})

		t.Run("caller deadline expiring at Fork publication returns DeadlineExceeded", func(t *testing.T) {
			source := seedIdleSource(t, store, "seed-op-4")
			boundary := forkEntryOf(t, store, source, harness.EntryInput, "seed-op-4").ID
			before, err := store.ListSessionIDs(ctx) // subtests share the store: no destination means an unchanged listing
			testassert.NoError(t, err)

			probe := &publicationCancelStore{Storage: store}
			f := newPublicFixture(t, probe, newScriptModel(), nil)
			defer f.close()
			caller, cancelCaller := context.WithTimeout(ctx, 50*time.Millisecond) // the caller's own native deadline
			defer cancelCaller()
			probe.atEntry = func() { <-caller.Done() } // publication gated until the caller deadline expires

			res, err := f.h.Fork(caller, harness.ForkRequest{
				SourceSessionID: source,
				BoundaryEntryID: boundary,
				OperationID:     "fork-3",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.Require(t, errors.Is(err, context.DeadlineExceeded), "fork whose publication lost the caller deadline = %+v err %v, want DeadlineExceeded", res, err)
			after, err := store.ListSessionIDs(ctx)
			testassert.NoError(t, err)
			testassert.Require(t, len(after) == len(before), "session listing after the aborted fork = %v, want it unchanged at %v", after, before)
			for i := range after {
				testassert.Require(t, after[i] == before[i], "session listing after the aborted fork = %v, want it unchanged at %v", after, before)
			}
			{
				calls := f.preparationCalls()
				testassert.Equal(t, len(calls), 1)
			}
		})

		t.Run("post-commit caller cancellation returns the committed admission", func(t *testing.T) {
			probe := &publicationCancelStore{Storage: store}
			script := newScriptModel(publicFail("committed turn")) // model-originated terminal, durable regardless of later cancellation
			f := newPublicFixture(t, probe, script, nil)
			defer f.close()
			session := createSession(t, f.h)
			caller, cancelCaller := context.WithCancel(ctx)
			defer cancelCaller()
			probe.afterCommit = cancelCaller

			res, err := f.h.Submit(caller, submitRequest(session, "op-c", "hello"))
			testassert.Require(t, err == nil && res.Disposition == harness.DispositionAdmitted && res.Operation != nil, "submit over a caller context canceled after the commit = %+v err %v, want the committed admission", res, err)
			testassert.Require(t, caller.Err() != nil, "the caller context was not canceled after the commit")
			<-script.arrived                       // the admitted execution reached its model boundary; its terminal commits in the effect's own transaction
			awaitTerminal(t, f.h, session, "op-c") // the settlement commits before the convergence cancellation
			{
				err := converge(t, f)
				testassert.NoError(t, err)
			}
			rec, err := f.h.ReadOperation(ctx, session, "op-c")
			testassert.Require(t, err == nil && rec.State.Status == harness.OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == "committed turn", "committed operation = %+v err %v, want the model-originated terminal", rec, err)
		})

		t.Run("post-commit caller cancellation returns the committed fork", func(t *testing.T) {
			source := seedIdleSource(t, store, "seed-op-2")
			boundary := forkEntryOf(t, store, source, harness.EntryInput, "seed-op-2").ID

			probe := &publicationCancelStore{Storage: store}
			script := newScriptModel(publicTurn())
			f := newPublicFixture(t, probe, script, nil)
			defer f.close()
			caller, cancelCaller := context.WithCancel(ctx)
			defer cancelCaller()
			probe.afterCommit = cancelCaller

			res, err := f.h.Fork(caller, harness.ForkRequest{
				SourceSessionID: source,
				BoundaryEntryID: boundary,
				OperationID:     "fork-2",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.NoError(t, err)
			dest := res.Session.Identity.SessionID
			testassert.Require(t, dest != source && res.Session.Identity.SourceSessionID == source && res.Operation.Admission.OperationID == "fork-2" && res.Operation.State.Status == harness.OperationRunning, "fork result = %+v, want the committed destination and its running fork operation", res)
			testassert.Require(t, caller.Err() != nil, "the caller context was not canceled after the commit")
			{
				err := converge(t, f)
				testassert.NoError(t, err)
			}
			assertForkSourceUnchanged(t, store, source, snapshotSession(t, store, source)) // the source is untouched
			{
				entries := recoverEntries(t, store, dest)
				testassert.Require(t, len(entries) != 0, "committed destination %q has no entries", dest)
			}
		})
	})
}

// tamper replaces an equal-length substring of one raw JSON buffer in place,
// exactly the scratch mutation an uncooperative opener could apply to the
// bytes it receives.
func tamper(raw json.RawMessage, want, with string) {
	if len(want) != len(with) {
		panic("in-place tampering needs an equal-length replacement")
	}
	i := bytes.Index(raw, []byte(want))
	if i < 0 {
		panic("in-place tampering target absent")
	}
	copy(raw[i:], with)
}

// TestPublicOpenerInputMutationKeepsAdmittedExecution proves the captured
// execution authority at the opener handoff: an opener that renames a tool
// and tampers with its received admission's nested Parameters bytes and
// capability names in place still faces the Agent with the admitted
// definitions, and the durable Operation retains them too; the unchanged
// opener is the positive sibling.
func TestPublicOpenerInputMutationKeepsAdmittedExecution(t *testing.T) {
	admittedParams := json.RawMessage(`{"type":"object","source":"admitted"}`)
	admitted := model.ToolDefinition{Name: "echo", Description: "echoes", Parameters: admittedParams}
	for _, mutate := range []bool{true, false} {
		name := "unchanged opener keeps the admitted capture"
		if mutate {
			name = "mutating opener keeps the admitted capture"
		}
		t.Run(name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, store harness.Storage) {
				script := newScriptModel()
				f := newPublicFixture(t, store, script, nil)
				defer f.close()
				var opens atomic.Int64
				settled := make(chan struct{}, 1)
				capture := publicCapture()
				capture.Tools = []model.ToolDefinition{admitted}
				capture.Capabilities = []string{"cap.one", "cap.two"}
				f.prepareHook = func(int, harness.PreparationRequest) (harness.PreparedExecution, error) {
					return harness.PreparedExecution{
						Capture: capture,
						Open: func(_ context.Context, adm harness.OperationAdmission) (harness.Execution, error) {
							opens.Add(1)
							if mutate {
								adm.Execution.Tools[0].Name = "renamed"
								adm.Execution.Tools[0].Description = "renamed"
								tamper(adm.Execution.Tools[0].Parameters, "admitted", "tampered")
								adm.Execution.Capabilities[0] = "dropped"
							}
							return harness.Execution{
								Model:         script.effect,
								CompactModel:  script.effect,
								NormalizeTool: publicNormalize,
								Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
									return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"}}}
								},
								Close: func() error { // runs after the terminal settlement commits
									select {
									case settled <- struct{}{}:
									default:
									}
									return nil
								},
							}, nil
						},
					}, nil
				}
				session := createSession(t, f.h)
				{
					_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
					testassert.NoError(t, err)
				}
				select {
				case <-settled:
				case <-time.After(10 * time.Second):
					t.Fatalf("the execution cleanup never ran after the settlement")
				}
				{
					err := converge(t, f)
					testassert.NoError(t, err)
				}
				if opens.Load() != 1 {
					t.Fatalf("opener calls = %d, want the mutation assertion to run through a real open", opens.Load())
				}
				seen := script.seen()
				testassert.Equal(t, len(seen), 1)
				got := seen[0].Tools
				testassert.Require(t, len(got) == 1 && got[0].Name == "echo" && got[0].Description == "echoes" && string(got[0].Parameters) == string(admittedParams), "advertised tools = %+v, want the admitted definitions, not the opener's local mutations", got)
				rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
				testassert.NoError(t, err)
				testassert.Equal(t, rec.State.Status, harness.OperationSuccess)
				durable := rec.Admission.Execution
				testassert.Require(t, len(durable.Tools) == 1 && durable.Tools[0].Name == "echo" && durable.Tools[0].Description == "echoes" && string(durable.Tools[0].Parameters) == string(admittedParams) && reflect.DeepEqual(durable.Capabilities, []string{"cap.one", "cap.two"}), "durable capture = %+v, want the admitted values", durable)
			})
		})
	}
}

// TestPublicBufferedItemFailure proves the buffer-lifetime row through public
// operations: a failed delivery attempt — failed preparation or a failed
// delivered Operation — is final for the item, and the next buffered message
// proceeds without retention or retry.
func TestPublicBufferedItemFailure(t *testing.T) {
	t.Run("failed preparation drops the item and proceeds", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(
				publicTurn(),                    // op-1 succeeds
				publicFail("next item settled"), // q2 settles through its own terminal
			)
			gate := make(chan struct{})
			script.gate = gate
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			prepared := scriptPrepared(script)
			f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
				if call == 1 { // the first drained queued item's preparation fails
					return harness.PreparedExecution{}, fmt.Errorf("preparation broke")
				}
				return prepared, nil
			}
			session := createSession(t, f.h)

			{
				_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
				testassert.NoError(t, err)
			}
			<-f.prepare      // the first admission prepared
			<-script.arrived // parked: the Session is active
			{
				_, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "q1")
				testassert.NoError(t, err)
			}
			{
				_, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "q2")
				testassert.NoError(t, err)
			}
			script.releaseGate()

			<-f.prepare // the failed item was dropped and the next one admitted
			<-script.arrived
			awaitTerminal(t, f.h, session, "op-3") // the settlement commits before the convergence cancellation
			{
				err := converge(t, f)
				testassert.NoError(t, err)
			}
			{
				_, err := f.h.ReadOperation(context.Background(), session, "op-2")
				testassert.Require(t, err != nil, "failed item's operation exists, want nothing retained")
			}
			rec, err := f.h.ReadOperation(context.Background(), session, "op-3")
			testassert.Require(t, err == nil && rec.State.Status == harness.OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == "next item settled", "next buffered message = %+v err %v, want admitted and settled", rec, err)
		})
	})

	t.Run("failed delivered operation proceeds to the next item", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(
				publicTurn(),                     // op-1 succeeds
				publicFail("queued turn failed"), // q1 fails
				publicFail("next item settled"),  // q2 settles through its own terminal
			)
			gate := make(chan struct{})
			script.gate = gate
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			session := createSession(t, f.h)

			{
				_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
				testassert.NoError(t, err)
			}
			<-f.prepare      // the first admission prepared
			<-script.arrived // parked: the Session is active
			{
				_, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "q1")
				testassert.NoError(t, err)
			}
			{
				_, err := submit(t, f.h, session, "op-3", harness.MessageModeQueued, "q2")
				testassert.NoError(t, err)
			}
			script.releaseGate()

			<-f.prepare // the drain admitted q1 over op-1's terminal
			<-script.arrived
			<-f.prepare // q1's failed terminal let the drain admit the next item
			<-script.arrived
			awaitTerminal(t, f.h, session, "op-3") // the settlement commits before the convergence cancellation
			{
				err := converge(t, f)
				testassert.NoError(t, err)
			}
			failed, err := f.h.ReadOperation(context.Background(), session, "op-2")
			testassert.Require(t, err == nil && failed.State.Status == harness.OperationFailure && failed.State.Terminal != nil && failed.State.Terminal.Detail == "queued turn failed", "failed delivered operation = %+v err %v, want terminal failure", failed, err)
			rec, err := f.h.ReadOperation(context.Background(), session, "op-3")
			testassert.Require(t, err == nil && rec.State.Status == harness.OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == "next item settled", "next buffered message = %+v err %v, want admitted and settled", rec, err)
		})
	})
}

// TestPublicFinalBoundarySerialization proves the coordinator linearizes the
// final model-result boundary against submissions through public operations:
// input submitted inside the model callback while the Operation is still
// current is never stranded — regular input arrives through its own successor
// Operation after the natural-success boundary exit, queued input drains after
// the terminal commit.
func TestPublicFinalBoundarySerialization(t *testing.T) {
	t.Run("regular input at the boundary arrives through its own Operation", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(
				publicTurn(), // the boundary result commits; the completion owner settles it
				publicFail("successor settled"),
			)
			var (
				f       *publicFixture
				session string
				first   = true
			)
			inner := script.effect
			boundary := func(ctx context.Context, req model.Request) (model.Stream, error) {
				if first { // submit at the model-result boundary, before the settlement returns
					first = false
					{
						_, err := submit(t, f.h, session, "op-2", harness.MessageModeRegular, "at-boundary")
						testassert.Check(t, err == nil, "boundary submit: %v", err)
					}
				}
				return inner(ctx, req)
			}
			f = newPublicFixture(t, store, script, boundary)
			defer f.close()
			session = createSession(t, f.h)

			{
				_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
				testassert.NoError(t, err)
			}
			<-script.arrived
			<-script.arrived // the boundary input's own Operation makes the next request
			{
				got := texts(script.seen()[1])
				testassert.Require(t, got[0] == "system" && got[1] == "hello" && got[2] == "done" && got[3] == "at-boundary", "successor projection = %v, want the boundary steering as its own Operation's first request", got)
			}
			awaitTerminal(t, f.h, session, "op-2") // the settlement commits before the convergence cancellation
			{
				err := converge(t, f)
				testassert.NoError(t, err)
			}
			rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
			testassert.Require(t, err == nil && rec.State.Status == harness.OperationSuccess && rec.State.Terminal != nil, "operation = %+v err %v, want the natural-success boundary's quiet success", rec, err)
			successor, err := f.h.ReadOperation(context.Background(), session, "op-2")
			testassert.Require(t, err == nil && successor.State.Status == harness.OperationFailure && successor.State.Terminal != nil && successor.State.Terminal.Detail == "successor settled", "successor operation = %+v err %v, want the boundary input's own Operation at its terminal", successor, err)
			for _, fact := range readHistoryFacts(t, f.h, session) {
				if fact.Kind == harness.EntryInput && fact.Input != nil && len(fact.Input.Content) > 0 && fact.Input.Content[0].Text == "at-boundary" {
					testassert.Equal(t, fact.OperationID, "op-2")
				}
			}
		})
	})

	t.Run("queued input at the boundary drains after the terminal", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			script := newScriptModel(
				publicFail("first settled"),        // model-originated terminal before the drain
				publicFail("boundary turn failed"), // the drained item's own terminal
			)
			var (
				f       *publicFixture
				session string
				first   = true
			)
			inner := script.effect
			boundary := func(ctx context.Context, req model.Request) (model.Stream, error) {
				if first { // defer one queued message at the final model-result boundary
					first = false
					{
						_, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "at-boundary")
						testassert.Check(t, err == nil, "boundary submit: %v", err)
					}
				}
				return inner(ctx, req)
			}
			f = newPublicFixture(t, store, script, boundary)
			defer f.close()
			session = createSession(t, f.h)

			{
				_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
				testassert.NoError(t, err)
			}
			<-script.arrived
			<-f.prepare // the drain admitted the boundary item after the terminal commit
			<-script.arrived
			awaitTerminal(t, f.h, session, "op-2") // the settlement commits before the convergence cancellation
			{
				err := converge(t, f)
				testassert.NoError(t, err)
			}
			firstRec, err := f.h.ReadOperation(context.Background(), session, "op-1")
			testassert.Require(t, err == nil && firstRec.State.Status == harness.OperationFailure && firstRec.State.Terminal != nil && firstRec.State.Terminal.Detail == "first settled", "first operation = %+v err %v, want its terminal before the drain", firstRec, err)
			rec, err := f.h.ReadOperation(context.Background(), session, "op-2")
			testassert.Require(t, err == nil && rec.State.Status == harness.OperationFailure, "drained operation = %+v err %v, want the boundary queued input admitted after the terminal", rec, err)
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
			publicFail("first session settled"), // model-originated terminal: durable regardless of later cancellation
		)
		gate := make(chan struct{})
		parked.gate = gate
		finished := newScriptModel(
			publicFail("second session settled"), // model-originated terminal: durable regardless of later cancellation
		)
		parkedEffect, finishedEffect := parked.effect, finished.effect
		route := func(ctx context.Context, req model.Request) (model.Stream, error) {
			got := texts(req)
			if got[len(got)-1] == "first" {
				return parkedEffect(ctx, req)
			}
			return finishedEffect(ctx, req)
		}
		f := newPublicFixture(t, store, parked, route)
		defer f.close()
		sessionA := createSession(t, f.h)
		sessionB := createSession(t, f.h)

		{
			_, err := submit(t, f.h, sessionA, "op-a", harness.MessageModeRegular, "first")
			testassert.NoError(t, err)
		}
		<-f.prepare      // the first admission prepared
		<-parked.arrived // the first Session is parked at its model boundary

		{
			_, err := submit(t, f.h, sessionB, "op-b", harness.MessageModeRegular, "second")
			testassert.NoError(t, err)
		}
		<-f.prepare                             // the second Session was admitted while the first is parked
		<-finished.arrived                      // and reached its model boundary before the first one resumed
		parked.releaseGate()                    // the first Session settles through its own model-originated terminal
		awaitTerminal(t, f.h, sessionA, "op-a") // both failing attempts' settlements commit
		awaitTerminal(t, f.h, sessionB, "op-b") // before the convergence cancellation can race either
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
		doneRec, err := f.h.ReadOperation(context.Background(), sessionB, "op-b")
		testassert.Require(t, err == nil && doneRec.State.Status == harness.OperationFailure && doneRec.State.Terminal != nil && doneRec.State.Terminal.Detail == "second session settled", "second session = %+v err %v, want it settled while the first was parked", doneRec, err)
		rec, err := f.h.ReadOperation(context.Background(), sessionA, "op-a")
		testassert.Require(t, err == nil && rec.State.Status == harness.OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == "first session settled", "first session = %+v err %v, want its model-originated terminal after its release", rec, err)
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
			publicTurn("call-1", "call-2"),     // the turn publishes two ordered calls
			publicTurn(),                       // the turn completes after the results
			publicFail("drained turn settled"), // the drained item's own terminal
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
			Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
				return harness.Execution{
					Model:         script.effect,
					CompactModel:  script.effect,
					NormalizeTool: publicNormalize,
					Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
						toolMu.Lock()
						toolOrder = append(toolOrder, call.ID)
						toolMu.Unlock()
						if call.ID == "call-1" { // executor-backed: intent before execution, one result after
							return harness.PreparedTool{Permissions: publicPermission, Execute: func(context.Context) harness.ToolOutcome {
								reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: session, Kind: harness.RegisterOperation, OperationID: "op-1"})
								if err != nil {
									t.Errorf("read operation register during execution: %v", err)
									return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call-1"}}
								}
								var wire opRegisterWire
								if err := json.Unmarshal(reg.Payload, &wire); err != nil {
									t.Errorf("decode operation register during execution: %v", err)
									return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call-1"}}
								}
								testassert.Check(t, wire.State.ActiveEffect != nil && wire.State.ActiveEffect.Kind == "tool" && wire.State.ActiveEffect.ToolCallID == "call-1" && wire.State.ActiveEffect.ResultEntryID != "", "tool intent during execution = %+v, want the committed call-1 intent with a reserved result", wire.State.ActiveEffect)
								return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call-1"}}
							}}
						}
						return harness.PreparedTool{Permissions: publicPermission, Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "immediate call-2"}}} // no effect intent
					},
				}, nil
			},
		}
		f.prepareHook = func(int, harness.PreparationRequest) (harness.PreparedExecution, error) { return prepared, nil }

		script.gate = make(chan struct{}) // the first turn parks at its model boundary until released below
		session = createSession(t, f.h)
		res, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
		testassert.Require(t, err == nil && res.Disposition == harness.DispositionAdmitted, "first submit = %+v err %v, want the auto-started admission", res, err)
		<-f.prepare      // the initial admission prepared: the later prepare barrier is the drain's alone
		<-script.arrived // the first model boundary — parked at the gate
		{
			got := texts(script.seen()[0])
			testassert.Require(t, got[0] == "system" && got[1] == "hello" && len(got) == 2, "first projection = %v, want the system prompt and the admitted input", got)
		}
		{
			queued, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1")
			testassert.Require(t, err == nil && queued.Disposition == harness.DispositionQueued, "queued submit = %+v err %v, want queued", queued, err)
		}
		script.releaseGate()

		<-script.arrived // the second request: both committed results project before it
		{
			got := texts(script.seen()[1])
			testassert.Require(t, len(got) == 5 && got[0] == "system" && got[1] == "hello" && got[2] == "done" && got[3] == "ran call-1" && got[4] == "immediate call-2", "subsequent projection = %v, want both tool results in dispatch order", got)
		}
		<-f.prepare // the drain admitted the queued item: op-1's terminal success committed
		<-script.arrived
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}

		rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
		testassert.Require(t, err == nil && rec.State.Status == harness.OperationSuccess && len(rec.State.PendingToolCalls) == 0, "tool turn = %+v err %v, want terminal success with every call settled", rec, err)
		toolMu.Lock()
		defer toolMu.Unlock()
		testassert.Require(t, len(toolOrder) == 2 && toolOrder[0] == "call-1" && toolOrder[1] == "call-2", "dispatch order = %v, want the completed calls in published order", toolOrder)
	})
}

// TestPublicToolMetadataPersistsOpaqueAcrossStores proves the durable metadata
// boundary through the public surface over both storage implementations:
// a tool's metadata commits as its encoded durable bytes — even a raw value
// over the bound whose compacted encoding fits, before and after a
// restarted Harness revalidates the payloads — an executor returning
// malformed metadata still commits its result without the member, a raw
// value within the bound whose durable HTML-escaped encoding exceeds it is
// dropped while its result still commits, the model-visible projection
// carries only each committed result's content, and recovery revalidates
// both payloads.
func TestPublicToolMetadataPersistsOpaqueAcrossStores(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		// the accepted executor returns the formatted fixture prefixed with
		// 1 MiB of ordinary spaces: raw over the bound, but its compacted,
		// HTML-escaped durable encoding is the short literal below it.
		const formatted = `{ "n" : [1, 1.0, 1e0, 9007199254740993], "s" : "<>&" }`
		const encoded = `{"n":[1,1.0,1e0,9007199254740993],"s":"\u003c\u003e\u0026"}`
		rawOverBound := json.RawMessage(strings.Repeat(" ", 1<<20) + formatted)
		// exactly 1 MiB of raw bytes, but every '<' encodes as \u003c in the
		// durable payload, expanding it far past the bound.
		const durableBound = 1 << 20
		expanded := json.RawMessage(`"` + strings.Repeat("<", durableBound-2) + `"`)
		testassert.Equal(t, len(expanded), durableBound)
		script := newScriptModel(
			publicTurn("call-1", "call-2", "call-3"),
			publicTurn(),
		)
		f := newPublicFixture(t, store, script, nil)
		f.prepareHook = func(_ int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: publicCapture(),
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					return harness.Execution{
						Model:         script.effect,
						CompactModel:  script.effect,
						NormalizeTool: publicNormalize,
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							switch call.ID {
							case "call-2": // malformed metadata: dropped, the result still commits
								return harness.PreparedTool{Permissions: publicPermission, Immediate: &harness.ToolOutcome{
									Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "no preview"},
									Metadata: json.RawMessage(`{broken`),
								}}
							case "call-3": // durable-oversized after escaping: dropped, no transaction failure
								return harness.PreparedTool{Permissions: publicPermission, Immediate: &harness.ToolOutcome{
									Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "expanded metadata"},
									Metadata: expanded,
								}}
							}
							return harness.PreparedTool{Permissions: publicPermission, Execute: func(context.Context) harness.ToolOutcome {
								return harness.ToolOutcome{
									Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call-1"},
									Metadata: rawOverBound,
								}
							}}
						},
					}, nil
				},
			}, nil
		}
		session := createSession(t, f.h)
		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived // the turn that published the calls
		<-script.arrived // the completion turn: all three results projected before it
		{
			got := texts(script.seen()[1])
			testassert.Require(t, len(got) == 6 && got[3] == "ran call-1" && got[4] == "no preview" && got[5] == "expanded metadata", "projection = %v, want the committed contents only", got)
		}
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}

		readResults := func() map[string]map[string]json.RawMessage {
			results := map[string]map[string]json.RawMessage{}
			for _, entry := range recoverEntries(t, store, session) {
				if entry.Kind != harness.EntryToolResult {
					continue
				}
				var wire map[string]json.RawMessage
				{
					err := json.Unmarshal(entry.Payload, &wire)
					testassert.NoError(t, err)
				}
				var callID string
				{
					err := json.Unmarshal(wire["tool_call_id"], &callID)
					testassert.NoError(t, err)
				}
				results[callID] = wire
			}
			return results
		}
		results := readResults()
		testassert.Require(t, string(results["call-1"]["metadata"]) == encoded, "call-1 metadata = %s, want the committed encoded bytes verbatim", results["call-1"]["metadata"])
		{
			_, present := results["call-2"]["metadata"]
			testassert.Require(t, !present, "call-2 carries malformed metadata %s, want it dropped while the result committed", results["call-2"]["metadata"])
		}
		testassert.Require(t, string(results["call-2"]["content"]) == `"no preview"`, "call-2 payload = %s, want the settled result alongside the dropped metadata", results["call-2"]["content"])
		{
			_, present := results["call-3"]["metadata"]
			testassert.Require(t, !present, "call-3 carries durable-oversized metadata, want it dropped while the result committed")
		}
		testassert.Require(t, string(results["call-3"]["content"]) == `"expanded metadata"`, "call-3 payload = %s, want the settled result committed past the metadata drop", results["call-3"]["content"])

		// a restarted Harness revalidates every committed payload: the durable
		// states prove no transaction failed and the Session stayed sound,
		// and the committed encoded member survives recovery unchanged.
		{
			err := harness.Recover(context.Background(), store)
			testassert.NoError(t, err)
		}
		results = readResults()
		testassert.Require(t, string(results["call-1"]["metadata"]) == encoded, "call-1 metadata after Recover = %s, want the committed encoded bytes verbatim", results["call-1"]["metadata"])
	})
}

// TestPublicPreparedPermissionBoundarySurvivesRestart proves the prepared
// permission boundary through the public surface on both stores: the
// committed capture
// carries the durable permission capability members; the advertised call's
// original normalization commits at the assistant producer and the allowed
// executor consumes those committed bytes; an immediate-success plan that
// omits its declarations is denied through the public path with the fixed
// bounded content and no metadata; and recovery revalidates the whole result
// with the capture and normalization unchanged.
func TestPublicPreparedPermissionBoundarySurvivesRestart(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		capture := publicCapture()
		capture.Readonly = true
		capture.WriteDir = "/w/sub"
		script := newScriptModel(
			publicTurn("call-1", "call-2"),
			publicTurn(),
		)
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		var consumed json.RawMessage
		var executions int
		f.prepareHook = func(_ int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: capture,
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					return harness.Execution{
						Model:         script.effect,
						CompactModel:  script.effect,
						NormalizeTool: publicNormalize,
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							if call.ID == "call-2" { // the exit-condition omission through the public path
								return harness.PreparedTool{Immediate: &harness.ToolOutcome{
									Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "undeclared success"},
									Metadata: json.RawMessage(`{"would":"leak"}`),
								}}
							}
							return harness.PreparedTool{
								Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "fixture"}},
								Execute: func(context.Context) harness.ToolOutcome {
									executions++
									consumed = call.Arguments
									return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call-1"}}
								},
							}
						},
					}, nil
				},
			}, nil
		}
		session := createSession(t, f.h)
		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived // the turn publishing both calls
		<-script.arrived // the continuation after the settled calls
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
		testassert.Equal(t, executions, 1)
		testassert.Require(t, string(consumed) == `{"x":1}`, "executor saw arguments %s, want the committed normalized bytes", consumed)
		rec, err := f.h.ReadOperation(context.Background(), session, "op-1")
		testassert.NoError(t, err)
		testassert.Require(t, rec.Admission.Execution.Readonly && rec.Admission.Execution.WriteDir == "/w/sub", "committed capture = %v %q, want the durable permission capability members", rec.Admission.Execution.Readonly, rec.Admission.Execution.WriteDir)
		var normalized, denial, settled int
		for _, entry := range recoverEntries(t, store, session) {
			var wire map[string]json.RawMessage
			{
				err := json.Unmarshal(entry.Payload, &wire)
				testassert.Require(t, err == nil, "decode %s payload: %v", entry.ID, err)
			}
			switch entry.Kind {
			case harness.EntryAssistant:
				var calls []struct {
					ID                  string          `json:"id"`
					NormalizedArguments json.RawMessage `json:"normalized_arguments"`
				}
				{
					err := json.Unmarshal(wire["tool_calls"], &calls)
					testassert.NoError(t, err)
				}
				for _, call := range calls {
					testassert.Require(t, string(call.NormalizedArguments) == `{"x":1}`, "%s normalized_arguments = %s, want the producer's committed object", call.ID, call.NormalizedArguments)
					normalized++
				}
			case harness.EntryToolResult:
				settled++
				var callID, status string
				{
					err := json.Unmarshal(wire["tool_call_id"], &callID)
					testassert.NoError(t, err)
				}
				{
					err := json.Unmarshal(wire["status"], &status)
					testassert.NoError(t, err)
				}
				if callID == "call-2" {
					denial++
					testassert.Require(t, status == "denied" && string(wire["content"]) == `"Permission denied."`, "call-2 result = %s, want the fixed public denial", entry.Payload)
					{
						_, present := wire["metadata"]
						testassert.Require(t, !present, "denied call-2 carries a metadata member")
					}
				} else if status != "success" {
					t.Fatalf("call-1 result = %s, want the allowed execution", entry.Payload)
				}
			}
		}
		testassert.Require(t, normalized == 2 && denial == 1 && settled == 2, "committed shape = %d normalized calls, %d denials, %d results, want 2/1/2", normalized, denial, settled)
		{
			err := harness.Recover(context.Background(), store)
			testassert.NoError(t, err)
		}
		after, err := f.h.ReadOperation(context.Background(), session, "op-1")
		testassert.Require(t, err == nil && after.Admission.Execution.WriteDir == "/w/sub" && after.Admission.Execution.Readonly, "capture after recovery = %+v err %v, want the same durable constraints", after.Admission.Execution, err)
	})
}

// sessionRegister returns the raw durable Session register of one Session.
func sessionRegister(t *testing.T, store harness.Storage, sessionID string) harness.Register {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	testassert.NoError(t, err)
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
			first, err := f.h.ReadSessionHeader(ctx, session)
			testassert.Require(t, err == nil && first.Lifecycle == harness.LifecycleOpen && first.Revision.DurableRevision == 1, "created session = %+v err %v, want open at revision 1", first, err)
			originalActivity := first.LastActivity

			reopened, err := f.h.ReopenSession(ctx, session)
			testassert.Require(t, err == nil && reopened.State.Lifecycle == harness.LifecycleOpen && reopened.State.ArchivedAt == nil && reopened.Revision == first.Revision.DurableRevision && reopened.State.LastActivity.Equal(originalActivity), "reopen of open = %+v err %v, want the no-write success", reopened, err)

			archived, err := f.h.ArchiveSession(ctx, session)
			testassert.Require(t, err == nil && archived.State.Lifecycle == harness.LifecycleArchived && archived.Revision == first.Revision.DurableRevision+1, "archive of open = %+v err %v, want archived at the next revision", archived, err)
			{
				reg := sessionRegister(t, store, session)
				testassert.Equal(t, reg.Revision, archived.Revision)
			}
			testassert.Require(t, archived.State.ArchivedAt != nil && !archived.State.ArchivedAt.IsZero() && archived.State.ArchivedAt.Location() == time.UTC, "archived_at = %v, want one sampled UTC time", archived.State.ArchivedAt)
			testassert.Require(t, archived.State.LastActivity.Equal(originalActivity), "archive moved last activity to %v, want it unchanged at %v", archived.State.LastActivity, originalActivity)

			again, err := f.h.ArchiveSession(ctx, session)
			testassert.Require(t, err == nil && again.Revision == archived.Revision && again.State.ArchivedAt.Equal(*archived.State.ArchivedAt), "archive of archived = %+v err %v, want the no-write success", again, err)

			back, err := f.h.ReopenSession(ctx, session)
			testassert.Require(t, err == nil && back.State.Lifecycle == harness.LifecycleOpen && back.State.ArchivedAt == nil && back.Revision == archived.Revision+1, "reopen of archived = %+v err %v, want open with the archived time cleared", back, err)
			{
				reg := sessionRegister(t, store, session)
				testassert.Equal(t, reg.Revision, back.Revision)
			}
			testassert.Require(t, back.State.LastActivity.After(originalActivity), "reopen left last activity at %v, want it advanced past %v", back.State.LastActivity, originalActivity)
			testassert.Require(t, back.Identity == first.Identity && back.State.CurrentAgentType == "coder", "reopen changed identity or Agent type: %+v", back)

			againOpen, err := f.h.ReopenSession(ctx, session)
			testassert.Require(t, err == nil && againOpen.Revision == back.Revision && againOpen.State.LastActivity.Equal(back.State.LastActivity), "second reopen = %+v err %v, want the no-write success", againOpen, err)
		})

		t.Run("archive rejects a running session without losing its buffers", func(t *testing.T) {
			script := newScriptModel()
			script.gate = make(chan struct{})
			f := newPublicFixture(t, store, script, nil)
			defer f.close()
			session := createSession(t, f.h)
			{
				_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
				testassert.NoError(t, err)
			}
			<-f.prepare      // the first admission prepared
			<-script.arrived // parked at the model boundary: the Session is running
			{
				_, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1")
				testassert.NoError(t, err)
			}
			before := sessionRegister(t, store, session)

			{
				_, err := f.h.ArchiveSession(ctx, session)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "archive of a running session = err %v, want ErrInvalid", err)
			}
			after := sessionRegister(t, store, session)
			testassert.Require(t, after.Revision == before.Revision && bytes.Equal(after.Payload, before.Payload), "rejected archive changed the durable register (revision %d -> %d)", before.Revision, after.Revision)

			script.releaseGate()
			<-f.prepare      // the rejected archive kept the buffered item: the drain admitted it
			<-script.arrived // the admitted item reached its model boundary: its admission committed
			{
				err := converge(t, f)
				testassert.NoError(t, err)
			}
			drained, err := f.h.ReadOperation(ctx, session, "op-2")
			testassert.Require(t, err == nil && drained.State.Terminal != nil, "buffered item after rejected archive = %+v err %v, want it delivered and settled", drained, err)
		})

		t.Run("delete requires archived and invalidates the coordinator", func(t *testing.T) {
			f := newPublicFixture(t, store, newScriptModel(), nil)
			defer f.close()
			session := createSession(t, f.h)
			before := sessionRegister(t, store, session)

			{
				err := f.h.DeleteSession(ctx, session)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "delete of an open session = err %v, want ErrInvalid", err)
			}
			after := sessionRegister(t, store, session)
			testassert.Require(t, after.Revision == before.Revision && bytes.Equal(after.Payload, before.Payload), "rejected delete changed the durable register (revision %d -> %d)", before.Revision, after.Revision)

			{
				_, err := f.h.ArchiveSession(ctx, session)
				testassert.NoError(t, err)
			}
			{
				err := f.h.DeleteSession(ctx, session)
				testassert.NoError(t, err)
			}

			key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession}
			{
				_, err := store.ReadRegister(ctx, key)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "deleted session register = err %v, want ErrNotFound", err)
			}
			ids, err := store.ListSessionIDs(ctx)
			testassert.NoError(t, err)
			for _, id := range ids { // deletion is absence: no tombstone, no listing
				testassert.Require(t, id != session, "deleted session still listed")
			}

			// holders of the materialized coordinator and later materialization both get ErrNotFound
			{
				_, err := f.h.ReadSessionHeader(ctx, session)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "read after delete = err %v, want ErrNotFound, not a stale record", err)
			}
			{
				_, err := f.h.ReadOperation(ctx, session, "op-1")
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "operation read after delete = err %v, want ErrNotFound", err)
			}
			{
				_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "late")
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "submit after delete = err %v, want ErrNotFound", err)
			}
			{
				_, err := f.h.ChangeAgentType(ctx, session, "reviewer")
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "agent-type change after delete = err %v, want ErrNotFound", err)
			}
			{
				_, err := f.h.ReopenSession(ctx, session)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "reopen after delete = err %v, want ErrNotFound", err)
			}
			{
				_, err := f.h.ArchiveSession(ctx, session)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "archive after delete = err %v, want ErrNotFound", err)
			}
			{
				err := f.h.DeleteSession(ctx, session)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "second delete = err %v, want ErrNotFound", err)
			}
		})

		t.Run("unknown and malformed identities", func(t *testing.T) {
			f := newPublicFixture(t, store, newScriptModel(), nil)
			defer f.close()
			unknown := "0123456789abcdef0123456789abcdef" // valid hex identity, absent Session
			{
				_, err := f.h.ReopenSession(ctx, unknown)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "reopen of unknown = err %v, want ErrNotFound", err)
			}
			{
				_, err := f.h.ArchiveSession(ctx, unknown)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "archive of unknown = err %v, want ErrNotFound", err)
			}
			{
				err := f.h.DeleteSession(ctx, unknown)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "delete of unknown = err %v, want ErrNotFound", err)
			}
			malformed := "not-a-session-id"
			{
				_, err := f.h.ReopenSession(ctx, malformed)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "reopen of malformed = err %v, want ErrInvalid", err)
			}
			{
				_, err := f.h.ArchiveSession(ctx, malformed)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "archive of malformed = err %v, want ErrInvalid", err)
			}
			{
				err := f.h.DeleteSession(ctx, malformed)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "delete of malformed = err %v, want ErrInvalid", err)
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
		first, err := f.h.ReadSessionHeader(ctx, session)
		testassert.NoError(t, err)
		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}

		boundary := first.LastActivity.Add(policy.ArchiveAfter)
		before := sessionRegister(t, store, session)
		{
			_, err := f.h.SweepSession(ctx, session, policy, boundary)
			testassert.NoError(t, err)
		}
		still, err := f.h.ReadSessionHeader(ctx, session)
		testassert.Require(t, err == nil && still.Lifecycle == harness.LifecycleOpen && still.Revision == first.Revision, "boundary sweep = %+v err %v, want the open Session unchanged", still, err)
		after := sessionRegister(t, store, session)
		testassert.Require(t, after.Revision == before.Revision && bytes.Equal(after.Payload, before.Payload), "no-op boundary sweep changed the durable register (revision %d -> %d)", before.Revision, after.Revision)

		past := boundary.Add(time.Nanosecond)
		{
			_, err := f.h.SweepSession(ctx, session, policy, past)
			testassert.NoError(t, err)
		}
		archived, err := f.h.ReadSessionHeader(ctx, session)
		testassert.Require(t, err == nil && archived.Lifecycle == harness.LifecycleArchived && archived.Revision.DurableRevision == first.Revision.DurableRevision+1, "past-boundary sweep = %+v err %v, want archived at the next revision", archived, err)
		{
			reg := sessionRegister(t, store, session)
			testassert.Equal(t, reg.Revision, archived.Revision.DurableRevision)
		}
		testassert.Require(t, archived.ArchivedAt != nil && archived.ArchivedAt.Equal(past), "sweep archive stamped %v, want the explicit sweep time %v", archived.ArchivedAt, past)
		testassert.Require(t, archived.LastActivity.Equal(first.LastActivity), "sweep archive moved last activity to %v, want it unchanged", archived.LastActivity)
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
		testassert.Require(t, err == nil && archived.State.ArchivedAt != nil, "archive = %+v err %v", archived, err)
		policy := harness.SweepPolicy{DeleteAfterArchive: 12 * time.Hour}

		boundary := archived.State.ArchivedAt.Add(policy.DeleteAfterArchive)
		before := sessionRegister(t, store, session)
		{
			_, err := f.h.SweepSession(ctx, session, policy, boundary)
			testassert.NoError(t, err)
		}
		{
			_, err := f.h.ReadSessionHeader(ctx, session)
			testassert.NoError(t, err)
		}
		after := sessionRegister(t, store, session)
		testassert.Require(t, after.Revision == before.Revision && bytes.Equal(after.Payload, before.Payload), "no-op delete-boundary sweep changed the durable register (revision %d -> %d)", before.Revision, after.Revision)

		{
			_, err := f.h.SweepSession(ctx, session, policy, boundary.Add(time.Nanosecond))
			testassert.NoError(t, err)
		}
		key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession}
		{
			_, err := store.ReadRegister(ctx, key)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "sweep-deleted session register = err %v, want ErrNotFound", err)
		}
		ids, err := store.ListSessionIDs(ctx)
		testassert.NoError(t, err)
		for _, id := range ids {
			testassert.Require(t, id != session, "sweep-deleted session still listed")
		}
		{
			_, err := f.h.ReadSessionHeader(ctx, session)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "read after sweep delete = err %v, want ErrNotFound, not a stale record", err)
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
			{
				_, err := f.h.ArchiveSession(ctx, archivedID)
				testassert.NoError(t, err)
			}
			beforeOpen, err := f.h.ReadSessionHeader(ctx, open)
			testassert.NoError(t, err)
			beforeArchived, err := f.h.ReadSessionHeader(ctx, archivedID)
			testassert.NoError(t, err)
			{
				_, err := f.h.SweepSession(ctx, open, harness.SweepPolicy{}, farPast(beforeOpen.LastActivity))
				testassert.NoError(t, err)
			}
			{
				_, err := f.h.SweepSession(ctx, archivedID, harness.SweepPolicy{}, farPast(beforeOpen.LastActivity))
				testassert.NoError(t, err)
			}
			afterOpen, err := f.h.ReadSessionHeader(ctx, open)
			testassert.Require(t, err == nil && afterOpen.Revision == beforeOpen.Revision && afterOpen.Lifecycle == harness.LifecycleOpen, "disabled sweep touched the open session: %+v err %v", afterOpen, err)
			afterArchived, err := f.h.ReadSessionHeader(ctx, archivedID)
			testassert.Require(t, err == nil && afterArchived.Revision == beforeArchived.Revision && afterArchived.Lifecycle == harness.LifecycleArchived, "disabled sweep touched the archived session: %+v err %v", afterArchived, err)
		})

		t.Run("delete disabled archives but does not delete", func(t *testing.T) {
			f := newPublicFixture(t, store, newScriptModel(), nil)
			defer f.close()
			open := createSession(t, f.h)
			archivedID := createSession(t, f.h)
			{
				_, err := f.h.ArchiveSession(ctx, archivedID)
				testassert.NoError(t, err)
			}
			beforeOpen, err := f.h.ReadSessionHeader(ctx, open)
			testassert.NoError(t, err)
			policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour}
			{
				_, err := f.h.SweepSession(ctx, open, policy, farPast(beforeOpen.LastActivity))
				testassert.NoError(t, err)
			}
			{
				_, err := f.h.SweepSession(ctx, archivedID, policy, farPast(beforeOpen.LastActivity))
				testassert.NoError(t, err)
			}
			sweptOpen, err := f.h.ReadSessionHeader(ctx, open)
			testassert.Require(t, err == nil && sweptOpen.Lifecycle == harness.LifecycleArchived, "enabled archive transition = %+v err %v, want archived", sweptOpen, err)
			{
				_, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: archivedID, Kind: harness.RegisterSession})
				testassert.NoError(t, err)
			}
			{
				_, err := f.h.ReadSessionHeader(ctx, archivedID)
				testassert.NoError(t, err)
			}
		})

		t.Run("archive disabled deletes but does not archive", func(t *testing.T) {
			f := newPublicFixture(t, store, newScriptModel(), nil)
			defer f.close()
			open := createSession(t, f.h)
			archivedID := createSession(t, f.h)
			{
				_, err := f.h.ArchiveSession(ctx, archivedID)
				testassert.NoError(t, err)
			}
			beforeOpen, err := f.h.ReadSessionHeader(ctx, open)
			testassert.NoError(t, err)
			policy := harness.SweepPolicy{DeleteAfterArchive: 12 * time.Hour}
			now := beforeOpen.LastActivity.Add(100 * time.Hour)
			{
				_, err := f.h.SweepSession(ctx, open, policy, now)
				testassert.NoError(t, err)
			}
			{
				_, err := f.h.SweepSession(ctx, archivedID, policy, now)
				testassert.NoError(t, err)
			}
			stillOpen, err := f.h.ReadSessionHeader(ctx, open)
			testassert.Require(t, err == nil && stillOpen.Lifecycle == harness.LifecycleOpen, "disabled archive transition = %+v err %v, want the session still open", stillOpen, err)
			key := harness.RegisterKey{SessionID: archivedID, Kind: harness.RegisterSession}
			{
				_, err := store.ReadRegister(ctx, key)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "enabled delete transition left the archived session: err %v", err)
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
		first, err := f.h.ReadSessionHeader(ctx, session)
		testassert.NoError(t, err)

		{
			_, err := submit(t, f.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-f.prepare      // the first admission prepared
		<-script.arrived // parked at the model boundary: the Session is running
		{
			_, err := submit(t, f.h, session, "op-2", harness.MessageModeQueued, "queued-1")
			testassert.NoError(t, err)
		}
		active, err := f.h.ReadSessionHeader(ctx, session) // the admission's own write is the sweep's baseline
		testassert.NoError(t, err)
		before := sessionRegister(t, store, session)

		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		{
			_, err := f.h.SweepSession(ctx, session, policy, first.LastActivity.Add(100*time.Hour))
			testassert.NoError(t, err)
		}
		still, err := f.h.ReadSessionHeader(ctx, session)
		testassert.Require(t, err == nil && still.Lifecycle == harness.LifecycleOpen && still.Revision == active.Revision, "sweep touched the running session: %+v err %v, want it unchanged", still, err)
		after := sessionRegister(t, store, session)
		testassert.Require(t, after.Revision == before.Revision && bytes.Equal(after.Payload, before.Payload), "sweep of a running session changed the durable register (revision %d -> %d)", before.Revision, after.Revision)

		script.releaseGate()
		<-f.prepare      // the buffered item was not lost by the sweep: the drain admitted it
		<-script.arrived // the admitted item reached its model boundary: its admission committed
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
		drained, err := f.h.ReadOperation(ctx, session, "op-2")
		testassert.Require(t, err == nil && drained.State.Terminal != nil, "buffered item after sweep = %+v err %v, want it delivered and settled", drained, err)
	})
}

// TestPublicSweepCorruptSibling proves the corruption axis of the lifecycle
// row through public operations: a Session whose register is corrupt in
// storage is left unchanged by its sweep transition, which reports the
// corruption class, while the valid sibling is still swept and the corrupt
// Session becomes unavailable in the Harness instance while staying present
// in storage.
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
		{
			err := json.Unmarshal(reg.Payload, &obj)
			testassert.NoError(t, err)
		}
		var state map[string]json.RawMessage
		{
			err := json.Unmarshal(obj["state"], &state)
			testassert.NoError(t, err)
		}
		state["lifecycle"] = json.RawMessage(`"bogus"`)
		newState, err := json.Marshal(state)
		testassert.NoError(t, err)
		obj["state"] = newState
		bad, err := json.Marshal(obj)
		testassert.NoError(t, err)
		key := harness.RegisterKey{SessionID: corrupt, Kind: harness.RegisterSession}
		{
			err := store.Transact(ctx, func(tx harness.Transaction) error {
				_, err := tx.ReplaceRegister(key, reg.Revision, bad)
				return err
			})
			testassert.NoError(t, err)
		}

		validBefore, err := f.h.ReadSessionHeader(ctx, valid)
		testassert.NoError(t, err)
		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		when := validBefore.LastActivity.Add(100 * time.Hour)
		{
			_, err := f.h.SweepSession(ctx, corrupt, policy, when)
			testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "sweep of the corrupt session = %v, want the corruption error and the row left in place", err)
		}
		{
			_, err := f.h.SweepSession(ctx, valid, policy, when)
			testassert.NoError(t, err)
		}

		untouched := sessionRegister(t, store, corrupt) // the corruption write itself took revision reg.Revision+1
		testassert.Require(t, bytes.Equal(untouched.Payload, bad) && untouched.Revision == reg.Revision+1, "sweep changed the corrupt register (revision %d -> %d)", reg.Revision+1, untouched.Revision)
		swept, err := f.h.ReadSessionHeader(ctx, valid)
		testassert.Require(t, err == nil && swept.Lifecycle == harness.LifecycleArchived, "valid sibling after sweep = %+v err %v, want it archived", swept, err)
		{
			_, err := f.h.ReadSessionHeader(ctx, corrupt)
			testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "corrupt session after sweep = err %v, want the corruption error", err)
		}
	})
}

// readCountingStore wraps one store and counts ReadRegisters calls: the
// cold materialization read a sweep transition reaches through its
// coordinator materialization.
type readCountingStore struct {
	harness.Storage
	reads int
}

func (s *readCountingStore) ReadRegisters(ctx context.Context, sessionID string) ([]harness.Register, error) {
	s.reads++
	return s.Storage.ReadRegisters(ctx, sessionID)
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
	{
		_, err := rand.Read(buf[:])
		testassert.NoError(t, err)
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
	testassert.NoError(t, err)
	return id
}

// TestPublicSweepZeroTime proves the sweep transition's explicit-time
// precondition at its actual boundary: a zero time is rejected with ErrInvalid
// before the coordinator materialization reads storage, and the rejected
// transition performs none.
func TestPublicSweepZeroTime(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		session := seedArchivedSession(t, store) // seeded straight through storage: never materialized
		counting := &readCountingStore{Storage: store}
		f := newPublicFixture(t, counting, newScriptModel(), nil)
		defer f.close()

		policy := harness.SweepPolicy{ArchiveAfter: 24 * time.Hour, DeleteAfterArchive: 12 * time.Hour}
		{
			deleted, err := f.h.SweepSession(ctx, session, policy, time.Time{})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid) && !deleted, "sweep with a zero time = (%v, %v), want ErrInvalid and no transition", deleted, err)
		}
		testassert.Equal(t, counting.reads, 0)

		// An ordinary valid read reaches the same counter through the cold
		// fixture, so the zero count above cannot pass vacuously, and shows
		// the seeded Session untouched by the rejection.
		header, err := f.h.ReadSessionHeader(ctx, session)
		testassert.Require(t, err == nil && header.Identity.SessionID == session && header.Lifecycle == harness.LifecycleArchived, "read after the rejected zero-time sweep = %+v err %v, want the seeded archived Session present and unchanged", header, err)
		testassert.Require(t, counting.reads != 0, "the cold fixture never reached the read counter: the zero-read assertion cannot pass vacuously")
	})
}

// TestPublicSweepSkipsBlockedPreparation proves the sweep's nonwaiting busy
// rule: a sweep transition that reaches a Session with an in-flight
// preparation (reservation held) applies the same no-transition outcome as
// running or buffered work and returns without waiting — the Session stays
// unchanged while the preparation is still parked, and the released admission commits
// and settles normally.
func TestPublicSweepSkipsBlockedPreparation(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		gate := make(chan struct{})
		script.gate = gate
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		ctx := context.Background()
		session := createSession(t, f.h) // the only Session: the sweep reaches the reserved candidate
		first, err := f.h.ReadSessionHeader(ctx, session)
		testassert.NoError(t, err)

		prepStarted := make(chan struct{}, 1)
		releasePrep := make(chan struct{})
		prepared := scriptPrepared(script)
		f.prepareHook = func(call int, req harness.PreparationRequest) (harness.PreparedExecution, error) {
			if call == 0 { // the first admission's preparation is blocked while the sweep passes
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
		go func() {
			_, err := f.h.SweepSession(ctx, session, policy, first.LastActivity.Add(100*time.Hour))
			sweepDone <- err
		}()
		select {
		case err := <-sweepDone:
			testassert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the sweep waited for the blocked preparation: busy candidates skip without waiting")
		}
		// The skip happened before the preparation released: nothing
		// transitioned while the reservation was still held.
		still, err := f.h.ReadSessionHeader(ctx, session)
		testassert.Require(t, err == nil && still.Lifecycle == harness.LifecycleOpen && still.CurrentOperationID == "", "sweep transitioned the Session under a blocked preparation: %+v err %v", still, err)

		close(releasePrep) // the preparation completes: the admission commits and the Session becomes running
		out := <-submitDone
		testassert.Require(t, out.err == nil && out.res.Disposition == harness.DispositionAdmitted, "first submit = %+v err %v, want admitted", out.res, out.err)
		settled, err := f.h.ReadSessionHeader(ctx, session)
		testassert.Require(t, err == nil && settled.Lifecycle == harness.LifecycleOpen && settled.CurrentOperationID == "op-1", "post-commit header = %+v err %v, want the running Session unchanged", settled, err)
		{
			_, err := f.h.ReadOperation(ctx, session, "op-1")
			testassert.NoError(t, err)
		}

		script.releaseGate()
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
	})
}

// TestPublicDeleteVsMaterialization proves the delete-vs-load row under the
// stable-cell protocol: a read parked inside its Session's first load holds
// that Session's cell, so a concurrent canonical deletion waits for the load
// and never observes a partially published graph; the parked read resolves
// either to its pre-deletion capture or to the not-found class, the deletion
// then commits, and no later read, listing, or registry lookup resurrects the
// deleted Session.
func TestPublicDeleteVsMaterialization(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		session := seedArchivedSession(t, store) // seeded before the live Harness is constructed
		gated := &gatedEntryStore{Storage: store, session: session, started: make(chan struct{}, 1), release: make(chan struct{})}
		f := newPublicFixture(t, gated, newScriptModel(), nil)
		defer f.close()
		ctx := context.Background()

		readDone := make(chan error, 1)
		go func() { _, err := f.h.ReadSessionHeader(ctx, session); readDone <- err }() // the first load
		<-gated.started                                                                // parked inside the load, holding the Session's cell

		deleteDone := make(chan error, 1)
		go func() { deleteDone <- f.h.DeleteSession(ctx, session) }() // the canonical deletion of the same Session
		select {
		case err := <-deleteDone:
			t.Fatalf("delete returned (%v) while the load still held the Session's cell", err)
		case <-time.After(200 * time.Millisecond):
		}

		close(gated.release) // validation now completes on the captured pre-deletion value
		{
			err := <-readDone
			testassert.Require(t, err == nil || errors.Is(err, harness.ErrNotFound), "load across a deletion = err %v, want the pre-deletion capture or ErrNotFound", err)
		}
		{
			err := <-deleteDone
			testassert.NoError(t, err)
		}
		{
			_, err := f.h.ReadSessionHeader(ctx, session)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "later materialization installed a stale coordinator: err %v, want ErrNotFound", err)
		}
		listed, err := f.h.ListSessions(ctx)
		testassert.NoError(t, err)
		for _, row := range listed {
			testassert.Require(t, row.Identity.SessionID != session, "the deleted Session stayed listed: %+v", row)
		}
	})
}

// errInjectedRollback is the sentinel the rollback probe returns from the
// transaction callback after performing its real mutation.
var errInjectedRollback = errors.New("harness_test: injected post-mutation transaction failure")

// rollbackProbeStore wraps one store to inject one post-mutation transaction
// failure: while armed, the next ReplaceRegister (or DeleteSession, or the
// counted InsertRegister) call inside a transaction performs its real
// mutation and then returns the sentinel, so the surrounding transaction
// rolls back after mutating. An empty target arms the probe against every
// identity; a set target arms it only against that Session's deletion.
type rollbackProbeStore struct {
	harness.Storage
	failReplace      bool
	failDelete       bool
	failInsert       bool
	insertsUntilFail int
	target           string
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
	if err == nil && t.probe.failDelete && (t.probe.target == "" || sessionID == t.probe.target) {
		t.probe.failDelete = false
		return errInjectedRollback
	}
	return err
}

func (t *rollbackProbeTransaction) InsertRegister(draft harness.RegisterDraft) (harness.Register, error) {
	reg, err := t.Transaction.InsertRegister(draft)
	if err == nil && t.probe.failInsert { // the counted insert performs its real mutation, then fails
		t.probe.insertsUntilFail--
		if t.probe.insertsUntilFail <= 0 {
			t.probe.failInsert = false
			return harness.Register{}, errInjectedRollback
		}
	}
	return reg, err
}

// assertRollback verifies that a failed lifecycle transaction left the durable
// register byte-identical at the same revision and the coordinator's cached
// view still returns the pre-attempt record.
func assertRollback(t *testing.T, store harness.Storage, h *harness.Harness, session string, before harness.Register, want harness.SessionHeader) {
	t.Helper()
	after := sessionRegister(t, store, session)
	testassert.Require(t, after.Revision == before.Revision && bytes.Equal(after.Payload, before.Payload), "failed transition changed the durable register (revision %d -> %d)", before.Revision, after.Revision)
	got, err := h.ReadSessionHeader(context.Background(), session)
	testassert.NoError(t, err)
	testassert.Require(t, got.Revision == want.Revision && got.Lifecycle == want.Lifecycle && got.LastActivity.Equal(want.LastActivity) && got.CurrentAgentType == want.CurrentAgentType && ((got.ArchivedAt == nil) == (want.ArchivedAt == nil) && (got.ArchivedAt == nil || got.ArchivedAt.Equal(*want.ArchivedAt))), "cached view after failed transition = %+v, want the pre-attempt record; cached view archived_at after failed transition = %v, want %v", got, got.ArchivedAt, want.ArchivedAt)
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
			testassert.NoError(t, err)
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
	{
		_, err := f.h.ArchiveSession(ctx, session)
		testassert.NoError(t, err)
	}
	archived, err := f.h.ReadSessionHeader(ctx, session)
	testassert.NoError(t, err)
	before := sessionRegister(t, store, session)

	probe.failReplace = true
	{
		_, err := f.h.ReopenSession(ctx, session)
		testassert.Require(t, errors.Is(err, errInjectedRollback), "reopen with injected post-mutation failure = err %v, want the injected rollback", err)
	}
	assertRollback(t, store, f.h, session, before, archived)

	reopened, err := f.h.ReopenSession(ctx, session)
	testassert.Require(t, err == nil && reopened.State.Lifecycle == harness.LifecycleOpen && reopened.State.ArchivedAt == nil && reopened.Revision == before.Revision+1, "reopen after rollback = %+v err %v, want open at the next revision", reopened, err)
}

// rollbackArchiveCase proves Archive's post-mutation rollback on one isolated store.
func rollbackArchiveCase(t *testing.T, store harness.Storage) {
	probe := &rollbackProbeStore{Storage: store}
	f := newPublicFixture(t, probe, newScriptModel(), nil)
	defer f.close()
	ctx := context.Background()
	session := createSession(t, f.h)
	first, err := f.h.ReadSessionHeader(ctx, session)
	testassert.NoError(t, err)
	before := sessionRegister(t, store, session)

	probe.failReplace = true
	{
		_, err := f.h.ArchiveSession(ctx, session)
		testassert.Require(t, errors.Is(err, errInjectedRollback), "archive with injected post-mutation failure = err %v, want the injected rollback", err)
	}
	assertRollback(t, store, f.h, session, before, first)

	archived, err := f.h.ArchiveSession(ctx, session)
	testassert.Require(t, err == nil && archived.State.Lifecycle == harness.LifecycleArchived && archived.State.ArchivedAt != nil && archived.Revision == before.Revision+1, "archive after rollback = %+v err %v, want archived at the next revision", archived, err)
}

// rollbackDeleteCase proves Delete's post-mutation rollback on one isolated store.
func rollbackDeleteCase(t *testing.T, store harness.Storage) {
	probe := &rollbackProbeStore{Storage: store}
	f := newPublicFixture(t, probe, newScriptModel(), nil)
	defer f.close()
	ctx := context.Background()
	session := createSession(t, f.h)
	{
		_, err := f.h.ArchiveSession(ctx, session)
		testassert.NoError(t, err)
	}
	archived, err := f.h.ReadSessionHeader(ctx, session)
	testassert.NoError(t, err)
	before := sessionRegister(t, store, session)

	probe.failDelete = true
	{
		err := f.h.DeleteSession(ctx, session)
		testassert.Require(t, errors.Is(err, errInjectedRollback), "delete with injected post-mutation failure = err %v, want the injected rollback", err)
	}
	assertRollback(t, store, f.h, session, before, archived)
	ids, err := store.ListSessionIDs(ctx)
	testassert.NoError(t, err)
	found := false
	for _, id := range ids { // the rolled-back deletion is absence-free: the Session is still listed
		if id == session {
			found = true
		}
	}
	testassert.Require(t, found, "rolled-back delete removed the session from the listing")

	{
		err := f.h.DeleteSession(ctx, session)
		testassert.NoError(t, err)
	}
	key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession}
	{
		_, err := store.ReadRegister(ctx, key)
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "register after delete = err %v, want ErrNotFound", err)
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
	first, err := f.h.ReadSessionHeader(ctx, session)
	testassert.NoError(t, err)
	before := sessionRegister(t, store, session)

	policy := harness.SweepPolicy{ArchiveAfter: time.Hour}
	now := first.LastActivity.Add(100 * time.Hour)
	probe.failReplace = true
	{
		_, err := f.h.SweepSession(ctx, session, policy, now)
		testassert.Require(t, errors.Is(err, errInjectedRollback), "sweep with injected post-mutation failure = err %v, want the transition to return it", err)
	}
	assertRollback(t, store, f.h, session, before, first)

	{
		_, err := f.h.SweepSession(ctx, session, policy, now)
		testassert.NoError(t, err)
	}
	archived, err := f.h.ReadSessionHeader(ctx, session)
	testassert.Require(t, err == nil && archived.Lifecycle == harness.LifecycleArchived && archived.Revision.DurableRevision == before.Revision+1 && archived.ArchivedAt != nil && archived.ArchivedAt.Equal(now), "sweep after rollback = %+v err %v, want archived at the sweep time", archived, err)
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
	testassert.Require(t, err == nil && archived.State.ArchivedAt != nil, "seed archive = %+v err %v", archived, err)
	before := sessionRegister(t, store, session)

	policy := harness.SweepPolicy{DeleteAfterArchive: time.Hour}
	now := archived.State.ArchivedAt.Add(100 * time.Hour)
	archivedHeader, err := f.h.ReadSessionHeader(ctx, session)
	testassert.NoError(t, err)
	probe.failDelete = true
	{
		_, err := f.h.SweepSession(ctx, session, policy, now)
		testassert.Require(t, errors.Is(err, errInjectedRollback), "sweep with injected post-mutation failure = err %v, want the transition to return it", err)
	}
	assertRollback(t, store, f.h, session, before, archivedHeader)

	{
		_, err := f.h.SweepSession(ctx, session, policy, now)
		testassert.NoError(t, err)
	}
	key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession}
	{
		_, err := store.ReadRegister(ctx, key)
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "register after swept delete = err %v, want ErrNotFound", err)
	}
}

// TestPublicWaitConvergesWithCorruptingTransition proves the Wait and
// corruption outcome row: with the register corrupted in storage after
// materialization, ArchiveSession fails with the corruption error while a
// concurrent Wait over the canceled Harness converges, and the Session stays
// unavailable. This is outcome coverage only — it does not deterministically
// reproduce any particular lock schedule between Wait's scans and the
// corrupting transition; the registry-mutex release is covered
// deterministically by the in-package snapshot postcondition test.
func TestPublicWaitConvergesWithCorruptingTransition(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		f := newPublicFixture(t, store, newScriptModel(), nil)
		session := createSession(t, f.h)
		ctx := context.Background()

		// corrupt the register directly through storage after materialization:
		// the transition discovers it in-transaction under c.mu
		reg := sessionRegister(t, store, session)
		var obj map[string]json.RawMessage
		{
			err := json.Unmarshal(reg.Payload, &obj)
			testassert.NoError(t, err)
		}
		var state map[string]json.RawMessage
		{
			err := json.Unmarshal(obj["state"], &state)
			testassert.NoError(t, err)
		}
		state["lifecycle"] = json.RawMessage(`"bogus"`)
		newState, err := json.Marshal(state)
		testassert.NoError(t, err)
		obj["state"] = newState
		bad, err := json.Marshal(obj)
		testassert.NoError(t, err)
		key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterSession}
		{
			err := store.Transact(ctx, func(tx harness.Transaction) error {
				_, err := tx.ReplaceRegister(key, reg.Revision, bad)
				return err
			})
			testassert.NoError(t, err)
		}

		f.cancel() // Harness loss before both calls
		waited := make(chan error, 1)
		go func() { waited <- f.h.Wait(ctx) }() // Wait begins while the transition is in flight

		{
			_, err := f.h.ArchiveSession(ctx, session)
			testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "archive over a corrupted register = err %v, want the corruption error", err)
		}
		{
			err := <-waited
			testassert.NoError(t, err)
		}
		{
			_, err := f.h.ReadSessionHeader(ctx, session)
			testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "corrupt session after convergence = err %v, want the sticky corruption error", err)
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
		HookID        string `json:"hook_id"`
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
	{
		err := json.Unmarshal(reg.Payload, &wire)
		testassert.Require(t, err == nil, "decode operation register %q: %v", operationID, err)
	}
	return wire.State
}

// operationRegister returns the raw durable Operation register of one
// Operation.
func operationRegister(t *testing.T, store harness.Storage, sessionID, operationID string) harness.Register {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID})
	testassert.Require(t, err == nil, "read operation register %q: %v", operationID, err)
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
	{
		err := json.Unmarshal(reg.Payload, &wire)
		testassert.NoError(t, err)
	}
	return wire.State.CurrentOperationID
}

// recoverEntries lists one Session's committed entries in sequence order.
func recoverEntries(t *testing.T, store harness.Storage, sessionID string) []harness.Entry {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.Require(t, err == nil, "read entries of session %q: %v", sessionID, err)
	return entries
}

// newRawSessionID returns one fresh durable 32-hex identity for raw seeding.
func newRawSessionID(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	{
		_, err := rand.Read(buf[:])
		testassert.NoError(t, err)
	}
	return hex.EncodeToString(buf[:])
}

// insertRawRegister inserts one register payload directly through the storage
// contract. The addressed Session must already exist for Operation registers
// and entries.
func insertRawRegister(t *testing.T, store harness.Storage, key harness.RegisterKey, payload string) {
	t.Helper()
	{
		err := store.Transact(context.Background(), func(tx harness.Transaction) error {
			_, err := tx.InsertRegister(harness.RegisterDraft{Key: key, Payload: json.RawMessage(payload)})
			return err
		})
		testassert.Require(t, err == nil, "insert register %v: %v", key, err)
	}
}

// insertRawEntry inserts one entry payload directly through the storage
// contract. The addressed Session must already exist.
func insertRawEntry(t *testing.T, store harness.Storage, sessionID, id, operationID string, kind harness.EntryKind, payload string) {
	t.Helper()
	{
		err := store.Transact(context.Background(), func(tx harness.Transaction) error {
			_, err := tx.InsertEntry(harness.EntryDraft{SessionID: sessionID, ID: id, OperationID: operationID, Kind: kind, Payload: json.RawMessage(payload)})
			return err
		})
		testassert.Require(t, err == nil, "insert entry %s: %v", id, err)
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
			`"context_window":4096,"output_reserve":2048,`+
			`"system_prompt":"system","tools":[%s],"readonly":false,"write_dir":"",`+
			`"compact":{"model":{"provider":"cprov","model":"compact-x"},"system_prompt":"summarize"}},"admitted_at":%q}`,
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
	testassert.Require(t, (activeEffect != "model" || len(calls) <= 0) && (activeEffect != "tool" && activeEffect != "hook" || len(calls) != 0), "a model effect admits no pending calls; a %s effect requires a first pending call", activeEffect)
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
	case "hook":
		modelReserved = newRawSessionID(t) // the hook effect's own reserved result identity
		activeEffectJSON = fmt.Sprintf(`,"active_effect":{"kind":"hook","result_entry_id":%q,"tool_call_id":%q,"hook_id":"cap.hook.one"}`, modelReserved, calls[0])
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
	testassert.Require(t, err == nil, "read registers of %q: %v", sessionID, err)
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.Require(t, err == nil, "read entries of %q: %v", sessionID, err)
	return sessionSnapshot{registers: registers, entries: entries}
}

// assertSessionUnchanged verifies that recovery left one Session's complete
// durable state byte-identical at the same revisions: no repair, no
// truncation, no rewrite.
func assertSessionUnchanged(t *testing.T, store harness.Storage, sessionID string, before sessionSnapshot) {
	t.Helper()
	after := snapshotSession(t, store, sessionID)
	testassert.Require(t, len(after.registers) == len(before.registers) && len(after.entries) == len(before.entries), "recovery changed the record count of corrupt session %q (registers %d -> %d, entries %d -> %d)", sessionID, len(before.registers), len(after.registers), len(before.entries), len(after.entries))
	for i := range before.registers {
		b, a := before.registers[i], after.registers[i]
		testassert.Require(t, b.Key == a.Key && b.Revision == a.Revision && bytes.Equal(b.Payload, a.Payload), "recovery changed register %v of corrupt session %q (revision %d -> %d)", b.Key, sessionID, b.Revision, a.Revision)
	}
	for i := range before.entries {
		b, a := before.entries[i], after.entries[i]
		testassert.Require(t, b.ID == a.ID && b.Kind == a.Kind && b.Sequence == a.Sequence && bytes.Equal(b.Payload, a.Payload), "recovery changed entry %s of corrupt session %q", b.ID, sessionID)
	}
}

// harnessOver constructs one live Harness over an already-recovered store for
// materialization verification only; its preparation callback must never run.
func harnessOver(t *testing.T, store harness.Storage) *harness.Harness {
	t.Helper()
	h, err := harness.New(context.Background(), harness.Dependencies{Storage: store, Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
		return harness.PreparedExecution{}, errors.New("the recovery suite never prepares")
	}})
	testassert.NoError(t, err)
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
	testassert.Require(t, state.Status == "interruption" && state.ActiveEffect == nil && len(state.PendingToolCalls) == 0 && state.Terminal != nil, "recovered operation = %+v, want terminal interruption with no active or pending effects", state)
	testassert.Equal(t, state.Terminal.Detail, recoveredInterruptionDetail)
	settlementID := state.Terminal.SettlementEntry.EntryID
	if wantSettlementID != "" {
		testassert.Equal(t, settlementID, wantSettlementID)
	} else {
		seen := map[string]bool{}
		for _, e := range preEntries {
			seen[e.ID] = true
		}
		for _, id := range wantInterruptedResultIDs {
			seen[id] = true
		}
		testassert.Require(t, len(settlementID) == 32 && !seen[settlementID], "settlement entry = %q, want a fresh identity unused by the pre-recovery state", settlementID)
	}
	{
		current := recoverSessionCurrent(t, store, sessionID)
		testassert.Equal(t, current, "")
	}
	wantTail := make([]harness.EntryKind, 0, len(wantInterruptedResultIDs)+2)
	for range wantInterruptedResultIDs {
		wantTail = append(wantTail, harness.EntryToolResult)
	}
	wantTail = append(wantTail, harness.EntrySignal, harness.EntryOperationSettlement)
	entries := recoverEntries(t, store, sessionID)
	testassert.Require(t, len(entries) == len(preEntries)+len(wantTail), "recovered entries = %d, want the pre-recovery %d plus the %d recovery commits", len(entries), len(preEntries), len(wantTail))
	for i, want := range preEntries {
		testassert.Require(t, entries[i].ID == want.ID && entries[i].Kind == want.Kind && bytes.Equal(entries[i].Payload, want.Payload), "pre-recovery entry %d = %s/%s, want %s/%s unchanged", i, entries[i].ID, entries[i].Kind, want.ID, want.Kind)
	}
	for i, kind := range wantTail {
		entry := entries[len(preEntries)+i]
		testassert.Require(t, entry.Kind == kind, "recovery commit %d is a %s entry, want %s", i, entry.Kind, kind)
		switch kind {
		case harness.EntryToolResult:
			testassert.Require(t, entry.ID == wantInterruptedResultIDs[i], "interrupted result %d = %s, want the reserved identity %s", i, entry.ID, wantInterruptedResultIDs[i])
			var wire struct {
				Status  model.ToolResultStatus `json:"status"`
				Content string                 `json:"content"`
			}
			{
				err := json.Unmarshal(entry.Payload, &wire)
				testassert.Require(t, err == nil && wire.Status == model.ResultInterrupted && wire.Content == "Tool call interrupted.", "interrupted tool payload = %s, want the ordinary interrupted result", entry.Payload)
			}
		case harness.EntryOperationSettlement:
			testassert.Equal(t, entry.ID, settlementID)
			var wire struct {
				Status string `json:"status"`
				Detail string `json:"detail"`
			}
			{
				err := json.Unmarshal(entry.Payload, &wire)
				testassert.NoError(t, err)
			}
			testassert.Require(t, wire.Status == "interruption" && wire.Detail == recoveredInterruptionDetail, "settlement payload = %+v, want the terminal interruption with the recovery detail", wire)
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
	{
		err := harness.Recover(context.Background(), store)
		testassert.NoError(t, err)
	}
	opAfter := operationRegister(t, store, sessionID, operationID)
	sessAfter := sessionRegister(t, store, sessionID)
	testassert.Require(t, opAfter.Revision == opBefore.Revision && bytes.Equal(opAfter.Payload, opBefore.Payload), "second recover changed the operation register (revision %d -> %d)", opBefore.Revision, opAfter.Revision)
	testassert.Require(t, sessAfter.Revision == sessBefore.Revision && bytes.Equal(sessAfter.Payload, sessBefore.Payload), "second recover changed the session register (revision %d -> %d)", sessBefore.Revision, sessAfter.Revision)
	{
		got := len(recoverEntries(t, store, sessionID))
		testassert.Require(t, got == len(entriesBefore), "second recover committed %d entries, want none", got-len(entriesBefore))
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
				testassert.Require(t, state.Status == "running" && len(state.PendingToolCalls) == shape.calls, "seeded operation = %+v, want running with %d pending call(s)", state, shape.calls)
				switch shape.activeEffect {
				case "model":
					testassert.Require(t, state.ActiveEffect != nil && state.ActiveEffect.Kind == "model" && state.ActiveEffect.ResultEntryID == modelReserved, "seeded operation = %+v, want the committed model effect intent", state)
				case "tool":
					testassert.Require(t, state.ActiveEffect != nil && state.ActiveEffect.Kind == "tool" && state.ActiveEffect.ToolCallID == calls[0] && state.ActiveEffect.ResultEntryID == pendingResultIDs[0], "seeded operation = %+v, want the committed tool effect intent on %s", state, calls[0])
				default:
					testassert.Require(t, state.ActiveEffect == nil, "seeded operation = %+v, want no active effect", state)
				}

				pre := recoverEntries(t, store, sessionID)
				// before any live Harness over the store
				{
					err := harness.Recover(ctx, store)
					testassert.NoError(t, err)
				}
				assertRecoveredOperation(t, store, sessionID, "op-1", pre, modelReserved, pendingResultIDs)
				assertRecoverIdempotent(t, store, sessionID, "op-1")

				h := harnessOver(t, store) // one Harness after recovery: materialization verification only
				sess, err := h.ReadSessionHeader(ctx, sessionID)
				testassert.Require(t, err == nil && sess.CurrentOperationID == "", "repaired session = %+v err %v, want open with the current Operation cleared", sess, err)
				rec, err := h.ReadOperation(ctx, sessionID, "op-1")
				testassert.Require(t, err == nil && rec.State.Status == harness.OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == recoveredInterruptionDetail, "repaired operation = %+v err %v, want the terminal interruption with the recovery detail", rec, err)
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

// rawHookResultPayload builds one settled success hook-result entry payload
// for one hook execution of the given call.
func rawHookResultPayload(sessionID, entryID, operationID, hookID, callID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"hook_id":%q,"tool_call_id":%q,"status":"success","arguments":{"x":1}}`,
		sessionID, entryID, operationID, hookID, callID)
}

// rawFailedHookResultPayload builds one settled failed or interrupted
// hook-result entry payload carrying a non-empty error and no arguments.
func rawFailedHookResultPayload(sessionID, entryID, operationID, hookID, callID, status, errText string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"hook_id":%q,"tool_call_id":%q,"status":%q,"error":%q}`,
		sessionID, entryID, operationID, hookID, callID, status, errText)
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

// seedCorruptSiblingTwoCalls seeds the valid target graph with two published
// calls left pending in publication order.
func seedCorruptSiblingTwoCalls(t *testing.T, store harness.Storage) string {
	t.Helper()
	sessionID, _, _ := seedRunningOperationShape(t, store, "", "op-1", []string{"call-1", "call-2"})
	return sessionID
}

// seedCorruptSiblingHookResult seeds the valid target graph whose single
// published call carries one settled success hook result between its
// publishing assistant and the still-pending state.
func seedCorruptSiblingHookResult(t *testing.T, store harness.Storage) string {
	t.Helper()
	sessionID, _, _ := seedRunningOperationShape(t, store, "", "op-1", []string{"call-1"})
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	for _, entry := range entries {
		if entry.Kind == harness.EntryAssistant {
			hookID := newRawSessionID(t)
			insertRawEntry(t, store, sessionID, hookID, "op-1", harness.EntryHookResult,
				rawHookResultPayload(sessionID, hookID, "op-1", "cap.hook.one", "call-1"))
			return sessionID
		}
	}
	t.Fatalf("seeded graph carries no assistant entry")
	return ""
}

// seedCorruptSiblingFailedHook seeds the valid target graph whose single
// published call carries one settled failed hook result between its
// publishing assistant and the still-pending state.
func seedCorruptSiblingFailedHook(t *testing.T, store harness.Storage) string {
	t.Helper()
	sessionID, _, _ := seedRunningOperationShape(t, store, "", "op-1", []string{"call-1"})
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	for _, entry := range entries {
		if entry.Kind == harness.EntryAssistant {
			hookID := newRawSessionID(t)
			insertRawEntry(t, store, sessionID, hookID, "op-1", harness.EntryHookResult,
				rawFailedHookResultPayload(sessionID, hookID, "op-1", "cap.hook.one", "call-1", "error", "hook broke"))
			return sessionID
		}
	}
	t.Fatalf("seeded graph carries no assistant entry")
	return ""
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
	{
		err := json.Unmarshal(raw, &obj)
		testassert.NoError(t, err)
	}
	edit(obj)
	out, err := json.Marshal(obj)
	testassert.NoError(t, err)
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
	{
		err := json.Unmarshal(raw, &items)
		testassert.NoError(t, err)
	}
	edit(items)
	out, err := json.Marshal(items)
	testassert.NoError(t, err)
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
	{
		err := json.Unmarshal(assistant.Payload, &wire)
		testassert.Require(t, err == nil && len(wire.ToolCalls) == 1, "assistant payload has no single published call: %v", err)
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
			name: "validator: hook result before its publishing assistant",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				hook := newRawSessionID(t)
				snap.entries = append(snap.entries, harness.Entry{
					SessionID: sessionID, ID: hook, Sequence: 2, OperationID: "op-1",
					Kind: harness.EntryHookResult, Payload: json.RawMessage(rawHookResultPayload(sessionID, hook, "op-1", "cap.hook.one", "call-1")),
				})
			},
		},
		{
			name: "validator: hook result after its call's terminal tool result",
			seed: seedCorruptSiblingWithResult,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				hook := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, hook, "op-1", harness.EntryHookResult,
					rawHookResultPayload(sessionID, hook, "op-1", "cap.hook.one", "call-1"))
			},
		},
		{
			name: "validator: duplicate hook result for one call",
			seed: seedCorruptSiblingHookResult,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				hook := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, hook, "op-1", harness.EntryHookResult,
					rawHookResultPayload(sessionID, hook, "op-1", "cap.hook.one", "call-1"))
			},
		},
		{
			name: "validator: hook result answers unpublished call",
			seed: seedCorruptSiblingRunning,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				hook := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, hook, "op-1", harness.EntryHookResult,
					rawHookResultPayload(sessionID, hook, "op-1", "cap.hook.one", "call-9"))
			},
		},
		{
			name: "validator: active hook effect repeats a settled hook",
			seed: seedCorruptSiblingHookResult,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["active_effect"] = json.RawMessage(fmt.Sprintf(`{"kind":"hook","result_entry_id":%q,"tool_call_id":"call-1","hook_id":"cap.hook.one"}`, corruptSiblingOtherEntry))
				})
			},
		},
		{
			name: "validator: hook result after a failed hook for the same call",
			seed: seedCorruptSiblingFailedHook,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				hook := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, hook, "op-1", harness.EntryHookResult,
					rawHookResultPayload(sessionID, hook, "op-1", "cap.hook.two", "call-1"))
			},
		},
		{
			name: "validator: active hook effect after a failed hook",
			seed: seedCorruptSiblingFailedHook,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["active_effect"] = json.RawMessage(fmt.Sprintf(`{"kind":"hook","result_entry_id":%q,"tool_call_id":"call-1","hook_id":"cap.hook.two"}`, corruptSiblingOtherEntry))
				})
			},
		},
		{
			name: "validator: active tool effect after a failed hook",
			seed: seedCorruptSiblingFailedHook,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["active_effect"] = json.RawMessage(fmt.Sprintf(`{"kind":"tool","result_entry_id":%q,"tool_call_id":"call-1"}`, snapReservedResultID(t, snap)))
				})
			},
		},
		{
			name: "validator: interrupted hook with an error tool result",
			seed: seedCorruptSiblingFailedHook,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				hook := snapEntryOf(snap, harness.EntryHookResult)
				hook.Payload = editPayloadObject(t, hook.Payload, func(obj map[string]json.RawMessage) {
					obj["status"] = json.RawMessage(`"interrupted"`)
				})
				assistant := snapEntryOf(snap, harness.EntryAssistant)
				reserved := snapReservedResultID(t, snap)
				failed := editPayloadObject(t, json.RawMessage(rawToolResultPayload(sessionID, reserved, "op-1", assistant.ID, "call-1")), func(obj map[string]json.RawMessage) {
					obj["status"] = json.RawMessage(`"error"`)
					obj["content"] = json.RawMessage(`"hook broke"`)
				})
				snapAppendEntry(snap, sessionID, reserved, "op-1", harness.EntryToolResult, string(failed))
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["pending_tool_calls"] = json.RawMessage(`[]`)
				})
			},
		},
		{
			name: "validator: error hook with a success tool result",
			seed: seedCorruptSiblingFailedHook,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				assistant := snapEntryOf(snap, harness.EntryAssistant)
				reserved := snapReservedResultID(t, snap)
				snapAppendEntry(snap, sessionID, reserved, "op-1", harness.EntryToolResult,
					rawToolResultPayload(sessionID, reserved, "op-1", assistant.ID, "call-1"))
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["pending_tool_calls"] = json.RawMessage(`[]`)
				})
			},
		},
		{
			name: "validator: hook for a later call while an earlier call is unresolved",
			seed: seedCorruptSiblingTwoCalls,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				hookID := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, hookID, "op-1", harness.EntryHookResult,
					rawHookResultPayload(sessionID, hookID, "op-1", "cap.hook.one", "call-2"))
			},
		},
		{
			name: "validator: hook for a later call before the earlier call's result",
			seed: seedCorruptSiblingTwoCalls,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				sessionID := snapSessionID(snap)
				hook := newRawSessionID(t)
				snapAppendEntry(snap, sessionID, hook, "op-1", harness.EntryHookResult,
					rawHookResultPayload(sessionID, hook, "op-1", "cap.hook.one", "call-2"))
				assistant := snapEntryOf(snap, harness.EntryAssistant)
				var wire struct {
					ToolCalls []struct {
						ID            string `json:"id"`
						ResultEntryID string `json:"result_entry_id"`
					} `json:"tool_calls"`
				}
				{
					err := json.Unmarshal(assistant.Payload, &wire)
					testassert.Require(t, err == nil && len(wire.ToolCalls) == 2, "seeded assistant has no two published calls: %v", err)
				}
				snapAppendEntry(snap, sessionID, wire.ToolCalls[0].ResultEntryID, "op-1", harness.EntryToolResult,
					rawToolResultPayload(sessionID, wire.ToolCalls[0].ResultEntryID, "op-1", assistant.ID, "call-1"))
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["pending_tool_calls"] = json.RawMessage(fmt.Sprintf(
						`[{"assistant_entry":{"session_id":%q,"entry_id":%q},"call_id":"call-2","result_entry_id":%q}]`,
						sessionID, assistant.ID, wire.ToolCalls[1].ResultEntryID))
				})
			},
		},
		{
			name: "validator: hook active effect reserves a pending tool-result identity",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				reserved := snapReservedResultID(t, snap)
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["active_effect"] = json.RawMessage(fmt.Sprintf(`{"kind":"hook","result_entry_id":%q,"tool_call_id":"call-1","hook_id":"cap.hook.one"}`, reserved))
				})
			},
		},
		{
			name: "validator: hook active effect reserves an already committed entry",
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				input := snapEntryOf(snap, harness.EntryInput)
				reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
				reg.Payload = editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
					state["active_effect"] = json.RawMessage(fmt.Sprintf(`{"kind":"hook","result_entry_id":%q,"tool_call_id":"call-1","hook_id":"cap.hook.one"}`, input.ID))
				})
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
		{name: "hook_result", seed: seedCorruptSiblingHookResult, kind: harness.EntryHookResult, container: "status", wrong: `[]`},
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

	// The tool_result metadata member inventory: persisted values that
	// violate the durable member rules — a null member (the schema-valid
	// representation of malformed metadata: absence encodes by omission,
	// never null) and a member beyond the bound — isolate their Session at
	// the current supported version while a valid sibling stays usable.
	metadataMutations := []struct {
		name     string
		metadata json.RawMessage
	}{
		{"null member", json.RawMessage(`null`)},
		{"raw bytes over the bound", json.RawMessage(`"` + strings.Repeat("x", 1+(1<<20)) + `"`)},
	}
	for _, mutation := range metadataMutations {
		rows = append(rows, corruptSiblingRow{
			name: "tool_result metadata wire: " + mutation.name,
			seed: seedCorruptSiblingWithResult,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				entry := snapEntryOf(snap, harness.EntryToolResult)
				entry.Payload = editPayloadObject(t, entry.Payload, func(obj map[string]json.RawMessage) {
					obj["metadata"] = mutation.metadata
				})
			},
		})
	}

	// The assistant normalized_arguments member inventory: the producer only
	// ever writes one complete JSON object, so a persisted non-object member
	// (an array representative — every wrong kind fails the same object check)
	// is codec corruption: it isolates its Session at decode and can never
	// reach the tool boundary as executable input.
	normalizedKinds := []struct {
		name  string
		wrong json.RawMessage
	}{
		{"array kind", json.RawMessage(`[1,2]`)},
	}
	for _, mutation := range normalizedKinds {
		rows = append(rows, corruptSiblingRow{
			name: "assistant normalized_arguments wire: " + mutation.name,
			seed: seedCorruptSiblingPendingCall,
			mutate: func(t *testing.T, snap *sessionSnapshot) {
				entry := snapEntryOf(snap, harness.EntryAssistant)
				entry.Payload = editPayloadObject(t, entry.Payload, func(obj map[string]json.RawMessage) {
					obj["tool_calls"] = editJSONArray(t, obj["tool_calls"], func(items []map[string]json.RawMessage) {
						items[0]["normalized_arguments"] = mutation.wrong
					})
				})
			},
		})
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

	// A malformed persisted execution-capture permission member isolates its
	// Session with the typed corruption error while a valid sibling stays
	// usable. The codec table owns the missing/null/wrong-type shape
	// enumeration for both members; this row proves the recovery routing.
	rows = append(rows, corruptSiblingRow{
		name: "operation admission execution wire: malformed readonly",
		seed: seedCorruptSiblingRunning,
		mutate: func(t *testing.T, snap *sessionSnapshot) {
			reg := snapRegisterOf(snap, harness.RegisterOperation, "op-1")
			reg.Payload = editOperationAdmission(t, reg.Payload, func(admission map[string]json.RawMessage) {
				admission["execution"] = editPayloadObject(t, admission["execution"], func(exec map[string]json.RawMessage) {
					exec["readonly"] = json.RawMessage(`"true"`)
				})
			})
		},
	})

	for _, row := range rows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, store harness.Storage) {
				ctx := context.Background()

				// The unmutated graph materializes on a separate fresh store.
				baseline := storage.NewMemory()
				baselineID := row.seed(t, baseline)
				{
					_, err := harnessOver(t, baseline).ReadSessionHeader(ctx, baselineID)
					testassert.NoError(t, err)
				}

				targetID := row.seed(t, store)
				snap := snapshotSession(t, store, targetID)
				row.mutate(t, &snap)
				siblingID, _, _ := seedRunningOperationShape(t, store, "", "op-sib", nil) // a valid quiet running sibling
				before := snapshotSession(t, store, targetID)
				pre := recoverEntries(t, store, siblingID)

				wrapped := &corruptSiblingStore{Storage: store, session: targetID, registers: snap.registers, entries: snap.entries}
				{
					err := harness.Recover(ctx, wrapped)
					testassert.Require(t, err == nil, "recover with %q corruption = %v, want the corruption left in place and the pass completed", row.name, err)
				}
				assertSessionUnchanged(t, store, targetID, before)
				assertRecoveredOperation(t, store, siblingID, "op-sib", pre, "", nil)

				h := harnessOver(t, wrapped) // constructed only after recovery: materialization verification
				{
					_, err := h.ReadSessionHeader(ctx, targetID)
					testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "corrupt session after recovery = err %v, want the corruption error", err)
				}
				rec, err := h.ReadOperation(ctx, siblingID, "op-sib")
				testassert.Require(t, err == nil && rec.State.Status == harness.OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == recoveredInterruptionDetail, "repaired sibling after recovery = %+v err %v, want the terminal interruption with the recovery detail", rec, err)
			})
		})
	}
}

// forkSiblingID sorts before every random 32-hex identity, so the lookup
// scan reaches it before any real Session.
const forkSiblingID = "00000000000000000000000000000000"

// forkEntryOf returns one committed entry of one Session by kind and owning
// Operation identity (empty matches operationless copied entries).
func forkEntryOf(t *testing.T, store harness.Storage, sessionID string, kind harness.EntryKind, operationID string) harness.Entry {
	t.Helper()
	for _, entry := range recoverEntries(t, store, sessionID) {
		if entry.Kind == kind && (operationID == "" || entry.OperationID == operationID) {
			return entry
		}
	}
	t.Fatalf("no %s entry owned by %q in session %q", kind, operationID, sessionID)
	return harness.Entry{}
}

// assertForkSourceUnchanged verifies that a fork left one source Session's
// complete durable state byte-identical at the same revisions.
func assertForkSourceUnchanged(t *testing.T, store harness.Storage, sessionID string, before sessionSnapshot) {
	t.Helper()
	after := snapshotSession(t, store, sessionID)
	testassert.Require(t, len(after.registers) == len(before.registers) && len(after.entries) == len(before.entries), "fork changed the record count of source %q (registers %d -> %d, entries %d -> %d)", sessionID, len(before.registers), len(after.registers), len(before.entries), len(after.entries))
	for i := range before.registers {
		b, a := before.registers[i], after.registers[i]
		testassert.Require(t, b.Key == a.Key && b.Revision == a.Revision && bytes.Equal(b.Payload, a.Payload), "fork changed register %v of source %q (revision %d -> %d)", b.Key, sessionID, b.Revision, a.Revision)
	}
	for i := range before.entries {
		b, a := before.entries[i], after.entries[i]
		testassert.Require(t, b.ID == a.ID && b.Kind == a.Kind && b.Sequence == a.Sequence && b.OperationID == a.OperationID && bytes.Equal(b.Payload, a.Payload), "fork changed entry %s of source %q", b.ID, sessionID)
	}
}

// assertSessionIDs asserts the exact Session listing of one store as a set
// (storage returns it sorted).
func assertSessionIDs(t *testing.T, store harness.Storage, want ...string) {
	t.Helper()
	ids, err := store.ListSessionIDs(context.Background())
	testassert.NoError(t, err)
	got := append([]string(nil), ids...)
	testassert.Require(t, len(got) == len(want), "session listing = %v, want %v", got, want)
	wantSet := make(map[string]bool, len(want))
	for _, id := range want {
		testassert.Require(t, !wantSet[id], "session listing expectation repeats %q", id)
		wantSet[id] = true
	}
	for _, id := range got {
		testassert.Require(t, wantSet[id], "session listing = %v, want %v", got, want)
	}
}

// rawForkSessionRegister builds one open fork Session register payload with
// the given source/boundary lineage.
func rawForkSessionRegister(sessionID, sourceID, boundaryID, currentOp string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	current := ""
	if currentOp != "" {
		current = fmt.Sprintf(`,"current_operation_id":%q`, currentOp)
	}
	return fmt.Sprintf(
		`{"identity":{"session_id":%q,"workspace":"/tmp/works","created_at":%q,`+
			`"source_session_id":%q,"source_boundary_entry_id":%q},`+
			`"state":{"lifecycle":"open","current_agent_type":"coder"%s,"usage":{"by_model":[]},"last_activity":%q}}`,
		sessionID, now, sourceID, boundaryID, current, now)
}

// seedForkDecoy seeds one valid fork destination look-alike Session owning a
// running message Operation, with lineage when withLineage holds, and returns
// its Session identity.
func seedForkDecoy(t *testing.T, store harness.Storage, sourceID, boundaryID, operationID string, withLineage bool) string {
	t.Helper()
	sid, entryID := newRawSessionID(t), newRawSessionID(t)
	payload := rawSessionRegister(sid, operationID)
	if withLineage {
		payload = rawForkSessionRegister(sid, sourceID, boundaryID, operationID)
	}
	insertRawRegister(t, store, harness.RegisterKey{SessionID: sid, Kind: harness.RegisterSession}, payload)
	insertRawEntry(t, store, sid, entryID, operationID, harness.EntryInput, rawInputEntryPayload(sid, entryID, operationID))
	insertRawRegister(t, store, harness.RegisterKey{SessionID: sid, Kind: harness.RegisterOperation, OperationID: operationID}, rawRunningOperationPayload(sid, operationID, entryID))
	return sid
}

// racePrepared returns the fixture's ordinary prepared execution for a race
// fixture whose preparation hook must still succeed.
func racePrepared(f *publicFixture) harness.PreparedExecution {
	return harness.PreparedExecution{
		Capture: publicCapture(),
		Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
			f.opens.Add(1)
			return publicExecution(f.model.effect, f.model.effect), nil
		},
	}
}

// corruptReadStore serves one Session's register reads as Session-scoped
// corruption and delegates everything else.
type corruptReadStore struct {
	harness.Storage
	session string
}

func (s *corruptReadStore) ReadRegister(ctx context.Context, key harness.RegisterKey) (harness.Register, error) {
	if key.SessionID == s.session {
		return harness.Register{}, &harness.CorruptionError{SessionID: s.session, Detail: "injected register read corruption"}
	}
	return s.Storage.ReadRegister(ctx, key)
}

// TestPublicForkCopiesPrefixAndAdmits proves the fork row's positive core
// through public operations: an idle source forks at a user-origin input
// boundary; the destination inherits the source Workspace and current Agent
// type with informational source/boundary lineage and zero usage; only the
// strict-before-boundary input, assistant, tool_result, and signal entries
// are copied, under new operationless identities with cleared usage, the
// rewritten result/assistant references, and the copied signal's source
// Operation left as informational history; settlements and the boundary
// itself are excluded; the fresh admission is a user-origin input owning a
// running message Operation; the source stays byte-identical; and the
// destination execution starts after the commit and projects the copied
// prefix before the fork input.
func TestPublicForkCopiesPrefixAndAdmits(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		script := newScriptModel(
			publicTurn("call-1"),                             // turn 1 publishes a call
			publicAttempt{stream: publicPartialTurnStream()}, // turn 2 continues from the errored partial with the continuation signal
			publicFail("source turn terminal"),               // turn 3 settles op-1's model-originated terminal
			publicTurn(),                                     // op-2 completes
			publicTurn(),                                     // op-3 completes
		)
		f := newPublicFixture(t, store, script, nil)
		f.prepareHook = func(_ int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: publicCapture(),
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					return harness.Execution{
						Model:         script.effect,
						CompactModel:  script.effect,
						NormalizeTool: publicNormalize,
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							// the copied tool result carries tool-owned
							// formatted metadata that the fork must preserve
							// in its encoded durable form
							return harness.PreparedTool{Permissions: publicPermission, Immediate: &harness.ToolOutcome{
								Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ran call-1"},
								Metadata: json.RawMessage(`{ "n" : [1, 1.0, 1e0, 9007199254740993], "s" : "<>&" }`),
							}}
						},
					}, nil
				},
			}, nil
		}
		source := createSession(t, f.h)

		{
			_, err := submit(t, f.h, source, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived // the published call
		<-script.arrived // the continued partial turn
		<-script.arrived // the terminal turn
		{
			_, err := f.h.Submit(ctx, harness.SubmitRequest{
				SessionID:   source,
				OperationID: "op-2",
				Origin:      harness.InputOriginUser,
				Content:     []model.ContentPart{{Kind: model.PartText, Text: "second"}},
				Mode:        harness.MessageModeQueued,
			})
			testassert.NoError(t, err)
		}
		<-f.prepare // the drain admitted op-2: op-1's terminal committed
		<-script.arrived
		{
			_, err := f.h.Submit(ctx, harness.SubmitRequest{
				SessionID:   source,
				OperationID: "op-3",
				Origin:      harness.InputOriginUser,
				Content:     []model.ContentPart{{Kind: model.PartText, Text: "third"}},
				Mode:        harness.MessageModeQueued,
			})
			testassert.NoError(t, err)
		}
		<-f.prepare // the drain admitted op-3: op-2's terminal committed
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}

		boundary := forkEntryOf(t, store, source, harness.EntryInput, "op-2").ID
		before := snapshotSession(t, store, source)

		// the fork's own turn settles through its model-originated terminal,
		// durable inside the effect transaction regardless of later cancellation
		forkScript := newScriptModel(publicFail("fork turn settled"))
		forkScript.gate = make(chan struct{})
		f2 := newPublicFixture(t, store, forkScript, nil)
		defer f2.close()
		res, err := f2.h.Fork(ctx, harness.ForkRequest{
			SourceSessionID: source,
			BoundaryEntryID: boundary,
			OperationID:     "fork-1",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
		})
		testassert.NoError(t, err)
		dest := res.Session.Identity.SessionID
		testassert.Require(t, dest != source && res.Session.Identity.SessionID != "", "fork session identity = %+v, want one fresh identity", res.Session.Identity)
		testassert.Require(t, res.Session.Identity.Workspace == "/tmp/works" && res.Session.Identity.SourceSessionID == source && res.Session.Identity.SourceBoundaryEntryID == boundary && (res.Session.State.Lifecycle == harness.LifecycleOpen && res.Session.State.CurrentAgentType == "coder" && len(res.Session.State.Usage.ByModel) == 0 && res.Session.State.CurrentOperationID == "fork-1") && (res.Operation.State.Status == harness.OperationRunning && res.Operation.Admission.OperationID == "fork-1" && res.Operation.Admission.RequestKind == harness.RequestKindMessage && res.Operation.Admission.AgentType == "coder" && res.Operation.Admission.AdmittedEntry.SessionID == dest && res.Operation.Admission.Execution.ConfigurationRevision == "rev-1"), "fork identity = %+v, want the inherited workspace and source/boundary lineage; fork session state = %+v, want open, the source type, zero usage, and the running fork; fork operation = %+v, want a running message admission with the fresh capture", res.Session.Identity, res.Session.State, res.Operation)

		<-forkScript.arrived // the destination execution reached its model boundary
		{
			got := texts(forkScript.seen()[0])
			testassert.Require(t, len(got) == 7 && got[0] == "system" && got[1] == "hello" && got[2] == "done" && got[3] == "ran call-1" && got[4] == "partial" && got[5] == "<system-signal>The previous model response failed after partial output. Continue from the retained response.</system-signal>" && got[6] == "fork input", "fork projection = %v, want the copied prefix before the fork input", got)
		}
		forkScript.releaseGate()
		awaitTerminal(t, f2.h, dest, "fork-1") // the fork turn's settlement commits before the convergence cancellation
		{
			err := converge(t, f2)
			testassert.NoError(t, err)
		}

		calls := f2.preparationCalls()
		testassert.Require(t, len(calls) == 1 && calls[0].Session.Identity.Workspace == "/tmp/works" && calls[0].Session.AgentType == "coder", "fork preparation calls = %+v, want one call with the source workspace and Agent type", calls)

		// the source is byte-identical after the fork
		assertForkSourceUnchanged(t, store, source, before)

		// the destination carries exactly the copied prefix, then the fresh
		// user-origin admission input and the fork's model-originated terminal
		entries := recoverEntries(t, store, dest)
		wantKinds := []harness.EntryKind{harness.EntryInput, harness.EntryAssistant, harness.EntryToolResult, harness.EntryAssistant, harness.EntrySignal, harness.EntryInput, harness.EntryOperationSettlement}
		testassert.Require(t, len(entries) == len(wantKinds), "destination entries = %d, want the %d copied prefix entries, the fork admission, and the fork's settled turn", len(entries), 5)
		sourceIDs := map[string]bool{}
		for _, entry := range before.entries {
			sourceIDs[entry.ID] = true
		}
		for i, entry := range entries {
			testassert.Require(t, entry.Kind == wantKinds[i] && entry.SessionID == dest, "destination entry %d is a %s entry, want %s; destination entry %d addresses session %q", i, entry.Kind, wantKinds[i], i, entry.SessionID)
			if i < 5 { // copied prefix entries are operationless and freshly identified
				testassert.Require(t, entry.OperationID == "", "copied entry %s carries operation %q, want no source ownership", entry.ID, entry.OperationID)
				testassert.Require(t, !sourceIDs[entry.ID], "copied entry %s reuses a source identity", entry.ID)
			}
		}
		testassert.Require(t, entries[5].OperationID == "fork-1", "fork admission entry = %+v, want it owned by fork-1", entries[5])
		var copiedInput struct {
			Origin string `json:"origin"`
		}
		{
			err := json.Unmarshal(entries[0].Payload, &copiedInput)
			testassert.NoError(t, err)
		}
		testassert.Equal(t, copiedInput.Origin, "user")
		var copiedAssistant struct {
			ToolCalls []struct {
				ID                  string          `json:"id"`
				ResultEntryID       string          `json:"result_entry_id"`
				NormalizedArguments json.RawMessage `json:"normalized_arguments"`
			} `json:"tool_calls"`
			Usage *struct{} `json:"usage"`
		}
		{
			err := json.Unmarshal(entries[1].Payload, &copiedAssistant)
			testassert.NoError(t, err)
		}
		testassert.Require(t, copiedAssistant.Usage == nil, "copied assistant carries source usage")
		testassert.Require(t, len(copiedAssistant.ToolCalls) == 1 && copiedAssistant.ToolCalls[0].ID == "call-1" && copiedAssistant.ToolCalls[0].ResultEntryID == entries[2].ID, "copied call reservation = %+v, want it rewritten to the copied result entry %s", copiedAssistant.ToolCalls, entries[2].ID)
		testassert.Require(t, string(copiedAssistant.ToolCalls[0].NormalizedArguments) == `{"x":1}`, "copied assistant normalized arguments = %s, want the source's committed normalization preserved verbatim", copiedAssistant.ToolCalls[0].NormalizedArguments)
		var copiedResult struct {
			AssistantEntry struct {
				SessionID string `json:"session_id"`
				EntryID   string `json:"entry_id"`
			} `json:"assistant_entry"`
			ToolCallID string          `json:"tool_call_id"`
			Metadata   json.RawMessage `json:"metadata"`
		}
		{
			err := json.Unmarshal(entries[2].Payload, &copiedResult)
			testassert.NoError(t, err)
		}
		testassert.Require(t, copiedResult.AssistantEntry.SessionID == dest && copiedResult.AssistantEntry.EntryID == entries[1].ID && copiedResult.ToolCallID == "call-1" && string(copiedResult.Metadata) == `{"n":[1,1.0,1e0,9007199254740993],"s":"\u003c\u003e\u0026"}`, "copied result reference = %+v, want it rewritten inside the copied prefix; copied result metadata = %s, want the source's encoded metadata preserved verbatim", copiedResult.AssistantEntry, copiedResult.Metadata)
		var copiedSignal struct {
			RelatedOperation struct {
				SessionID   string `json:"session_id"`
				OperationID string `json:"operation_id"`
			} `json:"related_operation"`
		}
		{
			err := json.Unmarshal(entries[4].Payload, &copiedSignal)
			testassert.NoError(t, err)
		}
		testassert.Require(t, copiedSignal.RelatedOperation.SessionID == source && copiedSignal.RelatedOperation.OperationID == "op-1", "copied signal related operation = %+v, want the source Operation kept as informational history", copiedSignal.RelatedOperation)
		var admission struct {
			Origin string `json:"origin"`
		}
		{
			err := json.Unmarshal(entries[5].Payload, &admission)
			testassert.NoError(t, err)
		}
		testassert.Equal(t, admission.Origin, "user")

		// the fork's own turn settled through its model-originated terminal
		destHeader, err := f2.h.ReadSessionHeader(ctx, dest)
		testassert.Require(t, err == nil && destHeader.CurrentOperationID == "", "fork session after convergence = %+v err %v, want it settled", destHeader, err)
		destView, err := f2.h.ReadSessionView(ctx, dest)
		testassert.Require(t, err == nil && len(destView.TotalUsage.ByModel) == 0, "fork session usage = %+v err %v, want zero usage", destView.TotalUsage, err)
		rec, err := f2.h.ReadOperation(ctx, dest, "fork-1")
		testassert.Require(t, err == nil && rec.State.Status == harness.OperationFailure && rec.State.Terminal != nil && rec.State.Terminal.Detail == "fork turn settled", "fork operation after convergence = %+v err %v, want its model-originated terminal", rec, err)
	})
}

// TestPublicForkArchivedSource proves the archived-source axis: an archived
// idle source is read without reopening, the fork succeeds, and the source
// stays archived while the destination is open.
func TestPublicForkArchivedSource(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		source := seedIdleSource(t, store, "op-1")

		f2 := newPublicFixture(t, store, newScriptModel(), nil)
		defer f2.close()
		{
			_, err := f2.h.ArchiveSession(ctx, source)
			testassert.NoError(t, err)
		}
		boundary := forkEntryOf(t, store, source, harness.EntryInput, "op-1").ID
		res, err := f2.h.Fork(ctx, harness.ForkRequest{
			SourceSessionID: source,
			BoundaryEntryID: boundary,
			OperationID:     "fork-1",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
		})
		testassert.NoError(t, err)
		testassert.Equal(t, res.Session.State.Lifecycle, harness.LifecycleOpen)
		src, err := f2.h.ReadSessionHeader(ctx, source)
		testassert.Require(t, err == nil && src.Lifecycle == harness.LifecycleArchived, "source after fork = %+v err %v, want it archived without reopening", src, err)
		{
			err := converge(t, f2)
			testassert.NoError(t, err)
		}
	})
}

// TestPublicForkBoundaryValidation proves the boundary/precondition axis: a
// running source, a missing boundary, a non-input boundary, a non-user
// boundary origin, and malformed identities are all rejected with nothing
// published.
func TestPublicForkBoundaryValidation(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		script := newScriptModel()
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		source := createSession(t, f.h)

		{
			_, err := submit(t, f.h, source, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived // parked: the source is running
		{
			_, err := f.h.Fork(ctx, harness.ForkRequest{
				SourceSessionID: source,
				BoundaryEntryID: newRawSessionID(t),
				OperationID:     "fork-1",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "fork of a running source = err %v, want ErrInvalid", err)
		}
		{
			_, err := f.h.Fork(ctx, harness.ForkRequest{SourceSessionID: "short", BoundaryEntryID: newRawSessionID(t), OperationID: "fork-1"})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "fork with a malformed source identity = err %v, want ErrInvalid", err)
		}
		{
			_, err := f.h.Fork(ctx, harness.ForkRequest{SourceSessionID: source, BoundaryEntryID: "short", OperationID: "fork-1"})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "fork with a malformed boundary identity = err %v, want ErrInvalid", err)
		}
		{
			_, err := f.h.Fork(ctx, harness.ForkRequest{SourceSessionID: source, BoundaryEntryID: newRawSessionID(t), OperationID: ""})
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "fork without an operation identity = err %v, want ErrInvalid", err)
		}
		script.releaseGate()
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}

		// an idle source with one runtime-origin input: non-user boundaries are invalid
		runtimeScript := newScriptModel()
		f2 := newPublicFixture(t, store, runtimeScript, nil)
		defer f2.close()
		{
			_, err := f2.h.Submit(ctx, harness.SubmitRequest{
				SessionID:   source,
				OperationID: "op-2",
				Origin:      harness.InputOriginRuntime,
				Content:     []model.ContentPart{{Kind: model.PartText, Text: "runtime input"}},
				Mode:        harness.MessageModeRegular,
			})
			testassert.NoError(t, err)
		}
		<-runtimeScript.arrived
		{
			err := converge(t, f2)
			testassert.NoError(t, err)
		}

		f3 := newPublicFixture(t, store, newScriptModel(), nil)
		defer f3.close()
		runtimeBoundary := forkEntryOf(t, store, source, harness.EntryInput, "op-2").ID
		assistantBoundary := forkEntryOf(t, store, source, harness.EntryAssistant, "op-1").ID
		for _, tt := range []struct {
			name     string
			boundary string
		}{
			{"missing boundary", newRawSessionID(t)},
			{"non-input boundary", assistantBoundary},
			{"runtime-origin boundary", runtimeBoundary},
		} {
			{
				_, err := f3.h.Fork(ctx, harness.ForkRequest{
					SourceSessionID: source,
					BoundaryEntryID: tt.boundary,
					OperationID:     "fork-1",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
				})
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "fork with %s = err %v, want ErrInvalid", tt.name, err)
			}
		}
		assertSessionIDs(t, store, source) // nothing was published by any rejection
	})
}

// TestPublicForkExistingLookup proves the idempotency axis of the fork row:
// the existing-ID lookup precedes source validation and preparation and
// returns only graph-validated state — a retry returns the first destination
// even with changed content and without re-preparing, it resolves after the
// source is deleted, other lineage/request reuse of the ID is invalid, a
// corrupt located owner surfaces its corruption, and Session-scoped
// corruption before the owner is located is skipped. An affected Session
// already materialized in the reporting Harness becomes sticky-corrupt for
// its later reads while valid siblings remain usable.
func TestPublicForkExistingLookup(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		source := seedIdleSource(t, store, "op-1")
		boundary := forkEntryOf(t, store, source, harness.EntryInput, "op-1").ID
		forkReq := harness.ForkRequest{
			SourceSessionID: source,
			BoundaryEntryID: boundary,
			OperationID:     "fork-a",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
		}

		f2 := newPublicFixture(t, store, newScriptModel(), nil)
		defer f2.close()
		first, err := f2.h.Fork(ctx, forkReq)
		testassert.NoError(t, err)
		dest := first.Session.Identity.SessionID
		{
			err := converge(t, f2)
			testassert.NoError(t, err)
		}

		// one further live idle source for the later subtests
		otherScript := newScriptModel()
		f3 := newPublicFixture(t, store, otherScript, nil)
		defer f3.close()
		other := createSession(t, f3.h)
		{
			_, err := submit(t, f3.h, other, "fork-b", harness.MessageModeRegular, "reused")
			testassert.NoError(t, err)
		}
		<-otherScript.arrived
		{
			err := converge(t, f3)
			testassert.NoError(t, err)
		}
		otherBoundary := forkEntryOf(t, store, other, harness.EntryInput, "fork-b").ID

		t.Run("retry returns the first destination without re-preparing", func(t *testing.T) {
			f4 := newPublicFixture(t, store, newScriptModel(), nil)
			defer f4.close()
			retry := forkReq
			retry.Content = []model.ContentPart{{Kind: model.PartText, Text: "changed"}}
			res, err := f4.h.Fork(ctx, retry)
			testassert.NoError(t, err)
			testassert.Require(t, res.Session.Identity.SessionID == dest && res.Operation.Admission.AdmittedEntry == first.Operation.Admission.AdmittedEntry, "retry = %+v, want the first destination", res)
			{
				calls := f4.preparationCalls()
				testassert.Equal(t, len(calls), 0)
			}
			assertSessionIDs(t, store, dest, other, source)
		})

		t.Run("lookup precedes source validation", func(t *testing.T) {
			f5 := newPublicFixture(t, store, newScriptModel(), nil)
			defer f5.close()
			{
				_, err := f5.h.ArchiveSession(ctx, source)
				testassert.NoError(t, err)
			}
			{
				err := f5.h.DeleteSession(ctx, source)
				testassert.NoError(t, err)
			}
			res, err := f5.h.Fork(ctx, forkReq)
			testassert.NoError(t, err)
			testassert.Require(t, res.Session.Identity.SessionID == dest, "retry after source deletion = %+v, want the first destination", res)
		})

		t.Run("other reuse of the ID is invalid", func(t *testing.T) {
			f6 := newPublicFixture(t, store, newScriptModel(), nil)
			defer f6.close()
			{
				_, err := f6.h.Fork(ctx, harness.ForkRequest{
					SourceSessionID: other,
					BoundaryEntryID: otherBoundary,
					OperationID:     "fork-b",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
				})
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "fork reusing a root Session's operation ID = err %v, want ErrInvalid", err)
			}
		})

		t.Run("corrupt located owner returns corruption", func(t *testing.T) {
			f7 := newPublicFixture(t, store, newScriptModel(), nil)
			defer f7.close()
			res, err := f7.h.Fork(ctx, harness.ForkRequest{
				SourceSessionID: other,
				BoundaryEntryID: otherBoundary,
				OperationID:     "fork-c",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.NoError(t, err)
			{
				err := converge(t, f7)
				testassert.NoError(t, err)
			}
			// corrupt the destination register directly through storage
			reg := sessionRegister(t, store, res.Session.Identity.SessionID)
			var obj map[string]json.RawMessage
			{
				err := json.Unmarshal(reg.Payload, &obj)
				testassert.NoError(t, err)
			}
			var state map[string]json.RawMessage
			{
				err := json.Unmarshal(obj["state"], &state)
				testassert.NoError(t, err)
			}
			state["lifecycle"] = json.RawMessage(`"bogus"`)
			newState, err := json.Marshal(state)
			testassert.NoError(t, err)
			obj["state"] = newState
			bad, err := json.Marshal(obj)
			testassert.NoError(t, err)
			key := harness.RegisterKey{SessionID: res.Session.Identity.SessionID, Kind: harness.RegisterSession}
			f8 := newPublicFixture(t, store, newScriptModel(), nil)
			defer f8.close()
			// the affected destination is already materialized in the retry
			// Harness before the corruption is reported
			{
				_, err := f8.h.ReadSessionHeader(ctx, res.Session.Identity.SessionID)
				testassert.NoError(t, err)
			}
			{
				err := store.Transact(ctx, func(tx harness.Transaction) error {
					_, err := tx.ReplaceRegister(key, reg.Revision, bad)
					return err
				})
				testassert.NoError(t, err)
			}
			{
				_, err := f8.h.Fork(ctx, harness.ForkRequest{
					SourceSessionID: other,
					BoundaryEntryID: otherBoundary,
					OperationID:     "fork-c",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
				})
				testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "retry over a corrupt located owner = err %v, want the corruption error", err)
			}
			// the affected cached Session is sticky-corrupt while valid siblings remain usable
			{
				_, err := f8.h.ReadSessionHeader(ctx, res.Session.Identity.SessionID)
				testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "cached read of the corrupt located owner = err %v, want the sticky corruption error", err)
			}
			{
				_, err := f8.h.ReadSessionHeader(ctx, other)
				testassert.NoError(t, err)
			}
		})

		t.Run("corrupt sibling before the owner is skipped", func(t *testing.T) {
			insertRawRegister(t, store, harness.RegisterKey{SessionID: forkSiblingID, Kind: harness.RegisterSession}, rawSessionRegister(forkSiblingID, ""))
			wrapped := &corruptReadStore{Storage: store, session: forkSiblingID}
			f9 := newPublicFixture(t, wrapped, newScriptModel(), nil)
			defer f9.close()
			// the affected sibling is already materialized in this Harness
			// before the corruption is reported
			{
				_, err := f9.h.ReadSessionHeader(ctx, forkSiblingID)
				testassert.NoError(t, err)
			}
			res, err := f9.h.Fork(ctx, forkReq)
			testassert.NoError(t, err)
			testassert.Require(t, res.Operation.Admission.OperationID == "fork-a", "retry = %+v, want the existing destination", res)
			// the skipped corruption sticks to the cached Session while valid siblings remain usable
			{
				_, err := f9.h.ReadSessionHeader(ctx, forkSiblingID)
				testassert.Require(t, errors.Is(err, harness.ErrCorrupt), "cached read of the corrupt sibling = err %v, want the sticky corruption error", err)
			}
			{
				_, err := f9.h.ReadSessionHeader(ctx, other)
				testassert.NoError(t, err)
			}
		})
	})
}

// TestPublicForkFirstWriterRace proves the first-writer resolution axis: a
// fork that loses a concurrent same-ID insertion race reruns neither
// preparation nor the transaction — the same exact lookup runs once and
// returns the matching first destination, a different lineage/request use of
// the ID is invalid, and an unreadable owner preserves the original storage
// conflict.
func TestPublicForkFirstWriterRace(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		source := seedIdleSource(t, store, "op-1")
		boundary := forkEntryOf(t, store, source, harness.EntryInput, "op-1").ID

		t.Run("matching first destination is returned", func(t *testing.T) {
			race := newPublicFixture(t, store, newScriptModel(), nil)
			defer race.close()
			decoyCh := make(chan string, 1)
			race.prepareHook = func(int, harness.PreparationRequest) (harness.PreparedExecution, error) {
				decoyCh <- seedForkDecoy(t, store, source, boundary, "race-1", true) // the concurrent winner commits during preparation
				return racePrepared(race), nil
			}
			res, err := race.h.Fork(ctx, harness.ForkRequest{
				SourceSessionID: source,
				BoundaryEntryID: boundary,
				OperationID:     "race-1",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.NoError(t, err)
			decoy := <-decoyCh
			testassert.Require(t, res.Session.Identity.SessionID == decoy && res.Session.Identity.SourceSessionID == source && res.Operation.Admission.OperationID == "race-1", "race resolution = %+v, want the first destination session %q", res, decoy)
			{
				calls := race.preparationCalls()
				testassert.Equal(t, len(calls), 1)
			}
			{
				got := race.opens.Load()
				testassert.Equal(t, got, 0)
			}
			assertSessionIDs(t, store, decoy, source) // the losing transaction published nothing
		})

		t.Run("different lineage reuse is invalid", func(t *testing.T) {
			race := newPublicFixture(t, store, newScriptModel(), nil)
			defer race.close()
			race.prepareHook = func(int, harness.PreparationRequest) (harness.PreparedExecution, error) {
				seedForkDecoy(t, store, source, boundary, "race-2", false) // a root Session wins the ID
				return racePrepared(race), nil
			}
			{
				_, err := race.h.Fork(ctx, harness.ForkRequest{
					SourceSessionID: source,
					BoundaryEntryID: boundary,
					OperationID:     "race-2",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
				})
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "fork losing to a different use of the ID = err %v, want ErrInvalid", err)
			}
			{
				got := race.opens.Load()
				testassert.Equal(t, got, 0)
			}
		})

		t.Run("unreadable owner preserves the conflict", func(t *testing.T) {
			race := newPublicFixture(t, store, newScriptModel(), nil)
			defer race.close()
			race.prepareHook = func(int, harness.PreparationRequest) (harness.PreparedExecution, error) {
				sid, entryID := newRawSessionID(t), newRawSessionID(t)
				insertRawRegister(t, store, harness.RegisterKey{SessionID: sid, Kind: harness.RegisterSession}, `{}`) // the owner's Session is unreadable
				insertRawEntry(t, store, sid, entryID, "race-3", harness.EntryInput, rawInputEntryPayload(sid, entryID, "race-3"))
				insertRawRegister(t, store, harness.RegisterKey{SessionID: sid, Kind: harness.RegisterOperation, OperationID: "race-3"}, rawRunningOperationPayload(sid, "race-3", entryID))
				return racePrepared(race), nil
			}
			{
				_, err := race.h.Fork(ctx, harness.ForkRequest{
					SourceSessionID: source,
					BoundaryEntryID: boundary,
					OperationID:     "race-3",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
				})
				testassert.Require(t, errors.Is(err, harness.ErrConflict), "fork losing to an unreadable owner = err %v, want the original storage conflict", err)
			}
		})
	})
}

// TestPublicForkSourceRevisionRace proves the source-revalidation failure
// point: a foreign writer that changes the source Session register between
// materialization and the fork transaction makes the fork fail with the
// conflict class, and nothing is published.
func TestPublicForkSourceRevisionRace(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		source := seedIdleSource(t, store, "op-1")
		boundary := forkEntryOf(t, store, source, harness.EntryInput, "op-1").ID
		before := snapshotSession(t, store, source)

		race := newPublicFixture(t, store, newScriptModel(), nil)
		defer race.close()
		race.prepareHook = func(int, harness.PreparationRequest) (harness.PreparedExecution, error) {
			// a foreign writer changes the source under the reservation
			reg := sessionRegister(t, store, source)
			edited := editStateObject(t, reg.Payload, func(state map[string]json.RawMessage) {
				state["current_agent_type"] = json.RawMessage(`"reviewer"`)
			})
			if err := store.Transact(ctx, func(tx harness.Transaction) error {
				_, err := tx.ReplaceRegister(harness.RegisterKey{SessionID: source, Kind: harness.RegisterSession}, reg.Revision, edited)
				return err
			}); err != nil {
				t.Errorf("foreign change: %v", err)
				return harness.PreparedExecution{}, err
			}
			return racePrepared(race), nil
		}
		{
			_, err := race.h.Fork(ctx, harness.ForkRequest{
				SourceSessionID: source,
				BoundaryEntryID: boundary,
				OperationID:     "race-1",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.Require(t, errors.Is(err, harness.ErrConflict), "fork over a concurrently changed source = err %v, want ErrConflict", err)
		}
		{
			got := race.opens.Load()
			testassert.Equal(t, got, 0)
		}
		assertSessionIDs(t, store, source) // nothing was published
		after := sessionRegister(t, store, source)
		testassert.Require(t, after.Revision == snapRegisterOf(&before, harness.RegisterSession, "").Revision+1 && bytes.Equal(after.Payload, mustForeignPayload(t, before)), "fork disturbed the foreign change: register = %+v", after)
	})
}

// mustForeignPayload returns the pre-fork register payload with the foreign
// writer's Agent-type change applied.
func mustForeignPayload(t *testing.T, snap sessionSnapshot) json.RawMessage {
	t.Helper()
	for i := range snap.registers {
		if snap.registers[i].Key.Kind == harness.RegisterSession {
			return editStateObject(t, snap.registers[i].Payload, func(state map[string]json.RawMessage) {
				state["current_agent_type"] = json.RawMessage(`"reviewer"`)
			})
		}
	}
	t.Fatalf("no session register in the snapshot")
	return nil
}

// TestPublicForkIndependentSourceDeletion proves the independent-lifecycle
// axis: lineage is informational only — deleting the source leaves the fork
// readable and admissible, and forking from the deleted source resolves to
// the not-found class.
func TestPublicForkIndependentSourceDeletion(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		source := seedIdleSource(t, store, "op-1")
		boundary := forkEntryOf(t, store, source, harness.EntryInput, "op-1").ID

		f2 := newPublicFixture(t, store, newScriptModel(), nil)
		defer f2.close()
		res, err := f2.h.Fork(ctx, harness.ForkRequest{
			SourceSessionID: source,
			BoundaryEntryID: boundary,
			OperationID:     "fork-1",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
		})
		testassert.NoError(t, err)
		dest := res.Session.Identity.SessionID
		{
			err := converge(t, f2)
			testassert.NoError(t, err)
		}

		// the source is archived and deleted; the fork stays usable
		destScript := newScriptModel()
		f3 := newPublicFixture(t, store, destScript, nil)
		defer f3.close()
		{
			_, err := f3.h.ArchiveSession(ctx, source)
			testassert.NoError(t, err)
		}
		{
			err := f3.h.DeleteSession(ctx, source)
			testassert.NoError(t, err)
		}
		{
			_, err := f3.h.ReadSessionHeader(ctx, dest)
			testassert.NoError(t, err)
		}
		{
			_, err := f3.h.ReadOperation(ctx, dest, "fork-1")
			testassert.NoError(t, err)
		}
		{
			_, err := submit(t, f3.h, dest, "fork-2", harness.MessageModeRegular, "after source loss")
			testassert.NoError(t, err)
		}
		<-destScript.arrived
		{
			_, err := submit(t, f3.h, dest, "fork-4", harness.MessageModeQueued, "settle barrier")
			testassert.NoError(t, err)
		}
		<-f3.prepare // the drain admitted fork-4: fork-2's terminal committed
		<-destScript.arrived
		{
			err := converge(t, f3)
			testassert.NoError(t, err)
		}
		rec, err := f3.h.ReadOperation(ctx, dest, "fork-2")
		testassert.Require(t, err == nil && rec.State.Status == harness.OperationSuccess, "fork admission after source deletion = %+v err %v, want success", rec, err)
		f4 := newPublicFixture(t, store, newScriptModel(), nil)
		defer f4.close()
		{
			_, err := f4.h.Fork(ctx, harness.ForkRequest{
				SourceSessionID: source,
				BoundaryEntryID: boundary,
				OperationID:     "fork-3",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "fork from a deleted source = err %v, want ErrNotFound", err)
		}
	})
}

// TestPublicForkRollbackOnTransactionFailure proves the fork's failure
// points: a post-mutation transaction failure rolls the whole fork back —
// the destination exists nowhere in storage and the source is byte-identical.
func TestPublicForkRollbackOnTransactionFailure(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		source := seedIdleSource(t, store, "op-1")
		boundary := forkEntryOf(t, store, source, harness.EntryInput, "op-1").ID
		before := snapshotSession(t, store, source)

		probe := &rollbackProbeStore{Storage: store, failInsert: true, insertsUntilFail: 2} // the destination Operation-register insert fails after mutating
		race := newPublicFixture(t, probe, newScriptModel(), nil)
		defer race.close()
		{
			_, err := race.h.Fork(ctx, harness.ForkRequest{
				SourceSessionID: source,
				BoundaryEntryID: boundary,
				OperationID:     "fork-1",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
			})
			testassert.Require(t, errors.Is(err, errInjectedRollback), "fork with injected post-mutation failure = err %v, want the injected rollback", err)
		}
		assertSessionIDs(t, store, source) // the rolled-back fork left no destination
		assertForkSourceUnchanged(t, store, source, before)
	})
}

// TestPublicCompositionProductionIsolation finalizes the Package-authority row
// from external composition: the inactive public Harness composes from a fully
// external caller through the public API only — caller-owned storage, the
// landed agent effect contracts, and no other wiring. One complete turn over
// each store leaves the caller's working directory untouched, so durable state
// exists only in the storage the caller supplied.
func TestPublicCompositionProductionIsolation(t *testing.T) {
	state := t.TempDir()
	t.Chdir(state)
	eachStore(t, func(t *testing.T, store harness.Storage) {
		script := newScriptModel()
		script.gate = make(chan struct{})
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		sessionID := createSession(t, f.h)
		res, err := submit(t, f.h, sessionID, "iso-1", harness.MessageModeRegular, "isolate")
		testassert.Require(t, err == nil && res.Disposition == harness.DispositionAdmitted, "Submit = %+v err %v, want admitted", res, err)
		<-script.arrived // parked at the model boundary: the Session is active
		{
			_, err := submit(t, f.h, sessionID, "iso-2", harness.MessageModeQueued, "drain")
			testassert.NoError(t, err)
		}
		script.releaseGate()
		<-script.arrived // the drain admitted iso-2: iso-1's terminal success committed
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}
		op, err := f.h.ReadOperation(context.Background(), sessionID, "iso-1")
		testassert.Require(t, err == nil && op.State.Status == harness.OperationSuccess, "composed isolation turn = %+v err %v, want success", op, err)
		touched, err := os.ReadDir(state)
		testassert.NoError(t, err)
		testassert.Require(t, len(touched) == 0, "composed inactive Harness derived production state in the caller's working directory: %v", touched)
	})
}

// TestPublicExecutionResourceLifetime proves the shared post-admission
// resource lifetime rows through public operations on both stores: each
// admitted execution — root, queued-drain, and Fork alike — opens its effects
// exactly once after its commit over the committed admission carrying the
// prepared capture, closes them exactly once after its terminal settlement,
// and closes before the successor opens; a plain successful turn's closer
// observes its own durable terminal.
func TestPublicExecutionResourceLifetime(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		events := make(chan string, 32)
		var (
			openMu sync.Mutex
			opens  []harness.OperationAdmission
		)
		script := newScriptModel() // every turn completes with the default ready settlement
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		f.prepareHook = func(int, harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: publicCapture(),
				Open: func(_ context.Context, adm harness.OperationAdmission) (harness.Execution, error) {
					openMu.Lock()
					opens = append(opens, adm)
					openMu.Unlock()
					events <- "open:" + adm.OperationID
					return harness.Execution{
						Model:         script.effect,
						CompactModel:  script.effect,
						NormalizeTool: publicNormalize,
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"}}}
						},
						Close: func() error {
							rec, err := f.h.ReadOperation(ctx, adm.SessionID, adm.OperationID)
							if err != nil {
								t.Errorf("read operation from its own closer: %v", err)
								return nil
							}
							events <- fmt.Sprintf("close:%s:%s", adm.OperationID, rec.State.Status)
							return nil
						},
					}, nil
				},
			}, nil
		}
		source := createSession(t, f.h)

		// nextEvent consumes one lifetime event, failing on any timeout or
		// reordering instead of hanging the suite.
		nextEvent := func(want string) {
			t.Helper()
			select {
			case got := <-events:
				testassert.Equal(t, got, want)
			case <-time.After(30 * time.Second):
				t.Fatalf("timed out waiting for lifetime event %q", want)
			}
		}

		{
			_, err := submit(t, f.h, source, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		nextEvent("open:op-1") // the root execution opens after its commit
		{
			_, err := submit(t, f.h, source, "op-2", harness.MessageModeQueued, "queued")
			testassert.NoError(t, err)
		}
		nextEvent("close:op-1:success") // closed once, with its own terminal settlement already durable
		nextEvent("open:op-2")          // the drain's successor opens only after that close
		nextEvent("close:op-2:success")

		// Fork shares the same lifetime: the destination execution opens
		// after the fork commit and closes after its own terminal.
		boundary := forkEntryOf(t, store, source, harness.EntryInput, "op-1").ID
		res, err := f.h.Fork(ctx, harness.ForkRequest{
			SourceSessionID: source,
			BoundaryEntryID: boundary,
			OperationID:     "fork-op",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
		})
		testassert.NoError(t, err)
		nextEvent("open:fork-op")
		nextEvent("close:fork-op:success")
		{
			err := converge(t, f)
			testassert.NoError(t, err)
		}

		openMu.Lock()
		defer openMu.Unlock()
		testassert.Equal(t, len(opens), 3)
		for i, adm := range opens {
			testassert.Require(t, reflect.DeepEqual(adm.Execution, publicCapture()), "open %d carries capture %+v, want the prepared capture", i, adm.Execution)
		}
		testassert.Require(t, opens[0].SessionID == source && opens[1].SessionID == source, "root/queued opens name sessions %q and %q, want the source", opens[0].SessionID, opens[1].SessionID)
		testassert.Equal(t, opens[2].SessionID, res.Session.Identity.SessionID)
	})
}

// TestPublicRecoverHookShapes proves the two hook recovery transitions over
// memory and temporary SQLite using the same public storage fixtures as the
// plain recovery shapes: an active hook effect's recovery atomically commits
// its reserved interrupted hook result, the pending calls' interrupted tool
// results, and the Operation/Session settlement in one transaction — a failed
// recovery publication rolls the whole transaction back — while a running
// quiet gap preserves settled hook evidence and commits the interrupted tool
// result without replaying the hook. Recovery runs before any Harness exists;
// a Harness constructed afterwards materializes the repaired state, and a
// second recovery run writes nothing.
func TestPublicRecoverHookShapes(t *testing.T) {
	t.Run("active hook effect settles its reserved interrupted result", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			sessionID, hookReserved, pending := seedRunningOperationShape(t, store, "hook", "op-1", []string{"call-1"})
			state := recoverOpStateAt(t, store, sessionID, "op-1")
			testassert.Require(t, state.ActiveEffect != nil && state.ActiveEffect.Kind == "hook" && state.ActiveEffect.ResultEntryID == hookReserved && state.ActiveEffect.HookID == "cap.hook.one" && state.ActiveEffect.ToolCallID == "call-1", "seeded operation = %+v, want the committed hook effect intent", state)

			pre := recoverEntries(t, store, sessionID)
			{
				err := harness.Recover(ctx, store)
				testassert.NoError(t, err)
			}
			// Recovery adds its reserved hook result before the ordinary
			// interrupted-tool, signal and settlement tail.
			entries := recoverEntries(t, store, sessionID)
			testassert.Require(t, len(entries) > len(pre), "%v", "recovery committed no hook result")
			hook := entries[len(pre)]
			testassert.Require(t, hook.Kind == harness.EntryHookResult && hook.ID == hookReserved, "recovery commit %s is %s, want the interrupted hook result under the reserved identity %s", hook.ID, hook.Kind, hookReserved)
			var hookWire struct {
				HookID     string `json:"hook_id"`
				ToolCallID string `json:"tool_call_id"`
				Status     string `json:"status"`
				Error      string `json:"error"`
			}
			{
				err := json.Unmarshal(hook.Payload, &hookWire)
				testassert.NoError(t, err)
			}
			testassert.Require(t, hookWire.HookID == "cap.hook.one" && hookWire.ToolCallID == "call-1" && hookWire.Status == "interrupted" && hookWire.Error == recoveredInterruptionDetail, "interrupted hook result = %+v, want the recovery-settled interrupted evidence", hookWire)
			assertRecoveredOperation(t, store, sessionID, "op-1", append(pre, hook), "", pending)
			assertRecoverIdempotent(t, store, sessionID, "op-1")

			h := harnessOver(t, store)
			{
				_, err := h.ReadOperation(ctx, sessionID, "op-1")
				testassert.NoError(t, err)
			}
		})
	})

	t.Run("quiet gap preserves settled hook evidence without replay", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			sessionID, _, pending := seedRunningOperationShape(t, store, "", "op-1", []string{"call-1"})
			hookID := newRawSessionID(t)
			insertRawEntry(t, store, sessionID, hookID, "op-1", harness.EntryHookResult,
				rawFailedHookResultPayload(sessionID, hookID, "op-1", "cap.hook.one", "call-1", "error", "hook broke"))

			pre := recoverEntries(t, store, sessionID)
			{
				err := harness.Recover(ctx, store)
				testassert.NoError(t, err)
			}
			assertRecoveredOperation(t, store, sessionID, "op-1", pre, "", pending)
			assertRecoverIdempotent(t, store, sessionID, "op-1")

			h := harnessOver(t, store)
			{
				_, err := h.ReadOperation(ctx, sessionID, "op-1")
				testassert.NoError(t, err)
			}
		})
	})

	t.Run("failed recovery publication rolls back", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			targetID, hookReserved, _ := seedRunningOperationShape(t, store, "hook", "op-1", []string{"call-1"})
			before := snapshotSession(t, store, targetID)

			// The probe performs the target's first recovery register
			// replacement for real, then fails the transaction: every recovery
			// commit must roll back together.
			probed := &rollbackProbeStore{Storage: store, failReplace: true}
			{
				err := harness.Recover(ctx, probed)
				testassert.Require(t, errors.Is(err, errInjectedRollback), "recover with the injected publication failure = %v, want the injected rollback", err)
			}
			assertSessionUnchanged(t, store, targetID, before)

			{
				err := harness.Recover(ctx, store)
				testassert.NoError(t, err)
			}
			state := recoverOpStateAt(t, store, targetID, "op-1")
			testassert.Require(t, state.Status == "interruption" && state.ActiveEffect == nil && state.Terminal != nil, "recovered state = %+v, want the terminal interruption", state)
			entries := recoverEntries(t, store, targetID)
			hookResults := 0
			for _, entry := range entries {
				if entry.Kind == harness.EntryHookResult {
					hookResults++
					testassert.Require(t, entry.ID == hookReserved, "hook result = %+v, want it under the reserved identity", entry)
				}
			}
			testassert.Equal(t, hookResults, 1)
			assertRecoverIdempotent(t, store, targetID, "op-1")
		})
	})
}

// TestPublicForkAcrossCompaction proves the complete fork-across-compaction
// copy oracle over public operations on both stores: a Session whose prefix
// carries a committed compaction entry forks with fresh copied identities, an
// in-prefix boundary rewritten to the mapped copy, no inherited Operation
// ownership or usage on the copied compaction entry, the source's
// configuration revision retained, the copied prefix in the source's order
// excluding settlements, zero inherited usage on the destination, the
// destination's projection recreated at the copied summary, and the source
// unchanged.
func TestPublicForkAcrossCompaction(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		conversation := newScriptModel(publicTurn(), publicTurn(), publicTurn()) // op-1, op-2, fork-1
		compact := newScriptModel(summaryAttempt("summary one"))
		f := newCompactManualFixture(t, store, conversation, compact)
		defer f.close()
		source := createSession(t, f.h)

		{
			_, err := submit(t, f.h, source, "op-1", harness.MessageModeRegular, "first question")
			testassert.NoError(t, err)
		}
		awaitTerminal(t, f.h, source, "op-1")
		compactWhenIdle(t, f.h, source, "c-1")
		{
			rec := awaitTerminal(t, f.h, source, "c-1")
			testassert.Require(t, rec.State.Status == harness.OperationSuccess, "manual compact = %+v, want success", rec.State)
		}
		{
			_, err := submit(t, f.h, source, "op-2", harness.MessageModeRegular, "later question")
			testassert.NoError(t, err)
		}
		awaitSettled(t, f.h, source, "op-2") // submit may buffer during the compact run's retirement; admission is the rendezvous

		boundary := forkEntryOf(t, store, source, harness.EntryInput, "op-2").ID
		before := snapshotSession(t, store, source)
		// The source's committed compaction entry: the values the fork must
		// copy with fresh identities and no ownership.
		var sourceComp *harness.Entry
		for i := range before.entries {
			if before.entries[i].Kind == harness.EntryCompaction {
				sourceComp = &before.entries[i]
			}
		}
		testassert.Require(t, sourceComp != nil, "no compaction entry in the source prefix")
		// The wire compaction entry carries its model as the string
		// "provider/model" form; the copy observes the identity members only.
		type compactionWire struct {
			OperationID           string              `json:"operation_id"`
			Summary               string              `json:"summary"`
			BoundaryEntryID       string              `json:"boundary_entry_id"`
			ConfigurationRevision string              `json:"configuration_revision"`
			Usage                 *harness.UsageCount `json:"usage"`
		}
		var sourceWire compactionWire
		{
			err := json.Unmarshal(sourceComp.Payload, &sourceWire)
			testassert.NoError(t, err)
		}
		testassert.Require(t, sourceWire.OperationID == "c-1" && sourceWire.Summary == "summary one" && sourceWire.Usage != nil, "source compaction entry = %+v, want the c-1-owned committed summary with usage", sourceWire)

		res, err := f.h.Fork(ctx, harness.ForkRequest{
			SourceSessionID: source,
			BoundaryEntryID: boundary,
			OperationID:     "fork-1",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "fork input"}},
		})
		testassert.NoError(t, err)
		dest := res.Session.Identity.SessionID
		testassert.Require(t, dest != source, "fork session identity = %q, want one fresh identity", dest)
		testassert.Require(t, len(res.Session.State.Usage.ByModel) == 0, "fork session usage = %+v, want zero inherited usage", res.Session.State.Usage)
		awaitTerminal(t, f.h, dest, "fork-1")

		destEntries := recoverEntries(t, store, dest)
		// The copied prefix maps one-to-one onto the source's strict-before-
		// boundary prefix, excluding the kinds the copy drops (Operation
		// settlements), and the fork's own input follows.
		var expectedCopied []harness.Entry
		for _, entry := range before.entries {
			if entry.ID == boundary {
				break
			}
			switch entry.Kind {
			case harness.EntryInput, harness.EntryAssistant, harness.EntryToolResult, harness.EntrySignal, harness.EntryCompaction:
				expectedCopied = append(expectedCopied, entry)
			}
		}
		if len(destEntries) <= len(expectedCopied) || destEntries[len(expectedCopied)].Kind != harness.EntryInput {
			t.Fatalf("destination entries = %+v, want the copied prefix followed by the fork's own input", kindsOf(destEntries))
		}
		idMap := map[string]string{}
		for i, entry := range expectedCopied {
			testassert.Require(t, destEntries[i].Kind == entry.Kind, "copied prefix %d = %s, want the source %s's %s", i, destEntries[i].Kind, entry.ID, entry.Kind)
			idMap[entry.ID] = destEntries[i].ID
		}
		// The one copied compaction entry: fresh identity, no operation
		// ownership, no inherited usage, the boundary rewritten to the mapped
		// copy, the revision retained, the summary verbatim.
		var copied *harness.Entry
		for i := range destEntries {
			if destEntries[i].Kind == harness.EntryCompaction {
				testassert.Require(t, copied == nil, "destination compaction entries: more than one copied")
				copied = &destEntries[i]
			}
		}
		testassert.Require(t, copied != nil, "no compaction entry copied into the destination")
		testassert.Require(t, copied.ID != sourceComp.ID && copied.OperationID == "", "copied compaction entry = %q/%q, want a fresh identity with no operation ownership", copied.ID, copied.OperationID)
		var copiedWire compactionWire
		{
			err := json.Unmarshal(copied.Payload, &copiedWire)
			testassert.NoError(t, err)
		}
		testassert.Require(t, copiedWire.OperationID == "" && copiedWire.Usage == nil && copiedWire.Summary == "summary one", "got %v, want %v; copied compaction entry usage = %+v, want none; got %v, want %v", copiedWire.OperationID, "", copiedWire.Usage, copiedWire.Summary, "summary one")
		expectedBoundary, ok := idMap[sourceWire.BoundaryEntryID]
		testassert.Require(t, ok, "the source boundary %q is not inside the copied prefix", sourceWire.BoundaryEntryID)
		testassert.Require(t, copiedWire.BoundaryEntryID == expectedBoundary && copiedWire.ConfigurationRevision == sourceWire.ConfigurationRevision, "got %v, want %v; got %v, want %v", copiedWire.BoundaryEntryID, expectedBoundary, copiedWire.ConfigurationRevision, sourceWire.ConfigurationRevision)
		// The destination projection is recreated at the copied summary and
		// inherits none of the source's usage: the source's compact-model
		// piece counts never reach the destination, whose only totals are its
		// own fork turn's conversation counts.
		destReg, err := f.store.ReadRegister(ctx, harness.RegisterKey{SessionID: dest, Kind: harness.RegisterSession})
		testassert.NoError(t, err)
		var destState struct {
			State struct {
				CompactionEntryID string `json:"compaction_entry_id"`
			} `json:"state"`
		}
		{
			err := json.Unmarshal(destReg.Payload, &destState)
			testassert.NoError(t, err)
		}
		testassert.Equal(t, destState.State.CompactionEntryID, copied.ID)
		destView, err := f.h.ReadSessionView(ctx, dest)
		testassert.NoError(t, err)
		for _, mu := range destView.TotalUsage.ByModel {
			testassert.Require(t, mu.Model != compactModelRef, "destination usage = %+v, want no inherited compact-model counts", destView.TotalUsage)
		}
		// The fork's own turn projected at the copied summary.
		reqs := conversation.seen()
		testassert.Require(t, len(reqs) != 0, "the fork execution never reached its model boundary")
		last := texts(reqs[len(reqs)-1])
		testassert.Require(t, len(last) == 3 && strings.Contains(last[1], "[Previous conversation summary]") && strings.Contains(last[1], "summary one") && last[2] == "fork input", "fork projection = %q, want the system prompt, the copied summary, and the fork input", last)
		// The source is unchanged.
		assertForkSourceUnchanged(t, store, source, before)
	})
}

// kindsOf renders one entry list's kinds, for failure messages.
func kindsOf(entries []harness.Entry) []harness.EntryKind {
	out := make([]harness.EntryKind, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Kind)
	}
	return out
}

// rawSessionRegisterWithCompaction builds one open root Session register
// payload that also carries the current compaction projection.
func rawSessionRegisterWithCompaction(sessionID, currentOp, compactionEntryID string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf(
		`{"identity":{"session_id":%q,"workspace":"/tmp/works","created_at":%q},`+
			`"state":{"lifecycle":"open","current_agent_type":"coder","current_operation_id":%q,`+
			`"compaction_entry_id":%q,"usage":{"by_model":[]},"last_activity":%q}}`,
		sessionID, now, currentOp, compactionEntryID, now)
}

// rawCompactAdmission builds one compact-kind admission payload: the compact
// request shape carries no admitted input entry.
func rawCompactAdmission(sessionID, operationID string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf(
		`{"session_id":%q,"operation_id":%q,"request_kind":"compact",`+
			`"agent_type":"coder",`+
			`"execution":{"configuration_revision":"rev-1","model":{"provider":"prov","model":"gpt-x"},`+
			`"context_window":4096,"output_reserve":2048,`+
			`"system_prompt":"system","tools":[%s],"readonly":false,"write_dir":"",`+
			`"compact":{"model":{"provider":"cprov","model":"compact-x"},"system_prompt":"summarize"}},"admitted_at":%q}`,
		sessionID, operationID, rawToolDefinition, now)
}

// rawCompactionEntryPayload builds one committed compaction entry payload.
func rawCompactionEntryPayload(sessionID, entryID, operationID, summary, boundaryID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"summary":%q,"boundary_entry_id":%q,`+
			`"model":{"provider":"prov","model":"gpt-x"},"configuration_revision":"rev-1"}`,
		sessionID, entryID, operationID, summary, boundaryID)
}

// rawSettledSuccessOperation seeds one settled success message Operation —
// its input and assistant entries plus the settlement entry in order, and the
// register with the matching terminal section — returning the assistant
// entry's identity, with both entries carrying the given texts.
func rawSettledSuccessOperation(t *testing.T, store harness.Storage, sessionID, operationID, inputText, assistantText string) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	inputID, assistantID, settlementID := newRawSessionID(t), newRawSessionID(t), newRawSessionID(t)
	insertRawEntry(t, store, sessionID, inputID, operationID, harness.EntryInput,
		fmt.Sprintf(`{"session_id":%q,"entry_id":%q,"operation_id":%q,"origin":"user","content":[{"kind":"text","text":%q}]}`,
			sessionID, inputID, operationID, inputText))
	insertRawEntry(t, store, sessionID, assistantID, operationID, harness.EntryAssistant,
		fmt.Sprintf(`{"session_id":%q,"entry_id":%q,"operation_id":%q,"status":"completed",`+
			`"source":{"provider":"prov","model":"gpt-x"},"content":[{"kind":"text","text":%q}],"tool_calls":[]}`,
			sessionID, assistantID, operationID, assistantText))
	insertRawEntry(t, store, sessionID, settlementID, operationID, harness.EntryOperationSettlement,
		fmt.Sprintf(`{"session_id":%q,"entry_id":%q,"operation_id":%q,"status":"success"}`, sessionID, settlementID, operationID))
	insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID},
		fmt.Sprintf(`{"admission":%s,"state":{"status":"success","started_at":%q,"settled_at":%q,"pending_tool_calls":[],"usage":{"by_model":[]},"terminal":{"settlement_entry":{"session_id":%q,"entry_id":%q}}}}`,
			rawAdmission(sessionID, operationID, inputID), now, now, sessionID, settlementID))
	return assistantID
}

// TestPublicRecoverRunningCompactOperation proves the compact recovery row:
// a running compact Operation holding a live model-effect intent, over a
// prior committed compaction and settled post-compaction turns, settles as
// the runtime-loss interruption through the quiescent recovery — the previous
// projection stays current, no effect replays, the seeded entries stay
// byte-identical with only the signal and settlement committed, the re-run
// writes nothing, and the next admission over the repaired state projects the
// prior summary and post-boundary entries.
func TestPublicRecoverRunningCompactOperation(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		sessionID := newRawSessionID(t)
		compactionID := newRawSessionID(t)

		insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession},
			rawSessionRegisterWithCompaction(sessionID, "compact-op", compactionID))
		assistant1 := rawSettledSuccessOperation(t, store, sessionID, "op-1", "hello", "done")
		insertRawEntry(t, store, sessionID, compactionID, "op-1", harness.EntryCompaction,
			rawCompactionEntryPayload(sessionID, compactionID, "op-1", "seeded summary", assistant1))
		rawSettledSuccessOperation(t, store, sessionID, "op-2", "after the summary", "later answer")
		reserved := newRawSessionID(t)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		insertRawRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "compact-op"},
			fmt.Sprintf(`{"admission":%s,"state":{"status":"running","started_at":%q,`+
				`"active_effect":{"kind":"model","result_entry_id":%q},"pending_tool_calls":[],"usage":{"by_model":[]}}}`,
				rawCompactAdmission(sessionID, "compact-op"), now, reserved))

		// The seeded shape: a running compact request with a live model intent.
		state := recoverOpStateAt(t, store, sessionID, "compact-op")
		testassert.Require(t, state.Status == "running" && state.ActiveEffect != nil && state.ActiveEffect.Kind == "model" && state.ActiveEffect.ResultEntryID == reserved, "seeded compact operation = %+v, want the running live model intent", state)

		pre := recoverEntries(t, store, sessionID)
		// quiescent: before any live Harness
		{
			err := harness.Recover(ctx, store)
			testassert.NoError(t, err)
		}
		// The compact Operation settles as the runtime-loss interruption with
		// no effect replay: the settlement consumes the reserved identity and
		// the committed tail is exactly the signal and the settlement.
		assertRecoveredOperation(t, store, sessionID, "compact-op", pre, reserved, nil)
		assertRecoverIdempotent(t, store, sessionID, "compact-op")

		// The previous projection stays current: the seeded compaction entry
		// is the only one, the session's compaction_entry_id still names it,
		// and the current Operation cleared.
		entries := recoverEntries(t, store, sessionID)
		compactions := 0
		for _, entry := range entries {
			if entry.Kind == harness.EntryCompaction {
				compactions++
				testassert.Equal(t, entry.ID, compactionID)
			}
		}
		testassert.Equal(t, compactions, 1)
		sessReg, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
		testassert.NoError(t, err)
		var sessWire struct {
			State struct {
				CurrentOperationID string `json:"current_operation_id"`
				CompactionEntryID  string `json:"compaction_entry_id"`
			} `json:"state"`
		}
		{
			err := json.Unmarshal(sessReg.Payload, &sessWire)
			testassert.NoError(t, err)
		}
		testassert.Require(t, sessWire.State.CurrentOperationID == "" && sessWire.State.CompactionEntryID == compactionID, "recovered session state = %+v, want the cleared current Operation and the previous projection", sessWire.State)

		// The next admission over the repaired state projects the prior
		// summary, the post-boundary entries, and the interruption signal —
		// through the real public path, with no replayed effect.
		script := newScriptModel(publicTurn())
		f := newPublicFixture(t, store, script, nil)
		defer f.close()
		{
			_, err := submit(t, f.h, sessionID, "op-next", harness.MessageModeRegular, "next question")
			testassert.NoError(t, err)
		}
		awaitTerminal(t, f.h, sessionID, "op-next")
		reqs := script.seen()
		testassert.Equal(t, len(reqs), 1)
		got := texts(reqs[0])
		want := []string{
			"system",
			"[Previous conversation summary]\n\nseeded summary\n\n[End of summary. Continue from here.]",
			"after the summary",
			"later answer",
			"<system-signal>Operation interrupted.</system-signal>",
			"next question",
		}
		testassert.Require(t, len(got) == len(want), "next admission projection = %q, want %q", got, want)
		for i := range want {
			testassert.Require(t, got[i] == want[i], "next admission projection[%d] = %q, want %q", i, got[i], want[i])
		}
	})
}
