package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
)

func TestApplyPatchValidateStagedStructureOnly(t *testing.T) {
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, t.TempDir())

	// Good patch: structure is valid; no FS access.
	if err := tool.ValidateStaged(context.Background(), jsonRaw(t, `{"input":"*** Begin Patch\n*** Add File: a.txt\n+x\n*** End Patch"}`)); err != nil {
		t.Fatalf("ValidateStaged good err = %v, want nil", err)
	}
}

func TestApplyPatchValidateStagedRejectsUnparseable(t *testing.T) {
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, t.TempDir())
	if err := tool.ValidateStaged(context.Background(), jsonRaw(t, `{"input":"garbage"}`)); err == nil {
		t.Fatal("ValidateStaged err = nil, want non-nil for unparseable input")
	}
}

func TestApplyPatchValidateStagedRejectsMissingInput(t *testing.T) {
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, t.TempDir())
	if err := tool.ValidateStaged(context.Background(), jsonRaw(t, `{}`)); err == nil {
		t.Fatal("ValidateStaged err = nil, want non-nil for missing input")
	}
}

func TestApplyPatchValidateStagedNoFSAccess(t *testing.T) {
	// Invariant 2: ValidateStaged must not touch the filesystem. A
	// patch referring to a non-existent file must still validate
	// successfully (the existence check happens in the apply engine,
	// after permission approval).
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, t.TempDir())
	// Build the args JSON programmatically (the patch contains newlines
	// that need JSON-escaping; using a string concatenation here would
	// produce invalid JSON).
	args := []byte(`{"input":"*** Begin Patch\n*** Update File: /no/such/file\n@@\n-x\n+y\n*** End Patch"}`)
	if err := tool.ValidateStaged(context.Background(), args); err != nil {
		t.Fatalf("ValidateStaged FS-touching err = %v, want nil (structure-only)", err)
	}
}

func TestApplyPatchStagedResultMessage(t *testing.T) {
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, t.TempDir())
	if got := tool.StagedResultMessage(); got != "Staged." {
		t.Fatalf("StagedResultMessage = %q, want %q", got, "Staged.")
	}
}

func TestApplyPatchStagedFlushAppliesViaOwnCommit(t *testing.T) {
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Allow: []string{ruleFor(dir, "**")}}),
		denyAskAction(t),
	)

	input := applyPatchInput(t, "*** Add File: a.txt\n+hi\n*** Add File: b.txt\n+there")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{
			ToolName:   "apply_patch",
			ToolCallID: "1",
			Args:       input,
			Params:     map[string]any{"input": input},
		},
	})
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Error != "" {
		t.Fatalf("results[0].Error = %q, want empty", results[0].Error)
	}
	if !results[0].Success {
		t.Fatalf("results[0].Success = false, want true")
	}
	if !strings.Contains(results[0].Result, "A a.txt") || !strings.Contains(results[0].Result, "A b.txt") {
		t.Fatalf("results[0].Result = %q, want it to list both Adds", results[0].Result)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("a.txt not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatalf("b.txt not created: %v", err)
	}
}

func TestApplyPatchStagedFlushPartialFailureSetsError(t *testing.T) {
	// First op's snapshot succeeds (the Add lands). Second op's
	// snapshot returns an error (simulated mid-write I/O failure
	// after validation passed). The staged result must carry the
	// A summary + the I/O error in BatchResult.Error so the existing
	// emitPendingResults branch routes it to the model with
	// isError = true (Invariant 5, no struct change).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1, failOnCall: 2, errFail: errSimulatedIO}
	tracker := NewFileTracker()
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Allow: []string{ruleFor(dir, "**")}}),
		denyAskAction(t),
	)

	input := applyPatchInput(t, "*** Add File: new.txt\n+hi\n*** Update File: x.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{
			ToolName:   "apply_patch",
			ToolCallID: "1",
			Args:       input,
			Params:     map[string]any{"input": input},
		},
	})
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Error == "" {
		t.Fatal("results[0].Error = empty, want it to carry the partial-failure body")
	}
	if !strings.Contains(results[0].Error, "A new.txt") {
		t.Fatalf("results[0].Error = %q, want it to include the A new.txt committed summary", results[0].Error)
	}
	if !strings.Contains(results[0].Error, "simulated io error") {
		t.Fatalf("results[0].Error = %q, want it to include the snapshot error", results[0].Error)
	}
}

