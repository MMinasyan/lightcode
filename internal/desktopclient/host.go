// Package desktopclient is the native desktop host for the target client: one
// credential-free Wails byte bridge carrying generated HTTP operations, one
// event pull over the generated Go subscription, and host-local dialogs,
// titles and JSON editing. It owns no Session selection, projection state, or
// work lifetime; the frontend and the Runtime own those.
package desktopclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/internal/version"
	"github.com/MMinasyan/lightcode/protocol"
)

// contentTypeJSON is the one content type this host's local invocation
// results carry.
const contentTypeJSON = "application/json"

// ConnectionInfo describes one opened native connection generation. The
// snake-case members are the generated binding wire shape.
type ConnectionInfo struct {
	InstanceID string `json:"instance_id"`
	Generation string `json:"generation"`
	Workspace  string `json:"workspace"`
}

// HTTPResponse is one raw byte response the native transport forwards: the
// status, the content type, the exact body bytes, and the one invalid-delivery
// bit. The snake-case members are the generated binding wire shape.
type HTTPResponse struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
	Invalid     bool   `json:"invalid"`
}

// Host is the Wails-bound native desktop host. Every exported method is a
// binding; startup and shutdown are the private lifecycle hooks Run wires
// into Wails. The mutex serializes only short pointer, context and generation
// changes: connection establishment, HTTP requests and event waits happen
// outside it.
type Host struct {
	mu        sync.Mutex
	opts      clienthost.Options
	workspace string

	closed  bool
	ready   chan struct{}
	hostCtx context.Context

	conn       *client.Client
	connCtx    context.Context
	connCancel context.CancelFunc
	events     <-chan protocol.Event
	errs       <-chan error
	generation string
	nextGen    uint64
	pulling    bool

	// host-local presentation injections, following the root adapter's
	// private-function pattern so focused tests exercise the native calls
	// without a window.
	pickDirFn func(context.Context) (string, error)
	titleFn   func(context.Context, string)
}

// New returns one desktop host that stores its startup options and selected
// Workspace without connecting. Open performs the actual connection.
func New(opts clienthost.Options, workspace string) *Host {
	return &Host{opts: opts, workspace: workspace, ready: make(chan struct{})}
}

// Run attaches the host to Wails over the given frontend assets. The private
// startup and shutdown callbacks are the lifecycle hooks; they are not
// exported bindings.
func Run(host *Host, assets fs.FS) error {
	return wails.Run(&options.App{
		Title:       windowTitle(host.workspace),
		Width:       900,
		Height:      700,
		AssetServer: &assetserver.Options{Assets: assets},
		OnStartup:   host.startup,
		OnShutdown:  host.shutdown,
		Bind:        []interface{}{host},
	})
}

// windowTitle is the one shared title algorithm, matching the retained
// root host's naming: the native title is always the selected Workspace
// basename under the product prefix.
func windowTitle(workspace string) string {
	return "Lightcode — " + filepath.Base(workspace)
}

// startup is the Wails startup hook. Wails launches it once and
// asynchronously with no binding-dispatch barrier, so it stores the lifecycle
// context and closes readiness under the short mutex. A startup that races a
// close is discarded: the close already marked the host closed and woke the
// waiters.
func (h *Host) startup(ctx context.Context) {
	h.mu.Lock()
	if !h.closed {
		h.hostCtx = ctx
		close(h.ready)
	}
	h.mu.Unlock()
}

// shutdown is the Wails shutdown hook: it marks the host closed, wakes any
// calls parked before startup exactly once (the single close lives here only
// for the shutdown-before-startup case), and cancels the current — possibly
// still pending — connection attempt, which cancels every in-flight request
// and parked event pull. It never calls Runtime shutdown, Interrupt, or Stop.
func (h *Host) shutdown(context.Context) {
	h.mu.Lock()
	if !h.closed && h.hostCtx == nil {
		close(h.ready)
	}
	h.closed = true
	connCancel := h.connCancel
	h.mu.Unlock()
	if connCancel != nil {
		connCancel()
	}
}

// lifecycleContext waits outside the mutex for the private startup or
// shutdown hook to resolve the host lifecycle, then returns the stored
// startup context or the closed-host error. There is no background fallback:
// the native dialog and window calls require the startup context's frontend
// value.
func (h *Host) lifecycleContext() (context.Context, error) {
	<-h.ready
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("desktop host is closed")
	}
	return h.hostCtx, nil
}

