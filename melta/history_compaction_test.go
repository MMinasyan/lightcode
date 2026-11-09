package melta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// TestHistoryCompactionRetainsOldCursor proves, over both stores, the full
// combination: a history of more than one page, an older cursor issued before
// a real manual compaction stays valid with its page unchanged, and the
// complete original history plus the compaction item stays pageable — every
// post-compaction page carrying the current revision, and the compaction item
// carrying the real scripted model's summary. It is the producer-level
// counterpart of the historical mounted generated-client row; the host's
// instance qualification is covered by the mounted projection test.
func TestHistoryCompactionRetainsOldCursor(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		session := newLifecycleID(t)
		usageSeedSession(t, store, session, "", "", "solo", emptyUsageWireTotals())
		for i := 1; i <= 18; i++ { // 18 settled turns × 3 items = 54 items
			compactRawSuccessOperation(t, store, session, fmt.Sprintf("op-%d", i), fmt.Sprintf("question op-%d", i), fmt.Sprintf("answer op-%d", i))
		}
		r, _ := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		caller := testCaller(r)
		get := func(cursor *string) (protocol.HistoryPage, error) {
			return r.GetHistory(ctx, caller, session, cursor)
		}

		// The pre-compaction paging: the newest 50 and the older 4, with the
		// older cursor anchored at the fourth item.
		page1, err := get(nil)
		testassert.Require(t, err == nil, "initial history: %v", err)
		if len(page1.Items) != 50 || page1.OlderCursor == nil {
			body, _ := json.Marshal(page1)
			t.Fatalf("initial history = %s, want exactly 50 items with an older cursor", body)
		}
		page2, err := get(page1.OlderCursor)
		testassert.Require(t, err == nil, "older history: %v", err)
		if len(page2.Items) != 4 || page2.OlderCursor != nil {
			body, _ := json.Marshal(page2)
			t.Fatalf("older history = %s, want the remaining 4 items with no cursor", body)
		}
		preItems := append(append([]protocol.ConversationItem{}, page1.Items...), page2.Items...)
		testassert.Equal(t, len(preItems), 54)
		preRevision := page1.SessionRevision.DurableRevision
		oldCursor := *page1.OlderCursor

		// The real manual compaction through the public command.
		compacted, err := r.CompactSession(ctx, caller, session, protocol.CompactRequest{OperationId: "compact-1"})
		testassert.NoError(t, err)
		testassert.Require(t, compacted.OperationId == "compact-1", "compact = %+v, want the admitted compact Operation", compacted)
		awaitIdleSession(t, r, session)

		// The old cursor stays valid: the same four items, no compaction
		// item, nothing older, and the current revision.
		page2After, err := get(&oldCursor)
		testassert.Require(t, err == nil, "older history after compaction: %v", err)
		if got := strings.Join(pageItemIDs(t, page2After), ","); got != strings.Join(pageItemIDs(t, page2), ",") {
			t.Fatalf("old-cursor page after compaction = %s, want the unchanged %s", got, strings.Join(pageItemIDs(t, page2), ","))
		}
		testassert.Require(t, page2After.OlderCursor == nil && page2After.SessionRevision.DurableRevision != preRevision, "old-cursor page after compaction produced an older cursor %v with nothing older; old-cursor page revision = %s, want the advanced current revision", page2After.OlderCursor, page2After.SessionRevision.DurableRevision)

		// The complete history plus the compaction item stays pageable: walk
		// every page and collect the items.
		var walked []protocol.ConversationItem
		var currentRevision string
		firstPageLen := 0
		for cursor := (*string)(nil); ; {
			page, err := get(cursor)
			testassert.NoError(t, err)
			if cursor == nil {
				firstPageLen = len(page.Items)
			}
			walked = append(walked, page.Items...)
			currentRevision = page.SessionRevision.DurableRevision
			testassert.Require(t, currentRevision != preRevision, "walk page revision = %s, want the advanced current revision", currentRevision)
			if page.OlderCursor == nil {
				break
			}
			cursor = page.OlderCursor
		}
		testassert.Equal(t, len(walked), 56)
		// Exactly one compaction item, carrying the real scripted model's
		// summary, and the compact Operation's settlement beside it — the
		// first page's two newest items.
		var compactions []*protocol.CheckpointItem
		compactionAt := -1
		for i, item := range walked {
			if itemKind(t, item) != "context_checkpoint" {
				continue
			}
			testassert.Require(t, compactionAt < 0, "second compaction item at walk position %d", i)
			value, err := item.AsCheckpointItem()
			testassert.Require(t, err == nil, "compaction item %d: %v", i, err)
			compactions = append(compactions, &value)
			compactionAt = i
		}
		testassert.Equal(t, compactionAt, firstPageLen-2)
		firstText := ""
		if len(compactions) == 1 && len(compactions[0].Messages) > 0 && compactions[0].Messages[0].Content != nil {
			parts := *compactions[0].Messages[0].Content
			if len(parts) > 0 {
				if text, terr := parts[0].AsTextPart(); terr == nil {
					firstText = text.Text
				}
			}
		}
		if len(compactions) != 1 || firstText != "[Previous conversation summary]\n\ndone\n\n[End of summary. Continue from here.]" || compactions[0].Model == nil || *compactions[0].Model != "prov/m" {
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
