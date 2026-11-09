package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// recordingStrategy delegates to the included summary policy while recording
// the exact Settings document the Harness supplied to Plan and Consume.
type recordingStrategy struct {
	mu       sync.Mutex
	plans    []json.RawMessage
	consumes []json.RawMessage
}

func (s *recordingStrategy) Plan(input harness.PlanInput) (harness.PlanOutput, error) {
	s.mu.Lock()
	s.plans = append(s.plans, append(json.RawMessage(nil), input.Settings...))
	s.mu.Unlock()
	return harness.IncludedStrategyBinding().Strategy.Plan(input)
}

func (s *recordingStrategy) Consume(input harness.ConsumeInput) (harness.PlanOutput, error) {
	s.mu.Lock()
	s.consumes = append(s.consumes, append(json.RawMessage(nil), input.Input.Settings...))
	s.mu.Unlock()
	return harness.IncludedStrategyBinding().Strategy.Consume(input)
}

func (s *recordingStrategy) observed() ([]json.RawMessage, []json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]json.RawMessage(nil), s.plans...), append([]json.RawMessage(nil), s.consumes...)
}

// strategyExtension declares one Operation-scoped context strategy capability
// under its supplied extension identity/version.
func strategyExtension(id, version, capabilityID string, implementation harness.ContextStrategy) extension.Native {
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           id,
			Version:      version,
			ConfigSchema: []byte(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"],"additionalProperties":false}`),
			Components: []extension.Component{{
				ID:       id + "-c",
				Lifetime: extension.LifetimeOperation,
				Provides: []string{capabilityID},
			}},
			Capabilities: []extension.Capability{extension.StrategyCapability(capabilityID, true)},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				capabilityID: {Strategy: implementation},
			}}, nil
		},
	}
}

// namedResolverExtension lists/resolves named inputs from one owned map under
// a supplied extension identity.
func namedResolverExtension(id string, definitions map[string]harness.AgentInputs) extension.Native {
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           id,
			Version:      "1",
			ConfigSchema: []byte(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"],"additionalProperties":false}`),
			Components: []extension.Component{{
				ID:       id + "-c",
				Lifetime: extension.LifetimeHarness,
				Provides: []string{id + ".definitions"},
			}},
			Capabilities: []extension.Capability{extension.AgentDefinitionsCapability(id + ".definitions")},
		},
		Open: func(ctx context.Context, _ extension.InstanceContext) (extension.Instance, error) {
			if err := ctx.Err(); err != nil {
				return extension.Instance{}, err
			}
			ids := make([]string, 0, len(definitions))
			for name := range definitions {
				ids = append(ids, name)
			}
			sort.Strings(ids)
			summaries := make([]extension.AgentDefinitionSummary, 0, len(ids))
			for _, name := range ids {
				summaries = append(summaries, extension.AgentDefinitionSummary{ID: name, Description: name})
			}
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				id + ".definitions": {Service: map[string]extension.Handler{
					"list": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
						value, err := json.Marshal(summaries)
						return extension.Result{Value: value}, err
					},
					"resolve": func(_ context.Context, _ extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
						var request struct {
							ID string `json:"id"`
						}
						if err := json.Unmarshal(arguments, &request); err != nil {
							return extension.Result{}, err
						}
						inputs, ok := definitions[request.ID]
						if !ok {
							return extension.Result{}, errors.New("unknown definition")
						}
						value, err := json.Marshal(extension.AgentDefinitionResolution{Inputs: inputs.Owned(), Description: request.ID})
						return extension.Result{Value: value}, err
					},
				}},
			}}, nil
		},
	}
}

func namedStrategyConfig(endpoint string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "",
		`{"m":{"name":"M","context_window":8192,"max_output_tokens":4096},`+
			`"k":{"name":"K","context_window":32768,"max_output_tokens":4096}}`) + `},
		"compaction":{"model":"prov/k"},
		"extensions":{"guestdefs":{"enabled":true},"strategyext":{"enabled":true}}}`
}

