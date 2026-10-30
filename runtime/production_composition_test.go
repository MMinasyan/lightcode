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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	modeladapt "github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
)

// chainAgentsDocument builds the combined scenario's agents document with the
// chained agent's system prompt as the given text: the agent selects the
// preparation hook and the argument hook, and declares the directly
// constructed baseline write and patch tools.
func chainAgentsDocument(body string) string {
	return `{
  "chained": {"model": "prov/gpt-5.5", "system_prompt": "simple", "prompt": "` + body + `", "tools": ["write_file", "apply_patch"], "capabilities": ["hook.first", "arg.rewrite"]}
}`
}

// chainPlainConfigDocument is the combined scenario's provider document: the
// configured provider pointed at the scenario's server, the hook tag the
// hookPlugin requires, and no sessions section.
func chainPlainConfigDocument(endpoint string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "PRODUCTION_TEST_KEY",
		`{"gpt-5.5":{"name":"G","context_window":262144}}`) + `},"plugins":{"hooks":{"tag":"T1"}}}`
}

// chainCommandConfigDocument is the readonly-git row's provider document: no
// plugins section, since its composition declares no configured plugin.
func chainCommandConfigDocument(endpoint string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "PRODUCTION_TEST_KEY",
		`{"m":{"name":"M","context_window":262144}}`) + `}}`
}

// chainSweepConfigDocument adds the explicit sweep thresholds and the hook
// tag to one provider document for the public-Open initial-sweep row.
func chainSweepConfigDocument(endpoint string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "",
		`{"m":{"name":"M","context_window":4096}}`) + `},"plugins":{"hooks":{"tag":"T1"}},"sessions":{"archive_after_days":1,"delete_after_archive_days":1}}`
}

// chainModelsConfigDocument declares the two models the capability-ID row
// composes: one matching the bundled GPT-family adaptation pattern, one
// matching nothing.
func chainModelsConfigDocument(endpoint string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "",
		`{"gpt-5.5":{"name":"G","context_window":262144},"vanilla":{"name":"V","context_window":262144}}`) + `}}`
}

// rewriteWriteHook is the combined scenario's ToolArgumentsHook: it replaces
// every write_file call's arguments with its fixed rewrite, passes every
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
	if call.Name == "write_file" {
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

func (bundledAdaptation) Resolve(ref model.ModelRef, mode prompt.Mode) (Adaptation, error) {
	match := modeladapt.Match(ref.Model)
	if match == nil {
		return Adaptation{}, nil
	}
	out := Adaptation{
		ExcludeTools:                match.ExcludeTools,
		ToolDescriptionReplacements: match.ToolDescriptionReplacements,
	}
	if mode == prompt.ModeFull {
		out.Append = match.Append
	}
	return out, nil
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
		testassert.Require(t, !time.Now().After(deadline), "chain server: request %d never arrived (received %d)", n, got)
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *chainServer) bodyAt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[i]
}

