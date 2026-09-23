package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
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
// starts one live job on a fresh Session, arming the seam to deliver the
// given terminal report when shutdown stops the job.
func startLiveJobRuntime(t *testing.T, content string) (*ownerEnv, *Runtime, harness.Storage, *stubStopper, string) {
	t.Helper()
	ctx := context.Background()
	e := newOwnerEnv(t)
	store := storage.NewMemory()
	stopper := &stubStopper{events: e.events, content: content}
	r, err := e.open(ctx, e.storagePlugin(store), jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, stopper))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	session, err := r.createSession(ctx, filepath.Join(e.home, "live-job-ws"), "solo")
	if err != nil {
		t.Fatalf("createSession: %v", err)
	}
	var completionID string
	if err := startJobThrough(r, session.Identity.SessionID, "0a1b2c3d", func(_ context.Context, id string) error {
		completionID = id // the job stays live: terminal delivery is still owed
		return nil
	}); err != nil {
		t.Fatalf("StartJob: %v", err)
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
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		return startJobThrough(r, session.Identity.SessionID, "0a1b2c3d", func(context.Context, string) error {
			*spawned = true
			return nil
		})
	}
	assertUnbound := func(t *testing.T, err error, spawned bool) {
		t.Helper()
		if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "no job stopper configured") {
			t.Fatalf("StartJob = %v, want the unbound-seam rejection", err)
		}
		if spawned {
			t.Fatal("spawn ran without a bound seam")
		}
	}
	closeOK := func(t *testing.T, r *Runtime) {
		t.Helper()
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	t.Run("absent export leaves the seam nil", func(t *testing.T) {
		e := newOwnerEnv(t)
		r, err := e.open(ctx, e.storagePlugin(storage.NewMemory()))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer closeOK(t, r)
		spawned := false
		assertUnbound(t, startFor(t, e, r, &spawned), spawned)
	})

	t.Run("binds the one exact declared type", func(t *testing.T) {
		e := newOwnerEnv(t)
		stopper := &stubStopper{events: e.events}
		r, err := e.open(ctx, e.storagePlugin(storage.NewMemory()), jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, stopper))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer closeOK(t, r)
		session, err := r.createSession(ctx, filepath.Join(e.home, "wiring-ws"), "solo")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		var completionID string
		err = startJobThrough(r, session.Identity.SessionID, "0a1b2c3d", func(_ context.Context, id string) error {
			completionID = id
			return errSpawn
		})
		// The bound seam admitted the call: spawn received the reserved
		// completion identity and its error returns unchanged (the
		// spawn-failure path finishes the member, so shutdown stops nothing).
		if !errors.Is(err, errSpawn) {
			t.Fatalf("StartJob = %v, want the spawn error through the bound seam", err)
		}
		if completionID == "" {
			t.Fatal("spawn received no completion identity")
		}
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
		if err != nil {
			t.Fatalf("open: %v", err)
		}
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
	_, r, store, stopper, sessionID := startLiveJobRuntime(t, "job stopped")

	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if stopper.deliverErr != nil {
		t.Fatalf("stopped job delivery: %v", stopper.deliverErr)
	}

	entries, err := store.ReadEntries(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
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
		if err := json.Unmarshal(entry.Payload, &wire); err != nil {
			t.Fatalf("decode signal payload: %v", err)
		}
		if wire.Signal != "background_completion" {
			continue
		}
		found = true
		if wire.Content != "job stopped" {
			t.Fatalf("completion signal content = %q, want the delivered %q", wire.Content, "job stopped")
		}
		if wire.RelatedMember == nil || wire.RelatedMember.Kind != "job" || wire.RelatedMember.ID != "0a1b2c3d" {
			t.Fatalf("completion signal member = %+v, want the job member 0a1b2c3d", wire.RelatedMember)
		}
	}
	if !found {
		t.Fatal("shutdown with a live job recorded no background_completion signal entry")
	}
}

// TestRuntimeJobStopperDisposesBeforeStorage pins the disposal order of one
// managed shutdown: the stop runs while both the job-stop capability and its
// storage are still open (the delivery succeeded), and the capability's scope
// Close runs before the storage Close.
func TestRuntimeJobStopperDisposesBeforeStorage(t *testing.T) {
	ctx := context.Background()
	e, r, _, stopper, _ := startLiveJobRuntime(t, "job stopped")

	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if stopper.deliverErr != nil {
		t.Fatalf("stopped job delivery: %v", stopper.deliverErr)
	}
	events := e.events.all()
	if !orderedSubset(events, "stopjob", "close:jobs", "close:core") {
		t.Fatalf("shutdown events = %v, want the stop, then the jobs capability close, then the storage close", events)
	}
}
