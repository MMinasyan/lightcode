package tool

import (
	"context"
	"fmt"
)

type ProcessController interface {
	Read(id string) (string, error)
	Kill(id string) error
	List() string
}

// ProcessTool implements the process management tool for background commands.
type ProcessTool struct {
	mgr ProcessController
}

// NewProcessTool creates a process management tool.
func NewProcessTool(mgr ProcessController) *ProcessTool {
	return &ProcessTool{mgr: mgr}
}

func (*ProcessTool) Name() string { return "process" }

func (*ProcessTool) Description() string {
	return `Manage background processes started by run_command with background=true.
	- Use action "read" with id to read the output of a running background process.
	- Use action "kill" with id to terminate a background process.
	- Use action "list" to list background processes in the current session and their status.`
}

func (*ProcessTool) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Action to perform: \"read\" to get output, \"kill\" to terminate, \"list\" to list processes in the current session.",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "Process ID returned by run_command with background=true.",
			},
		},
		"required": []string{"action"},
	}
}

func (p *ProcessTool) Execute(_ context.Context, params map[string]any) (string, error) {
	action, _ := params["action"].(string)
	switch action {
	case "read":
		id, _ := params["id"].(string)
		if id == "" {
			return "", fmt.Errorf("process: id is required for read")
		}
		output, err := p.mgr.Read(id)
		if err != nil {
			return "", err
		}
		return output, nil
	case "kill":
		id, _ := params["id"].(string)
		if id == "" {
			return "", fmt.Errorf("process: id is required for kill")
		}
		if err := p.mgr.Kill(id); err != nil {
			return "", err
		}
		return fmt.Sprintf("Process %s terminated.", id), nil
	case "list":
		return p.mgr.List(), nil
	default:
		return "", fmt.Errorf("process: unknown action %q", action)
	}
}
