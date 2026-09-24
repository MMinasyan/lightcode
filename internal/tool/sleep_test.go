package tool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSleepCustomDurationAndClampsMinimum(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]any
		want    string
		minWait time.Duration
	}{
		{name: "custom", params: map[string]any{"seconds": float64(1)}, want: "Slept for 1 seconds.", minWait: time.Second},
		{name: "default", params: map[string]any{}, want: "Slept for 1 seconds.", minWait: time.Second},
		{name: "minimum", params: map[string]any{"seconds": float64(0)}, want: "Slept for 1 seconds.", minWait: time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			result, err := (Sleep{}).Execute(context.Background(), tt.params)
			if err != nil {
				t.Fatalf("Execute error = %v", err)
			}
			if result != tt.want {
				t.Fatalf("Execute result = %q, want %q", result, tt.want)
			}
			if elapsed := time.Since(start); elapsed < tt.minWait {
				t.Fatalf("elapsed = %s, want at least %s", elapsed, tt.minWait)
			}
		})
	}
}

func TestSleepContextCancelReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	var result string
	var err error
	go func() {
		result, err = (Sleep{}).Execute(ctx, map[string]any{"seconds": float64(300)})
		close(done)
	}()
	select {
	case <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute result=%q error=%v, want context.Canceled", result, err)
		}
	case <-time.After(10 * time.Second): // completion is the fact; the bound only turns a hung sleep into a failure
		t.Fatal("the pre-cancelled sleep never returned")
	}
}

func TestSleepContextCancelDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()

	_, err := (Sleep{}).Execute(ctx, map[string]any{"seconds": float64(10)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want context.Canceled", err)
	}
	// The sleep timer never fires early: returning under the 10-second sleep
	// proves the cancellation was observed rather than the timer expiring,
	// and the select the tool waits on makes the return prompt by semantics.
	if elapsed := time.Since(start); elapsed >= 10*time.Second {
		t.Fatalf("sleep elapsed %s, want the cancellation observed", elapsed)
	}
}
