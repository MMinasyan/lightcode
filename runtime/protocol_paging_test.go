package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// The paging, pending, usage and hydration reads: pure producers over typed
// snapshots pin the cursor, page, and usage-clock arithmetic; the live rows
// drive the composed Runtime over both stores, seeding real durable
// histories through the storage contract (register usage equal to the
// entries) and reloading the captured configuration for the distinct
// window clocks.

// pageEntryID is one deterministic fixture entry identity.
func pageEntryID(i int) string {
	return fmt.Sprintf("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa%02d", i)
}

// pageSnapshot builds one typed snapshot with the given number of input
// facts, each projecting to exactly one item.
func pageSnapshot(n int) harness.SessionSnapshot {
	facts := make([]harness.HistoryFact, 0, n)
	for i := 0; i < n; i++ {
		facts = append(facts, convInputFact(pageEntryID(i), "op-1"))
	}
	return convSnapshot(facts)
}

// pageItemIDs reads one page's item identities in order.
func pageItemIDs(t *testing.T, page protocol.HistoryPage) []string {
	t.Helper()
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		id, err := conversationItemID(item)
		if err != nil {
			t.Fatalf("conversationItemID: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// wantPageItemID is the stable identity of fixture item i.
func wantPageItemID(i int) string {
	return projectItemID(convSessionID, pageEntryID(i))
}

// TestHistoryPagePureThreePages proves the exact anchored paging arithmetic
// over 120 items: the newest 50 ascending, then the contiguous older pages,
// the oldest returned item anchoring the next cursor exactly when older
// items remain, and every older page ending strictly before its anchor.
func TestHistoryPagePureThreePages(t *testing.T) {
	snap := pageSnapshot(120)

	page1, err := projectHistoryPage(snap, nil)
	if err != nil {
		t.Fatalf("initial page: %v", err)
	}
	if page1.SessionRevision != wantSessionRevision(t, snap) {
		t.Fatalf("page revision = %+v, want the snapshot pair %+v", page1.SessionRevision, wantSessionRevision(t, snap))
	}
	if len(page1.Items) != 50 {
		t.Fatalf("initial page = %d items, want exactly 50", len(page1.Items))
	}
	for i, id := range pageItemIDs(t, page1) {
		if id != wantPageItemID(70+i) {
			t.Fatalf("initial item %d = %q, want the ascending item %d", i, id, 70+i)
		}
	}
	if page1.OlderCursor == nil {
		t.Fatal("initial page produced no older cursor with 70 older items")
	}
	cursor1, err := decodeHistoryCursor(*page1.OlderCursor, convSessionID)
	if err != nil {
		t.Fatalf("decode older cursor: %v", err)
	}
	if cursor1.Version != 1 || cursor1.SessionID != convSessionID || cursor1.Direction != "older" ||
		cursor1.AnchorItemID != wantPageItemID(70) {
		t.Fatalf("older cursor = %+v, want version 1, this session, older, anchored at item 70", cursor1)
	}

	page2, err := projectHistoryPage(snap, page1.OlderCursor)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	ids2 := pageItemIDs(t, page2)
	if len(ids2) != 50 {
		t.Fatalf("second page = %d items, want exactly 50", len(ids2))
	}
	for i, id := range ids2 {
		if id != wantPageItemID(20+i) {
			t.Fatalf("second page item %d = %q, want item %d", i, id, 20+i)
		}
	}
	for _, id := range ids2 { // strictly before the anchor
		if id == cursor1.AnchorItemID {
			t.Fatalf("second page contains its own anchor %q", id)
		}
	}
	if page2.OlderCursor == nil {
		t.Fatal("second page produced no older cursor with 20 older items")
	}
	cursor2, err := decodeHistoryCursor(*page2.OlderCursor, convSessionID)
	if err != nil {
		t.Fatalf("decode second cursor: %v", err)
	}
	if cursor2.AnchorItemID != wantPageItemID(20) {
		t.Fatalf("second cursor anchor = %q, want item 20", cursor2.AnchorItemID)
	}

	page3, err := projectHistoryPage(snap, page2.OlderCursor)
	if err != nil {
		t.Fatalf("third page: %v", err)
	}
	ids3 := pageItemIDs(t, page3)
	if len(ids3) != 20 {
		t.Fatalf("third page = %d items, want the remaining 20", len(ids3))
	}
	for i, id := range ids3 {
		if id != wantPageItemID(i) {
			t.Fatalf("third page item %d = %q, want item %d", i, id, i)
		}
	}
	if page3.OlderCursor != nil {
		t.Fatalf("third page produced an older cursor %v with nothing older", page3.OlderCursor)
	}
}

// TestHistoryPagePureIndivisibleBoundary proves the page boundary can never
// split an assistant from its tool result: the result is absorbed into its
// assistant item, the assistant sits at the page's edge carrying its
// terminal outcome, and no standalone result item exists anywhere.
func TestHistoryPagePureIndivisibleBoundary(t *testing.T) {
	assistantID := pageEntryID(50)
	resultID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb99"
	facts := make([]harness.HistoryFact, 0, 52)
	for i := 0; i < 50; i++ {
		facts = append(facts, convInputFact(pageEntryID(i), "op-1"))
	}
	facts = append(facts, convAssistantFact(assistantID, "op-1", harness.AssistantEntry{
		SessionID: convSessionID, EntryID: assistantID, OperationID: "op-1",
		Status: model.OutputCompleted, Source: model.ModelRef{Provider: "prov", Model: "m"},
		Content:   []model.ContentPart{{Kind: model.PartText, Text: "with tools"}},
		ToolCalls: []harness.ToolCallRecord{{ID: "call-1", Ordinal: 0, Name: "read", ArgumentsBase64: convArgs(`{}`), ResultEntryID: resultID}},
	}))
	facts = append(facts, convResultFact(resultID, assistantID, "call-1", "success", "tool output", ""))

	snap := convSnapshot(facts)
	page1, err := projectHistoryPage(snap, nil)
	if err != nil {
		t.Fatalf("initial page: %v", err)
	}
	if len(page1.Items) != 50 {
		t.Fatalf("initial page = %d items, want exactly 50", len(page1.Items))
	}
	edge, err := page1.Items[49].AsAssistantItem()
	if err != nil {
		t.Fatalf("page edge item: %v", err)
	}
	if edge.ItemId != projectItemID(convSessionID, assistantID) ||
		len(edge.ToolCalls) != 1 || edge.ToolCalls[0].Status == nil ||
		*edge.ToolCalls[0].Status != protocol.ToolCallStatusSuccess ||
		edge.ToolCalls[0].Content == nil || *edge.ToolCalls[0].Content != "tool output" {
		t.Fatalf("page edge assistant = %+v, want the indivisible item with its attached result at the page boundary", edge)
	}
	if page1.OlderCursor == nil {
		t.Fatal("initial page produced no older cursor with one older item")
	}
	page2, err := projectHistoryPage(snap, page1.OlderCursor)
	if err != nil {
		t.Fatalf("older page: %v", err)
	}
	if len(page2.Items) != 1 {
		t.Fatalf("older page = %d items, want the single oldest input", len(page2.Items))
	}
	// The result entry never became an item of either page.
	for _, page := range []protocol.HistoryPage{page1, page2} {
		for i, item := range page.Items {
			if kind, _ := item.Discriminator(); kind == "assistant" {
				continue
			}
			data, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal item %d: %v", i, err)
			}
			if strings.Contains(string(data), resultID) {
				t.Fatalf("item %d wire = %s, want no standalone result item", i, data)
			}
		}
	}
}

func strPtr(s string) *string { return &s }

func quoteJSON(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// cursorBody encodes one raw cursor JSON body as its base64url form.
func cursorBody(t *testing.T, body string) *string {
	t.Helper()
	raw := base64.RawURLEncoding.EncodeToString([]byte(body))
	return &raw
}

func cursorJSON(sessionID, anchor, direction string) string {
	return fmt.Sprintf(`{"version":1,"session_id":%s,"anchor_item_id":%s,"direction":%s}`,
		quoteJSON(sessionID), quoteJSON(anchor), quoteJSON(direction))
}

// TestHistoryCursorStrictValidation proves the cursor decode's closed rule:
// only the exact four-member version-1 older document for this Session with
// a resolvable anchor passes, and every malformed sibling fails uniformly
// with harness.ErrInvalid.
func TestHistoryCursorStrictValidation(t *testing.T) {
	snap := pageSnapshot(3)
	valid := cursorBody(t, cursorJSON(convSessionID, wantPageItemID(1), "older"))
	if _, err := projectHistoryPage(snap, valid); err != nil {
		t.Fatalf("valid cursor rejected: %v", err)
	}

	rejections := map[string]*string{
		"empty cursor":      strPtr(""),
		"not base64":        strPtr("not a cursor!"),
		"unknown member":    cursorBody(t, `{"version":1,"session_id":`+quoteJSON(convSessionID)+`,"anchor_item_id":`+quoteJSON(wantPageItemID(1))+`,"direction":"older","extra":1}`),
		"trailing document": cursorBody(t, cursorJSON(convSessionID, wantPageItemID(1), "older")+`{"again":1}`),
		// The version, direction, and Session rows each keep an otherwise
		// valid anchor of the addressed Session: removing that one guard
		// would produce a page, so every row isolates its own rule.
		"wrong version":      cursorBody(t, `{"version":2,"session_id":`+quoteJSON(convSessionID)+`,"anchor_item_id":`+quoteJSON(wantPageItemID(1))+`,"direction":"older"}`),
		"wrong direction":    cursorBody(t, cursorJSON(convSessionID, wantPageItemID(1), "newer")),
		"empty anchor":       cursorBody(t, cursorJSON(convSessionID, "", "older")),
		"foreign session":    cursorBody(t, cursorJSON("ffffffffffffffffffffffffffffffff", wantPageItemID(1), "older")),
		"nonexistent anchor": cursorBody(t, cursorJSON(convSessionID, "missing", "older")),
	}
	for name, cursor := range rejections {
		if _, err := projectHistoryPage(snap, cursor); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("%s: projectHistoryPage = %v, want harness.ErrInvalid", name, err)
		}
	}

	// The one uniform slicing rule for a resolvable anchor with nothing
	// older: a present empty page with no next cursor.
	page, err := projectHistoryPage(snap, cursorBody(t, cursorJSON(convSessionID, wantPageItemID(0), "older")))
	if err != nil {
		t.Fatalf("oldest-anchor page: %v", err)
	}
	if page.Items == nil || len(page.Items) != 0 || page.OlderCursor != nil {
		t.Fatalf("oldest-anchor page = %+v, want the present empty page with no cursor", page)
	}
}

// --- the pure usage clocks ---

// usageRef is the fixture model identity every usage row reports under.
var usageRef = model.ModelRef{Provider: "prov", Model: "m"}

func usageFact(entryID string, kind harness.EntryKind, usage *harness.UsageCount) harness.HistoryFact {
	fact := harness.HistoryFact{EntryID: entryID, Sequence: 1, CommittedAt: convTime, Kind: kind}
	switch kind {
	case harness.EntryAssistant:
		fact.Assistant = &harness.AssistantEntry{
			SessionID: convSessionID, EntryID: entryID, Status: model.OutputCompleted,
			Source: usageRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "answer"}},
			Usage: usage,
		}
	case harness.EntryOperationSettlement:
		settlement := &harness.OperationSettlementEntry{
			SessionID: convSessionID, EntryID: entryID, Status: harness.OperationSuccess,
		}
		if usage != nil {
			ref := usageRef
			settlement.Model, settlement.Usage = &ref, usage
		}
		fact.Settlement = settlement
	case harness.EntryCompaction:
		fact.Compaction = &harness.CompactionEntry{
			SessionID: convSessionID, EntryID: entryID, Summary: "summary",
			Model: usageRef, Usage: usage,
		}
	}
	return fact
}

