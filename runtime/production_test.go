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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
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
        "m": {"name": "M", "context_window": 262144, "usage_in_stream": true,
              "extra_body": {"model_side": "model-value"},
              "protocol_metadata": {"family": "fam", "must_preserve": ["proto_keep"], "drop": ["drop_me"]}}
      }
    },
    "open": {
      "transport": {"base_url": "` + endpoint + `", "api_key_env": ""},
      "discovery": false,
      "models": {"km": {"name": "K", "context_window": 262144}}
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

// openPlugins opens the fixture's exact plugin list over its data and config
// roots.
func (e *productionEnv) openPlugins(ctx context.Context, plugins ...Plugin) (*Runtime, error) {
	e.t.Helper()
	return Open(ctx, Options{DataDir: e.dataDir, ConfigPath: e.configPath, Plugins: plugins})
}

// openWithPlugins composes the suite's base plugin set plus extras — the
// adaptation row adds its custom ModelAdaptation export this way.
func (e *productionEnv) openWithPlugins(ctx context.Context, hook *recordHook, park *parkingHook, extra ...Plugin) (*Runtime, error) {
	e.t.Helper()
	plugins := []Plugin{coreStoragePlugin(e.store), productionToolsPlugin(), hookPlugin(hook), parkPlugin(park)}
	return e.openPlugins(ctx, append(plugins, extra...)...)
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

// productionModelServer is the one shared configured-provider endpoint of the
// production, assembled and native owner fixtures. Every request's raw body,
// auth and trace headers are recorded in arrival order, and one arrival
// signal is sent after recording, before any wait. An armed hold parks each
// request until the channel closes or the client disconnects, ahead of
// dispatch. An installed script answers for the request's body (a nil return
// falls through, and a two-element result prefixed "HTTP " writes one
// scripted non-2xx response); otherwise dispatch is body-driven: without a
// configured default tool the plain done text turn answers every body, and
// with one every tool follow-up answers a final text turn ("done"), a
// "parked" first turn holds until the gate releases or the client disconnects
// (signal observed by the assembled shutdown scenario), and other first turns
// answer a tool call ("please run" runs the command row's run_command,
// everything else the constructor's tool).
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
	hold     chan struct{}
	script   func(ctx context.Context, body string) []string
	arrived  chan struct{}
}

func newProductionModelServer(t *testing.T, toolName, toolArgs string) *productionModelServer {
	t.Helper()
	s := &productionModelServer{toolName: toolName, toolArgs: toolArgs, gone: make(chan struct{}, 16), arrived: make(chan struct{}, 64)}
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
	hold := s.hold
	script := s.script
	toolName, toolArgs := s.toolName, s.toolArgs
	s.mu.Unlock()

	select {
	case s.arrived <- struct{}{}:
	default:
	}
	if hold != nil { // the before-dispatch owner wait: release or client disconnect
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	if script != nil {
		if events := script(r.Context(), string(body)); events != nil {
			if len(events) == 2 && strings.HasPrefix(events[0], "HTTP ") {
				// One scripted non-2xx provider response: its status and body
				// are the transport's own facts.
				code, err := strconv.Atoi(strings.TrimPrefix(events[0], "HTTP "))
				if err != nil {
					panic("production model server: bad scripted status " + events[0])
				}
				w.WriteHeader(code)
				_, _ = w.Write([]byte(events[1]))
				return
			}
			writeSSE(w, events...)
			return
		}
	}
	if toolName == "" { // no default tool: the plain owner fixture's done fallback
		writeTextTurn(w, "done")
		return
	}
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
		writeToolCallTurn(w, "call-1", toolName, toolArgs)
	}
}

// setScript installs one wire-dispatched turn script; a nil return leaves the
// request to the body-driven dispatch. The script receives the HTTP request
// context, so a scripted turn parks on the client's own cancellation.
func (s *productionModelServer) setScript(script func(ctx context.Context, body string) []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = script
}

// setHold parks every request arriving while the channel is armed until the
// channel closes; the parking request still records and signals its arrival
// first.
func (s *productionModelServer) setHold(hold chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = hold
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
	testassert.NoError(t, json.Unmarshal([]byte(body), &doc))
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
			rec, rerr := h.ReadSessionHeader(ctx, sessionID)
			idle = rerr == nil && rec.CurrentOperationID == ""
			return rerr
		})
		if err == nil && idle {
			return
		}
		testassert.Require(t, !time.Now().After(deadline), "session %q never became idle (read error %v)", sessionID, err)
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProductionAdmittedOperation(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		testassert.NoError(t, err)
		defer r.Close(ctx)

		workspace := e.workspace("prod-ws")
		session, err := r.createSession(ctx, workspace, "worker")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "please write")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)

		// The real file effect happened in the Workspace.
		data, err := os.ReadFile(filepath.Join(workspace, "out.txt"))
		testassert.Require(t, err == nil && string(data) == "written by production", "prod_write effect = (%q, %v), want the written content", data, err)
		testassert.Equal(t, rec.Admission.Execution.ConfigurationRevision, "1")
		if !slices.Equal(toolNames(rec.Admission.Execution.Tools), []string{"prod_write", "prod_read"}) {
			t.Fatalf("advertisement = %v, want the agent's composed tools", toolNames(rec.Admission.Execution.Tools))
		}
		testassert.Require(t, strings.HasSuffix(rec.Admission.Execution.SystemPrompt, "|hooked"), "capture prompt = %q, want the hooked composed prompt", rec.Admission.Execution.SystemPrompt)

		// The wire: one tool-call turn and one follow-up, both carrying the
		// resolved credential, the custom headers, the no-max_tokens body,
		// the advertised tools, and the sidecar layers with raw numbers.
		if e.server.requests() != 2 {
			t.Fatalf("model requests = %d, want the tool-call turn and the follow-up", e.server.requests())
		}
		first := decodeWireChatBody(t, e.server.bodyAt(0))
		testassert.Require(t, first.Model == "m" && first.Stream && first.ToolChoice == "auto" && first.MaxTokens == nil && first.MaxCompletion == nil && first.StreamOptions != nil && first.StreamOptions.IncludeUsage, "wire body base = %+v, want the fixed streaming shape for model m; wire body carries an output-token field (%v, %v), want none; wire stream_options = %+v, want include_usage from the effective streamed-usage flag", first, first.MaxTokens, first.MaxCompletion, first.StreamOptions)
		var wireTools []string
		for _, tool := range first.Tools {
			wireTools = append(wireTools, tool.Function.Name)
		}
		testassert.Require(t, slices.Equal(wireTools, []string{"prod_write", "prod_read"}), "wire tools = %v, want the advertised order", wireTools)
		testassert.Require(t, len(first.Messages) != 0 && first.Messages[0].Role == "system", "wire messages = %+v, want the captured composed prompt first", first.Messages)
		{
			want, err := json.Marshal(rec.Admission.Execution.SystemPrompt)
			testassert.Require(t, err == nil && string(first.Messages[0].Content) == string(want), "wire system message = %s, want the captured composed prompt %s", first.Messages[0].Content, want)
		}
		testassert.Equal(t, e.server.authAt(0), "Bearer production-secret-1")
		testassert.Equal(t, e.server.traceAt(0), "trace-1")
		// The merged sidecar layers reach the wire body as decoded members,
		// not whole-body substrings: the exact-number decode keeps the
		// provider's number lexeme and both sidecar string values comparable.
		decoder := json.NewDecoder(strings.NewReader(e.server.bodyAt(0)))
		decoder.UseNumber()
		var wire map[string]any
		testassert.NoError(t, decoder.Decode(&wire))
		testassert.Require(t, wire["provider_side"] == "provider-value" && wire["model_side"] == "model-value" && wire["provider_number"] == json.Number("9007199254740993"), "wire body sidecar members = (%v, %v, %v), want the merged provider and model sidecar layers with the exact number lexeme", wire["provider_side"], wire["model_side"], wire["provider_number"])
		if lastMessageRole(e.server.bodyAt(1)) != "tool" {
			t.Fatalf("follow-up request = %s, want the tool result continuation", e.server.bodyAt(1))
		}
	})
}

