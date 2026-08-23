package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// SessionInfo is the summary returned by List for the session switcher.
type SessionInfo struct {
	ID              string `json:"id"`
	CreatedAt       string `json:"createdAt"`
	LastActivity    int64  `json:"lastActivity"`
	State           string `json:"state"`
	ArchivedAt      int64  `json:"archivedAt"`
	ProjectPath     string `json:"projectPath"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	ActiveAgentType string `json:"activeAgentType,omitempty"`
}

// LifecycleConfig controls Sweep's archive/delete thresholds. Days are
// counted in 24-hour units.
type LifecycleConfig struct {
	Enabled                bool
	ArchiveAfterDays       int
	DeleteAfterArchiveDays int
}

// List returns sessions under root, filtered to projectPath (empty =
// no project filter) and state (empty = any state). Sorted by
// LastActivity descending.
func List(root, projectPath, state string) ([]SessionInfo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapshot: list sessions: %w", err)
	}
	var out []SessionInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var meta SessionMeta
		if err := readJSON(filepath.Join(root, e.Name(), "meta.json"), &meta); err != nil {
			continue
		}
		// Identity comes from the directory: a record whose id is not its own
		// directory's name is not that session, so it is skipped rather than
		// listed under the id it declares.
		if meta.ID != e.Name() {
			continue
		}
		if projectPath != "" && meta.ProjectPath != projectPath {
			continue
		}
		if state != "" {
			// Treat empty State (old sessions) as active.
			effective := meta.State
			if effective == "" {
				effective = StateActive
			}
			if effective != state {
				continue
			}
		}
		out = append(out, SessionInfo{
			ID:              meta.ID,
			CreatedAt:       meta.CreatedAt,
			LastActivity:    meta.LastActivity,
			State:           effectiveState(meta.State),
			ArchivedAt:      meta.ArchivedAt,
			ProjectPath:     meta.ProjectPath,
			ParentSessionID: meta.ParentSessionID,
			ActiveAgentType: meta.ActiveAgentType,
		})
	}
	sortByActivityDesc(out)
	return out, nil
}

// LoadSessionMeta reads a session's persisted metadata without opening a Store.
func LoadSessionMeta(root, id string) (SessionMeta, error) {
	if err := ValidateSessionID(id); err != nil {
		return SessionMeta{}, err
	}
	var meta SessionMeta
	if err := readJSON(filepath.Join(root, id, "meta.json"), &meta); err != nil {
		return SessionMeta{}, err
	}
	return meta, nil
}

// LoadMostRecent returns the most recently active session for projectPath,
// restricted to state == active. Returns ("", nil) if none.
func LoadMostRecent(root, projectPath string) (string, error) {
	infos, err := List(root, projectPath, StateActive)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", nil
	}
	for _, info := range infos {
		if info.ParentSessionID == "" {
			return info.ID, nil
		}
	}
	return "", nil
}

// CandidateSerializer, when non-nil, serializes each sweep candidate against
// foreground lifecycle operations and carries owner-close admission: it is
// invoked before a candidate's metadata is read, returns (release, admitted),
// and the release is called after the candidate is processed. A refused
// candidate (admitted == false) is skipped entirely — no metadata read, no
// claim, no archive write, and no delete — which is how the owner closes sweep
// admission at the same boundary as every other lifecycle operation. The owner
// passes a hook that holds the lifecycle lock per candidate, so a foreground
// op and a sweep candidate on the same id never interleave, without blocking
// foreground ops for the whole sweep.
type CandidateSerializer func() (release func(), admitted bool)

// SweepAllProjects runs Sweep over every project's sessions/ dir under
// projectsRoot. Returns combined counts across all projects.
func SweepAllProjects(projectsRoot string, cfg LifecycleConfig, onDelete func(sessionID string), serialize CandidateSerializer) (int, int, error) {
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("snapshot: sweep projects: %w", err)
	}
	archived, deleted := 0, 0
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		a, d, err := Sweep(projectsRoot, e.Name(), cfg, onDelete, serialize)
		archived += a
		deleted += d
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return archived, deleted, firstErr
}

// Sweep walks one project's session dirs and applies state transitions per
// cfg. Every candidate passes the lifecycle serializer (if any) before its
// metadata is read — a close-first refusal performs no metadata read — and an
// admitted candidate is processed under the serializer and its session claim,
// so a session driven by any live process is skipped and no foreground op
// interleaves.
func Sweep(projectsRoot, projectID string, cfg LifecycleConfig, onDelete func(sessionID string), serialize CandidateSerializer) (int, int, error) {
	if !cfg.Enabled {
		return 0, 0, nil
	}
	root := filepath.Join(projectsRoot, projectID, "sessions")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("snapshot: sweep: %w", err)
	}
	now := time.Now().Unix()
	archiveCutoff := int64(cfg.ArchiveAfterDays) * 86400
	deleteCutoff := int64(cfg.DeleteAfterArchiveDays) * 86400
	archived, deleted := 0, 0
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sessionID := e.Name()
		if ValidateSessionID(sessionID) != nil {
			continue
		}
		result := sweepCandidate(projectsRoot, projectID, root, sessionID, now, archiveCutoff, deleteCutoff, onDelete, serialize)
		archived += result.archived
		deleted += result.deleted
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
	}
	return archived, deleted, firstErr
}

type sweepResult struct {
	archived int
	deleted  int
	err      error
}

// sweepCandidate processes one candidate. The lifecycle serializer (if any)
// runs before the candidate's metadata is read, so a close-first refusal
// performs no metadata read, claim, archive write, or delete — it cannot even
// block on the candidate's meta. For an admitted candidate the release stays
// deferred until the candidate completes. Eligibility is then pre-filtered
// lock-free and rechecked under the claim since state is only stable once
// held.
func sweepCandidate(projectsRoot, projectID, root, sessionID string, now, archiveCutoff, deleteCutoff int64, onDelete func(sessionID string), serialize CandidateSerializer) sweepResult {
	metaPath := filepath.Join(root, sessionID, "meta.json")
	if serialize != nil {
		release, admitted := serialize()
		if !admitted {
			return sweepResult{}
		}
		defer release()
	}
	var meta SessionMeta
	if readJSON(metaPath, &meta) != nil {
		return sweepResult{}
	}
	if !sweepEligible(meta, now, archiveCutoff, deleteCutoff) {
		return sweepResult{}
	}
	claim, ok, err := AcquireSessionClaim(projectsRoot, projectID, sessionID)
	if err != nil || !ok {
		return sweepResult{}
	}
	// ReleaseSessionClaim reports a failed release to stderr; the sweep's
	// return shape has no error to carry it in, so it is discarded here.
	defer func() { _ = ReleaseSessionClaim(claim, sessionID) }()
	if readJSON(metaPath, &meta) != nil {
		return sweepResult{}
	}
	switch effectiveState(meta.State) {
	case StateActive:
		if archiveCutoff > 0 && meta.LastActivity > 0 && now-meta.LastActivity > archiveCutoff {
			err := ArchiveSession(root, sessionID)
			var committed *CommittedMutationError
			if err == nil || errors.As(err, &committed) {
				return sweepResult{archived: 1, err: err}
			}
			return sweepResult{err: err}
		}
	case StateArchived:
		if deleteCutoff > 0 && meta.ArchivedAt > 0 && now-meta.ArchivedAt > deleteCutoff {
			// Commit the durable delete first; only then run post-commit cleanup
			// so a rename failure never loses summaries for a still-listed session.
			err := DeleteSession(root, sessionID)
			var committed *CommittedMutationError
			if err == nil || errors.As(err, &committed) {
				if onDelete != nil {
					onDelete(sessionID)
				}
				return sweepResult{deleted: 1, err: err}
			}
			return sweepResult{err: err}
		}
	}
	return sweepResult{}
}

func sweepEligible(meta SessionMeta, now, archiveCutoff, deleteCutoff int64) bool {
	switch effectiveState(meta.State) {
	case StateActive:
		return archiveCutoff > 0 && meta.LastActivity > 0 && now-meta.LastActivity > archiveCutoff
	case StateArchived:
		return deleteCutoff > 0 && meta.ArchivedAt > 0 && now-meta.ArchivedAt > deleteCutoff
	}
	return false
}

// ArchiveSession flips a session's state to archived on disk, same effect
// as the sweep's active→archived branch. No-op if already archived.
func ArchiveSession(sessionsRoot, id string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	dir := filepath.Join(sessionsRoot, id)
	metaPath := filepath.Join(dir, "meta.json")
	var meta SessionMeta
	if err := readJSON(metaPath, &meta); err != nil {
		return err
	}
	if effectiveState(meta.State) == StateArchived {
		return nil
	}
	meta.State = StateArchived
	meta.ArchivedAt = time.Now().Unix()
	if err := writeJSON(metaPath, meta); err != nil {
		return err
	}
	if err := atomicfs.SyncDir(filepath.Dir(metaPath)); err != nil {
		return &CommittedMutationError{Err: fmt.Errorf("snapshot: sync archive metadata parent: %w", err)}
	}
	return nil
}

// DeleteSession removes a session. The durable commit is an atomic rename of
// sessions/<id> into the project's unlisted .deleting namespace: after it, the
// session is gone from every listing and cannot reappear. Recursive removal of
// the renamed directory is best-effort post-commit cleanup — a leftover under
// .deleting is ignored, not an error — so a partial removal never destroys a
// listed session or converts the committed delete into a failure.
func DeleteSession(sessionsRoot, id string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	nonce, err := newSessionID()
	if err != nil {
		return fmt.Errorf("snapshot: delete nonce: %w", err)
	}
	deletingRoot := filepath.Join(filepath.Dir(sessionsRoot), ".deleting", "sessions")
	if err := os.MkdirAll(deletingRoot, 0o700); err != nil {
		return fmt.Errorf("snapshot: delete staging: %w", err)
	}
	src := filepath.Join(sessionsRoot, id)
	dst := filepath.Join(deletingRoot, id+"-"+nonce)
	if err := os.Rename(src, dst); err != nil {
		// Rename ENOENT is ambiguous (source gone vs destination parent gone);
		// only a missing source is the idempotent already-deleted case.
		if os.IsNotExist(err) {
			if _, statErr := os.Lstat(src); os.IsNotExist(statErr) {
				return nil
			}
		}
		return fmt.Errorf("snapshot: delete %s: %w", id, err)
	}
	var syncErr error
	if err := atomicfs.SyncDir(sessionsRoot); err != nil {
		syncErr = fmt.Errorf("snapshot: sync delete source parent: %w", err)
	}
	if err := atomicfs.SyncDir(filepath.Dir(dst)); err != nil && syncErr == nil {
		syncErr = fmt.Errorf("snapshot: sync delete destination parent: %w", err)
	}
	// Post-commit cleanup only; failure leaves an ignored orphan under .deleting.
	_ = os.RemoveAll(dst)
	if syncErr != nil {
		return &CommittedMutationError{Err: syncErr}
	}
	return nil
}

// NewStagingSessionsRoot allocates a fresh unlisted staging sessions root for a
// candidate under the same project as sessionsRoot: a sibling of the sessions
// directory. Session enumeration skips the .staging namespace, so a candidate
// prepared there is invisible until an atomic rename publishes it. The nonce
// disambiguates concurrent candidates.
func NewStagingSessionsRoot(sessionsRoot string) (string, error) {
	nonce, err := newSessionID()
	if err != nil {
		return "", fmt.Errorf("snapshot: staging nonce: %w", err)
	}
	return filepath.Join(filepath.Dir(sessionsRoot), ".staging", "sessions", nonce), nil
}

// mintPublishHook fires between a staged candidate's mint and its publish
// (the atomic rename), while the candidate is reserved on disk under staging
// but not yet visible to session enumeration. Production no-op; tests use it
// to hold a candidate unpublished while another mint runs. Follows the
// forkIntoLockReleasedHook precedent.
var mintPublishHook = func() {}

// PublishStagedSession atomically publishes a staged session into the real
// sessions namespace by renaming <stagingRoot>/<id> to <finalSessionsRoot>/<id>.
// The rename is the single durable commit; the caller relocates the candidate
// store's cached paths and removes the now-empty staging parent afterward.
func PublishStagedSession(stagingRoot, finalSessionsRoot, id string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	mintPublishHook()
	if err := os.MkdirAll(finalSessionsRoot, 0o700); err != nil {
		return fmt.Errorf("snapshot: publish staged: %w", err)
	}
	candidateDir := filepath.Join(stagingRoot, id)
	if err := syncCandidateTree(candidateDir); err != nil {
		return err
	}
	if err := os.Rename(candidateDir, filepath.Join(finalSessionsRoot, id)); err != nil {
		return fmt.Errorf("snapshot: publish staged %s: %w", id, err)
	}
	var syncErr error
	if err := atomicfs.SyncDir(stagingRoot); err != nil {
		syncErr = fmt.Errorf("snapshot: sync staging parent: %w", err)
	}
	if err := atomicfs.SyncDir(finalSessionsRoot); err != nil && syncErr == nil {
		syncErr = fmt.Errorf("snapshot: sync sessions root: %w", err)
	}
	if syncErr != nil {
		return &CommittedMutationError{Err: syncErr}
	}
	return nil
}

func syncCandidateTree(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("snapshot: sync candidate tree: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := syncCandidateTree(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	if err := atomicfs.SyncDir(root); err != nil {
		return fmt.Errorf("snapshot: sync candidate directory %s: %w", root, err)
	}
	return nil
}

// cleanupStagingRoot removes a staging directory after a preparation failure,
// joining any cleanup error with the original cause so neither is lost.
func cleanupStagingRoot(stagingRoot string, cause error) error {
	if err := os.RemoveAll(stagingRoot); err != nil {
		return errors.Join(cause, fmt.Errorf("snapshot: staging cleanup: %w", err))
	}
	return cause
}

func effectiveState(s string) string {
	if s == "" {
		return StateActive
	}
	return s
}

func sortByActivityDesc(infos []SessionInfo) {
	for i := 1; i < len(infos); i++ {
		for j := i; j > 0 && infos[j].LastActivity > infos[j-1].LastActivity; j-- {
			infos[j], infos[j-1] = infos[j-1], infos[j]
		}
	}
}
