package runtime

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// This file owns the configuration/model/provider/warning read projections:
// every read is one admitted call capturing the published configuration
// pointer exactly once and projecting from that snapshot alone — no handler
// rereads a configuration file, and no secret, private prompt, or
// credential value is returned.

// getConfiguration projects the complete published configuration view: the
// effective settings beside the raw sections, the Agent roster, and the
// provider views with their source and key-source labels.
func (r *Runtime) getConfiguration(ctx context.Context) (protocol.ConfigurationView, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ConfigurationView{}, err
	}
	defer release()
	captured := r.config.current()
	return protocol.ConfigurationView{
		ConfigurationRevision: configurationRevision(captured),
		Settings:              projectSettings(captured.sessions, captured.plugins),
		Agents:                projectAgents(captured),
		Providers:             projectProviders(captured, r.managedEnv),
	}, nil
}

// listModels projects the flat model picker: the visible (non-hidden) list
// or, with all, the complete catalog — both restricted to connected
// providers, the retained picker rule.
func (r *Runtime) listModels(ctx context.Context, params protocol.ListModelsParams) (protocol.ModelList, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ModelList{}, err
	}
	defer release()
	captured := r.config.current()
	refs := captured.catalog.VisibleModels()
	if params.All {
		refs = captured.catalog.AllModels()
	}
	models := make([]protocol.ModelListEntry, 0, len(refs))
	for _, ref := range refs {
		// The refs enumerate this same immutable catalog's verified provider
		// and model maps, so the lookup cannot fail; nothing is swallowed.
		prov, entry, _ := captured.catalog.LookupOrIncomplete(ref)
		if !catalog.ProviderConnected(prov, liveEnvIsSet) {
			continue
		}
		models = append(models, projectModelListEntry(prov, entry))
	}
	return protocol.ModelList{
		ConfigurationRevision: configurationRevision(captured),
		Models:                models,
	}, nil
}

// listProviders projects every effective provider view, sorted by ID — the
// shared projection producer with getConfiguration.
func (r *Runtime) listProviders(ctx context.Context) (protocol.ProviderList, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderList{}, err
	}
	defer release()
	captured := r.config.current()
	return protocol.ProviderList{
		ConfigurationRevision: configurationRevision(captured),
		Providers:             projectProviders(captured, r.managedEnv),
	}, nil
}

// getProvider projects one provider's complete view. An empty identity wraps
// the shared harness.ErrInvalid sentinel; a valid identity naming no
// effective provider returns the catalog's typed unknown-provider error.
func (r *Runtime) getProvider(ctx context.Context, providerID string) (protocol.ProviderDetail, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderDetail{}, err
	}
	defer release()
	captured := r.config.current()
	prov, err := capturedProvider(captured, providerID)
	if err != nil {
		return protocol.ProviderDetail{}, err
	}
	return protocol.ProviderDetail{
		ConfigurationRevision: configurationRevision(captured),
		Provider:              projectProvider(captured, r.managedEnv, prov),
	}, nil
}

// listProviderModels projects one provider's complete model views. The same
// identity rules as every per-provider read apply.
func (r *Runtime) listProviderModels(ctx context.Context, providerID string) (protocol.ProviderModelList, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderModelList{}, err
	}
	defer release()
	captured := r.config.current()
	prov, err := capturedProvider(captured, providerID)
	if err != nil {
		return protocol.ProviderModelList{}, err
	}
	models := projectModels(prov) // the projection is a fresh allocation; used directly
	return protocol.ProviderModelList{
		ConfigurationRevision: configurationRevision(captured),
		Models:                models,
	}, nil
}

// getWarnings is the unfiltered warning read: every group of the
// Runtime-owned presentation store, with the revision captured once.
func (r *Runtime) getWarnings(ctx context.Context) (protocol.WarningsSnapshot, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.WarningsSnapshot{}, err
	}
	defer release()
	revision, warnings := r.warnings.snapshot()
	return protocol.WarningsSnapshot{
		WarningsRevision: protocol.WarningsRevision{Revision: formatWarningRevision(revision)},
		Warnings:         warnings, // the shared read producer returns owned non-nil slices
	}, nil
}

