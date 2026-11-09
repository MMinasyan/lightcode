package melta

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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

// GetConfiguration projects the complete published configuration view: the
// effective settings beside the raw sections, the Agent roster, and the
// provider views with their source and key-source labels.
func (r *Melta) GetConfiguration(ctx context.Context, c Caller) (protocol.ConfigurationView, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ConfigurationView{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ConfigurationView{}, err
	}
	captured, err := r.config.capture(ctx)
	if err != nil {
		return protocol.ConfigurationView{}, err
	}
	return protocol.ConfigurationView{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Settings:              projectSettings(captured.snapshot.sessions, captured.snapshot.tools, captured.snapshot.jobs, captured.snapshot.compaction, captured.snapshot.extensions),
		Agents:                projectAgents(captured.snapshot),
		Providers:             projectProviders(captured),
	}, nil
}

// ListModels projects the flat model picker: the visible (non-hidden) list
// or, with all, the complete catalog — both restricted to connected
// providers, the retained picker rule.
func (r *Melta) ListModels(ctx context.Context, c Caller, params protocol.ListModelsParams) (protocol.ModelList, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ModelList{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ModelList{}, err
	}
	captured, err := r.config.capture(ctx)
	if err != nil {
		return protocol.ModelList{}, err
	}
	refs := captured.snapshot.catalog.VisibleModels()
	if params.All {
		refs = captured.snapshot.catalog.AllModels()
	}
	models := make([]protocol.ModelListEntry, 0, len(refs))
	for _, ref := range refs {
		// The refs enumerate this same immutable catalog's verified provider
		// and model maps, so the lookup cannot fail; nothing is swallowed.
		prov, entry, _ := captured.snapshot.catalog.LookupOrIncomplete(ref)
		if !catalog.ProviderConnected(prov, capturedEnvIsSet(captured.snapshot.credentials)) {
			continue
		}
		models = append(models, projectModelListEntry(prov, entry))
	}
	return protocol.ModelList{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Models:                models,
	}, nil
}

// ListProviders projects every effective provider view, sorted by ID — the
// shared projection producer with GetConfiguration.
func (r *Melta) ListProviders(ctx context.Context, c Caller) (protocol.ProviderList, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderList{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ProviderList{}, err
	}
	captured, err := r.config.capture(ctx)
	if err != nil {
		return protocol.ProviderList{}, err
	}
	return protocol.ProviderList{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Providers:             projectProviders(captured),
	}, nil
}

// GetProvider projects one provider's complete view. An empty identity wraps
// the shared harness.ErrInvalid sentinel; a valid identity naming no
// effective provider returns the catalog's typed unknown-provider error.
func (r *Melta) GetProvider(ctx context.Context, c Caller, providerID string) (protocol.ProviderDetail, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderDetail{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ProviderDetail{}, err
	}
	captured, err := r.config.capture(ctx)
	if err != nil {
		return protocol.ProviderDetail{}, err
	}
	prov, err := capturedProvider(captured.snapshot, providerID)
	if err != nil {
		return protocol.ProviderDetail{}, err
	}
	return protocol.ProviderDetail{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Provider:              projectProvider(captured, prov),
	}, nil
}

// ListProviderModels projects one provider's complete model views. The same
// identity rules as every per-provider read apply.
func (r *Melta) ListProviderModels(ctx context.Context, c Caller, providerID string) (protocol.ProviderModelList, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderModelList{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ProviderModelList{}, err
	}
	captured, err := r.config.capture(ctx)
	if err != nil {
		return protocol.ProviderModelList{}, err
	}
	prov, err := capturedProvider(captured.snapshot, providerID)
	if err != nil {
		return protocol.ProviderModelList{}, err
	}
	models := projectModels(prov) // the projection is a fresh allocation; used directly
	return protocol.ProviderModelList{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Models:                models,
	}, nil
}

// GetWarnings is the unfiltered warning read: every group of the
// Melta-owned presentation store, with the revision captured once.
func (r *Melta) GetWarnings(ctx context.Context, c Caller) (protocol.WarningsSnapshot, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.WarningsSnapshot{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.WarningsSnapshot{}, err
	}
	revision, warnings := r.warnings.snapshot()
	return protocol.WarningsSnapshot{
		WarningsRevision: protocol.WarningsRevision{Revision: formatWarningRevision(revision), InstanceId: r.instance},
		Warnings:         warnings, // the shared read producer returns owned non-nil slices
	}, nil
}

