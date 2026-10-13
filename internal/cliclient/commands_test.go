package cliclient

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/terminal"
	"github.com/MMinasyan/lightcode/protocol"
)

func settledReader(t *testing.T, c *CLI, transport *scriptedTransport) *sessionReader {
	t.Helper()
	r, done := c.startReader()
	receive(t, transport.requests).respond(hydration(rootA, "1", inputs(1, 1), nil))
	barrier(t, done)
	return r
}
func settleTasks(t *testing.T, c *CLI) {
	t.Helper()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	barrier(t, done)
}
func outputs(c *CLI) string {
	var b strings.Builder
	for len(c.outputs) > 0 {
		b.WriteString((<-c.outputs).text)
	}
	return b.String()
}
func press(c *CLI, key terminal.KeyMsg) {
	id := ""
	if key.Special == terminal.KeyEnter {
		id = operationID()
	}
	c.mu.Lock()
	action := c.keyLocked(key, id)
	c.mu.Unlock()
	if action != nil {
		action()
	}
}
func setDraft(c *CLI, text string) { c.mu.Lock(); c.draft.Set(text); c.draftRevision++; c.mu.Unlock() }
func attemptBody(t *testing.T, r request) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestUnknownAttemptIdentityDraftRefreshRetryAndAbandon(t *testing.T) {
	for _, operation := range []string{"SubmitSession", "CompactSession", "ForkSession"} {
		t.Run(operation, func(t *testing.T) {
			c, transport := fixture(t)
			r := settledReader(t, c, transport)
			setDraft(c, "immutable replacement")
			minted := operationID()
			c.mu.Lock()
			if operation == "ForkSession" {
				input, _ := r.items[0].AsInputItem()
				c.forkBoundary = &input
			}
			c.queued = true
			beforeRevision := c.draftRevision
			a, err := c.formAttemptLocked(operation, minted)
			c.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if a.draftRevision != beforeRevision {
				t.Fatal("forming a fork edited the draft")
			}
			c.task(func() { c.runAttempt(a.id) })
			first := receive(t, transport.requests)
			body := attemptBody(t, first)
			var id string
			_ = json.Unmarshal(body["operation_id"], &id)
			if id != a.id || len(id) != 32 || strings.ToLower(id) != id {
				t.Fatal("caller ID missing")
			}
			first.lost()
			fresh := receive(t, transport.requests)
			fresh.respond(hydration(rootA, "2", inputs(1, 1), nil))
			settleTasks(t, c)
			c.mu.Lock()
			if a.outcome != "unknown" || c.draft.String() != "immutable replacement" || c.queued {
				t.Fatal("unknown lost attempt/draft or queue mode")
			}
			c.mu.Unlock()
			if !strings.Contains(outputs(c), "Outcome unknown") {
				t.Fatal("unknown outcome not displayed")
			}
			setDraft(c, "newer edit")
			c.retryAttempt(a.id)
			retry := receive(t, transport.requests)
			if !reflect.DeepEqual(attemptBody(t, retry), body) {
				t.Fatal("retry changed immutable payload")
			}
			if operation == "ForkSession" {
				retry.respond(protocol.ForkResult{Session: protocol.Session{SessionId: rootB}, Operation: protocol.Operation{OperationId: id}})
			} else if operation == "CompactSession" {
				retry.respond(protocol.Operation{OperationId: id})
			} else {
				retry.respond(protocol.SubmitResult{Disposition: "queued"})
			}
			refresh := receive(t, transport.requests)
			selected := rootA
			if operation == "ForkSession" {
				selected = rootB
			}
			refresh.respond(hydration(selected, "3", inputs(1, 1), nil))
			settleTasks(t, c)
			c.mu.Lock()
			if c.draft.String() != "newer edit" || a.outcome != "accepted" {
				t.Fatal("accepted old attempt cleared a newer edit")
			}
			c.mu.Unlock()
			if !strings.Contains(outputs(c), "buffered input can be duplicated") {
				t.Fatal("retry warning missing")
			}
			// Unknown is an explicit /session item, addressed to this root only.
			setDraft(c, "abandon draft")
			press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
			unknown := receive(t, transport.requests)
			unknownBody := attemptBody(t, unknown)
			_ = json.Unmarshal(unknownBody["operation_id"], &id)
			unknown.lost()
			c.mu.Lock()
			s := c.stampLocked()
			c.mu.Unlock()
			receive(t, transport.requests).respond(hydration(s.root, "4", inputs(1, 1), nil))
			settleTasks(t, c)
			c.command("/session")
			list := receive(t, transport.requests)
			list.respond([]protocol.Session{{SessionId: s.root, AgentType: "primary"}})
			settleTasks(t, c)
			press(c, terminal.KeyMsg{Special: terminal.KeyDown})
			press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
			press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
			press(c, terminal.KeyMsg{Special: terminal.KeyDown})
			press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
			c.mu.Lock()
			_, exists := c.attempts[id]
			c.mu.Unlock()
			if exists {
				t.Fatal("explicit abandon retained attempt")
			}
		})
	}
}

