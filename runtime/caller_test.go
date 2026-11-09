package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// TestCallerValidation proves the trusted grant shape: the origin is closed
// with provenance exactly for plugin, and the scope is exactly one concrete
// target union member.
func TestCallerValidation(t *testing.T) {
	workspace := "/tmp/caller-workspace"
	sessionID := "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name  string
		grant callerContext
		valid bool
	}{
		{"user harness", callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeHarness}}, true},
		{"runtime harness", callerContext{Origin: harness.InputOriginRuntime, Scope: callerScope{kind: callerScopeHarness}}, true},
		{"plugin harness", callerContext{Origin: harness.InputOriginPlugin, ExtensionID: "ext", Scope: callerScope{kind: callerScopeHarness}}, true},
		{"workspace", callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeWorkspace, workspace: workspace}}, true},
		{"session", callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeSession, sessionID: sessionID}}, true},
		{"plugin without extension", callerContext{Origin: harness.InputOriginPlugin, Scope: callerScope{kind: callerScopeHarness}}, false},
		{"user with extension", callerContext{Origin: harness.InputOriginUser, ExtensionID: "ext", Scope: callerScope{kind: callerScopeHarness}}, false},
		{"runtime with extension", callerContext{Origin: harness.InputOriginRuntime, ExtensionID: "ext", Scope: callerScope{kind: callerScopeHarness}}, false},
		{"unknown origin", callerContext{Origin: harness.InputOrigin("agent"), Scope: callerScope{kind: callerScopeHarness}}, false},
		{"harness with target", callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeHarness, sessionID: sessionID}}, false},
		{"relative workspace", callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeWorkspace, workspace: "relative"}}, false},
		{"unclean workspace", callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeWorkspace, workspace: "/tmp/../tmp/ws"}}, false},
		{"malformed session", callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeSession, sessionID: "short"}}, false},
		{"session with workspace", callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeSession, sessionID: sessionID, workspace: workspace}}, false},
		{"unknown scope", callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: "owner"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newCaller(tc.grant)
			if tc.valid {
				testassert.Require(t, err == nil && got.origin == tc.grant.Origin && got.extensionID == tc.grant.ExtensionID && got.scope == tc.grant.Scope, "newCaller(%+v) = (%+v, %v), want the granted caller", tc.grant, got, err)
				return
			}
			testassert.Require(t, err != nil && errors.Is(err, harness.ErrInvalid), "newCaller(%+v) = (%+v, %v), want harness.ErrInvalid", tc.grant, got, err)
		})
	}
}

// TestCallerScopeDeniesBeforePublish proves target scope authority on the
// shared command producers: a Session or Workspace grant outside its target
// is denied before any Harness transition, while matching grants admit
// through the same producer. The private caller types never accept request
// data as a grant.
func TestCallerScopeDeniesBeforePublish(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)

		workspaceA := filepath.Join(e.home, "scope-a")
		workspaceB := filepath.Join(e.home, "scope-b")
		sessionA := projectionSession(t, r, workspaceA, "solo").Identity.SessionID
		sessionB := projectionSession(t, r, workspaceB, "solo").Identity.SessionID

		sessionCaller, err := newCaller(callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeSession, sessionID: sessionA}})
		testassert.NoError(t, err)
		workspaceCaller, err := newCaller(callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeWorkspace, workspace: workspaceA}})
		testassert.NoError(t, err)
		otherWorkspaceCaller, err := newCaller(callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeWorkspace, workspace: workspaceB}})
		testassert.NoError(t, err)

		for name, row := range map[string]struct {
			caller  caller
			session string
		}{
			"session caller on another session":     {sessionCaller, sessionB},
			"workspace caller on another workspace": {workspaceCaller, sessionB},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := r.submitSession(ctx, row.caller, row.session, commandSubmitRequest(t, "op-denied", "denied"))
				testassert.Require(t, errors.Is(err, ErrDenied), "submit = %v, want ErrDenied", err)
				code, status := classifyProtocolError(err)
				testassert.Require(t, code == protocol.ErrorCodeDenied && status == http.StatusForbidden, "classify = (%s, %d), want denied 403", code, status)
				view := sessionViewThroughRuntime(t, r, row.session)
				testassert.Require(t, len(view.Operations) == 0 && view.Header.CurrentOperationID == "" && len(view.Steering) == 0 && len(view.Queued) == 0, "denied submit published work: %+v", view)
			})
		}
		{
			_, err := r.forkSession(ctx, otherWorkspaceCaller, sessionA, protocol.ForkRequest{OperationId: "op-denied-fork", BoundaryItemId: "x", Content: []protocol.ContentPart{textContentPart(t, "fork", nil)}})
			testassert.Require(t, errors.Is(err, ErrDenied), "fork = %v, want ErrDenied", err)
		}

		// Matching grants of both narrowed kinds use the same producers.
		{
			result, err := r.submitSession(ctx, workspaceCaller, sessionA, commandSubmitRequest(t, "op-workspace", "workspace"))
			testassert.NoError(t, err)
			testassert.Require(t, result.Operation != nil && result.Operation.OperationId == "op-workspace", "workspace submit = %+v, want admitted op-workspace", result)
			awaitOperation(t, r, sessionA, "op-workspace", harness.OperationSuccess)
			awaitIdleSession(t, r, sessionA)
		}
		{
			result, err := r.submitSession(ctx, sessionCaller, sessionA, commandSubmitRequest(t, "op-session", "session"))
			testassert.NoError(t, err)
			testassert.Require(t, result.Operation != nil && result.Operation.OperationId == "op-session", "session submit = %+v, want admitted op-session", result)
			awaitOperation(t, r, sessionA, "op-session", harness.OperationSuccess)
			awaitIdleSession(t, r, sessionA)
		}
	})
}

