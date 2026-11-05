package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/jobs"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// The jobs-service fixtures: one scripted service records stop invocations
// and, when armed with a live Harness and a captured completion identity,
// delivers the stopped job's terminal report. The owner's construction seam
// (newJobs) is pointed at it; no composition declaration supplies the service.

type stubStopper struct {
	events       *traceLog
	h            *harness.Harness
	completionID string
	content      string
	deliverErr   error
	stopped      []string
}

func (s *stubStopper) StopJob(sessionID, jobID string) {
	s.stopped = append(s.stopped, sessionID+"/"+jobID)
	if s.events != nil {
		s.events.add("stopjob")
	}
	if s.h == nil {
		return
	}
	s.deliverErr = s.h.DeliverBackgroundCompletion(context.Background(), sessionID, s.completionID, s.content)
}

// The remaining jobsService methods are inert for these rows: the stop seam
// and the shutdown close are what they drive.
func (s *stubStopper) Reserve() (string, error) { return "stubjob1", nil }
func (s *stubStopper) Abort(string)             {}
func (s *stubStopper) Start(jobs.StartRequest) error {
	return errors.New("stub jobs service starts no process")
}
func (s *stubStopper) Read(string, string) (string, error) {
	return "", errors.New("stub jobs service reads nothing")
}
func (s *stubStopper) Kill(string, string) error {
	return errors.New("stub jobs service kills nothing")
}
func (s *stubStopper) List(string) string   { return "" }
func (s *stubStopper) Live(string) []string { return nil }

func (s *stubStopper) Close() error {
	if s.events != nil {
		s.events.add("close:jobs")
	}
	return nil
}

// jobStopperExportPlugin declares one ordinary capability export of the
// seam's declared type: the composition treats it like any other value, and
// the owner's constructed service stays the seam.
func jobStopperExportPlugin(id string, value harness.JobStopper) Plugin {
	return Plugin{
		ID:       id,
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[harness.JobStopper](id + ".stopper")},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{id + ".stopper": value}}, nil
		},
	}
}

// startJobThrough admits one StartJob call through the Runtime's ordinary
// admitted-call gate.
func startJobThrough(r *Runtime, sessionID, jobID string, spawn func(context.Context, string) error) error {
	return r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		return h.StartJob(ctx, sessionID, jobID, spawn)
	})
}

// startLiveJobRuntime opens one Runtime whose constructed jobs service is a
// scripted stopper, and starts one live job on a fresh Session over the given
// store, arming the stopper to deliver the given terminal report when shutdown
// stops the job.
func startLiveJobRuntime(t *testing.T, store harness.Storage, content string) (*ownerEnv, *Runtime, harness.Storage, *stubStopper, string) {
	t.Helper()
	ctx := context.Background()
	e := newOwnerEnv(t)
	stopper := &stubStopper{events: e.events, content: content}
	e.fixtureStore(store)
	e.fixtureJobs(stopper)
	r, err := e.open(ctx)
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, filepath.Join(e.home, "live-job-ws"), "solo")
	testassert.NoError(t, err)
	var completionID string
	{
		err := startJobThrough(r, session.Identity.SessionID, "0a1b2c3d", func(_ context.Context, id string) error {
			completionID = id // the job stays live: terminal delivery is still owed
			return nil
		})
		testassert.NoError(t, err)
	}
	stopper.h = r.harness
	stopper.completionID = completionID
	return e, r, store, stopper, session.Identity.SessionID
}

