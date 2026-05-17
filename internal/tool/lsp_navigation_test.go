package tool

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestWorkspaceSymbolRequiresQuery(t *testing.T) {
	client := &mockWorkspaceSymbolClient{}
	tool := NewWorkspaceSymbol(client)

	result, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute error = %v, want nil user-facing error", err)
	}
	if result != "error: query is required" {
		t.Fatalf("Execute result = %q, want query-required message", result)
	}
	if client.calls != 0 {
		t.Fatalf("client calls = %d, want 0", client.calls)
	}
}

func TestWorkspaceSymbolDelegatesToClient(t *testing.T) {
	client := &mockWorkspaceSymbolClient{result: "symbol results"}
	tool := NewWorkspaceSymbol(client)

	result, err := tool.Execute(context.Background(), map[string]any{"query": "Handler"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "symbol results" {
		t.Fatalf("Execute result = %q, want client result", result)
	}
	if client.query != "Handler" || client.calls != 1 {
		t.Fatalf("client query=%q calls=%d, want Handler once", client.query, client.calls)
	}
}

func TestWorkspaceSymbolPropagatesClientError(t *testing.T) {
	clientErr := errors.New("lsp unavailable")
	client := &mockWorkspaceSymbolClient{err: clientErr}
	tool := NewWorkspaceSymbol(client)

	_, err := tool.Execute(context.Background(), map[string]any{"query": "Handler"})
	if !errors.Is(err, clientErr) {
		t.Fatalf("Execute error = %v, want client error", err)
	}
}

func TestWorkspaceSymbolMetadataAndSchema(t *testing.T) {
	tool := NewWorkspaceSymbol(&mockWorkspaceSymbolClient{})
	if tool.Name() != "workspace_symbol" {
		t.Fatalf("Name = %q, want workspace_symbol", tool.Name())
	}
	schema := tool.ParametersSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v, want map", schema["properties"])
	}
	if _, ok := props["query"]; !ok {
		t.Fatalf("schema properties = %#v, want query property", props)
	}
	if required, ok := schema["required"].([]string); !ok || !reflect.DeepEqual(required, []string{"query"}) {
		t.Fatalf("schema required = %#v, want [query]", schema["required"])
	}
}

type mockWorkspaceSymbolClient struct {
	result string
	err    error
	query  string
	calls  int
}

func (c *mockWorkspaceSymbolClient) WorkspaceSymbol(_ context.Context, query string) (string, error) {
	c.query = query
	c.calls++
	return c.result, c.err
}
