package catalog

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BuildInputs contains the raw catalog layers consumed by Build.
type BuildInputs struct {
	Bundled map[string]json.RawMessage
	UserRaw map[string]any
	Cache   map[string]DiscoveredProvider
}

// DiscoveredProvider is provider metadata loaded from discovery cache.
type DiscoveredProvider struct {
	Models map[string]DiscoveredModel
}

// DiscoveredModel is model metadata loaded from discovery cache.
type DiscoveredModel struct {
	Name            string
	ContextWindow   int
	MaxOutputTokens int
	Cost            *Cost
}

// BuildResult contains the effective catalog and non-fatal warnings.
type BuildResult struct {
	Catalog  *Catalog
	Warnings []Warning
}

// Warning describes a recoverable catalog build problem.
type Warning struct {
	Kind     string
	Message  string
	Provider string
	Model    string
}

// Build constructs an effective catalog from bundled, discovery, and user layers.
func Build(inputs BuildInputs) BuildResult {
	result := BuildResult{Catalog: &Catalog{Providers: map[string]*Provider{}}}
	bundled := decodeBundledProviders(inputs.Bundled, &result)
	user := decodeUserProviders(inputs.UserRaw, &result)

	providerIDs := map[string]struct{}{}
	for id := range bundled {
		providerIDs[id] = struct{}{}
	}
	for id := range user {
		providerIDs[id] = struct{}{}
	}

	for providerID := range providerIDs {
		_, bundledProvider := bundled[providerID]
		merged := DeepMergeCatalog(bundled[providerID], user[providerID])
		applyProviderDefaults(providerID, merged)
		applyDiscovery(providerID, merged, inputs.Cache[providerID], user[providerID], bundledProvider)
		errs := ValidateRaw(providerID, merged, true)
		if len(errs) != 0 {
			result.Warnings = append(result.Warnings, validationWarning(providerID, "", warningKind(providerID, user), errs))
			continue
		}
		provider := rawToProvider(providerID, merged, bundledProvider)
		errs = ValidateEffective(provider)
		if len(errs) != 0 {
			result.Warnings = append(result.Warnings, validationWarning(providerID, "", warningKind(providerID, user), errs))
			continue
		}
		result.Catalog.Providers[providerID] = provider
		for _, ref := range (&Catalog{Providers: map[string]*Provider{providerID: provider}}).IncompleteModels() {
			result.Warnings = append(result.Warnings, Warning{
				Kind:     "incomplete_model",
				Provider: ref.Ref.Provider,
				Model:    ref.Ref.Model,
				Message:  fmt.Sprintf("model %s is incomplete", ref.Ref.String()),
			})
		}
		if !provider.Discovery && len(provider.Models) == 0 {
			result.Warnings = append(result.Warnings, Warning{
				Kind:     "empty_provider",
				Provider: providerID,
				Message:  fmt.Sprintf("provider %s has discovery disabled and no models", providerID),
			})
		}
	}
	return result
}

func decodeBundledProviders(raw map[string]json.RawMessage, result *BuildResult) map[string]map[string]any {
	providers := map[string]map[string]any{}
	for providerID, data := range raw {
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			result.Warnings = append(result.Warnings, Warning{Kind: "bundled_config_skip", Provider: providerID, Message: fmt.Sprintf("parse bundled provider: %v", err)})
			continue
		}
		providers[providerID] = decoded
	}
	return providers
}

func decodeUserProviders(raw map[string]any, result *BuildResult) map[string]map[string]any {
	providers := map[string]map[string]any{}
	for providerID, value := range raw {
		providerRaw, ok := value.(map[string]any)
		if !ok {
			result.Warnings = append(result.Warnings, Warning{Kind: "user_config_skip", Provider: providerID, Message: "provider must be an object"})
			continue
		}
		providers[providerID] = cloneJSONValue(providerRaw).(map[string]any)
	}
	return providers
}

