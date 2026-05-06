package tool

import (
	"context"
	"fmt"
	"time"
)

// Sleep implements a sleep/wait tool for the model to pause before
// checking background process output or waiting for servers.
type Sleep struct{}

func (Sleep) Name() string { return "sleep" }

func (Sleep) Description() string {
	return `Wait for a specified number of seconds before continuing.
- Use this to pause before checking the output of background processes started with run_command(background=true).
- Also useful for waiting for servers, builds, or file watchers to initialize.`
}

func (Sleep) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"seconds": map[string]any{
				"type":        "integer",
				"description": "Number of seconds to wait (max 300).",
			},
		},
		"required": []string{"seconds"},
	}
}

func (Sleep) Execute(ctx context.Context, params map[string]any) (string, error) {
	sec := 1
	if v, ok := params["seconds"].(float64); ok {
		sec = int(v)
	}
	if sec < 1 {
		sec = 1
	}
	if sec > 300 {
		sec = 300
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(time.Duration(sec) * time.Second):
	}
	return fmt.Sprintf("Slept for %d seconds.", sec), nil
}
