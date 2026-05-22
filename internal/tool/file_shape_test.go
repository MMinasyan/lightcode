package tool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
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
				_, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, NewFileTracker()).Execute(context.Background(), map[string]any{"path": path})
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
					_, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, NewFileTracker()).Execute(context.Background(), map[string]any{"path": path})
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
				_, err := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, NewFileTracker()).Execute(context.Background(), map[string]any{"path": path})
				return err
			},
		},
		{
			name: "write_directory",
			target: func(t *testing.T) (string, func()) {
				return nonRegularDirectoryTarget(t), func() {}
			},
			run: func(path string) error {
				_, err := NewWriteFile(nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
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
					_, err := NewWriteFile(nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
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
				_, err := NewWriteFile(nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "edit_directory",
			target: func(t *testing.T) (string, func()) {
				return nonRegularDirectoryTarget(t), func() {}
			},
			run: func(path string) error {
				_, err := NewEditFile(nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
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
					_, err := NewEditFile(nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
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
				_, err := NewEditFile(nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
					"path":       path,
					"old_string": "before",
					"new_string": "after",
				})
				return err
			},
		},
		{
			name: "staged_directory",
			target: func(t *testing.T) (string, func()) {
				return nonRegularDirectoryTarget(t), func() {}
			},
			run: func(path string) error {
				results := NewStagedExecutor(nil, nil, config.ToolsConfig{}, allowNonRegularStagedCall, nil).ExecutePending(context.Background(), []StagedCall{
					stagedNonRegularWrite(path, "call-1", "after"),
				})
				return singleNonRegularBatchError(results)
			},
		},
		{
			name: "staged_FIFO",
			target: func(t *testing.T) (string, func()) {
				path := nonRegularFIFO(t)
				return path, startNonRegularFIFOFeed(t, path, "before")
			},
			run: func(path string) error {
				return runNonRegularTargetWithTimeout(func() error {
					results := NewStagedExecutor(nil, nil, config.ToolsConfig{}, allowNonRegularStagedCall, nil).ExecutePending(context.Background(), []StagedCall{
						stagedNonRegularWrite(path, "call-1", "after"),
					})
					return singleNonRegularBatchError(results)
				})
			},
		},
		{
			name: "staged_socket",
			target: func(t *testing.T) (string, func()) {
				return nonRegularUnixSocket(t), func() {}
			},
			run: func(path string) error {
				results := NewStagedExecutor(nil, nil, config.ToolsConfig{}, allowNonRegularStagedCall, nil).ExecutePending(context.Background(), []StagedCall{
					stagedNonRegularWrite(path, "call-1", "after"),
				})
				return singleNonRegularBatchError(results)
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
		run    func(store *recordingNonRegularSnapshotStore, path string) error
	}{
		{
			name: "write_directory",
			target: func(t *testing.T) (string, func()) {
				return nonRegularDirectoryTarget(t), func() {}
			},
			run: func(store *recordingNonRegularSnapshotStore, path string) error {
				_, err := NewWriteFileWithSnapshot(store, nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "write_FIFO",
			target: func(t *testing.T) (string, func()) {
				return nonRegularFIFO(t), func() {}
			},
			run: func(store *recordingNonRegularSnapshotStore, path string) error {
				return runNonRegularTargetWithTimeout(func() error {
					_, err := NewWriteFileWithSnapshot(store, nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
					return err
				})
			},
		},
		{
			name: "edit_directory",
			target: func(t *testing.T) (string, func()) {
				return nonRegularDirectoryTarget(t), func() {}
			},
			run: func(store *recordingNonRegularSnapshotStore, path string) error {
				_, err := NewEditFileWithSnapshot(store, nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
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
			run: func(store *recordingNonRegularSnapshotStore, path string) error {
				return runNonRegularTargetWithTimeout(func() error {
					_, err := NewEditFileWithSnapshot(store, nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
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
			run: func(store *recordingNonRegularSnapshotStore, path string) error {
				_, err := NewWriteFileWithSnapshot(store, nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": path, "content": "after"})
				return err
			},
		},
		{
			name: "staged_FIFO",
			target: func(t *testing.T) (string, func()) {
				path := nonRegularFIFO(t)
				return path, startNonRegularFIFOFeed(t, path, "before")
			},
			run: func(store *recordingNonRegularSnapshotStore, path string) error {
				return runNonRegularTargetWithTimeout(func() error {
					results := NewStagedExecutor(store, nil, config.ToolsConfig{}, allowNonRegularStagedCall, nil).ExecutePending(context.Background(), []StagedCall{
						stagedNonRegularWrite(path, "call-1", "after"),
					})
					return singleNonRegularBatchError(results)
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingNonRegularSnapshotStore{turn: 1}
			path, release := tc.target(t)
			err := tc.run(store, path)
			release()
			assertNonRegularRefusal(t, path, err)
			if len(store.calls) != 0 {
				t.Fatalf("snapshot calls = %+v, want none", store.calls)
			}
		})
	}
}

func TestMissingWriteFileStillCreatesNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.txt")
	result, err := NewWriteFile(nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{
		"path":    path,
		"content": "created",
	})
	if err != nil {
		t.Fatalf("WriteFile missing target error = %v", err)
	}
	if !strings.Contains(result, "Wrote") {
		t.Fatalf("result = %q, want write success", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "created" {
		t.Fatalf("content = %q, want created", data)
	}
}

type nonRegularSnapshotCall struct {
	turn      int
	original  string
	canonical string
}

type recordingNonRegularSnapshotStore struct {
	turn  int
	calls []nonRegularSnapshotCall
}

func (s *recordingNonRegularSnapshotStore) Snapshot(turn int, absPath string) error {
	return s.SnapshotResolved(turn, absPath, absPath)
}

func (s *recordingNonRegularSnapshotStore) SnapshotResolved(turn int, originalPath, canonicalPath string) error {
	s.calls = append(s.calls, nonRegularSnapshotCall{turn: turn, original: originalPath, canonical: canonicalPath})
	return nil
}

func (s *recordingNonRegularSnapshotStore) CurrentTurn() int {
	return s.turn
}

func allowNonRegularStagedCall(string, string) permission.Decision {
	return permission.DecisionAllow
}

func stagedNonRegularWrite(path, id, content string) StagedCall {
	return StagedCall{
		ToolName:   "write_file",
		ToolCallID: id,
		Params: map[string]any{
			"path":    path,
			"content": content,
		},
	}
}

func singleNonRegularBatchError(results []BatchResult) error {
	if len(results) != 1 {
		return fmt.Errorf("batch returned %d results, want 1", len(results))
	}
	if results[0].Success {
		return nil
	}
	return errors.New(results[0].Error)
}

func assertNonRegularRefusal(t *testing.T, path string, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("operation succeeded, want non-regular target refusal")
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "has not been read yet") {
		t.Fatalf("error = %v, want target-shape refusal before read-required checks", err)
	}
	if strings.Contains(msg, "did not fail closed before blocking") {
		t.Fatalf("error = %v, want target-shape refusal before blocking on target I/O", err)
	}
	if !strings.Contains(msg, "non-regular") {
		t.Fatalf("error = %v, want explicit non-regular target-shape refusal", err)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		t.Fatalf("lstat %s = %v, want non-regular target still present", path, statErr)
	}
	if info.Mode().IsRegular() {
		t.Fatalf("%s became a regular file after refused operation", path)
	}
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
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
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
