package permission

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- PendingForSession snapshots only the named session's requests ---

func TestGatePendingForSession(t *testing.T) {
	gate := NewGate(func(ctx context.Context, req Request) {})

	go gate.AskRequest(context.Background(), Request{SessionID: "s1", ToolName: "write_file", Arg: "a"})
	go gate.AskRequest(context.Background(), Request{SessionID: "s2", ToolName: "write_file", Arg: "b"})
	waitForPending(t, gate, 2)

	if got := gate.PendingForSession("s1"); len(got) != 1 || got[0].SessionID != "s1" || got[0].ToolName != "write_file" {
		t.Fatalf("PendingForSession(s1) = %#v, want one s1 write_file request", got)
	}
	if got := gate.PendingForSession("nope"); len(got) != 0 {
		t.Fatalf("PendingForSession(nope) = %#v, want none", got)
	}

	gate.CancelAll()
}

// --- AskRequest happy path: Respond before ctx cancel ---

func TestGateAskRequestHappyPath(t *testing.T) {
	idCh := make(chan string, 1)
	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(context.Background(), Request{ToolName: "write_file", Arg: "src/main.go"})
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	if err := gate.RespondAction(id, string(ResponseAllow)); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-result:
		if got != ResponseAllow {
			t.Fatalf("AskRequest = %q, want allow", got)
		}
	case <-time.After(time.Second):
		t.Fatal("AskRequest did not return after Respond")
	}
}

// --- AskRequest on already-cancelled context returns deny immediately ---

func TestGateAskRequestAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	gate := NewGate(nil)
	start := time.Now()
	got := gate.AskRequest(ctx, Request{ToolName: "write_file", Arg: "src/main.go"})
	elapsed := time.Since(start)

	if got != ResponseDeny {
		t.Fatalf("AskRequest = %q, want deny", got)
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("AskRequest should return instantly for cancelled context, took %v", elapsed)
	}

	gate.mu.Lock()
	pending := len(gate.pending)
	gate.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending = %d, want 0", pending)
	}
}

// --- RespondAction before/after ctx cancellation ---

func TestGateRespondActionBeforeCancel(t *testing.T) {
	idCh := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(ctx, Request{ToolName: "read_file", Arg: "README.md"})
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	// Respond while context is still alive
	if err := gate.Respond(id, true); err != nil {
		t.Fatal(err)
	}
	cancel()

	select {
	case got := <-result:
		if got != ResponseAllow {
			t.Fatalf("got %q, want allow", got)
		}
	case <-time.After(time.Second):
		t.Fatal("AskRequest did not return")
	}
}

func TestGateSessionRespond(t *testing.T) {
	idCh := make(chan string, 1)
	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(context.Background(), Request{
			SessionID: "session-a",
			ProjectID: "project-a",
			ToolName:  "read_file",
			Arg:       "README.md",
		})
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	for _, sessionID := range []string{"session-b", ""} {
		if err := gate.RespondActionForSession(sessionID, id, string(ResponseAllow)); !errors.Is(err, ErrSessionMismatch) {
			t.Fatalf("RespondActionForSession(%q) err = %v, want session mismatch", sessionID, err)
		}
	}
	if err := gate.RespondActionForSession("session-a", id, string(ResponseDeny)); err != nil {
		t.Fatalf("RespondActionForSession matching session: %v", err)
	}

	select {
	case got := <-result:
		if got != ResponseDeny {
			t.Fatalf("AskRequest = %q, want deny", got)
		}
	case <-time.After(time.Second):
		t.Fatal("AskRequest did not return")
	}
	if err := gate.RespondActionForSession("session-a", id, string(ResponseAllow)); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("stale RespondActionForSession err = %v, want unknown request", err)
	}
}

