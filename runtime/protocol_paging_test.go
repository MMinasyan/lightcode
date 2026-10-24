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
	"github.com/MMinasyan/lightcode/internal/testassert"
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

// pageHeaders builds one sequence-ordered value-metadata fixture of n input
// entries, each of which forms exactly one page item.
func pageHeaders(n int) []harness.HistoryEntryHeader {
	headers := make([]harness.HistoryEntryHeader, 0, n)
	for i := 0; i < n; i++ {
		headers = append(headers, harness.HistoryEntryHeader{
			EntryID:     pageEntryID(i),
			OperationID: "op-1",
			Kind:        harness.EntryInput,
			InputOrigin: harness.InputOriginUser,
		})
	}
	return headers
}

// wireItemID reads one projected wire item's stable identity through the
// generated union accessors: the producer keeps each source-known identity
// alongside its item, and the tests verify the emitted wire identity.
func wireItemID(t *testing.T, item protocol.ConversationItem) string {
	t.Helper()
	discriminator, err := item.Discriminator()
	testassert.NoError(t, err)
	switch discriminator {
	case "input":
		v, err := item.AsInputItem()
		testassert.NoError(t, err)
		return v.ItemId
	case "assistant":
		v, err := item.AsAssistantItem()
		testassert.NoError(t, err)
		return v.ItemId
	case "signal":
		v, err := item.AsSignalItem()
		testassert.NoError(t, err)
		return v.ItemId
	case "compaction":
		v, err := item.AsCompactionItem()
		testassert.NoError(t, err)
		return v.ItemId
	case "operation_end":
		v, err := item.AsOperationEndItem()
		testassert.NoError(t, err)
		return v.ItemId
	default:
		t.Fatalf("conversation item kind %q is outside the closed set", discriminator)
		return ""
	}
}

// pageItemIDs reads one page's item identities in order.
func pageItemIDs(t *testing.T, page protocol.HistoryPage) []string {
	t.Helper()
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, wireItemID(t, item))
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
	headers := pageHeaders(120)

	page1, older1, err := selectHistoryPage(convSessionID, headers, nil)
	testassert.NoError(t, err)
	testassert.Equal(t, len(page1), 50)
	for i, entryID := range page1 {
		{
			id := projectItemID(convSessionID, entryID)
			testassert.Require(t, id == wantPageItemID(70+i), "initial item %d = %q, want the ascending item %d", i, id, 70+i)
		}
	}
	testassert.Require(t, older1 != nil, "initial page produced no older cursor with 70 older items")
	cursor1, err := decodeHistoryCursor(*older1, convSessionID)
	testassert.NoError(t, err)
	testassert.Require(t, cursor1.SessionID == convSessionID && cursor1.AnchorItemID == wantPageItemID(70), "older cursor = %+v, want this session anchored at item 70", cursor1)

	page2, older2, err := selectHistoryPage(convSessionID, headers, older1)
	testassert.NoError(t, err)
	testassert.Equal(t, len(page2), 50)
	for i, entryID := range page2 {
		{
			id := projectItemID(convSessionID, entryID)
			testassert.Require(t, id == wantPageItemID(20+i), "second page item %d = %q, want item %d", i, id, 20+i)
		}
	}
	for _, entryID := range page2 { // strictly before the anchor
		testassert.Require(t, projectItemID(convSessionID, entryID) != cursor1.AnchorItemID, "second page contains its own anchor %q", entryID)
	}
	testassert.Require(t, older2 != nil, "second page produced no older cursor with 20 older items")
	cursor2, err := decodeHistoryCursor(*older2, convSessionID)
	testassert.NoError(t, err)
	testassert.Require(t, cursor2.AnchorItemID == wantPageItemID(20), "second cursor anchor = %q, want item 20", cursor2.AnchorItemID)

	page3, older3, err := selectHistoryPage(convSessionID, headers, older2)
	testassert.NoError(t, err)
	testassert.Equal(t, len(page3), 20)
	for i, entryID := range page3 {
		{
			id := projectItemID(convSessionID, entryID)
			testassert.Require(t, id == wantPageItemID(i), "third page item %d = %q, want item %d", i, id, i)
		}
	}
	testassert.Require(t, older3 == nil, "third page produced an older cursor %v with nothing older", older3)
}

// TestHistoryPagePureIndivisibleBoundary proves the page boundary can never
// split an assistant from its tool result: the assistant forms one item at
// the page's edge, the terminal result forms no item anywhere, and the next
// older page holds exactly the input before the assistant. Its attachment to
// the assistant item is the conversation projector's own observation.
func TestHistoryPagePureIndivisibleBoundary(t *testing.T) {
	assistantID := pageEntryID(50)
	resultID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb99"
	headers := pageHeaders(50)
	headers = append(headers,
		harness.HistoryEntryHeader{EntryID: assistantID, OperationID: "op-1", Kind: harness.EntryAssistant},
		harness.HistoryEntryHeader{EntryID: resultID, OperationID: "op-1", Kind: harness.EntryToolResult},
	)

	page1, older1, err := selectHistoryPage(convSessionID, headers, nil)
	testassert.NoError(t, err)
	testassert.Equal(t, len(page1), 50)
	testassert.Equal(t, page1[49], assistantID)
	testassert.Require(t, older1 != nil, "initial page produced no older cursor with one older item")
	cursor1, err := decodeHistoryCursor(*older1, convSessionID)
	testassert.NoError(t, err)
	if cursor1.AnchorItemID != wantPageItemID(1) {
		t.Fatalf("older cursor anchor = %q, want the oldest returned input %q", cursor1.AnchorItemID, wantPageItemID(1))
	}
	page2, older2, err := selectHistoryPage(convSessionID, headers, older1)
	testassert.NoError(t, err)
	testassert.Require(t, len(page2) == 1 && page2[0] == pageEntryID(0), "older page = %v, want the single oldest input", page2)
	testassert.Require(t, older2 == nil, "older page produced a next cursor %v with nothing older", older2)
	// The result entry never became an item of either page.
	for _, page := range [][]string{page1, page2} {
		for _, entryID := range page {
			testassert.Require(t, entryID != resultID, "the terminal result entry %q became a page item", resultID)
		}
	}
}

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

func cursorJSON(sessionID, anchor string) string {
	return fmt.Sprintf(`{"session_id":%s,"anchor_item_id":%s}`,
		quoteJSON(sessionID), quoteJSON(anchor))
}

