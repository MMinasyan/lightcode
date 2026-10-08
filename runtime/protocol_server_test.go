package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/protocol"
)

// The mounted-protocol test fixtures: every row drives the real generated Go
// client or a raw HTTP request against the real loopback listener of one
// attached protocol server, over the existing isolated owner, store, model,
// and preparation fixtures. No server-specific production hooks were added.

// openProtocolServer attaches the isolated protocol listener to one opened
// Runtime and pins its endpoint, identity, and credential shapes.
func openProtocolServer(t *testing.T, r *Runtime) *ProtocolServer {
	t.Helper()
	ps, err := r.OpenProtocol(context.Background())
	if err != nil {
		t.Fatalf("OpenProtocol: %v", err)
	}
	host, _, err := net.SplitHostPort(strings.TrimPrefix(ps.Endpoint(), "http://"))
	if err != nil {
		t.Fatalf("endpoint %q is not host:port: %v", ps.Endpoint(), err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("endpoint host %q is not a loopback address", host)
	}
	if len(ps.instance) != 32 || strings.Trim(ps.instance, "0123456789abcdef") != "" {
		t.Fatalf("instance identity %q is not 32 lowercase hex", ps.instance)
	}
	if len(ps.credential) != 64 || strings.Trim(ps.credential, "0123456789abcdef") != "" {
		t.Fatal("credential is not 64 lowercase hex")
	}
	return ps
}

// protocolClient builds the generated Go client with the server's bearer
// credential on every request.
func protocolClient(t *testing.T, ps *ProtocolServer) *protocol.ClientWithResponses {
	t.Helper()
	client, err := protocol.NewClientWithResponses(ps.Endpoint(), protocol.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+ps.credential)
		return nil
	}))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return client
}

// rawProtocol performs one HTTP request against the mounted server with the
// given bearer credential (empty means none) and no redirect following, so
// transport-level redirects stay observable.
func rawProtocol(t *testing.T, method, target, bearer, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       30 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return resp
}

// decodeProtocolError decodes one schema-defined typed error body strictly.
func decodeProtocolError(t *testing.T, resp *http.Response) protocol.Error {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s response body: %v", resp.Request.Method, err)
	}
	body := string(data)
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	var typed protocol.Error
	if err := decoder.Decode(&typed); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("error body %q is not exactly one JSON document", body)
	}
	if !typed.Code.Valid() {
		t.Fatalf("error body %q carries an unknown code", body)
	}
	return typed
}

// protocolTarget builds one absolute URL against the mounted server.
func protocolTarget(ps *ProtocolServer, path string) string {
	return ps.Endpoint() + path
}

// textContentPart builds one generated text content part, optionally with an
// opaque extra object carried by its raw values.
func textContentPart(t *testing.T, text string, extra map[string]json.RawMessage) protocol.ContentPart {
	t.Helper()
	var part protocol.ContentPart
	var optional *protocol.JSONObject
	if extra != nil {
		optional = &extra
	}
	if err := part.FromTextPart(protocol.TextPart{Text: text, Extra: optional}); err != nil {
		t.Fatalf("build text part: %v", err)
	}
	return part
}

// imageURLContentPart builds one generated image_url content part.
func imageURLContentPart(t *testing.T, url string) protocol.ContentPart {
	t.Helper()
	var part protocol.ContentPart
	if err := part.FromImageURLPart(protocol.ImageURLPart{Url: url}); err != nil {
		t.Fatalf("build image_url part: %v", err)
	}
	return part
}

// opaqueContentPart builds one generated opaque content part.
func opaqueContentPart(t *testing.T, wireType string) protocol.ContentPart {
	t.Helper()
	var part protocol.ContentPart
	if err := part.FromOpaquePart(protocol.OpaquePart{OpaqueWireType: wireType}); err != nil {
		t.Fatalf("build opaque part: %v", err)
	}
	return part
}

// ptrTo returns a pointer to one value.
func ptrTo[T any](value T) *T { return &value }

// sseStream is one open events stream read through its raw response body.
type sseStream struct {
	resp   *http.Response
	reader *bufio.Reader
}

// openSSEStream opens one authenticated events stream on the mounted server.
func openSSEStream(t *testing.T, ps *ProtocolServer) *sseStream {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, protocolTarget(ps, "/v1/events"), nil)
	if err != nil {
		t.Fatalf("build events request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+ps.credential)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("open events stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("events stream status = %d, want 200", resp.StatusCode)
	}
	if media := resp.Header.Get("Content-Type"); media != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("events content type = %q, want text/event-stream", media)
	}
	return &sseStream{resp: resp, reader: bufio.NewReader(resp.Body)}
}

// close releases the stream's connection.
func (s *sseStream) close() { _ = s.resp.Body.Close() }

// readSSEFrame reads until one complete notification frame — skipping
// keepalive comments — and returns the frame's Event JSON, or reports the
// stream's end.
func (s *sseStream) readSSEFrame() (json.RawMessage, bool, error) {
	var data []byte
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return nil, false, err
		}
		switch {
		case strings.HasPrefix(line, ":"):
			// keepalive comment
		case strings.HasPrefix(line, "event:"):
			if strings.TrimSpace(strings.TrimPrefix(line, "event:")) != "notification" {
				return nil, false, fmt.Errorf("frame event %q is not notification", line)
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line, "data:")...)
		case strings.TrimSpace(line) == "":
			if data != nil {
				return json.RawMessage(bytes.TrimSpace(data)), true, nil
			}
		default:
			return nil, false, fmt.Errorf("unexpected stream line %q", line)
		}
	}
}

