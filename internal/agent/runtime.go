package agent

import (
	"context"
	"sync"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/tool"
)

type runtimeOptions struct {
	WorkspaceRoot    string
	PermissionPolicy runtimePermissionPolicy
}

type runtimePermissionPolicy struct {
	Check tool.CheckFunc
	Ask   tool.AskFunc
}

func (p runtimePermissionPolicy) checkFunc() tool.CheckFunc {
	if p.Check != nil {
		return p.Check
	}
	return func(string, string) permission.Decision {
		return permission.DecisionDeny
	}
}

func (p runtimePermissionPolicy) askFunc() tool.AskFunc {
	if p.Ask != nil {
		return p.Ask
	}
	return func(context.Context, permission.Request) permission.ResponseAction {
		return permission.ResponseDeny
	}
}

type runtime struct {
	agent *Agent

	workspaceRoot    string
	permissionPolicy runtimePermissionPolicy

	loopEvents   chan loop.Event
	taggedEvents chan TaggedLoopEvent
	loopFlush    chan chan struct{}
	signalWake   chan struct{}
	queueWake    chan struct{}
	signalSink   agentSignalSink

	// queueDrainPassHook is a test seam invoked after each queue drain pass
	// returns and before the drainer waits for its next wake, outside any
	// lock. The seam is nil in production; tests swap it to observe that a
	// pass completed — whether it took the claim or found the unit busy and
	// returned — which the wake token alone does not prove.
	queueDrainPassHook func()

	eventMu sync.RWMutex
	onEvent func(Event)

	initOnce sync.Once
	// resumedSessionID is the session Init resumed, or "" when none was
	// resumed. Init returns it so each adapter can adopt the resumed session as
	// its interface-local selection; sync.Once cannot carry a return value, so
	// the init body records it here and every caller of Do observes it.
	resumedSessionID string
	// lifecycleMu serializes identity-changing lifecycle operations
	// (open/resume/new/fork/archive/delete/history-revert and each sweep
	// candidate) so two cannot interleave preparation and publication. It is
	// the outermost owner lock, taken before mu, and is never held during turn
	// execution or acquired by a drainer/callback.
	lifecycleMu sync.Mutex
	mu          sync.Mutex

	// transcriptMu guards only the transcriptState map: insert, look up, and
	// delete an entry. It is taken briefly and released before the entry's own
	// seqMu, so it never serialises two sessions' transcript feeds on one lock —
	// each entry's coordinator keeps its per-session seqMu as the innermost lock
	// (transcript.go). It is a leaf lock: never held while acquiring any other.
	transcriptMu    sync.Mutex
	transcriptState map[string]*transcriptCursor

	// Owner shutdown. ownerCtx/ownerCancel bound the background goroutines and
	// the lifetime of accepted work; they are created independently of the host
	// context, which only triggers shutdown through the Init watcher. closed is
	// the turn-admission gate (guarded by mu); turnWG tracks in-flight turns
	// and mutations; bgWG tracks the background goroutines; shutdownOnce and
	// shutdownDone make ShutdownOwner one shared join for all callers.
	ownerCtx     context.Context
	ownerCancel  context.CancelFunc
	closed       bool
	turnWG       sync.WaitGroup
	bgWG         sync.WaitGroup
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	// shutdownClean records whether owner shutdown completed every join — both
	// the turn join and the background join drained. Only the caller that wins
	// shutdownOnce executes the shutdown body, so every other caller reads the
	// result here rather than receiving it from the Do; the close of
	// shutdownDone supplies the happens-before edge, so the field needs no
	// lock.
	shutdownClean bool
}

// transcriptCursor is one live session's registry entry: the session's
// transcript coordinator (the per-session {nextSeq, committedSeq,
// committedTurn} state, agents.md "Transcript hydration cursor") plus the
// store its durable history reads through. Keyed by session id in the
// runtime's transcriptState registry so root and child sessions resolve
// uniformly by id. The coordinator's own seqMu guards the coordinator state;
// transcriptMu guards only the map.
type transcriptCursor struct {
	coord *transcript
	store *snapshot.Store
	// childSignal is the optional child process-signal callback: set after a
	// child's loop is constructed (before the child can call a tool) so the
	// owner process manager's exit handler can deliver a child background-
	// process completion to the child loop, and removed with the entry by
	// unregisterTranscript. It is looked up under transcriptMu only, never
	// under rt.mu.
	childSignal func(loop.PendingSignal)
}