func TestGateSessionCancel(t *testing.T) {
	gate := NewGate(nil)
	ctx := context.Background()
	type pending struct {
		id     string
		result <-chan ResponseAction
	}
	start := func(sessionID string) pending {
		result := make(chan ResponseAction, 1)
		go func() {
			result <- gate.AskRequest(ctx, Request{SessionID: sessionID, ToolName: "read_file", Arg: sessionID + ".txt"})
		}()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			gate.mu.Lock()
			for id, p := range gate.pending {
				if p.req.SessionID == sessionID {
					gate.mu.Unlock()
					return pending{id: id, result: result}
				}
			}
			gate.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("missing pending request for %q", sessionID)
		return pending{}
	}

	first := start("session-a")
	second := start("session-b")
	if cancelled := gate.CancelSession("session-a"); cancelled != 1 {
		t.Fatalf("CancelSession cancelled %d, want 1", cancelled)
	}

	select {
	case got := <-first.result:
		if got != ResponseDeny {
			t.Fatalf("first result = %q, want deny", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not resolve")
	}
	select {
	case got := <-second.result:
		t.Fatalf("second request resolved as %q, want still pending", got)
	default:
	}
	if err := gate.RespondActionForSession("session-b", second.id, string(ResponseAllow)); err != nil {
		t.Fatalf("respond second: %v", err)
	}
	select {
	case got := <-second.result:
		if got != ResponseAllow {
			t.Fatalf("second result = %q, want allow", got)
		}
	case <-time.After(time.Second):
		t.Fatal("second request did not resolve")
	}
}

func TestGateRespondActionAfterCancel(t *testing.T) {
	idCh := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())

	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(ctx, Request{ToolName: "write_file", Arg: "tmp/out.txt"})
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	// Cancel context — AskRequest should return deny from the ctx.Done branch
	cancel()

	select {
	case got := <-result:
		if got != ResponseDeny {
			t.Fatalf("got %q, want deny after cancel", got)
		}
	case <-time.After(time.Second):
		t.Fatal("AskRequest did not return after cancellation")
	}

	// Respond after context cancellation — channel may already be drained;
	// RespondAction should return an error since the pending entry was cleaned up.
	err := gate.Respond(id, true)
	if err == nil {
		// It's acceptable for Respond to succeed if the buffered channel still
		// had the value, but it should not block.
		t.Log("Respond after cancel succeeded (channel was buffered)")
	}
}

// --- Double-Respond race safety ---

func TestGateDoubleRespondRace(t *testing.T) {
	idCh := make(chan string, 1)
	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(context.Background(), Request{ToolName: "write_file", Arg: "a.txt"})
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	// First respond
	if err := gate.Respond(id, true); err != nil {
		t.Fatal("first Respond failed:", err)
	}

	// Second respond should return ErrUnknownRequest
	err := gate.Respond(id, false)
	if err == nil {
		t.Fatal("second Respond should have failed")
	}

	select {
	case got := <-result:
		if got != ResponseAllow {
			t.Fatalf("got %q, want allow from first respond", got)
		}
	case <-time.After(time.Second):
		t.Fatal("AskRequest did not return")
	}
}

// --- AskRequest returns bool correctly ---

func TestGateAskReturnsTrue(t *testing.T) {
	idCh := make(chan string, 1)
	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan bool, 1)
	go func() {
		result <- gate.Ask(context.Background(), Request{ToolName: "write_file", Arg: "a.go"}.ToolName, Request{ToolName: "write_file", Arg: "a.go"}.Arg)
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	gate.Respond(id, true)
	select {
	case got := <-result:
		if !got {
			t.Fatal("Ask should return true for allow")
		}
	case <-time.After(time.Second):
		t.Fatal("Ask did not return")
	}
}

func TestGateAskReturnsFalse(t *testing.T) {
	idCh := make(chan string, 1)
	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan bool, 1)
	go func() {
		result <- gate.Ask(context.Background(), "write_file", "a.go")
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	gate.Respond(id, false)
	select {
	case got := <-result:
		if got {
			t.Fatal("Ask should return false for deny")
		}
	case <-time.After(time.Second):
		t.Fatal("Ask did not return")
	}
}

// --- Concurrent AskRequests ---

func TestGateConcurrentAskRequests(t *testing.T) {
	gate := NewGate(nil)
	ctx := context.Background()
	n := 10

	results := make(chan ResponseAction, n)
	for i := 0; i < n; i++ {
		go func() {
			results <- gate.AskRequest(ctx, Request{ToolName: "write_file", Arg: "{}"})
		}()
	}

	waitForPending(t, gate, n)

	// Respond to all with deny (simulating CancelAll)
	gate.CancelAll()

	for i := 0; i < n; i++ {
		select {
		case got := <-results:
			if got != ResponseDeny {
				t.Fatalf("result %d = %q, want deny", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("pending request %d was not resolved", i)
		}
	}
}

// --- RespondAction with invalid action ---

func TestGateRespondActionInvalid(t *testing.T) {
	idCh := make(chan string, 1)
	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(context.Background(), Request{ToolName: "write_file", Arg: "a.go", CanAllowAll: true})
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	err := gate.RespondAction(id, "invalid_action")
	if err == nil {
		t.Fatal("RespondAction should fail for invalid action")
	}

	// The pending request should still be there (not cleaned up on invalid action)
	gate.mu.Lock()
	_, ok := gate.pending[id]
	gate.mu.Unlock()
	if !ok {
		t.Fatal("pending entry should still exist after invalid action")
	}

	// Respond properly to clean up
	gate.Respond(id, false)
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("AskRequest did not return")
	}
}

func TestGateAskRequestForgedAllowAllBecomesAllow(t *testing.T) {
	idCh := make(chan string, 1)
	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(context.Background(), Request{ToolName: "apply_patch", Arg: "a.go", CanAllowAll: false})
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	if err := gate.RespondAction(id, string(ResponseAllowAll)); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-result:
		if got != ResponseAllow {
			t.Fatalf("got %q, want allow", got)
		}
	case <-time.After(time.Second):
		t.Fatal("AskRequest did not return")
	}
}

// --- AskRequest with AllowAll ---

func TestGateAskRequestAllowAll(t *testing.T) {
	idCh := make(chan string, 1)
	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(context.Background(), Request{ToolName: "write_file", Arg: "a.go", CanAllowAll: true})
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	if err := gate.RespondAction(id, string(ResponseAllowAll)); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-result:
		if got != ResponseAllowAll {
			t.Fatalf("got %q, want allow_all", got)
		}
	case <-time.After(time.Second):
		t.Fatal("AskRequest did not return")
	}
}