// readUntilEvent reads events until one matches the discriminator, bounding
// the search by a maximum frame count so a missing kind fails fast.
func readUntilEvent(t *testing.T, stream *sseStream, kind string, bound int) map[string]json.RawMessage {
	t.Helper()
	for i := 0; i < bound; i++ {
		frame, ok, err := stream.readSSEFrame()
		if err != nil || !ok {
			t.Fatalf("search for %s event: ok=%v err=%v after %d frames", kind, ok, err, i)
		}
		var member struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(frame, &member); err != nil {
			t.Fatalf("decode candidate frame %s: %v", frame, err)
		}
		if member.Kind != kind {
			continue
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(frame, &object); err != nil {
			t.Fatalf("decode %s frame %s: %v", kind, frame, err)
		}
		return object
	}
	t.Fatalf("no %s event within %d frames", kind, bound)
	return nil
}

// eventRevisionInstance extracts the instance identity from one revisioned
// event frame's revision object.
func eventRevisionInstance(t *testing.T, revisionRaw json.RawMessage, member string) string {
	t.Helper()
	var revision map[string]json.RawMessage
	if err := json.Unmarshal(revisionRaw, &revision); err != nil {
		t.Fatalf("decode revision %s: %v", revisionRaw, err)
	}
	var instance string
	if err := json.Unmarshal(revision["instance_id"], &instance); err != nil {
		t.Fatalf("decode revision instance: %v", err)
	}
	if instance == "" {
		t.Fatalf("revision %s carries an empty instance identity", member)
	}
	return instance
}

// assertQualifiedInstance pins one revision object's instance identity.
func assertQualifiedInstance(t *testing.T, which string, revision protocol.SessionRevision, instance string) {
	t.Helper()
	if revision.InstanceId != instance {
		t.Fatalf("%s session revision instance = %q, want the minted %q", which, revision.InstanceId, instance)
	}
}

// TestProtocolServerSessionFamily mounts the complete server and drives the
// Session and stream operation families end to end with the generated Go
// client over both stores: health, creation, reads with instance-qualified
// revisions, the submit dispositions, the control commands, the lifecycle
// transitions, deletion with its typed absence, workspace navigation, the
// bounded file view, warnings, and one qualified event frame.
func TestProtocolServerSessionFamily(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		ctx := context.Background()

		health, err := client.GetHealthWithResponse(ctx)
		if err != nil {
			t.Fatalf("GetHealth: %v", err)
		}
		if health.JSON200 == nil || health.HTTPResponse.StatusCode != http.StatusOK {
			t.Fatalf("health = (%d, %v), want the 200 typed body", health.HTTPResponse.StatusCode, health.JSON200)
		}
		if health.JSON200.InstanceId != ps.instance || health.JSON200.ProtocolVersion != protocol.N1 {
			t.Fatalf("health = %+v, want instance %q and protocol version %q", health.JSON200, ps.instance, protocol.N1)
		}

		workspace := filepath.Join(e.home, "mounted")
		created, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: workspace, AgentType: "solo"})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if created.HTTPResponse.StatusCode != http.StatusCreated || created.JSON201 == nil {
			t.Fatalf("create = (%d, %v), want 201 with the typed header", created.HTTPResponse.StatusCode, created.JSON201)
		}
		session := created.JSON201.SessionId
		assertQualifiedInstance(t, "created header", created.JSON201.SessionRevision, ps.instance)

		header, err := client.GetSessionHydrationWithResponse(ctx, session)
		if err != nil {
			t.Fatalf("GetSessionHydration: %v", err)
		}
		if header.JSON200 == nil || header.JSON200.Session.SessionId != session {
			t.Fatalf("read header = %v, want the created session", header.JSON200)
		}
		assertQualifiedInstance(t, "read header", header.JSON200.Session.SessionRevision, ps.instance)

		// The mounted list route normalizes one relative workspace root
		// before filtering; the literal compare is the forbidden sibling.
		t.Chdir(e.home)
		relative, err := filepath.Rel(e.home, workspace)
		if err != nil {
			t.Fatalf("relative workspace: %v", err)
		}
		listed, err := client.ListSessionsWithResponse(ctx, &protocol.ListSessionsParams{Workspace: relative, Lifecycle: "open"})
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		if listed.JSON200 == nil || len(*listed.JSON200) != 1 || (*listed.JSON200)[0].SessionId != session {
			body, _ := json.Marshal(listed.JSON200)
			t.Fatalf("relative-root list = %s, want exactly the created session", body)
		}
		if (*listed.JSON200)[0].SessionRevision.InstanceId != ps.instance {
			t.Fatalf("list row revision instance = %q, want %q", (*listed.JSON200)[0].SessionRevision.InstanceId, ps.instance)
		}
		// A valid filter matching no session is one 200 with an empty list.
		none, err := client.ListSessionsWithResponse(ctx, &protocol.ListSessionsParams{Workspace: filepath.Join(e.home, "absent"), Lifecycle: "archived"})
		if err != nil {
			t.Fatalf("ListSessions no-match: %v", err)
		}
		if none.JSON200 == nil || *none.JSON200 == nil || len(*none.JSON200) != 0 {
			body, _ := json.Marshal(none.JSON200)
			t.Fatalf("no-match list = %s, want the empty JSON array", body)
		}
		// The filter set is one rule: empty or unknown values fail invalid
		// through the mounted route.
		for name, params := range map[string]protocol.ListSessionsParams{
			"empty workspace":   {Workspace: "", Lifecycle: "open"},
			"empty lifecycle":   {Workspace: workspace, Lifecycle: ""},
			"unknown lifecycle": {Workspace: workspace, Lifecycle: "paused"},
		} {
			refused, err := client.ListSessionsWithResponse(ctx, &params)
			if err != nil {
				t.Fatalf("%s list: %v", name, err)
			}
			if refused.JSONDefault == nil || refused.JSONDefault.Code != protocol.Invalid {
				body, _ := json.Marshal(refused.JSONDefault)
				t.Fatalf("%s list = %s (status %d), want the typed invalid refusal", name, body, refused.HTTPResponse.StatusCode)
			}
		}
		// The mounted route also refuses omitted query members outright.
		for name, query := range map[string]string{
			"omitted workspace":  url.Values{"lifecycle": {"open"}}.Encode(),
			"omitted lifecycle":  url.Values{"workspace": {workspace}}.Encode(),
			"omitted filter set": "",
		} {
			target := protocolTarget(ps, "/v1/sessions")
			if query != "" {
				target += "?" + query
			}
			omitted := rawProtocol(t, http.MethodGet, target, ps.credential, "")
			if omitted.StatusCode != http.StatusBadRequest {
				omitted.Body.Close()
				t.Fatalf("%s list = %d, want the typed invalid refusal 400", name, omitted.StatusCode)
			}
			if typed := decodeProtocolError(t, omitted); typed.Code != protocol.Invalid {
				t.Fatalf("%s list error = %+v, want invalid", name, typed)
			}
		}

		// The submit dispositions over one gated session: admitted, then
		// existing for the same identity, then queued and steering buffers.
		// The one gate channel releases through one OnceFunc, so the normal
		// and deferred releases are idempotent.
		gate := make(chan struct{})
		e.server.setHold(gate)
		release := sync.OnceFunc(func() { close(gate) })
		defer release()
		submit := func(operationID, mode, text string) protocol.SubmitSessionResponse {
			t.Helper()
			response, err := client.SubmitSessionWithResponse(ctx, session, protocol.SubmitRequest{
				OperationId: operationID,
				Mode:        protocol.SubmitRequestMode(mode),
				Content:     []protocol.ContentPart{textContentPart(t, text, nil)},
			})
			if err != nil {
				t.Fatalf("SubmitSession(%s): %v", operationID, err)
			}
			return *response
		}
		admitted := submit("op-1", "regular", "first")
		if admitted.JSON200 == nil || admitted.JSON200.Disposition != protocol.SubmitResultDispositionAdmitted ||
			admitted.JSON200.Operation == nil || admitted.JSON200.Operation.OperationId != "op-1" {
			body, _ := json.Marshal(admitted.JSON200)
			t.Fatalf("admitted submit = %s (status %d), want the admitted op-1", body, admitted.HTTPResponse.StatusCode)
		}
		awaitModelArrival(t, e)
		existing := submit("op-1", "regular", "retry")
		if existing.JSON200 == nil || existing.JSON200.Disposition != protocol.SubmitResultDispositionExisting {
			body, _ := json.Marshal(existing.JSON200)
			t.Fatalf("same-identity submit = %s, want the existing disposition", body)
		}
		queued := submit("op-2", "queued", "later")
		if queued.JSON200 == nil || queued.JSON200.Disposition != protocol.SubmitResultDispositionQueued || queued.JSON200.Operation != nil {
			body, _ := json.Marshal(queued.JSON200)
			t.Fatalf("queued submit = %s, want a buffered queued item with no invented Operation", body)
		}
		steering := submit("op-3", "regular", "mid")
		if steering.JSON200 == nil || steering.JSON200.Disposition != protocol.SubmitResultDispositionSteering {
			body, _ := json.Marshal(steering.JSON200)
			t.Fatalf("steering submit = %s, want the steering disposition", body)
		}

		hydration, err := client.GetSessionHydrationWithResponse(ctx, session)
		if err != nil {
			t.Fatalf("GetSessionHydration: %v", err)
		}
		if hydration.JSON200 == nil {
			t.Fatalf("hydration = %d with no typed body", hydration.HTTPResponse.StatusCode)
		}
		if len(hydration.JSON200.Pending.Steering) != 1 || len(hydration.JSON200.Pending.Queued) != 1 {
			body, _ := json.Marshal(hydration.JSON200.Pending)
			t.Fatalf("pending queues = %s, want one steering and one queued item", body)
		}
		if hydration.JSON200.Usage.Totals.ByModel == nil {
			t.Fatalf("hydration usage totals = %+v, want the present by-model array", hydration.JSON200.Usage.Totals)
		}
		if hydration.JSON200.SelectedModel == nil || *hydration.JSON200.SelectedModel != "prov/m" {
			t.Fatalf("hydration selected model = %v, want the configured prov/m", hydration.JSON200.SelectedModel)
		}
		if hydration.JSON200.Usage.Context.ContextWindow != 262144 {
			t.Fatalf("hydration context window = %d, want the running Operation's captured catalog window", hydration.JSON200.Usage.Context.ContextWindow)
		}
		assertQualifiedInstance(t, "hydration", hydration.JSON200.SessionRevision, ps.instance)
		if hydration.JSON200.Session.SessionId != session || hydration.JSON200.Session.SessionRevision.InstanceId != ps.instance {
			t.Fatalf("hydration's nested session revision instance = %q, want %q", hydration.JSON200.Session.SessionRevision.InstanceId, ps.instance)
		}
		if hydration.JSON200.ConfigurationRevision.InstanceId != ps.instance {
			t.Fatalf("hydration's configuration revision instance = %q", hydration.JSON200.ConfigurationRevision.InstanceId)
		}
		if hydration.JSON200.WarningsRevision.InstanceId != ps.instance {
			t.Fatalf("hydration's warnings revision instance = %q", hydration.JSON200.WarningsRevision.InstanceId)
		}

		// An unresolvable Agent type yields the required null selection over
		// the mounted wire, never a fallback model.
		ghostSession, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: workspace, AgentType: "ghost"})
		if err != nil || ghostSession.JSON201 == nil {
			t.Fatalf("create unresolved-type session: %v", err)
		}
		ghostHydration, err := client.GetSessionHydrationWithResponse(ctx, ghostSession.JSON201.SessionId)
		if err != nil || ghostHydration.JSON200 == nil {
			t.Fatalf("hydration of an unresolved type = (%d, %v), want a typed body", ghostHydration.HTTPResponse.StatusCode, err)
		}
		if ghostHydration.JSON200.SelectedModel != nil {
			t.Fatalf("unresolved-type selected model = %v, want null", ghostHydration.JSON200.SelectedModel)
		}

		groups, err := client.GetSessionCodeSnapshotsWithResponse(ctx, session)
		if err != nil {
			t.Fatalf("GetSessionCodeSnapshots: %v", err)
		}
		if groups.JSON200 == nil || *groups.JSON200 == nil || len(*groups.JSON200) != 0 {
			body, _ := json.Marshal(groups.JSON200)
			t.Fatalf("empty code snapshot groups = %s, want the empty JSON array", body)
		}

		// The interrupt control answers 204 while the gated Operation runs;
		// the queued and steering buffers then drain and the whole Session
		// converges before the fork addresses it.
		if response, err := client.InterruptSessionWithResponse(ctx, session); err != nil || response.HTTPResponse.StatusCode != http.StatusNoContent {
			t.Fatalf("interrupt = (%d, %v), want 204", response.HTTPResponse.StatusCode, err)
		}
		release()
		// The drained buffers settle deterministically under their own
		// identities before the source Session is addressed as idle.
		awaitOperation(t, r, session, "op-1", harness.OperationInterruption)
		awaitOperation(t, r, session, "op-2", harness.OperationSuccess)
		awaitOperation(t, r, session, "op-3", harness.OperationSuccess)
		awaitIdleSession(t, r, session)

		// The boundary item id of the committed first input resolves through
		// the mounted fork route on the now-idle source Session.
		snap := snapshotThroughRuntime(t, r, session)
		boundary := projectItemID(session, snap.Facts[0].EntryID)
		forked, err := client.ForkSessionWithResponse(ctx, session, protocol.ForkRequest{
			BoundaryItemId: boundary,
			OperationId:    "fork-1",
			Content:        []protocol.ContentPart{textContentPart(t, "forked", nil)},
		})
		if err != nil {
			t.Fatalf("ForkSession: %v", err)
		}
		if forked.JSON200 == nil || forked.JSON200.Operation.OperationId != "fork-1" || forked.JSON200.Session.SessionId == session {
			body, _ := json.Marshal(forked.JSON200)
			t.Fatalf("fork = %s (status %d, %v), want the forked session and its admitted Operation", body, forked.HTTPResponse.StatusCode, forked.JSONDefault)
		}
		assertQualifiedInstance(t, "fork header", forked.JSON200.Session.SessionRevision, ps.instance)
		awaitIdleSession(t, r, forked.JSON200.Session.SessionId)

		agentType, err := client.SetSessionAgentTypeWithResponse(ctx, session, protocol.SetSessionAgentTypeRequest{AgentType: "worker"})
		if err != nil || agentType.JSON200 == nil {
			t.Fatalf("agent-type change = (%v, %+v)", err, agentType.JSON200)
		} else if agentType.JSON200.AgentType != "worker" {
			t.Fatalf("agent-type header = %q, want worker", agentType.JSON200.AgentType)
		}

		history, err := client.GetSessionHistoryWithResponse(ctx, session, &protocol.GetSessionHistoryParams{})
		if err != nil {
			t.Fatalf("GetSessionHistory: %v", err)
		}
		if history.JSON200 == nil || len(history.JSON200.Items) == 0 {
			body, _ := json.Marshal(history.JSON200)
			t.Fatalf("history = %s, want the committed input items", body)
		}
		assertQualifiedInstance(t, "history", history.JSON200.SessionRevision, ps.instance)

		// The stop control answers 204 after its convergence wait on one
		// idle root Session, and the compact command admits its Operation on
		// the same idle discipline.
		other := projectionSession(t, r, filepath.Join(e.home, "other"), "solo").Identity.SessionID
		if response, err := client.StopSessionWithResponse(ctx, other); err != nil || response.HTTPResponse.StatusCode != http.StatusNoContent {
			t.Fatalf("stop = (%d, %v), want 204", response.HTTPResponse.StatusCode, err)
		}
		compacted, err := client.CompactSessionWithResponse(ctx, other, protocol.CompactRequest{OperationId: "compact-1"})
		if err != nil {
			t.Fatalf("CompactSession: %v", err)
		}
		if compacted.JSON200 == nil || compacted.JSON200.OperationId != "compact-1" {
			body, _ := json.Marshal(compacted.JSON200)
			t.Fatalf("compact = %s, want the admitted compact Operation", body)
		}
		awaitIdleSession(t, r, other)

		workspaces, err := client.ListWorkspacesWithResponse(ctx)
		if err != nil {
			t.Fatalf("ListWorkspaces: %v", err)
		}
		if workspaces.JSON200 == nil || len(*workspaces.JSON200) < 2 {
			body, _ := json.Marshal(workspaces.JSON200)
			t.Fatalf("workspaces = %s, want the distinct mounted and other roots", body)
		}

		view := filepath.Join(e.home, "view.txt")
		if err := os.WriteFile(view, []byte("seen\n"), 0o600); err != nil {
			t.Fatalf("write workspace file: %v", err)
		}
		read, err := client.ReadWorkspaceFileWithResponse(ctx, protocol.ReadFileRequest{Workspace: e.home, Path: "view.txt"})
		if err != nil {
			t.Fatalf("ReadWorkspaceFile: %v", err)
		}
		if read.JSON200 == nil || read.JSON200.Content != "seen\n" || read.JSON200.Truncated {
			body, _ := json.Marshal(read.JSON200)
			t.Fatalf("file view = %s, want the bounded content", body)
		}

		warnings, err := client.GetWarningsWithResponse(ctx)
		if err != nil {
			t.Fatalf("GetWarnings: %v", err)
		}
		if warnings.JSON200 == nil || warnings.JSON200.WarningsRevision.InstanceId != ps.instance {
			t.Fatalf("warnings revision instance = %+v", warnings.JSON200)
		}

		archived, err := client.ArchiveSessionWithResponse(ctx, other)
		if err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if archived.JSON200 == nil || archived.JSON200.Lifecycle != "archived" {
			body, _ := json.Marshal(archived.JSON200)
			t.Fatalf("archive = %s, want the archived header", body)
		}
		reopened, err := client.ReopenSessionWithResponse(ctx, other)
		if err != nil {
			t.Fatalf("ReopenSession: %v", err)
		}
		if reopened.JSON200 == nil || reopened.JSON200.Lifecycle != "open" {
			body, _ := json.Marshal(reopened.JSON200)
			t.Fatalf("reopen = %s, want the reopened header", body)
		}
		if response, err := client.ArchiveSessionWithResponse(ctx, other); err != nil {
			t.Fatalf("re-archive: %v", err)
		} else if response.JSON200 == nil {
			t.Fatalf("re-archive = %d", response.HTTPResponse.StatusCode)
		}
		deleted, err := client.DeleteSessionWithResponse(ctx, other)
		if err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		if deleted.HTTPResponse.StatusCode != http.StatusNoContent {
			t.Fatalf("delete = %d, want 204", deleted.HTTPResponse.StatusCode)
		}
		if missing, err := client.GetSessionHydrationWithResponse(ctx, other); err != nil || missing.JSON200 != nil {
			t.Fatalf("deleted session read = (%d, %+v), want the typed not-found", missing.HTTPResponse.StatusCode, missing.JSON200)
		} else if missing.JSONDefault == nil || missing.JSONDefault.Code != protocol.NotFound {
			t.Fatalf("deleted session read error = %+v, want not_found", missing.JSONDefault)
		}

		// One qualified session_changed frame: one fresh queued submit
		// publishes one live invalidation for this late stream.
		stream := openSSEStream(t, ps)
		defer stream.close()
		if _, err := r.submitSession(ctx, session, protocol.SubmitRequest{
			OperationId: "op-4",
			Mode:        protocol.SubmitRequestModeQueued,
			Content:     []protocol.ContentPart{textContentPart(t, "stream", nil)},
		}); err != nil {
			t.Fatalf("stream submit: %v", err)
		}
		frame := readUntilEvent(t, stream, "session_changed", 16)
		if instance := eventRevisionInstance(t, frame["session_revision"], "session_changed"); instance != ps.instance {
			t.Fatalf("stream session revision instance = %q, want %q", instance, ps.instance)
		}
	})
}

