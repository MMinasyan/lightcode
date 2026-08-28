package tool

import "github.com/MMinasyan/lightcode/internal/permission"

func applyPatchPermissionPlanWithOptions(check CheckFunc, workspaceRoot string, params map[string]any, opts CapabilityOptions) (targets []applyPatchTarget, perPath []permission.Decision, aggregate permission.Decision, err error) {
	_, targets, err = resolveApplyPatchTargetsWithOptions(workspaceRoot, params, opts)
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
