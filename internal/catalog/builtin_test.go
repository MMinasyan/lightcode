package catalog

import (
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
		{
			err := json.Unmarshal(data, &raw)
			testassert.Require(t, err == nil, "%s: unmarshal: %v", path, err)
		}
		{
			errs := ValidateRaw(providerID, raw, true)
			testassert.Require(t, len(errs) == 0, "%s: ValidateRaw(strict=true) errors = %#v", path, errs)
		}

		result := Build(BuildInputs{Bundled: map[string]json.RawMessage{providerID: data}})
		testassert.Require(t, len(result.Warnings) == 0, "%s: Build warnings = %#v, want none", path, result.Warnings)
		provider := result.Catalog.Providers[providerID]
		testassert.Require(t, provider != nil, "%s: provider %q missing after Build", path, providerID)
		{
			errs := ValidateEffective(provider)
			testassert.Require(t, len(errs) == 0, "%s: ValidateEffective errors = %#v", path, errs)
		}
		return nil
	})
	testassert.NoError(t, err)

	for _, providerID := range []string{"openai", "openrouter"} {
		testassert.Require(t, seen[providerID], "bundled provider %q was not found", providerID)
	}
}

func TestBrokenBundledCatalogFixtureFailsStrictValidation(t *testing.T) {
	var raw map[string]any
	testassert.NoError(t, json.Unmarshal([]byte(`{
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
	}`), &raw))

	errs := ValidateRaw("broken", raw, true)
	testassert.Require(t, len(errs) != 0, "ValidateRaw accepted deliberately broken fixture")
	for _, err := range errs {
		if err.Field == "models.bad.extra_body.model" && err.Reason == "reserved request body key" {
			return
		}
	}
	t.Fatalf("ValidateRaw errors = %#v, want reserved-key failure for broken fixture", errs)
}
