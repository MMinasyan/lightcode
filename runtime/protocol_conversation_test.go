package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// The client conversation projection: every row runs the private pure
// projector over one SessionSnapshot. The typed-fixture rows pin the exact
// item shapes and value fidelity against stable validated facts; the live
// rows drive real model/tool/storage effects through the composed Runtime and
// project the facts the Harness actually validated.

// convSessionID and convEntryID are the known-vector identities: a realistic
// 32-hex Session and entry.
const (
	convSessionID = "0123456789abcdef0123456789abcdef"
	convEntryID   = "fedcba9876543210fedcba9876543210"
)

// convTime is one fixed UTC commit instant for the typed fixtures.
var convTime = time.Date(2026, 9, 29, 10, 20, 30, 0, time.UTC)

func convArgs(raw string) string {
	return base64.StdEncoding.Strict().EncodeToString([]byte(raw))
}

// convSnapshot builds one minimal owned snapshot for the given session.
func convSnapshot(facts []harness.HistoryFact) harness.SessionSnapshot {
	return harness.SessionSnapshot{
		Session: harness.SessionRecord{
			Revision: 7,
			Identity: harness.SessionIdentity{SessionID: convSessionID, Workspace: "/w", CreatedAt: convTime},
			State:    harness.SessionState{Lifecycle: harness.LifecycleOpen, LastActivity: convTime},
		},
		Operations: []harness.OperationRecord{},
		Facts:      facts,
		Steering:   []harness.PendingInput{},
		Queued:     []harness.PendingInput{},
		Background: []harness.BackgroundMemberView{},
	}
}

func convInputFact(entryID, operationID string, parts ...model.ContentPart) harness.HistoryFact {
	return harness.HistoryFact{
		EntryID: entryID, OperationID: operationID, Sequence: 1, CommittedAt: convTime, Kind: harness.EntryInput,
		Input: &harness.InputEntry{
			SessionID: convSessionID, EntryID: entryID, OperationID: operationID,
			Origin: harness.InputOriginUser, Content: parts,
		},
	}
}

func convAssistantFact(entryID, operationID string, entry harness.AssistantEntry) harness.HistoryFact {
	return harness.HistoryFact{
		EntryID: entryID, OperationID: operationID, Sequence: 2, CommittedAt: convTime, Kind: harness.EntryAssistant,
		Assistant: &entry,
	}
}

func convResultFact(entryID, assistantID, callID, status, content, metadata string) harness.HistoryFact {
	result := harness.ToolResultEntry{
		SessionID: convSessionID, EntryID: entryID,
		AssistantEntry: harness.EntryRef{SessionID: convSessionID, EntryID: assistantID},
		ToolCallID:     callID, Status: model.ToolResultStatus(status), Content: content,
	}
	if metadata != "" {
		result.Metadata = json.RawMessage(metadata)
	}
	return harness.HistoryFact{
		EntryID: entryID, Sequence: 3, CommittedAt: convTime, Kind: harness.EntryToolResult,
		ToolResult: &result,
	}
}

// TestConversationItemIDKnownVector pins the exact item identity derivation:
// the unpadded base64url of the first 16 bytes of SHA-256 over the
// Session/entry binding, stable per binding and distinct per binding.
func TestConversationItemIDKnownVector(t *testing.T) {
	const want = "4EngWdrqzp5Q-GPdY7_DFw"
	if got := projectItemID(convSessionID, convEntryID); got != want {
		t.Fatalf("projectItemID = %q, want the pinned vector %q", got, want)
	}
	otherSession := projectItemID("ffffffffffffffffffffffffffffffff", convEntryID)
	otherEntry := projectItemID(convSessionID, "ffffffffffffffffffffffffffffffff")
	if otherSession == want || otherEntry == want || otherSession == otherEntry {
		t.Fatalf("derived identities collide: %q %q %q", want, otherSession, otherEntry)
	}
}

// conversationItems maps one projection's source-known pairs to their
// generated items for the wire-shape oracles; pagination itself consumes the
// pairs and never re-decodes a union variant.
func conversationItems(pairs []projectedItem) []protocol.ConversationItem {
	items := make([]protocol.ConversationItem, len(pairs))
	for i, pair := range pairs {
		items[i] = pair.item
	}
	return items
}