// TestHistoryCursorStrictValidation proves the cursor decode's closed rule:
// only the exact two-member document for this Session with a resolvable
// anchor passes, and every malformed sibling fails uniformly with
// harness.ErrInvalid.
func TestHistoryCursorStrictValidation(t *testing.T) {
	headers := pageHeaders(3)
	valid := cursorBody(t, cursorJSON(convSessionID, wantPageItemID(1)))
	{
		_, _, err := selectHistoryPage(convSessionID, headers, valid)
		testassert.NoError(t, err)
	}

	rejections := map[string]*string{
		"empty cursor":      ptrTo(""),
		"not base64":        ptrTo("not a cursor!"),
		"unknown member":    cursorBody(t, `{"session_id":`+quoteJSON(convSessionID)+`,"anchor_item_id":`+quoteJSON(wantPageItemID(1))+`,"extra":1}`),
		"trailing document": cursorBody(t, cursorJSON(convSessionID, wantPageItemID(1))+`{"again":1}`),
		// The retired version and direction members are ordinary unknown
		// members now: each row keeps an otherwise valid anchor of the
		// addressed Session, so every row isolates the closed member set.
		"retired version member":   cursorBody(t, `{"version":1,"session_id":`+quoteJSON(convSessionID)+`,"anchor_item_id":`+quoteJSON(wantPageItemID(1))+`}`),
		"retired direction member": cursorBody(t, `{"session_id":`+quoteJSON(convSessionID)+`,"anchor_item_id":`+quoteJSON(wantPageItemID(1))+`,"direction":"older"}`),
		"empty anchor":             cursorBody(t, cursorJSON(convSessionID, "")),
		"foreign session":          cursorBody(t, cursorJSON("ffffffffffffffffffffffffffffffff", wantPageItemID(1))),
		"nonexistent anchor":       cursorBody(t, cursorJSON(convSessionID, "missing")),
	}
	for name, cursor := range rejections {
		{
			_, _, err := selectHistoryPage(convSessionID, headers, cursor)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "%s: selectHistoryPage = %v, want harness.ErrInvalid", name, err)
		}
	}

	// The one uniform slicing rule for a resolvable anchor with nothing
	// older: a present empty page with no next cursor.
	page, older, err := selectHistoryPage(convSessionID, headers, cursorBody(t, cursorJSON(convSessionID, wantPageItemID(0))))
	testassert.NoError(t, err)
	testassert.Require(t, page != nil && len(page) == 0 && older == nil, "oldest-anchor page = %+v / %+v, want the present empty page with no cursor", page, older)
}

// --- the pure usage clocks ---

// usageRef is the fixture model identity every usage row reports under.
var usageRef = model.ModelRef{Provider: "prov", Model: "m"}

// TestUsedTokensPureClocks proves the display estimate's exact arithmetic:
// the signed input+cached sum stays exact for mixed signs and beyond
// MaxInt64, and no measurement renders zero. The boundary scan that selects
// the measurement is the history view accessor's own observation.
func TestUsedTokensPureClocks(t *testing.T) {
	cases := []struct {
		name     string
		measured *harness.UsageCount
		want     string
	}{
		{"no measurement", nil, "0"},
		{"plain sum", &harness.UsageCount{InputTokens: 100, CachedInputTokens: 10, OutputTokens: 5}, "110"},
		{"negative cached stays exact", &harness.UsageCount{InputTokens: 0, CachedInputTokens: -5, OutputTokens: 0}, "-5"},
		{"negative input stays exact", &harness.UsageCount{InputTokens: -5, CachedInputTokens: 0, OutputTokens: 0}, "-5"},
		{"mixed signs stay exact", &harness.UsageCount{InputTokens: 100, CachedInputTokens: -105, OutputTokens: 0}, "-5"},
		{"positive extreme beyond MaxInt64", &harness.UsageCount{InputTokens: math.MaxInt64, CachedInputTokens: math.MaxInt64, OutputTokens: 0}, "18446744073709551614"},
	}
	for _, tc := range cases {
		{
			got := projectUsedTokens(tc.measured)
			testassert.Require(t, got == tc.want, "%s: used tokens = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// usageWindowConfig and usageWindowAgents assemble one captured
// configuration with a positive catalog window distinct from the fixtures'
// captured 4096, plus a modelless Agent type.
const (
	usageWindowConfig = `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"models":{"m":{"name":"M","context_window":8192}}}}}`
	usageWindowAgents = `{"solo":{"model":"prov/m","system_prompt":"simple"},"modelless":{"system_prompt":"simple"}}`
)

// TestSessionContextWindowPureClocks proves the window resolution rule: the
// durably running Operation's captured window while one is admitted — the
// busy fact and a retiring run alone are not an active Operation — otherwise
// the captured configuration's catalog window for the selected Agent type,
// with every unavailable resolution reporting 0. The capture of that window
// from a durably current Operation is the composed view's own observation.
func TestSessionContextWindowPureClocks(t *testing.T) {
	captured, err := assembleConfiguration(2, usageWindowConfig, usageWindowAgents, nil)
	testassert.NoError(t, err)
	window := func(view harness.SessionReadView) int {
		return sessionContextWindow(view, resolveSessionSelection(view.Header.CurrentAgentType, captured))
	}

	// The active Operation's capture wins over the distinct catalog window.
	active := harness.SessionReadView{
		Header:              harness.SessionHeader{CurrentOperationID: "op-1", CurrentAgentType: "solo"},
		ActiveContextWindow: 4096,
	}
	testassert.Equal(t, window(active), 4096)

	// The busy fact and a retiring run alone are not an active Operation:
	// with no durably running current Operation the idle clock applies.
	retiring := harness.SessionReadView{
		Header:  harness.SessionHeader{CurrentAgentType: "solo"},
		History: harness.HistoryReadView{ExecutionBusy: true},
	}
	testassert.Equal(t, window(retiring), 8192)

	idle := harness.SessionReadView{Header: harness.SessionHeader{CurrentAgentType: "solo"}}
	testassert.Equal(t, window(idle), 8192)
	testassert.Equal(t, window(harness.SessionReadView{Header: harness.SessionHeader{CurrentAgentType: "modelless"}}), 0)
	testassert.Equal(t, window(harness.SessionReadView{Header: harness.SessionHeader{CurrentAgentType: "unknown"}}), 0)
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
	testassert.NoError(t, err)
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
		compactRawInputEntry(sessionID, inputID, operationID, "user", "question "+operationID))
	assistant, err := json.Marshal(struct {
		SessionID   string                   `json:"session_id"`
		EntryID     string                   `json:"entry_id"`
		OperationID string                   `json:"operation_id"`
		RequestID   string                   `json:"request_id"`
		Status      string                   `json:"status"`
		Source      usageWireRef             `json:"source"`
		Content     json.RawMessage          `json:"content"`
		ToolCalls   []harness.ToolCallRecord `json:"tool_calls"`
		Usage       *harness.UsageCount      `json:"usage,omitempty"`
	}{
		SessionID: sessionID, EntryID: assistantID, OperationID: operationID, RequestID: "req-" + operationID,
		Status: "completed", Source: usageWireRef{Provider: "prov", Model: "m"},
		Content:   json.RawMessage(`[{"kind":"text","text":"answer ` + operationID + `"}]`),
		ToolCalls: []harness.ToolCallRecord{},
		Usage:     assistantUsage,
	})
	testassert.NoError(t, err)
	settlementExtra := ""
	if settlementUsage != nil {
		model, err := json.Marshal(usageWireRef{Provider: "prov", Model: "m"})
		testassert.NoError(t, err)
		usage, err := json.Marshal(settlementUsage)
		testassert.NoError(t, err)
		settlementExtra = `,"model":` + string(model) + `,"usage":` + string(usage)
	}
	lifecycleInsertEntry(t, store, sessionID, assistantID, operationID, harness.EntryAssistant, string(assistant))
	lifecycleInsertEntry(t, store, sessionID, settlementID, operationID, harness.EntryOperationSettlement,
		compactRawSettlementEntry(sessionID, settlementID, operationID, settlementExtra))

	stateUsage := emptyUsageWireTotals()
	if assistantUsage != nil || settlementUsage != nil {
		stateUsage = usageWireTotalsFor(derefUsage(assistantUsage), derefUsage(settlementUsage))
	}
	usageJSON, err := json.Marshal(stateUsage)
	testassert.NoError(t, err)
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID},
		compactRawOperationRegister(
			compactRawMessageAdmission(sessionID, operationID, inputID),
			compactRawSuccessState(now.Format(time.RFC3339Nano), settlementID, sessionID, string(usageJSON))))
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
		testassert.NoError(t, err)
		testassert.Require(t, len(page1.Items) == 50 && page1.OlderCursor != nil, "initial page = %d items (cursor %v), want exactly 50 with an older cursor", len(page1.Items), page1.OlderCursor)
		wantPage1 := strings.Join(pageItemIDs(t, page1), ",")
		page2, err := r.getHistory(context.Background(), session, page1.OlderCursor)
		testassert.NoError(t, err)
		testassert.Require(t, len(page2.Items) == 10 && page2.OlderCursor == nil, "older page = %d items (cursor %v), want the remaining 10 with no cursor", len(page2.Items), page2.OlderCursor)
		wantPage2 := strings.Join(pageItemIDs(t, page2), ",")
		firstRevision := page1.SessionRevision

		// A newer append between page requests: the client's older cursor
		// stays valid, its page unchanged, and the read returns the current
		// authoritative revision with the stable item identities.
		submitConvergedThroughRuntime(t, r, session, "op-21", "late turn", harness.OperationSuccess)
		awaitIdleSession(t, r, session)

		page1After, err := r.getHistory(context.Background(), session, nil)
		testassert.NoError(t, err)
		testassert.Require(t, len(page1After.Items) == 50 && page1After.OlderCursor != nil, "post-append initial page = %d items, want exactly 50", len(page1After.Items))
		afterIDs := pageItemIDs(t, page1After)
		oldIDs := strings.Split(wantPage1, ",")
		for i, id := range oldIDs {
			// Exactly the three appended items aged out of the newest-50
			// window; every other stable identity remains.
			{
				agedOut := i < 3
				testassert.Require(t, slices.Contains(afterIDs, id) != agedOut, "post-append page item %d (%q) presence = %v, want the stable window shift by 3", i, id, !agedOut)
			}
		}
		testassert.Require(t, page1After.SessionRevision.DurableRevision != firstRevision.DurableRevision, "post-append revision = %+v, want the advanced current revision (was %+v)", page1After.SessionRevision, firstRevision)

		page2After, err := r.getHistory(context.Background(), session, page1.OlderCursor)
		testassert.NoError(t, err)
		testassert.Equal(t, strings.Join(pageItemIDs(t, page2After), ","), wantPage2)
		testassert.Require(t, page2After.SessionRevision.DurableRevision != firstRevision.DurableRevision && page2After.SessionRevision.DurableRevision == page1After.SessionRevision.DurableRevision, "older page returned the stale client revision %+v, want the current one; older page revision = %+v, want the same current revision as the fresh initial page %+v", page2After.SessionRevision, page2After.SessionRevision, page1After.SessionRevision)
	})
}

