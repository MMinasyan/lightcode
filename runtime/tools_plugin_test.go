package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/plugins/sqlite"
	"github.com/MMinasyan/lightcode/internal/plugins/tools"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/tool"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/plugins/jobs"
	"github.com/MMinasyan/lightcode/runtime"
)

// The tools-plugin composition tests: the real native tools plugin is
// assembled through the test-build OpenForTest bridge like the SQLite
// plugin, and its prepared tools run through a real Harness over real
// temporary SQLite — allow/deny at the automatic policy boundary, file
// effects and code-group creation on disk, and interruption recovery. The
// direct open of the plugin happens through the composed scope helper, where
// a test jobs plugin is registered and the runtime constructs the binding
// in-package; the plugin requires the jobs capability, so no test opens it
// with empty bindings.

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
	r, err := runtime.OpenForTest(ctx, e.dataDir, e.configPath, []runtime.Plugin{sqlite.Plugin(), fakeJobsPlugin(&fakeJobs{}), tools.Plugin()})
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

// The admitted-input identity the direct preparation tests prepare with.
const (
	testSessionID = "0123456789abcdef0123456789abcdef"
	testEntryID   = "fedcba9876543210fedcba9876543210"
)

// toolExportIDs are the tools plugin's tool exports in declaration order.
var toolExportIDs = []string{"read_file", "write_file", "edit_file", "apply_patch", "run_command", "process", "sleep"}

// fakeJobs is the scriptable jobs.Jobs test double: it records every call
// and serves scripted errors, reservations, and live results.
type fakeJobs struct {
	reserveErr error
	startErr   error
	live       []string

	reserves int
	aborted  []string
	started  []jobs.StartRequest
	reads    int
	kills    int
	lists    int
}

func (f *fakeJobs) Reserve() (string, error) {
	f.reserves++
	if f.reserveErr != nil {
		return "", f.reserveErr
	}
	return "b0b0b0b0", nil
}

func (f *fakeJobs) Abort(jobID string) { f.aborted = append(f.aborted, jobID) }

func (f *fakeJobs) Start(_ context.Context, req jobs.StartRequest) error {
	f.started = append(f.started, req)
	return f.startErr
}

func (f *fakeJobs) Read(_, _ string) (string, error) {
	f.reads++
	return "", nil
}

func (f *fakeJobs) Kill(_, _ string) error {
	f.kills++
	return nil
}

func (f *fakeJobs) List(string) string {
	f.lists++
	return ""
}

func (f *fakeJobs) Live(string) []string { return f.live }

// fakeJobsPlugin registers one test jobs capability under the id the tools
// plugin requires.
func fakeJobsPlugin(j *fakeJobs) runtime.Plugin {
	return runtime.Plugin{
		ID:       "jobs",
		Scope:    runtime.ScopeRuntime,
		Provides: []runtime.CapabilitySpec{runtime.Spec[jobs.Jobs]("jobs")},
		Open: func(context.Context, runtime.ScopeInfo, runtime.Bindings) (runtime.Instance, error) {
			return runtime.Instance{Values: map[string]any{"jobs": jobs.Jobs(j)}}, nil
		},
	}
}

// openToolsComposed opens the real tools plugin through the composed scope —
// the runtime constructs the jobs binding in-package — with one optional
// extra plugin composed in the same scope (the zero Plugin composes none),
// and returns its tool exports plus every bound capability value.
func openToolsComposed(t *testing.T, dataDir string, managedKeys []string, jobsPlugin, extra runtime.Plugin) (map[string]runtime.Tool, map[string]any) {
	t.Helper()
	plugins := []runtime.Plugin{jobsPlugin, tools.Plugin()}
	if extra.ID != "" {
		plugins = append(plugins, extra)
	}
	values, closeScope, err := runtime.ComposeScopeForTest(context.Background(), runtime.ScopeInfo{
		Kind:           runtime.ScopeRuntime,
		DataDir:        dataDir,
		ManagedEnvKeys: managedKeys,
	}, plugins)
	if err != nil {
		t.Fatalf("compose tools plugin: %v", err)
	}
	t.Cleanup(func() { _ = closeScope() })
	byID := make(map[string]runtime.Tool, len(toolExportIDs))
	for _, id := range toolExportIDs {
		value, ok := values[id]
		if !ok {
			t.Fatalf("composition is missing the tool export %q", id)
		}
		toolValue, ok := value.(runtime.Tool)
		if !ok {
			t.Fatalf("export %q supplies %T, not a runtime.Tool", id, value)
		}
		byID[id] = toolValue
	}
	return byID, values
}

func openTools(t *testing.T, dataDir string) map[string]runtime.Tool {
	t.Helper()
	byID, _ := openToolsComposed(t, dataDir, nil, fakeJobsPlugin(&fakeJobs{}), runtime.Plugin{})
	return byID
}

func openToolsWithKeys(t *testing.T, dataDir string, managedKeys []string) map[string]runtime.Tool {
	t.Helper()
	byID, _ := openToolsComposed(t, dataDir, managedKeys, fakeJobsPlugin(&fakeJobs{}), runtime.Plugin{})
	return byID
}

// TestComposedToolsPluginContract proves the plugin declaration and the
// composed open: seven tool exports, the strict jobs dependency, and every
// export usable as a runtime.Tool once a jobs provider is registered.
func TestComposedToolsPluginContract(t *testing.T) {
	p := tools.Plugin()
	if p.ID != "tools" || p.Scope != runtime.ScopeRuntime || p.ValidateConfig == nil || p.Open == nil {
		t.Fatalf("plugin declaration = %+v, want the Runtime-scoped tools plugin with a validator and an opener", p)
	}
	if len(p.Provides) != 7 {
		t.Fatalf("plugin declares %d exports, want the four file tools plus run_command, process, and sleep", len(p.Provides))
	}
	// Without a jobs provider the composed open fails, naming the required
	// capability: there is no degraded mode.
	if _, _, err := runtime.ComposeScopeForTest(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: t.TempDir()}, []runtime.Plugin{tools.Plugin()}); err == nil || !strings.Contains(err.Error(), `"jobs"`) {
		t.Fatalf("open without a jobs provider = %v, want the unresolved jobs dependency error", err)
	}
	byID, _ := openToolsComposed(t, t.TempDir(), nil, fakeJobsPlugin(&fakeJobs{}), runtime.Plugin{})
	if len(byID) != 7 {
		t.Fatalf("composed open returned %d tools, want all seven", len(byID))
	}
}

// callToolContext is the ToolContext the direct tests prepare with: a real
// temporary Workspace, a valid-looking admitted-input identity, the zero
// Invocation (defaults), and the given constraints.
func callToolContext(workspace string, constraints runtime.ToolConstraints) runtime.ToolContext {
	return runtime.ToolContext{
		Workspace:     workspace,
		AdmittedEntry: harness.EntryRef{SessionID: testSessionID, EntryID: testEntryID},
		Invocation:    runtime.Invocation{},
		Constraints:   constraints,
	}
}

func makeCall(t *testing.T, name, args string) model.ToolCall {
	t.Helper()
	call, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("NewToolCall: %v", err)
	}
	return call
}

// prepare prepares one call, returning the plan.
func prepare(t *testing.T, tool runtime.Tool, tc runtime.ToolContext, name, args string) harness.PreparedTool {
	t.Helper()
	return tool.Prepare(context.Background(), tc, makeCall(t, name, args))
}

// normalize runs one tool's normalization over raw argument bytes.
func normalize(t *testing.T, tool runtime.Tool, tc runtime.ToolContext, name, args string) (json.RawMessage, error) {
	t.Helper()
	return tool.Normalize(tc, makeCall(t, name, args))
}

// groupDir is the exact per-Operation code-group directory the mutating
// calls derive from the ToolContext's admitted-input identity.
func groupDir(dataDir, sessionID, entryID string) string {
	return filepath.Join(dataDir, "code", sessionID, entryID)
}

// scriptBackground is the test BackgroundServices over no Harness: StartJob
// runs the spawn inline (or surfaces the scripted failure); DeliverCompletion
// is a no-op.
type scriptBackground struct {
	startErr error
}

func (scriptBackground) LaunchChild(context.Context, harness.LaunchChildRequest) (harness.LaunchChildResult, error) {
	return harness.LaunchChildResult{}, errors.New("unexpected child launch in this fixture")
}

func (b scriptBackground) StartJob(ctx context.Context, _, _ string, spawn func(context.Context, string) error) error {
	if b.startErr != nil {
		return b.startErr
	}
	return spawn(ctx, "0123456789abcdef0123456789abcdef")
}

func (scriptBackground) DeliverCompletion(context.Context, string, string, string) error {
	return nil
}

// harnessBackground is the production-shaped BackgroundServices over the
// fixture Harness; DeliverCompletion signals after the harness accepted the
// delivery.
type harnessBackground struct {
	h         *harness.Harness
	delivered chan struct{}
}

func (b *harnessBackground) LaunchChild(ctx context.Context, req harness.LaunchChildRequest) (harness.LaunchChildResult, error) {
	return b.h.LaunchChildSession(ctx, req)
}

func (b *harnessBackground) StartJob(ctx context.Context, sessionID, jobID string, spawn func(context.Context, string) error) error {
	return b.h.StartJob(ctx, sessionID, jobID, spawn)
}

