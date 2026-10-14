package cliclient

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/internal/terminal"
	"github.com/MMinasyan/lightcode/protocol"
)

var commands = []string{"/help", "/clear", "/exit", "/model", "/agent", "/session", "/project", "/new", "/resume", "/context", "/warnings", "/compact", "/copy", "/fork", "/revert", "/children", "/stop", "/runtime"}

type attempt struct {
	id, operation, session    string
	content                   []protocol.ContentPart
	mode                      protocol.SubmitRequestMode
	boundary, rewindOperation string
	draftRevision             uint64
	stamp                     stamp
	outcome                   string
	inflight                  bool
	restoreHandled            bool
	restore                   json.RawMessage
}
type menu struct {
	title, kind, lifecycle string
	items                  []terminal.MenuItem
	selected               int
	stamp                  stamp
	listGeneration         uint64
}

func textParts(text string) []protocol.ContentPart {
	var part protocol.ContentPart
	_ = part.FromTextPart(protocol.TextPart{Kind: protocol.TextPartKindText, Text: text})
	return []protocol.ContentPart{part}
}
func contentText(parts []protocol.ContentPart) string {
	var text []string
	for _, part := range parts {
		kind, _ := part.Discriminator()
		if kind == "text" {
			p, err := part.AsTextPart()
			if err == nil {
				text = append(text, p.Text)
			}
		}
	}
	return strings.Join(text, "\n")
}
func knownError(err error) bool {
	var response *clientops.ResponseError
	return errors.As(err, &response) || errors.Is(err, clientops.ErrInvalidParams)
}
func resultText(raw json.RawMessage, err error) string {
	if err == nil {
		return terminal.RenderSystemMsg(string(raw))
	}
	var response *clientops.ResponseError
	if errors.As(err, &response) && response.Partial != nil {
		raw, _ := json.Marshal(response.Partial)
		return terminal.RenderWarningMsg(string(raw))
	}
	if knownError(err) {
		return terminal.RenderErrorMsg(err.Error())
	}
	return terminal.RenderWarningMsg("Outcome unknown: " + err.Error())
}

func (c *CLI) navigationLocked() {
	c.selection++
	c.listGeneration++
	c.menu = nil
	c.forkBoundary = nil
	c.forkRewind = false
	c.clearProgressLocked()
}

// beginNavigationLocked starts one navigation intent: advancing the existing
// selection stamp is the one guard that rejects a superseded adoption or error,
// and it supersedes stamped task output and passive lists while the accepted
// source stays displayed until adoption.
func (c *CLI) beginNavigationLocked() stamp {
	c.navigationLocked()
	return c.stampLocked()
}

// resumeReaderLocked restarts one retained reader under the current selection:
// its chain was superseded (or the reader is dirty) but its accepted state is
// still the displayed one, so the existing serialized reader owner re-reads it.
func (c *CLI) resumeReaderLocked(r *sessionReader) {
	if r == nil {
		return
	}
	if r.chain != nil {
		r.chain.cancel()
		r.chain = nil
	}
	r.dirty = true
	c.kickReaderLocked(r)
}

// navigationReport keeps a failed current navigation live: the retained source
// root and inspector resume reading under the current selection, and the error
// is delivered. A superseded navigation neither refreshes nor clears the newer
// destination.
func (c *CLI) navigationReport(s stamp, text string) {
	c.mu.Lock()
	current := c.currentLocked(s)
	if current {
		c.resumeReaderLocked(c.root)
		c.resumeReaderLocked(c.child)
	}
	c.mu.Unlock()
	if current {
		c.report(s, text, false)
	}
}

// adoptRoot publishes one resolved destination — its immutable Workspace, its
// root reader and an empty child inspector. Adoption is a second navigation
// step: every task, menu and Fork begun against the retained source during
// resolution is superseded before the destination publishes, and a superseded
// resolution never adopts.
func (c *CLI) adoptRoot(s stamp, workspace, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentLocked(s) {
		return
	}
	c.navigationLocked()
	c.resetReadersLocked()
	if workspace != "" {
		c.workspace = workspace
	}
	c.rootID = id
	c.childID = ""
	c.root = c.newReaderLocked(id)
	c.kickReaderLocked(c.root)
	c.notifyLocked()
}

