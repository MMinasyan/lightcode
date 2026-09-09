package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"sync"

	"github.com/MMinasyan/lightcode/harness"
)

// Error identities reported by typed composition and scope lifetime.
var (
	// ErrComposition wraps invalid declarations, missing or wrongly typed
	// bindings, invalid instance values, and failed construction.
	ErrComposition = errors.New("runtime: composition failure")

	// ErrClosed reports a closed or owner-canceled scope or owner: no guard
	// can enter and no binding resolves from an unavailable supplying scope.
	ErrClosed = errors.New("runtime: closed")
)

// ScopeKind names one lifetime tier of the nested scope hierarchy.
type ScopeKind string

const (
	ScopeRuntime   ScopeKind = "runtime"
	ScopeWorkspace ScopeKind = "workspace"
	ScopeOperation ScopeKind = "operation"
	ScopeAgent     ScopeKind = "agent"
)

// rank orders scopes from longer- to shorter-lived. A dependency may only be
// retained from the same or a longer-lived scope, so a consumer's rank must
// never be below its provider's. An unknown kind ranks zero and is invalid.
func (k ScopeKind) rank() int {
	switch k {
	case ScopeRuntime:
		return 1
	case ScopeWorkspace:
		return 2
	case ScopeOperation:
		return 3
	case ScopeAgent:
		return 4
	}
	return 0
}

// ScopeInfo is the immutable identity handed to one factory. DataDir is the
// same normalized owner root in every ScopeInfo. Workspace scopes carry only
// lexical Workspace identity, never Session or Operation attribution; the
// Runtime scope carries no narrower attribution at all; short scopes carry
// the complete Workspace/Session/Operation identity.
type ScopeInfo struct {
	Kind        ScopeKind
	DataDir     string
	Workspace   string
	SessionID   string
	OperationID string
}

// CapabilitySpec pairs one capability lookup ID with its Go type. The ID is
// the only lookup key; type metadata never leaves the declaration.
type CapabilitySpec struct {
	id  string
	typ reflect.Type
}

// Spec declares the capability contract named id for the Go type T. It serves
// both Plugin.Provides and Plugin.Requires.
func Spec[T any](id string) CapabilitySpec {
	return CapabilitySpec{id: id, typ: reflect.TypeFor[T]()}
}

// Instance is one plugin's constructed output: Values must hold exactly the
// plugin's declared capability IDs, each carrying a value assignable to its
// declaration, and Close is optional when no external disposal is needed.
// Close runs after the scope context is canceled and must stop and join any
// internal service work; it disposes resources, never new Core work.
type Instance struct {
	Values map[string]any
	Close  func() error
}

// Plugin is one statically registered composition unit. Open receives the
// scope context, the scope identity, and immutable Bindings of exactly its
// declared dependencies, already resolved from the same or an ancestor
// scope; it wires native dependencies and stable scope resources without
// configuration. ValidateConfig is synchronous, pure, terminating validation
// of its owned input: no I/O, background work, waits, or retained input. A
// nil ValidateConfig means the plugin has no settings, so only absent
// configuration or the empty object is accepted, never arbitrary ignored
// JSON. An export declared with exactly the harness.Storage type is the
// private Core storage binding, identified by type rather than a reserved
// ID; it stays in the same declaration, instance, and disposal machinery but
// is omitted from ordinary Bindings and the capability universe.
type Plugin struct {
	ID             string
	Scope          ScopeKind
	Provides       []CapabilitySpec
	Requires       []CapabilitySpec
	ValidateConfig func(json.RawMessage) error
	Open           func(context.Context, ScopeInfo, Bindings) (Instance, error)
}

// Bindings is an immutable resolved dependency set; the zero value is empty.
// Each entry carries its declared type, the native value, and the supplying
// scope. Copying a Bindings copies the map, never the shared implementations.
type Bindings struct {
	entries map[string]bindingEntry
}

type bindingEntry struct {
	declared reflect.Type
	value    any
	from     *scope
}

