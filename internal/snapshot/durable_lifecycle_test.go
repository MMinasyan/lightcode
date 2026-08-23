package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

func newTestStoreAtRoot(t *testing.T, root string) *Store {
	t.Helper()
	store, err := NewForSessionsRoot(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginNewSession(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestArchiveSessionClassifiesWriteAndParentSyncFailures(t *testing.T) {
	root := t.TempDir()
	store := newTestStoreAtRoot(t, root)
	id := store.SessionID()
	metaPath := filepath.Join(root, id, "meta.json")
	original, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected metadata write failure")
	atomicfs.SyncFileFunc = func(*os.File) error { return injected }
	if err := ArchiveSession(root, id); err == nil || errors.As(err, new(*CommittedMutationError)) {
		t.Fatalf("metadata write error = %v, want plain precommit error", err)
	}
	atomicfs.SyncFileFunc = nil
	after, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, original) {
		t.Fatal("metadata changed after a failed archive write")
	}

	var syncCalls int
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == filepath.Dir(metaPath) {
			syncCalls++
			if syncCalls == 2 {
				return errors.New("injected archive parent sync failure")
			}
		}
		return nil
	}
	err = ArchiveSession(root, id)
	var committed *CommittedMutationError
	if !errors.As(err, &committed) {
		t.Fatalf("archive parent sync error = %v, want committed error", err)
	}
	meta, err := LoadSessionMeta(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.State != StateArchived {
		t.Fatalf("state after committed archive error = %q, want archived", meta.State)
	}
	atomicfs.SyncDirFunc = nil
	if err := ArchiveSession(root, id); err != nil {
		t.Fatalf("already archived archive: %v", err)
	}
	successRoot := t.TempDir()
	successStore := newTestStoreAtRoot(t, successRoot)
	if err := ArchiveSession(successRoot, successStore.SessionID()); err != nil {
		t.Fatalf("successful archive: %v", err)
	}
	successMeta, err := LoadSessionMeta(successRoot, successStore.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if successMeta.State != StateArchived {
		t.Fatalf("successful archive state = %q, want archived", successMeta.State)
	}
}

func TestDeleteSessionClassifiesPreRenameAndPostRenameFailures(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-delete-durable"
	root := filepath.Join(projectsRoot, projectID, "sessions")
	store := newTestStoreAtRoot(t, root)
	id := store.SessionID()
	store.Detach()

	deleting := filepath.Join(projectsRoot, projectID, ".deleting")
	if err := os.WriteFile(deleting, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSession(root, id); err == nil {
		t.Fatal("delete with blocked staging parent returned nil")
	}
	if _, err := os.Stat(filepath.Join(root, id)); err != nil {
		t.Fatalf("source after pre-rename failure: %v", err)
	}
	_ = os.Remove(deleting)

	injected := errors.New("injected delete parent sync failure")
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == root {
			return injected
		}
		return nil
	}
	err := DeleteSession(root, id)
	var committed *CommittedMutationError
	if !errors.As(err, &committed) {
		t.Fatalf("delete parent sync error = %v, want committed error", err)
	}
	if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
		t.Fatalf("source after committed delete = %v, want absent", err)
	}
}

func TestDeleteSessionClassifiesDestinationParentAndSuccess(t *testing.T) {
	for _, row := range []struct {
		name      string
		failOn    string
		wantError bool
	}{
		{name: "source_parent", failOn: "source_parent", wantError: true},
		{name: "destination_parent", failOn: "destination_parent", wantError: true},
		{name: "success", wantError: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			projectsRoot := t.TempDir()
			projectID := "p-delete-outcome-" + row.name
			root := filepath.Join(projectsRoot, projectID, "sessions")
			store := newTestStoreAtRoot(t, root)
			id := store.SessionID()
			store.Detach()
			deletingRoot := filepath.Join(projectsRoot, projectID, ".deleting", "sessions")

			injected := errors.New("injected delete parent sync failure")
			atomicfs.SyncDirFunc = func(dir string) error {
				if row.failOn == "source_parent" && dir == root {
					return injected
				}
				if row.failOn == "destination_parent" && dir == deletingRoot {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

			err := DeleteSession(root, id)
			if row.wantError {
				var committed *CommittedMutationError
				if !errors.As(err, &committed) {
					t.Fatalf("delete %s error = %v, want committed error", row.name, err)
				}
			} else if err != nil {
				t.Fatalf("successful delete = %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
				t.Fatalf("source after %s = %v, want absent", row.name, err)
			}
			infos, err := List(root, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(infos) != 0 {
				t.Fatalf("listed sessions after %s = %+v, want none", row.name, infos)
			}
		})
	}
}

func TestSweepArchiveDeleteOutcomeTable(t *testing.T) {
	for _, row := range []struct {
		name              string
		state             string
		failure           string
		wantArchived      int
		wantDeleted       int
		wantError         bool
		wantCommitted     bool
		wantOnDelete      bool
		wantState         string
		wantSourcePresent bool
	}{
		{name: "archive_success", state: StateActive, wantArchived: 1, wantState: StateArchived, wantSourcePresent: true},
		{name: "archive_precommit", state: StateActive, failure: "archive_write", wantError: true, wantState: StateActive, wantSourcePresent: true},
		{name: "archive_committed", state: StateActive, failure: "archive_sync", wantArchived: 1, wantError: true, wantCommitted: true, wantState: StateArchived, wantSourcePresent: true},
		{name: "delete_success", state: StateArchived, wantDeleted: 1, wantOnDelete: true},
		{name: "delete_precommit", state: StateArchived, failure: "delete_rename", wantError: true, wantSourcePresent: true},
		{name: "delete_committed", state: StateArchived, failure: "delete_source_sync", wantDeleted: 1, wantError: true, wantCommitted: true, wantOnDelete: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			projectsRoot := t.TempDir()
			projectID := "p-sweep-outcome-" + row.name
			root := filepath.Join(projectsRoot, projectID, "sessions")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			id := "candidate"
			candidateDir := filepath.Join(root, id)
			if err := os.MkdirAll(candidateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			meta := SessionMeta{ID: id, State: row.state, LastActivity: 1, ArchivedAt: 1}
			if err := writeJSON(filepath.Join(candidateDir, "meta.json"), meta); err != nil {
				t.Fatal(err)
			}

			deletingAncestor := filepath.Join(projectsRoot, projectID, ".deleting")
			injected := errors.New("injected sweep outcome failure")
			var archiveSyncCalls int
			atomicfs.SyncFileFunc = nil
			atomicfs.SyncDirFunc = func(dir string) error {
				switch row.failure {
				case "archive_sync":
					if dir == candidateDir {
						archiveSyncCalls++
						if archiveSyncCalls == 2 {
							return injected
						}
					}
				case "delete_source_sync":
					if dir == root {
						return injected
					}
				}
				return nil
			}
			if row.failure == "archive_write" {
				atomicfs.SyncFileFunc = func(*os.File) error { return injected }
			}
			if row.failure == "delete_rename" {
				if err := os.WriteFile(deletingAncestor, []byte("blocked"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				atomicfs.SyncFileFunc = nil
				atomicfs.SyncDirFunc = nil
			})

			var deleted []string
			archived, deletedCount, err := SweepAllProjects(projectsRoot, LifecycleConfig{
				Enabled:                true,
				ArchiveAfterDays:       1,
				DeleteAfterArchiveDays: 1,
			}, func(deletedID string) { deleted = append(deleted, deletedID) }, nil)
			if archived != row.wantArchived || deletedCount != row.wantDeleted {
				t.Fatalf("sweep counts = %d/%d, want %d/%d", archived, deletedCount, row.wantArchived, row.wantDeleted)
			}
			if row.wantError != (err != nil) {
				t.Fatalf("sweep error = %v, want error=%v", err, row.wantError)
			}
			var committed *CommittedMutationError
			if row.wantCommitted != errors.As(err, &committed) {
				t.Fatalf("sweep committed classification = %v for %v, want %v", errors.As(err, &committed), err, row.wantCommitted)
			}
			if (len(deleted) == 1) != row.wantOnDelete {
				t.Fatalf("onDelete calls = %v, want one=%v", deleted, row.wantOnDelete)
			}
			if row.wantState != "" {
				got, loadErr := LoadSessionMeta(root, id)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if got.State != row.wantState {
					t.Fatalf("state after %s = %q, want %q", row.name, got.State, row.wantState)
				}
			}
			_, statErr := os.Stat(filepath.Join(root, id))
			if row.wantSourcePresent != (statErr == nil) {
				t.Fatalf("source presence after %s = %v (err %v), want %v", row.name, statErr == nil, statErr, row.wantSourcePresent)
			}
			if row.wantDeleted > 0 && row.wantSourcePresent {
				t.Fatal("successful or committed delete unexpectedly kept the source")
			}
		})
	}
}

func TestPublishStagedSessionSyncsCandidateTreeBeforeRename(t *testing.T) {
	base := t.TempDir()
	stagingRoot := filepath.Join(base, ".staging", "sessions", "nonce")
	finalRoot := filepath.Join(base, "sessions")
	id := "candidate1"
	candidate := filepath.Join(stagingRoot, id)
	for _, dir := range []string{
		filepath.Join(candidate, "snapshots", "1", "entry"),
		filepath.Join(candidate, "turns", "1"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(candidate, "meta.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	var synced []string
	atomicfs.SyncDirFunc = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
	if err := PublishStagedSession(stagingRoot, finalRoot, id); err != nil {
		t.Fatalf("PublishStagedSession: %v", err)
	}
	want := []string{
		filepath.Join(candidate, "snapshots", "1", "entry"),
		filepath.Join(candidate, "snapshots", "1"),
		filepath.Join(candidate, "snapshots"),
		filepath.Join(candidate, "turns", "1"),
		filepath.Join(candidate, "turns"),
		candidate,
		stagingRoot,
		finalRoot,
	}
	if !reflect.DeepEqual(synced, want) {
		t.Fatalf("sync order = %v, want %v", synced, want)
	}
}

func TestPublishStagedSessionPreRenameSyncFailureDoesNotPublish(t *testing.T) {
	base := t.TempDir()
	stagingRoot := filepath.Join(base, ".staging", "sessions", "nonce")
	finalRoot := filepath.Join(base, "sessions")
	id := "candidate2"
	candidate := filepath.Join(stagingRoot, id)
	if err := os.MkdirAll(filepath.Join(candidate, "turns"), 0o700); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected candidate tree sync failure")
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == candidate {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
	if err := PublishStagedSession(stagingRoot, finalRoot, id); !errors.Is(err, injected) {
		t.Fatalf("pre-rename error = %v, want injected error", err)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate after pre-rename failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(finalRoot, id)); !os.IsNotExist(err) {
		t.Fatalf("destination after pre-rename failure = %v, want absent", err)
	}
}

func TestSweepPropagatesArchiveAndDeleteOutcomes(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-sweep-durable"
	root := filepath.Join(projectsRoot, projectID, "sessions")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(id string, meta SessionMeta) {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
			t.Fatal(err)
		}
	}
	write("archive1", SessionMeta{ID: "archive1", State: StateActive, LastActivity: 1})
	write("delete1", SessionMeta{ID: "delete1", State: StateArchived, ArchivedAt: 1})
	cfg := LifecycleConfig{Enabled: true, ArchiveAfterDays: 1, DeleteAfterArchiveDays: 1}
	var deleted []string
	injected := errors.New("injected sweep sync failure")
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == filepath.Join(root, "archive1") {
			return nil
		}
		if dir == root {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
	archived, deletedCount, err := SweepAllProjects(projectsRoot, cfg, func(id string) { deleted = append(deleted, id) }, nil)
	if archived != 1 || deletedCount != 1 {
		t.Fatalf("sweep counts = %d/%d, want 1/1", archived, deletedCount)
	}
	if err == nil {
		t.Fatal("sweep with committed archive/delete outcome returned nil")
	}
	if !strings.Contains(err.Error(), "sweep sync failure") {
		t.Fatalf("sweep error = %v, want injected failure", err)
	}
	if !reflect.DeepEqual(deleted, []string{"delete1"}) {
		t.Fatalf("onDelete calls = %v, want delete1 only", deleted)
	}
}

func TestSweepAdmittedSerializerClaimsCommittedArchiveAndDelete(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-sweep-admitted"
	root := filepath.Join(projectsRoot, projectID, "sessions")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const archiveID = "archive-committed"
	const deleteID = "delete-committed"
	for id, meta := range map[string]SessionMeta{
		archiveID: {ID: archiveID, State: StateActive, LastActivity: 1},
		deleteID:  {ID: deleteID, State: StateArchived, ArchivedAt: 1},
	} {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
			t.Fatal(err)
		}
	}

	archiveErr := errors.New("injected admitted archive committed error")
	deleteErr := errors.New("injected admitted delete committed error")
	var serializerCalls, releaseCalls int
	var serializerClaimAvailable []string
	var releaseClaimAvailable []string
	var claimHeld []string
	var onDeleteIDs []string
	atomicfs.SyncDirFunc = func(dir string) error {
		checkHeld := func(id string) {
			claim, ok, err := AcquireSessionClaim(projectsRoot, projectID, id)
			if err != nil {
				t.Errorf("claim probe for %s: %v", id, err)
				return
			}
			if ok {
				_ = claim.Release()
				t.Errorf("claim probe for %s succeeded during committed operation", id)
				return
			}
			claimHeld = append(claimHeld, id)
		}
		if dir == filepath.Join(root, archiveID) {
			checkHeld(archiveID)
			return archiveErr
		}
		if dir == root {
			checkHeld(deleteID)
			return deleteErr
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	serializer := func() (func(), bool) {
		ids := []string{archiveID, deleteID}
		if serializerCalls >= len(ids) {
			t.Errorf("serializer called %d times, want at most %d", serializerCalls+1, len(ids))
			return nil, false
		}
		id := ids[serializerCalls]
		serializerCalls++
		claim, ok, err := AcquireSessionClaim(projectsRoot, projectID, id)
		if err != nil || !ok {
			t.Errorf("serializer claim probe for %s = ok:%v err:%v, want claim available before admission", id, ok, err)
		} else {
			_ = claim.Release()
			serializerClaimAvailable = append(serializerClaimAvailable, id)
		}
		return func() {
			releaseCalls++
			claim, ok, err := AcquireSessionClaim(projectsRoot, projectID, id)
			if err != nil || !ok {
				t.Errorf("release claim probe for %s = ok:%v err:%v, want claim released before serializer release", id, ok, err)
				return
			}
			_ = claim.Release()
			releaseClaimAvailable = append(releaseClaimAvailable, id)
		}, true
	}

	archived, deleted, err := SweepAllProjects(projectsRoot, LifecycleConfig{
		Enabled:                true,
		ArchiveAfterDays:       1,
		DeleteAfterArchiveDays: 1,
	}, func(id string) {
		onDeleteIDs = append(onDeleteIDs, id)
		if id != deleteID {
			t.Errorf("onDelete(%q), want only %q", id, deleteID)
		}
	}, serializer)
	if archived != 1 || deleted != 1 {
		t.Fatalf("sweep counts = %d/%d, want 1/1", archived, deleted)
	}
	if !errors.Is(err, archiveErr) || errors.Is(err, deleteErr) {
		t.Fatalf("sweep error = %v, want the first committed archive error only", err)
	}
	if serializerCalls != 2 || releaseCalls != 2 {
		t.Fatalf("serializer calls = %d releases = %d, want 2/2", serializerCalls, releaseCalls)
	}
	if !reflect.DeepEqual(serializerClaimAvailable, []string{archiveID, deleteID}) {
		t.Fatalf("claims available at serializer admission = %v, want archive then delete", serializerClaimAvailable)
	}
	if !reflect.DeepEqual(releaseClaimAvailable, []string{archiveID, deleteID}) {
		t.Fatalf("claims available at serializer release = %v, want archive then delete", releaseClaimAvailable)
	}
	if !reflect.DeepEqual(claimHeld, []string{archiveID, archiveID, deleteID}) {
		t.Fatalf("claim-held probes = %v, want archive write+parent and delete source parent", claimHeld)
	}
	if !reflect.DeepEqual(onDeleteIDs, []string{deleteID}) {
		t.Fatalf("onDelete calls = %v, want exactly one delete outcome", onDeleteIDs)
	}
	meta, err := LoadSessionMeta(root, archiveID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.State != StateArchived {
		t.Fatalf("archive state = %q, want archived after committed error", meta.State)
	}
	if _, err := os.Stat(filepath.Join(root, deleteID)); !os.IsNotExist(err) {
		t.Fatalf("delete source stat = %v, want absent after committed error", err)
	}
	for _, id := range []string{archiveID, deleteID} {
		claim, ok, err := AcquireSessionClaim(projectsRoot, projectID, id)
		if err != nil || !ok {
			t.Fatalf("final claim for %s = ok:%v err:%v, want released", id, ok, err)
		}
		_ = claim.Release()
	}
}
