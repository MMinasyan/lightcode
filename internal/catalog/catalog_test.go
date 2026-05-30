package catalog

import (
	"errors"
	"testing"
)

func TestCatalogLookupReturnsUnknownErrors(t *testing.T) {
	cat := &Catalog{Providers: map[string]*Provider{
		"openai": {
			ID:            "openai",
			Name:          "OpenAI",
			Transport:     Transport{BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
			SystemRole:    RoleSystem,
			UsageInStream: true,
			ExtraBody:     map[string]any{},
			Models:        map[string]*Model{},
		},
	}}

	if _, _, err := cat.Lookup(ModelRef{Provider: "missing", Model: "gpt"}); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("missing provider err = %v, want ErrUnknownProvider", err)
	}
	if _, _, err := cat.Lookup(ModelRef{Provider: "openai", Model: "missing"}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("missing model err = %v, want ErrUnknownModel", err)
	}
}

func TestMergeDiscoveredProviderUpdatesExistingCost(t *testing.T) {
	// Model already has cost with input=10, output=20.
	// Discovery provides input=15, output=30, cache_read=5.
	// Discovery should update all cost subfields.
	in15 := 15.0
	out30 := 30.0
	cacheRead := 5.0
	cat := &Catalog{Providers: map[string]*Provider{
		"openai": {
			ID:            "openai",
			Name:          "OpenAI",
			Transport:     Transport{BaseURL: "https://api.openai.com/v1"},
			SystemRole:    RoleSystem,
			UsageInStream: true,
			ExtraBody:     map[string]any{},
			Models: map[string]*Model{
				"gpt-5.4-mini": {
					ID:              "gpt-5.4-mini",
					Name:            "GPT-5.4 mini",
					ContextWindow:   400000,
					MaxOutputTokens: 128000,
					InputModalities: []Modality{ModalityText},
					SystemRole:      RoleSystem,
					UsageInStream:   true,
					ExtraBody:       map[string]any{},
					Cost:            &Cost{Input: ptrFloat64(10), Output: ptrFloat64(20)},
				},
			},
		},
	}}
	err := cat.MergeDiscoveredProvider("openai", DiscoveredProvider{
		Models: map[string]DiscoveredModel{
			"gpt-5.4-mini": {
				Name:          "GPT-5.4 mini",
				ContextWindow: 400000,
				Cost:          &Cost{Input: &in15, Output: &out30, CacheRead: &cacheRead},
			},
		},
	})
	if err != nil {
		t.Fatalf("MergeDiscoveredProvider error: %v", err)
	}
	model := cat.Providers["openai"].Models["gpt-5.4-mini"]
	if model.Cost == nil {
		t.Fatalf("model.Cost = nil")
	}
	if model.Cost.Input == nil || *model.Cost.Input != 15 {
		t.Fatalf("model.Cost.Input = %v, want 15", model.Cost.Input)
	}
	if model.Cost.Output == nil || *model.Cost.Output != 30 {
		t.Fatalf("model.Cost.Output = %v, want 30", model.Cost.Output)
	}
	if model.Cost.CacheRead == nil || *model.Cost.CacheRead != 5 {
		t.Fatalf("model.Cost.CacheRead = %v, want 5", model.Cost.CacheRead)
	}
}

