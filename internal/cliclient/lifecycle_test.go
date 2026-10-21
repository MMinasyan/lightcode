package cliclient

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
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

type liveRuntime struct {
	opts      clienthost.Options
	mu        sync.Mutex
	calls     []string
	feeds     chan protocol.Event
	streams   chan struct{}
	hydration protocol.Hydration
	special   func(http.ResponseWriter, *http.Request) bool
}

func liveFixture(t *testing.T, active bool, special func(http.ResponseWriter, *http.Request) bool) *liveRuntime {
	t.Helper()
	live := &liveRuntime{feeds: make(chan protocol.Event, 1024), streams: make(chan struct{}, 32), hydration: hydration(rootA, "1", []protocol.ConversationItem{assistant("canonical", "canonical clipboard", false)}, nil), special: special}
	if active {
		live.hydration.ActiveOperation = &protocol.Operation{OperationId: strings.Repeat("2", 32), RequestKind: "message", Status: "running", AgentType: "captured", Model: "captured/model"}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		live.mu.Lock()
		live.calls = append(live.calls, r.Method+" "+r.URL.RequestURI())
		live.mu.Unlock()
		testassert.Check(t, r.Header.Get("Authorization") == "Bearer "+strings.Repeat("1", 64), "unauthenticated generated request")
		if live.special != nil && live.special(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/health":
			health(w)
		case r.URL.Path == "/v1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			live.streams <- struct{}{}
			for {
				select {
				case <-r.Context().Done():
					return
				case event := <-live.feeds:
					raw, _ := json.Marshal(event)
					if _, err := fmt.Fprintf(w, "event: notification\ndata: %s\n\n", raw); err != nil {
						return
					}
					w.(http.Flusher).Flush()
				}
			}
		case r.URL.Path == "/v1/configuration":
			_ = json.NewEncoder(w).Encode(configuration("1"))
		case r.URL.Path == "/v1/warnings":
			_ = json.NewEncoder(w).Encode(warnings("1"))
		case r.URL.Path == "/v1/sessions":
			_ = json.NewEncoder(w).Encode([]protocol.Session{{SessionId: childA, ParentSessionId: pointer(rootA)}, {SessionId: rootA, Lifecycle: "open", Workspace: "/workspace"}})
		case strings.HasSuffix(r.URL.Path, "/hydration"):
			h := live.hydration
			h.Session.SessionId = strings.Split(r.URL.Path, "/")[3]
			if h.Session.SessionId != rootA {
				h.Session.ParentSessionId = pointer(rootA)
			}
			_ = json.NewEncoder(w).Encode(h)
		case strings.HasSuffix(r.URL.Path, "/submit"):
			var body protocol.SubmitRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(protocol.SubmitResult{Disposition: "admitted", Operation: &protocol.Operation{OperationId: body.OperationId}})
		case strings.HasSuffix(r.URL.Path, "/interrupt") || strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected actual route %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(protocol.Error{Code: "not_found", Message: "test route"})
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]string{"endpoint": server.URL, "instance_id": instance, "protocol_version": string(protocol.N2), "credential": strings.Repeat("1", 64)})
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "runtime.json"), raw, 0600))
	live.opts = clienthost.Options{DataDir: dir}
	return live
}
func pipeInput(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	read, write, err := os.Pipe()
	testassert.NoError(t, err)
	t.Cleanup(func() { read.Close(); write.Close() })
	return read, write
}
func runClient(c *CLI, input *os.File, output io.Writer) <-chan error {
	result := make(chan error, 1)
	go func() { result <- c.Run(context.Background(), input, output) }()
	return result
}
func assertNoImplicitControls(t *testing.T, live *liveRuntime) {
	t.Helper()
	live.mu.Lock()
	defer live.mu.Unlock()
	for _, route := range live.calls {
		testassert.Require(t, !strings.Contains(route, "shutdown") && !strings.Contains(route, "/interrupt") && !strings.Contains(route, "/stop"), "implicit control on exit: %s", route)
	}
}

type blockedWriter struct {
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	copyOnly  bool
	ready     chan struct{}
	readyOnce sync.Once
}

