package acpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// The in-package frame fixtures drive the real Runner helpers and queues:
// establishment, repair, selection adoption, and the sole writer over a
// recording writer whose first write can be parked so queued frames stay
// queued while the test mutates runner state.

const (
	frameInstance   = "0123456789abcdef0123456789abcdef"
	frameInstance2  = "fedcba9876543210fedcba9876543210"
	frameCredential = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

type frameFixture struct {
	server        *httptest.Server
	instance      string
	parkHydration bool

	mu               sync.Mutex
	opens            int
	healthCount      int
	parkHealthAfter  int
	eventsFailure    string
	markers          map[int]chan string
	ends             map[int]chan struct{}
	requests         []string
	arriveOnce       sync.Once
	healthOnce       sync.Once
	releaseOnce      sync.Once
	hydrationArrived chan struct{}
	releaseHydration chan struct{}
	healthArrived    chan struct{}
	releaseHealth    chan struct{}
	opensCh          chan struct{}
}

// SessionHeader is one generated Session header document for the startup
// fixtures.
func SessionHeader(id, workspace, lastActivity string) string {
	return fmt.Sprintf(`{"session_id":%q,"workspace":%q,"agent_type":"primary","lifecycle":"open","created_at":"2026-10-01T00:00:00Z","last_activity":%q,"session_revision":{"durable_revision":"1","local_revision":"0","instance_id":%q}}`, id, workspace, lastActivity, frameInstance)
}

func newFrameFixture(t *testing.T, instance string, parkHydration bool) *frameFixture {
	t.Helper()
	f := &frameFixture{
		instance:         instance,
		parkHydration:    parkHydration,
		markers:          map[int]chan string{},
		ends:             map[int]chan struct{}{},
		hydrationArrived: make(chan struct{}),
		releaseHydration: make(chan struct{}),
		healthArrived:    make(chan struct{}),
		releaseHealth:    make(chan struct{}),
		opensCh:          make(chan struct{}, 8),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		f.mu.Lock()
		f.healthCount++
		count := f.healthCount
		f.mu.Unlock()
		if f.parkHealthAfter > 0 && count > f.parkHealthAfter {
			f.healthOnce.Do(func() { close(f.healthArrived) })
			select {
			case <-f.releaseHealth:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"instance_id":%q,"protocol_version":"3"}`, f.instance)
	})
	mux.HandleFunc("/v1/events", f.serveEvents)
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, SessionHeader("s-startup", "/ws", "2026-10-01T00:00:00Z"))
			return
		}
		fmt.Fprint(w, "[]")
	})
	mux.HandleFunc("/v1/sessions/s1/hydration", f.hydration("s1", "/ws/s1"))
	mux.HandleFunc("/v1/sessions/s2/hydration", f.hydration("s2", "/ws/s2"))
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// note records one request path so a test can prove cancellation issued no
// owner control.
func (f *frameFixture) note(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path)
}

// release unblocks every parked fixture gate once, so a failing test still
// lets its handlers return.
func (f *frameFixture) release() {
	f.releaseOnce.Do(func() {
		close(f.releaseHydration)
		close(f.releaseHealth)
	})
}

// paths returns every recorded request path.
func (f *frameFixture) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *frameFixture) serveEvents(w http.ResponseWriter, r *http.Request) {
	f.note(r)
	f.awaitableEventsOpen()
	if f.failureMode() == "transport" {
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	if f.failureMode() == "status" {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// The stream identity is taken before the flush: GetEvents returns to the
	// client on the flush, so the numbering by which the tests address markers
	// would otherwise race the next request's handler.
	f.mu.Lock()
	f.opens++
	open := f.opens
	gate := f.markers[open]
	if gate == nil {
		gate = make(chan string, 1)
		f.markers[open] = gate
	}
	end := f.ends[open]
	if end == nil {
		end = make(chan struct{})
		f.ends[open] = end
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if gate != nil || end != nil {
		select {
		case document := <-gate:
			fmt.Fprint(w, "event: notification\ndata: "+document+"\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case <-end:
			return
		case <-r.Context().Done():
			return
		}
	}
	<-r.Context().Done()
}

// awaitableEventsOpen records one event-stream request of any outcome.
func (f *frameFixture) awaitableEventsOpen() {
	select {
	case f.opensCh <- struct{}{}:
	default:
	}
}

// awaitEventsOpen waits for one event-stream request.
func (f *frameFixture) awaitEventsOpen(t *testing.T) {
	t.Helper()
	select {
	case <-f.opensCh:
	case <-time.After(15 * time.Second):
		t.Fatal("no event stream was opened")
	}
}

// setEventsFailure sets the event route's failure mode for later connections.
func (f *frameFixture) setEventsFailure(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventsFailure = mode
}

func (f *frameFixture) failureMode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.eventsFailure
}

// end returns the gate whose closure ends the fixture's nth connection.
func (f *frameFixture) end(open int) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ends[open] == nil {
		f.ends[open] = make(chan struct{})
	}
	return f.ends[open]
}

func (f *frameFixture) countPath(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, request := range f.requests {
		if request == path {
			count++
		}
	}
	return count
}

func (f *frameFixture) hydration(sessionID, workspace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		if f.parkHydration {
			f.arriveOnce.Do(func() { close(f.hydrationArrived) })
			select {
			case <-f.releaseHydration:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"session":{"session_id":%q,"workspace":%q}}`, sessionID, workspace)
	}
}

// marker returns the event gate of the fixture's nth connection: its stream
// writes the one document the test sends.
func (f *frameFixture) marker(open int) chan string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markers[open] == nil {
		f.markers[open] = make(chan string, 1)
	}
	return f.markers[open]
}