// registerTranscript inserts a fresh coordinator entry for id if none exists.
// It is called when a session becomes live and is idempotent so re-registering
// a still-live session keeps its coordinator (and its sequenced rows). A fresh
// coordinator adopts the store's current complete turn as its committed bound —
// a loaded store names the highest marker-backed complete turn, a fresh store
// zero. The bound is read before transcriptMu is taken, so registration never
// reads store state while holding transcript locks; the existing-entry check
// and insert run under the lock, so a concurrently created coordinator is
// preserved. Every production caller first-registers outside rt.mu — the
// locked live registration only verifies the pre-registered entry — so the
// store read never runs under rt.mu.
func (rt *runtime) registerTranscript(id string, store *snapshot.Store) {
	if id == "" {
		return
	}
	var initialCommitted int
	if store != nil {
		initialCommitted = store.HighestCompleteTurn()
	}
	rt.transcriptMu.Lock()
	if _, ok := rt.transcriptState[id]; !ok {
		coord := newTranscript()
		coord.committedTurn = initialCommitted
		rt.transcriptState[id] = &transcriptCursor{coord: coord, store: store}
	}
	rt.transcriptMu.Unlock()
}

// unregisterTranscript drops the entry for id. It is called when a session
// stops being live; a feed resolving after the drop lands nowhere, matching
// the pre-registry window where a unit left the live map mid-feed. Dropping
// the entry also removes any child process-signal callback registered on it.
func (rt *runtime) unregisterTranscript(id string) {
	if id == "" {
		return
	}
	rt.transcriptMu.Lock()
	delete(rt.transcriptState, id)
	rt.transcriptMu.Unlock()
}

// setChildSignalCallback attaches the child process-signal callback to the
// transcript entry for id. It is set after the child's loop is constructed —
// before lp.Run can call a tool — and is removed with the entry by
// unregisterTranscript. The caller holds no rt.mu.
func (rt *runtime) setChildSignalCallback(id string, cb func(loop.PendingSignal)) {
	if id == "" || cb == nil {
		return
	}
	rt.transcriptMu.Lock()
	if e := rt.transcriptState[id]; e != nil {
		e.childSignal = cb
	}
	rt.transcriptMu.Unlock()
}

// childSignalForSessionID returns the child process-signal callback registered
// for id, or nil. It takes only transcriptMu and never rt.mu, so the owner
// process manager's exit handler can look a child completion up without
// holding an owner lock.
func (rt *runtime) childSignalForSessionID(id string) func(loop.PendingSignal) {
	if id == "" {
		return nil
	}
	rt.transcriptMu.Lock()
	var cb func(loop.PendingSignal)
	if e := rt.transcriptState[id]; e != nil {
		cb = e.childSignal
	}
	rt.transcriptMu.Unlock()
	return cb
}

// transcriptForSessionID resolves the live coordinator owning a session id, or
// nil for an unknown or sessionless id. It takes transcriptMu only for the map
// lookup and releases it before the caller takes the entry's seqMu.
func (rt *runtime) transcriptForSessionID(id string) *transcript {
	if id == "" {
		return nil
	}
	rt.transcriptMu.Lock()
	e := rt.transcriptState[id]
	rt.transcriptMu.Unlock()
	if e == nil {
		return nil
	}
	return e.coord
}

// ownerCtxDone returns the owner context's done channel, or nil when the owner
// context is not set (a bare newRuntime in a test that never saw Init). A nil
// channel blocks a select forever, which is the correct "no owner context yet"
// behaviour.
func (rt *runtime) ownerCtxDone() <-chan struct{} {
	if rt == nil || rt.ownerCtx == nil {
		return nil
	}
	return rt.ownerCtx.Done()
}

// workCtx returns the context accepted work derives from: the owner context,
// or a fresh background context when the owner was never initialised (a bare
// runtime in a test that never saw Init). Once the owner accepts work, that
// work's lifetime is the owner's; a host context may trigger shutdown but
// never severs accepted work. No call site may substitute the caller's
// context here.
func (rt *runtime) workCtx() context.Context {
	if rt.ownerCtx != nil {
		return rt.ownerCtx
	}
	return context.Background()
}