// Bind returns the native value bound to id, checking that it is assignable
// to T. A missing or wrongly typed entry reports ErrComposition; a supplying
// scope that has closed or whose context the owner canceled reports
// ErrClosed.
func Bind[T any](bindings Bindings, id string) (T, error) {
	var zero T
	entry, ok := bindings.entries[id]
	if !ok {
		return zero, fmt.Errorf("capability %q is not a declared dependency: %w", id, ErrComposition)
	}
	if entry.from.unavailable() {
		return zero, fmt.Errorf("capability %q: %w", id, ErrClosed)
	}
	target := reflect.TypeFor[T]()
	if entry.value == nil {
		return zero, fmt.Errorf("capability %q supplies no verifiable type: %w", id, ErrComposition)
	}
	if !reflect.TypeOf(entry.value).AssignableTo(target) {
		return zero, fmt.Errorf("capability %q supplies %s, not assignable to %s: %w", id, reflect.TypeOf(entry.value), target, ErrComposition)
	}
	return entry.value.(T), nil
}

// Invocation is immutable configuration data for one Core call tree, never a
// lifetime or capability handle. It holds one captured snapshot; a plugin
// knows its own registered ID and reads its settings from this explicit
// argument, and native dependency calls receive the same Invocation so a
// dependency reads its own settings from the revision its caller used. The
// zero Invocation returns empty values.
type Invocation struct {
	snapshot *configuration
}

// Revision returns the captured snapshot's publication generation as an
// unsigned decimal string, or empty for the zero Invocation.
func (in Invocation) Revision() string {
	if in.snapshot == nil {
		return ""
	}
	return strconv.FormatUint(in.snapshot.generation, 10)
}

