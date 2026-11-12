package melta

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	workerfixture "github.com/MMinasyan/lightcode/extension/worker/testdata/fixture/native"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
	"golang.org/x/sys/unix"
)

const (
	fixtureExtensionID = "fixture"
	fixtureService     = "fixture.service"
	fixtureAlias       = "fixture.service.alias"
	fixtureCommands    = "fixture.commands"
	fixtureWorkspace   = "fixture.workspace.query"
)

func fixtureHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func fixtureCaller(t *testing.T, r *Melta, scope CallerScope) Caller {
	t.Helper()
	caller, err := r.Caller(CallerContext{Origin: harness.InputOriginUser, Scope: scope})
	testassert.NoError(t, err)
	return caller
}

func fixtureRequest(extensionID, capabilityID, contractID, method string, target protocol.ExtensionInvocationTarget, arguments, requestID string) protocol.ExtensionInvocationRequest {
	rawArguments := json.RawMessage(arguments)
	return protocol.ExtensionInvocationRequest{
		ExtensionId:     extensionID,
		CapabilityId:    capabilityID,
		ContractId:      contractID,
		ContractVersion: extension.ContractVersion,
		Method:          method,
		Target:          target,
		Arguments:       &rawArguments,
		RequestId:       requestID,
	}
}

func workspaceTarget(workspace string) protocol.ExtensionInvocationTarget {
	return protocol.ExtensionInvocationTarget{Kind: protocol.ExtensionInvocationTargetKindWorkspace, Workspace: &workspace}
}

func sessionTarget(sessionID string) protocol.ExtensionInvocationTarget {
	return protocol.ExtensionInvocationTarget{Kind: protocol.ExtensionInvocationTargetKindSession, SessionId: &sessionID}
}

func harnessTarget() protocol.ExtensionInvocationTarget {
	return protocol.ExtensionInvocationTarget{Kind: protocol.ExtensionInvocationTargetKindHarness}
}