// TestConversationProjectionDeterministicItemKinds proves the exact item
// shapes of every kind from one stable typed snapshot: literal user text
// stays an input, a signal stays typed, an assistant is indivisible with its
// attached and pending calls, and a terminal settlement carries its required
// detail even when the success detail is empty.
func TestConversationProjectionDeterministicItemKinds(t *testing.T) {
	assistantID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa02"
	resultID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb03"
	facts := []harness.HistoryFact{
		convInputFact("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01", "op-1",
			model.ContentPart{Kind: model.PartText, Text: "Operation interrupted."},
			model.ContentPart{Kind: model.PartImageURL, URL: "https://img.test/p.png"},
			model.ContentPart{Kind: model.PartOpaque, OpaqueWireType: "thinking", Extra: model.Extra{"depth": json.RawMessage(`1e1000`)}},
		),
		convAssistantFact(assistantID, "op-1", harness.AssistantEntry{
			SessionID: convSessionID, EntryID: assistantID, OperationID: "op-1",
			Status: model.OutputCompleted, Source: model.ModelRef{Provider: "prov", Model: "m"},
			Content: []model.ContentPart{{Kind: model.PartText, Text: "answer", Extra: model.Extra{"grade": json.RawMessage(`9007199254740993`)}}},
			Refusal: "no",
			Extra:   model.Extra{"model_extra": json.RawMessage(`{"k":1}`)},
			ToolCalls: []harness.ToolCallRecord{
				{ID: "call-1", Ordinal: 0, Name: "read", ArgumentsBase64: convArgs(`{"n":9007199254740993}`), Extra: model.Extra{"tool_extra": json.RawMessage(`{"tool_n":9007199254740993}`)}, NormalizedArguments: json.RawMessage(`{"x":1e1000}`), ResultEntryID: resultID},
				{ID: "call-2", Ordinal: 1, Name: "grep", ArgumentsBase64: convArgs(`[]`), ResultEntryID: "cccccccccccccccccccccccccccccc04"},
			},
		}),
		convResultFact(resultID, assistantID, "call-1", "success", "", `{"rows":1e1000,"zero":0}`),
		{
			EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa05", OperationID: "op-1", Sequence: 4, CommittedAt: convTime, Kind: harness.EntrySignal,
			Signal: &harness.SignalEntry{SessionID: convSessionID, EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa05", OperationID: "op-1", Signal: harness.SignalInterruption, Content: "Operation interrupted."},
		},
		{
			EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa06", Sequence: 5, CommittedAt: convTime, Kind: harness.EntrySignal,
			Signal: &harness.SignalEntry{
				SessionID: convSessionID, EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa06", Signal: harness.SignalBackgroundCompletion,
				RelatedMember: &harness.RelatedMember{Kind: "child", ID: "dddddddddddddddddddddddddddddd07"},
				Content:       "the child finished",
			},
		},
		{
			EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa08", OperationID: "op-2", Sequence: 6, CommittedAt: convTime, Kind: harness.EntryCompaction,
			Compaction: &harness.CompactionEntry{
				SessionID: convSessionID, EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa08", OperationID: "op-2",
				Summary: "summary two", Model: model.ModelRef{Provider: "prov", Model: "m2"},
			},
		},
		{
			EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa09", OperationID: "op-2", Sequence: 7, CommittedAt: convTime, Kind: harness.EntryOperationSettlement,
			Settlement: &harness.OperationSettlementEntry{
				SessionID: convSessionID, EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa09", OperationID: "op-2",
				Status: harness.OperationFailure, Detail: "boom",
			},
		},
		{
			EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa10", OperationID: "op-3", Sequence: 8, CommittedAt: convTime, Kind: harness.EntryOperationSettlement,
			Settlement: &harness.OperationSettlementEntry{
				SessionID: convSessionID, EntryID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa10", OperationID: "op-3",
				Status: harness.OperationSuccess,
			},
		},
	}
	pairs, err := projectConversation(convSessionID, facts)
	if err != nil {
		t.Fatalf("projectConversation: %v", err)
	}
	items := conversationItems(pairs)
	if len(items) != 7 { // the tool result stays absorbed
		t.Fatalf("items = %d, want exactly the 7 projected facts", len(items))
	}

	// The input item keeps its literal text and per-part extras.
	input, err := items[0].AsInputItem()
	if err != nil {
		t.Fatalf("AsInputItem: %v", err)
	}
	if input.ItemId != projectItemID(convSessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01") ||
		input.Origin != protocol.InputOriginUser ||
		!input.CommittedAt.Equal(convTime) || input.OperationId == nil || *input.OperationId != "op-1" {
		t.Fatalf("input item = %+v, want the op-1 user input at the pinned identity", input)
	}
	if len(input.Content) != 3 {
		t.Fatalf("input content = %d parts, want 3", len(input.Content))
	}
	textPart, err := input.Content[0].AsTextPart()
	if err != nil || textPart.Text != "Operation interrupted." {
		t.Fatalf("first part = %+v err %v, want the literal user text", textPart, err)
	}
	imagePart, err := input.Content[1].AsImageURLPart()
	if err != nil || imagePart.Url != "https://img.test/p.png" {
		t.Fatalf("second part = %+v err %v, want the image part", imagePart, err)
	}
	opaquePart, err := input.Content[2].AsOpaquePart()
	if err != nil || opaquePart.OpaqueWireType != "thinking" || opaquePart.Extra == nil || string((*opaquePart.Extra)["depth"]) != `1e1000` {
		t.Fatalf("third part = %+v err %v, want the opaque part with its raw extra", opaquePart, err)
	}

	// The assistant item is indivisible: both calls in published order, the
	// first carrying its terminal outcome, the second pending.
	assistant, err := items[1].AsAssistantItem()
	if err != nil {
		t.Fatalf("AsAssistantItem: %v", err)
	}
	if assistant.Status != protocol.AssistantItemStatusCompleted || assistant.Source != "prov/m" ||
		assistant.ItemId != projectItemID(convSessionID, assistantID) ||
		!assistant.CommittedAt.Equal(convTime) {
		t.Fatalf("assistant item = %+v, want the completed prov/m item at the pinned identity and commit time", assistant)
	}
	if assistant.Extra == nil || string((*assistant.Extra)["model_extra"]) != `{"k":1}` {
		t.Fatalf("assistant extra = %v, want the validated message-level extra", assistant.Extra)
	}
	if assistant.Refusal == nil || *assistant.Refusal != "no" {
		t.Fatalf("assistant refusal = %v, want the projected refusal", assistant.Refusal)
	}
	textPart, err = assistant.Content[0].AsTextPart()
	if err != nil || textPart.Extra == nil || string((*textPart.Extra)["grade"]) != `9007199254740993` {
		t.Fatalf("assistant part extra = %+v err %v, want the raw number preserved", textPart, err)
	}
	if len(assistant.ToolCalls) != 2 ||
		assistant.ToolCalls[0].Id != "call-1" || assistant.ToolCalls[1].Id != "call-2" {
		t.Fatalf("tool calls = %+v, want call-1 then call-2 in published order", assistant.ToolCalls)
	}
	call1 := assistant.ToolCalls[0]
	if call1.Arguments != `{"n":9007199254740993}` {
		t.Fatalf("call-1 arguments = %q, want the decoded raw text", call1.Arguments)
	}
	// The call-level extra is its own level, independent of the message and
	// content-part extras, with the same raw-number fidelity.
	if call1.Extra == nil || string((*call1.Extra)["tool_extra"]) != `{"tool_n":9007199254740993}` {
		t.Fatalf("call-1 extra = %v, want the call-level extra with its raw number", call1.Extra)
	}
	if call1.NormalizedArguments == nil || string((*call1.NormalizedArguments)["x"]) != `1e1000` {
		t.Fatalf("call-1 normalized arguments = %v, want the raw object", call1.NormalizedArguments)
	}
	if call1.Status == nil || *call1.Status != protocol.ToolCallStatusSuccess ||
		call1.Content == nil || *call1.Content != "" {
		t.Fatalf("call-1 terminal outcome = (%v, %v), want success with a present empty content", call1.Status, call1.Content)
	}
	if call1.Metadata == nil || string(*call1.Metadata) != `{"rows":1e1000,"zero":0}` {
		t.Fatalf("call-1 metadata = %v, want the raw value preserved", call1.Metadata)
	}
	call2 := assistant.ToolCalls[1]
	if call2.Status != nil || call2.Content != nil || call2.Metadata != nil || call2.NormalizedArguments != nil {
		t.Fatalf("pending call-2 = %+v, want every terminal member omitted", call2)
	}
	if call2.Arguments != `[]` {
		t.Fatalf("call-2 arguments = %q, want the decoded raw text", call2.Arguments)
	}

	// The signals carry their typed subtypes; the background completion names
	// its related member and no operation. Every kind's item identity is the
	// fixture entry bound to this fixture Session's namespace.
	interruption, err := items[2].AsSignalItem()
	if err != nil || interruption.Subtype != protocol.SignalItemSubtypeInterruption ||
		interruption.Content != "Operation interrupted." || interruption.OperationId == nil {
		t.Fatalf("interruption item = %+v err %v, want the typed interruption signal", interruption, err)
	}
	if interruption.ItemId != projectItemID(convSessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa05") {
		t.Fatalf("interruption item_id = %q, want the fixture entry's namespace identity", interruption.ItemId)
	}
	completion, err := items[3].AsSignalItem()
	if err != nil || completion.Subtype != protocol.SignalItemSubtypeBackgroundCompletion ||
		completion.Content != "the child finished" || completion.OperationId != nil ||
		completion.RelatedMember == nil || completion.RelatedMember.Kind != protocol.BackgroundMemberKindChild || completion.RelatedMember.Id != "dddddddddddddddddddddddddddddd07" {
		t.Fatalf("background completion item = %+v err %v, want the operationless typed signal with its member", completion, err)
	}
	if completion.ItemId != projectItemID(convSessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa06") {
		t.Fatalf("completion item_id = %q, want the fixture entry's namespace identity", completion.ItemId)
	}

	// The compaction and terminal outcomes, each at its own entry's identity.
	compaction, err := items[4].AsCompactionItem()
	if err != nil || compaction.Summary != "summary two" || compaction.Model != "prov/m2" ||
		compaction.OperationId == nil || *compaction.OperationId != "op-2" {
		t.Fatalf("compaction item = %+v err %v, want the op-2 compaction", compaction, err)
	}
	if compaction.ItemId != projectItemID(convSessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa08") {
		t.Fatalf("compaction item_id = %q, want the fixture entry's namespace identity", compaction.ItemId)
	}
	failureEnd, err := items[5].AsOperationEndItem()
	if err != nil || failureEnd.Status != protocol.OperationEndItemStatusFailure || failureEnd.Detail != "boom" {
		t.Fatalf("failure end item = %+v err %v, want the failure detail", failureEnd, err)
	}
	if failureEnd.ItemId != projectItemID(convSessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa09") {
		t.Fatalf("failure end item_id = %q, want the fixture entry's namespace identity", failureEnd.ItemId)
	}
	successEnd, err := items[6].AsOperationEndItem()
	if err != nil || successEnd.Status != protocol.OperationEndItemStatusSuccess || successEnd.Detail != "" {
		t.Fatalf("success end item = %+v err %v, want the required present empty detail", successEnd, err)
	}
	if successEnd.ItemId != projectItemID(convSessionID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa10") {
		t.Fatalf("success end item_id = %q, want the fixture entry's namespace identity", successEnd.ItemId)
	}

	// Every item's wire shape is exactly its kind's schema surface, the raw
	// entry identities never leave the facts, and the required arrays stay
	// present.
	for i, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal item %d: %v", i, err)
		}
		for _, raw := range []string{assistantID, resultID, "cccccccccccccccccccccccccccccc04"} {
			if strings.Contains(string(data), raw) {
				t.Fatalf("item %d wire = %s, want no raw entry identity %q", i, data, raw)
			}
		}
		if strings.Contains(string(data), "arguments_base64") || strings.Contains(string(data), "result_entry_id") {
			t.Fatalf("item %d wire = %s, want no internal payload keys", i, data)
		}
	}
	assertExactJSONKeys(t, items[0], []string{
		"committed_at", "content", "item_id", "kind", "operation_id", "origin",
	})
	assertExactJSONKeys(t, items[1], []string{
		"committed_at", "content", "extra", "extra.model_extra", "item_id", "kind", "operation_id",
		"refusal", "source", "status", "tool_calls",
	})
	empty, err := json.Marshal(items[0])
	if err != nil {
		t.Fatalf("marshal input item: %v", err)
	}
	if !strings.Contains(string(empty), `"content":[`) {
		t.Fatalf("input item wire = %s, want a present content array", empty)
	}

	// Determinism: a second projection of the same snapshot is identical.
	againPairs, err := projectConversation(convSessionID, facts)
	if err != nil {
		t.Fatalf("second projectConversation: %v", err)
	}
	again := conversationItems(againPairs)
	first, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal first projection: %v", err)
	}
	second, err := json.Marshal(again)
	if err != nil {
		t.Fatalf("marshal second projection: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("second projection = %s, want the deterministic %s", second, first)
	}
}

