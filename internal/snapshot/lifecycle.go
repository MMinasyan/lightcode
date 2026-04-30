package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionInfo is the summary returned by List for the session switcher.
type SessionInfo struct {
	ID           string `json:"id"`
	CreatedAt    string `json:"createdAt"`
	LastActivity int64  `json:"lastActivity"`
	State        string `json:"state"`
	ArchivedAt   int64  `json:"archivedAt"`
	ProjectPath  string `json:"projectPath"`
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
			ID:           meta.ID,
			CreatedAt:    meta.CreatedAt,
			LastActivity: meta.LastActivity,
			State:        effectiveState(meta.State),
			ArchivedAt:   meta.ArchivedAt,
			ProjectPath:  meta.ProjectPath,
		})
	}
	sortByActivityDesc(out)
	return out, nil
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
	return infos[0].ID, nil
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
		if !e.IsDir() {
			continue
		}
		sessionsRoot := filepath.Join(projectsRoot, e.Name(), "sessions")
		a, d, err := Sweep(sessionsRoot, cfg, onDelete)
		if err != nil {
			continue
		}
		archived += a
		deleted += d
	}
	return archived, deleted, nil
}

// Sweep walks every session dir and applies state transitions per cfg.
// Returns (archived, deleted) counts.
func Sweep(root string, cfg LifecycleConfig, onDelete func(sessionID string)) (int, int, error) {
	if !cfg.Enabled {
		return 0, 0, nil
	}
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
		dir := filepath.Join(root, e.Name())
		metaPath := filepath.Join(dir, "meta.json")
		var meta SessionMeta
		if err := readJSON(metaPath, &meta); err != nil {
			continue
		}
		state := effectiveState(meta.State)
		switch state {
		case StateActive:
			if archiveCutoff > 0 && meta.LastActivity > 0 && now-meta.LastActivity > archiveCutoff {
				meta.State = StateArchived
				meta.ArchivedAt = now
				if err := writeJSON(metaPath, meta); err == nil {
					archived++
				}
			}
		case StateArchived:
			if deleteCutoff > 0 && meta.ArchivedAt > 0 && now-meta.ArchivedAt > deleteCutoff {
				if onDelete != nil {
					onDelete(e.Name())
				}
				if err := os.RemoveAll(dir); err == nil {
					deleted++
				}
			}
		}
	}
	return archived, deleted, nil
}

// ArchiveSession flips a session's state to archived on disk, same effect
// as the sweep's active→archived branch. No-op if already archived.
func ArchiveSession(sessionsRoot, id string) error {
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
