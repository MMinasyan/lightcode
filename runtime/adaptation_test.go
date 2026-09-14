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

	"github.com/MMinasyan/lightcode/internal/prompt"
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
		if err == nil || !errors.Is(err, ErrComposition) || !strings.Contains(err.Error(), "requires Runtime scope") {
			t.Errorf("newComposition with a %s-scoped ModelAdaptation = %v, want a Runtime-scope composition failure", scope, err)
		}
	}
}

func TestCompositionRejectsAmbiguousModelAdaptation(t *testing.T) {
	_, err := newComposition([]Plugin{
		adaptationPlugin("adapt-a", "model_adaptation_a", ScopeRuntime),
		adaptationPlugin("adapt-b", "model_adaptation_b", ScopeRuntime),
	})
	if err == nil || !errors.Is(err, ErrComposition) || !strings.Contains(err.Error(), "declared as runtime.ModelAdaptation") {
		t.Fatalf("newComposition with two ModelAdaptation exports = %v, want an ambiguity composition failure", err)
	}
}

func TestCompositionSelectsSingleModelAdaptationAndLeavesOtherExportsUnselected(t *testing.T) {
	c, err := newComposition([]Plugin{
		adaptationPlugin("adapt", "model_adaptation", ScopeRuntime),
	})
	if err != nil {
		t.Fatalf("newComposition: %v", err)
	}
	if c.modelAdaptation != "model_adaptation" {
		t.Fatalf("composition adaptation export = %q, want model_adaptation", c.modelAdaptation)
	}
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
	if err != nil {
		t.Fatalf("newComposition(mixed): %v", err)
	}
	if mixed.modelAdaptation != "model_adaptation" {
		t.Fatalf("mixed composition adaptation export = %q, want model_adaptation", mixed.modelAdaptation)
	}
	if !reflect.DeepEqual(mixed.capabilityIDs, []string{"model_adaptation", "extra"}) {
		t.Fatalf("capability universe = %q, want both exports selectable", mixed.capabilityIDs)
	}
	if got := defaultCapabilityIDsFor(t, mixed); !reflect.DeepEqual(got, []string{"model_adaptation"}) {
		t.Fatalf("default capability list for the mixed plugin = %q, want [model_adaptation] only", got)
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
	if got := defaultCapabilityIDsFor(t, c); !reflect.DeepEqual(got, []string{"model_adaptation"}) {
		t.Fatalf("default capability list = %q, want [model_adaptation]", got)
	}
	// No provider: empty defaults, so every definition keeps its baseline.
	plain := mustComposition(t, servicePlugin("alpha", nil, nil))
	if got := defaultCapabilityIDsFor(t, plain); got != nil {
		t.Fatalf("default capability list without adaptation = %q, want empty", got)
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
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	want := []string{"model_adaptation"}
	for _, def := range snapshot.definitions {
		switch def.Name {
		case "clear":
			if def.Capabilities != nil {
				t.Fatalf("%s capabilities = %q, want the explicit empty clear", def.Name, def.Capabilities)
			}
		default:
			if !reflect.DeepEqual(def.Capabilities, want) {
				t.Fatalf("%s capabilities = %q, want the projected default %q", def.Name, def.Capabilities, want)
			}
		}
	}
}

// Tool-spec fixtures for the composer: ToolSpec records the pure describe
// function behind the same validation production declarations use.

func staticToolSpec(id, description string, available, hidden bool) CapabilitySpec {
	return ToolSpec(id, func(Invocation, ToolConstraints) (ToolDescription, error) {
		return toolDescriptionFor(id, description, available, hidden)
	})
}

// mutationToolSpec mirrors the native mutation-tool descriptions: available
// unless the hard constraints make it ineligible.
func mutationToolSpec(id string, hidden bool) CapabilitySpec {
	return ToolSpec(id, func(_ Invocation, constraints ToolConstraints) (ToolDescription, error) {
		return toolDescriptionFor(id, id+" description", !constraints.Readonly || constraints.WriteDir != "", hidden)
	})
}

func failingToolSpec(id string) CapabilitySpec {
	return ToolSpec(id, func(Invocation, ToolConstraints) (ToolDescription, error) {
		return ToolDescription{}, errors.New(id + " describe failure")
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
		if err != nil {
			t.Fatalf("composeSurface(%s): %v", size, err)
		}
		direct := prompt.NewService(req.home).Assemble(req.workspace, req.sessionStart, prompt.Spec{Size: size})
		if result.Prompt != direct.Prompt {
			t.Fatalf("composeSurface(%s) prompt diverged from the direct assembly", size)
		}
		if size == prompt.SizeNone {
			// None skips the rules reads: no warnings even though no rules
			// file exists under the isolated home.
			if len(result.Warnings) != 0 {
				t.Fatalf("none-size warnings = %#v, want none", result.Warnings)
			}
		} else if len(result.Warnings) != 1 || result.Warnings[0].Kind != prompt.WarnRulesNotFound {
			t.Fatalf("%s-size warnings = %#v, want the rules_not_found warning", size, result.Warnings)
		}
	}
}

func TestComposeSurfaceRulesPrecedenceAndOverrides(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".lightcode", "AGENTS.md"), []byte("GLOBAL CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("PROJECT CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := baseSurfaceRequest(t)
	req.home, req.workspace = home, workspace
	result, _, err := composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if !strings.Contains(result.Prompt, "GLOBAL CONTENT") || !strings.Contains(result.Prompt, "PROJECT CONTENT") {
		t.Fatal("the composed prompt lost a rules layer")
	}
	if strings.Index(result.Prompt, "GLOBAL CONTENT") >= strings.Index(result.Prompt, "PROJECT CONTENT") {
		t.Fatal("project rules did not render after global rules")
	}
	// A user heading overrides the built-in section while the rest of the
	// assembly is unchanged.
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("## Tone\n\nProject tone rule."), 0o600); err != nil {
		t.Fatal(err)
	}
	result, _, err = composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if strings.Contains(result.Prompt, "Be direct and concise") {
		t.Fatal("the built-in tone section survived a user tone heading override")
	}
	if !strings.Contains(result.Prompt, "Project tone rule.") {
		t.Fatal("the overriding project tone rule is missing")
	}
}

func TestComposeSurfaceBlocksAndAdditionsRender(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.promptBody = "Do the task."
	req.adaptation = &fakeAdaptation{value: Adaptation{
		Blocks:    []string{"COACHING BLOCK"},
		Additions: map[string]string{"safety": "ADDITION SAFETY"},
	}}
	result, _, err := composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if !strings.Contains(result.Prompt, "COACHING BLOCK") || !strings.Contains(result.Prompt, "ADDITION SAFETY") {
		t.Fatal("the adaptation blocks or additions did not render")
	}
	if strings.Index(result.Prompt, "COACHING BLOCK") >= strings.Index(result.Prompt, "## Your Role and Instructions") {
		t.Fatal("coaching blocks did not render before the agent body and rules")
	}
}

func TestComposeSurfaceDescriptionReplacements(t *testing.T) {
	const description = "Edit <EDIT FILE OR WRITE FILE> via one call."
	req := baseSurfaceRequest(t)
	req.toolSpecs = []CapabilitySpec{staticToolSpec("read_file", description, true, false)}

	// No adaptation: the bundled default replacement applies.
	_, advertised, err := composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if advertised[0].Description != "Edit edit_file or write_file via one call." {
		t.Fatalf("baseline description = %q, want the bundled default replacement", advertised[0].Description)
	}

	// The shipped GPT row's entry override applies instead.
	req.adaptation = &fakeAdaptation{value: Adaptation{
		ToolDescriptionReplacements: map[string]string{"<EDIT FILE OR WRITE FILE>": "apply_patch"},
	}}
	_, advertised, err = composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if advertised[0].Description != "Edit apply_patch via one call." {
		t.Fatalf("adapted description = %q, want the entry override replacement", advertised[0].Description)
	}
}

func TestComposeSurfaceAdvertisement(t *testing.T) {
	req := baseSurfaceRequest(t)

	// Unselected: only the available, non-hidden tool is advertised.
	_, advertised, err := composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if names := advertisedNames(advertised); !reflect.DeepEqual(names, []string{"read_file"}) {
		t.Fatalf("baseline advertisement = %q, want [read_file]", names)
	}

	// The hidden tool appears only through the include list.
	req.adaptation = &fakeAdaptation{value: Adaptation{IncludeTools: []string{"apply_patch"}}}
	_, advertised, err = composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if names := advertisedNames(advertised); !reflect.DeepEqual(names, []string{"read_file", "apply_patch"}) {
		t.Fatalf("included advertisement = %q, want the declaration order", names)
	}

	// Exclusion always wins over inclusion.
	req.adaptation = &fakeAdaptation{value: Adaptation{
		ExcludeTools: []string{"read_file"},
		IncludeTools: []string{"read_file", "apply_patch"},
	}}
	_, advertised, err = composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if names := advertisedNames(advertised); !reflect.DeepEqual(names, []string{"apply_patch"}) {
		t.Fatalf("exclusion-wins advertisement = %q, want [apply_patch]", names)
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
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if names := advertisedNames(advertised); !reflect.DeepEqual(names, []string{"read_file"}) {
		t.Fatalf("readonly advertisement = %q, want only the eligible read_file", names)
	}
	// The nearest sibling: with a write dir the tools are eligible and the
	// include surfaces the hidden one.
	req.constraints = ToolConstraints{Readonly: true, WriteDir: "/tmp/plan"}
	_, advertised, err = composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if names := advertisedNames(advertised); !reflect.DeepEqual(names, []string{"read_file", "write_file", "edit_file", "apply_patch"}) {
		t.Fatalf("write-dir advertisement = %q, want every eligible tool in declaration order", names)
	}
}

func TestComposeSurfaceDescribeFailureAborts(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.toolSpecs = []CapabilitySpec{failingToolSpec("broken")}
	_, advertised, err := composeSurface(req)
	if err == nil || !strings.Contains(err.Error(), "broken describe failure") {
		t.Fatalf("composeSurface = (%#v, %v), want the describe error", advertised, err)
	}
	if advertised != nil {
		t.Fatalf("a failed description still advertised %d tools", len(advertised))
	}
}

func TestComposeSurfaceResolvesExactlyOnce(t *testing.T) {
	req := baseSurfaceRequest(t)
	fake := &fakeAdaptation{}
	req.adaptation = fake
	if _, _, err := composeSurface(req); err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("Resolve calls = %d, want exactly 1", fake.calls)
	}
	if fake.ref != req.modelRef {
		t.Fatalf("Resolve received %s, want %s", fake.ref.String(), req.modelRef.String())
	}
}

func TestComposeSurfaceResolveErrorPropagates(t *testing.T) {
	req := baseSurfaceRequest(t)
	req.adaptation = &fakeAdaptation{err: errors.New("adaptation unavailable")}
	result, advertised, err := composeSurface(req)
	if err == nil || !strings.Contains(err.Error(), "adaptation unavailable") {
		t.Fatalf("composeSurface = (%+v, %v), want the Resolve error", result, err)
	}
	if advertised != nil {
		t.Fatalf("a failed Resolve still advertised %d tools", len(advertised))
	}
}

func TestComposeSurfaceUnselectedAdaptationKeepsBaselinePrompt(t *testing.T) {
	req := baseSurfaceRequest(t)
	start := time.Now().Add(-time.Hour)
	req.sessionStart = start
	result, advertised, err := composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	direct := prompt.NewService(req.home).Assemble(req.workspace, start, prompt.Spec{Size: prompt.SizeFull})
	if result.Prompt != direct.Prompt {
		t.Fatal("the unselected adaptation changed the baseline prompt")
	}
	if names := advertisedNames(advertised); !reflect.DeepEqual(names, []string{"read_file"}) {
		t.Fatalf("baseline advertisement = %q, want the full eligible surface", names)
	}
}

func TestComposeSurfaceZeroSessionStartSamplesComposeTime(t *testing.T) {
	req := baseSurfaceRequest(t)
	before := time.Now().Add(-time.Second)
	result, _, err := composeSurface(req)
	after := time.Now().Add(time.Second)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	const marker = "Session started: "
	i := strings.Index(result.Prompt, marker)
	if i < 0 {
		t.Fatalf("the composed prompt lost the session start: %q", result.Prompt)
	}
	line := result.Prompt[i+len(marker):]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	sampled, err := time.ParseInLocation("2006-01-02 15:04:05 MST", line, time.Local)
	if err != nil {
		t.Fatalf("parsing the sampled session start %q: %v", line, err)
	}
	if sampled.Before(before) || sampled.After(after) {
		t.Fatalf("sampled session start = %s, want a compose-time value within [%s, %s]", sampled, before, after)
	}
	// An explicit session start is used verbatim.
	req.sessionStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	result, _, err = composeSurface(req)
	if err != nil {
		t.Fatalf("composeSurface: %v", err)
	}
	if !strings.Contains(result.Prompt, "Session started: 2026-01-02 03:04:05 UTC") {
		t.Fatalf("the explicit session start did not render verbatim: %q", result.Prompt)
	}
}

func advertisedNames(definitions []model.ToolDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}
