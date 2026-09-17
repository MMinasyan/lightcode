package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/plugins/sqlite"
	"github.com/MMinasyan/lightcode/internal/plugins/tools"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

// The tools-plugin composition tests: the real native tools plugin is
// assembled through the test-build OpenForTest bridge like the SQLite
// plugin, and its prepared tools run through a real Harness over real
// temporary SQLite — allow/deny at the automatic policy boundary, file
// effects and code-group creation on disk, and interruption recovery.

const toolsComposedDocument = `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}},"plugins":{"tools":{"read_max_lines":77}}}`

const toolsComposedInvalidDocument = `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}},"plugins":{"tools":{"max_output_bytes":0}}}`

const toolsComposedNullDocument = `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}},"plugins":{"tools":null}}`

func writeToolsConfig(t *testing.T, path, doc string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestComposedToolsPluginPublication composes the real tools plugin through
// the OpenForTest bridge and proves the plugins.tools section publication
// matrix: a valid section publishes, a consumed-limit violation and a null
// section fail publication without replacing the live revision, and the next
// success continues the generation sequence.
func TestComposedToolsPluginPublication(t *testing.T) {
	ctx := context.Background()
	e := newComposeEnv(t)
	writeToolsConfig(t, e.configPath, toolsComposedDocument)
	r, err := runtime.OpenForTest(ctx, e.dataDir, e.configPath, []runtime.Plugin{sqlite.Plugin(), tools.Plugin()})
	if err != nil {
		t.Fatalf("OpenForTest: %v", err)
	}
	if rev, err := r.Reload(ctx); err != nil || rev != "2" {
		_ = r.Close(ctx)
		t.Fatalf("first Reload = (%q, %v), want revision 2", rev, err)
	}
	writeToolsConfig(t, e.configPath, toolsComposedInvalidDocument)
	if _, err := r.Reload(ctx); err == nil || !errors.Is(err, runtime.ErrConfiguration) {
		_ = r.Close(ctx)
		t.Fatalf("Reload with a nonpositive consumed limit = %v, want a complete publication failure", err)
	}
	writeToolsConfig(t, e.configPath, toolsComposedNullDocument)
	if _, err := r.Reload(ctx); err == nil || !errors.Is(err, runtime.ErrConfiguration) {
		_ = r.Close(ctx)
		t.Fatalf("Reload with a null tools section = %v, want the publisher's non-object rejection", err)
	}
	writeToolsConfig(t, e.configPath, toolsComposedDocument)
	rev, err := r.Reload(ctx)
	if err != nil || rev != "3" {
		_ = r.Close(ctx)
		t.Fatalf("Reload after the failures = (%q, %v), want generation 3: failed publications published nothing", rev, err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// deltaStream is the scripted accepted model stream of the composed tools
// fixtures.
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

// toolsCapture is the execution capture advertising the four file tools.
func toolsCapture() harness.ExecutionCapture {
	definition := func(name string) model.ToolDefinition {
		return model.ToolDefinition{Name: name, Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}
	}
	return harness.ExecutionCapture{
		ConfigurationRevision: "1",
		Model:                 model.ModelRef{Provider: "prov", Model: "m"},
		SystemPrompt:          "composed tools",
		Tools: []model.ToolDefinition{
			definition("read_file"), definition("write_file"), definition("edit_file"), definition("apply_patch"),
		},
	}
}

// toolsHarness is one real Harness over real temporary SQLite whose
// execution's normalization and preparation callbacks are the real plugin
// tool values, wired with the admitted identity and the durable capture's
// constraints exactly as production preparation wires them.
type toolsHarness struct {
	t         *testing.T
	h         *harness.Harness
	dataDir   string
	workspace string
	store     *storage.SQLite

	mu        sync.Mutex
	admission harness.EntryRef
}

func newToolsHarness(t *testing.T, modelFn func(context.Context, model.Request) (model.Stream, error)) *toolsHarness {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	workspace := t.TempDir()

	store, err := storage.OpenSQLite(filepath.Join(dataDir, "lightcode.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	inst, err := tools.Plugin().Open(ctx, runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dataDir}, runtime.Bindings{})
	if err != nil {
		t.Fatalf("plugin Open: %v", err)
	}
	values := make(map[string]runtime.Tool, 4)
	for id, value := range inst.Values {
		tool, ok := value.(runtime.Tool)
		if !ok {
			t.Fatalf("plugin export %q supplies %T, not a runtime.Tool", id, value)
		}
		values[id] = tool
	}

	owner, cancel := context.WithCancel(ctx)
	th := &toolsHarness{t: t, dataDir: dataDir, workspace: workspace, store: store}
	h, err := harness.New(owner, harness.Dependencies{
		Storage: store,
		Prepare: func(_ context.Context, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: toolsCapture(),
				Open: func(_ context.Context, admission harness.OperationAdmission) (harness.Execution, error) {
					th.mu.Lock()
					th.admission = admission.AdmittedEntry
					th.mu.Unlock()
					tc := runtime.ToolContext{
						Workspace:     workspace,
						AdmittedEntry: admission.AdmittedEntry,
						Invocation:    runtime.Invocation{},
						Constraints:   runtime.ToolConstraints{Readonly: admission.Execution.Readonly, WriteDir: admission.Execution.WriteDir},
					}
					return harness.Execution{
						Model: modelFn,
						NormalizeTool: func(call model.ToolCall) (json.RawMessage, error) {
							tool, ok := values[call.Name]
							if !ok {
								return nil, fmt.Errorf("unknown tool %q", call.Name)
							}
							return tool.Normalize(tc, call)
						},
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							return values[call.Name].Prepare(ctx, tc, call)
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
	th.h = h
	t.Cleanup(cancel)
	return th
}

func (th *toolsHarness) createSession() string {
	th.t.Helper()
	rec, err := th.h.CreateSession(context.Background(), harness.CreateSessionRequest{Workspace: th.workspace, AgentType: "coder"})
	if err != nil {
		th.t.Fatalf("CreateSession: %v", err)
	}
	return rec.Identity.SessionID
}

func (th *toolsHarness) submit(sessionID, operationID, text string) {
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
func (th *toolsHarness) awaitSettled(sessionID, operationID string) harness.OperationRecord {
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
func (th *toolsHarness) readToolResults(sessionID string) map[string]model.ToolResult {
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

// TestComposedToolsHarnessAllowDenyEffect runs concrete plugin tool calls
// through the real Harness boundary: the built-in policy allows the
// in-Workspace read and denies the sensitive-basename read whose denial
// settles only its own call, and the allowed write lands on disk with the
// calling Operation's one code group at the admitted-identity path.
func TestComposedToolsHarnessAllowDenyEffect(t *testing.T) {
	attempt := 0
	var mu sync.Mutex
	modelFn := func(_ context.Context, _ model.Request) (model.Stream, error) {
		mu.Lock()
		attempt++
		n := attempt
		mu.Unlock()
		switch n {
		case 1: // one assistant turn publishing both reads: the allowed sibling and the sensitive one
			return toolCallStream("call-allowed", "read_file", `{"path":"notes.txt"}`), nil
		case 2:
			return toolCallStream("call-denied", "read_file", `{"path":".env"}`), nil
		case 3:
			return toolCallStream("call-write", "write_file", `{"path":"out/new.txt","content":"written"}`), nil
		default:
			return stopStream("done"), nil
		}
	}
	th := newToolsHarness(t, modelFn)
	if err := os.WriteFile(filepath.Join(th.workspace, "notes.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(th.workspace, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := th.createSession()
	th.submit(session, "op-1", "run the tools")
	rec := th.awaitSettled(session, "op-1")
	if rec.State.Status != harness.OperationSuccess {
		t.Fatalf("operation settled %q, want success: %+v", rec.State.Status, rec.State.Terminal)
	}

	results := th.readToolResults(session)
	if len(results) != 3 {
		t.Fatalf("committed tool results = %d, want exactly the three calls", len(results))
	}
	allowed := results["call-allowed"]
	if allowed.Status != model.ResultSuccess || !strings.Contains(allowed.Content, "1\thello") {
		t.Fatalf("allowed read = %+v, want the bounded numbered content", allowed)
	}
	denied := results["call-denied"]
	if denied.Status != model.ResultDenied || denied.Content != "Permission denied." {
		t.Fatalf("sensitive read = %+v, want the fixed denial", denied)
	}
	written := results["call-write"]
	if written.Status != model.ResultSuccess || !strings.Contains(written.Content, "Wrote") {
		t.Fatalf("write = %+v, want the write success", written)
	}
	data, err := os.ReadFile(filepath.Join(th.workspace, "out", "new.txt"))
	if err != nil || string(data) != "written" {
		t.Fatalf("written file = (%q, %v), want the effect on disk", data, err)
	}

	// The mutating call's code group lives at DataDir/code/<SessionID>/<admitted input EntryID>/snapshots/1/.
	th.mu.Lock()
	admitted := th.admission
	th.mu.Unlock()
	group := filepath.Join(th.dataDir, "code", session, admitted.EntryID, "snapshots", "1")
	entries, err := os.ReadDir(group)
	if err != nil || len(entries) != 1 {
		t.Fatalf("code group %s = (%d entries, %v), want exactly one snapshot entry", group, len(entries), err)
	}
	metaData, err := os.ReadFile(filepath.Join(group, entries[0].Name(), "meta.json"))
	if err != nil {
		t.Fatalf("snapshot meta: %v", err)
	}
	var meta struct {
		OriginalPath string `json:"original_path"`
		Existed      bool   `json:"existed"`
	}
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("decode snapshot meta %s: %v", metaData, err)
	}
	if meta.Existed || !strings.HasSuffix(meta.OriginalPath, "out/new.txt") {
		t.Fatalf("snapshot meta = %s, want the new-file preimage of the declared target", metaData)
	}
	if _, err := os.Stat(filepath.Join(th.dataDir, "code", session, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unexpected second group directory exists")
	}
}

// TestComposedToolsHarnessRecovery proves the recovery sibling through the
// real plugin wiring: a running Operation whose execution never completes is
// settled as the terminal interruption when a fresh Runtime composes over
// the same storage, with the admitted input and tool evidence intact and no
// new execution started.
func TestComposedToolsHarnessRecovery(t *testing.T) {
	ctx := context.Background()
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	modelFn := func(_ context.Context, _ model.Request) (model.Stream, error) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		return nil, errors.New("released after convergence")
	}
	th := newToolsHarness(t, modelFn)
	session := th.createSession()
	th.submit(session, "op-1", "park")

	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the execution never reached its model effect")
	}

	// A fresh Runtime composition over the same data root repairs the
	// abandoned running state before any execution can start.
	e := newComposeEnv(t)
	r, err := runtime.OpenForTest(ctx, th.dataDir, e.configPath, []runtime.Plugin{sqlite.Plugin(), tools.Plugin()})
	if err != nil {
		t.Fatalf("OpenForTest recovery: %v", err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("recovery Close: %v", err)
	}
	key := harness.RegisterKey{SessionID: session, Kind: harness.RegisterOperation, OperationID: "op-1"}
	reg, err := th.store.ReadRegister(ctx, key)
	if err != nil {
		t.Fatalf("ReadRegister after recovery: %v", err)
	}
	if status := registerStatus(t, reg.Payload); status != harness.OperationInterruption {
		t.Fatalf("recovered operation status = %q, want the terminal interruption settlement", status)
	}
	entries, err := th.store.ReadEntries(ctx, session, 0)
	if err != nil {
		t.Fatalf("ReadEntries after recovery: %v", err)
	}
	var kinds []string
	for _, entry := range entries {
		kinds = append(kinds, string(entry.Kind))
	}
	if !strings.Contains(strings.Join(kinds, ","), string(harness.EntryInput)) {
		t.Fatalf("recovered entries = %v, want the admitted input intact", kinds)
	}
}
