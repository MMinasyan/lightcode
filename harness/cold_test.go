// Cold-process execution observation: a completed Operation, a mid-flight
// Interrupt and the canceled Harness's Wait all finish in a fresh child
// process without contacting an external transport.
package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// coldChildEnv switches this test binary into the child mode of the cold
// execution regression.
const coldChildEnv = "LIGHTCODE_COLD_CHILD"

// coldChildDone is the child's completion marker: the parent requires it so a
// child that ran no assertion cannot pass by exiting zero.
const coldChildDone = "cold child: done"

// refusedTransport fails every attempt after recording it, so the child can
// prove that no hidden external acquisition reached a transport.
type refusedTransport struct{ attempts atomic.Int64 }

func (f *refusedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.attempts.Add(1)
	return nil, errors.New("external transport is unavailable in this process")
}

// TestColdChildNoExternalIO runs one real Operation, a mid-flight Interrupt
// and a Wait in a fresh child process with a transport that refuses (and
// records) every attempt. All three must finish without contacting that
// transport.
func TestColdChildNoExternalIO(t *testing.T) {
	if os.Getenv(coldChildEnv) == "1" {
		runColdChild(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), coldChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cold child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), coldChildDone) {
		t.Fatalf("the cold child ran no completed work:\n%s", out)
	}
}

// runColdChild is the child body: a completed Operation, a parked Operation
// interrupted mid-flight, and the canceled Harness's Wait join.
func runColdChild(t *testing.T) {
	transport := &refusedTransport{}
	http.DefaultTransport = transport

	store := emptyStore(t)
	started := make(chan struct{})
	var requests atomic.Int64
	modelFn := func(ctx context.Context, _ model.Request) (model.Stream, error) {
		if requests.Add(1) == 1 {
			return completedTurnStream(), nil
		}
		close(started) // the second Operation parks inside its model attempt
		<-ctx.Done()
		return nil, ctx.Err()
	}
	h, cancel := newCancelableHarness(t, store, modelPrepared(modelFn), nil)
	session := createSession(t, h)
	watch := watchSettlements(store)

	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	watch.next()
	if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationSuccess {
		t.Fatalf("op-1 status = %s, want success", rec.State.Status)
	}
	if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "park"); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	<-started
	if err := h.Interrupt(context.Background(), session); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	watch.next()
	if rec := settledOperation(t, store, session, "op-2"); rec.State.Status != OperationInterruption {
		t.Fatalf("op-2 status = %s, want interruption", rec.State.Status)
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer waitCancel()
	if err := h.Wait(waitCtx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if attempts := transport.attempts.Load(); attempts != 0 {
		t.Fatalf("the cold child contacted the external transport %d time(s)", attempts)
	}
	fmt.Println(coldChildDone)
}
