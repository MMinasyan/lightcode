// Package jobs declares the background jobs capability: reservation-native
// job identities, per-Session attribution over real background processes,
// bounded output capture under the session artifact tree, per-Session limits,
// and kill escalation with a total StopJob over every state. It is an
// ordinary Runtime-scoped composition unit providing two declared views of
// one instance — the Jobs contract for tool-facing calls and the Harness
// job-stop seam at its lowest consumer — with no additional Runtime
// authority and no legacy process-manager wrapper.
package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/cmdoutput"
	"github.com/MMinasyan/lightcode/runtime"
)

const pluginID = "jobs"

// Exit reasons: first reason wins, recorded by whichever of the timeout or
// kill paths gets there before the exit goroutine settles the real result.
const (
	reasonCompleted = "completed"
	reasonError     = "error"
	reasonTimeout   = "timeout"
	reasonKilled    = "killed"
)

// jobState is the record lifecycle: reserved -> running -> exited; an exited
// record is removed after the callback and cleanup. There is no aborted
// state: a reservation is removed, not transitioned.
type jobState string

const (
	stateReserved jobState = "reserved"
	stateRunning  jobState = "running"
	stateExited   jobState = "exited"
)

// ExitResult is one job's terminal outcome delivered to the caller's OnExit
// callback: the identity, the first-wins reason, the exit code, the bounded
// formatted output, and this job's captured max_output_bytes setting, which
// also bounds the composed completion.
type ExitResult struct {
	ID, Command, Reason string
	ExitCode            int
	Output              string
	MaxOutputBytes      int
}

// StartRequest is one reserved job's launch input. Config is the opaque
// plugins.jobs section the caller forwards; Env is the exact child
// environment; TimeoutSec 0 means no timeout.
type StartRequest struct {
	JobID, SessionID, Workspace, Command string
	TimeoutSec                           int
	Env                                  []string
	Config                               json.RawMessage
	OnExit                               func(ExitResult)
}

// Jobs is the capability contract: a reservation-native identity mint,
// per-Session start/read/kill/list/live over background processes, and the
// total StopJob (also exported separately as the Harness job-stop seam).
type Jobs interface {
	Reserve() (string, error) // mint an 8-hex job ID (crypto/rand, the newProcessID shape) and register it reserved
	Abort(jobID string)       // resolve a reservation that will not start; idempotent
	Start(ctx context.Context, req StartRequest) error
	Read(sessionID, jobID string) (string, error)
	Kill(sessionID, jobID string) error
	List(sessionID string) string
	Live(sessionID string) []string // the session's running job IDs, sorted
}

// settings is the plugin's private configuration, decoded with json.Unmarshal
// from the plugins.jobs section. Nil input and missing or null-valued members
// keep the defaults; unknown members are ignored; every consumed field is
// range-checked against the retained bounds.
type settings struct {
	MaxBackgroundProcesses int `json:"max_background_processes"`
	MaxOutputBytes         int `json:"max_output_bytes"`
	ReadLineMaxChars       int `json:"read_line_max_chars"`
}

func defaultSettings() settings {
	return settings{MaxBackgroundProcesses: 10, MaxOutputBytes: 15360, ReadLineMaxChars: 5000}
}

// decodeSettings decodes one supplied plugins.jobs section on top of the
// defaults and enforces the retained ranges.
func decodeSettings(raw json.RawMessage) (settings, error) {
	s := defaultSettings()
	if len(bytes.TrimSpace(raw)) != 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return settings{}, fmt.Errorf("jobs: invalid settings: %w", err)
		}
	}
	switch {
	case s.MaxBackgroundProcesses < 1 || s.MaxBackgroundProcesses > 50:
		return settings{}, errors.New("jobs: max_background_processes must be between 1 and 50")
	case s.MaxOutputBytes < 1024 || s.MaxOutputBytes > 1048576:
		return settings{}, errors.New("jobs: max_output_bytes must be between 1024 and 1048576")
	case s.ReadLineMaxChars < 100 || s.ReadLineMaxChars > 100000:
		return settings{}, errors.New("jobs: read_line_max_chars must be between 100 and 100000")
	}
	return s, nil
}

