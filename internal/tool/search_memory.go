package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/memory"
)

type SearchMemory struct {
	store     *memory.Store
	projectID string
}

func NewSearchMemory(store *memory.Store, projectID string) *SearchMemory {
	return &SearchMemory{store: store, projectID: projectID}
}

func (t *SearchMemory) Name() string { return "search_memory" }

func (t *SearchMemory) Description() string {
	return "Search saved memories by semantic similarity. Use when starting work that might have prior context, when the user references past sessions, or when encountering something familiar."
}

func (t *SearchMemory) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":        map[string]any{"type": "string", "description": "Natural language search query"},
			"all_projects": map[string]any{"type": "boolean", "description": "Search across all projects (default: current project only)"},
			"limit":        map[string]any{"type": "integer", "description": "Max results to return (default: 3)"},
		},
		"required": []string{"query"},
	}
}

func (t *SearchMemory) Execute(_ context.Context, params map[string]any) (string, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return "error: query is required", nil
	}
	allProjects, _ := params["all_projects"].(bool)
	limit := 3
	if l, ok := params["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	results, err := t.store.SearchMemory(query, t.projectID, allProjects, limit)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(results) == 0 {
		return "No memories found.", nil
	}

	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		fmt.Fprintf(&b, "**%s** (project: %s, saved: %s)\n", r.Title, r.Project, r.CreatedAt)
		fmt.Fprintf(&b, "File: %s\n\n", r.FilePath)
		b.WriteString(r.Content)
	}
	return b.String(), nil
}
