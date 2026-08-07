package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// TestOwnerGlobalSessionIDNamespace proves session ids are unique across every
// project an owner can see: the mint scans both the visible sessions
// namespaces and the staged candidates of every project, retries on a
// collision without entering the colliding directory, and serializes the
// reservation across concurrent creations in the same and different projects.
func TestOwnerGlobalSessionIDNamespace(t *testing.T) {
	t.Run("dead session directory is not reused or rewritten", func(t *testing.T) {
		projectsRoot := t.TempDir()
		projectID := "p-dead"
		sessionsRoot := filepath.Join(projectsRoot, projectID, "sessions")
		store, err := NewForSessionsRoot(sessionsRoot, projectsRoot, projectID)
		if err != nil {
			t.Fatal(err)
		}

		// A closed session holds no flock, so the only thing standing between a
		// fresh mint and this directory is the collision scan. Pre-create it with
		// stale turns, as a dead session leaves behind.
		deadID := "deadbeef"
		deadDir := filepath.Join(sessionsRoot, deadID)
		if err := os.MkdirAll(filepath.Join(deadDir, "turns", "1"), 0o700); err != nil {
			t.Fatal(err)
		}
		stale := []byte("{\"role\":\"user\",\"content\":\"stale\"}\n")
		if err := os.WriteFile(filepath.Join(deadDir, "turns", "1", "messages.jsonl"), stale, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deadDir, "turns", "1", "complete"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		deadMeta := []byte("{\"id\":\"deadbeef\",\"state\":\"active\"}\n")
		if err := os.WriteFile(filepath.Join(deadDir, "meta.json"), deadMeta, 0o600); err != nil {
			t.Fatal(err)
		}

		origMint := mintSessionIDFunc
		defer func() { mintSessionIDFunc = origMint }()
		calls := 0
		mintSessionIDFunc = func() (string, error) {
			calls++
			if calls == 1 {
				return deadID, nil // force the collision with the dead directory
			}
			return "fresh000", nil
		}

		if err := store.BeginNewSession(t.TempDir()); err != nil {
			t.Fatalf("BeginNewSession: %v", err)
		}
		defer func() { _, _ = store.Close() }()
		if store.SessionID() == deadID {
			t.Fatalf("mint reused the dead session id %q", deadID)
		}
		if store.SessionID() != "fresh000" {
			t.Fatalf("mint id = %q, want fresh000", store.SessionID())
		}
		gotStale, err := os.ReadFile(filepath.Join(deadDir, "turns", "1", "messages.jsonl"))
		if err != nil {
			t.Fatalf("read stale turns: %v", err)
		}
		if string(gotStale) != string(stale) {
			t.Fatalf("stale turns rewritten: got %q, want %q", gotStale, stale)
		}
		gotMeta, err := os.ReadFile(filepath.Join(deadDir, "meta.json"))
		if err != nil {
			t.Fatalf("read dead meta: %v", err)
		}
		if string(gotMeta) != string(deadMeta) {
			t.Fatalf("dead session meta rewritten: got %q, want %q", gotMeta, deadMeta)
		}
	})

	t.Run("concurrent creations in two projects get distinct ids", func(t *testing.T) {
		projectsRoot := t.TempDir()
		// Every mint's first draw collides with the single reserved id, so the
		// retry path is exercised under concurrency: without the mint mutex two
		// mints could scan before either reserved directory exists and draw the
		// same id.
		origMint := mintSessionIDFunc
		defer func() { mintSessionIDFunc = origMint }()
		draws := 0
		var drawMu sync.Mutex
		mintSessionIDFunc = func() (string, error) {
			drawMu.Lock()
			defer drawMu.Unlock()
			draws++
			if draws <= 4 {
				return "collide", nil
			}
			return fmt.Sprintf("uniq%05d", draws), nil
		}

		const workers = 4
		var wg sync.WaitGroup
		var mu sync.Mutex
		ids := make(map[string]struct{}, workers)
		var firstErr error
		for i := 0; i < workers; i++ {
			projectID := fmt.Sprintf("p-conc-%d", i)
			store, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectID, "sessions"), projectsRoot, projectID)
			if err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			// The stores stay open (no Close): like production children, the
			// reserved directories must remain on disk while other mints run —
			// Close would discard the empty sessions and remove the reservation.
			go func(store *Store) {
				defer wg.Done()
				if err := store.BeginChildSession(t.TempDir(), "parent"); err != nil {
					mu.Lock()
					firstErr = err
					mu.Unlock()
					return
				}
				mu.Lock()
				ids[store.SessionID()] = struct{}{}
				mu.Unlock()
			}(store)
		}
		wg.Wait()
		if firstErr != nil {
			t.Fatalf("concurrent child creation across projects: %v", firstErr)
		}
		if len(ids) != workers {
			t.Fatalf("got %d distinct ids, want %d: %v", len(ids), workers, ids)
		}
	})

	t.Run("concurrent children in one project get distinct ids", func(t *testing.T) {
		projectsRoot := t.TempDir()
		projectID := "p-same"
		sessionsRoot := filepath.Join(projectsRoot, projectID, "sessions")
		// Task-child creation takes no owner lock; only the mint mutex
		// serializes these reservations.
		origMint := mintSessionIDFunc
		defer func() { mintSessionIDFunc = origMint }()
		draws := 0
		var drawMu sync.Mutex
		mintSessionIDFunc = func() (string, error) {
			drawMu.Lock()
			defer drawMu.Unlock()
			draws++
			if draws <= 4 {
				return "collide", nil
			}
			return fmt.Sprintf("uniq%05d", draws), nil
		}

		const workers = 4
		var wg sync.WaitGroup
		var mu sync.Mutex
		ids := make(map[string]struct{}, workers)
		var firstErr error
		for i := 0; i < workers; i++ {
			store, err := NewForSessionsRoot(sessionsRoot, projectsRoot, projectID)
			if err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			// The stores stay open; see the two-project subtest for why.
			go func(store *Store) {
				defer wg.Done()
				if err := store.BeginChildSession(t.TempDir(), "parent"); err != nil {
					mu.Lock()
					firstErr = err
					mu.Unlock()
					return
				}
				mu.Lock()
				ids[store.SessionID()] = struct{}{}
				mu.Unlock()
			}(store)
		}
		wg.Wait()
		if firstErr != nil {
			t.Fatalf("concurrent child creation in one project: %v", firstErr)
		}
		if len(ids) != workers {
			t.Fatalf("got %d distinct ids, want %d: %v", len(ids), workers, ids)
		}
	})

	t.Run("staged candidate blocks a child mint in another project", func(t *testing.T) {
		projectsRoot := t.TempDir()
		projectA := "p-a"
		projectB := "p-b"
		storeA, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectA, "sessions"), projectsRoot, projectA)
		if err != nil {
			t.Fatal(err)
		}
		storeB, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectB, "sessions"), projectsRoot, projectB)
		if err != nil {
			t.Fatal(err)
		}

		origMint := mintSessionIDFunc
		defer func() { mintSessionIDFunc = origMint }()
		origHook := mintPublishHook
		defer func() { mintPublishHook = origHook }()

		const stagedID = "staged01"
		const childID = "child002"
		calls := 0
		mintSessionIDFunc = func() (string, error) {
			calls++
			if calls == 1 {
				return stagedID, nil // the staged root's mint
			}
			if calls == 2 {
				return stagedID, nil // the child's first draw must collide
			}
			return childID, nil
		}

		// Hold the staged candidate unpublished between its mint and its
		// publish, the window a visible-only scan cannot see into.
		hookFired := make(chan struct{})
		release := make(chan struct{})
		mintPublishHook = func() {
			close(hookFired)
			<-release
		}

		publishDone := make(chan error, 1)
		go func() {
			prepared, err := storeA.PrepareStagedNewSession(t.TempDir())
			if err != nil {
				publishDone <- err
				return
			}
			publishDone <- storeA.PublishPreparedSession(prepared)
		}()
		<-hookFired // the staged id is reserved on disk under .staging

		if err := storeB.BeginChildSession(t.TempDir(), "parent"); err != nil {
			t.Fatalf("child in project B: %v", err)
		}
		defer func() { _, _ = storeB.Close() }()
		if storeB.SessionID() == stagedID {
			t.Fatalf("child drew the unpublished staged id %q", stagedID)
		}
		if storeB.SessionID() != childID {
			t.Fatalf("child id = %q, want %q", storeB.SessionID(), childID)
		}

		close(release)
		if err := <-publishDone; err != nil {
			t.Fatalf("publish staged: %v", err)
		}
		defer func() { _, _ = storeA.Close() }()
		if _, err := os.Stat(filepath.Join(storeA.Root(), stagedID, "meta.json")); err != nil {
			t.Fatalf("staged session not published: %v", err)
		}
	})
}

