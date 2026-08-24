package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/project"
)

// foreignLockHolderEnv selects the child half of every owner contention test
// that uses startForeignLockHolder and names the lock path the child holds.
const foreignLockHolderEnv = "LIGHTCODE_AGENT_FOREIGN_LOCK_HOLDER"

// startForeignLockHolder spawns a child of this test binary that acquires the
// given lock path and holds it until the parent closes its stdin. It is the
// repository's self-exec foreign-process flock pattern, so contention is
// exercised against a real separate process rather than a same-process open.
// The returned release closes the child's stdin and reaps it; the cleanup
// cancels and reaps it again (the second Wait is ignored).
func startForeignLockHolder(t *testing.T, testName, lockPath string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = append(os.Environ(), foreignLockHolderEnv+"="+lockPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("child stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("child stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start child: %v", err)
	}
	ready := make(chan struct{})
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				close(ready)
				return
			}
		}
	}()
	reap := func() error {
		_ = stdin.Close()
		err := cmd.Wait()
		<-scannerDone
		return err
	}
	fail := func() string {
		cancel()
		_ = reap()
		return stderr.String()
	}
	t.Cleanup(func() { cancel(); _ = reap() })
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatalf("child never held %s within %v: %v\n%s", lockPath, 30*time.Second, ctx.Err(), fail())
	}
	return func() { _ = reap() }
}

// foreignLockHolderChild acquires the lock path named by foreignLockHolderEnv
// and holds it until the parent closes stdin. Every owner contention test whose
// name is passed to startForeignLockHolder starts with this branch.
func foreignLockHolderChild() {
	if lockPath := os.Getenv(foreignLockHolderEnv); lockPath != "" {
		l, err := atomicfs.Acquire(lockPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		if err := l.Release(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		os.Exit(0)
	}
}

// TestConnectProviderEnvLockContentionReturnsPromptly proves owner env writes
// are one-attempt Try operations: with a foreign process holding the env leaf
// lock, ConnectProvider returns a retryable error promptly and mutates neither
// the .env file, the process env, nor the managed set. It fails against the
// pre-fix blocking env.Set, which parks on the foreign lock.
func TestConnectProviderEnvLockContentionReturnsPromptly(t *testing.T) {
	foreignLockHolderChild()
	keyEnv := "LIGHTCODE_CONNECT_CONTENDED"
	_ = os.Unsetenv(keyEnv)
	t.Cleanup(func() { _ = os.Unsetenv(keyEnv) })
	a := newProviderManagementAgent(t, fmt.Sprintf(`{
  "providers": {
    "test": {
      "name": "Test",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": %q },
      "discovery": false,
      "models": { "m": { "context_window": 1000 } }
    }
  }
}`, keyEnv))
	envLockPath := filepath.Join(filepath.Dir(a.ManagedEnv().Path()), ".locks", "env.lock")
	releaseHolder := startForeignLockHolder(t, "TestConnectProviderEnvLockContentionReturnsPromptly", envLockPath)
	defer releaseHolder()

	done := make(chan error, 1)
	go func() { done <- a.ConnectProvider("test", "secret") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "retry") {
			t.Fatalf("ConnectProvider under env contention = %v, want a retryable error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ConnectProvider blocked on the foreign env lock; owner env writes must be one-attempt Try")
	}
	if _, set := os.LookupEnv(keyEnv); set {
		t.Fatalf("process env %s was set under contention", keyEnv)
	}
	if a.ManagedEnv().IsManaged(keyEnv) {
		t.Fatal("key was added to the managed set under contention")
	}
	data, err := os.ReadFile(a.ManagedEnv().Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), keyEnv) {
		t.Fatalf(".env was mutated under contention: %q", data)
	}
}

// TestConnectProviderDiscoveryLockContentionReturnsBeforeEnvOrReload proves the
// ConnectProvider discovery-path final hold publishes the cache through a
// one-attempt Try: with a foreign process holding the provider's discovery lock
// after a real fetched discovery outcome, ConnectProvider returns a retryable
// error before env publication or reload, and no cache/attempt/env/reload-
// derived state is published. It fails against the pre-fix blocking
// WriteDiscoveryCache, which parks on the foreign lock.
func TestConnectProviderDiscoveryLockContentionReturnsBeforeEnvOrReload(t *testing.T) {
	foreignLockHolderChild()
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	t.Cleanup(discoveryServer.Close)
	keyEnv := "LIGHTCODE_CONNECT_DISC_PATH"
	_ = os.Unsetenv(keyEnv)
	t.Cleanup(func() { _ = os.Unsetenv(keyEnv) })
	a := newProviderManagementAgent(t, fmt.Sprintf(`{
  "providers": {
    "disc": {
      "name": "Disc",
      "transport": { "base_url": %q, "api_key_env": %q },
      "discovery": true,
      "models": {}
    }
  }
}`, discoveryServer.URL+"/v1", keyEnv))
	lockPath := filepath.Join(a.home, ".lightcode", "cache", "discovery", ".locks", "disc.lock")
	releaseHolder := startForeignLockHolder(t, "TestConnectProviderDiscoveryLockContentionReturnsBeforeEnvOrReload", lockPath)
	defer releaseHolder()
	cfgBefore := a.cfg
	catBefore := a.catalog
	warningsBefore := a.CurrentWarnings()

	done := make(chan error, 1)
	go func() { done <- a.ConnectProvider("disc", "secret") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "retry") {
			t.Fatalf("ConnectProvider under discovery contention = %v, want a retryable error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ConnectProvider blocked on the foreign discovery lock; the cache publication must be one-attempt Try")
	}
	// No cache or attempt publication.
	cache, _ := catalog.ReadDiscoveryCache(a.home)
	if _, ok := cache["disc"]; ok {
		t.Fatalf("contended ConnectProvider wrote the discovery cache: %#v", cache["disc"])
	}
	if record, ok := cache["disc"]; ok && !record.AttemptedAt.IsZero() {
		t.Fatalf("contended ConnectProvider wrote a discovery attempt: %#v", record)
	}
	// No env publication.
	if _, set := os.LookupEnv(keyEnv); set {
		t.Fatalf("process env %s was set under discovery contention", keyEnv)
	}
	if a.ManagedEnv().IsManaged(keyEnv) {
		t.Fatal("key was added to the managed set under discovery contention")
	}
	data, err := os.ReadFile(a.ManagedEnv().Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), keyEnv) {
		t.Fatalf(".env was mutated under discovery contention: %q", data)
	}
	// No reload-derived publication.
	if a.cfg != cfgBefore || a.catalog != catBefore {
		t.Fatal("contended ConnectProvider applied a reload")
	}
	if !reflect.DeepEqual(a.CurrentWarnings(), warningsBefore) {
		t.Fatalf("contended ConnectProvider published warnings: %#v", a.CurrentWarnings())
	}
}

