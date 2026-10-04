package runtime

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/protocol"
)

// The mounted lifetime contract rows: stream isolation, heartbeat, shutdown
// topology, discovery publication, and the attachment races.

// subscriberCount reads the bounded bus's live subscriber count.
func subscriberCount(r *Runtime) int {
	r.obs.mu.Lock()
	defer r.obs.mu.Unlock()
	return len(r.obs.subs)
}

// dialSmallBuffer opens one raw connection with a minimal receive window so
// an unread events stream blocks its server-side writer after a few
// kilobytes.
func dialSmallBuffer(t *testing.T, ps *ProtocolServer) net.Conn {
	t.Helper()
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, _ string, raw syscall.RawConn) error {
		return raw.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		})
	}}
	conn, err := dialer.Dial("tcp", strings.TrimPrefix(ps.Endpoint(), "http://"))
	if err != nil {
		t.Fatalf("dial small-buffer connection: %v", err)
	}
	return conn
}

// openSSEOverConn hand-writes one authenticated events request over a raw
// connection and reads its response headers; the connection then stays open
// for the caller.
func openSSEOverConn(t *testing.T, conn net.Conn, ps *ProtocolServer) {
	t.Helper()
	request := "GET /v1/events HTTP/1.1\r\nHost: events\r\nAuthorization: Bearer " + ps.credential + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write events request: %v", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("events handshake = (%q, %v), want the 200 status line", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read events headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
}

