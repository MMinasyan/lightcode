package lsp

import (
	"context"
	"testing"
)

func TestClientNoInstancesMessages(t *testing.T) {
	client := NewClient(NewManager(t.TempDir(), t.TempDir()))
	got, err := client.WorkspaceSymbol(context.Background(), "x")
	if err != nil || got != "No language servers available." {
		t.Fatalf("WorkspaceSymbol no instances = %q, %v", got, err)
	}
	got, err = client.GetDiagnostics(context.Background(), []string{"main.go"})
	if err != nil || got != "No language servers available for the modified files." {
		t.Fatalf("GetDiagnostics no instances = %q, %v", got, err)
	}
}
