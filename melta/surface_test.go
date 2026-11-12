package melta

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/Syntheresis/melta/extension"
	"github.com/Syntheresis/melta/extensions/adaptation"
	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/internal/tool"
	"github.com/Syntheresis/melta/model"
	"github.com/Syntheresis/melta/prompt"
)

// Tool-spec fixtures for the composer: ToolSpec records the pure describe
// function behind the same validation production declarations use.

func staticToolSpec(id, description string, available bool) toolDeclaration {
	return toolDeclaration{id: id, standardDescribe: func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return toolDescriptionFor(id, description, available)
	}}
}

// mutationToolSpec mirrors the native mutation-tool descriptions: available
// unless the hard constraints make it ineligible.
func mutationToolSpec(id string) toolDeclaration {
	return toolDeclaration{id: id, standardDescribe: func(_ Invocation, constraints ToolConstraints, _ harness.SessionIdentity) (ToolDescription, error) {
		return toolDescriptionFor(id, id+" description", !constraints.Readonly || constraints.WriteDir != "")
	}}
}

func failingToolSpec(id string) toolDeclaration {
	return toolDeclaration{id: id, standardDescribe: func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return ToolDescription{}, errors.New(id + " describe failure")
	}}
}

// lineageToolSpec declares a tool available only on a root Session: its
// description function receives the preparation session identity and gates
// availability on the parent lineage.
func lineageToolSpec(id string) toolDeclaration {
	return toolDeclaration{id: id, standardDescribe: func(_ Invocation, _ ToolConstraints, identity harness.SessionIdentity) (ToolDescription, error) {
		return toolDescriptionFor(id, id+" description", identity.ParentSessionID == "")
	}}
}

func toolDescriptionFor(id, description string, available bool) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        id,
		Description: description,
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: available}, nil
}

func baseSurfaceRequest(t *testing.T) surfaceRequest {
	t.Helper()
	return surfaceRequest{
		hostIdentity:   "FIXTURE_IDENTITY",
		globalRulesDir: t.TempDir(),
		workspace:      t.TempDir(),
		promptSize:     prompt.ModeFull,
		shell:          "/bin/fixture-shell",
		toolSpecs: []toolDeclaration{
			staticToolSpec("read_file", "read_file description", true),
			staticToolSpec("apply_patch", "apply_patch description", true),
		},
	}
}

func TestComposeSurfacePromptSizesSkipRulesUnderNone(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.sessionStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, mode := range []prompt.Mode{prompt.ModeNone, prompt.ModeSimple, prompt.ModeFull} {
		r := req
		r.promptSize = mode
		result, _, err := composeSurface(context.Background(), r)
		testassert.Require(t, err == nil, "composeSurface(%s): %v", mode, err)
		rules, err := prompt.ReadRules(req.globalRulesDir, req.workspace, mode)
		testassert.NoError(t, err)
		direct, err := prompt.Assemble(prompt.AssemblyInput{
			Identity:      req.hostIdentity,
			Mode:          mode,
			Rules:         rules,
			Workspace:     req.workspace,
			SessionStart:  req.sessionStart,
			OS:            goruntime.GOOS,
			Shell:         req.shell,
			OSDescription: captureOSDescription(),
		})
		testassert.NoError(t, err)
		testassert.Require(t, result.Prompt == direct, "composeSurface(%s) prompt diverged from the direct assembly", mode)
		testassert.Require(t, result.Assembly.Mode == mode && result.Assembly.Identity == req.hostIdentity && result.Assembly.Workspace == req.workspace, "composeSurface(%s) assembly = %+v, want the captured baseline inputs", mode, result.Assembly)
		if mode == prompt.ModeNone {
			// None skips the rules reads: no warnings even though no rules
			// file exists under the explicit directories.
			testassert.Require(t, len(result.Warnings) == 0, "none-size warnings = %#v, want none", result.Warnings)
		} else if len(result.Warnings) != 1 || result.Warnings[0].Kind != prompt.WarnRulesNotFound {
			t.Fatalf("%s-size warnings = %#v, want the rules_not_found warning", mode, result.Warnings)
		}
	}
}

func TestComposeSurfaceRulesPrecedenceAndOverrides(t *testing.T) {
	globalDir := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(globalDir, "AGENTS.md"), []byte("GLOBAL CONTENT"), 0o600))
	workspace := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("PROJECT CONTENT"), 0o600))
	req := baseSurfaceRequest(t)
	req.globalRulesDir, req.workspace = globalDir, workspace
	result, _, err := composeSurface(context.Background(), req)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(result.Prompt, "GLOBAL CONTENT") && strings.Contains(result.Prompt, "PROJECT CONTENT"), "the composed prompt lost a rules layer")
	testassert.Require(t, strings.Index(result.Prompt, "GLOBAL CONTENT") < strings.Index(result.Prompt, "PROJECT CONTENT"), "project rules did not render after global rules")
	// A user heading overrides the built-in section while the rest of the
	// assembly is unchanged.
	testassert.NoError(t, os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("## Tone\n\nProject tone rule."), 0o600))
	result, _, err = composeSurface(context.Background(), req)
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(result.Prompt, "Be direct and concise"), "the built-in tone section survived a user tone heading override")
	testassert.Require(t, strings.Contains(result.Prompt, "Project tone rule."), "the overriding project tone rule is missing")
}