func TestProductionKeyResolution(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("key-ws"), "worker")
		testassert.NoError(t, err)

		// A required credential that resolves empty rejects admission with
		// the retained provider/env-name diagnostic; no Operation exists.
		e.t.Setenv("PRODUCTION_TEST_KEY", "")
		err = submitExpectFailure(r, session.Identity.SessionID, "op-missing", "please write")
		testassert.Require(t, errors.Is(err, config.ErrMissingEnvVar) && errors.Is(err, harness.ErrInvalid) && strings.Contains(err.Error(), "PRODUCTION_TEST_KEY") && strings.Contains(err.Error(), `for provider "prov"`), "missing credential submit = %v, want the retained diagnostic wrapping both identities; missing credential diagnostic = %v, want the env name and provider identity", err, err)
		{
			err := readOperationError(ctx, r, session.Identity.SessionID, "op-missing")
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "rejected admission left %v, want no Operation", err)
		}

		// Setting the variable lets the next admission succeed; unsetting it
		// after the prepared admission leaves the captured transport working:
		// both the parked first request and the follow-up carry the captured
		// key, and the turn completes.
		e.t.Setenv("PRODUCTION_TEST_KEY", "production-secret-1")
		e.server.armGate()
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "please write")
		e.server.awaitRequests(1)
		e.t.Setenv("PRODUCTION_TEST_KEY", "")
		e.server.releaseGate()
		awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		if e.server.requests() != 2 {
			t.Fatalf("model requests = %d, want both turns", e.server.requests())
		}
		for i := 0; i < 2; i++ {
			{
				got := e.server.authAt(i)
				testassert.Require(t, got == "Bearer production-secret-1", "request %d authorization = %q, want the captured key", i, got)
			}
		}
	})
}

