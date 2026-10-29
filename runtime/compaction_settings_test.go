package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/prompt"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// TestParseCompactionSettings pins the top-level compaction section: an
// absent or empty model keeps the conversation-model fallback, a nonempty
// model goes through the existing model-reference parser, an absent prompt
// keeps the shared builtin text, and a present prompt must be nonempty and is
// kept exactly. Unknown members are ignored.
func TestParseCompactionSettings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        string
		wantModel  model.ModelRef
		wantPrompt string
	}{
		{"absent", "", model.ModelRef{}, prompt.DefaultSummarizerPrompt},
		{"empty object", `{}`, model.ModelRef{}, prompt.DefaultSummarizerPrompt},
		{"empty model", `{"model":""}`, model.ModelRef{}, prompt.DefaultSummarizerPrompt},
		{"model", `{"model":"prov/m2"}`, model.ModelRef{Provider: "prov", Model: "m2"}, prompt.DefaultSummarizerPrompt},
		{"custom prompt", `{"prompt":"  exact\nbytes  "}`, model.ModelRef{}, "  exact\nbytes  "},
		{"unknown members ignored", `{"model":"prov/m2","summarizer_provider":"legacy"}`, model.ModelRef{Provider: "prov", Model: "m2"}, prompt.DefaultSummarizerPrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCompactionSettings(json.RawMessage(tc.raw))
			testassert.NoError(t, err)
			testassert.Require(t, got.model == tc.wantModel && got.prompt == tc.wantPrompt, "parseCompactionSettings(%s) = %+v, want %+v/%q", tc.raw, got, tc.wantModel, tc.wantPrompt)
		})
	}
	for _, raw := range []string{
		`null`, `[]`, `7`, `"compaction"`,
		`{"model":"malformed"}`, `{"model":"prov/"}`, `{"model":5}`,
		`{"prompt":""}`, `{"prompt":null}`, `{"prompt":5}`,
	} {
		t.Run("reject "+raw, func(t *testing.T) {
			_, err := parseCompactionSettings(json.RawMessage(raw))
			testassert.Require(t, err != nil, "parseCompactionSettings(%s) accepted, want rejection", raw)
		})
	}
}

// TestCandidateRejectsInvalidBaselineSections pins the publication boundary:
// a non-object tools/jobs/compaction member or an invalid compaction model or
// prompt rejects the complete candidate with the typed configuration failure,
// while an absent section keeps its defaults.
func TestCandidateRejectsInvalidBaselineSections(t *testing.T) {
	valid := `{"providers":{}}`
	{
		_, err := assembleConfiguration(1, valid, `{}`, nil)
		testassert.NoError(t, err)
	}
	for _, tc := range []struct{ name, document string }{
		{"tools null", `{"providers":{},"tools":null}`},
		{"tools array", `{"providers":{},"tools":[]}`},
		{"jobs null", `{"providers":{},"jobs":null}`},
		{"jobs scalar", `{"providers":{},"jobs":7}`},
		{"compaction null", `{"providers":{},"compaction":null}`},
		{"compaction string", `{"providers":{},"compaction":"{}"}`},
		{"compaction malformed model", `{"providers":{},"compaction":{"model":"prov/"}}`},
		{"compaction empty prompt", `{"providers":{},"compaction":{"prompt":""}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := assembleConfiguration(1, tc.document, `{}`, nil)
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "captured configuration"), "candidate %s = %v, want a captured-configuration rejection", tc.document, err)
		})
	}
}

// TestSettingsMutationWritesBaselineRootsExactly proves the settings writer
// carries the three top-level objects with exact numeric lexemes, unknown
// members, and the custom compaction prompt unchanged, preserves an unowned
// root member, and projects the same captured bytes back.
func TestSettingsMutationWritesBaselineRootsExactly(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	svc := h.service(context.Background(), servicePlugin("capowner", &h.opens, acceptValidator))
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	settings := mutationSettings()
	settings.Tools = json.RawMessage(`{"read_max_lines":9007199254740993,"unknown":1}`)
	settings.Jobs = json.RawMessage(`{"max_output_bytes":1048576,"unknown":2}`)
	settings.Compaction = json.RawMessage(`{"model":"prov/m2","prompt":"  exact prompt  ","unknown":3}`)
	settings.Plugins = protocol.PluginsSettings{}
	candidate, err := svc.mutate(context.Background(), editSettings(settings))
	testassert.NoError(t, err)
	root := fileRoot(t, h.configPath)
	for name, want := range map[string]string{
		"tools":      `{"read_max_lines":9007199254740993,"unknown":1}`,
		"jobs":       `{"max_output_bytes":1048576,"unknown":2}`,
		"compaction": `{"model":"prov/m2","prompt":"  exact prompt  ","unknown":3}`,
	} {
		testassert.Equal(t, compactJSON(t, root[name]), want)
	}
	view := projectSettings(candidate.snapshot.sessions, candidate.snapshot.tools, candidate.snapshot.jobs, candidate.snapshot.compaction, candidate.snapshot.plugins)
	testassert.Equal(t, compactJSON(t, view.Tools), `{"read_max_lines":9007199254740993,"unknown":1}`)
	testassert.Equal(t, compactJSON(t, view.Jobs), `{"max_output_bytes":1048576,"unknown":2}`)
	testassert.Equal(t, compactJSON(t, view.Compaction), `{"model":"prov/m2","prompt":"  exact prompt  ","unknown":3}`)
	testassert.Require(t, candidate.snapshot.compactionValues.model == (model.ModelRef{Provider: "prov", Model: "m2"}) && candidate.snapshot.compactionValues.prompt == "  exact prompt  ", "parsed compaction = %+v, want the written model and exact prompt", candidate.snapshot.compactionValues)
	testassert.Require(t, string(root["custom_flag"]) == "true", "the settings edit dropped the unowned root member: %s", root["custom_flag"])
}
