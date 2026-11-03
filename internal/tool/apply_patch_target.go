package tool

import (
	"fmt"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/pathutil"
)

type applyPatchTargetRole string

const (
	applyPatchTargetAdd        applyPatchTargetRole = "add"
	applyPatchTargetUpdate     applyPatchTargetRole = "update"
	applyPatchTargetDelete     applyPatchTargetRole = "delete"
	applyPatchTargetMoveSource applyPatchTargetRole = "move_source"
	applyPatchTargetMoveDest   applyPatchTargetRole = "move_dest"
)

type applyPatchTarget struct {
	Path          string
	AbsPath       string
	CanonicalPath string
	Role          applyPatchTargetRole
	Destination   bool
}

// resolveApplyPatchTargetsFromParsed resolves every op (and Move
// destination) of an already-parsed patch into canonical targets. This is
// pure path/stat/canonicalization plus the collision and write-dir checks;
// no content is read. The V4A patch is parsed exactly once per call by the
// caller.
func resolveApplyPatchTargetsFromParsed(root string, p *patch, opts CapabilityOptions) ([]applyPatchTarget, error) {
	targets := make([]applyPatchTarget, 0, len(p.ops)*2)
	for _, op := range p.ops {
		role := applyPatchTargetUpdate
		switch op.kind {
		case opAdd:
			role = applyPatchTargetAdd
		case opDelete:
			role = applyPatchTargetDelete
		case opUpdate:
			if op.movePath != "" {
				role = applyPatchTargetMoveSource
			}
		}
		target, err := resolveApplyPatchTarget(root, op.path, role, false)
		if err != nil {
			return nil, err
		}
		if err := checkWriteDirTarget(root, "apply_patch", target.CanonicalPath, opts); err != nil {
			return nil, err
		}
		targets = append(targets, target)
		if op.movePath != "" {
			target, err := resolveApplyPatchTarget(root, op.movePath, applyPatchTargetMoveDest, true)
			if err != nil {
				return nil, err
			}
			if err := checkWriteDirTarget(root, "apply_patch", target.CanonicalPath, opts); err != nil {
				return nil, err
			}
			targets = append(targets, target)
		}
	}
	// Reject canonical-path collisions: two different raw paths (symlink
	// aliases, ./normalization, etc.) that resolve to the same file would
	// cause a deterministic partial apply because the second op's content
	// revalidation would fail after the first op mutated the file.
	seen := map[string]bool{}
	for _, t := range targets {
		key := filepath.Clean(t.CanonicalPath)
		if seen[key] {
			return nil, fmt.Errorf("apply_patch: multiple patch entries resolve to the same file: %s", t.CanonicalPath)
		}
		seen[key] = true
	}
	return targets, nil
}

func resolveApplyPatchTarget(root, path string, role applyPatchTargetRole, destination bool) (applyPatchTarget, error) {
	resolved, err := pathutil.ResolveFilePathFrom(root, path)
	if err != nil {
		return applyPatchTarget{}, fmt.Errorf("apply_patch: %s: %w", path, err)
	}
	return applyPatchTarget{
		Path:          path,
		AbsPath:       resolved.AbsPath,
		CanonicalPath: resolved.CanonicalPath,
		Role:          role,
		Destination:   destination,
	}, nil
}

// compareApplyPatchTargets refuses any change between the approved target
// list bound at authorization and the freshly resolved plan.
func compareApplyPatchTargets(approved, current []applyPatchTarget) error {
	if len(approved) != len(current) {
		return fmt.Errorf("apply_patch: approved target list changed")
	}
	for i := range current {
		bound := filepath.Clean(approved[i].CanonicalPath)
		resolved := filepath.Clean(current[i].CanonicalPath)
		if bound != resolved {
			return fmt.Errorf("apply_patch: approved canonical path changed from %s to %s", bound, resolved)
		}
	}
	return nil
}
