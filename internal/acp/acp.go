package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// Request is an incoming JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is an outgoing JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Notification is an outgoing JSON-RPC 2.0 notification (no id).
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type turnActionParams struct {
	SessionID      string `json:"session_id"`
	Turn           int    `json:"turn"`
	AlsoRevertCode bool   `json:"alsoRevertCode"`
}

// acpOutputJoinTimeout bounds how long shutdown waits for the output drainer.
var acpOutputJoinTimeout = 5 * time.Second

// frameKind selects how the sole drainer filters an output frame against
// presentation current before writing it.
type frameKind uint8

const (
	// frameNormal is written while its sessionID is presentation-current; an empty
	// sessionID is an untagged frame (a response or a global notification) the
	// drainer always writes.
	frameNormal frameKind = iota
	// frameAdvance is a boundary: written when it carries bytes, and it adopts
	// sessionID as the new presentation current (empty for a detach).
	frameAdvance
)

// outFrame is one item the output FIFO carries: the marshaled JSON-RPC bytes (nil
// for an invisible presentation-current advance) plus the drain-time filter tag.
type outFrame struct {
	data      []byte
	kind      frameKind
	sessionID string
}

// Runner drives the ACP stdio protocol.
type Runner struct {
	// agent is the concrete local owner this adapter constructs, initializes, and
	// shuts down; owner is the same object typed for the concrete-only surface
	// (complete-state hydration and ShutdownOwner).
	agent agent.AdapterService
	owner *agent.Agent
	in    io.Reader
	out   io.Writer

	// Output delivery spine: every outbound JSON-RPC frame is appended here and the
	// sole drainer writes r.out, so protocol order is the drainer's write order, not
	// merely non-interleaved bytes. Producers only append and wake the drainer; the
	// drainer decides delivery against presented (presentation current), which
	// advances only when the drainer consumes a boundary. Both fields are under mu.
	mu        sync.Mutex
	outFrames []outFrame
	presented string
	outWake   chan struct{}
	outClosed bool
	outDone   chan struct{}
	outOnce   sync.Once

	// Input dispatch admission: the single scan loop admits one whole line at a
	// time under dispatchMu. Once closed no further line is admitted, and
	// dispatchWG tracks the in-flight line-processing interval so shutdown joins a
	// running dispatch before tearing down the owner and output.
	dispatchMu     sync.Mutex
	dispatchClosed bool
	dispatchWG     sync.WaitGroup

	viewOnce sync.Once
	view     *agent.SessionView

	// routeProjectPath is the project the connection's project-implicit
	// operations scope to, committed alongside the routing-current session id
	// on every set and deliberately left in place when the current session is
	// removed: the project the client was working in is still the right answer
	// for the project-scoped routes. It is read and written only by the
	// dispatch loop, plus the bootstrap that precedes it, so it needs no lock
	// of its own. Empty until the first session is committed — the pre-session
	// state currentProjectPath falls back to the owner-startup project for.
	routeProjectPath string
}

// admitDispatch reserves the line-processing interval unless dispatch admission is
// closed. On success the caller must call dispatchWG.Done once the line is fully
// processed and its response or parse error is enqueued.
func (r *Runner) admitDispatch() bool {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	if r.dispatchClosed {
		return false
	}
	r.dispatchWG.Add(1)
	return true
}

// closeDispatch rejects further line admission under the gate, so a line read after
// shutdown starts is never processed.
func (r *Runner) closeDispatch() {
	r.dispatchMu.Lock()
	r.dispatchClosed = true
	r.dispatchMu.Unlock()
}

// startOutput installs the single output drainer before any producer runs.
func (r *Runner) startOutput() {
	r.outWake = make(chan struct{}, 1)
	r.outDone = make(chan struct{})
	go r.runOutputDrainer()
}

// enqueueFrame appends one frame and wakes the drainer. It never blocks, writes,
// or queries the owner, so it is safe to call from the event callback that runs
// under an owner lock.
func (r *Runner) enqueueFrame(f outFrame) {
	r.mu.Lock()
	if r.outClosed {
		r.mu.Unlock()
		return
	}
	r.outFrames = append(r.outFrames, f)
	r.mu.Unlock()
	select {
	case r.outWake <- struct{}{}:
	default:
	}
}

// enqueue appends one untagged frame (a response or a global notification) the
// drainer always writes.
func (r *Runner) enqueue(data []byte) {
	r.enqueueFrame(outFrame{data: data})
}

