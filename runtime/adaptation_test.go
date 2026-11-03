package runtime

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

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/internal/tool"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
)

// fakeAdaptation is a counting ModelAdaptation: it records every Resolve call
// and returns the configured value or error.
type fakeAdaptation struct {
	calls int
	ref   model.ModelRef
	mode  prompt.Mode
	value Adaptation
	err   error
}

func (f *fakeAdaptation) Resolve(ref model.ModelRef, mode prompt.Mode) (Adaptation, error) {
	f.calls++
	f.ref = ref
	f.mode = mode
	return f.value, f.err
}

func adaptationPlugin(id, exportID string, scope ScopeKind) Plugin {
	return Plugin{
		ID:    id,
		Scope: scope,
		Provides: []CapabilitySpec{
			Spec[ModelAdaptation](exportID),
		},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{exportID: &fakeAdaptation{}}}, nil
		},
	}
}

func TestCompositionRejectsMisplacedModelAdaptationScope(t *testing.T) {
	for _, scope := range []ScopeKind{ScopeWorkspace, ScopeOperation, ScopeAgent} {
		_, err := newComposition([]Plugin{adaptationPlugin("adapt", "model_adaptation", scope)})
		testassert.Check(t, err != nil && errors.Is(err, ErrComposition) && strings.Contains(err.Error(), "requires Runtime scope"), "newComposition with a %s-scoped ModelAdaptation = %v, want a Runtime-scope composition failure", scope, err)
	}
}

func TestCompositionRejectsAmbiguousModelAdaptation(t *testing.T) {
	_, err := newComposition([]Plugin{
		adaptationPlugin("adapt-a", "model_adaptation_a", ScopeRuntime),
		adaptationPlugin("adapt-b", "model_adaptation_b", ScopeRuntime),
	})
	testassert.Require(t, err != nil && errors.Is(err, ErrComposition) && strings.Contains(err.Error(), "declared as runtime.ModelAdaptation"), "newComposition with two ModelAdaptation exports = %v, want an ambiguity composition failure", err)
}

func TestCompositionSelectsSingleModelAdaptationAndLeavesOtherExportsUnselected(t *testing.T) {
	c, err := newComposition([]Plugin{
		adaptationPlugin("adapt", "model_adaptation", ScopeRuntime),
	})
	testassert.NoError(t, err)
	testassert.Equal(t, c.modelAdaptation, "model_adaptation")
	// A plugin exporting ModelAdaptation plus another capability: both exports
	// stay in the ordinary capability universe for explicit selection, but
	// only the ModelAdaptation ID becomes the composition's default.
	mixed, err := newComposition([]Plugin{{
		ID:    "mixed",
		Scope: ScopeRuntime,
		Provides: []CapabilitySpec{
			Spec[ModelAdaptation]("model_adaptation"),
			Spec[any]("extra"),
		},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"model_adaptation": &fakeAdaptation{}, "extra": "extra"}}, nil
		},
	}})
	testassert.NoError(t, err)
	testassert.Require(t, mixed.modelAdaptation == "model_adaptation" && reflect.DeepEqual(mixed.capabilityIDs[len(standardToolSpecs()):], []string{"model_adaptation", "extra"}), "got %v, want %v; capability universe past the standard-tool prefix = %q, want both exports selectable", mixed.modelAdaptation, "model_adaptation", mixed.capabilityIDs[len(standardToolSpecs()):])
	{
		got := defaultCapabilityIDsFor(t, mixed)
		testassert.Require(t, reflect.DeepEqual(got, []string{"model_adaptation"}), "default capability list for the mixed plugin = %q, want [model_adaptation] only", got)
	}
}

// defaultCapabilityIDsFor projects the composition onto the configuration
// service's derived default list, exactly as newConfigurationService wires it.
func defaultCapabilityIDsFor(t *testing.T, c *composition) []string {
	t.Helper()
	svc := newConfigurationService(context.Background(), c, nil, t.TempDir(), newObservation())
	return svc.defaultCapabilityIDs
}

