package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/protocol"
)

// The mounted transport contract rows: authentication ordering, strict
// bodies, routing dispositions, and the error taxonomy — each pinned through
// the real listener with raw HTTP requests or the generated client.

// TestProtocolServerAuthenticationOrder pins the gate's position: every
// mounted handler and the root catch-all authenticate before the generated
// wrapper binds any parameter and before any producer effect, and the
// credential never rides a response.
func TestProtocolServerAuthenticationOrder(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		ctx := context.Background()

		absent := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1/health"), "", "")
		if absent.StatusCode != http.StatusUnauthorized {
			t.Fatalf("absent credential = %d, want 401", absent.StatusCode)
		}
		typed := decodeProtocolError(t, absent)
		if typed.Code != protocol.Unauthorized || strings.Contains(typed.Message, ps.credential) {
			t.Fatalf("absent credential error = %+v, want unauthorized with no credential echo", typed)
		}

		wrong := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1/health"), "wrong-token", "")
		if wrong.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong credential = %d, want 401", wrong.StatusCode)
		}
		scheme := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1/health"), "Basic dXNlcjpwYXNz", "")
		if scheme.StatusCode != http.StatusUnauthorized {
			t.Fatalf("foreign scheme = %d, want 401", scheme.StatusCode)
		}

		// Authentication precedes parameter binding: a request missing its
		// required query member is answered unauthorized while the
		// credential is absent, and invalid once it is present.
		bindless := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1/providers/detail"), "", "")
		if bindless.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated missing query = %d, want the 401 gate answer", bindless.StatusCode)
		}
		binding := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1/providers/detail"), ps.credential, "")
		if binding.StatusCode != http.StatusBadRequest {
			t.Fatalf("missing query = %d, want the typed binding failure 400", binding.StatusCode)
		}
		if typed := decodeProtocolError(t, binding); typed.Code != protocol.Invalid {
			t.Fatalf("missing query error = %+v, want invalid", typed)
		}

		// Authentication precedes every producer effect: a creation with a
		// bad credential answers unauthorized and creates nothing.
		workspace := filepath.Join(e.home, "auth")
		creation := rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/sessions"), "bad",
			fmt.Sprintf(`{"workspace":%q,"agent_type":"solo"}`, workspace))
		if creation.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated creation = %d, want 401", creation.StatusCode)
		}
		sessions, err := r.listSessions(ctx, protocol.ListSessionsParams{Workspace: workspace, Lifecycle: "open"})
		if err != nil {
			t.Fatalf("list after refused creation: %v", err)
		}
		if len(sessions) != 0 {
			t.Fatalf("refused creation left %d sessions, want none", len(sessions))
		}

		// The root catch-all rides the same gate: an unknown route without a
		// credential is unauthorized, not a typed route miss.
		catchAll := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1/nope"), "", "")
		if catchAll.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated unknown route = %d, want the 401 gate answer", catchAll.StatusCode)
		}

		// The events stream authenticates before it subscribes: an
		// unauthenticated request answers the typed 401 and creates no
		// subscription.
		unauthenticatedEvents := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1/events"), "", "")
		if unauthenticatedEvents.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated events = %d, want the typed 401", unauthenticatedEvents.StatusCode)
		}
		if typed := decodeProtocolError(t, unauthenticatedEvents); typed.Code != protocol.Unauthorized {
			t.Fatalf("unauthenticated events error = %+v, want unauthorized", typed)
		}
		if subscribers := subscriberCount(r); subscribers != 0 {
			t.Fatalf("the unauthenticated events request created %d subscriptions", subscribers)
		}

		// Version compatibility is connection-side: a bogus per-request
		// version header changes nothing on the authenticated health read.
		versioned, err := http.NewRequest(http.MethodGet, protocolTarget(ps, "/v1/health"), nil)
		if err != nil {
			t.Fatalf("build versioned health request: %v", err)
		}
		versioned.Header.Set("Authorization", "Bearer "+ps.credential)
		versioned.Header.Set("X-Lightcode-Version", "999")
		versionedResponse, err := (&http.Client{Timeout: 30 * time.Second}).Do(versioned)
		if err != nil {
			t.Fatalf("versioned health: %v", err)
		}
		if versionedResponse.StatusCode != http.StatusOK {
			versionedResponse.Body.Close()
			t.Fatalf("versioned health = %d, want 200", versionedResponse.StatusCode)
		}
		var health protocol.Health
		if err := json.NewDecoder(versionedResponse.Body).Decode(&health); err != nil {
			versionedResponse.Body.Close()
			t.Fatalf("decode versioned health: %v", err)
		}
		versionedResponse.Body.Close()
		if health.ProtocolVersion != protocol.N1 {
			t.Fatalf("versioned health protocol = %q, want %q", health.ProtocolVersion, protocol.N1)
		}
	})
}

