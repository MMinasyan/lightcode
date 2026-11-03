package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	provider, model, err := result.Catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5.4-mini"})
	testassert.NoError(t, err)
	testassert.Equal(t, provider.Transport.BaseURL, "https://api.openai.com/v1")
	testassert.Require(t, model.Name == "GPT-5.4 mini" && model.ContextWindow == 400000 && model.MaxOutputTokens == 128000 && len(model.InputModalities) == 1 && model.InputModalities[0] == ModalityText, "model = %#v; InputModalities = %#v, want default [text]", model, model.InputModalities)
	testassert.Require(t, model.UsageInStream, "UsageInStream default/flattening was false")
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
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	provider, model, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "qwen2.5-coder:32b"})
	testassert.NoError(t, err)
	testassert.Require(t, provider.ID == "local" && provider.Name == "local", "provider defaults = %#v", provider)
	testassert.Equal(t, model.Name, "qwen2.5-coder:32b")
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
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	provider, ok := result.Catalog.Providers["local"]
	testassert.Require(t, ok && provider != nil, "provider local missing from catalog: %#v", result.Catalog.Providers)
	testassert.Require(t, provider.Discovery, "provider discovery = false, want true")
	testassert.Require(t, len(provider.Models) == 0, "provider models = %#v, want empty discovery-only model map", provider.Models)
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
		Records: map[string]DiscoveryRecord{
			"local": {
				TransportFingerprint: transportFingerprint(Transport{BaseURL: "http://localhost:11434/v1"}),
				Models: map[string]DiscoveredModel{
					"qwen3": {Name: "Qwen 3", ContextWindow: 32768, MaxOutputTokens: 8192},
				},
			},
		},
	})
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "qwen3"})
		testassert.Require(t, errors.Is(err, ErrUnknownModel), "Lookup error = %v, want ErrUnknownModel for config-only discovered model", err)
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
		Records: map[string]DiscoveryRecord{
			"local": {
				TransportFingerprint: transportFingerprint(Transport{BaseURL: "http://localhost:11434/v1"}),
				Models: map[string]DiscoveredModel{
					"qwen3": {Name: "Qwen 3", ContextWindow: 32768, MaxOutputTokens: 8192},
				},
			},
		},
	})
	testassert.Require(t, len(result.Warnings) == 1 && result.Warnings[0].Kind == "user_config_skip" && result.Warnings[0].Provider == "local", "warnings = %#v, want user_config_skip for invalid models", result.Warnings)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "qwen3"})
		testassert.Require(t, errors.Is(err, ErrUnknownProvider), "Lookup error = %v, want ErrUnknownProvider", err)
	}
}

func TestBuildMalformedTransportSkipsOnlyInvalidProvider(t *testing.T) {
	for _, transport := range []any{"bad", []any{"bad"}, nil} {
		t.Run(fmt.Sprintf("%T", transport), func(t *testing.T) {
			result := Build(BuildInputs{UserRaw: map[string]any{
				"invalid": map[string]any{
					"transport": transport,
					"models":    map[string]any{},
				},
				"valid": map[string]any{
					"transport": map[string]any{"base_url": "http://valid.test/v1", "api_key_env": ""},
					"models":    map[string]any{"m": map[string]any{"context_window": 1}},
				},
			}})
			{
				_, ok := result.Catalog.Providers["invalid"]
				testassert.Require(t, !ok, "invalid provider was retained")
			}
			{
				_, ok := result.Catalog.Providers["valid"]
				testassert.Require(t, ok, "valid sibling was omitted: %#v", result.Warnings)
			}
			testassert.Require(t, hasWarningForProvider(result.Warnings, "user_config_skip", "invalid"), "warnings = %#v, want user_config_skip for invalid", result.Warnings)
		})
	}
	t.Run("missing", func(t *testing.T) {
		result := Build(BuildInputs{UserRaw: map[string]any{
			"invalid": map[string]any{"models": map[string]any{}},
			"valid": map[string]any{
				"transport": map[string]any{"base_url": "http://valid.test/v1", "api_key_env": ""},
				"models":    map[string]any{"m": map[string]any{"context_window": 1}},
			},
		}})
		{
			_, ok := result.Catalog.Providers["invalid"]
			testassert.Require(t, !ok, "provider with missing transport was retained")
		}
		{
			_, ok := result.Catalog.Providers["valid"]
			testassert.Require(t, ok, "valid sibling was omitted: %#v", result.Warnings)
		}
		testassert.Require(t, hasWarningForProvider(result.Warnings, "user_config_skip", "invalid"), "warnings = %#v, want user_config_skip for invalid", result.Warnings)
	})
}