func openFixtureParity(t *testing.T, host parityHost) (*Melta, *productionEnv) {
	t.Helper()
	e := newProductionEnv(t, storage.NewMemory())
	t.Setenv("FIXTURE_ACTIVATION", "activation-secret")
	configureFixture(t, e, `{"enabled":true,"activation_token":"FIXTURE_ACTIVATION"}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple"}}`)
	r, err := e.openParity(context.Background(), host)
	if err != nil {
		t.Fatalf("open %s owner: %v", host.name, err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r, e
}

func fixtureEvents(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv(workerfixture.EventLogEnv))
	if err != nil {
		t.Fatalf("read fixture event log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func countFixtureEvent(t *testing.T, prefix string) int {
	t.Helper()
	count := 0
	for _, line := range fixtureEvents(t) {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

// TestExtensionCatalogAndAddressingParity proves the public catalog and the
// single invocation path resolve the concrete declaration and target before
// binding, keep two same-contract capabilities separately addressable, preserve
// the constructed scope while carrying target attribution, and refuse every
// out-of-scope or unexposed address without calling the handler.
func TestExtensionCatalogAndAddressingParity(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		r, e := openFixtureParity(t, host)
		ctx := context.Background()
		harnessCaller := fixtureCaller(t, r, harnessScope)
		workspaceA, workspaceB := e.workspace("catalog-a"), e.workspace("catalog-b")
		sessionA, err := r.CreateSession(ctx, harnessCaller, workspaceA, nil)
		testassert.NoError(t, err)
		sessionB, err := r.CreateSession(ctx, harnessCaller, workspaceB, nil)
		testassert.NoError(t, err)

		catalog, err := r.ListExtensionCapabilities(ctx, harnessCaller)
		testassert.NoError(t, err)
		var entry *protocol.ExtensionCapabilityCatalog
		for i := range catalog {
			if catalog[i].ExtensionId == fixtureExtensionID {
				entry = &catalog[i]
			}
		}
		testassert.Require(t, entry != nil, "catalog omitted the enabled fixture extension: %+v", catalog)
		testassert.Require(t, entry.ConfigurationSchema != "" && entry.Settings != "" && strings.Contains(entry.Settings, `"enabled":true`), "catalog schema/settings = %q/%q", entry.ConfigurationSchema, entry.Settings)
		var sawService, sawTool, sawAlias bool
		for _, capability := range entry.Capabilities {
			switch capability.CapabilityId {
			case fixtureService:
				sawService = true
				testassert.Require(t, capability.ContractId == "fixture.service/v1" && capability.ContractVersion == "1" && capability.Kind == protocol.ExtensionCapabilityKindService, "service capability = %+v", capability)
				var sawHidden bool
				for _, method := range capability.Methods {
					sawHidden = sawHidden || method.Name == "retain"
				}
				testassert.Require(t, sawHidden, "catalog omitted the hidden retain method: %+v", capability.Methods)
			case fixtureAlias:
				sawAlias = true
			case "fixture.tool":
				sawTool = true
			}
		}
		testassert.Require(t, sawService && sawAlias && sawTool, "catalog capabilities = %+v, want the service, its same-contract alias and a tool", entry.Capabilities)
		var sawEcho, sawWorkspaceProbe bool
		for _, exposed := range entry.Exposed {
			sawEcho = sawEcho || exposed.CapabilityId == fixtureService && exposed.Method == "echo" && exposed.Kind == protocol.Query && exposed.Target == protocol.ExtensionExposureTargetHarness
			sawWorkspaceProbe = sawWorkspaceProbe || exposed.CapabilityId == fixtureWorkspace && exposed.Method == "probe" && exposed.Target == protocol.ExtensionExposureTargetWorkspace
		}
		testassert.Require(t, sawEcho && sawWorkspaceProbe, "catalog exposures = %+v", entry.Exposed)

		// Two same-contract capabilities stay separately addressable and call
		// their own concrete handler even though the contract identity matches.
		body := "{\n  \"big\": 9007199254740993,\n  \"escaped\": \"line\\n\\t<>&\"\n}"
		mainResult, err := r.InvokeExtension(ctx, harnessCaller, fixtureRequest(fixtureExtensionID, fixtureService, "fixture.service/v1", "echo", harnessTarget(), body, "req-main"))
		testassert.NoError(t, err)
		testassert.Require(t, mainResult.Failure == nil && string(mainResult.Value) == body, "main echo result = %+v, want the exact raw bytes", mainResult)
		aliasResult, err := r.InvokeExtension(ctx, harnessCaller, fixtureRequest(fixtureExtensionID, fixtureAlias, "fixture.service/v1", "echo", harnessTarget(), body, "req-alias"))
		testassert.NoError(t, err)
		testassert.Require(t, aliasResult.Failure == nil && string(aliasResult.Value) == body, "alias echo result = %+v, want the exact raw bytes", aliasResult)
		testassert.Require(t, countFixtureEvent(t, "service:echo:") == 1 && countFixtureEvent(t, "alias:echo:") == 1, "same-contract calls did not reach their own handlers: %v", fixtureEvents(t))

		// The first Workspace construction wins through the shared builder and
		// carries the declared activation credential binding.
		workspaceCaller := fixtureCaller(t, r, CallerScope{Kind: CallerScopeWorkspace, Workspace: workspaceA})
		probe, err := r.InvokeExtension(ctx, workspaceCaller, fixtureRequest(fixtureExtensionID, fixtureWorkspace, "fixture.workspace/v1", "probe", workspaceTarget(workspaceA), `{}`, "req-probe"))
		testassert.NoError(t, err)
		testassert.Require(t, probe.Failure == nil, "workspace probe failure = %+v", probe.Failure)
		var probeValue map[string]string
		testassert.NoError(t, json.Unmarshal(probe.Value, &probeValue))
		testassert.Require(t, probeValue["credential"] == fixtureHash("activation-secret"), "workspace activation credential = %q, want the first-created Workspace capture", probeValue["credential"])
		testassert.Require(t, probeValue["workspace"] == workspaceA, "workspace attribution = %q, want %q", probeValue["workspace"], workspaceA)

		// A Workspace grant cannot reach another Workspace or its Sessions.
		_, err = r.InvokeExtension(ctx, workspaceCaller, fixtureRequest(fixtureExtensionID, fixtureWorkspace, "fixture.workspace/v1", "probe", workspaceTarget(workspaceB), `{}`, "req-cross-workspace"))
		testassert.Require(t, errors.Is(err, ErrDenied), "cross-Workspace call = %v, want denied", err)
		_, err = r.InvokeExtension(ctx, workspaceCaller, fixtureRequest(fixtureExtensionID, fixtureCommands, "fixture.commands/v1", "command", sessionTarget(sessionB.SessionId), `{"method":"submit","arguments":{"operation_id":"x","mode":"regular","content":[{"kind":"text","text":"x"}]}}`, "req-cross-session"))
		testassert.Require(t, errors.Is(err, ErrDenied), "cross-Session call = %v, want denied", err)
		// A Session grant cannot select another Session for A's call.
		sessionCaller := fixtureCaller(t, r, CallerScope{Kind: CallerScopeSession, SessionID: sessionA.SessionId})
		_, err = r.InvokeExtension(ctx, sessionCaller, fixtureRequest(fixtureExtensionID, fixtureCommands, "fixture.commands/v1", "command", sessionTarget(sessionB.SessionId), `{"method":"submit","arguments":{"operation_id":"y","mode":"regular","content":[{"kind":"text","text":"y"}]}}`, "req-session-b"))
		testassert.Require(t, errors.Is(err, ErrDenied), "Session B selected for A's call = %v, want denied", err)
		// Hidden methods and forged contract addresses never dispatch.
		_, err = r.InvokeExtension(ctx, harnessCaller, fixtureRequest(fixtureExtensionID, fixtureService, "fixture.service/v1", "retain", harnessTarget(), `{}`, "req-hidden"))
		testassert.Require(t, errors.Is(err, ErrDenied), "unexposed method = %v, want denied", err)
		_, err = r.InvokeExtension(ctx, harnessCaller, fixtureRequest(fixtureExtensionID, fixtureService, "other.contract/v1", "echo", harnessTarget(), `{}`, "req-forged-contract"))
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "forged contract = %v, want invalid", err)
		_, err = r.InvokeExtension(ctx, harnessCaller, fixtureRequest(fixtureExtensionID, "fixture.unknown", "fixture.unknown/v1", "x", harnessTarget(), `{}`, "req-unknown"))
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "unknown capability = %v, want not found", err)
		testassert.Require(t, countFixtureEvent(t, "command:submit") == 0, "refused addresses executed a command handler: %v", fixtureEvents(t))
		return newParityObservation()
	})
}

// TestExtensionSessionCommandsParity proves the fixed melta.commands/v1 bridge
// uses ordinary admission and attribution for every command route, derives the
// Session only from the host-issued context, rejects malformed generated
// arguments before mutation, and gives pure/management callbacks no command
// grant.
func TestExtensionSessionCommandsParity(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		r, e := openFixtureParity(t, host)
		e.server.setScript(func(_ context.Context, _ string) []string { return textTurnEvents("done") })
		ctx := context.Background()
		harnessCaller := fixtureCaller(t, r, harnessScope)
		workspace := e.workspace("commands")
		selection := &protocol.AgentSelection{Definition: &protocol.DefinitionRef{ExtensionId: "agents", CapabilityId: "agents.definitions", DefinitionId: "fixture_agent"}}
		session, err := r.CreateSession(ctx, harnessCaller, workspace, selection)
		testassert.NoError(t, err)
		other, err := r.CreateSession(ctx, harnessCaller, workspace, selection)
		testassert.NoError(t, err)
		sessionCaller := fixtureCaller(t, r, CallerScope{Kind: CallerScopeSession, SessionID: session.SessionId})

		command := func(method, arguments, requestID string) (extension.Result, error) {
			return r.InvokeExtension(ctx, sessionCaller, fixtureRequest(fixtureExtensionID, fixtureCommands, "fixture.commands/v1", "command", sessionTarget(session.SessionId), `{"method":"`+method+`","arguments":`+arguments+`}`, requestID))
		}
		result, err := command("submit", `{"operation_id":"op-submit","mode":"regular","content":[{"kind":"text","text":"hello"}]}`, "req-submit")
		testassert.NoError(t, err)
		testassert.Require(t, result.Failure == nil, "submit failure = %+v", result.Failure)
		awaitOperation(t, r, session.SessionId, "op-submit", harness.OperationSuccess)
		// Repeating the stable admission ID observes no second Operation.
		before := e.server.requests()
		_, _ = command("submit", `{"operation_id":"op-submit","mode":"regular","content":[{"kind":"text","text":"hello"}]}`, "req-submit-repeat")
		testassert.Require(t, e.server.requests() == before, "repeat admission ID started a second model execution")
		testassert.Require(t, countFixtureEvent(t, "command:submit") == 2, "fixed command bridge ran %d submit calls, want two invocations over one admission", countFixtureEvent(t, "command:submit"))
		testassert.Require(t, countFixtureEvent(t, "observer;") == 2, "nested calls did not preserve one call tree per invocation")

		// The nested observer view keeps the bound Session/request while the
		// constructed Scope remains the provider's own scope.
		events := fixtureEvents(t)
		var commandEvent, observerEvent string
		for _, line := range events {
			if strings.HasPrefix(line, "command:submit") && commandEvent == "" {
				commandEvent = line
			}
			if strings.HasPrefix(line, "observer;") && observerEvent == "" {
				observerEvent = line
			}
		}
		want := `session="` + session.SessionId + `"`
		testassert.Require(t, strings.Contains(commandEvent, want) && strings.Contains(commandEvent, `request="req-submit"`), "command attribution = %q", commandEvent)
		testassert.Require(t, strings.Contains(observerEvent, want) && strings.Contains(observerEvent, `request="req-submit"`), "nested observer attribution = %q", observerEvent)
		testassert.Require(t, strings.Contains(commandEvent, `scope-workspace=""`), "harness provider scope changed under session target: %q", commandEvent)

		// Session-addressed arguments can never claim another Session: an
		// unknown generated member refuses before any mutation.
		_, err = command("submit", `{"session_id":"`+other.SessionId+`","operation_id":"op-wrong","mode":"regular","content":[{"kind":"text","text":"x"}]}`, "req-wrong-session")
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "Session-claiming submit = %v, want invalid", err)
		testassert.Require(t, errors.Is(readOperationError(ctx, r, other.SessionId, "op-wrong"), harness.ErrNotFound), "refused Session-claiming arguments mutated the other Session")
		testassert.Require(t, errors.Is(readOperationError(ctx, r, session.SessionId, "op-wrong"), harness.ErrNotFound), "refused Session-claiming arguments mutated the bound Session")

		// Every fixed command route reaches its ordinary producer through the
		// same bridge.
		_, err = command("compact", `{"operation_id":"op-compact"}`, "req-compact")
		testassert.NoError(t, err)
		awaitOperation(t, r, session.SessionId, "op-compact", harness.OperationSuccess)
		_, err = command("fork", `{"operation_id":"op-fork","boundary_item_id":"`+firstItemID(t, r, session.SessionId)+`","content":[{"kind":"text","text":"fork"}]}`, "req-fork")
		testassert.NoError(t, err)
		_, err = command("set_agent", `{"agent_selection":{"inputs":{"model":"prov/m","instructions":"","assembly_mode":"simple","tools":[],"capabilities":[],"constraints":{"readonly":false,"write_dir":""},"strategy":{"id":"","settings":"{}"}}}}`, "req-agent")
		testassert.NoError(t, err)
		_, err = command("set_model", `{"model":"prov/m"}`, "req-model")
		testassert.NoError(t, err)
		reverted, err := command("revert_code", `{"boundary_operation_id":"op-submit"}`, "req-revert-success")
		testassert.NoError(t, err)
		testassert.Require(t, reverted.Failure == nil, "revert success failure = %+v", reverted.Failure)
		var revertResult protocol.CodeRevertResult
		testassert.NoError(t, json.Unmarshal(reverted.Value, &revertResult))
		testassert.Require(t, revertResult.Restored != nil && revertResult.Skipped != nil && revertResult.Error == nil, "revert success receipt = %+v", revertResult)
		preTraversal, err := command("revert_code", `{"boundary_operation_id":"missing"}`, "req-revert-invalid")
		testassert.Require(t, errors.Is(err, harness.ErrInvalid) && preTraversal.Failure == nil, "pre-traversal revert = (%+v, %v), want the original invalid error", preTraversal, err)
		_, err = command("interrupt", `{}`, "req-interrupt")
		testassert.NoError(t, err)
		_, err = command("stop", `{}`, "req-stop")
		testassert.NoError(t, err)
		archive, err := command("archive", `{}`, "req-archive")
		testassert.NoError(t, err)
		testassert.Require(t, archive.Failure == nil, "archive failure = %+v", archive.Failure)
		_, err = command("reopen", `{}`, "req-reopen")
		testassert.NoError(t, err)
		_, err = command("archive", `{}`, "req-rearchive")
		testassert.NoError(t, err)
		deleted, err := command("delete", `{}`, "req-delete")
		testassert.NoError(t, err)
		testassert.Require(t, deleted.Failure == nil, "delete failure = %+v", deleted.Failure)
		var deletion protocol.DeletionResult
		testassert.NoError(t, json.Unmarshal(deleted.Value, &deletion))
		testassert.Require(t, deletion.CleanupFailures != nil, "delete receipt lost its cleanup_failures array")
		testassert.Require(t, countFixtureEvent(t, "command:revert-call-error") == 0, "pre-traversal revert produced a CallError partial")

		// Create uses its ordinary request value and Caller authorization on a
		// Harness-target session_work method.
		created, err := r.InvokeExtension(ctx, harnessCaller, fixtureRequest(fixtureExtensionID, fixtureCommands, "fixture.commands/v1", "command_harness", harnessTarget(), `{"method":"create_session","arguments":{"workspace":"`+workspace+`","agent_selection":{"definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"fixture_agent"}}}}`, "req-create"))
		testassert.NoError(t, err)
		testassert.Require(t, created.Failure == nil, "create_session failure = %+v", created.Failure)
		var createdSession protocol.Session
		testassert.NoError(t, json.Unmarshal(created.Value, &createdSession))
		testassert.Require(t, createdSession.SessionId != "", "create_session returned no Session: %s", created.Value)

		// A pure callback cannot dispatch service I/O; a management callback
		// holds no command grant. Neither records a command effect.
		pure, err := r.InvokeExtension(ctx, harnessCaller, fixtureRequest(fixtureExtensionID, fixtureCommands, "fixture.commands/v1", "pure_command", harnessTarget(), `{"method":"submit","arguments":{}}`, "req-pure"))
		testassert.Require(t, err != nil && pure.Failure == nil, "pure command = (%+v, %v), want the composition refusal", pure, err)
		management, err := r.InvokeExtension(ctx, harnessCaller, fixtureRequest(fixtureExtensionID, fixtureCommands, "fixture.commands/v1", "management_command", harnessTarget(), `{"method":"submit","arguments":{}}`, "req-management"))
		testassert.Require(t, err != nil && management.Failure == nil, "management command = (%+v, %v), want the denied refusal", management, err)
		testassert.Require(t, countFixtureEvent(t, "command:submit") == 3, "pure/management callbacks reached the command bridge: %v", fixtureEvents(t))
		testassert.Require(t, countFixtureEvent(t, "pure:submit:attempt") == 1 && countFixtureEvent(t, "management:submit:attempt") == 1, "zero-grant callbacks did not attempt the refused bridge")
		return newParityObservation()
	})
}

