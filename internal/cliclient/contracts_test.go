package cliclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/terminal"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

type gatedScreen struct {
	screen           *screenWriter
	entered, release chan struct{}
	once             sync.Once
}

func (w *gatedScreen) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.screen.Write(data)
}

func TestStaleOlderPageRetriesThroughTheSameReader(t *testing.T) {
	c, transport := fixture(t)
	r, done := c.startReader()
	h := hydration(rootA, "5", inputs(51, 100), pointer("real-oldest"))
	receive(t, transport.requests).respond(h)
	barrier(t, done)
	c.mu.Lock()
	r.pendingOlder = true
	c.kickReaderLocked(r)
	chain := r.chain
	c.mu.Unlock()
	receive(t, transport.requests).respond(protocol.HistoryPage{SessionRevision: revision("4"), Items: inputs(1, 50)})
	fresh := receive(t, transport.requests)
	testassert.Require(t, strings.HasSuffix(fresh.req.URL.Path, "/hydration"), "stale page did not dirty hydration")
	fresh.respond(h)
	retry := receive(t, transport.requests)
	testassert.Require(t, retry.req.URL.Query().Get("cursor") == "real-oldest", "stale older request lost or cursor changed")
	c.mu.Lock()
	testassert.Require(t, r.chain == chain && len(r.items) == 50 && r.hydration.SessionRevision.DurableRevision == "5", "stale page advanced view or spawned second chain")
	c.mu.Unlock()
	retry.respond(protocol.HistoryPage{SessionRevision: revision("5"), Items: inputs(1, 50)})
	barrier(t, chain.done)
	c.mu.Lock()
	defer c.mu.Unlock()
	testassert.Require(t, len(r.items) == 100, "valid disjoint older page rejected")
}

func TestMalformedGlobalsAndLostConversationContinuityRepairActualConnection(t *testing.T) {
	for _, invalid := range []string{"configuration", "continuity"} {
		t.Run(invalid, func(t *testing.T) {
			var mu sync.Mutex
			configurationReads, hydrations := 0, 0
			historyEntered := make(chan struct{}, 1)
			live := liveFixture(t, false, func(w http.ResponseWriter, r *http.Request) bool {
				w.Header().Set("Content-Type", "application/json")
				mu.Lock()
				defer mu.Unlock()
				if invalid == "configuration" && r.URL.Path == "/v1/configuration" {
					configurationReads++
					if configurationReads == 1 {
						_ = json.NewEncoder(w).Encode(configuration("1.5"))
						return true
					}
				}
				if invalid == "continuity" && strings.HasSuffix(r.URL.Path, "/hydration") {
					hydrations++
					items := inputs(1, 2)
					var cursor *string
					if hydrations >= 2 {
						items = inputs(4, 5)
					}
					if hydrations == 2 {
						cursor = pointer("gap")
					}
					_ = json.NewEncoder(w).Encode(hydration(rootA, fmt.Sprint(hydrations), items, cursor))
					return true
				}
				if invalid == "continuity" && strings.HasSuffix(r.URL.Path, "/history") {
					historyEntered <- struct{}{}
					_ = json.NewEncoder(w).Encode(protocol.HistoryPage{SessionRevision: revision("2"), Items: []protocol.ConversationItem{}})
					return true
				}
				return false
			})
			input, write := pipeInput(t)
			c := New(Options{Startup: live.opts, Workspace: "/workspace"})
			screen := newScreen(80)
			done := runClient(c, input, screen)
			barrier(t, live.streams)
			if invalid == "continuity" {
				frameContaining(t, screen, "input 2")
				c.mu.Lock()
				reader := c.root
				c.mu.Unlock()
				c.refreshReader(reader)
				barrier(t, historyEntered)
			}
			barrier(t, live.streams)
			frameContaining(t, screen, "Connection repaired")
			if invalid == "continuity" && !strings.Contains(screen.text(), "input 5") {
				frameContaining(t, screen, "input 5")
			}
			write.Close()
			testassert.NoError(t, receive(t, done))
			assertNoImplicitControls(t, live)
		})
	}
}

