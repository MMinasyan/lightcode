package runtime

import (
	"context"
	"encoding/json"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// OpenForTest is the test-build composition bridge used by the external
// runtime_test integration tests to assemble the actual plugin set without a
// runtime-to-plugin import cycle. It runs the private open path with the
// existing controlled preparation fixture; it is absent from production
// builds, and no production constructor or concrete-plugin import backs it.
func OpenForTest(ctx context.Context, dataDir, configPath string, plugins []Plugin) (*Runtime, error) {
	return open(ctx, options{
		DataDir:    dataDir,
		ConfigPath: configPath,
		Plugins:    plugins,
		prepare:    newControlledPrep().prepare,
	})
}

// ConfiguredInvocationForTest builds one configured Invocation whose
// captured snapshot carries the given per-plugin sections, for the external
// tests that exercise a plugin's real Invocation.Config channel — outside
// this package only the zero Invocation is constructible. It is absent from
// production builds.
func ConfiguredInvocationForTest(sections map[string]string) (Invocation, error) {
	plugins := make(map[string]json.RawMessage, len(sections))
	for id, section := range sections {
		plugins[id] = json.RawMessage(section)
	}
	snapshot, err := newConfiguration(1, capturedConfigDocument{Plugins: plugins}, catalog.BuildResult{}, []byte("{}"), nil, nil, nil, nil)
	if err != nil {
		return Invocation{}, err
	}
	return Invocation{snapshot: snapshot}, nil
}

// EqualEventForTest exposes the field-wise Event comparison to the external
// runtime_test package. It is absent from production builds.
func EqualEventForTest(a, b Event) bool {
	return equalEvent(a, b)
}

// ComposeScopeForTest opens the given plugins as one Runtime scope through
// the composition machinery — every dependency binding is constructed
// in-package — and returns the bound capability values by ID (ordinary
// bindings plus the private Core seam values a Harness consumer wires, such
// as the job stopper) together with the scope disposal. It is absent from
// production builds.
func ComposeScopeForTest(ctx context.Context, info ScopeInfo, plugins []Plugin) (map[string]any, func() error, error) {
	c, err := newComposition(plugins)
	if err != nil {
		return nil, nil, err
	}
	sc, err := c.openScope(ctx, info, nil)
	if err != nil {
		return nil, nil, err
	}
	values := make(map[string]any, len(sc.bindings.entries)+len(sc.jobStoppers))
	for id, entry := range sc.bindings.entries {
		values[id] = entry.value
	}
	for id, value := range sc.jobStoppers {
		values[id] = value
	}
	return values, sc.close, nil
}

// DescribeToolForTest invokes one declared tool spec's recorded description
// function for the external tests that exercise a real plugin's describe
// closure — outside this package only the zero Invocation is constructible.
// It is absent from production builds.
func DescribeToolForTest(spec CapabilitySpec, inv Invocation, constraints ToolConstraints, identity harness.SessionIdentity) (ToolDescription, error) {
	return spec.describe(inv, constraints, identity)
}

// GetConfigurationForTest bridges the private configuration read for the
// external composition tests. It is absent from production builds.
func GetConfigurationForTest(r *Runtime) (protocol.ConfigurationView, error) {
	return r.getConfiguration(context.Background())
}

// GetWarningsForTest bridges the unfiltered warning read for the external
// composition tests. It is absent from production builds.
func GetWarningsForTest(r *Runtime) (protocol.WarningsSnapshot, error) {
	return r.getWarnings(context.Background())
}

// UpdateSettingsForTest bridges the private settings mutation — the
// whole-shape configuration writer — for the external composition tests that
// compose the real plugin validators. It is absent from production builds.
func UpdateSettingsForTest(r *Runtime, settings protocol.Settings) (protocol.SettingsMutation, error) {
	return r.updateSettings(context.Background(), settings)
}

// SetAgentTypeModelForTest bridges the private Agent-model mutation for the
// external composition tests that compose the real agent roster. It is
// absent from production builds.
func SetAgentTypeModelForTest(r *Runtime, agentType, modelRef string) (protocol.AgentMutation, error) {
	return r.setAgentTypeModel(context.Background(), agentType, modelRef)
}

// CreateSessionForTest bridges the private root Session creation for the
// external composition tests. It is absent from production builds.
func CreateSessionForTest(ctx context.Context, r *Runtime, workspace, agentType string) (harness.SessionRecord, error) {
	return r.createSession(ctx, workspace, agentType)
}

// SubmitForTest bridges one private regular user-message submission for the
// external composition tests. It is absent from production builds.
func SubmitForTest(ctx context.Context, r *Runtime, sessionID, operationID, text string) error {
	return r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		_, err := h.Submit(ctx, harness.SubmitRequest{
			SessionID:   sessionID,
			OperationID: operationID,
			Origin:      harness.InputOriginUser,
			Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
			Mode:        harness.MessageModeRegular,
		})
		return err
	})
}
