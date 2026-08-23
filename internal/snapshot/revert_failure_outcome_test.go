package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func installHistoryRemoval(t *testing.T, fn func(string) error) {
	t.Helper()
	RemoveHistoryTurnFunc = fn
	t.Cleanup(func() { RemoveHistoryTurnFunc = nil })
}

func historyTurnPath(path string) int {
	turn, _ := strconv.Atoi(filepath.Base(path))
	return turn
}

func TestRevertHistoryOutcomeDistinguishesUnchangedAndPartialFailure(t *testing.T) {
	t.Run("unchanged first failure", func(t *testing.T) {
		store := newTestStore(t)
		seedTurns(t, store, 5)
		injected := errors.New("injected removal failure")
		installHistoryRemoval(t, func(string) error { return injected })

		outcome, err := store.RevertHistory(2)
		if !errors.Is(err, injected) {
			t.Fatalf("error = %v, want injected failure", err)
		}
		if outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != 5 {
			t.Fatalf("outcome = %+v, want unchanged/known/current 5", outcome)
		}
	})

	t.Run("highest turn partial failure", func(t *testing.T) {
		store := newTestStore(t)
		seedTurns(t, store, 5)
		injected := errors.New("injected partial removal failure")
		installHistoryRemoval(t, func(path string) error {
			if historyTurnPath(path) == 5 {
				if err := os.Remove(filepath.Join(path, "messages.jsonl")); err != nil {
					return err
				}
			}
			return injected
		})

		outcome, err := store.RevertHistory(2)
		if !errors.Is(err, injected) {
			t.Fatalf("error = %v, want injected failure", err)
		}
		if !outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != 5 {
			t.Fatalf("outcome = %+v, want changed/known/current 5", outcome)
		}
		if _, err := os.Stat(filepath.Join(store.turnsDir, "5", "complete")); err != nil {
			t.Fatalf("complete marker after partial removal: %v", err)
		}
		turns, err := store.LoadCompleteTurns()
		if err != nil {
			t.Fatal(err)
		}
		if got := turns[len(turns)-1].Turn; got != 4 {
			t.Fatalf("visible highest turn = %d, want 4", got)
		}
	})

	t.Run("lower turn failure after higher removals", func(t *testing.T) {
		store := newTestStore(t)
		seedTurns(t, store, 5)
		injected := errors.New("injected lower-turn failure")
		installHistoryRemoval(t, func(path string) error {
			if historyTurnPath(path) == 4 {
				return injected
			}
			return os.RemoveAll(path)
		})

		outcome, err := store.RevertHistory(2)
		if !errors.Is(err, injected) || !outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != 4 {
			t.Fatalf("outcome = %+v, err = %v, want changed/known/current 4", outcome, err)
		}
		if got := readIntDirs(store.turnsDir); !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
			t.Fatalf("turn dirs after lower-turn failure = %v, want [1 2 3 4]", got)
		}
	})
}

func TestRevertHistoryOutcomeRecoversOrDiscardsMarkerLoss(t *testing.T) {
	t.Run("text-only valid history", func(t *testing.T) {
		store := newTestStore(t)
		seedTurns(t, store, 3)
		injected := errors.New("marker removal failure")
		installHistoryRemoval(t, func(path string) error {
			if err := os.Remove(filepath.Join(path, "complete")); err != nil {
				return err
			}
			return injected
		})

		outcome, err := store.RevertHistory(2)
		if !errors.Is(err, injected) || !outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != 3 {
			t.Fatalf("outcome = %+v, err = %v, want changed/known/current 3", outcome, err)
		}
		if _, err := os.Stat(filepath.Join(store.turnsDir, "3", "complete")); err != nil {
			t.Fatalf("recovered complete marker: %v", err)
		}
	})

	t.Run("completed tool iteration", func(t *testing.T) {
		store := newTestStore(t)
		turn := store.BeginTurn()
		if err := store.AppendMessage(turn, []byte(`{"role":"user","content":"ask"}`)); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(turn, []byte(`{"role":"assistant","content":"","tool_calls":[{"id":"call-1"}]}`)); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(turn, []byte(`{"role":"tool","content":"done","tool_call_id":"call-1"}`)); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkTurnComplete(turn); err != nil {
			t.Fatal(err)
		}
		injected := errors.New("tool marker removal failure")
		installHistoryRemoval(t, func(path string) error {
			if err := os.Remove(filepath.Join(path, "complete")); err != nil {
				return err
			}
			return injected
		})

		outcome, err := store.RevertHistory(0)
		if !errors.Is(err, injected) || !outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != turn {
			t.Fatalf("outcome = %+v, err = %v, want changed/known/current %d", outcome, err, turn)
		}
		if _, err := os.Stat(filepath.Join(store.turnsDir, strconv.Itoa(turn), "complete")); err != nil {
			t.Fatalf("recovered tool marker: %v", err)
		}
	})

	for _, row := range []struct {
		name string
		data []byte
	}{
		{name: "malformed", data: []byte("not json\n")},
		{name: "empty", data: nil},
	} {
		t.Run("unrecoverable "+row.name+" history", func(t *testing.T) {
			store := newTestStore(t)
			seedTurns(t, store, 3)
			injected := errors.New("unrecoverable marker removal failure")
			installHistoryRemoval(t, func(path string) error {
				if err := os.Remove(filepath.Join(path, "complete")); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(path, "messages.jsonl"), row.data, 0o600); err != nil {
					return err
				}
				return injected
			})

			outcome, err := store.RevertHistory(2)
			if !errors.Is(err, injected) || !outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != 2 {
				t.Fatalf("outcome = %+v, err = %v, want changed/known/current 2", outcome, err)
			}
			if _, err := os.Stat(filepath.Join(store.turnsDir, "3")); !os.IsNotExist(err) {
				t.Fatalf("unrecoverable turn after removal = %v, want gone", err)
			}
		})
	}
}