func TestUnknownAcceptanceByPendingIDNotAbsenceOrContent(t *testing.T) {
	c, transport := fixture(t)
	r := settledReader(t, c, transport)
	setDraft(c, "draft")
	press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
	req := receive(t, transport.requests)
	body := attemptBody(t, req)
	var id string
	_ = json.Unmarshal(body["operation_id"], &id)
	req.lost()
	fresh := hydration(rootA, "2", inputs(1, 1), nil)
	fresh.Pending.Steering = []protocol.PendingInput{{OperationId: "different-id", Content: textParts("draft")}}
	receive(t, transport.requests).respond(fresh)
	settleTasks(t, c)
	c.mu.Lock()
	if c.attempts[id].outcome != "unknown" || c.draft.String() != "draft" {
		t.Fatal("content matched or absence treated as rejection")
	}
	c.mu.Unlock()
	done := c.refreshReader(r)
	fresh.SessionRevision = revision("3")
	fresh.Pending.Steering[0].OperationId = id
	receive(t, transport.requests).respond(fresh)
	barrier(t, done)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempts[id].outcome != "accepted" || c.draft.String() != "" {
		t.Fatal("pending caller ID did not reconcile acceptance")
	}
}

func TestForkRestoreOrderedPartialRetainedAcrossRetryAndNavigation(t *testing.T) {
	for _, restoreKind := range []string{"success", "partial", "rejected", "unknown"} {
		t.Run(restoreKind, func(t *testing.T) {
			c, transport := fixture(t)
			r := settledReader(t, c, transport)
			input, _ := r.items[0].AsInputItem()
			setDraft(c, "replacement")
			minted := operationID()
			c.mu.Lock()
			c.forkBoundary = &input
			c.forkRewind = true
			a, err := c.formAttemptLocked("ForkSession", minted)
			initiating := a.stamp
			c.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			c.task(func() { c.runAttempt(a.id) })
			restore := receive(t, transport.requests)
			if !strings.HasSuffix(restore.req.URL.Path, "/revert-code") || !strings.Contains(string(restore.body), *input.OperationId) {
				t.Fatalf("wrong restore address %s %s", restore.req.URL.Path, restore.body)
			}
			partial := map[string]any{"restored": []string{"one.go"}, "skipped": []map[string]string{{"path": "two.go", "reason": "changed"}}, "error": protocol.Error{Code: "conflict", Message: "partial"}}
			switch restoreKind {
			case "success":
				restore.respond(map[string]any{"restored": []string{"one.go"}, "skipped": []any{}})
			case "partial":
				restore.reject(409, partial)
			case "rejected":
				restore.reject(409, protocol.Error{Code: "conflict", Message: "denied restore"})
			case "unknown":
				restore.lost()
			}
			if restoreKind == "unknown" {
				receive(t, transport.requests).respond(hydration(rootA, "2", inputs(1, 1), nil))
				settleTasks(t, c)
				c.mu.Lock()
				if a.outcome != "unknown restore" || a.restoreHandled {
					t.Fatal("unknown rewind continued")
				}
				c.mu.Unlock()
				transport.mu.Lock()
				for _, route := range transport.calls {
					if strings.Contains(route, "/fork") {
						t.Fatal("Fork preceded unknown restore completion")
					}
				}
				transport.mu.Unlock()
				return
			}
			fork := receive(t, transport.requests)
			if !strings.HasSuffix(fork.req.URL.Path, "/fork") {
				t.Fatal("known restore did not precede Fork")
			}
			forkBody := append([]byte(nil), fork.body...)
			fork.lost()
			receive(t, transport.requests).respond(hydration(rootA, "2", inputs(1, 1), nil))
			settleTasks(t, c)
			out := outputs(c)
			if restoreKind == "partial" && (!strings.Contains(out, "one.go") || !strings.Contains(out, "two.go") || !strings.Contains(out, "partial")) {
				t.Fatal("partial restore dropped")
			}
			c.selectRoot(rootB, false)
			fresh := receive(t, transport.requests)
			fresh.respond(hydration(rootB, "1", nil, nil))
			settleTasks(t, c)
			c.retryAttempt(a.id)
			retry := receive(t, transport.requests)
			if !strings.HasSuffix(retry.req.URL.Path, "/fork") || string(retry.body) != string(forkBody) {
				t.Fatal("retry reran restore or changed Fork")
			}
			retry.respond(protocol.ForkResult{Session: protocol.Session{SessionId: childA}, Operation: protocol.Operation{OperationId: a.id}})
			settleTasks(t, c)
			c.mu.Lock()
			defer c.mu.Unlock()
			if a.stamp != initiating || c.rootID != rootB || a.restoreHandled == false {
				t.Fatal("retry replaced initiating stamp/adopted unrelated destination")
			}
		})
	}
}