// writeRecord publishes this fixture's live discovery record.
func (f *frameFixture) writeRecord(t *testing.T, dataDir string) {
	t.Helper()
	record := fmt.Sprintf(`{"endpoint":%q,"instance_id":%q,"protocol_version":"3","credential":%q}`,
		f.server.URL, f.instance, frameCredential)
	testassert.NoError(t, os.WriteFile(filepath.Join(dataDir, "runtime.json"), []byte(record), 0o600))
}

// newFrameRunner builds one Runner over this fixture's discovery record.
func newFrameRunner(t *testing.T, f *frameFixture) (*Runner, string) {
	t.Helper()
	dataDir := t.TempDir()
	f.writeRecord(t, dataDir)
	return New(Options{
		Startup: clienthost.Options{
			DataDir:    dataDir,
			ConfigPath: filepath.Join(dataDir, "config.json"),
			Executable: filepath.Join(dataDir, "absent-runtime"),
		},
		Workspace: "/ws",
	}), dataDir
}

// eventDocument builds one valid session_changed event whose durable revision
// identifies its frame in the output.
func eventDocument(instance, revision string) string {
	return fmt.Sprintf(`{"kind":"session_changed","scope":{"kind":"session","session_id":%q},"session_revision":{"durable_revision":%q,"local_revision":"0","instance_id":%q}}`,
		instance, revision, instance)
}

// frameWriter records every written line and can park every write until
// released, so queued frames stay queued while the test mutates state.
type frameWriter struct {
	mu          sync.Mutex
	lines       []string
	gate        chan struct{}
	blocked     chan struct{}
	written     chan struct{}
	releaseOnce sync.Once
}

func newFrameWriter(parked bool) *frameWriter {
	w := &frameWriter{blocked: make(chan struct{}, 1), written: make(chan struct{}, 1)}
	if parked {
		w.gate = make(chan struct{})
	}
	return w
}

