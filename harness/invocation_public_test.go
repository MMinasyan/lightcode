package harness_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// Invocation ownership rows: one pressure-triggered Operation carries one
// conversation invocation and a distinct maintenance invocation — different
// identities and purposes, the captured conversation model against the
// captured compact model — each closed after its effects settle and before the
// owning execution closes. Manual maintenance opens no conversation invocation.

var invocationIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

type recordedInvocation struct {
	operation string
	request   harness.InvocationRequest
}

type recordedAttempt struct {
	operation  string
	purpose    harness.InvocationPurpose
	systemText string
}

// invocationRecordingFixture wires one public Harness whose preparation opens
// recording executions: every Engine request, every physical attempt, and every
// invocation and execution close lands on shared fixture state under one mutex.
type invocationRecordingFixture struct {
	t        *testing.T
	h        *harness.Harness
	events   chan string
	mu       sync.Mutex
	requests []recordedInvocation
	attempts []recordedAttempt
}

func newInvocationRecordingFixture(t *testing.T, store harness.Storage, script *scriptModel, capture harness.ExecutionCapture) *invocationRecordingFixture {
	t.Helper()
	rec := &invocationRecordingFixture{t: t, events: make(chan string, 64)}
	fixture := newPublicFixture(t, store, script, nil)
	t.Cleanup(fixture.close)
	fixture.prepareHook = func(_ int, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
		return harness.PreparedExecution{
			Capture: capture,
			Open: func(_ context.Context, adm harness.OperationAdmission) (harness.Execution, error) {
				rec.emit("open:" + adm.OperationID)
				return harness.Execution{OpenInvocation: func(_ context.Context, request harness.InvocationRequest) (harness.InvocationExecution, error) {
					rec.mu.Lock()
					rec.requests = append(rec.requests, recordedInvocation{operation: adm.OperationID, request: request})
					rec.mu.Unlock()
					rec.emit("invocation:" + adm.OperationID + ":" + string(request.Purpose))
					purpose := request.Purpose
					return harness.InvocationExecution{
						Model: func(ctx context.Context, req model.Request) (model.Stream, error) {
							system := ""
							if len(req.Messages) > 0 {
								system = req.Messages[0].TextContent()
							}
							rec.mu.Lock()
							rec.attempts = append(rec.attempts, recordedAttempt{operation: adm.OperationID, purpose: purpose, systemText: system})
							rec.mu.Unlock()
							return script.effect(ctx, req)
						},
						NormalizeTool: publicNormalize,
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"}}}
						},
						Close: func() error {
							// The closer reads the durable state: the
							// invocation disposes only after its effects
							// settled, and a maintenance stage disposes only
							// after its compaction checkpoint committed.
							state, err := rec.operationState(adm.SessionID, adm.OperationID)
							if err != nil {
								rec.t.Errorf("read operation from the invocation closer: %v", err)
							} else {
								switch purpose {
								case harness.InvocationPurposeMaintenance:
									if state.ActiveEffect != nil {
										rec.t.Errorf("maintenance invocation %q closed with active effect %+v still durable", request.ID, state.ActiveEffect)
									}
									committed, err := rec.checkpointCommitted(store, adm.SessionID, adm.OperationID)
									if err != nil {
										rec.t.Errorf("read the checkpoint from the maintenance closer: %v", err)
									} else if !committed {
										rec.t.Errorf("maintenance invocation %q closed before its compaction checkpoint committed", request.ID)
									}
								case harness.InvocationPurposeConversation:
									if state.Status == harness.OperationRunning {
										rec.t.Errorf("conversation invocation %q closed while the operation still runs", request.ID)
									}
								}
							}
							rec.emit("invocation-close:" + adm.OperationID + ":" + string(purpose))
							return nil
						},
					}, nil
				}, Close: func() error {
					rec.emit("close:" + adm.OperationID)
					return nil
				}}, nil
			},
		}, nil
	}
	rec.h = fixture.h
	return rec
}

// checkpointCommitted reports whether one Operation's compaction entry is
// already durable in the store.
func (r *invocationRecordingFixture) checkpointCommitted(store harness.Storage, sessionID, operationID string) (bool, error) {
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Kind == harness.EntryCompaction && entry.OperationID == operationID {
			return true, nil
		}
	}
	return false, nil
}

func (r *invocationRecordingFixture) emit(event string) {
	r.events <- event
}

func (r *invocationRecordingFixture) operationState(sessionID, operationID string) (harness.OperationCurrentState, error) {
	rec, err := r.h.ReadOperation(context.Background(), sessionID, operationID)
	if err != nil {
		return harness.OperationCurrentState{}, err
	}
	return rec.State, nil
}

