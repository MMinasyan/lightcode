package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestDirectWritesAreBoundToReadFileIdentity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		run         func(t *testing.T, path string, tracker *FileTracker) error
		wantAbsent  bool
		wantContent string
	}{
		{
			name:       "write deleted after read",
			wantAbsent: true,
			run: func(t *testing.T, path string, tracker *FileTracker) error {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				_, err := NewWriteFile(tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name:        "write recreated with same mtime",
			wantContent: "external",
			run: func(t *testing.T, path string, tracker *FileTracker) error {
				recreateWithSameMtimeForIdentity(t, path, "external")
				_, err := NewWriteFile(tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name:        "edit recreated with same mtime",
			wantContent: "external",
			run: func(t *testing.T, path string, tracker *FileTracker) error {
				recreateWithSameMtimeForIdentity(t, path, "external")
				_, err := NewEditFile(tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
					"path":       path,
					"old_string": "external",
					"new_string": "after",
				})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := readIdentityTrackedFile(t, "before")
			tracker := NewFileTracker()
			if _, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker).Execute(context.Background(), map[string]any{"path": path}); err != nil {
				t.Fatal(err)
			}

			err := tc.run(t, path, tracker)
			var changedErr *FileChangedError
			if !errors.As(err, &changedErr) {
				t.Fatalf("operation error = %T %v, want *FileChangedError", err, err)
			}
			if tc.wantAbsent {
				assertFileAbsentAfterStaleRefusal(t, path)
			} else {
				assertFileContent(t, path, tc.wantContent)
			}
		})
	}
}

func TestReadFileTracksKernelIdentity(t *testing.T) {
	path := readIdentityTrackedFile(t, "content")
	tracker := NewFileTracker()

	if _, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker).Execute(context.Background(), map[string]any{"path": path}); err != nil {
		t.Fatal(err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	identity, ok := tracker.identities[path]
	if !ok {
		t.Fatalf("tracker has no read identity for %s", path)
	}
	if !identity.Valid || identity.Dev == 0 || identity.Ino == 0 || identity.Mtime.IsZero() || identity.Ctime.IsZero() || !identity.Mode.IsRegular() {
		t.Fatalf("tracked identity = %+v, want valid regular kernel identity", identity)
	}
}

func TestDeleteRecreateChangesTrackedIdentity(t *testing.T) {
	path := readIdentityTrackedFile(t, "before")
	tracker := NewFileTracker()
	if _, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker).Execute(context.Background(), map[string]any{"path": path}); err != nil {
		t.Fatal(err)
	}

	tracker.mu.Lock()
	before := tracker.identities[path]
	tracker.mu.Unlock()
	recreateWithSameMtimeForIdentity(t, path, "external")
	after := fileIdentityForPath(t, path)

	if identityMatches(before, after) {
		t.Fatalf("identity matched after delete/recreate with same mtime: before=%+v after=%+v", before, after)
	}
}

func TestStagedWritesAreBoundToReadFileIdentity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		run         func(t *testing.T, path string, tracker *FileTracker) []BatchResult
		wantAbsent  bool
		wantContent string
	}{
		{
			name:       "write deleted after read",
			wantAbsent: true,
			run: func(t *testing.T, path string, tracker *FileTracker) []BatchResult {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				return NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil).ExecutePending(context.Background(), []StagedCall{
					stagedWrite(path, "call-1", "after"),
				})
			},
		},
		{
			name:        "write recreated with same mtime",
			wantContent: "external",
			run: func(t *testing.T, path string, tracker *FileTracker) []BatchResult {
				recreateWithSameMtimeForIdentity(t, path, "external")
				return NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil).ExecutePending(context.Background(), []StagedCall{
					stagedWrite(path, "call-1", "after"),
				})
			},
		},
		{
			name:        "edit recreated with same mtime",
			wantContent: "external",
			run: func(t *testing.T, path string, tracker *FileTracker) []BatchResult {
				recreateWithSameMtimeForIdentity(t, path, "external")
				return NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil).ExecutePending(context.Background(), []StagedCall{
					stagedEdit(path, "call-1", "external", "after", false),
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := readIdentityTrackedFile(t, "before")
			tracker := NewFileTracker()
			if _, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker).Execute(context.Background(), map[string]any{"path": path}); err != nil {
				t.Fatal(err)
			}

			results := tc.run(t, path, tracker)
			assertSingleStaleReadBatch(t, results)
			if tc.wantAbsent {
				assertFileAbsentAfterStaleRefusal(t, path)
			} else {
				assertFileContent(t, path, tc.wantContent)
			}
		})
	}
}

func TestSnapshotWritesRevalidateReadIdentityBeforeFinalWrite(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mutate      func(t *testing.T, path string)
		wantAbsent  bool
		wantContent string
	}{
		{
			name:       "deleted after snapshot",
			wantAbsent: true,
			mutate: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:        "recreated with same mtime after snapshot",
			wantContent: "external",
			mutate: func(t *testing.T, path string) {
				recreateWithSameMtimeForIdentity(t, path, "external")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := readIdentityTrackedFile(t, "before")
			tracker := NewFileTracker()
			if _, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker).Execute(context.Background(), map[string]any{"path": path}); err != nil {
				t.Fatal(err)
			}
			store := &mutatingSnapshotStoreForIdentity{turn: 1, path: path, mutate: func(path string) {
				tc.mutate(t, path)
			}}

			_, err := NewWriteFileWithSnapshot(store, tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})

			var changedErr *FileChangedError
			if !errors.As(err, &changedErr) {
				t.Fatalf("write_file error = %T %v, want *FileChangedError", err, err)
			}
			if tc.wantAbsent {
				assertFileAbsentAfterStaleRefusal(t, path)
			} else {
				assertFileContent(t, path, tc.wantContent)
			}
		})
	}
}