func TestQueuedTaskMenuAndErrorCannotPublishAfterNavigation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			c, transport := fixture(t)
			settledReader(t, c, transport)
			screen := newScreen(80)
			blocked := &gatedScreen{screen: screen, entered: make(chan struct{}), release: make(chan struct{})}
			// Park the sole writer before the asynchronous command completion.
			go c.writer(blocked)
			c.mu.Lock()
			c.notifyLocked()
			s := c.stampLocked()
			c.mu.Unlock()
			barrier(t, blocked.entered)
			c.task(func() { c.workspaceMenu(s) })
			old := receive(t, transport.requests)
			if fail {
				old.reject(404, protocol.Error{Code: "not_found", Message: "stale workspace error"})
			} else {
				old.respond([]protocol.Workspace{{DisplayName: "stale workspace menu", Root: "/old"}})
			}
			settleTasks(t, c)
			c.selectRoot(rootB)
			receive(t, transport.requests).respond(hydration(rootB, "1", nil, nil))
			fresh := receive(t, transport.requests)
			fresh.respond(hydration(rootB, "1", nil, nil))
			settleTasks(t, c)
			close(blocked.release)
			c.mu.Lock()
			testassert.Require(t, c.menu == nil, "queued menu survived newer selection")
			current := c.stampLocked()
			c.mu.Unlock()
			c.report(current, terminal.RenderSystemMsg("current command marker"), false)
			frameContaining(t, screen, "current command marker")
			if strings.Contains(screen.text(), "stale workspace") || !strings.Contains(screen.text(), rootB) {
				t.Fatalf("queued stale task/menu published: %s", screen.text())
			}
			c.requestExit(nil)
		})
	}
}

func TestRuntimeFileSuccessAndErrorAreStampedAndUseGeneratedAddress(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			c, transport := fixture(t)
			settledReader(t, c, transport)
			c.command(`/runtime ReadWorkspaceFile {"body":{"workspace":"/workspace","path":"file.go"}}`)
			old := receive(t, transport.requests)
			testassert.Require(t, strings.Contains(string(old.body), `"workspace":"/workspace"`) && strings.Contains(string(old.body), `"path":"file.go"`), "file read omitted explicit address")
			c.selectRoot(rootB)
			receive(t, transport.requests).respond(hydration(rootB, "1", nil, nil))
			receive(t, transport.requests).respond(hydration(rootB, "1", nil, nil))
			if fail {
				old.reject(400, protocol.Error{Code: "invalid", Message: "stale file error"})
			} else {
				old.respond(protocol.ReadFileResult{CanonicalPath: "/workspace/stale.go", Content: "stale content", Truncated: true})
			}
			settleTasks(t, c)
			testassert.Require(t, !strings.Contains(outputs(c), "stale"), "superseded viewer success/error printed")
			c.command(`/runtime ReadWorkspaceFile {"body":{"workspace":"/workspace","path":"current.go"}}`)
			receive(t, transport.requests).respond(protocol.ReadFileResult{CanonicalPath: "/workspace/current.go", Content: "current", Truncated: true})
			settleTasks(t, c)
			text := outputs(c)
			testassert.Require(t, strings.Contains(text, "/workspace/current.go") && strings.Contains(text, `"truncated":true`), "canonical path/truncation lost")
			c.command(`/runtime ReadWorkspaceFile {"body":{"workspace":"/workspace","path":"../denied"}}`)
			receive(t, transport.requests).reject(400, protocol.Error{Code: "invalid", Message: "denied workspace path"})
			settleTasks(t, c)
			{
				text := outputs(c)
				testassert.Require(t, strings.Contains(text, "denied workspace path") && !strings.Contains(text, "Outcome unknown"), "denied read hidden or treated as unknown mutation")
			}
		})
	}
}

