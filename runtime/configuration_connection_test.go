package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/tool"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// The connection suite: the write-only provider connect/disconnect and the
// discovery reads over the one configuration writer. Every row pins the
// semantic matrix — the ready candidate before first persistence, the live
// identity revalidation, the one managed-env key action, and the exact
// generation/event/file/env oracles — with real network fixtures and real
// env state under an isolated HOME.

// connectionKey is the supplied write-only credential of the connect tests;
// its value must never reach a status, error, event, or config file.
const connectionKey = "sk-live-connect-1"

// connectionDiscoveryServer is the discovery endpoint: the response is the
// test's own function of the request's Authorization header, and every
// request's header is recorded. When the park gates are armed, every request
// parks the fetch so a test can interleave a concurrent writer. The gate
// makes the startup refresh (no credential yet) fail harmlessly while the
// connect's transient bearer fetch succeeds — one deterministic rule.
type connectionDiscoveryServer struct {
	*httptest.Server
	mu      sync.Mutex
	auths   []string
	respond func(auth string) string
	arrive  chan struct{}
	release chan struct{}
}

// newConnectionDiscoveryServer answers every request with the payload. The
// credential gate keys off the Authorization header: an empty header (the
// startup refresh) gets connectionEmptyDiscovery, a bearer gets payload.
func newConnectionDiscoveryServer(t *testing.T, payload string) *connectionDiscoveryServer {
	t.Helper()
	return newGatedDiscoveryServer(t, func(auth string) string {
		if auth == "" {
			return connectionEmptyDiscovery
		}
		return payload
	})
}

func newGatedDiscoveryServer(t *testing.T, respond func(auth string) string) *connectionDiscoveryServer {
	t.Helper()
	s := &connectionDiscoveryServer{respond: respond}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *connectionDiscoveryServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	respond, arrive, release := s.respond, s.arrive, s.release
	s.mu.Unlock()
	if arrive != nil {
		arrive <- struct{}{}
		<-release
	}
	_, _ = w.Write([]byte(respond(r.Header.Get("Authorization"))))
}

// park arms the fetch parking gate; the server's own Close runs first at
// cleanup, so an armed test must always release its fetch.
func (s *connectionDiscoveryServer) park() (chan struct{}, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arrive = make(chan struct{}, 1)
	s.release = make(chan struct{})
	return s.arrive, s.release
}

// retarget swaps the responder under the lock.
func (s *connectionDiscoveryServer) retarget(respond func(auth string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = respond
}

func (s *connectionDiscoveryServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.auths)
}

func (s *connectionDiscoveryServer) authorizations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...)
}

// Fetched payloads: connectionGhostDiscovery fills the custom provider's
// declared windowless model with a usable window; connectionFullDiscovery
// carries a model id the fixture provider never declared.
const (
	connectionGhostDiscovery = `{"data":[{"id":"ghost","name":"Ghost","context_window":4096}]}`
	connectionFullDiscovery  = `{"data":[{"id":"fresh","name":"Fresh","context_window":4096,"max_output_tokens":1024,"cost":{"input":1.5,"output":2.5}}]}`
	connectionEmptyDiscovery = `{"data":[]}`
)

// connectionProvidersDocument builds the connection fixture document: the
// embedded builtin groq overlaid from the raw layer with the discovery
// endpoint and its windowless ghost model (its builtin-only discovery
// gapfill decides the connect's effective candidate), the custom
// discovery-backed discp whose declared model has no window (the retained
// custom cost-only fill makes it refusable), an already-usable provider, a
// keyless discovery provider, the usable static providers, and the
// discovery-disabled unusable keyed/keyless pair.
func connectionProvidersDocument(endpoint string) string {
	return `{
  "providers": {
    "groq": {"transport": {"base_url": "` + endpoint + `", "api_key_env": "CONNECTION_DISC_KEY"}, "discovery": true, "models": {"ghost": {"name": "Ghost"}}},
    "discp": {"transport": {"base_url": "` + endpoint + `", "api_key_env": "CONNECTION_DISC_KEY"}, "discovery": true, "models": {"ghost": {"name": "Ghost"}}},
    "usablep": {"transport": {"base_url": "` + endpoint + `", "api_key_env": "CONNECTION_USABLE_KEY"}, "discovery": true, "models": {"m": {"context_window": 4096}}},
    "keylessp": {"transport": {"base_url": "http://127.0.0.1:1/v1", "api_key_env": ""}, "discovery": true, "models": {}},
    "staticp": {"transport": {"base_url": "https://static.test/v1", "api_key_env": "CONNECTION_STATIC_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}},
    "keylessusable": {"transport": {"base_url": "https://ku.test/v1", "api_key_env": ""}, "discovery": false, "models": {"m": {"context_window": 4096}}},
    "staticempty": {"transport": {"base_url": "` + endpoint + `", "api_key_env": "CONNECTION_STATIC_KEY"}, "discovery": false, "models": {"m": {"name": "M"}}},
    "keylessempty": {"transport": {"base_url": "` + endpoint + `", "api_key_env": ""}, "discovery": false, "models": {"m": {"name": "M"}}}
  },
  "sessions": {"auto_archive": false, "archive_after_days": 3}
}`
}

const connectionAgentsDocument = `{"primary": {"model": "usablep/m"}}`

// openConnectionRuntime opens the connection fixture's Runtime over one
// store, with the embedded bundled catalog and the isolated HOME giving the
// Runtime its own real managed env and discovery cache. The startup refresh
// carries no credential yet and fails harmlessly on the gated endpoint.
func openConnectionRuntime(t *testing.T, store harness.Storage, endpoint string, extra ...Plugin) (*Runtime, *ownerEnv) {
	t.Helper()
	reserveEnvKey(t, "CONNECTION_DISC_KEY")
	reserveEnvKey(t, "CONNECTION_USABLE_KEY")
	return openConfigurationRuntimeWithEnv(t, store, newOwnerEnv(t), connectionProvidersDocument(endpoint), connectionAgentsDocument, extra...)
}

// reserveEnvKey starts one env name unset and restores its exact prior
// state at cleanup: managed TrySets own no cleanup, and the package's
// subtests share the process environment.
func reserveEnvKey(t *testing.T, name string) {
	t.Helper()
	previous, had := os.LookupEnv(name)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(name, previous)
		} else {
			_ = os.Unsetenv(name)
		}
	})
	_ = os.Unsetenv(name)
}

// assertConnectionRefused pins one refused connection operator: the typed
// failure class, untouched config bytes, unchanged generation and warning
// revision, event silence — plus the connection-specific silence: the key
// env var stays unset and unmanaged.
func assertConnectionRefused(t *testing.T, r *Runtime, sub *Subscription, before []byte, generation uint64, warnRev uint64, err error, want error) {
	t.Helper()
	assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, want)
	assertConnectionEnvSilent(t, r)
}

// assertConnectionEnvSilent proves no connection key action ran: the key
// env var carries no value (a test's own set-empty probe excepted) and the
// retained manager never owns it.
func assertConnectionEnvSilent(t *testing.T, r *Runtime) {
	t.Helper()
	if value := os.Getenv("CONNECTION_DISC_KEY"); value != "" {
		t.Fatalf("a refused connection operator wrote the key env var (%q)", value)
	}
	if r.managedEnv.IsManaged("CONNECTION_DISC_KEY") {
		t.Fatal("a refused connection operator managed the key")
	}
}

// assertNoConnectionPublication asserts zero publication and event silence
// without re-checking file bytes the test itself legitimately changed.
func assertNoConnectionPublication(t *testing.T, r *Runtime, sub *Subscription, generation uint64, warnRev uint64) {
	t.Helper()
	if current := r.config.current(); current == nil || current.generation != generation {
		t.Fatalf("generation advanced to %v", current)
	}
	if got, _ := r.warnings.snapshot(); got != warnRev {
		t.Fatalf("warning revision advanced: %d → %d", warnRev, got)
	}
	assertNoEvent(t, sub)
	assertConnectionEnvSilent(t, r)
}

// assertFollowUpEditProvesReleasedOwnership runs one ordinary edit after a
// failed writer and fails unless it publishes: a leaked build or capture
// ownership would wedge it on a mutex instead.
func assertFollowUpEditProvesReleasedOwnership(t *testing.T, r *Runtime, providerID string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := r.updateProvider(context.Background(), providerID, protocol.ProviderEdit{})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follow-up edit after the failed writer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the failed writer leaked publication ownership")
	}
}

// drainConnectionEvent consumes exactly the events a single completed
// connection operator publishes: the one configuration event, plus the one
// global warning event a warning-changing publication additionally requires —
// tolerated only when it is exactly the store's current revision, the one
// the change advanced — and then pins the stream silent. Batched writers
// keep draining with drainMutationEvent before one final silence assertion.
func drainConnectionEvent(t *testing.T, store *warningStore, sub *Subscription, generation string) Event {
	t.Helper()
	event, ok := nextEvent(t, sub)
	if !ok || eventKind(t, event) != "configuration_changed" || eventGeneration(t, event) != generation {
		t.Fatalf("connection event = %s (ok=%v), want the generation %s configuration event", eventJSON(t, event), ok, generation)
	}
	drainOptionalWarningEvent(t, store, sub)
	return event
}

// drainOptionalWarningEvent consumes the optional global warning event of
// one warning-changing publication — its revision a real store value — and
// then pins the stream silent.
func drainOptionalWarningEvent(t *testing.T, store *warningStore, sub *Subscription) {
	t.Helper()
	consumeOptionalWarningEvent(t, sub, store)
	assertNoEvent(t, sub)
}

// assertDiscoveryCacheLacks proves one fetched payload never reached the
// discovery cache.
func assertDiscoveryCacheLacks(t *testing.T, r *Runtime, providerID, modelID string) {
	t.Helper()
	records, _ := catalog.ReadDiscoveryCache(r.config.loader.Home())
	for id, record := range records {
		if id != providerID {
			continue
		}
		if _, ok := record.Models[modelID]; ok {
			t.Fatalf("discovery cache for %q carries %q", providerID, modelID)
		}
	}
}

// assertDotenvUnchanged reads the managed .env and fails unless its bytes
// still equal the recorded prior state.
func assertDotenvUnchanged(t *testing.T, r *Runtime, before []byte, what string) {
	t.Helper()
	after, err := os.ReadFile(r.managedEnv.Path())
	if err != nil || string(after) != string(before) {
		t.Fatalf(".env after %s = (%q, %v), want the exact prior bytes", what, after, err)
	}
}

func TestConnectProviderDiscoveryBackedManagedKey(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, _, _ := runtimeMutationBaseline(t, r)

		key := connectionKey
		mutation, err := r.connectProvider(ctx, "groq", &key)
		if err != nil {
			t.Fatalf("connectProvider: %v", err)
		}
		if mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("connect generation = %q, want 2", mutation.ConfigurationRevision.Generation)
		}
		if got := mutation.Result; !got.Connected || got.KeySource != protocol.ProviderKeySource(config.KeySourceManaged) || got.ApiKeyEnv != "CONNECTION_DISC_KEY" {
			t.Fatalf("connected provider view = %+v, want connected with a managed key", got)
		}
		if len(mutation.Result.Models) == 0 || !mutation.Result.Models[0].Usable {
			t.Fatalf("connected provider models = %+v, want the fetched usable model", mutation.Result.Models)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")

		// The existing provider's connection owns no file: the configured
		// main and agents bytes are untouched while the generation advanced.
		after, err := os.ReadFile(r.config.configPath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("connect changed the owning config (%v)", err)
		}

		// The managed key: written to the .env, applied to the process env,
		// owned by the retained manager, and already scrubbed from the
		// call-time subprocess snapshot.
		if os.Getenv("CONNECTION_DISC_KEY") != connectionKey || !r.managedEnv.IsManaged("CONNECTION_DISC_KEY") {
			t.Fatalf("managed key state = (%q, %v), want the exact value set and managed", os.Getenv("CONNECTION_DISC_KEY"), r.managedEnv.IsManaged("CONNECTION_DISC_KEY"))
		}
		envData, err := os.ReadFile(r.managedEnv.Path())
		if err != nil || !strings.Contains(string(envData), "CONNECTION_DISC_KEY="+connectionKey+"\n") {
			t.Fatalf(".env = (%q, %v), want the managed key line", envData, err)
		}
		for _, entry := range r.managedEnv.SubprocessEnv() {
			if strings.HasPrefix(entry, "CONNECTION_DISC_KEY=") {
				t.Fatalf("the connected key leaked into the subprocess snapshot: %q", entry)
			}
		}

		// The fetch carried the transient Authorization of the resolved
		// credential; the persisted cache record binds to the configured
		// transport — the current api_key_env binding — never to the
		// provisional fetch copy.
		sawBearer := false
		for _, auth := range server.authorizations() {
			if auth == "Bearer "+connectionKey {
				sawBearer = true
			}
		}
		if !sawBearer {
			t.Fatalf("discovery authorizations = %v, want the transient bearer", server.authorizations())
		}
		records, _ := catalog.ReadDiscoveryCache(r.config.loader.Home())
		record, ok := records["groq"]
		if !ok || len(record.Models) == 0 {
			t.Fatalf("discovery cache records = %+v, want the persisted discovery", records)
		}
		transport := r.config.current().catalog.Providers["groq"].Transport
		if !record.BoundTo(transport) {
			t.Fatal("cache record does not bind to the configured transport")
		}
		provisional := transport
		provisional.APIKeyEnv = ""
		if record.BoundTo(provisional) {
			t.Fatal("cache record binds to the provisional fetch copy")
		}

		// No secret value in the published mutation bytes.
		raw, merr := json.Marshal(mutation)
		if merr != nil || strings.Contains(string(raw), connectionKey) {
			t.Fatalf("mutation bytes carry the key value (%v)", merr)
		}
	})
}

func TestConnectProviderRepeatStillPublishes(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		t.Setenv("CONNECTION_USABLE_KEY", "usable-key-value")

		// A repeat connection of an already-connected usable provider is a
		// real publication under the one shared successful-edit rule.
		first, err := r.connectProvider(ctx, "usablep", nil)
		if err != nil || first.ConfigurationRevision.Generation != "2" {
			t.Fatalf("first connect = (%v, %q), want generation 2", err, first.ConfigurationRevision.Generation)
		}
		before, err := os.ReadFile(r.config.configPath)
		if err != nil {
			t.Fatalf("read config: %v", err)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		second, err := r.connectProvider(ctx, "usablep", nil)
		if err != nil || second.ConfigurationRevision.Generation != "3" {
			t.Fatalf("repeat connect = (%v, %q), want generation 3", err, second.ConfigurationRevision.Generation)
		}
		drainConnectionEvent(t, r.warnings, sub, "3")
		after, err := os.ReadFile(r.config.configPath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("repeat connect changed the owning config (%v)", err)
		}
	})
}

