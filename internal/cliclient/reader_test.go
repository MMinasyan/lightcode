package cliclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/clientview"
	"github.com/MMinasyan/lightcode/internal/terminal"
	"github.com/MMinasyan/lightcode/protocol"
)

const instance = "0123456789abcdef0123456789abcdef"
const rootA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const rootB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const childA = "cccccccccccccccccccccccccccccccc"

type reply struct {
	status int
	value  any
	raw    string
	err    error
}
type request struct {
	req   *http.Request
	body  []byte
	reply chan reply
}

func (r request) respond(value any)            { r.reply <- reply{status: 200, value: value} }
func (r request) respondBody(raw string)       { r.reply <- reply{status: 200, raw: raw} }
func (r request) reject(status int, value any) { r.reply <- reply{status: status, value: value} }
func (r request) lost()                        { r.reply <- reply{err: io.ErrUnexpectedEOF} }

type scriptedTransport struct {
	requests chan request
	mu       sync.Mutex
	globals  bool
	calls    []string
}

func (s *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "Bearer "+strings.Repeat("1", 64) {
		return nil, fmt.Errorf("missing authenticated generated request")
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	s.mu.Lock()
	s.calls = append(s.calls, req.Method+" "+req.URL.RequestURI())
	globals := s.globals
	s.mu.Unlock()
	var result reply
	if !globals && req.URL.Path == "/v1/configuration" {
		result = reply{status: 200, value: configuration("1")}
	} else if !globals && req.URL.Path == "/v1/warnings" {
		result = reply{status: 200, value: warnings("1")}
	} else {
		r := request{req: req, body: body, reply: make(chan reply, 1)}
		select {
		case s.requests <- r:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		// Deliberately return parked success/errors even after cancellation:
		// supersession, not an obliging HTTP transport, guards publication.
		result = <-r.reply
	}
	if result.err != nil {
		return nil, result.err
	}
	raw := []byte(result.raw)
	if result.raw == "" {
		raw, _ = json.Marshal(result.value)
	}
	if result.status == 204 {
		raw = nil
	}
	return &http.Response{StatusCode: result.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: req}, nil
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("channel barrier not reached")
		var zero T
		return zero
	}
}
func barrier(t *testing.T, ch <-chan struct{}) { t.Helper(); receive(t, ch) }

func discovery(t *testing.T, handler http.Handler) (clienthost.Options, *client.Client) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]string{"endpoint": server.URL, "instance_id": instance, "protocol_version": string(protocol.N2), "credential": strings.Repeat("1", 64)})
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	conn, err := client.Connect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return clienthost.Options{DataDir: dir}, conn
}
func health(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(protocol.Health{InstanceId: instance, ProtocolVersion: protocol.N2})
}

func fixture(t *testing.T) (*CLI, *scriptedTransport) {
	t.Helper()
	_, conn := discovery(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			t.Errorf("unexpected discovery %s", r.URL.Path)
		}
		health(w)
	}))
	transport := &scriptedTransport{requests: make(chan request, 32)}
	generated, ok := conn.ClientWithResponses.ClientInterface.(*protocol.Client)
	if !ok {
		t.Fatal("connection does not own the concrete generated client")
	}
	generated.Client = &http.Client{Transport: transport}
	c := New(Options{Workspace: "/workspace"})
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.conn = conn
	c.connCtx, c.connCancel = context.WithCancel(c.ctx)
	c.rootID = rootA
	c.sessions = []protocol.Session{{SessionId: rootA, Workspace: "/workspace"}, {SessionId: rootB, Workspace: "/workspace"}}
	t.Cleanup(func() { c.mu.Lock(); c.closed = true; c.cancel(); c.mu.Unlock() })
	return c, transport
}
func revision(n string) protocol.SessionRevision {
	return protocol.SessionRevision{InstanceId: instance, DurableRevision: n, LocalRevision: "0"}
}
func configuration(n string) protocol.ConfigurationView {
	return protocol.ConfigurationView{ConfigurationRevision: protocol.ConfigurationRevision{InstanceId: instance, Generation: n}, Agents: []protocol.Agent{{Name: "primary", Model: "provider/next", Capabilities: []string{}, Tools: []string{}}}, Providers: []protocol.Provider{}, Settings: protocol.Settings{Plugins: protocol.PluginsSettings{}}}
}
func warnings(n string) protocol.WarningsSnapshot {
	return protocol.WarningsSnapshot{WarningsRevision: protocol.WarningsRevision{InstanceId: instance, Revision: n}, Warnings: []protocol.Warning{}}
}
func hydration(id, n string, items []protocol.ConversationItem, cursor *string) protocol.Hydration {
	if items == nil {
		items = []protocol.ConversationItem{}
	}
	return protocol.Hydration{Session: protocol.Session{SessionId: id, Workspace: "/workspace", AgentType: "primary", Lifecycle: "open", SessionRevision: revision(n)}, SessionRevision: revision(n), ConfigurationRevision: configuration("1").ConfigurationRevision, WarningsRevision: warnings("1").WarningsRevision, Conversation: protocol.ConversationPage{Items: items, OlderCursor: cursor}, Operations: []protocol.Operation{}, Background: []protocol.BackgroundMember{}, Warnings: []protocol.Warning{}, Pending: protocol.PendingQueues{Queued: []protocol.PendingInput{}, Steering: []protocol.PendingInput{}}, Usage: protocol.UsageProjection{Context: protocol.UsageContext{UsedTokens: "0"}, Totals: protocol.UsageTotals{ByModel: []protocol.ModelUsage{}}}}
}
func pointer[T any](v T) *T { return &v }