// chainCodeGroupDirs lists one session's code-group directory names under the
// owner data root.
func chainCodeGroupDirs(t *testing.T, dataDir, sessionID string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDir, "code", sessionID))
	testassert.NoError(t, err)
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
	testassert.NoError(t, err)
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
		testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
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
	testassert.NoError(t, err)
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
		testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
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
// submit with no group of its own while it waits whose delivered mutating
// call derives its own group through the successor admission on the current
// capture, a denied multi-target apply_patch move settling only its call, a
// later allowed apply_patch call retaining the preview and snapshot
// evidence, the next admitted operation on the reloaded revision, an
// owner-context cancellation settling the standard interruption signal, and a
// restart over the same data root with a post-repair admission.
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
		testassert.NoError(t, err)
		allowedPatch, err := json.Marshal(map[string]any{"input": "*** Begin Patch\n*** Update File: notes.txt\n*** Move to: renamed.txt\n@@\n-alpha\n+ALPHA\n*** End Patch"})
		testassert.NoError(t, err)
		writeTurn := func(id string) func(http.ResponseWriter) {
			return func(w http.ResponseWriter) { writeToolCallTurn(w, id, "write_file", modelWriteArgs) }
		}
		release2 := make(chan struct{})
		release3 := make(chan struct{})
		parked := make(chan struct{}) // the canceled operation parks until the owner cancels
		server := newChainServer(t,
			chainTurn{respond: func(w http.ResponseWriter) { writeToolCallTurn(w, "call-1", "write_file", modelWriteArgs) }},
			chainTurn{hold: release2, respond: writeTurn("call-2")},
			chainTurn{hold: release3, respond: writeTurn("call-3")},
			chainTurn{respond: func(w http.ResponseWriter) { writeToolCallTurn(w, "call-4", "apply_patch", string(deniedPatch)) }},
			chainTurn{respond: func(w http.ResponseWriter) { writeToolCallTurn(w, "call-5", "apply_patch", string(allowedPatch)) }},
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
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte(originalNotesTxt), 0o600))

		hook := &recordHook{name: "hook.first", events: &traceLog{}, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
			c.SystemPrompt += "|hooked"
			return c
		}}
		argHook := &rewriteWriteHook{replace: hookRewriteArgs}
		useFixtureStore(t, e.store)
		openChained := func(ctx context.Context, hook *recordHook, argHook *rewriteWriteHook) (*Runtime, error) {
			return Open(ctx, Options{DataDir: dataDir, ConfigPath: configPath, Plugins: []Plugin{
				hookPlugin(hook),
				argumentHookPlugin("arg.rewrite", argHook), bundledAdaptationPlugin(),
			}})
		}
		owner, ownerCancel := context.WithCancel(ctx)
		defer ownerCancel()
		r, err := openChained(owner, hook, argHook)
		testassert.NoError(t, err)

		session, err := r.createSession(ctx, workspace, "chained")
		testassert.NoError(t, err)
		sessionID := session.Identity.SessionID
		submitThroughRuntime(t, r, sessionID, "op-1", "start")
		server.awaitRequests(t, 2) // turn 1 answered, turn 2 parked at the server

		// 1. The hook rewrote the call end to end: the replacement is what
		// executed, the model's original arguments never reached disk, and
		// the hook observed exactly the original arguments.
		data, err := os.ReadFile(filepath.Join(workspace, "rewritten.txt"))
		testassert.Require(t, err == nil && string(data) == "hook replacement", "rewritten.txt = (%q, %v), want the hook's replacement content", data, err)
		{
			_, err := os.Stat(filepath.Join(workspace, "hook.txt"))
			testassert.Require(t, errors.Is(err, os.ErrNotExist), "hook.txt = err %v, want absent: the model's original arguments must never execute", err)
		}
		{
			seen := argHook.received()
			testassert.Require(t, slices.Equal(seen, []string{"write_file " + modelWriteArgs}), "argument hook observations = %v, want exactly the original call arguments", seen)
		}
		admitted := readOperation(t, r, sessionID, "op-1").Admission.AdmittedEntry.EntryID

		// 2. The reload publishes a changed prompt while the operation runs.
		writeServiceFile(t, agents.PathForConfig(configPath), chainAgentsDocument("reloaded body"))
		{
			revision, err := r.Reload(ctx)
			testassert.Require(t, err == nil && revision == "2", "Reload = (%q, %v), want 2", revision, err)
		}

		// 3. The steering submit buffers over the active operation: no
		// committed input and no new code-group directory while it sits
		// buffered.
		steering, err := submitRawThroughRuntime(ctx, r, sessionID, "op-1-steer", steeringText, harness.MessageModeRegular)
		testassert.NoError(t, err)
		testassert.Require(t, steering.Disposition == harness.DispositionSteering, "steering submit = %+v, want buffered steering", steering.Disposition)
		{
			got := chainCodeGroupDirs(t, dataDir, sessionID)
			testassert.Require(t, slices.Equal(got, []string{admitted}), "code groups while the steering input is buffered = %v, want only the admitted operation's group %q", got, admitted)
		}
		for _, entry := range mustChainEntries(t, e.store, sessionID) {
			testassert.Require(t, entry.Kind != harness.EntryInput || !strings.Contains(string(entry.Payload), steeringText), "the buffered steering input committed a durable entry")
		}

		// The turn parked before the steering submit carries no steering
		// input; releasing it runs the pre-steering continued turn.
		close(release2)
		server.awaitRequests(t, 3)

		// The steering input delivers at the next conversation boundary: the
		// handoff settles op-1 quietly and the steering head's own Operation
		// makes the next request — still on the old capture, so the reloaded
		// revision never reaches running work — and its mutating call derives
		// the steering head's own code group beside the predecessor's.
		close(release3)
		server.awaitRequests(t, 4)
		steerAdmitted := readOperation(t, r, sessionID, "op-1-steer").Admission.AdmittedEntry.EntryID
		{
			got := chainCodeGroupDirs(t, dataDir, sessionID)
			testassert.Require(t, len(got) == 2 && slices.Contains(got, admitted) && slices.Contains(got, steerAdmitted), "code groups after the delivered steering's mutating call = %v, want the predecessor's %q and the steering head's own %q", got, admitted, steerAdmitted)
		}

		// 4+5. The denied multi-target move settles only its call; the later
		// allowed call in the subsequent turn succeeds. The steering head's
		// Operation runs out on its scripted tail.
		rec := awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		testassert.Equal(t, rec.Admission.Execution.ConfigurationRevision, "1")
		steerRec := awaitOperation(t, r, sessionID, "op-1-steer", harness.OperationSuccess)
		testassert.Equal(t, steerRec.Admission.Execution.ConfigurationRevision, "2")
		{
			_, err := os.Stat(filepath.Join(workspace, ".env"))
			testassert.Require(t, errors.Is(err, os.ErrNotExist), ".env = err %v, want absent: the denied move's destination must never be written", err)
		}

		// The steering input's delivery is durable evidence now: a committed
		// input entry owned by the steering head's own Operation.
		steeringCommitted := false
		for _, entry := range mustChainEntries(t, e.store, sessionID) {
			if entry.Kind == harness.EntryInput && entry.OperationID == "op-1-steer" && strings.Contains(string(entry.Payload), steeringText) {
				steeringCommitted = true
			}
		}
		testassert.Require(t, steeringCommitted, "the delivered steering input left no committed input entry")

		// 6. The allowed apply_patch turn's retained preview evidence: the
		// durable tool_result entry carries the edit_preview_files metadata
		// with the move's file/op shape.
		results := chainToolResults(t, e.store, sessionID)
		denied, ok := results["call-4"]
		testassert.Require(t, ok && denied.Status == model.ResultDenied && denied.Content == "Permission denied.", "denied move result = %+v (ok %v), want the fixed policy denial", denied, ok)
		testassert.Require(t, denied.Metadata == nil, "denied move metadata = %s, want none", denied.Metadata)
		allowed, ok := results["call-5"]
		testassert.Require(t, ok && allowed.Status == model.ResultSuccess, "allowed patch result = %+v (ok %v), want success after the denial settled only its own call", allowed, ok)
		var preview struct {
			EditPreviewFiles []struct {
				Path string `json:"path"`
				Op   string `json:"op"`
			} `json:"edit_preview_files"`
		}
		{
			err := json.Unmarshal(allowed.Metadata, &preview)
			testassert.Require(t, err == nil, "decode allowed patch metadata %s: %v", allowed.Metadata, err)
		}
		testassert.Require(t, len(preview.EditPreviewFiles) == 2 && preview.EditPreviewFiles[0].Path == "renamed.txt" && preview.EditPreviewFiles[0].Op == "M" && preview.EditPreviewFiles[1].Path == "notes.txt" && preview.EditPreviewFiles[1].Op == "D", "edit_preview_files = %s, want the move's two entries: the M destination then the D source", allowed.Metadata)

		// 7. The retained snapshot evidence: the moved file's preimage in the
		// steering head's own code group carries the original bytes.
		found := false
		turnDir := filepath.Join(dataDir, "code", sessionID, steerAdmitted, "snapshots", "1")
		entries, err := os.ReadDir(turnDir)
		testassert.NoError(t, err)
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
			testassert.Require(t, err == nil && string(original) == originalNotesTxt, "notes.txt preimage = (%q, %v), want the original bytes", original, err)
			found = true
		}
		testassert.Require(t, found, "no notes.txt preimage entry in %s, want the retained snapshot evidence", turnDir)

		// The hook observed every call's original arguments exactly once per
		// call: three rewritten writes and two pass-through patch calls.
		{
			seen := argHook.received()
			testassert.Require(t, len(seen) == 5 && seen[0] == "write_file "+modelWriteArgs && seen[1] == "write_file "+modelWriteArgs && seen[2] == "write_file "+modelWriteArgs && seen[3] == "apply_patch "+string(deniedPatch) && seen[4] == "apply_patch "+string(allowedPatch), "argument hook observations = %v, want each call's original arguments exactly once", seen)
		}

		// The predecessor's own requests carry its immutable old capture, and
		// the steering head's own first request carries the current reloaded
		// capture with the delivered steering input in its history.
		oldPrompt := rec.Admission.Execution.SystemPrompt
		wire := decodeWireChatBody(t, server.bodyAt(1))
		testassert.Require(t, len(wire.Messages) != 0 && wire.Messages[0].Role == "system", "pre-steering request messages = %+v, want the captured system prompt first", wire.Messages)
		{
			want, _ := json.Marshal(oldPrompt)
			testassert.Require(t, string(wire.Messages[0].Content) == string(want), "pre-steering system message = %.120s, want the old captured prompt %.120s", wire.Messages[0].Content, want)
		}
		steerWire := decodeWireChatBody(t, server.bodyAt(2))
		testassert.Require(t, len(steerWire.Messages) != 0 && steerWire.Messages[0].Role == "system", "steering-continued request messages = %+v, want the successor system prompt first", steerWire.Messages)
		{
			want, _ := json.Marshal(steerRec.Admission.Execution.SystemPrompt)
			testassert.Require(t, string(steerWire.Messages[0].Content) == string(want), "steering-continued system message = %.120s, want the successor's current captured prompt %.120s", steerWire.Messages[0].Content, want)
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
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, next.Identity.SessionID, "op-2", "after reload")
		nextRec := awaitOperation(t, r, next.Identity.SessionID, "op-2", harness.OperationSuccess)
		testassert.Require(t, nextRec.Admission.Execution.ConfigurationRevision == "2" && nextRec.Admission.Execution.SystemPrompt != oldPrompt && strings.Contains(nextRec.Admission.Execution.SystemPrompt, "reloaded body") && !strings.Contains(nextRec.Admission.Execution.SystemPrompt, "first body") && strings.HasSuffix(nextRec.Admission.Execution.SystemPrompt, "|hooked"), "next capture = (%q, %.120s), want revision 2 with the reloaded hooked prompt", nextRec.Admission.Execution.ConfigurationRevision, nextRec.Admission.Execution.SystemPrompt)

		// 8. The production-reachable cancellation act: the owner context
		// cancels while an operation is active, and the operation settles
		// interrupted with the standard model-visible signal.
		cxl, err := r.createSession(ctx, filepath.Join(home, "chain-ws-3"), "chained")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, cxl.Identity.SessionID, "op-3", "parked")
		server.awaitRequests(t, 8)
		ownerCancel()
		testassert.NoError(t, r.Close(ctx))
		status, detail := operationRegisterTerminal(t, e.store, cxl.Identity.SessionID, "op-3")
		testassert.Require(t, status == harness.OperationInterruption && detail == "agent interrupted", "canceled operation = (%q, %q), want the committed interruption with the standard detail", status, detail)
		signals := chainSessionSignals(t, e.store, cxl.Identity.SessionID)
		testassert.Require(t, len(signals) == 1 && signals[0].Signal == "interruption" && signals[0].Content == "Operation interrupted.", "canceled operation signals = %+v, want exactly the standard interruption signal", signals)

		// 9. Restart: a fresh Open over the same data root runs recovery over
		// the committed interruption evidence and a post-repair admitted
		// operation works.
		r2, err := openChained(ctx, e.hookedHook(), &rewriteWriteHook{replace: hookRewriteArgs})
		testassert.NoError(t, err)
		defer r2.Close(ctx)
		{
			rec := readOperation(t, r2, cxl.Identity.SessionID, "op-3")
			testassert.Require(t, rec.State.Status == harness.OperationInterruption && rec.State.Terminal != nil && rec.State.Terminal.Detail == "agent interrupted", "restarted operation state = %+v, want the committed interruption intact", rec.State)
		}
		repaired, err := r2.createSession(ctx, filepath.Join(home, "chain-ws-4"), "chained")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r2, repaired.Identity.SessionID, "op-4", "continue")
		awaitOperation(t, r2, repaired.Identity.SessionID, "op-4", harness.OperationSuccess)
	})
}