func firstItemID(t *testing.T, r *Melta, sessionID string) string {
	t.Helper()
	page, err := r.GetHistory(context.Background(), fixtureCaller(t, r, harnessScope), sessionID, nil)
	testassert.NoError(t, err)
	testassert.Require(t, len(page.Items) != 0, "session %q has no history", sessionID)
	for _, item := range page.Items {
		raw, err := json.Marshal(item)
		testassert.NoError(t, err)
		var members map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal(raw, &members))
		if string(bytes.TrimSpace(members["kind"])) != `"input"` {
			continue
		}
		var value struct {
			ItemId string `json:"item_id"`
		}
		testassert.NoError(t, json.Unmarshal(raw, &value))
		if value.ItemId != "" {
			return value.ItemId
		}
	}
	t.Fatalf("session %q has no input item", sessionID)
	return ""
}

// TestExtensionQueryDeletionAndCancellationParity proves the authoritative
// Session-target query completion: a deleted Session replaces the produced
// query with not-found, while a late request cancellation cannot replace a
// produced result after its Session remains available.
func TestExtensionQueryDeletionAndCancellationParity(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		r, e := openFixtureParity(t, host)
		ctx := context.Background()
		harnessCaller := fixtureCaller(t, r, harnessScope)
		workspace := e.workspace("query")
		selection := &protocol.AgentSelection{Definition: &protocol.DefinitionRef{ExtensionId: "agents", CapabilityId: "agents.definitions", DefinitionId: "fixture_agent"}}
		session, err := r.CreateSession(ctx, harnessCaller, workspace, selection)
		testassert.NoError(t, err)
		_, err = r.ArchiveSession(ctx, harnessCaller, session.SessionId)
		testassert.NoError(t, err)
		sessionCaller := fixtureCaller(t, r, CallerScope{Kind: CallerScopeSession, SessionID: session.SessionId})

		invokeHold := func(hold, release, block, requestID string) (extension.Result, error) {
			arguments, _ := json.Marshal(map[string]string{"hold": hold, "release": release, "block": block})
			return r.InvokeExtension(ctx, sessionCaller, fixtureRequest(fixtureExtensionID, fixtureCommands, "fixture.commands/v1", "query_hold", sessionTarget(session.SessionId), string(arguments), requestID))
		}
		hold, release := filepath.Join(t.TempDir(), "hold"), filepath.Join(t.TempDir(), "release")
		outcomes := make(chan extensionOutcome, 1)
		go func() {
			result, err := invokeHold(hold, release, "", "req-deleted")
			outcomes <- extensionOutcome{result: result, err: err}
		}()
		awaitFile(t, hold)
		_, err = r.DeleteSession(ctx, harnessCaller, session.SessionId)
		testassert.NoError(t, err)
		testassert.NoError(t, os.WriteFile(release, []byte("release"), 0o600))
		var deletion extensionOutcome
		select {
		case deletion = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("parked query never returned")
		}
		testassert.Require(t, errors.Is(deletion.err, harness.ErrNotFound), "query after Session deletion = %v, want not-found", deletion.err)

		// A late cancellation after the handler produced its success leaves the
		// produced result in place.
		session, err = r.CreateSession(ctx, harnessCaller, workspace, selection)
		testassert.NoError(t, err)
		sessionCaller = fixtureCaller(t, r, CallerScope{Kind: CallerScopeSession, SessionID: session.SessionId})
		hold2, release2, block2 := filepath.Join(t.TempDir(), "hold"), filepath.Join(t.TempDir(), "release"), filepath.Join(t.TempDir(), "block")
		cancelCtx, cancel := context.WithCancel(ctx)
		outcomes = make(chan extensionOutcome, 1)
		go func() {
			result, err := r.InvokeExtension(cancelCtx, sessionCaller, fixtureRequest(fixtureExtensionID, fixtureCommands, "fixture.commands/v1", "query_hold", sessionTarget(session.SessionId), `{"hold":"`+hold2+`","release":"`+release2+`","block":"`+block2+`"}`, "req-late-cancel"))
			outcomes <- extensionOutcome{result: result, err: err}
		}()
		awaitFile(t, hold2)
		testassert.NoError(t, os.WriteFile(release2, []byte("release"), 0o600))
		awaitFile(t, block2+".produced")
		cancel()
		testassert.NoError(t, os.WriteFile(block2, []byte("block"), 0o600))
		var late extensionOutcome
		select {
		case late = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("late-cancelled query never returned")
		}
		testassert.Require(t, late.err == nil && late.result.Failure == nil, "late-cancelled query = (%+v, %v), want the produced result", late.result, late.err)
		return newParityObservation()
	})
}

