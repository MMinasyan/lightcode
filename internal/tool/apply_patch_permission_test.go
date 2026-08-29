package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
)

// ruleFor builds an apply_patch rule in the project-relative form used by
// the runtime's permission checker: the leading "/" in the pattern is
// resolved against projectRoot (= the workspace root in our tests).
func ruleFor(root, rel string) string {
	return "apply_patch(/" + rel + ")"
}

func TestApplyPatchFilesAtRootListsAllTouchedPaths(t *testing.T) {
	dir := t.TempDir()
	params := map[string]any{
		"input": "*** Begin Patch\n*** Add File: a.txt\n+x\n*** Update File: b.txt\n@@\n-x\n+y\n*** Delete File: c.txt\n*** Update File: d.txt\n*** Move to: e.txt\n@@\n-x\n+y\n*** End Patch",
	}
	// Exercise the live helper: the shared target plan that PermWrapped.Execute
	// uses for apply_patch permission checks, with the same display-file
	// projection the affected-file lists are built from.
	targets, _, _, err := applyPatchPermissionPlanWithOptions(nil, dir, params, CapabilityOptions{})
	if err != nil {
		t.Fatalf("applyPatchPermissionPlanWithOptions err = %v", err)
	}
	paths := applyPatchDisplayFiles(targets)
	want := []string{
		filepath.Join(dir, "a.txt"),
		filepath.Join(dir, "b.txt"),
		filepath.Join(dir, "c.txt"),
		filepath.Join(dir, "d.txt"),
		filepath.Join(dir, "e.txt"),
	}
	if len(paths) != len(want) {
		t.Fatalf("paths len = %d, want %d: %v", len(paths), len(want), paths)
	}
	for i, p := range paths {
		if p != want[i] {
			t.Fatalf("paths[%d] = %q, want %q", i, p, want[i])
		}
	}
}

func TestApplyPatchPermissionDecisionAggregate(t *testing.T) {
	dir := t.TempDir()
	allowAll := rulesCheck(dir, permission.Rules{Allow: []string{ruleFor(dir, "**")}})
	params := map[string]any{
		"input": "*** Begin Patch\n*** Add File: a.txt\n+x\n*** Update File: b.txt\n@@\n-x\n+y\n*** End Patch",
	}
	_, perPath, agg, err := applyPatchPermissionPlanWithOptions(allowAll, dir, params, CapabilityOptions{})
	if err != nil {
		t.Fatalf("plan err = %v", err)
	}
	if agg != permission.DecisionAllow {
		t.Fatalf("agg = %v, want Allow", agg)
	}
	if len(perPath) != 2 {
		t.Fatalf("perPath len = %d, want 2", len(perPath))
	}
	for i, d := range perPath {
		if d != permission.DecisionAllow {
			t.Fatalf("perPath[%d] = %v, want Allow", i, d)
		}
	}
}

func TestApplyPatchPermissionDecisionAnyDenyDeniesAll(t *testing.T) {
	dir := t.TempDir()
	check := rulesCheck(dir, permission.Rules{
		Allow: []string{ruleFor(dir, "a.txt")},
		Deny:  []string{ruleFor(dir, "b.txt")},
	})
	params := map[string]any{
		"input": "*** Begin Patch\n*** Add File: a.txt\n+x\n*** Update File: b.txt\n@@\n-x\n+y\n*** End Patch",
	}
	_, perPath, agg, err := applyPatchPermissionPlanWithOptions(check, dir, params, CapabilityOptions{})
	if err != nil {
		t.Fatalf("plan err = %v", err)
	}
	if agg != permission.DecisionDeny {
		t.Fatalf("agg = %v, want Deny (any-deny denies whole patch)", agg)
	}
	if perPath[0] != permission.DecisionAllow {
		t.Fatalf("perPath[0] = %v, want Allow (a.txt is allowed)", perPath[0])
	}
	if perPath[1] != permission.DecisionDeny {
		t.Fatalf("perPath[1] = %v, want Deny (b.txt is denied)", perPath[1])
	}
}