// Config returns the captured snapshot's owned JSON for pluginID, or nil when
// the plugin has no configuration in this revision.
func (in Invocation) Config(pluginID string) json.RawMessage {
	if in.snapshot == nil {
		return nil
	}
	raw, ok := in.snapshot.plugins[pluginID]
	if !ok {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// acceptSettings applies one declaration's configuration rule: a plugin with
// a nil ValidateConfig accepts only absent configuration or the empty
// object, and any other input document is rejected instead of silently
// ignored. A non-nil validator receives an independent clone of the input, so
// its in-place scratch never reaches the caller's retained bytes.
func acceptSettings(plugin Plugin, raw json.RawMessage) error {
	if plugin.ValidateConfig != nil {
		return plugin.ValidateConfig(bytes.Clone(raw))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err == nil {
		if object, isObject := probe.(map[string]any); isObject && len(object) == 0 {
			return nil
		}
	}
	return fmt.Errorf("plugin %q declares no settings validator but received configuration: %w", plugin.ID, ErrComposition)
}

// coreExportDecl records one export declared as exactly harness.Storage so
// the Runtime can later identify the private Core storage binding by type.
type coreExportDecl struct {
	plugin string
	scope  ScopeKind
	id     string
}

// composition is the validated static plugin set: an owned declaration copy,
// per-scope construction plans in stable topological order (registration
// order breaks ties), the ordinary capability universe excluding Core
// storage exports, and the storage export declarations.
type composition struct {
	plugins       []Plugin
	plan          map[ScopeKind][]Plugin
	capabilityIDs []string
	coreExports   []coreExportDecl
}

// newComposition validates the complete selected plugin set before any
// factory can run: unique nonempty plugin IDs, valid scopes, non-nil Open,
// at least one provider, unique nonempty export IDs globally and unique
// nonempty dependency IDs per plugin, assignable provider types,
// lifetime-compatible PreparationHook declarations (Runtime or Workspace
// scope only), same-or-longer-lived dependency scopes, and an acyclic
// dependency graph. An ordinary dependency on an ID reserved by a Core
// storage export is a plain missing binding and is rejected.
func newComposition(plugins []Plugin) (*composition, error) {
	owned := make([]Plugin, len(plugins))
	for i, p := range plugins {
		p.Provides = append([]CapabilitySpec(nil), p.Provides...)
		p.Requires = append([]CapabilitySpec(nil), p.Requires...)
		owned[i] = p
	}

	type exportSource struct {
		plugin int
		spec   CapabilitySpec
	}
	storageType := reflect.TypeFor[harness.Storage]()
	all := make(map[string]exportSource)
	ordinary := make(map[string]exportSource)
	seenPluginIDs := make(map[string]bool)
	var capabilityIDs []string
	var coreExports []coreExportDecl
	for i, p := range owned {
		if p.ID == "" {
			return nil, fmt.Errorf("plugin %d: empty plugin ID: %w", i, ErrComposition)
		}
		if seenPluginIDs[p.ID] {
			return nil, fmt.Errorf("plugin %q: duplicate plugin ID: %w", p.ID, ErrComposition)
		}
		seenPluginIDs[p.ID] = true
		if p.Scope.rank() == 0 {
			return nil, fmt.Errorf("plugin %q: invalid scope %q: %w", p.ID, p.Scope, ErrComposition)
		}
		if p.Open == nil {
			return nil, fmt.Errorf("plugin %q: missing Open: %w", p.ID, ErrComposition)
		}
		if len(p.Provides) == 0 {
			return nil, fmt.Errorf("plugin %q: missing providers: %w", p.ID, ErrComposition)
		}
		seenRequires := make(map[string]bool, len(p.Requires))
		for _, req := range p.Requires {
			if req.id == "" {
				return nil, fmt.Errorf("plugin %q: empty required capability ID: %w", p.ID, ErrComposition)
			}
			if seenRequires[req.id] {
				return nil, fmt.Errorf("plugin %q: duplicate required capability %q: %w", p.ID, req.id, ErrComposition)
			}
			seenRequires[req.id] = true
		}
		for _, prov := range p.Provides {
			if prov.id == "" {
				return nil, fmt.Errorf("plugin %q: empty provided capability ID: %w", p.ID, ErrComposition)
			}
			if src, dup := all[prov.id]; dup {
				return nil, fmt.Errorf("plugin %q: capability %q already exported by %q: %w", p.ID, prov.id, owned[src.plugin].ID, ErrComposition)
			}
			all[prov.id] = exportSource{plugin: i, spec: prov}
			if prov.typ.Implements(preparationHookType) && p.Scope != ScopeRuntime && p.Scope != ScopeWorkspace {
				return nil, fmt.Errorf("plugin %q (%s): capability %q declared as %s implements PreparationHook and requires Runtime or Workspace scope: %w", p.ID, p.Scope, prov.id, prov.typ, ErrComposition)
			}
			if prov.typ == storageType {
				coreExports = append(coreExports, coreExportDecl{plugin: p.ID, scope: p.Scope, id: prov.id})
			} else {
				ordinary[prov.id] = exportSource{plugin: i, spec: prov}
				capabilityIDs = append(capabilityIDs, prov.id)
			}
		}
	}
	for _, p := range owned {
		for _, req := range p.Requires {
			src, ok := ordinary[req.id]
			if !ok {
				return nil, fmt.Errorf("plugin %q: required capability %q has no ordinary binding: %w", p.ID, req.id, ErrComposition)
			}
			if !src.spec.typ.AssignableTo(req.typ) {
				return nil, fmt.Errorf("plugin %q: capability %q declared as %s is not assignable to required %s: %w", p.ID, req.id, src.spec.typ, req.typ, ErrComposition)
			}
			provider := owned[src.plugin]
			if provider.Scope.rank() > p.Scope.rank() {
				return nil, fmt.Errorf("plugin %q (%s) requires %q from shorter-lived %s plugin %q: %w", p.ID, p.Scope, req.id, provider.Scope, provider.ID, ErrComposition)
			}
		}
	}

	// Stable topological order over the whole plugin set: dependencies are
	// emitted before consumers and ties keep registration order. Edges only
	// point to same-or-longer-lived scopes, so only same-scope cycles are
	// possible; any cycle is rejected before factories run.
	consumedBy := make([][]int, len(owned))
	indegree := make([]int, len(owned))
	edges := make([]map[int]bool, len(owned))
	for i := range edges {
		edges[i] = make(map[int]bool)
	}
	for i, p := range owned {
		for _, req := range p.Requires {
			src := ordinary[req.id]
			if edges[i][src.plugin] {
				continue
			}
			edges[i][src.plugin] = true
			consumedBy[src.plugin] = append(consumedBy[src.plugin], i)
			indegree[i]++
		}
	}
	ordered := make([]int, 0, len(owned))
	emitted := make([]bool, len(owned))
	for len(ordered) < len(owned) {
		next := -1
		for i := range owned {
			if !emitted[i] && indegree[i] == 0 {
				next = i
				break
			}
		}
		if next < 0 {
			return nil, fmt.Errorf("dependency cycle among plugins: %w", ErrComposition)
		}
		emitted[next] = true
		ordered = append(ordered, next)
		for _, consumer := range consumedBy[next] {
			indegree[consumer]--
		}
	}

	plan := make(map[ScopeKind][]Plugin)
	for _, i := range ordered {
		p := owned[i]
		plan[p.Scope] = append(plan[p.Scope], p)
	}
	return &composition{
		plugins:       owned,
		plan:          plan,
		capabilityIDs: capabilityIDs,
		coreExports:   coreExports,
	}, nil
}

// validateScopeInfo enforces the per-kind attribution shape: the Runtime
// scope carries only the owner root, the Workspace scope carries the root and
// the lexical Workspace alone, and short scopes carry the complete
// Workspace/Session/Operation identity.
func validateScopeInfo(info ScopeInfo) error {
	if info.DataDir == "" {
		return fmt.Errorf("scope %s: missing data directory: %w", info.Kind, ErrComposition)
	}
	switch info.Kind {
	case ScopeRuntime:
		if info.Workspace != "" || info.SessionID != "" || info.OperationID != "" {
			return fmt.Errorf("runtime scope carries narrower attribution %q/%q/%q: %w", info.Workspace, info.SessionID, info.OperationID, ErrComposition)
		}
	case ScopeWorkspace:
		if info.Workspace == "" || info.SessionID != "" || info.OperationID != "" {
			return fmt.Errorf("workspace scope requires only the Workspace identity, got %q/%q/%q: %w", info.Workspace, info.SessionID, info.OperationID, ErrComposition)
		}
	case ScopeOperation, ScopeAgent:
		if info.Workspace == "" || info.SessionID == "" || info.OperationID == "" {
			return fmt.Errorf("scope %s requires Workspace, Session, and Operation identity: %w", info.Kind, ErrComposition)
		}
	default:
		return fmt.Errorf("invalid scope kind %q: %w", info.Kind, ErrComposition)
	}
	return nil
}

// openScope constructs the plugins planned for info.Kind in topological
// order, seeding ordinary dependencies from the supplied ancestor scopes in
// longer-to-shorter order. The failing factory owns its provisional
// resources; every other failure runs the common scope cleanup — cancel, join
// admitted work, dispose constructed instances in reverse construction order
// attempting all and joining errors — and publishes nothing.
func (c *composition) openScope(ctx context.Context, info ScopeInfo, ancestors []*scope) (*scope, error) {
	if err := validateScopeInfo(info); err != nil {
		return nil, err
	}
	scopeCtx, cancel := context.WithCancel(ctx)
	sc := &scope{
		info:     info,
		ctx:      scopeCtx,
		cancel:   cancel,
		storages: make(map[string]any),
	}
	if len(ancestors) > 0 {
		sc.obs = ancestors[0].obs
	}
	resolved := make(map[string]bindingEntry)
	for _, ancestor := range ancestors {
		for id, entry := range ancestor.bindings.entries {
			resolved[id] = entry
		}
	}
	own := make(map[string]bindingEntry)
	fail := func(err error) (*scope, error) {
		return nil, errors.Join(err, sc.cleanup())
	}
	for _, p := range c.plan[info.Kind] {
		if err := scopeCtx.Err(); err != nil {
			return fail(err)
		}
		deps := make(map[string]bindingEntry, len(p.Requires))
		for _, req := range p.Requires {
			source, ok := resolved[req.id]
			if !ok {
				return fail(fmt.Errorf("plugin %q: dependency %q unresolved in scope %s: %w", p.ID, req.id, info.Kind, ErrComposition))
			}
			deps[req.id] = bindingEntry{declared: req.typ, value: source.value, from: source.from}
		}
		instance, err := p.Open(scopeCtx, info, Bindings{entries: deps})
		if err != nil {
			return fail(fmt.Errorf("plugin %q: open: %w", p.ID, err))
		}
		if instance.Close != nil {
			sc.closers = append(sc.closers, instance.Close)
		}
		if err := registerInstance(p, instance.Values, sc, resolved, own); err != nil {
			return fail(fmt.Errorf("plugin %q: %w", p.ID, err))
		}
	}
	commit := func() { sc.bindings = Bindings{entries: own} }
	if sc.obs != nil && info.Kind != ScopeWorkspace {
		// Construction completion is this scope's publication; a Workspace
		// scope publishes later, at the registry commit in build.
		sc.obs.publish(commit, scopeEvent(EventScopeOpened, info))
	} else {
		commit()
	}
	return sc, nil
}

// registerInstance validates one factory output — exactly the declared IDs,
// each with a dynamic Go type assignable to its declaration, untyped nil
// supplying no verifiable type — and copies the returned map into the scope's
// resolved ordinary bindings and Core storage values. Typed nils and zero
// concrete values follow the ordinary assignability rule.
func registerInstance(p Plugin, values map[string]any, sc *scope, resolved, own map[string]bindingEntry) error {
	declared := make(map[string]CapabilitySpec, len(p.Provides))
	for _, spec := range p.Provides {
		declared[spec.id] = spec
	}
	for id := range values {
		if _, ok := declared[id]; !ok {
			return fmt.Errorf("instance exports undeclared capability %q: %w", id, ErrComposition)
		}
	}
	storageType := reflect.TypeFor[harness.Storage]()
	for id, spec := range declared {
		value, ok := values[id]
		if !ok {
			return fmt.Errorf("instance is missing declared capability %q: %w", id, ErrComposition)
		}
		if value == nil {
			return fmt.Errorf("capability %q supplies an untyped nil with no verifiable type: %w", id, ErrComposition)
		}
		if !reflect.TypeOf(value).AssignableTo(spec.typ) {
			return fmt.Errorf("capability %q supplied as %T is not assignable to declared %s: %w", id, value, spec.typ, ErrComposition)
		}
		if spec.typ == storageType {
			sc.storages[id] = value
		} else {
			entry := bindingEntry{declared: spec.typ, value: value, from: sc}
			own[id] = entry
			resolved[id] = entry
		}
	}
	return nil
}

// selectCapabilities builds an Agent's selected view from the same resolved
// scope instances, containing only the selected ordinary exports. Unknown or
// Core-storage IDs are not in the capability universe and are rejected; the
// selected view never rebinds.
func selectCapabilities(scopes []*scope, selected []string) (Bindings, error) {
	entries := make(map[string]bindingEntry, len(selected))
	for _, id := range selected {
		found := false
		for _, sc := range scopes {
			if entry, ok := sc.bindings.entries[id]; ok {
				entries[id] = entry
				found = true
				break
			}
		}
		if !found {
			return Bindings{}, fmt.Errorf("capability %q is not an ordinary export of the supplied scopes: %w", id, ErrComposition)
		}
	}
	return Bindings{entries: entries}, nil
}

// scope is one constructed instance set with its guard state. The complete
// Core call tree runs under exactly one guard: work is registered under the
// short state mutex, executed outside every lock on a context canceled by
// either the scope or the caller, and deregistered once on return. Closing
// first commits closed admission — publishing its scope event through the
// Runtime observation when one is attached — and cancels the scope context,
// then joins admitted work and disposes instances in reverse construction
// order.
type scope struct {
	info   ScopeInfo
	ctx    context.Context
	cancel context.CancelFunc

	// obs is the Runtime's passive publisher inherited from the first
	// ancestor; scopes constructed before the owner exists keep it nil.
	obs *observation

	bindings Bindings
	storages map[string]any
	closers  []func() error

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func (s *scope) unavailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unavailableLocked()
}

// unavailableLocked applies the one scope-admission rule under the scope
// mutex: the scope is unavailable once closure has begun or its own context
// has been canceled by the owner. Work admitted before a later cancellation
// remains admitted; its derived context is canceled and release deregisters
// it as usual.
func (s *scope) unavailableLocked() bool {
	return s.closed || s.ctx.Err() != nil
}

// enter admits one call against the caller's context, returning the derived
// call context and its once-only release. A closed or owner-canceled scope
// rejects with ErrClosed and a canceled caller context is rejected with its
// own error before work is registered.
func (s *scope) enter(ctx context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	if s.unavailableLocked() {
		s.mu.Unlock()
		return nil, nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	s.wg.Add(1)
	s.mu.Unlock()
	callCtx, cancelCall := context.WithCancel(s.ctx)
	stopCallerWatch := context.AfterFunc(ctx, cancelCall)
	var once sync.Once
	return callCtx, func() {
		once.Do(func() {
			stopCallerWatch()
			cancelCall()
			s.wg.Done()
		})
	}, nil
}

// close is called once by the scope owner. It closes admission before
// cancellation, then joins admitted work and disposes instances synchronously.
func (s *scope) close() error {
	commit := func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	}
	if s.obs != nil {
		s.obs.publish(commit, scopeEvent(EventScopeClosed, s.info))
	} else {
		commit()
	}
	return s.cleanup()
}

// cleanup is the common scope cleanup used by both construction rollback and
// closure: join admitted work even without external cleanup, then dispose.
func (s *scope) cleanup() error {
	s.cancel()
	s.wg.Wait()
	return s.dispose()
}

// dispose attempts every non-nil closer in reverse construction order and
// joins all errors.
func (s *scope) dispose() error {
	var errs []error
	for i := len(s.closers) - 1; i >= 0; i-- {
		if err := s.closers[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// workspaceAttempt is one shared Workspace scope construction attempt. The
// attempt belongs to the Runtime, not its initiating caller: construction
// runs on the Runtime's owned context with every same-key caller joined to
// one result, a canceled wait never cancels the attempt, failed attempts
// publish nothing, and only a later independent request may try again.
type workspaceAttempt struct {
	done chan struct{}
	sc   *scope
	err  error
}

// workspaceScopes is the Runtime-owned registry of shared Workspace scopes.
// Same-key callers join one construction; other keys proceed independently.
// Successful Workspaces live until Runtime shutdown, not Session deletion.
// A successful publication happens inside one observation section with the
// registry insertion, so subscribers and joined waiters see one commit order.
type workspaceScopes struct {
	owner     context.Context
	c         *composition
	ancestors []*scope
	obs       *observation

	mu       sync.Mutex
	closed   bool
	live     map[string]*scope
	attempts map[string]*workspaceAttempt
	building sync.WaitGroup
}

func newWorkspaceScopes(owner context.Context, c *composition, ancestors []*scope, obs *observation) *workspaceScopes {
	return &workspaceScopes{
		owner:     owner,
		c:         c,
		ancestors: ancestors,
		obs:       obs,
		live:      make(map[string]*scope),
		attempts:  make(map[string]*workspaceAttempt),
	}
}

// get returns the Workspace scope for info's lexical identity, joining one
// shared attempt on first construction. The caller context bounds only its
// own wait.
func (w *workspaceScopes) get(ctx context.Context, info ScopeInfo) (*scope, error) {
	if info.Kind != ScopeWorkspace {
		return nil, fmt.Errorf("workspace scope requested with kind %q: %w", info.Kind, ErrComposition)
	}
	key := info.Workspace
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil, ErrClosed
	}
	if sc := w.live[key]; sc != nil {
		w.mu.Unlock()
		return sc, nil
	}
	attempt := w.attempts[key]
	if attempt == nil {
		attempt = &workspaceAttempt{done: make(chan struct{})}
		w.attempts[key] = attempt
		w.building.Add(1)
		go w.build(key, info, attempt)
	}
	w.mu.Unlock()
	select {
	case <-attempt.done:
		return attempt.sc, attempt.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// build runs one construction attempt as Runtime-owned work and publishes the
// complete scope or the shared failure without holding the registry lock
// across factories. A successful publication takes the observation mutex
// before the registry lock and enqueues the scope_opened event in that same
// section, so publication and enqueue share one order; a failed attempt
// publishes nothing.
func (w *workspaceScopes) build(key string, info ScopeInfo, attempt *workspaceAttempt) {
	defer w.building.Done()
	sc, err := w.c.openScope(w.owner, info, w.ancestors)
	if err != nil {
		w.mu.Lock()
		delete(w.attempts, key)
		w.mu.Unlock()
		attempt.sc, attempt.err = sc, err
		close(attempt.done)
		return
	}
	w.obs.publish(func() {
		sc.obs = w.obs
		w.mu.Lock()
		delete(w.attempts, key)
		w.live[key] = sc
		w.mu.Unlock()
		attempt.sc, attempt.err = sc, nil
		close(attempt.done)
	}, scopeEvent(EventScopeOpened, info))
}

// shutdown is called once by the Runtime owner. It closes registry admission,
// joins construction, and closes live Workspace scopes in sorted path order.
func (w *workspaceScopes) shutdown() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.building.Wait()
	w.mu.Lock()
	keys := make([]string, 0, len(w.live))
	for key := range w.live {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	live := make([]*scope, len(keys))
	for i, key := range keys {
		live[i] = w.live[key]
	}
	w.mu.Unlock()
	var errs []error
	for _, sc := range live {
		errs = append(errs, sc.close())
	}
	return errors.Join(errs...)
}