func warningKind(providerID string, user map[string]map[string]any) string {
	if _, ok := user[providerID]; ok {
		return "user_config_skip"
	}
	return "bundled_config_skip"
}

func validationWarning(providerID, modelID, kind string, errs []ValidationError) Warning {
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		parts = append(parts, err.Error())
	}
	return Warning{Kind: kind, Provider: providerID, Model: modelID, Message: strings.Join(parts, "; ")}
}

func applyProviderDefaults(providerID string, raw map[string]any) {
	if _, ok := raw["id"]; !ok {
		raw["id"] = providerID
	}
	if _, ok := raw["name"]; !ok {
		raw["name"] = providerID
	}
	if _, ok := raw["system_role"]; !ok {
		raw["system_role"] = string(RoleSystem)
	}
	if _, ok := raw["usage_in_stream"]; !ok {
		raw["usage_in_stream"] = true
	}
	if _, ok := raw["max_tokens_field"]; !ok {
		raw["max_tokens_field"] = "max_tokens"
	}
	if _, ok := raw["discovery"]; !ok {
		raw["discovery"] = true
	}
	if _, ok := raw["hidden"]; !ok {
		raw["hidden"] = false
	}
	if _, ok := raw["extra_body"]; !ok {
		raw["extra_body"] = map[string]any{}
	}
	transport, ok := raw["transport"].(map[string]any)
	if ok {
		if _, ok := transport["headers"]; !ok {
			transport["headers"] = map[string]any{}
		}
		if _, ok := transport["options"]; !ok {
			transport["options"] = map[string]any{}
		}
	}
	modelsVal, exists := raw["models"]
	models, ok := modelsVal.(map[string]any)
	if !exists {
		raw["models"] = map[string]any{}
		return
	}
	if !ok {
		return
	}
	for modelID, value := range models {
		modelRaw, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := modelRaw["name"]; !ok {
			modelRaw["name"] = modelID
		}
		if _, ok := modelRaw["input_modalities"]; !ok {
			modelRaw["input_modalities"] = []any{string(ModalityText)}
		}
		if _, ok := modelRaw["system_role"]; !ok {
			modelRaw["system_role"] = raw["system_role"]
		}
		if _, ok := modelRaw["usage_in_stream"]; !ok {
			modelRaw["usage_in_stream"] = raw["usage_in_stream"]
		}
		if _, ok := modelRaw["hidden"]; !ok {
			modelRaw["hidden"] = false
		}
		if _, ok := modelRaw["extra_body"]; !ok {
			modelRaw["extra_body"] = map[string]any{}
		}
	}
}

func applyDiscovery(providerID string, raw map[string]any, discovered DiscoveredProvider, userRaw map[string]any, bundledProvider bool) {
	if len(discovered.Models) == 0 {
		return
	}
	discovery, ok := raw["discovery"].(bool)
	if ok && !discovery {
		return
	}
	modelsVal, exists := raw["models"]
	models, ok := modelsVal.(map[string]any)
	if !exists {
		models = map[string]any{}
		raw["models"] = models
	} else if !ok {
		return
	}
	for modelID, modelDiscovery := range discovered.Models {
		modelRaw, ok := models[modelID].(map[string]any)
		if !ok {
			if !bundledProvider {
				continue
			}
			modelRaw = map[string]any{}
			models[modelID] = modelRaw
		}
		if !bundledProvider {
			if modelDiscovery.Cost != nil {
				existingCost, _ := modelRaw["cost"].(map[string]any)
				if existingCost == nil {
					existingCost = map[string]any{}
					modelRaw["cost"] = existingCost
				}
				mergeCostRawFields(existingCost, modelDiscovery.Cost, nil)
			}
			continue
		}
		if _, ok := modelRaw["name"]; !ok && modelDiscovery.Name != "" {
			modelRaw["name"] = modelDiscovery.Name
		}
		if shouldFillInt(modelRaw["context_window"]) && modelDiscovery.ContextWindow > 0 {
			modelRaw["context_window"] = float64(modelDiscovery.ContextWindow)
		}
		if shouldFillInt(modelRaw["max_output_tokens"]) && modelDiscovery.MaxOutputTokens > 0 {
			modelRaw["max_output_tokens"] = float64(modelDiscovery.MaxOutputTokens)
		}
		if modelDiscovery.Cost != nil {
			existingCost, _ := modelRaw["cost"].(map[string]any)
			if existingCost == nil {
				existingCost = map[string]any{}
				modelRaw["cost"] = existingCost
			}
			mergeCostRawFields(existingCost, modelDiscovery.Cost, userCostFields(userRaw, modelID))
		}
	}
	applyProviderDefaults(providerID, raw)
}

