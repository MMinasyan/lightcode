// Package config loads the Lightcode config file and resolves the default
// model to a provider + API key at request time.
//
// Phase 1 uses plain JSON per DESIGN.md §5.4. Secrets are read from the
// environment at request time — never stored on disk, never logged.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/permission"
)

// ErrMissingEnvVar is returned (wrapped) by ResolveDefault when a provider's
// API key environment variable is required but unset. Callers can use
// errors.Is(err, ErrMissingEnvVar) to surface a friendlier hint that
// points at the .env file.
var ErrMissingEnvVar = errors.New("provider API key env var is unset")

// ErrEmptyConfig is returned (wrapped) by Load when the config file has
// no providers or no default_model set. The user must populate the file
// before lightcode can run.
var ErrEmptyConfig = errors.New("config is empty — providers and default_model must be set")

// ConfigPath returns the path where Lightcode expects its main config file.
// The user owns this file; it is auto-created as an empty skeleton on first
// run if it does not exist.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lightcode", "config.json"), nil
}

// emptyConfigTemplate is written to ~/.lightcode/config.json the first
// time Lightcode runs. It is a valid but empty skeleton — the user must
// fill it in with their own providers and default model.
const emptyConfigTemplate = `{
  "providers": {},
  "default_model": { "provider": "", "model": "" }
}
`

// Provider is one entry in the "providers" map: a base URL, the env var
// holding its API key, and the list of model IDs available under it.
type Provider struct {
	BaseURL        string         `json:"base_url"`
	APIKeyEnv      string         `json:"api_key_env"`
	Models         []string       `json:"models"`
	ContextWindows map[string]int `json:"context_windows,omitempty"`
}

// ModelRef uniquely identifies a model by the (provider, model) tuple.
// The same model string can legitimately appear under multiple providers,
// so neither field alone is a stable identifier.
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// SessionConfig governs the session lifecycle sweep. Defaults: auto_archive
// on, 7 days → archived, +7 days → deleted.
type SessionConfig struct {
	AutoArchive            bool `json:"auto_archive"`
	ArchiveAfterDays       int  `json:"archive_after_days"`
	DeleteAfterArchiveDays int  `json:"delete_after_archive_days"`
}

// CompactionConfig controls context lifecycle management.
type CompactionConfig struct {
	Enabled            bool    `json:"enabled"`
	ThresholdPct       float64 `json:"threshold_pct"`
	SummarizerProvider string  `json:"summarizer_provider,omitempty"`
	SummarizerModel    string  `json:"summarizer_model,omitempty"`
}

// SubagentsConfig controls subagent orchestration.
type SubagentsConfig struct {
	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
}

// Config is the full config file.
type Config struct {
	Providers    map[string]Provider  `json:"providers"`
	DefaultModel ModelRef             `json:"default_model"`
	Sessions     SessionConfig        `json:"sessions"`
	Compaction   CompactionConfig     `json:"compaction,omitempty"`
	Subagents    SubagentsConfig      `json:"subagents,omitempty"`
	Permissions  permission.Rules     `json:"permissions,omitempty"`
}

// rawSessionConfig is used to detect which sessions fields were
// present in the on-disk JSON, so absent fields get defaults rather
// than zero values (e.g., 0 days would archive sessions instantly).
type rawSessionConfig struct {
	AutoArchive            *bool `json:"auto_archive"`
	ArchiveAfterDays       *int  `json:"archive_after_days"`
	DeleteAfterArchiveDays *int  `json:"delete_after_archive_days"`
}

// defaultSessionConfig returns the fallback sessions policy.
func defaultSessionConfig() SessionConfig {
	return SessionConfig{
		AutoArchive:            true,
		ArchiveAfterDays:       7,
		DeleteAfterArchiveDays: 7,
	}
}

type rawCompactionConfig struct {
	Enabled            *bool    `json:"enabled"`
	ThresholdPct       *float64 `json:"threshold_pct"`
	SummarizerProvider *string  `json:"summarizer_provider"`
	SummarizerModel    *string  `json:"summarizer_model"`
}

func defaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		Enabled:      true,
		ThresholdPct: 0.90,
	}
}

