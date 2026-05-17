package catalog

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestBuildCatalogFromBundledOnly(t *testing.T) {
	result := Build(BuildInputs{
		Bundled: map[string]json.RawMessage{
			"openai": rawProviderJSON(`{
				"id": "openai",
				"name": "OpenAI",
				"transport": {"base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY"},
				"system_role": "system",
				"usage_in_stream": true,
				"discovery": true,
				"models": {
					"gpt-5.4-mini": {"name": "GPT-5.4 mini", "context_window": 400000, "max_output_tokens": 128000}
				}
			}`),
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v, want none", result.Warnings)
	}
	provider, model, err := result.Catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5.4-mini"})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if provider.Transport.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("BaseURL = %q", provider.Transport.BaseURL)
	}
	if model.Name != "GPT-5.4 mini" || model.ContextWindow != 400000 || model.MaxOutputTokens != 128000 {
		t.Fatalf("model = %#v", model)
	}
	if len(model.InputModalities) != 1 || model.InputModalities[0] != ModalityText {
		t.Fatalf("InputModalities = %#v, want default [text]", model.InputModalities)
	}
	if !model.UsageInStream {
		t.Fatalf("UsageInStream default/flattening was false")
	}
}

func TestBuildCatalogFromUserOnlyCustomProvider(t *testing.T) {
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"local": map[string]any{
				"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"models": map[string]any{
					"qwen2.5-coder:32b": map[string]any{"context_window": float64(32768), "max_output_tokens": float64(8192)},
				},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v, want none", result.Warnings)
	}
	provider, model, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "qwen2.5-coder:32b"})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if provider.ID != "local" || provider.Name != "local" {
		t.Fatalf("provider defaults = %#v", provider)
	}
	if model.Name != "qwen2.5-coder:32b" {
		t.Fatalf("model name = %q, want id fallback", model.Name)
	}
}

func TestBuildKeepsDiscoveryOnlyProviderWithEmptyModels(t *testing.T) {
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"local": map[string]any{
				"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"discovery": true,
				"models":    map[string]any{},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v, want none", result.Warnings)
	}
	provider, ok := result.Catalog.Providers["local"]
	if !ok || provider == nil {
		t.Fatalf("provider local missing from catalog: %#v", result.Catalog.Providers)
	}
	if !provider.Discovery {
		t.Fatalf("provider discovery = false, want true")
	}
	if len(provider.Models) != 0 {
		t.Fatalf("provider models = %#v, want empty discovery-only model map", provider.Models)
	}
}

func TestBuildConfigOnlyProviderDoesNotAddDiscoveredModelsFromCache(t *testing.T) {
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"local": map[string]any{
				"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"discovery": true,
				"models":    map[string]any{},
			},
		},
		Cache: map[string]DiscoveredProvider{
			"local": {
				Models: map[string]DiscoveredModel{
					"qwen3": {Name: "Qwen 3", ContextWindow: 32768, MaxOutputTokens: 8192},
				},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v, want none", result.Warnings)
	}
	if _, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "qwen3"}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("Lookup error = %v, want ErrUnknownModel for config-only discovered model", err)
	}
}

func TestBuildDoesNotMaskInvalidModelsWithDiscoveryCache(t *testing.T) {
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"local": map[string]any{
				"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"models":    "not an object",
				"discovery": true,
			},
		},
		Cache: map[string]DiscoveredProvider{
			"local": {
				Models: map[string]DiscoveredModel{
					"qwen3": {Name: "Qwen 3", ContextWindow: 32768, MaxOutputTokens: 8192},
				},
			},
		},
	})
	if len(result.Warnings) != 1 || result.Warnings[0].Kind != "user_config_skip" || result.Warnings[0].Provider != "local" {
		t.Fatalf("warnings = %#v, want user_config_skip for invalid models", result.Warnings)
	}
	if _, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "qwen3"}); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("Lookup error = %v, want ErrUnknownProvider", err)
	}
}

