package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
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

// discoveryEnabledConfig builds a config with a "test" model provider and a
// "disc" discovery-enabled provider whose base_url points at discBase. The
// disc key is intentionally not set so construction and Init never refresh:
// tests set LIGHTCODE_DISC_KEY after the agent exists to make the provider
// connected and due for the edit under test.
func discoveryEnabledConfig(t *testing.T, modelBase, discBase string) (string, *Agent, func()) {
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
}`, modelBase, discBase)
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
	return configPath, a, func() { t.Setenv("LIGHTCODE_DISC_KEY", "disc-key") }
}

// TestSetProviderConfigTransportEditQueriesCandidateEndpoint proves a
// transport edit's discovery targets the candidate transport B, never the old
// endpoint A: with A healthy and counting requests, the edit must perform zero
// A requests, fetch B exactly once, and commit B's metadata as authoritative.
// It fails against the old pre-edit warm path, which read the pre-edit config
// from disk, fetched A, and let that attempt suppress the B fetch.
func TestSetProviderConfigTransportEditQueriesCandidateEndpoint(t *testing.T) {
	var aRequests, bRequests atomic.Int32
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aRequests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"a-model","name":"A Model","context_window":1024}]}`))
	}))
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bRequests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"b-model","name":"B Model","context_window":2048}]}`))
	}))
	defer serverA.Close()
	defer serverB.Close()

	_, a, setDiscKey := discoveryEnabledConfig(t, "http://127.0.0.1:9/v1", serverA.URL+"/v1")
	setDiscKey()

	if err := a.SetProviderConfig("disc", ProviderConfigInput{BaseURL: serverB.URL + "/v1"}); err != nil {
		t.Fatalf("SetProviderConfig transport edit: %v", err)
	}
	if got := aRequests.Load(); got != 0 {
		t.Fatalf("old endpoint A received %d discovery requests, want 0", got)
	}
	if got := bRequests.Load(); got != 1 {
		t.Fatalf("candidate endpoint B received %d discovery requests, want 1", got)
	}
	cache, _, _ := catalog.ReadDiscoveryCache(a.home)
	disc, ok := cache["disc"]
	if !ok {
		t.Fatal("candidate discovery did not land in the cache")
	}
	if _, ok := disc.Models["b-model"]; !ok {
		t.Fatalf("cache models = %#v, want B's b-model authoritative", disc.Models)
	}
	if _, ok := disc.Models["a-model"]; ok {
		t.Fatalf("cache models = %#v, want no A model", disc.Models)
	}
	st := providerStatusByID(t, a.ProviderList(), "disc")
	if st.BaseURL != serverB.URL+"/v1" {
		t.Fatalf("effective base_url = %q, want %s", st.BaseURL, serverB.URL+"/v1")
	}
}

// TestSetProviderConfigCandidateDiscoveryDoesNotBlockSubmit proves both halves
// of the candidate-fetch contract: a submit admitted while the edit's
// discovery fetch is in flight completes without waiting for the HTTP call,
// and the edit's final owner-state recheck refuses once the fetch lands while
// the turn is still running — before any config or cache write. It fails
// against the old pre-edit warm path, which wrote the discovery attempt and
// cache during the fetch, so the refused edit leaves cache residue behind.
func TestSetProviderConfigCandidateDiscoveryDoesNotBlockSubmit(t *testing.T) {
	discoveryGate := make(chan struct{})
	var closeDiscoveryGate sync.Once
	releaseDiscoveryGate := func() { closeDiscoveryGate.Do(func() { close(discoveryGate) }) }
	entered := make(chan struct{}, 1)
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-discoveryGate
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	modelGate := make(chan struct{})
	var closeModelGate sync.Once
	releaseModelGate := func() { closeModelGate.Do(func() { close(modelGate) }) }
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-modelGate
		writeTextResponse(w, "hi")
	}))
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
	t.Cleanup(releaseDiscoveryGate)
	t.Cleanup(releaseModelGate)

	configPath, agent, setDiscKey := discoveryEnabledConfig(t, modelServer.URL+"/v1", discoveryServer.URL+"/v1")
	a = agent
	a.Init(ctx)
	setDiscKey()

	// The edit changes a mixed payload (base URL unchanged, name changed): a
	// committed edit would observably rename the provider, so the refusal's
	// no-write property is assertable from the config file.
	editDone := make(chan error, 1)
	go func() {
		editDone <- a.SetProviderConfig("disc", ProviderConfigInput{
			BaseURL: discoveryServer.URL + "/v1",
			Name:    "Edited Provider",
		})
	}()

	select {
	case <-entered:
		// The edit's candidate discovery fetch is in flight.
	case <-time.After(5 * time.Second):
		t.Fatal("provider edit never reached the candidate discovery fetch")
	}

	// A concurrent submit must complete while the fetch is still blocked on
	// the gate; the model endpoint is gated so the turn stays running.
	submitDone := make(chan error, 1)
	go func() {
		_, err := a.Submit(ctx, "hello")
		submitDone <- err
	}()
	select {
	case err := <-submitDone:
		if err != nil {
			t.Fatalf("Submit while candidate discovery fetch in flight: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked while the provider edit's discovery fetch held runtime.mu")
	}

	// The turn is still running, so the edit's final owner-state recheck must
	// refuse before any config or cache write.
	releaseDiscoveryGate()
	select {
	case err := <-editDone:
		if err == nil || !strings.Contains(err.Error(), "turn is running") {
			t.Fatalf("edit after release = %v, want the busy refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider edit never reached the final owner-state recheck")
	}

	cache, _, _ := catalog.ReadDiscoveryCache(a.home)
	if _, ok := cache["disc"]; ok {
		t.Fatalf("refused edit wrote the discovery cache: %#v", cache["disc"])
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Edited Provider") {
		t.Fatalf("refused edit wrote the config: %q", data)
	}
	releaseModelGate()
	waitUntilFullyDrained(t, a)
}

// TestSetProviderConfigConcurrentEditLastWriterWins proves two concurrent
// complete edits to one provider both succeed and the one whose commit lands
// last wins: the first edit's candidate fetch is held on a gate while the
// second edit commits, then the first commits after it, and the final config
// carries the first edit's complete root — never a torn merge.
func TestSetProviderConfigConcurrentEditLastWriterWins(t *testing.T) {
	gate := make(chan struct{})
	var closeGate sync.Once
	releaseGate := func() { closeGate.Do(func() { close(gate) }) }
	entered := make(chan struct{}, 1)
	gatedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-gate
		_, _ = w.Write([]byte(`{"data":[{"id":"g-model","name":"G Model","context_window":4096}]}`))
	}))
	plainServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"p-model","name":"P Model","context_window":2048}]}`))
	}))
	t.Cleanup(gatedServer.Close)
	t.Cleanup(plainServer.Close)
	var a *Agent
	t.Cleanup(func() {
		if a != nil {
			a.ShutdownOwner()
		}
	})
	t.Cleanup(releaseGate)

	// Both edits start from the gated endpoint; edit 1 targets it again (its
	// candidate fetch is the one held on the gate), edit 2 targets the plain
	// server and commits first.
	_, agent, setDiscKey := discoveryEnabledConfig(t, "http://127.0.0.1:9/v1", gatedServer.URL+"/v1")
	a = agent
	setDiscKey()

	edit1Done := make(chan error, 1)
	go func() {
		edit1Done <- a.SetProviderConfig("disc", ProviderConfigInput{BaseURL: gatedServer.URL + "/v1"})
	}()
	select {
	case <-entered:
		// Edit 1's candidate discovery fetch is in flight; its commit cannot
		// land until the gate opens.
	case <-time.After(5 * time.Second):
		t.Fatal("first edit never reached its candidate discovery fetch")
	}

	// The second edit commits completely while the first is still in flight.
	if err := a.SetProviderConfig("disc", ProviderConfigInput{BaseURL: plainServer.URL + "/v1"}); err != nil {
		t.Fatalf("second edit: %v", err)
	}

	releaseGate()
	select {
	case err := <-edit1Done:
		if err != nil {
			t.Fatalf("first edit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first edit never completed")
	}

	// The first edit's commit landed last, so its complete root wins; the
	// config stays one complete provider object, never a torn merge.
	st := providerStatusByID(t, a.ProviderList(), "disc")
	if st.BaseURL != gatedServer.URL+"/v1" {
		t.Fatalf("final base_url = %q, want the last-committed %s", st.BaseURL, gatedServer.URL+"/v1")
	}
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), gatedServer.URL+"/v1") {
		t.Fatalf("config does not carry the last commit: %q", data)
	}
	if strings.Contains(string(data), plainServer.URL+"/v1") {
		t.Fatalf("config carries a stale full root: %q", data)
	}
}

