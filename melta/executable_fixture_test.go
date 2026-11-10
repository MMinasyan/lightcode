package melta

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// buildExecutableFixture builds the shared Go authoring-adapter fixture at
// its default declaration version.
func buildExecutableFixture(t *testing.T) string {
	t.Helper()
	return buildExecutableFixtureVersion(t, "")
}

// buildExecutableFixtureVersion builds the shared fixture with an explicit
// declaration version stamped through the fixture's one version variable.
func buildExecutableFixtureVersion(t *testing.T, version string) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "fixture-worker")
	arguments := []string{"build"}
	if version != "" {
		arguments = append(arguments, "-ldflags=-X github.com/MMinasyan/lightcode/extension/worker/testdata/fixture/native.fixtureVersion="+version)
	}
	arguments = append(arguments, "-o", binary, "./testdata/fixture")
	command := exec.Command("go", arguments...)
	command.Dir = filepath.Join(working, "..", "extension", "worker")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("building fixture: %v\n%s", err, output)
	}
	return binary
}

func fixtureExecutableDeclaration(t *testing.T, binary string) extension.Declaration {
	t.Helper()
	output, err := exec.Command(binary, "-declaration").Output()
	if err != nil {
		t.Fatalf("fixture declaration: %v", err)
	}
	var declaration worker.Declaration
	if err := json.Unmarshal(output, &declaration); err != nil {
		t.Fatalf("decoding fixture declaration: %v", err)
	}
	return declaration.Value()
}

