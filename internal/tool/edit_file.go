package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/editpreview"
	"github.com/MMinasyan/lightcode/internal/safefs"
)

// EditFile implements the edit_file tool with mtime enforcement,
// O(1) results with line ranges, and pending support.
type EditFile struct {
	tracker       *FileTracker
	cfg           config.ToolsConfig
	workspaceRoot string
}

// NewEditFile creates an EditFile tool.
func NewEditFile(tracker *FileTracker, cfg config.ToolsConfig) *EditFile {
	return &EditFile{tracker: tracker, cfg: cfg}
}

func NewEditFileAtRoot(tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string) *EditFile {
	return &EditFile{tracker: tracker, cfg: cfg, workspaceRoot: workspaceRoot}
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
}

func (e *EditFile) Execute(_ context.Context, params map[string]any) (string, error) {
	return e.editFileExec(params)
}

func (e *EditFile) ValidateStaged(_ context.Context, args json.RawMessage) error {
	return validateEditStagedArgs(args)
}

func (e *EditFile) StagedResultMessage() string { return "Staged." }

func (e *EditFile) DisplayMetadata(_ context.Context, args json.RawMessage, result string) map[string]any {
	return editMetadataFromArgs(args, result)
}

// EditFileWithSnapshot wraps EditFile so the pre-edit file content is
// captured by the snapshot store before the edit is applied.
type EditFileWithSnapshot struct {
	store         SnapshotStore
	tracker       *FileTracker
	cfg           config.ToolsConfig
	workspaceRoot string
}

// NewEditFileWithSnapshot returns a snapshot-aware edit_file tool.
func NewEditFileWithSnapshot(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig) *EditFileWithSnapshot {
	return &EditFileWithSnapshot{store: store, tracker: tracker, cfg: cfg}
}

func NewEditFileWithSnapshotAtRoot(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string) *EditFileWithSnapshot {
	return &EditFileWithSnapshot{store: store, tracker: tracker, cfg: cfg, workspaceRoot: workspaceRoot}
}

func (*EditFileWithSnapshot) Name() string        { return "edit_file" }
func (*EditFileWithSnapshot) Description() string { return (&EditFile{}).Description() }
func (*EditFileWithSnapshot) ParametersSchema() map[string]any {
	return (&EditFile{}).ParametersSchema()
}

func (*EditFileWithSnapshot) ValidateStaged(_ context.Context, args json.RawMessage) error {
	return validateEditStagedArgs(args)
}

func (*EditFileWithSnapshot) StagedResultMessage() string { return "Staged." }

func (*EditFileWithSnapshot) DisplayMetadata(_ context.Context, args json.RawMessage, result string) map[string]any {
	return editMetadataFromArgs(args, result)
}

func (e *EditFileWithSnapshot) Execute(_ context.Context, params map[string]any) (string, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return "", fmt.Errorf("edit_file: path is required")
	}
	displayAbsPath, err := fileDisplayAbsPathAtRoot(e.workspaceRoot, path)
	if err != nil {
		return "", fmt.Errorf("edit_file: resolve path: %w", err)
	}
	// re-resolve canonical: detects approved-target swap between snapshot and write (see fileSecurityPath)
	securityPath, err := fileSecurityPathAtRoot(e.workspaceRoot, params, path)
	if err != nil {
		return "", fmt.Errorf("edit_file: resolve path: %w", err)
	}
	if err := preflightEditSnapshotTarget(securityPath, e.tracker); err != nil {
		return "", err
	}
	// The edit path revalidates and applies the textual match after snapshotting;
	// if that fails before truncation, release only this tool's snapshot claim.
	snapshot, err := snapshotFileForMutation(e.store, e.store.CurrentTurn(), displayAbsPath, securityPath)
	if err != nil {
		return "", fmt.Errorf("edit_file: snapshot: %w", err)
	}
	res, mutationStarted, err := editFileExecCommonForSnapshot(params, e.tracker, e.cfg, e.workspaceRoot)
	if err != nil {
		if !mutationStarted {
			if discardErr := discardUnmutatedSnapshot(snapshot); discardErr != nil {
				return "", fmt.Errorf("%w; additionally failed to discard snapshot: %v", err, discardErr)
			}
		} else {
			retainMutatedSnapshot(snapshot)
		}
		return "", err
	}
	retainMutatedSnapshot(snapshot)
	return res.Result, nil
}