func TestBuildDeepMergesUserOverridesAndPassesSidecarsThrough(t *testing.T) {
	result := Build(BuildInputs{
		Bundled: map[string]json.RawMessage{
			"openrouter": rawProviderJSON(`{
				"id": "openrouter",
				"name": "OpenRouter",
				"transport": {
					"base_url": "https://openrouter.ai/api/v1",
					"api_key_env": "OPENROUTER_API_KEY",
					"headers": {"OpenAI-Beta": "assistants=v2"}
				},
				"extra_body": {"transforms": ["middle-out"]},
				"models": {
					"openai/gpt-5.4-mini": {
						"context_window": 200000,
						"max_output_tokens": 100000,
						"input_modalities": ["text", "image"],
						"extra_body": {"reasoning_effort": "medium"}
					}
				}
			}`),
		},
		UserRaw: map[string]any{
			"openrouter": map[string]any{
				"transport": map[string]any{"headers": map[string]any{"HTTP-Referer": "https://lightcode.local"}},
				"models": map[string]any{
					"openai/gpt-5.4-mini": map[string]any{
						"input_modalities": []any{"text"},
						"extra_body": map[string]any{
							"reasoning_effort": "high",
							"thinking":         map[string]any{"budget_tokens": float64(4000)},
						},
					},
				},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v, want none", result.Warnings)
	}
	provider, model, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "openai/gpt-5.4-mini"})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if provider.Transport.Headers["OpenAI-Beta"] != "assistants=v2" || provider.Transport.Headers["HTTP-Referer"] != "https://lightcode.local" {
		t.Fatalf("headers = %#v", provider.Transport.Headers)
	}
	body := ShallowMergeBody(provider.ExtraBody, model.ExtraBody)
	if body["reasoning_effort"] != "high" {
		t.Fatalf("sidecar body = %#v", body)
	}
	if _, ok := body["thinking"]; !ok {
		t.Fatalf("thinking sidecar missing: %#v", body)
	}
	if len(model.InputModalities) != 1 || model.InputModalities[0] != ModalityText {
		t.Fatalf("InputModalities = %#v, want user replacement", model.InputModalities)
	}
}

func TestBuildProtocolMetadataInheritsAndOverrides(t *testing.T) {
	result := Build(BuildInputs{
		Bundled: map[string]json.RawMessage{
			"openrouter": rawProviderJSON(`{
				"id": "openrouter",
				"name": "OpenRouter",
				"transport": {"base_url": "https://openrouter.ai/api/v1", "api_key_env": "OPENROUTER_API_KEY"},
				"protocol_metadata": {
					"family": "anthropic-thinking",
					"must_preserve": ["reasoning_details"],
					"drop": ["provider_drop"]
				},
				"models": {
					"anthropic/a": {
						"context_window": 200000,
						"max_output_tokens": 64000
					},
					"anthropic/b": {
						"context_window": 200000,
						"max_output_tokens": 64000,
						"protocol_metadata": {
							"family": "anthropic-thinking-v2",
							"drop": ["model_drop"]
						}
					}
				}
			}`),
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v, want none", result.Warnings)
	}
	_, inherited, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "anthropic/a"})
	if err != nil {
		t.Fatalf("Lookup inherited returned error: %v", err)
	}
	if inherited.ProtocolMetadata == nil || inherited.ProtocolMetadata.Family != "anthropic-thinking" {
		t.Fatalf("inherited protocol metadata = %#v", inherited.ProtocolMetadata)
	}
	if len(inherited.ProtocolMetadata.MustPreserve) != 1 || inherited.ProtocolMetadata.MustPreserve[0] != "reasoning_details" {
		t.Fatalf("inherited must_preserve = %#v", inherited.ProtocolMetadata.MustPreserve)
	}

	_, overridden, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "anthropic/b"})
	if err != nil {
		t.Fatalf("Lookup overridden returned error: %v", err)
	}
	if overridden.ProtocolMetadata == nil || overridden.ProtocolMetadata.Family != "anthropic-thinking-v2" {
		t.Fatalf("overridden protocol metadata = %#v", overridden.ProtocolMetadata)
	}
	if len(overridden.ProtocolMetadata.MustPreserve) != 1 || overridden.ProtocolMetadata.MustPreserve[0] != "reasoning_details" {
		t.Fatalf("overridden must_preserve = %#v", overridden.ProtocolMetadata.MustPreserve)
	}
	if len(overridden.ProtocolMetadata.Drop) != 1 || overridden.ProtocolMetadata.Drop[0] != "model_drop" {
		t.Fatalf("overridden drop = %#v", overridden.ProtocolMetadata.Drop)
	}
}