// TestProtocolServerStrictBodies pins the one decode boundary through the
// mounted server: unknown members at top, nested, and union levels, trailing
// documents, wrong content, null declared members, missing required members,
// invalid enum values, non-empty control bodies, and the null top-level body
// — each rejected with the typed invalid error before any mutation, while
// opaque map values keep null and exact numbers.
func TestProtocolServerStrictBodies(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		ctx := context.Background()

		create := func(body string) *http.Response {
			return rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/sessions"), ps.credential, body)
		}
		workspace := filepath.Join(e.home, "strict")
		reject := func(name, body string) {
			t.Helper()
			response := create(body)
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s = %d, want the typed invalid rejection 400", name, response.StatusCode)
			}
			if typed := decodeProtocolError(t, response); typed.Code != protocol.Invalid {
				t.Fatalf("%s error = %+v, want invalid", name, typed)
			}
			sessions, err := r.listSessions(ctx, protocol.ListSessionsParams{Workspace: workspace, Lifecycle: "open"})
			if err != nil {
				t.Fatalf("list after %s: %v", name, err)
			}
			if len(sessions) != 0 {
				t.Fatalf("%s created a session, want no mutation before validation", name)
			}
		}
		reject("unknown top member", fmt.Sprintf(`{"workspace":%q,"agent_type":"solo","note":"x"}`, workspace))
		reject("missing required member", fmt.Sprintf(`{"workspace":%q}`, workspace))
		reject("null declared member", `{"workspace":null,"agent_type":"solo"}`)
		reject("two concatenated documents", fmt.Sprintf(`{"workspace":%q,"agent_type":"solo"} {"workspace":%q}`, workspace, workspace))
		reject("trailing scalar", fmt.Sprintf(`{"workspace":%q,"agent_type":"solo"} 5`, workspace))
		reject("wrong member content", fmt.Sprintf(`{"workspace":%q,"agent_type":5}`, workspace))
		reject("top-level null", "null")
		reject("top-level array", "[]")

		// The submit route carries the one request union: an unknown member
		// inside a selected variant is closed out, an unknown discriminator
		// is rejected, and an invalid mode enum fails the generated
		// validator. A valid session exists for these rows.
		client := protocolClient(t, ps)
		created, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: workspace, AgentType: "solo"})
		if err != nil || created.JSON201 == nil {
			t.Fatalf("create: %v", err)
		}
		session := created.JSON201.SessionId
		gate := make(chan struct{})
		e.server.setHold(gate)
		defer close(gate)
		if _, err := client.SubmitSessionWithResponse(ctx, session, protocol.SubmitRequest{
			OperationId: "op-1", Mode: "regular",
			Content: []protocol.ContentPart{textContentPart(t, "first", nil)},
		}); err != nil {
			t.Fatalf("gated submit: %v", err)
		}
		awaitModelArrival(t, e)

		submit := func(body string) *http.Response {
			return rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/sessions/"+session+"/submit"), ps.credential, body)
		}
		rejectSubmit := func(name, body string) {
			t.Helper()
			// The gated Operation keeps the Session stable, so one
			// independent pre-request snapshot (facts, pending queues, and
			// revisions) is the baseline the rejection must leave
			// untouched.
			before := snapshotThroughRuntime(t, r, session)
			response := submit(body)
			if response.StatusCode != http.StatusBadRequest {
				response.Body.Close()
				t.Fatalf("%s = %d, want the typed invalid rejection 400", name, response.StatusCode)
			}
			if typed := decodeProtocolError(t, response); typed.Code != protocol.Invalid {
				t.Fatalf("%s error = %+v, want the typed invalid body", name, typed)
			}
			after := snapshotThroughRuntime(t, r, session)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("%s changed the Session snapshot (pending queues, facts, or revisions)", name)
			}
		}
		rejectSubmit("unknown union member", `{"operation_id":"op-2","mode":"queued","content":[{"kind":"text","text":"x","forged":1}]}`)
		rejectSubmit("unknown union discriminator", `{"operation_id":"op-2","mode":"queued","content":[{"kind":"pdf","text":"x"}]}`)
		rejectSubmit("union null kind member", `{"operation_id":"op-2","mode":"queued","content":[{"kind":null,"text":"x"}]}`)
		rejectSubmit("union missing required member", `{"operation_id":"op-2","mode":"queued","content":[{"kind":"text"}]}`)
		rejectSubmit("invalid mode enum", `{"operation_id":"op-2","mode":"fast","content":[{"kind":"text","text":"x"}]}`)
		rejectSubmit("image_url missing url", `{"operation_id":"op-2","mode":"queued","content":[{"kind":"image_url"}]}`)
		rejectSubmit("image_url null url", `{"operation_id":"op-2","mode":"queued","content":[{"kind":"image_url","url":null}]}`)
		rejectSubmit("image_url unknown member", `{"operation_id":"op-2","mode":"queued","content":[{"kind":"image_url","url":"https://images.test/i.png","forged":1}]}`)
		rejectSubmit("opaque missing wire type", `{"operation_id":"op-2","mode":"queued","content":[{"kind":"opaque"}]}`)
		rejectSubmit("opaque null wire type", `{"operation_id":"op-2","mode":"queued","content":[{"kind":"opaque","opaque_wire_type":null}]}`)
		rejectSubmit("opaque unknown member", `{"operation_id":"op-2","mode":"queued","content":[{"kind":"opaque","opaque_wire_type":"vendor","forged":1}]}`)
		// The valid siblings of both variants stay accepted on the same
		// route: the gated Operation buffers them with the queued
		// disposition.
		accepted, err := client.SubmitSessionWithResponse(ctx, session, protocol.SubmitRequest{
			OperationId: "op-parts",
			Mode:        protocol.SubmitRequestModeQueued,
			Content: []protocol.ContentPart{
				imageURLContentPart(t, "https://images.test/i.png"),
				opaqueContentPart(t, "vendor_wire"),
			},
		})
		if err != nil {
			t.Fatalf("valid image_url/opaque submit: %v", err)
		}
		if accepted.JSON200 == nil || accepted.JSON200.Disposition != protocol.SubmitResultDispositionQueued {
			body, _ := json.Marshal(accepted.JSON200)
			t.Fatalf("valid image_url/opaque submit = %s (status %d), want the buffered disposition", body, accepted.HTTPResponse.StatusCode)
		}

		// Nested closed members inside the provider-create request's typed
		// model map and a null typed map value are rejected before any
		// write.
		providerCreate := func(body string) *http.Response {
			return rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/providers"), ps.credential, body)
		}
		nested := providerCreate(`{"id":"newp","provider":{"base_url":"https://x.test/v1"},"models":{"m":{"context_window":4096,"bogus":1}}}`)
		defer nested.Body.Close()
		if nested.StatusCode != http.StatusBadRequest {
			t.Fatalf("unknown nested model member = %d, want 400", nested.StatusCode)
		}
		nullModel := providerCreate(`{"id":"newp","provider":{"base_url":"https://x.test/v1"},"models":{"m":null}}`)
		defer nullModel.Body.Close()
		if nullModel.StatusCode != http.StatusBadRequest {
			t.Fatalf("null typed map value = %d, want 400", nullModel.StatusCode)
		}
		// The provider-create typed model map carries otherwise valid
		// members and an invalid generated SystemRole: the nested enum
		// validator refuses it before any write.
		invalidEnum := providerCreate(`{"id":"newp","provider":{"base_url":"https://x.test/v1"},"models":{"m":{"context_window":4096,"system_role":"boss"}}}`)
		defer invalidEnum.Body.Close()
		if invalidEnum.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid nested SystemRole = %d, want the typed invalid rejection 400", invalidEnum.StatusCode)
		}
		if typed := decodeProtocolError(t, invalidEnum); typed.Code != protocol.Invalid {
			t.Fatalf("invalid nested SystemRole error = %+v, want invalid", typed)
		}
		// The same typed map carries an invalid element inside the real
		// input_modalities slice; its valid sibling slice element is
		// accepted.
		invalidModalities := providerCreate(`{"id":"newp","provider":{"base_url":"https://x.test/v1"},"models":{"m":{"context_window":4096,"input_modalities":["text","hologram"]}}}`)
		defer invalidModalities.Body.Close()
		if invalidModalities.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid input_modalities element = %d, want the typed invalid rejection 400", invalidModalities.StatusCode)
		}
		if typed := decodeProtocolError(t, invalidModalities); typed.Code != protocol.Invalid {
			t.Fatalf("invalid input_modalities element error = %+v, want invalid", typed)
		}
		validModalities := providerCreate(`{"id":"validmods","provider":{"base_url":"https://x.test/v1"},"models":{"m":{"context_window":4096,"input_modalities":["text"]}}}`)
		defer validModalities.Body.Close()
		if validModalities.StatusCode != http.StatusOK {
			t.Fatalf("valid input_modalities sibling = %d, want 200", validModalities.StatusCode)
		}
		if _, err := r.getProvider(ctx, "validmods"); err != nil {
			t.Fatalf("the valid input_modalities sibling was not written: %v", err)
		}
		if _, err := r.getProvider(ctx, "newp"); !errors.Is(err, catalog.ErrUnknownProvider) {
			t.Fatalf("rejected creation wrote a provider: %v", err)
		}

		// A null header value inside a declared typed map is not an opaque
		// map member: the discovery route rejects it before any fetch.
		nullHeader := rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/providers/discover"), ps.credential,
			`{"base_url":"https://x.test/v1","api_key_env":"","headers":{"h":null}}`)
		defer nullHeader.Body.Close()
		if nullHeader.StatusCode != http.StatusBadRequest {
			t.Fatalf("null header value = %d, want 400", nullHeader.StatusCode)
		}

		// The controls are argument-free POSTs: no request-body schema
		// exists, so any body is ignored and the addressed Session decides.
		// The nearest forbidden sibling is that addressed-Session rule: an
		// unknown target still answers the typed not-found, body or no body.
		interrupt := func(target, body string) *http.Response {
			return rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/sessions/"+target+"/interrupt"), ps.credential, body)
		}
		for name, body := range map[string]string{
			"no control body":          "",
			"empty object body":        "{}",
			"control body with member": `{"x":1}`,
			"control null body":        `null`,
			"control trailing member":  `{} {}`,
		} {
			response := interrupt(session, body)
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("%s = %d, want the accepted 204", name, response.StatusCode)
			}
		}
		unknown := interrupt("0123456789abcdef0123456789abcdef", "")
		unknown.Body.Close()
		if unknown.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown control target = %d, want the typed not-found 404", unknown.StatusCode)
		}
	})
}

