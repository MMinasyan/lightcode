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
