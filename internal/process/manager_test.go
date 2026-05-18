package process

import (
	"strings"
	"testing"
	"time"
)

func TestManagerListRemoveAndIDs(t *testing.T) {
	m := NewManager(2)
	if got := m.List(); got != "No background processes." {
		t.Fatalf("empty List = %q", got)
	}
	id, err := newProcessID()
	if err != nil {
		t.Fatalf("newProcessID: %v", err)
	}
	if len(id) != 8 {
		t.Fatalf("process ID = %q, want 8 hex chars", id)
	}
	m.procs["p1"] = &CommandStarted{ID: "p1", Command: "sleep 1", StartedAt: time.Now()}
	if got := m.List(); !strings.Contains(got, "p1") || !strings.Contains(got, "sleep 1") {
		t.Fatalf("List with process = %q", got)
	}
	m.Remove("p1")
	if len(m.procs) != 0 {
		t.Fatalf("Remove left procs = %+v", m.procs)
	}
}

func TestManagerStartReadExitHandlerAndLimit(t *testing.T) {
	m := NewManager(1)
	exitCh := make(chan int, 1)
	m.SetExitHandler(func(id, command string, exitCode int) { exitCh <- exitCode })
	id, err := m.Start("printf hello; sleep 1", 0)
	if err != nil {
		t.Fatalf("Start printf: %v", err)
	}
	var output string
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		output, err = m.Read(id)
		if err == nil && strings.Contains(output, "hello") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil || !strings.Contains(output, "hello") {
		t.Fatalf("Read started process = %q, %v", output, err)
	}
	if err := m.Kill(id); err != nil {
		t.Fatalf("Kill(%s): %v", id, err)
	}
	select {
	case code := <-exitCh:
		if code == 0 {
			t.Fatalf("exit code = %d after kill, want non-zero", code)
		}
	case <-time.After(time.Second):
		t.Fatal("exit handler not called")
	}

	longID, err := m.Start("sleep 2", 0)
	if err != nil {
		t.Fatalf("Start sleep: %v", err)
	}
	if _, err := m.Start("sleep 2", 0); err == nil {
		t.Fatal("Start beyond process limit error = nil, want error")
	}
	if err := m.Kill(longID); err != nil {
		t.Fatalf("Kill(%s): %v", longID, err)
	}
}

func TestSetExitHandler(t *testing.T) {
	m := NewManager(0)
	called := false
	m.SetExitHandler(func(id, command string, exitCode int) { called = true })
	m.mu.Lock()
	handler := m.onExit
	m.mu.Unlock()
	if handler == nil {
		t.Fatal("SetExitHandler did not store handler")
	}
	handler("id", "cmd", 0)
	if !called {
		t.Fatal("stored handler did not run")
	}
}
