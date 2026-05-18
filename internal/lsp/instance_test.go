package lsp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/lsp/protocol"
	"github.com/MMinasyan/lightcode/internal/lsp/server"
)

func TestInstanceDiagnosticsAndReadyNotification(t *testing.T) {
	inst := newInstance(&server.Definition{Name: "fake"}, t.TempDir(), t.TempDir(), nil)
	uri := protocol.URIFromPath("/tmp/main.go")
	severity := protocol.SeverityError
	params, err := json.Marshal(protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: []protocol.Diagnostic{{Severity: &severity, Message: "bad", Range: protocol.Range{Start: protocol.Position{Line: 3}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	inst.handleNotification("textDocument/publishDiagnostics", params)
	diags := inst.fileDiagnostics(uri)
	if len(diags) != 1 || diags[0].Message != "bad" || diags[0].Range.Start.Line != 3 {
		t.Fatalf("diagnostics = %+v", diags)
	}

	progress, err := json.Marshal(protocol.ProgressParams{Value: json.RawMessage(`{"kind":"end"}`)})
	if err != nil {
		t.Fatal(err)
	}
	inst.handleNotification("$/progress", progress)
	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady after progress end: %v", err)
	}
}

func TestInstanceWaitReadyCanceledWhenStarting(t *testing.T) {
	inst := newInstance(&server.Definition{Name: "fake"}, t.TempDir(), t.TempDir(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := inst.waitReady(ctx); err == nil {
		t.Fatal("waitReady canceled error = nil, want error")
	}
}