func shouldFillInt(v any) bool {
	if v == nil {
		return true
	}
	i, ok := validationInt(v)
	return ok && i == 0
}

func costToRaw(cost *Cost) map[string]any {
	raw := map[string]any{}
	if cost.Input != nil {
		raw["input"] = *cost.Input
	}
	if cost.Output != nil {
		raw["output"] = *cost.Output
	}
	if cost.CacheRead != nil {
		raw["cache_read"] = *cost.CacheRead
	}
	if cost.CacheWrite != nil {
		raw["cache_write"] = *cost.CacheWrite
	}
	return raw
}

// mergeCostRawFields merges individual cost subfields from discovered into the raw
// cost map. Only fields present in discovered are written; existing fields not in
// discovered are preserved. User-provided cost fields are protected because user
// config is the highest-authority layer.
func mergeCostRawFields(existing map[string]any, discovered *Cost, protected map[string]bool) {
	if discovered.Input != nil && !protected["input"] {
		existing["input"] = *discovered.Input
	}
	if discovered.Output != nil && !protected["output"] {
		existing["output"] = *discovered.Output
	}
	if discovered.CacheRead != nil && !protected["cache_read"] {
		existing["cache_read"] = *discovered.CacheRead
	}
	if discovered.CacheWrite != nil && !protected["cache_write"] {
		existing["cache_write"] = *discovered.CacheWrite
	}
}

func userCostFields(userRaw map[string]any, modelID string) map[string]bool {
	protected := map[string]bool{}
	modelsRaw, _ := userRaw["models"].(map[string]any)
	modelRaw, _ := modelsRaw[modelID].(map[string]any)
	costRaw, _ := modelRaw["cost"].(map[string]any)
	for _, field := range []string{"input", "output", "cache_read", "cache_write"} {
		if _, ok := costRaw[field]; ok {
			protected[field] = true
		}
	}
	return protected
}

func rawToProvider(providerID string, raw map[string]any, bundledProvider bool) *Provider {
	transportRaw := raw["transport"].(map[string]any)
	protocolMetadata := effectiveProtocolMetadata(nil, raw["protocol_metadata"])
	provider := &Provider{
		ID:               stringValue(raw["id"], providerID),
		Name:             stringValue(raw["name"], providerID),
		Transport:        rawToTransport(transportRaw),
		SystemRole:       SystemRole(stringValue(raw["system_role"], string(RoleSystem))),
		UsageInStream:    boolValue(raw["usage_in_stream"], true),
		MaxTokensField:   stringValue(raw["max_tokens_field"], "max_tokens"),
		ExtraBody:        anyMapValue(raw["extra_body"]),
		Discovery:        boolValue(raw["discovery"], true),
		Hidden:           boolValue(raw["hidden"], false),
		ProtocolMetadata: protocolMetadata,
		Models:           map[string]*Model{},
		Builtin:          bundledProvider,
	}
	modelsRaw := raw["models"].(map[string]any)
	for modelID, value := range modelsRaw {
		modelRaw := value.(map[string]any)
		model := &Model{
			ID:               modelID,
			Name:             stringValue(modelRaw["name"], modelID),
			ContextWindow:    intValue(modelRaw["context_window"], 0),
			MaxOutputTokens:  intValue(modelRaw["max_output_tokens"], 0),
			InputModalities:  modalitiesValue(modelRaw["input_modalities"]),
			SystemRole:       SystemRole(stringValue(modelRaw["system_role"], string(provider.SystemRole))),
			UsageInStream:    boolValue(modelRaw["usage_in_stream"], provider.UsageInStream),
			Hidden:           boolValue(modelRaw["hidden"], false),
			ExtraBody:        anyMapValue(modelRaw["extra_body"]),
			Cost:             costValue(modelRaw["cost"]),
			ProtocolMetadata: effectiveProtocolMetadata(raw["protocol_metadata"], modelRaw["protocol_metadata"]),
		}
		provider.Models[modelID] = model
	}
	return provider
}

