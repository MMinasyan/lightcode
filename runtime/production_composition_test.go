package runtime

// The cross-boundary composition suite: one combined scenario chaining the
// production-reachable boundaries (hook rewrite, reload, steering, denied
// multi-target move, later allowed call, retained preview/snapshot evidence,
// owner cancellation, restart) plus the individual gap rows the row-by-row
// audit left open (readonly git through a no-op argument hook, model-switch
// capability-ID invariance, and public Open's initial sweep cleanup). Every
// scenario runs the concrete preparation through public Open over both
// stores with isolated HOME.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	modeladapt "github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/tool"
	"github.com/MMinasyan/lightcode/model"
)

// chainAgentsDocument builds the combined scenario's agents document with the
// chained agent's system prompt as the given text: the agent selects the
// preparation hook, the argument hook, and the model adaptation, and declares
// the write tool and the patch tool.
func chainAgentsDocument(body string) string {
	return `{
  "chained": {"model": "prov/gpt-5.5", "system_prompt": "simple", "prompt": "` + body + `", "tools": ["prod_write", "apply_patch"], "capabilities": ["hook.first", "arg.rewrite", "model_adaptation"]}
}`
}

// chainPlainConfigDocument is the combined scenario's provider document: the
// configured provider pointed at the scenario's server, the hook tag the
// hookPlugin requires, and no sessions section.
func chainPlainConfigDocument(endpoint string) string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "` + endpoint + `", "api_key_env": "PRODUCTION_TEST_KEY"},
      "discovery": false,
      "models": {"gpt-5.5": {"name": "G", "context_window": 4096}}
    }
  },
  "plugins": {"hooks": {"tag": "T1"}}
}`
}

// chainCommandConfigDocument is the readonly-git row's provider document: no
// plugins section, since its composition declares no configured plugin.
func chainCommandConfigDocument(endpoint string) string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "` + endpoint + `", "api_key_env": "PRODUCTION_TEST_KEY"},
      "discovery": false,
      "models": {"m": {"name": "M", "context_window": 4096}}
    }
  }
}`
}

// chainSweepConfigDocument adds the explicit sweep thresholds and the hook
// tag to one provider document for the public-Open initial-sweep row.
func chainSweepConfigDocument(endpoint string) string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "` + endpoint + `", "api_key_env": ""},
      "discovery": false,
      "models": {"m": {"name": "M", "context_window": 4096}}
    }
  },
  "plugins": {"hooks": {"tag": "T1"}},
  "sessions": {"archive_after_days": 1, "delete_after_archive_days": 1}
}`
}

// chainModelsConfigDocument declares the two models the capability-ID row
// composes: one matching the bundled GPT-family adaptation pattern, one
// matching nothing.
func chainModelsConfigDocument(endpoint string) string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "` + endpoint + `", "api_key_env": ""},
      "discovery": false,
      "models": {
        "gpt-5.5": {"name": "G", "context_window": 4096},
        "vanilla": {"name": "V", "context_window": 4096}
      }
    }
  }
}`
}

// rewriteWriteHook is the combined scenario's ToolArgumentsHook: it replaces
// every prod_write call's arguments with its fixed rewrite, passes every
// other call through unchanged, and records every received call's identity
// and raw argument bytes.
type rewriteWriteHook struct {
	mu      sync.Mutex
	seen    []string
	replace string
}

func (h *rewriteWriteHook) BeforeTool(_ context.Context, _ Invocation, call model.ToolCall) (json.RawMessage, error) {
	h.mu.Lock()
	h.seen = append(h.seen, call.Name+" "+string(call.Arguments))
	h.mu.Unlock()
	if call.Name == "prod_write" {
		return json.RawMessage(h.replace), nil
	}
	return call.Arguments, nil
}

func (h *rewriteWriteHook) received() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// noopArgumentHook is the no-op ToolArgumentsHook: it records every received
// call's identity and raw argument bytes and returns them unchanged.
type noopArgumentHook struct {
	mu   sync.Mutex
	seen []string
}

func (h *noopArgumentHook) BeforeTool(_ context.Context, _ Invocation, call model.ToolCall) (json.RawMessage, error) {
	h.mu.Lock()
	h.seen = append(h.seen, call.Name+" "+string(call.Arguments))
	h.mu.Unlock()
	return call.Arguments, nil
}

func (h *noopArgumentHook) received() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// argumentHookPlugin declares one ToolArgumentsHook capability under id.
func argumentHookPlugin(id string, hook ToolArgumentsHook) Plugin {
	return Plugin{
		ID:       "arghooks",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[ToolArgumentsHook](id)},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{id: hook}}, nil
		},
	}
}

// bundledAdaptation is the capability-ID row's ModelAdaptation value: the
// bundled binding table's own resolver body (the shipped adaptation plugin
// package cannot be imported from in-package tests, so its resolver logic is
// composed directly over the real bundled matcher).
type bundledAdaptation struct{}

func (bundledAdaptation) Resolve(ref model.ModelRef) (Adaptation, error) {
	match := modeladapt.Match(ref.Model)
	if match == nil {
		return Adaptation{}, nil
	}
	return Adaptation{
		ExcludeTools:                match.ExcludeTools,
		IncludeTools:                match.IncludeTools,
		Blocks:                      match.Blocks,
		Additions:                   match.Additions,
		ToolDescriptionReplacements: match.ToolDescriptionReplacements,
	}, nil
}

// bundledAdaptationPlugin declares the bundled resolver as the composition's
// single ModelAdaptation export.
func bundledAdaptationPlugin() Plugin {
	return Plugin{
		ID:       "adapt",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[ModelAdaptation]("model_adaptation")},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"model_adaptation": bundledAdaptation{}}}, nil
		},
	}
}