// TestConnectProviderUsableWindowRemovedRefuses is the ready-candidate
// regression: the published staticp is usable, but a direct external edit
// zeroes its only model window. A no-fetch connect must refuse because the
// ready candidate it would publish is unusable — never persist the key or
// publish an unusable provider. The baseline is the immutable post-edit state.
func TestConnectProviderUsableWindowRemovedRefuses(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_STATIC_KEY")

		// The external hand edit removes staticp's only model window: the
		// published snapshot stays usable, so the connect skips the fetch and
		// would persist a key over an unusable latest raw layer.
		edited := strings.Replace(connectionProvidersDocument(server.URL),
			`"staticp": {"transport": {"base_url": "https://static.test/v1", "api_key_env": "CONNECTION_STATIC_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}}`,
			`"staticp": {"transport": {"base_url": "https://static.test/v1", "api_key_env": "CONNECTION_STATIC_KEY"}, "discovery": false, "models": {"m": {}}}`, 1)
		writeServiceFile(t, r.config.configPath, edited)
		before, generation, warnRev := runtimeMutationBaseline(t, r)
		fetches := server.requests()

		key := connectionKey
		_, err = r.connectProvider(ctx, "staticp", &key)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		if got := server.requests(); got != fetches {
			t.Fatalf("usable connect performed %d fetches, want none", got-fetches)
		}
		if r.managedEnv.IsManaged("CONNECTION_STATIC_KEY") || os.Getenv("CONNECTION_STATIC_KEY") != "" {
			t.Fatal("a refused connect persisted the key")
		}
	})
}

func TestConnectProviderUsableSkipsFetch(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_STATIC_KEY")
		fetchesAtOpen := server.requests()

		// An already-usable provider is never probed with a discovery fetch;
		// a supplied key still updates the managed key through TrySet.
		key := connectionKey
		mutation, err := r.connectProvider(ctx, "usablep", &key)
		if err != nil {
			t.Fatalf("connectProvider: %v", err)
		}
		if got := server.requests(); got != fetchesAtOpen {
			t.Fatalf("usable connect performed %d fetches, want none", got-fetchesAtOpen)
		}
		if mutation.ConfigurationRevision.Generation != "2" || !mutation.Result.Connected ||
			mutation.Result.KeySource != protocol.ProviderKeySource(config.KeySourceManaged) {
			t.Fatalf("usable connect = (%q, %+v), want generation 2 and a managed connected provider", mutation.ConfigurationRevision.Generation, mutation.Result)
		}
		event := drainConnectionEvent(t, r.warnings, sub, "2")
		if os.Getenv("CONNECTION_USABLE_KEY") != connectionKey || !r.managedEnv.IsManaged("CONNECTION_USABLE_KEY") {
			t.Fatalf("supplied key state = (%q, %v), want it TrySet through the manager", os.Getenv("CONNECTION_USABLE_KEY"), r.managedEnv.IsManaged("CONNECTION_USABLE_KEY"))
		}

		// The key value is absent from every observable byte: the mutation
		// envelope, the provider read, a later typed error, and the event.
		mutationBytes, merr := json.Marshal(mutation)
		if merr != nil || strings.Contains(string(mutationBytes), connectionKey) {
			t.Fatalf("mutation bytes carry the key value (%v)", merr)
		}
		detail, err := r.getProvider(ctx, "usablep")
		if err != nil {
			t.Fatalf("getProvider: %v", err)
		}
		detailBytes, derr := json.Marshal(detail)
		if derr != nil || strings.Contains(string(detailBytes), connectionKey) {
			t.Fatalf("provider read bytes carry the key value (%v)", derr)
		}
		if _, err := r.connectProvider(ctx, "staticp", nil); err == nil || strings.Contains(err.Error(), connectionKey) {
			t.Fatalf("refusal error = %v, want a typed error without the key value", err)
		}
		eventBytes, everr := json.Marshal(event)
		if everr != nil || strings.Contains(string(eventBytes), connectionKey) {
			t.Fatalf("event bytes carry the key value (%v)", everr)
		}
	})
}

func TestConnectProviderExternalKeyNeverPersisted(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		// The shell key is set outside Lightcode and stays unmanaged.
		t.Setenv("CONNECTION_DISC_KEY", "shell-exported-value")
		before, _, _ := runtimeMutationBaseline(t, r)
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}

		// Without a supplied credential the external value is used as-is and
		// never persisted, removed, or overwritten.
		mutation, err := r.connectProvider(ctx, "groq", nil)
		if err != nil {
			t.Fatalf("connectProvider: %v", err)
		}
		if !mutation.Result.Connected || mutation.Result.KeySource != protocol.ProviderKeySource(config.KeySourceExternal) {
			t.Fatalf("connected provider view = %+v, want an external key source", mutation.Result)
		}
		sawShell := false
		for _, auth := range server.authorizations() {
			if auth == "Bearer shell-exported-value" {
				sawShell = true
			}
			if strings.Contains(auth, connectionKey) {
				t.Fatalf("discovery authorization %q carried a supplied key", auth)
			}
		}
		if !sawShell {
			t.Fatalf("discovery authorizations = %v, want the shell key's bearer", server.authorizations())
		}
		if r.managedEnv.IsManaged("CONNECTION_DISC_KEY") {
			t.Fatal("the external key became managed")
		}
		assertDotenvUnchanged(t, r, envBefore, "the external-key connect")
		after, err := os.ReadFile(r.config.configPath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("connect changed the owning config (%v)", err)
		}

		// A supplied nonempty key on the same external binding refuses before
		// any configuration or cache side effect: the external variable is
		// not silently substituted for the supplied key.
		supplied := connectionKey
		_, err = r.connectProvider(ctx, "groq", &supplied)
		if err == nil || !errors.Is(err, ErrConfiguration) {
			t.Fatalf("supplied key on an external binding = %v, want the pre-effect refusal", err)
		}
		if os.Getenv("CONNECTION_DISC_KEY") != "shell-exported-value" || r.managedEnv.IsManaged("CONNECTION_DISC_KEY") {
			t.Fatalf("external key after the refusal = (%q, %v), want it untouched and unmanaged", os.Getenv("CONNECTION_DISC_KEY"), r.managedEnv.IsManaged("CONNECTION_DISC_KEY"))
		}
		after, err = os.ReadFile(r.config.configPath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("the refused connect changed the owning config (%v)", err)
		}
		assertDotenvUnchanged(t, r, envBefore, "the refused connect")
	})
}

// TestConnectProviderTransientKeyOverridesConfiguredAuthorization pins the
// discovery wire: a nonempty transient key must be the one Authorization the
// fetch carries even when the raw transport configures case-variant
// Authorization spellings, and the captured transport headers stay
// byte-identical to the raw config.
func TestConnectProviderTransientKeyOverridesConfiguredAuthorization(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()

		doc := connectionProvidersDocument(server.URL)
		oldGroq := `"groq": {"transport": {"base_url": "` + server.URL + `", "api_key_env": "CONNECTION_DISC_KEY"}, "discovery": true, "models": {"ghost": {"name": "Ghost"}}}`
		newGroq := `"groq": {"transport": {"base_url": "` + server.URL + `", "api_key_env": "CONNECTION_DISC_KEY", "headers": {"authorization": "Bearer configured-lower", "AUTHORIZATION": "Bearer configured-upper"}}, "discovery": true, "models": {"ghost": {"name": "Ghost"}}}`
		edited := strings.Replace(doc, oldGroq, newGroq, 1)
		if edited == doc {
			t.Fatal("fixture replacement did not apply")
		}
		writeServiceFile(t, r.config.configPath, edited)
		if _, err := r.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		fetches := server.requests()

		key := connectionKey
		if _, err := r.connectProvider(ctx, "groq", &key); err != nil {
			t.Fatalf("connectProvider: %v", err)
		}
		if got := server.requests(); got != fetches+1 {
			t.Fatalf("connect performed %d fetches, want one", got-fetches)
		}
		if auth := server.authorizations()[fetches]; auth != "Bearer "+connectionKey {
			t.Fatalf("connect Authorization = %q, want the resolved transient bearer", auth)
		}
		headers := r.config.current().catalog.Providers["groq"].Transport.Headers
		if len(headers) != 2 || headers["authorization"] != "Bearer configured-lower" || headers["AUTHORIZATION"] != "Bearer configured-upper" {
			t.Fatalf("captured transport headers = %v, want the raw spellings intact", headers)
		}
		drainConnectionEvent(t, r.warnings, sub, "3")
	})
}

// TestConnectProviderKeylessFetchKeepsConfiguredAuthorization is the
// no-transient sibling: a keyless provider's configured Authorization header
// rides its discovery fetch unchanged. The endpoint serves no models, so the
// connect refuses without writes while the wire header stays pinned.
func TestConnectProviderKeylessFetchKeepsConfiguredAuthorization(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		server.retarget(func(string) string { return connectionEmptyDiscovery })

		doc := connectionProvidersDocument(server.URL)
		oldKeyless := `"keylessusable": {"transport": {"base_url": "https://ku.test/v1", "api_key_env": ""}, "discovery": false, "models": {"m": {"context_window": 4096}}}`
		newKeyless := `"moonshot": {"transport": {"base_url": "` + server.URL + `", "api_key_env": "", "headers": {"authorization": "Bearer keyless-configured"}}, "discovery": true, "models": {"ghost": {}}},
    ` + oldKeyless
		edited := strings.Replace(doc, oldKeyless, newKeyless, 1)
		if edited == doc {
			t.Fatal("fixture replacement did not apply")
		}
		writeServiceFile(t, r.config.configPath, edited)
		if _, err := r.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")

		before, generation, warnRev := runtimeMutationBaseline(t, r)
		fetches := server.requests()
		_, err = r.connectProvider(ctx, "moonshot", nil)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		if got := server.requests(); got != fetches+1 {
			t.Fatalf("keyless connect performed %d fetches, want one", got-fetches)
		}
		if auth := server.authorizations()[fetches]; auth != "Bearer keyless-configured" {
			t.Fatalf("keyless connect Authorization = %q, want the configured header unchanged", auth)
		}
		headers := r.config.current().catalog.Providers["moonshot"].Transport.Headers
		if len(headers) != 1 || headers["authorization"] != "Bearer keyless-configured" {
			t.Fatalf("captured transport headers = %v, want the configured spelling intact", headers)
		}
	})
}

func TestConnectProviderKeyResolutionRefusals(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, generation, warnRev := runtimeMutationBaseline(t, r)

		// Unset env, no supplied key: the required credential is missing.
		_, err = r.connectProvider(ctx, "discp", nil)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)

		// A set-but-empty external value fails uniformly.
		t.Setenv("CONNECTION_DISC_KEY", "")
		_, err = r.connectProvider(ctx, "discp", nil)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)

		// An already-usable keyed provider with no credential anywhere still
		// refuses through the one key-resolution rule (no fetch involved).
		_, err = r.connectProvider(ctx, "staticp", nil)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)

		// An unknown provider keeps the read path's typed error.
		_, err = r.connectProvider(ctx, "ghost-provider", nil)
		if !errors.Is(err, catalog.ErrUnknownProvider) {
			t.Fatalf("unknown provider connect = %v, want ErrUnknownProvider", err)
		}
		assertNoConnectionPublication(t, r, sub, generation, warnRev)
	})
}

// TestConnectProviderKeylessSuppliedKeyRefuses pins the shared credential
// rule's keyless shape: a keyless provider with a supplied nonempty key
// refuses through the same resolver before any effect — the supplied key is
// never silently ignored — for a usable provider (no fetch, no publication)
// and an unusable discovery provider (no fetch, no cache record), while the
// no-key keyless connect stays the no-credential no-action connection.
func TestConnectProviderKeylessSuppliedKeyRefuses(t *testing.T) {
	store := storage.NewMemory()
	server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
	r, _ := openConnectionRuntime(t, store, server.URL)
	defer closeProjectionRuntime(r)
	ctx := context.Background()
	sub, err := r.Subscribe(8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()
	envBefore, err := os.ReadFile(r.managedEnv.Path())
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	before, generation, warnRev := runtimeMutationBaseline(t, r)

	// The usable keyless provider currently ignores a supplied key and
	// connects; under the one credential rule the supplied key refuses
	// the connect before any effect, leaving the owning file, the
	// publication, and the managed env untouched.
	key := connectionKey
	_, err = r.connectProvider(ctx, "keylessusable", &key)
	assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
	assertDotenvUnchanged(t, r, envBefore, "the refused keyless connect")

	// The unusable keyless discovery provider takes the same pre-fetch
	// refusal: no discovery record is written for it.
	if _, err := r.connectProvider(ctx, "keylessp", &key); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("unusable keyless connect = %v, want the configuration failure", err)
	}
	assertDiscoveryCacheLacks(t, r, "keylessp", "ghost")
	assertNoConnectionPublication(t, r, sub, generation, warnRev)

	// Without a supplied key the keyless connect is the retained
	// no-credential connection: no key action, one publication.
	mutation, err := r.connectProvider(ctx, "keylessusable", nil)
	if err != nil || !mutation.Result.Connected || mutation.Result.KeySource != protocol.ProviderKeySource(config.KeySourceKeyless) {
		t.Fatalf("keyless connect without a key = (%v, %+v), want the connected keyless view", err, mutation.Result)
	}
	if want := strconv.FormatUint(generation+1, 10); mutation.ConfigurationRevision.Generation != want {
		t.Fatalf("keyless connect generation = %q, want %q", mutation.ConfigurationRevision.Generation, want)
	}
	drainConnectionEvent(t, r.warnings, sub, strconv.FormatUint(generation+1, 10))
}