// jobRecord is one job's state. Immutable fields (id, command, capture,
// onExit, session identity, process handle, captured settings, and the done
// channel created when the reservation becomes running) are written once
// before any reader can observe them; the lifecycle fields are guarded by the
// record's own mutex so the exit and timeout goroutines settle them without
// the capability mutex. The map of records, the closed flag, and the work
// group are guarded by the capability mutex; the lock order is always
// capability mutex then record mutex.
type jobRecord struct {
	id   string
	done chan struct{} // created when the reservation becomes running; closed when the process is reaped, before the callback

	mu        sync.Mutex
	state     jobState
	sessionID string
	command   string
	startedAt time.Time
	cmd       *exec.Cmd
	capture   *cmdoutput.Capture
	onExit    func(ExitResult)
	maxBytes  int
	exitCode  int
	reason    string
}

// markReason records the first terminal reason; it fails once the process has
// settled or an earlier reason won.
func (r *jobRecord) markReason(reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateExited || r.reason != "" {
		return false
	}
	r.reason = reason
	return true
}

// claimKilledLocked claims the killed reason for a record still observed as
// running; the caller holds r.mu (and the capability mutex), so a concurrent
// reap cannot record its own reason first — first-reason-wins resolves to
// killed.
func (r *jobRecord) claimKilledLocked() {
	if r.state != stateExited && r.reason == "" {
		r.reason = reasonKilled
	}
}

// instance is one Open's constructed state: the owner data root, the job map,
// the closed flag, and the work group covering every exit/timeout goroutine
// (and therefore every admitted callback and all capture/record cleanup).
type instance struct {
	mu      sync.Mutex
	jobs    map[string]*jobRecord
	closed  bool
	wg      sync.WaitGroup
	dataDir string
}

var _ Jobs = (*instance)(nil)
var _ harness.JobStopper = (*instance)(nil)

// waitCommand is the one cmd.Wait seam for background jobs: the Wait-returned
// point of every job's exit goroutine routes through it, so tests drive the
// reaping boundary deterministically. It is nil-free in production and simply
// delegates to cmd.Wait.
var waitCommand = func(cmd *exec.Cmd) error { return cmd.Wait() }

// Reserve mints an 8-hex job identity and registers it reserved.
func (j *instance) Reserve() (string, error) {
	id, err := newJobID()
	if err != nil {
		return "", err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return "", fmt.Errorf("process: manager is closed")
	}
	j.jobs[id] = &jobRecord{id: id, state: stateReserved}
	return id, nil
}

// Abort removes a reservation that will not start; it is idempotent and a
// no-op over running, exited, or absent records.
func (j *instance) Abort(jobID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	rec, ok := j.jobs[jobID]
	if !ok {
		return
	}
	rec.mu.Lock()
	reserved := rec.state == stateReserved
	rec.mu.Unlock()
	if reserved {
		delete(j.jobs, jobID)
	}
}

