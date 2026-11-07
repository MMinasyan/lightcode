// Package tasks declares the child-session task tool: it composes the
// subagent roster description, validates the requested subagent type, and
// launches one child Session through the declared child-launch host service,
// returning the child session ID immediately. It is an ordinary
// Harness-scoped native extension with one named-service binding and no
// additional Runtime authority.
package tasks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/toolargs"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

const (
	pluginID = "agents"
	toolID   = "task"
)

// taskDescriptionPrefix is the fixed model-facing introduction every task
// tool description opens with.
const taskDescriptionPrefix = "Spawn a subagent to work on a task in the background. The subagent runs in its own context with the toolset defined by its subagent_type. You will be notified when it completes; the result arrives as a message. Continue with your next steps in the meantime."

// taskParameters is the retained schema: prompt and subagent_type, both
// required strings, with their retained leaf descriptions.
const taskParameters = `{"type":"object","properties":{"prompt":{"type":"string","description":"The task prompt for this subagent."},"subagent_type":{"type":"string","description":"The type of subagent to use."}},"required":["prompt","subagent_type"]}`

// taskStartedTemplate is the retained immediate success text; the single
// argument is the child session ID. No live-members line is forced.
const taskStartedTemplate = "Task started in the background with session ID: `%s`. You will be notified when it completes. Continue with your next steps; the result will arrive as a message when it is ready."

const configSchema = `{"type":"object","required":["enabled"],"properties":{"enabled":{"type":"boolean"},"definitions":true,"task":{"anyOf":[{"type":"null"},{"type":"object","properties":{"max_concurrent":{"anyOf":[{"type":"integer","minimum":1,"maximum":20},{"type":"null"}]},"max_output_bytes":{"anyOf":[{"type":"integer","minimum":1024,"maximum":1048576},{"type":"null"}]}},"additionalProperties":true}]}},"additionalProperties":true}`

// settings is the tool's private configuration, decoded once from the task
// member of the active extensions.agents section at Open. Nil input and
// missing or null-valued members keep the defaults; unknown members are
// ignored; every consumed field is range-checked against the retained
// bounds.
type settings struct {
	MaxConcurrent  int `json:"max_concurrent"`
	MaxOutputBytes int `json:"max_output_bytes"`
}

func defaultSettings() settings {
	return settings{MaxConcurrent: 4, MaxOutputBytes: 15360}
}

// decodeSettings decodes the task member on top of the defaults and enforces
// the retained ranges.
func decodeSettings(raw json.RawMessage) (settings, error) {
	s := defaultSettings()
	if len(bytes.TrimSpace(raw)) != 0 {
		var section struct {
			Task json.RawMessage `json:"task"`
		}
		if err := json.Unmarshal(raw, &section); err != nil {
			return settings{}, fmt.Errorf("tasks: invalid settings: %w", err)
		}
		if len(section.Task) != 0 {
			if err := json.Unmarshal(section.Task, &s); err != nil {
				return settings{}, fmt.Errorf("tasks: invalid settings: %w", err)
			}
		}
	}
	switch {
	case s.MaxConcurrent < 1 || s.MaxConcurrent > 20:
		return settings{}, errors.New("tasks: max_concurrent must be between 1 and 20")
	case s.MaxOutputBytes < 1024 || s.MaxOutputBytes > 1048576:
		return settings{}, errors.New("tasks: max_output_bytes must be between 1024 and 1048576")
	}
	return s, nil
}

// launchData is the tool's opaque ToolPlan payload: the validated call facts
// needed by Execute after authorization.
type launchData struct {
	CallID         string `json:"call_id"`
	Prompt         string `json:"prompt"`
	SubagentType   string `json:"subagent_type"`
	MaxConcurrent  int    `json:"max_concurrent"`
	MaxOutputBytes int    `json:"max_output_bytes"`
}

// taskTool is the task contribution: unavailable to child Sessions, its
// description composes the Subagent roster from the captured Agent roster,
// preparation uses the settings retained at Open and validates the call
// before declaring the one child.launch pair, and execution launches through
// the declared host service.
type taskTool struct {
	settings settings
}

func (taskTool) describe(_ context.Context, input extension.DescribeInput) (extension.ToolDescription, error) {
	var b strings.Builder
	b.WriteString(taskDescriptionPrefix)
	b.WriteString("\n\nAvailable subagent types:\n")
	for _, at := range input.AgentTypes {
		if at.Subagent {
			fmt.Fprintf(&b, "- %s: %s\n", at.Name, at.Description)
		}
	}
	return extension.ToolDescription{
		Definition: model.ToolDefinition{
			Name:        toolID,
			Description: b.String(),
			Parameters:  json.RawMessage(taskParameters),
		},
		Available: input.Session.ParentSessionID == "",
	}, nil
}

func (taskTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return normalizeCallArguments(call, normalizeTaskArgs)
}

func (t taskTool) Prepare(_ context.Context, tc extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	s := t.settings
	// Preparation accepts only the committed normalized arguments: one
	// strict decode-and-pass, never a second normalization.
	args, err := toolargs.Decode(call.Arguments)
	if err != nil {
		return immediateError(call.ID, err)
	}
	prompt, ok := args["prompt"].(string)
	if !ok {
		return immediateError(call.ID, errors.New("missing 'prompt' parameter"))
	}
	subagentType, ok := args["subagent_type"].(string)
	if !ok {
		return immediateError(call.ID, errors.New("missing 'subagent_type' parameter"))
	}
	if err := resolveSubagent(subagentType, tc.AgentTypes); err != nil {
		return immediateError(call.ID, fmt.Errorf("unknown subagent type %q: %w", subagentType, err))
	}
	data, err := json.Marshal(launchData{CallID: call.ID, Prompt: prompt, SubagentType: subagentType, MaxConcurrent: s.MaxConcurrent, MaxOutputBytes: s.MaxOutputBytes})
	if err != nil {
		return immediateError(call.ID, err)
	}
	return extension.ToolPlan{
		Permissions: []harness.PermissionRequest{{Permission: harness.PermissionChildLaunch, Target: subagentType}},
		Data:        data,
	}
}

