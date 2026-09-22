package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

// Direct plugin contract tests. Outside the runtime package only the zero
// Invocation is constructible, so Normalize/Prepare here exercise the
// defaults path; configuration-bearing behavior is covered by
// ValidateConfig directly and by the runtime-composed tests in the runtime
// package.

// testEntry is a valid-looking Harness admitted-input identity.
const (
	testSessionID = "0123456789abcdef0123456789abcdef"
	testEntryID   = "fedcba9876543210fedcba9876543210"
)

// openTools opens the real plugin over a fresh data root and returns its
// tool exports by declared ID.
func openTools(t *testing.T, dataDir string) map[string]runtime.Tool {
	t.Helper()
	return openToolsWithKeys(t, dataDir, nil)
}

// openToolsWithKeys is openTools over a scope carrying the given managed
// env key names.
func openToolsWithKeys(t *testing.T, dataDir string, managedKeys []string) map[string]runtime.Tool {
	t.Helper()
	p := Plugin()
	if p.ID != "tools" || p.Scope != runtime.ScopeRuntime || p.ValidateConfig == nil || p.Open == nil || len(p.Requires) != 0 {
		t.Fatalf("plugin declaration = %+v, want the Runtime-scoped tools plugin with a validator and no dependencies", p)
	}
	if len(p.Provides) != 6 {
		t.Fatalf("plugin declares %d exports, want the four file tools plus run_command and sleep", len(p.Provides))
	}
	inst, err := p.Open(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dataDir, ManagedEnvKeys: managedKeys}, runtime.Bindings{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(inst.Values) != 6 || inst.Close != nil {
		t.Fatalf("instance = %d exports with a non-nil Close, want exactly the six tools and no closer", len(inst.Values))
	}
	byID := make(map[string]runtime.Tool, 6)
	for _, id := range []string{"read_file", "write_file", "edit_file", "apply_patch", "run_command", "sleep"} {
		value, ok := inst.Values[id]
		if !ok {
			t.Fatalf("instance is missing the declared export %q", id)
		}
		tool, ok := value.(runtime.Tool)
		if !ok {
			t.Fatalf("export %q supplies %T, not a runtime.Tool", id, value)
		}
		byID[id] = tool
	}
	return byID
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
	// the plugin declaration/instance assertions run in every test that
	// opens the real plugin (normalize, prepare shapes, execute effects).
	describe := map[string]func(runtime.Invocation, runtime.ToolConstraints, harness.SessionIdentity) (runtime.ToolDescription, error){
		"read_file":   readTool{}.describe,
		"write_file":  writeTool{}.describe,
		"edit_file":   editTool{}.describe,
		"apply_patch": patchTool{}.describe,
		"run_command": runCommandTool{}.describe,
		"sleep":       sleepTool{}.describe,
	}

	cases := []struct {
		name        string
		constraints runtime.ToolConstraints
		available   map[string]bool
	}{
		{"unconstrained agent", runtime.ToolConstraints{}, map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "apply_patch": true, "run_command": true, "sleep": true}},
		{"readonly without write dir", runtime.ToolConstraints{Readonly: true}, map[string]bool{"read_file": true, "write_file": false, "edit_file": false, "apply_patch": false, "run_command": true, "sleep": true}},
		{"readonly with write dir", runtime.ToolConstraints{Readonly: true, WriteDir: "/w"}, map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "apply_patch": true, "run_command": true, "sleep": true}},
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

func TestNormalizeArguments(t *testing.T) {
	byID := openTools(t, t.TempDir())
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
	t.Run("malformed, null and trailing argument bytes are validation errors", func(t *testing.T) {
		for _, args := range []string{``, `not json`, `null`, `[1]`, `{"path":"a"} trailing`, `"str"`} {
			for _, name := range []string{"read_file", "write_file", "edit_file", "apply_patch", "run_command", "sleep"} {
				if _, err := normalize(t, byID[name], tc, name, args); err == nil {
					t.Errorf("%s normalized %q, want rejection", name, args)
				}
			}
		}
	})
}

func TestPrepareShapesAndCanonicals(t *testing.T) {
	byID := openTools(t, t.TempDir())

	t.Run("read_file binds canonical targets with file.read pairs", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["read_file"], callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"notes.txt"}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want one executor", plan)
		}
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "file.read" {
			t.Fatalf("permissions = %+v, want one file.read pair", plan.Permissions)
		}
		if plan.Permissions[0].Target != filepath.Join(ws, "notes.txt") {
			t.Fatalf("target = %q, want the canonical file path", plan.Permissions[0].Target)
		}
		if plan.CanonicalWorkspace != ws || plan.CanonicalWriteDir != "" {
			t.Fatalf("canonicals = %q/%q, want the workspace and an empty write dir", plan.CanonicalWorkspace, plan.CanonicalWriteDir)
		}
	})
	t.Run("read_file of a missing leaf declares the parent directory too", func(t *testing.T) {
		ws := t.TempDir()
		plan := prepare(t, byID["read_file"], callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"ghost.txt"}`)
		if len(plan.Permissions) != 2 {
			t.Fatalf("permissions = %+v, want the file and parent-directory file.read pairs", plan.Permissions)
		}
		for _, pair := range plan.Permissions {
			if pair.Permission != "file.read" || pair.Target == "" {
				t.Fatalf("pair = %+v, want a nonempty canonical file.read pair", pair)
			}
		}
	})
	t.Run("canonical workspace resolves symlinks and stays lexically fixed", func(t *testing.T) {
		real := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["read_file"], callToolContext(link, runtime.ToolConstraints{}), "read_file", `{"path":"a.txt"}`)
		if plan.CanonicalWorkspace != real {
			t.Fatalf("CanonicalWorkspace = %q, want the symlink-resolved %q", plan.CanonicalWorkspace, real)
		}
	})
	t.Run("write_file declares file.write and the canonical write-dir boundary", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, "wd"), 0o755); err != nil {
			t.Fatal(err)
		}
		constraints := runtime.ToolConstraints{WriteDir: "wd"}
		plan := prepare(t, byID["write_file"], callToolContext(ws, constraints), "write_file", `{"path":"wd/out.txt","content":"x"}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want one executor", plan)
		}
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "file.write" {
			t.Fatalf("permissions = %+v, want one file.write pair", plan.Permissions)
		}
		wantBoundary := filepath.Join(ws, "wd")
		if plan.CanonicalWriteDir != wantBoundary || plan.CanonicalWorkspace != ws {
			t.Fatalf("canonicals = %q/%q, want %q/%q", plan.CanonicalWorkspace, plan.CanonicalWriteDir, ws, wantBoundary)
		}
	})
	t.Run("apply_patch declares every source and destination in declaration order", func(t *testing.T) {
		ws := t.TempDir()
		for name, content := range map[string]string{"b.txt": "old\n", "c.txt": "gone\n", "d.txt": "d\n"} {
			if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		patch := "*** Begin Patch\n*** Add File: a.txt\n+new\n*** Update File: b.txt\n@@\n-old\n+new\n*** Delete File: c.txt\n*** Update File: d.txt\n*** Move to: e.txt\n*** End Patch"
		input, err := json.Marshal(map[string]string{"input": patch})
		if err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["apply_patch"], callToolContext(ws, runtime.ToolConstraints{}), "apply_patch", string(input))
		want := []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"}
		if len(plan.Permissions) != len(want) {
			t.Fatalf("permissions = %+v, want %d file.write pairs in declaration order", plan.Permissions, len(want))
		}
		for i, name := range want {
			pair := plan.Permissions[i]
			if pair.Permission != "file.write" || pair.Target != filepath.Join(ws, name) {
				t.Fatalf("pair %d = %+v, want file.write on %s", i, pair, name)
			}
		}
	})
	t.Run("undecodable arguments in Prepare map to the immediate validation error", func(t *testing.T) {
		ws := t.TempDir()
		plan := prepare(t, byID["read_file"], callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":`)
		if plan.Execute != nil || plan.Immediate == nil {
			t.Fatalf("plan = %+v, want one immediate outcome", plan)
		}
		outcome := *plan.Immediate
		if outcome.Result.Status != model.ResultError || outcome.Result.CallID != "call-1" || outcome.Result.Content == "" {
			t.Fatalf("immediate = %+v, want the bounded validation error for the original call", outcome)
		}
	})
	t.Run("prepare normalizes once: it never re-runs argument normalization", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("one\ntwo\nthree"), 0o644); err != nil {
			t.Fatal(err)
		}
		tool := byID["read_file"]
		// The committed normalized arguments are authoritative: a strict
		// "1e0" spelling canonicalizes once at normalization, and
		// preparation consumes the byte-identical committed value and reads
		// exactly line 1 with the canonical integer window.
		committed, err := normalize(t, tool, callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"f.txt","offset":1e0,"limit":1.0}`)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if string(committed) != `{"limit":1,"offset":1,"path":"f.txt"}` {
			t.Fatalf("committed normalized arguments = %s, want the canonical integers once", committed)
		}
		plan := prepare(t, tool, callToolContext(ws, runtime.ToolConstraints{}), "read_file", string(committed))
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.HasPrefix(outcome.Result.Content, "1\tone\n") || !strings.Contains(outcome.Result.Content, "(Showing lines 1-1 of 3. Use offset=2 to continue.)") {
			t.Fatalf("result = %+v, want the canonical 1/1 window through preparation", outcome.Result)
		}
		// Preparation performs no validation or defaulting of its own: an
		// un-normalized fraction handed directly to Prepare is not a second
		// normalization error — preparation binds targets and parses the
		// canonical lexeme only at its point of use.
		plan = prepare(t, tool, callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"f.txt","offset":2.5}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want one executor: Prepare runs no second normalization", plan)
		}
	})
	t.Run("failed canonical preparation maps to the immediate denial", func(t *testing.T) {
		ws := t.TempDir()
		loop := filepath.Join(ws, "loop")
		if err := os.Symlink("loop", loop); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["write_file"], callToolContext(ws, runtime.ToolConstraints{WriteDir: "loop"}), "write_file", `{"path":"out.txt","content":"x"}`)
		if plan.Execute != nil || plan.Immediate == nil {
			t.Fatalf("plan = %+v, want one immediate outcome", plan)
		}
		outcome := *plan.Immediate
		if outcome.Result.Status != model.ResultDenied || outcome.Result.Content != "Permission denied." {
			t.Fatalf("immediate = %+v, want the fixed denial", outcome)
		}
	})
	t.Run("a write target outside the write dir is denied at preparation", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, "wd"), 0o755); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["edit_file"], callToolContext(ws, runtime.ToolConstraints{WriteDir: "wd"}), "edit_file", `{"path":"outside.txt","old_string":"a","new_string":"b"}`)
		if plan.Execute != nil || plan.Immediate == nil || plan.Immediate.Result.Status != model.ResultDenied {
			t.Fatalf("plan = %+v, want the preparation denial", plan)
		}
	})
}

func TestExecuteFileEffects(t *testing.T) {
	dataDir := t.TempDir()
	byID := openTools(t, dataDir)

	t.Run("read_file returns bounded numbered content and no metadata", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["read_file"], callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"notes.txt","offset":2,"limit":2}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.CallID != "call-1" {
			t.Fatalf("result = %+v, want success for call-1", outcome.Result)
		}
		if !strings.Contains(outcome.Result.Content, "2\ttwo") || !strings.Contains(outcome.Result.Content, "3\tthree") {
			t.Fatalf("content = %q, want the numbered requested window", outcome.Result.Content)
		}
		if outcome.Metadata != nil {
			t.Fatalf("metadata = %s, want none for read_file", outcome.Metadata)
		}
	})
	t.Run("read_file of a missing file returns the suggestion outcome", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["read_file"], callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"notes.tx"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.Contains(outcome.Result.Content, "not found") {
			t.Fatalf("result = %+v, want the bounded not-found suggestions outcome", outcome)
		}
	})
	t.Run("write_file creates parents, writes the file and captures the preimage group", func(t *testing.T) {
		dataDir := t.TempDir()
		byID := openTools(t, dataDir)
		ws := t.TempDir()
		plan := prepare(t, byID["write_file"], callToolContext(ws, runtime.ToolConstraints{}), "write_file", `{"path":"sub/dir/new.txt","content":"body"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.Contains(outcome.Result.Content, "Wrote") {
			t.Fatalf("result = %+v, want the write success", outcome.Result)
		}
		data, err := os.ReadFile(filepath.Join(ws, "sub", "dir", "new.txt"))
		if err != nil || string(data) != "body" {
			t.Fatalf("written file = (%q, %v), want the new content", data, err)
		}
		if outcome.Metadata != nil {
			t.Fatalf("metadata = %s, want none for write_file", outcome.Metadata)
		}
		// The preimage group lives at DataDir/code/<SessionID>/<EntryID>/snapshots/1/.
		turnDir := filepath.Join(groupDir(dataDir, testSessionID, testEntryID), "snapshots", "1")
		entries, err := os.ReadDir(turnDir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("group turn dir = (%d entries, %v), want one snapshot entry", len(entries), err)
		}
		meta, err := os.ReadFile(filepath.Join(turnDir, entries[0].Name(), "meta.json"))
		if err != nil {
			t.Fatalf("snapshot meta: %v", err)
		}
		var wire struct {
			OriginalPath string `json:"original_path"`
			Existed      bool   `json:"existed"`
		}
		if err := json.Unmarshal(meta, &wire); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		if wire.Existed || !strings.HasSuffix(wire.OriginalPath, "new.txt") {
			t.Fatalf("meta = %s, want the new-file preimage of the declared target", meta)
		}
	})
	t.Run("edit_file mutates the file and emits the edit_preview metadata shape", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["edit_file"], callToolContext(ws, runtime.ToolConstraints{}), "edit_file", `{"path":"notes.txt","old_string":"beta","new_string":"BETA"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.Contains(outcome.Result.Content, "lines 2") {
			t.Fatalf("result = %+v, want the edit summary", outcome.Result)
		}
		data, err := os.ReadFile(filepath.Join(ws, "notes.txt"))
		if err != nil || string(data) != "alpha\nBETA\ngamma\n" {
			t.Fatalf("edited file = (%q, %v), want the replacement", data, err)
		}
		if outcome.Metadata == nil {
			t.Fatal("edit_file emitted no metadata")
		}
		var metadata struct {
			EditPreview *struct {
				Hunks []struct {
					Rows []struct {
						Kind string `json:"kind"`
						Text string `json:"text"`
					} `json:"rows"`
				} `json:"hunks"`
			} `json:"edit_preview"`
		}
		if err := json.Unmarshal(outcome.Metadata, &metadata); err != nil {
			t.Fatalf("metadata %s is not the documented shape: %v", outcome.Metadata, err)
		}
		if metadata.EditPreview == nil || len(metadata.EditPreview.Hunks) != 1 || len(metadata.EditPreview.Hunks[0].Rows) != 2 {
			t.Fatalf("metadata = %s, want one hunk with the remove/add diff rows", outcome.Metadata)
		}
	})
	t.Run("apply_patch applies add update move delete and emits the per-file preview shape", func(t *testing.T) {
		ws := t.TempDir()
		for name, content := range map[string]string{"b.txt": "old\n", "c.txt": "gone\n", "d.txt": "d\n"} {
			if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		patch := "*** Begin Patch\n*** Add File: a.txt\n+new\n*** Update File: b.txt\n@@\n-old\n+new\n*** Delete File: c.txt\n*** Update File: d.txt\n*** Move to: e.txt\n*** End Patch"
		input, err := json.Marshal(map[string]string{"input": patch})
		if err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["apply_patch"], callToolContext(ws, runtime.ToolConstraints{}), "apply_patch", string(input))
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.Contains(outcome.Result.Content, "Success. Updated the following files:") {
			t.Fatalf("result = %+v, want the patch success summary", outcome.Result)
		}
		if _, err := os.Stat(filepath.Join(ws, "a.txt")); err != nil {
			t.Fatalf("added file missing: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(ws, "b.txt"))
		if err != nil || string(data) != "new\n" {
			t.Fatalf("updated file = (%q, %v), want the replacement", data, err)
		}
		if _, err := os.Stat(filepath.Join(ws, "c.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted file stat = %v, want gone", err)
		}
		if _, err := os.Stat(filepath.Join(ws, "d.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("move source stat = %v, want gone", err)
		}
		if data, err := os.ReadFile(filepath.Join(ws, "e.txt")); err != nil || string(data) != "d\n" {
			t.Fatalf("move destination = (%q, %v), want the renamed content", data, err)
		}
		if outcome.Metadata == nil {
			t.Fatal("apply_patch emitted no metadata")
		}
		var metadata struct {
			Files []struct {
				Path string `json:"path"`
				Op   string `json:"op"`
			} `json:"edit_preview_files"`
		}
		if err := json.Unmarshal(outcome.Metadata, &metadata); err != nil {
			t.Fatalf("metadata %s is not the documented shape: %v", outcome.Metadata, err)
		}
		var ops []string
		for _, f := range metadata.Files {
			ops = append(ops, f.Path+"="+f.Op)
		}
		// The engine emits the move destination (M) before its source (D).
		if strings.Join(ops, ",") != "a.txt=A,b.txt=M,c.txt=D,e.txt=M,d.txt=D" {
			t.Fatalf("previews = %v, want the full A/M/D list with content retained", ops)
		}
	})
	t.Run("successive calls of one operation share the disk group through fresh handles", func(t *testing.T) {
		dataDir := t.TempDir()
		byID := openTools(t, dataDir)
		ws := t.TempDir()
		first := prepare(t, byID["write_file"], callToolContext(ws, runtime.ToolConstraints{}), "write_file", `{"path":"one.txt","content":"1"}`)
		if outcome := first.Execute(context.Background()); outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("first write = %+v", outcome.Result)
		}
		second := prepare(t, byID["write_file"], callToolContext(ws, runtime.ToolConstraints{}), "write_file", `{"path":"two.txt","content":"2"}`)
		if outcome := second.Execute(context.Background()); outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("second write = %+v", outcome.Result)
		}
		turnDir := filepath.Join(groupDir(dataDir, testSessionID, testEntryID), "snapshots", "1")
		entries, err := os.ReadDir(turnDir)
		if err != nil || len(entries) != 2 {
			t.Fatalf("group turn dir = (%d entries, %v), want both calls' preimages in the one group", len(entries), err)
		}
	})
	t.Run("another operation's admitted input opens its own group", func(t *testing.T) {
		dataDir := t.TempDir()
		byID := openTools(t, dataDir)
		ws := t.TempDir()
		tc := callToolContext(ws, runtime.ToolConstraints{})
		tc.AdmittedEntry = harness.EntryRef{SessionID: testSessionID, EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		plan := prepare(t, byID["write_file"], tc, "write_file", `{"path":"other.txt","content":"x"}`)
		if outcome := plan.Execute(context.Background()); outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("write = %+v", outcome.Result)
		}
		turnDir := filepath.Join(groupDir(dataDir, testSessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "snapshots", "1")
		if entries, err := os.ReadDir(turnDir); err != nil || len(entries) != 1 {
			t.Fatalf("other group turn dir = (%d entries, %v), want exactly one entry", len(entries), err)
		}
	})
	t.Run("a readonly agent with a write dir writes inside the boundary", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, "wd"), 0o755); err != nil {
			t.Fatal(err)
		}
		tc := callToolContext(ws, runtime.ToolConstraints{Readonly: true, WriteDir: "wd"})
		plan := prepare(t, byID["write_file"], tc, "write_file", `{"path":"wd/allowed.txt","content":"x"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("write = %+v, want success inside the write dir", outcome.Result)
		}
	})
	t.Run("executed patch failures surface as error outcomes", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("different\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		patch := "*** Begin Patch\n*** Update File: b.txt\n@@\n-old\n+new\n*** End Patch"
		input, err := json.Marshal(map[string]string{"input": patch})
		if err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["apply_patch"], callToolContext(ws, runtime.ToolConstraints{}), "apply_patch", string(input))
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError || outcome.Result.Content == "" {
			t.Fatalf("result = %+v, want the failed-hunk error outcome", outcome.Result)
		}
	})
}

