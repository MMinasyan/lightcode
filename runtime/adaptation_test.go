package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/prompt"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// fakeAdaptation is a counting ModelAdaptation: it records every Resolve call
// and returns the configured value or error.
type fakeAdaptation struct {
	calls int
	ref   model.ModelRef
	value Adaptation
	err   error
}

func (f *fakeAdaptation) Resolve(ref model.ModelRef) (Adaptation, error) {
	f.calls++
	f.ref = ref
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
	testassert.Require(t, mixed.modelAdaptation == "model_adaptation" && reflect.DeepEqual(mixed.capabilityIDs, []string{"model_adaptation", "extra"}), "got %v, want %v; capability universe = %q, want both exports selectable", mixed.modelAdaptation, "model_adaptation", mixed.capabilityIDs)
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

func staticToolSpec(id, description string, available, hidden bool) CapabilitySpec {
	return ToolSpec(id, func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return toolDescriptionFor(id, description, available, hidden)
	})
}

// mutationToolSpec mirrors the native mutation-tool descriptions: available
// unless the hard constraints make it ineligible.
func mutationToolSpec(id string, hidden bool) CapabilitySpec {
	return ToolSpec(id, func(_ Invocation, constraints ToolConstraints, _ harness.SessionIdentity) (ToolDescription, error) {
		return toolDescriptionFor(id, id+" description", !constraints.Readonly || constraints.WriteDir != "", hidden)
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
		return toolDescriptionFor(id, id+" description", identity.ParentSessionID == "", false)
	})
}

func toolDescriptionFor(id, description string, available, hidden bool) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        id,
		Description: description,
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: available, DefaultHidden: hidden}, nil
}

func baseSurfaceRequest(t *testing.T) surfaceRequest {
	t.Helper()
	return surfaceRequest{
		home:       t.TempDir(),
		workspace:  t.TempDir(),
		promptSize: prompt.SizeFull,
		toolSpecs: []CapabilitySpec{
			staticToolSpec("read_file", "read_file description", true, false),
			staticToolSpec("apply_patch", "apply_patch description", true, true),
		},
		modelRef: model.ModelRef{Provider: "prov", Model: "m"},
	}
}

func TestComposeSurfacePromptSizesSkipRulesUnderNone(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.sessionStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, size := range []string{prompt.SizeNone, prompt.SizeSimple, prompt.SizeFull} {
		r := req
		r.promptSize = size
		result, _, err := composeSurface(r)
		testassert.Require(t, err == nil, "composeSurface(%s): %v", size, err)
		direct := prompt.NewService(req.home).Assemble(req.workspace, req.sessionStart, prompt.Spec{Size: size})
		testassert.Require(t, result.Prompt == direct.Prompt, "composeSurface(%s) prompt diverged from the direct assembly", size)
		if size == prompt.SizeNone {
			// None skips the rules reads: no warnings even though no rules
			// file exists under the isolated home.
			testassert.Require(t, len(result.Warnings) == 0, "none-size warnings = %#v, want none", result.Warnings)
		} else if len(result.Warnings) != 1 || result.Warnings[0].Kind != prompt.WarnRulesNotFound {
			t.Fatalf("%s-size warnings = %#v, want the rules_not_found warning", size, result.Warnings)
		}
	}
}

func TestComposeSurfaceRulesPrecedenceAndOverrides(t *testing.T) {
	home := t.TempDir()
	testassert.NoError(t, os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700))
	testassert.NoError(t, os.WriteFile(filepath.Join(home, ".lightcode", "AGENTS.md"), []byte("GLOBAL CONTENT"), 0o600))
	workspace := t.TempDir()
	testassert.NoError(t, os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("PROJECT CONTENT"), 0o600))
	req := baseSurfaceRequest(t)
	req.home, req.workspace = home, workspace
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

func TestComposeSurfaceBlocksAndAdditionsRender(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.promptBody = "Do the task."
	req.adaptation = &fakeAdaptation{value: Adaptation{
		Blocks:    []string{"COACHING BLOCK"},
		Additions: map[string]string{"safety": "ADDITION SAFETY"},
	}}
	result, _, err := composeSurface(req)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(result.Prompt, "COACHING BLOCK") && strings.Contains(result.Prompt, "ADDITION SAFETY"), "the adaptation blocks or additions did not render")
	testassert.Require(t, strings.Index(result.Prompt, "COACHING BLOCK") < strings.Index(result.Prompt, "## Your Role and Instructions"), "coaching blocks did not render before the agent body and rules")
}

