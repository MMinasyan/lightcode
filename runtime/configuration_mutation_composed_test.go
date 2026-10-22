package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
	"github.com/MMinasyan/lightcode/runtime"
)

// The composed mutation rows over the real shipped plugin set: the
// settings PUT's candidate is validated by the real tools/jobs/tasks
// validators, and the Agent-model edit resolves the real roster — the
// in-package fake validators alone do not prove the real limits.

// composedMutationSettings is one complete settings shape the real
// validators accept: each compiled plugin's section as its opaque document
// string.
func composedMutationSettings() protocol.Settings {
	return protocol.Settings{
		Sessions: protocol.SessionsSettings{AutoArchive: true, ArchiveAfterDays: 14, DeleteAfterArchiveDays: 3},
		Plugins: protocol.PluginsSettings{
			"tools": `{"command_timeout":90,"max_output_bytes":4096,"read_line_max_chars":4000,"read_max_lines":200}`,
			"jobs":  `{"max_background_processes":5,"max_output_bytes":8192,"read_line_max_chars":4000}`,
		},
	}
}

// TestComposedSettingsMutationThroughRealValidators proves the settings PUT
// end to end over the shipped plugin set: a valid whole-shape write is
// validated by the real plugins' validators, rewritten to the owning file,
// and published with the next generation and one event; a write the real
// tools validator rejects changes no file, generation, warning, or event.
func TestComposedSettingsMutationThroughRealValidators(t *testing.T) {
	ctx := context.Background()
	e := newComposeEnv(t)
	r, err := runtime.OpenForTest(ctx, e.dataDir, e.configPath, builtin.Plugins())
	testassert.NoError(t, err)
	sub, err := r.Subscribe(8)
	testassert.NoError(t, err)
	defer sub.Close()

	view, err := runtime.GetConfigurationForTest(r)
	testassert.NoError(t, err)
	testassert.Require(t, view.ConfigurationRevision.Generation == "1", "initial revision = %+v, want generation 1", view.ConfigurationRevision)

	// The valid write: the real validators accept the marshaled sections.
	mutation, err := runtime.UpdateSettingsForTest(r, composedMutationSettings())
	testassert.NoError(t, err)
	testassert.Require(t, mutation.ConfigurationRevision.Generation == "2" && mutation.Result.Sessions.ArchiveAfterDays == 14 && compactComposed(t, []byte(mutation.Result.Plugins["jobs"])) == compactComposed(t, []byte(composedMutationSettings().Plugins["jobs"])), "mutation revision = %+v, want generation 2; mutation result = %+v, want the written shape", mutation.ConfigurationRevision, mutation.Result)
	data, err := os.ReadFile(e.configPath)
	testassert.NoError(t, err)
	var root struct {
		Plugins struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"plugins"`
		Sessions json.RawMessage `json:"sessions"`
	}
	testassert.NoError(t, json.Unmarshal(data, &root))
	{
		got := compactComposed(t, root.Plugins.Tools)
		testassert.Require(t, strings.Contains(got, `"max_output_bytes":4096`), "plugins.tools member = %s, want the written section", got)
	}
	if event, ok := nextComposedEvent(t, sub); !ok || composedEventKind(t, event) != "configuration_changed" || composedEventGeneration(t, event) != "2" {
		t.Fatalf("event = %s (ok=%v), want the generation 2 configuration event", composedEventJSON(event), ok)
	}

	// The rejected write: the real tools validator refuses a zero
	// max_output_bytes — no file write, no generation, no warning change, no
	// event.
	before, err := os.ReadFile(e.configPath)
	testassert.NoError(t, err)
	warnings, err := runtime.GetWarningsForTest(r)
	testassert.NoError(t, err)
	rejected := composedMutationSettings()
	rejected.Plugins["tools"] = `{"command_timeout":0,"max_output_bytes":0,"read_line_max_chars":0,"read_max_lines":0}`
	{
		mutation, err := runtime.UpdateSettingsForTest(r, rejected)
		testassert.Require(t, err != nil, "rejected mutation = %+v, want the real tools validator's failure", mutation)
	}
	after, err := os.ReadFile(e.configPath)
	testassert.Require(t, err == nil && string(before) == string(after), "a rejected candidate changed the owning file (%v)", err)
	fresh, err := runtime.GetConfigurationForTest(r)
	testassert.NoError(t, err)
	testassert.Require(t, fresh.ConfigurationRevision.Generation == "2", "generation after rejection = %+v, want 2", fresh.ConfigurationRevision)
	freshWarnings, err := runtime.GetWarningsForTest(r)
	testassert.NoError(t, err)
	testassert.Equal(t, freshWarnings.WarningsRevision.Revision, warnings.WarningsRevision.Revision)
	select {
	case event, ok := <-sub.Events():
		testassert.Require(t, !ok, "a rejected candidate published event %+v, want silence", event)
	default:
	}
}

// TestComposedAgentModelMutationThroughRealRoster proves the Agent-model PUT
// over the real roster: a builtin type that needs no explicit user entry is
// addressable and the written override lands in the owning agents file,
// while an unknown type fails invalid without creating a bare entry.
func TestComposedAgentModelMutationThroughRealRoster(t *testing.T) {
	ctx := context.Background()
	e := newComposeEnv(t)
	r, err := runtime.OpenForTest(ctx, e.dataDir, e.configPath, builtin.Plugins())
	testassert.NoError(t, err)
	agentsPath := filepath.Join(filepath.Dir(e.configPath), "agents.json")
	before, err := os.ReadFile(agentsPath)
	testassert.NoError(t, err)

	mutation, err := runtime.SetAgentTypeModelForTest(r, "primary", "prov/m")
	testassert.NoError(t, err)
	testassert.Require(t, mutation.ConfigurationRevision.Generation == "2" && mutation.Result.Model == "prov/m" && mutation.Result.Name == "primary", "mutation = %+v, want the primary override at generation 2", mutation)
	var root map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(before, &root))
	{
		_, ok := root["primary"]
		testassert.Require(t, !ok, "agents file already had a primary entry: %s", before)
	}
	afterData, err := os.ReadFile(agentsPath)
	testassert.NoError(t, err)
	var after map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(afterData, &after))
	testassert.Equal(t, compactComposed(t, after["primary"]), `{"model":"prov/m"}`)

	{
		_, err := runtime.SetAgentTypeModelForTest(r, "ghost", "prov/m")
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "unknown type = %v, want the shared invalid failure", err)
	}
	final, err := os.ReadFile(agentsPath)
	testassert.NoError(t, err)
	var finalRoot map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(final, &finalRoot))
	{
		_, ok := finalRoot["ghost"]
		testassert.Require(t, !ok, "a failed mutation created a bare entry: %s", final)
	}
}

// compactComposed renders one JSON value's canonical compacted form.
func compactComposed(t *testing.T, data []byte) string {
	t.Helper()
	var out bytes.Buffer
	{
		err := json.Compact(&out, data)
		testassert.Require(t, err == nil, "compact %s: %v", data, err)
	}
	return out.String()
}