// TestConnectProviderDiscoveryEnvContentionRetainsCache proves env contention
// after a committed cache on the ConnectProvider discovery path: the cache is
// retained, the env publication and reload are refused, and no reload-derived
// state changes. It fails against the pre-fix blocking env.Set, which parks on
// the foreign lock after the cache commit.
func TestConnectProviderDiscoveryEnvContentionRetainsCache(t *testing.T) {
	foreignLockHolderChild()
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	t.Cleanup(discoveryServer.Close)
	keyEnv := "LIGHTCODE_CONNECT_DISC_PATH"
	_ = os.Unsetenv(keyEnv)
	t.Cleanup(func() { _ = os.Unsetenv(keyEnv) })
	a := newProviderManagementAgent(t, fmt.Sprintf(`{
  "providers": {
    "disc": {
      "name": "Disc",
      "transport": { "base_url": %q, "api_key_env": %q },
      "discovery": true,
      "models": {}
    }
  }
}`, discoveryServer.URL+"/v1", keyEnv))
	envLockPath := filepath.Join(filepath.Dir(a.ManagedEnv().Path()), ".locks", "env.lock")
	releaseHolder := startForeignLockHolder(t, "TestConnectProviderDiscoveryEnvContentionRetainsCache", envLockPath)
	defer releaseHolder()
	cfgBefore := a.cfg
	catBefore := a.catalog
	warningsBefore := a.CurrentWarnings()

	done := make(chan error, 1)
	go func() { done <- a.ConnectProvider("disc", "secret") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "retry") {
			t.Fatalf("ConnectProvider under env contention = %v, want a retryable error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ConnectProvider blocked on the foreign env lock after the cache commit; the env write must be one-attempt Try")
	}
	// The committed cache is retained: the env contention does not roll it back.
	cache, _ := catalog.ReadDiscoveryCache(a.home)
	disc, ok := cache["disc"]
	if !ok {
		t.Fatal("committed discovery cache was rolled back by the env contention")
	}
	if _, ok := disc.Models["disc-model"]; !ok {
		t.Fatalf("cache models = %#v, want the fetched disc-model retained", disc.Models)
	}
	// No env publication.
	if _, set := os.LookupEnv(keyEnv); set {
		t.Fatalf("process env %s was set under env contention", keyEnv)
	}
	if a.ManagedEnv().IsManaged(keyEnv) {
		t.Fatal("key was added to the managed set under env contention")
	}
	data, err := os.ReadFile(a.ManagedEnv().Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), keyEnv) {
		t.Fatalf(".env was mutated under env contention: %q", data)
	}
	// No reload-derived publication.
	if a.cfg != cfgBefore || a.catalog != catBefore {
		t.Fatal("env-contended ConnectProvider applied a reload")
	}
	if !reflect.DeepEqual(a.CurrentWarnings(), warningsBefore) {
		t.Fatalf("env-contended ConnectProvider published warnings: %#v", a.CurrentWarnings())
	}
}

// TestDisconnectProviderEnvLockContentionReturnsPromptly proves the owner
// DisconnectProvider route with a managed key is one-attempt Try on the env
// leaf: with a foreign process holding the env lock, DisconnectProvider returns
// a retryable error promptly and leaves the .env, process env, managed
// ownership, and reload-derived owner state unchanged; after the holder
// releases, the normal success sibling completes and removes the key. It fails
// against the pre-fix blocking env.Remove, which parks on the foreign lock.
func TestDisconnectProviderEnvLockContentionReturnsPromptly(t *testing.T) {
	foreignLockHolderChild()
	keyEnv := "LIGHTCODE_DISCONNECT_LOCK"
	_ = os.Unsetenv(keyEnv)
	t.Cleanup(func() { _ = os.Unsetenv(keyEnv) })
	a := newProviderManagementAgent(t, fmt.Sprintf(`{
  "providers": {
    "disc": {
      "name": "Disc",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": %q },
      "discovery": false,
      "models": { "m": { "context_window": 1000 } }
    }
  }
}`, keyEnv))
	// Establish a managed key through the blocking env wrapper.
	if err := a.env.Set(keyEnv, "secret"); err != nil {
		t.Fatalf("seed managed key: %v", err)
	}
	envLockPath := filepath.Join(filepath.Dir(a.ManagedEnv().Path()), ".locks", "env.lock")
	releaseHolder := startForeignLockHolder(t, "TestDisconnectProviderEnvLockContentionReturnsPromptly", envLockPath)
	defer releaseHolder()
	cfgBefore := a.cfg
	catBefore := a.catalog
	warningsBefore := a.CurrentWarnings()
	envBefore, err := os.ReadFile(a.ManagedEnv().Path())
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- a.DisconnectProvider("disc") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "retry") {
			t.Fatalf("DisconnectProvider under env contention = %v, want a retryable error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DisconnectProvider blocked on the foreign env lock; the env write must be one-attempt Try")
	}
	// .env, process env, and managed ownership unchanged.
	if got := os.Getenv(keyEnv); got != "secret" {
		t.Fatalf("process env %s = %q, want secret unchanged", keyEnv, got)
	}
	if !a.ManagedEnv().IsManaged(keyEnv) {
		t.Fatal("managed ownership was dropped under env contention")
	}
	envAfter, err := os.ReadFile(a.ManagedEnv().Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(envBefore, envAfter) {
		t.Fatalf(".env changed under env contention:\nbefore %q\nafter  %q", envBefore, envAfter)
	}
	// Reload-derived owner state unchanged.
	if a.cfg != cfgBefore || a.catalog != catBefore {
		t.Fatal("env-contended DisconnectProvider applied a reload")
	}
	if !reflect.DeepEqual(a.CurrentWarnings(), warningsBefore) {
		t.Fatalf("env-contended DisconnectProvider published warnings: %#v", a.CurrentWarnings())
	}

	// After release the normal success sibling completes and removes the key.
	releaseHolder()
	if err := a.DisconnectProvider("disc"); err != nil {
		t.Fatalf("DisconnectProvider after the holder released: %v", err)
	}
	if _, set := os.LookupEnv(keyEnv); set {
		t.Fatalf("process env %s still set after the successful DisconnectProvider", keyEnv)
	}
	if a.ManagedEnv().IsManaged(keyEnv) {
		t.Fatal("key still managed after the successful DisconnectProvider")
	}
	envAfterSuccess, err := os.ReadFile(a.ManagedEnv().Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(envAfterSuccess), keyEnv) {
		t.Fatalf(".env still carries the key after the successful DisconnectProvider: %q", envAfterSuccess)
	}
}

