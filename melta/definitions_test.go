package melta

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/Syntheresis/melta/extension"
	agents "github.com/Syntheresis/melta/extensions/agents"
	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/storage"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/model"
	"github.com/Syntheresis/melta/protocol"
)

// guestResolverNative is one read-only definition resolver beside the supplied
// Agents extension: it lists and resolves one definition under the same
// displayed name, declares no configuration edit, and never imports Melta.
func guestResolverNative(t *testing.T) extension.Native {
	t.Helper()
	return guestResolverNativeDefinition(t, "primary")
}

// guestResolverNativeDefinition is the same read-only resolver over one named
// definition, for rows that need an explicit same-named foreign provider.
func guestResolverNativeDefinition(t *testing.T, definitionID string) extension.Native {
	t.Helper()
	inputs := harness.AgentInputs{
		Model:        model.ModelRef{Provider: "prov", Model: "m"},
		Instructions: "guest instructions",
		AssemblyMode: "simple",
		Tools:        []string{"read_file"},
	}.Owned()
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           "guestdefs",
			Version:      "1",
			ConfigSchema: []byte(`{"type":"object","required":["enabled"],"properties":{"enabled":{"type":"boolean"}},"additionalProperties":false}`),
			Components: []extension.Component{{
				ID:       "guestdefs",
				Lifetime: extension.LifetimeHarness,
				Provides: []string{"guestdefs.definitions"},
			}},
			Capabilities: []extension.Capability{extension.AgentDefinitionsCapability("guestdefs.definitions")},
		},
		Open: func(ctx context.Context, _ extension.InstanceContext) (extension.Instance, error) {
			if err := ctx.Err(); err != nil {
				return extension.Instance{}, err
			}
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				"guestdefs.definitions": {Service: map[string]extension.Handler{
					"list": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
						value, err := json.Marshal([]extension.AgentDefinitionSummary{{ID: definitionID, Description: "guest " + definitionID}})
						return extension.Result{Value: value}, err
					},
					"resolve": func(_ context.Context, _ extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
						var request struct {
							ID string `json:"id"`
						}
						if err := json.Unmarshal(arguments, &request); err != nil {
							return extension.Result{}, err
						}
						if request.ID != definitionID {
							return extension.Result{}, errors.New("unknown guest definition")
						}
						value, err := json.Marshal(extension.AgentDefinitionResolution{Inputs: inputs, Description: "guest " + definitionID})
						return extension.Result{Value: value}, err
					},
				}},
			}}, nil
		},
	}
}