type extensionOutcome struct {
	result extension.Result
	err    error
}

// parkOrOutcome waits for the restore reader to park, but reports an early
// restore result instead of hiding it behind the FIFO deadline.
func parkOrOutcome(t *testing.T, fifo string, outcomes <-chan extensionOutcome) *fifoPark {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			return &fifoPark{writerFD: fd, fifo: fifo}
		}
		select {
		case out := <-outcomes:
			if out.result.Failure != nil {
				t.Fatalf("restore returned before parking: failure=%+v partial=%s err=%v", *out.result.Failure, out.result.Failure.Partial, out.err)
			}
			t.Fatalf("restore returned before parking: result=%+v err=%v", out.result, out.err)
		default:
		}
		testassert.Require(t, errors.Is(err, unix.ENXIO), "FIFO writer open: %v", err)
		testassert.Require(t, !time.Now().After(deadline), "the restore never parked on the FIFO saved original")
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitFile waits for one fixture marker file.
func awaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		testassert.Require(t, !time.Now().After(deadline), "marker %s never appeared", path)
		time.Sleep(5 * time.Millisecond)
	}
}

// TestExtensionRevertPartialParity proves the actual produced reversion
// partial through the public extension path on both transports: the fixed
// revert command preserves the complete accumulated result as the declared
// failure's partial, not a fabricated empty error or a second restore.
func TestExtensionRevertPartialParity(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		r, e := openFixtureParity(t, host)
		e.server.setScript(func(_ context.Context, _ string) []string { return textTurnEvents("done") })
		ctx := context.Background()
		harnessCaller := fixtureCaller(t, r, harnessScope)
		workspace := e.workspace("revert")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSessionNamed(t, r, harnessCaller, workspace)
		first := submitCodeOperation(t, r, session, "op-first", "first")
		second := submitCodeOperation(t, r, session, "op-second", "second")
		awaitRestorableSession(t, r, session)

		// The later-visited (earlier-admitted) group fails on a corrupt meta
		// after the earlier-visited group restores.
		badEntryDir := filepath.Join(codeGroupRoot(r, session, first.Admission.AdmittedEntry.EntryID), "snapshots", "1", "aaaaaaaaaaaaaaaa")
		testassert.NoError(t, os.MkdirAll(badEntryDir, 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(badEntryDir, "meta.json"), []byte("{"), 0o600))
		target := filepath.Join(workspace, "parked.txt")
		testassert.NoError(t, os.WriteFile(target, []byte("current"), 0o600))
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, session, second.Admission.AdmittedEntry.EntryID), target, "current")

		sessionCaller := fixtureCaller(t, r, CallerScope{Kind: CallerScopeSession, SessionID: session})
		outcomes := make(chan extensionOutcome, 1)
		go func() {
			result, err := r.InvokeExtension(ctx, sessionCaller, fixtureRequest(fixtureExtensionID, fixtureCommands, "fixture.commands/v1", "command", sessionTarget(session), `{"method":"revert_code","arguments":{"boundary_operation_id":"op-first"}}`, "req-revert"))
			outcomes <- extensionOutcome{result: result, err: err}
		}()
		park := parkOrOutcome(t, fifo, outcomes)
		defer park.flush()
		park.release("restored")
		var out extensionOutcome
		select {
		case out = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("revert command never returned")
		}
		testassert.Require(t, out.err == nil, "revert extension call = %v, want a declared failure with nil Go error", out.err)
		testassert.Require(t, out.result.Failure != nil && out.result.Failure.Code == "internal" && out.result.Failure.Message != "" && out.result.Value == nil, "revert result = %+v, want the declared internal failure", out.result)
		var partial protocol.CodeRevertResult
		testassert.NoError(t, json.Unmarshal(out.result.Failure.Partial, &partial))
		testassert.Require(t, len(partial.Restored) == 1 && partial.Restored[0] == target && partial.Skipped != nil && partial.Error != nil && partial.Error.Code == protocol.ErrorCodeInternal, "revert partial = %+v, want the parked group's accumulated restore and the traversal error", partial)
		wantPartial := "command:revert-call-error;class=internal;partial=" + fixtureHash(string(out.result.Failure.Partial))
		found := false
		for _, line := range fixtureEvents(t) {
			found = found || line == wantPartial
		}
		testassert.Require(t, found, "author handler did not observe a nonnil CallError with the exact partial: %v", fixtureEvents(t))
		return newParityObservation()
	})
}