// selectRoot begins one named root navigation.
func (c *CLI) selectRoot(id string) {
	c.mu.Lock()
	s := c.beginNavigationLocked()
	c.mu.Unlock()
	c.task(func() { c.selectRootWith(s, id) })
}

// selectRootWith resolves one named root through the globally addressed passive
// Session read, reopens an archived destination and adopts the destination
// Workspace on success; the adopted reader owns the one fresh hydration. A
// failed read, eligibility check or reopen keeps the previous accepted
// selection live.
func (c *CLI) selectRootWith(s stamp, id string) {
	var hydration protocol.Hydration
	if _, err := call(s, "GetSessionHydration", id, nil, nil, &hydration); err != nil {
		c.navigationReport(s, terminal.RenderErrorMsg(err.Error()))
		return
	}
	if hydration.Session.SessionId != id {
		c.navigationReport(s, terminal.RenderErrorMsg("Session read answered for a different Session"))
		return
	}
	if hydration.Session.ParentSessionId != nil {
		c.navigationReport(s, terminal.RenderErrorMsg("Select a root Session; children are inspection-only"))
		return
	}
	if hydration.Session.Lifecycle == protocol.Archived {
		// The reopen is a work mutation: an already-superseded resolution must
		// not issue one.
		c.mu.Lock()
		current := c.currentLocked(s)
		c.mu.Unlock()
		if !current {
			return
		}
		if _, err := call(s, "ReopenSession", id, nil, nil, nil); err != nil {
			c.navigationReport(s, terminal.RenderErrorMsg(err.Error()))
			return
		}
	}
	c.adoptRoot(s, hydration.Session.Workspace, id)
}

// resumeNewest selects the newest open root in the current Workspace: this is
// the retained no-argument form. It never creates a Session and never consults
// archived Sessions; an empty open list reports the retained no-session
// outcome and keeps the current selection.
func (c *CLI) resumeNewest(s stamp) {
	var list []protocol.Session
	_, err := call(s, "ListSessions", "", nil, protocol.ListSessionsParams{Workspace: s.workspace, Lifecycle: protocol.SessionLifecycle("open")}, &list)
	if err != nil {
		c.navigationReport(s, terminal.RenderErrorMsg(err.Error()))
		return
	}
	roots := rootSessions(list)
	if len(roots) == 0 {
		c.navigationReport(s, terminal.RenderErrorMsg("no active sessions"))
		return
	}
	c.adoptRoot(s, roots[0].Workspace, roots[0].SessionId)
}
func (c *CLI) inspectChild(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.navigationLocked()
	if c.child != nil && c.child.chain != nil {
		c.child.chain.cancel()
	}
	c.child = nil
	c.childID = id
	c.restampRootLocked()
	if id != "" {
		c.child = c.newReaderLocked(id)
		c.kickReaderLocked(c.child)
	}
	c.notifyLocked()
}
func (c *CLI) selectWorkspace(workspace string) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		c.mu.Lock()
		s := c.stampLocked()
		c.mu.Unlock()
		c.report(s, terminal.RenderErrorMsg(err.Error()), false)
		return
	}
	c.mu.Lock()
	c.navigationLocked()
	c.resetReadersLocked()
	c.workspace = abs
	c.rootID = ""
	c.childID = ""
	s := c.stampLocked()
	c.mu.Unlock()
	c.task(func() { c.compose(s) })
}

