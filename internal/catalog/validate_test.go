package catalog

import "testing"

func TestValidateRawAcceptsValidProvider(t *testing.T) {
	raw := validRawProvider()
	if errs := ValidateRaw("openai", raw, true); len(errs) != 0 {
		t.Fatalf("ValidateRaw returned errors: %#v", errs)
	}
}

func TestValidateRawAcceptsDiscoveryOnlyProviderWithoutModels(t *testing.T) {
	raw := validRawProvider()
	delete(raw, "models")

	if errs := ValidateRaw("openai", raw, true); len(errs) != 0 {
		t.Fatalf("ValidateRaw returned errors: %#v, want discovery-only provider without models to validate", errs)
	}
}

func TestValidateRawAcceptsDiscoveryOnlyProviderWithEmptyModels(t *testing.T) {
	raw := validRawProvider()
	raw["models"] = map[string]any{}

	if errs := ValidateRaw("openai", raw, true); len(errs) != 0 {
		t.Fatalf("ValidateRaw returned errors: %#v, want discovery-only provider with empty models to validate", errs)
	}
}

func TestValidateRawStrictRejectsUnknownProviderKey(t *testing.T) {
	raw := validRawProvider()
	raw["base_uri"] = "https://api.openai.com/v1"

	errs := ValidateRaw("openai", raw, true)
	if !hasValidationError(errs, "base_uri") {
		t.Fatalf("ValidateRaw errors = %#v, want base_uri error", errs)
	}
}

func TestValidateRawReportsMissingRequiredTransportBaseURL(t *testing.T) {
	raw := validRawProvider()
	delete(raw["transport"].(map[string]any), "base_url")

	errs := ValidateRaw("openai", raw, true)
	if !hasValidationError(errs, "transport.base_url") {
		t.Fatalf("ValidateRaw errors = %#v, want transport.base_url error", errs)
	}
}

func TestValidateRawRejectsReservedProviderExtraBodyKey(t *testing.T) {
	raw := validRawProvider()
	raw["extra_body"] = map[string]any{"model": "bad"}

	errs := ValidateRaw("openai", raw, true)
	if !hasValidationError(errs, "extra_body.model") {
		t.Fatalf("ValidateRaw errors = %#v, want reserved extra_body.model error", errs)
	}
}

func TestValidateRawRejectsInvalidProtocolMetadata(t *testing.T) {
	raw := validRawProvider()
	raw["protocol_metadata"] = map[string]any{
		"family":        "Bad Family",
		"must_preserve": []any{"reasoning_content", "content"},
		"drop":          []any{""},
	}

	errs := ValidateRaw("openai", raw, true)
	for _, field := range []string{"protocol_metadata.family", "protocol_metadata.must_preserve[1]", "protocol_metadata.drop[0]"} {
		if !hasValidationError(errs, field) {
			t.Fatalf("ValidateRaw errors = %#v, want %s error", errs, field)
		}
	}
}

func TestValidateRawRejectsIllegalEnums(t *testing.T) {
	raw := validRawProvider()
	raw["system_role"] = "assistant"
	raw["models"].(map[string]any)["gpt-5.4-mini"].(map[string]any)["input_modalities"] = []any{"text", "video"}

	errs := ValidateRaw("openai", raw, true)
	if !hasValidationError(errs, "system_role") {
		t.Fatalf("ValidateRaw errors = %#v, want system_role error", errs)
	}
	if !hasValidationError(errs, "models.gpt-5.4-mini.input_modalities[1]") {
		t.Fatalf("ValidateRaw errors = %#v, want input modality error", errs)
	}
}

func TestValidateEffectiveRejectsReservedModelExtraBodyKey(t *testing.T) {
	p := &Provider{
		ID:            "openai",
		Name:          "OpenAI",
		Transport:     Transport{BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
		SystemRole:    RoleSystem,
		UsageInStream: true,
		ExtraBody:     map[string]any{},
		Models: map[string]*Model{
			"gpt-5.4-mini": {
				ID:              "gpt-5.4-mini",
				Name:            "GPT-5.4 mini",
				ContextWindow:   128000,
				MaxOutputTokens: 16384,
				InputModalities: []Modality{ModalityText},
				SystemRole:      RoleSystem,
				UsageInStream:   true,
				ExtraBody:       map[string]any{"messages": []any{}},
			},
		},
	}

	errs := ValidateEffective(p)
	if !hasValidationError(errs, "models.gpt-5.4-mini.extra_body.messages") {
		t.Fatalf("ValidateEffective errors = %#v, want reserved model extra_body error", errs)
	}
}

func TestValidateEffectiveAllowsIncompleteCapacity(t *testing.T) {
	p := &Provider{
		ID:            "local",
		Name:          "local",
		Transport:     Transport{BaseURL: "http://localhost:11434/v1", APIKeyEnv: ""},
		SystemRole:    RoleSystem,
		UsageInStream: true,
		ExtraBody:     map[string]any{},
		Models: map[string]*Model{
			"qwen": {
				ID:              "qwen",
				Name:            "qwen",
				InputModalities: []Modality{ModalityText},
				SystemRole:      RoleSystem,
				UsageInStream:   true,
				ExtraBody:       map[string]any{},
			},
		},
	}

	if errs := ValidateEffective(p); len(errs) != 0 {
		t.Fatalf("ValidateEffective errors = %#v, want incomplete model to remain loadable", errs)
	}
}

func validRawProvider() map[string]any {
	return map[string]any{
		"id":   "openai",
		"name": "OpenAI",
		"transport": map[string]any{
			"base_url":    "https://api.openai.com/v1",
			"api_key_env": "OPENAI_API_KEY",
			"headers":     map[string]any{"OpenAI-Beta": "assistants=v2"},
			"options":     map[string]any{"timeout": float64(60)},
		},
		"system_role":     "system",
		"usage_in_stream": true,
		"extra_body":      map[string]any{"reasoning_effort": "medium"},
		"discovery":       true,
		"hidden":          false,
		"models": map[string]any{
			"gpt-5.4-mini": map[string]any{
				"name":              "GPT-5.4 mini",
				"context_window":    float64(128000),
				"max_output_tokens": float64(16384),
				"input_modalities":  []any{"text"},
				"system_role":       "system",
				"usage_in_stream":   true,
				"extra_body":        map[string]any{},
				"cost":              map[string]any{"input": float64(0.15), "output": float64(0.60)},
			},
		},
	}
}

func hasValidationError(errs []ValidationError, field string) bool {
	for _, err := range errs {
		if err.Field == field {
			return true
		}
	}
	return false
}
