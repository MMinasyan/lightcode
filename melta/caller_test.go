package melta

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// TestCallerValidation proves the trusted grant shape: the origin is closed
// with provenance exactly for plugin, and the scope is exactly one concrete
// target union member.
func TestCallerValidation(t *testing.T) {
	workspace := "/tmp/Caller-workspace"
	sessionID := "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name  string
		grant CallerContext
		valid bool
	}{
		{"user harness", CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeHarness}}, true},
		{"runtime harness", CallerContext{Origin: harness.InputOriginRuntime, Scope: CallerScope{Kind: CallerScopeHarness}}, true},
		{"plugin harness", CallerContext{Origin: harness.InputOriginPlugin, ExtensionID: "ext", Scope: CallerScope{Kind: CallerScopeHarness}}, true},
		{"workspace", CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeWorkspace, Workspace: workspace}}, true},
		{"session", CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeSession, SessionID: sessionID}}, true},
		{"plugin without extension", CallerContext{Origin: harness.InputOriginPlugin, Scope: CallerScope{Kind: CallerScopeHarness}}, false},
		{"user with extension", CallerContext{Origin: harness.InputOriginUser, ExtensionID: "ext", Scope: CallerScope{Kind: CallerScopeHarness}}, false},
		{"runtime with extension", CallerContext{Origin: harness.InputOriginRuntime, ExtensionID: "ext", Scope: CallerScope{Kind: CallerScopeHarness}}, false},
		{"unknown origin", CallerContext{Origin: harness.InputOrigin("agent"), Scope: CallerScope{Kind: CallerScopeHarness}}, false},
		{"harness with target", CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeHarness, SessionID: sessionID}}, false},
		{"relative workspace", CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeWorkspace, Workspace: "relative"}}, false},
		{"unclean workspace", CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeWorkspace, Workspace: "/tmp/../tmp/ws"}}, false},
		{"malformed session", CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeSession, SessionID: "short"}}, false},
		{"session with workspace", CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeSession, SessionID: sessionID, Workspace: workspace}}, false},
		{"unknown scope", CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: "owner"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newCaller(tc.grant)
			if tc.valid {
				testassert.Require(t, err == nil && got.origin == tc.grant.Origin && got.extensionID == tc.grant.ExtensionID && got.scope == tc.grant.Scope, "newCaller(%+v) = (%+v, %v), want the granted Caller", tc.grant, got, err)
				return
			}
			testassert.Require(t, err != nil && errors.Is(err, harness.ErrInvalid), "newCaller(%+v) = (%+v, %v), want harness.ErrInvalid", tc.grant, got, err)
		})
	}
}

