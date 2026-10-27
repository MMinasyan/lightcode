package cliclient

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/MMinasyan/lightcode/internal/terminal"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// A test-only screen for the relative cursor/erase, SGR and OSC sequences
// emitted here. It asserts the resulting screen, not just escape byte order.
type screenWriter struct {
	mu          sync.Mutex
	width, x, y int
	rows        [][]rune
	frames      chan string
}

func newScreen(width int) *screenWriter {
	return &screenWriter{width: width, rows: [][]rune{{}}, frames: make(chan string, 256)}
}
func (s *screenWriter) row() {
	for len(s.rows) <= s.y {
		s.rows = append(s.rows, nil)
	}
}
func (s *screenWriter) Write(data []byte) (int, error) {
	s.mu.Lock()
	text := string(data)
	for i := 0; i < len(text); {
		if text[i] == '\x1b' {
			if i+1 < len(text) && text[i+1] == ']' {
				end := strings.IndexByte(text[i:], '\a')
				if end < 0 {
					break
				}
				i += end + 1
				continue
			}
			if i+1 < len(text) && text[i+1] == '[' {
				j := i + 2
				for j < len(text) && !(text[j] >= 'A' && text[j] <= 'Z' || text[j] >= 'a' && text[j] <= 'z') {
					j++
				}
				if j == len(text) {
					break
				}
				n, _ := strconv.Atoi(text[i+2 : j])
				if n == 0 {
					n = 1
				}
				switch text[j] {
				case 'A':
					s.y = max(0, s.y-n)
				case 'B':
					s.y += n
				case 'C':
					s.x += n
				case 'K':
					s.row()
					s.rows[s.y] = nil
				case 'J':
					s.rows = [][]rune{{}}
					s.x = 0
					s.y = 0
				case 'H':
					s.x = 0
					s.y = 0
				}
				i = j + 1
				continue
			}
		}
		r, n := utf8.DecodeRuneInString(text[i:])
		i += n
		switch r {
		case '\r':
			s.x = 0
		case '\n':
			s.y++
		default:
			if s.x >= s.width {
				s.x = 0
				s.y++
			}
			s.row()
			for len(s.rows[s.y]) <= s.x {
				s.rows[s.y] = append(s.rows[s.y], ' ')
			}
			s.rows[s.y][s.x] = r
			s.x++
		}
	}
	s.mu.Unlock()
	s.frames <- text
	return len(data), nil
}
func (s *screenWriter) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := make([]string, len(s.rows))
	for i, row := range s.rows {
		lines[i] = strings.TrimRight(string(row), " ")
	}
	return strings.Join(lines, "\n")
}
func frameContaining(t *testing.T, s *screenWriter, text string) string {
	t.Helper()
	for {
		frame := receive(t, s.frames)
		if strings.Contains(frame, text) || strings.Contains(strings.ReplaceAll(frame, "\r\n", ""), text) {
			return frame
		}
	}
}