// TestRuntimeJobsSeamIsDirectlyBound pins the job-stop seam's construction:
// direct construction binds it with no export at all — StartJob admits and
// hands the spawn the reserved completion identity, and its error returns
// unchanged — while a plugin export of the seam's own declared type is an
// ordinary capability that never becomes the seam.
func TestRuntimeJobsSeamIsDirectlyBound(t *testing.T) {
	ctx := context.Background()
	errSpawn := errors.New("spawn ran past the seam")

	t.Run("the seam is bound with no export at all", func(t *testing.T) {
		e := newOwnerEnv(t)
		e.fixtureStore(storage.NewMemory())
		e.fixtureJobs(&stubStopper{events: e.events})
		r, err := e.open(ctx)
		testassert.NoError(t, err)
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		session, err := r.createSession(ctx, filepath.Join(e.home, "wiring-ws"), "solo")
		testassert.NoError(t, err)
		spawned := false
		completionID := ""
		err = startJobThrough(r, session.Identity.SessionID, "0a1b2c3d", func(_ context.Context, id string) error {
			spawned = true
			completionID = id
			return errSpawn
		})
		// The bound seam admitted the call: spawn received the reserved
		// completion identity and its error returns unchanged (the
		// spawn-failure path finishes the member, so shutdown stops nothing).
		testassert.Require(t, errors.Is(err, errSpawn), "StartJob = %v, want the spawn error through the bound seam", err)
		testassert.Require(t, spawned && completionID != "", "spawn ran=%v completion identity %q, want the call admitted with its completion identity", spawned, completionID)
	})

	t.Run("an exported seam value is inert", func(t *testing.T) {
		e := newOwnerEnv(t)
		constructed := &stubStopper{events: e.events, content: "job stopped"}
		exported := &stubStopper{events: e.events}
		e.fixtureStore(storage.NewMemory())
		e.fixtureJobs(constructed)
		r, err := e.open(ctx,
			jobStopperExportPlugin("shadow", exported))
		testassert.NoError(t, err)
		session, err := r.createSession(ctx, filepath.Join(e.home, "wiring-ws"), "solo")
		testassert.NoError(t, err)
		var completionID string
		err = startJobThrough(r, session.Identity.SessionID, "0a1b2c3d", func(_ context.Context, id string) error {
			completionID = id
			return nil
		})
		testassert.NoError(t, err)
		constructed.h = r.harness
		constructed.completionID = completionID
		testassert.NoError(t, r.Close(ctx))
		{
			got := constructed.stopped
			testassert.Require(t, len(got) == 1 && got[0] == session.Identity.SessionID+"/0a1b2c3d", "constructed service stops = %v, want the live job", got)
		}
		testassert.Require(t, len(exported.stopped) == 0, "exported seam value stops = %v, want none", exported.stopped)
		testassert.Require(t, constructed.deliverErr == nil, "stopped job delivery: %v", constructed.deliverErr)
	})
}

// realJobsProbe wraps the real jobs manager: Start wraps the received exit
// callback with the two bounded barriers (before the original delivery, and
// after it returns but before the callback returns), and Close records its
// entry and return while delegating the real workgroup join.
type realJobsProbe struct {
	*jobs.Manager
	events *traceLog

	exitEntered   chan struct{}
	exitDelivered chan struct{}
	exitRelease   chan struct{}
	exitReturn    chan struct{}
	exits         atomic.Int64
	jobID         string
}

func (p *realJobsProbe) Start(req jobs.StartRequest) error {
	original := req.OnExit
	p.jobID = req.JobID
	req.OnExit = func(er jobs.ExitResult) {
		p.exits.Add(1)
		signalOnce(p.exitEntered)
		<-p.exitRelease
		if original != nil {
			original(er)
		}
		signalOnce(p.exitDelivered)
		<-p.exitReturn
	}
	return p.Manager.Start(req)
}

func (p *realJobsProbe) Close() error {
	p.events.add("jobclose:enter")
	err := p.Manager.Close()
	p.events.add("jobclose:return")
	return err
}

// signalOnce publishes one non-blocking arrival signal.
func signalOnce(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// awaitSignal waits for one bounded arrival signal.
func awaitSignal(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never arrived", what)
	}
}