// mustChainEntries reads one session's complete committed entries.
func mustChainEntries(t *testing.T, store harness.Storage, sessionID string) []harness.Entry {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
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
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		{
			out, err := exec.Command("git", "init", workspace).CombinedOutput()
			testassert.Require(t, err == nil, "git init: %v (%s)", err, out)
		}
		testassert.NoError(t, os.WriteFile(filepath.Join(workspace, "ro-note.txt"), []byte("untracked\n"), 0o600))

		server := newChainServer(t,
			chainTurn{respond: func(w http.ResponseWriter) { writeToolCallTurn(w, "call-1", "run_command", `{"command":"git status"}`) }},
			chainTurn{respond: func(w http.ResponseWriter) { writeTextTurn(w, "done") }},
		)
		writeServiceFile(t, e.configPath, chainCommandConfigDocument(server.URL))
		writeServiceFile(t, agents.PathForConfig(e.configPath),
			`{"rogit": {"model": "prov/m", "system_prompt": "simple", "tools": ["run_command"], "readonly": true, "capabilities": ["arg.noop"]}}`)

		hook := &noopArgumentHook{}
		r, err := e.openPlugins(ctx, argumentHookPlugin("arg.noop", hook))
		testassert.NoError(t, err)
		defer r.Close(ctx)

		session, err := r.createSession(ctx, workspace, "rogit")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "status")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		testassert.Require(t, rec.Admission.Execution.Readonly, "capture readonly = %v, want the agent's constraint", rec.Admission.Execution.Readonly)

		// The hook saw the call exactly once, with the original arguments.
		{
			seen := hook.received()
			testassert.Require(t, slices.Equal(seen, []string{`run_command {"command":"git status"}`}), "argument hook observations = %v, want exactly one original-argument observation", seen)
		}

		// The command settled with real git status output in this workspace
		// (the kept assertions pin non-empty output listing the workspace's
		// untracked file; the injected readonly flags do not change git
		// status stdout in a fresh repo, so rewritten-vs-unrewritten is not
		// distinguishable here — the hook-observation discriminator above
		// owns that row).
		results := chainToolResults(t, e.store, session.Identity.SessionID)
		settled, ok := results["call-1"]
		testassert.Require(t, ok && settled.Status == model.ResultSuccess, "run_command result = %+v (ok %v), want success", settled, ok)
		testassert.Require(t, strings.TrimSpace(settled.Content) != "" && strings.Contains(settled.Content, "ro-note.txt"), "settled run_command content = %q, want real git status output; settled run_command content = %q, want the workspace's real untracked file listed", settled.Content, settled.Content)
	})
}

