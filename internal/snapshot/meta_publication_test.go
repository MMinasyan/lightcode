package snapshot

import (
	"sync"
	"testing"
)

// TestConcurrentSessionMetadataWritesStayComplete verifies that session
// meta.json stays complete and consistent under concurrent writers: a reader
// always observes complete JSON, and two writers updating distinct fields
// cannot clobber each other's update.
func TestConcurrentSessionMetadataWritesStayComplete(t *testing.T) {
	store, err := NewForSessionsRoot(t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("NewForSessionsRoot: %v", err)
	}
	if err := store.BeginNewSession(t.TempDir()); err != nil {
		t.Fatalf("BeginNewSession: %v", err)
	}

	const rounds = 40
	var writers sync.WaitGroup
	writers.Add(2)
	go func() {
		defer writers.Done()
		for i := 0; i < rounds; i++ {
			if err := store.SetModel("prov", "mod"); err != nil {
				t.Errorf("SetModel: %v", err)
				return
			}
		}
	}()
	go func() {
		defer writers.Done()
		for i := 0; i < rounds; i++ {
			if err := store.SetActiveAgentType("typ"); err != nil {
				t.Errorf("SetActiveAgentType: %v", err)
				return
			}
		}
	}()

	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := store.Meta(); err != nil {
				t.Errorf("Meta observed incomplete record during writes: %v", err)
				return
			}
		}
	}()

	writers.Wait()
	close(stop)
	<-readerDone

	meta, err := store.Meta()
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Provider != "prov" || meta.Model != "mod" || meta.ActiveAgentType != "typ" {
		t.Fatalf("distinct field update lost: %+v", meta)
	}
}