func TestApplyPatchPermissionDecisionAllAllowExceptOneAskIsAsk(t *testing.T) {
	dir := t.TempDir()
	check := rulesCheck(dir, permission.Rules{
		Allow: []string{ruleFor(dir, "a.txt")},
		Ask:   []string{ruleFor(dir, "b.txt")},
	})
	params := map[string]any{
		"input": "*** Begin Patch\n*** Add File: a.txt\n+x\n*** Update File: b.txt\n@@\n-x\n+y\n*** End Patch",
	}
	_, _, agg, err := applyPatchPermissionPlanWithOptions(check, dir, params, CapabilityOptions{})
	if err != nil {
		t.Fatalf("plan err = %v", err)
	}
	if agg != permission.DecisionAsk {
		t.Fatalf("agg = %v, want Ask (one path needs ask, none denied)", agg)
	}
}

// TestApplyPatchPermissionDecisionDenyPreservedThroughLaterAsk guards
// the Deny-sticky invariant: any path denied denies the whole patch even
// if a later path would otherwise Ask. The test orders the patch so
// the denied path comes first, then a sensitive path (which forces
// Ask). Without the Deny-sticky fix, the aggregate would be Ask.
func TestApplyPatchPermissionDecisionDenyPreservedThroughLaterAsk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	check := rulesCheck(dir, permission.Rules{
		Allow: []string{ruleFor(dir, "a.txt")},
		Deny:  []string{ruleFor(dir, "b.txt")},
	})
	params := map[string]any{
		"input": "*** Begin Patch\n*** Add File: b.txt\n+x\n*** Update File: a.txt\n@@\n-x\n+y\n*** Update File: .env\n@@\n-SECRET=1\n+SECRET=2\n*** End Patch",
	}
	_, _, agg, err := applyPatchPermissionPlanWithOptions(check, dir, params, CapabilityOptions{})
	if err != nil {
		t.Fatalf("plan err = %v", err)
	}
	if agg != permission.DecisionDeny {
		t.Fatalf("agg = %v, want Deny (deny must be sticky across later Ask)", agg)
	}
}

func TestApplyPatchPermissionDecisionMoveDestIncluded(t *testing.T) {
	dir := t.TempDir()
	check := rulesCheck(dir, permission.Rules{
		Allow: []string{ruleFor(dir, "old.go")},
		Deny:  []string{ruleFor(dir, "new.go")},
	})
	params := map[string]any{
		"input": "*** Begin Patch\n*** Update File: old.go\n*** Move to: new.go\n@@\n-x\n+y\n*** End Patch",
	}
	targets, perPath, agg, err := applyPatchPermissionPlanWithOptions(check, dir, params, CapabilityOptions{})
	if err != nil {
		t.Fatalf("plan err = %v", err)
	}
	paths := applyPatchDisplayFiles(targets)
	if agg != permission.DecisionDeny {
		t.Fatalf("agg = %v, want Deny (move dest denied)", agg)
	}
	if len(paths) != 2 || paths[1] != filepath.Join(dir, "new.go") {
		t.Fatalf("paths = %v, want [old.go new.go]", paths)
	}
	if perPath[1] != permission.DecisionDeny {
		t.Fatalf("perPath[1] = %v, want Deny (move dest)", perPath[1])
	}
}

func TestApplyPatchPermissionDecisionMalformedPatchIsAsk(t *testing.T) {
	dir := t.TempDir()
	targets, _, agg, err := applyPatchPermissionPlanWithOptions(nil, dir, map[string]any{"input": "garbage"}, CapabilityOptions{})
	if err == nil {
		t.Fatalf("plan err = nil, want parse error for malformed patch")
	}
	if agg != permission.DecisionAsk {
		t.Fatalf("agg = %v, want Ask for malformed patch", agg)
	}
	if paths := applyPatchDisplayFiles(targets); paths != nil {
		t.Fatalf("paths = %v, want nil for malformed patch", paths)
	}
}