// TestProductionCapabilityIDsModelIndependent proves the model-switch row:
// two agents with identical configured capabilities but differently-adapted
// models keep IDENTICAL capability ID lists in their captures while the
// model-dependent adaptation changes their advertisements and prompts — the
// adaptation never leaks into the capability list. The GPT-adapted Agent then
// attempts the two tools its adaptation excludes: each call settles the
// ordinary exact unavailable error and leaves no file effect.
func TestProductionCapabilityIDsModelIndependent(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		server := newChainServer(t,
			chainTurn{respond: func(w http.ResponseWriter) {
				writeToolCallTurn(w, "call-write", "write_file", `{"path":"gpt-write.txt","content":"must not land"}`)
			}},
			chainTurn{respond: func(w http.ResponseWriter) {
				writeToolCallTurn(w, "call-edit", "edit_file", `{"path":"gpt-edit.txt","old_string":"keep me","new_string":"changed"}`)
			}},
			chainTurn{respond: func(w http.ResponseWriter) { writeTextTurn(w, "done") }},
			chainTurn{respond: func(w http.ResponseWriter) { writeTextTurn(w, "done") }},
		)
		writeServiceFile(t, e.configPath, chainModelsConfigDocument(server.URL))
		writeServiceFile(t, agents.PathForConfig(e.configPath), `{
  "gptagent": {"model": "prov/gpt-5.5", "system_prompt": "full", "tools": ["read_file", "write_file", "edit_file", "apply_patch"], "capabilities": ["model_adaptation"]},
  "vanillaagent": {"model": "prov/vanilla", "system_prompt": "full", "tools": ["read_file", "write_file", "edit_file", "apply_patch"], "capabilities": ["model_adaptation"]}
}`)

		r, err := e.openPlugins(ctx, bundledAdaptationPlugin())
		testassert.NoError(t, err)
		defer r.Close(ctx)

		gptWorkspace := e.workspace("adapt-gpt")
		testassert.NoError(t, os.MkdirAll(gptWorkspace, 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(gptWorkspace, "gpt-edit.txt"), []byte("keep me\n"), 0o600))
		gpt, err := r.createSession(ctx, gptWorkspace, "gptagent")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, gpt.Identity.SessionID, "op-gpt", "hello")
		gptRec := awaitOperation(t, r, gpt.Identity.SessionID, "op-gpt", harness.OperationSuccess)

		van, err := r.createSession(ctx, e.workspace("adapt-van"), "vanillaagent")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, van.Identity.SessionID, "op-van", "hello")
		vanRec := awaitOperation(t, r, van.Identity.SessionID, "op-van", harness.OperationSuccess)

		// Identical capability ID lists, from identical configured
		// capabilities.
		testassert.Require(t, slices.Equal(gptRec.Admission.Execution.Capabilities, vanRec.Admission.Execution.Capabilities) && slices.Equal(gptRec.Admission.Execution.Capabilities, []string{"model_adaptation"}), "capability IDs = %q vs %q, want identical lists across the model switch; capability IDs = %q, want exactly the agents' configured selection", gptRec.Admission.Execution.Capabilities, vanRec.Admission.Execution.Capabilities, gptRec.Admission.Execution.Capabilities)

		// The advertisements differ exactly as the bundled adaptations
		// resolve: the GPT-family match excludes write/edit and keeps the
		// patch tool, the unmatched model selects all four tools.
		gptTools := toolNames(gptRec.Admission.Execution.Tools)
		vanTools := toolNames(vanRec.Admission.Execution.Tools)
		testassert.Require(t, slices.Equal(gptTools, []string{"read_file", "apply_patch"}), "adapted advertisement = %v, want write/edit excluded and apply_patch included", gptTools)
		testassert.Require(t, slices.Equal(vanTools, []string{"read_file", "write_file", "edit_file", "apply_patch"}), "baseline advertisement = %v, want every selected tool", vanTools)

		// The excluded tools are refused by the ordinary unavailable path:
		// each attempted call settles its exact error and no file effect
		// happens, so the exclusion is an advertisement/dispatch rule rather
		// than a rejected definition.
		results := chainToolResults(t, e.store, gpt.Identity.SessionID)
		for _, row := range []struct{ callID, name string }{
			{"call-write", "write_file"},
			{"call-edit", "edit_file"},
		} {
			res, ok := results[row.callID]
			testassert.Require(t, ok && res.Status == model.ResultError && res.Content == `Tool "`+row.name+`" is not available.`, "%s result = %+v (ok %v), want the ordinary exact unavailable error", row.name, res, ok)
		}
		{
			_, err := os.Stat(filepath.Join(gptWorkspace, "gpt-write.txt"))
			testassert.Require(t, errors.Is(err, os.ErrNotExist), "gpt-write.txt = err %v, want absent: the excluded write tool ran", err)
		}
		{
			data, err := os.ReadFile(filepath.Join(gptWorkspace, "gpt-edit.txt"))
			testassert.Require(t, err == nil && string(data) == "keep me\n", "gpt-edit.txt = (%q, %v), want the unchanged bytes: the excluded edit tool ran", data, err)
		}

		// The prompts differ too: the adapted model's bundled guidance
		// append renders in the full prompt.
		testassert.Require(t, strings.Contains(gptRec.Admission.Execution.SystemPrompt, "Use more tool calls when they can improve the response"), "adapted prompt = %.200s, want the adaptation's append paragraph", gptRec.Admission.Execution.SystemPrompt)
		testassert.Require(t, !strings.Contains(vanRec.Admission.Execution.SystemPrompt, "Use more tool calls when they can improve the response"), "baseline prompt = %.200s, want no automatic bundled guidance", vanRec.Admission.Execution.SystemPrompt)
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
		testassert.NoError(t, err)
		defer r.Close(ctx)

		// The initial pass deleted the aged session and removed its artifact
		// tree before the owner was published.
		{
			err := headerErrorThroughRuntime(context.Background(), r, deletable)
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "aged session after the initial sweep = err %v, want it deleted", err)
		}
		mustNotExist(t, deletableCode, "the deleted session's artifact tree")

		// The stale-but-not-yet-deletable session survives with its tree.
		rec := headerThroughRuntime(t, r, young)
		testassert.Require(t, rec.Lifecycle == harness.LifecycleArchived, "young session after the initial sweep = %+v, want it archived and intact", rec)
		mustExist(t, youngCode, "the surviving session's artifact tree")
	})
}
