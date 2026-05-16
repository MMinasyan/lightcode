package catalog

import (
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundledCatalogFilesValidateAndBuild(t *testing.T) {
	seen := map[string]bool{}
	err := fs.WalkDir(bundledFS, "builtin", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}

		providerID := strings.TrimSuffix(filepath.Base(path), ".json")
		data, err := bundledFS.ReadFile(path)
		if err != nil {
			return err
		}
		seen[providerID] = true

		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("%s: unmarshal: %v", path, err)
		}
		if errs := ValidateRaw(providerID, raw, true); len(errs) != 0 {
			t.Fatalf("%s: ValidateRaw(strict=true) errors = %#v", path, errs)
		}

		result := Build(BuildInputs{Bundled: map[string]json.RawMessage{providerID: data}})
		if len(result.Warnings) != 0 {
			t.Fatalf("%s: Build warnings = %#v, want none", path, result.Warnings)
		}
		provider := result.Catalog.Providers[providerID]
		if provider == nil {
			t.Fatalf("%s: provider %q missing after Build", path, providerID)
		}
		if errs := ValidateEffective(provider); len(errs) != 0 {
			t.Fatalf("%s: ValidateEffective errors = %#v", path, errs)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk bundled catalog: %v", err)
	}

	for _, providerID := range []string{"openai", "openrouter"} {
		if !seen[providerID] {
			t.Fatalf("bundled provider %q was not found", providerID)
		}
	}
}

func TestBrokenBundledCatalogFixtureFailsStrictValidation(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{
		"id": "broken",
		"name": "Broken",
		"transport": {"base_url": "https://example.com/v1", "api_key_env": "BROKEN_API_KEY"},
		"models": {
			"bad": {
				"context_window": 128000,
				"max_output_tokens": 4096,
				"extra_body": {"model": "not allowed"}
			}
		}
	}`), &raw); err != nil {
		t.Fatalf("broken fixture unmarshal: %v", err)
	}

	errs := ValidateRaw("broken", raw, true)
	if len(errs) == 0 {
		t.Fatalf("ValidateRaw accepted deliberately broken fixture")
	}
	for _, err := range errs {
		if err.Field == "models.bad.extra_body.model" && err.Reason == "reserved request body key" {
			return
		}
	}
	t.Fatalf("ValidateRaw errors = %#v, want reserved-key failure for broken fixture", errs)
}
