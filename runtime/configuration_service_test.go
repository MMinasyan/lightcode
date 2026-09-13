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

func providerConfigFile(name string) string {
	return `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"` +
		name + `","context_window":9007199254740993}}}},"sessions":{"archive_after_days":3}}`
}

func writeServiceFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
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
	if err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	if first.generation != 1 || (Invocation{snapshot: first}).Revision() != "1" {
		t.Fatalf("initial publication = generation %d revision %q, want 1", first.generation, (Invocation{snapshot: first}).Revision())
	}
	if model := first.catalog.Providers["prov"].Models["m"]; model.ContextWindow != 9007199254740993 {
		t.Fatalf("captured model = %+v, want the exact number preserved through the assembly", model)
	}
	if !first.sessions.AutoArchive || first.sessions.ArchiveAfterDays != 3 {
		t.Fatalf("sessions = %+v, want the decoded policy over the defaults", first.sessions)
	}
	if len(first.catalogWarnings) != 0 {
		t.Fatalf("catalog warnings = %#v, want none", first.catalogWarnings)
	}
	if svc.current() != first {
		t.Fatal("current() does not return the published snapshot")
	}

	writeServiceFile(t, h.configPath, providerConfigFile("Two"))
	second, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if second.generation != 2 || snapshotModelName(second, "prov", "m") != "Two" {
		t.Fatalf("reload = generation %d name %q, want 2/Two", second.generation, snapshotModelName(second, "prov", "m"))
	}
	if svc.current() != second {
		t.Fatal("current() does not return the newest publication")
	}
	if first.generation != 1 || snapshotModelName(first, "prov", "m") != "One" {
		t.Fatalf("the old capture changed: generation %d name %q", first.generation, snapshotModelName(first, "prov", "m"))
	}

	writeServiceFile(t, h.configPath, `{not json`)
	failed, err := svc.publish(context.Background())
	if failed != nil || !errors.Is(err, ErrConfiguration) {
		t.Fatalf("failed publish = (%v, %v), want a nil candidate and ErrConfiguration", failed, err)
	}
	if !strings.Contains(err.Error(), "decode captured configuration") {
		t.Fatalf("failed publish error = %v, want the source error preserved", err)
	}
	if svc.current() != second {
		t.Fatal("a failed build published something")
	}

	writeServiceFile(t, h.configPath, providerConfigFile("Three"))
	third, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("publish after failure: %v", err)
	}
	if third.generation != 3 {
		t.Fatalf("generation = %d after a failed build, want the failed build to consume nothing (3)", third.generation)
	}
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
	if err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	if snapshotModelName(first, "prov", "m") != "One" {
		t.Fatalf("the in-flight candidate reread the main configuration: name %q", snapshotModelName(first, "prov", "m"))
	}
	if early, _ := hasDefinition(first, "early"); !early {
		t.Fatal("the captured agent definitions were replaced mid-build")
	}
	if late, _ := hasDefinition(first, "late"); late {
		t.Fatal("the in-flight candidate reread the agent definitions")
	}

	second, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if snapshotModelName(second, "prov", "m") != "Two" {
		t.Fatalf("the reload did not capture the current bytes: name %q", snapshotModelName(second, "prov", "m"))
	}
	if found, model := hasDefinition(second, "late"); !found || model != "prov/m" {
		t.Fatalf("late definition = (%v, %q), want the freshly captured bytes", found, model)
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
		if candidate != nil || !errors.Is(err, ErrConfiguration) {
			t.Fatalf("%s: publish = (%v, %v), want a rejected candidate", row.name, candidate, err)
		}
		if row.wantIs != nil && !errors.Is(err, row.wantIs) {
			t.Fatalf("%s: error %v does not preserve the validator source error", row.name, err)
		}
		if row.wantSource != "" && !strings.Contains(err.Error(), row.wantSource) {
			t.Fatalf("%s: error %v does not preserve %q", row.name, err, row.wantSource)
		}
		if svc.current() != nil {
			t.Fatalf("%s: a rejected candidate published a snapshot", row.name)
		}
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
		if _, err := svc.publish(context.Background()); err != nil {
			t.Fatalf("%s: publish: %v", row.name, err)
		}
		if row.name == "absent section passes nil" {
			if got := seen.Load(); got == nil || len(*got) != 0 {
				t.Fatalf("%s: the plugin received %q, want nil", row.name, string(*got))
			}
		}
		if h.opens.Load() != 0 {
			t.Fatalf("%s: configuration publication ran a factory", row.name)
		}
	}
}

