package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

// toolType is the declared-type identity of the public Tool contract: a
// Provides declaration whose type is exactly this interface is a tool
// declaration, its ID is the model-visible tool name, and its description
// function is mandatory.
var toolType = reflect.TypeFor[Tool]()

// ToolConstraints carries the selected Agent's hard capability constraints:
// only the readonly/write-dir boundary. Description receives the resolved
// Agent definition's values and execution the values from its validated
// capture; tools supply neither and cannot loosen them.
type ToolConstraints struct {
	Readonly bool
	WriteDir string
}

// ToolDescription is one static tool description: the model-visible
// definition, whether the tool is available under the hard constraints
// (never a permission filter), and whether it stays hidden from the default
// advertisement.
type ToolDescription struct {
	Definition    model.ToolDefinition
	Available     bool
	DefaultHidden bool
}

// BackgroundServices is the background-work surface a prepared call carries:
// child launch, job start, and completion delivery over the Runtime's public
// Harness methods. The Runtime's implementation is the armed backgroundBridge
// injected by the production opener; the zero ToolContext carries none.
type BackgroundServices interface {
	LaunchChild(ctx context.Context, req harness.LaunchChildRequest) (harness.LaunchChildResult, error)
	StartJob(ctx context.Context, sessionID, jobID string, spawn func(ctx context.Context, completionID string) error) error
	DeliverCompletion(ctx context.Context, sessionID, completionID, content string) error
}

// backgroundBridge is the BackgroundServices implementation: a small struct
// holding the Runtime's Harness. It is created before harness.New, threaded
// into the preparation, and armed with the constructed Harness before the
// owner is published. No synchronization: harness.New performs no I/O and the
// first Prepare requires the admission gate, so no caller can observe the
// unarmed bridge.
type backgroundBridge struct {
	h *harness.Harness
}

func (b *backgroundBridge) LaunchChild(ctx context.Context, req harness.LaunchChildRequest) (harness.LaunchChildResult, error) {
	return b.h.LaunchChildSession(ctx, req)
}

func (b *backgroundBridge) StartJob(ctx context.Context, sessionID, jobID string, spawn func(ctx context.Context, completionID string) error) error {
	return b.h.StartJob(ctx, sessionID, jobID, spawn)
}

func (b *backgroundBridge) DeliverCompletion(ctx context.Context, sessionID, completionID, content string) error {
	return b.h.DeliverBackgroundCompletion(ctx, sessionID, completionID, content)
}

// ToolContext carries what one tool call needs beyond the call itself: the
// Workspace, the calling Operation's admitted-input identity, the captured
// Invocation, the selected Agent's hard constraints, and the armed background
// services bridge. It exposes neither the complete admission register nor
// file/command-specific numeric fields.
type ToolContext struct {
	Workspace     string
	AdmittedEntry harness.EntryRef
	Invocation    Invocation
	Constraints   ToolConstraints
	Background    BackgroundServices
}

// Tool is one concrete tool capability: pure terminating argument
// normalization plus preparation of one authorized call. Both members
// receive the same ToolContext; normalization supplies the immutable call
// facts preparation consumes.
type Tool interface {
	Normalize(ToolContext, model.ToolCall) (json.RawMessage, error)
	Prepare(context.Context, ToolContext, model.ToolCall) harness.PreparedTool
}

// ToolSpec is the Provides declaration for the public Tool contract: it
// records the tool ID, the Tool interface type, and the pure description
// function in one CapabilitySpec. Spec[Tool] remains the ordinary Requires
// declaration. The recorded description validates and copies the returned
// definition and enforces the ID/name match when invoked; no execution
// instance is constructed to obtain the declaration's metadata. A nil
// description function records a declaration lacking one, which composition
// rejects.
func ToolSpec(id string, describe func(Invocation, ToolConstraints) (ToolDescription, error)) CapabilitySpec {
	if describe == nil {
		return CapabilitySpec{id: id, typ: toolType}
	}
	return CapabilitySpec{
		id:  id,
		typ: toolType,
		describe: func(invocation Invocation, constraints ToolConstraints) (ToolDescription, error) {
			description, err := describe(invocation, constraints)
			if err != nil {
				return ToolDescription{}, err
			}
			definition, err := model.NewToolDefinition(description.Definition)
			if err != nil {
				return ToolDescription{}, fmt.Errorf("tool %q: %w", id, err)
			}
			if definition.Name != id {
				return ToolDescription{}, fmt.Errorf("tool %q: description declares name %q", id, definition.Name)
			}
			description.Definition = definition
			return description, nil
		},
	}
}
