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

// mergeCostModel is the nominal typed merge model stem with the row's
// existing cost; nil is the absent-cost state.
func mergeCostModel(cost *Cost) *Model {
	return &Model{
		ID:              "gpt-5.4-mini",
		Name:            "GPT-5.4 mini",
		ContextWindow:   400000,
		MaxOutputTokens: 128000,
		InputModalities: []Modality{ModalityText},
		SystemRole:      RoleSystem,
		UsageInStream:   true,
		ExtraBody:       map[string]any{},
		Cost:            cost,
	}
}

// mergeCostProvider wraps one model in the nominal typed merge provider.
func mergeCostProvider(model *Model) *Catalog {
	return &Catalog{Providers: map[string]*Provider{
		"openai": {
			ID:            "openai",
			Name:          "OpenAI",
			Transport:     Transport{BaseURL: "https://api.openai.com/v1"},
			SystemRole:    RoleSystem,
			UsageInStream: true,
			ExtraBody:     map[string]any{},
			Models:        map[string]*Model{model.ID: model},
		},
	}}
}

func TestMergeDiscoveredProviderCostFields(t *testing.T) {
	rows := []struct {
		name                          string
		existing                      *Cost
		discovered                    *Cost
		wantInput, wantOutput         *float64
		wantCacheRead, wantCacheWrite *float64
	}{
		{
			name:          "updates existing cost",
			existing:      &Cost{Input: ptrFloat64(10), Output: ptrFloat64(20)},
			discovered:    &Cost{Input: ptrFloat64(15), Output: ptrFloat64(30), CacheRead: ptrFloat64(5)},
			wantInput:     ptrFloat64(15),
			wantOutput:    ptrFloat64(30),
			wantCacheRead: ptrFloat64(5),
		},
		{
			name:           "preserves fields discovery omits",
			existing:       &Cost{Input: ptrFloat64(10), Output: ptrFloat64(20), CacheRead: ptrFloat64(3), CacheWrite: ptrFloat64(7)},
			discovered:     &Cost{Input: ptrFloat64(12), Output: ptrFloat64(25)},
			wantInput:      ptrFloat64(12),
			wantOutput:     ptrFloat64(25),
			wantCacheRead:  ptrFloat64(3),
			wantCacheWrite: ptrFloat64(7),
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cat := mergeCostProvider(mergeCostModel(row.existing))
			err := cat.MergeDiscoveredProvider("openai", DiscoveredProvider{
				Models: map[string]DiscoveredModel{
					"gpt-5.4-mini": {
						Name:          "GPT-5.4 mini",
						ContextWindow: 400000,
						Cost:          row.discovered,
					},
				},
			})
			testassert.NoError(t, err)
			model := cat.Providers["openai"].Models["gpt-5.4-mini"]
			assertCostFields(t, model.Cost, row.wantInput, row.wantOutput, row.wantCacheRead, row.wantCacheWrite)
		})
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
				"zero-cost": {
					ID:              "zero-cost",
					Name:            "Zero Cost",
					InputModalities: []Modality{ModalityText},
					SystemRole:      RoleSystem,
					UsageInStream:   true,
					ExtraBody:       map[string]any{},
					Cost:            &Cost{Input: ptrFloat64(0), Output: ptrFloat64(20)},
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
			"zero-cost": {
				Cost: &Cost{Input: ptrFloat64(15), Output: ptrFloat64(30)},
			},
		},
	}, map[string]map[string]bool{
		"gpt-5.4-mini": {"input": true},
		"zero-cost":    {"input": true},
	})
	testassert.NoError(t, err)

	protected := cat.Providers["openai"].Models["gpt-5.4-mini"].Cost
	assertCostFields(t, protected, ptrFloat64(99), ptrFloat64(2), ptrFloat64(4), ptrFloat64(5))

	omitted := cat.Providers["openai"].Models["omitted-costs"].Cost
	assertCostFields(t, omitted, ptrFloat64(15), ptrFloat64(11), ptrFloat64(12), ptrFloat64(13))

	zero := cat.Providers["openai"].Models["zero-cost"].Cost
	assertCostFields(t, zero, ptrFloat64(0), ptrFloat64(30), nil, nil)
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
	assertCostFields(t, model.Cost, ptrFloat64(5), ptrFloat64(10), nil, nil)
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
