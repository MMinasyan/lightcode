package runtime_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

// TestToolsPluginConsumesConfiguredValues proves the published
// plugins.tools values change tool behavior through the real
// Invocation.Config channel: the configured read_max_lines is the
// normalization default limit and bounds the executed read window
// end-to-end, while the zero Invocation keeps the shipped defaults. The
// configured Invocation comes from the test-only runtime bridge, because
// outside the runtime package only the zero Invocation is constructible.
func TestToolsPluginConsumesConfiguredValues(t *testing.T) {
	ctx := context.Background()
	byID, _ := openToolsComposed(t, t.TempDir(), nil, fakeJobsPlugin(&fakeJobs{}), runtime.Plugin{})
	value, ok := byID["read_file"]
	if !ok {
		t.Fatal("plugin instance exports no read_file")
	}
	readTool := value
	call := func(t *testing.T, args string) model.ToolCall {
		t.Helper()
		completed, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(args)})
		if err != nil {
			t.Fatalf("NewToolCall: %v", err)
		}
		return completed
	}

	configured, err := runtime.ConfiguredInvocationForTest(map[string]string{"tools": `{"read_max_lines":77}`})
	if err != nil {
		t.Fatalf("ConfiguredInvocationForTest: %v", err)
	}
	raw, err := readTool.Normalize(runtime.ToolContext{Invocation: configured}, call(t, `{"path":"a.txt"}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if string(raw) != `{"limit":77,"offset":1,"path":"a.txt"}` {
		t.Fatalf("configured normalization = %s, want the configured default limit 77", raw)
	}

	// The zero Invocation keeps the shipped default: the channel, not the
	// decoder, carries the configured value.
	raw, err = readTool.Normalize(runtime.ToolContext{}, call(t, `{"path":"a.txt"}`))
	if err != nil {
		t.Fatalf("zero-Invocation Normalize: %v", err)
	}
	if string(raw) != `{"limit":500,"offset":1,"path":"a.txt"}` {
		t.Fatalf("zero-Invocation normalization = %s, want the shipped default limit 500", raw)
	}

	// End to end through the real pipeline: Normalize commits the
	// configured default limit, and Prepare consumes those committed bytes —
	// the configured limit bounds the executed read window.
	ws := t.TempDir()
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = strings.Repeat("x", 10)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	tc := runtime.ToolContext{
		Workspace:     ws,
		AdmittedEntry: harness.EntryRef{SessionID: "0123456789abcdef0123456789abcdef", EntryID: "fedcba9876543210fedcba9876543210"},
		Invocation:    configured,
	}
	committed, err := readTool.Normalize(tc, call(t, `{"path":"big.txt"}`))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	plan := readTool.Prepare(ctx, tc, call(t, string(committed)))
	if plan.Immediate != nil || plan.Execute == nil {
		t.Fatalf("plan = %+v, want one executor", plan)
	}
	outcome := plan.Execute(ctx)
	if outcome.Result.Status != model.ResultSuccess {
		t.Fatalf("result = %+v, want success", outcome.Result)
	}
	if !strings.HasPrefix(outcome.Result.Content, "1\txxxxxxxxxx\n") || !strings.Contains(outcome.Result.Content, "\n77\txxxxxxxxxx\n") {
		t.Fatalf("content = %q, want the configured 77-line window", outcome.Result.Content)
	}
	if !strings.HasSuffix(outcome.Result.Content, "(Showing lines 1-77 of 100. Use offset=78 to continue.)") {
		t.Fatalf("content = %q, want the configured-limit pagination footer", outcome.Result.Content)
	}
}