// seedPresented sets presentation current at startup, before the client's first
// prompt, so the initially-current session's live events reach the client.
func (r *Runner) seedPresented(id string) {
	r.mu.Lock()
	r.presented = strings.TrimSpace(id)
	r.mu.Unlock()
}

// runOutputDrainer is the only goroutine that writes r.out. It drains frames in
// FIFO order and exits once closed and its queue is empty: closing delivery
// refuses new frames but the drainer still writes what was already queued.
func (r *Runner) runOutputDrainer() {
	defer close(r.outDone)
	for {
		r.mu.Lock()
		if r.outClosed && len(r.outFrames) == 0 {
			r.mu.Unlock()
			return
		}
		if len(r.outFrames) == 0 {
			r.mu.Unlock()
			<-r.outWake
			continue
		}
		frame := r.outFrames[0]
		r.outFrames = r.outFrames[1:]
		write := r.presentAcceptsLocked(frame)
		r.mu.Unlock()
		if write && frame.data != nil {
			_, _ = r.out.Write(frame.data)
		}
	}
}

// presentAcceptsLocked decides whether a frame is written and adopts a boundary's
// destination as presentation current. It runs under mu on the sole drainer, so
// presented is drainer-owned and never queried from the owner: an untagged frame
// (response or global notification) always writes, a session-tagged notification
// writes only while its session is presentation-current, and a boundary always
// advances presented (empty for a detach).
func (r *Runner) presentAcceptsLocked(f outFrame) bool {
	if f.kind == frameAdvance {
		r.presented = f.sessionID
		return true
	}
	return f.sessionID == "" || f.sessionID == r.presented
}

// closeOutput refuses further frames and joins the drainer, letting it write what
// is already queued before it exits, bounded by acpOutputJoinTimeout: the client
// process is still reading the pipe at close, and the queued frames are the
// terminal events shutdown just produced. A drainer blocked inside one write is
// abandoned at that bound.
func (r *Runner) closeOutput() {
	r.outOnce.Do(func() {
		r.mu.Lock()
		r.outClosed = true
		r.mu.Unlock()
		select {
		case r.outWake <- struct{}{}:
		default:
		}
		if r.outDone == nil {
			return
		}
		select {
		case <-r.outDone:
		case <-time.After(acpOutputJoinTimeout):
		}
	})
}

// New creates an ACP Runner that owns one concrete local agent.
func New(a *agent.Agent) *Runner {
	return &Runner{agent: a, owner: a, in: os.Stdin, out: os.Stdout}
}

// Run reads JSON-RPC requests from stdin and dispatches them. It
// blocks until stdin is closed or ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// One signal watcher owns process shutdown: SIGTERM/Interrupt (or a cancelled
	// parent context) cancels sigCtx. The scan loop observes it before its next
	// Scan, and Run observes it to tear down even when a Scan is already blocked on
	// stdin — that blocked Scan is the process-lifetime exception, abandoned rather
	// than joined.
	sigCtx, sigStop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer sigStop()

	r.startOutput()
	r.agent.SetEventHandler(r.handleEvent)
	// Init resumes the most recent acquirable session and returns its id, so
	// the adapter adopts exactly the session that was resumed; a new session
	// is created only when nothing was resumed.
	sessionID := r.agent.Init(ctx)
	if sessionID == "" {
		if id, err := r.agent.NewSession("", "primary"); err == nil {
			sessionID = id
		}
	}
	// Seed the routing project from the resumed session's summary; if that
	// read fails the id is still committed and the project path stays unset so
	// the pre-session fallback applies by itself.
	if summary, err := r.agent.SessionSummaryForSession(sessionID); err == nil {
		r.setCurrent(sessionID, summary)
	} else {
		r.setCurrentSessionID(sessionID)
	}
	r.seedPresented(sessionID)

	scanErr := make(chan error, 1)
	go func() { scanErr <- r.scanLoop(sigCtx) }()

	var err error
	select {
	case err = <-scanErr:
	case <-sigCtx.Done():
	}

	// Teardown, once, in order: close admission under the gate so no further line
	// is processed, join the owner's turns/workers first — so their terminal
	// events (e.g. turn_end) are still admitted, and so a dispatch blocked on the
	// owner can unwind: a compaction's summarizer call derives its context from
	// the owner, only the owner's shutdown cancels it, and the provider client
	// carries no timeout, so behind the dispatch join that cancellation is
	// unreachable. Then join any in-flight dispatch, and drain and close the
	// output writer. The deferred cancel then tears down the host context.
	r.closeDispatch()
	if r.owner != nil {
		// An abandoned shutdown means work is still in flight when the host
		// exits; fold that into the returned error so a script driving this
		// process detects it from the exit code.
		if !r.owner.ShutdownOwner() {
			err = errors.Join(err, fmt.Errorf("owner shutdown abandoned in-flight work"))
		}
	}
	r.dispatchWG.Wait()
	r.closeOutput()
	return err
}