// TestProtocolServerStreamIsolation pins the one bounded stream contract: a
// never-reading client's own writer blocks on its socket, its overflowing
// subscription is closed alone, its private watcher closes exactly its
// connection even while the socket write blocks, a healthy sibling stream
// keeps receiving, and the Runtime keeps serving ordinary work.
func TestProtocolServerStreamIsolation(t *testing.T) {
	store := storage.NewMemory()
	r, e := openProjectionRuntime(t, store)
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)
	client := protocolClient(t, ps)
	ctx := context.Background()

	session := projectionSession(t, r, filepath.Join(e.home, "flood"), "solo").Identity.SessionID

	// The healthy sibling stream, drained continuously through the flood.
	healthy := openSSEStream(t, ps)
	defer healthy.close()
	healthyCount := atomic.Int64{}
	healthyDone := make(chan struct{})
	go func() {
		defer close(healthyDone)
		for {
			_, ok, err := healthy.readSSEFrame()
			if err != nil || !ok {
				return
			}
			healthyCount.Add(1)
		}
	}()

	// The flood victim: a raw connection with a minimal receive window that
	// never reads a frame. The connection stays referenced for the whole
	// row so its finalizer cannot close it early.
	victimConn := dialSmallBuffer(t, ps)
	defer victimConn.Close()
	openSSEOverConn(t, victimConn, ps)

	// Hold one gated operation so committed work exists across the flood.
	parked := make(chan struct{})
	e.prep.modelGate = parked
	gate := sync.OnceFunc(func() { close(parked) })
	defer gate()
	if _, err := client.SubmitSessionWithResponse(ctx, session, protocol.SubmitRequest{
		OperationId: "op-1", Mode: "regular",
		Content: []protocol.ContentPart{textContentPart(t, "running", nil)},
	}); err != nil {
		t.Fatalf("gated submit: %v", err)
	}
	awaitModelArrival(t, e)

	// Flood bounded by the victim's removal from the bus: large transient
	// frames published one batch at a time. The pauses let the healthy
	// sibling's writer and reader drain every frame while the victim's
	// unread socket fills; once it is full its blocked writer cannot drain,
	// its queue saturates, and the bus removes exactly its subscription
	// while the healthy sibling stays. The victim is never drained before
	// this point — a drain would let its writer catch up and prevent
	// saturation.
	payload := strings.Repeat("x", 32*1024)
	floodScope := operationEventScope("/flood", session, "flood-op")
	overflowed := false
	flooded := 0
	for batch := 0; batch < 800 && !overflowed; batch++ {
		flooded++
		r.obs.publish(func() []Event {
			return []Event{textDeltaEvent(floodScope, flooded, payload)}
		})
		overflowed = subscriberCount(r) == 1
		time.Sleep(time.Millisecond)
	}
	if !overflowed {
		t.Fatalf("the overflowing stream survived %d published frames, want its subscription removed alone", flooded)
	}

	// The removed subscription's watcher closed exactly this connection
	// while its socket write was blocked: draining the buffered window now
	// ends in EOF. A live connection would answer a timeout, which is the
	// forbidden sibling.
	if err := victimConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set victim read deadline: %v", err)
	}
	// io.Copy reports nil only when the peer's EOF ends the read; a live
	// connection would answer the read deadline instead.
	if _, err := io.Copy(io.Discard, victimConn); err != nil {
		t.Fatalf("draining the removed stream's connection ended with %v, want the watcher's EOF", err)
	}

	// The healthy sibling observed a substantial share of the flood, keeps
	// receiving live events after the victim is gone, and ordinary work
	// keeps answering.
	awaitHealthy := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for healthyCount.Load() < want {
			if time.Now().After(deadline) {
				t.Fatalf("the healthy sibling stalled at %d events, want %d", healthyCount.Load(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	awaitHealthy(100)
	before := healthyCount.Load()
	if _, err := r.submitSession(ctx, session, protocol.SubmitRequest{
		OperationId: "op-final", Mode: "queued",
		Content: []protocol.ContentPart{textContentPart(t, "final", nil)},
	}); err != nil {
		t.Fatalf("post-flood submit: %v", err)
	}
	if _, err := client.GetSessionHydrationWithResponse(ctx, session); err != nil {
		t.Fatalf("post-flood session read: %v", err)
	}
	awaitHealthy(before + 1)

	// The gated operation is untouched by the loss: releasing the gate lets
	// it settle success.
	gate()
	e.prep.awaitCleanups(1)
	record := readOperation(t, r, session, "op-1")
	if record.State.Status != harness.OperationSuccess {
		t.Fatalf("the gated operation settled as %q, want success after the stream loss", record.State.Status)
	}
}

// TestProtocolServerHeartbeat pins the 15-second keepalive comment on one
// idle stream.
func TestProtocolServerHeartbeat(t *testing.T) {
	store := storage.NewMemory()
	r, _ := openProjectionRuntime(t, store)
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)

	stream := openSSEStream(t, ps)
	defer stream.close()
	keepalive := make(chan string, 1)
	go func() {
		for {
			line, err := stream.reader.ReadString('\n')
			if err != nil {
				close(keepalive)
				return
			}
			if strings.HasPrefix(line, ":") {
				select {
				case keepalive <- strings.TrimSpace(line):
				default:
				}
				return
			}
		}
	}()
	select {
	case line := <-keepalive:
		if line != ": keepalive" {
			t.Fatalf("keepalive comment = %q, want exactly %q", line, ": keepalive")
		}
	case <-time.After(sseHeartbeatInterval + 5*time.Second):
		t.Fatalf("no keepalive comment within one heartbeat interval (%s)", sseHeartbeatInterval)
	}
}

// TestProtocolServerEventQualification pins the SSE qualification rows: the
// three revisioned variants carry the instance identity through their
// generated constructors and accessors, transient events never grow a
// revision member, and the mounted stream qualifies the configuration
// publication hint it observes.
func TestProtocolServerEventQualification(t *testing.T) {
	const instance = "0123456789abcdef0123456789abcdef"

	warning := warningChangedEvent(runtimeEventScope(), 7)
	qualified := qualifyEvent(warning, instance)
	value, err := qualified.AsWarningChangedEvent()
	if err != nil || value.WarningsRevision.InstanceId != instance || value.WarningsRevision.Revision != "7" {
		t.Fatalf("qualified warning event = (%+v, %v), want the instance-qualified revision", value, err)
	}

	delta := textDeltaEvent(operationEventScope("ws", "session", "op"), 3, "chunk")
	unmodified := qualifyEvent(delta, instance)
	frame, err := json.Marshal(unmodified)
	if err != nil || strings.Contains(string(frame), "instance_id") {
		t.Fatalf("qualified transient event = %s (%v), want no instance member on the wire", frame, err)
	}
	transient, err := unmodified.AsTextDeltaEvent()
	if err != nil || transient.Content != "chunk" || transient.Position != 3 {
		t.Fatalf("qualified transient event = (%+v, %v), want the unchanged payload", transient, err)
	}

	store := storage.NewMemory()
	r, _ := openProjectionRuntime(t, store)
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)
	client := protocolClient(t, ps)
	ctx := context.Background()

	stream := openSSEStream(t, ps)
	defer stream.close()
	if _, err := client.ReloadConfigurationWithResponse(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	frameEvent := readUntilEvent(t, stream, "configuration_changed", 16)
	if got := eventRevisionInstance(t, frameEvent["configuration_revision"], "configuration_changed"); got != ps.instance {
		t.Fatalf("stream configuration revision instance = %q, want the minted %q", got, ps.instance)
	}
}

// TestProtocolServerShutdownTopology pins the shutdown ordering: the listener
// and every active stream close before the admitted-call wait, a separately
// blocked command keeps the close joined until it settles, the discovery
// record withdraws before the owner lock releases, and no attachment can
// happen after shutdown begins.
func TestProtocolServerShutdownTopology(t *testing.T) {
	store := storage.NewMemory()
	blocked := newTargetedBlockStore(store)
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(blocked))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)
	client := protocolClient(t, ps)
	ctx := context.Background()

	// One failure-safe cleanup in explicit order: release the storage gate,
	// join a started shutdown, join the delete client goroutine, then close
	// and join the stream. Every completion channel is received at most
	// once, and the ordinary deferred owner disposal registered above runs
	// last.
	releaseGate := sync.OnceFunc(blocked.releaseBlock)
	var (
		stream        *sseStream
		streamClosed  chan struct{}
		streamJoined  bool
		deleteDone    chan error
		deleteStarted bool
		deleteJoined  bool
		closeDone     chan error
		closeStarted  bool
		closeJoined   bool
	)
	defer func() {
		releaseGate()
		if closeStarted && !closeJoined {
			closeJoined = true
			<-closeDone
		}
		if deleteStarted && !deleteJoined {
			deleteJoined = true
			<-deleteDone
		}
		if stream != nil {
			stream.close()
		}
		if streamClosed != nil && !streamJoined {
			streamJoined = true
			<-streamClosed
		}
	}()

	// One published discovery record the shutdown must withdraw.
	record := filepath.Join(t.TempDir(), "discovery.json")
	if err := ps.PublishDiscovery(record); err != nil {
		t.Fatalf("PublishDiscovery: %v", err)
	}

	workspace := filepath.Join(e.home, "shutdown")
	created, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: workspace, AgentType: "solo"})
	if err != nil || created.JSON201 == nil {
		t.Fatalf("create: %v", err)
	}
	session := created.JSON201.SessionId
	if response, err := client.ArchiveSessionWithResponse(ctx, session); err != nil || response.JSON200 == nil {
		t.Fatalf("archive: %v", err)
	}

	// One active stream closes when the server's connections close.
	stream = openSSEStream(t, ps)
	streamClosed = make(chan struct{})
	go func() {
		defer close(streamClosed)
		_, _, _ = stream.readSSEFrame()
		for {
			if _, err := stream.reader.ReadString('\n'); err != nil {
				return
			}
		}
	}()

	// One separately blocked command: the archived Session's deletion parks
	// inside its storage transaction, holding its admitted call open. Its
	// HTTP result is only the transport outcome — native Server.Close is
	// required to close the parked connection, so the client may observe
	// EOF long before the backend call converges.
	blocked.block(session)
	deleteDone = make(chan error, 1)
	deleteStarted = true
	go func() {
		_, err := client.DeleteSessionWithResponse(ctx, session)
		deleteDone <- err
	}()
	select {
	case <-blocked.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the parked deletion never entered its transaction")
	}

	closeDone = make(chan error, 1)
	closeStarted = true
	go func() { closeDone <- r.Close(context.Background()) }()

	// The stream observes the server's connection close while the parked
	// deletion still holds its admitted call: the parked transaction is the
	// gate-held oracle that the owner shutdown has not converged.
	select {
	case <-streamClosed:
		streamJoined = true
	case <-time.After(10 * time.Second):
		t.Fatal("the active stream was not closed before the admitted-call wait")
	}
	select {
	case <-closeDone:
		closeJoined = true
		t.Fatal("the owner shutdown completed while the parked command still held its admitted call")
	default:
	}

	// The record is still in place while the owner is joined on the command.
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("the discovery record was withdrawn before the joined command settled: %v", err)
	}

	// A racing attachment after shutdown began cannot attach.
	if second, err := r.OpenProtocol(context.Background()); err == nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("post-shutdown attachment = (%v, %v), want the closed refusal", second, err)
	}

	// Releasing the parked transaction lets the deletion settle, the close
	// converge, the record withdraw, and the lock release — in that order.
	releaseGate()
	select {
	case <-deleteDone:
		// The delete client goroutine joined; its transport result may be
		// the EOF the connection close produced, never the backend
		// settlement.
		deleteJoined = true
	case <-time.After(10 * time.Second):
		t.Fatal("the delete client goroutine never joined")
	}
	select {
	case err := <-closeDone:
		closeJoined = true
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the owner shutdown never converged after the command settled")
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the discovery record survived shutdown: %v", err)
	}
	if _, err := net.Dial("tcp", strings.TrimPrefix(ps.Endpoint(), "http://")); err == nil {
		t.Fatal("the protocol listener still accepts after shutdown")
	}
	e.assertLockReleased(t)
}

