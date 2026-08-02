package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/project"
)

// TestOwnerGlobalSessionIDResolverRejectsAmbiguity proves a bare session id
// that exists in more than one project is rejected by every adapter-facing
// route that resolves ids against disk: the id is ambiguous, so no route may
// silently pick one project over another.
func TestOwnerGlobalSessionIDResolverRejectsAmbiguity(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	a.Init(context.Background())

	first, err := a.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure first project: %v", err)
	}
	second, err := project.EnsureForPath(a.projects.Root(), t.TempDir())
	if err != nil {
		t.Fatalf("ensure second project: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("test setup: projects are not distinct")
	}

	// Plant the same session directory in both projects' sessions namespaces.
	const sharedID = "d00dcafe"
	for _, proj := range []*project.Project{first, second} {
		dir := filepath.Join(a.projects.SessionsRoot(proj.ID), sharedID)
		if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "turns"), 0o700); err != nil {
			t.Fatal(err)
		}
		meta := `{"id":"` + sharedID + `","state":"active","project_path":"` + proj.Path + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	routes := []struct {
		name string
		run  func() error
	}{
		{"OpenSession", func() error { _, err := a.OpenSession(sharedID); return err }},
		{"SessionMessagesFor", func() error { _, err := a.SessionMessagesFor(sharedID); return err }},
		{"SessionArchive", func() error { return a.SessionArchive(sharedID) }},
		{"SessionDelete", func() error { return a.SessionDelete(sharedID) }},
	}
	for _, rt := range routes {
		err := rt.run()
		if err == nil {
			t.Fatalf("%s(%q) accepted an ambiguous id", rt.name, sharedID)
		}
		if !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("%s(%q) error = %v, want ambiguity rejection", rt.name, sharedID, err)
		}
	}
}
