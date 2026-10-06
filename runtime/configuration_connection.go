package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/protocol"
)

// This file owns the connection operators: the write-only connect and
// disconnect of existing providers, the custom-provider discovery read, and
// the provider-model discovery read. The two writes route through the one
// configuration mutation path — the connect's needed discovery is fetched
// outside every lock, revalidated against the mutation's captured published
// identity inside the build hold, and persisted only after the ready
// candidate validates; the discovery reads fetch under the retained
// 30-second timeout and write nothing. No key value ever reaches a status,
// error, observation, or log.

// connectDiscoveryTimeout bounds connect-time and discovery-read HTTP calls
// so a stalled endpoint cannot hold an admitted call indefinitely.
const connectDiscoveryTimeout = 30 * time.Second

// connectHTTPClient is the shared timeout-bounded discovery client. It
// deliberately replaces http.DefaultClient, which has no timeout.
var connectHTTPClient = &http.Client{Timeout: connectDiscoveryTimeout}

// connectProvider connects an existing published provider through the one
// mutation path. Phase 1 — under no lock, against the immutable published
// snapshot — refuses an unusable provider whose discovery is disabled before
// any credential resolution, captures the provider's transport identity, and
// resolves the transient credential through the one shared rule for both
// credential shapes: a keyless provider carries no credential (no supplied
// key means no action, a supplied key refuses), a supplied key on an
// externally defined variable refuses before any side effect, a managed key
// is used or updated through TrySet, and a missing required key fails. An
// already-usable provider is never probed with a network fetch; an empty
// discovery-backed provider fetches its candidates with the original
// transport plus a transient Authorization copy. Phase 2 — back inside the
// mutation's build hold — revalidates the live identity, requires a usable
// ready candidate before any side effect, then persists the accepted
// discovery and the managed key in that order before the shared commit. A
// repeat connection is a real publication: the ready next generation always
// consumes and publishes.
func (r *Runtime) connectProvider(ctx context.Context, providerID string, optionalKey *string) (protocol.ProviderMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	defer release()
	captured, err := r.config.capture(ctx)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	prov, err := capturedProvider(captured.snapshot, providerID)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	transport := prov.Transport
	envName := transport.APIKeyEnv

	plan := &connectionEffects{providerID: providerID, transport: transport}
	usable := usableModelCount(prov) > 0
	// One rule for both credential shapes: an unusable provider is
	// connectable only through discovery, and disabled discovery refuses
	// before any key resolution or fetch.
	if !usable && !prov.Discovery {
		return protocol.ProviderMutation{}, configurationFailure(fmt.Errorf("provider %q has no usable models and discovery is disabled", prov.ID))
	}
	var fetchKey string
	// Both credential shapes resolve through the one shared rule over the
	// capture's own observation, before any effect: a keyless provider
	// carries no credential (a supplied key refuses), and every keyed
	// provider — usable or not — resolves its binding. A usable provider
	// still runs no discovery and no key probe.
	key, persist, err := resolveConnectKey(envName, optionalKey, captured.credentials[envName])
	if err != nil {
		return protocol.ProviderMutation{}, configurationFailure(err)
	}
	if persist {
		plan.keyAction, plan.keyEnv, plan.keyValue = keyActionSet, envName, key
	}
	if !usable {
		fetchKey = key
	}
	if !usable {
		discovered, err := connectFetchDiscovery(ctx, prov.ID, transport, fetchKey)
		if err != nil {
			return protocol.ProviderMutation{}, err
		}
		plan.discovered = &discovered
	}
	result, err := r.config.mutate(ctx, r.config.editProviderConnect(plan))
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	return protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(result.snapshot),
		Result:                providerPostState(result, prov.ID),
	}, nil
}

// disconnectProvider disconnects one existing published provider through the
// same ready-publication path while the configuration bytes stay untouched.
// Actual ownership is resolved inside the build hold: a managed key is
// removed through TryRemove, an external set key refuses with the env name
// alone, a keyless provider refuses, and an absent unmanaged key publishes
// the successful disconnected status without touching anything. There is no
// busy-turn refusal: disconnects no longer gate on running work.
func (r *Runtime) disconnectProvider(ctx context.Context, providerID string) (protocol.ProviderMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	defer release()
	plan := &connectionEffects{providerID: providerID}
	captured, err := r.config.mutate(ctx, r.config.editProviderDisconnect(providerID, plan))
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	return protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(captured.snapshot),
		Result:                providerPostState(captured, providerID),
	}, nil
}

