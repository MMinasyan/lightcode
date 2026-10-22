package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestDefinitionsLookupAndDetection(t *testing.T) {
	testassert.Require(t, len(All()) != 0, "All() returned no definitions")
	{
		got := ForExtension(".go")
		testassert.Require(t, got != nil && got.Name == "gopls", "ForExtension(.go) = %+v, want gopls", got)
	}
	{
		got := ForExtension(".unknown")
		testassert.Require(t, got == nil, "ForExtension(.unknown) = %+v, want nil", got)
	}

	root := t.TempDir()
	{
		got := DetectFromProject(root)
		testassert.Require(t, len(got) == 0, "DetectFromProject(empty) = %+v, want none", got)
	}
	testassert.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o600))
	found := DetectFromProject(root)
	testassert.Require(t, len(found) == 1 && found[0].Name == "gopls", "DetectFromProject(go.mod) = %+v, want gopls", found)
}

func TestCacheDirAndResolveBinaryPrefersCache(t *testing.T) {
	home := t.TempDir()
	def := &Definition{Name: "fake", Command: "fake-lsp"}
	cache := CacheDir(home, def.Name)
	testassert.Require(t, cache == filepath.Join(home, ".cache", "lightcode", "lsp", "fake"), "CacheDir = %q", cache)
	bin := filepath.Join(cache, def.Command)
	testassert.NoError(t, os.MkdirAll(cache, 0o700))
	testassert.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700))
	testassert.Equal(t, ResolveBinary(home, def), bin)
}