func TestRevertHistoryOutcomeDirectoryGoneAndCompactionPartial(t *testing.T) {
	t.Run("directory gone", func(t *testing.T) {
		store := newTestStore(t)
		seedTurns(t, store, 4)
		injected := errors.New("directory removal failure")
		installHistoryRemoval(t, func(path string) error {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			return injected
		})

		outcome, err := store.RevertHistory(2)
		if !errors.Is(err, injected) || !outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != 3 {
			t.Fatalf("outcome = %+v, err = %v, want changed/known/current 3", outcome, err)
		}
	})

	t.Run("compaction unlink plus first turn failure", func(t *testing.T) {
		store := newTestStore(t)
		seedTurns(t, store, 4)
		if err := store.SaveCompaction(CompactionRecord{BoundaryTurn: 4, Summary: "summary"}); err != nil {
			t.Fatal(err)
		}
		injected := errors.New("first turn failure")
		installHistoryRemoval(t, func(string) error { return injected })

		outcome, err := store.RevertHistory(2)
		if !errors.Is(err, injected) || !outcome.CompactionRemoved || !outcome.HistoryChanged || !outcome.HistoryStateKnown {
			t.Fatalf("outcome = %+v, err = %v, want compaction and changed/known", outcome, err)
		}
		if outcome.CurrentTurn != 4 {
			t.Fatalf("current turn = %d, want 4", outcome.CurrentTurn)
		}
	})
}

func TestRevertHistoryMissingAuthoritativeRowsLowersLiveAndRestartState(t *testing.T) {
	store := newTestStore(t)
	seedTurns(t, store, 3)
	id := store.SessionID()
	injected := errors.New("missing authoritative rows")
	installHistoryRemoval(t, func(path string) error {
		if err := os.Remove(filepath.Join(path, "complete")); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(path, "messages.jsonl")); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "unrelated-child"), []byte("leftover"), 0o600); err != nil {
			return err
		}
		return injected
	})

	outcome, err := store.RevertHistory(2)
	if !errors.Is(err, injected) || !outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != 2 {
		t.Fatalf("outcome = %+v err=%v, want changed/known/current 2", outcome, err)
	}
	live, err := store.LoadCompleteTurns()
	if err != nil {
		t.Fatal(err)
	}
	if got := []int{live[0].Turn, live[1].Turn}; !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("live turns = %v, want [1 2]", got)
	}
	root := store.Root()
	store.Detach()
	restarted, err := NewForSessionsRoot(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.LoadSession(id); err != nil {
		t.Fatal(err)
	}
	if got := restarted.CurrentTurn(); got != 2 {
		t.Fatalf("restart current turn = %d, want 2", got)
	}
	restartedTurns, err := restarted.LoadCompleteTurns()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live, restartedTurns) {
		t.Fatalf("live turns = %+v, restart turns = %+v", live, restartedTurns)
	}
}

