package snapshot

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"golang.org/x/sys/unix"
)

func TestSnapshotResolvedEntryRejectsNonRegularExistingTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(t *testing.T) string
	}{
		{
			name: "directory",
			target: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "target-dir")
				testassert.NoError(t, os.Mkdir(path, 0o755))
				return path
			},
		},
		{
			name: "FIFO",
			target: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "target-fifo")
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Skipf("mkfifo unavailable: %v", err)
				}
				return path
			},
		},
		{
			name: "socket",
			target: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "target.sock")
				ln, err := net.Listen("unix", path)
				if err != nil {
					t.Skipf("unix socket unavailable: %v", err)
				}
				t.Cleanup(func() {
					_ = ln.Close()
				})
				return path
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestCodeGroup(t)
			target := tc.target(t)

			err := runSnapshotShapeWithTimeout(func() error {
				_, _, err := store.SnapshotResolvedEntry(target, target)
				return err
			})
			testassert.Require(t, err != nil, "SnapshotResolvedEntry(%s) succeeded, want non-regular target refusal", tc.name)
			msg := strings.ToLower(err.Error())
			testassert.Require(t, !strings.Contains(msg, "did not fail closed before blocking"), "SnapshotResolvedEntry(%s) error = %v, want refusal before blocking on target I/O", tc.name, err)
			testassert.Require(t, strings.Contains(msg, "non-regular"), "SnapshotResolvedEntry(%s) error = %v, want explicit non-regular target refusal", tc.name, err)

			entries, readErr := os.ReadDir(filepath.Join(store.snapshotsDir, "1"))
			testassert.Require(t, readErr == nil || errors.Is(readErr, os.ErrNotExist), "%v", readErr)
			testassert.Require(t, len(entries) == 0, "snapshot entries = %d, want none for refused %s", len(entries), tc.name)
			if info, statErr := os.Lstat(target); statErr != nil {
				t.Fatalf("lstat %s = %v, want target preserved", target, statErr)
			} else if info.Mode().IsRegular() {
				t.Fatalf("%s became regular after refused snapshot", target)
			}
		})
	}
}

func TestSnapshotResolvedEntryStillAllowsMissingAndRegularTargets(t *testing.T) {
	store := newTestCodeGroup(t)

	missing := filepath.Join(t.TempDir(), "new-file.txt")
	{
		_, _, err := store.SnapshotResolvedEntry(missing, missing)
		testassert.NoError(t, err)
	}

	regular := filepath.Join(t.TempDir(), "regular.txt")
	testassert.NoError(t, os.WriteFile(regular, []byte("before"), 0o644))
	{
		_, _, err := store.SnapshotResolvedEntry(regular, regular)
		testassert.NoError(t, err)
	}
}

func runSnapshotShapeWithTimeout(fn func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(500 * time.Millisecond):
		return fmt.Errorf("operation did not fail closed before blocking")
	}
}
