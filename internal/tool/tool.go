// Package tool defines the Tool interface, a registry, and the four
// built-in tools used by the agentic loop: read_file, write_file,
// edit_file, and run_command.
package tool

import (
	"context"
	"errors"

	openai "github.com/sashabaranov/go-openai"
)

// ErrDenied is returned by a tool's Execute when the user explicitly
// denies the operation. The loop stops the turn immediately rather
// than feeding the error back to the model as a tool result.
var ErrDenied = errors.New("denied by user")

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

// Registry holds all registered tools. Registration order is preserved
// so that OpenAITools() emits a stable sequence, which helps with logging
// and debugging across runs.
type Registry struct {
	tools map[string]Tool
	order []string
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

// OpenAITools returns the tool definitions in the shape every OpenAI-
// compatible endpoint expects.
func (r *Registry) OpenAITools() []openai.Tool {
	out := make([]openai.Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		out = append(out, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.ParametersSchema(),
			},
		})
	}
	return out
}
