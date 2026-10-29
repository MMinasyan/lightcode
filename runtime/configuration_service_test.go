package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// serviceHarness roots one configurationService test at a scratch HOME and a
// separate config directory, so nothing touches the real user environment and
// DataDir-side relocation is observable.
type serviceHarness struct {
	t          *testing.T
	home       string
	dataDir    string
	configPath string
	loader     *catalog.Loader
	opens      atomic.Int64
}

func newServiceHarness(t *testing.T) *serviceHarness {
	t.Helper()
	h := &serviceHarness{t: t, home: t.TempDir(), dataDir: t.TempDir()}
	// One isolated HOME for every harness consumer: the publisher's
	// Workspace-permission capture resolves the home-based projects
	// directory, so no test here ever reads the real user HOME.
	t.Setenv("HOME", h.home)
	h.configPath = filepath.Join(h.dataDir, "config.json")
	h.loader = catalog.NewLoader(h.home, staticBundledFS())
	return h
}

func (h *serviceHarness) service(owner context.Context, plugins ...Plugin) *configurationService {
	h.t.Helper()
	c, err := newComposition(plugins)
	if err != nil {
		h.t.Fatalf("newComposition: %v", err)
	}
	return newConfigurationService(owner, c, h.loader, h.configPath, newObservation())
}

func staticBundledFS() fstest.MapFS {
	return fstest.MapFS{"builtin/static.json": {Data: []byte(`{
		"id": "static",
		"transport": {"base_url": "http://static.test/v1", "api_key_env": ""},
		"discovery": false,
		"models": {"m": {"context_window": 1000}}
	}`)}}
}

// nominalProvMember builds the header-free nominal prov provider object the
// configuration fixtures share: a transport carrying only the endpoint and
// the key-env name (both encoded as JSON strings), discovery disabled, and
// the authored raw models object spliced exactly as written — its numeric
// lexemes are never decoded and re-encoded. Callers keep their own document
// roots (permissions, plugins, sessions, custom members).
func nominalProvMember(endpoint, keyEnv, models string) string {
	return `{"transport":{"base_url":` + fixtureString(endpoint) + `,"api_key_env":` + fixtureString(keyEnv) +
		`},"discovery":false,"models":` + models + `}`
}

// workerAgentsDocument is the shared one-worker agent document the
// configuration rows seed: a bare worker entry with no model selection.
const workerAgentsDocument = `{"worker":{"system_prompt":"simple"}}`

// fixtureString renders one dynamic fixture value as its JSON string literal.
// A string value always encodes, so the marshal error is impossible.
func fixtureString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func providerConfigFile(name string) string {
	return `{"providers":{"prov":` + nominalProvMember("https://prov.test/v1", "",
		`{"m":{"name":`+fixtureString(name)+`,"context_window":9007199254740993}}`) + `},"sessions":{"archive_after_days":3}}`
}

func writeServiceFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	{
		err := os.WriteFile(path, []byte(content), 0o600)
		testassert.Require(t, err == nil, "WriteFile(%s): %v", path, err)
	}
}

func servicePlugin(id string, opens *atomic.Int64, validate func(json.RawMessage) error) Plugin {
	return Plugin{
		ID:       id,
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[any]("capability." + id)},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			opens.Add(1)
			return Instance{Values: map[string]any{"capability." + id: id}}, nil
		},
		ValidateConfig: validate,
	}
}

func snapshotModelName(cfg *configuration, providerID, modelID string) string {
	provider, found := cfg.catalog.Providers[providerID]
	if !found || provider.Models[modelID] == nil {
		return ""
	}
	return provider.Models[modelID].Name
}

func hasDefinition(cfg *configuration, name string) (bool, string) {
	for _, def := range cfg.definitions {
		if def.Name == name {
			return true, def.Model
		}
	}
	return false, ""
}

