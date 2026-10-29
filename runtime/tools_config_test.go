package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// TestConfiguredToolsSettingsTakeEffect proves the published top-level tools
// values change tool behavior through the real captured Invocation channel:
// the configured read_max_lines is the normalization default limit and
// bounds the executed read window end-to-end, while the zero Invocation keeps
// the shipped defaults. The configured Invocation comes from the test-only
// runtime bridge, because outside the runtime package only the zero
// Invocation is constructible.
func TestConfiguredToolsSettingsTakeEffect(t *testing.T) {
	ctx := context.Background()
	byID, _ := openToolsComposed(t, t.TempDir(), &fakeJobs{}, Plugin{})
	value, ok := byID["read_file"]
	testassert.Require(t, ok, "plugin instance exports no read_file")
	readTool := value
	call := func(t *testing.T, args string) model.ToolCall {
		t.Helper()
		completed, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(args)})
		testassert.NoError(t, err)
		return completed
	}

	configured, err := ConfiguredInvocationForTest(`{"read_max_lines":77}`, "", "", nil)
	testassert.NoError(t, err)
	raw, err := readTool.Normalize(ToolContext{Invocation: configured}, call(t, `{"path":"a.txt"}`))
	testassert.NoError(t, err)
	testassert.Require(t, string(raw) == `{"limit":77,"offset":1,"path":"a.txt"}`, "configured normalization = %s, want the configured default limit 77", raw)

	// The zero Invocation keeps the shipped default: the channel, not the
	// decoder, carries the configured value.
	raw, err = readTool.Normalize(ToolContext{}, call(t, `{"path":"a.txt"}`))
	testassert.NoError(t, err)
	testassert.Require(t, string(raw) == `{"limit":500,"offset":1,"path":"a.txt"}`, "zero-Invocation normalization = %s, want the shipped default limit 500", raw)

	// End to end through the real pipeline: Normalize commits the
	// configured default limit, and Prepare consumes those committed bytes —
	// the configured limit bounds the executed read window.
	ws := t.TempDir()
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = strings.Repeat("x", 10)
	}
	testassert.NoError(t, os.WriteFile(filepath.Join(ws, "big.txt"), []byte(strings.Join(lines, "\n")), 0o644))
	tc := ToolContext{
		Workspace:     ws,
		AdmittedEntry: harness.EntryRef{SessionID: "0123456789abcdef0123456789abcdef", EntryID: "fedcba9876543210fedcba9876543210"},
		Invocation:    configured,
	}
	committed, err := readTool.Normalize(tc, call(t, `{"path":"big.txt"}`))
	testassert.NoError(t, err)
	plan := readTool.Prepare(ctx, tc, call(t, string(committed)))
	testassert.Require(t, plan.Immediate == nil && plan.Execute != nil, "plan = %+v, want one executor", plan)
	outcome := plan.Execute(ctx)
	testassert.Require(t, outcome.Result.Status == model.ResultSuccess && strings.HasPrefix(outcome.Result.Content, "1\txxxxxxxxxx\n") && strings.Contains(outcome.Result.Content, "\n77\txxxxxxxxxx\n") && strings.HasSuffix(outcome.Result.Content, "(Showing lines 1-77 of 100. Use offset=78 to continue.)"), "result = %+v, want success; content = %q, want the configured 77-line window; content = %q, want the configured-limit pagination footer", outcome.Result, outcome.Result.Content, outcome.Result.Content)
}