// TestProtocolServerDiscoveryPublication pins the one owner-held publisher:
// the record's strict shape and mode, the one-publication rule, the released
// claim after a failed pre-write attempt, the required existing parent, and
// the post-shutdown refusal.
func TestProtocolServerDiscoveryPublication(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)

	dir := t.TempDir()
	record := filepath.Join(dir, "discovery.json")
	if err := ps.PublishDiscovery(record); err != nil {
		t.Fatalf("PublishDiscovery: %v", err)
	}
	info, err := os.Stat(record)
	if err != nil {
		t.Fatalf("published record: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("published record mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read published record: %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var published struct {
		Endpoint        string `json:"endpoint"`
		InstanceId      string `json:"instance_id"`
		ProtocolVersion string `json:"protocol_version"`
		Credential      string `json:"credential"`
	}
	if err := decoder.Decode(&published); err != nil {
		t.Fatalf("decode published record %s: %v", string(data), err)
	}
	if published.Endpoint != ps.Endpoint() || published.InstanceId != ps.instance ||
		published.ProtocolVersion != string(protocol.N1) || published.Credential != ps.credential {
		t.Fatalf("published record = %+v, want the endpoint, identity, wire version, and credential", published)
	}

	// A second publication is refused — successful publication cannot repeat.
	second := filepath.Join(dir, "second.json")
	if err := ps.PublishDiscovery(second); err == nil {
		t.Fatal("repeat publication was accepted")
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatal("the refused repeat published a second record")
	}

	// After the owner's shutdown, publication is refused and leaves no
	// record: the owner never answers health again.
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	late := filepath.Join(dir, "late.json")
	if err := ps.PublishDiscovery(late); err == nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("post-shutdown publication = %v, want the closed refusal", err)
	}
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Fatal("the refused post-shutdown publication wrote a record")
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the discovery record %s survived shutdown", record)
	}
}