// TestConfigurationServicePublishesImmutableGenerations proves the
// publication contract end to end: initial publication is generation 1, each
// success increments, a failed build preserves its source error and publishes
// nothing without consuming a generation, numbers stay exact through the
// captured assembly, and an earlier capture keeps its complete old content
// after a later publication.
func TestConfigurationServicePublishesImmutableGenerations(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, providerConfigFile("One"))
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))

	first, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	if first.snapshot.generation != 1 || (Invocation{snapshot: first.snapshot}).Revision() != "1" {
		t.Fatalf("initial publication = generation %d revision %q, want 1", first.snapshot.generation, (Invocation{snapshot: first.snapshot}).Revision())
	}
	{
		model := first.snapshot.catalog.Providers["prov"].Models["m"]
		testassert.Require(t, model.ContextWindow == 9007199254740993, "captured model = %+v, want the exact number preserved through the assembly", model)
	}
	testassert.Require(t, first.snapshot.sessions.AutoArchive && first.snapshot.sessions.ArchiveAfterDays == 3 && len(first.snapshot.catalogWarnings) == 0, "sessions = %+v, want the decoded policy over the defaults; catalog warnings = %#v, want none", first.snapshot.sessions, first.snapshot.catalogWarnings)
	testassert.Require(t, svc.current() == first.snapshot, "current() does not return the published snapshot")

	writeServiceFile(t, h.configPath, providerConfigFile("Two"))
	second, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	if second.snapshot.generation != 2 || snapshotModelName(second.snapshot, "prov", "m") != "Two" {
		t.Fatalf("reload = generation %d name %q, want 2/Two", second.snapshot.generation, snapshotModelName(second.snapshot, "prov", "m"))
	}
	testassert.Require(t, svc.current() == second.snapshot, "current() does not return the newest publication")
	if first.snapshot.generation != 1 || snapshotModelName(first.snapshot, "prov", "m") != "One" {
		t.Fatalf("the old capture changed: generation %d name %q", first.snapshot.generation, snapshotModelName(first.snapshot, "prov", "m"))
	}

	writeServiceFile(t, h.configPath, `{not json`)
	failed, err := svc.publish(context.Background())
	testassert.Require(t, failed.snapshot == nil && errors.Is(err, ErrConfiguration), "failed publish = (%v, %v), want a nil candidate and ErrConfiguration", failed, err)
	testassert.Require(t, strings.Contains(err.Error(), "decode captured configuration"), "failed publish error = %v, want the source error preserved", err)
	testassert.Require(t, svc.current() == second.snapshot, "a failed build published something")

	writeServiceFile(t, h.configPath, providerConfigFile("Three"))
	third, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	testassert.Equal(t, third.snapshot.generation, 3)
}

// TestConfigurationServiceCapturesInputOnce proves one build consumes exactly
// one input set: bytes changed on disk during the build are not visible to
// that candidate (no mixed rereads), while the next build captures them.
func TestConfigurationServiceCapturesInputOnce(t *testing.T) {
	h := newServiceHarness(t)
	agentsPath := filepath.Join(h.dataDir, "agents.json")
	writeServiceFile(t, h.configPath, providerConfigFile("One"))
	writeServiceFile(t, agentsPath, `{"early":{"model":"prov/m"}}`)

	rewrite := func(json.RawMessage) error {
		writeServiceFile(t, h.configPath, providerConfigFile("Two"))
		writeServiceFile(t, agentsPath, `{"late":{"model":"prov/m"}}`)
		return nil
	}
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, rewrite))

	first, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	if snapshotModelName(first.snapshot, "prov", "m") != "One" {
		t.Fatalf("the in-flight candidate reread the main configuration: name %q", snapshotModelName(first.snapshot, "prov", "m"))
	}
	{
		early, _ := hasDefinition(first.snapshot, "early")
		testassert.Require(t, early, "the captured agent definitions were replaced mid-build")
	}
	{
		late, _ := hasDefinition(first.snapshot, "late")
		testassert.Require(t, !late, "the in-flight candidate reread the agent definitions")
	}

	second, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	if snapshotModelName(second.snapshot, "prov", "m") != "Two" {
		t.Fatalf("the reload did not capture the current bytes: name %q", snapshotModelName(second.snapshot, "prov", "m"))
	}
	{
		found, model := hasDefinition(second.snapshot, "late")
		testassert.Require(t, found && model == "prov/m", "late definition = (%v, %q), want the freshly captured bytes", found, model)
	}
}

