package tool

import (
	"context"
	"encoding/json"
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

type transactionalSnapshotStore interface {
	SnapshotResolvedEntry(turn int, originalPath, canonicalPath string) (entryID string, created bool, err error)
	DiscardSnapshotEntry(turn int, entryID string) error
	RetainSnapshotEntry(turn int, entryID string)
}

type snapshotEntry struct {
	turn    int
	entryID string
	store   transactionalSnapshotStore
}

func snapshotFile(store SnapshotStore, turn int, originalPath, canonicalPath string) error {
	if resolved, ok := store.(resolvedSnapshotStore); ok {
		return resolved.SnapshotResolved(turn, originalPath, canonicalPath)
	}
	return store.Snapshot(turn, canonicalPath)
}

func snapshotFileForMutation(store SnapshotStore, turn int, originalPath, canonicalPath string) (snapshotEntry, error) {
	if transactional, ok := store.(transactionalSnapshotStore); ok {
		entryID, _, err := transactional.SnapshotResolvedEntry(turn, originalPath, canonicalPath)
		if err != nil {
			return snapshotEntry{}, err
		}
		return snapshotEntry{turn: turn, entryID: entryID, store: transactional}, nil
	}
	return snapshotEntry{}, snapshotFile(store, turn, originalPath, canonicalPath)
}

func discardUnmutatedSnapshot(entry snapshotEntry) error {
	if entry.store == nil || entry.entryID == "" {
		return nil
	}
	return entry.store.DiscardSnapshotEntry(entry.turn, entry.entryID)
}

func retainMutatedSnapshot(entry snapshotEntry) {
	if entry.store == nil || entry.entryID == "" {
		return
	}
	entry.store.RetainSnapshotEntry(entry.turn, entry.entryID)
}

// WriteFile implements the write_file tool with mtime enforcement
// and pending support.
type WriteFile struct {
	tracker       *FileTracker
	cfg           config.ToolsConfig
	workspaceRoot string
}

// NewWriteFile creates a WriteFile tool.
func NewWriteFile(tracker *FileTracker, cfg config.ToolsConfig) *WriteFile {
	return &WriteFile{tracker: tracker, cfg: cfg}
}

func NewWriteFileAtRoot(tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string) *WriteFile {
	return &WriteFile{tracker: tracker, cfg: cfg, workspaceRoot: workspaceRoot}
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
	res, err := writeFileExecCommon(params, w.tracker, w.cfg, w.workspaceRoot)
	if err != nil {
		return "", err
	}
	return res.Result, nil
}

func (w *WriteFile) ValidateStaged(_ context.Context, args json.RawMessage) error {
	return validateWriteStagedArgs(args)
}

func (w *WriteFile) StagedResultMessage() string { return "Staged." }

// WriteFileWithSnapshot wraps WriteFile so that every successful write
// is preceded by a call to the snapshot store.
type WriteFileWithSnapshot struct {
	store         SnapshotStore
	tracker       *FileTracker
	cfg           config.ToolsConfig
	workspaceRoot string
}

// NewWriteFileWithSnapshot returns a snapshot-aware write_file tool.
func NewWriteFileWithSnapshot(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig) *WriteFileWithSnapshot {
	return &WriteFileWithSnapshot{store: store, tracker: tracker, cfg: cfg}
}

func NewWriteFileWithSnapshotAtRoot(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string) *WriteFileWithSnapshot {
	return &WriteFileWithSnapshot{store: store, tracker: tracker, cfg: cfg, workspaceRoot: workspaceRoot}
}

func (*WriteFileWithSnapshot) Name() string        { return "write_file" }
func (*WriteFileWithSnapshot) Description() string { return (&WriteFile{}).Description() }
func (*WriteFileWithSnapshot) ParametersSchema() map[string]any {
	return (&WriteFile{}).ParametersSchema()
}

func (*WriteFileWithSnapshot) ValidateStaged(_ context.Context, args json.RawMessage) error {
	return validateWriteStagedArgs(args)
}

func (*WriteFileWithSnapshot) StagedResultMessage() string { return "Staged." }

func (w *WriteFileWithSnapshot) Execute(_ context.Context, params map[string]any) (string, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return "", fmt.Errorf("write_file: path is required")
	}
	displayAbsPath, err := fileDisplayAbsPathAtRoot(w.workspaceRoot, path)
	if err != nil {
		return "", fmt.Errorf("write_file: resolve path: %w", err)
	}
	// re-resolve canonical: detects approved-target swap between snapshot and write (see fileSecurityPath)
	securityPath, err := fileSecurityPathAtRoot(w.workspaceRoot, params, path)
	if err != nil {
		return "", fmt.Errorf("write_file: resolve path: %w", err)
	}
	if err := preflightWriteSnapshotTarget(securityPath, w.tracker); err != nil {
		return "", err
	}
	// The write path revalidates the approved target after snapshotting; if
	// that pre-mutation validation fails, release only this tool's snapshot claim.
	snapshot, err := snapshotFileForMutation(w.store, w.store.CurrentTurn(), displayAbsPath, securityPath)
	if err != nil {
		return "", fmt.Errorf("write_file: snapshot: %w", err)
	}
	res, mutationStarted, err := writeFileExecCommonForSnapshot(params, w.tracker, w.cfg, w.workspaceRoot)
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

func validateWriteStagedArgs(args json.RawMessage) error {
	var params map[string]any
	if err := json.Unmarshal(args, &params); err != nil {
		return fmt.Errorf("write_file: invalid staged arguments: %w", err)
	}
	path, _ := params["path"].(string)
	if path == "" {
		return fmt.Errorf("write_file: path is required")
	}
	if _, ok := params["content"].(string); !ok {
		return fmt.Errorf("write_file: content must be a string")
	}
	return nil
}

// writeFileExecCommon is the shared implementation.
func writeFileExecCommon(params map[string]any, tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string) (*writeResult, error) {
	res, _, err := writeFileExecCommonForSnapshot(params, tracker, cfg, workspaceRoot)
	return res, err
}

func writeFileExecCommonForSnapshot(params map[string]any, tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string) (*writeResult, bool, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return nil, false, fmt.Errorf("write_file: path is required")
	}
	content, _ := params["content"].(string)

	// re-resolve canonical: detects approved-target swap between snapshot and write (see fileSecurityPath)
	absPath, err := fileSecurityPathAtRoot(workspaceRoot, params, path)
	if err != nil {
		return nil, false, fmt.Errorf("write_file: resolve path: %w", err)
	}

	f, openedExisting, mutationStarted, err := openWriteTargetForMutation(absPath, tracker)
	if err != nil {
		return nil, mutationStarted, fmt.Errorf("write_file: %w", err)
	}
	defer f.Close()

	if openedExisting {
		mutationStarted = true
	}
	if err := f.Truncate(0); err != nil {
		return nil, mutationStarted, fmt.Errorf("write_file: truncate: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, mutationStarted, fmt.Errorf("write_file: seek: %w", err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		return nil, mutationStarted, fmt.Errorf("write_file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, mutationStarted, fmt.Errorf("write_file: sync: %w", err)
	}

	// Refresh mtime after successful write without creating read authorization.
	if tracker != nil {
		if info, err := f.Stat(); err == nil {
			tracker.UpdateAfterWriteIdentity(absPath, FileIdentityFromFileInfoAndData(info, []byte(content)))
		}
	}

	return &writeResult{
		Result: fmt.Sprintf("Wrote %s.", path),
	}, mutationStarted, nil
}

func openWriteTargetForMutation(absPath string, tracker *FileTracker) (*os.File, bool, bool, error) {
	existed, err := ensureRegularExistingTarget(absPath)
	if err != nil {
		return nil, false, false, err
	}
	if tracker != nil && tracker.HasRead(absPath) {
		f, err := safefs.OpenExisting(absPath, os.O_RDWR)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, false, false, &FileChangedError{Path: absPath}
			}
			return nil, false, false, err
		}
		if err := validateWriteIdentity(f, absPath, tracker); err != nil {
			_ = f.Close()
			return nil, false, false, err
		}
		return f, true, false, nil
	}

	f, openedExisting, err := safefs.OpenForWrite(absPath, 0o644)
	if err != nil {
		return nil, false, false, err
	}
	if openedExisting {
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, false, false, fmt.Errorf("stat: %w", err)
		}
		if err := ensureRegularFileInfo(absPath, info); err != nil {
			_ = f.Close()
			return nil, false, false, err
		}
		if tracker != nil {
			if err := validateWriteIdentity(f, absPath, tracker); err != nil {
				_ = f.Close()
				return nil, false, false, err
			}
		}
	} else if existed {
		_ = f.Close()
		return nil, false, true, &FileChangedError{Path: absPath}
	}
	return f, openedExisting, !openedExisting, nil
}

func validateWriteIdentity(f *os.File, absPath string, tracker *FileTracker) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if err := ensureRegularFileInfo(absPath, info); err != nil {
		return err
	}
	identity, err := FileIdentityFromOpenFile(f, info)
	if err != nil {
		return fmt.Errorf("read identity: %w", err)
	}
	return tracker.WasReadCheckIdentity(absPath, identity)
}

func preflightWriteSnapshotTarget(absPath string, tracker *FileTracker) error {
	existed, err := ensureRegularExistingTarget(absPath)
	if err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	if !existed {
		if tracker != nil && tracker.HasRead(absPath) {
			return &FileChangedError{Path: absPath}
		}
		return nil
	}

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
		return fmt.Errorf("write_file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("write_file: stat: %w", err)
	}
	if err := ensureRegularFileInfo(absPath, info); err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	if tracker == nil {
		return nil
	}
	return validateWriteIdentity(f, absPath, tracker)
}