// TestFailedCallRetainsEarlierSnapshot proves the failed-call retention
// sibling at the plugin boundary: a failed mutating call in the SAME
// Operation's code group — reopened through a fresh handle per call —
// discards only its own pre-mutation claim and never the earlier
// successful call's retained preimage.
func TestFailedCallRetainsEarlierSnapshot(t *testing.T) {
	dataDir := t.TempDir()
	byID := openTools(t, dataDir)
	ws := t.TempDir()
	tc := callToolContext(ws, runtime.ToolConstraints{})
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First call: a successful write retains its preimage in the group.
	first := prepare(t, byID["write_file"], tc, "write_file", `{"path":"a.txt","content":"ALPHA\n"}`)
	if outcome := first.Execute(context.Background()); outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("first write = %+v, want success", outcome.Result)
	}

	// Second call: a failing edit in the same group — the shared body
	// captures b.txt's preimage, fails before any mutation, and discards
	// its own claim through the same fresh-handle group.
	second := prepare(t, byID["edit_file"], tc, "edit_file", `{"path":"b.txt","old_string":"missing","new_string":"x"}`)
	if outcome := second.Execute(context.Background()); outcome.Result.Status != model.ResultError {
		t.Fatalf("failing edit = %+v, want the error outcome", outcome.Result)
	}

	// The first call's retained preimage survives on disk; the failing
	// call discarded its own claim, so the turn holds exactly that one
	// entry.
	turnDir := filepath.Join(groupDir(dataDir, testSessionID, testEntryID), "snapshots", "1")
	entries, err := os.ReadDir(turnDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("group turn dir = (%d entries, %v), want only the first call's retained preimage", len(entries), err)
	}
	metaData, err := os.ReadFile(filepath.Join(turnDir, entries[0].Name(), "meta.json"))
	if err != nil {
		t.Fatalf("snapshot meta: %v", err)
	}
	var meta struct {
		OriginalPath string `json:"original_path"`
	}
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if !strings.HasSuffix(meta.OriginalPath, "a.txt") {
		t.Fatalf("retained entry = %q, want the first call's a.txt preimage", meta.OriginalPath)
	}
	preimage, err := os.ReadFile(filepath.Join(turnDir, entries[0].Name(), "original"))
	if err != nil || string(preimage) != "alpha\n" {
		t.Fatalf("retained preimage = (%q, %v), want the first call's content", preimage, err)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "a.txt")); err != nil || string(data) != "ALPHA\n" {
		t.Fatalf("written file = (%q, %v), want the first call's mutation intact", data, err)
	}
}

