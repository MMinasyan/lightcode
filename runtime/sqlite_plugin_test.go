package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/plugins/sqlite"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/model"
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
	if err := os.WriteFile(configPath, []byte(composedConfigDocument), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return &composeEnv{t: t, home: home, dataDir: dataDir, configPath: configPath}
}

// isolateBundledCredentials empties every api_key_env declared by the bundled
// catalog so a captured build never starts a network attempt.
func isolateBundledCredentials(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "internal", "catalog", "builtin"))
	if err != nil {
		t.Fatalf("read bundled catalog: %v", err)
	}
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
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return files
}

func sortedNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
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
		if !ok {
			t.Fatalf("%s: file %q vanished", what, name)
		}
		if got != want {
			t.Fatalf("%s: file %q was modified", what, name)
		}
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
	if p.ID != "sqlite" {
		t.Fatalf("plugin ID = %q, want %q", p.ID, "sqlite")
	}
	if p.Scope != runtime.ScopeRuntime {
		t.Fatalf("plugin scope = %q, want the Runtime scope", p.Scope)
	}
	if len(p.Provides) != 1 {
		t.Fatalf("plugin declares %d exports, want exactly one harness.Storage export", len(p.Provides))
	}
	if len(p.Requires) != 0 {
		t.Fatalf("plugin requires %d capabilities, want none", len(p.Requires))
	}
	if p.ValidateConfig != nil {
		t.Fatal("plugin declares a settings validator, want none")
	}
	if p.Open == nil {
		t.Fatal("plugin declares no Open")
	}

	t.Run("open derives the exact database path and returns the declared store with its Close", func(t *testing.T) {
		dir := t.TempDir()
		inst, err := p.Open(context.Background(), runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dir}, runtime.Bindings{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if len(inst.Values) != 1 {
			t.Fatalf("instance values = %d exports, want exactly one declared export", len(inst.Values))
		}
		var store harness.Storage
		for _, value := range inst.Values {
			storageValue, ok := value.(harness.Storage)
			if !ok {
				t.Fatalf("instance export supplies %T, not harness.Storage", value)
			}
			store = storageValue
		}
		if store == nil {
			t.Fatal("instance export supplies a nil harness.Storage")
		}
		if inst.Close == nil {
			t.Fatal("instance declares no Close for the opened store")
		}
		if _, err := os.Stat(filepath.Join(dir, "lightcode.db")); err != nil {
			t.Fatalf("the store was not opened at the exact derived path: %v", err)
		}
		if err := inst.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		reopened, err := storage.OpenSQLite(filepath.Join(dir, "lightcode.db"))
		if err != nil {
			t.Fatalf("the closed database did not remain a valid canonical store: %v", err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatalf("Close of the reopened store: %v", err)
		}
	})

	t.Run("open checks the context before touching the filesystem", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		inst, err := p.Open(ctx, runtime.ScopeInfo{Kind: runtime.ScopeRuntime, DataDir: dir}, runtime.Bindings{})
		if inst.Values != nil || inst.Close != nil {
			t.Fatalf("canceled Open returned the instance %+v", inst)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Open error = %v, want the context error", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "lightcode.db")); !os.IsNotExist(err) {
			t.Fatalf("a canceled Open touched the filesystem: stat err = %v", err)
		}
	})
}