// TestProtocolServerDiscoveryClaimRelease pins the released publication
// claim: a pre-write failure — the parent directory must already exist —
// leaves the server free to publish at the next valid path.
func TestProtocolServerDiscoveryClaimRelease(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)

	dir := t.TempDir()
	missing := filepath.Join(dir, "absent-parent", "discovery.json")
	if err := ps.PublishDiscovery(missing); err == nil {
		t.Fatal("publication into a missing parent was accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("the failed attempt published a record")
	}
	if _, err := os.Stat(filepath.Join(dir, "absent-parent")); !os.IsNotExist(err) {
		t.Fatal("the failed attempt resurrected the parent directory")
	}
	retry := filepath.Join(dir, "retry.json")
	if err := ps.PublishDiscovery(retry); err != nil {
		t.Fatalf("publication after the failed attempt: %v", err)
	}
	if _, err := os.Stat(retry); err != nil {
		t.Fatalf("the retried publication's record: %v", err)
	}
}

// TestProtocolServerAttachmentRaces pins the one-attached-server rule: a
// second attachment while one exists fails invalid and the first server keeps
// serving.
func TestProtocolServerAttachmentRaces(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	first := openProtocolServer(t, r)

	second, err := r.OpenProtocol(context.Background())
	if err == nil || !errors.Is(err, harness.ErrInvalid) {
		t.Fatalf("second attachment = (%v, %v), want the invalid refusal", second, err)
	}
	if second != nil {
		t.Fatal("the refused attachment returned a server")
	}

	// The first server is untouched: health still answers.
	resp := rawProtocol(t, http.MethodGet, protocolTarget(first, "/v1/health"), first.credential, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health after the refused attachment = %d, want 200", resp.StatusCode)
	}
}

