package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/permission"
)

const canonicalPathParam = "_lightcode_canonical_path"

type canonicalPathValue struct {
	path string
}

func (canonicalPathValue) String() string {
	return "<internal canonical path>"
}

func (canonicalPathValue) GoString() string {
	return "canonicalPathValue{}"
}

// CheckFunc evaluates rules for a tool call and returns a Decision.
type CheckFunc func(toolName, arg string) permission.Decision

// AskFunc blocks until the user responds to a permission prompt.
// Returns a response action from the user.
type AskFunc func(ctx context.Context, req permission.Request) permission.ResponseAction

// PermWrapped wraps a Tool with permission enforcement. The wrapped
// tool's Execute is only called if the check allows it or the user
// approves via ask.
type PermWrapped struct {
	inner         Tool
	check         CheckFunc
	ask           AskFunc
	workspaceRoot string
	options       CapabilityOptions
}

// WrapWithPermission wraps t so that every Execute call is gated by
// the check and ask functions.
func WrapWithPermission(t Tool, check CheckFunc, ask AskFunc) *PermWrapped {
	return &PermWrapped{inner: t, check: check, ask: ask}
}

func WrapWithPermissionAtRoot(t Tool, workspaceRoot string, check CheckFunc, ask AskFunc) *PermWrapped {
	return &PermWrapped{inner: t, check: check, ask: ask, workspaceRoot: workspaceRoot}
}

func WrapWithPermissionAtRootWithOptions(t Tool, workspaceRoot string, check CheckFunc, ask AskFunc, opts CapabilityOptions) *PermWrapped {
	return &PermWrapped{inner: t, check: check, ask: ask, workspaceRoot: workspaceRoot, options: opts}
}

func (p *PermWrapped) Name() string                     { return p.inner.Name() }
func (p *PermWrapped) Description() string              { return p.inner.Description() }
func (p *PermWrapped) ParametersSchema() map[string]any { return p.inner.ParametersSchema() }
func (p *PermWrapped) WrappedTool() Tool                { return p.inner }

func (p *PermWrapped) Execute(ctx context.Context, params map[string]any) (string, error) {
	// apply_patch has no path param and one approval covers the whole patch.
	// The shared target plan drives permission checks, prompt fields, and the
	// internal approval receipt used during execution.
	if p.inner.Name() == "apply_patch" {
		targets, _, agg, err := applyPatchPermissionPlanWithOptions(p.check, p.workspaceRoot, params, p.options)
		if err != nil {
			return "", err
		}
		switch agg {
		case permission.DecisionAllow:
			return p.inner.Execute(ctx, withApplyPatchReceipt(params, targets))
		case permission.DecisionDeny:
			return "", ErrDenied
		default:
			if p.ask != nil {
				req := applyPatchAskRequest(targets)
				if permissionAllows(p.ask(ctx, req)) {
					return p.inner.Execute(ctx, withApplyPatchReceipt(params, targets))
				}
			}
			return "", ErrDenied
		}
	}

	execParams, err := resolveFileToolParamsAtRootWithOptions(p.workspaceRoot, p.inner.Name(), params, p.options)
	if err != nil {
		return "", err
	}
	checkArg := PermissionCheckArgAtRoot(p.workspaceRoot, p.inner.Name(), execParams)

	switch p.check(p.inner.Name(), checkArg) {
	case permission.DecisionAllow:
		return p.inner.Execute(ctx, execParams)
	case permission.DecisionDeny:
		return "", ErrDenied
	default: // DecisionAsk
		if p.ask != nil && permissionAllows(p.ask(ctx, permissionRequestAtRoot(p.workspaceRoot, p.inner.Name(), params, execParams))) {
			return p.inner.Execute(ctx, execParams)
		}
		return "", ErrDenied
	}
}

func (p *PermWrapped) StageableTool() (StageableTool, bool) {
	stageable, ok := p.inner.(StageableTool)
	if !ok {
		return nil, false
	}
	if !p.options.hasWriteDir() || !isWriteTool(p.inner.Name()) {
		return stageable, true
	}
	return writeDirStageable{
		inner: stageable,
		name:  p.inner.Name(),
		root:  p.workspaceRoot,
		opts:  p.options,
	}, true
}

type writeDirStageable struct {
	inner StageableTool
	name  string
	root  string
	opts  CapabilityOptions
}

func (s writeDirStageable) ValidateStaged(ctx context.Context, args json.RawMessage) error {
	if err := s.inner.ValidateStaged(ctx, args); err != nil {
		return err
	}
	var params map[string]any
	if err := json.Unmarshal(args, &params); err != nil {
		return fmt.Errorf("%s: invalid staged arguments: %w", s.name, err)
	}
	if s.name == "apply_patch" {
		_, _, err := resolveApplyPatchTargetsWithOptions(s.root, params, s.opts)
		return err
	}
	_, err := resolveFileToolParamsAtRootWithOptions(s.root, s.name, params, s.opts)
	return err
}

func (s writeDirStageable) StagedResultMessage() string {
	return s.inner.StagedResultMessage()
}