// TestProductionAdmissionRacingParkedManagedWriteUsesPublishedPair parks an
// accepted managed-key write mid-file and starts a real admission: the
// admission waits at configuration capture, then runs on the writer's
// published pair — the published generation and the real model transport's
// new key.
func TestProductionAdmissionRacingParkedManagedWriteUsesPublishedPair(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		// The fixture credential is managed-empty: no shell export, one empty
		// .env line loaded at startup, so the connect's supplied key is the
		// one accepted managed write.
		_ = os.Unsetenv("PRODUCTION_TEST_KEY")
		writeDotEnv(t, e.home, "PRODUCTION_TEST_KEY=\n")
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		testassert.NoError(t, err)
		defer r.Close(ctx)
		testassert.Require(t, r.managedEnv != nil && r.managedEnv.IsManaged("PRODUCTION_TEST_KEY"), "the fixture credential is not managed")
		session, err := r.createSession(ctx, e.workspace("race-ws"), "worker")
		testassert.NoError(t, err)

		probe := installOwningSyncProbe(t, r.managedEnv.Path())
		defer probe.restore()
		probe.park = true
		release := sync.OnceFunc(probe.releaseProbe)
		defer release()
		key := "sk-published-pair"
		type connectResult struct {
			mutation protocol.ProviderMutation
			err      error
		}
		connectDone := make(chan connectResult, 1)
		go func() {
			mutation, err := r.connectProvider(ctx, "prov", &key)
			connectDone <- connectResult{mutation, err}
		}()
		probe.awaitWriteStarted(t)

		// The admission must establish its wait at configuration capture
		// while the writer holds the publication/capture ownership.
		submitDone := make(chan error, 1)
		go func() { submitDone <- submitExpectFailure(r, session.Identity.SessionID, "op-race", "please write") }()
		awaitConfigurationCaptureWait(t)
		release()
		connect := <-connectDone
		testassert.Require(t, connect.err == nil, "connect: %v", connect.err)
		select {
		case err := <-submitDone:
			testassert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the admission never resumed after the write published")
		}
		e.server.awaitRequests(1)
		testassert.Equal(t, e.server.authAt(0), "Bearer "+key)
		record := awaitOperation(t, r, session.Identity.SessionID, "op-race", harness.OperationSuccess)
		testassert.Equal(t, record.Admission.Execution.ConfigurationRevision, connect.mutation.ConfigurationRevision.Generation)
		{
			published := r.config.current()
			testassert.Require(t, published != nil && strconv.FormatUint(published.generation, 10) == connect.mutation.ConfigurationRevision.Generation, "live publication = %v, want the writer's published generation %s", published, connect.mutation.ConfigurationRevision.Generation)
		}
	})
}

