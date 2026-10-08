package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/protocol"
)

// Passive-observation fixtures reuse the owner-lifecycle harness from
// runtime_test.go: one controlled Runtime over a real store, with ordinary
// Workspace plugins for deterministic scope publications.

const obsWorkspace = "/ws"

// equalEvent compares two Events by their exact serialized union bodies: the
// generated marshaling is the one wire form, so byte equality pins every
// member of the closed event union.
func equalEvent(a, b Event) bool {
	ab, aerr := a.MarshalJSON()
	bb, berr := b.MarshalJSON()
	return aerr == nil && berr == nil && bytes.Equal(ab, bb)
}

// eventKind reads one Event's discriminator.
func eventKind(t *testing.T, event Event) string {
	t.Helper()
	kind, err := event.Discriminator()
	if err != nil {
		t.Fatalf("event discriminator: %v", err)
	}
	return kind
}

// scopeKind reads one delivered scope's union discriminator literal.
func scopeKind(t *testing.T, scope protocol.Scope) string {
	t.Helper()
	kind, err := scope.Discriminator()
	if err != nil {
		t.Fatalf("scope discriminator: %v", err)
	}
	return kind
}

// scopeSessionIdentity reads the Session identity one scope names through its
// Session-carrying union branches; the runtime and workspace scopes name none.
func scopeSessionIdentity(t *testing.T, scope protocol.Scope) (string, bool) {
	t.Helper()
	var sessionID string
	switch scopeKind(t, scope) {
	case "session":
		body, err := scope.AsSessionScope()
		if err != nil {
			t.Fatalf("session scope: %v", err)
		}
		sessionID = body.SessionId
	case "operation":
		body, err := scope.AsOperationScope()
		if err != nil {
			t.Fatalf("operation scope: %v", err)
		}
		sessionID = body.SessionId
	case "agent":
		body, err := scope.AsAgentScope()
		if err != nil {
			t.Fatalf("agent scope: %v", err)
		}
		sessionID = body.SessionId
	case "job":
		body, err := scope.AsJobScope()
		if err != nil {
			t.Fatalf("job scope: %v", err)
		}
		sessionID = body.SessionId
	default:
		return "", false
	}
	return sessionID, true
}

// scopeWorkspaceAttribution reads the optional Workspace attribution one
// scope carries through its union branches; the Workspace scope carries it as
// its required identity.
func scopeWorkspaceAttribution(t *testing.T, scope protocol.Scope) (string, bool) {
	t.Helper()
	var workspace *string
	switch scopeKind(t, scope) {
	case "runtime":
		body, err := scope.AsRuntimeScope()
		if err != nil {
			t.Fatalf("runtime scope: %v", err)
		}
		workspace = body.Workspace
	case "workspace":
		body, err := scope.AsWorkspaceScope()
		if err != nil {
			t.Fatalf("workspace scope: %v", err)
		}
		return body.Workspace, true
	case "session":
		body, err := scope.AsSessionScope()
		if err != nil {
			t.Fatalf("session scope: %v", err)
		}
		workspace = body.Workspace
	case "operation":
		body, err := scope.AsOperationScope()
		if err != nil {
			t.Fatalf("operation scope: %v", err)
		}
		workspace = body.Workspace
	case "agent":
		body, err := scope.AsAgentScope()
		if err != nil {
			t.Fatalf("agent scope: %v", err)
		}
		workspace = body.Workspace
	case "job":
		body, err := scope.AsJobScope()
		if err != nil {
			t.Fatalf("job scope: %v", err)
		}
		workspace = body.Workspace
	default:
		return "", false
	}
	if workspace == nil {
		return "", false
	}
	return *workspace, true
}

// eventGeneration reads one configuration event's published generation.
func eventGeneration(t *testing.T, event Event) string {
	t.Helper()
	body, err := event.AsConfigurationChangedEvent()
	if err != nil {
		t.Fatalf("configuration event body: %v", err)
	}
	return body.ConfigurationRevision.Generation
}