// TestUsedTokensPureClocks proves the display estimate's separate clock: the
// latest measured input+cached count strictly after the compaction boundary,
// nil usage transparent, compaction usage totals-only, no later measurement
// zero, and the exact signed sum — mixed signs stay exact and a sum beyond
// MaxInt64 stays positive-exact.
func TestUsedTokensPureClocks(t *testing.T) {
	cases := []struct {
		name  string
		facts []harness.HistoryFact
		want  string
	}{
		{"no measurement", nil, "0"},
		{
			"latest assistant wins",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: 100, CachedInputTokens: 10, OutputTokens: 5}),
				usageFact("e2", harness.EntryAssistant, &harness.UsageCount{InputTokens: 200, CachedInputTokens: 20, OutputTokens: 7}),
			},
			"220",
		},
		{
			"latest settlement wins",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: 100, CachedInputTokens: 10, OutputTokens: 5}),
				usageFact("e2", harness.EntryOperationSettlement, &harness.UsageCount{InputTokens: 300, CachedInputTokens: 20, OutputTokens: 7}),
			},
			"320",
		},
		{
			"nil usage transparent",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: 100, CachedInputTokens: 10, OutputTokens: 5}),
				usageFact("e2", harness.EntryAssistant, nil),
			},
			"110",
		},
		{
			"compaction resets to zero",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: 100, CachedInputTokens: 10, OutputTokens: 5}),
				usageFact("c1", harness.EntryCompaction, &harness.UsageCount{InputTokens: 9, CachedInputTokens: 9, OutputTokens: 9}),
				usageFact("e2", harness.EntryAssistant, nil),
			},
			"0",
		},
		{
			"boundary hides earlier measurement",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: 100, CachedInputTokens: 10, OutputTokens: 5}),
				usageFact("c1", harness.EntryCompaction, nil),
				usageFact("e2", harness.EntryAssistant, &harness.UsageCount{InputTokens: 50, CachedInputTokens: 5, OutputTokens: 3}),
			},
			"55",
		},
		{
			"settlement measured after boundary",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: 100, CachedInputTokens: 10, OutputTokens: 5}),
				usageFact("c1", harness.EntryCompaction, nil),
				usageFact("e2", harness.EntryAssistant, nil),
				usageFact("e3", harness.EntryOperationSettlement, &harness.UsageCount{InputTokens: 300, CachedInputTokens: 20, OutputTokens: 7}),
			},
			"320",
		},
		{
			"negative cached stays exact",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: 0, CachedInputTokens: -5, OutputTokens: 0}),
			},
			"-5",
		},
		{
			"mixed signs stay exact",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: 100, CachedInputTokens: -5, OutputTokens: 0}),
				usageFact("e2", harness.EntryOperationSettlement, &harness.UsageCount{InputTokens: -7, CachedInputTokens: 0, OutputTokens: 0}),
			},
			"-7",
		},
		{
			"negative input stays exact",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: -5, CachedInputTokens: 0, OutputTokens: 0}),
			},
			"-5",
		},
		{
			"positive extreme beyond MaxInt64",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: math.MaxInt64, CachedInputTokens: math.MaxInt64, OutputTokens: 0}),
			},
			"18446744073709551614",
		},
		{
			"negative total with later positive recovery",
			[]harness.HistoryFact{
				usageFact("e1", harness.EntryAssistant, &harness.UsageCount{InputTokens: -20, CachedInputTokens: 0, OutputTokens: 0}),
				usageFact("e2", harness.EntryAssistant, &harness.UsageCount{InputTokens: 15, CachedInputTokens: 40, OutputTokens: 0}),
			},
			"55",
		},
	}
	for _, tc := range cases {
		if got := projectUsedTokens(tc.facts, "c1"); got != tc.want {
			t.Fatalf("%s: used tokens = %q, want %q", tc.name, got, tc.want)
		}
	}

	// No boundary named: the whole history is the scan domain.
	if got := projectUsedTokens(cases[1].facts, ""); got != "220" {
		t.Fatalf("no-boundary scan = %q, want 220", got)
	}
}