// chainDecodeArguments strictly decodes one call's JSON arguments into an
// owned map, rejecting malformed, non-object, null and trailing data.
func chainDecodeArguments(raw json.RawMessage) (map[string]any, error) {
	var args map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&args); err != nil || args == nil {
		return nil, errors.New("arguments must be one JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("arguments must be one JSON object")
	}
	return args, nil
}

// chainImmediateError is the validation-class immediate outcome: status error
// carrying the bounded diagnostic.
func chainImmediateError(callID string, cause error) harness.PreparedTool {
	msg := cause.Error()
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return harness.PreparedTool{Immediate: &harness.ToolOutcome{
		Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: msg},
	}}
}

// chainImmediateDenied is the failed-canonical-preparation immediate outcome:
// the fixed denial.
func chainImmediateDenied(callID string) harness.PreparedTool {
	return harness.PreparedTool{Immediate: &harness.ToolOutcome{
		Result: model.ToolResult{CallID: callID, Status: model.ResultDenied, Content: "Permission denied."},
	}}
}

// chainCodeGroup adapts one per-call snapshot.CodeStore to the shared
// SnapshotStore interface, mirroring the shipped plugin's group wrapper: the
// transactional path preempts the plain methods; they are interface
// compliance only.
type chainCodeGroup struct{ *snapshot.CodeStore }

func (chainCodeGroup) CurrentTurn() int { return 1 }

func (s chainCodeGroup) Snapshot(turn int, absPath string) error {
	_, _, err := s.SnapshotResolvedEntry(turn, absPath, absPath)
	return err
}

// chainWriteTool is the combined scenario's prod_write capability: the
// shared write preparation (the per-call code group opened fresh from the
// admitted-input identity, the pre-write preimage capture, one file.write
// pair) behind the production Tool contract, mirroring the shipped plugin's
// write_file export.
type chainWriteTool struct{ dataDir string }

func (chainWriteTool) describe(_ Invocation, constraints ToolConstraints) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        "prod_write",
		Description: "writes one file in the workspace",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: !constraints.Readonly || constraints.WriteDir != ""}, nil
}

func (t chainWriteTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	args, err := chainDecodeArguments(call.Arguments)
	if err != nil {
		return nil, err
	}
	normalized, err := tool.NormalizeWriteArgs(args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func (t chainWriteTool) Prepare(_ context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	args, err := chainDecodeArguments(call.Arguments)
	if err != nil {
		return chainImmediateError(call.ID, err)
	}
	resolved, err := pathutil.ResolveFilePathFrom("", tc.Workspace)
	if err != nil {
		return chainImmediateDenied(call.ID)
	}
	writeDirCanonical := ""
	if tc.Constraints.WriteDir != "" {
		resolvedWriteDir, err := pathutil.ResolveFilePathFrom(tc.Workspace, tc.Constraints.WriteDir)
		if err != nil {
			return chainImmediateDenied(call.ID)
		}
		writeDirCanonical = resolvedWriteDir.CanonicalPath
	}
	group, err := snapshot.OpenCodeStore(filepath.Join(t.dataDir, "code", tc.AdmittedEntry.SessionID, tc.AdmittedEntry.EntryID))
	if err != nil {
		return chainImmediateDenied(call.ID)
	}
	prepared, err := tool.PrepareWriteCall(tc.Workspace, resolved.CanonicalPath, writeDirCanonical, tool.CapabilityOptions{WriteDir: tc.Constraints.WriteDir}, chainCodeGroup{group}, args)
	if err != nil {
		return chainImmediateDenied(call.ID)
	}
	pairs := make([]harness.PermissionRequest, 0, len(prepared.Targets))
	for _, target := range prepared.Targets {
		pairs = append(pairs, harness.PermissionRequest{Permission: "file.write", Target: target.CanonicalPath})
	}
	return harness.PreparedTool{
		Permissions:        pairs,
		CanonicalWorkspace: resolved.CanonicalPath,
		Execute: func(ctx context.Context) harness.ToolOutcome {
			result, execErr := prepared.Execute(ctx)
			if execErr != nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: execErr.Error()}}
			}
			return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: result}}
		},
	}
}

// chainPatchTool is the combined scenario's apply_patch capability: the
// shared patch engine (one V4A parse, the per-call code group opened fresh
// from the admitted-input identity, the same preview capture) behind the
// production Tool contract, mirroring the shipped plugin's apply_patch
// export.
type chainPatchTool struct{ dataDir string }

func (chainPatchTool) describe(_ Invocation, constraints ToolConstraints) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        "apply_patch",
		Description: "edits, creates, deletes, or renames files using the V4A patch format",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: !constraints.Readonly || constraints.WriteDir != "", DefaultHidden: true}, nil
}

