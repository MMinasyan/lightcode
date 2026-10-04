package runtime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/protocol"
)

// This file extends the one validated configuration writer with the retained
// provider/model metadata edits: custom provider create/update/delete,
// provider-field reset, user model upsert/delete, and model-field reset.
// Every edit resolves the edited subject against the captured published
// catalog inside the build hold, writes the latest raw user layer, and
// rejects an edited subject the shared catalog validation dropped through
// the writer's candidate check — before the owning write, with no legacy
// owner code, no second validator, and no secret parameter.

// invalidEdit classifies one refused metadata edit as invalid caller input.
func invalidEdit(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), harness.ErrInvalid)
}

// unknownModel reports one model ID the captured catalog does not know.
func unknownModel(providerID, modelID string) error {
	return fmt.Errorf("model %q: %w", providerID+"/"+modelID, catalog.ErrUnknownModel)
}

// userProvidersMember decodes the config root's providers member into its
// UseNumber map — absent and null members are the empty map, and every
// untouched value keeps its exact shape, numbers included. A decode failure
// is the shared raw-configuration failure class.
func userProvidersMember(roots rawRoots) (map[string]any, error) {
	raw, ok := roots.config["providers"]
	if !ok || string(raw) == "null" {
		return map[string]any{}, nil
	}
	var providers map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&providers); err != nil {
		return nil, configurationFailure(fmt.Errorf("decode providers member: %w", err))
	}
	if providers == nil {
		providers = map[string]any{}
	}
	return providers, nil
}

// writeUserProvidersMember re-encodes the edited providers map onto the
// config root: json.Number values marshal as their exact lexemes. An encode
// failure is the shared raw-configuration failure class.
func writeUserProvidersMember(roots rawRoots, providers map[string]any) error {
	data, err := json.Marshal(providers)
	if err != nil {
		return configurationFailure(fmt.Errorf("encode providers member: %w", err))
	}
	roots.config["providers"] = data
	return nil
}

// rawObjectMember is the one raw object-member accessor behind every
// metadata edit's navigation: an absent or null member is (nil, nil), a
// present non-object value is the shared raw-configuration failure class
// naming path, and create scaffolds a fresh object only where the member is
// missing (the write paths; the reset paths never scaffold). The providers
// member's UseNumber decode layer stays separate in userProvidersMember.
func rawObjectMember(parent map[string]any, key, path string, create bool) (map[string]any, error) {
	entry, ok := parent[key]
	if !ok || entry == nil {
		if !create {
			return nil, nil
		}
		object := map[string]any{}
		parent[key] = object
		return object, nil
	}
	object, ok := entry.(map[string]any)
	if !ok {
		return nil, configurationFailure(fmt.Errorf("%s must be an object", path))
	}
	return object, nil
}

// credentialHeaderName is the one shared retained predicate: a transport
// header name is a credential header when it is Authorization or
// Proxy-Authorization under any casing, whitespace-padded or not. The write
// refusal and the read projection's strip both use it; no other header name
// is sanitized.
func credentialHeaderName(name string) bool {
	trimmed := strings.TrimSpace(name)
	return strings.EqualFold(trimmed, "Authorization") || strings.EqualFold(trimmed, "Proxy-Authorization")
}

// refuseCredentialHeaders applies the retained rule: a caller-supplied
// transport header named Authorization or Proxy-Authorization under any
// casing is refused — the credential belongs to api_key_env.
func refuseCredentialHeaders(headers map[string]string) error {
	for name := range headers {
		if credentialHeaderName(name) {
			return invalidEdit("custom transport header %q is not allowed; use api_key_env instead", name)
		}
	}
	return nil
}

// writeTransportHeaders writes the supplied header map wholesale: an empty
// map deletes the raw user headers member, so removals and clears take
// effect.
func writeTransportHeaders(transport map[string]any, headers map[string]string) {
	if len(headers) == 0 {
		delete(transport, "headers")
		return
	}
	transport["headers"] = headers
}