// usageWindowConfig and usageWindowAgents assemble one captured
// configuration with a positive catalog window distinct from the fixtures'
// captured 4096, plus a modelless Agent type.
const (
	usageWindowConfig = `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"models":{"m":{"name":"M","context_window":8192}}}}}`
	usageWindowAgents = `{"solo":{"model":"prov/m","system_prompt":"simple"},"modelless":{"system_prompt":"simple"}}`
)

// windowSnapshot builds one typed snapshot with the given durably running
// current Operation, busy fact, and Agent selection.
func windowSnapshot(currentOp, agentType string, busy bool, ops ...harness.OperationRecord) harness.SessionSnapshot {
	snap := convSnapshot(nil)
	snap.Session.State.CurrentOperationID = currentOp
	snap.Session.State.CurrentAgentType = agentType
	snap.ExecutionBusy = busy
	snap.Operations = ops
	return snap
}

func windowOperation(operationID string, window int) harness.OperationRecord {
	return harness.OperationRecord{
		Admission: harness.OperationAdmission{
			OperationID: operationID,
			Execution:   harness.ExecutionCapture{Model: usageRef, ContextWindow: window},
		},
		State: harness.OperationCurrentState{Status: harness.OperationRunning},
	}
}

// TestSessionContextWindowPureClocks proves the window's two clocks: the
// durably running Operation's captured window while one is admitted — the
// busy fact and a retiring run alone are not an active Operation —
// otherwise the captured configuration's catalog window for the selected
// Agent type, with every unavailable resolution reporting 0.
func TestSessionContextWindowPureClocks(t *testing.T) {
	captured, err := assembleConfiguration(2, usageWindowConfig, usageWindowAgents, nil)
	if err != nil {
		t.Fatalf("assembleConfiguration: %v", err)
	}

	// The active Operation's capture wins over the distinct catalog window.
	active := windowSnapshot("op-1", "solo", true, windowOperation("op-1", 4096))
	if got := sessionContextWindow(active, captured); got != 4096 {
		t.Fatalf("active window = %d, want the captured 4096", got)
	}

	// The busy fact and a retiring run alone are not an active Operation:
	// with no durably running current Operation the idle clock applies.
	retiring := windowSnapshot("", "solo", true)
	if got := sessionContextWindow(retiring, captured); got != 8192 {
		t.Fatalf("retiring-run window = %d, want the captured catalog 8192", got)
	}

	idle := windowSnapshot("", "solo", false)
	if got := sessionContextWindow(idle, captured); got != 8192 {
		t.Fatalf("idle window = %d, want the catalog 8192", got)
	}
	if got := sessionContextWindow(windowSnapshot("", "modelless", false), captured); got != 0 {
		t.Fatalf("modelless window = %d, want 0", got)
	}
	if got := sessionContextWindow(windowSnapshot("", "unknown", false), captured); got != 0 {
		t.Fatalf("unknown type window = %d, want 0", got)
	}
}

// --- the live composed-Runtime rows ---

// The durable payload wire shapes: the envelope's model identity is an
// object, and every usage row carries all three signed counts.
type usageWireRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type usageWireModelUsage struct {
	Model usageWireRef       `json:"model"`
	Usage harness.UsageCount `json:"usage"`
}

type usageWireTotals struct {
	ByModel []usageWireModelUsage `json:"by_model"`
}

func usageWireTotalsFor(counts ...harness.UsageCount) usageWireTotals {
	var merged harness.UsageCount
	for _, c := range counts {
		merged.InputTokens += c.InputTokens
		merged.CachedInputTokens += c.CachedInputTokens
		merged.OutputTokens += c.OutputTokens
	}
	return usageWireTotals{ByModel: []usageWireModelUsage{{
		Model: usageWireRef{Provider: "prov", Model: "m"}, Usage: merged,
	}}}
}

// emptyUsageWireTotals is the present empty totals value.
func emptyUsageWireTotals() usageWireTotals {
	return usageWireTotals{ByModel: []usageWireModelUsage{}}
}

func derefUsage(u *harness.UsageCount) harness.UsageCount {
	if u == nil {
		return harness.UsageCount{}
	}
	return *u
}

// usageSeedSessionState is the mutable state section of one seeded usage
// fixture's Session register.
type usageSeedSessionState struct {
	Lifecycle          harness.SessionLifecycle `json:"lifecycle"`
	CurrentAgentType   string                   `json:"current_agent_type"`
	CurrentOperationID string                   `json:"current_operation_id,omitempty"`
	CompactionEntryID  string                   `json:"compaction_entry_id,omitempty"`
	Usage              usageWireTotals          `json:"usage"`
	LastActivity       time.Time                `json:"last_activity"`
}

type usageSeedSessionRegister struct {
	Identity harness.SessionIdentity `json:"identity"`
	State    usageSeedSessionState   `json:"state"`
}

// usageSeedSession inserts one open root Session register carrying the given
// usage totals — equal by construction to the seeded entries' contributions.
func usageSeedSession(t *testing.T, store harness.Storage, sessionID, currentOp, compactionID, agentType string, totals usageWireTotals) {
	t.Helper()
	now := time.Now().UTC()
	payload, err := json.Marshal(usageSeedSessionRegister{
		Identity: harness.SessionIdentity{SessionID: sessionID, Workspace: "/tmp/usage-works", CreatedAt: now},
		State: usageSeedSessionState{
			Lifecycle:          harness.LifecycleOpen,
			CurrentAgentType:   agentType,
			CurrentOperationID: currentOp,
			CompactionEntryID:  compactionID,
			Usage:              totals,
			LastActivity:       now,
		},
	})
	if err != nil {
		t.Fatalf("marshal session register: %v", err)
	}
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}, string(payload))
}

