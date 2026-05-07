package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MMinasyan/lightcode/internal/config"
)

// EditFile implements the edit_file tool with mtime enforcement,
// O(1) results with line ranges, and pending support.
type EditFile struct {
	tracker *FileTracker
	cfg     config.ToolsConfig
}

// NewEditFile creates an EditFile tool.
func NewEditFile(tracker *FileTracker, cfg config.ToolsConfig) *EditFile {
	return &EditFile{tracker: tracker, cfg: cfg}
}

func (e *EditFile) Name() string { return "edit_file" }

func (e *EditFile) Description() string {
	return `Performs exact string replacement in a file.
- You must use read_file on the file before editing. This tool will error if the file was not read or was modified since your last read.
- When using text from read_file output, never include the line number prefix in old_string or new_string. The prefix format is: number + tab. Everything after the tab is the actual file content.
- The edit will FAIL if old_string is not unique in the file. Provide more surrounding context to make it unique, or use replace_all to change every instance.
- old_string and new_string must be different. old_string must not be empty.
- ALWAYS prefer editing existing files. Use write_file only for new files or complete rewrites.
- ALWAYS use pending=true when your task requires multiple edits or writes. Pending calls will be applied AUTOMATICALLY with next tool call or after your response.
- Do not use pending in the last edit or write tool call if you need them applied immediately, OR use execute_pending tool separately after it. ALWAYS prefer non-pending last edit over execute_pending.`
}

func (e *EditFile) ParametersSchema() map[string]any {
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
			"pending": map[string]any{
				"type":        "boolean",
				"description": "If true, stage this edit for batch execution with other pending edits.",
			},
		},
		"required": []string{"path", "old_string", "new_string"},
	}
}

type editResult struct {
	Result     string
	LineRanges string
	Count      int
	Diff       string
}

func (e *EditFile) Execute(_ context.Context, params map[string]any) (string, error) {
	return e.editFileExec(params)
}

// EditFileWithSnapshot wraps EditFile so the pre-edit file content is
// captured by the snapshot store before the edit is applied.
type EditFileWithSnapshot struct {
	store   SnapshotStore
	tracker *FileTracker
	cfg     config.ToolsConfig
}

// NewEditFileWithSnapshot returns a snapshot-aware edit_file tool.
func NewEditFileWithSnapshot(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig) *EditFileWithSnapshot {
	return &EditFileWithSnapshot{store: store, tracker: tracker, cfg: cfg}
}

func (*EditFileWithSnapshot) Name() string        { return "edit_file" }
func (*EditFileWithSnapshot) Description() string { return (&EditFile{}).Description() }
func (*EditFileWithSnapshot) ParametersSchema() map[string]any {
	return (&EditFile{}).ParametersSchema()
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
	res, err := editFileExecCommon(params, e.tracker, e.cfg)
	if err != nil {
		return "", err
	}
	return res.Result, nil
}

func (e *EditFile) editFileExec(params map[string]any) (string, error) {
	res, err := editFileExecCommon(params, e.tracker, e.cfg)
	if err != nil {
		return "", err
	}
	return res.Result, nil
}

// editFileExecCommon is the shared implementation.
func editFileExecCommon(params map[string]any, tracker *FileTracker, cfg config.ToolsConfig) (*editResult, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("edit_file: path is required")
	}
	oldString, _ := params["old_string"].(string)
	newString, _ := params["new_string"].(string)
	replaceAll, _ := params["replace_all"].(bool)

	if oldString == "" {
		return nil, fmt.Errorf("edit_file: old_string must not be empty")
	}
	if oldString == newString {
		return nil, fmt.Errorf("edit_file: old_string and new_string are identical")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("edit_file: resolve path: %w", err)
	}

	// Mtime enforcement.
	if tracker != nil {
		if err := tracker.WasReadCheck(absPath); err != nil {
			return nil, err
		}
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("edit_file: %w", err)
	}
	content := string(data)

	res, err := ApplyEdit(content, oldString, newString, replaceAll, path)
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(absPath, []byte(res.UpdatedContent), 0o644); err != nil {
		return nil, fmt.Errorf("edit_file: write: %w", err)
	}

	// Refresh mtime after successful write without creating read authorization.
	if tracker != nil {
		tracker.UpdateAfterWrite(absPath)
	}

	diff := computeDiff(content, res.UpdatedContent)

	return &editResult{
		Result:     res.Summary,
		LineRanges: res.LineRanges,
		Count:      res.Count,
		Diff:       diff,
	}, nil
}