// applyPatchAskRequest builds the permission ask for apply_patch: BatchFiles
// lists every touched file; Arg and ResolvedArg are the first touched path
// (the representative path the rule engine sees). The batch-level fields
// (BatchIndex, BatchTotal, CanAllowAll) are set by the staged caller; the
// immediate path leaves them zero, which is correct (single-call).
func applyPatchAskRequest(targets []applyPatchTarget) permission.Request {
	arg := ""
	resolved := ""
	paths := applyPatchDisplayFiles(targets)
	resolvedPaths := applyPatchCanonicalFiles(targets)
	if len(targets) > 0 {
		arg = targets[0].AbsPath
		resolved = targets[0].CanonicalPath
	}
	return permission.Request{
		ToolName:           "apply_patch",
		Arg:                arg,
		ResolvedArg:        resolved,
		BatchFiles:         paths,
		BatchResolvedFiles: resolvedPaths,
		DisableProjectSave: len(paths) > 1,
	}
}

func permissionAllows(action permission.ResponseAction) bool {
	return action == permission.ResponseAllow || action == permission.ResponseAllowAll
}

func permissionRequest(toolName string, params, execParams map[string]any) permission.Request {
	return permissionRequestAtRoot("", toolName, params, execParams)
}

func permissionRequestAtRoot(root, toolName string, params, execParams map[string]any) permission.Request {
	arg := PermissionArgAtRoot(root, toolName, params)
	req := permission.Request{ToolName: toolName, Arg: arg}
	if permission.IsFileTool(toolName) {
		if resolved := PermissionCheckArgAtRoot(root, toolName, execParams); resolved != "" && resolved != arg {
			req.ResolvedArg = resolved
		}
	}
	return req
}

// PermissionArg pulls the permission-relevant argument from the tool params.
// File paths are resolved to absolute so they match against resolved rule patterns.
func PermissionArg(toolName string, params map[string]any) string {
	return PermissionArgAtRoot("", toolName, params)
}

func PermissionArgAtRoot(root, toolName string, params map[string]any) string {
	switch toolName {
	case "run_command":
		s, _ := params["command"].(string)
		return s
	case "read_file", "write_file", "edit_file":
		s, _ := params["path"].(string)
		if s != "" {
			if root != "" && !filepath.IsAbs(s) {
				s = filepath.Join(root, s)
			}
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
	return PermissionCheckArgAtRoot("", toolName, params)
}

func PermissionCheckArgAtRoot(root, toolName string, params map[string]any) string {
	if permission.IsFileTool(toolName) {
		if canonicalPath := canonicalPathFromParams(params); canonicalPath != "" {
			return canonicalPath
		}
		path, _ := params["path"].(string)
		if path != "" {
			if resolved, err := pathutil.ResolveFilePathFrom(root, path); err == nil {
				return resolved.CanonicalPath
			}
		}
	}
	return PermissionArg(toolName, params)
}

func resolveFileToolParams(toolName string, params map[string]any) (map[string]any, error) {
	return resolveFileToolParamsAtRoot("", toolName, params)
}

func resolveFileToolParamsAtRoot(root, toolName string, params map[string]any) (map[string]any, error) {
	return resolveFileToolParamsAtRootWithOptions(root, toolName, params, CapabilityOptions{})
}

func resolveFileToolParamsAtRootWithOptions(root, toolName string, params map[string]any, opts CapabilityOptions) (map[string]any, error) {
	if !permission.IsFileTool(toolName) {
		return params, nil
	}
	cleanParams := withoutCanonicalPathParam(params)
	path, _ := cleanParams["path"].(string)
	if path == "" {
		return cleanParams, nil
	}
	resolved, err := pathutil.ResolveFilePathFrom(root, path)
	if err != nil {
		return nil, err
	}
	if err := checkWriteDirTarget(root, toolName, resolved.CanonicalPath, opts); err != nil {
		return nil, err
	}
	return withCanonicalPathParam(cleanParams, resolved.CanonicalPath), nil
}

func withCanonicalPathParam(params map[string]any, canonicalPath string) map[string]any {
	next := withoutCanonicalPathParam(params)
	next[canonicalPathParam] = canonicalPathValue{path: canonicalPath}
	return next
}

func withoutCanonicalPathParam(params map[string]any) map[string]any {
	next := make(map[string]any, len(params)+1)
	for k, v := range params {
		if k == canonicalPathParam {
			continue
		}
		next[k] = v
	}
	return next
}

func canonicalPathFromParams(params map[string]any) string {
	canonicalPath, _ := params[canonicalPathParam].(canonicalPathValue)
	return canonicalPath.path
}

func fileSecurityPath(params map[string]any, path string) (string, error) {
	return fileSecurityPathAtRoot("", params, path)
}

func fileSecurityPathAtRoot(root string, params map[string]any, path string) (string, error) {
	if canonicalPath := canonicalPathFromParams(params); canonicalPath != "" {
		resolved, err := pathutil.ResolveFilePathFrom(root, path)
		if err != nil {
			return "", err
		}
		if resolved.CanonicalPath != canonicalPath {
			return "", fmt.Errorf("approved canonical path changed from %s to %s", canonicalPath, resolved.CanonicalPath)
		}
		return canonicalPath, nil
	}
	resolved, err := pathutil.ResolveFilePathFrom(root, path)
	if err != nil {
		return "", err
	}
	return resolved.CanonicalPath, nil
}

func fileDisplayAbsPath(path string) (string, error) {
	return fileDisplayAbsPathAtRoot("", path)
}

func fileDisplayAbsPathAtRoot(root, path string) (string, error) {
	if root != "" && !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absPath, nil
}
