package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/melta"
	"github.com/MMinasyan/lightcode/protocol"
)

func mountedOwner(t *testing.T, ctx context.Context, options melta.Options) (*melta.Melta, *ProtocolHost, *ProtocolServer) {
	t.Helper()
	host := NewProtocolHost()
	options.BeforeRelease = host.BeforeRelease
	owner, err := melta.Open(ctx, options)
	testassert.NoError(t, err)
	server, err := host.OpenProtocol(ctx, owner)
	testassert.NoError(t, err)
	t.Cleanup(func() {
		_ = owner.Close(context.Background())
	})
	return owner, host, server
}

func mountedClient(t *testing.T, server *ProtocolServer) *protocol.ClientWithResponses {
	t.Helper()
	client, err := protocol.NewClientWithResponses(server.Endpoint(), protocol.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+server.credential)
		return nil
	}))
	testassert.NoError(t, err)
	return client
}

func rawMounted(t *testing.T, method, target, bearer, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	testassert.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	testassert.NoError(t, err)
	return resp
}

func decodeMountedError(t *testing.T, resp *http.Response) protocol.Error {
	t.Helper()
	defer resp.Body.Close()
	var body protocol.Error
	testassert.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// TestProtocolMountedProjectionAndOwnership proves the generated client over
// the real listener against the public owner: identity, authenticated
// projection reads, Session lifecycle, strict-wire rejection, and discovery
// withdrawal at the owner's release boundary.
func TestProtocolMountedProjectionAndOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	options := testOwnerOptions(t)
	owner, _, server := mountedOwner(t, ctx, options)
	client := mountedClient(t, server)
	workspace := t.TempDir()

	health, err := client.GetHealthWithResponse(ctx)
	testassert.Require(t, err == nil && health.JSON200 != nil, "GetHealth = (%+v, %v)", health, err)
	testassert.Require(t, health.JSON200.InstanceId == owner.InstanceID() && health.JSON200.ProtocolVersion == protocol.N14, "health = %+v, want the owner instance and N14", health.JSON200)
	warnings, err := client.GetWarningsWithResponse(ctx)
	testassert.Require(t, err == nil && warnings.JSON200 != nil && warnings.JSON200.WarningsRevision.InstanceId == owner.InstanceID(), "warnings = (%+v, %v), want the owner-qualified warning revision", warnings, err)

	created, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: workspace})
	testassert.Require(t, err == nil && created.JSON201 != nil, "CreateSession = (%+v, %v)", created, err)
	sessionID := created.JSON201.SessionId
	hydration, err := client.GetSessionHydrationWithResponse(ctx, sessionID)
	testassert.Require(t, err == nil && hydration.JSON200 != nil, "GetSessionHydration = (%+v, %v)", hydration, err)
	testassert.Require(t, hydration.JSON200.Session.SessionId == sessionID && hydration.JSON200.SessionRevision.InstanceId == owner.InstanceID(), "hydration = %+v, want the created Session qualified with the owner instance", hydration.JSON200)
	sessions, err := client.ListSessionsWithResponse(ctx, &protocol.ListSessionsParams{Workspace: workspace, Lifecycle: protocol.Open})
	testassert.Require(t, err == nil && sessions.JSON200 != nil && len(*sessions.JSON200) == 1, "ListSessions = (%+v, %v), want the created Session", sessions, err)

	// The wire carries no attribution: an unknown origin member fails the
	// strict decode as invalid and publishes nothing.
	forged := rawMounted(t, http.MethodPost, server.Endpoint()+"/v1/sessions/"+sessionID+"/submit", server.credential, `{"operation_id":"op-forged","mode":"regular","origin":"plugin","content":[{"kind":"text","text":"forged"}]}`)
	typed := decodeMountedError(t, forged)
	testassert.Require(t, forged.StatusCode == http.StatusBadRequest && typed.Code == protocol.ErrorCodeInvalid, "forged submit = (%d, %+v), want typed invalid 400", forged.StatusCode, typed)

	// Authentication and unknown routes answer the schema's typed errors.
	unauthorized := rawMounted(t, http.MethodGet, server.Endpoint()+"/v1/health", "", "")
	typed = decodeMountedError(t, unauthorized)
	testassert.Require(t, unauthorized.StatusCode == http.StatusUnauthorized && typed.Code == protocol.ErrorCodeUnauthorized, "unauthenticated health = (%d, %+v)", unauthorized.StatusCode, typed)
	unknown := rawMounted(t, http.MethodGet, server.Endpoint()+"/v1/no-such-operation", server.credential, "")
	typed = decodeMountedError(t, unknown)
	testassert.Require(t, unknown.StatusCode == http.StatusNotFound && typed.Code == protocol.ErrorCodeNotFound, "unknown route = (%d, %+v)", unknown.StatusCode, typed)
	wrongMethod := rawMounted(t, http.MethodPost, server.Endpoint()+"/v1/health", server.credential, "")
	typed = decodeMountedError(t, wrongMethod)
	testassert.Require(t, wrongMethod.StatusCode == http.StatusNotFound && typed.Code == protocol.ErrorCodeNotFound, "wrong method = (%d, %+v), want the typed catch-all", wrongMethod.StatusCode, typed)

	// A second attachment while one exists fails invalid.
	{
		_, err := server.host.OpenProtocol(ctx, owner)
		testassert.Require(t, err != nil, "second protocol attachment succeeded")
	}

	recordPath := filepath.Join(t.TempDir(), "runtime.json")
	testassert.NoError(t, server.PublishDiscovery(recordPath))
	{
		data, err := os.ReadFile(recordPath)
		testassert.Require(t, err == nil && strings.Contains(string(data), owner.InstanceID()), "discovery record = (%q, %v), want the owner instance", data, err)
	}

	archived, err := client.ArchiveSessionWithResponse(ctx, sessionID)
	testassert.Require(t, err == nil && archived.JSON200 != nil, "ArchiveSession = (%+v, %v)", archived, err)
	deleted, err := client.DeleteSessionWithResponse(ctx, sessionID)
	testassert.Require(t, err == nil && deleted.JSON200 != nil && len(deleted.JSON200.CleanupFailures) == 0, "DeleteSession = (%+v, %v), want the 200 result with no cleanup failures", deleted, err)

	// Owner cancellation withdraws the listener and the discovery record
	// before the ownership lock releases.
	cancel()
	testassert.NoError(t, owner.Close(context.Background()))
	{
		_, err := os.Stat(recordPath)
		testassert.Require(t, os.IsNotExist(err), "discovery record survived owner shutdown: %v", err)
	}
	{
		conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.Endpoint(), "http://"), time.Second)
		if err == nil {
			_ = conn.Close()
		}
		testassert.Require(t, err != nil, "the protocol listener still accepts after owner shutdown")
	}
	{
		_, err := server.host.OpenProtocol(context.Background(), owner)
		testassert.Require(t, err != nil, "a protocol listener attached after owner shutdown")
	}
}

