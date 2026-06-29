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

type observedExit struct {
	Event  ExitEvent
	Output string
}

func observeExit(event ExitEvent) observedExit {
	output := ""
	if event.FormatOutput != nil {
		output = event.FormatOutput()
	}
	return observedExit{Event: event, Output: output}
}

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
	m.procs["p2"] = &CommandStarted{ID: "p2", Command: "sleep 2", StartedAt: time.Now()}
	m.procs["done"] = &CommandStarted{ID: "done", Command: "true", StartedAt: time.Now(), exited: true}
	if got := strings.Join(m.ActiveIDs(), ","); got != "p1,p2" {
		t.Fatalf("ActiveIDs = %q, want p1,p2", got)
	}
	m.Remove("p1")
	m.Remove("p2")
	m.Remove("done")
	if len(m.procs) != 0 {
		t.Fatalf("Remove left procs = %+v", m.procs)
	}
}

func TestManagerStartReadExitHandlerAndLimit(t *testing.T) {
	m := NewManager(1, cmdoutput.Options{})
	exitCh := make(chan observedExit, 1)
	m.SetExitHandler(func(event ExitEvent) { exitCh <- observeExit(event) })
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
	case observed := <-exitCh:
		event := observed.Event
		if event.ExitCode == 0 {
			t.Fatalf("exit code = %d after kill, want non-zero", event.ExitCode)
		}
		if event.Reason != ExitReasonKilled {
			t.Fatalf("exit reason = %q after kill, want %q", event.Reason, ExitReasonKilled)
		}
		if event.ID != id || event.Command != "printf hello; sleep 1" {
			t.Fatalf("exit event = %+v, want id and command", event)
		}
		if !strings.Contains(observed.Output, "hello") {
			t.Fatalf("exit output = %q, want final output", observed.Output)
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
	exitCh := make(chan observedExit, 1)
	m.SetExitHandler(func(event ExitEvent) { exitCh <- observeExit(event) })
	id, err := m.Start("printf final", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case observed := <-exitCh:
		event := observed.Event
		if event.ID != id || event.Command != "printf final" || event.ExitCode != 0 || event.Reason != ExitReasonCompleted || observed.Output != "final" {
			t.Fatalf("exit event = %+v output = %q, want id, command, completed code 0, output final", event, observed.Output)
		}
	case <-time.After(time.Second):
		t.Fatal("exit handler not called")
	}
}

func TestManagerExitEventReasonsForErrorAndTimeout(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		m := NewManager(1, cmdoutput.Options{})
		exitCh := make(chan observedExit, 1)
		m.SetExitHandler(func(event ExitEvent) { exitCh <- observeExit(event) })
		if _, err := m.Start("printf fail; exit 7", 0); err != nil {
			t.Fatalf("Start: %v", err)
		}
		select {
		case observed := <-exitCh:
			event := observed.Event
			if event.Reason != ExitReasonError || event.ExitCode != 7 || observed.Output != "fail" {
				t.Fatalf("exit event = %+v output = %q, want error code 7 with output", event, observed.Output)
			}
		case <-time.After(time.Second):
			t.Fatal("exit handler not called")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		m := NewManager(1, cmdoutput.Options{})
		exitCh := make(chan observedExit, 1)
		m.SetExitHandler(func(event ExitEvent) { exitCh <- observeExit(event) })
		if _, err := m.Start("printf start; sleep 5", 1); err != nil {
			t.Fatalf("Start: %v", err)
		}
		select {
		case observed := <-exitCh:
			event := observed.Event
			if event.Reason != ExitReasonTimeout || event.TimeoutSec != 1 {
				t.Fatalf("exit event = %+v, want timeout reason and timeoutSec", event)
			}
			if event.ExitCode == 0 {
				t.Fatalf("timeout exit code = %d, want non-zero", event.ExitCode)
			}
			if !strings.Contains(observed.Output, "start") {
				t.Fatalf("timeout output = %q, want captured output", observed.Output)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("exit handler not called")
		}
	})
}

func TestManagerExitEventIncludesStartSessionID(t *testing.T) {
	m := NewManager(1, cmdoutput.Options{})
	sessionID := "session-a"
	m.SetSessionProvider(func() string { return sessionID })
	exitCh := make(chan ExitEvent, 1)
	m.SetExitHandler(func(event ExitEvent) { exitCh <- event })
	id, err := m.Start("printf final", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sessionID = "session-b"

	select {
	case event := <-exitCh:
		if event.ID != id || event.SessionID != "session-a" {
			t.Fatalf("exit event = %+v, want start session id", event)
		}
	case <-time.After(time.Second):
		t.Fatal("exit handler not called")
	}
}

func TestManagerStartUsesWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	t.Chdir(other)

	m := NewManagerAtRoot(1, cmdoutput.Options{}, root)
	exitCh := make(chan observedExit, 1)
	m.SetExitHandler(func(event ExitEvent) { exitCh <- observeExit(event) })
	if _, err := m.Start("pwd", 0); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case observed := <-exitCh:
		if strings.TrimSpace(observed.Output) != root {
			t.Fatalf("background pwd = %q, want workspace root %q", observed.Output, root)
		}
	case <-time.After(time.Second):
		t.Fatal("exit handler not called")
	}
}

