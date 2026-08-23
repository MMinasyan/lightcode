// Package config loads the Lightcode config file and resolves process-level
// settings. Provider/model metadata is owned by internal/catalog and
// internal/agents; this package keeps only non-model runtime sections.
//
// Secrets are env vars. They are loaded at startup from ~/.lightcode/.env (if
// present) or inherited from the shell. They are never written to config.json
// and never logged.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/permission"
)

// ErrMissingEnvVar is returned (wrapped) when a provider's API key environment
// variable is required but unset. Callers can use errors.Is(err,
// ErrMissingEnvVar) to surface a friendlier hint that points at the .env file.
var ErrMissingEnvVar = errors.New("provider API key env var is unset")

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

// ResolvePath returns the config file path the process should use: the
// LIGHTCODE_CONFIG override when set, otherwise ConfigPath(). It only
// resolves the path — it never creates or reads the file.
func ResolvePath() (string, error) {
	if path := os.Getenv("LIGHTCODE_CONFIG"); path != "" {
		return path, nil
	}
	return ConfigPath()
}

// emptyConfigTemplate is written to ~/.lightcode/config.json the first time
// Lightcode runs. It is a valid but empty skeleton.
const emptyConfigTemplate = `{
  "providers": {}
}
`

// SessionConfig governs the session lifecycle sweep. Defaults: auto_archive
// on, 7 days → archived, +7 days → deleted.
type SessionConfig struct {
	AutoArchive            bool `json:"auto_archive"`
	ArchiveAfterDays       int  `json:"archive_after_days"`
	DeleteAfterArchiveDays int  `json:"delete_after_archive_days"`
}

// CompactionConfig controls context lifecycle management.
type CompactionConfig struct {
	Enabled      bool    `json:"enabled"`
	ThresholdPct float64 `json:"threshold_pct"`
}

// SubagentsConfig controls subagent orchestration.
type SubagentsConfig struct {
	MaxConcurrent int `json:"max_concurrent,omitempty"`
}

// ToolsConfig holds limits and timeouts for built-in tools.
type ToolsConfig struct {
	MaxOutputBytes         int `json:"max_output_bytes"`
	ReadMaxLines           int `json:"read_max_lines"`
	ReadLineMaxChars       int `json:"read_line_max_chars"`
	CommandTimeout         int `json:"command_timeout"`
	MaxBackgroundProcesses int `json:"max_background_processes"`
}

func defaultToolsConfig() ToolsConfig {
	return ToolsConfig{
		MaxOutputBytes:         15360,
		ReadMaxLines:           500,
		ReadLineMaxChars:       5000,
		CommandTimeout:         120,
		MaxBackgroundProcesses: 10,
	}
}

// Config is the full config file. Providers are retained as raw JSON-ish data
// so config consumers can round-trip the section, but catalog.Loader is the
// only code that interprets provider/model metadata.
type Config struct {
	Providers   map[string]any   `json:"providers,omitempty"`
	Sessions    SessionConfig    `json:"sessions"`
	Compaction  CompactionConfig `json:"compaction,omitempty"`
	Subagents   SubagentsConfig  `json:"subagents,omitempty"`
	Permissions permission.Rules `json:"permissions,omitempty"`
	Tools       ToolsConfig      `json:"tools,omitempty"`
}

// rawSessionConfig is used to detect which sessions fields were present in the
// on-disk JSON, so absent fields get defaults rather than zero values.
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
	Enabled      *bool    `json:"enabled"`
	ThresholdPct *float64 `json:"threshold_pct"`
}

func defaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		Enabled:      true,
		ThresholdPct: 0.90,
	}
}

