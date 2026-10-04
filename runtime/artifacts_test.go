// The private per-Session artifact interval registry: exactly one holder per
// key, reference-counted entries reclaimed only at zero references, and
// cancellable blocking acquisition for committed execution openers.
package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
)

// assertRegistryState asserts the entry's settled state directly: waiting is
// reserved for actual asynchronous waiter registration.
func assertRegistryState(t *testing.T, a *artifactIntervals, key string, wantEntries, wantRefs int, wantHeld bool) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	refs, held := 0, false
	if entry, ok := a.entries[key]; ok {
		refs, held = entry.refs, entry.held
	}
	if len(a.entries) != wantEntries || refs != wantRefs || held != wantHeld {
		t.Fatalf("registry state for %q = (%d entries, %d refs, held %v), want (%d entries, %d refs, held %v)",
			key, len(a.entries), refs, held, wantEntries, wantRefs, wantHeld)
	}
}

// awaitRegistryState polls the owned state until an asynchronously
// registering waiter has landed on the held entry.
func awaitRegistryState(t *testing.T, a *artifactIntervals, key string, wantRefs int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a.mu.Lock()
		entry := a.entries[key]
		refs, held := 0, false
		if entry != nil {
			refs, held = entry.refs, entry.held
		}
		a.mu.Unlock()
		if held && refs == wantRefs {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the held entry for %q never reached %d references", key, wantRefs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestArtifactIntervalRegistrySingleHolderAndSharedEntry proves the ordering
// row: a blocking waiter joins the held entry, a releasing holder hands the
// token to the waiter, a later contender conflicts against the same entry, and
// the key never splits into two entries.
func TestArtifactIntervalRegistrySingleHolderAndSharedEntry(t *testing.T) {
	a := newArtifactIntervals()
	key := "code/session"
	releaseHolder, err := a.acquireExecution(context.Background(), key)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	assertRegistryState(t, a, key, 1, 1, true)

	type waiterResult struct {
		release func()
		err     error
	}
	done := make(chan waiterResult, 1)
	go func() {
		release, err := a.acquireExecution(context.Background(), key)
		done <- waiterResult{release: release, err: err}
	}()
	awaitRegistryState(t, a, key, 2) // the waiter registered on the same entry

	releaseHolder()
	var releaseWaiter func()
	select {
	case res := <-done:
		if res.err != nil { // an unexpected waiter failure is asserted, never ignored
			t.Fatalf("waiter acquire failed: %v", res.err)
		}
		releaseWaiter = res.release
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter never won the released interval")
	}

	if _, err := a.acquireTry(key); !errors.Is(err, harness.ErrConflict) {
		t.Fatalf("contender acquireTry = %v, want the conflict class", err)
	}
	assertRegistryState(t, a, key, 1, 1, true) // the contender's failed attempt dropped its one reference

	releaseWaiter()
	assertRegistryState(t, a, key, 0, 0, false)
}

// TestArtifactIntervalRegistryCanceledWaiterDropsReference proves the
// cancellation row: a canceled waiter drops its one reference without
// disturbing the holder.
func TestArtifactIntervalRegistryCanceledWaiterDropsReference(t *testing.T) {
	a := newArtifactIntervals()
	key := "code/session"
	releaseHolder, err := a.acquireExecution(context.Background(), key)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	assertRegistryState(t, a, key, 1, 1, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.acquireExecution(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire = %v, want context.Canceled", err)
	}
	assertRegistryState(t, a, key, 1, 1, true) // the canceled attempt dropped its one reference
	releaseHolder()
}

// TestArtifactIntervalRegistryNonblockingContenderDropsReference proves the
// transient-contender row: a refused nonblocking attempt drops its one
// reference and leaves the held entry to its holder.
func TestArtifactIntervalRegistryNonblockingContenderDropsReference(t *testing.T) {
	a := newArtifactIntervals()
	key := "code/session"
	releaseHolder, err := a.acquireExecution(context.Background(), key)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := a.acquireTry(key); !errors.Is(err, harness.ErrConflict) {
		t.Fatalf("contender acquireTry = %v, want the conflict class", err)
	}
	assertRegistryState(t, a, key, 1, 1, true) // only the holder's reference remains
	releaseHolder()
}

// TestArtifactIntervalRegistryReclaimsAtZeroReferences proves the reclaim row:
// a released entry is removed from the registry, so the next acquirer starts a
// fresh free interval — held with its own one reference — instead of
// inheriting stale holder state.
func TestArtifactIntervalRegistryReclaimsAtZeroReferences(t *testing.T) {
	a := newArtifactIntervals()
	key := "code/session"
	release, err := a.acquireExecution(context.Background(), key)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	release()
	assertRegistryState(t, a, key, 0, 0, false)

	again, err := a.acquireExecution(context.Background(), key)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	assertRegistryState(t, a, key, 1, 1, true) // the fresh entry's own held/reference state
	again()
}

// TestArtifactIntervalRegistryCanceledWinnerDropsReference proves the
// winner-cancellation row: an acquisition of a FREE entry under an
// already-canceled context hands back the typed cancellation instead of a
// lease, leaves zero references and no entry, and never disturbs a later
// acquirer.
func TestArtifactIntervalRegistryCanceledWinnerDropsReference(t *testing.T) {
	a := newArtifactIntervals()
	key := "code/session"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release, err := a.acquireExecution(ctx, key)
	if release != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled winner returned a lease (%t) with err %v, want no lease and the typed cancellation", release != nil, err)
	}
	assertRegistryState(t, a, key, 0, 0, false)
	if _, err := a.acquireExecution(context.Background(), key); err != nil {
		t.Fatalf("later acquire: %v", err)
	}
}