// usageSeedMessageOperation seeds one settled success message Operation: its
// input and assistant entries, its settlement entry carrying the optional
// usage, and its register whose state usage equals the entries' sum. It
// returns the assistant entry's identity.
func usageSeedMessageOperation(t *testing.T, store harness.Storage, sessionID, operationID string, assistantUsage, settlementUsage *harness.UsageCount) string {
	t.Helper()
	now := time.Now().UTC()
	inputID, assistantID, settlementID := newLifecycleID(t), newLifecycleID(t), newLifecycleID(t)
	lifecycleInsertEntry(t, store, sessionID, inputID, operationID, harness.EntryInput,
		compactRawInputEntry(sessionID, inputID, operationID, "question "+operationID))
	assistant, err := json.Marshal(struct {
		SessionID   string                   `json:"session_id"`
		EntryID     string                   `json:"entry_id"`
		OperationID string                   `json:"operation_id"`
		Status      string                   `json:"status"`
		Source      usageWireRef             `json:"source"`
		Content     json.RawMessage          `json:"content"`
		ToolCalls   []harness.ToolCallRecord `json:"tool_calls"`
		Usage       *harness.UsageCount      `json:"usage,omitempty"`
	}{
		SessionID: sessionID, EntryID: assistantID, OperationID: operationID,
		Status: "completed", Source: usageWireRef{Provider: "prov", Model: "m"},
		Content:   json.RawMessage(`[{"kind":"text","text":"answer ` + operationID + `"}]`),
		ToolCalls: []harness.ToolCallRecord{},
		Usage:     assistantUsage,
	})
	if err != nil {
		t.Fatalf("marshal assistant: %v", err)
	}
	settlementWire := struct {
		SessionID   string              `json:"session_id"`
		EntryID     string              `json:"entry_id"`
		OperationID string              `json:"operation_id"`
		Status      string              `json:"status"`
		Model       *usageWireRef       `json:"model,omitempty"`
		Usage       *harness.UsageCount `json:"usage,omitempty"`
	}{
		SessionID: sessionID, EntryID: settlementID, OperationID: operationID, Status: "success",
	}
	if settlementUsage != nil {
		settlementWire.Model = &usageWireRef{Provider: "prov", Model: "m"}
		settlementWire.Usage = settlementUsage
	}
	settlementData, err := json.Marshal(settlementWire)
	if err != nil {
		t.Fatalf("marshal settlement: %v", err)
	}
	lifecycleInsertEntry(t, store, sessionID, assistantID, operationID, harness.EntryAssistant, string(assistant))
	lifecycleInsertEntry(t, store, sessionID, settlementID, operationID, harness.EntryOperationSettlement, string(settlementData))

	stateUsage := emptyUsageWireTotals()
	if assistantUsage != nil || settlementUsage != nil {
		stateUsage = usageWireTotalsFor(derefUsage(assistantUsage), derefUsage(settlementUsage))
	}
	state, err := json.Marshal(struct {
		Status           harness.OperationState    `json:"status"`
		StartedAt        time.Time                 `json:"started_at"`
		SettledAt        time.Time                 `json:"settled_at"`
		PendingToolCalls []harness.PendingToolCall `json:"pending_tool_calls"`
		Usage            usageWireTotals           `json:"usage"`
		Terminal         harness.OperationTerminal `json:"terminal"`
	}{
		Status:           harness.OperationSuccess,
		StartedAt:        now,
		SettledAt:        now,
		PendingToolCalls: []harness.PendingToolCall{},
		Usage:            stateUsage,
		Terminal:         harness.OperationTerminal{SettlementEntry: harness.EntryRef{SessionID: sessionID, EntryID: settlementID}},
	})
	if err != nil {
		t.Fatalf("marshal operation state: %v", err)
	}
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID},
		fmt.Sprintf(`{"admission":%s,"state":%s}`, compactRawMessageAdmission(sessionID, operationID, inputID), state))
	return assistantID
}

// --- the live composed-Runtime rows ---

// wantUsageRow builds the one expected by-model usage row of the fixture
// model identity.
func wantUsageRow(input, cached, output string) protocol.UsageTotals {
	return protocol.UsageTotals{ByModel: []protocol.ModelUsage{{
		Model: usageRef.String(),
		Usage: protocol.UsageCount{InputTokens: input, CachedInputTokens: cached, OutputTokens: output},
	}}}
}

// TestHistoryPagingAppendStableAnchors proves, over the real seeded durable
// history, the three-surface paging contract: the newest-50 initial page,
// the anchored older page, and an append between page requests that leaves
// the anchored older page's item identities stable while the returned
// revision is the current authoritative one (the newest-50 window itself
// shifts by the appended items).
func TestHistoryPagingAppendStableAnchors(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, _ := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		session := newLifecycleID(t)
		usageSeedSession(t, store, session, "", "", "solo", emptyUsageWireTotals())
		for i := 1; i <= 20; i++ { // the existing no-usage seeder: zero register totals stay coherent
			compactRawSuccessOperation(t, store, session, fmt.Sprintf("op-%d", i), fmt.Sprintf("question op-%d", i), fmt.Sprintf("answer op-%d", i))
		}

		page1, err := r.getHistory(context.Background(), session, nil)
		if err != nil {
			t.Fatalf("initial history: %v", err)
		}
		if len(page1.Items) != 50 || page1.OlderCursor == nil {
			t.Fatalf("initial page = %d items (cursor %v), want exactly 50 with an older cursor", len(page1.Items), page1.OlderCursor)
		}
		wantPage1 := strings.Join(pageItemIDs(t, page1), ",")
		page2, err := r.getHistory(context.Background(), session, page1.OlderCursor)
		if err != nil {
			t.Fatalf("older history: %v", err)
		}
		if len(page2.Items) != 10 || page2.OlderCursor != nil {
			t.Fatalf("older page = %d items (cursor %v), want the remaining 10 with no cursor", len(page2.Items), page2.OlderCursor)
		}
		wantPage2 := strings.Join(pageItemIDs(t, page2), ",")
		firstRevision := page1.SessionRevision

		// A newer append between page requests: the client's older cursor
		// stays valid, its page unchanged, and the read returns the current
		// authoritative revision with the stable item identities.
		submitConvergedThroughRuntime(t, r, session, "op-21", "late turn", harness.OperationSuccess)
		awaitIdleSession(t, r, session)

		page1After, err := r.getHistory(context.Background(), session, nil)
		if err != nil {
			t.Fatalf("initial history after append: %v", err)
		}
		if len(page1After.Items) != 50 || page1After.OlderCursor == nil {
			t.Fatalf("post-append initial page = %d items, want exactly 50", len(page1After.Items))
		}
		afterIDs := pageItemIDs(t, page1After)
		oldIDs := strings.Split(wantPage1, ",")
		for i, id := range oldIDs {
			// Exactly the three appended items aged out of the newest-50
			// window; every other stable identity remains.
			if agedOut := i < 3; slices.Contains(afterIDs, id) == agedOut {
				t.Fatalf("post-append page item %d (%q) presence = %v, want the stable window shift by 3", i, id, !agedOut)
			}
		}
		if page1After.SessionRevision.DurableRevision == firstRevision.DurableRevision {
			t.Fatalf("post-append revision = %+v, want the advanced current revision (was %+v)", page1After.SessionRevision, firstRevision)
		}

		page2After, err := r.getHistory(context.Background(), session, page1.OlderCursor)
		if err != nil {
			t.Fatalf("older history after append: %v", err)
		}
		if got := strings.Join(pageItemIDs(t, page2After), ","); got != wantPage2 {
			t.Fatalf("post-append older page = %s, want the unchanged %s", got, wantPage2)
		}
		if page2After.SessionRevision.DurableRevision == firstRevision.DurableRevision {
			t.Fatalf("older page returned the stale client revision %+v, want the current one", page2After.SessionRevision)
		}
		if page2After.SessionRevision.DurableRevision != page1After.SessionRevision.DurableRevision {
			t.Fatalf("older page revision = %+v, want the same current revision as the fresh initial page %+v", page2After.SessionRevision, page1After.SessionRevision)
		}
	})
}