func TestComposeSurfaceInvalidModeFailsBeforeRulesRead(t *testing.T) {
	req := baseSurfaceRequest(t)
	globalDir := t.TempDir()
	testassert.NoError(t, os.Mkdir(filepath.Join(globalDir, "AGENTS.md"), 0o700))
	req.globalRulesDir = globalDir
	req.promptSize = "bogus"
	result, catalog, err := composeSurface(context.Background(), req)
	testassert.Require(t, errors.Is(err, prompt.ErrInvalidInput), "composeSurface invalid mode error = %v, want ErrInvalidInput", err)
	testassert.Require(t, result.Prompt == "" && len(result.Warnings) == 0 && catalog == nil, "invalid mode produced a surface: %+v", result)
}

func TestComposeSurfaceAdvertisement(t *testing.T) {
	req := baseSurfaceRequest(t)

	// Every eligible tool is advertised in declaration order.
	_, catalog, err := composeSurface(context.Background(), req)
	testassert.NoError(t, err)
	{
		names := toolNames(catalog)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "apply_patch"}), "baseline advertisement = %q, want the declaration order", names)
	}

	// An ineligible tool stays out of the catalog: selection can never revive
	// a tool the hard constraints made ineligible.
	req.toolSpecs = []toolDeclaration{
		staticToolSpec("read_file", "read_file description", true),
		mutationToolSpec("write_file"),
	}
	req.constraints = ToolConstraints{Readonly: true}
	_, catalog, err = composeSurface(context.Background(), req)
	testassert.NoError(t, err)
	testassert.Require(t, reflect.DeepEqual(toolNames(catalog), []string{"read_file"}), "readonly catalog = %q, want only the eligible read_file", toolNames(catalog))
}

func TestComposeSurfaceReadonlyCannotRegainMutationTools(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.toolSpecs = []toolDeclaration{
		staticToolSpec("read_file", "read_file description", true),
		mutationToolSpec("write_file"),
		mutationToolSpec("edit_file"),
		mutationToolSpec("apply_patch"),
	}
	req.constraints = ToolConstraints{Readonly: true}
	_, catalog, err := composeSurface(context.Background(), req)
	testassert.NoError(t, err)
	{
		names := toolNames(catalog)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file"}), "readonly advertisement = %q, want only the eligible read_file", names)
	}
	// The nearest sibling: with a write dir the tools are eligible.
	req.constraints = ToolConstraints{Readonly: true, WriteDir: "/tmp/plan"}
	_, catalog, err = composeSurface(context.Background(), req)
	testassert.NoError(t, err)
	{
		names := toolNames(catalog)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "write_file", "edit_file", "apply_patch"}), "write-dir advertisement = %q, want every eligible tool in declaration order", names)
	}
}

func TestComposeSurfaceDescribeFailureAborts(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.toolSpecs = []toolDeclaration{failingToolSpec("broken")}
	_, catalog, err := composeSurface(context.Background(), req)
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "broken describe failure"), "composeSurface = (%#v, %v), want the describe error", catalog, err)
	testassert.Require(t, catalog == nil, "a failed description still advertised %d tools", len(catalog))
}

func TestComposeSurfaceZeroSessionStartSamplesComposeTime(t *testing.T) {
	req := baseSurfaceRequest(t)
	before := time.Now().Add(-time.Second)
	result, _, err := composeSurface(context.Background(), req)
	after := time.Now().Add(time.Second)
	testassert.NoError(t, err)
	const marker = "Session started: "
	i := strings.Index(result.Prompt, marker)
	testassert.Require(t, i >= 0, "the composed prompt lost the session start: %q", result.Prompt)
	line := result.Prompt[i+len(marker):]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	sampled, err := time.ParseInLocation("2006-01-02 15:04:05 MST", line, time.Local)
	testassert.Require(t, err == nil, "parsing the sampled session start %q: %v", line, err)
	testassert.Require(t, !sampled.Before(before) && !sampled.After(after), "sampled session start = %s, want a compose-time value within [%s, %s]", sampled, before, after)
	// An explicit session start is used verbatim.
	req.sessionStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	result, _, err = composeSurface(context.Background(), req)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(result.Prompt, "Session started: 2026-01-02 03:04:05 UTC"), "the explicit session start did not render verbatim: %q", result.Prompt)
}

