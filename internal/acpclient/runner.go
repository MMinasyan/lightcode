// Package acpclient bridges newline-delimited JSON-RPC 2.0 over stdio to the
// generated Runtime client. It owns only its connection, input, and output
// resources: requests become generated Runtime operations and delivered
// events, while conversation composition, permissions, and work lifetime
// stay with the Runtime. Closing the bridge cancels no Runtime work.
package acpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/internal/version"
	"github.com/MMinasyan/lightcode/protocol"
)

const (
	jsonRPCVersion = "2.0"

	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeRuntimeError   = -32000
	codeTransport      = -32001

	// maxLineBytes is the existing per-line input bound.
	maxLineBytes = 1 << 20
	// responseQueueSize and eventQueueSize bound the output frames the one
	// writer owns: responses wait for capacity, progress overflow disconnects
	// only this client's stream.
	responseQueueSize = 16
	eventQueueSize    = 256
	// outputJoinTimeout bounds how long Run waits for an uninterruptible
	// stdout write after its teardown began.
	outputJoinTimeout = 5 * time.Second
)

// Options carries the common startup inputs and the initial local Workspace.
type Options struct {
	Startup   clienthost.Options
	Workspace string
}

// Runner is one stdio bridge. It is not reusable across Run calls.
type Runner struct {
	opts       Options
	operations map[string]bool

	input     chan []byte
	responses chan []byte
	events    chan eventFrame

	ctx context.Context
	out io.Writer

	connectMu   sync.Mutex
	mu          sync.Mutex
	initialized bool
	conn        *connection
	workspace   string
	selection   selection

	pumps sync.WaitGroup
}

// selection is the local Session selection and the generation that guards
// every asynchronous adoption, selection change, and repair snapshot.
type selection struct {
	id         string
	generation uint64
}

// eventFrame is one queued notification with the immutable connection that
// served it and, for a selection-bound snapshot, the local selection that
// authorized it. Addresssed responses stay unguarded in their own queue; the
// sole writer discards a notification whose stamps are no longer current, so
// a retired connection's delivery or a superseded snapshot is never written
// after the fact.
type eventFrame struct {
	data      []byte
	conn      *connection
	selection *selection
}

// New builds one bridge over the common startup inputs.
func New(opts Options) *Runner {
	workspace, err := filepath.Abs(opts.Workspace)
	if err != nil {
		workspace = opts.Workspace
	}
	operations := make(map[string]bool)
	for _, name := range clientops.OperationNames() {
		operations[name] = true
	}
	return &Runner{
		opts:       opts,
		operations: operations,
		input:      make(chan []byte, 1),
		responses:  make(chan []byte, responseQueueSize),
		events:     make(chan eventFrame, eventQueueSize),
		workspace:  workspace,
	}
}

// Run serves the bridge until input ends, the output writer fails, the
// process receives an interrupt or termination signal, or the caller's context
// ends. It starts input, signal, and output resources without a connection;
// closing them cancels no Runtime work. Valid input never ends the run: a full
// pending slot backpressures the scanner until dispatch or cancellation
// releases it.
func (r *Runner) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCtx, cancel := context.WithCancel(sigCtx)
	defer cancel()
	r.ctx = runCtx
	r.out = out

	// The completion channel carries the writer's own result beside scanner
	// and context completion: a failed output ends the run instead of being
	// ignored while dispatch continues.
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- r.writeLoop(runCtx)
		close(writerDone)
	}()

	scanned := make(chan error, 1)
	go func() { scanned <- scanLines(runCtx, in, r.input) }()

	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		r.dispatchLoop(runCtx)
	}()

	var runErr error
	select {
	case runErr = <-scanned:
	case runErr = <-writerDone:
	case <-runCtx.Done():
	}
	if err := runCtx.Err(); err != nil {
		runErr = err
	}

	cancel()
	<-dispatchDone
	r.pumps.Wait()
	select {
	case <-writerDone:
	case <-time.After(outputJoinTimeout):
	}
	return runErr
}

// scanLines admits one line at a time into the bridge's single pending slot.
// Every admitted line is a fresh copy, never a slice aliasing Scanner's
// reusable buffer. A full slot backpressures the scanner — capacity alone
// never drops a line or ends the run — and a canceled context releases that
// wait. EOF is therefore observed only after every preceding line was
// admitted and dispatched.
func scanLines(ctx context.Context, in io.Reader, pending chan<- []byte) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, maxLineBytes), maxLineBytes)
	for {
		if ctx.Err() != nil {
			return nil
		}
		if !scanner.Scan() {
			return scanner.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		admitted := append([]byte(nil), line...)
		select {
		case pending <- admitted:
		case <-ctx.Done():
			return nil
		}
	}
}

