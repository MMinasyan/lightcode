package runtime

import (
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/model"
)

// Production composition suite: the concrete preparation through public Open
// over both stores, with isolated HOME and a test model server as the
// configured provider endpoint. The composition's concrete tools are local
// declared capabilities performing real file effects — the shipped
// registration set itself composes in the external package suite. No test
// touches the user's HOME, .env, cache, or database.

const productionWriteArgs = `{"path":"out.txt","content":"written by production"}`

func productionConfigDocument(endpoint, tag string) string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "` + endpoint + `", "api_key_env": "PRODUCTION_TEST_KEY", "headers": {"X-Provider-Trace": "trace-1"}},
      "usage_in_stream": false,
      "discovery": false,
      "extra_body": {"provider_side": "provider-value", "provider_number": 9007199254740993},
      "models": {
        "m": {"name": "M", "context_window": 4096, "usage_in_stream": true,
              "extra_body": {"model_side": "model-value"},
              "protocol_metadata": {"family": "fam", "must_preserve": ["proto_keep"], "drop": ["drop_me"]}}
      }
    },
    "open": {
      "transport": {"base_url": "` + endpoint + `", "api_key_env": ""},
      "discovery": false,
      "models": {"km": {"name": "K", "context_window": 4096}}
    }
  },
  "plugins": {"hooks": {"tag": "` + tag + `"}}
}`
}

// "duper" keeps duplicate configured tool names in an order that
// discriminates first-occurrence from last-occurrence dedupe, "ro" is the
// readonly constraint sibling, "runner" exercises the command row, "keyless"
// rides the credential-free provider, "adapted" selects the custom
// ModelAdaptation export, "nomodel" has no model at all, and "absent" selects
// a catalog-missing model — the discriminating sibling of the empty identity.
const productionAgentsDocument = `{
  "worker": {"model": "prov/m", "system_prompt": "simple", "tools": ["prod_write", "prod_read"], "capabilities": ["hook.first", "park.second"]},
  "duper": {"model": "prov/m", "system_prompt": "simple", "tools": ["prod_read", "prod_write", "prod_read"], "capabilities": []},
  "ro": {"model": "prov/m", "system_prompt": "simple", "tools": ["prod_write", "prod_read"], "readonly": true},
  "runner": {"model": "prov/m", "system_prompt": "simple", "tools": ["run_command"], "capabilities": []},
  "keyless": {"model": "open/km", "system_prompt": "simple", "tools": ["prod_write"]},
  "adapted": {"model": "prov/m", "system_prompt": "simple", "tools": ["prod_write", "prod_read"], "capabilities": ["model_adaptation"]},
  "nomodel": {"system_prompt": "simple"},
  "absent": {"model": "prov/absent", "system_prompt": "simple"}
}`

// coreStoragePlugin supplies one externally constructed store under the
// private Core contract — the memory variant's store, or the real SQLite
// engine the shipped plugin declares.
func coreStoragePlugin(store harness.Storage) Plugin {
	return Plugin{
		ID:       "core",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[harness.Storage]("session_store")},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"session_store": store}}, nil
		},
	}
}

func hookPlugin(hook *recordHook) Plugin {
	return Plugin{
		ID:       "hooks",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[PreparationHook]("hook.first")},
		ValidateConfig: func(raw json.RawMessage) error {
			var settings struct {
				Tag string `json:"tag"`
			}
			if err := json.Unmarshal(raw, &settings); err != nil || settings.Tag == "" {
				return errors.New("hooks require a non-empty tag")
			}
			return nil
		},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"hook.first": hook}}, nil
		},
	}
}

func parkPlugin(park *parkingHook) Plugin {
	return Plugin{
		ID:       "park",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[PreparationHook]("park.second")},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"park.second": park}}, nil
		},
	}
}

// fileWriteTool is the concrete mutation tool of the production suite: pure
// terminating normalization, one declared file.write pair on the canonical
// target, and an executor performing the real file effect in the Workspace.
type fileWriteTool struct{}

func (t fileWriteTool) describe(_ Invocation, constraints ToolConstraints, _ harness.SessionIdentity) (ToolDescription, error) {
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

func (t fileWriteTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t fileWriteTool) Prepare(_ context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args.Path == "" || args.Content == "" {
		return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "prod_write requires path and content"}}}
	}
	target := filepath.Clean(filepath.Join(tc.Workspace, args.Path))
	return harness.PreparedTool{
		Permissions:        []harness.PermissionRequest{{Permission: "file.write", Target: target}},
		CanonicalWorkspace: filepath.Clean(tc.Workspace),
		Execute: func(ctx context.Context) harness.ToolOutcome {
			if err := ctx.Err(); err != nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "interrupted"}}
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: err.Error()}}
			}
			if err := os.WriteFile(target, []byte(args.Content), 0o600); err != nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: err.Error()}}
			}
			return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "written"}}
		},
	}
}

// fileReadTool is the concrete read-only sibling: always available, its
// executor returns the file content.
type fileReadTool struct{}

func (t fileReadTool) describe(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        "prod_read",
		Description: "reads one file in the workspace",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: true}, nil
}

func (t fileReadTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t fileReadTool) Prepare(_ context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args.Path == "" {
		return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "prod_read requires path"}}}
	}
	target := filepath.Clean(filepath.Join(tc.Workspace, args.Path))
	return harness.PreparedTool{
		Permissions:        []harness.PermissionRequest{{Permission: "file.read", Target: target}},
		CanonicalWorkspace: filepath.Clean(tc.Workspace),
		Execute: func(ctx context.Context) harness.ToolOutcome {
			if err := ctx.Err(); err != nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "interrupted"}}
			}
			data, err := os.ReadFile(target)
			if err != nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: err.Error()}}
			}
			return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: string(data)}}
		},
	}
}

// commandTool is the concrete command row's tool: one declared command.run
// pair on the fixed target and an executor running the requested token as
// argv (no shell), so the settled result is deterministic.
type commandTool struct{}

func (t commandTool) describe(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        "run_command",
		Description: "echoes one token",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: true}, nil
}

func (t commandTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t commandTool) Prepare(_ context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args.Command == "" {
		return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "run_command requires command"}}}
	}
	return harness.PreparedTool{
		Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "*"}},
		Execute: func(ctx context.Context) harness.ToolOutcome {
			if err := ctx.Err(); err != nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "interrupted"}}
			}
			out, err := exec.Command("echo", args.Command).Output()
			if err != nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: err.Error()}}
			}
			return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: strings.TrimSpace(string(out))}}
		},
	}
}

// productionToolsPlugin declares the suite's concrete tools.
func productionToolsPlugin() Plugin {
	return Plugin{
		ID:       "prodtools",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("prod_write", fileWriteTool{}.describe), ToolSpec("prod_read", fileReadTool{}.describe), ToolSpec("run_command", commandTool{}.describe)},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"prod_write": fileWriteTool{}, "prod_read": fileReadTool{}, "run_command": commandTool{}}}, nil
		},
	}
}

// testAdaptation is the custom test plugin's ModelAdaptation value: one
// coaching block, one section addition, and one excluded tool.
type testAdaptation struct{}

func (testAdaptation) Resolve(model.ModelRef) (Adaptation, error) {
	return Adaptation{
		ExcludeTools: []string{"prod_read"},
		Blocks:       []string{"PROD-ADAPTATION-BLOCK"},
		Additions:    map[string]string{"safety": "ADDITION-SAFETY-TEXT"}, // the one section the simple prompt renders
	}, nil
}

// prodAdaptationPlugin declares the suite's custom ModelAdaptation export.
func prodAdaptationPlugin() Plugin {
	return Plugin{
		ID:       "adapt",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[ModelAdaptation]("model_adaptation")},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"model_adaptation": testAdaptation{}}}, nil
		},
	}
}

// eachProductionStore runs one production composition fixture against the
// real SQLite engine and the memory store, both behind the same Core
// contract the shipped SQLite plugin declares: the store variants and their
// cleanup belong to the shared preparation fixture.
func eachProductionStore(t *testing.T, run func(t *testing.T, e *productionEnv)) {
	t.Helper()
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		run(t, newProductionEnv(t, store))
	})
}

type productionEnv struct {
	t          *testing.T
	home       string
	dataDir    string
	configPath string
	server     *productionModelServer
	store      harness.Storage
}

// newProductionEnv isolates HOME and both data roots, points the configured
// provider at the test model server, and resolves its credential from a
// test-set environment variable.
func newProductionEnv(t *testing.T, store harness.Storage) *productionEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	isolateBundledCredentials(t)
	server := newProductionModelServer(t, "prod_write", productionWriteArgs)
	t.Setenv("PRODUCTION_TEST_KEY", "production-secret-1")
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, productionConfigDocument(server.URL, "T1"))
	writeServiceFile(t, agents.PathForConfig(configPath), productionAgentsDocument)
	return &productionEnv{t: t, home: home, dataDir: dataDir, configPath: configPath, server: server, store: store}
}

func (e *productionEnv) open(ctx context.Context, hook *recordHook, park *parkingHook) (*Runtime, error) {
	return e.openWithPlugins(ctx, hook, park)
}

// openWithPlugins composes the suite's base plugin set plus extras — the
// adaptation row adds its custom ModelAdaptation export this way.
func (e *productionEnv) openWithPlugins(ctx context.Context, hook *recordHook, park *parkingHook, extra ...Plugin) (*Runtime, error) {
	e.t.Helper()
	plugins := []Plugin{coreStoragePlugin(e.store), productionToolsPlugin(), hookPlugin(hook), parkPlugin(park)}
	return Open(ctx, Options{DataDir: e.dataDir, ConfigPath: e.configPath, Plugins: append(plugins, extra...)})
}

func (e *productionEnv) hookedHook() *recordHook {
	return &recordHook{name: "hook.first", events: &traceLog{}, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
		c.SystemPrompt += "|hooked"
		return c
	}}
}

func (e *productionEnv) workspace(name string) string {
	return filepath.Join(e.home, name)
}

// productionModelServer is the shared configured-provider endpoint of the
// production and assembled scenarios. Dispatch is body-driven: every first
// turn answers with a tool call ("please run" runs the command row's
// run_command, everything else the constructor's tool), every tool follow-up
// with a final text turn, and "parked" first turns hold until the gate
// releases or the client disconnects (signal observed by the assembled
// shutdown scenario); every request's body and interesting headers are
// recorded.
type productionModelServer struct {
	*httptest.Server

	mu       sync.Mutex
	toolName string
	toolArgs string
	body     []string
	auth     []string
	trace    []string
	gate     chan struct{}
	gone     chan struct{}
}

func newProductionModelServer(t *testing.T, toolName, toolArgs string) *productionModelServer {
	t.Helper()
	s := &productionModelServer{toolName: toolName, toolArgs: toolArgs, gone: make(chan struct{}, 16)}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *productionModelServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.body = append(s.body, string(body))
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	s.trace = append(s.trace, r.Header.Get("X-Provider-Trace"))
	gate := s.gate
	s.mu.Unlock()

	switch role := lastMessageRole(string(body)); {
	case role == "tool":
		writeTextTurn(w, "done")
	case lastUserText(string(body)) == "parked":
		select {
		case <-gate:
			writeTextTurn(w, "released")
		case <-r.Context().Done():
			s.signalGone()
		}
	default:
		toolName, toolArgs := s.toolName, s.toolArgs
		if lastUserText(string(body)) == "please run" {
			toolName, toolArgs = "run_command", `{"command":"production-command-row"}`
		}
		if gate != nil { // one shared first-turn gate wait for every tool turn
			select {
			case <-gate:
			case <-r.Context().Done():
				s.signalGone()
				return
			}
		}
		writeToolCallTurn(w, toolName, toolArgs)
	}
}

func (s *productionModelServer) signalGone() {
	select {
	case s.gone <- struct{}{}:
	default:
	}
}

func (s *productionModelServer) armGate() {
	s.mu.Lock()
	s.gate = make(chan struct{})
	s.mu.Unlock()
}

func (s *productionModelServer) releaseGate() {
	s.mu.Lock()
	gate := s.gate
	s.gate = nil
	s.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (s *productionModelServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.body)
}

func (s *productionModelServer) awaitRequests(n int) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := s.requests(); got >= n {
			return
		}
		if time.Now().After(deadline) {
			panic(fmt.Sprintf("production model server: request %d never arrived (received %d)", n, s.requests()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *productionModelServer) bodyAt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body[i]
}

func (s *productionModelServer) authAt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth[i]
}

func (s *productionModelServer) traceAt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trace[i]
}

// wireChatBody is the encoded request shape the server's recorder asserts.
type wireChatBody struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice    string `json:"tool_choice"`
	MaxTokens     any    `json:"max_tokens"`
	MaxCompletion any    `json:"max_completion_tokens"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func decodeWireChatBody(t *testing.T, body string) wireChatBody {
	t.Helper()
	var doc wireChatBody
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode wire body: %v", err)
	}
	return doc
}

