package runtime

// Retained native child-authority coverage over the actual Native tool ->
// declared host child_launch service -> Harness admission path. The removed
// legacy BackgroundServices fixture is intentionally not restored; these tests
// drive the current native Tool/ExtensionContext/CallContext contract and
// assert the committed child and settlement identities.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

func bridgeChildLaunchArguments(operationID string) json.RawMessage {
	value, _ := json.Marshal(map[string]any{
		"agent_type":     "secondary",
		"prompt":         "child work",
		"operation_id":   operationID,
		"max_concurrent": 4,
		"output_limit":   15360,
	})
	return value
}

type bridgeChildProbe struct {
	mode         string
	childStarted chan struct{}
	started      chan struct{}
	launched     chan struct{}
	done         chan struct{}

	childID      string
	launchErr    error
	retained     extension.CallContext
	executes     atomic.Int64
	launchedOnce sync.Once
	startedOnce  sync.Once
	doneOnce     sync.Once
}

func (p *bridgeChildProbe) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (p *bridgeChildProbe) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionChildLaunch, Target: "secondary"}}}
}
func (p *bridgeChildProbe) Execute(ctx context.Context, tc extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	arguments := bridgeChildLaunchArguments("probe-child-op")
	p.executes.Add(1)
	if p.mode == "canceled_origin" {
		p.startedOnce.Do(func() { close(p.started) })
		<-ctx.Done()
		_, p.launchErr = tc.Calls.Call(context.Background(), "child_launch", "launch", arguments)
		p.doneOnce.Do(func() { close(p.done) })
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: tc.Calls.ToolCallID, Status: model.ResultInterrupted, Content: "canceled"}}
	}
	result, err := tc.Calls.Call(ctx, "child_launch", "launch", arguments)
	if err != nil {
		p.launchErr = err
		p.launchedOnce.Do(func() { close(p.launched) })
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: tc.Calls.ToolCallID, Status: model.ResultError, Content: err.Error()}}
	}
	var launched struct {
		ChildSessionID string `json:"child_session_id"`
	}
	if err := json.Unmarshal(result.Value, &launched); err != nil {
		p.launchErr = err
		p.launchedOnce.Do(func() { close(p.launched) })
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: tc.Calls.ToolCallID, Status: model.ResultError, Content: err.Error()}}
	}
	p.childID = launched.ChildSessionID
	<-p.childStarted // the committed child is blocked in its own model request
	if p.mode == "interrupted_committed" {
		p.launchedOnce.Do(func() { close(p.launched) })
		<-ctx.Done()
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: tc.Calls.ToolCallID, Status: model.ResultInterrupted, Content: "interrupted"}}
	}
	p.retained = tc.Calls
	p.launchedOnce.Do(func() { close(p.launched) })
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: tc.Calls.ToolCallID, Status: model.ResultSuccess, Content: launched.ChildSessionID}}
}

func bridgeChildCount(t *testing.T, r *Runtime, store harness.Storage) int {
	t.Helper()
	ids, err := store.ListSessionIDs(context.Background())
	testassert.NoError(t, err)
	children := 0
	for _, id := range ids {
		header, err := r.harness.ReadSessionHeader(context.Background(), id)
		if err == nil && header.Identity.ParentSessionID != "" {
			children++
		}
	}
	return children
}

func bridgeParentHasChildResult(t *testing.T, e *ownerEnv, childID string) bool {
	t.Helper()
	for i := 0; i < e.server.requests(); i++ {
		var doc struct {
			Messages []struct {
				Role       string          `json:"role"`
				Content    json.RawMessage `json:"content"`
				ToolCallID string          `json:"tool_call_id"`
			} `json:"messages"`
		}
		_ = json.Unmarshal([]byte(e.server.bodyAt(i)), &doc)
		for _, message := range doc.Messages {
			if message.Role == "tool" && message.ToolCallID == "call-probe" && strings.Contains(string(message.Content), childID) {
				return true
			}
		}
	}
	return false
}

