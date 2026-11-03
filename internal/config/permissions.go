package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
)

// ProjectsDir returns the home-based projects inventory directory that holds
// one per-Workspace permission record directory per derived storage key. It
// reuses ConfigPath's single home/.lightcode resolution and appends projects.
func ProjectsDir() (string, error) {
	configPath, err := ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(configPath), "projects"), nil
}

// WorkspacePermissionDirID derives the deterministic storage directory ID for
// one Workspace: p- plus the full lowercase SHA-256 hex of the cleaned
// absolute path bytes, with no symlink or case rewriting. It is a storage key
// for the retained home-based permission location, not durable project
// identity, and derives from the lexical path alone with no project record.
func WorkspacePermissionDirID(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return "p-" + hex.EncodeToString(sum[:]), nil
}
