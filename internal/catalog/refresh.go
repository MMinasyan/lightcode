package catalog

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

const discoveryFetchTimeout = 10 * time.Second

// RefreshProviderDiscovery fetches one provider's live discovery data, merges it into
// the in-memory catalog, and writes the discovery cache. Discovery failures are
// returned as warnings so callers can continue startup or model switching.
func RefreshProviderDiscovery(ctx context.Context, home string, cat *Catalog, providerID string) []Warning {
	provider := catalogProvider(cat, providerID)
	if provider == nil {
		return []Warning{{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("unknown provider %q", providerID)}}
	}
	if !provider.Discovery {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryFetchTimeout)
	defer cancel()
	discovered, err := FetchDiscovery(ctx, http.DefaultClient, provider)
	if err != nil {
		return []Warning{{Kind: "discovery_failure", Provider: providerID, Message: err.Error()}}
	}
	if err := WriteDiscoveryCache(home, providerID, discovered, time.Now().UTC()); err != nil {
		return []Warning{{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("write discovery cache: %v", err)}}
	}
	if err := cat.MergeDiscoveredProvider(providerID, discovered); err != nil {
		return []Warning{{Kind: "discovery_failure", Provider: providerID, Message: err.Error()}}
	}
	return nil
}

// DiscoveryRefreshCandidates returns enabled discovery providers that need live
// discovery because the model list is empty or at least one model is incomplete.
func DiscoveryRefreshCandidates(cat *Catalog) []string {
	if cat == nil {
		return nil
	}
	ids := make([]string, 0, len(cat.Providers))
	for providerID, provider := range cat.Providers {
		if provider == nil || !provider.Discovery {
			continue
		}
		if len(provider.Models) == 0 || providerHasIncompleteModel(provider) {
			ids = append(ids, providerID)
		}
	}
	sort.Strings(ids)
	return ids
}

// MergeDiscoveredProvider gap-fills provider models from discovery data.
func (c *Catalog) MergeDiscoveredProvider(providerID string, discovered DiscoveredProvider) error {
	provider := catalogProvider(c, providerID)
	if provider == nil {
		return fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
	}
	if provider.Models == nil {
		provider.Models = map[string]*Model{}
	}
	for modelID, discoveredModel := range discovered.Models {
		model := provider.Models[modelID]
		if model == nil {
			name := discoveredModel.Name
			if name == "" {
				name = modelID
			}
			model = &Model{
				ID:              modelID,
				Name:            name,
				InputModalities: []Modality{ModalityText},
				SystemRole:      provider.SystemRole,
				UsageInStream:   provider.UsageInStream,
				ExtraBody:       map[string]any{},
			}
			provider.Models[modelID] = model
		}
		if model.ID == "" {
			model.ID = modelID
		}
		if model.Name == "" && discoveredModel.Name != "" {
			model.Name = discoveredModel.Name
		}
		if model.Name == "" {
			model.Name = modelID
		}
		if model.ContextWindow == 0 && discoveredModel.ContextWindow > 0 {
			model.ContextWindow = discoveredModel.ContextWindow
		}
		if model.MaxOutputTokens == 0 && discoveredModel.MaxOutputTokens > 0 {
			model.MaxOutputTokens = discoveredModel.MaxOutputTokens
		}
		if model.MaxOutputTokens == 0 && model.ContextWindow > 0 {
			model.MaxOutputTokens = model.ContextWindow
		}
		if discoveredModel.Cost != nil {
			if model.Cost == nil {
				model.Cost = &Cost{}
			}
			mergeCostPointers(model.Cost, discoveredModel.Cost)
		}
		if len(model.InputModalities) == 0 {
			model.InputModalities = []Modality{ModalityText}
		}
		if model.SystemRole == "" {
			model.SystemRole = provider.SystemRole
		}
		if model.ExtraBody == nil {
			model.ExtraBody = map[string]any{}
		}
	}
	return nil
}

func catalogProvider(cat *Catalog, providerID string) *Provider {
	if cat == nil || cat.Providers == nil {
		return nil
	}
	return cat.Providers[providerID]
}

// mergeCostPointers merges individual cost subfields from discovered into existing.
// Only fields present in discovered are written; existing fields not in discovered
// are preserved. This allows discovery to update prices without inventing missing
// cost fields.
func mergeCostPointers(existing, discovered *Cost) {
	if discovered.Input != nil {
		existing.Input = discovered.Input
	}
	if discovered.Output != nil {
		existing.Output = discovered.Output
	}
	if discovered.CacheRead != nil {
		existing.CacheRead = discovered.CacheRead
	}
	if discovered.CacheWrite != nil {
		existing.CacheWrite = discovered.CacheWrite
	}
}

func providerHasIncompleteModel(provider *Provider) bool {
	for _, model := range provider.Models {
		if model == nil {
			return true
		}
		if _, incomplete := model.Incomplete(); incomplete {
			return true
		}
	}
	return false
}