func TestComposeSurfaceDescriptionReplacements(t *testing.T) {
	const description = "Edit <EDIT FILE OR WRITE FILE> via one call."
	req := baseSurfaceRequest(t)
	req.toolSpecs = []CapabilitySpec{staticToolSpec("read_file", description, true, false)}

	// No adaptation: the bundled default replacement applies.
	_, advertised, err := composeSurface(req)
	testassert.NoError(t, err)
	testassert.Equal(t, advertised[0].Description, "Edit edit_file or write_file via one call.")

	// The shipped GPT row's entry override applies instead.
	req.adaptation = &fakeAdaptation{value: Adaptation{
		ToolDescriptionReplacements: map[string]string{"<EDIT FILE OR WRITE FILE>": "apply_patch"},
	}}
	_, advertised, err = composeSurface(req)
	testassert.NoError(t, err)
	testassert.Equal(t, advertised[0].Description, "Edit apply_patch via one call.")
}

func TestComposeSurfaceAdvertisement(t *testing.T) {
	req := baseSurfaceRequest(t)

	// Unselected: only the available, non-hidden tool is advertised.
	_, advertised, err := composeSurface(req)
	testassert.NoError(t, err)
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file"}), "baseline advertisement = %q, want [read_file]", names)
	}

	// The hidden tool appears only through the include list.
	req.adaptation = &fakeAdaptation{value: Adaptation{IncludeTools: []string{"apply_patch"}}}
	_, advertised, err = composeSurface(req)
	testassert.NoError(t, err)
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "apply_patch"}), "included advertisement = %q, want the declaration order", names)
	}

	// Exclusion always wins over inclusion.
	req.adaptation = &fakeAdaptation{value: Adaptation{
		ExcludeTools: []string{"read_file"},
		IncludeTools: []string{"read_file", "apply_patch"},
	}}
	_, advertised, err = composeSurface(req)
	testassert.NoError(t, err)
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"apply_patch"}), "exclusion-wins advertisement = %q, want [apply_patch]", names)
	}
}

func TestComposeSurfaceReadonlyCannotRegainMutationTools(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.toolSpecs = []CapabilitySpec{
		staticToolSpec("read_file", "read_file description", true, false),
		mutationToolSpec("write_file", false),
		mutationToolSpec("edit_file", false),
		mutationToolSpec("apply_patch", true),
	}
	req.constraints = ToolConstraints{Readonly: true}
	req.adaptation = &fakeAdaptation{value: Adaptation{
		IncludeTools: []string{"write_file", "edit_file", "apply_patch"},
	}}
	_, advertised, err := composeSurface(req)
	testassert.NoError(t, err)
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file"}), "readonly advertisement = %q, want only the eligible read_file", names)
	}
	// The nearest sibling: with a write dir the tools are eligible and the
	// include surfaces the hidden one.
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
	if fake.ref != req.modelRef {
		t.Fatalf("Resolve received %s, want %s", fake.ref.String(), req.modelRef.String())
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
	direct := prompt.NewService(req.home).Assemble(req.workspace, start, prompt.Spec{Size: prompt.SizeFull})
	testassert.Require(t, result.Prompt == direct.Prompt, "the unselected adaptation changed the baseline prompt")
	{
		names := advertisedNames(advertised)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file"}), "baseline advertisement = %q, want the full eligible surface", names)
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
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file", "task"}), "root advertisement = %q, want the lineage-gated tool present alongside its sibling", names)
	}

	childReq := req
	childReq.identity = harness.SessionIdentity{SessionID: "child-session", ParentSessionID: "root-session"}
	_, child, err := composeSurface(childReq)
	testassert.NoError(t, err)
	{
		names := advertisedNames(child)
		testassert.Require(t, reflect.DeepEqual(names, []string{"read_file"}), "child advertisement = %q, want the lineage-gated tool excluded", names)
	}
}

func advertisedNames(definitions []model.ToolDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}
