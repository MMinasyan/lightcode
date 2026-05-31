// Package catalog builds and validates Lightcode's provider/model catalog.
package catalog

import "github.com/MMinasyan/lightcode/internal/coremodel"

// ErrInvalidModelRef is returned when a provider-prefixed model reference is malformed.
var ErrInvalidModelRef = coremodel.ErrInvalidModelRef

// ModelRef is the canonical internal identity of a model.
type ModelRef = coremodel.ModelRef

// ParseModelRef parses a provider-prefixed model reference.
func ParseModelRef(s string) (ModelRef, error) {
	return coremodel.Parse(s)
}

// SystemRole is the request role used for the system prompt.
type SystemRole string

const (
	// RoleSystem uses the OpenAI system role.
	RoleSystem SystemRole = "system"
	// RoleUser uses the user role for system-prompt content.
	RoleUser SystemRole = "user"
	// RoleDeveloper uses the OpenAI developer role.
	RoleDeveloper SystemRole = "developer"
)

// Modality is an input modality supported by a model.
type Modality string

const (
	// ModalityText is plain text input.
	ModalityText Modality = "text"
	// ModalityImage is image input.
	ModalityImage Modality = "image"
	// ModalityAudio is audio input.
	ModalityAudio Modality = "audio"
	// ModalityDocument is document input.
	ModalityDocument Modality = "document"
)

// Transport holds provider HTTP-construction config.
type Transport struct {
	BaseURL   string            `json:"base_url"`
	APIKeyEnv string            `json:"api_key_env"`
	Headers   map[string]string `json:"headers,omitempty"`
	Options   map[string]any    `json:"options,omitempty"`
}

// Cost is per-million-token USD pricing.
type Cost struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

// ProtocolMetadata describes provider-specific protocol-state policy.
type ProtocolMetadata struct {
	Family       string   `json:"family,omitempty"`
	MustPreserve []string `json:"must_preserve,omitempty"`
	Drop         []string `json:"drop,omitempty"`
}

// Model is a resolved model entry in the effective catalog.
type Model struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	ContextWindow    int               `json:"context_window"`
	MaxOutputTokens  int               `json:"max_output_tokens"`
	InputModalities  []Modality        `json:"input_modalities"`
	SystemRole       SystemRole        `json:"system_role"`
	UsageInStream    bool              `json:"usage_in_stream"`
	Hidden           bool              `json:"hidden,omitempty"`
	ExtraBody        map[string]any    `json:"extra_body,omitempty"`
	Cost             *Cost             `json:"cost,omitempty"`
	ProtocolMetadata *ProtocolMetadata `json:"protocol_metadata,omitempty"`
}

// Provider is a resolved provider entry in the effective catalog.
type Provider struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Transport        Transport         `json:"transport"`
	SystemRole       SystemRole        `json:"system_role"`
	UsageInStream    bool              `json:"usage_in_stream"`
	MaxTokensField   string            `json:"max_tokens_field"`
	ExtraBody        map[string]any    `json:"extra_body,omitempty"`
	Discovery        bool              `json:"discovery"`
	Hidden           bool              `json:"hidden,omitempty"`
	ProtocolMetadata *ProtocolMetadata `json:"protocol_metadata,omitempty"`
	Models           map[string]*Model `json:"models"`
	Builtin          bool              `json:"-"`
}

// Catalog is the in-memory effective catalog.
type Catalog struct {
	Providers map[string]*Provider
}