// TestConversationProjectionSameCallIDAcrossOperations proves the tool-result
// binding when two Operations of one Session publish the same call ID: call
// identities are unique per Operation, while the reserved result entry
// identities are Session-unique — each assistant item keeps its own result's
// content and metadata, at its own independent item identity. The fixture's
// distinct result identities discriminate a call-ID-only shortcut; the
// result-identity lookup itself is owned by the validated facts' reservation
// rule, whose uniqueness and binding the Harness graph validation proves.
func TestConversationProjectionSameCallIDAcrossOperations(t *testing.T) {
	assistantA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa02"
	resultA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb03"
	assistantB := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa05"
	resultB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb06"
	toolCalls := []harness.ToolCallRecord{{ID: "call-1", Ordinal: 0, Name: "read", ArgumentsBase64: convArgs(`{}`)}}
	facts := []harness.HistoryFact{
		convAssistantFact(assistantA, "op-1", harness.AssistantEntry{
			SessionID: convSessionID, EntryID: assistantA, OperationID: "op-1",
			Status: model.OutputCompleted, Source: model.ModelRef{Provider: "prov", Model: "m"},
			Content: []model.ContentPart{{Kind: model.PartText, Text: "first answer"}},
			ToolCalls: []harness.ToolCallRecord{func() harness.ToolCallRecord {
				call := toolCalls[0]
				call.ResultEntryID = resultA
				return call
			}()},
		}),
		convResultFact(resultA, assistantA, "call-1", "success", "answer-a", `{"which":"a"}`),
		convAssistantFact(assistantB, "op-2", harness.AssistantEntry{
			SessionID: convSessionID, EntryID: assistantB, OperationID: "op-2",
			Status: model.OutputCompleted, Source: model.ModelRef{Provider: "prov", Model: "m"},
			Content: []model.ContentPart{{Kind: model.PartText, Text: "second answer"}},
			ToolCalls: []harness.ToolCallRecord{func() harness.ToolCallRecord {
				call := toolCalls[0]
				call.ResultEntryID = resultB
				return call
			}()},
		}),
		convResultFact(resultB, assistantB, "call-1", "success", "answer-b", `{"which":"b"}`),
	}
	pairs, err := projectConversation(convSessionID, facts)
	if err != nil {
		t.Fatalf("projectConversation: %v", err)
	}
	items := conversationItems(pairs)
	if len(items) != 2 { // the results stay absorbed
		t.Fatalf("items = %d, want exactly the two assistants", len(items))
	}
	first, err := items[0].AsAssistantItem()
	if err != nil {
		t.Fatalf("AsAssistantItem: %v", err)
	}
	second, err := items[1].AsAssistantItem()
	if err != nil {
		t.Fatalf("AsAssistantItem: %v", err)
	}
	// Independent item identities in the shared Session namespace.
	if first.ItemId != projectItemID(convSessionID, assistantA) ||
		second.ItemId != projectItemID(convSessionID, assistantB) ||
		first.ItemId == second.ItemId {
		t.Fatalf("item identities = (%q, %q), want each assistant's own namespace identity", first.ItemId, second.ItemId)
	}
	// Each projected call carries its own assistant's result, never the
	// same-ID sibling's.
	if first.ToolCalls[0].Content == nil || *first.ToolCalls[0].Content != "answer-a" ||
		first.ToolCalls[0].Metadata == nil || string(*first.ToolCalls[0].Metadata) != `{"which":"a"}` {
		t.Fatalf("first call = %+v, want its own answer-a result", first.ToolCalls[0])
	}
	if second.ToolCalls[0].Content == nil || *second.ToolCalls[0].Content != "answer-b" ||
		second.ToolCalls[0].Metadata == nil || string(*second.ToolCalls[0].Metadata) != `{"which":"b"}` {
		t.Fatalf("second call = %+v, want its own answer-b result", second.ToolCalls[0])
	}
}

// TestConversationProjectionMetadataShapeFidelity pins the tool-owned
// metadata representation: every non-null JSON kind is preserved verbatim
// through the projection and the generated item's re-encode, while absent
// metadata stays omitted.
func TestConversationProjectionMetadataShapeFidelity(t *testing.T) {
	for name, raw := range map[string]string{
		"object":      `{"n":9007199254740993}`,
		"array":       `[1, null, "x"]`,
		"string":      `"line count"`,
		"bool":        `true`,
		"zero":        `0`,
		"nested null": `{"rows": [null, {"depth": 1}]}`,
		"large int":   `9007199254740993`,
		"extreme exp": `1e1000`,
	} {
		t.Run(name, func(t *testing.T) {
			assistantID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa02"
			resultID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb03"
			facts := []harness.HistoryFact{
				convAssistantFact(assistantID, "op-1", harness.AssistantEntry{
					SessionID: convSessionID, EntryID: assistantID, OperationID: "op-1",
					Status: model.OutputCompleted, Source: model.ModelRef{Provider: "prov", Model: "m"},
					Content:   []model.ContentPart{{Kind: model.PartText, Text: "answer"}},
					ToolCalls: []harness.ToolCallRecord{{ID: "call-1", Ordinal: 0, Name: "read", ArgumentsBase64: convArgs(`{}`), ResultEntryID: resultID}},
				}),
				convResultFact(resultID, assistantID, "call-1", "success", "out", raw),
			}
			pairs, err := projectConversation(convSessionID, facts)
			if err != nil {
				t.Fatalf("projectConversation: %v", err)
			}
			items := conversationItems(pairs)
			// The generated union's own encoding compacts raw values, so the
			// fidelity contract is the compacted byte form: every value stays
			// byte-exact after compaction, never re-decoded to a Go float.
			var compacted bytes.Buffer
			if err := json.Compact(&compacted, []byte(raw)); err != nil {
				t.Fatalf("compacting %s: %v", raw, err)
			}
			assistant, err := items[0].AsAssistantItem()
			if err != nil {
				t.Fatalf("AsAssistantItem: %v", err)
			}
			if assistant.ToolCalls[0].Metadata == nil || string(*assistant.ToolCalls[0].Metadata) != compacted.String() {
				t.Fatalf("projected metadata = %v, want %s preserved", assistant.ToolCalls[0].Metadata, compacted.String())
			}
			data, err := json.Marshal(items[0])
			if err != nil {
				t.Fatalf("marshal item: %v", err)
			}
			if !strings.Contains(string(data), compacted.String()) {
				t.Fatalf("item wire = %s, want %s preserved", data, compacted.String())
			}
		})
	}

	// Absent metadata stays omitted on every terminal status.
	assistantID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa02"
	resultID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb03"
	facts := []harness.HistoryFact{
		convAssistantFact(assistantID, "op-1", harness.AssistantEntry{
			SessionID: convSessionID, EntryID: assistantID, OperationID: "op-1",
			Status: model.OutputCompleted, Source: model.ModelRef{Provider: "prov", Model: "m"},
			Content:   []model.ContentPart{{Kind: model.PartText, Text: "answer"}},
			ToolCalls: []harness.ToolCallRecord{{ID: "call-1", Ordinal: 0, Name: "read", ArgumentsBase64: convArgs(`{}`), ResultEntryID: resultID}},
		}),
		convResultFact(resultID, assistantID, "call-1", "denied", "Permission denied.", ""),
	}
	pairs, err := projectConversation(convSessionID, facts)
	if err != nil {
		t.Fatalf("projectConversation: %v", err)
	}
	items := conversationItems(pairs)
	assistant, err := items[0].AsAssistantItem()
	if err != nil {
		t.Fatalf("AsAssistantItem: %v", err)
	}
	if assistant.ToolCalls[0].Metadata != nil ||
		assistant.ToolCalls[0].Status == nil || *assistant.ToolCalls[0].Status != protocol.ToolCallStatusDenied {
		t.Fatalf("denied call = %+v, want the denied status and omitted metadata", assistant.ToolCalls[0])
	}
}

