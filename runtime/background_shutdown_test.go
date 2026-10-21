package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// The job-stop seam fixtures: one stub records stop invocations and, when
// armed with a live Harness and a captured completion identity, delivers the
// stopped job's terminal report — the plugin-side delivery shape the Runtime
// drives through Dependencies.Jobs.

type stubStopper struct {
	events       *traceLog
	h            *harness.Harness
	completionID string
	content      string
	deliverErr   error
}

func (s *stubStopper) StopJob(sessionID, jobID string) {
	s.events.add("stopjob")
	if s.h == nil {
		return
	}
	s.deliverErr = s.h.DeliverBackgroundCompletion(context.Background(), sessionID, s.completionID, s.content)
}

// jobStopperPlugin declares one export of the given declared type over the
// seam value, recording its construction and disposal order.
func jobStopperPlugin(e *ownerEnv, id, exportID string, scope ScopeKind, value any) Plugin {
	return Plugin{
		ID:       id,
		Scope:    scope,
		Provides: []CapabilitySpec{Spec[harness.JobStopper](exportID)},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			e.events.add("open:" + id)
			return Instance{
				Values: map[string]any{exportID: value},
				Close:  func() error { e.events.add("close:" + id); return nil },
			}, nil
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

// startLiveJobRuntime opens one Runtime with a delivering job-stop seam and
// starts one live job on a fresh Session over the given store, arming the
// seam to deliver the given terminal report when shutdown stops the job.
func startLiveJobRuntime(t *testing.T, store harness.Storage, content string) (*ownerEnv, *Runtime, harness.Storage, *stubStopper, string) {
	t.Helper()
	ctx := context.Background()
	e := newOwnerEnv(t)
	stopper := &stubStopper{events: e.events, content: content}
	r, err := e.open(ctx, e.storagePlugin(store), jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, stopper))
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

// TestRuntimeJobsCapabilityWiring pins the job-stopper export contract: the
// seam stays nil without an export or under a narrower declared type, binds
// exactly one Runtime-scoped export declared as harness.JobStopper, and
// rejects typed nils, ambiguity, and non-Runtime scopes before Harness
// publication.
func TestRuntimeJobsCapabilityWiring(t *testing.T) {
	ctx := context.Background()
	errSpawn := errors.New("spawn ran past the seam")

	startFor := func(t *testing.T, e *ownerEnv, r *Runtime, spawned *bool) error {
		t.Helper()
		session, err := r.createSession(ctx, filepath.Join(e.home, "wiring-ws"), "solo")
		testassert.NoError(t, err)
		return startJobThrough(r, session.Identity.SessionID, "0a1b2c3d", func(context.Context, string) error {
			*spawned = true
			return nil
		})
	}
	assertUnbound := func(t *testing.T, err error, spawned bool) {
		t.Helper()
		testassert.Require(t, errors.Is(err, harness.ErrInvalid) && strings.Contains(err.Error(), "no job stopper configured"), "StartJob = %v, want the unbound-seam rejection", err)
		testassert.Require(t, !spawned, "spawn ran without a bound seam")
	}
	closeOK := func(t *testing.T, r *Runtime) {
		t.Helper()
		testassert.NoError(t, r.Close(ctx))
	}

	t.Run("absent export leaves the seam nil", func(t *testing.T) {
		e := newOwnerEnv(t)
		r, err := e.open(ctx, e.storagePlugin(storage.NewMemory()))
		testassert.NoError(t, err)
		defer closeOK(t, r)
		spawned := false
		assertUnbound(t, startFor(t, e, r, &spawned), spawned)
	})

	t.Run("binds the one exact declared type", func(t *testing.T) {
		e := newOwnerEnv(t)
		stopper := &stubStopper{events: e.events}
		r, err := e.open(ctx, e.storagePlugin(storage.NewMemory()), jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, stopper))
		testassert.NoError(t, err)
		defer closeOK(t, r)
		session, err := r.createSession(ctx, filepath.Join(e.home, "wiring-ws"), "solo")
		testassert.NoError(t, err)
		var completionID string
		err = startJobThrough(r, session.Identity.SessionID, "0a1b2c3d", func(_ context.Context, id string) error {
			completionID = id
			return errSpawn
		})
		// The bound seam admitted the call: spawn received the reserved
		// completion identity and its error returns unchanged (the
		// spawn-failure path finishes the member, so shutdown stops nothing).
		testassert.Require(t, errors.Is(err, errSpawn), "StartJob = %v, want the spawn error through the bound seam", err)
		testassert.Require(t, completionID != "", "spawn received no completion identity")
	})

	t.Run("a narrower declared type leaves the seam nil", func(t *testing.T) {
		e := newOwnerEnv(t)
		narrow := Plugin{
			ID:       "narrow",
			Scope:    ScopeRuntime,
			Provides: []CapabilitySpec{Spec[*stubStopper]("narrow.stopper")},
			Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
				return Instance{Values: map[string]any{"narrow.stopper": &stubStopper{events: e.events}}}, nil
			},
		}
		r, err := e.open(ctx, e.storagePlugin(storage.NewMemory()), narrow)
		testassert.NoError(t, err)
		defer closeOK(t, r)
		spawned := false
		assertUnbound(t, startFor(t, e, r, &spawned), spawned)
	})

	t.Run("typed nil rejects before Harness publication", func(t *testing.T) {
		e := newOwnerEnv(t)
		var absent *stubStopper // typed nil: assignable to the seam, no verifiable instance
		wantFailedOpen(t, e, []Plugin{
			e.storagePlugin(storage.NewMemory()),
			jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, absent),
		}, ErrComposition)
		e.assertLockReleased(t)
	})

	t.Run("ambiguity rejects before Harness publication", func(t *testing.T) {
		e := newOwnerEnv(t)
		stopper := &stubStopper{events: e.events}
		wantFailedOpen(t, e, []Plugin{
			e.storagePlugin(storage.NewMemory()),
			jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, stopper),
			jobStopperPlugin(e, "jobs.second", "job-stopper.second", ScopeRuntime, stopper),
		}, ErrComposition)
		e.assertLockReleased(t)
	})

	t.Run("non-Runtime scope rejects before Harness publication", func(t *testing.T) {
		e := newOwnerEnv(t)
		wantFailedOpen(t, e, []Plugin{
			e.storagePlugin(storage.NewMemory()),
			jobStopperPlugin(e, "jobs", "job-stopper", ScopeWorkspace, &stubStopper{events: e.events}),
		}, ErrComposition)
		e.assertLockReleased(t)
	})
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

