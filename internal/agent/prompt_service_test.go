package agent

import (
	"strings"
	"testing"
	"time"
)

// TestOwnerPromptServiceRendersPerUnitIdentity verifies the owner assembles every
// unit's prompt through one stateless service, rendering each unit's own project
// root and session start, and keeps installed-prompt state per unit. A second
// unit's assembly cannot leak into or suppress the primary's installed prompt —
// the hazard a shared mutable assembler cache would create.
func TestOwnerPromptServiceRendersPerUnitIdentity(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	primary := a.session
	primaryInstalled := primary.installedPrompt
	if primaryInstalled == "" {
		t.Fatal("primary unit did not record an installed prompt at construction")
	}

	// A second unit in a different project with a later session start.
	otherRoot := t.TempDir()
	other := &session{
		activeAgentType: "primary",
		projectID:       primary.projectID,
		projectName:     primary.projectName,
		projectRoot:     otherRoot,
		sessionStart:    primary.sessionStart.Add(2 * time.Hour),
		installedPrompt: "SENTINEL_OTHER_INSTALLED",
	}

	rt := a.ensureRuntime()
	rt.mu.Lock()
	primaryRes := a.assembleSystemPromptForSessionLocked(primary)
	otherRes := a.assembleSystemPromptForSessionLocked(other)
	rt.mu.Unlock()

	// Each unit renders its own working directory; neither leaks the other's root.
	if !strings.Contains(otherRes.Prompt, "Working directory: "+otherRoot) {
		t.Fatalf("second unit prompt did not render its own root\n%s", otherRes.Prompt)
	}
	if strings.Contains(primaryRes.Prompt, otherRoot) {
		t.Fatalf("primary prompt leaked the second unit's root\n%s", primaryRes.Prompt)
	}
	// Distinct roots and session starts produce distinct prompts.
	if primaryRes.Prompt == otherRes.Prompt {
		t.Fatal("distinct project roots/session starts produced identical prompts")
	}
	// The primary's assembly is unchanged by assembling for another unit, and the
	// installed-prompt state stays per unit — read-only assembly mutates neither.
	if primaryRes.Prompt != primaryInstalled {
		t.Fatal("primary assembly drifted from its recorded installed prompt")
	}
	if primary.installedPrompt != primaryInstalled || other.installedPrompt != "SENTINEL_OTHER_INSTALLED" {
		t.Fatal("assembly mutated per-unit installed-prompt state")
	}
}
