package project

import (
	"os"
	"path/filepath"
	"strings"
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
	if p.ID == "" || p.Path != projectRoot || p.Name != filepath.Base(projectRoot) {
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
	older := Project{ID: "old", Name: "old", Path: "/old", CreatedAt: time.Now().UTC().Format(time.RFC3339), LastActivity: 1}
	newer := Project{ID: "new", Name: "new", Path: "/new", CreatedAt: time.Now().UTC().Format(time.RFC3339), LastActivity: 2}
	writeProjectForTest(t, root, older)
	writeProjectForTest(t, root, newer)
	projects, err := List(root)
	if err != nil || len(projects) != 2 {
		t.Fatalf("List = %+v, %v; want 2", projects, err)
	}
	found, err := FindByPath(root, "/new")
	if err != nil || found == nil || found.ID != "new" {
		t.Fatalf("FindByPath = %+v, %v; want new", found, err)
	}
	sorted, err := ListSortedByActivity(root)
	if err != nil || len(sorted) != 2 || sorted[0].ID != "new" || sorted[1].ID != "old" {
		t.Fatalf("ListSortedByActivity = %+v, %v", sorted, err)
	}
	if err := TouchActivity(root, "old"); err != nil {
		t.Fatalf("TouchActivity: %v", err)
	}
	found, err = FindByPath(root, "/old")
	if err != nil || found.LastActivity < older.LastActivity {
		t.Fatalf("TouchActivity result = %+v, %v", found, err)
	}
	if got, err := List(filepath.Join(root, "missing")); err != nil || got != nil {
		t.Fatalf("List missing = %+v, %v; want nil, nil", got, err)
	}
}

func TestProjectIDAndJSONHelpers(t *testing.T) {
	id1, err := newProjectID()
	if err != nil {
		t.Fatalf("newProjectID: %v", err)
	}
	id2, err := newProjectID()
	if err != nil {
		t.Fatalf("newProjectID: %v", err)
	}
	if len(id1) != 36 || id1 == id2 || strings.Count(id1, "-") != 4 {
		t.Fatalf("ids = %q, %q; want distinct UUID-ish IDs", id1, id2)
	}

	path := filepath.Join(t.TempDir(), "x.json")
	want := Project{ID: "id", Name: "n", Path: "/p", LastActivity: 1}
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

func writeProjectForTest(t *testing.T, root string, p Project) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, p.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(root, p.ID, "meta.json"), p); err != nil {
		t.Fatal(err)
	}
}
