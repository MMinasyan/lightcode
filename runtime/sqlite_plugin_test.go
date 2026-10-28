package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/internal/plugins/sqlite"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
	"github.com/MMinasyan/lightcode/runtime"
)

// External composition integration: the actual sqlite.Plugin is assembled
// through the test-build OpenForTest bridge, without a runtime-to-plugin
// import cycle. Every test isolates HOME and every bundled provider
// credential, so construction reaches no network, starts no user's Runtime,
// and touches no real .env or discovery cache.

const composedConfigDocument = `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}}}`

// composeEnv is one fully isolated composition environment: a throwaway home,
// a dedicated config directory, and the sole data root the plugin owns.
type composeEnv struct {
	t          *testing.T
	home       string
	dataDir    string
	configPath string
}

func newComposeEnv(t *testing.T) *composeEnv {
	t.Helper()
	home, dataDir, configDir := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	isolateBundledCredentials(t)
	configPath := filepath.Join(configDir, "config.json")
	testassert.NoError(t, os.WriteFile(configPath, []byte(composedConfigDocument), 0o600))
	return &composeEnv{t: t, home: home, dataDir: dataDir, configPath: configPath}
}

// isolateBundledCredentials empties every api_key_env declared by the bundled
// catalog so a captured build never starts a network attempt.
func isolateBundledCredentials(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "internal", "catalog", "builtin"))
	testassert.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "internal", "catalog", "builtin", entry.Name()))
		if err != nil {
			t.Fatalf("read bundled provider %s: %v", entry.Name(), err)
		}
		var provider struct {
			Transport struct {
				APIKeyEnv string `json:"api_key_env"`
			} `json:"transport"`
		}
		if err := json.Unmarshal(data, &provider); err != nil {
			t.Fatalf("decode bundled provider %s: %v", entry.Name(), err)
		}
		if provider.Transport.APIKeyEnv != "" {
			t.Setenv(provider.Transport.APIKeyEnv, "")
		}
	}
}

// openComposed assembles the real sqlite plugin through the test bridge.
func (e *composeEnv) openComposed(ctx context.Context) (*runtime.Runtime, error) {
	e.t.Helper()
	return runtime.OpenForTest(ctx, e.dataDir, e.configPath, []runtime.Plugin{sqlite.Plugin()})
}

// treeFiles maps every file under root, relative slash path to bytes.
func treeFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	testassert.Require(t, err == nil, "walk %s: %v", root, err)
	return files
}

func sortedNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	testassert.Require(t, err == nil, "read dir %s: %v", dir, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func assertSameTree(t *testing.T, before, after map[string]string, what string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s: file set changed: before %d files %v, after %d files %v", what, len(before), keys(before), len(after), keys(after))
	}
	for name, want := range before {
		got, ok := after[name]
		testassert.Require(t, ok, "%s: file %q vanished", what, name)
		testassert.Require(t, got == want, "%s: file %q was modified", what, name)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSQLitePluginDeclaration exercises the plugin's ordinary Plugin/Instance
// contract directly, the same surface an externally supplied capability
// implements.
func TestSQLitePluginDeclaration(t *testing.T) {
	p := sqlite.Plugin()
	testassert.Require(t, p.ID == "sqlite" && p.Scope == runtime.ScopeRuntime && len(p.Provides) == 1 && len(p.Requires) == 0, "got %v, want %v; got %v, want %v; got %v, want %v; got %v, want %v", p.ID, "sqlite", p.Scope, runtime.ScopeRuntime, len(p.Provides), 1, len(p.Requires), 0)
	testassert.Require(t, p.ValidateConfig == nil, "plugin declares a settings validator, want none")
	testassert.Require(t, p.Open != nil, "plugin declares no Open")

	t.Run("open derives the exact database path and returns the declared store with its Close", func(t *testing.T) {
		dir := t.TempDir()
		inst, err := p.Open(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dir}, runtime.Bindings{})
		testassert.NoError(t, err)
		testassert.Equal(t, len(inst.Values), 1)
		var store harness.Storage
		for _, value := range inst.Values {
			storageValue, ok := value.(harness.Storage)
			testassert.Require(t, ok, "instance export supplies %T, not harness.Storage", value)
			store = storageValue
		}
		testassert.Require(t, store != nil, "instance export supplies a nil harness.Storage")
		testassert.Require(t, inst.Close != nil, "instance declares no Close for the opened store")
		{
			_, err := os.Stat(filepath.Join(dir, "lightcode.db"))
			testassert.NoError(t, err)
		}
		testassert.NoError(t, inst.Close())
		reopened, err := storage.OpenSQLite(filepath.Join(dir, "lightcode.db"))
		testassert.NoError(t, err)
		testassert.NoError(t, reopened.Close())
	})

	t.Run("open checks the context before touching the filesystem", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		inst, err := p.Open(ctx, runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dir}, runtime.Bindings{})
		testassert.Require(t, inst.Values == nil && inst.Close == nil, "canceled Open returned the instance %+v", inst)
		testassert.Require(t, errors.Is(err, context.Canceled), "canceled Open error = %v, want the context error", err)
		{
			_, err := os.Stat(filepath.Join(dir, "lightcode.db"))
			testassert.Require(t, os.IsNotExist(err), "a canceled Open touched the filesystem: stat err = %v", err)
		}
	})
}