// TestHistoryReadsArchivedDeletedSessions proves archived Sessions stay
// fully readable and deleted Sessions fail every read with the typed
// not-found.
func TestHistoryReadsArchivedDeletedSessions(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, _ := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		archived := projectionSession(t, r, "/tmp/paging-archived", "solo").Identity.SessionID
		deleted := projectionSession(t, r, "/tmp/paging-deleted", "solo").Identity.SessionID
		if err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, archived)
			return err
		}); err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, deleted)
			return err
		}); err != nil {
			t.Fatalf("ArchiveSession(deleted): %v", err)
		}
		if err := r.deleteSession(context.Background(), deleted); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}

		archivedReads := []func() error{
			func() error { _, err := r.getHistory(context.Background(), archived, nil); return err },
			func() error { _, err := r.getPending(context.Background(), archived); return err },
			func() error { _, err := r.getUsage(context.Background(), archived); return err },
			func() error { _, err := r.buildHydration(context.Background(), archived); return err },
		}
		for i, read := range archivedReads {
			if err := read(); err != nil {
				t.Fatalf("archived read %d: %v", i, err)
			}
		}
		deletedReads := []func() error{
			func() error { _, err := r.getHistory(context.Background(), deleted, nil); return err },
			func() error { _, err := r.getPending(context.Background(), deleted); return err },
			func() error { _, err := r.getUsage(context.Background(), deleted); return err },
			func() error { _, err := r.buildHydration(context.Background(), deleted); return err },
			func() error { _, err := r.resolveForkBoundary(context.Background(), deleted, "any"); return err },
		}
		for i, read := range deletedReads {
			if err := read(); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("deleted read %d = %v, want harness.ErrNotFound", i, err)
			}
		}
	})
}

// TestHistoryCursorRejectionsThroughRuntime proves the read body propagates
// the cursor's uniform invalid rule against the live owner; the version,
// direction, and Session rows each carry an otherwise valid anchor of the
// addressed Session, so every rejection isolates its own guard.
func TestHistoryCursorRejectionsThroughRuntime(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, _ := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		session := projectionSession(t, r, "/tmp/paging-cursors", "solo").Identity.SessionID
		submitConvergedThroughRuntime(t, r, session, "op-1", "the anchor turn", harness.OperationSuccess)
		awaitIdleSession(t, r, session)
		anchor := projectItemID(session, inputEntryID(t, r, session, "op-1"))
		rejected := []*string{
			strPtr(""),
			strPtr("!!!!"),
			cursorBody(t, `{"version":2,"session_id":`+quoteJSON(session)+`,"anchor_item_id":`+quoteJSON(anchor)+`,"direction":"older"}`),
			cursorBody(t, cursorJSON(session, anchor, "newer")),
			cursorBody(t, cursorJSON("ffffffffffffffffffffffffffffffff", anchor, "older")),
			cursorBody(t, cursorJSON(session, "missing", "older")),
		}
		for i, cursor := range rejected {
			if _, err := r.getHistory(context.Background(), session, cursor); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("cursor rejection %d = %v, want harness.ErrInvalid", i, err)
			}
		}
	})
}

// TestForkBoundaryResolution proves the boundary item resolution against the
// one snapshot's committed user-origin inputs: the valid user input resolves
// to its private entry identity, while a foreign Session's item, a
// non-user item, and a nonexistent item fail uniformly with
// harness.ErrInvalid.
func TestForkBoundaryResolution(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, _ := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		session := projectionSession(t, r, "/tmp/paging-fork", "solo").Identity.SessionID
		submitConvergedThroughRuntime(t, r, session, "op-1", "first turn", harness.OperationSuccess)
		awaitIdleSession(t, r, session)
		submitConvergedThroughRuntime(t, r, session, "op-2", "second turn", harness.OperationSuccess)
		awaitIdleSession(t, r, session)

		entryID := inputEntryID(t, r, session, "op-1")
		boundary, err := r.resolveForkBoundary(context.Background(), session, projectItemID(session, entryID))
		if err != nil {
			t.Fatalf("resolveForkBoundary: %v", err)
		}
		if boundary != entryID {
			t.Fatalf("resolved boundary = %q, want the private entry identity %q", boundary, entryID)
		}

		// The user input of a different Session's namespace never resolves.
		foreign := projectItemID("ffffffffffffffffffffffffffffffff", entryID)
		if _, err := r.resolveForkBoundary(context.Background(), session, foreign); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("foreign boundary = %v, want harness.ErrInvalid", err)
		}
		// A committed non-user item never resolves: find the first turn's
		// assistant item identity.
		snap := snapshotThroughRuntime(t, r, session)
		var assistantItem string
		for _, fact := range snap.Facts {
			if fact.Kind == harness.EntryAssistant {
				assistantItem = projectItemID(session, fact.EntryID)
				break
			}
		}
		if _, err := r.resolveForkBoundary(context.Background(), session, assistantItem); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("assistant boundary = %v, want harness.ErrInvalid", err)
		}
		if _, err := r.resolveForkBoundary(context.Background(), session, "no-such-item"); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("nonexistent boundary = %v, want harness.ErrInvalid", err)
		}
	})
}

