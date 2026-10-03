package runtime

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/protocol"
)

// sseConnectionKey is the private context key carrying the native
// connection of one request, installed by the server's ConnContext so an
// events stream can unblock its writer by closing exactly its connection.
type sseConnectionKey struct{}

// ProtocolServer is the one attached isolated protocol listener of a Runtime:
// it owns the loopback listener, the native HTTP server, the minted instance
// identity and bearer credential, and the remembered discovery path once one
// is published. It exposes no credential-returning API and grants no
// lifetime authority: the owning Runtime's Close is the only shutdown
// command, and it closes this server, joins its serving goroutine, and
// withdraws the remembered discovery record.
type ProtocolServer struct {
	owner      *Runtime
	listener   net.Listener
	server     *http.Server
	instance   string // 32 lowercase hex: the Runtime's process-lifetime identity
	credential string // 64 lowercase hex: the bearer credential
	endpoint   string // "http://127.0.0.1:<port>", the host-facing address
	serveDone  chan struct{}

	// pubMu coordinates the one publication attempt: a remembered path
	// blocks every repeat, a failed pre-write attempt releases the claim,
	// and a successful publication never repeats.
	pubMu         sync.Mutex
	discoveryPath string
}

// OpenProtocol binds one isolated loopback listener and mounts the generated
// protocol server against the Runtime's private producers. It owns one
// admitted call, binds outside the Runtime mutex, and rechecks closure and
// attachment under that mutex: a shutdown race or an existing attachment
// closes the rejected listener and fails without touching network state.
// Once attached, the serving goroutine always starts and shutdown joins it.
// A second attachment while one exists fails invalid.
func (r *Runtime) OpenProtocol(ctx context.Context) (*ProtocolServer, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	instance, credential, err := mintProtocolIdentity()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	ps := &ProtocolServer{
		owner:      r,
		listener:   listener,
		instance:   instance,
		credential: credential,
		endpoint:   "http://" + listener.Addr().String(),
		serveDone:  make(chan struct{}),
	}
	mux := http.NewServeMux()
	router := authenticatedMux{mux: mux, authorize: ps.authorize}
	handler := protocol.HandlerWithOptions(&protocolHandlers{Runtime: r}, protocol.StdHTTPServerOptions{
		BaseRouter: router,
		// A malformed or missing request parameter is the one binding-failure
		// class, answered by the same typed error body as every handler.
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProtocolError(w, fmt.Errorf("request parameter binding: %w: %w", err, harness.ErrInvalid))
		},
	})
	// The method-less root catch-all rides the same authentication gate as
	// every operation: any request matching no operation pattern or method
	// receives the schema's typed not_found error.
	router.HandleFunc("/", ps.notFound)
	ps.server = &http.Server{
		Handler: handler,
		// Bounded header reads keep the server from holding a connection on
		// an absent peer; bodies and streams stay unbounded by design.
		ReadHeaderTimeout: 30 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, sseConnectionKey{}, c)
		},
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = listener.Close()
		return nil, ErrClosed
	}
	if r.protocol != nil {
		r.mu.Unlock()
		_ = listener.Close()
		return nil, fmt.Errorf("a protocol server is already attached: %w", harness.ErrInvalid)
	}
	r.protocol = ps
	r.mu.Unlock()
	go func() {
		defer close(ps.serveDone)
		_ = ps.server.Serve(ps.listener)
	}()
	return ps, nil
}

// Endpoint returns the host-facing address of the attached listener.
func (ps *ProtocolServer) Endpoint() string { return ps.endpoint }

// mintProtocolIdentity mints the Runtime's process-lifetime identity — 16
// random bytes as 32 lowercase hex — and its bearer credential — 32 random
// bytes as 64 lowercase hex.
func mintProtocolIdentity() (instance, credential string, err error) {
	var instanceRaw [16]byte
	if _, err := rand.Read(instanceRaw[:]); err != nil {
		return "", "", fmt.Errorf("mint protocol instance identity: %w", err)
	}
	var credentialRaw [32]byte
	if _, err := rand.Read(credentialRaw[:]); err != nil {
		return "", "", fmt.Errorf("mint protocol credential: %w", err)
	}
	return hex.EncodeToString(instanceRaw[:]), hex.EncodeToString(credentialRaw[:]), nil
}

// protocolInstance returns the attached server's minted instance identity, or
// empty when no server is attached. Handlers only run while their server is
// attached, so the value is stable for their whole response.
func (r *Runtime) protocolInstance() string {
	if ps := r.protocol; ps != nil {
		return ps.instance
	}
	return ""
}

// authenticatedMux adapts the generated route registration onto the real
// native mux: every operation handler and the root catch-all are wrapped by
// the authentication gate before the generated wrapper performs parameter
// binding, and the native mux's own redirects and transport answers stay in
// front of both the gate and every handler.
type authenticatedMux struct {
	mux       *http.ServeMux
	authorize func(http.HandlerFunc) http.HandlerFunc
}