// capturedCatalogProvider resolves one provider ID against the mutation's
// captured catalog — the edits resolve subjects against their one owned
// capture; there is no fallback catalog.
func capturedCatalogProvider(captured configurationCapture, providerID string) *catalog.Provider {
	return captured.snapshot.catalog.Providers[providerID]
}

// providerEditTarget resolves one metadata edit's addressed provider
// identity and its raw user node. The identity is addressed from the
// effective catalog OR the latest user layer; one absent from both is the
// typed not-found. A present raw node is returned decoded (a non-object
// node is the shared raw-configuration failure), an absent node with a live
// effective builtin is scaffolded for the write paths, and an absent node
// with a live effective custom provider stays absent — the latest user
// layer no longer owns that definition, so the edit applies nothing and the
// candidate check governs the refusal; no partial patch reconstructs it.
func providerEditTarget(captured configurationCapture, providers map[string]any, providerID string, scaffold bool) (map[string]any, *catalog.Provider, error) {
	if providerID == "" {
		return nil, nil, fmt.Errorf("provider id must be non-empty: %w", harness.ErrInvalid)
	}
	eff := capturedCatalogProvider(captured, providerID)
	node, err := rawObjectMember(providers, providerID, "providers."+providerID, false)
	if err != nil {
		return nil, nil, err
	}
	if eff == nil && node == nil {
		return nil, nil, fmt.Errorf("provider %q: %w", providerID, catalog.ErrUnknownProvider)
	}
	if node == nil && scaffold && eff != nil && eff.Builtin {
		node = map[string]any{}
		providers[providerID] = node
	}
	return node, eff, nil
}

// modelEditTarget resolves one model edit's addressed identities under the
// same effective-or-latest-user rule: the provider identity first, then the
// raw models member of the provider's user node (created only for the
// upsert's scaffolding writes). The model identity itself is checked by the
// caller: the PUT is an upsert and accepts a missing model, while the
// delete and the reset require it in the effective models or the latest raw
// models and fail the typed unknown-model error otherwise.
func modelEditTarget(captured configurationCapture, providers map[string]any, providerID, modelID string, scaffold bool) (map[string]any, map[string]any, *catalog.Provider, error) {
	if modelID == "" {
		return nil, nil, nil, fmt.Errorf("model id must be non-empty: %w", harness.ErrInvalid)
	}
	node, eff, err := providerEditTarget(captured, providers, providerID, scaffold)
	if err != nil {
		return nil, nil, nil, err
	}
	var rawModels map[string]any
	if node != nil {
		if rawModels, err = rawObjectMember(node, "models", "providers models", scaffold); err != nil {
			return nil, nil, nil, err
		}
	}
	return node, rawModels, eff, nil
}

// modelIdentityPresent reports whether the addressed model exists in the
// effective catalog or the latest raw user layer.
func modelIdentityPresent(eff *catalog.Provider, rawModels map[string]any, modelID string) bool {
	if eff != nil && eff.Models[modelID] != nil {
		return true
	}
	_, present := rawModels[modelID]
	return present
}

// writeProviderEditMembers writes the eight non-transport ProviderEdit
// members onto a raw provider map: every provided member replaces its whole
// value exactly as supplied, zeros, falses, and whitespace included — the
// pointer presence is the patch, and the catalog validators decide
// validity. The create and the update share this one write point; system_role
// stays a raw enum value and no member is normalized.
func writeProviderEditMembers(pm map[string]any, patch protocol.ProviderEdit) {
	if patch.Name != nil {
		pm["name"] = *patch.Name
	}
	if patch.SystemRole != nil {
		pm["system_role"] = *patch.SystemRole
	}
	if patch.UsageInStream != nil {
		pm["usage_in_stream"] = *patch.UsageInStream
	}
	if patch.MaxTokensField != nil {
		pm["max_tokens_field"] = *patch.MaxTokensField
	}
	if patch.ExtraBody != nil {
		pm["extra_body"] = *patch.ExtraBody
	}
	if patch.Discovery != nil {
		pm["discovery"] = *patch.Discovery
	}
	if patch.Hidden != nil {
		pm["hidden"] = *patch.Hidden
	}
	if patch.ProtocolMetadata != nil {
		pm["protocol_metadata"] = *patch.ProtocolMetadata
	}
}