// TestProtocolServerDiscoveryPublicationRacingClose pins the publication
// claim against shutdown: the owning-file sync barrier parks a discovery
// write inside its atomic publication, Close joins the admitted write, the
// written record is withdrawn before the ownership lock releases, and a
// publication attempted after closure is the refused sibling.
func TestProtocolServerDiscoveryPublicationRacingClose(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)

	dir := t.TempDir()
	record := filepath.Join(dir, "discovery.json")

	probe := installOwningSyncProbe(t, record)
	defer probe.restore()
	releaseWrite := sync.OnceFunc(probe.releaseProbe)
	defer releaseWrite()
	probe.park = true

	previousRelease := atomicfs.ReleaseFunc
	releaseArrived := make(chan struct{}, 1)
	releaseGate := make(chan struct{})
	atomicfs.ReleaseFunc = func(*atomicfs.Lock) error {
		select {
		case releaseArrived <- struct{}{}:
		default:
		}
		<-releaseGate
		return nil
	}
	defer func() { atomicfs.ReleaseFunc = previousRelease }()
	releaseLock := sync.OnceFunc(func() { close(releaseGate) })
	defer releaseLock()

	publishDone := make(chan error, 1)
	go func() { publishDone <- ps.PublishDiscovery(record) }()
	select {
	case <-probe.arrive:
	case <-time.After(10 * time.Second):
		t.Fatal("the discovery write never parked")
	}
	select {
	case err := <-publishDone:
		t.Fatalf("the parked publication returned before its write completed: %v", err)
	default:
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the parked write exposed the record before publication completed: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close(context.Background()) }()
	// The parked admitted publication holds the call wait: Close cannot
	// converge while it is parked.
	select {
	case err := <-closeDone:
		t.Fatalf("Close completed while the admitted publication was still parked: %v", err)
	default:
	}

	probe.park = false
	releaseWrite()
	select {
	case err := <-publishDone:
		if err != nil {
			t.Fatalf("the joined publication failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the released publication never returned")
	}
	if got := probe.count(); got != 1 {
		t.Fatalf("the owning probe matched %d syncs, want exactly the discovery write", got)
	}

	select {
	case <-releaseArrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the owner lock release was never reached")
	}
	// The record is already withdrawn while the lock release is parked.
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the discovery record survived into the lock release: %v", err)
	}
	releaseLock()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never converged after the lock release")
	}

	// The hooked release reported success without dropping the flock; drop
	// it for real and prove the ownership lock is free.
	atomicfs.ReleaseFunc = previousRelease
	if err := r.lock.Release(); err != nil {
		t.Fatalf("real ownership lock release: %v", err)
	}
	e.assertLockReleased(t)

	// The nearest sibling: a publication attempted after closure is refused
	// and leaves no record.
	late := filepath.Join(dir, "late.json")
	if err := ps.PublishDiscovery(late); err == nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("post-shutdown publication = %v, want the closed refusal", err)
	}
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Fatal("the refused post-shutdown publication wrote a record")
	}
}

// TestProtocolServerStreamDisconnectPreservesLiveJob pins the disconnect
// contract against real background work: closing the events stream leaves
// the live job member in hydration and never invokes the job stopper; the
// existing stop path then completes the job through the seam.
func TestProtocolServerStreamDisconnectPreservesLiveJob(t *testing.T) {
	e, r, _, _, session := startLiveJobRuntime(t, "stopped job")
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)
	client := protocolClient(t, ps)
	ctx := context.Background()

	stream := openSSEStream(t, ps)
	stream.close()

	hydration, err := client.GetSessionHydrationWithResponse(ctx, session)
	if err != nil || hydration.JSON200 == nil {
		t.Fatalf("hydration after disconnect: %v", err)
	}
	live := false
	for _, member := range hydration.JSON200.Background {
		if member.Kind == protocol.BackgroundMemberKindJob && member.Id == "0a1b2c3d" {
			live = true
		}
	}
	if !live {
		t.Fatalf("hydration background = %+v, want the live job member after the stream disconnect", hydration.JSON200.Background)
	}
	if stops := countEvents(e.events.all(), "stopjob"); stops != 0 {
		t.Fatalf("the stream disconnect invoked the job stopper %d times", stops)
	}

	// The existing stop path completes the job through the fixture seam.
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		return h.Stop(ctx, session)
	}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stops := countEvents(e.events.all(), "stopjob"); stops != 1 {
		t.Fatalf("stop invoked the job stopper %d times, want exactly one", stops)
	}
	settled, err := client.GetSessionHydrationWithResponse(ctx, session)
	if err != nil || settled.JSON200 == nil {
		t.Fatalf("hydration after stop: %v", err)
	}
	if len(settled.JSON200.Background) != 0 {
		t.Fatalf("hydration background = %+v, want the stopped job removed", settled.JSON200.Background)
	}
}

