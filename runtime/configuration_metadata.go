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

// stripBundledHeaderKeys removes the supplied keys that case-insensitively
// collide with the bundled provider's own header names. The one shared
// predicate of the read projection (user_headers) and the write path, which
// strips before the wholesale user-layer write, healing any leaked override.
func stripBundledHeaderKeys(headers map[string]string, providerID string) map[string]string {
	bundled := catalog.BundledProviderHeaders(providerID)
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		collides := false
		for bundledName := range bundled {
			if strings.EqualFold(name, bundledName) {
				collides = true
				break
			}
		}
		if !collides {
			out[name] = value
		}
	}
	return out
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

// requireCustomRawEntry refuses to scaffold a custom provider's owning user
// definition: a custom definition deleted from the latest raw layer by an
// external editor is not silently recreated by an update, delete, or model
// write. Builtin user overrides may be absent — those scaffold.
func requireCustomRawEntry(providers map[string]any, providerID string) (map[string]any, error) {
	pm, err := rawObjectMember(providers, providerID, "providers."+providerID, false)
	if err != nil {
		return nil, err
	}
	if pm == nil {
		return nil, fmt.Errorf("provider %q has no user definition in the latest configuration to edit: %w", providerID, catalog.ErrUnknownProvider)
	}
	return pm, nil
}

// catalogProvider resolves one provider ID against the captured published
// catalog — the edits resolve subjects against s.current().catalog directly;
// there is no fallback catalog.
func (s *configurationService) catalogProvider(providerID string) *catalog.Provider {
	return s.current().catalog.Providers[providerID]
}

// envNameInUse reports whether the captured catalog's other providers occupy
// the env name.
func (s *configurationService) envNameInUse(apiKeyEnv, selfID string) bool {
	for id, prov := range s.current().catalog.Providers {
		if id == selfID || prov == nil {
			continue
		}
		if prov.Transport.APIKeyEnv == apiKeyEnv {
			return true
		}
	}
	return false
}

// refusedModelFields refuses one identity/protocol member supplied on a
// bundled or discovered model: those fields belong to their source, and
// presence — even of an equal value — is the refusal trigger.
func refusedModelFields(edit protocol.ModelEdit, modelID string) error {
	switch {
	case edit.Name != nil:
		return invalidEdit("cannot rename model %q: its name belongs to its bundled or discovered source", modelID)
	case edit.SystemRole != nil:
		return invalidEdit("cannot change the system role of model %q", modelID)
	case edit.UsageInStream != nil:
		return invalidEdit("cannot change usage-in-stream of model %q", modelID)
	case edit.InputModalities != nil:
		return invalidEdit("cannot change the input modalities of model %q", modelID)
	case edit.ProtocolMetadata != nil:
		return invalidEdit("cannot change protocol metadata of model %q", modelID)
	}
	return nil
}