// executablePackageSource writes one local source package: exactly its
// manifest and entry point. A real entry-point file is copied; a
// test-spawned name gets a contained placeholder executable because the
// process spawn is faked.
func executablePackageSource(t *testing.T, executable executableExtension) string {
	t.Helper()
	source := t.TempDir()
	entryPoint := executable.EntryPoint
	data, err := os.ReadFile(executable.EntryPoint)
	if err != nil {
		entryPoint = filepath.Base(executable.EntryPoint)
		data = []byte("#!/bin/sh\nexit 0\n")
	} else {
		entryPoint = filepath.Base(entryPoint)
	}
	entryPath := filepath.Join(source, entryPoint)
	if err := os.MkdirAll(filepath.Dir(entryPath), 0o700); err != nil {
		t.Fatalf("source entry directory: %v", err)
	}
	if err := os.WriteFile(entryPath, data, 0o700); err != nil {
		t.Fatalf("source entry point: %v", err)
	}
	manifest, err := worker.EncodeManifest(executable.Declaration, entryPoint)
	if err != nil {
		t.Fatalf("encode package manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatalf("source manifest: %v", err)
	}
	return source
}

// installExecutablePackage installs one executable selection through the
// package manager.
func installExecutablePackage(t *testing.T, dataDir string, executable executableExtension) {
	t.Helper()
	if _, err := newPackageManager(dataDir).install(executablePackageSource(t, executable)); err != nil {
		t.Fatalf("install package %q: %v", executable.Declaration.ID, err)
	}
}

func TestExecutableFixtureEndToEnd(t *testing.T) {
	binary := buildExecutableFixture(t)
	declaration := fixtureExecutableDeclaration(t, binary)
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, `{"extensions":{"fixture":{"enabled":true}}}`)
	installExecutablePackage(t, dataDir, executableExtension{Declaration: declaration, EntryPoint: binary})
	owner, err := open(context.Background(), options{
		DataDir:       dataDir,
		Configuration: NewFileConfigurationSource(configPath),
		Credentials:   NewEnvironmentCredentialSource(filepath.Join(dataDir, ".env")),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		testassert.NoError(t, owner.Close(context.Background()))
	}()

	snapshot := owner.config.current()
	spec, ok := snapshot.composition.toolSpec("fixture.tool")
	if !ok {
		t.Fatal("fixture tool is not composed")
	}
	description, err := spec.describe(context.Background(), Invocation{snapshot: snapshot}, model.ModelRef{Provider: "provider", Model: "model"}, harness.ToolConstraints{}, harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace"}, harness.AgentInputs{Model: model.ModelRef{Provider: "provider", Model: "model"}})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if description.Definition.Name != "fixture.tool" || description.Definition.Description != "fixture tool" {
		t.Fatalf("description = %+v", description)
	}

	call := model.ToolCall{ID: "call-1", Name: "fixture.tool", Arguments: json.RawMessage(`{"value":1}`)}
	toolContext := ToolContext{
		Session:       harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace"},
		Model:         model.ModelRef{Provider: "provider", Model: "model"},
		Workspace:     "/workspace",
		AdmittedEntry: harness.EntryRef{SessionID: "session-1", EntryID: "entry-1"},
		Constraints:   harness.ToolConstraints{},
		Invocation:    Invocation{snapshot: snapshot},
	}
	outcome := preparedToolOutcome(t, snapshot.composition, []*scope{snapshot.resources.runtime}, "fixture.tool", toolContext, call)
	if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "fixture result" || outcome.Evidence != harness.ExecutionObserved {
		t.Fatalf("outcome = %+v", outcome)
	}

	service, ok := snapshot.resources.runtime.capabilities["fixture.service"]
	if !ok {
		t.Fatal("fixture service is not open")
	}
	hostcall := make(chan []byte, 1)
	callContext := extension.CallContext{
		ExtensionID: "fixture", ComponentID: "fixture.component", InstanceID: "instance",
		Lifetime: extension.LifetimeHarness, Revision: "1",
		Call: func(_ context.Context, binding, method string, arguments json.RawMessage) (extension.Result, error) {
			hostcall <- append([]byte(nil), arguments...)
			return extension.Result{Value: json.RawMessage(`"assembled"`)}, nil
		},
	}
	result, err := service.implementation.Service["assemble"](context.Background(), callContext, json.RawMessage(`{"identity":"fixture","instructions":"","mode":"none"}`))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if string(result.Value) != `"assembled"` {
		t.Fatalf("service result = %q", result.Value)
	}
	if got := <-hostcall; string(got) != `{"identity":"fixture","instructions":"","mode":"none"}` {
		t.Fatalf("hostcall arguments = %q", got)
	}

	hookContext := extension.CallContext{ExtensionID: "fixture", ComponentID: "fixture.component", InstanceID: "instance", Lifetime: extension.LifetimeHarness, Revision: "1"}
	hooks := snapshot.resources.runtime.capabilities["fixture.hooks"].implementation
	consequence, err := hooks.Preparation(context.Background(), hookContext, extension.PreparationInput{
		Session:  harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace"},
		Model:    model.ModelRef{Provider: "provider", Model: "model"},
		Assembly: promptAssembly(),
	})
	wantPrompt := "fixture-preparation:" + hashHex([]byte("provider/model:"))
	if err != nil || consequence.Prompt == nil || *consequence.Prompt != wantPrompt {
		t.Fatalf("preparation hook = %+v, %v, want %q", consequence, err, wantPrompt)
	}
	request, err := hooks.Request(context.Background(), hookContext, harness.RequestHookInput{RequestID: "request-1"})
	if err != nil || request.Prompt != nil || request.ToolNames == nil || len(request.ToolNames) != 0 {
		t.Fatalf("request hook = %+v, %v, want the empty visible subset", request, err)
	}
	wantArguments := json.RawMessage(`{"hooked":"{\"value\":1}"}`)
	arguments, err := hooks.BeforeTool(context.Background(), hookContext, call)
	if err != nil || string(arguments) != string(wantArguments) {
		t.Fatalf("before-tool hook = %q, %v, want %q", arguments, err, wantArguments)
	}
	wantShaped := "fixture-shaped:" + hashHex([]byte("raw")) + ":raw"
	shaped, err := hooks.AfterTool(context.Background(), hookContext, harness.ToolOutputInput{Content: "raw"})
	if err != nil || shaped != wantShaped {
		t.Fatalf("after-tool hook = %q, %v, want %q", shaped, err, wantShaped)
	}
	strategy := snapshot.resources.runtime.capabilities["fixture.strategy"].implementation.Strategy
	planOutput, err := strategy.Plan(harness.PlanInput{})
	if err != nil || planOutput.Kind != harness.PlanModel || planOutput.Model == nil || len(planOutput.Model.State) == 0 || len(planOutput.Model.Request.Messages) != 1 || !strings.HasPrefix(messageText(planOutput.Model.Request.Messages[0]), "fixture-plan:") {
		t.Fatalf("strategy plan = %+v, %v, want an input-derived model plan with state", planOutput, err)
	}

	// The fixture declaration and its process participate in the ordinary
	// configuration lifecycle.
	testassert.Require(t, snapshot.composition.compiled != nil, "fixture composition has no compiled declaration")
}

// openExecutables opens the production fixture with one executable extension
// and the real Agents definition resolver over its declared tools.
func (e *productionEnv) openExecutables(ctx context.Context, executables []executableExtension, declaration extension.Declaration) (*Melta, error) {
	for _, executable := range executables {
		installExecutablePackage(e.t, e.dataDir, executable)
	}
	useFixtureStore(e.t, e.store)
	return open(ctx, options{
		DataDir:       e.dataDir,
		Configuration: NewFileConfigurationSource(e.configPath),
		Credentials:   NewEnvironmentCredentialSource(filepath.Join(e.home, ".lightcode", ".env")),
		Locations:     e.locations(),
		Identity:      e.identity,
		Extensions:    []extension.Native{fixtureAgentPlugin(extension.Native{Declaration: declaration})},
	})
}

func addDenyRule(t *testing.T, configPath, permission, target string) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	rules, err := json.Marshal(map[string]any{"rules": []map[string]string{{"permission": permission, "target": target, "access": "deny"}}})
	if err != nil {
		t.Fatalf("encode rules: %v", err)
	}
	root["permissions"] = rules
	data, err = json.Marshal(root)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	writeServiceFile(t, configPath, string(data))
}

