package agent

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// AdapterScope holds an adapter-local project path and routes the
// project-scoped convenience methods through the *ForProjectPath variants.
// It is used by in-process adapters (Wails, CLI) that need mutable project
// paths for in-place project switching. The attached *server.Client has its
// own equivalent routing fixed at attach time.
type AdapterScope struct {
	svc  AdapterService
	mu   sync.Mutex
	path string
}

// NewAdapterScope creates a scope bound to svc, seeded with projectPath.
func NewAdapterScope(svc AdapterService, projectPath string) *AdapterScope {
	return &AdapterScope{svc: svc, path: projectPath}
}

// SetProjectPath changes the adapter-local project path.
func (s *AdapterScope) SetProjectPath(path string) {
	s.mu.Lock()
	s.path = path
	s.mu.Unlock()
}

// ProjectPath returns the current adapter-local project path.
func (s *AdapterScope) ProjectPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// ProjectName returns the basename of the adapter-local project directory.
func (s *AdapterScope) ProjectName() string {
	return filepath.Base(s.ProjectPath())
}

// ProjectRoot returns the adapter-local project root.
func (s *AdapterScope) ProjectRoot() string {
	return s.ProjectPath()
}

// ProjectCurrent returns the project record for the adapter-local project.
func (s *AdapterScope) ProjectCurrent() ProjectSummary {
	out, _ := s.svc.ProjectCurrentForPath(s.ProjectPath())
	return out
}

// SessionList returns sessions for the adapter-local project.
func (s *AdapterScope) SessionList(state string) ([]SessionSummary, error) {
	return s.svc.SessionListForProjectPath(s.ProjectPath(), state)
}

// NewSession creates a session in the adapter-local project.
func (s *AdapterScope) NewSession(agentType string) (string, error) {
	return s.svc.NewSessionForProjectPath(s.ProjectPath(), agentType)
}

// ReadFileContent reads a file scoped to the adapter-local project.
func (s *AdapterScope) ReadFileContent(path string) (string, error) {
	return s.svc.ReadFileContentForProjectPath(s.ProjectPath(), path)
}

// OpenOrCreateSession opens the most recent active session for the given
// project path, or creates a new one if none exists.
func (s *AdapterScope) OpenOrCreateSession(projectPath string) (SessionSummary, error) {
	sessions, err := s.svc.SessionListForProjectPath(projectPath, "active")
	if err != nil {
		return SessionSummary{}, err
	}
	// Scan newest-first and skip a candidate another process is driving, so a
	// contended session does not fail the whole open. Any other open failure
	// surfaces: this is user-initiated, so a corrupt session must not be
	// skipped silently.
	for _, session := range sessions {
		summary, err := s.svc.OpenSession(session.ID)
		if err == nil {
			return summary, nil
		}
		if !errors.Is(err, snapshot.ErrSessionContended) {
			return SessionSummary{}, err
		}
	}
	id, err := s.svc.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		return SessionSummary{}, err
	}
	return s.svc.SessionSummaryForSessionOrPersisted(id)
}

// SessionView owns an adapter-local current-session pointer and the
// event-acceptance rules shared by the in-process adapters.
type SessionView struct {
	svc AdapterService

	mu      sync.Mutex
	current string
	// readOnly holds the id of the routing-current session that was opened
	// read-only because another process holds its claim. The two states are
	// explicit: SetCurrent clears it on every commit, and only the read-only
	// open path sets it, after committing the id it names — so a successful
	// live commit of the same session can never leave it stale.
	readOnly string
	children map[string]struct{}
}

func NewSessionView(svc AdapterService) *SessionView {
	return &SessionView{svc: svc}
}

func (v *SessionView) SetCurrent(id string) {
	v.mu.Lock()
	id = strings.TrimSpace(id)
	if v.current != id {
		v.children = nil
	}
	// Every routing commit clears the read-only marker unconditionally: only
	// the read-only open path sets it, after committing the id it names, so a
	// successful live commit of the same session can never leave it stale.
	v.readOnly = ""
	v.current = id
	v.mu.Unlock()
}

// SetReadOnly records that the routing current was opened read-only because
// another process holds the session's claim. SetCurrent clears the marker on
// every commit, so it must be set after committing the id it names.
func (v *SessionView) SetReadOnly(id string) {
	v.mu.Lock()
	v.readOnly = strings.TrimSpace(id)
	v.mu.Unlock()
}

// IsReadOnly reports whether the given id is the read-only-marked session.
func (v *SessionView) IsReadOnly(id string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.readOnly != "" && v.readOnly == strings.TrimSpace(id)
}

