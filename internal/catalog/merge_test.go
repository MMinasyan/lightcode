package catalog

import (
	"github.com/MMinasyan/lightcode/internal/testassert"
	"testing"
)

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
	testassert.Require(t, headers["OpenAI-Beta"] == "assistants=v2" && headers["HTTP-Referer"] == "https://lightcode.local", "bundled header missing after merge: %#v; user header missing after merge: %#v", headers, headers)

	model := merged["models"].(map[string]any)["gpt-5.4-mini"].(map[string]any)
	extra := model["extra_body"].(map[string]any)
	testassert.Equal(t, extra["reasoning_effort"], "high")
	{
		_, ok := extra["thinking"].(map[string]any)
		testassert.Require(t, ok, "thinking sidecar missing after merge: %#v", extra)
	}

	modalities := model["input_modalities"].([]any)
	testassert.Require(t, len(modalities) == 1 && modalities[0] == "text", "input_modalities = %#v, want replacement with [text]", modalities)
}

func TestDeepMergeCatalogDoesNotMutateInputs(t *testing.T) {
	bundled := map[string]any{"transport": map[string]any{"headers": map[string]any{"A": "b"}}}
	user := map[string]any{"transport": map[string]any{"headers": map[string]any{"C": "d"}}}

	merged := DeepMergeCatalog(bundled, user)
	merged["transport"].(map[string]any)["headers"].(map[string]any)["A"] = "changed"

	testassert.Equal(t, bundled["transport"].(map[string]any)["headers"].(map[string]any)["A"], "b")
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
	testassert.Require(t, ok, "nullable key missing after merge: %#v", extra)
	testassert.Require(t, value == nil, "nullable = %#v, want nil passthrough", value)
	testassert.Require(t, extra["keep"] == "bundled", "lower-layer sibling key missing after null passthrough: %#v", extra)
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
	testassert.Equal(t, merged["reasoning_effort"], "low")
	{
		_, ok := merged["transforms"]
		testassert.Require(t, ok, "provider key missing: %#v", merged)
	}
	thinking := merged["thinking"].(map[string]any)
	testassert.Require(t, len(thinking) == 1 && thinking["budget_tokens"] == float64(4000), "thinking = %#v, want shallow replacement", thinking)
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
	testassert.Require(t, ok, "nullable key missing after merge: %#v", merged)
	testassert.Require(t, value == nil, "nullable = %#v, want nil passthrough", value)
	testassert.Require(t, merged["keep"] == "provider", "lower-layer sibling key missing after null passthrough: %#v", merged)
}
