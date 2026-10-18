package adaptation

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestMatchInOrderingMostSpecificFirst(t *testing.T) {
	// gpt-oss* is listed before gpt-* so the more specific family wins on overlap.
	tbl := []entry{
		{pattern: "gpt-oss*", adapt: Adaptation{Additions: map[string]string{"tone": "gpt-oss-addition"}}},
		{pattern: "gpt-*", adapt: Adaptation{Additions: map[string]string{"tone": "gpt-addition"}}},
	}
	cases := []struct {
		id   string
		want string // tone addition marker, "" means nil
	}{
		{"gpt-oss-20b", "gpt-oss-addition"},
		{"gpt-oss-120b", "gpt-oss-addition"}, // first match wins on overlap
		{"gpt-5.4", "gpt-addition"},
		{"gpt-5.4-mini", "gpt-addition"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got := matchIn(tbl, tc.id)
			if got == nil {
				t.Fatalf("matchIn(%q) = nil, want %q", tc.id, tc.want)
			}
			if got.Additions["tone"] != tc.want {
				t.Fatalf("matchIn(%q).Additions[tone] = %q, want %q", tc.id, got.Additions["tone"], tc.want)
			}
		})
	}
}

func TestMatchInAbstention(t *testing.T) {
	tbl := []entry{{pattern: "gpt-*", adapt: Adaptation{Additions: map[string]string{"tone": "gpt-addition"}}}}
	for _, id := range []string{"claude-opus-4-8", "qwen3.7-plus", "", "gp"} {
		if got := matchIn(tbl, id); got != nil {
			t.Fatalf("matchIn(%q) = %+v, want nil", id, got)
		}
	}
}

func TestMatchInCaseInsensitive(t *testing.T) {
	tbl := []entry{{pattern: "GPT-OSS*", adapt: Adaptation{Additions: map[string]string{"tone": "gpt-oss-addition"}}}}
	// Both the pattern (upper) and the input (mixed) are folded before matching.
	for _, id := range []string{"gpt-oss-20b", "GPT-OSS-20B", "Gpt-Oss-20b"} {
		got := matchIn(tbl, id)
		if got == nil || got.Additions["tone"] != "gpt-oss-addition" {
			t.Fatalf("matchIn(%q) = %+v, want the gpt-oss row", id, got)
		}
	}
}

func TestMatchInSlashContainingBareID(t *testing.T) {
	// A bare model id can itself contain slashes (coremodel.Parse cuts only the
	// first '/'), so the wildcard must cross '/'.
	tbl := []entry{{pattern: "deepseek*", adapt: Adaptation{Additions: map[string]string{"tone": "deepseek-addition"}}}}
	got := matchIn(tbl, "deepseek/deepseek-chat")
	if got == nil || got.Additions["tone"] != "deepseek-addition" {
		t.Fatalf("matchIn(deepseek/deepseek-chat) = %+v, want the deepseek row", got)
	}
}

func TestMatchInIsAnchoredNotSubstring(t *testing.T) {
	// The resolver is always fed the bare model id; a provider-prefixed string
	// must not match a pattern that the bare id matches (documents "feed ref.Model").
	tbl := []entry{{pattern: "gpt-5*", adapt: Adaptation{Additions: map[string]string{"tone": "gpt5-addition"}}}}
	if got := matchIn(tbl, "gpt-5.4"); got == nil || got.Additions["tone"] != "gpt5-addition" {
		t.Fatalf("matchIn(gpt-5.4) = %+v, want the gpt-5 row", got)
	}
	if got := matchIn(tbl, "openai/gpt-5.4"); got != nil {
		t.Fatalf("matchIn(openai/gpt-5.4) = %+v, want nil (anchored, not substring)", got)
	}
}

func TestMatchInReturnsCopyNotAlias(t *testing.T) {
	tbl := []entry{{pattern: "gpt-*", adapt: Adaptation{
		ExcludeTools:                []string{"edit_file"},
		IncludeTools:                []string{"apply_patch"},
		Blocks:                      []string{"coach"},
		Additions:                   map[string]string{"tone": "tone-add"},
		ToolDescriptionReplacements: map[string]string{"<EDIT FILE OR WRITE FILE>": "apply_patch"},
	}}}
	got := matchIn(tbl, "gpt-5.4")
	if got == nil {
		t.Fatal("matchIn returned nil")
	}
	if got == &tbl[0].adapt {
		t.Fatal("matchIn returned a pointer aliasing the table entry")
	}
	// Mutating every field of the result must not touch the table entry.
	got.ExcludeTools[0] = "mutated"
	got.IncludeTools[0] = "mutated"
	got.Blocks[0] = "mutated"
	got.Blocks = append(got.Blocks, "extra")
	got.Additions["tone"] = "mutated"
	got.Additions["safety"] = "mutated"
	got.ToolDescriptionReplacements["<EDIT FILE OR WRITE FILE>"] = "mutated"
	if tbl[0].adapt.ExcludeTools[0] != "edit_file" {
		t.Fatalf("ExcludeTools leaked into the table: %q", tbl[0].adapt.ExcludeTools[0])
	}
	if tbl[0].adapt.IncludeTools[0] != "apply_patch" {
		t.Fatalf("IncludeTools leaked into the table: %q", tbl[0].adapt.IncludeTools[0])
	}
	if len(tbl[0].adapt.Blocks) != 1 || tbl[0].adapt.Blocks[0] != "coach" {
		t.Fatalf("Blocks leaked into the table: %v", tbl[0].adapt.Blocks)
	}
	if len(tbl[0].adapt.Additions) != 1 || tbl[0].adapt.Additions["tone"] != "tone-add" {
		t.Fatalf("Additions leaked into the table: %v", tbl[0].adapt.Additions)
	}
	if len(tbl[0].adapt.ToolDescriptionReplacements) != 1 || tbl[0].adapt.ToolDescriptionReplacements["<EDIT FILE OR WRITE FILE>"] != "apply_patch" {
		t.Fatalf("ToolDescriptionReplacements leaked into the table: %v", tbl[0].adapt.ToolDescriptionReplacements)
	}
}

