package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
)

// discoveryTimeout bounds connect-time discovery HTTP calls so a stalled
// endpoint cannot block the runtime lock (and therefore every adapter call)
// indefinitely.
const discoveryTimeout = 30 * time.Second

// discoveryHTTPClient is a shared, timeout-bounded client used for connect-time
// discovery. It deliberately replaces http.DefaultClient, which has no timeout.
var discoveryHTTPClient = &http.Client{Timeout: discoveryTimeout}

// ProviderList returns all effective providers with live connection/key-source status.
func (a *Agent) ProviderList() []ProviderStatus {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	return a.providerListLocked()
}

func (a *Agent) providerListLocked() []ProviderStatus {
	if a.catalog == nil {
		return nil
	}
	ids := make([]string, 0, len(a.catalog.Providers))
	for id := range a.catalog.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ProviderStatus, 0, len(ids))
	for _, id := range ids {
		out = append(out, a.providerStatusLocked(id, a.catalog.Providers[id]))
	}
	return out
}

func (a *Agent) providerStatusLocked(id string, prov *catalog.Provider) ProviderStatus {
	status := ProviderStatus{ID: id, KeySource: ProviderKeySourceNone}
	if prov == nil {
		return status
	}
	status.Name = prov.Name
	if status.Name == "" {
		status.Name = id
	}
	status.Builtin = prov.Builtin
	status.APIKeyEnv = prov.Transport.APIKeyEnv
	status.GeneratedKeyEnv = a.generateAPIKeyEnvNameLocked(id)
	status.BaseURL = prov.Transport.BaseURL
	status.Discovery = prov.Discovery
	status.ModelCount = len(prov.Models)
	status.UsableModels = usableModelCount(prov)
	status.Connected = providerConnected(prov)
	status.KeySource = a.keySourceLocked(prov.Transport.APIKeyEnv)
	status.Disconnectable = status.KeySource == ProviderKeySourceManaged
	status.Removable = !prov.Builtin && (status.KeySource == ProviderKeySourceKeyless || !status.Connected)
	return status
}

func usableModelCount(prov *catalog.Provider) int {
	if prov == nil {
		return 0
	}
	count := 0
	for _, model := range prov.Models {
		if model != nil && model.ContextWindow > 0 {
			count++
		}
	}
	return count
}

// keySourceLocked classifies via the shared classifier with the agent's env
// semantics: LoadDotEnv already Setenv'd every managed key, so external means
// set-in-env and not managed — managed keys stay managed.
func (a *Agent) keySourceLocked(apiKeyEnv string) string {
	managed := func(name string) bool { return a.env != nil && a.env.IsManaged(name) }
	external := func(name string) bool { return os.Getenv(name) != "" && !managed(name) }
	return config.ClassifyKeySource(apiKeyEnv, external, managed)
}

// GenerateAPIKeyEnvName returns a stable, unique env var name derived from providerID.
func (a *Agent) GenerateAPIKeyEnvName(providerID string) string {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	return a.generateAPIKeyEnvNameLocked(providerID)
}

func (a *Agent) generateAPIKeyEnvNameLocked(providerID string) string {
	base := "LIGHTCODE_" + upperSnake(providerID) + "_API_KEY"
	if base == "LIGHTCODE__API_KEY" {
		base = "LIGHTCODE_PROVIDER_API_KEY"
	}
	used := map[string]struct{}{}
	if a.catalog != nil {
		for _, prov := range a.catalog.Providers {
			if prov != nil && prov.Transport.APIKeyEnv != "" {
				used[prov.Transport.APIKeyEnv] = struct{}{}
			}
		}
	}
	if _, ok := used[base]; !ok {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", base, i)
		if _, ok := used[candidate]; !ok {
			return candidate
		}
	}
}

func upperSnake(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToUpper(r))
			lastUnderscore = false
			continue
		}
		if !lastUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "PROVIDER"
	}
	return out
}

