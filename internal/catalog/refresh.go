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
	return RefreshProviderDiscoveryWithConfigPath(ctx, home, "", cat, providerID)
}

// RefreshProviderDiscoveryWithConfigPath is RefreshProviderDiscovery with an
// explicit config path for preserving user-owned cost fields during merges.
// It is the blocking wrapper for startup callers: the attempt and cache
// publications block on the per-provider discovery lock until the holder
// releases.
func RefreshProviderDiscoveryWithConfigPath(ctx context.Context, home, configPath string, cat *Catalog, providerID string) (bool, []Warning) {
	return refreshProviderDiscovery(ctx, home, configPath, cat, providerID, false)
}

// RefreshProviderDiscoveryTryWithConfigPath is the one-attempt counterpart of
// RefreshProviderDiscoveryWithConfigPath for owner paths that must not block a
// shutting-down owner on a foreign discovery-lock holder. Every publication
// runs through the one-attempt Try writers; contention yields the existing
// discovery_failure warning and publishes nothing.
func RefreshProviderDiscoveryTryWithConfigPath(ctx context.Context, home, configPath string, cat *Catalog, providerID string) (bool, []Warning) {
	return refreshProviderDiscovery(ctx, home, configPath, cat, providerID, true)
}

// FetchDiscoveryIfDue performs the fetch-only core of a discovery refresh:
// readiness — including validation of the discovery models URL before the
// network-attempt boundary — the current recent-attempt check, the bounded
// timeout, and the network fetch. It performs no disk write, catalog merge,
// owner mutation, or warning publication. attempted is true exactly once the
// network begins; a pre-network URL validation error leaves it false, and a
// valid-request failure after the boundary stays attempted. discovered carries
// the fetched models only on success; warnings carry the existing
// discovery_failure warnings. It is the shared core of the blocking and Try
// refresh wrappers.
func FetchDiscoveryIfDue(ctx context.Context, home string, provider *Provider, now time.Time) (attempted bool, discovered *DiscoveredProvider, warnings []Warning) {
	if provider == nil {
		return false, nil, []Warning{{Kind: "discovery_failure", Message: "discovery provider is nil"}}
	}
	if !provider.Discovery {
		return false, nil, nil
	}
	if provider.Transport.APIKeyEnv != "" && os.Getenv(provider.Transport.APIKeyEnv) == "" {
		return false, nil, nil
	}
	// The discovery models URL is validated before the network-attempt
	// boundary, using the same package logic FetchDiscovery applies. A
	// pre-network validation error is a readiness failure: attempted stays
	// false, so neither refresh wrapper creates an attempt marker for a
	// request that never reached the network, and the existing
	// discovery_failure warning is returned.
	if _, err := discoveryModelsURL(provider.Transport.BaseURL); err != nil {
		return false, nil, []Warning{{Kind: "discovery_failure", Provider: provider.ID, Message: err.Error()}}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	recent, err := DiscoveryAttemptRecent(home, provider.ID, now)
	if err != nil {
		return false, nil, []Warning{{Kind: "discovery_failure", Provider: provider.ID, Message: fmt.Sprintf("read discovery attempt: %v", err)}}
	}
	if recent {
		return false, nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryFetchTimeout)
	defer cancel()
	// The network-attempt boundary: attempted is set immediately before the
	// existing FetchDiscovery call, so a valid-request failure after this
	// point stays attempted and preserves TTL suppression.
	attempted = true
	got, err := FetchDiscovery(ctx, http.DefaultClient, provider)
	if err != nil {
		return attempted, nil, []Warning{{Kind: "discovery_failure", Provider: provider.ID, Message: err.Error()}}
	}
	return attempted, &got, nil
}

// refreshProviderDiscovery is the shared refresh body behind the blocking and
// Try wrappers. Both preserve read-cost protection before publication/merge.
func refreshProviderDiscovery(ctx context.Context, home, configPath string, cat *Catalog, providerID string, try bool) (bool, []Warning) {
	provider := catalogProvider(cat, providerID)
	if provider == nil {
		return false, []Warning{{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("unknown provider %q", providerID)}}
	}
	attempted, discovered, warnings := FetchDiscoveryIfDue(ctx, home, provider, time.Now().UTC())
	if !attempted {
		return false, warnings
	}
	if discovered == nil {
		// The network failed: record the attempt so a duplicate fetch is
		// suppressed inside the TTL, then surface the fetch failure.
		if try {
			ok, err := TryWriteDiscoveryAttempt(home, providerID, time.Now().UTC())
			if err != nil {
				return false, append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("write discovery attempt: %v", err)})
			}
			if !ok {
				return false, append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("discovery lock is held for %q; attempt write skipped", providerID)})
			}
		} else {
			if err := WriteDiscoveryAttempt(home, providerID, time.Now().UTC()); err != nil {
				return false, append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("write discovery attempt: %v", err)})
			}
		}
		return false, warnings
	}
	if try {
		ok, err := TryWriteDiscoveryCache(home, providerID, *discovered, time.Now().UTC())
		if err != nil {
			return false, append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("write discovery cache: %v", err)})
		}
		if !ok {
			return false, append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("discovery lock is held for %q; cache write skipped", providerID)})
		}
	} else {
		if err := WriteDiscoveryCache(home, providerID, *discovered, time.Now().UTC()); err != nil {
			return false, append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("write discovery cache: %v", err)})
		}
	}
	protected := userCostProtectionForProviderAt(home, configPath, providerID)
	if err := cat.MergeDiscoveredProviderWithCostProtection(providerID, *discovered, protected); err != nil {
		return false, append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: err.Error()})
	}
	return true, nil
}

func userCostProtectionForProvider(home, providerID string) map[string]map[string]bool {
	return userCostProtectionForProviderAt(home, "", providerID)
}

func userCostProtectionForProviderAt(home, configPath, providerID string) map[string]map[string]bool {
	userRaw, _ := readUserConfigProvidersAt(home, configPath)
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
			if !discoveredModelAllowed(discoveredModel) {
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
