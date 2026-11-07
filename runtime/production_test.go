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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/config"
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
  "extensions": {"hooks": {"enabled": true, "tag": "` + tag + `"}}
}`
}

// "duper" keeps duplicate configured tool names in an order that
// discriminates first-occurrence from last-occurrence dedupe, "ro" is the
// readonly constraint sibling, "runner" exercises the command row, "keyless"
// rides the credential-free provider, "adapted" selects the custom
// preparation contribution, "nomodel" has no model at all, and "absent" selects
// a catalog-missing model — the discriminating sibling of the empty identity.
const productionAgentsDocument = `{
  "worker": {"model": "prov/m", "system_prompt": "simple", "tools": ["prod_write", "prod_read"], "capabilities": ["hook.first", "park.second"]},
  "duper": {"model": "prov/m", "system_prompt": "simple", "tools": ["prod_read", "prod_write", "prod_read"], "capabilities": []},
  "ro": {"model": "prov/m", "system_prompt": "simple", "tools": ["prod_write", "prod_read"], "readonly": true},
  "runner": {"model": "prov/m", "system_prompt": "simple", "tools": ["run_command"], "capabilities": []},
  "keyless": {"model": "open/km", "system_prompt": "simple", "tools": ["prod_write"]},
  "nomodel": {"system_prompt": "simple"},
  "absent": {"model": "prov/absent", "system_prompt": "simple"}
}`

// useFixtureStore points the owner's store construction at one externally
// built store — the memory variant's store, or a real SQLite engine opened
// outside the data directory — for the fixture run. The store's lifetime stays
// the fixture's: the owner's storage close disposes nothing the row may still
// read, and eachProductionStore closes the store at the run's end.
func useFixtureStore(t *testing.T, store harness.Storage) {
	t.Helper()
	previous := openStore
	openStore = func(ctx context.Context, dataDir string) (harness.Storage, func() error, error) {
		return store, func() error { return nil }, nil
	}
	t.Cleanup(func() { openStore = previous })
}

func hookPlugin(hook *recordHook) extension.Native {
	capability := extension.HookCapability("hook.first", extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure))
	return testNative("hooks", extension.LifetimeHarness, []extension.Capability{capability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"hook.first": {Preparation: hook.Prepare}}}, nil
	})
}

// parkPlugin declares the suite's parking preparation contribution.
func parkPlugin(park *parkingHook) extension.Native {
	capability := extension.HookCapability("park.second", extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure))
	return testNative("park", extension.LifetimeHarness, []extension.Capability{capability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"park.second": {Preparation: park.Prepare}}}, nil
	})
}

// fileWriteTool is the concrete mutation tool of the production suite.
type fileWriteTool struct{}

func (t fileWriteTool) describe(_ context.Context, input extension.DescribeInput) (extension.ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        "prod_write",
		Description: "writes one file in the workspace",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
	})
	if err != nil {
		return extension.ToolDescription{}, err
	}
	return extension.ToolDescription{Definition: definition, Available: !input.Constraints.Readonly || input.Constraints.WriteDir != ""}, nil
}

func (t fileWriteTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t fileWriteTool) Prepare(_ context.Context, tc extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args.Path == "" || args.Content == "" {
		return extension.ToolPlan{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "prod_write requires path and content"}}}
	}
	target := filepath.Clean(filepath.Join(tc.Workspace, args.Path))
	data, _ := json.Marshal(args)
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: "file.write", Target: target}}, CanonicalWorkspace: filepath.Clean(tc.Workspace), Data: data}
}

func (t fileWriteTool) Execute(ctx context.Context, tc extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(plan.Data, &args); err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	target := filepath.Clean(filepath.Join(tc.Workspace, args.Path))
	if err := ctx.Err(); err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "interrupted"}}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	if err := os.WriteFile(target, []byte(args.Content), 0o600); err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "written"}}
}

// fileReadTool is the concrete read-only sibling.
type fileReadTool struct{}

func (t fileReadTool) describe(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{Name: "prod_read", Description: "reads one file in the workspace", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)})
	if err != nil {
		return extension.ToolDescription{}, err
	}
	return extension.ToolDescription{Definition: definition, Available: true}, nil
}

func (t fileReadTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t fileReadTool) Prepare(_ context.Context, tc extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil || args.Path == "" {
		return extension.ToolPlan{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "prod_read requires path"}}}
	}
	target := filepath.Clean(filepath.Join(tc.Workspace, args.Path))
	data, _ := json.Marshal(args)
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: "file.read", Target: target}}, CanonicalWorkspace: filepath.Clean(tc.Workspace), Data: data}
}

func (t fileReadTool) Execute(ctx context.Context, tc extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(plan.Data, &args); err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	target := filepath.Clean(filepath.Join(tc.Workspace, args.Path))
	if err := ctx.Err(); err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "interrupted"}}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: string(data)}}
}

// productionToolsPlugin declares the suite's concrete tools.
func productionToolsPlugin() extension.Native {
	native := testNative("prodtools", extension.LifetimeHarness,
		[]extension.Capability{extension.ToolCapability("prod_write"), extension.ToolCapability("prod_read")}, nil,
		func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				"prod_write": {Tool: fileWriteTool{}},
				"prod_read":  {Tool: fileReadTool{}},
			}}, nil
		})
	native.Describe = map[string]extension.ToolDescribe{
		"prod_write": fileWriteTool{}.describe,
		"prod_read":  fileReadTool{}.describe,
	}
	return native
}

// testAdaptation is the custom test preparation contribution.
type testAdaptation struct{}

func (testAdaptation) Prepare(_ context.Context, _ extension.CallContext, input extension.PreparationInput) (extension.PreparationConsequence, error) {
	prompt := input.Prompt + "\n\n  PROD-ADAPTATION-BLOCK\n"
	return extension.PreparationConsequence{Prompt: &prompt, ToolNames: []string{"prod_write"}}, nil
}

func prodAdaptationPlugin() extension.Native {
	capability := extension.HookCapability("model_adaptation", extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure))
	return testNative("adapt", extension.LifetimeHarness, []extension.Capability{capability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"model_adaptation": {Preparation: testAdaptation{}.Prepare}}}, nil
	})
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
	t              *testing.T
	home           string
	dataDir        string
	configPath     string
	server         *productionModelServer
	store          harness.Storage
	identity       string
	globalRulesDir string
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
	writeAgentDefinitions(t, configPath, productionAgentsDocument)
	return &productionEnv{t: t, home: home, dataDir: dataDir, configPath: configPath, server: server, store: store}
}

func (e *productionEnv) open(ctx context.Context, hook *recordHook, park *parkingHook) (*Runtime, error) {
	return e.openWithPlugins(ctx, hook, park)
}

// openPlugins opens the fixture's exact plugin list over its data and config
// roots, with the owner's store construction pointed at the fixture's store.
func (e *productionEnv) openPlugins(ctx context.Context, natives ...extension.Native) (*Runtime, error) {
	e.t.Helper()
	useFixtureStore(e.t, e.store)
	data, err := os.ReadFile(e.configPath)
	testassert.NoError(e.t, err)
	var doc struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Open(ctx, Options{DataDir: e.dataDir, ConfigPath: e.configPath, Plugins: natives, Identity: e.identity, GlobalRulesDir: e.globalRulesDir})
	}
	hasAgents := false
	for _, native := range natives {
		id := native.Declaration.ID
		hasAgents = hasAgents || id == "agents"
		if _, exists := doc.Extensions[id]; !exists {
			enableFixturePlugin(e.t, e.configPath, id)
		}
	}
	if !hasAgents && sectionEnabled(doc.Extensions["agents"]) {
		natives = append(natives, fixtureAgentPlugin(natives...))
	}
	return Open(ctx, Options{DataDir: e.dataDir, ConfigPath: e.configPath, Plugins: natives, Identity: e.identity, GlobalRulesDir: e.globalRulesDir})
}

// openWithPlugins composes the suite's base plugin set plus extras — the
// adaptation row adds its custom preparation contribution this way.
func (e *productionEnv) openWithPlugins(ctx context.Context, hook *recordHook, park *parkingHook, extra ...extension.Native) (*Runtime, error) {
	e.t.Helper()
	plugins := []extension.Native{productionToolsPlugin(), hookPlugin(hook), parkPlugin(park)}
	return e.openPlugins(ctx, append(plugins, extra...)...)
}

func (e *productionEnv) hookedHook() *recordHook {
	return &recordHook{name: "hook.first", events: &traceLog{}, mutate: func(in extension.PreparationInput) extension.PreparationConsequence {
		prompt := in.Prompt + "|hooked"
		return extension.PreparationConsequence{Prompt: &prompt}
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
			toolName, toolArgs = "run_command", `{"command":"printf production-command-row"}`
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
		candidateBarrier = func() {
			if !armed.Load() {
				return
			}
			select {
			case arrived <- struct{}{}:
			default:
			}
			<-ungate
		}
		t.Cleanup(func() { candidateBarrier = nil })
		gate := servicePlugin("gate", &opens, acceptValidator)
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
		r.config.current().resources.workspaces.mu.Lock()
		live := len(r.config.current().resources.workspaces.live)
		r.config.current().resources.workspaces.mu.Unlock()
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
		writeAgentDefinitions(t, e.configPath, productionAgentsDocument)
		enableFixturePlugin(t, e.configPath, "prodtools")
		enableFixturePlugin(t, e.configPath, "park")
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
			testassert.Require(t, slices.Equal(got, []hookCall{{tag: "T1"}, {tag: "T2"}}), "hook calls = %v, want each admission's own settings tag", got)
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
		hook.set(nil, func(extension.PreparationInput) extension.PreparationConsequence {
			return extension.PreparationConsequence{Descriptions: map[string]string{"prod_write": "narrowed by the hook"}}
		})
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-narrow", "hello")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-narrow", harness.OperationSuccess)
		testassert.Equal(t, rec.Admission.Execution.Tools[0].Description, "narrowed by the hook")

		// Input identity and constraint mutation stays a non-effect: the
		// committed capture keeps the definition's values and the admission
		// succeeds. Every row runs on its own fresh Session, so no
		// post-terminal drain can race the submit into a steering buffer.
		for _, row := range []struct {
			name   string
			mutate func(in extension.PreparationInput) extension.PreparationConsequence
		}{
			{"model", func(in extension.PreparationInput) extension.PreparationConsequence {
				in.Model = model.ModelRef{Provider: "prov", Model: "other"}
				return extension.PreparationConsequence{}
			}},
			{"constraints", func(in extension.PreparationInput) extension.PreparationConsequence {
				in.Constraints = ToolConstraints{Readonly: true, WriteDir: "/somewhere-else"}
				return extension.PreparationConsequence{}
			}},
		} {
			hook.set(nil, row.mutate)
			rowSession, err := r.createSession(ctx, e.workspace("hook-"+row.name+"-ws"), "worker")
			testassert.Require(t, err == nil, "createSession(%s): %v", row.name, err)
			submitThroughRuntime(t, r, rowSession.Identity.SessionID, "op-"+row.name, "hello")
			rowRec := awaitOperation(t, r, rowSession.Identity.SessionID, "op-"+row.name, harness.OperationSuccess)
			testassert.Require(t, rowRec.Admission.Execution.Model == prepModelRef && !rowRec.Admission.Execution.Readonly && rowRec.Admission.Execution.WriteDir == "", "%s capture = (%s, %v, %q), want the definition's untouched identity and constraints", row.name, rowRec.Admission.Execution.Model.String(), rowRec.Admission.Execution.Readonly, rowRec.Admission.Execution.WriteDir)
		}
		hook.set(nil, nil)
	})
}

// TestProductionHostDefaultCapabilitiesSelectContribution proves the
// application-supplied default capability selection travels through the
// Agents constructor: a definition with no explicit selection inherits the
// host's default and its ordinary preparation contribution runs.
func TestProductionHostDefaultCapabilitiesSelectContribution(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		writeAgentDefinitions(t, e.configPath, `{"defaulted":{"model":"prov/m","system_prompt":"simple","tools":["prod_write"]}}`)
		enableFixturePlugin(t, e.configPath, "prodtools")
		useFixtureStore(t, e.store)
		tools := productionToolsPlugin()
		hooks := hookPlugin(e.hookedHook())
		r, err := Open(ctx, Options{
			DataDir:    e.dataDir,
			ConfigPath: e.configPath,
			Plugins:    []extension.Native{tools, hooks, agentsTestNativeWithDefaults([]string{"hook.first"}, tools, hooks)},
		})
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("default-cap-ws"), "defaulted")
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "hello")
		rec := awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)
		testassert.Require(t, slices.Equal(rec.Admission.Execution.Capabilities, []string{"hook.first"}), "defaulted capabilities = %v, want the host-selected default", rec.Admission.Execution.Capabilities)
		testassert.Require(t, strings.HasSuffix(rec.Admission.Execution.SystemPrompt, "|hooked"), "defaulted prompt = %.120s, want the selected contribution applied", rec.Admission.Execution.SystemPrompt)
	})
}

// TestProductionDefaultCapabilityForDisabledContributionFailsAdmission proves
// the constructor default is an unchecked selection: naming a registered but
// inactive capability opens the Runtime normally and rejects the later
// admission with no Operation published.
func TestProductionDefaultCapabilityForDisabledContribution(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		writeAgentDefinitions(t, e.configPath, `{"defaulted":{"model":"prov/m","system_prompt":"simple","tools":["prod_write"]}}`)
		enableFixturePlugin(t, e.configPath, "prodtools")
		useFixtureStore(t, e.store)
		tools := productionToolsPlugin()
		hooks := hookPlugin(e.hookedHook())
		park := parkPlugin(&parkingHook{})
		// The fixture enables hooks only; park stays installed but inactive.
		r, err := Open(ctx, Options{
			DataDir:    e.dataDir,
			ConfigPath: e.configPath,
			Plugins:    []extension.Native{tools, hooks, park, agentsTestNativeWithDefaults([]string{"park.second"}, tools, hooks, park)},
		})
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("disabled-default-ws"), "defaulted")
		testassert.NoError(t, err)
		_, err = submitRawThroughRuntime(ctx, r, session.Identity.SessionID, "op-disabled", "hello", harness.MessageModeRegular)
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "disabled default submit = %v, want harness.ErrInvalid", err)
		testassert.Require(t, errors.Is(readOperationError(ctx, r, session.Identity.SessionID, "op-disabled"), harness.ErrNotFound), "disabled default published an operation")
	})
}

// countingWorkspacePlugin is one real Workspace-scoped declaration whose
// factory counts construction, so a preparation test can observe whether
// admission reached Workspace construction.
func countingWorkspacePlugin(opens *atomic.Int64) extension.Native {
	capability := testServiceCapability("ws.probe")
	return testNative("wsprobe", extension.LifetimeWorkspace, []extension.Capability{capability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		opens.Add(1)
		return extension.Instance{Capabilities: map[string]extension.Implementation{"ws.probe": testServiceImplementation()}}, nil
	})
}
