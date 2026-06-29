package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestDirectWritesIgnoreReadFileIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, path string, tracker *FileTracker) error
	}{
		{
			name: "write deleted after read",
			run: func(t *testing.T, path string, tracker *FileTracker) error {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				_, err := NewWriteFile(tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "write recreated after read",
			run: func(t *testing.T, path string, tracker *FileTracker) error {
				recreateWithSameMtimeForIdentity(t, path, "external")
				_, err := NewWriteFile(tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "edit recreated after read",
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

			if err := tc.run(t, path, tracker); err != nil {
				t.Fatalf("operation error = %v", err)
			}
			assertFileContent(t, path, "after")
		})
	}
}

func TestStagedWritesIgnoreReadFileIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, path string, tracker *FileTracker) []BatchResult
	}{
		{
			name: "write deleted after read",
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
			name: "write recreated after read",
			run: func(t *testing.T, path string, tracker *FileTracker) []BatchResult {
				recreateWithSameMtimeForIdentity(t, path, "external")
				return NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil).ExecutePending(context.Background(), []StagedCall{
					stagedWrite(path, "call-1", "after"),
				})
			},
		},
		{
			name: "edit recreated after read",
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
			assertSingleSuccessfulBatch(t, results)
			assertFileContent(t, path, "after")
		})
	}
}

func TestSnapshotAwareFileToolsRecordPostWriteIdentityAfterRevalidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(path string, tracker *FileTracker, store *recordingIdentitySnapshotStore) error
	}{
		{
			name: "write",
			run: func(path string, tracker *FileTracker, store *recordingIdentitySnapshotStore) error {
				_, err := NewWriteFileWithSnapshot(store, tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
					"path":    path,
					"content": "after",
				})
				return err
			},
		},
		{
			name: "edit",
			run: func(path string, tracker *FileTracker, store *recordingIdentitySnapshotStore) error {
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
			store := &recordingIdentitySnapshotStore{turn: 1, entryID: "entry", mutate: func() {
				recreateWithSameMtimeForIdentity(t, path, "external")
			}}

			if err := tc.run(path, tracker, store); err != nil {
				t.Fatalf("operation error = %v", err)
			}
			assertFileContent(t, path, "after")
			if string(store.recordedContent) != "after" {
				t.Fatalf("recorded content = %q, want after", store.recordedContent)
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

	records := tracker.Snapshot()
	if len(records) != 1 || records[0].Path != path {
		t.Fatalf("tracker records = %+v, want one read for %s", records, path)
	}
	identity := records[0].Identity
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

	records := tracker.Snapshot()
	if len(records) != 1 {
		t.Fatalf("tracker records = %+v, want one", records)
	}
	before := records[0].Identity
	recreateWithSameMtimeForIdentity(t, path, "external")
	after := fileIdentityForPath(t, path)

	if identityMatches(before, after) {
		t.Fatalf("identity matched after delete/recreate with same mtime: before=%+v after=%+v", before, after)
	}
}

func TestNewMissingWriteWithoutReadRecordStillCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.txt")
	tracker := NewFileTracker()

	if _, err := NewWriteFile(tracker, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "created"}); err != nil {
		t.Fatalf("write_file new missing target error = %v", err)
	}

	assertFileContent(t, path, "created")
	if trackerHasRead(tracker, path) {
		t.Fatal("new write created read record, want no read record")
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
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	setTrackerFileMtime(t, path, mtime)
}

func assertSingleSuccessfulBatch(t *testing.T, results []BatchResult) {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if !results[0].Success || results[0].Error != "" {
		t.Fatalf("result = %+v, want success", results[0])
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

type recordingIdentitySnapshotStore struct {
	turn            int
	entryID         string
	mutate          func()
	recordedContent []byte
}

func (s *recordingIdentitySnapshotStore) Snapshot(turn int, absPath string) error {
	return s.SnapshotResolved(turn, absPath, absPath)
}

func (s *recordingIdentitySnapshotStore) SnapshotResolved(int, string, string) error {
	if s.mutate != nil {
		mutate := s.mutate
		s.mutate = nil
		mutate()
	}
	return nil
}

func (s *recordingIdentitySnapshotStore) SnapshotResolvedEntry(int, string, string) (string, bool, error) {
	if err := s.SnapshotResolved(s.turn, "", ""); err != nil {
		return "", false, err
	}
	return s.entryID, true, nil
}

func (s *recordingIdentitySnapshotStore) DiscardSnapshotEntry(int, string) error {
	return nil
}

func (s *recordingIdentitySnapshotStore) RetainSnapshotEntry(int, string) {}

func (s *recordingIdentitySnapshotStore) RecordSnapshotContent(_ int, _ string, content []byte) error {
	s.recordedContent = append([]byte(nil), content...)
	return nil
}

func (s *recordingIdentitySnapshotStore) RecordSnapshotAbsence(int, string) error {
	return nil
}

func (s *recordingIdentitySnapshotStore) CurrentTurn() int {
	return s.turn
}