// TestConfigurationServicePluginValidationRejectsPublication proves the
// plugin section contract against the compiled declarations: unknown IDs and
// non-object values reject the candidate, each validator's source error is
// preserved, nothing publishes, and no factory runs. The positive siblings
// (absent section passes nil, a no-settings plugin accepts {}, and a missing
// model selection stays startable) must publish.
func TestConfigurationServicePluginValidationRejectsPublication(t *testing.T) {
	sentinel := errors.New("alpha settings invalid")
	for _, row := range []struct {
		name       string
		config     string
		validate   func(json.RawMessage) error
		wantSource string
		wantIs     error
	}{
		{name: "unknown plugin ID", config: `{"plugins":{"alpha":{},"ghost":{}}}`, wantSource: "ghost"},
		{name: "value is not an object", config: `{"plugins":{"alpha":[1]}}`, wantSource: "not a JSON object"},
		{name: "value is null", config: `{"plugins":{"alpha":null}}`, wantSource: "not a JSON object"},
		{name: "validator failure", config: `{"plugins":{"alpha":{"x":1}}}`, validate: func(json.RawMessage) error { return sentinel }, wantIs: sentinel},
		{name: "no-settings violation", config: `{"plugins":{"alpha":{"x":true}}}`, wantSource: "declares no settings validator"},
	} {
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, row.config)
		svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, row.validate))
		candidate, err := svc.publish(context.Background())
		testassert.Require(t, candidate.snapshot == nil && errors.Is(err, ErrConfiguration), "%s: publish = (%v, %v), want a rejected candidate", row.name, candidate, err)
		testassert.Require(t, (row.wantIs == nil || errors.Is(err, row.wantIs)) && (row.wantSource == "" || strings.Contains(err.Error(), row.wantSource)), "%s: error %v does not preserve the validator source error; %s: error %v does not preserve %q", row.name, err, row.name, err, row.wantSource)
		testassert.Require(t, svc.current() == nil, "%s: a rejected candidate published a snapshot", row.name)
		if h.opens.Load() != 0 {
			t.Fatalf("%s: rejection ran %d factories", row.name, h.opens.Load())
		}
	}

	for _, row := range []struct {
		name     string
		config   string
		validate func(json.RawMessage) error
	}{
		{name: "absent section passes nil", config: `{}`},
		{name: "no-settings accepts the empty object", config: `{"plugins":{"alpha":{}}}`},
	} {
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, row.config)
		var seen atomic.Pointer[json.RawMessage]
		validate := row.validate
		if validate == nil {
			validate = func(raw json.RawMessage) error {
				clone := append(json.RawMessage(nil), raw...)
				seen.Store(&clone)
				return nil
			}
		}
		svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, validate))
		{
			_, err := svc.publish(context.Background())
			testassert.Require(t, err == nil, "%s: publish: %v", row.name, err)
		}
		if row.name == "absent section passes nil" {
			if got := seen.Load(); got == nil || len(*got) != 0 {
				t.Fatalf("%s: the plugin received %q, want nil", row.name, string(*got))
			}
		}
		testassert.Require(t, h.opens.Load() == 0, "%s: configuration publication ran a factory", row.name)
	}
}

// pluginsConfigFile is providerConfigFile with one plugin section verbatim.
func pluginsConfigFile(alpha string) string {
	return `{"providers":{"prov":` + nominalProvMember("https://prov.test/v1", "", `{"m":{"name":"One","context_window":100}}`) + `},"plugins":{"alpha":` + alpha + `}}`
}

// TestConfigurationServiceValidatorScratchCannotChangePublication proves the
// owned-validator-input rows: a synchronous validator that uses its received
// bytes as an in-place scratch buffer cannot change the retained candidate,
// the published snapshot, or a later Invocation.Config handout; absent
// settings still deliver nil to the validator; and a tampering rejected
// candidate notifies nothing while leaving the prior pointer, generation,
// and retained bytes intact.
func TestConfigurationServiceValidatorScratchCannotChangePublication(t *testing.T) {
	const admitted = `{"value":"original"}`
	t.Run("in-place scratch never reaches the publication", func(t *testing.T) {
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, pluginsConfigFile(admitted))
		svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, func(raw json.RawMessage) error {
			tamperScratch(raw, "original", "tampered")
			return nil
		}))
		pub, err := svc.publish(context.Background())
		testassert.NoError(t, err)
		testassert.Require(t, string(pub.snapshot.plugins["alpha"]) == admitted, "published snapshot bytes = %s, want the retained candidate", pub.snapshot.plugins["alpha"])
		{
			got := (Invocation{snapshot: pub.snapshot}).Config("alpha")
			testassert.Require(t, string(got) == admitted, "Invocation.Config = %s, want the retained candidate", got)
		}
	})
	t.Run("absent settings still pass nil through", func(t *testing.T) {
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, providerConfigFile("One"))
		sawNil := false
		svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, func(raw json.RawMessage) error {
			sawNil = raw == nil
			return nil
		}))
		{
			_, err := svc.publish(context.Background())
			testassert.NoError(t, err)
		}
		testassert.Require(t, sawNil, "absent settings reached the validator as something other than nil")
	})
	t.Run("rejected tampering leaves the prior publication intact", func(t *testing.T) {
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, pluginsConfigFile(admitted))
		var attempts atomic.Int32
		svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, func(raw json.RawMessage) error {
			tamperScratch(raw, "original", "tampered")
			if attempts.Add(1) == 1 {
				return nil
			}
			return errValidator
		}))
		sub, err := svc.obs.subscribe(4)
		testassert.NoError(t, err)
		t.Cleanup(sub.Close)
		first, err := svc.publish(context.Background())
		testassert.NoError(t, err)
		nextConfigurationEvent(t, sub, "1")
		{
			_, err := svc.publish(context.Background())
			testassert.Require(t, errors.Is(err, ErrConfiguration) && errors.Is(err, errValidator), "reload = %v, want a rejection preserving the validator source error", err)
		}
		select {
		case event, ok := <-sub.Events():
			testassert.Require(t, !ok, "rejected build published event %+v, want silence", event)
			t.Fatal("the subscription closed around the rejected build, want silence with the subscription open")
		default:
		}
		testassert.Require(t, svc.current() == first.snapshot, "a rejected candidate replaced the prior publication")
		testassert.Require(t, first.snapshot.generation == 1 && string(first.snapshot.plugins["alpha"]) == admitted, "prior snapshot = generation %d bytes %s, want generation 1 with the retained bytes", first.snapshot.generation, first.snapshot.plugins["alpha"])
	})
}