// mergeModelEdit writes the supplied ModelEdit members onto one raw model
// map in place: every provided member replaces its whole value, zeros and
// falses included, and no unsupplied member is touched — the raw entry is
// never replaced wholesale, so one partial patch preserves every earlier
// member of the model's user layer.
func mergeModelEdit(m map[string]any, edit protocol.ModelEdit) {
	if edit.Name != nil {
		m["name"] = *edit.Name
	}
	if edit.ContextWindow != nil {
		m["context_window"] = *edit.ContextWindow
	}
	if edit.MaxOutputTokens != nil {
		m["max_output_tokens"] = *edit.MaxOutputTokens
	}
	if edit.InputModalities != nil {
		m["input_modalities"] = *edit.InputModalities
	}
	if edit.SystemRole != nil {
		m["system_role"] = *edit.SystemRole
	}
	if edit.UsageInStream != nil {
		m["usage_in_stream"] = *edit.UsageInStream
	}
	if edit.ExtraBody != nil {
		m["extra_body"] = *edit.ExtraBody
	}
	if edit.Cost != nil {
		m["cost"] = *edit.Cost
	}
	if edit.ProtocolMetadata != nil {
		m["protocol_metadata"] = *edit.ProtocolMetadata
	}
	if edit.Hidden != nil {
		m["hidden"] = *edit.Hidden
	}
}

// requireCandidateProvider is the shared candidate check: the touched
// provider must be present in the built candidate's catalog — the same
// catalog validation that constructs the ready candidate accepted it, and a
// provider dropped as a build warning refuses the edit before the write.
func requireCandidateProvider(c *configuration, providerID string) error {
	if prov := c.catalog.Providers[providerID]; prov != nil {
		return nil
	}
	return fmt.Errorf("provider %q is not valid after the edit", providerID)
}

// requireCandidateModel is the model-side candidate check over the same
// candidate catalog.
func requireCandidateModel(c *configuration, providerID, modelID string) error {
	if err := requireCandidateProvider(c, providerID); err != nil {
		return err
	}
	if entry := c.catalog.Providers[providerID].Models[modelID]; entry != nil {
		return nil
	}
	return fmt.Errorf("model %q is not valid after the edit", providerID+"/"+modelID)
}

