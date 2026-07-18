// Package process manages background processes started by run_command
// with background=true.
package process

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/cmdoutput"
)

type ExitReason string

const (
	ExitReasonCompleted ExitReason = "completed"
	ExitReasonError     ExitReason = "error"
	ExitReasonTimeout   ExitReason = "timeout"
	ExitReasonKilled    ExitReason = "killed"
)

// CommandStarted represents a running background process.
type CommandStarted struct {
	ID         string
	Command    string
	SessionID  string
	StartedAt  time.Time
	cmd        *exec.Cmd
	capture    *cmdoutput.Capture
	done       chan struct{}
	mu         sync.Mutex
	exitCode   int
	exited     bool
	finalized  bool
	exitErr    error
	reason     ExitReason
	timeoutSec int
}

// Manager tracks background processes.
type Manager struct {
	mu            sync.Mutex
	procs         map[string]*CommandStarted
	onExit        func(ExitEvent)
	sessionID     func() string
	maxProcs      int
	outputOptions cmdoutput.Options
	workspaceRoot string
	// closed rejects new starts after Close. cbClosed suppresses exit callbacks
	// not yet admitted; cbWG tracks callbacks admitted before close, both guarded
	// by mu so admission cannot race Close's wait. closeOnce/closeDone make Close
	// a single shared join so concurrent callers all wait for the same cleanup.
	closed    bool
	cbClosed  bool
	cbWG      sync.WaitGroup
	closeOnce sync.Once
	closeDone chan struct{}
}

type ExitEvent struct {
	ID           string
	Command      string
	SessionID    string
	ExitCode     int
	Reason       ExitReason
	TimeoutSec   int
	FormatOutput func() string
}

// SessionManager binds process operations to one session.
type SessionManager struct {
	manager   *Manager
	sessionID func() string
}

// NewManager creates a new process Manager. maxProcs limits concurrent
// background processes (0 = unlimited).
func NewManager(maxProcs int, outputOptions cmdoutput.Options) *Manager {
	return NewManagerAtRoot(maxProcs, outputOptions, "")
}

func NewManagerAtRoot(maxProcs int, outputOptions cmdoutput.Options, workspaceRoot string) *Manager {
	return &Manager{
		procs:         make(map[string]*CommandStarted),
		maxProcs:      maxProcs,
		outputOptions: outputOptions,
		workspaceRoot: workspaceRoot,
		closeDone:     make(chan struct{}),
	}
}

func (m *Manager) SetLimits(maxProcs int, outputOptions cmdoutput.Options) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxProcs = maxProcs
	m.outputOptions = outputOptions
}

// SetExitHandler sets a callback invoked when a background process exits.
// The callback should append a system signal to the loop.
func (m *Manager) SetExitHandler(handler func(ExitEvent)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onExit = handler
}

// SetSessionProvider sets a callback used to tag newly started background
// processes with the session that launched them.
func (m *Manager) SetSessionProvider(provider func() string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionID = provider
}

func (m *Manager) ForSession(sessionID func() string) *SessionManager {
	return &SessionManager{manager: m, sessionID: sessionID}
}

func (m *SessionManager) currentSessionID() string {
	if m == nil || m.sessionID == nil {
		return ""
	}
	return strings.TrimSpace(m.sessionID())
}

func (m *SessionManager) Start(command string, timeoutSec int) (string, error) {
	if m == nil || m.manager == nil {
		return "", fmt.Errorf("process: manager unavailable")
	}
	return m.manager.StartForSession(m.currentSessionID(), command, timeoutSec)
}

func (m *SessionManager) Read(id string) (string, error) {
	if m == nil || m.manager == nil {
		return "", fmt.Errorf("process: manager unavailable")
	}
	return m.manager.ReadForSession(m.currentSessionID(), id)
}

func (m *SessionManager) Kill(id string) error {
	if m == nil || m.manager == nil {
		return fmt.Errorf("process: manager unavailable")
	}
	return m.manager.KillForSession(m.currentSessionID(), id)
}

func (m *SessionManager) List() string {
	if m == nil || m.manager == nil {
		return "No background processes."
	}
	return m.manager.ListForSession(m.currentSessionID())
}

func (m *SessionManager) ActiveIDs() []string {
	if m == nil || m.manager == nil {
		return nil
	}
	return m.manager.ActiveIDsForSession(m.currentSessionID())
}

// Start launches a background process and returns its ID.
func (m *Manager) Start(command string, timeoutSec int) (string, error) {
	return m.StartForSession(m.currentSessionID(), command, timeoutSec)
}

