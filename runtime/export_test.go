package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/extension"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/protocol"
)

type ToolConstraints = harness.ToolConstraints

func wireSelection(name string) *protocol.AgentSelection {
	value := projectAgentSelection(harness.AgentSelection{Definition: &harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: name}})
	return &value
}

func WriteAgentDefinitionsForTest(t *testing.T, configPath, content string) {
	writeAgentDefinitions(t, configPath, content)
}

// OpenForTest is the test-build composition bridge used by the external
// runtime_test integration tests to assemble the actual plugin set without a
// runtime-to-plugin import cycle. It forwards to the public Open with the
// supplied plugins: the same concrete preparation path, with no alternate
// producer. It is absent from production builds.
func OpenForTest(ctx context.Context, dataDir, configPath string, plugins []extension.Native) (*Runtime, error) {
	return Open(ctx, Options{
		DataDir:    dataDir,
		ConfigPath: configPath,
		Plugins:    plugins,
	})
}

// ConfiguredInvocationForTest builds one configured Invocation whose
// captured snapshot carries the given top-level baseline sections and
// per-plugin sections, for the external tests that exercise the real
// Invocation settings channels — outside this package only the zero
// Invocation is constructible. It is absent from production builds.
func ConfiguredInvocationForTest(tools, jobs, compaction string, sections map[string]string) (Invocation, error) {
	plugins := make(map[string]json.RawMessage, len(sections))
	for id, section := range sections {
		plugins[id] = json.RawMessage(section)
	}
	doc := capturedConfigDocument{
		Tools:      json.RawMessage(tools),
		Jobs:       json.RawMessage(jobs),
		Compaction: json.RawMessage(compaction),
		Extensions: plugins,
	}
	snapshot, err := newConfiguration(1, doc, catalog.BuildResult{}, nil)
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

// UpdateSettingsForTest bridges the private settings/extension-edit mutation —
// the whole-shape configuration writer — for the external composition tests
// that compose the real plugin validators. It is absent from production
// builds.
func UpdateSettingsForTest(r *Runtime, settings protocol.Settings) (protocol.SettingsMutation, error) {
	return r.updateSettings(context.Background(), protocol.UpdateSettingsRequest{Settings: &settings})
}

// SetAgentDefinitionModelForTest drives the supplied set_model configuration edit
// through the public UpdateSettings extension-edit arm for the external
// composition tests that compose the real agent roster. It is absent from
// production builds.
func SetAgentDefinitionModelForTest(r *Runtime, agentType, modelRef string) (protocol.SettingsMutation, error) {
	arguments, err := json.Marshal(struct {
		DefinitionID string `json:"definition_id"`
		Model        string `json:"model"`
	}{DefinitionID: agentType, Model: modelRef})
	if err != nil {
		return protocol.SettingsMutation{}, err
	}
	return r.updateSettings(context.Background(), protocol.UpdateSettingsRequest{ExtensionEdit: &protocol.ExtensionEdit{
		ExtensionId: "agents",
		EditId:      "set_model",
		Arguments:   string(arguments),
	}})
}

// createSession is the test-build root Session creation bridge: it enters
// Runtime's existing admitted-call gate and runs the one normalized creation
// core, so tests needing the full Core record observe the real gate. It is
// absent from production builds.
func (r *Runtime) createSession(ctx context.Context, workspace, agentType string) (harness.SessionRecord, error) {
	var record harness.SessionRecord
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		var err error
		record, err = r.createSessionRecord(ctx, h, workspace, harness.AgentSelection{Definition: &harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: agentType}})
		return err
	}); err != nil {
		return harness.SessionRecord{}, err
	}
	return record, nil
}

// CreateSessionForTest bridges the private root Session creation for the
// external composition tests. It is absent from production builds.
func CreateSessionForTest(ctx context.Context, r *Runtime, workspace, agentType string) (harness.SessionRecord, error) {
	return r.createSession(ctx, workspace, agentType)
}

// SubmitForTest bridges one private regular user-message submission for the
// external composition tests. It is absent from production builds.
func SubmitForTest(ctx context.Context, r *Runtime, sessionID, operationID, text string) error {
	_, err := submitRawThroughRuntime(ctx, r, sessionID, operationID, text, harness.MessageModeRegular)
	return err
}

// ShippedAdaptationPlugin supplies the shipped adaptation plugin to
// in-package tests, which receive it from the external composition test's
// init assignment.
var ShippedAdaptationPlugin func() extension.Native
