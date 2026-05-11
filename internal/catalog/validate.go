package catalog

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidationError describes a catalog validation failure.
type ValidationError struct {
	Provider string
	Model    string
	Field    string
	Reason   string
}

// Error returns a human-readable validation error.
func (e ValidationError) Error() string {
	where := e.Provider
	if e.Model != "" {
		where += "/" + e.Model
	}
	if where == "" {
		where = "catalog"
	}
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", where, e.Reason)
	}
	return fmt.Sprintf("%s: %s: %s", where, e.Field, e.Reason)
}

// ValidateRaw checks a raw provider envelope.
func ValidateRaw(provID string, raw map[string]any, strict bool) []ValidationError {
	var errs []ValidationError
	add := func(model, field, reason string) {
		errs = append(errs, ValidationError{Provider: provID, Model: model, Field: field, Reason: reason})
	}
	if provID == "" {
		add("", "provider", "provider id is required")
	} else if strings.Contains(provID, "/") {
		add("", "provider", "provider id cannot contain slash")
	}
	if raw == nil {
		add("", "", "provider must be an object")
		return errs
	}
	if strict {
		for key := range raw {
			if !allowedRawProviderKey(key) {
				add("", key, "unknown provider field")
			}
		}
	}
	if v, ok := raw["id"]; ok {
		id, ok := v.(string)
		if !ok {
			add("", "id", "must be a string")
		} else if id == "" {
			add("", "id", "must not be empty")
		} else if strings.Contains(id, "/") {
			add("", "id", "cannot contain slash")
		} else if provID != "" && id != provID {
			add("", "id", "must match provider key")
		}
	}
	if v, ok := raw["name"]; ok {
		if _, ok := v.(string); !ok {
			add("", "name", "must be a string")
		}
	}

	transport, ok := validationObject(raw["transport"])
	if !ok {
		add("", "transport", "must be an object")
	} else {
		validateRawTransport(add, transport)
	}
	validateRawSystemRole(add, "", "system_role", raw["system_role"])
	validateRawBool(add, "", "usage_in_stream", raw["usage_in_stream"])
	validateRawMaxTokensField(add, raw["max_tokens_field"])
	validateRawBool(add, "", "discovery", raw["discovery"])
	validateRawBool(add, "", "hidden", raw["hidden"])
	validateRawExtraBody(add, "", "extra_body", raw["extra_body"])

	if modelsVal, exists := raw["models"]; exists {
		models, ok := validationObject(modelsVal)
		if !ok {
			add("", "models", "must be an object")
		} else {
			for modelID, value := range models {
				modelRaw, ok := validationObject(value)
				if modelID == "" {
					add("", "models", "model id must not be empty")
				}
				if !ok {
					add(modelID, "models."+modelID, "model must be an object")
					continue
				}
				validateRawModel(add, modelID, modelRaw, strict)
			}
		}
	}
	return errs
}

// ValidateEffective checks a resolved provider and its models.
func ValidateEffective(p *Provider) []ValidationError {
	var errs []ValidationError
	providerID := ""
	if p != nil {
		providerID = p.ID
	}
	add := func(model, field, reason string) {
		errs = append(errs, ValidationError{Provider: providerID, Model: model, Field: field, Reason: reason})
	}
	if p == nil {
		add("", "", "provider is nil")
		return errs
	}
	if p.ID == "" {
		add("", "id", "must not be empty")
	} else if strings.Contains(p.ID, "/") {
		add("", "id", "cannot contain slash")
	}
	if p.Name == "" {
		add("", "name", "must not be empty")
	}
	validateURL(add, "", "transport.base_url", p.Transport.BaseURL)
	if p.Transport.APIKeyEnv != "" && !envNamePattern.MatchString(p.Transport.APIKeyEnv) {
		add("", "transport.api_key_env", "must be an environment variable name")
	}
	if !validSystemRole(p.SystemRole) {
		add("", "system_role", "must be system, user, or developer")
	}
	if p.MaxTokensField != "" && !validMaxTokensField(p.MaxTokensField) {
		add("", "max_tokens_field", "must be max_tokens or max_completion_tokens")
	}
	for _, key := range CheckReservedKeys(p.ExtraBody) {
		add("", "extra_body."+key, "reserved request body key")
	}
	for modelID, model := range p.Models {
		if model == nil {
			add(modelID, "models."+modelID, "model is nil")
			continue
		}
		if model.ID == "" {
			add(modelID, "models."+modelID+".id", "must not be empty")
		}
		if model.Name == "" {
			add(modelID, "models."+modelID+".name", "must not be empty")
		}
		if !validSystemRole(model.SystemRole) {
			add(modelID, "models."+modelID+".system_role", "must be system, user, or developer")
		}
		for i, modality := range model.InputModalities {
			if !validModality(modality) {
				add(modelID, fmt.Sprintf("models.%s.input_modalities[%d]", modelID, i), "unknown modality")
			}
		}
		for _, key := range CheckReservedKeys(model.ExtraBody) {
			add(modelID, "models."+modelID+".extra_body."+key, "reserved request body key")
		}
	}
	return errs
}

