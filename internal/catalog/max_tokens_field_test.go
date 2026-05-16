package catalog

import "testing"

func TestBuildDefaultsAndOverridesMaxTokensField(t *testing.T) {
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"openai": map[string]any{
				"transport":        map[string]any{"base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY"},
				"max_tokens_field": "max_completion_tokens",
				"models": map[string]any{
					"gpt-5.4-mini": map[string]any{"context_window": float64(400000), "max_output_tokens": float64(128000)},
				},
			},
			"local": map[string]any{
				"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"models": map[string]any{
					"qwen": map[string]any{"context_window": float64(32768), "max_output_tokens": float64(8192)},
				},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v, want none", result.Warnings)
	}
	openAI, _, err := result.Catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5.4-mini"})
	if err != nil {
		t.Fatalf("Lookup openai/gpt-5.4-mini returned error: %v", err)
	}
	if openAI.MaxTokensField != "max_completion_tokens" {
		t.Fatalf("openai MaxTokensField = %q, want max_completion_tokens", openAI.MaxTokensField)
	}
	local, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "qwen"})
	if err != nil {
		t.Fatalf("Lookup local/qwen returned error: %v", err)
	}
	if local.MaxTokensField != "max_tokens" {
		t.Fatalf("local MaxTokensField = %q, want max_tokens default", local.MaxTokensField)
	}
}

func TestValidateRawRejectsInvalidMaxTokensField(t *testing.T) {
	raw := validRawProvider()
	raw["max_tokens_field"] = "max_output_tokens"

	errs := ValidateRaw("openai", raw, true)
	if !hasValidationError(errs, "max_tokens_field") {
		t.Fatalf("ValidateRaw errors = %#v, want max_tokens_field error", errs)
	}
}