func rootSessions(list []protocol.Session) []protocol.Session {
	filtered := make([]protocol.Session, 0, len(list))
	for _, session := range list {
		if session.ParentSessionId == nil {
			filtered = append(filtered, session)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].LastActivity.Equal(filtered[j].LastActivity) {
			return filtered[i].SessionId < filtered[j].SessionId
		}
		return filtered[i].LastActivity.After(filtered[j].LastActivity)
	})
	return filtered
}
func (c *CLI) compose(s stamp) {
	c.mu.Lock()
	if !c.currentLocked(s) {
		c.mu.Unlock()
		return
	}
	c.listGeneration++
	generation := c.listGeneration
	c.mu.Unlock()
	var list []protocol.Session
	_, err := call(s, "ListSessions", "", nil, protocol.ListSessionsParams{Workspace: s.workspace, Lifecycle: "open"}, &list)
	c.mu.Lock()
	if !c.currentLocked(s) || generation != c.listGeneration {
		c.mu.Unlock()
		return
	}
	if err != nil {
		c.mu.Unlock()
		c.report(s, terminal.RenderErrorMsg(err.Error()), false)
		return
	}
	list = rootSessions(list)
	c.sessions = list
	if len(list) != 0 {
		c.rootID = list[0].SessionId
		c.root = c.newReaderLocked(c.rootID)
		c.kickReaderLocked(c.root)
		c.notifyLocked()
		c.mu.Unlock()
		return
	}
	unknown := c.unknownCreates[s.workspace]
	c.mu.Unlock()
	if unknown {
		c.report(s, terminal.RenderWarningMsg("Outcome unknown: prior CreateSession in this Workspace; inspect /session before creating again."), false)
		return
	}
	c.create(s)
}
func (c *CLI) create(s stamp) {
	var session protocol.Session
	raw, err := call(s, "CreateSession", "", protocol.CreateSessionRequest{AgentType: "primary", Workspace: s.workspace}, nil, &session)
	c.mu.Lock()
	if err != nil && !knownError(err) {
		c.unknownCreates[s.workspace] = true
	}
	current := c.currentLocked(s)
	if err == nil && current {
		c.resetReadersLocked()
		c.rootID = session.SessionId
		c.childID = ""
		c.root = c.newReaderLocked(c.rootID)
		c.kickReaderLocked(c.root)
		c.notifyLocked()
	}
	c.mu.Unlock()
	c.report(s, resultText(raw, err), true)
	if err != nil && current {
		c.refreshList(s, "open", false)
	}
}

func (c *CLI) refreshList(s stamp, lifecycle string, show bool) {
	c.mu.Lock()
	if !c.currentLocked(s) {
		c.mu.Unlock()
		return
	}
	c.listGeneration++
	generation := c.listGeneration
	c.mu.Unlock()
	var list []protocol.Session
	_, err := call(s, "ListSessions", "", nil, protocol.ListSessionsParams{Workspace: s.workspace, Lifecycle: protocol.SessionLifecycle(lifecycle)}, &list)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentLocked(s) || generation != c.listGeneration {
		return
	}
	if err != nil {
		c.taskLocked(func() { c.reportList(s, generation, terminal.RenderErrorMsg(err.Error())) })
		return
	}
	list = rootSessions(list)
	c.sessions = list
	if show {
		items := make([]terminal.MenuItem, 0, len(list)+1)
		for _, session := range list {
			items = append(items, terminal.MenuItem{Label: session.SessionId, Detail: session.AgentType, Selectable: true, Current: session.SessionId == c.rootID, Extra: session})
		}
		outstanding := false
		for _, a := range c.attempts {
			if a.session == s.root && a.stamp.workspace == s.workspace && strings.HasPrefix(a.outcome, "unknown") {
				outstanding = true
			}
		}
		if outstanding {
			items = append(items, terminal.MenuItem{Label: "Outstanding attempts", Selectable: true, Extra: "attempts"})
		}
		c.menu = &menu{title: "Sessions — " + lifecycle, kind: "session", lifecycle: lifecycle, items: items, selected: terminal.FirstSelectableMenuItem(items), stamp: s, listGeneration: generation}
	}
	c.notifyLocked()
}

