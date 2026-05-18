package tool

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestProcessToolReadRequiresID(t *testing.T) {
	tool := NewProcessTool(&mockProcessController{}, config.ToolsConfig{}, t.TempDir())

	_, err := tool.Execute(context.Background(), map[string]any{"action": "read"})
	if err == nil || !strings.Contains(err.Error(), "id is required for read") {
		t.Fatalf("Execute error = %v, want read id-required error", err)
	}
}

func TestProcessToolReadReturnsAndTruncatesOutput(t *testing.T) {
	home := t.TempDir()
	mgr := &mockProcessController{readOutput: "abcdefg\n1234567\n"}
	tool := NewProcessTool(mgr, config.ToolsConfig{MaxOutputBytes: 12, ReadLineMaxChars: 5}, home)

	result, err := tool.Execute(context.Background(), map[string]any{"action": "read", "id": "proc-1"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if mgr.readID != "proc-1" {
		t.Fatalf("Read id = %q, want proc-1", mgr.readID)
	}
	if !strings.Contains(result, "abcde... [truncated 7 chars]") {
		t.Fatalf("Execute result = %q, want per-line truncation", result)
	}
	if !strings.Contains(result, "Full output (16 bytes) saved to: "+home+"/.lightcode/proc_output_") {
		t.Fatalf("Execute result = %q, want process spill path", result)
	}
}

func TestProcessToolReadTruncatesManyLinesAndSpillsFullOutput(t *testing.T) {
	home := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 25; i++ {
		b.WriteString("line ")
		b.WriteString(string(rune('A' + i - 1)))
		b.WriteByte('\n')
	}
	fullOutput := b.String()
	mgr := &mockProcessController{readOutput: fullOutput}
	tool := NewProcessTool(mgr, config.ToolsConfig{MaxOutputBytes: 100}, home)

	result, err := tool.Execute(context.Background(), map[string]any{"action": "read", "id": "proc-1"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "line A") || !strings.Contains(result, "line Y") {
		t.Fatalf("Execute result = %q, want first and last lines", result)
	}
	spillPath := extractSpillPath(t, result)
	if !strings.HasPrefix(spillPath, home+"/.lightcode/proc_output_") {
		t.Fatalf("spill path = %q, want process spill path under home %q", spillPath, home)
	}
	data, err := os.ReadFile(spillPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", spillPath, err)
	}
	if string(data) != fullOutput {
		t.Fatalf("spill file content = %q, want full output %q", string(data), fullOutput)
	}
}

func TestProcessToolReadPropagatesManagerError(t *testing.T) {
	mgr := &mockProcessController{readErr: errors.New("missing process")}
	tool := NewProcessTool(mgr, config.ToolsConfig{}, t.TempDir())

	_, err := tool.Execute(context.Background(), map[string]any{"action": "read", "id": "proc-1"})
	if err == nil || !strings.Contains(err.Error(), "missing process") {
		t.Fatalf("Execute error = %v, want manager read error", err)
	}
}

func TestProcessToolKillRequiresIDAndDelegates(t *testing.T) {
	mgr := &mockProcessController{}
	tool := NewProcessTool(mgr, config.ToolsConfig{}, t.TempDir())

	_, err := tool.Execute(context.Background(), map[string]any{"action": "kill"})
	if err == nil || !strings.Contains(err.Error(), "id is required for kill") {
		t.Fatalf("Execute error = %v, want kill id-required error", err)
	}

	result, err := tool.Execute(context.Background(), map[string]any{"action": "kill", "id": "proc-2"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Process proc-2 terminated." {
		t.Fatalf("Execute result = %q, want termination message", result)
	}
	if mgr.killID != "proc-2" {
		t.Fatalf("Kill id = %q, want proc-2", mgr.killID)
	}
}

func TestProcessToolKillPropagatesManagerError(t *testing.T) {
	mgr := &mockProcessController{killErr: errors.New("already exited")}
	tool := NewProcessTool(mgr, config.ToolsConfig{}, t.TempDir())

	_, err := tool.Execute(context.Background(), map[string]any{"action": "kill", "id": "proc-1"})
	if err == nil || !strings.Contains(err.Error(), "already exited") {
		t.Fatalf("Execute error = %v, want manager kill error", err)
	}
}

func TestProcessToolListAndUnknownAction(t *testing.T) {
	mgr := &mockProcessController{listOutput: "proc-1 running"}
	tool := NewProcessTool(mgr, config.ToolsConfig{}, t.TempDir())

	result, err := tool.Execute(context.Background(), map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("list Execute error = %v", err)
	}
	if result != "proc-1 running" || mgr.listCalls != 1 {
		t.Fatalf("list result=%q calls=%d, want manager list output once", result, mgr.listCalls)
	}

	_, err = tool.Execute(context.Background(), map[string]any{"action": "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown action \"bogus\"") {
		t.Fatalf("unknown Execute error = %v, want unknown-action error", err)
	}
}

type mockProcessController struct {
	readID     string
	readOutput string
	readErr    error
	killID     string
	killErr    error
	listOutput string
	listCalls  int
}

func (m *mockProcessController) Read(id string) (string, error) {
	m.readID = id
	return m.readOutput, m.readErr
}

func (m *mockProcessController) Kill(id string) error {
	m.killID = id
	return m.killErr
}

func (m *mockProcessController) List() string {
	m.listCalls++
	return m.listOutput
}

func extractSpillPath(t *testing.T, result string) string {
	t.Helper()
	marker := "saved to: "
	idx := strings.LastIndex(result, marker)
	if idx < 0 {
		t.Fatalf("result = %q, missing spill marker", result)
	}
	path := result[idx+len(marker):]
	if end := strings.IndexAny(path, "]\n"); end >= 0 {
		path = path[:end]
	}
	return strings.TrimSpace(path)
}