func TestExecutableFixtureRealAdmissionAuthorization(t *testing.T) {
	binary := buildExecutableFixture(t)
	declaration := fixtureExecutableDeclaration(t, binary)
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		enableFixturePlugin(t, e.configPath, "fixture")
		writeAgentDefinitions(t, e.configPath, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool"]}}`)
		e.server.setScript(func(_ context.Context, body string) []string {
			if lastMessageRole(body) == "tool" {
				return textTurnEvents("done")
			}
			return toolCallTurnEvents("call-fixture", "fixture.tool", `{"value":1}`)
		})
		r, err := e.openExecutables(ctx, []executableExtension{{Declaration: declaration, EntryPoint: binary}}, declaration)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		session, err := r.createSession(ctx, e.workspace("fixture-ws"), "fixture_agent")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-fixture", "run")
		awaitOperation(t, r, session.Identity.SessionID, "op-fixture", harness.OperationSuccess)
		if e.server.requests() != 2 {
			t.Fatalf("model requests = %d, want the tool turn and follow-up", e.server.requests())
		}
		if !strings.Contains(e.server.bodyAt(1), "fixture result") {
			t.Fatalf("follow-up body does not carry the executed fixture tool result: %s", e.server.bodyAt(1))
		}
	})
}

func TestExecutableFixtureRealAdmissionDenied(t *testing.T) {
	binary := buildExecutableFixture(t)
	declaration := fixtureExecutableDeclaration(t, binary)
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		enableFixturePlugin(t, e.configPath, "fixture")
		writeAgentDefinitions(t, e.configPath, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool"]}}`)
		target := filepath.Join(e.workspace("fixture-ws"), "fixture.txt")
		addDenyRule(t, e.configPath, harness.PermissionFileRead, target)
		e.server.setScript(func(_ context.Context, body string) []string {
			if lastMessageRole(body) == "tool" {
				return textTurnEvents("done")
			}
			return toolCallTurnEvents("call-fixture", "fixture.tool", `{"value":1}`)
		})
		r, err := e.openExecutables(ctx, []executableExtension{{Declaration: declaration, EntryPoint: binary}}, declaration)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		session, err := r.createSession(ctx, e.workspace("fixture-ws"), "fixture_agent")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-fixture", "run")
		awaitOperation(t, r, session.Identity.SessionID, "op-fixture", harness.OperationSuccess)
		body := e.server.bodyAt(1)
		if strings.Contains(body, "fixture result") {
			t.Fatalf("denied fixture tool executed: %s", body)
		}
		if !strings.Contains(strings.ToLower(body), "denied") && !strings.Contains(strings.ToLower(body), "permission") {
			t.Fatalf("denied follow-up lacks a denial result: %s", body)
		}
	})
}