func (b *harnessBackground) DeliverCompletion(ctx context.Context, sessionID, completionID, content string) error {
	err := b.h.DeliverBackgroundCompletion(ctx, sessionID, completionID, content)
	if err == nil {
		select {
		case b.delivered <- struct{}{}:
		default:
		}
	}
	return err
}

// backgroundImmediatePattern is the retained complete immediate template;
// the captured group is the job ID.
var backgroundImmediatePattern = regexp.MustCompile("^Command running in the background with ID: `([0-9a-f]{8})`\\. You will be notified when it finishes\\. If your next steps do not depend on its output, continue with them; otherwise use sleep to wait and process to read the output\\.\\nRunning in the background: `[^`]*`\\.$")

// assertBackgroundImmediate validates one complete retained immediate
// template and returns its job ID.
func assertBackgroundImmediate(t *testing.T, content string) string {
	t.Helper()
	m := backgroundImmediatePattern.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("result %q does not match the retained background template", content)
	}
	return m[1]
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

// toolsCapture is the execution capture advertising the given tool names.
func toolsCapture(names []string) harness.ExecutionCapture {
	definitions := make([]model.ToolDefinition, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, model.ToolDefinition{Name: name, Parameters: json.RawMessage(`{"type":"object","properties":{}}`)})
	}
	return harness.ExecutionCapture{
		ConfigurationRevision: "1",
		Model:                 model.ModelRef{Provider: "prov", Model: "m"},
		SystemPrompt:          "composed tools",
		Tools:                 definitions,
	}
}

// toolsHarnessOpts varies one composed tools Harness: the jobs provider, the
// advertised tool set, the resolved permission policy, the background bridge,
// the captured invocation, and the scope's managed env keys.
type toolsHarnessOpts struct {
	jobsPlugin  runtime.Plugin
	advertise   []string
	permissions harness.PermissionPolicy
	background  runtime.BackgroundServices
	invocation  runtime.Invocation
	managedKeys []string
	// store, when non-nil, is the durable store behind the fixture's
	// Harness; the zero value opens a temporary SQLite store.
	store harness.Storage
	// extraPlugin, when non-empty, composes beside the tools plugin in the
	// same scope, and extraToolID is additionally bound as a tool export —
	// the fixture's seam for a caller-owned plugin's tool.
	extraPlugin runtime.Plugin
	extraToolID string
}

// toolsHarness is one real Harness over one durable store whose
// execution's normalization and preparation callbacks are the real plugin
// tool values, wired with the admitted identity and the durable capture's
// constraints exactly as production preparation wires them.
type toolsHarness struct {
	t         *testing.T
	h         *harness.Harness
	dataDir   string
	workspace string
	store     harness.Storage

	mu        sync.Mutex
	admission harness.EntryRef
}

func newToolsHarness(t *testing.T, modelFn func(context.Context, model.Request) (model.Stream, error)) *toolsHarness {
	t.Helper()
	return newToolsHarnessWith(t, modelFn, toolsHarnessOpts{})
}

func newToolsHarnessWith(t *testing.T, modelFn func(context.Context, model.Request) (model.Stream, error), opts toolsHarnessOpts) *toolsHarness {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	workspace := t.TempDir()

	store := opts.store
	if store == nil {
		opened, err := storage.OpenSQLite(filepath.Join(dataDir, "lightcode.db"))
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		t.Cleanup(func() { _ = opened.Close() })
		store = opened
	}

	jobsPlugin := opts.jobsPlugin
	if jobsPlugin.ID == "" {
		jobsPlugin = fakeJobsPlugin(&fakeJobs{})
	}
	byID, values := openToolsComposed(t, dataDir, opts.managedKeys, jobsPlugin, opts.extraPlugin)
	if opts.extraToolID != "" {
		value, ok := values[opts.extraToolID]
		if !ok {
			t.Fatalf("composition is missing the extra tool export %q", opts.extraToolID)
		}
		toolValue, ok := value.(runtime.Tool)
		if !ok {
			t.Fatalf("export %q supplies %T, not a runtime.Tool", opts.extraToolID, value)
		}
		byID[opts.extraToolID] = toolValue
	}
	advertise := opts.advertise
	if advertise == nil {
		advertise = []string{"read_file", "write_file", "edit_file", "apply_patch"}
	}

	owner, cancel := context.WithCancel(ctx)
	th := &toolsHarness{t: t, dataDir: dataDir, workspace: workspace, store: store}
	deps := harness.Dependencies{
		Storage: store,
		Prepare: func(_ context.Context, _ harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: toolsCapture(advertise),
				Open: func(_ context.Context, admission harness.OperationAdmission) (harness.Execution, error) {
					th.mu.Lock()
					th.admission = admission.AdmittedEntry
					th.mu.Unlock()
					tc := runtime.ToolContext{
						Workspace:     workspace,
						AdmittedEntry: admission.AdmittedEntry,
						Invocation:    opts.invocation,
						Constraints:   runtime.ToolConstraints{Readonly: admission.Execution.Readonly, WriteDir: admission.Execution.WriteDir},
						Background:    opts.background,
					}
					return harness.Execution{
						Model:       modelFn,
						Permissions: opts.permissions,
						NormalizeTool: func(call model.ToolCall) (json.RawMessage, error) {
							tool, ok := byID[call.Name]
							if !ok {
								return nil, fmt.Errorf("unknown tool %q", call.Name)
							}
							return tool.Normalize(tc, call)
						},
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							return byID[call.Name].Prepare(ctx, tc, call)
						},
					}, nil
				},
			}, nil
		},
	}
	// The composed jobs capability also declares the Harness job-stop seam;
	// wire it exactly as production wires it so background starts admit.
	if stopper, ok := values["job-stopper"].(harness.JobStopper); ok {
		deps.Jobs = stopper
	}
	h, err := harness.New(owner, deps)
	if err != nil {
		cancel()
		t.Fatalf("harness.New: %v", err)
	}
	th.h = h
	t.Cleanup(cancel)
	if bg, ok := opts.background.(*harnessBackground); ok {
		bg.h = h
	}
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
	r, err := runtime.OpenForTest(ctx, th.dataDir, e.configPath, []runtime.Plugin{sqlite.Plugin(), fakeJobsPlugin(&fakeJobs{}), tools.Plugin()})
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

func TestRunCommandPrepareTargets(t *testing.T) {
	dataDir := t.TempDir()
	byID := openTools(t, dataDir)
	ws := t.TempDir()

	t.Run("conclusive simple commands declare one target per segment", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{}), "run_command", `{"command":"echo one; echo two && echo three"}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want an executor plan", plan)
		}
		if len(plan.Permissions) != 3 {
			t.Fatalf("pairs = %+v, want one command.run pair per segment", plan.Permissions)
		}
		want := []string{"echo one", "echo two", "echo three"}
		for i, pair := range plan.Permissions {
			if pair.Permission != "command.run" || pair.Target != want[i] {
				t.Fatalf("pair %d = %+v, want command.run on %q", i, pair, want[i])
			}
		}
	})

	t.Run("everything else falls back to the one complete target", func(t *testing.T) {
		for _, tc := range []struct{ args, want string }{
			{`{"command":"printf ok # ; rm -rf scratch"}`, "printf ok # ; rm -rf scratch"},
			{`{"command":"echo a > b"}`, "echo a > b"},
			{`{"command":"cat <<EOF"}`, "cat <<EOF"},
			{`{"command":"FOO=bar ls"}`, "FOO=bar ls"},
			{`{"command":"echo a &&"}`, "echo a &&"},
			// Leading ASCII blanks/newlines are trimmed and every remaining
			// byte — the heredoc body and the trailing newline — preserved.
			{"{\"command\":\"  \\ncat <<EOF\\nbody\\nEOF\\n\"}", "cat <<EOF\nbody\nEOF\n"},
		} {
			plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{}), "run_command", tc.args)
			if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "command.run" {
				t.Fatalf("%s: pairs = %+v, want the single complete-command target", tc.args, plan.Permissions)
			}
			if plan.Permissions[0].Target != tc.want {
				t.Fatalf("%s: target = %q, want %q", tc.args, plan.Permissions[0].Target, tc.want)
			}
		}
	})

	t.Run("all-blank text takes the whole-command fallback with the original input", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{}), "run_command", `{"command":"   "}`)
		if len(plan.Permissions) != 1 || plan.Permissions[0].Target != "   " {
			t.Fatalf("pairs = %+v, want the unchanged original blank input as the one target", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "(No output)" {
			t.Fatalf("result = %+v, want the retained no-op run", outcome.Result)
		}
	})

	t.Run("empty command text is an immediate argument error", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{}), "run_command", `{"command":""}`)
		if plan.Execute != nil || plan.Immediate == nil {
			t.Fatalf("plan = %+v, want one immediate outcome", plan)
		}
		if plan.Immediate.Result.Status != model.ResultError || plan.Immediate.Result.Content != "run_command: command is required" {
			t.Fatalf("immediate = %+v, want the retained required error", plan.Immediate)
		}
	})

	t.Run("readonly declares the rewritten command and executes it", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{Readonly: true}), "run_command", `{"command":"git status --short"}`)
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "command.run" {
			t.Fatalf("pairs = %+v, want one command.run pair", plan.Permissions)
		}
		target := plan.Permissions[0].Target
		if !strings.HasPrefix(target, "git --no-pager --no-optional-locks") || strings.Contains(target, "status --short") == false {
			t.Fatalf("target = %q, want the rewritten read-only git command", target)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError {
			t.Fatalf("result = %+v, want the git failure settled as an error result", outcome.Result)
		}
	})

	t.Run("readonly plain command runs in the workspace", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{Readonly: true}), "run_command", `{"command":"pwd"}`)
		if len(plan.Permissions) != 1 || plan.Permissions[0].Target != "pwd" {
			t.Fatalf("pairs = %+v, want the unchanged pwd target", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || strings.TrimSpace(outcome.Result.Content) != ws {
			t.Fatalf("result = %+v, want pwd in the workspace", outcome.Result)
		}
	})

	t.Run("readonly rejection is an immediate error with the fixed text", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{Readonly: true}), "run_command", `{"command":"curl example.com"}`)
		if plan.Execute != nil || plan.Immediate == nil {
			t.Fatalf("plan = %+v, want one immediate outcome", plan)
		}
		if plan.Immediate.Result.Status != model.ResultError || !strings.Contains(plan.Immediate.Result.Content, "read-only agent") || strings.Contains(plan.Immediate.Result.Content, "curl") {
			t.Fatalf("immediate = %+v, want the fixed rejection that never echoes the command", plan.Immediate)
		}
	})

	t.Run("readonly blank and empty commands settle the fixed rejection", func(t *testing.T) {
		for _, args := range []string{`{"command":""}`, `{"command":"   "}`, "{\"command\":\" \\n\\t \"}"} {
			plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{Readonly: true}), "run_command", args)
			if plan.Execute != nil || plan.Immediate == nil {
				t.Fatalf("%s: plan = %+v, want one immediate outcome", args, plan)
			}
			if plan.Immediate.Result.Status != model.ResultError || plan.Immediate.Result.Content != tool.ReadOnlyRunCommandRejected {
				t.Fatalf("%s: immediate = %+v, want the fixed rejection that never echoes the command", args, plan.Immediate)
			}
		}
	})

	t.Run("readonly ignores a model timeout override", func(t *testing.T) {
		// cat on a FIFO blocks until the test's writer opens it at two
		// seconds: honoring the 1s override would time out, while the
		// retained captured default lets the command finish.
		fifo := filepath.Join(ws, "pipe")
		if err := exec.Command("mkfifo", fifo).Run(); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
		go func() {
			time.Sleep(2 * time.Second)
			f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
			if err == nil {
				_, _ = f.WriteString("done")
				_ = f.Close()
			}
		}()
		start := time.Now()
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{Readonly: true}), "run_command", `{"command":"cat pipe","timeout":1}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "done" {
			t.Fatalf("result = %+v, want the default timeout to let the command finish", outcome.Result)
		}
		if elapsed := time.Since(start); elapsed < 2*time.Second {
			t.Fatalf("elapsed %s, want the 1s override ignored in favor of the captured default", elapsed)
		}
	})
}

