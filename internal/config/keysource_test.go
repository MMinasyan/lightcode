package config

import "testing"

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
			if got := ClassifyKeySource(tc.apiKeyEnv, tc.external, tc.managed); got != tc.want {
				t.Fatalf("ClassifyKeySource = %q, want %q", got, tc.want)
			}
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

	if got := ClassifyKeySource("MANAGED_KEY", external, managed); got != KeySourceManaged {
		t.Fatalf("managed+env key = %q, want %q", got, KeySourceManaged)
	}
	if got := ClassifyKeySource("SHELL_KEY", external, managed); got != KeySourceExternal {
		t.Fatalf("shell key = %q, want %q", got, KeySourceExternal)
	}
	if got := ClassifyKeySource("UNSET_KEY", external, managed); got != KeySourceNone {
		t.Fatalf("unset key = %q, want %q", got, KeySourceNone)
	}
}

// Doctor-context adapter: LoadDotEnv never ran, so doctor passes the shell
// env directly and the ReadDotEnvKeys name set as managed. A shell-exported
// key beats .env; a .env-only key stays managed.
func TestClassifyKeySourceDoctorContext(t *testing.T) {
	shell := map[string]bool{"BOTH_KEY": true, "SHELL_KEY": true}
	dotenv := map[string]struct{}{"BOTH_KEY": {}, "DOTENV_KEY": {}}
	external := func(name string) bool { return shell[name] }
	managed := func(name string) bool { _, ok := dotenv[name]; return ok }

	if got := ClassifyKeySource("BOTH_KEY", external, managed); got != KeySourceExternal {
		t.Fatalf("shell+.env key = %q, want %q", got, KeySourceExternal)
	}
	if got := ClassifyKeySource("DOTENV_KEY", external, managed); got != KeySourceManaged {
		t.Fatalf(".env-only key = %q, want %q", got, KeySourceManaged)
	}
	if got := ClassifyKeySource("OTHER_KEY", external, managed); got != KeySourceNone {
		t.Fatalf("absent key = %q, want %q", got, KeySourceNone)
	}
}
