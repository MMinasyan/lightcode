package extension

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

func testNative(id string, lifetime Lifetime, capabilities []Capability, requires []Binding, open func(context.Context, InstanceContext) (Instance, error)) Native {
	provides := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		provides = append(provides, capability.ID)
	}
	declaration := Declaration{
		ID:           id,
		Version:      "1",
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"value":{"type":"integer"}},"required":["enabled"],"additionalProperties":false}`),
		Components:   []Component{{ID: id + "-c", Lifetime: lifetime, Provides: provides, Requires: requires}},
		Capabilities: capabilities,
	}
	if open == nil {
		open = func(context.Context, InstanceContext) (Instance, error) {
			return Instance{Capabilities: map[string]Implementation{}}, nil
		}
	}
	return Native{Declaration: declaration, Describe: describeAll(capabilities), Open: open}
}

func describeAll(capabilities []Capability) map[string]ToolDescribe {
	out := make(map[string]ToolDescribe)
	for _, capability := range capabilities {
		if capability.Kind == KindTool {
			out[capability.ID] = func(context.Context, DescribeInput) (ToolDescription, error) {
				return ToolDescription{Definition: modelToolDefinition(capability.ID), Available: true}, nil
			}
		}
	}
	return out
}

func modelToolDefinition(id string) model.ToolDefinition {
	return model.ToolDefinition{Name: id, Description: id, Parameters: json.RawMessage(`{"type":"object"}`)}
}

func TestCompileAcceptsDeclaredGraphAndPreservesRawBytes(t *testing.T) {
	provider := ServiceCapability("provider", "test.service", PureMethod("read", `{"type":"object"}`, `{"type":"object"}`))
	definition := provider.definition()
	consumerBinding := Binding{Name: "dep", Provider: "ext/provider-c", CapabilityID: "provider", Definition: definition}
	consumer := ServiceCapability("consumer", "test.consumer", PureMethod("call", `{"type":"object"}`, `{"type":"object"}`))
	native := testNative("ext", LifetimeHarness, []Capability{provider, consumer, ToolCapability("tool"), HookCapability("hook", SingleHook(BoundaryPrepare, HookModePure)), StrategyCapability("strategy", true)}, []Binding{consumerBinding}, nil)
	native.Declaration.Components = []Component{
		{ID: "provider-c", Lifetime: LifetimeHarness, Provides: []string{"provider"}},
		{ID: "consumer-c", Lifetime: LifetimeHarness, Provides: []string{"consumer", "tool", "hook", "strategy"}, Requires: []Binding{consumerBinding}},
	}
	compiled, err := Compile([]Native{native})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	capability, ok := compiled.Capability("provider")
	if !ok || capability.ContractID != "test.service" || len(capability.Methods) != 1 {
		t.Fatalf("provider = %+v", capability)
	}
	if got := string(func() json.RawMessage { c, _ := compiled.Capability("provider"); return c.Methods["read"].Input }()); got != `{"type":"object"}` {
		t.Fatalf("input bytes = %s, want the supplied bytes", got)
	}
	if len(compiled.Components(LifetimeHarness)) != 2 {
		t.Fatalf("construction = %d components, want 2", len(compiled.Components(LifetimeHarness)))
	}
}

func TestCompileRejectsContractViolations(t *testing.T) {
	base := func() Native {
		return testNative("ext", LifetimeHarness, []Capability{ToolCapability("tool")}, nil, nil)
	}
	for _, row := range []struct {
		name   string
		mutate func(*Native)
	}{
		{"empty version", func(n *Native) { n.Declaration.Version = "" }},
		{"missing describe", func(n *Native) { delete(n.Describe, "tool") }},
		{"extra describe", func(n *Native) {
			n.Describe["other"] = func(context.Context, DescribeInput) (ToolDescription, error) { return ToolDescription{}, nil }
		}},
		{"nil describe", func(n *Native) { n.Describe["tool"] = nil }},
		{"missing Open", func(n *Native) { n.Open = nil }},
		{"undeclared capability", func(n *Native) {
			n.Declaration.Components[0].Provides = append(n.Declaration.Components[0].Provides, "ghost")
		}},
		{"wrong tool contract", func(n *Native) { n.Declaration.Capabilities[0].ContractID = "other" }},
		{"wrong method set", func(n *Native) { n.Declaration.Capabilities[0].Methods = nil }},
		{"service without methods", func(n *Native) {
			n.Declaration.Capabilities = append(n.Declaration.Capabilities, ServiceCapability("service", "test.service"))
			n.Declaration.Components[0].Provides = append(n.Declaration.Components[0].Provides, "service")
		}},
		{"duplicate capability", func(n *Native) {
			n.Declaration.Capabilities = append(n.Declaration.Capabilities, ToolCapability("tool"))
			n.Declaration.Components[0].Provides = append(n.Declaration.Components[0].Provides, "tool")
		}},
		{"external schema reference", func(n *Native) {
			n.Declaration.Capabilities[0].Methods[0].Input = json.RawMessage(`{"$ref":"https://example.test/schema.json"}`)
		}},
		{"missing binding provider", func(n *Native) {
			n.Declaration.Components[0].Requires = []Binding{{Name: "dep", Provider: "missing/component", CapabilityID: "tool", Definition: n.Declaration.Capabilities[0].definition()}}
		}},
		{"mismatched binding definition", func(n *Native) {
			n.Declaration.Capabilities = append(n.Declaration.Capabilities, ServiceCapability("service", "test.service", PureMethod("read", `{}`, `{}`)))
			n.Declaration.Components[0].Provides = append(n.Declaration.Components[0].Provides, "service")
			n.Declaration.Components[0].Requires = []Binding{{Name: "dep", Provider: "ext/ext-c", CapabilityID: "service", Definition: ServiceCapability("x", "test.service", PureMethod("other", `{}`, `{}`)).definition()}}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			native := base()
			row.mutate(&native)
			if _, err := Compile([]Native{native}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Compile error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestCompileRejectsShorterLivedDependencyAndCycle(t *testing.T) {
	short := ServiceCapability("short", "test.short", PureMethod("call", `{}`, `{}`))
	long := ServiceCapability("long", "test.long", PureMethod("call", `{}`, `{}`))
	native := testNative("ext", LifetimeHarness, []Capability{short, long}, nil, nil)
	native.Declaration.Components = []Component{
		{ID: "short-c", Lifetime: LifetimeInvocation, Provides: []string{"short"}},
		{ID: "long-c", Lifetime: LifetimeHarness, Provides: []string{"long"}, Requires: []Binding{{Name: "dep", Provider: "ext/short-c", CapabilityID: "short", Definition: short.definition()}}},
	}
	if _, err := Compile([]Native{native}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "shorter-lived") {
		t.Fatalf("Compile error = %v, want the shorter-lived rejection", err)
	}
	cycleA := ServiceCapability("a", "test.a", PureMethod("call", `{}`, `{}`))
	cycleB := ServiceCapability("b", "test.b", PureMethod("call", `{}`, `{}`))
	cyclic := testNative("cycle", LifetimeHarness, []Capability{cycleA, cycleB}, nil, nil)
	cyclic.Declaration.Components = []Component{
		{ID: "a-c", Lifetime: LifetimeHarness, Provides: []string{"a"}, Requires: []Binding{{Name: "b", Provider: "cycle/b-c", CapabilityID: "b", Definition: cycleB.definition()}}},
		{ID: "b-c", Lifetime: LifetimeHarness, Provides: []string{"b"}, Requires: []Binding{{Name: "a", Provider: "cycle/a-c", CapabilityID: "a", Definition: cycleA.definition()}}},
	}
	if _, err := Compile([]Native{cyclic}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Compile error = %v, want the cycle rejection", err)
	}
}

func TestCompileRejectsBoundaryLifetimeMismatch(t *testing.T) {
	hook := HookCapability("hook", SingleHook(BoundaryPrepare, HookModePure))
	hookNative := testNative("hookext", LifetimeInvocation, []Capability{hook}, nil, nil)
	if _, err := Compile([]Native{hookNative}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "Harness or Workspace") {
		t.Fatalf("preparation hook error = %v, want the lifetime rejection", err)
	}
	strategy := StrategyCapability("strategy", true)
	strategyNative := testNative("strategyext", LifetimeInvocation, []Capability{strategy}, nil, nil)
	if _, err := Compile([]Native{strategyNative}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "Operation-or-longer") {
		t.Fatalf("strategy error = %v, want the lifetime rejection", err)
	}
}

func TestCompileRejectsIncompatibleContractReuse(t *testing.T) {
	first := testNative("one", LifetimeHarness, []Capability{ServiceCapability("one", "test.shared/v1", PureMethod("read", `{}`, `{}`))}, nil, nil)
	second := testNative("two", LifetimeHarness, []Capability{ServiceCapability("two", "test.shared/v1", PureMethod("read", `{"type":"object"}`, `{}`))}, nil, nil)
	if _, err := Compile([]Native{first, second}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("Compile error = %v, want incompatible contract reuse", err)
	}
}

func TestValidateResultDeclaredFailureUnion(t *testing.T) {
	var methods MethodSet
	compiled, err := Compile([]Native{testNative("ext", LifetimeHarness, []Capability{ServiceCapability("service", "test.service", ServiceMethod("call", ModeServiceIO, `{}`, `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`, `{"type":"object","properties":{"reason":{"type":"string"}},"required":["reason"]}`, `{"type":"string"}`))}, nil, nil)})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	service, _ := compiled.Capability("service")
	methods = service.Methods
	valid := Result{Value: json.RawMessage(`{"ok":true}`)}
	if err := ValidateResult(methods, "call", valid); err != nil {
		t.Fatalf("valid success: %v", err)
	}
	failure := Result{Failure: &Failure{Code: "invalid", Message: "bad", Data: json.RawMessage(`{"reason":"x"}`), Partial: json.RawMessage(`"partial"`)}}
	if err := ValidateResult(methods, "call", failure); err != nil {
		t.Fatalf("valid failure: %v", err)
	}
	for _, row := range []struct {
		name   string
		result Result
	}{
		{"both", Result{Value: json.RawMessage(`{}`), Failure: &Failure{Code: "x", Message: "y"}}},
		{"neither", Result{}},
		{"empty failure identity", Result{Failure: &Failure{Code: "", Message: "y"}}},
		{"undeclared data", Result{Failure: &Failure{Code: "x", Message: "y", Data: json.RawMessage(`{}`)}}},
		{"wrong success shape", Result{Value: json.RawMessage(`{"bad":true}`)}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if err := ValidateResult(methods, "call", row.result); err == nil {
				t.Fatal("ValidateResult accepted an invalid union")
			}
		})
	}
	partial := Result{Failure: &Failure{Code: "x", Message: "y", Partial: json.RawMessage(`{}`)}}
	if err := ValidateResult(methods, "call", partial); err == nil {
		t.Fatal("ValidateResult accepted an undeclared partial")
	}
}

func TestValidateInstanceRejectsWrongBindingShape(t *testing.T) {
	compiled, err := Compile([]Native{testNative("ext", LifetimeHarness, []Capability{ToolCapability("tool")}, nil, nil)})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if err := compiled.ValidateInstance("ext", "ext-c", Instance{Capabilities: map[string]Implementation{"tool": {Service: map[string]Handler{"x": nil}}}}); err == nil {
		t.Fatal("wrong-kind implementation accepted")
	}
	if err := compiled.ValidateInstance("ext", "ext-c", Instance{Capabilities: map[string]Implementation{}}); err == nil {
		t.Fatal("missing capability accepted")
	}
}

func TestValidateSectionKeepsLosslessNumbers(t *testing.T) {
	compiled, err := Compile([]Native{testNative("ext", LifetimeHarness, []Capability{ToolCapability("tool")}, nil, nil)})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if err := compiled.ValidateSection("ext", json.RawMessage(`{"enabled":true,"value":9007199254740993}`)); err != nil {
		t.Fatalf("exact integer rejected: %v", err)
	}
	if err := compiled.ValidateSection("ext", json.RawMessage(`{"enabled":true,"value":"1"}`)); err == nil {
		t.Fatal("string was coerced into an integer value")
	}
	if err := compiled.ValidateSection("ext", json.RawMessage(`{"enabled":true,"extra":1}`)); err == nil {
		t.Fatal("undeclared section member accepted")
	}
}

func TestCompileAcceptsMultipleHookBoundaries(t *testing.T) {
	capability := HookCapability("hooks", SingleHook(BoundaryPrepare, HookModePure), SingleHook(BoundaryBeforeTool, HookModeEffectful))
	native := testNative("hooks", LifetimeHarness, []Capability{capability}, nil, func(context.Context, InstanceContext) (Instance, error) {
		return Instance{Capabilities: map[string]Implementation{"hooks": {
			Preparation: func(context.Context, CallContext, PreparationInput) (PreparationConsequence, error) {
				return PreparationConsequence{}, nil
			},
			BeforeTool: func(context.Context, CallContext, model.ToolCall) (json.RawMessage, error) { return nil, nil },
		}}}, nil
	})
	compiled, err := Compile([]Native{native})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	hooks, _ := compiled.Capability("hooks")
	if len(hooks.Hooks) != 2 || hooks.Methods["before_tool"].Mode != ModeServiceIO || hooks.Methods["prepare"].Mode != ModePure {
		t.Fatalf("hooks = %+v methods = %+v", hooks.Hooks, hooks.Methods)
	}
	for _, row := range []struct {
		name string
		cap  Capability
	}{
		{"duplicate boundary", HookCapability("dup", SingleHook(BoundaryPrepare, HookModePure), SingleHook(BoundaryPrepare, HookModePure))},
		{"effectful prepare", HookCapability("effect", SingleHook(BoundaryPrepare, HookModeEffectful))},
		{"effectful after_tool", HookCapability("after", SingleHook(BoundaryAfterTool, HookModeEffectful))},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := Compile([]Native{testNative("bad", LifetimeHarness, []Capability{row.cap}, nil, nil)}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Compile error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestCompileExposesIndividualMethodKeys(t *testing.T) {
	methods := []Method{PureMethod("read", `{}`, `{}`), PureMethod("write", `{}`, `{}`)}
	capability := ServiceCapability("service", "test.service", methods...)
	capability.Exposed = []ExposedMethod{{Method: "read", Kind: "query", Target: "session"}}
	native := testNative("ext", LifetimeHarness, []Capability{capability}, nil, nil)
	if _, err := Compile([]Native{native}); err != nil {
		t.Fatalf("Compile with one exposed method: %v", err)
	}
	capability.Exposed = []ExposedMethod{
		{Method: "read", Kind: "query", Target: "session"},
		{Method: "read", Kind: "management", Target: "harness"},
	}
	if _, err := Compile([]Native{testNative("ext", LifetimeHarness, []Capability{capability}, nil, nil)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate exposed method error = %v, want ErrInvalid", err)
	}
	capability.Exposed = []ExposedMethod{{Method: "ghost", Kind: "query", Target: "session"}}
	if _, err := Compile([]Native{testNative("ext", LifetimeHarness, []Capability{capability}, nil, nil)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("undeclared exposed method error = %v, want ErrInvalid", err)
	}
}

func TestCompileComparesMethodsBySetAndIgnoresParticipationMetadata(t *testing.T) {
	providerMethods := []Method{PureMethod("a", `{}`, `{}`), PureMethod("b", `{}`, `{}`)}
	provider := ServiceCapability("provider", "test.provider", providerMethods...)
	consumer := ServiceCapability("consumer", "test.consumer", PureMethod("call", `{}`, `{}`))
	reversed := []Method{providerMethods[1], providerMethods[0]}
	binding := Binding{Name: "dep", Provider: "ext/provider-c", CapabilityID: "provider", Definition: CapabilityDefinition{ContractID: "test.provider", ContractVersion: "1", Kind: KindService, Methods: reversed}}
	native := Native{
		Declaration: Declaration{
			ID:           "ext",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components: []Component{
				{ID: "provider-c", Lifetime: LifetimeHarness, Provides: []string{"provider"}},
				{ID: "consumer-c", Lifetime: LifetimeHarness, Provides: []string{"consumer"}, Requires: []Binding{binding}},
			},
			Capabilities: []Capability{provider, consumer},
		},
		Open: func(context.Context, InstanceContext) (Instance, error) {
			return Instance{Capabilities: map[string]Implementation{}}, nil
		},
	}
	if _, err := Compile([]Native{native}); err != nil {
		t.Fatalf("Compile with reversed binding methods: %v", err)
	}
	first := testNative("one", LifetimeHarness, []Capability{StrategyCapability("one.strategy", false)}, nil, nil)
	second := testNative("two", LifetimeHarness, []Capability{StrategyCapability("two.strategy", true)}, nil, nil)
	if _, err := Compile([]Native{first, second}); err != nil {
		t.Fatalf("strategy participation metadata became a global incompatibility: %v", err)
	}
}

func fixedCapabilityMethods(t *testing.T, capability Capability) map[string]*methodContract {
	t.Helper()
	native := testNative("schemaext", LifetimeHarness, []Capability{capability}, nil, nil)
	if capability.Kind == KindTool {
		native.Describe = map[string]ToolDescribe{capability.ID: func(context.Context, DescribeInput) (ToolDescription, error) {
			return ToolDescription{Definition: modelToolDefinition(capability.ID), Available: true}, nil
		}}
	}
	compiled, err := Compile([]Native{native})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	resolved, _ := compiled.Capability(capability.ID)
	return resolved.Methods
}

func TestFixedToolMethodSchemasCoverActualSemanticValues(t *testing.T) {
	methods := fixedCapabilityMethods(t, ToolCapability("tool"))
	call := `{"id":"call-1","name":"tool","arguments":"e30=","extra":[{"key":"aw==","value":"MQ=="}]}`
	calls := `{"extension_id":"ext","component_id":"ext-c","instance_id":"instance","lifetime":"operation","scope":{"kind":"operation","revision":"1"},"revision":"1","caller":{"extension_id":"ext","component_id":"ext-c","instance_id":"instance"}}`
	contextValue := `{"session":{"session_id":"s","workspace":"/w"},"model":{"provider":"p","model":"m"},"revision":"1","workspace":"/w","admitted_entry":{"session_id":"s","entry_id":"e"},"constraints":{"readonly":false,"write_dir":""},"config":"e30=","agent_types":[],"calls":` + calls + `}`
	plan := `{"permissions":[{"permission":"p","target":"t"}],"canonical_workspace":"/w","canonical_write_dir":"","data":"ZGF0YQ=="}`
	positive := []struct {
		method string
		input  string
	}{
		{"normalize", `{"context":` + contextValue + `,"call":` + call + `}`},
		{"prepare", `{"call":` + call + `,"context":` + contextValue + `}`},
		{"execute", `{"context":` + contextValue + `,"plan":` + plan + `}`},
	}
	for _, row := range positive {
		if err := ValidateArguments(methods, row.method, json.RawMessage(row.input)); err != nil {
			t.Fatalf("%s positive input: %v", row.method, err)
		}
	}
	if err := ValidateResult(methods, "prepare", Result{Value: json.RawMessage(plan)}); err != nil {
		t.Fatalf("prepare positive plan: %v", err)
	}
	outcome := `{"result":{"call_id":"call-1","status":"success","content":"ok"},"metadata":"e30=","evidence":"observed"}`
	if err := ValidateResult(methods, "execute", Result{Value: json.RawMessage(outcome)}); err != nil {
		t.Fatalf("execute positive outcome: %v", err)
	}
	negative := []struct {
		name   string
		method string
		input  string
	}{
		{"prepare missing context", "prepare", `{"call":` + call + `}`},
		{"prepare extra member", "prepare", `{"call":` + call + `,"context":` + contextValue + `,"extra":1}`},
		{"normalize missing context", "normalize", `{"call":` + call + `}`},
		{"call wrong argument type", "normalize", `{"context":` + contextValue + `,"call":{"id":"c","name":"t","arguments":5}}`},
		{"context missing session", "prepare", `{"call":` + call + `,"context":{"model":{"provider":"p","model":"m"},"revision":"1","workspace":"/w","admitted_entry":{"session_id":"s","entry_id":"e"},"constraints":{"readonly":false,"write_dir":""},"config":"","agent_types":[],"calls":` + calls + `}}`},
		{"execute missing plan", "execute", `{"context":` + contextValue + `}`},
		{"execute plan data wrong type", "execute", `{"context":` + contextValue + `,"plan":{"permissions":[],"data":5}}`},
	}
	for _, row := range negative {
		if err := ValidateArguments(methods, row.method, json.RawMessage(row.input)); err == nil {
			t.Fatalf("%s accepted invalid input %s", row.name, row.input)
		}
	}
	for _, row := range []struct {
		name   string
		method string
		value  string
	}{
		{"plan data wrong type", "prepare", `{"permissions":[],"data":5}`},
		{"plan extra member", "prepare", `{"permissions":[],"data":"","extra":1}`},
		{"outcome status outside enum", "execute", `{"result":{"call_id":"c","status":"pending","content":"x"},"evidence":"observed"}`},
		{"outcome missing evidence", "execute", `{"result":{"call_id":"c","status":"success","content":"x"}}`},
		{"outcome undeclared member", "execute", `{"result":{"call_id":"c","status":"success","content":"x"},"evidence":"observed","extra":1}`},
	} {
		if err := ValidateResult(methods, row.method, Result{Value: json.RawMessage(row.value)}); err == nil {
			t.Fatalf("%s accepted invalid result %s", row.name, row.value)
		}
	}
}

func TestFixedHookAndStrategySchemasCoverActualSemanticValues(t *testing.T) {
	hooks := fixedCapabilityMethods(t, HookCapability("hook", SingleHook(BoundaryPrepare, HookModePure)))
	session := `{"session_id":"s","workspace":"/w"}`
	prep := `{"session":` + session + `,"model":{"provider":"p","model":"m"},"constraints":{"readonly":false,"write_dir":""},"assembly":{"identity":"i","instructions":"u","mode":"full"},"prompt":"p","catalog":[],"tools":[],"settings":""}`
	if err := ValidateArguments(hooks, "prepare", json.RawMessage(prep)); err != nil {
		t.Fatalf("prepare hook input: %v", err)
	}
	consequence := `{"prompt":"","descriptions":{},"tool_names":[]}`
	if err := ValidateResult(hooks, "prepare", Result{Value: json.RawMessage(consequence)}); err != nil {
		t.Fatalf("prepare hook consequence: %v", err)
	}
	if err := ValidateArguments(hooks, "prepare", json.RawMessage(`{"prompt":"missing"}`)); err == nil {
		t.Fatal("prepare hook accepted a missing semantic input")
	}
	if err := ValidateResult(hooks, "prepare", Result{Value: json.RawMessage(`{"prompt":5}`)}); err == nil {
		t.Fatal("prepare hook accepted a wrong-typed consequence")
	}

	strategies := fixedCapabilityMethods(t, StrategyCapability("strategy", true))
	planInput := `{"trigger":"automatic","messages":[],"boundary_entry_id":"b","instructions":"i","tools":[],"context_window":10,"output_reserve":5,"settings":""}`
	if err := ValidateArguments(strategies, "plan", json.RawMessage(planInput)); err != nil {
		t.Fatalf("plan input: %v", err)
	}
	for _, output := range []string{
		`{"kind":"none"}`,
		`{"kind":"model","model":{"request":{"messages":[],"tools":[]},"state":"e30="}}`,
		`{"kind":"checkpoint","checkpoint":{"boundary_entry_id":"b","items":[{"retain":0},{"context":{"role":"user","content":[]}}]}}`,
	} {
		if err := ValidateResult(strategies, "plan", Result{Value: json.RawMessage(output)}); err != nil {
			t.Fatalf("plan output %s: %v", output, err)
		}
	}
	if err := ValidateResult(strategies, "plan", Result{Value: json.RawMessage(`{"kind":"none","model":{"request":{"messages":[],"tools":[]},"state":""}}`)}); err == nil {
		t.Fatal("plan output accepted a non-exclusive union")
	}
	if err := ValidateResult(strategies, "plan", Result{Value: json.RawMessage(`{"kind":"checkpoint","checkpoint":{"boundary_entry_id":"b","items":[]}}`)}); err == nil {
		t.Fatal("checkpoint accepted an empty item list")
	}
}

// TestFailurePayloadPresenceSemantics proves the common failure arm: bare
// code/message is always valid, Data and Partial are independently optional
// payloads declared by their own schema documents, undeclared or malformed
// payloads reject, and whitespace bytes are present rather than absent.
func TestFailurePayloadPresenceSemantics(t *testing.T) {
	service := ServiceCapability("service", "test.presence",
		ServiceMethod("bare", ModeServiceIO, `{}`, `{}`, "", ""),
		ServiceMethod("data", ModeServiceIO, `{}`, `{}`, `{"type":"object","required":["reason"],"properties":{"reason":{"type":"string"}},"additionalProperties":false}`, ""),
		ServiceMethod("partial", ModeServiceIO, `{}`, `{}`, "", `{"type":"string"}`),
	)
	compiled, err := Compile([]Native{testNative("ext", LifetimeHarness, []Capability{service}, nil, nil)})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	resolved, _ := compiled.Capability("service")
	methods := resolved.Methods
	bare := Result{Failure: &Failure{Code: "invalid", Message: "bad"}}
	if err := ValidateResult(methods, "bare", bare); err != nil {
		t.Fatalf("bare failure: %v", err)
	}
	if err := ValidateResult(methods, "data", Result{Failure: &Failure{Code: "invalid", Message: "bad", Data: json.RawMessage(`{"reason":"x"}`)}}); err != nil {
		t.Fatalf("declared data: %v", err)
	}
	// Partial is independent of Error: a method declaring only Partial accepts
	// it while the bare failure arm stays available.
	if err := ValidateResult(methods, "partial", Result{Failure: &Failure{Code: "invalid", Message: "bad", Partial: json.RawMessage(`"p"`)}}); err != nil {
		t.Fatalf("declared partial without error schema: %v", err)
	}
	rejected := []struct {
		name   string
		method string
		result Result
	}{
		{"undeclared data", "bare", Result{Failure: &Failure{Code: "x", Message: "y", Data: json.RawMessage(`{}`)}}},
		{"whitespace data on bare", "bare", Result{Failure: &Failure{Code: "x", Message: "y", Data: json.RawMessage("  ")}}},
		{"undeclared partial", "bare", Result{Failure: &Failure{Code: "x", Message: "y", Partial: json.RawMessage(`"p"`)}}},
		{"whitespace partial on bare", "bare", Result{Failure: &Failure{Code: "x", Message: "y", Partial: json.RawMessage("  ")}}},
		{"whitespace data on declared", "data", Result{Failure: &Failure{Code: "x", Message: "y", Data: json.RawMessage("  ")}}},
		{"wrong declared data", "data", Result{Failure: &Failure{Code: "x", Message: "y", Data: json.RawMessage(`{"reason":5}`)}}},
		{"whitespace partial on declared", "partial", Result{Failure: &Failure{Code: "x", Message: "y", Partial: json.RawMessage("  ")}}},
		{"wrong declared partial", "partial", Result{Failure: &Failure{Code: "x", Message: "y", Partial: json.RawMessage(`5`)}}},
		{"whitespace success", "bare", Result{Value: json.RawMessage("  ")}},
		{"empty code", "bare", Result{Failure: &Failure{Code: "", Message: "y"}}},
		{"empty message", "bare", Result{Failure: &Failure{Code: "x", Message: ""}}},
	}
	for _, row := range rejected {
		if err := ValidateResult(methods, row.method, row.result); err == nil {
			t.Fatalf("%s accepted", row.name)
		}
	}
}

// TestWhitespaceOptionalSchemaDocumentsAreMalformed proves zero raw length is
// absence while nonempty whitespace is a present, malformed schema document.
func TestWhitespaceOptionalSchemaDocumentsAreMalformed(t *testing.T) {
	service := ServiceCapability("service", "test.schema-presence", ServiceMethod("call", ModeServiceIO, `{}`, `{}`, "  ", ""))
	if _, err := Compile([]Native{testNative("ext", LifetimeHarness, []Capability{service}, nil, nil)}); err == nil {
		t.Fatal("Compile accepted a whitespace error schema document")
	}
	service = ServiceCapability("service", "test.schema-presence", ServiceMethod("call", ModeServiceIO, `{}`, `{}`, "", "  "))
	if _, err := Compile([]Native{testNative("ext", LifetimeHarness, []Capability{service}, nil, nil)}); err == nil {
		t.Fatal("Compile accepted a whitespace partial schema document")
	}
	service = ServiceCapability("service", "test.schema-presence", ServiceMethod("call", ModeServiceIO, `{}`, `{}`, "", `{"type":"string"}`))
	if _, err := Compile([]Native{testNative("ext", LifetimeHarness, []Capability{service}, nil, nil)}); err != nil {
		t.Fatalf("Compile refused partial without an error schema: %v", err)
	}
}

// TestFixedEffectfulHookSchemaCoversTerminalFailure proves the effectful hook
// methods describe their complete input and their fixed terminal failure
// payload: status/evidence values, wrong-type and undeclared siblings reject.
func TestFixedEffectfulHookSchemaCoversTerminalFailure(t *testing.T) {
	before := fixedCapabilityMethods(t, HookCapability("before", SingleHook(BoundaryBeforeTool, HookModeEffectful)))
	call := json.RawMessage(`{"id":"call-1","name":"tool","arguments":"e30="}`)
	if err := ValidateArguments(before, "before_tool", call); err != nil {
		t.Fatalf("before_tool input: %v", err)
	}
	if err := ValidateArguments(before, "before_tool", json.RawMessage(`{"id":"call-1","name":"tool"}`)); err == nil {
		t.Fatal("before_tool accepted a call without arguments")
	}
	if err := ValidateArguments(before, "before_tool", json.RawMessage(`{"id":"call-1","name":"tool","arguments":{}}`)); err == nil {
		t.Fatal("before_tool accepted non-opaque argument bytes")
	}
	if err := ValidateArguments(before, "before_tool", json.RawMessage(`{"id":"call-1","name":"tool","arguments":"e30=","extra":1}`)); err == nil {
		t.Fatal("before_tool accepted an undeclared member")
	}
	for _, row := range []struct {
		name   string
		result Result
	}{
		{"terminal error", Result{Failure: &Failure{Code: "hook", Message: "failed", Data: json.RawMessage(`{"status":"error","evidence":"observed"}`)}}},
		{"terminal interruption", Result{Failure: &Failure{Code: "hook", Message: "stopped", Data: json.RawMessage(`{"status":"interrupted","evidence":"not_started"}`)}}},
	} {
		if err := ValidateResult(before, "before_tool", row.result); err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
	}
	for _, row := range []struct {
		name string
		data string
	}{
		{"status outside enum", `{"status":"success","evidence":"observed"}`},
		{"missing evidence", `{"status":"error"}`},
		{"evidence outside enum", `{"status":"error","evidence":"maybe"}`},
		{"undeclared member", `{"status":"error","evidence":"observed","extra":1}`},
	} {
		result := Result{Failure: &Failure{Code: "hook", Message: "failed", Data: json.RawMessage(row.data)}}
		if err := ValidateResult(before, "before_tool", result); err == nil {
			t.Fatalf("%s accepted", row.name)
		}
	}
	request := fixedCapabilityMethods(t, HookCapability("request", SingleHook(BoundaryRequest, HookModeEffectful)))
	input := json.RawMessage(`{"request_id":"req-1","canonical_messages":[],"messages":[],"captured_prompt":"","prompt":"","catalog":[],"tools":[]}`)
	if err := ValidateArguments(request, "request", input); err != nil {
		t.Fatalf("request hook input: %v", err)
	}
	if err := ValidateArguments(request, "request", json.RawMessage(`{"request_id":"req-1","canonical_messages":[],"messages":[]}`)); err == nil {
		t.Fatal("request hook accepted a missing prompt/catalog/tools shape")
	}
	if err := ValidateArguments(request, "request", json.RawMessage(`{"request_id":"req-1","canonical_messages":[],"messages":[],"captured_prompt":"","prompt":"","catalog":[],"tools":[],"extra":1}`)); err == nil {
		t.Fatal("request hook accepted an undeclared member")
	}
}

// TestJSONAcceptanceRequiresSingleValueEOF proves the shared lossless decoder
// rejects any input after the first complete JSON value: a second value and a
// malformed tail both fail for every actual consumer, while a trailing
// whitespace-only tail remains valid.
func TestJSONAcceptanceRequiresSingleValueEOF(t *testing.T) {
	compiled, err := Compile([]Native{testNative("ext", LifetimeHarness, []Capability{ServiceCapability("service", "test.eof",
		ServiceMethod("call", ModeServiceIO, `{"type":"object"}`, `{"type":"object"}`, `{"type":"object"}`, `{"type":"object"}`))}, nil, nil)})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	service, _ := compiled.Capability("service")
	methods := service.Methods
	for _, tail := range []string{" {}", " null", ` 1`, " ]", `{"x":`} {
		section := `{"enabled":true}` + tail
		if err := compiled.ValidateSection("ext", json.RawMessage(section)); err == nil {
			t.Fatalf("ValidateSection accepted %q after the first value", tail)
		}
		arguments := `{}` + tail
		if err := ValidateArguments(methods, "call", json.RawMessage(arguments)); err == nil {
			t.Fatalf("ValidateArguments accepted %q after the first value", tail)
		}
		success := `{}` + tail
		if err := ValidateResult(methods, "call", Result{Value: json.RawMessage(success)}); err == nil {
			t.Fatalf("ValidateResult success accepted %q after the first value", tail)
		}
		failure := Result{Failure: &Failure{Code: "x", Message: "y", Data: json.RawMessage(success)}}
		if err := ValidateResult(methods, "call", failure); err == nil {
			t.Fatalf("ValidateResult failure data accepted %q after the first value", tail)
		}
		partial := Result{Failure: &Failure{Code: "x", Message: "y", Partial: json.RawMessage(success)}}
		if err := ValidateResult(methods, "call", partial); err == nil {
			t.Fatalf("ValidateResult partial accepted %q after the first value", tail)
		}
	}
	if err := compiled.ValidateSection("ext", json.RawMessage("{\"enabled\":true}  \n\t")); err != nil {
		t.Fatalf("trailing whitespace-only input rejected: %v", err)
	}
	if err := ValidateArguments(methods, "call", json.RawMessage("{} \n")); err != nil {
		t.Fatalf("trailing whitespace-only arguments rejected: %v", err)
	}
	// Contract reuse compares the complete decoded value: two schemas whose
	// first value is equal but whose tails differ are not the same contract.
	first := ServiceCapability("first", "test.eof.reuse", ServiceMethod("call", ModeServiceIO, `{}`, `{}`, "", ""))
	second := ServiceCapability("second", "test.eof.reuse", ServiceMethod("call", ModeServiceIO, `{} {}`, `{}`, "", ""))
	if _, err := Compile([]Native{
		testNative("one", LifetimeHarness, []Capability{first}, nil, nil),
		testNative("two", LifetimeHarness, []Capability{second}, nil, nil),
	}); err == nil {
		t.Fatal("Compile treated a trailing-value schema as structurally equal")
	}
}

func customService(id, contractID, version string, methods ...Method) Capability {
	return Capability{ID: id, ContractID: contractID, ContractVersion: version, Kind: KindService, Methods: methods}
}

// TestCustomContractVersionsAreOpaqueExactAndTupleKeyed proves author-defined
// named-service contract versions are nonempty opaque strings matched
// exactly, and contract reuse uses the identity pair rather than a rendered
// delimiter string.
func TestCustomContractVersionsAreOpaqueExactAndTupleKeyed(t *testing.T) {
	provider := testNative("provider", LifetimeHarness, []Capability{
		customService("svc", "author.service", "2", PureMethod("read", `{}`, `{}`)),
	}, nil, nil)
	if _, err := Compile([]Native{provider}); err != nil {
		t.Fatalf("Compile custom version 2: %v", err)
	}
	consumerBinding := func(version string) Binding {
		return Binding{Name: "dep", Provider: "provider/provider-c", CapabilityID: "svc", Definition: CapabilityDefinition{
			ContractID: "author.service", ContractVersion: version, Kind: KindService, Methods: []Method{method("read", ModePure, `{}`, `{}`, "", "")},
		}}
	}
	matching := testNative("matching", LifetimeHarness, []Capability{extensionServiceCapability("consumer")}, []Binding{consumerBinding("2")}, nil)
	if _, err := Compile([]Native{provider, matching}); err != nil {
		t.Fatalf("Compile matching custom version: %v", err)
	}
	different := testNative("different", LifetimeHarness, []Capability{extensionServiceCapability("consumer2")}, []Binding{consumerBinding("3")}, nil)
	if _, err := Compile([]Native{provider, different}); err == nil {
		t.Fatal("Compile accepted a binding whose custom contract version differs from its provider")
	}
	sameDefinition := testNative("reuse", LifetimeHarness, []Capability{
		customService("svc2", "author.service", "2", PureMethod("read", `{}`, `{}`)),
	}, nil, nil)
	if _, err := Compile([]Native{provider, sameDefinition}); err != nil {
		t.Fatalf("Compile identical reused custom contract: %v", err)
	}
	incompatible := testNative("incompatible", LifetimeHarness, []Capability{
		customService("svc3", "author.service", "2", PureMethod("read", `{"type":"object"}`, `{}`)),
	}, nil, nil)
	if _, err := Compile([]Native{provider, incompatible}); err == nil {
		t.Fatal("Compile accepted incompatible methods under the same custom contract identity")
	}
	// Delimiter-colliding identities are different contracts under tuple keys.
	collisionA := testNative("collisiona", LifetimeHarness, []Capability{customService("ca", "a", "1/v2", PureMethod("read", `{}`, `{}`))}, nil, nil)
	collisionB := testNative("collisionb", LifetimeHarness, []Capability{customService("cb", "a/v1", "2", PureMethod("read", `{"type":"object"}`, `{}`))}, nil, nil)
	if _, err := Compile([]Native{collisionA, collisionB}); err != nil {
		t.Fatalf("Compile treated delimiter-colliding contract identities as one: %v", err)
	}
}

func extensionServiceCapability(id string) Capability {
	return ServiceCapability(id, "test."+id, ServiceMethod("call", ModeServiceIO, `{}`, `{}`, "", ""))
}

// TestFixedContractIdentityIsEnforcedBothWays proves known fixed contract IDs
// retain their v1/kind/method definitions and cannot be reclassified as
// arbitrary services, while fixed kinds cannot use custom service contracts.
func TestFixedContractIdentityIsEnforcedBothWays(t *testing.T) {
	rejected := []Native{
		testNative("service-as-tool", LifetimeHarness, []Capability{customService("bad", ContractTool, "1", PureMethod("read", `{}`, `{}`))}, nil, nil),
		testNative("tool-as-service", LifetimeHarness, []Capability{Capability{ID: "bad", ContractID: "author.tool", ContractVersion: "1", Kind: KindTool, Methods: toolMethods()}}, nil, nil),
		testNative("hook-as-service", LifetimeHarness, []Capability{customService("bad", ContractHook, "1", PureMethod("read", `{}`, `{}`))}, nil, nil),
		testNative("strategy-as-service", LifetimeHarness, []Capability{customService("bad", ContractContextStrategy, "1", PureMethod("read", `{}`, `{}`))}, nil, nil),
		testNative("fixed-version", LifetimeHarness, []Capability{customService("bad", "melta.prompt", "2", PureMethod("read", `{}`, `{}`))}, nil, nil),
		testNative("wrong-methods", LifetimeHarness, []Capability{Capability{ID: "bad", ContractID: ContractTool, ContractVersion: ContractVersion, Kind: KindTool, Methods: []Method{method("describe", ModePure, `{}`, `{}`, "", "")}}}, nil, nil),
		testNative("hook-wrong-boundary", LifetimeHarness, []Capability{Capability{ID: "bad", ContractID: ContractHook, ContractVersion: ContractVersion, Kind: KindHook, Hooks: []Hook{{Boundary: BoundaryPrepare, Mode: HookModePure}}, Methods: hookMethods([]Hook{{Boundary: BoundaryRequest, Mode: HookModePure}})}}, nil, nil),
		testNative("definitions-wrong-kind", LifetimeHarness, []Capability{Capability{ID: "bad", ContractID: ContractAgentDefinitions, ContractVersion: ContractVersion, Kind: KindTool, Methods: agentDefinitionMethods()}}, nil, nil),
		testNative("definitions-wrong-version", LifetimeHarness, []Capability{Capability{ID: "bad", ContractID: ContractAgentDefinitions, ContractVersion: "2", Kind: KindService, Methods: agentDefinitionMethods()}}, nil, nil),
		testNative("definitions-wrong-methods", LifetimeHarness, []Capability{Capability{ID: "bad", ContractID: ContractAgentDefinitions, ContractVersion: ContractVersion, Kind: KindService, Methods: agentDefinitionMethods()[:1]}}, nil, nil),
		testNative("definitions-shorter-lifetime", LifetimeWorkspace, []Capability{AgentDefinitionsCapability("bad")}, nil, nil),
	}
	for i, native := range rejected {
		if _, err := Compile([]Native{native}); err == nil {
			t.Fatalf("Compile accepted reclassified fixed contract row %d", i)
		}
	}
	if _, err := Compile([]Native{
		testNative("tools", LifetimeHarness, []Capability{ToolCapability("tool")}, nil, nil),
		testNative("hooks", LifetimeHarness, []Capability{HookCapability("hook", SingleHook(BoundaryPrepare, HookModePure))}, nil, nil),
		testNative("strategies", LifetimeOperation, []Capability{StrategyCapability("strategy", true)}, nil, nil),
		testNative("definitions", LifetimeHarness, []Capability{AgentDefinitionsCapability("definitions")}, nil, nil),
	}); err != nil {
		t.Fatalf("Compile rejected valid fixed contracts: %v", err)
	}
}

// TestBindingProviderResolvesByCapabilityAndExactLabel proves a binding is
// resolved by its unique capability and then compared to the exact provider
// label, without splitting opaque extension/component IDs.
func TestBindingProviderResolvesByCapabilityAndExactLabel(t *testing.T) {
	opaqueCapability := extensionServiceCapability("opaque")
	provider := testNative("provider/with/slashes", LifetimeHarness, []Capability{opaqueCapability}, nil, nil)
	provider.Declaration.Components[0].ID = "component/with/slashes"
	label := provider.Declaration.ID + "/" + provider.Declaration.Components[0].ID
	binding := Binding{Name: "dep", Provider: label, CapabilityID: "opaque", Definition: opaqueCapability.Definition()}
	consumer := testNative("consumer", LifetimeHarness, []Capability{extensionServiceCapability("consumer")}, []Binding{binding}, nil)
	if _, err := Compile([]Native{provider, consumer}); err != nil {
		t.Fatalf("Compile opaque provider label: %v", err)
	}
	wrong := testNative("wrong", LifetimeHarness, []Capability{extensionServiceCapability("consumer2")}, []Binding{{
		Name: "dep", Provider: "other/place", CapabilityID: "opaque", Definition: opaqueCapability.Definition(),
	}}, nil)
	if _, err := Compile([]Native{provider, wrong}); err == nil {
		t.Fatal("Compile accepted a binding whose provider label does not identify the capability's provider")
	}
	unknown := testNative("unknown", LifetimeHarness, []Capability{extensionServiceCapability("consumer3")}, []Binding{{
		Name: "dep", Provider: label, CapabilityID: "ghost", Definition: opaqueCapability.Definition(),
	}}, nil)
	if _, err := Compile([]Native{provider, unknown}); err == nil {
		t.Fatal("Compile accepted an unknown binding capability ID")
	}
}

// TestFixedNormalizeSchemaCarriesFullBoundContext proves the normalize input
// describes the actual callback value: the bound ToolContext and call,
// including the shared readonly CallContext identity data but excluding the
// Call function itself.
func TestFixedNormalizeSchemaCarriesFullBoundContext(t *testing.T) {
	methods := fixedCapabilityMethods(t, ToolCapability("tool"))
	call := `{"id":"call-1","name":"tool","arguments":"e30="}`
	callContext := `{"extension_id":"ext","component_id":"ext-c","instance_id":"instance-1","lifetime":"operation","scope":{"kind":"operation","revision":"1","workspace":"/w","session_id":"s","operation_id":"op","invocation_id":"inv","purpose":"conversation"},"revision":"1","section":"e30=","session_id":"s","operation_id":"op","invocation_id":"inv","purpose":"conversation","request_id":"req","tool_call_id":"call-1","effect_id":"effect","caller":{"extension_id":"caller","component_id":"caller-c","instance_id":"caller-instance"}}`
	contextValue := `{"session":{"session_id":"s","workspace":"/w"},"model":{"provider":"p","model":"m"},"revision":"1","workspace":"/w","admitted_entry":{"session_id":"s","entry_id":"e"},"constraints":{"readonly":false,"write_dir":""},"config":"e30=","agent_types":[],"calls":` + callContext + `}`
	if err := ValidateArguments(methods, "normalize", json.RawMessage(`{"context":`+contextValue+`,"call":`+call+`}`)); err != nil {
		t.Fatalf("normalize full bound context: %v", err)
	}
	if err := ValidateArguments(methods, "prepare", json.RawMessage(`{"context":`+contextValue+`,"call":`+call+`}`)); err != nil {
		t.Fatalf("prepare full bound context: %v", err)
	}
	plan := `{"permissions":[],"data":"e30="}`
	if err := ValidateArguments(methods, "execute", json.RawMessage(`{"context":`+contextValue+`,"plan":`+plan+`}`)); err != nil {
		t.Fatalf("execute full bound context: %v", err)
	}
	rejected := []struct {
		name  string
		input string
	}{
		{"normalize missing context", `{"call":` + call + `}`},
		{"normalize context without calls", `{"context":{"session":{"session_id":"s","workspace":"/w"},"model":{"provider":"p","model":"m"},"revision":"1","workspace":"/w","admitted_entry":{"session_id":"s","entry_id":"e"},"constraints":{"readonly":false,"write_dir":""},"config":"e30=","agent_types":[]},"call":` + call + `}`},
		{"calls carries undeclared member", `{"context":` + strings.TrimSuffix(contextValue, "}") + `,"calls":{"call":{}}},"call":` + call + `}`},
		{"calls incomplete caller", `{"context":` + strings.TrimSuffix(contextValue, "}") + `,"calls":{"extension_id":"ext","component_id":"ext-c","instance_id":"instance","lifetime":"operation","scope":{"kind":"operation","revision":"1"},"revision":"1","caller":{}}},"call":` + call + `}`},
		{"calls wrong lifetime", `{"context":` + strings.Replace(strings.TrimSuffix(contextValue, "}"), `"lifetime":"operation"`, `"lifetime":"forever"`, 1) + `,"call":` + call + `}`},
	}
	for _, row := range rejected {
		if err := ValidateArguments(methods, "normalize", json.RawMessage(row.input)); err == nil {
			t.Fatalf("%s accepted", row.name)
		}
	}
}

// TestCompileRejectsSelfDependencyBeforeFactories proves a component's own
// capability edge is a dependency cycle under the same ordering rule as any
// other edge, rejected during compilation before any Open factory runs.
func TestCompileRejectsSelfDependencyBeforeFactories(t *testing.T) {
	var opens atomic.Int64
	capability := ServiceCapability("self", "test.self", ServiceMethod("call", ModeServiceIO, `{}`, `{}`, "", ""))
	binding := Binding{Name: "self", Provider: "selfext/selfext-c", CapabilityID: "self", Definition: capability.Definition()}
	native := testNative("selfext", LifetimeHarness, []Capability{capability}, []Binding{binding}, func(context.Context, InstanceContext) (Instance, error) {
		opens.Add(1)
		return Instance{}, nil
	})
	if _, err := Compile([]Native{native}); err == nil {
		t.Fatal("Compile accepted a self dependency")
	}
	if opens.Load() != 0 {
		t.Fatalf("self dependency reached %d factories, want compile-time rejection", opens.Load())
	}
}

// TestFixedExtraSchemaUsesFramedEntries proves fixed Extra values are an
// explicit entry list whose key and raw value bytes use canonical framing:
// noncompact/HTML/exact-number values and non-UTF8 key bytes survive, while
// object-shaped, unframed and wrong-shape siblings reject.
func TestFixedExtraSchemaUsesFramedEntries(t *testing.T) {
	methods := fixedCapabilityMethods(t, ToolCapability("tool"))
	encode := func(value []byte) string { return base64.StdEncoding.EncodeToString(value) }
	actual := model.Extra{"k\xff": json.RawMessage(`{ "large" : 9007199254740993, "exp":1e400, "html":"<&>", "escaped":"\u003c" }`)}
	var key, value string
	for rawKey, rawValue := range actual {
		key, value = encode([]byte(rawKey)), encode(rawValue)
	}
	call := `{"id":"call-extra","name":"tool","arguments":"e30=","extra":[{"key":"` + key + `","value":"` + value + `"}]}`
	if err := ValidateArguments(methods, "normalize", json.RawMessage(`{"context":`+minimalToolContext()+`,"call":`+call+`}`)); err != nil {
		t.Fatalf("framed extra entries rejected: %v", err)
	}
	for _, extra := range []string{
		`{"k":1}`,
		`[{"key":"` + encode([]byte("k")) + `"}]`,
		`[{"key":"` + key + `"}]`,
		`[{"key":"k","value":"` + value + `"}]`,
		`[{"key":` + strconv.Quote(key) + `,"value":` + strconv.Quote(value) + `,"extra":1}]`,
		`[{"key":"` + key + `","value":"not-base64!"}]`,
		`[[1,2]]`,
	} {
		input := `{"context":` + minimalToolContext() + `,"call":{"id":"call-extra","name":"tool","arguments":"e30=","extra":` + extra + `}}`
		if err := ValidateArguments(methods, "normalize", json.RawMessage(input)); err == nil {
			t.Fatalf("extra shape accepted: %s", extra)
		}
	}
}

func minimalToolContext() string {
	calls := `{"extension_id":"ext","component_id":"ext-c","instance_id":"instance","lifetime":"operation","scope":{"kind":"operation","revision":"1"},"revision":"1"}`
	return `{"session":{"session_id":"s","workspace":"/w"},"model":{"provider":"p","model":"m"},"revision":"1","workspace":"/w","admitted_entry":{"session_id":"s","entry_id":"e"},"constraints":{"readonly":false,"write_dir":""},"config":"e30=","agent_types":[],"calls":` + calls + `}`
}

// TestEmptyProvidesComponentCompiles proves an explicitly declared
// lifecycle-only component with no provided capabilities is a normal graph
// node that may declare a dependency; the runtime test covers real opening
// and once-only disposal. Missing dependencies and malformed component
// identities remain rejected.
func TestEmptyProvidesComponentCompiles(t *testing.T) {
	providerCapability := ServiceCapability("empty.provider", "test.empty.provider", ServiceMethod("call", ModeServiceIO, `{}`, `{}`, "", ""))
	provider := testNative("emptyprovider", LifetimeHarness, []Capability{providerCapability}, nil, nil)
	binding := Binding{Name: "dep", Provider: "emptyprovider/emptyprovider-c", CapabilityID: "empty.provider", Definition: providerCapability.Definition()}
	empty := testNative("emptycomponent", LifetimeHarness, nil, []Binding{binding}, nil)
	empty.Declaration.Components[0].Provides = nil
	if _, err := Compile([]Native{provider, empty}); err != nil {
		t.Fatalf("Compile empty-provides component: %v", err)
	}
	unknown := testNative("unknownprovider", LifetimeHarness, nil, []Binding{{Name: "dep", Provider: "missing/missing-c", CapabilityID: "missing", Definition: providerCapability.Definition()}}, nil)
	unknown.Declaration.Components[0].Provides = nil
	if _, err := Compile([]Native{unknown}); err == nil {
		t.Fatal("Compile accepted an empty component with an unknown dependency")
	}
	malformed := testNative("malformedempty", LifetimeHarness, nil, nil, nil)
	malformed.Declaration.Components[0].Provides = nil
	malformed.Declaration.Components[0].ID = ""
	if _, err := Compile([]Native{malformed}); err == nil {
		t.Fatal("Compile accepted an empty component with an empty identity")
	}
}

// TestFixedDescribeSchemaCarriesResolvedAgentInputs proves the describe method
// carries the complete resolved AgentInputs value, including instructions,
// assembly mode, model, tools, capabilities, constraints and strategy
// selection/settings, with no separate flattened copy.
func TestFixedDescribeSchemaCarriesResolvedAgentInputs(t *testing.T) {
	methods := fixedCapabilityMethods(t, ToolCapability("tool"))
	inputs := `{"model":{"provider":"p","model":"m"},"instructions":"resolved instructions","assembly_mode":"full","tools":["tool"],"capabilities":["capability"],"constraints":{"readonly":false,"write_dir":""},"strategy":{"id":"strategy","settings_base64":"e30="}}`
	input := `{"config":"e30=","model":{"provider":"p","model":"m"},"session":{"session_id":"s","workspace":"/w"},"constraints":{"readonly":false,"write_dir":""},"agent_types":[],"revision":"1","inputs":` + inputs + `}`
	if err := ValidateArguments(methods, "describe", json.RawMessage(input)); err != nil {
		t.Fatalf("describe resolved inputs: %v", err)
	}
	for _, mutation := range []struct {
		name  string
		input string
	}{
		{"missing inputs", `{"config":"e30=","model":{"provider":"p","model":"m"},"session":{"session_id":"s","workspace":"/w"},"constraints":{"readonly":false,"write_dir":""},"agent_types":[],"revision":"1"}`},
		{"wrong assembly mode", `{"config":"e30=","model":{"provider":"p","model":"m"},"session":{"session_id":"s","workspace":"/w"},"constraints":{"readonly":false,"write_dir":""},"agent_types":[],"revision":"1","inputs":{"model":{"provider":"p","model":"m"},"instructions":"","assembly_mode":"other","tools":[],"capabilities":[],"constraints":{"readonly":false,"write_dir":""},"strategy":{"id":""}}}`},
		{"unframed strategy settings", `{"config":"e30=","model":{"provider":"p","model":"m"},"session":{"session_id":"s","workspace":"/w"},"constraints":{"readonly":false,"write_dir":""},"agent_types":[],"revision":"1","inputs":{"model":{"provider":"p","model":"m"},"instructions":"","assembly_mode":"full","tools":[],"capabilities":[],"constraints":{"readonly":false,"write_dir":""},"strategy":{"id":"","settings_base64":"{}"}}}`},
		{"missing constraints", `{"config":"e30=","model":{"provider":"p","model":"m"},"session":{"session_id":"s","workspace":"/w"},"constraints":{"readonly":false,"write_dir":""},"agent_types":[],"revision":"1","inputs":{"model":{"provider":"p","model":"m"},"instructions":"","assembly_mode":"full","tools":[],"capabilities":[],"strategy":{"id":""}}}`},
		{"extra inputs member", `{"config":"e30=","model":{"provider":"p","model":"m"},"session":{"session_id":"s","workspace":"/w"},"constraints":{"readonly":false,"write_dir":""},"agent_types":[],"revision":"1","inputs":{"model":{"provider":"p","model":"m"},"instructions":"","assembly_mode":"full","tools":[],"capabilities":[],"constraints":{"readonly":false,"write_dir":""},"strategy":{"id":""},"extra":1}}`},
	} {
		if err := ValidateArguments(methods, "describe", json.RawMessage(mutation.input)); err == nil {
			t.Fatalf("%s accepted", mutation.name)
		}
	}
}

// TestFixedExecuteSchemaIsContextAndPlanOnly proves the Execute method input
// is exactly the context and accepted plan Native.Tool.Execute receives; the
// obsolete call member is neither required nor allowed.
func TestFixedExecuteSchemaIsContextAndPlanOnly(t *testing.T) {
	methods := fixedCapabilityMethods(t, ToolCapability("tool"))
	plan := `{"permissions":[],"data":"e30="}`
	valid := `{"context":` + minimalToolContext() + `,"plan":` + plan + `}`
	if err := ValidateArguments(methods, "execute", json.RawMessage(valid)); err != nil {
		t.Fatalf("execute context+plan: %v", err)
	}
	for _, input := range []string{
		`{"call":{"id":"call-1","name":"tool","arguments":"e30="},"context":` + minimalToolContext() + `,"plan":` + plan + `}`,
		`{"context":` + minimalToolContext() + `,"plan":` + plan + `,"extra":1}`,
		`{"plan":` + plan + `}`,
	} {
		if err := ValidateArguments(methods, "execute", json.RawMessage(input)); err == nil {
			t.Fatalf("execute accepted invalid input %s", input)
		}
	}
}

func TestSessionCleanupAndDataContracts(t *testing.T) {
	cleanup := CleanupCapability("cleanup")
	native := testNative("ext", LifetimeHarness, []Capability{ToolCapability("tool"), cleanup}, nil, nil)
	native.Declaration.Components = []Component{
		{ID: "tool-c", Lifetime: LifetimeHarness, Provides: []string{"tool"}},
		{ID: "cleanup-c", Lifetime: LifetimeHarness, Provides: []string{"cleanup"}},
	}
	compiled, err := Compile([]Native{native})
	if err != nil {
		t.Fatalf("Compile with one cleanup declaration: %v", err)
	}
	{
		resolved, ok := compiled.CleanupCapability("ext")
		if !ok || resolved.ContractID != ContractSessionCleanup || resolved.ID != "cleanup" {
			t.Fatalf("CleanupCapability = (%+v, %v), want the extension's declaration", resolved, ok)
		}
		if _, ok := compiled.CleanupCapability("other"); ok {
			t.Fatal("CleanupCapability resolved an unknown extension")
		}
	}

	hostData, ok := HostCapability(ContractData)
	if !ok || len(hostData.Methods) != 3 {
		t.Fatalf("host data capability = (%+v, %v), want the three fixed methods", hostData, ok)
	}
	bound := testNative("bound", LifetimeHarness, []Capability{ServiceCapability("caller", "test.caller", PureMethod("call", `{}`, `{}`))}, []Binding{{Name: "data", Provider: HostProvider, CapabilityID: ContractData, Definition: hostData}}, nil)
	if _, err := Compile([]Native{bound}); err != nil {
		t.Fatalf("Compile with the data host binding: %v", err)
	}

	for _, row := range []struct {
		name   string
		native Native
		want   string
	}{
		{"two cleanup declarations", func() Native {
			n := testNative("ext", LifetimeHarness, []Capability{CleanupCapability("a"), CleanupCapability("b")}, nil, nil)
			n.Declaration.Components = []Component{{ID: "c", Lifetime: LifetimeHarness, Provides: []string{"a", "b"}}}
			return n
		}(), "more than one"},
		{"shorter cleanup lifetime", func() Native {
			n := testNative("ext", LifetimeWorkspace, []Capability{CleanupCapability("cleanup")}, nil, nil)
			return n
		}(), "Harness lifetime"},
		{"cleanup method mismatch", func() Native {
			n := testNative("ext", LifetimeHarness, []Capability{CleanupCapability("cleanup")}, nil, nil)
			n.Declaration.Capabilities[0].Methods[0].Input = json.RawMessage(`{}`)
			return n
		}(), "exact method definitions"},
		{"data host definition mismatch", func() Native {
			n := testNative("ext", LifetimeHarness, []Capability{ServiceCapability("caller", "test.caller", PureMethod("call", `{}`, `{}`))}, []Binding{{Name: "data", Provider: HostProvider, CapabilityID: ContractData, Definition: CapabilityDefinition{ContractID: ContractData, ContractVersion: ContractVersion, Kind: KindService, Methods: []Method{PureMethod("session_namespace", `{}`, `{}`)}}}}, nil)
			return n
		}(), "expected host contract differs"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := Compile([]Native{row.native}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("Compile error = %v, want ErrInvalid containing %q", err, row.want)
			}
		})
	}
}