// TestRuntimeShutdownJoinsRealJobsWorkGroup extends the owner shutdown rows to
// a REAL jobs manager and a real background shell process started through the
// real run_command tool over the ordinary admission path. The wrapped exit
// callback parks twice: before the original delivery (no durable completion,
// no service close, lock held) and after it returns (completion durable and
// the Harness member joined, while the owner's real jobs close stays joined on
// the manager workgroup with storage and lock still open). Releasing the
// callback converges the manager, then storage closes and the lock releases in
// order.
func TestRuntimeShutdownJoinsRealJobsWorkGroup(t *testing.T) {
	ctx := context.Background()
	e := newOwnerEnv(t)
	// lockHeld reports whether an independent contender can acquire the data
	// root: while the owner holds the lock, its open fails with ErrOwned before
	// any store or factory.
	lockHeld := func() bool {
		_, err := open(context.Background(), options{DataDir: e.dataDir, ConfigPath: e.configPath})
		return errors.Is(err, ErrOwned)
	}
	probe := &realJobsProbe{
		Manager:       jobs.New(e.dataDir),
		events:        e.events,
		exitEntered:   make(chan struct{}, 1),
		exitDelivered: make(chan struct{}, 1),
		exitRelease:   make(chan struct{}),
		exitReturn:    make(chan struct{}),
	}
	var releaseOnce, returnOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(probe.exitRelease) }) }
	releaseReturn := func() { returnOnce.Do(func() { close(probe.exitReturn) }) }
	t.Cleanup(func() { // a failed row must release both barriers and join the manager
		release()
		releaseReturn()
		_ = probe.Manager.Close()
	})

	store := storage.NewMemory()
	e.fixtureStoreClose(store, func() error { e.events.add("close:core"); return nil })
	e.fixtureJobs(probe)
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		return toolCallTurnEvents("call-1", "run_command", `{"command":"sleep 60","background":true}`)
	})

	workspace := filepath.Join(e.home, "real-job-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	r, err := e.open(ctx)
	testassert.NoError(t, err)
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	sessionID := session.Identity.SessionID
	submitThroughRuntime(t, r, sessionID, "op-1", "start the background process")
	awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
	testassert.Require(t, probe.jobID != "", "the background start reserved no job identity")
	testassert.Require(t, len(completionsOf(store, sessionID)) == 0, "completions before shutdown = %v, want the live job still owing its report", completionsOf(store, sessionID))

	// Close while the real job is live: the stop reaps the process, then the
	// wrapped callback parks before its original delivery.
	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close(ctx) }()
	awaitSignal(t, probe.exitEntered, "the job exit callback's pre-delivery barrier")
	{
		testassert.Require(t, len(completionsOf(store, sessionID)) == 0, "completions at the pre-delivery barrier = %v, want none yet", completionsOf(store, sessionID))
		testassert.Require(t, eventsNamed(e.events.all(), "jobclose:enter") == 0 && eventsNamed(e.events.all(), "close:core") == 0, "service events at the pre-delivery barrier = %v, want no jobs or storage cleanup", e.events.all())
		testassert.Require(t, lockHeld(), "ownership lock at the pre-delivery barrier = free, want it held")
		select {
		case err := <-closeDone:
			t.Fatalf("Close returned before the job delivered its completion (%v)", err)
		default:
		}
	}

	// The delivery runs: the durable completion lands and the Harness member
	// finishes, so shutdown advances to the real jobs close — which stays
	// joined on the manager workgroup because the callback has not returned.
	release()
	awaitSignal(t, probe.exitDelivered, "the wrapped callback's original delivery return")
	deadline := time.Now().Add(10 * time.Second)
	for eventsNamed(e.events.all(), "jobclose:enter") == 0 {
		testassert.Require(t, !time.Now().After(deadline), "the owner's jobs close never entered after the member settled; events = %v", e.events.all())
		time.Sleep(5 * time.Millisecond)
	}
	{
		completions := completionsOf(store, sessionID)
		testassert.Require(t, len(completions) == 1 && completions[0].memberKind == "job" && completions[0].memberID == probe.jobID && completions[0].content != "", "completions after delivery = %+v, want the job %s with its report", completions, probe.jobID)
		testassert.Require(t, eventsNamed(e.events.all(), "jobclose:return") == 0 && eventsNamed(e.events.all(), "close:core") == 0, "service events after delivery = %v, want the jobs close still joined and no storage close", e.events.all())
		testassert.Require(t, lockHeld(), "ownership lock after delivery = free, want it still held")
		select {
		case err := <-closeDone:
			t.Fatalf("Close returned while the real jobs close was still joined on the manager workgroup (%v)", err)
		default:
		}
	}

	// The callback returns: the manager workgroup joins, the jobs close
	// returns, storage closes, and the lock releases in order.
	releaseReturn()
	select {
	case err := <-closeDone:
		testassert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Close never converged after the exit callback returned")
	}
	events := e.events.all()
	testassert.Require(t, orderedSubset(events, "jobclose:enter", "jobclose:return", "close:core"), "shutdown events = %v, want the jobs close entry, its return, then the storage close", events)
	testassert.Equal(t, probe.exits.Load(), int64(1))
	testassert.NoError(t, r.Close(ctx)) // repeat callers join the shared shutdown
	testassert.Equal(t, probe.exits.Load(), int64(1))
	// The ownership lock released last: an independent owner acquires the same
	// data root and closes cleanly.
	e.assertLockReleased(t)
}

