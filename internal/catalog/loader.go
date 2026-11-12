package catalog

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

// bundledFS contains Melta's hand-curated built-in provider catalog files.
//
//go:embed builtin
var bundledFS embed.FS

// Loader reads catalog inputs from disk and delegates assembly to Build.
type Loader struct {
	cacheDir     string
	bundled      fs.FS
	AllowRefresh func(providerID string, provider *Provider) bool
}

// NewLoader constructs a catalog loader over the explicit discovery cache
// directory. An empty directory performs no cache read, write, lock, or TTL
// record, so no automatic refresh runs. A nil bundled FS selects the embedded
// built-in catalog.
func NewLoader(cacheDir string, bundled fs.FS) *Loader {
	if bundled == nil {
		bundled = bundledFS
	}
	return &Loader{cacheDir: cacheDir, bundled: bundled}
}

// CacheDir returns the loader's explicit discovery cache directory.
func (l *Loader) CacheDir() string {
	return l.cacheDir
}

// AssembleCaptured is the captured-input assembly step: the user provider
// layer arrives as the caller's already-read map and the main configuration
// is never read. It reads the selected discovery cache, builds the effective
// catalog, and reports the cache and build warnings. It performs no network
// fetch and no cache write; RefreshCaptured consumes the caller's credential
// capture and publishes due refreshes.
func (l *Loader) AssembleCaptured(ctx context.Context, userRaw map[string]any) (BuildResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BuildResult{}, err
	}
	bundled, err := readBundledProviders(l.bundled)
	if err != nil {
		return BuildResult{}, fmt.Errorf("read bundled catalog: %w", err)
	}
	records, warnings := ReadDiscoveryCache(l.cacheDir)
	result := Build(BuildInputs{Bundled: bundled, UserRaw: userRaw, Records: records})
	warnings = append(warnings, result.Warnings...)
	return BuildResult{Catalog: result.Catalog, Warnings: warnings}, nil
}