// TestProtocolMountedEvents proves the mounted SSE stream delivers qualified
// configuration and Session events to the generated client's raw reader and
// holds no owner admission across its wait.
func TestProtocolMountedEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	options := testOwnerOptions(t)
	owner, _, server := mountedOwner(t, ctx, options)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.Endpoint()+"/v1/events", nil)
	testassert.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+server.credential)
	resp, err := http.DefaultClient.Do(request)
	testassert.Require(t, err == nil && resp.StatusCode == http.StatusOK, "GET events = (%+v, %v)", resp, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	readEvent := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			testassert.Require(t, !time.Now().After(deadline), "no %s event arrived on the mounted stream", want)
			line, err := reader.ReadString('\n')
			testassert.NoError(t, err)
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event protocol.Event
			testassert.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &event))
			kind, err := event.Discriminator()
			testassert.NoError(t, err)
			switch kind {
			case "configuration_changed":
				value, err := event.AsConfigurationChangedEvent()
				testassert.Require(t, err == nil && value.ConfigurationRevision.InstanceId == owner.InstanceID(), "configuration event = %+v, want the owner-qualified revision", value)
			case "session_changed":
				value, err := event.AsSessionChangedEvent()
				testassert.Require(t, err == nil && value.SessionRevision.InstanceId == owner.InstanceID(), "session event = %+v, want the owner-qualified revision", value)
			}
			if kind == want {
				return
			}
		}
	}
	// One reload publishes a configuration_changed event after the stream
	// attached.
	{
		source := options.Configuration
		testassert.NoError(t, source.Write(ctx, []byte(`{"sessions":{"archive_after_days":7}}`)))
		_, err := owner.Reload(ctx, testCaller(t, owner))
		testassert.NoError(t, err)
	}
	readEvent("configuration_changed")
	{
		created, err := owner.CreateSession(ctx, testCaller(t, owner), t.TempDir(), nil)
		testassert.NoError(t, err)
		_ = created
	}
	readEvent("session_changed")
}