// TestRuntimeShutdownDeliversLiveJobCompletion pins the managed-shutdown
// convergence: Close stops the live background group, whose stop delivers the
// job's completion, so the durable background_completion signal entry is
// recorded before the Harness wait has converged and shutdown returns.
func TestRuntimeShutdownDeliversLiveJobCompletion(t *testing.T) {
	ctx := context.Background()
	_, r, store, stopper, sessionID := startLiveJobRuntime(t, storage.NewMemory(), "job stopped")

	testassert.NoError(t, r.Close(ctx))
	testassert.Require(t, stopper.deliverErr == nil, "stopped job delivery: %v", stopper.deliverErr)

	entries, err := store.ReadEntries(ctx, sessionID, 0)
	testassert.NoError(t, err)
	found := false
	for _, entry := range entries {
		if entry.Kind != harness.EntrySignal {
			continue
		}
		var wire struct {
			Signal        string `json:"signal"`
			Content       string `json:"content"`
			RelatedMember *struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"related_member"`
		}
		testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
		if wire.Signal != "background_completion" {
			continue
		}
		found = true
		testassert.Require(t, wire.Content == "job stopped" && wire.RelatedMember != nil && wire.RelatedMember.Kind == "job" && wire.RelatedMember.ID == "0a1b2c3d", "got %v, want %v; completion signal member = %+v, want the job member 0a1b2c3d", wire.Content, "job stopped", wire.RelatedMember)
	}
	testassert.Require(t, found, "shutdown with a live job recorded no background_completion signal entry")
}

// TestRuntimeShutdownStopsJobsBeforeStorageClose pins the disposal order of one
// managed shutdown. The unparked sibling proves the stop runs while both the
// jobs service and its storage are still open (the delivery succeeded) and the
// jobs service Close runs before the storage Close. The parked row holds the
// stopped job's required terminal completion transaction at its real
// commit-return boundary over both stores: the durable completion is observable
// while Close stays joined behind it and no service closer has run; only after
// the release do the disposals record stopjob → close:jobs → close:core.
func TestRuntimeShutdownStopsJobsBeforeStorageClose(t *testing.T) {
	ctx := context.Background()

	t.Run("the stop runs while the services are open", func(t *testing.T) {
		e, r, _, stopper, _ := startLiveJobRuntime(t, storage.NewMemory(), "job stopped")

		testassert.NoError(t, r.Close(ctx))
		testassert.Require(t, stopper.deliverErr == nil, "stopped job delivery: %v", stopper.deliverErr)
		events := e.events.all()
		testassert.Require(t, orderedSubset(events, "stopjob", "close:jobs", "close:core"), "shutdown events = %v, want the stop, then the jobs service close, then the storage close", events)
	})

	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		t.Run("no closer runs before the held terminal commit", func(t *testing.T) {
			gated := newGatedStore(store)
			e, r, _, stopper, sessionID := startLiveJobRuntime(t, gated, "job stopped")
			gated.armPostCommit(1)
			closeDone := make(chan error, 1)
			t.Cleanup(func() { // a failed row must never leave the joined Close parked
				gated.releaseAll()
				bound, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = r.Close(bound) // repeat callers join the shared shutdown
			})
			go func() { closeDone <- r.Close(ctx) }()

			select {
			case <-gated.parked:
			case <-time.After(10 * time.Second):
				t.Fatal("the stopped job's terminal completion transaction never committed")
			}

			// The committed completion is durable at the frozen return
			// boundary, while shutdown is still joined behind it.
			completions := completionsOf(gated, sessionID)
			testassert.Require(t, len(completions) == 1 && completions[0].memberKind == "job" && completions[0].memberID == "0a1b2c3d" && completions[0].content == "job stopped", "committed completion while held = %+v, want the job 0a1b2c3d with its report", completions)
			select {
			case err := <-closeDone:
				t.Fatalf("Close returned while the terminal completion transaction was still held (%v)", err)
			default:
			}
			// The stop already ran, and no service closer has started before
			// the held terminal commit returns.
			testassert.Equal(t, eventsNamed(e.events.all(), "stopjob"), 1)
			{
				closes := eventsNamedList(e.events.all(), "close")
				testassert.Require(t, len(closes) == 0, "service closes before the held terminal commit = %v, want none", closes)
			}

			gated.releaseGate()
			select {
			case err := <-closeDone:
				testassert.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("Close never converged after the terminal commit was released")
			}
			testassert.Require(t, stopper.deliverErr == nil, "stopped job delivery: %v", stopper.deliverErr)
			events := e.events.all()
			testassert.Require(t, orderedSubset(events, "stopjob", "close:jobs", "close:core"), "shutdown events = %v, want the stop, then the jobs service close, then the storage close", events)
		})
	})
}