func TestRunCommandExecuteOutcomes(t *testing.T) {
	dataDir := t.TempDir()
	byID := openTools(t, dataDir)
	ws := t.TempDir()
	tc := callToolContext(ws, runtime.ToolConstraints{})

	t.Run("success carries the captured output", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"printf ok"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "ok" {
			t.Fatalf("result = %+v, want the command output", outcome.Result)
		}
	})

	t.Run("nonzero exit settles exactly as the legacy engine settles the ExitError", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"printf fail && exit 7"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError || outcome.Result.Content != "Error: Exit code 7\nfail" {
			t.Fatalf("result = %+v, want the engine's error settlement with the ExitError output", outcome.Result)
		}
	})

	t.Run("a configured timeout settles as an error result with the retained text", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"sleep 5","timeout":1}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError || !strings.HasPrefix(outcome.Result.Content, "Error: Exit code -1 (timeout)\n") {
			t.Fatalf("result = %+v, want the retained timeout text as an error result", outcome.Result)
		}
	})

	t.Run("cancellation settles as an interrupted result with the retained text", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"sleep 5"}`)
		outcomeCh := make(chan struct {
			Status model.ToolResultStatus
			Body   string
		}, 1)
		go func() {
			outcome := plan.Execute(ctx)
			outcomeCh <- struct {
				Status model.ToolResultStatus
				Body   string
			}{outcome.Result.Status, outcome.Result.Content}
		}()
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case outcome := <-outcomeCh:
			if outcome.Status != model.ResultInterrupted || outcome.Body != "command cancelled" {
				t.Fatalf("outcome = (%v, %q), want the retained cancellation text as interrupted", outcome.Status, outcome.Body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled command did not return")
		}
	})

	t.Run("output spills under the calling session's output directory", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"yes overflowing-output | head -c 20000"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("result = %+v, want success with the spill marker", outcome.Result)
		}
		wantPrefix := filepath.Join(dataDir, "code", testSessionID, "output", "cmd_output_")
		if !strings.Contains(outcome.Result.Content, "saved to: "+wantPrefix) {
			t.Fatalf("result = %q, want the spill marker under %q", outcome.Result.Content, wantPrefix)
		}
		marker := "saved to: "
		idx := strings.LastIndex(outcome.Result.Content, marker)
		path := outcome.Result.Content[idx+len(marker):]
		if end := strings.IndexAny(path, "]\n"); end >= 0 {
			path = path[:end]
		}
		data, err := os.ReadFile(strings.TrimSpace(path))
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		if len(data) != 20000 {
			t.Fatalf("spill size = %d, want the full 20000 output bytes", len(data))
		}
	})

	t.Run("the managed key is scrubbed while an unlisted key and the workspace cwd remain", func(t *testing.T) {
		t.Setenv("LIGHTCODE_TOOLS_PLUGIN_TEST", "env-value")
		t.Setenv("LIGHTCODE_TOOLS_PLUGIN_MANAGED", "managed-secret")
		runCommand := openToolsWithKeys(t, dataDir, []string{"LIGHTCODE_TOOLS_PLUGIN_MANAGED"})["run_command"]
		plan := prepare(t, runCommand, tc, "run_command", `{"command":"printf '%s|%s' \"$LIGHTCODE_TOOLS_PLUGIN_TEST\" \"$LIGHTCODE_TOOLS_PLUGIN_MANAGED\"; pwd"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("result = %+v, want success", outcome.Result)
		}
		content := outcome.Result.Content
		if !strings.Contains(content, "env-value|") || strings.Contains(content, "managed-secret") {
			t.Fatalf("result = %q, want the unlisted key visible and the managed key scrubbed", content)
		}
		if !strings.Contains(content, ws) {
			t.Fatalf("result = %q, want the workspace cwd", content)
		}
	})
}