// inputOf is the one input-item constructor; inputs and the recall stimuli
// reuse it instead of rebuilding InputItems per test.
func inputOf(id, operation string, origin protocol.InputOrigin, content []protocol.ContentPart) protocol.ConversationItem {
	var item protocol.ConversationItem
	_ = item.FromInputItem(protocol.InputItem{Kind: "input", Origin: origin, ItemId: id, Content: content, OperationId: pointer(operation)})
	return item
}

// imageURLPart is the one image content part the image-only recall stimulus
// uses.
func imageURLPart() protocol.ContentPart {
	var part protocol.ContentPart
	_ = part.FromImageURLPart(protocol.ImageURLPart{Kind: protocol.ImageUrl, Url: "http://img/x.png"})
	return part
}

func inputs(start, end int) []protocol.ConversationItem {
	var items []protocol.ConversationItem
	for i := start; i <= end; i++ {
		items = append(items, inputOf(fmt.Sprintf("item-%d", i), fmt.Sprintf("operation-%d", i), protocol.InputOriginUser, textParts(fmt.Sprintf("input %d", i))))
	}
	return items
}
func assistant(id, text string, pending bool) protocol.ConversationItem {
	a := protocol.AssistantItem{Kind: "assistant", ItemId: id, Status: "completed", Source: "provider/model", Content: textParts(text), ToolCalls: []protocol.ToolCallView{}}
	if pending {
		a.ToolCalls = []protocol.ToolCallView{{Id: "call", Name: "shell", Arguments: "{}"}}
	}
	var item protocol.ConversationItem
	_ = item.FromAssistantItem(a)
	return item
}
func (c *CLI) startReader() (*sessionReader, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.root = c.newReaderLocked(c.rootID)
	c.kickReaderLocked(c.root)
	return c.root, c.root.chain.done
}
func (c *CLI) refreshReader(r *sessionReader) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	r.dirty = true
	c.kickReaderLocked(r)
	return r.chain.done
}
func ids(t *testing.T, items []protocol.ConversationItem) []string {
	t.Helper()
	var ids []string
	for _, item := range items {
		id, err := clientview.ItemID(item)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestReaderAnchoredHistory(t *testing.T) {
	for _, test := range []struct {
		name       string
		pages      []protocol.HistoryPage
		freshStart int
		unresolved bool
	}{
		{"terminal-1-50-52-101", []protocol.HistoryPage{{Items: inputs(2, 51), OlderCursor: pointer("older-edge"), SessionRevision: revision("2")}}, 52, false},
		{"several-missing-pages", []protocol.HistoryPage{{Items: inputs(80, 120), OlderCursor: pointer("page-two"), SessionRevision: revision("2")}, {Items: inputs(40, 79), OlderCursor: pointer("page-three"), SessionRevision: revision("2")}}, 121, false},
		{"unresolved-beyond-first-overlap", []protocol.HistoryPage{{Items: inputs(30, 51), OlderCursor: pointer("page-two"), SessionRevision: revision("2")}, {Items: inputs(1, 29), OlderCursor: pointer("older-edge"), SessionRevision: revision("2")}}, 52, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, transport := fixture(t)
			r, done := c.startReader()
			cached := inputs(1, 50)
			if test.unresolved {
				cached[9] = assistant("item-10", "pending", true)
				test.pages[1].Items[9] = assistant("item-10", "settled", false)
			}
			receive(t, transport.requests).respond(hydration(rootA, "1", cached, pointer("oldest-original")))
			barrier(t, done)
			done = c.refreshReader(r)
			receive(t, transport.requests).respond(hydration(rootA, "2", inputs(test.freshStart, max(101, test.freshStart+10)), pointer("page-one")))
			for i, page := range test.pages {
				req := receive(t, transport.requests)
				want := "page-one"
				if i > 0 {
					want = *test.pages[i-1].OlderCursor
				}
				if req.req.URL.Query().Get("cursor") != want {
					t.Fatalf("cursor %q, want %q", req.req.URL.Query().Get("cursor"), want)
				}
				c.mu.Lock()
				if !reflect.DeepEqual(ids(t, r.items), ids(t, cached)) {
					t.Error("disconnected range published before walk completed")
				}
				c.mu.Unlock()
				req.respond(page)
			}
			barrier(t, done)
			// The loaded USER text range is recallable, older pages included.
			press(c, terminal.KeyMsg{Special: terminal.KeyUp})
			if c.draft.String() != fmt.Sprintf("input %d", max(101, test.freshStart+10)) {
				t.Fatalf("recall = %q, want the newest loaded text", c.draft.String())
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			want := ids(t, inputs(1, max(101, test.freshStart+10)))
			if !reflect.DeepEqual(ids(t, r.items), want) {
				t.Fatalf("range %v", ids(t, r.items))
			}
			if r.cursor == nil || *r.cursor != "oldest-original" {
				t.Fatalf("oldest edge changed: %v", r.cursor)
			}
			if test.unresolved {
				a, _ := r.items[9].AsAssistantItem()
				if contentText(a.Content) != "settled" {
					t.Fatal("unresolved value not replaced")
				}
			}
		})
	}
}

func TestReaderFloorOlderDisjointAndHints(t *testing.T) {
	c, transport := fixture(t)
	r, done := c.startReader()
	receive(t, transport.requests).respond(hydration(rootA, "5", inputs(51, 100), pointer("real-oldest")))
	barrier(t, done)
	c.mu.Lock()
	r.pendingOlder = true
	c.kickReaderLocked(r)
	done = r.chain.done
	c.mu.Unlock()
	older := receive(t, transport.requests)
	if older.req.URL.Query().Get("cursor") != "real-oldest" {
		t.Fatal("older cursor changed")
	}
	c.mu.Lock()
	r.dirty = true
	c.kickReaderLocked(r)
	if done != r.chain.done {
		t.Fatal("parallel reader")
	}
	c.mu.Unlock()
	older.respond(protocol.HistoryPage{Items: inputs(1, 50), OlderCursor: pointer("edge"), SessionRevision: revision("6")})
	receive(t, transport.requests).respond(hydration(rootA, "5", inputs(51, 100), nil))
	// The whole stale hydration is rejected, including its queue and usage.
	fresh := hydration(rootA, "6", inputs(51, 100), nil)
	fresh.Usage.Context.UsedTokens = "9007199254740993"
	fresh.Pending.Queued = []protocol.PendingInput{{OperationId: "queued-id"}}
	receive(t, transport.requests).respond(fresh)
	barrier(t, done)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(r.items) != 100 || r.floor.DurableRevision != "6" || *r.cursor != "edge" || r.hydration.Usage.Context.UsedTokens != "9007199254740993" || r.hydration.Pending.Queued[0].OperationId != "queued-id" {
		t.Fatalf("reader %#v", r)
	}
}

func TestReaderNavigationSupersedesSuccessAndError(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			c, transport := fixture(t)
			old, oldDone := c.startReader()
			parked := receive(t, transport.requests)
			c.selectRoot(rootB)
			receive(t, transport.requests).respond(hydration(rootB, "1", nil, nil))
			fresh := receive(t, transport.requests)
			fresh.respond(hydration(rootB, "1", inputs(1, 1), nil))
			c.mu.Lock()
			selected := c.root
			done := selected.chain.done
			c.mu.Unlock()
			barrier(t, done)
			if fail {
				parked.reject(404, protocol.Error{Code: "not_found", Message: "late error"})
			} else {
				parked.respond(hydration(rootA, "9223372036854775807", inputs(1, 5), nil))
			}
			barrier(t, oldDone)
			c.mu.Lock()
			if c.root != selected || c.rootID != rootB || old.hydration != nil {
				t.Fatal("superseded reader published")
			}
			c.mu.Unlock()
			for len(c.outputs) > 0 {
				if d := <-c.outputs; strings.Contains(d.text, "late error") {
					t.Fatal("superseded error printed")
				}
			}
		})
	}
}

