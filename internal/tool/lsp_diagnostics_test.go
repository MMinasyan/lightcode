package tool

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestLSPDiagnosticsReturnsNoModifiedFiles(t *testing.T) {
	client := &mockDiagnosticsClient{}
	store := &mockDiagStore{currentTurn: 1}
	tool := NewLSPDiagnostics(client, store)

	result, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "No files have been modified." {
		t.Fatalf("Execute result = %q, want no-modified-files message", result)
	}
	if client.calls != 0 {
		t.Fatalf("diagnostics calls = %d, want 0", client.calls)
	}
}

func TestLSPDiagnosticsListTurnsError(t *testing.T) {
	client := &mockDiagnosticsClient{}
	store := &mockDiagStore{listErr: errors.New("store failed")}
	tool := NewLSPDiagnostics(client, store)

	result, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute error = %v, want nil user-facing error", err)
	}
	if result != "error: could not list changed files" {
		t.Fatalf("Execute result = %q, want list-turns error message", result)
	}
	if client.calls != 0 {
		t.Fatalf("diagnostics calls = %d, want 0", client.calls)
	}
}

func TestLSPDiagnosticsCollectsUniquePathsAndDelegates(t *testing.T) {
	client := &mockDiagnosticsClient{result: "diagnostics ok"}
	store := &mockDiagStore{
		currentTurn: 3,
		turns: []DiagTurnEntry{
			{Turn: 1, Files: []DiagFileMeta{{OriginalPath: "a.go"}, {OriginalPath: "b.go"}}},
			{Turn: 2, Files: []DiagFileMeta{{OriginalPath: "a.go"}, {OriginalPath: "c.go"}}},
		},
	}
	tool := NewLSPDiagnostics(client, store)

	result, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "diagnostics ok" {
		t.Fatalf("Execute result = %q, want client result", result)
	}
	wantPaths := []string{"a.go", "b.go", "c.go"}
	if !reflect.DeepEqual(client.paths, wantPaths) {
		t.Fatalf("diagnostic paths = %#v, want %#v", client.paths, wantPaths)
	}
}

func TestLSPDiagnosticsSkipsAlreadyCheckedTurnsAndResetRechecks(t *testing.T) {
	client := &mockDiagnosticsClient{result: "diagnostics ok"}
	store := &mockDiagStore{
		currentTurn: 2,
		turns: []DiagTurnEntry{
			{Turn: 1, Files: []DiagFileMeta{{OriginalPath: "old.go"}}},
			{Turn: 2, Files: []DiagFileMeta{{OriginalPath: "current.go"}}},
		},
	}
	tool := NewLSPDiagnostics(client, store)
	if _, err := tool.Execute(context.Background(), nil); err != nil {
		t.Fatalf("first Execute error = %v", err)
	}

	store.currentTurn = 3
	store.turns = append(store.turns, DiagTurnEntry{Turn: 3, Files: []DiagFileMeta{{OriginalPath: "new.go"}}})
	if _, err := tool.Execute(context.Background(), nil); err != nil {
		t.Fatalf("second Execute error = %v", err)
	}
	if want := []string{"current.go", "new.go"}; !reflect.DeepEqual(client.paths, want) {
		t.Fatalf("second diagnostic paths = %#v, want %#v", client.paths, want)
	}

	tool.Reset()
	if _, err := tool.Execute(context.Background(), nil); err != nil {
		t.Fatalf("after Reset Execute error = %v", err)
	}
	if want := []string{"old.go", "current.go", "new.go"}; !reflect.DeepEqual(client.paths, want) {
		t.Fatalf("after Reset paths = %#v, want %#v", client.paths, want)
	}
}

func TestLSPDiagnosticsPropagatesClientError(t *testing.T) {
	clientErr := errors.New("lsp failed")
	client := &mockDiagnosticsClient{err: clientErr}
	store := &mockDiagStore{currentTurn: 1, turns: []DiagTurnEntry{{Turn: 1, Files: []DiagFileMeta{{OriginalPath: "a.go"}}}}}
	tool := NewLSPDiagnostics(client, store)

	_, err := tool.Execute(context.Background(), nil)
	if !errors.Is(err, clientErr) {
		t.Fatalf("Execute error = %v, want client error", err)
	}
}

type mockDiagStore struct {
	currentTurn int
	turns       []DiagTurnEntry
	listErr     error
}

func (s *mockDiagStore) CurrentTurn() int {
	return s.currentTurn
}

func (s *mockDiagStore) ListTurns() ([]DiagTurnEntry, error) {
	return s.turns, s.listErr
}

type mockDiagnosticsClient struct {
	mu     sync.Mutex
	result string
	err    error
	paths  []string
	calls  int
}

func (c *mockDiagnosticsClient) GetDiagnostics(_ context.Context, paths []string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.paths = append([]string(nil), paths...)
	return c.result, c.err
}
