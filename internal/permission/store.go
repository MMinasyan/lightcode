package permission

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// SaveLocal writes to the per-project permissions file, merging new
// patterns into the existing file (append-only, no duplicates).
func SaveLocal(projectsRoot, projectID string, add Rules) error {
	existing, err := LoadLocal(projectsRoot, projectID)
	if err != nil {
		return err
	}
	existing.Allow = mergeUnique(existing.Allow, add.Allow)
	existing.Deny = mergeUnique(existing.Deny, add.Deny)
	existing.Ask = mergeUnique(existing.Ask, add.Ask)
	return writeLocalJSON(localPath(projectsRoot, projectID), existing)
}

func localPath(projectsRoot, projectID string) string {
	return filepath.Join(projectsRoot, projectID, "permissions.json")
}

func writeLocalJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
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