// sessionReadCalls builds the four independent reads of one Session over one
// owner — the page, the hydration, and the two history-error probes — each
// invoked with the caller's own context, so archived, deleted, closed, and
// canceled callers share the same read set.
func sessionReadCalls(ctx context.Context, r *Runtime, sessionID string) []func() error {
	return []func() error{
		func() error { _, err := r.getHistory(ctx, sessionID, nil); return err },
		func() error { _, err := r.buildHydration(ctx, sessionID); return err },
		func() error { return headerErrorThroughRuntime(ctx, r, sessionID) },
		func() error { return historyErrorThroughRuntime(ctx, r, sessionID) },
	}
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
		testassert.NoError(t, r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, archived)
			return err
		}))
		testassert.NoError(t, r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, deleted)
			return err
		}))
		testassert.NoError(t, r.deleteSession(context.Background(), deleted))

		archivedReads := sessionReadCalls(context.Background(), r, archived)
		for i, read := range archivedReads {
			{
				err := read()
				testassert.Require(t, err == nil, "archived read %d: %v", i, err)
			}
		}
		deletedReads := sessionReadCalls(context.Background(), r, deleted)
		for i, read := range deletedReads {
			{
				err := read()
				testassert.Require(t, errors.Is(err, harness.ErrNotFound), "deleted read %d = %v, want harness.ErrNotFound", i, err)
			}
		}
	})
}

// TestHistoryCursorRejectionsThroughRuntime proves the read body propagates
// the cursor's uniform invalid rule against the live owner; the retired-
// member and Session rows each carry an otherwise valid anchor of the
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
			ptrTo(""),
			ptrTo("!!!!"),
			cursorBody(t, `{"version":1,"session_id":`+quoteJSON(session)+`,"anchor_item_id":`+quoteJSON(anchor)+`,"direction":"older"}`),
			cursorBody(t, cursorJSON("ffffffffffffffffffffffffffffffff", anchor)),
			cursorBody(t, cursorJSON(session, "missing")),
		}
		for i, cursor := range rejected {
			{
				_, err := r.getHistory(context.Background(), session, cursor)
				testassert.Require(t, errors.Is(err, harness.ErrInvalid), "cursor rejection %d = %v, want harness.ErrInvalid", i, err)
			}
		}
	})
}

