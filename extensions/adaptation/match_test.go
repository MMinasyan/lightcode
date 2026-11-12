package adaptation

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Syntheresis/melta/internal/testassert"
)

func TestMatcherOrderingFirstMatch(t *testing.T) {
	// gpt-oss* is listed before gpt-* so the first matching row wins on overlap.
	tbl := []entry{
		{pattern: "gpt-oss*", adapt: Adaptation{Append: []string{"gpt-oss-addition"}}},
		{pattern: "gpt-*", adapt: Adaptation{Append: []string{"gpt-addition"}}},
	}
	cases := []struct {
		id   string
		want string // first append marker
	}{
		{"gpt-oss-20b", "gpt-oss-addition"},
		{"gpt-oss-120b", "gpt-oss-addition"}, // first match wins on overlap
		{"gpt-5.4", "gpt-addition"},
		{"gpt-5.4-mini", "gpt-addition"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got := matchEntryIn(tbl, tc.id)
			testassert.Require(t, got != nil, "matchEntryIn(%q) = nil, want %q", tc.id, tc.want)
			testassert.Require(t, got.adapt.Append[0] == tc.want, "matchEntryIn(%q).adapt.Append[0] = %q, want %q", tc.id, got.adapt.Append[0], tc.want)
		})
	}
}

func TestMatcherAbstention(t *testing.T) {
	tbl := []entry{{pattern: "gpt-*", adapt: Adaptation{Append: []string{"gpt-addition"}}}}
	for _, id := range []string{"claude-opus-4-8", "qwen3.7-plus", "", "gp"} {
		{
			got := matchEntryIn(tbl, id)
			testassert.Require(t, got == nil, "matchEntryIn(%q) = %+v, want nil", id, got)
		}
	}
}

func TestMatcherCaseInsensitive(t *testing.T) {
	tbl := []entry{{pattern: "GPT-OSS*", adapt: Adaptation{Append: []string{"gpt-oss-addition"}}}}
	// Both the pattern (upper) and the input (mixed) are folded before matching.
	for _, id := range []string{"gpt-oss-20b", "GPT-OSS-20B", "Gpt-Oss-20b"} {
		got := matchEntryIn(tbl, id)
		testassert.Require(t, got != nil && got.adapt.Append[0] == "gpt-oss-addition", "matchEntryIn(%q) = %+v, want the gpt-oss row", id, got)
	}
}

func TestMatcherSlashContainingBareID(t *testing.T) {
	// A bare model id can itself contain slashes (coremodel.Parse cuts only the
	// first '/'), so the wildcard must cross '/'.
	tbl := []entry{{pattern: "deepseek*", adapt: Adaptation{Append: []string{"deepseek-addition"}}}}
	got := matchEntryIn(tbl, "deepseek/deepseek-chat")
	testassert.Require(t, got != nil && got.adapt.Append[0] == "deepseek-addition", "matchEntryIn(deepseek/deepseek-chat) = %+v, want the deepseek row", got)
}

func TestMatcherIsAnchoredNotSubstring(t *testing.T) {
	// The matcher is always fed the bare model id; a provider-prefixed string
	// must not match a pattern that the bare id matches.
	tbl := []entry{{pattern: "gpt-5*", adapt: Adaptation{Append: []string{"gpt5-addition"}}}}
	{
		got := matchEntryIn(tbl, "gpt-5.4")
		testassert.Require(t, got != nil && got.adapt.Append[0] == "gpt5-addition", "matchEntryIn(gpt-5.4) = %+v, want the gpt-5 row", got)
	}
	{
		got := matchEntryIn(tbl, "openai/gpt-5.4")
		testassert.Require(t, got == nil, "matchEntryIn(openai/gpt-5.4) = %+v, want nil (anchored, not substring)", got)
	}
}

func TestTransformPromptNoOpReturnsIncomingBytes(t *testing.T) {
	incoming := "## Identity\n\nbaseline text\n\n## Rules\n\nuser rules\n"
	for _, adapt := range []*Adaptation{nil, {}} {
		testassert.Equal(t, TransformPrompt(incoming, adapt), incoming)
	}
}

