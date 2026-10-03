package runtime

import (
	"context"
	"errors"
	"strconv"
	"sync"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// Event is the generated closed event union: every passive notification the
// Runtime publishes on the bounded observation bus is a protocol Event built
// by the generated constructors, and subscribers read it through the
// generated discriminator and accessors. No handwritten event shape exists.
type Event = protocol.Event

// Subscription is one passive observer's single bounded delivery queue. It
// preserves its own event order, blocks no producer, and grants no authority:
// a subscriber may request ordinary reads when that API exists, but it cannot
// veto transitions, decide execution, or own cleanup. There is no replay:
// buffered events may drain before closure is observed, missed events are
// repaired from authoritative reads, and transient progress may be lost.
// The closed channel signals the subscription's removal exactly once — by
// its subscriber, by saturation, or after Runtime cleanup — without
// competing with the event queue; a delivery goroutine may select on it to
// release resources the queue alone cannot reach.
type Subscription struct {
	observation *observation
	events      chan Event
	closed      chan struct{}
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
// over the subscriber set. Each producer takes the mutex, applies its
// in-memory publication and samples current data inside the section, returns
// zero or more events, and only then enqueues nonblocking to every
// subscriber, so publication and enqueue have one order without a sequence
// counter, reorder buffer, or delivery worker. No filesystem or network
// work, plugin call, or shutdown wait ever occurs inside the section. A
// saturated subscriber is removed and closed while delivery continues to
// healthy ones.
type observation struct {
	mu     sync.Mutex
	subs   map[*Subscription]struct{}
	closed bool
}

func newObservation() *observation {
	return &observation{subs: make(map[*Subscription]struct{})}
}

// publish runs one producer's observation section: the produce callback
// applies its in-memory publication, samples the current data its events
// carry, and returns the events to enqueue — zero when nothing observable
// changed or the subject is unavailable. Everything produce touches is
// in-memory; the section never waits on a subscriber, a socket, or a client.
func (o *observation) publish(produce func() []Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, event := range produce() {
		for sub := range o.subs {
			select {
			case sub.events <- event:
			default:
				delete(o.subs, sub)
				close(sub.events)
				close(sub.closed)
			}
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
	sub := &Subscription{
		observation: o,
		events:      make(chan Event, capacity),
		closed:      make(chan struct{}),
	}
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
		close(s.closed)
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
		close(sub.closed)
	}
}

// eventString renders one scope member as its optional wire pointer; an
// empty value is an absent member, matching the validated scope shapes.
func eventString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// runtimeEventScope is the scope of every Runtime-global publication: the
// configuration generation, the global warning groups, and the LSP ingest
// own no narrower attribution.
func runtimeEventScope() protocol.Scope {
	return protocol.Scope{Kind: protocol.ScopeKindRuntime}
}

// sessionEventScope is one Session-granularity invalidation scope.
func sessionEventScope(workspace, sessionID string) protocol.Scope {
	return protocol.Scope{
		Kind:      protocol.ScopeKindSession,
		Workspace: eventString(workspace),
		SessionId: eventString(sessionID),
	}
}

// jobEventScope is one job-member invalidation scope: the owning Session with
// the Job identity.
func jobEventScope(workspace, sessionID, jobID string) protocol.Scope {
	return protocol.Scope{
		Kind:      protocol.ScopeKindJob,
		Workspace: eventString(workspace),
		SessionId: eventString(sessionID),
		JobId:     eventString(jobID),
	}
}

// operationEventScope is one progress scope: the Operation running the model
// stream or tool call, with its Session and Workspace ancestors.
func operationEventScope(workspace, sessionID, operationID string) protocol.Scope {
	return protocol.Scope{
		Kind:        protocol.ScopeKindOperation,
		Workspace:   eventString(workspace),
		SessionId:   eventString(sessionID),
		OperationId: eventString(operationID),
	}
}

// scopeEvent projects one scope identity into its passive event: the scope
// kind with every ancestor the ScopeInfo actually carries — runtime carries
// nothing, a Workspace scope carries the lexical Workspace, and Operation and
// Agent scopes carry the complete Workspace/Session/Operation identity. The
// constructor-only DataDir and the passive warning callback never ride.
func scopeEvent(kind protocol.ScopeEventKind, info ScopeInfo) Event {
	var event Event
	_ = event.FromScopeEvent(protocol.ScopeEvent{ // plain string members; the marshal cannot fail
		Kind: kind,
		Scope: protocol.Scope{
			Kind:        protocol.ScopeKind(info.Kind),
			Workspace:   eventString(info.Workspace),
			SessionId:   eventString(info.SessionID),
			OperationId: eventString(info.OperationID),
		},
	})
	return event
}

// configurationChangedEvent builds the configuration publication hint: the
// new generation with the empty pre-server instance identity.
func configurationChangedEvent(generation uint64) Event {
	var event Event
	_ = event.FromConfigurationChangedEvent(protocol.ConfigurationChangedEvent{ // plain members; the marshal cannot fail
		Kind:                  protocol.ConfigurationChanged,
		Scope:                 runtimeEventScope(),
		ConfigurationRevision: protocol.ConfigurationRevision{Generation: strconv.FormatUint(generation, 10)},
	})
	return event
}

// warningChangedEvent builds the warning-store change hint with the captured
// revision.
func warningChangedEvent(scope protocol.Scope, revision uint64) Event {
	var event Event
	_ = event.FromWarningChangedEvent(protocol.WarningChangedEvent{ // plain members; the marshal cannot fail
		Kind:             protocol.WarningChanged,
		Scope:            scope,
		WarningsRevision: protocol.WarningsRevision{Revision: strconv.FormatUint(revision, 10)},
	})
	return event
}

// sessionChangedEvent builds one invalidation hint carrying the current
// revision pair with the empty pre-server instance identity.
func sessionChangedEvent(scope protocol.Scope, pair harness.SessionRevision) Event {
	var event Event
	_ = event.FromSessionChangedEvent(protocol.SessionChangedEvent{ // plain members; the marshal cannot fail
		Kind:  protocol.SessionChanged,
		Scope: scope,
		SessionRevision: protocol.SessionRevision{
			DurableRevision: strconv.FormatInt(pair.DurableRevision, 10),
			LocalRevision:   strconv.FormatUint(pair.LocalRevision, 10),
		},
	})
	return event
}

// textDeltaEvent builds one transient model-stream fragment hint.
func textDeltaEvent(scope protocol.Scope, position int, content string) Event {
	var event Event
	_ = event.FromTextDeltaEvent(protocol.TextDeltaEvent{ // plain members; the marshal cannot fail
		Kind:     protocol.TextDelta,
		Scope:    scope,
		Position: position,
		Content:  content,
	})
	return event
}

// toolStartedEvent builds one dispatched tool-call hint.
func toolStartedEvent(scope protocol.Scope, callID string, ordinal int64, name string) Event {
	var event Event
	_ = event.FromToolStartedEvent(protocol.ToolStartedEvent{ // plain members; the marshal cannot fail
		Kind:    protocol.ToolStarted,
		Scope:   scope,
		CallId:  callID,
		Ordinal: int(ordinal),
		Name:    name,
	})
	return event
}

// toolFinishedEvent builds one committed tool-result hint.
func toolFinishedEvent(scope protocol.Scope, callID string, status model.ToolResultStatus) Event {
	var event Event
	_ = event.FromToolFinishedEvent(protocol.ToolFinishedEvent{ // plain members; the marshal cannot fail
		Kind:   protocol.ToolFinished,
		Scope:  scope,
		CallId: callID,
		Status: protocol.ToolCallStatus(status),
	})
	return event
}

// observationAdapter is the Runtime's one passive publication adapter: it
// maps committed Harness facts and warning-store changes onto the bounded
// observation bus as generated protocol events. It is created before any
// plugin scope opens — the Runtime scope's ScopeInfo carries its LSP closure —
// and its Harness handle is bound before work admission, so no caller can
// observe it unarmed: facts and warning publications only exist for admitted
// work. Every publication samples the subject's identity and current
// revision inside the observation section through the cached-only
// Harness read, so a notification delayed behind a later commit carries the
// current state, never the producer's stale pair; an unavailable subject
// suppresses its hint while the underlying store change stands.
type observationAdapter struct {
	obs      *observation
	warnings *warningStore

	// h is the bound Harness; the adapter is constructed before it exists.
	h *harness.Harness
}

func newObservationAdapter(obs *observation, warnings *warningStore) *observationAdapter {
	return &observationAdapter{obs: obs, warnings: warnings}
}

// observe is the Harness fact callback: one fact publishes at most one event.
// The fact's own revision pair is superseded by the pair sampled at
// publication time; a Session that is deleted, corrupt, or not cached
// suppresses its hint; progress events carry the Operation's scope.
func (a *observationAdapter) observe(fact harness.HarnessFact) {
	if a == nil {
		return
	}
	a.obs.publish(func() []Event {
		identity, pair, ok := a.readSubject(fact.SessionID)
		if !ok {
			return nil
		}
		switch fact.Kind {
		case harness.FactInvalidation:
			scope := sessionEventScope(identity.Workspace, fact.SessionID)
			if fact.JobID != "" {
				scope = jobEventScope(identity.Workspace, fact.SessionID, fact.JobID)
			}
			return []Event{sessionChangedEvent(scope, pair)}
		case harness.FactTextDelta:
			scope := operationEventScope(identity.Workspace, fact.SessionID, fact.OperationID)
			return []Event{textDeltaEvent(scope, fact.Position, fact.Content)}
		case harness.FactToolStarted:
			scope := operationEventScope(identity.Workspace, fact.SessionID, fact.OperationID)
			return []Event{toolStartedEvent(scope, fact.CallID, fact.Ordinal, fact.Name)}
		case harness.FactToolFinished:
			scope := operationEventScope(identity.Workspace, fact.SessionID, fact.OperationID)
			return []Event{toolFinishedEvent(scope, fact.CallID, fact.Status)}
		}
		return nil
	})
}

// readSubject samples one Session's identity through the cached-only
// observation read. A test-constructed adapter without a bound Harness
// suppresses the hint, mirroring the nil-store presentation drop.
func (a *observationAdapter) readSubject(sessionID string) (harness.SessionIdentity, harness.SessionRevision, bool) {
	if a.h == nil {
		return harness.SessionIdentity{}, harness.SessionRevision{}, false
	}
	return a.h.ReadObservation(sessionID)
}

// publishPrompt replaces one Session's prompt group and publishes its
// warning_changed hint in the same section as the group change and revision
// capture. Only a successful preparation calls it.
func (a *observationAdapter) publishPrompt(sessionID string, warnings []protocol.Warning) {
	if a == nil {
		return
	}
	a.obs.publish(func() []Event {
		if !a.warnings.setSessionPrompt(sessionID, warnings) {
			return nil
		}
		return a.sessionWarningEvents(sessionID)
	})
}

// appendProtocolWarnings adds one transport attempt's diagnostics to the
// owning Session's protocol group and publishes its warning_changed hint in
// the same section.
func (a *observationAdapter) appendProtocolWarnings(sessionID string, warnings []protocol.Warning) {
	if a == nil {
		return
	}
	a.obs.publish(func() []Event {
		if !a.warnings.appendProtocol(sessionID, warnings) {
			return nil
		}
		return a.sessionWarningEvents(sessionID)
	})
}

// reportLSP is the Runtime scope's passive warning callback: it appends one
// report into the Runtime-global LSP group and publishes the runtime-scoped
// warning_changed hint in the same section. Reports after the warning
// store's closure are ignored by the store core.
func (a *observationAdapter) reportLSP(kind, message string) {
	if a == nil {
		return
	}
	a.obs.publish(func() []Event {
		if !a.warnings.addLSP(kind, message) {
			return nil
		}
		return []Event{warningChangedEvent(runtimeEventScope(), a.warnings.storeRevision())}
	})
}

// sessionWarningEvents builds one Session-scoped warning change hint: the
// scope's Workspace comes from the cached identity read, and an unavailable
// Session suppresses the hint while the store change stands — every prompt or
// protocol publication runs while its Session is busy, so the branch is the
// uniform suppression rule.
func (a *observationAdapter) sessionWarningEvents(sessionID string) []Event {
	identity, _, ok := a.readSubject(sessionID)
	if !ok {
		return nil
	}
	return []Event{warningChangedEvent(sessionEventScope(identity.Workspace, sessionID), a.warnings.storeRevision())}
}

// Subscribe attaches one bounded passive observer of committed configuration,
// scope, Session, and warning transitions. It requires a positive capacity
// and enters through the same admitted-call gate as every other Runtime
// operation, so it rejects with ErrClosed once closure has begun.
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
