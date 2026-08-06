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

func TestReadProjectNameAndSummaryIndex(t *testing.T) {
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

	indexPath := filepath.Join(dir, "index.json")
	if err := os.WriteFile(indexPath, []byte(`{"generation":"gen-abc","project_id":"p1","project_name":"Project","created_at":"now","compaction_path":"/c.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := readSummaryIndex(indexPath)
	if !ok || got.Generation != "gen-abc" || got.ProjectID != "p1" || got.ProjectName != "Project" || got.CreatedAt != "now" || got.CompactionPath != "/c.json" {
		t.Fatalf("readSummaryIndex = %+v, ok=%v", got, ok)
	}
	if _, ok := readSummaryIndex(filepath.Join(dir, "missing.json")); ok {
		t.Fatal("readSummaryIndex missing = ok, want not ok")
	}
	// An index with no generation names no committed record.
	noGen := filepath.Join(dir, "nogen.json")
	if err := os.WriteFile(noGen, []byte(`{"project_id":"p1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSummaryIndex(noGen); ok {
		t.Fatal("readSummaryIndex without generation = ok, want not ok")
	}
	// A generation that is not a single path component is joined into a path,
	// so it must be rejected rather than read through.
	escape := filepath.Join(dir, "escape.json")
	if err := os.WriteFile(escape, []byte(`{"generation":"../.."}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSummaryIndex(escape); ok {
		t.Fatal("readSummaryIndex with escaping generation = ok, want not ok")
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

func TestIndexSummaryFailsAtomicallyOnIndexWriteError(t *testing.T) {
	home := t.TempDir()
	store := &Store{embedder: &fakeMemoryEmbedder{}, projectsRoot: t.TempDir(), home: home}
	sessionDir := filepath.Join(home, ".lightcode", "summaries", "session-1")
	// Make index.json a directory so its atomic publication fails after the
	// generation's sections are written; the generation must stay uncommitted.
	if err := os.MkdirAll(filepath.Join(sessionDir, "index.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := store.IndexSummary("session-1", "project-1", "Project", "## Goal\nbody", "now", "/tmp/compaction.json")
	if err == nil || !strings.Contains(err.Error(), "write summary index") {
		t.Fatalf("IndexSummary error = %v, want write summary index", err)
	}
	if _, ok := readSummaryIndex(filepath.Join(sessionDir, "index.json")); ok {
		t.Fatal("summary index committed despite write failure")
	}
	results, err := store.SearchHistory("body", "project-1", false, 5)
	if err != nil {
		t.Fatalf("SearchHistory: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("SearchHistory returned %d results from an uncommitted generation", len(results))
	}
}

func TestSearchHistoryReadsCommittedGenerationAndIgnoresOrphans(t *testing.T) {
	home := t.TempDir()
	store := NewStoreWithEmbedder(&fakeMemoryEmbedder{}, t.TempDir(), home)

	if err := store.IndexSummary("session-1", "project-1", "Project", "## Goal\nfindme", "now", "/c.json"); err != nil {
		t.Fatalf("IndexSummary: %v", err)
	}
	sessionDir := filepath.Join(home, ".lightcode", "summaries", "session-1")
	idx, ok := readSummaryIndex(filepath.Join(sessionDir, "index.json"))
	if !ok || idx.Generation == "" {
		t.Fatalf("no committed index after IndexSummary: %+v ok=%v", idx, ok)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, idx.Generation, "00-goal.md")); err != nil {
		t.Fatalf("committed section md missing: %v", err)
	}

	// An orphan generation not named by index.json must be ignored.
	orphan := filepath.Join(sessionDir, "gen-orphan")
	writeSummarySectionForTest(t, orphan, "00-goal", "orphan-body")

	// A session dir with no index.json at all must be ignored.
	writeSummarySectionForTest(t, filepath.Join(home, ".lightcode", "summaries", "session-2", "gen-x"), "00-goal", "uncommitted-body")

	results, err := store.SearchHistory("findme", "project-1", false, 10)
	if err != nil {
		t.Fatalf("SearchHistory: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("SearchHistory returned %d results, want 1 (committed only)", len(results))
	}
	if results[0].SectionContent != "findme" || results[0].SessionID != "session-1" {
		t.Fatalf("unexpected result: %+v", results[0])
	}
}

func writeSummarySectionForTest(t *testing.T, dir, base, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteVec(filepath.Join(dir, base+".vec"), []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, base+".md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSaveMemorySameTitleDistinctPairs(t *testing.T) {
	memoriesDir := filepath.Join(t.TempDir(), "memories")
	store := NewStoreWithEmbedder(&fakeMemoryEmbedder{}, t.TempDir(), t.TempDir())

	fp1, err := store.SaveMemory(memoriesDir, "Same Title", "content one")
	if err != nil {
		t.Fatalf("save 1: %v", err)
	}
	fp2, err := store.SaveMemory(memoriesDir, "Same Title", "content two")
	if err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if fp1 == fp2 {
		t.Fatal("two same-title saves produced the same .md path")
	}
	for _, fp := range []string{fp1, fp2} {
		if _, err := os.Stat(fp); err != nil {
			t.Fatalf("md missing: %v", err)
		}
		if _, err := os.Stat(strings.TrimSuffix(fp, ".md") + ".vec"); err != nil {
			t.Fatalf("vec missing for %s: %v", fp, err)
		}
	}
	entries, _ := os.ReadDir(memoriesDir)
	var mds, vecs int
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".md"):
			mds++
		case strings.HasSuffix(e.Name(), ".vec"):
			vecs++
		}
	}
	if mds != 2 || vecs != 2 {
		t.Fatalf("got %d .md + %d .vec, want 2 each", mds, vecs)
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

// TestStoreCloseBorrowsEmbedder proves closing a store never closes the borrowed
// embedder, so one project's store close cannot disable the shared embedder.
func TestStoreCloseBorrowsEmbedder(t *testing.T) {
	e := &closeTrackingEmbedder{}
	s := NewStoreWithEmbedder(e, t.TempDir(), t.TempDir())
	s.Close()
	if e.closed {
		t.Fatal("Store.Close closed the borrowed embedder; it must be closed only by its owner")
	}
}

type closeTrackingEmbedder struct{ closed bool }

func (e *closeTrackingEmbedder) Embed(string) ([]float32, error) { return []float32{1, 0, 0}, nil }
func (e *closeTrackingEmbedder) Close()                          { e.closed = true }