func (t chainPatchTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	args, err := chainDecodeArguments(call.Arguments)
	if err != nil {
		return nil, err
	}
	normalized, err := tool.NormalizePatchArgs(args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func (t chainPatchTool) Prepare(_ context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	args, err := chainDecodeArguments(call.Arguments)
	if err != nil {
		return chainImmediateError(call.ID, err)
	}
	resolved, err := pathutil.ResolveFilePathFrom("", tc.Workspace)
	if err != nil {
		return chainImmediateDenied(call.ID)
	}
	writeDirCanonical := ""
	if tc.Constraints.WriteDir != "" {
		resolvedWriteDir, err := pathutil.ResolveFilePathFrom(tc.Workspace, tc.Constraints.WriteDir)
		if err != nil {
			return chainImmediateDenied(call.ID)
		}
		writeDirCanonical = resolvedWriteDir.CanonicalPath
	}
	group, err := snapshot.OpenCodeStore(filepath.Join(t.dataDir, "code", tc.AdmittedEntry.SessionID, tc.AdmittedEntry.EntryID))
	if err != nil {
		return chainImmediateDenied(call.ID)
	}
	prepared, err := tool.PreparePatchCall(tc.Workspace, resolved.CanonicalPath, writeDirCanonical, tool.CapabilityOptions{WriteDir: tc.Constraints.WriteDir}, chainCodeGroup{group}, args)
	if err != nil {
		return chainImmediateDenied(call.ID)
	}
	pairs := make([]harness.PermissionRequest, 0, len(prepared.Targets))
	for _, target := range prepared.Targets {
		pairs = append(pairs, harness.PermissionRequest{Permission: "file.write", Target: target.CanonicalPath})
	}
	return harness.PreparedTool{
		Permissions:        pairs,
		CanonicalWorkspace: resolved.CanonicalPath,
		Execute: func(ctx context.Context) harness.ToolOutcome {
			result, execErr := prepared.Execute(ctx)
			if execErr != nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: execErr.Error()}}
			}
			outcome := harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: result.Result}}
			if m := tool.ApplyPatchPreviewMetadata(result.Previews); m != nil {
				if raw, merr := json.Marshal(m); merr == nil {
					outcome.Metadata = raw
				}
			}
			return outcome
		},
	}
}

// chainCommandTool is the readonly-git row's run_command capability: the
// read-only allowlist rewrite with its injected safety flags, one command.run
// pair on the rewritten target, and real foreground execution in the
// Workspace — mirroring the shipped plugin's readonly command path.
type chainCommandTool struct{ dataDir string }

func (chainCommandTool) describe(Invocation, ToolConstraints) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        "run_command",
		Description: "executes one command",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: true}, nil
}

func (t chainCommandTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	args, err := chainDecodeArguments(call.Arguments)
	if err != nil {
		return nil, err
	}
	normalized, err := tool.NormalizeRunCommandArgs(args, 120)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func (t chainCommandTool) Prepare(_ context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	args, err := chainDecodeArguments(call.Arguments)
	if err != nil {
		return chainImmediateError(call.ID, err)
	}
	command, _ := args["command"].(string)
	rewritten, err := tool.ReadOnlyCommand(command)
	if err != nil {
		return chainImmediateError(call.ID, errors.New(tool.ReadOnlyRunCommandRejected))
	}
	spillDir := filepath.Join(t.dataDir, "code", tc.AdmittedEntry.SessionID, "output")
	return harness.PreparedTool{
		Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: rewritten}},
		Execute: func(ctx context.Context) harness.ToolOutcome {
			result, execErr := tool.RunForegroundCommand(ctx, rewritten, tc.Workspace, 120, 15360, 5000, spillDir, os.Environ())
			if execErr != nil {
				status := model.ResultError
				var exitErr *tool.ExitError
				if errors.As(execErr, &exitErr) && exitErr.Cancelled {
					status = model.ResultInterrupted
				}
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: status, Content: execErr.Error()}}
			}
			return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: result}}
		},
	}
}

// chainCommandPlugin declares the readonly-git row's run_command export.
func chainCommandPlugin() Plugin {
	return Plugin{
		ID:       "chaincommands",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("run_command", chainCommandTool{}.describe)},
		Open: func(_ context.Context, info ScopeInfo, _ Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"run_command": chainCommandTool{dataDir: info.DataDir}}}, nil
		},
	}
}

// inertTool is the capability-ID row's tool value: it composes a real
// description and advertisement and dispatches nothing (the row's model turns
// carry no tool calls; a dispatch settles the call with an immediate error).
type inertTool struct {
	name     string
	defThide bool
}

func (t inertTool) describe(Invocation, ToolConstraints) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        t.name,
		Description: "inert " + t.name,
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: true, DefaultHidden: t.defThide}, nil
}

func (t inertTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t inertTool) Prepare(_ context.Context, _ ToolContext, call model.ToolCall) harness.PreparedTool {
	return chainImmediateError(call.ID, errors.New("inert tool dispatch"))
}

// inertToolsPlugin declares the four native file-tool names as inert tools
// for the capability-ID row's advertisement composition.
func inertToolsPlugin() Plugin {
	return Plugin{
		ID:    "inerttools",
		Scope: ScopeRuntime,
		Provides: []CapabilitySpec{
			ToolSpec("read_file", inertTool{name: "read_file"}.describe),
			ToolSpec("write_file", inertTool{name: "write_file"}.describe),
			ToolSpec("edit_file", inertTool{name: "edit_file"}.describe),
			ToolSpec("apply_patch", inertTool{name: "apply_patch", defThide: true}.describe),
		},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{
				"read_file":   inertTool{name: "read_file"},
				"write_file":  inertTool{name: "write_file"},
				"edit_file":   inertTool{name: "edit_file"},
				"apply_patch": inertTool{name: "apply_patch", defThide: true},
			}}, nil
		},
	}
}

// chainToolsPlugin declares the combined scenario's two concrete tools: the
// group-capturing write tool and the patch tool, both deriving their code
// groups from the owner data root.
func chainToolsPlugin() Plugin {
	return Plugin{
		ID:       "chaintools",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("prod_write", chainWriteTool{}.describe), ToolSpec("apply_patch", chainPatchTool{}.describe)},
		Open: func(_ context.Context, info ScopeInfo, _ Bindings) (Instance, error) {
			return Instance{Values: map[string]any{
				"prod_write":  chainWriteTool{dataDir: info.DataDir},
				"apply_patch": chainPatchTool{dataDir: info.DataDir},
			}}, nil
		},
	}
}

// chainTurn is one scripted model turn: a response action, optionally held
// until the test closes its hold channel.
type chainTurn struct {
	hold    chan struct{}
	respond func(w http.ResponseWriter)
}

