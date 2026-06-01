package tool

import (
	"context"

	enginetool "github.com/MMinasyan/lightcode/internal/engine/tool"
)

// ExecutePending implements the execute_pending tool.
// The actual flush is handled by the loop's dispatch; this tool
// is registered for schema visibility but Execute is a no-op.
type ExecutePending struct{}

func (ExecutePending) Name() string { return "execute_pending" }

func (ExecutePending) Description() string {
	return `Execute all staged (pending) edits and writes.
- Use this tool to apply changes that were staged with pending=true.
- You can also use action: "discard" to cancel all pending edits without applying them.
- Pending edits are automatically applied when you call any non-pending tool or when your response finishes. Prefer using non-pending last edit instead of this tool.`
}

func (ExecutePending) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Action to take on pending edits: \"apply\" (default) to execute them, or \"discard\" to cancel them.",
			},
		},
		"required": []string{},
	}
}

func (ExecutePending) Execute(_ context.Context, params map[string]any) (string, error) {
	return "No pending edits to execute.", nil
}

// NewPendingCoordinator returns the default pending flush coordinator.
func NewPendingCoordinator(executor PendingExecutor) *DefaultPendingCoordinator {
	return enginetool.NewPendingCoordinator((ExecutePending{}).Name(), executor)
}