// awaitIdleSession polls one Session's register until its current Operation
// cleared, bounding the wait: the terminal register publishes before the
// post-terminal drain clears the Session.
func awaitIdleSession(t *testing.T, r *Runtime, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var idle bool
		err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			rec, rerr := h.ReadSession(ctx, sessionID)
			idle = rerr == nil && rec.State.CurrentOperationID == ""
			return rerr
		})
		if err == nil && idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %q never became idle (read error %v)", sessionID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProductionAdmittedOperation(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)

		workspace := e.workspace("prod-ws")
		session, err := r.createSession(ctx, workspace, "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "please write")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)

		// The real file effect happened in the Workspace.
		data, err := os.ReadFile(filepath.Join(workspace, "out.txt"))
		if err != nil || string(data) != "written by production" {
			t.Fatalf("prod_write effect = (%q, %v), want the written content", data, err)
		}
		if rec.Admission.Execution.ConfigurationRevision != "1" {
			t.Fatalf("capture revision = %q, want 1", rec.Admission.Execution.ConfigurationRevision)
		}
		if !slices.Equal(toolNames(rec.Admission.Execution.Tools), []string{"prod_write", "prod_read"}) {
			t.Fatalf("advertisement = %v, want the agent's composed tools", toolNames(rec.Admission.Execution.Tools))
		}
		if !strings.HasSuffix(rec.Admission.Execution.SystemPrompt, "|hooked") {
			t.Fatalf("capture prompt = %q, want the hooked composed prompt", rec.Admission.Execution.SystemPrompt)
		}

		// The wire: one tool-call turn and one follow-up, both carrying the
		// resolved credential, the custom headers, the no-max_tokens body,
		// the advertised tools, and the sidecar layers with raw numbers.
		if e.server.requests() != 2 {
			t.Fatalf("model requests = %d, want the tool-call turn and the follow-up", e.server.requests())
		}
		first := decodeWireChatBody(t, e.server.bodyAt(0))
		if first.Model != "m" || !first.Stream || first.ToolChoice != "auto" {
			t.Fatalf("wire body base = %+v, want the fixed streaming shape for model m", first)
		}
		if first.MaxTokens != nil || first.MaxCompletion != nil {
			t.Fatalf("wire body carries an output-token field (%v, %v), want none", first.MaxTokens, first.MaxCompletion)
		}
		if first.StreamOptions == nil || !first.StreamOptions.IncludeUsage {
			t.Fatalf("wire stream_options = %+v, want include_usage from the effective streamed-usage flag", first.StreamOptions)
		}
		var wireTools []string
		for _, tool := range first.Tools {
			wireTools = append(wireTools, tool.Function.Name)
		}
		if !slices.Equal(wireTools, []string{"prod_write", "prod_read"}) {
			t.Fatalf("wire tools = %v, want the advertised order", wireTools)
		}
		if len(first.Messages) == 0 || first.Messages[0].Role != "system" {
			t.Fatalf("wire messages = %+v, want the captured composed prompt first", first.Messages)
		}
		if want, err := json.Marshal(rec.Admission.Execution.SystemPrompt); err != nil || string(first.Messages[0].Content) != string(want) {
			t.Fatalf("wire system message = %s, want the captured composed prompt %s", first.Messages[0].Content, want)
		}
		if auth := e.server.authAt(0); auth != "Bearer production-secret-1" {
			t.Fatalf("wire authorization = %q, want the resolved key's bearer form", auth)
		}
		if trace := e.server.traceAt(0); trace != "trace-1" {
			t.Fatalf("wire X-Provider-Trace = %q, want the captured transport headers", trace)
		}
		body := e.server.bodyAt(0)
		for _, fragment := range []string{`"provider_side":"provider-value"`, `"model_side":"model-value"`, "9007199254740993"} {
			if !strings.Contains(body, fragment) {
				t.Fatalf("wire body misses sidecar fragment %s: %s", fragment, body)
			}
		}
		if lastMessageRole(e.server.bodyAt(1)) != "tool" {
			t.Fatalf("follow-up request = %s, want the tool result continuation", e.server.bodyAt(1))
		}
	})
}