func executableProbeDeclaration() extension.Declaration {
	credentials, _ := extension.HostCapability(extension.ContractCredentials)
	data, _ := extension.HostCapability(extension.ContractData)
	childLaunch, _ := extension.HostCapability(extension.ContractChildLaunch)
	return extension.Declaration{
		ID:           "execprobe",
		Version:      "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"token":{"type":"string"}},"additionalProperties":false}`),
		Secrets:      []extension.SecretRef{{Field: "/token", Lifetime: extension.SecretOperation}},
		Components: []extension.Component{{
			ID:       "execprobe.component",
			Lifetime: extension.LifetimeOperation,
			Provides: []string{"execprobe.tool"},
			Requires: []extension.Binding{
				{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: credentials},
				{Name: "data", Provider: extension.HostProvider, CapabilityID: extension.ContractData, Definition: data},
				{Name: "child_launch", Provider: extension.HostProvider, CapabilityID: extension.ContractChildLaunch, Definition: childLaunch},
			},
		}},
		Capabilities: []extension.Capability{extension.ToolCapability("execprobe.tool")},
	}
}

type executableProbeTool struct {
	childTarget string
}

func (t *executableProbeTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func (t *executableProbeTool) Prepare(_ context.Context, _ extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{
		Permissions: []harness.PermissionRequest{{Permission: harness.PermissionChildLaunch, Target: t.childTarget}},
		Data:        append([]byte(nil), call.Arguments...),
	}
}

func (t *executableProbeTool) Execute(ctx context.Context, toolContext extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	var arguments struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(plan.Data, &arguments); err != nil {
		return harness.ToolOutcome{Result: model.ToolResult{CallID: toolContext.Calls.ToolCallID, Status: model.ResultError, Content: err.Error()}, Evidence: harness.ExecutionObserved}
	}
	var call extension.Result
	var err error
	switch arguments.Mode {
	case "credentials":
		call, err = toolContext.Calls.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/token"}`))
	case "data":
		call, err = toolContext.Calls.Call(ctx, "data", "cache_namespace", json.RawMessage(`{}`))
	case "child":
		call, err = toolContext.Calls.Call(ctx, "child_launch", "launch", json.RawMessage(`{"definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"child_agent"},"prompt":"child task","operation_id":"child-op","max_concurrent":1,"output_limit":1024}`))
	default:
		err = errors.New("unknown mode")
	}
	if err != nil {
		return harness.ToolOutcome{Result: model.ToolResult{CallID: toolContext.Calls.ToolCallID, Status: model.ResultError, Content: err.Error()}, Evidence: harness.ExecutionObserved}
	}
	content := string(call.Value)
	if call.Failure != nil {
		content = call.Failure.Code + ":" + call.Failure.Message
	}
	return harness.ToolOutcome{Result: model.ToolResult{CallID: toolContext.Calls.ToolCallID, Status: model.ResultSuccess, Content: content}, Evidence: harness.ExecutionObserved}
}

func enableExecutableProbe(t *testing.T, configPath, token string) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	var extensions map[string]json.RawMessage
	if len(root["extensions"]) != 0 {
		if err := json.Unmarshal(root["extensions"], &extensions); err != nil {
			t.Fatalf("decode extensions: %v", err)
		}
	}
	if extensions == nil {
		extensions = map[string]json.RawMessage{}
	}
	extensions["execprobe"] = json.RawMessage(`{"enabled":true,"token":"` + token + `"}`)
	root["extensions"], err = json.Marshal(extensions)
	if err != nil {
		t.Fatalf("encode extensions: %v", err)
	}
	data, err = json.Marshal(root)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	writeServiceFile(t, configPath, string(data))
}

