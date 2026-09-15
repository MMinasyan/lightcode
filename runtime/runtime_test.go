package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/model"
)

// Owner-lifecycle fixtures: one controlled prepare/opener pair and traceable
// plugins drive the real owner over memory and temporary SQLite. Each test
// isolates HOME and every bundled provider credential, so construction reaches
// no network and mutates no real user state.

const ownerConfigDocument = `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}}}`

const ownerGatedConfigDocument = `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}},"plugins":{"gate":{}}}`

const ownerAgentsDocument = `{"solo":{"model":"prov/m","system_prompt":"simple"}}`

type ownerEnv struct {
	t              *testing.T
	home           string
	dataDir        string
	configPath     string
	events         *traceLog
	prep           *controlledPrep
	scopeDataDir   atomic.Value
	scopeWorkspace atomic.Value
}

func newOwnerEnv(t *testing.T) *ownerEnv {
	t.Helper()
	return newOwnerEnvIn(t, t.TempDir(), t.TempDir())
}

func newOwnerEnvIn(t *testing.T, home, dataDir string) *ownerEnv {
	t.Helper()
	t.Setenv("HOME", home)
	isolateBundledCredentials(t)
	e := &ownerEnv{
		t: t, home: home, dataDir: dataDir,
		configPath: filepath.Join(dataDir, "config.json"),
		events:     &traceLog{},
		prep:       newControlledPrep(),
	}
	e.scopeDataDir.Store("")
	e.scopeWorkspace.Store("")
	writeServiceFile(t, e.configPath, ownerConfigDocument)
	writeServiceFile(t, agents.PathForConfig(e.configPath), ownerAgentsDocument)
	return e
}

// isolateBundledCredentials empties every api_key_env declared by the bundled
// catalog so discovery readiness fails for all providers: a captured build in
// these tests never starts a network attempt.
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

func (e *ownerEnv) options(plugins ...Plugin) options {
	return options{DataDir: e.dataDir, ConfigPath: e.configPath, Plugins: plugins, prepare: e.prep.prepare}
}

func (e *ownerEnv) open(ctx context.Context, plugins ...Plugin) (*Runtime, error) {
	e.t.Helper()
	return open(ctx, e.options(plugins...))
}

// corePlugin is one Runtime-scoped plugin whose single export is declared
// exactly as harness.Storage: the private Core binding. Its Close records the
// disposal event only; the test retains the store itself.
func (e *ownerEnv) corePlugin(id, exportID string, value any, closeFn func() error) Plugin {
	return Plugin{
		ID:       id,
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[harness.Storage](exportID)},
		Open: func(_ context.Context, info ScopeInfo, _ Bindings) (Instance, error) {
			e.scopeDataDir.Store(info.DataDir)
			e.events.add("open:" + id)
			return Instance{Values: map[string]any{exportID: value}, Close: closeFn}, nil
		},
	}
}

func (e *ownerEnv) storagePlugin(value any) Plugin {
	return e.corePlugin("core", "session_store", value, func() error {
		e.events.add("close:core")
		return nil
	})
}

// ordinaryPlugin provides one value capability and records its construction
// and disposal order.
func (e *ownerEnv) ordinaryPlugin(id string, scope ScopeKind, providesID string, requires ...CapabilitySpec) Plugin {
	return Plugin{
		ID:       id,
		Scope:    scope,
		Provides: []CapabilitySpec{Spec[any](providesID)},
		Requires: requires,
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			e.events.add("open:" + id)
			return Instance{
				Values: map[string]any{providesID: id},
				Close:  func() error { e.events.add("close:" + id); return nil },
			}, nil
		},
	}
}

// countedPlugin is one ordinary plugin whose Open returns each construction
// number on a buffered signal.
func (e *ownerEnv) countedPlugin(id string, scope ScopeKind, providesID string, opens chan<- int64) Plugin {
	var count atomic.Int64
	return Plugin{
		ID:       id,
		Scope:    scope,
		Provides: []CapabilitySpec{Spec[any](providesID)},
		Open: func(_ context.Context, info ScopeInfo, _ Bindings) (Instance, error) {
			e.scopeWorkspace.Store(info.Workspace)
			e.events.add("open:" + id)
			opens <- count.Add(1)
			return Instance{Values: map[string]any{providesID: id}}, nil
		},
	}
}

// sqliteDerivationPlugin opens lightcode.db under the ScopeInfo DataDir it
// receives, proving the same normalized owner root reaches the factory.
func (e *ownerEnv) sqliteDerivationPlugin() Plugin {
	return Plugin{
		ID:       "db",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[harness.Storage]("session_store")},
		Open: func(_ context.Context, info ScopeInfo, _ Bindings) (Instance, error) {
			e.scopeDataDir.Store(info.DataDir)
			e.events.add("open:db")
			store, err := storage.OpenSQLite(filepath.Join(info.DataDir, "lightcode.db"))
			if err != nil {
				return Instance{}, err
			}
			return Instance{
				Values: map[string]any{"session_store": store},
				Close:  func() error { e.events.add("close:db"); return store.Close() },
			}, nil
		},
	}
}

// controlledPrep is the options.prepare fixture: every admission captures the
// selected Agent unchanged and runs one immediately successful turn unless a
// test parks the model effect.
type controlledPrep struct {
	mu           sync.Mutex
	calls        int
	openCalls    int
	modelGate    chan struct{}
	modelArrived chan struct{}
	cleanups     chan struct{}
	cleanupSeen  int
}

func newControlledPrep() *controlledPrep {
	return &controlledPrep{modelArrived: make(chan struct{}, 16), cleanups: make(chan struct{}, 16)}
}

func (p *controlledPrep) prepare(_ context.Context, req harness.PreparationRequest, sel selection) (harness.ExecutionCapture, openExecution, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	capture := harness.ExecutionCapture{
		ConfigurationRevision: sel.invocation.Revision(),
		Model:                 sel.agent.Model,
		SystemPrompt:          "prompt-" + req.Session.AgentType,
		Tools:                 captureTools(sel.agent.Tools),
	}
	return capture, p.open, nil
}