// nextEvent reads one event with the test budget applied.
func nextEvent(t *testing.T, sub *Subscription) (Event, bool) {
	t.Helper()
	select {
	case event, ok := <-sub.Events():
		return event, ok
	case <-time.After(10 * time.Second):
		t.Fatal("no event arrived within the test budget")
		return Event{}, false
	}
}

// drainClosed reads every remaining buffered event and requires the queue to
// report closure, proving buffered events drain before closure is observed.
func drainClosed(t *testing.T, sub *Subscription) []Event {
	t.Helper()
	var out []Event
	for {
		event, ok := nextEvent(t, sub)
		if !ok {
			return out
		}
		out = append(out, event)
	}
}

func TestRuntimeSubscribeValidatesCapacityAndUsesTheClosedGate(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := r.Subscribe(0); err == nil {
		t.Fatal("Subscribe(0) succeeded, want capacity > 0 required")
	}
	if _, err := r.Subscribe(-2); err == nil {
		t.Fatal("Subscribe(-2) succeeded, want capacity > 0 required")
	}
	sub, err := r.Subscribe(4)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := r.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	sub.Close()
	sub.Close() // idempotent
	got := drainClosed(t, sub)
	if len(got) != 1 || eventKind(t, got[0]) != "configuration_changed" || eventGeneration(t, got[0]) != "2" {
		t.Fatalf("events = %+v, want the buffered configuration event 2 then closure", got)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := r.Subscribe(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
	}
}

func TestRuntimeObservationPublishesCommittedTransitionsInOrder(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(),
		e.storagePlugin(storage.NewMemory()),
		e.ordinaryPlugin("w", ScopeWorkspace, "w.cap"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sub, err := r.Subscribe(16)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if revision, err := r.Reload(context.Background()); err != nil || revision != "2" {
		t.Fatalf("Reload = (%q, %v), want 2", revision, err)
	}
	if _, err := r.workspaces.get(context.Background(), ScopeInfo{Kind: ScopeWorkspace, DataDir: e.dataDir, Workspace: obsWorkspace}); err != nil {
		t.Fatalf("workspace get: %v", err)
	}
	if revision, err := r.Reload(context.Background()); err != nil || revision != "3" {
		t.Fatalf("Reload = (%q, %v), want 3", revision, err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []Event{
		configurationChangedEvent(2),
		scopeEvent(protocol.ScopeOpened, ScopeInfo{Kind: ScopeWorkspace, Workspace: obsWorkspace}),
		configurationChangedEvent(3),
		scopeEvent(protocol.ScopeClosed, ScopeInfo{Kind: ScopeWorkspace, Workspace: obsWorkspace}),
		scopeEvent(protocol.ScopeClosed, ScopeInfo{Kind: ScopeRuntime}),
	}
	got := drainClosed(t, sub)
	if !slices.EqualFunc(got, want, equalEvent) {
		t.Fatalf("events = %s, want configuration revisions and scope open/close in publication order %s", eventsJSON(t, got), eventsJSON(t, want))
	}
}

func TestObservationPausedPublisherOrdersTheNextPublication(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(),
		e.storagePlugin(storage.NewMemory()),
		e.ordinaryPlugin("w", ScopeWorkspace, "w.cap"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		if err := r.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	sub, err := r.Subscribe(8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	pausedEvent := scopeEvent(protocol.ScopeOpened, ScopeInfo{Kind: ScopeWorkspace, Workspace: "/ws-paused-synthetic"})
	pausedSecond := scopeEvent(protocol.ScopeClosed, ScopeInfo{Kind: ScopeWorkspace, Workspace: "/ws-paused-synthetic"})

	t.Run("a configuration publisher cannot commit or enqueue past a paused publication", func(t *testing.T) {
		_, release := pauseObservation(t, r.obs, pausedEvent)
		defer release()
		reloadDone := make(chan error, 1)
		go func() {
			_, err := r.Reload(context.Background())
			reloadDone <- err
		}()
		select {
		case err := <-reloadDone:
			t.Fatalf("Reload committed while a paused publication held the observation section (%v)", err)
		case event, ok := <-sub.Events():
			t.Fatalf("event %s enqueued before its publication committed (ok=%v)", eventJSON(t, event), ok)
		case <-time.After(200 * time.Millisecond):
		}
		if snapshot := r.config.current(); snapshot == nil || snapshot.generation != 1 {
			t.Fatalf("published generation during pause = %+v, want the paused publication's state commit to block the next one", snapshot)
		}
		release()
		if err := <-reloadDone; err != nil {
			t.Fatalf("Reload after release: %v", err)
		}
		first, ok := nextEvent(t, sub)
		if !ok || !equalEvent(first, pausedEvent) {
			t.Fatalf("first event = %s (ok=%v), want the paused publication's event before the next revision", eventJSON(t, first), ok)
		}
		second, ok := nextEvent(t, sub)
		if !ok || eventKind(t, second) != "configuration_changed" || eventGeneration(t, second) != "2" {
			t.Fatalf("second event = %s (ok=%v), want configuration revision 2 enqueued in publication order", eventJSON(t, second), ok)
		}
	})

	t.Run("a workspace publication cannot commit past a paused publication", func(t *testing.T) {
		_, release := pauseObservation(t, r.obs, pausedSecond)
		defer release()
		getDone := make(chan error, 1)
		go func() {
			_, err := r.workspaces.get(context.Background(), ScopeInfo{Kind: ScopeWorkspace, DataDir: e.dataDir, Workspace: "/ws-paused"})
			getDone <- err
		}()
		select {
		case err := <-getDone:
			t.Fatalf("Workspace scope published while the observation section was held (%v)", err)
		case event, ok := <-sub.Events():
			t.Fatalf("event %s enqueued before its publication committed (ok=%v)", eventJSON(t, event), ok)
		case <-time.After(200 * time.Millisecond):
		}
		release()
		if err := <-getDone; err != nil {
			t.Fatalf("workspace get after release: %v", err)
		}
		first, ok := nextEvent(t, sub)
		if !ok || !equalEvent(first, pausedSecond) {
			t.Fatalf("first event = %s (ok=%v), want the paused publication's event first", eventJSON(t, first), ok)
		}
		second, ok := nextEvent(t, sub)
		wantSecond := scopeEvent(protocol.ScopeOpened, ScopeInfo{Kind: ScopeWorkspace, Workspace: "/ws-paused"})
		if !ok || !equalEvent(second, wantSecond) {
			t.Fatalf("second event = %s (ok=%v), want %s", eventJSON(t, second), ok, eventJSON(t, wantSecond))
		}
	})
}

// eventsJSON renders one event slice compactly for failure diagnostics.
func eventsJSON(t *testing.T, events []Event) string {
	t.Helper()
	parts := make([]string, 0, len(events))
	for _, event := range events {
		parts = append(parts, eventJSON(t, event))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func eventJSON(t *testing.T, event Event) string {
	t.Helper()
	data, err := event.MarshalJSON()
	if err != nil {
		return "<unset>"
	}
	return string(data)
}

// pauseObservation takes the Runtime's observation section through the same
// primitive every producer uses, reports arrival after its state commit, and
// stays paused inside the section until the returned release runs.
func pauseObservation(t *testing.T, o *observation, event Event) (<-chan struct{}, func()) {
	t.Helper()
	arrive := make(chan struct{})
	released := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		o.publish(func() []Event {
			close(arrive)
			<-released
			return []Event{event}
		})
		close(done)
	}()
	select {
	case <-arrive:
	case <-time.After(10 * time.Second):
		t.Fatal("the paused publisher never reached its commit")
	}
	return arrive, func() {
		once.Do(func() { close(released) })
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the paused publisher never released the observation section")
		}
	}
}

func TestObservationSaturatedSubscriberIsRemovedAndHealthyOnesContinue(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	full, err := r.Subscribe(1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	healthy, err := r.Subscribe(8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	for _, want := range []string{"2", "3", "4"} {
		if revision, err := r.Reload(context.Background()); err != nil || revision != want {
			t.Fatalf("Reload = (%q, %v), want %s", revision, err, want)
		}
	}
	first, ok := nextEvent(t, full)
	if !ok || eventGeneration(t, first) != "2" {
		t.Fatalf("saturated subscriber first event = %s (ok=%v), want revision 2", eventJSON(t, first), ok)
	}
	if _, ok := nextEvent(t, full); ok {
		t.Fatal("saturated subscriber still open, want it removed and closed")
	}
	full.Close()
	full.Close() // idempotent after producer removal
	for _, want := range []string{"2", "3", "4"} {
		event, ok := nextEvent(t, healthy)
		if !ok {
			t.Fatalf("healthy subscriber closed early, want revision %s", want)
		}
		if eventKind(t, event) != "configuration_changed" || eventGeneration(t, event) != want {
			t.Fatalf("healthy subscriber event = %s, want configuration %s", eventJSON(t, event), want)
		}
	}
	if revision, err := r.Reload(context.Background()); err != nil || revision != "5" {
		t.Fatalf("Reload = (%q, %v), want 5", revision, err)
	}
	event, ok := nextEvent(t, healthy)
	if !ok || eventGeneration(t, event) != "5" {
		t.Fatalf("post-saturation event = %s (ok=%v), want revision 5", eventJSON(t, event), ok)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	drainClosed(t, healthy)
}

func TestObservationSubscriptionCloseRacesPublication(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	healthy, err := r.Subscribe(64)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	var churn, reloads sync.WaitGroup
	for range 8 {
		churn.Add(1)
		go func() {
			defer churn.Done()
			for range 16 {
				sub, err := r.Subscribe(1)
				if err != nil {
					t.Errorf("Subscribe during publication race: %v", err)
					return
				}
				select {
				case event, ok := <-sub.Events():
					if ok && eventKind(t, event) != "configuration_changed" {
						t.Errorf("churn subscriber event = %s, want configuration", eventJSON(t, event))
					}
				default:
				}
				sub.Close()
				sub.Close()
			}
		}()
	}
	for i := range 10 {
		reloads.Add(1)
		go func(i int) {
			defer reloads.Done()
			if _, err := r.Reload(context.Background()); err != nil {
				t.Errorf("Reload %d: %v", i, err)
			}
		}(i)
	}
	reloads.Wait()
	churn.Wait()
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var revisions []string
	for _, event := range drainClosed(t, healthy) {
		switch eventKind(t, event) {
		case "configuration_changed":
			revisions = append(revisions, eventGeneration(t, event))
		case "scope_closed":
			body, err := event.AsScopeEvent()
			if err != nil {
				t.Fatalf("scope event body: %v", err)
			}
			if scopeKind(t, body.Scope) != "runtime" {
				t.Fatalf("unexpected closure event %s, only the Runtime scope closes here", eventJSON(t, event))
			}
			runtimeScope, err := body.Scope.AsRuntimeScope()
			if err != nil {
				t.Fatalf("runtime scope: %v", err)
			}
			if runtimeScope.Workspace != nil {
				t.Fatalf("unexpected closure event %s, only the Runtime scope closes here", eventJSON(t, event))
			}
		default:
			t.Fatalf("unexpected event %s", eventJSON(t, event))
		}
	}
	want := make([]string, 0, 10)
	for generation := uint64(2); generation <= 11; generation++ {
		want = append(want, strconv.FormatUint(generation, 10))
	}
	if !slices.Equal(revisions, want) {
		t.Fatalf("raced publication revisions = %v, want every committed generation exactly once in order %v", revisions, want)
	}
}

func TestRuntimeShutdownDoesNotWaitForUndrainedObservers(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(),
		e.storagePlugin(storage.NewMemory()),
		e.ordinaryPlugin("w", ScopeWorkspace, "w.cap"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	stalled, err := r.Subscribe(1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	for _, want := range []string{"2", "3"} {
		if revision, err := r.Reload(context.Background()); err != nil || revision != want {
			t.Fatalf("Reload = (%q, %v), want %s", revision, err, want)
		}
	}
	if _, err := r.workspaces.get(context.Background(), ScopeInfo{Kind: ScopeWorkspace, DataDir: e.dataDir, Workspace: obsWorkspace}); err != nil {
		t.Fatalf("workspace get: %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close(context.Background()) }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown waited for undrained observer output")
	}
	first, ok := nextEvent(t, stalled)
	if !ok || eventGeneration(t, first) != "2" {
		t.Fatalf("stalled subscriber event = %s (ok=%v), want its one buffered event", eventJSON(t, first), ok)
	}
	if _, ok := nextEvent(t, stalled); ok {
		t.Fatal("stalled subscriber was never closed")
	}
	e.assertLockReleased(t)
}

func TestObservationSubscriberLossDoesNotAffectExecution(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		r, err := e.open(context.Background(), e.storagePlugin(store),
			e.ordinaryPlugin("w", ScopeWorkspace, "w.cap"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		session, err := r.createSession(context.Background(), e.dataDir, "solo")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sessionID := session.Identity.SessionID
		gate := make(chan struct{})
		e.server.setHold(gate)
		submitThroughRuntime(t, r, sessionID, "op-observed", "hello")
		select {
		case <-e.server.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the gated execution never reached its model request")
		}
		full, err := r.Subscribe(1)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		healthy, err := r.Subscribe(64)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		for _, want := range []string{"2", "3"} {
			if revision, err := r.Reload(context.Background()); err != nil || revision != want {
				t.Fatalf("Reload during execution = (%q, %v), want %s", revision, err, want)
			}
		}
		close(gate)
		if rec := awaitOperation(t, r, sessionID, "op-observed", harness.OperationSuccess); rec.State.Status != harness.OperationSuccess {
			t.Fatalf("Operation state = %+v, want success: subscriber saturation changed no execution outcome", rec.State)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		first, ok := nextEvent(t, full)
		if !ok || eventGeneration(t, first) != "2" {
			t.Fatalf("saturated subscriber event = %s (ok=%v), want revision 2", eventJSON(t, first), ok)
		}
		if _, ok := nextEvent(t, full); ok {
			t.Fatal("saturated subscriber was never removed")
		}
		want := []Event{
			configurationChangedEvent(2),
			configurationChangedEvent(3),
			scopeEvent(protocol.ScopeClosed, ScopeInfo{Kind: ScopeAgent, Workspace: e.dataDir, SessionID: sessionID, OperationID: "op-observed"}),
			scopeEvent(protocol.ScopeClosed, ScopeInfo{Kind: ScopeOperation, Workspace: e.dataDir, SessionID: sessionID, OperationID: "op-observed"}),
			scopeEvent(protocol.ScopeClosed, ScopeInfo{Kind: ScopeWorkspace, Workspace: e.dataDir}),
			scopeEvent(protocol.ScopeClosed, ScopeInfo{Kind: ScopeRuntime}),
		}
		// The committed scope/configuration order stays exact while the
		// execution's passive facts are accounted separately: each must be
		// scoped to the real Session and its running Operation, never a
		// foreign identity, and none may carry the pre-subscription
		// admission replay — the admission committed before this
		// subscription existed and there is no replay mechanism to
		// rediscover it.
		var committed []Event
		for _, event := range drainClosed(t, healthy) {
			switch eventKind(t, event) {
			case "configuration_changed", "scope_opened", "scope_closed":
				committed = append(committed, event)
				continue
			case "session_changed":
				body, err := event.AsSessionChangedEvent()
				if err != nil {
					t.Fatalf("session event body: %v", err)
				}
				invalidation, err := body.Scope.AsSessionScope()
				if err != nil {
					t.Fatalf("passive invalidation scope: %v", err)
				}
				if invalidation.SessionId != sessionID {
					t.Fatalf("passive invalidation for %q, want the real session", invalidation.SessionId)
				}
			case "text_delta", "tool_started", "tool_finished":
				if err := assertOperationScoped(t, event, sessionID, "op-observed"); err != nil {
					t.Fatalf("passive progress %s: %v", eventJSON(t, event), err)
				}
			default:
				t.Fatalf("unexpected event %s", eventJSON(t, event))
			}
		}
		if !slices.EqualFunc(committed, want, equalEvent) {
			t.Fatalf("healthy subscriber committed events = %s, want reload events during active execution plus every committed closure in shutdown order: %s", eventsJSON(t, committed), eventsJSON(t, want))
		}
	})
}

// progressEventScope extracts the operation scope one progress event
// carries; any other event kind fails the test.
func progressEventScope(t *testing.T, event Event) protocol.Scope {
	t.Helper()
	switch eventKind(t, event) {
	case "text_delta":
		body, err := event.AsTextDeltaEvent()
		if err != nil {
			t.Fatalf("delta event body: %v", err)
		}
		return body.Scope
	case "refusal_delta":
		body, err := event.AsRefusalDeltaEvent()
		if err != nil {
			t.Fatalf("refusal event body: %v", err)
		}
		return body.Scope
	case "tool_started":
		body, err := event.AsToolStartedEvent()
		if err != nil {
			t.Fatalf("tool event body: %v", err)
		}
		return body.Scope
	case "tool_finished":
		body, err := event.AsToolFinishedEvent()
		if err != nil {
			t.Fatalf("tool event body: %v", err)
		}
		return body.Scope
	}
	t.Fatalf("event %s is not a progress event", eventJSON(t, event))
	return protocol.Scope{}
}

// assertOperationScoped checks one progress event carries the real Session
// and Operation identity.
func assertOperationScoped(t *testing.T, event Event, sessionID, operationID string) error {
	t.Helper()
	scope := progressEventScope(t, event)
	if scopeKind(t, scope) != "operation" {
		return fmt.Errorf("progress carries the %s scope, want the operation scope", scopeKind(t, scope))
	}
	body, err := scope.AsOperationScope()
	if err != nil {
		t.Fatalf("operation scope: %v", err)
	}
	if body.SessionId != sessionID || body.OperationId != operationID {
		return errors.New("progress carries a foreign session or operation identity")
	}
	return nil
}

// snapshotPairOf reads one Session's current revision pair through the
// public snapshot.
func snapshotPairOf(t *testing.T, r *Runtime, sessionID string) protocol.SessionRevision {
	t.Helper()
	snap := snapshotThroughRuntime(t, r, sessionID)
	return wireRevision(snapshotRevision(snap))
}

// TestObservationRefusalDeltaProgress proves live refusal-bearing model
// streams reach the passive bus as distinct refusal_delta events: a
// refusal-only turn and a mixed content turn keep their exact fragment
// order — content text before the refusal inside one delta — and every
// hint addresses the real running Operation.
func TestObservationRefusalDeltaProgress(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		bg := openBackgroundRuntime(t, store, newLifecycleStopper())
		defer func() {
			if err := bg.r.Close(context.Background()); err != nil {
				bg.t.Errorf("Close: %v", err)
			}
		}()
		sub, err := bg.r.Subscribe(256)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		session := bg.session("refusal")
		// The wire dispatch: the request whose history carries no refusal
		// receives the refusal-only turn; the later one — whose history
		// retains the refused assistant — receives the mixed turn.
		bg.e.server.setScript(func(_ context.Context, body string) []string {
			if !strings.Contains(body, `"refusal"`) {
				return []string{
					`{"choices":[{"delta":{"role":"assistant","refusal":"I cannot"}}]}`,
					`{"choices":[{"delta":{"refusal":" help with that."}}]}`,
					`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				}
			}
			return []string{
				`{"choices":[{"delta":{"role":"assistant","content":"working"}}]}`,
				`{"choices":[{"delta":{"content":"harder","refusal":" but no"}}]}`,
				`{"choices":[{"delta":{"refusal":" can do."}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			}
		})
		bg.submitMode(session, "op-refuse", "hello", harness.MessageModeRegular)
		awaitOperation(t, bg.r, session, "op-refuse", harness.OperationSuccess)
		bg.submitMode(session, "op-mixed", "hello", harness.MessageModeRegular)
		awaitOperation(t, bg.r, session, "op-mixed", harness.OperationSuccess)

		type progress struct {
			kind    string
			content string
			op      string
		}
		var got []progress
		for {
			select {
			case event, ok := <-sub.Events():
				if !ok {
					t.Fatal("the subscription closed before the drain finished")
				}
				var entry progress
				switch eventKind(t, event) {
				case "text_delta":
					body, err := event.AsTextDeltaEvent()
					if err != nil {
						t.Fatalf("delta event body: %v", err)
					}
					scope, err := body.Scope.AsOperationScope()
					if err != nil {
						t.Fatalf("text progress scope: %v", err)
					}
					entry = progress{kind: "text_delta", content: body.Content, op: scope.OperationId}
				case "refusal_delta":
					body, err := event.AsRefusalDeltaEvent()
					if err != nil {
						t.Fatalf("refusal event body: %v", err)
					}
					scope, err := body.Scope.AsOperationScope()
					if err != nil {
						t.Fatalf("refusal progress scope: %v", err)
					}
					entry = progress{kind: "refusal_delta", content: body.Content, op: scope.OperationId}
				default:
					// committed and lifecycle publications interleave with progress
					continue
				}
				if scope := progressEventScope(t, event); scopeKind(t, scope) != "operation" {
					t.Fatalf("progress %s carries the %s scope, want the operation scope", eventJSON(t, event), scopeKind(t, scope))
				}
				if sessionID, _ := scopeSessionIdentity(t, progressEventScope(t, event)); sessionID != session {
					t.Fatalf("progress %s names a foreign session", eventJSON(t, event))
				}
				got = append(got, entry)
				continue
			default:
			}
			break
		}
		want := []progress{
			{kind: "refusal_delta", content: "I cannot", op: "op-refuse"},
			{kind: "refusal_delta", content: " help with that.", op: "op-refuse"},
			{kind: "text_delta", content: "working", op: "op-mixed"},
			{kind: "text_delta", content: "harder", op: "op-mixed"},
			{kind: "refusal_delta", content: " but no", op: "op-mixed"},
			{kind: "refusal_delta", content: " can do.", op: "op-mixed"},
		}
		if !slices.Equal(got, want) {
			t.Fatalf("progress = %+v, want the exact fragment order and operation identity %+v", got, want)
		}
	})
}

// lessPair reports whether one wire revision pair is strictly older than
// another on the numeric pair alone (the instance identity is empty
// pre-server). The wire members are decimal strings; malformed input fails
// the test rather than comparing lexicographically.
func lessPair(t *testing.T, a, b protocol.SessionRevision) bool {
	t.Helper()
	ad, err := strconv.ParseInt(a.DurableRevision, 10, 64)
	if err != nil {
		t.Fatalf("revision durable member %q is not a decimal integer", a.DurableRevision)
	}
	bd, err := strconv.ParseInt(b.DurableRevision, 10, 64)
	if err != nil {
		t.Fatalf("revision durable member %q is not a decimal integer", b.DurableRevision)
	}
	al, err := strconv.ParseUint(a.LocalRevision, 10, 64)
	if err != nil {
		t.Fatalf("revision local member %q is not a decimal integer", a.LocalRevision)
	}
	bl, err := strconv.ParseUint(b.LocalRevision, 10, 64)
	if err != nil {
		t.Fatalf("revision local member %q is not a decimal integer", b.LocalRevision)
	}
	return ad < bd || (ad == bd && al < bl)
}
