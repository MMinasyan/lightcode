package tool

import (
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/permission"
)

// applyPatchFilesAtRoot returns the absolute paths every file op in the
// patch touches: add, update, delete, move source, and move destination.
// apply_patch has no path param, so the affected-file list is derived by
// parsing params["input"]. Used by the multi-path permission check
// (immediate + staged) and by the staged batch prompt to list every
// patched file.
func applyPatchFilesAtRoot(workspaceRoot string, params map[string]any) []string {
	input, _ := params["input"].(string)
	p, err := parsePatch(input)
	if err != nil {
		return nil
	}
	var out []string
	for _, op := range p.ops {
		out = append(out, resolvePermissionArg(workspaceRoot, op.path))
		if op.movePath != "" {
			out = append(out, resolvePermissionArg(workspaceRoot, op.movePath))
		}
	}
	return out
}

// applyPatchPermissionDecision evaluates the rule check for every file
// the patch touches (including Move destinations) and returns the
// per-path decisions plus the aggregate. The aggregate is Deny if any
// path is denied, Allow if all paths are allowed, otherwise Ask. The
// check function is the runtime's per-tool CheckFunc; passing the same
// check function to both the immediate PermWrapped.Execute branch and
// the staged executor's permission loop is what makes the two paths
// identical (Invariant 3).
func applyPatchPermissionDecision(check CheckFunc, workspaceRoot string, params map[string]any) (paths []string, perPath []permission.Decision, aggregate permission.Decision) {
	paths = applyPatchFilesAtRoot(workspaceRoot, params)
	if len(paths) == 0 {
		// Malformed patch or no ops; let the apply engine surface the error.
		return nil, nil, permission.DecisionAsk
	}
	if check == nil {
		return paths, nil, permission.DecisionAsk
	}
	perPath = make([]permission.Decision, len(paths))
	aggregate = permission.DecisionAllow
	for i, p := range paths {
		d := check("apply_patch", p)
		perPath[i] = d
		switch d {
		case permission.DecisionDeny:
			// Deny is sticky: any Deny in any path denies the whole
			// patch regardless of what other paths decide (Invariant 3).
			aggregate = permission.DecisionDeny
		case permission.DecisionAllow:
			// aggregate stays whatever it was (Allow doesn't promote
			// Ask to Allow).
		default: // Ask
			if aggregate != permission.DecisionDeny {
				aggregate = permission.DecisionAsk
			}
		}
	}
	return paths, perPath, aggregate
}

// resolvePermissionArg resolves a path the way the rest of the file
// tools do: relative paths join workspaceRoot, absolute paths are
// accepted as-is. The check function sees the canonical arg.
func resolvePermissionArg(workspaceRoot, path string) string {
	if workspaceRoot != "" && !filepath.IsAbs(path) {
		path = filepath.Join(workspaceRoot, path)
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