func TestBuildRejectsTrailingBundledProviderData(t *testing.T) {
	result := Build(BuildInputs{Bundled: map[string]json.RawMessage{
		"invalid": json.RawMessage(`{"id":"invalid","transport":{"base_url":"http://invalid.test/v1","api_key_env":""},"models":{}} {"extra":1}`),
		"valid":   rawProviderJSON(`{"id":"valid","transport":{"base_url":"http://valid.test/v1","api_key_env":""},"models":{"m":{"context_window":1}}}`),
	}})
	{
		_, ok := result.Catalog.Providers["invalid"]
		testassert.Require(t, !ok, "bundled provider with trailing data was retained")
	}
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "valid", Model: "m"})
		testassert.NoError(t, err)
	}
	testassert.Require(t, len(result.Warnings) == 1 && result.Warnings[0].Kind == "bundled_config_skip", "warnings = %#v, want one bundled_config_skip", result.Warnings)
}

// TestBuildLabelsModelSourcesFromSameAcceptedInputs pins the source labels
// stamped on the effective Model entries: they come from the same bundled
// layer, user layer, and accepted transport-bound discovery record that
// produced each provider in the same Build loop — custom providers are
// always user, a bundled provider's shipped model IDs are bundled, a bound
// record's models are discovered, and every remaining effective model is
// user. Models outside the effective catalog (a rejected provider, an
// unadopted record model) carry no entry at all rather than a label.
func TestBuildLabelsModelSourcesFromSameAcceptedInputs(t *testing.T) {
	bundledTransport := Transport{BaseURL: "http://bundled.test/v1"}
	result := Build(BuildInputs{
		Bundled: map[string]json.RawMessage{
			"bundled": rawProviderJSON(`{
				"id": "bundled",
				"transport": {"base_url": "http://bundled.test/v1", "api_key_env": ""},
				"discovery": true,
				"models": {"ship": {"context_window": 4096}, "overridden": {"context_window": 4096}}
			}`),
		},
		UserRaw: map[string]any{
			"bundled": map[string]any{
				"transport": map[string]any{"base_url": "http://bundled.test/v1", "api_key_env": ""},
				"models": map[string]any{
					"overridden": map[string]any{"context_window": 8192},
					"user_added": map[string]any{"context_window": 4096},
				},
			},
			"custom": map[string]any{
				"transport": map[string]any{"base_url": "http://custom.test/v1", "api_key_env": ""},
				"discovery": true,
				"models":    map[string]any{"mine": map[string]any{"context_window": 4096}},
			},
		},
		Records: map[string]DiscoveryRecord{
			"bundled": {TransportFingerprint: transportFingerprint(bundledTransport), Models: map[string]DiscoveredModel{
				"found":      {ContextWindow: 4096},
				"overridden": {ContextWindow: 1},
			}},
			"custom": {TransportFingerprint: transportFingerprint(Transport{BaseURL: "http://custom.test/v1"}), Models: map[string]DiscoveredModel{
				"unadopted": {ContextWindow: 4096},
			}},
			"absent": {TransportFingerprint: transportFingerprint(Transport{BaseURL: "http://nowhere.test/v1"}), Models: map[string]DiscoveredModel{
				"unbound": {ContextWindow: 4096},
			}},
		},
	})
	want := map[string]map[string]string{
		"bundled": {"ship": SourceBundled, "overridden": SourceBundled, "found": SourceDiscovered, "user_added": SourceUser},
		"custom":  {"mine": SourceUser},
	}
	if len(result.Catalog.Providers) != len(want) {
		t.Fatalf("accepted providers = %v, want exactly %v", providerIDs(result.Catalog), want)
	}
	for providerID, models := range want {
		provider := result.Catalog.Providers[providerID]
		if provider == nil || len(provider.Models) != len(models) {
			t.Fatalf("provider %q effective models = %d, want %d", providerID, len(provider.Models), len(models))
		}
		for modelID, source := range models {
			{
				got := provider.Models[modelID].Source
				testassert.Require(t, got == source, "source of %s/%s = %q, want %q", providerID, modelID, got, source)
			}
		}
	}
	{
		_, ok := result.Catalog.Providers["bundled"].Models["unadopted"]
		testassert.Require(t, !ok, "custom-provider discovery model was adopted into the bundled-provider catalog")
	}
	for _, absent := range []struct{ provider, model string }{
		{"custom", "unadopted"},
		{"bundled", "unbound"},
	} {
		{
			_, present := result.Catalog.Providers[absent.provider].Models[absent.model]
			testassert.Require(t, !present, "model %s/%s carries an effective entry outside the accepted layers", absent.provider, absent.model)
		}
	}
}

