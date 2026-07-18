package permission

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// LoadLocal reads the per-project permissions file.
// Returns empty Rules (not an error) if the file doesn't exist.
func LoadLocal(projectsRoot, projectID string) (Rules, error) {
	if projectsRoot == "" || projectID == "" {
		return Rules{}, nil
	}
	p := localPath(projectsRoot, projectID)
	var r Rules
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Rules{}, nil
		}
		return Rules{}, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Rules{}, err
	}
	return r, nil
}

// SaveLocal merges new patterns into the per-project permissions file
// (append-only, no duplicates). The read-merge-write cycle runs under the
// project permissions lock, so concurrent savers each observe the previous
// committed file and no rule is lost.
func SaveLocal(projectsRoot, projectID string, add Rules) error {
	if projectsRoot == "" || projectID == "" {
		return nil
	}
	return atomicfs.WithLock(lockPath(projectsRoot, projectID), func() error {
		existing, err := LoadLocal(projectsRoot, projectID)
		if err != nil {
			return err
		}
		existing.Allow = mergeUnique(existing.Allow, add.Allow)
		existing.Deny = mergeUnique(existing.Deny, add.Deny)
		existing.Ask = mergeUnique(existing.Ask, add.Ask)
		return writeLocalJSON(localPath(projectsRoot, projectID), existing)
	})
}

func localPath(projectsRoot, projectID string) string {
	return filepath.Join(projectsRoot, projectID, "permissions.json")
}

func lockPath(projectsRoot, projectID string) string {
	return filepath.Join(projectsRoot, projectID, ".locks", "permissions.lock")
}

func writeLocalJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicfs.Write(path, data, 0o600)
}

func mergeUnique(existing, add []string) []string {
	set := make(map[string]struct{}, len(existing))
	for _, s := range existing {
		set[s] = struct{}{}
	}
	for _, s := range add {
		if _, ok := set[s]; !ok {
			existing = append(existing, s)
			set[s] = struct{}{}
		}
	}
	return existing
}