// TestConfigurationServiceCancellationBetweenValidatorsStopsTheRest proves a
// cancellation observed between finite validators prevents later validation
// and publication, and the context error stays unwrapped.
func TestConfigurationServiceCancellationBetweenValidatorsStopsTheRest(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	var lastRan atomic.Bool
	svc := h.service(context.Background(),
		servicePlugin("first", &h.opens, nil),
		servicePlugin("second", &h.opens, func(json.RawMessage) error { cancel(); return nil }),
		servicePlugin("third", &h.opens, func(json.RawMessage) error { lastRan.Store(true); return nil }),
	)
	candidate, err := svc.publish(ctx)
	testassert.Require(t, candidate.snapshot == nil && errors.Is(err, context.Canceled), "publish = (%v, %v), want the caller context error", candidate, err)
	testassert.Require(t, !errors.Is(err, ErrConfiguration) && !errors.Is(err, ErrClosed), "cancellation was classified as %v, want unwrapped", err)
	testassert.Require(t, !lastRan.Load(), "validation continued past the observed cancellation")
	testassert.Require(t, svc.current() == nil, "a canceled build published a snapshot")
}

// TestConfigurationServiceChecksCancellationBeforePublication proves the
// checks around the serialized build: a pre-canceled caller returns without
// waiting or reading inputs, owner cancellation reports ErrClosed without
// publication, a cancellation observed after the last validator is reported
// immediately before the atomic Store without a revision or event, and a
// caller done together with the owner still reports its own context error
// under the caller-first checkpoints.
func TestConfigurationServiceChecksCancellationBeforePublication(t *testing.T) {
	t.Run("caller canceled before waiting", func(t *testing.T) {
		h := newServiceHarness(t)
		svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		{
			_, err := svc.publish(ctx)
			testassert.Require(t, errors.Is(err, context.Canceled), "publish = %v, want the caller context error", err)
		}
		{
			_, err := os.Stat(h.configPath)
			testassert.Require(t, os.IsNotExist(err), "a pre-canceled publish started input reads: %v", err)
		}
	})
	t.Run("owner canceled before waiting", func(t *testing.T) {
		owner, cancelOwner := context.WithCancel(context.Background())
		h := newServiceHarness(t)
		svc := h.service(owner, servicePlugin("alpha", &h.opens, nil))
		cancelOwner()
		{
			_, err := svc.publish(context.Background())
			testassert.Require(t, errors.Is(err, ErrClosed), "publish = %v, want ErrClosed for the canceled owner", err)
		}
		{
			_, err := os.Stat(h.configPath)
			testassert.Require(t, os.IsNotExist(err), "an owner-canceled publish started input reads: %v", err)
		}
	})
	t.Run("caller canceled after the last validator", func(t *testing.T) {
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, `{}`)
		ctx, cancel := context.WithCancel(context.Background())
		svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, func(json.RawMessage) error {
			cancel()
			return nil
		}))
		sub, err := svc.obs.subscribe(4)
		testassert.NoError(t, err)
		t.Cleanup(sub.Close)
		candidate, err := svc.publish(ctx)
		testassert.Require(t, candidate.snapshot == nil && errors.Is(err, context.Canceled), "publish = (%v, %v), want the Store check to reject the completed candidate", candidate, err)
		testassert.Require(t, svc.current() == nil, "the Store check published anyway")
		select {
		case event, ok := <-sub.Events():
			testassert.Require(t, !ok, "the canceled publication emitted %+v, want event silence", event)
			t.Fatal("the subscription closed around the canceled publication, want silence with the subscription open")
		default:
		}
	})
	t.Run("caller and owner both done reports the caller error", func(t *testing.T) {
		owner, cancelOwner := context.WithCancel(context.Background())
		caller, cancelCaller := context.WithCancel(context.Background())
		h := newServiceHarness(t)
		svc := h.service(owner, servicePlugin("alpha", &h.opens, nil))
		sub, err := svc.obs.subscribe(4)
		testassert.NoError(t, err)
		t.Cleanup(sub.Close)
		cancelOwner()
		cancelCaller()
		candidate, err := svc.publish(caller)
		testassert.Require(t, candidate.snapshot == nil && errors.Is(err, context.Canceled), "both-done publish = (%v, %v), want the caller's own context error from the caller-first checkpoint", candidate, err)
		testassert.Require(t, !errors.Is(err, ErrClosed), "both-done publish = %v, want the caller error, not the owner-lifecycle identity", err)
		select {
		case event, ok := <-sub.Events():
			testassert.Require(t, !ok, "the canceled publication emitted %+v, want event silence", event)
			t.Fatal("the subscription closed around the canceled publication, want silence with the subscription open")
		default:
		}
		testassert.Require(t, svc.current() == nil, "a canceled publication stored a snapshot")
		{
			_, err := os.Stat(h.configPath)
			testassert.Require(t, os.IsNotExist(err), "a both-done publish started input reads: %v", err)
		}
	})
	t.Run("owner canceled after the last validator", func(t *testing.T) {
		owner, cancelOwner := context.WithCancel(context.Background())
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, `{}`)
		svc := h.service(owner, servicePlugin("alpha", &h.opens, func(json.RawMessage) error {
			cancelOwner()
			return nil
		}))
		candidate, err := svc.publish(context.Background())
		testassert.Require(t, candidate.snapshot == nil && errors.Is(err, ErrClosed), "publish = (%v, %v), want ErrClosed from the Store check", candidate, err)
		testassert.Require(t, svc.current() == nil, "an owner-canceled publication stored a snapshot")
	})
}

