package client_test

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
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/protocol"
)

// The client package's controlled loopback fixtures: a scripted server models
// the transport for the discovery-shape, health-classification, and
// frame-parser rows, while the runtime integration tests drive the real
// authenticated owner. Every record is a real mode-0600 file under the test's
// temporary root.

const (
	testInstance   = "0123456789abcdef0123456789abcdef"
	testCredential = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// recordContent builds one private four-field discovery document.
func recordContent(endpoint, instance, version, credential string) string {
	return fmt.Sprintf(`{"endpoint":%q,"instance_id":%q,"protocol_version":%q,"credential":%q}`,
		endpoint, instance, version, credential)
}

// writeDiscoveryContent writes one discovery document at mode 0600.
func writeDiscoveryContent(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "discovery.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write discovery record: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod discovery record: %v", err)
	}
	return path
}

// writeDiscovery writes one well-formed discovery record at mode 0600.
func writeDiscovery(t *testing.T, endpoint, instance, version, credential string) string {
	t.Helper()
	return writeDiscoveryContent(t, recordContent(endpoint, instance, version, credential))
}

// newFixture starts one controlled loopback fixture with the given health and
// events handlers; a nil handler leaves that route unmounted, and each
// supplied extra map is mounted beside them.
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

// jsonHealth answers one authenticated health body.
func jsonHealth(instance, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"instance_id":%q,"protocol_version":%q}`, instance, version)
	}
}

// scriptDoer is the test-local capturing transport installed on one connected
// client's generated transport. It records every request it receives and
// answers with the scripted response, or one empty 204 when no script is set.
type scriptDoer struct {
	calls   []scriptCall
	respond func(*http.Request) (*http.Response, error)
}

// scriptCall is one request as the transport received it.
type scriptCall struct {
	method string
	target string
	header http.Header
	body   string
}

// Do records one request and answers it.
func (d *scriptDoer) Do(request *http.Request) (*http.Response, error) {
	body := ""
	if request.Body != nil {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		body = string(data)
		_ = request.Body.Close()
	}
	d.calls = append(d.calls, scriptCall{method: request.Method, target: request.URL.String(), header: request.Header.Clone(), body: body})
	if d.respond != nil {
		return d.respond(request)
	}
	return scriptedResponse(http.StatusNoContent, ""), nil
}

// count returns the number of requests the transport received.
func (d *scriptDoer) count() int { return len(d.calls) }

// one returns the single received request, failing on any other count.
func (d *scriptDoer) one(t *testing.T) scriptCall {
	t.Helper()
	if len(d.calls) != 1 {
		t.Fatalf("transport received %d requests, want exactly one", len(d.calls))
	}
	return d.calls[0]
}

// scriptedResponse builds one raw transport response with exact body bytes.
func scriptedResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// installDoer replaces one connected client's generated HTTP doer with the
// test-local transport, leaving the verified endpoint and request editors.
func installDoer(t *testing.T, c *client.Client, doer protocol.HttpRequestDoer) {
	t.Helper()
	generated, ok := c.ClientWithResponses.ClientInterface.(*protocol.Client)
	if !ok {
		t.Fatal("the connected client does not wrap a generated protocol client")
	}
	generated.Client = doer
}

// newScriptFixture connects one client to the authenticated fixture and
// replaces its generated doer with the given capturing transport.
func newScriptFixture(t *testing.T, doer *scriptDoer) (*client.Client, string) {
	t.Helper()
	server := newFixture(t, jsonHealth(testInstance, "1"), nil)
	c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
	installDoer(t, c, doer)
	return c, server.URL
}

// connectFixture connects one client to a discovery path and fails the test
// on any error.
func connectFixture(t *testing.T, path string) *client.Client {
	t.Helper()
	c, err := client.Connect(context.Background(), path)
	if err != nil {
		t.Fatalf("Connect(%s): %v", path, err)
	}
	return c
}

// drainEvents reads every delivered event and then the single terminal error.
func drainEvents(events <-chan protocol.Event, errs <-chan error) ([]protocol.Event, error) {
	var delivered []protocol.Event
	for event := range events {
		delivered = append(delivered, event)
	}
	return delivered, <-errs
}

// notificationFrame encodes one generated Event as a declared SSE frame.
func notificationFrame(t *testing.T, event protocol.Event) string {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return "event: notification\ndata: " + string(data) + "\n\n"
}

// rawNotification wraps one raw JSON document as a declared SSE frame.
func rawNotification(document string) string {
	return "event: notification\ndata: " + document + "\n\n"
}

// ptr returns a pointer to one value.
func ptr[T any](value T) *T { return &value }

// operationEventScope is the real producer shape of every progress hint: the
// Operation's scope with its Session and Workspace ancestors.
func operationEventScope() protocol.Scope {
	var scope protocol.Scope
	if err := scope.FromOperationScope(protocol.OperationScope{
		Kind:        protocol.OperationScopeKindOperation,
		Workspace:   ptr("/w"),
		SessionId:   testInstance,
		OperationId: "op-1",
	}); err != nil {
		panic(err) // plain members; the marshal cannot fail
	}
	return scope
}

// scopeOpenedEvent builds one valid scope lifecycle hint carrying the given
// addressable scope.
func scopeOpenedEvent(t *testing.T, scope protocol.Scope) protocol.Event {
	t.Helper()
	var event protocol.Event
	if err := event.FromScopeEvent(protocol.ScopeEvent{Kind: protocol.ScopeOpened, Scope: scope}); err != nil {
		t.Fatalf("build scope_opened: %v", err)
	}
	return event
}

// sessionEventScope builds one Session-granularity scope, the producer shape
// of the revisioned invalidation hints.
func sessionEventScope() protocol.Scope {
	var scope protocol.Scope
	if err := scope.FromSessionScope(protocol.SessionScope{
		Kind:      protocol.SessionScopeKindSession,
		SessionId: testInstance,
	}); err != nil {
		panic(err) // plain members; the marshal cannot fail
	}
	return scope
}

// workspaceEventScope builds one Workspace scope carrying the lexical root.
func workspaceEventScope() protocol.Scope {
	var scope protocol.Scope
	if err := scope.FromWorkspaceScope(protocol.WorkspaceScope{
		Kind:      protocol.WorkspaceScopeKindWorkspace,
		Workspace: "/w",
	}); err != nil {
		panic(err) // plain members; the marshal cannot fail
	}
	return scope
}

// agentEventScope builds one Agent scope carrying its complete ancestor
// identity, the producer shape of the agent lifecycle hints.
func agentEventScope() protocol.Scope {
	var scope protocol.Scope
	if err := scope.FromAgentScope(protocol.AgentScope{
		Kind:        protocol.AgentScopeKindAgent,
		Workspace:   ptr("/w"),
		SessionId:   testInstance,
		OperationId: "op-1",
	}); err != nil {
		panic(err) // plain members; the marshal cannot fail
	}
	return scope
}

// jobEventScope builds one job-member scope, the producer shape of the
// job-granularity invalidation hints.
func jobEventScope() protocol.Scope {
	var scope protocol.Scope
	if err := scope.FromJobScope(protocol.JobScope{
		Kind:      protocol.JobScopeKindJob,
		Workspace: ptr("/w"),
		SessionId: testInstance,
		JobId:     "job-1",
	}); err != nil {
		panic(err) // plain members; the marshal cannot fail
	}
	return scope
}

// sessionChangedEvent builds one valid revisioned invalidation hint.
func sessionChangedEvent(t *testing.T, instance, durable, local string) protocol.Event {
	t.Helper()
	var event protocol.Event
	if err := event.FromSessionChangedEvent(protocol.SessionChangedEvent{
		Kind:  protocol.SessionChanged,
		Scope: sessionEventScope(),
		SessionRevision: protocol.SessionRevision{
			InstanceId:      instance,
			DurableRevision: durable,
			LocalRevision:   local,
		},
	}); err != nil {
		t.Fatalf("build session_changed: %v", err)
	}
	return event
}

// writeFrames answers the events route with the fixed script and returns.
func writeFrames(frames string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, frames)
	}
}

// TestConnectStrictDiscovery drives the private startup-file shape: a regular
// mode-0600 single document with the exact four fields and a loopback HTTP
// endpoint; every deviation is one ErrNoRuntime.
func TestConnectStrictDiscovery(t *testing.T) {
	server := newFixture(t, jsonHealth(testInstance, "1"), nil)
	valid := recordContent(server.URL, testInstance, "1", testCredential)

	t.Run("valid record connects", func(t *testing.T) {
		c := connectFixture(t, writeDiscoveryContent(t, valid))
		if c.InstanceID() != testInstance {
			t.Fatalf("InstanceID = %q, want %q", c.InstanceID(), testInstance)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := client.Connect(context.Background(), filepath.Join(t.TempDir(), "absent.json"))
		assertNoRuntime(t, err)
	})

	t.Run("wrong mode", func(t *testing.T) {
		path := writeDiscoveryContent(t, valid)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		_, err := client.Connect(context.Background(), path)
		assertNoRuntime(t, err)
	})

	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "discovery.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_, err := client.Connect(context.Background(), path)
		assertNoRuntime(t, err)
	})

	cases := []struct {
		name    string
		content string
	}{
		{"unknown member", strings.TrimSuffix(valid, "}") + `,"extra":1}`},
		{"trailing document", valid + valid},
		{"null document", "null"},
		{"missing member", fmt.Sprintf(`{"endpoint":%q,"instance_id":%q,"protocol_version":"1"}`, server.URL, testInstance)},
		{"wrong member type", fmt.Sprintf(`{"endpoint":1,"instance_id":%q,"protocol_version":"1","credential":%q}`, testInstance, testCredential)},
		{"endpoint with userinfo", recordContent("http://user@"+strings.TrimPrefix(server.URL, "http://"), testInstance, "1", testCredential)},
		{"endpoint with path", recordContent(server.URL+"/", testInstance, "1", testCredential)},
		{"endpoint with query", recordContent(server.URL+"?x=1", testInstance, "1", testCredential)},
		{"endpoint with fragment", recordContent(server.URL+"#frag", testInstance, "1", testCredential)},
		{"endpoint is https", recordContent("https://"+strings.TrimPrefix(server.URL, "http://"), testInstance, "1", testCredential)},
		{"endpoint host is localhost", recordContent("http://localhost:"+strings.TrimPrefix(server.URL, "http://127.0.0.1:"), testInstance, "1", testCredential)},
		{"endpoint has no port", recordContent("http://127.0.0.1", testInstance, "1", testCredential)},
		{"endpoint port is not numeric", recordContent("http://127.0.0.1:notaport", testInstance, "1", testCredential)},
		{"endpoint port is zero", recordContent("http://127.0.0.1:0", testInstance, "1", testCredential)},
		{"endpoint port overflows", recordContent("http://127.0.0.1:99999999999999999999", testInstance, "1", testCredential)},
		{"instance is uppercase hex", recordContent(server.URL, strings.ToUpper(testInstance), "1", testCredential)},
		{"instance is short", recordContent(server.URL, testInstance[:31], "1", testCredential)},
		{"credential is short", recordContent(server.URL, testInstance, "1", testCredential[:63])},
		{"credential is uppercase hex", recordContent(server.URL, testInstance, "1", strings.ToUpper(testCredential))},
		{"version is empty", recordContent(server.URL, testInstance, "", testCredential)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Connect(context.Background(), writeDiscoveryContent(t, tc.content))
			assertNoRuntime(t, err)
		})
	}
}

// TestConnectHealthClassification proves the authenticated-health decision
// order: instance identity first, then the live version, then the record's
// version, all against the generated protocol version; every other failure
// class collapses to ErrNoRuntime and a redirect is never followed.
func TestConnectHealthClassification(t *testing.T) {
	other := strings.Repeat("a", 32)

	t.Run("health instance differs", func(t *testing.T) {
		server := newFixture(t, jsonHealth(other, "1"), nil)
		_, err := client.Connect(context.Background(), writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		assertNoRuntime(t, err)
	})

	t.Run("live version is malformed", func(t *testing.T) {
		bodies := []struct {
			name string
			body string
		}{
			{"missing", fmt.Sprintf(`{"instance_id":%q}`, testInstance)},
			{"null", fmt.Sprintf(`{"instance_id":%q,"protocol_version":null}`, testInstance)},
			{"empty", fmt.Sprintf(`{"instance_id":%q,"protocol_version":""}`, testInstance)},
		}
		for _, tc := range bodies {
			t.Run(tc.name, func(t *testing.T) {
				server := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tc.body)
				}, nil)
				_, err := client.Connect(context.Background(), writeDiscovery(t, server.URL, testInstance, "1", testCredential))
				assertNoRuntime(t, err)
			})
		}
	})

	t.Run("live version differs", func(t *testing.T) {
		server := newFixture(t, jsonHealth(testInstance, "2"), nil)
		_, err := client.Connect(context.Background(), writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		assertIncompatible(t, err)
	})

	t.Run("record and live versions differ", func(t *testing.T) {
		server := newFixture(t, jsonHealth(testInstance, "2"), nil)
		_, err := client.Connect(context.Background(), writeDiscovery(t, server.URL, testInstance, "2", testCredential))
		assertIncompatible(t, err)
	})

	t.Run("record version disagrees with a live compatible owner", func(t *testing.T) {
		server := newFixture(t, jsonHealth(testInstance, "1"), nil)
		_, err := client.Connect(context.Background(), writeDiscovery(t, server.URL, testInstance, "2", testCredential))
		assertNoRuntime(t, err)
	})

	t.Run("health status failure", func(t *testing.T) {
		server := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusInternalServerError)
		}, nil)
		_, err := client.Connect(context.Background(), writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		assertNoRuntime(t, err)
	})

	t.Run("health body is malformed", func(t *testing.T) {
		server := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"instance_id":`)
		}, nil)
		_, err := client.Connect(context.Background(), writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		assertNoRuntime(t, err)
	})

	t.Run("readiness probe does not follow redirects", func(t *testing.T) {
		var redirected bool
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
		})
		mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, _ *http.Request) {
			redirected = true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"instance_id":%q,"protocol_version":"1"}`, testInstance)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		_, err := client.Connect(context.Background(), writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		assertNoRuntime(t, err)
		if redirected {
			t.Fatal("the readiness probe followed the redirect")
		}
	})

	t.Run("caller cancellation is preserved", func(t *testing.T) {
		arrived := make(chan struct{})
		server := newFixture(t, func(_ http.ResponseWriter, r *http.Request) {
			close(arrived)
			<-r.Context().Done()
		}, nil)
		path := writeDiscovery(t, server.URL, testInstance, "1", testCredential)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := client.Connect(ctx, path)
			done <- err
		}()
		<-arrived
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Connect = %v, want context.Canceled", err)
		}
	})
}

// TestEventsFrames drives the SSE parser through the public stream: declared
// notification frames and keepalive comments are accepted, duplicates and
// out-of-order hints are preserved, every malformed or unauthenticated frame
// is one ErrResyncRequired, and intentional cancellation is ctx.Err.
func TestEventsFrames(t *testing.T) {
	t.Run("duplicate and out-of-order hints are preserved", func(t *testing.T) {
		frames := ": keepalive\n\n" +
			notificationFrame(t, sessionChangedEvent(t, testInstance, "3", "0")) +
			": keepalive\n\n" +
			notificationFrame(t, sessionChangedEvent(t, testInstance, "3", "0")) +
			notificationFrame(t, sessionChangedEvent(t, testInstance, "2", "0"))
		server := newFixture(t, jsonHealth(testInstance, "1"), writeFrames(frames))
		c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		events, errs := c.Events(context.Background())
		delivered, err := drainEvents(events, errs)
		if !errors.Is(err, client.ErrResyncRequired) {
			t.Fatalf("disconnect error = %v, want ErrResyncRequired", err)
		}
		want := []string{"3", "3", "2"}
		if len(delivered) != len(want) {
			t.Fatalf("delivered %d hints, want %d", len(delivered), len(want))
		}
		for i, event := range delivered {
			if kind, err := event.Discriminator(); err != nil || kind != "session_changed" {
				t.Fatalf("hint %d kind = %q (%v), want session_changed", i, kind, err)
			}
			body, err := event.AsSessionChangedEvent()
			if err != nil {
				t.Fatalf("hint %d decode: %v", i, err)
			}
			if body.SessionRevision.DurableRevision != want[i] {
				t.Fatalf("hint %d durable revision = %q, want %q", i, body.SessionRevision.DurableRevision, want[i])
			}
			if body.SessionRevision.InstanceId != c.InstanceID() {
				t.Fatalf("hint %d instance = %q, want the connection instance %q", i, body.SessionRevision.InstanceId, c.InstanceID())
			}
		}
	})

	t.Run("valid transient progress", func(t *testing.T) {
		var delta protocol.Event
		if err := delta.FromTextDeltaEvent(protocol.TextDeltaEvent{
			Kind: protocol.TextDelta, Scope: operationEventScope(), Position: 7, Content: "chunk",
		}); err != nil {
			t.Fatalf("build text_delta: %v", err)
		}
		var refusal protocol.Event
		if err := refusal.FromRefusalDeltaEvent(protocol.RefusalDeltaEvent{
			Kind: protocol.RefusalDelta, Scope: operationEventScope(), Content: "refused",
		}); err != nil {
			t.Fatalf("build refusal_delta: %v", err)
		}
		var started protocol.Event
		if err := started.FromToolStartedEvent(protocol.ToolStartedEvent{
			Kind: protocol.ToolStarted, Scope: operationEventScope(), CallId: "call-1", Ordinal: 0, Name: "read",
		}); err != nil {
			t.Fatalf("build tool_started: %v", err)
		}
		var finished protocol.Event
		if err := finished.FromToolFinishedEvent(protocol.ToolFinishedEvent{
			Kind: protocol.ToolFinished, Scope: operationEventScope(), CallId: "call-1", Status: protocol.ToolCallStatusSuccess,
		}); err != nil {
			t.Fatalf("build tool_finished: %v", err)
		}
		frames := notificationFrame(t, delta) + notificationFrame(t, refusal) + notificationFrame(t, started) + notificationFrame(t, finished)
		server := newFixture(t, jsonHealth(testInstance, "1"), writeFrames(frames))
		c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		events, errs := c.Events(context.Background())
		delivered, err := drainEvents(events, errs)
		if !errors.Is(err, client.ErrResyncRequired) {
			t.Fatalf("disconnect error = %v, want ErrResyncRequired", err)
		}
		if len(delivered) != 4 {
			t.Fatalf("delivered %d transient events, want 4", len(delivered))
		}
		for i, want := range []string{"text_delta", "refusal_delta", "tool_started", "tool_finished"} {
			if kind, _ := delivered[i].Discriminator(); kind != want {
				t.Fatalf("event %d kind = %q, want %q", i, kind, want)
			}
		}
		refusalBody, err := delivered[1].AsRefusalDeltaEvent()
		if err != nil || refusalBody.Content != "refused" {
			t.Fatalf("delivered refusal delta = (%+v, %v), want its distinct refusal content", refusalBody, err)
		}
	})

	// Every non-global scope kind delivers through its real producer shape:
	// the Workspace and Agent lifecycle hints and the job-member
	// invalidation hint carry the identities that address their subject.
	t.Run("addressable scope kinds", func(t *testing.T) {
		var job protocol.Event
		if err := job.FromSessionChangedEvent(protocol.SessionChangedEvent{
			Kind:            protocol.SessionChanged,
			Scope:           jobEventScope(),
			SessionRevision: protocol.SessionRevision{InstanceId: testInstance, DurableRevision: "1", LocalRevision: "0"},
		}); err != nil {
			t.Fatalf("build session_changed: %v", err)
		}
		frames := notificationFrame(t, scopeOpenedEvent(t, workspaceEventScope())) +
			notificationFrame(t, scopeOpenedEvent(t, agentEventScope())) +
			notificationFrame(t, job)
		server := newFixture(t, jsonHealth(testInstance, "1"), writeFrames(frames))
		c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		events, errs := c.Events(context.Background())
		delivered, err := drainEvents(events, errs)
		if !errors.Is(err, client.ErrResyncRequired) {
			t.Fatalf("disconnect error = %v, want ErrResyncRequired", err)
		}
		if len(delivered) != 3 {
			t.Fatalf("delivered %d events, want the workspace, agent, and job hints", len(delivered))
		}
		for i, want := range []string{"scope_opened", "scope_opened", "session_changed"} {
			if kind, _ := delivered[i].Discriminator(); kind != want {
				t.Fatalf("event %d kind = %q, want %q", i, kind, want)
			}
		}
	})

	malformed := []struct {
		name  string
		frame string
	}{
		{"unknown kind", rawNotification(`{"kind":"bogus","scope":{"kind":"runtime"}}`)},
		{"unknown member", rawNotification(strings.TrimSuffix(mustMarshal(t, sessionChangedEvent(t, testInstance, "1", "0")), "}") + `,"extra":1}`)},
		{"top-level instance_id on a transient", rawNotification(fmt.Sprintf(`{"kind":"scope_opened","scope":{"kind":"runtime"},"instance_id":%q}`, testInstance))},
		{"missing required member", rawNotification(`{"kind":"session_changed","scope":{"kind":"session"}}`)},
		{"null required member", rawNotification(`{"kind":"session_changed","scope":null,"session_revision":{"instance_id":"` + testInstance + `","durable_revision":"1","local_revision":"0"}}`)},
		{"revision instance mismatch", notificationFrame(t, sessionChangedEvent(t, strings.Repeat("b", 32), "1", "0"))},
		{"unknown scope kind", rawNotification(`{"kind":"scope_opened","scope":{"kind":"bogus"}}`)},
		{"unknown scope member", rawNotification(fmt.Sprintf(`{"kind":"session_changed","scope":{"kind":"session","bogus":true},"session_revision":{"instance_id":%q,"durable_revision":"1","local_revision":"0"}}`, testInstance))},
		{"wrong-typed scope member", rawNotification(`{"kind":"scope_opened","scope":{"kind":5}}`)},
		{"session_id is not hex32", rawNotification(fmt.Sprintf(`{"kind":"session_changed","scope":{"kind":"session","session_id":"nothex"},"session_revision":{"instance_id":%q,"durable_revision":"1","local_revision":"0"}}`, testInstance))},
		{"workspace scope without its identity", rawNotification(`{"kind":"scope_opened","scope":{"kind":"workspace"}}`)},
		{"session scope without its identity", rawNotification(`{"kind":"scope_opened","scope":{"kind":"session"}}`)},
		{"operation scope without the session identity", rawNotification(`{"kind":"scope_opened","scope":{"kind":"operation","workspace":"/w","operation_id":"op-1"}}`)},
		{"operation scope without the operation identity", rawNotification(fmt.Sprintf(`{"kind":"scope_opened","scope":{"kind":"operation","workspace":"/w","session_id":%q}}`, testInstance))},
		{"agent scope without the operation identity", rawNotification(fmt.Sprintf(`{"kind":"scope_opened","scope":{"kind":"agent","session_id":%q}}`, testInstance))},
		{"job scope without the session identity", rawNotification(`{"kind":"scope_opened","scope":{"kind":"job","job_id":"job-1"}}`)},
		{"job scope without the job identity", rawNotification(fmt.Sprintf(`{"kind":"scope_opened","scope":{"kind":"job","session_id":%q}}`, testInstance))},
		{"empty scope member", rawNotification(`{"kind":"scope_opened","scope":{"kind":"runtime","workspace":""}}`)},
		{"negative text position", rawNotification(`{"kind":"text_delta","scope":{"kind":"runtime"},"position":-1,"content":"x"}`)},
		{"empty text content", rawNotification(`{"kind":"text_delta","scope":{"kind":"runtime"},"position":0,"content":""}`)},
		{"empty refusal content", rawNotification(`{"kind":"refusal_delta","scope":{"kind":"runtime"},"content":""}`)},
		{"missing refusal content", rawNotification(`{"kind":"refusal_delta","scope":{"kind":"runtime"}}`)},
		{"position member on refusal_delta", rawNotification(`{"kind":"refusal_delta","scope":{"kind":"runtime"},"content":"x","position":0}`)},
		{"unknown tool status", rawNotification(`{"kind":"tool_finished","scope":{"kind":"runtime"},"call_id":"c","status":"pending"}`)},
		{"negative tool ordinal", rawNotification(`{"kind":"tool_started","scope":{"kind":"runtime"},"call_id":"c","ordinal":-1,"name":"n"}`)},
		{"wrong event name", "event: other\ndata: {}\n\n"},
		{"unexpected stream field", "id: 7\nevent: notification\ndata: {}\n\n"},
		{"unknown stream line", "garbage\n\n"},
		{"incomplete frame at EOF", "event: notification\ndata: " + mustMarshal(t, sessionChangedEvent(t, testInstance, "1", "0"))},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			server := newFixture(t, jsonHealth(testInstance, "1"), writeFrames(tc.frame))
			c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
			events, errs := c.Events(context.Background())
			delivered, err := drainEvents(events, errs)
			if !errors.Is(err, client.ErrResyncRequired) {
				t.Fatalf("malformed frame error = %v, want ErrResyncRequired", err)
			}
			if len(delivered) != 0 {
				t.Fatalf("malformed frame delivered %d events, want none", len(delivered))
			}
		})
	}

	t.Run("status failure", func(t *testing.T) {
		server := newFixture(t, jsonHealth(testInstance, "1"), func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusInternalServerError)
		})
		c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		events, errs := c.Events(context.Background())
		delivered, err := drainEvents(events, errs)
		if !errors.Is(err, client.ErrResyncRequired) {
			t.Fatalf("status failure error = %v, want ErrResyncRequired", err)
		}
		if len(delivered) != 0 {
			t.Fatalf("status failure delivered %d events, want none", len(delivered))
		}
	})

	t.Run("queue overflow", func(t *testing.T) {
		const total = 300
		server := newFixture(t, jsonHealth(testInstance, "1"), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			for i := 0; i < total; i++ {
				if r.Context().Err() != nil {
					return
				}
				if _, err := io.WriteString(w, notificationFrame(t, sessionChangedEvent(t, testInstance, "1", "0"))); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		})
		c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		events, errs := c.Events(context.Background())
		// The caller never reads, so the bounded queue overflows on frame 257
		// and the stream ends with the resync signal alone.
		err := <-errs
		if !errors.Is(err, client.ErrResyncRequired) {
			t.Fatalf("overflow error = %v, want ErrResyncRequired", err)
		}
		if got := len(events); got != 256 {
			t.Fatalf("buffered events = %d, want the 256-event queue", got)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		server := newFixture(t, jsonHealth(testInstance, "1"), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		})
		c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		ctx, cancel := context.WithCancel(context.Background())
		events, errs := c.Events(ctx)
		cancel()
		delivered, err := drainEvents(events, errs)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled stream error = %v, want context.Canceled", err)
		}
		if len(delivered) != 0 {
			t.Fatalf("canceled stream delivered %d events, want none", len(delivered))
		}
	})
}

// TestRequestTransport drives the authenticated byte transport: request
// construction carries the verified endpoint, method, intact query and body,
// the bearer editor, and a JSON content type only when a body exists; raw
// status and body bytes return unprojected; invalid or foreign targets and
// the event route are refused before the transport is called; a redirect is
// returned unfollowed; and cancellation cancels the request alone.
func TestRequestTransport(t *testing.T) {
	t.Run("method query body and auth reach the verified endpoint", func(t *testing.T) {
		doer := &scriptDoer{}
		c, endpoint := newScriptFixture(t, doer)
		response, err := c.Request(context.Background(), "PUT", "/v1/sessions/abc/restore?limit=2&name=%2Ftmp&keep=a+b", strings.NewReader(`{"operation_id":"op-1"}`))
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", response.StatusCode)
		}
		call := doer.one(t)
		if call.method != "PUT" {
			t.Fatalf("method = %q, want PUT", call.method)
		}
		if want := endpoint + "/v1/sessions/abc/restore?limit=2&name=%2Ftmp&keep=a+b"; call.target != want {
			t.Fatalf("target = %q, want %q", call.target, want)
		}
		if got := call.header.Get("Authorization"); got != "Bearer "+testCredential {
			t.Fatalf("authorization = %q, want the connection credential", got)
		}
		if got := call.header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("content type = %q, want application/json", got)
		}
		if call.body != `{"operation_id":"op-1"}` {
			t.Fatalf("body = %q, want the exact request document", call.body)
		}
	})

	t.Run("bodyless request carries no content type", func(t *testing.T) {
		doer := &scriptDoer{}
		c, endpoint := newScriptFixture(t, doer)
		response, err := c.Request(context.Background(), "GET", "/v1/configuration", nil)
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", response.StatusCode)
		}
		call := doer.one(t)
		if call.method != "GET" {
			t.Fatalf("method = %q, want GET", call.method)
		}
		if want := endpoint + "/v1/configuration"; call.target != want {
			t.Fatalf("target = %q, want %q", call.target, want)
		}
		if got := call.header.Get("Authorization"); got != "Bearer "+testCredential {
			t.Fatalf("authorization = %q, want the connection credential", got)
		}
		if got := call.header.Get("Content-Type"); got != "" {
			t.Fatalf("content type = %q, want none without a body", got)
		}
		if call.body != "" {
			t.Fatalf("body = %q, want none", call.body)
		}
	})

	t.Run("encoded hash data stays legal", func(t *testing.T) {
		doer := &scriptDoer{}
		c, endpoint := newScriptFixture(t, doer)
		response, err := c.Request(context.Background(), "GET", "/v1/sessions/a%23b?filter=%23x", nil)
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", response.StatusCode)
		}
		call := doer.one(t)
		if want := endpoint + "/v1/sessions/a%23b?filter=%23x"; call.target != want {
			t.Fatalf("target = %q, want the encoded hash data %q", call.target, want)
		}
	})

	t.Run("raw error and partial bodies pass through", func(t *testing.T) {
		const partial = `{"restored":[1,2],"failed":[3],"durable_revision":"9007199254740993"}`
		doer := &scriptDoer{respond: func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusConflict, partial), nil
		}}
		c, _ := newScriptFixture(t, doer)
		response, err := c.Request(context.Background(), "POST", "/v1/sessions/abc/restore", strings.NewReader(`{"operation_id":"op-1"}`))
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want the raw 409", response.StatusCode)
		}
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read response body: %v", err)
		}
		if string(data) != partial {
			t.Fatalf("body = %q, want the exact raw bytes %q", data, partial)
		}
	})

	t.Run("invalid and foreign targets fail before the transport", func(t *testing.T) {
		doer := &scriptDoer{}
		c, _ := newScriptFixture(t, doer)
		targets := []struct {
			name   string
			method string
			path   string
		}{
			{"empty target", "GET", ""},
			{"relative path", "GET", "v1/configuration"},
			{"query only", "GET", "?x=1"},
			{"outside v1", "GET", "/v2/configuration"},
			{"version prefix without a slash", "GET", "/v1"},
			{"absolute http target", "GET", "http://example.com/v1/configuration"},
			{"absolute https target", "GET", "https://example.com/v1/configuration"},
			{"network path reference", "GET", "//example.com/v1/configuration"},
			{"fragment", "GET", "/v1/configuration#frag"},
			{"fragment after query", "GET", "/v1/configuration?x=1#frag"},
			{"raw empty fragment", "GET", "/v1/health#"},
			{"raw empty fragment after query", "GET", "/v1/health?x#"},
			{"asterisk form", "OPTIONS", "*"},
		}
		for _, target := range targets {
			t.Run(target.name, func(t *testing.T) {
				response, err := c.Request(context.Background(), target.method, target.path, nil)
				if err == nil {
					response.Body.Close()
					t.Fatalf("Request(%q) = %s, want a refused target", target.path, response.Status)
				}
				if response != nil {
					t.Fatalf("Request(%q) returned a response beside the refusal", target.path)
				}
			})
		}
		if calls := doer.count(); calls != 0 {
			t.Fatalf("transport received %d requests, want none", calls)
		}
	})

	t.Run("event route is refused for every spelling and method", func(t *testing.T) {
		doer := &scriptDoer{}
		c, _ := newScriptFixture(t, doer)
		targets := []struct {
			name   string
			method string
			path   string
		}{
			{"exact route", "GET", "/v1/events"},
			{"query text", "GET", "/v1/events?x=1"},
			{"escaped first letter", "GET", "/v1/%65vents"},
			{"escaped last letter", "GET", "/v1/event%73"},
			{"escaped route with query", "GET", "/v1/event%73?x=1"},
			{"head", "HEAD", "/v1/events"},
			{"post", "POST", "/v1/events"},
		}
		for _, target := range targets {
			t.Run(target.name, func(t *testing.T) {
				response, err := c.Request(context.Background(), target.method, target.path, nil)
				if err == nil {
					response.Body.Close()
					t.Fatalf("Request(%q) = %s, want a refused target", target.path, response.Status)
				}
				if response != nil {
					t.Fatalf("Request(%q) returned a response beside the refusal", target.path)
				}
			})
		}
		if calls := doer.count(); calls != 0 {
			t.Fatalf("transport received %d requests, want none", calls)
		}
	})

	t.Run("redirect is returned unfollowed", func(t *testing.T) {
		var followed bool
		server := newFixture(t, jsonHealth(testInstance, "1"), nil, map[string]http.HandlerFunc{
			"/v1/moved": func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/v1/target", http.StatusTemporaryRedirect)
			},
			"/v1/target": func(http.ResponseWriter, *http.Request) { followed = true },
		})
		c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		response, err := c.Request(context.Background(), "GET", "/v1/moved", nil)
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("status = %d, want the raw 307", response.StatusCode)
		}
		if location := response.Header.Get("Location"); location != "/v1/target" {
			t.Fatalf("location = %q, want /v1/target", location)
		}
		if followed {
			t.Fatal("the request followed the redirect")
		}
	})

	t.Run("unclean path receives the server redirect unfollowed", func(t *testing.T) {
		server := newFixture(t, jsonHealth(testInstance, "1"), nil)
		c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		response, err := c.Request(context.Background(), "GET", "/v1//health", nil)
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("status = %d, want the server's 307", response.StatusCode)
		}
		if location := response.Header.Get("Location"); location != "/v1/health" {
			t.Fatalf("location = %q, want /v1/health", location)
		}
	})

	t.Run("cancellation cancels the request alone", func(t *testing.T) {
		arrived := make(chan struct{})
		observed := make(chan struct{})
		server := newFixture(t, jsonHealth(testInstance, "1"), nil, map[string]http.HandlerFunc{
			"/v1/parked": func(_ http.ResponseWriter, r *http.Request) {
				close(arrived)
				<-r.Context().Done()
				close(observed)
			},
		})
		c := connectFixture(t, writeDiscovery(t, server.URL, testInstance, "1", testCredential))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			response, err := c.Request(ctx, "GET", "/v1/parked", nil)
			if response != nil {
				response.Body.Close()
			}
			done <- err
		}()
		<-arrived
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Request = %v, want context.Canceled", err)
		}
		select {
		case <-observed:
		case <-time.After(30 * time.Second):
			t.Fatal("the parked transport never observed the cancellation")
		}
	})
}

// assertNoRuntime proves one failure is the single usable-owner classification.
func assertNoRuntime(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, client.ErrNoRuntime) {
		t.Fatalf("error = %v, want ErrNoRuntime", err)
	}
	if errors.Is(err, client.ErrIncompatibleProtocol) {
		t.Fatalf("error = %v, must not be ErrIncompatibleProtocol", err)
	}
}

// assertIncompatible proves one authenticated version mismatch classification.
func assertIncompatible(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, client.ErrIncompatibleProtocol) {
		t.Fatalf("error = %v, want ErrIncompatibleProtocol", err)
	}
	if errors.Is(err, client.ErrNoRuntime) {
		t.Fatalf("error = %v, must not be ErrNoRuntime", err)
	}
}

// mustMarshal encodes one generated event as JSON.
func mustMarshal(t *testing.T, event protocol.Event) string {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(data)
}
