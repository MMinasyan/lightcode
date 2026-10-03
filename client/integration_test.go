package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/protocol"
	"github.com/MMinasyan/lightcode/runtime"
)

// The runtime integration fixtures: every row drives the real isolated owner,
// its real loopback listener, its real mode-0600 discovery record, and the
// generated HTTP operations and SSE stream. HOME, the data root, and the
// configuration path are all isolated under test temporary roots.

const clientConfigDocument = `{"providers":{"prov":{"transport":{"base_url":%q,"api_key_env":"CLIENT_TEST_KEY"},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}}}`

const clientAgentsDocument = `{"integrated":{"model":"prov/m","system_prompt":"simple"}}`

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
	entries, err := os.ReadDir(filepath.Join("..", "internal", "catalog", "builtin"))
	if err != nil {
		t.Fatalf("read bundled catalog: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "internal", "catalog", "builtin", entry.Name()))
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

// newRealRuntime opens one production-composition Runtime over isolated HOME,
// data, and config paths, configured with one provider pointing at the given
// base URL.
func newRealRuntime(t *testing.T, baseURL string) (*runtime.Runtime, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLIENT_TEST_KEY", "client-test-key")
	isolateBundledCredentials(t)
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, fmt.Sprintf(clientConfigDocument, baseURL))
	writeServiceFile(t, agents.PathForConfig(configPath), clientAgentsDocument)
	r, err := runtime.Open(context.Background(), runtime.Options{DataDir: dataDir, ConfigPath: configPath, Plugins: builtin.Plugins()})
	if err != nil {
		t.Fatalf("runtime.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(context.Background()); err != nil {
			t.Errorf("runtime.Close: %v", err)
		}
	})
	return r, configPath
}

// openProtocol attaches the real listener and publishes its real discovery
// record at an isolated path.
func openProtocol(t *testing.T, r *runtime.Runtime) (*runtime.ProtocolServer, string) {
	t.Helper()
	ps, err := r.OpenProtocol(context.Background())
	if err != nil {
		t.Fatalf("OpenProtocol: %v", err)
	}
	record := filepath.Join(t.TempDir(), "discovery.json")
	if err := ps.PublishDiscovery(record); err != nil {
		t.Fatalf("PublishDiscovery: %v", err)
	}
	return ps, record
}

// textContentPart builds one generated text content part.
func textContentPart(t *testing.T, text string) protocol.ContentPart {
	t.Helper()
	var part protocol.ContentPart
	if err := part.FromTextPart(protocol.TextPart{Kind: protocol.TextPartKindText, Text: text}); err != nil {
		t.Fatalf("build text part: %v", err)
	}
	return part
}

// createSession creates one Session through the generated operation.
func createSession(t *testing.T, c *client.Client, workspace string) string {
	t.Helper()
	resp, err := c.CreateSessionWithResponse(context.Background(), protocol.CreateSessionRequest{Workspace: workspace, AgentType: "integrated"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if resp.JSON201 == nil {
		t.Fatalf("CreateSession status %d: %s", resp.HTTPResponse.StatusCode, resp.Body)
	}
	return resp.JSON201.SessionId
}

// hydrateSession reads one authoritative Session hydration through the
// generated operation.
func hydrateSession(t *testing.T, c *client.Client, sessionID string) *protocol.Hydration {
	t.Helper()
	resp, err := c.GetSessionHydrationWithResponse(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetSessionHydration: %v", err)
	}
	if resp.JSON200 == nil {
		t.Fatalf("GetSessionHydration status %d: %s", resp.HTTPResponse.StatusCode, resp.Body)
	}
	return resp.JSON200
}

// archiveSession and reopenSession advance one Session's durable revision.
func archiveSession(t *testing.T, c *client.Client, sessionID string) {
	t.Helper()
	resp, err := c.ArchiveSessionWithResponse(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}
	if resp.JSON200 == nil {
		t.Fatalf("ArchiveSession status %d: %s", resp.HTTPResponse.StatusCode, resp.Body)
	}
}

func reopenSession(t *testing.T, c *client.Client, sessionID string) {
	t.Helper()
	resp, err := c.ReopenSessionWithResponse(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReopenSession: %v", err)
	}
	if resp.JSON200 == nil {
		t.Fatalf("ReopenSession status %d: %s", resp.HTTPResponse.StatusCode, resp.Body)
	}
}

// submitOperation submits one regular operation through the generated
// operation.
func submitOperation(t *testing.T, c *client.Client, sessionID, operationID, text string) {
	t.Helper()
	resp, err := c.SubmitSessionWithResponse(context.Background(), sessionID, protocol.SubmitRequest{
		OperationId: operationID,
		Mode:        protocol.SubmitRequestModeRegular,
		Content:     []protocol.ContentPart{textContentPart(t, text)},
	})
	if err != nil {
		t.Fatalf("SubmitSession: %v", err)
	}
	if resp.JSON200 == nil {
		t.Fatalf("SubmitSession status %d: %s", resp.HTTPResponse.StatusCode, resp.Body)
	}
}

// waitEvent consumes the stream until one delivered event matches, failing on
// stream closure or timeout.
func waitEvent(t *testing.T, events <-chan protocol.Event, errs <-chan error, match func(protocol.Event) bool, what string) protocol.Event {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("event stream closed before %s: %v", what, <-errs)
			}
			if match(event) {
				return event
			}
		case <-timeout:
			t.Fatalf("no %s event arrived", what)
		}
	}
}