func (m *Manager) StartForSession(sessionID string, command string, timeoutSec int) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	id, err := newProcessID()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("sh", "-c", command)
	if m.workspaceRoot != "" {
		cmd.Dir = m.workspaceRoot
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Hold mu from the closed/limit check through cmd.Start and registration so a
	// child is either registered before Close or never starts after it.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", fmt.Errorf("process: manager is closed")
	}
	if m.maxProcs > 0 {
		running := 0
		for _, cs := range m.procs {
			if cs.SessionID != sessionID {
				continue
			}
			cs.mu.Lock()
			if !cs.exited {
				running++
			}
			cs.mu.Unlock()
		}
		if running >= m.maxProcs {
			m.mu.Unlock()
			return "", fmt.Errorf("process: background process limit reached (%d/%d). Kill existing processes or wait for them to exit", running, m.maxProcs)
		}
	}
	capture := cmdoutput.NewCapture(m.outputOptions)
	cmd.Stdout = capture.Stdout()
	cmd.Stderr = capture.Stderr()
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		capture.Close()
		return "", err
	}
	cs := &CommandStarted{
		ID:         id,
		Command:    command,
		SessionID:  sessionID,
		StartedAt:  time.Now(),
		cmd:        cmd,
		capture:    capture,
		done:       make(chan struct{}),
		timeoutSec: timeoutSec,
	}
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
			if cs.reason == "" {
				cs.reason = ExitReasonError
			}
		} else if cs.reason == "" {
			cs.reason = ExitReasonCompleted
		}
		code := cs.exitCode
		reason := cs.reason
		cs.mu.Unlock()

		// The child is reaped: unblock kill/Close before running the exit
		// callback, so reaping never waits on a callback.
		close(cs.done)

		// Run the exit callback only if admitted; Close suppresses callbacks not
		// yet admitted and joins those admitted before it. FormatOutput stays lazy
		// so a dropped handler never surfaces the full spilled output.
		m.mu.Lock()
		admit := handler != nil && !m.cbClosed
		if admit {
			m.cbWG.Add(1)
		}
		m.mu.Unlock()
		if admit {
			handler(ExitEvent{
				ID:           id,
				Command:      command,
				SessionID:    sessionID,
				ExitCode:     code,
				Reason:       reason,
				TimeoutSec:   timeoutSec,
				FormatOutput: capture.Format,
			})
			m.cbWG.Done()
		}

		cs.mu.Lock()
		cs.finalized = true
		cs.mu.Unlock()

		// Auto-remove finalized process so it doesn't count against the limit.
		m.Remove(id)
		capture.Close()
	}()

	if timeoutSec > 0 {
		go func() {
			timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
			defer timer.Stop()
			select {
			case <-cs.done:
			case <-timer.C:
				if cs.markReason(ExitReasonTimeout) {
					terminateProcessGroup(cmd.Process.Pid)
					select {
					case <-cs.done:
					case <-time.After(500 * time.Millisecond):
						_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					}
				}
			}
		}()
	}

	return id, nil
}

// Read returns output accumulated so far for a running background process.
func (m *Manager) Read(id string) (string, error) {
	return m.ReadForSession(m.currentSessionID(), id)
}

func (m *Manager) ReadForSession(sessionID, id string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	m.mu.Lock()
	cs, ok := m.procs[id]
	m.mu.Unlock()
	if !ok || cs.SessionID != sessionID {
		return "", fmt.Errorf("process: no process with ID %q", id)
	}

	cs.mu.Lock()
	if cs.finalized {
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
	return m.KillForSession(m.currentSessionID(), id)
}

func (m *Manager) KillForSession(sessionID, id string) error {
	return m.kill(id, strings.TrimSpace(sessionID), true)
}

func (m *Manager) kill(id string, sessionID string, enforceSession bool) error {
	m.mu.Lock()
	cs, ok := m.procs[id]
	m.mu.Unlock()
	if !ok || (enforceSession && cs.SessionID != sessionID) {
		return fmt.Errorf("process: no process with ID %q", id)
	}

	cs.mu.Lock()
	if cs.exited {
		cs.mu.Unlock()
		return nil
	}
	cs.mu.Unlock()

	cs.markReason(ExitReasonKilled)
	terminateProcessGroup(cs.cmd.Process.Pid)

	select {
	case <-cs.done:
	case <-time.After(500 * time.Millisecond):
		_ = syscall.Kill(-cs.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-cs.done:
		case <-time.After(5 * time.Second):
			// An unreapable child after SIGKILL is diagnosed and abandoned rather
			// than blocking shutdown indefinitely.
			fmt.Fprintf(os.Stderr, "lightcode: process %s not reaped within 5s after SIGKILL\n", id)
		}
	}
	return nil
}

// Close stops accepting new starts, terminates and reaps every registered child,
// and joins every exit callback admitted before close; callbacks not yet
// admitted are suppressed. It is idempotent.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cbClosed = true
		ids := make([]string, 0, len(m.procs))
		for id := range m.procs {
			ids = append(ids, id)
		}
		m.mu.Unlock()
		for _, id := range ids {
			_ = m.kill(id, "", false)
		}
		m.cbWG.Wait()
		close(m.closeDone)
	})
	// Every caller, including the one that performed the work, waits for the
	// shared cleanup to complete so Close is a real join.
	<-m.closeDone
}

func (cs *CommandStarted) markReason(reason ExitReason) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.exited || cs.reason != "" {
		return false
	}
	cs.reason = reason
	return true
}

func terminateProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
}

// List returns a formatted list of background processes for the current session.
func (m *Manager) List() string {
	return m.ListForSession(m.currentSessionID())
}

func (m *Manager) ListForSession(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	m.mu.Lock()
	defer m.mu.Unlock()
	var result string
	for _, cs := range m.procs {
		if cs.SessionID != sessionID {
			continue
		}
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
	if result == "" {
		return "No background processes."
	}
	return result
}

func (m *Manager) ActiveIDs() []string {
	return m.ActiveIDsForSession(m.currentSessionID())
}

func (m *Manager) ActiveIDsForSession(sessionID string) []string {
	sessionID = strings.TrimSpace(sessionID)
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.procs))
	for id, cs := range m.procs {
		if cs.SessionID != sessionID {
			continue
		}
		cs.mu.Lock()
		running := !cs.exited
		cs.mu.Unlock()
		if running {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
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
		_ = m.kill(id, "", false)
	}
}

// Remove cleans up a finished process entry.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.procs, id)
}

func (m *Manager) currentSessionID() string {
	m.mu.Lock()
	provider := m.sessionID
	m.mu.Unlock()
	if provider == nil {
		return ""
	}
	return provider()
}

func newProcessID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