// pluginsConfigFile is providerConfigFile with one plugin section verbatim.
func pluginsConfigFile(alpha string) string {
	return `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"One","context_window":100}}}},"plugins":{"alpha":` + alpha + `}}`
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
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if string(pub.plugins["alpha"]) != admitted {
			t.Fatalf("published snapshot bytes = %s, want the retained candidate", pub.plugins["alpha"])
		}
		if got := (Invocation{snapshot: pub}).Config("alpha"); string(got) != admitted {
			t.Fatalf("Invocation.Config = %s, want the retained candidate", got)
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
		if _, err := svc.publish(context.Background()); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if !sawNil {
			t.Fatal("absent settings reached the validator as something other than nil")
		}
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
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		t.Cleanup(sub.Close)
		first, err := svc.publish(context.Background())
		if err != nil {
			t.Fatalf("initial publish: %v", err)
		}
		if event, ok := nextEvent(t, sub); !ok || event.Kind != EventConfiguration || event.ConfigurationRevision != "1" {
			t.Fatalf("initial publication event = %+v (ok=%v), want the generation 1 configuration event", event, ok)
		}
		if _, err := svc.publish(context.Background()); !errors.Is(err, ErrConfiguration) || !errors.Is(err, errValidator) {
			t.Fatalf("reload = %v, want a rejection preserving the validator source error", err)
		}
		select {
		case event, ok := <-sub.Events():
			if ok {
				t.Fatalf("rejected build published event %+v, want silence", event)
			}
			t.Fatal("the subscription closed around the rejected build, want silence with the subscription open")
		default:
		}
		if svc.current() != first {
			t.Fatal("a rejected candidate replaced the prior publication")
		}
		if first.generation != 1 || string(first.plugins["alpha"]) != admitted {
			t.Fatalf("prior snapshot = generation %d bytes %s, want generation 1 with the retained bytes", first.generation, first.plugins["alpha"])
		}
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
	if candidate != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("publish = (%v, %v), want the caller context error", candidate, err)
	}
	if errors.Is(err, ErrConfiguration) || errors.Is(err, ErrClosed) {
		t.Fatalf("cancellation was classified as %v, want unwrapped", err)
	}
	if lastRan.Load() {
		t.Fatal("validation continued past the observed cancellation")
	}
	if svc.current() != nil {
		t.Fatal("a canceled build published a snapshot")
	}
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
		if _, err := svc.publish(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("publish = %v, want the caller context error", err)
		}
		if _, err := os.Stat(h.configPath); !os.IsNotExist(err) {
			t.Fatalf("a pre-canceled publish started input reads: %v", err)
		}
	})
	t.Run("owner canceled before waiting", func(t *testing.T) {
		owner, cancelOwner := context.WithCancel(context.Background())
		h := newServiceHarness(t)
		svc := h.service(owner, servicePlugin("alpha", &h.opens, nil))
		cancelOwner()
		if _, err := svc.publish(context.Background()); !errors.Is(err, ErrClosed) {
			t.Fatalf("publish = %v, want ErrClosed for the canceled owner", err)
		}
		if _, err := os.Stat(h.configPath); !os.IsNotExist(err) {
			t.Fatalf("an owner-canceled publish started input reads: %v", err)
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
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		t.Cleanup(sub.Close)
		candidate, err := svc.publish(ctx)
		if candidate != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("publish = (%v, %v), want the Store check to reject the completed candidate", candidate, err)
		}
		if svc.current() != nil {
			t.Fatal("the Store check published anyway")
		}
		select {
		case event, ok := <-sub.Events():
			if ok {
				t.Fatalf("the canceled publication emitted %+v, want event silence", event)
			}
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
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		t.Cleanup(sub.Close)
		cancelOwner()
		cancelCaller()
		candidate, err := svc.publish(caller)
		if candidate != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("both-done publish = (%v, %v), want the caller's own context error from the caller-first checkpoint", candidate, err)
		}
		if errors.Is(err, ErrClosed) {
			t.Fatalf("both-done publish = %v, want the caller error, not the owner-lifecycle identity", err)
		}
		select {
		case event, ok := <-sub.Events():
			if ok {
				t.Fatalf("the canceled publication emitted %+v, want event silence", event)
			}
			t.Fatal("the subscription closed around the canceled publication, want silence with the subscription open")
		default:
		}
		if svc.current() != nil {
			t.Fatal("a canceled publication stored a snapshot")
		}
		if _, err := os.Stat(h.configPath); !os.IsNotExist(err) {
			t.Fatalf("a both-done publish started input reads: %v", err)
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
		if candidate != nil || !errors.Is(err, ErrClosed) {
			t.Fatalf("publish = (%v, %v), want ErrClosed from the Store check", candidate, err)
		}
		if svc.current() != nil {
			t.Fatal("an owner-canceled publication stored a snapshot")
		}
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
		cfg *configuration
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
	if err := os.Rename(h.configPath, dangling); err != nil {
		t.Fatalf("hide captured input: %v", err)
	}
	if err := os.Mkdir(h.configPath, 0o700); err != nil {
		t.Fatalf("block the input path: %v", err)
	}

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
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled publish = %v, want its context error", err)
		}
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
	if first.err != nil {
		t.Fatalf("active build: %v", first.err)
	}
	if first.cfg.generation != 1 {
		t.Fatalf("active build generation = %d, want 1", first.cfg.generation)
	}
	if waiter := <-waiting; waiter.cfg != nil || !errors.Is(waiter.err, context.Canceled) {
		t.Fatalf("canceled waiter = (%v, %v), want its own context error without starting another build", waiter.cfg, waiter.err)
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("builds started = %d, want the canceled waiter to start no build", got)
	}
	if err := os.Remove(h.configPath); err != nil {
		t.Fatalf("remove the input block: %v", err)
	}
	if err := os.Rename(dangling, h.configPath); err != nil {
		t.Fatalf("restore the input: %v", err)
	}
	second, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("publish after the serialized build: %v", err)
	}
	if second.generation != 2 {
		t.Fatalf("generation after serialization = %d, want 2", second.generation)
	}
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
	if candidate != nil || !errors.Is(err, ErrConfiguration) {
		t.Fatalf("overflow publish = (%v, %v), want ErrConfiguration without publication", candidate, err)
	}
	if svc.current() != exhausted {
		t.Fatal("the overflow failure replaced the published snapshot")
	}
}

