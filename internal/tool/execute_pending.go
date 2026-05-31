package tool

import (
	"context"
	"fmt"
)

// StagedCall represents a pending tool call.
type StagedCall struct {
	ToolName   string
	ToolCallID string
	Args       string
	Params     map[string]any
}

// PendingQueue manages staged tool calls.
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

// DefaultPendingCoordinator handles explicit and automatic pending flushes.
type DefaultPendingCoordinator struct {
	executor PendingExecutor
}

// NewPendingCoordinator returns the default pending flush coordinator.
func NewPendingCoordinator(executor PendingExecutor) *DefaultPendingCoordinator {
	return &DefaultPendingCoordinator{executor: executor}
}

// ExplicitFlushToolName returns the model-visible tool name used for explicit
// pending flushes.
func (c *DefaultPendingCoordinator) ExplicitFlushToolName() string {
	return (ExecutePending{}).Name()
}

// FlushPending flushes staged calls immediately.
func (c *DefaultPendingCoordinator) FlushPending(ctx context.Context, q *PendingQueue) ([]BatchResult, bool, error) {
	return c.flush(ctx, q)
}

// AutoFlushBefore flushes before any non-explicit-flush tool.
func (c *DefaultPendingCoordinator) AutoFlushBefore(ctx context.Context, next ToolCall, q *PendingQueue) ([]BatchResult, bool, error) {
	if q == nil || q.Len() == 0 || next.Name == c.ExplicitFlushToolName() {
		return nil, false, nil
	}
	return c.flush(ctx, q)
}

// FlushAtTurnEnd flushes any remaining staged calls at turn end.
func (c *DefaultPendingCoordinator) FlushAtTurnEnd(ctx context.Context, q *PendingQueue) ([]BatchResult, bool, error) {
	if q == nil || q.Len() == 0 {
		return nil, false, nil
	}
	return c.flush(ctx, q)
}

func (c *DefaultPendingCoordinator) flush(ctx context.Context, q *PendingQueue) ([]BatchResult, bool, error) {
	if q == nil {
		return nil, false, nil
	}
	staged := q.Staged()
	q.Discard()
	if len(staged) == 0 {
		return nil, false, nil
	}
	if c == nil || c.executor == nil {
		results := make([]BatchResult, 0, len(staged))
		for _, call := range staged {
			results = append(results, BatchResult{
				ToolName:   call.ToolName,
				ToolCallID: call.ToolCallID,
				Error:      fmt.Sprintf("no pending executor configured for %q", call.ToolName),
			})
		}
		return results, true, nil
	}
	return c.executor.ExecutePending(ctx, staged), true, nil
}

// BatchResult is the outcome of a single staged call.
type BatchResult struct {
	ToolName   string
	ToolCallID string
	Success    bool
	Result     string
	Error      string
	Metadata   map[string]any
}
