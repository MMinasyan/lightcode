package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCodeStoreArtifactOnlyGroupReopenAndRestore proves that an isolated
// target code group can be captured, reopened, listed and restored through
// CodeStore alone, with no conversation state, Project record, claim,
// session metadata, transcript, tokens or compaction file involved.
func TestCodeStoreArtifactOnlyGroupReopenAndRestore(t *testing.T) {
	groupDir := t.TempDir()
	projectDir := t.TempDir()
	target := filepath.Join(projectDir, "notes.txt")
	original := []byte("original\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	code, err := OpenCodeStore(groupDir)
	if err != nil {
		t.Fatalf("OpenCodeStore: %v", err)
	}
	// Construction creates no directories.
	if _, err := os.Stat(filepath.Join(groupDir, "snapshots")); !os.IsNotExist(err) {
		t.Fatalf("construction created snapshots dir: stat err = %v, want not exist", err)
	}

	abs, err := filepath.Abs(target)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	entryID, created, err := code.SnapshotResolvedEntry(1, abs, abs)
	if err != nil {
		t.Fatalf("SnapshotResolvedEntry: %v", err)
	}
	if !created {
		t.Fatalf("SnapshotResolvedEntry created = false, want true")
	}
	code.RetainSnapshotEntry(1, entryID)

	mutated := []byte("mutated\n")
	if err := os.WriteFile(target, mutated, 0o644); err != nil {
		t.Fatalf("mutate target: %v", err)
	}
	if err := code.RecordSnapshotContent(1, entryID, mutated); err != nil {
		t.Fatalf("RecordSnapshotContent: %v", err)
	}

	// The group directory holds only the snapshot artifact tree: no
	// conversation, Project, claim, metadata or transcript files.
	entries, err := os.ReadDir(groupDir)
	if err != nil {
		t.Fatalf("read group dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "snapshots" {
		t.Fatalf("group dir entries = %v, want only snapshots", entries)
	}

	turns, err := code.ListTurns()
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(turns) != 1 || turns[0].Turn != 1 || len(turns[0].Files) != 1 {
		t.Fatalf("ListTurns = %+v, want one turn with one file", turns)
	}
	if turns[0].Files[0].LastWrite == nil || turns[0].Files[0].LastWrite.Hash != hashBytes(mutated) {
		t.Fatalf("last-write identity = %+v, want hash of mutated content", turns[0].Files[0].LastWrite)
	}

	// Reopen the same group with a fresh handle: listing and restore must
	// work with no conversation/Project state at all.
	reopened, err := OpenCodeStore(groupDir)
	if err != nil {
		t.Fatalf("reopen OpenCodeStore: %v", err)
	}
	reopenedTurns, err := reopened.ListTurns()
	if err != nil {
		t.Fatalf("reopened ListTurns: %v", err)
	}
	if len(reopenedTurns) != 1 || len(reopenedTurns[0].Files) != 1 {
		t.Fatalf("reopened ListTurns = %+v, want one turn with one file", reopenedTurns)
	}

	result, err := reopened.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode: %v", err)
	}
	if len(result.Restored) != 1 || result.Restored[0] != abs {
		t.Fatalf("RevertCode restored = %v, want [%s]", result.Restored, abs)
	}
	if len(result.Skipped) != 0 {
		t.Fatalf("RevertCode skipped = %+v, want none", result.Skipped)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read restored target: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("restored content = %q, want %q", got, original)
	}
	after, err := os.ReadDir(filepath.Join(groupDir, "snapshots"))
	if err != nil {
		t.Fatalf("read snapshots dir after restore: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("snapshots dir after restore = %v, want empty", after)
	}
}

// TestCodeStoreTwoGroupsRestoreIndependently proves the one-disk-group-per-
// Operation ownership boundary: two groups under one session's code dir each
// hold their own captured preimage, restoring one group through a fresh
// OpenCodeStore handle restores only its own preimage and leaves the other
// group's snapshot tree untouched, and the second group restores
// independently afterward.
func TestCodeStoreTwoGroupsRestoreIndependently(t *testing.T) {
	sessionCodeDir := t.TempDir()
	groupADir := filepath.Join(sessionCodeDir, "entry-a")
	groupBDir := filepath.Join(sessionCodeDir, "entry-b")
	projectDir := t.TempDir()
	fileA := filepath.Join(projectDir, "a.txt")
	fileB := filepath.Join(projectDir, "b.txt")
	originalA, originalB := []byte("a original\n"), []byte("b original\n")
	if err := os.WriteFile(fileA, originalA, 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(fileB, originalB, 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	// Each operation's group captures its own target's preimage; each
	// mutation records its post-write identity.
	capture := func(groupDir, target string, mutated []byte) {
		t.Helper()
		code, err := OpenCodeStore(groupDir)
		if err != nil {
			t.Fatalf("OpenCodeStore(%s): %v", groupDir, err)
		}
		entryID, created, err := code.SnapshotResolvedEntry(1, target, target)
		if err != nil || !created {
			t.Fatalf("SnapshotResolvedEntry(%s) = (%v, %v), want one created entry", groupDir, entryID, err)
		}
		code.RetainSnapshotEntry(1, entryID)
		if err := os.WriteFile(target, mutated, 0o644); err != nil {
			t.Fatalf("mutate %s: %v", target, err)
		}
		if err := code.RecordSnapshotContent(1, entryID, mutated); err != nil {
			t.Fatalf("RecordSnapshotContent(%s): %v", groupDir, err)
		}
	}
	mutatedA, mutatedB := []byte("a mutated\n"), []byte("b mutated\n")
	capture(groupADir, fileA, mutatedA)
	capture(groupBDir, fileB, mutatedB)

	// Restoring group A through a fresh handle: only A's preimage returns,
	// B's file stays mutated and B's snapshot tree stays intact.
	restoreA, err := OpenCodeStore(groupADir)
	if err != nil {
		t.Fatalf("reopen group A: %v", err)
	}
	resultA, err := restoreA.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode(A): %v", err)
	}
	if len(resultA.Restored) != 1 || resultA.Restored[0] != fileA || len(resultA.Skipped) != 0 {
		t.Fatalf("RevertCode(A) = %+v, want exactly %s restored", resultA, fileA)
	}
	gotA, err := os.ReadFile(fileA)
	if err != nil || string(gotA) != string(originalA) {
		t.Fatalf("a.txt after restore A = (%q, %v), want the original bytes", gotA, err)
	}
	gotB, err := os.ReadFile(fileB)
	if err != nil || string(gotB) != string(mutatedB) {
		t.Fatalf("b.txt after restore A = (%q, %v), want it untouched at the mutated bytes", gotB, err)
	}
	turnsB, err := mustOpenCodeStore(t, groupBDir).ListTurns()
	if err != nil || len(turnsB) != 1 || len(turnsB[0].Files) != 1 {
		t.Fatalf("group B turns after restore A = %+v err %v, want the snapshot tree intact", turnsB, err)
	}
	turnsA, err := mustOpenCodeStore(t, groupADir).ListTurns()
	if err != nil || len(turnsA) != 0 {
		t.Fatalf("group A turns after restore A = %+v err %v, want the restored turn dirs removed", turnsA, err)
	}

	// Restoring group B independently restores its own preimage.
	restoreB, err := OpenCodeStore(groupBDir)
	if err != nil {
		t.Fatalf("reopen group B: %v", err)
	}
	resultB, err := restoreB.RevertCode(0)
	if err != nil {
		t.Fatalf("RevertCode(B): %v", err)
	}
	if len(resultB.Restored) != 1 || resultB.Restored[0] != fileB || len(resultB.Skipped) != 0 {
		t.Fatalf("RevertCode(B) = %+v, want exactly %s restored", resultB, fileB)
	}
	gotB, err = os.ReadFile(fileB)
	if err != nil || string(gotB) != string(originalB) {
		t.Fatalf("b.txt after restore B = (%q, %v), want the original bytes", gotB, err)
	}
}

// mustOpenCodeStore opens one code group, failing the test on error.
func mustOpenCodeStore(t *testing.T, groupDir string) *CodeStore {
	t.Helper()
	code, err := OpenCodeStore(groupDir)
	if err != nil {
		t.Fatalf("OpenCodeStore(%s): %v", groupDir, err)
	}
	return code
}