func TestForkAndRevertMenusWaitRealOlderReader(t *testing.T) {
	for _, command := range []string{"/fork", "/revert"} {
		t.Run(command, func(t *testing.T) {
			c, transport := fixture(t)
			r, done := c.startReader()
			receive(t, transport.requests).respond(hydration(rootA, "1", inputs(10, 10), pointer("unchanged-oldest")))
			barrier(t, done)
			c.command(command)
			settleTasks(t, c)
			c.mu.Lock()
			if len(c.menu.items) != 2 || c.menu.items[0].Label != "input 10" {
				t.Fatal("recent input missing")
			}
			c.mu.Unlock()
			press(c, terminal.KeyMsg{Special: terminal.KeyDown})
			press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
			page := receive(t, transport.requests)
			if page.req.URL.Query().Get("cursor") != "unchanged-oldest" {
				t.Fatal("older menu decoded/replaced cursor")
			}
			c.mu.Lock()
			chain := r.chain
			if c.menu != nil {
				t.Fatal("menu reopened before real completion")
			}
			c.mu.Unlock()
			page.respond(protocol.HistoryPage{SessionRevision: revision("1"), Items: inputs(1, 9)})
			barrier(t, chain.done)
			settleTasks(t, c)
			c.mu.Lock()
			if c.menu == nil || len(c.menu.items) != 10 {
				t.Fatal("real reader completion did not reopen full menu")
			}
			c.mu.Unlock()
			press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
			if command == "/revert" {
				restore := receive(t, transport.requests)
				if !strings.Contains(string(restore.body), "operation-1") {
					t.Fatal("display position used as rewind boundary")
				}
				restore.respond(map[string]any{"restored": []any{}, "skipped": []any{}})
				settleTasks(t, c)
			} else {
				c.mu.Lock()
				if c.draft.String() != "input 1" || c.forkBoundary.ItemId != "item-1" || c.menu.kind != "fork-choice" {
					t.Fatal("fork not editable")
				}
				c.mu.Unlock()
			}
		})
	}
}

func TestLifecycleInvalidatesStaleListArchiveFirstDelete(t *testing.T) {
	c, transport := fixture(t)
	settledReader(t, c, transport)
	c.mu.Lock()
	s := c.stampLocked()
	c.mu.Unlock()
	c.task(func() { c.refreshList(s, "open", true) })
	parked := receive(t, transport.requests)
	c.archiveDelete(s, rootA, true)
	archive := receive(t, transport.requests)
	if !strings.HasSuffix(archive.req.URL.Path, "/archive") {
		t.Fatal("DELETE before Archive")
	}
	archive.respond(protocol.Session{SessionId: rootA, Lifecycle: "archived"})
	var deleteReq, listReq request
	for i := 0; i < 2; i++ {
		req := receive(t, transport.requests)
		if req.req.Method == "DELETE" {
			deleteReq = req
		} else {
			listReq = req
		}
	}
	c.mu.Lock()
	if c.rootID != "" || c.childID != "" {
		t.Fatal("known archive did not clear selection immediately")
	}
	c.mu.Unlock()
	deleteReq.reject(204, nil)
	final := receive(t, transport.requests)
	final.respond([]protocol.Session{{SessionId: rootB}})
	listReq.respond([]protocol.Session{{SessionId: rootA}})
	parked.respond([]protocol.Session{{SessionId: childA}})
	settleTasks(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rootID != "" || c.menu != nil || len(c.sessions) != 1 || c.sessions[0].SessionId != rootB {
		t.Fatal("stale list resurrected root or unrelated auto-adoption")
	}
}

func TestLifecycleUnknownRejectedArchiveStopsDelete(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			c, transport := fixture(t)
			c.mu.Lock()
			s := c.stampLocked()
			c.mu.Unlock()
			c.archiveDelete(s, rootB, true)
			req := receive(t, transport.requests)
			if unknown {
				req.lost()
			} else {
				req.reject(409, protocol.Error{Code: "conflict", Message: "busy"})
			}
			settleTasks(t, c)
			transport.mu.Lock()
			defer transport.mu.Unlock()
			for _, call := range transport.calls {
				if strings.HasPrefix(call, "DELETE") {
					t.Fatal("unknown/rejected archive continued DELETE")
				}
			}
		})
	}
}