func (r *invocationRecordingFixture) nextEvent(want string) {
	r.t.Helper()
	select {
	case got := <-r.events:
		testassert.Equal(r.t, got, want)
	case <-time.After(30 * time.Second):
		r.t.Fatalf("timed out waiting for lifetime event %q", want)
	}
}

// TestPublicAutomaticCompactionInvocationOwnership is the pressure-triggered
// row: one overflowing submission opens one Operation, one conversation
// invocation, and one distinct maintenance invocation — different identities
// and purposes, the conversation model against the captured compact model, the
// summary request ahead of the rebuilt conversation request — and every
// invocation closes after its effects settle, before the owning execution
// closes.
func TestPublicAutomaticCompactionInvocationOwnership(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		capture := publicCapture()
		capture.ContextWindow = 4096
		capture.OutputReserve = 2048 // the trigger threshold: 2048
		capture.Compact.Model = model.ModelRef{Provider: "prov", Model: "compact-x"}
		// The seed turn reports 3001 input tokens: the next checkpoint
		// compacts. The overflowing Operation answers the maintenance request
		// with the summary, then the rebuilt conversation request completes.
		script := newScriptModel(
			publicAttempt{stream: publicUsageTurnStream(model.Usage{InputTokens: 3000, OutputTokens: 1})},
			publicAttempt{stream: publicTurnStream()},
			publicAttempt{stream: publicUsageTurnStream(model.Usage{InputTokens: 3, OutputTokens: 1})},
		)
		rec := newInvocationRecordingFixture(t, store, script, capture)
		session := createSession(t, rec.h)

		{
			_, err := submit(t, rec.h, session, "op-1", harness.MessageModeRegular, "first question")
			testassert.NoError(t, err)
		}
		{
			op1 := awaitTerminal(t, rec.h, session, "op-1")
			testassert.Require(t, op1.State.Status == harness.OperationSuccess, "seed operation = %+v, want success", op1.State)
		}
		rec.nextEvent("open:op-1")
		rec.nextEvent("invocation:op-1:conversation")
		rec.nextEvent("invocation-close:op-1:conversation")
		rec.nextEvent("close:op-1")

		{
			_, err := submit(t, rec.h, session, "op-2", harness.MessageModeRegular, "the overflowing checkpoint")
			testassert.NoError(t, err)
		}
		rec.nextEvent("open:op-2") // one Operation
		rec.nextEvent("invocation:op-2:conversation")
		rec.nextEvent("invocation:op-2:maintenance")
		rec.nextEvent("invocation-close:op-2:maintenance") // the maintenance invocation closes once its summary request settled
		rec.nextEvent("invocation-close:op-2:conversation")
		rec.nextEvent("close:op-2") // the execution closes after both invocations
		{
			op2 := awaitTerminal(t, rec.h, session, "op-2")
			testassert.Require(t, op2.State.Status == harness.OperationSuccess, "overflowing operation = %+v, want the compacted success", op2.State)
		}

		rec.mu.Lock()
		defer rec.mu.Unlock()
		var op2Requests []harness.InvocationRequest
		var op2Attempts []recordedAttempt
		for _, entry := range rec.requests {
			if entry.operation == "op-2" {
				op2Requests = append(op2Requests, entry.request)
			}
		}
		for _, attempt := range rec.attempts {
			if attempt.operation == "op-2" {
				op2Attempts = append(op2Attempts, attempt)
			}
		}
		testassert.Equal(t, len(op2Requests), 2) // exactly one conversation and one maintenance invocation
		testassert.Equal(t, len(op2Attempts), 2) // each opened invocation ran its own request
		testassert.Equal(t, op2Requests[0].Purpose, harness.InvocationPurposeConversation)
		testassert.Equal(t, op2Requests[1].Purpose, harness.InvocationPurposeMaintenance)
		testassert.Require(t, invocationIDShape.MatchString(op2Requests[0].ID) && invocationIDShape.MatchString(op2Requests[1].ID), "invocation IDs %q/%q, want fresh per-call identities", op2Requests[0].ID, op2Requests[1].ID)
		testassert.Require(t, op2Requests[0].ID != op2Requests[1].ID, "conversation and maintenance invocations share identity %q", op2Requests[0].ID)
		testassert.Require(t, op2Requests[0].Model == capture.Model, "conversation invocation model = %+v, want the captured conversation model", op2Requests[0].Model)
		testassert.Require(t, op2Requests[1].Model == capture.Compact.Model, "maintenance invocation model = %+v, want the captured compact model", op2Requests[1].Model)
		testassert.Equal(t, op2Attempts[0].purpose, harness.InvocationPurposeMaintenance) // the summary request runs first at the checkpoint
		testassert.Require(t, op2Attempts[0].systemText == capture.Compact.SystemPrompt, "maintenance request system = %q, want the captured compact prompt", op2Attempts[0].systemText)
		testassert.Equal(t, op2Attempts[1].purpose, harness.InvocationPurposeConversation)
		testassert.Require(t, op2Attempts[1].systemText == capture.SystemPrompt, "conversation request system = %q, want the captured conversation prompt", op2Attempts[1].systemText)

		// The pressure-triggered checkpoint is one Operation's work: exactly
		// one compaction entry, owned by op-2.
		entries, err := store.ReadEntries(context.Background(), session, 0)
		testassert.NoError(t, err)
		compactions := 0
		for _, entry := range entries {
			if entry.Kind != harness.EntryCompaction {
				continue
			}
			compactions++
			testassert.Require(t, entry.OperationID == "op-2", "compaction entry %s owned by %q, want op-2", entry.ID, entry.OperationID)
		}
		testassert.Equal(t, compactions, 1)
	})
}