func TestProductionKeyResolution(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("key-ws"), "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		submit := func(opID string) error {
			return r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.Submit(ctx, harness.SubmitRequest{
					SessionID: session.Identity.SessionID, OperationID: opID, Origin: harness.InputOriginUser,
					Content: []model.ContentPart{{Kind: model.PartText, Text: "please write"}}, Mode: harness.MessageModeRegular,
				})
				return err
			})
		}
		readOperationErr := func(opID string) error {
			return r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ReadOperation(ctx, session.Identity.SessionID, opID)
				return err
			})
		}

		// A required credential that resolves empty rejects admission with
		// the retained provider/env-name diagnostic; no Operation exists.
		e.t.Setenv("PRODUCTION_TEST_KEY", "")
		err = submit("op-missing")
		if !errors.Is(err, config.ErrMissingEnvVar) || !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("missing credential submit = %v, want the retained diagnostic wrapping both identities", err)
		}
		if !strings.Contains(err.Error(), "PRODUCTION_TEST_KEY") || !strings.Contains(err.Error(), `for provider "prov"`) {
			t.Fatalf("missing credential diagnostic = %v, want the env name and provider identity", err)
		}
		if err := readOperationErr("op-missing"); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("rejected admission left %v, want no Operation", err)
		}

		// Setting the variable lets the next admission succeed; unsetting it
		// after the prepared admission leaves the captured transport working:
		// both the parked first request and the follow-up carry the captured
		// key, and the turn completes.
		e.t.Setenv("PRODUCTION_TEST_KEY", "production-secret-1")
		e.server.armGate()
		if err := submit("op-1"); err != nil {
			t.Fatalf("submit after the key is set: %v", err)
		}
		e.server.awaitRequests(1)
		e.t.Setenv("PRODUCTION_TEST_KEY", "")
		e.server.releaseGate()
		awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		if e.server.requests() != 2 {
			t.Fatalf("model requests = %d, want both turns", e.server.requests())
		}
		for i := 0; i < 2; i++ {
			if got := e.server.authAt(i); got != "Bearer production-secret-1" {
				t.Fatalf("request %d authorization = %q, want the captured key", i, got)
			}
		}
	})
}

