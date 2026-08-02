package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
)

// TestOwnerConfigReadsRaceFree is a race-detector regression for the owner's
// config and agents-config readers. a.cfg and a.agents are written under
// runtime.mu by applyReloadStateLocked, and the reader sites that run without
// that lock must take it around their reads. Run under -race: before the
// reads take the mutex, an unsynchronized access is reported as a data race;
// with the reads locked, the same triggers stay clean.
//
// The triggers drive reloads that actually write a.cfg. Selecting an
// already-live session performs no write and would prove nothing, which is
// why the reload runs while a turn executes on another session (Reload's
// idle gate looks at the current session only, so the current one stays
// idle).
func TestOwnerConfigReadsRaceFree(t *testing.T) {
	t.Run("trigger=reload_during_other_session_turn", func(t *testing.T) {
		server := newRaceModelServer(t)
		a, ctx := newRaceTurnAgent(t, server.URL+"/v1")

		firstID, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession first: %v", err)
		}
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession second: %v", err)
		}
		// The second session is current and never runs a turn, so Reload's
		// busy gate never fires; the turns run on the first session.
		first := a.sessions[firstID]
		seedUserTurns(t, first, 2)
		first.tokensMu.Lock()
		first.lastContextUsed = 5000
		first.tokensMu.Unlock()

		const turns = 30
		const reloads = 240
		var started atomic.Int64
		var reloaded atomic.Int64
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < turns; i++ {
				res, err := a.SubmitToSession(ctx, firstID, fmt.Sprintf("race turn %d", i))
				if err != nil {
					t.Errorf("SubmitToSession: %v", err)
					return
				}
				if !res.Started {
					t.Errorf("SubmitToSession turn %d enqueued instead of starting; queue=%d", i, len(res.Queue))
					return
				}
				started.Add(1)
				waitSessionIdle(t, a, firstID)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < reloads; i++ {
				if err := a.Reload(); err != nil {
					t.Logf("Reload: %v", err)
					continue
				}
				reloaded.Add(1)
			}
		}()
		wg.Wait()
		if started.Load() != turns {
			t.Fatalf("started turns = %d, want %d", started.Load(), turns)
		}
		if reloaded.Load() < reloads/2 {
			t.Fatalf("Reload succeeded %d times, want at least %d real cfg writes", reloaded.Load(), reloads/2)
		}
		waitSessionDrained(t, a, firstID)
	})

	// A turn submitted concurrently with a session reactivation has been
	// observed to produce overlapping loop runs on one session's loop: two
	// turn goroutines inside Loop.Run at once. Reactivation turned out to be
	// incidental — the mechanism is launchTurn's deferred busy clear firing
	// after a later turn has already claimed the unit, so concurrent submits
	// alone expose it. The trigger below forces that interleaving
	// deterministically instead of sampling for it: turn N is parked in its
	// model request while the test takes the session transcript's seqMu, so
	// the turn's final feedTranscript(endEv) — the last step before the
	// deferred cleanup — blocks; the test then claims turn N+1 while the
	// deferred is pending and releases the stall. The busy gate must survive
	// the deferred, or a later submit can launch a second Run on the same
	// loop.

	t.Run("trigger=submit_while_deferred_clear_pending", func(t *testing.T) {
		// Deterministic busy-gate regression: a claim sets unit.busy under
		// runtime.mu and it must stay set until that turn is over. launchTurn's
		// deferred cleanup once cleared busy unconditionally, so when it fired
		// after a later turn had claimed the unit it dropped the later turn's
		// gate. The deferred now clears only its own turn's state, keyed on
		// the per-turn context the unit holds.
		//
		// The model response is payload-only (a MessageExtra field, no
		// content, no usage): no TextDelta reaches the loop event queue while
		// seqMu is held, so the drainer stays idle and the flush ack completes
		// instead of deadlocking on the held transcript lock.
		releaseRun := make(chan struct{})
		releaseN1 := make(chan struct{})
		reqNSeen := make(chan struct{}, 1)
		reqN1Seen := make(chan struct{}, 1)
		var runOnce, n1Once sync.Once
		closeRun := func() { runOnce.Do(func() { close(releaseRun) }) }
		closeN1 := func() { n1Once.Do(func() { close(releaseN1) }) }
		t.Cleanup(closeRun)
		t.Cleanup(closeN1)

		var reqs atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Tools []map[string]any `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(body.Tools) == 0 {
				writeTextResponse(w, "summary")
				return
			}
			gate := releaseN1
			seen := reqN1Seen
			if reqs.Add(1) == 1 {
				gate = releaseRun
				seen = reqNSeen
			}
			select {
			case seen <- struct{}{}:
			default:
			}
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","reasoning":"stall"},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		t.Cleanup(server.Close)

		a, ctx := newRaceTurnAgent(t, server.URL+"/v1")
		id, err := a.NewSession("", "primary")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		// Turn N: submit and park its Run in the model request.
		resN, err := a.SubmitToSession(ctx, id, "turn N")
		if err != nil {
			t.Fatalf("SubmitToSession N: %v", err)
		}
		if !resN.Started {
			t.Fatalf("turn N enqueued instead of starting")
		}
		waitForSignal(t, reqNSeen, "turn N model request")

		// Hold the transcript feed lock while Run(N) is parked: the loop event
		// queues are empty, so nothing else contends on seqMu. When the gate
		// releases, Run(N) finishes, the turn-end section clears busy and
		// emits turn_end, and the goroutine parks at feedTranscript(endEv)
		// with the deferred cleanup pending.
		tr := a.transcriptForSessionID(id)
		tr.seqMu.Lock()
		seqMuHeld := true
		defer func() {
			if seqMuHeld {
				tr.seqMu.Unlock()
			}
		}()

		closeRun()
		waitBusyState(t, a, id, false) // turn N's turn-end section ran; deferred pending

		// Claim turn N+1 while turn N's deferred cleanup is still pending.
		var resN1 SubmitResult
		var errN1 error
		var subWg sync.WaitGroup
		subWg.Add(1)
		go func() {
			defer subWg.Done()
			resN1, errN1 = a.SubmitToSession(ctx, id, "turn N+1")
		}()
		waitBusyState(t, a, id, true) // N+1 claimed; submitter parked at feedTranscript(startEv)

		// Release the stall: the deferred now runs with N+1 holding the unit.
		tr.seqMu.Unlock()
		seqMuHeld = false

		// N+1's Run must be in flight (parked at the second gate) and busy
		// must stay set — the deferred must not have cleared N+1's gate.
		waitForSignal(t, reqN1Seen, "turn N+1 model request")
		assertBusyStaysSet(t, a, id, 500*time.Millisecond)

		closeN1()
		subWg.Wait()
		if errN1 != nil {
			t.Fatalf("SubmitToSession N+1: %v", errN1)
		}
		if !resN1.Started {
			t.Fatalf("turn N+1 enqueued instead of claiming the unit")
		}
		waitSessionDrained(t, a, id)
	})

	t.Run("trigger=sweep_during_reload", func(t *testing.T) {
		// runSweep is unexported; its production callers are Init's one-shot
		// sweep and the hourly ticker, neither waitable in a test, so the
		// sweep loop is driven directly while Reload writes a.cfg. The sweep
		// loop runs until the reload loop finishes so the reads stay inside
		// the race detector's window for the whole reload duration.
		server := newRaceModelServer(t)
		a, _ := newRaceTurnAgent(t, server.URL+"/v1")
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}

		const reloads = 240
		var sweeps atomic.Int64
		var reloaded atomic.Int64
		var stop atomic.Bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				a.runSweep()
				sweeps.Add(1)
				time.Sleep(2 * time.Millisecond)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < reloads; i++ {
				if err := a.Reload(); err != nil {
					t.Logf("Reload: %v", err)
					continue
				}
				reloaded.Add(1)
			}
			stop.Store(true)
		}()
		wg.Wait()
		if sweeps.Load() < 10 {
			t.Fatalf("sweep loop ran %d times, want at least 10", sweeps.Load())
		}
		if reloaded.Load() < reloads/2 {
			t.Fatalf("Reload succeeded %d times, want at least %d real cfg writes", reloaded.Load(), reloads/2)
		}
	})
}

// newRaceModelServer serves both loop roles of the race tests: the main model
// (tool-bearing request, completion with usage so auto-compaction stays
// engaged) and the summarizer (bare chat request).
func newRaceModelServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(body.Tools) == 0 {
			writeTextResponse(w, "compact summary")
			return
		}
		// A short per-request delay keeps the turn outside runtime.mu (between
		// the model-request checkpoint and the next lock section) long enough
		// for the race detector to observe a concurrent config write racing
		// the checkpoint's reads.
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

// newRaceTurnAgent builds an agent whose config enables auto-compaction at a
// threshold every turn crosses, so each model-request checkpoint reads both
// compaction fields and runs compactAtCheckpointForSession (reading the tools
// config) while a concurrent reload can be writing a.cfg.
func newRaceTurnAgent(t *testing.T, baseURL string) (*Agent, context.Context) {
	t.Helper()
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test")
	configPath := filepath.Join(lightcodeDir, "config.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": %q, "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "compaction": { "enabled": true, "threshold_pct": 0.01 },
  "default_model": "test/test-model"
}`, baseURL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAgentsTestConfig(t, configPath, `{"primary": {"model": "test/test-model"}}`)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	ctx := startEventOrderAgent(t, a, &eventCapture{})
	return a, ctx
}

// seedUserTurns appends n complete user-only turns to a live session's loop so
// the next real turn's first checkpoint has history before its own user
// message (activeStart > 1) and auto-compaction actually runs.
func seedUserTurns(t *testing.T, unit *session, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		turn := unit.store.BeginTurn()
		unit.lp.AppendUserMessage(turn, fmt.Sprintf("seed %d", i))
		if err := unit.store.MarkTurnComplete(turn); err != nil {
			t.Fatalf("MarkTurnComplete: %v", err)
		}
	}
}

