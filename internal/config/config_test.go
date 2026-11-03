package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.NoError(t, err)
	{
		_, ok := cfg.Providers["local"]
		testassert.Require(t, ok, "providers map did not retain local entry: %#v", cfg.Providers)
	}
}

func TestParseAcceptsEmptySkeleton(t *testing.T) {
	cfg, err := Parse([]byte(firstRunConfigSkeleton))
	testassert.NoError(t, err)
	testassert.Require(t, cfg.Providers != nil && len(cfg.Providers) == 0 && cfg.Sessions.AutoArchive && cfg.Sessions.ArchiveAfterDays == 7, "Providers = %v, want empty map; Sessions defaults not applied: %+v", cfg.Providers, cfg.Sessions)
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
	testassert.NoError(t, err)
	providers := cfg.Providers["local"].(map[string]any)
	extraBody := providers["extra_body"].(map[string]any)
	{
		got, ok := extraBody["large"].(json.Number)
		testassert.Require(t, ok && got.String() == "90071992547409931234567890", "provider extra_body number = %#v (%T), want exact json.Number", extraBody["large"], extraBody["large"])
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
			{
				_, err := Parse([]byte(tc.data))
				testassert.Require(t, err != nil, "Parse accepted trailing whole-document data")
			}
		})
	}
}

func TestParseRejectsMalformedWholeDocument(t *testing.T) {
	_, err := Parse([]byte(`{not json`))
	testassert.Require(t, err != nil, "Parse returned nil error, want the decode rejection")
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
			testassert.Require(t, err == nil, "ParseSessions(%s): %v", row.data, err)
			testassert.Require(t, got == row.want, "ParseSessions(%s) = %+v, want %+v", row.data, got, row.want)
		})
	}

	for _, bad := range []string{
		`{"auto_archive":`, `{"archive_after_days":106752}`, `{"delete_after_archive_days":9223372036854775807}`,
		`[]`, `"7"`, `5`,
	} {
		{
			got, err := ParseSessions([]byte(bad))
			testassert.Require(t, err != nil, "ParseSessions(%s) = %+v, want rejection", bad, got)
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
		testassert.Require(t, err == nil, "Parse(%s): %v", doc, err)
		var members map[string]json.RawMessage
		testassert.NoError(t, json.Unmarshal([]byte(doc), &members))
		want, err := ParseSessions(members["sessions"])
		testassert.Require(t, err == nil, "ParseSessions for %s: %v", doc, err)
		testassert.Require(t, cfg.Sessions == want, "Parse(%s) sessions = %+v, ParseSessions = %+v", doc, cfg.Sessions, want)
	}
	// The whole-document entry shares the ParseSessions contract: its
	// validation errors are propagated instead of inventing defaults for a
	// malformed or overflowing sessions policy.
	doc := []byte(`{"providers":{},"sessions":{"archive_after_days":106752}}`)
	_, docErr := Parse(doc)
	var members map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(doc, &members))
	_, sharedErr := ParseSessions(members["sessions"])
	testassert.Require(t, sharedErr != nil, "ParseSessions accepted the overflowed policy")
	testassert.Require(t, docErr != nil, "Parse of overflowed days = nil error, want the ParseSessions rejection %q", sharedErr)
	testassert.Require(t, docErr.Error() == sharedErr.Error(), "Parse error = %q, want the propagated ParseSessions error %q", docErr, sharedErr)
}