// TestProtocolServerRoutingDispositions pins the native mux's transport
// answers and the typed catch-all: an unknown route and a wrong method both
// answer the schema's typed not_found, a pure trailing slash is never
// redirected, an unclean or doubled path answers the transport-level 307
// whose followed redirect never reaches a handler with an unclean parameter,
// and the asterisk-form OPTIONS request is answered by the server layer
// before any handler or the authentication gate.
func TestProtocolServerRoutingDispositions(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		ctx := context.Background()
		created, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: filepath.Join(e.home, "unclean"), AgentType: "solo"})
		if err != nil || created.JSON201 == nil {
			t.Fatalf("create session for the unclean-path row: %v", err)
		}
		session := created.JSON201.SessionId

		unknown := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1/nope"), ps.credential, "")
		if unknown.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown route = %d, want the typed 404", unknown.StatusCode)
		}
		if typed := decodeProtocolError(t, unknown); typed.Code != protocol.NotFound {
			t.Fatalf("unknown route error = %+v, want not_found", typed)
		}
		if media := unknown.Header.Get("Content-Type"); !strings.Contains(media, "json") {
			t.Fatalf("unknown route content type = %q, want the typed JSON body", media)
		}

		wrongMethod := rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/health"), ps.credential, "")
		if wrongMethod.StatusCode != http.StatusNotFound {
			t.Fatalf("wrong method = %d, want the typed 404", wrongMethod.StatusCode)
		}
		if typed := decodeProtocolError(t, wrongMethod); typed.Code != protocol.NotFound {
			t.Fatalf("wrong method error = %+v, want not_found", typed)
		}

		// The removed standalone Session reads are unregistered routes: each
		// answers the existing typed catch-all, never a partial handler.
		for _, removed := range []string{
			"/v1/sessions/" + session,
			"/v1/sessions/" + session + "/pending",
			"/v1/sessions/" + session + "/usage",
		} {
			response := rawProtocol(t, http.MethodGet, protocolTarget(ps, removed), ps.credential, "")
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("removed route %s = %d, want the typed 404", removed, response.StatusCode)
			}
			if typed := decodeProtocolError(t, response); typed.Code != protocol.NotFound {
				t.Fatalf("removed route %s error = %+v, want not_found", removed, typed)
			}
		}

		trailing := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1/health/"), ps.credential, "")
		if trailing.StatusCode != http.StatusNotFound || trailing.Header.Get("Location") != "" {
			t.Fatalf("trailing slash = (%d, location %q), want the typed 404 with no redirect",
				trailing.StatusCode, trailing.Header.Get("Location"))
		}

		unclean := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1//health"), ps.credential, "")
		if unclean.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("unclean path = %d, want the transport-level 307", unclean.StatusCode)
		}
		if location := unclean.Header.Get("Location"); location != "/v1/health" {
			t.Fatalf("unclean redirect location = %q, want the cleaned path", location)
		}
		// The clean operation answers with a clean parameter, never an
		// untyped answer.
		healthy := rawProtocol(t, http.MethodGet, protocolTarget(ps, "/v1/health"), ps.credential, "")
		if healthy.StatusCode != http.StatusOK {
			t.Fatalf("clean health = %d, want 200", healthy.StatusCode)
		}
		healthy.Body.Close()

		// An unclean Session path is transport-redirected to its clean
		// spelling; following it with the credential reaches the clean
		// hydration handler and reads exactly the addressed Session.
		uncleanSessionPath := "/v1//sessions/" + session + "/hydration"
		uncleanSession := rawProtocol(t, http.MethodGet, protocolTarget(ps, uncleanSessionPath), ps.credential, "")
		if uncleanSession.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("unclean Session path = %d, want the transport-level 307", uncleanSession.StatusCode)
		}
		if location := uncleanSession.Header.Get("Location"); location != "/v1/sessions/"+session+"/hydration" {
			t.Fatalf("unclean Session redirect location = %q, want the cleaned Session path", location)
		}
		uncleanSession.Body.Close()
		redirectRequest, err := http.NewRequest(http.MethodGet, protocolTarget(ps, uncleanSessionPath), nil)
		if err != nil {
			t.Fatalf("build unclean Session request: %v", err)
		}
		redirectRequest.Header.Set("Authorization", "Bearer "+ps.credential)
		redirected, err := (&http.Client{Timeout: 30 * time.Second}).Do(redirectRequest)
		if err != nil {
			t.Fatalf("follow the unclean Session redirect: %v", err)
		}
		if redirected.StatusCode != http.StatusOK {
			redirected.Body.Close()
			t.Fatalf("followed unclean Session path = %d, want the clean 200", redirected.StatusCode)
		}
		var followedSession protocol.Hydration
		if err := json.NewDecoder(redirected.Body).Decode(&followedSession); err != nil {
			redirected.Body.Close()
			t.Fatalf("decode the followed Session: %v", err)
		}
		redirected.Body.Close()
		if followedSession.Session.SessionId != session {
			t.Fatalf("followed Session = %q, want the addressed %q", followedSession.Session.SessionId, session)
		}

		// The asterisk-form OPTIONS request is answered by the server layer
		// before the routing tree, the authentication gate, and every
		// handler: it produces no typed protocol answer.
		conn, err := net.Dial("tcp", strings.TrimPrefix(ps.Endpoint(), "http://"))
		if err != nil {
			t.Fatalf("dial for OPTIONS: %v", err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("OPTIONS * HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")); err != nil {
			t.Fatalf("write OPTIONS: %v", err)
		}
		buffer := make([]byte, 512)
		read, err := conn.Read(buffer)
		if err != nil && read == 0 {
			t.Fatalf("read OPTIONS answer: %v", err)
		}
		answer := string(buffer[:read])
		statusLine, _, _ := strings.Cut(answer, "\r\n")
		if !strings.Contains(statusLine, "200") {
			t.Fatalf("OPTIONS * status = %q, want the stdlib transport-level 200", statusLine)
		}
		if !strings.Contains(answer, "Content-Length: 0") {
			t.Fatalf("OPTIONS * headers = %q, want the empty transport answer", answer)
		}
		if _, body, terminated := strings.Cut(answer, "\r\n\r\n"); terminated && body != "" {
			t.Fatalf("OPTIONS * body = %q, want the empty transport answer", body)
		}
	})
}

