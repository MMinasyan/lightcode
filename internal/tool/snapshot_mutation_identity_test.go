package tool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// Last-produced snapshot identities must come only from what the mutation
// producer actually completed: retained write byte counts and known-complete
// produced content, never a post-error filesystem reread. The pre-mutation
// proof clear must also hold: when it fails, no mutation I/O may run.

// --- incomplete and foreign-byte mutation failures over the real store ---

func TestShortWriteClaimsNoProducedIdentityAndRevertSkips(t *testing.T) {
	for _, step := range []mutationFailureStep{failShortWriteError, failShortWriteNil} {
		for _, tc := range producerMutationCases() {
			t.Run(string(step)+"_"+tc.name, func(t *testing.T) {
				runProducerMutationCase(t, tc, step)
			})
		}
	}
}

func TestForeignWriteBeforeErrorKeepsOwnProducedIdentityAndRevertSkips(t *testing.T) {
	for _, step := range []mutationFailureStep{failForeignWriteError, failForeignSync} {
		for _, tc := range producerMutationCases() {
			t.Run(string(step)+"_"+tc.name, func(t *testing.T) {
				runProducerMutationCase(t, tc, step)
			})
		}
	}
}

// --- mutation file I/O counting ---

type mutationIOCount struct {
	opens     int
	writes    int
	truncates int
}

type countingMutationFile struct {
	mutationFile
	count *mutationIOCount
}

func (c *countingMutationFile) Write(p []byte) (int, error) {
	c.count.writes++
	return c.mutationFile.Write(p)
}

func (c *countingMutationFile) Truncate(size int64) error {
	c.count.truncates++
	return c.mutationFile.Truncate(size)
}

func withMutationFileCounting(t *testing.T, count *mutationIOCount) {
	t.Helper()
	prev := openWriteTargetForMutationFunc
	openWriteTargetForMutationFunc = func(absPath string) (mutationFile, bool, error) {
		f, existed, err := prev(absPath)
		if err == nil {
			count.opens++
			f = &countingMutationFile{mutationFile: f, count: count}
		}
		return f, existed, err
	}
	t.Cleanup(func() { openWriteTargetForMutationFunc = prev })
}

// --- snapshot meta publication failure seam ---

type injectedMetaSyncError string

func (e injectedMetaSyncError) Error() string { return string(e) }

var errInjectedMetaSync = injectedMetaSyncError("injected snapshot meta publish failure")

// withSnapshotMetaPublishFailure injects a failure on the nth meta.json
// temp-file publication (capture writes and proof/identity writes) done while
// the seam is installed, leaving every other publication to the real sync.
func withSnapshotMetaPublishFailure(t *testing.T, nth int) {
	t.Helper()
	previous := atomicfs.SyncFileFunc
	metaSyncs := 0
	atomicfs.SyncFileFunc = func(f *os.File) error {
		if strings.HasPrefix(filepath.Base(f.Name()), "meta.json.tmp-") {
			metaSyncs++
			if metaSyncs == nth {
				return errInjectedMetaSync
			}
		}
		if previous != nil {
			return previous(f)
		}
		return f.Sync()
	}
	t.Cleanup(func() { atomicfs.SyncFileFunc = previous })
}

// --- pre-mutation proof clear and prior-proof restoration ---

func TestProofClearFailureBeforeMutationLeavesFileAndPreimageUntouched(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	groupDir := filepath.Join(t.TempDir(), "group")
	store := newCodeGroupStore(t, groupDir)
	{
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": path, "content": "first"})
		testassert.NoError(t, err)
	}
	testassert.Require(t, soleSnapshotMeta(t, store).LastWrite != nil && soleSnapshotMeta(t, store).LastWrite.Hash == snapshotIdentityHash("first"), "seed LastWrite = %+v, want the first write's proof", soleSnapshotMeta(t, store).LastWrite)

	withSnapshotMetaPublishFailure(t, 1)
	counts := &mutationIOCount{}
	withMutationFileCounting(t, counts)

	_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": path, "content": "second"})

	testassert.Require(t, err != nil, "write whose pre-mutation proof clear fails succeeded, want refusal without mutation")
	assertFileContent(t, path, "first")
	last := soleSnapshotMeta(t, store).LastWrite
	testassert.Require(t, last != nil && last.Hash == snapshotIdentityHash("first"), "LastWrite after proof-clear failure = %+v, want the prior proof unchanged", last)
	assertEntryOriginal(t, groupDir, "before")
	testassert.Require(t, counts.writes == 0 && counts.truncates == 0, "mutation file I/O = %+v, want no Write/Truncate after a failed proof clear", counts)
}