// TestProtocolServerHistoryRoundTripsOpaqueNumbers pins the shared decode
// boundary's exact-number and opaque-value rules through the mounted server:
// a content part's extra carries a non-float-exact integer and a null member,
// and the committed history returns both exactly — the boundary never
// rewrites opaque JSON, and the null and exact numbers inside the opaque map
// stay valid while the declared members around them stay closed.
func TestProtocolServerHistoryRoundTripsOpaqueNumbers(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		ctx := context.Background()

		workspace := filepath.Join(e.home, "exact")
		created, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: workspace, AgentType: "solo"})
		if err != nil || created.JSON201 == nil {
			t.Fatalf("create: %v", err)
		}
		session := created.JSON201.SessionId
		submitted, err := client.SubmitSessionWithResponse(ctx, session, protocol.SubmitRequest{
			OperationId: "op-1",
			Mode:        protocol.SubmitRequestModeRegular,
			Content: []protocol.ContentPart{textContentPart(t, "exact", protocol.JSONObject{
				"n":           json.RawMessage("9007199254740993"),
				"nullable":    json.RawMessage("null"),
				"instance_id": json.RawMessage(`"opaque-identity"`),
			})},
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		if submitted.JSON200 == nil || submitted.JSON200.Disposition != protocol.SubmitResultDispositionAdmitted {
			body, _ := json.Marshal(submitted.JSON200)
			t.Fatalf("submit = %s (status %d)", body, submitted.HTTPResponse.StatusCode)
		}
		awaitOperation(t, r, session, "op-1", harness.OperationSuccess)

		history, err := client.GetSessionHistoryWithResponse(ctx, session, &protocol.GetSessionHistoryParams{})
		if err != nil || history.JSON200 == nil {
			t.Fatalf("history: %v", err)
		}
		var input *protocol.InputItem
		for _, item := range history.JSON200.Items {
			if value, err := item.AsInputItem(); err == nil {
				input = &value
				break
			}
		}
		if input == nil || len(input.Content) == 0 {
			t.Fatalf("history = %d items with no input, want the submitted input item", len(history.JSON200.Items))
		}
		part, err := input.Content[0].AsTextPart()
		if err != nil {
			t.Fatalf("decode history part: %v", err)
		}
		if part.Extra == nil {
			t.Fatal("history part lost its extra object")
		}
		if n, ok := (*part.Extra)["n"]; !ok || string(bytes.TrimSpace(n)) != "9007199254740993" {
			t.Fatalf("extra n = %s, want the exact integer 9007199254740993", n)
		}
		if nullable, ok := (*part.Extra)["nullable"]; !ok || !isJSONNull(nullable) {
			t.Fatalf("extra nullable = %s, want the preserved opaque null", nullable)
		}
		// The opaque member named instance_id is never rewritten: the
		// envelope's revision carries the minted identity while the extra's
		// own member stays exactly as submitted.
		if opaque, ok := (*part.Extra)["instance_id"]; !ok || string(bytes.TrimSpace(opaque)) != `"opaque-identity"` {
			t.Fatalf("extra instance_id = %s, want the untouched opaque member", opaque)
		}
		if history.JSON200.SessionRevision.InstanceId != ps.instance {
			t.Fatalf("history revision instance = %q, want the minted %q", history.JSON200.SessionRevision.InstanceId, ps.instance)
		}
	})
}

// readRawBody reads one response's complete raw bytes and closes it.
func readRawBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read raw response body: %v", err)
	}
	return string(data)
}

// TestProtocolServerCredentialFlowRawWireSecretFree scans the raw mounted HTTP
// success and refusal bodies, the raw notification frames, and the real
// process stderr sink across a successful managed-key publication and a real
// credential-bearing refusal. Typed re-marshalling and in-process events are
// not substitutes for these raw oracles.
func TestProtocolServerCredentialFlowRawWireSecretFree(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		reserveEnvKey(t, "RAW_SCAN_KEY")
		reserveEnvKey(t, "RAW_NUL_KEY")
		const secret = "sk-raw-wire-secret"
		const nulSecret = "sk-raw\x00-refused"
		stderr := captureSweepStderr(t)
		e := newOwnerEnv(t)
		writeServiceFile(t, agents.PathForConfig(e.configPath), projectionAgentsDocument)
		// One malformed line makes the startup LoadDotEnv diagnostic run, so
		// the captured stderr sink is genuinely exercised before the scan.
		writeDotEnv(t, e.home, "MALFORMED LINE\n")
		r, err := e.open(context.Background(), e.storagePlugin(store))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		stream := openSSEStream(t, ps)
		defer stream.close()

		window := 4096
		// scanFrames reads raw notification frames until one
		// configuration_changed witness, scanning every frame for the key.
		scanFrames := func(what string) {
			t.Helper()
			observed := false
			for i := 0; i < 4; i++ {
				frame, ok, err := stream.readSSEFrame()
				if err != nil || !ok {
					break
				}
				if strings.Contains(string(frame), secret) {
					t.Fatalf("a raw %s notification frame carries the credential value: %s", what, frame)
				}
				if bytes.Contains(frame, []byte(`"configuration_changed"`)) {
					observed = true
					break
				}
			}
			if !observed {
				t.Fatalf("no raw configuration_changed frame was observed for %s", what)
			}
		}
		createBody, err := json.Marshal(protocol.CreateProviderRequest{
			Id: "rawscan",
			Provider: protocol.ProviderEdit{
				BaseUrl:   ptrTo("https://rawscan.test/v1"),
				ApiKeyEnv: ptrTo("RAW_SCAN_KEY"),
			},
			Models: map[string]protocol.ModelEdit{"m": {ContextWindow: &window}},
			ApiKey: ptrTo(secret),
		})
		if err != nil {
			t.Fatalf("encode create: %v", err)
		}

		// Successful credential publication: raw response body and witness.
		resp := rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/providers"), ps.credential, string(createBody))
		successBody := readRawBody(t, resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(successBody, `"rawscan"`) {
			t.Fatalf("raw create = %d: %s", resp.StatusCode, successBody)
		}
		if strings.Contains(successBody, secret) {
			t.Fatalf("the raw create response carries the credential value: %s", successBody)
		}
		scanFrames("create")

		// Real connect response and its raw witness over the same key.
		connectBody, err := json.Marshal(protocol.ConnectRequest{ApiKey: ptrTo(secret)})
		if err != nil {
			t.Fatalf("encode connect: %v", err)
		}
		connectResp := rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/providers/connect?provider_id=rawscan"), ps.credential, string(connectBody))
		connectBytes := readRawBody(t, connectResp)
		if connectResp.StatusCode != http.StatusOK || !strings.Contains(connectBytes, `"connected":true`) || !strings.Contains(connectBytes, `"key_source":"managed"`) {
			t.Fatalf("raw connect = %d: %s", connectResp.StatusCode, connectBytes)
		}
		if strings.Contains(connectBytes, secret) {
			t.Fatalf("the raw connect response carries the credential value: %s", connectBytes)
		}
		scanFrames("connect")

		// Real disconnect response and its raw witness over the same key.
		disconnectResp := rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/providers/disconnect?provider_id=rawscan"), ps.credential, "")
		disconnectBytes := readRawBody(t, disconnectResp)
		if disconnectResp.StatusCode != http.StatusOK || !strings.Contains(disconnectBytes, `"connected":false`) {
			t.Fatalf("raw disconnect = %d: %s", disconnectResp.StatusCode, disconnectBytes)
		}
		if strings.Contains(disconnectBytes, secret) {
			t.Fatalf("the raw disconnect response carries the credential value: %s", disconnectBytes)
		}
		scanFrames("disconnect")

		// Real credential-bearing refusal: the NUL value fails the native
		// env-value preflight after the owning config write and its rollback,
		// and the raw typed refusal body must not echo it.
		refusalBody, err := json.Marshal(protocol.CreateProviderRequest{
			Id: "rawnul",
			Provider: protocol.ProviderEdit{
				BaseUrl:   ptrTo("https://rawnul.test/v1"),
				ApiKeyEnv: ptrTo("RAW_NUL_KEY"),
			},
			Models: map[string]protocol.ModelEdit{"m": {ContextWindow: &window}},
			ApiKey: ptrTo(nulSecret),
		})
		if err != nil {
			t.Fatalf("encode refusal: %v", err)
		}
		refused := rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/providers"), ps.credential, string(refusalBody))
		refusalBytes := readRawBody(t, refused)
		if refused.StatusCode == http.StatusOK || !strings.Contains(refusalBytes, `"configuration"`) {
			t.Fatalf("raw refusal = %d: %s", refused.StatusCode, refusalBytes)
		}
		escaped := strings.ReplaceAll(nulSecret, "\x00", `\u0000`)
		if strings.Contains(refusalBytes, nulSecret) || strings.Contains(refusalBytes, escaped) {
			t.Fatalf("the raw refusal body carries the credential value: %s", refusalBytes)
		}

		// The real process logging sink exercised by the flow: the startup
		// diagnostic ran, and no credential value reached the sink.
		logged := stderr()
		if !strings.Contains(logged, "skipping malformed line") {
			t.Fatalf("the startup logging path did not run; captured stderr = %q", logged)
		}
		if strings.Contains(logged, secret) || strings.Contains(logged, nulSecret) {
			t.Fatalf("stderr carries a credential value: %q", logged)
		}
	})
}

