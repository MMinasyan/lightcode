package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/adaptation"
)

// A named adaptation with no additions produces the same prompt as baseline;
// only the presence of additions/blocks changes the output.
func TestAssembleForNoAdditionsMatchesBaseline(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("some user rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := assembleFull(projectRoot, home, nil).Prompt
	adapt := assembleFull(projectRoot, home, &adaptation.Adaptation{Name: "no-additions"}).Prompt
	if base != adapt {
		t.Fatalf("adaptation with no additions changed the prompt\n--- baseline ---\n%s\n--- adaptation ---\n%s", base, adapt)
	}
}

// Bullet 2: coaching blocks are inserted after the overridable sections and before
// user rules, without occupying or suppressing a section slot.
func TestAssembleForInsertsBlocksAfterSectionsBeforeRules(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	// User rules with a unique marker and NO section-override headings, so every
	// overridable section renders.
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("PROJECT_USER_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := assembleFull(projectRoot, home, &adaptation.Adaptation{Name: "fix", Blocks: []string{"COACHING_BLOCK_MARKER"}}).Prompt

	idxLanguage := strings.Index(prompt, strings.TrimSpace(languageSection)) // last overridable section
	idxBlock := strings.Index(prompt, "COACHING_BLOCK_MARKER")
	idxRules := strings.Index(prompt, "PROJECT_USER_RULES_MARKER")
	if idxLanguage < 0 || idxBlock < 0 || idxRules < 0 {
		t.Fatalf("missing anchor: language=%d block=%d rules=%d", idxLanguage, idxBlock, idxRules)
	}
	endLanguage := idxLanguage + len(strings.TrimSpace(languageSection))
	if !(endLanguage <= idxBlock && idxBlock < idxRules) {
		t.Fatalf("block not between last section and user rules: endLanguage=%d block=%d rules=%d", endLanguage, idxBlock, idxRules)
	}
	// The block does not suppress any overridable section.
	for name, section := range overridableSections {
		if !strings.Contains(prompt, strings.TrimSpace(section)) {
			t.Fatalf("overridable section %q suppressed by the block", name)
		}
	}
	// detectOverrides is unaffected: no section heading in the rules, none overridden.
	if ov := detectOverrides("PROJECT_USER_RULES_MARKER"); ov["safety"] || ov["tone"] || ov["task_execution"] || ov["language"] {
		t.Fatalf("detectOverrides false positive from a block-only adaptation: %+v", ov)
	}
}

// Section additions: an adaptation with Additions but no Blocks renders each
// addition immediately after its section main and before the next section.
func TestAssembleForRendersSectionAddition(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("PROJECT_USER_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapt := &adaptation.Adaptation{
		Name:      "additions",
		Additions: map[string]string{"tone": "TONE_ADDITION_MARKER"},
	}
	prompt := assembleFull(projectRoot, home, adapt).Prompt

	idxToneMain := strings.Index(prompt, strings.TrimSpace(toneSection))
	idxToneAdd := strings.Index(prompt, "TONE_ADDITION_MARKER")
	idxTask := strings.Index(prompt, strings.TrimSpace(taskExecutionSection))
	idxRules := strings.Index(prompt, "PROJECT_USER_RULES_MARKER")
	if idxToneMain < 0 || idxToneAdd < 0 || idxTask < 0 || idxRules < 0 {
		t.Fatalf("missing anchor: toneMain=%d toneAdd=%d task=%d rules=%d", idxToneMain, idxToneAdd, idxTask, idxRules)
	}
	endToneMain := idxToneMain + len(strings.TrimSpace(toneSection))
	if !(endToneMain <= idxToneAdd && idxToneAdd < idxTask && idxTask < idxRules) {
		t.Fatalf("addition not between its section main and the next section: endToneMain=%d toneAdd=%d task=%d", endToneMain, idxToneAdd, idxTask)
	}
}

// Section additions render even when a user heading overrides the section main.
func TestAssembleForAdditionRendersUnderUserOverride(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	// A user heading suppresses the tone main, but the tone addition must still render.
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("# tone\n\nUSER_TONE_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapt := &adaptation.Adaptation{
		Name:      "additions",
		Additions: map[string]string{"tone": "TONE_ADDITION_MARKER"},
	}
	prompt := assembleFull(projectRoot, home, adapt).Prompt

	if strings.Contains(prompt, strings.TrimSpace(toneSection)) {
		t.Fatal("user override did not suppress the tone main")
	}
	idxToneAdd := strings.Index(prompt, "TONE_ADDITION_MARKER")
	idxUser := strings.Index(prompt, "USER_TONE_RULES_MARKER")
	if idxToneAdd < 0 || idxUser < 0 {
		t.Fatalf("missing anchor: toneAdd=%d user=%d", idxToneAdd, idxUser)
	}
	if idxToneAdd >= idxUser {
		t.Fatalf("tone addition (%d) must render before user rules (%d)", idxToneAdd, idxUser)
	}
}

// Section additions apply independently per section id.
func TestAssembleForAdditionPerSection(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("PROJECT_USER_RULES_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range overridableOrder {
		t.Run(name, func(t *testing.T) {
			marker := strings.ToUpper(name) + "_ADDITION_MARKER"
			adapt := &adaptation.Adaptation{
				Name:      "per-section",
				Additions: map[string]string{name: marker},
			}
			prompt := assembleFull(projectRoot, home, adapt).Prompt
			if !strings.Contains(prompt, marker) {
				t.Fatalf("addition for %q missing from prompt", name)
			}
			for _, other := range overridableOrder {
				if other == name {
					continue
				}
				if strings.Contains(prompt, strings.ToUpper(other)+"_ADDITION_MARKER") {
					t.Fatalf("addition for %q leaked into %q's test", other, name)
				}
			}
		})
	}
}
