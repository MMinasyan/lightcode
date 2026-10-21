package runtime

// The composed runtime-loss row: the existing cold seeds — one real running
// root Operation whose model effect parks until cleanup, plus the quiescent
// valid running child under its idle parent committed directly through the
// storage contract — are the durable state a process loss leaves behind; the
// process-local buffers and members are lost by construction. A successor is
// opened through the assembled production composition over the same store,
// its protocol server is mounted, and every read proves durable interruption
// and no model/tool/job replay; the fresh stream then proves it carries only
// fresh work — created and submitted through the generated HTTP operations —
// with ordinary subsequent execution.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

func TestProtocolComposedRuntimeLoss(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		runningRoot, runningOp := seedRunningOperation(t, store, "/tmp/loss-running-ws")
		parent := newLifecycleID(t)
		child := newLifecycleID(t)
		childOp := newLifecycleID(t)
		seedLifecycleRoot(t, store, parent)
		seedLifecycleChild(t, store, parent, child, childOp)

		e := newOwnerEnv(t)
		server := newProductionModelServer(t, "probe", "{}")
		t.Setenv("ASSEMBLED_TEST_KEY", "assembled-key-1")
		writeServiceFile(t, e.configPath, assembledConfigDocument(server.URL, "T1", "L1"))
		writeServiceFile(t, agents.PathForConfig(e.configPath), assembledAgentsDocument)
		hook := &recordHook{name: "hook.first", events: e.events, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
			return c
		}}
		ws := &workspaceOpens{}
		r, err := Open(ctx, Options{DataDir: e.dataDir, ConfigPath: e.configPath, Plugins: assembledPlugins(e, store, hook, ws)})
		testassert.NoError(t, err)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		// The stream opens before the recovered Sessions are ever touched, so
		// any publication their later reads would trigger is observable by
		// the per-frame replay oracle below.
		stream := openSSEStream(t, ps)
		defer stream.close()
		// The deferred owner join runs after every assertion; the explicit
		// closed-owner phase below closes it first and the second join is a
		// no-op.
		defer closeProjectionRuntime(r)

		// No replay: recovery invoked no model transport and no tool
		// capability while the successor opened and recovered. countEvents is
		// exact-match on the recorded trace, so the fixture's probe tool's
		// real trace is spelled out: one invocation during recovery fails this
		// oracle even while the model counter still reads zero.
		testassert.Equal(t, server.requests(), 0)
		testassert.Equal(t, countEvents(e.events.all(), "work:L1:base"), 0)

		// Durable interruption through the mounted read: the seeded running
		// root settled as the runtime-loss interruption with no live member.
		rootHydration := composedLossHydration(t, client, runningRoot)
		{
			op := composedLossOperation(t, rootHydration, runningOp)
			testassert.Equal(t, op.Status, protocol.OperationStatusInterruption)
		}
		testassert.Require(t, rootHydration.ActiveOperation == nil && len(rootHydration.Background) == 0 && rootHydration.SessionRevision.InstanceId == ps.instance, "seeded root hydration = active %+v background %+v, want no active work and no member; got %v, want %v", rootHydration.ActiveOperation, rootHydration.Background, rootHydration.SessionRevision.InstanceId, ps.instance)

		// The child hydrates as one ordinary Session with its own interrupted
		// Operation and its committed input visible.
		childHydration := composedLossHydration(t, client, child)
		{
			op := composedLossOperation(t, childHydration, childOp)
			testassert.Equal(t, op.Status, protocol.OperationStatusInterruption)
		}
		testassert.Require(t, childHydration.Session.SessionId == child && childHydration.Session.Lifecycle == protocol.Open && childHydration.ActiveOperation == nil && len(childHydration.Background) == 0, "seeded child hydration = %+v, want one ordinary open session", childHydration.Session)
		testassert.Require(t, len(childHydration.Conversation.Items) != 0, "the seeded child's committed input vanished from its hydration")

		// No replayed facts reach the fresh stream: fresh work — created and
		// submitted through the generated operations — is read until its own
		// session_changed witness, and every frame consumed up to that
		// witness carries only the fresh Session and Workspace identities.
		freshWorkspace := filepath.Join(e.home, "loss-next")
		created, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: freshWorkspace, AgentType: "integrated"})
		testassert.NoError(t, err)
		testassert.Require(t, created.JSON201 != nil, "CreateSession(fresh) = status %d: %s", created.HTTPResponse.StatusCode, created.Body)
		fresh := created.JSON201.SessionId
		var freshPart protocol.ContentPart
		testassert.NoError(t, freshPart.FromTextPart(protocol.TextPart{Kind: protocol.TextPartKindText, Text: "continue"}))
		submitted, err := client.SubmitSessionWithResponse(ctx, fresh, protocol.SubmitRequest{
			OperationId: "op-after-loss",
			Mode:        protocol.SubmitRequestModeRegular,
			Content:     []protocol.ContentPart{freshPart},
		})
		testassert.NoError(t, err)
		testassert.Require(t, submitted.JSON200 != nil, "SubmitSession(fresh) = status %d: %s", submitted.HTTPResponse.StatusCode, submitted.Body)
		for {
			frame, ok, err := stream.readSSEFrame()
			testassert.NoError(t, err)
			testassert.Require(t, ok, "the fresh stream closed before the fresh work's witness")
			var event protocol.Event
			{
				err := json.Unmarshal(frame, &event)
				testassert.Require(t, err == nil, "decode fresh stream frame %s: %v", frame, err)
			}
			scope, err := composedLossEventScope(t, event)
			testassert.NoError(t, err)
			{
				stale, ok := scopeSessionIdentity(t, scope)
				testassert.Require(t, !ok || stale == fresh, "replayed frame for session %s reached the fresh stream: %s", stale, frame)
			}
			{
				stale, ok := scopeWorkspaceAttribution(t, scope)
				testassert.Require(t, !ok || stale == freshWorkspace, "replayed frame for workspace %s reached the fresh stream: %s", stale, frame)
			}
			if body, err := event.AsSessionChangedEvent(); err == nil &&
				scopeKind(t, body.Scope) == "session" {
				freshScope, err := body.Scope.AsSessionScope()
				testassert.NoError(t, err)
				if freshScope.SessionId == fresh {
					break // the fresh work's own invalidation is the witness
				}
			}
		}

		// Ordinary work proceeds on the repaired state through the mounted
		// route: the successor's first model requests belong to fresh work.
		composedLossAwaitSettled(t, client, fresh, "op-after-loss")
		{
			got := server.requests()
			testassert.Require(t, got >= 1, "the fresh turn's model requests = %d, want the real execution", got)
		}
		settled := composedLossHydration(t, client, fresh)
		{
			op := composedLossOperation(t, settled, "op-after-loss")
			testassert.Equal(t, op.Status, protocol.OperationStatusSuccess)
		}

		// The closed owner: the listener refuses connections.
		stream.close()
		testassert.NoError(t, r.Close(ctx))
		{
			_, err := net.Dial("tcp", strings.TrimPrefix(ps.Endpoint(), "http://"))
			testassert.Require(t, err != nil, "the protocol listener still accepts after the successor closed")
		}
	})
}

