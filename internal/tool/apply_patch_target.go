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

// applyPatchReceiptValue is the harness-internal approval receipt carried
// from authorization into execution: the approved canonical targets, the
// patch parsed exactly once during authorization preparation, and the
// write-dir witness with its canonical boundary that the execution closure
// revalidates.
type applyPatchReceiptValue struct {
	targets           []applyPatchTarget
	parsed            *patch
	writeDir          string
	writeDirCanonical string
	rootCanonical     string
}

func (applyPatchReceiptValue) String() string { return "<internal apply_patch approval>" }

func (applyPatchReceiptValue) GoString() string { return "applyPatchReceiptValue{}" }

func resolveApplyPatchTargetsWithOptions(root string, params map[string]any, opts CapabilityOptions) (*patch, []applyPatchTarget, error) {
	input, _ := params["input"].(string)
	p, err := parsePatch(input)
	if err != nil {
		return nil, nil, err
	}
	targets, err := resolveApplyPatchTargetsFromParsed(root, p, opts)
	if err != nil {
		return nil, nil, err
	}
	return p, targets, nil
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

// withApplyPatchReceipt injects the internal approval receipt: the approved
// canonical targets, the patch parsed exactly once during authorization
// preparation, and the write-dir witness with its canonical boundary.
// Execution revalidates its fresh canonical resolution against the approved
// list; no private receipt can survive into executable authority from
// model-supplied arguments because normalization strips `_lightcode_`
// fields and the receipt value is only satisfiable by this internal type.
func withApplyPatchReceipt(params map[string]any, targets []applyPatchTarget, parsed *patch, writeDir, writeDirCanonical, rootCanonical string) map[string]any {
	next := withoutApplyPatchReceipt(params)
	copied := make([]applyPatchTarget, len(targets))
	copy(copied, targets)
	next[applyPatchReceiptParam] = applyPatchReceiptValue{targets: copied, parsed: parsed, writeDir: writeDir, writeDirCanonical: writeDirCanonical, rootCanonical: rootCanonical}
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
