package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/tool"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

// Direct plugin contract tests. Outside the runtime package only the zero
// Invocation is constructible, so Normalize/Prepare here exercise the
// defaults path; configuration-bearing behavior is covered by
// ValidateConfig directly and by the runtime-composed tests in the runtime
// package. These tests never open the plugin — the tools plugin requires the
// jobs capability, whose binding only the composed Runtime scope constructs —
// so the concrete tool values are built from one instance directly.

// testEntry is a valid-looking Harness admitted-input identity.
const (
	testSessionID = "0123456789abcdef0123456789abcdef"
	testEntryID   = "fedcba9876543210fedcba9876543210"
)

// directTools constructs the plugin's tool exports from one instance the
// way open publishes them, without opening the plugin.
func directTools(dataDir string, managedKeys []string) map[string]runtime.Tool {
	inst := &instance{dataDir: dataDir, managedKeys: managedKeys}
	return map[string]runtime.Tool{
		"read_file":   readTool{inst},
		"write_file":  writeTool{inst},
		"edit_file":   editTool{inst},
		"apply_patch": patchTool{inst},
		"run_command": runCommandTool{inst},
		"process":     processTool{inst},
		"sleep":       sleepTool{},
	}
}

// callToolContext is the ToolContext the direct tests prepare with: a real
// temporary Workspace, a valid-looking admitted-input identity, the zero
// Invocation (defaults), and the given constraints.
func callToolContext(workspace string, constraints runtime.ToolConstraints) runtime.ToolContext {
	return runtime.ToolContext{
		Workspace:     workspace,
		AdmittedEntry: harness.EntryRef{SessionID: testSessionID, EntryID: testEntryID},
		Invocation:    runtime.Invocation{},
		Constraints:   constraints,
	}
}

func makeCall(t *testing.T, name, args string) model.ToolCall {
	t.Helper()
	call, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("NewToolCall: %v", err)
	}
	return call
}

// prepare normalizes and prepares one call, returning the plan.
func prepare(t *testing.T, tool runtime.Tool, tc runtime.ToolContext, name, args string) harness.PreparedTool {
	t.Helper()
	return tool.Prepare(context.Background(), tc, makeCall(t, name, args))
}

// normalize runs one tool's normalization over raw argument bytes.
func normalize(t *testing.T, tool runtime.Tool, tc runtime.ToolContext, name, args string) (json.RawMessage, error) {
	t.Helper()
	return tool.Normalize(tc, makeCall(t, name, args))
}

// groupDir is the exact per-Operation code-group directory the mutating
// calls must derive from the ToolContext's admitted-input identity.
func groupDir(dataDir, sessionID, entryID string) string {
	return filepath.Join(dataDir, "code", sessionID, entryID)
}

func TestValidateConfigSettings(t *testing.T) {
	validate := func(raw string) error {
		return Plugin().ValidateConfig(json.RawMessage(raw))
	}
	for _, raw := range []string{
		"", " ", "{}", "null",
		`{"max_output_bytes":null,"read_max_lines":null,"read_line_max_chars":null,"command_timeout":null}`,
		`{"max_output_bytes":1,"read_max_lines":1,"read_line_max_chars":1,"command_timeout":1}`,
		`{"max_output_bytes":9223372036854775807,"read_max_lines":1,"read_line_max_chars":1,"command_timeout":1}`,
		`{"command_timeout":9223372036}`,
		// Every other member is ignored, including max_background_processes
		// regardless of its JSON value.
		`{"max_background_processes":5}`,
		`{"max_background_processes":"many","unknown_member":{"deep":[1,2]}}`,
	} {
		if err := validate(raw); err != nil {
			t.Errorf("ValidateConfig(%s) = %v, want accepted", raw, err)
		}
	}
	for _, raw := range []string{
		`{"max_output_bytes":0}`, `{"read_max_lines":-1}`, `{"read_line_max_chars":0}`, `{"command_timeout":0}`,
		`{"command_timeout":-5}`,
		`{"max_output_bytes":"many"}`, `{"read_max_lines":true}`, `{"command_timeout":1.5}`,
		// Integer overflow and the seconds-to-duration bound (MaxInt64/time.Second).
		`{"max_output_bytes":99999999999999999999}`,
		`{"command_timeout":9223372036854775808}`,
		`{"command_timeout":9223372036854775807}`,
		`{"command_timeout":9223372037}`,
		// Wrong section shapes.
		`[]`, `"tools"`, `5`,
	} {
		if err := validate(raw); err == nil {
			t.Errorf("ValidateConfig(%s) = nil, want rejected", raw)
		}
	}
}