func TestCompositionAdaptationDefaultProjection(t *testing.T) {
	// One provider: the default list is exactly the single export ID.
	c := mustComposition(t, adaptationPlugin("adapt", "model_adaptation", ScopeRuntime))
	{
		got := defaultCapabilityIDsFor(t, c)
		testassert.Require(t, reflect.DeepEqual(got, []string{"model_adaptation"}), "default capability list = %q, want [model_adaptation]", got)
	}
	// No provider: empty defaults, so every definition keeps its baseline.
	plain := mustComposition(t, servicePlugin("alpha", nil, nil))
	{
		got := defaultCapabilityIDsFor(t, plain)
		testassert.Require(t, got == nil, "default capability list without adaptation = %q, want empty", got)
	}
}

func TestConfigurationServiceProjectsDefaultCapabilities(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, providerConfigFile("One"))
	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{
	  "custom": {},
	  "clear": {"capabilities": []},
	  "explicit": {"capabilities": ["model_adaptation"]}
	}`)
	svc := h.service(context.Background(), adaptationPlugin("adaptation", "model_adaptation", ScopeRuntime))
	snapshot, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	want := []string{"model_adaptation"}
	for _, def := range snapshot.snapshot.definitions {
		switch def.Name {
		case "clear":
			testassert.Require(t, def.Capabilities == nil, "%s capabilities = %q, want the explicit empty clear", def.Name, def.Capabilities)
		default:
			testassert.Require(t, reflect.DeepEqual(def.Capabilities, want), "%s capabilities = %q, want the projected default %q", def.Name, def.Capabilities, want)
		}
	}
}

// Tool-spec fixtures for the composer: ToolSpec records the pure describe
// function behind the same validation production declarations use.

func staticToolSpec(id, description string, available bool) CapabilitySpec {
	return ToolSpec(id, func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return toolDescriptionFor(id, description, available)
	})
}

// mutationToolSpec mirrors the native mutation-tool descriptions: available
// unless the hard constraints make it ineligible.
func mutationToolSpec(id string) CapabilitySpec {
	return ToolSpec(id, func(_ Invocation, constraints ToolConstraints, _ harness.SessionIdentity) (ToolDescription, error) {
		return toolDescriptionFor(id, id+" description", !constraints.Readonly || constraints.WriteDir != "")
	})
}

func failingToolSpec(id string) CapabilitySpec {
	return ToolSpec(id, func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return ToolDescription{}, errors.New(id + " describe failure")
	})
}

// lineageToolSpec declares a tool available only on a root Session: its
// description function receives the preparation session identity and gates
// availability on the parent lineage.
func lineageToolSpec(id string) CapabilitySpec {
	return ToolSpec(id, func(_ Invocation, _ ToolConstraints, identity harness.SessionIdentity) (ToolDescription, error) {
		return toolDescriptionFor(id, id+" description", identity.ParentSessionID == "")
	})
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
		toolSpecs: []CapabilitySpec{
			staticToolSpec("read_file", "read_file description", true),
			staticToolSpec("apply_patch", "apply_patch description", true),
		},
		modelRef: model.ModelRef{Provider: "prov", Model: "m"},
	}
}

func TestComposeSurfacePromptSizesSkipRulesUnderNone(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.sessionStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, mode := range []prompt.Mode{prompt.ModeNone, prompt.ModeSimple, prompt.ModeFull} {
		r := req
		r.promptSize = mode
		result, _, err := composeSurface(r)
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
			Shell:         captureShell(),
			OSDescription: captureOSDescription(),
		})
		testassert.NoError(t, err)
		testassert.Require(t, result.Prompt == direct, "composeSurface(%s) prompt diverged from the direct assembly", mode)
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
	result, _, err := composeSurface(req)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(result.Prompt, "GLOBAL CONTENT") && strings.Contains(result.Prompt, "PROJECT CONTENT"), "the composed prompt lost a rules layer")
	testassert.Require(t, strings.Index(result.Prompt, "GLOBAL CONTENT") < strings.Index(result.Prompt, "PROJECT CONTENT"), "project rules did not render after global rules")
	// A user heading overrides the built-in section while the rest of the
	// assembly is unchanged.
	testassert.NoError(t, os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("## Tone\n\nProject tone rule."), 0o600))
	result, _, err = composeSurface(req)
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(result.Prompt, "Be direct and concise"), "the built-in tone section survived a user tone heading override")
	testassert.Require(t, strings.Contains(result.Prompt, "Project tone rule."), "the overriding project tone rule is missing")
}

func TestComposeSurfaceAppendsParagraphAfterAssembly(t *testing.T) {
	workspace := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("USER_RULES_MARKER"), 0o600))
	req := baseSurfaceRequest(t)
	req.workspace = workspace
	baseline, _, err := composeSurface(req)
	testassert.NoError(t, err)
	req.adaptation = &fakeAdaptation{value: Adaptation{Append: []string{"  APPENDED PARAGRAPH  "}}}
	result, _, err := composeSurface(req)
	testassert.NoError(t, err)
	testassert.Equal(t, result.Prompt, baseline.Prompt+"\n\n  APPENDED PARAGRAPH  ")
	for _, retained := range []string{"FIXTURE_IDENTITY", "## Core Rules", "USER_RULES_MARKER", "## Environment"} {
		testassert.Require(t, strings.Contains(result.Prompt, retained), "the append lost the assembled %q", retained)
	}
}

func TestComposeSurfaceCustomPromptAppendAppliesInEveryMode(t *testing.T) {
	for _, mode := range []prompt.Mode{prompt.ModeFull, prompt.ModeSimple, prompt.ModeNone} {
		t.Run(string(mode), func(t *testing.T) {
			req := baseSurfaceRequest(t)
			req.promptSize = mode
			req.promptBody = "DEFAULT_INSTRUCTIONS"
			baseline, _, err := composeSurface(req)
			testassert.NoError(t, err)
			req.adaptation = &fakeAdaptation{value: Adaptation{Append: []string{"  CUSTOM_APPEND\n"}}}
			result, _, err := composeSurface(req)
			testassert.NoError(t, err)
			testassert.Equal(t, result.Prompt, baseline.Prompt+"\n\n  CUSTOM_APPEND\n")
		})
	}
}

func TestComposeSurfaceZeroValueAdaptationIsNoOp(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.sessionStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	baseline, _, err := composeSurface(req)
	testassert.NoError(t, err)
	req.adaptation = &fakeAdaptation{value: Adaptation{}}
	got, _, err := composeSurface(req)
	testassert.NoError(t, err)
	testassert.Equal(t, got.Prompt, baseline.Prompt)
}

func TestComposeSurfaceInvalidModeFailsBeforeRulesRead(t *testing.T) {
	req := baseSurfaceRequest(t)
	globalDir := t.TempDir()
	testassert.NoError(t, os.Mkdir(filepath.Join(globalDir, "AGENTS.md"), 0o700))
	req.globalRulesDir = globalDir
	req.promptSize = "bogus"
	result, advertised, err := composeSurface(req)
	testassert.Require(t, errors.Is(err, prompt.ErrInvalidInput), "composeSurface invalid mode error = %v, want ErrInvalidInput", err)
	testassert.Require(t, result.Prompt == "" && len(result.Warnings) == 0 && advertised == nil, "invalid mode produced a surface: %+v", result)
}

func TestComposeSurfaceDescriptionReplacements(t *testing.T) {
	const description = "Edit edit_file or write_file via one call."
	req := baseSurfaceRequest(t)
	req.toolSpecs = []CapabilitySpec{staticToolSpec("read_file", description, true)}

	// No adaptation: the literal description is advertised unchanged.
	_, advertised, err := composeSurface(req)
	testassert.NoError(t, err)
	testassert.Equal(t, advertised[0].Description, "Edit edit_file or write_file via one call.")

	// The shipped GPT row's literal override applies instead.
	req.adaptation = &fakeAdaptation{value: Adaptation{
		ToolDescriptionReplacements: map[string]string{"edit_file or write_file": "apply_patch"},
	}}
	_, advertised, err = composeSurface(req)
	testassert.NoError(t, err)
	testassert.Equal(t, advertised[0].Description, "Edit apply_patch via one call.")
}

// bundledGPTGuidanceLine and bundledGPTGuidanceReplacement are the complete
// literal standard run_command guidance line the bundled GPT rows replace and
// its GPT form: the data names the whole line, so no other prose carrying the
// phrase can be rewritten.
const bundledGPTGuidanceLine = "- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use edit_file or write_file."
const bundledGPTGuidanceReplacement = "- Do not use this tool to read file contents — use read_file. Do not use this tool to edit files — use apply_patch."

// TestComposeSurfaceBundledGPTDescriptionReplacementIsLiteral proves the
// shipped GPT replacement is a literal whole-line rule over the REAL standard
// run_command description composed with the REAL bundled matcher: the final
// text is exactly the standard description with that one line rewritten (the
// same text the previous broad phrase key produced), while an unrelated
// optional tool whose text merely names the same phrase is untouched. Nil,
// unselected and readonly behavior stay as before.
func TestComposeSurfaceBundledGPTDescriptionReplacementIsLiteral(t *testing.T) {
	const unrelated = "Edit edit_file or write_file via one call."
	descriptions := func(advertised []model.ToolDefinition) map[string]string {
		out := make(map[string]string, len(advertised))
		for _, definition := range advertised {
			out[definition.Name] = definition.Description
		}
		return out
	}
	req := baseSurfaceRequest(t)
	req.toolSpecs = []CapabilitySpec{
		ToolSpec("run_command", runCommandTool{}.describe),
		staticToolSpec("note_editor", unrelated, true),
	}

	// Unselected: with no active adaptation each tool advertises its own text.
	_, advertised, err := composeSurface(req)
	testassert.NoError(t, err)
	{
		got := descriptions(advertised)
		testassert.Require(t, len(got) == 2 && got["run_command"] == runCommandDescription && got["note_editor"] == unrelated, "baseline descriptions = %q, want each tool's own text", got)
	}

	want := strings.Replace(runCommandDescription, bundledGPTGuidanceLine, bundledGPTGuidanceReplacement, 1)
	testassert.Require(t, strings.ReplaceAll(runCommandDescription, "edit_file or write_file", "apply_patch") == want, "the literal expectation is not the text the previous broad phrase key composed")
	req.modelRef = model.ModelRef{Provider: "prov", Model: "gpt-5.5"}
	req.adaptation = &bundledAdaptation{}
	_, advertised, err = composeSurface(req)
	testassert.NoError(t, err)
	{
		got := descriptions(advertised)
		testassert.Require(t, got["run_command"] == want, "GPT run_command description = %q, want the standard text with only its guidance line rewritten", got["run_command"])
		testassert.Require(t, got["note_editor"] == unrelated, "GPT unrelated description = %q, want the bundled whole-line rule to leave it unchanged", got["note_editor"])
	}

	// Readonly swaps run_command to its retained read-only text and toggles
	// availability only: the bundled whole-line rule names the standard
	// guidance line, so the read-only text stays exactly as the tool declares
	// it and both tools remain advertised.
	req.constraints = ToolConstraints{Readonly: true}
	_, advertised, err = composeSurface(req)
	testassert.NoError(t, err)
	{
		got := descriptions(advertised)
		testassert.Require(t, got["run_command"] == tool.ReadOnlyRunCommandDescription && got["note_editor"] == unrelated, "readonly GPT descriptions = %q, want the read-only text and the untouched unrelated text", got)
	}
}

func TestComposeSurfaceAdvertisement(t *testing.T) {
	req := baseSurfaceRequest(t)

	// Unselected: every eligible tool is advertised in declaration order.
	_, advertised, err := composeSurface(req)
	testassert.NoError(t, err)
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "apply_patch"}), "baseline advertisement = %q, want the declaration order", names)
	}

	// The adaptation only subtracts: the exclusion removes one selected tool.
	req.adaptation = &fakeAdaptation{value: Adaptation{ExcludeTools: []string{"read_file"}}}
	_, advertised, err = composeSurface(req)
	testassert.NoError(t, err)
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"apply_patch"}), "excluded advertisement = %q, want [apply_patch]", names)
	}

	// The nearest forbidden sibling: an exclusion naming nothing selects no
	// tool removal, and an unknown name removes none either.
	req.adaptation = &fakeAdaptation{value: Adaptation{ExcludeTools: []string{"absent_tool"}}}
	_, advertised, err = composeSurface(req)
	testassert.NoError(t, err)
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "apply_patch"}), "unknown-name advertisement = %q, want the untouched surface", names)
	}
}

func TestComposeSurfaceReadonlyCannotRegainMutationTools(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.toolSpecs = []CapabilitySpec{
		staticToolSpec("read_file", "read_file description", true),
		mutationToolSpec("write_file"),
		mutationToolSpec("edit_file"),
		mutationToolSpec("apply_patch"),
	}
	req.constraints = ToolConstraints{Readonly: true}
	_, advertised, err := composeSurface(req)
	testassert.NoError(t, err)
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file"}), "readonly advertisement = %q, want only the eligible read_file", names)
	}
	// The nearest sibling: with a write dir the tools are eligible.
	req.constraints = ToolConstraints{Readonly: true, WriteDir: "/tmp/plan"}
	_, advertised, err = composeSurface(req)
	testassert.NoError(t, err)
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "write_file", "edit_file", "apply_patch"}), "write-dir advertisement = %q, want every eligible tool in declaration order", names)
	}
}

func TestComposeSurfaceDescribeFailureAborts(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.toolSpecs = []CapabilitySpec{failingToolSpec("broken")}
	_, advertised, err := composeSurface(req)
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "broken describe failure"), "composeSurface = (%#v, %v), want the describe error", advertised, err)
	testassert.Require(t, advertised == nil, "a failed description still advertised %d tools", len(advertised))
}

func TestComposeSurfaceResolvesExactlyOnce(t *testing.T) {
	req := baseSurfaceRequest(t)
	fake := &fakeAdaptation{}
	req.adaptation = fake
	{
		_, _, err := composeSurface(req)
		testassert.NoError(t, err)
	}
	testassert.Equal(t, fake.calls, 1)
	if fake.ref != req.modelRef || fake.mode != req.promptSize {
		t.Fatalf("Resolve received %s/%q, want %s/%q", fake.ref.String(), fake.mode, req.modelRef.String(), req.promptSize)
	}
}

func TestComposeSurfaceResolveErrorPropagates(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.adaptation = &fakeAdaptation{err: errors.New("adaptation unavailable")}
	result, advertised, err := composeSurface(req)
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "adaptation unavailable"), "composeSurface = (%+v, %v), want the Resolve error", result, err)
	testassert.Require(t, advertised == nil, "a failed Resolve still advertised %d tools", len(advertised))
}

func TestComposeSurfaceUnselectedAdaptationKeepsBaselinePrompt(t *testing.T) {
	req := baseSurfaceRequest(t)
	start := time.Now().Add(-time.Hour)
	req.sessionStart = start
	result, advertised, err := composeSurface(req)
	testassert.NoError(t, err)
	rules, err := prompt.ReadRules(req.globalRulesDir, req.workspace, prompt.ModeFull)
	testassert.NoError(t, err)
	direct, err := prompt.Assemble(prompt.AssemblyInput{
		Identity:      req.hostIdentity,
		Mode:          prompt.ModeFull,
		Rules:         rules,
		Workspace:     req.workspace,
		SessionStart:  start,
		OS:            goruntime.GOOS,
		Shell:         captureShell(),
		OSDescription: captureOSDescription(),
	})
	testassert.NoError(t, err)
	testassert.Require(t, result.Prompt == direct, "the unselected adaptation changed the baseline prompt")
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "apply_patch"}), "baseline advertisement = %q, want the full eligible surface", names)
	}
}

func TestComposeSurfaceZeroSessionStartSamplesComposeTime(t *testing.T) {
	req := baseSurfaceRequest(t)
	before := time.Now().Add(-time.Second)
	result, _, err := composeSurface(req)
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
	result, _, err = composeSurface(req)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(result.Prompt, "Session started: 2026-01-02 03:04:05 UTC"), "the explicit session start did not render verbatim: %q", result.Prompt)
}

// TestComposeSurfaceExcludesLineageUnavailableTools proves the lineage
// description input: the composed surface passes the session identity to
// every description function, so a spec that reports unavailable for a
// child is excluded from the child's advertised surface while the same spec
// stays on a root's, and the always-available sibling is advertised on both.
func TestComposeSurfaceExcludesLineageUnavailableTools(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.identity = harness.SessionIdentity{SessionID: "root-session"}
	req.toolSpecs = append(req.toolSpecs, lineageToolSpec("task"))

	_, root, err := composeSurface(req)
	testassert.NoError(t, err)
	{
		names := advertisedNames(root)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "apply_patch", "task"}), "root advertisement = %q, want the lineage-gated tool present alongside its siblings", names)
	}

	childReq := req
	childReq.identity = harness.SessionIdentity{SessionID: "child-session", ParentSessionID: "root-session"}
	_, child, err := composeSurface(childReq)
	testassert.NoError(t, err)
	{
		names := advertisedNames(child)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "apply_patch"}), "child advertisement = %q, want the lineage-gated tool excluded", names)
	}
}

func advertisedNames(definitions []model.ToolDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}