func TestProductionModelSelectionRejection(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)

		for _, row := range []struct {
			agent     string
			opID      string
			wantText  string
			wantClass error
		}{
			{agent: "nomodel", opID: "op-empty", wantText: "no model configured", wantClass: harness.ErrInvalid},
			{agent: "absent", opID: "op-absent", wantText: "is not available in the catalog", wantClass: harness.ErrInvalid},
		} {
			session, err := r.createSession(ctx, e.workspace("reject-"+row.agent), row.agent)
			if err != nil {
				t.Fatalf("createSession(%s): %v", row.agent, err)
			}
			err = r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.Submit(ctx, harness.SubmitRequest{
					SessionID: session.Identity.SessionID, OperationID: row.opID, Origin: harness.InputOriginUser,
					Content: []model.ContentPart{{Kind: model.PartText, Text: "hello"}}, Mode: harness.MessageModeRegular,
				})
				return err
			})
			if !errors.Is(err, row.wantClass) || !strings.Contains(err.Error(), row.wantText) {
				t.Fatalf("%s submit = %v, want %v with %q", row.agent, err, row.wantClass, row.wantText)
			}
			if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ReadOperation(ctx, session.Identity.SessionID, row.opID)
				return err
			}); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("%s rejection left %v, want no Operation", row.agent, err)
			}
		}

		// Both rejections happened before any scope opened.
		r.workspaces.mu.Lock()
		live := len(r.workspaces.live)
		r.workspaces.mu.Unlock()
		if live != 0 {
			t.Fatalf("live Workspace scopes after the rejections = %d, want none", live)
		}
	})
}

