package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
)

func TestPermWrappedAllowsLiteralRuleMatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "exact.txt")
	inner := &recordingTool{name: "write_file", result: "wrote"}
	wrapped := WrapWithPermission(inner, rulesCheck(root, permission.Rules{
		Allow: []string{"write_file(/exact.txt)"},
	}), denyIfAsked(t))

	result, err := wrapped.Execute(context.Background(), map[string]any{"path": path})

	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "wrote" {
		t.Fatalf("Execute result = %q, want wrote", result)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}
	if inner.lastParams["path"] != path {
		t.Fatalf("inner params path = %v, want %q", inner.lastParams["path"], path)
	}
}

func TestPermWrappedAllowsGlobRuleMatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "src", "pkg", "file.go")
	inner := &recordingTool{name: "read_file", result: "read"}
	wrapped := WrapWithPermission(inner, rulesCheck(root, permission.Rules{
		Allow: []string{"read_file(/src/**/*.go)"},
	}), denyIfAsked(t))

	result, err := wrapped.Execute(context.Background(), map[string]any{"path": path})

	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "read" {
		t.Fatalf("Execute result = %q, want read", result)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}
}

func TestPermWrappedDenyShortCircuitsInnerAndAsk(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "blocked.txt")
	inner := &recordingTool{name: "write_file", result: "should not run"}
	askCalls := 0
	wrapped := WrapWithPermission(inner, rulesCheck(root, permission.Rules{
		Allow: []string{"write_file(/blocked.txt)"},
		Deny:  []string{"write_file(/blocked.txt)"},
	}), func(context.Context, permission.Request) permission.ResponseAction {
		askCalls++
		return permission.ResponseAllow
	})

	result, err := wrapped.Execute(context.Background(), map[string]any{"path": path})

	if !errors.Is(err, ErrDenied) {
		t.Fatalf("Execute error = %v, want ErrDenied", err)
	}
	if result != "" {
		t.Fatalf("Execute result = %q, want empty", result)
	}
	if inner.calls != 0 {
		t.Fatalf("inner calls = %d, want 0", inner.calls)
	}
	if askCalls != 0 {
		t.Fatalf("ask calls = %d, want 0", askCalls)
	}
}

func TestPermWrappedAskAllowsExecution(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ask.txt")
	inner := &recordingTool{name: "edit_file", result: "edited"}
	askCalls := 0
	wrapped := WrapWithPermission(inner, rulesCheck(root, permission.Rules{
		Ask: []string{"edit_file(/ask.txt)"},
	}), func(_ context.Context, req permission.Request) permission.ResponseAction {
		askCalls++
		if req.ToolName != "edit_file" || req.Arg != path {
			t.Fatalf("ask got (%q, %q), want (edit_file, %q)", req.ToolName, req.Arg, path)
		}
		return permission.ResponseAllow
	})

	result, err := wrapped.Execute(context.Background(), map[string]any{"path": path})

	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "edited" {
		t.Fatalf("Execute result = %q, want edited", result)
	}
	if askCalls != 1 {
		t.Fatalf("ask calls = %d, want 1", askCalls)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}
}

func TestPermWrappedAskDenySkipsInner(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ask.txt")
	inner := &recordingTool{name: "edit_file", result: "should not run"}
	askCalls := 0
	wrapped := WrapWithPermission(inner, rulesCheck(root, permission.Rules{
		Ask: []string{"edit_file(/ask.txt)"},
	}), func(context.Context, permission.Request) permission.ResponseAction {
		askCalls++
		return permission.ResponseDeny
	})

	result, err := wrapped.Execute(context.Background(), map[string]any{"path": path})

	if !errors.Is(err, ErrDenied) {
		t.Fatalf("Execute error = %v, want ErrDenied", err)
	}
	if result != "" {
		t.Fatalf("Execute result = %q, want empty", result)
	}
	if askCalls != 1 {
		t.Fatalf("ask calls = %d, want 1", askCalls)
	}
	if inner.calls != 0 {
		t.Fatalf("inner calls = %d, want 0", inner.calls)
	}
}

