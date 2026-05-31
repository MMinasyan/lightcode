package tool

import (
	"context"
	"fmt"
)

// PendingExecutor applies staged calls at flush time.
type PendingExecutor interface {
	ExecutePending(ctx context.Context, staged []StagedCall) []BatchResult
}

// PendingCoordinator owns pending-queue flush policy.
type PendingCoordinator interface {
	ExplicitFlushToolName() string
	FlushPending(ctx context.Context, q *PendingQueue) ([]BatchResult, bool, error)
	AutoFlushBefore(ctx context.Context, next ToolCall, q *PendingQueue) ([]BatchResult, bool, error)
	FlushAtTurnEnd(ctx context.Context, q *PendingQueue) ([]BatchResult, bool, error)
}

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

// DefaultPendingCoordinator is a generic pending flush coordinator. The
// runtime supplies the model-visible explicit flush tool name.
type DefaultPendingCoordinator struct {
	explicitFlushToolName string
	executor              PendingExecutor
}

// NewPendingCoordinator returns a generic pending flush coordinator.
func NewPendingCoordinator(explicitFlushToolName string, executor PendingExecutor) *DefaultPendingCoordinator {
	return &DefaultPendingCoordinator{explicitFlushToolName: explicitFlushToolName, executor: executor}
}

// SetPendingExecutor replaces the executor used by future flushes.
func (c *DefaultPendingCoordinator) SetPendingExecutor(executor PendingExecutor) {
	if c != nil {
		c.executor = executor
	}
}

// ExplicitFlushToolName returns the model-visible tool name used for explicit
// pending flushes.
func (c *DefaultPendingCoordinator) ExplicitFlushToolName() string {
	if c == nil {
		return ""
	}
	return c.explicitFlushToolName
}

// FlushPending flushes staged calls immediately.
func (c *DefaultPendingCoordinator) FlushPending(ctx context.Context, q *PendingQueue) ([]BatchResult, bool, error) {
	return c.flush(ctx, q)
}

// AutoFlushBefore flushes before any non-explicit-flush tool.
func (c *DefaultPendingCoordinator) AutoFlushBefore(ctx context.Context, next ToolCall, q *PendingQueue) ([]BatchResult, bool, error) {
	if q == nil || q.Len() == 0 || (c != nil && c.explicitFlushToolName != "" && next.Name == c.explicitFlushToolName) {
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
