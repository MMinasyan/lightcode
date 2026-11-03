package tool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
			testassert.NoError(t, err)
			testassert.Equal(t, result, tt.want)
			{
				elapsed := time.Since(start)
				testassert.Require(t, elapsed >= tt.minWait, "elapsed = %s, want at least %s", elapsed, tt.minWait)
			}
		})
	}
}

func TestSleepContextCancelReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()

	result, err := (Sleep{}).Execute(ctx, map[string]any{"seconds": float64(300)})
	testassert.Require(t, errors.Is(err, context.Canceled), "Execute result=%q error=%v, want context.Canceled", result, err)
	{
		elapsed := time.Since(start)
		testassert.Require(t, elapsed <= 100*time.Millisecond, "cancelled sleep elapsed %s, want prompt return", elapsed)
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
	testassert.Require(t, errors.Is(err, context.Canceled), "Execute error = %v, want context.Canceled", err)
	{
		elapsed := time.Since(start)
		testassert.Require(t, elapsed <= 500*time.Millisecond, "cancelled sleep elapsed %s, want prompt return", elapsed)
	}
}
