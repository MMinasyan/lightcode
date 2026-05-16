package permission

import (
	"context"
	"testing"
	"time"
)

func TestGateCancelAllResolvesAllPending(t *testing.T) {
	gate := NewGate(nil)
	ctx := context.Background()

	results := make(chan ResponseAction, 3)
	for i := 0; i < 3; i++ {
		go func() {
			results <- gate.AskRequest(ctx, Request{ToolName: "write_file", Arg: "{}"})
		}()
	}

	waitForPending(t, gate, 3)
	gate.CancelAll()

	for i := 0; i < 3; i++ {
		select {
		case got := <-results:
			if got != ResponseDeny {
				t.Fatalf("result %d = %q, want deny", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("pending request %d was not resolved by CancelAll", i)
		}
	}

	gate.mu.Lock()
	pending := len(gate.pending)
	gate.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending = %d, want 0", pending)
	}
}

func TestGateCancelAllMakesResponsesStale(t *testing.T) {
	idCh := make(chan string, 1)
	gate := NewGate(func(req Request) { idCh <- req.ID })

	result := make(chan ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(context.Background(), Request{ToolName: "write_file", Arg: "{}"})
	}()
	waitForPending(t, gate, 1)
	id := <-idCh

	gate.CancelAll()
	if err := gate.RespondAction(id, string(ResponseAllow)); err == nil {
		t.Fatal("RespondAction succeeded for a request cancelled by CancelAll")
	}
	if got := <-result; got != ResponseDeny {
		t.Fatalf("AskRequest = %q, want deny", got)
	}
}

func TestGateAskRequestPrefersQueuedResponseOverCancelledContext(t *testing.T) {
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		idCh := make(chan string, 1)
		gate := NewGate(func(req Request) { idCh <- req.ID })

		result := make(chan ResponseAction, 1)
		go func() {
			result <- gate.AskRequest(ctx, Request{ToolName: "write_file", Arg: "{}"})
		}()
		waitForPending(t, gate, 1)
		id := <-idCh

		if err := gate.RespondAction(id, string(ResponseAllow)); err != nil {
			t.Fatal(err)
		}
		cancel()

		select {
		case got := <-result:
			if got != ResponseAllow {
				t.Fatalf("iteration %d: AskRequest = %q, want allow", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: AskRequest did not return", i)
		}
	}
}

func TestGateAskRequestCancelWithoutResponseDeniesAndCleansPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	gate := NewGate(nil)

	result := make(chan ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(ctx, Request{ToolName: "write_file", Arg: "{}"})
	}()
	waitForPending(t, gate, 1)

	cancel()
	select {
	case got := <-result:
		if got != ResponseDeny {
			t.Fatalf("AskRequest = %q, want deny", got)
		}
	case <-time.After(time.Second):
		t.Fatal("AskRequest did not return after cancellation")
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