// updateSettings replaces the complete settings shape through the one
// configuration mutation path: the whole sessions member and the whole
// plugins member are written as the generated target shape — each plugin
// document string becomes that plugin's owned raw section, an omitted
// section removing any newer one — and every successful edit atomically
// rewrites the owning main configuration and publishes the next generation,
// identical bytes and absent-member resets included. Unowned top-level root
// members are untouched. The result projects the returned candidate, never a
// second current() load that could see a later writer.
func (r *Runtime) updateSettings(ctx context.Context, settings protocol.Settings) (protocol.SettingsMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.SettingsMutation{}, err
	}
	defer release()
	candidate, err := r.config.mutate(ctx, editSettings(settings))
	if err != nil {
		return protocol.SettingsMutation{}, err
	}
	return protocol.SettingsMutation{
		ConfigurationRevision: configurationRevision(candidate),
		Result:                projectSettings(candidate.sessions, candidate.plugins),
	}, nil
}

// setAgentTypeModel edits one known Agent type's user model override through
// the same mutation path: a nonempty ref replaces it, an empty string clears
// it. Every successful request — set, clear, absent override, identical
// bytes — writes the owning agents file and publishes the next generation;
// the no-override exception belongs to the retained public field reset
// operator, which this client is not. An unknown or dropped type fails
// invalid and writes nothing; the result projects from the returned
// candidate.
func (r *Runtime) setAgentTypeModel(ctx context.Context, agentType, modelRef string) (protocol.AgentMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.AgentMutation{}, err
	}
	defer release()
	candidate, err := r.config.mutate(ctx, r.config.editAgentModel(agentType, modelRef))
	if err != nil {
		return protocol.AgentMutation{}, err
	}
	mutation := protocol.AgentMutation{ConfigurationRevision: configurationRevision(candidate)}
	for _, agent := range projectAgents(candidate) {
		if agent.Name == agentType {
			mutation.Result = agent
			break
		}
	}
	// The edit's known-type check already guarantees the requested type is a
	// member of the returned candidate's roster, so Result is always set on
	// every supported request.
	return mutation, nil
}

// The provider/model metadata mutations: every operator is one admitted call
// through the one configuration mutation path, projects its result from the
// returned candidate only — never a second current() load — and has no
// fallible step after the publication. Visibility edits ride the patches'
// Hidden pointers; there is no second setter family and no secret parameter.

// addProvider creates one custom provider with its models through the one
// mutation path: a new trimmed nonempty no-slash ID, a required base URL,
// normalized unique model IDs, and at least one usable model. The optional
// write-only key is never written to the raw layer: a missing api_key_env
// member with a supplied key generates the retained unique env name, the
// candidate validates completely before any write, and the managed key is
// persisted only after the owning file — a failure there restores the exact
// prior bytes. The candidate check proves the created subject survived the
// shared catalog validation before the owning write.
func (r *Runtime) addProvider(ctx context.Context, providerID string, patch protocol.ProviderEdit, models map[string]protocol.ModelEdit, key *string) (protocol.ProviderMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	defer release()
	candidate, err := r.config.mutate(ctx, r.config.editProviderCreate(providerID, patch, models, key))
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	// The edit's candidate check already proved the created provider is a
	// member of this candidate's catalog.
	prov := candidate.catalog.Providers[strings.TrimSpace(providerID)]
	return protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(candidate),
		Result:                projectProvider(candidate, r.managedEnv, prov),
	}, nil
}

