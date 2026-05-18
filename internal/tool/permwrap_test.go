package tool

import (
	"context"
	"errors"
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
	}), func(string, string) bool {
		askCalls++
		return true
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
	}), func(toolName, arg string) bool {
		askCalls++
		if toolName != "edit_file" || arg != path {
			t.Fatalf("ask got (%q, %q), want (edit_file, %q)", toolName, arg, path)
		}
		return true
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
	}), func(string, string) bool {
		askCalls++
		return false
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
	}), func(toolName, arg string) bool {
		askCalls++
		if toolName != "read_file" || arg != path {
			t.Fatalf("ask got (%q, %q), want (read_file, %q)", toolName, arg, path)
		}
		return false
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
	return func(toolName, arg string) bool {
		t.Fatalf("ask called for %s %s", toolName, arg)
		return false
	}
}