// HandleFunc registers the authentication gate around one generated handler
// on the real mux. The raw handler function type keeps the adapter the
// generated registration's exact ServeMux shape.
func (m authenticatedMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.mux.HandleFunc(pattern, m.authorize(handler))
}

// ServeHTTP delegates to the real mux, whose own match, redirect, and
// transport behavior is unchanged.
func (m authenticatedMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mux.ServeHTTP(w, r)
}

// authorize returns the gate around one mounted handler: the bearer
// credential is compared in constant time, a missing or invalid credential
// answers the typed unauthorized error, and the credential or any presented
// header value never rides the response.
func (ps *ProtocolServer) authorize(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) ||
			subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(ps.credential)) != 1 {
			writeProtocolError(w, errUnauthorized)
			return
		}
		handler(w, r)
	}
}

// notFound is the typed root catch-all: any request matching no operation
// pattern or method — an unknown route or a wrong method alike — receives
// the schema's not_found error, never the native mux's plain-text response.
func (ps *ProtocolServer) notFound(w http.ResponseWriter, _ *http.Request) {
	writeProtocolError(w, fmt.Errorf("no protocol operation matches the request method and path: %w", harness.ErrNotFound))
}

// discoveryRecord is the owner-held readiness record published at the
// caller's isolated path: the loopback endpoint, the Runtime's minted
// instance identity, the wire protocol version, and the bearer credential.
// The credential never leaves through any HTTP response.
type discoveryRecord struct {
	Endpoint        string `json:"endpoint"`
	InstanceId      string `json:"instance_id"`
	ProtocolVersion string `json:"protocol_version"`
	Credential      string `json:"credential"`
}

// PublishDiscovery is the one owner-held, ready-server publisher: it holds
// the owning Runtime's admitted call, verifies authenticated readiness over
// the live loopback listener, atomically writes its own mode-0600 record at
// the caller's path once, and retains that path for shutdown withdrawal. One
// local coordination admits a single publication: a remembered path blocks
// every repeat, while a failed pre-write attempt releases the claim so the
// caller may retry. The destination parent must already exist — atomic
// publication is a replace, never a directory resurrection. Once the write
// begins, the path is remembered even when a Close races, so shutdown
// always joins the admitted write and withdraws its record.
func (ps *ProtocolServer) PublishDiscovery(path string) error {
	release, err := ps.owner.enter(context.Background())
	if err != nil {
		return err
	}
	defer release()
	ps.pubMu.Lock()
	defer ps.pubMu.Unlock()
	if ps.discoveryPath != "" {
		return fmt.Errorf("discovery already published at %s: %w", ps.discoveryPath, harness.ErrInvalid)
	}
	if err := ps.verifyReady(); err != nil {
		return err
	}
	record, err := json.Marshal(discoveryRecord{
		Endpoint:        ps.endpoint,
		InstanceId:      ps.instance,
		ProtocolVersion: string(protocol.N1),
		Credential:      ps.credential,
	})
	if err != nil {
		return err
	}
	if err := atomicfs.Write(path, record, 0o600); err != nil {
		return err
	}
	ps.discoveryPath = path
	return nil
}

// verifyReady proves the attached server answers one authenticated health
// request with its own identity and the wire version, over the live loopback
// listener. The request rides the owning Runtime's work context, so a
// closing owner cancels a pre-write verification and prevents publication
// without an extra limit.
func (ps *ProtocolServer) verifyReady() error {
	req, err := http.NewRequestWithContext(ps.owner.work, http.MethodGet, ps.endpoint+"/v1/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+ps.credential)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("verify protocol readiness: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read health response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("authenticated health returned %s", http.StatusText(resp.StatusCode))
	}
	var health protocol.Health
	if err := decodeOneDocument(body, &health); err != nil {
		return fmt.Errorf("decode health response: %w", err)
	}
	if health.InstanceId != ps.instance || health.ProtocolVersion != protocol.N1 {
		return fmt.Errorf("health response identity %q/%q does not match this server", health.InstanceId, health.ProtocolVersion)
	}
	return nil
}

// closeNetwork closes the server's listener and every active connection and
// joins the serving goroutine. It is the one network action of Runtime
// shutdown, run outside the Runtime mutex before admitted calls converge.
func (ps *ProtocolServer) closeNetwork() error {
	err := ps.server.Close()
	<-ps.serveDone
	return err
}

// withdrawDiscovery removes the remembered discovery record. A record the
// filesystem already removed is withdrawn; every other removal failure
// reports to the shutdown error.
func (ps *ProtocolServer) withdrawDiscovery() error {
	ps.pubMu.Lock()
	path := ps.discoveryPath
	ps.pubMu.Unlock()
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("withdraw discovery record %s: %w", path, err)
	}
	return nil
}
