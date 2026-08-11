package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
)

// GetProviderConfig returns the merged effective config of a provider and its
// models for the config editor. Read-only.
func (a *Agent) GetProviderConfig(providerID string) (ProviderConfigView, error) {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	prov := a.catalog.Providers[providerID]
	if prov == nil {
		return ProviderConfigView{}, fmt.Errorf("provider %q not found", providerID)
	}
	view := ProviderConfigView{
		ID:               prov.ID,
		Name:             prov.Name,
		Builtin:          prov.Builtin,
		Connected:        providerConnected(prov),
		BaseURL:          prov.Transport.BaseURL,
		APIKeyEnv:        prov.Transport.APIKeyEnv,
		GeneratedKeyEnv:  a.generateAPIKeyEnvNameLocked(providerID),
		Headers:          prov.Transport.Headers,
		UserHeaders:      a.userAddedHeadersLocked(providerID, prov.Builtin),
		Options:          prov.Transport.Options,
		SystemRole:       string(prov.SystemRole),
		UsageInStream:    prov.UsageInStream,
		MaxTokensField:   prov.MaxTokensField,
		ExtraBody:        prov.ExtraBody,
		Discovery:        prov.Discovery,
		ProtocolMetadata: prov.ProtocolMetadata,
	}
	bundled := a.bundledModelIDsLocked()
	disc := a.discoveryCacheLocked()
	ids := make([]string, 0, len(prov.Models))
	for id := range prov.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := prov.Models[id]
		// Only included (usable) models appear in the config list; incomplete
		// ones are added via DiscoverableModels ("Add model").
		if m == nil || m.ContextWindow <= 0 {
			continue
		}
		view.Models = append(view.Models, ModelConfigView{
			ID:               id,
			Name:             m.Name,
			ContextWindow:    m.ContextWindow,
			MaxOutputTokens:  m.MaxOutputTokens,
			InputModalities:  m.InputModalities,
			SystemRole:       string(m.SystemRole),
			UsageInStream:    m.UsageInStream,
			ExtraBody:        m.ExtraBody,
			Cost:             m.Cost,
			ProtocolMetadata: m.ProtocolMetadata,
			Hidden:           m.Hidden,
			Source:           classifyModelSource(prov, id, bundled, disc),
		})
	}
	return view, nil
}

