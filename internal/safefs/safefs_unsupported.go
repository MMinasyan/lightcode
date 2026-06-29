//go:build !linux

package safefs

import (
	"errors"
	"fmt"
	"os"
)

var (
	ErrNonRegular = errors.New("safefs: non-regular target")
	ErrHardlink   = errors.New("safefs: hardlinked target")
)

func OpenExisting(path string, flag int) (*os.File, error) {
	return nil, unsupported(path)
}

func OpenForWrite(path string, perm os.FileMode) (*os.File, bool, error) {
	return nil, false, unsupported(path)
}

func RemoveLeaf(path string) error {
	return unsupported(path)
}

func unsupported(path string) error {
	return fmt.Errorf("safefs: protected filesystem operations unsupported on this platform: %s", path)
}