// TestProtocolServerDiscoverySuppliedKey drives the unpersisted discovery
// read's credential input through the mounted server: the generated client
// and the raw wire both carry the supplied key's exact value on the
// transient fetch — never the externally owned reference's value — the
// fallback row uses the reference's captured value, and no response byte,
// notification frame, or persisted state carries a credential.
func TestProtocolServerDiscoverySuppliedKey(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		const pasted = "sk-mounted-discovery-key"
		const external = "sk-mounted-external-value"
		t.Setenv("MOUNTED_DISCOVERY_KEY", external)
		r, _ := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		stream := openSSEStream(t, ps)
		defer stream.close()
		ctx := context.Background()

		before, err := client.GetConfigurationWithResponse(ctx)
		if err != nil || before.JSON200 == nil {
			t.Fatalf("GetConfiguration: %v", err)
		}

		var mu sync.Mutex
		var auths []string
		discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			auths = append(auths, req.Header.Get("Authorization"))
			mu.Unlock()
			if req.Header.Get("Authorization") == "Bearer "+pasted {
				_, _ = w.Write([]byte(`{"data":[{"id":"stub/m","name":"Stub","context_window":4096,"max_output_tokens":1024}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer discovery.Close()

		// Pre-save pasted-key discovery through the generated client: the
		// supplied value binds the transient fetch over the externally
		// owned reference.
		candidates, err := client.DiscoverProviderCandidatesWithResponse(ctx, protocol.DiscoveryRequest{
			BaseUrl:   discovery.URL + "/v1",
			ApiKeyEnv: "MOUNTED_DISCOVERY_KEY",
			ApiKey:    ptrTo(pasted),
		})
		if err != nil || candidates.JSON200 == nil || len(*candidates.JSON200) != 1 || (*candidates.JSON200)[0].Id != "stub/m" {
			body, _ := json.Marshal(candidates)
			t.Fatalf("supplied-key discovery = %v %s, want the pasted key's candidate", err, body)
		}

		// The fallback row: with no supplied key the reference's captured
		// value decides — the gate answers it with an empty list, the
		// ordinary typed fetch outcome, whose body carries no credential.
		fallback, err := client.DiscoverProviderCandidatesWithResponse(ctx, protocol.DiscoveryRequest{
			BaseUrl:   discovery.URL + "/v1",
			ApiKeyEnv: "MOUNTED_DISCOVERY_KEY",
		})
		if err != nil || fallback.JSON200 != nil || fallback.JSONDefault == nil {
			body, _ := json.Marshal(fallback)
			t.Fatalf("fallback discovery = %v %s, want the empty answer's typed refusal", err, body)
		}
		if refusal, merr := json.Marshal(fallback.JSONDefault); merr != nil || strings.Contains(string(refusal), pasted) || strings.Contains(string(refusal), external) {
			t.Fatalf("the fallback refusal body carries a credential value: (%s, %v)", refusal, merr)
		}

		// The raw wire row: the strict body decode accepts the supplied
		// key, the answer carries the candidate, and no response byte
		// carries a credential value.
		rawBody := `{"base_url":"` + discovery.URL + `/v1","api_key_env":"MOUNTED_DISCOVERY_KEY","api_key":"` + pasted + `"}`
		raw := rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/providers/discover"), ps.credential, rawBody)
		rawBytes := readRawBody(t, raw)
		if raw.StatusCode != http.StatusOK || !strings.Contains(rawBytes, `"stub/m"`) {
			t.Fatalf("raw supplied-key discovery = %d: %s", raw.StatusCode, rawBytes)
		}
		if strings.Contains(rawBytes, pasted) || strings.Contains(rawBytes, external) {
			t.Fatalf("the raw discovery response carries a credential value: %s", rawBytes)
		}

		// The exact wire credentials per row: the pasted key twice, with
		// the captured external value between them.
		wantAuths := []string{"Bearer " + pasted, "Bearer " + external, "Bearer " + pasted}
		mu.Lock()
		wireAuths := append([]string(nil), auths...)
		mu.Unlock()
		if !reflect.DeepEqual(wireAuths, wantAuths) {
			t.Fatalf("discovery wire credentials = %v, want %v", wireAuths, wantAuths)
		}

		// The reads published nothing: the configuration revision is
		// unchanged, no notification frame carries a credential value, no
		// discovery cache was written, and the externally owned reference
		// is untouched and unmanaged.
		after, err := client.GetConfigurationWithResponse(ctx)
		if err != nil || after.JSON200 == nil {
			t.Fatalf("GetConfiguration after the reads: %v", err)
		}
		if after.JSON200.ConfigurationRevision != before.JSON200.ConfigurationRevision {
			t.Fatalf("the discovery reads advanced the revision from %+v to %+v", before.JSON200.ConfigurationRevision, after.JSON200.ConfigurationRevision)
		}
		frames := make(chan string, 1)
		go func() {
			frame, ok, rerr := stream.readSSEFrame()
			if rerr != nil || !ok {
				return
			}
			frames <- string(frame)
		}()
		select {
		case frame := <-frames:
			t.Fatalf("the discovery reads published a notification frame: %s", frame)
		case <-time.After(2 * time.Second):
		}
		if _, serr := os.Stat(filepath.Join(r.config.loader.Home(), ".lightcode", "cache", "discovery", "custom.json")); !os.IsNotExist(serr) {
			t.Fatalf("the discovery reads wrote a cache file: %v", serr)
		}
		if os.Getenv("MOUNTED_DISCOVERY_KEY") != external || r.managedEnv.IsManaged("MOUNTED_DISCOVERY_KEY") {
			t.Fatalf("external reference state = (%q, %v), want the exact value intact and unmanaged", os.Getenv("MOUNTED_DISCOVERY_KEY"), r.managedEnv.IsManaged("MOUNTED_DISCOVERY_KEY"))
		}
	})
}

// TestProtocolServerConfigurationFamily drives the configuration, model, and
// provider operation families through the mounted server with the generated
// client: reads carry their instance-qualified revision, the mutation
// envelope keeps its required post-state result — null exactly when no
// subject remains — the publication reload makes a hand-edited file
// current while a malformed candidate leaves the previous revision, and the
// discovery read answers candidates from a stub endpoint.
func TestProtocolServerConfigurationFamily(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		// The managed-key connection below writes its key into the process
		// environment for this owner's lifetime; the row clears it again so
		// a later store's owner starts from the same external state.
		defer os.Unsetenv("MOUNTED_CONNECT_KEY")
		r, _ := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		ctx := context.Background()

		configuration, err := client.GetConfigurationWithResponse(ctx)
		if err != nil {
			t.Fatalf("GetConfiguration: %v", err)
		}
		if configuration.JSON200 == nil {
			t.Fatalf("configuration = %d with no typed body", configuration.HTTPResponse.StatusCode)
		}
		if configuration.JSON200.ConfigurationRevision.InstanceId != ps.instance {
			t.Fatalf("configuration revision instance = %q", configuration.JSON200.ConfigurationRevision.InstanceId)
		}

		models, err := client.ListModelsWithResponse(ctx, &protocol.ListModelsParams{All: true})
		if err != nil || models.JSON200 == nil {
			t.Fatalf("ListModels: %v", err)
		}
		if models.JSON200.ConfigurationRevision.InstanceId != ps.instance {
			t.Fatalf("model list revision instance = %q", models.JSON200.ConfigurationRevision.InstanceId)
		}

		providers, err := client.ListProvidersWithResponse(ctx)
		if err != nil || providers.JSON200 == nil {
			t.Fatalf("ListProviders: %v", err)
		}
		if providers.JSON200.ConfigurationRevision.InstanceId != ps.instance {
			t.Fatalf("provider list revision instance = %q", providers.JSON200.ConfigurationRevision.InstanceId)
		}

		detail, err := client.GetProviderDetailWithResponse(ctx, &protocol.GetProviderDetailParams{ProviderId: "prov"})
		if err != nil || detail.JSON200 == nil || detail.JSON200.Provider.Id != "prov" {
			t.Fatalf("GetProviderDetail: %v", err)
		}

		name := "Renamed"
		edited, err := client.UpdateProviderDetailWithResponse(ctx, &protocol.UpdateProviderDetailParams{ProviderId: "prov"}, protocol.UpdateProviderDetailRequest{
			Provider: protocol.ProviderEdit{Name: &name},
		})
		if err != nil {
			t.Fatalf("UpdateProviderDetail: %v", err)
		}
		if edited.JSON200 == nil || edited.JSON200.Result.Name != "Renamed" || edited.JSON200.ConfigurationRevision.InstanceId != ps.instance {
			body, _ := json.Marshal(edited.JSON200)
			t.Fatalf("provider edit = %s, want the renamed post-state under the qualified mutation envelope", body)
		}

		// The unpersisted provider discovery read answers a stub endpoint's
		// candidates without writing anything.
		discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"id":"stub/m","name":"Stub","context_window":4096,"max_output_tokens":1024},{"id":"fresh/m","name":"Fresh","context_window":8192,"max_output_tokens":2048}]}`))
		}))
		defer discovery.Close()
		candidates, err := client.DiscoverProviderCandidatesWithResponse(ctx, protocol.DiscoveryRequest{
			BaseUrl:   discovery.URL + "/v1",
			ApiKeyEnv: "",
		})
		if err != nil {
			t.Fatalf("DiscoverProviderCandidates: %v", err)
		}
		if candidates.JSON200 == nil || len(*candidates.JSON200) != 2 {
			body, _ := json.Marshal(candidates.JSON200)
			t.Fatalf("discovery candidates = %s, want both stub models", body)
		}

		window := 4096
		added, err := client.CreateProviderWithResponse(ctx, protocol.CreateProviderRequest{
			Id: "stub",
			Provider: protocol.ProviderEdit{
				BaseUrl:   ptrTo(discovery.URL + "/v1"),
				Discovery: ptrTo(true),
			},
			Models: map[string]protocol.ModelEdit{
				"stub/m": {Name: ptrTo("Stub"), ContextWindow: &window},
			},
		})
		if err != nil {
			t.Fatalf("CreateProvider: %v", err)
		}
		if added.JSON200 == nil || added.JSON200.Result.Id != "stub" {
			body, _ := json.Marshal(added.JSON200)
			t.Fatalf("provider add = %s (status %d)", body, added.HTTPResponse.StatusCode)
		}

		// The provider's own model discovery read answers the same stub,
		// excluding its already-included usable model.
		modelCandidates, err := client.DiscoverProviderModelCandidatesWithResponse(ctx, &protocol.DiscoverProviderModelCandidatesParams{ProviderId: "stub"})
		if err != nil {
			t.Fatalf("DiscoverProviderModelCandidates: %v", err)
		}
		if modelCandidates.JSON200 == nil || len(*modelCandidates.JSON200) != 1 || (*modelCandidates.JSON200)[0].Id != "fresh/m" {
			body, _ := json.Marshal(modelCandidates.JSON200)
			t.Fatalf("provider model candidates = %s, want only the not-yet-included candidate", body)
		}

		// A keyed provider connects through the mounted route with its
		// write-only key and disconnects again, each projection under the
		// qualified mutation envelope.
		keyedWindow := 4096
		keyed, err := client.CreateProviderWithResponse(ctx, protocol.CreateProviderRequest{
			Id:       "keyed",
			Provider: protocol.ProviderEdit{BaseUrl: ptrTo("https://keyed.test/v1"), ApiKeyEnv: ptrTo("MOUNTED_CONNECT_KEY")},
			Models: map[string]protocol.ModelEdit{
				"keyed/m": {Name: ptrTo("Keyed"), ContextWindow: &keyedWindow},
			},
			ApiKey: ptrTo("mounted-secret"),
		})
		if err != nil || keyed.JSON200 == nil {
			body := []byte(nil)
			status := 0
			if keyed != nil {
				body, status = keyed.Body, keyed.HTTPResponse.StatusCode
			}
			t.Fatalf("keyed provider add: %v (status %d, body %s)", err, status, body)
		}
		if disconnected, err := client.DisconnectProviderWithResponse(ctx, &protocol.DisconnectProviderParams{ProviderId: "keyed"}); err != nil {
			t.Fatalf("DisconnectProvider: %v", err)
		} else if disconnected.JSON200 == nil || disconnected.JSON200.Result.Connected {
			body, _ := json.Marshal(disconnected.JSON200)
			t.Fatalf("disconnect = %s (status %d, raw %s), want the disconnected provider", body, disconnected.HTTPResponse.StatusCode, disconnected.Body)
		}
		connected, err := client.ConnectProviderWithResponse(ctx, &protocol.ConnectProviderParams{ProviderId: "keyed"}, protocol.ConnectRequest{ApiKey: ptrTo("mounted-secret")})
		if err != nil {
			t.Fatalf("ConnectProvider: %v", err)
		}
		if connected.JSON200 == nil || !connected.JSON200.Result.Connected || connected.JSON200.ConfigurationRevision.InstanceId != ps.instance {
			body, _ := json.Marshal(connected.JSON200)
			t.Fatalf("connect = %s (status %d), want the connected provider under the qualified envelope", body, connected.HTTPResponse.StatusCode)
		}

		listed, err := client.ListProviderModelsWithResponse(ctx, &protocol.ListProviderModelsParams{ProviderId: "stub"})
		if err != nil || listed.JSON200 == nil {
			t.Fatalf("ListProviderModels: %v", err)
		}
		if listed.JSON200.ConfigurationRevision.InstanceId != ps.instance {
			t.Fatalf("provider model list revision instance = %q", listed.JSON200.ConfigurationRevision.InstanceId)
		}

		hidden := true
		saved, err := client.UpdateProviderModelWithResponse(ctx, &protocol.UpdateProviderModelParams{ProviderId: "stub", ModelId: "stub/m"}, protocol.UpdateProviderModelRequest{
			Model: protocol.ModelEdit{Hidden: &hidden},
		})
		if err != nil || saved.JSON200 == nil || !saved.JSON200.Result.Hidden {
			t.Fatalf("UpdateProviderModel: %v", err)
		}

		// The absent-override field reset publishes under the shared
		// successful-edit rule: the unchanged provider view returns under the
		// next generation.
		beforeReset, err := client.GetProviderDetailWithResponse(ctx, &protocol.GetProviderDetailParams{ProviderId: "stub"})
		if err != nil || beforeReset.JSON200 == nil {
			t.Fatalf("read before reset: %v", err)
		}
		reset, err := client.ResetProviderFieldWithResponse(ctx, protocol.ProviderField("name"), &protocol.ResetProviderFieldParams{ProviderId: "stub"})
		if err != nil {
			t.Fatalf("ResetProviderField: %v", err)
		}
		if reset.JSON200 == nil {
			body, _ := json.Marshal(reset.JSON200)
			t.Fatalf("provider field reset = %s (status %d, %v)", body, reset.HTTPResponse.StatusCode, reset.JSONDefault)
		}
		if reset.JSON200.Result == nil || !reflect.DeepEqual(*reset.JSON200.Result, beforeReset.JSON200.Provider) {
			body, _ := json.Marshal(reset.JSON200.Result)
			other, _ := json.Marshal(beforeReset.JSON200.Provider)
			t.Fatalf("absent-override reset = %s, want the unchanged view %s", body, other)
		}
		if reset.JSON200.ConfigurationRevision.Generation == beforeReset.JSON200.ConfigurationRevision.Generation {
			t.Fatalf("absent-override reset kept generation %s, want the shared successful-edit publication",
				reset.JSON200.ConfigurationRevision.Generation)
		}

		modelReset, err := client.ResetProviderModelFieldWithResponse(ctx, protocol.ModelField("name"), &protocol.ResetProviderModelFieldParams{ProviderId: "stub", ModelId: "stub/m"})
		if err != nil || modelReset.JSON200 == nil {
			t.Fatalf("ResetProviderModelField: %v", err)
		}

		removed, err := client.DeleteProviderModelWithResponse(ctx, &protocol.DeleteProviderModelParams{ProviderId: "stub", ModelId: "stub/m"})
		if err != nil {
			t.Fatalf("DeleteProviderModel: %v", err)
		}
		if removed.JSON200 == nil || removed.JSON200.Result != nil {
			body, _ := json.Marshal(removed.JSON200)
			t.Fatalf("model deletion = %s, want the null post-state", body)
		}
		if removed.JSON200.ConfigurationRevision.InstanceId != ps.instance {
			t.Fatalf("deletion revision instance = %q", removed.JSON200.ConfigurationRevision.InstanceId)
		}

		deleted, err := client.DeleteProviderDetailWithResponse(ctx, &protocol.DeleteProviderDetailParams{ProviderId: "stub"})
		if err != nil {
			t.Fatalf("DeleteProviderDetail: %v", err)
		}
		if deleted.JSON200 == nil || deleted.JSON200.Result != nil {
			body, _ := json.Marshal(deleted.JSON200)
			t.Fatalf("provider deletion = %s, want the null post-state", body)
		}

		// The settings write returns the published settings under the
		// mutation envelope.
		archiveDays := 30
		settings, err := client.UpdateConfigurationSettingsWithResponse(ctx, protocol.UpdateSettingsRequest{Settings: protocol.Settings{
			Sessions: protocol.SessionsSettings{
				AutoArchive:            true,
				ArchiveAfterDays:       archiveDays,
				DeleteAfterArchiveDays: archiveDays,
			},
			Plugins: protocol.PluginsSettings{},
		}})
		if err != nil {
			t.Fatalf("UpdateConfigurationSettings: %v", err)
		}
		if settings.JSON200 == nil || !settings.JSON200.Result.Sessions.AutoArchive || settings.JSON200.Result.Sessions.ArchiveAfterDays != archiveDays {
			body, _ := json.Marshal(settings.JSON200)
			t.Fatalf("settings write = %s, want the published settings result", body)
		}
		if settings.JSON200.ConfigurationRevision.InstanceId != ps.instance {
			t.Fatalf("settings revision instance = %q", settings.JSON200.ConfigurationRevision.InstanceId)
		}

		// The settings boundary negatives over the mounted generated client:
		// an unknown compiled ID, a malformed document string, and a valid
		// but non-object document each refuse the complete candidate with the
		// typed configuration 422. The existing refusal oracles pin the
		// latest owning bytes, publication generation, warning revision and
		// event silence for every row. The row IDs are the composition's own
		// compiled "core" declaration, so only the document boundary is
		// exercised.
		refusalSub, err := r.Subscribe(64)
		if err != nil {
			t.Fatalf("subscribe for settings refusals: %v", err)
		}
		defer refusalSub.Close()
		refusalFile, refusalGeneration, refusalWarnRev := runtimeMutationBaseline(t, r)
		for _, row := range []struct {
			name    string
			plugins protocol.PluginsSettings
		}{
			{"unknown plugin id", protocol.PluginsSettings{"ghost": `{}`}},
			{"malformed document", protocol.PluginsSettings{"core": `{not json`}},
			{"non-object document", protocol.PluginsSettings{"core": `1`}},
		} {
			response, err := client.UpdateConfigurationSettingsWithResponse(ctx, protocol.UpdateSettingsRequest{Settings: protocol.Settings{
				Sessions: protocol.SessionsSettings{AutoArchive: true, ArchiveAfterDays: archiveDays, DeleteAfterArchiveDays: archiveDays},
				Plugins:  row.plugins,
			}})
			if err != nil {
				t.Fatalf("%s: UpdateConfigurationSettings: %v", row.name, err)
			}
			if response.HTTPResponse.StatusCode != http.StatusUnprocessableEntity || response.JSONDefault == nil || response.JSONDefault.Code != protocol.Configuration {
				body, _ := json.Marshal(response)
				t.Fatalf("%s = %s (status %d), want the typed configuration 422", row.name, body, response.HTTPResponse.StatusCode)
			}
			assertRuntimeMutationRefused(t, r, refusalSub, refusalFile, refusalGeneration, refusalWarnRev,
				fmt.Errorf("mounted settings refusal %s: %w", row.name, ErrConfiguration), ErrConfiguration)
		}

		// The one publication operation makes a real hand-edit current: the
		// config file gains a second model and the agents file selects it,
		// so the next admission's captured selection is the new model under
		// the advanced revision. A malformed candidate leaves the previous
		// revision.
		before, err := client.GetConfigurationWithResponse(ctx)
		if err != nil || before.JSON200 == nil {
			t.Fatalf("read before reload: %v", err)
		}
		configPath := filepath.Join(r.dataDir, "config.json")
		writeServiceFile(t, configPath, `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":4096},"m2":{"name":"M2","context_window":8192}}}}}`)
		writeServiceFile(t, agents.PathForConfig(configPath), `{"solo":{"model":"prov/m2","system_prompt":"simple"},"worker":{"model":"prov/m","system_prompt":"simple"}}`)
		reload, err := client.ReloadConfigurationWithResponse(ctx)
		if err != nil {
			t.Fatalf("ReloadConfiguration: %v", err)
		}
		if reload.JSON200 == nil || reload.JSON200.ConfigurationRevision.InstanceId != ps.instance {
			t.Fatalf("reload revision = %+v", reload.JSON200)
		}
		after, err := client.GetConfigurationWithResponse(ctx)
		if err != nil || after.JSON200 == nil {
			t.Fatalf("read after reload: %v", err)
		}
		if after.JSON200.ConfigurationRevision.Generation == before.JSON200.ConfigurationRevision.Generation {
			t.Fatal("the hand-edited publication did not advance the configuration revision")
		}
		selectionChanged := false
		for _, agent := range after.JSON200.Agents {
			if agent.Name == "solo" && agent.Model == "prov/m2" {
				selectionChanged = true
			}
		}
		if !selectionChanged {
			t.Fatal("the hand-edited agents selection is not current after reload")
		}
		session, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: filepath.Join(r.dataDir, "reload-ws"), AgentType: "solo"})
		if err != nil || session.JSON201 == nil {
			t.Fatalf("create after reload: %v", err)
		}
		submitted, err := client.SubmitSessionWithResponse(ctx, session.JSON201.SessionId, protocol.SubmitRequest{
			OperationId: "op-reload",
			Mode:        protocol.SubmitRequestModeRegular,
			Content:     []protocol.ContentPart{textContentPart(t, "reloaded", nil)},
		})
		if err != nil {
			t.Fatalf("submit after reload: %v", err)
		}
		if submitted.JSON200 == nil || submitted.JSON200.Operation == nil {
			body, _ := json.Marshal(submitted.JSON200)
			t.Fatalf("submit after reload = %s (status %d), want the admitted Operation", body, submitted.HTTPResponse.StatusCode)
		}
		if submitted.JSON200.Operation.Model != "prov/m2" {
			t.Fatalf("next admitted Operation model = %q, want the reloaded selection %q", submitted.JSON200.Operation.Model, "prov/m2")
		}

		malformed := func(body string) {
			t.Helper()
			configPath := filepath.Join(r.dataDir, "config.json")
			original, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}
			writeServiceFile(t, configPath, body)
			defer func() {
				writeServiceFile(t, configPath, string(original))
			}()
			response, err := client.ReloadConfigurationWithResponse(ctx)
			if err != nil {
				t.Fatalf("malformed reload: %v", err)
			}
			if response.JSONDefault == nil || response.JSONDefault.Code != protocol.Configuration {
				t.Fatalf("malformed reload error = %+v, want the typed configuration failure", response.JSONDefault)
			}
			kept, err := client.GetConfigurationWithResponse(ctx)
			if err != nil || kept.JSON200 == nil {
				t.Fatalf("read after malformed reload: %v", err)
			}
			if kept.JSON200.ConfigurationRevision.Generation != after.JSON200.ConfigurationRevision.Generation {
				t.Fatal("a malformed candidate changed the published revision")
			}
		}
		malformed(`{"providers":`)
	})
}

