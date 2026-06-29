package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfigForTest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAcceptsCatalogProviderEnvelopeAndIgnoresRemovedModelFields(t *testing.T) {
	path := writeConfigForTest(t, `{
  "providers": {
    "local": {
      "name": "Local Test",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LOCAL_TEST_KEY" },
      "models": {
        "chat": { "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "local/chat",
  "compaction": {
    "enabled": false,
    "threshold_pct": 0.75,
    "summarizer_model": "local/chat"
  },
  "subagents": {
    "max_concurrent": 2,
    "model": { "provider": "local", "model": "chat" }
  }
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error for new schema: %v", err)
	}
	if cfg.Compaction.Enabled {
		t.Fatal("Compaction.Enabled did not preserve explicit false")
	}
	if cfg.Compaction.ThresholdPct != 0.75 {
		t.Fatalf("ThresholdPct = %v", cfg.Compaction.ThresholdPct)
	}
	if cfg.Subagents.MaxConcurrent != 2 {
		t.Fatalf("Subagents = %#v, want max_concurrent=2 and removed model ignored", cfg.Subagents)
	}
	if _, ok := cfg.Providers["local"]; !ok {
		t.Fatalf("providers map did not retain local entry: %#v", cfg.Providers)
	}
}

func TestLoadRejectsOldShapeWithClearErrors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "compaction summarizer provider",
			body: `{
  "providers": {},
  "default_model": "openai/gpt-5.4-mini",
  "compaction": { "summarizer_provider": "openai", "summarizer_model": "gpt-5.4-mini" }
}`,
			wantErr: "compaction.summarizer_provider is no longer supported",
		},
		{
			name: "subagent provider",
			body: `{
  "providers": {},
  "default_model": "openai/gpt-5.4-mini",
  "subagents": { "provider": "openai", "model": "gpt-5.4-mini" }
}`,
			wantErr: "subagents.provider is no longer supported",
		},
		{
			name: "provider models array",
			body: `{
  "providers": {
    "openai": { "base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY", "models": ["gpt-5.4-mini"] }
  },
  "default_model": "openai/gpt-5.4-mini"
}`,
			wantErr: "providers.openai.models must be an object",
		},
		{
			name: "context windows map",
			body: `{
  "providers": {
    "openai": { "models": { "gpt-5.4-mini": {} }, "context_windows": { "gpt-5.4-mini": 128000 } }
  },
  "default_model": "openai/gpt-5.4-mini"
}`,
			wantErr: "providers.openai.context_windows is no longer supported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigForTest(t, tc.body))
			if err == nil {
				t.Fatalf("Load returned nil error, want %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadMissingFileCreatesEmptyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent", "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load missing file returned error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load returned nil config")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("Config file was not created: %v", readErr)
	}
	if strings.Contains(string(data), "default_model") {
		t.Fatalf("Created config unexpectedly contains default_model field: %s", data)
	}
}

func TestParseAcceptsEmptySkeleton(t *testing.T) {
	cfg, err := Parse([]byte(emptyConfigTemplate))
	if err != nil {
		t.Fatalf("Parse(empty skeleton) returned error: %v", err)
	}
	if cfg.Providers == nil || len(cfg.Providers) != 0 {
		t.Fatalf("Providers = %v, want empty map", cfg.Providers)
	}
	if !cfg.Sessions.AutoArchive || cfg.Sessions.ArchiveAfterDays != 7 {
		t.Fatalf("Sessions defaults not applied: %+v", cfg.Sessions)
	}
	if !cfg.Compaction.Enabled || cfg.Compaction.ThresholdPct != 0.90 {
		t.Fatalf("Compaction defaults not applied: %+v", cfg.Compaction)
	}
}

func TestParseRejectsOldShapes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "context windows map",
			body:    `{"providers": {"openai": {"models": {"gpt-5.4-mini": {}}, "context_windows": {"gpt-5.4-mini": 128000}}}, "default_model": "openai/gpt-5.4-mini"}`,
			wantErr: "providers.openai.context_windows is no longer supported",
		},
		{
			name:    "invalid json",
			body:    `{not json`,
			wantErr: "invalid character",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if err == nil {
				t.Fatalf("Parse returned nil error, want %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}