// editProviderCreate builds the custom-provider create edit: a new trimmed
// nonempty ID, a required base URL, and a models map with normalized unique
// IDs — candidate validity, not an editor-only usable-model count, governs
// metadata creation, so incomplete and model-less definitions are
// representable. No secret parameter is written to the raw layer: the
// create carries a non-secret env name plus the optional write-only key
// value under the one credential-input rule. The candidate check proves the
// created provider survived the shared catalog validation, and the edit's
// connection data carries the one managed-key write that follows the owning
// file.
func (s *configurationService) editProviderCreate(providerID string, patch protocol.ProviderEdit, models map[string]protocol.ModelEdit, key *string) configurationEdit {
	// The create identity is trim-normalized once at the constructor entry:
	// the apply and the candidate check resolve the same canonicalized ID
	// (the Runtime operator's separate by-value parameter still canonicalizes
	// its own result lookup — normalization does not propagate back from the
	// callee).
	providerID = strings.TrimSpace(providerID)
	// The create's connection data is one plain record the apply fills and
	// the writer consumes after the owning file: the key action is None
	// unless a managed key must be persisted.
	plan := &connectionEffects{}
	return configurationEdit{
		apply: func(roots rawRoots, captured configurationCapture) (editedFile, error) {
			if providerID == "" {
				return 0, invalidEdit("provider id is required")
			}
			if capturedCatalogProvider(captured, providerID) != nil {
				return 0, invalidEdit("provider %q already exists", providerID)
			}
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, err := rawObjectMember(providers, providerID, "providers."+providerID, false)
			if err != nil {
				return 0, err
			}
			if pm != nil {
				return 0, invalidEdit("provider %q already exists", providerID)
			}
			baseURL := ""
			if patch.BaseUrl != nil {
				baseURL = *patch.BaseUrl
			}
			if baseURL == "" {
				return 0, invalidEdit("base_url is required")
			}
			headers := map[string]string(nil)
			if patch.Headers != nil {
				if err := refuseCredentialHeaders(*patch.Headers); err != nil {
					return 0, err
				}
				headers = *patch.Headers
			}
			// The env binding resolves inside the build hold under the one
			// credential-input rule. Without a supplied key the create
			// performs no credential action and no resolution refusal —
			// any explicit binding, occupied or absent, is registered
			// as-is. A supplied nonempty key requires a binding: an
			// explicitly empty one is invalid, and a missing member
			// allocates a fresh name; the supplied key itself resolves
			// through the same shared rule connect uses, so its
			// external-variable refusal fires before any side effect. The
			// exact supplied bytes reach only the managed owner.
			keyEnv := ""
			if patch.ApiKeyEnv != nil {
				keyEnv = *patch.ApiKeyEnv
				if keyEnv == "" && key != nil && *key != "" {
					return 0, invalidEdit("api_key_env is empty; name a variable or omit it to generate one for the supplied key")
				}
			}
			if key != nil && *key != "" {
				if keyEnv == "" {
					keyEnv = s.generateAPIKeyEnvName(providerID, captured.snapshot.catalog)
				}
				observed := s.env.Capture([]string{keyEnv})[keyEnv]
				if _, persist, err := resolveConnectKey(keyEnv, key, observed); err != nil {
					return 0, configurationFailure(err)
				} else if persist {
					plan.keyAction = keyActionSet
					plan.keyEnv = keyEnv
					plan.keyValue = *key // the exact supplied value; never trim-normalized
				}
			}
			modelsRaw := map[string]any{}
			for modelID, edit := range models {
				normalized := strings.TrimSpace(modelID)
				if normalized == "" {
					return 0, invalidEdit("model id is required")
				}
				if _, exists := modelsRaw[normalized]; exists {
					return 0, invalidEdit("duplicate model id %q", normalized)
				}
				modelMap := map[string]any{}
				mergeModelEdit(modelMap, edit)
				modelsRaw[normalized] = modelMap
			}
			providerMap := map[string]any{
				"transport": providerTransportRaw(patch, baseURL, headers, keyEnv),
				"models":    modelsRaw,
			}
			writeProviderEditMembers(providerMap, patch)
			providers[providerID] = providerMap
			if err := writeUserProvidersMember(roots, providers); err != nil {
				return 0, err
			}
			return editMainConfig, nil
		},
		connection: plan,
		check: func(c *configuration, _ configurationCapture) error {
			return requireCandidateProvider(c, providerID)
		},
	}
}

// providerTransportRaw renders the create patch's transport members. The
// api_key_env member is always present in a created transport — the raw
// validator requires the member itself and admits an empty value — with the
// resolved name (explicit, generated, or empty for keyless) written by the
// caller, and the provided headers go through the same wholesale writer as
// the update, so an empty provided map leaves the raw headers member absent.
func providerTransportRaw(patch protocol.ProviderEdit, baseURL string, headers map[string]string, keyEnv string) map[string]any {
	transport := map[string]any{"base_url": baseURL, "api_key_env": keyEnv}
	if patch.Headers != nil {
		writeTransportHeaders(transport, headers)
	}
	if patch.Options != nil {
		transport["options"] = *patch.Options
	}
	return transport
}

