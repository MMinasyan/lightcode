package config

import (
	"github.com/MMinasyan/lightcode/internal/testassert"
	"testing"
)

func TestClassifyKeySourceOrder(t *testing.T) {
	yes := func(string) bool { return true }
	no := func(string) bool { return false }

	cases := []struct {
		name      string
		apiKeyEnv string
		external  func(string) bool
		managed   func(string) bool
		want      string
	}{
		{"empty env is keyless regardless of closures", "", yes, yes, KeySourceKeyless},
		{"external wins over managed", "K", yes, yes, KeySourceExternal},
		{"managed when not external", "K", no, yes, KeySourceManaged},
		{"none when neither", "K", no, no, KeySourceNone},
		{"nil closures classify none", "K", nil, nil, KeySourceNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testassert.Equal(t, ClassifyKeySource(tc.apiKeyEnv, tc.external, tc.managed), tc.want)
		})
	}
}

// Agent-context adapter: LoadDotEnv has Setenv'd every managed key, so the
// agent passes external = set-in-env AND NOT managed. A key that is both in
// the env and managed must classify managed, not external.
func TestClassifyKeySourceAgentContext(t *testing.T) {
	envSet := map[string]bool{"MANAGED_KEY": true, "SHELL_KEY": true}
	managedSet := map[string]bool{"MANAGED_KEY": true}
	managed := func(name string) bool { return managedSet[name] }
	external := func(name string) bool { return envSet[name] && !managed(name) }

	testassert.Equal(t, ClassifyKeySource("MANAGED_KEY", external, managed), KeySourceManaged)
	testassert.Equal(t, ClassifyKeySource("SHELL_KEY", external, managed), KeySourceExternal)
	testassert.Equal(t, ClassifyKeySource("UNSET_KEY", external, managed), KeySourceNone)
}

// Doctor-context adapter: LoadDotEnv never ran, so doctor mirrors its
// effective env — external means a non-empty shell value; managed means the
// name is defined in .env (any value) and not shadowed by the shell, which
// skips even empty exports. A shell-exported key beats .env; a .env-only
// key stays managed.
func TestClassifyKeySourceDoctorContext(t *testing.T) {
	shell := map[string]string{"BOTH_KEY": "x", "SHELL_KEY": "x", "EMPTY_SHELL_KEY": ""}
	dotenv := map[string]bool{"BOTH_KEY": true, "DOTENV_KEY": true, "EMPTY_DOTENV_KEY": false}
	external := func(name string) bool { return shell[name] != "" }
	managed := func(name string) bool {
		if _, shellPresent := shell[name]; shellPresent {
			return false
		}
		_, inDotEnv := dotenv[name]
		return inDotEnv
	}

	testassert.Equal(t, ClassifyKeySource("BOTH_KEY", external, managed), KeySourceExternal)
	testassert.Equal(t, ClassifyKeySource("DOTENV_KEY", external, managed), KeySourceManaged)
	testassert.Equal(t, ClassifyKeySource("EMPTY_DOTENV_KEY", external, managed), KeySourceManaged)
	testassert.Equal(t, ClassifyKeySource("EMPTY_SHELL_KEY", external, managed), KeySourceNone)
	testassert.Equal(t, ClassifyKeySource("OTHER_KEY", external, managed), KeySourceNone)
}
