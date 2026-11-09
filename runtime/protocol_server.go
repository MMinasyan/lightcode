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
	"github.com/MMinasyan/lightcode/melta"
	"github.com/MMinasyan/lightcode/protocol"
)

// sseConnectionKey is the private context key carrying the native
// connection of one request, installed by the server's ConnContext so an
// events stream can unblock its writer by closing exactly its connection.
type sseConnectionKey struct{}

// ProtocolHost owns one host's listener attachment and supplies the
// BeforeRelease callback to the MELTA owner that must run after admitted work
// and scopes settle and before storage or the ownership lock is released.
type ProtocolHost struct {
	mu       sync.Mutex
	server   *ProtocolServer
	released bool
}

// NewProtocolHost constructs one empty host. BeforeRelease is supplied to
// melta.Options and OpenProtocol attaches the one listener afterwards.
func NewProtocolHost() *ProtocolHost { return &ProtocolHost{} }

// BeforeRelease closes the attached listener and every active connection and
// withdraws the remembered discovery record once. It runs at the owner's
// release boundary; a second call is a no-op.
func (h *ProtocolHost) BeforeRelease() error {
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return nil
	}
	h.released = true
	server := h.server
	h.mu.Unlock()
	if server == nil {
		return nil
	}
	return server.release()
}

// OpenProtocol binds one isolated loopback listener and mounts the generated
// protocol server over the public owner. It binds outside the host mutex and
// rechecks the attachment and release state under that mutex: a closing host
// or an existing attachment closes the rejected listener and fails without
// touching network state. Once attached, the serving goroutine always starts
// and the host release joins it.
func (h *ProtocolHost) OpenProtocol(ctx context.Context, owner *melta.Melta) (*ProtocolServer, error) {
	if ctx == nil {
		return nil, errors.New("runtime: protocol construction requires a non-nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, errors.New("runtime: protocol construction requires a non-nil owner")
	}
	caller, err := owner.Caller(melta.CallerContext{Origin: harness.InputOriginUser, Scope: melta.CallerScope{Kind: melta.CallerScopeHarness}})
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	credential, err := mintCredential()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	ps := &ProtocolServer{
		host:       h,
		owner:      owner,
		caller:     caller,
		listener:   listener,
		instance:   owner.InstanceID(),
		credential: credential,
		endpoint:   "http://" + listener.Addr().String(),
		serveDone:  make(chan struct{}),
	}
	mux := http.NewServeMux()
	router := authenticatedMux{mux: mux, authorize: ps.authorize}
	handler := protocol.HandlerWithOptions(&protocolHandlers{owner: owner, server: ps}, protocol.StdHTTPServerOptions{
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
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		_ = listener.Close()
		return nil, melta.ErrClosed
	}
	if h.server != nil {
		h.mu.Unlock()
		_ = listener.Close()
		return nil, fmt.Errorf("a protocol server is already attached: %w", harness.ErrInvalid)
	}
	h.server = ps
	h.mu.Unlock()
	go func() {
		defer close(ps.serveDone)
		_ = ps.server.Serve(ps.listener)
	}()
	return ps, nil
}

// ProtocolServer is the one attached isolated protocol listener of a host:
// it owns the loopback listener, the native HTTP server, the owner-minted
// instance identity and bearer credential, and the remembered discovery path
// once one is published. It exposes no credential-returning API; the host's
// release closes this server, joins its serving goroutine, and withdraws the
// remembered discovery record.
type ProtocolServer struct {
	host       *ProtocolHost
	owner      *melta.Melta
	caller     melta.Caller
	listener   net.Listener
	server     *http.Server
	instance   string // 32 lowercase hex: the owner's process-lifetime identity
	credential string // 64 lowercase hex: the bearer credential
	endpoint   string // "http://127.0.0.1:<port>", the host-facing address
	serveDone  chan struct{}

	// pubMu coordinates the one publication attempt: a remembered path
	// blocks every repeat, a failed pre-write attempt releases the claim,
	// and a successful publication never repeats.
	pubMu         sync.Mutex
	discoveryPath string
}

// Endpoint returns the host-facing address of the attached listener.
func (ps *ProtocolServer) Endpoint() string { return ps.endpoint }

// mintCredential mints the server's bearer credential: 32 random bytes as 64
// lowercase hex.
func mintCredential() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint protocol credential: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
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
			writeProtocolAuthError(w)
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

// discoveryRecord is the server-held readiness record published at the
// caller's isolated path: the loopback endpoint, the owner's minted instance
// identity, the wire protocol version, and the bearer credential. The
// credential never leaves through any HTTP response.
type discoveryRecord struct {
	Endpoint        string `json:"endpoint"`
	InstanceId      string `json:"instance_id"`
	ProtocolVersion string `json:"protocol_version"`
	Credential      string `json:"credential"`
}

// PublishDiscovery is the one ready-server publisher: readiness is verified
// before the final publication lock; the final release check and the record
// write then run under the host's one attachment/closing mutex, so release
// starting after readiness but before the write leaves no record. One local
// coordination admits a single publication: a remembered path blocks every
// repeat, while a failed pre-write attempt releases the claim so the caller
// may retry. The destination parent must already exist — atomic publication
// is a replace, never a directory resurrection.
func (ps *ProtocolServer) PublishDiscovery(path string) error {
	return ps.publishDiscovery(path, ps.verifyReady)
}

func (ps *ProtocolServer) publishDiscovery(path string, verify func() error) error {
	ps.pubMu.Lock()
	defer ps.pubMu.Unlock()
	if ps.discoveryPath != "" {
		return fmt.Errorf("discovery already published at %s: %w", ps.discoveryPath, harness.ErrInvalid)
	}
	if err := verify(); err != nil {
		return err
	}
	record, err := json.Marshal(discoveryRecord{
		Endpoint:        ps.endpoint,
		InstanceId:      ps.instance,
		ProtocolVersion: string(protocol.N13),
		Credential:      ps.credential,
	})
	if err != nil {
		return err
	}
	ps.host.mu.Lock()
	defer ps.host.mu.Unlock()
	if ps.host.released {
		return melta.ErrClosed
	}
	if ps.discoveryPath != "" {
		return fmt.Errorf("discovery already published at %s: %w", ps.discoveryPath, harness.ErrInvalid)
	}
	if err := atomicfs.Write(path, record, 0o600); err != nil {
		return err
	}
	ps.discoveryPath = path
	return nil
}

// verifyReady proves the attached server answers one authenticated health
// request with its own identity and the wire version, over the live loopback
// listener.
func (ps *ProtocolServer) verifyReady() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ps.endpoint+"/v1/health", nil)
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
	if health.InstanceId != ps.instance || health.ProtocolVersion != protocol.N13 {
		return fmt.Errorf("health response identity %q/%q does not match this server", health.InstanceId, health.ProtocolVersion)
	}
	return nil
}

// release closes the server's listener and every active connection, joins the
// serving goroutine, and withdraws the remembered discovery record. It is the
// one network action of host withdrawal, run after the owner's admitted work
// and scopes settle and before storage or the ownership lock release.
func (ps *ProtocolServer) release() error {
	var errs []error
	if err := ps.server.Close(); err != nil {
		errs = append(errs, err)
	}
	<-ps.serveDone
	ps.pubMu.Lock()
	path := ps.discoveryPath
	ps.discoveryPath = ""
	ps.pubMu.Unlock()
	if path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("withdraw discovery record %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}