// firstCheckContext is a waiter-check rendezvous: the first Err call captures
// the wrapped context's current result, signals that the check ran, and
// blocks until released before returning the captured result, so the first
// check cannot consume a cancellation issued after the signal. Every later
// Err call delegates to the wrapped context. Both calls arrive from the one
// publish goroutine.
type firstCheckContext struct {
	context.Context
	checked     chan struct{}
	release     chan struct{}
	intercepted bool
	captured    error
}

func (c *firstCheckContext) Err() error {
	if !c.intercepted {
		c.intercepted = true
		c.captured = c.Context.Err()
		close(c.checked)
		<-c.release
		return c.captured
	}
	return c.Context.Err()
}

// TestConfigurationServiceSerializesBuildsAndRejectsCanceledWaiters proves
// one build mutex: a second caller waits for the active builder, its
// cancellation while waiting returns its own error without starting another
// build, readers never observe a torn state, and a pre-canceled caller
// returns without waiting for the lock at all.
func TestConfigurationServiceSerializesBuildsAndRejectsCanceledWaiters(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, providerConfigFile("One"))
	var builds atomic.Int64
	gate := make(chan struct{})
	entered := make(chan struct{})
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, func(json.RawMessage) error {
		if builds.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return nil
	}))

	type result struct {
		cfg configurationCapture
		err error
	}
	buildA := make(chan result, 1)
	go func() {
		cfg, err := svc.publish(context.Background())
		buildA <- result{cfg, err}
	}()
	<-entered

	// The active builder has already captured its bytes; replacing the file
	// now makes any second build that skipped the post-lock cancellation
	// check fail its input read instead of returning the canceled waiter's
	// own context error.
	dangling := h.configPath + ".stale"
	testassert.NoError(t, os.Rename(h.configPath, dangling))
	testassert.NoError(t, os.Mkdir(h.configPath, 0o700))

	readersStop := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-readersStop:
					return
				default:
					if snapshot := svc.current(); snapshot != nil && snapshot.generation == 0 {
						t.Error("a reader observed a zero-generation snapshot")
						return
					}
				}
			}
		}()
	}

	deadlineCtx, cancelDeadline := context.WithCancel(context.Background())
	cancelDeadline()
	immediate := make(chan error, 1)
	go func() { immediate <- func() error { _, err := svc.publish(deadlineCtx); return err }() }()
	select {
	case err := <-immediate:
		testassert.Require(t, errors.Is(err, context.Canceled), "pre-canceled publish = %v, want its context error", err)
	case <-time.After(5 * time.Second):
		t.Fatal("a pre-canceled caller waited for the build mutex")
	}

	waitCtx, cancelWait := context.WithCancel(context.Background())
	// The waiter's context intercepts its first cancellation check: it
	// captures the current result, signals that the check ran, and blocks
	// until released. The active builder still holds the mutex while the
	// waiter passes that first check, so cancellation issued between the
	// signal and the release can only be observed by the post-lock check;
	// the hidden input proves any build that skips it never starts.
	waiter := &firstCheckContext{Context: waitCtx, checked: make(chan struct{}), release: make(chan struct{})}
	waiting := make(chan result, 1)
	go func() {
		cfg, err := svc.publish(waiter)
		waiting <- result{cfg, err}
	}()
	<-waiter.checked
	cancelWait()
	close(waiter.release)
	close(gate)

	first := <-buildA
	testassert.Require(t, first.err == nil && first.cfg.snapshot.generation == 1, "active build: %v; got %v, want %v", first.err, first.cfg.snapshot.generation, 1)
	{
		waiter := <-waiting
		testassert.Require(t, waiter.cfg.snapshot == nil && errors.Is(waiter.err, context.Canceled), "canceled waiter = (%v, %v), want its own context error without starting another build", waiter.cfg, waiter.err)
	}
	testassert.Equal(t, builds.Load(), 1)
	testassert.NoError(t, os.Remove(h.configPath))
	testassert.NoError(t, os.Rename(dangling, h.configPath))
	second, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	testassert.Equal(t, second.snapshot.generation, 2)
	close(readersStop)
	readers.Wait()
}