func TestMergeDiscoveredProviderPreservesCostFieldsDiscoveryDoesNotProvide(t *testing.T) {
	// Model has input/output/cache_read/cache_write.
	// Discovery only provides input/output.
	// Should update input/output but keep cache_read and cache_write.
	cat := &Catalog{Providers: map[string]*Provider{
		"openai": {
			ID:            "openai",
			Name:          "OpenAI",
			Transport:     Transport{BaseURL: "https://api.openai.com/v1"},
			SystemRole:    RoleSystem,
			UsageInStream: true,
			ExtraBody:     map[string]any{},
			Models: map[string]*Model{
				"gpt-5.4-mini": {
					ID:              "gpt-5.4-mini",
					Name:            "GPT-5.4 mini",
					ContextWindow:   400000,
					MaxOutputTokens: 128000,
					InputModalities: []Modality{ModalityText},
					SystemRole:      RoleSystem,
					UsageInStream:   true,
					ExtraBody:       map[string]any{},
					Cost:            &Cost{Input: ptrFloat64(10), Output: ptrFloat64(20), CacheRead: ptrFloat64(3), CacheWrite: ptrFloat64(7)},
				},
			},
		},
	}}
	err := cat.MergeDiscoveredProvider("openai", DiscoveredProvider{
		Models: map[string]DiscoveredModel{
			"gpt-5.4-mini": {
				Name:          "GPT-5.4 mini",
				ContextWindow: 400000,
				Cost:          &Cost{Input: ptrFloat64(12), Output: ptrFloat64(25)},
			},
		},
	})
	if err != nil {
		t.Fatalf("MergeDiscoveredProvider error: %v", err)
	}
	model := cat.Providers["openai"].Models["gpt-5.4-mini"]
	if model.Cost == nil {
		t.Fatalf("model.Cost = nil")
	}
	if model.Cost.Input == nil || *model.Cost.Input != 12 {
		t.Fatalf("model.Cost.Input = %v, want 12 (updated by discovery)", model.Cost.Input)
	}
	if model.Cost.Output == nil || *model.Cost.Output != 25 {
		t.Fatalf("model.Cost.Output = %v, want 25 (updated by discovery)", model.Cost.Output)
	}
	if model.Cost.CacheRead == nil || *model.Cost.CacheRead != 3 {
		t.Fatalf("model.Cost.CacheRead = %v, want 3 (preserved from existing)", model.Cost.CacheRead)
	}
	if model.Cost.CacheWrite == nil || *model.Cost.CacheWrite != 7 {
		t.Fatalf("model.Cost.CacheWrite = %v, want 7 (preserved from existing)", model.Cost.CacheWrite)
	}
}

func TestMergeDiscoveredProviderWithCostProtectionPreservesUserFields(t *testing.T) {
	cat := &Catalog{Providers: map[string]*Provider{
		"openai": {
			ID:            "openai",
			Name:          "OpenAI",
			Transport:     Transport{BaseURL: "https://api.openai.com/v1"},
			SystemRole:    RoleSystem,
			UsageInStream: true,
			ExtraBody:     map[string]any{},
			Builtin:       true,
			Models: map[string]*Model{
				"gpt-5.4-mini": {
					ID:              "gpt-5.4-mini",
					Name:            "GPT-5.4 mini",
					InputModalities: []Modality{ModalityText},
					SystemRole:      RoleSystem,
					UsageInStream:   true,
					ExtraBody:       map[string]any{},
					Cost:            &Cost{Input: ptrFloat64(99), Output: ptrFloat64(20), CacheRead: ptrFloat64(3), CacheWrite: ptrFloat64(7)},
				},
				"omitted-costs": {
					ID:              "omitted-costs",
					Name:            "Omitted Costs",
					InputModalities: []Modality{ModalityText},
					SystemRole:      RoleSystem,
					UsageInStream:   true,
					ExtraBody:       map[string]any{},
					Cost:            &Cost{Input: ptrFloat64(10), Output: ptrFloat64(11), CacheRead: ptrFloat64(12), CacheWrite: ptrFloat64(13)},
				},
			},
		},
	}}
	err := cat.MergeDiscoveredProviderWithCostProtection("openai", DiscoveredProvider{
		Models: map[string]DiscoveredModel{
			"gpt-5.4-mini": {
				Cost: &Cost{
					Input:      ptrFloat64(1),
					Output:     ptrFloat64(2),
					CacheRead:  ptrFloat64(4),
					CacheWrite: ptrFloat64(5),
				},
			},
			"omitted-costs": {
				Cost: &Cost{Input: ptrFloat64(15)},
			},
		},
	}, map[string]map[string]bool{
		"gpt-5.4-mini": {"input": true},
	})
	if err != nil {
		t.Fatalf("MergeDiscoveredProviderWithCostProtection error: %v", err)
	}

	protected := cat.Providers["openai"].Models["gpt-5.4-mini"].Cost
	if protected.Input == nil || *protected.Input != 99 {
		t.Fatalf("protected input = %v, want 99", protected.Input)
	}
	if protected.Output == nil || *protected.Output != 2 {
		t.Fatalf("unprotected output = %v, want 2", protected.Output)
	}
	if protected.CacheRead == nil || *protected.CacheRead != 4 {
		t.Fatalf("unprotected cache_read = %v, want 4", protected.CacheRead)
	}
	if protected.CacheWrite == nil || *protected.CacheWrite != 5 {
		t.Fatalf("unprotected cache_write = %v, want 5", protected.CacheWrite)
	}

	omitted := cat.Providers["openai"].Models["omitted-costs"].Cost
	if omitted.Input == nil || *omitted.Input != 15 {
		t.Fatalf("omitted input = %v, want updated 15", omitted.Input)
	}
	if omitted.Output == nil || *omitted.Output != 11 {
		t.Fatalf("omitted output = %v, want preserved 11", omitted.Output)
	}
	if omitted.CacheRead == nil || *omitted.CacheRead != 12 {
		t.Fatalf("omitted cache_read = %v, want preserved 12", omitted.CacheRead)
	}
	if omitted.CacheWrite == nil || *omitted.CacheWrite != 13 {
		t.Fatalf("omitted cache_write = %v, want preserved 13", omitted.CacheWrite)
	}
}