// writeProviderEditMembers writes the eight non-transport ProviderEdit
// members onto a raw provider map: every provided member replaces its whole
// value, zeros and falses included — the pointer presence is the patch, and
// the catalog validators decide validity. The name and max-tokens field are
// trim-normalized at this one shared write point (the create and the update
// share it); system_role stays a raw enum value and no other member is
// normalized.
func writeProviderEditMembers(pm map[string]any, patch protocol.ProviderEdit) {
	if patch.Name != nil {
		pm["name"] = strings.TrimSpace(*patch.Name)
	}
	if patch.SystemRole != nil {
		pm["system_role"] = *patch.SystemRole
	}
	if patch.UsageInStream != nil {
		pm["usage_in_stream"] = *patch.UsageInStream
	}
	if patch.MaxTokensField != nil {
		pm["max_tokens_field"] = strings.TrimSpace(*patch.MaxTokensField)
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
		m["name"] = strings.TrimSpace(*edit.Name)
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
// nonempty ID, a required base URL, a models map with normalized unique IDs
// and at least one usable model, and no secret parameter in the edit — the
// create accepts a non-secret env name plus the optional write-only key
// value, which is never written to the raw layer. A missing api_key_env
// member with a supplied nonempty key generates the current unique name
// inside the build hold; an explicitly empty member stays keyless. The
// candidate check proves the created provider survived the shared catalog
// validation with a usable model, and the edit's connection data carries the
// one managed-key write that follows the owning file.
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
		apply: func(roots rawRoots) (editedFile, error) {
			if providerID == "" {
				return 0, invalidEdit("provider id is required")
			}
			captured := s.catalogProvider(providerID)
			if captured != nil {
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
				baseURL = strings.TrimSpace(*patch.BaseUrl)
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
			// The env name resolves inside the build hold: the pointer
			// presence is the patch — an explicitly empty member stays
			// keyless — and the generated name for a supplied key occupies
			// the current catalog's api_key_env set. The one credential rule
			// resolves a nonempty name: a managed name (including a
			// generated-name collision with a managed orphan) is updated by
			// a supplied nonempty value or used as-is when it already holds
			// one; an external shell key wins and is never persisted; every
			// resolution failure is the shared configuration failure class.
			keyEnv := ""
			if patch.ApiKeyEnv != nil {
				keyEnv = strings.TrimSpace(*patch.ApiKeyEnv)
				if keyEnv != "" && s.envNameInUse(keyEnv, providerID) {
					return 0, invalidEdit("api_key_env %s is already used by another provider", keyEnv)
				}
			} else if key != nil && *key != "" {
				keyEnv = generatedAPIKeyEnvName(providerID, s.current().catalog)
			}
			if keyEnv != "" {
				_, persist, err := resolveConnectKey(keyEnv, key, s.env)
				if err != nil {
					return 0, configurationFailure(err)
				}
				if persist {
					plan.keyAction = keyActionSet
					plan.keyEnv = keyEnv
					plan.keyValue = *key // the exact supplied value; never trim-normalized
				}
			}
			modelsRaw := map[string]any{}
			usable := 0
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
				if edit.ContextWindow != nil && *edit.ContextWindow > 0 {
					usable++
				}
			}
			if usable == 0 {
				return 0, invalidEdit("custom provider requires at least one usable model")
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
		check: func(c *configuration) error {
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

// providerRawTarget returns the raw provider entry a write edit targets:
// a builtin's absent user override scaffolds a bare entry (the override may
// be absent), while a custom provider's owning definition must exist in the
// latest raw layer and is never silently recreated.
func providerRawTarget(providers map[string]any, providerID string, builtin bool) (map[string]any, error) {
	var pm map[string]any
	var err error
	if builtin {
		if pm, err = rawObjectMember(providers, providerID, "providers."+providerID, false); err != nil {
			return nil, err
		}
		if pm == nil {
			pm = map[string]any{}
			providers[providerID] = pm
		}
		return pm, nil
	}
	return requireCustomRawEntry(providers, providerID)
}

// editProviderUpdate builds the existing-provider patch edit: the captured
// catalog resolves the subject and its builtin locks (member presence, even
// of an equal value, is the refusal), the connected-provider api_key_env
// change prohibition and the env-name uniqueness run before any change, and
// the latest raw user layer receives the provided members — wholesale per
// member, headers written wholesale with the bundled-key strip.
func (s *configurationService) editProviderUpdate(providerID string, patch protocol.ProviderEdit) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots) (editedFile, error) {
			captured, err := capturedProvider(s.current(), providerID)
			if err != nil {
				return 0, err
			}
			if captured.Builtin {
				if err := refuseBuiltinProviderFields(patch, providerID); err != nil {
					return 0, err
				}
			}
			headers := map[string]string(nil)
			if patch.Headers != nil {
				if err := refuseCredentialHeaders(*patch.Headers); err != nil {
					return 0, err
				}
				headers = *patch.Headers
				if captured.Builtin {
					headers = stripBundledHeaderKeys(headers, providerID)
				}
			}
			// The env name is trim-normalized before the retained
			// current-effective comparison, the uniqueness check, and the
			// raw write: a padded same name is a legitimate no-change, and
			// any ACTUAL change — a clear to keyless included — is refused
			// while the current provider is connected.
			env := ""
			if patch.ApiKeyEnv != nil {
				env = strings.TrimSpace(*patch.ApiKeyEnv)
				if env != captured.Transport.APIKeyEnv {
					if catalog.ProviderConnected(captured, liveEnvIsSet) {
						return 0, invalidEdit("disconnect provider %q before changing its API key variable", providerID)
					}
					if env != "" && s.envNameInUse(env, providerID) {
						return 0, invalidEdit("api_key_env %s is already used by another provider", env)
					}
				}
			}
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, err := providerRawTarget(providers, providerID, captured.Builtin)
			if err != nil {
				return 0, err
			}
			transport, err := rawObjectMember(pm, "transport", "providers transport", true)
			if err != nil {
				return 0, err
			}
			if patch.BaseUrl != nil {
				transport["base_url"] = strings.TrimSpace(*patch.BaseUrl)
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
		check: func(c *configuration) error {
			return requireCandidateProvider(c, providerID)
		},
	}
}

// refuseBuiltinProviderFields refuses every locked member supplied on a
// builtin provider patch. Presence — even of the bundled value itself — is
// the refusal trigger; headers, api_key_env, extra_body, discovery and
// hidden stay writable through the candidate path.
func refuseBuiltinProviderFields(patch protocol.ProviderEdit, providerID string) error {
	switch {
	case patch.Name != nil:
		return invalidEdit("cannot change the name of a built-in provider")
	case patch.BaseUrl != nil:
		return invalidEdit("cannot change the base URL of a built-in provider")
	case patch.Options != nil:
		return invalidEdit("cannot change the options of a built-in provider")
	case patch.SystemRole != nil:
		return invalidEdit("cannot change the system role of a built-in provider")
	case patch.MaxTokensField != nil:
		return invalidEdit("cannot change the max-tokens field of a built-in provider")
	case patch.UsageInStream != nil:
		return invalidEdit("cannot change usage-in-stream of a built-in provider")
	case patch.ProtocolMetadata != nil:
		return invalidEdit("cannot change protocol metadata of a built-in provider")
	}
	return nil
}

// editProviderDelete builds the custom-provider delete edit: the captured
// catalog refuses builtins and connected env-keyed providers (a connected
// keyless custom provider is removable), and the latest raw layer loses only
// its owning user definition.
func (s *configurationService) editProviderDelete(providerID string) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots) (editedFile, error) {
			captured, err := capturedProvider(s.current(), providerID)
			if err != nil {
				return 0, err
			}
			if captured.Builtin {
				return 0, invalidEdit("cannot remove bundled provider %q", providerID)
			}
			if catalog.ProviderConnected(captured, liveEnvIsSet) && captured.Transport.APIKeyEnv != "" {
				return 0, invalidEdit("disconnect provider %q before removing it", providerID)
			}
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			if _, err := requireCustomRawEntry(providers, providerID); err != nil {
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
var providerResetTransport = map[protocol.ResetProviderFieldParamsField]bool{
	protocol.ResetProviderFieldParamsFieldBaseUrl:             true,
	protocol.ResetProviderFieldParamsFieldEnvironmentVariable: true,
	protocol.ResetProviderFieldParamsFieldHeaders:             true,
	protocol.ResetProviderFieldParamsFieldOptions:             true,
}

// editProviderFieldReset builds the provider-field reset edit: a closed
// generated field enum, the retained connected-provider api_key_env refusal,
// and the deletion of exactly one user override from its owning raw path. A
// successful reset with no override present still rewrites the owning file
// and publishes the next generation — the one shared edit rule. The candidate
// check proves the reset subject still validates (a custom provider losing a
// required key is refused before the write).
func (s *configurationService) editProviderFieldReset(providerID string, field protocol.ResetProviderFieldParamsField) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots) (editedFile, error) {
			if !field.Valid() {
				return 0, invalidEdit("field %q cannot be reset", field)
			}
			captured, err := capturedProvider(s.current(), providerID)
			if err != nil {
				return 0, err
			}
			if field == protocol.ResetProviderFieldParamsFieldEnvironmentVariable && catalog.ProviderConnected(captured, liveEnvIsSet) {
				return 0, invalidEdit("disconnect provider %q before resetting its API key variable", providerID)
			}
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, err := rawObjectMember(providers, providerID, "providers."+providerID, false)
			if err != nil {
				return 0, err
			}
			if pm == nil {
				return editMainConfig, nil // no user override: the reset still rewrites and publishes
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
		check: func(c *configuration) error {
			return requireCandidateProvider(c, providerID)
		},
	}
}

// editModelSave builds the model upsert edit — the same PUT as the retained
// SaveModel, so a missing model creates a user model. The captured catalog's
// Model.Source labels the subject: bundled and discovered models refuse
// their identity/protocol members (presence is the trigger), user models and
// new models take the provided members merged onto their prior raw entry —
// no unsupplied member is touched. Custom providers must own a latest raw
// definition; builtin providers scaffold their user override.
func (s *configurationService) editModelSave(providerID, modelID string, patch protocol.ModelEdit) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots) (editedFile, error) {
			if providerID == "" || modelID == "" {
				return 0, invalidEdit("provider and model id are required")
			}
			captured, lookupErr := capturedProvider(s.current(), providerID)
			if lookupErr != nil {
				return 0, lookupErr
			}
			if entry := captured.Models[modelID]; entry != nil && entry.Source != catalog.SourceUser {
				if err := refusedModelFields(patch, modelID); err != nil {
					return 0, err
				}
			}
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, err := providerRawTarget(providers, providerID, captured.Builtin)
			if err != nil {
				return 0, err
			}
			models, err := rawObjectMember(pm, "models", "providers models", true)
			if err != nil {
				return 0, err
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
		check: func(c *configuration) error {
			return requireCandidateModel(c, providerID, modelID)
		},
	}
}

// editModelDelete builds the user-model delete edit: only user-source models
// are deletable, missing identities fail their typed unknown errors (no
// silent success), and the latest raw layer loses only that model entry.
func (s *configurationService) editModelDelete(providerID, modelID string) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots) (editedFile, error) {
			if providerID == "" || modelID == "" {
				return 0, invalidEdit("provider and model id are required")
			}
			captured, lookupErr := capturedProvider(s.current(), providerID)
			if lookupErr != nil {
				return 0, lookupErr
			}
			entry := captured.Models[modelID]
			if entry == nil {
				return 0, unknownModel(providerID, modelID)
			}
			if entry.Source != catalog.SourceUser {
				return 0, invalidEdit("cannot delete model %q: only user-added models can be removed; hide or reset it instead", modelID)
			}
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, err := rawObjectMember(providers, providerID, "providers."+providerID, false)
			if err != nil {
				return 0, err
			}
			if pm == nil {
				return 0, unknownModel(providerID, modelID)
			}
			models, err := rawObjectMember(pm, "models", "providers models", false)
			if err != nil {
				return 0, err
			}
			if _, present := models[modelID]; !present {
				return 0, unknownModel(providerID, modelID)
			}
			delete(models, modelID)
			if err := writeUserProvidersMember(roots, providers); err != nil {
				return 0, err
			}
			return editMainConfig, nil
		},
	}
}