// TestProducerScopeAuthority proves the one target rule on every producer
// reachable from the HTTP handlers: the owner requires harness scope, a
// Workspace accepts harness or that Workspace, and a Session accepts harness,
// its own Session, or its owning Workspace. Every other grant is denied
// before the producer reads or transitions, leaving configuration and every
// Session exactly unchanged.
func TestProducerScopeAuthority(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		counting := &countingStore{Storage: store}
		r, e := openProjectionRuntime(t, counting)
		defer closeProjectionRuntime(r)

		workspaceA := filepath.Join(e.home, "scope-a")
		workspaceB := filepath.Join(e.home, "scope-b")
		sessionA := projectionSession(t, r, workspaceA, "solo").Identity.SessionID
		sessionB := projectionSession(t, r, workspaceB, "solo").Identity.SessionID
		archivedID := projectionSession(t, r, workspaceA, "solo").Identity.SessionID
		{
			_, err := r.archiveSession(ctx, protocolCaller(), archivedID)
			testassert.NoError(t, err)
		}
		testassert.NoError(t, os.MkdirAll(workspaceA, 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(workspaceA, "note.txt"), []byte("note"), 0o600))
		workspaceOf := map[string]string{sessionA: workspaceA, sessionB: workspaceB, archivedID: workspaceA}

		ownerRows := []struct {
			name string
			run  func(c caller) error
		}{
			{"reload", func(c caller) error { _, err := r.reload(ctx, c); return err }},
			{"get configuration", func(c caller) error { _, err := r.getConfiguration(ctx, c); return err }},
			{"update settings", func(c caller) error { _, err := r.updateSettings(ctx, c, protocol.UpdateSettingsRequest{}); return err }},
			{"list models", func(c caller) error { _, err := r.listModels(ctx, c, protocol.ListModelsParams{}); return err }},
			{"list providers", func(c caller) error { _, err := r.listProviders(ctx, c); return err }},
			{"add provider", func(c caller) error {
				_, err := r.addProvider(ctx, c, "", protocol.ProviderEdit{}, nil, nil)
				return err
			}},
			{"get provider", func(c caller) error { _, err := r.getProvider(ctx, c, "ghost"); return err }},
			{"update provider", func(c caller) error {
				_, err := r.updateProvider(ctx, c, "ghost", protocol.ProviderEdit{})
				return err
			}},
			{"delete provider", func(c caller) error { _, err := r.deleteProvider(ctx, c, "ghost"); return err }},
			{"discover provider", func(c caller) error { _, err := r.discoverProvider(ctx, c, protocol.DiscoveryRequest{}); return err }},
			{"connect provider", func(c caller) error { _, err := r.connectProvider(ctx, c, "ghost", nil); return err }},
			{"disconnect provider", func(c caller) error { _, err := r.disconnectProvider(ctx, c, "ghost"); return err }},
			{"list provider models", func(c caller) error { _, err := r.listProviderModels(ctx, c, "ghost"); return err }},
			{"save model", func(c caller) error {
				_, err := r.saveModel(ctx, c, "ghost", "model", protocol.ModelEdit{})
				return err
			}},
			{"delete model", func(c caller) error { _, err := r.deleteModel(ctx, c, "ghost", "model"); return err }},
			{"discover provider models", func(c caller) error { _, err := r.discoverProviderModels(ctx, c, "ghost"); return err }},
			{"reset provider field", func(c caller) error {
				_, err := r.resetProviderField(ctx, c, "ghost", protocol.ProviderFieldName)
				return err
			}},
			{"reset model field", func(c caller) error {
				_, err := r.resetModelField(ctx, c, "ghost", "model", protocol.ModelFieldName)
				return err
			}},
			{"get warnings", func(c caller) error { _, err := r.getWarnings(ctx, c); return err }},
			{"list workspaces", func(c caller) error { _, err := r.listWorkspaces(ctx, c); return err }},
			{"subscribe events", func(c caller) error {
				sub, release, err := r.subscribeEvents(ctx, c)
				if err != nil {
					return err
				}
				sub.Close()
				release()
				return nil
			}},
		}
		workspaceRows := []struct {
			name string
			run  func(c caller) error
		}{
			{"create session", func(c caller) error {
				_, err := r.createSessionHeader(ctx, c, workspaceA, nil)
				return err
			}},
			{"list sessions workspace", func(c caller) error {
				_, err := r.listSessions(ctx, c, protocol.ListSessionsParams{Workspace: workspaceA, Lifecycle: protocol.Open})
				return err
			}},
			{"read workspace file", func(c caller) error {
				_, err := r.readWorkspaceFile(ctx, c, protocol.ReadFileRequest{Workspace: workspaceA, Path: "note.txt"})
				return err
			}},
		}
		sessionRows := []struct {
			name   string
			target string
			run    func(c caller) error
		}{
			{"set session model", sessionA, func(c caller) error {
				_, err := r.setSessionModel(ctx, c, sessionA, protocol.SetSessionModelRequest{})
				return err
			}},
			{"delete session", sessionA, func(c caller) error { _, err := r.deleteSession(ctx, c, sessionA); return err }},
			{"archive session", archivedID, func(c caller) error { _, err := r.archiveSession(ctx, c, archivedID); return err }},
			{"reopen session", sessionA, func(c caller) error { _, err := r.reopenSession(ctx, c, sessionA); return err }},
			{"submit session", archivedID, func(c caller) error {
				_, err := r.submitSession(ctx, c, archivedID, commandSubmitRequest(t, "scope-submit", "x"))
				return err
			}},
			{"compact session", sessionA, func(c caller) error {
				_, err := r.compactSession(ctx, c, sessionA, protocol.CompactRequest{})
				return err
			}},
			{"interrupt session", sessionA, func(c caller) error { return r.interruptSession(ctx, c, sessionA) }},
			{"stop session", sessionA, func(c caller) error { return r.stopSession(ctx, c, sessionA) }},
			{"fork session", sessionA, func(c caller) error {
				_, err := r.forkSession(ctx, c, sessionA, protocol.ForkRequest{OperationId: "scope-fork", BoundaryItemId: "missing", Content: []protocol.ContentPart{textContentPart(t, "fork", nil)}})
				return err
			}},
			{"set session agent type", sessionA, func(c caller) error {
				_, err := r.setSessionAgentType(ctx, c, sessionA, protocol.SetSessionAgentTypeRequest{})
				return err
			}},
			{"build hydration", sessionA, func(c caller) error { _, err := r.buildHydration(ctx, c, sessionA); return err }},
			{"get history", sessionA, func(c caller) error { _, err := r.getHistory(ctx, c, sessionA, nil); return err }},
			{"list code snapshots", sessionA, func(c caller) error { _, err := r.listSessionCodeSnapshots(ctx, c, sessionA); return err }},
			{"revert session code", sessionA, func(c caller) error {
				_, err := r.revertSessionCode(ctx, c, sessionA, "missing-operation")
				return err
			}},
		}

		workspaceCaller, err := newCaller(callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeWorkspace, workspace: workspaceA}})
		testassert.NoError(t, err)
		otherWorkspaceCaller, err := newCaller(callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeWorkspace, workspace: workspaceB}})
		testassert.NoError(t, err)
		sessionCaller := func(id string) caller {
			c, err := newCaller(callerContext{Origin: harness.InputOriginUser, Scope: callerScope{kind: callerScopeSession, sessionID: id}})
			testassert.NoError(t, err)
			return c
		}

		state := func() string {
			var rows []harness.SessionHeader
			err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				var err error
				rows, err = h.ListSessions(ctx)
				return err
			})
			testassert.NoError(t, err)
			var snap []string
			for _, row := range rows {
				snap = append(snap, fmt.Sprintf("%s/%d/%s/%s", row.Identity.SessionID, row.Revision.DurableRevision, row.Lifecycle, row.CurrentOperationID))
			}
			return fmt.Sprintf("g%d|%v", r.config.current().generation, snap)
		}
		type row struct {
			name   string
			target callerScope
			run    func(c caller) error
		}
		var rows []row
		for _, item := range ownerRows {
			rows = append(rows, row{name: item.name, target: harnessScope, run: item.run})
		}
		for _, item := range workspaceRows {
			rows = append(rows, row{name: item.name, target: callerScope{kind: callerScopeWorkspace, workspace: workspaceA}, run: item.run})
		}
		for _, item := range sessionRows {
			rows = append(rows, row{name: item.name, target: callerScope{kind: callerScopeSession, sessionID: item.target}, run: item.run})
		}
		for _, item := range rows {
			t.Run(item.name, func(t *testing.T) {
				otherSession := sessionA
				if item.target.kind == callerScopeSession && item.target.sessionID == sessionA {
					otherSession = sessionB
				}
				matchingSession := sessionA
				if item.target.kind == callerScopeSession {
					matchingSession = item.target.sessionID
				}
				callers := []struct {
					name    string
					c       caller
					allowed bool
				}{
					{"harness", protocolCaller(), true},
					{"workspace", workspaceCaller, item.target.kind == callerScopeWorkspace && item.target.workspace == workspaceA || item.target.kind == callerScopeSession && workspaceOf[item.target.sessionID] == workspaceA},
					{"other workspace", otherWorkspaceCaller, item.target.kind == callerScopeWorkspace && item.target.workspace == workspaceB || item.target.kind == callerScopeSession && workspaceOf[item.target.sessionID] == workspaceB},
					{"matching session", sessionCaller(matchingSession), item.target.kind == callerScopeSession},
					{"other session", sessionCaller(otherSession), false},
				}
				for _, candidate := range callers {
					t.Run(candidate.name, func(t *testing.T) {
						if candidate.allowed {
							err := item.run(candidate.c)
							testassert.Require(t, !errors.Is(err, ErrDenied), "allowed producer = %v, want the producer reached", err)
							return
						}
						before := state()
						lists := counting.listCount()
						err := item.run(candidate.c)
						testassert.Require(t, errors.Is(err, ErrDenied), "denied producer = %v, want ErrDenied", err)
						code, status := classifyProtocolError(err)
						testassert.Require(t, code == protocol.ErrorCodeDenied && status == http.StatusForbidden, "classify = (%s, %d), want denied 403", code, status)
						testassert.Require(t, counting.listCount() == lists, "denied producer enumerated Sessions")
						testassert.Require(t, state() == before, "denied producer changed owner or Session state")
					})
				}
			})
		}
	})
}