// TestHydrationPendingFIFOsAndActiveOperation proves the pending read and
// the hydration's pending membership and active pointer from one snapshot:
// the steering and queued FIFOs project in order with their contents, the
// durably running Operation is the active pointer, and the hydration's
// members equal the separate producers' reads at the same stable state.
func TestHydrationPendingFIFOsAndActiveOperation(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		session := projectionSession(t, r, "/tmp/paging-pending", "solo").Identity.SessionID

		gate := make(chan struct{})
		e.prep.modelGate = gate
		release := sync.OnceFunc(func() { close(gate) })
		defer release() // LIFO: the gate releases before the owner close joins

		submitThroughRuntime(t, r, session, "op-1", "running")
		awaitModelArrival(t, e)
		// A regular submit while running rides the steering buffer; queued
		// submissions fill the queued FIFO in order.
		if err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			res, err := h.Submit(ctx, harness.SubmitRequest{
				SessionID: session, OperationID: "op-s1", Origin: harness.InputOriginUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: "steer me"}}, Mode: harness.MessageModeRegular,
			})
			if err != nil {
				return err
			}
			if res.Disposition != harness.DispositionSteering {
				return fmt.Errorf("steering submit = %s, want steering", res.Disposition)
			}
			return nil
		}); err != nil {
			t.Fatalf("steering Submit: %v", err)
		}
		submitQueuedThroughRuntime(t, r, session, "op-q1", "queued one")
		submitQueuedThroughRuntime(t, r, session, "op-q2", "queued two")

		pending, err := r.getPending(context.Background(), session)
		if err != nil {
			t.Fatalf("getPending: %v", err)
		}
		if len(pending.Pending.Steering) != 1 || len(pending.Pending.Queued) != 2 {
			t.Fatalf("pending = %+v, want one steering and two queued members", pending.Pending)
		}
		steerText := pendingText(t, pending.Pending.Steering[0])
		if pending.Pending.Steering[0].OperationId != "op-s1" || steerText != "steer me" ||
			pending.Pending.Steering[0].Origin != protocol.InputOriginUser {
			t.Fatalf("steering member = %+v (%q), want op-s1's user text", pending.Pending.Steering[0], steerText)
		}
		if pending.Pending.Queued[0].OperationId != "op-q1" || pendingText(t, pending.Pending.Queued[0]) != "queued one" ||
			pending.Pending.Queued[1].OperationId != "op-q2" || pendingText(t, pending.Pending.Queued[1]) != "queued two" {
			t.Fatalf("queued members = %+v, want the FIFO order q1 then q2", pending.Pending.Queued)
		}

		hydration, err := r.buildHydration(context.Background(), session)
		if err != nil {
			t.Fatalf("buildHydration: %v", err)
		}
		if !reflect.DeepEqual(hydration.Pending, pending.Pending) {
			t.Fatalf("hydration pending = %+v, want the pending read's %+v", hydration.Pending, pending.Pending)
		}
		if hydration.ActiveOperation == nil || hydration.ActiveOperation.OperationId != "op-1" ||
			hydration.ActiveOperation.Status != protocol.OperationStatusRunning {
			t.Fatalf("active operation = %+v, want the durably running op-1", hydration.ActiveOperation)
		}
		header, err := r.getSession(context.Background(), session)
		if err != nil {
			t.Fatalf("getSession: %v", err)
		}
		if !reflect.DeepEqual(hydration.Session, header) {
			t.Fatalf("hydration session = %+v, want the header read %+v", hydration.Session, header)
		}
		data, err := json.Marshal(hydration)
		if err != nil {
			t.Fatalf("marshal hydration: %v", err)
		}
		for _, member := range []string{"prompt-solo", "system_prompt", "result_entry_id"} {
			if strings.Contains(string(data), member) {
				t.Fatalf("hydration wire = %s, want no internal %q representation", data, member)
			}
		}
	})
}

// pendingText reads one projected pending member's first text part.
func pendingText(t *testing.T, member protocol.PendingInput) string {
	t.Helper()
	if len(member.Content) == 0 {
		return ""
	}
	part, err := member.Content[0].AsTextPart()
	if err != nil {
		t.Fatalf("pending part: %v", err)
	}
	return part.Text
}

// TestHydrationRootChildCoherence proves the hydration equality oracle
// across lineage: a running child Session hydrates as an ordinary Session
// with its own active Operation and job membership, the parent's hydration
// carries the child background member that vanishes when the child settles,
// and every hydration member equals the separate single-snapshot producers'
// reads at the same stable state. The delivered runtime-origin completion
// input is committed non-user history: it never resolves as a fork boundary.
func TestHydrationRootChildCoherence(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		stopper := newLifecycleStopper(store)
		bg := openBackgroundLifecycle(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		stopper.arm(bg.r.harness)

		root := bg.session("hyd-root")
		arrived := make(chan struct{}, 1)
		park := make(chan struct{})
		parkOnce := sync.OnceFunc(func() { close(park) })
		defer parkOnce() // LIFO: the gate releases before the owner close joins
		bg.prep.setAgentScript("worker", &lifecycleScript{
			model: func(mctx context.Context, _ string, attempt int, _ model.Request) (model.Stream, error) {
				if attempt == 1 {
					select {
					case arrived <- struct{}{}:
					default:
					}
					select {
					case <-park:
					case <-mctx.Done():
						return nil, mctx.Err()
					}
				}
				return lifecycleTextTurn("child finished"), nil
			},
		})
		child := bg.launchChild(root, "child work", "child-op-1", 1)
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the child's model effect never arrived")
		}
		completionID := bg.startJob(child, "cc000003")

		// The running child hydrates as an ordinary Session: its own header,
		// its active Operation, its conversation of one input, and its live
		// job member.
		childHydration, err := bg.r.buildHydration(context.Background(), child)
		if err != nil {
			t.Fatalf("buildHydration(child): %v", err)
		}
		if childHydration.Session.SessionId != child || childHydration.ActiveOperation == nil ||
			childHydration.ActiveOperation.OperationId != "child-op-1" ||
			childHydration.ActiveOperation.Status != protocol.OperationStatusRunning {
			t.Fatalf("child hydration = %+v active %+v, want the ordinary running child Session", childHydration.Session, childHydration.ActiveOperation)
		}
		if len(childHydration.Conversation.Items) != 1 || len(childHydration.Operations) != 1 {
			t.Fatalf("child hydration = %d items %d operations, want one item and one Operation", len(childHydration.Conversation.Items), len(childHydration.Operations))
		}
		if len(childHydration.Background) != 1 || childHydration.Background[0].Id != "cc000003" {
			t.Fatalf("child background = %+v, want the live job member", childHydration.Background)
		}

		// The parent carries the live child member.
		rootHydration, err := bg.r.buildHydration(context.Background(), root)
		if err != nil {
			t.Fatalf("buildHydration(root): %v", err)
		}
		if len(rootHydration.Background) != 1 || rootHydration.Background[0].Id != child {
			t.Fatalf("root background = %+v, want the live child member", rootHydration.Background)
		}

		// The child settles; its parent member finishes and the completion
		// input is delivered to the parent's conversation.
		parkOnce()
		awaitOperation(t, bg.r, child, "child-op-1", harness.OperationSuccess)
		awaitIdleSession(t, bg.r, child)
		stopper.addDelivery(child, completionID, "job report")
		if err := bg.stop(child); err != nil {
			t.Fatalf("Stop(child): %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			rootHydration, err = bg.r.buildHydration(context.Background(), root)
			if err != nil {
				t.Fatalf("buildHydration(root) after completion: %v", err)
			}
			if hasRuntimeOriginInput(t, rootHydration.Conversation.Items, "child finished") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("root conversation = %s, want the runtime-origin completion input", itemKinds(t, rootHydration.Conversation.Items))
			}
			time.Sleep(5 * time.Millisecond)
		}
		if len(rootHydration.Background) != 0 {
			t.Fatalf("root background after completion = %+v, want the finished member gone", rootHydration.Background)
		}

		// The delivered runtime-origin input is committed non-user history:
		// its item identity never resolves as a fork boundary.
		snap := snapshotThroughRuntime(t, bg.r, root)
		for _, fact := range snap.Facts {
			if fact.Kind == harness.EntryInput && fact.Input.Origin == harness.InputOriginRuntime {
				if _, err := bg.r.resolveForkBoundary(context.Background(), root, projectItemID(root, fact.EntryID)); !errors.Is(err, harness.ErrInvalid) {
					t.Fatalf("runtime-origin boundary = %v, want harness.ErrInvalid", err)
				}
			}
		}

		// The equality oracle at the settled stable state: every hydration
		// member equals the separate producers' reads of the same snapshot.
		childHeader, err := bg.r.getSession(context.Background(), child)
		if err != nil {
			t.Fatalf("getSession(child): %v", err)
		}
		childPage, err := bg.r.getHistory(context.Background(), child, nil)
		if err != nil {
			t.Fatalf("getHistory(child): %v", err)
		}
		childPending, err := bg.r.getPending(context.Background(), child)
		if err != nil {
			t.Fatalf("getPending(child): %v", err)
		}
		childUsage, err := bg.r.getUsage(context.Background(), child)
		if err != nil {
			t.Fatalf("getUsage(child): %v", err)
		}
		childHydration, err = bg.r.buildHydration(context.Background(), child)
		if err != nil {
			t.Fatalf("buildHydration(child) settled: %v", err)
		}
		if !reflect.DeepEqual(childHydration.Session, childHeader) ||
			childHydration.SessionRevision != childPage.SessionRevision ||
			!reflect.DeepEqual(childHydration.Conversation.Items, childPage.Items) ||
			!reflect.DeepEqual(childHydration.Conversation.OlderCursor, childPage.OlderCursor) ||
			!reflect.DeepEqual(childHydration.Pending, childPending.Pending) ||
			!reflect.DeepEqual(childHydration.Usage, childUsage.Usage) ||
			childHydration.ActiveOperation != nil {
			t.Fatalf("settled child hydration disagrees with the separate producers: %+v", childHydration)
		}
	})
}