// firstNonLoopbackAddress returns the machine's first usable non-loopback
// interface address, or skips when the host has none to probe.
func firstNonLoopbackAddress(t *testing.T) string {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list network interfaces: %v", err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil || ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			return ip.String()
		}
	}
	t.Skip("no non-loopback local address exists to probe")
	return ""
}

// TestProtocolServerListenerIsLoopbackOnly proves the fixed 127.0.0.1
// endpoint answers only on the loopback interface: dialing the bound port
// through a non-loopback local address fails.
func TestProtocolServerListenerIsLoopbackOnly(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(ps.Endpoint(), "http://"))
	if err != nil {
		t.Fatalf("endpoint %q is not host:port: %v", ps.Endpoint(), err)
	}
	address := firstNonLoopbackAddress(t)
	target := net.JoinHostPort(address, port)
	conn, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err == nil {
		conn.Close()
		t.Fatalf("the listener answered through the non-loopback address %s", target)
	}
}

// gatedRandReader parks its first read and delegates every later read to the
// original crypto/rand reader, so one identity mint can be suspended after
// its listener is already bound.
type gatedRandReader struct {
	original io.Reader
	arrived  chan struct{}
	gate     chan struct{}
	once     sync.Once
}

func (g *gatedRandReader) Read(p []byte) (int, error) {
	g.once.Do(func() {
		select {
		case g.arrived <- struct{}{}:
		default:
		}
		<-g.gate
	})
	return g.original.Read(p)
}

// processListenPorts returns the listening TCP ports this process has bound
// to 127.0.0.1, discovered through its own file-descriptor table with
// SO_ACCEPTCONN and Getsockname, so a concurrent package test process cannot
// contribute entries.
func processListenPorts(t *testing.T) map[int]bool {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no process file-descriptor table to read: %v", err)
	}
	ports := map[int]bool{}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:") {
			continue
		}
		listening, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
		if err != nil || listening != 1 {
			continue
		}
		address, err := syscall.Getsockname(fd)
		if err != nil {
			continue
		}
		if ipv4, ok := address.(*syscall.SockaddrInet4); ok && ipv4.Addr == [4]byte{127, 0, 0, 1} {
			ports[ipv4.Port] = true
		}
	}
	return ports
}

