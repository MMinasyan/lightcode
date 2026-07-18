package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/adaptation"
)

func TestAssembleFullSpecIsStatelessAndDeterministic(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("PROJECT_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Size: SizeFull, Memory: true}
	// A baseline (nil adapt) render and an explicit full/memory-on spec are the
	// same prompt, and repeated renders are byte-identical: the service holds no
	// state that would drift between calls.
	base := assembleFull(projectRoot, home, nil).Prompt
	explicit := assembleSpec(projectRoot, home, spec).Prompt
	if base != explicit {
		t.Fatalf("full empty-body memory-on spec differs from baseline\n--- baseline ---\n%s\n--- spec ---\n%s", base, explicit)
	}
	if again := assembleSpec(projectRoot, home, spec).Prompt; again != explicit {
		t.Fatalf("repeated render differs\n--- first ---\n%s\n--- again ---\n%s", explicit, again)
	}
}

func TestAssembleConcurrent(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("PROJECT_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewService(home)
	adapt := &adaptation.Adaptation{Name: "concurrent", Blocks: []string{"CONCURRENT_BLOCK"}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				res := svc.Assemble(projectRoot, testStart, Spec{
					Size:   []string{SizeFull, SizeSimple, SizeNone}[j%3],
					Body:   "body",
					Memory: j%2 == 0,
					Adapt:  adapt,
				})
				if strings.TrimSpace(res.Prompt) == "" {
					t.Errorf("goroutine %d build %d produced empty prompt", i, j)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestAssembleFullWithAdaptationRendersAdaptationContent(t *testing.T) {
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
	prompt := assembleFull(projectRoot, home, adapt).Prompt
	for _, want := range []string{"FULL_BLOCK_MARKER", "FULL_TONE_ADDITION", "PROJECT_RULES_MARKER", "## Environment"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("adapted full prompt missing %q\n%s", want, prompt)
		}
	}
}

func TestAssembleForSpecSimpleIncludesOnlySimpleSectionsAndBodySlot(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("## Tone\n\nPROJECT_TONE_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapt := &adaptation.Adaptation{
		Name:      "simple",
		Blocks:    []string{"SIMPLE_BLOCK_MARKER"},
		Additions: map[string]string{"safety": "SIMPLE_SAFETY_ADDITION", "tone": "SIMPLE_TONE_ADDITION"},
	}
	prompt := assembleSpec(projectRoot, home, Spec{
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
	prompt := assembleSpec(projectRoot, home, Spec{
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

	withoutMemory := assembleSpec(projectRoot, home, Spec{Size: SizeNone, Body: body, Memory: false}).Prompt
	if withoutMemory != body {
		t.Fatalf("none memory-off prompt = %q, want body only", withoutMemory)
	}
}

func TestAssembleForSpecMemoryGatingIsIndependentOfSize(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()

	fullNoMemory := assembleSpec(projectRoot, home, Spec{Size: SizeFull, Memory: false}).Prompt
	if strings.Contains(fullNoMemory, strings.TrimSpace(memoryInstructionsSection)) {
		t.Fatalf("full memory-off prompt contains memory instructions\n%s", fullNoMemory)
	}

	noneWithMemory := assembleSpec(projectRoot, home, Spec{Size: SizeNone, Body: "Remember.", Memory: true}).Prompt
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
