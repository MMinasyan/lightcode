package cliclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/terminal"
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
	if !strings.HasSuffix(fresh.req.URL.Path, "/hydration") {
		t.Fatal("stale page did not dirty hydration")
	}
	fresh.respond(h)
	retry := receive(t, transport.requests)
	if retry.req.URL.Query().Get("cursor") != "real-oldest" {
		t.Fatal("stale older request lost or cursor changed")
	}
	c.mu.Lock()
	if r.chain != chain || len(r.items) != 50 || r.hydration.SessionRevision.DurableRevision != "5" {
		t.Fatal("stale page advanced view or spawned second chain")
	}
	c.mu.Unlock()
	retry.respond(protocol.HistoryPage{SessionRevision: revision("5"), Items: inputs(1, 50)})
	barrier(t, chain.done)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(r.items) != 100 {
		t.Fatal("valid disjoint older page rejected")
	}
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
			if err := receive(t, done); err != nil {
				t.Fatal(err)
			}
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
			c.selectRoot(rootB, false)
			fresh := receive(t, transport.requests)
			fresh.respond(hydration(rootB, "1", nil, nil))
			settleTasks(t, c)
			close(blocked.release)
			c.mu.Lock()
			if c.menu != nil {
				t.Fatal("queued menu survived newer selection")
			}
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
			if !strings.Contains(string(old.body), `"workspace":"/workspace"`) || !strings.Contains(string(old.body), `"path":"file.go"`) {
				t.Fatal("file read omitted explicit address")
			}
			c.selectRoot(rootB, false)
			receive(t, transport.requests).respond(hydration(rootB, "1", nil, nil))
			if fail {
				old.reject(400, protocol.Error{Code: "invalid", Message: "stale file error"})
			} else {
				old.respond(protocol.ReadFileResult{CanonicalPath: "/workspace/stale.go", Content: "stale content", Truncated: true})
			}
			settleTasks(t, c)
			if strings.Contains(outputs(c), "stale") {
				t.Fatal("superseded viewer success/error printed")
			}
			c.command(`/runtime ReadWorkspaceFile {"body":{"workspace":"/workspace","path":"current.go"}}`)
			receive(t, transport.requests).respond(protocol.ReadFileResult{CanonicalPath: "/workspace/current.go", Content: "current", Truncated: true})
			settleTasks(t, c)
			text := outputs(c)
			if !strings.Contains(text, "/workspace/current.go") || !strings.Contains(text, `"truncated":true`) {
				t.Fatal("canonical path/truncation lost")
			}
			c.command(`/runtime ReadWorkspaceFile {"body":{"workspace":"/workspace","path":"../denied"}}`)
			receive(t, transport.requests).reject(400, protocol.Error{Code: "invalid", Message: "denied workspace path"})
			settleTasks(t, c)
			if text := outputs(c); !strings.Contains(text, "denied workspace path") || strings.Contains(text, "Outcome unknown") {
				t.Fatal("denied read hidden or treated as unknown mutation")
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
	if len(c.menu.items) != 2 {
		t.Fatal("non-user input became a fork boundary")
	}
	c.mu.Unlock()
	press(c, terminal.KeyMsg{Special: terminal.KeyDown})
	press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
	c.mu.Lock()
	if c.menu != nil || c.forkBoundary.OperationId != nil || c.forkRewind {
		t.Fatal("copied input invented a source restore boundary")
	}
	c.forkBoundary = nil
	c.mu.Unlock()
	c.command("/revert")
	settleTasks(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.menu.items) != 1 || c.menu.items[0].Extra.(protocol.InputItem).OperationId == nil || r != c.root {
		t.Fatal("copied/non-user rewind sibling admitted")
	}
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
	if c.draft.String() != "rejected draft" {
		t.Fatal("rejected draft cleared")
	}
	c.mu.Unlock()
	text := outputs(c)
	if !strings.Contains(text, "server gate rejection") || strings.Contains(text, "Outcome unknown") {
		t.Fatal("known rejection hidden/reclassified")
	}
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
			if c.menu != nil || c.conn == s.conn {
				t.Fatal("old connection child menu published")
			}
			fresh := c.stampLocked()
			c.mu.Unlock()
			c.report(fresh, terminal.RenderSystemMsg("new owner marker"), false)
			frameContaining(t, screen, "new owner marker")
			if strings.Contains(screen.text(), "old owner child error") {
				t.Fatal("old-owner error published")
			}
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
	if !strings.Contains(req.req.URL.Path, rootA) || string(req.body) != `{"agent_type":"planner"}` {
		t.Fatalf("wrong Session Agent edit: %s %s", req.req.URL, req.body)
	}
	req.respond(protocol.Session{SessionId: rootA, AgentType: "planner"})
	next := hydration(rootA, "2", inputs(1, 1), nil)
	next.Session.AgentType = "planner"
	next.ActiveOperation = &protocol.Operation{OperationId: "active", AgentType: "captured-old", Model: "captured/model", RequestKind: "message"}
	receive(t, transport.requests).respond(next)
	settleTasks(t, c)
	c.command("/model provider/next")
	req = receive(t, transport.requests)
	if req.req.URL.Query().Get("agent_type") != "planner" || string(req.body) != `{"model":"provider/next"}` || strings.Contains(req.req.URL.Path, rootA) {
		t.Fatalf("model was not a type-wide future edit: %s %s", req.req.URL, req.body)
	}
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
	if !strings.Contains(text, "Future: planner / provider/next") || !strings.Contains(text, "captured-old / captured/model") {
		t.Fatal("future selection overwrote active capture")
	}
	c.requestExit(nil)
}

