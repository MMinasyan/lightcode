package acpclient_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/acpclient"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/internal/version"
)

// The scripted fixture speaks the real generated HTTP protocol over one
// authenticated loopback route: health, one idle event stream, and the
// operation handlers a test mounts. Every bridge assertion goes through the
// actual Run owner and its real stdio framing.

const (
	testInstance   = "0123456789abcdef0123456789abcdef"
	testCredential = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

type fixtureCall struct {
	method string
	path   string
	body   string
}

type fixture struct {
	mu         sync.Mutex
	calls      []fixtureCall
	handlers   map[string]http.HandlerFunc
	server     *httptest.Server
	eventsOpen chan struct{}
}

func newFixture(t *testing.T, routes map[string]http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{handlers: map[string]http.HandlerFunc{}, eventsOpen: make(chan struct{}, 16)}
	f.handlers["/v1/health"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, healthBody())
	}
	f.handlers["/v1/events"] = f.idleEvents
	// Startup composition defaults: an empty open-root list and one created
	// primary Session over the selected Workspace's open-lifecycle list query.
	// A test needing a different startup outcome replaces the handler.
	f.handlers["/v1/sessions"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, acpclient.SessionHeader(startupSessionID, "/ws", "2026-10-01T00:00:00Z"))
			return
		}
		testassert.Check(t, r.Method == http.MethodGet && r.URL.Query().Get("lifecycle") == "open" && r.URL.Query().Get("workspace") != "", "startup list = %s %s, want the selected Workspace's open lifecycle", r.Method, r.URL.RawQuery)
		fmt.Fprint(w, "[]")
	}
	f.handlers["/v1/sessions/"+startupSessionID+"/hydration"] = jsonHandler(200, hydrationBody(startupSessionID, "/ws"))
	for path, handler := range routes {
		f.handlers[path] = handler
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, fixtureCall{method: r.Method, path: r.URL.Path, body: string(body)})
	handler := f.handlers[r.URL.Path]
	f.mu.Unlock()
	if handler == nil {
		http.NotFound(w, r)
		return
	}
	handler(w, r)
}

func (f *fixture) hand(path string, handler http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[path] = handler
}

func (f *fixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if call.path == path {
			n++
		}
	}
	return n
}

func (f *fixture) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// paths returns every recorded request path in arrival order.
func (f *fixture) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		paths = append(paths, call.path)
	}
	return paths
}

func (f *fixture) lastBody(t *testing.T, path string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].path == path {
			return f.calls[i].body
		}
	}
	t.Fatalf("no request reached %s", path)
	return ""
}

// startupSessionID names the fixture's default created startup Session.
const startupSessionID = "s-startup"

// signalEventsOpen records one opened event stream.
func (f *fixture) signalEventsOpen() {
	select {
	case f.eventsOpen <- struct{}{}:
	default:
	}
}

// idleEvents serves one authenticated SSE stream that stays open until its
// request context ends.
func (f *fixture) idleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	f.signalEventsOpen()
	<-r.Context().Done()
}

func (f *fixture) awaitEventsOpen(t *testing.T) {
	t.Helper()
	select {
	case <-f.eventsOpen:
	case <-time.After(15 * time.Second):
		t.Fatal("the event stream was never established")
	}
}

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// gatedWriter is one output writer whose writes block after the configured
// count until released, so output pressure is a real capacity condition.
type gatedWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	gate    chan struct{}
	blocked chan struct{}
	once    sync.Once
	written chan struct{}
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{
		gate:    make(chan struct{}),
		blocked: make(chan struct{}, 1),
		written: make(chan struct{}, 1),
	}
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	select {
	case w.blocked <- struct{}{}:
	default:
	}
	<-w.gate
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	select {
	case w.written <- struct{}{}:
	default:
	}
	return n, err
}

func (w *gatedWriter) release() { w.once.Do(func() { close(w.gate) }) }

// awaitBlocked reports that one write is parked on the gate.
func (w *gatedWriter) awaitBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-w.blocked:
	case <-time.After(15 * time.Second):
		t.Fatal("no output write was parked")
	}
}

func (w *gatedWriter) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *gatedWriter) awaitContains(t *testing.T, fragment string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for !strings.Contains(w.text(), fragment) {
		select {
		case <-w.written:
		case <-deadline:
			t.Fatalf("output = %q, want %s", w.text(), fragment)
		}
	}
}

// bridge is one running Runner over an input pipe and, by default, an output
// pipe drained into a frame channel.
type bridge struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan string
	done   chan error
	cancel context.CancelFunc
}

func startBridge(t *testing.T, f *fixture, workspace string) *bridge {
	t.Helper()
	return startBridgeWith(t, f, workspace, nil)
}

// startBridgeWith starts one Runner. A nil writer drains output into the
// frame channel; a supplied writer receives the raw output.
func startBridgeWith(t *testing.T, f *fixture, workspace string, writer io.Writer) *bridge {
	t.Helper()
	dataDir := t.TempDir()
	record := fmt.Sprintf(`{"endpoint":%q,"instance_id":%q,"protocol_version":"5","credential":%q}`,
		f.server.URL, testInstance, testCredential)
	testassert.NoError(t, os.WriteFile(filepath.Join(dataDir, "runtime.json"), []byte(record), 0o600))
	return startRunner(t, clienthost.Options{
		DataDir:    dataDir,
		ConfigPath: filepath.Join(dataDir, "config.json"),
		Executable: filepath.Join(dataDir, "absent-runtime"),
	}, workspace, writer)
}

// startRunner starts one Runner over the given startup inputs. A nil writer
// drains output into the frame channel; a supplied writer receives the raw
// output.
func startRunner(t *testing.T, startup clienthost.Options, workspace string, writer io.Writer) *bridge {
	t.Helper()
	runner := acpclient.New(acpclient.Options{Startup: startup, Workspace: workspace})
	inR, inW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	b := &bridge{t: t, in: inW, done: done, cancel: cancel}
	if writer == nil {
		outR, outW := io.Pipe()
		go func() {
			err := runner.Run(ctx, inR, outW)
			_ = outW.Close()
			done <- err
		}()
		lines := make(chan string, 512)
		go func() {
			scanner := bufio.NewScanner(outR)
			scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
			close(lines)
		}()
		b.lines = lines
	} else {
		go func() { done <- runner.Run(ctx, inR, writer) }()
	}
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
	})
	return b
}

