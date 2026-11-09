package melta

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// deliveryParkStore wraps one store: once armed with a payload fragment, the
// matching entry insertion parks inside its transaction until releasePark,
// signalling arrival from inside the park before the real insertion runs.
type deliveryParkStore struct {
	harness.Storage
	mu      sync.Mutex
	payload string
	release chan struct{}
	arrived chan struct{}
}

func newDeliveryParkStore(base harness.Storage) *deliveryParkStore {
	return &deliveryParkStore{Storage: base, arrived: make(chan struct{}, 1)}
}

func (s *deliveryParkStore) arm(payload string) {
	s.mu.Lock()
	s.payload = payload
	s.release = make(chan struct{})
	s.mu.Unlock()
}

func (s *deliveryParkStore) releasePark() {
	s.mu.Lock()
	release := s.release
	s.payload = ""
	s.release = nil
	s.mu.Unlock()
	if release != nil {
		close(release)
	}
}

func (s *deliveryParkStore) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	return s.Storage.Transact(ctx, func(tx harness.Transaction) error {
		return fn(&deliveryParkTransaction{Transaction: tx, store: s})
	})
}

type deliveryParkTransaction struct {
	harness.Transaction
	store *deliveryParkStore
}

func (t *deliveryParkTransaction) InsertEntry(draft harness.EntryDraft) (harness.Entry, error) {
	t.store.mu.Lock()
	payload, release := t.store.payload, t.store.release
	t.store.mu.Unlock()
	if payload != "" && strings.Contains(string(draft.Payload), payload) {
		select {
		case t.store.arrived <- struct{}{}:
		default:
		}
		<-release
	}
	return t.Transaction.InsertEntry(draft)
}

// TestHydrationPendingDuringParkedQueuedDelivery proves the hydration
// pending-delivery row over both stores: a queued head whose admission is
// parked inside its storage transaction stays listed as pending — with no
// adopted Operation — and the adopted state replaces it atomically.
func TestHydrationPendingDuringParkedQueuedDelivery(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		parked := newDeliveryParkStore(store)
		r, e := openProjectionRuntime(t, parked)
		defer closeProjectionRuntime(r)
		caller := testCaller(r)

		gate := make(chan struct{})
		e.server.setHold(gate)
		release := sync.OnceFunc(func() { close(gate) })
		defer release() // LIFO: the gate releases before the owner close joins

		session := projectionSession(t, r, filepath.Join(e.home, "pending-delivery"), "solo").Identity.SessionID

		// op-1 parks at its model boundary; the queued message buffers behind
		// the active turn.
		submitThroughRuntime(t, r, session, "op-1", "running")
		awaitModelArrival(t, e)
		submitQueuedThroughRuntime(t, r, session, "op-2", "queued-1")

		buffered, err := r.GetHydration(ctx, caller, session)
		testassert.NoError(t, err)
		testassert.Require(t, len(buffered.Pending.Queued) == 1 && buffered.Pending.Queued[0].OperationId == "op-2", "buffered pending = %+v, want the queued op-2", buffered.Pending)

		// The queued delivery parks inside its admission's storage lifetime:
		// the coordinator mutex is free there, so the hydration read sees the
		// unresolved delivery's state directly.
		parked.arm("queued-1")
		defer parked.releasePark()
		release()
		select {
		case <-parked.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the queued delivery never parked inside its admission")
		}

		unresolved, err := r.GetHydration(ctx, caller, session)
		testassert.NoError(t, err)
		testassert.Require(t, len(unresolved.Pending.Queued) == 1 && unresolved.Pending.Queued[0].OperationId == "op-2", "pending during the parked delivery = %+v, want the selected head still pending", unresolved.Pending)
		for _, op := range unresolved.Operations {
			testassert.Require(t, op.OperationId != "op-2", "the pending item %s committed before its delivery completed", op.OperationId)
		}

		parked.releasePark() // the adoption removes the queued head in its own critical section
		awaitOperation(t, r, session, "op-2", harness.OperationSuccess)

		settled, err := r.GetHydration(ctx, caller, session)
		testassert.NoError(t, err)
		testassert.Require(t, len(settled.Pending.Steering) == 0 && len(settled.Pending.Queued) == 0, "pending after the adoption = %+v, want both FIFOs empty", settled.Pending)
		found := false
		for _, item := range settled.Conversation.Items {
			if itemKind(t, item) != "input" {
				continue
			}
			input, err := item.AsInputItem()
			testassert.NoError(t, err)
			if input.OperationId != nil && *input.OperationId == "op-2" {
				found = true
			}
		}
		testassert.Require(t, found, "the adopted queued input is missing from the conversation page")
	})
}
