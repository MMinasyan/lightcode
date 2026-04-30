package tool

import (
	"context"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/permission"
)

// CheckFunc evaluates rules for a tool call and returns a Decision.
type CheckFunc func(toolName, arg string) permission.Decision

// AskFunc blocks until the user responds to a permission prompt.
// Returns true for allow, false for deny.
type AskFunc func(toolName, arg string) bool

// PermWrapped wraps a Tool with permission enforcement. The wrapped
// tool's Execute is only called if the check allows it or the user
// approves via ask.
type PermWrapped struct {
	inner Tool
	check CheckFunc
	ask   AskFunc
}

// WrapWithPermission wraps t so that every Execute call is gated by
// the check and ask functions.
func WrapWithPermission(t Tool, check CheckFunc, ask AskFunc) *PermWrapped {
	return &PermWrapped{inner: t, check: check, ask: ask}
}

func (p *PermWrapped) Name() string               { return p.inner.Name() }
func (p *PermWrapped) Description() string         { return p.inner.Description() }
func (p *PermWrapped) ParametersSchema() map[string]any { return p.inner.ParametersSchema() }

func (p *PermWrapped) Execute(ctx context.Context, params map[string]any) (string, error) {
	arg := extractArg(p.inner.Name(), params)

	switch p.check(p.inner.Name(), arg) {
	case permission.DecisionAllow:
		return p.inner.Execute(ctx, params)
	case permission.DecisionDeny:
		return "", ErrDenied
	default: // DecisionAsk
		if p.ask(p.inner.Name(), arg) {
			return p.inner.Execute(ctx, params)
		}
		return "", ErrDenied
	}
}

// extractArg pulls the permission-relevant argument from the tool params.
// File paths are resolved to absolute so they match against resolved rule patterns.
func extractArg(toolName string, params map[string]any) string {
	switch toolName {
	case "run_command":
		s, _ := params["command"].(string)
		return s
	case "read_file", "write_file", "edit_file":
		s, _ := params["path"].(string)
		if s != "" {
			if abs, err := filepath.Abs(s); err == nil {
				return abs
			}
		}
		return s
	default:
		return ""
	}
}