// TestHistoryGeneratedClientCompactionRetainsOldCursor proves, over both
// stores and the mounted generated client, the full combination: a history of
// more than one page, an older cursor issued before a real manual compaction
// stays valid with its page unchanged, and the complete original history plus
// the compaction item stays pageable — every post-compaction page carrying
// the current revision, and the compaction item carrying the real scripted
// model's summary.
func TestHistoryGeneratedClientCompactionRetainsOldCursor(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		session := newLifecycleID(t)
		usageSeedSession(t, store, session, "", "", "solo", emptyUsageWireTotals())
		for i := 1; i <= 18; i++ { // 18 settled turns × 3 items = 54 items
			compactRawSuccessOperation(t, store, session, fmt.Sprintf("op-%d", i), fmt.Sprintf("question op-%d", i), fmt.Sprintf("answer op-%d", i))
		}
		r, _ := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)
		ctx := context.Background()

		// The pre-compaction paging: the newest 50 and the older 4, with the
		// older cursor anchored at the fourth item.
		page1, err := client.GetSessionHistoryWithResponse(ctx, session, &protocol.GetSessionHistoryParams{})
		testassert.Require(t, err == nil && page1.JSON200 != nil, "initial history: %v", err)
		if len(page1.JSON200.Items) != 50 || page1.JSON200.OlderCursor == nil {
			body, _ := json.Marshal(page1.JSON200)
			t.Fatalf("initial history = %s, want exactly 50 items with an older cursor", body)
		}
		assertQualifiedInstance(t, "initial history", page1.JSON200.SessionRevision, ps.instance)
		page2, err := client.GetSessionHistoryWithResponse(ctx, session, &protocol.GetSessionHistoryParams{Cursor: page1.JSON200.OlderCursor})
		testassert.Require(t, err == nil && page2.JSON200 != nil, "older history: %v", err)
		if len(page2.JSON200.Items) != 4 || page2.JSON200.OlderCursor != nil {
			body, _ := json.Marshal(page2.JSON200)
			t.Fatalf("older history = %s, want the remaining 4 items with no cursor", body)
		}
		preItems := append(append([]protocol.ConversationItem{}, page1.JSON200.Items...), page2.JSON200.Items...)
		testassert.Equal(t, len(preItems), 54)
		preRevision := page1.JSON200.SessionRevision.DurableRevision
		oldCursor := *page1.JSON200.OlderCursor

		// The real manual compaction through the mounted command.
		compacted, err := client.CompactSessionWithResponse(ctx, session, protocol.CompactRequest{OperationId: "compact-1"})
		testassert.NoError(t, err)
		if compacted.JSON200 == nil || compacted.JSON200.OperationId != "compact-1" {
			body, _ := json.Marshal(compacted.JSON200)
			t.Fatalf("compact = %s, want the admitted compact Operation", body)
		}
		awaitIdleSession(t, r, session)

		// The old cursor stays valid: the same four items, no compaction
		// item, nothing older, and the current revision.
		page2After, err := client.GetSessionHistoryWithResponse(ctx, session, &protocol.GetSessionHistoryParams{Cursor: &oldCursor})
		testassert.Require(t, err == nil && page2After.JSON200 != nil, "older history after compaction: %v", err)
		if got := strings.Join(pageItemIDs(t, *page2After.JSON200), ","); got != strings.Join(pageItemIDs(t, *page2.JSON200), ",") {
			t.Fatalf("old-cursor page after compaction = %s, want the unchanged %s", got, strings.Join(pageItemIDs(t, *page2.JSON200), ","))
		}
		testassert.Require(t, page2After.JSON200.OlderCursor == nil && page2After.JSON200.SessionRevision.DurableRevision != preRevision, "old-cursor page after compaction produced an older cursor %v with nothing older; old-cursor page revision = %s, want the advanced current revision", page2After.JSON200.OlderCursor, page2After.JSON200.SessionRevision.DurableRevision)
		assertQualifiedInstance(t, "old-cursor page", page2After.JSON200.SessionRevision, ps.instance)

		// The complete history plus the compaction item stays pageable: walk
		// every page and collect the items.
		type walkPage struct {
			page   *protocol.HistoryPage
			cursor *string
		}
		walk := func(cursor *string) (*protocol.HistoryPage, *string, error) {
			resp, err := client.GetSessionHistoryWithResponse(ctx, session, &protocol.GetSessionHistoryParams{Cursor: cursor})
			if err != nil || resp.JSON200 == nil {
				return nil, nil, fmt.Errorf("history page: %v", err)
			}
			return resp.JSON200, resp.JSON200.OlderCursor, nil
		}
		var walked []protocol.ConversationItem
		var currentRevision string
		firstPageLen := 0
		for cursor := (*string)(nil); ; {
			page, next, err := walk(cursor)
			testassert.NoError(t, err)
			if cursor == nil {
				firstPageLen = len(page.Items)
			}
			walked = append(walked, page.Items...)
			currentRevision = page.SessionRevision.DurableRevision
			testassert.Require(t, currentRevision != preRevision, "walk page revision = %s, want the advanced current revision", currentRevision)
			if next == nil {
				break
			}
			cursor = next
		}
		testassert.Equal(t, len(walked), 56)
		// Exactly one compaction item, carrying the real scripted model's
		// summary, and the compact Operation's settlement beside it — the
		// first page's two newest items.
		var compactions []*protocol.CompactionItem
		compactionAt := -1
		for i, item := range walked {
			if itemKind(t, item) != "compaction" {
				continue
			}
			testassert.Require(t, compactionAt < 0, "second compaction item at walk position %d", i)
			value, err := item.AsCompactionItem()
			testassert.Require(t, err == nil, "compaction item %d: %v", i, err)
			compactions = append(compactions, &value)
			compactionAt = i
		}
		testassert.Equal(t, compactionAt, firstPageLen-2)
		if len(compactions) != 1 || compactions[0].Summary != "done" || compactions[0].Model != "prov/m" {
			body, _ := json.Marshal(compactions)
			t.Fatalf("compaction items = %s, want exactly the real model summary", body)
		}
		end, err := walked[firstPageLen-1].AsOperationEndItem()
		testassert.Require(t, err == nil && end.Status == protocol.OperationEndItemStatusSuccess && end.OperationId != nil && *end.OperationId == "compact-1", "compact settlement item = %+v err %v, want compact-1's settled success", end, err)
		// Removing the compaction and settlement items leaves exactly the
		// original items: every pre-compaction identity appears once with
		// byte-identical wire content.
		without := append(append([]protocol.ConversationItem{}, walked[:compactionAt]...), walked[compactionAt+2:]...)
		testassert.Equal(t, len(without), len(preItems))
		preByItem := make(map[string]string, len(preItems))
		for _, item := range preItems {
			data, err := json.Marshal(item)
			testassert.NoError(t, err)
			preByItem[wireItemID(t, item)] = string(data)
		}
		for _, item := range without {
			id := wireItemID(t, item)
			data, err := json.Marshal(item)
			testassert.NoError(t, err)
			if preByItem[id] != string(data) {
				t.Fatalf("item %q = %s, want the original %s", id, data, preByItem[id])
			}
			delete(preByItem, id)
		}
		testassert.Equal(t, len(preByItem), 0)
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

		facts, _ := historyThroughRuntime(t, r, session)
		view := historyViewThroughRuntime(t, r, session)
		headers := view.HistoryHeaders()
		entryID := inputEntryID(t, r, session, "op-1")
		boundary, err := resolveBoundaryEntry(session, headers, projectItemID(session, entryID))
		testassert.NoError(t, err)
		testassert.Equal(t, boundary, entryID)

		// The user input of a different Session's namespace never resolves.
		foreign := projectItemID("ffffffffffffffffffffffffffffffff", entryID)
		{
			_, err := resolveBoundaryEntry(session, headers, foreign)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "foreign boundary = %v, want harness.ErrInvalid", err)
		}
		// A committed non-user item never resolves: find the first turn's
		// assistant item identity.
		var assistantItem string
		for _, fact := range facts {
			if fact.Kind == harness.EntryAssistant {
				assistantItem = projectItemID(session, fact.EntryID)
				break
			}
		}
		{
			_, err := resolveBoundaryEntry(session, headers, assistantItem)
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "assistant boundary = %v, want harness.ErrInvalid", err)
		}
		{
			_, err := resolveBoundaryEntry(session, headers, "no-such-item")
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "nonexistent boundary = %v, want harness.ErrInvalid", err)
		}
	})
}

