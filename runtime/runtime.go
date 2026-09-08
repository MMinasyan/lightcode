package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
)

// ErrOwned reports advisory-lock contention: another Runtime process owns the
// normalized data directory, so this contender initializes no authoritative
// state and invokes no factory.
var ErrOwned = errors.New("runtime: data directory owned by another Runtime")

// options are the private construction inputs of one managed Runtime. DataDir
// is the sole data root: runtime.lock and the storage backend's own files
// derive from it, while agents.json stays beside ConfigPath. Dotenv and the
// discovery cache keep their home-based paths: neither option relocates them
// nor changes owner identity. prepare is the controlled preparation function
// supplied by every caller until concrete production preparation lands.
// sweepTicks optionally replaces the automatic sweep scheduler's owned
// hourly ticker with a controlled tick stream whose sends carry each pass's
// explicit time; the zero value keeps the production time.Ticker.
type options struct {
	DataDir, ConfigPath string
	Plugins             []Plugin
	prepare             prepare
	sweepTicks          <-chan time.Time
}

// Runtime is the single live owner of one Harness, its durable Session
// access, the composed plugin instances, and running Agents. The process
// owns the Runtime lifetime: no client connection or Session request does.
// The constructor context is the managed Runtime process lifetime; its
// cancellation requests the same joined shutdown that Close starts.
type Runtime struct {
	lock         *atomicfs.Lock
	work         context.Context
	cancelWork   context.CancelFunc
	config       *configurationService
	obs          *observation
	runtimeScope *scope
	workspaces   *workspaceScopes
	harness      *harness.Harness

	// mu guards only the admission transition below; it is never held across
	// any call, wait, or I/O.
	mu     sync.Mutex
	closed bool
	calls  sync.WaitGroup

	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

// open constructs the complete owner and publishes nothing until it has
// finished. It requires nonempty paths and a non-nil prepare, normalizes both
// paths once with filepath.Abs, and validates the context before any
// initialization. Startup order is fixed: validate declarations (definitions
// and capability declarations, including exactly one Runtime-scoped export
// declared as harness.Storage, all before any factory), acquire the
// process-lifetime lock at <DataDir>/runtime.lock, load the managed dotenv and
// publish the initial configuration snapshot, construct the complete Runtime
// scope in the composition's stable topological order, obtain the private Core
// storage export, run restart recovery against it, and construct the Harness
// with the bound preparation. The storage factory initializes its backend
// under the held ownership; other factories receive no canonical Storage.
// The automatic sweep's owned ticker loop is then registered as Runtime work
// and one initial pass runs, both before the completed-owner publication.
// Failure or cancellation before completed construction unwinds every
// acquired resource, closes state access before releasing the lock, and
// preserves committed repair. Construction never returns a non-nil Runtime
// together with an error; the Harness and every scope stay private until this
// call returns.
func open(ctx context.Context, options options) (*Runtime, error) {
	if ctx == nil {
		return nil, errors.New("runtime: construction requires a non-nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.DataDir == "" {
		return nil, errors.New("runtime: options.DataDir must be non-empty")
	}
	if options.ConfigPath == "" {
		return nil, errors.New("runtime: options.ConfigPath must be non-empty")
	}
	if options.prepare == nil {
		return nil, errors.New("runtime: options.prepare must be non-nil")
	}
	dataDir, err := filepath.Abs(options.DataDir)
	if err != nil {
		return nil, fmt.Errorf("normalize data directory: %w", err)
	}
	configPath, err := filepath.Abs(options.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("normalize config path: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	c, err := newComposition(options.Plugins)
	if err != nil {
		return nil, err
	}
	if err := requireCoreStorage(c); err != nil {
		return nil, err
	}

	lock, ok, err := atomicfs.TryAcquire(filepath.Join(dataDir, "runtime.lock"))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("data directory %s: %w", dataDir, ErrOwned)
	}
	work, cancelWork := context.WithCancel(ctx)
	unlock := func(cause error) (*Runtime, error) {
		cancelWork()
		return nil, errors.Join(cause, lock.Release())
	}

	if _, err := config.LoadDotEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: .env: %v\n", err)
	}

	obs := newObservation()
	configService := newConfigurationService(work, c, catalog.NewLoader(home, nil), configPath, obs)
	if _, err := configService.publish(work); err != nil {
		// The service's owner-first cancellation rule reports its own bare
		// ErrClosed sentinel value, but no owner is closed yet during
		// construction: exactly that value with a canceled constructor
		// context returns the context's own error. Every other failure,
		// including a source error that wraps ErrClosed, keeps its identity
		// unchanged.
		if err == ErrClosed && ctx.Err() != nil {
			return unlock(ctx.Err())
		}
		return unlock(err)
	}

	runtimeScope, err := c.openScope(work, ScopeInfo{Kind: ScopeRuntime, DataDir: dataDir}, nil)
	if err != nil {
		return unlock(err)
	}
	// The observation is attached after construction: no subscriber can
	// exist before the owner is published, so the Runtime scope announces
	// only its closure, and descendant scopes inherit the publisher from it.
	runtimeScope.obs = obs
	unwind := func(cause error) (*Runtime, error) {
		cancelWork()
		return nil, errors.Join(cause, runtimeScope.cleanup(), lock.Release())
	}

	storage, err := coreStorage(runtimeScope)
	if err != nil {
		return unwind(err)
	}
	if err := harness.Recover(work, storage); err != nil {
		return unwind(err)
	}

	workspaces := newWorkspaceScopes(work, c, []*scope{runtimeScope}, obs)
	h, err := harness.New(work, harness.Dependencies{
		Storage: storage,
		Prepare: newPreparation(configService, c, runtimeScope, workspaces, options.prepare).bind(),
	})
	if err != nil {
		return unwind(err)
	}

	r := &Runtime{
		lock:         lock,
		work:         work,
		cancelWork:   cancelWork,
		config:       configService,
		obs:          obs,
		runtimeScope: runtimeScope,
		workspaces:   workspaces,
		harness:      h,
		shutdownDone: make(chan struct{}),
	}
	sweepTicks, stopSweepTicker := options.sweepTicks, func() {}
	if sweepTicks == nil {
		ticker := time.NewTicker(sweepInterval)
		sweepTicks, stopSweepTicker = ticker.C, ticker.Stop
	}
	r.startMaintenance(sweepTicks, stopSweepTicker)
	return r.publishOwner(work)
}

// requireCoreStorage enforces the canonical-state contract on the compiled
// declarations before any factory: exactly one export is declared as
// harness.Storage, and its plugin is Runtime-scoped. Names grant nothing; an
// unrelated ordinary capability may be named storage.
func requireCoreStorage(c *composition) error {
	switch {
	case len(c.coreExports) == 0:
		return fmt.Errorf("no export is declared as harness.Storage: %w", ErrComposition)
	case len(c.coreExports) > 1:
		return fmt.Errorf("%d exports are declared as harness.Storage: %w", len(c.coreExports), ErrComposition)
	case c.coreExports[0].scope != ScopeRuntime:
		return fmt.Errorf("Core storage export %q is %s-scoped: %w", c.coreExports[0].id, c.coreExports[0].scope, ErrComposition)
	}
	return nil
}

// coreStorage obtains the constructed Runtime scope's private Core export for
// the owner. All capabilities of the storage plugin already share its ordinary
// scope lifecycle; only the Runtime passes this value to the Harness and
// recovery. An invalid success unwinds through the same scope rollback and
// Instance.Close as other invalid exports.
func coreStorage(runtimeScope *scope) (harness.Storage, error) {
	var value any
	for _, stored := range runtimeScope.storages {
		value = stored
	}
	storage, ok := value.(harness.Storage)
	if !ok {
		return nil, fmt.Errorf("Core storage export supplies %T, not harness.Storage: %w", value, ErrComposition)
	}
	if rv := reflect.ValueOf(storage); rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Map ||
		rv.Kind() == reflect.Slice || rv.Kind() == reflect.Chan || rv.Kind() == reflect.Func || rv.Kind() == reflect.UnsafePointer {
		if rv.IsNil() {
			return nil, fmt.Errorf("Core storage export supplies a typed-nil harness.Storage: %w", ErrComposition)
		}
	}
	return storage, nil
}

// publishOwner registers the constructor context's cancellation watcher and
// checks the context once more before completed-owner publication. Observed
// cancellation returns (nil, context error) only after the shared cleanup has
// joined; once publication wins, the Runtime is returned even if cancellation
// immediately starts its shutdown. The watcher requests the same asynchronous
// shutdown a Close starts and never waits inside its callback; it is not
// admitted work the cleanup joins.
func (r *Runtime) publishOwner(work context.Context) (*Runtime, error) {
	context.AfterFunc(work, r.beginShutdown)
	if err := work.Err(); err != nil {
		r.beginShutdown()
		<-r.shutdownDone
		return nil, err
	}
	return r, nil
}

// enter registers one admitted call under the short Runtime mutex: explicit
// closure or a canceled Runtime context rejects entry with ErrClosed,
// including before the cancellation watcher runs, and a canceled caller
// context is rejected with its own error. The caller executes outside the
// mutex and deregisters once on return.
func (r *Runtime) enter(ctx context.Context) (func(), error) {
	r.mu.Lock()
	if r.closed || r.work.Err() != nil {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.calls.Add(1)
	r.mu.Unlock()
	var once sync.Once
	return func() { once.Do(r.calls.Done) }, nil
}

// withHarness holds one admission, not the mutex, across its call. It is the
// only Harness access: there is no public getter and no forwarding-method
// family.
func (r *Runtime) withHarness(ctx context.Context, fn func(context.Context, *harness.Harness) error) error {
	release, err := r.enter(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx, r.harness)
}

// createSession is the private root Session creation: it normalizes the
// Workspace with filepath.Abs alone and uses the same admitted-call gate.
func (r *Runtime) createSession(ctx context.Context, workspace, agentType string) (harness.SessionRecord, error) {
	if workspace == "" {
		return harness.SessionRecord{}, errors.New("runtime: workspace must be non-empty")
	}
	normalized, err := filepath.Abs(workspace)
	if err != nil {
		return harness.SessionRecord{}, fmt.Errorf("normalize workspace: %w", err)
	}
	var record harness.SessionRecord
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		var err error
		record, err = h.CreateSession(ctx, harness.CreateSessionRequest{Workspace: normalized, AgentType: agentType})
		return err
	}); err != nil {
		return harness.SessionRecord{}, err
	}
	return record, nil
}

// Reload publishes the next configuration revision through the admitted-call
// gate and returns its revision string. It is serialized by the configuration
// service's own build mutex; the gate joins it to shutdown and holds no
// Runtime mutex across the build.
func (r *Runtime) Reload(ctx context.Context) (string, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	snapshot, err := r.config.publish(ctx)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(snapshot.generation, 10), nil
}

// Close starts (or joins) the one shared managed shutdown and waits for it.
// The argument bounds only this caller's wait; every Close caller joins the
// same result.
func (r *Runtime) Close(ctx context.Context) error {
	r.beginShutdown()
	select {
	case <-r.shutdownDone:
		return r.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// beginShutdown first closes admission and cancels the owned work context,
// then starts the one shared cleanup asynchronously. Repeat callers only join.
func (r *Runtime) beginShutdown() {
	r.shutdownOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.cancelWork()
		go func() {
			r.shutdownErr = r.joinShutdown()
			close(r.shutdownDone)
		}()
	})
}

// joinShutdown converges the owner: it joins admitted Runtime work, then the
// Harness after its context cancellation has settled every in-flight
// preparation, execution, and required terminal commit, then closes every
// live Workspace scope in sorted path order and the Runtime scope in reverse
// dependency order, closes every passive subscription once those cleanup
// events have been published, and releases the lock as the final ownership
// action. Every required close is attempted and all errors are joined; no
// state-owning mutex is held across any of it.
func (r *Runtime) joinShutdown() error {
	var errs []error
	r.calls.Wait()
	if err := r.harness.Wait(context.Background()); err != nil {
		errs = append(errs, err)
	}
	if err := r.workspaces.shutdown(context.Background()); err != nil {
		errs = append(errs, err)
	}
	if err := r.runtimeScope.close(context.Background()); err != nil {
		errs = append(errs, err)
	}
	r.obs.closeAll()
	if err := r.lock.Release(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