// TestConnectProviderUnusableDiscoveryDisabledRefusals pins the one early
// unusable-plus-discovery-disabled guard on both credential shapes: the
// refusal precedes key resolution and fetch even with otherwise valid
// credentials, and leaves key env, .env, config, generation, warnings, and
// events untouched.
func TestConnectProviderUnusableDiscoveryDisabledRefusals(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_STATIC_KEY")
		t.Setenv("CONNECTION_STATIC_KEY", "valid-external-key")
		before, generation, warnRev := runtimeMutationBaseline(t, r)
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}
		fetches := server.requests()

		// The keyed unusable provider has a valid external credential: the
		// guard still fires before any key resolution or fetch.
		_, err = r.connectProvider(ctx, "staticempty", nil)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		if r.managedEnv.IsManaged("CONNECTION_STATIC_KEY") {
			t.Fatal("the refused keyed connect managed the external key")
		}

		// The keyless unusable provider takes the same guard with no
		// credential involved.
		_, err = r.connectProvider(ctx, "keylessempty", nil)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)

		if got := server.requests(); got != fetches {
			t.Fatalf("the guard performed %d fetches, want none", got-fetches)
		}
		assertDotenvUnchanged(t, r, envBefore, "the refused guard")
	})
}

func TestConnectProviderUpdatesManagedKey(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		// Seed a stale managed key first.
		if err := r.managedEnv.TrySet("CONNECTION_DISC_KEY", "stale-managed-value"); err != nil {
			t.Fatalf("seed TrySet: %v", err)
		}
		updated := connectionKey
		if _, err := r.connectProvider(ctx, "groq", &updated); err != nil {
			t.Fatalf("connectProvider: %v", err)
		}
		if os.Getenv("CONNECTION_DISC_KEY") != connectionKey {
			t.Fatalf("managed key = %q, want the supplied update", os.Getenv("CONNECTION_DISC_KEY"))
		}
		// The connected-after-Open key is scrubbed from the live call-time
		// snapshot.
		for _, entry := range r.managedEnv.SubprocessEnv() {
			if strings.HasPrefix(entry, "CONNECTION_DISC_KEY=") {
				t.Fatalf("the connected key leaked into the subprocess snapshot: %q", entry)
			}
		}
	})
}

// connectRaceFixture parks the connect fetch so a test can interleave a
// concurrent writer, and carries the post-writer refusal baseline. The caller
// defers closeProjectionRuntime and the release/join pair: a failing
// assertion releases the parked fetch and joins the admitted connect before
// the owner close, so no failure path can hang on the parked HTTP handler.
type connectRaceFixture struct {
	r           *Runtime
	endpoint    string
	arrive      chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	done        chan struct{}
	result      connectRaceResult
	before      []byte
	generation  uint64
	warnRev     uint64
}

func newConnectRaceFixture(t *testing.T, store harness.Storage) *connectRaceFixture {
	server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
	r, _ := openConnectionRuntime(t, store, server.URL)
	arrive, release := server.park()
	return &connectRaceFixture{r: r, endpoint: server.URL, arrive: arrive, release: release}
}

// start launches one parked connect whose outcome lands in the fixture.
func (f *connectRaceFixture) start(providerID string, key *string) {
	f.done = make(chan struct{})
	go func() {
		defer close(f.done)
		f.result.mutation, f.result.err = f.r.connectProvider(context.Background(), providerID, key)
	}()
}

// proceed releases the parked fetch once.
func (f *connectRaceFixture) proceed() {
	f.releaseOnce.Do(func() { close(f.release) })
}

// join waits for the started connect; it is safe before start, after a
// completed join, and when the release runs from a failure defer.
func (f *connectRaceFixture) join() {
	if f.done != nil {
		<-f.done
	}
}

// captureBaseline records the state the legitimate concurrent writer left,
// before the parked connect is released.
func (f *connectRaceFixture) captureBaseline(t *testing.T) {
	t.Helper()
	f.before, f.generation, f.warnRev = runtimeMutationBaseline(t, f.r)
}

// connectRaceResult carries one parked connect's outcome.
type connectRaceResult struct {
	mutation protocol.ProviderMutation
	err      error
}

// assertRefused pins the race outcome against the baseline the concurrent
// writer left: the typed refusal, zero publication, zero env write, and the
// fetched payload never in the cache — the refusal preceded every write.
func (f *connectRaceFixture) assertRefused(t *testing.T, sub *Subscription) {
	t.Helper()
	if f.result.err == nil || !errors.Is(f.result.err, ErrConfiguration) {
		t.Fatalf("raced connect = %v, want the typed refusal", f.result.err)
	}
	assertConnectionRefused(t, f.r, sub, f.before, f.generation, f.warnRev, f.result.err, ErrConfiguration)
	assertDiscoveryCacheLacks(t, f.r, "discp", "ghost")
}

func TestConnectProviderRaceRemovedRefusesBeforeWrites(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		f := newConnectRaceFixture(t, store)
		defer closeProjectionRuntime(f.r)
		ctx := context.Background()
		sub, err := f.r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()

		key := connectionKey
		f.start("discp", &key)
		defer func() { f.proceed(); f.join() }()
		<-f.arrive

		// The concurrent writer removes the provider while the fetch is
		// parked; an unconnected keyed custom provider is removable. The
		// refusal baseline is the state that writer left, captured before
		// the parked connect is released.
		if _, err := f.r.deleteProvider(ctx, "discp"); err != nil {
			t.Fatalf("deleteProvider during the parked fetch: %v", err)
		}
		drainConnectionEvent(t, f.r.warnings, sub, "2")
		f.captureBaseline(t)
		f.proceed()
		f.join()
		f.assertRefused(t, sub)
	})
}

func TestConnectProviderRaceTransportChangedRefuses(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		f := newConnectRaceFixture(t, store)
		defer closeProjectionRuntime(f.r)
		ctx := context.Background()
		sub, err := f.r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()

		key := connectionKey
		f.start("discp", &key)
		defer func() { f.proceed(); f.join() }()
		<-f.arrive

		// The concurrent writer changes the provider's transport identity;
		// the baseline is the published state it left.
		moved := "https://moved.test/v1"
		if _, err := f.r.updateProvider(ctx, "discp", protocol.ProviderEdit{BaseUrl: &moved}); err != nil {
			t.Fatalf("updateProvider during the parked fetch: %v", err)
		}
		drainConnectionEvent(t, f.r.warnings, sub, "2")
		f.captureBaseline(t)
		f.proceed()
		f.join()
		f.assertRefused(t, sub)
	})
}

// TestConnectProviderRaceInvalidLatestTransportRefuses covers the candidate
// revalidation branch the published-identity race cannot: the live snapshot
// still matches, but an external edit makes the latest raw provider invalid
// — the candidate build drops it, so the connect refuses before any write.
func TestConnectProviderRaceInvalidLatestTransportRefuses(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		f := newConnectRaceFixture(t, store)
		defer closeProjectionRuntime(f.r)
		sub, err := f.r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()

		key := connectionKey
		f.start("discp", &key)
		defer func() { f.proceed(); f.join() }()
		<-f.arrive

		// The external edit empties the latest raw transport, so the
		// candidate build drops discp while the live publication still
		// matches the connect's captured identity.
		oldDiscp := `"discp": {"transport": {"base_url": "` + f.endpoint + `", "api_key_env": "CONNECTION_DISC_KEY"}, "discovery": true, "models": {"ghost": {"name": "Ghost"}}}`
		newDiscp := `"discp": {"transport": {}, "discovery": true, "models": {"ghost": {"name": "Ghost"}}}`
		edited := strings.Replace(connectionProvidersDocument(f.endpoint), oldDiscp, newDiscp, 1)
		if edited == connectionProvidersDocument(f.endpoint) {
			t.Fatal("fixture replacement did not apply")
		}
		writeServiceFile(t, f.r.config.configPath, edited)
		f.captureBaseline(t)
		f.proceed()
		f.join()
		f.assertRefused(t, sub)
	})
}

func TestConnectProviderRaceConcurrentModelsSkipsCacheWrite(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, _, _ := runtimeMutationBaseline(t, r)
		arrive, release := server.park()

		key := connectionKey
		done := make(chan struct{})
		var result connectRaceResult
		go func() {
			defer close(done)
			result.mutation, result.err = r.connectProvider(ctx, "discp", &key)
		}()
		var releaseOnce sync.Once
		proceed := func() { releaseOnce.Do(func() { close(release) }) }
		defer func() { proceed(); <-done }()
		<-arrive

		// A concurrent valid writer supplies usable models under the same
		// identity: the overlay AND the cache write are skipped, and the
		// connect still publishes.
		window := 4096
		if _, err := r.saveModel(ctx, "discp", "ghost", protocol.ModelEdit{ContextWindow: &window}); err != nil {
			t.Fatalf("saveModel during the parked fetch: %v", err)
		}
		proceed()
		<-done
		// The batched pair: each publication's warning event is required only
		// when its own refresh changed the store — the concurrent writer's
		// and the connect's groups interleave — so each is consumed optionally
		// and the sequence ends with one silence check.
		nextConfigurationEvent(t, sub, "2")
		consumeOptionalWarningEvent(t, sub, r.warnings)
		if result.err != nil || result.mutation.ConfigurationRevision.Generation != "3" {
			t.Fatalf("raced connect = (%v, %+v), want success at generation 3", result.err, result.mutation)
		}
		nextConfigurationEvent(t, sub, "3")
		consumeOptionalWarningEvent(t, sub, r.warnings)
		assertNoEvent(t, sub)
		after, err := os.ReadFile(r.config.configPath)
		if err != nil || string(after) == string(before) {
			t.Fatalf("the connect published over the wrong bytes (%v)", err)
		}
		// The fetched payload never reached the cache: the concurrent
		// writer's models made the write unnecessary.
		assertDiscoveryCacheLacks(t, r, "discp", "ghost")
		if os.Getenv("CONNECTION_DISC_KEY") != connectionKey {
			t.Fatalf("managed key = %q, want the supplied value", os.Getenv("CONNECTION_DISC_KEY"))
		}
	})
}

func TestConnectProviderDiscoveryFilteredRefuses(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		// The fetch succeeds but never fills the declared windowless model:
		// no usable effective candidate, no publication, no cache write.
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery) // the fetched model id never fills the declared ghost
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, generation, warnRev := runtimeMutationBaseline(t, r)

		key := connectionKey
		_, err = r.connectProvider(ctx, "discp", &key)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		assertDiscoveryCacheLacks(t, r, "discp", "ghost")
	})
}

func TestConnectProviderCacheContentionRefuses(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, generation, warnRev := runtimeMutationBaseline(t, r)
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}

		// A foreign holder keeps the per-provider discovery lock across the
		// connect's cache write.
		lockPath := filepath.Join(r.config.loader.Home(), ".lightcode", "cache", "discovery", ".locks", "groq.lock")
		held := make(chan struct{})
		released := make(chan struct{})
		go func() {
			_ = atomicfs.WithLock(lockPath, func() error {
				close(held)
				<-released
				return nil
			})
		}()
		<-held

		key := connectionKey
		_, err = r.connectProvider(ctx, "groq", &key)
		close(released)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		assertDotenvUnchanged(t, r, envBefore, "the cache contention")
		assertFollowUpEditProvesReleasedOwnership(t, r, "usablep")
	})
}

// TestConnectProviderCacheWrittenKeyWriteFails is the honest partial-state
// row: the fetched discovery cache write succeeds, then the .env key write
// fails, so the ready candidate is never published while the cache stays
// written, fingerprint-bound, and stamped. No cache rollback, no new hook.
func TestConnectProviderCacheWrittenKeyWriteFails(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, generation, warnRev := runtimeMutationBaseline(t, r)
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}

		// Fail only the .env atomic write's temp sync: the discovery cache
		// write is a different directory and completes first. The scoped
		// probe path is resolved before the write because TrySet holds the
		// manager mutex across its own write.
		probe := installOwningSyncProbe(t, r.managedEnv.Path())
		probe.fail = errors.New("injected .env sync failure")

		key := connectionKey
		_, err = r.connectProvider(ctx, "groq", &key)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)

		records, _ := catalog.ReadDiscoveryCache(r.config.loader.Home())
		record, ok := records["groq"]
		if !ok || record.Models["ghost"].ContextWindow != 4096 {
			t.Fatalf("cache record after the failed key write = %+v, want the fetched ghost model", record)
		}
		if !record.BoundTo(r.config.current().catalog.Providers["groq"].Transport) {
			t.Fatal("the partial cache record lost its configured transport binding")
		}
		if record.FetchedAt.IsZero() || record.AttemptedAt.IsZero() {
			t.Fatalf("partial cache record timestamps = (%v, %v), want both stamped", record.FetchedAt, record.AttemptedAt)
		}
		if r.managedEnv.IsManaged("CONNECTION_DISC_KEY") || os.Getenv("CONNECTION_DISC_KEY") != "" {
			t.Fatal("the failed key write left managed/env state behind")
		}
		assertDotenvUnchanged(t, r, envBefore, "the failed key write")
	})
}

func TestConnectProviderCacheUnsafeIdentityRefuses(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_BAD_KEY")

		// A provider identity the catalog's own rules accept — the separate
		// discovery-cache writer refuses it (backslash guard), and the
		// connection refuses with it. The identity regex is never broadened.
		raw := strings.Replace(connectionProvidersDocument(server.URL),
			`"keylessp"`,
			`"bad\\id": {"transport": {"base_url": "`+server.URL+`", "api_key_env": "CONNECTION_BAD_KEY"}, "discovery": true, "models": {"ghost": {}}}, "keylessp"`, 1)
		writeServiceFile(t, r.config.configPath, raw)
		if _, err := r.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		before, generation, warnRev := runtimeMutationBaseline(t, r)

		key := connectionKey
		_, err = r.connectProvider(ctx, `bad\id`, &key)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		assertDiscoveryCacheLacks(t, r, `bad\id`, "fresh")
	})
}

func TestConnectProviderNULKeyRefusedWithoutFileEffect(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, generation, warnRev := runtimeMutationBaseline(t, r)
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}

		// The NUL value fails the native env-value preflight before any
		// owning env-file write: no line, no Setenv, no managed membership.
		broken := "sk-\x00-broken"
		_, err = r.connectProvider(ctx, "usablep", &broken)
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		if os.Getenv("CONNECTION_USABLE_KEY") != "" || r.managedEnv.IsManaged("CONNECTION_USABLE_KEY") {
			t.Fatal("the refused NUL write left env state behind")
		}
		assertDotenvUnchanged(t, r, envBefore, "the NUL refusal")
		if strings.Contains(err.Error(), broken) {
			t.Fatal("the refusal error carries the key value")
		}
	})
}

