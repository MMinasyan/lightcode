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

	// trigger=submit_while_deferred_clear_pending was deleted: the step-13 move (commit feed inside the busy-clear runtime.mu section) closed its window — the feed was the only parkable point between the busy clear and the deferred cleanup.

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
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home, NewMemoryEmbedder: disabledMemoryEmbedder})
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
	rt := a.ensureRuntime()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		// Read busy and the queue in one hold of the runtime lock: the
		// drainer's claim sets busy and empties the queue inside a single
		// critical section, so two separate polls could observe a stale idle
		// flag together with an already-emptied queue and return while the
		// re-drained turn is still on its way to its model request. The test
		// lives in the same package as the runtime, so it takes rt.mu
		// directly.
		rt.mu.Lock()
		unit, err := a.liveSessionLocked(sessionID)
		if err != nil {
			rt.mu.Unlock()
			t.Fatalf("resolve session %q: %v", sessionID, err)
		}
		busy := rt.busySnapshotLocked(unit)
		queue := rt.queueSnapshotLocked(unit)
		rt.mu.Unlock()
		if !busy && (!requireEmptyQueue || len(queue.Items) == 0) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session %s did not become idle", sessionID)
}

// waitForSignal waits until the given channel is signaled or the deadline
// passes. Used by the deterministic racing-turn triggers to sequence the two
// gated model requests.
func waitForSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