// scanLoop is the single stdin reader. It processes one whole line at a time and
// returns when stdin ends, a read errors, ctx is cancelled between lines, or
// dispatch admission has closed. A Scan already blocked on stdin is not unblocked
// here; Run abandons that goroutine as the process-lifetime exception.
func (r *Runner) scanLoop(ctx context.Context) error {
	scanner := bufio.NewScanner(r.in)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if !scanner.Scan() {
			return scanner.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if !r.admitDispatch() {
			return nil
		}
		r.processLine(ctx, line)
		r.dispatchWG.Done()
	}
}

// processLine parses and dispatches one request line, enqueueing its response or a
// parse error to the output FIFO. It is the whole interval the dispatch WaitGroup
// tracks between Add and Done, so the response is always enqueued before Done.
func (r *Runner) processLine(ctx context.Context, line []byte) {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		r.sendResponse(Response{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &RPCError{Code: -32700, Message: "parse error"},
		})
		return
	}
	r.dispatch(ctx, req)
}

func (r *Runner) dispatch(ctx context.Context, req Request) {
	switch req.Method {
	case "initialize":
		r.handleInitialize(req)
	case "session/new":
		r.handleSessionNew(req)
	case "session/prompt":
		r.handleSessionPrompt(ctx, req)
	case "session/cancel":
		r.handleSessionCancel(req)
	case "session/current":
		r.respond(req.ID, r.currentSessionSummary())
	case "session/list":
		r.handleSessionList(req)
	case "session/switch":
		r.handleSessionSwitch(req)
	case "session/messages":
		r.handleSessionMessages(req)
	case "queue/list":
		r.handleQueueList(req)
	case "session/archive":
		r.handleSessionArchive(req)
	case "session/delete":
		r.handleSessionDelete(req)
	case "session/fork":
		r.handleSessionFork(req)
	case "session/revert_code":
		r.handleRevertCode(req)
	case "session/revert_history":
		r.handleRevertHistory(req)
	case "model/current":
		r.handleModelCurrent(req)
	case "model/list":
		r.respond(req.ID, r.agent.ModelList())
	case "model/switch":
		r.handleModelSwitch(req)
	case "snapshot/list":
		sessionID, err := r.sv().LiveCurrentOrErr()
		if err != nil {
			r.respondError(req.ID, -32000, err.Error())
			return
		}
		list, err := r.agent.SnapshotListForSession(sessionID)
		if err != nil {
			r.respondError(req.ID, -32000, err.Error())
		} else {
			r.respond(req.ID, list)
		}
	case "tokens/usage":
		r.handleTokenUsage(req)
	case "project/current":
		out, err := r.projectCurrent()
		if err != nil {
			r.respondError(req.ID, -32000, err.Error())
			return
		}
		r.respond(req.ID, out)
	case "project/list":
		list, err := r.agent.ProjectList()
		if err != nil {
			r.respondError(req.ID, -32000, err.Error())
		} else {
			r.respond(req.ID, list)
		}
	case "file/read":
		r.handleFileRead(req)
	case "permission/respond":
		r.handlePermissionRespond(req)
	case "permission/suggest":
		r.handlePermissionSuggest(req)
	case "permission/save":
		r.handlePermissionSave(req)
	case "compact":
		r.handleCompact(ctx, req)
	case "warnings/current":
		r.respond(req.ID, warningSnapshot(r.agent.CurrentWarnings()))
	default:
		r.respondError(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// --- Event handler ---

func (r *Runner) handleEvent(ev agent.Event) {
	if ev.SubagentSessionID != "" {
		return
	}
	// This callback only appends the event tagged with its session; the sole
	// drainer, not this callback, decides delivery against presentation current, so
	// the callback never queries the owner for liveness.
	var method string
	var params any

	switch ev.Kind {
	case agent.EventTextDelta:
		method = "agent/message_chunk"
		params = map[string]any{"seq": ev.Seq, "content": ev.Result}
	case agent.EventToolCallStart:
		method = "agent/tool_start"
		params = map[string]any{"seq": ev.Seq, "id": ev.ToolCallID, "name": ev.ToolName, "args": ev.Args}
	case agent.EventToolCallEnd:
		method = "agent/tool_result"
		params = map[string]any{"id": ev.ToolCallID, "name": ev.ToolName, "args": ev.Args, "success": !ev.IsError, "output": ev.Result, "metadata": ev.Metadata}
	case agent.EventBackgroundProcessComplete:
		method = "agent/background_process_complete"
		if ev.BackgroundProcess != nil {
			params = map[string]any{
				"seq":      ev.Seq,
				"id":       ev.BackgroundProcess.ID,
				"command":  ev.BackgroundProcess.Command,
				"reason":   ev.BackgroundProcess.Reason,
				"exitCode": ev.BackgroundProcess.ExitCode,
				"success":  !ev.IsError,
				"output":   ev.Result,
			}
		}
	case agent.EventUserMessageDisplay:
		method = "agent/user_message"
		params = map[string]any{"seq": ev.Seq, "turn": ev.Turn, "content": ev.Result}
	case agent.EventGenericSystemSignal:
		method = "agent/system_signal"
		params = map[string]any{"seq": ev.Seq, "content": "System: " + ev.Result}
	case agent.EventQueueChanged:
		queue := ev.Queue
		if queue == nil {
			queue = []agent.QueuedItem{}
		}
		method = "agent/queue_changed"
		params = map[string]any{"items": queue, "version": ev.QueueVersion}
	case agent.EventUsage:
		method = "agent/usage"
		params = r.tokenUsageForEvent(ev)
	case agent.EventTurnStart:
		method = "agent/turn_start"
		params = map[string]any{"turn": ev.Turn}
	case agent.EventTurnEnd:
		r.sendNotification(Notification{
			JSONRPC: "2.0",
			Method:  "agent/turn_end",
			Params:  map[string]any{"turn": ev.Turn, "cancelled": ev.Cancelled},
		}, ev.SessionID)
		return
	case agent.EventSessionRewrite:
		r.pushResyncForEvent(ev)
		return
	case agent.EventError:
		method = "agent/error"
		// A notification carries a sequence only when the event actually has
		// one: a sessionless error is emitted directly, never sequenced, and a
		// zero-stamped seq would gate it as already shown against every
		// snapshot high-water, dropping the error client-side.
		errorParams := map[string]any{"message": ev.Error, "turn": ev.Turn}
		if ev.Seq != 0 {
			errorParams["seq"] = ev.Seq
		}
		params = errorParams
	case agent.EventPermissionRequest:
		method = "agent/permission_request"
		params = map[string]any{
			"id":                 ev.PermReq.ID,
			"sessionId":          ev.PermReq.SessionID,
			"projectId":          ev.PermReq.ProjectID,
			"tool":               ev.PermReq.ToolName,
			"arg":                ev.PermReq.Arg,
			"resolvedArg":        ev.PermReq.ResolvedArg,
			"canAllowAll":        ev.PermReq.CanAllowAll,
			"canSaveProject":     !ev.PermReq.DisableProjectSave,
			"batchIndex":         ev.PermReq.BatchIndex,
			"batchTotal":         ev.PermReq.BatchTotal,
			"batchFiles":         ev.PermReq.BatchFiles,
			"batchResolvedFiles": ev.PermReq.BatchResolvedFiles,
		}
	case agent.EventPermissionResolved:
		// The client holds the pending mirror; forward the resolution so it can
		// clear a cancelled prompt before the turn-end notification.
		r.sendNotification(Notification{
			JSONRPC: "2.0",
			Method:  "agent/permission_resolved",
			Params:  map[string]any{"id": ev.PermReq.ID, "sessionId": ev.PermReq.SessionID},
		}, ev.SessionID)
		return
	case agent.EventCompactionStart:
		method = "agent/compaction_start"
	case agent.EventCompactionEnd:
		r.sendNotification(Notification{
			JSONRPC: "2.0",
			Method:  "agent/compaction_end",
		}, ev.SessionID)
		return
	case agent.EventWarning:
		method = "agent/warnings"
		params = warningSnapshot(ev.Warnings)
	default:
		return
	}

	r.sendNotification(Notification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}, ev.SessionID)
}

// --- Method handlers ---

func (r *Runner) handleInitialize(req Request) {
	r.respond(req.ID, map[string]any{
		"protocolVersion": 1,
		"serverInfo": map[string]any{
			"name":    "lightcode",
			"version": "0.3.0",
		},
		"capabilities": map[string]any{
			"sessions":    map[string]any{"list": true, "fork": true},
			"permissions": true,
			"models":      true,
		},
	})
}

func (r *Runner) sv() *agent.SessionView {
	r.viewOnce.Do(func() { r.view = agent.NewSessionView(r.agent) })
	return r.view
}

func (r *Runner) setCurrentSessionID(id string) {
	r.sv().SetCurrent(id)
}

// setCurrent commits the connection's routing current: the session id, and the
// project path that scopes its project-implicit operations when the session's
// summary carries one. The two move together on every set; a set that commits
// only the id leaves the previous project answering for the switched-to
// session. A summary carrying no project path — an eviction boundary, for
// example — commits only the id, leaving the routing project in place: a set
// site must never become a clear site. The path is stored as given; trimming
// only tests whether it is empty.
func (r *Runner) setCurrent(id string, summary agent.SessionSummary) {
	if strings.TrimSpace(summary.ProjectPath) != "" {
		r.routeProjectPath = summary.ProjectPath
	}
	r.setCurrentSessionID(id)
}

func (r *Runner) currentSession() (string, error) {
	return r.sv().CurrentOrErr()
}

func (r *Runner) paramsSessionID(explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit, nil
	}
	return r.currentSession()
}

func (r *Runner) liveCurrentSessionID() string {
	return r.sv().LiveCurrent()
}

func (r *Runner) currentSessionSummary() agent.SessionSummary {
	return r.sv().CurrentSummary()
}

// projectCurrent resolves the project of the connection's routing-current session.
// ACP has no project-switch RPC, so the routing-current project is definitionally
// the current session's project; resolving from it avoids the owner/backend current
// and startup cwd, which do not track this connection's session switches. With no
// current session the query has no truthful answer, so it errors like the other
// no-current-session handlers rather than returning a zero-value project.
func (r *Runner) projectCurrent() (agent.ProjectSummary, error) {
	id, err := r.currentSession()
	if err != nil {
		return agent.ProjectSummary{}, err
	}
	summary, err := r.agent.SessionSummaryForSessionOrPersisted(id)
	if err != nil {
		return agent.ProjectSummary{}, err
	}
	out, err := r.agent.ProjectCurrentForPath(summary.ProjectPath)
	return out, err
}

// currentProjectPath resolves the project to scope a project-implicit operation
// to: the project the connection's routing current was committed with, else the
// owner-startup project — the same fallback Run() uses before any session
// exists. It is committed with the session id on every set and deliberately
// survives a current-session removal, so the project-scoped routes keep
// answering for the project the client was working in.
func (r *Runner) currentProjectPath() string {
	if r.routeProjectPath == "" {
		return r.agent.ProjectRoot()
	}
	return r.routeProjectPath
}

func (r *Runner) tokenUsageForEvent(ev agent.Event) agent.TokenReport {
	// The event carries the session's absolute cumulative report (computed under
	// tokensMu); apply it as a replacement rather than querying the owner, so this
	// callback never re-enters the owner while it is emitting under tokensMu.
	if ev.CumulativeTokens != nil {
		return *ev.CumulativeTokens
	}
	return agent.TokenReport{}
}

func (r *Runner) handleSessionNew(req Request) {
	_, err := r.agent.NewSessionForProjectPathWithBoundary(r.currentProjectPath(), "primary", func(state agent.HydrationState) {
		id := strings.TrimSpace(state.Session.ID)
		r.setCurrent(id, state.Session)
		r.sendBoundary(Notification{
			JSONRPC: "2.0",
			Method:  "agent/session_changed",
			Params:  state,
		}, id)
	})
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, r.currentSessionSummary())
}