// editModelFieldReset builds the model-field reset edit: a closed generated
// field enum, the retained user-model context_window refusal, and the
// deletion of exactly one user override. A successful reset with no override
// present still rewrites the owning file and publishes the next generation.
// The candidate check proves the reset subject still validates.
func (s *configurationService) editModelFieldReset(providerID, modelID string, field protocol.ResetProviderModelFieldParamsField) configurationEdit {
	return configurationEdit{
		apply: func(roots rawRoots) (editedFile, error) {
			if providerID == "" || modelID == "" {
				return 0, invalidEdit("provider and model id are required")
			}
			if !field.Valid() {
				return 0, invalidEdit("field %q cannot be reset", field)
			}
			captured, lookupErr := capturedProvider(s.current(), providerID)
			if lookupErr != nil {
				return 0, lookupErr
			}
			entry := captured.Models[modelID]
			if entry == nil {
				return 0, unknownModel(providerID, modelID)
			}
			if field == protocol.ResetProviderModelFieldParamsFieldContextWindow && entry.Source == catalog.SourceUser {
				return 0, invalidEdit("cannot reset context_window for user-added model %q", modelID)
			}
			providers, err := userProvidersMember(roots)
			if err != nil {
				return 0, err
			}
			pm, err := rawObjectMember(providers, providerID, "providers."+providerID, false)
			if err != nil {
				return 0, err
			}
			if pm == nil {
				return editMainConfig, nil // no user override: the reset still rewrites and publishes
			}
			models, err := rawObjectMember(pm, "models", "providers models", false)
			if err != nil {
				return 0, err
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
		check: func(c *configuration) error {
			return requireCandidateModel(c, providerID, modelID)
		},
	}
}