// ConnectProvider connects an existing catalog provider. Providers that already
// have usable models only need credential persistence; empty discovery-backed
// providers run a one-shot connect-time discovery before any secret is persisted.
// The network fetch runs outside runtime.mu so a stalled endpoint cannot block
// other adapter calls.
func (a *Agent) ConnectProvider(providerID, apiKey string) error {
	// Phase 1: snapshot provider state and resolve key under the lock.
	type connectPlan struct {
		needsDiscovery bool
		apiKeyEnv      string
		key            string
		shouldPersist  bool
		prov           *catalog.Provider
		transport      catalog.Transport
	}
	var plan connectPlan
	a.ensureRuntime().mu.Lock()
	if a.ensureRuntime().sessionLocked().busy {
		a.ensureRuntime().mu.Unlock()
		return fmt.Errorf("cannot connect provider while a turn is running")
	}
	prov := a.catalog.Providers[providerID]
	if prov == nil {
		a.ensureRuntime().mu.Unlock()
		return fmt.Errorf("provider %q not found", providerID)
	}
	capturedTransport := prov.Transport
	if prov.Transport.APIKeyEnv == "" {
		current := a.catalog.Providers[providerID]
		if current == nil {
			a.ensureRuntime().mu.Unlock()
			return fmt.Errorf("provider %q not found", providerID)
		}
		if !catalog.SameTransport(capturedTransport, current.Transport) {
			a.ensureRuntime().mu.Unlock()
			return fmt.Errorf("provider %s changed while connecting; retry", providerID)
		}
		err := a.reloadLockedNoRefresh()
		a.ensureRuntime().mu.Unlock()
		return err
	}
	if usableModelCount(prov) == 0 {
		if !prov.Discovery {
			a.ensureRuntime().mu.Unlock()
			return fmt.Errorf("provider %q has no usable models and discovery is disabled", providerID)
		}
		key, shouldPersist, err := a.resolveConnectKeyLocked(prov.Transport.APIKeyEnv, apiKey)
		if err != nil {
			a.ensureRuntime().mu.Unlock()
			return err
		}
		plan = connectPlan{needsDiscovery: true, apiKeyEnv: prov.Transport.APIKeyEnv, key: key, shouldPersist: shouldPersist, prov: prov, transport: capturedTransport}
	} else {
		plan = connectPlan{apiKeyEnv: prov.Transport.APIKeyEnv, prov: prov, transport: capturedTransport}
	}
	a.ensureRuntime().mu.Unlock()

	// Phase 2: network call without holding runtime.mu.
	if plan.needsDiscovery {
		ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
		defer cancel()
		discovered, err := fetchConnectDiscovery(ctx, discoveryHTTPClient, plan.prov, plan.key)
		if err != nil {
			return err
		}
		if usableDiscoveredModelCount(discovered) == 0 {
			return fmt.Errorf("provider %q discovery returned no usable models", providerID)
		}
		if a.connectProviderBeforeFinalLock != nil {
			a.connectProviderBeforeFinalLock()
		}
		// Phase 3: re-acquire lock, re-validate, persist. The final hold checks
		// owner open before anything, publishes the discovery cache through a
		// one-attempt Try (contention errors before env/reload), publishes the
		// key through the guarded Try env sink, then reloads.
		a.ensureRuntime().mu.Lock()
		defer a.ensureRuntime().mu.Unlock()
		if err := a.requireOwnerOpenLocked(); err != nil {
			return err
		}
		if a.ensureRuntime().sessionLocked().busy {
			return fmt.Errorf("cannot connect provider while a turn is running")
		}
		current := a.catalog.Providers[providerID]
		if current == nil {
			return fmt.Errorf("provider %q not found", providerID)
		}
		if !catalog.SameTransport(plan.transport, current.Transport) {
			return fmt.Errorf("provider %s changed while connecting; retry", providerID)
		}
		if usableModelCount(current) > 0 {
			// Another path populated models while we were fetching; skip cache write.
		} else {
			ok, err := catalog.TryWriteDiscoveryCache(a.home, providerID, plan.transport, discovered, time.Now().UTC())
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("discovery lock is held for %q; retry", providerID)
			}
		}
		if plan.shouldPersist {
			if err := a.setManagedKeyLocked(plan.apiKeyEnv, apiKey); err != nil {
				return err
			}
		}
		return a.reloadLockedNoRefresh()
	}

	// Non-discovery path: persist key and reload. The final hold checks owner
	// open, publishes the key through the guarded Try env sink, then reloads.
	if a.connectProviderBeforeFinalLock != nil {
		a.connectProviderBeforeFinalLock()
	}
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if err := a.requireOwnerOpenLocked(); err != nil {
		return err
	}
	if a.ensureRuntime().sessionLocked().busy {
		return fmt.Errorf("cannot connect provider while a turn is running")
	}
	current := a.catalog.Providers[providerID]
	if current == nil {
		return fmt.Errorf("provider %q not found", providerID)
	}
	if !catalog.SameTransport(plan.transport, current.Transport) {
		return fmt.Errorf("provider %s changed while connecting; retry", providerID)
	}
	if apiKey != "" {
		if err := a.setManagedKeyLocked(plan.apiKeyEnv, apiKey); err != nil {
			if !errors.Is(err, config.ErrExternalKey) {
				return err
			}
			if os.Getenv(plan.apiKeyEnv) == "" {
				return fmt.Errorf("provider %q env var %s is externally set but empty", providerID, plan.apiKeyEnv)
			}
		}
		return a.reloadLockedNoRefresh()
	}
	if os.Getenv(plan.apiKeyEnv) == "" {
		return fmt.Errorf("provider requires API key env var %s", plan.apiKeyEnv)
	}
	return a.reloadLockedNoRefresh()
}