// ownerShuttingDown reports whether the owner context is done. It is the
// post-admission lifetime check: once work is accepted, only the owner's
// lifetime — or an explicit per-session cancel, which cancels the turn's own
// context directly — may end it; a cancelled caller context cannot.
func (rt *runtime) ownerShuttingDown() bool {
	done := rt.ownerCtxDone()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// touchProjectActivityBeforeRun admits the best-effort project publication
// separately from the session turn. The lifecycle lock makes the closed check
// and the one-attempt filesystem operation one shutdown boundary, while the
// runtime mutex is released before any filesystem work.
func (rt *runtime) touchProjectActivityBeforeRun(store *snapshot.Store) {
	if rt == nil || store == nil {
		return
	}
	rt.lifecycleMu.Lock()
	rt.mu.Lock()
	closed := rt.closed
	rt.mu.Unlock()
	if closed {
		rt.lifecycleMu.Unlock()
		return
	}
	_, _ = store.TryTouchProjectActivity()
	rt.lifecycleMu.Unlock()
}

// flushAndCommitTranscript waits for the loop event drainer to dispatch every
// queued event, then commits the session's turn. The round-trip is required,
// not defensive: nextSeq is assigned inside dispatchLoopEvent and
// dispatchTaggedEvent on the single drainLoopEvents goroutine, so a producer
// finishing proves nothing about dispatch. The wait gives up on the owner
// context — cancelled only by owner shutdown — so a shutting-down owner cannot
// block the caller forever. It deliberately takes no caller context, so no call
// site can reintroduce a turn-cancellable context that would race the flush and
// commit an understated cursor.
func (rt *runtime) flushAndCommitTranscript(sessionID string, turn int) {
	if rt == nil {
		return
	}
	// Turn numbers begin at 1, so a call naming turn 0 names a turn that was
	// never begun — BeginTurn returns 0 only for an inactive session. Feeding
	// EventTurnEnd{Turn: 0} would drive commitLocked(0), wiping the tail and
	// regressing the committed markers. Return before the flush and before
	// feeding EventTurnEnd: the flush exists only to make the commit correct.
	if turn == 0 {
		return
	}
	done := make(chan struct{})
	select {
	case rt.loopFlush <- done:
	case <-rt.ownerCtxDone():
		return
	}
	select {
	case <-done:
	case <-rt.ownerCtxDone():
		return
	}
	feedTranscript(rt.transcriptForSessionID(sessionID), Event{Kind: EventTurnEnd, SessionID: sessionID, Turn: turn})
}

func newRuntime(a *Agent, opts runtimeOptions) *runtime {
	rt := &runtime{
		agent:            a,
		workspaceRoot:    opts.WorkspaceRoot,
		permissionPolicy: opts.PermissionPolicy,
		loopEvents:       make(chan loop.Event, 256),
		loopFlush:        make(chan chan struct{}, 1),
		signalWake:       make(chan struct{}, 1),
		queueWake:        make(chan struct{}, 1),
		transcriptState:  make(map[string]*transcriptCursor),
		shutdownDone:     make(chan struct{}),
	}
	rt.signalSink = loopSignalSink{agent: a}
	return rt
}

func (a *Agent) ensureRuntime() *runtime {
	if a.rt == nil {
		if a.session != nil && a.session.rt != nil {
			a.rt = a.session.rt
		} else {
			a.rt = newRuntime(a, runtimeOptions{})
		}
		if a.session == nil {
			a.session = &session{rt: a.rt}
		}
	}
	if a.rt.agent == nil {
		a.rt.agent = a
	}
	if a.rt.loopEvents == nil {
		a.rt.loopEvents = make(chan loop.Event, 256)
	}
	if a.rt.loopFlush == nil {
		a.rt.loopFlush = make(chan chan struct{}, 1)
	}
	if a.rt.signalWake == nil {
		a.rt.signalWake = make(chan struct{}, 1)
	}
	if a.rt.queueWake == nil {
		a.rt.queueWake = make(chan struct{}, 1)
	}
	if a.rt.transcriptState == nil {
		a.rt.transcriptState = make(map[string]*transcriptCursor)
	}
	if a.rt.shutdownDone == nil {
		a.rt.shutdownDone = make(chan struct{})
	}
	if a.rt.signalSink == nil {
		a.rt.signalSink = loopSignalSink{agent: a}
	}
	return a.rt
}

func (rt *runtime) sessionLocked() *session {
	if rt == nil || rt.agent == nil {
		return nil
	}
	if rt.agent.session == nil {
		rt.agent.session = &session{}
	}
	return rt.agent.session
}
