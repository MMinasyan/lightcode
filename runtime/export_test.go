package runtime

import "context"

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
