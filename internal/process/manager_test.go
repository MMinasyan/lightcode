package process

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/cmdoutput"
)

func TestManagerListRemoveAndIDs(t *testing.T) {
	m := NewManager(2, cmdoutput.Options{})
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
	m := NewManager(1, cmdoutput.Options{})
	exitCh := make(chan ExitEvent, 1)
	m.SetExitHandler(func(event ExitEvent) { exitCh <- event })
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
	case event := <-exitCh:
		if event.ExitCode == 0 {
			t.Fatalf("exit code = %d after kill, want non-zero", event.ExitCode)
		}
		if event.ID != id || event.Command != "printf hello; sleep 1" {
			t.Fatalf("exit event = %+v, want id and command", event)
		}
		if !strings.Contains(event.Output, "hello") {
			t.Fatalf("exit output = %q, want final output", event.Output)
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

func TestManagerExitHandlerReceivesFinalOutput(t *testing.T) {
	m := NewManager(1, cmdoutput.Options{})
	exitCh := make(chan ExitEvent, 1)
	m.SetExitHandler(func(event ExitEvent) { exitCh <- event })
	id, err := m.Start("printf final", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case event := <-exitCh:
		if event.ID != id || event.Command != "printf final" || event.ExitCode != 0 || event.Output != "final" {
			t.Fatalf("exit event = %+v, want id, command, code 0, output final", event)
		}
	case <-time.After(time.Second):
		t.Fatal("exit handler not called")
	}
}

func TestManagerLargeBackgroundOutputSpillsAndReadReusesPath(t *testing.T) {
	home := t.TempDir()
	m := NewManager(1, cmdoutput.Options{
		HomeDir:      home,
		SpillPrefix:  "proc_output_",
		MaxBytes:     100,
		MaxLineChars: 80,
	})
	fullOutput := numberedLines(25)
	id, err := m.Start("cat <<'EOF'\n"+fullOutput+"EOF\nsleep 2", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = m.Kill(id) }()

	first := waitForReadContaining(t, m, id, "saved to:")
	second, err := m.Read(id)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	firstPath := extractSpillPath(t, first)
	secondPath := extractSpillPath(t, second)
	if firstPath != secondPath {
		t.Fatalf("spill path changed: first=%q second=%q", firstPath, secondPath)
	}
	if !strings.HasPrefix(firstPath, filepath.Join(home, ".lightcode", "proc_output_")) {
		t.Fatalf("spill path = %q, want proc_output_ under home", firstPath)
	}
	data, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", firstPath, err)
	}
	if string(data) != fullOutput {
		t.Fatalf("spill content = %q, want full output", string(data))
	}
}

func TestManagerReadAfterExitReturnsMissingProcess(t *testing.T) {
	m := NewManager(1, cmdoutput.Options{})
	exitCh := make(chan ExitEvent, 1)
	m.SetExitHandler(func(event ExitEvent) { exitCh <- event })
	id, err := m.Start("printf done", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-exitCh:
	case <-time.After(time.Second):
		t.Fatal("exit handler not called")
	}
	_, err = m.Read(id)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no process with ID %q", id)) {
		t.Fatalf("Read after exit error = %v, want missing process", err)
	}
}

func TestManagerWithoutExitHandlerDoesNotKeepUnreferencedSpill(t *testing.T) {
	home := t.TempDir()
	m := NewManager(1, cmdoutput.Options{
		HomeDir:      home,
		SpillPrefix:  "proc_output_",
		MaxBytes:     8,
		MaxLineChars: 80,
	})
	id, err := m.Start("printf 'abcdefghijklmnopqrstuvwxyz'", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if _, err := m.Read(id); err != nil && strings.Contains(err.Error(), "no process with ID") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".lightcode"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("ReadDir spill dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "proc_output_") {
			t.Fatalf("unexpected unreferenced spill file kept: %s", entry.Name())
		}
	}
}

func TestManagerEmptyRunningReadMarker(t *testing.T) {
	m := NewManager(1, cmdoutput.Options{})
	id, err := m.Start("sleep 2", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = m.Kill(id) }()
	got, err := m.Read(id)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "(No output yet)" {
		t.Fatalf("Read = %q, want no-output-yet marker", got)
	}
}

func TestManagerConcurrentWaitWritesAndReads(t *testing.T) {
	home := t.TempDir()
	m := NewManager(1, cmdoutput.Options{
		HomeDir:      home,
		SpillPrefix:  "proc_output_",
		MaxBytes:     128,
		MaxLineChars: 80,
	})
	exitCh := make(chan ExitEvent, 1)
	m.SetExitHandler(func(event ExitEvent) { exitCh <- event })
	id, err := m.Start("i=0; while [ $i -lt 100 ]; do echo line-$i; i=$((i+1)); done", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-exitCh:
					return
				default:
				}
				if _, err := m.Read(id); err != nil && strings.Contains(err.Error(), "no process with ID") {
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestSetExitHandler(t *testing.T) {
	m := NewManager(0, cmdoutput.Options{})
	called := false
	m.SetExitHandler(func(event ExitEvent) { called = true })
	m.mu.Lock()
	handler := m.onExit
	m.mu.Unlock()
	if handler == nil {
		t.Fatal("SetExitHandler did not store handler")
	}
	handler(ExitEvent{ID: "id", Command: "cmd", ExitCode: 0})
	if !called {
		t.Fatal("stored handler did not run")
	}
}

func waitForReadContaining(t *testing.T, m *Manager, id, want string) string {
	t.Helper()
	var output string
	var err error
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		output, err = m.Read(id)
		if err == nil && strings.Contains(output, want) {
			return output
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Read(%q) = %q, %v; missing %q", id, output, err, want)
	return ""
}

func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %02d\n", i)
	}
	return b.String()
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