// Start consumes the identified reservation on every result: a successful
// spawn changes it to running, and every error — config, limit, spawn, or a
// non-reserved identity failing the retained unknown-ID check — removes only
// the identified reserved record when it is one. The capability mutex spans
// the closed, reserved-record, config, and limit checks through cmd.Start and
// the running registration, so a child is either registered before a close or
// never starts after it. The process is not bound to the caller context: only
// its timeout or an explicit stop ends it.
func (j *instance) Start(_ context.Context, req StartRequest) error {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return fmt.Errorf("process: manager is closed")
	}
	rec, ok := j.jobs[req.JobID]
	if !ok {
		j.mu.Unlock()
		return fmt.Errorf("process: no process with ID %q", req.JobID)
	}
	rec.mu.Lock()
	reserved := rec.state == stateReserved
	rec.mu.Unlock()
	if !reserved {
		j.mu.Unlock()
		return fmt.Errorf("process: no process with ID %q", req.JobID)
	}
	s, err := decodeSettings(req.Config)
	if err != nil {
		delete(j.jobs, req.JobID)
		j.mu.Unlock()
		return err
	}
	running := 0
	for _, other := range j.jobs {
		other.mu.Lock()
		if other.state == stateRunning && other.sessionID == req.SessionID {
			running++
		}
		other.mu.Unlock()
	}
	if running >= s.MaxBackgroundProcesses {
		delete(j.jobs, req.JobID)
		j.mu.Unlock()
		return fmt.Errorf("process: background process limit reached (%d/%d). Kill existing processes or wait for them to exit", running, s.MaxBackgroundProcesses)
	}
	cmd := exec.Command("sh", "-c", req.Command)
	cmd.Dir = req.Workspace
	cmd.Env = req.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	capture := cmdoutput.NewCapture(cmdoutput.Options{
		Directory:    filepath.Join(j.dataDir, "code", req.SessionID, "output"),
		SpillPrefix:  "proc_output_",
		MaxBytes:     s.MaxOutputBytes,
		MaxLineChars: s.ReadLineMaxChars,
	})
	cmd.Stdout = capture.Stdout()
	cmd.Stderr = capture.Stderr()
	if err := cmd.Start(); err != nil {
		delete(j.jobs, req.JobID)
		capture.Close()
		j.mu.Unlock()
		return err
	}
	rec.mu.Lock()
	rec.state = stateRunning
	rec.sessionID = req.SessionID
	rec.command = req.Command
	rec.startedAt = time.Now()
	rec.cmd = cmd
	rec.capture = capture
	rec.onExit = req.OnExit
	rec.maxBytes = s.MaxOutputBytes
	rec.done = make(chan struct{})
	rec.mu.Unlock()
	// Every exit/timeout goroutine is added to the work group under the
	// capability mutex before it can start, so Close's mark-then-wait cannot
	// race an Add.
	j.wg.Add(1)
	if req.TimeoutSec > 0 {
		j.wg.Add(1)
	}
	j.mu.Unlock()

	go j.exitLoop(rec, cmd, capture)
	if req.TimeoutSec > 0 {
		go j.timeoutLoop(rec, cmd, req.TimeoutSec)
	}
	return nil
}

// exitLoop waits for the process through the wait seam, settles the exit code
// and the first-wins reason, closes the reaped notification before the
// callback, admits a non-nil OnExit only while the instance is not closed,
// invokes it outside the capability mutex with the formatted output, then
// removes the record and closes the capture before the work-group Done. Nil
// callbacks skip invocation but not cleanup.
func (j *instance) exitLoop(rec *jobRecord, cmd *exec.Cmd, capture *cmdoutput.Capture) {
	defer j.wg.Done()
	waitErr := waitCommand(cmd)

	rec.mu.Lock()
	rec.state = stateExited
	code := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}
	rec.exitCode = code
	if rec.reason == "" {
		if waitErr != nil {
			rec.reason = reasonError
		} else {
			rec.reason = reasonCompleted
		}
	}
	reason := rec.reason
	rec.mu.Unlock()

	// The child is reaped: unblock StopJob/Close/Kill before the callback, so
	// reaping never waits on a callback.
	close(rec.done)

	j.mu.Lock()
	admit := rec.onExit != nil && !j.closed
	j.mu.Unlock()
	if admit {
		rec.onExit(ExitResult{
			ID:             rec.id,
			Command:        rec.command,
			Reason:         reason,
			ExitCode:       code,
			Output:         capture.Format(),
			MaxOutputBytes: rec.maxBytes,
		})
	}

	j.mu.Lock()
	if j.jobs[rec.id] == rec {
		delete(j.jobs, rec.id)
	}
	j.mu.Unlock()
	capture.Close()
}