// TestCallerScopeDeniesBeforePublish proves target scope authority on the
// shared command producers: a Session or Workspace grant outside its target
// is denied before any Harness transition, while matching grants admit
// through the same producer. The private Caller types never accept request
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

		sessionCaller, err := newCaller(CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeSession, SessionID: sessionA}})
		testassert.NoError(t, err)
		workspaceCaller, err := newCaller(CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeWorkspace, Workspace: workspaceA}})
		testassert.NoError(t, err)
		otherWorkspaceCaller, err := newCaller(CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeWorkspace, Workspace: workspaceB}})
		testassert.NoError(t, err)

		for name, row := range map[string]struct {
			Caller  Caller
			session string
		}{
			"session Caller on another session":     {sessionCaller, sessionB},
			"workspace Caller on another workspace": {workspaceCaller, sessionB},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := r.SubmitSession(ctx, row.Caller, row.session, commandSubmitRequest(t, "op-denied", "denied"))
				testassert.Require(t, errors.Is(err, ErrDenied), "submit = %v, want ErrDenied", err)
				testassert.Require(t, ErrorCode(err) == protocol.ErrorCodeDenied, "classify = %s, want denied", ErrorCode(err))
				view := sessionViewThroughRuntime(t, r, row.session)
				testassert.Require(t, len(view.Operations) == 0 && view.Header.CurrentOperationID == "" && len(view.Steering) == 0 && len(view.Queued) == 0, "denied submit published work: %+v", view)
			})
		}
		{
			_, err := r.ForkSession(ctx, otherWorkspaceCaller, sessionA, protocol.ForkRequest{OperationId: "op-denied-fork", BoundaryItemId: "x", Content: []protocol.ContentPart{textContentPart(t, "fork", nil)}})
			testassert.Require(t, errors.Is(err, ErrDenied), "fork = %v, want ErrDenied", err)
		}

		// Matching grants of both narrowed kinds use the same producers.
		{
			result, err := r.SubmitSession(ctx, workspaceCaller, sessionA, commandSubmitRequest(t, "op-workspace", "workspace"))
			testassert.NoError(t, err)
			testassert.Require(t, result.Operation != nil && result.Operation.OperationId == "op-workspace", "workspace submit = %+v, want admitted op-workspace", result)
			awaitOperation(t, r, sessionA, "op-workspace", harness.OperationSuccess)
			awaitIdleSession(t, r, sessionA)
		}
		{
			result, err := r.SubmitSession(ctx, sessionCaller, sessionA, commandSubmitRequest(t, "op-session", "session"))
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
			_, err := r.ArchiveSession(ctx, testCaller(), archivedID)
			testassert.NoError(t, err)
		}
		testassert.NoError(t, os.MkdirAll(workspaceA, 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(workspaceA, "note.txt"), []byte("note"), 0o600))
		workspaceOf := map[string]string{sessionA: workspaceA, sessionB: workspaceB, archivedID: workspaceA}

		ownerRows := []struct {
			name string
			run  func(c Caller) error
		}{
			{"reload", func(c Caller) error { _, err := r.Reload(ctx, c); return err }},
			{"get configuration", func(c Caller) error { _, err := r.GetConfiguration(ctx, c); return err }},
			{"update settings", func(c Caller) error { _, err := r.UpdateSettings(ctx, c, protocol.UpdateSettingsRequest{}); return err }},
			{"list models", func(c Caller) error { _, err := r.ListModels(ctx, c, protocol.ListModelsParams{}); return err }},
			{"list providers", func(c Caller) error { _, err := r.ListProviders(ctx, c); return err }},
			{"add provider", func(c Caller) error {
				_, err := r.AddProvider(ctx, c, "", protocol.ProviderEdit{}, nil, nil)
				return err
			}},
			{"get provider", func(c Caller) error { _, err := r.GetProvider(ctx, c, "ghost"); return err }},
			{"update provider", func(c Caller) error {
				_, err := r.UpdateProvider(ctx, c, "ghost", protocol.ProviderEdit{})
				return err
			}},
			{"delete provider", func(c Caller) error { _, err := r.DeleteProvider(ctx, c, "ghost"); return err }},
			{"discover provider", func(c Caller) error { _, err := r.DiscoverProvider(ctx, c, protocol.DiscoveryRequest{}); return err }},
			{"connect provider", func(c Caller) error { _, err := r.ConnectProvider(ctx, c, "ghost", nil); return err }},
			{"disconnect provider", func(c Caller) error { _, err := r.DisconnectProvider(ctx, c, "ghost"); return err }},
			{"list provider models", func(c Caller) error { _, err := r.ListProviderModels(ctx, c, "ghost"); return err }},
			{"save model", func(c Caller) error {
				_, err := r.SaveModel(ctx, c, "ghost", "model", protocol.ModelEdit{})
				return err
			}},
			{"delete model", func(c Caller) error { _, err := r.DeleteModel(ctx, c, "ghost", "model"); return err }},
			{"discover provider models", func(c Caller) error { _, err := r.DiscoverProviderModels(ctx, c, "ghost"); return err }},
			{"reset provider field", func(c Caller) error {
				_, err := r.ResetProviderField(ctx, c, "ghost", protocol.ProviderFieldName)
				return err
			}},
			{"reset model field", func(c Caller) error {
				_, err := r.ResetModelField(ctx, c, "ghost", "model", protocol.ModelFieldName)
				return err
			}},
			{"get warnings", func(c Caller) error { _, err := r.GetWarnings(ctx, c); return err }},
			{"list workspaces", func(c Caller) error { _, err := r.ListWorkspaces(ctx, c); return err }},
		}
		workspaceRows := []struct {
			name string
			run  func(c Caller) error
		}{
			{"create session", func(c Caller) error {
				_, err := r.CreateSession(ctx, c, workspaceA, nil)
				return err
			}},
			{"list sessions workspace", func(c Caller) error {
				_, err := r.ListSessions(ctx, c, protocol.ListSessionsParams{Workspace: workspaceA, Lifecycle: protocol.Open})
				return err
			}},
			{"read workspace file", func(c Caller) error {
				_, err := r.ReadWorkspaceFile(ctx, c, protocol.ReadFileRequest{Workspace: workspaceA, Path: "note.txt"})
				return err
			}},
		}
		sessionRows := []struct {
			name   string
			target string
			run    func(c Caller) error
		}{
			{"set session model", sessionA, func(c Caller) error {
				_, err := r.SetSessionModel(ctx, c, sessionA, "")
				return err
			}},
			{"delete session", sessionA, func(c Caller) error { _, err := r.DeleteSession(ctx, c, sessionA); return err }},
			{"archive session", archivedID, func(c Caller) error { _, err := r.ArchiveSession(ctx, c, archivedID); return err }},
			{"reopen session", sessionA, func(c Caller) error { _, err := r.ReopenSession(ctx, c, sessionA); return err }},
			{"submit session", archivedID, func(c Caller) error {
				_, err := r.SubmitSession(ctx, c, archivedID, commandSubmitRequest(t, "scope-submit", "x"))
				return err
			}},
			{"compact session", sessionA, func(c Caller) error {
				_, err := r.CompactSession(ctx, c, sessionA, protocol.CompactRequest{})
				return err
			}},
			{"interrupt session", sessionA, func(c Caller) error { return r.InterruptSession(ctx, c, sessionA) }},
			{"stop session", sessionA, func(c Caller) error { return r.StopSession(ctx, c, sessionA) }},
			{"fork session", sessionA, func(c Caller) error {
				_, err := r.ForkSession(ctx, c, sessionA, protocol.ForkRequest{OperationId: "scope-fork", BoundaryItemId: "missing", Content: []protocol.ContentPart{textContentPart(t, "fork", nil)}})
				return err
			}},
			{"set session agent type", sessionA, func(c Caller) error {
				_, err := r.SetSessionAgentType(ctx, c, sessionA, protocol.SetSessionAgentTypeRequest{})
				return err
			}},
			{"build hydration", sessionA, func(c Caller) error { _, err := r.GetHydration(ctx, c, sessionA); return err }},
			{"get history", sessionA, func(c Caller) error { _, err := r.GetHistory(ctx, c, sessionA, nil); return err }},
			{"list code snapshots", sessionA, func(c Caller) error { _, err := r.ListSessionCodeSnapshots(ctx, c, sessionA); return err }},
			{"revert session code", sessionA, func(c Caller) error {
				_, err := r.RevertSessionCode(ctx, c, sessionA, "missing-operation")
				return err
			}},
		}

		workspaceCaller, err := newCaller(CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeWorkspace, Workspace: workspaceA}})
		testassert.NoError(t, err)
		otherWorkspaceCaller, err := newCaller(CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeWorkspace, Workspace: workspaceB}})
		testassert.NoError(t, err)
		sessionCaller := func(id string) Caller {
			c, err := newCaller(CallerContext{Origin: harness.InputOriginUser, Scope: CallerScope{Kind: CallerScopeSession, SessionID: id}})
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
			target CallerScope
			run    func(c Caller) error
		}
		var rows []row
		for _, item := range ownerRows {
			rows = append(rows, row{name: item.name, target: harnessScope, run: item.run})
		}
		for _, item := range workspaceRows {
			rows = append(rows, row{name: item.name, target: CallerScope{Kind: CallerScopeWorkspace, Workspace: workspaceA}, run: item.run})
		}
		for _, item := range sessionRows {
			rows = append(rows, row{name: item.name, target: CallerScope{Kind: CallerScopeSession, SessionID: item.target}, run: item.run})
		}
		for _, item := range rows {
			t.Run(item.name, func(t *testing.T) {
				otherSession := sessionA
				if item.target.Kind == CallerScopeSession && item.target.SessionID == sessionA {
					otherSession = sessionB
				}
				matchingSession := sessionA
				if item.target.Kind == CallerScopeSession {
					matchingSession = item.target.SessionID
				}
				callers := []struct {
					name    string
					c       Caller
					allowed bool
				}{
					{"harness", testCaller(), true},
					{"workspace", workspaceCaller, item.target.Kind == CallerScopeWorkspace && item.target.Workspace == workspaceA || item.target.Kind == CallerScopeSession && workspaceOf[item.target.SessionID] == workspaceA},
					{"other workspace", otherWorkspaceCaller, item.target.Kind == CallerScopeWorkspace && item.target.Workspace == workspaceB || item.target.Kind == CallerScopeSession && workspaceOf[item.target.SessionID] == workspaceB},
					{"matching session", sessionCaller(matchingSession), item.target.Kind == CallerScopeSession},
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
						testassert.Require(t, ErrorCode(err) == protocol.ErrorCodeDenied, "classify = %s, want denied", ErrorCode(err))
						testassert.Require(t, counting.listCount() == lists, "denied producer enumerated Sessions")
						testassert.Require(t, state() == before, "denied producer changed owner or Session state")
					})
				}
			})
		}
	})
}