// TestConfigurationServiceGenerationOverflow proves an exhausted generation
// space returns ErrConfiguration without publication.
func TestConfigurationServiceGenerationOverflow(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, providerConfigFile("One"))
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))
	exhausted := &configuration{generation: math.MaxUint64}
	svc.published.Store(exhausted)

	candidate, err := svc.publish(context.Background())
	testassert.Require(t, candidate.snapshot == nil && errors.Is(err, ErrConfiguration), "overflow publish = (%v, %v), want ErrConfiguration without publication", candidate, err)
	testassert.Require(t, svc.current() == exhausted, "the overflow failure replaced the published snapshot")
}

// TestConfigurationServiceKeepsFirstRunSkeletons proves a fresh machine
// build creates exactly the retained first-run skeleton contents and starts
// successfully on their defaults, including a selected Agent whose model is
// wholly missing.
func TestConfigurationServiceKeepsFirstRunSkeletons(t *testing.T) {
	h := newServiceHarness(t)
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))
	first, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	if configData, err := os.ReadFile(h.configPath); err != nil || string(configData) != mainConfigSkeleton {
		t.Fatalf("first-run config = %q (%v), want the retained skeleton", string(configData), err)
	}
	if agentsData, err := os.ReadFile(filepath.Join(h.dataDir, "agents.json")); err != nil || string(agentsData) != agentsSkeleton {
		t.Fatalf("first-run agents = %q (%v), want the retained skeleton", string(agentsData), err)
	}
	testassert.Equal(t, len(first.snapshot.definitions), 5)

	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{"plain":{}}`)
	second, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	{
		found, model := hasDefinition(second.snapshot, "plain")
		testassert.Require(t, found && model == "", "plain definition = (%v, %q), want the roster with a wholly empty model", found, model)
	}
}

// TestConfigurationServiceKeepsHomeBasedCachePaths proves neither an
// alternate ConfigPath nor a separate data root relocates the discovery
// cache: the publication and its TTL record stay under the once-resolved
// home, the service creates no dotenv itself, and the data directory holds
// exactly the captured input files.
func TestConfigurationServiceKeepsHomeBasedCachePaths(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"remote-model","name":"Remote Model","context_window":16384}]}`))
	}))
	t.Cleanup(server.Close)
	bundled := fstest.MapFS{"builtin/remote.json": {Data: []byte(`{
		"id": "remote",
		"transport": {"base_url": "` + server.URL + `/v1", "api_key_env": ""},
		"discovery": true,
		"models": {}
	}`)}}

	h := newServiceHarness(t)
	h.home = home
	h.loader = catalog.NewLoader(home, bundled)
	h.configPath = configPath
	writeServiceFile(t, configPath, `{}`)
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))
	snapshot, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	testassert.Require(t, snapshot.snapshot.catalog.Providers["remote"].Models["remote-model"] != nil, "the discovery publication never reached the assembled catalog")
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json"))
		testassert.NoError(t, err)
	}
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", ".env"))
		testassert.Require(t, os.IsNotExist(err), "the configuration build touched the dotenv source: %v", err)
	}
	entries, err := os.ReadDir(dataDir)
	testassert.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	testassert.Require(t, strings.Join(names, ",") == "agents.json,config.json", "data directory entries = %q, want only the captured input files", names)
}