// TestPluginCallerProvenanceCrossesTheProducer proves plugin attribution
// supplied by a trusted caller reaches the committed input and both generated
// projections with no wire field involved.
func TestPluginCallerProvenanceCrossesTheProducer(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		sessionID := projectionSession(t, r, filepath.Join(e.home, "plugin-provenance"), "solo").Identity.SessionID
		pluginCaller, err := newCaller(callerContext{Origin: harness.InputOriginPlugin, ExtensionID: "calling.extension", Scope: callerScope{kind: callerScopeHarness}})
		testassert.NoError(t, err)
		gate := make(chan struct{})
		e.server.setHold(gate)
		release := sync.OnceFunc(func() { close(gate) })
		defer release()
		{
			_, err := r.submitSession(ctx, pluginCaller, sessionID, commandSubmitRequest(t, "op-plugin", "plugin work"))
			testassert.NoError(t, err)
		}
		awaitModelArrival(t, e)
		{
			result, err := r.submitSession(ctx, pluginCaller, sessionID, commandSubmitRequest(t, "op-buffered", "buffered work"))
			testassert.NoError(t, err)
			testassert.Require(t, result.Disposition == protocol.SubmitResultDispositionSteering, "buffered submit = %s, want steering", result.Disposition)
		}
		{
			hydration, err := r.buildHydration(ctx, protocolCaller(), sessionID)
			testassert.NoError(t, err)
			testassert.Require(t, len(hydration.Pending.Steering) == 1 && hydration.Pending.Steering[0].ExtensionId != nil && *hydration.Pending.Steering[0].ExtensionId == "calling.extension", "pending steering = %+v, want the plugin extension", hydration.Pending.Steering)
		}
		release()
		awaitOperation(t, r, sessionID, "op-plugin", harness.OperationSuccess)
		awaitOperation(t, r, sessionID, "op-buffered", harness.OperationSuccess)
		awaitIdleSession(t, r, sessionID)
		view := sessionViewThroughRuntime(t, r, sessionID)
		testassert.Require(t, len(view.Queued) == 0, "unexpected queued buffer: %+v", view.Queued)
		fact := commandInputFact(t, view, "op-plugin")
		testassert.Require(t, fact.Input.Origin == harness.InputOriginPlugin && fact.Input.ExtensionID == "calling.extension", "committed input = %+v, want plugin/calling.extension", fact.Input)
		{
			hydration, err := r.buildHydration(ctx, protocolCaller(), sessionID)
			testassert.NoError(t, err)
			var item protocol.InputItem
			found := false
			for _, candidate := range hydration.Conversation.Items {
				kind, err := candidate.Discriminator()
				if err != nil || kind != "input" {
					continue
				}
				input, err := candidate.AsInputItem()
				if err != nil {
					continue
				}
				if input.OperationId != nil && *input.OperationId == "op-plugin" {
					item, found = input, true
				}
			}
			testassert.Require(t, found && item.ExtensionId != nil && *item.ExtensionId == "calling.extension", "hydrated input = %+v (found %v), want the plugin extension", item, found)
		}
	})
}

