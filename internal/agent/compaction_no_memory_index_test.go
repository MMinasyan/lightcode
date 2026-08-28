package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestCompactionFailureLeavesRecordAndContextUnchanged is the direct compaction
// failure regression, driven without any memory hooks: a summarizer failure must
// leave both the compaction record and the conversation context untouched. Before
// memory was removed this path also reconciled summary-index data, so a silent
// success would have created it; on failure nothing is written anywhere. The
// summarizer's working store may still be opened for the call, but it carries no
// record and rewrites nothing.
func TestCompactionFailureLeavesRecordAndContextUnchanged(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, strings.Repeat("alpha ", 1600))
	appendUserTurn(t, a, strings.Repeat("beta ", 1600))
	before := userContents(a.SessionMessages())

	// Dead-port transport: the summarizer HTTP call fails, so compact.Run errors
	// before SaveCompaction is ever reached and no rewrite is published.
	if err := a.runCompaction(context.Background(), false); err == nil {
		t.Fatal("runCompaction succeeded, want summarizer failure")
	}

	// The compaction record was never written.
	if _, err := os.Stat(filepath.Join(a.store.Dir(), "compaction.json")); !os.IsNotExist(err) {
		t.Fatalf("compaction.json created despite summarizer failure: %v", err)
	}
	// The context is unchanged: no rewrite published, messages intact.
	if got := userContents(a.SessionMessages()); !equalStrings(got, before) {
		t.Fatalf("context changed after a failed compaction:\n before=%v\n after =%v", before, got)
	}
}

// TestCompactionSuccessWritesNoSemanticSummaryIndex is the compaction success
// regression: a successful summary persists its record/context but creates no
// semantic summary-index data (no embedding/.vec files, no summaries index, no
// model cache). Before memory was removed this path embedded every summary into
// a sibling vector and wrote under the bge model cache; removal means none of
// those paths are touched.
func TestCompactionSuccessWritesNoSemanticSummaryIndex(t *testing.T) {
	var summarizerCalled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		summarizerCalled.Store(true)
		writeTextResponse(w, "the compacted summary")
	}))
	defer server.Close()

	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, strings.Repeat("alpha ", 1600))
	appendUserTurn(t, a, strings.Repeat("beta ", 1600))
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"

	if err := a.runCompaction(context.Background(), false); err != nil {
		t.Fatalf("runCompaction returned error: %v", err)
	}
	if !summarizerCalled.Load() {
		t.Fatal("summarizer was not called")
	}

	data, err := os.ReadFile(filepath.Join(a.store.Dir(), "compaction.json"))
	if err != nil {
		t.Fatalf("read compaction.json: %v", err)
	}
	if !strings.Contains(string(data), "the compacted summary") {
		t.Fatalf("compaction.json = %s, want the persisted summary", data)
	}

	assertNoSemanticSummaryIndex(t, a.home)
}

// assertNoSemanticSummaryIndex fails if any semantic summary-index artifact
// exists under root: a summaries index directory, a bge embedder cache path, or
// any .vec embedding file.
func assertNoSemanticSummaryIndex(t *testing.T, root string) {
	t.Helper()
	var violations []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		switch {
		case d.IsDir() && d.Name() == "summaries":
			violations = append(violations, "summary-index dir: "+path)
		case strings.Contains(path, "bge-small"):
			violations = append(violations, "embedder cache path: "+path)
		case !d.IsDir() && strings.HasSuffix(d.Name(), ".vec"):
			violations = append(violations, "embedding file: "+path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk home: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("compaction created semantic summary-index data:\n  %s", strings.Join(violations, "\n  "))
	}
}
