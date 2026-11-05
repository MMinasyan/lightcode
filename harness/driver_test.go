// Real execution fixtures: behavioral rows drive the actual runExecution
// owner over an admitted Operation and observe durable state. No fixture
// constructs the private execution owner or calls one of its effects alone.
package harness

import (
	"context"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// executeDirect drives one already-admitted Operation's real execution and
// complete retirement with real owned control and graph use, without
// installing a public run slot.
func executeDirect(h *Harness, c *coordinator, execCtx context.Context, operationID string, prepared PreparedExecution) error {
	release := h.retainGraph(c)
	// The direct fixture owns a real child interval exactly like startExecution:
	// the execution context drives the opener/effects/streams, and the owner
	// cancels it at retirement while the supplied parent stays live.
	child, cancel := context.WithCancel(execCtx)
	control := &activeExecution{done: make(chan struct{}), execCtx: child, cancel: cancel}
	return h.runExecution(c, control, operationID, prepared, release)
}

// preparedExecutionFor wraps one test-built Execution as the production
// preparation output over an already-admitted Operation.
func preparedExecutionFor(capture ExecutionCapture, open func(context.Context, OperationAdmission) (Execution, error)) PreparedExecution {
	return PreparedExecution{Capture: capture, Open: open}
}

// defaultInvocation fills the fixtures' ordinary opened-invocation defaults: a
// model that completes without calls, an empty tool plan and the pure
// normalizer. Deliberately malformed opener values are never defaulted.
func defaultInvocation(inv InvocationExecution) InvocationExecution {
	if inv.Model == nil {
		inv.Model = func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}
	}
	if inv.Tool == nil {
		inv.Tool = func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} }
	}
	if inv.NormalizeTool == nil {
		inv.NormalizeTool = objectNormalize
	}
	return inv
}

// executionForInvocation wraps one directly supplied invocation contract as
// the Execution whose production opener returns it.
func executionForInvocation(inv InvocationExecution) Execution {
	inv = defaultInvocation(inv)
	return Execution{OpenInvocation: func(context.Context, InvocationRequest) (InvocationExecution, error) {
		return inv, nil
	}}
}

// toolCallResults returns the canonical settled tool results for the given
// calls, in call order.
func toolCallResults(t *testing.T, store *graphStorage, sessionID string, calls []model.ToolCall) []toolResultEntry {
	t.Helper()
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	index := make(map[string]*toolResultEntry, len(calls))
	for i := range graph.Entries {
		if result := graph.Entries[i].ToolResult; result != nil {
			index[result.ToolCallID] = result
		}
	}
	out := make([]toolResultEntry, 0, len(calls))
	for _, call := range calls {
		result, ok := index[call.ID]
		testassert.Require(t, ok, "call %q produced no canonical tool result", call.ID)
		out = append(out, *result)
	}
	return out
}

// firstResponseModel wraps one conversation model function with the fixture's
// first-response input: the first conversation request answers with the given
// calls, and the supplied continuation handles every later request. A nil
// continuation completes later turns without calls.
func firstResponseModel(continuation func(context.Context, model.Request) (model.Stream, error), calls []model.ToolCall) func(context.Context, model.Request) (model.Stream, error) {
	first := true
	return func(ctx context.Context, req model.Request) (model.Stream, error) {
		if first {
			first = false
			return completedTurnStream(calls...), nil
		}
		if continuation != nil {
			return continuation(ctx, req)
		}
		return completedTurnStream(), nil
	}
}

// runToolCalls drives one real admitted execution whose first conversation
// turn emits exactly the given calls; later turns complete without calls. It
// returns the execution error and the canonical settled results.
func runToolCalls(t *testing.T, h *Harness, c *coordinator, store *graphStorage, sessionID string, execCtx context.Context, operationID string, exec Execution, capture ExecutionCapture, calls []model.ToolCall) ([]toolResultEntry, error) {
	t.Helper()
	open := exec.OpenInvocation
	exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
		var inv InvocationExecution
		var err error
		if open != nil {
			inv, err = open(ctx, request)
			if err != nil {
				return inv, err
			}
		}
		if request.Purpose == InvocationPurposeConversation {
			inv.Model = firstResponseModel(nil, calls)
		}
		return defaultInvocation(inv), nil
	}
	prepared := preparedExecutionFor(capture, func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})
	runErr := executeDirect(h, c, execCtx, operationID, prepared)
	return toolCallResults(t, store, sessionID, calls), runErr
}

