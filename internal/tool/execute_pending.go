package tool

import (
	"context"
	"fmt"
	"strings"
)

// StagedCall represents a pending edit_file or write_file call.
type StagedCall struct {
	ToolName   string
	ToolCallID string
	Params     map[string]any
}

// PendingQueue manages the staged execution of edits and writes.
type PendingQueue struct {
	staged []StagedCall
}

// NewPendingQueue creates an empty PendingQueue.
func NewPendingQueue() *PendingQueue {
	return &PendingQueue{}
}

// Stage adds a call to the pending queue.
func (q *PendingQueue) Stage(call StagedCall) {
	q.staged = append(q.staged, call)
}

// Discard clears all staged calls without executing.
func (q *PendingQueue) Discard() {
	q.staged = nil
}

// Len returns the number of staged calls.
func (q *PendingQueue) Len() int {
	return len(q.staged)
}

// Staged returns a copy of the staged calls.
func (q *PendingQueue) Staged() []StagedCall {
	return append([]StagedCall(nil), q.staged...)
}

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

// FormatBatchResult formats the results of a batch execution.
func FormatBatchResult(results []BatchResult) string {
	if len(results) == 0 {
		return "No pending edits to execute."
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Executed %d staged edits:\n", len(results)))
	for i, r := range results {
		if r.Error != "" {
			sb.WriteString(fmt.Sprintf("%d. %s: %s\n", i+1, r.ToolName, r.Error))
		} else {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, r.Result))
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// BatchResult is the outcome of a single staged call.
type BatchResult struct {
	ToolName   string
	ToolCallID string
	Success    bool
	Result     string
	Error      string
	Diff       string
}