// TestConversationProjectionRawArgumentText proves the decoded argument text
// carries no JSON validity gate: malformed and non-UTF-8 argument bytes
// project to the native JSON string encoding's replacement text, never to an
// error or a base64 field.
func TestConversationProjectionRawArgumentText(t *testing.T) {
	assistantID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa02"
	facts := []harness.HistoryFact{
		convAssistantFact(assistantID, "op-1", harness.AssistantEntry{
			SessionID: convSessionID, EntryID: assistantID, OperationID: "op-1",
			Status: model.OutputCompleted, Source: model.ModelRef{Provider: "prov", Model: "m"},
			Content: []model.ContentPart{{Kind: model.PartText, Text: "answer"}},
			ToolCalls: []harness.ToolCallRecord{
				{ID: "call-1", Ordinal: 0, Name: "read", ArgumentsBase64: convArgs(`[1,2,`), ResultEntryID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb03"},
				{ID: "call-2", Ordinal: 1, Name: "read", ArgumentsBase64: convArgs("\xff\xfe"), ResultEntryID: "cccccccccccccccccccccccccccccc04"},
			},
		}),
	}
	pairs, err := projectConversation(convSessionID, facts)
	if err != nil {
		t.Fatalf("projectConversation: %v", err)
	}
	items := conversationItems(pairs)
	assistant, err := items[0].AsAssistantItem()
	if err != nil {
		t.Fatalf("AsAssistantItem: %v", err)
	}
	if assistant.ToolCalls[0].Arguments != `[1,2,` {
		t.Fatalf("malformed arguments = %q, want the raw text ungated", assistant.ToolCalls[0].Arguments)
	}
	if !strings.ContainsRune(assistant.ToolCalls[1].Arguments, '\uFFFD') {
		t.Fatalf("non-UTF-8 arguments = %q, want the replacement text", assistant.ToolCalls[1].Arguments)
	}
	data, err := json.Marshal(items[0])
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	if strings.Contains(string(data), "arguments_base64") {
		t.Fatalf("item wire = %s, want no base64 arguments", data)
	}
}

// TestConversationProjectionHookEvidenceExcluded proves, over real validated
// Harness facts, that a committed argument-hook execution never reaches the
// client conversation: the hook ran, its evidence entry is in storage, and
// the projection carries only the conversation items.
func TestConversationProjectionHookEvidenceExcluded(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newPrepEnv(t, store)
		t.Cleanup(func() { e.converge() })
		turn := []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage(` {"x": 1} `)}}
		e.model = func(selection) func(context.Context, model.Request) (model.Stream, error) {
			return func(context.Context, model.Request) (model.Stream, error) {
				next := turn
				turn = nil
				if len(next) > 0 {
					return &prepStream{calls: next}, nil
				}
				return &prepStream{}, nil
			}
		}
		session := e.session("hooky")
		e.admit(session, "op-1", "one")
		e.awaitCleanups(1)

		runtimeSeen, _ := e.argHooks[0].received()
		if len(runtimeSeen) != 1 {
			t.Fatalf("argument hook received = %v, want the one real call", runtimeSeen)
		}
		entries, err := store.ReadEntries(context.Background(), session, 0)
		if err != nil {
			t.Fatalf("ReadEntries: %v", err)
		}
		var hookEntries int
		for _, entry := range entries {
			if entry.Kind == harness.EntryHookResult {
				hookEntries++
			}
		}
		if hookEntries != 4 { // the hooky type's four argument hooks, one entry each
			t.Fatalf("hook evidence entries = %d, want the four committed hook_result entries", hookEntries)
		}

		snap, err := e.h.SnapshotSession(context.Background(), session)
		if err != nil {
			t.Fatalf("SnapshotSession: %v", err)
		}
		for _, fact := range snap.Facts {
			if fact.Kind == harness.EntryHookResult {
				t.Fatal("hook evidence became a client fact")
			}
		}
		pairs, err := projectConversation(snap.Session.Identity.SessionID, snap.Facts)
		if err != nil {
			t.Fatalf("projectConversation: %v", err)
		}
		items := conversationItems(pairs)
		if len(items) != 4 { // input, publishing assistant, continuation assistant, end
			t.Fatalf("items = %d (%s), want exactly the conversation items", len(items), itemKinds(t, items))
		}
		for _, item := range items {
			data, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal item: %v", err)
			}
			if strings.Contains(string(data), "hook") {
				t.Fatalf("item wire = %s, want no hook evidence", data)
			}
		}
		// The publishing assistant keeps the call and its normalized arguments.
		assistant, err := items[1].AsAssistantItem()
		if err != nil || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Id != "call-1" ||
			assistant.ToolCalls[0].Status == nil || *assistant.ToolCalls[0].Status != protocol.ToolCallStatusError {
			t.Fatalf("publishing assistant = %+v err %v, want the settled call-1", assistant, err)
		}
	})
}

// TestConversationProjectionToolCallLifecycle proves, over real validated
// facts, the whole call lifecycle: a settled call carries its terminal
// outcome inside its indivisible assistant item, a later commit fills a
// pending call under the same item ID, and one assistant's published calls
// stay ordered and together.
func TestConversationProjectionToolCallLifecycle(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		stopper := newLifecycleStopper(store)
		bg := openBackgroundLifecycle(t, store, stopper)
		defer closeProjectionRuntime(bg.r) // failure-safe: runs after the release below
		session := bg.session("lifecycle")
		releaseTool := make(chan struct{})
		releaseToolOnce := sync.OnceFunc(func() { close(releaseTool) })
		defer releaseToolOnce()
		toolStarted := make(chan struct{}, 1)
		arrivals := make(chan int, 8)
		bg.prep.setSessionScript(session, &lifecycleScript{
			advertise: []string{"read", "grep"},
			model: func(_ context.Context, _ string, attempt int, _ model.Request) (model.Stream, error) {
				select {
				case arrivals <- attempt:
				default:
				}
				switch attempt {
				case 1:
					return lifecycleCallTurn("call-1", "read", `{"n":9007199254740993}`), nil
				case 2:
					return lifecycleCallTurn("call-2", "grep", `{"q":"late"}`), nil
				case 3:
					position := 0
					return &lifecycleTurn{delta: model.StreamDelta{
						HasChoice: true, Role: "assistant",
						ToolFragments: []model.ToolCallFragment{
							{Position: &position, ID: "call-3", Name: "read", ArgumentFragment: `{"third":1}`},
							{Position: &position, ID: "call-4", Name: "read", ArgumentFragment: `{"fourth":2}`},
						},
						FinishReason: "tool_calls",
					}}, nil
				default:
					return lifecycleTextTurn("done"), nil
				}
			},
			tool: func(_ context.Context, _ string, call model.ToolCall) harness.PreparedTool {
				switch call.ID {
				case "call-1":
					return harness.PreparedTool{
						Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}},
						Immediate: &harness.ToolOutcome{
							Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "first"},
							Metadata: json.RawMessage(`{"n":9007199254740993}`),
						},
					}
				case "call-2": // parks: its result lands after the pending read
					return harness.PreparedTool{
						Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}},
						Execute: func(context.Context) harness.ToolOutcome {
							select {
							case toolStarted <- struct{}{}:
							default:
							}
							<-releaseTool
							return harness.ToolOutcome{
								Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "second"},
								Metadata: json.RawMessage(`{"late":true}`),
							}
						},
					}
				default:
					return harness.PreparedTool{
						Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}},
						Immediate:   &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: call.ID + " ran"}},
					}
				}
			},
		})

		submitThroughRuntime(t, bg.r, session, "op-1", "work")
		awaitArrival(t, arrivals, 2, "attempt 2")
		<-toolStarted
		pending := projectSessionItems(t, bg.r, session)
		if len(pending) != 3 { // input, publishing assistant, pending assistant
			t.Fatalf("pending items = %d, want the input and both assistants", len(pending))
		}
		first, err := pending[1].AsAssistantItem()
		if err != nil || len(first.ToolCalls) != 1 || first.ToolCalls[0].Status == nil ||
			first.ToolCalls[0].Content == nil || *first.ToolCalls[0].Content != "first" ||
			first.ToolCalls[0].Metadata == nil || string(*first.ToolCalls[0].Metadata) != `{"n":9007199254740993}` {
			t.Fatalf("settled assistant = %+v err %v, want call-1 attached with its metadata", first, err)
		}
		second, err := pending[2].AsAssistantItem()
		if err != nil || len(second.ToolCalls) != 1 || second.ToolCalls[0].Id != "call-2" ||
			second.ToolCalls[0].Status != nil || second.ToolCalls[0].Content != nil || second.ToolCalls[0].Metadata != nil {
			t.Fatalf("pending assistant = %+v err %v, want call-2 with every terminal member omitted", second, err)
		}
		pendingIDs := itemIDs(t, pending)

		releaseToolOnce()
		awaitArrival(t, arrivals, 4, "attempt 4")
		awaitOperation(t, bg.r, session, "op-1", harness.OperationSuccess)

		attached := projectSessionItems(t, bg.r, session)
		attachedIDs := itemIDs(t, attached)
		if len(attached) != 6 { // + the two-call assistant, the closing assistant
			t.Fatalf("attached items = %d, want the complete conversation", len(attached))
		}
		for i, id := range pendingIDs {
			if attachedIDs[i] != id {
				t.Fatalf("item %d identity drifted from %q to %q", i, id, attachedIDs[i])
			}
		}
		second, err = attached[2].AsAssistantItem()
		if err != nil || second.ToolCalls[0].Status == nil || *second.ToolCalls[0].Status != protocol.ToolCallStatusSuccess ||
			second.ToolCalls[0].Content == nil || *second.ToolCalls[0].Content != "second" ||
			second.ToolCalls[0].Metadata == nil || string(*second.ToolCalls[0].Metadata) != `{"late":true}` {
			t.Fatalf("late-attached call = %+v err %v, want the second outcome under the same item", second, err)
		}
		pair, err := attached[3].AsAssistantItem()
		if err != nil || len(pair.ToolCalls) != 2 ||
			pair.ToolCalls[0].Id != "call-3" || pair.ToolCalls[1].Id != "call-4" ||
			pair.ToolCalls[0].Status == nil || pair.ToolCalls[1].Status == nil {
			t.Fatalf("multi-call assistant = %+v err %v, want both calls attached in published order", pair, err)
		}
		if err := bg.r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// awaitArrival waits for the scripted model function's attempt counter to
