package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile is written to disk so clients can discover a running daemon.
type LockFile struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// Path returns the lockfile path for the given project.
func Path(home, projectID string) string {
	return filepath.Join(home, ".lightcode", "daemon", projectID+".lock")
}

// Write persists a lockfile to disk, creating parent dirs as needed.
func Write(home, projectID string, lf LockFile) error {
	p := Path(home, projectID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("lockfile: mkdir: %w", err)
	}
	data, err := json.Marshal(lf)
	if err != nil {
		return fmt.Errorf("lockfile: marshal: %w", err)
	}
	return os.WriteFile(p, data, 0o600)
}

// Read loads a lockfile from disk.
func Read(home, projectID string) (LockFile, error) {
	data, err := os.ReadFile(Path(home, projectID))
	if err != nil {
		return LockFile{}, err
	}
	var lf LockFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return LockFile{}, fmt.Errorf("lockfile: unmarshal: %w", err)
	}
	return lf, nil
}

// Remove deletes the lockfile from disk.
func Remove(home, projectID string) error {
	return os.Remove(Path(home, projectID))
}

// IsStale checks whether the PID in the lockfile is still alive.
func IsStale(lf LockFile) bool {
	proc, err := os.FindProcess(lf.PID)
	if err != nil {
		return true
	}
	err = proc.Signal(syscall.Signal(0))
	return err != nil
}