func (w *blockedWriter) Write(data []byte) (int, error) {
	if w.ready != nil && strings.Contains(string(data), "canonical clipboard") {
		w.readyOnce.Do(func() { close(w.ready) })
	}
	if !w.copyOnly || strings.Contains(string(data), "\x1b]52;") {
		w.once.Do(func() { close(w.entered) })
		<-w.release
	}
	return len(data), nil
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRunEOFSignalReadErrorAndOutputError(t *testing.T) {
	for _, exit := range []string{"EOF", "signal", "read-error", "output-error"} {
		t.Run(exit, func(t *testing.T) {
			live := liveFixture(t, false, nil)
			input, write := pipeInput(t)
			c := New(Options{Startup: live.opts, Workspace: "/workspace"})
			var output io.Writer = io.Discard
			if exit == "output-error" {
				output = errorWriter{}
			}
			if exit == "read-error" {
				input.Close()
			}
			done := runClient(c, input, output)
			if exit != "read-error" && exit != "output-error" {
				barrier(t, live.streams)
				switch exit {
				case "EOF":
					write.Close()
				case "signal":
					c.handleSignal(syscall.SIGTERM, int(input.Fd()))
				}
			}
			err := receive(t, done)
			switch exit {
			case "EOF":
				testassert.NoError(t, err)
			case "signal":
				var e ExitError
				testassert.Require(t, errors.As(err, &e) && e.Code == 130, "%v", err)
			case "read-error":
				testassert.Require(t, err != nil && !errors.Is(err, io.EOF), "read error hidden")
			case "output-error":
				testassert.Require(t, errors.Is(err, io.ErrClosedPipe), "%v", err)
			}
			assertNoImplicitControls(t, live)
		})
	}
}

// TestRunInputPressureBackpressuresAndReleases pins the accepted input-pressure
// contract: a full key queue makes the input reader wait for the writer instead
// of ending the client, and an EOF already pending behind the blocked keys is
// observed only after they were admitted, keeping the ordinary EOF teardown.
func TestRunInputPressureBackpressuresAndReleases(t *testing.T) {
	live := liveFixture(t, false, nil)
	input, write := pipeInput(t)
	c := New(Options{Startup: live.opts, Workspace: "/workspace"})
	blocked := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	done := runClient(c, input, blocked)
	barrier(t, live.streams)
	barrier(t, blocked.entered)
	// Far more keys than the queue holds: every one past capacity waits, so the
	// reader never reads past them while the input is closed behind them.
	{
		_, err := write.Write([]byte(strings.Repeat("x", 5000)))
		testassert.NoError(t, err)
	}
	write.Close()
	close(blocked.release)
	testassert.NoError(t, receive(t, done))
	assertNoImplicitControls(t, live)
}

// TestRunInputPressureReleasesOnSignal pins that explicit signal cancellation
// releases a capacity-blocked input reader without waiting for output
// capacity, holding no exit/state mutex across the wait.
func TestRunInputPressureReleasesOnSignal(t *testing.T) {
	live := liveFixture(t, false, nil)
	input, write := pipeInput(t)
	c := New(Options{Startup: live.opts, Workspace: "/workspace"})
	blocked := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(blocked.release)
	done := runClient(c, input, blocked)
	barrier(t, live.streams)
	barrier(t, blocked.entered)
	{
		_, err := write.Write([]byte(strings.Repeat("x", 5000)))
		testassert.NoError(t, err)
	}
	barrierUntilFull(t, c)
	c.handleSignal(syscall.SIGTERM, int(input.Fd()))
	err := receive(t, done)
	var e ExitError
	testassert.Require(t, errors.As(err, &e) && e.Code == 130, "Run = %v, want the signal exit while input pressure persists", err)
	assertNoImplicitControls(t, live)
}

// barrierUntilFull waits until the bounded key queue is full: the reader is then
// provably blocked on admission, not read-ahead.
func barrierUntilFull(t *testing.T, c *CLI) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for len(c.keys) != cap(c.keys) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	testassert.Require(t, len(c.keys) == cap(c.keys), "the bounded key queue never filled")
}