func (w *frameWriter) Write(p []byte) (int, error) {
	if w.gate != nil {
		select {
		case w.blocked <- struct{}{}:
		default:
		}
		<-w.gate
	}
	w.mu.Lock()
	w.lines = append(w.lines, strings.TrimSuffix(string(p), "\n"))
	w.mu.Unlock()
	select {
	case w.written <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *frameWriter) release() {
	if w.gate != nil {
		w.releaseOnce.Do(func() { close(w.gate) })
	}
}

func (w *frameWriter) awaitBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-w.blocked:
	case <-time.After(15 * time.Second):
		t.Fatal("no output write was parked")
	}
}

func (w *frameWriter) awaitContains(t *testing.T, fragment string) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for !w.contains(fragment) {
		select {
		case <-w.written:
		case <-deadline:
			t.Fatalf("output = %v, want %s", w.frames(), fragment)
		}
	}
}

func (w *frameWriter) contains(fragment string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range w.lines {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func (w *frameWriter) count(fragment string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, line := range w.lines {
		if strings.Contains(line, fragment) {
			n++
		}
	}
	return n
}

func (w *frameWriter) frames() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.lines...)
}

// startParkedWriter starts the real writer over a parked recording writer and
// parks its first write on one addressed response, so every later frame stays
// queued until the test releases the writer.
func startParkedWriter(t *testing.T, r *Runner, ctx context.Context) *frameWriter {
	t.Helper()
	writer := newFrameWriter(true)
	r.out = writer
	go r.writeLoop(ctx)
	r.send(response{JSONRPC: jsonRPCVersion, ID: json.RawMessage("0"), Result: json.RawMessage("null")})
	writer.awaitBlocked(t)
	return writer
}

// TestNotificationFramesFollowTheCurrentConnection proves a queued
// notification served by a replaced connection is never written, whether the
// replacement authenticates the same instance or a new one.
func TestNotificationFramesFollowTheCurrentConnection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		newInstance bool
	}{
		{"the replacement authenticates the same instance", false},
		{"the replacement authenticates a new instance", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrameFixture(t, frameInstance, false)
			r, dataDir := newFrameRunner(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.ctx = ctx
			writer := startParkedWriter(t, r, ctx)

			first, err := r.establish(ctx)
			testassert.NoError(t, err)
			r.retire(first)
			r.pumps.Wait()
			testassert.Require(t, r.currentConnection() == nil, "the retired connection generation repaired itself")
			markerFixture := f
			markerOpen := 2
			if tc.newInstance {
				replacement := newFrameFixture(t, frameInstance2, false)
				replacement.writeRecord(t, dataDir)
				markerFixture = replacement
				markerOpen = 1
			}
			second, err := r.establish(ctx)
			testassert.NoError(t, err)
			testassert.Require(t, second != first, "the replacement connection is the retired one")
			markerFixture.marker(markerOpen) <- eventDocument(markerFixture.instance, "7")
			writer.release()
			writer.awaitContains(t, `"durable_revision":"7"`)

			if got := writer.count(`"method":"runtime/connected"`); got != 1 {
				t.Fatalf("connected notifications = %d, want only the current connection's: %v", got, writer.frames())
			}
			if !writer.contains(`"instance_id":"` + markerFixture.instance + `"`) {
				t.Fatalf("output = %v, want the current instance's frames", writer.frames())
			}
			if tc.newInstance && writer.contains(`"instance_id":"`+frameInstance+`"`) {
				t.Fatalf("a retired connection's frame was written: %v", writer.frames())
			}
		})
	}
}

