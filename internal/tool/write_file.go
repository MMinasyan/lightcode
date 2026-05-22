package tool

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/safefs"
)

// SnapshotStore is the minimum surface WriteFileWithSnapshot and
// EditFileWithSnapshot need from the snapshot store.
type SnapshotStore interface {
	Snapshot(turn int, absPath string) error
	CurrentTurn() int
}

type resolvedSnapshotStore interface {
	SnapshotResolved(turn int, originalPath, canonicalPath string) error
}

func snapshotFile(store SnapshotStore, turn int, originalPath, canonicalPath string) error {
	if resolved, ok := store.(resolvedSnapshotStore); ok {
		return resolved.SnapshotResolved(turn, originalPath, canonicalPath)
	}
	return store.Snapshot(turn, canonicalPath)
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
func (*WriteFileWithSnapshot) Description() string { return (&WriteFile{}).Description() }
func (*WriteFileWithSnapshot) ParametersSchema() map[string]any {
	return (&WriteFile{}).ParametersSchema()
}

func (w *WriteFileWithSnapshot) Execute(_ context.Context, params map[string]any) (string, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return "", fmt.Errorf("write_file: path is required")
	}
	displayAbsPath, err := fileDisplayAbsPath(path)
	if err != nil {
		return "", fmt.Errorf("write_file: resolve path: %w", err)
	}
	securityPath, err := fileSecurityPath(params, path)
	if err != nil {
		return "", fmt.Errorf("write_file: resolve path: %w", err)
	}
	if err := preflightWriteSnapshotTarget(securityPath, w.tracker); err != nil {
		return "", err
	}
	if err := snapshotFile(w.store, w.store.CurrentTurn(), displayAbsPath, securityPath); err != nil {
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

	absPath, err := fileSecurityPath(params, path)
	if err != nil {
		return nil, fmt.Errorf("write_file: resolve path: %w", err)
	}

	f, _, err := openWriteTarget(absPath, tracker)
	if err != nil {
		return nil, fmt.Errorf("write_file: %w", err)
	}
	defer f.Close()

	if err := f.Truncate(0); err != nil {
		return nil, fmt.Errorf("write_file: truncate: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("write_file: seek: %w", err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		return nil, fmt.Errorf("write_file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("write_file: sync: %w", err)
	}

	// Refresh mtime after successful write without creating read authorization.
	if tracker != nil {
		if info, err := f.Stat(); err == nil {
			tracker.UpdateAfterWriteIdentity(absPath, FileIdentityFromFileInfoAndData(info, []byte(content)))
		}
	}

	return &writeResult{
		Result: fmt.Sprintf("Wrote %s.", path),
	}, nil
}

func openWriteTarget(absPath string, tracker *FileTracker) (*os.File, bool, error) {
	if tracker != nil && tracker.HasRead(absPath) {
		f, err := safefs.OpenExisting(absPath, os.O_RDWR)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, false, &FileChangedError{Path: absPath}
			}
			return nil, false, err
		}
		if err := validateWriteIdentity(f, absPath, tracker); err != nil {
			_ = f.Close()
			return nil, false, err
		}
		return f, true, nil
	}

	f, existed, err := safefs.OpenForWrite(absPath, 0o644)
	if err != nil {
		return nil, false, err
	}
	if existed && tracker != nil {
		if err := validateWriteIdentity(f, absPath, tracker); err != nil {
			_ = f.Close()
			return nil, false, err
		}
	}
	return f, existed, nil
}

func validateWriteIdentity(f *os.File, absPath string, tracker *FileTracker) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	identity, err := FileIdentityFromOpenFile(f, info)
	if err != nil {
		return fmt.Errorf("read identity: %w", err)
	}
	return tracker.WasReadCheckIdentity(absPath, identity)
}

func preflightWriteSnapshotTarget(absPath string, tracker *FileTracker) error {
	if tracker != nil && tracker.HasRead(absPath) {
		f, err := safefs.OpenExisting(absPath, os.O_RDWR)
		if err != nil {
			if os.IsNotExist(err) {
				return &FileChangedError{Path: absPath}
			}
			return fmt.Errorf("write_file: %w", err)
		}
		defer f.Close()
		return validateWriteIdentity(f, absPath, tracker)
	}

	f, err := safefs.OpenExisting(absPath, os.O_RDWR)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("write_file: %w", err)
	}
	defer f.Close()
	if tracker == nil {
		return nil
	}
	return validateWriteIdentity(f, absPath, tracker)
}
