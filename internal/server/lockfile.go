package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile is written to disk so clients can discover the running owner.
type LockFile struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// Path returns the owner lockfile path.
func Path(home string) string {
	return filepath.Join(home, ".lightcode", "owner.lock")
}

// Write persists a lockfile to disk, creating parent dirs as needed.
func Write(home string, lf LockFile) error {
	p := Path(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("lockfile: mkdir: %w", err)
	}
	data, err := json.Marshal(lf)
	if err != nil {
		return fmt.Errorf("lockfile: marshal: %w", err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		_ = os.Remove(p)
		return err
	}
	return nil
}

// Read loads a lockfile from disk.
func Read(home string) (LockFile, error) {
	data, err := os.ReadFile(Path(home))
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
func Remove(home string) error {
	return os.Remove(Path(home))
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