func providerIDs(catalog *Catalog) []string {
	out := make([]string, 0, len(catalog.Providers))
	for id := range catalog.Providers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func TestBuildIgnoresForeignDiscoveryRecord(t *testing.T) {
	transport := Transport{BaseURL: "http://configured.test/v1"}
	foreign := Transport{BaseURL: "http://foreign.test/v1"}
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"local": map[string]any{
				"transport": map[string]any{"base_url": transport.BaseURL, "api_key_env": ""},
				"models":    map[string]any{},
			},
		},
		Records: map[string]DiscoveryRecord{
			"local": {TransportFingerprint: transportFingerprint(foreign), Models: map[string]DiscoveredModel{"stale": {ContextWindow: 4096}}},
		},
	})
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "stale"})
		testassert.Require(t, errors.Is(err, ErrUnknownModel), "foreign discovery model lookup = %v, want unknown model", err)
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
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	provider, model, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "openai/gpt-5.4-mini"})
	testassert.NoError(t, err)
	testassert.Require(t, provider.Transport.Headers["OpenAI-Beta"] == "assistants=v2" && provider.Transport.Headers["HTTP-Referer"] == "https://lightcode.local", "headers = %#v", provider.Transport.Headers)
	body := ShallowMergeBody(provider.ExtraBody, model.ExtraBody)
	testassert.Require(t, body["reasoning_effort"] == "high", "sidecar body = %#v", body)
	{
		_, ok := body["thinking"]
		testassert.Require(t, ok, "thinking sidecar missing: %#v", body)
	}
	testassert.Require(t, len(model.InputModalities) == 1 && model.InputModalities[0] == ModalityText, "InputModalities = %#v, want user replacement", model.InputModalities)
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
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	_, inherited, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "anthropic/a"})
	testassert.NoError(t, err)
	testassert.Require(t, inherited.ProtocolMetadata != nil && inherited.ProtocolMetadata.Family == "anthropic-thinking", "inherited protocol metadata = %#v", inherited.ProtocolMetadata)
	testassert.Require(t, len(inherited.ProtocolMetadata.MustPreserve) == 1 && inherited.ProtocolMetadata.MustPreserve[0] == "reasoning_details", "inherited must_preserve = %#v", inherited.ProtocolMetadata.MustPreserve)

	_, overridden, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "anthropic/b"})
	testassert.NoError(t, err)
	testassert.Require(t, overridden.ProtocolMetadata != nil && overridden.ProtocolMetadata.Family == "anthropic-thinking-v2", "overridden protocol metadata = %#v", overridden.ProtocolMetadata)
	testassert.Require(t, len(overridden.ProtocolMetadata.MustPreserve) == 1 && overridden.ProtocolMetadata.MustPreserve[0] == "reasoning_details" && len(overridden.ProtocolMetadata.Drop) == 1 && overridden.ProtocolMetadata.Drop[0] == "model_drop", "overridden must_preserve = %#v; overridden drop = %#v", overridden.ProtocolMetadata.MustPreserve, overridden.ProtocolMetadata.Drop)
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
		Records: map[string]DiscoveryRecord{
			"local": {
				TransportFingerprint: transportFingerprint(Transport{BaseURL: "http://localhost:11434/v1"}),
				Models: map[string]DiscoveredModel{
					"known": {Name: "Discovered", ContextWindow: 32768, MaxOutputTokens: 8192, Cost: &Cost{Input: ptrFloat64(0.2)}},
					"new":   {Name: "New Model", ContextWindow: 65536, MaxOutputTokens: 4096, Cost: &Cost{Input: ptrFloat64(0.4)}},
				},
			},
		},
	})
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	_, known, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "known"})
	testassert.NoError(t, err)
	testassert.Require(t, known.Name == "Configured" && known.ContextWindow == 1000 && known.MaxOutputTokens == 100 && known.Cost != nil && known.Cost.Input != nil && *known.Cost.Input == 0.2, "known metadata = %#v, want configured metadata unchanged; known cost = %#v, want discovered input cost", known, known.Cost)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "new"})
		testassert.Require(t, errors.Is(err, ErrUnknownModel), "Lookup new error = %v, want ErrUnknownModel", err)
	}
}

