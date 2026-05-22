package tool

import (
	"fmt"
	"os"
)

type nonRegularTargetError struct {
	Path string
	Mode os.FileMode
}

func (e *nonRegularTargetError) Error() string {
	return fmt.Sprintf("non-regular file target: %s (mode %s)", e.Path, e.Mode)
}

func ensureRegularExistingTarget(absPath string) (bool, error) {
	info, err := os.Lstat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", absPath, err)
	}
	if err := ensureRegularFileInfo(absPath, info); err != nil {
		return true, err
	}
	return true, nil
}

func ensureRegularFileInfo(absPath string, info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() {
		mode := os.FileMode(0)
		if info != nil {
			mode = info.Mode()
		}
		return &nonRegularTargetError{Path: absPath, Mode: mode}
	}
	return nil
}
