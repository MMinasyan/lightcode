package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// testCodeGroupStore adapts one real snapshot.CodeStore to the complete
// SnapshotStore interface in the same private group-adapter shape the
// production tools plugin uses around a per-call code group.
type testCodeGroupStore struct {
	*snapshot.CodeStore
	groupDir string
}

// newCodeGroupStore opens one real artifact code group at the test's explicit
// directory.
func newCodeGroupStore(t *testing.T, groupDir string) testCodeGroupStore {
	t.Helper()
	store, err := snapshot.OpenCodeStore(groupDir)
	testassert.NoError(t, err)
	return testCodeGroupStore{CodeStore: store, groupDir: groupDir}
}

// newTempCodeGroupStore opens one real artifact code group in a fresh
// temporary directory.
func newTempCodeGroupStore(t *testing.T) testCodeGroupStore {
	t.Helper()
	return newCodeGroupStore(t, filepath.Join(t.TempDir(), "group"))
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

type snapshotCall struct {
	path      string
	canonical string
	entryID   string
	created   bool
}

type snapshotIdentityRecord struct {
	entryID string
	content string
	absent  bool
}

// recordingCodeGroupStore is the single recording/fault wrapper over one real
// code group. Capture, entry ids, created flags, identity replacement,
// records and discard all delegate to the real store and persist on disk;
// the counters exist only so invocation-order assertions can inspect what
// was captured. onSnapshot fires after the underlying SnapshotResolvedEntry
// succeeded, before the returned entry is used by the caller. failOnCall
// makes the Nth capture return errFail instead of capturing (simulated
// capture failure); plantBefore creates one file just before the next
// capture (concurrent appearance); failAbandon makes a non-nil identity
// replacement (the prior-proof restoration) fail without performing it.
type recordingCodeGroupStore struct {
	testCodeGroupStore
	calls           []snapshotCall
	identityRecords []snapshotIdentityRecord
	discards        []string
	onSnapshot      func(call int)
	failOnCall      int
	errFail         error
	plantBefore     string
	failAbandon     bool
}

func newRecordingCodeGroupStore(t *testing.T) *recordingCodeGroupStore {
	t.Helper()
	return &recordingCodeGroupStore{testCodeGroupStore: newTempCodeGroupStore(t)}
}

func (s *recordingCodeGroupStore) SnapshotResolvedEntry(originalPath, canonicalPath string) (string, bool, error) {
	if s.plantBefore != "" {
		path := s.plantBefore
		s.plantBefore = ""
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(path, appearedDestinationBytes, 0o644); err != nil {
				return "", false, err
			}
		}
	}
	if s.failOnCall > 0 && len(s.calls)+1 == s.failOnCall {
		return "", false, s.errFail
	}
	entryID, created, err := s.testCodeGroupStore.SnapshotResolvedEntry(originalPath, canonicalPath)
	if err != nil {
		return "", false, err
	}
	s.calls = append(s.calls, snapshotCall{path: originalPath, canonical: canonicalPath, entryID: entryID, created: created})
	if s.onSnapshot != nil {
		s.onSnapshot(len(s.calls))
	}
	return entryID, created, nil
}

func (s *recordingCodeGroupStore) DiscardSnapshotEntry(entryID string) error {
	s.discards = append(s.discards, entryID)
	return s.testCodeGroupStore.DiscardSnapshotEntry(entryID)
}

func (s *recordingCodeGroupStore) ReplaceSnapshotIdentity(entryID string, identity *snapshot.SnapshotContentIdentity) (*snapshot.SnapshotContentIdentity, error) {
	if s.failAbandon && identity != nil {
		return nil, errInjectedAbandon
	}
	return s.testCodeGroupStore.ReplaceSnapshotIdentity(entryID, identity)
}

func (s *recordingCodeGroupStore) RecordSnapshotContent(entryID string, content []byte) error {
	s.identityRecords = append(s.identityRecords, snapshotIdentityRecord{entryID: entryID, content: string(content)})
	return s.testCodeGroupStore.RecordSnapshotContent(entryID, content)
}

func (s *recordingCodeGroupStore) RecordSnapshotAbsence(entryID string) error {
	s.identityRecords = append(s.identityRecords, snapshotIdentityRecord{entryID: entryID, absent: true})
	return s.testCodeGroupStore.RecordSnapshotAbsence(entryID)
}

// entryIDFor returns the real entry id the recorded capture reported for one
// canonical target.
func (s *recordingCodeGroupStore) entryIDFor(canonical string) string {
	for _, c := range s.calls {
		if c.canonical == canonical {
			return c.entryID
		}
	}
	return ""
}