func TestActualWriterPromptErasureCanonicalReplacementAndPartialOutput(t *testing.T) {
	c, transport := fixture(t)
	r := settledReader(t, c, transport)
	c.mu.Lock()
	c.width = 30
	c.draft.Set("unsubmitted 012345678901234567890123456789 tail")
	c.draft.MoveLeft()
	c.draft.MoveLeft()
	c.draftRevision++
	c.mu.Unlock()
	screen := newScreen(30)
	go c.writer(screen)
	c.mu.Lock()
	c.notifyLocked()
	c.mu.Unlock()
	frameContaining(t, screen, "unsubmitted")
	c.command("/runtime NotAnOperation {}")
	frameContaining(t, screen, "unknown operation")
	result := screen.text()
	testassert.Require(t, strings.Count(result, "unsubmitted") == 1 && strings.Count(result, "input 1") == 1 && strings.Contains(result, "unknown operation"), "prompt corrupted transcript/command output:\n%s", result)
	// A pending assistant becomes terminal under the SAME item identity. The
	// accepted reader assignment, not permanent ID suppression, refreshes the range.
	pending := assistant("same-item", "pending content", true)
	done := c.refreshReader(r)
	receive(t, transport.requests).respond(hydration(rootA, "2", []protocol.ConversationItem{inputs(1, 1)[0], pending}, nil))
	barrier(t, done)
	frameContaining(t, screen, "pending content")
	settled, _ := pending.AsAssistantItem()
	settled.Content = textParts("terminal content")
	settled.ToolCalls[0].Status = pointer(protocol.ToolCallStatusSuccess)
	settled.ToolCalls[0].Content = pointer("actual tool result")
	_ = pending.FromAssistantItem(settled)
	done = c.refreshReader(r)
	receive(t, transport.requests).respond(hydration(rootA, "3", []protocol.ConversationItem{inputs(1, 1)[0], pending}, nil))
	barrier(t, done)
	frameContaining(t, screen, "terminal content")
	c.mu.Lock()
	s := c.stampLocked()
	c.mu.Unlock()
	partial := &protocol.CodeRevertResult{Restored: []string{"one.go"}, Skipped: []protocol.SkippedFile{{Path: "two.go", Reason: "changed"}}, Error: &protocol.Error{Code: "conflict", Message: "partial restore"}}
	raw, _ := json.Marshal(partial)
	c.report(s, terminal.RenderWarningMsg(string(raw)), true)
	frameContaining(t, screen, "partial restore")
	result = screen.text()
	testassert.Require(t, strings.Count(result, "unsubmitted") == 1 && strings.Contains(result, "terminal content") && strings.Contains(result, "actual tool result") && strings.Contains(strings.ReplaceAll(result, "\n", ""), "one.go") && strings.Contains(strings.ReplaceAll(result, "\n", ""), "two.go"), "resulting screen lost content:\n%s", result)
	c.requestExit(nil)
}

func TestFrameFollowsAcceptedReaderAssignments(t *testing.T) {
	c, transport := fixture(t)
	r, done := c.startReader()
	h := hydration(rootA, "1", inputs(2, 2), pointer("oldest"))
	h.ActiveOperation = &protocol.Operation{OperationId: "active", RequestKind: "message", Status: "running", AgentType: "primary", Model: "provider/model"}
	receive(t, transport.requests).respond(h)
	barrier(t, done)
	frame := func(content string) string {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.frameLocked(content)
	}
	const refreshed = "Authoritative refreshed range"
	testassert.Require(t, strings.Contains(frame(""), refreshed), "initial accepted range not rendered")
	press(c, terminal.KeyMsg{Rune: 'x'})
	testassert.Require(t, !strings.Contains(frame(""), refreshed), "key-only redraw reprinted history")
	c.acceptEvent(eventDelivery{c.conn, scopedProgress(rootA, "active", "provisional only", false)})
	testassert.Require(t, !strings.Contains(frame(""), refreshed), "provisional-only redraw reprinted history")
	done = c.refreshReader(r)
	receive(t, transport.requests).respond(h)
	barrier(t, done)
	testassert.Require(t, strings.Contains(frame(""), refreshed), "equal-body accepted assignment did not refresh the range")
	c.mu.Lock()
	r.pendingOlder = true
	c.kickReaderLocked(r)
	done = r.chain.done
	c.mu.Unlock()
	older := receive(t, transport.requests)
	testassert.Require(t, older.req.URL.Query().Get("cursor") == "oldest", "render invalidation changed the oldest cursor")
	older.respond(protocol.HistoryPage{SessionRevision: revision("1"), Items: inputs(1, 1)})
	barrier(t, done)
	rendered := frame("")
	testassert.Require(t, strings.Contains(rendered, refreshed) && strings.Contains(rendered, "input 1") && strings.Contains(rendered, "input 2"), "accepted older range not rendered")
	c.mu.Lock()
	testassert.Require(t, r.assignmentVersion == 3 && r.floor.DurableRevision == "1" && r.cursor == nil, "local assignments changed protocol revision or oldest-edge state")
	c.mu.Unlock()
	testassert.Require(t, strings.Contains(frame("\x1b[2J\x1b[3J\x1b[H"), refreshed), "clear did not redraw the accepted range")
}