func projectionSessionNamed(t *testing.T, r *Melta, caller Caller, workspace string) string {
	t.Helper()
	selection := &protocol.AgentSelection{Definition: &protocol.DefinitionRef{ExtensionId: "agents", CapabilityId: "agents.definitions", DefinitionId: "fixture_agent"}}
	header, err := r.CreateSession(context.Background(), caller, workspace, selection)
	testassert.NoError(t, err)
	return header.SessionId
}

// TestExtensionInvocationCallerOwnership proves the public invocation path
// uses the same owner-bound Caller check as every other producer: a Caller
// issued by another real owner, an unissued Caller with a valid-looking scope,
// and the zero Caller all deny a valid exposed address before any handler or
// command dispatch. The same-owner grant remains the positive control.
func TestExtensionInvocationCallerOwnership(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		r, _ := openFixtureParity(t, host)
		ctx := context.Background()
		harnessCaller := fixtureCaller(t, r, harnessScope)
		request := func(requestID string) protocol.ExtensionInvocationRequest {
			return fixtureRequest(fixtureExtensionID, fixtureService, "fixture.service/v1", "echo", harnessTarget(), `{"ok":true}`, requestID)
		}

		before := countFixtureEvent(t, "service:echo:")
		result, err := r.InvokeExtension(ctx, harnessCaller, request("req-owner"))
		testassert.NoError(t, err)
		testassert.Require(t, result.Failure == nil && string(result.Value) == `{"ok":true}`, "same-owner result = %+v", result)
		testassert.Require(t, countFixtureEvent(t, "service:echo:") == before+1, "same-owner grant did not dispatch")

		foreign, _ := openFixtureParity(t, host)
		foreignCaller := fixtureCaller(t, foreign, harnessScope)
		unissued := Caller{origin: harness.InputOriginUser, scope: harnessScope}
		for _, tc := range []struct {
			name   string
			caller Caller
		}{
			{"foreign owner", foreignCaller},
			{"unissued caller", unissued},
			{"zero caller", Caller{}},
		} {
			before = countFixtureEvent(t, "service:echo:")
			_, err := r.InvokeExtension(ctx, tc.caller, request("req-"+strings.ReplaceAll(tc.name, " ", "-")))
			testassert.Require(t, errors.Is(err, ErrDenied), "%s = %v, want denied", tc.name, err)
			testassert.Require(t, countFixtureEvent(t, "service:echo:") == before, "%s dispatched a handler", tc.name)
		}
		return newParityObservation()
	})
}

