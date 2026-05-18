package tool

import (
	"context"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/permission"
)

const canonicalPathParam = "_lightcode_canonical_path"

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

func (p *PermWrapped) Name() string                     { return p.inner.Name() }
func (p *PermWrapped) Description() string              { return p.inner.Description() }
func (p *PermWrapped) ParametersSchema() map[string]any { return p.inner.ParametersSchema() }

func (p *PermWrapped) Execute(ctx context.Context, params map[string]any) (string, error) {
	execParams, err := resolveFileToolParams(p.inner.Name(), params)
	if err != nil {
		return "", err
	}
	arg := PermissionArg(p.inner.Name(), params)
	checkArg := PermissionCheckArg(p.inner.Name(), execParams)

	switch p.check(p.inner.Name(), checkArg) {
	case permission.DecisionAllow:
		return p.inner.Execute(ctx, execParams)
	case permission.DecisionDeny:
		return "", ErrDenied
	default: // DecisionAsk
		if p.ask(p.inner.Name(), arg) {
			return p.inner.Execute(ctx, execParams)
		}
		return "", ErrDenied
	}
}

// PermissionArg pulls the permission-relevant argument from the tool params.
// File paths are resolved to absolute so they match against resolved rule patterns.
func PermissionArg(toolName string, params map[string]any) string {
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
	case "process":
		id, _ := params["id"].(string)
		if id != "" {
			return "process:" + id
		}
		return "process"
	default:
		return ""
	}
}

func PermissionCheckArg(toolName string, params map[string]any) string {
	if isFileTool(toolName) {
		if canonicalPath := canonicalPathFromParams(params); canonicalPath != "" {
			return canonicalPath
		}
		path, _ := params["path"].(string)
		if path != "" {
			if resolved, err := pathutil.ResolveFilePath(path); err == nil {
				return resolved.CanonicalPath
			}
		}
	}
	return PermissionArg(toolName, params)
}

func resolveFileToolParams(toolName string, params map[string]any) (map[string]any, error) {
	if !isFileTool(toolName) {
		return params, nil
	}
	path, _ := params["path"].(string)
	if path == "" {
		return params, nil
	}
	resolved, err := pathutil.ResolveFilePath(path)
	if err != nil {
		return nil, err
	}
	return withCanonicalPathParam(params, resolved.CanonicalPath), nil
}

func withCanonicalPathParam(params map[string]any, canonicalPath string) map[string]any {
	next := make(map[string]any, len(params)+1)
	for k, v := range params {
		next[k] = v
	}
	next[canonicalPathParam] = canonicalPath
	return next
}

func canonicalPathFromParams(params map[string]any) string {
	canonicalPath, _ := params[canonicalPathParam].(string)
	return canonicalPath
}

func fileSecurityPath(params map[string]any, path string) (string, error) {
	if canonicalPath := canonicalPathFromParams(params); canonicalPath != "" {
		return canonicalPath, nil
	}
	resolved, err := pathutil.ResolveFilePath(path)
	if err != nil {
		return "", err
	}
	return resolved.CanonicalPath, nil
}

func fileDisplayAbsPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absPath, nil
}

func isFileTool(toolName string) bool {
	return toolName == "read_file" || toolName == "write_file" || toolName == "edit_file"
}
