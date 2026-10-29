package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/internal/tool"
	"github.com/MMinasyan/lightcode/model"
)

// Direct standard-tool contract tests: the baseline tool values are built
// from one constructed state the way the owner composes them; the zero
// Invocation exercises the defaults path, and configuration-bearing behavior
// is covered by the publication tests in the runtime package.

// testEntry is a valid-looking Harness admitted-input identity.
const (
	testSessionID = "0123456789abcdef0123456789abcdef"
	testEntryID   = "fedcba9876543210fedcba9876543210"
)

// directTools constructs the baseline tool values over one data root with no
// jobs service, the way newStandardTools composes them.
func directTools(dataDir string) map[string]Tool {
	return newStandardTools(dataDir, nil)
}

// callToolContext is the ToolContext the direct tests prepare with: a real
// temporary Workspace, a valid-looking admitted-input identity, the zero
// Invocation (defaults), and the given constraints.
func callToolContext(workspace string, constraints ToolConstraints) ToolContext {
	return ToolContext{
		Workspace:     workspace,
		AdmittedEntry: harness.EntryRef{SessionID: testSessionID, EntryID: testEntryID},
		Invocation:    Invocation{},
		Constraints:   constraints,
	}
}

func makeCall(t *testing.T, name, args string) model.ToolCall {
	t.Helper()
	call, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: name, Arguments: json.RawMessage(args)})
	testassert.NoError(t, err)
	return call
}

// prepare normalizes and prepares one call, returning the plan.
func prepare(t *testing.T, tool Tool, tc ToolContext, name, args string) harness.PreparedTool {
	t.Helper()
	return tool.Prepare(context.Background(), tc, makeCall(t, name, args))
}

// normalize runs one tool's normalization over raw argument bytes.
func normalize(t *testing.T, tool Tool, tc ToolContext, name, args string) (json.RawMessage, error) {
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
		return validateToolsSettings(json.RawMessage(raw))
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
		{
			err := validate(raw)
			testassert.Check(t, err == nil, "ValidateConfig(%s) = %v, want accepted", raw, err)
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
		{
			err := validate(raw)
			testassert.Check(t, err != nil, "ValidateConfig(%s) = nil, want rejected", raw)
		}
	}
}

func TestDescribeAvailabilityAndDefinitions(t *testing.T) {
	// The describe functions are static method values needing no instance;
	// the composed open contract is asserted in the runtime package's
	// composed tools suite.
	describe := map[string]func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error){
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
		constraints ToolConstraints
		available   map[string]bool
	}{
		{"unconstrained agent", ToolConstraints{}, map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "apply_patch": true, "run_command": true, "process": true, "sleep": true}},
		{"readonly without write dir", ToolConstraints{Readonly: true}, map[string]bool{"read_file": true, "write_file": false, "edit_file": false, "apply_patch": false, "run_command": true, "process": true, "sleep": true}},
		{"readonly with write dir", ToolConstraints{Readonly: true, WriteDir: "/w"}, map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "apply_patch": true, "run_command": true, "process": true, "sleep": true}},
	}
	for _, tc := range cases {
		for name, want := range tc.available {
			got, err := describe[name](Invocation{}, tc.constraints, harness.SessionIdentity{})
			testassert.Require(t, err == nil, "%s: describe %s: %v", tc.name, name, err)
			testassert.Check(t, got.Available == want, "%s: describe %s Available = %v, want %v", tc.name, name, got.Available, want)
			testassert.Check(t, got.Definition.Name == name, "%s: describe %s declares name %q", tc.name, name, got.Definition.Name)
			testassert.Check(t, got.Definition.Description != "" && json.Valid(got.Definition.Parameters), "%s: describe %s produced an unusable definition", tc.name, name)
		}
	}
}

// assertJSONEqual compares two JSON payloads by their decoded value.
func assertJSONEqual(t *testing.T, got json.RawMessage, want any) {
	t.Helper()
	wantRaw, err := json.Marshal(want)
	testassert.NoError(t, err)
	var gotValue, wantValue any
	{
		err := json.Unmarshal(got, &gotValue)
		testassert.Require(t, err == nil, "decode described schema %s: %v", got, err)
	}
	{
		err := json.Unmarshal(wantRaw, &wantValue)
		testassert.Require(t, err == nil, "decode retained schema %s: %v", wantRaw, err)
	}
	testassert.Check(t, reflect.DeepEqual(gotValue, wantValue), "described schema = %s, want the retained schema %s", got, wantRaw)
}

