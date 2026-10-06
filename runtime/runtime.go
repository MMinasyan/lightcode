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

// Options are the public construction inputs of one managed Runtime: the
// sole data root, the main configuration path, and the complete plugin set —
// builtin.Plugins() for the shipped registration, an ordinary custom slice
// for a custom build.
type Options struct {
	DataDir    string
	ConfigPath string
	Plugins    []Plugin
}

// Open constructs the complete managed Runtime with concrete production
// preparation and returns the published owner. It delegates to the private
// lifecycle implementation, which owns every retained property: explicit
// paths, ownership-before-I/O, config publication, recovery-before-execution,
// one admitted-call gate, and the joined shutdown.
func Open(ctx context.Context, opts Options) (*Runtime, error) {
	return open(ctx, options{
		DataDir:    opts.DataDir,
		ConfigPath: opts.ConfigPath,
		Plugins:    opts.Plugins,
	})
}

// options are the private construction inputs of one managed Runtime. DataDir
// is the sole data root: runtime.lock and the storage backend's own files
// derive from it, while agents.json stays beside ConfigPath. Dotenv and the
// discovery cache keep their home-based paths: neither option relocates them
// nor changes owner identity. prepare is the controlled preparation function
// supplied by tests; a nil prepare selects the concrete production
// preparation. sweepTicks optionally replaces the automatic sweep scheduler's
// owned hourly ticker with a controlled tick stream whose sends carry each
// pass's explicit time; the zero value keeps the production time.Ticker.
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
	warnings     *warningStore
	passive      *observationAdapter
	managedEnv   *config.ManagedEnv
	runtimeScope *scope
	workspaces   *workspaceScopes
	harness      *harness.Harness
	artifacts    *artifactIntervals
	dataDir      string
	// protocol is the one attached isolated protocol server, set under mu by
	// OpenProtocol and frozen once closure begins; nil when none is attached.
	protocol *ProtocolServer

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
// finished. It requires nonempty paths, normalizes both
// paths once with filepath.Abs, and validates the context before any
// initialization. Startup order is fixed: validate declarations (definitions
// and capability declarations, including exactly one Runtime-scoped export
// declared as harness.Storage, all before any factory), acquire the
// process-lifetime lock at <DataDir>/runtime.lock, load the managed dotenv and
// publish the initial configuration snapshot, construct the complete Runtime
// scope in the composition's stable topological order, obtain the private Core
// storage export and the optional job-stop seam, run restart recovery against
// the storage, and construct the Harness with the bound preparation and seam.
// The storage factory initializes its backend
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

	managedEnv, err := config.LoadDotEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: .env: %v\n", err)
	}

	obs := newObservation()
	// The warning store and the passive publication adapter initialize
	// before any plugin scope opens: the Runtime installs the adapter's
	// neutral plugin-warning sink on the composition, which binds each
	// Runtime-scoped factory's copied ScopeInfo to its own registered ID, and
	// scopes and preparations report into the store from the moment they
	// exist.
	warnings := newWarningStore()
	adapter := newObservationAdapter(obs, warnings)
	c.reportWarning = adapter.reportPluginWarning
	loader := catalog.NewLoader(home, nil)
	configService := newConfigurationService(work, c, loader, configPath, obs)
	configService.attachWarnings(warnings)
	configService.attachEnv(managedEnv)
	if _, err := configService.publish(work); err != nil {
		// Initial publication supplies the owned context as both caller and
		// owner, so its caller-first checkpoints report a canceled
		// constructor's own context error directly, while every independent
		// failure — including a source error that wraps ErrClosed — keeps
		// its identity unchanged.
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
	jobs, err := jobStopper(c, runtimeScope)
	if err != nil {
		return unwind(err)
	}
	if err := harness.Recover(work, storage); err != nil {
		return unwind(err)
	}

	workspaces := newWorkspaceScopes(work, c, []*scope{runtimeScope}, obs)
	background := &backgroundBridge{}
	// The private per-Session artifact interval registry initializes before
	// the Harness: every committed opener, restore, committed deletion and
	// sweep candidate coordinates on it, and no caller can observe an
	// unarmed registry because the owner is unpublished.
	artifacts := newArtifactIntervals()
	// The call-time subprocess environment producer exists only when the
	// retained manager exists; without it no manager is constructed on
	// demand and every cooperative command fails.
	var subprocessEnv func() []string
	if managedEnv != nil {
		subprocessEnv = managedEnv.SubprocessEnv
	}
	h, err := harness.New(work, harness.Dependencies{
		Storage: storage,
		Jobs:    jobs,
		Prepare: newPreparation(configService, c, runtimeScope, workspaces, home, background, adapter, artifacts, subprocessEnv, options.prepare).bind(),
		Observe: adapter.observe,
	})
	if err != nil {
		return unwind(err)
	}
	// Armed before publication: harness.New performed no I/O and the first
	// Prepare requires the admission gate, so no caller can observe the
	// unarmed bridge or the adapter's unbound Harness.
	background.h = h
	adapter.h = h

	r := &Runtime{
		lock:         lock,
		work:         work,
		cancelWork:   cancelWork,
		config:       configService,
		obs:          obs,
		warnings:     warnings,
		passive:      adapter,
		managedEnv:   managedEnv,
		runtimeScope: runtimeScope,
		workspaces:   workspaces,
		harness:      h,
		artifacts:    artifacts,
		dataDir:      dataDir,
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
	if isTypedNil(storage) {
		return nil, fmt.Errorf("Core storage export supplies a typed-nil harness.Storage: %w", ErrComposition)
	}
	return storage, nil
}

// isTypedNil reports whether v's dynamic value is a nil of a nil-able kind:
// the one typed-nil shape shared by the private Harness seam bindings.
func isTypedNil(v any) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

// jobStopper resolves the optional Harness job-stop seam over the Runtime
// scope: composition recorded every export declared exactly as
// harness.JobStopper, zero leaves the seam nil, and the single
// Runtime-scoped export resolves from the scope's private Core seam values
// after the coreStorage-shape typed-nil check — multiple or narrower-scoped
// exports fail composition. It runs after the Runtime scope's bindings commit
// and before Harness construction, so a failure never publishes a Harness.
func jobStopper(c *composition, runtimeScope *scope) (harness.JobStopper, error) {
	switch {
	case len(c.jobStoppers) == 0:
		return nil, nil
	case len(c.jobStoppers) > 1:
		return nil, fmt.Errorf("%d exports are declared as harness.JobStopper: %w", len(c.jobStoppers), ErrComposition)
	case c.jobStoppers[0].scope != ScopeRuntime:
		return nil, fmt.Errorf("job stopper export %q is %s-scoped: %w", c.jobStoppers[0].id, c.jobStoppers[0].scope, ErrComposition)
	}
	stopper := runtimeScope.jobStoppers[c.jobStoppers[0].id].(harness.JobStopper)
	if isTypedNil(stopper) {
		return nil, fmt.Errorf("job stopper export %q supplies a typed-nil harness.JobStopper: %w", c.jobStoppers[0].id, ErrComposition)
	}
	return stopper, nil
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

// createSession is the private root Session creation: it runs the one
// normalized creation core inside the shared admitted-call gate.
func (r *Runtime) createSession(ctx context.Context, workspace, agentType string) (harness.SessionRecord, error) {
	var record harness.SessionRecord
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		var err error
		record, err = r.createSessionRecord(ctx, h, workspace, agentType)
		return err
	}); err != nil {
		return harness.SessionRecord{}, err
	}
	return record, nil
}