// reach want: the barrier proving every earlier effect committed.
func awaitArrival(t *testing.T, arrivals <-chan int, want int, name string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	last := 0
	for {
		select {
		case attempt := <-arrivals:
			last = attempt
			if attempt >= want {
				return
			}
		case <-deadline:
			t.Fatalf("%s never arrived (last attempt %d)", name, last)
		}
	}
}

// projectSessionItems snapshots one Session through the Runtime and projects
// its complete conversation.
func projectSessionItems(t *testing.T, r *Runtime, sessionID string) []protocol.ConversationItem {
	t.Helper()
	snap := snapshotThroughRuntime(t, r, sessionID)
	pairs, err := projectConversation(sessionID, snap.Facts)
	if err != nil {
		t.Fatalf("projectConversation: %v", err)
	}
	items := conversationItems(pairs)
	return items
}

// submitConvergedThroughRuntime submits one regular message and waits for its
// operation's terminal state. The tolerated disposition set is exactly the
// buffered-disposition helper's closed rule — the retiring-run window routes
// to the steering or queued buffer — but that helper lives in the external
// runtime_test suite and cannot reach the private projector this file tests,
// so the one-rule check repeats here; awaitOperation owns the convergence.
func submitConvergedThroughRuntime(t *testing.T, r *Runtime, sessionID, operationID, text string, want harness.OperationState) {
	t.Helper()
	var disposition harness.SubmitDisposition
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		res, submitErr := h.Submit(ctx, harness.SubmitRequest{
			SessionID:   sessionID,
			OperationID: operationID,
			Origin:      harness.InputOriginUser,
			Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
			Mode:        harness.MessageModeRegular,
		})
		if submitErr == nil {
			disposition = res.Disposition
		}
		return submitErr
	})
	if err != nil {
		t.Fatalf("Submit(%q): %v", operationID, err)
	}
	switch disposition {
	case harness.DispositionAdmitted, harness.DispositionSteering, harness.DispositionQueued:
	default:
		t.Fatalf("Submit(%q) = %+v, want an admitted or buffered disposition", operationID, disposition)
	}
	awaitOperation(t, r, sessionID, operationID, want)
}

func itemIDs(t *testing.T, items []protocol.ConversationItem) []string {
	t.Helper()
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = itemIdentity(t, item)
	}
	return out
}

