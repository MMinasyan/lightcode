package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApplyTurnActionRevertCodeUsesClickedTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	path := filepath.Join(a.projectRoot, "created.txt")

	appendUserTurn(t, a, "first")
	clickedTurn := appendUserTurnWithSnapshot(t, a, "create file", path, "created\n")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected created file before revert: %v", err)
	}

	result, err := a.ApplyTurnAction(clickedTurn, TurnActionRevertCode, false)
	if err != nil {
		t.Fatalf("ApplyTurnAction returned error: %v", err)
	}

	if result.TargetTurn != clickedTurn-1 {
		t.Fatalf("TargetTurn = %d, want %d", result.TargetTurn, clickedTurn-1)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file still exists after revert code; stat err=%v", err)
	}
	if got := userContents(a.SessionMessages()); !equalStrings(got, []string{"first", "create file"}) {
		t.Fatalf("history changed after code-only revert: %q", got)
	}
}

func TestApplyTurnActionRevertHistoryWithCodeUsesClickedTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	path := filepath.Join(a.projectRoot, "created.txt")

	appendUserTurn(t, a, "first")
	clickedTurn := appendUserTurnWithSnapshot(t, a, "create file", path, "created\n")
	appendUserTurn(t, a, "after")

	result, err := a.ApplyTurnAction(clickedTurn, TurnActionRevertHistory, true)
	if err != nil {
		t.Fatalf("ApplyTurnAction returned error: %v", err)
	}

	if result.TargetTurn != clickedTurn-1 {
		t.Fatalf("TargetTurn = %d, want %d", result.TargetTurn, clickedTurn-1)
	}
	if result.Prefill != "create file" {
		t.Fatalf("Prefill = %q, want selected user message", result.Prefill)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file still exists after history+code revert; stat err=%v", err)
	}
	if got := userContents(a.SessionMessages()); !equalStrings(got, []string{"first"}) {
		t.Fatalf("history after revert = %q, want only first turn", got)
	}
}

func TestApplyTurnActionForkIncludesClickedTurn(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	appendUserTurn(t, a, "first")
	clickedTurn := appendUserTurn(t, a, "fork point")
	appendUserTurn(t, a, "after")
	beforeID := a.SessionCurrent().ID

	result, err := a.ApplyTurnAction(clickedTurn, TurnActionFork, false)
	if err != nil {
		t.Fatalf("ApplyTurnAction returned error: %v", err)
	}

	if result.TargetTurn != clickedTurn {
		t.Fatalf("TargetTurn = %d, want %d", result.TargetTurn, clickedTurn)
	}
	if result.Session.ID == "" || result.Session.ID == beforeID {
		t.Fatalf("fork session ID = %q, before = %q", result.Session.ID, beforeID)
	}
	if got := userContents(a.SessionMessages()); !equalStrings(got, []string{"first", "fork point"}) {
		t.Fatalf("fork history = %q, want selected turn included", got)
	}
}

func TestSendQueuedMessagesReturnsAllTurnNumbers(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	a.lp.SetClient(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result, err := a.SendQueuedMessages(ctx, []string{"first", "second", "third"})
	if err != nil {
		t.Fatalf("SendQueuedMessages returned error: %v", err)
	}

	if len(result.Appended) != 2 {
		t.Fatalf("appended len = %d, want 2", len(result.Appended))
	}
	if result.Appended[0].Turn != 1 || result.Appended[1].Turn != 2 || result.Started.Turn != 3 {
		t.Fatalf("queued turns = %#v, started = %#v; want 1,2,3", result.Appended, result.Started)
	}
	cancel()
	waitUntilIdle(t, a)
	if got := userContents(a.SessionMessages()); !equalStrings(got, []string{"first", "second", "third"}) {
		t.Fatalf("queued history = %q, want all user turns", got)
	}
}

func appendUserTurn(t *testing.T, a *Agent, content string) int {
	t.Helper()
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	a.lp.AppendUserMessage(turn, content)
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete returned error: %v", err)
	}
	return turn
}

func appendUserTurnWithSnapshot(t *testing.T, a *Agent, content, path, after string) int {
	t.Helper()
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession returned error: %v", err)
	}
	turn := a.store.BeginTurn()
	if err := a.store.Snapshot(turn, path); err != nil {
		t.Fatalf("Snapshot returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatalf("write changed file: %v", err)
	}
	a.lp.AppendUserMessage(turn, content)
	if err := a.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete returned error: %v", err)
	}
	return turn
}

func userContents(messages []DisplayMessage) []string {
	var out []string
	for _, msg := range messages {
		if msg.Type == "user" {
			out = append(out, msg.Content)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func waitUntilIdle(t *testing.T, a *Agent) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !a.Busy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent did not become idle")
}
