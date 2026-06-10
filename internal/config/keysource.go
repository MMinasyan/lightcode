package config

// Key-source classes for a provider's API key, as shown in settings
// surfaces and diagnostics.
const (
	KeySourceNone     = "none"
	KeySourceKeyless  = "keyless"
	KeySourceManaged  = "managed"
	KeySourceExternal = "external"
)

// ClassifyKeySource reports where a provider's API key comes from. The
// caller injects the environment semantics: externalEnvIsSet must report
// keys set outside Lightcode's control, managedKey must report keys
// Lightcode owns. The invariant every caller preserves: a shell-exported
// key beats .env; a key Lightcode loaded from .env stays managed.
func ClassifyKeySource(apiKeyEnv string, externalEnvIsSet func(string) bool, managedKey func(string) bool) string {
	if apiKeyEnv == "" {
		return KeySourceKeyless
	}
	if externalEnvIsSet != nil && externalEnvIsSet(apiKeyEnv) {
		return KeySourceExternal
	}
	if managedKey != nil && managedKey(apiKeyEnv) {
		return KeySourceManaged
	}
	return KeySourceNone
}