// waitSessionIdle waits until the given session has no running turn and no
// queued items.
func waitSessionIdle(t *testing.T, a *Agent, sessionID string) {
	t.Helper()
	waitSessionState(t, a, sessionID, false)
}

// waitSessionDrained waits until the given session is idle with an empty
// queue.
func waitSessionDrained(t *testing.T, a *Agent, sessionID string) {
	t.Helper()
	waitSessionState(t, a, sessionID, true)
}

func waitSessionState(t *testing.T, a *Agent, sessionID string, requireEmptyQueue bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		busy, err := a.BusyForSession(sessionID)
		if err != nil {
			t.Fatalf("BusyForSession: %v", err)
		}
		queue, err := a.QueueSnapshotForSession(sessionID)
		if err != nil {
			t.Fatalf("QueueSnapshotForSession: %v", err)
		}
		if !busy && (!requireEmptyQueue || len(queue.Items) == 0) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session %s did not become idle", sessionID)
}

// waitForSignal waits until the given channel is signaled or the deadline
// passes. Used by the deterministic deferred-clear trigger to sequence the two
// gated model requests.
func waitForSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitBusyState polls a session's busy flag until it equals want.
func waitBusyState(t *testing.T, a *Agent, sessionID string, want bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		busy, err := a.BusyForSession(sessionID)
		if err != nil {
			t.Fatalf("BusyForSession: %v", err)
		}
		if busy == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %s busy = %v, want %v", sessionID, !want, want)
}

// assertBusyStaysSet polls the session's busy flag for the whole window and
// fails on the first false sample. Used to prove a claimed turn's busy gate
// survives a stale deferred cleanup: while the later turn is parked in its
// model request nothing legitimate can clear busy, so any false sample means
// the earlier turn's deferred cleanup dropped the later turn's gate.
func assertBusyStaysSet(t *testing.T, a *Agent, sessionID string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		busy, err := a.BusyForSession(sessionID)
		if err != nil {
			t.Fatalf("BusyForSession: %v", err)
		}
		if !busy {
			t.Fatalf("session %s busy dropped to false while its turn is in flight", sessionID)
		}
		time.Sleep(time.Millisecond)
	}
}