func TestProductionReloadCapture(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		hook := e.hookedHook()
		r, err := e.open(ctx, hook, &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("reload-ws"), "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}

		// The in-flight admission stays on its captured revision.
		e.server.armGate()
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "parked")
		e.server.awaitRequests(1)
		writeServiceFile(t, e.configPath, productionConfigDocument(e.server.URL, "T2"))
		if revision, err := r.Reload(ctx); err != nil || revision != "2" {
			t.Fatalf("Reload = (%q, %v), want 2", revision, err)
		}
		e.server.releaseGate()
		if rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess); rec.Admission.Execution.ConfigurationRevision != "1" {
			t.Fatalf("in-flight capture revision = %q, want the captured 1", rec.Admission.Execution.ConfigurationRevision)
		}
		// The next admission prepares on the new revision and its hook reads
		// the new tag; the fresh Session rules out any post-terminal drain
		// racing the submit into a steering buffer.
		next, err := r.createSession(ctx, e.workspace("reload-ws-2"), "worker")
		if err != nil {
			t.Fatalf("createSession(next): %v", err)
		}
		submitThroughRuntime(t, r, next.Identity.SessionID, "op-2", "hello")
		if rec := awaitOperation(t, r, next.Identity.SessionID, "op-2", harness.OperationSuccess); rec.Admission.Execution.ConfigurationRevision != "2" {
			t.Fatalf("next capture revision = %q, want 2", rec.Admission.Execution.ConfigurationRevision)
		}
		if got := hook.calls(); !slices.Equal(got, []hookCall{{revision: "1", tag: "T1"}, {revision: "2", tag: "T2"}}) {
			t.Fatalf("hook calls = %v, want each admission's own revision tag", got)
		}
	})
}

func TestProductionQueuedAndFork(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("queue-ws"), "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}

		// The queued message buffers over the blocked Operation and delivers
		// a working execution when the drain reaches it.
		e.server.armGate()
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "parked")
		e.server.awaitRequests(1)
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			res, err := h.Submit(ctx, harness.SubmitRequest{
				SessionID: session.Identity.SessionID, OperationID: "op-2", Origin: harness.InputOriginUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: "please write"}}, Mode: harness.MessageModeQueued,
			})
			if err != nil {
				return err
			}
			if res.Disposition != harness.DispositionQueued {
				return fmt.Errorf("submit = %v, want queued", res.Disposition)
			}
			return nil
		}); err != nil {
			t.Fatalf("queued submit: %v", err)
		}
		e.server.releaseGate()
		awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		awaitOperation(t, r, session.Identity.SessionID, "op-2", harness.OperationSuccess)
		awaitIdleSession(t, r, session.Identity.SessionID)

		// Fork admission runs the destination Operation through the same
		// production preparation with its zero session start sampled at
		// compose time.
		boundary := ""
		entries, err := e.store.ReadEntries(ctx, session.Identity.SessionID, 0)
		if err != nil {
			t.Fatalf("ReadEntries: %v", err)
		}
		for _, entry := range entries {
			if entry.Kind == harness.EntryInput && entry.OperationID == "op-2" {
				boundary = entry.ID
				break
			}
		}
		if boundary == "" {
			t.Fatal("no admitted input entry was found for the fork boundary")
		}
		var forked harness.SessionRecord
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			res, err := h.Fork(ctx, harness.ForkRequest{
				SourceSessionID: session.Identity.SessionID,
				BoundaryEntryID: boundary,
				OperationID:     "op-fork",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "please write"}},
			})
			if err != nil {
				return err
			}
			forked = res.Session
			return nil
		}); err != nil {
			t.Fatalf("Fork: %v", err)
		}
		rec := awaitOperation(t, r, forked.Identity.SessionID, "op-fork", harness.OperationSuccess)
		if rec.Admission.Execution.ConfigurationRevision != "1" || !strings.HasSuffix(rec.Admission.Execution.SystemPrompt, "|hooked") {
			t.Fatalf("forked Operation capture = %+v, want the hooked production capture", rec.Admission.Execution)
		}
	})
}