// settleAdmitted runs one real admitted execution whose conversation completes
// without calls, settling the Operation through the actual owner.
func settleAdmitted(t *testing.T, h *Harness, c *coordinator, operationID string) {
	t.Helper()
	prepared := preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
		return effectExecution(func(context.Context, model.Request) (model.Stream, error) {
			return completedTurnStream(), nil
		}, nil), nil
	})
	testassert.NoError(t, executeDirect(h, c, h.ctx, operationID, prepared))
}

// freshToolBoundary admits a new Session/Operation under the given capture and
// drives one real conversation turn emitting call through the supplied
// invocation contract, returning the canonical settled result.
func freshToolBoundary(t *testing.T, capture ExecutionCapture, inv InvocationExecution, call model.ToolCall) (*Harness, *graphStorage, string, toolResultEntry, error) {
	t.Helper()
	h, store, sessionID, results, runErr := freshToolCalls(t, capture, inv, call)
	if len(results) == 0 {
		return h, store, sessionID, toolResultEntry{}, runErr
	}
	return h, store, sessionID, results[0], runErr
}

// freshToolCalls is freshToolBoundary over an ordered call batch.
func freshToolCalls(t *testing.T, capture ExecutionCapture, inv InvocationExecution, calls ...model.ToolCall) (*Harness, *graphStorage, string, []toolResultEntry, error) {
	t.Helper()
	admission := preparedExecutionFor(capture, func(context.Context, OperationAdmission) (Execution, error) {
		return Execution{}, nil
	})
	h, store, c, sessionID := newAdmittedHarness(t, context.Background(), admission)
	results, runErr := runToolCalls(t, h, c, store, sessionID, context.Background(), testOpID, executionForInvocation(inv), capture, calls)
	return h, store, sessionID, results, runErr
}

// freshExecutionToolCalls admits one Operation under capture and drives one
// real conversation turn emitting calls through the given Execution.
func freshExecutionToolCalls(t *testing.T, capture ExecutionCapture, exec Execution, calls ...model.ToolCall) (*Harness, *graphStorage, *coordinator, string, []toolResultEntry, error) {
	t.Helper()
	return freshExecutionToolCallsCtx(t, context.Background(), capture, exec, calls...)
}

// freshExecutionToolCallsCtx is freshExecutionToolCalls under an explicit
// execution context.
func freshExecutionToolCallsCtx(t *testing.T, execCtx context.Context, capture ExecutionCapture, exec Execution, calls ...model.ToolCall) (*Harness, *graphStorage, *coordinator, string, []toolResultEntry, error) {
	t.Helper()
	return freshExecutionToolCallsConfig(t, execCtx, capture, exec, nil, calls...)
}

// freshExecutionToolCallsConfig is freshExecutionToolCallsCtx with a
// pre-execution store configuration hook for named entry/register faults.
func freshExecutionToolCallsConfig(t *testing.T, execCtx context.Context, capture ExecutionCapture, exec Execution, configure func(*graphStorage), calls ...model.ToolCall) (*Harness, *graphStorage, *coordinator, string, []toolResultEntry, error) {
	t.Helper()
	h, store, c, sessionID, err := freshExecutionRunConfig(t, execCtx, capture, exec, configure, calls...)
	if err != nil {
		return h, store, c, sessionID, nil, err
	}
	return h, store, c, sessionID, toolCallResults(t, store, sessionID, calls), nil
}

