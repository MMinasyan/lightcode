package agent

import "testing"

// TestRootOnlySnapshotAuthority verifies the drive boundary for snapshot
// listing, code/history revert, and user fork accepts a live root session and
// rejects a compact/child session and any non-live id.
func TestRootOnlySnapshotAuthority(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, "hello")
	rootID := a.store.SessionID()

	// A live root session is accepted and can list snapshots.
	if _, err := a.resolveRootDriveSession(rootID); err != nil {
		t.Fatalf("root session rejected by drive gate: %v", err)
	}
	if _, err := a.SnapshotListForSession(rootID); err != nil {
		t.Fatalf("SnapshotListForSession(root): %v", err)
	}

	setAgentTypeForTest(a, "compact")
	if _, err := a.resolveRootDriveSession(rootID); err == nil {
		t.Fatal("compact-type session accepted by root-only gate")
	}
	if _, err := a.SnapshotListForSession(rootID); err == nil {
		t.Fatal("SnapshotListForSession accepted a compact session")
	}
	if _, err := a.RevertCodeForSession(rootID, 0); err == nil {
		t.Fatal("RevertCodeForSession accepted a compact session")
	}
	if _, err := a.ApplyTurnActionForSession(rootID, 0, TurnActionRevertCode, false); err == nil {
		t.Fatal("ApplyTurnActionForSession accepted a compact session")
	}
	if err := a.ForkSessionForSession(rootID, 0); err == nil {
		t.Fatal("ForkSessionForSession accepted a compact session")
	}
	setAgentTypeForTest(a, "primary")

	// A non-live id — as any task/compact child would be, since children are
	// never registered as drivable units — is rejected before touching a store.
	if _, err := a.resolveRootDriveSession("deadbeef"); err == nil {
		t.Fatal("unknown session accepted by drive gate")
	}
}

func setAgentTypeForTest(a *Agent, agentType string) {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	a.session.activeAgentType = agentType
	rt.mu.Unlock()
}

// TestPassiveMessagesResolveOwningSession proves a passive transcript read
// targets the requested session and never falls back to the current session's
// messages for an id that is not the current one.
func TestPassiveMessagesResolveOwningSession(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, "alpha detail")

	current, err := a.SessionMessagesFor(a.store.SessionID())
	if err != nil || len(current) == 0 {
		t.Fatalf("current session read: %v (%d msgs)", err, len(current))
	}

	// A read of a session that is not live and not on disk must not leak the
	// current session's messages via a fallback.
	msgs, err := a.SessionMessagesFor("nonexistentsession")
	if err == nil && len(msgs) > 0 {
		t.Fatalf("passive read of unknown session leaked %d current messages", len(msgs))
	}
}