func TestMergeDiscoveredProviderAddsCostWhenPreviouslyNil(t *testing.T) {
	// Model has no cost. Discovery provides cost. Should set it.
	cat := &Catalog{Providers: map[string]*Provider{
		"local": {
			ID:            "local",
			Name:          "local",
			Transport:     Transport{BaseURL: "http://localhost:11434/v1"},
			SystemRole:    RoleSystem,
			UsageInStream: true,
			ExtraBody:     map[string]any{},
			Models: map[string]*Model{
				"qwen3": {
					ID:              "qwen3",
					Name:            "qwen3",
					InputModalities: []Modality{ModalityText},
					SystemRole:      RoleSystem,
					UsageInStream:   true,
					ExtraBody:       map[string]any{},
				},
			},
		},
	}}
	err := cat.MergeDiscoveredProvider("local", DiscoveredProvider{
		Models: map[string]DiscoveredModel{
			"qwen3": {
				Name:          "Qwen 3",
				ContextWindow: 32768,
				Cost:          &Cost{Input: ptrFloat64(5), Output: ptrFloat64(10)},
			},
		},
	})
	if err != nil {
		t.Fatalf("MergeDiscoveredProvider error: %v", err)
	}
	model := cat.Providers["local"].Models["qwen3"]
	if model.Cost == nil {
		t.Fatalf("model.Cost = nil, want cost from discovery")
	}
	if model.Cost.Input == nil || *model.Cost.Input != 5 {
		t.Fatalf("model.Cost.Input = %v, want 5", model.Cost.Input)
	}
	if model.Cost.Output == nil || *model.Cost.Output != 10 {
		t.Fatalf("model.Cost.Output = %v, want 10", model.Cost.Output)
	}
}

func TestMergeDiscoveredProviderLeavesMaxOutputUnsetWhenDiscoveryOmits(t *testing.T) {
	cat := &Catalog{Providers: map[string]*Provider{
		"openrouter": {
			ID:            "openrouter",
			Name:          "openrouter",
			Transport:     Transport{BaseURL: "https://openrouter.ai/api/v1"},
			SystemRole:    RoleSystem,
			UsageInStream: true,
			ExtraBody:     map[string]any{},
			Builtin:       true,
			Models:        map[string]*Model{},
		},
	}}
	err := cat.MergeDiscoveredProvider("openrouter", DiscoveredProvider{
		Models: map[string]DiscoveredModel{
			"free-model": {
				Name:          "Free Model",
				ContextWindow: 200000,
				// MaxOutputTokens=0 — discovery didn't provide it
			},
		},
	})
	if err != nil {
		t.Fatalf("MergeDiscoveredProvider error: %v", err)
	}
	m := cat.Providers["openrouter"].Models["free-model"]
	if m == nil {
		t.Fatal("model not found in catalog")
	}
	if m.ContextWindow != 200000 {
		t.Fatalf("ContextWindow = %d, want 200000", m.ContextWindow)
	}
	if m.MaxOutputTokens != 0 {
		t.Fatalf("MaxOutputTokens = %d, want 0 (no fallback to context_window)", m.MaxOutputTokens)
	}
}