func TestTransformPromptOrderedAppend(t *testing.T) {
	cases := []struct {
		name     string
		incoming string
		adapt    *Adaptation
		want     string
	}{
		{
			name:     "first-line indentation",
			incoming: "BASE",
			adapt:    &Adaptation{Append: []string{"  indented\nbody", "after"}},
			want:     "BASE\n\n  indented\nbody\n\nafter",
		},
		{
			name:     "trailing whitespace and newlines",
			incoming: "BASE",
			adapt:    &Adaptation{Append: []string{"first  \n\n", "last\t"}},
			want:     "BASE\n\nfirst  \n\n\n\nlast\t",
		},
		{
			name:     "empty and whitespace entries between nonempty",
			incoming: "BASE",
			adapt:    &Adaptation{Append: []string{"first", "", "   ", "second"}},
			want:     "BASE\n\nfirst\n\n\n\n   \n\nsecond",
		},
		{
			name:     "empty base with leading empty append",
			incoming: "",
			adapt:    &Adaptation{Append: []string{"", "after"}},
			want:     "\n\nafter",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testassert.Equal(t, TransformPrompt(tc.incoming, tc.adapt), tc.want)
		})
	}
}

func TestTransformPromptEqualTextAndTransformProduceEqualOutput(t *testing.T) {
	// Two differently assembled texts that are equal after assembly take the
	// same transformation regardless of headings or provenance.
	first := "## Identity\n\nprompt\n\n## Rules\n\nrules"
	second := strings.Join([]string{"## Identity", "", "prompt", "", "## Rules", "", "rules"}, "\n")
	testassert.Equal(t, first, second)
	adapt := &Adaptation{Append: []string{"appended"}}
	testassert.Equal(t, TransformPrompt(first, adapt), TransformPrompt(second, adapt))
}

func TestToolDescriptionReplacerLiteralReplacements(t *testing.T) {
	description := strings.Join([]string{
		"use edit_file or write_file",
		"- removed by adaptation",
		"done",
	}, "\n")

	// No replacement map yields the stdlib zero-pair identity replacer: the
	// description is the tool's own text.
	for name, replacements := range map[string]map[string]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			testassert.Equal(t, newToolDescriptionReplacer(replacements).Replace(description), description)
		})
	}

	// The active adaptation's literal overrides apply: one rewrites the
	// naming phrase inside its line, the other erases only its matched
	// substring and leaves the line's newline in place.
	adapted := newToolDescriptionReplacer(map[string]string{
		"edit_file or write_file": "apply_patch",
		"- removed by adaptation": "",
	}).Replace(description)
	testassert.Require(t, !strings.Contains(adapted, "removed by adaptation"), "adapted description = %q", adapted)
	{
		want := "use apply_patch\n\ndone"
		testassert.Equal(t, adapted, want)
	}
}

// TestToolDescriptionReplacerIsOnePassLongestFirst proves the replacement
// pass is one strings.NewReplacer over keys ordered by descending byte length
// then lexical order: original edit_file maps to apply_patch and is never
// reprocessed into PATCH, an original apply_patch maps to PATCH, an
// overlapping shorter key never wins at the same position, and map
// construction order cannot change the result.
func TestToolDescriptionReplacerIsOnePassLongestFirst(t *testing.T) {
	description := "edit_file then apply_patch; tail"
	want := "apply_patch then PATCH; tail"
	forward := make(map[string]string)
	forward["apply"] = "WRONG"
	forward["apply_patch"] = "PATCH"
	forward["edit_file"] = "apply_patch"
	reversed := make(map[string]string)
	reversed["edit_file"] = "apply_patch"
	reversed["apply_patch"] = "PATCH"
	reversed["apply"] = "WRONG"
	for name, replacements := range map[string]map[string]string{"forward": forward, "reversed": reversed} {
		t.Run(name, func(t *testing.T) {
			testassert.Equal(t, newToolDescriptionReplacer(replacements).Replace(description), want)
		})
	}
}

// TestToolDescriptionReplacerEmptyValueErasesOnlySubstring proves an empty
// replacement removes exactly its matched bytes: the surrounding text and
// every newline stay in place, unlike the removed whole-line erasure policy.
func TestToolDescriptionReplacerEmptyValueErasesOnlySubstring(t *testing.T) {
	description := "first\nremove me\nlast"
	got := newToolDescriptionReplacer(map[string]string{"remove me": ""}).Replace(description)
	testassert.Equal(t, got, "first\n\nlast")
}

