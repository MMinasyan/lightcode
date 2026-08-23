package agent

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/engine/message"
)

// TestHydrationStateFromCompleteState verifies the conversion maps every captured
// class into the hydration state, preserving the tail/error sequences and the
// capture cursor.
func TestHydrationStateFromCompleteState(t *testing.T) {
	cs := completeState{
		transcript: completeTranscript{
			committed: []DisplayMessage{{Type: "user", Content: "q", Turn: 1}},
			tail:      []tailRow{{seq: 5, msg: DisplayMessage{Type: "assistant", Content: "hi"}}},
			errors:    []errorRow{{seq: 6, turn: 2, msg: DisplayMessage{Type: "error", Content: "boom"}}},
			revision:  captureRevision{committedTurn: 1, committedSeq: 4, rewriteEpoch: 2},
		},
		tokens:      TokenReport{Total: TokenEntry{Input: 7}},
		model:       ModelInfo{Ref: "p/m", Provider: "p", Model: "m"},
		busy:        true,
		compacting:  true,
		queue:       QueueState{Items: []QueuedItem{{ID: "q1"}}, Version: 3},
		warnings:    []PromptWarning{{Kind: "k", Message: "w"}},
		permissions: nil,
	}

	hs := hydrationStateFrom(SessionSummary{ID: "s1"}, cs)

	if hs.Session.ID != "s1" {
		t.Fatalf("session = %+v", hs.Session)
	}
	if len(hs.Messages) != 3 || hs.Messages[0].Content != "q" || hs.Messages[1].Content != "hi" || hs.Messages[2].Content != "boom" {
		t.Fatalf("messages = %#v", hs.Messages)
	}
	if len(hs.Tail) != 1 || hs.Tail[0].Seq != 5 || hs.Tail[0].Message.Content != "hi" {
		t.Fatalf("tail = %#v", hs.Tail)
	}
	if len(hs.Errors) != 1 || hs.Errors[0].Seq != 6 || hs.Errors[0].Message.Content != "boom" {
		t.Fatalf("errors = %#v", hs.Errors)
	}
	if hs.Cursor != (HydrationCursor{CommittedTurn: 1, CommittedSeq: 4, RewriteEpoch: 2}) {
		t.Fatalf("cursor = %+v", hs.Cursor)
	}
	if !hs.TranscriptReplay {
		t.Fatal("live hydration transcriptReplay = false, want true")
	}
	if hs.Tokens.Total.Input != 7 {
		t.Fatalf("tokens = %+v", hs.Tokens)
	}
	if hs.Model.Ref != "p/m" || hs.Model.Provider != "p" || hs.Model.Model != "m" {
		t.Fatalf("model = %+v, want resolved p/m", hs.Model)
	}
	if !hs.Busy || !hs.Compacting {
		t.Fatalf("activity busy=%v compacting=%v", hs.Busy, hs.Compacting)
	}
	if hs.Queue.Version != 3 || len(hs.Queue.Items) != 1 {
		t.Fatalf("queue = %+v", hs.Queue)
	}
	if len(hs.Warnings) != 1 || hs.Warnings[0].Kind != "k" {
		t.Fatalf("warnings = %#v", hs.Warnings)
	}
}

func TestProjectHydrationMessagesPreservesTurnAndLiveOrder(t *testing.T) {
	committed := []DisplayMessage{
		{Type: "user", Content: "turn one", Turn: 1},
		{Type: "tool", Content: "tool one", Turn: 1},
		{Type: "user", Content: "turn two", Turn: 2},
	}
	tail := []tailRow{
		{seq: 13, msg: DisplayMessage{Type: "assistant", Content: "live", Turn: 3}},
		{seq: 16, msg: DisplayMessage{Type: "tool", Content: "later live", Turn: 2}},
	}
	errors := []errorRow{
		{seq: 10, turn: 1, msg: DisplayMessage{Type: "error", Content: "first error", Turn: 1}},
		{seq: 11, turn: 1, msg: DisplayMessage{Type: "error", Content: "second error", Turn: 1}},
		{seq: 12, turn: 2, msg: DisplayMessage{Type: "error", Content: "second turn error", Turn: 2}},
		{seq: 14, turn: 3, msg: DisplayMessage{Type: "error", Content: "live error", Turn: 3}},
		{seq: 15, turn: 0, msg: DisplayMessage{Type: "error", Content: "unattributed", Turn: 0}},
	}

	got := projectHydrationMessages(committed, tail, errors)
	var contents []string
	for _, msg := range got {
		contents = append(contents, msg.Content)
	}
	want := []string{"turn one", "tool one", "first error", "second error", "turn two", "second turn error", "live", "live error", "unattributed", "later live"}
	if len(contents) != len(want) {
		t.Fatalf("projection length = %d, want %d: %v", len(contents), len(want), contents)
	}
	for i := range want {
		if contents[i] != want[i] {
			t.Fatalf("projection[%d] = %q, want %q: %v", i, contents[i], want[i], contents)
		}
	}
}