// editProviderUpdate builds the existing-provider patch edit: the addressed
// identity resolves from the effective catalog or the latest user layer, no
// source-specific field lock applies — the provided members land on the
// user layer for every source and the candidate check governs validity —
// and the latest raw user layer receives the provided members wholesale per
// member, headers included.
func (s *configurationService) editProviderUpdate(providerID string, patch protocol.ProviderEdit) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots, captured configurationCapture) (editedFile, error) {
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, _, err := providerEditTarget(captured, providers, providerID, true)
			if err != nil {
				return 0, err
			}
			if pm == nil {
				// The effective catalog still knows the identity but the
				// latest user layer no longer owns a definition: nothing is
				// scaffolded or written, and the candidate check refuses.
				return editMainConfig, nil
			}
			headers := map[string]string(nil)
			if patch.Headers != nil {
				if err := refuseCredentialHeaders(*patch.Headers); err != nil {
					return 0, err
				}
				headers = *patch.Headers
			}
			// The binding value is written exactly as supplied; no
			// connection state or sibling reference constrains the
			// rebinding — the credential itself is never touched.
			env := ""
			if patch.ApiKeyEnv != nil {
				env = *patch.ApiKeyEnv
			}
			transport, err := rawObjectMember(pm, "transport", "providers transport", true)
			if err != nil {
				return 0, err
			}
			if patch.BaseUrl != nil {
				transport["base_url"] = *patch.BaseUrl
			}
			if patch.ApiKeyEnv != nil {
				transport["api_key_env"] = env
			}
			if patch.Options != nil {
				transport["options"] = *patch.Options
			}
			if patch.Headers != nil {
				writeTransportHeaders(transport, headers)
			}
			writeProviderEditMembers(pm, patch)
			if err := writeUserProvidersMember(roots, providers); err != nil {
				return 0, err
			}
			return editMainConfig, nil
		},
		check: func(c *configuration, _ configurationCapture) error {
			return requireCandidateProvider(c, providerID)
		},
	}
}

// editProviderDelete builds the provider delete edit: the addressed identity
// resolves from the effective catalog or the latest user layer, and only the
// user node leaves the latest raw layer — never bundled data or the
// discovery cache, and no connection state gates the removal. An identity
// whose user node is already absent still publishes: the post-state is the
// surviving effective subject, or null when the candidate no longer
// contains one.
func (s *configurationService) editProviderDelete(providerID string) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots, captured configurationCapture) (editedFile, error) {
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			if _, _, err := providerEditTarget(captured, providers, providerID, false); err != nil {
				return 0, err
			}
			delete(providers, providerID)
			if err := writeUserProvidersMember(roots, providers); err != nil {
				return 0, err
			}
			return editMainConfig, nil
		},
	}
}

// providerResetTransport marks the reset fields that own a transport member.
var providerResetTransport = map[protocol.ProviderField]bool{
	protocol.ProviderFieldBaseUrl:             true,
	protocol.ProviderFieldEnvironmentVariable: true,
	protocol.ProviderFieldHeaders:             true,
	protocol.ProviderFieldOptions:             true,
}

// editProviderFieldReset builds the provider-field reset edit: the shared
// closed field enum, and the deletion of exactly one user override from its
// owning raw path. No connection state or source constrains the reset — the
// candidate check governs validity, so a required custom transport key that
// leaves the definition invalid refuses before the write while a builtin
// override's removal reveals the bundled value. A successful reset with no
// override present still rewrites the owning file and publishes the next
// generation — the one shared edit rule.
func (s *configurationService) editProviderFieldReset(providerID string, field protocol.ProviderField) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots, captured configurationCapture) (editedFile, error) {
			if !field.Valid() {
				return 0, invalidEdit("field %q cannot be reset", field)
			}
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, _, err := providerEditTarget(captured, providers, providerID, false)
			if err != nil {
				return 0, err
			}
			if pm == nil {
				return editMainConfig, nil // no user node: the reset still rewrites and publishes
			}
			target := pm
			if providerResetTransport[field] {
				if target, err = rawObjectMember(pm, "transport", "providers transport", false); err != nil {
					return 0, err
				}
				if target == nil {
					return editMainConfig, nil
				}
			}
			if _, present := target[string(field)]; !present {
				return editMainConfig, nil
			}
			delete(target, string(field))
			if err := writeUserProvidersMember(roots, providers); err != nil {
				return 0, err
			}
			return editMainConfig, nil
		},
		check: func(c *configuration, _ configurationCapture) error {
			return requireCandidateProvider(c, providerID)
		},
	}
}

