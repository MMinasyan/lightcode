package runtime

// The mounted hydration pending-delivery row: while one queued delivery's
// admission is unresolved — parked inside its storage transaction — the
// generated client's hydration still lists the selected FIFO head as pending,
// and the adoption removes it in the same coordinator state that commits the
// input and the Operation: a delivery never appears half-applied. Proven over
// the memory and SQLite stores.

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/protocol"
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

// arm arms the park for one entry-payload fragment.
func (s *deliveryParkStore) arm(payload string) {
	s.mu.Lock()
	s.payload = payload
	s.release = make(chan struct{})
	s.mu.Unlock()
}

// releasePark releases an armed park exactly once; repeat and unarmed calls
// are no-ops, so a failing subtest's deferred release cannot deadlock on the
// parked transaction.
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

// deliveryParkTransaction forwards every transaction call and parks the armed
// payload's insertion before its real effect.
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

// TestProtocolHydrationPendingDuringParkedQueuedDelivery proves the mounted
// hydration's pending-delivery row over both stores: a queued head whose
// admission is parked inside its storage transaction stays listed as pending
// — with no adopted Operation — and the adopted state replaces it atomically.
func TestProtocolHydrationPendingDuringParkedQueuedDelivery(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		parked := newDeliveryParkStore(store)
		r, e := openProjectionRuntime(t, parked)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)

		gate := make(chan struct{})
		e.prep.modelGate = gate
		release := sync.OnceFunc(func() { close(gate) })
		defer release() // LIFO: the gate releases before the owner close joins

		created, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: filepath.Join(e.home, "pending-delivery"), AgentType: "solo"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if created.JSON201 == nil {
			t.Fatalf("create = status %d: %s", created.HTTPResponse.StatusCode, created.Body)
		}
		session := created.JSON201.SessionId

		// op-1 parks at its model boundary; the queued message buffers behind
		// the active turn.
		if _, err := client.SubmitSessionWithResponse(ctx, session, protocol.SubmitRequest{
			OperationId: "op-1", Mode: protocol.SubmitRequestModeRegular,
			Content: []protocol.ContentPart{textContentPart(t, "running", nil)},
		}); err != nil {
			t.Fatalf("op-1 submit: %v", err)
		}
		awaitModelArrival(t, e)
		if _, err := client.SubmitSessionWithResponse(ctx, session, protocol.SubmitRequest{
			OperationId: "op-2", Mode: protocol.SubmitRequestModeQueued,
			Content: []protocol.ContentPart{textContentPart(t, "queued-1", nil)},
		}); err != nil {
			t.Fatalf("queued submit: %v", err)
		}

		buffered := mountedHydration(t, client, session)
		if len(buffered.Pending.Queued) != 1 || buffered.Pending.Queued[0].OperationId != "op-2" {
			t.Fatalf("buffered pending = %+v, want the queued op-2", buffered.Pending)
		}

		// The queued delivery parks inside its admission's storage lifetime:
		// the coordinator mutex is free there, so the mounted hydration reads
		// the unresolved delivery's state directly.
		parked.arm("queued-1")
		defer parked.releasePark()
		release()
		select {
		case <-parked.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the queued delivery never parked inside its admission")
		}

		unresolved := mountedHydration(t, client, session)
		if len(unresolved.Pending.Queued) != 1 || unresolved.Pending.Queued[0].OperationId != "op-2" {
			t.Fatalf("pending during the parked delivery = %+v, want the selected head still pending", unresolved.Pending)
		}
		for _, op := range unresolved.Operations {
			if op.OperationId == "op-2" {
				t.Fatalf("the pending item %s committed before its delivery completed", op.OperationId)
			}
		}

		parked.releasePark() // the adoption removes the queued head in its own critical section
		awaitOperation(t, r, session, "op-2", harness.OperationSuccess)

		settled := mountedHydration(t, client, session)
		if len(settled.Pending.Steering) != 0 || len(settled.Pending.Queued) != 0 {
			t.Fatalf("pending after the adoption = %+v, want both FIFOs empty", settled.Pending)
		}
		found := false
		for _, item := range settled.Conversation.Items {
			if itemKind(t, item) != "input" {
				continue
			}
			input, err := item.AsInputItem()
			if err != nil {
				t.Fatalf("input item: %v", err)
			}
			if input.OperationId != nil && *input.OperationId == "op-2" {
				found = true
			}
		}
		if !found {
			t.Fatal("the adopted queued input is missing from the conversation page")
		}
	})
}

// mountedHydration reads one mounted hydration through the generated client.
func mountedHydration(t *testing.T, client *protocol.ClientWithResponses, sessionID string) *protocol.Hydration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.GetSessionHydrationWithResponse(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSessionHydration(%s): %v", sessionID, err)
	}
	if resp.JSON200 == nil {
		t.Fatalf("GetSessionHydration(%s) = status %d: %s", sessionID, resp.HTTPResponse.StatusCode, resp.Body)
	}
	return resp.JSON200
}