// RefreshCaptured is the captured-input refresh step: it routes discovery
// readiness, fetches and cache publications from the caller's one captured
// credential map, never the process environment. Every publication uses the
// one-attempt Try writers, and cancellation observed after a fetch and before
// a cache writer skips that write and returns the context error. An empty
// cache directory skips automatic refresh entirely.
func (l *Loader) RefreshCaptured(ctx context.Context, userRaw map[string]any, built BuildResult, credentials map[string]string) (BuildResult, error) {
	if l.cacheDir == "" || built.Catalog == nil {
		return built, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	records, _ := ReadDiscoveryCache(l.cacheDir)
	candidates := DiscoveryRefreshCandidates(built.Catalog, records, time.Now().UTC())
	candidates = l.filterRefreshCandidates(candidates, built.Catalog)
	discoveryWarnings, discoveryChanged, err := refreshDiscoveryCandidates(ctx, l.cacheDir, candidates, built.Catalog, userRaw, credentials)
	built.Warnings = append(built.Warnings, discoveryWarnings...)
	if err != nil {
		return BuildResult{}, err
	}
	if discoveryChanged {
		records, cacheWarnings := ReadDiscoveryCache(l.cacheDir)
		built.Warnings = append(built.Warnings, cacheWarnings...)
		bundled, err := readBundledProviders(l.bundled)
		if err != nil {
			return BuildResult{}, fmt.Errorf("read bundled catalog: %w", err)
		}
		result := Build(BuildInputs{Bundled: bundled, UserRaw: userRaw, Records: records})
		built = BuildResult{Catalog: result.Catalog, Warnings: append(built.Warnings, result.Warnings...)}
	}
	return built, nil
}

// LoadCapturedConnection is the connection-candidate entry: over the same
// captured bundled FS, discovery-cache read, and Build producer as
// LoadCaptured, it performs no network fetch, no cache write, and no
// refresh — a connection candidate is fully built and validated before its
// caller's first persistence. A nonnil discovered overlays ONLY that
// provider's in-memory discovery record: the fingerprint is computed here
// over the caller's original configured transport, and the fetched models are
// carried verbatim, their unexported metadata included — never round-tripped
// through the cache. A nil discovered leaves the ordinary cached inputs.
func (l *Loader) LoadCapturedConnection(ctx context.Context, userRaw map[string]any, providerID string, transport Transport, discovered *DiscoveredProvider) (BuildResult, error) {
	catalog, warnings, err := l.loadCapturedConnection(userRaw, providerID, transport, discovered)
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{Catalog: catalog, Warnings: warnings}, nil
}

func (l *Loader) loadCapturedConnection(userRaw map[string]any, providerID string, transport Transport, discovered *DiscoveredProvider) (*Catalog, []Warning, error) {
	bundled, err := readBundledProviders(l.bundled)
	if err != nil {
		return nil, nil, fmt.Errorf("read bundled catalog: %w", err)
	}
	records, warnings := ReadDiscoveryCache(l.cacheDir)
	if discovered != nil {
		records[providerID] = DiscoveryRecord{
			TransportFingerprint: transportFingerprint(transport),
			FetchedAt:            time.Now().UTC(),
			AttemptedAt:          time.Now().UTC(),
			Models:               discovered.Models,
		}
	}
	result := Build(BuildInputs{Bundled: bundled, UserRaw: userRaw, Records: records})
	warnings = append(warnings, result.Warnings...)
	return result.Catalog, warnings, nil
}

func (l *Loader) filterRefreshCandidates(candidateIDs []string, cat *Catalog) []string {
	if l == nil || l.AllowRefresh == nil || cat == nil || len(candidateIDs) == 0 {
		return candidateIDs
	}
	filtered := candidateIDs[:0]
	for _, providerID := range candidateIDs {
		if l.AllowRefresh(providerID, cat.Providers[providerID]) {
			filtered = append(filtered, providerID)
		}
	}
	return filtered
}

// refreshDiscoveryCandidates is the loader's sorted refresh loop over the
// filtered candidates: per provider it performs the fetch over the caller's
// captured credentials, checks ctx cancellation once the network attempt
// began and before any cache writer starts, and then publishes through the
// shared per-provider code with the already-read userRaw as the
// cost-protection source. A cancellation observed between the fetch and the
// publication skips that write and returns the context error with the
// warnings collected so far; it is not a discovery warning.
func refreshDiscoveryCandidates(ctx context.Context, cacheDir string, candidateIDs []string, cat *Catalog, userRaw map[string]any, credentials map[string]string) ([]Warning, bool, error) {
	var warnings []Warning
	changed := false
	if cat == nil {
		return warnings, changed, nil
	}
	for _, providerID := range candidateIDs {
		provider := catalogProvider(cat, providerID)
		if provider == nil {
			warnings = append(warnings, Warning{Kind: "discovery_failure", Provider: providerID, Message: fmt.Sprintf("unknown provider %q", providerID)})
			continue
		}
		attempted, discovered, providerWarnings := FetchDiscoveryIfDue(ctx, cacheDir, provider, credentials, time.Now().UTC())
		if !attempted {
			warnings = append(warnings, providerWarnings...)
			continue
		}
		if err := ctx.Err(); err != nil {
			return warnings, changed, err
		}
		refreshed, providerWarnings := publishDiscoveryResult(cacheDir, provider, providerID, discovered, providerWarnings, cat, func() map[string]map[string]bool {
			return userCostProtection(userRaw, providerID)
		})
		if len(providerWarnings) != 0 {
			warnings = append(warnings, providerWarnings...)
			continue
		}
		if refreshed {
			changed = true
		}
	}
	return warnings, changed, nil
}

func readBundledProviders(fsys fs.FS) (map[string]json.RawMessage, error) {
	providers := map[string]json.RawMessage{}
	err := fs.WalkDir(fsys, "builtin", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || path.Ext(file) != ".json" {
			return nil
		}
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return err
		}
		providerID := strings.TrimSuffix(path.Base(file), ".json")
		providers[providerID] = append(json.RawMessage(nil), data...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return providers, nil
}