// TestComposedSQLiteRuntime assembles the real plugin through the test-build
// bridge over real temporary SQLite: R1-R3 and R13 composition behavior.
func TestComposedSQLiteRuntime(t *testing.T) {
	ctx := context.Background()

	t.Run("composes the selected backend at the exact data root and preserves unrelated files", func(t *testing.T) {
		e := newComposeEnv(t)
		if err := os.MkdirAll(filepath.Join(e.home, ".lightcode"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.home, ".lightcode", ".env"), []byte("ISOLATED=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.home, ".lightcode", "unrelated.txt"), []byte("keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(e.home, "project"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.home, "project", "notes.txt"), []byte("keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		homeBefore := treeFiles(t, e.home)

		r, err := e.openComposed(ctx)
		if err != nil {
			t.Fatalf("OpenForTest: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.dataDir, "lightcode.db")); err != nil {
			t.Fatalf("the composed backend did not open lightcode.db under the data root: %v", err)
		}
		for _, name := range sortedNames(t, e.dataDir) {
			switch name {
			case "runtime.lock", "lightcode.db", "lightcode.db-wal", "lightcode.db-shm":
			default:
				t.Fatalf("unexpected file %q in the data root while open; it is the sole data-root input", name)
			}
		}
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := sortedNames(t, e.dataDir); !slices.Equal(got, []string{"lightcode.db", "runtime.lock"}) {
			t.Fatalf("data root after Close = %v, want exactly the ownership lock and the composed database", got)
		}
		assertSameTree(t, homeBefore, treeFiles(t, e.home), "the isolated home")
	})

	t.Run("lock contention yields one owner and reacquisition follows release", func(t *testing.T) {
		e := newComposeEnv(t)
		r, err := e.openComposed(ctx)
		if err != nil {
			t.Fatalf("OpenForTest: %v", err)
		}
		contender := newComposeEnv(t)
		rivalData := e.dataDir
		contender.dataDir = rivalData
		if got, err := contender.openComposed(ctx); err == nil || !errors.Is(err, runtime.ErrOwned) {
			_ = r.Close(ctx)
			t.Fatalf("contender open = (%v, %v), want (nil, ErrOwned)", got, err)
		}
		if err := r.Close(ctx); err != nil {
			t.Fatalf("owner Close: %v", err)
		}
		again, err := e.openComposed(ctx)
		if err != nil {
			t.Fatalf("reopen after release: %v", err)
		}
		if err := again.Close(ctx); err != nil {
			t.Fatalf("Close after reacquire: %v", err)
		}
	})

	t.Run("close and reopen compose the same database", func(t *testing.T) {
		e := newComposeEnv(t)
		first, err := e.openComposed(ctx)
		if err != nil {
			t.Fatalf("first open: %v", err)
		}
		if err := first.Close(ctx); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		second, err := e.openComposed(ctx)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if err := second.Close(ctx); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if got := sortedNames(t, e.dataDir); !slices.Equal(got, []string{"lightcode.db", "runtime.lock"}) {
			t.Fatalf("data root after the second Close = %v, want exactly the lock and the database", got)
		}
	})

	t.Run("failed assembly releases ownership and creates no second owner", func(t *testing.T) {
		e := newComposeEnv(t)
		if err := os.WriteFile(filepath.Join(e.dataDir, "lightcode.db"), []byte("not a database at all"), 0o600); err != nil {
			t.Fatal(err)
		}
		r, err := e.openComposed(ctx)
		if r != nil {
			t.Fatalf("failed assembly returned a Runtime (%v)", err)
		}
		if err == nil || errors.Is(err, runtime.ErrOwned) {
			t.Fatalf("failed assembly error = %v, want the storage failure, not the ownership identity", err)
		}
		// The lock was released: the next attempt fails at storage again rather
		// than reporting ownership contention, and no partial owner remains.
		again, err := e.openComposed(ctx)
		if again != nil || err == nil || errors.Is(err, runtime.ErrOwned) {
			t.Fatalf("open over the failed assembly = (%v, %v), want (nil, the same storage failure)", again, err)
		}
	})

	t.Run("startup recovery repairs the seeded running state before execution", func(t *testing.T) {
		e := newComposeEnv(t)
		store, err := storage.OpenSQLite(filepath.Join(e.dataDir, "lightcode.db"))
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		// Registered before the seed's own cleanup, so the parked execution is
		// released before the store closes.
		t.Cleanup(func() { _ = store.Close() })
		sessionID, executions := seedRunningOperation(t, store, filepath.Join(e.home, "workspace"))

		key := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "seed-op"}
		reg, err := store.ReadRegister(ctx, key)
		if err != nil {
			t.Fatalf("ReadRegister before composition: %v", err)
		}
		if status := registerStatus(t, reg.Payload); status != harness.OperationRunning {
			t.Fatalf("seeded Operation status before composition = %q, want running", status)
		}

		r, err := e.openComposed(ctx)
		if err != nil {
			t.Fatalf("OpenForTest: %v", err)
		}

		// Repair completed during construction, before any execution: the
		// register is terminal and no execution callback ever ran again.
		reg, err = store.ReadRegister(ctx, key)
		if err != nil {
			t.Fatalf("ReadRegister after composition: %v", err)
		}
		if status := registerStatus(t, reg.Payload); status != harness.OperationInterruption {
			t.Fatalf("recovered Operation status = %q, want the terminal interruption settlement", status)
		}
		entries, err := store.ReadEntries(ctx, sessionID, 0)
		if err != nil {
			t.Fatalf("ReadEntries: %v", err)
		}
		var kinds []string
		for _, entry := range entries {
			kinds = append(kinds, string(entry.Kind))
		}
		sort.Strings(kinds)
		if !slices.Equal(kinds, []string{string(harness.EntryInput), string(harness.EntryOperationSettlement), string(harness.EntrySignal)}) {
			t.Fatalf("repaired Session entries = %v, want the admitted input, the interruption signal, and the settlement", kinds)
		}
		session := readSessionRegister(t, store, sessionID)
		if session != "" {
			t.Fatalf("repaired Session still names current Operation %q, want the cleared current Operation", session)
		}
		if n := executions.Load(); n != 1 {
			t.Fatalf("seeded model executions = %d, want only the original seeded one: recovery started no execution", n)
		}

		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
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
					SystemPrompt:          "seeded",
				},
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					return harness.Execution{
						NormalizeTool: func(call model.ToolCall) (json.RawMessage, error) {
							var obj map[string]json.RawMessage
							if err := json.Unmarshal(call.Arguments, &obj); err != nil || obj == nil {
								return nil, errors.New("arguments must be one non-null JSON object")
							}
							return json.Marshal(obj)
						},
						Model: func(context.Context, model.Request) (model.Stream, error) {
							executions.Add(1)
							select {
							case arrived <- struct{}{}:
							default:
							}
							<-release
							return nil, errors.New("seeded execution released after the test converged")
						},
						Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
							return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "seeded"}}}
						},
					}, nil
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("seed harness.New: %v", err)
	}
	rec, err := h.CreateSession(context.Background(), harness.CreateSessionRequest{Workspace: workspace, AgentType: "seeded"})
	if err != nil {
		t.Fatalf("seed CreateSession: %v", err)
	}
	if _, err := h.Submit(context.Background(), harness.SubmitRequest{
		SessionID: rec.Identity.SessionID, OperationID: "seed-op", Origin: harness.InputOriginUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "running"}}, Mode: harness.MessageModeRegular,
	}); err != nil {
		t.Fatalf("seed Submit: %v", err)
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
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatalf("decode operation register payload: %v", err)
	}
	return wire.State.Status
}

