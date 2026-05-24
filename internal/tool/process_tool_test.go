package tool

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestProcessToolReadRequiresID(t *testing.T) {
	tool := NewProcessTool(&mockProcessController{})

	_, err := tool.Execute(context.Background(), map[string]any{"action": "read"})
	if err == nil || !strings.Contains(err.Error(), "id is required for read") {
		t.Fatalf("Execute error = %v, want read id-required error", err)
	}
}

func TestProcessToolReadDelegatesExactIDAndOutput(t *testing.T) {
	mgr := &mockProcessController{readOutput: "abcdefg\n1234567\n"}
	tool := NewProcessTool(mgr)

	result, err := tool.Execute(context.Background(), map[string]any{"action": "read", "id": "proc-1"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if mgr.readID != "proc-1" {
		t.Fatalf("Read id = %q, want proc-1", mgr.readID)
	}
	if result != "abcdefg\n1234567\n" {
		t.Fatalf("Execute result = %q, want manager output unchanged", result)
	}
}

func TestProcessToolReadPropagatesManagerError(t *testing.T) {
	mgr := &mockProcessController{readErr: errors.New("missing process")}
	tool := NewProcessTool(mgr)

	_, err := tool.Execute(context.Background(), map[string]any{"action": "read", "id": "proc-1"})
	if err == nil || !strings.Contains(err.Error(), "missing process") {
		t.Fatalf("Execute error = %v, want manager read error", err)
	}
}

func TestProcessToolKillRequiresIDAndDelegates(t *testing.T) {
	mgr := &mockProcessController{}
	tool := NewProcessTool(mgr)

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
	tool := NewProcessTool(mgr)

	_, err := tool.Execute(context.Background(), map[string]any{"action": "kill", "id": "proc-1"})
	if err == nil || !strings.Contains(err.Error(), "already exited") {
		t.Fatalf("Execute error = %v, want manager kill error", err)
	}
}

func TestProcessToolListAndUnknownAction(t *testing.T) {
	mgr := &mockProcessController{listOutput: "proc-1 running"}
	tool := NewProcessTool(mgr)

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