func TestActualWriterToolMetadataRawArgsAndCanonicalClipboard(t *testing.T) {
	c, transport := fixture(t)
	r := settledReader(t, c, transport)
	metadata := json.RawMessage(`{"edit_preview":{"hunks":[{"rows":[{"kind":"remove","oldLine":1,"text":"old metadata"},{"kind":"add","newLine":1,"text":"new metadata"}]}]}}`)
	a := protocol.AssistantItem{Kind: "assistant", ItemId: "edit", Status: "completed", Source: "provider/model", Content: append(textParts("canonical first"), textParts("canonical second")...), ToolCalls: []protocol.ToolCallView{
		{Id: "call", Name: "edit_file", Arguments: `{"path":"raw.go"}`, NormalizedArguments: pointer(protocol.JSONObject{"path": json.RawMessage(`"normalized.go"`)}), Metadata: &metadata, Status: pointer(protocol.ToolCallStatusSuccess), Content: pointer("not a preview")},
		{Id: "uncertain", Name: "echo", Arguments: "{}", Status: pointer(protocol.ToolCallStatusInterrupted), Content: pointer("Tool call interrupted."), Evidence: pointer(protocol.Unknown)},
		{Id: "clean", Name: "echo", Arguments: "{}", Status: pointer(protocol.ToolCallStatusInterrupted), Content: pointer("Tool call interrupted."), Evidence: pointer(protocol.NotStarted)},
	}}
	var item protocol.ConversationItem
	_ = item.FromAssistantItem(a)
	done := c.refreshReader(r)
	// The empty assistant after the copyable text is skipped by /copy.
	receive(t, transport.requests).respond(hydration(rootA, "2", []protocol.ConversationItem{inputs(1, 1)[0], item, assistant("empty", "", false)}, nil))
	barrier(t, done)
	screen := newScreen(80)
	go c.writer(screen)
	c.mu.Lock()
	c.provisional = "must not copy"
	c.notifyLocked()
	c.mu.Unlock()
	frameContaining(t, screen, "new metadata")
	result := screen.text()
	testassert.Require(t, strings.Contains(result, "raw.go") && !strings.Contains(result, "normalized.go") && strings.Contains(result, "old metadata"), "tool display used wrong arguments/metadata: %s", result)
	// The unknown outcome renders uncertain — its own "outcome unknown"
	// marker — while the proven-unstarted clean cancellation renders exactly
	// as before: both stored contents appear, only the uncertain call gains
	// the marker.
	testassert.Require(t, strings.Count(result, "outcome unknown") == 1 && strings.Count(result, "Tool call interrupted.") == 2, "unknown-vs-clean cancellation rendering: %s", result)
	c.command("/copy")
	osc := frameContaining(t, screen, "\x1b]52;")
	encoded := base64.StdEncoding.EncodeToString([]byte("canonical first\ncanonical second"))
	testassert.Require(t, strings.Contains(osc, "\x1b]52;c;"+encoded+"\a"), "not canonical OSC52: %q", osc)
	c.requestExit(nil)
}

func scopedProgress(id, op, text string, refusal bool) protocol.Event {
	var scope protocol.Scope
	_ = scope.FromOperationScope(protocol.OperationScope{Kind: "operation", SessionId: id, OperationId: op})
	var event protocol.Event
	if refusal {
		_ = event.FromRefusalDeltaEvent(protocol.RefusalDeltaEvent{Kind: "refusal_delta", Content: text, Scope: scope})
	} else {
		_ = event.FromTextDeltaEvent(protocol.TextDeltaEvent{Kind: "text_delta", Content: text, Position: 0, Scope: scope})
	}
	return event
}
func TestActualWriterProvisionalBoundedNoncanonicalAndReset(t *testing.T) {
	c, transport := fixture(t)
	r, done := c.startReader()
	h := hydration(rootA, "1", inputs(1, 1), nil)
	h.ActiveOperation = &protocol.Operation{OperationId: "active", RequestKind: "compact", Model: "captured/model", AgentType: "captured-type"}
	h.SelectedModel = pointer("future/model")
	receive(t, transport.requests).respond(h)
	barrier(t, done)
	screen := newScreen(80)
	go c.writer(screen)
	c.mu.Lock()
	c.notifyLocked()
	c.mu.Unlock()
	frameContaining(t, screen, "Active compact")
	c.events <- eventDelivery{c.conn, scopedProgress(rootA, "active", "first-position", false)}
	frameContaining(t, screen, "first-position")
	c.events <- eventDelivery{c.conn, scopedProgress(rootA, "active", "repeated-position", true)}
	frameContaining(t, screen, "[Refusal]")
	testassert.Require(t, strings.Count(screen.text(), "input 1") == 1, "provisional reprinted canonical history")
	c.mu.Lock()
	testassert.Require(t, len(r.items) == 1, "progress created canonical items")
	c.mu.Unlock()
	// Direct concrete event acceptance keeps the screen fixture small while
	// exercising the same bounded area used by the writer above.
	c.acceptEvent(eventDelivery{c.conn, scopedProgress(rootA, "active", strings.Repeat("é", 40000), false)})
	c.mu.Lock()
	testassert.Require(t, len(c.provisional) <= 65536 && utf8.ValidString(c.provisional) && c.provisionalTruncated, "provisional bound is not UTF8 safe")
	c.mu.Unlock()
	done = c.refreshReader(r)
	h.SessionRevision = revision("2")
	h.ActiveOperation = nil
	receive(t, transport.requests).respond(h)
	barrier(t, done)
	c.mu.Lock()
	testassert.Require(t, c.provisional == "" && !c.provisionalTruncated, "accepted hydration did not clear provisional")
	c.mu.Unlock()
	c.requestExit(nil)
}