func TestBuildConfigOnlyDiscoveryCacheOnlyUpdatesKnownModelCost(t *testing.T) {
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"local": map[string]any{
				"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"models": map[string]any{
					"known": map[string]any{"name": "Configured", "context_window": float64(1000), "max_output_tokens": float64(100)},
				},
			},
		},
		Cache: map[string]DiscoveredProvider{
			"local": {
				Models: map[string]DiscoveredModel{
					"known": {Name: "Discovered", ContextWindow: 32768, MaxOutputTokens: 8192, Cost: &Cost{Input: ptrFloat64(0.2)}},
					"new":   {Name: "New Model", ContextWindow: 65536, MaxOutputTokens: 4096, Cost: &Cost{Input: ptrFloat64(0.4)}},
				},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v, want none", result.Warnings)
	}
	_, known, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "known"})
	if err != nil {
		t.Fatalf("Lookup known returned error: %v", err)
	}
	if known.Name != "Configured" || known.ContextWindow != 1000 || known.MaxOutputTokens != 100 {
		t.Fatalf("known metadata = %#v, want configured metadata unchanged", known)
	}
	if known.Cost == nil || known.Cost.Input == nil || *known.Cost.Input != 0.2 {
		t.Fatalf("known cost = %#v, want discovered input cost", known.Cost)
	}
	if _, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "new"}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("Lookup new error = %v, want ErrUnknownModel", err)
	}
}

func TestBuildBundledDiscoveryCacheCanAddModels(t *testing.T) {
	result := Build(BuildInputs{
		Bundled: map[string]json.RawMessage{
			"openrouter": rawProviderJSON(`{
				"id": "openrouter",
				"name": "OpenRouter",
				"transport": {"base_url": "https://openrouter.ai/api/v1", "api_key_env": "OPENROUTER_API_KEY"},
				"discovery": true,
				"models": {}
			}`),
		},
		Cache: map[string]DiscoveredProvider{
			"openrouter": {
				Models: map[string]DiscoveredModel{
					"remote": {Name: "Remote", ContextWindow: 32768, MaxOutputTokens: 8192},
				},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v, want none", result.Warnings)
	}
	if _, model, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "remote"}); err != nil {
		t.Fatalf("Lookup remote returned error: %v", err)
	} else if model.Name != "Remote" || model.ContextWindow != 32768 {
		t.Fatalf("remote model = %#v", model)
	}
}

func TestBuildKeepsIncompleteModelsAndWarns(t *testing.T) {
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"local": map[string]any{
				"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"models":    map[string]any{"qwen": map[string]any{}},
			},
		},
	})
	if len(result.Warnings) != 1 || result.Warnings[0].Kind != "incomplete_model" {
		t.Fatalf("warnings = %#v, want incomplete_model", result.Warnings)
	}
	_, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "qwen"})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Lookup error = %v, want ErrIncomplete", err)
	}
	_, model, err := result.Catalog.LookupOrIncomplete(ModelRef{Provider: "local", Model: "qwen"})
	if err != nil {
		t.Fatalf("LookupOrIncomplete error = %v", err)
	}
	if _, ok := model.Incomplete(); !ok {
		t.Fatalf("model incomplete = false")
	}
	incomplete := result.Catalog.IncompleteModels()
	if len(incomplete) != 1 || incomplete[0].Ref.String() != "local/qwen" {
		t.Fatalf("IncompleteModels = %#v", incomplete)
	}
}

func TestBuildSkipsReservedKeyViolationWithWarning(t *testing.T) {
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"bad": map[string]any{
				"transport": map[string]any{"base_url": "https://example.com/v1", "api_key_env": "BAD_API_KEY"},
				"models": map[string]any{
					"bad-model": map[string]any{
						"context_window":    float64(128000),
						"max_output_tokens": float64(4096),
						"extra_body":        map[string]any{"model": "not allowed"},
					},
				},
			},
		},
	})
	if len(result.Warnings) != 1 || result.Warnings[0].Kind != "user_config_skip" || result.Warnings[0].Provider != "bad" {
		t.Fatalf("warnings = %#v, want user_config_skip for bad", result.Warnings)
	}
	if _, _, err := result.Catalog.Lookup(ModelRef{Provider: "bad", Model: "bad-model"}); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("Lookup error = %v, want ErrUnknownProvider", err)
	}
}