// editModelSave builds the model upsert edit — the same PUT as the retained
// SaveModel, so a missing model creates a user model. The provider identity
// resolves from the effective catalog or the latest user layer and no
// source-specific field lock applies: the provided members merge onto the
// model's prior raw entry for every source — no unsupplied member is
// touched — and the candidate check governs validity. Builtin providers
// scaffold their user override; a custom provider whose latest raw
// definition is gone applies nothing and the candidate refuses.
func (s *configurationService) editModelSave(providerID, modelID string, patch protocol.ModelEdit) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots, captured configurationCapture) (editedFile, error) {
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, models, _, err := modelEditTarget(captured, providers, providerID, modelID, true)
			if err != nil {
				return 0, err
			}
			if pm == nil {
				// The latest user layer no longer owns the provider
				// definition; nothing is scaffolded and the candidate check
				// refuses.
				return editMainConfig, nil
			}
			// The patch merges into the model's prior raw entry — a partial
			// patch preserves every unsupplied member, and an empty patch
			// wipes nothing — with a fresh entry only for a new model.
			rawModel, err := rawObjectMember(models, modelID, "models."+modelID, false)
			if err != nil {
				return 0, err
			}
			if rawModel == nil {
				rawModel = map[string]any{}
			}
			mergeModelEdit(rawModel, patch)
			models[modelID] = rawModel
			if err := writeUserProvidersMember(roots, providers); err != nil {
				return 0, err
			}
			return editMainConfig, nil
		},
		check: func(c *configuration, _ configurationCapture) error {
			return requireCandidateModel(c, providerID, modelID)
		},
	}
}

// editModelDelete builds the model delete edit: the addressed model identity
// resolves from the effective catalog or the latest user layer (absent from
// both is the typed not-found), and only its user node leaves the latest raw
// layer — a bundled or discovered model loses its override and reveals its
// base, a standalone user model leaves nothing behind. An identity whose
// user node is already absent still publishes; the post-state decides.
func (s *configurationService) editModelDelete(providerID, modelID string) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots, captured configurationCapture) (editedFile, error) {
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, models, eff, err := modelEditTarget(captured, providers, providerID, modelID, false)
			if err != nil {
				return 0, err
			}
			if !modelIdentityPresent(eff, models, modelID) {
				return 0, unknownModel(providerID, modelID)
			}
			if pm != nil {
				delete(models, modelID)
				if err := writeUserProvidersMember(roots, providers); err != nil {
					return 0, err
				}
			}
			return editMainConfig, nil
		},
	}
}

// editModelFieldReset builds the model-field reset edit: the shared closed
// field enum, and the deletion of exactly one user override from its owning
// raw path. The addressed model identity resolves from the effective catalog
// or the latest user layer; no source or member constrains the reset —
// removing a sole user-model window leaves a valid incomplete model, and the
// candidate check governs the rest. A successful reset with no override
// present still rewrites the owning file and publishes the next generation.
func (s *configurationService) editModelFieldReset(providerID, modelID string, field protocol.ModelField) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots, captured configurationCapture) (editedFile, error) {
			if !field.Valid() {
				return 0, invalidEdit("field %q cannot be reset", field)
			}
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, models, eff, err := modelEditTarget(captured, providers, providerID, modelID, false)
			if err != nil {
				return 0, err
			}
			if !modelIdentityPresent(eff, models, modelID) {
				return 0, unknownModel(providerID, modelID)
			}
			if pm == nil {
				return editMainConfig, nil // no user node: the reset still rewrites and publishes
			}
			rawModel, err := rawObjectMember(models, modelID, "models."+modelID, false)
			if err != nil {
				return 0, err
			}
			if rawModel == nil {
				return editMainConfig, nil
			}
			if _, present := rawModel[string(field)]; !present {
				return editMainConfig, nil
			}
			delete(rawModel, string(field))
			if err := writeUserProvidersMember(roots, providers); err != nil {
				return 0, err
			}
			return editMainConfig, nil
		},
		check: func(c *configuration, _ configurationCapture) error {
			return requireCandidateModel(c, providerID, modelID)
		},
	}
}