// TestRefreshDiscoveryForeignLockReturnsPromptly proves owner discovery
// publication is one-attempt Try: with a foreign process holding the provider's
// discovery lock, explicit RefreshDiscovery returns promptly and writes neither
// the attempt marker nor the cache. It fails against the pre-fix blocking
// refresh, which parks on the foreign lock.
func TestRefreshDiscoveryForeignLockReturnsPromptly(t *testing.T) {
	foreignLockHolderChild()
	keyEnv := "LIGHTCODE_REFRESH_CONTENDED"
	t.Setenv(keyEnv, "")
	_ = os.Unsetenv(keyEnv)
	a := newProviderManagementAgent(t, `{
  "providers": {
    "disc": {
      "name": "Disc",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_REFRESH_CONTENDED" },
      "discovery": true,
      "models": {}
    }
  }
}`)
	t.Setenv(keyEnv, "key")
	lockPath := filepath.Join(a.home, ".lightcode", "cache", "discovery", ".locks", "disc.lock")
	releaseHolder := startForeignLockHolder(t, "TestRefreshDiscoveryForeignLockReturnsPromptly", lockPath)
	defer releaseHolder()

	done := make(chan error, 1)
	go func() { done <- a.RefreshDiscovery("disc") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RefreshDiscovery succeeded under foreign discovery-lock contention")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshDiscovery blocked on the foreign discovery lock; owner discovery writes must be one-attempt Try")
	}
	cache, _ := catalog.ReadDiscoveryCache(a.home)
	if _, ok := cache["disc"]; ok {
		t.Fatalf("contended RefreshDiscovery wrote the discovery cache: %#v", cache["disc"])
	}
	if record, ok := cache["disc"]; ok && !record.AttemptedAt.IsZero() {
		t.Fatalf("contended RefreshDiscovery wrote a discovery attempt: %#v", record)
	}
}

