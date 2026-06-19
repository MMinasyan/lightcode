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
	paths := applyPatchFilesAtRoot(dir, params)
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
	_, perPath, agg := applyPatchPermissionDecision(allowAll, dir, params)
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
	_, perPath, agg := applyPatchPermissionDecision(check, dir, params)
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
	_, _, agg := applyPatchPermissionDecision(check, dir, params)
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
	_, _, agg := applyPatchPermissionDecision(check, dir, params)
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
	paths, perPath, agg := applyPatchPermissionDecision(check, dir, params)
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
	paths, _, agg := applyPatchPermissionDecision(nil, dir, map[string]any{"input": "garbage"})
	if agg != permission.DecisionAsk {
		t.Fatalf("agg = %v, want Ask for malformed patch", agg)
	}
	if paths != nil {
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

func TestApplyPatchStagedAllAllowNoAsk(t *testing.T) {
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Allow: []string{ruleFor(dir, "**")}}),
		func(_ context.Context, req permission.Request) permission.ResponseAction {
			t.Errorf("ask called unexpectedly: %+v", req)
			return permission.ResponseDeny
		})

	input := applyPatchInput(t, "*** Add File: a.txt\n+hi\n*** Add File: b.txt\n+there")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{
			ToolName:   "apply_patch",
			ToolCallID: "1",
			Args:       input,
			Params:     map[string]any{"input": input},
		},
	})
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	// The apply dispatch lands in commit 5; at this stage the staged
	// branch isn't wired, so the executor will return an error from
	// the per-file grouping path. The key point: the permission step
	// did not call ask (all-allow).
}

func TestApplyPatchStagedAnyDenyNoAsk(t *testing.T) {
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{
			Allow: []string{ruleFor(dir, "a.txt")},
			Deny:  []string{ruleFor(dir, "b.txt")},
		}),
		func(_ context.Context, req permission.Request) permission.ResponseAction {
			t.Errorf("ask called unexpectedly: %+v", req)
			return permission.ResponseDeny
		})

	input := applyPatchInput(t, "*** Add File: a.txt\n+hi\n*** Add File: b.txt\n+there")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{
			ToolName:   "apply_patch",
			ToolCallID: "1",
			Args:       input,
			Params:     map[string]any{"input": input},
		},
	})
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Error != "denied by user" {
		t.Fatalf("results[0].Error = %q, want %q (any-deny returns immediately, no ask)", results[0].Error, "denied by user")
	}
}

func TestApplyPatchStagedPermissionAsksWithPatchFiles(t *testing.T) {
	// Commit 4 only wires the staged permission loop to route
	// apply_patch through the multi-path helper; the apply dispatch
	// branch lands in commit 5. This test verifies the ask request
	// is built correctly (BatchFiles = patch's files, Arg/ResolvedArg
	// = first path) by intercepting the ask and returning Deny so the
	// executor never reaches the apply engine.
	dir := t.TempDir()
	store := &applyPatchStore{turn: 1}
	tracker := NewFileTracker()
	executor := NewStagedExecutorAtRoot(store, tracker, config.ToolsConfig{}, dir,
		rulesCheck(dir, permission.Rules{Ask: []string{ruleFor(dir, "**")}}),
		func(_ context.Context, req permission.Request) permission.ResponseAction {
			if req.ToolName != "apply_patch" {
				return permission.ResponseDeny
			}
			if len(req.BatchFiles) != 2 {
				t.Errorf("ask BatchFiles len = %d, want 2: %v", len(req.BatchFiles), req.BatchFiles)
			}
			if !strings.HasSuffix(req.BatchFiles[0], "a.txt") || !strings.HasSuffix(req.BatchFiles[1], "b.txt") {
				t.Errorf("ask BatchFiles = %v, want [a.txt b.txt]", req.BatchFiles)
			}
			if req.Arg != req.BatchFiles[0] {
				t.Errorf("ask Arg = %q, want first BatchFiles entry %q", req.Arg, req.BatchFiles[0])
			}
			if req.ResolvedArg != req.Arg {
				t.Errorf("ask ResolvedArg = %q, want %q", req.ResolvedArg, req.Arg)
			}
			return permission.ResponseDeny
		})

	input := applyPatchInput(t, "*** Add File: a.txt\n+hi\n*** Add File: b.txt\n+there")
	results := executor.ExecutePending(context.Background(), []StagedCall{
		{
			ToolName:   "apply_patch",
			ToolCallID: "1",
			Args:       input,
			Params:     map[string]any{"input": input},
		},
	})
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Error != "denied by user" {
		t.Fatalf("results[0].Error = %q, want %q (deny intercepted before apply dispatch)", results[0].Error, "denied by user")
	}
}