func TestSectionAdditionResolution(t *testing.T) {
	cases := []struct {
		name    string
		adapt   *Adaptation
		section string
		want    string
	}{
		{"nil adapt", nil, "safety", ""},
		{"active non-empty key", &Adaptation{Additions: map[string]string{"tone": "ACTIVE_TONE"}}, "tone", "ACTIVE_TONE"},
		{"active key absent", &Adaptation{Additions: map[string]string{"safety": "ACTIVE_SAFETY"}}, "tone", ""},
		{"nil Additions map", &Adaptation{}, "tone", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SectionAddition(tc.adapt, tc.section)
			if got != tc.want {
				t.Fatalf("SectionAddition(%v, %q) = %q, want %q", tc.adapt, tc.section, got, tc.want)
			}
		})
	}
}

func TestShippedToolDescriptionDefaults(t *testing.T) {
	want := map[string]string{
		"<EDIT FILE OR WRITE FILE>": "edit_file or write_file",
	}
	for key, value := range want {
		if got := defaultToolDescriptionReplacements[key]; got != value {
			t.Fatalf("defaultToolDescriptionReplacements[%q] = %q, want %q", key, got, value)
		}
	}
}

func TestRenderToolDescriptionReplacements(t *testing.T) {
	orig := defaultToolDescriptionReplacements
	defer func() { defaultToolDescriptionReplacements = orig }()
	defaultToolDescriptionReplacements = map[string]string{
		"<EDIT FILE OR WRITE FILE>": "edit_file or write_file",
		"<REMOVE LINE>":             "- removed by adaptation",
	}

	description := strings.Join([]string{
		"use <EDIT FILE OR WRITE FILE>",
		"<REMOVE LINE>",
		"done",
	}, "\n")

	baseline := RenderToolDescription(description, nil)
	if strings.Contains(baseline, "<") || !strings.Contains(baseline, "use edit_file or write_file") || !strings.Contains(baseline, "- removed by adaptation") {
		t.Fatalf("baseline RenderToolDescription = %q", baseline)
	}

	adapted := RenderToolDescription(description, &Adaptation{
		ToolDescriptionReplacements: map[string]string{
			"<EDIT FILE OR WRITE FILE>": "apply_patch",
			"<REMOVE LINE>":             "",
		},
	})
	if strings.Contains(adapted, "<") || strings.Contains(adapted, "removed by adaptation") {
		t.Fatalf("adapted RenderToolDescription = %q", adapted)
	}
	if want := "use apply_patch\ndone"; adapted != want {
		t.Fatalf("adapted RenderToolDescription = %q, want %q", adapted, want)
	}
}

// TestShippedTableMatchesBundledGPTCodex asserts that the bundled data's
// GPT-Codex row (5.4 / 5.5) gets the combined adaptation: prompt addition
// plus the apply_patch tool swap. The expected text is sourced from the
// fixture JSON — no Go constant for the addition text.
func TestShippedTableMatchesBundledGPTCodex(t *testing.T) {
	fx := loadFixture(t)
	codex := fixtureRow(fx, "gpt-codex")
	want := "Use more tool calls when they can improve the response, provide useful context, or help plan or complete the task.\n\nIf you encounter a new or unexpected challenge before the active task is complete, try to resolve it yourself first. If a tool call fails or returns incomplete or unhelpful output, try another tool or a different approach."
	if codex.Additions["task_execution"] != want {
		t.Fatalf("fixture gpt-codex additions text differs from canonical Decision-1 text")
	}
	for _, id := range []string{
		"gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "openai/gpt-5.4-mini", "openai/gpt-5.5-pro",
	} {
		t.Run(id, func(t *testing.T) {
			got := Match(id)
			if got == nil {
				t.Fatalf("Match(%q) = nil, want gpt-codex", id)
			}
			if !reflect.DeepEqual(got.Additions, codex.Additions) {
				t.Fatalf("Match(%q).Additions = %v, want the %s row additions", id, got.Additions, codex.Name)
			}
			if add := SectionAddition(got, "task_execution"); add != want {
				t.Fatalf("SectionAddition(Match(%q), task_execution) differs from canonical text", id)
			}
			if !slices.Contains(got.IncludeTools, "apply_patch") {
				t.Errorf("Match(%q).IncludeTools = %v, want apply_patch", id, got.IncludeTools)
			}
		})
	}
}

