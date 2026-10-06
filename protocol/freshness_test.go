package protocol_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGeneratedContractsFresh regenerates the Go protocol artifact from the
// committed schema with the committed generator configuration and compares
// the bytes against the committed protocol/protocol.gen.go: any hand edit or
// schema change without regeneration fails. The TypeScript artifact is not
// covered here — its regeneration needs the frontend toolchain and is
// gated by the make check-protocol target.
func TestGeneratedContractsFresh(t *testing.T) {
	// go test runs the test binary with the package directory as its working
	// directory, so the module root is exactly "..".
	committed, err := os.ReadFile(filepath.Join("..", "protocol", "protocol.gen.go"))
	if err != nil {
		t.Fatalf("reading committed generated contracts: %v", err)
	}
	generated := filepath.Join(t.TempDir(), "protocol.gen.go")
	cmd := exec.Command("go", "tool", "oapi-codegen",
		"--config", "protocol/oapi-codegen.yaml",
		"-o", generated,
		"protocol/openapi.yaml")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("regenerating protocol contracts: %v\n%s", err, out)
	}
	regenerated, err := os.ReadFile(generated)
	if err != nil {
		t.Fatalf("reading regenerated contracts: %v", err)
	}
	if !bytes.Equal(committed, regenerated) {
		t.Fatalf("committed protocol/protocol.gen.go is stale: regenerate with make generate-protocol (%d committed bytes, %d regenerated)", len(committed), len(regenerated))
	}
}

// TestGeneratedFileSetGate exercises the shared freshness shell check in a
// disposable Git fixture: staged tracked bytes matching the fixture state
// pass, modified tracked output fails, a newly emitted untracked artifact
// fails, and an unrelated untracked file does not. The fixture never
// commits and never touches Git configuration; the only Git writes anywhere
// in this test are init and add inside that disposable fixture.
func TestGeneratedFileSetGate(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "scripts", "check-protocol-generated.sh"))
	if err != nil {
		t.Fatalf("resolving the freshness script: %v", err)
	}

	fixture := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		for _, file := range []string{"protocol/protocol.gen.go", "frontend/src/generated/protocol/types.gen.ts"} {
			path := filepath.Join(dir, filepath.FromSlash(file))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("creating fixture directory: %v", err)
			}
			if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
				t.Fatalf("writing fixture artifact: %v", err)
			}
		}
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("fixture git init: %v\n%s", err, out)
		}
		if out, err := exec.Command("git", "-C", dir, "add", ".").CombinedOutput(); err != nil {
			t.Fatalf("fixture git add: %v\n%s", err, out)
		}
		return dir
	}

	run := func(t *testing.T, dir string) error {
		t.Helper()
		cmd := exec.Command("sh", script)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("exit: %v\n%s", err, out)
		}
		return nil
	}

	t.Run("matching staged bytes pass", func(t *testing.T) {
		if err := run(t, fixture(t)); err != nil {
			t.Fatalf("gate rejected matching staged artifacts: %v", err)
		}
	})

	t.Run("modified tracked output fails", func(t *testing.T) {
		dir := fixture(t)
		path := filepath.Join(dir, "protocol", "protocol.gen.go")
		if err := os.WriteFile(path, []byte("hand-edited"), 0o644); err != nil {
			t.Fatalf("modifying fixture artifact: %v", err)
		}
		if err := run(t, dir); err == nil {
			t.Fatal("gate accepted a modified tracked artifact")
		}
	})

	t.Run("new untracked generated file fails", func(t *testing.T) {
		dir := fixture(t)
		path := filepath.Join(dir, "frontend", "src", "generated", "protocol", "client.gen.ts")
		if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
			t.Fatalf("emitting untracked fixture artifact: %v", err)
		}
		if err := run(t, dir); err == nil {
			t.Fatal("gate accepted a newly emitted untracked artifact")
		}
	})

	t.Run("unrelated untracked file passes", func(t *testing.T) {
		dir := fixture(t)
		path := filepath.Join(dir, "unrelated.txt")
		if err := os.WriteFile(path, []byte("junk"), 0o644); err != nil {
			t.Fatalf("writing unrelated fixture file: %v", err)
		}
		if err := run(t, dir); err != nil {
			t.Fatalf("gate rejected an unrelated untracked file: %v", err)
		}
	})
}