// LiveCurrentOrErr is LiveCurrent for a caller that must distinguish why no
// live session is current: the read-only-marked session is current, so another
// process drives it, or there is no current session at all.
func (v *SessionView) LiveCurrentOrErr() (string, error) {
	id := v.LiveCurrent()
	if id != "" {
		return id, nil
	}
	v.mu.Lock()
	readOnly := v.readOnly
	current := strings.TrimSpace(v.current)
	v.mu.Unlock()
	if readOnly != "" && readOnly == current {
		return "", SessionContendedError(current)
	}
	return "", fmt.Errorf("no current session")
}

// clearIfCurrent clears the current pointer only when it still equals id, so a
// stale validation cannot clear a session another goroutine has since selected.
func (v *SessionView) clearIfCurrent(id string) {
	v.mu.Lock()
	if v.current == strings.TrimSpace(id) {
		v.current = ""
		v.children = nil
	}
	v.mu.Unlock()
}

func (v *SessionView) Current() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return strings.TrimSpace(v.current)
}

func (v *SessionView) CurrentOrErr() (string, error) {
	id := v.Current()
	if id == "" {
		return "", fmt.Errorf("no current session")
	}
	return id, nil
}

func (v *SessionView) Resolve(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id != "" {
		return id, nil
	}
	return v.CurrentOrErr()
}

func (v *SessionView) LiveCurrent() string {
	v.mu.Lock()
	id := strings.TrimSpace(v.current)
	readOnly := v.readOnly == id
	v.mu.Unlock()
	if id == "" {
		return ""
	}
	if v.svc == nil {
		return id
	}
	if readOnly {
		// The routing current was opened read-only because another process
		// drives it; it stays routed, just not live here.
		return ""
	}
	if _, err := v.svc.SessionSummaryForSession(id); err != nil {
		v.clearIfCurrent(id)
		return ""
	}
	return id
}

func (v *SessionView) AcceptsSessionEvent(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return true
	}
	return v.LiveCurrent() == sessionID
}

func (v *SessionView) AcceptsEvent(ev Event) bool {
	if ev.SubagentSessionID != "" {
		return v.acceptsSubagentEvent(ev)
	}
	if ev.SessionID != "" {
		return v.AcceptsSessionEvent(ev.SessionID)
	}
	return true
}

// AcceptsEventForCurrent decides event delivery against an explicit current
// session id without querying the owner. A single-goroutine consumer that has
// already fixed its presentation-current id calls this while owner state locks
// may be held, so it must never probe liveness or take an owner lock. Global
// (untagged) events always pass; a session-tagged event passes only for the
// given current; subagent events pass through the children set.
func (v *SessionView) AcceptsEventForCurrent(current string, ev Event) bool {
	current = strings.TrimSpace(current)
	if ev.SubagentSessionID != "" {
		if current == "" {
			return false
		}
		return v.AcceptsSubagentEventForCurrent(current, ev)
	}
	if sessionID := strings.TrimSpace(ev.SessionID); sessionID != "" {
		return sessionID == current
	}
	return true
}

func (v *SessionView) acceptsSubagentEvent(ev Event) bool {
	current := v.LiveCurrent()
	if current == "" {
		return false
	}
	return v.AcceptsSubagentEventForCurrent(current, ev)
}

func (v *SessionView) AcceptsSubagentEvent(ev Event) bool {
	return v.acceptsSubagentEvent(ev)
}

func (v *SessionView) AcceptsSubagentEventForCurrent(current string, ev Event) bool {
	child := strings.TrimSpace(ev.SubagentSessionID)
	if child == "" {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if strings.TrimSpace(v.current) != current {
		return false
	}
	if ev.ParentSessionID == current {
		if v.children == nil {
			v.children = make(map[string]struct{})
		}
		v.children[child] = struct{}{}
		return true
	}
	_, ok := v.children[child]
	return ok
}

func (v *SessionView) CurrentSummary() SessionSummary {
	id := v.Current()
	if id == "" {
		return SessionSummary{}
	}
	s, err := v.svc.SessionSummaryForSessionOrPersisted(id)
	if err != nil {
		v.SetCurrent("")
		return SessionSummary{}
	}
	return s
}

func (v *SessionView) TokenUsage() TokenReport {
	id := v.Current()
	if id == "" {
		return TokenReport{}
	}
	report, err := v.svc.TokenUsageForSession(id)
	if err != nil {
		return TokenReport{}
	}
	return report
}

// RemovedCurrent clears the pointer when the removed id was the adapter's
// current session and reports whether the adapter should refresh its view.
func (v *SessionView) RemovedCurrent(id string) bool {
	wasCurrent := v.Current() == strings.TrimSpace(id)
	if wasCurrent {
		v.SetCurrent("")
	}
	return wasCurrent
}

func (v *SessionView) SessionChangedPayload(sessionID string) SessionPayload {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return SessionPayload{}
	}
	payload, err := v.svc.SessionPayloadForSession(sessionID)
	if err != nil {
		v.clearIfCurrent(sessionID)
		return SessionPayload{}
	}
	return payload
}