// TestProtocolHostRejectsPublishWhileClosing proves a host that has begun
// release refuses a later discovery publication instead of publishing a
// record it cannot withdraw.
func TestProtocolHostRejectsPublishWhileClosing(t *testing.T) {
	ctx := context.Background()
	options := testOwnerOptions(t)
	owner, host, server := mountedOwner(t, ctx, options)
	testassert.NoError(t, host.BeforeRelease())
	{
		err := server.PublishDiscovery(filepath.Join(t.TempDir(), "runtime.json"))
		testassert.Require(t, err != nil, "a closing host published a discovery record")
	}
	testassert.NoError(t, owner.Close(ctx))
}

// TestProtocolHostDiscoveryPublicationAndWithdrawal proves one publication
// per server, the typed duplicate refusal, and withdrawal of the remembered
// record at the owner's release boundary even when publication races it.
func TestProtocolHostDiscoveryPublicationAndWithdrawal(t *testing.T) {
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		options := testOwnerOptions(t)
		owner, _, server := mountedOwner(t, ctx, options)
		path := filepath.Join(t.TempDir(), "runtime.json")
		var publishErr error
		var published sync.WaitGroup
		published.Add(1)
		go func() {
			defer published.Done()
			publishErr = server.PublishDiscovery(path)
		}()
		cancel()
		testassert.NoError(t, owner.Close(context.Background()))
		published.Wait()
		{
			_, err := os.Stat(path)
			testassert.Require(t, os.IsNotExist(err), "iteration %d: discovery record survived a release racing publication (publish err %v)", i, publishErr)
		}
	}
	ctx := context.Background()
	options := testOwnerOptions(t)
	owner, _, server := mountedOwner(t, ctx, options)
	path := filepath.Join(t.TempDir(), "runtime.json")
	testassert.NoError(t, server.PublishDiscovery(path))
	{
		err := server.PublishDiscovery(filepath.Join(t.TempDir(), "second.json"))
		testassert.Require(t, err != nil, "a second discovery publication succeeded")
	}
	testassert.NoError(t, owner.Close(ctx))
	{
		_, err := os.Stat(path)
		testassert.Require(t, os.IsNotExist(err), "the published discovery record survived owner shutdown")
	}
}

