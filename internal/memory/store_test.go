package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestIndexSummaryReturnsMetaWriteErrorBeforeSections(t *testing.T) {
	home := t.TempDir()
	store := &Store{embedder: &fakeMemoryEmbedder{}, projectsRoot: t.TempDir(), home: home}
	dir := filepath.Join(home, ".lightcode", "summaries", "session-1")
	if err := os.MkdirAll(filepath.Join(dir, "meta.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := store.IndexSummary("session-1", "project-1", "Project", "## Goal\nbody", "now", "/tmp/compaction.json")
	if err == nil || !strings.Contains(err.Error(), "write summary meta") {
		t.Fatalf("IndexSummary error = %v, want write summary meta", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "00-goal.md")); !os.IsNotExist(err) {
		t.Fatalf("00-goal.md stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "00-goal.vec")); !os.IsNotExist(err) {
		t.Fatalf("00-goal.vec stat error = %v, want not exist", err)
	}
}

func TestStoreSaveMemoryWithoutEmbedderReturnsUnavailable(t *testing.T) {
	store := NewStore(nil, t.TempDir(), t.TempDir())
	memoriesDir := filepath.Join(t.TempDir(), "memories")

	if _, err := store.SaveMemory(memoriesDir, "Title", "Content"); !errors.Is(err, errEmbedderUnavailable) {
		t.Fatalf("SaveMemory error = %v, want %v", err, errEmbedderUnavailable)
	}
	if entries, err := os.ReadDir(memoriesDir); err == nil && len(entries) != 0 {
		t.Fatalf("memory files were written with unavailable embedder: %v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadDir memoriesDir: %v", err)
	}
}

func TestStoreSearchMemoryWithoutEmbedderReturnsUnavailable(t *testing.T) {
	store := NewStore(nil, t.TempDir(), t.TempDir())

	if _, err := store.SearchMemory("query", "project-1", false, 3); !errors.Is(err, errEmbedderUnavailable) {
		t.Fatalf("SearchMemory error = %v, want %v", err, errEmbedderUnavailable)
	}
}

func TestStoreSearchHistoryWithoutEmbedderReturnsUnavailable(t *testing.T) {
	store := NewStore(nil, t.TempDir(), t.TempDir())

	if _, err := store.SearchHistory("query", "project-1", false, 3); !errors.Is(err, errEmbedderUnavailable) {
		t.Fatalf("SearchHistory error = %v, want %v", err, errEmbedderUnavailable)
	}
}

func TestStoreIndexSummaryWithoutEmbedderReturnsUnavailable(t *testing.T) {
	home := t.TempDir()
	store := NewStore(nil, t.TempDir(), home)

	err := store.IndexSummary("session-1", "project-1", "Project", "## Goal\nbody", "now", "/tmp/compaction.json")
	if !errors.Is(err, errEmbedderUnavailable) {
		t.Fatalf("IndexSummary error = %v, want %v", err, errEmbedderUnavailable)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "summaries")); !os.IsNotExist(err) {
		t.Fatalf("summaries root stat error = %v, want not exist", err)
	}
}

func TestStoreIndexSummaryWithUnavailableEmbedderWritesNoPartialFiles(t *testing.T) {
	home := t.TempDir()
	store := &Store{
		embedder:     &fakeMemoryEmbedder{err: errEmbedderUnavailable},
		projectsRoot: t.TempDir(),
		home:         home,
	}

	err := store.IndexSummary("session-1", "project-1", "Project", "## Goal\nbody", "now", "/tmp/compaction.json")
	if !errors.Is(err, errEmbedderUnavailable) {
		t.Fatalf("IndexSummary error = %v, want %v", err, errEmbedderUnavailable)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "summaries")); !os.IsNotExist(err) {
		t.Fatalf("summaries root stat error = %v, want not exist", err)
	}
}

func TestStoreReindexWithoutEmbedderReturnsUnavailable(t *testing.T) {
	store := NewStore(nil, t.TempDir(), t.TempDir())

	if err := store.Reconcile(); !errors.Is(err, errEmbedderUnavailable) {
		t.Fatalf("Reconcile error = %v, want %v", err, errEmbedderUnavailable)
	}
}

func TestStoreReindexWithUnavailableEmbedderReturnsUnavailable(t *testing.T) {
	projectsRoot := t.TempDir()
	memoriesDir := filepath.Join(projectsRoot, "project-1", "memories")
	if err := os.MkdirAll(memoriesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mdPath := filepath.Join(memoriesDir, "20260626-memory.md")
	if err := os.WriteFile(mdPath, []byte("---\ntitle: Existing\ncreated_at: now\n---\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &Store{
		embedder:     &fakeMemoryEmbedder{err: errEmbedderUnavailable},
		projectsRoot: projectsRoot,
		home:         t.TempDir(),
	}

	if err := store.Reconcile(); !errors.Is(err, errEmbedderUnavailable) {
		t.Fatalf("Reconcile error = %v, want %v", err, errEmbedderUnavailable)
	}
	if _, err := os.Stat(strings.TrimSuffix(mdPath, ".md") + ".vec"); !os.IsNotExist(err) {
		t.Fatalf("vec stat error = %v, want not exist", err)
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	home := t.TempDir()
	projectsRoot := t.TempDir()
	projectID := "project-1"
	projectRoot := filepath.Join(projectsRoot, projectID)
	memoriesDir := filepath.Join(projectRoot, "memories")
	if err := os.MkdirAll(memoriesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "meta.json"), []byte(`{"name":"Project"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStoreWithEmbedder(&countingMemoryEmbedder{}, projectsRoot, home)

	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for i := 0; i < 4; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.SaveMemory(memoriesDir, fmt.Sprintf("Title %d", i), fmt.Sprintf("content %d", i)); err != nil {
				errCh <- err
			}
		}()
	}
	for i := 0; i < 4; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.IndexSummary(fmt.Sprintf("session-%d", i), projectID, "Project", "## Goal\nsummary", "now", "/tmp/compaction.json"); err != nil {
				errCh <- err
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.SearchMemory("content", projectID, false, 3)
			_, _ = store.SearchHistory("summary", projectID, false, 3)
			_ = store.Reconcile()
			_ = store.DeleteSessionSummaries("delete-only")
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent store operation: %v", err)
	}
}

type fakeMemoryEmbedder struct {
	vec []float32
	err error
}

func (f *fakeMemoryEmbedder) Embed(string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.vec != nil {
		return append([]float32(nil), f.vec...), nil
	}
	return []float32{1, 0, 0}, nil
}

func (f *fakeMemoryEmbedder) Close() {}

type countingMemoryEmbedder struct {
	calls int
}

func (f *countingMemoryEmbedder) Embed(string) ([]float32, error) {
	f.calls++
	return []float32{1, 0, 0}, nil
}

func (f *countingMemoryEmbedder) Close() {}