// TestProtocolServerErrorClasses pins the reachable error classes through the
// mounted server and the one classifier table for the classes a wire request
// cannot reach deterministically: every class keeps the plan's status, and
// an unclassified error stays internal.
func TestProtocolServerErrorClasses(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		ctx := context.Background()

		// A well-formed but absent session identity answers not_found.
		unused := newLifecycleID(t)
		if missing, err := client.GetSessionHydrationWithResponse(ctx, unused); err != nil || missing.JSONDefault == nil ||
			missing.JSONDefault.Code != protocol.NotFound || missing.HTTPResponse.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown session = (%d, %+v, %v), want the typed 404", missing.HTTPResponse.StatusCode, missing.JSONDefault, err)
		}
		// A target route never accepts a legacy 8-hex identity.
		legacy := rawProtocol(t, http.MethodDelete, protocolTarget(ps, "/v1/sessions/decafbad"), ps.credential, "")
		if legacy.StatusCode != http.StatusBadRequest {
			t.Fatalf("legacy identity on a target route = %d, want invalid 400", legacy.StatusCode)
		}
		legacy.Body.Close()

		// A valid unknown provider answers the catalog's typed not-found.
		if unknown, err := client.GetProviderDetailWithResponse(ctx, &protocol.GetProviderDetailParams{ProviderId: "ghost"}); err != nil ||
			unknown.JSONDefault == nil || unknown.JSONDefault.Code != protocol.NotFound {
			t.Fatalf("unknown provider = (%d, %+v, %v), want the typed 404", unknown.HTTPResponse.StatusCode, unknown.JSONDefault, err)
		}

		// A slash provider identity is refused at creation by the complete
		// candidate's validation before any write, under the candidate
		// failure class — and the refusal leaves the owning file, the
		// configuration and warning revisions, and the event stream
		// untouched. The baseline and the post-assertions use the existing
		// refusal helpers; the mounted response's class is checked first
		// and then re-pinned through the helper with the same class.
		subscription, err := r.Subscribe(64)
		if err != nil {
			t.Fatalf("subscribe for the slash refusal baseline: %v", err)
		}
		defer subscription.Close()
		slashBefore, slashGeneration, slashWarnRev := runtimeMutationBaseline(t, r)
		slash := rawProtocol(t, http.MethodPost, protocolTarget(ps, "/v1/providers"), ps.credential,
			`{"id":"has/slash","provider":{"base_url":"https://x.test/v1"},"models":{"m":{"context_window":4096}}}`)
		if slash.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("slash provider creation = %d, want the typed candidate failure 422", slash.StatusCode)
		}
		if typed := decodeProtocolError(t, slash); typed.Code != protocol.Configuration {
			t.Fatalf("slash provider creation error = %+v, want configuration", typed)
		}
		assertRuntimeMutationRefused(t, r, subscription, slashBefore, slashGeneration, slashWarnRev,
			fmt.Errorf("mounted slash refusal: %w", ErrConfiguration), ErrConfiguration)

		// A corrupt stored Session answers corrupt with its session_id
		// member; the corrupt row is absent from the list while its valid
		// sibling stays.
		workspace := filepath.Join(e.home, "errors")
		corruptID := newLifecycleID(t)
		lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: corruptID, Kind: harness.RegisterSession}, `{"not":"a session register"}`)
		valid := projectionSession(t, r, workspace, "solo").Identity.SessionID
		corrupt, err := client.GetSessionHydrationWithResponse(ctx, corruptID)
		if err != nil || corrupt.JSONDefault == nil || corrupt.JSONDefault.Code != protocol.Corrupt ||
			corrupt.HTTPResponse.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("corrupt session = (%d, %+v, %v), want the typed corrupt 422", corrupt.HTTPResponse.StatusCode, corrupt.JSONDefault, err)
		}
		if corrupt.JSONDefault.SessionId == nil || *corrupt.JSONDefault.SessionId != corruptID {
			t.Fatalf("corrupt error session id = %v, want the corrupt session's identity", corrupt.JSONDefault.SessionId)
		}
		listed, err := client.ListSessionsWithResponse(ctx, &protocol.ListSessionsParams{Workspace: workspace, Lifecycle: "open"})
		if err != nil || listed.JSON200 == nil || len(*listed.JSON200) != 1 || (*listed.JSON200)[0].SessionId != valid {
			body, _ := json.Marshal(listed.JSON200)
			t.Fatalf("list around corruption = %s, want only the valid sibling", body)
		}

		// An invalid settings write fails invalid before any candidate work,
		// with the previous revision intact.
		before, err := client.GetConfigurationWithResponse(ctx)
		if err != nil || before.JSON200 == nil {
			t.Fatalf("read before invalid settings: %v", err)
		}
		invalidSettings := rawProtocol(t, http.MethodPut, protocolTarget(ps, "/v1/configuration/settings"), ps.credential,
			`{"settings":{"sessions":{"auto_archive":true,"archive_after_days":-5,"delete_after_archive_days":0},"plugins":{"unknown-plugin":{}}}}`)
		if invalidSettings.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid settings = %d, want the typed invalid 400", invalidSettings.StatusCode)
		}
		if typed := decodeProtocolError(t, invalidSettings); typed.Code != protocol.Invalid {
			t.Fatalf("invalid settings error = %+v, want invalid", typed)
		}
		after, err := client.GetConfigurationWithResponse(ctx)
		if err != nil || after.JSON200 == nil {
			t.Fatalf("read after invalid settings: %v", err)
		}
		if after.JSON200.ConfigurationRevision.Generation != before.JSON200.ConfigurationRevision.Generation {
			t.Fatal("an invalid candidate changed the published revision")
		}

		// The settings outer shape stays closed and its plugins member is a
		// required complete map: a missing or null member, a wrong-typed
		// member, a null document value, and an unknown settings member are
		// all invalid-request failures before any candidate work.
		for _, row := range []struct{ name, body string }{
			{"missing plugins member", `{"settings":{"sessions":{"auto_archive":true,"archive_after_days":1,"delete_after_archive_days":0}}}`},
			{"null plugins member", `{"settings":{"sessions":{"auto_archive":true,"archive_after_days":1,"delete_after_archive_days":0},"plugins":null}}`},
			{"wrong-typed plugins member", `{"settings":{"sessions":{"auto_archive":true,"archive_after_days":1,"delete_after_archive_days":0},"plugins":[]}}`},
			{"null document value", `{"settings":{"sessions":{"auto_archive":true,"archive_after_days":1,"delete_after_archive_days":0},"plugins":{"alpha":null}}}`},
			{"unknown settings member", `{"settings":{"sessions":{"auto_archive":true,"archive_after_days":1,"delete_after_archive_days":0},"plugins":{},"forged":1}}`},
		} {
			response := rawProtocol(t, http.MethodPut, protocolTarget(ps, "/v1/configuration/settings"), ps.credential, row.body)
			if response.StatusCode != http.StatusBadRequest {
				response.Body.Close()
				t.Fatalf("%s = %d, want the typed invalid 400", row.name, response.StatusCode)
			}
			if typed := decodeProtocolError(t, response); typed.Code != protocol.Invalid {
				t.Fatalf("%s error = %+v, want invalid", row.name, typed)
			}
		}
		finalRead, err := client.GetConfigurationWithResponse(ctx)
		if err != nil || finalRead.JSON200 == nil {
			t.Fatalf("read after settings shape failures: %v", err)
		}
		if finalRead.JSON200.ConfigurationRevision.Generation != before.JSON200.ConfigurationRevision.Generation {
			t.Fatal("a rejected settings shape changed the published revision")
		}

		// A restore attempt against a busy Session answers the conflict
		// class from the one idle gate.
		session := projectionSession(t, r, filepath.Join(e.home, "busy"), "solo").Identity.SessionID
		gate := make(chan struct{})
		e.server.setHold(gate)
		defer close(gate)
		if _, err := client.SubmitSessionWithResponse(ctx, session, protocol.SubmitRequest{
			OperationId: "op-1", Mode: "regular",
			Content: []protocol.ContentPart{textContentPart(t, "busy", nil)},
		}); err != nil {
			t.Fatalf("busy submit: %v", err)
		}
		conflict, err := client.RevertSessionCodeWithResponse(ctx, session, protocol.RevertCodeRequest{BoundaryOperationId: "op-1"})
		if err != nil || conflict.JSONDefault == nil {
			t.Fatalf("busy revert = (%v, %+v)", err, conflict.JSONDefault)
		}
		conflictError, err := conflict.JSONDefault.AsError()
		if err != nil || conflictError.Code != protocol.Conflict || conflict.HTTPResponse.StatusCode != http.StatusConflict {
			t.Fatalf("busy revert = (%d, %+v, %v), want the typed conflict 409", conflict.HTTPResponse.StatusCode, conflictError, err)
		}
	})

	// The classifier table for the classes a wire request cannot reach
	// deterministically: the Runtime's own closure sentinel is the closed
	// class alone, while plain caller cancellation, deadline, storage, and
	// unclassified errors follow the default internal rule.
	classes := []struct {
		err    error
		code   protocol.ErrorCode
		status int
	}{
		{fmt.Errorf("wrapped: %w", ErrClosed), protocol.Closed, http.StatusGone},
		{context.Canceled, protocol.Internal, http.StatusInternalServerError},
		{context.DeadlineExceeded, protocol.Internal, http.StatusInternalServerError},
		{fmt.Errorf("engine: %w", harness.ErrStorage), protocol.Storage, http.StatusInternalServerError},
		{errors.New("anything unclassified"), protocol.Internal, http.StatusInternalServerError},
	}
	for _, row := range classes {
		code, status := classifyProtocolError(row.err)
		if code != row.code || status != row.status {
			t.Fatalf("classify(%v) = (%s, %d), want (%s, %d)", row.err, code, status, row.code, row.status)
		}
	}
}