func (r *Runner) handleSessionPrompt(ctx context.Context, req Request) {
	var params struct {
		SessionID string `json:"session_id"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	sessionID, err := r.paramsSessionID(params.SessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	// The precheck resolves the explicit target's summary before admission;
	// its project path is kept and committed alongside the id inside the
	// admitted callback, never here, which runs before admission and must not
	// move routing. The resolution stays live-only: for an explicit id naming
	// the read-only session it refuses, and the refusal names the contention.
	var summary agent.SessionSummary
	if strings.TrimSpace(params.SessionID) != "" {
		summary, err = r.agent.SessionSummaryForSession(sessionID)
		if err != nil {
			if r.sv().IsReadOnly(sessionID) {
				err = agent.SessionContendedError(sessionID)
			}
			r.respondError(req.ID, -32000, err.Error())
			return
		}
	}
	// The implicit switch an explicit session_id performs commits routing
	// current and advances presentation only inside submit admission, FIFO-ordered
	// ahead of this turn's frames and without a client-visible boundary, so the
	// turn's events reach the client. A submit that fails (a reserved unit or a
	// closed runtime) never fires admitted and leaves routing and presentation on
	// the previous session.
	res, err := r.agent.SubmitToSessionWithBoundary(ctx, sessionID, params.Content, func() {
		if strings.TrimSpace(params.SessionID) != "" {
			prev, _ := r.currentSession()
			r.setCurrent(sessionID, summary)
			if strings.TrimSpace(prev) != sessionID {
				r.enqueueFrame(outFrame{kind: frameAdvance, sessionID: sessionID})
			}
		}
	})
	if err != nil {
		if r.sv().IsReadOnly(sessionID) {
			// The session is read-only: another process drives it, so the
			// owner refuses it as unknown. Say what is actually wrong.
			err = agent.SessionContendedError(sessionID)
		}
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{
		"started": res.Started,
		"turn":    res.Turn,
		"queue":   res.Queue,
		"version": res.Version,
	})
}

func (r *Runner) handleSessionCancel(req Request) {
	sessionID, err := r.currentSession()
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	if err := r.agent.CancelSession(sessionID); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleQueueList(req Request) {
	sessionID, err := r.currentSession()
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	q, err := r.agent.QueueSnapshotForSession(sessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, q)
}

func (r *Runner) handleSessionList(req Request) {
	var params struct {
		State string `json:"state"`
	}
	if req.Params != nil {
		_ = json.Unmarshal(req.Params, &params)
	}
	if params.State == "" {
		params.State = "active"
	}
	list, err := r.agent.SessionListForProjectPath(r.currentProjectPath(), params.State)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, list)
}

func (r *Runner) handleSessionMessages(req Request) {
	var params struct {
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
	}
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			r.respondError(req.ID, -32602, "invalid params")
			return
		}
	}
	id := strings.TrimSpace(params.SessionID)
	if id == "" {
		id = strings.TrimSpace(params.ID)
	}
	if id == "" {
		var err error
		id, err = r.currentSession()
		if err != nil {
			r.respondError(req.ID, -32000, err.Error())
			return
		}
		if _, err := r.agent.SessionSummaryForSessionOrPersisted(id); err != nil {
			r.setCurrentSessionID("")
			r.respondError(req.ID, -32000, err.Error())
			return
		}
	}
	msgs, err := r.agent.SessionMessagesFor(id)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, msgs)
}

func (r *Runner) handleSessionSwitch(req Request) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	// The owner publishes the destination boundary in-commit; the callback commits
	// connection routing current before enqueueing the boundary, which precedes the
	// success response. A failed switch calls emit never, leaving routing unchanged.
	summary, err := r.agent.OpenSessionWithBoundary(params.ID, func(state agent.HydrationState) {
		id := strings.TrimSpace(state.Session.ID)
		r.setCurrent(id, state.Session)
		r.sendBoundary(Notification{
			JSONRPC: "2.0",
			Method:  "agent/session_changed",
			Params:  state,
		}, id)
	})
	if err != nil {
		if !errors.Is(err, snapshot.ErrSessionContended) {
			r.respondError(req.ID, -32000, err.Error())
			return
		}
		// The client named a session another process is driving: the open
		// succeeds as a read-only one. Mark it, commit it as routing current,
		// and notify the client with the read-only hydration state, committing
		// only once the view is available so a failed presentation leaves
		// routing unchanged.
		state, herr := r.owner.HydrateSession(params.ID)
		if herr != nil {
			r.respondError(req.ID, -32000, herr.Error())
			return
		}
		id := strings.TrimSpace(state.Session.ID)
		r.setCurrent(id, state.Session)
		r.sv().SetReadOnly(id)
		r.sendBoundary(Notification{
			JSONRPC: "2.0",
			Method:  "agent/session_changed",
			Params:  state,
		}, id)
		r.respond(req.ID, state.Session)
		return
	}
	r.respond(req.ID, summary)
}

func (r *Runner) handleSessionArchive(req Request) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if err := r.agent.SessionArchive(params.ID); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	wasCurrent := r.sv().RemovedCurrent(params.ID)
	if wasCurrent {
		// Current removal: deterministic no-session boundary before the response, with no
		// complete-state capture of the removed session.
		r.sendBoundary(Notification{
			JSONRPC: "2.0",
			Method:  "agent/session_changed",
			Params:  agent.HydrationState{},
		}, "")
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleSessionDelete(req Request) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if err := r.agent.SessionDelete(params.ID); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	wasCurrent := r.sv().RemovedCurrent(params.ID)
	if wasCurrent {
		// Current removal: deterministic no-session boundary before the response, with no
		// complete-state capture of the removed session.
		r.sendBoundary(Notification{
			JSONRPC: "2.0",
			Method:  "agent/session_changed",
			Params:  agent.HydrationState{},
		}, "")
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleSessionFork(req Request) {
	r.handleTurnAction(req, agent.TurnActionFork)
}

func (r *Runner) handleRevertCode(req Request) {
	r.handleTurnAction(req, agent.TurnActionRevertCode)
}

func (r *Runner) handleRevertHistory(req Request) {
	r.handleTurnAction(req, agent.TurnActionRevertHistory)
}

func (r *Runner) handleTurnAction(req Request, action string) {
	var params turnActionParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if action == agent.TurnActionRevertCode {
		params.AlsoRevertCode = false
	}
	sessionID, err := r.paramsSessionID(params.SessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	// The owner publishes the session-changing revert/fork boundary in-commit before
	// the response; a code-only revert changes no session and emits nothing.
	result, err := r.agent.ApplyTurnActionForSessionWithBoundary(sessionID, params.Turn, action, params.AlsoRevertCode, func(state agent.HydrationState, _ []snapshot.SkippedRevert) {
		id := strings.TrimSpace(state.Session.ID)
		r.setCurrent(id, state.Session)
		r.sendBoundary(Notification{
			JSONRPC: "2.0",
			Method:  "agent/session_changed",
			Params:  state,
		}, id)
	})
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, result)
}

func (r *Runner) handleModelCurrent(req Request) {
	sessionID, err := r.currentSession()
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	model, err := r.agent.CurrentModelForSession(sessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, model)
}

func (r *Runner) handleModelSwitch(req Request) {
	var params struct {
		SessionID string `json:"session_id"`
		Ref       string `json:"ref"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	sessionID, err := r.paramsSessionID(params.SessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	if err := r.agent.SwitchModelForSession(sessionID, params.Ref); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	model, err := r.agent.CurrentModelForSession(sessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, model)
}

func (r *Runner) handleFileRead(req Request) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	content, err := r.agent.ReadFileContentForProjectPath(r.currentProjectPath(), params.Path)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"content": content})
}

