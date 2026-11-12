package melta

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Syntheresis/melta/extension"
	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/atomicfs"
	"github.com/Syntheresis/melta/internal/catalog"
	"github.com/Syntheresis/melta/internal/jobs"
	"github.com/Syntheresis/melta/internal/pathutil"
	"github.com/Syntheresis/melta/internal/storage"
	"github.com/Syntheresis/melta/protocol"
)

// ErrOwned reports advisory-lock contention: another Melta process owns the
// normalized data directory, so this contender initializes no authoritative
// state and invokes no factory.
var ErrOwned = errors.New("melta: data directory owned by another Melta")

// Options are the public construction inputs of one managed Melta: the
// sole data root, the application-supplied configuration and credential
// sources, and the complete plugin set — builtin.Plugins() for the shipped
// registration, an ordinary custom slice for a custom build. Optional
// locations are independently selected; an omitted location performs no
// lookup there. Nil Agent and Strategy select the included defaults, and nil
// NewEvaluator selects the included permission evaluator.
type Options struct {
	DataDir       string
	Configuration ConfigurationSource
	Credentials   CredentialSource
	Locations     Locations

	// Identity is the application-supplied prompt persona text. Its zero
	// value supplies no identity, never an implicit fallback.
	Identity string

	Agent    *harness.AgentInputs
	Strategy *harness.StrategyBinding

	Extensions []extension.Native

	// OpenStore optionally replaces the included SQLite construction. The
	// supplied factory owns exclusivity of its actual storage namespace; the
	// acquired closer releases that namespace after durable access closes.
	OpenStore func(context.Context, string) (harness.Storage, func() error, error)

	NewEvaluator func(PolicyInputs) (harness.PermissionEvaluator, error)

	// BeforeRelease is the optional host withdrawal callback: it runs once
	// after admitted work and scopes settle and before storage or the
	// ownership lock is released. It must not recursively Close or admit
	// work; its errors join the cleanup result.
	BeforeRelease func() error
}

// Open constructs the complete managed Melta with concrete production
// preparation and returns the published owner. It delegates to the private
// lifecycle implementation, which owns every retained property: explicit
// sources and locations, ownership-before-I/O, config publication,
// recovery-before-execution, one admitted-call gate, and the joined shutdown.
func Open(ctx context.Context, opts Options) (*Melta, error) {
	return open(ctx, options{
		DataDir:       opts.DataDir,
		Configuration: opts.Configuration,
		Credentials:   opts.Credentials,
		Locations:     opts.Locations,
		Identity:      opts.Identity,
		Agent:         opts.Agent,
		Strategy:      opts.Strategy,
		Extensions:    opts.Extensions,
		OpenStore:     opts.OpenStore,
		NewEvaluator:  opts.NewEvaluator,
		BeforeRelease: opts.BeforeRelease,
	})
}

// options are the private construction inputs of one managed Melta. DataDir
// is the sole data root: runtime.lock and the storage backend's own files
// derive from it. sweepTicks optionally replaces the automatic sweep
// scheduler's owned hourly ticker with a controlled tick stream whose sends
// carry each pass's explicit time; the zero value keeps the production
// time.Ticker.
type options struct {
	DataDir       string
	Configuration ConfigurationSource
	Credentials   CredentialSource
	Locations     Locations
	Identity      string
	Agent         *harness.AgentInputs
	Strategy      *harness.StrategyBinding
	Extensions    []extension.Native

	OpenStore     func(context.Context, string) (harness.Storage, func() error, error)
	NewEvaluator  func(PolicyInputs) (harness.PermissionEvaluator, error)
	BeforeRelease func() error
	sweepTicks    <-chan time.Time
}

