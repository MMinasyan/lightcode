package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/memory"
)

type SearchHistory struct {
	store     *memory.Store
	projectID string
}

func NewSearchHistory(store *memory.Store, projectID string) *SearchHistory {
	return &SearchHistory{store: store, projectID: projectID}
}

func (t *SearchHistory) Name() string { return "search_history" }

func (t *SearchHistory) Description() string {
	return "Search past session summaries by semantic similarity. Use when you need context from previous sessions."
}

func (t *SearchHistory) ParametersSchema() map[string]any {
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

func (t *SearchHistory) Execute(_ context.Context, params map[string]any) (string, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return "error: query is required", nil
	}
	allProjects, _ := params["all_projects"].(bool)
	limit := 3
	if l, ok := params["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	results, err := t.store.SearchHistory(query, t.projectID, allProjects, limit)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(results) == 0 {
		return "No matching session history found.", nil
	}

	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		fmt.Fprintf(&b, "Session: %s (project: %s, compacted: %s)\n", r.SessionID, r.Project, r.CreatedAt)
		fmt.Fprintf(&b, "Full summary: %s\n\n", r.CompactionPath)
		b.WriteString(r.SectionContent)
	}
	return b.String(), nil
}
