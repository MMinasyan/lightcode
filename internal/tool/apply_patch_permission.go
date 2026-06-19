package tool

import "github.com/MMinasyan/lightcode/internal/permission"

// applyPatchFilesAtRoot returns the absolute paths every file op in the
// patch touches: add, update, delete, move source, and move destination.
// apply_patch has no path param, so the affected-file list is derived by
// parsing params["input"]. Used by the multi-path permission check
// (immediate + staged) and by the staged batch prompt to list every
// patched file.
func applyPatchFilesAtRoot(workspaceRoot string, params map[string]any) []string {
	_, targets, err := resolveApplyPatchTargets(workspaceRoot, params)
	if err != nil {
		return nil
	}
	return applyPatchDisplayFiles(targets)
}

// applyPatchPermissionDecision evaluates the rule check for every file
// the patch touches (including Move destinations) and returns the
// per-path decisions plus the aggregate. The aggregate is Deny if any
// path is denied, Allow if all paths are allowed, otherwise Ask. The
// check function is the runtime's per-tool CheckFunc; passing the same
// check function to both the immediate PermWrapped.Execute branch and
// the staged executor's permission loop keeps the two paths identical.
func applyPatchPermissionDecision(check CheckFunc, workspaceRoot string, params map[string]any) (paths []string, perPath []permission.Decision, aggregate permission.Decision) {
	targets, perPath, aggregate, _ := applyPatchPermissionPlan(check, workspaceRoot, params)
	return applyPatchDisplayFiles(targets), perPath, aggregate
}

func applyPatchPermissionPlan(check CheckFunc, workspaceRoot string, params map[string]any) (targets []applyPatchTarget, perPath []permission.Decision, aggregate permission.Decision, err error) {
	_, targets, err = resolveApplyPatchTargets(workspaceRoot, params)
	if err != nil || len(targets) == 0 {
		return nil, nil, permission.DecisionAsk, err
	}
	if check == nil {
		return targets, nil, permission.DecisionAsk, nil
	}
	perPath = make([]permission.Decision, len(targets))
	aggregate = permission.DecisionAllow
	for i, target := range targets {
		d := check("apply_patch", target.CanonicalPath)
		perPath[i] = d
		switch d {
		case permission.DecisionDeny:
			// Deny is sticky: any Deny in any path denies the whole patch
			// regardless of what other paths decide.
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
	return targets, perPath, aggregate, nil
}