// timeoutLoop retains the timer/SIGTERM/500ms/SIGKILL escalation: once the
// timer fires and the first-reason claim succeeds, the process group is
// converged; the exit goroutine's reaped notification ends the wait.
func (j *instance) timeoutLoop(rec *jobRecord, cmd *exec.Cmd, timeoutSec int) {
	defer j.wg.Done()
	timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer timer.Stop()
	select {
	case <-rec.done:
	case <-timer.C:
		if rec.markReason(reasonTimeout) {
			terminateProcessGroup(cmd.Process.Pid)
			select {
			case <-rec.done:
			case <-time.After(500 * time.Millisecond):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	}
}

// Read returns the retained output text for one owned record: unknown-ID for
// absent or foreign identities (reservations are invisible), the retained
// empty placeholder for a silent capture, and the bounded format otherwise.
func (j *instance) Read(sessionID, jobID string) (string, error) {
	j.mu.Lock()
	rec, ok := j.jobs[jobID]
	j.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("process: no process with ID %q", jobID)
	}
	rec.mu.Lock()
	if rec.state == stateReserved || rec.sessionID != sessionID {
		rec.mu.Unlock()
		return "", fmt.Errorf("process: no process with ID %q", jobID)
	}
	capture := rec.capture
	rec.mu.Unlock()
	if capture.Len() == 0 {
		return "(No output yet)", nil
	}
	return capture.Format(), nil
}

// Kill is the user-facing termination: absent, foreign, or still-reserved
// identities report the retained unknown-ID error, an exited record returns
// nil (termination, not reaping, is Kill's contract), and a running job gets
// the group SIGTERM/500ms/SIGKILL escalation with the legacy 5-second
// diagnose-and-abandon outcome returning nil.
func (j *instance) Kill(sessionID, jobID string) error {
	j.mu.Lock()
	rec, ok := j.jobs[jobID]
	j.mu.Unlock()
	if !ok {
		return fmt.Errorf("process: no process with ID %q", jobID)
	}
	rec.mu.Lock()
	switch {
	case rec.state == stateReserved || rec.sessionID != sessionID:
		rec.mu.Unlock()
		return fmt.Errorf("process: no process with ID %q", jobID)
	case rec.state == stateExited:
		rec.mu.Unlock()
		return nil
	}
	pid := rec.cmd.Process.Pid
	rec.mu.Unlock()

	rec.markReason(reasonKilled)
	terminateProcessGroup(pid)
	select {
	case <-rec.done:
	case <-time.After(500 * time.Millisecond):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case <-rec.done:
		case <-time.After(5 * time.Second):
			// An unreapable child after SIGKILL is diagnosed and abandoned
			// rather than blocking the caller indefinitely.
			fmt.Fprintf(os.Stderr, "lightcode: process %s not reaped within 5s after SIGKILL\n", jobID)
		}
	}
	return nil
}

// StopJob is the Harness job-stop seam and the capability's total stop: a
// reservation is removed and returns; a running job has the killed reason
// claimed and its pid/reaped channel captured while the capability mutex is
// still held — so a concurrent reap cannot record completed/error first —
// then converges through the same unbounded reaping path outside every mutex
// (Start and StopJob stay serialized), returning once the child is reaped;
// the ordinary exit callback then delivers the stopped result. Exited,
// absent, or foreign identities return.
func (j *instance) StopJob(sessionID, jobID string) {
	j.mu.Lock()
	rec, ok := j.jobs[jobID]
	if !ok {
		j.mu.Unlock()
		return
	}
	rec.mu.Lock()
	state, session := rec.state, rec.sessionID
	if state == stateReserved {
		rec.mu.Unlock()
		delete(j.jobs, jobID)
		j.mu.Unlock()
		return
	}
	if state == stateExited || session != sessionID {
		rec.mu.Unlock()
		j.mu.Unlock()
		return
	}
	rec.claimKilledLocked()
	pid := rec.cmd.Process.Pid
	done := rec.done
	rec.mu.Unlock()
	j.mu.Unlock()

	reapGroup(pid, done)
}

// reapGroup performs the unbounded group convergence — SIGTERM, 500ms,
// SIGKILL, then the reaping wait with no second timeout — outside every
// mutex. The killed-reason claim already happened under the capability
// mutex.
func reapGroup(pid int, done <-chan struct{}) {
	terminateProcessGroup(pid)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	<-done
}