// TestUsageReadsDistinctWindowsAndClocks proves, over real seeded durable
// histories, every separate usage clock of the plan's row: register totals
// independent of the display estimate, the compaction reset, the idle
// catalog window across a reload to a distinct positive window, the
// no-model and deleted-model windows of zero retaining their estimates, and
// the active Operation's captured window surviving the reload.
func TestUsageReadsDistinctWindowsAndClocks(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ctx := context.Background()

		// The measured session: two pre-compaction turns and one after, with
		// register totals equal to the entries.
		measured := newLifecycleID(t)
		compactionID := newLifecycleID(t)
		usageSeedSession(t, store, measured, "", compactionID, "solo",
			usageWireTotalsFor(
				harness.UsageCount{InputTokens: 100, CachedInputTokens: 10, OutputTokens: 5},
				harness.UsageCount{InputTokens: 300, CachedInputTokens: 20, OutputTokens: 7},
				harness.UsageCount{InputTokens: 50, CachedInputTokens: 5, OutputTokens: 3}))
		usageSeedMessageOperation(t, store, measured, "m-op-1", &harness.UsageCount{InputTokens: 100, CachedInputTokens: 10, OutputTokens: 5}, nil)
		op2Assistant := usageSeedMessageOperation(t, store, measured, "m-op-2", nil, &harness.UsageCount{InputTokens: 300, CachedInputTokens: 20, OutputTokens: 7})
		lifecycleInsertEntry(t, store, measured, compactionID, "m-op-2", harness.EntryCompaction,
			compactRawCompactionEntry(measured, compactionID, "m-op-2", "seeded summary", op2Assistant))
		usageSeedMessageOperation(t, store, measured, "m-op-3", &harness.UsageCount{InputTokens: 50, CachedInputTokens: 5, OutputTokens: 3}, nil)

		// The reset session: measured usage only before its compaction.
		reset := newLifecycleID(t)
		resetCompaction := newLifecycleID(t)
		usageSeedSession(t, store, reset, "", resetCompaction, "solo",
			usageWireTotalsFor(harness.UsageCount{InputTokens: 70, CachedInputTokens: 7, OutputTokens: 2}))
		resetAssistant := usageSeedMessageOperation(t, store, reset, "r-op-1", &harness.UsageCount{InputTokens: 70, CachedInputTokens: 7, OutputTokens: 2}, nil)
		lifecycleInsertEntry(t, store, reset, resetCompaction, "r-op-1", harness.EntryCompaction,
			compactRawCompactionEntry(reset, resetCompaction, "r-op-1", "reset summary", resetAssistant))

		// The busy session: one durably running Operation whose capture
		// window is the fixture's 4096.
		busy := newLifecycleID(t)
		busyInput := newLifecycleID(t)
		usageSeedSession(t, store, busy, "b-op-1", "", "solo", emptyUsageWireTotals())
		lifecycleInsertEntry(t, store, busy, busyInput, "b-op-1", harness.EntryInput,
			compactRawInputEntry(busy, busyInput, "b-op-1", "running"))
		lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: busy, Kind: harness.RegisterOperation, OperationID: "b-op-1"},
			compactRawOperationRegister(compactRawMessageAdmission(busy, "b-op-1", busyInput), compactRawRunningModelState(time.Now().UTC().Format(time.RFC3339Nano), newLifecycleID(t))))

		// A modelless Session: the Agent type resolves but selects no model.
		modelless := newLifecycleID(t)
		usageSeedSession(t, store, modelless, "", "", "modelless", emptyUsageWireTotals())

		usageRow := func(t *testing.T, sessionID string) protocol.UsageSnapshot {
			t.Helper()
			snapshot, err := r.getUsage(ctx, sessionID)
			if err != nil {
				t.Fatalf("getUsage(%s): %v", sessionID, err)
			}
			return snapshot
		}

		// Generation 1: the fixture catalog's 4096 window for idle reads,
		// the captured window for the busy read.
		got := usageRow(t, measured)
		if !reflect.DeepEqual(got.Usage.Totals, wantUsageRow("450", "35", "15")) {
			t.Fatalf("measured totals = %+v, want the register totals 450/35/15", got.Usage.Totals)
		}
		if got.Usage.Context.UsedTokens != "55" || got.Usage.Context.ContextWindow != 4096 {
			t.Fatalf("measured context = %+v, want the estimate 55 under the 4096 window", got.Usage.Context)
		}
		if got.ConfigurationRevision.Generation != "1" {
			t.Fatalf("configuration revision = %+v, want generation 1", got.ConfigurationRevision)
		}
		resetRow := usageRow(t, reset)
		if resetRow.Usage.Context.UsedTokens != "0" || resetRow.Usage.Context.ContextWindow != 4096 ||
			!reflect.DeepEqual(resetRow.Usage.Totals, wantUsageRow("70", "7", "2")) {
			t.Fatalf("reset usage = %+v, want estimate 0 with the independent 70/7/2 totals", resetRow.Usage)
		}
		busyRow := usageRow(t, busy)
		if busyRow.Usage.Context.ContextWindow != 4096 {
			t.Fatalf("busy window = %d, want the captured 4096", busyRow.Usage.Context.ContextWindow)
		}
		if _, err := r.getUsage(ctx, modelless); err != nil {
			t.Fatalf("getUsage(modelless): %v", err)
		}

		// Reload to a distinct positive catalog window plus the modelless
		// Agent type: the idle reads report the new selection while the
		// busy read keeps its capture, and every read names the captured
		// generation.
		writeServiceFile(t, e.configPath, `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":8192}}}}}`)
		writeServiceFile(t, agents.PathForConfig(e.configPath), usageWindowAgents)
		if _, err := r.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		got = usageRow(t, measured)
		if got.Usage.Context.ContextWindow != 8192 || got.Usage.Context.UsedTokens != "55" || got.ConfigurationRevision.Generation != "2" {
			t.Fatalf("reloaded idle usage = %+v rev %+v, want estimate 55 under 8192 at generation 2", got.Usage, got.ConfigurationRevision)
		}
		busyRow = usageRow(t, busy)
		if busyRow.Usage.Context.ContextWindow != 4096 || busyRow.ConfigurationRevision.Generation != "2" {
			t.Fatalf("reloaded busy usage = %+v rev %+v, want the active captured 4096 at generation 2", busyRow.Usage, busyRow.ConfigurationRevision)
		}
		// The same running Session's hydration: the separately captured
		// configuration revision and the retained captured window ride the
		// one snapshot producer.
		busyHydration, err := r.buildHydration(ctx, busy)
		if err != nil {
			t.Fatalf("buildHydration(busy): %v", err)
		}
		if busyHydration.ConfigurationRevision.Generation != "2" || busyHydration.Usage.Context.ContextWindow != 4096 ||
			busyHydration.ActiveOperation == nil || busyHydration.ActiveOperation.OperationId != "b-op-1" {
			t.Fatalf("reloaded busy hydration = rev %+v context %+v active %+v, want generation 2, the captured 4096 window, and the running b-op-1",
				busyHydration.ConfigurationRevision, busyHydration.Usage.Context, busyHydration.ActiveOperation)
		}
		if row := usageRow(t, modelless); row.Usage.Context.ContextWindow != 0 || row.ConfigurationRevision.Generation != "2" {
			t.Fatalf("modelless usage = %+v, want window 0 at generation 2", row.Usage)
		}

		// Deleting the model from the catalog: the idle window is 0 while
		// the estimate and totals stay independent.
		writeServiceFile(t, e.configPath, `{"providers":{}}`)
		if _, err := r.Reload(ctx); err != nil {
			t.Fatalf("Reload after deletion: %v", err)
		}
		got = usageRow(t, measured)
		if got.Usage.Context.ContextWindow != 0 || got.Usage.Context.UsedTokens != "55" ||
			!reflect.DeepEqual(got.Usage.Totals, wantUsageRow("450", "35", "15")) || got.ConfigurationRevision.Generation != "3" {
			t.Fatalf("deleted-model usage = %+v rev %+v, want window 0 with the retained estimate and totals at generation 3", got.Usage, got.ConfigurationRevision)
		}
	})
}