func (a *Agent) resolveConnectKeyLocked(apiKeyEnv, apiKey string) (key string, shouldPersist bool, err error) {
	if apiKeyEnv == "" {
		return apiKey, false, nil
	}
	if a.env != nil && a.env.IsManaged(apiKeyEnv) {
		if apiKey != "" {
			return apiKey, true, nil
		}
		key = os.Getenv(apiKeyEnv)
		if key == "" {
			return "", false, fmt.Errorf("provider requires API key env var %s", apiKeyEnv)
		}
		return key, false, nil
	}
	if external, exists := os.LookupEnv(apiKeyEnv); exists {
		if external == "" {
			return "", false, fmt.Errorf("provider env var %s is externally set but empty", apiKeyEnv)
		}
		return external, false, nil
	}
	if apiKey == "" {
		return "", false, fmt.Errorf("provider requires API key env var %s", apiKeyEnv)
	}
	return apiKey, true, nil
}

// setManagedKeyLocked persists one API key through the owner's guarded env
// sink: the guard refuses after owner close and the env write is a single
// one-attempt Try, so a foreign env-lock holder returns an operation error
// instead of blocking a shutting-down owner. Caller holds runtime.mu.
func (a *Agent) setManagedKeyLocked(apiKeyEnv, apiKey string) error {
	if apiKeyEnv == "" || apiKey == "" {
		return nil
	}
	if a.env == nil {
		return fmt.Errorf("managed env is not initialized")
	}
	if err := a.requireOwnerOpenLocked(); err != nil {
		return err
	}
	return a.env.TrySet(apiKeyEnv, apiKey)
}

// removeManagedKeyLocked removes one API key through the owner's guarded env
// sink: the guard refuses after owner close and the env write is a single
// one-attempt Try. Caller holds runtime.mu.
func (a *Agent) removeManagedKeyLocked(apiKeyEnv string) error {
	if apiKeyEnv == "" {
		return nil
	}
	if a.env == nil {
		return fmt.Errorf("managed env is not initialized")
	}
	if err := a.requireOwnerOpenLocked(); err != nil {
		return err
	}
	return a.env.TryRemove(apiKeyEnv)
}