func TestProductionHookCapture(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		hook := e.hookedHook()
		r, err := e.open(ctx, hook, &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("hook-ws"), "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		submit := func(opID string) error {
			return r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.Submit(ctx, harness.SubmitRequest{
					SessionID: session.Identity.SessionID, OperationID: opID, Origin: harness.InputOriginUser,
					Content: []model.ContentPart{{Kind: model.PartText, Text: "hello"}}, Mode: harness.MessageModeRegular,
				})
				return err
			})
		}
		// The hook's narrowed description survives into the committed
		// capture.
		hook.set(nil, func(c harness.ExecutionCapture) harness.ExecutionCapture {
			c.Tools[0].Description = "narrowed by the hook"
			return c
		})
		if err := submit("op-narrow"); err != nil {
			t.Fatalf("submit: %v", err)
		}
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-narrow", harness.OperationSuccess)
		if got := rec.Admission.Execution.Tools[0].Description; got != "narrowed by the hook" {
			t.Fatalf("committed description = %q, want the hook's narrowed form", got)
		}

		// Capability expansion and constraint changes still reject with no
		// Operation published. Every rejection runs on its own fresh Session,
		// so no post-terminal drain can race the submit into a steering
		// buffer.
		for _, row := range []struct {
			name   string
			mutate func(harness.ExecutionCapture) harness.ExecutionCapture
		}{
			{"expansion", func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.Capabilities = append(c.Capabilities, "hook.second")
				return c
			}},
			{"readonly", func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.Readonly = true
				return c
			}},
			{"writedir", func(c harness.ExecutionCapture) harness.ExecutionCapture {
				c.WriteDir = "/somewhere-else"
				return c
			}},
		} {
			hook.set(nil, row.mutate)
			rowSession, err := r.createSession(ctx, e.workspace("hook-"+row.name+"-ws"), "worker")
			if err != nil {
				t.Fatalf("createSession(%s): %v", row.name, err)
			}
			err = r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.Submit(ctx, harness.SubmitRequest{
					SessionID: rowSession.Identity.SessionID, OperationID: "op-" + row.name, Origin: harness.InputOriginUser,
					Content: []model.ContentPart{{Kind: model.PartText, Text: "hello"}}, Mode: harness.MessageModeRegular,
				})
				return err
			})
			if !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("%s submit = %v, want the ErrInvalid rejection", row.name, err)
			}
			if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ReadOperation(ctx, rowSession.Identity.SessionID, "op-"+row.name)
				return err
			}); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("%s rejection left %v, want no Operation", row.name, err)
			}
		}
		hook.set(nil, nil)
	})
}

func TestProductionFailedOpening(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		writeServiceFile(t, e.configPath, "{not json")

		if r, err := e.open(ctx, e.hookedHook(), &parkingHook{}); err == nil {
			r.Close(ctx)
			t.Fatal("Open over a malformed configuration succeeded, want failure with no partial authority")
		}

		// The failed construction released the data-directory ownership: the
		// same root opens again once the document is valid.
		writeServiceFile(t, e.configPath, productionConfigDocument(e.server.URL, "T1"))
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("reopen after failed construction: %v", err)
		}
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

func TestProductionBlockedShutdown(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		park := &parkingHook{}
		r, err := e.open(ctx, e.hookedHook(), park)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		session, err := r.createSession(ctx, e.workspace("shutdown-ws"), "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}

		// The armed hook parks the admission: an in-flight admitted call
		// blocks Close until released, and the joined shutdown completes.
		release := make(chan struct{})
		used := make(chan struct{}, 1)
		park.arm(release, used)
		done := make(chan error, 1)
		go func() {
			done <- r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.Submit(ctx, harness.SubmitRequest{
					SessionID: session.Identity.SessionID, OperationID: "op-parked", Origin: harness.InputOriginUser,
					Content: []model.ContentPart{{Kind: model.PartText, Text: "hello"}}, Mode: harness.MessageModeRegular,
				})
				return err
			})
		}()
		select {
		case <-used:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked admission never reached the hook")
		}
		closeDone := make(chan error, 1)
		go func() { closeDone <- r.Close(ctx) }()
		joinDeadline := time.Now().Add(5 * time.Second)
		for !JoinedInFlightCalls(r) { // provably parked on the call-gate join; the parked admission holds the token
			if time.Now().After(joinDeadline) {
				t.Fatal("Close never joined the parked admission")
			}
			time.Sleep(2 * time.Millisecond)
		}
		close(release)
		if err := <-closeDone; err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := <-done; err == nil {
			t.Fatal("the parked admission succeeded after the owner canceled; want the cancellation failure")
		}
	})
}

func TestProductionDuplicateTools(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("dup-ws"), "duper")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "hello")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		if got := toolNames(rec.Admission.Execution.Tools); !slices.Equal(got, []string{"prod_read", "prod_write"}) {
			t.Fatalf("advertisement = %v, want duplicates deduped by first occurrence", got)
		}
	})
}

func TestProductionReadonlyAdvertisement(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("ro-ws"), "ro")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "hello")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		if !rec.Admission.Execution.Readonly {
			t.Fatalf("capture readonly = %v, want the agent's constraint", rec.Admission.Execution.Readonly)
		}
		if got := toolNames(rec.Admission.Execution.Tools); !slices.Equal(got, []string{"prod_read"}) {
			t.Fatalf("advertisement = %v, want the mutation tool dropped by the hard constraint", got)
		}
	})
}

