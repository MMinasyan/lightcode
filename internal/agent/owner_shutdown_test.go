package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestOwnerShutdown verifies owner shutdown closes turn admission,
// joins the in-flight turn (so it always completes rather than hanging on a
// cancelled turn), rejects new work, and is a shared, idempotent join.
func TestOwnerShutdown(t *testing.T) {
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	defer cancelSrv()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHangingResponse(w, srvCtx)
	}))
	defer server.Close()
	a := newEventOrderAgent(t, server.URL+"/v1")
	cap := &eventCapture{}
	ctx := startEventOrderAgent(t, a, cap)

	if _, err := a.Submit(ctx, "first"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitUntilBusy(t, a)

	// ShutdownOwner cancels the in-flight turn and joins it; it must complete.
	done := make(chan struct{})
	go func() {
		a.ShutdownOwner()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ShutdownOwner did not complete: the in-flight-turn join hung")
	}

	// Admission is closed: a new turn is rejected.
	if _, err := a.Submit(ctx, "after"); !errors.Is(err, errOwnerClosed) {
		t.Fatalf("Submit after shutdown = %v, want errOwnerClosed", err)
	}

	// Shutdown is a shared, idempotent join: a second call also returns promptly.
	done2 := make(chan struct{})
	go func() {
		a.ShutdownOwner()
		close(done2)
	}()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("second ShutdownOwner did not return: shared join broken")
	}
}