// TestComposedSQLiteRuntime assembles the real plugin through the test-build
// bridge over real temporary SQLite: R1-R3 and R13 composition behavior.
func TestComposedSQLiteRuntime(t *testing.T) {
	ctx := context.Background()

	t.Run("composes the selected backend at the exact data root and preserves unrelated files", func(t *testing.T) {
		e := newComposeEnv(t)
		testassert.NoError(t, os.MkdirAll(filepath.Join(e.home, ".lightcode"), 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(e.home, ".lightcode", ".env"), []byte("ISOLATED=1\n"), 0o600))
		testassert.NoError(t, os.WriteFile(filepath.Join(e.home, ".lightcode", "unrelated.txt"), []byte("keep\n"), 0o600))
		testassert.NoError(t, os.MkdirAll(filepath.Join(e.home, "project"), 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(e.home, "project", "notes.txt"), []byte("keep\n"), 0o600))
		homeBefore := treeFiles(t, e.home)

		r, err := e.openComposed(ctx)
		testassert.NoError(t, err)
		{
			_, err := os.Stat(filepath.Join(e.dataDir, "lightcode.db"))
			testassert.NoError(t, err)
		}
		for _, name := range sortedNames(t, e.dataDir) {
			switch name {
			case "runtime.lock", "lightcode.db", "lightcode.db-wal", "lightcode.db-shm":
			default:
				t.Fatalf("unexpected file %q in the data root while open; it is the sole data-root input", name)
			}
		}
		testassert.NoError(t, r.Close(ctx))
		{
			got := sortedNames(t, e.dataDir)
			testassert.Require(t, slices.Equal(got, []string{"lightcode.db", "runtime.lock"}), "data root after Close = %v, want exactly the ownership lock and the composed database", got)
		}
		assertSameTree(t, homeBefore, treeFiles(t, e.home), "the isolated home")
	})

	t.Run("lock contention yields one owner and reacquisition follows release", func(t *testing.T) {
		e := newComposeEnv(t)
		r, err := e.openComposed(ctx)
		testassert.NoError(t, err)
		contender := newComposeEnv(t)
		rivalData := e.dataDir
		contender.dataDir = rivalData
		if got, err := contender.openComposed(ctx); err == nil || !errors.Is(err, runtime.ErrOwned) {
			_ = r.Close(ctx)
			t.Fatalf("contender open = (%v, %v), want (nil, ErrOwned)", got, err)
		}
		testassert.NoError(t, r.Close(ctx))
		again, err := e.openComposed(ctx)
		testassert.NoError(t, err)
		testassert.NoError(t, again.Close(ctx))
	})

	t.Run("close and reopen compose the same database", func(t *testing.T) {
		e := newComposeEnv(t)
		first, err := e.openComposed(ctx)
		testassert.NoError(t, err)
		testassert.NoError(t, first.Close(ctx))
		second, err := e.openComposed(ctx)
		testassert.NoError(t, err)
		testassert.NoError(t, second.Close(ctx))
		{
			got := sortedNames(t, e.dataDir)
			testassert.Require(t, slices.Equal(got, []string{"lightcode.db", "runtime.lock"}), "data root after the second Close = %v, want exactly the lock and the database", got)
		}
	})

	t.Run("failed assembly releases ownership and creates no second owner", func(t *testing.T) {
		e := newComposeEnv(t)
		testassert.NoError(t, os.WriteFile(filepath.Join(e.dataDir, "lightcode.db"), []byte("not a database at all"), 0o600))
		r, err := e.openComposed(ctx)
		testassert.Require(t, r == nil, "failed assembly returned a Runtime (%v)", err)
		testassert.Require(t, err != nil && !errors.Is(err, runtime.ErrOwned), "failed assembly error = %v, want the storage failure, not the ownership identity", err)
		// The lock was released: the next attempt fails at storage again rather
		// than reporting ownership contention, and no partial owner remains.
		again, err := e.openComposed(ctx)
		testassert.Require(t, again == nil && err != nil && !errors.Is(err, runtime.ErrOwned), "open over the failed assembly = (%v, %v), want (nil, the same storage failure)", again, err)
	})

	t.Run("startup recovery repairs the seeded running state before execution", func(t *testing.T) {
		e := newComposeEnv(t)
		store, err := storage.OpenSQLite(filepath.Join(e.dataDir, "lightcode.db"))
		testassert.NoError(t, err)
		// Registered before the seed's own cleanup, so the parked execution is
		// released before the store closes.
		t.Cleanup(func() { _ = store.Close() })
		sessionID, executions := seedRunningOperation(t, store, filepath.Join(e.home, "workspace"))

		key := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "seed-op"}
		reg, err := store.ReadRegister(ctx, key)
		testassert.NoError(t, err)
		testassert.Equal(t, registerStatus(t, reg.Payload), harness.OperationRunning)

		r, err := e.openComposed(ctx)
		testassert.NoError(t, err)

		// Repair completed during construction, before any execution: the
		// register is terminal and no execution callback ever ran again.
		reg, err = store.ReadRegister(ctx, key)
		testassert.NoError(t, err)
		testassert.Equal(t, registerStatus(t, reg.Payload), harness.OperationInterruption)
		entries, err := store.ReadEntries(ctx, sessionID, 0)
		testassert.NoError(t, err)
		var kinds []string
		for _, entry := range entries {
			kinds = append(kinds, string(entry.Kind))
		}
		sort.Strings(kinds)
		testassert.Require(t, slices.Equal(kinds, []string{string(harness.EntryInput), string(harness.EntryOperationSettlement), string(harness.EntrySignal)}), "repaired Session entries = %v, want the admitted input, the interruption signal, and the settlement", kinds)
		session := readSessionRegister(t, store, sessionID)
		testassert.Equal(t, session, "")
		testassert.Equal(t, executions.Load(), 1)

		testassert.NoError(t, r.Close(ctx))
	})
}