func TestApplyPatchImmediateAllAllowApplies(t *testing.T) {
	dir := t.TempDir()
	check := rulesCheck(dir, permission.Rules{Allow: []string{ruleFor(dir, "**")}})
	asked := 0
	ask := func(_ context.Context, req permission.Request) permission.ResponseAction {
		asked++
		return permission.ResponseDeny
	}
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, dir)
	wrapped := WrapWithPermissionAtRoot(tool, dir, check, ask)

	input := applyPatchInput(t, "*** Add File: a.txt\n+hi\n*** Add File: b.txt\n+there")
	if _, err := wrapped.Execute(context.Background(), map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if asked != 0 {
		t.Fatalf("asked = %d, want 0 (all paths allowed)", asked)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("a.txt not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatalf("b.txt not created: %v", err)
	}
}

func TestApplyPatchImmediateAnyDenyReturnsErrDenied(t *testing.T) {
	dir := t.TempDir()
	check := rulesCheck(dir, permission.Rules{
		Allow: []string{ruleFor(dir, "a.txt")},
		Deny:  []string{ruleFor(dir, "b.txt")},
	})
	asked := 0
	ask := func(_ context.Context, req permission.Request) permission.ResponseAction {
		asked++
		return permission.ResponseAllow
	}
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, dir)
	wrapped := WrapWithPermissionAtRoot(tool, dir, check, ask)

	input := applyPatchInput(t, "*** Add File: a.txt\n+hi\n*** Add File: b.txt\n+there")
	_, err := wrapped.Execute(context.Background(), map[string]any{"input": input})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("Execute err = %v, want ErrDenied", err)
	}
	if asked != 0 {
		t.Fatalf("asked = %d, want 0 (any-deny returns immediately, no ask)", asked)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("a.txt exists despite deny: stat err = %v", err)
	}
}

func TestApplyPatchImmediateMalformedInputReturnsBeforePermission(t *testing.T) {
	dir := t.TempDir()
	asked := 0
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, dir)
	wrapped := WrapWithPermissionAtRoot(tool, dir,
		func(string, string) permission.Decision {
			return permission.DecisionAsk
		},
		func(context.Context, permission.Request) permission.ResponseAction {
			asked++
			return permission.ResponseDeny
		},
	)

	_, err := wrapped.Execute(context.Background(), map[string]any{"input": "not a patch"})
	if err == nil || !strings.Contains(err.Error(), "apply_patch:") {
		t.Fatalf("Execute err = %v, want parse error", err)
	}
	if asked != 0 {
		t.Fatalf("permission prompts = %d, want 0", asked)
	}
}

func TestApplyPatchImmediateAskSingleAllowAppliesAll(t *testing.T) {
	dir := t.TempDir()
	check := rulesCheck(dir, permission.Rules{Ask: []string{ruleFor(dir, "**")}})
	asked := 0
	var seenReq permission.Request
	ask := func(_ context.Context, req permission.Request) permission.ResponseAction {
		asked++
		seenReq = req
		return permission.ResponseAllow
	}
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, dir)
	wrapped := WrapWithPermissionAtRoot(tool, dir, check, ask)

	input := applyPatchInput(t, "*** Add File: a.txt\n+hi\n*** Add File: b.txt\n+there")
	if _, err := wrapped.Execute(context.Background(), map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if asked != 1 {
		t.Fatalf("asked = %d, want 1 (one approval covers the whole patch)", asked)
	}
	if seenReq.ToolName != "apply_patch" {
		t.Fatalf("seenReq.ToolName = %q, want apply_patch", seenReq.ToolName)
	}
	if len(seenReq.BatchFiles) != 2 {
		t.Fatalf("seenReq.BatchFiles len = %d, want 2: %v", len(seenReq.BatchFiles), seenReq.BatchFiles)
	}
	if seenReq.Arg != filepath.Join(dir, "a.txt") {
		t.Fatalf("seenReq.Arg = %q, want first touched file", seenReq.Arg)
	}
	if seenReq.ResolvedArg != seenReq.Arg {
		t.Fatalf("seenReq.ResolvedArg = %q, want %q (same as Arg)", seenReq.ResolvedArg, seenReq.Arg)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("a.txt not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatalf("b.txt not created: %v", err)
	}
}

func TestApplyPatchImmediateSensitiveSecondaryForcesAsk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	check := rulesCheck(dir, permission.Rules{Allow: []string{ruleFor(dir, "a.txt")}})
	asked := 0
	ask := func(_ context.Context, req permission.Request) permission.ResponseAction {
		asked++
		return permission.ResponseAllow
	}
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, dir)
	wrapped := WrapWithPermissionAtRoot(tool, dir, check, ask)

	input := applyPatchInput(t, "*** Add File: a.txt\n+hi\n*** Update File: .env\n@@\n-SECRET=1\n+SECRET=2")
	if _, err := wrapped.Execute(context.Background(), map[string]any{"input": input}); err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if asked != 1 {
		t.Fatalf("asked = %d, want 1 (sensitive path forces ask)", asked)
	}
}