func (p *controlledPrep) open(_ context.Context, adm harness.OperationAdmission, sel selection) (harness.Execution, error) {
	p.mu.Lock()
	p.openCalls++
	gate := p.modelGate
	p.mu.Unlock()
	return harness.Execution{
		NormalizeTool: runtimeNormalize,
		Model: func(ctx context.Context, _ model.Request) (model.Stream, error) {
			select {
			case p.modelArrived <- struct{}{}:
			default:
			}
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return &prepStream{}, nil
		},
		Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
			return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no concrete tools yet"}}}
		},
		Close: func() error {
			select {
			case p.cleanups <- struct{}{}:
			default:
			}
			return nil
		},
	}, nil
}

func (p *controlledPrep) awaitCleanups(n int) {
	for p.cleanupSeen < n {
		select {
		case <-p.cleanups:
			p.cleanupSeen++
		case <-time.After(10 * time.Second):
			panic(fmt.Sprintf("execution cleanup %d never ran", p.cleanupSeen+1))
		}
	}
}

func (p *controlledPrep) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.openCalls
}

// gatedValidator is one plugin settings validator a test can arm to park a
// configuration build in flight and then answer with a configured error.
type gatedValidator struct {
	mu     sync.Mutex
	gate   chan struct{}
	arrive chan struct{}
	fail   error
}

func (v *gatedValidator) ValidateConfig(json.RawMessage) error {
	v.mu.Lock()
	gate, arrive, fail := v.gate, v.arrive, v.fail
	v.mu.Unlock()
	if arrive != nil {
		select {
		case arrive <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	return fail
}

func (v *gatedValidator) arm() <-chan struct{} {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.gate = make(chan struct{})
	v.arrive = make(chan struct{}, 1)
	return v.arrive
}

func (v *gatedValidator) release() {
	v.mu.Lock()
	gate := v.gate
	v.gate = nil
	v.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// countingStore records Core-binding use: only the canonical storage value may
// ever reach the Harness or recovery.
type countingStore struct {
	harness.Storage
	mu    sync.Mutex
	lists int
}

func (s *countingStore) ListSessionIDs(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Storage.ListSessionIDs(ctx)
}

func (s *countingStore) listCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

// failingListStore aborts one storage-service call so a recovery-stage failure
// is constructible over a real store.
type failingListStore struct {
	harness.Storage
	err error
}

func (s *failingListStore) ListSessionIDs(context.Context) ([]string, error) { return nil, s.err }

// nilStorage is a typed-nil canonical export candidate: the interface value is
// non-nil while its dynamic pointer is nil.
type nilStorage struct{ harness.Storage }

func wantFailedOpen(t *testing.T, e *ownerEnv, plugins []Plugin, wantIs error) {
	t.Helper()
	r, err := e.open(context.Background(), plugins...)
	if r != nil {
		t.Fatalf("failed construction returned a non-nil Runtime with error %v", err)
	}
	if !errors.Is(err, wantIs) {
		t.Fatalf("open error = %v, want %v", err, wantIs)
	}
}

// assertLockReleased proves the failed attempt released the ownership lock.
func (e *ownerEnv) assertLockReleased(t *testing.T) {
	t.Helper()
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("reopen after the failed attempt: %v (a resource or the ownership lock was left behind)", err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// assertNoOwnershipSideEffects proves a pure declaration rejection touched
// neither the filesystem nor any factory.
func (e *ownerEnv) assertNoOwnershipSideEffects(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(e.dataDir, "runtime.lock")); !os.IsNotExist(err) {
		t.Fatalf("declaration validation reached the filesystem or ownership: stat err = %v", err)
	}
	if events := e.events.all(); len(events) != 0 {
		t.Fatalf("declaration validation invoked factories: %v", events)
	}
}

// eventNames strips the identity suffix from each recorded event.
func eventNames(events []string) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		parts := strings.Split(event, ":")
		if len(parts) > 2 {
			parts = parts[:2]
		}
		out = append(out, strings.Join(parts, ":"))
	}
	return out
}

// orderedSubset reports whether want appears inside events in order.
func orderedSubset(events []string, want ...string) bool {
	i := 0
	for _, event := range events {
		if i < len(want) && event == want[i] {
			i++
		}
	}
	return i == len(want)
}

func eventsNamed(events []string, name string) int {
	n := 0
	for _, event := range eventNames(events) {
		if event == name {
			n++
		}
	}
	return n
}

func submitThroughRuntime(t *testing.T, r *Runtime, sessionID, operationID, text string) {
	t.Helper()
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		res, err := h.Submit(ctx, harness.SubmitRequest{
			SessionID:   sessionID,
			OperationID: operationID,
			Origin:      harness.InputOriginUser,
			Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
			Mode:        harness.MessageModeRegular,
		})
		if err != nil {
			return err
		}
		if res.Disposition != harness.DispositionAdmitted {
			return fmt.Errorf("submit %q = %+v, want admitted", operationID, res.Disposition)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Submit(%q): %v", operationID, err)
	}
}

func readOperation(t *testing.T, r *Runtime, sessionID, operationID string) harness.OperationRecord {
	t.Helper()
	var rec harness.OperationRecord
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		rec, err = h.ReadOperation(ctx, sessionID, operationID)
		return err
	})
	if err != nil {
		t.Fatalf("ReadOperation(%q): %v", operationID, err)
	}
	return rec
}