func TestExecutableOperationToolDescribeHostcallsAndMetadata(t *testing.T) {
	const token = "EXECPROBE_SECRET"
	t.Setenv(token, "secret-value")
	declaration := executableProbeDeclaration()
	childTarget := (harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: "child_agent"}).PermissionTarget()
	tool := &executableProbeTool{childTarget: childTarget}
	bigDescription := strings.Repeat("m", 1<<20)
	opens := 0
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		native := adapterSimpleNative(declaration, tool, nil)
		native.Describe["execprobe.tool"] = func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
			return extension.ToolDescription{
				Definition: model.ToolDefinition{Name: "execprobe.tool", Parameters: json.RawMessage(`{"type":"object","description":"` + bigDescription + `"}`)},
				Available:  true,
			}, nil
		}
		original := native.Open
		native.Open = func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
			opens++
			return original(ctx, instance)
		}
		return native, nil
	}}
	spawner.install()
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		opens = 0
		enableExecutableProbe(t, e.configPath, token)
		writeAgentDefinitions(t, e.configPath, `{"execprobe_agent":{"model":"prov/m","system_prompt":"simple","tools":["execprobe.tool"]},"child_agent":{"model":"prov/m","system_prompt":"simple","tools":[]}}`)
		e.server.setScript(func(_ context.Context, body string) []string {
			if lastMessageRole(body) == "tool" {
				return textTurnEvents("done")
			}
			user := lastUserText(body)
			if strings.Contains(user, "child task") {
				return textTurnEvents("child done")
			}
			switch {
			case strings.Contains(user, "credentials"):
				return toolCallTurnEvents("call-credentials", "execprobe.tool", `{"mode":"credentials"}`)
			case strings.Contains(user, "data"):
				return toolCallTurnEvents("call-data", "execprobe.tool", `{"mode":"data"}`)
			case strings.Contains(user, "child"):
				return toolCallTurnEvents("call-child", "execprobe.tool", `{"mode":"child"}`)
			}
			return textTurnEvents("done")
		})
		r, err := e.openExecutables(ctx, []executableExtension{{Declaration: declaration, EntryPoint: "execprobe"}}, declaration)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		snapshot := r.config.current()
		spec, ok := snapshot.composition.toolSpec("execprobe.tool")
		if !ok {
			t.Fatal("execprobe tool is not composed")
		}
		description, err := spec.describe(ctx, Invocation{snapshot: snapshot}, model.ModelRef{Provider: "prov", Model: "m"}, harness.ToolConstraints{}, harness.SessionIdentity{SessionID: "describe", Workspace: "/w"}, harness.AgentInputs{Model: model.ModelRef{Provider: "prov", Model: "m"}})
		if err != nil {
			t.Fatalf("describe before open: %v", err)
		}
		if len(description.Definition.Parameters) < 1<<20 {
			t.Fatalf("metadata parameters = %d bytes, want the 1 MiB round trip", len(description.Definition.Parameters))
		}
		testassert.Equal(t, opens, 0)
		session, err := r.createSession(ctx, e.workspace("execprobe-ws"), "execprobe_agent")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		for _, mode := range []string{"credentials", "data", "child"} {
			submitThroughRuntime(t, r, session.Identity.SessionID, "op-"+mode, mode)
			awaitOperation(t, r, session.Identity.SessionID, "op-"+mode, harness.OperationSuccess)
		}
		if opens == 0 {
			t.Fatal("Operation-lifetime tool never opened during admission")
		}
		// Each tool result is the follow-up request after its operation.
		var bodies []string
		for i := 0; i < e.server.requests(); i++ {
			bodies = append(bodies, e.server.bodyAt(i))
		}
		joined := strings.Join(bodies, "\n")
		if !strings.Contains(joined, "secret-value") {
			t.Fatal("credentials hostcall result did not reach the model")
		}
		if !strings.Contains(joined, "cache") || !strings.Contains(joined, "execprobe") {
			t.Fatal("data hostcall result did not reach the model")
		}
		if !strings.Contains(joined, "child_session_id") {
			t.Fatal("child launch hostcall result did not reach the model")
		}
	})
}