// discoverProvider is the custom-provider discovery read: a pure network
// read over the request's transport shape — no cache write, no attempt
// marker, no env write, no configuration write — returning the sorted
// candidates. The credential is the one read rule: a supplied nonempty
// api_key binds its exact value to this read's transient fetch and is
// never substituted — even when the referenced env name is externally
// owned — while an absent or empty supplied key uses the referenced
// owner's captured value, where named-unset and named-defined-empty both
// mean no credential and fetch unauthenticated, matching saved-provider
// discovery. Nothing is persisted or published in any case.
func (r *Runtime) discoverProvider(ctx context.Context, req protocol.DiscoveryRequest) ([]protocol.DiscoveredModelCandidate, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	baseURL := strings.TrimSpace(req.BaseUrl)
	if baseURL == "" {
		return nil, invalidEdit("base_url is required")
	}
	headers := map[string]string(nil)
	if req.Headers != nil {
		if err := refuseCredentialHeaders(*req.Headers); err != nil {
			return nil, err
		}
		headers = *req.Headers
	}
	key := ""
	if req.ApiKey != nil && *req.ApiKey != "" {
		key = *req.ApiKey
	} else if envName := strings.TrimSpace(req.ApiKeyEnv); envName != "" {
		key = r.managedEnv.Capture([]string{envName})[envName].Value
	}
	discovered, err := connectFetchDiscovery(ctx, "custom", catalog.Transport{BaseURL: baseURL, Headers: headers}, key)
	if err != nil {
		return nil, err
	}
	return projectDiscoveredCandidates(discovered.Models, nil), nil
}

// discoverProviderModels is one published provider's model-discovery read:
// the capture's own transport and credential sample bind one transient fetch
// transport before HTTP, nothing is written, and the returned sorted
// candidates exclude the provider's already-included usable model IDs.
func (r *Runtime) discoverProviderModels(ctx context.Context, providerID string) ([]protocol.DiscoveredModelCandidate, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	captured, err := r.config.capture(ctx)
	if err != nil {
		return nil, err
	}
	prov, err := capturedProvider(captured.snapshot, providerID)
	if err != nil {
		return nil, err
	}
	key := ""
	if env := prov.Transport.APIKeyEnv; env != "" {
		key = captured.credentials[env].Value
	}
	discovered, err := connectFetchDiscovery(ctx, prov.ID, prov.Transport, key)
	if err != nil {
		return nil, err
	}
	return projectDiscoveredCandidates(discovered.Models, func(id string) bool {
		if existing := prov.Models[id]; existing != nil && existing.ContextWindow > 0 {
			return true
		}
		return false
	}), nil
}

// editProviderConnect builds the existing-provider connect edit: it owns no
// file and always edits — a successful repeat connection consumes a
// generation under the one shared successful-edit rule. Its check revalidates
// the phase-1 identity against the live publication, then requires the ready
// candidate to be usable before any side effect: a connect that would
// publish an unusable provider — a latest raw layer that removed the only
// model window included — refuses even when no discovery was fetched.
func (s *configurationService) editProviderConnect(plan *connectionEffects) configurationEdit {
	return configurationEdit{
		apply: func(rawRoots, configurationCapture) (editedFile, error) {
			return editConnection, nil
		},
		connection: plan,
		check: func(c *configuration, captured configurationCapture) error {
			if err := checkConnectionCandidate(captured.snapshot, plan, c); err != nil {
				return err
			}
			if cand := c.catalog.Providers[plan.providerID]; usableModelCount(cand) == 0 {
				return fmt.Errorf("provider %q has no usable models after the edit", plan.providerID)
			}
			return nil
		},
	}
}

// editProviderDisconnect builds the existing-provider disconnect edit: the
// whole resolution runs inside the build hold — there is no out-of-lock
// work to gate — filling the plan's key action from the captured published
// provider's actual ownership. Disconnect owns no file and publishes the
// latest raw layer, so its check only proves the touched provider still
// exists in the ready candidate; the published binding, not the latest raw
// transport, decides which key is removed.
func (s *configurationService) editProviderDisconnect(providerID string, plan *connectionEffects) configurationEdit {
	return configurationEdit{
		apply: func(_ rawRoots, captured configurationCapture) (editedFile, error) {
			prov, err := capturedProvider(captured.snapshot, providerID)
			if err != nil {
				return 0, err
			}
			envName := prov.Transport.APIKeyEnv
			if envName == "" {
				return 0, invalidEdit("provider %q is keyless; remove it instead", providerID)
			}
			plan.providerID = providerID
			observed := captured.credentials[envName]
			if observed.Managed {
				plan.keyAction, plan.keyEnv = keyActionRemove, envName
			} else if observed.Value != "" {
				return 0, configurationFailure(fmt.Errorf("provider %q is connected via environment; unset %s outside Lightcode", providerID, envName))
			}
			// An absent unmanaged key: nothing to remove; the disconnect
			// still publishes the ready next generation.
			return editConnection, nil
		},
		connection: plan,
		check: func(c *configuration, _ configurationCapture) error {
			return requireCandidateProvider(c, providerID)
		},
	}
}

// checkConnectionCandidate revalidates the connect against the mutation's
// captured published identity and the ready candidate, before any side
// effect: the provider must still exist, and its captured and candidate
// transports must both match the phase-1 identity under catalog identity — so
// a latest raw layer that dropped the provider or changed the transport
// cannot be published as a successful connection. It is connect-only;
// disconnect resolves its key action from the captured binding and proves
// candidate presence alone.
func checkConnectionCandidate(live *configuration, plan *connectionEffects, candidate *configuration) error {
	liveProvider := live.catalog.Providers[plan.providerID]
	if liveProvider == nil {
		return fmt.Errorf("provider %q not found", plan.providerID)
	}
	if !catalog.SameTransport(plan.transport, liveProvider.Transport) {
		return fmt.Errorf("provider %s changed while connecting; retry", plan.providerID)
	}
	cand := candidate.catalog.Providers[plan.providerID]
	if cand == nil {
		return fmt.Errorf("provider %q is not valid after the edit", plan.providerID)
	}
	if !catalog.SameTransport(plan.transport, cand.Transport) {
		return fmt.Errorf("provider %s changed in the latest configuration while connecting; retry", plan.providerID)
	}
	return nil
}

