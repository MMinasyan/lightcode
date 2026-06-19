package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/config"
)

func applyPatchInput(t *testing.T, s string) string {
	t.Helper()
	return "*** Begin Patch\n" + s + "\n*** End Patch"
}

// applyPatchStore is a SnapshotStore that tolerates non-existent files
// (recording "didn't exist" as the captured state) and records every
// call. The real snapshot store does this; the test store used by
// edit_file tests assumes a pre-existing file, which is wrong for
// apply_patch's Add and Move-destination cases.
type applyPatchStore struct {
	turn  int
	calls []snapshotCall
	seen  map[string]string
	// failOnCall, when > 0, makes the Nth snapshot call return errFail
	// (used to simulate a mid-write failure at the second op).
	failOnCall int
	errFail    error
}

func (s *applyPatchStore) Snapshot(turn int, absPath string) error {
	return s.SnapshotResolved(turn, absPath, absPath)
}

func (s *applyPatchStore) SnapshotResolved(turn int, originalPath, canonicalPath string) error {
	s.calls = append(s.calls, snapshotCall{turn: turn, path: originalPath, canonical: canonicalPath})
	if s.failOnCall > 0 && len(s.calls) == s.failOnCall {
		return s.errFail
	}
	if s.seen == nil {
		s.seen = map[string]string{}
	}
	data, err := os.ReadFile(canonicalPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.seen[canonicalPath] = "" // captured "didn't exist"
			return nil
		}
		return err
	}
	s.seen[canonicalPath] = string(data)
	return nil
}

func (s *applyPatchStore) CurrentTurn() int { return s.turn }

func runApplyPatch(t *testing.T, tool *ApplyPatch, params map[string]any) (string, error) {
	t.Helper()
	return tool.Execute(context.Background(), params)
}

func TestApplyPatchAddsNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Add File: new.txt\n+hello\n+world")
	result, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if !strings.Contains(result, "Success. Updated the following files:") {
		t.Fatalf("result = %q, want it to contain success summary", result)
	}
	if !strings.Contains(result, "A new.txt") {
		t.Fatalf("result = %q, want A new.txt line", result)
	}
	if got := readFile(t, path); got != "hello\nworld" {
		t.Fatalf("file = %q, want %q", got, "hello\nworld")
	}
	if len(store.calls) != 1 {
		t.Fatalf("snapshot calls = %d, want 1 (Add of new.txt)", len(store.calls))
	}
}

func TestApplyPatchAddFailsIfFileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Add File: exists.txt\n+new")
	_, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err == nil {
		t.Fatal("Execute err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want it to mention already-exists", err)
	}
	if got := readFile(t, path); got != "old" {
		t.Fatalf("file = %q, want unchanged %q", got, "old")
	}
}

func TestApplyPatchUpdatesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Update File: a.go\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	result, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if !strings.Contains(result, "M a.go") {
		t.Fatalf("result = %q, want M a.go line", result)
	}
	if got := readFile(t, path); got != "alpha\nAFTER\nbeta" {
		t.Fatalf("file = %q, want %q", got, "alpha\nAFTER\nbeta")
	}
}

func TestApplyPatchUpdateFailsIfHunkContextMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("alpha\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Update File: a.go\n@@\n-missing\n+new")
	_, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err == nil {
		t.Fatal("Execute err = nil, want non-nil")
	}
	if !strings.HasPrefix(err.Error(), "Failed to find expected lines in a.go:") {
		t.Fatalf("err = %v, want Codex's pattern-miss prefix", err)
	}
	if got := readFile(t, path); got != "alpha\nbeta" {
		t.Fatalf("file = %q, want unchanged %q (all-or-nothing)", got, "alpha\nbeta")
	}
}

func TestApplyPatchDeletesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(path, []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Delete File: old.txt")
	result, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if !strings.Contains(result, "D old.txt") {
		t.Fatalf("result = %q, want D old.txt line", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still exists: stat err = %v", err)
	}
}

func TestApplyPatchDeleteFailsIfFileAbsent(t *testing.T) {
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Delete File: ghost.txt")
	_, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err == nil {
		t.Fatal("Execute err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want it to mention does-not-exist", err)
	}
}

func TestApplyPatchMovesFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.go")
	dst := filepath.Join(dir, "new.go")
	if err := os.WriteFile(src, []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Update File: old.go\n*** Move to: new.go\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	result, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if !strings.Contains(result, "M new.go") {
		t.Fatalf("result = %q, want M new.go line (Move labels by destination)", result)
	}
	if !strings.Contains(result, "D old.go") {
		t.Fatalf("result = %q, want D old.go line (source removed)", result)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still exists: stat err = %v", err)
	}
	if got := readFile(t, dst); got != "alpha\nAFTER\nbeta" {
		t.Fatalf("dest = %q, want %q", got, "alpha\nAFTER\nbeta")
	}
}

func TestApplyPatchMoveFailsIfDestinationExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.go")
	dst := filepath.Join(dir, "new.go")
	if err := os.WriteFile(src, []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("preexisting"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Update File: old.go\n*** Move to: new.go\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	_, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err == nil {
		t.Fatal("Execute err = nil, want non-nil (move destination already exists)")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want already-exists error", err)
	}
	if got := readFile(t, src); got != "alpha\nBEFORE\nbeta" {
		t.Fatalf("source modified: got %q, want unchanged", got)
	}
	if got := readFile(t, dst); got != "preexisting" {
		t.Fatalf("destination modified: got %q, want unchanged", got)
	}
}

func TestApplyPatchAllOrNothingValidationFailure(t *testing.T) {
	// First op is a successful Add; second op is an Update of a
	// non-existent file. Validation runs the existence check before any
	// write, so the Add must not land (validate-first all-or-nothing).
	dir := t.TempDir()
	newPath := filepath.Join(dir, "new.txt")
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Add File: new.txt\n+hello\n*** Update File: ghost.go\n@@\n-missing\n+replacement")
	_, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err == nil {
		t.Fatal("Execute err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want does-not-exist error", err)
	}
	if _, statErr := os.Stat(newPath); !os.IsNotExist(statErr) {
		t.Fatalf("Add landed despite later op's validation failure: stat err = %v", statErr)
	}
}

func TestApplyPatchAllOrNothingPartialApplyError(t *testing.T) {
	// First op is a successful Add; second op is a Delete of a file that
	// doesn't exist. Validation is all-or-nothing: the existence check on
	// the Delete fails, so the Add is not written either.
	dir := t.TempDir()
	newPath := filepath.Join(dir, "new.txt")
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Add File: new.txt\n+hi\n*** Delete File: ghost.txt")
	_, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err == nil {
		t.Fatal("Execute err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want does-not-exist error", err)
	}
	if _, statErr := os.Stat(newPath); !os.IsNotExist(statErr) {
		t.Fatalf("Add landed despite later op's validation failure: stat err = %v", statErr)
	}
}

func TestApplyPatchNoReadGate(t *testing.T) {
	// Decision 12: apply_patch never calls tracker.WasReadCheckIdentity.
	// The file is updated without ever being read_file'd.
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := NewFileTracker()
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, tracker, config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Update File: a.go\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	if _, err := runApplyPatch(t, tool, map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if got := readFile(t, path); got != "alpha\nAFTER\nbeta" {
		t.Fatalf("file = %q, want %q (no read-before-edit gate)", got, "alpha\nAFTER\nbeta")
	}
}

func TestApplyPatchResultSuccessFormat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\nB\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Add File: b.txt\n+x\n*** Update File: a.txt\n@@\n a\n-B\n+B2\n c\n*** Delete File: c.txt")
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
}

func TestApplyPatchMidWriteFailureReturnsExitError(t *testing.T) {
	// Simulate a mid-write failure: the first op's snapshot succeeds
	// (the Add lands), the second op's snapshot returns an error
	// (simulated disk full / IO error after the first mutation). The
	// result must be a *ExitError carrying the A summary and the I/O
	// error message.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{
		turn:       1,
		failOnCall: 2, // the second snapshot call fails
		errFail:    errors.New("simulated disk full"),
	}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Add File: new.txt\n+hi\n*** Update File: x.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	_, err := runApplyPatch(t, tool, map[string]any{"input": input})
	if err == nil {
		t.Fatal("Execute err = nil, want non-nil (mid-write failure)")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute err = %T %v, want *ExitError", err, err)
	}
	if !strings.Contains(exitErr.Output, "A new.txt") {
		t.Fatalf("ExitError.Output = %q, want it to include the A new.txt committed summary", exitErr.Output)
	}
	if !strings.Contains(exitErr.Output, "simulated disk full") {
		t.Fatalf("ExitError.Output = %q, want it to include the snapshot error", exitErr.Output)
	}
}

func TestApplyPatchSnapshotsEveryTouchedFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.go")
	updated := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(src, []byte("a\nB\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(updated, []byte("a\nB\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 7}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Update File: x.txt\n@@\n a\n-B\n+B2\n c\n*** Update File: old.go\n*** Move to: new.go\n@@\n a\n-B\n+B2\n c")
	if _, err := runApplyPatch(t, tool, map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	// Expected snapshots: updated (Update), new.go (Move dest, Existed:false),
	// old.go (Move source, Existed:true). That's 3 snapshot calls under turn=7.
	if len(store.calls) != 3 {
		t.Fatalf("snapshot calls = %d, want 3", len(store.calls))
	}
	for _, c := range store.calls {
		if c.turn != 7 {
			t.Fatalf("snapshot call turn = %d, want 7", c.turn)
		}
	}
}

func TestApplyPatchDefaultHidden(t *testing.T) {
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, t.TempDir())
	if !tool.DefaultHidden() {
		t.Fatal("DefaultHidden = false, want true")
	}
}

func TestApplyPatchCapturesPreviewForDisplayMetadata(t *testing.T) {
	// Verifies the engine captures per-op pre/post/hunks data during
	// apply so DisplayMetadata (commit 6) can build edit_preview_files
	// without post-write disk reads. Read-after-apply is not viable
	// for updates (source is overwritten), deletes (file is gone), or
	// moves (source is unlinked).
	dir := t.TempDir()
	src := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(src, []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Add File: new.txt\n+hi\n*** Update File: x.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta\n*** Delete File: a.txt")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("byebye"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runApplyPatch(t, tool, map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}

	previews := tool.takeApplyPreview()
	if len(previews) != 3 {
		t.Fatalf("previews = %d, want 3 (Add, Update, Delete)", len(previews))
	}
	// Add: post is "hi", no pre, no hunks.
	if previews[0].Op != "A" || previews[0].Path != "new.txt" {
		t.Fatalf("preview[0] = %+v, want A new.txt", previews[0])
	}
	if len(previews[0].Pre) != 0 {
		t.Fatalf("preview[0].Pre = %v, want empty", previews[0].Pre)
	}
	if len(previews[0].Post) != 1 || previews[0].Post[0] != "hi" {
		t.Fatalf("preview[0].Post = %v, want [hi]", previews[0].Post)
	}
	// Update: pre is the file's original lines, post is the new, one hunk.
	// The pattern is [alpha, BEFORE]; it matches at pre[0]="alpha", so
	// StartLine (1-based) is 1.
	if previews[1].Op != "M" || previews[1].Path != "x.txt" {
		t.Fatalf("preview[1] = %+v, want M x.txt", previews[1])
	}
	if len(previews[1].Pre) != 3 || previews[1].Pre[1] != "BEFORE" {
		t.Fatalf("preview[1].Pre = %v, want [alpha BEFORE beta]", previews[1].Pre)
	}
	if len(previews[1].Hunks) != 1 {
		t.Fatalf("preview[1].Hunks = %d, want 1", len(previews[1].Hunks))
	}
	if previews[1].Hunks[0].StartLine != 1 {
		t.Fatalf("hunk StartLine = %d, want 1 (1-based; pre[0]=alpha, the context line)", previews[1].Hunks[0].StartLine)
	}
	// Delete: pre is the file's content, no post, no hunks.
	if previews[2].Op != "D" || previews[2].Path != "a.txt" {
		t.Fatalf("preview[2] = %+v, want D a.txt", previews[2])
	}
	if len(previews[2].Pre) != 1 || previews[2].Pre[0] != "byebye" {
		t.Fatalf("preview[2].Pre = %v, want [byebye]", previews[2].Pre)
	}
}

func TestApplyPatchCapturesPreviewForMove(t *testing.T) {
	// Move produces two preview entries: M <newpath> with the new
	// content and the applied hunks; D <oldpath> with the original
	// content and no hunks.
	dir := t.TempDir()
	src := filepath.Join(dir, "old.go")
	if err := os.WriteFile(src, []byte("a\nB\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	input := applyPatchInput(t, "*** Update File: old.go\n*** Move to: new.go\n@@\n a\n-B\n+B2\n c")
	if _, err := runApplyPatch(t, tool, map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	previews := tool.takeApplyPreview()
	if len(previews) != 2 {
		t.Fatalf("previews = %d, want 2 (M new.go, D old.go)", len(previews))
	}
	if previews[0].Op != "M" || previews[0].Path != "new.go" {
		t.Fatalf("preview[0] = %+v, want M new.go", previews[0])
	}
	if previews[0].Post[1] != "B2" {
		t.Fatalf("preview[0].Post = %v, want second line B2", previews[0].Post)
	}
	if previews[1].Op != "D" || previews[1].Path != "old.go" {
		t.Fatalf("preview[1] = %+v, want D old.go", previews[1])
	}
	if previews[1].Pre[1] != "B" {
		t.Fatalf("preview[1].Pre = %v, want second line B (pre-mutation source)", previews[1].Pre)
	}
}

func TestApplyPatchPreviewClearedOnError(t *testing.T) {
	// A failed apply must clear any previous preview so a later
	// successful apply cannot read a stale one.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, dir)

	// First call: succeeds.
	input1 := applyPatchInput(t, "*** Add File: first.txt\n+hi")
	if _, err := runApplyPatch(t, tool, map[string]any{"input": input1}); err != nil {
		t.Fatalf("first Execute err = %v", err)
	}
	if p := tool.takeApplyPreview(); len(p) != 1 {
		t.Fatalf("first preview len = %d, want 1", len(p))
	}

	// Second call: fails (delete of non-existent file).
	input2 := applyPatchInput(t, "*** Delete File: ghost.txt")
	if _, err := runApplyPatch(t, tool, map[string]any{"input": input2}); err == nil {
		t.Fatal("second Execute err = nil, want non-nil")
	}
	if p := tool.takeApplyPreview(); len(p) != 0 {
		t.Fatalf("preview after failed call len = %d, want 0 (cleared)", len(p))
	}
}

func TestApplyPatchRegisteredAsDefaultHiddenExcludedFromBaseline(t *testing.T) {
	// Build a registry that contains the same core tools as the main
	// agent (read_file / write_file / edit_file / apply_patch) plus
	// execute_pending, then verify apply_patch is hidden from the
	// baseline advertisement and revealed only by an IncludeTools
	// adaptation. This is the master-invariant guard.
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	core := CoreTools(store, tracker, config.ToolsConfig{}, dir, nil, nil)
	registry := NewRegistry()
	for _, t := range core {
		registry.Register(t)
	}
	registry.Register(ExecutePending{})

	// Baseline: apply_patch must be absent.
	baselineNames := openAIToolNames(registry, nil)
	if contains(baselineNames, "apply_patch") {
		t.Fatalf("apply_patch leaked into baseline: %v", baselineNames)
	}
	// Revealed only when IncludeTools names it.
	reveal := &adaptation.Adaptation{IncludeTools: []string{"apply_patch"}}
	revealNames := openAIToolNames(registry, reveal)
	if !contains(revealNames, "apply_patch") {
		t.Fatalf("apply_patch absent under IncludeTools adaptation: %v", revealNames)
	}
	// And edit_file / write_file are absent when ExcludeTools names them.
	exclude := &adaptation.Adaptation{IncludeTools: []string{"apply_patch"}, ExcludeTools: []string{"edit_file", "write_file"}}
	exclNames := openAIToolNames(registry, exclude)
	if contains(exclNames, "edit_file") || contains(exclNames, "write_file") {
		t.Fatalf("edit_file/write_file leaked under ExcludeTools: %v", exclNames)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func openAIToolNames(r *Registry, adapt *adaptation.Adaptation) []string {
	var names []string
	for _, tl := range r.AdvertisedTools(adapt) {
		if tl.Function != nil {
			names = append(names, tl.Function.Name)
		}
	}
	return names
}

func contains(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}