// TestProtocolServerForkAndHydrationRefuseWarmMarkedCorruptSession pins the
// warm corruption row of the mounted transport contract through the real
// listener and the generated client: the durable source register is corrupted
// while the mounted Runtime holds the warm source coordinator, so the real
// fork transition over HTTP — POST /v1/sessions/{id}/fork — fails its
// store-wide transaction on the corrupted register, sticks the sticky marker
// on the warm source, and answers its typed corrupt response naming the
// session; the mounted hydration over HTTP — GET /v1/sessions/{id}/hydration,
// built on the same SnapshotSession capture — then refuses with the same typed
// corrupt body and session identity while the valid sibling still hydrates
// and stays listed. This complements the cold corrupt hydration answer inside
// TestProtocolServerErrorClasses: the cold row seeds an undecodable register
// before any materialization, while this row proves the marker a real
// transition installs on an already-warm coordinator; both surface the one
// error taxonomy with no production seam between them.
func TestProtocolServerForkAndHydrationRefuseWarmMarkedCorruptSession(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)

		source, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{
			Workspace: filepath.Join(e.home, "marked-src"), AgentType: "solo",
		})
		if err != nil || source.JSON201 == nil {
			t.Fatalf("create source over HTTP: %v (typed %+v)", err, source.JSONDefault)
		}
		sourceID := source.JSON201.SessionId
		sibling, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{
			Workspace: filepath.Join(e.home, "marked-sibling"), AgentType: "solo",
		})
		if err != nil || sibling.JSON201 == nil {
			t.Fatalf("create sibling over HTTP: %v (typed %+v)", err, sibling.JSONDefault)
		}
		siblingID := sibling.JSON201.SessionId

		if _, err := client.SubmitSessionWithResponse(ctx, sourceID, protocol.SubmitRequest{
			OperationId: "op-src", Mode: "regular",
			Content: []protocol.ContentPart{textContentPart(t, "boundary", nil)},
		}); err != nil {
			t.Fatalf("source submit over HTTP: %v", err)
		}
		awaitOperation(t, r, sourceID, "op-src", harness.OperationSuccess)
		awaitIdleSession(t, r, sourceID)

		before, err := client.GetSessionHydrationWithResponse(ctx, sourceID)
		if err != nil || before.JSON200 == nil {
			t.Fatalf("hydration before the corruption: %v (typed %+v)", err, before.JSONDefault)
		}
		// The fork boundary is resolved from the mounted response itself: the
		// committed user-origin input item the server projected.
		boundaryItem := ""
		for _, item := range before.JSON200.Conversation.Items {
			input, asInput := item.AsInputItem()
			if asInput != nil || input.Origin != protocol.InputOriginUser {
				continue
			}
			boundaryItem = input.ItemId
		}
		if boundaryItem == "" {
			t.Fatalf("the mounted hydration carries no user-origin input item: %+v", before.JSON200.Conversation.Items)
		}

		// The durable register is corrupted while the mounted Runtime holds
		// the warm source coordinator, so the sticky marker the HTTP fork
		// refusal reports is the real transition's, not a test injection.
		corruptSessionRegister(t, store, sourceID)

		refused, err := client.ForkSessionWithResponse(ctx, sourceID, protocol.ForkRequest{
			BoundaryItemId: boundaryItem,
			OperationId:    "op-fork-marked",
			Content:        []protocol.ContentPart{textContentPart(t, "forked", nil)},
		})
		if err != nil || refused.JSONDefault == nil {
			t.Fatalf("fork over HTTP = (%d, %+v, %v), want the typed corrupt refusal", refused.HTTPResponse.StatusCode, refused.JSONDefault, err)
		}
		if refused.JSONDefault.Code != protocol.Corrupt || refused.HTTPResponse.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("fork over HTTP = (%d, %+v), want the typed corrupt 422", refused.HTTPResponse.StatusCode, refused.JSONDefault)
		}
		if refused.JSONDefault.SessionId == nil || *refused.JSONDefault.SessionId != sourceID {
			t.Fatalf("fork corrupt error session id = %v, want the marked source identity", refused.JSONDefault.SessionId)
		}

		after, err := client.GetSessionHydrationWithResponse(ctx, sourceID)
		if err != nil || after.JSONDefault == nil || after.JSONDefault.Code != protocol.Corrupt ||
			after.HTTPResponse.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("hydration of the marked warm source = (%d, %+v, %v), want the typed corrupt 422", after.HTTPResponse.StatusCode, after.JSONDefault, err)
		}
		if after.JSONDefault.SessionId == nil || *after.JSONDefault.SessionId != sourceID {
			t.Fatalf("hydration corrupt error session id = %v, want the marked source identity", after.JSONDefault.SessionId)
		}

		siblingHydration, err := client.GetSessionHydrationWithResponse(ctx, siblingID)
		if err != nil || siblingHydration.JSON200 == nil || siblingHydration.JSON200.Session.SessionId != siblingID {
			t.Fatalf("sibling hydration = (%d, %+v, %v), want the valid sibling read", siblingHydration.HTTPResponse.StatusCode, siblingHydration.JSON200, err)
		}
		listed, err := client.ListSessionsWithResponse(ctx, &protocol.ListSessionsParams{
			Workspace: filepath.Join(e.home, "marked-sibling"), Lifecycle: "open",
		})
		if err != nil || listed.JSON200 == nil || len(*listed.JSON200) != 1 || (*listed.JSON200)[0].SessionId != siblingID {
			body, _ := json.Marshal(listed.JSON200)
			t.Fatalf("sibling list = %s, want exactly the valid sibling", body)
		}
		sourceListed, err := client.ListSessionsWithResponse(ctx, &protocol.ListSessionsParams{
			Workspace: filepath.Join(e.home, "marked-src"), Lifecycle: "open",
		})
		if err != nil || sourceListed.JSON200 == nil || len(*sourceListed.JSON200) != 0 {
			body, _ := json.Marshal(sourceListed.JSON200)
			t.Fatalf("marked source list = %s, want the corrupt source omitted", body)
		}
	})
}