// Open cancels the previous connection generation, connects one compatible
// Runtime, and establishes its event stream. An already reported stream
// establishment failure rejects the open instead of installing a
// healthy-looking connection, and a superseded open cannot install its
// completed connection.
func (h *Host) Open() (ConnectionInfo, error) {
	base, err := h.lifecycleContext()
	if err != nil {
		return ConnectionInfo{}, err
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ConnectionInfo{}, errors.New("desktop host is closed")
	}
	if h.connCancel != nil {
		h.connCancel()
	}
	// Retirement: a nonempty generation always names an installed,
	// non-retired attempt whose matching connection, context and cancel are
	// stored. While an attempt is pending, failed or superseded, no stamp is
	// valid, so neither a request nor a pull nor an old generation's terminal
	// cleanup can act on it.
	h.generation = ""
	h.nextGen++
	attempt := h.nextGen
	generation := strconv.FormatUint(attempt, 10)
	connCtx, cancel := context.WithCancel(base)
	h.connCancel = cancel
	opts := h.opts
	workspace := h.workspace
	h.mu.Unlock()

	conn, err := clienthost.Connect(connCtx, opts)
	if err != nil {
		cancel()
		return ConnectionInfo{}, fmt.Errorf("connect runtime: %w", err)
	}
	events, errs := conn.Events(connCtx)
	// Synchronous establishment: the first read of the fresh cap-1 terminal
	// channel observes the one non-nil error client.Events sends before any
	// close, so a delivered value is an establishment failure and must not
	// install a healthy-looking connection.
	select {
	case streamErr := <-errs:
		cancel()
		return ConnectionInfo{}, fmt.Errorf("event stream establishment: %w", streamErr)
	default:
	}

	h.mu.Lock()
	closedDuringConnect := h.closed
	superseded := h.nextGen != attempt
	if closedDuringConnect || superseded {
		h.mu.Unlock()
		cancel()
		if closedDuringConnect {
			return ConnectionInfo{}, errors.New("desktop host is closed")
		}
		return ConnectionInfo{}, errors.New("connection superseded by a newer open")
	}
	h.conn = conn
	h.connCtx = connCtx
	h.events = events
	h.errs = errs
	h.generation = generation
	h.pulling = false
	h.mu.Unlock()
	return ConnectionInfo{InstanceID: conn.InstanceID(), Generation: generation, Workspace: workspace}, nil
}

// Call performs one lossless generated operation over the named connection
// generation and returns its actual status and available raw body bytes. The
// connection and its context are captured before I/O, so a later open or
// close cancels the call. A received success, a validated Runtime rejection
// or an invalid delivery all return their status/body without rejecting the
// binding; an invalid delivery sets the one local Invalid bit because a
// rejected Wails promise cannot preserve a Go error identity. Any other error
// rejects the binding. Content-Type is JSON for this local invocation result;
// credentials are never exposed.
func (h *Host) Call(generation, operation, params string) (HTTPResponse, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return HTTPResponse{}, errors.New("desktop host is closed")
	}
	if h.generation == "" || h.generation != generation {
		h.mu.Unlock()
		return HTTPResponse{}, errors.New("call carries a stale connection generation")
	}
	conn := h.conn
	ctx := h.connCtx
	h.mu.Unlock()

	status, body, err := clientops.Invoke(ctx, conn, operation, json.RawMessage(params))
	if err != nil {
		var responseErr *clientops.ResponseError
		if errors.As(err, &responseErr) {
			return HTTPResponse{Status: status, ContentType: contentTypeJSON, Body: string(body)}, nil
		}
		if errors.Is(err, clientops.ErrInvalidResponse) {
			return HTTPResponse{Status: status, ContentType: contentTypeJSON, Body: string(body), Invalid: true}, nil
		}
		return HTTPResponse{}, err
	}
	return HTTPResponse{Status: status, ContentType: contentTypeJSON, Body: string(body)}, nil
}