func TestBuildDiscoveryUpdatesBundledCostFields(t *testing.T) {
	// Bundled model has cost with input=10, output=20.
	// Discovery cache has updated cost with input=15, output=30.
	// Discovery should update the cost fields.
	in15 := 15.0
	out30 := 30.0
	result := Build(BuildInputs{
		Bundled: map[string]json.RawMessage{
			"openai": rawProviderJSON(`{
				"id": "openai",
				"name": "OpenAI",
				"transport": {"base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY"},
				"discovery": true,
				"models": {
					"gpt-5.4-mini": {
						"name": "GPT-5.4 mini",
						"context_window": 400000,
						"max_output_tokens": 128000,
						"cost": {"input": 10, "output": 20}
					}
				}
			}`),
		},
		Cache: map[string]DiscoveredProvider{
			"openai": {
				Models: map[string]DiscoveredModel{
					"gpt-5.4-mini": {
						Name:          "GPT-5.4 mini",
						ContextWindow: 400000,
						Cost:          &Cost{Input: &in15, Output: &out30},
					},
				},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v", result.Warnings)
	}
	_, model, err := result.Catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5.4-mini"})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if model.Cost == nil {
		t.Fatalf("model.Cost = nil, want cost from discovery")
	}
	if model.Cost.Input == nil || *model.Cost.Input != 15 {
		t.Fatalf("model.Cost.Input = %v, want 15 from discovery", model.Cost.Input)
	}
	if model.Cost.Output == nil || *model.Cost.Output != 30 {
		t.Fatalf("model.Cost.Output = %v, want 30 from discovery", model.Cost.Output)
	}
}

func TestBuildDiscoveryMergesCostSubfieldsOverBundled(t *testing.T) {
	// Bundled has input/output. Discovery has input/output/cache_read.
	// Discovery should merge: update input/output, add cache_read.
	in5 := 5.0
	out12 := 12.0
	cacheRead := 1.5
	result := Build(BuildInputs{
		Bundled: map[string]json.RawMessage{
			"openai": rawProviderJSON(`{
				"id": "openai",
				"name": "OpenAI",
				"transport": {"base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY"},
				"discovery": true,
				"models": {
					"gpt-5.4-mini": {
						"name": "GPT-5.4 mini",
						"context_window": 400000,
						"max_output_tokens": 128000,
						"cost": {"input": 10, "output": 20}
					}
				}
			}`),
		},
		Cache: map[string]DiscoveredProvider{
			"openai": {
				Models: map[string]DiscoveredModel{
					"gpt-5.4-mini": {
						Name:          "GPT-5.4 mini",
						ContextWindow: 400000,
						Cost:          &Cost{Input: &in5, Output: &out12, CacheRead: &cacheRead},
					},
				},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v", result.Warnings)
	}
	_, model, err := result.Catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5.4-mini"})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if model.Cost == nil {
		t.Fatalf("model.Cost = nil")
	}
	if model.Cost.Input == nil || *model.Cost.Input != 5 {
		t.Fatalf("model.Cost.Input = %v, want 5 (updated by discovery)", model.Cost.Input)
	}
	if model.Cost.Output == nil || *model.Cost.Output != 12 {
		t.Fatalf("model.Cost.Output = %v, want 12 (updated by discovery)", model.Cost.Output)
	}
	if model.Cost.CacheRead == nil || *model.Cost.CacheRead != 1.5 {
		t.Fatalf("model.Cost.CacheRead = %v, want 1.5 (added by discovery)", model.Cost.CacheRead)
	}
}

func TestBuildDiscoveryDoesNotOverrideUserCostFields(t *testing.T) {
	// User cost is an explicit override. Discovery may update bundled prices, but
	// must not overwrite fields the user set.
	result := Build(BuildInputs{
		Bundled: map[string]json.RawMessage{
			"openai": rawProviderJSON(`{
				"id": "openai",
				"name": "OpenAI",
				"transport": {"base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY"},
				"discovery": true,
				"models": {
					"gpt-5.4-mini": {
						"name": "GPT-5.4 mini",
						"context_window": 400000,
						"max_output_tokens": 128000,
						"cost": {"input": 10, "output": 20, "cache_read": 2}
					}
				}
			}`),
		},
		UserRaw: map[string]any{
			"openai": map[string]any{
				"models": map[string]any{
					"gpt-5.4-mini": map[string]any{
						"cost": map[string]any{"input": 99.0},
					},
				},
			},
		},
		Cache: map[string]DiscoveredProvider{
			"openai": {
				Models: map[string]DiscoveredModel{
					"gpt-5.4-mini": {
						Name:          "GPT-5.4 mini",
						ContextWindow: 400000,
						Cost:          &Cost{Input: ptrFloat64(8), Output: ptrFloat64(12), CacheRead: ptrFloat64(1)},
					},
				},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v", result.Warnings)
	}
	_, model, err := result.Catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5.4-mini"})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if model.Cost == nil {
		t.Fatalf("model.Cost = nil")
	}
	if model.Cost.Input == nil || *model.Cost.Input != 99 {
		t.Fatalf("model.Cost.Input = %v, want 99 from user override", model.Cost.Input)
	}
	if model.Cost.Output == nil || *model.Cost.Output != 12 {
		t.Fatalf("model.Cost.Output = %v, want 12 from discovery", model.Cost.Output)
	}
	if model.Cost.CacheRead == nil || *model.Cost.CacheRead != 1 {
		t.Fatalf("model.Cost.CacheRead = %v, want 1 from discovery", model.Cost.CacheRead)
	}
}

func TestBuildDiscoveryPreservesExistingCostFieldsItDoesNotProvide(t *testing.T) {
	// Bundled has input/output/cache_read. Discovery only has input.
	// Discovery should update input, but keep output and cache_read from bundled.
	cacheRead := 3.0
	result := Build(BuildInputs{
		Bundled: map[string]json.RawMessage{
			"openai": rawProviderJSON(`{
				"id": "openai",
				"name": "OpenAI",
				"transport": {"base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY"},
				"discovery": true,
				"models": {
					"gpt-5.4-mini": {
						"name": "GPT-5.4 mini",
						"context_window": 400000,
						"max_output_tokens": 128000,
						"cost": {"input": 10, "output": 20, "cache_read": 3}
					}
				}
			}`),
		},
		Cache: map[string]DiscoveredProvider{
			"openai": {
				Models: map[string]DiscoveredModel{
					"gpt-5.4-mini": {
						Name:          "GPT-5.4 mini",
						ContextWindow: 400000,
						Cost:          &Cost{Input: ptrFloat64(8)},
					},
				},
			},
		},
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("Build warnings = %#v", result.Warnings)
	}
	_, model, err := result.Catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5.4-mini"})
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if model.Cost == nil {
		t.Fatalf("model.Cost = nil")
	}
	if model.Cost.Input == nil || *model.Cost.Input != 8 {
		t.Fatalf("model.Cost.Input = %v, want 8 (updated by discovery)", model.Cost.Input)
	}
	if model.Cost.Output == nil || *model.Cost.Output != 20 {
		t.Fatalf("model.Cost.Output = %v, want 20 (preserved from bundled)", model.Cost.Output)
	}
	if model.Cost.CacheRead == nil || *model.Cost.CacheRead != cacheRead {
		t.Fatalf("model.Cost.CacheRead = %v, want 3 (preserved from bundled)", model.Cost.CacheRead)
	}
}

func ptrFloat64(v float64) *float64 { return &v }

func BenchmarkBuildCatalog(b *testing.B) {
	bundled, err := readBundledProviders(bundledFS)
	if err != nil {
		b.Fatalf("read bundled providers: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := Build(BuildInputs{Bundled: bundled})
		if len(result.Warnings) != 0 {
			b.Fatalf("Build warnings = %#v", result.Warnings)
		}
		if len(result.Catalog.Providers) == 0 {
			b.Fatal("built catalog has no providers")
		}
	}
}

func rawProviderJSON(s string) json.RawMessage {
	return json.RawMessage(s)
}