func TestSleepTool(t *testing.T) {
	byID := openTools(t, t.TempDir())
	tc := callToolContext(t.TempDir(), runtime.ToolConstraints{})
	sleepTool := byID["sleep"]

	t.Run("normalization strips private fields, requires strict integers and clamps to 1..300", func(t *testing.T) {
		for _, args := range []string{
			`{"seconds":1.5}`,                  // fraction
			`{"seconds":"5"}`,                  // wrong type
			`{"seconds":true}`,                 // wrong type
			`{"seconds":null}`,                 // present null
			`null`,                             // whole-null arguments
			`{"seconds":99999999999999999999}`, // too large
		} {
			if _, err := normalize(t, sleepTool, tc, "sleep", args); err == nil {
				t.Errorf("sleep normalized %s, want rejection", args)
			}
		}
		raw, err := normalize(t, sleepTool, tc, "sleep", `{"seconds":2,"_lightcode_receipt":"x"}`)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if !strings.Contains(string(raw), `"seconds":2`) || strings.Contains(string(raw), "_lightcode_receipt") {
			t.Fatalf("normalized = %s, want the clamped value with private fields stripped", raw)
		}
		raw, err = normalize(t, sleepTool, tc, "sleep", `{"seconds":5000}`)
		if err != nil || !strings.Contains(string(raw), `"seconds":300`) {
			t.Fatalf("normalized = (%s, %v), want the 300 clamp", raw, err)
		}
		for _, args := range []string{`{}`, `{"seconds":0}`, `{"seconds":-5}`} {
			raw, err := normalize(t, sleepTool, tc, "sleep", args)
			if err != nil || !strings.Contains(string(raw), `"seconds":1`) {
				t.Fatalf("normalized = (%s, %v), want the retained default 1 for %s", raw, err, args)
			}
		}
	})

	t.Run("prepare declares the fixed sleep target", func(t *testing.T) {
		plan := prepare(t, sleepTool, tc, "sleep", `{"seconds":1}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want an executor plan", plan)
		}
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "sleep" || plan.Permissions[0].Target != "*" {
			t.Fatalf("pairs = %+v, want the fixed sleep target *", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "Slept for 1 seconds." {
			t.Fatalf("result = %+v, want the retained sleep result", outcome.Result)
		}
	})

	t.Run("cancellation settles as an interrupted result", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		plan := prepare(t, sleepTool, tc, "sleep", `{"seconds":30}`)
		type outcome struct {
			status model.ToolResultStatus
			body   string
		}
		outcomeCh := make(chan outcome, 1)
		go func() {
			result := plan.Execute(ctx)
			outcomeCh <- outcome{status: result.Result.Status, body: result.Result.Content}
		}()
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case got := <-outcomeCh:
			if got.status != model.ResultInterrupted || got.body == "" {
				t.Fatalf("outcome = (%v, %q), want a nonempty interrupted result", got.status, got.body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled sleep did not return")
		}
	})
}

// TestReadOnlyRunCommandExecutesRewriteSuppressingGitContentHelpers drives
// the shipped plugin's readonly run_command over a git repository whose
// configured content helpers (diff.external and the secret textconv driver)
// append to a marker file. The rewritten command suppresses both, so a
// successful execution must never create the marker — raw and rewritten
// commands are distinguishable by observable effect, not output shape.
func TestReadOnlyRunCommandExecutesRewriteSuppressingGitContentHelpers(t *testing.T) {
	repo := t.TempDir()
	// Isolate HOME so user-level git configuration (signing, hooks,
	// templates) cannot leak into or break the fixture.
	t.Setenv("HOME", t.TempDir())
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.invalid")
	runGit("config", "user.name", "Test User")

	marker := filepath.Join(repo, "helper-ran")
	textconv := filepath.Join(repo, "textconv.sh")
	external := filepath.Join(repo, "external.sh")
	writeScript := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeScript(textconv, fmt.Sprintf("#!/bin/sh\nprintf textconv >> %q\ncat \"$1\"\n", marker))
	writeScript(external, fmt.Sprintf("#!/bin/sh\nprintf external >> %q\nexit 0\n", marker))

	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.secret diff=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "file.secret"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "initial")
	runGit("config", "diff.secret.textconv", textconv)
	runGit("config", "diff.external", external)
	if err := os.WriteFile(filepath.Join(repo, "file.secret"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "file.secret")
	runGit("commit", "-m", "second")
	if err := os.WriteFile(filepath.Join(repo, "file.secret"), []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	byID := openTools(t, t.TempDir())
	for _, command := range []string{
		"git diff -- file.secret",
		"git log -p -1 -- file.secret",
		"git show HEAD -- file.secret",
		"git show HEAD:file.secret",
		"git blame -- file.secret",
	} {
		t.Run(command, func(t *testing.T) {
			_ = os.Remove(marker)
			plan := prepare(t, byID["run_command"], callToolContext(repo, runtime.ToolConstraints{Readonly: true}), "run_command", `{"command":"`+command+`"}`)
			if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "command.run" {
				t.Fatalf("pairs = %+v, want one command.run pair", plan.Permissions)
			}
			if target := plan.Permissions[0].Target; !strings.HasPrefix(target, "git --no-pager --no-optional-locks") {
				t.Fatalf("target = %q, want the rewritten read-only git command", target)
			}
			outcome := plan.Execute(context.Background())
			if outcome.Result.Status != model.ResultSuccess {
				t.Fatalf("result = %+v, want the rewritten command to succeed", outcome.Result)
			}
			if _, err := os.Stat(marker); err == nil {
				data, _ := os.ReadFile(marker)
				t.Fatalf("executed the raw command instead of the rewrite: helper marker = %q", data)
			} else if !os.IsNotExist(err) {
				t.Fatalf("marker read error = %v", err)
			}
		})
	}
}

func TestPrepareShapesAndCanonicals(t *testing.T) {
	byID := openTools(t, t.TempDir())

	t.Run("read_file binds canonical targets with file.read pairs", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["read_file"], callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"notes.txt"}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want one executor", plan)
		}
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "file.read" {
			t.Fatalf("permissions = %+v, want one file.read pair", plan.Permissions)
		}
		if plan.Permissions[0].Target != filepath.Join(ws, "notes.txt") {
			t.Fatalf("target = %q, want the canonical file path", plan.Permissions[0].Target)
		}
		if plan.CanonicalWorkspace != ws || plan.CanonicalWriteDir != "" {
			t.Fatalf("canonicals = %q/%q, want the workspace and an empty write dir", plan.CanonicalWorkspace, plan.CanonicalWriteDir)
		}
	})
	t.Run("read_file of a missing leaf declares the parent directory too", func(t *testing.T) {
		ws := t.TempDir()
		plan := prepare(t, byID["read_file"], callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"ghost.txt"}`)
		if len(plan.Permissions) != 2 {
			t.Fatalf("permissions = %+v, want the file and parent-directory file.read pairs", plan.Permissions)
		}
		for _, pair := range plan.Permissions {
			if pair.Permission != "file.read" || pair.Target == "" {
				t.Fatalf("pair = %+v, want a nonempty canonical file.read pair", pair)
			}
		}
	})
	t.Run("canonical workspace resolves symlinks and stays lexically fixed", func(t *testing.T) {
		real := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["read_file"], callToolContext(link, runtime.ToolConstraints{}), "read_file", `{"path":"a.txt"}`)
		if plan.CanonicalWorkspace != real {
			t.Fatalf("CanonicalWorkspace = %q, want the symlink-resolved %q", plan.CanonicalWorkspace, real)
		}
	})
	t.Run("write_file declares file.write and the canonical write-dir boundary", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, "wd"), 0o755); err != nil {
			t.Fatal(err)
		}
		constraints := runtime.ToolConstraints{WriteDir: "wd"}
		plan := prepare(t, byID["write_file"], callToolContext(ws, constraints), "write_file", `{"path":"wd/out.txt","content":"x"}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want one executor", plan)
		}
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "file.write" {
			t.Fatalf("permissions = %+v, want one file.write pair", plan.Permissions)
		}
		wantBoundary := filepath.Join(ws, "wd")
		if plan.CanonicalWriteDir != wantBoundary || plan.CanonicalWorkspace != ws {
			t.Fatalf("canonicals = %q/%q, want %q/%q", plan.CanonicalWorkspace, plan.CanonicalWriteDir, ws, wantBoundary)
		}
	})
	t.Run("apply_patch declares every source and destination in declaration order", func(t *testing.T) {
		ws := t.TempDir()
		for name, content := range map[string]string{"b.txt": "old\n", "c.txt": "gone\n", "d.txt": "d\n"} {
			if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		patch := "*** Begin Patch\n*** Add File: a.txt\n+new\n*** Update File: b.txt\n@@\n-old\n+new\n*** Delete File: c.txt\n*** Update File: d.txt\n*** Move to: e.txt\n*** End Patch"
		input, err := json.Marshal(map[string]string{"input": patch})
		if err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["apply_patch"], callToolContext(ws, runtime.ToolConstraints{}), "apply_patch", string(input))
		want := []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"}
		if len(plan.Permissions) != len(want) {
			t.Fatalf("permissions = %+v, want %d file.write pairs in declaration order", plan.Permissions, len(want))
		}
		for i, name := range want {
			pair := plan.Permissions[i]
			if pair.Permission != "file.write" || pair.Target != filepath.Join(ws, name) {
				t.Fatalf("pair %d = %+v, want file.write on %s", i, pair, name)
			}
		}
	})
	t.Run("undecodable arguments in Prepare map to the immediate validation error", func(t *testing.T) {
		ws := t.TempDir()
		plan := prepare(t, byID["read_file"], callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":`)
		if plan.Execute != nil || plan.Immediate == nil {
			t.Fatalf("plan = %+v, want one immediate outcome", plan)
		}
		outcome := *plan.Immediate
		if outcome.Result.Status != model.ResultError || outcome.Result.CallID != "call-1" || outcome.Result.Content == "" {
			t.Fatalf("immediate = %+v, want the bounded validation error for the original call", outcome)
		}
	})
	t.Run("prepare normalizes once: it never re-runs argument normalization", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("one\ntwo\nthree"), 0o644); err != nil {
			t.Fatal(err)
		}
		tool := byID["read_file"]
		// The committed normalized arguments are authoritative: a strict
		// "1e0" spelling canonicalizes once at normalization, and
		// preparation consumes the byte-identical committed value and reads
		// exactly line 1 with the canonical integer window.
		committed, err := normalize(t, tool, callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"f.txt","offset":1e0,"limit":1.0}`)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if string(committed) != `{"limit":1,"offset":1,"path":"f.txt"}` {
			t.Fatalf("committed normalized arguments = %s, want the canonical integers once", committed)
		}
		plan := prepare(t, tool, callToolContext(ws, runtime.ToolConstraints{}), "read_file", string(committed))
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.HasPrefix(outcome.Result.Content, "1\tone\n") || !strings.Contains(outcome.Result.Content, "(Showing lines 1-1 of 3. Use offset=2 to continue.)") {
			t.Fatalf("result = %+v, want the canonical 1/1 window through preparation", outcome.Result)
		}
		// Preparation performs no validation or defaulting of its own: an
		// un-normalized fraction handed directly to Prepare is not a second
		// normalization error — preparation binds targets and parses the
		// canonical lexeme only at its point of use.
		plan = prepare(t, tool, callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"f.txt","offset":2.5}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want one executor: Prepare runs no second normalization", plan)
		}
	})
	t.Run("failed canonical preparation maps to the immediate denial", func(t *testing.T) {
		ws := t.TempDir()
		loop := filepath.Join(ws, "loop")
		if err := os.Symlink("loop", loop); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["write_file"], callToolContext(ws, runtime.ToolConstraints{WriteDir: "loop"}), "write_file", `{"path":"out.txt","content":"x"}`)
		if plan.Execute != nil || plan.Immediate == nil {
			t.Fatalf("plan = %+v, want one immediate outcome", plan)
		}
		outcome := *plan.Immediate
		if outcome.Result.Status != model.ResultDenied || outcome.Result.Content != "Permission denied." {
			t.Fatalf("immediate = %+v, want the fixed denial", outcome)
		}
	})
	t.Run("a write target outside the write dir is denied at preparation", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, "wd"), 0o755); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["edit_file"], callToolContext(ws, runtime.ToolConstraints{WriteDir: "wd"}), "edit_file", `{"path":"outside.txt","old_string":"a","new_string":"b"}`)
		if plan.Execute != nil || plan.Immediate == nil || plan.Immediate.Result.Status != model.ResultDenied {
			t.Fatalf("plan = %+v, want the preparation denial", plan)
		}
	})
}

func TestExecuteFileEffects(t *testing.T) {
	dataDir := t.TempDir()
	byID := openTools(t, dataDir)

	t.Run("read_file returns bounded numbered content and no metadata", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["read_file"], callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"notes.txt","offset":2,"limit":2}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.CallID != "call-1" {
			t.Fatalf("result = %+v, want success for call-1", outcome.Result)
		}
		if !strings.Contains(outcome.Result.Content, "2\ttwo") || !strings.Contains(outcome.Result.Content, "3\tthree") {
			t.Fatalf("content = %q, want the numbered requested window", outcome.Result.Content)
		}
		if outcome.Metadata != nil {
			t.Fatalf("metadata = %s, want none for read_file", outcome.Metadata)
		}
	})
	t.Run("read_file of a missing file returns the suggestion outcome", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["read_file"], callToolContext(ws, runtime.ToolConstraints{}), "read_file", `{"path":"notes.tx"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.Contains(outcome.Result.Content, "not found") {
			t.Fatalf("result = %+v, want the bounded not-found suggestions outcome", outcome)
		}
	})
	t.Run("write_file creates parents, writes the file and captures the preimage group", func(t *testing.T) {
		dataDir := t.TempDir()
		byID := openTools(t, dataDir)
		ws := t.TempDir()
		plan := prepare(t, byID["write_file"], callToolContext(ws, runtime.ToolConstraints{}), "write_file", `{"path":"sub/dir/new.txt","content":"body"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.Contains(outcome.Result.Content, "Wrote") {
			t.Fatalf("result = %+v, want the write success", outcome.Result)
		}
		data, err := os.ReadFile(filepath.Join(ws, "sub", "dir", "new.txt"))
		if err != nil || string(data) != "body" {
			t.Fatalf("written file = (%q, %v), want the new content", data, err)
		}
		if outcome.Metadata != nil {
			t.Fatalf("metadata = %s, want none for write_file", outcome.Metadata)
		}
		// The preimage group lives at DataDir/code/<SessionID>/<EntryID>/snapshots/1/.
		turnDir := filepath.Join(groupDir(dataDir, testSessionID, testEntryID), "snapshots", "1")
		entries, err := os.ReadDir(turnDir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("group turn dir = (%d entries, %v), want one snapshot entry", len(entries), err)
		}
		meta, err := os.ReadFile(filepath.Join(turnDir, entries[0].Name(), "meta.json"))
		if err != nil {
			t.Fatalf("snapshot meta: %v", err)
		}
		var wire struct {
			OriginalPath string `json:"original_path"`
			Existed      bool   `json:"existed"`
		}
		if err := json.Unmarshal(meta, &wire); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		if wire.Existed || !strings.HasSuffix(wire.OriginalPath, "new.txt") {
			t.Fatalf("meta = %s, want the new-file preimage of the declared target", meta)
		}
	})
	t.Run("edit_file mutates the file and emits the edit_preview metadata shape", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["edit_file"], callToolContext(ws, runtime.ToolConstraints{}), "edit_file", `{"path":"notes.txt","old_string":"beta","new_string":"BETA"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.Contains(outcome.Result.Content, "lines 2") {
			t.Fatalf("result = %+v, want the edit summary", outcome.Result)
		}
		data, err := os.ReadFile(filepath.Join(ws, "notes.txt"))
		if err != nil || string(data) != "alpha\nBETA\ngamma\n" {
			t.Fatalf("edited file = (%q, %v), want the replacement", data, err)
		}
		if outcome.Metadata == nil {
			t.Fatal("edit_file emitted no metadata")
		}
		var metadata struct {
			EditPreview *struct {
				Hunks []struct {
					Rows []struct {
						Kind string `json:"kind"`
						Text string `json:"text"`
					} `json:"rows"`
				} `json:"hunks"`
			} `json:"edit_preview"`
		}
		if err := json.Unmarshal(outcome.Metadata, &metadata); err != nil {
			t.Fatalf("metadata %s is not the documented shape: %v", outcome.Metadata, err)
		}
		if metadata.EditPreview == nil || len(metadata.EditPreview.Hunks) != 1 || len(metadata.EditPreview.Hunks[0].Rows) != 2 {
			t.Fatalf("metadata = %s, want one hunk with the remove/add diff rows", outcome.Metadata)
		}
	})
	t.Run("apply_patch applies add update move delete and emits the per-file preview shape", func(t *testing.T) {
		ws := t.TempDir()
		for name, content := range map[string]string{"b.txt": "old\n", "c.txt": "gone\n", "d.txt": "d\n"} {
			if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		patch := "*** Begin Patch\n*** Add File: a.txt\n+new\n*** Update File: b.txt\n@@\n-old\n+new\n*** Delete File: c.txt\n*** Update File: d.txt\n*** Move to: e.txt\n*** End Patch"
		input, err := json.Marshal(map[string]string{"input": patch})
		if err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["apply_patch"], callToolContext(ws, runtime.ToolConstraints{}), "apply_patch", string(input))
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.Contains(outcome.Result.Content, "Success. Updated the following files:") {
			t.Fatalf("result = %+v, want the patch success summary", outcome.Result)
		}
		if _, err := os.Stat(filepath.Join(ws, "a.txt")); err != nil {
			t.Fatalf("added file missing: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(ws, "b.txt"))
		if err != nil || string(data) != "new\n" {
			t.Fatalf("updated file = (%q, %v), want the replacement", data, err)
		}
		if _, err := os.Stat(filepath.Join(ws, "c.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted file stat = %v, want gone", err)
		}
		if _, err := os.Stat(filepath.Join(ws, "d.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("move source stat = %v, want gone", err)
		}
		if data, err := os.ReadFile(filepath.Join(ws, "e.txt")); err != nil || string(data) != "d\n" {
			t.Fatalf("move destination = (%q, %v), want the renamed content", data, err)
		}
		if outcome.Metadata == nil {
			t.Fatal("apply_patch emitted no metadata")
		}
		var metadata struct {
			Files []struct {
				Path string `json:"path"`
				Op   string `json:"op"`
			} `json:"edit_preview_files"`
		}
		if err := json.Unmarshal(outcome.Metadata, &metadata); err != nil {
			t.Fatalf("metadata %s is not the documented shape: %v", outcome.Metadata, err)
		}
		var ops []string
		for _, f := range metadata.Files {
			ops = append(ops, f.Path+"="+f.Op)
		}
		// The engine emits the move destination (M) before its source (D).
		if strings.Join(ops, ",") != "a.txt=A,b.txt=M,c.txt=D,e.txt=M,d.txt=D" {
			t.Fatalf("previews = %v, want the full A/M/D list with content retained", ops)
		}
	})
	t.Run("successive calls of one operation share the disk group through fresh handles", func(t *testing.T) {
		dataDir := t.TempDir()
		byID := openTools(t, dataDir)
		ws := t.TempDir()
		first := prepare(t, byID["write_file"], callToolContext(ws, runtime.ToolConstraints{}), "write_file", `{"path":"one.txt","content":"1"}`)
		if outcome := first.Execute(context.Background()); outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("first write = %+v", outcome.Result)
		}
		second := prepare(t, byID["write_file"], callToolContext(ws, runtime.ToolConstraints{}), "write_file", `{"path":"two.txt","content":"2"}`)
		if outcome := second.Execute(context.Background()); outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("second write = %+v", outcome.Result)
		}
		turnDir := filepath.Join(groupDir(dataDir, testSessionID, testEntryID), "snapshots", "1")
		entries, err := os.ReadDir(turnDir)
		if err != nil || len(entries) != 2 {
			t.Fatalf("group turn dir = (%d entries, %v), want both calls' preimages in the one group", len(entries), err)
		}
	})
	t.Run("another operation's admitted input opens its own group", func(t *testing.T) {
		dataDir := t.TempDir()
		byID := openTools(t, dataDir)
		ws := t.TempDir()
		tc := callToolContext(ws, runtime.ToolConstraints{})
		tc.AdmittedEntry = harness.EntryRef{SessionID: testSessionID, EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		plan := prepare(t, byID["write_file"], tc, "write_file", `{"path":"other.txt","content":"x"}`)
		if outcome := plan.Execute(context.Background()); outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("write = %+v", outcome.Result)
		}
		turnDir := filepath.Join(groupDir(dataDir, testSessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "snapshots", "1")
		if entries, err := os.ReadDir(turnDir); err != nil || len(entries) != 1 {
			t.Fatalf("other group turn dir = (%d entries, %v), want exactly one entry", len(entries), err)
		}
	})
	t.Run("a readonly agent with a write dir writes inside the boundary", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, "wd"), 0o755); err != nil {
			t.Fatal(err)
		}
		tc := callToolContext(ws, runtime.ToolConstraints{Readonly: true, WriteDir: "wd"})
		plan := prepare(t, byID["write_file"], tc, "write_file", `{"path":"wd/allowed.txt","content":"x"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("write = %+v, want success inside the write dir", outcome.Result)
		}
	})
	t.Run("executed patch failures surface as error outcomes", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("different\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		patch := "*** Begin Patch\n*** Update File: b.txt\n@@\n-old\n+new\n*** End Patch"
		input, err := json.Marshal(map[string]string{"input": patch})
		if err != nil {
			t.Fatal(err)
		}
		plan := prepare(t, byID["apply_patch"], callToolContext(ws, runtime.ToolConstraints{}), "apply_patch", string(input))
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError || outcome.Result.Content == "" {
			t.Fatalf("result = %+v, want the failed-hunk error outcome", outcome.Result)
		}
	})
}

// TestFailedCallRetainsEarlierSnapshot proves the failed-call retention
// sibling at the plugin boundary: a failed mutating call in the SAME
// Operation's code group — reopened through a fresh handle per call —
// discards only its own pre-mutation claim and never the earlier
// successful call's retained preimage.
func TestFailedCallRetainsEarlierSnapshot(t *testing.T) {
	dataDir := t.TempDir()
	byID := openTools(t, dataDir)
	ws := t.TempDir()
	tc := callToolContext(ws, runtime.ToolConstraints{})
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First call: a successful write retains its preimage in the group.
	first := prepare(t, byID["write_file"], tc, "write_file", `{"path":"a.txt","content":"ALPHA\n"}`)
	if outcome := first.Execute(context.Background()); outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("first write = %+v, want success", outcome.Result)
	}

	// Second call: a failing edit in the same group — the shared body
	// captures b.txt's preimage, fails before any mutation, and discards
	// its own claim through the same fresh-handle group.
	second := prepare(t, byID["edit_file"], tc, "edit_file", `{"path":"b.txt","old_string":"missing","new_string":"x"}`)
	if outcome := second.Execute(context.Background()); outcome.Result.Status != model.ResultError {
		t.Fatalf("failing edit = %+v, want the error outcome", outcome.Result)
	}

	// The first call's retained preimage survives on disk; the failing
	// call discarded its own claim, so the turn holds exactly that one
	// entry.
	turnDir := filepath.Join(groupDir(dataDir, testSessionID, testEntryID), "snapshots", "1")
	entries, err := os.ReadDir(turnDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("group turn dir = (%d entries, %v), want only the first call's retained preimage", len(entries), err)
	}
	metaData, err := os.ReadFile(filepath.Join(turnDir, entries[0].Name(), "meta.json"))
	if err != nil {
		t.Fatalf("snapshot meta: %v", err)
	}
	var meta struct {
		OriginalPath string `json:"original_path"`
	}
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if !strings.HasSuffix(meta.OriginalPath, "a.txt") {
		t.Fatalf("retained entry = %q, want the first call's a.txt preimage", meta.OriginalPath)
	}
	preimage, err := os.ReadFile(filepath.Join(turnDir, entries[0].Name(), "original"))
	if err != nil || string(preimage) != "alpha\n" {
		t.Fatalf("retained preimage = (%q, %v), want the first call's content", preimage, err)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "a.txt")); err != nil || string(data) != "ALPHA\n" {
		t.Fatalf("written file = (%q, %v), want the first call's mutation intact", data, err)
	}
}

// TestExecutedPlanRejectsPostAuthorizationBindingChange proves the R5
// recovery sibling at the plugin boundary: a prepared canonical target whose
// binding changes between preparation and execution fails without
// substituting a changed path and without mutating through the repointed
// leaf.
func TestExecutedPlanRejectsPostAuthorizationBindingChange(t *testing.T) {
	dataDir := t.TempDir()
	byID := openTools(t, dataDir)
	ws := t.TempDir()
	real := filepath.Join(ws, "real.txt")
	if err := os.WriteFile(real, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "alias.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	plan := prepare(t, byID["write_file"], callToolContext(ws, runtime.ToolConstraints{}), "write_file", `{"path":"alias.txt","content":"after"}`)
	// Repoint the alias at a different leaf after authorization.
	other := filepath.Join(ws, "other.txt")
	if err := os.WriteFile(other, []byte("do not touch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	outcome := plan.Execute(context.Background())
	if outcome.Result.Status != model.ResultError || !strings.Contains(outcome.Result.Content, "resolve path") {
		t.Fatalf("result = %+v, want the binding-change error", outcome.Result)
	}
	data, err := os.ReadFile(other)
	if err != nil || string(data) != "do not touch\n" {
		t.Fatalf("repointed leaf = (%q, %v), want it unmutated", data, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "code")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed pre-mutation call left %s: %v", filepath.Join(dataDir, "code"), err)
	}
}

// TestExecutedReadFailsUnderWorkspaceRootChange proves the closure
// revalidates the bound canonical Workspace root: repointing the root
// symlink after preparation fails the execution without reading the
// substituted tree.
func TestExecutedReadFailsUnderWorkspaceRootChange(t *testing.T) {
	byID := openTools(t, t.TempDir())
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "notes.txt"), []byte("real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "ws-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	plan := prepare(t, byID["read_file"], callToolContext(link, runtime.ToolConstraints{}), "read_file", `{"path":"notes.txt"}`)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "notes.txt"), []byte("substituted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	outcome := plan.Execute(context.Background())
	if outcome.Result.Status != model.ResultError || strings.Contains(outcome.Result.Content, "substituted") {
		t.Fatalf("result = %+v, want the changed-root failure without substituted content", outcome.Result)
	}
}

// TestBackgroundStartFailuresRenderOneWrapperAndAbortReservation pins the one
// error wrapper for every background start failure and the reservation abort
// on the StartJob error path.
func TestBackgroundStartFailuresRenderOneWrapperAndAbortReservation(t *testing.T) {
	ws := t.TempDir()

	t.Run("handoff rejection renders through the wrapper and aborts the reservation", func(t *testing.T) {
		fake := &fakeJobs{}
		byID, _ := openToolsComposed(t, t.TempDir(), nil, fakeJobsPlugin(fake), runtime.Plugin{})
		tc := callToolContext(ws, runtime.ToolConstraints{})
		tc.Background = &scriptBackground{startErr: errors.New("background group is closed or stopping")}
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"true","background":true}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want an executor plan", plan)
		}
		if len(plan.Permissions) == 0 || plan.Permissions[0].Permission != "command.run" {
			t.Fatalf("pairs = %+v, want command.run pairs on the background call", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError || outcome.Result.Content != "run_command: background start: background group is closed or stopping" {
			t.Fatalf("result = %+v, want the one wrapper over the handoff rejection", outcome.Result)
		}
		if len(fake.aborted) != 1 || fake.aborted[0] != "b0b0b0b0" {
			t.Fatalf("aborted = %v, want the reservation aborted once", fake.aborted)
		}
		if len(fake.started) != 0 {
			t.Fatalf("started = %d, want the job never started", len(fake.started))
		}
	})

	t.Run("spawn failure renders through the wrapper and aborts the reservation", func(t *testing.T) {
		fake := &fakeJobs{startErr: errors.New("spawn boom")}
		byID, _ := openToolsComposed(t, t.TempDir(), nil, fakeJobsPlugin(fake), runtime.Plugin{})
		tc := callToolContext(ws, runtime.ToolConstraints{})
		tc.Background = &scriptBackground{}
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"true","background":true}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError || outcome.Result.Content != "run_command: background start: spawn boom" {
			t.Fatalf("result = %+v, want the one wrapper over the spawn failure", outcome.Result)
		}
		if len(fake.aborted) != 1 || fake.aborted[0] != "b0b0b0b0" {
			t.Fatalf("aborted = %v, want the reservation aborted once", fake.aborted)
		}
		if len(fake.started) != 1 {
			t.Fatalf("started = %d, want the failed Start attempt recorded", len(fake.started))
		}
	})

	t.Run("reserve failure renders through the wrapper without an abort", func(t *testing.T) {
		fake := &fakeJobs{reserveErr: errors.New("process: manager is closed")}
		byID, _ := openToolsComposed(t, t.TempDir(), nil, fakeJobsPlugin(fake), runtime.Plugin{})
		tc := callToolContext(ws, runtime.ToolConstraints{})
		tc.Background = &scriptBackground{}
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"true","background":true}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError || outcome.Result.Content != "run_command: background start: process: manager is closed" {
			t.Fatalf("result = %+v, want the one wrapper over the reserve failure", outcome.Result)
		}
		if len(fake.aborted) != 0 {
			t.Fatalf("aborted = %v, want no abort when the reservation never existed", fake.aborted)
		}
		if fake.reserves != 1 {
			t.Fatalf("reserves = %d, want exactly one attempt", fake.reserves)
		}
	})
}

// TestReadOnlyBackgroundStartRewritesAndHonorsTimeout pins the readonly
// background path: the rewritten command runs under the one permission
// target, the model timeout override is forwarded when >= 1 and absent or
// sub-one becomes 0, and the immediate result is the retained template.
func TestReadOnlyBackgroundStartRewritesAndHonorsTimeout(t *testing.T) {
	fake := &fakeJobs{live: []string{"b0b0b0b0", "11111111"}}
	byID, _ := openToolsComposed(t, t.TempDir(), nil, fakeJobsPlugin(fake), runtime.Plugin{})
	ws := t.TempDir()
	tc := callToolContext(ws, runtime.ToolConstraints{Readonly: true})
	tc.Background = &scriptBackground{}

	wantCommand, err := tool.ReadOnlyCommand("git status")
	if err != nil {
		t.Fatalf("ReadOnlyCommand: %v", err)
	}
	plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"git status","background":true,"timeout":5}`)
	if plan.Immediate != nil || plan.Execute == nil {
		t.Fatalf("plan = %+v, want an executor plan", plan)
	}
	if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "command.run" || plan.Permissions[0].Target != wantCommand {
		t.Fatalf("pairs = %+v, want the one command.run pair on the rewritten command", plan.Permissions)
	}
	outcome := plan.Execute(context.Background())
	wantContent := fmt.Sprintf("Command running in the background with ID: `%s`. You will be notified when it finishes. If your next steps do not depend on its output, continue with them; otherwise use sleep to wait and process to read the output.\nRunning in the background: `%s`.", "b0b0b0b0", strings.Join(fake.live, ", "))
	if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != wantContent {
		t.Fatalf("result = %+v, want the retained immediate template %q", outcome.Result, wantContent)
	}
	if len(fake.started) != 1 {
		t.Fatalf("started = %d, want exactly one background start", len(fake.started))
	}
	req := fake.started[0]
	if req.Command != wantCommand || req.TimeoutSec != 5 || req.Workspace != ws || req.SessionID != testSessionID {
		t.Fatalf("start request = %+v, want the rewritten command, timeout 5, the workspace, and the session", req)
	}

	// Absent or sub-one background timeout becomes 0.
	plan = prepare(t, byID["run_command"], tc, "run_command", `{"command":"pwd","background":true}`)
	outcome = plan.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("result = %+v, want success", outcome.Result)
	}
	if len(fake.started) != 2 || fake.started[1].TimeoutSec != 0 || fake.started[1].Command != "pwd" {
		t.Fatalf("started = %+v, want the second start with timeout 0 and the plain pwd command", fake.started)
	}
}

// TestBackgroundJobEnvironmentScrubsManagedKey proves the background job's
// environment carries the unlisted user key and never the managed key.
func TestBackgroundJobEnvironmentScrubsManagedKey(t *testing.T) {
	t.Setenv("LIGHTCODE_BG_UNLISTED", "visible-value")
	t.Setenv("LIGHTCODE_BG_MANAGED", "managed-secret")
	dataDir := t.TempDir()
	byID, values := openToolsComposed(t, dataDir, []string{"LIGHTCODE_BG_MANAGED"}, jobs.Plugin(), runtime.Plugin{})
	jobsInst, ok := values["jobs"].(jobs.Jobs)
	if !ok {
		t.Fatalf("values[\"jobs\"] supplies %T, not jobs.Jobs", values["jobs"])
	}
	ws := t.TempDir()
	tc := callToolContext(ws, runtime.ToolConstraints{})
	tc.Background = &scriptBackground{}
	plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"printf '%s|%s' \"$LIGHTCODE_BG_UNLISTED\" \"$LIGHTCODE_BG_MANAGED\"; sleep 30","background":true}`)
	outcome := plan.Execute(context.Background())
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("result = %+v, want the background start to succeed", outcome.Result)
	}
	jobID := assertBackgroundImmediate(t, outcome.Result.Content)
	deadline := time.Now().Add(15 * time.Second)
	var output string
	for time.Now().Before(deadline) {
		got, err := jobsInst.Read(testSessionID, jobID)
		if err == nil && strings.Contains(got, "visible-value|") {
			output = got
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if output == "" {
		t.Fatal("the background job never produced its output")
	}
	if strings.Contains(output, "managed-secret") {
		t.Fatalf("job output %q carries the managed key", output)
	}
}

// completionTextIn returns the first completion text carried by a model
// request's conversation messages.
func completionTextIn(req model.Request) string {
	for _, msg := range req.Messages {
		for _, part := range msg.Content {
			if part.Kind == model.PartText && strings.Contains(part.Text, "Background process ") && strings.Contains(part.Text, " finished: ") {
				return part.Text
			}
		}
	}
	return ""
}

// TestBackgroundStartRealProcessDeliversTruncatedSteeringCompletion drives a
// background start against a real short-lived process through the real
// Harness bridge: the immediate result is the retained template, the
// completion is delivered as steering input into the running operation, and
// the composed completion text is truncated at the configured bound.
func TestBackgroundStartRealProcessDeliversTruncatedSteeringCompletion(t *testing.T) {
	delivered := make(chan struct{}, 1)
	bg := &harnessBackground{delivered: delivered}
	invocation, err := runtime.ConfiguredInvocationForTest(map[string]string{"jobs": `{"max_output_bytes":1024}`})
	if err != nil {
		t.Fatalf("ConfiguredInvocationForTest: %v", err)
	}
	var mu sync.Mutex
	attempt := 0
	completion := ""
	modelFn := func(_ context.Context, req model.Request) (model.Stream, error) {
		if text := completionTextIn(req); text != "" {
			mu.Lock()
			completion = text
			mu.Unlock()
		}
		mu.Lock()
		attempt++
		n := attempt
		seen := completion != ""
		mu.Unlock()
		switch {
		case n == 1:
			return toolCallStream("call-bg", "run_command", `{"command":"printf '%1000d' 0","background":true}`), nil
		case seen:
			return stopStream("done"), nil
		case n == 2:
			select {
			case <-delivered:
			case <-time.After(15 * time.Second):
				return nil, errors.New("the background completion was never delivered")
			}
			return toolCallStream("call-followup", "run_command", `{"command":"true"}`), nil
		default:
			return nil, errors.New("the background completion never reached a model boundary")
		}
	}
	th := newToolsHarnessWith(t, modelFn, toolsHarnessOpts{
		jobsPlugin: jobs.Plugin(),
		advertise:  []string{"run_command"},
		background: bg,
		invocation: invocation,
	})
	session := th.createSession()
	th.submit(session, "op-1", "run the background command")
	rec := th.awaitSettled(session, "op-1")
	if rec.State.Status != harness.OperationSuccess {
		t.Fatalf("operation settled %q, want success: %+v", rec.State.Status, rec.State.Terminal)
	}

	results := th.readToolResults(session)
	immediate := results["call-bg"]
	if immediate.Status != model.ResultSuccess {
		t.Fatalf("immediate result = %+v, want success", immediate)
	}
	jobID := assertBackgroundImmediate(t, immediate.Content)

	mu.Lock()
	gotCompletion := completion
	mu.Unlock()
	if gotCompletion == "" {
		t.Fatal("the completion never reached a model boundary as steering input")
	}
	if len(gotCompletion) > 1024 || !strings.HasSuffix(gotCompletion, "\n[truncated]") || !utf8.ValidString(gotCompletion) {
		t.Fatalf("completion length = %d, want the text capped at 1024 bytes with the trailing marker and a complete UTF-8 prefix", len(gotCompletion))
	}
	if !strings.Contains(gotCompletion, "Background process "+jobID) {
		t.Fatalf("completion = %q, want it to name the started job %q", gotCompletion, jobID)
	}

	entries, err := th.store.ReadEntries(context.Background(), session, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Kind != harness.EntryInput || !strings.Contains(string(entry.Payload), "Background process ") {
			continue
		}
		var wire struct {
			Origin string `json:"origin"`
		}
		if err := json.Unmarshal(entry.Payload, &wire); err != nil {
			t.Fatalf("decode input payload: %v", err)
		}
		if wire.Origin != "runtime" {
			t.Fatalf("completion input origin = %q, want runtime", wire.Origin)
		}
		found = true
	}
	if !found {
		t.Fatal("no runtime-origin input entry carries the delivered completion")
	}
}

// reserveAndStart seeds one real background job on the composed capability.
func reserveAndStart(t *testing.T, jobsInst jobs.Jobs, workspace, command string) string {
	t.Helper()
	id, err := jobsInst.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := jobsInst.Start(context.Background(), jobs.StartRequest{
		JobID:     id,
		SessionID: testSessionID,
		Workspace: workspace,
		Command:   command,
		Env:       os.Environ(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return id
}

// TestProcessToolReturnsRetainedActionResults reproduces every legacy
// process action's result text against a real jobs capability, including the
// owner-scoped canonical targets and the capability's unknown-ID text for
// well-shaped unknown and foreign identities.
func TestProcessToolReturnsRetainedActionResults(t *testing.T) {
	byID, values := openToolsComposed(t, t.TempDir(), nil, jobs.Plugin(), runtime.Plugin{})
	jobsInst, ok := values["jobs"].(jobs.Jobs)
	if !ok {
		t.Fatalf("values[\"jobs\"] supplies %T, not jobs.Jobs", values["jobs"])
	}
	process := byID["process"]
	ws := t.TempDir()
	tc := callToolContext(ws, runtime.ToolConstraints{})

	t.Run("empty list retains the empty text on the fixed target", func(t *testing.T) {
		plan := prepare(t, process, tc, "process", `{"action":"list"}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want an executor plan", plan)
		}
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "process.own" || plan.Permissions[0].Target != "*" {
			t.Fatalf("pairs = %+v, want process.own on *", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "No background processes." {
			t.Fatalf("result = %+v, want the retained empty list text", outcome.Result)
		}
	})

	runningID := reserveAndStart(t, jobsInst, ws, "sleep 30")
	outID := reserveAndStart(t, jobsInst, ws, "printf hello; sleep 30")

	t.Run("list carries the retained line format", func(t *testing.T) {
		plan := prepare(t, process, tc, "process", `{"action":"list"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("result = %+v, want success", outcome.Result)
		}
		if !strings.Contains(outcome.Result.Content, runningID) || !strings.Contains(outcome.Result.Content, "sleep 30") || !strings.Contains(outcome.Result.Content, "(running for") {
			t.Fatalf("list = %q, want the retained running line for the seeded job", outcome.Result.Content)
		}
	})

	t.Run("read of a silent running job retains the placeholder on the owner-scoped target", func(t *testing.T) {
		plan := prepare(t, process, tc, "process", fmt.Sprintf(`{"action":"read","id":"%s"}`, runningID))
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "process.own" || plan.Permissions[0].Target != testSessionID+"/"+runningID {
			t.Fatalf("pairs = %+v, want process.own on the owner-scoped canonical target", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "(No output yet)" {
			t.Fatalf("result = %+v, want the retained empty-output placeholder", outcome.Result)
		}
	})

	t.Run("read carries the job's captured output", func(t *testing.T) {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			got, err := jobsInst.Read(testSessionID, outID)
			if err == nil && strings.Contains(got, "hello") {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		plan := prepare(t, process, tc, "process", fmt.Sprintf(`{"action":"read","id":"%s"}`, outID))
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || !strings.Contains(outcome.Result.Content, "hello") {
			t.Fatalf("result = %+v, want the captured job output", outcome.Result)
		}
	})

	t.Run("a foreign session id reaches the capability and returns the same text", func(t *testing.T) {
		tcForeign := callToolContext(ws, runtime.ToolConstraints{})
		tcForeign.AdmittedEntry.SessionID = "ffffffffffffffffffffffffffffffff"
		plan := prepare(t, process, tcForeign, "process", fmt.Sprintf(`{"action":"kill","id":"%s"}`, runningID))
		if len(plan.Permissions) != 1 || plan.Permissions[0].Target != "ffffffffffffffffffffffffffffffff/"+runningID {
			t.Fatalf("pairs = %+v, want the foreign owner-scoped target", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		want := fmt.Sprintf("process: no process with ID %q", runningID)
		if outcome.Result.Status != model.ResultError || outcome.Result.Content != want {
			t.Fatalf("result = %+v, want %q", outcome.Result, want)
		}
	})

	t.Run("kill retains the terminated text", func(t *testing.T) {
		plan := prepare(t, process, tc, "process", fmt.Sprintf(`{"action":"kill","id":"%s"}`, runningID))
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "process.own" || plan.Permissions[0].Target != testSessionID+"/"+runningID {
			t.Fatalf("pairs = %+v, want process.own on the owner-scoped canonical target", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		want := fmt.Sprintf("Process %s terminated.", runningID)
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != want {
			t.Fatalf("result = %+v, want %q", outcome.Result, want)
		}
		plan = prepare(t, process, tc, "process", fmt.Sprintf(`{"action":"kill","id":"%s"}`, outID))
		outcome = plan.Execute(context.Background())
		want = fmt.Sprintf("Process %s terminated.", outID)
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != want {
			t.Fatalf("result = %+v, want %q", outcome.Result, want)
		}
	})

	t.Run("a well-shaped unknown id reaches the capability and returns the retained text", func(t *testing.T) {
		plan := prepare(t, process, tc, "process", `{"action":"read","id":"deadbeef"}`)
		if len(plan.Permissions) != 1 || plan.Permissions[0].Target != testSessionID+"/deadbeef" {
			t.Fatalf("pairs = %+v, want the owner-scoped target for the well-shaped unknown id", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError || outcome.Result.Content != `process: no process with ID "deadbeef"` {
			t.Fatalf("result = %+v, want the retained unknown-ID text", outcome.Result)
		}
	})
}

// TestProcessToolValidationIsImmediateError pins the validation outcomes:
// missing ids, unknown actions, and ids outside the 8-lowercase-hex
// namespace settle as immediate error results with no permission target and
// no capability call — never permission-denied status.
func TestProcessToolValidationIsImmediateError(t *testing.T) {
	fake := &fakeJobs{}
	byID, _ := openToolsComposed(t, t.TempDir(), nil, fakeJobsPlugin(fake), runtime.Plugin{})
	process := byID["process"]
	ws := t.TempDir()
	cases := []struct {
		args string
		want string
	}{
		{`{"action":"read"}`, "process: id is required for read"},
		{`{"action":"kill"}`, "process: id is required for kill"},
		{`{"action":"drop","id":"deadbeef"}`, `process: unknown action "drop"`},
		{`{}`, `process: unknown action ""`},
		{`{"action":"read","id":"xyz"}`, `process: no process with ID "xyz"`},
		{`{"action":"kill","id":"DEADBEEF"}`, `process: no process with ID "DEADBEEF"`},
		{`{"action":"read","id":"deadbee"}`, `process: no process with ID "deadbee"`},
		{`{"action":"read","id":"deadbeef0"}`, `process: no process with ID "deadbeef0"`},
		{`{"action":"kill","id":"deadbeeg"}`, `process: no process with ID "deadbeeg"`},
	}
	for _, tc := range cases {
		plan := prepare(t, process, callToolContext(ws, runtime.ToolConstraints{}), "process", tc.args)
		if plan.Execute != nil || plan.Immediate == nil {
			t.Fatalf("%s: plan = %+v, want one immediate outcome", tc.args, plan)
		}
		if plan.Immediate.Result.Status != model.ResultError || plan.Immediate.Result.Content != tc.want {
			t.Fatalf("%s: immediate = %+v, want error %q", tc.args, plan.Immediate.Result, tc.want)
		}
		if len(plan.Permissions) != 0 {
			t.Fatalf("%s: pairs = %+v, want no permission target", tc.args, plan.Permissions)
		}
	}
	if fake.reserves != 0 || fake.reads != 0 || fake.kills != 0 || fake.lists != 0 {
		t.Fatalf("capability calls = reserves %d reads %d kills %d lists %d, want none", fake.reserves, fake.reads, fake.kills, fake.lists)
	}
}

// TestComposedPermissionDenialSettlesOnlyTheCall denies the background
// command and the process permission under one configured policy: both
// declarations settle as denials without reaching the jobs capability, and
// the later allowed write still runs — the denial settles only its call.
func TestComposedPermissionDenialSettlesOnlyTheCall(t *testing.T) {
	fake := &fakeJobs{}
	policy := harness.ResolvePermissionPolicy(json.RawMessage(`{"rules":[
		{"permission":"command.run","target":"*","access":"deny"},
		{"permission":"process.own","target":"*","access":"deny"}
	]}`), nil)
	attempt := 0
	var mu sync.Mutex
	modelFn := func(_ context.Context, _ model.Request) (model.Stream, error) {
		mu.Lock()
		attempt++
		n := attempt
		mu.Unlock()
		switch n {
		case 1:
			return toolCallStream("call-bg", "run_command", `{"command":"true","background":true}`), nil
		case 2:
			return toolCallStream("call-list", "process", `{"action":"list"}`), nil
		case 3:
			return toolCallStream("call-write", "write_file", `{"path":"out.txt","content":"written"}`), nil
		default:
			return stopStream("done"), nil
		}
	}
	th := newToolsHarnessWith(t, modelFn, toolsHarnessOpts{
		jobsPlugin:  fakeJobsPlugin(fake),
		advertise:   []string{"run_command", "process", "write_file"},
		permissions: policy,
	})
	session := th.createSession()
	th.submit(session, "op-1", "exercise the denied calls")
	rec := th.awaitSettled(session, "op-1")
	if rec.State.Status != harness.OperationSuccess {
		t.Fatalf("operation settled %q, want success: %+v", rec.State.Status, rec.State.Terminal)
	}
	results := th.readToolResults(session)
	if len(results) != 3 {
		t.Fatalf("committed tool results = %d, want exactly the three calls", len(results))
	}
	for _, id := range []string{"call-bg", "call-list"} {
		got := results[id]
		if got.Status != model.ResultDenied || got.Content != "Permission denied." {
			t.Fatalf("%s = %+v, want the fixed denial", id, got)
		}
	}
	write := results["call-write"]
	if write.Status != model.ResultSuccess || !strings.Contains(write.Content, "Wrote") {
		t.Fatalf("call-write = %+v, want the later allowed call to run", write)
	}
	if data, err := os.ReadFile(filepath.Join(th.workspace, "out.txt")); err != nil || string(data) != "written" {
		t.Fatalf("written file = (%q, %v), want the later write on disk", data, err)
	}
	if fake.reserves != 0 || len(fake.started) != 0 || fake.lists != 0 {
		t.Fatalf("capability calls = reserves %d started %d lists %d, want none behind the denials", fake.reserves, len(fake.started), fake.lists)
	}
}