func TestProvenPreMutationFailureRestoresPriorProof(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	groupDir := filepath.Join(t.TempDir(), "group")
	store := newCodeGroupStore(t, groupDir)
	{
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": path, "content": "first"})
		testassert.NoError(t, err)
	}

	_, err := execEdit(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": path, "old_string": "missing", "new_string": "second"})

	testassert.Require(t, err != nil, "edit with no match succeeded, want a pre-mutation failure")
	assertFileContent(t, path, "first")
	last := soleSnapshotMeta(t, store).LastWrite
	testassert.Require(t, last != nil && last.Hash == snapshotIdentityHash("first"), "LastWrite after proven non-mutating failure = %+v, want the prior proof restored", last)
	result := revertTestGroup(t, groupDir)
	testassert.Require(t, len(result.Restored) == 1 && result.Restored[0] == path && len(result.Skipped) == 0, "restored = %v skipped = %+v, want the retained snapshot to restore %s", result.Restored, result.Skipped, path)
	assertFileContent(t, path, "before")
}

func TestFailedPriorProofRestoreKeepsNilProofAndSkipsLaterRevert(t *testing.T) {
	root := t.TempDir()
	path := snapshotLifecycleFile(t, root, "file.txt", "before")
	groupDir := filepath.Join(t.TempDir(), "group")
	store := newCodeGroupStore(t, groupDir)
	{
		_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": path, "content": "first"})
		testassert.NoError(t, err)
	}

	// The failing publication is the prior-proof restore: the pre-mutation
	// proof clear that precedes it succeeds, the restore is the second meta
	// publication of this call.
	withSnapshotMetaPublishFailure(t, 2)
	_, err := execEdit(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": path, "old_string": "missing", "new_string": "second"})

	testassert.Require(t, err != nil && strings.Contains(err.Error(), "restore snapshot proof"), "edit error = %v, want the failed proof restoration reported", err)
	assertFileContent(t, path, "first")
	testassert.Require(t, soleSnapshotMeta(t, store).LastWrite == nil, "LastWrite after failed proof restoration = %+v, want nil proof", soleSnapshotMeta(t, store).LastWrite)
	assertEntryOriginal(t, groupDir, "before")
	result := revertTestGroup(t, groupDir)
	testassert.Require(t, len(result.Restored) == 0 && len(result.Skipped) == 1 && result.Skipped[0].Path == path, "restored = %v skipped = %+v, want the nil proof to skip %s", result.Restored, result.Skipped, path)
	assertSnapshotEntryCount(t, groupDir, 1)
	assertFileContent(t, path, "first")
}

func TestFailedFinalIdentityRecordKeepsNilProofAndSkipsRevert(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "created.txt")
	groupDir := filepath.Join(t.TempDir(), "group")
	store := newCodeGroupStore(t, groupDir)

	// Publications under the seam: capture meta, pre-mutation proof clear,
	// and the final identity record — only the final record fails.
	withSnapshotMetaPublishFailure(t, 3)
	_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": path, "content": "created"})

	testassert.Require(t, err != nil && strings.Contains(err.Error(), "record snapshot identity"), "write error = %v, want the failed final identity record", err)
	assertFileContent(t, path, "created")
	testassert.Require(t, soleSnapshotMeta(t, store).LastWrite == nil, "LastWrite after failed final record = %+v, want nil proof", soleSnapshotMeta(t, store).LastWrite)
	assertSnapshotEntryCount(t, groupDir, 1)
	result := revertTestGroup(t, groupDir)
	testassert.Require(t, len(result.Restored) == 0 && len(result.Skipped) == 1 && result.Skipped[0].Path == path, "restored = %v skipped = %+v, want the nil proof to skip %s", result.Restored, result.Skipped, path)
	assertFileContent(t, path, "created")
}

// --- destination appeared during apply: refusal plus abandonment cleanup ---

// appearedDestinationBytes are the outside-origin bytes that create the
// destination between plan validation and the apply-time open.
var appearedDestinationBytes = []byte("appeared")

type injectedAbandonError string

func (e injectedAbandonError) Error() string { return string(e) }

var errInjectedAbandon = injectedAbandonError("injected snapshot abandonment failure")

func TestAppearedDestinationWithFailedAbandonReportsRefusalAndCleanupFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "created.txt")
	groupDir := filepath.Join(t.TempDir(), "group")
	store := newCodeGroupStore(t, groupDir)
	{
		_, err := execPatch(t, root, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, "*** Add File: created.txt\n+one")})
		testassert.NoError(t, err)
	}
	testassert.NoError(t, os.Remove(path))

	wrapped := &recordingCodeGroupStore{testCodeGroupStore: store, plantBefore: path, failAbandon: true}
	_, err := execPatch(t, root, "", "", CapabilityOptions{}, wrapped, map[string]any{"input": applyPatchInput(t, "*** Add File: created.txt\n+two")})

	testassert.Require(t, err != nil, "Add whose destination appeared with failing abandonment succeeded, want refusal")
	msg := err.Error()
	testassert.Require(t, strings.Contains(msg, "appeared during apply"), "err = %v, want the destination-appeared refusal itself", err)
	testassert.Require(t, strings.Contains(msg, "additionally failed to abandon snapshot"), "err = %v, want the abandonment failure reported alongside the refusal", err)
	testassert.Require(t, strings.Contains(msg, errInjectedAbandon.Error()), "err = %v, want the injected cleanup failure visible", err)
	testassert.Require(t, !strings.Contains(msg, "%!w(<nil>)"), "err = %q, want the refusal wrapped around the cleanup failure, no %%!w(<nil>)", msg)
	testassert.Require(t, len(wrapped.discards) == 0, "discards = %v, want none: the reused entry is not discardable", wrapped.discards)
	assertFileContent(t, path, string(appearedDestinationBytes))
	testassert.Require(t, soleSnapshotMeta(t, store).LastWrite == nil, "LastWrite after failed restoration = %+v, want the cleared nil proof preserved", soleSnapshotMeta(t, store).LastWrite)
	assertSnapshotEntryCount(t, groupDir, 1)
}

