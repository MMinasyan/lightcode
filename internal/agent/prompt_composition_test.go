package agent

import (
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/prompt"
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
	if cached := a.assembler.AssembleForSpec(prompt.Spec{Size: prompt.SizeFull, Memory: true}); cached.Rebuilt {
		t.Fatal("default primary construction did not seed the full/memory-on prompt cache")
	}
}