// seedRunningOperation commits one running Operation through a standalone
// Harness whose model effect never completes: abandoning that owner is the
// durable state restart recovery consumes. The returned counter observes the
// model effect, and the parked execution is released during cleanup.
func seedRunningOperation(t *testing.T, store harness.Storage, workspace string) (string, *atomic.Int32) {
	t.Helper()
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	var executions atomic.Int32
	t.Cleanup(func() { close(release) })
	h, err := harness.New(context.Background(), harness.Dependencies{
		Storage: store,
		Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: harness.ExecutionCapture{
					ConfigurationRevision: "1",
					Model:                 model.ModelRef{Provider: "prov", Model: "m"},
					ContextWindow:         4096,
					OutputReserve:         2048,
					SystemPrompt:          "seeded",
					Compact: harness.CompactCapture{
						Model:        model.ModelRef{Provider: "prov", Model: "m"},
						SystemPrompt: "summarize",
					},
					Strategy: harness.StrategyCapture{ID: harness.IncludedStrategyID, RequiresContextWindow: true},
				},
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					modelFn := func(context.Context, model.Request) (model.Stream, error) {
						executions.Add(1)
						select {
						case arrived <- struct{}{}:
						default:
						}
						<-release
						return nil, errors.New("seeded execution released after the test converged")
					}
					return harness.Execution{OpenInvocation: func(context.Context, harness.InvocationRequest) (harness.InvocationExecution, error) {
						return harness.InvocationExecution{
							NormalizeTool: func(call model.ToolCall) (json.RawMessage, error) {
								var obj map[string]json.RawMessage
								if err := json.Unmarshal(call.Arguments, &obj); err != nil || obj == nil {
									return nil, errors.New("arguments must be one non-null JSON object")
								}
								return json.Marshal(obj)
							},
							Model: modelFn,
							Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
								return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "seeded"}}}
							},
						}, nil
					}}, nil
				},
			}, nil
		},
	})
	testassert.NoError(t, err)
	rec, err := h.CreateSession(context.Background(), harness.CreateSessionRequest{Workspace: workspace, AgentType: "seeded"})
	testassert.NoError(t, err)
	{
		_, err := h.Submit(context.Background(), harness.SubmitRequest{
			SessionID: rec.Identity.SessionID, OperationID: "seed-op", Origin: harness.InputOriginUser,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "running"}}, Mode: harness.MessageModeRegular,
		})
		testassert.NoError(t, err)
	}
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the seeded execution never reached its model effect")
	}
	return rec.Identity.SessionID, &executions
}