// createSessionRecord is the one root creation core: the existing empty
// workspace check wraps the shared harness.ErrInvalid sentinel, the Workspace
// is normalized with filepath.Abs alone, and the Harness retains its
// nonempty-only Agent-type selection. It executes inside the caller's
// admission.
func (r *Runtime) createSessionRecord(ctx context.Context, h *harness.Harness, workspace, agentType string) (harness.SessionRecord, error) {
	if workspace == "" {
		return harness.SessionRecord{}, fmt.Errorf("workspace must be non-empty: %w", harness.ErrInvalid)
	}
	normalized, err := filepath.Abs(workspace)
	if err != nil {
		return harness.SessionRecord{}, fmt.Errorf("normalize workspace: %w", err)
	}
	return h.CreateSession(ctx, harness.CreateSessionRequest{Workspace: normalized, AgentType: agentType})
}

// deleteSession is the private canonical Session deletion: the whole body
// runs inside one admitted call, so shutdown joins the deletion and its
// cleanup together. The deletion takes the Session's artifact interval
// nonblockingly around its Harness transition and retains it through the
// warning and artifact cleanup, so a held interval refuses the deletion
// before any transition; after Harness.DeleteSession commits — or reports
// the Session already absent for a valid identity, the same idempotent result
// — the Session's warning groups are removed in one observation section with
// one runtime-scoped hint, then its artifact tree is removed once. Any other
// Harness error (invalid input, corruption, revision races, a closed
// admission) returns as-is and authorizes no cleanup.
func (r *Runtime) deleteSession(ctx context.Context, sessionID string) error {
	return r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		releaseArtifacts, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, sessionID))
		if err != nil {
			return err
		}
		defer releaseArtifacts()
		err = h.DeleteSession(ctx, sessionID)
		if err != nil && !errors.Is(err, harness.ErrNotFound) {
			return err
		}
		r.passive.removeSessionWarnings([]string{sessionID})
		return r.removeSessionCode(sessionID)
	})
}