// TestNamedStrategySettingsReachPlanConsumeAndCapture proves a named resolver's
// complete AgentInputs, including a custom Operation-scoped strategy and its
// exact Settings document, is the one execution value: the captured strategy
// carries the same bytes, Plan and Consume receive them, and provenance names
// the concrete resolver and strategy roots without an unused Agents root.
func TestNamedStrategySettingsReachPlanConsumeAndCapture(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, namedStrategyConfig(e.server.URL))
		e.fixtureStore(store)
		recorder := &recordingStrategy{}
		guest := namedResolverExtension("guestdefs", map[string]harness.AgentInputs{
			"named": {
				Model:        model.ModelRef{Provider: "prov", Model: "m"},
				Instructions: "named prompt",
				AssemblyMode: "simple",
				Tools:        []string{"read_file"},
				Strategy:     harness.StrategySelection{ID: "recorder.strategy", Settings: json.RawMessage(`{"exact":9007199254740993}`)},
			},
		})
		strategy := strategyExtension("strategyext", "7", "recorder.strategy", recorder)
		owner, err := e.open(context.Background(), guest, strategy)
		testassert.NoError(t, err)
		defer closeProjectionRuntime(owner)

		e.server.setScript(func(_ context.Context, body string) []string {
			if strings.Contains(body, `"model":"k"`) || strings.Contains(body, `"model": "k"`) {
				return append(textTurnEvents("summary"), `{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
			}
			if lastUserText(body) == "seed" {
				return append(textTurnEvents("seeded answer"), `{"usage":{"prompt_tokens":5000,"completion_tokens":10,"total_tokens":5010}}`)
			}
			return nil
		})

		session := createNamedSession(t, owner, e.dataDir, "guestdefs", "guestdefs.definitions", "named")
		submitThroughRuntime(t, owner, session.Identity.SessionID, "op-1", "seed")
		awaitOperation(t, owner, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		submitThroughRuntime(t, owner, session.Identity.SessionID, "op-2", "trigger")
		record := awaitOperation(t, owner, session.Identity.SessionID, "op-2", harness.OperationSuccess)

		wantSettings := json.RawMessage(`{"exact":9007199254740993}`)
		capture := record.Admission.Execution
		testassert.Require(t, capture.Strategy.ID == "recorder.strategy" && capture.Strategy.RequiresContextWindow && reflect.DeepEqual(capture.Strategy.Settings, wantSettings), "captured strategy = %+v, want the named strategy and exact settings", capture.Strategy)
		testassert.Require(t, capture.Model == (model.ModelRef{Provider: "prov", Model: "m"}) && capture.Readonly == false && capture.WriteDir == "", "capture identity/constraints = %+v, want the resolved inputs", capture)

		plans, consumes := recorder.observed()
		testassert.Require(t, len(plans) != 0 && len(consumes) != 0, "strategy calls = %d plans/%d consumes, want both", len(plans), len(consumes))
		for _, settings := range plans {
			testassert.Require(t, reflect.DeepEqual(settings, wantSettings), "Plan settings = %s, want exact %s", settings, wantSettings)
		}
		for _, settings := range consumes {
			testassert.Require(t, reflect.DeepEqual(settings, wantSettings), "Consume settings = %s, want exact %s", settings, wantSettings)
		}

		foundResolver, foundStrategy, foundAgents := false, false, false
		for _, implementation := range capture.Implementations {
			switch implementation.ExtensionID {
			case "guestdefs":
				foundResolver = implementation.Version == "1" && string(implementation.Settings) == `{"enabled":true}`
			case "strategyext":
				foundStrategy = implementation.Version == "7" && string(implementation.Settings) == `{"enabled":true}`
			case "agents":
				foundAgents = true
			}
		}
		testassert.Require(t, foundResolver && foundStrategy && !foundAgents, "provenance = %+v, want resolver+strategy roots and no unused Agents root", capture.Implementations)
	})
}

// TestNamedStrategyInvalidRefusesAdmissionWithoutOperation proves an unknown
// strategy and a strategy capability whose declaring extension is inactive
// both refuse named admission before any Operation is published.
func TestNamedStrategyInvalidRefusesAdmissionWithoutOperation(t *testing.T) {
	// The inactive strategy extension is installed but its section is never
	// enabled in this composition.
	disabled := strategyExtension("inactiveext", "1", "inactive.strategy", &recordingStrategy{})
	guest := namedResolverExtension("guestdefs", map[string]harness.AgentInputs{
		"ghost": {
			Model:        model.ModelRef{Provider: "prov", Model: "m"},
			Instructions: "ghost strategy",
			AssemblyMode: "simple",
			Strategy:     harness.StrategySelection{ID: "ghost.strategy"},
		},
		"inactive": {
			Model:        model.ModelRef{Provider: "prov", Model: "m"},
			Instructions: "inactive strategy",
			AssemblyMode: "simple",
			Strategy:     harness.StrategySelection{ID: "inactive.strategy"},
		},
	})
	for _, row := range []struct {
		name       string
		definition string
	}{
		{"unknown strategy", "ghost"},
		{"inactive strategy", "inactive"},
	} {
		t.Run(row.name, func(t *testing.T) {
			eachPrepStore(t, func(t *testing.T, store harness.Storage) {
				e := newOwnerEnv(t)
				writeServiceFile(t, e.configPath, namedStrategyConfig(e.server.URL))
				e.fixtureStore(store)
				owner, err := e.open(context.Background(), guest, disabled)
				testassert.NoError(t, err)
				defer closeProjectionRuntime(owner)
				session := createNamedSession(t, owner, e.dataDir, "guestdefs", "guestdefs.definitions", row.definition)
				_, err = submitRawThroughRuntime(context.Background(), owner, session.Identity.SessionID, "op-invalid", "hello", harness.MessageModeRegular)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "%s submit = %v, want harness.ErrInvalid", row.name, err)
				testassert.Require(t, errors.Is(readOperationError(context.Background(), owner, session.Identity.SessionID, "op-invalid"), harness.ErrNotFound), "%s published an Operation", row.name)
			})
		})
	}
}

// TestDirectStrategyInvalidRefusesAdmissionWithoutOperation proves the same
// declared-strategy validation applies to a resolved-inputs selection: an
// unknown strategy refuses before any Operation is published.
func TestDirectStrategyInvalidRefusesAdmissionWithoutOperation(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, namedStrategyConfig(e.server.URL))
		e.fixtureStore(store)
		recorder := &recordingStrategy{}
		strategy := strategyExtension("strategyext", "7", "recorder.strategy", recorder)
		owner, err := e.open(context.Background(), strategy)
		testassert.NoError(t, err)
		defer closeProjectionRuntime(owner)

		var session harness.SessionRecord
		err = owner.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			var createErr error
			session, createErr = h.CreateSession(ctx, harness.CreateSessionRequest{
				Workspace: e.dataDir,
				AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{
					Model:        model.ModelRef{Provider: "prov", Model: "m"},
					Instructions: "direct",
					AssemblyMode: "simple",
					Strategy:     harness.StrategySelection{ID: "ghost.strategy"},
				}},
			})
			return createErr
		})
		testassert.NoError(t, err)
		_, err = submitRawThroughRuntime(context.Background(), owner, session.Identity.SessionID, "op-direct-invalid", "hello", harness.MessageModeRegular)
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "direct invalid strategy submit = %v, want harness.ErrInvalid", err)
		testassert.Require(t, errors.Is(readOperationError(context.Background(), owner, session.Identity.SessionID, "op-direct-invalid"), harness.ErrNotFound), "direct invalid strategy published an Operation")
	})
}