func TestBuildConfigOnlyDiscoveryDoesNotOverrideUserCostFields(t *testing.T) {
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"local": map[string]any{
				"transport": map[string]any{"base_url": "http://localhost:11434/v1", "api_key_env": ""},
				"models": map[string]any{
					"known": map[string]any{
						"name":              "Configured",
						"context_window":    float64(1000),
						"max_output_tokens": float64(100),
						"cost":              map[string]any{"input": float64(99)},
					},
				},
			},
		},
		Records: map[string]DiscoveryRecord{
			"local": {
				TransportFingerprint: transportFingerprint(Transport{BaseURL: "http://localhost:11434/v1"}),
				Models: map[string]DiscoveredModel{
					"known": {Name: "Discovered", ContextWindow: 32768, MaxOutputTokens: 8192, Cost: &Cost{Input: ptrFloat64(0.2), Output: ptrFloat64(0.4)}},
					"new":   {Name: "New Model", ContextWindow: 65536, MaxOutputTokens: 4096, Cost: &Cost{Input: ptrFloat64(0.8), Output: ptrFloat64(1.2)}},
				},
			},
		},
	})
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	_, known, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "known"})
	testassert.NoError(t, err)
	testassert.Require(t, known.Name == "Configured" && known.ContextWindow == 1000 && known.MaxOutputTokens == 100, "known metadata = %#v, want configured metadata unchanged", known)
	testassert.Require(t, known.Cost != nil, "known cost = nil")
	testassert.Require(t, known.Cost.Input != nil && *known.Cost.Input == 99 && known.Cost.Output != nil && *known.Cost.Output == 0.4, "known cost input = %v, want user value 99; known cost output = %v, want discovered value 0.4", known.Cost.Input, known.Cost.Output)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "new"})
		testassert.Require(t, errors.Is(err, ErrUnknownModel), "Lookup new error = %v, want ErrUnknownModel", err)
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
		Records: map[string]DiscoveryRecord{
			"openrouter": {
				TransportFingerprint: transportFingerprint(Transport{BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"}),
				Models: map[string]DiscoveredModel{
					"remote": {Name: "Remote", ContextWindow: 32768, MaxOutputTokens: 8192},
				},
			},
		},
	})
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	if _, model, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "remote"}); err != nil {
		t.Fatalf("Lookup remote returned error: %v", err)
	} else if model.Name != "Remote" || model.ContextWindow != 32768 {
		t.Fatalf("remote model = %#v", model)
	}
}