// TestProtocolServerProviderPostStateAndKeyRules pins the post-state and
// credential rules through the mounted generated client: a bundled model
// override's delete reveals the base as the non-null result, a keyed create
// returns its actual binding, a supplied key on an explicitly named external
// variable refuses before any side effect, and no response byte carries a
// key value.
func TestProtocolServerProviderPostStateAndKeyRules(t *testing.T) {
	reserveEnvKey(t, "LIGHTCODE_GENERATED_API_KEY")
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)
	client := protocolClient(t, ps)
	ctx := context.Background()

	// A bundled openrouter model gains a user override, whose delete then
	// reveals the bundled base as the required non-null post-state.
	bundledModel := bundledCatalogModel(t, r, "openrouter")
	name := "Overridden"
	saved, err := client.UpdateProviderModelWithResponse(ctx, &protocol.UpdateProviderModelParams{ProviderId: "openrouter", ModelId: bundledModel}, protocol.UpdateProviderModelRequest{
		Model: protocol.ModelEdit{Name: &name},
	})
	if err != nil || saved.JSON200 == nil || saved.JSON200.Result == nil || saved.JSON200.Result.Name != name {
		body, _ := json.Marshal(saved.JSON200)
		t.Fatalf("bundled override = %s (%v), want the landed override", body, err)
	}
	revealed, err := client.DeleteProviderModelWithResponse(ctx, &protocol.DeleteProviderModelParams{ProviderId: "openrouter", ModelId: bundledModel})
	if err != nil || revealed.JSON200 == nil || revealed.JSON200.Result == nil ||
		revealed.JSON200.Result.Id != bundledModel || revealed.JSON200.Result.Name == name ||
		revealed.JSON200.Result.Source != protocol.ModelSourceBundled {
		body, _ := json.Marshal(revealed.JSON200)
		t.Fatalf("override delete = %s (%v), want the revealed bundled base under its exact ID and bundled source", body, err)
	}

	// A keyed create without a binding member allocates a fresh name and
	// returns it as the actual binding; the key value itself never leaves.
	key := "mounted-generated-secret"
	created, err := client.CreateProviderWithResponse(ctx, protocol.CreateProviderRequest{
		Id:       "generated",
		Provider: protocol.ProviderEdit{BaseUrl: ptrTo("https://generated.test/v1")},
		Models:   map[string]protocol.ModelEdit{"m": {ContextWindow: ptrTo(4096)}},
		ApiKey:   &key,
	})
	if err != nil || created.JSON200 == nil || created.JSON200.Result.ApiKeyEnv != "LIGHTCODE_GENERATED_API_KEY" {
		body, _ := json.Marshal(created.JSON200)
		t.Fatalf("generated-binding create = %s (%v), want the actual binding in api_key_env", body, err)
	}
	if os.Getenv("LIGHTCODE_GENERATED_API_KEY") != key {
		t.Fatalf("managed key state = %q, want the exact supplied value", os.Getenv("LIGHTCODE_GENERATED_API_KEY"))
	}
	body, _ := json.Marshal(created.JSON200)
	if strings.Contains(string(body), key) {
		t.Fatalf("the create response carries key bytes: %s", body)
	}

	// A supplied key on an explicitly named external variable refuses before
	// any configuration or cache side effect, leaving the variable intact.
	supplied := "mounted-refused-secret"
	external := "PATH"
	refused, err := client.CreateProviderWithResponse(ctx, protocol.CreateProviderRequest{
		Id:       "external",
		Provider: protocol.ProviderEdit{BaseUrl: ptrTo("https://external.test/v1"), ApiKeyEnv: &external},
		Models:   map[string]protocol.ModelEdit{"m": {ContextWindow: ptrTo(4096)}},
		ApiKey:   &supplied,
	})
	if err != nil {
		t.Fatalf("external-binding create transport: %v", err)
	}
	if refused.JSONDefault == nil || refused.JSONDefault.Code != protocol.Configuration {
		body, _ := json.Marshal(refused.JSON200)
		t.Fatalf("external-binding create = %s (status %d), want the typed configuration refusal", body, refused.HTTPResponse.StatusCode)
	}
	if r.managedEnv.IsManaged("PATH") {
		t.Fatal("the refused create managed the external variable")
	}
	if detail, derr := client.GetProviderDetailWithResponse(ctx, &protocol.GetProviderDetailParams{ProviderId: "external"}); derr != nil || detail.JSONDefault == nil || detail.JSONDefault.Code != protocol.NotFound {
		t.Fatalf("the refused create registered the provider: (%v, %+v)", derr, detail.JSONDefault)
	}

	// The same credential rule crosses the mounted connect route for both
	// credential shapes: a usable keyless provider's connect refuses a
	// supplied key, and the registered PATH-bound provider's connect refuses
	// its supplied key — each before any effect, with the owning file, the
	// publication revision, and the external variable untouched.
	keyless, err := client.CreateProviderWithResponse(ctx, protocol.CreateProviderRequest{
		Id:       "keylessc",
		Provider: protocol.ProviderEdit{BaseUrl: ptrTo("https://keylessc.test/v1")},
		Models:   map[string]protocol.ModelEdit{"m": {ContextWindow: ptrTo(4096)}},
	})
	if err != nil || keyless.JSON200 == nil {
		body, _ := json.Marshal(keyless.JSON200)
		t.Fatalf("keyless create = %s (%v, status %d)", body, err, keyless.HTTPResponse.StatusCode)
	}
	externalRegistered, err := client.CreateProviderWithResponse(ctx, protocol.CreateProviderRequest{
		Id:       "externalc",
		Provider: protocol.ProviderEdit{BaseUrl: ptrTo("https://externalc.test/v1"), ApiKeyEnv: &external},
		Models:   map[string]protocol.ModelEdit{"m": {ContextWindow: ptrTo(4096)}},
	})
	if err != nil || externalRegistered.JSON200 == nil {
		body, _ := json.Marshal(externalRegistered.JSON200)
		t.Fatalf("no-key external registration = %s (%v, status %d)", body, err, externalRegistered.HTTPResponse.StatusCode)
	}

	// Both registered subjects are in place; the refusal baseline sits after
	// their successful writes so the refused connects are provably silent.
	configPath := filepath.Join(r.dataDir, "config.json")
	refusalBaseline, rerr := os.ReadFile(configPath)
	if rerr != nil {
		t.Fatalf("read owning config: %v", rerr)
	}
	baselineView, err := client.GetConfigurationWithResponse(ctx)
	if err != nil || baselineView.JSON200 == nil {
		t.Fatalf("read revision baseline: %v", err)
	}
	suppliedConnect := "mounted-connect-secret"
	keylessConnect, err := client.ConnectProviderWithResponse(ctx, &protocol.ConnectProviderParams{ProviderId: "keylessc"}, protocol.ConnectRequest{ApiKey: &suppliedConnect})
	if err != nil || keylessConnect.JSONDefault == nil || keylessConnect.JSONDefault.Code != protocol.Configuration {
		body, _ := json.Marshal(keylessConnect.JSON200)
		t.Fatalf("keyless connect with a supplied key = %s (status %d, %+v), want the typed configuration refusal", body, keylessConnect.HTTPResponse.StatusCode, keylessConnect.JSONDefault)
	}
	// The supplied-key connect on the externally defined PATH variable
	// refuses through the same shared rule.
	externalConnect, err := client.ConnectProviderWithResponse(ctx, &protocol.ConnectProviderParams{ProviderId: "externalc"}, protocol.ConnectRequest{ApiKey: &suppliedConnect})
	if err != nil || externalConnect.JSONDefault == nil || externalConnect.JSONDefault.Code != protocol.Configuration {
		body, _ := json.Marshal(externalConnect.JSON200)
		t.Fatalf("external connect with a supplied key = %s (status %d, %+v), want the typed configuration refusal", body, externalConnect.HTTPResponse.StatusCode, externalConnect.JSONDefault)
	}
	if r.managedEnv.IsManaged("PATH") {
		t.Fatal("a refused connect managed the external variable")
	}
	afterConfig, rerr := os.ReadFile(configPath)
	if rerr != nil || string(afterConfig) != string(refusalBaseline) {
		t.Fatalf("a refused connect changed the owning config (%v)", rerr)
	}
	currentView, err := client.GetConfigurationWithResponse(ctx)
	if err != nil || currentView.JSON200 == nil {
		t.Fatalf("read after refusals: %v", err)
	}
	if currentView.JSON200.ConfigurationRevision.Generation != baselineView.JSON200.ConfigurationRevision.Generation {
		t.Fatalf("a refused connect advanced the revision %s -> %s",
			baselineView.JSON200.ConfigurationRevision.Generation, currentView.JSON200.ConfigurationRevision.Generation)
	}

	// A field outside the shared enum refuses invalid at the mounted owner
	// through both reset routes, before any write.
	for _, target := range []string{
		"/v1/providers/fields/bogus?provider_id=keylessc",
		"/v1/providers/models/fields/bogus?provider_id=keylessc&model_id=m",
	} {
		refusal := rawProtocol(t, http.MethodDelete, protocolTarget(ps, target), ps.credential, "")
		defer refusal.Body.Close()
		if refusal.StatusCode != http.StatusBadRequest {
			t.Fatalf("reset %q = %d, want the typed invalid rejection 400", target, refusal.StatusCode)
		}
		if typed := decodeProtocolError(t, refusal); typed.Code != protocol.Invalid {
			t.Fatalf("reset %q error = %+v, want invalid", target, typed)
		}
	}
	afterConfig, rerr = os.ReadFile(configPath)
	if rerr != nil || string(afterConfig) != string(refusalBaseline) {
		t.Fatalf("a refused reset changed the owning config (%v)", rerr)
	}
}