func TestAppearedDestinationWithCompletedAbandonKeepsEntryAndPriorProof(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "created.txt")
	groupDir := filepath.Join(t.TempDir(), "group")
	store := newCodeGroupStore(t, groupDir)
	{
		_, err := execPatch(t, root, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, "*** Add File: created.txt\n+one")})
		testassert.NoError(t, err)
	}
	testassert.NoError(t, os.Remove(path))

	wrapped := &recordingCodeGroupStore{testCodeGroupStore: store, plantBefore: path}
	_, err := execPatch(t, root, "", "", CapabilityOptions{}, wrapped, map[string]any{"input": applyPatchInput(t, "*** Add File: created.txt\n+two")})

	testassert.Require(t, err != nil && strings.Contains(err.Error(), "appeared during apply"), "err = %v, want the destination-appeared refusal", err)
	testassert.Require(t, !strings.Contains(err.Error(), "additionally failed"), "err = %v, want the completed abandonment reported without a cleanup failure", err)
	testassert.Require(t, !strings.Contains(err.Error(), "%!w(<nil>)"), "err = %q, want no %%!w(<nil>)", err.Error())
	testassert.Require(t, len(wrapped.discards) == 0, "discards = %v, want none: the reused entry survives", wrapped.discards)
	assertFileContent(t, path, string(appearedDestinationBytes))
	last := soleSnapshotMeta(t, store).LastWrite
	testassert.Require(t, last != nil && last.Hash == snapshotIdentityHash("one"), "LastWrite after refusal = %+v, want the existing entry's prior proof restored", last)
	assertSnapshotEntryCount(t, groupDir, 1)
}

// --- post-capture rebinding refusals for every mutation producer ---

func rebindingLifecycleFixture(t *testing.T) (root, link, real1, real2 string) {
	t.Helper()
	root = t.TempDir()
	real1 = filepath.Join(root, "real1.txt")
	real2 = filepath.Join(root, "real2.txt")
	for _, p := range []string{real1, real2} {
		testassert.NoError(t, os.WriteFile(p, []byte("before"), 0o644))
	}
	link = filepath.Join(root, "link.txt")
	testassert.NoError(t, os.Symlink(real1, link))
	return root, link, real1, real2
}

func rebindLink(t *testing.T, link, target string) {
	t.Helper()
	testassert.NoError(t, os.Remove(link))
	testassert.NoError(t, os.Symlink(target, link))
}

