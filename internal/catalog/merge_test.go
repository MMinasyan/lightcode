package catalog

import "testing"

func TestDeepMergeCatalogMergesObjectsAndReplacesArrays(t *testing.T) {
	bundled := map[string]any{
		"transport": map[string]any{
			"base_url":    "https://openrouter.ai/api/v1",
			"api_key_env": "OPENROUTER_API_KEY",
			"headers": map[string]any{
				"OpenAI-Beta": "assistants=v2",
			},
		},
		"models": map[string]any{
			"gpt-5.4-mini": map[string]any{
				"input_modalities": []any{"text", "image"},
				"extra_body": map[string]any{
					"reasoning_effort": "medium",
				},
			},
		},
	}
	user := map[string]any{
		"transport": map[string]any{
			"headers": map[string]any{
				"HTTP-Referer": "https://lightcode.local",
			},
		},
		"models": map[string]any{
			"gpt-5.4-mini": map[string]any{
				"input_modalities": []any{"text"},
				"extra_body": map[string]any{
					"reasoning_effort": "high",
					"thinking": map[string]any{
						"budget_tokens": float64(4000),
					},
				},
			},
		},
	}

	merged := DeepMergeCatalog(bundled, user)

	headers := merged["transport"].(map[string]any)["headers"].(map[string]any)
	if headers["OpenAI-Beta"] != "assistants=v2" {
		t.Fatalf("bundled header missing after merge: %#v", headers)
	}
	if headers["HTTP-Referer"] != "https://lightcode.local" {
		t.Fatalf("user header missing after merge: %#v", headers)
	}

	model := merged["models"].(map[string]any)["gpt-5.4-mini"].(map[string]any)
	extra := model["extra_body"].(map[string]any)
	if extra["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", extra["reasoning_effort"])
	}
	if _, ok := extra["thinking"].(map[string]any); !ok {
		t.Fatalf("thinking sidecar missing after merge: %#v", extra)
	}

	modalities := model["input_modalities"].([]any)
	if len(modalities) != 1 || modalities[0] != "text" {
		t.Fatalf("input_modalities = %#v, want replacement with [text]", modalities)
	}
}

func TestDeepMergeCatalogDoesNotMutateInputs(t *testing.T) {
	bundled := map[string]any{"transport": map[string]any{"headers": map[string]any{"A": "b"}}}
	user := map[string]any{"transport": map[string]any{"headers": map[string]any{"C": "d"}}}

	merged := DeepMergeCatalog(bundled, user)
	merged["transport"].(map[string]any)["headers"].(map[string]any)["A"] = "changed"

	got := bundled["transport"].(map[string]any)["headers"].(map[string]any)["A"]
	if got != "b" {
		t.Fatalf("bundled input mutated: %v", got)
	}
}

func TestDeepMergeCatalogPreservesNullValues(t *testing.T) {
	bundled := map[string]any{
		"extra_body": map[string]any{
			"keep":     "bundled",
			"nullable": "bundled-value",
		},
	}
	user := map[string]any{
		"extra_body": map[string]any{
			"nullable": nil,
		},
	}

	merged := DeepMergeCatalog(bundled, user)
	extra := merged["extra_body"].(map[string]any)
	value, ok := extra["nullable"]
	if !ok {
		t.Fatalf("nullable key missing after merge: %#v", extra)
	}
	if value != nil {
		t.Fatalf("nullable = %#v, want nil passthrough", value)
	}
	if extra["keep"] != "bundled" {
		t.Fatalf("lower-layer sibling key missing after null passthrough: %#v", extra)
	}
}

func TestShallowMergeBodyMergesTopLevelOnly(t *testing.T) {
	provider := map[string]any{
		"transforms": []any{"middle-out"},
		"thinking": map[string]any{
			"enabled":       true,
			"budget_tokens": float64(1000),
		},
	}
	model := map[string]any{
		"reasoning_effort": "high",
		"thinking": map[string]any{
			"budget_tokens": float64(4000),
		},
	}
	runtime := map[string]any{"reasoning_effort": "low"}

	merged := ShallowMergeBody(provider, model, runtime)
	if merged["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v, want runtime override", merged["reasoning_effort"])
	}
	if _, ok := merged["transforms"]; !ok {
		t.Fatalf("provider key missing: %#v", merged)
	}
	thinking := merged["thinking"].(map[string]any)
	if len(thinking) != 1 || thinking["budget_tokens"] != float64(4000) {
		t.Fatalf("thinking = %#v, want shallow replacement", thinking)
	}
}

func TestShallowMergeBodyPreservesNullValues(t *testing.T) {
	provider := map[string]any{
		"keep":     "provider",
		"nullable": "provider-value",
	}
	model := map[string]any{
		"nullable": nil,
	}

	merged := ShallowMergeBody(provider, model)
	value, ok := merged["nullable"]
	if !ok {
		t.Fatalf("nullable key missing after merge: %#v", merged)
	}
	if value != nil {
		t.Fatalf("nullable = %#v, want nil passthrough", value)
	}
	if merged["keep"] != "provider" {
		t.Fatalf("lower-layer sibling key missing after null passthrough: %#v", merged)
	}
}