// TestShippedTableMatchesBundledGPTCodex asserts that the bundled data's
// GPT-Codex row (5.4 / 5.5) gets the combined adaptation: prompt append plus
// the apply_patch tool swap. The expected text is sourced from the fixture
// JSON — no Go constant for the append text.
// gptGuidanceLine and gptGuidanceReplacement are the complete literal standard
// run_command guidance line the bundled GPT rows replace and its GPT form: the
// data names the whole line, so unrelated prose carrying the same phrase is
// never rewritten.
const gptGuidanceLine = "- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use edit_file or write_file."
const gptGuidanceReplacement = "- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use apply_patch."

func TestShippedTableMatchesBundledGPTCodex(t *testing.T) {
	fx := loadFixture(t)
	codex := fixtureRow(fx, "gpt-codex")
	want := "Use more tool calls when they can improve the response, provide useful context, or help plan or complete the task.\n\nIf you encounter a new or unexpected challenge before the active task is complete, try to resolve it yourself first. If a tool call fails or returns incomplete or unhelpful output, try another tool or a different approach."
	testassert.Require(t, slices.Equal(codex.Append, []string{want}), "fixture append = %q, want the one task-execution paragraph", codex.Append)
	for _, id := range []string{
		"gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "openai/gpt-5.4-mini", "openai/gpt-5.5-pro",
	} {
		t.Run(id, func(t *testing.T) {
			matched := matchEntryIn(bundledTable, id)
			testassert.Require(t, matched != nil, "matchEntryIn(%q) = nil, want gpt-codex", id)
			got := matched.adapt
			testassert.Require(t, slices.Equal(got.Append, codex.Append), "matchEntryIn(%q).adapt.Append = %v, want the %s row append", id, got.Append, codex.Name)
			testassert.Require(t, reflect.DeepEqual(got.ToolDescriptionReplacements, map[string]string{gptGuidanceLine: gptGuidanceReplacement}), "matchEntryIn(%q).adapt.ToolDescriptionReplacements = %q, want the one literal guidance-line rule", id, got.ToolDescriptionReplacements)
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
			matched := matchEntryIn(bundledTable, id)
			testassert.Require(t, matched != nil, "matchEntryIn(%q) = nil, want gpt-apply-patch", id)
			got := matched.adapt
			testassert.Require(t, reflect.DeepEqual(got.ExcludeTools, ap.ExcludeTools), "matchEntryIn(%q).adapt.ExcludeTools = %v, want the %s row exclude_tools", id, got.ExcludeTools, ap.Name)
			testassert.Check(t, slices.Contains(got.ExcludeTools, "write_file"), "matchEntryIn(%q).adapt.ExcludeTools = %v, want the file tools the V4A row replaces", id, got.ExcludeTools)
			testassert.Check(t, len(got.Append) == 0, "matchEntryIn(%q).adapt.Append = %q, want absent (broader gpt-5 has no prompt addition)", id, got.Append)
		})
	}
}

func TestShippedTableMatchesBundledGrok(t *testing.T) {
	fx := loadFixture(t)
	ge := fixtureRow(fx, "gpt-task-execution")
	for _, id := range []string{"grok-build-0.1", "x-ai/grok-build-0.1"} {
		t.Run(id, func(t *testing.T) {
			matched := matchEntryIn(bundledTable, id)
			testassert.Require(t, matched != nil, "matchEntryIn(%q) = nil, want adaptation", id)
			got := matched.adapt
			testassert.Require(t, len(got.Append) != 0 && slices.Equal(got.Append, ge.Append), "matchEntryIn(%q).adapt.Append = %q, want the nonempty %s row append", id, got.Append, ge.Name)
			testassert.Require(t, len(got.ExcludeTools) == 0 && len(got.ToolDescriptionReplacements) == 0, "grok adaptation carries tool levers %+v, want the append-only row", got)
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
			matched := matchEntryIn(bundledTable, id)
			testassert.Require(t, matched != nil, "matchEntryIn(%q) = nil, want google-task-execution", id)
			got := matched.adapt
			testassert.Require(t, len(got.Append) != 0 && slices.Equal(got.Append, ge.Append), "matchEntryIn(%q).adapt.Append = %q, want the nonempty %s row append", id, got.Append, ge.Name)
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
			{
				got := matchEntryIn(bundledTable, id)
				testassert.Require(t, got == nil, "matchEntryIn(%q) = %+v, want nil", id, got)
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
			{
				got := globMatch(tc.glob, tc.s)
				testassert.Require(t, got == tc.want, "globMatch(%q, %q) = %v, want %v", tc.glob, tc.s, got, tc.want)
			}
		})
	}
}