// TestProtocolHostReleaseEndsStreams proves owner shutdown ends an attached
// events stream through the host release, not through an owner admission
// held across the network wait.
func TestProtocolHostReleaseEndsStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	options := testOwnerOptions(t)
	owner, _, server := mountedOwner(t, ctx, options)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.Endpoint()+"/v1/events", nil)
	testassert.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+server.credential)
	resp, err := http.DefaultClient.Do(request)
	testassert.Require(t, err == nil && resp.StatusCode == http.StatusOK, "GET events = (%+v, %v)", resp, err)
	defer resp.Body.Close()
	cancel()
	testassert.NoError(t, owner.Close(context.Background()))
	done := make(chan error, 1)
	go func() {
		_, err := bufio.NewReader(resp.Body).ReadString('\n')
		done <- err
	}()
	select {
	case err := <-done:
		testassert.Require(t, err != nil, "the events stream stayed readable after owner shutdown")
	case <-time.After(10 * time.Second):
		t.Fatal("the events stream did not end at owner shutdown")
	}
}

// TestProtocolHostReleaseBeforeAttachment proves a host that has begun
// release cannot publish a later listener: the bind is rejected and no server
// is attached.
func TestProtocolHostReleaseBeforeAttachment(t *testing.T) {
	owner, _, _ := mountedOwner(t, context.Background(), testOwnerOptions(t))
	testassert.NoError(t, owner.Close(context.Background()))
	host := NewProtocolHost()
	testassert.NoError(t, host.BeforeRelease())
	{
		attached, err := host.OpenProtocol(context.Background(), owner)
		testassert.Require(t, err != nil && attached == nil, "OpenProtocol after release = (%v, %v), want a closed refusal", attached, err)
	}
}

// TestProtocolMountedSettingsSectionsRequireObjects proves a non-object
// baseline settings member is the typed invalid 400 over HTTP, from the same
// owner producer embedded callers use, rather than a candidate configuration
// 422.
func TestProtocolMountedSettingsSectionsRequireObjects(t *testing.T) {
	ctx := context.Background()
	_, _, server := mountedOwner(t, ctx, testOwnerOptions(t))
	const body = `{"settings":{"sessions":{"auto_archive":false,"archive_after_days":0,"delete_after_archive_days":0},"tools":{},"jobs":{},"compaction":{},"extensions":{}}}`
	for _, member := range []string{"tools", "jobs", "compaction"} {
		response := rawMounted(t, http.MethodPut, server.Endpoint()+"/v1/configuration/settings", server.credential,
			strings.Replace(body, `"`+member+`":{}`, `"`+member+`":42`, 1))
		typed := decodeMountedError(t, response)
		testassert.Require(t, response.StatusCode == http.StatusBadRequest && typed.Code == protocol.ErrorCodeInvalid, "%s shape = (%d, %+v), want the typed invalid 400", member, response.StatusCode, typed)
	}
}

// TestProtocolHostDiscoveryClaimReleaseAndConcurrency proves a failed
// publication releases the one-claim slot for a later retry and that
// concurrent publishers produce exactly one record, withdrawn at release.
func TestProtocolHostDiscoveryClaimReleaseAndConcurrency(t *testing.T) {
	ctx := context.Background()
	owner, _, server := mountedOwner(t, ctx, testOwnerOptions(t))
	bad := filepath.Join(t.TempDir(), "missing-parent", "runtime.json")
	testassert.Require(t, server.PublishDiscovery(bad) != nil, "publication into a missing parent succeeded")
	path := filepath.Join(t.TempDir(), "runtime.json")
	testassert.NoError(t, server.PublishDiscovery(path))

	// The claim is now held; every concurrent publisher loses.
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := server.PublishDiscovery(filepath.Join(t.TempDir(), fmt.Sprintf("publisher-%d.json", i))); err != nil {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()
	testassert.Equal(t, failures.Load(), int32(8))
	testassert.NoError(t, owner.Close(ctx))
	{
		_, err := os.Stat(path)
		testassert.Require(t, os.IsNotExist(err), "the discovery record survived owner shutdown")
	}
}

// TestProtocolServerAuthenticationOrder proves the authentication gate runs
// before body decoding and routing: a malformed or unknown request with no or
// wrong credentials is the typed unauthorized answer, never a decode or route
// error.
func TestProtocolServerAuthenticationOrder(t *testing.T) {
	ctx := context.Background()
	_, _, server := mountedOwner(t, ctx, testOwnerOptions(t))
	for _, row := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/sessions", `{"workspace":`},
		{http.MethodPost, "/v1/sessions/garbage/submit", `not json`},
		{http.MethodGet, "/v1/no-such-operation", ""},
	} {
		response := rawMounted(t, row.method, server.Endpoint()+row.path, "", row.body)
		typed := decodeMountedError(t, response)
		testassert.Require(t, response.StatusCode == http.StatusUnauthorized && typed.Code == protocol.ErrorCodeUnauthorized, "%s %s = (%d, %+v), want unauthorized", row.method, row.path, response.StatusCode, typed)
		response = rawMounted(t, row.method, server.Endpoint()+row.path, strings.Repeat("0", 64), row.body)
		typed = decodeMountedError(t, response)
		testassert.Require(t, response.StatusCode == http.StatusUnauthorized && typed.Code == protocol.ErrorCodeUnauthorized, "%s %s wrong credential = (%d, %+v), want unauthorized", row.method, row.path, response.StatusCode, typed)
	}
}