// TestProductionAdmissionUsesReadyRevisionDuringParkedBuild parks an unrelated
// candidate build in a validator and admits real work: the ready revision's
// capture, transport key, and generation stay available until the build
// publishes.
func TestProductionAdmissionUsesReadyRevisionDuringParkedBuild(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		var armed atomic.Bool
		arrived := make(chan struct{}, 1)
		ungate := make(chan struct{})
		releaseGate := sync.OnceFunc(func() { close(ungate) })
		var opens atomic.Int64
		gate := servicePlugin("gate", &opens, func(json.RawMessage) error {
			if !armed.Load() {
				return nil
			}
			select {
			case arrived <- struct{}{}:
			default:
			}
			<-ungate
			return nil
		})
		ctx := context.Background()
		r, err := e.openWithPlugins(ctx, e.hookedHook(), &parkingHook{}, gate)
		testassert.NoError(t, err)
		// Teardown order: the ctx-blind validator gate is released first, so
		// the buffered Reload and Submit result sends complete before the
		// ordinary owner/store close joins both admission calls. A broken
		// capture therefore yields the named assertion failure, not a hung
		// disposal.
		defer r.Close(ctx)  // registered first: runs last
		defer releaseGate() // registered second: runs first

		session, err := r.createSession(ctx, e.workspace("ready-ws"), "worker")
		testassert.NoError(t, err)

		armed.Store(true)
		reloadDone := make(chan error, 1)
		go func() {
			_, err := r.Reload(ctx)
			reloadDone <- err
		}()
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the reload never reached the parked validator")
		}
		submitDone := make(chan error, 1)
		go func() {
			_, err := submitRawThroughRuntime(ctx, r, session.Identity.SessionID, "op-ready", "please write", harness.MessageModeRegular)
			submitDone <- err
		}()
		select {
		case err := <-submitDone:
			testassert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("admission did not use the ready revision while the build was parked")
		}
		e.server.awaitRequests(1)
		testassert.Equal(t, e.server.authAt(0), "Bearer production-secret-1")
		record := awaitOperation(t, r, session.Identity.SessionID, "op-ready", harness.OperationSuccess)
		testassert.Equal(t, record.Admission.Execution.ConfigurationRevision, "1")
		releaseGate()
		testassert.NoError(t, <-reloadDone)
	})
}

func TestProductionModelSelectionRejection(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		testassert.NoError(t, err)
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
			testassert.Require(t, err == nil, "createSession(%s): %v", row.agent, err)
			err = submitExpectFailure(r, session.Identity.SessionID, row.opID, "hello")
			testassert.Require(t, errors.Is(err, row.wantClass) && strings.Contains(err.Error(), row.wantText), "%s submit = %v, want %v with %q", row.agent, err, row.wantClass, row.wantText)
			{
				err := readOperationError(ctx, r, session.Identity.SessionID, row.opID)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "%s rejection left %v, want no Operation", row.agent, err)
			}
		}

		// Both rejections happened before any scope opened.
		r.workspaces.mu.Lock()
		live := len(r.workspaces.live)
		r.workspaces.mu.Unlock()
		testassert.Equal(t, live, 0)
	})
}

