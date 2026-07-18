package agent

import (
	"testing"

	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
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
		model:       coremodel.ModelRef{Provider: "p", Model: "m"},
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
	if len(hs.Messages) != 1 || hs.Messages[0].Content != "q" {
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
	if hs.Tokens.Total.Input != 7 {
		t.Fatalf("tokens = %+v", hs.Tokens)
	}
	if hs.Model.Provider != "p" || hs.Model.Model != "m" {
		t.Fatalf("model = %+v", hs.Model)
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

// TestHydrateSessionWithBoundaryEmitsCapturedStateOnce verifies the atomic-capture
// wrapper invokes emit exactly once with the resolved session's state, and once
// with the zero state (a detach) for an empty id.
func TestHydrateSessionWithBoundaryEmitsCapturedStateOnce(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	id, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	var got []HydrationState
	a.HydrateSessionWithBoundary(id, func(hs HydrationState) { got = append(got, hs) })
	if len(got) != 1 {
		t.Fatalf("emit called %d times, want 1", len(got))
	}
	if got[0].Session.ID != id {
		t.Fatalf("boundary session = %q, want %q", got[0].Session.ID, id)
	}

	got = nil
	a.HydrateSessionWithBoundary("", func(hs HydrationState) { got = append(got, hs) })
	if len(got) != 1 || got[0].Session.ID != "" {
		t.Fatalf("detach = %#v, want one zero state", got)
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

	if _, err := a.HydrateSession("missing-session"); err == nil {
		t.Fatal("HydrateSession(missing) = nil error, want unknown session")
	}
}
