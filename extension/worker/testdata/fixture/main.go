// Command fixture serves one native extension through the Go authoring
// adapter for host integration tests.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-declaration" {
		encoded, err := json.Marshal(worker.NewDeclaration(fixtureDeclaration()))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if _, err := os.Stdout.Write(encoded); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := worker.Serve(context.Background(), fixture(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func fixture() extension.Native {
	return extension.Native{
		Declaration: fixtureDeclaration(),
		Describe:    map[string]extension.ToolDescribe{"fixture.tool": describeTool},
		Open:        open,
	}
}

func fixtureDeclaration() extension.Declaration {
	prompt, _ := extension.HostCapability(extension.ContractPrompt)
	return extension.Declaration{
		ID:           "fixture",
		Version:      "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"additionalProperties":false}`),
		Components: []extension.Component{{
			ID:       "fixture.component",
			Lifetime: extension.LifetimeHarness,
			Provides: []string{"fixture.tool", "fixture.hooks", "fixture.strategy", "fixture.service"},
			Requires: []extension.Binding{{
				Name:         "prompt",
				Provider:     extension.HostProvider,
				CapabilityID: extension.ContractPrompt,
				Definition:   prompt,
			}},
		}},
		Capabilities: []extension.Capability{
			extension.ToolCapability("fixture.tool"),
			extension.HookCapability("fixture.hooks",
				extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure),
				extension.SingleHook(extension.BoundaryRequest, extension.HookModeEffectful),
				extension.SingleHook(extension.BoundaryBeforeTool, extension.HookModePure),
				extension.SingleHook(extension.BoundaryAfterTool, extension.HookModePure),
			),
			extension.StrategyCapability("fixture.strategy", true),
			extension.ServiceCapability("fixture.service", "fixture.service/v1",
				extension.ServiceMethod("assemble", extension.ModeServiceIO,
					`{"type":"object","required":["identity","instructions","mode"],"properties":{"identity":{"type":"string"},"instructions":{"type":"string"},"mode":{"type":"string","enum":["none","simple","full"]}},"additionalProperties":false}`,
					`{"type":"string"}`, "", "")),
		},
	}
}

func describeTool(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
	return extension.ToolDescription{
		Definition: model.ToolDefinition{Name: "fixture.tool", Description: "fixture tool", Parameters: json.RawMessage(`{"type":"object"}`)},
		Available:  true,
	}, nil
}

func open(context.Context, extension.InstanceContext) (extension.Instance, error) {
	return extension.Instance{
		Capabilities: map[string]extension.Implementation{
			"fixture.tool":     {Tool: fixtureTool{}},
			"fixture.hooks":    {Preparation: prepareHook, Request: requestHook, BeforeTool: beforeToolHook, AfterTool: afterToolHook},
			"fixture.strategy": {Strategy: fixtureStrategy{}},
			"fixture.service":  {Service: map[string]extension.Handler{"assemble": assemble}},
		},
		Close: func(context.Context, extension.CallContext) error { return nil },
	}, nil
}

type fixtureTool struct{}

func (fixtureTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func (fixtureTool) Prepare(_ context.Context, context extension.ToolContext, _ model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{
		Permissions:        []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: filepath.Join(context.Workspace, "fixture.txt")}},
		CanonicalWorkspace: context.Workspace,
	}
}

func (fixtureTool) Execute(_ context.Context, context extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Result: model.ToolResult{CallID: context.Calls.ToolCallID, Status: model.ResultSuccess, Content: "fixture result"}, Evidence: harness.ExecutionObserved}
}

func prepareHook(_ context.Context, _ extension.CallContext, _ extension.PreparationInput) (extension.PreparationConsequence, error) {
	return extension.PreparationConsequence{}, nil
}

func requestHook(_ context.Context, _ extension.CallContext, _ harness.RequestHookInput) (harness.RequestConsequence, error) {
	return harness.RequestConsequence{}, nil
}

func beforeToolHook(_ context.Context, _ extension.CallContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func afterToolHook(_ context.Context, _ extension.CallContext, input harness.ToolOutputInput) (string, error) {
	return input.Content, nil
}

type fixtureStrategy struct{}

func (fixtureStrategy) Plan(harness.PlanInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

func (fixtureStrategy) Consume(harness.ConsumeInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

func assemble(ctx context.Context, callContext extension.CallContext, _ json.RawMessage) (extension.Result, error) {
	return callContext.Call(ctx, "prompt", "assemble", json.RawMessage(`{"identity":"fixture","instructions":"","mode":"none"}`))
}
