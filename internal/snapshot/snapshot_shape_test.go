package snapshot

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSnapshotResolvedRejectsNonRegularExistingTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(t *testing.T) string
	}{
		{
			name: "directory",
			target: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "target-dir")
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
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
			store := newTestStore(t)
			target := tc.target(t)
			turn := store.BeginTurn()

			err := runSnapshotShapeWithTimeout(func() error {
				return store.SnapshotResolved(turn, target, target)
			})
			if err == nil {
				t.Fatalf("SnapshotResolved(%s) succeeded, want non-regular target refusal", tc.name)
			}
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "did not fail closed before blocking") {
				t.Fatalf("SnapshotResolved(%s) error = %v, want refusal before blocking on target I/O", tc.name, err)
			}
			if !strings.Contains(msg, "non-regular") {
				t.Fatalf("SnapshotResolved(%s) error = %v, want explicit non-regular target refusal", tc.name, err)
			}

			entries, readErr := os.ReadDir(filepath.Join(store.snapshotsDir, "1"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("snapshot entries = %d, want none for refused %s", len(entries), tc.name)
			}
			if info, statErr := os.Lstat(target); statErr != nil {
				t.Fatalf("lstat %s = %v, want target preserved", target, statErr)
			} else if info.Mode().IsRegular() {
				t.Fatalf("%s became regular after refused snapshot", target)
			}
		})
	}
}

func TestSnapshotResolvedStillAllowsMissingAndRegularTargets(t *testing.T) {
	store := newTestStore(t)
	turn := store.BeginTurn()

	missing := filepath.Join(t.TempDir(), "new-file.txt")
	if err := store.SnapshotResolved(turn, missing, missing); err != nil {
		t.Fatalf("SnapshotResolved missing target error = %v", err)
	}

	regular := filepath.Join(t.TempDir(), "regular.txt")
	if err := os.WriteFile(regular, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.SnapshotResolved(turn, regular, regular); err != nil {
		t.Fatalf("SnapshotResolved regular target error = %v", err)
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