// TestPluginCallerProvenanceCrossesTheProducer proves plugin attribution
// supplied by a trusted Caller reaches the committed input and both generated
// projections with no wire field involved.
func TestPluginCallerProvenanceCrossesTheProducer(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		sessionID := projectionSession(t, r, filepath.Join(e.home, "plugin-provenance"), "solo").Identity.SessionID
		pluginCaller, err := newCaller(CallerContext{Origin: harness.InputOriginPlugin, ExtensionID: "calling.extension", Scope: CallerScope{Kind: CallerScopeHarness}})
		testassert.NoError(t, err)
		gate := make(chan struct{})
		e.server.setHold(gate)
		release := sync.OnceFunc(func() { close(gate) })
		defer release()
		{
			_, err := r.SubmitSession(ctx, pluginCaller, sessionID, commandSubmitRequest(t, "op-plugin", "plugin work"))
			testassert.NoError(t, err)
		}
		awaitModelArrival(t, e)
		{
			result, err := r.SubmitSession(ctx, pluginCaller, sessionID, commandSubmitRequest(t, "op-buffered", "buffered work"))
			testassert.NoError(t, err)
			testassert.Require(t, result.Disposition == protocol.SubmitResultDispositionSteering, "buffered submit = %s, want steering", result.Disposition)
		}
		{
			hydration, err := r.GetHydration(ctx, testCaller(), sessionID)
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
			hydration, err := r.GetHydration(ctx, testCaller(), sessionID)
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

// TestCallerIsBoundToItsIssuingOwner proves the root-caller constructor
// requires a live owner and commands deny the zero Caller and a Caller issued
// by a different Melta.
func TestCallerIsBoundToItsIssuingOwner(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		first, _ := openProjectionRuntime(t, storage.NewMemory())
		defer closeProjectionRuntime(first)
		second, _ := openProjectionRuntime(t, storage.NewMemory())
		defer closeProjectionRuntime(second)

		var nilOwner *Melta
		if _, err := nilOwner.Caller(CallerContext{Origin: harness.InputOriginUser, Scope: harnessScope}); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("nil owner Caller = %v, want harness.ErrInvalid", err)
		}
		issuedByFirst, err := first.Caller(CallerContext{Origin: harness.InputOriginUser, Scope: harnessScope})
		testassert.NoError(t, err)
		if _, err := second.GetConfiguration(ctx, issuedByFirst); !errors.Is(err, ErrDenied) {
			t.Fatalf("foreign Caller = %v, want ErrDenied", err)
		}
		if _, err := first.GetConfiguration(ctx, Caller{}); !errors.Is(err, ErrDenied) {
			t.Fatalf("zero Caller = %v, want ErrDenied", err)
		}
		if _, err := first.GetConfiguration(ctx, issuedByFirst); err != nil {
			t.Fatalf("own Caller = %v, want the producer reached", err)
		}
		_ = store
	})
}
