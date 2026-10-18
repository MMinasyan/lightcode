package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// The retained first-run skeleton the Runtime configuration service creates
// for the main config document.
const firstRunConfigSkeleton = `{
  "providers": {}
}
`

func TestParseAcceptsCatalogProviderEnvelopeAndIgnoresRemovedMembers(t *testing.T) {
	cfg, err := Parse([]byte(`{
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
}`))
	if err != nil {
		t.Fatalf("Parse returned error for the provider envelope: %v", err)
	}
	if _, ok := cfg.Providers["local"]; !ok {
		t.Fatalf("providers map did not retain local entry: %#v", cfg.Providers)
	}
}

func TestParseAcceptsEmptySkeleton(t *testing.T) {
	cfg, err := Parse([]byte(firstRunConfigSkeleton))
	if err != nil {
		t.Fatalf("Parse(empty skeleton) returned error: %v", err)
	}
	if cfg.Providers == nil || len(cfg.Providers) != 0 {
		t.Fatalf("Providers = %v, want empty map", cfg.Providers)
	}
	if !cfg.Sessions.AutoArchive || cfg.Sessions.ArchiveAfterDays != 7 {
		t.Fatalf("Sessions defaults not applied: %+v", cfg.Sessions)
	}
}

func TestParsePreservesExactProviderExtraBodyNumbers(t *testing.T) {
	cfg, err := Parse([]byte(` 
{
  "providers": {
    "local": {
      "transport": {
        "base_url": "http://localhost:11434/v1",
        "api_key_env": ""
      },
      "extra_body": {"large": 90071992547409931234567890}
    }
  }
}

`))
	if err != nil {
		t.Fatal(err)
	}
	providers := cfg.Providers["local"].(map[string]any)
	extraBody := providers["extra_body"].(map[string]any)
	if got, ok := extraBody["large"].(json.Number); !ok || got.String() != "90071992547409931234567890" {
		t.Fatalf("provider extra_body number = %#v (%T), want exact json.Number", extraBody["large"], extraBody["large"])
	}
}

func TestParseRejectsTrailingWholeDocumentData(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "second value", data: `{"providers": {}} {"extra": 1}`},
		{name: "junk", data: `{"providers": {}} junk`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.data)); err == nil {
				t.Fatal("Parse accepted trailing whole-document data")
			}
		})
	}
}

func TestParseRejectsMalformedWholeDocument(t *testing.T) {
	_, err := Parse([]byte(`{not json`))
	if err == nil {
		t.Fatal("Parse returned nil error, want the decode rejection")
	}
	if !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("error = %q, want substring %q", err.Error(), "invalid character")
	}
}

func TestParseSessionsSharedPolicy(t *testing.T) {
	for _, row := range []struct {
		name string
		data string
		want SessionConfig
	}{
		{name: "absent", data: "", want: SessionConfig{AutoArchive: true, ArchiveAfterDays: 7, DeleteAfterArchiveDays: 7}},
		{name: "null", data: "null", want: SessionConfig{AutoArchive: true, ArchiveAfterDays: 7, DeleteAfterArchiveDays: 7}},
		{name: "empty object", data: "{}", want: SessionConfig{AutoArchive: true, ArchiveAfterDays: 7, DeleteAfterArchiveDays: 7}},
		{name: "partial keeps defaults", data: `{"auto_archive":false}`, want: SessionConfig{AutoArchive: false, ArchiveAfterDays: 7, DeleteAfterArchiveDays: 7}},
		{name: "explicit", data: `{"auto_archive":true,"archive_after_days":30,"delete_after_archive_days":60}`, want: SessionConfig{AutoArchive: true, ArchiveAfterDays: 30, DeleteAfterArchiveDays: 60}},
		{name: "nonpositive retained", data: `{"archive_after_days":0,"delete_after_archive_days":-5}`, want: SessionConfig{AutoArchive: true, ArchiveAfterDays: 0, DeleteAfterArchiveDays: -5}},
		{name: "boundary days", data: `{"archive_after_days":106751}`, want: SessionConfig{AutoArchive: true, ArchiveAfterDays: 106751, DeleteAfterArchiveDays: 7}},
	} {
		t.Run(row.name, func(t *testing.T) {
			var data []byte
			if row.data != "" {
				data = []byte(row.data)
			}
			got, err := ParseSessions(data)
			if err != nil {
				t.Fatalf("ParseSessions(%s): %v", row.data, err)
			}
			if got != row.want {
				t.Fatalf("ParseSessions(%s) = %+v, want %+v", row.data, got, row.want)
			}
		})
	}

	for _, bad := range []string{
		`{"auto_archive":`, `{"archive_after_days":106752}`, `{"delete_after_archive_days":9223372036854775807}`,
		`[]`, `"7"`, `5`,
	} {
		if got, err := ParseSessions([]byte(bad)); err == nil {
			t.Fatalf("ParseSessions(%s) = %+v, want rejection", bad, got)
		}
	}
}

func TestParseSessionsSharesTheLegacyParseImplementation(t *testing.T) {
	for _, doc := range []string{
		`{"providers":{}}`,
		`{"providers":{},"sessions":null}`,
		`{"providers":{},"sessions":{}}`,
		`{"providers":{},"sessions":{"auto_archive":false,"archive_after_days":1,"delete_after_archive_days":2}}`,
		`{"providers":{},"sessions":{"archive_after_days":-3}}`,
	} {
		cfg, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("Parse(%s): %v", doc, err)
		}
		var members map[string]json.RawMessage
		if err := json.Unmarshal([]byte(doc), &members); err != nil {
			t.Fatal(err)
		}
		want, err := ParseSessions(members["sessions"])
		if err != nil {
			t.Fatalf("ParseSessions for %s: %v", doc, err)
		}
		if cfg.Sessions != want {
			t.Fatalf("Parse(%s) sessions = %+v, ParseSessions = %+v", doc, cfg.Sessions, want)
		}
	}
	// The whole-document entry shares the ParseSessions contract: its
	// validation errors are propagated instead of inventing defaults for a
	// malformed or overflowing sessions policy.
	doc := []byte(`{"providers":{},"sessions":{"archive_after_days":106752}}`)
	_, docErr := Parse(doc)
	var members map[string]json.RawMessage
	if err := json.Unmarshal(doc, &members); err != nil {
		t.Fatal(err)
	}
	_, sharedErr := ParseSessions(members["sessions"])
	if sharedErr == nil {
		t.Fatal("ParseSessions accepted the overflowed policy")
	}
	if docErr == nil {
		t.Fatalf("Parse of overflowed days = nil error, want the ParseSessions rejection %q", sharedErr)
	}
	if docErr.Error() != sharedErr.Error() {
		t.Fatalf("Parse error = %q, want the propagated ParseSessions error %q", docErr, sharedErr)
	}
}