func TestConnectProviderFetchCancellationBeforePersistence(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		_, generation, warnRev := runtimeMutationBaseline(t, r)
		arrive, release := server.park()

		ctx, cancel := context.WithCancel(context.Background())
		key := connectionKey
		done := make(chan error, 1)
		go func() {
			_, err := r.connectProvider(ctx, "discp", &key)
			done <- err
		}()
		<-arrive
		cancel()
		close(release)
		err = <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled fetch = %v, want the unwrapped context error", err)
		}
		assertNoConnectionPublication(t, r, sub, generation, warnRev)
		assertDiscoveryCacheLacks(t, r, "discp", "ghost")
	})
}

func TestConnectProviderPublicationSurvivesLateCancellation(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()

		// Park the discovery-cache write's temp sync: the caller cancels
		// after the final cancellation check, once persistence has begun.
		probe := installOwningSyncProbe(t, filepath.Join(r.config.loader.Home(), ".lightcode", "cache", "discovery", "groq.json"))
		defer probe.restore()
		probe.park = true
		ctx, cancel := context.WithCancel(context.Background())
		key := connectionKey
		done := make(chan error, 1)
		go func() {
			_, err := r.connectProvider(ctx, "groq", &key)
			done <- err
		}()
		select {
		case <-probe.arrive:
		case <-time.After(10 * time.Second):
			t.Fatal("the cache write never began")
		}
		cancel()
		probe.park = false
		probe.release <- struct{}{}
		if err := <-done; err != nil {
			t.Fatalf("connect after late cancellation = %v, want the ready publication", err)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		if os.Getenv("CONNECTION_DISC_KEY") != connectionKey || !r.managedEnv.IsManaged("CONNECTION_DISC_KEY") {
			t.Fatalf("late-canceled connect key state = (%q, %v)", os.Getenv("CONNECTION_DISC_KEY"), r.managedEnv.IsManaged("CONNECTION_DISC_KEY"))
		}
	})
}

func TestConnectProviderClosedOwnerRefuses(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		ctx := context.Background()

		// After Close: the admission gate refuses.
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		key := connectionKey
		if _, err := r.connectProvider(ctx, "discp", &key); !errors.Is(err, ErrClosed) {
			t.Fatalf("connect after Close = %v, want ErrClosed", err)
		}
		assertDiscoveryCacheLacks(t, r, "discp", "ghost")
	})
}

func TestConnectProviderOwnerCloseDuringFetchRefuses(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		_, generation, _ := runtimeMutationBaseline(t, r)
		arrive, release := server.park()

		key := connectionKey
		done := make(chan error, 1)
		go func() {
			_, err := r.connectProvider(ctx, "discp", &key)
			done <- err
		}()
		<-arrive
		closeDone := make(chan struct{})
		go func() {
			_ = r.Close(ctx) // joins the admitted connect before releasing the lock
			close(closeDone)
		}()
		close(release)
		if err := <-done; !errors.Is(err, ErrClosed) {
			t.Fatalf("connect under owner close = %v, want ErrClosed", err)
		}
		<-closeDone
		// The owner's shutdown publishes its own scope events; only the
		// configuration publication is asserted silent.
		if current := r.config.current(); current == nil || current.generation != generation {
			t.Fatalf("generation advanced to %v", current)
		}
		assertConnectionEnvSilent(t, r)
		assertDiscoveryCacheLacks(t, r, "discp", "ghost")
	})
}

// TestDisconnectProviderUnusableRemovesManagedKey is the nearest forbidden
// sibling of the connect usability rule: an unusable provider (the
// windowless discovery-backed discp) still disconnects, removing its managed
// key through the ready-publication path over untouched bytes.
func TestDisconnectProviderUnusableRemovesManagedKey(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		if err := r.managedEnv.TrySet("CONNECTION_DISC_KEY", "managed-secret"); err != nil {
			t.Fatalf("seed TrySet: %v", err)
		}
		before, _, _ := runtimeMutationBaseline(t, r)

		mutation, err := r.disconnectProvider(ctx, "discp")
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("unusable disconnect = (%v, %+v), want generation 2", err, mutation)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		if os.Getenv("CONNECTION_DISC_KEY") != "" || r.managedEnv.IsManaged("CONNECTION_DISC_KEY") {
			t.Fatal("the managed key survived the unusable provider's disconnect")
		}
		after, rerr := os.ReadFile(r.config.configPath)
		if rerr != nil || string(after) != string(before) {
			t.Fatalf("disconnect changed the owning config (%v)", rerr)
		}
	})
}

func TestDisconnectProviderManagedKey(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		if err := r.managedEnv.TrySet("CONNECTION_USABLE_KEY", "managed-secret"); err != nil {
			t.Fatalf("seed TrySet: %v", err)
		}
		before, _, _ := runtimeMutationBaseline(t, r)

		mutation, err := r.disconnectProvider(ctx, "usablep")
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("disconnectProvider = (%v, %+v), want generation 2", err, mutation)
		}
		if mutation.Result.Connected || mutation.Result.KeySource != protocol.ProviderKeySource(config.KeySourceNone) {
			t.Fatalf("disconnected provider view = %+v, want the disconnected status", mutation.Result)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		if os.Getenv("CONNECTION_USABLE_KEY") != "" || r.managedEnv.IsManaged("CONNECTION_USABLE_KEY") {
			t.Fatal("the managed key survived the disconnect")
		}
		envData, err := os.ReadFile(r.managedEnv.Path())
		if err != nil || strings.Contains(string(envData), "CONNECTION_USABLE_KEY=") {
			t.Fatalf(".env after disconnect = (%q, %v), want the line removed", envData, err)
		}
		after, err := os.ReadFile(r.config.configPath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("disconnect changed the owning config (%v)", err)
		}
	})
}

// awaitStackMatch polls the process stacks until one stack satisfies match,
// bounding the wait so a missing owner wait fails fast.
func awaitStackMatch(t *testing.T, what string, match func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for {
		stacks := string(buf[:goruntime.Stack(buf, true)])
		for _, stack := range strings.Split(stacks, "\n\n") {
			if match(stack) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached its owner wait", what)
		}
		goruntime.Gosched()
	}
}

// awaitProviderQueryOwnerWait waits until the running provider query is
// blocked on the credential owner: the baseline composed projection's
// managed-environment wait or the corrected configuration-capture wait. Both
// signatures are retained so the overlap oracle discriminates either
// implementation.
func awaitProviderQueryOwnerWait(t *testing.T) {
	awaitStackMatch(t, "the provider query", func(stack string) bool {
		if !strings.Contains(stack, "(*Runtime).getProvider") {
			return false
		}
		return strings.Contains(stack, "(*ManagedEnv).IsManaged") || strings.Contains(stack, "configurationService).capture")
	})
}

// awaitConfigurationCaptureWait waits until one admission is blocked on the
// configuration service's capture mutex — the genuine owner wait a writer's
// parked publication produces, not a timing inference.
func awaitConfigurationCaptureWait(t *testing.T) {
	awaitStackMatch(t, "the admission", func(stack string) bool {
		return strings.Contains(stack, "(*configurationService).capture") && strings.Contains(stack, "sync.(*Mutex).Lock")
	})
}

// assertEventStreamSecretFree drains one subscription and proves no event
// body carries the credential value.
func assertEventStreamSecretFree(t *testing.T, sub *Subscription, secret string) {
	t.Helper()
drain:
	for {
		select {
		case event, ok := <-sub.Events():
			if !ok {
				break drain
			}
			body, err := event.MarshalJSON()
			if err != nil {
				t.Fatalf("event body: %v", err)
			}
			if strings.Contains(string(body), secret) {
				t.Fatalf("an event carries the credential value: %s", body)
			}
		default:
			break drain
		}
	}
}

// TestConnectProviderOverlappingReadSeesPublishedPair parks an accepted
// managed-key write mid-file, starts one provider read, and releases the
// writer: the read must observe the published pair — the credential managed
// and connected under the connect's own generation — never the old revision
// with the new key. Neither the response nor the event stream carries the
// key bytes.
func TestConnectProviderOverlappingReadSeesPublishedPair(t *testing.T) {
	store := storage.NewMemory()
	server := newConnectionDiscoveryServer(t, connectionGhostDiscovery)
	r, _ := openConnectionRuntime(t, store, server.URL)
	defer closeProjectionRuntime(r)
	ctx := context.Background()
	sub, err := r.Subscribe(16)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()
	reserveEnvKey(t, "CONNECTION_USABLE_KEY")
	probe := installOwningSyncProbe(t, r.managedEnv.Path())
	defer probe.restore()
	probe.park = true
	release := sync.OnceFunc(probe.releaseProbe)
	defer release()

	type connectResult struct {
		mutation protocol.ProviderMutation
		err      error
	}
	key := connectionKey
	connectDone := make(chan connectResult, 1)
	go func() {
		mutation, err := r.connectProvider(ctx, "usablep", &key)
		connectDone <- connectResult{mutation, err}
	}()
	probe.awaitWriteStarted(t)

	type readResult struct {
		view protocol.ProviderDetail
		err  error
	}
	readDone := make(chan readResult, 1)
	go func() {
		view, err := r.getProvider(ctx, "usablep")
		readDone <- readResult{view, err}
	}()
	awaitProviderQueryOwnerWait(t)
	release()

	connect := <-connectDone
	if connect.err != nil {
		t.Fatalf("connect: %v", connect.err)
	}
	read := <-readDone
	if read.err != nil {
		t.Fatal(read.err)
	}
	if read.view.Provider.KeySource != protocol.Managed || !read.view.Provider.Connected {
		t.Fatalf("overlapping read = source %v connected %v, want the published managed pair",
			read.view.Provider.KeySource, read.view.Provider.Connected)
	}
	if read.view.ConfigurationRevision.Generation != connect.mutation.ConfigurationRevision.Generation {
		t.Fatalf("overlapping read revision = %s, want the connect's published %s",
			read.view.ConfigurationRevision.Generation, connect.mutation.ConfigurationRevision.Generation)
	}
	if body, _ := json.Marshal(read.view); strings.Contains(string(body), key) {
		t.Fatal("the provider read carries the credential value")
	}
	assertEventStreamSecretFree(t, sub, key)
}

// TestDisconnectProviderOverlappingReadStaysCoherent parks a managed-key
// removal mid-file, starts one provider read, and releases the writer: the
// read must observe one real owner state — the key was managed then removed,
// never external — and the state and revision it reports must be the
// writer's published pair, not a composition of live getters.
func TestDisconnectProviderOverlappingReadStaysCoherent(t *testing.T) {
	store := storage.NewMemory()
	server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
	r, _ := openConnectionRuntime(t, store, server.URL)
	defer closeProjectionRuntime(r)
	ctx := context.Background()
	sub, err := r.Subscribe(16)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()
	if err := r.managedEnv.TrySet("CONNECTION_USABLE_KEY", "managed-secret"); err != nil {
		t.Fatalf("seed TrySet: %v", err)
	}
	before, err := r.getProvider(ctx, "usablep")
	if err != nil || before.Provider.KeySource != protocol.Managed || !before.Provider.Connected {
		t.Fatalf("initial provider = (%v, %+v), want the managed connected state", err, before.Provider)
	}
	probe := installOwningSyncProbe(t, r.managedEnv.Path())
	defer probe.restore()
	probe.park = true
	release := sync.OnceFunc(probe.releaseProbe)
	defer release()

	type disconnectResult struct {
		mutation protocol.ProviderMutation
		err      error
	}
	disconnectDone := make(chan disconnectResult, 1)
	go func() {
		mutation, err := r.disconnectProvider(ctx, "usablep")
		disconnectDone <- disconnectResult{mutation, err}
	}()
	probe.awaitWriteStarted(t)

	type readResult struct {
		view protocol.ProviderDetail
		err  error
	}
	readDone := make(chan readResult, 1)
	go func() {
		view, err := r.getProvider(ctx, "usablep")
		readDone <- readResult{view, err}
	}()
	awaitProviderQueryOwnerWait(t)
	release()

	disconnect := <-disconnectDone
	if disconnect.err != nil {
		t.Fatalf("disconnect: %v", disconnect.err)
	}
	read := <-readDone
	if read.err != nil {
		t.Fatal(read.err)
	}
	if read.view.Provider.KeySource == protocol.External {
		t.Fatalf("read fabricated external ownership: source %v connected %v; the key was managed then absent, never external",
			read.view.Provider.KeySource, read.view.Provider.Connected)
	}
	if read.view.Provider.KeySource != protocol.None || read.view.Provider.Connected {
		t.Fatalf("overlapping read = source %v connected %v, want the published removed state",
			read.view.Provider.KeySource, read.view.Provider.Connected)
	}
	if read.view.ConfigurationRevision.Generation != disconnect.mutation.ConfigurationRevision.Generation {
		t.Fatalf("overlapping read revision = %s, want the disconnect's published %s",
			read.view.ConfigurationRevision.Generation, disconnect.mutation.ConfigurationRevision.Generation)
	}
	after, err := r.getProvider(ctx, "usablep")
	if err != nil || after.Provider.KeySource != protocol.None || after.Provider.Connected {
		t.Fatalf("final provider = (%v, %+v), want the removed key", err, after.Provider)
	}
	assertEventStreamSecretFree(t, sub, "managed-secret")
}