// eventKind returns one event's discriminator.
func eventKind(t *testing.T, event protocol.Event) string {
	t.Helper()
	kind, err := event.Discriminator()
	if err != nil {
		t.Fatalf("event discriminator: %v", err)
	}
	return kind
}

// awaitOperation polls the authoritative hydration until the named operation
// reaches the wanted terminal status.
func awaitOperation(t *testing.T, c *client.Client, sessionID, operationID string, want protocol.OperationStatus) *protocol.Hydration {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		hydration := hydrateSession(t, c, sessionID)
		for _, operation := range hydration.Operations {
			if operation.OperationId == operationID && operation.Status == want {
				return hydration
			}
		}
		if time.Now().After(deadline) {
			body, _ := json.Marshal(hydration.Operations)
			t.Fatalf("operation %s never reached %s: %s", operationID, want, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRealRuntimeClient drives the generated operations and the real SSE
// stream over one authenticated owner, including explicit rehydration after a
// reconnect and the typed refusal of a wrong credential.
func TestRealRuntimeClient(t *testing.T) {
	const baseURL = "http://127.0.0.1:1/v1"
	r, configPath := newRealRuntime(t, baseURL)
	ps, record := openProtocol(t, r)

	t.Run("generated reads, stream, and explicit rehydration", func(t *testing.T) {
		ctx := context.Background()
		c := connectFixture(t, record)
		health, err := c.GetHealthWithResponse(ctx)
		if err != nil || health.JSON200 == nil {
			t.Fatalf("GetHealth: %v", err)
		}
		if health.JSON200.InstanceId != c.InstanceID() {
			t.Fatalf("health instance = %q, client instance = %q", health.JSON200.InstanceId, c.InstanceID())
		}
		session := createSession(t, c, filepath.Join(t.TempDir(), "workspace"))
		hydration := hydrateSession(t, c, session)
		if hydration.Session.SessionId != session || hydration.SessionRevision.InstanceId != c.InstanceID() {
			t.Fatalf("hydration = %+v", hydration.Session)
		}
		listed, err := c.ListSessionsWithResponse(ctx, &protocol.ListSessionsParams{Workspace: hydration.Session.Workspace, Lifecycle: protocol.Open})
		if err != nil || listed.JSON200 == nil || len(*listed.JSON200) != 1 {
			t.Fatalf("ListSessions = %v (%v)", listed, err)
		}

		streamCtx, cancelStream := context.WithCancel(ctx)
		events, errs := c.Events(streamCtx)
		// A second creation after the stream is established is the delivery
		// barrier: its committed hint is guaranteed to be queued.
		createSession(t, c, filepath.Join(t.TempDir(), "workspace-2"))
		waitEvent(t, events, errs, func(protocol.Event) bool { return true }, "stream delivery")
		cancelStream()
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled stream error = %v, want context.Canceled", err)
		}

		reconnected := connectFixture(t, record)
		if reconnected.InstanceID() != c.InstanceID() {
			t.Fatalf("reconnect instance = %q, want %q", reconnected.InstanceID(), c.InstanceID())
		}
		rehydrated := hydrateSession(t, reconnected, session)
		if rehydrated.Session.SessionId != session || rehydrated.SessionRevision.InstanceId != reconnected.InstanceID() {
			t.Fatalf("rehydrated session = %+v", rehydrated.Session)
		}
	})

	t.Run("authenticated stream carries a configuration change", func(t *testing.T) {
		ctx := context.Background()
		c := connectFixture(t, record)
		streamCtx, cancelStream := context.WithCancel(ctx)
		defer cancelStream()
		events, errs := c.Events(streamCtx)
		createSession(t, c, filepath.Join(t.TempDir(), "workspace"))
		waitEvent(t, events, errs, func(protocol.Event) bool { return true }, "stream delivery")

		changed := fmt.Sprintf(`{"providers":{"prov":{"transport":{"base_url":%q,"api_key_env":"CLIENT_TEST_KEY"},"discovery":false,"models":{"m":{"name":"M","context_window":4096},"m2":{"name":"M2","context_window":8192}}}}}`, baseURL)
		writeServiceFile(t, configPath, changed)
		reload, err := c.ReloadConfigurationWithResponse(ctx)
		if err != nil || reload.JSON200 == nil {
			t.Fatalf("ReloadConfiguration: %v", err)
		}
		event := waitEvent(t, events, errs, func(event protocol.Event) bool {
			return eventKind(t, event) == "configuration_changed"
		}, "configuration_changed")
		body, err := event.AsConfigurationChangedEvent()
		if err != nil {
			t.Fatalf("configuration_changed decode: %v", err)
		}
		if body.ConfigurationRevision.InstanceId != c.InstanceID() {
			t.Fatalf("configuration revision instance = %q, want %q", body.ConfigurationRevision.InstanceId, c.InstanceID())
		}
		cancelStream()
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled stream error = %v, want context.Canceled", err)
		}
	})

	t.Run("wrong credential is no usable owner", func(t *testing.T) {
		path := writeDiscovery(t, ps.Endpoint(), testInstance, "1", "0"+testCredential[1:])
		_, err := client.Connect(context.Background(), path)
		assertNoRuntime(t, err)
	})
}

// parkedModel is one loopback model endpoint whose first response holds until
// the test releases it, recording whether the request context was canceled
// while parked.
type parkedModel struct {
	*httptest.Server

	arrived     chan struct{}
	release     chan struct{}
	arriveOnce  sync.Once
	releaseOnce sync.Once
	mu          sync.Mutex
	canceled    bool
}

// newParkedModel starts the parked model endpoint.
func newParkedModel(t *testing.T) *parkedModel {
	t.Helper()
	m := &parkedModel{arrived: make(chan struct{}), release: make(chan struct{})}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(func() {
		m.unblock()
		m.Server.Close()
	})
	return m
}

func (m *parkedModel) serve(w http.ResponseWriter, r *http.Request) {
	m.arriveOnce.Do(func() { close(m.arrived) })
	select {
	case <-m.release:
	case <-r.Context().Done():
		m.mu.Lock()
		m.canceled = true
		m.mu.Unlock()
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"released\"},\"finish_reason\":null}]}\n\n")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// unblock releases the parked request once.
func (m *parkedModel) unblock() {
	m.releaseOnce.Do(func() { close(m.release) })
}

// wasCanceled reports whether the parked request context was canceled before
// release.
func (m *parkedModel) wasCanceled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.canceled
}

// TestStreamCloseDoesNotCancelWork proves closing the client's event stream
// cancels neither the Runtime nor its admitted model work: the parked request
// stays live and the Operation settles successfully after release.
func TestStreamCloseDoesNotCancelWork(t *testing.T) {
	model := newParkedModel(t)
	r, _ := newRealRuntime(t, model.URL)
	_, record := openProtocol(t, r)
	c := connectFixture(t, record)
	session := createSession(t, c, filepath.Join(t.TempDir(), "workspace"))

	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	_, errs := c.Events(streamCtx)
	submitOperation(t, c, session, "op-parked", "parked")
	select {
	case <-model.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the parked model request never arrived")
	}
	cancelStream()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stream error = %v, want context.Canceled", err)
	}
	if model.wasCanceled() {
		t.Fatal("closing the event stream canceled the parked model work")
	}
	model.unblock()
	awaitOperation(t, c, session, "op-parked", protocol.OperationStatusSuccess)
	if model.wasCanceled() {
		t.Fatal("the parked model work was canceled during settlement")
	}
}