func (c *CLI) archiveDelete(s stamp, id string, deleteAfter bool) {
	c.task(func() {
		raw, err := call(s, "ArchiveSession", id, nil, nil, nil)
		c.report(s, resultText(raw, err), true)
		if err != nil {
			c.mu.Lock()
			if c.currentLocked(s) {
				c.refreshReadersLocked()
			}
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		// Every known local lifecycle mutation invalidates older passive lists,
		// even when it addressed a root not currently selected.
		if c.conn == s.conn {
			c.invalidateListLocked()
		}
		if c.conn == s.conn && !c.closed && c.rootID == id {
			c.navigationLocked()
			c.resetReadersLocked()
			c.rootID = ""
			c.childID = ""
		}
		fresh := c.stampLocked()
		current := c.conn == s.conn && !c.closed
		c.mu.Unlock()
		if current {
			c.task(func() { c.refreshList(fresh, "open", false) })
		}
		if !deleteAfter {
			return
		}
		// Refresh runs independently: a slow list cannot delay archive-first DELETE.
		raw, err = call(s, "DeleteSession", id, nil, nil, nil)
		c.report(s, resultText(raw, err), true)
		c.mu.Lock()
		if c.conn == s.conn {
			c.invalidateListLocked()
		}
		fresh = c.stampLocked()
		current = c.conn == s.conn && !c.closed
		c.mu.Unlock()
		if current {
			c.refreshList(fresh, "open", false)
		}
	})
}

func operationID() string {
	var bytes [16]byte
	// On the supported Go version, rand.Read fills the buffer or terminates.
	rand.Read(bytes[:])
	return hex.EncodeToString(bytes[:])
}

func (c *CLI) formAttemptLocked(operation, id string) (*attempt, error) {
	if c.conn == nil || c.rootID == "" || c.childID != "" || c.root == nil || c.root.hydration == nil || c.root.hydration.Session.ParentSessionId != nil {
		return nil, errors.New("Return to a selected root Session to send work")
	}
	a := &attempt{id: id, operation: operation, session: c.rootID, content: textParts(c.draft.String()), mode: protocol.SubmitRequestModeRegular, draftRevision: c.draftRevision, stamp: c.stampLocked(), inflight: true}
	if c.queued {
		a.mode = protocol.SubmitRequestModeQueued
	}
	c.queued = false
	if operation == "ForkSession" {
		if c.forkBoundary == nil {
			return nil, errors.New("No fork boundary selected")
		}
		a.boundary = c.forkBoundary.ItemId
		if c.forkRewind && c.forkBoundary.OperationId != nil {
			a.rewindOperation = *c.forkBoundary.OperationId
		}
	}
	c.attempts[a.id] = a
	return a, nil
}
func (c *CLI) runAttempt(id string) {
	c.mu.Lock()
	a := c.attempts[id]
	if a == nil {
		c.mu.Unlock()
		return
	}
	snapshot := *a
	transport := c.stampLocked()
	c.mu.Unlock()
	// Retrying changes the transport only, never the attempt's initiating stamp.
	s := snapshot.stamp
	s.conn = transport.conn
	s.ctx = transport.ctx
	if s.conn == nil {
		c.mu.Lock()
		if a = c.attempts[id]; a != nil {
			a.inflight = false
			a.outcome = "unknown"
		}
		c.mu.Unlock()
		return
	}
	if snapshot.rewindOperation != "" && !snapshot.restoreHandled {
		raw, err := call(s, "RevertSessionCode", snapshot.session, protocol.RevertCodeRequest{BoundaryOperationId: snapshot.rewindOperation}, nil, nil)
		c.report(snapshot.stamp, resultText(raw, err), true)
		if err != nil && !knownError(err) {
			c.mu.Lock()
			if a = c.attempts[id]; a != nil {
				a.inflight = false
				a.outcome = "unknown restore"
			}
			if c.currentLocked(snapshot.stamp) {
				c.refreshReadersLocked()
			}
			c.mu.Unlock()
			return
		}
		if err != nil {
			var response *clientops.ResponseError
			if errors.As(err, &response) {
				if response.Partial != nil {
					raw, _ = json.Marshal(response.Partial)
				} else {
					raw, _ = json.Marshal(response.Payload)
				}
			}
		}
		c.mu.Lock()
		if a = c.attempts[id]; a != nil {
			a.restoreHandled = true
			a.restore = raw
		}
		c.mu.Unlock()
	}
	var body any
	switch snapshot.operation {
	case "SubmitSession":
		body = protocol.SubmitRequest{OperationId: id, Content: snapshot.content, Mode: snapshot.mode}
	case "CompactSession":
		body = protocol.CompactRequest{OperationId: id}
	case "ForkSession":
		body = protocol.ForkRequest{OperationId: id, Content: snapshot.content, BoundaryItemId: snapshot.boundary}
	}
	raw, err := call(s, snapshot.operation, snapshot.session, body, nil, nil)
	c.report(snapshot.stamp, resultText(raw, err), true)
	c.mu.Lock()
	defer c.mu.Unlock()
	a = c.attempts[id]
	if a == nil {
		return
	}
	a.inflight = false
	if err != nil {
		if knownError(err) {
			a.outcome = "rejected"
		} else {
			a.outcome = "unknown"
		}
	} else {
		a.outcome = "accepted"
		c.acceptDraftLocked(a)
		if snapshot.operation == "ForkSession" && c.currentLocked(snapshot.stamp) {
			var result protocol.ForkResult
			if json.Unmarshal(raw, &result) == nil {
				c.navigationLocked()
				c.resetReadersLocked()
				c.rootID = result.Session.SessionId
				c.childID = ""
				c.root = c.newReaderLocked(c.rootID)
				c.kickReaderLocked(c.root)
			}
		}
	}
	if c.currentLocked(snapshot.stamp) {
		c.refreshReadersLocked()
	}
	c.notifyLocked()
}
func (c *CLI) acceptDraftLocked(a *attempt) {
	if a.operation != "CompactSession" && c.draftRevision == a.draftRevision {
		c.draft.Clear()
		c.forkBoundary = nil
		c.forkRewind = false
	}
}
func (c *CLI) reconcileAttemptsLocked(r *sessionReader) {
	for _, a := range c.attempts {
		if a.session != r.id || a.outcome != "unknown" {
			continue
		}
		found := false
		for _, op := range r.hydration.Operations {
			if op.OperationId == a.id {
				found = true
			}
		}
		for _, list := range [][]protocol.PendingInput{r.hydration.Pending.Steering, r.hydration.Pending.Queued} {
			for _, input := range list {
				if input.OperationId == a.id {
					found = true
				}
			}
		}
		if found {
			a.outcome = "accepted"
			c.acceptDraftLocked(a)
		}
	}
}
func (c *CLI) retryAttempt(id string) {
	c.mu.Lock()
	a := c.attempts[id]
	if a == nil || a.inflight || a.outcome != "unknown" {
		c.mu.Unlock()
		return
	}
	a.inflight = true
	s := c.stampLocked()
	c.mu.Unlock()
	c.report(s, terminal.RenderWarningMsg("Explicit retry retains its ID; buffered input can be duplicated before admission."), false)
	c.task(func() { c.runAttempt(id) })
}
func (c *CLI) control(s stamp, operation, id string) {
	c.task(func() {
		raw, err := call(s, operation, id, nil, nil, nil)
		c.report(s, resultText(raw, err), true)
		c.mu.Lock()
		if c.currentLocked(s) {
			c.refreshReadersLocked()
		}
		c.mu.Unlock()
	})
}

func (c *CLI) command(text string) {
	parts := strings.SplitN(strings.TrimSpace(text), " ", 2)
	name := parts[0]
	arg := ""
	if len(parts) > 1 {
		arg = strings.TrimSpace(parts[1])
	}
	c.mu.Lock()
	s := c.stampLocked()
	r := c.displayedLocked()
	c.mu.Unlock()
	switch name {
	case "/help":
		c.report(s, terminal.RenderSystemMsg(strings.Join(commands, " ")+"\n/runtime <OperationId> <JSON envelope>; /children returns to root; /project [directory]\nRuntime operations: "+strings.Join(clientops.OperationNames(), ", ")), false)
	case "/exit":
		c.requestExit(nil)
	case "/clear":
		c.report(s, "\x1b[2J\x1b[3J\x1b[H", false)
	case "/copy":
		c.copyCanonical(s)
	case "/context":
		c.mu.Lock()
		if r != nil && r.hydration != nil {
			raw, _ := json.Marshal(r.hydration.Usage)
			c.mu.Unlock()
			c.report(s, terminal.RenderSystemMsg(string(raw)), false)
		} else {
			c.mu.Unlock()
		}
	case "/warnings":
		c.mu.Lock()
		var warnings []protocol.Warning
		if r != nil && r.hydration != nil {
			warnings = r.hydration.Warnings
		} else if c.warnings != nil {
			for _, w := range c.warnings.Warnings {
				if w.SessionId == nil {
					warnings = append(warnings, w)
				}
			}
		}
		raw, _ := json.Marshal(warnings)
		c.mu.Unlock()
		c.report(s, terminal.RenderSystemMsg(string(raw)), false)
	case "/stop":
		if s.root != "" {
			c.control(s, "StopSession", s.root)
		}
	case "/compact":
		id := operationID()
		c.mu.Lock()
		a, err := c.formAttemptLocked("CompactSession", id)
		c.mu.Unlock()
		if err != nil {
			c.report(s, terminal.RenderErrorMsg(err.Error()), false)
		} else {
			c.task(func() { c.runAttempt(a.id) })
		}
	case "/session":
		c.task(func() { c.refreshList(s, "open", true) })
	case "/new":
		c.mu.Lock()
		c.navigationLocked()
		c.resetReadersLocked()
		c.rootID = ""
		c.childID = ""
		s = c.stampLocked()
		c.mu.Unlock()
		c.task(func() { c.create(s) })
	case "/resume":
		c.mu.Lock()
		intent := c.beginNavigationLocked()
		c.mu.Unlock()
		if arg == "" {
			c.task(func() { c.resumeNewest(intent) })
		} else {
			c.task(func() { c.selectRootWith(intent, arg) })
		}
	case "/project":
		if arg != "" {
			c.selectWorkspace(arg)
		} else {
			c.task(func() { c.workspaceMenu(s) })
		}
	case "/children":
		c.mu.Lock()
		inspected := c.childID != ""
		c.mu.Unlock()
		if inspected {
			c.inspectChild("")
		} else {
			c.task(func() { c.childrenMenu(s) })
		}
	case "/fork", "/revert":
		c.task(func() { c.inputMenu(s, name == "/revert") })
	case "/agent", "/model":
		c.task(func() { c.selectionMenu(s, name, arg) })
	case "/runtime":
		invocation := strings.SplitN(arg, " ", 2)
		if len(invocation) != 2 {
			c.report(s, terminal.RenderErrorMsg("/runtime <OperationId> <JSON envelope>"), false)
			return
		}
		c.task(func() {
			if s.conn == nil {
				c.report(s, terminal.RenderErrorMsg("Runtime is not connected"), true)
				return
			}
			raw, err := clientops.Call(s.ctx, s.conn, invocation[0], json.RawMessage(invocation[1]))
			read := strings.HasPrefix(invocation[0], "Get") || strings.HasPrefix(invocation[0], "List") || strings.HasPrefix(invocation[0], "Read") || strings.HasPrefix(invocation[0], "Discover")
			text := resultText(raw, err)
			if read && err != nil {
				text = terminal.RenderErrorMsg(err.Error())
			}
			label := invocation[0]
			if strings.Contains(label, "Retained") {
				label += " — retained snapshot boundaries, not conversation turns"
			}
			text = terminal.RenderSystemMsg(label) + text
			c.report(s, text, !read)
			if errors.Is(err, clientops.ErrInvalidParams) {
				return
			}
			if read {
				return
			}
			c.mu.Lock()
			if c.currentLocked(s) {
				c.globalsDirty = true
				c.kickGlobalsLocked()
				c.refreshReadersLocked()
			}
			c.mu.Unlock()
		})
	default:
		c.report(s, terminal.RenderErrorMsg(fmt.Sprintf("Unknown command %s", name)), false)
	}
}
