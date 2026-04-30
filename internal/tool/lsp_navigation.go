package tool

import (
	"context"

	"github.com/MMinasyan/lightcode/internal/lsp"
)

type WorkspaceSymbol struct{ client *lsp.Client }

func NewWorkspaceSymbol(client *lsp.Client) *WorkspaceSymbol {
	return &WorkspaceSymbol{client: client}
}

func (t *WorkspaceSymbol) Name() string { return "workspace_symbol" }

func (t *WorkspaceSymbol) Description() string {
	return "Search for symbols by name across the project. Returns matching functions, types, variables, etc. with their file locations."
}

func (t *WorkspaceSymbol) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Symbol name or partial name to search for."},
		},
		"required": []string{"query"},
	}
}

func (t *WorkspaceSymbol) Execute(ctx context.Context, params map[string]any) (string, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return "error: query is required", nil
	}
	return t.client.WorkspaceSymbol(ctx, query)
}