func TestDescribeAvailabilityAndDefinitions(t *testing.T) {
	// The describe functions are static method values needing no instance;
	// the composed open contract is asserted in the runtime package's
	// composed tools suite.
	describe := map[string]func(runtime.Invocation, runtime.ToolConstraints, harness.SessionIdentity) (runtime.ToolDescription, error){
		"read_file":   readTool{}.describe,
		"write_file":  writeTool{}.describe,
		"edit_file":   editTool{}.describe,
		"apply_patch": patchTool{}.describe,
		"run_command": runCommandTool{}.describe,
		"process":     processTool{}.describe,
		"sleep":       sleepTool{}.describe,
	}

	cases := []struct {
		name        string
		constraints runtime.ToolConstraints
		available   map[string]bool
	}{
		{"unconstrained agent", runtime.ToolConstraints{}, map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "apply_patch": true, "run_command": true, "process": true, "sleep": true}},
		{"readonly without write dir", runtime.ToolConstraints{Readonly: true}, map[string]bool{"read_file": true, "write_file": false, "edit_file": false, "apply_patch": false, "run_command": true, "process": true, "sleep": true}},
		{"readonly with write dir", runtime.ToolConstraints{Readonly: true, WriteDir: "/w"}, map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "apply_patch": true, "run_command": true, "process": true, "sleep": true}},
	}
	for _, tc := range cases {
		for name, want := range tc.available {
			got, err := describe[name](runtime.Invocation{}, tc.constraints, harness.SessionIdentity{})
			if err != nil {
				t.Fatalf("%s: describe %s: %v", tc.name, name, err)
			}
			if got.Available != want {
				t.Errorf("%s: describe %s Available = %v, want %v", tc.name, name, got.Available, want)
			}
			wantHidden := name == "apply_patch"
			if got.DefaultHidden != wantHidden {
				t.Errorf("%s: describe %s DefaultHidden = %v, want %v", tc.name, name, got.DefaultHidden, wantHidden)
			}
			if got.Definition.Name != name {
				t.Errorf("%s: describe %s declares name %q", tc.name, name, got.Definition.Name)
			}
			if got.Definition.Description == "" || !json.Valid(got.Definition.Parameters) {
				t.Errorf("%s: describe %s produced an unusable definition", tc.name, name)
			}
		}
	}
}

// assertJSONEqual compares two JSON payloads by their decoded value.
func assertJSONEqual(t *testing.T, got json.RawMessage, want any) {
	t.Helper()
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal retained schema: %v", err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode described schema %s: %v", got, err)
	}
	if err := json.Unmarshal(wantRaw, &wantValue); err != nil {
		t.Fatalf("decode retained schema %s: %v", wantRaw, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("described schema = %s, want the retained schema %s", got, wantRaw)
	}
}

// TestRunCommandDescriptionsAndSchemasMatchRetained pins both run_command
// surfaces to the retained texts: the ordinary Agent receives the complete
// legacy description with the background bullets and the readonly Agent
// receives the retained readonly description; both share the retained schema.
func TestRunCommandDescriptionsAndSchemasMatchRetained(t *testing.T) {
	legacy := &tool.RunCommand{}
	ordinary, err := runCommandTool{}.describe(runtime.Invocation{}, runtime.ToolConstraints{}, harness.SessionIdentity{})
	if err != nil {
		t.Fatalf("describe ordinary: %v", err)
	}
	if ordinary.Definition.Description != legacy.Description() {
		t.Errorf("ordinary description = %q, want the retained legacy description %q", ordinary.Definition.Description, legacy.Description())
	}
	assertJSONEqual(t, ordinary.Definition.Parameters, legacy.ParametersSchema())

	readonly, err := runCommandTool{}.describe(runtime.Invocation{}, runtime.ToolConstraints{Readonly: true}, harness.SessionIdentity{})
	if err != nil {
		t.Fatalf("describe readonly: %v", err)
	}
	if readonly.Definition.Description != tool.ReadOnlyRunCommandDescription {
		t.Errorf("readonly description = %q, want the retained readonly description", readonly.Definition.Description)
	}
	legacyReadonly := tool.NewReadOnlyRunCommand(legacy)
	assertJSONEqual(t, readonly.Definition.Parameters, legacyReadonly.ParametersSchema())
	if !ordinary.Available || !readonly.Available {
		t.Errorf("availability ordinary/readonly = %v/%v, want both available", ordinary.Available, readonly.Available)
	}
}

