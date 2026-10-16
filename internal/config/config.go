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
	"math"
	"os"
	"path/filepath"
	"time"
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

// SessionConfig governs the session lifecycle sweep. Defaults: auto_archive
// on, 7 days → archived, +7 days → deleted.
type SessionConfig struct {
	AutoArchive            bool `json:"auto_archive"`
	ArchiveAfterDays       int  `json:"archive_after_days"`
	DeleteAfterArchiveDays int  `json:"delete_after_archive_days"`
}

// ToolsConfig holds limits and timeouts for built-in tools.
type ToolsConfig struct {
	MaxOutputBytes         int `json:"max_output_bytes"`
	ReadMaxLines           int `json:"read_max_lines"`
	ReadLineMaxChars       int `json:"read_line_max_chars"`
	CommandTimeout         int `json:"command_timeout"`
	MaxBackgroundProcesses int `json:"max_background_processes"`
}

// Config is the decoded config file. Providers are retained as raw JSON-ish
// data so config consumers can round-trip the section, but catalog.Loader is
// the only code that interprets provider/model metadata.
type Config struct {
	Providers map[string]any `json:"providers,omitempty"`
	Sessions  SessionConfig  `json:"sessions"`
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

// maxSweepDays is the largest positive day count whose 24-hour duration
// conversion still fits an int64 nanosecond time.Duration.
const maxSweepDays = math.MaxInt64 / int64(24*time.Hour)

// ParseSessions decodes the raw JSON bytes of one config document's
// sessions section into the sweep policy. Absent (nil) or null input, and
// absent fields within an object, keep the defaults true/7/7. Malformed
// input, a non-object section, or a positive day count whose 24-hour
// duration conversion overflows is rejected with an error.
func ParseSessions(data []byte) (SessionConfig, error) {
	var raw *rawSessionConfig
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &raw); err != nil {
			return defaultSessionConfig(), err
		}
	}
	return sessionsFromRaw(raw)
}

// sessionsFromRaw applies the defaults and the fields present in one
// decoded sessions section. It is the shared session implementation used by
// both the whole-document Parse and ParseSessions.
func sessionsFromRaw(raw *rawSessionConfig) (SessionConfig, error) {
	c := defaultSessionConfig()
	if raw != nil {
		if raw.AutoArchive != nil {
			c.AutoArchive = *raw.AutoArchive
		}
		if raw.ArchiveAfterDays != nil {
			c.ArchiveAfterDays = *raw.ArchiveAfterDays
		}
		if raw.DeleteAfterArchiveDays != nil {
			c.DeleteAfterArchiveDays = *raw.DeleteAfterArchiveDays
		}
	}
	if err := checkSweepDays(c.ArchiveAfterDays, "archive_after_days"); err != nil {
		return c, err
	}
	if err := checkSweepDays(c.DeleteAfterArchiveDays, "delete_after_archive_days"); err != nil {
		return c, err
	}
	return c, nil
}

// checkSweepDays rejects a positive day count whose 24-hour duration
// conversion overflows; a nonpositive value disables its transition and
// needs no conversion.
func checkSweepDays(days int, field string) error {
	if days <= 0 || int64(days) <= maxSweepDays {
		return nil
	}
	return fmt.Errorf("sessions.%s: %d days overflows the 24-hour duration conversion", field, days)
}

// Parse parses and validates one config document without touching the
// filesystem: a single UseNumber decode keeps provider numbers exact and
// rejects trailing whole-document data, and the sessions member is decoded
// through the shared ParseSessions contract with its validation errors
// propagated. Provider raw shape is validated at the catalog, not here.
func Parse(data []byte) (*Config, error) {
	var doc struct {
		Providers map[string]any  `json:"providers"`
		Sessions  json.RawMessage `json:"sessions"`
	}
	if err := decodeJSONUseNumber(data, &doc); err != nil {
		return nil, err
	}
	sessions, err := ParseSessions(doc.Sessions)
	if err != nil {
		return nil, err
	}
	if doc.Providers == nil {
		doc.Providers = map[string]any{}
	}
	return &Config{Providers: doc.Providers, Sessions: sessions}, nil
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
