package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/melta"
	"github.com/Syntheresis/melta/model"
	"github.com/Syntheresis/melta/protocol"
)

// dialSmallBuffer opens one raw connection with a minimal receive window so
// an unread events stream blocks its server-side writer after a few
// kilobytes.
func dialSmallBuffer(t *testing.T, server *ProtocolServer) net.Conn {
	t.Helper()
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, _ string, raw syscall.RawConn) error {
		return raw.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		})
	}}
	conn, err := dialer.Dial("tcp", strings.TrimPrefix(server.Endpoint(), "http://"))
	testassert.NoError(t, err)
	return conn
}

// openSSEOverConn hand-writes one authenticated events request over a raw
// connection and reads its response headers; the connection then stays open
// for the caller.
func openSSEOverConn(t *testing.T, conn net.Conn, server *ProtocolServer) {
	t.Helper()
	request := "GET /v1/events HTTP/1.1\r\nHost: events\r\nAuthorization: Bearer " + server.credential + "\r\n\r\n"
	{
		_, err := conn.Write([]byte(request))
		testassert.NoError(t, err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	testassert.Require(t, err == nil && strings.Contains(status, "200"), "events handshake = (%q, %v), want the 200 status line", status, err)
	for {
		line, err := reader.ReadString('\n')
		testassert.NoError(t, err)
		if strings.TrimSpace(line) == "" {
			break
		}
	}
}

// largeTextServer streams one assistant turn of large text deltas so the
// mounted event writer must block on an unread client socket.
type largeTextServer struct {
	*httptest.Server
}

func newLargeTextServer(t *testing.T, deltas, size int) *largeTextServer {
	t.Helper()
	s := &largeTextServer{}
	chunk := strings.Repeat("x", size)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < deltas; i++ {
			payload, err := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"delta":         map[string]any{"role": "assistant", "content": chunk},
					"finish_reason": nil,
				}},
			})
			if err != nil {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// TestProtocolServerStreamIsolation pins the one bounded stream contract: a
// never-reading client's own writer blocks on its socket, its overflowing
// subscription is closed alone, its private watcher closes exactly its
// connection even while the socket write blocks, a healthy sibling stream
// keeps receiving, and the host keeps serving ordinary work.
func TestProtocolServerStreamIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	large := newLargeTextServer(t, 800, 32*1024)
	options := testOwnerOptions(t)
	options.Configuration = melta.NewValueConfigurationSource([]byte(fmt.Sprintf(
		`{"providers":{"prov":{"transport":{"base_url":%q,"api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":262144}}}}}`, large.URL+"/v1")))
	options.Agent = &harness.AgentInputs{Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "full"}
	owner, _, server := mountedOwner(t, ctx, options)

	// The healthy sibling stream, drained continuously through the flood.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.Endpoint()+"/v1/events", nil)
	testassert.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+server.credential)
	healthy, err := http.DefaultClient.Do(request)
	testassert.Require(t, err == nil && healthy.StatusCode == http.StatusOK, "healthy events = (%+v, %v)", healthy, err)
	defer healthy.Body.Close()
	healthyReader := bufio.NewReader(healthy.Body)
	healthyCount := atomic.Int64{}
	go func() {
		for {
			line, err := healthyReader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				healthyCount.Add(1)
			}
		}
	}()

	// The flood victim: a raw connection with a minimal receive window that
	// never reads a frame. The connection stays referenced for the whole row
	// so its finalizer cannot close it early.
	victim := dialSmallBuffer(t, server)
	defer victim.Close()
	openSSEOverConn(t, victim, server)

	created, err := owner.CreateSession(ctx, testCaller(t, owner), t.TempDir(), nil)
	testassert.NoError(t, err)
	var part protocol.ContentPart
	testassert.NoError(t, part.FromTextPart(protocol.TextPart{Kind: protocol.TextPartKindText, Text: "go"}))
	submitted, err := owner.SubmitSession(ctx, testCaller(t, owner), created.SessionId, protocol.SubmitRequest{OperationId: "op-flood", Mode: protocol.SubmitRequestModeRegular, Content: []protocol.ContentPart{part}})
	testassert.NoError(t, err)
	testassert.Require(t, submitted.Disposition == protocol.SubmitResultDispositionAdmitted, "submit = %+v, want admitted", submitted)

	// The healthy sibling drains the complete flood while the victim is never
	// read, so the victim's socket write blocks and its bounded queue
	// saturates.
	deadline := time.Now().Add(30 * time.Second)
	for healthyCount.Load() < 800 {
		if time.Now().After(deadline) {
			t.Fatalf("the healthy sibling stalled at %d events, want the flood", healthyCount.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The removed subscription's watcher closed exactly this connection
	// while its socket write was blocked: draining the buffered window now
	// ends in EOF. A live connection would answer the read deadline.
	testassert.NoError(t, victim.SetReadDeadline(time.Now().Add(10*time.Second)))
	{
		_, err := io.Copy(io.Discard, victim)
		testassert.NoError(t, err)
	}

	// The healthy sibling keeps receiving live events after the victim is
	// gone, and ordinary work keeps answering.
	before := healthyCount.Load()
	{
		_, err := owner.CreateSession(ctx, testCaller(t, owner), t.TempDir(), nil)
		testassert.NoError(t, err)
	}
	for healthyCount.Load() <= before {
		if time.Now().After(deadline) {
			t.Fatalf("the healthy sibling saw no event after ordinary work (stalled at %d)", healthyCount.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestProtocolServerHeartbeat pins the 15-second keepalive comment on one
// idle stream.
func TestProtocolServerHeartbeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, server := mountedOwner(t, ctx, testOwnerOptions(t))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.Endpoint()+"/v1/events", nil)
	testassert.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+server.credential)
	response, err := http.DefaultClient.Do(request)
	testassert.Require(t, err == nil && response.StatusCode == http.StatusOK, "GET events = (%+v, %v)", response, err)
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	deadline := time.Now().Add(20 * time.Second)
	for {
		testassert.Require(t, !time.Now().After(deadline), "no keepalive comment arrived within the heartbeat budget")
		line, err := reader.ReadString('\n')
		testassert.NoError(t, err)
		if strings.HasPrefix(line, ": keepalive") {
			return
		}
	}
}

// TestProtocolServerListenerIsLoopbackOnly proves the fixed 127.0.0.1
// endpoint answers only on the loopback interface: the bound host is a
// loopback address and a non-loopback local address never accepts the port.
func TestProtocolServerListenerIsLoopbackOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, server := mountedOwner(t, ctx, testOwnerOptions(t))
	endpoint := strings.TrimPrefix(server.Endpoint(), "http://")
	host, port, err := net.SplitHostPort(endpoint)
	testassert.NoError(t, err)
	testassert.Require(t, net.ParseIP(host).IsLoopback(), "endpoint host %q is not loopback", host)
	for _, iface := range mustInterfaces(t) {
		addrs, err := iface.Addrs()
		testassert.NoError(t, err)
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), port), time.Second)
			if err == nil {
				_ = conn.Close()
			}
			testassert.Require(t, err != nil, "the protocol port accepted on non-loopback address %s", ip)
		}
	}
}

func mustInterfaces(t *testing.T) []net.Interface {
	t.Helper()
	interfaces, err := net.Interfaces()
	testassert.NoError(t, err)
	return interfaces
}
