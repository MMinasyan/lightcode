package tool

import (
	"fmt"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/pathutil"
)

const applyPatchReceiptParam = "_lightcode_apply_patch_receipt"

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

type applyPatchReceiptValue struct {
	targets []applyPatchTarget
}

func (applyPatchReceiptValue) String() string { return "<internal apply_patch approval>" }

func (applyPatchReceiptValue) GoString() string { return "applyPatchReceiptValue{}" }

func resolveApplyPatchTargets(root string, params map[string]any) (*patch, []applyPatchTarget, error) {
	return resolveApplyPatchTargetsWithOptions(root, params, CapabilityOptions{})
}

func resolveApplyPatchTargetsWithOptions(root string, params map[string]any, opts CapabilityOptions) (*patch, []applyPatchTarget, error) {
	input, _ := params["input"].(string)
	p, err := parsePatch(input)
	if err != nil {
		return nil, nil, err
	}
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
			return nil, nil, err
		}
		if err := checkWriteDirTarget(root, "apply_patch", target.CanonicalPath, opts); err != nil {
			return nil, nil, err
		}
		targets = append(targets, target)
		if op.movePath != "" {
			target, err := resolveApplyPatchTarget(root, op.movePath, applyPatchTargetMoveDest, true)
			if err != nil {
				return nil, nil, err
			}
			if err := checkWriteDirTarget(root, "apply_patch", target.CanonicalPath, opts); err != nil {
				return nil, nil, err
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
			return nil, nil, fmt.Errorf("apply_patch: multiple patch entries resolve to the same file: %s", t.CanonicalPath)
		}
		seen[key] = true
	}
	return p, targets, nil
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

func applyPatchDisplayFiles(targets []applyPatchTarget) []string {
	if len(targets) == 0 {
		return nil
	}
	files := make([]string, 0, len(targets))
	for _, target := range targets {
		files = append(files, target.AbsPath)
	}
	return files
}

func applyPatchCanonicalFiles(targets []applyPatchTarget) []string {
	if len(targets) == 0 {
		return nil
	}
	files := make([]string, 0, len(targets))
	for _, target := range targets {
		files = append(files, target.CanonicalPath)
	}
	return files
}

func withApplyPatchReceipt(params map[string]any, targets []applyPatchTarget) map[string]any {
	next := withoutApplyPatchReceipt(params)
	copied := make([]applyPatchTarget, len(targets))
	copy(copied, targets)
	next[applyPatchReceiptParam] = applyPatchReceiptValue{targets: copied}
	return next
}

func withoutApplyPatchReceipt(params map[string]any) map[string]any {
	next := make(map[string]any, len(params)+1)
	for k, v := range params {
		if k == applyPatchReceiptParam {
			continue
		}
		next[k] = v
	}
	return next
}

func applyPatchReceiptFromParams(params map[string]any) (applyPatchReceiptValue, bool) {
	receipt, ok := params[applyPatchReceiptParam].(applyPatchReceiptValue)
	return receipt, ok
}

func validateApplyPatchReceipt(root string, params map[string]any) (*patch, []applyPatchTarget, error) {
	p, current, err := resolveApplyPatchTargets(root, params)
	if err != nil {
		return nil, nil, err
	}
	receipt, ok := applyPatchReceiptFromParams(params)
	if !ok {
		return p, current, nil
	}
	if len(receipt.targets) != len(current) {
		return nil, nil, fmt.Errorf("apply_patch: approved target list changed")
	}
	for i := range current {
		approved := filepath.Clean(receipt.targets[i].CanonicalPath)
		resolved := filepath.Clean(current[i].CanonicalPath)
		if approved != resolved {
			return nil, nil, fmt.Errorf("apply_patch: approved canonical path changed from %s to %s", approved, resolved)
		}
	}
	return p, current, nil
}