func (b *bridge) send(t *testing.T, line string) {
	t.Helper()
	{
		_, err := io.WriteString(b.in, line+"\n")
		testassert.NoError(t, err)
	}
}

func (b *bridge) eof() {
	_ = b.in.Close()
}

func (b *bridge) awaitRun(t *testing.T) error {
	t.Helper()
	select {
	case err := <-b.done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func (b *bridge) recv(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	line := b.recvLine(t)
	var frame map[string]json.RawMessage
	{
		err := json.Unmarshal([]byte(line), &frame)
		testassert.Require(t, err == nil, "output frame %q: %v", line, err)
	}
	return frame
}

// recvLine reads one raw output line.
func (b *bridge) recvLine(t *testing.T) string {
	t.Helper()
	select {
	case line, ok := <-b.lines:
		testassert.Require(t, ok, "bridge output ended")
		return line
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for one output frame")
		return ""
	}
}

// awaitFrame reads frames until the match reports true.
func (b *bridge) awaitFrame(t *testing.T, match func(map[string]json.RawMessage) bool) map[string]json.RawMessage {
	t.Helper()
	frames := b.collectFrames(t, match)
	return frames[len(frames)-1]
}

// collectFrames reads frames until stop reports true, returning every frame
// read including the stopping one. It is the one collection loop the bridge
// tests share, so an order-independent oracle exercises the same code.
func (b *bridge) collectFrames(t *testing.T, stop func(map[string]json.RawMessage) bool) []map[string]json.RawMessage {
	t.Helper()
	deadline := time.After(15 * time.Second)
	var frames []map[string]json.RawMessage
	for {
		select {
		case line, ok := <-b.lines:
			testassert.Require(t, ok, "bridge output ended before the expected frame")
			var frame map[string]json.RawMessage
			{
				err := json.Unmarshal([]byte(line), &frame)
				testassert.Require(t, err == nil, "output frame %q: %v", line, err)
			}
			frames = append(frames, frame)
			if stop(frame) {
				return frames
			}
		case <-deadline:
			t.Fatal("timed out waiting for the expected frame")
			return nil
		}
	}
}

// frameWithID returns one collected frame carrying the given raw ID.
func frameWithID(t *testing.T, frames []map[string]json.RawMessage, id string) map[string]json.RawMessage {
	t.Helper()
	for _, frame := range frames {
		if raw, ok := frame["id"]; ok && string(raw) == id {
			return frame
		}
	}
	t.Fatalf("no collected frame carries id %s: %v", id, frames)
	return nil
}

// frameWithMethod returns one collected notification carrying the method.
func frameWithMethod(t *testing.T, frames []map[string]json.RawMessage, method string) map[string]json.RawMessage {
	t.Helper()
	for _, frame := range frames {
		if raw, ok := frame["method"]; ok && string(raw) == `"`+method+`"` {
			return frame
		}
	}
	t.Fatalf("no collected frame carries method %s: %v", method, frames)
	return nil
}

func idFrame(id string) func(map[string]json.RawMessage) bool {
	return func(frame map[string]json.RawMessage) bool {
		raw, ok := frame["id"]
		return ok && string(raw) == id
	}
}

func methodFrame(method string) func(map[string]json.RawMessage) bool {
	quoted := `"` + method + `"`
	return func(frame map[string]json.RawMessage) bool {
		raw, ok := frame["method"]
		return ok && string(raw) == quoted
	}
}

func requestLine(id, method, params string) string {
	line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q`, id, method)
	if params != "" {
		line += `,"params":` + params
	}
	return line + `}`
}

func notificationLine(method, params string) string {
	line := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q`, method)
	if params != "" {
		line += `,"params":` + params
	}
	return line + `}`
}

func (b *bridge) call(t *testing.T, id, method, params string) map[string]json.RawMessage {
	t.Helper()
	b.send(t, requestLine(id, method, params))
	return b.awaitFrame(t, idFrame(id))
}

func errorFrame(t *testing.T, frame map[string]json.RawMessage) (int, string, json.RawMessage) {
	t.Helper()
	raw, ok := frame["error"]
	testassert.Require(t, ok, "frame carries no error: %s", frame)
	var rpcErr struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	{
		err := json.Unmarshal(raw, &rpcErr)
		testassert.Require(t, err == nil, "error frame %s: %v", raw, err)
	}
	return rpcErr.Code, rpcErr.Message, rpcErr.Data
}

func errorCode(t *testing.T, frame map[string]json.RawMessage) int {
	t.Helper()
	code, _, _ := errorFrame(t, frame)
	return code
}

func resultOf(t *testing.T, frame map[string]json.RawMessage) string {
	t.Helper()
	raw, ok := frame["result"]
	testassert.Require(t, ok, "frame carries no result: %s", frame)
	return string(raw)
}

func sessionChangedEvent(sessionID string) string {
	return fmt.Sprintf(`{"kind":"session_changed","scope":{"kind":"session","session_id":%q},"session_revision":{"durable_revision":"1","local_revision":"0","instance_id":%q}}`, sessionID, testInstance)
}

func sseFrame(document string) string {
	return "event: notification\ndata: " + document + "\n\n"
}

func healthBody() string {
	return fmt.Sprintf(`{"instance_id":%q,"protocol_version":"5"}`, testInstance)
}

func hydrationBody(sessionID, workspace string) string {
	return fmt.Sprintf(`{"session":{"session_id":%q,"workspace":%q,"agent_type":"primary"},"conversation":{"items":[]},"operations":[],"pending":{},"background":[],"session_revision":{"durable_revision":"1","local_revision":"0","instance_id":%q},"configuration_revision":{"instance_id":%q,"generation":"1"},"warnings":[],"warnings_revision":{"instance_id":%q,"revision":"1"}}`, sessionID, workspace, testInstance, testInstance, testInstance)
}