// openExecutablesWith opens the production fixture with executable extensions
// and supplied native plugins.
func (e *productionEnv) openExecutablesWith(ctx context.Context, executables []executableExtension, extras ...extension.Native) (*Melta, error) {
	for _, executable := range executables {
		installExecutablePackage(e.t, e.dataDir, executable)
	}
	useFixtureStore(e.t, e.store)
	return open(ctx, options{
		DataDir:       e.dataDir,
		Configuration: NewFileConfigurationSource(e.configPath),
		Credentials:   NewEnvironmentCredentialSource(filepath.Join(e.home, ".lightcode", ".env")),
		Locations:     e.locations(),
		Identity:      e.identity,
		Extensions:    extras,
	})
}

type blockingStrategy struct {
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingStrategy) Plan(harness.PlanInput) (harness.PlanOutput, error) {
	if !s.block.Load() {
		return harness.PlanOutput{Kind: harness.PlanNone}, nil
	}
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

func (s *blockingStrategy) Consume(harness.ConsumeInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

func TestExecutableStrategyOwnerCloseBounded(t *testing.T) {
	grace := executableShutdownGrace
	executableShutdownGrace = 200 * time.Millisecond
	defer func() { executableShutdownGrace = grace }()

	declaration := extension.Declaration{
		ID:           "strategyprobe",
		Version:      "1",
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"],"additionalProperties":false}`),
		Components: []extension.Component{{
			ID:       "strategyprobe-c",
			Lifetime: extension.LifetimeOperation,
			Provides: []string{"probe.strategy"},
		}},
		Capabilities: []extension.Capability{extension.StrategyCapability("probe.strategy", true)},
	}
	strategy := &blockingStrategy{entered: make(chan struct{}), release: make(chan struct{})}
	spawner := &adapterSpawner{t: t, factory: func() (extension.Native, func()) {
		return extension.Native{
				Declaration: declaration,
				Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
					return extension.Instance{Capabilities: map[string]extension.Implementation{
						"probe.strategy": {Strategy: strategy},
					}}, nil
				},
			}, func() {
				select {
				case <-strategy.release:
				default:
					close(strategy.release)
				}
			}
	}}
	spawner.install()
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		writeServiceFile(t, e.configPath, `{"providers":{"prov":`+nominalProvMember(e.server.URL, "",
			`{"m":{"name":"M","context_window":8192,"max_output_tokens":4096},"k":{"name":"K","context_window":32768,"max_output_tokens":4096}}`)+`},"compaction":{"model":"prov/k"},"extensions":{"guestdefs":{"enabled":true},"strategyprobe":{"enabled":true}}}`)
		e.server.setScript(func(_ context.Context, body string) []string {
			if strings.Contains(body, `"model":"k"`) || strings.Contains(body, `"model": "k"`) {
				return append(textTurnEvents("summary"), `{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
			}
			if lastUserText(body) == "seed" {
				return append(textTurnEvents("seeded answer"), `{"usage":{"prompt_tokens":5000,"completion_tokens":10,"total_tokens":5010}}`)
			}
			return textTurnEvents("trigger")
		})
		guest := namedResolverExtension("guestdefs", map[string]harness.AgentInputs{
			"named": {
				Model:        model.ModelRef{Provider: "prov", Model: "m"},
				Instructions: "named prompt",
				AssemblyMode: "simple",
				Tools:        []string{},
				Strategy:     harness.StrategySelection{ID: "probe.strategy"},
			},
		})
		owner, err := e.openExecutablesWith(ctx, []executableExtension{{Declaration: declaration, EntryPoint: "strategyprobe"}}, guest)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		session := createNamedSession(t, owner, e.dataDir, "guestdefs", "guestdefs.definitions", "named")
		submitThroughRuntime(t, owner, session.Identity.SessionID, "seed-op", "seed")
		awaitOperation(t, owner, session.Identity.SessionID, "seed-op", harness.OperationSuccess)
		strategy.block.Store(true)
		submitThroughRuntime(t, owner, session.Identity.SessionID, "trigger-op", "trigger")
		select {
		case <-strategy.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("worker strategy Plan never started")
		}
		closed := make(chan error, 1)
		go func() { closed <- owner.Close(context.Background()) }()
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Fatal("owner Close blocked on an unresponsive strategy Plan")
		}
	})
}

// messageText returns one message's first text part.
func messageText(message model.Message) string {
	for _, part := range message.Content {
		if part.Kind == model.PartText {
			return part.Text
		}
	}
	return ""
}