// TestWarmSettingsEditCloseFirstPublishesNothing parks a warm settings edit's
// discovery fetch on a gated endpoint, closes the owner, then releases the
// gate: the edit must refuse at the final guarded hold with nothing published —
// no config write, no discovery cache, no attempt marker, no reload. It fails
// against the pre-fix warm path, which writes the attempt before the fetch and
// the cache plus config after the gate opens, despite the close.
func TestWarmSettingsEditCloseFirstPublishesNothing(t *testing.T) {
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
	t.Cleanup(discoveryServer.Close)
	t.Cleanup(modelServer.Close)
	t.Cleanup(releaseGate)
	configPath, a, setDiscKey := discoveryEnabledConfig(t, modelServer.URL+"/v1", discoveryServer.URL+"/v1")
	setDiscKey()
	cfgBefore := a.cfg
	catBefore := a.catalog
	warningsBefore := a.CurrentWarnings()

	editDone := make(chan error, 1)
	go func() { editDone <- a.SaveModel("test", "test-model", ModelConfigInput{ContextWindow: 9000}) }()
	select {
	case <-entered:
		// The edit's fetch-only discovery phase is in flight.
	case <-time.After(5 * time.Second):
		t.Fatal("warm edit never reached the discovery fetch")
	}

	if !a.ShutdownOwner() {
		t.Fatal("shutdown reported abandoned in-flight work")
	}
	releaseGate()
	select {
	case err := <-editDone:
		if !errors.Is(err, errOwnerClosed) {
			t.Fatalf("SaveModel after close = %v, want the owner-closed refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("warm edit never completed after close")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "9000") {
		t.Fatalf("close-first warm edit wrote the config: %q", data)
	}
	cache, _ := catalog.ReadDiscoveryCache(a.home)
	if _, ok := cache["disc"]; ok {
		t.Fatalf("close-first warm edit wrote the discovery cache: %#v", cache["disc"])
	}
	if record, ok := cache["disc"]; ok && !record.AttemptedAt.IsZero() {
		t.Fatalf("close-first warm edit wrote a discovery attempt: %#v", record)
	}
	if a.cfg != cfgBefore || a.catalog != catBefore {
		t.Fatal("close-first warm edit applied a reload")
	}
	if !reflect.DeepEqual(a.CurrentWarnings(), warningsBefore) {
		t.Fatalf("close-first warm edit published warnings: %#v", a.CurrentWarnings())
	}
}

// TestConnectProviderFetchCloseFirstPublishesNothing parks ConnectProvider's
// write-free discovery fetch on a gated endpoint, closes the owner, then
// releases the gate: the final hold must refuse before any cache or env write.
// It fails against the pre-fix final hold, which has no close guard and
// publishes cache and env after the gate opens.
func TestConnectProviderFetchCloseFirstPublishesNothing(t *testing.T) {
	gate := make(chan struct{})
	var closeGate sync.Once
	releaseGate := func() { closeGate.Do(func() { close(gate) }) }
	entered := make(chan struct{}, 1)
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-gate
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	t.Cleanup(discoveryServer.Close)
	t.Cleanup(releaseGate)
	keyEnv := "LIGHTCODE_CONNECT_FETCH_KEY"
	_ = os.Unsetenv(keyEnv)
	t.Cleanup(func() { _ = os.Unsetenv(keyEnv) })
	a := newProviderManagementAgent(t, fmt.Sprintf(`{
  "providers": {
    "disc": {
      "name": "Disc",
      "transport": { "base_url": %q, "api_key_env": %q },
      "discovery": true,
      "models": {}
    }
  }
	}`, discoveryServer.URL+"/v1", keyEnv))
	cfgBefore := a.cfg
	catBefore := a.catalog
	warningsBefore := a.CurrentWarnings()

	connectDone := make(chan error, 1)
	go func() { connectDone <- a.ConnectProvider("disc", "secret") }()
	select {
	case <-entered:
		// ConnectProvider's write-free fetch is in flight.
	case <-time.After(5 * time.Second):
		t.Fatal("ConnectProvider never reached the discovery fetch")
	}

	if !a.ShutdownOwner() {
		t.Fatal("shutdown reported abandoned in-flight work")
	}
	releaseGate()
	select {
	case err := <-connectDone:
		if !errors.Is(err, errOwnerClosed) {
			t.Fatalf("ConnectProvider after close = %v, want the owner-closed refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ConnectProvider never completed after close")
	}
	cache, _ := catalog.ReadDiscoveryCache(a.home)
	if _, ok := cache["disc"]; ok {
		t.Fatalf("close-first ConnectProvider wrote the discovery cache: %#v", cache["disc"])
	}
	if _, set := os.LookupEnv(keyEnv); set {
		t.Fatalf("close-first ConnectProvider set the process env: %s", keyEnv)
	}
	if a.ManagedEnv().IsManaged(keyEnv) {
		t.Fatal("close-first ConnectProvider marked the key managed")
	}
	data, err := os.ReadFile(a.ManagedEnv().Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), keyEnv) {
		t.Fatalf("close-first ConnectProvider wrote the .env: %q", data)
	}
	if a.cfg != cfgBefore || a.catalog != catBefore {
		t.Fatal("close-first ConnectProvider applied a reload")
	}
	if !reflect.DeepEqual(a.CurrentWarnings(), warningsBefore) {
		t.Fatalf("close-first ConnectProvider published warnings: %#v", a.CurrentWarnings())
	}
}

// TestCloseFirstSinksCreateNoFiles proves every named first-write sink refuses
// after owner close before creating or replacing a file: a close-first reload
// creates neither config.json nor agents.json, a close-first primary-model
// write creates no agents.json, and a close-first runtime-config write leaves
// the existing config file untouched. All fail against the pre-fix unguarded
// writers, which publish after close.
func TestCloseFirstSinksCreateNoFiles(t *testing.T) {
	const seedCfg = `{
  "providers": {
    "test": {
      "name": "Test",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_CLOSE_SINK" },
      "discovery": false,
      "models": { "m": { "name": "M", "context_window": 1000 } }
    }
  }
}`

	t.Run("reload", func(t *testing.T) {
		a := newProviderManagementAgent(t, seedCfg)
		if err := os.Remove(a.configPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(a.agentsPath); err != nil {
			t.Fatal(err)
		}
		if !a.ShutdownOwner() {
			t.Fatal("shutdown reported abandoned in-flight work")
		}
		if err := a.Reload(); !errors.Is(err, errOwnerClosed) {
			t.Fatalf("Reload after close = %v, want the owner-closed refusal", err)
		}
		if _, err := os.Stat(a.configPath); !os.IsNotExist(err) {
			t.Fatalf("close-first reload created config.json: %v", err)
		}
		if _, err := os.Stat(a.agentsPath); !os.IsNotExist(err) {
			t.Fatalf("close-first reload created agents.json: %v", err)
		}
	})

	t.Run("set_default_model", func(t *testing.T) {
		a := newProviderManagementAgent(t, seedCfg)
		if err := os.Remove(a.agentsPath); err != nil {
			t.Fatal(err)
		}
		if !a.ShutdownOwner() {
			t.Fatal("shutdown reported abandoned in-flight work")
		}
		if err := a.SetDefaultModel("test/m"); !errors.Is(err, errOwnerClosed) {
			t.Fatalf("SetDefaultModel after close = %v, want the owner-closed refusal", err)
		}
		if _, err := os.Stat(a.agentsPath); !os.IsNotExist(err) {
			t.Fatalf("close-first SetDefaultModel created agents.json: %v", err)
		}
	})

	t.Run("runtime_config", func(t *testing.T) {
		a := newProviderManagementAgent(t, seedCfg)
		if !a.ShutdownOwner() {
			t.Fatal("shutdown reported abandoned in-flight work")
		}
		err := a.SetRuntimeConfig(RuntimeConfigSettings{
			Sessions:   RuntimeSessionsConfig{ArchiveAfterDays: 60, DeleteAfterArchiveDays: 90},
			Compaction: RuntimeCompactionConfig{ThresholdPct: 0.5},
			Subagents:  RuntimeSubagentsConfig{MaxConcurrent: 4},
			Tools: RuntimeToolsConfig{
				MaxOutputBytes:         65536,
				ReadMaxLines:           500,
				ReadLineMaxChars:       1000,
				CommandTimeout:         60,
				MaxBackgroundProcesses: 8,
			},
		})
		if !errors.Is(err, errOwnerClosed) {
			t.Fatalf("SetRuntimeConfig after close = %v, want the owner-closed refusal", err)
		}
		data, readErr := os.ReadFile(a.configPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(data), "archive_after_days") {
			t.Fatalf("close-first SetRuntimeConfig rewrote the config: %q", data)
		}
	})

	t.Run("warm_edit", func(t *testing.T) {
		// A close-first settings edit must not create or rewrite the config
		// through its preparation phase or its guarded final hold.
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
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": { "test-model": { "name": "Test Model", "context_window": 8192 } }
    }
  },
  "default_model": "test/test-model"
}`)
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
		if err := os.Remove(configPath); err != nil {
			t.Fatal(err)
		}
		if !a.ShutdownOwner() {
			t.Fatal("shutdown reported abandoned in-flight work")
		}
		if err := a.SaveModel("test", "test-model", ModelConfigInput{ContextWindow: 9000}); !errors.Is(err, errOwnerClosed) {
			t.Fatalf("SaveModel after close = %v, want the owner-closed refusal", err)
		}
		if _, err := os.Stat(configPath); !os.IsNotExist(err) {
			t.Fatalf("close-first warm edit created config.json: %v", err)
		}
	})
}

// TestOperationFirstConfigWriteCompletesBeforeClose proves an operation
// admitted before close finishes its existing write before close publishes: a
// runtime-config write blocked inside the sink's atomicfs write completes, the
// config carries the committed settings, and shutdown that started while the
// write held rt.mu still joins cleanly.
func TestOperationFirstConfigWriteCompletesBeforeClose(t *testing.T) {
	a := newProviderManagementAgent(t, `{
  "providers": {
    "test": {
      "name": "Test",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_OP_FIRST" },
      "discovery": false,
      "models": { "m": { "context_window": 1000 } }
    }
  }
}`)
	blocked := make(chan struct{})
	var closeBlock sync.Once
	releaseWrite := func() { closeBlock.Do(func() { close(blocked) }) }
	writeInFlight := make(chan struct{})
	var writeOnce sync.Once
	atomicfs.SyncFileFunc = func(*os.File) error {
		writeOnce.Do(func() { close(writeInFlight) })
		<-blocked
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil })
	t.Cleanup(releaseWrite)

	opDone := make(chan error, 1)
	go func() {
		opDone <- a.SetRuntimeConfig(RuntimeConfigSettings{
			Sessions:   RuntimeSessionsConfig{ArchiveAfterDays: 30, DeleteAfterArchiveDays: 60},
			Compaction: RuntimeCompactionConfig{ThresholdPct: 0.4},
			Subagents:  RuntimeSubagentsConfig{MaxConcurrent: 3},
			Tools: RuntimeToolsConfig{
				MaxOutputBytes:         32768,
				ReadMaxLines:           400,
				ReadLineMaxChars:       900,
				CommandTimeout:         45,
				MaxBackgroundProcesses: 6,
			},
		})
	}()
	<-writeInFlight
	shutdownDone := make(chan bool, 1)
	go func() { shutdownDone <- a.ShutdownOwner() }()
	// Let the shutdown observe the blocked owner lock, then release the write.
	time.Sleep(50 * time.Millisecond)
	releaseWrite()
	if err := <-opDone; err != nil {
		t.Fatalf("operation-first SetRuntimeConfig: %v", err)
	}
	if !<-shutdownDone {
		t.Fatal("shutdown after the operation-first write reported abandoned work")
	}
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "archive_after_days") {
		t.Fatalf("operation-first write did not land: %q", data)
	}
}

// TestOperationFirstPrimaryModelWriteCompletesBeforeClose proves the
// agents/primary-model sink is operation-first: a primary-model write admitted
// before close and blocked inside the atomicfs agents write completes, the
// agents file carries the committed model, and shutdown that started while the
// write held rt.mu still joins cleanly.
func TestOperationFirstPrimaryModelWriteCompletesBeforeClose(t *testing.T) {
	a := newProviderManagementAgent(t, `{
  "providers": {
    "test": {
      "name": "Test",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_OP_FIRST_MODEL" },
      "discovery": false,
      "models": { "m": { "name": "M", "context_window": 1000 } }
    }
  }
}`)
	blocked := make(chan struct{})
	var closeBlock sync.Once
	releaseWrite := func() { closeBlock.Do(func() { close(blocked) }) }
	writeInFlight := make(chan struct{})
	var writeOnce sync.Once
	atomicfs.SyncFileFunc = func(*os.File) error {
		writeOnce.Do(func() { close(writeInFlight) })
		<-blocked
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil })
	t.Cleanup(releaseWrite)

	opDone := make(chan error, 1)
	go func() { opDone <- a.SetDefaultModel("test/m") }()
	<-writeInFlight
	shutdownDone := make(chan bool, 1)
	go func() { shutdownDone <- a.ShutdownOwner() }()
	// Let the shutdown observe the blocked owner lock, then release the write.
	time.Sleep(50 * time.Millisecond)
	releaseWrite()
	if err := <-opDone; err != nil {
		t.Fatalf("operation-first SetDefaultModel: %v", err)
	}
	if !<-shutdownDone {
		t.Fatal("shutdown after the operation-first primary-model write reported abandoned work")
	}
	data, err := os.ReadFile(a.agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "test/m") {
		t.Fatalf("operation-first primary-model write did not land: %q", data)
	}
}

// TestOperationFirstReloadCreatesFirstRunBeforeClose proves the absent
// first-run reload sink is operation-first: a reload admitted before close and
// blocked inside the first config.json creation completes, both first-run files
// are created before close publishes, and shutdown that started while the
// reload held rt.mu still joins cleanly.
func TestOperationFirstReloadCreatesFirstRunBeforeClose(t *testing.T) {
	a := newProviderManagementAgent(t, `{
  "providers": {
    "test": {
      "name": "Test",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_OP_FIRST_RELOAD" },
      "discovery": false,
      "models": { "m": { "context_window": 1000 } }
    }
  }
}`)
	if err := os.Remove(a.configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.agentsPath); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	var closeBlock sync.Once
	releaseWrite := func() { closeBlock.Do(func() { close(blocked) }) }
	writeInFlight := make(chan struct{})
	var writeOnce sync.Once
	atomicfs.SyncFileFunc = func(*os.File) error {
		writeOnce.Do(func() { close(writeInFlight) })
		<-blocked
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil })
	t.Cleanup(releaseWrite)

	opDone := make(chan error, 1)
	go func() { opDone <- a.Reload() }()
	<-writeInFlight
	shutdownDone := make(chan bool, 1)
	go func() { shutdownDone <- a.ShutdownOwner() }()
	// Let the shutdown observe the blocked owner lock, then release the write.
	time.Sleep(50 * time.Millisecond)
	releaseWrite()
	if err := <-opDone; err != nil {
		t.Fatalf("operation-first Reload: %v", err)
	}
	if !<-shutdownDone {
		t.Fatal("shutdown after the operation-first first-run reload reported abandoned work")
	}
	for path, name := range map[string]string{a.configPath: "config.json", a.agentsPath: "agents.json"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("operation-first reload did not create %s: %v", name, err)
		}
	}
}

// TestOwnerPublicationSinkStructure reads the production owner source files
// and proves every owner mutation root converges on a named guarded sink: the
// blocking permission/env/discovery leaf writers appear nowhere in the agent
// files, the raw config write appears only inside writeAgentConfigLocked, the
// blocking Loader.Load appears only in the pre-owner New startup exception, no
// extracted payload acquires a leaf lock, and no warning sink is independently
// guarded.
func TestOwnerPublicationSinkStructure(t *testing.T) {
	agentData, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatalf("read agent.go: %v", err)
	}
	agentSrc := string(agentData)
	editData, err := os.ReadFile("config_editing.go")
	if err != nil {
		t.Fatalf("read config_editing.go: %v", err)
	}
	editSrc := string(editData)
	providerData, err := os.ReadFile("provider_management.go")
	if err != nil {
		t.Fatalf("read provider_management.go: %v", err)
	}
	providerSrc := string(providerData)
	permData, err := os.ReadFile("../permission/store.go")
	if err != nil {
		t.Fatalf("read permission store.go: %v", err)
	}
	permSrc := string(permData)
	dotenvData, err := os.ReadFile("../config/dotenv.go")
	if err != nil {
		t.Fatalf("read config dotenv.go: %v", err)
	}
	dotenvSrc := string(dotenvData)
	discoveryData, err := os.ReadFile("../catalog/discovery.go")
	if err != nil {
		t.Fatalf("read catalog discovery.go: %v", err)
	}
	discoverySrc := string(discoveryData)

	// No owner path may remain on a blocking leaf wrapper: the blocking
	// permission save, the non-Try discovery writers, and the blocking
	// managed-env Set/Remove appear nowhere in the three agent production files.
	// Catalog loading uses the one-attempt Loader.LoadTry on startup and reload.
	// Raw config writes appear exactly twice in agent.go —
	// the definition and writeAgentConfigLocked's own call — and nowhere else.
	for _, file := range []struct {
		name string
		src  string
	}{{"agent.go", agentSrc}, {"config_editing.go", editSrc}, {"provider_management.go", providerSrc}} {
		for _, banned := range []string{
			"permission.SaveLocal(",
			"catalog.WriteDiscoveryCache(",
			"catalog.WriteDiscoveryAttempt(",
			"a.env.Set(",
			"a.env.Remove(",
		} {
			if strings.Contains(file.src, banned) {
				t.Errorf("%s contains %s; no owner path may use a blocking leaf wrapper", file.name, banned)
			}
		}
		if got := strings.Count(file.src, "modelLoader.Load()"); got != 0 {
			t.Errorf("%s calls the blocking modelLoader.Load() %d times, want 0 on owner paths", file.name, got)
		}
		wantRaw := 0
		if file.name == "agent.go" {
			wantRaw = 2
		}
		if got := strings.Count(file.src, "writeAgentConfigAtomic("); got != wantRaw {
			t.Errorf("%s references writeAgentConfigAtomic %d times, want %d (only the definition and writeAgentConfigLocked may)", file.name, got, wantRaw)
		}
	}

	// The named sinks guard before their write and use the one-attempt Try
	// publication.
	sink, ok := extractFunctionBody(agentSrc, "func (a *Agent) writeAgentConfigLocked(")
	if !ok || !strings.Contains(sink, "requireOwnerOpenLocked(") || !strings.Contains(sink, "writeAgentConfigAtomic(") {
		t.Error("writeAgentConfigLocked must guard before the atomicfs config write")
	}
	save, ok := extractFunctionBody(agentSrc, "func (a *Agent) SaveProjectPermissionForSession(")
	if !ok || !strings.Contains(save, "permission.TrySaveLocal(") {
		t.Error("SaveProjectPermissionForSession must publish through the one-attempt permission.TrySaveLocal")
	}
	load, ok := extractFunctionBody(agentSrc, "func (a *Agent) loadCatalogLocked(")
	if !ok || !strings.Contains(load, "modelLoader.LoadTry(") {
		t.Error("loadCatalogLocked must publish through the Try Loader.LoadTry")
	}
	startup, ok := extractFunctionBody(agentSrc, "func New(c Config) (*Agent, error) {")
	if !ok || !strings.Contains(startup, "modelLoader.LoadTry()") {
		t.Error("New must publish startup discovery through Loader.LoadTry")
	}
	setEnv, ok := extractFunctionBody(providerSrc, "func (a *Agent) setManagedKeyLocked(")
	if !ok || !strings.Contains(setEnv, "a.env.TrySet(") {
		t.Error("setManagedKeyLocked must publish through the one-attempt a.env.TrySet")
	}
	removeEnv, ok := extractFunctionBody(providerSrc, "func (a *Agent) removeManagedKeyLocked(")
	if !ok || !strings.Contains(removeEnv, "a.env.TryRemove(") {
		t.Error("removeManagedKeyLocked must publish through the one-attempt a.env.TryRemove")
	}

	// The discovery publication sink enforces the final owner-open authority
	// itself, immediately before its one Try attempt/cache publication — not
	// only at its callers.
	pub, ok := extractFunctionBody(editSrc, "func (a *Agent) publishDiscoveryOutcomeLocked(")
	if !ok {
		t.Fatal("publishDiscoveryOutcomeLocked not found")
	}
	guardIdx := strings.Index(pub, "requireOwnerOpenLocked(")
	tryCacheIdx := strings.Index(pub, "catalog.TryWriteDiscoveryCache(")
	tryAttemptIdx := strings.Index(pub, "catalog.TryWriteDiscoveryAttempt(")
	if guardIdx < 0 || tryCacheIdx < 0 || tryAttemptIdx < 0 || guardIdx > tryCacheIdx || guardIdx > tryAttemptIdx {
		t.Error("publishDiscoveryOutcomeLocked must enforce requireOwnerOpenLocked immediately before its one Try attempt/cache publication")
	}

	// No independent warning guard: the warning/event sinks carry no closed
	// check of their own; they stay in the guarded hold after a named
	// first-write sink (CurrentModelForSession is the named direct exception,
	// not one of these sinks).
	for _, fn := range []struct {
		src    string
		prefix string
	}{
		{agentSrc, "func (a *Agent) setWarningGroup("},
		{agentSrc, "func (a *Agent) addWarning("},
		{agentSrc, "func (a *Agent) updateWarningGroup("},
		{editSrc, "func (a *Agent) surfaceCatalogWarnings("},
	} {
		body, ok := extractFunctionBody(fn.src, fn.prefix)
		if !ok {
			t.Fatalf("%s not found", fn.prefix)
		}
		if strings.Contains(body, "requireOwnerOpenLocked(") || strings.Contains(body, "errOwnerClosed") || strings.Contains(body, "rt.closed") {
			t.Errorf("%s carries an independent warning guard; warnings stay in the guarded hold after a first-write sink", fn.prefix)
		}
	}

	// No nested leaf acquisition inside extracted payloads: the shared payload
	// bodies run under a leaf lock held by their blocking/Try wrapper and never
	// acquire one themselves.
	for _, payload := range []struct {
		src    string
		prefix string
	}{
		{permSrc, "func saveLocalPayload("},
		{dotenvSrc, "func writeDotEnvLinePayload("},
		{dotenvSrc, "func removeDotEnvLinePayload("},
		{dotenvSrc, "func (m *ManagedEnv) setEnvPayloadLocked("},
		{dotenvSrc, "func (m *ManagedEnv) removeEnvPayloadLocked("},
		{discoverySrc, "func writeDiscoveryAttemptPayload("},
		{discoverySrc, "func writeDiscoveryCachePayload("},
	} {
		body, ok := extractFunctionBody(payload.src, payload.prefix)
		if !ok {
			t.Fatalf("%s not found", payload.prefix)
		}
		for _, acq := range []string{"atomicfs.WithLock(", "atomicfs.TryWithLock(", "atomicfs.Acquire(", "atomicfs.TryAcquire("} {
			if strings.Contains(body, acq) {
				t.Errorf("%s calls %s; the payload must never acquire the leaf lock", payload.prefix, acq)
			}
		}
	}
}

// TestPermissionSaveCloseFirstRefusesWithoutWrite proves a permission save
// admitted before close but acquiring lifecycleMu after close refuses with the
// owner-closed error and writes nothing: it parks at lifecycle admission, the
// owner publishes close and completes, then the save is released.
func TestPermissionSaveCloseFirstRefusesWithoutWrite(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	resolver, err := project.NewResolver(home, projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	idCh := make(chan string, 1)
	gate := permission.NewGate(func(_ context.Context, req permission.Request) {
		idCh <- req.ID
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan permission.ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(ctx, permission.Request{ToolName: "write_file", Arg: "{}", ProjectID: "p-save-close"})
	}()
	id := <-idCh

	a := &Agent{projects: resolver, gate: gate}
	parked, release := parkAtLifecycleAdmission(t, a)
	errCh := make(chan error, 1)
	go func() { errCh <- a.SaveProjectPermissionForSession("session-a", id, []string{"write_file:*"}) }()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("permission save never parked at lifecycle admission")
	}
	if !a.ShutdownOwner() {
		t.Fatal("shutdown reported abandoned in-flight work")
	}
	release()
	if err := <-errCh; !errors.Is(err, errOwnerClosed) {
		t.Fatalf("permission save after close = %v, want errOwnerClosed", err)
	}
	if _, err := os.Stat(filepath.Join(resolver.Root(), "p-save-close", "permissions.json")); !os.IsNotExist(err) {
		t.Fatalf("close-first permission save wrote permissions.json: %v", err)
	}
}

func TestPermissionSaveWithoutProjectIdentityPublishesNothing(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	resolver, err := project.NewResolver(home, projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	idCh := make(chan string, 1)
	gate := permission.NewGate(func(_ context.Context, req permission.Request) { idCh <- req.ID })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = gate.AskRequest(ctx, permission.Request{ToolName: "write_file", Arg: "{}"})
	}()
	id := <-idCh

	a := &Agent{projects: resolver, gate: gate}
	err = a.SaveProjectPermissionForSession("session-a", id, []string{"write_file:*"})
	if err == nil || !strings.Contains(err.Error(), "project id is required") {
		t.Fatalf("unbound permission save = %v, want project identity error", err)
	}
	if p, findErr := project.FindByPath(resolver.Root(), projectRoot); findErr != nil {
		t.Fatal(findErr)
	} else if p != nil {
		t.Fatal("unbound permission save created a project record")
	}
}

// TestWarmSettingsEditDiscoveryContentionLeavesConfigAndDue proves a warm
// edit's discovery publication is one-attempt Try: with a foreign process
// holding the provider's discovery lock, the edit commits its config, produces
// a discovery_failure warning, writes no cache, and leaves the provider due.
func TestWarmSettingsEditDiscoveryContentionLeavesConfigAndDue(t *testing.T) {
	foreignLockHolderChild()
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "hi")
	}))
	t.Cleanup(discoveryServer.Close)
	t.Cleanup(modelServer.Close)
	configPath, a, setDiscKey := discoveryEnabledConfig(t, modelServer.URL+"/v1", discoveryServer.URL+"/v1")
	setDiscKey()
	cfgBefore := a.cfg
	catBefore := a.catalog
	lockPath := filepath.Join(a.home, ".lightcode", "cache", "discovery", ".locks", "disc.lock")
	releaseHolder := startForeignLockHolder(t, "TestWarmSettingsEditDiscoveryContentionLeavesConfigAndDue", lockPath)
	defer releaseHolder()

	if err := a.SaveModel("test", "test-model", ModelConfigInput{ContextWindow: 9000}); err != nil {
		t.Fatalf("SaveModel under discovery contention: %v", err)
	}
	// The successful edit applied a reload, so the pointer identity used by the
	// close-first no-reload assertions is proven sensitive: a reload would be
	// observed as a changed a.cfg/a.catalog.
	if a.cfg == cfgBefore || a.catalog == catBefore {
		t.Fatal("successful warm edit did not apply a reload; the close-first no-reload pointer check would be vacuous")
	}
	// The config edit committed despite the discovery contention.
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "9000") {
		t.Fatalf("warm edit did not commit its config under discovery contention: %q", data)
	}
	// No cache was written and the provider stays due.
	cache, _ := catalog.ReadDiscoveryCache(a.home)
	if _, ok := cache["disc"]; ok {
		t.Fatalf("contended warm edit wrote the discovery cache: %#v", cache["disc"])
	}
	if record, ok := cache["disc"]; ok && !record.AttemptedAt.IsZero() {
		t.Fatalf("contended warm edit wrote a discovery attempt: %#v", record)
	}
	recent, err := catalog.DiscoveryAttemptRecent(a.home, "disc", a.catalog.Providers["disc"].Transport, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if recent {
		t.Fatal("contended warm edit marked discovery recent; the provider must stay due")
	}
	// A discovery_failure warning was surfaced.
	found := false
	for _, w := range a.CurrentWarnings() {
		if w.Kind == "catalog_discovery_failure" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("CurrentWarnings = %#v, want a discovery_failure warning", a.CurrentWarnings())
	}
}

// TestSetProviderConfigCloseFirstPublishesNothing parks a candidate transport
// edit's write-free discovery fetch on a gated endpoint, closes the owner, then
// releases the gate: the final hold must refuse with nothing published — no
// config write, no cache/attempt, no reload, no warning.
func TestSetProviderConfigCloseFirstPublishesNothing(t *testing.T) {
	gate := make(chan struct{})
	var closeGate sync.Once
	releaseGate := func() { closeGate.Do(func() { close(gate) }) }
	entered := make(chan struct{}, 1)
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-gate
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	t.Cleanup(discoveryServer.Close)
	t.Cleanup(releaseGate)
	// The config's disc base_url starts unreachable; the edit's candidate
	// endpoint is the gated server, so a committed edit would be observable.
	configPath, a, setDiscKey := discoveryEnabledConfig(t, "http://127.0.0.1:9/v1", "http://127.0.0.1:9/v1")
	setDiscKey()
	cfgBefore := a.cfg
	catBefore := a.catalog
	warningsBefore := a.CurrentWarnings()

	editDone := make(chan error, 1)
	go func() {
		editDone <- a.SetProviderConfig("disc", ProviderConfigInput{BaseURL: discoveryServer.URL + "/v1"})
	}()
	select {
	case <-entered:
		// The candidate edit's write-free discovery fetch is in flight.
	case <-time.After(5 * time.Second):
		t.Fatal("provider edit never reached the candidate discovery fetch")
	}
	if !a.ShutdownOwner() {
		t.Fatal("shutdown reported abandoned in-flight work")
	}
	releaseGate()
	select {
	case err := <-editDone:
		if !errors.Is(err, errOwnerClosed) {
			t.Fatalf("SetProviderConfig after close = %v, want the owner-closed refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider edit never completed after close")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), discoveryServer.URL) {
		t.Fatalf("close-first provider edit wrote the config: %q", data)
	}
	cache, _ := catalog.ReadDiscoveryCache(a.home)
	if _, ok := cache["disc"]; ok {
		t.Fatalf("close-first provider edit wrote the discovery cache: %#v", cache["disc"])
	}
	if record, ok := cache["disc"]; ok && !record.AttemptedAt.IsZero() {
		t.Fatalf("close-first provider edit wrote a discovery attempt: %#v", record)
	}
	if a.cfg != cfgBefore || a.catalog != catBefore {
		t.Fatal("close-first provider edit applied a reload")
	}
	if !reflect.DeepEqual(a.CurrentWarnings(), warningsBefore) {
		t.Fatalf("close-first provider edit published warnings: %#v", a.CurrentWarnings())
	}
}

// TestSetProviderConfigDiscoveryContentionCommitsConfigWithWarning proves the
// candidate transport edit's discovery publication is one-attempt Try: with a
// foreign process holding the provider's discovery lock, the edit commits its
// complete config root, surfaces a discovery_failure warning, writes no cache,
// and leaves the provider due. The candidate complete-root LWW sibling is
// TestSetProviderConfigConcurrentEditLastWriterWins.
func TestSetProviderConfigDiscoveryContentionCommitsConfigWithWarning(t *testing.T) {
	foreignLockHolderChild()
	discoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"disc-model","name":"Disc Model","context_window":4096}]}`))
	}))
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "hi")
	}))
	t.Cleanup(discoveryServer.Close)
	t.Cleanup(modelServer.Close)
	configPath, a, setDiscKey := discoveryEnabledConfig(t, modelServer.URL+"/v1", "http://127.0.0.1:9/v1")
	setDiscKey()
	lockPath := filepath.Join(a.home, ".lightcode", "cache", "discovery", ".locks", "disc.lock")
	releaseHolder := startForeignLockHolder(t, "TestSetProviderConfigDiscoveryContentionCommitsConfigWithWarning", lockPath)
	defer releaseHolder()

	if err := a.SetProviderConfig("disc", ProviderConfigInput{BaseURL: discoveryServer.URL + "/v1"}); err != nil {
		t.Fatalf("SetProviderConfig under discovery contention: %v", err)
	}
	// The complete candidate root committed despite the discovery contention.
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), discoveryServer.URL) {
		t.Fatalf("contended provider edit did not commit its complete root: %q", data)
	}
	// No cache was written and the provider stays due.
	cache, _ := catalog.ReadDiscoveryCache(a.home)
	if _, ok := cache["disc"]; ok {
		t.Fatalf("contended provider edit wrote the discovery cache: %#v", cache["disc"])
	}
	if record, ok := cache["disc"]; ok && !record.AttemptedAt.IsZero() {
		t.Fatalf("contended provider edit wrote a discovery attempt: %#v", record)
	}
	recent, err := catalog.DiscoveryAttemptRecent(a.home, "disc", a.catalog.Providers["disc"].Transport, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if recent {
		t.Fatal("contended provider edit marked discovery recent; the provider must stay due")
	}
	// A discovery_failure warning was surfaced after the config.
	found := false
	for _, w := range a.CurrentWarnings() {
		if w.Kind == "catalog_discovery_failure" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("CurrentWarnings = %#v, want a discovery_failure warning", a.CurrentWarnings())
	}
}