// updateProvider patches one known provider's writable members through the
// same mutation path: builtin locks, the connected-provider api_key_env
// prohibition and the wholesale headers rule are the edit's own prewrite
// checks, and the result projects the returned candidate's effective view.
func (r *Runtime) updateProvider(ctx context.Context, providerID string, patch protocol.ProviderEdit) (protocol.ProviderMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	defer release()
	candidate, err := r.config.mutate(ctx, r.config.editProviderUpdate(providerID, patch))
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	// The edit's candidate check already proved the patched provider is a
	// member of this candidate's catalog.
	prov := candidate.catalog.Providers[providerID]
	return protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(candidate),
		Result:                projectProvider(candidate, r.managedEnv, prov),
	}, nil
}

// deleteProvider removes one custom provider's owning user definition. The
// mutation's result is the generated deletion envelope: no post-state view,
// an explicit null result.
func (r *Runtime) deleteProvider(ctx context.Context, providerID string) (protocol.DeletionMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.DeletionMutation{}, err
	}
	defer release()
	candidate, err := r.config.mutate(ctx, r.config.editProviderDelete(providerID))
	if err != nil {
		return protocol.DeletionMutation{}, err
	}
	return protocol.DeletionMutation{
		ConfigurationRevision: configurationRevision(candidate),
		Result:                nil,
	}, nil
}

// resetProviderField removes exactly one provider user-layer override — or
// no-ops on an absent override, returning the unchanged projection and the
// current revision with no file write, generation, or event.
func (r *Runtime) resetProviderField(ctx context.Context, providerID string, field protocol.ResetProviderFieldParamsField) (protocol.ProviderMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	defer release()
	candidate, err := r.config.mutate(ctx, r.config.editProviderFieldReset(providerID, field))
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	// The edit's candidate check already proved the reset provider is a
	// member of this candidate's catalog.
	prov := candidate.catalog.Providers[providerID]
	return protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(candidate),
		Result:                projectProvider(candidate, r.managedEnv, prov),
	}, nil
}

// saveModel upserts one model's user-layer fields under a known provider —
// the same PUT as the retained SaveModel, so a missing model creates a user
// model — through the same mutation path.
func (r *Runtime) saveModel(ctx context.Context, providerID, modelID string, patch protocol.ModelEdit) (protocol.ModelMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ModelMutation{}, err
	}
	defer release()
	candidate, err := r.config.mutate(ctx, r.config.editModelSave(providerID, modelID, patch))
	if err != nil {
		return protocol.ModelMutation{}, err
	}
	// The edit's candidate check already proved the saved model is a member
	// of this candidate's catalog.
	prov := candidate.catalog.Providers[providerID]
	return protocol.ModelMutation{
		ConfigurationRevision: configurationRevision(candidate),
		Result:                projectModelView(prov, prov.Models[modelID]),
	}, nil
}

// deleteModel removes one user model's owning definition; the result is the
// generated deletion envelope with its explicit null.
func (r *Runtime) deleteModel(ctx context.Context, providerID, modelID string) (protocol.DeletionMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.DeletionMutation{}, err
	}
	defer release()
	candidate, err := r.config.mutate(ctx, r.config.editModelDelete(providerID, modelID))
	if err != nil {
		return protocol.DeletionMutation{}, err
	}
	return protocol.DeletionMutation{
		ConfigurationRevision: configurationRevision(candidate),
		Result:                nil,
	}, nil
}

// resetModelField removes exactly one model user-layer override — or no-ops
// on an absent override, returning the unchanged projection and the current
// revision with no file write, generation, or event.
func (r *Runtime) resetModelField(ctx context.Context, providerID, modelID string, field protocol.ResetProviderModelFieldParamsField) (protocol.ModelMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ModelMutation{}, err
	}
	defer release()
	candidate, err := r.config.mutate(ctx, r.config.editModelFieldReset(providerID, modelID, field))
	if err != nil {
		return protocol.ModelMutation{}, err
	}
	// The edit's candidate check already proved the reset model is a member
	// of this candidate's catalog.
	prov := candidate.catalog.Providers[providerID]
	return protocol.ModelMutation{
		ConfigurationRevision: configurationRevision(candidate),
		Result:                projectModelView(prov, prov.Models[modelID]),
	}, nil
}