// dispatchLoop interprets admitted lines one at a time.
func (r *Runner) dispatchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case line := <-r.input:
			r.handleLine(ctx, line)
		}
	}
}

// writeLoop is the one owner of the output writer. It returns the first
// output failure; the writer never continues dispatch after one, and Run
// observes the failure through the writer's own completion channel.
func (r *Runner) writeLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case frame := <-r.responses:
			if err := r.writeFrame(frame); err != nil {
				return err
			}
		case frame := <-r.events:
			if !r.frameIsCurrent(frame) {
				continue
			}
			if err := r.writeFrame(frame.data); err != nil {
				return err
			}
		}
	}
}

// frameIsCurrent reports whether one queued notification still belongs to the
// current connection generation and, for a selection-bound frame, the current
// local selection. The comparison holds r.mu only for the check, never for
// output.
func (r *Runner) frameIsCurrent(frame eventFrame) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != frame.conn {
		return false
	}
	return frame.selection == nil || r.selection == *frame.selection
}

// writeFrame writes one complete frame, reporting the first output failure; a
// short write is the same failure class.
func (r *Runner) writeFrame(frame []byte) error {
	written, err := r.out.Write(frame)
	if err != nil {
		return err
	}
	if written != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}

// request is one parsed JSON-RPC request line. The ID and params keep their
// raw JSON, so a numeric ID is never rounded through a float and a forwarded
// envelope reaches Runtime byte-for-byte.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// handleLine parses and answers one admitted line. Only invalid JSON is a
// parse error; valid JSON outside the request object shape, or an object
// whose typed envelope members do not bind, is an invalid request answered
// with the available legal ID or the null ID. A legitimate notification — a
// valid envelope without an ID — is served without a response.
func (r *Runner) handleLine(ctx context.Context, line []byte) {
	if !json.Valid(line) {
		r.sendError(nil, codeParseError, "parse error")
		return
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(line, &members); err != nil || members == nil {
		r.sendError(nil, codeInvalidRequest, "invalid request")
		return
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		r.sendError(availableID(members), codeInvalidRequest, "invalid request")
		return
	}
	if req.JSONRPC != jsonRPCVersion || req.Method == "" {
		r.sendError(availableID(members), codeInvalidRequest, "invalid request")
		return
	}
	if !validRequestID(req.ID) {
		r.sendError(nil, codeInvalidRequest, "invalid request")
		return
	}
	result, rpcErr := r.dispatch(ctx, req)
	if len(req.ID) == 0 {
		return
	}
	resp := response{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result}
	if rpcErr != nil {
		resp.Result = nil
		resp.Error = rpcErr
	}
	r.send(resp)
}

// availableID returns one request object's raw ID when it carries a legal one
// (a string, number, or null literal); a missing or illegal ID answers with
// the null ID.
func availableID(members map[string]json.RawMessage) json.RawMessage {
	raw, ok := members["id"]
	if !ok || len(raw) == 0 || !validRequestID(raw) {
		return nil
	}
	return raw
}

// validRequestID reports whether one raw request ID is a JSON string, number,
// or the null literal. A missing ID is the notification shape and is legal;
// every other JSON kind is an invalid request envelope.
func validRequestID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '"', '-', 'n':
		return true
	default:
		return trimmed[0] >= '0' && trimmed[0] <= '9'
	}
}

