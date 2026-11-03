package runtime

import (
	"context"
	"fmt"
	"sync"

	"github.com/MMinasyan/lightcode/harness"
)

// This file owns the private per-Session artifact interval: internal
// deferred-subsystem resource ownership, not a public lock manager, Core
// reservation, plugin capability or durable claim. The interval is keyed by
// the server-derived target Session artifact directory or a verified retained
// directory. Exactly one token holder is allowed; the token spans the file
// work, never the registry mutex.

// errArtifactBusy is the one held-interval refusal: a nonblocking contender
// — restore, committed deletion or a sweep candidate — reports contention
// instead of waiting.
var errArtifactBusy = fmt.Errorf("session artifact interval is busy: %w", harness.ErrConflict)

// artifactIntervals manages one token entry per artifact directory with its
// acquisition-reference count. The count includes holders, waiters and
// transient nonblocking contenders; it is registry lifetime, not permission
// for multiple holders. An entry is reclaimed only at zero references, so a
// releasing holder can never leave a waiter on an old entry while a newcomer
// creates a second entry for the same key.
type artifactIntervals struct {
	mu      sync.Mutex
	entries map[string]*artifactInterval
}

// artifactInterval is one keyed token: held marks the single holder, refs
// counts every live acquisition attempt, and changed wakes waiting acquirers
// when the holder releases.
type artifactInterval struct {
	refs    int
	held    bool
	changed chan struct{}
}

// newArtifactIntervals returns the empty registry.
func newArtifactIntervals() *artifactIntervals {
	return &artifactIntervals{entries: make(map[string]*artifactInterval)}
}

// acquireExecution leases the interval for one committed execution opener in
// the blocking mode: it waits for the current holder to release and reports
// caller cancellation. The caller checks its own cancellation after winning.
// The returned closure releases the lease exactly once.
func (a *artifactIntervals) acquireExecution(ctx context.Context, key string) (func(), error) {
	return a.acquire(ctx, key, true)
}

// acquireTry leases the interval nonblockingly: a held token refuses with
// the conflict class instead of waiting. The refused attempt drops its one
// reference before returning; the count keeps waiters alive so an entry with
// waiters is never reclaimed while they wait, and a contender arriving while
// the token is momentarily free may win it ahead of waking waiters — the
// single-holder rule is what the registry guarantees, not a waiter priority.
func (a *artifactIntervals) acquireTry(key string) (func(), error) {
	return a.acquire(context.Background(), key, false)
}

// acquire implements both acquisition modes. Every attempt increments the
// entry's reference count before it can wait or use the entry; a winning
// attempt holds the token, a failing or canceled attempt decrements its
// reference once, and a lease release decrements the holder's reference once.
func (a *artifactIntervals) acquire(ctx context.Context, key string, blocking bool) (func(), error) {
	if a == nil { // an isolated preparation owns no Runtime artifact registry
		return func() {}, nil
	}
	a.mu.Lock()
	entry, ok := a.entries[key]
	if !ok {
		entry = &artifactInterval{changed: make(chan struct{})}
		a.entries[key] = entry
	}
	entry.refs++
	won := !entry.held
	if won {
		entry.held = true
	}
	var changed chan struct{}
	if !won && blocking {
		changed = entry.changed
	}
	a.mu.Unlock()
	if won {
		return a.checkedLease(ctx, key)
	}
	if !blocking {
		a.dropReference(key)
		return nil, errArtifactBusy
	}
	for {
		select {
		case <-changed:
		case <-ctx.Done():
			a.dropReference(key)
			return nil, ctx.Err()
		}
		a.mu.Lock()
		if !entry.held {
			entry.held = true
			a.mu.Unlock()
			return a.checkedLease(ctx, key)
		}
		changed = entry.changed // a later release cycle replaced the wake channel
		a.mu.Unlock()
	}
}

// checkedLease hands the won token to the caller only under a live context:
// a winner that arrives canceled releases the token immediately, so a
// canceled attempt never owns the interval and never keeps a waiter parked.
func (a *artifactIntervals) checkedLease(ctx context.Context, key string) (func(), error) {
	release := a.releaseOnce(key)
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// releaseOnce wraps one release in a single-shot guard, so an owner error
// path can never double-release the same lease.
func (a *artifactIntervals) releaseOnce(key string) func() {
	var once sync.Once
	return func() { once.Do(func() { a.release(key) }) }
}

// release marks the holder's lease ended, wakes every waiter, and drops the
// holder's reference — reclaiming the entry when no attempt holds or waits on
// it anymore.
func (a *artifactIntervals) release(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[key]
	if !ok {
		return
	}
	entry.held = false
	close(entry.changed)
	entry.changed = make(chan struct{})
	entry.refs--
	if entry.refs == 0 {
		delete(a.entries, key)
	}
}

// dropReference cancels one failed or canceled attempt: the reference the
// attempt took before it could wait is dropped once, reclaiming the entry at
// zero references.
func (a *artifactIntervals) dropReference(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[key]
	if !ok {
		return
	}
	entry.refs--
	if entry.refs == 0 {
		delete(a.entries, key)
	}
}