// Load reads and parses the config file at path. If the file does not exist,
// Load creates it with an empty skeleton (a valid config with no
// provider model) and parses that. Old provider catalog shapes are rejected
// with explicit cutover errors; no migration is attempted.
func Load(path string) (*Config, error) {
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		if _, err := atomicfs.CreateExclusive(path, []byte(emptyConfigTemplate), 0o600); err != nil {
			return nil, fmt.Errorf("create config %s: %w", path, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return c, nil
}

// Parse parses and validates config bytes without touching the filesystem.
// It accepts the empty first-run skeleton and rejects the same old provider
// catalog shapes Load rejects.
func Parse(data []byte) (*Config, error) {
	if err := rejectOldShape(data); err != nil {
		return nil, err
	}

	var c Config
	if err := decodeJSONUseNumber(data, &c); err != nil {
		return nil, err
	}
	if c.Providers == nil {
		c.Providers = map[string]any{}
	}

	type rawSubagentsConfig struct {
		MaxConcurrent *int `json:"max_concurrent"`
	}

	var raw struct {
		Sessions   *rawSessionConfig    `json:"sessions"`
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
	}

	c.Subagents = SubagentsConfig{MaxConcurrent: 4}
	if raw.Subagents != nil {
		if raw.Subagents.MaxConcurrent != nil {
			c.Subagents.MaxConcurrent = *raw.Subagents.MaxConcurrent
		}
	}

	type rawToolsConfig struct {
		MaxOutputBytes         *int `json:"max_output_bytes"`
		ReadMaxLines           *int `json:"read_max_lines"`
		ReadLineMaxChars       *int `json:"read_line_max_chars"`
		CommandTimeout         *int `json:"command_timeout"`
		MaxBackgroundProcesses *int `json:"max_background_processes"`
	}
	var rawToolsRoot struct {
		Tools *rawToolsConfig `json:"tools"`
	}
	_ = json.Unmarshal(data, &rawToolsRoot)
	c.Tools = defaultToolsConfig()
	if rawToolsRoot.Tools != nil {
		if rawToolsRoot.Tools.MaxOutputBytes != nil {
			c.Tools.MaxOutputBytes = *rawToolsRoot.Tools.MaxOutputBytes
		}
		if rawToolsRoot.Tools.ReadMaxLines != nil {
			c.Tools.ReadMaxLines = *rawToolsRoot.Tools.ReadMaxLines
		}
		if rawToolsRoot.Tools.ReadLineMaxChars != nil {
			c.Tools.ReadLineMaxChars = *rawToolsRoot.Tools.ReadLineMaxChars
		}
		if rawToolsRoot.Tools.CommandTimeout != nil {
			c.Tools.CommandTimeout = *rawToolsRoot.Tools.CommandTimeout
		}
		if rawToolsRoot.Tools.MaxBackgroundProcesses != nil {
			c.Tools.MaxBackgroundProcesses = *rawToolsRoot.Tools.MaxBackgroundProcesses
		}
	}

	return &c, nil
}

func decodeJSONUseNumber(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectOldShape(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}

	if raw, ok := root["compaction"]; ok {
		var compaction map[string]json.RawMessage
		if err := json.Unmarshal(raw, &compaction); err == nil && compaction != nil {
			if _, exists := compaction["summarizer_provider"]; exists {
				return fmt.Errorf("compaction.summarizer_provider is no longer supported; compaction model selection now lives in agents.json")
			}
		}
	}

	if raw, ok := root["subagents"]; ok {
		var subagents map[string]json.RawMessage
		if err := json.Unmarshal(raw, &subagents); err == nil && subagents != nil {
			if _, exists := subagents["provider"]; exists {
				return fmt.Errorf("subagents.provider is no longer supported; task agent model selection now lives in agents.json")
			}
		}
	}

	if raw, ok := root["providers"]; ok {
		var providers map[string]json.RawMessage
		if err := json.Unmarshal(raw, &providers); err == nil {
			for providerID, providerRaw := range providers {
				var provider map[string]json.RawMessage
				if err := json.Unmarshal(providerRaw, &provider); err != nil || provider == nil {
					continue
				}
				if _, exists := provider["context_windows"]; exists {
					return fmt.Errorf("providers.%s.context_windows is no longer supported; put context_window on each model entry", providerID)
				}
				if modelsRaw, exists := provider["models"]; exists && !isJSONObject(modelsRaw) {
					return fmt.Errorf("providers.%s.models must be an object keyed by model id, not the old array shape", providerID)
				}
			}
		}
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{")
}