func TestRunParkedStartupReadStopAndOutputCancel(t *testing.T) {
	for _, stage := range []string{"startup", "read", "Stop", "output"} {
		t.Run(stage, func(t *testing.T) {
			entered, cancelled := make(chan struct{}), make(chan struct{})
			var once sync.Once
			special := func(w http.ResponseWriter, r *http.Request) bool {
				park := stage == "startup" && strings.HasSuffix(r.URL.Path, "/health") || stage == "read" && strings.HasSuffix(r.URL.Path, "/hydration") || stage == "Stop" && strings.HasSuffix(r.URL.Path, "/stop")
				if !park {
					return false
				}
				once.Do(func() { close(entered) })
				<-r.Context().Done()
				close(cancelled)
				return true
			}
			live := liveFixture(t, false, special)
			input, write := pipeInput(t)
			c := New(Options{Startup: live.opts, Workspace: "/workspace"})
			screen := newScreen(80)
			var output io.Writer = screen
			var blocked *blockedWriter
			if stage == "output" {
				blocked = &blockedWriter{entered: entered, release: make(chan struct{})}
				output = blocked
				defer close(blocked.release)
			}
			done := runClient(c, input, output)
			if stage == "Stop" {
				frameContaining(t, screen, "canonical clipboard")
				_, _ = write.Write([]byte("/stop\r"))
			}
			barrier(t, entered)
			write.Close()
			testassert.NoError(t, receive(t, done))
			if stage != "output" {
				barrier(t, cancelled)
			}
			if stage != "Stop" {
				assertNoImplicitControls(t, live)
			}
		})
	}
}

func TestRunSSEBeforeReadsRejectsEstablishmentFailure(t *testing.T) {
	live := liveFixture(t, false, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/events" {
			return false
		}
		w.WriteHeader(503)
		return true
	})
	input, _ := pipeInput(t)
	c := New(Options{Startup: live.opts, Workspace: "/workspace"})
	err := receive(t, runClient(c, input, io.Discard))
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "event stream establishment"), "%v", err)
	live.mu.Lock()
	defer live.mu.Unlock()
	for _, route := range live.calls {
		testassert.Require(t, !strings.Contains(route, "configuration") && !strings.Contains(route, "hydration") && !strings.Contains(route, "sessions?"), "state read before SSE: %s", route)
	}
}

func TestRunBlockedOutputSSEOverflowRepairsBoundedQueue(t *testing.T) {
	live := liveFixture(t, false, nil)
	input, write := pipeInput(t)
	w := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(w.release)
	c := New(Options{Startup: live.opts, Workspace: "/workspace"})
	done := runClient(c, input, w)
	barrier(t, live.streams)
	barrier(t, w.entered)
	for i := 0; i < 800; i++ {
		live.feeds <- configHint("2")
	}
	barrier(t, live.streams) // actual second establishment, not polling a counter
	c.mu.Lock()
	testassert.Require(t, cap(c.events) == 256 && len(c.events) <= 256, "unbounded event sink")
	c.mu.Unlock()
	write.Close()
	testassert.NoError(t, receive(t, done))
	assertNoImplicitControls(t, live)
}

