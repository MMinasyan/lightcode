package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
)

// TestSettingsEditDoesNotBlockSubmitDuringDiscoveryFetch proves that a
// settings edit's reload runs its provider-discovery network fetch outside
// runtime.mu: while the edit is stalled on a non-responding discovery
// endpoint, a concurrent submit must complete without waiting for the fetch.
//
// The assertion needs fault injection at the transport seam — the gated
// discovery endpoint the edit's warm-phase fetch targets, the same seam the
// package's other discovery tests use — because without a stalled endpoint
// the edit's fetch would finish before the concurrent submit could observe
// the lock hold. The gate also lets the test release the fetch once the
// submit is proven unblocked, so the edit completes normally. The concurrent
// submit is the probe for lock ownership: submit takes runtime.mu to claim
// its turn, so if the edit's fetch held the lock, the submit would not return
// until the gate was released.
func TestSettingsEditDoesNotBlockSubmitDuringDiscoveryFetch(t *testing.T) {
	gate := make(chan struct{})
	var closeGate sync.Once
	releaseGate := func() { closeGate.Do(func() { close(gate) }) }
	entered := make(chan struct{}, 1)
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-gate
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "hi")
	}))
	// Cleanup runs in LIFO order: release the gated fetch, then shut the owner
	// down, then close the servers (httptest Close waits for outstanding
	// requests, which would hang on the still-open gate otherwise).
	t.Cleanup(discoveryServer.Close)
	t.Cleanup(modelServer.Close)
	var a *Agent
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if a != nil {
			a.ShutdownOwner()
		}
	})
	t.Cleanup(releaseGate)

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
      "models": { "test-model": { "name": "Test Model", "context_window": 8192 } }
    },
    "disc": {
      "name": "Discovery Provider",
      "transport": { "base_url": %q, "api_key_env": "LIGHTCODE_DISC_KEY" },
      "discovery": true,
      "models": { "disc-model": { "name": "Disc Model", "context_window": 4096 } }
    }
  },
  "default_model": "test/test-model"
}`, modelServer.URL+"/v1", discoveryServer.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAgentsTestConfig(t, configPath, `{"primary": {"model": "test/test-model"}}`)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err = New(Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	a.Init(ctx)

	// The "disc" provider is discovery-enabled and due for a refresh (no cache
	// yet), but its key is set only now, after construction: New's catalog load
	// refreshes connected+due providers, which would consume the due state and
	// hide the fetch this test needs to observe. With the key set, the edit's
	// warm phase plans a fetch against the gated endpoint.
	t.Setenv("LIGHTCODE_DISC_KEY", "disc-key")
	editDone := make(chan error, 1)
	go func() {
		editDone <- a.SaveModel("test", "test-model", ModelConfigInput{ContextWindow: 9000})
	}()

	select {
	case <-entered:
		// The edit's discovery fetch is in flight.
	case <-time.After(5 * time.Second):
		t.Fatal("settings edit never reached the discovery fetch")
	}

	// A concurrent submit must complete while the fetch is still blocked on
	// the gate.
	submitDone := make(chan error, 1)
	go func() {
		_, err := a.Submit(ctx, "hello")
		submitDone <- err
	}()
	select {
	case err := <-submitDone:
		if err != nil {
			t.Fatalf("Submit while discovery fetch in flight: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked while the settings edit's discovery fetch held runtime.mu")
	}

	// Let the concurrent turn finish so the edit's phase-3 busy check (which
	// refuses while a turn is running) sees an idle session, then release the
	// fetch.
	waitUntilFullyDrained(t, a)
	releaseGate()
	if err := <-editDone; err != nil {
		t.Fatalf("SaveModel after releasing the discovery fetch: %v", err)
	}

	// The warm-phase fetch's result landed on disk: the discovery cache now
	// carries the provider fetched while the submit ran, and the locked reload
	// that followed read it from there.
	cache, _, _ := catalog.ReadDiscoveryCache(a.home)
	disc, ok := cache["disc"]
	if !ok {
		t.Fatal("settings edit's discovery refresh did not land in the cache")
	}
	if _, ok := disc.Models["disc-model"]; !ok {
		t.Fatalf("discovery cache for disc = %#v, want disc-model", disc.Models)
	}
}