func TestCallErrorOwnershipPreservesErrorChain(t *testing.T) {
	for _, joined := range []bool{false, true} {
		partial := json.RawMessage(`{"restored":["a.go"]}`)
		call := &extension.CallError{Err: harness.ErrStorage, Partial: partial}
		var original error = fmt.Errorf("command failed: %w", call)
		if joined {
			original = errors.Join(harness.ErrConflict, original)
		}
		owned := ownCallError(original)
		if owned.Error() != original.Error() || ErrorCode(owned) != ErrorCode(original) || !errors.Is(owned, harness.ErrStorage) || errors.Is(owned, harness.ErrConflict) != joined {
			t.Fatalf("owned error lost its enclosing chain: got %v (%s), want %v (%s)", owned, ErrorCode(owned), original, ErrorCode(original))
		}
		var result *extension.CallError
		if !errors.As(owned, &result) || !bytes.Equal(result.Partial, partial) {
			t.Fatalf("owned error lost its partial: %v", owned)
		}
		partial[0] = '['
		if string(result.Partial) != `{"restored":["a.go"]}` {
			t.Fatalf("owned partial retained caller bytes: %s", result.Partial)
		}
	}
	plain := errors.New("ordinary failure")
	if ownCallError(plain) != plain {
		t.Fatal("ordinary error was replaced")
	}
}

