// Package process manages background processes started by run_command
// with background=true.
package process

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/cmdoutput"
)

// CommandStarted represents a running background process.
type CommandStarted struct {
	ID        string
	Command   string
	StartedAt time.Time
	cmd       *exec.Cmd
	capture   *cmdoutput.Capture
	done      chan struct{}
	mu        sync.Mutex
	exitCode  int
	exited    bool
	exitErr   error
}

// Manager tracks background processes.
type Manager struct {
	mu            sync.Mutex
	procs         map[string]*CommandStarted
	onExit        func(ExitEvent)
	maxProcs      int
	outputOptions cmdoutput.Options
}

type ExitEvent struct {
	ID       string
	Command  string
	ExitCode int
	Output   string
}

// NewManager creates a new process Manager. maxProcs limits concurrent
// background processes (0 = unlimited).
func NewManager(maxProcs int, outputOptions cmdoutput.Options) *Manager {
	return &Manager{
		procs:         make(map[string]*CommandStarted),
		maxProcs:      maxProcs,
		outputOptions: outputOptions,
	}
}

// SetExitHandler sets a callback invoked when a background process exits.
// The callback should append a system signal to the loop.
func (m *Manager) SetExitHandler(handler func(ExitEvent)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onExit = handler
}

// Start launches a background process and returns its ID.
func (m *Manager) Start(command string, timeoutSec int) (string, error) {
	if m.maxProcs > 0 {
		running := 0
		m.mu.Lock()
		for _, cs := range m.procs {
			cs.mu.Lock()
			if !cs.exited {
				running++
			}
			cs.mu.Unlock()
		}
		m.mu.Unlock()
		if running >= m.maxProcs {
			return "", fmt.Errorf("process: background process limit reached (%d/%d). Kill existing processes or wait for them to exit", running, m.maxProcs)
		}
	}

	id, err := newProcessID()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	capture := cmdoutput.NewCapture(m.outputOptions)
	cmd.Stdout = capture.Stdout()
	cmd.Stderr = capture.Stderr()

	if err := cmd.Start(); err != nil {
		capture.Close()
		return "", err
	}

	cs := &CommandStarted{
		ID:        id,
		Command:   command,
		StartedAt: time.Now(),
		cmd:       cmd,
		capture:   capture,
		done:      make(chan struct{}),
	}

	m.mu.Lock()
	m.procs[id] = cs
	handler := m.onExit
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		cs.mu.Lock()
		cs.exited = true
		cs.exitErr = err
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				cs.exitCode = exitErr.ExitCode()
			} else {
				cs.exitCode = -1
			}
		}
		code := cs.exitCode
		cs.mu.Unlock()

		if handler != nil {
			output := capture.Format()
			handler(ExitEvent{
				ID:       id,
				Command:  command,
				ExitCode: code,
				Output:   output,
			})
		}

		// Auto-remove dead process so it doesn't count against the limit.
		m.Remove(id)
		close(cs.done)
		capture.Close()
	}()

	return id, nil
}

// Read returns output accumulated so far for a running background process.
func (m *Manager) Read(id string) (string, error) {
	m.mu.Lock()
	cs, ok := m.procs[id]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("process: no process with ID %q", id)
	}

	cs.mu.Lock()
	if cs.exited {
		cs.mu.Unlock()
		return "", fmt.Errorf("process: no process with ID %q", id)
	}
	cs.mu.Unlock()

	if cs.capture.Len() == 0 {
		return "(No output yet)", nil
	}
	return cs.capture.Format(), nil
}

// Kill terminates a background process. Sends SIGTERM, waits 500ms,
// then SIGKILL.
func (m *Manager) Kill(id string) error {
	m.mu.Lock()
	cs, ok := m.procs[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("process: no process with ID %q", id)
	}

	cs.mu.Lock()
	if cs.exited {
		cs.mu.Unlock()
		return nil
	}
	cs.mu.Unlock()

	// SIGTERM to process group.
	_ = syscall.Kill(-cs.cmd.Process.Pid, syscall.SIGTERM)

	select {
	case <-cs.done:
	case <-time.After(500 * time.Millisecond):
		_ = syscall.Kill(-cs.cmd.Process.Pid, syscall.SIGKILL)
		<-cs.done
	}
	return nil
}

// List returns a formatted list of all background processes.
func (m *Manager) List() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.procs) == 0 {
		return "No background processes."
	}
	var result string
	for _, cs := range m.procs {
		cs.mu.Lock()
		dur := time.Since(cs.StartedAt).Round(time.Second)
		status := ""
		if cs.exited {
			status = fmt.Sprintf(" (exited with code %d)", cs.exitCode)
		}
		cs.mu.Unlock()
		result += fmt.Sprintf("%s  %s  (running for %s%s)\n", cs.ID, cs.Command, dur, status)
	}
	if len(result) > 0 && result[len(result)-1] == '\n' {
		result = result[:len(result)-1]
	}
	return result
}

// KillAll terminates all running background processes.
func (m *Manager) KillAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.procs))
	for id := range m.procs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Kill(id)
	}
}

// Remove cleans up a finished process entry.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.procs, id)
}

func newProcessID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