// resolveConnectKey applies the one credential-input rule over one captured
// environment observation, for both credential shapes: a keyless binding
// carries no credential — no supplied key means no action, and a supplied
// key has no named variable to persist to, so it is refused before any side
// effect. A named binding with a supplied nonempty key is applied only
// through the managed owner's ordinary writable-name rule: an explicitly
// named externally defined variable refuses before any side effect, leaving
// its value intact — it is never silently substituted for the supplied key.
// Without a supplied credential the captured configured binding decides: a
// managed or external defined value is used as-is, a defined-but-empty
// external value fails uniformly, and an absent key fails the required
// credential.
func resolveConnectKey(envName string, optionalKey *string, observed config.EnvValue) (key string, persist bool, err error) {
	if envName == "" {
		if optionalKey != nil && *optionalKey != "" {
			return "", false, fmt.Errorf("provider is keyless; a supplied key has no named variable to persist to")
		}
		return "", false, nil
	}
	if optionalKey != nil && *optionalKey != "" {
		if observed.Defined && !observed.Managed {
			return "", false, fmt.Errorf("env var %s is set externally; a supplied key cannot be applied to it", envName)
		}
		return *optionalKey, true, nil
	}
	if observed.Managed {
		if observed.Value == "" {
			return "", false, fmt.Errorf("provider requires API key env var %s", envName)
		}
		return observed.Value, false, nil
	}
	if observed.Defined {
		if observed.Value == "" {
			return "", false, fmt.Errorf("provider env var %s is externally set but empty", envName)
		}
		return observed.Value, false, nil
	}
	return "", false, fmt.Errorf("provider requires API key env var %s", envName)
}

// connectFetchDiscovery performs one /models fetch for the provider identity
// under the retained 30-second timeout. The transport is the caller's
// original configured transport; the credential rides a transient
// Authorization copy alone — the fetch copy's api_key_env is cleared so no
// binding is consulted or changed, every case-insensitive Authorization
// spelling configured on the transport is removed from the copy before the
// canonical header is set (the captured transport itself is never mutated),
// and no cache, attempt marker, env, or configuration write ever follows. A
// canceled caller keeps its unwrapped context error.
func connectFetchDiscovery(ctx context.Context, providerID string, transport catalog.Transport, key string) (catalog.DiscoveredProvider, error) {
	provisional := transport
	provisional.APIKeyEnv = ""
	provisional.Headers = cloneTransportHeaders(transport.Headers)
	if key != "" {
		for name := range provisional.Headers {
			if strings.EqualFold(name, "Authorization") {
				delete(provisional.Headers, name)
			}
		}
		provisional.Headers["Authorization"] = "Bearer " + key
	}
	return discoveryFetch(ctx, &catalog.Provider{ID: providerID, Transport: provisional})
}

// discoveryFetch performs one /models fetch under the retained 30-second
// timeout and maps the outcome: a canceled caller keeps its unwrapped
// context error, everything else is the typed configuration failure. No
// cache, attempt marker, env, or configuration write ever follows.
func discoveryFetch(ctx context.Context, prov *catalog.Provider) (catalog.DiscoveredProvider, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, connectDiscoveryTimeout)
	defer cancel()
	discovered, err := catalog.FetchDiscovery(fetchCtx, connectHTTPClient, prov)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return catalog.DiscoveredProvider{}, err
		}
		return catalog.DiscoveredProvider{}, configurationFailure(err)
	}
	return discovered, nil
}

// cloneTransportHeaders copies one header map, nil-safe, so the transient
// Authorization never reaches the caller's captured headers.
func cloneTransportHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for name, value := range in {
		out[name] = value
	}
	return out
}

// projectDiscoveredCandidates maps one fetched discovery onto the sorted
// generated candidates: Usable is the positive-window usability key and the
// cost is an owned copy. The skip predicate excludes already-included
// usable model IDs for the provider-model read; the custom read filters
// nothing.
func projectDiscoveredCandidates(models map[string]catalog.DiscoveredModel, skip func(id string) bool) []protocol.DiscoveredModelCandidate {
	ids := make([]string, 0, len(models))
	for id := range models {
		if skip != nil && skip(id) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]protocol.DiscoveredModelCandidate, 0, len(ids))
	for _, id := range ids {
		m := models[id]
		out = append(out, protocol.DiscoveredModelCandidate{
			Id:              id,
			Name:            m.Name,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens,
			Cost:            projectCost(m.Cost),
			Usable:          m.ContextWindow > 0,
		})
	}
	return out
}