// TestSetProviderConfigNameEditUsesCandidateDiscovery proves a name-only
// payload still runs the candidate discovery path: the discovery fetch lands
// before the config write is published (the config directory is synced before
// the cache directory), the provider's /models endpoint is queried exactly
// once, and the fetched metadata lands in the cache. It fails against the old
// pre-edit warm path, which published the cache before the config write, and
// against any candidate implementation that classifies payloads by field.
func TestSetProviderConfigNameEditUsesCandidateDiscovery(t *testing.T) {
	var requests atomic.Int32
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	defer discoveryServer.Close()

	_, a, setDiscKey := discoveryEnabledConfig(t, "http://127.0.0.1:9/v1", discoveryServer.URL+"/v1")
	setDiscKey()

	var synced []string
	atomicfs.SyncDirFunc = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	if err := a.SetProviderConfig("disc", ProviderConfigInput{Name: "Renamed Provider"}); err != nil {
		t.Fatalf("SetProviderConfig name edit: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("discovery requests = %d, want exactly 1 for the candidate path", got)
	}
	// The config write is the first publication; the discovery cache write
	// follows it inside the same commit hold.
	if len(synced) == 0 || synced[0] != filepath.Dir(a.configPath) {
		t.Fatalf("first directory sync = %v, want the config directory (config before cache)", synced)
	}
	cache, _, _ := catalog.ReadDiscoveryCache(a.home)
	disc, ok := cache["disc"]
	if !ok {
		t.Fatal("candidate discovery did not land in the cache")
	}
	if _, ok := disc.Models["disc-model"]; !ok {
		t.Fatalf("cache models = %#v, want disc-model", disc.Models)
	}
	st := providerStatusByID(t, a.ProviderList(), "disc")
	if st.Name != "Renamed Provider" {
		t.Fatalf("effective name = %q, want Renamed Provider", st.Name)
	}
}

// TestResetProviderFieldTransportUsesCandidateEndpoint proves a transport
// field reset's discovery uses the candidate transport — the post-reset
// transport, without the overridden headers — not the pre-edit disk state.
// The reset removes a user header, so the single discovery request must not
// carry that header. It fails against the old pre-edit warm path, which
// fetched with the pre-edit transport (header still present).
func TestResetProviderFieldTransportUsesCandidateEndpoint(t *testing.T) {
	var sawOldHeader atomic.Bool
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Old") != "" {
			sawOldHeader.Store(true)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	defer discoveryServer.Close()

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
    "disc": {
      "name": "Discovery Provider",
      "transport": { "base_url": %q, "api_key_env": "LIGHTCODE_DISC_KEY", "headers": { "X-Old": "1" } },
      "discovery": true,
      "models": { "disc-model": { "name": "Disc Model", "context_window": 4096 } }
    }
  },
  "default_model": ""
}`, discoveryServer.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_DISC_KEY", "disc-key")

	if err := a.ResetProviderField("disc", "headers"); err != nil {
		t.Fatalf("ResetProviderField headers: %v", err)
	}
	if sawOldHeader.Load() {
		t.Fatal("discovery request carried the pre-reset X-Old header; the fetch did not use the candidate transport")
	}
	cache, _, _ := catalog.ReadDiscoveryCache(a.home)
	disc, ok := cache["disc"]
	if !ok {
		t.Fatal("transport reset's candidate discovery did not land in the cache")
	}
	if _, ok := disc.Models["disc-model"]; !ok {
		t.Fatalf("cache models = %#v, want disc-model", disc.Models)
	}
	view, err := a.GetProviderConfig("disc")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.UserHeaders) != 0 {
		t.Fatalf("headers after reset = %#v, want the override removed", view.UserHeaders)
	}
}

// TestResetProviderFieldTransportNoOpSkipsDiscovery proves a transport reset
// that removes nothing performs no discovery fetch, no config write, and no
// attempt or cache write: the candidate preparation returns immediately. It
// fails against the old warm path, which fetched and wrote the cache before
// discovering the reset was a no-op.
func TestResetProviderFieldTransportNoOpSkipsDiscovery(t *testing.T) {
	var requests atomic.Int32
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	defer discoveryServer.Close()

	_, a, setDiscKey := discoveryEnabledConfig(t, "http://127.0.0.1:9/v1", discoveryServer.URL+"/v1")
	setDiscKey()

	if err := a.ResetProviderField("disc", "headers"); err != nil {
		t.Fatalf("no-op ResetProviderField: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("no-op transport reset performed %d discovery requests, want 0", got)
	}
	cache, attempts, _ := catalog.ReadDiscoveryCache(a.home)
	if _, ok := cache["disc"]; ok {
		t.Fatalf("no-op transport reset wrote the discovery cache: %#v", cache["disc"])
	}
	if _, ok := attempts["disc"]; ok {
		t.Fatalf("no-op transport reset wrote a discovery attempt: %#v", attempts)
	}
}

// TestResetProviderFieldAPIKeyEnvDuringConnectRejected proves the final
// commit rechecks the api_key_env guard against the currently persisted raw
// override, including deletion: a reset prepared while the provider was
// disconnected must refuse when a concurrent ConnectProvider makes the live
// provider connected before the reset's commit lands — before any config or
// cache write. It fails against a commit guard that only compares the
// candidate value against the live catalog, which sees an empty candidate
// value and lets the reset silently delete the override the connect just
// activated.
func TestResetProviderFieldAPIKeyEnvDuringConnectRejected(t *testing.T) {
	gate := make(chan struct{})
	var closeGate sync.Once
	releaseGate := func() { closeGate.Do(func() { close(gate) }) }
	entered := make(chan struct{}, 1)
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-gate
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-test","name":"GPT Test","context_window":4096}]}`))
	}))
	t.Cleanup(discoveryServer.Close)
	var a *Agent
	t.Cleanup(func() {
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
	dotenvPath := filepath.Join(lightcodeDir, ".env")
	if err := os.WriteFile(dotenvPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	// The bundled env name OPENAI_API_KEY resolves (so the post-reset
	// candidate — which inherits it — is connected and fetches discovery),
	// while the user override LIGHTCODE_OPENAI_KEY does not (so the live
	// provider is disconnected and the reset prepares).
	t.Setenv("OPENAI_API_KEY", "bundled-key")
	// ConnectProvider persists the key with a plain os.Setenv; unset it at the
	// end so repeated runs in one process never see the provider connected at
	// construction (which would make New's catalog load refresh discovery).
	t.Cleanup(func() { _ = os.Unsetenv("LIGHTCODE_OPENAI_KEY") })
	configPath := filepath.Join(lightcodeDir, "config.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "openai": {
      "transport": { "base_url": %q, "api_key_env": "LIGHTCODE_OPENAI_KEY" }
    }
  },
  "default_model": ""
}`, discoveryServer.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err = New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home, Env: config.NewManagedEnvForTest(dotenvPath)})
	if err != nil {
		t.Fatal(err)
	}

	// The reset removes the user api_key_env override, so the candidate
	// inherits the bundled env name and is connected; candidate discovery runs
	// against the gated endpoint and holds the commit open.
	resetDone := make(chan error, 1)
	go func() {
		resetDone <- a.ResetProviderField("openai", "api_key_env")
	}()
	select {
	case <-entered:
		// The reset's candidate discovery fetch is in flight; its commit is
		// held until the gate opens.
	case <-time.After(5 * time.Second):
		t.Fatal("reset never reached its candidate discovery fetch")
	}

	// Connect the provider while the reset is in flight: the key lands in the
	// managed env under the current override name and the live catalog becomes
	// connected.
	if err := a.ConnectProvider("openai", "secret-key"); err != nil {
		t.Fatalf("ConnectProvider: %v", err)
	}

	releaseGate()
	select {
	case err := <-resetDone:
		if err == nil || !strings.Contains(err.Error(), "disconnect provider") {
			t.Fatalf("reset during connect = %v, want the disconnect refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reset never reached its final commit recheck")
	}

	// The refusal wrote nothing: the persisted override the connect activated
	// is intact, and no discovery cache entry exists.
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"api_key_env": "LIGHTCODE_OPENAI_KEY"`) {
		t.Fatalf("reset removed the persisted api_key_env override: %q", data)
	}
	cache, _, _ := catalog.ReadDiscoveryCache(a.home)
	if _, ok := cache["openai"]; ok {
		t.Fatalf("refused reset wrote the discovery cache: %#v", cache["openai"])
	}
}

