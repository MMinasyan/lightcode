// Whole-invocation revert traversal coverage: one call owning the skip and
// reported-skip ledgers across every supplied directory and descending turn.
package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// stageGroupCapture drives one real CodeStore capture in the given group
// directory: the current target content is captured as the preimage, content
// is written, and the post-write identity is recorded and retained.
func stageGroupCapture(t *testing.T, groupDir, target string, turn int, content []byte) {
	t.Helper()
	code := mustOpenCodeStore(t, groupDir)
	entryID, _, err := code.SnapshotResolvedEntry(turn, target, target)
	testassert.Require(t, err == nil, "SnapshotResolvedEntry(%s): %v", target, err)
	{
		err := os.WriteFile(target, content, 0o600)
		testassert.Require(t, err == nil, "write %s: %v", target, err)
	}
	{
		err := code.RecordSnapshotContent(turn, entryID, content)
		testassert.Require(t, err == nil, "RecordSnapshotContent(%s): %v", target, err)
	}
	code.RetainSnapshotEntry(turn, entryID)
}

// stageTwoGroups seeds the shared two-group prologue: an entry-new and an
// entry-old store, one file snapshotted at v0 and captured twice — the older
// group's preimage v0 with post-write identity v1, the newest group's preimage
// v1 with post-write identity v2, leaving the file at v2.
func stageTwoGroups(t *testing.T) (newest, oldest, file string) {
	t.Helper()
	root := t.TempDir()
	newest = filepath.Join(root, "entry-new")
	oldest = filepath.Join(root, "entry-old")
	file = filepath.Join(root, "tree", "app.txt")
	testassert.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	testassert.NoError(t, os.WriteFile(file, []byte("v0"), 0o600))
	stageGroupCapture(t, oldest, file, 1, []byte("v1"))
	stageGroupCapture(t, newest, file, 1, []byte("v2"))
	return newest, oldest, file
}

// TestRevertCodeGroupsSharesSkipLedgerAcrossGroups proves the one-invocation
// ledger: the newest group's externally-changed identity is skipped once and
// the older group's snapshot of the same canonical identity is never restored,
// so the external content survives the whole traversal.
func TestRevertCodeGroupsSharesSkipLedgerAcrossGroups(t *testing.T) {
	newest, oldest, file := stageTwoGroups(t)

	// The external edit matches the older group's last-write identity, so only
	// the shared ledger can keep the older group from restoring.
	testassert.NoError(t, os.WriteFile(file, []byte("v1"), 0o600))

	result, err := RevertCodeGroups([]string{newest, oldest}, 0)
	testassert.NoError(t, err)
	testassert.Require(t, len(result.Restored) == 0 && len(result.Skipped) == 1 && result.Skipped[0].Path == file, "restored = %v, want no restore after the newer skip; skipped = %+v, want exactly one reported skip for %s", result.Restored, result.Skipped, file)
	data, err := os.ReadFile(file)
	testassert.Require(t, err == nil && string(data) == "v1", "file = (%q, %v), want the external v1 untouched", data, err)
}

// TestRevertCodeGroupsRestoresAcrossGroupsInOrder proves the shared ledger
// blocks only skipped identities: an intact chain restores the newest group's
// preimage and then the older group's preimage, in that order.
func TestRevertCodeGroupsRestoresAcrossGroupsInOrder(t *testing.T) {
	newest, oldest, file := stageTwoGroups(t)

	result, err := RevertCodeGroups([]string{newest, oldest}, 0)
	testassert.NoError(t, err)
	testassert.Require(t, len(result.Restored) == 2 && result.Restored[0] == file && result.Restored[1] == file, "restored = %v, want the newest then the older preimage", result.Restored)
	data, err := os.ReadFile(file)
	testassert.Require(t, err == nil && string(data) == "v0", "file = (%q, %v), want the oldest preimage v0", data, err)
	{
		_, err := os.Stat(filepath.Join(newest, "snapshots", "1"))
		testassert.Require(t, errors.Is(err, os.ErrNotExist), "newest turn dir = %v, want it removed after the restore", err)
	}
	{
		_, err := os.Stat(filepath.Join(oldest, "snapshots", "1"))
		testassert.Require(t, errors.Is(err, os.ErrNotExist), "older turn dir = %v, want it removed after the restore", err)
	}
}

// TestRevertCodeGroupsPartialFailurePreservesPriorEffects proves the honest
// partial-result contract across directories: the newest group's restore
// survives an older group's traversal failure, and the accumulated result
// rides the error.
func TestRevertCodeGroupsPartialFailurePreservesPriorEffects(t *testing.T) {
	newest, oldest, file := stageTwoGroups(t)

	badEntry := filepath.Join(oldest, "snapshots", "1", hashString(file))
	testassert.NoError(t, os.MkdirAll(badEntry, 0o700))
	testassert.NoError(t, os.WriteFile(filepath.Join(badEntry, "meta.json"), []byte("{"), 0o600))

	result, err := RevertCodeGroups([]string{newest, oldest}, 0)
	testassert.Require(t, err != nil, "RevertCodeGroups = %+v, want the older group's traversal error", result)
	testassert.Require(t, len(result.Restored) == 1 && result.Restored[0] == file, "restored = %v, want the newest group's honest restore", result.Restored)
	data, err := os.ReadFile(file)
	testassert.Require(t, err == nil && string(data) == "v1", "file = (%q, %v), want the newest group's preimage v1", data, err)
	{
		_, err := os.Stat(filepath.Join(newest, "snapshots", "1"))
		testassert.Require(t, errors.Is(err, os.ErrNotExist), "newest turn dir = %v, want it removed after its restore", err)
	}
	{
		_, err := os.Stat(filepath.Join(oldest, "snapshots", "1"))
		testassert.NoError(t, err)
	}
}

// TestRevertCodeGroupsAfterTurnAppliesPerDirectory proves after_turn is each
// directory's own turn floor: turns at or below it stay untouched in every
// group while the turns above it restore.
func TestRevertCodeGroupsAfterTurnAppliesPerDirectory(t *testing.T) {
	newest, oldest, file := stageTwoGroups(t)

	stageGroupCapture(t, newest, file, 2, []byte("v3"))

	result, err := RevertCodeGroups([]string{newest, oldest}, 1)
	testassert.NoError(t, err)
	testassert.Require(t, len(result.Restored) == 1 && result.Restored[0] == file, "restored = %v, want only the newest group's turn-2 preimage", result.Restored)
	data, err := os.ReadFile(file)
	testassert.Require(t, err == nil && string(data) == "v2", "file = (%q, %v), want the turn-2 preimage v2", data, err)
	{
		_, err := os.Stat(filepath.Join(newest, "snapshots", "1"))
		testassert.NoError(t, err)
	}
	{
		_, err := os.Stat(filepath.Join(oldest, "snapshots", "1"))
		testassert.NoError(t, err)
	}
}