// removeSessionCode removes one Session's artifact tree under the normalized
// data directory — its snapshots and command spills. It removes nothing else:
// not sibling Sessions' trees, not other data-directory content. A missing
// directory is already clean.
func (r *Runtime) removeSessionCode(sessionID string) error {
	return os.RemoveAll(filepath.Join(r.dataDir, "code", sessionID))
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
	return strconv.FormatUint(snapshot.snapshot.generation, 10), nil
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
// then closes the attached protocol server's listener and every active
// connection outside the mutex — before any admitted call is joined — and
// only then starts the one shared cleanup asynchronously. Repeat callers
// only join.
func (r *Runtime) beginShutdown() {
	r.shutdownOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		server := r.protocol
		r.mu.Unlock()
		// The warning store's admission closes with the Runtime: later
		// reports are ignored presentation. Short in-memory coordination
		// only — no network or plugin work runs here.
		r.warnings.close()
		r.cancelWork()
		// The attached server's listener and every active stream close
		// before admitted calls converge: an events stream never holds the
		// call wait open, and a served request observes its own work
		// context end instead of a live server. Once attached, the serving
		// goroutine always started, so this close always joins it.
		var closeErr error
		if server != nil {
			closeErr = server.closeNetwork()
		}
		go func() {
			r.shutdownErr = errors.Join(closeErr, r.joinShutdown())
			close(r.shutdownDone)
		}()
	})
}

// joinShutdown converges the owner: it joins admitted Runtime work, stops
// every background ownership group so live members deliver and finish while
// the capabilities and storage are still open, then joins the Harness after
// its context cancellation has settled every in-flight preparation,
// execution, and required terminal commit, then closes every
// live Workspace scope in sorted path order and the Runtime scope in reverse
// dependency order, closes every passive subscription once those cleanup
// events have been published, withdraws the attached protocol server's
// remembered discovery record, and releases the lock as the final ownership
// action. Every required close is attempted and all errors are joined; no
// state-owning mutex is held across any of it.
func (r *Runtime) joinShutdown() error {
	var errs []error
	r.calls.Wait()
	if err := r.harness.StopAll(context.Background()); err != nil {
		errs = append(errs, err)
	}
	if err := r.harness.Wait(context.Background()); err != nil {
		errs = append(errs, err)
	}
	if err := r.workspaces.shutdown(); err != nil {
		errs = append(errs, err)
	}
	if err := r.runtimeScope.close(); err != nil {
		errs = append(errs, err)
	}
	r.obs.closeAll()
	// The remembered discovery record is withdrawn after the Harness and
	// every scope have converged and before the owner lock is released, so
	// no successor observes a record this owner cannot answer. After closure
	// began, the attached server is frozen, so this read is stable.
	r.mu.Lock()
	server := r.protocol
	r.mu.Unlock()
	if server != nil {
		if err := server.withdrawDiscovery(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.lock.Release(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
