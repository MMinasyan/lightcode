package project

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

func TestNewResolverSyncsNamespaceLeafBeforeParents(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	var synced []string
	atomicfs.SyncDirFunc = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	if _, err := NewResolver(home, projectRoot); err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	want := []string{
		filepath.Join(home, ".lightcode", "projects"),
		filepath.Join(home, ".lightcode"),
		home,
	}
	if !reflect.DeepEqual(synced, want) {
		t.Fatalf("sync order = %v, want %v", synced, want)
	}
}

func TestEnsureForPathSyncsCreationAndExistingRetry(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "project")
	var synced []string
	atomicfs.SyncDirFunc = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	p, err := EnsureForPath(root, path)
	if err != nil {
		t.Fatalf("first EnsureForPath: %v", err)
	}
	projectDir := filepath.Join(root, p.ID)
	if got := synced[len(synced)-2:]; !reflect.DeepEqual(got, []string{projectDir, root}) {
		t.Fatalf("creation tail sync order = %v, want project then root", got)
	}
	synced = nil
	if _, err := EnsureForPath(root, path); err != nil {
		t.Fatalf("existing EnsureForPath: %v", err)
	}
	if !reflect.DeepEqual(synced, []string{projectDir, root}) {
		t.Fatalf("existing sync order = %v, want project then root", synced)
	}
}

func TestEnsureForPathExistingSyncFailuresRetry(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "project")
	initial, err := EnsureForPath(root, path)
	if err != nil {
		t.Fatalf("initial EnsureForPath: %v", err)
	}
	projectDir := filepath.Join(root, initial.ID)

	for _, row := range []struct {
		name      string
		failDir   string
		wantCalls []string
	}{
		{name: "project_directory", failDir: projectDir, wantCalls: []string{projectDir}},
		{name: "projects_root", failDir: root, wantCalls: []string{projectDir, root}},
	} {
		t.Run(row.name, func(t *testing.T) {
			injected := errors.New("injected existing project sync failure")
			var synced []string
			atomicfs.SyncDirFunc = func(dir string) error {
				synced = append(synced, dir)
				if dir == row.failDir {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

			if _, err := EnsureForPath(root, path); !errors.Is(err, injected) {
				t.Fatalf("existing EnsureForPath error = %v, want ordinary injected retryable error", err)
			}
			if !reflect.DeepEqual(synced, row.wantCalls) {
				t.Fatalf("failed existing sync order = %v, want %v", synced, row.wantCalls)
			}
			atomicfs.SyncDirFunc = nil
			synced = nil
			retried, err := EnsureForPath(root, path)
			if err != nil {
				t.Fatalf("existing EnsureForPath retry: %v", err)
			}
			if retried.ID != initial.ID || retried.Path != initial.Path {
				t.Fatalf("retried project = %+v, want original %+v", retried, initial)
			}
		})
	}
}

func TestProjectNamespaceSyncFailuresRemainRetryable(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	projectsRoot := filepath.Join(home, ".lightcode", "projects")
	injected := errors.New("injected directory sync failure")
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == projectsRoot {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
	if _, err := NewResolver(home, projectRoot); !errors.Is(err, injected) {
		t.Fatalf("NewResolver error = %v, want injected failure", err)
	}

	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "project")
	projectsRoot = root
	if _, err := EnsureForPath(root, path); err == nil {
		t.Fatal("EnsureForPath with failed projects-root sync returned nil")
	}
	if _, statErr := os.Stat(root); statErr != nil {
		t.Fatalf("projects root disappeared after retryable failure: %v", statErr)
	}
}

func TestNewResolverReportsEveryAncestorSyncFailure(t *testing.T) {
	for _, row := range []struct {
		name string
		fail int
	}{
		{name: "projects_root", fail: 0},
		{name: "lightcode_parent", fail: 1},
		{name: "home_parent", fail: 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			home := t.TempDir()
			projectRoot := t.TempDir()
			ancestors := []string{
				filepath.Join(home, ".lightcode", "projects"),
				filepath.Join(home, ".lightcode"),
				home,
			}
			injected := errors.New("injected resolver ancestor sync failure")
			var synced []string
			atomicfs.SyncDirFunc = func(dir string) error {
				synced = append(synced, dir)
				if dir == ancestors[row.fail] {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

			if _, err := NewResolver(home, projectRoot); !errors.Is(err, injected) {
				t.Fatalf("NewResolver error = %v, want ancestor %s failure", err, row.name)
			}
			if !reflect.DeepEqual(synced, ancestors[:row.fail+1]) {
				t.Fatalf("sync order before %s failure = %v, want %v", row.name, synced, ancestors[:row.fail+1])
			}
			for _, dir := range ancestors {
				if info, err := os.Stat(dir); err != nil || !info.IsDir() {
					t.Fatalf("ancestor %s after retryable failure: info=%v err=%v", dir, info, err)
				}
			}
		})
	}
}

func TestEnsureForPathProjectDirectorySyncFailureIsRetryable(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "project")
	injected := errors.New("injected project directory sync failure")
	projectDir := filepath.Join(root, projectID(path))
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == projectDir {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	if _, err := EnsureForPath(root, path); !errors.Is(err, injected) {
		t.Fatalf("EnsureForPath error = %v, want injected project-dir sync failure", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "meta.json")); err != nil {
		t.Fatalf("project metadata after retryable sync failure: %v", err)
	}
}

func TestEnsureForPathNewSyncFailuresSucceedOnRetry(t *testing.T) {
	for _, row := range []struct {
		name    string
		failDir func(root, projectDir string) string
	}{
		{
			name: "project_directory",
			failDir: func(_, projectDir string) string {
				return projectDir
			},
		},
		{
			name: "projects_root",
			failDir: func(root, _ string) string {
				return root
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(t.TempDir(), "project")
			projectDir := filepath.Join(root, projectID(path))
			failAt := row.failDir(root, projectDir)
			injected := errors.New("injected new project sync failure")
			atomicfs.SyncDirFunc = func(dir string) error {
				if dir == failAt {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

			if _, err := EnsureForPath(root, path); !errors.Is(err, injected) {
				t.Fatalf("new EnsureForPath error = %v, want injected sync failure", err)
			}
			atomicfs.SyncDirFunc = nil

			retried, err := EnsureForPath(root, path)
			if err != nil {
				t.Fatalf("new EnsureForPath retry: %v", err)
			}
			if retried.ID != projectID(path) || retried.Path != path {
				t.Fatalf("retried project = %+v, want id %q and path %q", retried, projectID(path), path)
			}
		})
	}
}