func TestApplyPatchStagedFlushTwoCallsInEmissionOrder(t *testing.T) {
	// Two apply_patch calls in the same batch apply in emission order.
	// A failing second call does not block the first.
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Allow: []string{ruleFor(dir, "**")}}),
		denyAskAction(t),
	)

	first := applyPatchInput(t, "*** Add File: first.txt\n+hi")
	second := applyPatchInput(t, "*** Add File: second.txt\n+there")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{ToolName: "apply_patch", ToolCallID: "1", Args: first, Params: map[string]any{"input": first}},
		{ToolName: "apply_patch", ToolCallID: "2", Args: second, Params: map[string]any{"input": second}},
	})
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	for i, r := range results {
		if r.Error != "" {
			t.Fatalf("results[%d].Error = %q, want empty", i, r.Error)
		}
		if !r.Success {
			t.Fatalf("results[%d].Success = false, want true", i)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "first.txt")); err != nil {
		t.Fatalf("first.txt not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "second.txt")); err != nil {
		t.Fatalf("second.txt not created: %v", err)
	}
}

func TestApplyPatchStagedFlushDenialDoesNotApply(t *testing.T) {
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Deny: []string{ruleFor(dir, "a.txt")}}),
		denyAskAction(t),
	)

	input := applyPatchInput(t, "*** Add File: a.txt\n+hi")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{ToolName: "apply_patch", ToolCallID: "1", Args: input, Params: map[string]any{"input": input}},
	})
	if results[0].Error != "denied by user" {
		t.Fatalf("results[0].Error = %q, want %q", results[0].Error, "denied by user")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("a.txt exists despite deny: stat err = %v", err)
	}
}

func TestApplyPatchStagedBatchPromptListsAllPatchFiles(t *testing.T) {
	// A staged batch that includes an apply_patch call must show the
	// patch's touched files in the ask request's BatchFiles, not just
	// the staged batch's path-keyed files (which would be empty for
	// apply_patch). The shared helper does this; verify the staged
	// batch's batchFiles (computed at the top of ExecutePending)
	// includes the patch's files.
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Ask: []string{ruleFor(dir, "**")}}),
		func(_ context.Context, req permission.Request) permission.ResponseAction {
			if req.ToolName != "apply_patch" {
				return permission.ResponseDeny
			}
			if len(req.BatchFiles) != 2 {
				t.Errorf("ask BatchFiles len = %d, want 2: %v", len(req.BatchFiles), req.BatchFiles)
			}
			return permission.ResponseDeny
		},
	)

	input := applyPatchInput(t, "*** Add File: a.txt\n+hi\n*** Add File: b.txt\n+there")
	_ = executor.ExecutePending(context.Background(), []StagedCall{
		{ToolName: "apply_patch", ToolCallID: "1", Args: input, Params: map[string]any{"input": input}},
	})
}

func TestApplyPatchStagedEditFileRegression(t *testing.T) {
	// Regression: a staged batch with only edit_file / write_file
	// must still flush through executeFileGroup (unchanged). Mixing
	// in an apply_patch goes through the new dispatch branch; the
	// edit_file / write_file calls still go through executeFileGroup.
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, path, 1, 100)
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{
			Allow: []string{"edit_file(/a.txt)"},
		}),
		denyAskAction(t),
	)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		{
			ToolName:   "edit_file",
			ToolCallID: "1",
			Args:       `{"path":"` + path + `","old_string":"hi","new_string":"bye"}`,
			Params: map[string]any{
				"path":       path,
				"old_string": "hi",
				"new_string": "bye",
			},
		},
	})
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Error != "" {
		t.Fatalf("edit_file staged err = %q, want empty (regression)", results[0].Error)
	}
	if got := readFile(t, path); got != "bye" {
		t.Fatalf("a.txt = %q, want %q (edit_file flush unchanged)", got, "bye")
	}
}

// jsonRaw returns the raw JSON args a model would emit. Used by
// ValidateStaged tests (which take json.RawMessage).
func jsonRaw(t *testing.T, s string) []byte {
	t.Helper()
	return []byte(s)
}

// jsonString returns s as a JSON string literal (with quotes).
func jsonString(s string) string {
	return `"` + s + `"`
}

// denyAskAction returns an AskActionFunc that fails the test if ask
// is called. Mirrors denyIfAsked in permwrap_test.go but for the
// staged executor's signature.
func denyAskAction(t *testing.T) AskActionFunc {
	t.Helper()
	return func(_ context.Context, req permission.Request) permission.ResponseAction {
		t.Fatalf("ask called for %s %s", req.ToolName, req.Arg)
		return permission.ResponseDeny
	}
}

var errSimulatedIO = ioError("simulated io error")

type ioError string

func (e ioError) Error() string { return string(e) }