// UpdateSettings is the one configuration settings/extension-edit writer: the
// request carries exactly one of the complete settings shape or one declared
// extension configuration edit. A settings write replaces the whole sessions
// member and the whole extensions member as the generated target shape — each
// extension document string becomes that extension's owned raw section, an
// omitted section removing any newer one — while a declared edit transforms
// only its own extension's latest raw section. Every successful edit
// atomically rewrites the owning main configuration and publishes the next
// generation, identical bytes and absent-member resets included. Unowned
// top-level root members are untouched. The result projects the returned
// candidate, never a second current() load that could see a later writer.
func (r *Melta) UpdateSettings(ctx context.Context, c Caller, request protocol.UpdateSettingsRequest) (protocol.SettingsMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.SettingsMutation{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.SettingsMutation{}, err
	}
	switch {
	case (request.Settings == nil) == (request.ExtensionEdit == nil):
		return protocol.SettingsMutation{}, fmt.Errorf("settings request requires exactly one of settings or extension_edit: %w", harness.ErrInvalid)
	}
	if request.Settings != nil {
		for _, section := range []struct {
			name string
			raw  json.RawMessage
		}{
			{"tools", request.Settings.Tools},
			{"jobs", request.Settings.Jobs},
			{"compaction", request.Settings.Compaction},
		} {
			if err := requireSettingsObject(section.name, section.raw); err != nil {
				return protocol.SettingsMutation{}, fmt.Errorf("%v: %w", err, harness.ErrInvalid)
			}
		}
	}
	var edit configurationEdit
	if request.ExtensionEdit != nil {
		edit = r.config.editExtension(ctx, request.ExtensionEdit.ExtensionId, request.ExtensionEdit.EditId, json.RawMessage(request.ExtensionEdit.Arguments))
	} else {
		edit = editSettings(*request.Settings)
	}
	captured, err := r.config.mutate(ctx, edit)
	if err != nil {
		return protocol.SettingsMutation{}, err
	}
	return protocol.SettingsMutation{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Result:                projectSettings(captured.snapshot.sessions, captured.snapshot.tools, captured.snapshot.jobs, captured.snapshot.compaction, captured.snapshot.extensions),
	}, nil
}

// The provider/model metadata mutations: every operator is one admitted call
// through the one configuration mutation path and projects its required
// result from the returned frozen capture — the mutated subject's effective
// post-state, or null when no subject remains — never a second current()
// load. Visibility edits ride the patches' Hidden pointers; there is no
// second setter family and no secret parameter.

// providerPostState projects one provider's effective post-state from the
// returned capture; a subject the candidate no longer contains is null.
func providerPostState(captured configurationCapture, providerID string) *protocol.Provider {
	prov := captured.snapshot.catalog.Providers[providerID]
	if prov == nil {
		return nil
	}
	view := projectProvider(captured, prov)
	return &view
}

// modelPostState projects one model's effective post-state from the returned
// capture; a subject the candidate no longer contains is null.
func modelPostState(captured configurationCapture, providerID, modelID string) *protocol.ModelView {
	prov := captured.snapshot.catalog.Providers[providerID]
	if prov == nil {
		return nil
	}
	entry := prov.Models[modelID]
	if entry == nil {
		return nil
	}
	view := projectModelView(prov, entry)
	return &view
}

// AddProvider creates one custom provider with its models through the one
// mutation path: a new trimmed nonempty no-slash ID and a required base URL;
// candidate validity, not an editor-only usable-model count, governs the
// creation. The optional write-only key is never written to the raw layer:
// the one credential-input rule applies before any side effect, the
// candidate validates completely before any write, and the managed key is
// persisted only after the owning file — a failure there restores the exact
// prior bytes. The result carries the created provider's actual binding.
func (r *Melta) AddProvider(ctx context.Context, c Caller, providerID string, patch protocol.ProviderEdit, models map[string]protocol.ModelEdit, key *string) (protocol.ProviderMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ProviderMutation{}, err
	}
	captured, err := r.config.mutate(ctx, r.config.editProviderCreate(ctx, providerID, patch, models, key))
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	// The edit's candidate check already proved the created provider is a
	// member of this candidate's catalog.
	return protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Result:                providerPostState(captured, strings.TrimSpace(providerID)),
	}, nil
}

// UpdateProvider patches one known provider's writable members through the
// same mutation path: no source-specific lock applies, the wholesale
// headers rule and the candidate check govern the edit, and the result
// projects the returned candidate's effective view.
func (r *Melta) UpdateProvider(ctx context.Context, c Caller, providerID string, patch protocol.ProviderEdit) (protocol.ProviderMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ProviderMutation{}, err
	}
	captured, err := r.config.mutate(ctx, r.config.editProviderUpdate(providerID, patch))
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	return protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Result:                providerPostState(captured, providerID),
	}, nil
}