// freshExecutionRunConfig admits and runs one call batch without requiring a
// canonical result for every call (named intent/result fault rows).
func freshExecutionRunConfig(t *testing.T, execCtx context.Context, capture ExecutionCapture, exec Execution, configure func(*graphStorage), calls ...model.ToolCall) (*Harness, *graphStorage, *coordinator, string, error) {
	t.Helper()
	admission := preparedExecutionFor(capture, func(context.Context, OperationAdmission) (Execution, error) {
		return Execution{}, nil
	})
	h, store, c, sessionID := newAdmittedHarness(t, context.Background(), admission)
	if configure != nil {
		configure(store)
	}
	open := exec.OpenInvocation
	exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
		inv, err := open(ctx, request)
		if err != nil {
			return inv, err
		}
		if request.Purpose == InvocationPurposeConversation {
			inv.Model = firstResponseModel(inv.Model, calls)
		}
		return inv, nil
	}
	prepared := preparedExecutionFor(capture, func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})
	return h, store, c, sessionID, executeDirect(h, c, execCtx, testOpID, prepared)
}

// runAdmittedExecution drives one real execution over an already-admitted
// Operation with the given Execution and returns the first conversation model
// request it dispatched, plus the execution error.
func runAdmittedExecution(t *testing.T, h *Harness, c *coordinator, execCtx context.Context, operationID string, capture ExecutionCapture, exec Execution) (model.Request, error) {
	t.Helper()
	var sent model.Request
	open := exec.OpenInvocation
	exec.OpenInvocation = func(ctx context.Context, request InvocationRequest) (InvocationExecution, error) {
		inv, err := open(ctx, request)
		if err != nil {
			return inv, err
		}
		if request.Purpose == InvocationPurposeConversation {
			inner := inv.Model
			inv.Model = func(ctx context.Context, req model.Request) (model.Stream, error) {
				if sent.Messages == nil {
					sent = req
				}
				return inner(ctx, req)
			}
		}
		return inv, nil
	}
	prepared := preparedExecutionFor(capture, func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})
	return sent, executeDirect(h, c, execCtx, operationID, prepared)
}

// runAutomaticMaintenance drives one real conversation execution over the
// already-admitted Operation "op-1" with the given Execution under a
// background context: its request source runs the execution's selected
// strategy exactly as a production automatic round does, then the conversation
// model dispatches.
func runAutomaticMaintenance(t *testing.T, h *Harness, c *coordinator, exec Execution) error {
	t.Helper()
	return runAutomaticMaintenanceCtx(t, h, c, context.Background(), exec)
}

// runAutomaticMaintenanceCtx is runAutomaticMaintenance under an explicit
// execution context.
func runAutomaticMaintenanceCtx(t *testing.T, h *Harness, c *coordinator, execCtx context.Context, exec Execution) error {
	t.Helper()
	return executeDirect(h, c, execCtx, testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	}))
}

// runManualCompact admits one real compact Operation through the reserved
// admission body and drives its manual execution synchronously with the real
// owner, returning the run's error verbatim.
func runManualCompact(t *testing.T, h *Harness, c *coordinator, sessionID, operationID string) error {
	t.Helper()
	return runManualCompactCtx(t, h, c, context.Background(), sessionID, operationID)
}

// runManualCompactCtx is runManualCompact under an explicit execution context.
// It executes the PreparedExecution the reserved admission actually returned,
// so its reservation release and cleanup follow the production owner.
func runManualCompactCtx(t *testing.T, h *Harness, c *coordinator, execCtx context.Context, sessionID, operationID string) error {
	t.Helper()
	release, err := h.reserve(context.Background(), c)
	testassert.NoError(t, err)
	admitted, admitting, disposition, err := h.admitReserved(context.Background(), c, admissionRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Kind:        RequestKindCompact,
	})
	release()
	testassert.NoError(t, err)
	testassert.Require(t, disposition == DispositionAdmitted && admitting != nil && admitted.Admission.RequestKind == RequestKindCompact,
		"compact admission = %+v disposition %q, want a new compact Operation", admitted.Admission, disposition)
	return executeDirect(h, c, execCtx, operationID, *admitting)
}

// newCompactHarness builds a Harness over the store whose every preparation
// returns the given prepared execution, used by the manual-compaction rows.
func newCompactHarness(t *testing.T, store *graphStorage, prepared PreparedExecution) *Harness {
	t.Helper()
	return newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
		return prepared, nil
	})
}

// compactPrepared wraps one test-built Execution as the compact preparation
// under the given admitted capture.
func compactPrepared(capture ExecutionCapture, exec Execution) PreparedExecution {
	return PreparedExecution{Capture: capture, Open: func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	}}
}