// --- on-disk snapshot outcome assertions ---

func snapshotIdentityHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func snapshotEntryDir(store testCodeGroupStore, entryID string) string {
	return filepath.Join(store.groupDir, "snapshots", "1", entryID)
}

func soleSnapshotMeta(t *testing.T, store testCodeGroupStore) snapshot.SnapshotMeta {
	t.Helper()
	metas, err := store.ListEntries()
	testassert.NoError(t, err)
	testassert.Require(t, len(metas) == 1, "ListEntries = %+v, want one entry", metas)
	return metas[0]
}

func metaForCanonical(t *testing.T, store testCodeGroupStore, canonical string) snapshot.SnapshotMeta {
	t.Helper()
	metas, err := store.ListEntries()
	testassert.NoError(t, err)
	for _, meta := range metas {
		if meta.CanonicalPath == canonical {
			return meta
		}
	}
	t.Fatalf("ListEntries = %+v, want an entry recorded for %s", metas, canonical)
	return snapshot.SnapshotMeta{}
}

func assertSnapshotEntryCount(t *testing.T, groupDir string, want int) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(groupDir, "snapshots", "1"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	testassert.Require(t, len(entries) == want, "snapshot entries = %v, want %d", entries, want)
}

func assertEntryPresent(t *testing.T, store *recordingCodeGroupStore, canonical string) {
	t.Helper()
	entryID := store.entryIDFor(canonical)
	testassert.Require(t, entryID != "", "no recorded capture for %s", canonical)
	info, err := os.Stat(snapshotEntryDir(store.testCodeGroupStore, entryID))
	testassert.Require(t, err == nil && info.IsDir(), "entry %s for %s = %v, want it persisted on disk", entryID, canonical, err)
}

func assertEntryDiscarded(t *testing.T, store *recordingCodeGroupStore, canonical string) {
	t.Helper()
	entryID := store.entryIDFor(canonical)
	testassert.Require(t, entryID != "", "no recorded capture for %s", canonical)
	_, err := os.Stat(snapshotEntryDir(store.testCodeGroupStore, entryID))
	testassert.Require(t, os.IsNotExist(err), "entry %s for %s = %v, want the refused new capture discarded from disk", entryID, canonical, err)
}

func assertEntryOriginal(t *testing.T, groupDir, want string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(groupDir, "snapshots", "1"))
	testassert.NoError(t, err)
	testassert.Require(t, len(entries) == 1 && entries[0].IsDir(), "snapshot entries = %v, want one entry directory", entries)
	data, err := os.ReadFile(filepath.Join(groupDir, "snapshots", "1", entries[0].Name(), "original"))
	testassert.NoError(t, err)
	testassert.Require(t, string(data) == want, "preimage = %q, want %q", data, want)
}

func assertEntryPreimage(t *testing.T, store *recordingCodeGroupStore, canonical, want string) {
	t.Helper()
	entryID := store.entryIDFor(canonical)
	testassert.Require(t, entryID != "", "no recorded capture for %s", canonical)
	data, err := os.ReadFile(filepath.Join(snapshotEntryDir(store.testCodeGroupStore, entryID), "original"))
	testassert.NoError(t, err)
	testassert.Require(t, string(data) == want, "persisted preimage of %s = %q, want %q", canonical, data, want)
}