// capturedProvider resolves one provider ID against the captured catalog:
// an empty identity wraps the shared invalid sentinel and an unknown valid
// one returns the catalog's typed unknown-provider error.
func capturedProvider(captured *configuration, providerID string) (*catalog.Provider, error) {
	if providerID == "" {
		return nil, fmt.Errorf("provider id must be non-empty: %w", harness.ErrInvalid)
	}
	prov, ok := captured.catalog.Providers[providerID]
	if !ok || prov == nil {
		return nil, fmt.Errorf("provider %q: %w", providerID, catalog.ErrUnknownProvider)
	}
	return prov, nil
}

// liveEnvIsSet is the shared live connection sample: LoadDotEnv already ran
// at startup, so a plain non-empty Getenv covers both shell-exported and
// .env-managed keys. It is presentation only.
func liveEnvIsSet(name string) bool {
	return os.Getenv(name) != ""
}

// projectProviders projects every effective provider view, sorted by ID.
func projectProviders(captured *configuration, env *config.ManagedEnv) []protocol.Provider {
	ids := make([]string, 0, len(captured.catalog.Providers))
	for id := range captured.catalog.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]protocol.Provider, 0, len(ids))
	for _, id := range ids {
		out = append(out, projectProvider(captured, env, captured.catalog.Providers[id]))
	}
	return out
}

// projectProvider maps one effective catalog provider onto its generated
// view: the effective and user headers projected separately under the
// no-authorization response contract, the key-source classification from the
// live environment and the retained managed set, and the connection,
// disconnect, removal, and connect-readiness labels. These are labels, never
// values: no key value appears.
func projectProvider(captured *configuration, env *config.ManagedEnv, prov *catalog.Provider) protocol.Provider {
	connected := catalog.ProviderConnected(prov, liveEnvIsSet)
	keySource := classifyKeySource(prov.Transport.APIKeyEnv, env)
	generated := generatedAPIKeyEnvName(prov.ID, captured.catalog)
	return protocol.Provider{
		ApiKeyEnv:        prov.Transport.APIKeyEnv,
		BaseUrl:          prov.Transport.BaseURL,
		Builtin:          prov.Builtin,
		Connectable:      prov.Transport.BaseURL != "" && (usableModelCount(prov) > 0 || prov.Discovery),
		Connected:        connected,
		Disconnectable:   keySource == config.KeySourceManaged,
		Discovery:        prov.Discovery,
		ExtraBody:        jsonMapPointer(prov.ExtraBody),
		GeneratedKeyEnv:  &generated,
		Headers:          stripCredentialHeaders(prov.Transport.Headers),
		Hidden:           prov.Hidden,
		Id:               prov.ID,
		KeySource:        protocol.ProviderKeySource(keySource),
		MaxTokensField:   prov.MaxTokensField,
		Models:           projectModels(prov),
		Name:             prov.Name,
		Options:          jsonMapPointer(prov.Transport.Options),
		Removable:        !prov.Builtin && (keySource == config.KeySourceKeyless || !connected),
		SystemRole:       protocol.SystemRole(prov.SystemRole),
		UsageInStream:    prov.UsageInStream,
		UserHeaders:      captured.userHeaders(prov.ID, prov.Builtin),
		ProtocolMetadata: projectProtocolMetadata(prov.ProtocolMetadata),
	}
}

// userHeaders extracts one provider's user-layer transport headers from the
// retained decoded user layer — the additions on top of any bundled headers.
// For a builtin, bundled attribution keys are stripped by the retained rule
// so they never appear as editable, and the user file itself is never edited.
func (c *configuration) userHeaders(providerID string, builtin bool) map[string]string {
	prov, ok := c.userProviders[providerID].(map[string]any)
	if !ok {
		return map[string]string{}
	}
	transport, _ := prov["transport"].(map[string]any)
	headersRaw, _ := transport["headers"].(map[string]any)
	headers := make(map[string]string, len(headersRaw))
	for name, value := range headersRaw {
		if s, ok := value.(string); ok {
			headers[name] = s
		}
	}
	if builtin {
		headers = stripBundledHeaderKeys(headers, providerID)
	}
	return stripCredentialHeaders(headers)
}

