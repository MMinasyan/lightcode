package tool

import (
	"context"
	"encoding/json"
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

func (Sleep) NormalizeArguments(args json.RawMessage) (json.RawMessage, error) {
	var params map[string]any
	if err := json.Unmarshal(args, &params); err != nil {
		return args, err
	}
	v, ok := params["seconds"].(float64)
	if !ok {
		return args, nil
	}
	sec := normalizeSleepSeconds(int(v))
	if float64(sec) == v {
		return args, nil
	}
	params["seconds"] = sec
	data, err := json.Marshal(params)
	if err != nil {
		return args, err
	}
	return data, nil
}

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