func (r *Runner) handlePermissionSuggest(req Request) {
	var params struct {
		SessionID string `json:"session_id"`
		ProjectID string `json:"project_id"`
		Tool      string `json:"tool"`
		Arg       string `json:"arg"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	var (
		suggestions []agent.PermissionSuggestion
		err         error
	)
	if strings.TrimSpace(params.ProjectID) != "" {
		suggestions, err = r.agent.PermissionSuggestForProject(params.ProjectID, params.Tool, params.Arg)
	} else {
		sessionID, sessionErr := r.paramsSessionID(params.SessionID)
		if sessionErr != nil {
			r.respondError(req.ID, -32000, sessionErr.Error())
			return
		}
		suggestions, err = r.agent.PermissionSuggestForSession(sessionID, params.Tool, params.Arg)
	}
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, suggestions)
}

func (r *Runner) handlePermissionSave(req Request) {
	var params struct {
		SessionID string   `json:"session_id"`
		ID        string   `json:"id"`
		Patterns  []string `json:"patterns"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	sessionID, err := r.paramsSessionID(params.SessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	// The protocol client is third-party and may send anything: an unknown
	// request is real information and must be reported. The client already
	// receives a resolution for every prompt it was sent, which is what lets it
	// keep its own mirror straight.
	if err := r.agent.SaveProjectPermissionForSession(sessionID, params.ID, params.Patterns); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handlePermissionRespond(req Request) {
	var params struct {
		SessionID string `json:"session_id"`
		ID        string `json:"id"`
		Allow     *bool  `json:"allow"`
		Action    string `json:"action"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		r.respondError(req.ID, -32602, "invalid params")
		return
	}
	if params.Action == "" {
		if params.Allow == nil {
			r.respondError(req.ID, -32602, "missing permission action")
			return
		}
		if *params.Allow {
			params.Action = "allow"
		} else {
			params.Action = "deny"
		}
	}
	sessionID, err := r.paramsSessionID(params.SessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	// Same as handlePermissionSave: the client is third-party, so an unknown
	// request is reported, not suppressed.
	if err := r.agent.RespondPermissionActionForSession(sessionID, params.ID, params.Action); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleCompact(ctx context.Context, req Request) {
	var params struct {
		SessionID string `json:"session_id"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			r.respondError(req.ID, -32602, "invalid params")
			return
		}
	}
	sessionID := strings.TrimSpace(params.SessionID)
	if sessionID == "" {
		var err error
		sessionID, err = r.sv().LiveCurrentOrErr()
		if err != nil {
			r.respondError(req.ID, -32000, err.Error())
			return
		}
	}
	if err := r.agent.CompactNowForSession(ctx, sessionID); err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, map[string]any{"ok": true})
}

func (r *Runner) handleTokenUsage(req Request) {
	sessionID, err := r.currentSession()
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	report, err := r.agent.TokenUsageForSession(sessionID)
	if err != nil {
		r.respondError(req.ID, -32000, err.Error())
		return
	}
	r.respond(req.ID, report)
}

func warningSnapshot(warnings []agent.PromptWarning) []agent.PromptWarning {
	if warnings == nil {
		return []agent.PromptWarning{}
	}
	return warnings
}

// pushSessionResync re-syncs a session's transcript through the rt.mu-only
// payload. It is the request-handler fallback used when no owner is wired for the
// lifecycle-locked boundary capture; the event-callback resync instead applies
// the payload the producer carried on the event.
func (r *Runner) pushSessionResync(sessionID string) {
	r.sendNotification(Notification{
		JSONRPC: "2.0",
		Method:  "agent/session_resync",
		Params:  r.sv().SessionChangedPayload(sessionID),
	}, sessionID)
}

// pushResyncForEvent emits the replacement transcript the producer carried on the
// rewrite-boundary event, so this callback never re-enters the owner.
func (r *Runner) pushResyncForEvent(ev agent.Event) {
	id := strings.TrimSpace(ev.SessionID)
	if id == "" {
		if current, err := r.currentSession(); err == nil {
			id = current
		}
	}
	payload := agent.SessionPayload{}
	if ev.RewritePayload != nil {
		payload = *ev.RewritePayload
	}
	r.sendNotification(Notification{
		JSONRPC: "2.0",
		Method:  "agent/session_resync",
		Params:  payload,
	}, id)
}

// --- Wire helpers ---

func (r *Runner) respond(id any, result any) {
	r.sendResponse(Response{JSONRPC: "2.0", ID: id, Result: result})
}

func (r *Runner) respondError(id any, code int, msg string) {
	r.sendResponse(Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}})
}

func (r *Runner) sendResponse(resp Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	r.enqueue(append(data, '\n'))
}

// sendNotification appends a session-tagged notification the drainer delivers only
// while that session is presentation-current. An empty session id is a global
// notification the drainer always writes.
func (r *Runner) sendNotification(n Notification, sessionID string) {
	data, err := json.Marshal(n)
	if err != nil {
		return
	}
	r.enqueueFrame(outFrame{data: append(data, '\n'), sessionID: strings.TrimSpace(sessionID)})
}

// sendBoundary appends a navigation/turn-action boundary the drainer writes and
// then adopts sessionID as presentation current (empty for a detach).
func (r *Runner) sendBoundary(n Notification, sessionID string) {
	data, err := json.Marshal(n)
	if err != nil {
		return
	}
	r.enqueueFrame(outFrame{data: append(data, '\n'), kind: frameAdvance, sessionID: strings.TrimSpace(sessionID)})
}
