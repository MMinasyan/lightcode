package melta

import (
	"context"
	"encoding/json"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

const testConfigSchema = `{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"],"additionalProperties":true}`

func testNative(id string, lifetime extension.Lifetime, capabilities []extension.Capability, requires []extension.Binding, open func(context.Context, extension.InstanceContext) (extension.Instance, error)) extension.Native {
	componentID := id + "#component"
	provides := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		provides = append(provides, capability.ID)
	}
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           id,
			Version:      "1",
			ConfigSchema: json.RawMessage(testConfigSchema),
			Components:   []extension.Component{{ID: componentID, Lifetime: lifetime, Provides: provides, Requires: requires}},
			Capabilities: capabilities,
		},
		Describe: map[string]extension.ToolDescribe{},
		Open:     open,
	}
}

func testServiceCapability(id string) extension.Capability {
	return extension.ServiceCapability(id, id+".contract", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{}`, "", ""))
}

func testServiceHandler() extension.Handler {
	return func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	}
}

func testServiceImplementation() extension.Implementation {
	return extension.Implementation{Service: map[string]extension.Handler{"call": testServiceHandler()}}
}

func testToolNative(id string, lifetime extension.Lifetime, tool extension.Tool, describe func(context.Context, extension.DescribeInput) (extension.ToolDescription, error)) extension.Native {
	native := testNative(id, lifetime, []extension.Capability{extension.ToolCapability(id)}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{id: {Tool: tool}}}, nil
	})
	if describe != nil {
		native.Describe[id] = describe
	}
	return native
}

func testDescribeTool(id string) extension.ToolDescribe {
	return func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
		return extension.ToolDescription{Definition: model.ToolDefinition{Name: id, Description: id, Parameters: json.RawMessage(`{"type":"object"}`)}, Available: true}, nil
	}
}

func extensionToolDescribe(id string, describe func(Invocation, harness.ToolConstraints, harness.SessionIdentity) (ToolDescription, error)) extension.ToolDescribe {
	return func(_ context.Context, input extension.DescribeInput) (extension.ToolDescription, error) {
		description, err := describe(Invocation{}, input.Constraints, input.Session)
		if err != nil {
			return extension.ToolDescription{}, err
		}
		return extension.ToolDescription{Definition: description.Definition, Available: description.Available}, nil
	}
}

func serviceInstance(capabilityID string, closeFn func() error) extension.Instance {
	instance := extension.Instance{Capabilities: map[string]extension.Implementation{capabilityID: testServiceImplementation()}}
	if closeFn != nil {
		instance.Close = func(context.Context, extension.CallContext) error { return closeFn() }
	}
	return instance
}
