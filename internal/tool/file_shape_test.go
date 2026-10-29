package tool

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"golang.org/x/sys/unix"
)

func TestFileToolsRejectNonRegularExistingTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(t *testing.T) (path string, release func())
		run    func(path string) error
	}{
		{
			name: "read_directory",
			target: func(t *testing.T) (string, func()) {
				return nonRegularDirectoryTarget(t), func() {}
			},
			run: func(path string) error {
				_, err := execRead(t, filepath.Dir(path), "", config.ToolsConfig{ReadMaxLines: 500}, map[string]any{"path": path})
				return err
			},
		},
		{
			name: "read_FIFO",
			target: func(t *testing.T) (string, func()) {
				path := nonRegularFIFO(t)
				return path, startNonRegularFIFOFeed(t, path, "before")
			},
			run: func(path string) error {
				return runNonRegularTargetWithTimeout(func() error {
					_, err := execRead(t, filepath.Dir(path), "", config.ToolsConfig{ReadMaxLines: 500}, map[string]any{"path": path})
					return err
				})
			},
		},
		{
			name: "read_socket",
			target: func(t *testing.T) (string, func()) {
				return nonRegularUnixSocket(t), func() {}
			},
			run: func(path string) error {
				_, err := execRead(t, filepath.Dir(path), "", config.ToolsConfig{ReadMaxLines: 500}, map[string]any{"path": path})
				return err
			},
		},
		{
			name: "write_directory",
			target: func(t *testing.T) (string, func()) {
				return nonRegularDirectoryTarget(t), func() {}
			},
			run: func(path string) error {
				_, err := execWrite(t, filepath.Dir(path), "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "write_FIFO",
			target: func(t *testing.T) (string, func()) {
				return nonRegularFIFO(t), func() {}
			},
			run: func(path string) error {
				return runNonRegularTargetWithTimeout(func() error {
					_, err := execWrite(t, filepath.Dir(path), "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{"path": path, "content": "after"})
					return err
				})
			},
		},
		{
			name: "write_socket",
			target: func(t *testing.T) (string, func()) {
				return nonRegularUnixSocket(t), func() {}
			},
			run: func(path string) error {
				_, err := execWrite(t, filepath.Dir(path), "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "edit_directory",
			target: func(t *testing.T) (string, func()) {
				return nonRegularDirectoryTarget(t), func() {}
			},
			run: func(path string) error {
				_, err := execEdit(t, filepath.Dir(path), "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{
					"path":       path,
					"old_string": "before",
					"new_string": "after",
				})
				return err
			},
		},
		{
			name: "edit_FIFO",
			target: func(t *testing.T) (string, func()) {
				path := nonRegularFIFO(t)
				return path, startNonRegularFIFOFeed(t, path, "before")
			},
			run: func(path string) error {
				return runNonRegularTargetWithTimeout(func() error {
					_, err := execEdit(t, filepath.Dir(path), "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{
						"path":       path,
						"old_string": "before",
						"new_string": "after",
					})
					return err
				})
			},
		},
		{
			name: "edit_socket",
			target: func(t *testing.T) (string, func()) {
				return nonRegularUnixSocket(t), func() {}
			},
			run: func(path string) error {
				_, err := execEdit(t, filepath.Dir(path), "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{
					"path":       path,
					"old_string": "before",
					"new_string": "after",
				})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, release := tc.target(t)
			err := tc.run(path)
			release()
			assertNonRegularRefusal(t, path, err)
		})
	}
}

func TestNonRegularFileToolsDoNotSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(t *testing.T) (path string, release func())
		run    func(store *recordingCodeGroupStore, path string) error
	}{
		{
			name: "write_directory",
			target: func(t *testing.T) (string, func()) {
				return nonRegularDirectoryTarget(t), func() {}
			},
			run: func(store *recordingCodeGroupStore, path string) error {
				_, err := execWrite(t, filepath.Dir(path), "", "", CapabilityOptions{}, store, map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "write_FIFO",
			target: func(t *testing.T) (string, func()) {
				return nonRegularFIFO(t), func() {}
			},
			run: func(store *recordingCodeGroupStore, path string) error {
				return runNonRegularTargetWithTimeout(func() error {
					_, err := execWrite(t, filepath.Dir(path), "", "", CapabilityOptions{}, store, map[string]any{"path": path, "content": "after"})
					return err
				})
			},
		},
		{
			name: "edit_directory",
			target: func(t *testing.T) (string, func()) {
				return nonRegularDirectoryTarget(t), func() {}
			},
			run: func(store *recordingCodeGroupStore, path string) error {
				_, err := execEdit(t, filepath.Dir(path), "", "", CapabilityOptions{}, store, map[string]any{
					"path":       path,
					"old_string": "before",
					"new_string": "after",
				})
				return err
			},
		},
		{
			name: "edit_FIFO",
			target: func(t *testing.T) (string, func()) {
				path := nonRegularFIFO(t)
				return path, startNonRegularFIFOFeed(t, path, "before")
			},
			run: func(store *recordingCodeGroupStore, path string) error {
				return runNonRegularTargetWithTimeout(func() error {
					_, err := execEdit(t, filepath.Dir(path), "", "", CapabilityOptions{}, store, map[string]any{
						"path":       path,
						"old_string": "before",
						"new_string": "after",
					})
					return err
				})
			},
		},
		{
			name: "write_socket",
			target: func(t *testing.T) (string, func()) {
				return nonRegularUnixSocket(t), func() {}
			},
			run: func(store *recordingCodeGroupStore, path string) error {
				_, err := execWrite(t, filepath.Dir(path), "", "", CapabilityOptions{}, store, map[string]any{"path": path, "content": "after"})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newRecordingCodeGroupStore(t)
			path, release := tc.target(t)
			err := tc.run(store, path)
			release()
			assertNonRegularRefusal(t, path, err)
			testassert.Require(t, len(store.calls) == 0, "snapshot calls = %+v, want none", store.calls)
		})
	}
}

func TestMissingWriteFileStillCreatesNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.txt")
	result, err := execWrite(t, filepath.Dir(path), "", "", CapabilityOptions{}, newTempCodeGroupStore(t), map[string]any{
		"path":    path,
		"content": "created",
	})
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(result, "Wrote"), "result = %q, want write success", result)
	data, err := os.ReadFile(path)
	testassert.NoError(t, err)
	testassert.Require(t, string(data) == "created", "content = %q, want created", data)
}