func TestProductionReloadCapture(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		hook := e.hookedHook()
		r, err := e.open(ctx, hook, &parkingHook{})
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("reload-ws"), "worker")
		testassert.NoError(t, err)

		// The in-flight admission stays on its captured revision.
		e.server.armGate()
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "parked")
		e.server.awaitRequests(1)
		writeServiceFile(t, e.configPath, productionConfigDocument(e.server.URL, "T2"))
		{
			revision, err := r.Reload(ctx)
			testassert.Require(t, err == nil && revision == "2", "Reload = (%q, %v), want 2", revision, err)
		}
		e.server.releaseGate()
		{
			rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
			testassert.Equal(t, rec.Admission.Execution.ConfigurationRevision, "1")
		}
		// The next admission prepares on the new revision and its hook reads
		// the new tag; the fresh Session rules out any post-terminal drain
		// racing the submit into a steering buffer.
		next, err := r.createSession(ctx, e.workspace("reload-ws-2"), "worker")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, next.Identity.SessionID, "op-2", "hello")
		{
			rec := awaitOperation(t, r, next.Identity.SessionID, "op-2", harness.OperationSuccess)
			testassert.Equal(t, rec.Admission.Execution.ConfigurationRevision, "2")
		}
		{
			got := hook.calls()
			testassert.Require(t, slices.Equal(got, []hookCall{{revision: "1", tag: "T1"}, {revision: "2", tag: "T2"}}), "hook calls = %v, want each admission's own revision tag", got)
		}
	})
}

func TestProductionQueuedAndFork(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("queue-ws"), "worker")
		testassert.NoError(t, err)

		// The queued message buffers over the blocked Operation and delivers
		// a working execution when the drain reaches it.
		e.server.armGate()
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "parked")
		e.server.awaitRequests(1)
		submitQueuedThroughRuntime(t, r, session.Identity.SessionID, "op-2", "please write")
		e.server.releaseGate()
		awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		awaitOperation(t, r, session.Identity.SessionID, "op-2", harness.OperationSuccess)
		awaitIdleSession(t, r, session.Identity.SessionID)

		// Fork admission runs the destination Operation through the same
		// production preparation with its zero session start sampled at
		// compose time.
		boundary := ""
		entries, err := e.store.ReadEntries(ctx, session.Identity.SessionID, 0)
		testassert.NoError(t, err)
		for _, entry := range entries {
			if entry.Kind == harness.EntryInput && entry.OperationID == "op-2" {
				boundary = entry.ID
				break
			}
		}
		testassert.Require(t, boundary != "", "no admitted input entry was found for the fork boundary")
		var forked harness.SessionRecord
		testassert.NoError(t, r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
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
		}))
		rec := awaitOperation(t, r, forked.Identity.SessionID, "op-fork", harness.OperationSuccess)
		testassert.Require(t, rec.Admission.Execution.ConfigurationRevision == "1" && strings.HasSuffix(rec.Admission.Execution.SystemPrompt, "|hooked"), "forked Operation capture = %+v, want the hooked production capture", rec.Admission.Execution)
	})
}

func TestProductionHookCapture(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		hook := e.hookedHook()
		r, err := e.open(ctx, hook, &parkingHook{})
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("hook-ws"), "worker")
		testassert.NoError(t, err)
		// The hook's narrowed description survives into the committed
		// capture.
		hook.set(nil, func(c harness.ExecutionCapture) harness.ExecutionCapture {
			c.Tools[0].Description = "narrowed by the hook"
			return c
		})
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-narrow", "hello")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-narrow", harness.OperationSuccess)
		testassert.Equal(t, rec.Admission.Execution.Tools[0].Description, "narrowed by the hook")

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
			testassert.Require(t, err == nil, "createSession(%s): %v", row.name, err)
			err = submitExpectFailure(r, rowSession.Identity.SessionID, "op-"+row.name, "hello")
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "%s submit = %v, want the ErrInvalid rejection", row.name, err)
			{
				err := readOperationError(ctx, r, rowSession.Identity.SessionID, "op-"+row.name)
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "%s rejection left %v, want no Operation", row.name, err)
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
		testassert.NoError(t, err)
		testassert.NoError(t, r.Close(ctx))
	})
}