// List returns one line per started record of the session (reservations are
// invisible; unspecified order), with the retained running and exited suffix
// formats, the trailing newline stripped, and the retained empty text.
func (j *instance) List(sessionID string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var result string
	for _, rec := range j.jobs {
		rec.mu.Lock()
		if rec.state == stateReserved || rec.sessionID != sessionID {
			rec.mu.Unlock()
			continue
		}
		dur := time.Since(rec.startedAt).Round(time.Second)
		status := ""
		if rec.state == stateExited {
			status = fmt.Sprintf(" (exited with code %d)", rec.exitCode)
		}
		id, command := rec.id, rec.command
		rec.mu.Unlock()
		result += fmt.Sprintf("%s  %s  (running for %s%s)\n", id, command, dur, status)
	}
	if len(result) > 0 && result[len(result)-1] == '\n' {
		result = result[:len(result)-1]
	}
	if result == "" {
		return "No background processes."
	}
	return result
}

// Live returns the session's running job IDs, sorted; reservations and exited
// records are excluded.
func (j *instance) Live(sessionID string) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	ids := make([]string, 0, len(j.jobs))
	for _, rec := range j.jobs {
		rec.mu.Lock()
		if rec.state == stateRunning && rec.sessionID == sessionID {
			ids = append(ids, rec.id)
		}
		rec.mu.Unlock()
	}
	sort.Strings(ids)
	return ids
}

// Close marks the instance closed under the capability mutex — new
// reservations and starts fail the retained closed error, callbacks not yet
// admitted are suppressed, and no further goroutine can be added — claiming
// the killed reason and capturing the pid/reaped channel of every running
// job in the same hold, then, outside the mutex, converging each through the
// unbounded reaping path, waiting for the instance work group (admitted
// callbacks, timeout goroutines, and all capture/record cleanup included),
// and finally removing the leftover reservations. Callbacks execute outside
// the mutex.
func (j *instance) Close() error {
	type runningJob struct {
		pid  int
		done <-chan struct{}
	}
	j.mu.Lock()
	j.closed = true
	running := make([]runningJob, 0, len(j.jobs))
	for _, rec := range j.jobs {
		rec.mu.Lock()
		if rec.state == stateRunning {
			rec.claimKilledLocked()
			running = append(running, runningJob{pid: rec.cmd.Process.Pid, done: rec.done})
		}
		rec.mu.Unlock()
	}
	j.mu.Unlock()

	for _, job := range running {
		reapGroup(job.pid, job.done)
	}
	j.wg.Wait()

	j.mu.Lock()
	for id, rec := range j.jobs {
		rec.mu.Lock()
		reserved := rec.state == stateReserved
		rec.mu.Unlock()
		if reserved {
			delete(j.jobs, id)
		}
	}
	j.mu.Unlock()
	return nil
}

// Plugin returns the Runtime-scoped plugin providing the two declared views
// of one jobs instance: Spec[Jobs]("jobs") for tool-facing calls and
// Spec[harness.JobStopper]("job-stopper") for the Harness consumer. It has
// no dependencies. ValidateConfig validates the owned plugins.jobs section;
// Open captures the owner data root the session output directories derive
// from, and the instance's Close is the scope disposal.
func Plugin() runtime.Plugin {
	return runtime.Plugin{
		ID:    pluginID,
		Scope: runtime.ScopeRuntime,
		Provides: []runtime.CapabilitySpec{
			runtime.Spec[Jobs]("jobs"),
			runtime.Spec[harness.JobStopper]("job-stopper"),
		},
		ValidateConfig: func(raw json.RawMessage) error {
			_, err := decodeSettings(raw)
			return err
		},
		Open: open,
	}
}

// open checks the scope context and captures the scope identity's owner data
// root; one instance backs both declared views.
func open(ctx context.Context, info runtime.ScopeInfo, _ runtime.Bindings) (runtime.Instance, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Instance{}, err
	}
	inst := &instance{
		jobs:    make(map[string]*jobRecord),
		dataDir: info.DataDir,
	}
	return runtime.Instance{
		Values: map[string]any{
			"jobs":        Jobs(inst),
			"job-stopper": harness.JobStopper(inst),
		},
		Close: inst.Close,
	}, nil
}

func newJobID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func terminateProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
}