func fetchConnectDiscovery(ctx context.Context, client *http.Client, prov *catalog.Provider, apiKey string) (catalog.DiscoveredProvider, error) {
	if prov == nil {
		return catalog.DiscoveredProvider{}, fmt.Errorf("provider is nil")
	}
	provisional := *prov
	provisional.Transport.APIKeyEnv = ""
	provisional.Transport.Headers = cloneHeaders(prov.Transport.Headers)
	if apiKey != "" {
		provisional.Transport.Headers["Authorization"] = "Bearer " + apiKey
	}
	return catalog.FetchDiscovery(ctx, client, &provisional)
}

func cloneHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// DiscoverCustomProvider runs one-shot discovery for an unsaved custom provider.
// It persists nothing. The network fetch runs outside runtime.mu so a stalled
// endpoint cannot block other adapter calls.
func (a *Agent) DiscoverCustomProvider(req CustomProviderRequest) ([]DiscoveryModelCandidate, error) {
	// Phase 1: validate inputs under the lock. The write-free fetch refuses at
	// entry after owner close; it publishes nothing, so it needs no final guard.
	a.ensureRuntime().mu.Lock()
	if a.ensureRuntime().closed {
		a.ensureRuntime().mu.Unlock()
		return nil, errOwnerClosed
	}
	if a.ensureRuntime().sessionLocked().busy {
		a.ensureRuntime().mu.Unlock()
		return nil, fmt.Errorf("cannot discover provider while a turn is running")
	}
	providerID := strings.TrimSpace(req.ID)
	if providerID == "" {
		providerID = "custom"
	}
	if err := validateCustomHeaders(req.Headers); err != nil {
		a.ensureRuntime().mu.Unlock()
		return nil, err
	}
	prov := &catalog.Provider{ID: providerID, Name: req.Name, Transport: catalog.Transport{BaseURL: strings.TrimSpace(req.BaseURL), Headers: req.Headers, Options: req.Options}, Discovery: true, Models: map[string]*catalog.Model{}}
	key := req.APIKey
	if apiKeyEnv := strings.TrimSpace(req.APIKeyEnv); apiKeyEnv != "" {
		if external, exists := os.LookupEnv(apiKeyEnv); exists {
			if external == "" {
				a.ensureRuntime().mu.Unlock()
				return nil, fmt.Errorf("provider env var %s is externally set but empty", apiKeyEnv)
			}
			key = external
		}
	}
	a.ensureRuntime().mu.Unlock()

	// Phase 2: network call without holding runtime.mu.
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()
	discovered, err := fetchConnectDiscovery(ctx, discoveryHTTPClient, prov, key)
	if err != nil {
		return nil, err
	}
	return discoveryCandidates(discovered), nil
}

func usableDiscoveredModelCount(discovered catalog.DiscoveredProvider) int {
	count := 0
	for _, model := range discovered.Models {
		if model.ContextWindow > 0 {
			count++
		}
	}
	return count
}

func discoveryCandidates(discovered catalog.DiscoveredProvider) []DiscoveryModelCandidate {
	ids := make([]string, 0, len(discovered.Models))
	for id := range discovered.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]DiscoveryModelCandidate, 0, len(ids))
	for _, id := range ids {
		m := discovered.Models[id]
		out = append(out, DiscoveryModelCandidate{ID: id, Name: m.Name, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens, Cost: m.Cost, Usable: m.ContextWindow > 0})
	}
	return out
}

