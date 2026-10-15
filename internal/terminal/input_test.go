package terminal

import "testing"

func TestInputHistoryNavigationRestoresDraft(t *testing.T) {
	h := NewInputHistory()
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
	h := NewInputHistory()
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

func TestCompleteSlashCommandCompletesAgainstSuppliedList(t *testing.T) {
	commands := []string{"/help", "/clear", "/exit"}
	cases := []struct{ text, want string }{
		{"/cl", "/clear "}, // one match gets a trailing space
		{"/e", "/exit "},   // one match from the list
		{"/", "/"},         // every command matches; the common prefix adds nothing
		{"cl", "cl"},       // non-slash input is returned unchanged
		{"/nope", "/nope"}, // no match is returned unchanged
	}
	for _, tc := range cases {
		if got := CompleteSlashCommand(tc.text, commands); got != tc.want {
			t.Errorf("CompleteSlashCommand(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
	// Multiple matches extend to their longest common prefix.
	if got, want := CompleteSlashCommand("/al", []string{"/alpha", "/alpine"}), "/alp"; got != want {
		t.Errorf("CompleteSlashCommand(/al) = %q, want %q", got, want)
	}
}