// TestHistoryPageAttachesResultsBeyondTheBaseRange proves the page producer
// resolves every selected assistant's reserved result identities through the
// same captured history view, including results committed after the page's
// last base entry: the anchored page ending just before the interruption
// signal still carries the interrupted assistant's published call outcomes,
// and no result entry forms an item of its own.
func TestHistoryPageAttachesResultsBeyondTheBaseRange(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		stopper := newLifecycleStopper()
		bg := openBackgroundRuntime(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		session := bg.session("page-results")
		releaseTool := make(chan struct{})
		releaseToolOnce := sync.OnceFunc(func() { close(releaseTool) })
		defer releaseToolOnce()
		toolStarted := make(chan struct{}, 1)
		bg.e.server.setScript(func(_ context.Context, body string) []string {
			return twoReadCallTurnEvents()
		})
		// The started call parks until the run's cancellation releases it; the
		// unstarted call never begins and settles synthetically.
		bg.tools.setPrepare(parkedReadCallPrepare(releaseTool, toolStarted))
		submitThroughRuntime(t, bg.r, session, "op-1", "work")
		select {
		case <-toolStarted:
		case <-time.After(10 * time.Second):
			t.Fatal("the first call never started")
		}
		testassert.NoError(t, bg.r.interruptSession(ctx, session))
		awaitOperation(t, bg.r, session, "op-1", harness.OperationInterruption)
		awaitIdleSession(t, bg.r, session)

		// The interruption signal's item identity anchors the older page.
		newest, err := bg.r.getHistory(ctx, session, nil)
		testassert.NoError(t, err)
		signalAnchor := ""
		for _, item := range newest.Items {
			if kind, _ := item.Discriminator(); kind == "signal" {
				signalAnchor = wireItemID(t, item)
			}
		}
		if signalAnchor == "" {
			t.Fatalf("newest page items = %s, want the interruption signal", itemKinds(t, newest.Items))
		}

		page, err := bg.r.getHistory(ctx, session, cursorBody(t, cursorJSON(session, signalAnchor)))
		testassert.NoError(t, err)
		if len(page.Items) != 2 { // the input and the interrupted assistant
			t.Fatalf("anchored page = %s, want the input and the assistant before the signal", itemKinds(t, page.Items))
		}
		assistant, err := page.Items[1].AsAssistantItem()
		testassert.Require(t, err == nil && len(assistant.ToolCalls) == 2, "anchored assistant = %+v err %v, want both published calls", assistant, err)
		started := assistant.ToolCalls[0]
		testassert.Require(t, started.Id == "call-1" && started.Status != nil && *started.Status == protocol.ToolCallStatusSuccess && started.Content != nil && *started.Content == "first ran", "started call = %+v, want the real returned outcome attached beyond the page range", started)
		unstarted := assistant.ToolCalls[1]
		testassert.Require(t, unstarted.Id == "call-2" && unstarted.Status != nil && *unstarted.Status == protocol.ToolCallStatusInterrupted, "unstarted call = %+v, want the synthetic interrupted outcome attached beyond the page range", unstarted)
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
		e.server.setHold(gate)
		release := sync.OnceFunc(func() { close(gate) })
		defer release() // LIFO: the gate releases before the owner close joins

		submitThroughRuntime(t, r, session, "op-1", "running")
		awaitModelArrival(t, e)
		// A regular submit while running rides the steering buffer; queued
		// submissions fill the queued FIFO in order.
		steering, err := submitRawThroughRuntime(context.Background(), r, session, "op-s1", "steer me", harness.MessageModeRegular)
		testassert.Require(t, err == nil, "Submit(op-s1): %v", err)
		testassert.Require(t, steering.Disposition == harness.DispositionSteering, "steering submit = %s, want steering", steering.Disposition)
		submitQueuedThroughRuntime(t, r, session, "op-q1", "queued one")
		submitQueuedThroughRuntime(t, r, session, "op-q2", "queued two")

		hydration, err := r.buildHydration(context.Background(), session)
		testassert.NoError(t, err)
		pending := hydration.Pending
		testassert.Require(t, len(pending.Steering) == 1 && len(pending.Queued) == 2, "pending = %+v, want one steering and two queued members", pending)
		steerText := pendingText(t, pending.Steering[0])
		testassert.Require(t, pending.Steering[0].OperationId == "op-s1" && steerText == "steer me" && pending.Steering[0].Origin == protocol.InputOriginUser && pending.Queued[0].OperationId == "op-q1" && pendingText(t, pending.Queued[0]) == "queued one" && pending.Queued[1].OperationId == "op-q2" && pendingText(t, pending.Queued[1]) == "queued two", "steering member = %+v (%q), want op-s1's user text; queued members = %+v, want the FIFO order q1 then q2", pending.Steering[0], steerText, pending.Queued)
		testassert.Require(t, hydration.ActiveOperation != nil && hydration.ActiveOperation.OperationId == "op-1" && hydration.ActiveOperation.Status == protocol.OperationStatusRunning, "active operation = %+v, want the durably running op-1", hydration.ActiveOperation)
		header := projectSession(headerThroughRuntime(t, r, session))
		testassert.Require(t, reflect.DeepEqual(hydration.Session, header), "hydration session = %+v, want the header read %+v", hydration.Session, header)
		data, err := json.Marshal(hydration)
		testassert.NoError(t, err)
		for _, member := range []string{"prompt-solo", "system_prompt", "result_entry_id"} {
			testassert.Require(t, !strings.Contains(string(data), member), "hydration wire = %s, want no internal %q representation", data, member)
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
	testassert.NoError(t, err)
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
		stopper := newLifecycleStopper()
		bg := openBackgroundRuntime(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		stopper.arm(bg.r.harness)

		root := bg.session("hyd-root")
		arrived := make(chan struct{}, 1)
		park := make(chan struct{})
		parkOnce := sync.OnceFunc(func() { close(park) })
		defer parkOnce() // LIFO: the gate releases before the owner close joins
		bg.e.server.setScript(func(sctx context.Context, body string) []string {
			if lastUserText(body) != "child work" {
				return nil
			}
			select { // the child's first model request parks on the test gate
			case arrived <- struct{}{}:
			default:
			}
			select {
			case <-park:
				return textTurnEvents("child finished")
			case <-sctx.Done():
				return nil
			}
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
		testassert.NoError(t, err)
		testassert.Require(t, childHydration.Session.SessionId == child && childHydration.ActiveOperation != nil && childHydration.ActiveOperation.OperationId == "child-op-1" && childHydration.ActiveOperation.Status == protocol.OperationStatusRunning && len(childHydration.Conversation.Items) == 1 && len(childHydration.Operations) == 1 && len(childHydration.Background) == 1 && childHydration.Background[0].Id == "cc000003", "child hydration = %+v active %+v, want the ordinary running child Session; child hydration = %d items %d operations, want one item and one Operation; child background = %+v, want the live job member", childHydration.Session, childHydration.ActiveOperation, len(childHydration.Conversation.Items), len(childHydration.Operations), childHydration.Background)

		// The parent carries the live child member.
		rootHydration, err := bg.r.buildHydration(context.Background(), root)
		testassert.NoError(t, err)
		testassert.Require(t, len(rootHydration.Background) == 1 && rootHydration.Background[0].Id == child, "root background = %+v, want the live child member", rootHydration.Background)

		// The child settles; its parent member finishes and the completion
		// input is delivered to the parent's conversation.
		parkOnce()
		awaitOperation(t, bg.r, child, "child-op-1", harness.OperationSuccess)
		awaitIdleSession(t, bg.r, child)
		stopper.addDelivery(child, completionID, "job report")
		testassert.NoError(t, bg.r.stopSession(context.Background(), child))
		deadline := time.Now().Add(10 * time.Second)
		for {
			rootHydration, err = bg.r.buildHydration(context.Background(), root)
			testassert.NoError(t, err)
			if hasRuntimeOriginInput(t, rootHydration.Conversation.Items, "child finished") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("root conversation = %s, want the runtime-origin completion input", itemKinds(t, rootHydration.Conversation.Items))
			}
			time.Sleep(5 * time.Millisecond)
		}
		testassert.Require(t, len(rootHydration.Background) == 0, "root background after completion = %+v, want the finished member gone", rootHydration.Background)

		// The delivered runtime-origin input is committed non-user history:
		// its item identity never resolves as a fork boundary.
		facts, _ := historyThroughRuntime(t, bg.r, root)
		for _, fact := range facts {
			if fact.Kind == harness.EntryInput && fact.Input.Origin == harness.InputOriginRuntime {
				{
					_, err := resolveBoundaryEntry(root, historyViewThroughRuntime(t, bg.r, root).HistoryHeaders(), projectItemID(root, fact.EntryID))
					testassert.Require(t, errors.Is(err, harness.ErrInvalid), "runtime-origin boundary = %v, want harness.ErrInvalid", err)
				}
			}
		}

		// The equality oracle at the settled stable state: every hydration
		// member equals its own narrow producer's read of the same state.
		childHeader := projectSession(headerThroughRuntime(t, bg.r, child))
		childPage, err := bg.r.getHistory(context.Background(), child, nil)
		testassert.NoError(t, err)
		childHydration, err = bg.r.buildHydration(context.Background(), child)
		testassert.NoError(t, err)
		testassert.Require(t, reflect.DeepEqual(childHydration.Session, childHeader) && childHydration.SessionRevision == childPage.SessionRevision && reflect.DeepEqual(childHydration.Conversation.Items, childPage.Items) && reflect.DeepEqual(childHydration.Conversation.OlderCursor, childPage.OlderCursor) && len(childHydration.Pending.Steering) == 0 && len(childHydration.Pending.Queued) == 0 && childHydration.ActiveOperation == nil, "settled child hydration disagrees with the narrow producers: %+v", childHydration)
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
		// The row's own intentional initial catalog: the real 4096 idle window,
		// distinct from the fixtures' shared generous default. The row admits
		// no work, so the placeholder endpoint is never contacted.
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, `{"providers":{"prov":`+nominalProvMember("https://prov.test/v1", "", `{"m":{"name":"M","context_window":4096}}`)+`}}`)
		writeServiceFile(t, agents.PathForConfig(e.configPath), usageWindowAgents)
		r, err := e.open(context.Background(), e.storagePlugin(store))
		testassert.NoError(t, err)
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
			compactRawInputEntry(busy, busyInput, "b-op-1", "user", "running"))
		lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: busy, Kind: harness.RegisterOperation, OperationID: "b-op-1"},
			compactRawOperationRegister(compactRawMessageAdmission(busy, "b-op-1", busyInput), compactRawRunningModelState(time.Now().UTC().Format(time.RFC3339Nano), newLifecycleID(t))))

		// A modelless Session: the Agent type resolves but selects no model.
		modelless := newLifecycleID(t)
		usageSeedSession(t, store, modelless, "", "", "modelless", emptyUsageWireTotals())

		usageRow := func(t *testing.T, sessionID string) protocol.Hydration {
			t.Helper()
			hydration, err := r.buildHydration(ctx, sessionID)
			testassert.Require(t, err == nil, "buildHydration(%s): %v", sessionID, err)
			return hydration
		}

		// Generation 1: the fixture catalog's 4096 window for idle reads,
		// the captured window for the busy read.
		got := usageRow(t, measured)
		testassert.Require(t, reflect.DeepEqual(got.Usage.Totals, wantUsageRow("450", "35", "15")) && got.Usage.Context.UsedTokens == "55" && got.Usage.Context.ContextWindow == 4096 && got.ConfigurationRevision.Generation == "1", "measured totals = %+v, want the register totals 450/35/15; measured context = %+v, want the estimate 55 under the 4096 window; configuration revision = %+v, want generation 1", got.Usage.Totals, got.Usage.Context, got.ConfigurationRevision)
		resetRow := usageRow(t, reset)
		testassert.Require(t, resetRow.Usage.Context.UsedTokens == "0" && resetRow.Usage.Context.ContextWindow == 4096 && reflect.DeepEqual(resetRow.Usage.Totals, wantUsageRow("70", "7", "2")), "reset usage = %+v, want estimate 0 with the independent 70/7/2 totals", resetRow.Usage)
		busyRow := usageRow(t, busy)
		testassert.Equal(t, busyRow.Usage.Context.ContextWindow, 4096)
		{
			modellessRow := usageRow(t, modelless)
			testassert.Equal(t, modellessRow.Usage.Context.ContextWindow, 0)
		}

		// Reload to a distinct positive catalog window plus the modelless
		// Agent type: the idle reads report the new selection while the
		// busy read keeps its capture, and every read names the captured
		// generation.
		writeServiceFile(t, e.configPath, `{"providers":{"prov":`+nominalProvMember("https://prov.test/v1", "", `{"m":{"name":"M","context_window":8192}}`)+`}}`)
		writeServiceFile(t, agents.PathForConfig(e.configPath), usageWindowAgents)
		{
			_, err := r.Reload(ctx)
			testassert.NoError(t, err)
		}
		got = usageRow(t, measured)
		testassert.Require(t, got.Usage.Context.ContextWindow == 8192 && got.Usage.Context.UsedTokens == "55" && got.ConfigurationRevision.Generation == "2", "reloaded idle usage = %+v rev %+v, want estimate 55 under 8192 at generation 2", got.Usage, got.ConfigurationRevision)
		busyRow = usageRow(t, busy)
		testassert.Require(t, busyRow.Usage.Context.ContextWindow == 4096 && busyRow.ConfigurationRevision.Generation == "2", "reloaded busy usage = %+v rev %+v, want the active captured 4096 at generation 2", busyRow.Usage, busyRow.ConfigurationRevision)
		// The same running Session's hydration: the separately captured
		// configuration revision and the retained captured window ride the
		// one snapshot producer.
		busyHydration, err := r.buildHydration(ctx, busy)
		testassert.NoError(t, err)
		testassert.Require(t, busyHydration.ConfigurationRevision.Generation == "2" && busyHydration.Usage.Context.ContextWindow == 4096 && busyHydration.ActiveOperation != nil && busyHydration.ActiveOperation.OperationId == "b-op-1", "reloaded busy hydration = rev %+v context %+v active %+v, want generation 2, the captured 4096 window, and the running b-op-1", busyHydration.ConfigurationRevision, busyHydration.Usage.Context, busyHydration.ActiveOperation)
		{
			row := usageRow(t, modelless)
			testassert.Require(t, row.Usage.Context.ContextWindow == 0 && row.ConfigurationRevision.Generation == "2", "modelless usage = %+v, want window 0 at generation 2", row.Usage)
		}

		// Deleting the model from the catalog: the idle window is 0 while
		// the estimate and totals stay independent.
		writeServiceFile(t, e.configPath, `{"providers":{}}`)
		{
			_, err := r.Reload(ctx)
			testassert.NoError(t, err)
		}
		got = usageRow(t, measured)
		testassert.Require(t, got.Usage.Context.ContextWindow == 0 && got.Usage.Context.UsedTokens == "55" && reflect.DeepEqual(got.Usage.Totals, wantUsageRow("450", "35", "15")) && got.ConfigurationRevision.Generation == "3", "deleted-model usage = %+v rev %+v, want window 0 with the retained estimate and totals at generation 3", got.Usage, got.ConfigurationRevision)
	})
}

