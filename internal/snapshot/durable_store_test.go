package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

func TestBeginChildSessionSyncFailureReleasesClaimAndLeavesDirectory(t *testing.T) {
	for _, row := range []struct {
		name string
		fail func(root, dir string) bool
	}{
		{
			name: "child_directory",
			fail: func(root, dir string) bool {
				return dir != root && filepath.Dir(dir) == root
			},
		},
		{
			name: "sessions_root",
			fail: func(root, dir string) bool { return dir == root },
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			projectsRoot := t.TempDir()
			projectID := "p-child-durable-" + row.name
			root := filepath.Join(projectsRoot, projectID, "sessions")
			store, err := NewForSessionsRoot(root, projectsRoot, projectID)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected child sync failure")
			atomicfs.SyncDirFunc = func(dir string) error {
				if row.fail(root, dir) {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

			err = store.BeginChildSession(t.TempDir(), "parent1")
			var committed *CommittedMutationError
			if !errors.As(err, &committed) {
				t.Fatalf("child sync error = %v, want committed error", err)
			}
			if store.Active() {
				t.Fatal("store active after child sync failure")
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || !entries[0].IsDir() {
				t.Fatalf("child directory after sync failure = %v, want one complete directory", entries)
			}
			id := entries[0].Name()
			if _, err := os.Stat(filepath.Join(root, id, "meta.json")); err != nil {
				t.Fatalf("child metadata after sync failure: %v", err)
			}
			claim, ok, err := AcquireSessionClaim(projectsRoot, projectID, id)
			if err != nil || !ok {
				t.Fatalf("child claim after sync failure: ok=%v err=%v, want released", ok, err)
			}
			_ = claim.Release()
		})
	}
}

func TestBeginChildSessionSyncOrderAndActivation(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-child-order"
	root := filepath.Join(projectsRoot, projectID, "sessions")
	store, err := NewForSessionsRoot(root, projectsRoot, projectID)
	if err != nil {
		t.Fatal(err)
	}
	var synced []string
	atomicfs.SyncDirFunc = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
	if err := store.BeginChildSession(t.TempDir(), "parent1"); err != nil {
		t.Fatal(err)
	}
	childDir := store.Dir()
	if len(synced) < 3 {
		t.Fatalf("sync calls = %v, want metadata write and child/parent proof", synced)
	}
	if got := synced[len(synced)-2:]; !reflect.DeepEqual(got, []string{childDir, root}) {
		t.Fatalf("child activation sync tail = %v, want child then root", got)
	}
	if !store.Active() {
		t.Fatal("child store inactive after successful durability proof")
	}
}

func TestRevertHistoryClassifiesTurnDirectorySyncFailure(t *testing.T) {
	store := newTestStore(t)
	seedTurns(t, store, 3)
	turnsDir := store.turnsDir
	injected := errors.New("injected turns directory sync failure")
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == turnsDir {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	outcome, err := store.RevertHistory(1)
	var committed *CommittedMutationError
	if !errors.As(err, &committed) {
		t.Fatalf("turn sync error = %v, want committed error", err)
	}
	if got := store.CurrentTurn(); got != 2 {
		t.Fatalf("current turn after first successful removal = %d, want 2", got)
	}
	if !outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != 2 {
		t.Fatalf("outcome = %+v, want changed/known/current 2", outcome)
	}
	if got := readIntDirs(turnsDir); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("turn dirs after sync failure = %v, want [1 2]", got)
	}
}