func TestChildrenListsAndNavigationTaskMenusSuperseded(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			c, transport := fixture(t)
			settledReader(t, c, transport)
			c.mu.Lock()
			s := c.stampLocked()
			c.mu.Unlock()
			c.task(func() { c.childrenMenu(s) })
			old := receive(t, transport.requests)
			c.selectRoot(rootB, false)
			selected := receive(t, transport.requests)
			selected.respond(hydration(rootB, "1", nil, nil))
			if fail {
				old.reject(404, protocol.Error{Code: "not_found", Message: "stale child error"})
			} else {
				old.respond([]protocol.Session{{SessionId: childA, ParentSessionId: pointer(rootA)}})
			}
			settleTasks(t, c)
			c.mu.Lock()
			if c.menu != nil || c.rootID != rootB {
				t.Fatal("superseded child-list menu published")
			}
			c.mu.Unlock()
			if strings.Contains(outputs(c), "stale child error") {
				t.Fatal("stale task error published")
			}
			c.command("/children")
			open := receive(t, transport.requests)
			open.respond([]protocol.Session{{SessionId: childA, ParentSessionId: pointer(rootA)}, {SessionId: rootA, ParentSessionId: pointer(rootB)}})
			receive(t, transport.requests).respond([]protocol.Session{})
			settleTasks(t, c)
			c.mu.Lock()
			if len(c.menu.items) != 1 || c.menu.items[0].Label != rootA {
				t.Fatal("new root children were not filtered by parent")
			}
			c.mu.Unlock()
		})
	}
}

func TestUnknownCreateCapturedWorkspaceNoBlindComposition(t *testing.T) {
	c, transport := fixture(t)
	c.mu.Lock()
	c.rootID = ""
	s := c.stampLocked()
	c.mu.Unlock()
	c.task(func() { c.compose(s) })
	receive(t, transport.requests).respond([]protocol.Session{{SessionId: childA, ParentSessionId: pointer(rootB)}})
	create := receive(t, transport.requests)
	if !strings.Contains(string(create.body), "/workspace") {
		t.Fatal("Create did not capture Workspace")
	}
	create.lost()
	receive(t, transport.requests).respond([]protocol.Session{})
	settleTasks(t, c)
	c.task(func() { c.compose(s) })
	receive(t, transport.requests).respond([]protocol.Session{})
	settleTasks(t, c)
	transport.mu.Lock()
	count := 0
	for _, route := range transport.calls {
		if route == "POST /v1/sessions" {
			count++
		}
	}
	transport.mu.Unlock()
	if count != 1 {
		t.Fatal("unknown Create blindly resent")
	}
	if !strings.Contains(outputs(c), "Outcome unknown") {
		t.Fatal("unknown Create not presented")
	}
}