func TestRunExplicitInterruptStopAndChildCannotSubmit(t *testing.T) {
	interrupt, stop := make(chan struct{}, 1), make(chan struct{}, 1)
	live := liveFixture(t, true, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/interrupt") {
			interrupt <- struct{}{}
			w.WriteHeader(204)
			return true
		}
		if strings.HasSuffix(r.URL.Path, "/stop") {
			stop <- struct{}{}
			w.WriteHeader(204)
			return true
		}
		return false
	})
	input, write := pipeInput(t)
	c := New(Options{Startup: live.opts, Workspace: "/workspace"})
	screen := newScreen(80)
	done := runClient(c, input, screen)
	frameContaining(t, screen, "Active message")
	_, _ = write.Write([]byte{0x1b})
	barrier(t, interrupt)
	_, _ = write.Write([]byte{0x03})
	barrier(t, interrupt)
	_, _ = write.Write([]byte("/stop\r"))
	barrier(t, stop)
	// Inspect through the real generated hydration reader; Enter remains
	// read-only, while explicit Stop continues addressing the selected root.
	c.inspectChild(childA)
	frameContaining(t, screen, "child inspection")
	_, _ = write.Write([]byte("must not follow up\r"))
	frameContaining(t, screen, "Return to a selected root")
	_, _ = write.Write([]byte{0x04})
	_, _ = write.Write([]byte(strings.Repeat("\x7f", len("must not follow up")) + "/context\r"))
	frameContaining(t, screen, `"used_tokens"`)
	c.handleSignal(syscall.SIGTERM, int(input.Fd()))
	var exit ExitError
	{
		err := receive(t, done)
		testassert.Require(t, errors.As(err, &exit) && exit.Code == 130, "%v", err)
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	for _, call := range live.calls {
		testassert.Require(t, !strings.Contains(call, "/submit"), "child input submitted work")
		testassert.Require(t, !strings.Contains(call, "/stop") || strings.Contains(call, rootA), "Stop addressed inspector rather than root")
	}
}

func TestActualInputRegularQueuedResetAndEOFDoesNotControlOwner(t *testing.T) {
	submitted := make(chan protocol.SubmitRequest, 3)
	reads := make(chan struct{}, 8)
	var ownerMu sync.Mutex
	ownerIDs := make(map[string]bool)
	live := liveFixture(t, true, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/hydration") {
			reads <- struct{}{}
			return false
		}
		if !strings.HasSuffix(r.URL.Path, "/submit") {
			return false
		}
		var body protocol.SubmitRequest
		{
			err := json.NewDecoder(r.Body).Decode(&body)
			testassert.Check(t, err == nil, "%v", err)
		}
		submitted <- body
		ownerMu.Lock()
		ownerIDs[body.OperationId] = true
		ownerMu.Unlock()
		disposition := protocol.SubmitResultDispositionSteering
		if body.Mode == protocol.SubmitRequestModeQueued {
			disposition = protocol.SubmitResultDispositionQueued
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.SubmitResult{Disposition: disposition})
		return true
	})
	input, write := pipeInput(t)
	c := New(Options{Startup: live.opts, Workspace: "/workspace"})
	screen := newScreen(80)
	done := runClient(c, input, screen)
	frameContaining(t, screen, "Active message")
	barrier(t, reads)
	seen := make(map[string]bool)
	for i, mode := range []protocol.SubmitRequestMode{"regular", "queued", "regular"} {
		text := fmt.Sprintf("message %d", i)
		if mode == protocol.SubmitRequestModeQueued {
			_, _ = write.Write([]byte{0x14})
		}
		_, _ = write.Write([]byte(text + "\r"))
		body := receive(t, submitted)
		barrier(t, reads)
		testassert.Require(t, body.Mode == mode && contentText(body.Content) == text && len(body.OperationId) == 32 && !seen[body.OperationId], "wrong actual input translation: %#v", body)
		seen[body.OperationId] = true
	}
	write.Close()
	testassert.NoError(t, receive(t, done))
	assertNoImplicitControls(t, live)
	// The scripted owner retains all three delivered caller identities after
	// this client leaves. Runtime admission/draining semantics remain at their
	// existing producer owners, rather than a second terminal backend suite.
	ownerMu.Lock()
	defer ownerMu.Unlock()
	testassert.Require(t, len(ownerIDs) == 3, "exit discarded accepted owner work")
}

func TestIdleKeyboardCtrlCAndCtrlDExitZero(t *testing.T) {
	for _, key := range []byte{0x03, 0x04} {
		t.Run(fmt.Sprint(key), func(t *testing.T) {
			live := liveFixture(t, false, nil)
			input, write := pipeInput(t)
			c := New(Options{Startup: live.opts, Workspace: "/workspace"})
			screen := newScreen(80)
			done := runClient(c, input, screen)
			frameContaining(t, screen, "canonical clipboard")
			_, _ = write.Write([]byte{key})
			testassert.NoError(t, receive(t, done))
			assertNoImplicitControls(t, live)
		})
	}
}