// Load reads and parses the config file at path. If the file does not
// exist, Load creates it with an empty skeleton and returns ErrEmptyConfig
// so the caller can show the user what to put in it. If the file exists
// but has no providers or no default_model, ErrEmptyConfig is returned too.
func Load(path string) (*Config, error) {
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create config dir: %w", err)
		}
		if err := os.WriteFile(path, []byte(emptyConfigTemplate), 0o600); err != nil {
			return nil, fmt.Errorf("create config %s: %w", path, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	type rawSubagentsConfig struct {
		MaxConcurrent *int    `json:"max_concurrent"`
		Provider      *string `json:"provider"`
		Model         *string `json:"model"`
	}

	// Apply sessions defaults for any absent fields.
	var raw struct {
		Sessions   *rawSessionConfig   `json:"sessions"`
		Compaction *rawCompactionConfig `json:"compaction"`
		Subagents  *rawSubagentsConfig  `json:"subagents"`
	}
	_ = json.Unmarshal(data, &raw)
	c.Sessions = defaultSessionConfig()
	if raw.Sessions != nil {
		if raw.Sessions.AutoArchive != nil {
			c.Sessions.AutoArchive = *raw.Sessions.AutoArchive
		}
		if raw.Sessions.ArchiveAfterDays != nil {
			c.Sessions.ArchiveAfterDays = *raw.Sessions.ArchiveAfterDays
		}
		if raw.Sessions.DeleteAfterArchiveDays != nil {
			c.Sessions.DeleteAfterArchiveDays = *raw.Sessions.DeleteAfterArchiveDays
		}
	}
	c.Compaction = defaultCompactionConfig()
	if raw.Compaction != nil {
		if raw.Compaction.Enabled != nil {
			c.Compaction.Enabled = *raw.Compaction.Enabled
		}
		if raw.Compaction.ThresholdPct != nil {
			c.Compaction.ThresholdPct = *raw.Compaction.ThresholdPct
		}
		if raw.Compaction.SummarizerProvider != nil {
			c.Compaction.SummarizerProvider = *raw.Compaction.SummarizerProvider
		}
		if raw.Compaction.SummarizerModel != nil {
			c.Compaction.SummarizerModel = *raw.Compaction.SummarizerModel
		}
	}

	c.Subagents = SubagentsConfig{MaxConcurrent: 4}
	if raw.Subagents != nil {
		if raw.Subagents.MaxConcurrent != nil {
			c.Subagents.MaxConcurrent = *raw.Subagents.MaxConcurrent
		}
		if raw.Subagents.Provider != nil {
			c.Subagents.Provider = *raw.Subagents.Provider
		}
		if raw.Subagents.Model != nil {
			c.Subagents.Model = *raw.Subagents.Model
		}
	}

	if len(c.Providers) == 0 || c.DefaultModel.Provider == "" || c.DefaultModel.Model == "" {
		return &c, fmt.Errorf("%w: %s", ErrEmptyConfig, path)
	}

	return &c, nil
}

// ResolveDefault looks up the default model, validates it against the
// provider's model list, and reads the API key from the environment.
// Returns the provider config, the model ID to pass to the API, and the
// resolved API key.
func (c *Config) ResolveDefault() (Provider, string, string, error) {
	ref := c.DefaultModel

	prov, ok := c.Providers[ref.Provider]
	if !ok {
		return Provider{}, "", "", fmt.Errorf("default_model.provider %q is not defined in providers", ref.Provider)
	}

	found := false
	for _, m := range prov.Models {
		if m == ref.Model {
			found = true
			break
		}
	}
	if !found {
		return Provider{}, "", "", fmt.Errorf("default_model.model %q is not listed under provider %q", ref.Model, ref.Provider)
	}

	var apiKey string
	if prov.APIKeyEnv != "" {
		apiKey = os.Getenv(prov.APIKeyEnv)
		if apiKey == "" {
			return Provider{}, "", "", fmt.Errorf("%w: %s (for provider %q)", ErrMissingEnvVar, prov.APIKeyEnv, ref.Provider)
		}
	}

	return prov, ref.Model, apiKey, nil
}