func TestRevertHistoryRecoveryRewritesCanonicalUserTextBeforeToolSuffix(t *testing.T) {
	store := newTestStore(t)
	seedTurns(t, store, 3)
	turnDir := filepath.Join(store.turnsDir, "3")
	input := []byte("{\"role\":\"user\",\"content\":\"ask\"}\n" +
		"{\"role\":\"assistant\",\"content\":\"text\"}\n" +
		"{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[{\"id\":\"call-1\"}]}\n")
	if err := os.WriteFile(filepath.Join(turnDir, "messages.jsonl"), input, 0o600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("canonical recovery failure")
	installHistoryRemoval(t, func(path string) error {
		if err := os.Remove(filepath.Join(path, "complete")); err != nil {
			return err
		}
		return injected
	})

	outcome, err := store.RevertHistory(2)
	if !errors.Is(err, injected) || !outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != 3 {
		t.Fatalf("outcome = %+v err=%v, want changed/known/current 3", outcome, err)
	}
	want := []byte("{\"role\":\"user\",\"content\":\"ask\"}\n" +
		"{\"role\":\"assistant\",\"content\":\"text\"}\n")
	got, err := os.ReadFile(filepath.Join(turnDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rewritten messages = %q, want %q", got, want)
	}
	live, err := store.LoadCompleteTurns()
	if err != nil || len(live) != 3 || len(live[2].Messages) != 2 {
		t.Fatalf("live recovered turns = %+v err=%v, want three turns with canonical final turn", live, err)
	}
	root, id := store.Root(), store.SessionID()
	store.Detach()
	restarted, err := NewForSessionsRoot(root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.LoadSession(id); err != nil {
		t.Fatal(err)
	}
	restartedTurns, err := restarted.LoadCompleteTurns()
	if err != nil || !reflect.DeepEqual(live, restartedTurns) {
		t.Fatalf("live turns = %+v restart turns = %+v err=%v", live, restartedTurns, err)
	}
}

func TestRevertHistoryOutcomeInspectionFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block inspection as root")
	}

	t.Run("pre-inspection before mutation", func(t *testing.T) {
		store := newTestStore(t)
		seedTurns(t, store, 3)
		blocked := filepath.Join(store.turnsDir, "3")
		if err := os.Chmod(blocked, 0o000); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(blocked, 0o700) }()
		called := false
		installHistoryRemoval(t, func(string) error {
			called = true
			return errors.New("must not remove")
		})

		outcome, err := store.RevertHistory(2)
		if err == nil || outcome.HistoryChanged || !outcome.HistoryStateKnown || outcome.CurrentTurn != 3 || called {
			t.Fatalf("outcome = %+v err=%v called=%v, want unchanged/known/current 3 and no removal", outcome, err, called)
		}
	})

	t.Run("pre-inspection after earlier removal", func(t *testing.T) {
		store := newTestStore(t)
		seedTurns(t, store, 4)
		blocked := filepath.Join(store.turnsDir, "3")
		called := 0
		installHistoryRemoval(t, func(path string) error {
			called++
			if historyTurnPath(path) == 4 {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
				return os.Chmod(blocked, 0o000)
			}
			return nil
		})
		defer func() { _ = os.Chmod(blocked, 0o700) }()

		outcome, err := store.RevertHistory(2)
		if err == nil || !outcome.HistoryChanged || outcome.HistoryStateKnown || called != 1 {
			t.Fatalf("outcome = %+v err=%v called=%d, want changed/unknown after one removal", outcome, err, called)
		}
	})

	t.Run("post-inspection unreadable", func(t *testing.T) {
		store := newTestStore(t)
		seedTurns(t, store, 3)
		blocked := filepath.Join(store.turnsDir, "3")
		injected := errors.New("post-state unreadable")
		installHistoryRemoval(t, func(path string) error {
			if err := os.Remove(filepath.Join(path, "messages.jsonl")); err != nil {
				return err
			}
			if err := os.Chmod(path, 0o000); err != nil {
				return err
			}
			return injected
		})
		defer func() { _ = os.Chmod(blocked, 0o700) }()

		outcome, err := store.RevertHistory(2)
		if !errors.Is(err, injected) || !outcome.HistoryChanged || outcome.HistoryStateKnown {
			t.Fatalf("outcome = %+v err=%v, want changed/unknown post-state", outcome, err)
		}
	})

	t.Run("recovery input unreadable", func(t *testing.T) {
		store := newTestStore(t)
		seedTurns(t, store, 3)
		injected := errors.New("recovery input unreadable")
		installHistoryRemoval(t, func(path string) error {
			if err := os.Remove(filepath.Join(path, "complete")); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(path, "messages.jsonl")); err != nil {
				return err
			}
			if err := os.Mkdir(filepath.Join(path, "messages.jsonl"), 0o700); err != nil {
				return err
			}
			return injected
		})
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(store.turnsDir, "3", "messages.jsonl")) })

		outcome, err := store.RevertHistory(2)
		if !errors.Is(err, injected) || !outcome.HistoryChanged || outcome.HistoryStateKnown {
			t.Fatalf("outcome = %+v err=%v, want changed/unknown recovery input", outcome, err)
		}
	})
}

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