func TestProductionBlockedShutdown(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		park := &parkingHook{}
		r, err := e.open(ctx, e.hookedHook(), park)
		testassert.NoError(t, err)
		session, err := r.createSession(ctx, e.workspace("shutdown-ws"), "worker")
		testassert.NoError(t, err)

		// The armed hook parks the admission: an in-flight admitted call
		// blocks Close until released, and the joined shutdown completes.
		release := make(chan struct{})
		used := make(chan struct{}, 1)
		park.arm(release, used)
		done := make(chan error, 1)
		go func() {
			done <- submitExpectFailure(r, session.Identity.SessionID, "op-parked", "hello")
		}()
		select {
		case <-used:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked admission never reached the hook")
		}
		closeDone := make(chan error, 1)
		go func() { closeDone <- r.Close(ctx) }()
		select {
		case err := <-closeDone:
			t.Fatalf("Close returned while the admission was parked (%v); shutdown joined nothing", err)
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		testassert.NoError(t, <-closeDone)
		{
			err := <-done
			testassert.Require(t, err != nil, "the parked admission succeeded after the owner canceled; want the cancellation failure")
		}
	})

	// With a pending steering head, the joined shutdown never starts a
	// successor: the buffered head is discarded and becomes no Operation.
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		testassert.NoError(t, err)
		session, err := r.createSession(ctx, e.workspace("shutdown-successor-ws"), "worker")
		testassert.NoError(t, err)
		e.server.armGate() // the run parks at its first model request
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-run", "hello")
		e.server.awaitRequests(1)
		steering, err := submitRawThroughRuntime(ctx, r, session.Identity.SessionID, "op-steer", "steer", harness.MessageModeRegular)
		testassert.NoError(t, err)
		testassert.Check(t, steering.Disposition == harness.DispositionSteering, "steering submit disposition = %q, want steering", steering.Disposition)
		testassert.NoError(t, r.Close(ctx))
		e.server.releaseGate()
		{
			_, err := e.store.ReadRegister(ctx, harness.RegisterKey{
				SessionID: session.Identity.SessionID, Kind: harness.RegisterOperation, OperationID: "op-steer",
			})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "shutdown started a successor: %v", err)
		}
	})
}

func TestProductionDuplicateTools(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("dup-ws"), "duper")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "hello")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		{
			got := toolNames(rec.Admission.Execution.Tools)
			testassert.Require(t, slices.Equal(got, []string{"prod_read", "prod_write"}), "advertisement = %v, want duplicates deduped by first occurrence", got)
		}
	})
}