func TestForkBoundaryMenusUserOwnedCopiedRuntimeAndPlugin(t *testing.T) {
	c, transport := fixture(t)
	r, done := c.startReader()
	var items []protocol.ConversationItem
	for i, origin := range []protocol.InputOrigin{"user", "user", "runtime", "plugin"} {
		input := protocol.InputItem{Kind: "input", ItemId: fmt.Sprint(i), Origin: origin, Content: textParts(fmt.Sprint(origin, i))}
		if i != 1 {
			input.OperationId = pointer(fmt.Sprintf("owned-%d", i))
		}
		var item protocol.ConversationItem
		_ = item.FromInputItem(input)
		items = append(items, item)
	}
	receive(t, transport.requests).respond(hydration(rootA, "1", items, nil))
	barrier(t, done)
	c.command("/fork")
	settleTasks(t, c)
	c.mu.Lock()
	testassert.Require(t, len(c.menu.items) == 2, "non-user input became a fork boundary")
	c.mu.Unlock()
	press(c, terminal.KeyMsg{Special: terminal.KeyDown})
	press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
	c.mu.Lock()
	testassert.Require(t, c.menu == nil && c.forkBoundary.OperationId == nil && !c.forkRewind, "copied input invented a source restore boundary")
	c.forkBoundary = nil
	c.mu.Unlock()
	c.command("/revert")
	settleTasks(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	testassert.Require(t, len(c.menu.items) == 1 && c.menu.items[0].Extra.(protocol.InputItem).OperationId != nil && r == c.root, "copied/non-user rewind sibling admitted")
}

func TestRejectedSubmissionKeepsDraftAndDoesNotBecomeEmptySuccess(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	setDraft(c, "rejected draft")
	press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
	receive(t, transport.requests).reject(409, protocol.Error{Code: "conflict", Message: "server gate rejection"})
	receive(t, transport.requests).respond(hydration(rootA, "2", inputs(1, 1), nil))
	settleTasks(t, c)
	c.mu.Lock()
	testassert.Require(t, c.draft.String() == "rejected draft", "rejected draft cleared")
	c.mu.Unlock()
	text := outputs(c)
	testassert.Require(t, strings.Contains(text, "server gate rejection") && !strings.Contains(text, "Outcome unknown"), "known rejection hidden/reclassified")
}

func TestChildListOldConnectionSuccessAndErrorAreSuperseded(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			c, transport := fixture(t)
			settledReader(t, c, transport)
			live := liveFixture(t, false, nil)
			c.mu.Lock()
			s := c.stampLocked()
			c.opts.Startup = live.opts
			c.mu.Unlock()
			oldDone := make(chan struct{})
			c.task(func() { c.childrenMenu(s); close(oldDone) })
			parked := receive(t, transport.requests)
			screen := newScreen(80)
			go c.writer(screen)
			c.task(func() { c.connect(true, s.conn) })
			frameContaining(t, screen, "canonical clipboard")
			if fail {
				parked.reject(404, protocol.Error{Code: "not_found", Message: "old owner child error"})
			} else {
				parked.respond([]protocol.Session{{SessionId: childA, ParentSessionId: pointer(rootA)}})
			}
			barrier(t, oldDone)
			c.mu.Lock()
			testassert.Require(t, c.menu == nil && c.conn != s.conn, "old connection child menu published")
			fresh := c.stampLocked()
			c.mu.Unlock()
			c.report(fresh, terminal.RenderSystemMsg("new owner marker"), false)
			frameContaining(t, screen, "new owner marker")
			testassert.Require(t, !strings.Contains(screen.text(), "old owner child error"), "old-owner error published")
			c.mu.Lock()
			c.closed = true
			c.cancel()
			c.connCancel()
			c.mu.Unlock()
			c.requestExit(nil)
			settleTasks(t, c)
		})
	}
}

