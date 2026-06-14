// Package tool defines the engine-owned tool execution contracts.
package tool

import (
	"context"
	"encoding/json"
	"errors"

	openai "github.com/sashabaranov/go-openai"
)

// ErrDenied is returned by a tool's Execute when the user explicitly
// denies the operation. The engine stops the turn immediately rather
// than feeding the error back to the model as a tool result.
var ErrDenied = errors.New("denied by user")

// ExitError carries model-visible output for a tool failure that should be
// rendered as the tool result body instead of a generic Go error string.
type ExitError struct {
	Output   string
	ExitCode int
}

func (e *ExitError) Error() string { return e.Output }

// Tool is the minimal contract every tool implements.
//
// ParametersSchema returns a map[string]any shaped as a JSON Schema object
// ({"type": "object", "properties": {...}, "required": [...]}). Returning
// a raw map (rather than a typed Go struct) avoids encoding/json serializing
// field names into an unintended schema shape.
//
// Execute receives the JSON-unmarshaled arguments as a map[string]any. Each
// tool extracts its own fields with type assertions. Errors from Execute
// are fed back to the model as tool result content.
type Tool interface {
	Name() string
	Description() string
	ParametersSchema() map[string]any
	Execute(ctx context.Context, params map[string]any) (string, error)
}

// ArgumentNormalizer can rewrite raw model arguments before persistence,
// display, and execution.
type ArgumentNormalizer interface {
	NormalizeArguments(json.RawMessage) (json.RawMessage, error)
}

// DisplayMetadataProvider can derive UI metadata from tool args and result.
type DisplayMetadataProvider interface {
	DisplayMetadata(ctx context.Context, args json.RawMessage, result string) map[string]any
}

// StageableTool can stage a call for later batch execution.
type StageableTool interface {
	ValidateStaged(ctx context.Context, args json.RawMessage) error
	StagedResultMessage() string
}

// ToolCall is the generic identity of a model-requested tool call.
type ToolCall struct {
	Name      string
	ID        string
	Arguments json.RawMessage
}

// Registry holds all registered tools. Registration order is preserved
// so that OpenAITools() emits a stable sequence, which helps with logging
// and debugging across runs.
type Registry struct {
	tools              map[string]Tool
	order              []string
	pendingCoordinator PendingCoordinator
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds a tool, or replaces an existing tool with the same name.
// Order is preserved on first registration only.
func (r *Registry) Register(t Tool) {
	if _, exists := r.tools[t.Name()]; !exists {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// Get returns a tool by name and a bool indicating whether it was found.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) capabilityTool(name string) (Tool, bool) {
	t, ok := r.Get(name)
	if !ok {
		return nil, false
	}
	for {
		wrapped, ok := t.(interface{ WrappedTool() Tool })
		if !ok {
			return t, true
		}
		t = wrapped.WrappedTool()
		if t == nil {
			return nil, false
		}
	}
}

// ArgumentNormalizer returns a registered tool's argument normalizer.
func (r *Registry) ArgumentNormalizer(name string) (ArgumentNormalizer, bool) {
	t, ok := r.capabilityTool(name)
	if !ok {
		return nil, false
	}
	n, ok := t.(ArgumentNormalizer)
	return n, ok
}

// DisplayMetadataProvider returns a registered tool's display metadata provider.
func (r *Registry) DisplayMetadataProvider(name string) (DisplayMetadataProvider, bool) {
	t, ok := r.capabilityTool(name)
	if !ok {
		return nil, false
	}
	p, ok := t.(DisplayMetadataProvider)
	return p, ok
}

// StageableTool returns a registered tool's staging capability.
func (r *Registry) StageableTool(name string) (StageableTool, bool) {
	t, ok := r.capabilityTool(name)
	if !ok {
		return nil, false
	}
	s, ok := t.(StageableTool)
	return s, ok
}

// RegisterPendingCoordinator wires pending-queue behavior for this registry.
func (r *Registry) RegisterPendingCoordinator(c PendingCoordinator) {
	r.pendingCoordinator = c
}

// PendingCoordinator returns the registry-level pending coordinator.
func (r *Registry) PendingCoordinator() (PendingCoordinator, bool) {
	if r == nil || r.pendingCoordinator == nil {
		return nil, false
	}
	return r.pendingCoordinator, true
}

// OpenAITools returns the tool definitions in the shape every OpenAI-
// compatible endpoint expects.
func (r *Registry) OpenAITools() []openai.Tool {
	out := make([]openai.Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, toolDef(r.tools[name]))
	}
	return out
}
