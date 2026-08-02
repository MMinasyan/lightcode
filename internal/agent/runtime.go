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
	Check     tool.CheckFunc
	Ask       tool.AskFunc
	AskAction tool.AskActionFunc
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

func (p runtimePermissionPolicy) askActionFunc() tool.AskActionFunc {
	if p.AskAction != nil {
		return p.AskAction
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

	eventMu sync.RWMutex
	onEvent func(Event)

	initOnce sync.Once
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

	// Owner shutdown. ownerCtx/ownerCancel bound the background goroutines;
	// closed is the turn-admission gate (guarded by mu); turnWG tracks in-flight
	// turns and mutations; bgWG tracks the background goroutines; shutdownOnce and
	// shutdownDone make ShutdownOwner one shared join for all callers.
	ownerCtx     context.Context
	ownerCancel  context.CancelFunc
	closed       bool
	turnWG       sync.WaitGroup
	bgWG         sync.WaitGroup
	shutdownOnce sync.Once
	shutdownDone chan struct{}
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
}

// registerTranscript inserts a fresh coordinator entry for id if none exists.
// It is called when a session becomes live and is idempotent so re-registering
// a still-live session keeps its coordinator (and its sequenced rows). The
// caller may hold rt.mu; this takes only transcriptMu.
func (rt *runtime) registerTranscript(id string, store *snapshot.Store) {
	if id == "" {
		return
	}
	rt.transcriptMu.Lock()
	if _, ok := rt.transcriptState[id]; !ok {
		rt.transcriptState[id] = &transcriptCursor{coord: newTranscript(), store: store}
	}
	rt.transcriptMu.Unlock()
}

// unregisterTranscript drops the entry for id. It is called when a session
// stops being live; a feed resolving after the drop lands nowhere, matching
// the pre-registry window where a unit left the live map mid-feed.
func (rt *runtime) unregisterTranscript(id string) {
	if id == "" {
		return
	}
	rt.transcriptMu.Lock()
	delete(rt.transcriptState, id)
	rt.transcriptMu.Unlock()
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