// projectModels maps one provider's effective models onto their generated
// views sorted by model ID, each labeled from the captured build's source
// labels.
func projectModels(prov *catalog.Provider) []protocol.ModelView {
	ids := make([]string, 0, len(prov.Models))
	for id := range prov.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]protocol.ModelView, 0, len(ids))
	for _, id := range ids {
		out = append(out, projectModelView(prov, prov.Models[id]))
	}
	return out
}

// projectModelView maps one effective model entry: its Build-stamped source
// label, the usable/incomplete and provider-hidden flags, and the owned
// cost/metadata pointers.
func projectModelView(prov *catalog.Provider, entry *catalog.Model) protocol.ModelView {
	_, incomplete := entry.Incomplete()
	view := protocol.ModelView{
		ContextWindow:    entry.ContextWindow,
		Cost:             projectCost(entry.Cost),
		ExtraBody:        jsonMapPointer(entry.ExtraBody),
		Hidden:           entry.Hidden,
		Id:               entry.ID,
		Incomplete:       incomplete,
		InputModalities:  projectModalities(entry.InputModalities),
		MaxOutputTokens:  entry.MaxOutputTokens,
		Name:             entry.Name,
		ProviderHidden:   prov.Hidden,
		Source:           protocol.ModelSource(entry.Source),
		SystemRole:       protocol.SystemRole(entry.SystemRole),
		Usable:           entry.ContextWindow > 0,
		UsageInStream:    entry.UsageInStream,
		ProtocolMetadata: projectProtocolMetadata(entry.ProtocolMetadata),
	}
	return view
}

// projectModelListEntry maps one flat picker entry: the display fallbacks and
// the Build-stamped source label.
func projectModelListEntry(prov *catalog.Provider, entry *catalog.Model) protocol.ModelListEntry {
	displayName := entry.Name
	if displayName == "" {
		displayName = entry.ID
	}
	providerName := prov.Name
	if providerName == "" {
		providerName = prov.ID
	}
	_, incomplete := entry.Incomplete()
	return protocol.ModelListEntry{
		Ref:             model.ModelRef{Provider: prov.ID, Model: entry.ID}.String(),
		Provider:        prov.ID,
		ProviderName:    providerName,
		Model:           entry.ID,
		DisplayName:     displayName,
		ContextWindow:   entry.ContextWindow,
		MaxOutputTokens: entry.MaxOutputTokens,
		Cost:            projectCost(entry.Cost),
		Hidden:          entry.Hidden,
		ProviderHidden:  prov.Hidden,
		Incomplete:      incomplete,
		Source:          protocol.ModelSource(entry.Source),
	}
}

// projectAgents maps the captured definitions onto the roster view through
// the existing agentTypes() projection — the one selected-model and
// write-dir normalization producer admission already uses. Required array
// members are owned non-nil lists even when the definition carries none, and
// no private definition field (system prompt, LSP flag) is exposed.
func projectAgents(captured *configuration) []protocol.Agent {
	out := make([]protocol.Agent, 0, len(captured.definitions))
	for _, at := range captured.agentTypes() {
		out = append(out, protocol.Agent{
			Name:         at.Name,
			Description:  at.Description,
			Subagent:     at.Subagent,
			Model:        modelRefString(at.Model),
			Readonly:     at.Readonly,
			WriteDir:     at.WriteDir,
			Tools:        ownStrings(at.Tools),
			Capabilities: ownStrings(at.Capabilities),
		})
	}
	return out
}

// ownStrings copies one string list into an owned, non-nil array: a present
// collection serializes as [], never null.
func ownStrings(values []string) []string {
	out := make([]string, len(values))
	copy(out, values)
	return out
}

// modelRefString renders one resolved model ref in the provider/model wire
// spelling; a modelless definition stays empty.
func modelRefString(ref model.ModelRef) string {
	if ref.IsZero() {
		return ""
	}
	return ref.String()
}

