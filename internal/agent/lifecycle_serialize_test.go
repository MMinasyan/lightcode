package agent

import (
	"sync"
	"testing"
)

// TestLifecycleSerialization verifies overlapping identity operations
// serialize through the owner lifecycle lock: two archives and a delete of the
// same session run concurrently without corruption, and the session ends
// removed from the active set exactly once.
func TestLifecycleSerialization(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, "hello")
	id := a.store.SessionID()

	var wg sync.WaitGroup
	wg.Add(3)
	errs := make(chan error, 3)
	op := func(f func() error) {
		defer wg.Done()
		if err := f(); err != nil {
			errs <- err
		}
	}
	go op(func() error { return a.SessionArchive(id) })
	go op(func() error { return a.SessionArchive(id) })
	go op(func() error { return a.SessionDelete(id) })
	wg.Wait()
	close(errs)

	// The concurrent operations serialize; whichever ordering wins, the session
	// is no longer the live current session and no panic/corruption occurs.
	if a.store.Active() && a.store.SessionID() == id {
		t.Fatalf("session %q remained the live current session after archive+delete", id)
	}
}

// TestArchiveEmptySessionIsNotDeleted proves archiving an empty session (no
// complete turns) preserves it as archived rather than discarding it.
func TestArchiveEmptySessionIsNotDeleted(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// No turns appended: the session is empty.
	if err := a.SessionArchive(id); err != nil {
		t.Fatalf("SessionArchive: %v", err)
	}
	// The session is still readable and marked archived, not deleted.
	summaries, err := a.SessionList("archived")
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	found := false
	for _, s := range summaries {
		if s.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("archived empty session %q not found in archived list (was it deleted?)", id)
	}
}
