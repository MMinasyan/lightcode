package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// execWrite runs one write through the exported Normalize* + Prepare* entries.
// An empty rootCanonical is bound from root inside the helper.
func execWrite(t *testing.T, root, rootCanonical, writeDirCanonical string, opts CapabilityOptions, store SnapshotStore, args map[string]any) (string, error) {
	t.Helper()
	if rootCanonical == "" {
		var err error
		rootCanonical, err = bindWorkspaceRoot(root)
		if err != nil {
			return "", err
		}
	}
	norm, err := NormalizeWriteArgs(args)
	if err != nil {
		return "", err
	}
	call, err := PrepareWriteCall(root, rootCanonical, writeDirCanonical, opts, store, norm)
	if err != nil {
		return "", err
	}
	return call.Execute(context.Background())
}

func TestPreparedWriteCreatesNewFileAndParentDirs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "new.txt")

	result, err := execWrite(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
		"path":    path,
		"content": "created",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Wrote "+path+"." {
		t.Fatalf("Execute result = %q, want original path in success message", result)
	}
	assertFileContent(t, path, "created")
}

func TestPreparedWriteWritesContentExactlyIncludingTrailingNewline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "exact.txt")

	if _, err := execWrite(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
		"path":    path,
		"content": "no trailing newline",
	}); err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	assertFileContent(t, path, "no trailing newline")

	if _, err := execWrite(t, root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, map[string]any{
		"path":    path,
		"content": "with trailing newline\n",
	}); err != nil {
		t.Fatalf("second Execute error = %v", err)
	}
	assertFileContent(t, path, "with trailing newline\n")
}

func TestPreparedWriteWithSnapshotSnapshotsBeforeWrite(t *testing.T) {
	path := readFileTestFile(t, "file.txt", "before")
	root := filepath.Dir(path)
	store := &recordingSnapshotStore{turn: 9, before: map[string]string{}}

	result, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
		"path":    path,
		"content": "after",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Wrote "+path+"." {
		t.Fatalf("Execute result = %q, want original path in success message", result)
	}
	if len(store.calls) != 1 {
		t.Fatalf("snapshot calls = %d, want 1", len(store.calls))
	}
	if store.calls[0].turn != 9 || store.calls[0].path != path {
		t.Fatalf("snapshot call = %+v, want turn=9 path=%q", store.calls[0], path)
	}
	if store.before[path] != "before" {
		t.Fatalf("snapshot saw %q, want pre-write content", store.before[path])
	}
	assertFileContent(t, path, "after")
}

func TestPreparedWriteWithSnapshotUsesCanonicalTargetAndRequestedResult(t *testing.T) {
	root := t.TempDir()
	realDir := t.TempDir()
	target := filepath.Join(realDir, "file.txt")
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	requested := filepath.Join(linkDir, "file.txt")
	store := &recordingSnapshotStore{turn: 10, before: map[string]string{}}

	result, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{
		"path":    requested,
		"content": "after",
	})

	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Wrote "+requested+"." {
		t.Fatalf("Execute result = %q, want requested path in success message", result)
	}
	if len(store.calls) != 1 {
		t.Fatalf("snapshot calls = %d, want 1", len(store.calls))
	}
	if store.calls[0].path != requested || store.calls[0].canonical != target {
		t.Fatalf("snapshot call = %+v, want original=%q canonical=%q", store.calls[0], requested, target)
	}
	if store.before[target] != "before" {
		t.Fatalf("snapshot saw %q, want pre-write content", store.before[target])
	}
	assertFileContent(t, target, "after")
}