// TestPublicManualCompactionInvocationOwnership is the manual row: a dedicated
// compact Operation opens its maintenance invocation with the captured compact
// model and never opens a conversation invocation; its invocation closes before
// its execution closes.
func TestPublicManualCompactionInvocationOwnership(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		capture := publicCapture()
		capture.Compact.Model = model.ModelRef{Provider: "prov", Model: "compact-x"}
		script := newScriptModel(
			publicAttempt{stream: publicUsageTurnStream(model.Usage{InputTokens: 3, OutputTokens: 1})}, // op-1 completes below any trigger
			publicAttempt{stream: publicTurnStream()},                                                  // the manual summary
		)
		rec := newInvocationRecordingFixture(t, store, script, capture)
		session := createSession(t, rec.h)
		{
			_, err := submit(t, rec.h, session, "op-1", harness.MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		{
			op1 := awaitTerminal(t, rec.h, session, "op-1")
			testassert.Require(t, op1.State.Status == harness.OperationSuccess, "source operation = %+v, want success", op1.State)
		}
		rec.nextEvent("open:op-1")
		rec.nextEvent("invocation:op-1:conversation")
		rec.nextEvent("invocation-close:op-1:conversation")
		rec.nextEvent("close:op-1")

		// Compact waits for true idleness: the retiring predecessor's run slot
		// rejection is retried against its own stable Operation identity.
		deadline := time.Now().Add(30 * time.Second)
		for {
			_, err := rec.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: "cx-1"})
			if err == nil {
				break
			}
			testassert.Require(t, errors.Is(err, harness.ErrInvalid) && strings.Contains(err.Error(), "not idle"), "Compact = %v, want the not-idle rejection until retirement", err)
			testassert.Require(t, time.Now().Before(deadline), "the Session never became idle for manual compaction")
			time.Sleep(time.Millisecond)
		}
		rec.nextEvent("open:cx-1")
		rec.nextEvent("invocation:cx-1:maintenance") // manual maintenance opens no conversation invocation
		rec.nextEvent("invocation-close:cx-1:maintenance")
		rec.nextEvent("close:cx-1")
		{
			cx := awaitTerminal(t, rec.h, session, "cx-1")
			testassert.Require(t, cx.State.Status == harness.OperationSuccess && cx.Admission.RequestKind == harness.RequestKindCompact, "manual compaction = %+v, want the compact success", cx.State)
		}

		rec.mu.Lock()
		defer rec.mu.Unlock()
		var cxRequests []harness.InvocationRequest
		var cxAttempts []recordedAttempt
		for _, entry := range rec.requests {
			if entry.operation == "cx-1" {
				cxRequests = append(cxRequests, entry.request)
			}
		}
		for _, attempt := range rec.attempts {
			if attempt.operation == "cx-1" {
				cxAttempts = append(cxAttempts, attempt)
			}
		}
		testassert.Equal(t, len(cxRequests), 1)
		testassert.Equal(t, cxRequests[0].Purpose, harness.InvocationPurposeMaintenance)
		testassert.Require(t, invocationIDShape.MatchString(cxRequests[0].ID), "maintenance invocation ID %q, want a fresh per-call identity", cxRequests[0].ID)
		testassert.Require(t, cxRequests[0].Model == capture.Compact.Model, "maintenance invocation model = %+v, want the captured compact model", cxRequests[0].Model)
		testassert.Equal(t, len(cxAttempts), 1)
		testassert.Require(t, cxAttempts[0].systemText == capture.Compact.SystemPrompt, "manual summary system = %q, want the captured compact prompt", cxAttempts[0].systemText)
	})
}
