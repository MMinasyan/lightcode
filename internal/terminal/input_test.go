package terminal

import (
	"github.com/MMinasyan/lightcode/internal/testassert"
	"testing"
)

func TestInputHistoryNavigationRestoresDraft(t *testing.T) {
	h := NewInputHistory()
	h.Reset([]string{"first", "second"})

	{
		got, ok := h.Prev("draft")
		testassert.Require(t, ok && got == "second", "Prev() = %q, %v; want second, true", got, ok)
	}
	{
		got, ok := h.Prev("ignored")
		testassert.Require(t, ok && got == "first", "Prev() = %q, %v; want first, true", got, ok)
	}
	{
		_, ok := h.Prev("ignored")
		testassert.Require(t, !ok, "Prev() at oldest entry returned ok")
	}
	{
		got, ok := h.Next()
		testassert.Require(t, ok && got == "second", "Next() = %q, %v; want second, true", got, ok)
	}
	{
		got, ok := h.Next()
		testassert.Require(t, ok && got == "draft", "Next() = %q, %v; want draft, true", got, ok)
	}
	{
		_, ok := h.Next()
		testassert.Require(t, !ok, "Next() at draft returned ok")
	}
}

func TestInputHistoryAddSkipsConsecutiveDuplicates(t *testing.T) {
	h := NewInputHistory()
	h.Add("same")
	h.Add("same")
	h.Add("next")

	{
		got, ok := h.Prev("")
		testassert.Require(t, ok && got == "next", "Prev() = %q, %v; want next, true", got, ok)
	}
	{
		got, ok := h.Prev("")
		testassert.Require(t, ok && got == "same", "Prev() = %q, %v; want same, true", got, ok)
	}
	{
		_, ok := h.Prev("")
		testassert.Require(t, !ok, "duplicate entry was stored")
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
		{
			got := CompleteSlashCommand(tc.text, commands)
			testassert.Check(t, got == tc.want, "CompleteSlashCommand(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
	// Multiple matches extend to their longest common prefix.
	{
		got, want := CompleteSlashCommand("/al", []string{"/alpha", "/alpine"}), "/alp"
		testassert.Check(t, got == want, "CompleteSlashCommand(/al) = %q, want %q", got, want)
	}
}
