package agent

import (
	"testing"

	"github.com/MMinasyan/lightcode/internal/loop"
	"github.com/MMinasyan/lightcode/internal/prompt"
)

func TestWarningSnapshotGroupsAndClearsWarnings(t *testing.T) {
	a := &Agent{warningGroups: make(map[string][]PromptWarning)}
	var events [][]PromptWarning
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventWarning {
			events = append(events, append([]PromptWarning(nil), ev.Warnings...))
		}
	})

	a.setWarningGroup("prompt", []prompt.Warning{{Kind: "rules_not_found", Message: "No AGENTS.md found"}})
	a.setWarningGroup("catalog", []prompt.Warning{{Kind: "catalog_discovery_failure", Message: "openrouter: failed"}})
	a.addWarning("lsp", prompt.Warning{Kind: "lsp_install_failed", Message: "Failed to install gopls"})
	a.addWarning("lsp", prompt.Warning{Kind: "lsp_install_failed", Message: "Failed to install gopls"})

	want := []PromptWarning{
		{Kind: "rules_not_found", Message: "No AGENTS.md found"},
		{Kind: "catalog_discovery_failure", Message: "openrouter: failed"},
		{Kind: "lsp_install_failed", Message: "Failed to install gopls"},
	}
	assertPromptWarningsEqual(t, a.CurrentWarnings(), want)
	if got, wantEvents := len(events), 3; got != wantEvents {
		t.Fatalf("warning events = %d, want %d", got, wantEvents)
	}

	a.setWarningGroup("prompt", nil)
	want = []PromptWarning{
		{Kind: "catalog_discovery_failure", Message: "openrouter: failed"},
		{Kind: "lsp_install_failed", Message: "Failed to install gopls"},
	}
	assertPromptWarningsEqual(t, a.CurrentWarnings(), want)
}

func TestWarningSnapshotSuppressesUnchangedGroup(t *testing.T) {
	a := &Agent{warningGroups: make(map[string][]PromptWarning)}
	events := 0
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventWarning {
			events++
		}
	})
	warnings := []prompt.Warning{{Kind: "rules_not_found", Message: "No AGENTS.md found"}}

	a.setWarningGroup("prompt", warnings)
	a.setWarningGroup("prompt", warnings)

	if events != 1 {
		t.Fatalf("warning events = %d, want 1", events)
	}
}

func TestDispatchLoopWarningEmitsWarningSnapshot(t *testing.T) {
	a := &Agent{warningGroups: make(map[string][]PromptWarning)}
	var got []PromptWarning
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventWarning {
			got = append([]PromptWarning(nil), ev.Warnings...)
		}
	})

	a.dispatchLoopEvent(loop.Event{
		Kind:   loop.Warning,
		Result: "missing reasoning_content",
		Metadata: map[string]any{
			"kind": "protocol_must_preserve_missing",
		},
	})

	assertPromptWarningsEqual(t, got, []PromptWarning{{
		Kind:    "protocol_must_preserve_missing",
		Message: "missing reasoning_content",
	}})
}

func assertPromptWarningsEqual(t *testing.T, got, want []PromptWarning) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("warnings = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("warnings = %#v, want %#v", got, want)
		}
	}
}