func TestUnresolvedAttemptCannotBeReplacedByUnchangedComposer(t *testing.T) {
	for _, operation := range []string{"SubmitSession", "CompactSession", "ForkSession"} {
		t.Run(operation, func(t *testing.T) {
			c, transport := fixture(t)
			r := settledReader(t, c, transport)
			setDraft(c, "retained draft")
			minted := operationID()
			c.mu.Lock()
			if operation == "ForkSession" {
				input, _ := r.items[0].AsInputItem()
				c.forkBoundary = &input
			}
			a, err := c.formAttemptLocked(operation, minted)
			c.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			c.task(func() { c.runAttempt(a.id) })
			first := receive(t, transport.requests)
			resubmit := func() {
				if operation == "CompactSession" {
					c.command("/compact")
				} else {
					press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
				}
			}
			resubmit()
			c.mu.Lock()
			if len(c.attempts) != 1 {
				t.Fatal("pending response formed a replacement ID")
			}
			c.mu.Unlock()
			if !strings.Contains(outputs(c), "Awaiting this attempt") {
				t.Fatal("pending attempt not explained")
			}
			first.lost()
			receive(t, transport.requests).respond(hydration(rootA, "2", inputs(1, 1), nil))
			settleTasks(t, c)
			outputs(c)
			resubmit()
			c.mu.Lock()
			if len(c.attempts) != 1 || c.attempts[a.id].outcome != "unknown" {
				t.Fatal("unknown attempt formed a replacement ID")
			}
			c.mu.Unlock()
			if !strings.Contains(outputs(c), "Outcome unknown") {
				t.Fatal("unchanged unknown composition lacked explicit retry guidance")
			}
			if operation != "CompactSession" {
				setDraft(c, "new edited draft")
				resubmit()
				fresh := receive(t, transport.requests)
				body := attemptBody(t, fresh)
				var id string
				_ = json.Unmarshal(body["operation_id"], &id)
				if id == a.id {
					t.Fatal("editing changed retained retry payload instead of forming new attempt")
				}
				fresh.reject(409, protocol.Error{Code: "conflict", Message: "server rejection"})
				receive(t, transport.requests).respond(hydration(rootA, "3", inputs(1, 1), nil))
				settleTasks(t, c)
			}
		})
	}
}

func TestKnownForkFailureRetainsHandledRestoreAndSourceSelection(t *testing.T) {
	c, transport := fixture(t)
	r := settledReader(t, c, transport)
	input, _ := r.items[0].AsInputItem()
	setDraft(c, "replacement")
	minted := operationID()
	c.mu.Lock()
	c.forkBoundary = &input
	c.forkRewind = true
	a, err := c.formAttemptLocked("ForkSession", minted)
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	c.task(func() { c.runAttempt(a.id) })
	receive(t, transport.requests).respond(protocol.CodeRevertResult{Restored: []string{"one.go"}, Skipped: []protocol.SkippedFile{}})
	receive(t, transport.requests).reject(409, protocol.Error{Code: "conflict", Message: "Fork rejected"})
	receive(t, transport.requests).respond(hydration(rootA, "2", inputs(1, 1), nil))
	settleTasks(t, c)
	c.mu.Lock()
	if c.root != r || c.rootID != rootA || c.draft.String() != "replacement" || a.outcome != "rejected" || !a.restoreHandled || !strings.Contains(string(a.restore), "one.go") {
		t.Fatal("Fork failure lost source/draft/known earlier restore")
	}
	c.mu.Unlock()
	text := outputs(c)
	if !strings.Contains(text, "one.go") || !strings.Contains(text, "Fork rejected") || strings.Contains(text, "rollback") {
		t.Fatal("known restore effects hidden or rollback claimed")
	}
}

func TestUnselectedArchiveInvalidatesListSuccessAndErrorWithoutClearingRoot(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			c, transport := fixture(t)
			root := settledReader(t, c, transport)
			c.mu.Lock()
			s := c.stampLocked()
			c.mu.Unlock()
			c.task(func() { c.refreshList(s, "open", true) })
			old := receive(t, transport.requests)
			c.archiveDelete(s, rootB, false)
			receive(t, transport.requests).respond(protocol.Session{SessionId: rootB, Lifecycle: "archived"})
			receive(t, transport.requests).respond([]protocol.Session{{SessionId: rootA}})
			if fail {
				old.reject(404, protocol.Error{Code: "not_found", Message: "late list error"})
			} else {
				old.respond([]protocol.Session{{SessionId: rootB}})
			}
			settleTasks(t, c)
			c.mu.Lock()
			if c.root != root || c.rootID != rootA || c.menu != nil || len(c.sessions) != 1 || c.sessions[0].SessionId != rootA {
				t.Fatal("unselected lifecycle mutation lost current root or accepted stale list")
			}
			c.mu.Unlock()
			if strings.Contains(outputs(c), "late list error") {
				t.Fatal("late list error survived mutation invalidation")
			}
		})
	}
}
