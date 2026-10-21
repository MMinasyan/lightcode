package tool

import (
	"context"
	"os"
	"testing"

	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// testCodeGroupStore adapts one real snapshot.CodeStore to the shared
// SnapshotStore interface in the same private group-adapter shape the
// production tools plugin uses around a per-call code group: the group's
// single turn is explicit test identity, and Snapshot delegates to the
// resolved-entry capture.
type testCodeGroupStore struct {
	*snapshot.CodeStore
	turn int
}

// CurrentTurn pins the group's single explicit test turn.
func (s testCodeGroupStore) CurrentTurn() int { return s.turn }

// Snapshot satisfies the plain SnapshotStore surface through the resolved
// entry capture.
func (s testCodeGroupStore) Snapshot(turn int, absPath string) error {
	_, _, err := s.SnapshotResolvedEntry(turn, absPath, absPath)
	return err
}

// newCodeGroupStore opens one real artifact code group at the test's explicit
// directory and turn identity.
func newCodeGroupStore(t *testing.T, groupDir string, turn int) testCodeGroupStore {
	t.Helper()
	store, err := snapshot.OpenCodeStore(groupDir)
	testassert.NoError(t, err)
	return testCodeGroupStore{CodeStore: store, turn: turn}
}

// execPatch runs one apply_patch through the exported Normalize* + Prepare*
// entries and returns the structured execution result.
func execPatch(t *testing.T, root, rootCanonical, writeDirCanonical string, opts CapabilityOptions, store SnapshotStore, args map[string]any) (PreparedPatchResult, error) {
	t.Helper()
	if rootCanonical == "" {
		var err error
		rootCanonical, err = bindWorkspaceRoot(root)
		if err != nil {
			return PreparedPatchResult{}, err
		}
	}
	norm, err := NormalizePatchArgs(args)
	if err != nil {
		return PreparedPatchResult{}, err
	}
	call, err := PreparePatchCall(root, rootCanonical, writeDirCanonical, opts, store, norm)
	if err != nil {
		return PreparedPatchResult{}, err
	}
	return call.Execute(context.Background())
}

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
	testassert.NoError(t, err)
	{
		got := string(data)
		testassert.Require(t, got == want, "content of %s = %q, want %q", path, got, want)
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