// TestProtocolSubmitRejectsForgedOriginFields proves the wire cannot supply
// attribution: an otherwise well-shaped Submit or Fork body carrying origin
// or extension members fails the strict request decode as invalid and
// publishes nothing.
func TestProtocolSubmitRejectsForgedOriginFields(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		sessionID := projectionSession(t, r, filepath.Join(e.home, "forged"), "solo").Identity.SessionID
		for name, row := range map[string]struct {
			path string
			body string
		}{
			"submit origin": {
				fmt.Sprintf("/v1/sessions/%s/submit", sessionID),
				`{"operation_id":"op-forged","mode":"regular","origin":"plugin","content":[{"kind":"text","text":"forged"}]}`,
			},
			"submit extension": {
				fmt.Sprintf("/v1/sessions/%s/submit", sessionID),
				`{"operation_id":"op-forged","mode":"regular","extension_id":"ext","content":[{"kind":"text","text":"forged"}]}`,
			},
			"fork origin": {
				fmt.Sprintf("/v1/sessions/%s/fork", sessionID),
				`{"operation_id":"op-forged","origin":"plugin","boundary_item_id":"item","content":[{"kind":"text","text":"forged"}]}`,
			},
		} {
			t.Run(name, func(t *testing.T) {
				response := rawProtocol(t, http.MethodPost, protocolTarget(ps, row.path), ps.credential, row.body)
				typed := decodeProtocolError(t, response)
				testassert.Require(t, response.StatusCode == http.StatusBadRequest && typed.Code == protocol.ErrorCodeInvalid, "%s = (%d, %+v), want the typed invalid 400", name, response.StatusCode, typed)
			})
		}
		view := sessionViewThroughRuntime(t, r, sessionID)
		testassert.Require(t, len(view.Operations) == 0 && view.Header.CurrentOperationID == "", "forged requests published work: %+v", view)
	})
}