func TestStaleSnapshotAwareFileToolsDoNotSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(path string, tracker *FileTracker, store *recordingSnapshotStoreForIdentity) error
	}{
		{
			name: "write deleted after read",
			run: func(path string, tracker *FileTracker, store *recordingSnapshotStoreForIdentity) error {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				_, err := NewWriteFileWithSnapshot(store, tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
					"path":    path,
					"content": "after",
				})
				return err
			},
		},
		{
			name: "write recreated with same mtime",
			run: func(path string, tracker *FileTracker, store *recordingSnapshotStoreForIdentity) error {
				recreateWithSameMtimeForIdentity(t, path, "external")
				_, err := NewWriteFileWithSnapshot(store, tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
					"path":    path,
					"content": "after",
				})
				return err
			},
		},
		{
			name: "edit recreated with same mtime",
			run: func(path string, tracker *FileTracker, store *recordingSnapshotStoreForIdentity) error {
				recreateWithSameMtimeForIdentity(t, path, "external")
				_, err := NewEditFileWithSnapshot(store, tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
					"path":       path,
					"old_string": "external",
					"new_string": "after",
				})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := readIdentityTrackedFile(t, "before")
			tracker := NewFileTracker()
			if _, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker).Execute(context.Background(), map[string]any{"path": path}); err != nil {
				t.Fatal(err)
			}
			store := &recordingSnapshotStoreForIdentity{turn: 1}

			var changedErr *FileChangedError
			if err := tc.run(path, tracker, store); !errors.As(err, &changedErr) {
				t.Fatalf("operation error = %T %v, want *FileChangedError", err, err)
			}
			if store.calls != 0 {
				t.Fatalf("snapshot calls = %d, want none before stale-read refusal", store.calls)
			}
		})
	}
}

func TestStagedSnapshotWritesRevalidateReadIdentityBeforeFinalWrite(t *testing.T) {
	path := readIdentityTrackedFile(t, "before")
	tracker := NewFileTracker()
	if _, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, tracker).Execute(context.Background(), map[string]any{"path": path}); err != nil {
		t.Fatal(err)
	}
	store := &mutatingSnapshotStoreForIdentity{turn: 1, path: path, mutate: func(path string) {
		recreateWithSameMtimeForIdentity(t, path, "external")
	}}

	results := NewStagedExecutor(store, tracker, config.ToolsConfig{}, allowStagedCall, nil).ExecutePending(context.Background(), []StagedCall{
		stagedWrite(path, "call-1", "after"),
	})

	assertSingleStaleReadBatch(t, results)
	assertFileContent(t, path, "external")
}

func TestHistoricalReadsWithoutIdentityForceReread(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(path string, tracker *FileTracker) error
	}{
		{
			name: "write",
			run: func(path string, tracker *FileTracker) error {
				_, err := NewWriteFile(tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "edit",
			run: func(path string, tracker *FileTracker) error {
				_, err := NewEditFile(tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
					"path":       path,
					"old_string": "content",
					"new_string": "after",
				})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := readIdentityTrackedFile(t, "content")
			tracker := NewFileTracker()
			tracker.PopulateFromMessages([]PersistedMessage{{Role: "tool", ToolName: "read_file", Path: path}})

			var changedErr *FileChangedError
			if err := tc.run(path, tracker); !errors.As(err, &changedErr) {
				t.Fatalf("operation error = %T %v, want *FileChangedError", err, err)
			}
			assertFileContent(t, path, "content")
		})
	}
}

func TestNewMissingWriteWithoutReadRecordStillCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.txt")
	tracker := NewFileTracker()

	if _, err := NewWriteFile(tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "created"}); err != nil {
		t.Fatalf("write_file new missing target error = %v", err)
	}

	assertFileContent(t, path, "created")
	if _, ok := tracker.WasRead(path); ok {
		t.Fatal("new write created read authorization, want no read record")
	}
}

func readIdentityTrackedFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, time.Unix(1000, 0))
	return path
}

func recreateWithSameMtimeForIdentity(t *testing.T, path, content string) {
	t.Helper()
	mtime := mustStat(t, path).ModTime()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, mtime)
}

func assertSingleStaleReadBatch(t *testing.T, results []BatchResult) {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Success || !strings.Contains(results[0].Error, "modified since your last read") {
		t.Fatalf("result = %+v, want stale-read failure", results[0])
	}
}

func assertFileAbsentAfterStaleRefusal(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s = %v, want not exist", path, err)
	}
}

func fileIdentityForPath(t *testing.T, path string) FileIdentity {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return FileIdentityFromFileInfo(info)
}

type mutatingSnapshotStoreForIdentity struct {
	turn   int
	path   string
	mutate func(string)
}

func (s *mutatingSnapshotStoreForIdentity) Snapshot(turn int, absPath string) error {
	return s.SnapshotResolved(turn, absPath, absPath)
}

func (s *mutatingSnapshotStoreForIdentity) SnapshotResolved(int, string, string) error {
	s.mutate(s.path)
	return nil
}

func (s *mutatingSnapshotStoreForIdentity) CurrentTurn() int {
	return s.turn
}

type recordingSnapshotStoreForIdentity struct {
	turn  int
	calls int
}

func (s *recordingSnapshotStoreForIdentity) Snapshot(turn int, absPath string) error {
	return s.SnapshotResolved(turn, absPath, absPath)
}

func (s *recordingSnapshotStoreForIdentity) SnapshotResolved(int, string, string) error {
	s.calls++
	return nil
}

func (s *recordingSnapshotStoreForIdentity) CurrentTurn() int {
	return s.turn
}