// TestMutationResultStaysFrozenAfterNextPublication pins the F3 frozen-result
// rule through the real writer and the real shared projector: the capture
// returned by generation N still encodes N's revision and credential labels
// after a real generation N+1 credential mutation publishes, while N+1's own
// capture projects the removed pair.
func TestMutationResultStaysFrozenAfterNextPublication(t *testing.T) {
	reserveEnvKey(t, "LIGHTCODE_FROZEN_API_KEY")
	h := newMetadataHarness(t)
	writeServiceFile(t, h.configPath, metadataConfigDocument)
	env := config.NewManagedEnvForTest(filepath.Join(h.dataDir, ".env"))
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, acceptValidator))
	svc.attachWarnings(newWarningStore())
	svc.attachEnv(env)
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	ctx := context.Background()

	// N: a real credential mutation persists a managed key.
	key := "sk-frozen-credential"
	window := 4096
	baseURL := "https://frozen.test/v1"
	captureN, err := svc.mutate(ctx, svc.editProviderCreate("frozen", protocol.ProviderEdit{BaseUrl: &baseURL},
		map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	provN := captureN.snapshot.catalog.Providers["frozen"]
	if provN == nil {
		t.Fatal("created provider missing from the capture")
	}
	keyEnv := provN.Transport.APIKeyEnv
	if observed := captureN.credentials[keyEnv]; !observed.Managed || observed.Value != key {
		t.Fatalf("capture N credential = {defined:%v managed:%v valueMatch:%v}, want the managed supplied key",
			observed.Defined, observed.Managed, observed.Value == key)
	}
	generationN := strconv.FormatUint(captureN.snapshot.generation, 10)

	// N+1: another real credential mutation removes the key and publishes.
	if _, err := svc.mutate(ctx, svc.editProviderDisconnect("frozen", &connectionEffects{})); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if current := svc.current(); current == nil || current.generation != captureN.snapshot.generation+1 {
		t.Fatalf("N+1 publication = %+v, want the next generation", current)
	}

	// The actual shared projector, invoked with N's returned capture after
	// N+1, still encodes N's revision and pre-change labels.
	encoded, err := json.Marshal(protocol.ProviderMutation{
		ConfigurationRevision: configurationRevision(captureN.snapshot),
		Result:                providerPostState(captureN, provN.ID),
	})
	if err != nil {
		t.Fatalf("encode N's response: %v", err)
	}
	var decoded protocol.ProviderMutation
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode N's response: %v", err)
	}
	if decoded.ConfigurationRevision.Generation != generationN {
		t.Fatalf("N response revision = %s, want %s", decoded.ConfigurationRevision.Generation, generationN)
	}
	if decoded.Result.KeySource != protocol.Managed || !decoded.Result.Connected {
		t.Fatalf("N response labels = source %v connected %v, want the pre-change managed pair",
			decoded.Result.KeySource, decoded.Result.Connected)
	}

	// Nearest forbidden sibling: N+1's own capture projects the removed pair.
	after, err := svc.capture(ctx)
	if err != nil {
		t.Fatalf("capture after N+1: %v", err)
	}
	afterProv := after.snapshot.catalog.Providers["frozen"]
	if afterProv == nil {
		t.Fatal("the disconnected provider left the catalog")
	}
	if view := projectProvider(after, afterProv); view.KeySource != protocol.None || view.Connected {
		t.Fatalf("N+1 projection = %+v, want the removed key", view)
	}
}

func TestDisconnectProviderRefusals(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, generation, warnRev := runtimeMutationBaseline(t, r)

		// A keyless provider is removed, not disconnected: the nearest
		// forbidden sibling of the successful disconnect.
		_, err = r.disconnectProvider(ctx, "keylessp")
		if err == nil || !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("keyless disconnect = %v, want the invalid refusal", err)
		}
		assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, harness.ErrInvalid)

		// An external set key refuses, naming the env variable alone — no
		// value exists in a Lightcode store, and none is invented.
		t.Setenv("CONNECTION_STATIC_KEY", "shell-exported-value")
		_, err = r.disconnectProvider(ctx, "staticp")
		if err == nil || !errors.Is(err, ErrConfiguration) {
			t.Fatalf("external disconnect = %v, want the typed refusal", err)
		}
		if !strings.Contains(err.Error(), "CONNECTION_STATIC_KEY") || strings.Contains(err.Error(), "shell-exported-value") {
			t.Fatalf("external disconnect error %q, want the env name without any value", err)
		}
		assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
	})
}

func TestDisconnectProviderAbsentUnmanagedSucceeds(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, _, _ := runtimeMutationBaseline(t, r)

		// The env name is set nowhere: the disconnect publishes the ready
		// next generation through the same path, touching nothing.
		mutation, err := r.disconnectProvider(ctx, "staticp")
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("absent-key disconnect = (%v, %+v), want generation 2", err, mutation)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		after, err := os.ReadFile(r.config.configPath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("absent-key disconnect changed the owning config (%v)", err)
		}
	})
}

func TestDisconnectProviderRepeatStillPublishes(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		if err := r.managedEnv.TrySet("CONNECTION_USABLE_KEY", "managed-secret"); err != nil {
			t.Fatalf("seed TrySet: %v", err)
		}

		if _, err := r.disconnectProvider(ctx, "usablep"); err != nil {
			t.Fatalf("first disconnect: %v", err)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		// The second disconnect takes the absent-unmanaged path and STILL
		// publishes the ready next generation.
		second, err := r.disconnectProvider(ctx, "usablep")
		if err != nil || second.ConfigurationRevision.Generation != "3" {
			t.Fatalf("repeat disconnect = (%v, %+v), want generation 3", err, second)
		}
		drainConnectionEvent(t, r.warnings, sub, "3")
	})
}

// TestDisconnectProviderLatestRawTransportChangePublishes pins the
// disconnect live-action/latest-candidate split: the published binding owns
// the key action, while a valid latest raw transport changed without Reload
// is the candidate the disconnect publishes over untouched bytes.
func TestDisconnectProviderLatestRawTransportChangePublishes(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		if err := r.managedEnv.TrySet("CONNECTION_USABLE_KEY", "managed-secret"); err != nil {
			t.Fatalf("seed TrySet: %v", err)
		}

		moved := "https://moved.test/v1"
		oldLine := `"usablep": {"transport": {"base_url": "` + server.URL + `", "api_key_env": "CONNECTION_USABLE_KEY"}, "discovery": true, "models": {"m": {"context_window": 4096}}}`
		newLine := `"usablep": {"transport": {"base_url": "` + moved + `", "api_key_env": "CONNECTION_USABLE_KEY"}, "discovery": true, "models": {"m": {"context_window": 4096}}}`
		doc := connectionProvidersDocument(server.URL)
		edited := strings.Replace(doc, oldLine, newLine, 1)
		if edited == doc {
			t.Fatal("fixture replacement did not apply")
		}
		writeServiceFile(t, r.config.configPath, edited)
		before, _, _ := runtimeMutationBaseline(t, r)

		mutation, err := r.disconnectProvider(ctx, "usablep")
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("latest-raw disconnect = (%v, %+v), want generation 2", err, mutation)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		if mutation.Result.BaseUrl != moved || mutation.Result.ApiKeyEnv != "CONNECTION_USABLE_KEY" || mutation.Result.Connected {
			t.Fatalf("disconnect result = %+v, want the latest raw transport and the disconnected status", mutation.Result)
		}
		if os.Getenv("CONNECTION_USABLE_KEY") != "" || r.managedEnv.IsManaged("CONNECTION_USABLE_KEY") {
			t.Fatal("the published binding's managed key survived the disconnect")
		}
		after, rerr := os.ReadFile(r.config.configPath)
		if rerr != nil || string(after) != string(before) {
			t.Fatalf("disconnect changed the externally edited config (%v)", rerr)
		}
	})
}

// TestDisconnectProviderLatestRawEnvRebindKeepsExternalSibling pins the
// rebinding union: the published binding's managed key is removed, the new
// raw binding's external sibling is untouched, and the response projects the
// latest binding honestly.
func TestDisconnectProviderLatestRawEnvRebindKeepsExternalSibling(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		if err := r.managedEnv.TrySet("CONNECTION_USABLE_KEY", "managed-secret"); err != nil {
			t.Fatalf("seed TrySet: %v", err)
		}
		reserveEnvKey(t, "CONNECTION_REBOUND_KEY")
		t.Setenv("CONNECTION_REBOUND_KEY", "external-rebound-value")

		oldLine := `"usablep": {"transport": {"base_url": "` + server.URL + `", "api_key_env": "CONNECTION_USABLE_KEY"}, "discovery": true, "models": {"m": {"context_window": 4096}}}`
		newLine := `"usablep": {"transport": {"base_url": "` + server.URL + `", "api_key_env": "CONNECTION_REBOUND_KEY"}, "discovery": true, "models": {"m": {"context_window": 4096}}}`
		doc := connectionProvidersDocument(server.URL)
		edited := strings.Replace(doc, oldLine, newLine, 1)
		if edited == doc {
			t.Fatal("fixture replacement did not apply")
		}
		writeServiceFile(t, r.config.configPath, edited)
		before, _, _ := runtimeMutationBaseline(t, r)

		mutation, err := r.disconnectProvider(ctx, "usablep")
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("rebound disconnect = (%v, %+v), want generation 2", err, mutation)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		if mutation.Result.ApiKeyEnv != "CONNECTION_REBOUND_KEY" || mutation.Result.KeySource != protocol.ProviderKeySource(config.KeySourceExternal) || !mutation.Result.Connected {
			t.Fatalf("rebound disconnect result = %+v, want the latest external binding", mutation.Result)
		}
		if os.Getenv("CONNECTION_USABLE_KEY") != "" || r.managedEnv.IsManaged("CONNECTION_USABLE_KEY") {
			t.Fatal("the published binding's managed key survived the rebinding disconnect")
		}
		if os.Getenv("CONNECTION_REBOUND_KEY") != "external-rebound-value" || r.managedEnv.IsManaged("CONNECTION_REBOUND_KEY") {
			t.Fatalf("external sibling state = (%q, %v), want it untouched and unmanaged", os.Getenv("CONNECTION_REBOUND_KEY"), r.managedEnv.IsManaged("CONNECTION_REBOUND_KEY"))
		}
		envAfter, rerr := os.ReadFile(r.managedEnv.Path())
		if rerr != nil || strings.Contains(string(envAfter), "CONNECTION_USABLE_KEY=") || strings.Contains(string(envAfter), "CONNECTION_REBOUND_KEY=") {
			t.Fatalf(".env after the rebinding disconnect = (%q, %v), want the old managed line removed and no external line", envAfter, rerr)
		}
		after, rerr := os.ReadFile(r.config.configPath)
		if rerr != nil || string(after) != string(before) {
			t.Fatalf("disconnect changed the externally edited config (%v)", rerr)
		}
	})
}

// TestDisconnectProviderLatestCandidateRemovedRefuses is the pre-effect
// subject-proof sibling: a latest raw layer that drops the touched provider
// refuses before any key removal, with config/env/generation/warning/event
// untouched.
func TestDisconnectProviderLatestCandidateRemovedRefuses(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		if err := r.managedEnv.TrySet("CONNECTION_USABLE_KEY", "managed-secret"); err != nil {
			t.Fatalf("seed TrySet: %v", err)
		}
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}

		oldTransport := `"transport": {"base_url": "` + server.URL + `", "api_key_env": "CONNECTION_USABLE_KEY"}`
		doc := connectionProvidersDocument(server.URL)
		edited := strings.Replace(doc, oldTransport, `"transport": {}`, 1)
		if edited == doc {
			t.Fatal("fixture replacement did not apply")
		}
		writeServiceFile(t, r.config.configPath, edited)
		before, generation, warnRev := runtimeMutationBaseline(t, r)

		_, err = r.disconnectProvider(ctx, "usablep")
		assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		if os.Getenv("CONNECTION_USABLE_KEY") == "" || !r.managedEnv.IsManaged("CONNECTION_USABLE_KEY") {
			t.Fatal("a refused disconnect removed the published binding's key")
		}
		assertDotenvUnchanged(t, r, envBefore, "the refused disconnect")
	})
}

// --- custom create with the optional write-only key ---

func TestCreateProviderGeneratedEnvNamePersistsExactKey(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "LIGHTCODE_NEWP_API_KEY")

		key := "sk-live-add-1"
		window := 4096
		baseURL := "https://new.test/v1"
		mutation, err := r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key)
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("addProvider = (%v, %+v), want generation 2", err, mutation)
		}
		event := drainConnectionEvent(t, r.warnings, sub, "2")

		// The generated name went to the raw layer, the exact (untrimmed)
		// value only to the managed env.
		if mutation.Result.ApiKeyEnv != "LIGHTCODE_NEWP_API_KEY" {
			t.Fatalf("created provider env name = %q, want the generated name", mutation.Result.ApiKeyEnv)
		}
		raw, err := os.ReadFile(r.config.configPath)
		if err != nil || !strings.Contains(string(raw), `"api_key_env": "LIGHTCODE_NEWP_API_KEY"`) || strings.Contains(string(raw), key) {
			t.Fatalf("raw config = (%q, %v), want the generated name and never the key value", raw, err)
		}
		if os.Getenv("LIGHTCODE_NEWP_API_KEY") != key || !r.managedEnv.IsManaged("LIGHTCODE_NEWP_API_KEY") {
			t.Fatalf("managed key state = (%q, %v)", os.Getenv("LIGHTCODE_NEWP_API_KEY"), r.managedEnv.IsManaged("LIGHTCODE_NEWP_API_KEY"))
		}
		envData, err := os.ReadFile(r.managedEnv.Path())
		if err != nil || !strings.Contains(string(envData), "LIGHTCODE_NEWP_API_KEY="+key+"\n") {
			t.Fatalf(".env = (%q, %v), want the exact untrimmed value", envData, err)
		}
		mutBytes, merr := json.Marshal(mutation)
		if merr != nil || strings.Contains(string(mutBytes), key) {
			t.Fatalf("mutation bytes carry the key value (%v)", merr)
		}
		eventBytes, eerr := json.Marshal(event)
		if eerr != nil || strings.Contains(string(eventBytes), key) {
			t.Fatalf("event bytes carry the key value (%v)", eerr)
		}
	})
}

// TestCreateProviderExplicitEmptyEnvKeylessRefuses pins the supplied-key
// rule's keyless sibling: an explicitly empty binding with a supplied key is
// invalid — the caller must either name a variable or omit the member —
// before any side effect.
func TestCreateProviderExplicitEmptyEnvKeylessRefuses(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}
		before, generation, warnRev := runtimeMutationBaseline(t, r)

		emptyEnv := ""
		key := connectionKey
		window := 4096
		baseURL := "https://new.test/v1"
		_, err = r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &emptyEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key)
		// The refusal oracle already pins the owning file, generation,
		// warnings, and events; the .env silence is this row's own axis.
		assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, harness.ErrInvalid)
		assertDotenvUnchanged(t, r, envBefore, "the refused create")

		// Without a supplied key the explicit keyless create stays keyless.
		if _, err := r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &emptyEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, nil); err != nil {
			t.Fatalf("keyless create without a supplied key: %v", err)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		assertDotenvUnchanged(t, r, envBefore, "the keyless create")
	})
}