func TestShippedTableMatchesBundledGPTApplyPatchBroad(t *testing.T) {
	fx := loadFixture(t)
	ap := fixtureRow(fx, "gpt-apply-patch")
	for _, id := range []string{
		"gpt-5.3-codex", "openai/gpt-5.3-codex", "gpt-5", "openai/gpt-5",
	} {
		t.Run(id, func(t *testing.T) {
			got := Match(id)
			if got == nil {
				t.Fatalf("Match(%q) = nil, want gpt-apply-patch", id)
			}
			if !reflect.DeepEqual(got.IncludeTools, ap.IncludeTools) {
				t.Fatalf("Match(%q).IncludeTools = %v, want the %s row include_tools", id, got.IncludeTools, ap.Name)
			}
			if !slices.Contains(got.IncludeTools, "apply_patch") {
				t.Errorf("Match(%q).IncludeTools = %v, want apply_patch", id, got.IncludeTools)
			}
			if v, ok := got.Additions["task_execution"]; ok && v != "" {
				t.Errorf("Match(%q).Additions[task_execution] = %q, want absent (broader gpt-5 has no prompt addition)", id, v)
			}
		})
	}
}

func TestShippedTableMatchesBundledGrok(t *testing.T) {
	fx := loadFixture(t)
	ge := fixtureRow(fx, "gpt-task-execution")
	for _, id := range []string{"grok-build-0.1", "x-ai/grok-build-0.1"} {
		t.Run(id, func(t *testing.T) {
			got := Match(id)
			if got == nil {
				t.Fatalf("Match(%q) = nil, want adaptation", id)
			}
			if add := SectionAddition(got, "task_execution"); add == "" || add != ge.Additions["task_execution"] {
				t.Fatalf("Match(%q).Additions[task_execution] = %q, want the nonempty %s row addition", id, add, ge.Name)
			}
			if slices.Contains(got.IncludeTools, "apply_patch") {
				t.Errorf("grok should not include apply_patch (not V4A): %v", got.IncludeTools)
			}
		})
	}
}

func TestShippedTableMatchesBundledGoogle(t *testing.T) {
	fx := loadFixture(t)
	ge := fixtureRow(fx, "google-task-execution")
	for _, id := range []string{
		"gemini-3-flash-preview", "gemini-3.1-pro-preview", "gemini-3.1-flash-lite",
		"google/gemini-3.5-flash", "gemma-4-26b-a4b", "google/gemma-4-31b",
	} {
		t.Run(id, func(t *testing.T) {
			got := Match(id)
			if got == nil {
				t.Fatalf("Match(%q) = nil, want google-task-execution", id)
			}
			if add := SectionAddition(got, "task_execution"); add == "" || add != ge.Additions["task_execution"] {
				t.Fatalf("Match(%q).Additions[task_execution] = %q, want the nonempty %s row addition", id, add, ge.Name)
			}
		})
	}
}

func TestShippedTableLeavesOtherModelsBaseline(t *testing.T) {
	for _, id := range []string{
		"gpt-oss-120b",
		"accounts/fireworks/models/gpt-oss-120b",
		"grok-4.3",
		"grok-build-0.1-preview",
		"gemini-2.5-pro",
		"gemini-4-pro",
		"gemma-3",
		"gemma-5",
		"deepseek/deepseek-chat",
		"qwen3.7-plus",
		"",
	} {
		t.Run(id, func(t *testing.T) {
			if got := Match(id); got != nil {
				t.Fatalf("Match(%q) = %+v, want nil", id, got)
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, s string
		want    bool
	}{
		{"gpt-5*", "gpt-5.4", true},
		{"gpt-5*", "gpt-5", true},           // '*' matches empty
		{"gpt-5*", "openai/gpt-5.4", false}, // anchored at start
		{"gpt-5*", "gpt-4o", false},
		{"deepseek*", "deepseek/deepseek-chat", true}, // '*' crosses '/'
		{"*", "anything/at-all", true},
		{"*coder*", "qwen3.7-coder-plus", true},  // substring
		{"*-instruct", "llama-3-instruct", true}, // suffix
		{"*-instruct", "llama-3-base", false},
		{"gpt-5.4", "gpt-5.4", true},       // exact
		{"gpt-5.4", "gpt-5.4-mini", false}, // exact is not a prefix
		{"a*b*c", "axxbyyc", true},         // multiple stars, backtracking
		{"a*b*c", "axxbyy", false},
		{"", "", true},
		{"", "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.glob+"~"+tc.s, func(t *testing.T) {
			if got := globMatch(tc.glob, tc.s); got != tc.want {
				t.Fatalf("globMatch(%q, %q) = %v, want %v", tc.glob, tc.s, got, tc.want)
			}
		})
	}
}