// NextEvent returns one JSON-marshaled generated Event from the connection
// generation's bounded queue. One pull may be outstanding at a time; the
// terminal channel is checked before queued progress, so a known terminal
// failure dominates remaining buffered events, channel closure is loss, and
// a terminal failure cancels only that connection generation.
func (h *Host) NextEvent(generation string) (string, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return "", errors.New("desktop host is closed")
	}
	if h.generation == "" || h.generation != generation {
		h.mu.Unlock()
		return "", errors.New("event pull carries a stale connection generation")
	}
	if h.pulling {
		h.mu.Unlock()
		return "", errors.New("an event pull is already outstanding")
	}
	h.pulling = true
	events := h.events
	errs := h.errs
	ctx := h.connCtx
	h.mu.Unlock()

	var (
		delivered *protocol.Event
		pullErr   error
	)
	// The one terminal-first rule: the terminal channel is checked before any
	// acceptance, and every received event is rechecked against the terminal
	// and the generation context before it is accepted, including the event
	// that wakes the parked select. A raced terminal or canceled generation
	// rejects the pull; the progress channel never outranks the terminal.
	select {
	case err, ok := <-errs:
		pullErr = pullTerminal(err, ok)
	default:
	}
	if pullErr == nil {
		select {
		case err, ok := <-errs:
			pullErr = pullTerminal(err, ok)
		case event, ok := <-events:
			if !ok {
				pullErr = closedEventsLoss(errs)
			} else {
				select {
				case err, ok := <-errs:
					pullErr = pullTerminal(err, ok)
				case <-ctx.Done():
					pullErr = ctx.Err()
				default:
					delivered = &event
				}
			}
		case <-ctx.Done():
			pullErr = ctx.Err()
		}
	}

	// A genuine terminal stream failure cancels only its still-current
	// generation; a canceled context means the generation was already
	// superseded or the host closed, whose owner performs that cancellation.
	// A matching nonempty generation implies the installed attempt cancel is
	// stored, so the read under the mutex is always this generation's own.
	if pullErr != nil && ctx.Err() == nil {
		h.mu.Lock()
		if h.generation == generation {
			cancel := h.connCancel
			h.mu.Unlock()
			cancel()
		} else {
			h.mu.Unlock()
		}
	}

	h.mu.Lock()
	if h.generation == generation {
		h.pulling = false
	}
	h.mu.Unlock()

	if pullErr != nil {
		return "", pullErr
	}
	data, err := json.Marshal(*delivered)
	if err != nil {
		return "", fmt.Errorf("marshal event: %w", err)
	}
	return string(data), nil
}

// pullTerminal maps one terminal-channel delivery: a closed channel is
// stream loss, never an empty event.
func pullTerminal(err error, ok bool) error {
	if !ok {
		return errors.New("event stream closed without a terminal error")
	}
	return err
}

// closedEventsLoss maps one closed progress channel, preferring the terminal
// channel's real error when it raced in behind the closure.
func closedEventsLoss(errs <-chan error) error {
	select {
	case err, ok := <-errs:
		return pullTerminal(err, ok)
	default:
		return pullTerminal(nil, false)
	}
}

// PickDirectory opens the native directory dialog. An empty result means
// cancel; nothing is selected or created on the backend. It waits for the
// real startup context: the dialog requires its frontend value.
func (h *Host) PickDirectory() (string, error) {
	ctx, err := h.lifecycleContext()
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	pick := h.pickDirFn
	h.mu.Unlock()
	if pick == nil {
		pick = func(ctx context.Context) (string, error) {
			return wailsRuntime.OpenDirectoryDialog(ctx, wailsRuntime.OpenDialogOptions{})
		}
	}
	return pick(ctx)
}

// SetTitle applies the native window title from the selected Workspace
// basename. It holds no backend routing lock.
func (h *Host) SetTitle(workspace string) error {
	ctx, err := h.lifecycleContext()
	if err != nil {
		return err
	}
	h.mu.Lock()
	title := h.titleFn
	h.mu.Unlock()
	if title == nil {
		title = func(ctx context.Context, text string) {
			wailsRuntime.WindowSetTitle(ctx, text)
		}
	}
	title(ctx, windowTitle(workspace))
	return nil
}

// AppVersion returns the retained native version helper.
func (h *Host) AppVersion() string {
	return version.String()
}

// PatchJSON is pure strict one-object JSON member replacement: every patch
// member replaces its raw value in the document, absent members are
// preserved, unknown members remain for the server to validate, and unedited
// members and numeric lexemes survive byte-for-byte. It never reads or
// writes Runtime files.
func (h *Host) PatchJSON(document, patch string) (string, error) {
	members, err := parseJSONObject(document, "document")
	if err != nil {
		return "", err
	}
	edits, err := parseJSONObject(patch, "patch")
	if err != nil {
		return "", err
	}
	for name, value := range edits {
		members[name] = value
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(members); err != nil {
		return "", fmt.Errorf("encode document: %w", err)
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

// parseJSONObject strictly decodes one JSON document into its raw member
// map, rejecting non-objects and trailing data.
func parseJSONObject(raw, what string) (map[string]json.RawMessage, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		return nil, fmt.Errorf("decode %s: %w", what, err)
	}
	if members == nil {
		return nil, fmt.Errorf("%s is not a JSON object", what)
	}
	return members, nil
}