// TestResetProviderFieldUnrelatedTransportFieldNotBlockedByAPIKeyEnv proves
// the commit guard compares raw api_key_env overrides, not merged/live state:
// an unrelated transport-field reset succeeds on a connected provider whose
// raw override is unchanged, and on a built-in that only inherits a bundled
// env name (no raw override on either side). A guard that compared the
// candidate override against the live catalog's merged env would reject both.
func TestResetProviderFieldUnrelatedTransportFieldNotBlockedByAPIKeyEnv(t *testing.T) {
	t.Run("custom_override_unchanged", func(t *testing.T) {
		home := t.TempDir()
		projectRoot := t.TempDir()
		lightcodeDir := filepath.Join(home, ".lightcode")
		if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(lightcodeDir, "config.json")
		configJSON := `{
  "providers": {
    "disc": {
      "name": "Discovery Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_DISC_KEY", "headers": { "X-Old": "1" } },
      "discovery": false,
      "models": { "disc-model": { "name": "Disc Model", "context_window": 4096 } }
    }
  },
  "default_model": ""
}`
		if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("LIGHTCODE_DISC_KEY", "disc-key")

		if err := a.ResetProviderField("disc", "headers"); err != nil {
			t.Fatalf("headers reset on a connected provider with an unchanged env override: %v", err)
		}
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		body := string(data)
		if !strings.Contains(body, `"api_key_env": "LIGHTCODE_DISC_KEY"`) {
			t.Fatalf("unrelated reset removed the env override: %q", body)
		}
		if strings.Contains(body, "X-Old") {
			t.Fatalf("headers override not reset: %q", body)
		}
		view, err := a.GetProviderConfig("disc")
		if err != nil {
			t.Fatal(err)
		}
		if len(view.UserHeaders) != 0 {
			t.Fatalf("effective headers after reset = %#v, want none", view.UserHeaders)
		}
	})

	t.Run("builtin_inherits_bundled_env", func(t *testing.T) {
		home := t.TempDir()
		projectRoot := t.TempDir()
		lightcodeDir := filepath.Join(home, ".lightcode")
		if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(lightcodeDir, "config.json")
		configJSON := `{
  "providers": {
    "openai": {
      "discovery": false,
      "transport": { "headers": { "X-Test": "1" } }
    }
  },
  "default_model": ""
}`
		if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
		if err != nil {
			t.Fatal(err)
		}
		// OPENAI_API_KEY is the bundled env name openai inherits; with no raw
		// override on either side the provider is connected, and the headers
		// reset must not trip the env guard.
		t.Setenv("OPENAI_API_KEY", "test-key")

		if err := a.ResetProviderField("openai", "headers"); err != nil {
			t.Fatalf("headers reset on a built-in inheriting a bundled env name: %v", err)
		}
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		body := string(data)
		if strings.Contains(body, "api_key_env") {
			t.Fatalf("unrelated reset added an env override: %q", body)
		}
		if strings.Contains(body, "X-Test") {
			t.Fatalf("headers override not reset: %q", body)
		}
	})
}