// TestConfigurationServiceRestartsGenerationWithFreshCaptures proves the
// generation labels one Runtime lifetime: a fresh service starts again at 1,
// its content comes from the current bytes, and the previous lifetime's
// capture remains readable and unchanged under its same label.
func TestConfigurationServiceRestartsGenerationWithFreshCaptures(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, providerConfigFile("One"))
	before, err := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil)).publish(context.Background())
	testassert.NoError(t, err)

	writeServiceFile(t, h.configPath, providerConfigFile("Two"))
	after, err := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil)).publish(context.Background())
	testassert.NoError(t, err)
	testassert.Equal(t, after.snapshot.generation, 1)
	if (Invocation{snapshot: after.snapshot}).Revision() != "1" || snapshotModelName(after.snapshot, "prov", "m") != "Two" {
		t.Fatalf("post-restart capture = %q/%q, want revision 1 with the fresh content", (Invocation{snapshot: after.snapshot}).Revision(), snapshotModelName(after.snapshot, "prov", "m"))
	}
	testassert.Require(t, (Invocation{snapshot: before.snapshot}).Revision() == "1" && snapshotModelName(before.snapshot, "prov", "m") == "One", "the old-lifetime capture changed after the restart")
}

const captureGlobalAllow = `{"rules":[{"permission":"file.write","target":"*","access":"allow"}]}`

func captureConfigDoc() string {
	return `{"providers":{"prov":` + nominalProvMember("https://prov.test/v1", "", `{"m":{"name":"One","context_window":100}}`) + `},"permissions":` + captureGlobalAllow + `}`
}

// TestConfigurationServiceCapturesWorkspacePermissions proves the
// permission-inventory rows over startup and reload: a missing inventory is
// the fresh-install absent case, a present readable file is captured and
// resolved against the captured global member, files added, changed or
// removed later require Reload, the earlier revision keeps its complete old
// capture, an unreadable file is absent policy for its Workspace alone, a
// malformed file is built-in fallback without failing publication, and a
// failed inventory enumeration leaves the whole Workspace level absent.
func TestConfigurationServiceCapturesWorkspacePermissions(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, captureConfigDoc())
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))

	var builtin harness.PermissionPolicy
	globalOnly := harness.ResolvePermissionPolicy(json.RawMessage(captureGlobalAllow), nil)

	// Startup on a fresh machine: no projects inventory, the Workspace level
	// absent and the captured global member deciding.
	first, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	{
		got := first.snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, globalOnly), "fresh-install policy = %#v, want the global-only capture", got)
	}

	// A later file requires Reload: the old revision is untouched and the
	// reload resolves the workspace rule over the captured global member.
	dirID, err := config.WorkspacePermissionDirID("/ws")
	testassert.NoError(t, err)
	permissionPath := filepath.Join(h.home, ".lightcode", "projects", dirID, "permissions.json")
	writeServiceFile(t, permissionPath, `{"rules":[{"permission":"file.write","target":"*","access":"deny"}]}`)
	{
		got := first.snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, globalOnly), "policy changed without Reload: %#v", got)
	}
	second, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	want := harness.ResolvePermissionPolicy(json.RawMessage(captureGlobalAllow), json.RawMessage(`{"rules":[{"permission":"file.write","target":"*","access":"deny"}]}`))
	{
		got := second.snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, want), "reloaded policy = %#v, want the workspace deny over the global allow", got)
	}
	{
		got := first.snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, globalOnly), "the old revision's capture changed after Reload: %#v", got)
	}

	// An unreadable file is absent policy for that Workspace alone; the
	// global member still decides and publication succeeds.
	testassert.NoError(t, os.Chmod(permissionPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(permissionPath, 0o600) })
	third, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	{
		got := third.snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, globalOnly), "unreadable-file policy = %#v, want the global-only level", got)
	}
	{
		got := second.snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, want), "the unreadable file changed the prior revision: %#v", got)
	}
	// A successfully read malformed file keeps publication and follows the
	// built-in fallback posture for that Workspace, discarding both user
	// levels there.
	testassert.NoError(t, os.Chmod(permissionPath, 0o600))
	writeServiceFile(t, permissionPath, `NOT JSON`)
	fourth, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	{
		got := fourth.snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, builtin), "malformed-file policy = %#v, want the built-in fallback", got)
	}

	// A failed enumeration of the inventory directory leaves the whole
	// Workspace policy level absent for the revision, uniformly, without
	// failing publication.
	testassert.NoError(t, os.Chmod(filepath.Join(h.home, ".lightcode", "projects"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(h.home, ".lightcode", "projects"), 0o700) })
	fifth, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	{
		got := fifth.snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, globalOnly), "failed-enumeration policy = %#v, want the uniform absent Workspace level", got)
	}
	{
		got := fifth.snapshot.permissionPolicy("/elsewhere")
		testassert.Require(t, reflect.DeepEqual(got, globalOnly), "other-workspace policy under a failed enumeration = %#v, want the uniform absent level", got)
	}

	// A file removed again is absent policy at the reload that observes it.
	testassert.NoError(t, os.Chmod(filepath.Join(h.home, ".lightcode", "projects"), 0o700))
	testassert.NoError(t, os.Remove(permissionPath))
	sixth, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	{
		got := sixth.snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, globalOnly), "removed-file policy = %#v, want the absent Workspace level", got)
	}
	{
		got := fifth.snapshot.permissionPolicy("/ws")
		testassert.Require(t, reflect.DeepEqual(got, globalOnly), "the removed file changed the prior revision: %#v", got)
	}
}