// userHeadersLocked returns the user-layer (config.json) transport headers for a
// provider — the additions on top of any bundled headers. Caller holds the mutex.
func (a *Agent) userHeadersLocked(providerID string) map[string]string {
	out := map[string]string{}
	if a.cfg == nil || a.cfg.Providers == nil {
		return out
	}
	prov, ok := a.cfg.Providers[providerID].(map[string]any)
	if !ok {
		return out
	}
	transport, ok := prov["transport"].(map[string]any)
	if !ok {
		return out
	}
	headers, ok := transport["headers"].(map[string]any)
	if !ok {
		return out
	}
	for k, v := range headers {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// userAddedHeadersLocked returns the user's own headers for a provider. For a
// built-in, bundled (attribution) keys are excluded so they never appear as
// editable. Caller holds the mutex.
func (a *Agent) userAddedHeadersLocked(providerID string, builtin bool) map[string]string {
	headers := a.userHeadersLocked(providerID)
	if !builtin {
		return headers
	}
	return stripBundledHeaderKeys(headers, providerID)
}

// Model provenance classes. Only user-added models may be deleted from config;
// bundled and discovered models can be hidden or have field overrides reset.
const (
	modelSourceUser       = "user"
	modelSourceBundled    = "bundled"
	modelSourceDiscovered = "discovered"
)

// bundledModelIDsLocked lazily loads and caches the bundled provider→model id
// sets used for provenance. Caller holds the runtime mutex.
func (a *Agent) bundledModelIDsLocked() map[string]map[string]struct{} {
	if a.bundledModels == nil {
		a.bundledModels = catalog.BundledModelIDs()
	}
	return a.bundledModels
}

// discoveryCacheLocked reads the discovery cache (best-effort; empty on error).
// Caller holds the runtime mutex.
func (a *Agent) discoveryCacheLocked() map[string]catalog.DiscoveredProvider {
	cache, _, _ := catalog.ReadDiscoveryCache(a.home)
	return cache
}

// classifyModelSource returns a model's provenance. Discovery never adds models
// to non-bundled providers, so every model under a custom provider is user-added.
func classifyModelSource(prov *catalog.Provider, modelID string, bundled map[string]map[string]struct{}, disc map[string]catalog.DiscoveredProvider) string {
	if prov == nil || !prov.Builtin {
		return modelSourceUser
	}
	if ids, ok := bundled[prov.ID]; ok {
		if _, isBundled := ids[modelID]; isBundled {
			return modelSourceBundled
		}
	}
	if dp, ok := disc[prov.ID]; ok {
		if _, isDiscovered := dp.Models[modelID]; isDiscovered {
			return modelSourceDiscovered
		}
	}
	return modelSourceUser
}

func (a *Agent) modelSourceLocked(providerID, modelID string) string {
	return classifyModelSource(a.catalog.Providers[providerID], modelID, a.bundledModelIDsLocked(), a.discoveryCacheLocked())
}

// applyModelFields writes the set fields of cfg onto a model config map. Only
// non-empty fields are written (whitelist is implicit: these are the only keys
// produced); absent fields are left untouched. Mirrors customModelsMap.
func applyModelFields(m map[string]any, cfg ModelConfigInput) {
	if name := strings.TrimSpace(cfg.Name); name != "" {
		m["name"] = name
	}
	if cfg.ContextWindow > 0 {
		m["context_window"] = cfg.ContextWindow
	}
	if cfg.MaxOutputTokens > 0 {
		m["max_output_tokens"] = cfg.MaxOutputTokens
	}
	if len(cfg.InputModalities) != 0 {
		m["input_modalities"] = cfg.InputModalities
	}
	if cfg.SystemRole != "" {
		m["system_role"] = cfg.SystemRole
	}
	if cfg.UsageInStream != nil {
		m["usage_in_stream"] = *cfg.UsageInStream
	}
	if len(cfg.ExtraBody) != 0 {
		m["extra_body"] = cfg.ExtraBody
	}
	if cfg.Cost != nil {
		m["cost"] = cfg.Cost
	}
	if cfg.ProtocolMetadata != nil {
		m["protocol_metadata"] = cfg.ProtocolMetadata
	}
}

// applyProviderFields writes the set fields of cfg onto a provider config map,
// nesting transport fields under "transport". Only non-empty fields are written.
func applyProviderFields(pm map[string]any, cfg ProviderConfigInput) {
	if name := strings.TrimSpace(cfg.Name); name != "" {
		pm["name"] = name
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	apiKeyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if baseURL != "" || apiKeyEnv != "" || len(cfg.Headers) != 0 || len(cfg.Options) != 0 {
		transport, _ := pm["transport"].(map[string]any)
		if transport == nil {
			transport = map[string]any{}
			pm["transport"] = transport
		}
		if baseURL != "" {
			transport["base_url"] = baseURL
		}
		if apiKeyEnv != "" {
			transport["api_key_env"] = apiKeyEnv
		}
		if len(cfg.Headers) != 0 {
			transport["headers"] = cfg.Headers
		}
		if len(cfg.Options) != 0 {
			transport["options"] = cfg.Options
		}
	}
	if cfg.SystemRole != "" {
		pm["system_role"] = cfg.SystemRole
	}
	if cfg.UsageInStream != nil {
		pm["usage_in_stream"] = *cfg.UsageInStream
	}
	if mtf := strings.TrimSpace(cfg.MaxTokensField); mtf != "" {
		pm["max_tokens_field"] = mtf
	}
	if len(cfg.ExtraBody) != 0 {
		pm["extra_body"] = cfg.ExtraBody
	}
	if cfg.Discovery != nil {
		pm["discovery"] = *cfg.Discovery
	}
	if cfg.ProtocolMetadata != nil {
		pm["protocol_metadata"] = cfg.ProtocolMetadata
	}
}

// DiscoverableModels returns the provider's models that exist at its /models
// endpoint but are not currently included (usable) in the catalog — the pool
// the user picks from in "Add model". Runs live discovery against the
// connected provider.
func (a *Agent) DiscoverableModels(providerID string) ([]DiscoveryModelCandidate, error) {
	a.ensureRuntime().mu.Lock()
	if a.ensureRuntime().sessionLocked().busy {
		a.ensureRuntime().mu.Unlock()
		return nil, fmt.Errorf("cannot discover models while a turn is running")
	}
	prov := a.catalog.Providers[providerID]
	if prov == nil {
		a.ensureRuntime().mu.Unlock()
		return nil, fmt.Errorf("provider %q not found", providerID)
	}
	provisional := &catalog.Provider{ID: providerID, Name: prov.Name, Transport: prov.Transport, Discovery: true, Models: map[string]*catalog.Model{}}
	key := ""
	if env := strings.TrimSpace(prov.Transport.APIKeyEnv); env != "" {
		key = os.Getenv(env)
	}
	included := map[string]struct{}{}
	for id, m := range prov.Models {
		if m != nil && m.ContextWindow > 0 {
			included[id] = struct{}{}
		}
	}
	a.ensureRuntime().mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()
	discovered, err := fetchConnectDiscovery(ctx, discoveryHTTPClient, provisional, key)
	if err != nil {
		return nil, err
	}
	out := make([]DiscoveryModelCandidate, 0)
	for _, c := range discoveryCandidates(discovered) {
		if _, ok := included[c.ID]; ok {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// lockedProviderFields rejects edits to a built-in provider's identity/protocol
// fields. Built-ins are defined by the bundled catalog; only headers and
// request-body additions are user-editable.
func lockedProviderFields(cfg ProviderConfigInput) error {
	switch {
	case strings.TrimSpace(cfg.Name) != "":
		return fmt.Errorf("cannot change the name of a built-in provider")
	case strings.TrimSpace(cfg.BaseURL) != "":
		return fmt.Errorf("cannot change the base URL of a built-in provider")
	case len(cfg.Options) != 0:
		return fmt.Errorf("cannot change the options of a built-in provider")
	case cfg.SystemRole != "":
		return fmt.Errorf("cannot change the system role of a built-in provider")
	case strings.TrimSpace(cfg.MaxTokensField) != "":
		return fmt.Errorf("cannot change the max-tokens field of a built-in provider")
	case cfg.UsageInStream != nil:
		return fmt.Errorf("cannot change usage-in-stream of a built-in provider")
	case cfg.ProtocolMetadata != nil:
		return fmt.Errorf("cannot change protocol metadata of a built-in provider")
	}
	return nil
}

// lockedModelFields rejects edits to a bundled/discovered model's
// identity/capability fields; only context window, max output, and cost are
// correctable.
func lockedModelFields(cfg ModelConfigInput) error {
	switch {
	case strings.TrimSpace(cfg.Name) != "":
		return fmt.Errorf("cannot rename a built-in model")
	case cfg.SystemRole != "":
		return fmt.Errorf("cannot change the system role of a built-in model")
	case cfg.UsageInStream != nil:
		return fmt.Errorf("cannot change usage-in-stream of a built-in model")
	case len(cfg.InputModalities) != 0:
		return fmt.Errorf("cannot change the input modalities of a built-in model")
	case cfg.ProtocolMetadata != nil:
		return fmt.Errorf("cannot change protocol metadata of a built-in model")
	}
	return nil
}

// SaveModel adds or edits one model's fields under a provider (user-layer
// overrides). Used for both editing and "add model". The discovery cache is
// warmed for the providers the reload would refresh (outside runtime.mu, see
// warmSettingsEditDiscovery); the config mutation and the locked reload then
// share a single lock hold.
func (a *Agent) SaveModel(providerID, modelID string, cfg ModelConfigInput) error {
	warmWarnings, err := a.warmSettingsEditDiscovery()
	if err != nil {
		return err
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if err := a.saveModelLocked(providerID, modelID, cfg); err != nil {
		return err
	}
	if err := a.reloadLocked(); err != nil {
		return err
	}
	a.surfaceCatalogWarnings(warmWarnings)
	return nil
}

// saveModelLocked validates and writes a model edit. Caller holds runtime.mu.
func (a *Agent) saveModelLocked(providerID, modelID string, cfg ModelConfigInput) error {
	if a.ensureRuntime().sessionLocked().busy {
		return fmt.Errorf("cannot edit model while a turn is running")
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if providerID == "" || modelID == "" {
		return fmt.Errorf("provider and model id are required")
	}
	if a.catalog.Providers[providerID] == nil {
		return fmt.Errorf("provider %q not found", providerID)
	}
	// Built-in and discovered models lock their identity/capability fields;
	// only correctable metadata (context window, max output, cost) is editable.
	switch a.modelSourceLocked(providerID, modelID) {
	case modelSourceBundled, modelSourceDiscovered:
		if err := lockedModelFields(cfg); err != nil {
			return err
		}
	}
	ref := coremodel.ModelRef{Provider: providerID, Model: modelID}
	return a.mutateModelConfig(ref, func(m map[string]any) error {
		applyModelFields(m, cfg)
		return nil
	})
}

// DeleteModel removes a user-added model from config. Bundled/discovered models
// cannot be deleted (the merge would re-add them); hide or reset them instead.
// The discovery cache is warmed for the providers the reload would refresh
// (outside runtime.mu, see warmSettingsEditDiscovery); the config mutation and
// the locked reload then share a single lock hold.
func (a *Agent) DeleteModel(providerID, modelID string) error {
	warmWarnings, err := a.warmSettingsEditDiscovery()
	if err != nil {
		return err
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if err := a.deleteModelLocked(providerID, modelID); err != nil {
		return err
	}
	if err := a.reloadLocked(); err != nil {
		return err
	}
	a.surfaceCatalogWarnings(warmWarnings)
	return nil
}

// deleteModelLocked validates and writes a model deletion. Caller holds runtime.mu.
func (a *Agent) deleteModelLocked(providerID, modelID string) error {
	if a.ensureRuntime().sessionLocked().busy {
		return fmt.Errorf("cannot delete model while a turn is running")
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if a.modelSourceLocked(providerID, modelID) != modelSourceUser {
		return fmt.Errorf("cannot delete model %q: only user-added models can be removed; hide or reset it instead", modelID)
	}
	return a.mutateConfigRootLocked(func(root map[string]any) error {
		providers, err := providerRootMap(root)
		if err != nil {
			return err
		}
		pm, ok := providers[providerID].(map[string]any)
		if !ok {
			return nil
		}
		models, ok := pm["models"].(map[string]any)
		if !ok {
			return nil
		}
		delete(models, modelID)
		return nil
	})
}

var resettableModelFields = map[string]struct{}{
	"name": {}, "context_window": {}, "max_output_tokens": {}, "input_modalities": {},
	"system_role": {}, "usage_in_stream": {}, "extra_body": {}, "cost": {}, "protocol_metadata": {},
}

// ResetModelField deletes a single user-layer override on a model, reverting it
// to the bundled/discovery value. No-op (no write) when no override exists. The
// discovery cache is warmed for the providers the reload would refresh (outside
// runtime.mu, see warmSettingsEditDiscovery); the config mutation and the
// locked reload then share a single lock hold.
func (a *Agent) ResetModelField(providerID, modelID, field string) error {
	warmWarnings, err := a.warmSettingsEditDiscovery()
	if err != nil {
		return err
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	changed, err := a.resetModelFieldLocked(providerID, modelID, field)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := a.reloadLocked(); err != nil {
		return err
	}
	a.surfaceCatalogWarnings(warmWarnings)
	return nil
}

// resetModelFieldLocked validates and writes a model field reset, reporting
// whether the write actually removed an override. Caller holds runtime.mu.
func (a *Agent) resetModelFieldLocked(providerID, modelID, field string) (bool, error) {
	if a.ensureRuntime().sessionLocked().busy {
		return false, fmt.Errorf("cannot reset model field while a turn is running")
	}
	if _, ok := resettableModelFields[field]; !ok {
		return false, fmt.Errorf("field %q cannot be reset", field)
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if field == "context_window" && a.modelSourceLocked(providerID, modelID) == modelSourceUser {
		return false, fmt.Errorf("cannot reset context_window for user-added model %q", modelID)
	}
	changed := false
	if err := a.mutateModelConfig(coremodel.ModelRef{Provider: providerID, Model: modelID}, func(m map[string]any) error {
		if _, present := m[field]; present {
			delete(m, field)
			changed = true
		}
		return nil
	}); err != nil {
		return false, err
	}
	return changed, nil
}

// providerConfigCandidate carries one provider edit through preparation,
// discovery, and commit. root is a private copy of the full config root with
// the edit applied but not yet written; nothing is published until commit.
type providerConfigCandidate struct {
	providerID string
	root       map[string]any
}

// SetProviderConfig edits an existing provider's transport and provider-level
// fields (user-layer overrides). The API key value is never written here. The
// edit runs through the candidate path: preparation applies the edit to a
// private config root under a short runtime lock hold, live discovery fetches
// the candidate transport outside the lock, and commit rechecks the owner
// state and atomically publishes the root and any discovery result under a
// final hold. No HTTP runs under runtime.mu.
func (a *Agent) SetProviderConfig(providerID string, cfg ProviderConfigInput) error {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	candidate, err := a.prepareSetProviderConfigLocked(providerID, cfg)
	rt.mu.Unlock()
	if err != nil {
		return err
	}
	attempted, discovered, warnings, err := a.discoverProviderCandidate(candidate)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return a.commitProviderConfigCandidateLocked(candidate, attempted, discovered, warnings)
}

// prepareSetProviderConfigLocked validates a provider edit against the live
// catalog and applies it to a private copy of the config root without writing
// anything. Caller holds runtime.mu; the returned candidate's root carries the
// applied edit for the discovery and commit phases.
func (a *Agent) prepareSetProviderConfigLocked(providerID string, cfg ProviderConfigInput) (providerConfigCandidate, error) {
	rt := a.ensureRuntime()
	if rt.closed {
		return providerConfigCandidate{}, errOwnerClosed
	}
	if rt.sessionLocked().busy {
		return providerConfigCandidate{}, fmt.Errorf("cannot edit provider while a turn is running")
	}
	if rt.sessionLocked().transitioning {
		return providerConfigCandidate{}, fmt.Errorf("session is changing; retry")
	}
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return providerConfigCandidate{}, fmt.Errorf("provider id is required")
	}
	prov := a.catalog.Providers[providerID]
	if prov == nil {
		return providerConfigCandidate{}, fmt.Errorf("provider %q not found", providerID)
	}
	// Built-in providers lock their identity/protocol fields.
	if prov.Builtin {
		if err := lockedProviderFields(cfg); err != nil {
			return providerConfigCandidate{}, err
		}
	}
	if len(cfg.Headers) != 0 {
		if err := validateCustomHeaders(cfg.Headers); err != nil {
			return providerConfigCandidate{}, err
		}
	}
	newEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if newEnv != "" && newEnv != prov.Transport.APIKeyEnv {
		if providerConnected(prov) {
			return providerConfigCandidate{}, fmt.Errorf("disconnect provider %q before changing its API key variable", providerID)
		}
		if a.apiKeyEnvInUseLocked(newEnv, providerID) {
			return providerConfigCandidate{}, fmt.Errorf("API key variable %q is already used by another provider", newEnv)
		}
	}
	// Headers are written wholesale so removals/clears take effect (not just
	// additions). On a built-in the bundled keys (e.g. OpenRouter attribution)
	// are stripped first, which also heals any leaked override.
	headersProvided := cfg.Headers != nil
	cleaned := cfg.Headers
	if headersProvided {
		if prov.Builtin {
			cleaned = stripBundledHeaderKeys(cfg.Headers, providerID)
		}
		cfg.Headers = nil
	}
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		return providerConfigCandidate{}, err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return providerConfigCandidate{}, fmt.Errorf("parse config %s: %w", a.configPath, err)
	}
	if err := mutateProviderConfigRoot(root, providerID, func(pm map[string]any) error {
		applyProviderFields(pm, cfg)
		if headersProvided {
			writeTransportHeaders(pm, cleaned)
		}
		return nil
	}); err != nil {
		return providerConfigCandidate{}, err
	}
	// Normalize the candidate root through a JSON round trip, mirroring the
	// write-then-reload the warm path performs: in-memory mutation injects
	// Go-native shapes (map[string]string headers, *bool toggles) that the
	// catalog builder and the committed file expect in JSON shape.
	normalized, err := json.Marshal(root)
	if err != nil {
		return providerConfigCandidate{}, err
	}
	if err := json.Unmarshal(normalized, &root); err != nil {
		return providerConfigCandidate{}, err
	}
	if !prov.Builtin {
		providers, _ := root["providers"].(map[string]any)
		pm, _ := providers[providerID].(map[string]any)
		if err := validateRawProviderConfig(providerID, pm); err != nil {
			return providerConfigCandidate{}, err
		}
	}
	return providerConfigCandidate{providerID: providerID, root: root}, nil
}

// mutateProviderConfigRoot navigates to the specified provider map inside an
// in-memory config root and calls mutate on it, without reading or writing any
// file. It mirrors the read-modify half of mutateProviderConfig for the
// candidate path.
func mutateProviderConfigRoot(root map[string]any, providerID string, mutate func(providerMap map[string]any) error) error {
	if root == nil {
		root = map[string]any{}
	}
	providers, ok := root["providers"].(map[string]any)
	if !ok {
		providers = map[string]any{}
		root["providers"] = providers
	}
	providerRaw, ok := providers[providerID]
	if !ok {
		providerRaw = map[string]any{}
		providers[providerID] = providerRaw
	}
	providerMap, ok := providerRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("providers.%s must be an object", providerID)
	}
	return mutate(providerMap)
}

// discoverProviderCandidate runs the candidate edit's live discovery outside
// runtime.mu. It builds an offline catalog from the candidate's provider map,
// refuses when the edited provider is unavailable after the edit, and — only
// when the candidate provider is connected, discovery-enabled, and due —
// fetches /models through the candidate transport. Fetch performs no config,
// attempt, cache, or owner-state write; the caller publishes those under the
// final lock hold.
func (a *Agent) discoverProviderCandidate(candidate providerConfigCandidate) (attempted bool, discovered *catalog.DiscoveredProvider, warnings []catalog.Warning, err error) {
	providers, ok := candidate.root["providers"].(map[string]any)
	if !ok {
		return false, nil, nil, fmt.Errorf("provider %q is unavailable after edit", candidate.providerID)
	}
	buildResult, err := catalog.BuildOffline(a.home, providers)
	if err != nil {
		return false, nil, nil, err
	}
	prov := buildResult.Catalog.Providers[candidate.providerID]
	if prov == nil {
		return false, nil, nil, fmt.Errorf("provider %q is unavailable after edit", candidate.providerID)
	}
	if !providerConnected(prov) || !prov.Discovery {
		return false, nil, nil, nil
	}
	_, attempts, _ := catalog.ReadDiscoveryCache(a.home)
	now := time.Now().UTC()
	due := false
	for _, id := range catalog.DiscoveryRefreshCandidates(buildResult.Catalog, attempts, now) {
		if id == candidate.providerID {
			due = true
			break
		}
	}
	if !due {
		return false, nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()
	discoveredProvider, err := catalog.FetchDiscovery(ctx, discoveryHTTPClient, prov)
	if err != nil {
		return true, nil, []catalog.Warning{{Kind: "discovery_failure", Provider: candidate.providerID, Message: err.Error()}}, nil
	}
	return true, &discoveredProvider, nil, nil
}

// commitProviderConfigCandidateLocked atomically publishes a prepared provider
// edit under the final runtime lock hold. It rechecks close, busy,
// transitioning, and the connected-provider api_key_env guard before writing
// anything; a refusal discards the fetched data. On success it writes the
// candidate root, then records the discovery outcome (a successful fetch
// writes the cache with both fetched and attempted time; a failed fetch writes
// only the attempt), reloads without refresh, and appends the candidate
// warnings to the catalog warning group. No HTTP runs under runtime.mu.
func (a *Agent) commitProviderConfigCandidateLocked(candidate providerConfigCandidate, attempted bool, discovered *catalog.DiscoveredProvider, warnings []catalog.Warning) error {
	rt := a.ensureRuntime()
	if rt.closed {
		return errOwnerClosed
	}
	if rt.sessionLocked().busy {
		return fmt.Errorf("cannot edit provider while a turn is running")
	}
	if rt.sessionLocked().transitioning {
		return fmt.Errorf("session is changing; retry")
	}
	// Recheck the api_key_env guard against the currently persisted raw
	// override: preparation validated the edit against the live catalog, but a
	// concurrent connect or reset may have changed the persisted override
	// (including deleting it) and made the live provider connected in the
	// meantime. The refusal must land before any config or cache write. The
	// comparison is raw override vs raw override, so unrelated edits on
	// providers that only inherit a bundled env name are not blocked.
	// Concurrent complete config edits are not compared byte-wise; the whole
	// candidate root is written atomically and the commit landing last wins.
	prov := a.catalog.Providers[candidate.providerID]
	if prov == nil {
		return fmt.Errorf("provider %q not found", candidate.providerID)
	}
	candidateEnv := ""
	if providers, ok := candidate.root["providers"].(map[string]any); ok {
		if pm, ok := providers[candidate.providerID].(map[string]any); ok {
			if tr, ok := pm["transport"].(map[string]any); ok {
				candidateEnv, _ = tr["api_key_env"].(string)
			}
		}
	}
	persistedEnv := ""
	if data, err := os.ReadFile(a.configPath); err == nil {
		var persistedRoot map[string]any
		if err := json.Unmarshal(data, &persistedRoot); err == nil {
			if providers, ok := persistedRoot["providers"].(map[string]any); ok {
				if pm, ok := providers[candidate.providerID].(map[string]any); ok {
					if tr, ok := pm["transport"].(map[string]any); ok {
						persistedEnv, _ = tr["api_key_env"].(string)
					}
				}
			}
		}
	}
	if candidateEnv != persistedEnv {
		if providerConnected(prov) {
			return fmt.Errorf("disconnect provider %q before changing its API key variable", candidate.providerID)
		}
		if candidateEnv != "" && a.apiKeyEnvInUseLocked(candidateEnv, candidate.providerID) {
			return fmt.Errorf("API key variable %q is already used by another provider", candidateEnv)
		}
	}
	if err := writeAgentConfigAtomic(a.configPath, candidate.root); err != nil {
		return err
	}
	if attempted {
		if discovered != nil {
			if err := catalog.WriteDiscoveryCache(a.home, candidate.providerID, *discovered, time.Now().UTC()); err != nil {
				warnings = append(warnings, catalog.Warning{Kind: "discovery_failure", Provider: candidate.providerID, Message: fmt.Sprintf("write discovery cache: %v", err)})
			}
		} else {
			if err := catalog.WriteDiscoveryAttempt(a.home, candidate.providerID, time.Now().UTC()); err != nil {
				warnings = append(warnings, catalog.Warning{Kind: "discovery_failure", Provider: candidate.providerID, Message: fmt.Sprintf("write discovery attempt: %v", err)})
			}
		}
	}
	if err := a.reloadLockedNoRefresh(); err != nil {
		return err
	}
	a.surfaceCatalogWarnings(warnings)
	return nil
}

func stripBundledHeaderKeys(headers map[string]string, providerID string) map[string]string {
	bundled := catalog.BundledProviderHeaders(providerID)
	out := map[string]string{}
	for k, v := range headers {
		skip := false
		for bk := range bundled {
			if strings.EqualFold(k, bk) {
				skip = true
				break
			}
		}
		if !skip {
			out[k] = v
		}
	}
	return out
}

func writeTransportHeaders(pm map[string]any, headers map[string]string) {
	transport, _ := pm["transport"].(map[string]any)
	if transport == nil {
		transport = map[string]any{}
		pm["transport"] = transport
	}
	if len(headers) == 0 {
		delete(transport, "headers")
	} else {
		transport["headers"] = headers
	}
}

var resettableProviderFields = map[string]struct{}{
	"name": {}, "base_url": {}, "api_key_env": {}, "headers": {}, "options": {},
	"system_role": {}, "usage_in_stream": {}, "max_tokens_field": {}, "extra_body": {},
	"discovery": {}, "protocol_metadata": {},
}

var transportFields = map[string]struct{}{
	"base_url": {}, "api_key_env": {}, "headers": {}, "options": {},
}

// ResetProviderField deletes a single user-layer override on a provider,
// reverting it to the bundled value. No-op (no write) when no override exists.
// Transport-field resets run through the candidate path (preparation applies
// the reset to a private config root, live discovery fetches the candidate
// transport outside the lock, and commit rechecks owner state before
// publishing); other fields keep the warm discovery path.
func (a *Agent) ResetProviderField(providerID, field string) error {
	if _, isTransport := transportFields[field]; !isTransport {
		warmWarnings, err := a.warmSettingsEditDiscovery()
		if err != nil {
			return err
		}
		rt := a.ensureRuntime()
		rt.mu.Lock()
		defer rt.mu.Unlock()
		changed, err := a.resetProviderFieldLocked(providerID, field)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		if err := a.reloadLocked(); err != nil {
			return err
		}
		a.surfaceCatalogWarnings(warmWarnings)
		return nil
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	candidate, changed, err := a.prepareResetProviderFieldLocked(providerID, field)
	rt.mu.Unlock()
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	attempted, discovered, warnings, err := a.discoverProviderCandidate(candidate)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return a.commitProviderConfigCandidateLocked(candidate, attempted, discovered, warnings)
}

// prepareResetProviderFieldLocked applies a transport-field provider reset to
// a private copy of the config root without writing anything, reporting
// whether the reset actually removed an override. Caller holds runtime.mu. A
// no-op reset (no override present) returns changed=false so the caller can
// skip discovery and commit entirely.
func (a *Agent) prepareResetProviderFieldLocked(providerID, field string) (providerConfigCandidate, bool, error) {
	rt := a.ensureRuntime()
	if rt.closed {
		return providerConfigCandidate{}, false, errOwnerClosed
	}
	if rt.sessionLocked().busy {
		return providerConfigCandidate{}, false, fmt.Errorf("cannot reset provider field while a turn is running")
	}
	if rt.sessionLocked().transitioning {
		return providerConfigCandidate{}, false, fmt.Errorf("session is changing; retry")
	}
	if _, ok := resettableProviderFields[field]; !ok {
		return providerConfigCandidate{}, false, fmt.Errorf("field %q cannot be reset", field)
	}
	if field == "api_key_env" {
		if prov := a.catalog.Providers[providerID]; prov != nil && providerConnected(prov) {
			return providerConfigCandidate{}, false, fmt.Errorf("disconnect provider %q before resetting its API key variable", providerID)
		}
	}
	_, isTransport := transportFields[field]
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		return providerConfigCandidate{}, false, err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return providerConfigCandidate{}, false, fmt.Errorf("parse config %s: %w", a.configPath, err)
	}
	changed := false
	if err := mutateProviderConfigRoot(root, providerID, func(pm map[string]any) error {
		target := pm
		if isTransport {
			transport, ok := pm["transport"].(map[string]any)
			if !ok {
				return nil
			}
			target = transport
		}
		if _, present := target[field]; present {
			delete(target, field)
			changed = true
		}
		return nil
	}); err != nil {
		return providerConfigCandidate{}, false, err
	}
	// Normalize the candidate root through a JSON round trip, mirroring the
	// write-then-reload the warm path performs (see
	// prepareSetProviderConfigLocked).
	normalized, err := json.Marshal(root)
	if err != nil {
		return providerConfigCandidate{}, false, err
	}
	if err := json.Unmarshal(normalized, &root); err != nil {
		return providerConfigCandidate{}, false, err
	}
	if prov := a.catalog.Providers[providerID]; prov != nil && !prov.Builtin {
		providers, _ := root["providers"].(map[string]any)
		pm, _ := providers[providerID].(map[string]any)
		if err := validateRawProviderConfig(providerID, pm); err != nil {
			return providerConfigCandidate{}, false, err
		}
	}
	return providerConfigCandidate{providerID: providerID, root: root}, changed, nil
}

// resetProviderFieldLocked validates and writes a non-transport provider field
// reset, reporting whether the write actually removed an override. Caller
// holds runtime.mu. Transport-field resets go through the candidate path
// instead (prepareResetProviderFieldLocked).
func (a *Agent) resetProviderFieldLocked(providerID, field string) (bool, error) {
	if a.ensureRuntime().sessionLocked().busy {
		return false, fmt.Errorf("cannot reset provider field while a turn is running")
	}
	if _, ok := resettableProviderFields[field]; !ok {
		return false, fmt.Errorf("field %q cannot be reset", field)
	}
	if field == "api_key_env" {
		if prov := a.catalog.Providers[providerID]; prov != nil && providerConnected(prov) {
			return false, fmt.Errorf("disconnect provider %q before resetting its API key variable", providerID)
		}
	}
	_, isTransport := transportFields[field]
	changed := false
	if err := a.mutateProviderConfig(providerID, func(pm map[string]any) error {
		target := pm
		if isTransport {
			transport, ok := pm["transport"].(map[string]any)
			if !ok {
				return nil
			}
			target = transport
		}
		if _, present := target[field]; present {
			delete(target, field)
			changed = true
		}
		return nil
	}); err != nil {
		return false, err
	}
	return changed, nil
}

// warmSettingsEditDiscovery runs the first two phases of a settings edit's
// reload. Under runtime.mu it builds the same no-refresh catalog the locked
// reload starts from and works out which providers that reload would fetch
// discovery for; without the lock it then warms the discovery cache for
// exactly those providers by calling the same refresh function the locked
// reload calls (catalog.RefreshProviderDiscoveryWithConfigPath, which applies
// its own timeout bound internally). Nothing is merged, rebuilt, or applied
// here: the only effect is that the cache and attempt markers on disk are
// fresh, so the reload that follows (in the edit's single lock hold) finds the
// attempts recent and does no network I/O while holding runtime.mu. The
// refresh warnings are returned so the caller can surface them next to the
// reload's own catalog warnings.
func (a *Agent) warmSettingsEditDiscovery() ([]catalog.Warning, error) {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	modelCatalog, _, err := a.loadCatalogLocked(false)
	if err != nil {
		rt.mu.Unlock()
		return nil, err
	}
	_, attempts, _ := catalog.ReadDiscoveryCache(a.home)
	now := time.Now().UTC()
	var planned []string
	for _, providerID := range catalog.DiscoveryRefreshCandidates(modelCatalog, attempts, now) {
		prov := modelCatalog.Providers[providerID]
		if prov == nil || !providerConnected(prov) {
			continue
		}
		if prov.Transport.APIKeyEnv != "" && os.Getenv(prov.Transport.APIKeyEnv) == "" {
			continue
		}
		planned = append(planned, providerID)
	}
	rt.mu.Unlock()

	var warnings []catalog.Warning
	for _, providerID := range planned {
		_, providerWarnings := catalog.RefreshProviderDiscoveryWithConfigPath(context.Background(), a.home, a.configPath, modelCatalog, providerID)
		warnings = append(warnings, providerWarnings...)
	}
	return warnings, nil
}

// surfaceCatalogWarnings appends a settings edit's warm-phase discovery
// refresh warnings to the catalog warning group, next to the locked reload's
// own warnings.
func (a *Agent) surfaceCatalogWarnings(warnings []catalog.Warning) {
	for _, w := range catalogWarningsToPromptWarnings(warnings) {
		a.addWarning("catalog", w)
	}
}
