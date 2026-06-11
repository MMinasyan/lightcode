package selfupdate

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// Injectable seams for the interactive and privilege-dependent paths: CI
// never runs as root, chowns anything, or has a real TTY, so root-refusal,
// foreign-owner, and prompt tests inject these.
var (
	// IsTerminal reports whether stdin is a TTY.
	IsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

	// PromptInput is where confirmation answers are read from.
	PromptInput io.Reader = os.Stdin

	// FileOwner extracts the owning uid from a FileInfo.
	FileOwner = func(info os.FileInfo) (uid int, ok bool) {
		st, stOK := info.Sys().(*syscall.Stat_t)
		if !stOK {
			return 0, false
		}
		return int(st.Uid), true
	}
)

// DataDirState classifies ~/.lightcode for the purge gates.
type DataDirState int

const (
	DataDirAbsent DataDirState = iota
	DataDirSymlink
	DataDirNotDir
	DataDirNotOwned
	DataDirOwned
)

// ClassifyDataDir gates the purge target: only a real directory owned by
// the current euid may be removed. Lstat never follows a symlink, so a
// symlinked ~/.lightcode is seen as such, not as what it points to.
func ClassifyDataDir(path string) (DataDirState, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return DataDirAbsent, nil
	}
	if err != nil {
		return DataDirAbsent, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return DataDirSymlink, nil
	}
	if !info.IsDir() {
		return DataDirNotDir, nil
	}
	if uid, ok := FileOwner(info); !ok || uid != Geteuid() {
		return DataDirNotOwned, nil
	}
	return DataDirOwned, nil
}

// PurgeData removes the validated data directory. os.RemoveAll does not
// follow symlinks inside the tree.
func PurgeData(path string) error {
	return os.RemoveAll(path)
}

// SweepStaged removes leftover .lightcode.tmp.* staged files in dir — the
// litter a crashed upgrade may leave. Ownership-gated: each entry is
// Lstat'ed and removed only when it is a regular file owned by the current
// euid; symlinks and foreign-owned entries are left alone. Best-effort.
func SweepStaged(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".lightcode.tmp.") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if uid, ok := FileOwner(info); !ok || uid != Geteuid() {
			continue
		}
		_ = os.Remove(path)
	}
}
