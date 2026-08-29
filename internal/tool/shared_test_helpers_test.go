package tool

import (
	"os"
	"testing"
)

// Shared, non-staging test fixtures recovered from the deleted staging executor
// test file. These are used across the immediate-path tool test suites
// (apply_patch, write_file, edit_file, file_identity, snapshot_lifecycle, ...).

type snapshotCall struct {
	turn      int
	path      string
	canonical string
}

type snapshotIdentityRecord struct {
	turn    int
	entryID string
	content string
	absent  bool
}

// recordingSnapshotStore is a SnapshotStore that records every call and the
// before-content of each path. The real snapshot store does this; the test
// store lets immediate-path assertions inspect what was captured.
type recordingSnapshotStore struct {
	turn            int
	calls           []snapshotCall
	before          map[string]string
	identityRecords []snapshotIdentityRecord
}

func (s *recordingSnapshotStore) Snapshot(turn int, absPath string) error {
	return s.SnapshotResolved(turn, absPath, absPath)
}

func (s *recordingSnapshotStore) SnapshotResolved(turn int, originalPath, canonicalPath string) error {
	data, err := os.ReadFile(canonicalPath)
	if err != nil {
		return err
	}
	s.calls = append(s.calls, snapshotCall{turn: turn, path: originalPath, canonical: canonicalPath})
	s.before[canonicalPath] = string(data)
	return nil
}

func (s *recordingSnapshotStore) SnapshotResolvedEntry(turn int, originalPath, canonicalPath string) (string, bool, error) {
	if err := s.SnapshotResolved(turn, originalPath, canonicalPath); err != nil {
		return "", false, err
	}
	return canonicalPath, true, nil
}

func (s *recordingSnapshotStore) DiscardSnapshotEntry(int, string) error {
	return nil
}

func (s *recordingSnapshotStore) RetainSnapshotEntry(int, string) {}

func (s *recordingSnapshotStore) RecordSnapshotContent(turn int, entryID string, content []byte) error {
	s.identityRecords = append(s.identityRecords, snapshotIdentityRecord{
		turn:    turn,
		entryID: entryID,
		content: string(content),
	})
	return nil
}

func (s *recordingSnapshotStore) RecordSnapshotAbsence(turn int, entryID string) error {
	s.identityRecords = append(s.identityRecords, snapshotIdentityRecord{
		turn:    turn,
		entryID: entryID,
		absent:  true,
	})
	return nil
}

func (s *recordingSnapshotStore) CurrentTurn() int {
	return s.turn
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != want {
		t.Fatalf("content of %s = %q, want %q", path, got, want)
	}
}

func assertSnapshotIdentityContent(t *testing.T, records []snapshotIdentityRecord, turn int, entryID, content string) {
	t.Helper()
	for _, record := range records {
		if record.turn == turn && record.entryID == entryID && !record.absent && record.content == content {
			return
		}
	}
	t.Fatalf("snapshot identity records = %+v, want turn=%d entry=%q content=%q", records, turn, entryID, content)
}

func assertSnapshotIdentityAbsent(t *testing.T, records []snapshotIdentityRecord, turn int, entryID string) {
	t.Helper()
	for _, record := range records {
		if record.turn == turn && record.entryID == entryID && record.absent {
			return
		}
	}
	t.Fatalf("snapshot identity records = %+v, want turn=%d entry=%q absent", records, turn, entryID)
}
