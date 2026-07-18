package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestWailsDeliveryFIFOEmitsFramesInOrder verifies every frame reaches the emit
// choke point through the single drainer, in append order.
func TestWailsDeliveryFIFOEmitsFramesInOrder(t *testing.T) {
	a := &App{ctx: context.Background()}
	var mu sync.Mutex
	var got []string
	a.emitFn = func(name string, _ any) {
		mu.Lock()
		got = append(got, name)
		mu.Unlock()
	}
	a.startDelivery()
	defer a.closeDelivery()

	const n = 100
	for i := 0; i < n; i++ {
		a.emitFrame(fmt.Sprintf("e%d", i), nil)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		c := len(got)
		mu.Unlock()
		if c == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drained %d of %d frames", c, n)
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < n; i++ {
		if want := fmt.Sprintf("e%d", i); got[i] != want {
			t.Fatalf("frame %d = %q, want %q (out of order)", i, got[i], want)
		}
	}
}

// TestWailsDeliveryCloseJoinsAndRejects verifies close drains and joins the
// drainer, then rejects further frames.
func TestWailsDeliveryCloseJoinsAndRejects(t *testing.T) {
	a := &App{ctx: context.Background()}
	a.emitFn = func(string, any) {}
	a.startDelivery()
	a.emitFrame("x", nil)
	a.closeDelivery()

	select {
	case <-a.deliveryDone:
	default:
		t.Fatal("drainer did not exit after close")
	}

	// The drainer has exited, so the queue is stable. A frame appended after
	// close is rejected: it does not change the queue length.
	a.deliveryMu.Lock()
	before := len(a.deliveryFrames)
	a.deliveryMu.Unlock()
	a.emitFrame("after", nil)
	a.deliveryMu.Lock()
	after := len(a.deliveryFrames)
	a.deliveryMu.Unlock()
	if after != before {
		t.Fatalf("post-close emitFrame not rejected: queue length %d -> %d", before, after)
	}
}

// TestWailsDeliveryCloseAbandonsBlockedEmit verifies close returns even when the
// drainer is blocked inside one framework emit, and that a frame queued behind
// the blocked one is not emitted after shutdown has proceeded.
func TestWailsDeliveryCloseAbandonsBlockedEmit(t *testing.T) {
	old := deliveryJoinTimeout
	deliveryJoinTimeout = 50 * time.Millisecond
	defer func() { deliveryJoinTimeout = old }()

	a := &App{ctx: context.Background()}
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var emitted []string
	a.emitFn = func(name string, _ any) {
		mu.Lock()
		first := len(emitted) == 0
		emitted = append(emitted, name)
		mu.Unlock()
		if first {
			close(entered)
			<-release // block only the first emit
		}
	}
	a.startDelivery()
	a.emitFrame("blocked", nil)
	a.emitFrame("queued_behind", nil)
	<-entered // the drainer is now blocked inside the first emit

	done := make(chan struct{})
	go func() {
		a.closeDelivery()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closeDelivery did not return; blocked drainer not abandoned")
	}

	close(release)                    // unblock the drainer
	time.Sleep(100 * time.Millisecond) // give it a chance to (wrongly) emit the queued frame

	mu.Lock()
	defer mu.Unlock()
	if len(emitted) != 1 || emitted[0] != "blocked" {
		t.Fatalf("emitted = %v, want only [blocked]: no emission after close", emitted)
	}
}