// Melta is the single live owner of one Harness, its durable Session
// access, the composed plugin instances, and running Agents. The process
// owns the Melta lifetime: no client connection or Session request does.
// The constructor context is the managed Melta process lifetime; its
// cancellation requests the same joined shutdown that Close starts.
type Melta struct {
	lock          *atomicfs.Lock
	work          context.Context
	cancelWork    context.CancelFunc
	config        *configurationService
	obs           *observation
	warnings      *warningStore
	passive       *observationAdapter
	agentInputs   harness.AgentInputs
	harness       *harness.Harness
	artifacts     *artifactIntervals
	dataDir       string
	instance      string
	beforeRelease func() error
	packages      *packageManager

	// jobs and closeStore are the directly constructed baseline services:
	// the jobs service (also the Harness job-stop seam) and the acquired
	// store's closer. Both close after Harness settlement, the jobs work
	// group joining before the store closes and the lock releases.
	jobs       jobsService
	closeStore func() error
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
// finished. It requires nonempty construction inputs, resolves the data
// directory once to its canonical path (existing symlinks included and a
// missing tail preserved), and validates the context before any
// initialization. Startup acquires the process-lifetime lock at
// <DataDir>/runtime.lock, validates the active composition and initial
// configuration from the supplied sources, directly constructs the included
// SQLite store and jobs service, then prepare and publish the initial resource
// set with the standard tools bound before running restart recovery, and
// construct the Harness with the bound preparation and the jobs stop seam.
// Other factories receive no canonical Storage.
// The automatic sweep's owned ticker loop is then registered as Melta work
// and one initial pass runs, both before the completed-owner publication.
// Failure or cancellation before completed construction unwinds every acquired
// resource in reverse acquisition order — the constructed Melta scope's
// instances, then the jobs work group joining, then the store — closing state
// access before releasing the lock, and preserves committed repair.
// Construction never returns a non-nil Melta together with an error; the
// Harness and every scope stay private until this call returns.
func open(ctx context.Context, options options) (*Melta, error) {
	if ctx == nil {
		return nil, fmt.Errorf("melta: construction requires a non-nil context: %w", harness.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.DataDir == "" {
		return nil, fmt.Errorf("melta: options.DataDir must be non-empty: %w", harness.ErrInvalid)
	}
	if options.Configuration == nil {
		return nil, fmt.Errorf("melta: options.Configuration must be non-nil: %w", harness.ErrInvalid)
	}
	if options.Credentials == nil {
		return nil, fmt.Errorf("melta: options.Credentials must be non-nil: %w", harness.ErrInvalid)
	}
	inputs := DefaultAgentInputs()
	if options.Agent != nil {
		if err := harness.ValidateAgentInputs(*options.Agent); err != nil {
			return nil, fmt.Errorf("melta: options.Agent: %v: %w", err, harness.ErrInvalid)
		}
		inputs = options.Agent.Owned()
	}
	strategy := harness.IncludedStrategyBinding()
	if options.Strategy != nil {
		if options.Strategy.ID == "" || options.Strategy.Strategy == nil {
			return nil, fmt.Errorf("melta: options.Strategy requires an ID and an implementation: %w", harness.ErrInvalid)
		}
		owned := *options.Strategy
		strategy = &owned
	}
	resolved, err := pathutil.ResolveFilePath(options.DataDir)
	if err != nil {
		return nil, fmt.Errorf("normalize data directory: %w", err)
	}
	dataDir := resolved.CanonicalPath
	instance, err := mintInstance()
	if err != nil {
		return nil, err
	}
	var configService *configurationService
	supervisor := newExecutableSupervisor(func(ctx context.Context, excluded []string) ([]string, error) {
		return configService.executableEnvironment(ctx, excluded)
	})
	lock, ok, err := atomicfs.TryAcquire(filepath.Join(dataDir, "runtime.lock"))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("data directory %s: %w", dataDir, ErrOwned)
	}
	work, cancelWork := context.WithCancel(ctx)
	packages := newPackageManager(dataDir)
	if err := packages.load(supervisor.native); err != nil {
		cancelWork()
		return nil, errors.Join(err, lock.Release())
	}
	natives := append([]extension.Native(nil), options.Extensions...)
	natives = append(natives, packages.loadedNatives()...)
	c := registeredComposition(natives)

	obs := newObservation()
	// The warning store and the passive publication adapter initialize
	// before any plugin scope opens: the Melta installs the adapter's
	// neutral plugin-warning sink on the composition, which binds each
	// Melta-scoped factory's InstanceContext warning callback to its own
	// registered ID, and scopes and preparations report into the store from
	// the moment they exist.
	warnings := newWarningStore()
	adapter := newObservationAdapter(obs, warnings)
	adapter.instance = instance
	c.reportWarning = adapter.reportPluginWarning
	loader := catalog.NewLoader(options.Locations.DiscoveryCacheDir, nil)
	configService = newConfigurationService(work, c, loader, options.Configuration, options.Credentials, options.Locations, strategy, obs)
	configService.executables = packages.loadedExecutables()
	configService.unusablePackages = packages.unusable
	configService.supervisor = supervisor
	configService.instance = instance
	configService.attachWarnings(warnings)
	// The private per-Session artifact interval registry initializes before
	// the first resource set: every committed opener, standalone data call,
	// restore, committed deletion and sweep candidate coordinates on it, and
	// the same shared host owner supplies it to the data service.
	artifacts := newArtifactIntervals()
	c.hosts.artifacts = artifacts
	c.hosts.dataDir = dataDir
	initial, err := configService.build(work, 1)
	if err != nil {
		cancelWork()
		return nil, errors.Join(err, lock.Release())
	}
	if err := configService.canceled(work); err != nil {
		cancelWork()
		return nil, errors.Join(err, lock.Release())
	}

	// unwind releases the acquired prefix in reverse acquisition order: each
	// closer joins its own work — the jobs work group covers every monitor and
	// completion callback — and the ownership lock releases last. A failed
	// construction never appends its own resource: openScope already cleaned
	// the partial scope it built.
	var acquired []func() error
	unwind := func(cause error) (*Melta, error) {
		cancelWork()
		errs := []error{cause}
		for i := len(acquired) - 1; i >= 0; i-- {
			errs = append(errs, acquired[i]())
		}
		errs = append(errs, lock.Release())
		return nil, errors.Join(errs...)
	}

	// The baseline services are acquired before the optional Melta scope
	// opens, so a Melta-scoped factory's declared dependency on a standard
	// tool resolves from the scope's own bindings when its factory runs.
	openStorage := options.OpenStore
	if openStorage == nil {
		openStorage = openStore
	}
	store, closeStorage, err := openStorage(work, dataDir)
	if err != nil {
		return unwind(err)
	}
	if store == nil || closeStorage == nil {
		return unwind(fmt.Errorf("melta: OpenStore must return a storage and its closer: %w", harness.ErrInvalid))
	}
	acquired = append(acquired, closeStorage)
	jobsService := newJobs(dataDir)
	acquired = append(acquired, jobsService.Close)
	c.standardValues = newStandardTools(dataDir, jobsService)
	background := &backgroundBridge{}
	c.hosts.background = background
	configService.composition = c
	configService.dataDir = dataDir
	if err := configService.prepareResources(work, initial); err != nil {
		return unwind(err)
	}
	resources := initial.resources
	acquired = append(acquired, resources.release)
	if err := configService.projectDefinitions(work, initial); err != nil {
		return unwind(err)
	}
	if err := configService.canceled(work); err != nil {
		return unwind(err)
	}
	configService.buildMu.Lock()
	configService.commit(initial)
	if err := harness.Recover(work, store); err != nil {
		return unwind(err)
	}

	// The private per-Session artifact interval registry initializes before
	// the Harness: every committed opener, restore, committed deletion and
	// sweep candidate coordinates on it, and no Caller can observe an
	// unarmed registry because the owner is unpublished.
	// The call-time subprocess environment producer is the one coherent
	// projection of the current revision's names and the owner-wide held-name
	// union; the credential source filters the process environment.
	h, err := harness.New(work, harness.Dependencies{
		Storage: store,
		Jobs:    jobsService,
		Prepare: newPreparation(configService, options.Locations, options.Identity, background, adapter, artifacts, configService.subprocessEnvironment, options.NewEvaluator).bind(),
		Observe: adapter.observe,
	})
	if err != nil {
		return unwind(err)
	}
	// Armed before publication: harness.New performed no I/O and the first
	// Prepare requires the admission gate, so no Caller can observe the
	// unarmed bridge, data owner or the adapter's unbound Harness.
	background.h = h
	adapter.h = h
	c.hosts.harness = h

	r := &Melta{
		lock:          lock,
		work:          work,
		cancelWork:    cancelWork,
		config:        configService,
		obs:           obs,
		warnings:      warnings,
		passive:       adapter,
		agentInputs:   inputs,
		harness:       h,
		artifacts:     artifacts,
		dataDir:       dataDir,
		instance:      instance,
		beforeRelease: options.BeforeRelease,
		packages:      packages,
		jobs:          jobsService,
		closeStore:    closeStorage,
		shutdownDone:  make(chan struct{}),
	}
	c.hosts.owner = r
	sweepTicks, stopSweepTicker := options.sweepTicks, func() {}
	if sweepTicks == nil {
		ticker := time.NewTicker(sweepInterval)
		sweepTicks, stopSweepTicker = ticker.C, ticker.Stop
	}
	r.startMaintenance(sweepTicks, stopSweepTicker)
	return r.publishOwner(work)
}

// openStore is the owner's included storage construction: the SQLite database
// derived from the owned data directory, returned together with its closer.
// It checks the construction context before touching the filesystem and runs
// under the held ownership lock. Options.OpenStore replaces it; the global is
// the in-package fixture seam.
// jobsService is the directly constructed background process service: the
// standard tools' jobs contract, the Harness job-stop seam, and the shutdown
// closer. newJobs is its construction seam, which the in-package fixtures
// point at a scripted service; no composition declaration supplies it.
type jobsService interface {
	jobs.Jobs
	harness.JobStopper
	Close() error
}

var newJobs = func(dataDir string) jobsService { return jobs.New(dataDir) }

var openStore = func(ctx context.Context, dataDir string) (harness.Storage, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	store, err := storage.OpenSQLite(filepath.Join(dataDir, "lightcode.db"))
	if err != nil {
		return nil, nil, err
	}
	return store, store.Close, nil
}

// publishOwner registers the constructor context's cancellation watcher and
// checks the context once more before completed-owner publication. Observed
// cancellation returns (nil, context error) only after the shared cleanup has
// joined; once publication wins, the Melta is returned even if cancellation
// immediately starts its shutdown. The watcher requests the same asynchronous
// shutdown a Close starts and never waits inside its callback; it is not
// admitted work the cleanup joins.
func (r *Melta) publishOwner(work context.Context) (*Melta, error) {
	context.AfterFunc(work, r.beginShutdown)
	if err := work.Err(); err != nil {
		r.beginShutdown()
		<-r.shutdownDone
		return nil, err
	}
	return r, nil
}

// enter registers one admitted call under the short Melta mutex: explicit
// closure or a canceled Melta context rejects entry with ErrClosed,
// including before the cancellation watcher runs, and a canceled Caller
// context is rejected with its own error. The Caller executes outside the
// mutex and deregisters once on return.
func (r *Melta) enter(ctx context.Context) (func(), error) {
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
func (r *Melta) withHarness(ctx context.Context, fn func(context.Context, *harness.Harness) error) error {
	release, err := r.enter(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx, r.harness)
}

// createSessionRecord is the one root creation core: the existing empty
// workspace check wraps the shared harness.ErrInvalid sentinel, the Workspace
// is normalized with filepath.Abs alone, and the Harness retains its
// nonempty-only Agent-type selection. It executes inside the Caller's
// admission.
func (r *Melta) createSessionRecord(ctx context.Context, h *harness.Harness, workspace string, selection harness.AgentSelection) (harness.SessionRecord, error) {
	if workspace == "" {
		return harness.SessionRecord{}, fmt.Errorf("workspace must be non-empty: %w", harness.ErrInvalid)
	}
	normalized, err := filepath.Abs(workspace)
	if err != nil {
		return harness.SessionRecord{}, fmt.Errorf("normalize Workspace: %w", err)
	}
	return h.CreateSession(ctx, harness.CreateSessionRequest{Workspace: normalized, AgentSelection: selection})
}

// DeleteSession is the private canonical Session deletion: the whole body
// runs inside one admitted call, so shutdown joins the deletion and its
// cleanup together. The deletion takes the Session's artifact interval
// nonblockingly around its Harness transition and retains it through the
// warning and cleanup producer, so a held interval refuses the deletion before
// any transition. Only a committing deletion produces a result: after
// Harness.DeleteSession commits, the Session's warning groups are removed in
// one observation section with one runtime-scoped hint, then one producer
// removes the local tree and calls each once-returned external owner. An
// absent Session returns the Harness not-found unchanged, authorizing no
// cleanup. Any other Harness error (invalid input, corruption, revision
// races, a closed admission) returns as-is and authorizes no cleanup.
func (r *Melta) DeleteSession(ctx context.Context, c Caller, sessionID string) (protocol.DeletionResult, error) {
	var result protocol.DeletionResult
	err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		if err := r.authorize(c, ctx, h, CallerScope{Kind: CallerScopeSession, SessionID: sessionID}); err != nil {
			return err
		}
		releaseArtifacts, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, sessionID))
		if err != nil {
			return err
		}
		defer releaseArtifacts()
		owners, err := h.DeleteSession(ctx, sessionID)
		if err != nil {
			return err
		}
		r.passive.removeSessionWarnings([]string{sessionID})
		result.CleanupFailures = r.cleanupDeletedSessions(ctx, []deletedSession{{sessionID: sessionID, owners: owners}})
		return nil
	})
	if err != nil {
		return protocol.DeletionResult{}, err
	}
	return result, nil
}