// TestRepairSnapshotFramesFollowTheCurrentSelection proves the repair snapshot
// is stamped at queue time and decided at write time: a selection that moved
// after the frame was queued suppresses it, the still-current selection is
// written, and a stale snapshot queued before a reconnect never precedes the
// new one.
func TestRepairSnapshotFramesFollowTheCurrentSelection(t *testing.T) {
	setup := func(t *testing.T) (*Runner, *frameFixture, *frameWriter, context.Context, context.CancelFunc) {
		t.Helper()
		f := newFrameFixture(t, frameInstance, false)
		r, _ := newFrameRunner(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		r.ctx = ctx
		writer := startParkedWriter(t, r, ctx)
		return r, f, writer, ctx, cancel
	}

	t.Run("a selection that moves after queueing suppresses the snapshot", func(t *testing.T) {
		r, f, writer, ctx, cancel := setup(t)
		defer cancel()
		first, err := r.establish(ctx)
		testassert.NoError(t, err)
		r.selection = selection{id: "s1", generation: 1}
		r.repair(first)
		testassert.Require(t, r.currentConnection() != nil, "repair left no connection")
		{
			_, rpcErr := r.workspaceSelect(json.RawMessage(`{"workspace":"/other"}`))
			testassert.Require(t, rpcErr == nil, "workspace/select: %v", rpcErr)
		}
		f.marker(2) <- eventDocument(f.instance, "7")
		writer.release()
		writer.awaitContains(t, `"durable_revision":"7"`)
		if got := writer.count(`"session/snapshot"`); got != 0 {
			t.Fatalf("superseded snapshots written = %d, want none: %v", got, writer.frames())
		}
	})

	t.Run("the still-current selection is written", func(t *testing.T) {
		r, f, writer, ctx, cancel := setup(t)
		defer cancel()
		first, err := r.establish(ctx)
		testassert.NoError(t, err)
		r.selection = selection{id: "s1", generation: 1}
		r.repair(first)
		f.marker(2) <- eventDocument(f.instance, "7")
		writer.release()
		writer.awaitContains(t, `"durable_revision":"7"`)
		if got := writer.count(`"session/snapshot"`); got != 1 {
			t.Fatalf("snapshots written = %d, want the current selection's only: %v", got, writer.frames())
		}
		if !writer.contains(`"workspace":"/ws/s1"`) {
			t.Fatalf("output = %v, want the fresh hydration", writer.frames())
		}
	})

	t.Run("a stale snapshot never precedes the reconnected one", func(t *testing.T) {
		r, f, writer, ctx, cancel := setup(t)
		defer cancel()
		first, err := r.establish(ctx)
		testassert.NoError(t, err)
		r.selection = selection{id: "s1", generation: 1}
		r.repair(first)
		second := r.currentConnection()
		testassert.Require(t, second != nil, "the first repair left no connection")
		r.selection = selection{id: "s2", generation: 2}
		r.repair(second)
		f.marker(3) <- eventDocument(f.instance, "7")
		writer.release()
		writer.awaitContains(t, `"durable_revision":"7"`)
		if got := writer.count(`"session/snapshot"`); got != 1 {
			t.Fatalf("snapshots written = %d, want only the reconnected current one: %v", got, writer.frames())
		}
		if writer.contains(`"workspace":"/ws/s1"`) {
			t.Fatalf("a stale snapshot was written: %v", writer.frames())
		}
		if !writer.contains(`"workspace":"/ws/s2"`) {
			t.Fatalf("output = %v, want the new selection's snapshot", writer.frames())
		}
	})
}

// TestRetiredConnectionCancelsItsInFlightCall proves a genuinely in-flight
// call of a retired generation is cancelled with its real transport error,
// delivered under the original request ID on both the hydration and runtime
// boundaries, and that the cancellation issues no owner control.
func TestRetiredConnectionCancelsItsInFlightCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		line   string
		id     string
		parked func(*frameFixture) <-chan struct{}
		setup  func(*frameFixture)
	}{
		{
			name:   "an addressed selection read",
			line:   `{"jsonrpc":"2.0","id":7,"method":"session/select","params":{"session_id":"s1"}}`,
			id:     "7",
			parked: func(f *frameFixture) <-chan struct{} { return f.hydrationArrived },
		},
		{
			name:   "an addressed runtime call",
			line:   `{"jsonrpc":"2.0","id":8,"method":"runtime/GetHealth"}`,
			id:     "8",
			parked: func(f *frameFixture) <-chan struct{} { return f.healthArrived },
			setup:  func(f *frameFixture) { f.parkHealthAfter = 2 },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrameFixture(t, frameInstance, true)
			defer f.release()
			if tc.setup != nil {
				tc.setup(f)
			}
			r, _ := newFrameRunner(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.ctx = ctx
			conn, err := r.establish(ctx)
			testassert.NoError(t, err)

			handled := make(chan struct{})
			go func() {
				defer close(handled)
				r.handleLine(ctx, []byte(tc.line))
			}()
			select {
			case <-tc.parked(f):
			case <-time.After(15 * time.Second):
				t.Fatal("the addressed call never reached its transport")
			}
			r.retire(conn)
			select {
			case <-handled:
			case <-time.After(15 * time.Second):
				t.Fatal("the retired connection's call never completed")
			}

			var frame response
			select {
			case data := <-r.responses:
				testassert.NoError(t, json.Unmarshal(data, &frame))
			case <-time.After(15 * time.Second):
				t.Fatal("the canceled call produced no response")
			}
			testassert.Require(t, string(frame.ID) == tc.id && frame.Error != nil, "response ID = %s, want the original request ID %s; frame = %+v, want the canceled call's transport error", frame.ID, tc.id, frame)
			testassert.Require(t, frame.Error.Code == codeTransport && strings.Contains(frame.Error.Message, "context canceled") && len(frame.Result) == 0, "got %v, want %v; error message = %q, want the real transport cancellation; frame carries a result beside the error: %s", frame.Error.Code, codeTransport, frame.Error.Message, frame.Result)
			{
				got := r.selectionSnapshot()
				testassert.Require(t, got.id == "", "selection = %+v, want no adoption from a cancelled call", got)
			}
			for _, path := range f.paths() {
				testassert.Require(t, !strings.Contains(path, "stop") && !strings.Contains(path, "interrupt") && !strings.Contains(path, "shutdown"), "cancellation issued owner control %s", path)
			}
		})
	}
}