func composedLossHydration(t *testing.T, client *protocol.ClientWithResponses, sessionID string) *protocol.Hydration {
	t.Helper()
	resp, err := client.GetSessionHydrationWithResponse(context.Background(), sessionID)
	testassert.Require(t, err == nil, "GetSessionHydration(%s): %v", sessionID, err)
	testassert.Require(t, resp.JSON200 != nil, "GetSessionHydration(%s) = status %d", sessionID, resp.HTTPResponse.StatusCode)
	return resp.JSON200
}

// composedLossAwaitSettled polls the mounted hydration until one Operation
// settles — a real state barrier, never a timed wait.
func composedLossAwaitSettled(t *testing.T, client *protocol.ClientWithResponses, sessionID, operationID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		h := composedLossHydration(t, client, sessionID)
		for _, operation := range h.Operations {
			if operation.OperationId == operationID && operation.Status == protocol.OperationStatusSuccess {
				return
			}
		}
		testassert.Require(t, !time.Now().After(deadline), "operation %q never settled to success (operations %+v)", operationID, h.Operations)
		time.Sleep(10 * time.Millisecond)
	}
}

// composedLossEventScope extracts one event's scope; every SSE event kind
// carries exactly one, and an unknown kind is a failure, never a silent
// empty scope.
func composedLossEventScope(t *testing.T, event protocol.Event) (protocol.Scope, error) {
	t.Helper()
	kind, err := event.Discriminator()
	if err != nil {
		return protocol.Scope{}, err
	}
	switch kind {
	case "scope_opened", "scope_closed":
		body, err := event.AsScopeEvent()
		return body.Scope, err
	case "configuration_changed":
		body, err := event.AsConfigurationChangedEvent()
		return body.Scope, err
	case "session_changed":
		body, err := event.AsSessionChangedEvent()
		return body.Scope, err
	case "warning_changed":
		body, err := event.AsWarningChangedEvent()
		return body.Scope, err
	case "text_delta":
		body, err := event.AsTextDeltaEvent()
		return body.Scope, err
	case "tool_started":
		body, err := event.AsToolStartedEvent()
		return body.Scope, err
	case "tool_finished":
		body, err := event.AsToolFinishedEvent()
		return body.Scope, err
	}
	return protocol.Scope{}, fmt.Errorf("unknown event kind %q", kind)
}

func composedLossOperation(t *testing.T, h *protocol.Hydration, operationID string) protocol.Operation {
	t.Helper()
	for _, operation := range h.Operations {
		if operation.OperationId == operationID {
			return operation
		}
	}
	t.Fatalf("operation %q absent from hydration (operations %+v)", operationID, h.Operations)
	return protocol.Operation{}
}