// AddCustomProvider persists a user-defined provider and selected model entries.
func (a *Agent) AddCustomProvider(req CustomProviderRequest) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().sessionLocked().busy {
		return fmt.Errorf("cannot add provider while a turn is running")
	}
	providerID := strings.TrimSpace(req.ID)
	if providerID == "" {
		return fmt.Errorf("provider id is required")
	}
	if _, exists := a.catalog.Providers[providerID]; exists {
		return fmt.Errorf("provider %q already exists", providerID)
	}
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		return fmt.Errorf("base_url is required")
	}
	if err := validateCustomHeaders(req.Headers); err != nil {
		return err
	}
	models, usable, err := customModelsMap(req.Models)
	if err != nil {
		return err
	}
	if usable == 0 {
		return fmt.Errorf("custom provider requires at least one usable model")
	}
	apiKeyEnv := strings.TrimSpace(req.APIKeyEnv)
	if apiKeyEnv == "" && strings.TrimSpace(req.APIKey) != "" {
		apiKeyEnv = a.generateAPIKeyEnvNameLocked(providerID)
	}
	if apiKeyEnv != "" && a.apiKeyEnvInUseLocked(apiKeyEnv, providerID) {
		return fmt.Errorf("api_key_env %s is already used by another provider", apiKeyEnv)
	}
	shouldPersistKey := false
	if apiKeyEnv != "" {
		if external, exists := os.LookupEnv(apiKeyEnv); exists {
			if external == "" {
				return fmt.Errorf("api_key_env %s is externally set but empty", apiKeyEnv)
			}
		} else {
			if strings.TrimSpace(req.APIKey) == "" {
				return fmt.Errorf("api key value is required for %s", apiKeyEnv)
			}
			shouldPersistKey = true
		}
	}
	providerName := strings.TrimSpace(req.Name)
	if providerName == "" {
		providerName = providerID
	}
	if err := a.mutateConfigRootLocked(func(root map[string]any) error {
		providers, err := providerRootMap(root)
		if err != nil {
			return err
		}
		if _, exists := providers[providerID]; exists {
			return fmt.Errorf("provider %q already exists", providerID)
		}
		providerMap := map[string]any{
			"name":      providerName,
			"transport": customTransportMap(baseURL, apiKeyEnv, req.Headers, req.Options),
			"discovery": req.Discovery,
			"models":    models,
		}
		if req.SystemRole != "" {
			providerMap["system_role"] = req.SystemRole
		}
		if req.UsageInStream != nil {
			providerMap["usage_in_stream"] = *req.UsageInStream
		}
		if strings.TrimSpace(req.MaxTokensField) != "" {
			providerMap["max_tokens_field"] = strings.TrimSpace(req.MaxTokensField)
		}
		if len(req.ExtraBody) != 0 {
			providerMap["extra_body"] = req.ExtraBody
		}
		if req.ProtocolMetadata != nil {
			providerMap["protocol_metadata"] = req.ProtocolMetadata
		}
		if req.Hidden {
			providerMap["hidden"] = true
		}
		if err := validateRawProviderConfig(providerID, providerMap); err != nil {
			return err
		}
		providers[providerID] = providerMap
		return nil
	}); err != nil {
		return err
	}
	if shouldPersistKey {
		if err := a.setManagedKeyLocked(apiKeyEnv, req.APIKey); err != nil {
			_ = a.deleteProviderConfigLocked(providerID)
			return err
		}
	}
	return a.reloadLockedNoRefresh()
}

func customModelsMap(inputs []CustomProviderModelInput) (map[string]any, int, error) {
	models := map[string]any{}
	usable := 0
	for _, input := range inputs {
		id := strings.TrimSpace(input.ID)
		if id == "" {
			return nil, 0, fmt.Errorf("model id is required")
		}
		if _, exists := models[id]; exists {
			return nil, 0, fmt.Errorf("duplicate model id %q", id)
		}
		m := map[string]any{}
		if strings.TrimSpace(input.Name) != "" {
			m["name"] = strings.TrimSpace(input.Name)
		}
		if input.ContextWindow > 0 {
			m["context_window"] = input.ContextWindow
			usable++
		}
		if input.MaxOutputTokens > 0 {
			m["max_output_tokens"] = input.MaxOutputTokens
		}
		if len(input.InputModalities) != 0 {
			m["input_modalities"] = input.InputModalities
		}
		if input.SystemRole != "" {
			m["system_role"] = input.SystemRole
		}
		if input.UsageInStream != nil {
			m["usage_in_stream"] = *input.UsageInStream
		}
		if len(input.ExtraBody) != 0 {
			m["extra_body"] = input.ExtraBody
		}
		if input.Cost != nil {
			m["cost"] = input.Cost
		}
		if input.ProtocolMetadata != nil {
			m["protocol_metadata"] = input.ProtocolMetadata
		}
		if input.Hidden {
			m["hidden"] = true
		}
		models[id] = m
	}
	return models, usable, nil
}

