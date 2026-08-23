package catalog

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// snapshotTree records every entry under root with its size and mtime, so a
// before/after comparison proves zero filesystem writes. A missing root
// yields an empty snapshot.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		snap[path] = fmt.Sprintf("%d:%d:%v", info.Size(), info.ModTime().UnixNano(), info.IsDir())
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snap
}

func TestBuildOfflineZeroWrites(t *testing.T) {
	t.Run("absent data dir", func(t *testing.T) {
		home := t.TempDir()
		before := snapshotTree(t, home)
		if _, err := BuildOffline(home, nil); err != nil {
			t.Fatalf("BuildOffline: %v", err)
		}
		after := snapshotTree(t, home)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("filesystem changed:\nbefore: %v\nafter:  %v", before, after)
		}
		if _, statErr := os.Stat(filepath.Join(home, ".lightcode")); !os.IsNotExist(statErr) {
			t.Fatalf(".lightcode was created: stat err = %v", statErr)
		}
	})

	t.Run("populated cache dir", func(t *testing.T) {
		home := t.TempDir()
		dir := filepath.Join(home, ".lightcode", "cache", "discovery")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotTree(t, home)
		userRaw := map[string]any{
			"local": map[string]any{
				"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"discovery": false,
				"models":    map[string]any{"m1": map[string]any{"context_window": float64(8192)}},
			},
		}
		if _, err := BuildOffline(home, userRaw); err != nil {
			t.Fatalf("BuildOffline: %v", err)
		}
		after := snapshotTree(t, home)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("filesystem changed:\nbefore: %v\nafter:  %v", before, after)
		}
	})
}

func TestBuildOfflineSurfacesCorruptCacheWarning(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".lightcode", "cache", "discovery")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "someprovider.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := BuildOffline(home, nil)
	if err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}
	found := false
	for _, w := range result.Warnings {
		if w.Kind == "discovery_failure" && w.Provider == "someprovider" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no discovery_failure warning for the corrupt cache file; warnings = %+v", result.Warnings)
	}
	if result.Catalog == nil || len(result.Catalog.Providers) == 0 {
		t.Fatal("bundled providers missing from the offline catalog")
	}
}

func TestProviderConnected(t *testing.T) {
	usable := map[string]*Model{"m": {ID: "m", ContextWindow: 8192}}
	unusable := map[string]*Model{"m": {ID: "m", ContextWindow: 0}}
	envSet := func(set map[string]bool) func(string) bool {
		return func(name string) bool { return set[name] }
	}

	cases := []struct {
		name  string
		prov  *Provider
		isSet func(string) bool
		want  bool
	}{
		{"nil provider", nil, envSet(map[string]bool{"K": true}), false},
		{
			"keyed with env set and usable model",
			&Provider{Transport: Transport{APIKeyEnv: "K", BaseURL: "https://x"}, Models: usable},
			envSet(map[string]bool{"K": true}), true,
		},
		{
			"keyed with env unset",
			&Provider{Transport: Transport{APIKeyEnv: "K", BaseURL: "https://x"}, Models: usable},
			envSet(map[string]bool{}), false,
		},
		{
			"keyed with env set but no usable model",
			&Provider{Transport: Transport{APIKeyEnv: "K", BaseURL: "https://x"}, Models: unusable},
			envSet(map[string]bool{"K": true}), false,
		},
		{
			"keyless with base url and usable model",
			&Provider{Transport: Transport{BaseURL: "http://localhost:11434/v1"}, Models: usable},
			envSet(map[string]bool{}), true,
		},
		{
			"keyless without base url",
			&Provider{Models: usable},
			envSet(map[string]bool{}), false,
		},
		{
			"keyed with nil env lookup",
			&Provider{Transport: Transport{APIKeyEnv: "K", BaseURL: "https://x"}, Models: usable},
			nil, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderConnected(tc.prov, tc.isSet); got != tc.want {
				t.Fatalf("ProviderConnected = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiscoveryTransportReadyDoesNotRequireModels(t *testing.T) {
	lookup := func(set bool) func(string) bool {
		return func(string) bool { return set }
	}
	cases := []struct {
		name string
		prov *Provider
		set  func(string) bool
		want bool
	}{
		{"nil", nil, lookup(true), false},
		{"disabled", &Provider{Discovery: false, Transport: Transport{BaseURL: "http://x"}}, lookup(true), false},
		{"keyed empty models with key", &Provider{Discovery: true, Transport: Transport{APIKeyEnv: "K"}}, lookup(true), true},
		{"keyed without key", &Provider{Discovery: true, Transport: Transport{APIKeyEnv: "K"}}, lookup(false), false},
		{"keyless empty models", &Provider{Discovery: true, Transport: Transport{BaseURL: "http://x"}}, lookup(false), true},
		{"keyless without base url", &Provider{Discovery: true}, lookup(false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DiscoveryTransportReady(tc.prov, tc.set); got != tc.want {
				t.Fatalf("DiscoveryTransportReady = %v, want %v", got, tc.want)
			}
		})
	}
}

// Doctor-context resolution: LoadDotEnv never ran, so a .env-only key is not
// in the process env — it resolves because the ReadDotEnvKeys map (name ->
// value is non-empty) is part of the injected lookup; the shell, even an
// empty export, shadows it.
func TestProviderConnectedDoctorContext(t *testing.T) {
	dotenvKeys := map[string]bool{"DOTENV_ONLY_KEY": true}
	isSet := func(name string) bool {
		if shellVal, shellPresent := os.LookupEnv(name); shellPresent {
			return shellVal != ""
		}
		return dotenvKeys[name]
	}
	prov := &Provider{
		Transport: Transport{APIKeyEnv: "DOTENV_ONLY_KEY", BaseURL: "https://x"},
		Models:    map[string]*Model{"m": {ID: "m", ContextWindow: 8192}},
	}
	if _, set := os.LookupEnv("DOTENV_ONLY_KEY"); set {
		t.Fatal("test precondition: DOTENV_ONLY_KEY must not be in the process env")
	}
	if !ProviderConnected(prov, isSet) {
		t.Fatal("a .env-only key must resolve through the dotenv key set")
	}
	if ProviderConnected(prov, func(name string) bool { _, ok := os.LookupEnv(name); return ok }) {
		t.Fatal("without the dotenv set the key must not resolve (the false-disconnected regression)")
	}
}
