package agent

import (
	"strings"
	"testing"
)

func TestAgentDefaultPrimaryPromptUsesSpecDrivenAssembler(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	system := a.lp.Messages()[0].TextContent()

	if strings.Contains(system, "## Your Role and Instructions") {
		t.Fatalf("default primary prompt should not include an empty agent body heading\n%s", system)
	}
	if !strings.Contains(system, "## Memory Instructions") {
		t.Fatalf("default primary prompt should include memory instructions\n%s", system)
	}
	if got := a.assembleSystemPromptForSessionLocked(a.session); got.Prompt != a.session.installedPrompt {
		t.Fatal("default primary construction did not record the installed prompt: a fresh assembly differs")
	}
}
