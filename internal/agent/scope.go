package agent

import (
	"path/filepath"
	"sync"
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
	if err == nil && len(sessions) > 0 {
		return s.svc.OpenSession(sessions[0].ID)
	}
	id, err := s.svc.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		return SessionSummary{}, err
	}
	return s.svc.SessionSummaryForSession(id)
}
