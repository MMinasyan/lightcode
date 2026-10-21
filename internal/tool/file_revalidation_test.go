package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.NoError(t, os.WriteFile(safe, []byte("safe"), 0o644))
	testassert.NoError(t, os.WriteFile(secret, []byte("secret"), 0o644))
	testassert.NoError(t, os.Symlink(safe, link))

	// Prepare every call while the link resolves to the safe file.
	readArgs, err := NormalizeReadArgs(map[string]any{"path": link}, 500)
	testassert.NoError(t, err)
	readCall, err := PrepareReadCall(root, "", config.ToolsConfig{ReadMaxLines: 500}, readArgs)
	testassert.NoError(t, err)
	writeArgs, err := NormalizeWriteArgs(map[string]any{"path": link, "content": "after"})
	testassert.NoError(t, err)
	writeCall, err := PrepareWriteCall(root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, writeArgs)
	testassert.NoError(t, err)
	editArgs, err := NormalizeEditArgs(map[string]any{
		"path":       link,
		"old_string": "safe",
		"new_string": "after",
	})
	testassert.NoError(t, err)
	editCall, err := PrepareEditCall(root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, editArgs)
	testassert.NoError(t, err)

	testassert.NoError(t, os.Remove(link))
	testassert.NoError(t, os.Symlink(secret, link))

	{
		_, err := readCall.Execute(context.Background())
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "approved canonical path changed"), "read_file error = %v, want canonical change refusal", err)
	}
	{
		_, err := writeCall.Execute(context.Background())
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "approved canonical path changed"), "write_file error = %v, want canonical change refusal", err)
	}
	{
		_, err := editCall.Execute(context.Background())
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "approved canonical path changed"), "edit_file error = %v, want canonical change refusal", err)
	}

	assertFileContent(t, safe, "safe")
	assertFileContent(t, secret, "secret")
}

func TestFileToolsRefuseFinalTargetReplacedBySymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	secret := filepath.Join(root, "secret.txt")
	testassert.NoError(t, os.WriteFile(target, []byte("safe"), 0o644))
	testassert.NoError(t, os.WriteFile(secret, []byte("secret"), 0o644))

	// Prepare every call while the target is the real file.
	readArgs, err := NormalizeReadArgs(map[string]any{"path": target}, 500)
	testassert.NoError(t, err)
	readCall, err := PrepareReadCall(root, "", config.ToolsConfig{ReadMaxLines: 500}, readArgs)
	testassert.NoError(t, err)
	writeArgs, err := NormalizeWriteArgs(map[string]any{"path": target, "content": "after"})
	testassert.NoError(t, err)
	writeCall, err := PrepareWriteCall(root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, writeArgs)
	testassert.NoError(t, err)
	editArgs, err := NormalizeEditArgs(map[string]any{
		"path":       target,
		"old_string": "safe",
		"new_string": "after",
	})
	testassert.NoError(t, err)
	editCall, err := PrepareEditCall(root, "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, editArgs)
	testassert.NoError(t, err)

	testassert.NoError(t, os.Remove(target))
	testassert.NoError(t, os.Symlink(secret, target))

	{
		_, err := readCall.Execute(context.Background())
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "approved canonical path changed"), "read_file error = %v, want canonical change refusal", err)
	}
	{
		_, err := writeCall.Execute(context.Background())
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "approved canonical path changed"), "write_file error = %v, want canonical change refusal", err)
	}
	{
		_, err := editCall.Execute(context.Background())
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "approved canonical path changed"), "edit_file error = %v, want canonical change refusal", err)
	}
	assertFileContent(t, secret, "secret")
}