// DeleteProvider removes one provider's owning user node through the same
// mutation path; the result carries the surviving effective post-state, or
// null when the candidate no longer contains the subject.
func (r *Melta) DeleteProvider(ctx context.Context, c Caller, providerID string) (protocol.ProviderMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ProviderMutation{}, err
	}
	captured, err := r.config.mutate(ctx, r.config.editProviderDelete(providerID))
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	return protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Result:                providerPostState(captured, providerID),
	}, nil
}

// ResetProviderField removes exactly one provider user-layer override under
// the shared field enum — an absent override publishes the same successful
// edit — and projects the effective post-state from the returned candidate.
func (r *Melta) ResetProviderField(ctx context.Context, c Caller, providerID string, field protocol.ProviderField) (protocol.ProviderMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ProviderMutation{}, err
	}
	captured, err := r.config.mutate(ctx, r.config.editProviderFieldReset(providerID, field))
	if err != nil {
		return protocol.ProviderMutation{}, err
	}
	return protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Result:                providerPostState(captured, providerID),
	}, nil
}

// SaveModel upserts one model's user-layer fields under a known provider —
// the same PUT as the retained SaveModel, so a missing model creates a user
// model — through the same mutation path.
func (r *Melta) SaveModel(ctx context.Context, c Caller, providerID, modelID string, patch protocol.ModelEdit) (protocol.ModelMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ModelMutation{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ModelMutation{}, err
	}
	captured, err := r.config.mutate(ctx, r.config.editModelSave(providerID, modelID, patch))
	if err != nil {
		return protocol.ModelMutation{}, err
	}
	return protocol.ModelMutation{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Result:                modelPostState(captured, providerID, modelID),
	}, nil
}

// DeleteModel removes one model's owning user node through the same mutation
// path; the result carries the surviving effective post-state, or null when
// the candidate no longer contains the subject.
func (r *Melta) DeleteModel(ctx context.Context, c Caller, providerID, modelID string) (protocol.ModelMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ModelMutation{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ModelMutation{}, err
	}
	captured, err := r.config.mutate(ctx, r.config.editModelDelete(providerID, modelID))
	if err != nil {
		return protocol.ModelMutation{}, err
	}
	return protocol.ModelMutation{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Result:                modelPostState(captured, providerID, modelID),
	}, nil
}

// ResetModelField removes exactly one model user-layer override under the
// shared field enum — an absent override publishes the same successful edit
// — and projects the effective post-state from the returned candidate.
func (r *Melta) ResetModelField(ctx context.Context, c Caller, providerID, modelID string, field protocol.ModelField) (protocol.ModelMutation, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ModelMutation{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ModelMutation{}, err
	}
	captured, err := r.config.mutate(ctx, r.config.editModelFieldReset(providerID, modelID, field))
	if err != nil {
		return protocol.ModelMutation{}, err
	}
	return protocol.ModelMutation{
		ConfigurationRevision: configurationRevision(captured.snapshot, r.instance),
		Result:                modelPostState(captured, providerID, modelID),
	}, nil
}

// capturedProvider resolves one provider ID against the captured catalog:
// an empty identity wraps the shared invalid sentinel and an unknown valid
// one returns the public not-found class with the catalog's typed cause.
func capturedProvider(captured *configuration, providerID string) (*catalog.Provider, error) {
	if providerID == "" {
		return nil, fmt.Errorf("provider id must be non-empty: %w", harness.ErrInvalid)
	}
	prov, ok := captured.catalog.Providers[providerID]
	if !ok || prov == nil {
		return nil, fmt.Errorf("provider %q: %w: %w", providerID, catalog.ErrUnknownProvider, harness.ErrNotFound)
	}
	return prov, nil
}

// capturedEnvIsSet is the one captured credential predicate: a provider
// connection decision reads the nonemptiness of the value sampled with its
// own capture, never a live environment getter.
func capturedEnvIsSet(credentials map[string]CredentialValue) func(string) bool {
	return func(name string) bool {
		return credentials[name].Value != ""
	}
}

// projectProviders projects every effective provider view, sorted by ID.
func projectProviders(captured configurationCapture) []protocol.Provider {
	ids := make([]string, 0, len(captured.snapshot.catalog.Providers))
	for id := range captured.snapshot.catalog.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]protocol.Provider, 0, len(ids))
	for _, id := range ids {
		out = append(out, projectProvider(captured, captured.snapshot.catalog.Providers[id]))
	}
	return out
}

