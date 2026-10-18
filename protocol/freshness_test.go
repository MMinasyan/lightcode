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

// TestGeneratedNativeBindingsFresh regenerates the native operation bindings
// and the test-only native invocation contract from the committed schema with
// the committed build command and compares both byte-for-byte against the
// committed artifacts. Two runs must agree, so a hand edit, a stale artifact
// or a nondeterministic generator fails here. The TypeScript artifact is not
// covered here — its regeneration needs the frontend toolchain and is gated by
// the make check-protocol target.
func TestGeneratedNativeBindingsFresh(t *testing.T) {
	committedSource := committedArtifact(t, "internal/clientops/operations.gen.go")
	committedContract := committedArtifact(t, "frontend/src/generated/native-invocations.json")
	generate := func(t *testing.T) ([]byte, []byte) {
		t.Helper()
		dir := t.TempDir()
		source := filepath.Join(dir, "operations.gen.go")
		contract := filepath.Join(dir, "native-invocations.json")
		cmd := exec.Command("go", "run", "./internal/protocolgen",
			"-schema", "protocol/openapi.yaml",
			"-out", source,
			"-native-contract-out", contract)
		cmd.Dir = ".."
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("regenerating native bindings: %v\n%s", err, out)
		}
		return readArtifact(t, source), readArtifact(t, contract)
	}
	source, contract := generate(t)
	againSource, againContract := generate(t)
	if !bytes.Equal(source, againSource) || !bytes.Equal(contract, againContract) {
		t.Fatal("the native bindings generator is not deterministic")
	}
	if !bytes.Equal(committedSource, source) {
		t.Fatalf("committed internal/clientops/operations.gen.go is stale: regenerate with make generate-protocol (%d committed bytes, %d regenerated)", len(committedSource), len(source))
	}
	if !bytes.Equal(committedContract, contract) {
		t.Fatalf("committed frontend/src/generated/native-invocations.json is stale: regenerate with make generate-protocol (%d committed bytes, %d regenerated)", len(committedContract), len(contract))
	}
}

// committedArtifact reads one committed artifact relative to the module root.
func committedArtifact(t *testing.T, rel string) []byte {
	t.Helper()
	return readArtifact(t, filepath.Join("..", filepath.FromSlash(rel)))
}

// readArtifact reads one artifact or fails the calling test.
func readArtifact(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

// TestGeneratedFileSetGate exercises the shared freshness shell check in a
// disposable Git fixture: staged tracked bytes matching the fixture state
// pass, modified tracked output fails, a newly emitted or unstaged artifact
// fails, and an unrelated untracked file does not. The fixture never commits
// and never touches Git configuration; the only Git writes anywhere in this
// test are init, add and one unstaging rm inside that disposable fixture.
func TestGeneratedFileSetGate(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "scripts", "check-protocol-generated.sh"))
	if err != nil {
		t.Fatalf("resolving the freshness script: %v", err)
	}

	// Every artifact the gate names is a tracked file in the fixture, so the
	// matching case proves the gate's path list covers all four outputs.
	artifacts := []string{
		"protocol/protocol.gen.go",
		"frontend/src/generated/protocol/types.gen.ts",
		"internal/clientops/operations.gen.go",
		"frontend/src/generated/native-invocations.json",
	}

	fixture := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		for _, file := range artifacts {
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
		for _, artifact := range artifacts {
			dir := fixture(t)
			path := filepath.Join(dir, filepath.FromSlash(artifact))
			if err := os.WriteFile(path, []byte("hand-edited"), 0o644); err != nil {
				t.Fatalf("modifying fixture artifact: %v", err)
			}
			if err := run(t, dir); err == nil {
				t.Fatalf("gate accepted a modified tracked artifact: %s", artifact)
			}
		}
	})

	t.Run("new untracked generated file fails", func(t *testing.T) {
		for _, artifact := range []string{
			"frontend/src/generated/protocol/client.gen.ts",
			"internal/clientops/operations.gen.go",
			"frontend/src/generated/native-invocations.json",
		} {
			dir := fixture(t)
			path := filepath.Join(dir, filepath.FromSlash(artifact))
			if _, err := os.Stat(path); err == nil {
				// The staged artifact becomes untracked output again.
				if out, err := exec.Command("git", "-C", dir, "rm", "--cached", "-q", "--", artifact).CombinedOutput(); err != nil {
					t.Fatalf("unstaging fixture artifact %s: %v\n%s", artifact, err, out)
				}
			} else if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
				t.Fatalf("emitting untracked fixture artifact %s: %v", artifact, err)
			}
			if err := run(t, dir); err == nil {
				t.Fatalf("gate accepted a newly emitted untracked artifact: %s", artifact)
			}
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
