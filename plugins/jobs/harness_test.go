package jobs

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
)

// TestStopJobReaperBarrierAndHarnessStopDelivery proves the ordered
// convergence between the capability, StopJob, the Harness stop, and the
// completion delivery, in one barrier-controlled flow:
//
//  1. With the reaper seam held, StopJob (driven on its own goroutine so its
//     return is directly observable) stays blocked past its SIGKILL
//     escalation — no callback has run.
//  2. Releasing the reaper completes reaping: StopJob RETURNS while the
//     callback gate is still closed, and the admitted exit callback runs up
//     to that gate.
//  3. Harness Stop — whose own StopJob sees the exited record — remains
//     blocked on the member's done channel until the gate releases and the
//     delivery finishes the member.
//  4. Releasing the callback lets the delivery finish the member and Harness
//     Stop converges.
//
// The harness context is canceled before the stops so the gated delivery
// takes the durable closed path, exactly as managed shutdown does.
func TestStopJobReaperBarrierAndHarnessStopDelivery(t *testing.T) {
	j := openJobs(t, t.TempDir())
	release := armTestReaper(t, j)

	hctx, hcancel := context.WithCancel(context.Background())
	defer hcancel()
	h, err := harness.New(hctx, harness.Dependencies{
		Storage: storage.NewMemory(),
		Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{}, errors.New("no preparation expected")
		},
		Jobs: j,
	})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	session, err := h.CreateSession(context.Background(), harness.CreateSessionRequest{
		Workspace: t.TempDir(), AgentType: "coder",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := session.Identity.SessionID

	jobID, err := j.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	callbackEntered := make(chan struct{})
	callbackGate := make(chan struct{})
	deliveryErr := make(chan error, 1)
	if err := h.StartJob(context.Background(), sid, jobID, func(_ context.Context, completionID string) error {
		return j.Start(StartRequest{
			JobID: jobID, SessionID: sid, Workspace: t.TempDir(),
			Command: "sleep 30", Env: os.Environ(),
			OnExit: func(ExitResult) {
				close(callbackEntered)
				<-callbackGate
				deliveryErr <- h.DeliverBackgroundCompletion(context.Background(), sid, completionID, "job completed")
			},
		})
	}); err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	// The parent stops being receivable: the gated delivery commits the
	// durable signal entry on the closed path, like managed shutdown.
	hcancel()

	// Phase 1: StopJob runs on its own goroutine so its return is observable.
	// With the reaper held it stays blocked past the 500ms SIGKILL
	// escalation, and no callback has run.
	stopJobDone := make(chan struct{})
	go func() {
		j.StopJob(sid, jobID)
		close(stopJobDone)
	}()
	select {
	case <-stopJobDone:
		t.Fatal("StopJob returned before reaping")
	case <-time.After(600 * time.Millisecond):
	}
	select {
	case <-callbackEntered:
		t.Fatal("the exit callback ran while the reaper barrier was armed")
	default:
	}

	// Phase 2: releasing the reaper completes reaping. StopJob RETURNS while
	// the callback gate is still closed (it has not been released anywhere
	// yet), and the admitted callback runs up to that gate.
	release()
	select {
	case <-stopJobDone:
	case <-time.After(15 * time.Second):
		t.Fatal("StopJob did not return after reaping completed")
	}
	select {
	case <-callbackEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("the exit callback did not run after reaping completed")
	}
	select {
	case <-callbackGate:
		t.Fatal("the callback gate was released before Harness Stop was observed blocked")
	default:
	}

	// Phase 3: Harness Stop runs now; its own StopJob call sees the exited
	// record and returns, then Stop waits on the member's done channel, which
	// the gated delivery has not closed yet.
	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop(context.Background(), sid) }()
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before the delivery finished the member: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Phase 4: the delivery finishes the member and Stop converges.
	close(callbackGate)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Stop did not converge after the delivery finished the member")
	}
	if err := <-deliveryErr; err != nil {
		t.Fatalf("DeliverBackgroundCompletion: %v", err)
	}
}