// TestExtensionRevertUnmappedCallErrorParity proves an exposed handler that
// forwards call.Call directly keeps the public command class and the exact
// produced partial as extension.CallError on both native and worker transports.
func TestExtensionRevertUnmappedCallErrorParity(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		r, e := openFixtureParity(t, host)
		e.server.setScript(func(_ context.Context, _ string) []string { return textTurnEvents("done") })
		ctx := context.Background()
		harnessCaller := fixtureCaller(t, r, harnessScope)
		workspace := e.workspace("revert-unmapped")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session := projectionSessionNamed(t, r, harnessCaller, workspace)
		first := submitCodeOperation(t, r, session, "op-first", "first")
		second := submitCodeOperation(t, r, session, "op-second", "second")
		awaitRestorableSession(t, r, session)

		badEntryDir := filepath.Join(codeGroupRoot(r, session, first.Admission.AdmittedEntry.EntryID), "snapshots", "1", "aaaaaaaaaaaaaaaa")
		testassert.NoError(t, os.MkdirAll(badEntryDir, 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(badEntryDir, "meta.json"), []byte("{"), 0o600))
		target := filepath.Join(workspace, "parked.txt")
		testassert.NoError(t, os.WriteFile(target, []byte("current"), 0o600))
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, session, second.Admission.AdmittedEntry.EntryID), target, "current")

		sessionCaller := fixtureCaller(t, r, CallerScope{Kind: CallerScopeSession, SessionID: session})
		outcomes := make(chan extensionOutcome, 1)
		go func() {
			result, err := r.InvokeExtension(ctx, sessionCaller, fixtureRequest(fixtureExtensionID, fixtureCommands, "fixture.commands/v1", "command_unmapped", sessionTarget(session), `{"method":"revert_code","arguments":{"boundary_operation_id":"op-first"}}`, "req-revert-unmapped"))
			outcomes <- extensionOutcome{result: result, err: err}
		}()
		park := parkOrOutcome(t, fifo, outcomes)
		defer park.flush()
		park.release("restored")
		var out extensionOutcome
		select {
		case out = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("unmapped revert command never returned")
		}
		var callErr *extension.CallError
		testassert.Require(t, errors.As(out.err, &callErr) && callErr.Partial != nil && out.result.Failure == nil, "unmapped revert = (%+v, %v), want extension.CallError with a partial", out.result, out.err)
		testassert.Require(t, ErrorCode(callErr) == protocol.ErrorCodeInternal && callErr.Err != nil, "unmapped revert class = %q cause = %v, want the original internal class and cause", ErrorCode(callErr), callErr.Err)
		var partial protocol.CodeRevertResult
		testassert.NoError(t, json.Unmarshal(callErr.Partial, &partial))
		testassert.Require(t, len(partial.Restored) == 1 && partial.Restored[0] == target && partial.Skipped != nil && partial.Error != nil && partial.Error.Code == protocol.ErrorCodeInternal, "unmapped partial = %+v", partial)
		remarshaled, err := json.Marshal(partial)
		testassert.NoError(t, err)
		testassert.Require(t, bytes.Equal(callErr.Partial, remarshaled), "unmapped partial bytes changed: %q vs %q", callErr.Partial, remarshaled)
		return newParityObservation()
	})
}