func revertTestGroup(t *testing.T, groupDir string) snapshot.RevertResult {
	t.Helper()
	result, err := snapshot.RevertCodeGroups([]string{groupDir})
	testassert.NoError(t, err)
	return result
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	testassert.Require(t, os.IsNotExist(err), "%s = %v, want absent", path, err)
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

func assertSnapshotIdentityContent(t *testing.T, records []snapshotIdentityRecord, entryID, content string) {
	t.Helper()
	for _, record := range records {
		if record.entryID == entryID && !record.absent && record.content == content {
			return
		}
	}
	t.Fatalf("snapshot identity records = %+v, want entry=%q content=%q", records, entryID, content)
}

func assertSnapshotIdentityAbsent(t *testing.T, records []snapshotIdentityRecord, entryID string) {
	t.Helper()
	for _, record := range records {
		if record.entryID == entryID && record.absent {
			return
		}
	}
	t.Fatalf("snapshot identity records = %+v, want entry=%q absent", records, entryID)
}

// --- shared producer inventory ---

// producerMutationCase is one standard mutation producer with the content it
// intends to produce, shared by the lifecycle truncate/full-write/sync rows
// and the short/foreign-write identity rows.
type producerMutationCase struct {
	name     string
	existing bool
	seed     string
	intended string
	run      func(t *testing.T, root, path string, store testCodeGroupStore) error
}

func producerMutationCases() []producerMutationCase {
	return []producerMutationCase{
		{
			name: "write", existing: true, seed: "before", intended: "after",
			run: func(t *testing.T, root, path string, store testCodeGroupStore) error {
				_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "edit", existing: true, seed: "before", intended: "after",
			run: func(t *testing.T, root, path string, store testCodeGroupStore) error {
				_, err := execEdit(t, root, "", "", CapabilityOptions{}, store, map[string]any{
					"path": path, "old_string": "before", "new_string": "after",
				})
				return err
			},
		},
		{
			name: "patch_update", existing: true, seed: "a\nB\nc", intended: "a\nB2\nc",
			run: func(t *testing.T, root, _ string, store testCodeGroupStore) error {
				_, err := execPatch(t, root, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, "*** Update File: file.txt\n@@\n-B\n+B2")})
				return err
			},
		},
		{
			name: "write_created", existing: false, seed: "", intended: "after",
			run: func(t *testing.T, root, path string, store testCodeGroupStore) error {
				_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "patch_add_created", existing: false, seed: "", intended: "after",
			run: func(t *testing.T, root, _ string, store testCodeGroupStore) error {
				_, err := execPatch(t, root, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, "*** Add File: created.txt\n+after")})
				return err
			},
		},
	}
}

// runProducerMutationCase drives one producer case through one injected
// mutation stimulus over the real store and asserts the produced-bytes
// outcome, the persisted meta identity, and the reversion result.
func runProducerMutationCase(t *testing.T, tc producerMutationCase, step mutationFailureStep) {
	t.Helper()
	root := t.TempDir()
	name := "created.txt"
	if tc.existing {
		name = "file.txt"
	}
	path := filepath.Join(root, name)
	if tc.existing {
		testassert.NoError(t, os.WriteFile(path, []byte(tc.seed), 0o644))
	}
	groupDir := filepath.Join(t.TempDir(), "group")
	store := newCodeGroupStore(t, groupDir)
	withMutationFailure(t, path, step)

	err := tc.run(t, root, path, store)

	testassert.Require(t, err != nil, "%s with %s succeeded, want injected mutation failure", tc.name, step)
	meta := soleSnapshotMeta(t, store)
	assertSnapshotEntryCount(t, groupDir, 1)
	if tc.existing {
		assertEntryOriginal(t, groupDir, tc.seed)
	}
	prefix := tc.intended[:len(tc.intended)/2]
	switch step {
	case failAfterTruncate, failShortWriteError, failShortWriteNil:
		testassert.Require(t, meta.LastWrite == nil, "%s: LastWrite = %+v, want nil: an incomplete mutation may not claim a complete identity", step, meta.LastWrite)
	case failAfterWrite, failAfterSync, failForeignWriteError, failForeignSync:
		testassert.Require(t, meta.LastWrite != nil && meta.LastWrite.Hash == snapshotIdentityHash(tc.intended), "%s: LastWrite = %+v, want the own complete intended bytes, never a foreign reread", step, meta.LastWrite)
	}
	switch step {
	case failAfterTruncate:
		assertFileContent(t, path, "")
	case failShortWriteError, failShortWriteNil:
		assertFileContent(t, path, prefix)
	case failForeignWriteError, failForeignSync:
		assertFileContent(t, path, string(foreignMutationBytes))
	}
	result := revertTestGroup(t, groupDir)
	switch step {
	case failAfterWrite, failAfterSync:
		testassert.Require(t, len(result.Restored) == 1 && result.Restored[0] == path && len(result.Skipped) == 0, "restored = %v skipped = %+v, want the known-complete mutation restorable for %s", result.Restored, result.Skipped, path)
		if tc.existing {
			assertFileContent(t, path, tc.seed)
		} else {
			assertPathMissing(t, path)
		}
	default:
		testassert.Require(t, len(result.Restored) == 0 && len(result.Skipped) == 1 && result.Skipped[0].Path == path, "restored = %v want none; skipped = %+v, want one skip for %s", result.Restored, result.Skipped, path)
		switch step {
		case failAfterTruncate:
			assertFileContent(t, path, "")
		case failShortWriteError, failShortWriteNil:
			assertFileContent(t, path, prefix)
		case failForeignWriteError, failForeignSync:
			assertFileContent(t, path, string(foreignMutationBytes))
		}
	}
}
