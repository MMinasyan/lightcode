package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/adaptation"
)

func TestAssembleForSpecFullMatchesBaselineAndSharesCache(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("PROJECT_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	a1 := New(projectRoot, home)
	a2 := New(projectRoot, home)
	a2.sessionStart = a1.sessionStart

	base := a1.Assemble()
	spec := a2.AssembleForSpec(Spec{Size: SizeFull, Memory: true})
	if base.Prompt != spec.Prompt {
		t.Fatalf("full empty-body memory-on spec changed baseline prompt\n--- baseline ---\n%s\n--- spec ---\n%s", base.Prompt, spec.Prompt)
	}
	if !base.Rebuilt || !spec.Rebuilt {
		t.Fatalf("first build Rebuilt: baseline=%v spec=%v, want both true", base.Rebuilt, spec.Rebuilt)
	}
	if cached := a2.Assemble(); cached.Rebuilt {
		t.Fatal("Assemble after equivalent full spec Rebuilt=true, want cached false")
	}
}

func TestAssembleForSpecFullWithAdaptationMatchesAssembleFor(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("PROJECT_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapt := &adaptation.Adaptation{
		Name:      "adapted-full",
		Blocks:    []string{"FULL_BLOCK_MARKER"},
		Additions: map[string]string{"tone": "FULL_TONE_ADDITION"},
	}
	a1 := New(projectRoot, home)
	a2 := New(projectRoot, home)
	a2.sessionStart = a1.sessionStart

	wrapper := a1.AssembleFor(adapt)
	spec := a2.AssembleForSpec(Spec{Size: SizeFull, Memory: true, Adapt: adapt})
	if wrapper.Prompt != spec.Prompt {
		t.Fatalf("full adapted spec changed AssembleFor prompt\n--- wrapper ---\n%s\n--- spec ---\n%s", wrapper.Prompt, spec.Prompt)
	}
	if !wrapper.Rebuilt || !spec.Rebuilt {
		t.Fatalf("first build Rebuilt: wrapper=%v spec=%v, want both true", wrapper.Rebuilt, spec.Rebuilt)
	}
	if cached := a2.AssembleFor(adapt); cached.Rebuilt {
		t.Fatal("AssembleFor after equivalent adapted full spec Rebuilt=true, want cached false")
	}
}

func TestAssembleForSpecSimpleIncludesOnlySimpleSectionsAndBodySlot(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("## Tone\n\nPROJECT_TONE_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(projectRoot, home)
	adapt := &adaptation.Adaptation{
		Name:      "simple",
		Blocks:    []string{"SIMPLE_BLOCK_MARKER"},
		Additions: map[string]string{"safety": "SIMPLE_SAFETY_ADDITION", "tone": "SIMPLE_TONE_ADDITION"},
	}
	prompt := a.AssembleForSpec(Spec{
		Size:   SizeSimple,
		Body:   "Do the focused job.",
		Memory: true,
		Adapt:  adapt,
	}).Prompt

	for _, want := range []string{
		strings.TrimSpace(identitySection),
		strings.TrimSpace(coreRulesSection),
		strings.TrimSpace(compactionAwarenessSection),
		"## Environment",
		strings.TrimSpace(memoryInstructionsSection),
		strings.TrimSpace(safetySection),
		"SIMPLE_SAFETY_ADDITION",
		"SIMPLE_BLOCK_MARKER",
		agentPromptHeading,
		"Do the focused job.",
		"PROJECT_TONE_RULES_MARKER",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("simple prompt missing %q\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{
		strings.TrimSpace(rulesFileGuideSection),
		strings.TrimSpace(toneSection),
		strings.TrimSpace(taskExecutionSection),
		strings.TrimSpace(languageSection),
		"SIMPLE_TONE_ADDITION",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("simple prompt contains omitted content %q\n%s", forbidden, prompt)
		}
	}

	assertOrder(t, prompt,
		strings.TrimSpace(safetySection),
		"SIMPLE_SAFETY_ADDITION",
		"SIMPLE_BLOCK_MARKER",
		agentPromptHeading,
		"Do the focused job.",
		"PROJECT_TONE_RULES_MARKER",
	)
}

func TestAssembleForSpecNoneOnlyMemoryAndBody(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("PROJECT_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "## Your Role and Instructions\n\nOnly compact this transcript."
	a := New(projectRoot, home)
	prompt := a.AssembleForSpec(Spec{
		Size:   SizeNone,
		Body:   body,
		Memory: true,
		Adapt:  &adaptation.Adaptation{Name: "ignored", Blocks: []string{"IGNORED_BLOCK"}},
	}).Prompt

	for _, want := range []string{strings.TrimSpace(memoryInstructionsSection), body} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("none prompt missing %q\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{
		strings.TrimSpace(identitySection),
		"## Environment",
		strings.TrimSpace(safetySection),
		"PROJECT_RULES_MARKER",
		"IGNORED_BLOCK",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("none prompt contains omitted content %q\n%s", forbidden, prompt)
		}
	}
	if count := strings.Count(prompt, agentPromptHeading); count != 1 {
		t.Fatalf("agent prompt heading count = %d, want 1\n%s", count, prompt)
	}

	withoutMemory := a.AssembleForSpec(Spec{Size: SizeNone, Body: body, Memory: false}).Prompt
	if withoutMemory != body {
		t.Fatalf("none memory-off prompt = %q, want body only", withoutMemory)
	}
}

func TestAssembleForSpecMemoryGatingIsIndependentOfSize(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()

	fullNoMemory := New(projectRoot, home).AssembleForSpec(Spec{Size: SizeFull, Memory: false}).Prompt
	if strings.Contains(fullNoMemory, strings.TrimSpace(memoryInstructionsSection)) {
		t.Fatalf("full memory-off prompt contains memory instructions\n%s", fullNoMemory)
	}

	noneWithMemory := New(projectRoot, home).AssembleForSpec(Spec{Size: SizeNone, Body: "Remember.", Memory: true}).Prompt
	if !strings.Contains(noneWithMemory, strings.TrimSpace(memoryInstructionsSection)) {
		t.Fatalf("none memory-on prompt missing memory instructions\n%s", noneWithMemory)
	}
}

func assertOrder(t *testing.T, text string, markers ...string) {
	t.Helper()
	prev := -1
	for _, marker := range markers {
		idx := strings.Index(text, marker)
		if idx < 0 {
			t.Fatalf("missing order marker %q\n%s", marker, text)
		}
		if idx < prev {
			t.Fatalf("marker %q at %d appeared before previous marker at %d\n%s", marker, idx, prev, text)
		}
		prev = idx
	}
}