// TestConfigurationServiceKeepsFirstRunSkeletons proves a fresh machine
// build creates exactly the retained first-run skeleton contents and starts
// successfully on their defaults, including a selected Agent whose model is
// wholly missing.
func TestConfigurationServiceKeepsFirstRunSkeletons(t *testing.T) {
	h := newServiceHarness(t)
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))
	first, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("first-run publish: %v", err)
	}
	if configData, err := os.ReadFile(h.configPath); err != nil || string(configData) != mainConfigSkeleton {
		t.Fatalf("first-run config = %q (%v), want the retained skeleton", string(configData), err)
	}
	if agentsData, err := os.ReadFile(filepath.Join(h.dataDir, "agents.json")); err != nil || string(agentsData) != agentsSkeleton {
		t.Fatalf("first-run agents = %q (%v), want the retained skeleton", string(agentsData), err)
	}
	if len(first.definitions) != 5 {
		t.Fatalf("definitions = %d, want the built-in roster", len(first.definitions))
	}

	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{"plain":{}}`)
	second, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("publish with a missing model selection: %v", err)
	}
	if found, model := hasDefinition(second, "plain"); !found || model != "" {
		t.Fatalf("plain definition = (%v, %q), want the roster with a wholly empty model", found, model)
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
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if snapshot.catalog.Providers["remote"].Models["remote-model"] == nil {
		t.Fatal("the discovery publication never reached the assembled catalog")
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "cache", "discovery", "remote.json")); err != nil {
		t.Fatalf("discovery cache moved away from the home root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", ".env")); !os.IsNotExist(err) {
		t.Fatalf("the configuration build touched the dotenv source: %v", err)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "agents.json,config.json" {
		t.Fatalf("data directory entries = %q, want only the captured input files", names)
	}
}

// TestConfigurationServiceRestartsGenerationWithFreshCaptures proves the
// generation labels one Runtime lifetime: a fresh service starts again at 1,
// its content comes from the current bytes, and the previous lifetime's
// capture remains readable and unchanged under its same label.
func TestConfigurationServiceRestartsGenerationWithFreshCaptures(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, providerConfigFile("One"))
	before, err := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil)).publish(context.Background())
	if err != nil {
		t.Fatalf("publish before restart: %v", err)
	}

	writeServiceFile(t, h.configPath, providerConfigFile("Two"))
	after, err := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil)).publish(context.Background())
	if err != nil {
		t.Fatalf("publish after restart: %v", err)
	}
	if after.generation != 1 {
		t.Fatalf("after restart generation = %d, want a fresh lifetime starting again at 1", after.generation)
	}
	if (Invocation{snapshot: after}).Revision() != "1" || snapshotModelName(after, "prov", "m") != "Two" {
		t.Fatalf("post-restart capture = %q/%q, want revision 1 with the fresh content", (Invocation{snapshot: after}).Revision(), snapshotModelName(after, "prov", "m"))
	}
	if (Invocation{snapshot: before}).Revision() != "1" || snapshotModelName(before, "prov", "m") != "One" {
		t.Fatal("the old-lifetime capture changed after the restart")
	}
}

const captureGlobalAllow = `{"rules":[{"permission":"file.write","target":"*","access":"allow"}]}`

func captureConfigDoc() string {
	return `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"One","context_window":100}}}},"permissions":` + captureGlobalAllow + `}`
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
	t.Setenv("HOME", h.home)
	writeServiceFile(t, h.configPath, captureConfigDoc())
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))

	var builtin harness.PermissionPolicy
	globalOnly := harness.ResolvePermissionPolicy(json.RawMessage(captureGlobalAllow), nil)

	// Startup on a fresh machine: no projects inventory, the Workspace level
	// absent and the captured global member deciding.
	first, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("fresh-install publish: %v", err)
	}
	if got := first.permissionPolicy("/ws"); !reflect.DeepEqual(got, globalOnly) {
		t.Fatalf("fresh-install policy = %#v, want the global-only capture", got)
	}

	// A later file requires Reload: the old revision is untouched and the
	// reload resolves the workspace rule over the captured global member.
	dirID, err := config.WorkspacePermissionDirID("/ws")
	if err != nil {
		t.Fatalf("WorkspacePermissionDirID: %v", err)
	}
	permissionPath := filepath.Join(h.home, ".lightcode", "projects", dirID, "permissions.json")
	writeServiceFile(t, permissionPath, `{"rules":[{"permission":"file.write","target":"*","access":"deny"}]}`)
	if got := first.permissionPolicy("/ws"); !reflect.DeepEqual(got, globalOnly) {
		t.Fatalf("policy changed without Reload: %#v", got)
	}
	second, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("reload with the added file: %v", err)
	}
	want := harness.ResolvePermissionPolicy(json.RawMessage(captureGlobalAllow), json.RawMessage(`{"rules":[{"permission":"file.write","target":"*","access":"deny"}]}`))
	if got := second.permissionPolicy("/ws"); !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded policy = %#v, want the workspace deny over the global allow", got)
	}
	if got := first.permissionPolicy("/ws"); !reflect.DeepEqual(got, globalOnly) {
		t.Fatalf("the old revision's capture changed after Reload: %#v", got)
	}

	// An unreadable file is absent policy for that Workspace alone; the
	// global member still decides and publication succeeds.
	if err := os.Chmod(permissionPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(permissionPath, 0o600) })
	third, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("reload with the unreadable file: %v", err)
	}
	if got := third.permissionPolicy("/ws"); !reflect.DeepEqual(got, globalOnly) {
		t.Fatalf("unreadable-file policy = %#v, want the global-only level", got)
	}
	if got := second.permissionPolicy("/ws"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the unreadable file changed the prior revision: %#v", got)
	}
	// A successfully read malformed file keeps publication and follows the
	// built-in fallback posture for that Workspace, discarding both user
	// levels there.
	if err := os.Chmod(permissionPath, 0o600); err != nil {
		t.Fatalf("restore: %v", err)
	}
	writeServiceFile(t, permissionPath, `NOT JSON`)
	fourth, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("reload with the malformed file: %v", err)
	}
	if got := fourth.permissionPolicy("/ws"); !reflect.DeepEqual(got, builtin) {
		t.Fatalf("malformed-file policy = %#v, want the built-in fallback", got)
	}

	// A failed enumeration of the inventory directory leaves the whole
	// Workspace policy level absent for the revision, uniformly, without
	// failing publication.
	if err := os.Chmod(filepath.Join(h.home, ".lightcode", "projects"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(h.home, ".lightcode", "projects"), 0o700) })
	fifth, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("reload with the unreadable inventory: %v", err)
	}
	if got := fifth.permissionPolicy("/ws"); !reflect.DeepEqual(got, globalOnly) {
		t.Fatalf("failed-enumeration policy = %#v, want the uniform absent Workspace level", got)
	}
	if got := fifth.permissionPolicy("/elsewhere"); !reflect.DeepEqual(got, globalOnly) {
		t.Fatalf("other-workspace policy under a failed enumeration = %#v, want the uniform absent level", got)
	}

	// A file removed again is absent policy at the reload that observes it.
	if err := os.Chmod(filepath.Join(h.home, ".lightcode", "projects"), 0o700); err != nil {
		t.Fatalf("restore inventory: %v", err)
	}
	if err := os.Remove(permissionPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	sixth, err := svc.publish(context.Background())
	if err != nil {
		t.Fatalf("reload with the removed file: %v", err)
	}
	if got := sixth.permissionPolicy("/ws"); !reflect.DeepEqual(got, globalOnly) {
		t.Fatalf("removed-file policy = %#v, want the absent Workspace level", got)
	}
	if got := fifth.permissionPolicy("/ws"); !reflect.DeepEqual(got, globalOnly) {
		t.Fatalf("the removed file changed the prior revision: %#v", got)
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
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	if err := os.Remove(h.configPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(h.configPath, 0o700); err != nil {
		t.Fatalf("block: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(h.configPath)
	})
	candidate, err := svc.publish(context.Background())
	if candidate != nil || !errors.Is(err, ErrConfiguration) {
		t.Fatalf("publish = (%v, %v), want the unreadable document to fail publication", candidate, err)
	}
	if !strings.Contains(err.Error(), "read main configuration") {
		t.Fatalf("error = %v, want the retained source path", err)
	}
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
		ID:    "tools",
		Scope: ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("tool.x", func(Invocation, ToolConstraints) (ToolDescription, error) {
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
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	// The declared tool ID validated the configured reference and produced no
	// warning.
	found, _ := hasDefinition(snapshot, "worker")
	if !found || len(snapshot.agentWarnings) != 0 {
		t.Fatalf("worker retained = %v with warnings %+v, want the declared tool to validate cleanly", found, snapshot.agentWarnings)
	}
	// An undeclared reference drops the definition with the retained warning.
	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{"worker":{"tools":["ghost_tool"]}}`)
	snapshot, err = svc.publish(context.Background())
	if err != nil {
		t.Fatalf("publish with the unknown tool: %v", err)
	}
	if found, _ := hasDefinition(snapshot, "worker"); found {
		t.Fatal("the unknown tool name did not drop its definition")
	}
	if len(snapshot.agentWarnings) != 1 || snapshot.agentWarnings[0].Kind != "invalid_agent_type" || !strings.Contains(snapshot.agentWarnings[0].Message, "ghost_tool") {
		t.Fatalf("warnings = %+v, want the retained invalid-agent drop naming ghost_tool", snapshot.agentWarnings)
	}
	if describeCalled {
		t.Fatal("configuration publication ran the description function")
	}
	if h.opens.Load() != 0 {
		t.Fatalf("%d factories ran during configuration publication", h.opens.Load())
	}
}