// registerStatus reads the closed operation register payload's state status.
func registerStatus(t *testing.T, payload json.RawMessage) harness.OperationState {
	t.Helper()
	var wire struct {
		State struct {
			Status harness.OperationState `json:"status"`
		} `json:"state"`
	}
	testassert.NoError(t, json.Unmarshal(payload, &wire))
	return wire.State.Status
}

// readSessionRegister reads the repaired Session register's cleared current
// Operation.
func readSessionRegister(t *testing.T, store harness.Storage, sessionID string) string {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	testassert.NoError(t, err)
	var wire struct {
		State struct {
			CurrentOperationID string `json:"current_operation_id"`
		} `json:"state"`
	}
	testassert.NoError(t, json.Unmarshal(reg.Payload, &wire))
	return wire.State.CurrentOperationID
}

// nextComposedEvent reads one event with the test budget applied.
func nextComposedEvent(t *testing.T, sub *runtime.Subscription) (runtime.Event, bool) {
	t.Helper()
	select {
	case event, ok := <-sub.Events():
		return event, ok
	case <-time.After(10 * time.Second):
		t.Fatal("no event arrived within the test budget")
		return runtime.Event{}, false
	}
}

// composedEventKind reads one composed event's discriminator.
func composedEventKind(t *testing.T, event runtime.Event) string {
	t.Helper()
	kind, err := event.Discriminator()
	testassert.NoError(t, err)
	return kind
}

// composedEventGeneration reads one composed configuration event's
// generation.
func composedEventGeneration(t *testing.T, event runtime.Event) string {
	t.Helper()
	body, err := event.AsConfigurationChangedEvent()
	testassert.NoError(t, err)
	return body.ConfigurationRevision.Generation
}

// composedEventJSON renders one composed event for failure diagnostics.
func composedEventJSON(event runtime.Event) string {
	data, err := event.MarshalJSON()
	if err != nil {
		return "<unset>"
	}
	return string(data)
}

// composedConfigurationEvent builds one configuration event through the
// generated constructor for the composed order oracles.
func composedConfigurationEvent(generation uint64) runtime.Event {
	var scope protocol.Scope
	_ = scope.FromRuntimeScope(protocol.RuntimeScope{Kind: protocol.RuntimeScopeKindRuntime}) // plain members; the marshal cannot fail
	var event protocol.Event
	_ = event.FromConfigurationChangedEvent(protocol.ConfigurationChangedEvent{ // plain members; the marshal cannot fail
		Kind:                  protocol.ConfigurationChanged,
		Scope:                 scope,
		ConfigurationRevision: protocol.ConfigurationRevision{Generation: strconv.FormatUint(generation, 10)},
	})
	return event
}