// TestProtocolServerStrictBodies proves the one strict request decoder:
// unknown members, missing or null required members, concatenated documents,
// closed-union shape and enum values all fail the typed invalid class before
// any producer mutation.
func TestProtocolServerStrictBodies(t *testing.T) {
	ctx := context.Background()
	_, _, server := mountedOwner(t, ctx, testOwnerOptions(t))
	workspace := t.TempDir()
	create := func(body string) *http.Response {
		return rawMounted(t, http.MethodPost, server.Endpoint()+"/v1/sessions", server.credential, body)
	}
	reject := func(name string, response *http.Response) {
		t.Helper()
		typed := decodeMountedError(t, response)
		testassert.Require(t, response.StatusCode == http.StatusBadRequest && typed.Code == protocol.ErrorCodeInvalid, "%s = (%d, %+v), want the typed invalid 400", name, response.StatusCode, typed)
	}
	reject("unknown top member", create(fmt.Sprintf(`{"workspace":%q,"note":"x"}`, workspace)))
	reject("missing required member", create(`{}`))
	reject("null declared member", create(`{"workspace":null}`))
	reject("two concatenated documents", create(fmt.Sprintf(`{"workspace":%q} {"workspace":%q}`, workspace, workspace)))
	reject("trailing scalar", create(fmt.Sprintf(`{"workspace":%q} 5`, workspace)))
	reject("top-level null", create(`null`))
	reject("top-level array", create(`[]`))
	reject("wrong agent selection shape", create(fmt.Sprintf(`{"workspace":%q,"agent_selection":5}`, workspace)))

	client := mountedClient(t, server)
	created, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: workspace})
	testassert.Require(t, err == nil && created.JSON201 != nil, "create: %v", err)
	session := created.JSON201.SessionId
	submit := func(body string) *http.Response {
		return rawMounted(t, http.MethodPost, server.Endpoint()+"/v1/sessions/"+session+"/submit", server.credential, body)
	}
	reject("unknown union member", submit(`{"operation_id":"op-1","mode":"regular","content":[{"kind":"text","text":"x","forged":1}]}`))
	reject("unknown union discriminator", submit(`{"operation_id":"op-1","mode":"regular","content":[{"kind":"pdf","text":"x"}]}`))
	reject("union null kind member", submit(`{"operation_id":"op-1","mode":"regular","content":[{"kind":null,"text":"x"}]}`))
	reject("union missing required member", submit(`{"operation_id":"op-1","mode":"regular","content":[{"kind":"text"}]}`))
	reject("invalid mode enum", submit(`{"operation_id":"op-1","mode":"fast","content":[{"kind":"text","text":"x"}]}`))
}