func validateRawTransport(add func(string, string, string), transport map[string]any) {
	for key := range transport {
		if key != "base_url" && key != "api_key_env" && key != "headers" && key != "options" {
			add("", "transport."+key, "unknown transport field")
		}
	}
	baseURL, ok := transport["base_url"].(string)
	if !ok || baseURL == "" {
		add("", "transport.base_url", "must be a non-empty string")
	} else {
		validateURL(add, "", "transport.base_url", baseURL)
	}
	apiKeyEnv, ok := transport["api_key_env"]
	if !ok {
		add("", "transport.api_key_env", "is required")
	} else if s, ok := apiKeyEnv.(string); !ok {
		add("", "transport.api_key_env", "must be a string")
	} else if s != "" && !envNamePattern.MatchString(s) {
		add("", "transport.api_key_env", "must be an environment variable name")
	}
	if headers, ok := transport["headers"]; ok {
		m, ok := validationObject(headers)
		if !ok {
			add("", "transport.headers", "must be an object")
		} else {
			for key, value := range m {
				if _, ok := value.(string); !ok {
					add("", "transport.headers."+key, "must be a string")
				}
			}
		}
	}
	if options, ok := transport["options"]; ok {
		if _, ok := validationObject(options); !ok {
			add("", "transport.options", "must be an object")
		}
	}
}

func validateRawModel(add func(string, string, string), modelID string, raw map[string]any, strict bool) {
	if strict {
		for key := range raw {
			if !allowedRawModelKey(key) {
				add(modelID, "models."+modelID+"."+key, "unknown model field")
			}
		}
	}
	if v, ok := raw["name"]; ok {
		if _, ok := v.(string); !ok {
			add(modelID, "models."+modelID+".name", "must be a string")
		}
	}
	if v, ok := raw["context_window"]; ok {
		if _, ok := validationInt(v); !ok {
			add(modelID, "models."+modelID+".context_window", "must be a non-negative integer")
		}
	}
	if v, ok := raw["max_output_tokens"]; ok {
		if _, ok := validationInt(v); !ok {
			add(modelID, "models."+modelID+".max_output_tokens", "must be a non-negative integer")
		}
	}
	validateRawModalities(add, modelID, raw["input_modalities"])
	validateRawSystemRole(add, modelID, "models."+modelID+".system_role", raw["system_role"])
	validateRawBool(add, modelID, "models."+modelID+".usage_in_stream", raw["usage_in_stream"])
	validateRawBool(add, modelID, "models."+modelID+".hidden", raw["hidden"])
	validateRawExtraBody(add, modelID, "models."+modelID+".extra_body", raw["extra_body"])
	validateRawCost(add, modelID, raw["cost"])
}

func validateRawSystemRole(add func(string, string, string), model, field string, value any) {
	if value == nil {
		return
	}
	s, ok := value.(string)
	if !ok {
		add(model, field, "must be a string")
		return
	}
	if !validSystemRole(SystemRole(s)) {
		add(model, field, "must be system, user, or developer")
	}
}

func validateRawMaxTokensField(add func(string, string, string), value any) {
	if value == nil {
		return
	}
	s, ok := value.(string)
	if !ok || !validMaxTokensField(s) {
		add("", "max_tokens_field", "must be max_tokens or max_completion_tokens")
	}
}

func validateRawBool(add func(string, string, string), model, field string, value any) {
	if value == nil {
		return
	}
	if _, ok := value.(bool); !ok {
		add(model, field, "must be a bool")
	}
}

func validateRawModalities(add func(string, string, string), model string, value any) {
	if value == nil {
		return
	}
	items, ok := value.([]any)
	if !ok {
		add(model, "models."+model+".input_modalities", "must be an array")
		return
	}
	for i, item := range items {
		s, ok := item.(string)
		if !ok || !validModality(Modality(s)) {
			add(model, fmt.Sprintf("models.%s.input_modalities[%d]", model, i), "unknown modality")
		}
	}
}

func validateRawExtraBody(add func(string, string, string), model, field string, value any) {
	if value == nil {
		return
	}
	body, ok := validationObject(value)
	if !ok {
		add(model, field, "must be an object")
		return
	}
	for _, key := range CheckReservedKeys(body) {
		add(model, field+"."+key, "reserved request body key")
	}
}

func validateRawCost(add func(string, string, string), model string, value any) {
	if value == nil {
		return
	}
	cost, ok := validationObject(value)
	if !ok {
		add(model, "models."+model+".cost", "must be an object")
		return
	}
	for key, v := range cost {
		if key != "input" && key != "output" && key != "cache_read" && key != "cache_write" {
			add(model, "models."+model+".cost."+key, "unknown cost field")
			continue
		}
		if _, ok := validationNumber(v); !ok {
			add(model, "models."+model+".cost."+key, "must be a number")
		}
	}
}

func validateURL(add func(string, string, string), model, field, raw string) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		add(model, field, "must be an absolute URL")
	}
}

func allowedRawProviderKey(key string) bool {
	switch key {
	case "id", "name", "transport", "system_role", "usage_in_stream", "max_tokens_field", "extra_body", "discovery", "hidden", "models":
		return true
	default:
		return false
	}
}

func allowedRawModelKey(key string) bool {
	switch key {
	case "name", "context_window", "max_output_tokens", "input_modalities", "system_role", "usage_in_stream", "extra_body", "cost", "hidden":
		return true
	default:
		return false
	}
}

func validSystemRole(role SystemRole) bool {
	switch role {
	case RoleSystem, RoleUser, RoleDeveloper:
		return true
	default:
		return false
	}
}

func validMaxTokensField(field string) bool {
	switch field {
	case "max_tokens", "max_completion_tokens":
		return true
	default:
		return false
	}
}

func validModality(modality Modality) bool {
	switch modality {
	case ModalityText, ModalityImage, ModalityAudio, ModalityDocument:
		return true
	default:
		return false
	}
}

func validationObject(v any) (map[string]any, bool) {
	if v == nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

func validationInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, n >= 0
	case int64:
		if n < 0 {
			return 0, false
		}
		return int(n), true
	case float64:
		if n < 0 || n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

func validationNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