// projectProvider maps one effective catalog provider onto its generated
// view: the effective and user headers projected separately under the
// no-authorization response contract, and the key-source classification and
// connection label from the capture's own credential sample. These are
// labels, never values: no key value, permission suggestion, or generated
// name suggestion appears.
func projectProvider(captured configurationCapture, prov *catalog.Provider) protocol.Provider {
	connected := catalog.ProviderConnected(prov, capturedEnvIsSet(captured.snapshot.credentials))
	keySource := classifyKeySource(prov.Transport.APIKeyEnv, captured.snapshot.credentials)
	return protocol.Provider{
		ApiKeyEnv:        prov.Transport.APIKeyEnv,
		BaseUrl:          prov.Transport.BaseURL,
		Builtin:          prov.Builtin,
		Connected:        connected,
		Discovery:        prov.Discovery,
		ExtraBody:        jsonMapPointer(prov.ExtraBody),
		Headers:          stripCredentialHeaders(prov.Transport.Headers),
		Hidden:           prov.Hidden,
		Id:               prov.ID,
		KeySource:        protocol.ProviderKeySource(keySource),
		Models:           projectModels(prov),
		Name:             prov.Name,
		SystemRole:       protocol.SystemRole(prov.SystemRole),
		UsageInStream:    prov.UsageInStream,
		UserHeaders:      captured.snapshot.userHeaders(prov.ID),
		ProtocolMetadata: projectProtocolMetadata(prov.ProtocolMetadata),
	}
}

// userHeaders extracts one provider's user-layer transport headers from the
// retained decoded user layer — the additions on top of any bundled headers,
// exactly as the user file carries them (the file itself is never edited).
func (c *configuration) userHeaders(providerID string) map[string]string {
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

// projectAgents maps the captured resolver roster onto the roster view. Each
// entry carries its complete source-qualified reference, stamped by the owner
// from the supplying declaration; two providers may use the same displayed
// name. Required array members are owned non-nil lists even when the
// definition carries none, and no private definition field (prompt body) is
// exposed.
func projectAgents(captured *configuration) []protocol.Agent {
	out := make([]protocol.Agent, 0, len(captured.definitions))
	for _, at := range captured.definitions {
		inputs := at.inputs.Owned()
		deref := inputs.Model
		out = append(out, protocol.Agent{
			Name:         at.name,
			Definition:   protocol.DefinitionRef{ExtensionId: at.ref.ExtensionID, CapabilityId: at.ref.CapabilityID, DefinitionId: at.ref.DefinitionID},
			Description:  at.description,
			Subagent:     at.subagent,
			Model:        modelRefString(deref),
			Readonly:     inputs.Constraints.Readonly,
			WriteDir:     inputs.Constraints.WriteDir,
			Tools:        ownStrings(inputs.Tools),
			Capabilities: ownStrings(inputs.Capabilities),
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
// classifier over the capture's own observation: a defined unmanaged value is
// external regardless of emptiness — a defined-but-empty external variable
// supplies no credential but stays external — and managed membership comes
// from the managed set; neither is reread live.
func classifyKeySource(apiKeyEnv string, credentials map[string]CredentialValue) string {
	managed := func(name string) bool { return credentials[name].Managed }
	external := func(name string) bool {
		value := credentials[name]
		return value.Defined && !value.Managed
	}
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

// generateAPIKeyEnvName applies the retained name-generation rule: the base
// name LIGHTCODE_<UPPER_SNAKE_ID>_API_KEY (or the PROVIDER fallback for an
// unusable ID), suffixed 2+ while occupied. A name is occupied when the
// captured catalog references it or the credential source defines it —
// managed orphans and externally defined empty variables included — so
// allocation never reuses or overwrites an existing binding.
func (s *configurationService) generateAPIKeyEnvName(ctx context.Context, providerID string, cat *catalog.Catalog) (string, error) {
	base := "LIGHTCODE_" + upperSnake(providerID) + "_API_KEY"
	if base == "LIGHTCODE__API_KEY" {
		base = "LIGHTCODE_PROVIDER_API_KEY"
	}
	referenced := make(map[string]bool, len(cat.Providers))
	for _, prov := range cat.Providers {
		if prov != nil && prov.Transport.APIKeyEnv != "" {
			referenced[prov.Transport.APIKeyEnv] = true
		}
	}
	candidate := base
	for suffix := 2; ; suffix++ {
		if !referenced[candidate] {
			observed, err := s.credentials.Capture(ctx, []string{candidate})
			if err != nil {
				return "", err
			}
			if !observed[candidate].Defined {
				return candidate, nil
			}
		}
		candidate = fmt.Sprintf("%s_%d", base, suffix)
	}
}

// upperSnake renders a provider ID as its upper snake-case form over ASCII
// letters and digits alone: any other byte run collapses to one underscore
// and edge underscores are removed.
func upperSnake(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		var upper byte
		switch {
		case c >= 'a' && c <= 'z':
			upper = c - 'a' + 'A'
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			upper = c
		}
		if upper != 0 {
			b.WriteByte(upper)
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
