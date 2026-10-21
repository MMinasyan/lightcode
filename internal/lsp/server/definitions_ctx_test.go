package server

// The context-aware install runner tests: cancellation of the owner lifetime
// kills a running install promptly and returns the cancellation error, while
// the five-minute wall-clock timeout shape is preserved.

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestRunWithTimeoutContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sleep", "30")

	done := make(chan error, 1)
	go func() { done <- runWithTimeout(ctx, cmd, 5*time.Minute) }()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		testassert.Require(t, errors.Is(err, context.Canceled), "runWithTimeout after cancellation = %v, want context.Canceled", err)
	case <-time.After(5 * time.Second):
		t.Fatal("runWithTimeout never returned after the lifetime was canceled")
	}
}

func TestRunWithTimeoutPreservesDeadline(t *testing.T) {
	// The wall-clock timeout survives the context threading: a short derived
	// deadline kills the process and returns the deadline error.
	start := time.Now()
	err := runWithTimeout(context.Background(), exec.Command("sleep", "30"), 100*time.Millisecond)
	elapsed := time.Since(start)
	testassert.Require(t, errors.Is(err, context.DeadlineExceeded), "runWithTimeout past the deadline = %v, want context.DeadlineExceeded", err)
	testassert.Require(t, elapsed < 5*time.Second, "runWithTimeout took %s, want a prompt deadline kill", elapsed)
}
