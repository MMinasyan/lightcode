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
	"github.com/MMinasyan/lightcode/protocol"
	"github.com/MMinasyan/lightcode/runtime"
)

// The composed mutation rows over the real shipped plugin set: the
// settings PUT's candidate is validated by the real tools/jobs/tasks
// validators, and the Agent-model edit resolves the real roster — the
// in-package fake validators alone do not prove the real limits.

// composedMutationSettings is one complete settings shape the real
// validators accept.
func composedMutationSettings() protocol.Settings {
	tools := protocol.ToolsSettings{CommandTimeout: 90, MaxOutputBytes: 4096, ReadLineMaxChars: 4000, ReadMaxLines: 200}
	jobs := protocol.JobsSettings{MaxBackgroundProcesses: 5, MaxOutputBytes: 8192, ReadLineMaxChars: 4000}
	return protocol.Settings{
		Sessions: protocol.SessionsSettings{AutoArchive: true, ArchiveAfterDays: 14, DeleteAfterArchiveDays: 3},
		Plugins:  protocol.PluginsSettings{Tools: &tools, Jobs: &jobs},
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
	if err != nil {
		t.Fatalf("OpenForTest: %v", err)
	}
	sub, err := r.Subscribe(8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	view, err := runtime.GetConfigurationForTest(r)
	if err != nil {
		t.Fatalf("getConfiguration: %v", err)
	}
	if view.ConfigurationRevision.Generation != "1" {
		t.Fatalf("initial revision = %+v, want generation 1", view.ConfigurationRevision)
	}

	// The valid write: the real validators accept the marshaled sections.
	mutation, err := runtime.UpdateSettingsForTest(r, composedMutationSettings())
	if err != nil {
		t.Fatalf("updateSettings: %v", err)
	}
	if mutation.ConfigurationRevision.Generation != "2" {
		t.Fatalf("mutation revision = %+v, want generation 2", mutation.ConfigurationRevision)
	}
	if mutation.Result.Sessions.ArchiveAfterDays != 14 || mutation.Result.Plugins.Jobs == nil || mutation.Result.Plugins.Jobs.MaxBackgroundProcesses != 5 {
		t.Fatalf("mutation result = %+v, want the written shape", mutation.Result)
	}
	data, err := os.ReadFile(e.configPath)
	if err != nil {
		t.Fatalf("read owning file: %v", err)
	}
	var root struct {
		Plugins struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"plugins"`
		Sessions json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("decode owning file: %v", err)
	}
	if got := compactComposed(t, root.Plugins.Tools); !strings.Contains(got, `"max_output_bytes":4096`) {
		t.Fatalf("plugins.tools member = %s, want the written section", got)
	}
	if event, ok := nextComposedEvent(t, sub); !ok || composedEventKind(t, event) != "configuration_changed" || composedEventGeneration(t, event) != "2" {
		t.Fatalf("event = %s (ok=%v), want the generation 2 configuration event", composedEventJSON(event), ok)
	}

	// The rejected write: the real tools validator refuses a zero
	// max_output_bytes — no file write, no generation, no warning change, no
	// event.
	before, err := os.ReadFile(e.configPath)
	if err != nil {
		t.Fatalf("read owning file again: %v", err)
	}
	warnings, err := runtime.GetWarningsForTest(r)
	if err != nil {
		t.Fatalf("getWarnings: %v", err)
	}
	rejected := composedMutationSettings()
	zero := protocol.ToolsSettings{}
	rejected.Plugins.Tools = &zero
	if mutation, err := runtime.UpdateSettingsForTest(r, rejected); err == nil {
		t.Fatalf("rejected mutation = %+v, want the real tools validator's failure", mutation)
	}
	after, err := os.ReadFile(e.configPath)
	if err != nil || string(before) != string(after) {
		t.Fatalf("a rejected candidate changed the owning file (%v)", err)
	}
	fresh, err := runtime.GetConfigurationForTest(r)
	if err != nil {
		t.Fatalf("getConfiguration after rejection: %v", err)
	}
	if fresh.ConfigurationRevision.Generation != "2" {
		t.Fatalf("generation after rejection = %+v, want 2", fresh.ConfigurationRevision)
	}
	freshWarnings, err := runtime.GetWarningsForTest(r)
	if err != nil {
		t.Fatalf("getWarnings after rejection: %v", err)
	}
	if freshWarnings.WarningsRevision.Revision != warnings.WarningsRevision.Revision {
		t.Fatalf("warning revision advanced on a rejected candidate: %s → %s", warnings.WarningsRevision.Revision, freshWarnings.WarningsRevision.Revision)
	}
	select {
	case event, ok := <-sub.Events():
		if ok {
			t.Fatalf("a rejected candidate published event %+v, want silence", event)
		}
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
	if err != nil {
		t.Fatalf("OpenForTest: %v", err)
	}
	agentsPath := filepath.Join(filepath.Dir(e.configPath), "agents.json")
	before, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read agents file: %v", err)
	}

	mutation, err := runtime.SetAgentTypeModelForTest(r, "primary", "prov/m")
	if err != nil {
		t.Fatalf("setAgentTypeModel(primary): %v", err)
	}
	if mutation.ConfigurationRevision.Generation != "2" || mutation.Result.Model != "prov/m" || mutation.Result.Name != "primary" {
		t.Fatalf("mutation = %+v, want the primary override at generation 2", mutation)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(before, &root); err != nil {
		t.Fatalf("decode agents file: %v", err)
	}
	if _, ok := root["primary"]; ok {
		t.Fatalf("agents file already had a primary entry: %s", before)
	}
	afterData, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read agents file after edit: %v", err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(afterData, &after); err != nil {
		t.Fatalf("decode agents file after edit: %v", err)
	}
	if got := compactComposed(t, after["primary"]); got != `{"model":"prov/m"}` {
		t.Fatalf("primary entry = %s, want the bare model-only overlay", got)
	}

	if _, err := runtime.SetAgentTypeModelForTest(r, "ghost", "prov/m"); !errors.Is(err, harness.ErrInvalid) {
		t.Fatalf("unknown type = %v, want the shared invalid failure", err)
	}
	final, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read agents file after failure: %v", err)
	}
	var finalRoot map[string]json.RawMessage
	if err := json.Unmarshal(final, &finalRoot); err != nil {
		t.Fatalf("decode agents file after failure: %v", err)
	}
	if _, ok := finalRoot["ghost"]; ok {
		t.Fatalf("a failed mutation created a bare entry: %s", final)
	}
}

// compactComposed renders one JSON value's canonical compacted form.
func compactComposed(t *testing.T, data []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, data); err != nil {
		t.Fatalf("compact %s: %v", data, err)
	}
	return out.String()
}