// TestCreateProviderExternalBindingWithKeyRefusesBeforeWrites pins the
// supplied-key rule: an explicitly named externally defined variable refuses
// the create before any configuration or cache side effect, leaving its
// value intact — it is not silently substituted for the supplied key.
func TestCreateProviderExternalBindingWithKeyRefusesBeforeWrites(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_SHELL_KEY")
		t.Setenv("CONNECTION_SHELL_KEY", "shell-exported-value")
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}
		before, generation, warnRev := runtimeMutationBaseline(t, r)

		key := connectionKey
		window := 4096
		baseURL := "https://new.test/v1"
		shellEnv := "CONNECTION_SHELL_KEY"
		_, err = r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &shellEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key)
		// The refusal oracle already pins the owning file, generation,
		// warnings, and events; the external sibling and the .env silence are
		// this row's own axes.
		assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		if os.Getenv("CONNECTION_SHELL_KEY") != "shell-exported-value" || r.managedEnv.IsManaged("CONNECTION_SHELL_KEY") {
			t.Fatalf("external sibling state = (%q, %v), want it untouched and unmanaged", os.Getenv("CONNECTION_SHELL_KEY"), r.managedEnv.IsManaged("CONNECTION_SHELL_KEY"))
		}
		assertDotenvUnchanged(t, r, envBefore, "the external-sibling create")
	})
}

// TestCreateProviderManagedOrphanKeyUpdatedBySuppliedValue is the create-side
// ownership regression: a managed orphan name — Lightcode owns it, no
// provider references it — must be updated by a supplied nonempty value
// instead of being mistaken for a shell key and having the replacement
// discarded. The exact value reaches only the .env and never a response byte.
func TestCreateProviderManagedOrphanKeyUpdatedBySuppliedValue(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_ORPHAN_KEY")
		if err := r.managedEnv.TrySet("CONNECTION_ORPHAN_KEY", "stale-managed-value"); err != nil {
			t.Fatalf("seed managed orphan: %v", err)
		}

		key := "sk-live-orphan-replacement"
		window := 4096
		baseURL := "https://new.test/v1"
		orphanEnv := "CONNECTION_ORPHAN_KEY"
		mutation, err := r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &orphanEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key)
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("managed-orphan create = (%v, %+v), want generation 2", err, mutation)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		if os.Getenv("CONNECTION_ORPHAN_KEY") != key || !r.managedEnv.IsManaged("CONNECTION_ORPHAN_KEY") {
			t.Fatalf("managed orphan state = (%q, %v), want the supplied replacement", os.Getenv("CONNECTION_ORPHAN_KEY"), r.managedEnv.IsManaged("CONNECTION_ORPHAN_KEY"))
		}
		envData, rerr := os.ReadFile(r.managedEnv.Path())
		if rerr != nil || !strings.Contains(string(envData), "CONNECTION_ORPHAN_KEY="+key+"\n") || strings.Contains(string(envData), "stale-managed-value") {
			t.Fatalf(".env = (%q, %v), want the replacement line only", envData, rerr)
		}
		raw, rerr := os.ReadFile(r.config.configPath)
		if rerr != nil || !strings.Contains(string(raw), `"api_key_env": "CONNECTION_ORPHAN_KEY"`) || strings.Contains(string(raw), key) {
			t.Fatalf("raw config = (%q, %v), want the name only and never the value", raw, rerr)
		}
		mutBytes, merr := json.Marshal(mutation)
		if merr != nil || strings.Contains(string(mutBytes), key) {
			t.Fatalf("mutation bytes carry the key value (%v)", merr)
		}
	})
}

// TestCreateProviderManagedEmptyKeyFilledBySuppliedValue pins the
// managed-empty union: Lightcode owns the name with no value, and a supplied
// nonempty value fills it instead of tripping the external-empty refusal.
func TestCreateProviderManagedEmptyKeyFilledBySuppliedValue(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_EMPTY_ORPHAN_KEY")
		if err := r.managedEnv.TrySet("CONNECTION_EMPTY_ORPHAN_KEY", ""); err != nil {
			t.Fatalf("seed managed-empty name: %v", err)
		}

		key := "sk-live-empty-orphan-replacement"
		window := 4096
		baseURL := "https://new.test/v1"
		emptyEnv := "CONNECTION_EMPTY_ORPHAN_KEY"
		if _, err := r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &emptyEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key); err != nil {
			t.Fatalf("managed-empty create = %v, want the supplied value to fill the managed name", err)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		if os.Getenv("CONNECTION_EMPTY_ORPHAN_KEY") != key || !r.managedEnv.IsManaged("CONNECTION_EMPTY_ORPHAN_KEY") {
			t.Fatalf("managed-empty state = (%q, %v), want the supplied value", os.Getenv("CONNECTION_EMPTY_ORPHAN_KEY"), r.managedEnv.IsManaged("CONNECTION_EMPTY_ORPHAN_KEY"))
		}
	})
}

// TestCreateProviderManagedNameWithoutSuppliedKeyUsesExisting pins the
// no-supplied-value union: an existing nonempty managed value is used as-is
// and the create publishes without a key write.
func TestCreateProviderManagedNameWithoutSuppliedKeyUsesExisting(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_EXISTING_KEY")
		if err := r.managedEnv.TrySet("CONNECTION_EXISTING_KEY", "existing-managed-value"); err != nil {
			t.Fatalf("seed managed name: %v", err)
		}
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}

		window := 4096
		baseURL := "https://new.test/v1"
		existingEnv := "CONNECTION_EXISTING_KEY"
		mutation, err := r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &existingEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, nil)
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("managed-existing create = (%v, %+v), want generation 2", err, mutation)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		if mutation.Result.KeySource != protocol.ProviderKeySource(config.KeySourceManaged) {
			t.Fatalf("created provider key source = %q, want managed", mutation.Result.KeySource)
		}
		if os.Getenv("CONNECTION_EXISTING_KEY") != "existing-managed-value" || !r.managedEnv.IsManaged("CONNECTION_EXISTING_KEY") {
			t.Fatalf("managed value changed without a supplied key: (%q, %v)", os.Getenv("CONNECTION_EXISTING_KEY"), r.managedEnv.IsManaged("CONNECTION_EXISTING_KEY"))
		}
		assertDotenvUnchanged(t, r, envBefore, "the create without a supplied key")
	})
}

// TestCreateProviderGeneratedNameSkipsOccupiedNames pins the allocation
// union: a generated base name is occupied when a provider references it or
// the environment defines it — a managed orphan, an externally defined empty
// variable, an external shell key, and a catalog-referenced binding included
// — so allocation takes the first free suffixed name, the actual binding is
// returned and persisted with the exact supplied bytes, and every occupied
// value stays untouched.
func TestCreateProviderGeneratedNameSkipsOccupiedNames(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		window := 4096
		baseURL := "https://new.test/v1"

		// The catalog-referenced occupancy: one no-key registration names the
		// holder's binding explicitly, so every later captured catalog
		// references that base name.
		holderEnv := "LIGHTCODE_CATALOGREF_API_KEY"
		reserveEnvKey(t, holderEnv)
		if _, err := r.addProvider(ctx, "catrefholder",
			protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &holderEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, nil); err != nil {
			t.Fatalf("catalog-reference holder: %v", err)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")

		rows := []struct {
			name   string
			id     string
			base   string
			occupy func(t *testing.T)
			intact func(t *testing.T, base string)
		}{
			{"managed orphan", "newp", "LIGHTCODE_NEWP_API_KEY",
				func(t *testing.T) {
					if err := r.managedEnv.TrySet("LIGHTCODE_NEWP_API_KEY", "stale-managed-value"); err != nil {
						t.Fatalf("seed managed orphan: %v", err)
					}
				},
				func(t *testing.T, base string) {
					if os.Getenv(base) != "stale-managed-value" || !r.managedEnv.IsManaged(base) {
						t.Fatalf("managed orphan after allocation = (%q, %v), want the stale value untouched", os.Getenv(base), r.managedEnv.IsManaged(base))
					}
				}},
			{"externally defined empty", "emptyext", "LIGHTCODE_EMPTYEXT_API_KEY",
				func(t *testing.T) { t.Setenv("LIGHTCODE_EMPTYEXT_API_KEY", "") },
				func(t *testing.T, base string) {
					value, defined := os.LookupEnv(base)
					if !defined || value != "" || r.managedEnv.IsManaged(base) {
						t.Fatalf("external-empty occupancy after allocation = (%q, defined %v, managed %v), want it untouched and unmanaged", value, defined, r.managedEnv.IsManaged(base))
					}
				}},
			{"externally defined nonempty", "shellext", "LIGHTCODE_SHELLEXT_API_KEY",
				func(t *testing.T) { t.Setenv("LIGHTCODE_SHELLEXT_API_KEY", "shell-exported-value") },
				func(t *testing.T, base string) {
					if os.Getenv(base) != "shell-exported-value" || r.managedEnv.IsManaged(base) {
						t.Fatalf("external shell occupancy after allocation = (%q, %v), want it untouched and unmanaged", os.Getenv(base), r.managedEnv.IsManaged(base))
					}
				}},
			{"catalog referenced", "catalogref", "LIGHTCODE_CATALOGREF_API_KEY",
				func(t *testing.T) {},
				func(t *testing.T, base string) {
					if got := r.config.current().catalog.Providers["catrefholder"].Transport.APIKeyEnv; got != base {
						t.Fatalf("holder binding after allocation = %q, want the reference intact", got)
					}
					if value, defined := os.LookupEnv(base); defined || value != "" || r.managedEnv.IsManaged(base) {
						t.Fatalf("the referenced name gained a value: (%q, defined %v, managed %v)", value, defined, r.managedEnv.IsManaged(base))
					}
				}},
		}
		generation := 2 // the holder registration published generation 2
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				reserveEnvKey(t, row.base)
				reserveEnvKey(t, row.base+"_2")
				row.occupy(t)

				key := "sk-live-" + row.id
				mutation, err := r.addProvider(ctx, row.id, protocol.ProviderEdit{BaseUrl: &baseURL},
					map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key)
				if err != nil {
					t.Fatalf("generated-collision create = %v, want the first free suffixed allocation", err)
				}
				generation++
				if mutation.ConfigurationRevision.Generation != strconv.Itoa(generation) {
					t.Fatalf("create generation = %q, want %d", mutation.ConfigurationRevision.Generation, generation)
				}
				drainConnectionEvent(t, r.warnings, sub, strconv.Itoa(generation))
				if mutation.Result.ApiKeyEnv != row.base+"_2" || mutation.Result.KeySource != protocol.ProviderKeySource(config.KeySourceManaged) {
					t.Fatalf("generated result = %+v, want the actual %q binding under the managed source", mutation.Result, row.base+"_2")
				}
				if os.Getenv(row.base+"_2") != key || !r.managedEnv.IsManaged(row.base+"_2") {
					t.Fatalf("persisted binding = (%q, %v), want the exact supplied bytes under the fresh name", os.Getenv(row.base+"_2"), r.managedEnv.IsManaged(row.base+"_2"))
				}
				row.intact(t, row.base)
			})
		}
	})
}

// TestCreateProviderExternalBindingWithoutKeyRegisters pins the no-supplied
// key sibling of the allocation rule: an external shell key's variable is
// occupied for allocation, but a create without a supplied key performs no
// credential action at all — the binding registers as-is and stays external.
func TestCreateProviderExternalBindingWithoutKeyRegisters(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_SHELL_KEY")
		t.Setenv("CONNECTION_SHELL_KEY", "shell-exported-value")
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}

		window := 4096
		baseURL := "https://new.test/v1"
		shellEnv := "CONNECTION_SHELL_KEY"
		mutation, err := r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &shellEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, nil)
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("no-key external-binding create = (%v, %+v), want generation 2", err, mutation)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		if mutation.Result.KeySource != protocol.ProviderKeySource(config.KeySourceExternal) {
			t.Fatalf("created provider key source = %q, want external", mutation.Result.KeySource)
		}
		assertDotenvUnchanged(t, r, envBefore, "the no-key create")
		if r.managedEnv.IsManaged("CONNECTION_SHELL_KEY") {
			t.Fatal("the no-key create managed the external variable")
		}
	})
}

// TestCreateProviderMissingKeyRegistersNoAction pins the clarification's
// headline: a create whose configured binding is absent performs no
// credential action and no resolution refusal — the disconnected valid
// registration is representable, and the named binding lands in the raw
// transport and in the result.
func TestCreateProviderMissingKeyRegistersNoAction(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, e := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_MISSING_KEY")
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}

		window := 4096
		baseURL := "https://new.test/v1"
		missingEnv := "CONNECTION_MISSING_KEY"
		mutation, err := r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &missingEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, nil)
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("absent-binding create = (%v, %+v), want generation 2", err, mutation)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		if mutation.Result.KeySource != protocol.ProviderKeySource(config.KeySourceNone) || mutation.Result.ApiKeyEnv != missingEnv {
			t.Fatalf("created provider = (%q, %q), want the none source with the named binding", mutation.Result.KeySource, mutation.Result.ApiKeyEnv)
		}
		assertDotenvUnchanged(t, r, envBefore, "the absent-binding create")
		if _, present := metadataRawProvider(t, e.configPath, "newp")["transport"]; !present {
			t.Fatal("the created provider missing from the raw layer")
		}
	})
}

func TestCreateProviderInvalidCandidateNoWrites(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_RESTORE_KEY")
		before, generation, warnRev := runtimeMutationBaseline(t, r)
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}

		// The candidate-build refusal: a reserved extra_body key drops the
		// provider from the shared validation, so the pre-write subject
		// check refuses with zero publication. The incomplete zero-window
		// model is representable now — the candidate governs, not an
		// editor-only usable-model count.
		reserved := map[string]any{"model": "reserved"}
		supplied := connectionKey
		window := 4096
		baseURL := "https://new.test/v1"
		_, err = r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ExtraBody: &reserved},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &supplied)
		assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		assertDotenvUnchanged(t, r, envBefore, "the refused create")
	})
}

