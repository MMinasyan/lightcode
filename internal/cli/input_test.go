package cli

import (
	"testing"

	"github.com/MMinasyan/lightcode/internal/terminal"
)

// TestCompleteSlashCommandIncludesClear keeps the shipped CLI's own command
// list under test: the shared completion helper completes against the list its
// caller supplies, so this pins the legacy vocabulary.
func TestCompleteSlashCommandIncludesClear(t *testing.T) {
	if got, want := terminal.CompleteSlashCommand("/cl", slashCommands), "/clear "; got != want {
		t.Fatalf("CompleteSlashCommand(/cl) = %q, want %q", got, want)
	}
}