// TestHydrationSelectedModelFollowsNextAdmission proves the required nullable
// selection member: an idle Session names its configured model and window, a
// definition with no model or an unknown type yields null, an unavailable
// configured ref stays identifiable with a zero window, and a running
// Operation keeps its captured active model/window while selected_model names
// the next admission's selection under the same captured configuration.
func TestHydrationSelectedModelFollowsNextAdmission(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, `{"providers":{"prov":`+nominalProvMember(e.server.URL, "", `{"m":{"name":"M","context_window":4096,"max_output_tokens":1024},"m2":{"name":"M2","context_window":8192,"max_output_tokens":1024}}`)+`}}`)
		writeServiceFile(t, agents.PathForConfig(e.configPath), `{"solo":{"model":"prov/m","system_prompt":"simple"},"alternate":{"model":"prov/m2","system_prompt":"simple"},"modelless":{"system_prompt":"simple"},"gone":{"model":"prov/gone","system_prompt":"simple"}}`)
		r, err := e.open(context.Background(), e.storagePlugin(store))
		testassert.NoError(t, err)
		defer closeProjectionRuntime(r)

		// An idle Session resolves the selected definition in the captured
		// catalog: one shared selection supplies the model and the window.
		solo := projectionSession(t, r, "/tmp/selection-solo", "solo").Identity.SessionID
		idle, err := r.buildHydration(context.Background(), solo)
		testassert.NoError(t, err)
		testassert.Require(t, idle.SelectedModel != nil && *idle.SelectedModel == "prov/m" && idle.Usage.Context.ContextWindow == 4096 && idle.Session.AgentType == "solo", "idle selection = model %v window %d, want prov/m under 4096; got %v, want %v", idle.SelectedModel, idle.Usage.Context.ContextWindow, idle.Session.AgentType, "solo")

		// No configured ref and an unresolvable type both yield null rather
		// than a fallback model.
		for _, tc := range []struct{ name, agentType string }{{"modelless", "modelless"}, {"unknown", "ghost"}} {
			session := projectionSession(t, r, "/tmp/selection-"+tc.name, tc.agentType).Identity.SessionID
			hydration, err := r.buildHydration(context.Background(), session)
			testassert.Require(t, err == nil, "buildHydration(%s): %v", tc.name, err)
			testassert.Require(t, hydration.SelectedModel == nil && hydration.Usage.Context.ContextWindow == 0, "%s selection = model %v window %d, want null and 0", tc.name, hydration.SelectedModel, hydration.Usage.Context.ContextWindow)
		}

		// A configured but unavailable ref stays identifiable while admission
		// still refuses it: the window is 0, the model is not invented away.
		gone := projectionSession(t, r, "/tmp/selection-gone", "gone").Identity.SessionID
		goneHydration, err := r.buildHydration(context.Background(), gone)
		testassert.NoError(t, err)
		testassert.Require(t, goneHydration.SelectedModel != nil && *goneHydration.SelectedModel == "prov/gone" && goneHydration.Usage.Context.ContextWindow == 0, "gone selection = model %v window %d, want the configured prov/gone identifiable at 0", goneHydration.SelectedModel, goneHydration.Usage.Context.ContextWindow)

		// A running Operation keeps its capture: an Agent-type change while
		// it runs names the next admission's selection, while the active
		// model and window stay the captured pair under one configuration
		// revision.
		gate := make(chan struct{})
		e.server.setHold(gate)
		release := sync.OnceFunc(func() { close(gate) })
		defer release()
		active := projectionSession(t, r, "/tmp/selection-active", "solo").Identity.SessionID
		submitThroughRuntime(t, r, active, "op-1", "running")
		awaitModelArrival(t, e)
		testassert.NoError(t, r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ChangeAgentType(ctx, active, "alternate")
			return err
		}))
		activeHydration, err := r.buildHydration(context.Background(), active)
		testassert.NoError(t, err)
		testassert.Require(t, activeHydration.ActiveOperation != nil && activeHydration.ActiveOperation.Model == "prov/m" && activeHydration.Usage.Context.ContextWindow == 4096 && activeHydration.SelectedModel != nil && *activeHydration.SelectedModel == "prov/m2" && activeHydration.Session.AgentType == "alternate" && activeHydration.SessionRevision == activeHydration.Session.SessionRevision, "active hydration = active %+v window %d, want the captured prov/m under 4096; active next selection = %v, want the changed prov/m2; got %v, want %v; got %v, want %v", activeHydration.ActiveOperation, activeHydration.Usage.Context.ContextWindow, activeHydration.SelectedModel, activeHydration.Session.AgentType, "alternate", activeHydration.SessionRevision, activeHydration.Session.SessionRevision)

		// Once idle, the shared resolution supplies the new model's window.
		release()
		awaitIdleSession(t, r, active)
		settled, err := r.buildHydration(context.Background(), active)
		testassert.NoError(t, err)
		testassert.Require(t, settled.SelectedModel != nil && *settled.SelectedModel == "prov/m2" && settled.Usage.Context.ContextWindow == 8192, "settled selection = model %v window %d, want prov/m2 under 8192", settled.SelectedModel, settled.Usage.Context.ContextWindow)

		// A reload re-points the selected Agent type to another model while a
		// second Operation is blocked: selected_model follows the reloaded
		// catalog under the response's captured configuration revision, while
		// the active Operation keeps its captured model and window.
		awaitRestorableSession(t, r, active) // terminal publication can precede run-slot retirement
		secondGate := make(chan struct{})
		e.server.setHold(secondGate)
		releaseSecond := sync.OnceFunc(func() { close(secondGate) })
		defer releaseSecond()
		submitThroughRuntime(t, r, active, "op-2", "running again")
		awaitModelArrival(t, e)
		writeServiceFile(t, agents.PathForConfig(e.configPath), `{"solo":{"model":"prov/m","system_prompt":"simple"},"alternate":{"model":"prov/m","system_prompt":"simple"},"modelless":{"system_prompt":"simple"},"gone":{"model":"prov/gone","system_prompt":"simple"}}`)
		{
			_, err := r.Reload(context.Background())
			testassert.NoError(t, err)
		}
		reloaded, err := r.buildHydration(context.Background(), active)
		testassert.NoError(t, err)
		testassert.Require(t, reloaded.ActiveOperation != nil && reloaded.ActiveOperation.Model == "prov/m2" && reloaded.Usage.Context.ContextWindow == 8192 && reloaded.SelectedModel != nil && *reloaded.SelectedModel == "prov/m" && reloaded.ConfigurationRevision.Generation == "2", "reloaded active capture = active %+v window %d, want the captured prov/m2 under its captured 8192; reloaded selected model = %v, want the re-pointed prov/m; reloaded configuration revision = %+v, want generation 2", reloaded.ActiveOperation, reloaded.Usage.Context.ContextWindow, reloaded.SelectedModel, reloaded.ConfigurationRevision)

		releaseSecond()
		awaitIdleSession(t, r, active)
		afterReload, err := r.buildHydration(context.Background(), active)
		testassert.NoError(t, err)
		testassert.Require(t, afterReload.SelectedModel != nil && *afterReload.SelectedModel == "prov/m" && afterReload.Usage.Context.ContextWindow == 4096, "after-reload selection = model %v window %d, want prov/m under 4096", afterReload.SelectedModel, afterReload.Usage.Context.ContextWindow)
	})
}