// TestRuntimeJobStopperDisposesBeforeStorage pins the disposal order of one
// managed shutdown. The unparked sibling proves the stop runs while both the
// job-stop capability and its storage are still open (the delivery succeeded)
// and the capability's scope Close runs before the storage Close. The parked
// row holds the stopped job's required terminal completion transaction at its
// real commit-return boundary over both stores: the durable completion is
// observable while Close stays joined behind it and no plugin scope Close has
// run; only after the release do the disposals record
// stopjob → close:jobs → close:core.
func TestRuntimeJobStopperDisposesBeforeStorage(t *testing.T) {
	ctx := context.Background()

	t.Run("the stop runs while the scopes are open", func(t *testing.T) {
		e, r, _, stopper, _ := startLiveJobRuntime(t, storage.NewMemory(), "job stopped")

		testassert.NoError(t, r.Close(ctx))
		testassert.Require(t, stopper.deliverErr == nil, "stopped job delivery: %v", stopper.deliverErr)
		events := e.events.all()
		testassert.Require(t, orderedSubset(events, "stopjob", "close:jobs", "close:core"), "shutdown events = %v, want the stop, then the jobs capability close, then the storage close", events)
	})

	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		t.Run("no scope closes before the held terminal commit", func(t *testing.T) {
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
			// The stop already ran, and no plugin scope Close has started
			// before the held terminal commit returns.
			testassert.Equal(t, eventsNamed(e.events.all(), "stopjob"), 1)
			{
				closes := eventsNamedList(e.events.all(), "close")
				testassert.Require(t, len(closes) == 0, "scope closes before the held terminal commit = %v, want none", closes)
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
			testassert.Require(t, orderedSubset(events, "stopjob", "close:jobs", "close:core"), "shutdown events = %v, want the stop, then the jobs capability close, then the storage close", events)
		})
	})
}