// TestProtocolServerAgentModelEdit pins the agent-model operation and the
// identity-query round trip for one agent type whose name carries the
// retained directory-like spelling.
func TestProtocolServerAgentModelEdit(t *testing.T) {
	e := newOwnerEnv(t)
	writeServiceFile(t, agents.PathForConfig(e.configPath), `{"solo": {"model": "prov/m", "system_prompt": "simple"}, "..": {"model": "prov/m", "system_prompt": "simple"}}`)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)
	client := protocolClient(t, ps)
	ctx := context.Background()

	mutation, err := client.SetAgentTypeModelWithResponse(ctx, &protocol.SetAgentTypeModelParams{AgentType: ".."}, protocol.SetAgentTypeModelRequest{Model: "prov/m"})
	if err != nil {
		t.Fatalf("SetAgentTypeModel: %v", err)
	}
	if mutation.JSON200 == nil || mutation.JSON200.Result.Name != ".." || mutation.JSON200.Result.Model != "prov/m" {
		body, _ := json.Marshal(mutation.JSON200)
		t.Fatalf("agent model edit = %s, want the %q roster entry's new model", body, "..")
	}
	if mutation.JSON200.ConfigurationRevision.InstanceId != ps.instance {
		t.Fatalf("agent mutation revision instance = %q", mutation.JSON200.ConfigurationRevision.InstanceId)
	}
	unknown, err := client.SetAgentTypeModelWithResponse(ctx, &protocol.SetAgentTypeModelParams{AgentType: "ghost"}, protocol.SetAgentTypeModelRequest{Model: "prov/m"})
	if err != nil {
		t.Fatalf("SetAgentTypeModel(unknown): %v", err)
	}
	if unknown.JSONDefault == nil || unknown.JSONDefault.Code != protocol.Invalid {
		t.Fatalf("unknown agent type edit = %+v, want the typed invalid refusal", unknown.JSONDefault)
	}
}