// TestACPFraming pins the exact JSON-RPC framing, IDs, and null results of
// the real stdio owner.
func TestACPFraming(t *testing.T) {
	f := newFixture(t, nil)
	b := startBridge(t, f, "/ws")

	t.Run("parse error", func(t *testing.T) {
		b.send(t, "not json")
		line := b.recvLine(t)
		want := `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`
		testassert.Equal(t, line, want)
	})
	t.Run("invalid version", func(t *testing.T) {
		b.send(t, `{"jsonrpc":"1.0","id":7,"method":"workspace/current"}`)
		frame := b.recv(t)
		testassert.Equal(t, errorCode(t, frame), -32600)
		testassert.Require(t, string(frame["id"]) == "7", "id = %s, want 7", frame["id"])
	})
	t.Run("missing method", func(t *testing.T) {
		b.send(t, `{"jsonrpc":"2.0","id":"a"}`)
		frame := b.recv(t)
		testassert.Equal(t, errorCode(t, frame), -32600)
		testassert.Require(t, string(frame["id"]) == `"a"`, "id = %s, want \"a\"", frame["id"])
	})
	t.Run("invalid id", func(t *testing.T) {
		b.send(t, `{"jsonrpc":"2.0","id":true,"method":"workspace/current"}`)
		frame := b.recv(t)
		testassert.Equal(t, errorCode(t, frame), -32600)
		testassert.Require(t, string(frame["id"]) == "null", "id = %s, want null", frame["id"])
	})
	t.Run("valid JSON with a non-object root", func(t *testing.T) {
		for _, row := range []struct{ name, line string }{
			{"array", `[1,2]`},
			{"scalar", `5`},
			{"string", `"scalar"`},
			{"boolean", `true`},
			{"null", `null`},
		} {
			t.Run(row.name, func(t *testing.T) {
				b.send(t, row.line)
				frame := b.recv(t)
				testassert.Equal(t, errorCode(t, frame), -32600)
				testassert.Require(t, string(frame["id"]) == "null", "id = %s, want null", frame["id"])
			})
		}
	})
	t.Run("valid JSON with typed envelope mismatches", func(t *testing.T) {
		for _, row := range []struct{ name, line, id string }{
			{"method is a number", `{"jsonrpc":"2.0","id":7,"method":5}`, "7"},
			{"method is an array", `{"jsonrpc":"2.0","id":9,"method":[]}`, "9"},
			{"version is a number", `{"jsonrpc":1,"id":7,"method":"workspace/current"}`, "7"},
			{"version is a number with a string id", `{"jsonrpc":1,"id":"a","method":"workspace/current"}`, `"a"`},
			{"version is null", `{"jsonrpc":null,"id":"b","method":"workspace/current"}`, `"b"`},
		} {
			t.Run(row.name, func(t *testing.T) {
				b.send(t, row.line)
				frame := b.recv(t)
				testassert.Equal(t, errorCode(t, frame), -32600)
				testassert.Require(t, string(frame["id"]) == row.id, "id = %s, want the available caller ID %s", frame["id"], row.id)
			})
		}
	})
	t.Run("an invalid envelope without an id answers null", func(t *testing.T) {
		b.send(t, `{"jsonrpc":"1.0","method":"workspace/current"}`)
		b.send(t, requestLine("1", "workspace/current", ""))
		frame := b.recv(t)
		testassert.Require(t, string(frame["id"]) == "null", "first frame after an invalid notification-shaped envelope = %s, want the null-id invalid request", frame)
		testassert.Equal(t, errorCode(t, frame), -32600)
		{
			marker := b.recv(t)
			testassert.Require(t, string(marker["id"]) == "1", "marker response = %s, want the drained caller request", marker)
		}
	})
	t.Run("truncated JSON is a parse error", func(t *testing.T) {
		b.send(t, `{"jsonrpc":`)
		frame := b.recv(t)
		testassert.Equal(t, errorCode(t, frame), -32700)
		testassert.Require(t, string(frame["id"]) == "null", "id = %s, want null", frame["id"])
	})
	t.Run("unknown method", func(t *testing.T) {
		frame := b.call(t, "7", "session/new", "")
		code, message, _ := errorFrame(t, frame)
		testassert.Require(t, code == -32601 && message == "method not found: session/new", "error = (%d, %q), want -32601 method not found", code, message)
	})
	t.Run("notification produces no response", func(t *testing.T) {
		b.send(t, notificationLine("workspace/current", ""))
		b.send(t, requestLine("1", "workspace/current", ""))
		frame := b.recv(t)
		testassert.Require(t, string(frame["id"]) == "1", "first frame after a notification = %s, want the marker response", frame)
	})
	t.Run("explicit null id responds", func(t *testing.T) {
		frame := b.call(t, "null", "workspace/current", "")
		testassert.Require(t, string(frame["id"]) == "null", "id = %s, want null", frame["id"])
		if resultOf(t, frame) != `"/ws"` {
			t.Fatalf("result = %s, want the initial workspace", resultOf(t, frame))
		}
	})
	t.Run("large number id stays exact", func(t *testing.T) {
		frame := b.call(t, "9007199254740993", "workspace/current", "")
		testassert.Require(t, string(frame["id"]) == "9007199254740993", "id = %s, want the exact caller number", frame["id"])
	})
	t.Run("string id stays exact", func(t *testing.T) {
		frame := b.call(t, `"caller-id"`, "workspace/current", "")
		testassert.Require(t, string(frame["id"]) == `"caller-id"`, "id = %s", frame["id"])
	})
	t.Run("negative number id", func(t *testing.T) {
		frame := b.call(t, "-5", "workspace/current", "")
		testassert.Require(t, string(frame["id"]) == "-5", "id = %s", frame["id"])
	})
}

// TestACPStartupFailureIsUnavailable pins that a failed common startup is the
// separate unavailable code and leaves local methods usable.
func TestACPStartupFailureIsUnavailable(t *testing.T) {
	dataDir := t.TempDir()
	b := startRunner(t, clienthost.Options{
		DataDir:    dataDir,
		ConfigPath: filepath.Join(dataDir, "config.json"),
		Executable: filepath.Join(dataDir, "absent-runtime"),
	}, "/ws", nil)
	{
		code, _, _ := errorFrame(t, b.call(t, "1", "initialize", ""))
		testassert.Equal(t, code, -32001)
	}
	testassert.Equal(t, errorCode(t, b.call(t, "2", "runtime/GetHealth", "")), -32001)
	testassert.Equal(t, resultOf(t, b.call(t, "3", "workspace/current", "")), `"/ws"`)
	b.eof()
	testassert.NoError(t, b.awaitRun(t))
}