// TestProtocolServerPostBindCloseRace parks one identity mint after its
// listener is already bound, starts Close, and proves the rejected
// attachment returns the closed refusal with no attachment while the
// process-table-captured listener port stops answering.
func TestProtocolServerPostBindCloseRace(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)

	before := processListenPorts(t)

	originalReader := cryptorand.Reader
	arrived := make(chan struct{}, 1)
	gate := make(chan struct{})
	cryptorand.Reader = &gatedRandReader{original: originalReader, arrived: arrived, gate: gate}
	releaseRand := sync.OnceFunc(func() { close(gate) })

	type openResult struct {
		ps  *ProtocolServer
		err error
	}
	openDone := make(chan openResult, 1)
	closeDone := make(chan error, 1)
	var openConsumed, closeStarted, closeConsumed bool
	// Cleanup releases the parked mint first, joins the OpenProtocol result
	// before restoring the global reader, closes any unexpectedly returned
	// server, and joins a started shutdown exactly once.
	cleanup := func() {
		releaseRand()
		if !openConsumed {
			openConsumed = true
			if result := <-openDone; result.ps != nil {
				_ = result.ps.closeNetwork()
			}
		}
		if closeStarted && !closeConsumed {
			closeConsumed = true
			<-closeDone
		}
		cryptorand.Reader = originalReader
	}
	defer cleanup()

	go func() {
		ps, err := r.OpenProtocol(context.Background())
		openDone <- openResult{ps: ps, err: err}
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("OpenProtocol never parked inside identity minting")
	}

	// The listener is already bound: the process's own descriptor table
	// shows the new listening loopback socket.
	var boundPort int
	for port := range processListenPorts(t) {
		if before[port] {
			continue
		}
		if boundPort != 0 {
			t.Fatalf("more than one new listening socket appeared: %d and %d", boundPort, port)
		}
		boundPort = port
	}
	if boundPort == 0 {
		t.Fatal("no newly bound listening socket appeared while OpenProtocol was parked")
	}
	boundAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(boundPort))

	closeStarted = true
	go func() { closeDone <- r.Close(context.Background()) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		closed := r.closed
		r.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close never marked admission closed")
		}
		time.Sleep(time.Millisecond)
	}

	releaseRand()
	select {
	case result := <-openDone:
		openConsumed = true
		if result.ps != nil || !errors.Is(result.err, ErrClosed) {
			if result.ps != nil {
				_ = result.ps.closeNetwork()
			}
			t.Fatalf("post-bind attachment = (%v, %v), want the closed refusal with no server", result.ps, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OpenProtocol never returned after the closure race")
	}
	r.mu.Lock()
	attached := r.protocol != nil
	r.mu.Unlock()
	if attached {
		t.Fatal("the rejected OpenProtocol left an attachment")
	}

	select {
	case err := <-closeDone:
		closeConsumed = true
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never converged after the rejected attachment")
	}
	if conn, err := net.DialTimeout("tcp", boundAddress, time.Second); err == nil {
		conn.Close()
		t.Fatalf("the rejected listener still accepts connections at %s", boundAddress)
	}
}

// TestProtocolServerPublicationReadinessVsClose composes a health-request
// gate over the standard server's handler: readiness parks before any
// verification completes, Close cancels the owner work context, and the
// admitted publisher errors without remembering a path or writing a record.
func TestProtocolServerPublicationReadinessVsClose(t *testing.T) {
	e := newOwnerEnv(t)
	r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeProjectionRuntime(r)
	ps := openProtocolServer(t, r)

	original := ps.server.Handler
	healthArrived := make(chan struct{}, 1)
	healthGate := make(chan struct{})
	var once sync.Once
	ps.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/health" {
			once.Do(func() {
				select {
				case healthArrived <- struct{}{}:
				default:
				}
				<-healthGate
			})
		}
		original.ServeHTTP(w, req)
	})
	releaseHealth := sync.OnceFunc(func() { close(healthGate) })
	defer releaseHealth()

	dir := t.TempDir()
	record := filepath.Join(dir, "discovery.json")
	publishDone := make(chan error, 1)
	go func() { publishDone <- ps.PublishDiscovery(record) }()
	select {
	case <-healthArrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the readiness request never parked")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close(context.Background()) }()
	select {
	case err := <-publishDone:
		if err == nil {
			t.Fatal("the readiness-gated publication succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the readiness-gated publication never returned")
	}
	ps.pubMu.Lock()
	remembered := ps.discoveryPath
	ps.pubMu.Unlock()
	if remembered != "" {
		t.Fatalf("the refused publication remembered path %q", remembered)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the refused publication wrote a record: %v", err)
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never converged while readiness was gated")
	}
}

// TestProtocolServerDiscoveryConcurrentPublishers pins the one-publication
// claim under concurrency: a parked first write admits exactly one
// remembered path, and an injected first-write failure releases the claim so
// the waiting second publisher succeeds.
func TestProtocolServerDiscoveryConcurrentPublishers(t *testing.T) {
	assertOnePublication := func(t *testing.T, remembered string, firstErr, secondErr error, first, second string) {
		t.Helper()
		if (firstErr == nil) == (secondErr == nil) {
			t.Fatalf("concurrent publications = (%v, %v), want exactly one success", firstErr, secondErr)
		}
		winner, loser := first, second
		if remembered == second {
			winner, loser = second, first
		} else if remembered != first {
			t.Fatalf("remembered path %q is neither published path", remembered)
		}
		if _, err := os.Stat(winner); err != nil {
			t.Fatalf("the successful publication's record: %v", err)
		}
		if _, err := os.Stat(loser); !os.IsNotExist(err) {
			t.Fatalf("the refused publication left a record at %s: %v", loser, err)
		}
	}

	t.Run("parked first write leaves one remembered path", func(t *testing.T) {
		e := newOwnerEnv(t)
		r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		dir := t.TempDir()
		first := filepath.Join(dir, "first.json")
		second := filepath.Join(dir, "second.json")

		// The owning probe parks only the first path's write; the second
		// path delegates normally.
		probe := installOwningSyncProbe(t, first)
		defer probe.restore()
		releaseWrite := sync.OnceFunc(probe.releaseProbe)
		defer releaseWrite()
		probe.park = true

		firstResult := make(chan error, 1)
		secondResult := make(chan error, 1)
		go func() { firstResult <- ps.PublishDiscovery(first) }()
		select {
		case <-probe.arrive:
		case <-time.After(10 * time.Second):
			t.Fatal("the first publication never parked")
		}
		go func() { secondResult <- ps.PublishDiscovery(second) }()
		probe.park = false
		releaseWrite()
		firstErr := <-firstResult
		secondErr := <-secondResult
		if got := probe.count(); got != 1 {
			t.Fatalf("the owning probe matched %d syncs, want only the first path's write", got)
		}
		ps.pubMu.Lock()
		remembered := ps.discoveryPath
		ps.pubMu.Unlock()
		assertOnePublication(t, remembered, firstErr, secondErr, first, second)
	})

	t.Run("injected first-write failure lets the waiter succeed", func(t *testing.T) {
		e := newOwnerEnv(t)
		r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		dir := t.TempDir()
		first := filepath.Join(dir, "first.json")
		second := filepath.Join(dir, "second.json")

		injected := errors.New("injected first-write failure")
		probe := installOwningSyncProbe(t, first)
		defer probe.restore()
		probe.fail = injected

		firstResult := make(chan error, 1)
		secondResult := make(chan error, 1)
		go func() { firstResult <- ps.PublishDiscovery(first) }()
		select {
		case <-probe.arrive:
		case <-time.After(10 * time.Second):
			t.Fatal("the first publication never reached its write")
		}
		go func() { secondResult <- ps.PublishDiscovery(second) }()
		firstErr := <-firstResult
		secondErr := <-secondResult
		if !errors.Is(firstErr, injected) {
			t.Fatalf("the injected first write = %v, want the injected failure", firstErr)
		}
		if secondErr != nil {
			t.Fatalf("the waiting second publication = %v, want success after the released claim", secondErr)
		}
		if got := probe.count(); got != 1 {
			t.Fatalf("the owning probe matched %d syncs, want only the first path's write", got)
		}
		ps.pubMu.Lock()
		remembered := ps.discoveryPath
		ps.pubMu.Unlock()
		if remembered != second {
			t.Fatalf("remembered path = %q, want the second publisher's %q", remembered, second)
		}
		if _, err := os.Stat(second); err != nil {
			t.Fatalf("the second publication's record: %v", err)
		}
		if _, err := os.Stat(first); !os.IsNotExist(err) {
			t.Fatalf("the failed publication left a record at %s: %v", first, err)
		}
	})
}

// TestProtocolServerDiscoveryWithdrawalStates pins withdrawal's external
// states: a record already removed closes cleanly, and a record replaced by
// a nonempty directory reports the removal failure while the lock still
// releases and the directory contents survive.
func TestProtocolServerDiscoveryWithdrawalStates(t *testing.T) {
	t.Run("already removed", func(t *testing.T) {
		e := newOwnerEnv(t)
		r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		record := filepath.Join(t.TempDir(), "discovery.json")
		if err := ps.PublishDiscovery(record); err != nil {
			t.Fatalf("PublishDiscovery: %v", err)
		}
		if err := os.Remove(record); err != nil {
			t.Fatalf("remove the record before shutdown: %v", err)
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close with an already-removed record: %v", err)
		}
		if _, err := os.Stat(record); !os.IsNotExist(err) {
			t.Fatalf("the removed record reappeared: %v", err)
		}
		e.assertLockReleased(t)
	})

	t.Run("replaced by a nonempty directory", func(t *testing.T) {
		e := newOwnerEnv(t)
		r, err := e.open(context.Background(), e.storagePlugin(storage.NewMemory()))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		record := filepath.Join(t.TempDir(), "discovery.json")
		if err := ps.PublishDiscovery(record); err != nil {
			t.Fatalf("PublishDiscovery: %v", err)
		}
		if err := os.Remove(record); err != nil {
			t.Fatalf("remove the record: %v", err)
		}
		if err := os.Mkdir(record, 0o700); err != nil {
			t.Fatalf("replace the record with a directory: %v", err)
		}
		inner := filepath.Join(record, "inner.txt")
		if err := os.WriteFile(inner, []byte("kept"), 0o600); err != nil {
			t.Fatalf("write the directory content: %v", err)
		}
		err = r.Close(context.Background())
		if err == nil || !strings.Contains(err.Error(), "withdraw discovery record") {
			t.Fatalf("Close with a replaced record = %v, want the removal failure", err)
		}
		if data, err := os.ReadFile(inner); err != nil || string(data) != "kept" {
			t.Fatalf("the directory content = (%q, %v), want it preserved", data, err)
		}
		e.assertLockReleased(t)
	})
}