// TestProtocolServerRetainedFamily drives the retained pre-cutover snapshot
// operation family through the mounted server: the turn listing under the
// exact 8-hex legacy identity and its workspace proof, the code-only restore
// to the recorded preimage, and the identity boundary that refuses a target
// 32-hex Session ID on the retained route.
func TestProtocolServerRetainedFamily(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		ctx := context.Background()

		workspace := filepath.Join(e.home, "legacy-ws")
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir legacy workspace: %v", err)
		}
		legacyID := "feedface"
		sessionDir := seedRetainedSession(t, r.config.loader.Home(), workspace, legacyID)
		file := filepath.Join(workspace, "partial.txt")
		if err := os.WriteFile(file, []byte("p0"), 0o600); err != nil {
			t.Fatalf("write retained preimage: %v", err)
		}
		recordRetainedMutation(t, sessionDir, 1, file, file, []byte("p1"))

		turns, err := client.GetRetainedCodeSnapshotsWithResponse(ctx, &protocol.GetRetainedCodeSnapshotsParams{Workspace: workspace, SessionId: legacyID})
		if err != nil {
			t.Fatalf("GetRetainedCodeSnapshots: %v", err)
		}
		if turns.JSON200 == nil || len(turns.JSON200.Turns) != 1 {
			body, _ := json.Marshal(turns.JSON200)
			t.Fatalf("retained turns = %s (status %d, %v), want one recorded turn", body, turns.HTTPResponse.StatusCode, turns.JSONDefault)
		}
		turn := turns.JSON200.Turns[0]
		if turn.Turn != 1 || len(turn.Files) != 1 || turn.Files[0].Path != file || !turn.Files[0].Existed {
			body, _ := json.Marshal(turn)
			t.Fatalf("retained turn = %s, want the recorded file identity", body)
		}

		revert, err := client.RevertRetainedCodeWithResponse(ctx, protocol.RetainedRevertRequest{Workspace: workspace, SessionId: legacyID, AfterTurn: 0})
		if err != nil {
			t.Fatalf("RevertRetainedCode: %v", err)
		}
		if revert.JSON200 == nil || !slices.Contains(revert.JSON200.Restored, file) || revert.JSON200.Error != nil {
			body, _ := json.Marshal(revert.JSON200)
			t.Fatalf("retained revert = %s (status %d), want the completed restore", body, revert.HTTPResponse.StatusCode)
		}
		if restored, err := os.ReadFile(file); err != nil || string(restored) != "p0" {
			t.Fatalf("restored retained file = (%q, %v), want its recorded preimage", restored, err)
		}

		// The identity boundary is one rule: a target 32-hex Session ID is
		// refused on the retained route before any snapshot read.
		target := strings.Repeat("a", 32)
		wrong, err := client.GetRetainedCodeSnapshotsWithResponse(ctx, &protocol.GetRetainedCodeSnapshotsParams{Workspace: workspace, SessionId: target})
		if err != nil {
			t.Fatalf("retained read with a target identity: %v", err)
		}
		if wrong.JSONDefault == nil || wrong.JSONDefault.Code != protocol.Invalid {
			body, _ := json.Marshal(wrong.JSONDefault)
			t.Fatalf("target identity on the retained route = %s, want the typed invalid refusal", body)
		}
	})
}

// TestProtocolServerIdentityQueryRoundTrips pins the identity rule: every
// retained-valid provider identifier is a query value, never a path segment,
// so the directory-like and URL-significant spellings round-trip through the
// generated client on the detail, connect, model, and reset routes, a
// slash-containing model ID stays addressable, and the agent-type identity
// with the same spelling is reachable for model edits.
func TestProtocolServerIdentityQueryRoundTrips(t *testing.T) {
	// The keyed connection row writes its key into the process environment
	// for this owner's lifetime; reserve the name first so repeated runs
	// start from the same external state and any original shell value is
	// restored after the test.
	reserveEnvKey(t, "IDENTITY_QUERY_KEY")
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)
	client := protocolClient(t, ps)
	ctx := context.Background()

	window := 4096
	for _, id := range []string{".", "..", "?", "#", "%"} {
		added, err := client.CreateProviderWithResponse(ctx, protocol.CreateProviderRequest{
			Id:       id,
			Provider: protocol.ProviderEdit{BaseUrl: ptrTo("https://identity.test/v1")},
			Models:   map[string]protocol.ModelEdit{"m": {ContextWindow: &window}},
		})
		if err != nil {
			t.Fatalf("create %q: %v", id, err)
		}
		if added.JSON200 == nil || added.JSON200.Result.Id != id {
			body, _ := json.Marshal(added.JSON200)
			t.Fatalf("create %q = %s (status %d), want the provider under its exact ID", id, body, added.HTTPResponse.StatusCode)
		}
		detail, err := client.GetProviderDetailWithResponse(ctx, &protocol.GetProviderDetailParams{ProviderId: id})
		if err != nil || detail.JSON200 == nil || detail.JSON200.Provider.Id != id {
			t.Fatalf("detail %q = (%v, %+v), want the exact identity round trip", id, err, detail.JSON200)
		}
		edited, err := client.UpdateProviderDetailWithResponse(ctx, &protocol.UpdateProviderDetailParams{ProviderId: id}, protocol.UpdateProviderDetailRequest{
			Provider: protocol.ProviderEdit{Name: ptrTo("renamed")},
		})
		if err != nil || edited.JSON200 == nil || edited.JSON200.Result.Name != "renamed" {
			t.Fatalf("edit %q = (%v, %+v)", id, err, edited.JSON200)
		}
		models, err := client.ListProviderModelsWithResponse(ctx, &protocol.ListProviderModelsParams{ProviderId: id})
		if err != nil || models.JSON200 == nil || len(models.JSON200.Models) != 1 || models.JSON200.Models[0].Id != "m" {
			t.Fatalf("models %q = (%v, %+v)", id, err, models.JSON200)
		}
		hidden := true
		saved, err := client.UpdateProviderModelWithResponse(ctx, &protocol.UpdateProviderModelParams{ProviderId: id, ModelId: "m"}, protocol.UpdateProviderModelRequest{
			Model: protocol.ModelEdit{Hidden: &hidden},
		})
		if err != nil || saved.JSON200 == nil || !saved.JSON200.Result.Hidden {
			t.Fatalf("model edit %q = (%v, %+v)", id, err, saved.JSON200)
		}
		reset, err := client.ResetProviderModelFieldWithResponse(ctx, protocol.ModelField("name"), &protocol.ResetProviderModelFieldParams{ProviderId: id, ModelId: "m"})
		if err != nil || reset.JSON200 == nil {
			t.Fatalf("model reset %q = (%v, %+v)", id, err, reset.JSON200)
		}
		connected, err := client.ConnectProviderWithResponse(ctx, &protocol.ConnectProviderParams{ProviderId: id}, protocol.ConnectRequest{})
		if err != nil || connected.JSON200 == nil || connected.JSON200.Result.Id != id {
			body, _ := json.Marshal(connected.JSON200)
			t.Fatalf("connect %q = %s (status %d, %v), want the keyless connection under its exact ID", id, body, connected.HTTPResponse.StatusCode, err)
		}
		fieldReset, err := client.ResetProviderFieldWithResponse(ctx, protocol.ProviderField("name"), &protocol.ResetProviderFieldParams{ProviderId: id})
		if err != nil || fieldReset.JSON200 == nil || fieldReset.JSON200.Result.Id != id {
			body, _ := json.Marshal(fieldReset.JSON200)
			t.Fatalf("provider field reset %q = %s (status %d, %v), want the exact ID", id, body, fieldReset.HTTPResponse.StatusCode, err)
		}
		// A keyless provider's disconnect is the retained invalid refusal,
		// never a successful no-op.
		keylessDisconnect, err := client.DisconnectProviderWithResponse(ctx, &protocol.DisconnectProviderParams{ProviderId: id})
		if err != nil || keylessDisconnect.JSONDefault == nil || keylessDisconnect.JSONDefault.Code != protocol.Invalid {
			body, _ := json.Marshal(keylessDisconnect.JSONDefault)
			t.Fatalf("keyless disconnect %q = %s (status %d, %v), want the typed invalid refusal", id, body, keylessDisconnect.HTTPResponse.StatusCode, err)
		}
	}

	// The connect and disconnect routes address the same identity; a keyed
	// provider supplies its write-only key.
	keyed, err := client.CreateProviderWithResponse(ctx, protocol.CreateProviderRequest{
		Id:       "?keyed",
		Provider: protocol.ProviderEdit{BaseUrl: ptrTo("https://identity.test/v1"), ApiKeyEnv: ptrTo("IDENTITY_QUERY_KEY")},
		Models:   map[string]protocol.ModelEdit{"m": {ContextWindow: &window}},
		ApiKey:   ptrTo("identity-secret"),
	})
	if err != nil || keyed.JSON200 == nil {
		t.Fatalf("keyed create: %v", err)
	}
	if disconnected, err := client.DisconnectProviderWithResponse(ctx, &protocol.DisconnectProviderParams{ProviderId: "?keyed"}); err != nil || disconnected.JSON200 == nil {
		t.Fatalf("keyed disconnect: %v", err)
	}
	connected, err := client.ConnectProviderWithResponse(ctx, &protocol.ConnectProviderParams{ProviderId: "?keyed"}, protocol.ConnectRequest{ApiKey: ptrTo("identity-secret")})
	if err != nil || connected.JSON200 == nil || !connected.JSON200.Result.Connected {
		t.Fatalf("keyed connect = (%v, %+v)", err, connected.JSON200)
	}

	// A slash-containing model ID stays a valid query value.
	slashed, err := client.CreateProviderWithResponse(ctx, protocol.CreateProviderRequest{
		Id:       "slashmodel",
		Provider: protocol.ProviderEdit{BaseUrl: ptrTo("https://identity.test/v1")},
		Models:   map[string]protocol.ModelEdit{"org/model": {ContextWindow: &window}},
	})
	if err != nil || slashed.JSON200 == nil {
		t.Fatalf("slash-model create: %v", err)
	}
	slashSaved, err := client.UpdateProviderModelWithResponse(ctx, &protocol.UpdateProviderModelParams{ProviderId: "slashmodel", ModelId: "org/model"}, protocol.UpdateProviderModelRequest{
		Model: protocol.ModelEdit{Name: ptrTo("Slashed")},
	})
	if err != nil || slashSaved.JSON200 == nil || slashSaved.JSON200.Result.Id != "org/model" {
		t.Fatalf("slash model edit = (%v, %+v)", err, slashSaved.JSON200)
	}

	// An empty required identity value is the one invalid-set rule.
	empty, err := client.GetProviderDetailWithResponse(ctx, &protocol.GetProviderDetailParams{ProviderId: ""})
	if err != nil {
		t.Fatalf("empty identity read: %v", err)
	}
	if empty.JSONDefault == nil || empty.JSONDefault.Code != protocol.Invalid {
		body, _ := json.Marshal(empty.JSONDefault)
		t.Fatalf("empty identity = %s (status %d), want the typed invalid refusal", body, empty.HTTPResponse.StatusCode)
	}
}

