package tool

import "github.com/MMinasyan/lightcode/internal/permission"

// planApplyPatchPermissions evaluates the check per canonical target and
// aggregates the decisions: Deny is sticky, Ask never promotes to Allow.
func planApplyPatchPermissions(check CheckFunc, targets []applyPatchTarget) permission.Decision {
	if check == nil {
		return permission.DecisionAsk
	}
	aggregate := permission.DecisionAllow
	for _, target := range targets {
		switch check("apply_patch", target.CanonicalPath) {
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
	return aggregate
}