func TestCreateProviderKeyFailureRestoresExactBytes(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_RESTORE_KEY")
		before, generation, warnRev := runtimeMutationBaseline(t, r)
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}

		// The NUL value fails the create's native env-value preflight after
		// the owning file was written: the exact prior bytes are restored,
		// no .env line appears, and the joined failure claims no atomic
		// success.
		broken := "sk-\x00-broken"
		window := 4096
		baseURL := "https://new.test/v1"
		restoreEnv := "CONNECTION_RESTORE_KEY"
		_, err = r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &restoreEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &broken)
		if err == nil || !errors.Is(err, ErrConfiguration) || !strings.Contains(err.Error(), "persist API key") {
			t.Fatalf("create with a failing key write = %v, want the joined typed failure", err)
		}
		assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		after, rerr := os.ReadFile(r.config.configPath)
		if rerr != nil || string(after) != string(before) {
			t.Fatalf("the failed create did not restore the exact prior bytes (%v)", rerr)
		}
		assertDotenvUnchanged(t, r, envBefore, "the NUL refusal")
		if os.Getenv("CONNECTION_RESTORE_KEY") != "" {
			t.Fatal("the failed NUL write left the process env behind")
		}
		assertFollowUpEditProvesReleasedOwnership(t, r, "usablep")
	})
}

func TestCreateProviderKeyFailureRestoresPriorAbsence(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_RESTORE_KEY")
		_, generation, warnRev := runtimeMutationBaseline(t, r)
		if err := os.Remove(r.config.configPath); err != nil {
			t.Fatalf("remove config: %v", err)
		}

		// The owning file was absent before the create: the failed key
		// write restores that absence.
		broken := "sk-\x00-broken"
		window := 4096
		baseURL := "https://new.test/v1"
		restoreEnv := "CONNECTION_RESTORE_KEY"
		_, err = r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &restoreEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &broken)
		if err == nil || !errors.Is(err, ErrConfiguration) {
			t.Fatalf("create with a failing key write = %v, want the typed failure", err)
		}
		if _, rerr := os.Stat(r.config.configPath); !os.IsNotExist(rerr) {
			t.Fatalf("the prior file absence was not restored (%v)", rerr)
		}
		assertNoConnectionPublication(t, r, sub, generation, warnRev)
	})
}

func TestCreateProviderKeyFailureJoinedRestoreError(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		reserveEnvKey(t, "CONNECTION_RESTORE_KEY")
		before, generation, warnRev := runtimeMutationBaseline(t, r)

		// The restore write itself fails (the second sync of the owning
		// path): the error joins the key failure and the restore failure,
		// and no publication claims atomic success.
		previous := atomicfs.SyncFileFunc
		syncCount := 0
		atomicfs.SyncFileFunc = func(f *os.File) error {
			if filepath.Dir(f.Name()) == filepath.Dir(r.config.configPath) &&
				strings.HasPrefix(filepath.Base(f.Name()), filepath.Base(r.config.configPath)+".tmp-") {
				syncCount++
				if syncCount == 2 { // the restore write
					return errors.New("restore probe failure")
				}
			}
			return f.Sync()
		}
		t.Cleanup(func() { atomicfs.SyncFileFunc = previous })

		broken := "sk-\x00-broken"
		window := 4096
		baseURL := "https://new.test/v1"
		restoreEnv := "CONNECTION_RESTORE_KEY"
		_, err = r.addProvider(ctx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &restoreEnv},
			map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &broken)
		if err == nil || !errors.Is(err, ErrConfiguration) {
			t.Fatalf("create with a failing restore = %v, want the typed failure", err)
		}
		if !strings.Contains(err.Error(), "persist API key") || !strings.Contains(err.Error(), "restore") {
			t.Fatalf("joined failure = %v, want the key failure and the restore failure joined", err)
		}
		assertNoConnectionPublication(t, r, sub, generation, warnRev)
		// The restore failed: the written create bytes are the honest state
		// on disk, never a claimed rollback.
		after, rerr := os.ReadFile(r.config.configPath)
		if rerr != nil || string(after) == string(before) || !strings.Contains(string(after), "newp") {
			t.Fatalf("disk state after the failed restore = (%q, %v), want the written create kept", after, rerr)
		}
	})
}

func TestCreateProviderRestoreDespiteCancellation(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, generation, warnRev := runtimeMutationBaseline(t, r)

		// Park the create write's own temp sync and cancel the caller: the
		// write completes, the key write fails, and the restore still runs.
		probe := installOwningSyncProbe(t, r.config.configPath)
		defer probe.restore()
		probe.park = true
		cancelCtx, cancel := context.WithCancel(context.Background())
		broken := "sk-\x00-broken"
		window := 4096
		baseURL := "https://new.test/v1"
		restoreEnv := "CONNECTION_RESTORE_KEY"
		done := make(chan error, 1)
		go func() {
			_, err := r.addProvider(cancelCtx, "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &restoreEnv},
				map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &broken)
			done <- err
		}()
		select {
		case <-probe.arrive:
		case <-time.After(10 * time.Second):
			t.Fatal("the create write never began")
		}
		cancel()
		probe.park = false
		probe.release <- struct{}{}
		err = <-done
		if err == nil || !errors.Is(err, ErrConfiguration) {
			t.Fatalf("canceled create = %v, want the typed failure", err)
		}
		assertRuntimeMutationRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		after, rerr := os.ReadFile(r.config.configPath)
		if rerr != nil || string(after) != string(before) {
			t.Fatalf("the canceled create did not restore the exact prior bytes (%v)", rerr)
		}
	})
}

// --- discovery reads ---

func TestDiscoverCustomProviderPureRead(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		_, generation, warnRev := runtimeMutationBaseline(t, r)
		t.Setenv("CONNECTION_DISC_REQ_KEY", "request-key-value")
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}
		fetches := server.requests()

		headers := map[string]string{"X-Trace": "t1"}
		candidates, err := r.discoverProvider(ctx, protocol.DiscoveryRequest{
			BaseUrl:   server.URL + "/v1",
			ApiKeyEnv: "CONNECTION_DISC_REQ_KEY",
			Headers:   &headers,
		})
		if err != nil {
			t.Fatalf("discoverProvider: %v", err)
		}
		if len(candidates) != 1 || candidates[0].Id != "fresh" || !candidates[0].Usable || candidates[0].ContextWindow != 4096 {
			t.Fatalf("candidates = %+v, want the one usable fetched model", candidates)
		}
		if candidates[0].Cost == nil || candidates[0].Cost.Input == nil || *candidates[0].Cost.Input != 1.5 {
			t.Fatalf("candidate cost = %+v, want the owned fetched cost", candidates[0].Cost)
		}
		if got := server.requests(); got != fetches+1 {
			t.Fatalf("discovery read performed %d fetches, want exactly one", got-fetches)
		}
		if !slices.Contains(server.authorizations(), "Bearer request-key-value") {
			t.Fatalf("discovery authorizations = %v, want the referenced env's bearer", server.authorizations())
		}
		// The pure read wrote nothing: no cache, no env, no config, no
		// generation, no event.
		if _, err := os.Stat(filepath.Join(r.config.loader.Home(), ".lightcode", "cache", "discovery", "custom.json")); !os.IsNotExist(err) {
			t.Fatalf("the discovery read wrote a cache file: %v", err)
		}
		assertNoConnectionPublication(t, r, sub, generation, warnRev)
		assertDotenvUnchanged(t, r, envBefore, "the discovery read")

		// The credential-header refusal fires before any fetch.
		credHeaders := map[string]string{"Authorization": "Bearer forged"}
		if _, err := r.discoverProvider(ctx, protocol.DiscoveryRequest{BaseUrl: server.URL + "/v1", Headers: &credHeaders}); err == nil || !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("credential-header discovery = %v, want the invalid refusal", err)
		}
		if got := server.requests(); got != fetches+1 {
			t.Fatalf("the refused discovery fetched (%d -> %d)", fetches+1, got)
		}

		// A set-but-empty referenced env is no credential: the read fetches
		// unauthenticated and answers the endpoint, matching saved-provider
		// discovery. An empty base URL is invalid input and refuses before
		// any fetch.
		server.retarget(func(string) string { return connectionFullDiscovery })
		t.Setenv("CONNECTION_EMPTY_REQ_KEY", "")
		emptyAuthCandidates, err := r.discoverProvider(ctx, protocol.DiscoveryRequest{BaseUrl: server.URL + "/v1", ApiKeyEnv: "CONNECTION_EMPTY_REQ_KEY"})
		if err != nil || len(emptyAuthCandidates) != 1 || emptyAuthCandidates[0].Id != "fresh" {
			t.Fatalf("set-empty env discovery = (%v, %+v), want the unauthenticated fetch's candidates", err, emptyAuthCandidates)
		}
		if auths := server.authorizations(); auths[len(auths)-1] != "" {
			t.Fatalf("set-empty discovery authorization = %q, want the unauthenticated fetch", auths[len(auths)-1])
		}
		if got := server.requests(); got != fetches+2 {
			t.Fatalf("the set-empty discovery fetched (%d -> %d)", fetches+1, got)
		}
		if _, err := r.discoverProvider(ctx, protocol.DiscoveryRequest{}); err == nil || !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("empty base URL discovery = %v, want the invalid refusal", err)
		}
		if got := server.requests(); got != fetches+2 {
			t.Fatalf("the refused discovery fetched (%d -> %d)", fetches+2, got)
		}
	})
}

// TestDiscoverCustomProviderSuppliedKey pins the discovery read's one
// credential input rule over a pre-save request: a supplied nonempty key
// binds its exact value to the transient fetch — never the referenced
// owner's captured value — an absent or empty supplied key falls back to
// the referenced env's captured value, and no credential means an
// unauthenticated fetch. Every row is a pure read: no key, env, config,
// cache, or publication write, and no credential value in the returned
// candidates.
func TestDiscoverCustomProviderSuppliedKey(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		const pasted = "sk-pasted-discovery-key"
		const external = "sk-external-reference-value"
		// The gate answers one exact bearer per retarget, so each row's
		// payload proves which credential reached the wire.
		server := newGatedDiscoveryServer(t, func(auth string) string {
			if auth == "Bearer "+pasted {
				return connectionFullDiscovery
			}
			return connectionEmptyDiscovery
		})
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, generation, warnRev := runtimeMutationBaseline(t, r)
		envBefore, err := os.ReadFile(r.managedEnv.Path())
		if err != nil {
			t.Fatalf("read .env: %v", err)
		}
		reserveEnvKey(t, "CONNECTION_SUPPLIED_EXT_KEY")
		reserveEnvKey(t, "CONNECTION_SUPPLIED_UNSET_KEY")
		t.Setenv("CONNECTION_SUPPLIED_EXT_KEY", external)
		fetches := server.requests()
		assertCandidates := func(candidates []protocol.DiscoveredModelCandidate, wantID string) {
			t.Helper()
			if len(candidates) != 1 || candidates[0].Id != wantID {
				t.Fatalf("candidates = %+v, want exactly the fetched %q", candidates, wantID)
			}
		}

		// Pre-save pasted-key discovery with no reference: the supplied
		// value binds the transient fetch on its own.
		pastedCandidates, err := r.discoverProvider(ctx, protocol.DiscoveryRequest{
			BaseUrl: server.URL + "/v1",
			ApiKey:  ptrTo(pasted),
		})
		if err != nil {
			t.Fatalf("keyless-reference supplied-key discovery: %v", err)
		}
		assertCandidates(pastedCandidates, "fresh")

		// Pre-save pasted-key discovery over an externally owned reference:
		// the supplied value binds the fetch; the referenced owner's value
		// is never substituted for it.
		pastedCandidates, err = r.discoverProvider(ctx, protocol.DiscoveryRequest{
			BaseUrl:   server.URL + "/v1",
			ApiKeyEnv: "CONNECTION_SUPPLIED_EXT_KEY",
			ApiKey:    ptrTo(pasted),
		})
		if err != nil {
			t.Fatalf("supplied-key discovery: %v", err)
		}
		assertCandidates(pastedCandidates, "fresh")

		// Without a supplied key the referenced owner's captured value
		// binds the same read; an empty supplied key is the same fallback.
		server.retarget(func(auth string) string {
			if auth == "Bearer "+external {
				return connectionGhostDiscovery
			}
			return connectionEmptyDiscovery
		})
		fallbackCandidates, err := r.discoverProvider(ctx, protocol.DiscoveryRequest{BaseUrl: server.URL + "/v1", ApiKeyEnv: "CONNECTION_SUPPLIED_EXT_KEY"})
		if err != nil {
			t.Fatalf("fallback discovery: %v", err)
		}
		assertCandidates(fallbackCandidates, "ghost")
		emptySuppliedCandidates, err := r.discoverProvider(ctx, protocol.DiscoveryRequest{
			BaseUrl:   server.URL + "/v1",
			ApiKeyEnv: "CONNECTION_SUPPLIED_EXT_KEY",
			ApiKey:    ptrTo(""),
		})
		if err != nil {
			t.Fatalf("empty-supplied-key discovery: %v", err)
		}
		assertCandidates(emptySuppliedCandidates, "ghost")

		// A named-unset reference carries no credential: the read fetches
		// unauthenticated and answers the endpoint.
		server.retarget(func(auth string) string {
			if auth == "" {
				return connectionFullDiscovery
			}
			return connectionEmptyDiscovery
		})
		unsetCandidates, err := r.discoverProvider(ctx, protocol.DiscoveryRequest{BaseUrl: server.URL + "/v1", ApiKeyEnv: "CONNECTION_SUPPLIED_UNSET_KEY"})
		if err != nil {
			t.Fatalf("unset-reference discovery: %v", err)
		}
		assertCandidates(unsetCandidates, "fresh")

		// The exact wire credentials per row: the pasted key twice, the
		// captured external value twice, then the unauthenticated fetch.
		wantAuths := []string{"Bearer " + pasted, "Bearer " + pasted, "Bearer " + external, "Bearer " + external, ""}
		if got := server.authorizations(); !slices.Equal(got[fetches:], wantAuths) {
			t.Fatalf("discovery authorizations = %v, want %v", got[fetches:], wantAuths)
		}

		// The pure read wrote nothing: the externally owned value is
		// untouched and unmanaged, the .env and config bytes are unchanged,
		// nothing published or was cached, and no candidate carries a
		// credential value.
		if os.Getenv("CONNECTION_SUPPLIED_EXT_KEY") != external || r.managedEnv.IsManaged("CONNECTION_SUPPLIED_EXT_KEY") {
			t.Fatalf("external reference state = (%q, %v), want the exact value intact and unmanaged", os.Getenv("CONNECTION_SUPPLIED_EXT_KEY"), r.managedEnv.IsManaged("CONNECTION_SUPPLIED_EXT_KEY"))
		}
		assertDotenvUnchanged(t, r, envBefore, "the supplied-key discovery reads")
		assertNoConnectionPublication(t, r, sub, generation, warnRev)
		after, err := os.ReadFile(r.config.configPath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("the supplied-key discovery reads changed the owning config (%v)", err)
		}
		if _, err := os.Stat(filepath.Join(r.config.loader.Home(), ".lightcode", "cache", "discovery", "custom.json")); !os.IsNotExist(err) {
			t.Fatalf("the supplied-key discovery reads wrote a cache file: %v", err)
		}
		for _, row := range []struct {
			what       string
			candidates []protocol.DiscoveredModelCandidate
		}{{"pasted", pastedCandidates}, {"fallback", fallbackCandidates}, {"empty-supplied", emptySuppliedCandidates}, {"unset", unsetCandidates}} {
			raw, merr := json.Marshal(row.candidates)
			if merr != nil || strings.Contains(string(raw), pasted) || strings.Contains(string(raw), external) {
				t.Fatalf("the %s row's candidates carry a credential value: (%s, %v)", row.what, raw, merr)
			}
		}
	})
}

