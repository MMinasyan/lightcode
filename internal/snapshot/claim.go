package snapshot

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// ErrSessionContended is returned when another live process already holds a
// session's claim, so this process must not drive it.
var ErrSessionContended = errors.New("snapshot: session is driven by another process")

// ValidateSessionID rejects any session id that is not a safe single path
// segment, so an untrusted id can never escape the sessions directory or the
// claim namespace. Every id-to-path boundary runs this one validator.
func ValidateSessionID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) || filepath.Base(id) != id {
		return fmt.Errorf("snapshot: invalid session id %q", id)
	}
	return nil
}

// SessionClaimPath is the flock sidecar that grants one process the exclusive
// right to drive a session:
// ~/.lightcode/projects/<project-id>/.locks/sessions/<session-id>.lock
func SessionClaimPath(projectsRoot, projectID, sessionID string) string {
	return filepath.Join(projectsRoot, projectID, ".locks", "sessions", sessionID+".lock")
}

// AcquireSessionClaim validates the id and attempts a non-blocking exclusive
// claim on the session. Contention — another live process already drives the
// session — returns (nil, false, nil), not an error. The caller releases the
// returned lock when the session is no longer driven; the OS releases it on
// process exit if the caller crashes.
func AcquireSessionClaim(projectsRoot, projectID, sessionID string) (*atomicfs.Lock, bool, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return nil, false, err
	}
	if projectsRoot == "" || projectID == "" {
		return nil, false, fmt.Errorf("snapshot: session claim requires project root and id")
	}
	return atomicfs.TryAcquire(SessionClaimPath(projectsRoot, projectID, sessionID))
}

// acquireClaimLocked claims sessionID for this process before a mutating load
// or new-session publication. It is a no-op for a store with no project
// context (a legacy/test store that never drives across processes). Contention
// returns ErrSessionContended. Caller holds s.mu.
func (s *Store) acquireClaimLocked(sessionID string) error {
	if s.projectsRoot == "" || s.projectID == "" {
		return nil
	}
	claim, ok, err := AcquireSessionClaim(s.projectsRoot, s.projectID, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionContended, sessionID)
	}
	s.claim = claim
	return nil
}

// releaseClaimLocked drops the session claim if held. Caller holds s.mu.
func (s *Store) releaseClaimLocked() {
	if s.claim != nil {
		_ = s.claim.Release()
		s.claim = nil
	}
}