// TestCompletedResponseSurvivesRetirement proves a response fully received
// before retirement is still written as its addressed success, while the
// actual local adoption helper rejects the retired connection and adopts the
// current one.
func TestCompletedResponseSurvivesRetirement(t *testing.T) {
	f := newFrameFixture(t, frameInstance, false)
	r, _ := newFrameRunner(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ctx = ctx
	writer := newFrameWriter(false)
	r.out = writer
	go r.writeLoop(ctx)
	conn, err := r.establish(ctx)
	testassert.NoError(t, err)

	handled := make(chan struct{})
	go func() {
		defer close(handled)
		r.handleLine(ctx, []byte(`{"jsonrpc":"2.0","id":9,"method":"session/select","params":{"session_id":"s1"}}`))
	}()
	// The addressed success is written before retirement.
	writer.awaitContains(t, `"id":9`)
	if !writer.contains(`"workspace":"/ws/s1"`) {
		t.Fatalf("output = %v, want the completed response's actual body", writer.frames())
	}
	select {
	case <-handled:
	case <-time.After(15 * time.Second):
		t.Fatal("the completed addressed call never returned")
	}
	stamp := r.selectionSnapshot()
	testassert.Require(t, stamp.id == "s1", "selection = %+v, want the completed request's adoption", stamp)

	// Retirement after the completed response: the local adoption rejects the
	// retired connection and adopts the current one.
	r.retire(conn)
	replacement, err := r.establish(ctx)
	testassert.NoError(t, err)
	r.adoptSelection(conn, stamp, "s2", "/ws/s2")
	testassert.Equal(t, r.selectionSnapshot(), stamp)
	r.adoptSelection(replacement, stamp, "s2", "/ws/s2")
	{
		got := r.selectionSnapshot()
		testassert.Require(t, got.id == "s2", "selection = %+v, want the current connection's adoption", got)
	}
}

// TestSelectionAdoptionFollowsTheCurrentSelection proves the sibling: exactly
// one selection read is adopted when its connection and selection stamp are
// still current.
func TestSelectionAdoptionFollowsTheCurrentSelection(t *testing.T) {
	f := newFrameFixture(t, frameInstance, false)
	r, _ := newFrameRunner(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ctx = ctx
	{
		_, err := r.establish(ctx)
		testassert.NoError(t, err)
	}
	result, rpcErr := r.sessionSelect(ctx, json.RawMessage(`{"session_id":"s1"}`))
	testassert.Require(t, rpcErr == nil, "session/select = %+v", rpcErr)
	testassert.Require(t, strings.Contains(string(result), `"workspace":"/ws/s1"`), "session/select result = %s", result)
	{
		got := r.selectionSnapshot()
		testassert.Require(t, got.id == "s1", "selection = %+v, want the adopted explicit session", got)
	}
}

// TestEstablishInstallsOnlyAHealthyStream proves the one readiness producer:
// a candidate whose event stream already reported a transport or non-200
// failure is never installed, announced, or read from, and no automatic retry
// follows.
func TestEstablishInstallsOnlyAHealthyStream(t *testing.T) {
	for _, tc := range []struct{ name, failure string }{
		{"a non-200 event route", "status"},
		{"a transport failure opening the stream", "transport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrameFixture(t, frameInstance, false)
			f.setEventsFailure(tc.failure)
			r, _ := newFrameRunner(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.ctx = ctx

			{
				_, rpcErr := r.initialize(ctx, nil)
				testassert.Require(t, rpcErr != nil && rpcErr.Code == codeTransport, "initialize = %+v, want the unavailable transport error", rpcErr)
			}
			attempts := f.countPath("/v1/events")
			testassert.Require(t, attempts != 0, "the failed establish never attempted the event stream")
			r.pumps.Wait()
			testassert.Require(t, r.currentConnection() == nil, "a known failed stream was installed as healthy")
			testassert.Require(t, !r.initialized, "a failed stream marked the bridge initialized")
			{
				frame := drainEventFrames(r)
				testassert.Require(t, len(frame) == 0, "a failed stream published %v", frame)
			}
			testassert.Equal(t, f.countPath("/v1/events"), attempts)
			for _, path := range f.paths() {
				testassert.Require(t, !strings.HasPrefix(path, "/v1/sessions") && !strings.HasPrefix(path, "/v1/configuration"), "a failed stream performed %s", path)
			}
		})
	}
}

// TestEstablishOpensTheStreamBeforeTheDescriptiveRead pins the readiness
// order: the one subscription is established before the descriptive Health
// read, and a repeated initialize adds no stream.
func TestEstablishOpensTheStreamBeforeTheDescriptiveRead(t *testing.T) {
	f := newFrameFixture(t, frameInstance, false)
	r, _ := newFrameRunner(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ctx = ctx
	{
		_, err := r.establish(ctx)
		testassert.NoError(t, err)
	}
	want := []string{"/v1/health", "/v1/events", "/v1/health"}
	{
		got := f.paths()
		testassert.Require(t, slices.Equal(got, want), "establish requests = %v, want the stream established before the descriptive read %v", got, want)
	}
	{
		_, rpcErr := r.initialize(ctx, nil)
		testassert.Require(t, rpcErr == nil, "initialize: %+v", rpcErr)
	}
	testassert.Equal(t, f.countPath("/v1/events"), 1)
}

// TestFailedRepairLeavesNoConnection proves a lost established stream whose
// reconnect cannot open a new stream ends as one failed repair with the
// recovery obligation intact (established once, no connection, no
// publication), and that an explicit later initialize installs once through
// the same path: connected, resync, and a fresh snapshot for the retained
// selection.
func TestFailedRepairLeavesNoConnection(t *testing.T) {
	f := newFrameFixture(t, frameInstance, false)
	r, dataDir := newFrameRunner(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ctx = ctx
	endFirst := f.end(1)
	{
		_, rpcErr := r.initialize(ctx, nil)
		testassert.Require(t, rpcErr == nil, "initialize: %+v", rpcErr)
	}
	f.awaitEventsOpen(t)
	drainEventFrames(r) // the initial connection's announcement
	r.selection = selection{id: "s1", generation: 1}

	f.setEventsFailure("status")
	close(endFirst)
	f.awaitEventsOpen(t) // the failed reconnect's stream request
	// Stop any further startup attempt so quiescence is deterministic.
	testassert.NoError(t, os.Remove(filepath.Join(dataDir, "runtime.json")))
	r.pumps.Wait()
	testassert.Require(t, r.currentConnection() == nil, "a failed repair left a connection installed")
	testassert.Require(t, r.initialized, "the established-once flag was lost with the connection")
	{
		_, rpcErr := r.requireConnection()
		testassert.Require(t, rpcErr != nil, "a lost connection was reported available")
	}
	for _, frame := range drainEventFrames(r) {
		testassert.Require(t, !strings.Contains(frame, `"runtime/connected"`) && !strings.Contains(frame, `"runtime/resync_required"`) && !strings.Contains(frame, `"session/snapshot"`), "a failed repair published %s", frame)
	}
	testassert.Equal(t, f.countPath("/v1/events"), 2)

	f.setEventsFailure("")
	f.writeRecord(t, dataDir)
	{
		_, rpcErr := r.initialize(ctx, nil)
		testassert.Require(t, rpcErr == nil, "explicit initialize after recovery: %+v", rpcErr)
	}
	testassert.Require(t, r.currentConnection() != nil, "explicit initialize left no connection")
	testassert.Equal(t, f.countPath("/v1/events"), 3)
	frames := drainEventFrames(r)
	testassert.Require(t, containsFrame(frames, `"runtime/connected"`) && containsFrame(frames, `"runtime/resync_required"`) && containsFrame(frames, `"workspace":"/ws/s1"`), "the recovered connection was not announced: %v; the recovery did not record the repair: %v; the recovery re-snapshotted no retained selection: %v", frames, frames, frames)
	testassert.Equal(t, f.countPath("/v1/sessions/s1/hydration"), 1)
}

// TestResponseAdmissionWaitsCancellably proves the actual response admission
// waits on the full bounded queue and returns on caller cancellation without
// dropping or reordering anything.
func TestResponseAdmissionWaitsCancellably(t *testing.T) {
	r := New(Options{Workspace: "/ws"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ctx = ctx
	for i := 0; i < responseQueueSize; i++ {
		r.send(response{JSONRPC: jsonRPCVersion, ID: json.RawMessage(fmt.Sprintf("%d", i)), Result: json.RawMessage("null")})
	}
	testassert.Equal(t, len(r.responses), responseQueueSize)
	started := make(chan struct{})
	admitted := make(chan struct{})
	go func() {
		close(started)
		r.send(response{JSONRPC: jsonRPCVersion, ID: json.RawMessage("99"), Result: json.RawMessage("null")})
		close(admitted)
	}()
	<-started
	cancel()
	select {
	case <-admitted:
	case <-time.After(15 * time.Second):
		t.Fatal("a full response queue admission did not return on caller cancellation")
	}
	testassert.Equal(t, len(r.responses), responseQueueSize)
}

// drainEventFrames empties the queued notification frames.
func drainEventFrames(r *Runner) []string {
	var frames []string
	for {
		select {
		case frame := <-r.events:
			frames = append(frames, string(frame.data))
			continue
		default:
		}
		return frames
	}
}

// containsFrame reports whether one queued frame carries the fragment.
func containsFrame(frames []string, fragment string) bool {
	for _, frame := range frames {
		if strings.Contains(frame, fragment) {
			return true
		}
	}
	return false
}