func TestBuildFiltersRejectedDiscoveredOnlyModels(t *testing.T) {
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
		Records: map[string]DiscoveryRecord{
			"openrouter": {
				TransportFingerprint: transportFingerprint(Transport{BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"}),
				Models: map[string]DiscoveredModel{
					"image-only": {
						Name:          "Image Only",
						ContextWindow: 32768,
						metadata:      &discoveryModelMetadata{ArchitectureOutputModalities: []string{"image"}},
					},
					"old-cache": {
						Name:          "Old Cache",
						ContextWindow: 32768,
					},
					"vision-chat": {
						Name:          "Vision Chat",
						ContextWindow: 32768,
						metadata: &discoveryModelMetadata{
							ArchitectureInputModalities:  []string{"image", "text"},
							ArchitectureOutputModalities: []string{"text"},
						},
					},
				},
			},
		},
	})
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "image-only"})
		testassert.Require(t, errors.Is(err, ErrUnknownModel), "Lookup image-only error = %v, want ErrUnknownModel", err)
	}
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "old-cache"})
		testassert.NoError(t, err)
	}
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "vision-chat"})
		testassert.NoError(t, err)
	}
	for _, ref := range result.Catalog.AllModels() {
		testassert.Require(t, ref.String() != "openrouter/image-only", "rejected discovered model appeared in AllModels")
	}
}

