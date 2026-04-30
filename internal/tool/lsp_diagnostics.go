package tool

import (
	"context"
	"sync"

	"github.com/MMinasyan/lightcode/internal/lsp"
)

type DiagFileMeta struct {
	OriginalPath string
}

type DiagTurnEntry struct {
	Turn  int
	Files []DiagFileMeta
}

type DiagStore interface {
	CurrentTurn() int
	ListTurns() ([]DiagTurnEntry, error)
}

type LSPDiagnostics struct {
	client *lsp.Client
	store  DiagStore

	mu              sync.Mutex
	lastCheckedTurn int
}

func NewLSPDiagnostics(client *lsp.Client, store DiagStore) *LSPDiagnostics {
	return &LSPDiagnostics{client: client, store: store}
}

func (t *LSPDiagnostics) Name() string { return "diagnostics" }

func (t *LSPDiagnostics) Description() string {
	return "Check for compilation errors in files you have modified. Call after editing to verify correctness. Returns errors only (not warnings)."
}

func (t *LSPDiagnostics) ParametersSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *LSPDiagnostics) Execute(ctx context.Context, params map[string]any) (string, error) {
	turns, err := t.store.ListTurns()
	if err != nil {
		return "error: could not list changed files", nil
	}

	t.mu.Lock()
	threshold := t.lastCheckedTurn
	currentTurn := t.store.CurrentTurn()
	t.lastCheckedTurn = currentTurn
	t.mu.Unlock()

	seen := make(map[string]bool)
	var paths []string
	for _, turn := range turns {
		if turn.Turn < threshold {
			continue
		}
		for _, f := range turn.Files {
			if !seen[f.OriginalPath] {
				seen[f.OriginalPath] = true
				paths = append(paths, f.OriginalPath)
			}
		}
	}

	if len(paths) == 0 {
		return "No files have been modified.", nil
	}

	return t.client.GetDiagnostics(ctx, paths)
}

func (t *LSPDiagnostics) Reset() {
	t.mu.Lock()
	t.lastCheckedTurn = 0
	t.mu.Unlock()
}