// TestExecutedPlanRejectsPostAuthorizationBindingChange proves the R5
// recovery sibling at the plugin boundary: a prepared canonical target whose
// binding changes between preparation and execution fails without
// substituting a changed path and without mutating through the repointed
// leaf.
func TestExecutedPlanRejectsPostAuthorizationBindingChange(t *testing.T) {
	dataDir := t.TempDir()
	byID := openTools(t, dataDir)
	ws := t.TempDir()
	real := filepath.Join(ws, "real.txt")
	if err := os.WriteFile(real, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "alias.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	plan := prepare(t, byID["write_file"], callToolContext(ws, runtime.ToolConstraints{}), "write_file", `{"path":"alias.txt","content":"after"}`)
	// Repoint the alias at a different leaf after authorization.
	other := filepath.Join(ws, "other.txt")
	if err := os.WriteFile(other, []byte("do not touch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	outcome := plan.Execute(context.Background())
	if outcome.Result.Status != model.ResultError || !strings.Contains(outcome.Result.Content, "resolve path") {
		t.Fatalf("result = %+v, want the binding-change error", outcome.Result)
	}
	data, err := os.ReadFile(other)
	if err != nil || string(data) != "do not touch\n" {
		t.Fatalf("repointed leaf = (%q, %v), want it unmutated", data, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "code")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed pre-mutation call left %s: %v", filepath.Join(dataDir, "code"), err)
	}
}

// TestExecutedReadFailsUnderWorkspaceRootChange proves the closure
// revalidates the bound canonical Workspace root: repointing the root
// symlink after preparation fails the execution without reading the
// substituted tree.
func TestExecutedReadFailsUnderWorkspaceRootChange(t *testing.T) {
	byID := openTools(t, t.TempDir())
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "notes.txt"), []byte("real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "ws-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	plan := prepare(t, byID["read_file"], callToolContext(link, runtime.ToolConstraints{}), "read_file", `{"path":"notes.txt"}`)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "notes.txt"), []byte("substituted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	outcome := plan.Execute(context.Background())
	if outcome.Result.Status != model.ResultError || strings.Contains(outcome.Result.Content, "substituted") {
		t.Fatalf("result = %+v, want the changed-root failure without substituted content", outcome.Result)
	}
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
// resolutions and the shared preparation fails the execution with the
// changed-binding error instead of executing under the repointed tree. One
// cell per entry and bound slot: read binds the root; write, edit and
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
		{"write_file revalidates the write-dir witness", "write_file", `{"path":"wd/target.txt","content":"after"}`, "write_dir", "", false},
		{"edit_file revalidates the root witness", "edit_file", `{"path":"target.txt","old_string":"beta","new_string":"BETA"}`, "root", "beta\n", false},
		{"edit_file revalidates the write-dir witness", "edit_file", `{"path":"wd/target.txt","old_string":"beta","new_string":"BETA"}`, "write_dir", "beta\n", false},
		{"apply_patch revalidates the root witness", "apply_patch", bindingChangePatchInput(t, "target.txt"), "root", "", false},
		{"apply_patch revalidates the write-dir witness", "apply_patch", bindingChangePatchInput(t, "wd/target.txt"), "write_dir", "", true},
	}
	for _, cell := range cells {
		cell := cell
		t.Run(cell.name, func(t *testing.T) {
			byID := openTools(t, t.TempDir())
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