// TestCurrentModelForSessionOwnerCloseMatrix covers the CurrentModelForSession
// matrix row: a call admitted before close completes its lazy model activation
// and close does not roll it back, and a call after close refuses with the
// owner-closed error without mutating the session.
func TestCurrentModelForSessionOwnerCloseMatrix(t *testing.T) {
	keyEnv := "LIGHTCODE_CMF_KEY"
	t.Setenv(keyEnv, "")
	_ = os.Unsetenv(keyEnv)
	a := newProviderManagementAgent(t, fmt.Sprintf(`{
  "providers": {
    "test": {
      "name": "Test",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": %q },
      "discovery": false,
      "models": { "m": { "name": "M", "context_window": 1000 } }
    }
  }
}`, keyEnv))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	sessionID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// The primary model is declared after the session exists, while the
	// provider is still disconnected, so the session has no active model and
	// the first CurrentModelForSession is the lazy mutation under test.
	if err := a.SetDefaultModel("test/m"); err != nil {
		t.Fatalf("SetDefaultModel: %v", err)
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit, err := a.liveSessionLocked(sessionID)
	rt.mu.Unlock()
	if err != nil {
		t.Fatalf("liveSessionLocked: %v", err)
	}
	rt.mu.Lock()
	initiallyActivated := unit.currentRef.Provider != ""
	rt.mu.Unlock()
	if initiallyActivated {
		t.Fatal("session unexpectedly started with an active model")
	}

	// Operation-first sibling: an admitted call completes its lazy activation
	// before close; the activation is not rolled back by close.
	t.Setenv(keyEnv, "cmf-key")
	info, err := a.CurrentModelForSession(sessionID)
	if err != nil {
		t.Fatalf("CurrentModelForSession before close: %v", err)
	}
	if info.Ref != "test/m" {
		t.Fatalf("model info ref = %q, want test/m", info.Ref)
	}
	if !a.ShutdownOwner() {
		t.Fatal("shutdown reported abandoned in-flight work")
	}
	rt.mu.Lock()
	activated := unit.currentRef.Provider == "test" && unit.currentRef.Model == "m"
	rt.mu.Unlock()
	if !activated {
		t.Fatal("close rolled back the admitted CurrentModelForSession activation")
	}

	// Close-first: after close the direct locked open check refuses without
	// mutating the session.
	if _, err := a.CurrentModelForSession(sessionID); !errors.Is(err, errOwnerClosed) {
		t.Fatalf("CurrentModelForSession after close = %v, want errOwnerClosed", err)
	}
}