// TestACPInitializeRequiresAHealthyStream proves a candidate whose event
// stream fails to establish is reported as the unavailable transport without
// installing a connection, announcing it, reading Session/config state, or
// retrying on its own.
func TestACPInitializeRequiresAHealthyStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events http.HandlerFunc
	}{
		{"a non-200 event route", jsonHandler(http.StatusInternalServerError, `{"code":"internal","message":"broken"}`)},
		{"a transport failure opening the stream", func(w http.ResponseWriter, _ *http.Request) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]http.HandlerFunc{"/v1/events": tc.events})
			b := startBridge(t, f, "/ws")
			testassert.Equal(t, errorCode(t, b.call(t, "1", "initialize", "")), -32001)
			testassert.Equal(t, errorCode(t, b.call(t, "2", "runtime/GetHealth", "")), -32001)
			attempts := f.count("/v1/events")
			testassert.Require(t, attempts != 0, "initialize never attempted the event stream")
			testassert.Equal(t, resultOf(t, b.call(t, "3", "workspace/current", "")), `"/ws"`)
			testassert.Equal(t, f.count("/v1/events"), attempts)
			for _, path := range f.paths() {
				testassert.Require(t, !strings.HasPrefix(path, "/v1/sessions") && !strings.HasPrefix(path, "/v1/configuration"), "a failed initialize performed %s", path)
			}
		})
	}
}

// TestACPOversizedLine pins the existing one-megabyte per-line bound.
func TestACPOversizedLine(t *testing.T) {
	f := newFixture(t, nil)
	b := startBridge(t, f, "/ws")
	go func() {
		_, _ = io.WriteString(b.in, strings.Repeat("x", (1<<20)+1)+"\n")
	}()
	err := b.awaitRun(t)
	testassert.Require(t, err != nil, "an oversized line ended Run without an error")
	testassert.Require(t, strings.Contains(err.Error(), "token too long"), "error = %v, want the scanner's line-bound error", err)
}

// TestACPInitializeBeforeAndAfter pins the initializing gate, the exact
// initialize description, and repeated initialize without a second stream.
func TestACPInitializeBeforeAndAfter(t *testing.T) {
	f := newFixture(t, nil)
	b := startBridge(t, f, "/ws")

	// Local methods and method classification operate before initialization.
	testassert.Equal(t, resultOf(t, b.call(t, "1", "workspace/current", "")), `"/ws"`)
	testassert.Equal(t, resultOf(t, b.call(t, "2", "workspace/select", `{"workspace":"/other"}`)), `"/other"`)
	testassert.Equal(t, errorCode(t, b.call(t, "3", "runtime/GetHealth", "")), -32001)
	testassert.Equal(t, errorCode(t, b.call(t, "4", "session/current", "")), -32001)
	testassert.Equal(t, errorCode(t, b.call(t, "5", "session/select", `{"session_id":"s1"}`)), -32001)
	testassert.Equal(t, errorCode(t, b.call(t, "6", "runtime/Nope", "{}")), -32001)
	testassert.Equal(t, f.total(), 0)

	b.send(t, requestLine("10", "initialize", ""))
	var sawResponse, sawConnected bool
	frames := b.collectFrames(t, func(frame map[string]json.RawMessage) bool {
		if raw, ok := frame["id"]; ok && string(raw) == "10" {
			sawResponse = true
		}
		if raw, ok := frame["method"]; ok && string(raw) == `"runtime/connected"` {
			sawConnected = true
		}
		return sawResponse && sawConnected
	})
	f.awaitEventsOpen(t)
	// The response is retained by identity, never as the incidental
	// last-arriving frame, so either arrival order is legal.
	response := frameWithID(t, frames, "10")
	frameWithMethod(t, frames, "runtime/connected")

	var result struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Runtime json.RawMessage `json:"runtime"`
		Methods []string        `json:"methods"`
	}
	var members map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal([]byte(resultOf(t, response)), &members))
	testassert.Require(t, len(members) == 3, "initialize result members = %v, want exactly serverInfo, runtime, and methods", members)
	testassert.NoError(t, json.Unmarshal([]byte(resultOf(t, response)), &result))
	testassert.Require(t, result.ServerInfo.Name == "lightcode" && result.ServerInfo.Version == version.String() && string(result.Runtime) == healthBody(), "serverInfo = %+v, want the native identity; runtime = %s, want the generated Health", result.ServerInfo, result.Runtime)
	wantMethods := clientops.OperationNames()
	slices.Sort(wantMethods)
	testassert.Require(t, reflect.DeepEqual(result.Methods, wantMethods), "methods = %v, want the sorted supported invocation names", result.Methods)
	if f.count("/v1/events") != 1 {
		t.Fatalf("initialize opened %d event streams, want one", f.count("/v1/events"))
	}
	// The one startup composition created one primary Session in the captured
	// Workspace (the pre-initialize selection); the scripted list handler
	// checked its query and method.
	testassert.Equal(t, f.lastBody(t, "/v1/sessions"), `{"agent_type":"primary","workspace":"/other"}`)

	// A repeated initialize describes the current connection and never
	// composes or creates again.
	before := f.count("/v1/events")
	startupReads := f.count("/v1/sessions")
	again := b.call(t, "11", "initialize", "")
	if resultOf(t, again) != resultOf(t, response) {
		t.Fatalf("repeated initialize = %s, want the current description", resultOf(t, again))
	}
	testassert.Require(t, f.count("/v1/events") == before, "repeated initialize opened a second event stream")
	testassert.Require(t, f.count("/v1/sessions") == startupReads, "a repeated initialize composed again")

	health := b.call(t, "12", "runtime/GetHealth", "")
	if resultOf(t, health) != healthBody() {
		t.Fatalf("runtime/GetHealth = %s, want the exact health document", resultOf(t, health))
	}
	testassert.Equal(t, errorCode(t, b.call(t, "13", "runtime/GetHealth", `{"bogus":true}`)), -32602)
}