func TestApplyPatchImmediateSymlinkApprovalUsesCanonicalReceipt(t *testing.T) {
	dir := t.TempDir()
	real1 := filepath.Join(dir, "real1.txt")
	real2 := filepath.Join(dir, "real2.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(real1, []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real2, []byte("alpha\nBEFORE\nbeta"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real1, link); err != nil {
		t.Fatal(err)
	}
	check := rulesCheck(dir, permission.Rules{Ask: []string{ruleFor(dir, "**")}})
	asked := 0
	ask := func(_ context.Context, req permission.Request) permission.ResponseAction {
		asked++
		if len(req.BatchFiles) != 1 || req.BatchFiles[0] != link {
			t.Fatalf("BatchFiles = %v, want [%s]", req.BatchFiles, link)
		}
		if len(req.BatchResolvedFiles) != 1 || req.BatchResolvedFiles[0] != real1 {
			t.Fatalf("BatchResolvedFiles = %v, want [%s]", req.BatchResolvedFiles, real1)
		}
		if req.ResolvedArg != real1 {
			t.Fatalf("ResolvedArg = %q, want %q", req.ResolvedArg, real1)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real2, link); err != nil {
			t.Fatal(err)
		}
		return permission.ResponseAllow
	}
	tool := NewApplyPatchWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, dir)
	wrapped := WrapWithPermissionAtRoot(tool, dir, check, ask)

	input := applyPatchInput(t, "*** Update File: link.txt\n@@\n alpha\n-BEFORE\n+AFTER\n beta")
	_, err := wrapped.Execute(context.Background(), map[string]any{"input": input})
	if err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("Execute err = %v, want approved canonical path changed", err)
	}
	if asked != 1 {
		t.Fatalf("asked = %d, want 1", asked)
	}
	if got := readFile(t, real1); got != "alpha\nBEFORE\nbeta" {
		t.Fatalf("real1 = %q, want unchanged", got)
	}
	if got := readFile(t, real2); got != "alpha\nBEFORE\nbeta" {
		t.Fatalf("real2 = %q, want unchanged", got)
	}
}

func TestApplyPatchCanonicalCollisionRejectedBeforeApply(t *testing.T) {
	// Two different raw paths that resolve to the same canonical path
	// must be rejected before any mutation. The parser allows them
	// (raw-path dedup catches only exact duplicates); target planning
	// catches the canonical collision.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	tool := NewApplyPatchWithSnapshotAtRoot(store, tracker, config.ToolsConfig{}, dir)
	// "a.txt" and "./a.txt" are different raw paths but resolve to the same canonical.
	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n-hi\n+bye\n*** Update File: ./a.txt\n@@\n-hi\n+there")
	_, err := tool.Execute(context.Background(), map[string]any{"input": input})
	if err == nil || !strings.Contains(err.Error(), "resolve to the same file") {
		t.Fatalf("Execute err = %v, want canonical collision error", err)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "hi" {
		t.Fatalf("a.txt = %q, want unchanged (collision rejected before apply)", got)
	}
	if len(store.calls) != 0 {
		t.Fatalf("snapshot calls = %d, want 0 (collision rejected before apply)", len(store.calls))
	}
}

func TestApplyPatchCanonicalCollisionSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(real, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	tool := NewApplyPatchWithSnapshotAtRoot(store, tracker, config.ToolsConfig{}, dir)
	// Two raw paths resolving to the same canonical via symlink.
	input := applyPatchInput(t, "*** Update File: real.txt\n@@\n content\n+changed\n*** Delete File: link.txt")
	_, err := tool.Execute(context.Background(), map[string]any{"input": input})
	if err == nil || !strings.Contains(err.Error(), "resolve to the same file") {
		t.Fatalf("Execute err = %v, want canonical collision error", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("snapshot calls = %d, want 0 (collision rejected before apply)", len(store.calls))
	}
}

func TestApplyPatchImmediateEditFilePermissionUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	check := rulesCheck(dir, permission.Rules{Deny: []string{"edit_file(" + filepath.Join(dir, "a.txt") + ")"}})
	tool := NewEditFileWithSnapshotAtRoot(&applyPatchStore{turn: 1}, NewFileTracker(), config.ToolsConfig{}, dir)
	wrapped := WrapWithPermissionAtRoot(tool, dir, check, nil)
	_, err := wrapped.Execute(context.Background(), map[string]any{
		"path":       filepath.Join(dir, "a.txt"),
		"old_string": "hi",
		"new_string": "bye",
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("edit_file err = %v, want ErrDenied (regression)", err)
	}
}