func TestProductionReadonlyAdvertisement(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("ro-ws"), "ro")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "hello")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		testassert.Require(t, rec.Admission.Execution.Readonly, "capture readonly = %v, want the agent's constraint", rec.Admission.Execution.Readonly)
		{
			got := toolNames(rec.Admission.Execution.Tools)
			testassert.Require(t, slices.Equal(got, []string{"prod_read"}), "advertisement = %v, want the mutation tool dropped by the hard constraint", got)
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
	testassert.NoError(t, err)
	p := newPreparation(svc, c, runtimeScope, newWorkspaceScopes(owner, c, []*scope{runtimeScope}, obs), sh.home, nil, nil, nil, nil)
	ref := model.ModelRef{Provider: "prov", Model: "m"}
	provider, entry, err := snapshot.snapshot.catalog.Lookup(catalog.ModelRef{Provider: ref.Provider, Model: ref.Model})
	testassert.NoError(t, err)

	resolved, err := p.resolveTransport(provider, entry, ref, "resolved-key")
	testassert.NoError(t, err)
	testassert.Require(t, resolved.Model == ref && resolved.BaseURL == "https://prov.test/v1" && resolved.APIKey == "resolved-key" && resolved.WireSystemRole == "user", "resolved identity = %+v, want the captured endpoint, key, and model; got %v, want %v", resolved, resolved.WireSystemRole, "user")
	testassert.Require(t, resolved.StreamedUsage, "streamed usage = false, want the model-level true override of the provider-level false")
	testassert.Require(t, resolved.Headers["X-Trace"] == "t1", "resolved headers = %v, want the captured transport headers", resolved.Headers)
	testassert.Equal(t, string(resolved.ProviderExtraBody["provider_side"]), `"p"`)
	testassert.Equal(t, string(resolved.ProviderExtraBody["big"]), "9007199254740993")
	testassert.Require(t, string(resolved.ModelExtraBody["precise"]) == "1.25" && resolved.ProtocolFamily == "fam" && slices.Equal(resolved.MustPreserve, []string{"proto_keep"}) && resolved.Drop["drop_me"] && len(resolved.Drop) == 1, "got %v, want %v; resolved protocol metadata = family %q must-preserve %v drop %v, want the model-level effective values", string(resolved.ModelExtraBody["precise"]), "1.25", resolved.ProtocolFamily, resolved.MustPreserve, resolved.Drop)
	wantFamilies := map[model.ModelRef]string{
		{Provider: "prov", Model: "m"}:     "fam",
		{Provider: "prov", Model: "sib"}:   "fam",
		{Provider: "prov", Model: "other"}: "else",
	}
	testassert.Require(t, len(resolved.SourceFamilies) == len(wantFamilies), "source families = %v, want exactly the same-provider map %v", resolved.SourceFamilies, wantFamilies)
	for k, v := range wantFamilies {
		if resolved.SourceFamilies[k] != v {
			t.Fatalf("source family of %s = %q, want %q", k.String(), resolved.SourceFamilies[k], v)
		}
	}

	// The system message encodes with the effective wire role.
	body, _, err := model.Encode(resolved, model.Request{Messages: []model.Message{{Role: model.RoleSystem, Content: []model.ContentPart{{Kind: model.PartText, Text: "sys"}}}}}, nil)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(string(body), `"role":"user"`), "system message wire role = %s, want the effective user role", body)

	// The wire-debug opt-in: off by default; enabled it creates the
	// home-based private directory and supplies it; a creation failure
	// disables diagnostics, never execution.
	testassert.Equal(t, resolved.WireDebugDir, "")
	t.Setenv(debugWireEnv, "1")
	enabled, err := p.resolveTransport(provider, entry, ref, "resolved-key")
	testassert.NoError(t, err)
	want := filepath.Join(sh.home, ".lightcode", "debug", "wire")
	testassert.Equal(t, enabled.WireDebugDir, want)
	{
		info, err := os.Stat(want)
		testassert.Require(t, err == nil && info.IsDir(), "wire debug directory = (%v, %v), want the created private directory", info, err)
	}
	blocked := filepath.Join(sh.home, "file.txt")
	testassert.NoError(t, os.WriteFile(blocked, []byte("a file, not a directory"), 0o600))
	broken := &preparation{home: filepath.Join(blocked, "sub")}
	disabled, err := broken.resolveTransport(provider, entry, ref, "resolved-key")
	testassert.NoError(t, err)
	testassert.Equal(t, disabled.WireDebugDir, "")
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
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("run-ws"), "runner")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "please run")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		if !slices.Equal(toolNames(rec.Admission.Execution.Tools), []string{"run_command"}) {
			t.Fatalf("advertisement = %v, want the command tool", toolNames(rec.Admission.Execution.Tools))
		}
		// The settled result content proves the command ran through the
		// concrete opener's dispatch.
		entries, err := e.store.ReadEntries(ctx, session.Identity.SessionID, 0)
		testassert.NoError(t, err)
		var content string
		for _, entry := range entries {
			if entry.Kind != harness.EntryToolResult {
				continue
			}
			var wire struct {
				Content string `json:"content"`
			}
			testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
			content = wire.Content
		}
		testassert.Equal(t, content, "production-command-row")
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
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("adapt-ws"), "adapted")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "hello")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)

		prompt := rec.Admission.Execution.SystemPrompt
		testassert.Require(t, strings.Contains(prompt, "PROD-ADAPTATION-BLOCK") && strings.Contains(prompt, "ADDITION-SAFETY-TEXT"), "captured prompt misses the adaptation's coaching block: %.200s; captured prompt misses the adaptation's section addition: %.200s", prompt, prompt)
		{
			got := toolNames(rec.Admission.Execution.Tools)
			testassert.Require(t, slices.Equal(got, []string{"prod_write"}), "advertisement = %v, want the adaptation's excluded tool absent", got)
		}

		// The exclusion is the adaptation's work, not the hard constraints:
		// the capture's readonly/write-dir fields stay the definition's.
		testassert.Require(t, !rec.Admission.Execution.Readonly && rec.Admission.Execution.WriteDir == "", "capture constraints = (%v, %q), want the definition's untouched values", rec.Admission.Execution.Readonly, rec.Admission.Execution.WriteDir)
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
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("keyless-ws"), "keyless")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "please write")
		awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		if e.server.requests() != 2 {
			t.Fatalf("model requests = %d, want the tool-call turn and the follow-up", e.server.requests())
		}
		testassert.Equal(t, e.server.authAt(0), "")
		data, err := os.ReadFile(filepath.Join(e.workspace("keyless-ws"), "out.txt"))
		testassert.Require(t, err == nil && string(data) == "written by production", "prod_write effect = (%q, %v), want the written content", data, err)
	})
}