// testProjection is the caller-owned, instance-qualified Session projection:
// connecting to a changed instance discards the old projection before the new
// owner's read is accepted, and a read naming any instance other than the
// accepted owner — an older owner's delayed response — is rejected before any
// revision comparison. Within one accepted instance an older revision never
// overwrites the accepted one.
type testProjection struct {
	instance string
	revision protocol.SessionRevision
}

// switchOwner records the newly connected owner and discards the old
// projection.
func (p *testProjection) switchOwner(instance string) {
	p.instance, p.revision = instance, protocol.SessionRevision{}
}

// accept applies the caller's instance-first, revision-second rule and
// reports whether the candidate was accepted.
func (p *testProjection) accept(instance string, revision protocol.SessionRevision) bool {
	if instance != p.instance {
		return false
	}
	if p.revision.DurableRevision != "" && revisionOlder(revision, p.revision) {
		return false
	}
	p.revision = revision
	return true
}

// revisionOlder compares two same-instance revisions as decimal pairs.
func revisionOlder(a, b protocol.SessionRevision) bool {
	ad, al := parseRevision(a.DurableRevision), parseRevision(a.LocalRevision)
	bd, bl := parseRevision(b.DurableRevision), parseRevision(b.LocalRevision)
	return ad < bd || (ad == bd && al < bl)
}

