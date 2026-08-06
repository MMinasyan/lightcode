package project

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolverEnsureCurrentAndSessionsRoot(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	resolver, err := NewResolver(home, projectRoot)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if resolver.ProjectRoot() != projectRoot {
		t.Fatalf("ProjectRoot = %q, want %q", resolver.ProjectRoot(), projectRoot)
	}
	if current, err := resolver.Current(); err != nil || current != nil {
		t.Fatalf("Current before Ensure = %+v, %v; want nil, nil", current, err)
	}
	p, err := resolver.Ensure()
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if p.ID != projectID(projectRoot) || p.Path != projectRoot || p.Name != filepath.Base(projectRoot) {
		t.Fatalf("project = %+v", p)
	}
	if !strings.HasSuffix(resolver.SessionsRoot(p.ID), filepath.Join(p.ID, "sessions")) {
		t.Fatalf("SessionsRoot = %q", resolver.SessionsRoot(p.ID))
	}
	current, err := resolver.Current()
	if err != nil || current == nil || current.ID != p.ID {
		t.Fatalf("Current after Ensure = %+v, %v; want %s", current, err, p.ID)
	}
	again, err := resolver.Ensure()
	if err != nil || again.ID != p.ID {
		t.Fatalf("Ensure existing = %+v, %v; want %s", again, err, p.ID)
	}
}

func TestListFindSortAndTouch(t *testing.T) {
	root := t.TempDir()
	older := ensureForTest(t, root, filepath.Join(t.TempDir(), "old"))
	newer := ensureForTest(t, root, filepath.Join(t.TempDir(), "new"))
	// Force a deterministic activity ordering.
	setActivityForTest(t, root, older.ID, 1)
	setActivityForTest(t, root, newer.ID, 2)

	projects, err := List(root)
	if err != nil || len(projects) != 2 {
		t.Fatalf("List = %+v, %v; want 2", projects, err)
	}
	found, err := FindByPath(root, newer.Path)
	if err != nil || found == nil || found.ID != newer.ID {
		t.Fatalf("FindByPath = %+v, %v; want %s", found, err, newer.ID)
	}
	sorted, err := ListSortedByActivity(root)
	if err != nil || len(sorted) != 2 || sorted[0].ID != newer.ID || sorted[1].ID != older.ID {
		t.Fatalf("ListSortedByActivity = %+v, %v", sorted, err)
	}
	if err := TouchActivity(root, older.ID); err != nil {
		t.Fatalf("TouchActivity: %v", err)
	}
	found, err = FindByPath(root, older.Path)
	if err != nil || found.LastActivity < 1 {
		t.Fatalf("TouchActivity result = %+v, %v", found, err)
	}
	if got, err := List(filepath.Join(root, "missing")); err != nil || got != nil {
		t.Fatalf("List missing = %+v, %v; want nil, nil", got, err)
	}
}

// TestProjectIdentityIsDeterministicWithoutLegacyReuse: the same
// normalized path always yields the same p-<sha256> id, distinct paths yield
// distinct ids, and re-ensuring reuses the record without a second CreatedAt.
func TestProjectIdentityIsDeterministicWithoutLegacyReuse(t *testing.T) {
	root := t.TempDir()
	pathA := filepath.Join(t.TempDir(), "a")
	pathB := filepath.Join(t.TempDir(), "b")

	id := projectID(pathA)
	if !strings.HasPrefix(id, "p-") || len(id) != len("p-")+64 {
		t.Fatalf("id = %q; want p- plus 64 hex", id)
	}
	if projectID(pathA) != id {
		t.Fatal("projectID not deterministic for the same path")
	}
	if projectID(pathB) == id {
		t.Fatal("distinct paths produced the same id")
	}
	// Trailing-slash and dot segments normalize to the same identity.
	base, _ := normalizePath(pathA)
	trailing, _ := normalizePath(pathA + string(filepath.Separator))
	dotted, _ := normalizePath(filepath.Join(pathA, "."))
	if trailing != base || dotted != base || projectID(base) != id {
		t.Fatal("normalization mismatch for equivalent paths")
	}

	first, err := EnsureForPath(root, pathA)
	if err != nil {
		t.Fatalf("ensure a: %v", err)
	}
	if first.ID != id {
		t.Fatalf("ensured id = %q, want %q", first.ID, id)
	}
	second, err := EnsureForPath(root, pathA)
	if err != nil {
		t.Fatalf("re-ensure a: %v", err)
	}
	if second.ID != first.ID || second.CreatedAt != first.CreatedAt {
		t.Fatalf("re-ensure changed record: %+v vs %+v", second, first)
	}
	entries, _ := os.ReadDir(root)
	projectDirs := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			projectDirs++
		}
	}
	if projectDirs != 1 {
		t.Fatalf("project dirs = %d, want 1", projectDirs)
	}
}

func TestJSONHelpersRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	want := Project{ID: "id", Name: "n", Path: "/p", CreatedAt: time.Now().UTC().Format(time.RFC3339), LastActivity: 1}
	if err := writeJSON(path, want); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	var got Project
	if err := readJSON(path, &got); err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if got != want {
		t.Fatalf("readJSON = %+v, want %+v", got, want)
	}
}

// TestProjectIdentityComesFromDirectory proves every project reader reports
// a record under its own directory's id: a record whose stored id names a
// different project is listed, found and ensured under the directory's id,
// never under the id it declares.
func TestProjectIdentityComesFromDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")

	clean, err := normalizePath(path)
	if err != nil {
		t.Fatal(err)
	}
	id := projectID(clean)
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The record declares another project's id; its stored path stays correct.
	p := Project{ID: "p-declared-elsewhere", Name: "proj", Path: clean}
	if err := writeJSON(metaPathFor(root, id), p); err != nil {
		t.Fatal(err)
	}

	projects, err := List(root)
	if err != nil || len(projects) != 1 {
		t.Fatalf("List = %+v, %v; want one", projects, err)
	}
	if projects[0].ID != id {
		t.Fatalf("List id = %q, want the directory's id %q", projects[0].ID, id)
	}

	found, err := FindByPath(root, path)
	if err != nil || found == nil || found.ID != id {
		t.Fatalf("FindByPath = %+v, %v; want %s", found, err, id)
	}

	ensured, err := EnsureForPath(root, path)
	if err != nil || ensured == nil || ensured.ID != id {
		t.Fatalf("EnsureForPath = %+v, %v; want %s", ensured, err, id)
	}
}

// TestPathAwareReadersRejectRecordWhoseStoredPathDiffers proves FindByPath
// and EnsureForPath reject a record whose stored path is not the path
// searched for. The rejection is an error, never (nil, nil), which callers
// read as "no such project" and would answer by minting a second project
// over the corrupt record.
func TestPathAwareReadersRejectRecordWhoseStoredPathDiffers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")

	clean, err := normalizePath(path)
	if err != nil {
		t.Fatal(err)
	}
	id := projectID(clean)
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := Project{ID: id, Name: "proj", Path: filepath.Join(t.TempDir(), "other")}
	if err := writeJSON(metaPathFor(root, id), p); err != nil {
		t.Fatal(err)
	}

	if found, err := FindByPath(root, path); err == nil {
		t.Fatalf("FindByPath = %+v, nil error; want rejection of the stored path mismatch", found)
	}
	if _, err := EnsureForPath(root, path); err == nil {
		t.Fatal("EnsureForPath accepted a record whose stored path does not match")
	}
}

// TestProjectIdentityConcurrentEnsure: concurrent creators of the
// same path converge on one record with one deterministic id and one
// CreatedAt; neither misses the other and overwrites it.
func TestProjectIdentityConcurrentEnsure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "proj")

	const workers = 16
	var wg sync.WaitGroup
	results := make([]*Project, workers)
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = EnsureForPath(root, path)
		}(i)
	}
	wg.Wait()

	wantID := projectID(path)
	var createdAt string
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if results[i].ID != wantID {
			t.Fatalf("worker %d id = %q, want %q", i, results[i].ID, wantID)
		}
		if createdAt == "" {
			createdAt = results[i].CreatedAt
		} else if results[i].CreatedAt != createdAt {
			t.Fatalf("divergent CreatedAt: %q vs %q", results[i].CreatedAt, createdAt)
		}
	}
	entries, _ := os.ReadDir(root)
	projectDirs := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			projectDirs++
		}
	}
	if projectDirs != 1 {
		t.Fatalf("project dirs = %d, want 1", projectDirs)
	}
}

func ensureForTest(t *testing.T, root, path string) *Project {
	t.Helper()
	p, err := EnsureForPath(root, path)
	if err != nil {
		t.Fatalf("ensure %s: %v", path, err)
	}
	return p
}

func setActivityForTest(t *testing.T, root, id string, activity int64) {
	t.Helper()
	metaPath := metaPathFor(root, id)
	var p Project
	if err := readJSON(metaPath, &p); err != nil {
		t.Fatal(err)
	}
	p.LastActivity = activity
	if err := writeJSON(metaPath, p); err != nil {
		t.Fatal(err)
	}
}