// TestExecutionOpenerToolWriteWaitsForRestoreInterval proves the interval
// with a real Harness-mediated mutating tool: the parked restore owns the
// Session's artifact interval, the racing admission still commits
// (Harness-owned), and the committed opener — registered as a waiter in the
// owned registry state — cannot begin the prod_write effect until the restore
// released the interval. The durable Operation outcome and the actual file
// content prove the ordering: the restore's FIFO content lands first, the
// tool's real write last.
func TestExecutionOpenerToolWriteWaitsForRestoreInterval(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		testassert.NoError(t, err)
		defer r.Close(ctx)
		workspace := e.workspace("interval-ws")
		session, err := r.createSession(ctx, workspace, "worker")
		testassert.NoError(t, err)
		sessionID := session.Identity.SessionID
		op := submitCodeOperation(t, r, sessionID, "op-1", "first")
		awaitRestorableSession(t, r, sessionID)

		file := filepath.Join(workspace, "out.txt") // the prod_write target
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, sessionID, op.Admission.AdmittedEntry.EntryID), file, "written by production")

		park, outcomes := parkRestore(t, r, sessionID, "op-1", fifo)
		defer park.flush()

		admitted := make(chan error, 1)
		go func() {
			res, err := submitRawThroughRuntime(ctx, r, sessionID, "op-write", "write it", harness.MessageModeRegular)
			if err != nil {
				admitted <- err
				return
			}
			if res.Disposition != harness.DispositionAdmitted {
				admitted <- fmt.Errorf("disposition %q, want admitted", res.Disposition)
				return
			}
			admitted <- nil
		}()
		select {
		case err := <-admitted:
			testassert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("admission blocked during a parked restore: admission stays Harness-owned")
		}

		// The owned registry state is the deterministic parked-opener proof:
		// the restore holds the token and the opener's acquisition is
		// registered as its waiter. The tool effect runs strictly after the
		// opener wins, so this state cannot coexist with a begun write.
		awaitOpenerRegistered(t, r, sessionID)

		// The real write has not begun: the opener cannot have won the token.
		{
			data, err := os.ReadFile(file)
			testassert.Require(t, err == nil && len(data) == 0, "the tool write began inside the restore interval: (%q, %v)", data, err)
		}

		park.release("restored")
		var parkedRestore restoreOutcome
		select {
		case parkedRestore = <-outcomes:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked restore never returned after its FIFO completed")
		}
		testassert.Require(t, parkedRestore.err == nil && len(parkedRestore.result.Restored) == 1 && parkedRestore.result.Restored[0] == file, "parked restore = %v; restored = %v, want the parked group's restore", parkedRestore.err, parkedRestore.result.Restored)

		// The opener won the released interval: the real tool effect ran and
		// the durable Operation settled.
		awaitOperation(t, r, sessionID, "op-write", harness.OperationSuccess)
		{
			data, err := os.ReadFile(file)
			testassert.Require(t, err == nil && string(data) == "written by production", "file after the tool write = (%q, %v), want the tool's content", data, err)
		}
	})
}