// TestProjectionReadsClosedAndCanceledCallers proves the new reads hold the
// admission gate: closed owners reject with ErrClosed and canceled callers
// with their own context error.
func TestProjectionReadsClosedAndCanceledCallers(t *testing.T) {
	r, _ := openProjectionRuntime(t, storage.NewMemory())
	defer closeProjectionRuntime(r)
	const sessionID = "0123456789abcdef0123456789abcdef"
	testassert.NoError(t, r.Close(context.Background()))
	closed := sessionReadCalls(context.Background(), r, sessionID)
	for i, call := range closed {
		{
			err := call()
			testassert.Require(t, errors.Is(err, ErrClosed), "closed caller %d = %v, want ErrClosed", i, err)
		}
	}

	r2, _ := openProjectionRuntime(t, storage.NewMemory())
	defer closeProjectionRuntime(r2)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceledCalls := sessionReadCalls(canceled, r2, sessionID)
	for i, call := range canceledCalls {
		{
			err := call()
			testassert.Require(t, errors.Is(err, context.Canceled), "canceled caller %d = %v, want context.Canceled", i, err)
		}
	}
}

// TestHydrationAndPageReturnedValueOwnership proves a returned page or
// hydration is a caller copy: the pre-mutation expectation stays immutable,
// caller mutation cannot reach the owner, and the next read is unchanged.
func TestHydrationAndPageReturnedValueOwnership(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, _ := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		session := projectionSession(t, r, "/tmp/paging-owned", "solo").Identity.SessionID
		submitConvergedThroughRuntime(t, r, session, "op-1", "work", harness.OperationSuccess)
		// The run slot retires after the terminal is durably visible, and its
		// retirement is a coordinator-local publication: the immutable
		// baseline must be taken only after the busy state has cleared.
		awaitSessionNotBusy(t, r, session)

		page, err := r.getHistory(context.Background(), session, nil)
		testassert.NoError(t, err)
		wantPage, err := json.Marshal(page)
		testassert.NoError(t, err)
		page.Items = append(page.Items, protocol.ConversationItem{})
		page.Items[0] = protocol.ConversationItem{}
		reread, err := r.getHistory(context.Background(), session, nil)
		testassert.NoError(t, err)
		gotPage, err := json.Marshal(reread)
		testassert.NoError(t, err)
		testassert.Require(t, string(gotPage) == string(wantPage), "re-read page = %s, want the immutable %s", gotPage, wantPage)

		hydration, err := r.buildHydration(context.Background(), session)
		testassert.NoError(t, err)
		wantHydration, err := json.Marshal(hydration)
		testassert.NoError(t, err)
		hydration.Session.AgentType = "mutated"
		hydration.Conversation.Items = nil
		hydration.Pending.Steering = append(hydration.Pending.Steering, protocol.PendingInput{OperationId: "mutated"})
		hydration.Operations[0].OperationId = "mutated"
		hydration.Background = append(hydration.Background, protocol.BackgroundMember{Id: "mutated"})
		rereadHydration, err := r.buildHydration(context.Background(), session)
		testassert.NoError(t, err)
		gotHydration, err := json.Marshal(rereadHydration)
		testassert.NoError(t, err)
		testassert.Require(t, string(gotHydration) == string(wantHydration), "re-read hydration = %s, want the immutable %s", gotHydration, wantHydration)
	})
}