func TestManagerFiltersProcessOperationsByCurrentSession(t *testing.T) {
	m := NewManager(2, cmdoutput.Options{})
	sessionID := "session-a"
	m.SetSessionProvider(func() string { return sessionID })

	id, err := m.Start("printf secret; sleep 2", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		sessionID = "session-a"
		_ = m.Kill(id)
	}()

	waitForReadContaining(t, m, id, "secret")
	if got := m.List(); !strings.Contains(got, id) || !strings.Contains(got, "printf secret") {
		t.Fatalf("session-a List = %q, want started process", got)
	}

	sessionID = "session-b"
	if got := m.List(); got != "No background processes." {
		t.Fatalf("session-b List = %q, want no visible processes", got)
	}
	if _, err := m.Read(id); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no process with ID %q", id)) {
		t.Fatalf("session-b Read error = %v, want missing process", err)
	}
	if err := m.Kill(id); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no process with ID %q", id)) {
		t.Fatalf("session-b Kill error = %v, want missing process", err)
	}

	sessionID = "session-a"
	if got := m.List(); !strings.Contains(got, id) {
		t.Fatalf("session-a List after blocked kill = %q, want original process still running", got)
	}
	if err := m.Kill(id); err != nil {
		t.Fatalf("session-a Kill: %v", err)
	}
}

func TestManagerLimitAppliesPerSession(t *testing.T) {
	m := NewManager(1, cmdoutput.Options{})
	sessionID := "session-a"
	m.SetSessionProvider(func() string { return sessionID })

	idA, err := m.Start("sleep 2", 0)
	if err != nil {
		t.Fatalf("Start session-a: %v", err)
	}
	defer func() {
		sessionID = "session-a"
		_ = m.Kill(idA)
	}()
	if _, err := m.Start("sleep 2", 0); err == nil {
		t.Fatal("Start second session-a process error = nil, want limit error")
	}

	sessionID = "session-b"
	idB, err := m.Start("sleep 2", 0)
	if err != nil {
		t.Fatalf("Start session-b with session-a process running: %v", err)
	}
	defer func() {
		sessionID = "session-b"
		_ = m.Kill(idB)
	}()
	if _, err := m.Start("sleep 2", 0); err == nil {
		t.Fatal("Start second session-b process error = nil, want limit error")
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
	exitCh := make(chan struct{}, 1)
	m.SetExitHandler(func(event ExitEvent) { exitCh <- struct{}{} })
	id, err := m.Start("printf done", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-exitCh:
	case <-time.After(time.Second):
		t.Fatal("exit handler not called")
	}
	waitForProcessRemoval(t, m, id)
	_, err = m.Read(id)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no process with ID %q", id)) {
		t.Fatalf("Read after exit error = %v, want missing process", err)
	}
}

func TestManagerReadWhileExitHandlerPendingReturnsFinalOutput(t *testing.T) {
	m := NewManager(1, cmdoutput.Options{})
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	handlerDone := make(chan observedExit, 1)
	m.SetExitHandler(func(event ExitEvent) {
		close(handlerStarted)
		<-releaseHandler
		handlerDone <- observeExit(event)
	})

	id, err := m.Start("printf final", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("exit handler not called")
	}

	output, err := m.Read(id)
	if err != nil {
		t.Fatalf("Read while handler pending: %v", err)
	}
	if output != "final" {
		t.Fatalf("Read while handler pending = %q, want final", output)
	}

	close(releaseHandler)
	select {
	case observed := <-handlerDone:
		if observed.Event.ID != id || observed.Output != "final" {
			t.Fatalf("exit event = %+v output = %q, want id and final output", observed.Event, observed.Output)
		}
	case <-time.After(time.Second):
		t.Fatal("exit handler did not complete")
	}

	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		_, err = m.Read(id)
		if err != nil && strings.Contains(err.Error(), fmt.Sprintf("no process with ID %q", id)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Read after handler completion error = %v, want missing process", err)
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
	waitForProcessRemoval(t, m, id)
	assertNoProcOutputSpills(t, home)
}

func TestManagerDroppedExitHandlerDoesNotKeepUnreferencedSpill(t *testing.T) {
	home := t.TempDir()
	m := NewManager(1, cmdoutput.Options{
		HomeDir:      home,
		SpillPrefix:  "proc_output_",
		MaxBytes:     8,
		MaxLineChars: 80,
	})
	exitCh := make(chan ExitEvent, 1)
	m.SetExitHandler(func(event ExitEvent) {
		exitCh <- event
	})
	id, err := m.Start("printf 'abcdefghijklmnopqrstuvwxyz'", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case event := <-exitCh:
		if event.FormatOutput == nil {
			t.Fatal("exit event FormatOutput = nil")
		}
	case <-time.After(time.Second):
		t.Fatal("exit handler not called")
	}
	waitForProcessRemoval(t, m, id)
	assertNoProcOutputSpills(t, home)
}

func waitForProcessRemoval(t *testing.T, m *Manager, id string) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		m.mu.Lock()
		_, ok := m.procs[id]
		m.mu.Unlock()
		if !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %s did not finish", id)
}

func assertNoProcOutputSpills(t *testing.T, home string) {
	t.Helper()
	var leftovers []string
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		leftovers = nil
		entries, err := os.ReadDir(filepath.Join(home, ".lightcode"))
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Fatalf("ReadDir spill dir: %v", err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "proc_output_") {
				leftovers = append(leftovers, entry.Name())
			}
		}
		if len(leftovers) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("unexpected unreferenced spill files kept: %v", leftovers)
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
