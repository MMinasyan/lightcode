package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/config"
)

// SnapshotStore is the minimum surface WriteFileWithSnapshot and
// EditFileWithSnapshot need from the snapshot store.
type SnapshotStore interface {
	Snapshot(turn int, absPath string) error
	CurrentTurn() int
}

// WriteFile implements the write_file tool with mtime enforcement
// and pending support.
type WriteFile struct {
	tracker *FileTracker
	cfg     config.ToolsConfig
}

// NewWriteFile creates a WriteFile tool.
func NewWriteFile(tracker *FileTracker, cfg config.ToolsConfig) *WriteFile {
	return &WriteFile{tracker: tracker, cfg: cfg}
}

func (w *WriteFile) Name() string { return "write_file" }

func (w *WriteFile) Description() string {
	return `Writes a file to disk.
- Creates parent directories if they don't exist. Overwrites the file if it already exists.
- You must use read_file on the file before overwriting an existing file. This tool will error if the file was not read or was modified since your last read.
- Use this tool for new files or complete rewrites. For targeted changes to existing files, use edit_file instead.
- ALWAYS use pending=true when your task requires multiple edits or writes. Pending calls will be applied AUTOMATICALLY with next tool call or after your response.`
}

func (w *WriteFile) ParametersSchema() map[string]any {
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
			"pending": map[string]any{
				"type":        "boolean",
				"description": "If true, stage this write for batch execution with other pending edits.",
			},
		},
		"required": []string{"path", "content"},
	}
}

type writeResult struct {
	Result string
	Diff   string
}

func (w *WriteFile) Execute(_ context.Context, params map[string]any) (string, error) {
	res, err := writeFileExecCommon(params, w.tracker, w.cfg)
	if err != nil {
		return "", err
	}
	return res.Result, nil
}

// WriteFileWithSnapshot wraps WriteFile so that every successful write
// is preceded by a call to the snapshot store.
type WriteFileWithSnapshot struct {
	store   SnapshotStore
	tracker *FileTracker
	cfg     config.ToolsConfig
}

// NewWriteFileWithSnapshot returns a snapshot-aware write_file tool.
func NewWriteFileWithSnapshot(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig) *WriteFileWithSnapshot {
	return &WriteFileWithSnapshot{store: store, tracker: tracker, cfg: cfg}
}

func (*WriteFileWithSnapshot) Name() string        { return "write_file" }
func (*WriteFileWithSnapshot) Description() string  { return (&WriteFile{}).Description() }
func (*WriteFileWithSnapshot) ParametersSchema() map[string]any {
	return (&WriteFile{}).ParametersSchema()
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
	res, err := writeFileExecCommon(params, w.tracker, w.cfg)
	if err != nil {
		return "", err
	}
	return res.Result, nil
}

// writeFileExecCommon is the shared implementation.
func writeFileExecCommon(params map[string]any, tracker *FileTracker, cfg config.ToolsConfig) (*writeResult, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("write_file: path is required")
	}
	content, _ := params["content"].(string)

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("write_file: resolve path: %w", err)
	}

	// Read existing content for diff (if file exists).
	var prevContent string
	fileExists := false
	if data, err := os.ReadFile(absPath); err == nil {
		prevContent = string(data)
		fileExists = true
	} else if _, err := os.Stat(absPath); err == nil {
		// File exists but couldn't be read (permissions, etc.).
		// Don't block on this — if the file can't be read, write will fail anyway.
	}

	// Mtime enforcement for existing files (overwrite requires read).
	if fileExists && tracker != nil {
		if err := tracker.WasReadCheck(absPath); err != nil {
			return nil, err
		}
	}

	if dir := filepath.Dir(absPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("write_file: mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("write_file: %w", err)
	}

	// Update mtime tracker after successful write.
	if tracker != nil {
		tracker.Track(absPath, 0, 0)
	}

	diff := computeDiff(prevContent, content)

	return &writeResult{
		Result: fmt.Sprintf("Wrote %s.", path),
		Diff:   diff,
	}, nil
}
