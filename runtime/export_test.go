package runtime

import (
	"context"
	"encoding/json"

	"github.com/MMinasyan/lightcode/internal/catalog"
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