// wantRunCommandDescription is the independent golden fixture for the
// ordinary run_command description, verified against the retained value. It
// is deliberately a test-local literal rather than the production constant,
// so drift in descriptions.go fails this test.
const wantRunCommandDescription = `Executes a shell command and returns combined stdout and stderr.
- Each call starts a fresh shell in the project root. Environment variables, aliases, and working directory do not persist between calls. Use "cd /path && command" if you need a different working directory.
- Foreground commands use the default timeout. Background commands run until they exit, are killed, or reach an explicit timeout parameter.
- For commands that may run for a long time, keep producing output, wait on external state, and are not needed before your next step, set background=true. It returns immediately with a process ID. You will be notified when it finishes. To read output while it is still running, use sleep to wait, then process to read the output. To kill it, use process. Do not use background=true for commands that will probably finish in a few seconds.
- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use edit_file or write_file.`

// wantRunCommandParameters is the independent golden fixture for the one
// run_command schema the ordinary and readonly surfaces share.
const wantRunCommandParameters = `{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "The shell command to execute."
    },
    "timeout": {
      "type": "integer",
      "description": "Timeout in seconds for this command. Overrides the default."
    },
    "background": {
      "type": "boolean",
      "description": "If true, run the command in the background and return immediately with a process ID."
    }
  },
  "required": ["command"]
}`

// wantProcessDescription and wantProcessParameters are the independent golden
// fixtures for the process tool's model-visible surface, verified against the
// retained values. They are deliberately test-local literals rather than the
// production constants, so drift in descriptions.go fails these tests.
const wantProcessDescription = "Manage background processes started by run_command with background=true.\n" +
	"\t- Use action \"read\" with id to read the output of a running background process.\n" +
	"\t- Use action \"kill\" with id to terminate a background process.\n" +
	"\t- Use action \"list\" to list background processes in the current session and their status."

const wantProcessParameters = `{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "description": "Action to perform: \"read\" to get output, \"kill\" to terminate, \"list\" to list processes in the current session."
    },
    "id": {
      "type": "string",
      "description": "Process ID returned by run_command with background=true."
    }
  },
  "required": ["action"]
}`

// TestRunCommandDescriptionsAndSchemasMatchRetained pins both run_command
// surfaces to the retained texts: the ordinary Agent receives the complete
// description with the background bullets and the readonly Agent receives
// the retained readonly description; both share the retained schema. The
// expectations are independent test-local fixtures, not the production
// constants.
func TestRunCommandDescriptionsAndSchemasMatchRetained(t *testing.T) {
	ordinary, err := runCommandTool{}.describe(Invocation{}, ToolConstraints{}, harness.SessionIdentity{})
	testassert.NoError(t, err)
	testassert.Check(t, ordinary.Definition.Description == wantRunCommandDescription, "ordinary description = %q, want the retained description %q", ordinary.Definition.Description, wantRunCommandDescription)
	assertJSONEqual(t, ordinary.Definition.Parameters, json.RawMessage(wantRunCommandParameters))

	readonly, err := runCommandTool{}.describe(Invocation{}, ToolConstraints{Readonly: true}, harness.SessionIdentity{})
	testassert.NoError(t, err)
	testassert.Check(t, readonly.Definition.Description == tool.ReadOnlyRunCommandDescription, "readonly description = %q, want the retained readonly description", readonly.Definition.Description)
	assertJSONEqual(t, readonly.Definition.Parameters, json.RawMessage(wantRunCommandParameters))
	testassert.Check(t, ordinary.Available && readonly.Available, "availability ordinary/readonly = %v/%v, want both available", ordinary.Available, readonly.Available)
}

// TestProcessDescriptionAndSchemaMatchRetained pins the process tool's
// model-visible surface to the independent test-local fixtures.
func TestProcessDescriptionAndSchemaMatchRetained(t *testing.T) {
	got, err := processTool{}.describe(Invocation{}, ToolConstraints{}, harness.SessionIdentity{})
	testassert.NoError(t, err)
	testassert.Check(t, got.Definition.Description == wantProcessDescription, "process description = %q, want the retained description %q", got.Definition.Description, wantProcessDescription)
	assertJSONEqual(t, got.Definition.Parameters, json.RawMessage(wantProcessParameters))
	testassert.Check(t, got.Definition.Name == "process" && got.Available, "process definition = %+v, want name process and available", got)
}

