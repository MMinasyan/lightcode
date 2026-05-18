package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadProjectNameAndSummaryMeta(t *testing.T) {
	dir := t.TempDir()
	projectMeta := filepath.Join(dir, "project.json")
	if err := os.WriteFile(projectMeta, []byte(`{"name":"Logwise"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readProjectName(projectMeta); got != "Logwise" {
		t.Fatalf("readProjectName = %q, want Logwise", got)
	}
	if got := readProjectName(filepath.Join(dir, "missing.json")); got != "" {
		t.Fatalf("readProjectName missing = %q, want empty", got)
	}

	summaryMetaPath := filepath.Join(dir, "summary.json")
	if err := os.WriteFile(summaryMetaPath, []byte(`{"project_id":"p1","project_name":"Project","created_at":"now","compaction_path":"/c.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readSummaryMeta(summaryMetaPath)
	if got.ProjectID != "p1" || got.ProjectName != "Project" || got.CreatedAt != "now" || got.CompactionPath != "/c.json" {
		t.Fatalf("readSummaryMeta = %+v", got)
	}
	if got := readSummaryMeta(filepath.Join(dir, "missing-summary.json")); got != (summaryMeta{}) {
		t.Fatalf("readSummaryMeta missing = %+v, want zero", got)
	}
}

func TestStoreDeleteSessionSummaries(t *testing.T) {
	home := t.TempDir()
	store := NewStore(nil, t.TempDir(), home)
	dir := filepath.Join(home, ".lightcode", "summaries", "session-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.md"), []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSessionSummaries("session-1"); err != nil {
		t.Fatalf("DeleteSessionSummaries: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("summary dir stat error = %v, want not exist", err)
	}
}
