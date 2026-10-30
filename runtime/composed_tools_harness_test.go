package runtime_test

// The external suite's shared tool fixtures: the scripted model streams, the
// one-call preparation helper, the background-job identity reader, and the
// focused task Harness fixture the composed permission row drives.

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

const (
	testSessionID = "0123456789abcdef0123456789abcdef"
	testEntryID   = "fedcba9876543210fedcba9876543210"
)

// backgroundIDPattern extracts one started background job identity from a
// retained immediate result.
var backgroundIDPattern = regexp.MustCompile("Command running in the background with ID: `([0-9a-f]{8})`")

// backgroundJobID extracts the last started background job identity from one
// model request's accumulated text.
func backgroundJobID(text string) string {
	matches := backgroundIDPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

func makeCall(t *testing.T, name, args string) model.ToolCall {
	t.Helper()
	call, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: name, Arguments: json.RawMessage(args)})
	testassert.NoError(t, err)
	return call
}

// prepare prepares one call, returning the plan.
func prepare(t *testing.T, tool runtime.Tool, tc runtime.ToolContext, name, args string) harness.PreparedTool {
	t.Helper()
	return tool.Prepare(context.Background(), tc, makeCall(t, name, args))
}

type deltaStream struct {
	deltas []model.StreamDelta
	i      int
}

func (s *deltaStream) Recv() (model.StreamDelta, error) {
	if s.i >= len(s.deltas) {
		return model.StreamDelta{}, io.EOF
	}
	d := s.deltas[s.i]
	s.i++
	return d, nil
}

func (s *deltaStream) Close() error { return nil }

func streamOf(deltas ...model.StreamDelta) model.Stream {
	return &deltaStream{deltas: deltas}
}

// toolCallStream is one completed assistant turn publishing one tool call.
func toolCallStream(id, name, args string) model.Stream {
	return streamOf(
		model.StreamDelta{HasChoice: true, ToolFragments: []model.ToolCallFragment{{ID: id, Name: name, ArgumentFragment: args}}},
		model.StreamDelta{HasChoice: true, FinishReason: "tool_calls"},
	)
}

// stopStream is one completed assistant turn with plain text and no calls.
func stopStream(text string) model.Stream {
	return streamOf(
		model.StreamDelta{HasChoice: true, ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: text}}},
		model.StreamDelta{HasChoice: true, FinishReason: "stop"},
	)
}

// taskCapture is the task fixture's execution capture: it advertises exactly
// the one composed task tool.
func taskCapture() harness.ExecutionCapture {
	return harness.ExecutionCapture{
		ConfigurationRevision: "1",
		Model:                 model.ModelRef{Provider: "prov", Model: "m"},
		ContextWindow:         4096,
		OutputReserve:         2048,
		SystemPrompt:          "composed task",
		Tools:                 []model.ToolDefinition{{Name: "task", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
		Compact: harness.CompactCapture{
			Model:        model.ModelRef{Provider: "prov", Model: "m"},
			SystemPrompt: "summarize",
		},
		Strategy: harness.StrategyCapture{ID: harness.IncludedStrategyID, RequiresContextWindow: true},
	}
}

// taskHarness is one real Harness over a real SQLite store whose execution
// advertises and drives exactly the composed task tool: the scripted model,
// the resolved permission policy, the captured invocation, and the scripted
// background bridge are the only inputs.
type taskHarness struct {
	t         *testing.T
	h         *harness.Harness
	store     harness.Storage
	workspace string
}

func newTaskHarness(t *testing.T, modelFn func(context.Context, model.Request) (model.Stream, error), policy harness.PermissionPolicy, invocation runtime.Invocation, background runtime.BackgroundServices) *taskHarness {
	t.Helper()
	workspace := t.TempDir()
	store, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "lightcode.db"))
	testassert.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	task := openTask(t)
	owner, cancel := context.WithCancel(context.Background())
	h, err := harness.New(owner, harness.Dependencies{
		Storage: store,
		Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: taskCapture(),
				Open: func(_ context.Context, admission harness.OperationAdmission) (harness.Execution, error) {
					tc := runtime.ToolContext{
						Workspace:     workspace,
						AdmittedEntry: admission.AdmittedEntry,
						Invocation:    invocation,
						Constraints:   runtime.ToolConstraints{Readonly: admission.Execution.Readonly, WriteDir: admission.Execution.WriteDir},
						Background:    background,
					}
					return harness.Execution{
						Permissions: policy,
						OpenInvocation: func(context.Context, harness.InvocationRequest) (harness.InvocationExecution, error) {
							return harness.InvocationExecution{
								Model:         modelFn,
								NormalizeTool: func(call model.ToolCall) (json.RawMessage, error) { return task.Normalize(tc, call) },
								Tool: func(ctx context.Context, call model.ToolCall) harness.PreparedTool {
									return task.Prepare(ctx, tc, call)
								},
							}, nil
						},
					}, nil
				},
			}, nil
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("harness.New: %v", err)
	}
	th := &taskHarness{t: t, h: h, store: store, workspace: workspace}
	t.Cleanup(cancel)
	return th
}

func (th *taskHarness) createSession() string {
	th.t.Helper()
	rec, err := th.h.CreateSession(context.Background(), harness.CreateSessionRequest{Workspace: th.workspace, AgentType: "coder"})
	if err != nil {
		th.t.Fatalf("CreateSession: %v", err)
	}
	return rec.Identity.SessionID
}

func (th *taskHarness) submit(sessionID, operationID, text string) {
	th.t.Helper()
	res, err := th.h.Submit(context.Background(), harness.SubmitRequest{
		SessionID: sessionID, OperationID: operationID, Origin: harness.InputOriginUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: text}}, Mode: harness.MessageModeRegular,
	})
	if err != nil || res.Disposition != harness.DispositionAdmitted {
		th.t.Fatalf("Submit = (%+v, %v), want admission", res, err)
	}
}

// awaitSettled waits for the operation's terminal settlement and returns it.
func (th *taskHarness) awaitSettled(sessionID, operationID string) harness.OperationRecord {
	th.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rec, err := th.h.ReadOperation(context.Background(), sessionID, operationID)
		if err != nil {
			th.t.Fatalf("ReadOperation: %v", err)
		}
		switch rec.State.Status {
		case harness.OperationSuccess, harness.OperationFailure, harness.OperationInterruption:
			return rec
		}
		if time.Now().After(deadline) {
			th.t.Fatalf("operation %q never settled: %+v", operationID, rec.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// readToolResults decodes every committed tool_result entry of one session,
// keyed by tool call ID.
func (th *taskHarness) readToolResults(sessionID string) map[string]model.ToolResult {
	th.t.Helper()
	entries, err := th.store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		th.t.Fatalf("ReadEntries: %v", err)
	}
	results := make(map[string]model.ToolResult)
	for _, entry := range entries {
		if entry.Kind != harness.EntryToolResult {
			continue
		}
		var wire struct {
			ToolCallID string                 `json:"tool_call_id"`
			Status     model.ToolResultStatus `json:"status"`
			Content    string                 `json:"content"`
			Metadata   json.RawMessage        `json:"metadata"`
		}
		if err := json.Unmarshal(entry.Payload, &wire); err != nil {
			th.t.Fatalf("decode tool_result payload: %v", err)
		}
		results[wire.ToolCallID] = model.ToolResult{CallID: wire.ToolCallID, Status: wire.Status, Content: wire.Content}
	}
	return results
}