// TestProtocolServerRevertPartialFailure pins the restore traversal contract:
// a traversal I/O failure after earlier groups changed answers with the
// accumulated result — restored and skipped members with the error member and
// the mapped non-2xx status — never a replacement empty error, while a
// pre-traversal refusal answers the plain typed error.
func TestProtocolServerRevertPartialFailure(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)
	client := protocolClient(t, ps)
	ctx := context.Background()

	// One Session with two recorded code groups: the newest group (the
	// second admitted Operation) restores first, and the older group's
	// store directory becomes unreadable so the reverse traversal fails
	// after the newest group already changed files.
	workspace := filepath.Join(e.home, "revert")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	session := projectionSession(t, r, workspace, "solo").Identity.SessionID
	older := submitCodeOperation(t, r, session, "op-1", "older turn")
	newest := submitCodeOperation(t, r, session, "op-2", "newest turn")
	olderTarget := filepath.Join(workspace, "older.txt")
	newestTarget := filepath.Join(workspace, "newest.txt")
	if err := os.WriteFile(newestTarget, []byte("v0"), 0o600); err != nil {
		t.Fatalf("write the newest preimage: %v", err)
	}
	captureCodeMutation(t, codeGroupRoot(r, session, older.Admission.AdmittedEntry.EntryID), olderTarget, olderTarget, []byte("older"))
	captureCodeMutation(t, codeGroupRoot(r, session, newest.Admission.AdmittedEntry.EntryID), newestTarget, newestTarget, []byte("newest"))
	awaitRestorableSession(t, r, session)
	// The older group's recorded meta becomes unreadable after the capture:
	// the newest group restores first, then the older group's turn errors.
	olderTurn := filepath.Join(codeGroupRoot(r, session, older.Admission.AdmittedEntry.EntryID), "snapshots", "1")
	entries, err := os.ReadDir(olderTurn)
	if err != nil || len(entries) != 1 {
		t.Fatalf("older group turn entries = (%d, %v), want one recorded entry", len(entries), err)
	}
	if err := os.WriteFile(filepath.Join(olderTurn, entries[0].Name(), "meta.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("corrupt the older group's meta: %v", err)
	}

	response, err := client.RevertSessionCodeWithResponse(ctx, session, protocol.RevertCodeRequest{BoundaryOperationId: "op-1"})
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if response.HTTPResponse.StatusCode == http.StatusOK {
		t.Fatalf("partial restore answered 200: %s", string(response.Body))
	}
	var result protocol.CodeRevertResult
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatalf("decode partial restore body %s: %v", string(response.Body), err)
	}
	if result.Error == nil || result.Error.Code != protocol.Internal {
		t.Fatalf("partial restore error member = %+v, want the accumulated internal error", result.Error)
	}
	if !slices.Contains(result.Restored, newestTarget) {
		t.Fatalf("partial restore restored = %v, want the newest group's file before the failure", result.Restored)
	}
	if restored, err := os.ReadFile(newestTarget); err != nil || string(restored) != "v0" {
		t.Fatalf("the newest group's file = (%q, %v), want its restored pre-turn content", restored, err)
	}

	// A pre-traversal refusal answers the plain typed error with no result
	// members: an unknown boundary never opens a store.
	refusal, err := client.RevertSessionCodeWithResponse(ctx, session, protocol.RevertCodeRequest{BoundaryOperationId: "ghost-op"})
	if err != nil {
		t.Fatalf("unknown-boundary revert: %v", err)
	}
	if refusal.JSONDefault == nil {
		t.Fatalf("unknown-boundary revert = %s, want the typed error branch", string(refusal.Body))
	}
	typed, err := refusal.JSONDefault.AsError()
	if err != nil {
		t.Fatalf("decode the refused revert's error branch: %v", err)
	}
	if typed.Code != protocol.Invalid || refusal.HTTPResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown-boundary revert = (%d, %+v), want the typed invalid 400", refusal.HTTPResponse.StatusCode, typed)
	}
}