// itemIdentity reads one item's kind and identity member.
func itemIdentity(t *testing.T, item protocol.ConversationItem) string {
	t.Helper()
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	var probe struct {
		Kind   string `json:"kind"`
		ItemID string `json:"item_id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("decode item identity: %v", err)
	}
	return probe.Kind + ":" + probe.ItemID
}

// itemKind reports one projected item's discriminator kind.
func itemKind(t *testing.T, item protocol.ConversationItem) string {
	t.Helper()
	kind, err := item.Discriminator()
	if err != nil {
		t.Fatalf("item discriminator: %v", err)
	}
	return kind
}

// TestConversationProjectionSyntheticToolResult proves a synthetic error
// outcome projects as the call's terminal state with no invented metadata.
func TestConversationProjectionSyntheticToolResult(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		stopper := newLifecycleStopper(store)
		bg := openBackgroundLifecycle(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		session := bg.session("synthetic")
		bg.prep.setSessionScript(session, &lifecycleScript{
			advertise: []string{"read"},
			model: func(_ context.Context, _ string, attempt int, _ model.Request) (model.Stream, error) {
				if attempt == 1 {
					return lifecycleCallTurn("call-1", "read", `{}`), nil
				}
				return lifecycleTextTurn("done"), nil
			},
			tool: func(_ context.Context, _ string, call model.ToolCall) harness.PreparedTool {
				return harness.PreparedTool{
					Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no concrete tools yet"}},
				}
			},
		})
		submitThroughRuntime(t, bg.r, session, "op-1", "work")
		awaitOperation(t, bg.r, session, "op-1", harness.OperationSuccess)

		items := projectSessionItems(t, bg.r, session)
		if len(items) != 4 { // input, assistant with the synthetic result, closing assistant, end
			t.Fatalf("items = %d, want the complete conversation", len(items))
		}
		assistant, err := items[1].AsAssistantItem()
		if err != nil || len(assistant.ToolCalls) != 1 ||
			assistant.ToolCalls[0].Status == nil || *assistant.ToolCalls[0].Status != protocol.ToolCallStatusError ||
			assistant.ToolCalls[0].Content == nil || *assistant.ToolCalls[0].Content != "no concrete tools yet" ||
			assistant.ToolCalls[0].Metadata != nil {
			t.Fatalf("synthetic result call = %+v err %v, want the error outcome and no metadata", assistant.ToolCalls, err)
		}
		if err := bg.r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestConversationProjectionSyntheticInterruptedResult proves, over real
// facts, the interrupted synthetic terminal result: an unstarted published
// call's reserved identity receives the fixed interrupted outcome — present
// content, no invented metadata — when the interruption settles every
// remaining pending call, while the already-started call's real returned
// outcome wins the cancellation race.
func TestConversationProjectionSyntheticInterruptedResult(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		stopper := newLifecycleStopper(store)
		bg := openBackgroundLifecycle(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		session := bg.session("interrupted-synthetic")
		releaseTool := make(chan struct{})
		releaseToolOnce := sync.OnceFunc(func() { close(releaseTool) })
		defer releaseToolOnce()
		toolStarted := make(chan struct{}, 1)
		bg.prep.setSessionScript(session, &lifecycleScript{
			advertise: []string{"read"},
			model: func(_ context.Context, _ string, attempt int, _ model.Request) (model.Stream, error) {
				if attempt == 1 {
					position := 0
					return &lifecycleTurn{delta: model.StreamDelta{
						HasChoice: true, Role: "assistant",
						ToolFragments: []model.ToolCallFragment{
							{Position: &position, ID: "call-1", Name: "read", ArgumentFragment: `{}`},
							{Position: &position, ID: "call-2", Name: "read", ArgumentFragment: `{}`},
						},
						FinishReason: "tool_calls",
					}}, nil
				}
				return lifecycleTextTurn("never reached"), nil
			},
			tool: func(ctx context.Context, _ string, call model.ToolCall) harness.PreparedTool {
				return harness.PreparedTool{
					Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}},
					Execute: func(execCtx context.Context) harness.ToolOutcome {
						select {
						case toolStarted <- struct{}{}:
						default:
						}
						select {
						case <-releaseTool: // never released on the main path
						case <-execCtx.Done(): // the interrupt releases the started call
						}
						// A returned real outcome wins the cancellation race.
						return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "first ran"}}
					},
				}
			},
		})
		submitThroughRuntime(t, bg.r, session, "op-1", "work")
		select {
		case <-toolStarted:
		case <-time.After(10 * time.Second):
			t.Fatal("the first call never started")
		}
		if err := bg.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			return h.Interrupt(ctx, session)
		}); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		awaitOperation(t, bg.r, session, "op-1", harness.OperationInterruption)

		items := projectSessionItems(t, bg.r, session)
		// input, the two-call assistant, the typed interruption signal, the end.
		if len(items) != 4 {
			t.Fatalf("items = %d (%s), want the complete interrupted conversation", len(items), itemKinds(t, items))
		}
		assistant, err := items[1].AsAssistantItem()
		if err != nil || len(assistant.ToolCalls) != 2 {
			t.Fatalf("assistant = %+v err %v, want both published calls indivisible", assistant, err)
		}
		started := assistant.ToolCalls[0]
		if started.Id != "call-1" || started.Status == nil || *started.Status != protocol.ToolCallStatusSuccess ||
			started.Content == nil || *started.Content != "first ran" || started.Metadata != nil {
			t.Fatalf("started call = %+v, want the real returned outcome", started)
		}
		unstarted := assistant.ToolCalls[1]
		if unstarted.Id != "call-2" || unstarted.Status == nil || *unstarted.Status != protocol.ToolCallStatusInterrupted ||
			unstarted.Content == nil || *unstarted.Content != "Tool call interrupted." || unstarted.Metadata != nil {
			t.Fatalf("unstarted call = %+v, want the synthetic interrupted result and no metadata", unstarted)
		}
		signal, err := items[2].AsSignalItem()
		if err != nil || signal.Subtype != protocol.SignalItemSubtypeInterruption {
			t.Fatalf("signal = %+v err %v, want the typed interruption", signal, err)
		}
		end, err := items[3].AsOperationEndItem()
		if err != nil || end.Status != protocol.OperationEndItemStatusInterruption || end.Detail == "" {
			t.Fatalf("end = %+v err %v, want the interruption detail", end, err)
		}
	})
}

// TestConversationProjectionErroredPartialAssistant proves the errored
// partial axis: a stream that fails after yielding its partial answer keeps
// that content visible, rides the typed model_failure_continuation signal,
// and the operation's later failure still ends the conversation in committed
// order — distinct from both the interruption and the no-assistant failure.
func TestConversationProjectionErroredPartialAssistant(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		stopper := newLifecycleStopper(store)
		bg := openBackgroundLifecycle(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		session := bg.session("errored")
		arrivals := make(chan int, 8)
		bg.prep.setSessionScript(session, &lifecycleScript{
			model: func(_ context.Context, _ string, attempt int, _ model.Request) (model.Stream, error) {
				select {
				case arrivals <- attempt:
				default:
				}
				if attempt == 1 { // yields the partial answer, then fails
					return &compactPartialStream{fail: errors.New("chunk parse failed")}, nil
				}
				return nil, errors.New("model down") // nonretryable: settles failure
			},
		})
		submitThroughRuntime(t, bg.r, session, "op-1", "work")
		// Attempt 2's arrival is the rendezvous proving attempt 1's errored
		// partial assistant and continuation signal committed.
		awaitArrival(t, arrivals, 2, "attempt 2")
		awaitOperation(t, bg.r, session, "op-1", harness.OperationFailure)

		items := projectSessionItems(t, bg.r, session)
		if len(items) != 4 ||
			itemKind(t, items[0]) != "input" || itemKind(t, items[1]) != "assistant" ||
			itemKind(t, items[2]) != "signal" || itemKind(t, items[3]) != "operation_end" {
			t.Fatalf("items = %d (%s), want the committed order input, assistant, signal, end", len(items), itemKinds(t, items))
		}
		partial, err := items[1].AsAssistantItem()
		if err != nil || partial.Status != protocol.AssistantItemStatusErrored {
			t.Fatalf("partial assistant = %+v err %v, want the errored status", partial, err)
		}
		textPart, err := partial.Content[0].AsTextPart()
		if err != nil || textPart.Text != "partial answer" {
			t.Fatalf("partial content = %+v, want the retained fragment", partial.Content)
		}
		continuation, err := items[2].AsSignalItem()
		if err != nil || continuation.Subtype != protocol.SignalItemSubtypeModelFailureContinuation ||
			continuation.Content != "The previous model response failed after partial output. Continue from the retained response." {
			t.Fatalf("continuation signal = %+v err %v, want the typed signal with its required content", continuation, err)
		}
		end, err := items[3].AsOperationEndItem()
		if err != nil || end.Status != protocol.OperationEndItemStatusFailure || !strings.Contains(end.Detail, "model down") {
			t.Fatalf("end = %+v err %v, want the failure detail", end, err)
		}
	})
}

// TestConversationProjectionInterruptedPartialAssistant proves a stream
// interruption after partial output keeps the errored/interrupted assistant
// visible beside its typed signal and terminal end.
func TestConversationProjectionInterruptedPartialAssistant(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		stopper := newLifecycleStopper(store)
		bg := openBackgroundLifecycle(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		session := bg.session("interrupted")
		fragmentSent := make(chan struct{}, 1)
		bg.prep.setSessionScript(session, &lifecycleScript{
			model: func(ctx context.Context, _ string, attempt int, _ model.Request) (model.Stream, error) {
				if attempt > 1 {
					return lifecycleTextTurn("done"), nil
				}
				return &interruptPartialStream{ctx: ctx, sent: fragmentSent}, nil
			},
		})
		submitThroughRuntime(t, bg.r, session, "op-1", "work")
		select {
		case <-fragmentSent:
		case <-time.After(10 * time.Second):
			t.Fatal("the partial fragment never streamed")
		}
		if err := bg.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			return h.Interrupt(ctx, session)
		}); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		awaitOperation(t, bg.r, session, "op-1", harness.OperationInterruption)

		items := projectSessionItems(t, bg.r, session)
		var sawAssistant, sawSignal, sawEnd bool
		for _, item := range items {
			switch itemKind(t, item) {
			case "assistant":
				assistant, err := item.AsAssistantItem()
				if err != nil {
					t.Fatalf("AsAssistantItem: %v", err)
				}
				if assistant.Status != protocol.AssistantItemStatusInterrupted {
					t.Fatalf("assistant status = %q, want the interrupted partial", assistant.Status)
				}
				textPart, err := assistant.Content[0].AsTextPart()
				if err != nil || textPart.Text != "partial answer" {
					t.Fatalf("partial content = %+v, want the retained fragment", assistant.Content)
				}
				sawAssistant = true
			case "signal":
				signal, err := item.AsSignalItem()
				if err != nil {
					t.Fatalf("AsSignalItem: %v", err)
				}
				if signal.Subtype != protocol.SignalItemSubtypeInterruption || signal.Content != "Operation interrupted." {
					t.Fatalf("signal item = %+v, want the typed interruption", signal)
				}
				sawSignal = true
			case "operation_end":
				end, err := item.AsOperationEndItem()
				if err != nil {
					t.Fatalf("AsOperationEndItem: %v", err)
				}
				if end.Status != protocol.OperationEndItemStatusInterruption || end.Detail == "" {
					t.Fatalf("end item = %+v, want the interruption detail", end)
				}
				sawEnd = true
			}
		}
		if !sawAssistant || !sawSignal || !sawEnd {
			t.Fatalf("interrupted conversation = %d items (assistant %v signal %v end %v), want all three", len(items), sawAssistant, sawSignal, sawEnd)
		}
		if err := bg.r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// interruptPartialStream yields one text fragment and then fails when the run
// context is canceled: the assembler finalizes an interrupted partial output.
type interruptPartialStream struct {
	ctx  context.Context
	sent chan struct{}
	done bool
}

func (s *interruptPartialStream) Recv() (model.StreamDelta, error) {
	if !s.done {
		s.done = true
		select {
		case s.sent <- struct{}{}:
		default:
		}
		return model.StreamDelta{
			HasChoice:        true,
			Role:             "assistant",
			ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "partial answer"}},
		}, nil
	}
	<-s.ctx.Done()
	return model.StreamDelta{}, s.ctx.Err()
}

func (s *interruptPartialStream) Close() error { return nil }

// TestConversationProjectionForkPrefixIsIndependent proves a copied fork
// prefix projects with new identities, no Operation ownership, and no source
// identity — while the source keeps its own items unchanged.
func TestConversationProjectionForkPrefixIsIndependent(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		stopper := newLifecycleStopper(store)
		bg := openBackgroundLifecycle(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		root := bg.session("fork-root")
		bg.prep.setSessionScript(root, &lifecycleScript{
			advertise: []string{"read"},
			model: func(_ context.Context, _ string, attempt int, _ model.Request) (model.Stream, error) {
				if attempt == 1 {
					return lifecycleCallTurn("call-1", "read", `{"n":9007199254740993}`), nil
				}
				return lifecycleTextTurn("done"), nil
			},
			tool: func(_ context.Context, _ string, call model.ToolCall) harness.PreparedTool {
				return harness.PreparedTool{
					Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}},
					Immediate: &harness.ToolOutcome{
						Result:   model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "first"},
						Metadata: json.RawMessage(`{"n":9007199254740993}`),
					},
				}
			},
		})
		submitThroughRuntime(t, bg.r, root, "op-1", "hello fork")
		awaitOperation(t, bg.r, root, "op-1", harness.OperationSuccess)
		awaitIdleSession(t, bg.r, root)
		submitConvergedThroughRuntime(t, bg.r, root, "op-2", "second turn", harness.OperationSuccess)
		awaitIdleSession(t, bg.r, root)
		// The fork boundary is the second user input: the prefix inherited by
		// the destination is the first turn's input and its completed tool
		// assistant.
		boundary := inputEntryID(t, bg.r, root, "op-2")

		forked := forkThroughRuntime(t, bg.r, root, boundary, "fork-op-1")
		awaitOperation(t, bg.r, forked, "fork-op-1", harness.OperationSuccess)

		sourceItems := projectSessionItems(t, bg.r, root)
		forkItems := projectSessionItems(t, bg.r, forked)
		// The fork's prefix repeats the source's first turn with new
		// identities and no Operation ownership: the call-turn assistant, its
		// closing text assistant, and the input. A standalone tool-result
		// item would break the count.
		if len(forkItems) != 6 { // the three copied items plus the fork's own turn
			t.Fatalf("fork items = %d (%s), want the copied prefix plus the fork's own turn", len(forkItems), itemKinds(t, forkItems))
		}
		sourceIDs := make(map[string]bool)
		for _, item := range sourceItems {
			sourceIDs[itemIdentity(t, item)] = true
			data, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal source item: %v", err)
			}
			if strings.Contains(string(data), forked) {
				t.Fatalf("source item wire = %s, want no fork identity", data)
			}
		}
		for i, item := range forkItems[:3] {
			data, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal fork item: %v", err)
			}
			if strings.Contains(string(data), root) {
				t.Fatalf("fork prefix item wire = %s, want no source identity", data)
			}
			if sourceIDs[itemIdentity(t, item)] {
				t.Fatalf("fork prefix item %d shares the source identity %q", i, itemIdentity(t, item))
			}
			if strings.Contains(string(data), `"operation_id"`) {
				t.Fatalf("fork prefix item wire = %s, want no Operation ownership", data)
			}
		}

		// The copied assistant's call is a real completed tool turn: the
		// fork copied its reserved result under new identities, so the
		// destination's item keeps the decoded arguments, normalized
		// arguments, and terminal outcome inside the assistant — under a
		// destination-namespace identity.
		copiedAssistant, err := forkItems[1].AsAssistantItem()
		if err != nil {
			t.Fatalf("AsAssistantItem: %v", err)
		}
		copiedAssistantEntry := ""
		forkSnap := snapshotThroughRuntime(t, bg.r, forked)
		for _, fact := range forkSnap.Facts {
			// The first copied assistant (committed before its own closing
			// text assistant and the fork's own turn) publishes call-1; all
			// copied assistants carry the cleared source Operation.
			if fact.Kind == harness.EntryAssistant && fact.OperationID == "" {
				copiedAssistantEntry = fact.EntryID
				break
			}
		}
		if copiedAssistantEntry == "" {
			t.Fatal("the fork copied no assistant entry")
		}
		if copiedAssistant.ItemId != projectItemID(forked, copiedAssistantEntry) {
			t.Fatalf("copied assistant item_id = %q, want the destination namespace identity", copiedAssistant.ItemId)
		}
		if copiedAssistant.OperationId != nil {
			t.Fatalf("copied assistant operation_id = %v, want the cleared source attribution", copiedAssistant.OperationId)
		}
		call := copiedAssistant.ToolCalls[0]
		if call.Id != "call-1" || call.Arguments != `{"n":9007199254740993}` ||
			call.NormalizedArguments == nil || string((*call.NormalizedArguments)["n"]) != `9007199254740993` ||
			call.Status == nil || *call.Status != protocol.ToolCallStatusSuccess ||
			call.Content == nil || *call.Content != "first" ||
			call.Metadata == nil || string(*call.Metadata) != `{"n":9007199254740993}` {
			t.Fatalf("copied tool call = %+v, want the attached copied result with its fidelity", call)
		}

		// The fork's own admission keeps its Operation attribution.
		forkInput, err := forkItems[3].AsInputItem()
		if err != nil || forkInput.Origin != protocol.InputOriginUser || forkInput.OperationId == nil || *forkInput.OperationId != "fork-op-1" {
			t.Fatalf("fork input = %+v err %v, want the fork operation's own input", forkInput, err)
		}
		if err := bg.r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestConversationProjectionChildAndBackgroundCompletion proves, over real
// facts, the child's conversation parity and both completion surfaces: the
// parent's runtime-origin completion input, and the durable
// background_completion signal naming its member.
func TestConversationProjectionChildAndBackgroundCompletion(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		stopper := newLifecycleStopper(store)
		bg := openBackgroundLifecycle(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		stopper.arm(bg.r.harness)
		root := bg.session("completion-root")
		bg.prep.setAgentScript("worker", &lifecycleScript{
			model: func(_ context.Context, _ string, _ int, _ model.Request) (model.Stream, error) {
				return lifecycleTextTurn("child finished"), nil
			},
		})
		child := launchChildThroughRuntime(t, bg.r, root, "child-op-1")
		awaitOperation(t, bg.r, child, "child-op-1", harness.OperationSuccess)

		completionID := bg.startJob(child, "cc000003")
		stopper.addDelivery(child, completionID, "job report")
		if err := bg.stop(child); err != nil {
			t.Fatalf("Stop(child): %v", err)
		}

		// The child's conversation: its own turn plus the typed completion
		// signal naming the job member.
		childItems := projectSessionItems(t, bg.r, child)
		var sawSignal bool
		for _, item := range childItems {
			if itemKind(t, item) != "signal" {
				continue
			}
			signal, err := item.AsSignalItem()
			if err != nil {
				t.Fatalf("AsSignalItem: %v", err)
			}
			if signal.Subtype != protocol.SignalItemSubtypeBackgroundCompletion || signal.Content != "job report" ||
				signal.RelatedMember == nil || signal.RelatedMember.Kind != protocol.BackgroundMemberKindJob || signal.RelatedMember.Id != "cc000003" {
				t.Fatalf("completion signal = %+v, want the job member's typed record", signal)
			}
			sawSignal = true
		}
		if !sawSignal {
			t.Fatalf("child items = %s, want the background_completion signal item", itemKinds(t, childItems))
		}

		// The parent's conversation gains the child's completion as a
		// runtime-origin input under the completion identity; the delivery
		// converges after the child's run retirement.
		var rootItems []protocol.ConversationItem
		deadline := time.Now().Add(10 * time.Second)
		for {
			rootItems = projectSessionItems(t, bg.r, root)
			if hasRuntimeOriginInput(t, rootItems, "child finished") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("root items = %s, want the runtime-origin completion input", itemKinds(t, rootItems))
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := bg.r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// hasRuntimeOriginInput reports whether one projected item list contains a
// runtime-origin input with the given text.
func hasRuntimeOriginInput(t *testing.T, items []protocol.ConversationItem, want string) bool {
	t.Helper()
	for _, item := range items {
		if itemKind(t, item) != "input" {
			continue
		}
		input, err := item.AsInputItem()
		if err != nil {
			t.Fatalf("AsInputItem: %v", err)
		}
		if input.Origin != protocol.InputOriginRuntime || len(input.Content) == 0 {
			continue
		}
		textPart, err := input.Content[0].AsTextPart()
		if err == nil && strings.Contains(textPart.Text, want) {
			return true
		}
	}
	return false
}

// itemKinds renders one projection's kinds in order for failure messages.
func itemKinds(t *testing.T, items []protocol.ConversationItem) string {
	t.Helper()
	kinds := make([]string, len(items))
	for i, item := range items {
		kinds[i] = itemKind(t, item)
	}
	return strings.Join(kinds, ",")
}

// TestConversationProjectionCompactionTwiceRetainsEarlierItems proves two
// compactions contribute their own items while every earlier client item
// stays pageable: no model-summary truncation of the client history.
func TestConversationProjectionCompactionTwiceRetainsEarlierItems(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		f := openCompactLifecycle(t, store)
		defer closeProjectionRuntime(f.r)
		s := f.session("twice")
		f.submit(s, "op-1", "hello first")
		awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
		awaitIdleSession(t, f.r, s)
		f.compactIdle(s, "compact-1")
		f.submit(s, "op-2", "hello second")
		awaitOperation(t, f.r, s, "op-2", harness.OperationSuccess)
		awaitIdleSession(t, f.r, s)
		f.compactIdle(s, "compact-2")
		awaitOperation(t, f.r, s, "compact-2", harness.OperationSuccess)

		items := projectSessionItems(t, f.r, s)
		var compactions int
		var sawFirstInput, sawFirstAssistant, sawSecondInput, sawSecondAssistant bool
		var sawEnds int
		for _, item := range items {
			switch itemKind(t, item) {
			case "compaction":
				compaction, err := item.AsCompactionItem()
				if err != nil {
					t.Fatalf("AsCompactionItem: %v", err)
				}
				compactions++
				if compaction.Summary == "" || compaction.Model != "prov/m" {
					t.Fatalf("compaction item = %+v, want the summary and model", compaction)
				}
			case "input":
				input, err := item.AsInputItem()
				if err != nil {
					t.Fatalf("AsInputItem: %v", err)
				}
				textPart, err := input.Content[0].AsTextPart()
				if err != nil {
					t.Fatalf("input part: %v", err)
				}
				switch textPart.Text {
				case "hello first":
					sawFirstInput = true
				case "hello second":
					sawSecondInput = true
				}
			case "assistant":
				assistant, err := item.AsAssistantItem()
				if err != nil {
					t.Fatalf("AsAssistantItem: %v", err)
				}
				textPart, err := assistant.Content[0].AsTextPart()
				if err != nil {
					t.Fatalf("assistant part: %v", err)
				}
				if textPart.Text == "done" {
					if !sawFirstAssistant && !sawSecondAssistant {
						sawFirstAssistant = true
					} else {
						sawSecondAssistant = true
					}
				}
			case "operation_end":
				end, err := item.AsOperationEndItem()
				if err != nil {
					t.Fatalf("AsOperationEndItem: %v", err)
				}
				if end.Status != protocol.OperationEndItemStatusSuccess {
					t.Fatalf("end item = %+v, want the settled success", end)
				}
				sawEnds++
			}
		}
		if compactions != 2 {
			t.Fatalf("compaction items = %d, want both compactions visible", compactions)
		}
		if !sawFirstInput || !sawFirstAssistant || !sawSecondInput || !sawSecondAssistant || sawEnds != 4 {
			t.Fatalf("retained items = (input %v/%v assistant %v/%v ends %d), want every earlier item and all four ends",
				sawFirstInput, sawSecondInput, sawFirstAssistant, sawSecondAssistant, sawEnds)
		}
		if err := f.r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestConversationProjectionTerminalFailureWithoutAssistant proves a
// pre-acceptance model failure projects the terminal failure end with its
// required detail and no fabricated assistant — while a literal user message
// that repeats a signal's fixed text stays an input item.
func TestConversationProjectionTerminalFailureWithoutAssistant(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		stopper := newLifecycleStopper(store)
		bg := openBackgroundLifecycle(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		session := bg.session("failure")
		bg.prep.setSessionScript(session, &lifecycleScript{
			model: func(_ context.Context, _ string, _ int, _ model.Request) (model.Stream, error) {
				return lifecycleTextTurn("done"), nil
			},
		})
		// The literal text that exactly matches a fixed signal's content.
		submitThroughRuntime(t, bg.r, session, "op-1", "Operation interrupted.")
		awaitOperation(t, bg.r, session, "op-1", harness.OperationSuccess)
		awaitIdleSession(t, bg.r, session)

		items := projectSessionItems(t, bg.r, session)
		if len(items) != 3 { // the literal input, its assistant, the success end
			t.Fatalf("first turn items = %d (%s), want input, assistant, end", len(items), itemKinds(t, items))
		}
		literal, err := items[0].AsInputItem()
		if err != nil {
			t.Fatalf("first item: %v", err)
		}
		textPart, err := literal.Content[0].AsTextPart()
		if err != nil || textPart.Text != "Operation interrupted." {
			t.Fatalf("first item = %+v, want the literal input", literal)
		}

		// The failing turn: pre-acceptance failure, no assistant at all.
		bg.prep.setSessionScript(session, &lifecycleScript{
			model: func(_ context.Context, _ string, _ int, _ model.Request) (model.Stream, error) {
				return nil, errors.New("model down")
			},
		})
		submitConvergedThroughRuntime(t, bg.r, session, "op-2", "again", harness.OperationFailure)

		items = projectSessionItems(t, bg.r, session)
		if len(items) != 5 { // + the failing input and its failure end
			t.Fatalf("items = %d (%s), want the prefix plus input and failure end", len(items), itemKinds(t, items))
		}
		failureEnd, err := items[4].AsOperationEndItem()
		if err != nil {
			t.Fatalf("last item: %v", err)
		}
		failingInput, err := items[3].AsInputItem()
		if err != nil {
			t.Fatalf("fourth item: %v", err)
		}
		if failureEnd.Status != protocol.OperationEndItemStatusFailure ||
			!strings.Contains(failureEnd.Detail, "model down") || failingInput.OperationId == nil || *failingInput.OperationId != "op-2" {
			t.Fatalf("failure projection = (end %+v, input %+v), want the failed op-2 end with its detail", failureEnd, failingInput)
		}

		// The no-output interruption on a fresh Session: the model effect
		// arrives and cooperatively parks without yielding any content, the
		// existing Harness Interrupt settles the terminal interruption, and
		// no assistant is fabricated — the input, the typed interruption
		// signal, and the end are the whole conversation.
		interrupted := bg.session("no-output-interruption")
		interruptions := make(chan int, 8)
		park := make(chan struct{})
		parkOnce := sync.OnceFunc(func() { close(park) })
		defer parkOnce() // LIFO: releases before the owner close joins
		bg.prep.setSessionScript(interrupted, &lifecycleScript{
			model: func(mctx context.Context, _ string, attempt int, _ model.Request) (model.Stream, error) {
				select {
				case interruptions <- attempt:
				default:
				}
				select {
				case <-park:
				case <-mctx.Done(): // the interrupt releases the parked effect
				}
				return nil, mctx.Err()
			},
		})
		submitThroughRuntime(t, bg.r, interrupted, "op-int", "hold")
		select {
		case <-interruptions:
		case <-time.After(10 * time.Second):
			t.Fatal("the model effect never arrived")
		}
		if err := bg.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			return h.Interrupt(ctx, interrupted)
		}); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		awaitOperation(t, bg.r, interrupted, "op-int", harness.OperationInterruption)

		items = projectSessionItems(t, bg.r, interrupted)
		if len(items) != 3 ||
			itemKind(t, items[0]) != "input" || itemKind(t, items[1]) != "signal" || itemKind(t, items[2]) != "operation_end" {
			t.Fatalf("interruption items = %d (%s), want input, signal, end with no assistant", len(items), itemKinds(t, items))
		}
		// Every identity is the stable namespace derivation of its own
		// committed fact entry.
		wantEntries := make(map[harness.EntryKind]string, 3)
		interruptedSnap := snapshotThroughRuntime(t, bg.r, interrupted)
		for _, fact := range interruptedSnap.Facts {
			switch fact.Kind {
			case harness.EntryInput, harness.EntrySignal, harness.EntryOperationSettlement:
				wantEntries[fact.Kind] = fact.EntryID
			}
		}
		if len(wantEntries) != 3 {
			t.Fatalf("committed facts = %v, want exactly input, signal, settlement", wantEntries)
		}
		interruptionInput, err := items[0].AsInputItem()
		if err != nil || interruptionInput.Origin != protocol.InputOriginUser ||
			interruptionInput.OperationId == nil || *interruptionInput.OperationId != "op-int" ||
			interruptionInput.ItemId != projectItemID(interrupted, wantEntries[harness.EntryInput]) {
			t.Fatalf("interruption input = %+v err %v, want the op-int input at its fact identity", interruptionInput, err)
		}
		interruptionSignal, err := items[1].AsSignalItem()
		if err != nil || interruptionSignal.Subtype != protocol.SignalItemSubtypeInterruption ||
			interruptionSignal.Content != "Operation interrupted." ||
			interruptionSignal.ItemId != projectItemID(interrupted, wantEntries[harness.EntrySignal]) {
			t.Fatalf("interruption signal = %+v err %v, want the typed signal at its fact identity", interruptionSignal, err)
		}
		interruptedEnd, err := items[2].AsOperationEndItem()
		if err != nil || interruptedEnd.Status != protocol.OperationEndItemStatusInterruption ||
			interruptedEnd.Detail == "" ||
			interruptedEnd.ItemId != projectItemID(interrupted, wantEntries[harness.EntryOperationSettlement]) {
			t.Fatalf("interruption end = %+v err %v, want the interruption detail at its fact identity", interruptedEnd, err)
		}
		// The identities are stable across an immediate second read.
		if again := itemIDs(t, projectSessionItems(t, bg.r, interrupted)); !reflect.DeepEqual(again, itemIDs(t, items)) {
			t.Fatalf("re-read identities = %v, want the stable %v", again, itemIDs(t, items))
		}

		if err := bg.r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestConversationProjectionOverEmptySession proves an idle Session projects
// the present empty item list with no error.
func TestConversationProjectionOverEmptySession(t *testing.T) {
	r, _ := openProjectionRuntime(t, storage.NewMemory())
	defer closeProjectionRuntime(r)
	session := projectionSession(t, r, "/tmp/conv-empty", "solo").Identity.SessionID
	items := projectSessionItems(t, r, session)
	if items == nil || len(items) != 0 {
		t.Fatalf("items = %v, want the present empty list", items)
	}
}
