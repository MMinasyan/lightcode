package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/adaptation"
)

// Bullet 1: AssembleFor(nil) is byte-identical to Assemble() and shares the
// baseline Rebuilt cadence.
func TestAssembleForNilMatchesAssemble(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("some user rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	a1 := New(projectRoot, home)
	a2 := New(projectRoot, home)
	a2.sessionStart = a1.sessionStart // pin time so the environment block matches

	base := a1.Assemble()
	nilRes := a2.AssembleFor(nil)
	if base.Prompt != nilRes.Prompt {
		t.Fatalf("AssembleFor(nil) prompt != Assemble() prompt\n--- Assemble ---\n%s\n--- AssembleFor(nil) ---\n%s", base.Prompt, nilRes.Prompt)
	}
	if !base.Rebuilt || !nilRes.Rebuilt {
		t.Fatalf("first build Rebuilt: Assemble=%v AssembleFor(nil)=%v, want both true", base.Rebuilt, nilRes.Rebuilt)
	}
	// Cadence: a second baseline assemble is a cache hit.
	if second := a2.AssembleFor(nil); second.Rebuilt {
		t.Fatal("second AssembleFor(nil) Rebuilt=true, want cached false")
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
	a := New(projectRoot, home)
	res := a.AssembleFor(&adaptation.Adaptation{Name: "fix", Blocks: []string{"COACHING_BLOCK_MARKER"}})
	prompt := res.Prompt

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

// Bullet 3: the cache key is (rules hash, adaptation name) — both must be unchanged
// for a hit; either changing forces a rebuild.
func TestAssembleForCacheKeyMatrix(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("rules v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(projectRoot, home)
	fix := &adaptation.Adaptation{Name: "fix"}
	other := &adaptation.Adaptation{Name: "other"}

	if r := a.AssembleFor(fix); !r.Rebuilt {
		t.Fatal("first AssembleFor(fix) Rebuilt=false, want true")
	}
	if r := a.AssembleFor(fix); r.Rebuilt {
		t.Fatal("same (rules, name) Rebuilt=true, want cached false")
	}
	if r := a.AssembleFor(other); !r.Rebuilt {
		t.Fatal("name change with identical rules Rebuilt=false, want true")
	}
	if r := a.AssembleFor(other); r.Rebuilt {
		t.Fatal("same (rules, name) after name change Rebuilt=true, want false")
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "AGENTS.md"), []byte("rules v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := a.AssembleFor(other); !r.Rebuilt {
		t.Fatal("rules change with identical name Rebuilt=false, want true")
	}
}

// Bullet 3 (continued): an empty adaptation name shares the baseline rules-only key,
// so nil / Name:"" / Assemble() are mutually cache-compatible.
func TestAssembleForEmptyNameSharesBaselineKey(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	a := New(projectRoot, home)

	if r := a.AssembleFor(nil); !r.Rebuilt {
		t.Fatal("first AssembleFor(nil) Rebuilt=false, want true")
	}
	if r := a.Assemble(); r.Rebuilt {
		t.Fatal("Assemble() after AssembleFor(nil) Rebuilt=true, want cached false")
	}
	if r := a.AssembleFor(&adaptation.Adaptation{Name: ""}); r.Rebuilt {
		t.Fatal("AssembleFor(empty name) Rebuilt=true, want cached false (shares baseline key)")
	}
}