// chainServer is the combined scenarios' model endpoint: request N answers
// with scripted turn N (1-based) and every request body is recorded. A turn
// with a hold parks until the test closes it; requests past the script fail
// with an error instead of parking.
type chainServer struct {
	*httptest.Server

	mu     sync.Mutex
	bodies []string
	turns  []chainTurn
}

func newChainServer(t *testing.T, turns ...chainTurn) *chainServer {
	t.Helper()
	s := &chainServer{turns: turns}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *chainServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	n := len(s.bodies)
	s.bodies = append(s.bodies, string(body))
	var turn *chainTurn
	if n < len(s.turns) {
		turn = &s.turns[n]
	}
	s.mu.Unlock()

	if turn == nil { // past the script: fail fast, never park silently
		http.Error(w, "chain server: request past the script", http.StatusBadRequest) // 4xx: the retry classifier must not retry a fixture bug
		return
	}
	if turn.hold != nil {
		select {
		case <-turn.hold:
		case <-r.Context().Done():
			return
		}
	}
	turn.respond(w)
}

func (s *chainServer) awaitRequests(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		got := len(s.bodies)
		s.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("chain server: request %d never arrived (received %d)", n, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *chainServer) bodyAt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[i]
}

// chainToolCallTurn writes one completed assistant turn publishing one tool
// call with an explicit call identity.
func chainToolCallTurn(w http.ResponseWriter, id, name, args string) {
	writeSSE(w,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"`+id+`","type":"function","function":{"name":"`+name+`","arguments":`+strconv.Quote(args)+`}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
}

// chainCodeGroupDirs lists one session's code-group directory names under the
// owner data root.
func chainCodeGroupDirs(t *testing.T, dataDir, sessionID string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDir, "code", sessionID))
	if err != nil {
		t.Fatalf("read code dir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

// chainToolResults decodes every committed tool_result entry of one session,
// keyed by tool call ID, with the raw metadata retained.
func chainToolResults(t *testing.T, store harness.Storage, sessionID string) map[string]struct {
	Status   model.ToolResultStatus
	Content  string
	Metadata json.RawMessage
} {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	results := make(map[string]struct {
		Status   model.ToolResultStatus
		Content  string
		Metadata json.RawMessage
	})
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
			t.Fatalf("decode tool_result payload: %v", err)
		}
		results[wire.ToolCallID] = struct {
			Status   model.ToolResultStatus
			Content  string
			Metadata json.RawMessage
		}{Status: wire.Status, Content: wire.Content, Metadata: wire.Metadata}
	}
	return results
}