func (taskTool) Execute(ctx context.Context, tc extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	var data launchData
	if err := json.Unmarshal(plan.Data, &data); err != nil {
		return harness.ToolOutcome{Result: model.ToolResult{Status: model.ResultError, Content: err.Error()}}
	}
	callID := data.CallID
	operationID, err := newOperationID()
	if err != nil {
		return launchOutcome(callID, err)
	}
	arguments, err := json.Marshal(struct {
		AgentType     string `json:"agent_type"`
		Prompt        string `json:"prompt"`
		OperationID   string `json:"operation_id"`
		MaxConcurrent int    `json:"max_concurrent"`
		OutputLimit   int    `json:"output_limit"`
	}{
		AgentType:     data.SubagentType,
		Prompt:        data.Prompt,
		OperationID:   operationID,
		MaxConcurrent: data.MaxConcurrent,
		OutputLimit:   data.MaxOutputBytes,
	})
	if err != nil {
		return launchOutcome(callID, err)
	}
	result, err := tc.Calls.Call(ctx, "child_launch", "launch", arguments)
	if err != nil {
		return launchOutcome(callID, err)
	}
	if result.Failure != nil {
		return launchOutcome(callID, errors.New(result.Failure.Message))
	}
	var launched struct {
		ChildSessionID string `json:"child_session_id"`
	}
	if err := json.Unmarshal(result.Value, &launched); err != nil {
		return launchOutcome(callID, err)
	}
	return harness.ToolOutcome{Result: model.ToolResult{
		CallID:  callID,
		Status:  model.ResultSuccess,
		Content: fmt.Sprintf(taskStartedTemplate, launched.ChildSessionID),
	}}
}

// resolveSubagent checks the captured roster for the exactly-named Subagent
// entry: an absent name and a present non-subagent type produce the retained
// inner errors the caller wraps.
func resolveSubagent(name string, roster []protocol.Agent) error {
	for _, at := range roster {
		if at.Name != name {
			continue
		}
		if !at.Subagent {
			return errors.New("agent type is not available as a subagent")
		}
		return nil
	}
	return fmt.Errorf("unknown agent type %q", name)
}

// launchOutcome renders one launch failure through the uniform tool-error
// path: the harness's rejection details are model-facing and arrive unwrapped.
func launchOutcome(callID string, err error) harness.ToolOutcome {
	return harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: err.Error()}}
}

// immediateError is the normalization-class immediate outcome: status error
// carrying the validation diagnostic, per the tool-boundary validation
// contract.
func immediateError(callID string, cause error) extension.ToolPlan {
	return extension.ToolPlan{Immediate: &harness.ToolOutcome{
		Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: cause.Error()},
	}}
}

// newOperationID mints a fresh 32-lowercase-hex operation identity.
func newOperationID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// normalizeTaskArgs is the task tool's member validator: private
// `_lightcode_` fields are stripped, a present prompt or subagent_type must
// be a string, and every other member is preserved; Prepare owns the
// semantics.
func normalizeTaskArgs(args map[string]any) (map[string]any, error) {
	clean := make(map[string]any, len(args))
	for key, value := range args {
		if strings.HasPrefix(key, "_lightcode_") {
			continue
		}
		if key == "prompt" || key == "subagent_type" {
			if _, ok := value.(string); !ok {
				return nil, fmt.Errorf("task: %s must be a string", key)
			}
		}
		clean[key] = value
	}
	return clean, nil
}

// normalizeCallArguments is the one shared Normalize body.
func normalizeCallArguments(call model.ToolCall, normalize func(map[string]any) (map[string]any, error)) (json.RawMessage, error) {
	args, err := toolargs.Decode(call.Arguments)
	if err != nil {
		return nil, err
	}
	normalized, err := normalize(args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// Native returns the Harness-scoped native whose single export is the task
// tool and whose one declared binding is the child-launch host service.
func Native() extension.Native {
	childLaunch, _ := extension.HostCapability(extension.ContractChildLaunch)
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           pluginID,
			Version:      "1",
			ConfigSchema: []byte(configSchema),
			Components: []extension.Component{{
				ID:       pluginID,
				Lifetime: extension.LifetimeHarness,
				Provides: []string{toolID},
				Requires: []extension.Binding{{
					Name:         "child_launch",
					Provider:     extension.HostProvider,
					CapabilityID: extension.ContractChildLaunch,
					Definition:   childLaunch,
				}},
			}},
			Capabilities: []extension.Capability{extension.ToolCapability(toolID)},
		},
		Describe: map[string]extension.ToolDescribe{toolID: taskTool{}.describe},
		Open:     open,
	}
}

func open(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
	if err := ctx.Err(); err != nil {
		return extension.Instance{}, err
	}
	s, err := decodeSettings(instance.Section)
	if err != nil {
		return extension.Instance{}, err
	}
	return extension.Instance{Capabilities: map[string]extension.Implementation{
		toolID: {Tool: taskTool{settings: s}},
	}}, nil
}
