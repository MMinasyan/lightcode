package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// SnapshotStore is the minimum surface WriteFileWithSnapshot and
// EditFileWithSnapshot need from the snapshot store. Declaring it
// here (rather than importing internal/snapshot) keeps the tool
// package free of any dependency on the snapshot package — main.go
// wires the concrete *snapshot.Store in at construction time and
// it satisfies this interface via duck typing.
type SnapshotStore interface {
	Snapshot(turn int, absPath string) error
	CurrentTurn() int
}

// WriteFile implements the write_file tool.
type WriteFile struct{}

func (WriteFile) Name() string { return "write_file" }

func (WriteFile) Description() string {
	return "Write content to a file, creating it (and parent directories) if needed, or overwriting it entirely if it exists."
}

func (WriteFile) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute or relative path to the file to write.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Full content to write to the file.",
			},
		},
		"required": []string{"path", "content"},
	}
}

func (WriteFile) Execute(_ context.Context, params map[string]any) (string, error) {
	return writeFileExec(params)
}

// WriteFileWithSnapshot wraps WriteFile so that every successful write
// is preceded by a call to the snapshot store capturing the pre-write
// state. First-write-wins per turn is handled inside the store itself,
// so repeat snapshots of the same file in one turn are cheap.
type WriteFileWithSnapshot struct {
	store SnapshotStore
}

// NewWriteFileWithSnapshot returns a snapshot-aware write_file tool.
func NewWriteFileWithSnapshot(store SnapshotStore) *WriteFileWithSnapshot {
	return &WriteFileWithSnapshot{store: store}
}

func (*WriteFileWithSnapshot) Name() string { return WriteFile{}.Name() }
func (*WriteFileWithSnapshot) Description() string {
	return WriteFile{}.Description()
}
func (*WriteFileWithSnapshot) ParametersSchema() map[string]any {
	return WriteFile{}.ParametersSchema()
}

func (w *WriteFileWithSnapshot) Execute(_ context.Context, params map[string]any) (string, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return "", fmt.Errorf("write_file: path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("write_file: resolve path: %w", err)
	}
	if err := w.store.Snapshot(w.store.CurrentTurn(), absPath); err != nil {
		return "", fmt.Errorf("write_file: snapshot: %w", err)
	}
	return writeFileExec(params)
}

// writeFileExec is the shared implementation used by both WriteFile
// and WriteFileWithSnapshot. Keeps the path/content handling in one
// place so the two tools can never drift.
func writeFileExec(params map[string]any) (string, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return "", fmt.Errorf("write_file: path is required")
	}
	content, _ := params["content"].(string)
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("write_file: mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
}
