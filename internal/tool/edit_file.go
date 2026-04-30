package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EditFile implements the edit_file tool: exact-string search and replace.
type EditFile struct{}

func (EditFile) Name() string { return "edit_file" }

func (EditFile) Description() string {
	return "Search and replace text within a file. old_string must match exactly. It must be unique in the file unless replace_all is true."
}

func (EditFile) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to edit.",
			},
			"old_string": map[string]any{
				"type":        "string",
				"description": "Exact string to search for. Must match byte-for-byte.",
			},
			"new_string": map[string]any{
				"type":        "string",
				"description": "Replacement string.",
			},
			"replace_all": map[string]any{
				"type":        "boolean",
				"description": "If true, replace every occurrence. If false (default), old_string must be unique in the file.",
			},
		},
		"required": []string{"path", "old_string", "new_string"},
	}
}

func (EditFile) Execute(_ context.Context, params map[string]any) (string, error) {
	return editFileExec(params)
}

// EditFileWithSnapshot wraps EditFile so the pre-edit file content is
// captured by the snapshot store before the edit is applied. First-
// write-wins per turn is enforced by the store itself.
type EditFileWithSnapshot struct {
	store SnapshotStore
}

// NewEditFileWithSnapshot returns a snapshot-aware edit_file tool.
func NewEditFileWithSnapshot(store SnapshotStore) *EditFileWithSnapshot {
	return &EditFileWithSnapshot{store: store}
}

func (*EditFileWithSnapshot) Name() string { return EditFile{}.Name() }
func (*EditFileWithSnapshot) Description() string {
	return EditFile{}.Description()
}
func (*EditFileWithSnapshot) ParametersSchema() map[string]any {
	return EditFile{}.ParametersSchema()
}

func (e *EditFileWithSnapshot) Execute(_ context.Context, params map[string]any) (string, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return "", fmt.Errorf("edit_file: path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("edit_file: resolve path: %w", err)
	}
	if err := e.store.Snapshot(e.store.CurrentTurn(), absPath); err != nil {
		return "", fmt.Errorf("edit_file: snapshot: %w", err)
	}
	return editFileExec(params)
}

// editFileExec is the shared implementation used by both EditFile
// and EditFileWithSnapshot.
func editFileExec(params map[string]any) (string, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return "", fmt.Errorf("edit_file: path is required")
	}
	oldString, _ := params["old_string"].(string)
	newString, _ := params["new_string"].(string)
	replaceAll, _ := params["replace_all"].(bool)

	if oldString == "" {
		return "", fmt.Errorf("edit_file: old_string must not be empty")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}
	content := string(data)

	n := strings.Count(content, oldString)
	if n == 0 {
		return "", fmt.Errorf("edit_file: old_string not found in %s", path)
	}
	if n > 1 && !replaceAll {
		return "", fmt.Errorf("edit_file: old_string matches %d locations in %s; use replace_all=true to replace all, or provide more context to make it unique", n, path)
	}

	var updated string
	if replaceAll {
		updated = strings.ReplaceAll(content, oldString, newString)
	} else {
		updated = strings.Replace(content, oldString, newString, 1)
	}

	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("edit_file: write: %w", err)
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", path, n), nil
}
