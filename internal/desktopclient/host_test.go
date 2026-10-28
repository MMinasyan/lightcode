package desktopclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wailsapp/wails/v2/pkg/options/linux"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/internal/version"
	"github.com/MMinasyan/lightcode/protocol"
	"github.com/MMinasyan/lightcode/runtime"
)

// The desktop host's controlled fixtures: a scripted loopback server models
// the transport for the generation, pull and byte rows, and the mounted
// smoke test drives the real authenticated owner through the public Runtime
// composition, the same boundary the client integration fixtures use.

const (
	testInstance   = "0123456789abcdef0123456789abcdef"
	testCredential = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// recordContent builds one private discovery document pointing at the
// fixture endpoint.
func recordContent(endpoint string) string {
	return fmt.Sprintf(`{"endpoint":%q,"instance_id":%q,"protocol_version":"5","credential":%q}`,
		endpoint, testInstance, testCredential)
}

// newFixture starts one scripted loopback fixture with the given health and
// events handlers; each supplied extra map is mounted beside them.
func newFixture(t *testing.T, health, events http.HandlerFunc, extra ...map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	if health != nil {
		mux.HandleFunc("/v1/health", health)
	}
	if events != nil {
		mux.HandleFunc("/v1/events", events)
	}
	for _, routes := range extra {
		for path, handler := range routes {
			mux.HandleFunc(path, handler)
		}
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// jsonHealth answers one authenticated health body for the fixture instance.
func jsonHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"instance_id":%q,"protocol_version":"5"}`, testInstance)
	}
}

// parkedStream answers the events route with an open stream that holds until
// the request context ends.
func parkedStream() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}
}

// missingExecutable names one launcher that is guaranteed absent, so a host
// never starts a contender behind its fixture.
func missingExecutable(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "absent-binary")
}

// newHost wires one started host to the fixture's discovery record under an
// isolated data directory, and shuts it down on cleanup.
func newHost(t *testing.T, server *httptest.Server) *Host {
	t.Helper()
	dataDir := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(dataDir, "runtime.json"), []byte(recordContent(server.URL)), 0o600))
	host := New(clienthost.Options{DataDir: dataDir, Executable: missingExecutable(t)}, "/ws/name")
	host.startup(context.Background())
	t.Cleanup(func() { host.shutdown(context.Background()) })
	return host
}

// sessionChangedFrame encodes one valid revisioned session hint as a
// declared SSE notification frame.
func sessionChangedFrame(durable string) string {
	var scope protocol.Scope
	if err := scope.FromSessionScope(protocol.SessionScope{Kind: protocol.SessionScopeKindSession, SessionId: testInstance}); err != nil {
		panic(err)
	}
	var event protocol.Event
	if err := event.FromSessionChangedEvent(protocol.SessionChangedEvent{
		Kind:  protocol.SessionChanged,
		Scope: scope,
		SessionRevision: protocol.SessionRevision{
			InstanceId:      testInstance,
			DurableRevision: durable,
			LocalRevision:   "0",
		},
	}); err != nil {
		panic(err)
	}
	data, err := json.Marshal(event)
	if err != nil {
		panic(err)
	}
	return "event: notification\ndata: " + string(data) + "\n\n"
}

// awaitPulling waits until one event pull has taken the outstanding-pull
// gate.
func awaitPulling(t *testing.T, host *Host) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		host.mu.Lock()
		pulling := host.pulling
		host.mu.Unlock()
		if pulling {
			return
		}
		testassert.Require(t, !time.Now().After(deadline), "the event pull never took the outstanding-pull gate")
		time.Sleep(time.Millisecond)
	}
}

// awaitTerminal waits until the host's terminal channel holds one stream
// error.
func awaitTerminal(t *testing.T, host *Host) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		host.mu.Lock()
		ready := len(host.errs) > 0
		host.mu.Unlock()
		if ready {
			return
		}
		testassert.Require(t, !time.Now().After(deadline), "the stream never reported its terminal error")
		time.Sleep(time.Millisecond)
	}
}

// TestDesktopHostOpenGenerationSupersession proves a newer Open installs its
// own generation and invalidates the previous one for requests and pulls,
// while the current generation still reaches the owner.
func TestDesktopHostOpenGenerationSupersession(t *testing.T) {
	server := newFixture(t, jsonHealth(), parkedStream())
	host := newHost(t, server)

	first, err := host.Open()
	testassert.NoError(t, err)
	testassert.Require(t, first.InstanceID == testInstance && first.Workspace == "/ws/name" && first.Generation == "1", "got %v, want %v; got %v, want %v; got %v, want %v", first.InstanceID, testInstance, first.Workspace, "/ws/name", first.Generation, "1")

	second, err := host.Open()
	testassert.NoError(t, err)
	testassert.Equal(t, second.Generation, "2")

	{
		_, err := host.Call(first.Generation, "GetHealth", "")
		testassert.Require(t, err != nil, "a superseded generation accepted a call")
	}
	{
		_, err := host.NextEvent(first.Generation)
		testassert.Require(t, err != nil, "a superseded generation accepted an event pull")
	}

	response, err := host.Call(second.Generation, "GetHealth", "")
	testassert.NoError(t, err)
	testassert.Require(t, response.Status == http.StatusOK && strings.Contains(response.Body, testInstance), "got %v, want %v; health body = %q, want the authenticated instance", response.Status, http.StatusOK, response.Body)
}

// TestDesktopHostOpenCancelsInFlightCall proves a newer Open cancels a
// generated call the previous generation still holds in flight.
func TestDesktopHostOpenCancelsInFlightCall(t *testing.T) {
	arrived := make(chan struct{})
	observed := make(chan struct{})
	server := newFixture(t, jsonHealth(), parkedStream(), map[string]http.HandlerFunc{
		"/v1/sessions/parked/hydration": func(_ http.ResponseWriter, r *http.Request) {
			close(arrived)
			<-r.Context().Done()
			close(observed)
		},
	})
	host := newHost(t, server)
	info, err := host.Open()
	testassert.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := host.Call(info.Generation, "GetSessionHydration", `{"session_id":"parked"}`)
		done <- err
	}()
	<-arrived
	{
		_, err := host.Open()
		testassert.NoError(t, err)
	}
	{
		err := <-done
		testassert.Require(t, errors.Is(err, context.Canceled), "superseded call = %v, want context.Canceled", err)
	}
	select {
	case <-observed:
	case <-time.After(30 * time.Second):
		t.Fatal("the parked transport never observed the cancellation")
	}
}

// TestDesktopHostCallClassification proves the Call binding carries the shared
// invocation owner's result across Wails: success, a validated Runtime
// rejection, and a valid 204 all return their actual status and available raw
// body without rejecting the binding; an invalid delivery sets the one local
// Invalid bit while retaining the received status/body; and a transport
// failure rejects the binding. Exact request dispatch, envelope binding,
// opaque bytes and Error/partial discrimination stay at the clientops and
// protocol owners.
func TestDesktopHostCallClassification(t *testing.T) {
	const partial = `{"error":{"code":"conflict","message":"partial"},"restored":["a.go"],"skipped":[]}`
	server := newFixture(t, jsonHealth(), parkedStream(), map[string]http.HandlerFunc{
		"/v1/sessions/abc/revert-code": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, partial)
		},
		"/v1/sessions/abc/interrupt": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"/v1/sessions/malformed/hydration": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "not json")
		},
	})
	host := newHost(t, server)
	info, err := host.Open()
	testassert.NoError(t, err)

	success, err := host.Call(info.Generation, "GetHealth", "")
	testassert.NoError(t, err)
	testassert.Require(t, success.Status == http.StatusOK && !success.Invalid && strings.Contains(success.Body, testInstance), "success response = %+v, want the authenticated health body", success)

	rejected, err := host.Call(info.Generation, "RevertSessionCode", `{"session_id":"abc","body":{"operation_id":"op-1"}}`)
	testassert.NoError(t, err)
	testassert.Require(t, rejected.Status == http.StatusConflict && !rejected.Invalid && rejected.Body == partial, "rejection response = %+v, want the validated partial body without rejection", rejected)

	empty, err := host.Call(info.Generation, "InterruptSession", `{"session_id":"abc"}`)
	testassert.NoError(t, err)
	testassert.Require(t, empty.Status == http.StatusNoContent && !empty.Invalid && empty.Body == "", "204 response = %+v, want a valid empty success", empty)

	invalid, err := host.Call(info.Generation, "GetSessionHydration", `{"session_id":"malformed"}`)
	testassert.NoError(t, err)
	testassert.Require(t, invalid.Status == http.StatusOK && invalid.Invalid && invalid.Body == "not json", "invalid response = %+v, want the received status/body with the Invalid bit", invalid)

	{
		_, err := host.Call(info.Generation, "Nope", "")
		testassert.Require(t, err != nil, "an unknown operation did not reject the binding")
	}
}

// TestDesktopHostNextEventPullOwnership proves the one outstanding pull, the
// uniform single-select observation of the progress, terminal and context
// channels, channel closure as loss, and that an observed loss retires its
// generation so no buffered progress or later call survives it, over the
// host's actual channel fields.
func TestDesktopHostNextEventPullOwnership(t *testing.T) {
	t.Run("channel closure is loss", func(t *testing.T) {
		events := make(chan protocol.Event)
		errs := make(chan error)
		close(events)
		close(errs)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		host := &Host{generation: "1", events: events, errs: errs, connCtx: ctx, connCancel: cancel}
		{
			_, err := host.NextEvent("1")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "closed"), "NextEvent = %v, want a closure loss", err)
		}
	})

	t.Run("one outstanding pull at a time", func(t *testing.T) {
		events := make(chan protocol.Event)
		errs := make(chan error)
		ctx, cancel := context.WithCancel(context.Background())
		host := &Host{generation: "1", events: events, errs: errs, connCtx: ctx, connCancel: cancel}
		firstDone := make(chan struct{})
		go func() {
			_, _ = host.NextEvent("1")
			close(firstDone)
		}()
		awaitPulling(t, host)
		{
			_, err := host.NextEvent("1")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "already outstanding"), "second pull = %v, want the one outstanding pull", err)
		}
		cancel()
		<-firstDone
	})

	t.Run("progress closure without a terminal error is loss", func(t *testing.T) {
		events := make(chan protocol.Event)
		errs := make(chan error)
		close(events)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		host := &Host{generation: "1", events: events, errs: errs, connCtx: ctx, connCancel: cancel}
		{
			_, err := host.NextEvent("1")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "closed without a terminal error"), "NextEvent = %v, want the closure loss", err)
		}
	})

	t.Run("events closure after a queued terminal keeps the producer's actual error", func(t *testing.T) {
		events := make(chan protocol.Event)
		errs := make(chan error, 1)
		errs <- client.ErrResyncRequired
		close(events)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		host := &Host{generation: "1", events: events, errs: errs, connCtx: ctx, connCancel: cancel}
		{
			_, err := host.NextEvent("1")
			testassert.Require(t, errors.Is(err, client.ErrResyncRequired), "NextEvent = %v, want the producer's queued terminal error", err)
		}
	})

	t.Run("an observed loss retires the generation and its buffered progress", func(t *testing.T) {
		events := make(chan protocol.Event, 1)
		errs := make(chan error, 1)
		var scope protocol.Scope
		testassert.NoError(t, scope.FromRuntimeScope(protocol.RuntimeScope{Kind: protocol.RuntimeScopeKindRuntime}))
		var event protocol.Event
		testassert.NoError(t, event.FromScopeEvent(protocol.ScopeEvent{Kind: protocol.ScopeOpened, Scope: scope}))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		host := &Host{generation: "1", events: events, errs: errs, connCtx: ctx, connCancel: cancel}
		errs <- client.ErrResyncRequired
		{
			_, err := host.NextEvent("1")
			testassert.Require(t, errors.Is(err, client.ErrResyncRequired), "pull = %v, want the observed terminal error", err)
		}
		// The loss retires the generation before any later pull can observe
		// its remaining buffered progress.
		events <- event
		{
			_, err := host.NextEvent("1")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "stale connection generation"), "later pull = %v, want the stale-generation rejection", err)
		}
		host.mu.Lock()
		canceled, retired := ctx.Err() != nil, host.generation == ""
		host.mu.Unlock()
		testassert.Require(t, canceled && retired, "an observed loss must cancel and retire only its matching generation")
	})
}

// TestDesktopHostProgressPrecedesUnobservedTerminal proves the uniform
// observation order: valid queued progress is delivered ahead of a terminal
// signal that has not been observed yet, and the terminal arriving afterwards
// is observed by a later pull, which retires the generation. Whichever ready
// select case wins when both are queued at once is never promised.
func TestDesktopHostProgressPrecedesUnobservedTerminal(t *testing.T) {
	events := make(chan protocol.Event, 1)
	errs := make(chan error, 1)
	var scope protocol.Scope
	testassert.NoError(t, scope.FromRuntimeScope(protocol.RuntimeScope{Kind: protocol.RuntimeScopeKindRuntime}))
	var event protocol.Event
	testassert.NoError(t, event.FromScopeEvent(protocol.ScopeEvent{Kind: protocol.ScopeOpened, Scope: scope}))
	want, err := json.Marshal(event)
	testassert.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := &Host{generation: "1", events: events, errs: errs, connCtx: ctx, connCancel: cancel}

	events <- event
	raw, err := host.NextEvent("1")
	testassert.Require(t, err == nil && raw == string(want), "progress pull = (%q, %v), want the exact %q ahead of the unobserved terminal", raw, err, want)
	errs <- client.ErrResyncRequired
	{
		_, err := host.NextEvent("1")
		testassert.Require(t, errors.Is(err, client.ErrResyncRequired), "later pull = %v, want the now-observed terminal error", err)
	}
}

// TestDesktopHostOverflowTerminalRetiresGeneration drives the real stream
// through a queue overflow: the flood is gated until after Open establishes
// the stream (the response is flushed first), so Open can never randomly
// reject an already overflowed stream. Pulls observe the valid queued
// progress and the terminal failure in whatever order the single select
// chooses, and every delivered pull is the valid queued progress. Once the
// terminal failure is observed it cancels and retires exactly that
// connection generation, and no later pull or forwarded request acts healthy
// on it.
func TestDesktopHostOverflowTerminalRetiresGeneration(t *testing.T) {
	const total = 300
	start := make(chan struct{})
	server := newFixture(t, jsonHealth(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-start:
		case <-r.Context().Done():
			return
		}
		for i := 0; i < total; i++ {
			if r.Context().Err() != nil {
				return
			}
			if _, err := io.WriteString(w, sessionChangedFrame("1")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	host := newHost(t, server)
	info, err := host.Open()
	testassert.NoError(t, err)
	close(start)
	awaitTerminal(t, host)

	var observed error
	for pulls := 0; ; pulls++ {
		testassert.Require(t, pulls <= total, "the terminal failure was never observed")
		raw, err := host.NextEvent(info.Generation)
		if err != nil {
			observed = err
			break
		}
		var delivered map[string]any
		testassert.Require(t, json.Unmarshal([]byte(raw), &delivered) == nil && delivered["kind"] == "session_changed", "pull delivered %q, want only the valid queued progress", raw)
	}
	testassert.Require(t, errors.Is(observed, client.ErrResyncRequired), "observed terminal = %v, want the overflow resync error", observed)
	host.mu.Lock()
	generationErr := host.connCtx.Err()
	retired := host.generation == ""
	host.mu.Unlock()
	testassert.Require(t, generationErr != nil, "the terminal failure did not cancel its connection generation")
	testassert.Require(t, retired, "the terminal failure did not retire its connection generation")
	{
		_, err := host.NextEvent(info.Generation)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "stale connection generation"), "later pull = %v, want the stale-generation rejection", err)
	}
	{
		_, err := host.Call(info.Generation, "GetHealth", "")
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "stale connection generation"), "a later request acted healthy on the lost generation")
	}
}

// TestDesktopHostOpenRejectsEstablishmentFailure proves an already reported
// stream establishment failure refuses the open instead of installing a
// healthy-looking connection.
func TestDesktopHostOpenRejectsEstablishmentFailure(t *testing.T) {
	server := newFixture(t, jsonHealth(), func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no stream", http.StatusInternalServerError)
	})
	host := newHost(t, server)
	{
		_, err := host.Open()
		testassert.Require(t, err != nil, "Open succeeded over a failed event stream")
	}
	{
		_, err := host.Call("1", "GetHealth", "")
		testassert.Require(t, err != nil, "no generation was installed, yet a call succeeded")
	}
	{
		_, err := host.NextEvent("1")
		testassert.Require(t, err != nil, "no generation was installed, yet a pull succeeded")
	}
}

// TestDesktopHostCloseCancelsWaits proves host close cancels an in-flight
// call and a parked event pull without touching the Runtime.
func TestDesktopHostCloseCancelsWaits(t *testing.T) {
	arrived := make(chan struct{})
	observed := make(chan struct{})
	server := newFixture(t, jsonHealth(), parkedStream(), map[string]http.HandlerFunc{
		"/v1/sessions/parked/hydration": func(_ http.ResponseWriter, r *http.Request) {
			close(arrived)
			<-r.Context().Done()
			close(observed)
		},
	})
	host := newHost(t, server)
	info, err := host.Open()
	testassert.NoError(t, err)
	requestDone := make(chan error, 1)
	go func() {
		_, err := host.Call(info.Generation, "GetSessionHydration", `{"session_id":"parked"}`)
		requestDone <- err
	}()
	<-arrived
	pullDone := make(chan error, 1)
	go func() {
		_, err := host.NextEvent(info.Generation)
		pullDone <- err
	}()
	awaitPulling(t, host)

	host.shutdown(context.Background())
	{
		err := <-requestDone
		testassert.Require(t, errors.Is(err, context.Canceled), "closed request = %v, want context.Canceled", err)
	}
	{
		err := <-pullDone
		testassert.Require(t, errors.Is(err, context.Canceled), "closed pull = %v, want context.Canceled", err)
	}
	select {
	case <-observed:
	case <-time.After(30 * time.Second):
		t.Fatal("the parked transport never observed the close")
	}
}

// TestDesktopHostNativeInvocation exercises the host-local dialog, title and
// version bindings through their injected native functions, over the real
// constructor and private lifecycle.
func TestDesktopHostNativeInvocation(t *testing.T) {
	t.Run("picked directory passes through", func(t *testing.T) {
		host := New(clienthost.Options{}, "/ws/name")
		host.startup(context.Background())
		defer host.shutdown(context.Background())
		host.pickDirFn = func(context.Context) (string, error) { return "/chosen", nil }
		got, err := host.PickDirectory()
		testassert.Require(t, err == nil && got == "/chosen", "PickDirectory = (%q, %v)", got, err)
	})

	t.Run("directory cancel is an empty result", func(t *testing.T) {
		host := New(clienthost.Options{}, "/ws/name")
		host.startup(context.Background())
		defer host.shutdown(context.Background())
		host.pickDirFn = func(context.Context) (string, error) { return "", nil }
		got, err := host.PickDirectory()
		testassert.Require(t, err == nil && got == "", "PickDirectory = (%q, %v), want the cancel result", got, err)
	})

	t.Run("title matches the one shared algorithm", func(t *testing.T) {
		for _, workspace := range []string{"/ws/name", ".", "/", ""} {
			host := New(clienthost.Options{}, workspace)
			host.startup(context.Background())
			var applied []string
			host.titleFn = func(_ context.Context, title string) { applied = append(applied, title) }
			{
				err := host.SetTitle(workspace)
				testassert.Require(t, err == nil, "SetTitle(%q): %v", workspace, err)
			}
			{
				err := host.SetTitle(workspace)
				testassert.Require(t, err == nil, "second SetTitle(%q): %v", workspace, err)
			}
			// The initial window title and every subsequent SetTitle apply
			// the one shared algorithm: the plain basename join, no
			// special case.
			want := "Lightcode — " + filepath.Base(workspace)
			{
				got := windowTitle(workspace)
				testassert.Require(t, got == want, "initial title for %q = %q, want %q", workspace, got, want)
			}
			testassert.Require(t, applied[0] == want && applied[1] == want, "SetTitle(%q) applied %q, want the initial title %q", workspace, applied, want)
		}
	})

	t.Run("version is the native helper", func(t *testing.T) {
		host := New(clienthost.Options{}, "/ws/name")
		if got := host.AppVersion(); got != version.String() {
			t.Fatalf("AppVersion = %q, want %q", got, version.String())
		}
	})
}

// startupProbeKey marks the exact context a test hands to the private
// startup hook.
type startupProbeKey struct{}

// TestDesktopHostParkedCallsUnblockOnStartup proves the genuinely
// asynchronous native lifecycle: dialog, title and open calls parked before
// startup complete on the actual startup with that exact context. Wails
// launches OnStartup in a goroutine with no binding-dispatch barrier, so a
// call must never fall through to a background context.
func TestDesktopHostParkedCallsUnblockOnStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := New(clienthost.Options{DataDir: t.TempDir(), Executable: missingExecutable(t)}, "/ws/name")
		var nativeCtx context.Context
		host.pickDirFn = func(ctx context.Context) (string, error) { nativeCtx = ctx; return "/chosen", nil }
		var titledCtx context.Context
		host.titleFn = func(ctx context.Context, _ string) { titledCtx = ctx }

		type outcome struct {
			value string
			err   error
		}
		directory := make(chan outcome, 1)
		title := make(chan outcome, 1)
		opened := make(chan outcome, 1)
		go func() {
			got, err := host.PickDirectory()
			directory <- outcome{value: got, err: err}
		}()
		go func() {
			err := host.SetTitle("/ws/name")
			title <- outcome{err: err}
		}()
		go func() {
			// The data directory holds no discovery record: once released,
			// the open performs the actual clienthost.Connect.
			_, err := host.Open()
			opened <- outcome{err: err}
		}()

		synctest.Wait() // every call is parked before the lifecycle context exists
		select {
		case got := <-directory:
			t.Fatalf("PickDirectory completed before startup: %+v", got)
		default:
		}
		select {
		case got := <-title:
			t.Fatalf("SetTitle completed before startup: %+v", got)
		default:
		}
		select {
		case got := <-opened:
			t.Fatalf("Open completed before startup: %+v", got)
		default:
		}

		startupCtx := context.WithValue(context.Background(), startupProbeKey{}, "startup")
		host.startup(startupCtx)

		{
			got := <-directory
			testassert.Require(t, got.err == nil && got.value == "/chosen", "PickDirectory after startup = %+v", got)
		}
		{
			got := <-title
			testassert.Require(t, got.err == nil, "SetTitle after startup = %+v", got)
		}
		{
			got := <-opened
			testassert.Require(t, got.err != nil && strings.Contains(got.err.Error(), "connect runtime:"), "Open after startup = %v, want the actual clienthost.Connect outcome", got.err)
		}
		testassert.Require(t, nativeCtx == startupCtx, "the native dialog did not receive the exact startup context")
		testassert.Require(t, titledCtx == startupCtx, "the native title call did not receive the exact startup context")
		host.shutdown(context.Background())
	})
}

// TestDesktopHostShutdownBeforeStartupWakesWaiters proves shutdown-before-
// startup marks the host closed, releases every parked call with an error,
// and invokes no native function.
func TestDesktopHostShutdownBeforeStartupWakesWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := New(clienthost.Options{DataDir: t.TempDir(), Executable: missingExecutable(t)}, "/ws/name")
		invoked := false
		host.pickDirFn = func(context.Context) (string, error) { invoked = true; return "/chosen", nil }
		host.titleFn = func(context.Context, string) { invoked = true }

		type outcome struct {
			value string
			err   error
		}
		directory := make(chan outcome, 1)
		title := make(chan outcome, 1)
		opened := make(chan outcome, 1)
		go func() {
			got, err := host.PickDirectory()
			directory <- outcome{value: got, err: err}
		}()
		go func() {
			err := host.SetTitle("/ws/name")
			title <- outcome{err: err}
		}()
		go func() {
			_, err := host.Open()
			opened <- outcome{err: err}
		}()

		synctest.Wait() // parked before startup, as above
		host.shutdown(context.Background())

		{
			got := <-directory
			testassert.Require(t, got.err != nil, "PickDirectory after shutdown-before-startup = %+v, want an error", got)
		}
		{
			got := <-title
			testassert.Require(t, got.err != nil, "SetTitle after shutdown-before-startup = %+v, want an error", got)
		}
		{
			got := <-opened
			testassert.Require(t, got.err != nil && strings.Contains(got.err.Error(), "closed"), "Open after shutdown-before-startup = %v, want the closed-host error", got.err)
		}
		testassert.Require(t, !invoked, "shutdown-before-startup invoked a native presentation function")
	})
}

// TestDesktopHostPostCloseNativeCallsError proves native calls after a close
// report the closed host instead of reaching the dialog or the window.
func TestDesktopHostPostCloseNativeCallsError(t *testing.T) {
	host := New(clienthost.Options{}, "/ws/name")
	host.startup(context.Background())
	invoked := false
	host.pickDirFn = func(context.Context) (string, error) { invoked = true; return "/chosen", nil }
	host.titleFn = func(context.Context, string) { invoked = true }
	host.shutdown(context.Background())

	{
		got, err := host.PickDirectory()
		testassert.Require(t, err != nil, "PickDirectory after close = (%q, %v), want the closed-host error", got, err)
	}
	{
		err := host.SetTitle("/ws/name")
		testassert.Require(t, err != nil, "SetTitle after close succeeded")
	}
	{
		_, err := host.Open()
		testassert.Require(t, err != nil, "Open after close succeeded")
	}
	testassert.Require(t, !invoked, "post-close native calls reached the injected presentation functions")
}

// gatedFixture scripts one authenticated health endpoint whose second and
// later calls can be held at the gate, and one events stream whose first
// delivery terminates on demand. It drives the real clienthost.Connect and
// client.Events transitions without another producer.
type gatedFixture struct {
	mu          sync.Mutex
	healthCalls int
	eventsCalls int
	held        chan struct{} // one signal per health call waiting at the gate
	release     chan struct{} // closed to release held health calls
	terminate   chan struct{} // closed to end the first events stream
}

func newGatedFixture() *gatedFixture {
	return &gatedFixture{
		held:      make(chan struct{}, 8),
		release:   make(chan struct{}),
		terminate: make(chan struct{}),
	}
}

func (f *gatedFixture) health(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.healthCalls++
	gated := f.healthCalls > 1
	f.mu.Unlock()
	if gated {
		f.held <- struct{}{}
		select {
		case <-f.release:
		case <-r.Context().Done():
			return
		}
	}
	jsonHealth()(w, r)
}

func (f *gatedFixture) events(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.eventsCalls++
	first := f.eventsCalls == 1
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if first {
		select {
		case <-f.terminate:
		case <-r.Context().Done():
		}
		return
	}
	<-r.Context().Done()
}

// errBarrierContext embeds one actual installed context and parks its FIRST
// Err call: the underlying error is snapshotted, the holder is signalled,
// the call waits, and the snapshot is returned. Done, Deadline and Value
// delegate unchanged, so every other use behaves identically.
type errBarrierContext struct {
	context.Context
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	snap    error
}

func (b *errBarrierContext) Err() error {
	b.once.Do(func() {
		b.snap = b.Context.Err()
		close(b.entered)
		<-b.release
	})
	return b.snap
}

// recvBounded returns one channel delivery or fails at the bound; it is a
// teardown bound, never a scheduling oracle.
func recvBounded[T any](t *testing.T, stream <-chan T) T {
	t.Helper()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case value := <-stream:
		return value
	case <-timer.C:
		var zero T
		t.Fatalf("bounded channel wait timed out")
		return zero
	}
}

// TestDesktopHostPendingOpenRejectsOldStamps proves the retirement
// invariant while a newer attempt is held mid-connect: the previous
// generation stamp authorizes neither a request nor a pull, and both are
// rejected before any transport or queue I/O.
func TestDesktopHostPendingOpenRejectsOldStamps(t *testing.T) {
	fixture := newGatedFixture()
	server := newFixture(t, fixture.health, fixture.events)
	host := newHost(t, server)
	first, err := host.Open()
	testassert.NoError(t, err)
	testassert.Equal(t, first.Generation, "1")
	close(fixture.terminate)
	awaitTerminal(t, host)

	type openResult struct {
		info ConnectionInfo
		err  error
	}
	opened := make(chan openResult, 1)
	go func() {
		second, err := host.Open()
		opened <- openResult{info: second, err: err}
	}()
	recvBounded(t, fixture.held) // the newer attempt waits at its authenticated health gate

	{
		_, err := host.Call(first.Generation, "GetHealth", "")
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "stale connection generation"), "old-stamp request during a pending open = %v, want rejection before I/O", err)
	}
	{
		_, err := host.NextEvent(first.Generation)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "stale connection generation"), "old-stamp pull during a pending open = %v, want rejection before I/O", err)
	}

	close(fixture.release)
	result := recvBounded(t, opened)
	testassert.Require(t, result.err == nil && result.info.Generation == "2", "pending Open = (%+v, %v), want the installed second generation", result.info, result.err)
	response, err := host.Call(result.info.Generation, "GetHealth", "")
	testassert.Require(t, err == nil && response.Status == http.StatusOK, "new-generation request = (%+v, %v), want the usable install", response, err)
}

// TestDesktopHostFailedOpenDoesNotResurrectOldStamp proves a failed newer
// attempt leaves no valid stamp at all: the old generation stays rejected
// instead of silently becoming authoritative again.
func TestDesktopHostFailedOpenDoesNotResurrectOldStamp(t *testing.T) {
	var mu sync.Mutex
	var healthCalls int
	failingHealth := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		healthCalls++
		second := healthCalls > 1
		mu.Unlock()
		if second {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		jsonHealth()(w, r)
	}
	server := newFixture(t, failingHealth, parkedStream())
	host := newHost(t, server)
	first, err := host.Open()
	testassert.NoError(t, err)
	{
		_, err := host.Open()
		testassert.Require(t, err != nil, "second Open succeeded over a failing discovery owner")
	}

	{
		_, err := host.Call(first.Generation, "GetHealth", "")
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "stale connection generation"), "old-stamp request after a failed open = %v, want the retired stamp to stay rejected", err)
	}
	{
		_, err := host.NextEvent(first.Generation)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "stale connection generation"), "old-stamp pull after a failed open = %v, want the retired stamp to stay rejected", err)
	}
}

// TestDesktopHostLateTerminalCleanupCannotCancelPendingOpen drives the real
// read-to-lock window: an old pull holding its terminal cleanup is parked
// there (first-Err barrier over the actual installed context), the newer
// Open starts and stays pending at its authenticated health gate, and the
// cleanup is released. The retired old stamp must not authorize canceling
// the pending attempt: the open completes and remains usable.
func TestDesktopHostLateTerminalCleanupCannotCancelPendingOpen(t *testing.T) {
	fixture := newGatedFixture()
	server := newFixture(t, fixture.health, fixture.events)
	host := newHost(t, server)
	first, err := host.Open()
	testassert.NoError(t, err)

	host.mu.Lock()
	barrier := &errBarrierContext{Context: host.connCtx, entered: make(chan struct{}), release: make(chan struct{})}
	host.connCtx = barrier
	host.mu.Unlock()

	pulled := make(chan error, 1)
	go func() {
		_, err := host.NextEvent("1")
		pulled <- err
	}()
	close(fixture.terminate) // the real producer ends the old stream
	recvBounded(t, barrier.entered)

	type openResult struct {
		info ConnectionInfo
		err  error
	}
	opened := make(chan openResult, 1)
	go func() {
		second, err := host.Open()
		opened <- openResult{info: second, err: err}
	}()
	recvBounded(t, fixture.held) // the newer attempt is pending at its health gate

	close(barrier.release) // the cleanup proceeds through its read-to-lock window
	{
		err := recvBounded(t, pulled)
		testassert.Require(t, errors.Is(err, client.ErrResyncRequired), "old pull = %v, want the terminal resync error", err)
	}

	close(fixture.release)
	result := recvBounded(t, opened)
	testassert.Require(t, result.err == nil && result.info.Generation == "2", "pending Open after the late cleanup = (%+v, %v), want the cleanup to leave the pending attempt intact", result.info, result.err)
	{
		_, err := host.Call(first.Generation, "GetHealth", "")
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "stale connection generation"), "old stamp after the new install = %v, want the stale rejection", err)
	}
	response, err := host.Call(result.info.Generation, "GetHealth", "")
	testassert.Require(t, err == nil && response.Status == http.StatusOK, "installed generation request = (%+v, %v), want the usable connection", response, err)
}

// The mounted smoke fixture: the real production Runtime over isolated HOME,
// data and config paths, matching the client integration fixture.
const smokeConfigDocument = `{"providers":{"prov":{"transport":{"base_url":%q,"api_key_env":"DESKTOP_TEST_KEY"},"discovery":false,"models":{"m":{"name":"M","context_window":4096,"max_output_tokens":512}}}}}`

const smokeAgentsDocument = `{"integrated":{"model":"prov/m","system_prompt":"simple"}}`

// writeServiceFile writes one owner configuration file.
func writeServiceFile(t *testing.T, path, content string) {
	t.Helper()
	testassert.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	{
		err := os.WriteFile(path, []byte(content), 0o600)
		testassert.Require(t, err == nil, "write %s: %v", path, err)
	}
}

// isolateBundledCredentials empties every api_key_env declared by the bundled
// catalog so a captured build never starts a provider discovery attempt from
// an ambient shell key.
func isolateBundledCredentials(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "internal", "catalog", "builtin"))
	testassert.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "..", "internal", "catalog", "builtin", entry.Name()))
		if err != nil {
			t.Fatalf("read bundled provider %s: %v", entry.Name(), err)
		}
		var provider struct {
			Transport struct {
				APIKeyEnv string `json:"api_key_env"`
			} `json:"transport"`
		}
		if err := json.Unmarshal(data, &provider); err != nil {
			t.Fatalf("decode bundled provider %s: %v", entry.Name(), err)
		}
		if provider.Transport.APIKeyEnv != "" {
			t.Setenv(provider.Transport.APIKeyEnv, "")
		}
	}
}

// TestDesktopHostMountedRuntime proves the host composition over one real
// authenticated owner: Open, one byte request, one session creation, and one
// pulled generated event.
func TestDesktopHostMountedRuntime(t *testing.T) {
	const baseURL = "http://127.0.0.1:1/v1"
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DESKTOP_TEST_KEY", "desktop-test-key")
	isolateBundledCredentials(t)
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, fmt.Sprintf(smokeConfigDocument, baseURL))
	writeServiceFile(t, agents.PathForConfig(configPath), smokeAgentsDocument)
	owner, err := runtime.Open(context.Background(), runtime.Options{
		DataDir:    dataDir,
		ConfigPath: configPath,
		Plugins:    builtin.Plugins(),
	})
	testassert.NoError(t, err)
	t.Cleanup(func() {
		{
			err := owner.Close(context.Background())
			testassert.Check(t, err == nil, "runtime.Close: %v", err)
		}
	})
	server, err := owner.OpenProtocol(context.Background())
	testassert.NoError(t, err)
	testassert.NoError(t, server.PublishDiscovery(filepath.Join(dataDir, "runtime.json")))

	host := New(clienthost.Options{DataDir: dataDir, Executable: missingExecutable(t)}, "/ws/smoke")
	host.startup(context.Background())
	defer host.shutdown(context.Background())

	info, err := host.Open()
	testassert.NoError(t, err)
	testassert.Require(t, info.InstanceID != "" && info.Generation == "1" && info.Workspace == "/ws/smoke", "connection info = %+v", info)

	response, err := host.Call(info.Generation, "GetHealth", "")
	testassert.NoError(t, err)
	testassert.Equal(t, response.Status, http.StatusOK)
	var health protocol.Health
	testassert.NoError(t, json.Unmarshal([]byte(response.Body), &health))
	testassert.Require(t, health.InstanceId == info.InstanceID && health.ProtocolVersion == protocol.N5, "health = %+v, want the connected owner", health)

	create, err := host.Call(info.Generation, "CreateSession", `{"body":{"workspace":"/ws/smoke","agent_type":"integrated"}}`)
	testassert.NoError(t, err)
	testassert.Require(t, create.Status == http.StatusCreated, "create status = %d: %s", create.Status, create.Body)
	var created protocol.Session
	testassert.NoError(t, json.Unmarshal([]byte(create.Body), &created))

	// The established subscription is the delivery barrier: the committed
	// creation hint reaches the native pull.
	deadline := time.After(30 * time.Second)
	for {
		type pullResult struct {
			raw string
			err error
		}
		pulled := make(chan pullResult, 1)
		go func() {
			raw, err := host.NextEvent(info.Generation)
			pulled <- pullResult{raw: raw, err: err}
		}()
		select {
		case result := <-pulled:
			testassert.Require(t, result.err == nil, "NextEvent: %v", result.err)
			var event protocol.Event
			testassert.NoError(t, json.Unmarshal([]byte(result.raw), &event))
			kind, err := event.Discriminator()
			testassert.NoError(t, err)
			if kind != "session_changed" {
				continue
			}
			body, err := event.AsSessionChangedEvent()
			testassert.NoError(t, err)
			scope, err := body.Scope.AsSessionScope()
			testassert.NoError(t, err)
			if scope.SessionId != created.SessionId {
				continue
			}
			testassert.Equal(t, body.SessionRevision.InstanceId, info.InstanceID)
			return
		case <-deadline:
			t.Fatal("no session_changed event for the created session arrived")
		}
	}
}

// TestDesktopHostAppOptions pins the exact options the host hands to Wails:
// the shared window size, the Workspace-basename title, the given assets, the
// private lifecycle hooks and the host binding, and the retained Linux WebKit
// GPU policy — observed without opening a native window.
func TestDesktopHostAppOptions(t *testing.T) {
	host := New(clienthost.Options{}, filepath.Join("some", "WorkspaceDir"))
	assets := os.DirFS(t.TempDir())
	opts := appOptions(host, assets)
	testassert.Require(t, opts.Width == 900 && opts.Height == 700, "window size = %dx%d, want 900x700", opts.Width, opts.Height)
	{
		want := "Lightcode — WorkspaceDir"
		testassert.Equal(t, opts.Title, want)
	}
	testassert.Require(t, opts.AssetServer != nil && opts.AssetServer.Assets == assets, "AssetServer.Assets = %v, want the given filesystem", opts.AssetServer)
	testassert.Require(t, opts.OnStartup != nil && opts.OnShutdown != nil, "the private lifecycle hooks must be wired into the options")
	// The wired hooks act on the supplied host: startup stores the caller's
	// context and closes readiness (observed without waiting on the host's
	// lifecycle), and shutdown marks that same host closed.
	testCtx, cancelTestCtx := context.WithCancel(context.Background())
	defer cancelTestCtx()
	opts.OnStartup(testCtx)
	testassert.Equal(t, host.hostCtx, testCtx)
	select {
	case <-host.ready:
	default:
		t.Fatal("OnStartup did not close the host readiness channel")
	}
	opts.OnShutdown(testCtx)
	testassert.Require(t, host.closed, "OnShutdown did not close the host")
	testassert.Require(t, len(opts.Bind) == 1 && opts.Bind[0] == interface{}(host) && opts.Linux != nil && opts.Linux.WebviewGpuPolicy == linux.WebviewGpuPolicyAlways, "Bind = %v, want exactly the host; Linux GPU policy = %v, want WebviewGpuPolicyAlways", opts.Bind, opts.Linux)
}
