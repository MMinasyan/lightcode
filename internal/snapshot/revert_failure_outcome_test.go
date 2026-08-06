package snapshot

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestRevertFailureOutcomeContract asserts the history-removal walk's failure
// outcome: RevertHistory walks turn dirs descending, stops at the first failed
// removal, reports where it stopped, and lowers the recorded truncation point
// only as far as removal actually reached. The load path that reads complete
// turns applies no upper bound — it scans completion markers directly — so a
// surviving turn directory above the recorded truncation point would be
// re-read on the next load and the reverted turn would come back after a
// reload. The failure is injected at one turn directory through filesystem
// permissions: the turn dir is unwritable, so RemoveAll fails exactly there,
// midway through the walk.
func TestRevertFailureOutcomeContract(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	store := newTestStore(t)
	for i := 0; i < 5; i++ {
		turn := store.BeginTurn()
		mustAppendMessage(t, store, turn, `{"role":"user","content":"msg"}`)
		if err := store.MarkTurnComplete(turn); err != nil {
			t.Fatal(err)
		}
	}

	// Revert to turn 2 removes turns 3, 4 and 5. Block the removal of turn 4
	// so the walk removes turn 5, fails at turn 4, and must stop there.
	blocked := filepath.Join(store.turnsDir, "4")
	if err := os.Chmod(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(blocked, 0o700) }()

	_, err := store.RevertHistory(2)
	if err == nil {
		t.Fatal("RevertHistory swallowed the blocked removal and reported success")
	}
	if !strings.Contains(err.Error(), "turn 4") {
		t.Fatalf("RevertHistory error = %q, want it to name turn 4 where removal stopped", err.Error())
	}
	// The truncation point moved only as far as removal reached: turn 4, the
	// failed turn. Everything above it is gone; it and everything below survive.
	if got := store.CurrentTurn(); got != 4 {
		t.Fatalf("CurrentTurn after partially failed revert = %d, want 4 (the failed turn)", got)
	}
	if got := readIntDirs(store.turnsDir); !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
		t.Fatalf("turn dirs after partially failed revert = %v, want [1 2 3 4]", got)
	}
	if err := os.Chmod(blocked, 0o700); err != nil {
		t.Fatal(err)
	}

	// A real reload must not resurrect the reverted turn: the load path scans
	// completion markers with no upper bound, so every complete turn it re-reads
	// must sit at or below the recorded truncation point.
	reloaded, err := NewForSessionsRoot(store.Root(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.LoadSession(store.SessionID()); err != nil {
		t.Fatalf("reload session: %v", err)
	}
	turns, err := reloaded.LoadCompleteTurns()
	if err != nil {
		t.Fatalf("reload complete turns: %v", err)
	}
	var loaded []int
	for _, t := range turns {
		loaded = append(loaded, t.Turn)
	}
	if !reflect.DeepEqual(loaded, []int{1, 2, 3, 4}) {
		t.Fatalf("reloaded turns = %v, want [1 2 3 4]: the reverted turn 5 must not come back and nothing above the recorded truncation point (4) may be re-read", loaded)
	}
}