// TestExtensionCallErrorPartialValidationParity proves native and worker
// call-error forwarding preserves valid multiline/large-number, null, empty
// and wrapped causes exactly; only actual malformed native output is refused
// as the existing extension invalid-output sentinel.
func TestExtensionCallErrorPartialValidationParity(t *testing.T) {
	validPartial := "{\n  \"big\": 9007199254740993,\n  \"text\": \"line\\\\nbreak\"\n}"
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		r, _ := openFixtureParity(t, host)
		invoke := func(mode string) (extension.Result, error) {
			return r.InvokeExtension(context.Background(), fixtureCaller(t, r, harnessScope), fixtureRequest(fixtureExtensionID, fixtureService, "fixture.service/v1", "call_error", harnessTarget(), `{"mode":"`+mode+`"}`, "req-call-error-"+mode))
		}
		for _, tc := range []struct {
			mode    string
			partial string
			typed   bool
			wrapped bool
		}{
			{"valid", validPartial, true, false},
			{"null", "null", true, false},
			{"empty", "", false, false},
			{"wrapped", validPartial, true, true},
		} {
			result, err := invoke(tc.mode)
			var callErr *extension.CallError
			testassert.Require(t, result.Failure == nil && err != nil, "%s result = (%+v, %v)", tc.mode, result, err)
			testassert.Require(t, errors.Is(err, harness.ErrStorage) && ErrorCode(err) == protocol.ErrorCodeStorage, "%s class = %q", tc.mode, ErrorCode(err))
			if !tc.typed {
				if errors.As(err, &callErr) {
					testassert.Require(t, callErr.Partial == nil, "%s partial = %q, want absent", tc.mode, callErr.Partial)
				}
				continue
			}
			testassert.Require(t, errors.As(err, &callErr), "%s error = %v, want CallError", tc.mode, err)
			testassert.Require(t, string(callErr.Partial) == tc.partial, "%s partial = %q, want %q", tc.mode, callErr.Partial, tc.partial)
			if tc.wrapped {
				testassert.Require(t, strings.Contains(callErr.Err.Error(), "wrapped:"), "wrapped cause = %v", callErr.Err)
			}
		}
		return newParityObservation()
	})
}

// TestExtensionMalformedCallErrorPartialRefused proves native malformed call
// output is rejected at acceptance as the extension invalid-output sentinel,
// never an exposed CallError carrying invalid bytes. Worker malformed output
// retains its existing terminal refusal.
func TestExtensionMalformedCallErrorPartialRefused(t *testing.T) {
	r, _ := openFixtureParity(t, parityHost{name: "native"})
	result, err := r.InvokeExtension(context.Background(), fixtureCaller(t, r, harnessScope), fixtureRequest(fixtureExtensionID, fixtureService, "fixture.service/v1", "call_error", harnessTarget(), `{"mode":"bad"}`, "req-bad-call-error"))
	var callErr *extension.CallError
	testassert.Require(t, result.Failure == nil && err != nil && !errors.As(err, &callErr), "malformed call error = (%+v, %v), want no CallError", result, err)
	testassert.Require(t, errors.Is(err, extension.ErrInvalid), "malformed call error = %v, want the extension invalid-output sentinel", err)
}