// TestProtocolMountedCredentialBodiesAreSecretFree proves no credential value
// reaches a raw HTTP response body or notification frame through the public
// provider mutation path.
func TestProtocolMountedCredentialBodiesAreSecretFree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, server := mountedOwner(t, ctx, testOwnerOptions(t))
	const secret = "sk-raw-wire-secret"
	streamResponse, err := http.DefaultClient.Do(func() *http.Request {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.Endpoint()+"/v1/events", nil)
		testassert.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+server.credential)
		return request
	}())
	testassert.Require(t, err == nil && streamResponse.StatusCode == http.StatusOK, "GET events = (%+v, %v)", streamResponse, err)
	defer streamResponse.Body.Close()

	create := rawMounted(t, http.MethodPost, server.Endpoint()+"/v1/providers", server.credential,
		`{"id":"rawscan","provider":{"base_url":"https://rawscan.test/v1","api_key_env":"RAW_SCAN_KEY","discovery":false},"models":{"m":{"context_window":4096}},"api_key":"`+secret+`"}`)
	var createBody strings.Builder
	_, _ = io.Copy(&createBody, create.Body)
	create.Body.Close()
	testassert.Require(t, create.StatusCode == http.StatusOK && strings.Contains(createBody.String(), `"rawscan"`), "raw create = %d: %s", create.StatusCode, createBody.String())
	testassert.Require(t, !strings.Contains(createBody.String(), secret), "the raw create response carries the credential value")

	connect := rawMounted(t, http.MethodPost, server.Endpoint()+"/v1/providers/connect?provider_id=rawscan", server.credential, `{"api_key":"`+secret+`"}`)
	var connectBody strings.Builder
	_, _ = io.Copy(&connectBody, connect.Body)
	connect.Body.Close()
	testassert.Require(t, !strings.Contains(connectBody.String(), secret), "the raw connect response carries the credential value: %s", connectBody.String())

	scan := bufio.NewReader(streamResponse.Body)
	deadline := time.Now().Add(10 * time.Second)
	for {
		testassert.Require(t, !time.Now().After(deadline), "no raw notification frame observed")
		line, err := scan.ReadString('\n')
		testassert.NoError(t, err)
		testassert.Require(t, !strings.Contains(line, secret), "a raw notification frame carries the credential value: %s", line)
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"configuration_changed"`) {
			return
		}
	}
}

// TestProtocolHostAttachmentReleaseRace proves the attachment/closing guard
// has no window in which a released host publishes a later listener: each
// iteration either refuses the bind or attaches a server that the concurrent
// release immediately closes.
func TestProtocolHostAttachmentReleaseRace(t *testing.T) {
	for i := 0; i < 20; i++ {
		options := testOwnerOptions(t)
		host := NewProtocolHost()
		options.BeforeRelease = host.BeforeRelease
		owner, err := melta.Open(context.Background(), options)
		testassert.NoError(t, err)
		released := make(chan struct{})
		go func() {
			defer close(released)
			testassert.NoError(t, host.BeforeRelease())
		}()
		server, err := host.OpenProtocol(context.Background(), owner)
		<-released
		if err == nil {
			testassert.Require(t, server != nil, "attachment succeeded with a nil server")
			endpoint := strings.TrimPrefix(server.Endpoint(), "http://")
			conn, dialErr := net.DialTimeout("tcp", endpoint, time.Second)
			if dialErr == nil {
				_ = conn.Close()
			}
			testassert.Require(t, dialErr != nil, "iteration %d: the listener stayed open after release", i)
		} else {
			testassert.Require(t, errors.Is(err, melta.ErrClosed), "iteration %d: OpenProtocol = %v, want closed", i, err)
		}
		_ = owner.Close(context.Background())
	}
}

// TestProtocolMountedReloadCarriesOwnerInstance proves the HTTP reload
// response forwards the owner's stamped configuration revision rather than
// reconstructing a generation-only value.
func TestProtocolMountedReloadCarriesOwnerInstance(t *testing.T) {
	ctx := context.Background()
	owner, _, server := mountedOwner(t, ctx, testOwnerOptions(t))
	client := mountedClient(t, server)
	reloaded, err := client.ReloadConfigurationWithResponse(ctx)
	testassert.Require(t, err == nil && reloaded.JSON200 != nil, "ReloadConfiguration = (%+v, %v)", reloaded, err)
	testassert.Require(t, reloaded.JSON200.ConfigurationRevision.Generation == "2" && reloaded.JSON200.ConfigurationRevision.InstanceId == owner.InstanceID(), "reload = %+v, want generation 2 with the owner instance %q", reloaded.JSON200.ConfigurationRevision, owner.InstanceID())
}

// TestProtocolHealthFailsClosedDuringShutdownSettlement proves health stays an
// admitted owner call: while shutdown settlement is blocked, the readiness
// read answers the typed closed error and discovery publication therefore
// writes no record.
func TestProtocolHealthFailsClosedDuringShutdownSettlement(t *testing.T) {
	source := &blockingConfiguration{
		ConfigurationSource: melta.NewValueConfigurationSource([]byte(`{}`)),
		entered:             make(chan struct{}),
		gate:                make(chan struct{}),
	}
	options := testOwnerOptions(t)
	options.Configuration = source
	owner, _, server := mountedOwner(t, context.Background(), options)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(source.gate) }) }
	t.Cleanup(release)

	source.block.Store(true)
	reloadDone := make(chan error, 1)
	go func() {
		_, err := owner.Reload(context.Background(), testCaller(t, owner))
		reloadDone <- err
	}()
	<-source.entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		response := rawMounted(t, http.MethodGet, server.Endpoint()+"/v1/health", server.credential, "")
		typed := decodeMountedError(t, response)
		if response.StatusCode == http.StatusGone {
			testassert.Require(t, typed.Code == protocol.ErrorCodeClosed, "health during settlement = (%d, %+v), want the typed closed error", response.StatusCode, typed)
			break
		}
		testassert.Require(t, time.Now().Before(deadline), "health stayed ready during blocked shutdown settlement: (%d, %+v)", response.StatusCode, typed)
		time.Sleep(5 * time.Millisecond)
	}

	path := filepath.Join(t.TempDir(), "runtime.json")
	testassert.Require(t, server.PublishDiscovery(path) != nil, "discovery was published while health reported closed")
	_, statErr := os.Stat(path)
	testassert.Require(t, os.IsNotExist(statErr), "a discovery record was written while health reported closed")

	release()
	_ = <-reloadDone
	testassert.NoError(t, <-closeDone)
}

// TestProtocolHostReleaseAfterReadinessPreventsPublication proves the final
// release check and record write share the host mutex: release beginning
// after readiness completes but before the write leaves no record.
func TestProtocolHostReleaseAfterReadinessPreventsPublication(t *testing.T) {
	ctx := context.Background()
	_, host, server := mountedOwner(t, ctx, testOwnerOptions(t))
	path := filepath.Join(t.TempDir(), "runtime.json")
	ready := make(chan struct{})
	gate := make(chan struct{})
	published := make(chan error, 1)
	go func() {
		published <- server.publishDiscovery(path, func() error {
			close(ready)
			<-gate
			return nil
		})
	}()
	<-ready
	released := make(chan error, 1)
	go func() { released <- host.BeforeRelease() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		host.mu.Lock()
		started := host.released
		host.mu.Unlock()
		if started {
			break
		}
		testassert.Require(t, !time.Now().After(deadline), "host release never started")
		time.Sleep(time.Millisecond)
	}
	close(gate)
	publishErr := <-published
	testassert.Require(t, errors.Is(publishErr, melta.ErrClosed), "publication after release began = %v, want closed", publishErr)
	{
		_, err := os.Stat(path)
		testassert.Require(t, os.IsNotExist(err), "a discovery record was written after release began")
	}
	testassert.NoError(t, <-released)
}
