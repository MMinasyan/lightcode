package catalog

import (
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
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

	{
		_, _, err := cat.Lookup(ModelRef{Provider: "missing", Model: "gpt"})
		testassert.Require(t, errors.Is(err, ErrUnknownProvider), "missing provider err = %v, want ErrUnknownProvider", err)
	}
	{
		_, _, err := cat.Lookup(ModelRef{Provider: "openai", Model: "missing"})
		testassert.Require(t, errors.Is(err, ErrUnknownModel), "missing model err = %v, want ErrUnknownModel", err)
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
	testassert.NoError(t, err)
	model := cat.Providers["openai"].Models["gpt-5.4-mini"]
	testassert.Require(t, model.Cost != nil, "model.Cost = nil")
	testassert.Require(t, model.Cost.Input != nil && *model.Cost.Input == 15 && model.Cost.Output != nil && *model.Cost.Output == 30 && model.Cost.CacheRead != nil && *model.Cost.CacheRead == 5, "model.Cost.Input = %v, want 15; model.Cost.Output = %v, want 30; model.Cost.CacheRead = %v, want 5", model.Cost.Input, model.Cost.Output, model.Cost.CacheRead)
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
	testassert.NoError(t, err)
	model := cat.Providers["openai"].Models["gpt-5.4-mini"]
	testassert.Require(t, model.Cost != nil, "model.Cost = nil")
	testassert.Require(t, model.Cost.Input != nil && *model.Cost.Input == 12 && model.Cost.Output != nil && *model.Cost.Output == 25 && model.Cost.CacheRead != nil && *model.Cost.CacheRead == 3 && model.Cost.CacheWrite != nil && *model.Cost.CacheWrite == 7, "model.Cost.Input = %v, want 12 (updated by discovery); model.Cost.Output = %v, want 25 (updated by discovery); model.Cost.CacheRead = %v, want 3 (preserved from existing); model.Cost.CacheWrite = %v, want 7 (preserved from existing)", model.Cost.Input, model.Cost.Output, model.Cost.CacheRead, model.Cost.CacheWrite)
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
	testassert.NoError(t, err)

	protected := cat.Providers["openai"].Models["gpt-5.4-mini"].Cost
	testassert.Require(t, protected.Input != nil && *protected.Input == 99 && protected.Output != nil && *protected.Output == 2 && protected.CacheRead != nil && *protected.CacheRead == 4 && protected.CacheWrite != nil && *protected.CacheWrite == 5, "protected input = %v, want 99; unprotected output = %v, want 2; unprotected cache_read = %v, want 4; unprotected cache_write = %v, want 5", protected.Input, protected.Output, protected.CacheRead, protected.CacheWrite)

	omitted := cat.Providers["openai"].Models["omitted-costs"].Cost
	testassert.Require(t, omitted.Input != nil && *omitted.Input == 15 && omitted.Output != nil && *omitted.Output == 11 && omitted.CacheRead != nil && *omitted.CacheRead == 12 && omitted.CacheWrite != nil && *omitted.CacheWrite == 13, "omitted input = %v, want updated 15; omitted output = %v, want preserved 11; omitted cache_read = %v, want preserved 12; omitted cache_write = %v, want preserved 13", omitted.Input, omitted.Output, omitted.CacheRead, omitted.CacheWrite)
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
	testassert.NoError(t, err)
	model := cat.Providers["local"].Models["qwen3"]
	testassert.Require(t, model.Cost != nil, "model.Cost = nil, want cost from discovery")
	testassert.Require(t, model.Cost.Input != nil && *model.Cost.Input == 5 && model.Cost.Output != nil && *model.Cost.Output == 10, "model.Cost.Input = %v, want 5; model.Cost.Output = %v, want 10", model.Cost.Input, model.Cost.Output)
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
	testassert.NoError(t, err)
	m := cat.Providers["openrouter"].Models["free-model"]
	testassert.Require(t, m != nil, "model not found in catalog")
	testassert.Require(t, m.ContextWindow == 200000 && m.MaxOutputTokens == 0, "got %v, want %v; got %v, want %v", m.ContextWindow, 200000, m.MaxOutputTokens, 0)
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
	testassert.NoError(t, err)
	{
		_, ok := cat.Providers["openrouter"].Models["image-only"]
		testassert.Require(t, !ok, "rejected discovered model was added: %#v", cat.Providers["openrouter"].Models["image-only"])
	}
	{
		_, ok := cat.Providers["openrouter"].Models["unknown"]
		testassert.Require(t, ok, "unknown discovered model without metadata was not added")
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
	testassert.NoError(t, err)
	{
		_, ok := cat.Providers["local"].Models["qwen3"]
		testassert.Require(t, !ok, "config-only provider gained discovered model: %#v", cat.Providers["local"].Models["qwen3"])
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
	testassert.Require(t, len(visible) == len(want), "VisibleModels = %#v, want %v", visible, want)
	for i := range want {
		testassert.Require(t, visible[i].String() == want[i], "VisibleModels = %#v, want %v", visible, want)
	}

	all := cat.AllModels()
	testassert.Equal(t, len(all), 4)
}
