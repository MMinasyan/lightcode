package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
)

// The prepared call binds its canonical target at preparation; when the
// requested path is repointed to another file before execution, the
// execution closure's revalidation refuses the canonical change without
// substituting the replacement.

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

	// Prepare every call while the link resolves to the safe file.
	readArgs, err := NormalizeReadArgs(map[string]any{"path": link}, 500)
	if err != nil {
		t.Fatal(err)
	}
	readCall, err := PrepareReadCall(root, "", config.ToolsConfig{ReadMaxLines: 500}, readArgs)
	if err != nil {
		t.Fatal(err)
	}
	writeArgs, err := NormalizeWriteArgs(map[string]any{"path": link, "content": "after"})
	if err != nil {
		t.Fatal(err)
	}
	writeCall, err := PrepareWriteCall(root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, writeArgs)
	if err != nil {
		t.Fatal(err)
	}
	editArgs, err := NormalizeEditArgs(map[string]any{
		"path":       link,
		"old_string": "safe",
		"new_string": "after",
	})
	if err != nil {
		t.Fatal(err)
	}
	editCall, err := PrepareEditCall(root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, editArgs)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	if _, err := readCall.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("read_file error = %v, want canonical change refusal", err)
	}
	if _, err := writeCall.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("write_file error = %v, want canonical change refusal", err)
	}
	if _, err := editCall.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
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

	// Prepare every call while the target is the real file.
	readArgs, err := NormalizeReadArgs(map[string]any{"path": target}, 500)
	if err != nil {
		t.Fatal(err)
	}
	readCall, err := PrepareReadCall(root, "", config.ToolsConfig{ReadMaxLines: 500}, readArgs)
	if err != nil {
		t.Fatal(err)
	}
	writeArgs, err := NormalizeWriteArgs(map[string]any{"path": target, "content": "after"})
	if err != nil {
		t.Fatal(err)
	}
	writeCall, err := PrepareWriteCall(root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, writeArgs)
	if err != nil {
		t.Fatal(err)
	}
	editArgs, err := NormalizeEditArgs(map[string]any{
		"path":       target,
		"old_string": "safe",
		"new_string": "after",
	})
	if err != nil {
		t.Fatal(err)
	}
	editCall, err := PrepareEditCall(root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, editArgs)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, target); err != nil {
		t.Fatal(err)
	}

	if _, err := readCall.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("read_file error = %v, want canonical change refusal", err)
	}
	if _, err := writeCall.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("write_file error = %v, want canonical change refusal", err)
	}
	if _, err := editCall.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("edit_file error = %v, want canonical change refusal", err)
	}
	assertFileContent(t, secret, "secret")
}