// readSessionRegister reads the repaired Session register's cleared current
// Operation.
func readSessionRegister(t *testing.T, store harness.Storage, sessionID string) string {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	if err != nil {
		t.Fatalf("ReadRegister(session): %v", err)
	}
	var wire struct {
		State struct {
			CurrentOperationID string `json:"current_operation_id"`
		} `json:"state"`
	}
	if err := json.Unmarshal(reg.Payload, &wire); err != nil {
		t.Fatalf("decode session register payload: %v", err)
	}
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

// TestComposedSQLiteRuntimeObservationShutdownAndOwnership combines the
// external composition surface over the real plugin: a saturated observer is
// removed while a healthy one continues, shutdown converges and closes every
// passive subscription, the data root stays the sole plugin-owned state, and
// ownership transfers to the next composition after release.
func TestComposedSQLiteRuntimeObservationShutdownAndOwnership(t *testing.T) {
	ctx := context.Background()
	e := newComposeEnv(t)
	r, err := e.openComposed(ctx)
	if err != nil {
		t.Fatalf("OpenForTest: %v", err)
	}
	sat, err := r.Subscribe(1)
	if err != nil {
		t.Fatalf("Subscribe(saturated): %v", err)
	}
	healthy, err := r.Subscribe(8)
	if err != nil {
		t.Fatalf("Subscribe(healthy): %v", err)
	}
	for _, want := range []string{"2", "3"} {
		if revision, err := r.Reload(ctx); err != nil || revision != want {
			t.Fatalf("Reload = (%q, %v), want %s", revision, err, want)
		}
	}
	first, ok := nextComposedEvent(t, sat)
	if !ok || first.Kind != runtime.EventConfiguration || first.ConfigurationRevision != "2" {
		t.Fatalf("saturated subscriber first event = %+v (ok=%v), want revision 2", first, ok)
	}
	if _, ok := nextComposedEvent(t, sat); ok {
		t.Fatal("saturated subscriber still open at the second publication, want it removed and closed")
	}
	sat.Close()
	var observed []runtime.Event
	for _, want := range []string{"2", "3"} {
		event, ok := nextComposedEvent(t, healthy)
		if !ok || event.Kind != runtime.EventConfiguration || event.ConfigurationRevision != want {
			t.Fatalf("healthy subscriber event = %+v (ok=%v), want configuration %s", event, ok, want)
		}
		observed = append(observed, event)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := sortedNames(t, e.dataDir); !slices.Equal(got, []string{"lightcode.db", "runtime.lock"}) {
		t.Fatalf("data root after Close = %v, want exactly the ownership lock and the composed database", got)
	}
	for {
		event, ok := nextComposedEvent(t, healthy)
		if !ok {
			break
		}
		observed = append(observed, event)
	}
	want := []runtime.Event{
		{Kind: runtime.EventConfiguration, ConfigurationRevision: "2"},
		{Kind: runtime.EventConfiguration, ConfigurationRevision: "3"},
		{Kind: runtime.EventScopeClosed, Scope: runtime.ScopeInfo{Kind: runtime.ScopeRuntime}},
	}
	if !slices.Equal(observed, want) {
		t.Fatalf("healthy subscriber events = %+v, want the observed revisions plus the Runtime closure in publication order %+v", observed, want)
	}
	again, err := e.openComposed(ctx)
	if err != nil {
		t.Fatalf("reopen after the shutdown released ownership: %v", err)
	}
	if err := again.Close(ctx); err != nil {
		t.Fatalf("Close after reacquire: %v", err)
	}
}