// TestMutationToolsRefuseRebindingDuringSnapshotCapture threads every patch
// mutation (Update, Add, Delete, Move destination, Move source) and the
// write/edit producers through target, Workspace-root, and write-dir rebinding
// flips that land AFTER the real snapshot capture (the wrapper's onSnapshot
// seam): the post-capture fail-closed revalidators must refuse, leave BOTH
// possible targets unmutated, and abandon the refused capture on disk — a
// freshly created capture is discarded; a reused entry survives with its prior
// proof restored (shared abandonment rule, observed by the two reused rows).
// Move-source refusals must keep the already completed destination and report
// the partial work.
func TestMutationToolsRefuseRebindingDuringSnapshotCapture(t *testing.T) {
	refusalFlip := func(store *recordingCodeGroupStore, at int, flip func()) {
		store.onSnapshot = func(call int) {
			if call == at {
				flip()
			}
		}
	}
	assertPartialCommitted := func(t *testing.T, err error, committed string) {
		t.Helper()
		var exitErr *ExitError
		testassert.Require(t, errors.As(err, &exitErr) && strings.Contains(exitErr.Output, committed), "Execute err = %v, want partial work reporting %s", err, committed)
	}
	assertPlantedSymlink := func(t *testing.T, path string) {
		t.Helper()
		info, err := os.Lstat(path)
		testassert.Require(t, err == nil && info.Mode()&os.ModeSymlink != 0, "%s = %v %v, want the planted symlink, never a written regular file", path, info, err)
	}
	// A refused capture created by this call must be discarded on disk with
	// zero recorded identity for the refused operation.
	assertFreshCaptureAbandoned := func(t *testing.T, store *recordingCodeGroupStore, canonical string) {
		t.Helper()
		testassert.Require(t, len(store.discards) == 1, "discards = %v, want exactly one discard for the refused capture of %s", store.discards, canonical)
		assertEntryDiscarded(t, store, canonical)
		testassert.Require(t, len(store.identityRecords) == 0, "identity records = %+v, want none for the refused operation", store.identityRecords)
	}
	// A refused capture that reused this group's existing entry must leave the
	// entry and its preimage on disk with the prior proof restored, and
	// discard nothing.
	assertReusedCapturePreserved := func(t *testing.T, store *recordingCodeGroupStore, canonical, priorProof string) {
		t.Helper()
		testassert.Require(t, len(store.discards) == 0, "discards = %v, want none: a reused entry is never discarded", store.discards)
		assertEntryPresent(t, store, canonical)
		last := metaForCanonical(t, store.testCodeGroupStore, canonical).LastWrite
		testassert.Require(t, last != nil && last.Hash == snapshotIdentityHash(priorProof), "LastWrite after refusal = %+v, want the prior proof %q restored", last, priorProof)
	}

	type rebindingCase struct {
		name  string
		build func(t *testing.T, store *recordingCodeGroupStore) (run func() error, expect string, assertOutcome func(*testing.T, error))
	}
	cases := []rebindingCase{
		{
			name: "write_target",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				root, link, real1, real2 := rebindingLifecycleFixture(t)
				refusalFlip(store, 1, func() { rebindLink(t, link, real2) })
				run := func() error {
					_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": link, "content": "after"})
					return err
				}
				assert := func(t *testing.T, _ error) {
					assertFileContent(t, real1, "before")
					assertFileContent(t, real2, "before")
					assertFreshCaptureAbandoned(t, store, real1)
				}
				return run, "approved canonical path changed", assert
			},
		},
		{
			// Reused-entry sibling of write_target: the seed write already
			// created the group's entry for real1, so the refused capture
			// reuses it — the entry must survive with the prior proof
			// restored instead of being discarded.
			name: "write_reused_target",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				root, link, real1, real2 := rebindingLifecycleFixture(t)
				_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": link, "content": "first"})
				testassert.NoError(t, err)
				refusalFlip(store, 2, func() { rebindLink(t, link, real2) })
				run := func() error {
					_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": link, "content": "after"})
					return err
				}
				assert := func(t *testing.T, _ error) {
					assertFileContent(t, real1, "first")
					assertFileContent(t, real2, "before")
					assertReusedCapturePreserved(t, store, real1, "first")
					assertSnapshotIdentityContent(t, store.identityRecords, store.entryIDFor(real1), "first")
				}
				return run, "approved canonical path changed", assert
			},
		},
		{
			name: "edit_target",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				root, link, real1, real2 := rebindingLifecycleFixture(t)
				refusalFlip(store, 1, func() { rebindLink(t, link, real2) })
				run := func() error {
					_, err := execEdit(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": link, "old_string": "before", "new_string": "after"})
					return err
				}
				assert := func(t *testing.T, _ error) {
					assertFileContent(t, real1, "before")
					assertFileContent(t, real2, "before")
					assertFreshCaptureAbandoned(t, store, real1)
				}
				return run, "approved canonical path changed", assert
			},
		},
		{
			name: "patch_update_target",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				root, link, real1, real2 := rebindingLifecycleFixture(t)
				refusalFlip(store, 1, func() { rebindLink(t, link, real2) })
				run := func() error {
					_, err := execPatch(t, root, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, "*** Update File: link.txt\n@@\n-before\n+after")})
					return err
				}
				assert := func(t *testing.T, _ error) {
					assertFileContent(t, real1, "before")
					assertFileContent(t, real2, "before")
					assertFreshCaptureAbandoned(t, store, real1)
				}
				return run, "approved canonical path changed", assert
			},
		},
		{
			name: "patch_add_target",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				base := t.TempDir()
				approved := filepath.Join(base, "link.txt") // approved canonical: the absent leaf itself
				replacement := filepath.Join(base, "replacement.txt")
				testassert.NoError(t, os.WriteFile(replacement, []byte("secret"), 0o644))
				refusalFlip(store, 1, func() { testassert.NoError(t, os.Symlink(replacement, approved)) })
				run := func() error {
					_, err := execPatch(t, base, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, "*** Add File: link.txt\n+after")})
					return err
				}
				assert := func(t *testing.T, _ error) {
					assertFileContent(t, replacement, "secret")
					assertPlantedSymlink(t, approved)
					assertFreshCaptureAbandoned(t, store, approved)
				}
				return run, "approved canonical path changed", assert
			},
		},
		{
			name: "patch_delete_target",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				root, link, real1, real2 := rebindingLifecycleFixture(t)
				refusalFlip(store, 1, func() { rebindLink(t, link, real2) })
				run := func() error {
					_, err := execPatch(t, root, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, "*** Delete File: link.txt")})
					return err
				}
				assert := func(t *testing.T, _ error) {
					assertFileContent(t, real1, "before")
					assertFileContent(t, real2, "before")
					assertFreshCaptureAbandoned(t, store, real1)
				}
				return run, "approved canonical path changed", assert
			},
		},
		{
			name: "patch_move_dst_target",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				base := t.TempDir()
				src := filepath.Join(base, "src.txt")
				testassert.NoError(t, os.WriteFile(src, []byte("old content"), 0o644))
				approved := filepath.Join(base, "new.txt") // approved destination: the absent leaf itself
				replacement := filepath.Join(base, "replacement.txt")
				testassert.NoError(t, os.WriteFile(replacement, []byte("secret"), 0o644))
				refusalFlip(store, 1, func() { testassert.NoError(t, os.Symlink(replacement, approved)) })
				run := func() error {
					_, err := execPatch(t, base, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, "*** Update File: src.txt\n*** Move to: new.txt\n@@\n-old content\n+NEW")})
					return err
				}
				assert := func(t *testing.T, _ error) {
					assertFileContent(t, src, "old content")
					assertFileContent(t, replacement, "secret")
					assertPlantedSymlink(t, approved)
					assertFreshCaptureAbandoned(t, store, approved)
				}
				return run, "approved canonical path changed", assert
			},
		},
		{
			name: "patch_move_src_target",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				root, link, real1, real2 := rebindingLifecycleFixture(t)
				dst := filepath.Join(root, "new.txt")
				refusalFlip(store, 2, func() { rebindLink(t, link, real2) })
				run := func() error {
					_, err := execPatch(t, root, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, "*** Update File: link.txt\n*** Move to: new.txt\n@@\n-before\n+after")})
					return err
				}
				assert := func(t *testing.T, err error) {
					assertFileContent(t, dst, "after")
					assertFileContent(t, real1, "before")
					assertFileContent(t, real2, "before")
					assertPartialCommitted(t, err, "M new.txt")
					testassert.Require(t, len(store.discards) == 1, "discards = %v, want exactly one discard: the refused source capture", store.discards)
					assertEntryPresent(t, store, dst)
					assertEntryDiscarded(t, store, real1)
					testassert.Require(t, len(store.identityRecords) == 1, "identity records = %+v, want only the completed destination record", store.identityRecords)
					assertSnapshotIdentityContent(t, store.identityRecords, store.entryIDFor(dst), "after")
				}
				return run, "approved canonical path changed", assert
			},
		},
		{
			// Reused-entry sibling of patch_move_src_target: the seed write
			// created the group's entry for real1, so the refused source
			// capture reuses it — the entry must survive with the prior
			// proof restored while the completed destination stays recorded.
			name: "patch_move_src_reused_target",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				root, link, real1, real2 := rebindingLifecycleFixture(t)
				dst := filepath.Join(root, "new.txt")
				_, err := execWrite(t, root, "", "", CapabilityOptions{}, store, map[string]any{"path": link, "content": "first"})
				testassert.NoError(t, err)
				refusalFlip(store, 3, func() { rebindLink(t, link, real2) })
				run := func() error {
					_, err := execPatch(t, root, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, "*** Update File: link.txt\n*** Move to: new.txt\n@@\n-first\n+after")})
					return err
				}
				assert := func(t *testing.T, err error) {
					assertFileContent(t, dst, "after")
					assertFileContent(t, real1, "first")
					assertFileContent(t, real2, "before")
					assertPartialCommitted(t, err, "M new.txt")
					assertReusedCapturePreserved(t, store, real1, "first")
					assertEntryPresent(t, store, dst)
					testassert.Require(t, len(store.identityRecords) == 2, "identity records = %+v, want the seed write plus the completed destination", store.identityRecords)
					assertSnapshotIdentityContent(t, store.identityRecords, store.entryIDFor(dst), "after")
				}
				return run, "approved canonical path changed", assert
			},
		},
	}

	// Workspace-root rebinding rows.
	for _, op := range []struct {
		name     string
		seed     func(t *testing.T, realA, realB string)
		input    func() string
		flipCall int
		assert   func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, err error)
	}{
		{
			name: "patch_update", seed: func(t *testing.T, realA, realB string) {
				testassert.NoError(t, os.WriteFile(filepath.Join(realA, "file.txt"), []byte("hi"), 0o644))
				testassert.NoError(t, os.WriteFile(filepath.Join(realB, "file.txt"), []byte("secret"), 0o644))
			},
			input:    func() string { return "*** Update File: file.txt\n@@\n-hi\n+after" },
			flipCall: 1,
			assert: func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, _ error) {
				assertFileContent(t, filepath.Join(realA, "file.txt"), "hi")
				assertFileContent(t, filepath.Join(realB, "file.txt"), "secret")
				assertFreshCaptureAbandoned(t, store, filepath.Join(realA, "file.txt"))
			},
		},
		{
			name: "patch_add", seed: func(*testing.T, string, string) {},
			input:    func() string { return "*** Add File: new.txt\n+after" },
			flipCall: 1,
			assert: func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, _ error) {
				assertPathMissing(t, filepath.Join(realA, "new.txt"))
				assertPathMissing(t, filepath.Join(realB, "new.txt"))
				assertFreshCaptureAbandoned(t, store, filepath.Join(realA, "new.txt"))
			},
		},
		{
			name: "patch_delete", seed: func(t *testing.T, realA, realB string) {
				testassert.NoError(t, os.WriteFile(filepath.Join(realA, "gone.txt"), []byte("doomed"), 0o644))
				testassert.NoError(t, os.WriteFile(filepath.Join(realB, "gone.txt"), []byte("secret"), 0o644))
			},
			input:    func() string { return "*** Delete File: gone.txt" },
			flipCall: 1,
			assert: func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, _ error) {
				assertFileContent(t, filepath.Join(realA, "gone.txt"), "doomed")
				assertFileContent(t, filepath.Join(realB, "gone.txt"), "secret")
				assertFreshCaptureAbandoned(t, store, filepath.Join(realA, "gone.txt"))
			},
		},
		{
			name: "patch_move_dst", seed: func(t *testing.T, realA, _ string) {
				testassert.NoError(t, os.WriteFile(filepath.Join(realA, "src.txt"), []byte("old content"), 0o644))
			},
			input:    func() string { return "*** Update File: src.txt\n*** Move to: new.txt\n@@\n-old content\n+NEW" },
			flipCall: 1,
			assert: func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, _ error) {
				assertFileContent(t, filepath.Join(realA, "src.txt"), "old content")
				assertPathMissing(t, filepath.Join(realA, "new.txt"))
				assertPathMissing(t, filepath.Join(realB, "new.txt"))
				assertFreshCaptureAbandoned(t, store, filepath.Join(realA, "new.txt"))
			},
		},
		{
			name: "patch_move_src", seed: func(t *testing.T, realA, _ string) {
				testassert.NoError(t, os.WriteFile(filepath.Join(realA, "old.txt"), []byte("before"), 0o644))
			},
			input:    func() string { return "*** Update File: old.txt\n*** Move to: new.txt\n@@\n-before\n+after" },
			flipCall: 2,
			assert: func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, err error) {
				assertFileContent(t, filepath.Join(realA, "new.txt"), "after")
				assertFileContent(t, filepath.Join(realA, "old.txt"), "before")
				assertPathMissing(t, filepath.Join(realB, "old.txt"))
				assertPartialCommitted(t, err, "M new.txt")
				testassert.Require(t, len(store.discards) == 1, "discards = %v, want exactly one discard: the refused source capture", store.discards)
				assertEntryPresent(t, store, filepath.Join(realA, "new.txt"))
				assertEntryDiscarded(t, store, filepath.Join(realA, "old.txt"))
				testassert.Require(t, len(store.identityRecords) == 1, "identity records = %+v, want only the completed destination record", store.identityRecords)
			},
		},
	} {
		cases = append(cases, rebindingCase{
			name: op.name + "_root",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				base := t.TempDir()
				realA := filepath.Join(base, "realA")
				realB := filepath.Join(base, "realB")
				for _, dir := range []string{realA, realB} {
					testassert.NoError(t, os.Mkdir(dir, 0o755))
				}
				op.seed(t, realA, realB)
				rootLink := filepath.Join(base, "root")
				testassert.NoError(t, os.Symlink(realA, rootLink))
				refusalFlip(store, op.flipCall, func() { rebindLink(t, rootLink, realB) })
				run := func() error {
					_, err := execPatch(t, rootLink, "", "", CapabilityOptions{}, store, map[string]any{"input": applyPatchInput(t, op.input())})
					return err
				}
				assert := func(tt *testing.T, err error) { op.assert(tt, store, realA, realB, err) }
				return run, "approved canonical workspace root changed", assert
			},
		})
	}

	// Write/edit producers revalidated against a flipped Workspace root.
	for _, tool := range []struct {
		name string
		run  func(t *testing.T, store *recordingCodeGroupStore, rootLink string) error
	}{
		{name: "write", run: func(t *testing.T, store *recordingCodeGroupStore, rootLink string) error {
			_, err := execWrite(t, rootLink, "", "", CapabilityOptions{}, store, map[string]any{"path": "file.txt", "content": "after"})
			return err
		}},
		{name: "edit", run: func(t *testing.T, store *recordingCodeGroupStore, rootLink string) error {
			_, err := execEdit(t, rootLink, "", "", CapabilityOptions{}, store, map[string]any{"path": "file.txt", "old_string": "hi", "new_string": "after"})
			return err
		}},
	} {
		cases = append(cases, rebindingCase{
			name: tool.name + "_root",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				base := t.TempDir()
				realA := filepath.Join(base, "realA")
				realB := filepath.Join(base, "realB")
				for _, dir := range []string{realA, realB} {
					testassert.NoError(t, os.Mkdir(dir, 0o755))
				}
				testassert.NoError(t, os.WriteFile(filepath.Join(realA, "file.txt"), []byte("hi"), 0o644))
				testassert.NoError(t, os.WriteFile(filepath.Join(realB, "file.txt"), []byte("secret"), 0o644))
				rootLink := filepath.Join(base, "root")
				testassert.NoError(t, os.Symlink(realA, rootLink))
				refusalFlip(store, 1, func() { rebindLink(t, rootLink, realB) })
				run := func() error { return tool.run(t, store, rootLink) }
				assert := func(t *testing.T, _ error) {
					assertFileContent(t, filepath.Join(realA, "file.txt"), "hi")
					assertFileContent(t, filepath.Join(realB, "file.txt"), "secret")
					assertFreshCaptureAbandoned(t, store, filepath.Join(realA, "file.txt"))
				}
				return run, "approved canonical workspace root changed", assert
			},
		})
	}

	// Write-dir rebinding rows.
	for _, op := range []struct {
		name     string
		seed     func(t *testing.T, realA, realB string)
		input    func(realA string) string
		flipCall int
		assert   func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, err error)
	}{
		{
			name: "patch_update", seed: func(t *testing.T, realA, realB string) {
				testassert.NoError(t, os.WriteFile(filepath.Join(realA, "file.txt"), []byte("hi"), 0o644))
				testassert.NoError(t, os.WriteFile(filepath.Join(realB, "file.txt"), []byte("secret"), 0o644))
			},
			input: func(realA string) string {
				return "*** Update File: " + filepath.Join(realA, "file.txt") + "\n@@\n-hi\n+after"
			},
			flipCall: 1,
			assert: func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, _ error) {
				assertFileContent(t, filepath.Join(realA, "file.txt"), "hi")
				assertFileContent(t, filepath.Join(realB, "file.txt"), "secret")
				assertFreshCaptureAbandoned(t, store, filepath.Join(realA, "file.txt"))
			},
		},
		{
			name: "patch_add", seed: func(*testing.T, string, string) {},
			input:    func(realA string) string { return "*** Add File: " + filepath.Join(realA, "new.txt") + "\n+after" },
			flipCall: 1,
			assert: func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, _ error) {
				assertPathMissing(t, filepath.Join(realA, "new.txt"))
				assertPathMissing(t, filepath.Join(realB, "new.txt"))
				assertFreshCaptureAbandoned(t, store, filepath.Join(realA, "new.txt"))
			},
		},
		{
			name: "patch_delete", seed: func(t *testing.T, realA, realB string) {
				testassert.NoError(t, os.WriteFile(filepath.Join(realA, "gone.txt"), []byte("doomed"), 0o644))
				testassert.NoError(t, os.WriteFile(filepath.Join(realB, "gone.txt"), []byte("secret"), 0o644))
			},
			input:    func(realA string) string { return "*** Delete File: " + filepath.Join(realA, "gone.txt") },
			flipCall: 1,
			assert: func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, _ error) {
				assertFileContent(t, filepath.Join(realA, "gone.txt"), "doomed")
				assertFileContent(t, filepath.Join(realB, "gone.txt"), "secret")
				assertFreshCaptureAbandoned(t, store, filepath.Join(realA, "gone.txt"))
			},
		},
		{
			name: "patch_move_dst", seed: func(t *testing.T, realA, _ string) {
				testassert.NoError(t, os.WriteFile(filepath.Join(realA, "src.txt"), []byte("old content"), 0o644))
			},
			input: func(realA string) string {
				return "*** Update File: " + filepath.Join(realA, "src.txt") + "\n*** Move to: " + filepath.Join(realA, "new.txt") + "\n@@\n-old content\n+NEW"
			},
			flipCall: 1,
			assert: func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, _ error) {
				assertFileContent(t, filepath.Join(realA, "src.txt"), "old content")
				assertPathMissing(t, filepath.Join(realA, "new.txt"))
				assertPathMissing(t, filepath.Join(realB, "new.txt"))
				assertFreshCaptureAbandoned(t, store, filepath.Join(realA, "new.txt"))
			},
		},
		{
			name: "patch_move_src", seed: func(t *testing.T, realA, realB string) {
				testassert.NoError(t, os.WriteFile(filepath.Join(realA, "old.txt"), []byte("before"), 0o644))
				testassert.NoError(t, os.WriteFile(filepath.Join(realB, "old.txt"), []byte("secret"), 0o644))
			},
			input: func(realA string) string {
				return "*** Update File: " + filepath.Join(realA, "old.txt") + "\n*** Move to: " + filepath.Join(realA, "new.txt") + "\n@@\n-before\n+after"
			},
			flipCall: 2,
			assert: func(t *testing.T, store *recordingCodeGroupStore, realA, realB string, err error) {
				assertFileContent(t, filepath.Join(realA, "new.txt"), "after")
				assertFileContent(t, filepath.Join(realA, "old.txt"), "before")
				assertFileContent(t, filepath.Join(realB, "old.txt"), "secret")
				assertPartialCommitted(t, err, "M "+filepath.Join(realA, "new.txt"))
				testassert.Require(t, len(store.discards) == 1, "discards = %v, want exactly one discard: the refused source capture", store.discards)
				assertEntryPresent(t, store, filepath.Join(realA, "new.txt"))
				assertEntryDiscarded(t, store, filepath.Join(realA, "old.txt"))
				testassert.Require(t, len(store.identityRecords) == 1, "identity records = %+v, want only the completed destination record", store.identityRecords)
			},
		},
	} {
		cases = append(cases, rebindingCase{
			name: op.name + "_write_dir",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				base := t.TempDir()
				realA := filepath.Join(base, "realA")
				realB := filepath.Join(base, "realB")
				for _, dir := range []string{realA, realB} {
					testassert.NoError(t, os.Mkdir(dir, 0o755))
				}
				wd := filepath.Join(base, "wd")
				testassert.NoError(t, os.Symlink(realA, wd))
				op.seed(t, realA, realB)
				wdCanonical, err := bindWriteDir(base, "apply_patch", wd)
				testassert.NoError(t, err)
				refusalFlip(store, op.flipCall, func() { rebindLink(t, wd, realB) })
				run := func() error {
					_, err := execPatch(t, base, "", wdCanonical, CapabilityOptions{WriteDir: wd}, store, map[string]any{"input": applyPatchInput(t, op.input(realA))})
					return err
				}
				assert := func(tt *testing.T, err error) { op.assert(tt, store, realA, realB, err) }
				return run, "approved canonical write_dir changed", assert
			},
		})
	}

	// Write/edit producers revalidated against a flipped write-dir witness.
	for _, tool := range []struct {
		name string
		run  func(t *testing.T, store *recordingCodeGroupStore, base, realA, wd string) error
	}{
		{name: "write", run: func(t *testing.T, store *recordingCodeGroupStore, base, realA, wd string) error {
			_, err := execWrite(t, base, "", "", CapabilityOptions{WriteDir: wd}, store, map[string]any{"path": filepath.Join(realA, "file.txt"), "content": "after"})
			return err
		}},
		{name: "edit", run: func(t *testing.T, store *recordingCodeGroupStore, base, realA, wd string) error {
			_, err := execEdit(t, base, "", "", CapabilityOptions{WriteDir: wd}, store, map[string]any{"path": filepath.Join(realA, "file.txt"), "old_string": "hi", "new_string": "after"})
			return err
		}},
	} {
		cases = append(cases, rebindingCase{
			name: tool.name + "_write_dir",
			build: func(t *testing.T, store *recordingCodeGroupStore) (func() error, string, func(*testing.T, error)) {
				base := t.TempDir()
				realA := filepath.Join(base, "realA")
				realB := filepath.Join(base, "realB")
				for _, dir := range []string{realA, realB} {
					testassert.NoError(t, os.Mkdir(dir, 0o755))
				}
				wd := filepath.Join(base, "wd")
				testassert.NoError(t, os.Symlink(realA, wd))
				testassert.NoError(t, os.WriteFile(filepath.Join(realA, "file.txt"), []byte("hi"), 0o644))
				testassert.NoError(t, os.WriteFile(filepath.Join(realB, "file.txt"), []byte("secret"), 0o644))
				refusalFlip(store, 1, func() { rebindLink(t, wd, realB) })
				run := func() error { return tool.run(t, store, base, realA, wd) }
				assert := func(t *testing.T, _ error) {
					assertFileContent(t, filepath.Join(realA, "file.txt"), "hi")
					assertFileContent(t, filepath.Join(realB, "file.txt"), "secret")
					assertFreshCaptureAbandoned(t, store, filepath.Join(realA, "file.txt"))
				}
				return run, "approved canonical write_dir changed", assert
			},
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newRecordingCodeGroupStore(t)
			run, expect, assertOutcome := tc.build(t, store)
			err := run()
			testassert.Require(t, err != nil && strings.Contains(err.Error(), expect), "Execute error = %v, want %q refusal", err, expect)
			assertOutcome(t, err)
		})
	}
}