// EditBufferResult holds the result of an edit applied to a string buffer.
type EditBufferResult struct {
	UpdatedContent string
	Summary        string
	LineRanges     string
	Count          int
}

// ApplyEdit performs a string replacement on content without touching the
// filesystem. Used by the staging system to apply sequential edits to a
// running buffer.
func ApplyEdit(content, oldString, newString string, replaceAll bool, path string) (*EditBufferResult, error) {
	n := strings.Count(content, oldString)
	if n == 0 {
		return nil, fmt.Errorf("edit_file: old_string not found in %s", path)
	}
	if n > 1 && !replaceAll {
		return nil, fmt.Errorf("edit_file: old_string matches %d locations in %s; use replace_all=true to replace all, or provide more context to make it unique", n, path)
	}

	var updated string
	var replCount int
	var lineRanges []string

	if replaceAll {
		updated = strings.ReplaceAll(content, oldString, newString)
		replCount = n
		remaining := content
		shift := 0
		for strings.Contains(remaining, oldString) {
			idx := strings.Index(remaining, oldString)
			absIdx := shift + idx
			startLine := countNewlinesUpTo(content, absIdx) + 1
			endLine := startLine + countNewlinesIn(newString)
			lineRanges = append(lineRanges, fmt.Sprintf("%d-%d", startLine, endLine))
			shift = shift + idx + len(oldString)
			remaining = content[shift:]
		}
	} else {
		idx := strings.Index(content, oldString)
		startLine := countNewlinesUpTo(content, idx) + 1
		endLine := startLine + countNewlinesIn(newString)
		updated = strings.Replace(content, oldString, newString, 1)
		replCount = 1
		lineRanges = append(lineRanges, fmt.Sprintf("%d-%d", startLine, endLine))
	}

	summary := fmt.Sprintf("Edited %s (%d replacement, lines %s).", path, replCount, strings.Join(lineRanges, ", "))
	if replCount > 1 {
		summary = fmt.Sprintf("Edited %s (%d replacements, lines %s).", path, replCount, strings.Join(lineRanges, ", "))
	}

	return &EditBufferResult{
		UpdatedContent: updated,
		Summary:        summary,
		LineRanges:     strings.Join(lineRanges, ", "),
		Count:          replCount,
	}, nil
}

// ApplyWriteResult holds the result of a write applied to a string buffer.
type ApplyWriteResult struct {
	UpdatedContent string
	Summary        string
}

// ApplyWrite returns the result for a write_file applied to a buffer.
func ApplyWrite(content, path string) *ApplyWriteResult {
	return &ApplyWriteResult{
		UpdatedContent: content,
		Summary:        fmt.Sprintf("Wrote %s.", path),
	}
}

func countNewlinesUpTo(s string, idx int) int {
	count := 0
	for i := 0; i < idx && i < len(s); i++ {
		if s[i] == '\n' {
			count++
		}
	}
	return count
}

func countNewlinesIn(s string) int {
	count := 0
	for _, c := range s {
		if c == '\n' {
			count++
		}
	}
	return count
}

func computeDiff(old, new string) string {
	// Simple line-based diff: show removed and added lines.
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(new, "\n")

	var b strings.Builder
	for _, l := range oldLines {
		b.WriteString("- ")
		b.WriteString(l)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	for _, l := range newLines {
		b.WriteString("+ ")
		b.WriteString(l)
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}