func TestDiscoverProviderModelsFiltersUsable(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newGatedDiscoveryServer(t, func(auth string) string {
			if auth == "" { // the startup refresh: harmless failure, no cached models
				return connectionEmptyDiscovery
			}
			return `{"data":[{"id":"m","name":"Included","context_window":4096},{"id":"a","name":"A","context_window":0},{"id":"b","name":"B","context_window":8192,"cost":{"input":0.5}}]}`
		})
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		t.Setenv("CONNECTION_USABLE_KEY", "usable-key-value")
		_, generation, warnRev := runtimeMutationBaseline(t, r)
		records, _ := catalog.ReadDiscoveryCache(r.config.loader.Home())
		attemptBefore := records["usablep"].AttemptedAt
		fetches := server.requests()

		candidates, err := r.discoverProviderModels(ctx, "usablep")
		if err != nil {
			t.Fatalf("discoverProviderModels: %v", err)
		}
		if len(candidates) != 2 || candidates[0].Id != "a" || candidates[1].Id != "b" {
			t.Fatalf("candidates = %+v, want the sorted filtered pair a,b — the already-included usable m excluded", candidates)
		}
		if candidates[0].Usable || !candidates[1].Usable || candidates[1].Cost == nil || candidates[1].Cost.Input == nil || *candidates[1].Cost.Input != 0.5 {
			t.Fatalf("candidate flags = %+v, want windowless a unusable and usable b with its owned cost", candidates)
		}
		// The read wrote nothing: no cache write, no attempt marker, no
		// generation, no event.
		if got := server.requests(); got != fetches+1 {
			t.Fatalf("model discovery performed %d fetches, want exactly one", got-fetches)
		}
		records, _ = catalog.ReadDiscoveryCache(r.config.loader.Home())
		if !records["usablep"].AttemptedAt.Equal(attemptBefore) {
			t.Fatal("the failed-attempt marker changed on the model discovery read")
		}
		assertNoConnectionPublication(t, r, sub, generation, warnRev)

		// The typed unknown-provider error and the fetch failure class.
		if _, err := r.discoverProviderModels(ctx, "ghost-provider"); !errors.Is(err, catalog.ErrUnknownProvider) {
			t.Fatalf("unknown provider discovery = %v, want ErrUnknownProvider", err)
		}
		server.retarget(func(auth string) string { return connectionEmptyDiscovery }) // the endpoint stops serving models

		// The failed read is still the pure read: the baseline is captured
		// after the intentional retarget, and the cache attempt marker,
		// .env, config bytes, generation, warnings, and events stay unchanged.
		before, generation, warnRev := runtimeMutationBaseline(t, r)
		envBefore, rerr := os.ReadFile(r.managedEnv.Path())
		if rerr != nil {
			t.Fatalf("read .env: %v", rerr)
		}
		attemptBefore = records["usablep"].AttemptedAt
		fetches = server.requests()
		_, err = r.discoverProviderModels(ctx, "usablep")
		if err == nil || !errors.Is(err, ErrConfiguration) {
			t.Fatalf("failed model discovery = %v, want the typed refusal", err)
		}
		if got := server.requests(); got != fetches+1 {
			t.Fatalf("failed model discovery performed %d fetches, want exactly one", got-fetches)
		}
		assertConnectionRefused(t, r, sub, before, generation, warnRev, err, ErrConfiguration)
		records, _ = catalog.ReadDiscoveryCache(r.config.loader.Home())
		if !records["usablep"].AttemptedAt.Equal(attemptBefore) {
			t.Fatal("the attempt marker changed on the failed model discovery read")
		}
		assertDotenvUnchanged(t, r, envBefore, "the failed model discovery read")
	})
}

// --- nil manager ---

func TestCreateProviderNilManagedEnvTypedFailure(t *testing.T) {
	// The service without an attached manager: the create's key action is
	// the manager's own typed failure, the owning bytes are restored, and
	// no second manager is constructed on demand.
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, providerConfigFile("One"))
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, acceptValidator))
	svc.attachWarnings(newWarningStore())
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	sub, err := svc.obs.subscribe(32)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	key := "sk-live-nil-manager"
	window := 4096
	baseURL := "https://nil.test/v1"
	candidate, err := svc.mutate(context.Background(), svc.editProviderCreate("np", protocol.ProviderEdit{BaseUrl: &baseURL},
		map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key))
	if err == nil || !errors.Is(err, ErrConfiguration) || !strings.Contains(err.Error(), "managed env is not initialized") {
		t.Fatalf("nil-manager create = (%v, %+v), want the manager's typed failure", err, candidate)
	}
	// The exact prior bytes were restored and nothing was published.
	data, rerr := os.ReadFile(h.configPath)
	if rerr != nil || !strings.Contains(string(data), `"prov"`) || strings.Contains(string(data), `"np"`) {
		t.Fatalf("nil-manager create left the file = (%q, %v)", data, rerr)
	}
	if svc.current().generation != 1 {
		t.Fatalf("nil-manager create published generation %d", svc.current().generation)
	}
	assertNoEvent(t, sub)

	// resolveConnectKey without a manager or key: the absent path decides.
	if _, _, err := resolveConnectKey("CONNECTION_NIL_MGR_KEY", nil, config.EnvValue{}); err == nil {
		t.Fatal("resolveConnectKey without a manager or key succeeded")
	}

	// A connect-level key action without the retained manager is the same
	// typed failure: no publication, no event, no file write.
	live := svc.current().catalog.Providers["prov"]
	_, err = svc.mutate(context.Background(), svc.editProviderConnect(&connectionEffects{
		providerID: "prov",
		transport:  live.Transport,
		keyAction:  keyActionSet,
		keyEnv:     "CONNECTION_NIL_MGR_KEY",
		keyValue:   "sk-nil-manager-connect",
	}))
	if err == nil || !errors.Is(err, ErrConfiguration) || !strings.Contains(err.Error(), "managed env is not initialized") {
		t.Fatalf("nil-manager connect = %v, want the manager's typed failure", err)
	}
	if svc.current().generation != 1 {
		t.Fatalf("nil-manager connect published generation %d", svc.current().generation)
	}
	if data, rerr = os.ReadFile(h.configPath); rerr != nil || !strings.Contains(string(data), `"prov"`) {
		t.Fatalf("nil-manager connect changed the file = (%q, %v)", data, rerr)
	}
	assertNoEvent(t, sub)
}

// --- full chain ---

// connectEnvEchoTool is the full-chain row's command capability: it resolves
// the child's environment through the ToolContext's call-time producer
// exactly as the shipped tools plugin does, and runs one real `sh` child
// that echoes the two credential markers.
type connectEnvEchoTool struct{}

func (connectEnvEchoTool) describe(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        "env_echo",
		Description: "echoes the two marker variables",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: true}, nil
}

func (connectEnvEchoTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (connectEnvEchoTool) Prepare(_ context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	return harness.PreparedTool{
		Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "*"}},
		Execute: func(ctx context.Context) harness.ToolOutcome {
			if tc.SubprocessEnv == nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no subprocess environment producer"}}
			}
			env := tc.SubprocessEnv()
			if env == nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no environment"}}
			}
			out, err := tool.RunForegroundCommand(ctx, `printf '%s|%s' "$CONNECT_CHAIN_MANAGED" "$CONNECT_CHAIN_EXTERNAL"`, tc.Workspace, 0, 15360, 5000, "", env)
			if err != nil {
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: err.Error()}}
			}
			return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: out}}
		},
	}
}

// connectEnvEchoPlugin declares the full-chain row's tool export.
func connectEnvEchoPlugin() Plugin {
	return Plugin{
		ID:       "envecho",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("env_echo", connectEnvEchoTool{}.describe)},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"env_echo": connectEnvEchoTool{}}}, nil
		},
	}
}

// TestProductionConnectThenCommandEnvironment proves the full chain through
// the public Open with the concrete preparation: the connect operator adds a
// managed key AFTER the plugin scopes opened, and a real foreground child
// started afterwards resolves its environment through the call-time
// producer — the connected key never reaches it while a shell sibling does.
func TestProductionConnectThenCommandEnvironment(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		server := newChainServer(t,
			chainTurn{respond: func(w http.ResponseWriter) { chainToolCallTurn(w, "call-1", "env_echo", `{}`) }},
			chainTurn{respond: func(w http.ResponseWriter) { writeTextTurn(w, "done") }},
		)
		t.Setenv("CONNECT_CHAIN_EXTERNAL", "sibling-value")
		reserveEnvKey(t, "CONNECT_CHAIN_MANAGED")
		writeServiceFile(t, e.configPath, `{
  "providers": {
    "prov": {"transport": {"base_url": "`+server.URL+`", "api_key_env": "PRODUCTION_TEST_KEY"}, "discovery": false, "models": {"m": {"name": "M", "context_window": 262144}}},
    "discp": {"transport": {"base_url": "https://disc.test/v1", "api_key_env": "CONNECT_CHAIN_MANAGED"}, "discovery": false, "models": {"m": {"name": "M", "context_window": 4096}}}
  }
}`)
		writeServiceFile(t, agents.PathForConfig(e.configPath), `{"runner": {"model": "prov/m", "system_prompt": "simple", "tools": ["env_echo"]}}`)

		r, err := Open(ctx, Options{DataDir: e.dataDir, ConfigPath: e.configPath, Plugins: []Plugin{
			coreStoragePlugin(e.store), connectEnvEchoPlugin(),
		}})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)

		// The provider connects after every plugin scope opened.
		key := "chain-managed-secret"
		if _, err := r.connectProvider(ctx, "discp", &key); err != nil {
			t.Fatalf("connectProvider: %v", err)
		}

		session, err := r.createSession(ctx, e.workspace("connect-ws"), "runner")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		if err := os.MkdirAll(e.workspace("connect-ws"), 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "echo")
		awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)

		results := chainToolResults(t, e.store, session.Identity.SessionID)
		settled, ok := results["call-1"]
		if !ok || settled.Status != model.ResultSuccess {
			t.Fatalf("env_echo result = %+v (ok %v), want success", settled, ok)
		}
		if settled.Content != "|sibling-value" {
			t.Fatalf("child environment markers = %q, want the connected key scrubbed and the shell sibling present", settled.Content)
		}
	})
}

// TestConnectProviderSpecialIDsAndKeyless proves the P9 addressability row
// and the keyless-connect row: a `..` provider identity the catalog accepts
// is addressable through the connect operator, and a keyless usable provider
// connects with no key action at all — both through one ready publication
// over untouched bytes.
func TestConnectProviderSpecialIDsAndKeyless(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		server := newConnectionDiscoveryServer(t, connectionFullDiscovery)
		r, _ := openConnectionRuntime(t, store, server.URL)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		before, _, _ := runtimeMutationBaseline(t, r)

		// The keyless usable provider: no credential exists, none is needed,
		// nothing is written but the ready publication.
		mutation, err := r.connectProvider(ctx, "keylessusable", nil)
		if err != nil || mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("keyless connect = (%v, %+v), want generation 2", err, mutation)
		}
		if mutation.Result.KeySource != protocol.ProviderKeySource(config.KeySourceKeyless) || !mutation.Result.Connected {
			t.Fatalf("keyless provider view = %+v, want connected keyless", mutation.Result)
		}
		drainConnectionEvent(t, r.warnings, sub, "2")
		after, err := os.ReadFile(r.config.configPath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("keyless connect changed the owning config (%v)", err)
		}

		// The `..` identity is a retained-valid provider ID: addressable
		// through the connect operator like any other, with its own exact
		// generation, event, and untouched-config pins.
		raw := strings.Replace(connectionProvidersDocument(server.URL),
			`"keylessp"`,
			`"..": {"transport": {"base_url": "https://dots.test/v1", "api_key_env": "CONNECTION_DOTS_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}}, "keylessp"`, 1)
		writeServiceFile(t, r.config.configPath, raw)
		if _, err := r.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		drainConnectionEvent(t, r.warnings, sub, "3")
		dotsBefore, _, _ := runtimeMutationBaseline(t, r)
		reserveEnvKey(t, "CONNECTION_DOTS_KEY")
		t.Setenv("CONNECTION_DOTS_KEY", "dots-key-value")
		dotsMutation, err := r.connectProvider(ctx, "..", nil)
		if err != nil || !dotsMutation.Result.Connected || dotsMutation.Result.Id != ".." {
			t.Fatalf("dotdot connect = (%v, %+v), want the connected `..` view", err, dotsMutation)
		}
		if dotsMutation.ConfigurationRevision.Generation != "4" {
			t.Fatalf("dotdot connect generation = %q, want 4", dotsMutation.ConfigurationRevision.Generation)
		}
		drainConnectionEvent(t, r.warnings, sub, "4")
		dotsAfter, err := os.ReadFile(r.config.configPath)
		if err != nil || string(dotsAfter) != string(dotsBefore) {
			t.Fatalf("dotdot connect changed the owning config (%v)", err)
		}
	})
}