// TestComposeSurfaceExcludesLineageUnavailableTools proves the lineage
// description input: the composed catalog passes the session identity to
// every description function, so a spec that reports unavailable for a child
// is excluded from the child's eligible catalog while the same spec stays on
// a root's, and the always-available sibling is included on both.
func TestComposeSurfaceExcludesLineageUnavailableTools(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.identity = harness.SessionIdentity{SessionID: "root-session"}
	req.toolSpecs = append(req.toolSpecs, lineageToolSpec("task"))

	_, root, err := composeSurface(context.Background(), req)
	testassert.NoError(t, err)
	{
		names := toolNames(root)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "apply_patch", "task"}), "root advertisement = %q, want the lineage-gated tool present alongside its siblings", names)
	}

	childReq := req
	childReq.identity = harness.SessionIdentity{SessionID: "child-session", ParentSessionID: "root-session"}
	_, child, err := composeSurface(context.Background(), childReq)
	testassert.NoError(t, err)
	{
		names := toolNames(child)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "apply_patch"}), "child advertisement = %q, want the lineage-gated tool excluded", names)
	}
}

// bundledGPTGuidanceLine and bundledGPTGuidanceReplacement are the complete
// literal standard run_command guidance line the bundled GPT rows replace and
// its GPT form: the data names the whole line, so no other prose carrying the
// phrase can be rewritten.
const bundledGPTGuidanceLine = "- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use edit_file or write_file."
const bundledGPTGuidanceReplacement = "- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use apply_patch."

// TestPreparationContributionBundledDescriptionReplacementIsLiteral proves
// the shipped GPT replacement is a literal whole-line rule over the REAL
// standard run_command description applied through the ordinary selected
// contribution route with the REAL bundled matcher: the final text is exactly
// the standard description with that one line rewritten, while an unrelated
// optional tool whose text merely names the same phrase is untouched. Nil,
// unselected and readonly behavior stay as before.
func TestPreparationContributionBundledDescriptionReplacementIsLiteral(t *testing.T) {
	const unrelated = "Edit edit_file or write_file via one call."
	descriptions := func(catalog []model.ToolDefinition) map[string]string {
		out := make(map[string]string, len(catalog))
		for _, definition := range catalog {
			out[definition.Name] = definition.Description
		}
		return out
	}
	instance, err := adaptation.Native().Open(context.Background(), extension.InstanceContext{})
	testassert.NoError(t, err)
	adaptationHook := instance.Capabilities["model_adaptation"].Preparation
	req := baseSurfaceRequest(t)
	req.toolSpecs = []toolDeclaration{
		{id: "run_command", standardDescribe: runCommandTool{}.describe},
		staticToolSpec("note_editor", unrelated, true),
	}
	apply := func(t *testing.T, constraints ToolConstraints) map[string]string {
		t.Helper()
		r := req
		r.constraints = constraints
		_, catalog, err := composeSurface(context.Background(), r)
		testassert.NoError(t, err)
		view := newPreparationView(prompt.AssemblyInput{Mode: prompt.ModeFull}, "BASE", catalog)
		consequence, err := adaptationHook(context.Background(), extension.CallContext{}, view.input(harness.SessionIdentity{}, model.ModelRef{Provider: "prov", Model: "gpt-5.5"}, constraints, nil))
		testassert.NoError(t, err)
		testassert.NoError(t, view.apply("adapt", consequence))
		return descriptions(view.tools())
	}

	// Unselected: with no applied contribution each tool keeps its own text.
	_, catalog, err := composeSurface(context.Background(), req)
	testassert.NoError(t, err)
	{
		got := descriptions(catalog)
		testassert.Require(t, len(got) == 2 && got["run_command"] == runCommandDescription && got["note_editor"] == unrelated, "baseline descriptions = %q, want each tool's own text", got)
	}

	want := strings.Replace(runCommandDescription, bundledGPTGuidanceLine, bundledGPTGuidanceReplacement, 1)
	testassert.Require(t, strings.ReplaceAll(runCommandDescription, "edit_file or write_file", "apply_patch") == want, "the literal expectation is not the text the previous broad phrase key composed")
	{
		got := apply(t, ToolConstraints{})
		testassert.Require(t, got["run_command"] == want, "GPT run_command description = %q, want the standard text with only its guidance line rewritten", got["run_command"])
		testassert.Require(t, got["note_editor"] == unrelated, "GPT unrelated description = %q, want the bundled whole-line rule to leave it unchanged", got["note_editor"])
	}

	// Readonly swaps run_command to its retained read-only text and toggles
	// availability only: the bundled whole-line rule names the standard
	// guidance line, so the read-only text stays exactly as the tool declares
	// it and both tools remain eligible.
	{
		got := apply(t, ToolConstraints{Readonly: true})
		testassert.Require(t, got["run_command"] == tool.ReadOnlyRunCommandDescription && got["note_editor"] == unrelated, "readonly GPT descriptions = %q, want the read-only text and the untouched unrelated text", got)
	}
}
