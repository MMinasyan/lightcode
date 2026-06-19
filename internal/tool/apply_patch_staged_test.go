package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/editpreview"
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
	// ValidateStaged must not touch the filesystem. A patch referring to a
	// non-existent file must still validate successfully (the existence check
	// happens in the apply engine, after permission approval).
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

func TestApplyPatchStagedFlushEmitsPreviewMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	executor := NewStagedExecutorAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Allow: []string{ruleFor(dir, "**")}}),
		denyAskAction(t),
	)

	input := applyPatchInput(t, "*** Update File: x.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{ToolName: "apply_patch", ToolCallID: "1", Args: input, Params: map[string]any{"input": input}},
	})
	if results[0].Error != "" || !results[0].Success {
		t.Fatalf("result = %+v, want success", results[0])
	}
	files, ok := editpreview.FilesFromMetadata(results[0].Metadata)
	if !ok {
		t.Fatalf("metadata = %#v, want edit_preview_files", results[0].Metadata)
	}
	if len(files) != 1 || files[0].Path != "x.txt" || files[0].Op != "M" {
		t.Fatalf("metadata files = %+v, want M x.txt", files)
	}
}

func TestApplyPatchStagedMixedBatchRejectsWithoutMutation(t *testing.T) {
	for _, toolName := range []string{"edit_file", "write_file"} {
		t.Run(toolName, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a.txt")
			if err := os.WriteFile(path, []byte("hi"), 0o644); err != nil {
				t.Fatal(err)
			}
			tracker := NewFileTracker()
			trackIdentityForPath(t, tracker, path, 1, 100)
			asked := 0
			executor := NewStagedExecutorAtRoot(&applyPatchStore{turn: 1}, tracker, config.ToolsConfig{}, dir,
				rulesCheck(dir, permission.Rules{Allow: []string{ruleFor(dir, "**"), toolName + "(/a.txt)"}}),
				func(_ context.Context, req permission.Request) permission.ResponseAction {
					asked++
					return permission.ResponseAllow
				},
			)
			patch := applyPatchInput(t, "*** Add File: b.txt\n+new")
			otherParams := map[string]any{"path": path, "old_string": "hi", "new_string": "bye"}
			if toolName == "write_file" {
				otherParams = map[string]any{"path": path, "content": "bye"}
			}
			results := executor.ExecutePending(context.Background(), []StagedCall{
				{ToolName: "apply_patch", ToolCallID: "1", Args: patch, Params: map[string]any{"input": patch}},
				{ToolName: toolName, ToolCallID: "2", Args: `{}`, Params: otherParams},
			})
			if asked != 0 {
				t.Fatalf("asked = %d, want 0", asked)
			}
			for i, r := range results {
				if !strings.Contains(r.Error, "cannot be mixed") {
					t.Fatalf("results[%d].Error = %q, want mixed-batch rejection", i, r.Error)
				}
			}
			if got := readFile(t, path); got != "hi" {
				t.Fatalf("a.txt = %q, want unchanged", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "b.txt")); !os.IsNotExist(err) {
				t.Fatalf("b.txt exists despite mixed rejection: %v", err)
			}
		})
	}
}