// DisconnectProvider removes only a Lightcode-managed key. External/shell keys
// are deliberately left alone and reported as not disconnectable.
func (a *Agent) DisconnectProvider(providerID string) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().sessionLocked().busy {
		return fmt.Errorf("cannot disconnect provider while a turn is running")
	}
	prov := a.catalog.Providers[providerID]
	if prov == nil {
		return fmt.Errorf("provider %q not found", providerID)
	}
	apiKeyEnv := prov.Transport.APIKeyEnv
	if apiKeyEnv == "" {
		return fmt.Errorf("provider %q is keyless; remove it instead", providerID)
	}
	if a.env == nil || !a.env.IsManaged(apiKeyEnv) {
		if os.Getenv(apiKeyEnv) != "" {
			return fmt.Errorf("provider %q is connected via environment; unset %s outside Lightcode", providerID, apiKeyEnv)
		}
		return nil
	}
	if err := a.removeManagedKeyLocked(apiKeyEnv); err != nil {
		return err
	}
	return a.reloadLockedNoRefresh()
}

// RemoveProvider deletes a custom provider entry from config.json.
func (a *Agent) RemoveProvider(providerID string) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().sessionLocked().busy {
		return fmt.Errorf("cannot remove provider while a turn is running")
	}
	prov := a.catalog.Providers[providerID]
	if prov == nil {
		return fmt.Errorf("provider %q not found", providerID)
	}
	if prov.Builtin {
		return fmt.Errorf("cannot remove bundled provider %q", providerID)
	}
	if providerConnected(prov) && prov.Transport.APIKeyEnv != "" {
		return fmt.Errorf("disconnect provider %q before removing it", providerID)
	}
	if err := a.mutateConfigRootLocked(func(root map[string]any) error {
		providers, err := providerRootMap(root)
		if err != nil {
			return err
		}
		delete(providers, providerID)
		return nil
	}); err != nil {
		return err
	}
	return a.reloadLockedNoRefresh()
}

func (a *Agent) apiKeyEnvInUseLocked(apiKeyEnv, providerID string) bool {
	for id, prov := range a.catalog.Providers {
		if id == providerID || prov == nil {
			continue
		}
		if prov.Transport.APIKeyEnv == apiKeyEnv {
			return true
		}
	}
	return false
}

func validateCustomHeaders(headers map[string]string) error {
	for name := range headers {
		if strings.EqualFold(strings.TrimSpace(name), "authorization") || strings.EqualFold(strings.TrimSpace(name), "proxy-authorization") {
			return fmt.Errorf("custom transport header %q is not allowed; use api_key_env instead", name)
		}
	}
	return nil
}

func customTransportMap(baseURL, apiKeyEnv string, headers map[string]string, options map[string]any) map[string]any {
	transport := map[string]any{"base_url": baseURL, "api_key_env": apiKeyEnv}
	if len(headers) != 0 {
		transport["headers"] = headers
	}
	if len(options) != 0 {
		transport["options"] = options
	}
	return transport
}

func (a *Agent) deleteProviderConfigLocked(providerID string) error {
	return a.mutateConfigRootLocked(func(root map[string]any) error {
		providers, err := providerRootMap(root)
		if err != nil {
			return err
		}
		delete(providers, providerID)
		return nil
	})
}

func (a *Agent) mutateConfigRootLocked(mutate func(root map[string]any) error) error {
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		return err
	}
	var root map[string]any
	if err := decodeJSONUseNumber(data, &root); err != nil {
		return fmt.Errorf("parse config %s: %w", a.configPath, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	if err := mutate(root); err != nil {
		return err
	}
	return a.writeAgentConfigLocked(root)
}

func providerRootMap(root map[string]any) (map[string]any, error) {
	providersRaw, ok := root["providers"]
	if !ok {
		providers := map[string]any{}
		root["providers"] = providers
		return providers, nil
	}
	providers, ok := providersRaw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("providers must be an object")
	}
	return providers, nil
}
