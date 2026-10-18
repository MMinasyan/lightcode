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
	return fmt.Sprintf(`{"endpoint":%q,"instance_id":%q,"protocol_version":"2","credential":%q}`,
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
		fmt.Fprintf(w, `{"instance_id":%q,"protocol_version":"2"}`, testInstance)
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
	if err := os.WriteFile(filepath.Join(dataDir, "runtime.json"), []byte(recordContent(server.URL)), 0o600); err != nil {
		t.Fatalf("write discovery record: %v", err)
	}
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
		if time.Now().After(deadline) {
			t.Fatal("the event pull never took the outstanding-pull gate")
		}
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
		if time.Now().After(deadline) {
			t.Fatal("the stream never reported its terminal error")
		}
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
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if first.InstanceID != testInstance {
		t.Fatalf("first instance = %q, want %q", first.InstanceID, testInstance)
	}
	if first.Workspace != "/ws/name" {
		t.Fatalf("first workspace = %q, want the host workspace", first.Workspace)
	}
	if first.Generation != "1" {
		t.Fatalf("first generation = %q, want the first monotonic stamp", first.Generation)
	}

	second, err := host.Open()
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if second.Generation != "2" {
		t.Fatalf("second generation = %q, want the next monotonic stamp", second.Generation)
	}

	if _, err := host.Call(first.Generation, "GetHealth", ""); err == nil {
		t.Fatal("a superseded generation accepted a call")
	}
	if _, err := host.NextEvent(first.Generation); err == nil {
		t.Fatal("a superseded generation accepted an event pull")
	}

	response, err := host.Call(second.Generation, "GetHealth", "")
	if err != nil {
		t.Fatalf("current generation Call: %v", err)
	}
	if response.Status != http.StatusOK {
		t.Fatalf("health status = %d, want 200", response.Status)
	}
	if !strings.Contains(response.Body, testInstance) {
		t.Fatalf("health body = %q, want the authenticated instance", response.Body)
	}
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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := host.Call(info.Generation, "GetSessionHydration", `{"session_id":"parked"}`)
		done <- err
	}()
	<-arrived
	if _, err := host.Open(); err != nil {
		t.Fatalf("superseding Open: %v", err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("superseded call = %v, want context.Canceled", err)
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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	success, err := host.Call(info.Generation, "GetHealth", "")
	if err != nil {
		t.Fatalf("success Call: %v", err)
	}
	if success.Status != http.StatusOK || success.Invalid || !strings.Contains(success.Body, testInstance) {
		t.Fatalf("success response = %+v, want the authenticated health body", success)
	}

	rejected, err := host.Call(info.Generation, "RevertSessionCode", `{"session_id":"abc","body":{"operation_id":"op-1"}}`)
	if err != nil {
		t.Fatalf("rejection Call: %v", err)
	}
	if rejected.Status != http.StatusConflict || rejected.Invalid || rejected.Body != partial {
		t.Fatalf("rejection response = %+v, want the validated partial body without rejection", rejected)
	}

	empty, err := host.Call(info.Generation, "InterruptSession", `{"session_id":"abc"}`)
	if err != nil {
		t.Fatalf("204 Call: %v", err)
	}
	if empty.Status != http.StatusNoContent || empty.Invalid || empty.Body != "" {
		t.Fatalf("204 response = %+v, want a valid empty success", empty)
	}

	invalid, err := host.Call(info.Generation, "GetSessionHydration", `{"session_id":"malformed"}`)
	if err != nil {
		t.Fatalf("invalid Call: %v", err)
	}
	if invalid.Status != http.StatusOK || !invalid.Invalid || invalid.Body != "not json" {
		t.Fatalf("invalid response = %+v, want the received status/body with the Invalid bit", invalid)
	}

	if _, err := host.Call(info.Generation, "Nope", ""); err == nil {
		t.Fatal("an unknown operation did not reject the binding")
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
		if _, err := host.NextEvent("1"); err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("NextEvent = %v, want a closure loss", err)
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
		if _, err := host.NextEvent("1"); err == nil || !strings.Contains(err.Error(), "already outstanding") {
			t.Fatalf("second pull = %v, want the one outstanding pull", err)
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
		if _, err := host.NextEvent("1"); err == nil || !strings.Contains(err.Error(), "closed without a terminal error") {
			t.Fatalf("NextEvent = %v, want the closure loss", err)
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
		if _, err := host.NextEvent("1"); !errors.Is(err, client.ErrResyncRequired) {
			t.Fatalf("NextEvent = %v, want the producer's queued terminal error", err)
		}
	})

	t.Run("an observed loss retires the generation and its buffered progress", func(t *testing.T) {
		events := make(chan protocol.Event, 1)
		errs := make(chan error, 1)
		var scope protocol.Scope
		if err := scope.FromRuntimeScope(protocol.RuntimeScope{Kind: protocol.RuntimeScopeKindRuntime}); err != nil {
			t.Fatalf("build scope: %v", err)
		}
		var event protocol.Event
		if err := event.FromScopeEvent(protocol.ScopeEvent{Kind: protocol.ScopeOpened, Scope: scope}); err != nil {
			t.Fatalf("build event: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		host := &Host{generation: "1", events: events, errs: errs, connCtx: ctx, connCancel: cancel}
		errs <- client.ErrResyncRequired
		if _, err := host.NextEvent("1"); !errors.Is(err, client.ErrResyncRequired) {
			t.Fatalf("pull = %v, want the observed terminal error", err)
		}
		// The loss retires the generation before any later pull can observe
		// its remaining buffered progress.
		events <- event
		if _, err := host.NextEvent("1"); err == nil || !strings.Contains(err.Error(), "stale connection generation") {
			t.Fatalf("later pull = %v, want the stale-generation rejection", err)
		}
		host.mu.Lock()
		canceled, retired := ctx.Err() != nil, host.generation == ""
		host.mu.Unlock()
		if !canceled || !retired {
			t.Fatal("an observed loss must cancel and retire only its matching generation")
		}
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
	if err := scope.FromRuntimeScope(protocol.RuntimeScope{Kind: protocol.RuntimeScopeKindRuntime}); err != nil {
		t.Fatalf("build scope: %v", err)
	}
	var event protocol.Event
	if err := event.FromScopeEvent(protocol.ScopeEvent{Kind: protocol.ScopeOpened, Scope: scope}); err != nil {
		t.Fatalf("build event: %v", err)
	}
	want, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := &Host{generation: "1", events: events, errs: errs, connCtx: ctx, connCancel: cancel}

	events <- event
	raw, err := host.NextEvent("1")
	if err != nil || raw != string(want) {
		t.Fatalf("progress pull = (%q, %v), want the exact %q ahead of the unobserved terminal", raw, err, want)
	}
	errs <- client.ErrResyncRequired
	if _, err := host.NextEvent("1"); !errors.Is(err, client.ErrResyncRequired) {
		t.Fatalf("later pull = %v, want the now-observed terminal error", err)
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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	close(start)
	awaitTerminal(t, host)

	var observed error
	for pulls := 0; ; pulls++ {
		if pulls > total {
			t.Fatal("the terminal failure was never observed")
		}
		raw, err := host.NextEvent(info.Generation)
		if err != nil {
			observed = err
			break
		}
		var delivered map[string]any
		if json.Unmarshal([]byte(raw), &delivered) != nil || delivered["kind"] != "session_changed" {
			t.Fatalf("pull delivered %q, want only the valid queued progress", raw)
		}
	}
	if !errors.Is(observed, client.ErrResyncRequired) {
		t.Fatalf("observed terminal = %v, want the overflow resync error", observed)
	}
	host.mu.Lock()
	generationErr := host.connCtx.Err()
	retired := host.generation == ""
	host.mu.Unlock()
	if generationErr == nil {
		t.Fatal("the terminal failure did not cancel its connection generation")
	}
	if !retired {
		t.Fatal("the terminal failure did not retire its connection generation")
	}
	if _, err := host.NextEvent(info.Generation); err == nil || !strings.Contains(err.Error(), "stale connection generation") {
		t.Fatalf("later pull = %v, want the stale-generation rejection", err)
	}
	if _, err := host.Call(info.Generation, "GetHealth", ""); err == nil || !strings.Contains(err.Error(), "stale connection generation") {
		t.Fatal("a later request acted healthy on the lost generation")
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
	if _, err := host.Open(); err == nil {
		t.Fatal("Open succeeded over a failed event stream")
	}
	if _, err := host.Call("1", "GetHealth", ""); err == nil {
		t.Fatal("no generation was installed, yet a call succeeded")
	}
	if _, err := host.NextEvent("1"); err == nil {
		t.Fatal("no generation was installed, yet a pull succeeded")
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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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
	if err := <-requestDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("closed request = %v, want context.Canceled", err)
	}
	if err := <-pullDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("closed pull = %v, want context.Canceled", err)
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
		if err != nil || got != "/chosen" {
			t.Fatalf("PickDirectory = (%q, %v)", got, err)
		}
	})

	t.Run("directory cancel is an empty result", func(t *testing.T) {
		host := New(clienthost.Options{}, "/ws/name")
		host.startup(context.Background())
		defer host.shutdown(context.Background())
		host.pickDirFn = func(context.Context) (string, error) { return "", nil }
		got, err := host.PickDirectory()
		if err != nil || got != "" {
			t.Fatalf("PickDirectory = (%q, %v), want the cancel result", got, err)
		}
	})

	t.Run("title matches the one shared algorithm", func(t *testing.T) {
		for _, workspace := range []string{"/ws/name", ".", "/", ""} {
			host := New(clienthost.Options{}, workspace)
			host.startup(context.Background())
			var applied []string
			host.titleFn = func(_ context.Context, title string) { applied = append(applied, title) }
			if err := host.SetTitle(workspace); err != nil {
				t.Fatalf("SetTitle(%q): %v", workspace, err)
			}
			if err := host.SetTitle(workspace); err != nil {
				t.Fatalf("second SetTitle(%q): %v", workspace, err)
			}
			// The initial window title and every subsequent SetTitle apply
			// the one shared algorithm: the plain basename join, no
			// special case.
			want := "Lightcode — " + filepath.Base(workspace)
			if got := windowTitle(workspace); got != want {
				t.Fatalf("initial title for %q = %q, want %q", workspace, got, want)
			}
			if applied[0] != want || applied[1] != want {
				t.Fatalf("SetTitle(%q) applied %q, want the initial title %q", workspace, applied, want)
			}
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

		if got := <-directory; got.err != nil || got.value != "/chosen" {
			t.Fatalf("PickDirectory after startup = %+v", got)
		}
		if got := <-title; got.err != nil {
			t.Fatalf("SetTitle after startup = %+v", got)
		}
		if got := <-opened; got.err == nil || !strings.Contains(got.err.Error(), "connect runtime:") {
			t.Fatalf("Open after startup = %v, want the actual clienthost.Connect outcome", got.err)
		}
		if nativeCtx != startupCtx {
			t.Fatal("the native dialog did not receive the exact startup context")
		}
		if titledCtx != startupCtx {
			t.Fatal("the native title call did not receive the exact startup context")
		}
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

		if got := <-directory; got.err == nil {
			t.Fatalf("PickDirectory after shutdown-before-startup = %+v, want an error", got)
		}
		if got := <-title; got.err == nil {
			t.Fatalf("SetTitle after shutdown-before-startup = %+v, want an error", got)
		}
		if got := <-opened; got.err == nil || !strings.Contains(got.err.Error(), "closed") {
			t.Fatalf("Open after shutdown-before-startup = %v, want the closed-host error", got.err)
		}
		if invoked {
			t.Fatal("shutdown-before-startup invoked a native presentation function")
		}
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

	if got, err := host.PickDirectory(); err == nil {
		t.Fatalf("PickDirectory after close = (%q, %v), want the closed-host error", got, err)
	}
	if err := host.SetTitle("/ws/name"); err == nil {
		t.Fatal("SetTitle after close succeeded")
	}
	if _, err := host.Open(); err == nil {
		t.Fatal("Open after close succeeded")
	}
	if invoked {
		t.Fatal("post-close native calls reached the injected presentation functions")
	}
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
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"instance_id":%q,"protocol_version":"2"}`, testInstance)
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
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if first.Generation != "1" {
		t.Fatalf("first generation = %q, want 1", first.Generation)
	}
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

	if _, err := host.Call(first.Generation, "GetHealth", ""); err == nil || !strings.Contains(err.Error(), "stale connection generation") {
		t.Fatalf("old-stamp request during a pending open = %v, want rejection before I/O", err)
	}
	if _, err := host.NextEvent(first.Generation); err == nil || !strings.Contains(err.Error(), "stale connection generation") {
		t.Fatalf("old-stamp pull during a pending open = %v, want rejection before I/O", err)
	}

	close(fixture.release)
	result := recvBounded(t, opened)
	if result.err != nil || result.info.Generation != "2" {
		t.Fatalf("pending Open = (%+v, %v), want the installed second generation", result.info, result.err)
	}
	response, err := host.Call(result.info.Generation, "GetHealth", "")
	if err != nil || response.Status != http.StatusOK {
		t.Fatalf("new-generation request = (%+v, %v), want the usable install", response, err)
	}
}

// TestDesktopHostFailedOpenDoesNotResurrectOldStamp proves a failed newer
// attempt leaves no valid stamp at all: the old generation stays rejected
// instead of silently becoming authoritative again.
func TestDesktopHostFailedOpenDoesNotResurrectOldStamp(t *testing.T) {
	var mu sync.Mutex
	var healthCalls int
	failingHealth := func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		healthCalls++
		second := healthCalls > 1
		mu.Unlock()
		if second {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"instance_id":%q,"protocol_version":"2"}`, testInstance)
	}
	server := newFixture(t, failingHealth, parkedStream())
	host := newHost(t, server)
	first, err := host.Open()
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := host.Open(); err == nil {
		t.Fatal("second Open succeeded over a failing discovery owner")
	}

	if _, err := host.Call(first.Generation, "GetHealth", ""); err == nil || !strings.Contains(err.Error(), "stale connection generation") {
		t.Fatalf("old-stamp request after a failed open = %v, want the retired stamp to stay rejected", err)
	}
	if _, err := host.NextEvent(first.Generation); err == nil || !strings.Contains(err.Error(), "stale connection generation") {
		t.Fatalf("old-stamp pull after a failed open = %v, want the retired stamp to stay rejected", err)
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
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

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
	if err := recvBounded(t, pulled); !errors.Is(err, client.ErrResyncRequired) {
		t.Fatalf("old pull = %v, want the terminal resync error", err)
	}

	close(fixture.release)
	result := recvBounded(t, opened)
	if result.err != nil || result.info.Generation != "2" {
		t.Fatalf("pending Open after the late cleanup = (%+v, %v), want the cleanup to leave the pending attempt intact", result.info, result.err)
	}
	if _, err := host.Call(first.Generation, "GetHealth", ""); err == nil || !strings.Contains(err.Error(), "stale connection generation") {
		t.Fatalf("old stamp after the new install = %v, want the stale rejection", err)
	}
	response, err := host.Call(result.info.Generation, "GetHealth", "")
	if err != nil || response.Status != http.StatusOK {
		t.Fatalf("installed generation request = (%+v, %v), want the usable connection", response, err)
	}
}

// The mounted smoke fixture: the real production Runtime over isolated HOME,
// data and config paths, matching the client integration fixture.
const smokeConfigDocument = `{"providers":{"prov":{"transport":{"base_url":%q,"api_key_env":"DESKTOP_TEST_KEY"},"discovery":false,"models":{"m":{"name":"M","context_window":4096,"max_output_tokens":512}}}}}`

const smokeAgentsDocument = `{"integrated":{"model":"prov/m","system_prompt":"simple"}}`

// writeServiceFile writes one owner configuration file.
func writeServiceFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// isolateBundledCredentials empties every api_key_env declared by the bundled
// catalog so a captured build never starts a provider discovery attempt from
// an ambient shell key.
func isolateBundledCredentials(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "internal", "catalog", "builtin"))
	if err != nil {
		t.Fatalf("read bundled catalog: %v", err)
	}
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
	if err != nil {
		t.Fatalf("runtime.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := owner.Close(context.Background()); err != nil {
			t.Errorf("runtime.Close: %v", err)
		}
	})
	server, err := owner.OpenProtocol(context.Background())
	if err != nil {
		t.Fatalf("OpenProtocol: %v", err)
	}
	if err := server.PublishDiscovery(filepath.Join(dataDir, "runtime.json")); err != nil {
		t.Fatalf("PublishDiscovery: %v", err)
	}

	host := New(clienthost.Options{DataDir: dataDir, Executable: missingExecutable(t)}, "/ws/smoke")
	host.startup(context.Background())
	defer host.shutdown(context.Background())

	info, err := host.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if info.InstanceID == "" || info.Generation != "1" || info.Workspace != "/ws/smoke" {
		t.Fatalf("connection info = %+v", info)
	}

	response, err := host.Call(info.Generation, "GetHealth", "")
	if err != nil {
		t.Fatalf("health Call: %v", err)
	}
	if response.Status != http.StatusOK {
		t.Fatalf("health response status = %d", response.Status)
	}
	var health protocol.Health
	if err := json.Unmarshal([]byte(response.Body), &health); err != nil {
		t.Fatalf("decode health body: %v", err)
	}
	if health.InstanceId != info.InstanceID || health.ProtocolVersion != protocol.N2 {
		t.Fatalf("health = %+v, want the connected owner", health)
	}

	create, err := host.Call(info.Generation, "CreateSession", `{"body":{"workspace":"/ws/smoke","agent_type":"integrated"}}`)
	if err != nil {
		t.Fatalf("create Call: %v", err)
	}
	if create.Status != http.StatusCreated {
		t.Fatalf("create status = %d: %s", create.Status, create.Body)
	}
	var created protocol.Session
	if err := json.Unmarshal([]byte(create.Body), &created); err != nil {
		t.Fatalf("decode created session: %v", err)
	}

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
			if result.err != nil {
				t.Fatalf("NextEvent: %v", result.err)
			}
			var event protocol.Event
			if err := json.Unmarshal([]byte(result.raw), &event); err != nil {
				t.Fatalf("decode pulled event: %v", err)
			}
			kind, err := event.Discriminator()
			if err != nil {
				t.Fatalf("event discriminator: %v", err)
			}
			if kind != "session_changed" {
				continue
			}
			body, err := event.AsSessionChangedEvent()
			if err != nil {
				t.Fatalf("session_changed body: %v", err)
			}
			scope, err := body.Scope.AsSessionScope()
			if err != nil {
				t.Fatalf("session_changed scope: %v", err)
			}
			if scope.SessionId != created.SessionId {
				continue
			}
			if body.SessionRevision.InstanceId != info.InstanceID {
				t.Fatalf("hint instance = %q, want %q", body.SessionRevision.InstanceId, info.InstanceID)
			}
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
	if opts.Width != 900 || opts.Height != 700 {
		t.Fatalf("window size = %dx%d, want 900x700", opts.Width, opts.Height)
	}
	if want := "Lightcode — WorkspaceDir"; opts.Title != want {
		t.Fatalf("title = %q, want %q", opts.Title, want)
	}
	if opts.AssetServer == nil || opts.AssetServer.Assets != assets {
		t.Fatalf("AssetServer.Assets = %v, want the given filesystem", opts.AssetServer)
	}
	if opts.OnStartup == nil || opts.OnShutdown == nil {
		t.Fatal("the private lifecycle hooks must be wired into the options")
	}
	// The wired hooks act on the supplied host: startup stores the caller's
	// context and closes readiness (observed without waiting on the host's
	// lifecycle), and shutdown marks that same host closed.
	testCtx, cancelTestCtx := context.WithCancel(context.Background())
	defer cancelTestCtx()
	opts.OnStartup(testCtx)
	if host.hostCtx != testCtx {
		t.Fatalf("OnStartup stored context %v, want the supplied context", host.hostCtx)
	}
	select {
	case <-host.ready:
	default:
		t.Fatal("OnStartup did not close the host readiness channel")
	}
	opts.OnShutdown(testCtx)
	if !host.closed {
		t.Fatal("OnShutdown did not close the host")
	}
	if len(opts.Bind) != 1 || opts.Bind[0] != interface{}(host) {
		t.Fatalf("Bind = %v, want exactly the host", opts.Bind)
	}
	if opts.Linux == nil || opts.Linux.WebviewGpuPolicy != linux.WebviewGpuPolicyAlways {
		t.Fatalf("Linux GPU policy = %v, want WebviewGpuPolicyAlways", opts.Linux)
	}
}
