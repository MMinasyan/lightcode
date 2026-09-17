package snapshot

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// The ListChangedFiles contract tests: the durable identity shape on the
// session id and every group name, the absent-directory and enumeration-error
// rows, the skip rules, the empty-CanonicalPath rule, and the dedupe/sort
// result.

const listTestSession = "0123456789abcdef0123456789abcdef"

// seedListGroup writes one code group's snapshot metadata the way a real
// group records it: <group>/snapshots/<turn>/<entry>/meta.json.
func seedListGroup(t *testing.T, dataDir, sessionID, groupID string, turns map[int][]SnapshotMeta) {
	t.Helper()
	for turn, files := range turns {
		for i, meta := range files {
			entry := filepath.Join(dataDir, "code", sessionID, groupID, "snapshots", strconv.Itoa(turn), "entry"+strconv.Itoa(i))
			if err := os.MkdirAll(entry, 0o755); err != nil {
				t.Fatal(err)
			}
			writeMeta(t, filepath.Join(entry, "meta.json"), meta)
		}
	}
}

func writeMeta(t *testing.T, path string, meta SnapshotMeta) {
	t.Helper()
	data := "{\"original_path\":\"" + meta.OriginalPath + "\",\"canonical_path\":\"" + meta.CanonicalPath + "\",\"existed\":true}"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListChangedFilesAbsentSessionIsEmptySuccess(t *testing.T) {
	paths, err := ListChangedFiles(t.TempDir(), listTestSession)
	if err != nil {
		t.Fatalf("ListChangedFiles absent session: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("paths = %q, want empty", paths)
	}
}

func TestListChangedFilesSessionIDShape(t *testing.T) {
	for _, bad := range []string{"", "short", "0123456789abcdef0123456789abcde", "0123456789ABCDEF0123456789ABCDEF", "0123456789abcdef0123456789abcdeg"} {
		if _, err := ListChangedFiles(t.TempDir(), bad); err == nil {
			t.Errorf("ListChangedFiles(%q) = nil error, want the identity-shape rejection", bad)
		}
	}
}

func TestListChangedFilesEnumerationError(t *testing.T) {
	dataDir := t.TempDir()
	// The session path is a file: reading it as a directory is an error other
	// than not-exist.
	if err := os.MkdirAll(filepath.Join(dataDir, "code"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "code", listTestSession), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := ListChangedFiles(dataDir, listTestSession)
	if err == nil {
		t.Fatal("ListChangedFiles on a file session path = nil error, want the enumeration failure")
	}
	if paths != nil {
		t.Fatalf("paths = %q, want nil", paths)
	}
}

func TestListChangedFilesSkipsInvalidNamesSymlinksAndNonDirectories(t *testing.T) {
	dataDir := t.TempDir()
	sessionDir := filepath.Join(dataDir, "code", listTestSession)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The command-output spill directory: a non-hex sibling of the groups.
	if err := os.MkdirAll(filepath.Join(sessionDir, "output"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A valid group whose content would leak if the name check were missing.
	if err := os.MkdirAll(filepath.Join(sessionDir, "output", "snapshots", "1", "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMeta(t, filepath.Join(sessionDir, "output", "snapshots", "1", "e", "meta.json"), SnapshotMeta{OriginalPath: "/o", CanonicalPath: "/leaked"})

	// A symlinked group directory with real content behind it.
	real := filepath.Join(t.TempDir(), "realgroup")
	if err := os.MkdirAll(filepath.Join(real, "snapshots", "1", "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMeta(t, filepath.Join(real, "snapshots", "1", "e", "meta.json"), SnapshotMeta{OriginalPath: "/s", CanonicalPath: "/symlinked"})
	if err := os.Symlink(real, filepath.Join(sessionDir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}

	// A non-directory child with a valid group-ID name.
	if err := os.WriteFile(filepath.Join(sessionDir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A short-named group: content present, name invalid.
	if err := os.MkdirAll(filepath.Join(sessionDir, "short", "snapshots", "1", "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMeta(t, filepath.Join(sessionDir, "short", "snapshots", "1", "e", "meta.json"), SnapshotMeta{OriginalPath: "/h", CanonicalPath: "/shortgroup"})

	paths, err := ListChangedFiles(dataDir, listTestSession)
	if err != nil {
		t.Fatalf("ListChangedFiles: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("paths = %q, want every invalid-name, symlink and non-directory child skipped", paths)
	}
}

func TestListChangedFilesSkipsMalformedMetadataAndEmptyCanonical(t *testing.T) {
	dataDir := t.TempDir()
	sessionDir := filepath.Join(dataDir, "code", listTestSession)
	group := filepath.Join(sessionDir, "cccccccccccccccccccccccccccccccc", "snapshots", "1", "a")
	if err := os.MkdirAll(group, 0o755); err != nil {
		t.Fatal(err)
	}
	// Malformed metadata is skipped silently by ListTurns.
	if err := os.WriteFile(filepath.Join(group, "meta.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An empty-CanonicalPath entry is skipped: no legacy-path fallback.
	empty := filepath.Join(sessionDir, "cccccccccccccccccccccccccccccccc", "snapshots", "1", "b")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMeta(t, filepath.Join(empty, "meta.json"), SnapshotMeta{OriginalPath: "/legacy-only"})

	// The one valid entry.
	valid := filepath.Join(sessionDir, "cccccccccccccccccccccccccccccccc", "snapshots", "2", "c")
	if err := os.MkdirAll(valid, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMeta(t, filepath.Join(valid, "meta.json"), SnapshotMeta{OriginalPath: "/orig", CanonicalPath: "/kept.go"})

	paths, err := ListChangedFiles(dataDir, listTestSession)
	if err != nil {
		t.Fatalf("ListChangedFiles: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/kept.go" {
		t.Fatalf("paths = %q, want [/kept.go] only", paths)
	}
}

func TestListChangedFilesDeduplicatesAndSortsAcrossGroups(t *testing.T) {
	newMeta := func(original, canonical string) SnapshotMeta {
		return SnapshotMeta{OriginalPath: original, CanonicalPath: canonical}
	}
	groups := func() map[int][]SnapshotMeta {
		return map[int][]SnapshotMeta{
			1: {newMeta("/z", "/z.go"), newMeta("/a", "/a.go")},
			2: {newMeta("/a2", "/a.go"), newMeta("/m", "/m.go")},
		}
	}
	dataDir := t.TempDir()
	seedListGroup(t, dataDir, listTestSession, "dddddddddddddddddddddddddddddddd", groups())
	seedListGroup(t, dataDir, listTestSession, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", groups())
	paths, err := ListChangedFiles(dataDir, listTestSession)
	if err != nil {
		t.Fatalf("ListChangedFiles: %v", err)
	}
	want := []string{"/a.go", "/m.go", "/z.go"}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %q, want %q", paths, want)
	}
}

func TestListChangedFilesTurnErrorDiscardsResult(t *testing.T) {
	dataDir := t.TempDir()
	sessionDir := filepath.Join(dataDir, "code", listTestSession)

	// An earlier readable group contributes a path.
	seedListGroup(t, dataDir, listTestSession, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", map[int][]SnapshotMeta{
		1: {SnapshotMeta{OriginalPath: "/e", CanonicalPath: "/kept.go"}},
	})

	// A later group's turn is unreadable: ListTurns fails with its partial
	// entries, and the complete result — including the earlier group's
	// path — must be discarded.
	turnDir := filepath.Join(sessionDir, "ffffffffffffffffffffffffffffffff", "snapshots", "1", "e")
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMeta(t, filepath.Join(turnDir, "meta.json"), SnapshotMeta{OriginalPath: "/x", CanonicalPath: "/x.go"})
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block reads as root")
	}
	if err := os.Chmod(filepath.Join(sessionDir, "ffffffffffffffffffffffffffffffff", "snapshots", "1"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(sessionDir, "ffffffffffffffffffffffffffffffff", "snapshots", "1"), 0o755)
	})

	paths, err := ListChangedFiles(dataDir, listTestSession)
	if err == nil {
		t.Fatal("ListChangedFiles = nil error, want the turn-read failure")
	}
	if paths != nil {
		t.Fatalf("paths = %q, want the complete result discarded", paths)
	}
}