// TestProcessDescriptionAndSchemaMatchRetained pins the process tool's
// model-visible surface to the retained legacy description and schema.
func TestProcessDescriptionAndSchemaMatchRetained(t *testing.T) {
	legacy := &tool.ProcessTool{}
	got, err := processTool{}.describe(runtime.Invocation{}, runtime.ToolConstraints{}, harness.SessionIdentity{})
	if err != nil {
		t.Fatalf("describe process: %v", err)
	}
	if got.Definition.Description != legacy.Description() {
		t.Errorf("process description = %q, want the retained legacy description %q", got.Definition.Description, legacy.Description())
	}
	assertJSONEqual(t, got.Definition.Parameters, legacy.ParametersSchema())
	if got.Definition.Name != "process" || !got.Available || got.DefaultHidden {
		t.Errorf("process definition = %+v, want name process, available, not hidden", got)
	}
}

func TestNormalizeArguments(t *testing.T) {
	byID := directTools(t.TempDir(), nil)
	tc := callToolContext(t.TempDir(), runtime.ToolConstraints{})

	t.Run("read_file applies defaults and strips private fields", func(t *testing.T) {
		raw, err := normalize(t, byID["read_file"], tc, "read_file", `{"path":"a.txt","_lightcode_receipt":"x"}`)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("normalized bytes: %v", err)
		}
		if args["path"] != "a.txt" || args["offset"] != float64(1) || args["limit"] != float64(500) {
			t.Fatalf("normalized = %s, want the path with default offset/limit", raw)
		}
		if _, ok := args["_lightcode_receipt"]; ok {
			t.Fatal("normalized arguments retained a private field")
		}
	})
	t.Run("read_file keeps exact integral spellings and rejects fractions and overflow", func(t *testing.T) {
		raw, err := normalize(t, byID["read_file"], tc, "read_file", `{"path":"a","offset":1e0,"limit":2.0}`)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if !strings.Contains(string(raw), `"offset":1`) || !strings.Contains(string(raw), `"limit":2`) {
			t.Fatalf("normalized = %s, want exact integral values", raw)
		}
		if _, err := normalize(t, byID["read_file"], tc, "read_file", `{"path":"a","limit":2.5}`); err == nil {
			t.Error("fractional limit normalized, want rejection")
		}
		if _, err := normalize(t, byID["read_file"], tc, "read_file", `{"path":"a","offset":99999999999999999999}`); err == nil {
			t.Error("overflowing offset normalized, want rejection")
		}
	})
	t.Run("strict schema errors", func(t *testing.T) {
		for name, args := range map[string]string{
			"read_file":   `{"path":5}`,
			"read_file2":  `{"path":"a","limit":"many"}`,
			"write_file":  `{"path":"a"}`,
			"write_file2": `{"path":"a","content":5}`,
			"edit_file":   `{"path":"a","old_string":"x","new_string":5}`,
			"edit_file2":  `{"path":"a","old_string":"x","new_string":"y","replace_all":"yes"}`,
			"apply_patch": `{"input":5}`,
		} {
			toolName := name
			if strings.HasSuffix(toolName, "2") {
				toolName = strings.TrimSuffix(toolName, "2")
			}
			if _, err := normalize(t, byID[toolName], tc, toolName, args); err == nil {
				t.Errorf("%s normalized %s, want rejection", toolName, args)
			}
		}
	})
	t.Run("process requires string action and id members when present", func(t *testing.T) {
		for _, args := range []string{
			`{"action":5}`,
			`{"action":null}`,
			`{"action":"read","id":5}`,
			`{"action":"list","id":true}`,
		} {
			if _, err := normalize(t, byID["process"], tc, "process", args); err == nil {
				t.Errorf("process normalized %s, want rejection", args)
			}
		}
		raw, err := normalize(t, byID["process"], tc, "process", `{"action":"read","id":"deadbeef","_lightcode_receipt":"x","note":"kept"}`)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		var normalized map[string]any
		if err := json.Unmarshal(raw, &normalized); err != nil {
			t.Fatalf("normalized bytes: %v", err)
		}
		if normalized["action"] != "read" || normalized["id"] != "deadbeef" || normalized["note"] != "kept" {
			t.Fatalf("normalized = %s, want the members preserved", raw)
		}
		if _, ok := normalized["_lightcode_receipt"]; ok {
			t.Fatal("normalized arguments retained a private field")
		}
	})
	t.Run("malformed, null and trailing argument bytes are validation errors", func(t *testing.T) {
		for _, args := range []string{``, `not json`, `null`, `[1]`, `{"path":"a"} trailing`, `"str"`} {
			for _, name := range []string{"read_file", "write_file", "edit_file", "apply_patch", "run_command", "process", "sleep"} {
				if _, err := normalize(t, byID[name], tc, name, args); err == nil {
					t.Errorf("%s normalized %q, want rejection", name, args)
				}
			}
		}
	})
}