func TestBuildDoesNotDeleteConfiguredModelRejectedByDiscoveryMetadata(t *testing.T) {
	result := Build(BuildInputs{
		Bundled: map[string]json.RawMessage{
			"openrouter": rawProviderJSON(`{
				"id": "openrouter",
				"name": "OpenRouter",
				"transport": {"base_url": "https://openrouter.ai/api/v1", "api_key_env": "OPENROUTER_API_KEY"},
				"discovery": true,
				"models": {
					"configured": {"name": "Configured", "context_window": 1000, "max_output_tokens": 100}
				}
			}`),
		},
		Records: map[string]DiscoveryRecord{
			"openrouter": {
				TransportFingerprint: transportFingerprint(Transport{BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"}),
				Models: map[string]DiscoveredModel{
					"configured": {
						Name:          "Discovered",
						ContextWindow: 2000,
						Cost:          &Cost{Input: ptrFloat64(0.5)},
						metadata:      &discoveryModelMetadata{ArchitectureOutputModalities: []string{"image"}},
					},
				},
			},
		},
	})
	testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v, want none", result.Warnings)
	_, model, err := result.Catalog.Lookup(ModelRef{Provider: "openrouter", Model: "configured"})
	testassert.NoError(t, err)
	testassert.Require(t, model.Name == "Configured" && model.ContextWindow == 1000 && model.Cost != nil && model.Cost.Input != nil && *model.Cost.Input == 0.5, "configured model = %#v, want bundled metadata preserved; configured cost = %#v, want discovery cost still merged", model, model.Cost)
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
	testassert.Require(t, len(result.Warnings) == 1 && result.Warnings[0].Kind == "incomplete_model", "warnings = %#v, want incomplete_model", result.Warnings)
	_, _, err := result.Catalog.Lookup(ModelRef{Provider: "local", Model: "qwen"})
	testassert.Require(t, errors.Is(err, ErrIncomplete), "Lookup error = %v, want ErrIncomplete", err)
	_, model, err := result.Catalog.LookupOrIncomplete(ModelRef{Provider: "local", Model: "qwen"})
	testassert.NoError(t, err)
	{
		_, ok := model.Incomplete()
		testassert.Require(t, ok, "model incomplete = false")
	}
	incomplete := result.Catalog.IncompleteModels()
	testassert.Require(t, len(incomplete) == 1 && incomplete[0].Ref.String() == "local/qwen", "IncompleteModels = %#v", incomplete)
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
	testassert.Require(t, len(result.Warnings) == 1 && result.Warnings[0].Kind == "user_config_skip" && result.Warnings[0].Provider == "bad", "warnings = %#v, want user_config_skip for bad", result.Warnings)
	{
		_, _, err := result.Catalog.Lookup(ModelRef{Provider: "bad", Model: "bad-model"})
		testassert.Require(t, errors.Is(err, ErrUnknownProvider), "Lookup error = %v, want ErrUnknownProvider", err)
	}
}

func TestBuildSkipsRetiredProviderControlsWithWarning(t *testing.T) {
	result := Build(BuildInputs{
		UserRaw: map[string]any{
			"retired": map[string]any{
				"transport": map[string]any{
					"base_url": "https://retired.test/v1", "api_key_env": "RETIRED_API_KEY",
					"options": map[string]any{"retries": float64(3)},
				},
				"max_tokens_field": "max_completion_tokens",
				"models":           map[string]any{"m": map[string]any{"context_window": float64(4096)}},
			},
			"valid": map[string]any{
				"transport": map[string]any{"base_url": "http://valid.test/v1", "api_key_env": ""},
				"models":    map[string]any{"m": map[string]any{"context_window": float64(4096)}},
			},
		},
	})
	testassert.Require(t, hasWarningForProvider(result.Warnings, "user_config_skip", "retired"), "warnings = %#v, want user_config_skip for the retired controls", result.Warnings)
	{
		_, ok := result.Catalog.Providers["retired"]
		testassert.Require(t, !ok, "a provider carrying retired controls stayed effective")
	}
	{
		_, ok := result.Catalog.Providers["valid"]
		testassert.Require(t, ok, "valid sibling was omitted: %#v", result.Warnings)
	}
}

// buildCostProviderDocument is the raw bundled provider for the
// discovery-cost rows: one OpenAI provider whose single model carries the
// row's cost object.
func buildCostProviderDocument(cost string) json.RawMessage {
	return rawProviderJSON(fmt.Sprintf(`{
		"id": "openai",
		"name": "OpenAI",
		"transport": {"base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY"},
		"discovery": true,
		"models": {
			"gpt-5.4-mini": {
				"name": "GPT-5.4 mini",
				"context_window": 400000,
				"max_output_tokens": 128000,
				"cost": %s
			}
		}
	}`, cost))
}

// buildCostInputs is one raw Build input over the nominal provider: the
// bundled cost, an optional user override for the same model's cost, and the
// matching discovery record.
func buildCostInputs(bundledCost string, userCost map[string]any, discovered *Cost) BuildInputs {
	inputs := BuildInputs{
		Bundled: map[string]json.RawMessage{"openai": buildCostProviderDocument(bundledCost)},
		Records: map[string]DiscoveryRecord{
			"openai": {
				TransportFingerprint: transportFingerprint(Transport{BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"}),
				Models: map[string]DiscoveredModel{
					"gpt-5.4-mini": {
						Name:          "GPT-5.4 mini",
						ContextWindow: 400000,
						Cost:          discovered,
					},
				},
			},
		},
	}
	if userCost != nil {
		inputs.UserRaw = map[string]any{
			"openai": map[string]any{
				"models": map[string]any{
					"gpt-5.4-mini": map[string]any{"cost": userCost},
				},
			},
		}
	}
	return inputs
}

func TestBuildDiscoveryMergesCostFields(t *testing.T) {
	rows := []struct {
		name                          string
		bundledCost                   string
		userCost                      map[string]any
		discovered                    *Cost
		wantInput, wantOutput         *float64
		wantCacheRead, wantCacheWrite *float64
	}{
		{
			name:        "updates bundled input and output",
			bundledCost: `{"input": 10, "output": 20}`,
			discovered:  &Cost{Input: ptrFloat64(15), Output: ptrFloat64(30)},
			wantInput:   ptrFloat64(15),
			wantOutput:  ptrFloat64(30),
		},
		{
			name:          "adds discovered cache_read",
			bundledCost:   `{"input": 10, "output": 20}`,
			discovered:    &Cost{Input: ptrFloat64(5), Output: ptrFloat64(12), CacheRead: ptrFloat64(1.5)},
			wantInput:     ptrFloat64(5),
			wantOutput:    ptrFloat64(12),
			wantCacheRead: ptrFloat64(1.5),
		},
		{
			name:          "user field wins over discovery",
			bundledCost:   `{"input": 10, "output": 20, "cache_read": 2}`,
			userCost:      map[string]any{"input": 99.0},
			discovered:    &Cost{Input: ptrFloat64(8), Output: ptrFloat64(12), CacheRead: ptrFloat64(1)},
			wantInput:     ptrFloat64(99),
			wantOutput:    ptrFloat64(12),
			wantCacheRead: ptrFloat64(1),
		},
		{
			name:          "preserves bundled fields discovery omits",
			bundledCost:   `{"input": 10, "output": 20, "cache_read": 3}`,
			discovered:    &Cost{Input: ptrFloat64(8)},
			wantInput:     ptrFloat64(8),
			wantOutput:    ptrFloat64(20),
			wantCacheRead: ptrFloat64(3),
		},
		{
			name:        "user-owned zero input is preserved",
			bundledCost: `{"input": 10, "output": 20}`,
			userCost:    map[string]any{"input": 0.0},
			discovered:  &Cost{Input: ptrFloat64(8), Output: ptrFloat64(12)},
			wantInput:   ptrFloat64(0),
			wantOutput:  ptrFloat64(12),
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			result := Build(buildCostInputs(row.bundledCost, row.userCost, row.discovered))
			testassert.Require(t, len(result.Warnings) == 0, "Build warnings = %#v", result.Warnings)
			_, model, err := result.Catalog.Lookup(ModelRef{Provider: "openai", Model: "gpt-5.4-mini"})
			testassert.NoError(t, err)
			assertCostFields(t, model.Cost, row.wantInput, row.wantOutput, row.wantCacheRead, row.wantCacheWrite)
		})
	}
}

func ptrFloat64(v float64) *float64 { return &v }

// assertCostFields checks each cost subfield's independent presence and value.
// A nil want is an absent field, not a zero.
func assertCostFields(t *testing.T, cost *Cost, input, output, cacheRead, cacheWrite *float64) {
	t.Helper()
	testassert.Require(t, cost != nil, "model.Cost = nil")
	for _, field := range []struct {
		name      string
		got, want *float64
	}{
		{"input", cost.Input, input},
		{"output", cost.Output, output},
		{"cache_read", cost.CacheRead, cacheRead},
		{"cache_write", cost.CacheWrite, cacheWrite},
	} {
		if field.want == nil {
			testassert.Require(t, field.got == nil, "%s = %s, want absent", field.name, costFieldValue(field.got))
			continue
		}
		testassert.Require(t, field.got != nil && *field.got == *field.want, "%s = %s, want %g", field.name, costFieldValue(field.got), *field.want)
	}
}

func costFieldValue(v *float64) string {
	if v == nil {
		return "absent"
	}
	return fmt.Sprintf("%g", *v)
}

func BenchmarkBuildCatalog(b *testing.B) {
	bundled, err := readBundledProviders(bundledFS)
	testassert.Require(b, err == nil, "read bundled providers: %v", err)
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
