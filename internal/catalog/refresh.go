package catalog

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"
)

const discoveryFetchTimeout = 10 * time.Second

// RefreshProviderDiscovery fetches one provider's live discovery data, merges it into
// the in-memory catalog, and writes the discovery cache. Discovery failures are
// returned as warnings so callers can continue startup.
func RefreshProviderDiscovery(ctx context.Context, home string, cat *Catalog, providerID string) (bool, []Warning) {
	provider := catalogProvider(cat, providerID)
	if provider == nil {
		return false, []Warning{{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("unknown provider %q", providerID)}}
	}
	if !provider.Discovery {
		return false, nil
	}
	if provider.Transport.APIKeyEnv != "" && os.Getenv(provider.Transport.APIKeyEnv) == "" {
		return false, nil
	}
	now := time.Now().UTC()
	recent, err := DiscoveryAttemptRecent(home, providerID, now)
	if err != nil {
		return false, []Warning{{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("read discovery attempt: %v", err)}}
	}
	if recent {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryFetchTimeout)
	defer cancel()
	if err := WriteDiscoveryAttempt(home, providerID, now); err != nil {
		return false, []Warning{{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("write discovery attempt: %v", err)}}
	}
	discovered, err := FetchDiscovery(ctx, http.DefaultClient, provider)
	if err != nil {
		return false, []Warning{{Kind: "discovery_failure", Provider: providerID, Message: err.Error()}}
	}
	if err := WriteDiscoveryCache(home, providerID, discovered, now); err != nil {
		return false, []Warning{{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("write discovery cache: %v", err)}}
	}
	protected := userCostProtectionForProvider(home, providerID)
	if err := cat.MergeDiscoveredProviderWithCostProtection(providerID, discovered, protected); err != nil {
		return false, []Warning{{Kind: "discovery_failure", Provider: providerID, Message: err.Error()}}
	}
	return true, nil
}

func userCostProtectionForProvider(home, providerID string) map[string]map[string]bool {
	userRaw, _ := readUserConfigProviders(home)
	providerRaw, _ := userRaw[providerID].(map[string]any)
	if providerRaw == nil {
		return nil
	}
	modelsRaw, _ := providerRaw["models"].(map[string]any)
	if len(modelsRaw) == 0 {
		return nil
	}
	protected := map[string]map[string]bool{}
	for modelID := range modelsRaw {
		fields := userCostFields(providerRaw, modelID)
		if len(fields) != 0 {
			protected[modelID] = fields
		}
	}
	return protected
}

// DiscoveryRefreshCandidates returns enabled discovery providers that have not
// had a real discovery attempt in the last 24 hours.
func DiscoveryRefreshCandidates(cat *Catalog, attempts map[string]time.Time, now time.Time) []string {
	if cat == nil {
		return nil
	}
	ids := make([]string, 0, len(cat.Providers))
	for providerID, provider := range cat.Providers {
		if provider == nil || !provider.Discovery {
			continue
		}
		if discoveryAttemptDue(attempts[providerID], now) {
			ids = append(ids, providerID)
		}
	}
	sort.Strings(ids)
	return ids
}

// MergeDiscoveredProvider gap-fills provider models from discovery data.
func (c *Catalog) MergeDiscoveredProvider(providerID string, discovered DiscoveredProvider) error {
	return c.MergeDiscoveredProviderWithCostProtection(providerID, discovered, nil)
}

// MergeDiscoveredProviderWithCostProtection gap-fills provider models from
// discovery data while preserving user-declared cost subfields.
func (c *Catalog) MergeDiscoveredProviderWithCostProtection(providerID string, discovered DiscoveredProvider, protected map[string]map[string]bool) error {
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
			if !provider.Builtin {
				continue
			}
			name := discoveredModel.Name
			if name == "" {
				name = modelID
			}
			model = &Model{
				ID:               modelID,
				Name:             name,
				InputModalities:  []Modality{ModalityText},
				SystemRole:       provider.SystemRole,
				UsageInStream:    provider.UsageInStream,
				ExtraBody:        map[string]any{},
				ProtocolMetadata: cloneProtocolMetadata(provider.ProtocolMetadata),
			}
			provider.Models[modelID] = model
		}
		if !provider.Builtin {
			if discoveredModel.Cost != nil {
				if model.Cost == nil {
					model.Cost = &Cost{}
				}
				mergeCostPointersProtected(model.Cost, discoveredModel.Cost, protected[modelID])
			}
			continue
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
		if discoveredModel.Cost != nil {
			if model.Cost == nil {
				model.Cost = &Cost{}
			}
			mergeCostPointersProtected(model.Cost, discoveredModel.Cost, protected[modelID])
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
		if model.ProtocolMetadata == nil {
			model.ProtocolMetadata = cloneProtocolMetadata(provider.ProtocolMetadata)
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
	mergeCostPointersProtected(existing, discovered, nil)
}

func mergeCostPointersProtected(existing, discovered *Cost, protected map[string]bool) {
	if discovered.Input != nil && !protected["input"] {
		existing.Input = discovered.Input
	}
	if discovered.Output != nil && !protected["output"] {
		existing.Output = discovered.Output
	}
	if discovered.CacheRead != nil && !protected["cache_read"] {
		existing.CacheRead = discovered.CacheRead
	}
	if discovered.CacheWrite != nil && !protected["cache_write"] {
		existing.CacheWrite = discovered.CacheWrite
	}
}