func assertNonRegularRefusal(t *testing.T, path string, err error) {
	t.Helper()
	testassert.Require(t, err != nil, "operation succeeded, want non-regular target refusal")
	msg := strings.ToLower(err.Error())
	testassert.Require(t, !strings.Contains(msg, "has not been read yet"), "error = %v, want target-shape refusal before read-required checks", err)
	testassert.Require(t, !strings.Contains(msg, "did not fail closed before blocking"), "error = %v, want target-shape refusal before blocking on target I/O", err)
	testassert.Require(t, strings.Contains(msg, "non-regular"), "error = %v, want explicit non-regular target-shape refusal", err)
	info, statErr := os.Lstat(path)
	testassert.Require(t, statErr == nil, "lstat %s = %v, want non-regular target still present", path, statErr)
	testassert.Require(t, !info.Mode().IsRegular(), "%s became a regular file after refused operation", path)
}

func runNonRegularTargetWithTimeout(fn func() error) error {
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

func nonRegularDirectoryTarget(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target-dir")
	testassert.NoError(t, os.Mkdir(path, 0o755))
	return path
}

func nonRegularFIFO(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target-fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	return path
}

func startNonRegularFIFOFeed(t *testing.T, path, content string) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		_, _ = f.Write([]byte(content))
		_ = f.Close()
	}()
	return func() {
		select {
		case <-done:
			return
		default:
		}
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			defer unix.Close(fd)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("FIFO feeder did not finish")
		}
	}
}

func nonRegularUnixSocket(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})
	return path
}

// A hardlink inside the project pointing to an outside-boundary inode
// must be refused at every file-tool entry point: read_file, write_file,
// edit_file, and snapshot capture.
func TestPR11Closure_HardlinkRefusedAcrossAllOperations(t *testing.T) {
	makeHardlink := func(t *testing.T) (hardlinked, outside string) {
		t.Helper()
		project := t.TempDir()
		outsideDir := t.TempDir()
		outside = filepath.Join(outsideDir, "outside.txt")
		testassert.NoError(t, os.WriteFile(outside, []byte("outside-content"), 0o644))
		hardlinked = filepath.Join(project, "hardlinked.txt")
		testassert.NoError(t, os.Link(outside, hardlinked))
		info, err := os.Stat(hardlinked)
		testassert.NoError(t, err)
		st, ok := info.Sys().(*syscall.Stat_t)
		testassert.Require(t, ok && st.Nlink >= 2, "setup: hardlinked target nlink = %d, want >= 2", st.Nlink)
		return hardlinked, outside
	}

	assertHardlinkRefusal := func(t *testing.T, err error) {
		t.Helper()
		testassert.Require(t, err != nil, "operation succeeded on hardlinked target; want refusal")
		msg := strings.ToLower(err.Error())
		testassert.Require(t, strings.Contains(msg, "hardlink") || strings.Contains(msg, "link count") || strings.Contains(msg, "nlink"), "error %v does not mention hardlink/link count/nlink", err)
	}

	t.Run("read_file", func(t *testing.T) {
		hardlinked, _ := makeHardlink(t)
		_, err := execRead(t, filepath.Dir(hardlinked), "", config.ToolsConfig{ReadMaxLines: 500}, map[string]any{"path": hardlinked})
		assertHardlinkRefusal(t, err)
	})

	t.Run("write_file", func(t *testing.T) {
		hardlinked, _ := makeHardlink(t)
		_, err := execWrite(t, filepath.Dir(hardlinked), "", "", CapabilityOptions{}, newTempCodeGroupStore(t),
			map[string]any{"path": hardlinked, "content": "mutated"})
		assertHardlinkRefusal(t, err)
	})

	t.Run("edit_file", func(t *testing.T) {
		hardlinked, _ := makeHardlink(t)
		_, err := execEdit(t, filepath.Dir(hardlinked), "", "", CapabilityOptions{}, newTempCodeGroupStore(t),
			map[string]any{
				"path":       hardlinked,
				"old_string": "outside-content",
				"new_string": "tampered",
			})
		assertHardlinkRefusal(t, err)
	})

	t.Run("snapshot_capture", func(t *testing.T) {
		hardlinked, _ := makeHardlink(t)
		store := newCodeGroupStore(t, filepath.Join(t.TempDir(), "group"))
		_, err := execWrite(t, filepath.Dir(hardlinked), "", "", CapabilityOptions{}, store,
			map[string]any{"path": hardlinked, "content": "mutated"})
		assertHardlinkRefusal(t, err)
	})
}