func rawToTransport(raw map[string]any) Transport {
	return Transport{
		BaseURL:   stringValue(raw["base_url"], ""),
		APIKeyEnv: stringValue(raw["api_key_env"], ""),
		Headers:   stringMapValue(raw["headers"]),
		Options:   anyMapValue(raw["options"]),
	}
}

func stringValue(v any, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fallback
}

func boolValue(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

func intValue(v any, fallback int) int {
	if i, ok := validationInt(v); ok {
		return i
	}
	return fallback
}

func anyMapValue(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return cloneJSONValue(m).(map[string]any)
	}
	return map[string]any{}
}

func stringMapValue(v any) map[string]string {
	out := map[string]string{}
	m, ok := v.(map[string]any)
	if !ok {
		return out
	}
	for key, value := range m {
		if s, ok := value.(string); ok {
			out[key] = s
		}
	}
	return out
}

func modalitiesValue(v any) []Modality {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return []Modality{ModalityText}
	}
	modalities := make([]Modality, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			modalities = append(modalities, Modality(s))
		}
	}
	if len(modalities) == 0 {
		return []Modality{ModalityText}
	}
	return modalities
}

func effectiveProtocolMetadata(providerValue, modelValue any) *ProtocolMetadata {
	providerRaw, _ := providerValue.(map[string]any)
	modelRaw, _ := modelValue.(map[string]any)
	if providerRaw == nil && modelRaw == nil {
		return nil
	}
	meta := &ProtocolMetadata{}
	if family, ok := providerRaw["family"].(string); ok && family != "" {
		meta.Family = family
	}
	if family, ok := modelRaw["family"].(string); ok && family != "" {
		meta.Family = family
	}
	if values, ok := stringSliceValue(providerRaw["must_preserve"]); ok {
		meta.MustPreserve = values
	}
	if values, ok := stringSliceValue(modelRaw["must_preserve"]); ok {
		meta.MustPreserve = values
	}
	if values, ok := stringSliceValue(providerRaw["drop"]); ok {
		meta.Drop = values
	}
	if values, ok := stringSliceValue(modelRaw["drop"]); ok {
		meta.Drop = values
	}
	if meta.Family == "" && len(meta.MustPreserve) == 0 && len(meta.Drop) == 0 {
		return nil
	}
	return meta
}

func cloneProtocolMetadata(meta *ProtocolMetadata) *ProtocolMetadata {
	if meta == nil {
		return nil
	}
	return &ProtocolMetadata{
		Family:       meta.Family,
		MustPreserve: append([]string(nil), meta.MustPreserve...),
		Drop:         append([]string(nil), meta.Drop...),
	}
}

func stringSliceValue(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			values = append(values, s)
		}
	}
	return values, true
}

func costValue(v any) *Cost {
	raw, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	cost := &Cost{}
	seen := false
	if f, ok := validationNumber(raw["input"]); ok {
		cost.Input = &f
		seen = true
	}
	if f, ok := validationNumber(raw["output"]); ok {
		cost.Output = &f
		seen = true
	}
	if f, ok := validationNumber(raw["cache_read"]); ok {
		cost.CacheRead = &f
		seen = true
	}
	if f, ok := validationNumber(raw["cache_write"]); ok {
		cost.CacheWrite = &f
		seen = true
	}
	if !seen {
		return nil
	}
	return cost
}