func TestApplyPatchStagedSymlinkApprovalUsesCanonicalReceipt(t *testing.T) {
	dir := t.TempDir()
	real1 := filepath.Join(dir, "real1.txt")
	real2 := filepath.Join(dir, "real2.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(real1, []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real2, []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real1, link); err != nil {
		t.Fatal(err)
	}
	asked := 0
	executor := NewStagedExecutorAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Ask: []string{ruleFor(dir, "**")}}),
		func(_ context.Context, req permission.Request) permission.ResponseAction {
			asked++
			if len(req.BatchFiles) != 1 || req.BatchFiles[0] != link {
				t.Fatalf("BatchFiles = %v, want [%s]", req.BatchFiles, link)
			}
			if len(req.BatchResolvedFiles) != 1 || req.BatchResolvedFiles[0] != real1 {
				t.Fatalf("BatchResolvedFiles = %v, want [%s]", req.BatchResolvedFiles, real1)
			}
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real2, link); err != nil {
				t.Fatal(err)
			}
			return permission.ResponseAllow
		},
	)

	input := applyPatchInput(t, "*** Update File: link.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{ToolName: "apply_patch", ToolCallID: "1", Args: input, Params: map[string]any{"input": input}},
	})
	if asked != 1 {
		t.Fatalf("asked = %d, want 1", asked)
	}
	if len(results) != 1 || !strings.Contains(results[0].Error, "approved canonical path changed") {
		t.Fatalf("results = %+v, want canonical-change error", results)
	}
	if got := readFile(t, real1); got != "alpha\nBEFORE\nbeta" {
		t.Fatalf("real1 = %q, want unchanged", got)
	}
	if got := readFile(t, real2); got != "alpha\nBEFORE\nbeta" {
		t.Fatalf("real2 = %q, want unchanged", got)
	}
}

func TestApplyPatchStagedFlushPartialFailureSetsError(t *testing.T) {
	// First op's snapshot succeeds (the Add lands). Second op's
	// snapshot returns an error (simulated mid-write I/O failure
	// after validation passed). The staged result must carry the
	// A summary + the I/O error in BatchResult.Error so the existing
	// emitPendingResults branch routes it to the model with isError = true.
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

func TestApplyPatchStagedBatchCanAllowAllFalse(t *testing.T) {
	// DESIGN: Allow all is only for staged edit/write batch prompts.
	// A pure apply_patch batch must not expose CanAllowAll, even when
	// it contains multiple calls. The user sees only the current
	// patch's files; approving later patches unseen would be unsafe.
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	var seenCanAllowAll *bool
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Ask: []string{ruleFor(dir, "**")}}),
		func(_ context.Context, req permission.Request) permission.ResponseAction {
			v := req.CanAllowAll
			seenCanAllowAll = &v
			return permission.ResponseDeny
		},
	)

	first := applyPatchInput(t, "*** Add File: a.txt\n+hi")
	second := applyPatchInput(t, "*** Add File: b.txt\n+there")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{ToolName: "apply_patch", ToolCallID: "1", Args: first, Params: map[string]any{"input": first}},
		{ToolName: "apply_patch", ToolCallID: "2", Args: second, Params: map[string]any{"input": second}},
	})
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	if seenCanAllowAll == nil {
		t.Fatal("ask was never called; expected at least one permission prompt")
	}
	if *seenCanAllowAll {
		t.Fatalf("CanAllowAll = true, want false for pure apply_patch batch")
	}
}

func TestApplyPatchStagedForgedAllowAllDoesNotApproveLaterPatch(t *testing.T) {
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	asked := 0
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Ask: []string{ruleFor(dir, "**")}}),
		func(_ context.Context, req permission.Request) permission.ResponseAction {
			asked++
			if req.CanAllowAll {
				t.Fatalf("CanAllowAll = true for apply_patch request")
			}
			if asked == 1 {
				return permission.ResponseAllowAll
			}
			return permission.ResponseDeny
		},
	)

	first := applyPatchInput(t, "*** Add File: a.txt\n+hi")
	second := applyPatchInput(t, "*** Add File: b.txt\n+there")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{ToolName: "apply_patch", ToolCallID: "1", Args: first, Params: map[string]any{"input": first}},
		{ToolName: "apply_patch", ToolCallID: "2", Args: second, Params: map[string]any{"input": second}},
	})
	if asked != 2 {
		t.Fatalf("permission prompts = %d, want 2", asked)
	}
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	if !results[0].Success {
		t.Fatalf("first result = %+v, want success", results[0])
	}
	if results[1].Error != "denied by user" {
		t.Fatalf("second result error = %q, want denied by user", results[1].Error)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "hi" {
		t.Fatalf("a.txt = %q, want hi", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("b.txt exists after forged allow_all: %v", err)
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