// TestHydrateSessionResolvesAndCaptures verifies the method resolves a live session
// and returns its complete state.
func TestHydrateSessionResolvesAndCaptures(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	hs, err := a.HydrateSession(id)
	if err != nil {
		t.Fatalf("HydrateSession: %v", err)
	}
	if hs.Session.ID != id {
		t.Fatalf("hydrated session = %q, want %q", hs.Session.ID, id)
	}
	// The captured state carries the resolved model shape — identifier plus
	// catalog display name — so a snapshot-applying adapter can present the
	// session's model without a separate fetch.
	if hs.Model.Ref != "test/test-model" || hs.Model.Provider != "test" || hs.Model.Model != "test-model" ||
		hs.Model.DisplayName != "Test Model" || hs.Model.ContextWindow != 8192 {
		t.Fatalf("hydrated model = %+v, want resolved test/test-model (Test Model)", hs.Model)
	}

	if _, err := a.HydrateSession("missing-session"); err == nil {
		t.Fatal("HydrateSession(missing) = nil error, want unknown session")
	}
}

// TestSessionMessagesPointLookupIgnoresCommittedTurn proves the point-lookup
// exception: SessionMessagesFor reads the complete durable history with no
// committed bound, so the same durably marked turn is visible to the point
// lookup while live hydration excludes it from the durable half until the
// coordinator commits. The marked turn renders exactly once in the live
// snapshot (from the retained tail), never twice.
func TestSessionMessagesPointLookupIgnoresCommittedTurn(t *testing.T) {
	a := newLiveCatalogBackedTestAgent(t)
	unit := a.session
	id := sessionIDOf(unit)
	tr := a.transcriptForSessionID(id)

	turn := unit.store.BeginTurn()
	feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: id, ProjectID: unit.projectID, Turn: turn})
	feedTranscript(tr, Event{Kind: EventUserMessageDisplay, SessionID: id, Turn: turn, Result: "marked row"})
	data, err := json.Marshal(message.NewText(message.RoleUser, "marked row"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := unit.store.AppendMessage(turn, data); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := unit.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("MarkTurnComplete: %v", err)
	}

	// The point lookup is unbounded: the marked turn is visible here.
	msgs, err := a.SessionMessagesFor(id)
	if err != nil {
		t.Fatalf("SessionMessagesFor: %v", err)
	}
	found := 0
	for _, m := range msgs {
		if m.Content == "marked row" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("point lookup returns %q %d times, want exactly once (the unbounded lookup reads every complete turn)", "marked row", found)
	}

	// Live hydration is bounded by the coordinator's committed turn: the
	// marked turn is excluded from the durable half and renders once from the
	// retained tail.
	hs, err := a.HydrateSession(id)
	if err != nil {
		t.Fatalf("HydrateSession: %v", err)
	}
	if len(hs.Messages) != 1 {
		t.Fatalf("live hydration has %d display rows, want 1 (the marked turn is carried by the projection)", len(hs.Messages))
	}
	if got := countHydrationContent(hs, "marked row"); got != 1 {
		t.Fatalf("live hydration returns %q %d times, want exactly once (the tail carries the uncommitted row)", "marked row", got)
	}
	if hs.Cursor.CommittedTurn != 0 {
		t.Fatalf("cursor committedTurn = %d, want 0 (nothing committed yet)", hs.Cursor.CommittedTurn)
	}
}