// TestACPSelection pins explicit session selection, its Workspace adoption,
// fresh current reads, the null result with no selection, and that malformed
// params and received errors keep their JSON-RPC shapes.
func TestACPSelection(t *testing.T) {
	f := newFixture(t, map[string]http.HandlerFunc{
		"/v1/sessions": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				t.Error("startup created a Session despite existing roots")
			} else if r.Method != http.MethodGet || r.URL.Query().Get("lifecycle") != "open" || r.URL.Query().Get("workspace") == "" {
				t.Errorf("startup list = %s %s, want the selected Workspace's open lifecycle", r.Method, r.URL.RawQuery)
			}
			fmt.Fprint(w, "["+
				acpclient.SessionHeader("s-old", "/ws", "2026-10-01T00:00:00Z")+","+
				acpclient.SessionHeader("s-b", "/ws", "2026-10-02T00:00:00Z")+","+
				`{"session_id":"s-child","workspace":"/ws","agent_type":"primary","lifecycle":"open","parent_session_id":"s-old","created_at":"2026-10-01T00:00:00Z","last_activity":"2026-10-03T00:00:00Z","session_revision":{"durable_revision":"1","local_revision":"0","instance_id":"`+testInstance+`"}}`+","+
				acpclient.SessionHeader("s-a", "/ws", "2026-10-02T00:00:00Z")+"]")
		},
		"/v1/sessions/s-a/hydration": jsonHandler(200, hydrationBody("s-a", "/ws")),
		"/v1/sessions/s1/hydration":  jsonHandler(200, hydrationBody("s1", "/ws/s1")),
		"/v1/sessions/s2/hydration":  jsonHandler(200, hydrationBody("s2", "/ws/s2")),
		"/v1/sessions/s3/hydration":  jsonHandler(404, `{"code":"not_found","message":"no such session"}`),
	})
	b := startBridge(t, f, "/ws")
	b.call(t, "1", "initialize", "")
	f.awaitEventsOpen(t)

	// The startup composition adopted the newest root by last activity and ID;
	// an older root and a newer child header never win.
	{
		result := resultOf(t, b.call(t, "2", "session/current", ""))
		testassert.Require(t, result == hydrationBody("s-a", "/ws"), "startup selection = %s, want the newest root's fresh hydration", result)
	}
	// The local workspace operation clears the selection so the null result is
	// observable.
	testassert.Equal(t, resultOf(t, b.call(t, "3", "workspace/select", `{"workspace":"/ws"}`)), `"/ws"`)
	testassert.Equal(t, resultOf(t, b.call(t, "4", "session/current", "")), "null")
	testassert.Require(t, f.count("/v1/sessions/s1/hydration") == 0, "session/current without a selection performed a hydration read")

	selected := b.call(t, "5", "session/select", `{"session_id":"s1"}`)
	if resultOf(t, selected) != hydrationBody("s1", "/ws/s1") {
		t.Fatalf("session/select = %s, want the exact generated hydration", resultOf(t, selected))
	}
	testassert.Equal(t, resultOf(t, b.call(t, "6", "workspace/current", "")), `"/ws/s1"`)
	b.call(t, "7", "session/current", "")
	if f.count("/v1/sessions/s1/hydration") != 2 {
		t.Fatalf("hydration reads = %d, want the selection read and one fresh current read", f.count("/v1/sessions/s1/hydration"))
	}

	testassert.Equal(t, resultOf(t, b.call(t, "8", "workspace/select", `{"workspace":"/other"}`)), `"/other"`)
	testassert.Equal(t, resultOf(t, b.call(t, "9", "session/current", "")), "null")
	{
		result := resultOf(t, b.call(t, "10", "session/select", `{"session_id":"s2"}`))
		testassert.Require(t, result == hydrationBody("s2", "/ws/s2"), "session/select s2 = %s", result)
	}
	testassert.Equal(t, resultOf(t, b.call(t, "11", "workspace/current", "")), `"/ws/s2"`)

	rejected := b.call(t, "12", "session/select", `{"session_id":"s3"}`)
	code, message, data := errorFrame(t, rejected)
	testassert.Require(t, code == -32000 && message == "no such session", "received error = (%d, %q), want -32000 with the returned message", code, message)
	testassert.Require(t, string(data) == `{"code":"not_found","message":"no such session"}`, "error.data = %s, want the generated Error document", data)

	for _, row := range []struct {
		name   string
		method string
		params string
	}{
		{"session select missing id", "session/select", `{}`},
		{"session select null id", "session/select", `{"session_id":null}`},
		{"session select numeric id", "session/select", `{"session_id":5}`},
		{"session select unknown member", "session/select", `{"session_id":"s1","x":1}`},
		{"workspace select missing workspace", "workspace/select", `{}`},
		{"workspace select null workspace", "workspace/select", `{"workspace":null}`},
		{"workspace select positional array", "workspace/select", `[]`},
		{"session current unknown member", "session/current", `{"x":1}`},
		{"initialize unknown member", "initialize", `{"x":1}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			testassert.Equal(t, errorCode(t, b.call(t, `"`+row.name+`"`, row.method, row.params)), -32602)
		})
	}
}

// TestACPEventsAndRepair pins generated event forwarding, the exact repair
// notifications after a lost stream, and the fresh snapshot of the selected
// Session.
func TestACPEventsAndRepair(t *testing.T) {
	event := sessionChangedEvent(testInstance)
	release := make(chan struct{})
	f := newFixture(t, map[string]http.HandlerFunc{
		"/v1/sessions/s1/hydration": jsonHandler(200, hydrationBody("s1", "/ws/s1")),
	})
	var mu sync.Mutex
	opens := 0
	f.hand("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		mu.Lock()
		opens++
		current := opens
		mu.Unlock()
		flusher.Flush()
		if current == 1 {
			fmt.Fprint(w, sseFrame(event))
			flusher.Flush()
			f.signalEventsOpen()
			<-release // ends the body only after the test holds a selection
			return
		}
		f.signalEventsOpen()
		<-r.Context().Done()
	})
	b := startBridge(t, f, "/ws")

	b.send(t, requestLine("1", "initialize", ""))
	var sawResponse, sawConnected, sawEvent bool
	var eventParams json.RawMessage
	b.awaitFrame(t, func(frame map[string]json.RawMessage) bool {
		if raw, ok := frame["id"]; ok && string(raw) == "1" {
			sawResponse = true
		}
		if raw, ok := frame["method"]; ok && string(raw) == `"runtime/connected"` {
			sawConnected = true
		}
		if raw, ok := frame["method"]; ok && string(raw) == `"runtime/event"` {
			sawEvent = true
			eventParams = frame["params"]
		}
		return sawResponse && sawConnected && sawEvent
	})
	var forwarded, original map[string]any
	testassert.NoError(t, json.Unmarshal(eventParams, &forwarded))
	testassert.NoError(t, json.Unmarshal([]byte(event), &original))
	testassert.Require(t, reflect.DeepEqual(forwarded, original), "forwarded event = %v, want the generated event %v", forwarded, original)

	b.call(t, "2", "session/select", `{"session_id":"s1"}`)
	close(release)

	connected := b.awaitFrame(t, methodFrame("runtime/connected"))
	var health map[string]any
	testassert.NoError(t, json.Unmarshal(connected["params"], &health))
	testassert.Require(t, health["instance_id"] == testInstance && health["protocol_version"] == "5", "runtime/connected params = %v, want the generated Health", health)
	resync := b.awaitFrame(t, methodFrame("runtime/resync_required"))
	{
		params, ok := resync["params"]
		testassert.Require(t, !ok, "runtime/resync_required carries params %s, want none", params)
	}
	snapshot := b.awaitFrame(t, methodFrame("session/snapshot"))
	testassert.Require(t, string(snapshot["params"]) == hydrationBody("s1", "/ws/s1"), "session/snapshot = %s, want the selected Session's fresh hydration", snapshot["params"])
	if f.count("/v1/events") != 2 {
		t.Fatalf("event streams opened = %d, want one repair reconnect", f.count("/v1/events"))
	}
	// The passive repair never composed or created again.
	if f.count("/v1/sessions") != 2 {
		t.Fatalf("a passive repair composed again: %d /v1/sessions reads", f.count("/v1/sessions"))
	}
}

// TestACPNoParameterParams pins the shared local no-parameter binding: absent
// params and {} are accepted for every accepting method, and a supplied null
// is invalid params like every other non-object form.
func TestACPNoParameterParams(t *testing.T) {
	f := newFixture(t, nil)
	b := startBridge(t, f, "/ws")

	// initialize rejects a supplied null before any connection is made.
	testassert.Equal(t, errorCode(t, b.call(t, "1", "initialize", "null")), -32602)
	if f.total() != 0 {
		t.Fatalf("a rejected initialize started %d Runtime requests", f.total())
	}
	// initialize accepts the empty object.
	initialized := b.call(t, "2", "initialize", "{}")
	{
		_, ok := initialized["result"]
		testassert.Require(t, ok, "initialize with {} = %s, want the connection description", initialized)
	}
	// A repeated initialize accepts absent params.
	testassert.Require(t, resultOf(t, b.call(t, "3", "initialize", "")) == resultOf(t, initialized), "initialize with absent params did not describe the connection")

	// workspace/current accepts absent and {}, and rejects null.
	testassert.Equal(t, resultOf(t, b.call(t, "4", "workspace/current", "")), `"/ws"`)
	testassert.Equal(t, resultOf(t, b.call(t, "5", "workspace/current", "{}")), `"/ws"`)
	testassert.Equal(t, errorCode(t, b.call(t, "6", "workspace/current", "null")), -32602)

	// With the startup selection cleared, session/current accepts absent and
	// {}, and rejects null.
	b.call(t, "61", "workspace/select", `{"workspace":"/ws"}`)
	testassert.Equal(t, resultOf(t, b.call(t, "7", "session/current", "")), "null")
	testassert.Equal(t, resultOf(t, b.call(t, "8", "session/current", "{}")), "null")
	testassert.Equal(t, errorCode(t, b.call(t, "9", "session/current", "null")), -32602)
}

// TestACPProgressOverflowDisconnectsAndResyncs pins that a full progress
// queue disconnects only this client's stream and records a resync.
func TestACPProgressOverflowDisconnectsAndResyncs(t *testing.T) {
	f := newFixture(t, nil)
	var mu sync.Mutex
	opens := 0
	f.hand("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		mu.Lock()
		opens++
		current := opens
		mu.Unlock()
		flusher.Flush()
		if current != 1 {
			f.signalEventsOpen()
			<-r.Context().Done()
			return
		}
		f.signalEventsOpen()
		for i := 0; i < 300; i++ {
			fmt.Fprint(w, sseFrame(sessionChangedEvent(testInstance)))
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	writer := newGatedWriter()
	b := startBridgeWith(t, f, "/ws", writer)
	b.send(t, requestLine("1", "initialize", ""))
	f.awaitEventsOpen(t)
	// The writer is parked, so the progress queue fills and the stream is
	// retired; the repair reconnect proves the overflow was recorded.
	f.awaitEventsOpen(t)
	writer.release()
	writer.awaitContains(t, `"runtime/resync_required"`)
	if f.count("/v1/events") != 2 {
		t.Fatalf("event streams = %d, want exactly one repair reconnect", f.count("/v1/events"))
	}
}

// Input pressure preserves admission order. Observed EOF and independent
// cancellation end only client resources, without owner cleanup.
func TestACPInputPressureCancelsParkedCalls(t *testing.T) {
	newPressureFixture := func(t *testing.T) (*fixture, chan struct{}, chan struct{}, chan struct{}) {
		t.Helper()
		arrived := make(chan struct{})
		released := make(chan struct{})
		canceled := make(chan struct{})
		var arriveOnce, cancelOnce sync.Once
		f := newFixture(t, map[string]http.HandlerFunc{
			"/v1/sessions/s1/stop": func(w http.ResponseWriter, r *http.Request) {
				arriveOnce.Do(func() { close(arrived) })
				select {
				case <-released:
				case <-r.Context().Done():
					cancelOnce.Do(func() { close(canceled) })
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
			},
		})
		return f, arrived, released, canceled
	}

	t.Run("EOF ends the bridge with one line still pending", func(t *testing.T) {
		f, arrived, _, canceled := newPressureFixture(t)
		writer := newGatedWriter()
		b := startBridgeWith(t, f, "/ws", writer)
		b.send(t, requestLine("1", "initialize", ""))
		f.awaitEventsOpen(t)

		b.send(t, requestLine("2", "runtime/StopSession", `{"session_id":"s1"}`))
		select {
		case <-arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("the parked Stop call never arrived")
		}
		b.send(t, requestLine("3", "runtime/GetHealth", ""))
		healthBefore := f.count("/v1/health")
		b.eof()
		select {
		case <-canceled:
		case <-time.After(15 * time.Second):
			t.Fatal("EOF did not cancel the parked Runtime call")
		}
		writer.release()
		testassert.NoError(t, b.awaitRun(t))
		testassert.Equal(t, f.count("/v1/health"), healthBefore)
		testassert.Require(t, f.count("/v1/sessions/s1/stop") == 1, "the bridge issued an implicit control beyond the explicit call")
	})

	t.Run("EOF behind a capacity-blocked line is observed in order", func(t *testing.T) {
		f, arrived, released, _ := newPressureFixture(t)
		writer := newGatedWriter()
		b := startBridgeWith(t, f, "/ws", writer)
		defer b.cancel()
		b.send(t, requestLine("1", "initialize", ""))
		f.awaitEventsOpen(t)

		b.send(t, requestLine("2", "runtime/StopSession", `{"session_id":"s1"}`))
		select {
		case <-arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("the parked Stop call never arrived")
		}
		// The single pending slot is filled while two further valid lines wait in
		// the scanner and the input is then closed: the EOF cannot be observed
		// before those tokens were admitted, so the queued line still progresses
		// first and only the lines behind it may be released by the teardown.
		before := f.total()
		healthBefore := f.count("/v1/health")
		b.send(t, strings.Join([]string{
			requestLine("3", "runtime/GetHealth", ""),
			requestLine("4", "runtime/GetWarnings", ""),
			requestLine("5", "runtime/GetHealth", ""),
		}, "\n"))
		b.eof()
		close(released)
		writer.release()
		deadline := time.Now().Add(15 * time.Second)
		for f.count("/v1/health") < healthBefore+1 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := f.count("/v1/health"); got < healthBefore+1 || got > healthBefore+2 {
			t.Fatalf("the line admitted before EOF never progressed: health requests %d -> %d, calls=%v", healthBefore, got, f.paths())
		}
		// The ordered EOF then ends the run normally, never as an overflow.
		testassert.NoError(t, b.awaitRun(t))
		// Every line that reached dispatch kept its admission order; the racy
		// tail behind the observed EOF may be released, never reordered.
		want := []string{"/v1/health", "/v1/warnings", "/v1/health"}
		for i, path := range f.paths()[before:] {
			if i >= len(want) || path != want[i] {
				t.Fatalf("a line was reordered or skipped behind EOF: %v", f.paths())
			}
		}
	})

	t.Run("context cancellation releases a capacity-blocked scanner without dispatch or output", func(t *testing.T) {
		f, arrived, _, canceled := newPressureFixture(t)
		writer := newGatedWriter()
		b := startBridgeWith(t, f, "/ws", writer)
		b.send(t, requestLine("1", "initialize", ""))
		f.awaitEventsOpen(t)

		b.send(t, requestLine("2", "runtime/StopSession", `{"session_id":"s1"}`))
		select {
		case <-arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("the parked Stop call never arrived")
		}
		before := f.total()
		b.send(t, requestLine("3", "runtime/GetHealth", ""))
		b.send(t, requestLine("4", "runtime/GetHealth", ""))
		// Cancellation releases the blocked producer and cancels the parked
		// Runtime call while dispatch and output capacity stay held.
		b.cancel()
		select {
		case <-canceled:
		case <-time.After(15 * time.Second):
			t.Fatal("out-of-band cancellation did not cancel the parked Runtime call")
		}
		{
			err := b.awaitRun(t)
			testassert.Require(t, err != nil, "Run returned nil after its context was canceled")
		}
		writer.release()
		testassert.Equal(t, f.total(), before)
	})

	t.Run("an uninterruptible output write stays bounded", func(t *testing.T) {
		f, _, _, _ := newPressureFixture(t)
		writer := newGatedWriter()
		b := startBridgeWith(t, f, "/ws", writer)
		b.send(t, requestLine("1", "initialize", ""))
		f.awaitEventsOpen(t)
		writer.awaitBlocked(t)
		b.eof()
		start := time.Now()
		testassert.NoError(t, b.awaitRun(t))
		{
			elapsed := time.Since(start)
			testassert.Require(t, elapsed <= 10*time.Second, "Run waited %s for the parked writer, want the bounded join", elapsed)
		}
	})
}

// failingWriter fails its output after the configured number of writes, or
// short-writes from there on.
type failingWriter struct {
	mu     sync.Mutex
	writes int
	failOn int
	err    error
	short  bool
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.writes < w.failOn {
		return len(p), nil
	}
	if w.short {
		return len(p) - 1, nil
	}
	return 0, w.err
}

// TestACPFailedWriterEndsRun proves an output failure is Run's own result:
// the first failed or short write cancels parked calls and output admission
// and ends Run, while stdin stays open and no Runtime control is issued.
func TestACPFailedWriterEndsRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		short bool
	}{
		{"write error", false},
		{"short write", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrived := make(chan struct{})
			canceled := make(chan struct{})
			emitEvent := make(chan struct{})
			var arriveOnce, cancelOnce sync.Once
			var f *fixture
			f = newFixture(t, map[string]http.HandlerFunc{
				"/v1/events": func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					flusher, _ := w.(http.Flusher)
					flusher.Flush()
					f.signalEventsOpen()
					select {
					case <-emitEvent:
						fmt.Fprint(w, sseFrame(sessionChangedEvent(testInstance)))
						flusher.Flush()
					case <-r.Context().Done():
						return
					}
					<-r.Context().Done()
				},
				"/v1/sessions/s1/stop": func(w http.ResponseWriter, r *http.Request) {
					arriveOnce.Do(func() { close(arrived) })
					<-r.Context().Done()
					cancelOnce.Do(func() { close(canceled) })
				},
			})
			outErr := errors.New("output device failed")
			writer := &failingWriter{failOn: 3, err: outErr, short: tc.short}
			b := startBridgeWith(t, f, "/ws", writer)
			b.send(t, requestLine("1", "initialize", ""))
			f.awaitEventsOpen(t)
			b.send(t, requestLine("2", "runtime/StopSession", `{"session_id":"s1"}`))
			select {
			case <-arrived:
			case <-time.After(15 * time.Second):
				t.Fatal("the parked Stop call never arrived")
			}
			healthBefore := f.count("/v1/health")
			close(emitEvent)
			select {
			case <-canceled:
			case <-time.After(15 * time.Second):
				t.Fatal("the output failure did not cancel the parked Runtime call")
			}
			wantErr := outErr
			if tc.short {
				wantErr = io.ErrShortWrite
			}
			{
				err := b.awaitRun(t)
				testassert.Require(t, errors.Is(err, wantErr), "Run = %v, want %v", err, wantErr)
			}
			testassert.Equal(t, f.count("/v1/health"), healthBefore)
			testassert.Require(t, f.count("/v1/sessions/s1/stop") == 1, "the output failure issued an implicit Runtime control")
		})
	}
}

// TestACPRuntimeForwarding pins exact caller-supplied body forwarding, raw
// result return, error.data for the declared Runtime error and partial
// result, and the separate transport family for malformed delivery.
func TestACPRuntimeForwarding(t *testing.T) {
	f := newFixture(t, map[string]http.HandlerFunc{
		"/v1/sessions": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				fmt.Fprint(w, "["+acpclient.SessionHeader("s1", "/ws", "2026-10-01T00:00:00Z")+"]")
				return
			}
			w.WriteHeader(201)
			fmt.Fprint(w, `{"session_id":"s1","durable_revision":"9007199254740993"}`)
		},
		"/v1/sessions/s1/revert-code": jsonHandler(409, `{"error":{"code":"conflict","message":"partial restore"},"restored":["a.go"],"skipped":[{"path":"b.go","reason":"denied"}]}`),
		"/v1/sessions/s1/hydration":   jsonHandler(500, "<html>broken</html>"),
		"/v1/providers/discover":      jsonHandler(200, `[]`),
	})
	b := startBridge(t, f, "/ws")
	b.call(t, "1", "initialize", "")

	body := `{"workspace":"/ws","agent_type":"integrated","opaque":9007199254740993}`
	created := b.call(t, "2", "runtime/CreateSession", `{"body":`+body+`}`)
	if resultOf(t, created) != `{"session_id":"s1","durable_revision":"9007199254740993"}` {
		t.Fatalf("CreateSession result = %s, want the exact raw body", resultOf(t, created))
	}
	testassert.Equal(t, f.lastBody(t, "/v1/sessions"), body)
	testassert.Require(t, resultOf(t, b.call(t, "3", "runtime/DiscoverProviderCandidates", `{"body":{"base_url":"x","api_key_env":"K"}}`)) == "[]", "DiscoverProviderCandidates result did not pass through raw")

	reverted := b.call(t, "4", "runtime/RevertSessionCode", `{"session_id":"s1","body":{"operation_id":"op"}}`)
	code, message, data := errorFrame(t, reverted)
	testassert.Require(t, code == -32000 && message == "partial restore", "partial error = (%d, %q)", code, message)
	var gotPartial, wantPartial any
	{
		err := json.Unmarshal(data, &gotPartial)
		testassert.Require(t, err == nil, "partial error.data %s: %v", data, err)
	}
	testassert.NoError(t, json.Unmarshal([]byte(`{"error":{"code":"conflict","message":"partial restore"},"restored":["a.go"],"skipped":[{"path":"b.go","reason":"denied"}]}`), &wantPartial))
	testassert.Require(t, reflect.DeepEqual(gotPartial, wantPartial), "error.data = %s, want the complete generated result", data)

	malformed := b.call(t, "5", "runtime/GetSessionHydration", `{"session_id":"s1"}`)
	{
		code, _, _ := errorFrame(t, malformed)
		testassert.Equal(t, code, -32001)
	}

	unknownBefore := f.total()
	testassert.Equal(t, errorCode(t, b.call(t, "6", "runtime/NoSuchOperation", `{}`)), -32602)
	testassert.Require(t, f.total() == unknownBefore, "an unknown operation reached the Runtime")
	testassert.Equal(t, errorCode(t, b.call(t, "7", "runtime/CreateSession", `{"body":[]}`)), -32602)
	testassert.Equal(t, errorCode(t, b.call(t, "8", "runtime/GetHealth", `{"query":{}}`)), -32602)
	healthBefore := f.count("/v1/health")
	testassert.Equal(t, errorCode(t, b.call(t, "9", "runtime/GetHealth", "null")), -32602)
	testassert.Require(t, f.count("/v1/health") == healthBefore, "a rejected null envelope reached the Runtime")
}

// A composition failure ends that attempt through the RPC error mapping and
// leaves the established transport available.
func TestACPStartupCompositionFailureLeavesTransportAvailable(t *testing.T) {
	f := newFixture(t, map[string]http.HandlerFunc{
		"/v1/sessions": jsonHandler(500, `{"code":"internal","message":"list failed"}`),
	})
	b := startBridge(t, f, "/ws")
	code, message, _ := errorFrame(t, b.call(t, "1", "initialize", ""))
	testassert.Require(t, code == -32000 && message == "list failed", "composition failure = (%d, %q), want the received Runtime error mapping", code, message)
	{
		result := resultOf(t, b.call(t, "2", "runtime/GetHealth", ""))
		testassert.Require(t, result == healthBody(), "runtime/GetHealth after a composition failure = %s", result)
	}
}
