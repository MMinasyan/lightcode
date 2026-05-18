package tool

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/memory"
)

func TestSearchMemoryRequiresQuery(t *testing.T) {
	tool := NewSearchMemory(&mockMemorySearcher{}, "project-a")

	result, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute error = %v, want nil", err)
	}
	if result != "error: query is required" {
		t.Fatalf("Execute result = %q, want query-required error", result)
	}
}

func TestSearchMemoryPassesQueryShapeToStore(t *testing.T) {
	store := &mockMemorySearcher{results: []memory.MemoryResult{{
		Title:     "Useful detail",
		Content:   "Content body",
		CreatedAt: "2026-05-17T12:00:00Z",
		Project:   "Lightcode",
		FilePath:  "/projects/lightcode/memories/useful.md",
	}}}
	tool := NewSearchMemory(store, "project-a")

	result, err := tool.Execute(context.Background(), map[string]any{
		"query":        "useful",
		"all_projects": true,
		"limit":        float64(7),
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if store.query != "useful" || store.projectID != "project-a" || !store.allProjects || store.limit != 7 {
		t.Fatalf("store call = query:%q project:%q all:%v limit:%d", store.query, store.projectID, store.allProjects, store.limit)
	}
	want := "**Useful detail** (project: Lightcode, saved: 2026-05-17T12:00:00Z)\nFile: /projects/lightcode/memories/useful.md\n\nContent body"
	if result != want {
		t.Fatalf("Execute result = %q, want formatted memory result", result)
	}
}

func TestSearchMemoryDefaultsLimitAndCurrentProject(t *testing.T) {
	store := &mockMemorySearcher{}
	tool := NewSearchMemory(store, "project-a")

	result, err := tool.Execute(context.Background(), map[string]any{"query": "none"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "No memories found." {
		t.Fatalf("Execute result = %q, want no-results message", result)
	}
	if store.projectID != "project-a" || store.allProjects || store.limit != 3 {
		t.Fatalf("store call = project:%q all:%v limit:%d, want current project limit 3", store.projectID, store.allProjects, store.limit)
	}
}

func TestSearchMemoryFormatsMultipleResultsAndStoreError(t *testing.T) {
	store := &mockMemorySearcher{results: []memory.MemoryResult{
		{Title: "First", Content: "First body", CreatedAt: "t1", Project: "P1", FilePath: "/m/first.md"},
		{Title: "Second", Content: "Second body", CreatedAt: "t2", Project: "P2", FilePath: "/m/second.md"},
	}}
	tool := NewSearchMemory(store, "project-a")

	result, err := tool.Execute(context.Background(), map[string]any{"query": "memory"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "**First** (project: P1, saved: t1)") || !strings.Contains(result, "\n---\n") || !strings.Contains(result, "**Second** (project: P2, saved: t2)") {
		t.Fatalf("Execute result = %q, want two formatted results separated by rule", result)
	}

	store.err = errors.New("search failed")
	result, err = tool.Execute(context.Background(), map[string]any{"query": "memory"})
	if err != nil {
		t.Fatalf("Execute error = %v, want nil", err)
	}
	if result != "error: search failed" {
		t.Fatalf("Execute result = %q, want store error string", result)
	}
}

func TestSearchMemorySchemaShape(t *testing.T) {
	tool := NewSearchMemory(&mockMemorySearcher{}, "project-a")
	if tool.Name() != "search_memory" {
		t.Fatalf("Name = %q, want search_memory", tool.Name())
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

type mockMemorySearcher struct {
	results     []memory.MemoryResult
	err         error
	query       string
	projectID   string
	allProjects bool
	limit       int
}

func (m *mockMemorySearcher) SearchMemory(query, projectID string, allProjects bool, limit int) ([]memory.MemoryResult, error) {
	m.query = query
	m.projectID = projectID
	m.allProjects = allProjects
	m.limit = limit
	return m.results, m.err
}