// TestComposedSQLiteRuntimeObservationShutdownAndOwnership combines the
// external composition surface over the real plugin: a saturated observer is
// removed while a healthy one continues, shutdown converges and closes every
// passive subscription, the data root stays the sole plugin-owned state, and
// ownership transfers to the next composition after release.
func TestComposedSQLiteRuntimeObservationShutdownAndOwnership(t *testing.T) {
	ctx := context.Background()
	e := newComposeEnv(t)
	r, err := e.openComposed(ctx)
	testassert.NoError(t, err)
	sat, err := r.Subscribe(1)
	testassert.NoError(t, err)
	healthy, err := r.Subscribe(8)
	testassert.NoError(t, err)
	for _, want := range []string{"2", "3"} {
		{
			revision, err := r.Reload(ctx)
			testassert.Require(t, err == nil && revision == want, "Reload = (%q, %v), want %s", revision, err, want)
		}
	}
	first, ok := nextComposedEvent(t, sat)
	if !ok || composedEventGeneration(t, first) != "2" {
		t.Fatalf("saturated subscriber first event = %s (ok=%v), want revision 2", composedEventJSON(first), ok)
	}
	{
		_, ok := nextComposedEvent(t, sat)
		testassert.Require(t, !ok, "saturated subscriber still open at the second publication, want it removed and closed")
	}
	sat.Close()
	var observed []runtime.Event
	for _, want := range []string{"2", "3"} {
		event, ok := nextComposedEvent(t, healthy)
		if !ok || composedEventKind(t, event) != "configuration_changed" || composedEventGeneration(t, event) != want {
			t.Fatalf("healthy subscriber event = %s (ok=%v), want configuration %s", composedEventJSON(event), ok, want)
		}
		observed = append(observed, event)
	}
	testassert.NoError(t, r.Close(ctx))
	{
		got := sortedNames(t, e.dataDir)
		testassert.Require(t, slices.Equal(got, []string{"lightcode.db", "runtime.lock"}), "data root after Close = %v, want exactly the ownership lock and the composed database", got)
	}
	for {
		event, ok := nextComposedEvent(t, healthy)
		if !ok {
			break
		}
		observed = append(observed, event)
	}
	want := []runtime.Event{
		composedConfigurationEvent(2),
		composedConfigurationEvent(3),
		composedRuntimeScopeClosed(),
	}
	testassert.Require(t, slices.EqualFunc(observed, want, runtime.EqualEventForTest), "healthy subscriber events = %+v, want the observed revisions plus the Runtime closure in publication order %+v", observed, want)
	again, err := e.openComposed(ctx)
	testassert.NoError(t, err)
	testassert.NoError(t, again.Close(ctx))
}

// TestBuiltinRegistrationComposes proves the shipped registration set — the
// SQLite storage plugin, the native tools, the bundled adaptation, the LSP
// tools, and the background jobs plugin — composes through the real
// construction path: the complete declared set opens over the real plugin's
// database, reload publishes, and shutdown converges leaving exactly the
// plugin-owned files.
func TestBuiltinRegistrationComposes(t *testing.T) {
	ctx := context.Background()
	e := newComposeEnv(t)
	r, err := runtime.OpenForTest(ctx, e.dataDir, e.configPath, builtin.Plugins())
	testassert.NoError(t, err)
	{
		revision, err := r.Reload(ctx)
		testassert.Require(t, err == nil && revision == "2", "Reload = (%q, %v), want 2", revision, err)
	}
	testassert.NoError(t, r.Close(ctx))
	{
		got := sortedNames(t, e.dataDir)
		testassert.Require(t, slices.Equal(got, []string{"lightcode.db", "runtime.lock"}), "data root after shutdown = %v, want exactly the real SQLite plugin's files", got)
	}
}

// composedRuntimeScopeClosed builds the Runtime closure scope event for the
// composed order oracles.
func composedRuntimeScopeClosed() runtime.Event {
	var scope protocol.Scope
	_ = scope.FromRuntimeScope(protocol.RuntimeScope{Kind: protocol.RuntimeScopeKindRuntime}) // plain members; the marshal cannot fail
	var event protocol.Event
	_ = event.FromScopeEvent(protocol.ScopeEvent{ // plain members; the marshal cannot fail
		Kind:  protocol.ScopeClosed,
		Scope: scope,
	})
	return event
}