func TestUsageWarningsOwnAndGlobalAndUnknownRuntime(t *testing.T) {
	c, transport := fixture(t)
	r := settledReader(t, c, transport)
	h := hydration(rootA, "2", inputs(1, 1), nil)
	h.Usage.Context.UsedTokens = "9007199254740993"
	h.Usage.Totals.ByModel = []protocol.ModelUsage{{Model: "provider/model", Usage: protocol.UsageCount{InputTokens: "18446744073709551615", OutputTokens: "1", CachedInputTokens: "0"}}}
	h.Warnings = []protocol.Warning{{Message: "global"}, {SessionId: pointer(rootA), Message: "own"}}
	done := c.refreshReader(r)
	receive(t, transport.requests).respond(h)
	barrier(t, done)
	transport.mu.Lock()
	transport.globals = true
	transport.mu.Unlock()
	c.mu.Lock()
	c.globalsDirty = true
	c.kickGlobalsLocked()
	c.mu.Unlock()
	receive(t, transport.requests).respond(configuration("1"))
	w := warnings("1")
	w.Warnings = append(h.Warnings, protocol.Warning{SessionId: pointer(rootB), Message: "sibling"})
	receive(t, transport.requests).respond(w)
	settleTasks(t, c)
	c.command("/context")
	c.command("/warnings")
	text := outputs(c)
	testassert.Require(t, strings.Contains(text, "9007199254740993") && strings.Contains(text, "18446744073709551615") && strings.Contains(text, "own") && !strings.Contains(text, "sibling"), "usage/warnings lost exact subset")
	c.mu.Lock()
	c.root = nil
	c.mu.Unlock()
	c.command("/warnings")
	text = outputs(c)
	testassert.Require(t, strings.Contains(text, "global") && !strings.Contains(text, "own") && !strings.Contains(text, "sibling"), "global-only indicator included owned groups")
	c.command("/runtime UnknownOperation {}")
	settleTasks(t, c)
	text = outputs(c)
	testassert.Require(t, strings.Contains(text, "unknown operation") && !strings.Contains(text, "Outcome unknown"), "unknown /runtime classification: %s", text)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, route := range transport.calls {
		testassert.Require(t, !strings.Contains(route, "UnknownOperation"), "unknown runtime performed HTTP")
	}
}

