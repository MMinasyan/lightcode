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

type transactionalSnapshotStore interface {
	SnapshotResolvedEntry(turn int, originalPath, canonicalPath string) (entryID string, created bool, err error)
	DiscardSnapshotEntry(turn int, entryID string) error
	RetainSnapshotEntry(turn int, entryID string)
}

type snapshotIdentityRecorder interface {
	RecordSnapshotContent(turn int, entryID string, content []byte) error
	RecordSnapshotAbsence(turn int, entryID string) error
}

type snapshotMutationLocker interface {
	LockSnapshotMutation(turn int, entryID string) (func(), error)
}

type mutationFile interface {
	io.Reader
	io.Seeker
	io.Writer
	Close() error
	Stat() (os.FileInfo, error)
	Sync() error
	Truncate(size int64) error
}

var (
	openWriteTargetForMutationFunc = openWriteTargetForMutationReal
	openExistingMutationFile       = func(path string, flag int) (mutationFile, error) {
		return safefs.OpenExisting(path, flag)
	}
)

type snapshotEntry struct {
	turn    int
	entryID string
	store   transactionalSnapshotStore
	release func()
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
		entry := snapshotEntry{turn: turn, entryID: entryID, store: transactional}
		if locker, ok := store.(snapshotMutationLocker); ok {
			release, err := locker.LockSnapshotMutation(turn, entryID)
			if err != nil {
				if discardErr := transactional.DiscardSnapshotEntry(turn, entryID); discardErr != nil {
					return snapshotEntry{}, fmt.Errorf("%w; additionally failed to discard snapshot: %v", err, discardErr)
				}
				return snapshotEntry{}, err
			}
			entry.release = release
		}
		return entry, nil
	}
	return snapshotEntry{}, snapshotFile(store, turn, originalPath, canonicalPath)
}

func releaseSnapshotMutation(entry snapshotEntry) {
	if entry.release != nil {
		entry.release()
	}
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

func recordMutatedSnapshotContent(entry snapshotEntry, content []byte) error {
	if entry.store == nil || entry.entryID == "" {
		return nil
	}
	recorder, ok := entry.store.(snapshotIdentityRecorder)
	if !ok {
		return nil
	}
	return recorder.RecordSnapshotContent(entry.turn, entry.entryID, content)
}

func recordMutatedSnapshotAbsence(entry snapshotEntry) error {
	if entry.store == nil || entry.entryID == "" {
		return nil
	}
	recorder, ok := entry.store.(snapshotIdentityRecorder)
	if !ok {
		return nil
	}
	return recorder.RecordSnapshotAbsence(entry.turn, entry.entryID)
}

func recordCurrentSnapshotContent(entry snapshotEntry, path string) error {
	if entry.store == nil || entry.entryID == "" {
		return nil
	}
	f, err := safefs.OpenExisting(path, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	content, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	return recordMutatedSnapshotContent(entry, content)
}

func retainFailedMutatedSnapshot(entry snapshotEntry, path string, err error) error {
	if recordErr := recordCurrentSnapshotContent(entry, path); recordErr != nil {
		retainMutatedSnapshot(entry)
		return fmt.Errorf("%w; additionally failed to record snapshot identity: %v", err, recordErr)
	}
	retainMutatedSnapshot(entry)
	return err
}

// WriteFile implements the write_file tool.
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

func (w *WriteFile) SetToolsConfig(cfg config.ToolsConfig) { w.cfg = cfg }

func (w *WriteFile) Name() string { return "write_file" }

func (w *WriteFile) Description() string {
	return `Writes a file to disk.
- Creates parent directories if they don't exist. Overwrites the file if it already exists.
- Use this tool for new files or complete rewrites. For targeted changes to existing files, use edit_file instead. Writing to a path that already exists overwrites it entirely, so when creating a new file, use a path that does not already exist.`
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

func (w *WriteFileWithSnapshot) SetToolsConfig(cfg config.ToolsConfig) { w.cfg = cfg }

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
	defer releaseSnapshotMutation(snapshot)
	res, mutationStarted, err := writeFileExecCommonForSnapshot(params, w.tracker, w.cfg, w.workspaceRoot)
	if err != nil {
		if !mutationStarted {
			if discardErr := discardUnmutatedSnapshot(snapshot); discardErr != nil {
				return "", fmt.Errorf("%w; additionally failed to discard snapshot: %v", err, discardErr)
			}
		} else {
			err = retainFailedMutatedSnapshot(snapshot, securityPath, err)
		}
		return "", err
	}
	content, _ := params["content"].(string)
	if err := recordMutatedSnapshotContent(snapshot, []byte(content)); err != nil {
		retainMutatedSnapshot(snapshot)
		return "", fmt.Errorf("write_file: record snapshot identity: %w", err)
	}
	retainMutatedSnapshot(snapshot)
	return res.Result, nil
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

	return &writeResult{
		Result: fmt.Sprintf("Wrote %s.", path),
	}, mutationStarted, nil
}

func openWriteTargetForMutation(absPath string, tracker *FileTracker) (mutationFile, bool, bool, error) {
	return openWriteTargetForMutationFunc(absPath, tracker)
}

func openWriteTargetForMutationReal(absPath string, _ *FileTracker) (mutationFile, bool, bool, error) {
	_, err := ensureRegularExistingTarget(absPath)
	if err != nil {
		return nil, false, false, err
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
	}
	return f, openedExisting, !openedExisting, nil
}

func preflightWriteSnapshotTarget(absPath string, _ *FileTracker) error {
	existed, err := ensureRegularExistingTarget(absPath)
	if err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	if !existed {
		return nil
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
	return nil
}
