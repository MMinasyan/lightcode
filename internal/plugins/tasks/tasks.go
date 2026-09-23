// Package tasks declares the child-session task tool: it composes the
// subagent roster description, validates the requested subagent type, and
// launches one child Session through the Runtime's background bridge,
// returning the child session ID immediately. It is an ordinary
// Runtime-scoped composition unit with no cross-plugin capability contract
// and no additional Runtime authority.
package tasks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

const (
	pluginID              = "tasks"
	toolID                = "task"
	permissionChildLaunch = "child.launch"
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

// settings is the plugin's private configuration, decoded with json.Unmarshal
// from the plugins.tasks section. Nil input and missing or null-valued
// members keep the defaults; unknown members are ignored; every consumed
// field is range-checked against the retained bounds.
type settings struct {
	MaxConcurrent  int `json:"max_concurrent"`
	MaxOutputBytes int `json:"max_output_bytes"`
}

func defaultSettings() settings {
	return settings{MaxConcurrent: 4, MaxOutputBytes: 15360}
}

// decodeSettings decodes one supplied plugins.tasks section on top of the
// defaults and enforces the retained ranges. ValidateConfig and per-call
// preparation share it.
func decodeSettings(raw json.RawMessage) (settings, error) {
	s := defaultSettings()
	if len(bytes.TrimSpace(raw)) != 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return settings{}, fmt.Errorf("tasks: invalid settings: %w", err)
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

// taskTool is the task export: unavailable to child Sessions, its
// description composes the Subagent roster from the captured Invocation, and
// preparation validates the call before declaring the one child.launch pair.
type taskTool struct{}

func (taskTool) describe(inv runtime.Invocation, _ runtime.ToolConstraints, identity harness.SessionIdentity) (runtime.ToolDescription, error) {
	var b strings.Builder
	b.WriteString(taskDescriptionPrefix)
	b.WriteString("\n\nAvailable subagent types:\n")
	for _, at := range inv.AgentTypes() {
		if at.Subagent {
			fmt.Fprintf(&b, "- %s: %s\n", at.Name, at.Description)
		}
	}
	return runtime.ToolDescription{
		Definition: model.ToolDefinition{
			Name:        toolID,
			Description: b.String(),
			Parameters:  json.RawMessage(taskParameters),
		},
		Available: identity.ParentSessionID == "",
	}, nil
}

func (taskTool) Normalize(_ runtime.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return normalizeCallArguments(call, normalizeTaskArgs)
}

func (taskTool) Prepare(_ context.Context, tc runtime.ToolContext, call model.ToolCall) harness.PreparedTool {
	s, err := decodeSettings(tc.Invocation.Config(pluginID))
	if err != nil {
		return immediateError(call.ID, err)
	}
	// Preparation accepts only the committed normalized arguments: one
	// strict decode-and-pass, never a second normalization.
	args, err := decodeCallArguments(call.Arguments)
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
	if err := resolveSubagent(subagentType, tc.Invocation.AgentTypes()); err != nil {
		return immediateError(call.ID, fmt.Errorf("unknown subagent type %q: %w", subagentType, err))
	}
	return harness.PreparedTool{
		Permissions: []harness.PermissionRequest{{Permission: permissionChildLaunch, Target: subagentType}},
		Execute: func(ctx context.Context) harness.ToolOutcome {
			operationID, err := newOperationID()
			if err != nil {
				return launchOutcome(call.ID, err)
			}
			result, err := tc.Background.LaunchChild(ctx, harness.LaunchChildRequest{
				ParentSessionID: tc.AdmittedEntry.SessionID,
				AgentType:       subagentType,
				Content:         []model.ContentPart{{Kind: model.PartText, Text: prompt}},
				OperationID:     operationID,
				MaxConcurrent:   s.MaxConcurrent,
				OutputLimit:     s.MaxOutputBytes,
			})
			if err != nil {
				return launchOutcome(call.ID, err)
			}
			return harness.ToolOutcome{Result: model.ToolResult{
				CallID:  call.ID,
				Status:  model.ResultSuccess,
				Content: fmt.Sprintf(taskStartedTemplate, result.ChildSessionID),
			}}
		},
	}
}

// resolveSubagent checks the captured roster for the exactly-named Subagent
// entry: an absent name and a present non-subagent type produce the retained
// inner errors the caller wraps.
func resolveSubagent(name string, roster []harness.AgentType) error {
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

// launchOutcome renders one launch failure — or the operation-ID mint
// failure — through the uniform tool-error path: the harness's rejection
// details are model-facing and arrive unwrapped.
func launchOutcome(callID string, err error) harness.ToolOutcome {
	return harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: err.Error()}}
}

// immediateError is the normalization-class immediate outcome: status error
// carrying the validation diagnostic, per the tool-boundary validation
// contract.
func immediateError(callID string, cause error) harness.PreparedTool {
	return harness.PreparedTool{Immediate: &harness.ToolOutcome{
		Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: cause.Error()},
	}}
}

// newOperationID mints a fresh 32-lowercase-hex operation identity
// (crypto/rand, the newHexID shape).
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

// decodeCallArguments strictly decodes one call's JSON arguments into an
// owned map: UseNumber keeps every numeric lexeme exact, the decode clones
// the call data at the accepting boundary, and malformed, non-object, null
// or trailing data are argument-validation errors.
func decodeCallArguments(raw json.RawMessage) (map[string]any, error) {
	var args map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	if args == nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("arguments must be one JSON object")
	}
	return args, nil
}

// normalizeCallArguments is the one shared Normalize body: the strict decode
// of the call arguments, the tool's single argument normalization, and the
// marshaled normalized object.
func normalizeCallArguments(call model.ToolCall, normalize func(map[string]any) (map[string]any, error)) (json.RawMessage, error) {
	args, err := decodeCallArguments(call.Arguments)
	if err != nil {
		return nil, err
	}
	normalized, err := normalize(args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// Plugin returns the Runtime-scoped plugin whose single export is the task
// tool. ValidateConfig validates the owned plugins.tasks section; Open
// constructs the stateless tool value — settings and the roster arrive per
// call through the Invocation, launches through the ToolContext's background
// bridge.
func Plugin() runtime.Plugin {
	return runtime.Plugin{
		ID:    pluginID,
		Scope: runtime.ScopeRuntime,
		Provides: []runtime.CapabilitySpec{
			runtime.ToolSpec(toolID, taskTool{}.describe),
		},
		ValidateConfig: func(raw json.RawMessage) error {
			_, err := decodeSettings(raw)
			return err
		},
		Open: open,
	}
}

// open checks the scope context and publishes the stateless tool value; no
// per-Session state is created here.
func open(ctx context.Context, _ runtime.ScopeInfo, _ runtime.Bindings) (runtime.Instance, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Instance{}, err
	}
	return runtime.Instance{Values: map[string]any{toolID: taskTool{}}}, nil
}