// dispatch classifies one request by its exact method name. Recognition is
// local and independent of the connection state; the initializing gate below
// applies only to recognized Runtime and Session methods.
func (r *Runner) dispatch(ctx context.Context, req request) (json.RawMessage, *rpcError) {
	switch {
	case req.Method == "initialize":
		return r.initialize(ctx, req.Params)
	case req.Method == "workspace/current":
		return r.workspaceCurrent(req.Params)
	case req.Method == "workspace/select":
		return r.workspaceSelect(req.Params)
	case req.Method == "session/select":
		return r.sessionSelect(ctx, req.Params)
	case req.Method == "session/current":
		return r.sessionCurrent(ctx, req.Params)
	case strings.HasPrefix(req.Method, "runtime/"):
		return r.runtimeCall(strings.TrimPrefix(req.Method, "runtime/"), req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

// initialize connects through the common startup owner, subscribes to the
// generated event stream, and describes the compatible Runtime: the native
// server identity, the generated Health, and the exact supported invocation
// names. It creates no Session. A repeated initialize describes the current
// connection instead of opening a second stream.
func (r *Runner) initialize(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpcError) {
	if err := decodeParams(params, &struct{}{}); err != nil {
		return nil, err
	}
	conn, err := r.establish(ctx)
	if err != nil {
		return nil, &rpcError{Code: codeTransport, Message: err.Error()}
	}
	encoded, err := json.Marshal(initializeResult{
		ServerInfo: serverInfo{Name: "lightcode", Version: version.String()},
		Runtime:    conn.health,
		Methods:    supportedMethods(),
	})
	if err != nil {
		return nil, &rpcError{Code: codeTransport, Message: fmt.Sprintf("initialize result: %v", err)}
	}
	return encoded, nil
}

type initializeResult struct {
	ServerInfo serverInfo      `json:"serverInfo"`
	Runtime    protocol.Health `json:"runtime"`
	Methods    []string        `json:"methods"`
}

// serverInfo is the native identity only: no Runtime DTO and no protocol
// negotiation is hidden here, and no permission or claim capability exists.
type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// supportedMethods returns the exact supported invocation names, sorted.
func supportedMethods() []string {
	names := clientops.OperationNames()
	slices.Sort(names)
	return names
}

// runtimeCall forwards one recognized invocation to its generated Runtime
// operation on the serving connection. The call runs under that connection
// generation's context, so retiring the generation cancels its HTTP
// resources and the caller receives the real transport error under its
// original request ID; before a successful initialize every recognized
// Runtime method fails as an unavailable transport, without backend work;
// an unknown operation name is local invalid params.
func (r *Runner) runtimeCall(operation string, params json.RawMessage) (json.RawMessage, *rpcError) {
	if !r.operations[operation] {
		return nil, errInvalidParams("unknown operation %q", operation)
	}
	conn, rpcErr := r.requireConnection()
	if rpcErr != nil {
		return nil, rpcErr
	}
	result, err := clientops.Call(conn.ctx, conn.client, operation, params)
	if err != nil {
		return nil, callError(err)
	}
	return result, nil
}

// callError maps one invocation failure onto its JSON-RPC shape: local
// binding failures are invalid params, a received Runtime error or partial
// result rides in error.data, and every transport failure is the separate
// unavailable-code family, never a committed Operation failure.
func callError(err error) *rpcError {
	var responseErr *clientops.ResponseError
	switch {
	case errors.Is(err, clientops.ErrInvalidParams):
		return &rpcError{Code: codeInvalidParams, Message: err.Error()}
	case errors.As(err, &responseErr):
		data, _ := json.Marshal(responseErr.Payload)
		if responseErr.Partial != nil {
			data, _ = json.Marshal(responseErr.Partial)
		}
		return &rpcError{Code: codeRuntimeError, Message: responseErr.Error(), Data: data}
	default:
		return &rpcError{Code: codeTransport, Message: err.Error()}
	}
}

// workspaceCurrent reports the local selected root. It is local state and
// operates before initialization.
func (r *Runner) workspaceCurrent(params json.RawMessage) (json.RawMessage, *rpcError) {
	if err := decodeParams(params, &struct{}{}); err != nil {
		return nil, err
	}
	r.mu.Lock()
	workspace := r.workspace
	r.mu.Unlock()
	encoded, _ := json.Marshal(workspace)
	return encoded, nil
}

// workspaceSelect sets the local normalized Workspace and clears the local
// Session selection. It performs no Runtime mutation and operates before
// initialization.
func (r *Runner) workspaceSelect(params json.RawMessage) (json.RawMessage, *rpcError) {
	var doc struct {
		Workspace string `json:"workspace"`
	}
	if err := decodeParams(params, &doc, "workspace"); err != nil {
		return nil, err
	}
	workspace, err := filepath.Abs(doc.Workspace)
	if err != nil {
		return nil, errInvalidParams("workspace: %v", err)
	}
	r.mu.Lock()
	r.workspace = workspace
	r.selection.id = ""
	r.selection.generation++
	r.mu.Unlock()
	encoded, _ := json.Marshal(workspace)
	return encoded, nil
}

// sessionSelect passively selects one explicit Session: it hydrates that ID
// and adopts the local selection and Workspace only when this request is
// still the current selection intent. It never creates, claims, or reopens a
// Session.
func (r *Runner) sessionSelect(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpcError) {
	var doc struct {
		SessionID string `json:"session_id"`
	}
	if err := decodeParams(params, &doc, "session_id"); err != nil {
		return nil, err
	}
	conn, rpcErr := r.requireConnection()
	if rpcErr != nil {
		return nil, rpcErr
	}
	stamp := r.selectionSnapshot()
	raw, err := r.hydrate(conn, doc.SessionID)
	if err != nil {
		return nil, callError(err)
	}
	var hydration protocol.Hydration
	if err := json.Unmarshal(raw, &hydration); err != nil {
		return nil, &rpcError{Code: codeTransport, Message: fmt.Sprintf("hydration: %v", err)}
	}
	r.adoptSelection(conn, stamp, doc.SessionID, hydration.Session.Workspace)
	return raw, nil
}

// sessionCurrent returns a fresh generated hydration for the local selected
// ID, or the JSON null when nothing is selected.
func (r *Runner) sessionCurrent(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpcError) {
	if err := decodeParams(params, &struct{}{}); err != nil {
		return nil, err
	}
	conn, rpcErr := r.requireConnection()
	if rpcErr != nil {
		return nil, rpcErr
	}
	selected := r.selectionSnapshot()
	if selected.id == "" {
		return json.RawMessage("null"), nil
	}
	raw, err := r.hydrate(conn, selected.id)
	if err != nil {
		return nil, callError(err)
	}
	return raw, nil
}

// decodeParams binds one method's params document. Absent params are the
// empty document; a supplied null, any other non-object shape, any undeclared
// member, and any required or provided member that is null is invalid params.
func decodeParams(raw json.RawMessage, target any, required ...string) *rpcError {
	members, rpcErr := paramsMembers(raw)
	if rpcErr != nil {
		return rpcErr
	}
	for _, name := range required {
		if _, ok := members[name]; !ok {
			return errInvalidParams("params is missing required member %q", name)
		}
	}
	for name, value := range members {
		if isJSONNull(value) {
			return errInvalidParams("params member %q must not be null", name)
		}
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return errInvalidParams("params: %v", err)
	}
	return nil
}

// paramsMembers reads one method's params as a member map; absent params are
// empty and a supplied null is invalid params like every other non-object
// shape.
func paramsMembers(raw json.RawMessage) (map[string]json.RawMessage, *rpcError) {
	if len(raw) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	if isJSONNull(raw) {
		return nil, errInvalidParams("params must not be null")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, errInvalidParams("params: %v", err)
	}
	if members == nil {
		return nil, errInvalidParams("params must be one JSON object")
	}
	return members, nil
}

func errInvalidParams(format string, args ...any) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// send admits one response frame, waiting for output capacity with caller
// cancellation rather than dropping it.
func (r *Runner) send(resp response) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	frame := append(data, '\n')
	select {
	case r.responses <- frame:
	case <-r.ctx.Done():
	}
}

// sendError answers one request with a JSON-RPC error; a nil ID is the parse
// error's null ID.
func (r *Runner) sendError(id json.RawMessage, code int, message string) {
	r.send(response{JSONRPC: jsonRPCVersion, ID: id, Error: &rpcError{Code: code, Message: message}})
}

// notify admits one connection-bound notification, or one selection-bound
// snapshot when stamp is non-nil, waiting for output capacity with caller
// cancellation rather than dropping it. The writer decides currency.
func (r *Runner) notify(conn *connection, method string, params json.RawMessage, stamp *selection) bool {
	data, err := json.Marshal(notification{JSONRPC: jsonRPCVersion, Method: method, Params: params})
	if err != nil {
		return false
	}
	select {
	case r.events <- eventFrame{data: append(data, '\n'), conn: conn, selection: stamp}:
		return true
	case <-r.ctx.Done():
		return false
	}
}

// enqueueProgress admits one runtime/event frame without waiting: a full
// progress queue is a delivery gap and disconnects this client's stream with
// a recorded resync instead of blocking delivery or dropping silently.
func (r *Runner) enqueueProgress(conn *connection, event protocol.Event) bool {
	payload, err := json.Marshal(event)
	if err != nil {
		return false
	}
	data, err := json.Marshal(notification{JSONRPC: jsonRPCVersion, Method: "runtime/event", Params: payload})
	if err != nil {
		return false
	}
	select {
	case r.events <- eventFrame{data: append(data, '\n'), conn: conn}:
		return true
	default:
		return false
	}
}

// selectionSnapshot reads the current local selection under its generation.
func (r *Runner) selectionSnapshot() selection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.selection
}

// adoptSelection applies one completed addressed selection only while its
// serving connection and the captured local selection intent are still
// current. The addressed response itself is always returned to its caller.
func (r *Runner) adoptSelection(conn *connection, stamp selection, sessionID, workspace string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != conn || r.selection.generation != stamp.generation {
		return
	}
	r.workspace = workspace
	r.selection.id = sessionID
	r.selection.generation++
}
