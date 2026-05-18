package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefinitionsLookupAndDetection(t *testing.T) {
	if len(All()) == 0 {
		t.Fatal("All() returned no definitions")
	}
	if got := ForExtension(".go"); got == nil || got.Name != "gopls" {
		t.Fatalf("ForExtension(.go) = %+v, want gopls", got)
	}
	if got := ForExtension(".unknown"); got != nil {
		t.Fatalf("ForExtension(.unknown) = %+v, want nil", got)
	}

	root := t.TempDir()
	if got := DetectFromProject(root); len(got) != 0 {
		t.Fatalf("DetectFromProject(empty) = %+v, want none", got)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found := DetectFromProject(root)
	if len(found) != 1 || found[0].Name != "gopls" {
		t.Fatalf("DetectFromProject(go.mod) = %+v, want gopls", found)
	}
}

func TestCacheDirAndResolveBinaryPrefersCache(t *testing.T) {
	home := t.TempDir()
	def := &Definition{Name: "fake", Command: "fake-lsp"}
	cache := CacheDir(home, def.Name)
	if cache != filepath.Join(home, ".cache", "lightcode", "lsp", "fake") {
		t.Fatalf("CacheDir = %q", cache)
	}
	bin := filepath.Join(cache, def.Command)
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := ResolveBinary(home, def); got != bin {
		t.Fatalf("ResolveBinary = %q, want cached binary %q", got, bin)
	}
}