func TestFutureAgentAndTypeWideModelUseGeneratedScope(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	c.command("/agent planner")
	req := receive(t, transport.requests)
	testassert.Require(t, strings.Contains(req.req.URL.Path, rootA) && string(req.body) == `{"agent_type":"planner"}`, "wrong Session Agent edit: %s %s", req.req.URL, req.body)
	req.respond(protocol.Session{SessionId: rootA, AgentType: "planner"})
	next := hydration(rootA, "2", inputs(1, 1), nil)
	next.Session.AgentType = "planner"
	next.ActiveOperation = &protocol.Operation{OperationId: "active", AgentType: "captured-old", Model: "captured/model", RequestKind: "message"}
	receive(t, transport.requests).respond(next)
	settleTasks(t, c)
	c.command("/model provider/next")
	req = receive(t, transport.requests)
	testassert.Require(t, req.req.URL.Query().Get("agent_type") == "planner" && string(req.body) == `{"model":"provider/next"}` && !strings.Contains(req.req.URL.Path, rootA), "model was not a type-wide future edit: %s %s", req.req.URL, req.body)
	req.respond(protocol.RevisionResponse{ConfigurationRevision: configuration("2").ConfigurationRevision})
	next.SessionRevision = revision("3")
	next.SelectedModel = pointer("provider/next")
	receive(t, transport.requests).respond(next)
	settleTasks(t, c)
	screen := newScreen(100)
	go c.writer(screen)
	c.mu.Lock()
	c.width = 100
	c.notifyLocked()
	c.mu.Unlock()
	frameContaining(t, screen, "captured-old")
	text := strings.ReplaceAll(screen.text(), "\n", "")
	testassert.Require(t, strings.Contains(text, "Future: planner / provider/next") && strings.Contains(text, "captured-old / captured/model"), "future selection overwrote active capture")
	c.requestExit(nil)
}

// One failed Create is reported only for its current captured intent; a fresh
// explicit Workspace-selection intent composes once for the new address, and
// an old-Workspace completion neither adopts nor publishes a stale error.
func TestCreateFailureCapturedAddressAndExplicitWorkspaceIntent(t *testing.T) {
	c, transport := fixture(t)
	c.mu.Lock()
	s := c.stampLocked()
	c.mu.Unlock()
	c.task(func() { c.create(s) })
	old := receive(t, transport.requests)
	var pending protocol.CreateSessionRequest
	{
		err := json.Unmarshal(old.body, &pending)
		testassert.Require(t, err == nil && pending.Workspace == "/workspace", "pending create captured %q (%v)", pending.Workspace, err)
	}
	c.selectWorkspace("/workspace/../different")
	list := receive(t, transport.requests)
	testassert.Require(t, list.req.URL.Query().Get("workspace") == "/different", "Workspace did not use lexical Abs")
	// A child-only header list leaves no eligible root: the next request is
	// still the Create for the captured Workspace.
	list.respond([]protocol.Session{{SessionId: childA, ParentSessionId: pointer(rootB)}})
	newCreate := receive(t, transport.requests)
	var sent protocol.CreateSessionRequest
	{
		err := json.Unmarshal(newCreate.body, &sent)
		testassert.Require(t, err == nil && sent.Workspace == "/different", "explicit Workspace composition create = %s (%v)", newCreate.body, err)
	}
	newCreate.reject(201, protocol.Session{SessionId: rootB, Workspace: "/different", AgentType: "primary"})
	fresh := hydration(rootB, "1", nil, nil)
	fresh.Session.Workspace = "/different"
	receive(t, transport.requests).respond(fresh)
	old.lost()
	settleTasks(t, c)
	c.mu.Lock()
	adopted := c.rootID == rootB
	c.mu.Unlock()
	testassert.Require(t, adopted, "an old-Workspace completion or a failed intent blocked the new explicit composition")
	testassert.Require(t, !strings.Contains(outputs(c), "Outcome unknown"), "an old-Workspace completion published a stale local error")
}

func TestExplicitResumeCannotPromoteChildOrSubmitToItBeforeAdoption(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	// A live writer admits the explicit follow-up typed while the destination is
	// resolved.
	go c.writer(io.Discard)
	c.mu.Lock()
	source, sourceID := c.root, c.rootID
	c.mu.Unlock()
	c.command("/resume " + childA)
	destination := receive(t, transport.requests)
	testassert.Require(t, strings.HasSuffix(destination.req.URL.Path, "/hydration"), "named resume did not read its destination globally")
	// The retained source keeps accepting explicit work while the destination is
	// resolved, and nothing is addressed to the unadopted child.
	setDraft(c, "forged child follow up")
	press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
	submit := receive(t, transport.requests)
	testassert.Require(t, strings.Contains(submit.req.URL.Path, sourceID), "follow-up addressed outside the accepted source: %s", submit.req.URL.Path)
	submit.respond(protocol.SubmitResult{Disposition: "admitted", Operation: &protocol.Operation{OperationId: "forged"}})
	// The destination read's own Session record is the eligibility authority.
	child := hydration(childA, "1", nil, nil)
	child.Session.ParentSessionId = pointer(rootA)
	destination.respond(child)
	settleAnswering(t, c, transport)
	c.mu.Lock()
	testassert.Require(t, c.rootID == sourceID && c.root == source && c.childID == "", "child resume promoted the child or erased the source")
	c.mu.Unlock()
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, route := range transport.calls {
		testassert.Require(t, !strings.Contains(route, "/reopen") && !strings.Contains(route, childA+"/submit"), "child root bypass %s", route)
	}
}