// TestRuntimeReadOnlyResolverCoexistsWithAgents proves the generic resolver
// contract end to end over the real owner: two providers may list the same
// displayed name, the roster carries both owner-stamped full references, a
// selection executes through its concrete provider, and a resolver without
// edits refuses the supplied set_model edit without publishing.
func TestRuntimeReadOnlyResolverCoexistsWithAgents(t *testing.T) {
	ctx := context.Background()
	e := newOwnerEnv(t)
	e.fixtureStore(storage.NewMemory())
	enableFixturePlugin(t, e.configPath, "guestdefs")
	guest := guestResolverNative(t)
	owner, err := e.open(ctx, agentsTestNative(guest), guest)
	testassert.NoError(t, err)
	defer closeProjectionRuntime(owner)

	view, err := owner.GetConfiguration(ctx, testCaller(owner))
	testassert.NoError(t, err)
	var ownRef, guestRef protocol.DefinitionRef
	ownCount, guestCount := 0, 0
	for _, agent := range view.Agents {
		if agent.Name != "primary" {
			continue
		}
		switch agent.Definition.ExtensionId {
		case "agents":
			ownRef, ownCount = agent.Definition, ownCount+1
		case "guestdefs":
			guestRef, guestCount = agent.Definition, guestCount+1
		}
	}
	testassert.Require(t, ownCount == 1 && guestCount == 1, "primary roster entries = %d/%d, want one per provider: %+v", ownCount, guestCount, view.Agents)
	testassert.Require(t, ownRef == (protocol.DefinitionRef{ExtensionId: "agents", CapabilityId: "agents.definitions", DefinitionId: "primary"}) && guestRef == (protocol.DefinitionRef{ExtensionId: "guestdefs", CapabilityId: "guestdefs.definitions", DefinitionId: "primary"}), "stamped references = %+v / %+v", ownRef, guestRef)

	workspace := e.dataDir
	guestSession := createNamedSession(t, owner, workspace, "guestdefs", "guestdefs.definitions", "primary")
	submitThroughRuntime(t, owner, guestSession.Identity.SessionID, "op-guest", "hello")
	guestRecord := awaitOperation(t, owner, guestSession.Identity.SessionID, "op-guest", harness.OperationSuccess)
	testassert.Require(t, guestRecord.Admission.Execution.Model == (model.ModelRef{Provider: "prov", Model: "m"}) && reflect.DeepEqual(guestRecord.Admission.Execution.InitialToolNames, []string{"read_file"}), "guest capture = %v/%v, want the guest resolver's inputs", guestRecord.Admission.Execution.Model, guestRecord.Admission.Execution.InitialToolNames)
	testassert.Require(t, len(guestRecord.Admission.Execution.Implementations) == 1 && guestRecord.Admission.Execution.Implementations[0].ExtensionID == "guestdefs" && guestRecord.Admission.Execution.Implementations[0].Version == "1" && string(guestRecord.Admission.Execution.Implementations[0].Settings) == `{"enabled":true}`, "guest provenance = %+v, want only the concrete resolver root", guestRecord.Admission.Execution.Implementations)

	ownSession := createNamedSession(t, owner, workspace, "agents", "agents.definitions", "solo")
	submitThroughRuntime(t, owner, ownSession.Identity.SessionID, "op-own", "hello")
	ownRecord := awaitOperation(t, owner, ownSession.Identity.SessionID, "op-own", harness.OperationSuccess)
	testassert.Require(t, len(ownRecord.Admission.Execution.InitialToolNames) == len(agents.StandardTools), "own capture tools = %v, want the supplied Agents inherited tool set", ownRecord.Admission.Execution.InitialToolNames)

	// The read-only resolver declares no edit: the supplied edit ID and a
	// foreign edit ID both refuse invalid input and publish nothing.
	published := owner.config.current()
	for _, row := range []struct{ extensionID, editID string }{
		{"guestdefs", "set_model"},
		{"agents", "relabel"},
	} {
		_, err := owner.config.mutate(ctx, owner.config.editExtension(ctx, row.extensionID, row.editID, json.RawMessage(`{"definition_id":"primary","model":"prov/m"}`)))
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "%s/%s edit = %v, want harness.ErrInvalid", row.extensionID, row.editID, err)
		testassert.Require(t, owner.config.current() == published, "%s/%s edit published a generation", row.extensionID, row.editID)
	}
}

func createNamedSession(t *testing.T, owner *Melta, workspace, extensionID, capabilityID, definitionID string) harness.SessionRecord {
	t.Helper()
	var record harness.SessionRecord
	err := owner.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		record, err = h.CreateSession(ctx, harness.CreateSessionRequest{
			Workspace:      workspace,
			AgentSelection: harness.AgentSelection{Definition: &harness.DefinitionRef{ExtensionID: extensionID, CapabilityID: capabilityID, DefinitionID: definitionID}},
		})
		return err
	})
	testassert.NoError(t, err)
	return record
}

// TestSettingsRequestUnionAcceptsExactlyOne proves the accepting boundary of
// the generated settings/extension-edit request: neither arm or both arms is
// invalid input and publishes nothing.
func TestSettingsRequestUnionAcceptsExactlyOne(t *testing.T) {
	ctx := context.Background()
	e := newOwnerEnv(t)
	e.fixtureStore(storage.NewMemory())
	owner, err := e.open(ctx)
	testassert.NoError(t, err)
	defer closeProjectionRuntime(owner)
	published := owner.config.current()
	for _, request := range []protocol.UpdateSettingsRequest{
		{},
		{Settings: &protocol.Settings{}, ExtensionEdit: &protocol.ExtensionEdit{ExtensionId: "agents", EditId: "set_model", Arguments: "{}"}},
	} {
		_, err := owner.UpdateSettings(ctx, testCaller(owner), request)
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "union request = %+v, want harness.ErrInvalid", request)
		testassert.Require(t, owner.config.current() == published, "union rejection published a generation")
	}
}
