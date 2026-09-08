package runtime

import (
	"context"
	"errors"
	"sync"
)

// EventKind classifies the bounded passive notifications the Runtime
// publishes after committed state transitions.
type EventKind string

const (
	// EventConfiguration reports one published configuration generation.
	EventConfiguration EventKind = "configuration"
	// EventScopeOpened reports a scope committed to its publishing owner.
	EventScopeOpened EventKind = "scope_opened"
	// EventScopeClosed reports that a scope has committed closure and begun
	// disposal.
	EventScopeClosed EventKind = "scope_closed"
)

// Event is one committed transition's non-authoritative notification. A
// configuration event carries only the Runtime-local generation string; a
// scope event carries only Kind and the lexical Workspace, with the
// constructor-only DataDir and any narrower attribution left empty. No
// config, credential, plugin object, or storage envelope rides on an event.
type Event struct {
	Kind                  EventKind
	ConfigurationRevision string
	Scope                 ScopeInfo
}

// Subscription is one passive observer's single bounded delivery queue. It
// preserves its own event order, blocks no producer, and grants no authority:
// a subscriber may request ordinary reads when that API exists, but it cannot
// veto transitions, decide execution, or own cleanup. There is no replay:
// buffered events may drain before closure is observed, and missed events are
// repaired from authoritative reads.
type Subscription struct {
	observation *observation
	events      chan Event
	closeOnce   sync.Once
}

// Events returns the receive end of the subscription's bounded queue. The
// channel is closed when the subscription is removed — by Close, by
// saturation, or after Runtime cleanup.
func (s *Subscription) Events() <-chan Event { return s.events }

// Close removes the subscription. It is idempotent, safe after the Runtime
// already removed it, and consumes no buffered events.
func (s *Subscription) Close() { s.closeOnce.Do(func() { s.observation.unsubscribe(s) }) }

// observation is the Runtime's complete passive-event mechanism: one mutex
// over the subscriber set. Each producer takes the mutex immediately before
// its prepared state's short publication, commits that state (releasing its
// own state lock as the commit's final act), enqueues nonblocking to every
// subscriber, and only then releases the mutex, so publication and enqueue
// have one order without a sequence counter, reorder buffer, or delivery
// worker. No filesystem or network work, plugin call, or shutdown wait ever
// occurs inside the section. A saturated subscriber is removed and closed
// while delivery continues to healthy ones.
type observation struct {
	mu     sync.Mutex
	subs   map[*Subscription]struct{}
	closed bool
}

func newObservation() *observation {
	return &observation{subs: make(map[*Subscription]struct{})}
}

// publish runs one producer's observation section. commit is the prepared
// state's short publication, or nil when the caller's state is already
// committed and only the event's place in the order must be fixed.
func (o *observation) publish(commit func(), event Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if commit != nil {
		commit()
	}
	for sub := range o.subs {
		select {
		case sub.events <- event:
		default:
			delete(o.subs, sub)
			close(sub.events)
		}
	}
}

// subscribe attaches one bounded queue. It reports ErrClosed once Runtime
// cleanup has closed every subscription.
func (o *observation) subscribe(capacity int) (*Subscription, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, ErrClosed
	}
	sub := &Subscription{observation: o, events: make(chan Event, capacity)}
	o.subs[sub] = struct{}{}
	return sub, nil
}

// unsubscribe removes and closes one subscription, doing nothing when a
// producer already removed it.
func (o *observation) unsubscribe(s *Subscription) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.subs[s]; ok {
		delete(o.subs, s)
		close(s.events)
	}
}

// closeAll ends observation after the complete Runtime cleanup has published
// every remaining scope event.
func (o *observation) closeAll() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	for sub := range o.subs {
		delete(o.subs, sub)
		close(sub.events)
	}
}

// scopeEvent projects one scope identity into a passive event: Kind and
// lexical Workspace alone, with the constructor-only DataDir and any
// Session or Operation attribution left empty.
func scopeEvent(kind EventKind, info ScopeInfo) Event {
	return Event{Kind: kind, Scope: ScopeInfo{Kind: info.Kind, Workspace: info.Workspace}}
}

// Subscribe attaches one bounded passive observer of committed configuration
// and scope transitions. It requires a positive capacity and enters through
// the same admitted-call gate as every other Runtime operation, so it
// rejects with ErrClosed once closure has begun.
func (r *Runtime) Subscribe(capacity int) (*Subscription, error) {
	if capacity <= 0 {
		return nil, errors.New("runtime: subscription capacity must be positive")
	}
	release, err := r.enter(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	return r.obs.subscribe(capacity)
}