func TestPermWrappedNoRuleDefaultsToAsk(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "unmatched.txt")
	inner := &recordingTool{name: "read_file", result: "should not run"}
	askCalls := 0
	wrapped := WrapWithPermission(inner, rulesCheck(root, permission.Rules{
		Allow: []string{"read_file(/other.txt)"},
	}), func(_ context.Context, req permission.Request) permission.ResponseAction {
		askCalls++
		if req.ToolName != "read_file" || req.Arg != path {
			t.Fatalf("ask got (%q, %q), want (read_file, %q)", req.ToolName, req.Arg, path)
		}
		return permission.ResponseDeny
	})

	_, err := wrapped.Execute(context.Background(), map[string]any{"path": path})

	if !errors.Is(err, ErrDenied) {
		t.Fatalf("Execute error = %v, want ErrDenied", err)
	}
	if askCalls != 1 {
		t.Fatalf("ask calls = %d, want 1", askCalls)
	}
	if inner.calls != 0 {
		t.Fatalf("inner calls = %d, want 0", inner.calls)
	}
}

func TestPermWrappedChecksCanonicalPathAndPromptsWithResolvedTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, ".env")
	if err := os.WriteFile(target, []byte("SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "notes.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	inner := &recordingTool{name: "read_file", result: "should not run"}
	var checkedArg string
	var request permission.Request
	wrapped := WrapWithPermission(inner, func(toolName, arg string) permission.Decision {
		checkedArg = arg
		return permission.Check(permission.Rules{
			Allow: []string{"read_file(/**)"},
		}, permission.Rules{}, toolName, arg, root, root, root)
	}, func(_ context.Context, req permission.Request) permission.ResponseAction {
		request = req
		return permission.ResponseDeny
	})

	_, err := wrapped.Execute(context.Background(), map[string]any{
		"path":               link,
		canonicalPathParam:   filepath.Join(root, "spoofed.txt"),
		"unrelated_metadata": "kept",
	})

	if !errors.Is(err, ErrDenied) {
		t.Fatalf("Execute error = %v, want ErrDenied", err)
	}
	if checkedArg != target {
		t.Fatalf("check arg = %q, want canonical target %q", checkedArg, target)
	}
	if request.Arg != link {
		t.Fatalf("request arg = %q, want requested path %q", request.Arg, link)
	}
	if request.ResolvedArg != target {
		t.Fatalf("request resolved arg = %q, want canonical target %q", request.ResolvedArg, target)
	}
	if inner.calls != 0 {
		t.Fatalf("inner calls = %d, want 0", inner.calls)
	}
}

func TestPermWrappedInjectsCanonicalPathForInnerFileTool(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	inner := &recordingTool{name: "read_file", result: "read"}
	wrapped := WrapWithPermission(inner, func(toolName, arg string) permission.Decision {
		if arg != target {
			t.Fatalf("check arg = %q, want %q", arg, target)
		}
		return permission.DecisionAllow
	}, denyIfAsked(t))

	_, err := wrapped.Execute(context.Background(), map[string]any{
		"path":               link,
		canonicalPathParam:   filepath.Join(root, "spoofed.txt"),
		"unrelated_metadata": "kept",
	})

	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if inner.lastParams["path"] != link {
		t.Fatalf("inner path = %v, want requested path %q", inner.lastParams["path"], link)
	}
	if got := canonicalPathFromParams(inner.lastParams); got != target {
		t.Fatalf("inner canonical path = %q, want %q", got, target)
	}
	if _, ok := inner.lastParams[canonicalPathParam].(canonicalPathValue); !ok {
		t.Fatalf("inner canonical metadata type = %T, want canonicalPathValue", inner.lastParams[canonicalPathParam])
	}
	if inner.lastParams["unrelated_metadata"] != "kept" {
		t.Fatalf("inner unrelated metadata = %v, want kept", inner.lastParams["unrelated_metadata"])
	}
}

func TestPermWrappedWriteDirConfinementForFileTools(t *testing.T) {
	root := t.TempDir()
	writeDir := t.TempDir()
	inside := filepath.Join(writeDir, "inside.txt")
	outside := filepath.Join(root, "outside.txt")

	for _, name := range []string{"write_file", "edit_file"} {
		t.Run(name+" inside write_dir", func(t *testing.T) {
			checkCalls := 0
			inner := &recordingTool{name: name, result: "ok"}
			wrapped := WrapWithPermissionAtRootWithOptions(inner, root, func(toolName, arg string) permission.Decision {
				checkCalls++
				if toolName != name || arg != inside {
					t.Fatalf("check = (%q, %q), want (%q, %q)", toolName, arg, name, inside)
				}
				return permission.DecisionAllow
			}, denyIfAsked(t), CapabilityOptions{WriteDir: writeDir})

			if _, err := wrapped.Execute(context.Background(), map[string]any{"path": inside}); err != nil {
				t.Fatalf("Execute inside write_dir error = %v", err)
			}
			if checkCalls != 1 || inner.calls != 1 {
				t.Fatalf("checkCalls=%d inner.calls=%d, want 1/1", checkCalls, inner.calls)
			}
		})

		t.Run(name+" outside write_dir", func(t *testing.T) {
			checkCalls := 0
			inner := &recordingTool{name: name, result: "ok"}
			wrapped := WrapWithPermissionAtRootWithOptions(inner, root, func(string, string) permission.Decision {
				checkCalls++
				return permission.DecisionAllow
			}, denyIfAsked(t), CapabilityOptions{WriteDir: writeDir})

			_, err := wrapped.Execute(context.Background(), map[string]any{"path": outside})
			if err == nil || !strings.Contains(err.Error(), "outside write_dir") {
				t.Fatalf("Execute outside write_dir error = %v, want confinement error", err)
			}
			if checkCalls != 0 || inner.calls != 0 {
				t.Fatalf("checkCalls=%d inner.calls=%d, want 0/0 before permission or execution", checkCalls, inner.calls)
			}
		})
	}
}

func TestPermWrappedWriteDirConfinementForApplyPatchTargets(t *testing.T) {
	root := t.TempDir()
	writeDir := t.TempDir()
	inside := filepath.Join(writeDir, "inside.txt")
	outside := filepath.Join(root, "outside.txt")
	insidePatch := "*** Begin Patch\n*** Add File: " + inside + "\n+x\n*** End Patch"
	outsidePatch := "*** Begin Patch\n*** Add File: " + outside + "\n+x\n*** End Patch"

	checkCalls := 0
	inner := &recordingTool{name: "apply_patch", result: "ok"}
	wrapped := WrapWithPermissionAtRootWithOptions(inner, root, func(toolName, arg string) permission.Decision {
		checkCalls++
		if toolName != "apply_patch" || arg != inside {
			t.Fatalf("check = (%q, %q), want apply_patch/%q", toolName, arg, inside)
		}
		return permission.DecisionAllow
	}, denyIfAsked(t), CapabilityOptions{WriteDir: writeDir})

	if _, err := wrapped.Execute(context.Background(), map[string]any{"input": insidePatch}); err != nil {
		t.Fatalf("Execute inside write_dir error = %v", err)
	}
	if checkCalls != 1 || inner.calls != 1 {
		t.Fatalf("checkCalls=%d inner.calls=%d, want 1/1", checkCalls, inner.calls)
	}

	_, err := wrapped.Execute(context.Background(), map[string]any{"input": outsidePatch})
	if err == nil || !strings.Contains(err.Error(), "outside write_dir") {
		t.Fatalf("Execute outside write_dir error = %v, want confinement error", err)
	}
	if checkCalls != 1 || inner.calls != 1 {
		t.Fatalf("outside write_dir reached permission or execution: checkCalls=%d inner.calls=%d", checkCalls, inner.calls)
	}
}

func TestWriteDirConfinementAllowsActualWritesOutsideProject(t *testing.T) {
	root := t.TempDir()
	writeDir := t.TempDir()
	writeTarget := filepath.Join(writeDir, "write.txt")
	patchTarget := filepath.Join(writeDir, "patch.txt")
	opts := CapabilityOptions{WriteDir: writeDir}
	allow := func(string, string) permission.Decision { return permission.DecisionAllow }
	if err := os.WriteFile(writeTarget, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := &recordingSnapshotStore{turn: 1, before: map[string]string{}}
	writeTool := WrapWithPermissionAtRootWithOptions(NewWriteFileWithSnapshotAtRoot(store, nil, config.ToolsConfig{}, root), root, allow, denyIfAsked(t), opts)
	if _, err := writeTool.Execute(context.Background(), map[string]any{"path": writeTarget, "content": "written"}); err != nil {
		t.Fatalf("write_file inside outside-project write_dir error = %v", err)
	}
	if data, err := os.ReadFile(writeTarget); err != nil || string(data) != "written" {
		t.Fatalf("write target content = %q, %v; want written", data, err)
	}

	patchTool := WrapWithPermissionAtRootWithOptions(NewApplyPatchWithSnapshotAtRoot(nil, nil, config.ToolsConfig{}, root), root, allow, denyIfAsked(t), opts)
	input := "*** Begin Patch\n*** Add File: " + patchTarget + "\n+patched\n*** End Patch"
	if _, err := patchTool.Execute(context.Background(), map[string]any{"input": input}); err != nil {
		t.Fatalf("apply_patch inside outside-project write_dir error = %v", err)
	}
	if data, err := os.ReadFile(patchTarget); err != nil || string(data) != "patched" {
		t.Fatalf("patch target content = %q, %v; want patched", data, err)
	}
}

func TestPermWrappedWriteDirConfinementValidatesStagedBeforeQueue(t *testing.T) {
	root := t.TempDir()
	writeDir := t.TempDir()
	outside := filepath.Join(root, "outside.txt")
	registry := NewRegistry()
	registry.Register(WrapWithPermissionAtRootWithOptions(NewWriteFileWithSnapshotAtRoot(nil, nil, config.ToolsConfig{}, root), root, func(string, string) permission.Decision {
		t.Fatal("permission check called during staged validation")
		return permission.DecisionAllow
	}, nil, CapabilityOptions{WriteDir: writeDir}))

	stageable, ok := registry.StageableTool("write_file")
	if !ok {
		t.Fatal("wrapped write_file did not expose staged validation")
	}
	args, err := json.Marshal(map[string]any{"path": outside, "content": "x"})
	if err != nil {
		t.Fatal(err)
	}
	err = stageable.ValidateStaged(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "outside write_dir") {
		t.Fatalf("ValidateStaged outside write_dir error = %v, want confinement error", err)
	}
}

func TestPermWrappedWriteDirConfinementValidatesStagedApplyPatchTargets(t *testing.T) {
	root := t.TempDir()
	writeDir := t.TempDir()
	outside := filepath.Join(root, "outside.txt")
	registry := NewRegistry()
	registry.Register(WrapWithPermissionAtRootWithOptions(NewApplyPatchWithSnapshotAtRoot(nil, nil, config.ToolsConfig{}, root), root, func(string, string) permission.Decision {
		t.Fatal("permission check called during staged validation")
		return permission.DecisionAllow
	}, nil, CapabilityOptions{WriteDir: writeDir}))

	stageable, ok := registry.StageableTool("apply_patch")
	if !ok {
		t.Fatal("wrapped apply_patch did not expose staged validation")
	}
	args, err := json.Marshal(map[string]any{"input": "*** Begin Patch\n*** Add File: " + outside + "\n+x\n*** End Patch"})
	if err != nil {
		t.Fatal(err)
	}
	err = stageable.ValidateStaged(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "outside write_dir") {
		t.Fatalf("ValidateStaged outside write_dir error = %v, want confinement error", err)
	}
}

func TestWriteDirConfinementCanonicalBypassMatrix(t *testing.T) {
	root := t.TempDir()
	writeDir := filepath.Join(root, "write")
	outsideDir := filepath.Join(root, "outside")
	if err := os.MkdirAll(writeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkOut := filepath.Join(writeDir, "link-out")
	if err := os.Symlink(outsideDir, linkOut); err != nil {
		t.Fatal(err)
	}

	pathSep := string(os.PathSeparator)
	dotdotEscape := writeDir + pathSep + ".." + pathSep + "outside" + pathSep + "dotdot.txt"
	cases := []struct {
		name      string
		path      string
		wantAllow bool
	}{
		{name: "nearest allowed sibling inside write_dir", path: filepath.Join(writeDir, "allowed.txt"), wantAllow: true},
		{name: "absolute outside target", path: filepath.Join(outsideDir, "absolute.txt")},
		{name: "dotdot escape outside write_dir", path: dotdotEscape},
		{name: "symlink escape outside write_dir", path: filepath.Join(linkOut, "symlink.txt")},
	}

	for _, toolName := range []string{"write_file", "edit_file", "apply_patch"} {
		for _, tc := range cases {
			t.Run(toolName+" immediate "+tc.name, func(t *testing.T) {
				checkCalls := 0
				inner := &recordingTool{name: toolName, result: "ok"}
				wrapped := WrapWithPermissionAtRootWithOptions(inner, root, func(name, arg string) permission.Decision {
					checkCalls++
					if name != toolName {
						t.Fatalf("check tool = %q, want %q", name, toolName)
					}
					return permission.DecisionAllow
				}, denyIfAsked(t), CapabilityOptions{WriteDir: writeDir})

				_, err := wrapped.Execute(context.Background(), immediateWriteDirParams(toolName, tc.path))
				if tc.wantAllow {
					if err != nil {
						t.Fatalf("Execute inside write_dir error = %v", err)
					}
					if checkCalls != 1 || inner.calls != 1 {
						t.Fatalf("checkCalls=%d inner.calls=%d, want 1/1", checkCalls, inner.calls)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "outside write_dir") {
					t.Fatalf("Execute escape error = %v, want confinement error", err)
				}
				if checkCalls != 0 || inner.calls != 0 {
					t.Fatalf("escape reached permission or execution: checkCalls=%d inner.calls=%d", checkCalls, inner.calls)
				}
			})

			t.Run(toolName+" staged "+tc.name, func(t *testing.T) {
				registry := NewRegistry()
				registry.Register(WrapWithPermissionAtRootWithOptions(stageableWriteDirTool(toolName, root), root, func(string, string) permission.Decision {
					t.Fatal("permission check called during staged validation")
					return permission.DecisionAllow
				}, nil, CapabilityOptions{WriteDir: writeDir}))
				stageable, ok := registry.StageableTool(toolName)
				if !ok {
					t.Fatalf("%s did not expose staged validation", toolName)
				}
				args, err := json.Marshal(stagedWriteDirParams(toolName, tc.path))
				if err != nil {
					t.Fatal(err)
				}
				err = stageable.ValidateStaged(context.Background(), args)
				if tc.wantAllow {
					if err != nil {
						t.Fatalf("ValidateStaged inside write_dir error = %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "outside write_dir") {
					t.Fatalf("ValidateStaged escape error = %v, want confinement error", err)
				}
			})
		}
	}
}

func TestModelSuppliedCanonicalPathParamIsIgnored(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "real.txt")
	if err := os.WriteFile(realPath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	spoofedPath := filepath.Join(root, "spoofed.txt")
	params := map[string]any{
		"path":             realPath,
		canonicalPathParam: spoofedPath,
	}

	if got := PermissionCheckArg("read_file", params); got != realPath {
		t.Fatalf("PermissionCheckArg = %q, want real path %q", got, realPath)
	}
	got, err := fileSecurityPath(params, realPath)
	if err != nil {
		t.Fatalf("fileSecurityPath error = %v", err)
	}
	if got != realPath {
		t.Fatalf("fileSecurityPath = %q, want real path %q", got, realPath)
	}
}

func TestPermWrappedDelegatesMetadata(t *testing.T) {
	schema := map[string]any{"type": "object"}
	inner := &recordingTool{
		name:        "read_file",
		description: "read description",
		schema:      schema,
	}
	wrapped := WrapWithPermission(inner, nil, nil)

	if wrapped.Name() != "read_file" {
		t.Fatalf("Name = %q, want read_file", wrapped.Name())
	}
	if wrapped.Description() != "read description" {
		t.Fatalf("Description = %q, want read description", wrapped.Description())
	}
	if got := wrapped.ParametersSchema(); got["type"] != "object" {
		t.Fatalf("ParametersSchema = %#v, want delegated schema", got)
	}
}

func TestPermissionArgExtractsPermissionRelevantArguments(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "file.txt")
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		toolName string
		params   map[string]any
		want     string
	}{
		{name: "run command", toolName: "run_command", params: map[string]any{"command": "go test ./..."}, want: "go test ./..."},
		{name: "read file path", toolName: "read_file", params: map[string]any{"path": filePath}, want: absPath},
		{name: "write file path", toolName: "write_file", params: map[string]any{"path": filePath}, want: absPath},
		{name: "edit file path", toolName: "edit_file", params: map[string]any{"path": filePath}, want: absPath},
		{name: "process id", toolName: "process", params: map[string]any{"id": "abc123"}, want: "process:abc123"},
		{name: "process list", toolName: "process", params: map[string]any{}, want: "process"},
		{name: "unknown", toolName: "unknown", params: map[string]any{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PermissionArg(tt.toolName, tt.params); got != tt.want {
				t.Fatalf("PermissionArg = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRegistryCapabilitiesUnwrapPermissionWrapper(t *testing.T) {
	registry := NewRegistry()
	allow := func(string, string) permission.Decision { return permission.DecisionAllow }
	registry.Register(WrapWithPermission(Sleep{}, allow, nil))
	registry.Register(WrapWithPermission(NewEditFile(nil, config.ToolsConfig{}), allow, nil))
	registry.Register(WrapWithPermission(NewWriteFile(nil, config.ToolsConfig{}), allow, nil))

	normalizer, ok := registry.ArgumentNormalizer("sleep")
	if !ok {
		t.Fatal("wrapped sleep did not expose ArgumentNormalizer")
	}
	normalized, err := normalizer.NormalizeArguments(json.RawMessage(`{"seconds":0}`))
	if err != nil {
		t.Fatalf("NormalizeArguments: %v", err)
	}
	if string(normalized) != `{"seconds":1}` {
		t.Fatalf("normalized sleep args = %s, want seconds clamped", normalized)
	}

	if _, ok := registry.StageableTool("edit_file"); !ok {
		t.Fatal("wrapped edit_file did not expose StageableTool")
	}
	if _, ok := registry.DisplayMetadataProvider("edit_file"); !ok {
		t.Fatal("wrapped edit_file did not expose DisplayMetadataProvider")
	}
	if _, ok := registry.StageableTool("write_file"); !ok {
		t.Fatal("wrapped write_file did not expose StageableTool")
	}
}

type recordingTool struct {
	name        string
	description string
	schema      map[string]any
	result      string
	err         error
	calls       int
	lastParams  map[string]any
}

func (t *recordingTool) Name() string { return t.name }

func (t *recordingTool) Description() string { return t.description }

func (t *recordingTool) ParametersSchema() map[string]any {
	if t.schema != nil {
		return t.schema
	}
	return map[string]any{"type": "object"}
}

func (t *recordingTool) Execute(_ context.Context, params map[string]any) (string, error) {
	t.calls++
	t.lastParams = params
	return t.result, t.err
}

func immediateWriteDirParams(toolName, path string) map[string]any {
	if toolName == "apply_patch" {
		return map[string]any{"input": addFilePatch(path)}
	}
	return map[string]any{"path": path}
}

func stagedWriteDirParams(toolName, path string) map[string]any {
	switch toolName {
	case "write_file":
		return map[string]any{"path": path, "content": "x"}
	case "edit_file":
		return map[string]any{"path": path, "old_string": "old", "new_string": "new"}
	case "apply_patch":
		return map[string]any{"input": addFilePatch(path)}
	default:
		return map[string]any{"path": path}
	}
}

func addFilePatch(path string) string {
	return "*** Begin Patch\n*** Add File: " + path + "\n+x\n*** End Patch"
}

func stageableWriteDirTool(toolName, root string) Tool {
	switch toolName {
	case "write_file":
		return NewWriteFileWithSnapshotAtRoot(nil, nil, config.ToolsConfig{}, root)
	case "edit_file":
		return NewEditFileWithSnapshotAtRoot(nil, nil, config.ToolsConfig{}, root)
	case "apply_patch":
		return NewApplyPatchWithSnapshotAtRoot(nil, nil, config.ToolsConfig{}, root)
	default:
		return &recordingTool{name: toolName}
	}
}

func rulesCheck(root string, rules permission.Rules) CheckFunc {
	return func(toolName, arg string) permission.Decision {
		return permission.Check(rules, permission.Rules{}, toolName, arg, root, root, root)
	}
}

func denyIfAsked(t *testing.T) AskFunc {
	t.Helper()
	return func(_ context.Context, req permission.Request) permission.ResponseAction {
		t.Fatalf("ask called for %s %s", req.ToolName, req.Arg)
		return permission.ResponseDeny
	}
}

// Every fileSecurityPathAtRoot call site in write_file.go and edit_file.go must
// be preceded within 3 lines by a comment containing "re-resolve canonical"
// so the defense-in-depth intent is documented at each call site and a
// future refactor cannot silently elide the re-resolution.
func TestPR11Closure_FileSecurityPathDoubleValidationCommented(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	totalSites := 0
	for _, src := range []string{"write_file.go", "edit_file.go"} {
		path := filepath.Join(dir, src)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(data), "\n")
		var callSites []int
		for i, line := range lines {
			if strings.Contains(line, "fileSecurityPathAtRoot(") {
				callSites = append(callSites, i)
			}
		}
		if len(callSites) != 2 {
			t.Errorf("%s: expected 2 fileSecurityPath call sites, got %d", src, len(callSites))
			continue
		}
		totalSites += len(callSites)
		for _, idx := range callSites {
			commented := false
			for j := idx - 3; j < idx && j >= 0; j++ {
				if strings.Contains(lines[j], "re-resolve canonical") {
					commented = true
					break
				}
			}
			if !commented {
				t.Errorf("fileSecurityPath at %s:%d lacks // re-resolve canonical comment in preceding 3 lines: %q",
					src, idx+1, strings.TrimSpace(lines[idx]))
			}
		}
	}
	if totalSites != 4 {
		t.Fatalf("expected 4 fileSecurityPath call sites total across write_file.go + edit_file.go, got %d", totalSites)
	}
}
