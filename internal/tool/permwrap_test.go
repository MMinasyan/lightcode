package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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

	_, err := wrapped.Execute(context.Background(), map[string]any{"path": link})

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

	_, err := wrapped.Execute(context.Background(), map[string]any{"path": link})

	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if inner.lastParams["path"] != link {
		t.Fatalf("inner path = %v, want requested path %q", inner.lastParams["path"], link)
	}
	if inner.lastParams[canonicalPathParam] != target {
		t.Fatalf("inner canonical path = %v, want %q", inner.lastParams[canonicalPathParam], target)
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