// refusedAnswer extracts one generated response's typed default error body
// and HTTP status through the generated accessors; a transport failure or an
// answer without its typed body fails the calling row.
func refusedAnswer(t *testing.T, resp interface {
	GetJSONDefault() *protocol.Error
	StatusCode() int
}, err error) (int, protocol.Error) {
	t.Helper()
	if err != nil {
		t.Fatalf("generated client call: %v", err)
	}
	if typed := resp.GetJSONDefault(); typed != nil {
		return resp.StatusCode(), *typed
	}
	t.Fatalf("response %d carries no typed error body", resp.StatusCode())
	return 0, protocol.Error{}
}

// TestProtocolServerMissingMetadata pins the missing-metadata identity
// answers of the mounted generated client: the model DELETE/reset and
// provider detail/delete/reset routes answer the typed not-found 404 for an
// absent provider or model identity and the typed invalid 400 for a
// malformed empty one, while an existing identity still edits normally on
// the same routes. Every refused edit leaves the latest owning bytes, the
// published generation, the warning revision and the event stream
// untouched.
func TestProtocolServerMissingMetadata(t *testing.T) {
	r, _ := openProjectionRuntime(t, storage.NewMemory())
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)
	client := protocolClient(t, ps)
	ctx := context.Background()

	// The existing identities edit normally on the same routes the
	// absent-identity rows below refuse.
	window := 4096
	spare, err := client.UpdateProviderModelWithResponse(ctx, &protocol.UpdateProviderModelParams{ProviderId: "prov", ModelId: "spare"}, protocol.UpdateProviderModelRequest{
		Model: protocol.ModelEdit{Name: ptrTo("Spare"), ContextWindow: &window},
	})
	if err != nil || spare.JSON200 == nil || spare.JSON200.Result == nil || spare.JSON200.Result.Id != "spare" {
		body, _ := json.Marshal(spare.JSON200)
		t.Fatalf("existing model edit = %s (status %d, %v), want the upserted model", body, spare.HTTPResponse.StatusCode, err)
	}
	spareReset, err := client.ResetProviderModelFieldWithResponse(ctx, protocol.ModelFieldContextWindow, &protocol.ResetProviderModelFieldParams{ProviderId: "prov", ModelId: "spare"})
	if err != nil || spareReset.JSON200 == nil {
		body, _ := json.Marshal(spareReset.JSON200)
		t.Fatalf("existing model reset = %s (status %d, %v), want the published reset", body, spareReset.HTTPResponse.StatusCode, err)
	}
	detail, err := client.GetProviderDetailWithResponse(ctx, &protocol.GetProviderDetailParams{ProviderId: "prov"})
	if err != nil || detail.JSON200 == nil || detail.JSON200.Provider.Id != "prov" {
		t.Fatalf("existing provider read = (%v, %+v), want the provider view", err, detail.JSON200)
	}
	provReset, err := client.ResetProviderFieldWithResponse(ctx, protocol.ProviderFieldName, &protocol.ResetProviderFieldParams{ProviderId: "prov"})
	if err != nil || provReset.JSON200 == nil {
		body, _ := json.Marshal(provReset.JSON200)
		t.Fatalf("existing provider reset = %s (status %d, %v), want the published reset", body, provReset.HTTPResponse.StatusCode, err)
	}
	removed, err := client.DeleteProviderModelWithResponse(ctx, &protocol.DeleteProviderModelParams{ProviderId: "prov", ModelId: "spare"})
	if err != nil || removed.JSON200 == nil || removed.JSON200.Result != nil {
		body, _ := json.Marshal(removed.JSON200)
		t.Fatalf("existing model deletion = %s (status %d, %v), want the null post-state", body, removed.HTTPResponse.StatusCode, err)
	}

	// The refused identities: the loop checks each live answer's class and
	// status at the shared writer, and the shared refusal oracle checks the
	// latest owning bytes, the published generation, the warning revision
	// and the event stream through its wrapped expected class.
	subscription, err := r.Subscribe(64)
	if err != nil {
		t.Fatalf("subscribe for the refusal baseline: %v", err)
	}
	defer subscription.Close()
	refused := []struct {
		name   string
		want   error
		code   protocol.ErrorCode
		status int
		call   func(t *testing.T) (int, protocol.Error)
	}{
		{"model delete missing", catalog.ErrUnknownModel, protocol.NotFound, http.StatusNotFound, func(t *testing.T) (int, protocol.Error) {
			resp, err := client.DeleteProviderModelWithResponse(ctx, &protocol.DeleteProviderModelParams{ProviderId: "prov", ModelId: "ghost"})
			return refusedAnswer(t, resp, err)
		}},
		{"model reset missing", catalog.ErrUnknownModel, protocol.NotFound, http.StatusNotFound, func(t *testing.T) (int, protocol.Error) {
			resp, err := client.ResetProviderModelFieldWithResponse(ctx, protocol.ModelFieldName, &protocol.ResetProviderModelFieldParams{ProviderId: "prov", ModelId: "ghost"})
			return refusedAnswer(t, resp, err)
		}},
		{"provider detail missing", catalog.ErrUnknownProvider, protocol.NotFound, http.StatusNotFound, func(t *testing.T) (int, protocol.Error) {
			resp, err := client.GetProviderDetailWithResponse(ctx, &protocol.GetProviderDetailParams{ProviderId: "ghost"})
			return refusedAnswer(t, resp, err)
		}},
		{"provider delete missing", catalog.ErrUnknownProvider, protocol.NotFound, http.StatusNotFound, func(t *testing.T) (int, protocol.Error) {
			resp, err := client.DeleteProviderDetailWithResponse(ctx, &protocol.DeleteProviderDetailParams{ProviderId: "ghost"})
			return refusedAnswer(t, resp, err)
		}},
		{"provider reset missing", catalog.ErrUnknownProvider, protocol.NotFound, http.StatusNotFound, func(t *testing.T) (int, protocol.Error) {
			resp, err := client.ResetProviderFieldWithResponse(ctx, protocol.ProviderFieldName, &protocol.ResetProviderFieldParams{ProviderId: "ghost"})
			return refusedAnswer(t, resp, err)
		}},
		{"model delete empty identity", harness.ErrInvalid, protocol.Invalid, http.StatusBadRequest, func(t *testing.T) (int, protocol.Error) {
			resp, err := client.DeleteProviderModelWithResponse(ctx, &protocol.DeleteProviderModelParams{ProviderId: "prov", ModelId: ""})
			return refusedAnswer(t, resp, err)
		}},
		{"model reset empty identity", harness.ErrInvalid, protocol.Invalid, http.StatusBadRequest, func(t *testing.T) (int, protocol.Error) {
			resp, err := client.ResetProviderModelFieldWithResponse(ctx, protocol.ModelFieldName, &protocol.ResetProviderModelFieldParams{ProviderId: "prov", ModelId: ""})
			return refusedAnswer(t, resp, err)
		}},
		{"provider delete empty identity", harness.ErrInvalid, protocol.Invalid, http.StatusBadRequest, func(t *testing.T) (int, protocol.Error) {
			resp, err := client.DeleteProviderDetailWithResponse(ctx, &protocol.DeleteProviderDetailParams{ProviderId: ""})
			return refusedAnswer(t, resp, err)
		}},
	}
	for _, row := range refused {
		t.Run(row.name, func(t *testing.T) {
			before, generation, warnRev := runtimeMutationBaseline(t, r)
			status, typed := row.call(t)
			if typed.Code != row.code || status != row.status {
				t.Fatalf("%s = (%d, %+v), want the typed (%d, %s)", row.name, status, typed, row.status, row.code)
			}
			assertRuntimeMutationRefused(t, r, subscription, before, generation, warnRev,
				fmt.Errorf("%s: %w", row.name, row.want), row.want)
		})
	}
}
