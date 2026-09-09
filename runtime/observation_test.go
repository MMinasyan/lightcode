package runtime

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
)

// Passive-observation fixtures reuse the owner-lifecycle harness from
// runtime_test.go: one controlled Runtime over a real store, with ordinary
// Workspace plugins for deterministic scope publications.

const obsWorkspace = "/ws"

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
	if len(got) != 1 || got[0].Kind != EventConfiguration || got[0].ConfigurationRevision != "2" {
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
		{Kind: EventConfiguration, ConfigurationRevision: "2"},
		{Kind: EventScopeOpened, Scope: ScopeInfo{Kind: ScopeWorkspace, Workspace: obsWorkspace}},
		{Kind: EventConfiguration, ConfigurationRevision: "3"},
		{Kind: EventScopeClosed, Scope: ScopeInfo{Kind: ScopeWorkspace, Workspace: obsWorkspace}},
		{Kind: EventScopeClosed, Scope: ScopeInfo{Kind: ScopeRuntime}},
	}
	got := drainClosed(t, sub)
	if !slices.Equal(got, want) {
		t.Fatalf("events = %+v, want configuration revisions and scope open/close in publication order %+v", got, want)
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

	t.Run("a configuration publisher cannot commit or enqueue past a paused publication", func(t *testing.T) {
		_, release := pauseObservation(t, r.obs, Event{Kind: EventConfiguration, ConfigurationRevision: "paused"})
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
			t.Fatalf("event %+v enqueued before its publication committed (ok=%v)", event, ok)
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
		if !ok || first.ConfigurationRevision != "paused" {
			t.Fatalf("first event = %+v (ok=%v), want the paused publication's event before the next revision", first, ok)
		}
		second, ok := nextEvent(t, sub)
		if !ok || second.Kind != EventConfiguration || second.ConfigurationRevision != "2" {
			t.Fatalf("second event = %+v (ok=%v), want configuration revision 2 enqueued in publication order", second, ok)
		}
	})

	t.Run("a workspace publication cannot commit past a paused publication", func(t *testing.T) {
		_, release := pauseObservation(t, r.obs, Event{Kind: EventConfiguration, ConfigurationRevision: "paused-2"})
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
			t.Fatalf("event %+v enqueued before its publication committed (ok=%v)", event, ok)
		case <-time.After(200 * time.Millisecond):
		}
		release()
		if err := <-getDone; err != nil {
			t.Fatalf("workspace get after release: %v", err)
		}
		first, ok := nextEvent(t, sub)
		if !ok || first.ConfigurationRevision != "paused-2" {
			t.Fatalf("first event = %+v (ok=%v), want the paused publication's event first", first, ok)
		}
		second, ok := nextEvent(t, sub)
		wantSecond := Event{Kind: EventScopeOpened, Scope: ScopeInfo{Kind: ScopeWorkspace, Workspace: "/ws-paused"}}
		if !ok || second != wantSecond {
			t.Fatalf("second event = %+v (ok=%v), want %+v", second, ok, wantSecond)
		}
	})
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
		o.publish(func() {
			close(arrive)
			<-released
		}, event)
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
	if !ok || first.ConfigurationRevision != "2" {
		t.Fatalf("saturated subscriber first event = %+v (ok=%v), want revision 2", first, ok)
	}
	if _, ok := nextEvent(t, full); ok {
		t.Fatal("saturated subscriber still open, want it removed and closed")
	}
	full.Close()
	full.Close() // idempotent after producer removal
	var got []string
	for _, want := range []string{"2", "3", "4"} {
		event, ok := nextEvent(t, healthy)
		if !ok {
			t.Fatalf("healthy subscriber closed early, want revision %s", want)
		}
		if event.Kind != EventConfiguration || event.ConfigurationRevision != want {
			t.Fatalf("healthy subscriber event = %+v, want configuration %s", event, want)
		}
		got = append(got, event.ConfigurationRevision)
	}
	if revision, err := r.Reload(context.Background()); err != nil || revision != "5" {
		t.Fatalf("Reload = (%q, %v), want 5", revision, err)
	}
	event, ok := nextEvent(t, healthy)
	if !ok || event.ConfigurationRevision != "5" {
		t.Fatalf("post-saturation event = %+v (ok=%v), want revision 5", event, ok)
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
					if ok && event.Kind != EventConfiguration {
						t.Errorf("churn subscriber event = %+v, want configuration", event)
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
		switch event.Kind {
		case EventConfiguration:
			revisions = append(revisions, event.ConfigurationRevision)
		case EventScopeClosed:
			if event.Scope.Kind != ScopeRuntime || event.Scope.Workspace != "" {
				t.Fatalf("unexpected closure event %+v, only the Runtime scope closes here", event)
			}
		default:
			t.Fatalf("unexpected event %+v", event)
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
	if !ok || first.ConfigurationRevision != "2" {
		t.Fatalf("stalled subscriber event = %+v (ok=%v), want its one buffered event", first, ok)
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
		e.prep.modelGate = make(chan struct{})
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-observed", "hello")
		select {
		case <-e.prep.modelArrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the gated execution never reached its model effect")
		}
		full, err := r.Subscribe(1)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		healthy, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		for _, want := range []string{"2", "3"} {
			if revision, err := r.Reload(context.Background()); err != nil || revision != want {
				t.Fatalf("Reload during execution = (%q, %v), want %s", revision, err, want)
			}
		}
		close(e.prep.modelGate)
		e.prep.awaitCleanups(1)
		if rec := readOperation(t, r, session.Identity.SessionID, "op-observed"); rec.State.Status != harness.OperationSuccess {
			t.Fatalf("Operation state = %+v, want success: subscriber saturation changed no execution outcome", rec.State)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		first, ok := nextEvent(t, full)
		if !ok || first.ConfigurationRevision != "2" {
			t.Fatalf("saturated subscriber event = %+v (ok=%v), want revision 2", first, ok)
		}
		if _, ok := nextEvent(t, full); ok {
			t.Fatal("saturated subscriber was never removed")
		}
		want := []Event{
			{Kind: EventConfiguration, ConfigurationRevision: "2"},
			{Kind: EventConfiguration, ConfigurationRevision: "3"},
			{Kind: EventScopeClosed, Scope: ScopeInfo{Kind: ScopeAgent, Workspace: e.dataDir}},
			{Kind: EventScopeClosed, Scope: ScopeInfo{Kind: ScopeOperation, Workspace: e.dataDir}},
			{Kind: EventScopeClosed, Scope: ScopeInfo{Kind: ScopeWorkspace, Workspace: e.dataDir}},
			{Kind: EventScopeClosed, Scope: ScopeInfo{Kind: ScopeRuntime}},
		}
		got := drainClosed(t, healthy)
		if !slices.Equal(got, want) {
			t.Fatalf("healthy subscriber events = %+v, want reload events during active execution plus every committed closure in shutdown order, with the pre-subscription opens unreplayed: %+v", got, want)
		}
	})
}