func TestCreateUnknownRemainsCapturedAcrossWorkspaceIntent(t *testing.T) {
	c, transport := fixture(t)
	c.mu.Lock()
	s := c.stampLocked()
	c.mu.Unlock()
	c.task(func() { c.create(s) })
	old := receive(t, transport.requests)
	c.selectWorkspace("/workspace/../different")
	list := receive(t, transport.requests)
	if list.req.URL.Query().Get("workspace") != "/different" {
		t.Fatal("Workspace did not use lexical Abs")
	}
	list.respond([]protocol.Session{})
	newCreate := receive(t, transport.requests)
	newCreate.reject(201, protocol.Session{SessionId: rootB, Workspace: "/different", AgentType: "primary"})
	fresh := hydration(rootB, "1", nil, nil)
	fresh.Session.Workspace = "/different"
	receive(t, transport.requests).respond(fresh)
	old.lost()
	settleTasks(t, c)
	c.mu.Lock()
	if !c.unknownCreates["/workspace"] || c.unknownCreates["/different"] || c.rootID != rootB {
		t.Fatal("unknown creation moved to the newly selected Workspace")
	}
	c.mu.Unlock()
	c.selectWorkspace("/workspace")
	receive(t, transport.requests).respond([]protocol.Session{})
	settleTasks(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rootID != "" || !c.unknownCreates["/workspace"] {
		t.Fatal("later composition adopted/resubmitted unknown Create")
	}
}

func TestExplicitResumeCannotPromoteChildOrSubmitBeforeRootHydration(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	c.command("/resume " + childA)
	open := receive(t, transport.requests)
	setDraft(c, "forged child follow up")
	press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
	open.respond([]protocol.Session{{SessionId: childA, ParentSessionId: pointer(rootA)}})
	receive(t, transport.requests).respond([]protocol.Session{{SessionId: childA, ParentSessionId: pointer(rootA), Lifecycle: "archived"}})
	settleTasks(t, c)
	c.mu.Lock()
	if c.rootID != "" || c.root != nil || len(c.attempts) != 0 {
		t.Fatal("child became root work selection")
	}
	c.mu.Unlock()
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, route := range transport.calls {
		if strings.Contains(route, "/reopen") || strings.Contains(route, "/submit") {
			t.Fatalf("child root bypass %s", route)
		}
	}
}

func TestResumeUnknownRootVerifiesHeaderThenReopensAndHydrates(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	c.command("/resume " + childA)
	receive(t, transport.requests).respond([]protocol.Session{})
	receive(t, transport.requests).respond([]protocol.Session{{SessionId: childA, Lifecycle: "archived", Workspace: "/workspace"}})
	reopen := receive(t, transport.requests)
	if !strings.HasSuffix(reopen.req.URL.Path, "/reopen") {
		t.Fatal("archived work selection did not reopen first")
	}
	reopen.respond(protocol.Session{SessionId: childA, Lifecycle: "open"})
	fresh := receive(t, transport.requests)
	if !strings.HasSuffix(fresh.req.URL.Path, "/hydration") {
		t.Fatal("reopen did not precede hydration")
	}
	fresh.respond(hydration(childA, "1", nil, nil))
	settleTasks(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rootID != childA || c.root.hydration == nil {
		t.Fatal("valid root resume not adopted")
	}
}

func TestRuntimeCurrentRetainedSnapshotsAndPartialRestoreRemainReachable(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	c.command(`/runtime GetSessionCodeSnapshots {"session_id":"` + rootA + `"}`)
	current := receive(t, transport.requests)
	if !strings.Contains(current.req.URL.Path, rootA) {
		t.Fatal("current snapshots did not address Session")
	}
	current.respond([]protocol.CodeSnapshotGroup{{OperationId: "owned-operation", GroupItemId: "group", Files: []protocol.SnapshotFile{}}})
	settleTasks(t, c)
	if !strings.Contains(outputs(c), "owned-operation") {
		t.Fatal("Operation snapshot boundary dropped")
	}
	c.command(`/runtime GetRetainedCodeSnapshots {"query":{"workspace":"/workspace","session_id":"1234abcd"}}`)
	retained := receive(t, transport.requests)
	if retained.req.URL.Query().Get("workspace") != "/workspace" || retained.req.URL.Query().Get("session_id") != "1234abcd" {
		t.Fatal("retained snapshot address changed")
	}
	retained.respond(protocol.RetainedTurns{Turns: []protocol.RetainedTurn{{Turn: 7, Files: []protocol.SnapshotFile{}}}})
	settleTasks(t, c)
	if !strings.Contains(outputs(c), "retained snapshot boundaries") {
		t.Fatal("retained boundary labelled as conversation turn")
	}
	c.command(`/runtime RevertRetainedCode {"body":{"workspace":"/workspace","session_id":"1234abcd","after_turn":7}}`)
	restore := receive(t, transport.requests)
	if !strings.Contains(string(restore.body), `"after_turn":7`) {
		t.Fatal("retained restore boundary rewritten")
	}
	restore.reject(409, protocol.CodeRevertResult{Restored: []string{"one.go"}, Skipped: []protocol.SkippedFile{{Path: "two.go", Reason: "changed"}}, Error: &protocol.Error{Code: "conflict", Message: "partial restore"}})
	receive(t, transport.requests).respond(hydration(rootA, "2", inputs(1, 1), nil))
	settleTasks(t, c)
	text := outputs(c)
	if !strings.Contains(text, "one.go") || !strings.Contains(text, "two.go") || !strings.Contains(text, "partial restore") {
		t.Fatal("/runtime dropped complete partial restore")
	}
}