// TestConfigurationServiceUnreadableMainDocumentFailsPublication proves the
// global policy source follows the existing publication-failure path: an
// unreadable main configuration document rejects the whole candidate and
// publishes nothing.
func TestConfigurationServiceUnreadableMainDocumentFailsPublication(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, providerConfigFile("One"))
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	testassert.NoError(t, os.Remove(h.configPath))
	testassert.NoError(t, os.Mkdir(h.configPath, 0o700))
	t.Cleanup(func() {
		_ = os.Remove(h.configPath)
	})
	candidate, err := svc.publish(context.Background())
	testassert.Require(t, candidate.snapshot == nil && errors.Is(err, ErrConfiguration), "publish = (%v, %v), want the unreadable document to fail publication", candidate, err)
	testassert.Require(t, strings.Contains(err.Error(), "read main configuration"), "error = %v, want the retained source path", err)
}

// TestConfigurationServiceToolDeclarationsStayDeclarative proves the
// description/factory boundary: a Tool provider's compiled declaration feeds
// the agent parser's tool universe and its description function and factory
// both stay untouched by configuration publication.
func TestConfigurationServiceToolDeclarationsStayDeclarative(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, providerConfigFile("One"))
	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{"worker":{"tools":["tool.x"]}}`)
	describeCalled := false
	toolPlugin := Plugin{
		ID:    "tooldecl",
		Scope: ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("tool.x", func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
			describeCalled = true
			return ToolDescription{Definition: model.ToolDefinition{Name: "tool.x", Parameters: json.RawMessage(`{}`)}}, nil
		})},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			h.opens.Add(1)
			return Instance{Values: map[string]any{"tool.x": noopTool{}}}, nil
		},
	}
	svc := h.service(context.Background(), toolPlugin)
	snapshot, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	// The declared tool ID validated the configured reference and produced no
	// warning.
	found, _ := hasDefinition(snapshot.snapshot, "worker")
	testassert.Require(t, found && len(snapshot.snapshot.agentWarnings) == 0, "worker retained = %v with warnings %+v, want the declared tool to validate cleanly", found, snapshot.snapshot.agentWarnings)
	// An undeclared reference drops the definition with the retained warning.
	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{"worker":{"tools":["ghost_tool"]}}`)
	snapshot, err = svc.publish(context.Background())
	testassert.NoError(t, err)
	{
		found, _ := hasDefinition(snapshot.snapshot, "worker")
		testassert.Require(t, !found, "the unknown tool name did not drop its definition")
	}
	testassert.Require(t, len(snapshot.snapshot.agentWarnings) == 1 && snapshot.snapshot.agentWarnings[0].Kind == "invalid_agent_type" && strings.Contains(snapshot.snapshot.agentWarnings[0].Message, "ghost_tool"), "warnings = %+v, want the retained invalid-agent drop naming ghost_tool", snapshot.snapshot.agentWarnings)
	testassert.Require(t, !describeCalled, "configuration publication ran the description function")
	if h.opens.Load() != 0 {
		t.Fatalf("%d factories ran during configuration publication", h.opens.Load())
	}
}
