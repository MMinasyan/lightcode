package agent

import (
	"context"
	"sync"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/permission"
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

	eventMu             sync.RWMutex
	onEvent             func(Event)
	eventSubscribers    map[int]func(Event)
	nextEventSubscriber int

	mu sync.Mutex
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

func (rt *runtime) session() *session {
	return rt.sessionLocked()
}