// TestMintRedrawsOnLostClaim proves a mint whose first draw loses the claim
// to another live process redraws with a fresh id instead of aborting: a lost
// claim on a freshly drawn id is the same event as the scan collision, which
// already retries.
func TestMintRedrawsOnLostClaim(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-redraw"
	store, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectID, "sessions"), projectsRoot, projectID)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the claim on the first drawn id, as another live process would.
	heldID := "redraw01"
	held, ok, err := AcquireSessionClaim(projectsRoot, projectID, heldID)
	if err != nil || !ok {
		t.Fatalf("pre-hold claim on %s: held=%v err=%v", heldID, ok, err)
	}
	defer func() { _ = held.Release() }()

	origMint := mintSessionIDFunc
	defer func() { mintSessionIDFunc = origMint }()
	draws := 0
	mintSessionIDFunc = func() (string, error) {
		draws++
		if draws == 1 {
			return heldID, nil // the contended draw
		}
		return "redraw02", nil
	}

	id, err := store.mintReservedSessionID(t.TempDir(), SessionMeta{}, true)
	if err != nil {
		t.Fatalf("mint after a lost claim = %v, want a later draw to succeed", err)
	}
	if id != "redraw02" {
		t.Fatalf("mint id = %q, want redraw02", id)
	}
	if draws != 2 {
		t.Fatalf("draws = %d, want 2 (first draw contended)", draws)
	}
	if store.claim == nil {
		t.Fatal("mint did not hold the claim on the winning id")
	}
	if err := store.releaseClaimLocked(id); err != nil {
		t.Fatalf("release winning claim: %v", err)
	}
}