// projectCost copies one catalog cost into its generated pointer shape with
// owned floats; an absent cost stays omitted.
func projectCost(cost *catalog.Cost) *protocol.Cost {
	if cost == nil {
		return nil
	}
	out := &protocol.Cost{}
	if cost.Input != nil {
		value := *cost.Input
		out.Input = &value
	}
	if cost.Output != nil {
		value := *cost.Output
		out.Output = &value
	}
	if cost.CacheRead != nil {
		value := *cost.CacheRead
		out.CacheRead = &value
	}
	if cost.CacheWrite != nil {
		value := *cost.CacheWrite
		out.CacheWrite = &value
	}
	return out
}

// projectModalities copies one modality list into its generated form.
func projectModalities(modalities []catalog.Modality) []protocol.InputModality {
	out := make([]protocol.InputModality, len(modalities))
	for i, modality := range modalities {
		out[i] = protocol.InputModality(modality)
	}
	return out
}

// projectProtocolMetadata copies optional protocol metadata with owned
// slices; absent metadata stays omitted.
func projectProtocolMetadata(meta *catalog.ProtocolMetadata) *protocol.ProtocolMetadata {
	if meta == nil {
		return nil
	}
	out := &protocol.ProtocolMetadata{}
	if meta.Family != "" {
		family := meta.Family
		out.Family = &family
	}
	if len(meta.MustPreserve) > 0 {
		preserved := append([]string(nil), meta.MustPreserve...)
		out.MustPreserve = &preserved
	}
	if len(meta.Drop) > 0 {
		drop := append([]string(nil), meta.Drop...)
		out.Drop = &drop
	}
	return out
}

// jsonMapPointer deep-clones one decoded JSON object into its generated
// pointer form through the catalog's shared clone (exact number lexemes are
// preserved); an absent or empty object stays omitted.
func jsonMapPointer(in map[string]any) *map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := catalog.CloneJSONValue(in).(map[string]any)
	return &out
}

// stripCredentialHeaders removes the credential header names from one
// projected header map: the no-authorization response contract forbids
// returning Authorization or Proxy-Authorization under any casing.
func stripCredentialHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		if credentialHeaderName(name) {
			continue
		}
		out[name] = value
	}
	return out
}

// classifyKeySource classifies one provider's credential through the shared
// classifier with the retained managed-set semantics: a shell-exported key
// beats .env, a key Lightcode loaded stays managed.
func classifyKeySource(apiKeyEnv string, env *config.ManagedEnv) string {
	managed := func(name string) bool { return env.IsManaged(name) }
	external := func(name string) bool { return os.Getenv(name) != "" && !managed(name) }
	return config.ClassifyKeySource(apiKeyEnv, external, managed)
}

// usableModelCount counts the provider's usable (positive-window) models.
func usableModelCount(prov *catalog.Provider) int {
	count := 0
	for _, model := range prov.Models {
		if model != nil && model.ContextWindow > 0 {
			count++
		}
	}
	return count
}

// generatedAPIKeyEnvName applies the retained name-generation rule over the
// captured catalog's api_key_env occupancy: LIGHTCODE_<UPPER_SNAKE_ID>_API_KEY
// (or the PROVIDER fallback for an unusable ID), suffixed 2+ when occupied.
func generatedAPIKeyEnvName(providerID string, cat *catalog.Catalog) string {
	base := "LIGHTCODE_" + upperSnake(providerID) + "_API_KEY"
	if base == "LIGHTCODE__API_KEY" {
		base = "LIGHTCODE_PROVIDER_API_KEY"
	}
	used := make(map[string]struct{})
	for _, prov := range cat.Providers {
		if prov != nil && prov.Transport.APIKeyEnv != "" {
			used[prov.Transport.APIKeyEnv] = struct{}{}
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

// upperSnake renders a provider ID as its upper snake-case form.
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
	return strings.Trim(b.String(), "_")
}