// TestProjectionReadsClosedAndCanceledCallers proves the new reads hold the
// admission gate: closed owners reject with ErrClosed and canceled callers
// with their own context error.
func TestProjectionReadsClosedAndCanceledCallers(t *testing.T) {
	r, _ := openProjectionRuntime(t, storage.NewMemory())
	defer closeProjectionRuntime(r)
	const sessionID = "0123456789abcdef0123456789abcdef"
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed := []func() error{
		func() error { _, err := r.getHistory(context.Background(), sessionID, nil); return err },
		func() error { _, err := r.getPending(context.Background(), sessionID); return err },
		func() error { _, err := r.getUsage(context.Background(), sessionID); return err },
		func() error { _, err := r.buildHydration(context.Background(), sessionID); return err },
		func() error { _, err := r.resolveForkBoundary(context.Background(), sessionID, "x"); return err },
	}
	for i, call := range closed {
		if err := call(); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed caller %d = %v, want ErrClosed", i, err)
		}
	}

	r2, _ := openProjectionRuntime(t, storage.NewMemory())
	defer closeProjectionRuntime(r2)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceledCalls := []func() error{
		func() error { _, err := r2.getHistory(canceled, sessionID, nil); return err },
		func() error { _, err := r2.getPending(canceled, sessionID); return err },
		func() error { _, err := r2.getUsage(canceled, sessionID); return err },
		func() error { _, err := r2.buildHydration(canceled, sessionID); return err },
		func() error { _, err := r2.resolveForkBoundary(canceled, sessionID, "x"); return err },
	}
	for i, call := range canceledCalls {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller %d = %v, want context.Canceled", i, err)
		}
	}
}

// TestHydrationAndPageReturnedValueOwnership proves a returned page or
// hydration is a caller copy: the pre-mutation expectation stays immutable,
// caller mutation cannot reach the owner, and the next read is unchanged.
func TestHydrationAndPageReturnedValueOwnership(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		session := projectionSession(t, r, "/tmp/paging-owned", "solo").Identity.SessionID
		submitConvergedThroughRuntime(t, r, session, "op-1", "work", harness.OperationSuccess)
		e.prep.awaitCleanups(1)

		page, err := r.getHistory(context.Background(), session, nil)
		if err != nil {
			t.Fatalf("getHistory: %v", err)
		}
		wantPage, err := json.Marshal(page)
		if err != nil {
			t.Fatalf("marshal page: %v", err)
		}
		page.Items = append(page.Items, protocol.ConversationItem{})
		page.Items[0] = protocol.ConversationItem{}
		reread, err := r.getHistory(context.Background(), session, nil)
		if err != nil {
			t.Fatalf("getHistory re-read: %v", err)
		}
		gotPage, err := json.Marshal(reread)
		if err != nil {
			t.Fatalf("marshal re-read page: %v", err)
		}
		if string(gotPage) != string(wantPage) {
			t.Fatalf("re-read page = %s, want the immutable %s", gotPage, wantPage)
		}

		hydration, err := r.buildHydration(context.Background(), session)
		if err != nil {
			t.Fatalf("buildHydration: %v", err)
		}
		wantHydration, err := json.Marshal(hydration)
		if err != nil {
			t.Fatalf("marshal hydration: %v", err)
		}
		hydration.Session.AgentType = "mutated"
		hydration.Conversation.Items = nil
		hydration.Pending.Steering = append(hydration.Pending.Steering, protocol.PendingInput{OperationId: "mutated"})
		hydration.Operations[0].OperationId = "mutated"
		hydration.Background = append(hydration.Background, protocol.BackgroundMember{Id: "mutated"})
		rereadHydration, err := r.buildHydration(context.Background(), session)
		if err != nil {
			t.Fatalf("buildHydration re-read: %v", err)
		}
		gotHydration, err := json.Marshal(rereadHydration)
		if err != nil {
			t.Fatalf("marshal re-read hydration: %v", err)
		}
		if string(gotHydration) != string(wantHydration) {
			t.Fatalf("re-read hydration = %s, want the immutable %s", gotHydration, wantHydration)
		}
	})
}