// removeSessionCode removes one Session's artifact tree under the normalized
// data directory — its snapshots and command spills. It removes nothing else:
// not sibling Sessions' trees, not other data-directory content. A missing
// directory is already clean.
func (r *Melta) removeSessionCode(sessionID string) error {
	return os.RemoveAll(filepath.Join(r.dataDir, "code", sessionID))
}

// Reload publishes the next configuration revision through the admitted-call
// gate and returns its stamped revision. It is serialized by the configuration
// service's own build mutex; the gate joins it to shutdown and holds no
// Melta mutex across the build.
func (r *Melta) Reload(ctx context.Context, c Caller) (protocol.ConfigurationRevision, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ConfigurationRevision{}, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return protocol.ConfigurationRevision{}, err
	}
	snapshot, err := r.config.publish(ctx)
	if err != nil {
		return protocol.ConfigurationRevision{}, err
	}
	return configurationRevision(snapshot.snapshot, r.instance), nil
}

// Close starts (or joins) the one shared managed shutdown and waits for it.
// The argument bounds only this Caller's wait; every Close Caller joins the
// same result.
func (r *Melta) Close(ctx context.Context) error {
	r.beginShutdown()
	select {
	case <-r.shutdownDone:
		return r.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// beginShutdown first closes admission and cancels the owned work context,
// then starts the one shared cleanup asynchronously. Repeat callers only
// join.
func (r *Melta) beginShutdown() {
	r.shutdownOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		// The warning store's admission closes with the Melta: later
		// reports are ignored presentation. Short in-memory coordination
		// only — no network or plugin work runs here.
		r.warnings.close()
		r.cancelWork()
		go func() {
			r.shutdownErr = r.joinShutdown()
			close(r.shutdownDone)
		}()
	})
}