// bindingChangePatchInput builds one add-file patch call over the given path
// for the binding-witness oracle.
func bindingChangePatchInput(t *testing.T, path string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"input": "*** Begin Patch\n*** Add File: " + path + "\n+new\n*** End Patch"})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestPrepareBindingSurvivesCanonicalPathsRepoint proves the one-binding
// contract at the plugin boundary: the canonical Workspace root and write-dir
// boundary the plugin resolves for containment are the same witnesses the
// shared preparation binds, so repointing either symlink between the plugin's
// resolutions and the shared preparation's binding fails the execution with
// the changed-binding error instead of executing under the repointed tree.
// One cell per entry and bound slot: read binds the root; write, edit and
// apply_patch bind the root and the write-dir.
func TestPrepareBindingSurvivesCanonicalPathsRepoint(t *testing.T) {
	cells := []struct {
		name          string
		toolID        string
		args          string
		slot          string // "root" repoints the workspace symlink, "write_dir" the write-dir symlink
		content       string // planted in the repointed-to directory when nonempty
		boundWriteDir bool   // the failure must name the pre-flip canonical write-dir as the bound value, not the empty-witness form
	}{
		{"read_file revalidates the root witness", "read_file", `{"path":"target.txt"}`, "root", "substituted\n", false},
		{"write_file revalidates the root witness", "write_file", `{"path":"target.txt","content":"after"}`, "root", "", false},
		{"write_file revalidates the write-dir witness", "write_file", `{"path":"wd/target.txt","content":"x"}`, "write_dir", "", false},
		{"edit_file revalidates the root witness", "edit_file", `{"path":"target.txt","old_string":"beta","new_string":"BETA"}`, "root", "beta\n", false},
		{"edit_file revalidates the write-dir witness", "edit_file", `{"path":"wd/target.txt","old_string":"beta","new_string":"BETA"}`, "write_dir", "beta\n", false},
		{"apply_patch revalidates the root witness", "apply_patch", bindingChangePatchInput(t, "target.txt"), "root", "", false},
		{"apply_patch revalidates the write-dir witness", "apply_patch", bindingChangePatchInput(t, "wd/target.txt"), "write_dir", "", true},
	}
	for _, cell := range cells {
		cell := cell
		t.Run(cell.name, func(t *testing.T) {
			byID := directTools(t.TempDir(), nil)
			var workspace, link, flipTo, preFlipWriteDir string
			var constraints runtime.ToolConstraints
			if cell.slot == "root" {
				realA, realB := t.TempDir(), t.TempDir()
				flipTo = realB
				link = filepath.Join(t.TempDir(), "w")
				workspace = link
				if err := os.Symlink(realA, link); err != nil {
					t.Fatal(err)
				}
			} else {
				workspace = t.TempDir()
				wdA, wdB := t.TempDir(), t.TempDir()
				flipTo = wdB
				preFlipWriteDir = wdA
				link = filepath.Join(workspace, "wd")
				if err := os.Symlink(wdA, link); err != nil {
					t.Fatal(err)
				}
				constraints = runtime.ToolConstraints{WriteDir: "wd"}
			}
			if cell.content != "" {
				if err := os.WriteFile(filepath.Join(flipTo, "target.txt"), []byte(cell.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// The seam repoints the symlink on its return: after the plugin's
			// canonical resolutions, before the shared preparation binds.
			orig := canonicalPathsFn
			canonicalPathsFn = func(root, writeDir string) (string, string, error) {
				w, b, err := orig(root, writeDir)
				if err != nil {
					return w, b, err
				}
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(flipTo, link); err != nil {
					t.Fatal(err)
				}
				return w, b, err
			}
			defer func() { canonicalPathsFn = orig }()
			plan := prepare(t, byID[cell.toolID], callToolContext(workspace, constraints), cell.toolID, cell.args)
			outcome := plan.Execute(context.Background())
			if outcome.Result.Status != model.ResultError || !strings.Contains(outcome.Result.Content, "changed") {
				t.Fatalf("result = %+v, want the changed-binding failure without substitution", outcome.Result)
			}
			// The patch path threads the write-dir witness directly, so a
			// dropped witness would fail with the empty-witness form
			// ("changed from  to ..."); the real changed-binding error names
			// the pre-flip canonical write-dir as the bound value.
			if cell.boundWriteDir && !strings.Contains(outcome.Result.Content, preFlipWriteDir) {
				t.Fatalf("result = %+v, want the changed-binding error naming the bound write-dir %q", outcome.Result, preFlipWriteDir)
			}
		})
	}
}