// --- Ask returns true for AllowAll ---

func TestGateAskReturnsTrueForAllowAll(t *testing.T) {
	idCh := make(chan string, 1)
	gate := NewGate(func(ctx context.Context, req Request) {
		idCh <- req.ID
	})

	result := make(chan bool, 1)
	go func() {
		result <- gate.Ask(context.Background(), "write_file", "a.go")
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	gate.RespondAction(id, string(ResponseAllowAll))
	select {
	case got := <-result:
		if !got {
			t.Fatal("Ask should return true for allow_all")
		}
	case <-time.After(time.Second):
		t.Fatal("Ask did not return")
	}
}

// --- Respond to unknown ID ---

func TestGateRespondUnknownID(t *testing.T) {
	gate := NewGate(nil)
	err := gate.Respond("nonexistent-id", true)
	if err == nil {
		t.Fatal("Respond should fail for unknown id")
	}
}

// --- Race-buffered response (B5): respond from goroutine while AskRequest races ctx.Done ---

func TestGateRaceBufferedResponseB5(t *testing.T) {
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		idCh := make(chan string, 1)
		gate := NewGate(func(ctx context.Context, req Request) {
			idCh <- req.ID
		})

		result := make(chan ResponseAction, 1)
		go func() {
			result <- gate.AskRequest(ctx, Request{ToolName: "write_file", Arg: "race.go"})
		}()
		waitForPending(t, gate, 1)
		id := <-idCh

		// Cancel and respond concurrently — the buffered channel ensures no goroutine leak
		go func() {
			_ = gate.RespondAction(id, string(ResponseAllow))
		}()
		cancel()

		select {
		case got := <-result:
			if got != ResponseAllow {
				// Either allow or deny is acceptable in this race;
				// the important thing is that it terminates without deadlock.
				t.Logf("iteration %d: got %q (allow or deny acceptable in race)", i, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: AskRequest deadlocked", i)
		}
	}
}

// --- AskRequest with pre-cancelled context that has no pending entry yet ---

func TestGateAskRequestPreCancelledNoEntry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	gate := NewGate(func(ctx context.Context, req Request) {
		t.Fatal("OnRequest should not be called for pre-cancelled context")
	})

	got := gate.AskRequest(ctx, Request{ToolName: "write_file", Arg: "a.go"})
	if got != ResponseDeny {
		t.Fatalf("got %q, want deny", got)
	}

	gate.mu.Lock()
	pending := len(gate.pending)
	gate.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending = %d, want 0", pending)
	}
}

func waitForPending(t *testing.T, gate *Gate, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		got := len(gate.pending)
		gate.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	gate.mu.Lock()
	got := len(gate.pending)
	gate.mu.Unlock()
	t.Fatalf("pending = %d, want %d", got, want)
}