// joinShutdown converges the owner: it joins admitted Melta work, stops
// every background ownership group so live members deliver and finish while
// the capabilities and storage are still open, then joins the Harness after
// its context cancellation has settled every in-flight preparation,
// execution, and required terminal commit, then releases the published
// resource set and closes the jobs service — joining every monitor and
// completion callback — before invoking the host withdrawal callback, then
// closes the included store, closes every passive subscription once those
// cleanup events have been published, and releases the lock as the final
// ownership action. Every required close is attempted and all errors are
// joined; no state-owning mutex is held across any of it.
func (r *Melta) joinShutdown() error {
	var errs []error
	r.calls.Wait()
	if err := r.harness.StopAll(context.Background()); err != nil {
		errs = append(errs, err)
	}
	if err := r.harness.Wait(context.Background()); err != nil {
		errs = append(errs, err)
	}
	if current := r.config.current(); current != nil && current.resources != nil {
		_ = current.resources.release()
	}
	r.config.cleanupMu.Lock()
	errs = append(errs, r.config.cleanupErr)
	r.config.cleanupMu.Unlock()
	if err := r.jobs.Close(); err != nil {
		errs = append(errs, err)
	}
	if r.beforeRelease != nil {
		if err := r.beforeRelease(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.closeStore(); err != nil {
		errs = append(errs, err)
	}
	r.obs.closeAll()
	if err := r.lock.Release(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// InstanceID returns the owner's process-lifetime instance identity minted
// before publication.
func (r *Melta) InstanceID() string { return r.instance }