func TestMergeDiscoveredProviderFiltersRejectedDiscoveredOnlyModels(t *testing.T) {
	cat := &Catalog{Providers: map[string]*Provider{
		"openrouter": {
			ID:            "openrouter",
			Name:          "openrouter",
			Transport:     Transport{BaseURL: "https://openrouter.ai/api/v1"},
			SystemRole:    RoleSystem,
			UsageInStream: true,
			ExtraBody:     map[string]any{},
			Builtin:       true,
			Models:        map[string]*Model{},
		},
	}}
	err := cat.MergeDiscoveredProvider("openrouter", DiscoveredProvider{
		Models: map[string]DiscoveredModel{
			"image-only": {
				Name:          "Image Only",
				ContextWindow: 200000,
				metadata:      &discoveryModelMetadata{ArchitectureOutputModalities: []string{"image"}},
			},
			"unknown": {
				Name:          "Unknown",
				ContextWindow: 200000,
			},
		},
	})
	if err != nil {
		t.Fatalf("MergeDiscoveredProvider error: %v", err)
	}
	if _, ok := cat.Providers["openrouter"].Models["image-only"]; ok {
		t.Fatalf("rejected discovered model was added: %#v", cat.Providers["openrouter"].Models["image-only"])
	}
	if _, ok := cat.Providers["openrouter"].Models["unknown"]; !ok {
		t.Fatalf("unknown discovered model without metadata was not added")
	}
}

func TestMergeDiscoveredProviderConfigOnlyDoesNotAddModels(t *testing.T) {
	cat := &Catalog{Providers: map[string]*Provider{
		"local": {
			ID:            "local",
			Name:          "local",
			Transport:     Transport{BaseURL: "http://localhost:11434/v1"},
			SystemRole:    RoleSystem,
			UsageInStream: true,
			ExtraBody:     map[string]any{},
			Models:        map[string]*Model{},
		},
	}}
	err := cat.MergeDiscoveredProvider("local", DiscoveredProvider{
		Models: map[string]DiscoveredModel{
			"qwen3": {Name: "Qwen 3", ContextWindow: 32768, MaxOutputTokens: 8192, Cost: &Cost{Input: ptrFloat64(1)}},
		},
	})
	if err != nil {
		t.Fatalf("MergeDiscoveredProvider error: %v", err)
	}
	if _, ok := cat.Providers["local"].Models["qwen3"]; ok {
		t.Fatalf("config-only provider gained discovered model: %#v", cat.Providers["local"].Models["qwen3"])
	}
}

func TestCatalogVisibleModelsFiltersHiddenAndSorts(t *testing.T) {
	cat := &Catalog{Providers: map[string]*Provider{
		"openai": {
			ID:     "openai",
			Models: map[string]*Model{"z": {ID: "z"}, "a": {ID: "a", Hidden: true}},
		},
		"hidden": {
			ID:     "hidden",
			Hidden: true,
			Models: map[string]*Model{"m": {ID: "m"}},
		},
		"local": {
			ID:     "local",
			Models: map[string]*Model{"qwen": {ID: "qwen"}},
		},
	}}

	visible := cat.VisibleModels()
	want := []string{"local/qwen", "openai/z"}
	if len(visible) != len(want) {
		t.Fatalf("VisibleModels = %#v, want %v", visible, want)
	}
	for i := range want {
		if visible[i].String() != want[i] {
			t.Fatalf("VisibleModels = %#v, want %v", visible, want)
		}
	}

	all := cat.AllModels()
	if len(all) != 4 {
		t.Fatalf("AllModels length = %d, want 4", len(all))
	}
}
