package tool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/editpreview"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

func applyPatchInput(t *testing.T, s string) string {
	t.Helper()
	return "*** Begin Patch\n" + s + "\n*** End Patch"
}

func TestApplyPatchAllOrNothingValidationFailure(t *testing.T) {
	// First op is a successful Add; second op is an Update of a
	// non-existent file. Validation runs the existence check before any
	// write, so the Add must not land (validate-first all-or-nothing).
	dir := t.TempDir()
	newPath := filepath.Join(dir, "new.txt")
	store := newTempCodeGroupStore(t)

	input := applyPatchInput(t, "*** Add File: new.txt\n+hello\n*** Update File: ghost.go\n@@\n-missing\n+replacement")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil, "Execute err = nil, want non-nil")
	testassert.Require(t, strings.Contains(err.Error(), "does not exist"), "err = %v, want does-not-exist error", err)
	{
		_, statErr := os.Stat(newPath)
		testassert.Require(t, os.IsNotExist(statErr), "Add landed despite later op's validation failure: stat err = %v", statErr)
	}
}

func TestApplyPatchAllOrNothingPartialApplyError(t *testing.T) {
	// First op is a successful Add; second op is a Delete of a file that
	// doesn't exist. Validation is all-or-nothing: the existence check on
	// the Delete fails, so the Add is not written either.
	dir := t.TempDir()
	newPath := filepath.Join(dir, "new.txt")
	store := newTempCodeGroupStore(t)

	input := applyPatchInput(t, "*** Add File: new.txt\n+hi\n*** Delete File: ghost.txt")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil, "Execute err = nil, want non-nil")
	testassert.Require(t, strings.Contains(err.Error(), "does not exist"), "err = %v, want does-not-exist error", err)
	{
		_, statErr := os.Stat(newPath)
		testassert.Require(t, os.IsNotExist(statErr), "Add landed despite later op's validation failure: stat err = %v", statErr)
	}
}

func TestApplyPatchMidWriteFailureReturnsExitError(t *testing.T) {
	// Simulate a mid-write failure: the first op's snapshot succeeds
	// (the Add lands), the second op's snapshot returns an error
	// (simulated disk full / IO error after the first mutation). The
	// result must be a *ExitError carrying the A summary and the I/O
	// error message.
	dir := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "x.txt"), []byte("alpha\nBEFORE\nbeta"), 0o644))
	store := &recordingCodeGroupStore{
		testCodeGroupStore: newTempCodeGroupStore(t),
		failOnCall:         2, // the second snapshot call fails
		errFail:            errors.New("simulated disk full"),
	}

	input := applyPatchInput(t, "*** Add File: new.txt\n+hi\n*** Update File: x.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil, "Execute err = nil, want non-nil (mid-write failure)")
	var exitErr *ExitError
	testassert.Require(t, errors.As(err, &exitErr), "Execute err = %T %v, want *ExitError", err, err)
	testassert.Require(t, strings.Contains(exitErr.Output, "A new.txt") && strings.Contains(exitErr.Output, "simulated disk full"), "ExitError.Output = %q, want it to include the A new.txt committed summary; ExitError.Output = %q, want it to include the snapshot error", exitErr.Output, exitErr.Output)
}

func TestApplyPatchRefusesUpdateWhenContentChangesAfterValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("alpha\nBEFORE\nbeta"), 0o644))
	store := &recordingCodeGroupStore{testCodeGroupStore: newTempCodeGroupStore(t), onSnapshot: func(call int) {
		if call == 1 {
			testassert.NoError(t, os.WriteFile(path, []byte("alpha\nCHANGED\nbeta"), 0o644))
		}
	}}

	input := applyPatchInput(t, "*** Add File: first.txt\n+hi\n*** Update File: target.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "changed after validation"), "Execute err = %v, want changed-after-validation", err)
	testassert.Equal(t, readFile(t, path), "alpha\nCHANGED\nbeta")
}

func TestApplyPatchRefusesDeleteWhenContentChangesAfterValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("delete me"), 0o644))
	store := &recordingCodeGroupStore{testCodeGroupStore: newTempCodeGroupStore(t), onSnapshot: func(call int) {
		if call == 1 {
			testassert.NoError(t, os.WriteFile(path, []byte("changed"), 0o644))
		}
	}}

	input := applyPatchInput(t, "*** Add File: first.txt\n+hi\n*** Delete File: target.txt")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "changed after validation"), "Execute err = %v, want changed-after-validation", err)
	testassert.Equal(t, readFile(t, path), "changed")
}