// TestProtocolServerIntervalProtectionAndAvailability proves the artifact
// interval rows through the mounted generated client: a parked mounted
// restore owns the Session's artifact interval, so a mounted deletion cannot
// destroy the protected state, a second mounted rewind conflicts, the
// metadata-only agent-type change stays available, and the parked restore
// completes with its own result through the client once its FIFO released.
func TestProtocolServerIntervalProtectionAndAvailability(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		ctx := context.Background()

		workspace := filepath.Join(e.home, "interval-mounted")
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		session := projectionSession(t, r, workspace, "solo").Identity.SessionID
		op := submitCodeOperation(t, r, session, "op-1", "only")
		awaitRestorableSession(t, r, session)
		file := filepath.Join(workspace, "mounted.txt")
		if err := os.WriteFile(file, []byte("v1"), 0o600); err != nil {
			t.Fatalf("write v1: %v", err)
		}
		fifo := seedFIFOCodeEntry(t, codeGroupRoot(r, session, op.Admission.AdmittedEntry.EntryID), file, "v1")

		// The mounted restore parks inside its traversal: its HTTP handler is
		// mid-restore while the client waits.
		type mountedRestore struct {
			response *protocol.RevertSessionCodeResponse
			err      error
		}
		parked := make(chan mountedRestore, 1)
		go func() {
			response, err := client.RevertSessionCodeWithResponse(ctx, session, protocol.RevertCodeRequest{BoundaryOperationId: "op-1"})
			parked <- mountedRestore{response: response, err: err}
		}()
		// The shared FIFO park returns only once the mounted restore's
		// reader is parked in restoreFile's os.Open.
		park := parkFIFO(t, fifo)
		defer park.flush()

		// A mounted deletion cannot destroy the protected state.
		if del, err := client.DeleteSessionWithResponse(ctx, session); err != nil || del.JSONDefault == nil ||
			del.HTTPResponse.StatusCode != http.StatusConflict {
			body, _ := json.Marshal(del.JSONDefault)
			t.Fatalf("mounted delete during a parked restore = (%d, %s, %v), want the typed conflict 409", del.HTTPResponse.StatusCode, body, err)
		}
		if hydration, err := client.GetSessionHydrationWithResponse(ctx, session); err != nil || hydration.JSON200 == nil {
			t.Fatalf("hydration after the refused delete = (%v, %+v), want the surviving Session", err, hydration.JSON200)
		}

		// A second mounted rewind conflicts.
		if rewind, err := client.RevertSessionCodeWithResponse(ctx, session, protocol.RevertCodeRequest{BoundaryOperationId: "op-1"}); err != nil ||
			rewind.JSONDefault == nil || rewind.HTTPResponse.StatusCode != http.StatusConflict {
			body, _ := json.Marshal(rewind.JSONDefault)
			t.Fatalf("second mounted rewind = (%d, %s, %v), want the typed conflict 409", rewind.HTTPResponse.StatusCode, body, err)
		}

		// The metadata-only transition stays available inside the interval.
		changed, err := client.SetSessionAgentTypeWithResponse(ctx, session, protocol.SetSessionAgentTypeRequest{AgentType: "worker"})
		if err != nil || changed.JSON200 == nil || changed.JSON200.AgentType != "worker" {
			t.Fatalf("mounted agent-type change during a parked restore = (%v, %+v), want the changed selection", err, changed.JSON200)
		}

		// The parked mounted restore completes with its own result.
		park.release("restored")
		var out mountedRestore
		select {
		case out = <-parked:
		case <-time.After(10 * time.Second):
			t.Fatal("the parked mounted restore never returned after its FIFO completed")
		}
		if out.err != nil || out.response.JSON200 == nil {
			t.Fatalf("mounted restore = (%+v, %v), want the restored result", out.response, out.err)
		}
		if len(out.response.JSON200.Restored) != 1 || out.response.JSON200.Restored[0] != file {
			t.Fatalf("restored = %v, want the parked group's restore", out.response.JSON200.Restored)
		}
		if data, err := os.ReadFile(file); err != nil || string(data) != "restored" {
			t.Fatalf("file = (%q, %v), want the FIFO content", data, err)
		}
	})
}
