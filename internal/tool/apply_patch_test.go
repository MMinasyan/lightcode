package tool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/editpreview"
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
	turn            int
	calls           []snapshotCall
	seen            map[string]string
	identityRecords []snapshotIdentityRecord
	onSnapshot      func(call int)
	// failOnCall, when > 0, makes the Nth snapshot call return errFail
	// (used to simulate a mid-write failure at the second op).
	failOnCall int
	errFail    error
	// discards records every DiscardSnapshotEntry call (entry IDs) so tests
	// can observe the discard-before-mutation snapshot behavior.
	discards []string
}

func (s *applyPatchStore) Snapshot(turn int, absPath string) error {
	return s.SnapshotResolved(turn, absPath, absPath)
}

func (s *applyPatchStore) SnapshotResolved(turn int, originalPath, canonicalPath string) error {
	s.calls = append(s.calls, snapshotCall{turn: turn, path: originalPath, canonical: canonicalPath})
	if s.onSnapshot != nil {
		s.onSnapshot(len(s.calls))
	}
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

func (s *applyPatchStore) SnapshotResolvedEntry(turn int, originalPath, canonicalPath string) (string, bool, error) {
	if err := s.SnapshotResolved(turn, originalPath, canonicalPath); err != nil {
		return "", false, err
	}
	return canonicalPath, true, nil
}

func (s *applyPatchStore) DiscardSnapshotEntry(_ int, entryID string) error {
	s.discards = append(s.discards, entryID)
	return nil
}

func (s *applyPatchStore) RetainSnapshotEntry(int, string) {}

func (s *applyPatchStore) RecordSnapshotContent(turn int, entryID string, content []byte) error {
	s.identityRecords = append(s.identityRecords, snapshotIdentityRecord{
		turn:    turn,
		entryID: entryID,
		content: string(content),
	})
	return nil
}

func (s *applyPatchStore) RecordSnapshotAbsence(turn int, entryID string) error {
	s.identityRecords = append(s.identityRecords, snapshotIdentityRecord{
		turn:    turn,
		entryID: entryID,
		absent:  true,
	})
	return nil
}

func (s *applyPatchStore) CurrentTurn() int { return s.turn }

func TestApplyPatchAllOrNothingValidationFailure(t *testing.T) {
	// First op is a successful Add; second op is an Update of a
	// non-existent file. Validation runs the existence check before any
	// write, so the Add must not land (validate-first all-or-nothing).
	dir := t.TempDir()
	newPath := filepath.Join(dir, "new.txt")
	store := &applyPatchStore{turn: 1}

	input := applyPatchInput(t, "*** Add File: new.txt\n+hello\n*** Update File: ghost.go\n@@\n-missing\n+replacement")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
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

	input := applyPatchInput(t, "*** Add File: new.txt\n+hi\n*** Delete File: ghost.txt")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
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

	input := applyPatchInput(t, "*** Add File: new.txt\n+hi\n*** Update File: x.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
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

func TestApplyPatchRefusesUpdateWhenContentChangesAfterValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(path, []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1, onSnapshot: func(call int) {
		if call == 1 {
			if err := os.WriteFile(path, []byte("alpha\nCHANGED\nbeta"), 0o644); err != nil {
				t.Fatalf("mutate target: %v", err)
			}
		}
	}}

	input := applyPatchInput(t, "*** Add File: first.txt\n+hi\n*** Update File: target.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	if err == nil || !strings.Contains(err.Error(), "changed after validation") {
		t.Fatalf("Execute err = %v, want changed-after-validation", err)
	}
	if got := readFile(t, path); got != "alpha\nCHANGED\nbeta" {
		t.Fatalf("target = %q, want concurrent content retained", got)
	}
}

func TestApplyPatchRefusesDeleteWhenContentChangesAfterValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(path, []byte("delete me"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1, onSnapshot: func(call int) {
		if call == 1 {
			if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
				t.Fatalf("mutate target: %v", err)
			}
		}
	}}

	input := applyPatchInput(t, "*** Add File: first.txt\n+hi\n*** Delete File: target.txt")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	if err == nil || !strings.Contains(err.Error(), "changed after validation") {
		t.Fatalf("Execute err = %v, want changed-after-validation", err)
	}
	if got := readFile(t, path); got != "changed" {
		t.Fatalf("target = %q, want concurrent content retained", got)
	}
}

func TestApplyPatchRefusesMoveSourceWhenContentChangesAfterDestinationWrite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "old.txt")
	dst := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(src, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1, onSnapshot: func(call int) {
		if call == 2 {
			if err := os.WriteFile(src, []byte("changed"), 0o644); err != nil {
				t.Fatalf("mutate source: %v", err)
			}
		}
	}}

	input := applyPatchInput(t, "*** Add File: first.txt\n+hi\n*** Update File: old.txt\n*** Move to: new.txt")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	if err == nil || !strings.Contains(err.Error(), "changed after validation") {
		t.Fatalf("Execute err = %v, want changed-after-validation", err)
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(exitErr.Output, "M new.txt") {
		t.Fatalf("Execute err = %v, want partial summary with M new.txt", err)
	}
	if got := readFile(t, src); got != "changed" {
		t.Fatalf("source = %q, want concurrent content retained", got)
	}
	if got := readFile(t, dst); got != "old content" {
		t.Fatalf("dest = %q, want committed pre-change content", got)
	}
}