// TestNativeChildBridgeRetainsCommittedChildren proves through the actual
// native Tool -> host child_launch -> Harness admission bridge: a canceled
// Execute origin cannot launch even with Background; a retained Call view is
// closed after return; and an already-committed child blocked in its own model
// request survives both parent interruption and executor return, then settles
// successfully with the exact parent/child identity.
func TestNativeChildBridgeRetainsCommittedChildren(t *testing.T) {
	for _, row := range []struct {
		name       string
		mode       string
		wantParent harness.OperationState
		wantChild  bool
	}{
		{name: "canceled origin cannot launch", mode: "canceled_origin", wantParent: harness.OperationInterruption},
		{name: "retained view cannot launch after return", mode: "retained", wantParent: harness.OperationSuccess, wantChild: true},
		{name: "executor return keeps committed child", mode: "executor_committed", wantParent: harness.OperationSuccess, wantChild: true},
		{name: "interruption keeps committed child", mode: "interrupted_committed", wantParent: harness.OperationInterruption, wantChild: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			eachPrepStore(t, func(t *testing.T, store harness.Storage) {
				e := newOwnerEnv(t)
				e.fixtureStore(store)
				enableFixturePlugin(t, e.configPath, "grant")
				writeAgentDefinitions(t, e.configPath, `{"grant_agent":{"model":"prov/m","system_prompt":"simple","tools":["grant_tool"]},"secondary":{"model":"prov/m","system_prompt":"simple","subagent":true}}`)
				childStarted := make(chan struct{})
				var childStartedOnce sync.Once
				releaseChild := make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(releaseChild) }) }
				e.server.setScript(func(_ context.Context, body string) []string {
					if lastUserText(body) == "child work" {
						childStartedOnce.Do(func() { close(childStarted) })
						<-releaseChild
						return textTurnEvents("child done")
					}
					if lastMessageRole(body) == "tool" {
						return textTurnEvents("done")
					}
					return toolCallTurnEvents("call-probe", "grant_tool", `{}`)
				})
				probe := &bridgeChildProbe{mode: row.mode, childStarted: childStarted, started: make(chan struct{}), launched: make(chan struct{}), done: make(chan struct{})}
				ctx := context.Background()
				r, err := e.open(ctx, childGrantNative(probe))
				testassert.NoError(t, err)
				t.Cleanup(func() {
					release()
					_ = r.Close(context.Background())
				})
				session, err := r.createSession(ctx, e.dataDir, "grant_agent")
				testassert.NoError(t, err)
				submitThroughRuntime(t, r, session.Identity.SessionID, "probe-parent-op", "run")

				if row.mode == "canceled_origin" {
					select {
					case <-probe.started:
					case <-time.After(10 * time.Second):
						t.Fatal("the probe execution never started")
					}
					err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
						return h.Interrupt(ctx, session.Identity.SessionID)
					})
					testassert.NoError(t, err)
					awaitOperation(t, r, session.Identity.SessionID, "probe-parent-op", row.wantParent)
					select {
					case <-probe.done:
					case <-time.After(10 * time.Second):
						t.Fatal("the canceled-origin probe never resumed")
					}
					testassert.Require(t, probe.launchErr != nil, "post-cancellation Background launch = %v, want a refusal", probe.launchErr)
					testassert.Equal(t, bridgeChildCount(t, r, store), 0)
					return
				}

				select {
				case <-probe.launched:
				case <-time.After(10 * time.Second):
					t.Fatal("the committed child was never observed")
				}
				testassert.Require(t, probe.executes.Load() == 1, "probe Execute calls = %d, want one per tool call", probe.executes.Load())
				testassert.Require(t, len(probe.childID) == 32, "child id = %q, want the committed child identity", probe.childID)
				childHeader, err := r.harness.ReadSessionHeader(context.Background(), probe.childID)
				testassert.NoError(t, err)
				testassert.Equal(t, childHeader.Identity.ParentSessionID, session.Identity.SessionID)
				childOp, err := r.harness.ReadOperation(context.Background(), probe.childID, "probe-child-op")
				testassert.NoError(t, err)
				testassert.Equal(t, childOp.State.Status, harness.OperationRunning)

				if row.mode == "retained" {
					awaitOperation(t, r, session.Identity.SessionID, "probe-parent-op", row.wantParent)
					if _, err := probe.retained.Call(context.Background(), "child_launch", "launch", bridgeChildLaunchArguments("probe-child-op-2")); !errors.Is(err, extension.ErrClosed) {
						t.Fatalf("the retained Call view error = %v, want the closed view refusal", err)
					}
					testassert.Equal(t, bridgeChildCount(t, r, store), 1)
				} else if row.mode == "executor_committed" {
					awaitOperation(t, r, session.Identity.SessionID, "probe-parent-op", row.wantParent)
					testassert.Require(t, bridgeParentHasChildResult(t, e, probe.childID), "the committed parent result does not name child %q", probe.childID)
				} else {
					err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
						return h.Interrupt(ctx, session.Identity.SessionID)
					})
					testassert.NoError(t, err)
					awaitOperation(t, r, session.Identity.SessionID, "probe-parent-op", row.wantParent)
				}

				release()
				awaitOperation(t, r, probe.childID, "probe-child-op", harness.OperationSuccess)
				testassert.Equal(t, bridgeChildCount(t, r, store), 1)
				childHeader, err = r.harness.ReadSessionHeader(context.Background(), probe.childID)
				testassert.NoError(t, err)
				testassert.Equal(t, childHeader.Identity.ParentSessionID, session.Identity.SessionID)
			})
		})
	}
}