// seedRunningOperation commits one running Operation through a standalone
// Harness whose model effect never completes: abandoning that owner is the
// durable state restart recovery consumes. The effect is released during
// cleanup, after every test assertion has run.
func seedRunningOperation(t *testing.T, store harness.Storage, workspace string) (string, string) {
	t.Helper()
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h, err := harness.New(context.Background(), harness.Dependencies{
		Storage: store,
		Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{
				Capture: harness.ExecutionCapture{ConfigurationRevision: "1", Model: prepModelRef, SystemPrompt: "seeded"},
				Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
					return harness.Execution{
						NormalizeTool: runtimeNormalize,
						Model: func(context.Context, model.Request) (model.Stream, error) {
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
	return rec.Identity.SessionID, "seed-op"
}

// --- construction validation and the one data root ---

func TestRuntimeOpenValidatesOptionsAndNormalizesPaths(t *testing.T) {
	t.Run("invalid options report before any effect", func(t *testing.T) {
		e := newOwnerEnv(t)
		cases := []struct {
			name string
			opts options
		}{
			{"empty data directory", options{ConfigPath: e.configPath, Plugins: []Plugin{e.storagePlugin(nil)}, prepare: e.prep.prepare}},
			{"empty config path", options{DataDir: e.dataDir, Plugins: []Plugin{e.storagePlugin(nil)}, prepare: e.prep.prepare}},
		}
		for _, tc := range cases {
			r, err := open(context.Background(), tc.opts)
			if r != nil || err == nil {
				t.Fatalf("open(%s) = (%v, %v), want a nil Runtime and an error", tc.name, r, err)
			}
			e.assertNoOwnershipSideEffects(t)
		}
	})

	t.Run("canceled construction context reports before initialization", func(t *testing.T) {
		e := newOwnerEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r, err := e.open(ctx, e.storagePlugin(storage.NewMemory()))
		if r != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("open = (%v, %v), want (nil, context.Canceled)", r, err)
		}
		e.assertNoOwnershipSideEffects(t)
	})

	t.Run("relative paths normalize through one filepath.Abs pass", func(t *testing.T) {
		e := newOwnerEnv(t)
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		relData, err := filepath.Rel(cwd, e.dataDir)
		if err != nil {
			t.Fatalf("Rel: %v", err)
		}
		r, err := open(context.Background(), options{
			DataDir:    relData,
			ConfigPath: filepath.Join(relData, "config.json"),
			Plugins:    []Plugin{e.sqliteDerivationPlugin()},
			prepare:    e.prep.prepare,
		})
		if err != nil {
			t.Fatalf("open(relative): %v", err)
		}
		if events := e.events.all(); !slices.Equal(eventNames(events), []string{"open:db"}) {
			t.Fatalf("factory events = %v, want exactly the Core factory", events)
		}
		if got := e.scopeDataDir.Load().(string); got != e.dataDir {
			t.Fatalf("ScopeInfo DataDir = %q, want the same normalized owner root %q", got, e.dataDir)
		}
		if _, err := os.Stat(filepath.Join(e.dataDir, "runtime.lock")); err != nil {
			t.Fatalf("lock under the normalized root: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.dataDir, "lightcode.db")); err != nil {
			t.Fatalf("the storage backend derives its database from the same root: %v", err)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	t.Run("one data root with an independent config path", func(t *testing.T) {
		e := newOwnerEnv(t)
		configDir := t.TempDir()
		configPath := filepath.Join(configDir, "config.json")
		writeServiceFile(t, configPath, ownerConfigDocument)
		writeServiceFile(t, agents.PathForConfig(configPath), ownerAgentsDocument)
		e.configPath = configPath
		r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, err := os.Stat(filepath.Join(configDir, "agents.json")); err != nil {
			t.Fatalf("agents.json does not stay beside the config path: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.home, ".lightcode", ".env")); err != nil {
			t.Fatalf("the managed dotenv source is not home-based: %v", err)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- ownership lock ---

func TestRuntimeOwnershipLockIsExclusive(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A contender with its own configuration override still targets the same
	// owner root: identity is the normalized data directory.
	rival := newOwnerEnvIn(t, e.home, e.dataDir)
	if got, err := rival.open(context.Background(), rival.corePlugin("late", "session_store", storage.NewMemory(), nil)); err == nil || !errors.Is(err, ErrOwned) {
		_ = r.Close(context.Background())
		t.Fatalf("contender open = (%v, %v), want ErrOwned", got, err)
	}
	if len(rival.events.all()) != 0 {
		_ = r.Close(context.Background())
		t.Fatalf("contender factory events = %v, want no factory invoked while losing ownership", rival.events.all())
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("reopen after release: %v", err)
	}
	if err := again.Close(context.Background()); err != nil {
		t.Fatalf("Close after reacquire: %v", err)
	}
}

const lockChildEnv = "LIGHTCODE_RUNTIME_LOCK_CHILD"

// TestRuntimeOwnershipLockSpansProcessExit proves process ownership with a
// real second process: contention while the child holds the lock, and
// reacquisition after the child is killed or exits through managed shutdown.
// When LIGHTCODE_RUNTIME_LOCK_CHILD names a data directory this very test
// binary runs as the child: it acquires ownership, announces it, and shuts
// its owner down only when its standard input closes.
func TestRuntimeOwnershipLockSpansProcessExit(t *testing.T) {
	if dir := os.Getenv(lockChildEnv); dir != "" {
		e := newOwnerEnvIn(t, os.Getenv("HOME"), dir)
		r, err := e.open(context.Background(), e.sqliteDerivationPlugin())
		if err != nil {
			t.Fatalf("child open: %v", err)
		}
		fmt.Println("acquired")
		_, _ = io.Copy(io.Discard, os.Stdin)
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("child Close: %v", err)
		}
		fmt.Println("closed")
		return
	}

	type contender struct {
		cmd    *exec.Cmd
		env    *ownerEnv
		stdin  io.WriteCloser
		stdout *bufio.Scanner
	}
	start := func(t *testing.T) *contender {
		t.Helper()
		home := t.TempDir()
		dataDir := t.TempDir()
		parent := newOwnerEnvIn(t, home, dataDir)
		cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.timeout=120s")
		cmd.Env = append(os.Environ(), lockChildEnv+"="+dataDir)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatalf("StdinPipe: %v", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("StdoutPipe: %v", err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		c := &contender{cmd: cmd, env: parent, stdin: stdin, stdout: bufio.NewScanner(stdout)}
		for c.stdout.Scan() {
			if c.stdout.Text() == "acquired" {
				return c
			}
		}
		_ = cmd.Wait()
		t.Fatal("the child never announced ownership")
		return nil
	}

	t.Run("a killed holder releases ownership for a contender", func(t *testing.T) {
		c := start(t)
		if got, err := c.env.open(context.Background(), c.env.corePlugin("late", "session_store", storage.NewMemory(), nil)); err == nil || !errors.Is(err, ErrOwned) {
			_ = c.stdin.Close()
			_ = c.cmd.Wait()
			t.Fatalf("contender open while the child holds the lock = (%v, %v), want ErrOwned", got, err)
		}
		if events := c.env.events.all(); len(events) != 0 {
			_ = c.stdin.Close()
			_ = c.cmd.Wait()
			t.Fatalf("contender factory events = %v, want none while another process owns the state", events)
		}
		if err := c.cmd.Process.Kill(); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		if err := c.cmd.Wait(); err == nil {
			t.Fatal("the killed child reported success")
		}
		r, err := c.env.open(context.Background(), c.env.sqliteDerivationPlugin())
		if err != nil {
			t.Fatalf("open against the durable state left by the killed owner: %v", err)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	t.Run("managed shutdown in another process releases ownership", func(t *testing.T) {
		c := start(t)
		if err := c.stdin.Close(); err != nil {
			t.Fatalf("close stdin: %v", err)
		}
		closed := false
		for c.stdout.Scan() {
			if c.stdout.Text() == "closed" {
				closed = true
				break
			}
		}
		if err := c.cmd.Wait(); err != nil || !closed {
			t.Fatalf("child managed shutdown: scanned-closed=%v wait=%v", closed, err)
		}
		r, err := c.env.open(context.Background(), c.env.sqliteDerivationPlugin())
		if err != nil {
			t.Fatalf("open after the owner exited: %v", err)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- startup failure stages ---

func TestRuntimeStartupFailuresUnwindAcquiredResources(t *testing.T) {
	t.Run("invalid declarations reject before ownership", func(t *testing.T) {
		e := newOwnerEnv(t)
		duplicateA := e.ordinaryPlugin("same", ScopeRuntime, "a.cap")
		duplicateB := e.ordinaryPlugin("same", ScopeRuntime, "b.cap")
		wantFailedOpen(t, e, []Plugin{e.storagePlugin(storage.NewMemory()), duplicateA, duplicateB}, ErrComposition)
		e.assertNoOwnershipSideEffects(t)
	})

	t.Run("configuration failure releases the lock with no factory", func(t *testing.T) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, `{not json`)
		wantFailedOpen(t, e, []Plugin{e.storagePlugin(storage.NewMemory())}, ErrConfiguration)
		if events := e.events.all(); len(events) != 0 {
			t.Fatalf("factory events = %v, want the configuration rejection to precede any factory", events)
		}
		writeServiceFile(t, e.configPath, ownerConfigDocument)
		e.assertLockReleased(t)
	})

	t.Run("factory failure disposes the constructed prefix", func(t *testing.T) {
		e := newOwnerEnv(t)
		failing := e.ordinaryPlugin("failing", ScopeRuntime, "failing.cap")
		failing.Open = func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			e.events.add("open:failing")
			return Instance{}, errFactory
		}
		wantFailedOpen(t, e, []Plugin{e.storagePlugin(storage.NewMemory()), e.ordinaryPlugin("first", ScopeRuntime, "first.cap"), failing}, errFactory)
		events := eventNames(e.events.all())
		if !orderedSubset(events, "close:first") || slices.Contains(events, "close:failing") {
			t.Fatalf("factory events = %v, want only the constructed prefix disposed", events)
		}
		e.assertLockReleased(t)
	})

	t.Run("untyped-nil Core storage rolls back through its closer", func(t *testing.T) {
		e := newOwnerEnv(t)
		wantFailedOpen(t, e, []Plugin{e.storagePlugin(nil)}, ErrComposition)
		if events := eventNames(e.events.all()); !slices.Contains(events, "close:core") {
			t.Fatalf("factory events = %v, want the invalid instance disposed", events)
		}
		e.assertLockReleased(t)
	})

	t.Run("typed-nil Core storage rolls back through its closer", func(t *testing.T) {
		e := newOwnerEnv(t)
		wantFailedOpen(t, e, []Plugin{e.storagePlugin((*nilStorage)(nil))}, ErrComposition)
		if events := eventNames(e.events.all()); !slices.Contains(events, "close:core") {
			t.Fatalf("factory events = %v, want the invalid instance disposed", events)
		}
		e.assertLockReleased(t)
	})

	t.Run("recovery failure disposes the Runtime scope", func(t *testing.T) {
		e := newOwnerEnv(t)
		errRecover := errors.New("test recovery failure")
		store := &failingListStore{Storage: storage.NewMemory(), err: errRecover}
		wantFailedOpen(t, e, []Plugin{e.storagePlugin(store)}, errRecover)
		if events := eventNames(e.events.all()); !slices.Contains(events, "close:core") {
			t.Fatalf("factory events = %v, want the constructed Runtime scope disposed", events)
		}
		e.assertLockReleased(t)
	})

	t.Run("cancellation during construction unwinds without publication", func(t *testing.T) {
		e := newOwnerEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		arrive := make(chan struct{}, 1)
		first := e.ordinaryPlugin("signalled", ScopeRuntime, "signalled.cap")
		first.Open = func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			e.events.add("open:signalled")
			select {
			case arrive <- struct{}{}:
			default:
			}
			return Instance{Values: map[string]any{"signalled.cap": "signalled"}, Close: func() error { e.events.add("close:signalled"); return nil }}, nil
		}
		second := e.ordinaryPlugin("waiting", ScopeRuntime, "waiting.cap")
		second.Open = func(ctx context.Context, _ ScopeInfo, _ Bindings) (Instance, error) {
			<-ctx.Done()
			e.events.add("open:waiting")
			return Instance{}, ctx.Err()
		}
		go func() {
			<-arrive
			cancel()
		}()
		r, err := e.open(ctx, e.storagePlugin(storage.NewMemory()), first, second)
		if r != nil {
			cancel()
			t.Fatalf("canceled construction returned a non-nil Runtime (%v)", err)
		}
		if !errors.Is(err, context.Canceled) {
			cancel()
			t.Fatalf("canceled construction error = %v, want the context error", err)
		}
		if events := eventNames(e.events.all()); !orderedSubset(events, "close:signalled") {
			t.Fatalf("factory events = %v, want the constructed prefix disposed on cancellation", events)
		}
		e.assertLockReleased(t)
	})

	t.Run("cancellation during the initial publication reports the constructor context error", func(t *testing.T) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerGatedConfigDocument)
		validator := &gatedValidator{}
		gate := e.ordinaryPlugin("gate", ScopeRuntime, "gate.cap")
		gate.ValidateConfig = validator.ValidateConfig
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		arrive := validator.arm()
		type outcome struct {
			r   *Runtime
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			r, err := e.open(ctx, e.storagePlugin(storage.NewMemory()), gate)
			done <- outcome{r, err}
		}()
		<-arrive
		cancel()
		validator.release()
		var got outcome
		select {
		case got = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("canceled construction never returned")
		}
		if got.r != nil {
			t.Fatalf("construction completed after cancellation (Runtime %v, error %v)", got.r, got.err)
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("open error = %v, want the constructor context's own error, not the owner-lifecycle identity", got.err)
		}
		if events := e.events.all(); len(events) != 0 {
			t.Fatalf("factory events = %v, want the publication rejection to precede any factory", events)
		}
		writeServiceFile(t, e.configPath, ownerConfigDocument)
		e.assertLockReleased(t)
	})

	t.Run("an ErrClosed source identity survives without cancellation", func(t *testing.T) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerGatedConfigDocument)
		gate := e.ordinaryPlugin("gate", ScopeRuntime, "gate.cap")
		gate.ValidateConfig = func(json.RawMessage) error { return ErrClosed }
		wantFailedOpen(t, e, []Plugin{e.storagePlugin(storage.NewMemory()), gate}, ErrClosed)
		if events := e.events.all(); len(events) != 0 {
			t.Fatalf("factory events = %v, want the validator rejection preserved before any factory", events)
		}
		writeServiceFile(t, e.configPath, ownerConfigDocument)
		e.assertLockReleased(t)
	})

	t.Run("a wrapped ErrClosed source survives cancellation at the publication boundary", func(t *testing.T) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, ownerGatedConfigDocument)
		errValidator := fmt.Errorf("test validator unavailable: %w", ErrClosed)
		validator := &gatedValidator{fail: errValidator}
		gate := e.ordinaryPlugin("gate", ScopeRuntime, "gate.cap")
		gate.ValidateConfig = validator.ValidateConfig
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		arrive := validator.arm()
		type outcome struct {
			r   *Runtime
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			r, err := e.open(ctx, e.storagePlugin(storage.NewMemory()), gate)
			done <- outcome{r, err}
		}()
		<-arrive
		cancel()
		validator.release()
		var got outcome
		select {
		case got = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("canceled construction never returned")
		}
		if got.r != nil {
			t.Fatalf("construction completed after the validator rejection (Runtime %v, error %v)", got.r, got.err)
		}
		if !errors.Is(got.err, errValidator) || !errors.Is(got.err, ErrConfiguration) {
			t.Fatalf("open error = %v, want the wrapped validator and ErrConfiguration identities preserved, not replaced by the context error", got.err)
		}
		if errors.Is(got.err, context.Canceled) {
			t.Fatalf("open error = %v, want the source identity rather than the constructor context error", got.err)
		}
		writeServiceFile(t, e.configPath, ownerConfigDocument)
		e.assertLockReleased(t)
	})
}

// --- managed shutdown ---

func TestRuntimeCloseSharesOneCleanupAndReleasesTheLockLast(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		errStorageClose := errors.New("test storage close failure")
		errXClose := errors.New("test scope close failure")
		blockClose := make(chan struct{})
		entered := make(chan struct{}, 1)
		storagePlugin := e.corePlugin("core", "session_store", store, func() error {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-blockClose
			e.events.add("close:core")
			return errStorageClose
		})
		// The consumer is registered before its provider: construction must still
		// follow the composition's stable topological order.
		y := e.ordinaryPlugin("y", ScopeRuntime, "y.cap", Spec[any]("x.cap"))
		y.Open = func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			e.events.add("open:y")
			return Instance{Values: map[string]any{"y.cap": "y"}, Close: func() error { e.events.add("close:y"); return nil }}, nil
		}
		x := e.ordinaryPlugin("x", ScopeRuntime, "x.cap")
		x.Open = func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			e.events.add("open:x")
			return Instance{Values: map[string]any{"x.cap": "x"}, Close: func() error { e.events.add("close:x"); return errXClose }}, nil
		}
		w := e.ordinaryPlugin("w", ScopeWorkspace, "w.cap")

		r, err := e.open(context.Background(), storagePlugin, y, x, w)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if events := eventNames(e.events.all()); !orderedSubset(events, "open:core", "open:x", "open:y") {
			t.Fatalf("factory events = %v, want dependencies constructed before consumers in stable topological order", events)
		}
		if _, err := r.workspaces.get(context.Background(), ScopeInfo{Kind: ScopeWorkspace, DataDir: e.dataDir, Workspace: "/ws"}); err != nil {
			t.Fatalf("workspace get: %v", err)
		}

		firstDone := make(chan error, 1)
		go func() { firstDone <- r.Close(context.Background()) }()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the storage closer never started")
		}
		joinedDone := make(chan error, 1)
		go func() { joinedDone <- r.Close(context.Background()) }()

		canceledCtx, cancelWait := context.WithCancel(context.Background())
		cancelWait()
		if err := r.Close(canceledCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Close waiter = %v, want its own context error while the shared cleanup continues", err)
		}
		if got, err := e.open(context.Background(), e.corePlugin("contender", "session_store", storage.NewMemory(), nil)); err == nil || !errors.Is(err, ErrOwned) {
			close(blockClose)
			t.Fatalf("contender open during blocked disposal = (%v, %v), want the lock held until every required close has run", got, err)
		}

		close(blockClose)
		var closeErr error
		select {
		case closeErr = <-firstDone:
		case <-time.After(10 * time.Second):
			t.Fatal("Close never converged after disposal was released")
		}
		if !errors.Is(closeErr, errStorageClose) || !errors.Is(closeErr, errXClose) {
			t.Fatalf("Close error = %v, want every attempted close joined", closeErr)
		}
		select {
		case joined := <-joinedDone:
			if joined != closeErr {
				t.Fatalf("concurrent Close = %v, want the same joined shared result %v", joined, closeErr)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the second Close caller never joined the shared cleanup")
		}
		if again := r.Close(context.Background()); again != closeErr {
			t.Fatalf("repeated Close = %v, want the same joined shared result", again)
		}
		wantClose := []string{"close:w", "close:y", "close:x", "close:core"}
		if got := eventsNamedList(e.events.all(), "close"); !slices.Equal(got, wantClose) {
			t.Fatalf("close events = %v, want Workspace scopes before the Runtime scope and reverse dependency order inside it", got)
		}
	})
}

// eventsNamedList returns the ordered event names with the given prefix.
func eventsNamedList(events []string, prefix string) []string {
	var out []string
	for _, event := range eventNames(events) {
		if strings.HasPrefix(event, prefix+":") {
			out = append(out, event)
		}
	}
	return out
}

// --- configuration reload through the admitted-call gate ---

func TestRuntimeReloadSerializesAndShutdownJoinsTheBuild(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		t.Run("two reloads publish successive revisions", func(t *testing.T) {
			e := newOwnerEnv(t)
			r, err := e.open(context.Background(), e.storagePlugin(store))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() {
				if err := r.Close(context.Background()); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()
			var revisions [2]string
			var wg sync.WaitGroup
			for i := range revisions {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					revision, err := r.Reload(context.Background())
					if err != nil {
						t.Errorf("Reload: %v", err)
						return
					}
					revisions[i] = revision
				}(i)
			}
			wg.Wait()
			if !((revisions[0] == "2" && revisions[1] == "3") || (revisions[0] == "3" && revisions[1] == "2")) {
				t.Fatalf("concurrent Reload revisions = %v, want the serialized pair 2 and 3", revisions)
			}
		})

		t.Run("caller cancellation before publication consumes no revision", func(t *testing.T) {
			e := newOwnerEnv(t)
			writeServiceFile(t, e.configPath, ownerGatedConfigDocument)
			validator := &gatedValidator{}
			gate := e.ordinaryPlugin("gate", ScopeRuntime, "gate.cap")
			gate.ValidateConfig = validator.ValidateConfig
			r, err := e.open(context.Background(), e.storagePlugin(store), gate)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() {
				if err := r.Close(context.Background()); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			arrive := validator.arm()
			reloadDone := make(chan error, 1)
			go func() {
				_, err := r.Reload(ctx)
				reloadDone <- err
			}()
			<-arrive
			cancel()
			validator.release()
			if err := <-reloadDone; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled Reload = %v, want the caller's own context error", err)
			}
			if revision, err := r.Reload(context.Background()); err != nil || revision != "2" {
				t.Fatalf("Reload after the canceled build = (%q, %v), want 2: a failed build consumes no generation", revision, err)
			}
		})

		t.Run("shutdown joins the admitted build before releasing the lock", func(t *testing.T) {
			e := newOwnerEnv(t)
			writeServiceFile(t, e.configPath, ownerGatedConfigDocument)
			validator := &gatedValidator{}
			gate := e.ordinaryPlugin("gate", ScopeRuntime, "gate.cap")
			gate.ValidateConfig = validator.ValidateConfig
			r, err := e.open(context.Background(), e.storagePlugin(store), gate)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			arrive := validator.arm()
			reloadDone := make(chan error, 1)
			go func() {
				_, err := r.Reload(context.Background())
				reloadDone <- err
			}()
			<-arrive
			closeDone := make(chan error, 1)
			go func() { closeDone <- r.Close(context.Background()) }()
			select {
			case err := <-closeDone:
				t.Fatalf("Close returned while an admitted build was still in flight (%v); the call gate joined nothing", err)
			case <-time.After(200 * time.Millisecond):
			}
			validator.release()
			select {
			case err := <-closeDone:
				if err != nil {
					t.Fatalf("Close: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Close never converged after the admitted build was released")
			}
			select {
			case err := <-reloadDone:
				if !errors.Is(err, ErrClosed) {
					t.Fatalf("admitted Reload under shutdown = %v, want ErrClosed: a live caller on a done owner still reports closure", err)
				}
			default:
				t.Fatal("Close returned before the admitted Reload returned")
			}
		})
	})
}

// --- client-independent lifetime ---

func TestRuntimeLifetimeBelongsToTheProcessNotItsClients(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		t.Run("a canceled request leaves the owner and other work intact", func(t *testing.T) {
			e := newOwnerEnv(t)
			r, err := e.open(context.Background(), e.storagePlugin(store))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if _, err := r.createSession(context.Background(), e.dataDir, "solo"); err != nil {
				t.Fatalf("createSession: %v", err)
			}
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			held := make(chan struct{})
			callDone := make(chan error, 1)
			go func() {
				callDone <- r.withHarness(requestCtx, func(ctx context.Context, _ *harness.Harness) error {
					close(held)
					<-ctx.Done()
					return ctx.Err()
				})
			}()
			<-held
			if _, err := r.createSession(context.Background(), e.dataDir, "solo"); err != nil {
				t.Fatalf("createSession while one request was in flight: %v", err)
			}
			cancelRequest()
			if err := <-callDone; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled request = %v, want the caller's own error", err)
			}
			if revision, err := r.Reload(context.Background()); err != nil || revision != "2" {
				t.Fatalf("Reload after a canceled request = (%q, %v), want the owner alive at revision 2", revision, err)
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})

		t.Run("a canceled Runtime context rejects entry before the cancellation watcher runs", func(t *testing.T) {
			e := newOwnerEnv(t)
			r, err := e.open(context.Background(), e.storagePlugin(store))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			// The exact post-cancellation pre-watcher state: the owned work
			// context is done while no closure has begun.
			originalWork, originalCancel := r.work, r.cancelWork
			canceledWork, cancelWatcherFree := context.WithCancel(context.Background())
			cancelWatcherFree()
			r.work, r.cancelWork = canceledWork, func() {}
			_, reloadErr := r.Reload(context.Background())
			ranGuardedCall := false
			guardedErr := r.withHarness(context.Background(), func(context.Context, *harness.Harness) error {
				ranGuardedCall = true
				return nil
			})
			_, sessionErr := r.createSession(context.Background(), e.dataDir, "solo")
			r.work, r.cancelWork = originalWork, originalCancel
			if !errors.Is(reloadErr, ErrClosed) {
				t.Fatalf("Reload after owner cancellation = %v, want ErrClosed", reloadErr)
			}
			if ranGuardedCall || !errors.Is(guardedErr, ErrClosed) {
				t.Fatalf("withHarness after owner cancellation = %v (guarded call ran: %v), want ErrClosed with the call never run", guardedErr, ranGuardedCall)
			}
			if !errors.Is(sessionErr, ErrClosed) {
				t.Fatalf("createSession after owner cancellation = %v, want ErrClosed", sessionErr)
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatalf("Close joining the watcher-started shutdown: %v", err)
			}
			e.assertLockReleased(t)
		})

		t.Run("Runtime-context cancellation closes the whole owner without any Close", func(t *testing.T) {
			e := newOwnerEnv(t)
			disposed := make(chan struct{}, 1)
			plugin := e.corePlugin("core", "session_store", store, func() error {
				e.events.add("close:core")
				select {
				case disposed <- struct{}{}:
				default:
				}
				return nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			r, err := e.open(ctx, plugin)
			if err != nil {
				cancel()
				t.Fatalf("open: %v", err)
			}
			cancel()
			select {
			case <-r.shutdownDone:
			case <-time.After(10 * time.Second):
				t.Fatal("Runtime-context cancellation never completed the shared shutdown")
			}
			select {
			case <-disposed:
			default:
				t.Fatal("the watcher-started shutdown disposed no plugin")
			}
			again, err := e.open(context.Background(), e.storagePlugin(store))
			if err != nil {
				t.Fatalf("open after the watcher-started shutdown: %v", err)
			}
			if err := again.Close(context.Background()); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})

		t.Run("closed access rejects and repeated Close joins the same result", func(t *testing.T) {
			e := newOwnerEnv(t)
			r, err := e.open(context.Background(), e.storagePlugin(store))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatalf("first Close: %v", err)
			}
			if _, err := r.Reload(context.Background()); !errors.Is(err, ErrClosed) {
				t.Fatalf("Reload after Close = %v, want ErrClosed", err)
			}
			callerCtx, cancelCaller := context.WithCancel(context.Background())
			cancelCaller()
			if _, err := r.Reload(callerCtx); !errors.Is(err, ErrClosed) {
				t.Fatalf("Reload after Close with a canceled caller = %v, want ErrClosed: admission rejects before the caller check", err)
			}
			if _, err := r.createSession(context.Background(), e.dataDir, "solo"); !errors.Is(err, ErrClosed) {
				t.Fatalf("createSession after Close = %v, want ErrClosed", err)
			}
			if err := r.withHarness(context.Background(), func(context.Context, *harness.Harness) error {
				t.Error("the guarded call ran after Close")
				return nil
			}); !errors.Is(err, ErrClosed) {
				t.Fatalf("withHarness after Close = %v, want ErrClosed", err)
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatalf("repeated Close = %v, want the same joined result", err)
			}
		})
	})
}

func TestRuntimePublishOwnerBarrier(t *testing.T) {
	t.Run("observed cancellation unwinds and reports the context error", func(t *testing.T) {
		e := newOwnerEnv(t)
		r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := r.publishOwner(ctx)
		if got != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("publishOwner after observed cancellation = (%v, %v), want (nil, context error)", got, err)
		}
		if events := eventNames(e.events.all()); !slices.Contains(events, "close:core") {
			t.Fatalf("factory events = %v, want the completed cleanup before the return", events)
		}
		e.assertLockReleased(t)
	})

	t.Run("publication wins while cancellation starts shutdown behind it", func(t *testing.T) {
		e := newOwnerEnv(t)
		r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		got, err := r.publishOwner(context.Background())
		if got != r || err != nil {
			t.Fatalf("publishOwner = (%v, %v), want the same completed owner", got, err)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- workspace identity ---

func TestRuntimeWorkspaceIdentityIsLexicalNormalization(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		opens := make(chan int64, 16)
		ws := e.countedPlugin("ws", ScopeWorkspace, "ws.cap", opens)
		r, err := e.open(context.Background(), e.storagePlugin(store), ws)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() {
			if err := r.Close(context.Background()); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()

		if _, err := r.createSession(context.Background(), "", "solo"); err == nil {
			t.Fatal("an empty workspace was accepted")
		}
		if err := r.withHarness(context.Background(), func(context.Context, *harness.Harness) error { return nil }); err != nil {
			t.Fatalf("withHarness: %v", err)
		}

		t.Run("unclean and relative paths take one Abs", func(t *testing.T) {
			target := filepath.Join(e.home, "real", "project")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			unclean := filepath.Join(e.home, "real", "other", "..", "project")
			rec, err := r.createSession(context.Background(), unclean, "solo")
			if err != nil {
				t.Fatalf("createSession(unclean): %v", err)
			}
			if rec.Identity.Workspace != target {
				t.Fatalf("workspace = %q, want the lexical %q", rec.Identity.Workspace, target)
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			rel, err := filepath.Rel(cwd, target)
			if err != nil {
				t.Fatal(err)
			}
			relative, err := r.createSession(context.Background(), rel, "solo")
			if err != nil {
				t.Fatalf("createSession(relative): %v", err)
			}
			if relative.Identity.Workspace != target {
				t.Fatalf("relative workspace = %q, want the same normalized %q", relative.Identity.Workspace, target)
			}
		})

		t.Run("symlink siblings keep distinct lexical identities", func(t *testing.T) {
			if err := os.MkdirAll(filepath.Join(e.home, "real"), 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(e.home, "link")
			if err := os.Symlink(filepath.Join(e.home, "real"), link); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
			byLink, err := r.createSession(context.Background(), filepath.Join(link, "project"), "solo")
			if err != nil {
				t.Fatalf("createSession(link): %v", err)
			}
			byReal, err := r.createSession(context.Background(), filepath.Join(e.home, "real", "project"), "solo")
			if err != nil {
				t.Fatalf("createSession(real): %v", err)
			}
			if byLink.Identity.Workspace != filepath.Join(link, "project") {
				t.Fatalf("link workspace = %q, want the lexical link path, not %q", byLink.Identity.Workspace, byReal.Identity.Workspace)
			}
			if byLink.Identity.Workspace == byReal.Identity.Workspace {
				t.Fatalf("both workspaces normalized to %q, want distinct lexical identities", byLink.Identity.Workspace)
			}
		})

		t.Run("scope lookup and Fork inheritance use the normalized identity", func(t *testing.T) {
			shared := filepath.Join(e.home, "shared")
			left, err := r.createSession(context.Background(), filepath.Join(shared, "x", ".."), "solo")
			if err != nil {
				t.Fatalf("createSession(left): %v", err)
			}
			submitThroughRuntime(t, r, left.Identity.SessionID, "op-left", "first")
			right, err := r.createSession(context.Background(), shared, "solo")
			if err != nil {
				t.Fatalf("createSession(right): %v", err)
			}
			submitThroughRuntime(t, r, right.Identity.SessionID, "op-right", "first")
			e.prep.awaitCleanups(2)
			if n := <-opens; n != 1 {
				t.Fatalf("Workspace scope constructions = %d, want one shared scope for the same normalized identity", n)
			}

			entries, err := store.ReadEntries(context.Background(), left.Identity.SessionID, 0)
			if err != nil {
				t.Fatalf("ReadEntries: %v", err)
			}
			var boundary string
			for _, entry := range entries {
				if entry.Kind == harness.EntryInput && entry.OperationID == "op-left" {
					boundary = entry.ID
					break
				}
			}
			if boundary == "" {
				t.Fatal("no admitted input entry was found")
			}
			var forked harness.SessionRecord
			err = r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
				res, err := h.Fork(ctx, harness.ForkRequest{
					SourceSessionID: left.Identity.SessionID,
					BoundaryEntryID: boundary,
					OperationID:     "op-fork",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "continue"}},
				})
				if err != nil {
					return err
				}
				forked = res.Session
				return nil
			})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			if forked.Identity.Workspace != shared {
				t.Fatalf("Fork workspace = %q, want the inherited normalized %q", forked.Identity.Workspace, shared)
			}
			if e.scopeWorkspace.Load().(string) != shared {
				t.Fatalf("Workspace scope identity = %q, want the normalized %q", e.scopeWorkspace.Load().(string), shared)
			}
			e.prep.awaitCleanups(3)
			if n := eventsNamed(e.events.all(), "open:ws"); n != 1 {
				t.Fatalf("Workspace scope constructions = %d, want exactly one across both Sessions and the Fork", n)
			}
		})
	})
}

// --- Core storage binding contract ---

func TestRuntimeCoreStorageBindingContract(t *testing.T) {
	t.Run("invalid canonical storage declarations reject before any factory", func(t *testing.T) {
		cases := []struct {
			name    string
			plugins func(e *ownerEnv) []Plugin
		}{
			{"missing", func(e *ownerEnv) []Plugin { return []Plugin{e.ordinaryPlugin("plain", ScopeRuntime, "plain.cap")} }},
			{"duplicate plugins", func(e *ownerEnv) []Plugin {
				return []Plugin{e.storagePlugin(storage.NewMemory()), e.storagePlugin(storage.NewMemory())}
			}},
			{"duplicate exports", func(e *ownerEnv) []Plugin {
				plugin := e.storagePlugin(storage.NewMemory())
				plugin.Provides = append(plugin.Provides, Spec[harness.Storage]("second_store"))
				return []Plugin{plugin}
			}},
			{"wrong scope", func(e *ownerEnv) []Plugin {
				plugin := e.storagePlugin(storage.NewMemory())
				plugin.Scope = ScopeWorkspace
				return []Plugin{plugin}
			}},
			{"required as an ordinary dependency", func(e *ownerEnv) []Plugin {
				return []Plugin{
					e.storagePlugin(storage.NewMemory()),
					e.ordinaryPlugin("needs", ScopeRuntime, "needs.cap", Spec[harness.Storage]("session_store")),
				}
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				e := newOwnerEnv(t)
				wantFailedOpen(t, e, tc.plugins(e), ErrComposition)
				e.assertNoOwnershipSideEffects(t)
			})
		}
	})

	t.Run("names neither grant nor hide canonical authority", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			e := newOwnerEnv(t)
			canonical := &countingStore{Storage: store}
			named := e.ordinaryPlugin("storage", ScopeRuntime, "storage")
			plugin := e.corePlugin("state", "whatever", canonical, func() error { e.events.add("close:state"); return nil })
			r, err := e.open(context.Background(), plugin, named)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if err := r.Close(context.Background()); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if canonical.listCount() == 0 {
				t.Fatal("recovery never used the type-identified Core binding")
			}
		})
	})
}

// --- restart recovery before execution ---

func TestRuntimeRecoveryRepairsBeforeExecution(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		sessionID, operationID := seedRunningOperation(t, store, filepath.Join(e.home, "seeded"))
		r, err := e.open(context.Background(), e.storagePlugin(&countingStore{Storage: store}))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if calls, opens := e.prep.counts(); calls != 0 || opens != 0 {
			t.Fatalf("preparation/opener calls during startup = %d/%d, want recovery to invoke no execution callback", calls, opens)
		}
		rec := readOperation(t, r, sessionID, operationID)
		if rec.State.Status != harness.OperationInterruption || rec.State.Terminal == nil ||
			rec.State.Terminal.Detail != "Operation interrupted by Runtime loss." {
			t.Fatalf("recovered Operation state = %+v, want the terminal interruption settlement", rec.State)
		}
		// Repair precedes execution: a later admission enters ordinary
		// admission on the repaired durable state.
		next, err := r.createSession(context.Background(), filepath.Join(e.home, "seeded"), "solo")
		if err != nil {
			t.Fatalf("createSession after recovery: %v", err)
		}
		submitThroughRuntime(t, r, next.Identity.SessionID, "op-after-repair", "continue")
		e.prep.awaitCleanups(1)
		if post := readOperation(t, r, next.Identity.SessionID, "op-after-repair"); post.State.Status != harness.OperationSuccess {
			t.Fatalf("post-repair Operation = %+v, want success", post.State)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}