func TestApplyPatchRecordsSnapshotIdentityForAddUpdateAndDelete(t *testing.T) {
	dir := t.TempDir()
	updatePath := filepath.Join(dir, "update.txt")
	deletePath := filepath.Join(dir, "delete.txt")
	addPath := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(updatePath, []byte("a\nB\nc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deletePath, []byte("remove me"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 4}

	input := applyPatchInput(t, "*** Add File: new.txt\n+created\n*** Update File: update.txt\n@@\n a\n-B\n+B2\n c\n*** Delete File: delete.txt")
	if _, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}

	assertSnapshotIdentityContent(t, store.identityRecords, 4, addPath, "created")
	assertSnapshotIdentityContent(t, store.identityRecords, 4, updatePath, "a\nB2\nc")
	assertSnapshotIdentityAbsent(t, store.identityRecords, 4, deletePath)
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

	input := applyPatchInput(t, "*** Update File: x.txt\n@@\n a\n-B\n+B2\n c\n*** Update File: old.go\n*** Move to: new.go\n@@\n a\n-B\n+B2\n c")
	if _, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input}); err != nil {
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

func TestApplyPatchAddFailsIfFileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}

	input := applyPatchInput(t, "*** Add File: exists.txt\n+new")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	if err == nil {
		t.Fatal("Execute err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want it to mention already-exists", err)
	}
	if got := readFile(t, path); got != "old" {
		t.Fatalf("file = %q, want unchanged %q", got, "old")
	}
	if len(store.calls) != 0 {
		t.Fatalf("snapshot calls = %d, want 0 (refused before any snapshot capture)", len(store.calls))
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

	input := applyPatchInput(t, "*** Update File: old.go\n*** Move to: new.go\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
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
	if len(store.calls) != 0 {
		t.Fatalf("snapshot calls = %d, want 0 (refused before any snapshot capture)", len(store.calls))
	}
}

func TestApplyPatchCanonicalCollisionRejectedBeforeApply(t *testing.T) {
	// Two different raw paths that resolve to the same canonical path
	// must be rejected before any mutation. The parser allows them
	// (raw-path dedup catches only exact duplicates); target planning
	// catches the canonical collision.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	// "a.txt" and "./a.txt" are different raw paths but resolve to the same canonical.
	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n-hi\n+bye\n*** Update File: ./a.txt\n@@\n-hi\n+there")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	if err == nil || !strings.Contains(err.Error(), "resolve to the same file") {
		t.Fatalf("Execute err = %v, want canonical collision error", err)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "hi" {
		t.Fatalf("a.txt = %q, want unchanged (collision rejected before apply)", got)
	}
	if len(store.calls) != 0 {
		t.Fatalf("snapshot calls = %d, want 0 (collision rejected before apply)", len(store.calls))
	}
}

func TestApplyPatchCanonicalCollisionSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(real, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	// Two raw paths resolving to the same canonical via symlink.
	input := applyPatchInput(t, "*** Update File: real.txt\n@@\n content\n+changed\n*** Delete File: link.txt")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	if err == nil || !strings.Contains(err.Error(), "resolve to the same file") {
		t.Fatalf("Execute err = %v, want canonical collision error", err)
	}
	if got := readFile(t, real); got != "content" {
		t.Fatalf("real.txt = %q, want unchanged (collision rejected before apply)", got)
	}
	if len(store.calls) != 0 {
		t.Fatalf("snapshot calls = %d, want 0 (collision rejected before apply)", len(store.calls))
	}
}

func TestApplyPatchFuzzyMatchPreservesActualTrailingWhitespaceContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("alpha   \nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := applyPatchInput(t, "*** Update File: a.go\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	if _, err := execPatch(t, dir, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if got := readFile(t, path); got != "alpha   \nAFTER\nbeta" {
		t.Fatalf("file = %q, want context trailing spaces preserved", got)
	}
}

func TestApplyPatchFuzzyMatchPreservesActualUnicodeContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("say \u201Chello\u201D\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := applyPatchInput(t, "*** Update File: a.go\n@@\n say \"hello\"\n-BEFORE\n+AFTER\n beta")
	if _, err := execPatch(t, dir, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if got := readFile(t, path); got != "say \u201Chello\u201D\nAFTER\nbeta" {
		t.Fatalf("file = %q, want unicode context preserved", got)
	}
}

func TestApplyPatchSyntheticEOFContextDoesNotCreateTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("foo\nbar"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n foo\n-bar\n+baz\n ")
	res, err := execPatch(t, dir, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{"input": input})
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if got := readFile(t, path); got != "foo\nbaz" {
		t.Fatalf("file = %q, want no synthetic trailing newline", got)
	}
	rows := previewRowsFor(t, ApplyPatchPreviewMetadata(res.Previews), "a.txt")
	if len(rows) != 3 {
		t.Fatalf("preview rows = %#v, want context/remove/add only", rows)
	}
	if rows[0].Kind != editpreview.KindContext || rows[0].Text != "foo" ||
		rows[1].Kind != editpreview.KindRemove || rows[1].Text != "bar" ||
		rows[2].Kind != editpreview.KindAdd || rows[2].Text != "baz" {
		t.Fatalf("preview rows = %#v, want synthetic EOF context omitted", rows)
	}
}

func TestApplyPatchSyntheticEOFRemoveFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("foo"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n-\n+bar")
	_, err := execPatch(t, dir, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{"input": input})
	if err == nil {
		t.Fatal("Execute err = nil, want synthetic EOF remove to fail")
	}
	if !strings.HasPrefix(err.Error(), "Failed to find expected lines in a.txt:") {
		t.Fatalf("err = %v, want expected-lines failure", err)
	}
	if got := readFile(t, path); got != "foo" {
		t.Fatalf("file = %q, want unchanged", got)
	}
}

func TestApplyPatchRealEOFContextPreservesTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("foo\nbar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n foo\n-bar\n+baz\n ")
	res, err := execPatch(t, dir, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{"input": input})
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if got := readFile(t, path); got != "foo\nbaz\n" {
		t.Fatalf("file = %q, want real trailing newline preserved", got)
	}
	rows := previewRowsFor(t, ApplyPatchPreviewMetadata(res.Previews), "a.txt")
	if len(rows) != 4 || rows[3].Kind != editpreview.KindContext || rows[3].Text != "" {
		t.Fatalf("preview rows = %#v, want real EOF context retained", rows)
	}
}

func TestApplyPatchRealEmptyRemoveStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("foo\n\nbaz"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n foo\n-\n+bar\n baz")
	if _, err := execPatch(t, dir, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if got := readFile(t, path); got != "foo\nbar\nbaz" {
		t.Fatalf("file = %q, want real empty line replaced", got)
	}
}

// previewRowsFor returns the captured preview rows for one path from the
// edit_preview_files metadata map, which must hold exactly one entry.
func previewRowsFor(t *testing.T, md map[string]any, path string) []editpreview.Row {
	t.Helper()
	files, ok := editpreview.FilesFromMetadata(md)
	if !ok {
		t.Fatalf("edit_preview_files not present: %+v", md)
	}
	if len(files) != 1 {
		t.Fatalf("preview files = %+v, want exactly one entry", files)
	}
	for _, f := range files {
		if f.Path == path {
			if len(f.Preview.Hunks) != 1 {
				t.Fatalf("preview for %s = %+v, want one hunk", path, f.Preview)
			}
			return f.Preview.Hunks[0].Rows
		}
	}
	t.Fatalf("no preview entry for %s in %+v", path, files)
	return nil
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