// TestProductionTransportConversion pins the catalog-to-transport conversion
// on one published snapshot: effective role and streamed usage, sidecar
// layers with raw numbers, the effective protocol metadata, the
// same-provider source-family map, and the retained wire-debug opt-in.
func TestProductionTransportConversion(t *testing.T) {
	sh := newServiceHarness(t)
	writeServiceFile(t, sh.configPath, conversionConfigDocument())
	writeServiceFile(t, agents.PathForConfig(sh.configPath), "{}")
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := mustComposition(t, coreStoragePlugin(storage.NewMemory()))
	runtimeScope := mustOpenScope(t, c, owner, ScopeInfo{Kind: ScopeRuntime, DataDir: sh.dataDir}, nil)
	obs := newObservation()
	svc := newConfigurationService(owner, c, sh.loader, sh.configPath, obs)
	snapshot, err := svc.publish(owner)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	p := newPreparation(svc, c, runtimeScope, newWorkspaceScopes(owner, c, []*scope{runtimeScope}, obs), sh.home, nil, nil)
	ref := model.ModelRef{Provider: "prov", Model: "m"}
	provider, entry, err := snapshot.catalog.Lookup(catalog.ModelRef{Provider: ref.Provider, Model: ref.Model})
	if err != nil {
		t.Fatalf("catalog lookup: %v", err)
	}

	resolved, err := p.resolveTransport(provider, entry, ref, "resolved-key")
	if err != nil {
		t.Fatalf("resolveTransport: %v", err)
	}
	if resolved.Model != ref || resolved.BaseURL != "https://prov.test/v1" || resolved.APIKey != "resolved-key" {
		t.Fatalf("resolved identity = %+v, want the captured endpoint, key, and model", resolved)
	}
	if resolved.WireSystemRole != "user" {
		t.Fatalf("wire system role = %q, want the model-level override", resolved.WireSystemRole)
	}
	if !resolved.StreamedUsage {
		t.Fatal("streamed usage = false, want the model-level true override of the provider-level false")
	}
	if resolved.Headers["X-Trace"] != "t1" {
		t.Fatalf("resolved headers = %v, want the captured transport headers", resolved.Headers)
	}
	if got := string(resolved.ProviderExtraBody["provider_side"]); got != `"p"` {
		t.Fatalf("provider sidecar = %s, want the verbatim re-encoded value", got)
	}
	if got := string(resolved.ProviderExtraBody["big"]); got != "9007199254740993" {
		t.Fatalf("provider sidecar number = %s, want the raw lexeme with no float coercion", got)
	}
	if got := string(resolved.ModelExtraBody["precise"]); got != "1.25" {
		t.Fatalf("model sidecar number = %s, want the raw lexeme", got)
	}
	if resolved.ProtocolFamily != "fam" || !slices.Equal(resolved.MustPreserve, []string{"proto_keep"}) ||
		!resolved.Drop["drop_me"] || len(resolved.Drop) != 1 {
		t.Fatalf("resolved protocol metadata = family %q must-preserve %v drop %v, want the model-level effective values", resolved.ProtocolFamily, resolved.MustPreserve, resolved.Drop)
	}
	wantFamilies := map[model.ModelRef]string{
		{Provider: "prov", Model: "m"}:     "fam",
		{Provider: "prov", Model: "sib"}:   "fam",
		{Provider: "prov", Model: "other"}: "else",
	}
	if len(resolved.SourceFamilies) != len(wantFamilies) {
		t.Fatalf("source families = %v, want exactly the same-provider map %v", resolved.SourceFamilies, wantFamilies)
	}
	for k, v := range wantFamilies {
		if resolved.SourceFamilies[k] != v {
			t.Fatalf("source family of %s = %q, want %q", k.String(), resolved.SourceFamilies[k], v)
		}
	}

	// The system message encodes with the effective wire role.
	body, _, err := model.Encode(resolved, model.Request{Messages: []model.Message{{Role: model.RoleSystem, Content: []model.ContentPart{{Kind: model.PartText, Text: "sys"}}}}}, nil)
	if err != nil {
		t.Fatalf("encode system: %v", err)
	}
	if !strings.Contains(string(body), `"role":"user"`) {
		t.Fatalf("system message wire role = %s, want the effective user role", body)
	}

	// The wire-debug opt-in: off by default; enabled it creates the
	// home-based private directory and supplies it; a creation failure
	// disables diagnostics, never execution.
	if got := resolved.WireDebugDir; got != "" {
		t.Fatalf("wire debug dir with the opt-in off = %q, want empty", got)
	}
	t.Setenv(debugWireEnv, "1")
	enabled, err := p.resolveTransport(provider, entry, ref, "resolved-key")
	if err != nil {
		t.Fatalf("resolveTransport with the opt-in on: %v", err)
	}
	want := filepath.Join(sh.home, ".lightcode", "debug", "wire")
	if enabled.WireDebugDir != want {
		t.Fatalf("wire debug dir = %q, want the created home-based %q", enabled.WireDebugDir, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("wire debug directory = (%v, %v), want the created private directory", info, err)
	}
	blocked := filepath.Join(sh.home, "file.txt")
	if err := os.WriteFile(blocked, []byte("a file, not a directory"), 0o600); err != nil {
		t.Fatalf("write the blocking file: %v", err)
	}
	broken := &preparation{home: filepath.Join(blocked, "sub")}
	disabled, err := broken.resolveTransport(provider, entry, ref, "resolved-key")
	if err != nil {
		t.Fatalf("resolveTransport over an unusable home: %v", err)
	}
	if disabled.WireDebugDir != "" {
		t.Fatalf("wire debug dir over an unusable home = %q, want the empty disabled form", disabled.WireDebugDir)
	}
}

func conversionConfigDocument() string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "https://prov.test/v1", "api_key_env": "", "headers": {"X-Trace": "t1"}},
      "system_role": "developer",
      "usage_in_stream": false,
      "discovery": false,
      "extra_body": {"provider_side": "p", "big": 9007199254740993},
      "protocol_metadata": {"family": "fam", "must_preserve": ["keep_provider"], "drop": ["drop_provider"]},
      "models": {
        "m": {"name": "M", "context_window": 4096, "usage_in_stream": true, "system_role": "user",
              "extra_body": {"precise": 1.25},
              "protocol_metadata": {"family": "fam", "must_preserve": ["proto_keep"], "drop": ["drop_me"]}},
        "sib": {"name": "S", "context_window": 4096},
        "other": {"name": "O", "context_window": 4096, "protocol_metadata": {"family": "else"}}
      }
    }
  }
}`
}

// TestProductionCommandRow dispatches one run_command through the production
// composition: the concrete opener's tool dispatch runs the real command and
// the settled result content reaches the durable tool-result entry.
func TestProductionCommandRow(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("run-ws"), "runner")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "please run")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		if !slices.Equal(toolNames(rec.Admission.Execution.Tools), []string{"run_command"}) {
			t.Fatalf("advertisement = %v, want the command tool", toolNames(rec.Admission.Execution.Tools))
		}
		// The settled result content proves the command ran through the
		// concrete opener's dispatch.
		entries, err := e.store.ReadEntries(ctx, session.Identity.SessionID, 0)
		if err != nil {
			t.Fatalf("ReadEntries: %v", err)
		}
		var content string
		for _, entry := range entries {
			if entry.Kind != harness.EntryToolResult {
				continue
			}
			var wire struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(entry.Payload, &wire); err != nil {
				t.Fatalf("decode tool result: %v", err)
			}
			content = wire.Content
		}
		if content != "production-command-row" {
			t.Fatalf("settled run_command content = %q, want the echoed token", content)
		}
	})
}

// TestProductionAdaptationComposes composes the suite's custom
// ModelAdaptation export through public Open and proves through one real
// admitted operation that the selected adaptation reached the composed
// surface: the coaching block and section addition appear in the captured
// prompt and the excluded tool is absent from the advertisement.
func TestProductionAdaptationComposes(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.openWithPlugins(ctx, e.hookedHook(), &parkingHook{}, prodAdaptationPlugin())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("adapt-ws"), "adapted")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "hello")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)

		prompt := rec.Admission.Execution.SystemPrompt
		if !strings.Contains(prompt, "PROD-ADAPTATION-BLOCK") {
			t.Fatalf("captured prompt misses the adaptation's coaching block: %.200s", prompt)
		}
		if !strings.Contains(prompt, "ADDITION-SAFETY-TEXT") {
			t.Fatalf("captured prompt misses the adaptation's section addition: %.200s", prompt)
		}
		if got := toolNames(rec.Admission.Execution.Tools); !slices.Equal(got, []string{"prod_write"}) {
			t.Fatalf("advertisement = %v, want the adaptation's excluded tool absent", got)
		}

		// The exclusion is the adaptation's work, not the hard constraints:
		// the capture's readonly/write-dir fields stay the definition's.
		if rec.Admission.Execution.Readonly || rec.Admission.Execution.WriteDir != "" {
			t.Fatalf("capture constraints = (%v, %q), want the definition's untouched values",
				rec.Admission.Execution.Readonly, rec.Admission.Execution.WriteDir)
		}
	})
}

// TestProductionKeylessProvider admits an agent on the credential-free
// provider (empty api_key_env) through public Open: the admission resolves
// no credential, the real model request carries no Authorization header, and
// the turn settles through the concrete opener.
func TestProductionKeylessProvider(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("keyless-ws"), "keyless")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "please write")
		awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		if e.server.requests() != 2 {
			t.Fatalf("model requests = %d, want the tool-call turn and the follow-up", e.server.requests())
		}
		if got := e.server.authAt(0); got != "" {
			t.Fatalf("keyless request authorization = %q, want the header omitted entirely", got)
		}
		data, err := os.ReadFile(filepath.Join(e.workspace("keyless-ws"), "out.txt"))
		if err != nil || string(data) != "written by production" {
			t.Fatalf("prod_write effect = (%q, %v), want the written content", data, err)
		}
	})
}