// parseRevision parses one decimal revision string emitted by the server.
func parseRevision(value string) uint64 {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		panic(fmt.Sprintf("revision %q: %v", value, err))
	}
	return parsed
}

// TestCallerProjectionStaleOwner holds an already received higher-revision
// hydration from one real owner, accepts a new owner's real read, then
// delivers the old value late and proves instance-first rejection.
func TestCallerProjectionStaleOwner(t *testing.T) {
	oldRuntime, _ := newRealRuntime(t, "http://127.0.0.1:1/v1")
	_, oldRecord := openProtocol(t, oldRuntime)
	oldClient := connectFixture(t, oldRecord)
	oldSession := createSession(t, oldClient, filepath.Join(t.TempDir(), "workspace-old"))
	archiveSession(t, oldClient, oldSession)
	reopenSession(t, oldClient, oldSession)
	held := hydrateSession(t, oldClient, oldSession)
	// A real owner closes its active sockets: the held hydration was already
	// received before the owner goes away.
	if err := oldRuntime.Close(context.Background()); err != nil {
		t.Fatalf("close old owner: %v", err)
	}

	newRuntime, _ := newRealRuntime(t, "http://127.0.0.1:1/v1")
	_, newRecord := openProtocol(t, newRuntime)
	newClient := connectFixture(t, newRecord)
	newSession := createSession(t, newClient, filepath.Join(t.TempDir(), "workspace-new"))
	accepted := hydrateSession(t, newClient, newSession)

	if !revisionOlder(accepted.SessionRevision, held.SessionRevision) {
		t.Fatalf("held old revision %+v is not numerically newer than the accepted %+v", held.SessionRevision, accepted.SessionRevision)
	}
	projection := &testProjection{}
	projection.switchOwner(held.SessionRevision.InstanceId)
	if !projection.accept(held.SessionRevision.InstanceId, held.SessionRevision) {
		t.Fatal("the old owner's held read was not accepted")
	}
	// The new owner is accepted: the old projection is discarded first.
	projection.switchOwner(accepted.SessionRevision.InstanceId)
	if !projection.accept(accepted.SessionRevision.InstanceId, accepted.SessionRevision) {
		t.Fatal("the new owner's read was not accepted")
	}
	if projection.accept(held.SessionRevision.InstanceId, held.SessionRevision) {
		t.Fatal("a delayed old-owner hydration replaced the accepted projection")
	}
	if projection.instance != accepted.SessionRevision.InstanceId {
		t.Fatalf("projection instance = %q, want the new owner %q", projection.instance, accepted.SessionRevision.InstanceId)
	}
	older := accepted.SessionRevision
	older.DurableRevision = "0"
	older.LocalRevision = "0"
	if projection.accept(older.InstanceId, older) {
		t.Fatal("an older same-instance revision overwrote the accepted one")
	}
	newer := accepted.SessionRevision
	newer.DurableRevision = strconv.FormatUint(parseRevision(accepted.SessionRevision.DurableRevision)+1, 10)
	if !projection.accept(newer.InstanceId, newer) {
		t.Fatal("a newer same-instance revision was rejected")
	}
}