func TestRootResetDisposesInspectorAndSameRootIntent(t *testing.T) {
	c, transport := fixture(t)
	_, done := c.startReader()
	// The accepted root range seeds recall from USER text inputs only: a
	// Runtime-origin, image-only and blank inputs never enter it.
	items := append(inputs(1, 3),
		inputOf("runtime-1", "operation-runtime", protocol.InputOriginRuntime, textParts("runtime input")),
		inputOf("image-1", "operation-image", protocol.InputOriginUser, []protocol.ContentPart{imageURLPart()}),
		inputOf("blank-1", "operation-blank", protocol.InputOriginUser, textParts(" \t ")),
	)
	receive(t, transport.requests).respond(hydration(rootA, "1", items, nil))
	barrier(t, done)
	press(c, terminal.KeyMsg{Special: terminal.KeyUp})
	if c.draft.String() != "input 3" {
		t.Fatalf("recall = %q, want the newest USER text input", c.draft.String())
	}
	c.inspectChild(childA)
	// The inspector publishes its own USER inputs and never seeds recall; the
	// root's stale read stays parked across the reset below.
	var rootStale request
	var childDone chan struct{}
	for i := 0; i < 2; i++ {
		req := receive(t, transport.requests)
		if strings.Contains(req.req.URL.Path, childA) {
			c.mu.Lock()
			childDone = c.child.chain.done
			c.mu.Unlock()
			req.respond(hydration(childA, "1", inputs(9, 9), nil))
		} else {
			rootStale = req
		}
	}
	c.mu.Lock()
	oldRoot := c.root
	rootChain := oldRoot.chain.done
	generation := c.selection
	c.mu.Unlock()
	press(c, terminal.KeyMsg{Rune: 'z'})
	press(c, terminal.KeyMsg{Special: terminal.KeyUp})
	if c.draft.String() != "input 3" {
		t.Fatalf("inspector seeded recall: %q", c.draft.String())
	}
	// An ordinary entered slash command joins volatile recall.
	setDraft(c, "/help")
	press(c, terminal.KeyMsg{Special: terminal.KeyEnter})
	press(c, terminal.KeyMsg{Special: terminal.KeyUp})
	if c.draft.String() != "/help" {
		t.Fatalf("slash recall = %q", c.draft.String())
	}
	c.selectRoot(rootA)
	receive(t, transport.requests).respond(hydration(rootA, "1", nil, nil))
	fresh := receive(t, transport.requests)
	fresh.respond(hydration(rootA, "2", nil, nil))
	c.mu.Lock()
	newRoot := c.root
	newDone := newRoot.chain.done
	c.mu.Unlock()
	barrier(t, newDone)
	// The replaced root clears the previous range's recall.
	setDraft(c, "keep")
	press(c, terminal.KeyMsg{Special: terminal.KeyUp})
	if c.draft.String() != "keep" {
		t.Fatalf("root replacement kept recall: %q", c.draft.String())
	}
	rootStale.respond(hydration(rootA, "100", inputs(1, 1), nil))
	barrier(t, rootChain)
	barrier(t, childDone)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selection <= generation || c.root == oldRoot || c.child != nil || c.childID != "" || c.root.hydration.SessionRevision.DurableRevision != "2" {
		t.Fatal("root reset failed to dispose both readers")
	}
}
