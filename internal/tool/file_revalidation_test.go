package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
)

func TestFileToolsRefuseRepointedApprovedSymlink(t *testing.T) {
	root := t.TempDir()
	safe := filepath.Join(root, "safe.txt")
	secret := filepath.Join(root, "secret.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(safe, []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(safe, link); err != nil {
		t.Fatal(err)
	}
	params := withCanonicalPathParam(map[string]any{"path": link}, safe)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	if _, err := NewReadFile(config.ToolsConfig{}, nil).Execute(context.Background(), params); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("read_file error = %v, want canonical change refusal", err)
	}

	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, safe, 1, 100)
	writeParams := withCanonicalPathParam(map[string]any{"path": link, "content": "after"}, safe)
	if _, err := NewWriteFile(tracker, config.ToolsConfig{}).Execute(context.Background(), writeParams); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("write_file error = %v, want canonical change refusal", err)
	}
	editParams := withCanonicalPathParam(map[string]any{
		"path":       link,
		"old_string": "safe",
		"new_string": "after",
	}, safe)
	if _, err := NewEditFile(tracker, config.ToolsConfig{}).Execute(context.Background(), editParams); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("edit_file error = %v, want canonical change refusal", err)
	}

	assertFileContent(t, safe, "safe")
	assertFileContent(t, secret, "secret")
}

func TestFileToolsRefuseFinalTargetReplacedBySymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(target, []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	params := withCanonicalPathParam(map[string]any{"path": target}, target)
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, target); err != nil {
		t.Fatal(err)
	}

	if _, err := NewReadFile(config.ToolsConfig{}, nil).Execute(context.Background(), params); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("read_file error = %v, want canonical change refusal", err)
	}

	tracker := NewFileTracker()
	tracker.TrackIdentity(target, 1, 100, FileIdentity{Mtime: mustStat(t, secret).ModTime()})
	writeParams := withCanonicalPathParam(map[string]any{"path": target, "content": "after"}, target)
	if _, err := NewWriteFile(tracker, config.ToolsConfig{}).Execute(context.Background(), writeParams); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("write_file error = %v, want canonical change refusal", err)
	}
	editParams := withCanonicalPathParam(map[string]any{
		"path":       target,
		"old_string": "safe",
		"new_string": "after",
	}, target)
	if _, err := NewEditFile(tracker, config.ToolsConfig{}).Execute(context.Background(), editParams); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("edit_file error = %v, want canonical change refusal", err)
	}
	assertFileContent(t, secret, "secret")
}

func TestStagedAliasGroupFailsWhenAliasRepointedAfterApproval(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	secret := filepath.Join(root, "secret.txt")
	alias1 := filepath.Join(root, "alias1.txt")
	alias2 := filepath.Join(root, "alias2.txt")
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias1); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias2); err != nil {
		t.Fatal(err)
	}

	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, target, 1, 100)
	checks := 0
	executor := NewStagedExecutor(nil, tracker, config.ToolsConfig{}, func(toolName, arg string) permission.Decision {
		checks++
		if checks == 2 && arg == target {
			if err := os.Remove(alias2); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secret, alias2); err != nil {
				t.Fatal(err)
			}
		}
		return permission.DecisionAllow
	}, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		{ToolName: "write_file", ToolCallID: "a", Params: map[string]any{"path": alias1, "content": "first"}},
		{ToolName: "write_file", ToolCallID: "b", Params: map[string]any{"path": alias2, "content": "second"}},
	})

	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	for i, result := range results {
		if result.Success || !strings.Contains(result.Error, "approved canonical path changed") {
			t.Fatalf("result[%d] = %+v, want canonical change failure", i, result)
		}
	}
	assertFileContent(t, target, "before")
	assertFileContent(t, secret, "secret")
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
