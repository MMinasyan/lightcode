package agent

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

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
	if a.ensureRuntime().session().busy {
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
// overrides). Used for both editing and "add model".
func (a *Agent) SaveModel(providerID, modelID string, cfg ModelConfigInput) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().session().busy {
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
	if err := a.mutateModelConfig(ref, func(m map[string]any) error {
		applyModelFields(m, cfg)
		return nil
	}); err != nil {
		return err
	}
	return a.reloadLocked()
}

// DeleteModel removes a user-added model from config. Bundled/discovered models
// cannot be deleted (the merge would re-add them); hide or reset them instead.
func (a *Agent) DeleteModel(providerID, modelID string) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().session().busy {
		return fmt.Errorf("cannot delete model while a turn is running")
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if a.modelSourceLocked(providerID, modelID) != modelSourceUser {
		return fmt.Errorf("cannot delete model %q: only user-added models can be removed; hide or reset it instead", modelID)
	}
	if err := a.mutateConfigRootLocked(func(root map[string]any) error {
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
	}); err != nil {
		return err
	}
	return a.reloadLocked()
}

var resettableModelFields = map[string]struct{}{
	"name": {}, "context_window": {}, "max_output_tokens": {}, "input_modalities": {},
	"system_role": {}, "usage_in_stream": {}, "extra_body": {}, "cost": {}, "protocol_metadata": {},
}

// ResetModelField deletes a single user-layer override on a model, reverting it
// to the bundled/discovery value. No-op (no write) when no override exists.
func (a *Agent) ResetModelField(providerID, modelID, field string) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().session().busy {
		return fmt.Errorf("cannot reset model field while a turn is running")
	}
	if _, ok := resettableModelFields[field]; !ok {
		return fmt.Errorf("field %q cannot be reset", field)
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if field == "context_window" && a.modelSourceLocked(providerID, modelID) == modelSourceUser {
		return fmt.Errorf("cannot reset context_window for user-added model %q", modelID)
	}
	changed := false
	if err := a.mutateModelConfig(coremodel.ModelRef{Provider: providerID, Model: modelID}, func(m map[string]any) error {
		if _, present := m[field]; present {
			delete(m, field)
			changed = true
		}
		return nil
	}); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return a.reloadLocked()
}

// SetProviderConfig edits an existing provider's transport and provider-level
// fields (user-layer overrides). The API key value is never written here.
func (a *Agent) SetProviderConfig(providerID string, cfg ProviderConfigInput) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().session().busy {
		return fmt.Errorf("cannot edit provider while a turn is running")
	}
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return fmt.Errorf("provider id is required")
	}
	prov := a.catalog.Providers[providerID]
	if prov == nil {
		return fmt.Errorf("provider %q not found", providerID)
	}
	// Built-in providers lock their identity/protocol fields.
	if prov.Builtin {
		if err := lockedProviderFields(cfg); err != nil {
			return err
		}
	}
	if len(cfg.Headers) != 0 {
		if err := validateCustomHeaders(cfg.Headers); err != nil {
			return err
		}
	}
	newEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if newEnv != "" && newEnv != prov.Transport.APIKeyEnv {
		if providerConnected(prov) {
			return fmt.Errorf("disconnect provider %q before changing its API key variable", providerID)
		}
		if a.apiKeyEnvInUseLocked(newEnv, providerID) {
			return fmt.Errorf("API key variable %q is already used by another provider", newEnv)
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
	if err := a.mutateProviderConfig(providerID, func(pm map[string]any) error {
		applyProviderFields(pm, cfg)
		if headersProvided {
			writeTransportHeaders(pm, cleaned)
		}
		return nil
	}); err != nil {
		return err
	}
	return a.reloadLocked()
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
func (a *Agent) ResetProviderField(providerID, field string) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().session().busy {
		return fmt.Errorf("cannot reset provider field while a turn is running")
	}
	if _, ok := resettableProviderFields[field]; !ok {
		return fmt.Errorf("field %q cannot be reset", field)
	}
	if field == "api_key_env" {
		if prov := a.catalog.Providers[providerID]; prov != nil && providerConnected(prov) {
			return fmt.Errorf("disconnect provider %q before resetting its API key variable", providerID)
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
		return err
	}
	if !changed {
		return nil
	}
	return a.reloadLocked()
}
