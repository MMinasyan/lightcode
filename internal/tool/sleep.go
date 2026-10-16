package tool

import (
	"context"
	"fmt"
	"time"
)

// Sleep implements a sleep/wait tool for the model to pause before
// checking background process output or waiting for servers.
type Sleep struct{}

func (Sleep) Execute(ctx context.Context, params map[string]any) (string, error) {
	sec := 1
	if v, ok := params["seconds"].(float64); ok {
		sec = normalizeSleepSeconds(int(v))
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(time.Duration(sec) * time.Second):
	}
	return fmt.Sprintf("Slept for %d seconds.", sec), nil
}

func normalizeSleepSeconds(sec int) int {
	if sec < 1 {
		return 1
	}
	if sec > 300 {
		return 300
	}
	return sec
}