func TestNormalizeArguments(t *testing.T) {
	byID := directTools(t.TempDir())
	tc := callToolContext(t.TempDir(), ToolConstraints{})

	t.Run("read_file applies defaults and strips private fields", func(t *testing.T) {
		raw, err := normalize(t, byID["read_file"], tc, "read_file", `{"path":"a.txt","_lightcode_receipt":"x"}`)
		testassert.NoError(t, err)
		var args map[string]any
		testassert.NoError(t, json.Unmarshal(raw, &args))
		testassert.Require(t, args["path"] == "a.txt" && args["offset"] == float64(1) && args["limit"] == float64(500), "normalized = %s, want the path with default offset/limit", raw)
		{
			_, ok := args["_lightcode_receipt"]
			testassert.Require(t, !ok, "normalized arguments retained a private field")
		}
	})
	t.Run("read_file keeps exact integral spellings and rejects fractions and overflow", func(t *testing.T) {
		raw, err := normalize(t, byID["read_file"], tc, "read_file", `{"path":"a","offset":1e0,"limit":2.0}`)
		testassert.NoError(t, err)
		testassert.Require(t, strings.Contains(string(raw), `"offset":1`) && strings.Contains(string(raw), `"limit":2`), "normalized = %s, want exact integral values", raw)
		{
			_, err := normalize(t, byID["read_file"], tc, "read_file", `{"path":"a","limit":2.5}`)
			testassert.Check(t, err != nil, "fractional limit normalized, want rejection")
		}
		{
			_, err := normalize(t, byID["read_file"], tc, "read_file", `{"path":"a","offset":99999999999999999999}`)
			testassert.Check(t, err != nil, "overflowing offset normalized, want rejection")
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
			{
				_, err := normalize(t, byID[toolName], tc, toolName, args)
				testassert.Check(t, err != nil, "%s normalized %s, want rejection", toolName, args)
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
			{
				_, err := normalize(t, byID["process"], tc, "process", args)
				testassert.Check(t, err != nil, "process normalized %s, want rejection", args)
			}
		}
		raw, err := normalize(t, byID["process"], tc, "process", `{"action":"read","id":"deadbeef","_lightcode_receipt":"x","note":"kept"}`)
		testassert.NoError(t, err)
		var normalized map[string]any
		testassert.NoError(t, json.Unmarshal(raw, &normalized))
		testassert.Require(t, normalized["action"] == "read" && normalized["id"] == "deadbeef" && normalized["note"] == "kept", "normalized = %s, want the members preserved", raw)
		{
			_, ok := normalized["_lightcode_receipt"]
			testassert.Require(t, !ok, "normalized arguments retained a private field")
		}
	})
	t.Run("malformed, null and trailing argument bytes are validation errors", func(t *testing.T) {
		for _, args := range []string{``, `not json`, `null`, `[1]`, `{"path":"a"} trailing`, `"str"`} {
			for _, name := range []string{"read_file", "write_file", "edit_file", "apply_patch", "run_command", "process", "sleep"} {
				{
					_, err := normalize(t, byID[name], tc, name, args)
					testassert.Check(t, err != nil, "%s normalized %q, want rejection", name, args)
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
	testassert.NoError(t, err)
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
			byID := directTools(t.TempDir())
			var workspace, link, flipTo, preFlipWriteDir string
			var constraints ToolConstraints
			if cell.slot == "root" {
				realA, realB := t.TempDir(), t.TempDir()
				flipTo = realB
				link = filepath.Join(t.TempDir(), "w")
				workspace = link
				testassert.NoError(t, os.Symlink(realA, link))
			} else {
				workspace = t.TempDir()
				wdA, wdB := t.TempDir(), t.TempDir()
				flipTo = wdB
				preFlipWriteDir = wdA
				link = filepath.Join(workspace, "wd")
				testassert.NoError(t, os.Symlink(wdA, link))
				constraints = ToolConstraints{WriteDir: "wd"}
			}
			if cell.content != "" {
				testassert.NoError(t, os.WriteFile(filepath.Join(flipTo, "target.txt"), []byte(cell.content), 0o644))
			}
			// The seam repoints the symlink on its return: after the plugin's
			// canonical resolutions, before the shared preparation binds.
			orig := canonicalPathsFn
			canonicalPathsFn = func(root, writeDir string) (string, string, error) {
				w, b, err := orig(root, writeDir)
				if err != nil {
					return w, b, err
				}
				testassert.NoError(t, os.Remove(link))
				testassert.NoError(t, os.Symlink(flipTo, link))
				return w, b, err
			}
			defer func() { canonicalPathsFn = orig }()
			plan := prepare(t, byID[cell.toolID], callToolContext(workspace, constraints), cell.toolID, cell.args)
			outcome := plan.Execute(context.Background())
			testassert.Require(t, outcome.Result.Status == model.ResultError && strings.Contains(outcome.Result.Content, "changed"), "result = %+v, want the changed-binding failure without substitution", outcome.Result)
			// The patch path threads the write-dir witness directly, so a
			// dropped witness would fail with the empty-witness form
			// ("changed from  to ..."); the real changed-binding error names
			// the pre-flip canonical write-dir as the bound value.
			testassert.Require(t, !cell.boundWriteDir || strings.Contains(outcome.Result.Content, preFlipWriteDir), "result = %+v, want the changed-binding error naming the bound write-dir %q", outcome.Result, preFlipWriteDir)
		})
	}
}