func TestNamedResumeAdoptsCrossWorkspaceRootAndReopensArchived(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	// An open root elsewhere resolves through the global read alone and adopts
	// its immutable destination Workspace.
	c.command("/resume " + rootB)
	destination := hydration(rootB, "1", nil, nil)
	destination.Session.Workspace = "/different"
	receive(t, transport.requests).respond(destination)
	receive(t, transport.requests).respond(destination)
	settleTasks(t, c)
	c.mu.Lock()
	testassert.Require(t, c.rootID == rootB && c.workspace == "/different" && c.root != nil, "cross-Workspace root resume was refused or not adopted: root=%q workspace=%q", c.rootID, c.workspace)
	c.mu.Unlock()
	// An archived root is reopened first and its fresh hydration is then adopted.
	c.command("/resume " + childA)
	archived := hydration(childA, "1", nil, nil)
	archived.Session.Lifecycle = protocol.Archived
	receive(t, transport.requests).respond(archived)
	reopen := receive(t, transport.requests)
	testassert.Require(t, strings.HasSuffix(reopen.req.URL.Path, "/reopen"), "archived destination did not reopen before work")
	reopen.respond(protocol.Session{SessionId: childA, Lifecycle: protocol.Open})
	// The adopted reader owns the one fresh hydration after the reopen.
	fresh := receive(t, transport.requests)
	testassert.Require(t, strings.HasSuffix(fresh.req.URL.Path, "/hydration"), "reopen did not precede the fresh destination read")
	fresh.respond(hydration(childA, "2", nil, nil))
	settleTasks(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	testassert.Require(t, c.rootID == childA && c.root != nil && c.root.hydration != nil && c.root.hydration.SessionRevision.DurableRevision == "2", "reopened root was not adopted with its fresh read")
}

func TestBareResumeSelectsNewestOpenRootAndKeepsSelectionWithoutAny(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	// The newest open root wins; archived and child headers never appear.
	c.command("/resume")
	receive(t, transport.requests).respond([]protocol.Session{
		{SessionId: childA, ParentSessionId: pointer(rootA), Workspace: "/workspace", LastActivity: time.Unix(9, 0)},
		{SessionId: rootA, Workspace: "/workspace", LastActivity: time.Unix(1, 0)},
		{SessionId: rootB, Workspace: "/workspace", LastActivity: time.Unix(5, 0)},
	})
	receive(t, transport.requests).respond(hydration(rootB, "2", nil, nil))
	settleTasks(t, c)
	c.mu.Lock()
	testassert.Require(t, c.rootID == rootB && c.menu == nil, "bare resume did not select the newest open root: root=%q menu=%v", c.rootID, c.menu)
	c.mu.Unlock()
	// No available root reports the retained no-session outcome and keeps the
	// current selection; no Session is created.
	c.command("/resume")
	receive(t, transport.requests).respond([]protocol.Session{})
	// The retained source stays live: its own refresh follows the failed intent.
	settleAnsweringRequested(t, c, transport)
	c.mu.Lock()
	testassert.Equal(t, c.rootID, rootB)
	c.mu.Unlock()
	testassert.Require(t, strings.Contains(outputs(c), "no active sessions"), "no-session outcome was not reported")
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, route := range transport.calls {
		testassert.Require(t, !strings.Contains(route, "/create") && !strings.Contains(route, "/archived"), "bare resume created a Session or consulted archived Sessions: %s", route)
	}
}

// Failed root navigation over the actual commands and failure replies keeps
// the accepted root, inspector and Workspace live with their resume reads and
// reports the outcome; a superseded completion neither adopts nor prints, and
// recall survives the failed intent.
func TestFailedRootNavigationKeepsTheAcceptedSelection(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		fail    func(request)
		note    string
	}{
		{"known rejected New", "/new", func(req request) { req.reject(409, protocol.Error{Code: "conflict", Message: "create rejected"}) }, "create rejected"},
		{"lost New", "/new", func(req request) { req.lost() }, "Outcome unknown"},
		{"known rejected named resume", "/resume missing-root", func(req request) { req.reject(404, protocol.Error{Code: "not_found", Message: "no such session"}) }, "no such session"},
		{"lost named resume", "/resume missing-root", func(req request) { req.lost() }, "unexpected EOF"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, transport := fixture(t)
			settledReader(t, c, transport)
			c.mu.Lock()
			source, sourceID, workspace := c.root, c.rootID, c.workspace
			c.mu.Unlock()
			c.command(test.command)
			test.fail(receive(t, transport.requests))
			// The retained source readers resume under the current selection.
			resumed := receive(t, transport.requests)
			testassert.Require(t, strings.Contains(resumed.req.URL.Path, rootA+"/hydration"), "failed navigation did not resume the source read: %s", resumed.req.URL)
			resumed.respond(hydration(rootA, "2", inputs(1, 1), nil))
			settleTasks(t, c)
			c.mu.Lock()
			kept := c.root == source && c.rootID == sourceID && c.workspace == workspace && c.child == nil && c.childID == "" && c.menu == nil
			c.mu.Unlock()
			testassert.Require(t, kept, "failed navigation changed the accepted selection")
			testassert.Require(t, strings.Contains(outputs(c), test.note), "failed navigation hid its outcome")
			press(c, terminal.KeyMsg{Rune: 'z'})
			press(c, terminal.KeyMsg{Special: terminal.KeyUp})
			if c.draft.String() != "input 1" {
				t.Fatalf("failed navigation cleared recall: %q", c.draft.String())
			}
		})
	}

	t.Run("a superseded completion neither adopts nor prints", func(t *testing.T) {
		c, transport := fixture(t)
		settledReader(t, c, transport)
		c.command("/new")
		parkedCreate := receive(t, transport.requests)
		c.command("/resume also-missing")
		parkedError := receive(t, transport.requests)
		c.command("/resume " + rootB)
		destination := receive(t, transport.requests)
		destination.respond(hydration(rootB, "1", nil, nil))
		receive(t, transport.requests).respond(hydration(rootB, "2", nil, nil))
		parkedCreate.reject(201, protocol.Session{SessionId: childA, Workspace: "/workspace", AgentType: "primary"})
		parkedError.reject(404, protocol.Error{Code: "not_found", Message: "stale destination error"})
		settleTasks(t, c)
		c.mu.Lock()
		defer c.mu.Unlock()
		testassert.Require(t, c.rootID == rootB, "a superseded completion adopted over newer navigation")
		testassert.Require(t, !strings.Contains(outputs(c), "stale destination error"), "superseded destination error printed")
	})
}