func validateEditStagedArgs(args json.RawMessage) error {
	var params map[string]any
	if err := json.Unmarshal(args, &params); err != nil {
		return fmt.Errorf("edit_file: invalid staged arguments: %w", err)
	}
	path, _ := params["path"].(string)
	if path == "" {
		return fmt.Errorf("edit_file: path is required")
	}
	oldStr, _ := params["old_string"].(string)
	newStr, _ := params["new_string"].(string)
	if oldStr == "" {
		return fmt.Errorf("edit_file: old_string must not be empty")
	}
	if oldStr == newStr {
		return fmt.Errorf("edit_file: old_string and new_string are identical")
	}
	return nil
}

func editMetadataFromArgs(args json.RawMessage, result string) map[string]any {
	if !strings.Contains(result, "lines ") {
		return nil
	}
	return editpreview.MetadataFromArgs(string(args), result)
}

func (e *EditFile) editFileExec(params map[string]any) (string, error) {
	res, err := editFileExecCommon(params, e.tracker, e.cfg, e.workspaceRoot)
	if err != nil {
		return "", err
	}
	return res.Result, nil
}

func preflightEditSnapshotTarget(absPath string, tracker *FileTracker) error {
	if _, err := ensureRegularExistingTarget(absPath); err != nil {
		return fmt.Errorf("edit_file: %w", err)
	}
	f, err := safefs.OpenExisting(absPath, os.O_RDWR)
	if err != nil {
		return fmt.Errorf("edit_file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("edit_file: stat: %w", err)
	}
	if err := ensureRegularFileInfo(absPath, info); err != nil {
		return fmt.Errorf("edit_file: %w", err)
	}
	if tracker == nil {
		return nil
	}
	identity, err := FileIdentityFromOpenFile(f, info)
	if err != nil {
		return fmt.Errorf("edit_file: read identity: %w", err)
	}
	return tracker.WasReadCheckIdentity(absPath, identity)
}

// editFileExecCommon is the shared implementation.
func editFileExecCommon(params map[string]any, tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string) (*editResult, error) {
	res, _, err := editFileExecCommonForSnapshot(params, tracker, cfg, workspaceRoot)
	return res, err
}

func editFileExecCommonForSnapshot(params map[string]any, tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string) (*editResult, bool, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return nil, false, fmt.Errorf("edit_file: path is required")
	}
	oldString, _ := params["old_string"].(string)
	newString, _ := params["new_string"].(string)
	replaceAll, _ := params["replace_all"].(bool)

	if oldString == "" {
		return nil, false, fmt.Errorf("edit_file: old_string must not be empty")
	}
	if oldString == newString {
		return nil, false, fmt.Errorf("edit_file: old_string and new_string are identical")
	}

	// re-resolve canonical: detects approved-target swap between snapshot and write (see fileSecurityPath)
	absPath, err := fileSecurityPathAtRoot(workspaceRoot, params, path)
	if err != nil {
		return nil, false, fmt.Errorf("edit_file: resolve path: %w", err)
	}

	if _, err := ensureRegularExistingTarget(absPath); err != nil {
		return nil, false, fmt.Errorf("edit_file: %w", err)
	}

	// Mtime enforcement.
	f, err := safefs.OpenExisting(absPath, os.O_RDWR)
	if err != nil {
		return nil, false, fmt.Errorf("edit_file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("edit_file: stat: %w", err)
	}
	if err := ensureRegularFileInfo(absPath, info); err != nil {
		return nil, false, fmt.Errorf("edit_file: %w", err)
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false, fmt.Errorf("edit_file: %w", err)
	}
	if tracker != nil {
		if err := tracker.WasReadCheckIdentity(absPath, FileIdentityFromFileInfoAndData(info, data)); err != nil {
			return nil, false, err
		}
	}
	content := string(data)

	res, err := ApplyEdit(content, oldString, newString, replaceAll, path)
	if err != nil {
		return nil, false, err
	}

	mutationStarted := true
	if err := f.Truncate(0); err != nil {
		return nil, mutationStarted, fmt.Errorf("edit_file: truncate: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, mutationStarted, fmt.Errorf("edit_file: seek: %w", err)
	}
	if _, err := f.Write([]byte(res.UpdatedContent)); err != nil {
		return nil, mutationStarted, fmt.Errorf("edit_file: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, mutationStarted, fmt.Errorf("edit_file: sync: %w", err)
	}

	// Refresh mtime after successful write without creating read authorization.
	if tracker != nil {
		if info, err := f.Stat(); err == nil {
			tracker.UpdateAfterWriteIdentity(absPath, FileIdentityFromFileInfoAndData(info, []byte(res.UpdatedContent)))
		}
	}

	return &editResult{
		Result:     res.Summary,
		LineRanges: res.LineRanges,
		Count:      res.Count,
	}, mutationStarted, nil
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
