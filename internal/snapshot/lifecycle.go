package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// SweepAllProjects runs Sweep over every project's sessions/ dir under
// projectsRoot. Returns combined counts across all projects.
func SweepAllProjects(projectsRoot string, cfg LifecycleConfig, onDelete func(sessionID string)) (int, int, error) {
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("snapshot: sweep projects: %w", err)
	}
	archived, deleted := 0, 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		a, d, err := Sweep(projectsRoot, e.Name(), cfg, onDelete)
		if err != nil {
			continue
		}
		archived += a
		deleted += d
	}
	return archived, deleted, nil
}

// Sweep walks one project's session dirs and applies state transitions per
// cfg. Each candidate is claimed before mutation: a session driven by any
// live process (this owner or another) is contended and skipped, and
// eligibility is rechecked under the claim since state is only stable once
// held. Returns (archived, deleted) counts.
func Sweep(projectsRoot, projectID string, cfg LifecycleConfig, onDelete func(sessionID string)) (int, int, error) {
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
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sessionID := e.Name()
		if ValidateSessionID(sessionID) != nil {
			continue
		}
		dir := filepath.Join(root, sessionID)
		metaPath := filepath.Join(dir, "meta.json")
		var meta SessionMeta
		if err := readJSON(metaPath, &meta); err != nil {
			continue
		}
		if !sweepEligible(meta, now, archiveCutoff, deleteCutoff) {
			continue
		}
		claim, ok, err := AcquireSessionClaim(projectsRoot, projectID, sessionID)
		if err != nil || !ok {
			continue
		}
		// Recheck under the claim; the holder may have changed state or the
		// session may no longer be eligible.
		if readJSON(metaPath, &meta) != nil {
			claim.Release()
			continue
		}
		switch effectiveState(meta.State) {
		case StateActive:
			if archiveCutoff > 0 && meta.LastActivity > 0 && now-meta.LastActivity > archiveCutoff {
				meta.State = StateArchived
				meta.ArchivedAt = now
				if writeJSON(metaPath, meta) == nil {
					archived++
				}
			}
		case StateArchived:
			if deleteCutoff > 0 && meta.ArchivedAt > 0 && now-meta.ArchivedAt > deleteCutoff {
				if onDelete != nil {
					onDelete(sessionID)
				}
				if os.RemoveAll(dir) == nil {
					deleted++
				}
			}
		}
		claim.Release()
	}
	return archived, deleted, nil
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
	return writeJSON(metaPath, meta)
}

// DeleteSession removes a session's dir entirely, same effect as the
// sweep's archived→deleted branch.
func DeleteSession(sessionsRoot, id string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	dir := filepath.Join(sessionsRoot, id)
	return os.RemoveAll(dir)
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
