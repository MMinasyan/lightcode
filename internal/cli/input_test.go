package cli

import "testing"

func TestInputHistoryNavigationRestoresDraft(t *testing.T) {
	h := newInputHistory()
	h.Reset([]string{"first", "second"})

	if got, ok := h.Prev("draft"); !ok || got != "second" {
		t.Fatalf("Prev() = %q, %v; want second, true", got, ok)
	}
	if got, ok := h.Prev("ignored"); !ok || got != "first" {
		t.Fatalf("Prev() = %q, %v; want first, true", got, ok)
	}
	if _, ok := h.Prev("ignored"); ok {
		t.Fatalf("Prev() at oldest entry returned ok")
	}
	if got, ok := h.Next(); !ok || got != "second" {
		t.Fatalf("Next() = %q, %v; want second, true", got, ok)
	}
	if got, ok := h.Next(); !ok || got != "draft" {
		t.Fatalf("Next() = %q, %v; want draft, true", got, ok)
	}
	if _, ok := h.Next(); ok {
		t.Fatalf("Next() at draft returned ok")
	}
}

func TestInputHistoryAddSkipsConsecutiveDuplicates(t *testing.T) {
	h := newInputHistory()
	h.Add("same")
	h.Add("same")
	h.Add("next")

	if got, ok := h.Prev(""); !ok || got != "next" {
		t.Fatalf("Prev() = %q, %v; want next, true", got, ok)
	}
	if got, ok := h.Prev(""); !ok || got != "same" {
		t.Fatalf("Prev() = %q, %v; want same, true", got, ok)
	}
	if _, ok := h.Prev(""); ok {
		t.Fatalf("duplicate entry was stored")
	}
}

func TestCompleteSlashCommandIncludesClear(t *testing.T) {
	if got, want := completeSlashCommand("/cl"), "/clear "; got != want {
		t.Fatalf("completeSlashCommand(/cl) = %q, want %q", got, want)
	}
}
