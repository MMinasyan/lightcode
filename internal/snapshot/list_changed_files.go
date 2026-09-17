package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ListChangedFiles returns the deduplicated, sorted canonical paths of every
// file recorded in the session's target code groups under
// DataDir/code/<sessionID>. The session ID must have the durable identity
// shape — exactly 32 lowercase hexadecimal characters — and the same shape
// check skips every child that is not a valid group directory (including the
// command-output spill directory), every symlink, and every non-directory.
// Entries whose metadata carries no CanonicalPath are skipped: target groups
// require CanonicalPath and there is no legacy-path fallback in this
// namespace. An absent session directory is empty success; any other
// enumeration failure, and any ListTurns error, discards the complete result
// and returns nil with the error. The helper is read-only: it creates no
// groups and reads no conversation or project data.
func ListChangedFiles(dataDir, sessionID string) ([]string, error) {
	if !isDurableID(sessionID) {
		return nil, fmt.Errorf("snapshot: invalid session id %q", sessionID)
	}
	sessionDir := filepath.Join(dataDir, "code", sessionID)
	children, err := os.ReadDir(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	seen := make(map[string]bool)
	var paths []string
	for _, child := range children {
		// ReadDir reports Lstat info, so IsDir is already false for symlinks:
		// this one check skips symlinks and non-directories alike.
		if !child.IsDir() {
			continue
		}
		if !isDurableID(child.Name()) {
			continue
		}
		store, err := OpenCodeStore(filepath.Join(sessionDir, child.Name()))
		if err != nil {
			return nil, err
		}
		turns, err := store.ListTurns()
		if err != nil {
			return nil, err
		}
		for _, turn := range turns {
			for _, file := range turn.Files {
				if file.CanonicalPath == "" || seen[file.CanonicalPath] {
					continue
				}
				seen[file.CanonicalPath] = true
				paths = append(paths, file.CanonicalPath)
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// isDurableID reports whether s has the durable identity shape: exactly 32
// lowercase hexadecimal characters. It mirrors the harness identity
// validator's shape — this package cannot import the harness.
func isDurableID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