// chainSessionSignal reads one session's committed signal entries.
func chainSessionSignals(t *testing.T, store harness.Storage, sessionID string) []struct {
	Signal  string
	Content string
} {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	var signals []struct {
		Signal  string
		Content string
	}
	for _, entry := range entries {
		if entry.Kind != harness.EntrySignal {
			continue
		}
		var wire struct {
			Signal  string `json:"signal"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(entry.Payload, &wire); err != nil {
			t.Fatalf("decode signal payload: %v", err)
		}
		signals = append(signals, struct {
			Signal  string
			Content string
		}{Signal: wire.Signal, Content: wire.Content})
	}
	return signals
}

// TestProductionCrossBoundaryComposition chains, in one production
// composition over both stores through public Open: an end-to-end
// hook-rewritten tool call, a mid-flight config reload, a buffered steering
// submit with no group of its own whose delivered mutating call reuses the
// operation's one group on the old capture, a denied multi-target apply_patch
// move settling only its call, a later allowed apply_patch call retaining the
// preview and snapshot evidence, the next admitted operation on the reloaded
// revision, an owner-context cancellation settling the standard interruption
// signal, and a restart over the same data root with a post-repair admission.
func TestProductionCrossBoundaryComposition(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()

		// The scripted turns of the whole scenario, in request order.
		const (
			modelWriteArgs   = `{"path":"hook.txt","content":"model original"}`
			hookRewriteArgs  = `{"path":"rewritten.txt","content":"hook replacement"}`
			steeringText     = "please continue steering"
			originalNotesTxt = "alpha\nbeta\ngamma\n"
		)
		deniedPatch, err := json.Marshal(map[string]any{"input": "*** Begin Patch\n*** Update File: notes.txt\n*** Move to: .env\n@@\n-alpha\n+ALPHA\n*** End Patch"})
		if err != nil {
			t.Fatalf("marshal denied patch: %v", err)
		}
		allowedPatch, err := json.Marshal(map[string]any{"input": "*** Begin Patch\n*** Update File: notes.txt\n*** Move to: renamed.txt\n@@\n-alpha\n+ALPHA\n*** End Patch"})
		if err != nil {
			t.Fatalf("marshal allowed patch: %v", err)
		}
		writeTurn := func(id string) func(http.ResponseWriter) {
			return func(w http.ResponseWriter) { chainToolCallTurn(w, id, "prod_write", modelWriteArgs) }
		}
		release2 := make(chan struct{})
		release3 := make(chan struct{})
		parked := make(chan struct{}) // the canceled operation parks until the owner cancels
		server := newChainServer(t,
			chainTurn{respond: func(w http.ResponseWriter) { chainToolCallTurn(w, "call-1", "prod_write", modelWriteArgs) }},
			chainTurn{hold: release2, respond: writeTurn("call-2")},
			chainTurn{hold: release3, respond: writeTurn("call-3")},
			chainTurn{respond: func(w http.ResponseWriter) { chainToolCallTurn(w, "call-4", "apply_patch", string(deniedPatch)) }},
			chainTurn{respond: func(w http.ResponseWriter) { chainToolCallTurn(w, "call-5", "apply_patch", string(allowedPatch)) }},
			chainTurn{respond: func(w http.ResponseWriter) { writeTextTurn(w, "done") }},
			chainTurn{respond: func(w http.ResponseWriter) { writeTextTurn(w, "done") }},
			chainTurn{hold: parked, respond: func(w http.ResponseWriter) { writeTextTurn(w, "unreachable") }},
			chainTurn{respond: func(w http.ResponseWriter) { writeTextTurn(w, "done") }},
		)

		// Isolated HOME and data roots, pointed at the scenario's server with
		// the chained agent's document.
		home := t.TempDir()
		t.Setenv("HOME", home)
		isolateBundledCredentials(t)
		t.Setenv("PRODUCTION_TEST_KEY", "production-secret-1")
		dataDir := t.TempDir()
		configPath := filepath.Join(dataDir, "config.json")
		writeServiceFile(t, configPath, chainPlainConfigDocument(server.URL))
		writeServiceFile(t, agents.PathForConfig(configPath), chainAgentsDocument("first body"))

		workspace := filepath.Join(home, "chain-ws")
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte(originalNotesTxt), 0o600); err != nil {
			t.Fatalf("write notes.txt: %v", err)
		}

		hook := &recordHook{name: "hook.first", events: &traceLog{}, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
			c.SystemPrompt += "|hooked"
			return c
		}}
		argHook := &rewriteWriteHook{replace: hookRewriteArgs}
		openChained := func(ctx context.Context, hook *recordHook, argHook *rewriteWriteHook) (*Runtime, error) {
			return Open(ctx, Options{DataDir: dataDir, ConfigPath: configPath, Plugins: []Plugin{
				coreStoragePlugin(e.store), chainToolsPlugin(), hookPlugin(hook),
				argumentHookPlugin("arg.rewrite", argHook), bundledAdaptationPlugin(),
			}})
		}
		owner, ownerCancel := context.WithCancel(ctx)
		defer ownerCancel()
		r, err := openChained(owner, hook, argHook)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		session, err := r.createSession(ctx, workspace, "chained")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sessionID := session.Identity.SessionID
		submitThroughRuntime(t, r, sessionID, "op-1", "start")
		server.awaitRequests(t, 2) // turn 1 answered, turn 2 parked at the server

		// 1. The hook rewrote the call end to end: the replacement is what
		// executed, the model's original arguments never reached disk, and
		// the hook observed exactly the original arguments.
		data, err := os.ReadFile(filepath.Join(workspace, "rewritten.txt"))
		if err != nil || string(data) != "hook replacement" {
			t.Fatalf("rewritten.txt = (%q, %v), want the hook's replacement content", data, err)
		}
		if _, err := os.Stat(filepath.Join(workspace, "hook.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("hook.txt = err %v, want absent: the model's original arguments must never execute", err)
		}
		if seen := argHook.received(); !slices.Equal(seen, []string{"prod_write " + modelWriteArgs}) {
			t.Fatalf("argument hook observations = %v, want exactly the original call arguments", seen)
		}
		admitted := readOperation(t, r, sessionID, "op-1").Admission.AdmittedEntry.EntryID

		// 2. The reload publishes a changed prompt while the operation runs.
		writeServiceFile(t, agents.PathForConfig(configPath), chainAgentsDocument("reloaded body"))
		if revision, err := r.Reload(ctx); err != nil || revision != "2" {
			t.Fatalf("Reload = (%q, %v), want 2", revision, err)
		}

		// 3. The steering submit buffers over the active operation: no
		// committed input and no new code-group directory while it sits
		// buffered.
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			res, err := h.Submit(ctx, harness.SubmitRequest{
				SessionID: sessionID, OperationID: "op-1-steer", Origin: harness.InputOriginUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: steeringText}}, Mode: harness.MessageModeRegular,
			})
			if err != nil {
				return err
			}
			if res.Disposition != harness.DispositionSteering {
				return fmt.Errorf("steering submit = %+v, want buffered steering", res.Disposition)
			}
			return nil
		}); err != nil {
			t.Fatalf("steering submit: %v", err)
		}
		if got := chainCodeGroupDirs(t, dataDir, sessionID); !slices.Equal(got, []string{admitted}) {
			t.Fatalf("code groups while the steering input is buffered = %v, want only the admitted operation's group %q", got, admitted)
		}
		for _, entry := range mustChainEntries(t, e.store, sessionID) {
			if entry.Kind == harness.EntryInput && strings.Contains(string(entry.Payload), steeringText) {
				t.Fatal("the buffered steering input committed a durable entry")
			}
		}

		// The turn parked before the steering submit carries no steering
		// input; releasing it runs the pre-steering continued turn.
		close(release2)
		server.awaitRequests(t, 3)

		// The steering input delivers at the next model boundary: the
		// continued turn's request carries it, still on the old capture —
		// the reloaded revision never reaches running work.
		close(release3)
		server.awaitRequests(t, 4)
		if got := chainCodeGroupDirs(t, dataDir, sessionID); !slices.Equal(got, []string{admitted}) {
			t.Fatalf("code groups after the delivered steering's mutating call = %v, want still only the one group %q", got, admitted)
		}

		// 4+5. The denied multi-target move settles only its call; the later
		// allowed call in the subsequent turn succeeds. The operation runs
		// out on its scripted tail.
		rec := awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		if rec.Admission.Execution.ConfigurationRevision != "1" {
			t.Fatalf("capture revision = %q, want the admitted 1", rec.Admission.Execution.ConfigurationRevision)
		}
		if _, err := os.Stat(filepath.Join(workspace, ".env")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf(".env = err %v, want absent: the denied move's destination must never be written", err)
		}

		// The steering input's delivery is durable evidence now: a committed
		// input entry owned by the same operation.
		steeringCommitted := false
		for _, entry := range mustChainEntries(t, e.store, sessionID) {
			if entry.Kind == harness.EntryInput && entry.OperationID == "op-1" && strings.Contains(string(entry.Payload), steeringText) {
				steeringCommitted = true
			}
		}
		if !steeringCommitted {
			t.Fatal("the delivered steering input left no committed input entry")
		}

		// 6. The allowed apply_patch turn's retained preview evidence: the
		// durable tool_result entry carries the edit_preview_files metadata
		// with the move's file/op shape.
		results := chainToolResults(t, e.store, sessionID)
		denied, ok := results["call-4"]
		if !ok || denied.Status != model.ResultDenied || denied.Content != "Permission denied." {
			t.Fatalf("denied move result = %+v (ok %v), want the fixed policy denial", denied, ok)
		}
		if denied.Metadata != nil {
			t.Fatalf("denied move metadata = %s, want none", denied.Metadata)
		}
		allowed, ok := results["call-5"]
		if !ok || allowed.Status != model.ResultSuccess {
			t.Fatalf("allowed patch result = %+v (ok %v), want success after the denial settled only its own call", allowed, ok)
		}
		var preview struct {
			EditPreviewFiles []struct {
				Path string `json:"path"`
				Op   string `json:"op"`
			} `json:"edit_preview_files"`
		}
		if err := json.Unmarshal(allowed.Metadata, &preview); err != nil {
			t.Fatalf("decode allowed patch metadata %s: %v", allowed.Metadata, err)
		}
		if len(preview.EditPreviewFiles) != 2 ||
			preview.EditPreviewFiles[0].Path != "renamed.txt" || preview.EditPreviewFiles[0].Op != "M" ||
			preview.EditPreviewFiles[1].Path != "notes.txt" || preview.EditPreviewFiles[1].Op != "D" {
			t.Fatalf("edit_preview_files = %s, want the move's two entries: the M destination then the D source", allowed.Metadata)
		}

		// 7. The retained snapshot evidence: the moved file's preimage in the
		// operation's one code group carries the original bytes.
		found := false
		turnDir := filepath.Join(dataDir, "code", sessionID, admitted, "snapshots", "1")
		entries, err := os.ReadDir(turnDir)
		if err != nil {
			t.Fatalf("read group turn dir: %v", err)
		}
		for _, entry := range entries {
			metaData, err := os.ReadFile(filepath.Join(turnDir, entry.Name(), "meta.json"))
			if err != nil {
				continue
			}
			var meta struct {
				OriginalPath string `json:"original_path"`
				Existed      bool   `json:"existed"`
			}
			if err := json.Unmarshal(metaData, &meta); err != nil || !meta.Existed || !strings.HasSuffix(meta.OriginalPath, "notes.txt") {
				continue
			}
			original, err := os.ReadFile(filepath.Join(turnDir, entry.Name(), "original"))
			if err != nil || string(original) != originalNotesTxt {
				t.Fatalf("notes.txt preimage = (%q, %v), want the original bytes", original, err)
			}
			found = true
		}
		if !found {
			t.Fatalf("no notes.txt preimage entry in %s, want the retained snapshot evidence", turnDir)
		}

		// The hook observed every call's original arguments exactly once per
		// call: three rewritten writes and two pass-through patch calls.
		if seen := argHook.received(); len(seen) != 5 ||
			seen[0] != "prod_write "+modelWriteArgs || seen[1] != "prod_write "+modelWriteArgs || seen[2] != "prod_write "+modelWriteArgs ||
			seen[3] != "apply_patch "+string(deniedPatch) || seen[4] != "apply_patch "+string(allowedPatch) {
			t.Fatalf("argument hook observations = %v, want each call's original arguments exactly once", seen)
		}

		// The requests of the running operation all carried the old capture:
		// turn 2 (pre-reload) and turn 3 (the steering-continued turn after
		// the reload) both project the captured prompt, and turn 3's history
		// carries the delivered steering input.
		oldPrompt := rec.Admission.Execution.SystemPrompt
		for _, i := range []int{1, 2} {
			wire := decodeWireChatBody(t, server.bodyAt(i))
			if len(wire.Messages) == 0 || wire.Messages[0].Role != "system" {
				t.Fatalf("request %d messages = %+v, want the captured system prompt first", i, wire.Messages)
			}
			if want, _ := json.Marshal(oldPrompt); string(wire.Messages[0].Content) != string(want) {
				t.Fatalf("request %d system message = %.120s, want the old captured prompt %.120s", i, wire.Messages[0].Content, want)
			}
		}
		if !strings.Contains(server.bodyAt(2), steeringText) {
			t.Fatalf("steering-continued request = %s, want the delivered steering input in its history", server.bodyAt(2))
		}
		if strings.Contains(server.bodyAt(1), steeringText) {
			t.Fatalf("pre-steering request = %s, want no steering input in its history", server.bodyAt(1))
		}

		// 2 (cont). The next admitted operation prepares on the reloaded
		// revision: a fresh Session rules out the post-terminal drain racing
		// the submit into a steering buffer.
		next, err := r.createSession(ctx, filepath.Join(home, "chain-ws-2"), "chained")
		if err != nil {
			t.Fatalf("createSession(next): %v", err)
		}
		submitThroughRuntime(t, r, next.Identity.SessionID, "op-2", "after reload")
		nextRec := awaitOperation(t, r, next.Identity.SessionID, "op-2", harness.OperationSuccess)
		if nextRec.Admission.Execution.ConfigurationRevision != "2" ||
			nextRec.Admission.Execution.SystemPrompt == oldPrompt ||
			!strings.Contains(nextRec.Admission.Execution.SystemPrompt, "reloaded body") ||
			strings.Contains(nextRec.Admission.Execution.SystemPrompt, "first body") ||
			!strings.HasSuffix(nextRec.Admission.Execution.SystemPrompt, "|hooked") {
			t.Fatalf("next capture = (%q, %.120s), want revision 2 with the reloaded hooked prompt",
				nextRec.Admission.Execution.ConfigurationRevision, nextRec.Admission.Execution.SystemPrompt)
		}

		// 8. The production-reachable cancellation act: the owner context
		// cancels while an operation is active, and the operation settles
		// interrupted with the standard model-visible signal.
		cxl, err := r.createSession(ctx, filepath.Join(home, "chain-ws-3"), "chained")
		if err != nil {
			t.Fatalf("createSession(cxl): %v", err)
		}
		submitThroughRuntime(t, r, cxl.Identity.SessionID, "op-3", "parked")
		server.awaitRequests(t, 8)
		ownerCancel()
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close after the owner cancellation: %v", err)
		}
		status, detail := operationRegisterTerminal(t, e.store, cxl.Identity.SessionID, "op-3")
		if status != harness.OperationInterruption || detail != "agent interrupted" {
			t.Fatalf("canceled operation = (%q, %q), want the committed interruption with the standard detail", status, detail)
		}
		signals := chainSessionSignals(t, e.store, cxl.Identity.SessionID)
		if len(signals) != 1 || signals[0].Signal != "interruption" || signals[0].Content != "Operation interrupted." {
			t.Fatalf("canceled operation signals = %+v, want exactly the standard interruption signal", signals)
		}

		// 9. Restart: a fresh Open over the same data root runs recovery over
		// the committed interruption evidence and a post-repair admitted
		// operation works.
		r2, err := openChained(ctx, e.hookedHook(), &rewriteWriteHook{replace: hookRewriteArgs})
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer r2.Close(ctx)
		if rec := readOperation(t, r2, cxl.Identity.SessionID, "op-3"); rec.State.Status != harness.OperationInterruption ||
			rec.State.Terminal == nil || rec.State.Terminal.Detail != "agent interrupted" {
			t.Fatalf("restarted operation state = %+v, want the committed interruption intact", rec.State)
		}
		repaired, err := r2.createSession(ctx, filepath.Join(home, "chain-ws-4"), "chained")
		if err != nil {
			t.Fatalf("createSession after repair: %v", err)
		}
		submitThroughRuntime(t, r2, repaired.Identity.SessionID, "op-4", "continue")
		awaitOperation(t, r2, repaired.Identity.SessionID, "op-4", harness.OperationSuccess)
	})
}

// mustChainEntries reads one session's complete committed entries.
func mustChainEntries(t *testing.T, store harness.Storage, sessionID string) []harness.Entry {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	return entries
}

// TestProductionReadonlyGitHookSeesOriginalArguments proves the readonly-git
// row through the production path: a readonly agent's git status run_command
// call reaches a selected no-op ToolArgumentsHook exactly once with the
// original arguments — the readonly rewrite's injected safety flags never
// reach the hook and no second normalization occurs — while the command
// settles with real git status output in this workspace.
func TestProductionReadonlyGitHookSeesOriginalArguments(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()

		// A real temporary git workspace with one untracked file.
		workspace := e.workspace("git-ws")
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		if out, err := exec.Command("git", "init", workspace).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v (%s)", err, out)
		}
		if err := os.WriteFile(filepath.Join(workspace, "ro-note.txt"), []byte("untracked\n"), 0o600); err != nil {
			t.Fatalf("write untracked file: %v", err)
		}

		server := newChainServer(t,
			chainTurn{respond: func(w http.ResponseWriter) { chainToolCallTurn(w, "call-1", "run_command", `{"command":"git status"}`) }},
			chainTurn{respond: func(w http.ResponseWriter) { writeTextTurn(w, "done") }},
		)
		writeServiceFile(t, e.configPath, chainCommandConfigDocument(server.URL))
		writeServiceFile(t, agents.PathForConfig(e.configPath),
			`{"rogit": {"model": "prov/m", "system_prompt": "simple", "tools": ["run_command"], "readonly": true, "capabilities": ["arg.noop"]}}`)

		hook := &noopArgumentHook{}
		r, err := Open(ctx, Options{DataDir: e.dataDir, ConfigPath: e.configPath, Plugins: []Plugin{
			coreStoragePlugin(e.store), chainCommandPlugin(), argumentHookPlugin("arg.noop", hook),
		}})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)

		session, err := r.createSession(ctx, workspace, "rogit")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "status")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		if !rec.Admission.Execution.Readonly {
			t.Fatalf("capture readonly = %v, want the agent's constraint", rec.Admission.Execution.Readonly)
		}

		// The hook saw the call exactly once, with the original arguments.
		if seen := hook.received(); !slices.Equal(seen, []string{`run_command {"command":"git status"}`}) {
			t.Fatalf("argument hook observations = %v, want exactly one original-argument observation", seen)
		}

		// The command settled with real git status output in this workspace
		// (the kept assertions pin non-empty output listing the workspace's
		// untracked file; the injected readonly flags do not change git
		// status stdout in a fresh repo, so rewritten-vs-unrewritten is not
		// distinguishable here — the hook-observation discriminator above
		// owns that row).
		results := chainToolResults(t, e.store, session.Identity.SessionID)
		settled, ok := results["call-1"]
		if !ok || settled.Status != model.ResultSuccess {
			t.Fatalf("run_command result = %+v (ok %v), want success", settled, ok)
		}
		if strings.TrimSpace(settled.Content) == "" {
			t.Fatalf("settled run_command content = %q, want real git status output", settled.Content)
		}
		if !strings.Contains(settled.Content, "ro-note.txt") {
			t.Fatalf("settled run_command content = %q, want the workspace's real untracked file listed", settled.Content)
		}
	})
}

// TestProductionCapabilityIDsModelIndependent proves the model-switch row:
// two agents with identical configured capabilities but differently-adapted
// models keep IDENTICAL capability ID lists in their captures while the
// model-dependent adaptation changes their advertisements and prompts — the
// adaptation never leaks into the capability list.
func TestProductionCapabilityIDsModelIndependent(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		server := newChainServer(t,
			chainTurn{respond: func(w http.ResponseWriter) { writeTextTurn(w, "done") }},
			chainTurn{respond: func(w http.ResponseWriter) { writeTextTurn(w, "done") }},
		)
		writeServiceFile(t, e.configPath, chainModelsConfigDocument(server.URL))
		writeServiceFile(t, agents.PathForConfig(e.configPath), `{
  "gptagent": {"model": "prov/gpt-5.5", "system_prompt": "full", "tools": ["read_file", "write_file", "edit_file", "apply_patch"], "capabilities": ["model_adaptation"]},
  "vanillaagent": {"model": "prov/vanilla", "system_prompt": "full", "tools": ["read_file", "write_file", "edit_file", "apply_patch"], "capabilities": ["model_adaptation"]}
}`)

		r, err := Open(ctx, Options{DataDir: e.dataDir, ConfigPath: e.configPath, Plugins: []Plugin{
			coreStoragePlugin(e.store), inertToolsPlugin(), bundledAdaptationPlugin(),
		}})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)

		gpt, err := r.createSession(ctx, e.workspace("adapt-gpt"), "gptagent")
		if err != nil {
			t.Fatalf("createSession(gpt): %v", err)
		}
		submitThroughRuntime(t, r, gpt.Identity.SessionID, "op-gpt", "hello")
		gptRec := awaitOperation(t, r, gpt.Identity.SessionID, "op-gpt", harness.OperationSuccess)

		van, err := r.createSession(ctx, e.workspace("adapt-van"), "vanillaagent")
		if err != nil {
			t.Fatalf("createSession(van): %v", err)
		}
		submitThroughRuntime(t, r, van.Identity.SessionID, "op-van", "hello")
		vanRec := awaitOperation(t, r, van.Identity.SessionID, "op-van", harness.OperationSuccess)

		// Identical capability ID lists, from identical configured
		// capabilities.
		if !slices.Equal(gptRec.Admission.Execution.Capabilities, vanRec.Admission.Execution.Capabilities) {
			t.Fatalf("capability IDs = %q vs %q, want identical lists across the model switch",
				gptRec.Admission.Execution.Capabilities, vanRec.Admission.Execution.Capabilities)
		}
		if !slices.Equal(gptRec.Admission.Execution.Capabilities, []string{"model_adaptation"}) {
			t.Fatalf("capability IDs = %q, want exactly the agents' configured selection", gptRec.Admission.Execution.Capabilities)
		}

		// The advertisements differ exactly as the bundled adaptations
		// resolve: the GPT-family match swaps write/edit for apply_patch,
		// the unmatched model keeps the default-hidden patch tool out.
		gptTools := toolNames(gptRec.Admission.Execution.Tools)
		vanTools := toolNames(vanRec.Admission.Execution.Tools)
		if !slices.Equal(gptTools, []string{"read_file", "apply_patch"}) {
			t.Fatalf("adapted advertisement = %v, want write/edit excluded and apply_patch included", gptTools)
		}
		if !slices.Equal(vanTools, []string{"read_file", "write_file", "edit_file"}) {
			t.Fatalf("baseline advertisement = %v, want the untouched list with the hidden patch tool out", vanTools)
		}

		// The prompts differ too: the adapted model's task_execution
		// addition renders in the full prompt.
		if !strings.Contains(gptRec.Admission.Execution.SystemPrompt, "Use more tool calls when they can improve the response") {
			t.Fatalf("adapted prompt = %.200s, want the adaptation's task_execution addition", gptRec.Admission.Execution.SystemPrompt)
		}
		if strings.Contains(vanRec.Admission.Execution.SystemPrompt, "Use more tool calls when they can improve the response") {
			t.Fatalf("baseline prompt = %.200s, want no adaptation addition", vanRec.Admission.Execution.SystemPrompt)
		}
	})
}

// TestProductionOpenInitialSweepCleansArtifacts proves the public-Open sweep
// row: the initial maintenance pass inside a public Open deletes an aged
// archived session and removes its planted artifact tree, while a
// stale-but-not-yet-deletable sibling survives with its tree.
func TestProductionOpenInitialSweepCleansArtifacts(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		writeServiceFile(t, e.configPath, chainSweepConfigDocument(e.server.URL))
		writeServiceFile(t, agents.PathForConfig(e.configPath), productionAgentsDocument)

		deletable := seedStaleArchivedSession(t, e.store, filepath.Join(e.dataDir, "sweep-a"), 100*time.Hour)
		young := seedStaleArchivedSession(t, e.store, filepath.Join(e.dataDir, "sweep-b"), 1*time.Hour)
		deletableCode := plantSessionCode(t, e.dataDir, deletable)
		youngCode := plantSessionCode(t, e.dataDir, young)

		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)

		// The initial pass deleted the aged session and removed its artifact
		// tree before the owner was published.
		if _, err := readSweptSession(t, r, deletable); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("aged session after the initial sweep = err %v, want it deleted", err)
		}
		mustNotExist(t, deletableCode, "the deleted session's artifact tree")

		// The stale-but-not-yet-deletable session survives with its tree.
		rec, err := readSweptSession(t, r, young)
		if err != nil || rec.State.Lifecycle != harness.LifecycleArchived {
			t.Fatalf("young session after the initial sweep = %+v err %v, want it archived and intact", rec, err)
		}
		mustExist(t, youngCode, "the surviving session's artifact tree")
	})
}