func TestActualClipboardRefusalAndNoLoadedAssistant(t *testing.T) {
	empty := assistant("empty", "", false)
	toolOnly := assistant("tool-only", "", true)
	refusalItem := func() protocol.ConversationItem {
		var item protocol.ConversationItem
		_ = item.FromAssistantItem(protocol.AssistantItem{Kind: "assistant", ItemId: "refusal", Status: "completed", Source: "provider/model", Content: []protocol.ContentPart{}, ToolCalls: []protocol.ToolCallView{}, Refusal: pointer("canonical refusal")})
		return item
	}
	for _, test := range []struct {
		name string
		tail []protocol.ConversationItem
		want string
	}{
		{"no assistant", nil, ""},
		{"empty and tool-only assistants only", []protocol.ConversationItem{empty, toolOnly}, ""},
		{"refusal before an empty assistant", []protocol.ConversationItem{refusalItem(), empty}, base64.StdEncoding.EncodeToString([]byte("canonical refusal"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, transport := fixture(t)
			_, done := c.startReader()
			h := hydration(rootA, "1", append(inputs(1, 1), test.tail...), pointer("unloaded-older"))
			h.ActiveOperation = &protocol.Operation{OperationId: "active", RequestKind: "message", Status: "running", AgentType: "primary", Model: "provider/model"}
			receive(t, transport.requests).respond(h)
			barrier(t, done)
			screen := newScreen(80)
			go c.writer(screen)
			c.events <- eventDelivery{c.conn, scopedProgress(rootA, "active", "provisional must not be copied", false)}
			frameContaining(t, screen, "provisional must not be copied")
			c.command("/copy")
			if test.want == "" {
				frame := frameContaining(t, screen, "No loaded authoritative assistant")
				testassert.Require(t, !strings.Contains(frame, "\x1b]52;"), "an absent or empty history wrote the clipboard")
			} else {
				frame := frameContaining(t, screen, "\x1b]52;")
				testassert.Require(t, strings.Contains(frame, test.want), "copy did not skip the empty assistant: %q", frame)
			}
			transport.mu.Lock()
			for _, route := range transport.calls {
				testassert.Require(t, !strings.Contains(route, "/history"), "copy guessed skipped history")
			}
			transport.mu.Unlock()
			c.requestExit(nil)
		})
	}
}

func TestProgressTupleUnmatchedOperationRepairsWithoutReplay(t *testing.T) {
	c, transport := fixture(t)
	r, done := c.startReader()
	h := hydration(rootA, "1", inputs(1, 1), nil)
	h.ActiveOperation = &protocol.Operation{OperationId: "first", RequestKind: "message", Status: "running", AgentType: "primary", Model: "provider/model"}
	receive(t, transport.requests).respond(h)
	barrier(t, done)
	tool := func(session, operation string) protocol.Event {
		var scope protocol.Scope
		_ = scope.FromOperationScope(protocol.OperationScope{Kind: "operation", SessionId: session, OperationId: operation})
		var event protocol.Event
		_ = event.FromToolStartedEvent(protocol.ToolStartedEvent{Kind: "tool_started", CallId: "same-call", Name: "temporary name", Scope: scope})
		return event
	}
	c.acceptEvent(eventDelivery{c.conn, tool(rootA, "first")})
	c.mu.Lock()
	testassert.Require(t, strings.Contains(c.provisional, "same-call") && len(r.items) == 1, "matched tool hint not temporary activity")
	c.mu.Unlock()
	done = c.refreshReader(r)
	h.SessionRevision = revision("2")
	h.ActiveOperation.OperationId = "second"
	receive(t, transport.requests).respond(h)
	barrier(t, done)
	c.acceptEvent(eventDelivery{c.conn, tool(rootA, "first")})
	repair := receive(t, transport.requests)
	c.mu.Lock()
	done = r.chain.done
	testassert.Require(t, c.provisional == "", "old Operation call ID reused in new activity")
	c.mu.Unlock()
	repair.respond(h)
	barrier(t, done)
	c.mu.Lock()
	testassert.Require(t, c.provisional == "" && len(r.items) == 1, "unmatched hint replayed into canonical/provisional items")
	c.mu.Unlock()
	c.acceptEvent(eventDelivery{c.conn, tool(rootB, "second")})
	c.mu.Lock()
	testassert.Require(t, r.chain == nil && c.provisional == "", "foreign Session tool hint dirtied selected Session")
	c.mu.Unlock()
	c.acceptEvent(eventDelivery{c.conn, tool(rootA, "second")})
	// A finished unknown outcome's transient label is uncertain — never the
	// plain status of a clean, safe cancellation.
	var finished protocol.Event
	{
		var scope protocol.Scope
		_ = scope.FromOperationScope(protocol.OperationScope{Kind: "operation", SessionId: rootA, OperationId: "second"})
		unknownEvidence := protocol.Unknown
		_ = finished.FromToolFinishedEvent(protocol.ToolFinishedEvent{Kind: protocol.ToolFinished, CallId: "same-call", Status: protocol.ToolCallStatusInterrupted, Scope: scope, Evidence: &unknownEvidence})
	}
	c.acceptEvent(eventDelivery{c.conn, finished})
	c.mu.Lock()
	defer c.mu.Unlock()
	testassert.Require(t, strings.Contains(c.provisional, "same-call"), "same call ID under current Operation was suppressed")
	testassert.Require(t, strings.Contains(c.provisional, "same-call: outcome unknown") && !strings.Contains(c.provisional, "same-call: interrupted"), "unknown tool outcome transient label = %q, want the uncertain wording, not a clean cancellation", c.provisional)
}