func TestSourceAgentMenuCannotPublishAfterDestinationAdoption(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	// The menu's own configuration read parks so its completion can be ordered
	// after the destination adoption.
	transport.globals = true
	// The destination intent starts, then the source's own /agent menu is begun
	// while the destination is still being resolved.
	c.command("/resume " + rootB)
	destination := receive(t, transport.requests)
	c.command("/agent")
	menuRead := receive(t, transport.requests)
	testassert.Require(t, strings.HasSuffix(menuRead.req.URL.Path, "/configuration"), "agent menu did not read the configuration: %s", menuRead.req.URL.Path)
	// The adopted reader's own read proves the destination published, then the
	// menu task completes afterwards.
	destination.respond(hydration(rootB, "2", nil, nil))
	adopted := receive(t, transport.requests)
	menuRead.respond(configuration("1"))
	adopted.respond(hydration(rootB, "2", nil, nil))
	settleTasks(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	testassert.Require(t, c.menu == nil && c.rootID == rootB && c.root != nil, "source agent menu published after destination adoption: %+v; destination was not adopted: root=%q", c.menu, c.rootID)
}

func TestSourceForkCompletionCannotReplaceAdoptedDestination(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	c.command("/resume " + rootB)
	destination := receive(t, transport.requests)
	// The Fork attempt is begun against the retained source during resolution.
	c.mu.Lock()
	c.forkBoundary = &protocol.InputItem{ItemId: "boundary"}
	a, err := c.formAttemptLocked("ForkSession", "fork-op")
	c.mu.Unlock()
	testassert.NoError(t, err)
	c.task(func() { c.runAttempt(a.id) })
	fork := receive(t, transport.requests)
	testassert.Require(t, strings.HasSuffix(fork.req.URL.Path, "/fork"), "fork attempt did not fork: %s", fork.req.URL.Path)
	// The adopted reader's own read proves the destination published, then the
	// source Fork completes afterwards.
	destination.respond(hydration(rootB, "2", nil, nil))
	adopted := receive(t, transport.requests)
	fork.respond(protocol.ForkResult{Session: protocol.Session{SessionId: childA}, Operation: protocol.Operation{OperationId: a.id}})
	adopted.respond(hydration(rootB, "2", nil, nil))
	settleTasks(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	testassert.Require(t, c.rootID == rootB && c.root != nil, "source Fork completion replaced the adopted destination: root=%q", c.rootID)
	// The attempt's own outcome and identity survive.
	settled := c.attempts["fork-op"]
	testassert.Require(t, settled != nil && settled.outcome == "accepted" && settled.session == rootA, "fork outcome/identity lost: %+v", settled)
}

func TestFailedResumeLeavesTheSourceLiveUnderAHeldHydration(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	// A source refresh is in flight, and a further dirty hint lands while the
	// destination is resolved.
	c.mu.Lock()
	c.root.dirty = true
	c.kickReaderLocked(c.root)
	c.mu.Unlock()
	held := receive(t, transport.requests)
	c.command("/resume missing-root")
	destination := receive(t, transport.requests)
	c.mu.Lock()
	c.refreshReadersLocked()
	c.mu.Unlock()
	destination.reject(404, protocol.Error{Code: "not_found", Message: "no such session"})
	held.respond(hydration(rootA, "2", inputs(1, 1), nil))
	fresh := receive(t, transport.requests)
	testassert.Require(t, strings.Contains(fresh.req.URL.Path, rootA) && strings.HasSuffix(fresh.req.URL.Path, "/hydration"), "the retained source did not resume reading: %s", fresh.req.URL.Path)
	fresh.respond(hydration(rootA, "3", inputs(1, 1), nil))
	settleTasks(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	testassert.Require(t, c.rootID == rootA && c.root != nil && c.root.hydration != nil && c.root.hydration.SessionRevision.DurableRevision == "3", "the retained source was left stale after a failed resume")
}

func TestRuntimeCurrentSnapshotsAndPartialRestoreRemainReachable(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	c.command(`/runtime GetSessionCodeSnapshots {"session_id":"` + rootA + `"}`)
	current := receive(t, transport.requests)
	testassert.Require(t, strings.Contains(current.req.URL.Path, rootA), "current snapshots did not address Session")
	current.respond([]protocol.CodeSnapshotGroup{{OperationId: "owned-operation", GroupItemId: "group", Files: []protocol.SnapshotFile{}}})
	settleTasks(t, c)
	testassert.Require(t, strings.Contains(outputs(c), "owned-operation"), "Operation snapshot boundary dropped")
	c.command(`/runtime RevertSessionCode {"session_id":"` + rootA + `","body":{"boundary_operation_id":"owned-operation"}}`)
	restore := receive(t, transport.requests)
	testassert.Require(t, strings.Contains(string(restore.body), `"boundary_operation_id":"owned-operation"`), "canonical restore boundary rewritten")
	restore.reject(409, protocol.CodeRevertResult{Restored: []string{"one.go"}, Skipped: []protocol.SkippedFile{{Path: "two.go", Reason: "changed"}}, Error: &protocol.Error{Code: "conflict", Message: "partial restore"}})
	receive(t, transport.requests).respond(hydration(rootA, "2", inputs(1, 1), nil))
	settleTasks(t, c)
	text := outputs(c)
	testassert.Require(t, strings.Contains(text, "one.go") && strings.Contains(text, "two.go") && strings.Contains(text, "partial restore"), "/runtime dropped complete partial restore")
}