func TestApplyPatchRefusesMoveSourceWhenContentChangesAfterDestinationWrite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.txt")
	dst := filepath.Join(dir, "new.txt")
	testassert.NoError(t, os.WriteFile(src, []byte("old content"), 0o644))
	store := &recordingCodeGroupStore{testCodeGroupStore: newTempCodeGroupStore(t), onSnapshot: func(call int) {
		if call == 2 {
			testassert.NoError(t, os.WriteFile(src, []byte("changed"), 0o644))
		}
	}}

	input := applyPatchInput(t, "*** Add File: first.txt\n+hi\n*** Update File: old.txt\n*** Move to: new.txt")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "changed after validation"), "Execute err = %v, want changed-after-validation", err)
	var exitErr *ExitError
	testassert.Require(t, errors.As(err, &exitErr) && strings.Contains(exitErr.Output, "M new.txt"), "Execute err = %v, want partial summary with M new.txt", err)
	testassert.Equal(t, readFile(t, src), "changed")
	testassert.Equal(t, readFile(t, dst), "old content")
}

func TestApplyPatchRecordsSnapshotIdentityForAddUpdateAndDelete(t *testing.T) {
	dir := t.TempDir()
	updatePath := filepath.Join(dir, "update.txt")
	deletePath := filepath.Join(dir, "delete.txt")
	addPath := filepath.Join(dir, "new.txt")
	testassert.NoError(t, os.WriteFile(updatePath, []byte("a\nB\nc"), 0o644))
	testassert.NoError(t, os.WriteFile(deletePath, []byte("remove me"), 0o644))
	store := &recordingCodeGroupStore{testCodeGroupStore: newCodeGroupStore(t, filepath.Join(t.TempDir(), "group"), 4)}

	input := applyPatchInput(t, "*** Add File: new.txt\n+created\n*** Update File: update.txt\n@@\n a\n-B\n+B2\n c\n*** Delete File: delete.txt")
	{
		_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
		testassert.NoError(t, err)
	}

	assertSnapshotIdentityContent(t, store.identityRecords, 4, store.entryIDFor(addPath), "created")
	assertSnapshotIdentityContent(t, store.identityRecords, 4, store.entryIDFor(updatePath), "a\nB2\nc")
	assertSnapshotIdentityAbsent(t, store.identityRecords, 4, store.entryIDFor(deletePath))
}

func TestApplyPatchSnapshotsEveryTouchedFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.go")
	updated := filepath.Join(dir, "x.txt")
	testassert.NoError(t, os.WriteFile(src, []byte("a\nB\nc"), 0o644))
	testassert.NoError(t, os.WriteFile(updated, []byte("a\nB\nc"), 0o644))
	store := &recordingCodeGroupStore{testCodeGroupStore: newCodeGroupStore(t, filepath.Join(t.TempDir(), "group"), 7)}

	input := applyPatchInput(t, "*** Update File: x.txt\n@@\n a\n-B\n+B2\n c\n*** Update File: old.go\n*** Move to: new.go\n@@\n a\n-B\n+B2\n c")
	{
		_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
		testassert.NoError(t, err)
	}
	// Expected snapshots: updated (Update), new.go (Move dest, Existed:false),
	// old.go (Move source, Existed:true). That's 3 snapshot calls under turn=7.
	testassert.Equal(t, len(store.calls), 3)
	for _, c := range store.calls {
		testassert.Equal(t, c.turn, 7)
	}
}

func TestApplyPatchAddFailsIfFileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	store := newRecordingCodeGroupStore(t)

	input := applyPatchInput(t, "*** Add File: exists.txt\n+new")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil, "Execute err = nil, want non-nil")
	testassert.Require(t, strings.Contains(err.Error(), "already exists"), "err = %v, want it to mention already-exists", err)
	testassert.Equal(t, readFile(t, path), "old")
	testassert.Equal(t, len(store.calls), 0)
}

func TestApplyPatchMoveFailsIfDestinationExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.go")
	dst := filepath.Join(dir, "new.go")
	testassert.NoError(t, os.WriteFile(src, []byte("alpha\nBEFORE\nbeta"), 0o644))
	testassert.NoError(t, os.WriteFile(dst, []byte("preexisting"), 0o644))
	store := newRecordingCodeGroupStore(t)

	input := applyPatchInput(t, "*** Update File: old.go\n*** Move to: new.go\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil, "Execute err = nil, want non-nil (move destination already exists)")
	testassert.Require(t, strings.Contains(err.Error(), "already exists"), "err = %v, want already-exists error", err)
	testassert.Equal(t, readFile(t, src), "alpha\nBEFORE\nbeta")
	testassert.Equal(t, readFile(t, dst), "preexisting")
	testassert.Equal(t, len(store.calls), 0)
}

func TestApplyPatchCanonicalCollisionRejectedBeforeApply(t *testing.T) {
	// Two different raw paths that resolve to the same canonical path
	// must be rejected before any mutation. The parser allows them
	// (raw-path dedup catches only exact duplicates); target planning
	// catches the canonical collision.
	dir := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644))
	store := newRecordingCodeGroupStore(t)
	// "a.txt" and "./a.txt" are different raw paths but resolve to the same canonical.
	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n-hi\n+bye\n*** Update File: ./a.txt\n@@\n-hi\n+there")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "resolve to the same file"), "Execute err = %v, want canonical collision error", err)
	testassert.Equal(t, readFile(t, filepath.Join(dir, "a.txt")), "hi")
	testassert.Equal(t, len(store.calls), 0)
}

func TestApplyPatchCanonicalCollisionSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	testassert.NoError(t, os.WriteFile(real, []byte("content"), 0o644))
	testassert.NoError(t, os.Symlink(real, link))
	store := newRecordingCodeGroupStore(t)
	// Two raw paths resolving to the same canonical via symlink.
	input := applyPatchInput(t, "*** Update File: real.txt\n@@\n content\n+changed\n*** Delete File: link.txt")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "resolve to the same file"), "Execute err = %v, want canonical collision error", err)
	testassert.Equal(t, readFile(t, real), "content")
	testassert.Equal(t, len(store.calls), 0)
}

func TestApplyPatchFuzzyMatchPreservesActualTrailingWhitespaceContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	testassert.NoError(t, os.WriteFile(path, []byte("alpha   \nBEFORE\nbeta"), 0o644))

	input := applyPatchInput(t, "*** Update File: a.go\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	{
		_, err := execPatch(t, dir, "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{"input": input})
		testassert.NoError(t, err)
	}
	testassert.Equal(t, readFile(t, path), "alpha   \nAFTER\nbeta")
}

func TestApplyPatchFuzzyMatchPreservesActualUnicodeContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	testassert.NoError(t, os.WriteFile(path, []byte("say \u201Chello\u201D\nBEFORE\nbeta"), 0o644))

	input := applyPatchInput(t, "*** Update File: a.go\n@@\n say \"hello\"\n-BEFORE\n+AFTER\n beta")
	{
		_, err := execPatch(t, dir, "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{"input": input})
		testassert.NoError(t, err)
	}
	testassert.Equal(t, readFile(t, path), "say \u201Chello\u201D\nAFTER\nbeta")
}

func TestApplyPatchSyntheticEOFContextDoesNotCreateTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("foo\nbar"), 0o644))

	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n foo\n-bar\n+baz\n ")
	res, err := execPatch(t, dir, "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{"input": input})
	testassert.NoError(t, err)
	testassert.Equal(t, readFile(t, path), "foo\nbaz")
	rows := previewRowsFor(t, ApplyPatchPreviewMetadata(res.Previews), "a.txt")
	testassert.Require(t, len(rows) == 3 && rows[0].Kind == editpreview.KindContext && rows[0].Text == "foo" && rows[1].Kind == editpreview.KindRemove && rows[1].Text == "bar" && rows[2].Kind == editpreview.KindAdd && rows[2].Text == "baz", "preview rows = %#v, want context/remove/add only; preview rows = %#v, want synthetic EOF context omitted", rows, rows)
}

func TestApplyPatchSyntheticEOFRemoveFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("foo"), 0o644))

	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n-\n+bar")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{"input": input})
	testassert.Require(t, err != nil, "Execute err = nil, want synthetic EOF remove to fail")
	testassert.Require(t, strings.HasPrefix(err.Error(), "Failed to find expected lines in a.txt:"), "err = %v, want expected-lines failure", err)
	testassert.Equal(t, readFile(t, path), "foo")
}

func TestApplyPatchRealEOFContextPreservesTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("foo\nbar\n"), 0o644))

	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n foo\n-bar\n+baz\n ")
	res, err := execPatch(t, dir, "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{"input": input})
	testassert.NoError(t, err)
	testassert.Equal(t, readFile(t, path), "foo\nbaz\n")
	rows := previewRowsFor(t, ApplyPatchPreviewMetadata(res.Previews), "a.txt")
	testassert.Require(t, len(rows) == 4 && rows[3].Kind == editpreview.KindContext && rows[3].Text == "", "preview rows = %#v, want real EOF context retained", rows)
}

func TestApplyPatchRealEmptyRemoveStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	testassert.NoError(t, os.WriteFile(path, []byte("foo\n\nbaz"), 0o644))

	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n foo\n-\n+bar\n baz")
	{
		_, err := execPatch(t, dir, "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{"input": input})
		testassert.NoError(t, err)
	}
	testassert.Equal(t, readFile(t, path), "foo\nbar\nbaz")
}

// previewRowsFor returns the captured preview rows for one path from the
// edit_preview_files metadata map, which must hold exactly one entry.
func previewRowsFor(t *testing.T, md map[string]any, path string) []editpreview.Row {
	t.Helper()
	files, ok := editpreview.FilesFromMetadata(md)
	testassert.Require(t, ok, "edit_preview_files not present: %+v", md)
	testassert.Require(t, len(files) == 1, "preview files = %+v, want exactly one entry", files)
	for _, f := range files {
		if f.Path == path {
			testassert.Require(t, len(f.Preview.Hunks) == 1, "preview for %s = %+v, want one hunk", path, f.Preview)
			return f.Preview.Hunks[0].Rows
		}
	}
	t.Fatalf("no preview entry for %s in %+v", path, files)
	return nil
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	testassert.Require(t, err == nil, "read %s: %v", path, err)
	return string(data)
}
