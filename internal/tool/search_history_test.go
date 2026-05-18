package tool

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/memory"
)

func TestSearchHistoryRequiresQuery(t *testing.T) {
	tool := NewSearchHistory(&mockHistorySearcher{}, "project-a")

	result, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute error = %v, want nil", err)
	}
	if result != "error: query is required" {
		t.Fatalf("Execute result = %q, want query-required error", result)
	}
}

func TestSearchHistoryPassesQueryShapeToStore(t *testing.T) {
	store := &mockHistorySearcher{results: []memory.HistoryResult{{
		SectionContent: "Section body",
		SessionID:      "session-1",
		CreatedAt:      "2026-05-17T12:00:00Z",
		Project:        "Lightcode",
		CompactionPath: "/summaries/session-1.md",
	}}}
	tool := NewSearchHistory(store, "project-a")

	result, err := tool.Execute(context.Background(), map[string]any{
		"query":        "previous fix",
		"all_projects": true,
		"limit":        float64(5),
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if store.query != "previous fix" || store.projectID != "project-a" || !store.allProjects || store.limit != 5 {
		t.Fatalf("store call = query:%q project:%q all:%v limit:%d", store.query, store.projectID, store.allProjects, store.limit)
	}
	want := "Session: session-1 (project: Lightcode, compacted: 2026-05-17T12:00:00Z)\nFull summary: /summaries/session-1.md\n\nSection body"
	if result != want {
		t.Fatalf("Execute result = %q, want formatted history result", result)
	}
}

func TestSearchHistoryDefaultsLimitAndCurrentProject(t *testing.T) {
	store := &mockHistorySearcher{}
	tool := NewSearchHistory(store, "project-a")

	result, err := tool.Execute(context.Background(), map[string]any{"query": "none"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "No matching session history found." {
		t.Fatalf("Execute result = %q, want no-results message", result)
	}
	if store.projectID != "project-a" || store.allProjects || store.limit != 3 {
		t.Fatalf("store call = project:%q all:%v limit:%d, want current project limit 3", store.projectID, store.allProjects, store.limit)
	}
}

func TestSearchHistoryFormatsMultipleResultsAndStoreError(t *testing.T) {
	store := &mockHistorySearcher{results: []memory.HistoryResult{
		{SessionID: "s1", Project: "P1", CreatedAt: "t1", CompactionPath: "/c1.md", SectionContent: "First section"},
		{SessionID: "s2", Project: "P2", CreatedAt: "t2", CompactionPath: "/c2.md", SectionContent: "Second section"},
	}}
	tool := NewSearchHistory(store, "project-a")

	result, err := tool.Execute(context.Background(), map[string]any{"query": "history"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "Session: s1 (project: P1, compacted: t1)") || !strings.Contains(result, "\n---\n") || !strings.Contains(result, "Session: s2 (project: P2, compacted: t2)") {
		t.Fatalf("Execute result = %q, want two formatted results separated by rule", result)
	}

	store.err = errors.New("history failed")
	result, err = tool.Execute(context.Background(), map[string]any{"query": "history"})
	if err != nil {
		t.Fatalf("Execute error = %v, want nil", err)
	}
	if result != "error: history failed" {
		t.Fatalf("Execute result = %q, want store error string", result)
	}
}

func TestSearchHistorySchemaShape(t *testing.T) {
	tool := NewSearchHistory(&mockHistorySearcher{}, "project-a")
	if tool.Name() != "search_history" {
		t.Fatalf("Name = %q, want search_history", tool.Name())
	}
	schema := tool.ParametersSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v, want map", schema["properties"])
	}
	for _, key := range []string{"query", "all_projects", "limit"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("schema properties = %#v, want key %q", props, key)
		}
	}
	if required, ok := schema["required"].([]string); !ok || !reflect.DeepEqual(required, []string{"query"}) {
		t.Fatalf("schema required = %#v, want [query]", schema["required"])
	}
}

type mockHistorySearcher struct {
	results     []memory.HistoryResult
	err         error
	query       string
	projectID   string
	allProjects bool
	limit       int
}

func (m *mockHistorySearcher) SearchHistory(query, projectID string, allProjects bool, limit int) ([]memory.HistoryResult, error) {
	m.query = query
	m.projectID = projectID
	m.allProjects = allProjects
	m.limit = limit
	return m.results, m.err
}