// TestMintFailsOnNonContendedClaimError proves a claim error that is not
// contention fails the mint after exactly one draw: only a lost claim means
// another process took the id and warrants a redraw; every other claim error
// is a real failure and must not be hidden by retrying.
func TestMintFailsOnNonContendedClaimError(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-claim-err"
	store, err := NewForSessionsRoot(filepath.Join(projectsRoot, projectID, "sessions"), projectsRoot, projectID)
	if err != nil {
		t.Fatal(err)
	}

	// A regular file where the claim lock directory must be created makes the
	// claim's directory creation fail with ENOTDIR — a filesystem claim error
	// that is not contention.
	lockDir := filepath.Join(projectsRoot, projectID, ".locks")
	if err := os.WriteFile(lockDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	origMint := mintSessionIDFunc
	defer func() { mintSessionIDFunc = origMint }()
	draws := 0
	mintSessionIDFunc = func() (string, error) {
		draws++
		return "claimfail01", nil
	}

	_, err = store.mintReservedSessionID(t.TempDir(), SessionMeta{}, true)
	if err == nil {
		t.Fatal("mint with a non-contended claim error = nil error")
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("mint error = %v, want the claim's filesystem cause", err)
	}
	if draws != 1 {
		t.Fatalf("draws = %d, want 1: a non-contended claim error must fail the mint, not redraw", draws)
	}
}

// TestMintExhaustsAttemptsWithTerminalError proves a mint whose every draw
// collides draws exactly the bounded number of ids and then returns the
// terminal error: the retry bound is a guarantee, not an accident of the
// collision distribution.
func TestMintExhaustsAttemptsWithTerminalError(t *testing.T) {
	projectsRoot := t.TempDir()
	projectID := "p-exhaust"
	sessionsRoot := filepath.Join(projectsRoot, projectID, "sessions")
	store, err := NewForSessionsRoot(sessionsRoot, projectsRoot, projectID)
	if err != nil {
		t.Fatal(err)
	}

	// Reserve the id every draw produces, so every attempt collides.
	if err := os.MkdirAll(filepath.Join(sessionsRoot, "collide"), 0o700); err != nil {
		t.Fatal(err)
	}

	origMint := mintSessionIDFunc
	defer func() { mintSessionIDFunc = origMint }()
	draws := 0
	mintSessionIDFunc = func() (string, error) {
		draws++
		return "collide", nil
	}

	_, err = store.mintReservedSessionID(t.TempDir(), SessionMeta{}, true)
	if err == nil {
		t.Fatal("mint with every draw colliding = nil error")
	}
	if draws != mintSessionIDMaxAttempts {
		t.Fatalf("draws = %d, want the %d-attempt bound", draws, mintSessionIDMaxAttempts)
	}
	want := fmt.Sprintf("snapshot: could not mint a unique session id in %d attempts (collision or contention)", mintSessionIDMaxAttempts)
	if err.Error() != want {
		t.Fatalf("mint error = %q, want %q", err, want)
	}
}
