package agent_test

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const publicModule = "github.com/MMinasyan/lightcode"

// TestPublicFoundationDependencyIsolation enforces the pre-cutover dependency baseline over the authoritative complete set of Git-tracked non-test Go files: the model package imports only the standard library; the agent package imports only the standard library and the public model package; every other tracked production file imports neither new package, whatever its directory name is. Test files are exempt in every directory — external-package test files are exactly where direct and composition tests of the new packages live — and untracked or ignored files never gate the guard. When a later phase adds a new target package that must consume model or agent, it extends the allowlist for its own package only; existing root and internal/ production packages stay forbidden until their owning cutover or deletion phase.
func TestPublicFoundationDependencyIsolation(t *testing.T) {
	root := moduleRoot(t)
	std := standardLibraryImports(t)
	var modelFiles, agentFiles int
	for _, rel := range trackedGoFiles(t, root) {
		switch path.Dir(rel) {
		case "model":
			modelFiles++
		case "agent":
			agentFiles++
		}
		imports, err := parseFileImports(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: parsing imports failed: %v", rel, err)
			continue
		}
		for _, problem := range checkTrackedGoFile(rel, imports, std) {
			t.Error(problem)
		}
	}
	if modelFiles == 0 || agentFiles == 0 {
		t.Fatalf("tracked set holds %d model and %d agent production files from %s; both packages must be present", modelFiles, agentFiles, root)
	}
}

// checkTrackedGoFile applies the baseline rules to one tracked production file from its module-root slash path and parsed imports: model/ accepts only authoritative stdlib members, agent/ accepts those plus the public model package, and every other file — under any directory name, with no path-based skipping — accepts neither new package.
func checkTrackedGoFile(rel string, imports []string, std map[string]bool) []string {
	const modelPkg = publicModule + "/model"
	const agentPkg = publicModule + "/agent"
	var problems []string
	for _, imp := range imports {
		switch path.Dir(rel) {
		case "model":
			if !std[imp] {
				problems = append(problems, rel+": model package imports "+imp+"; model may import only the standard library")
			}
		case "agent":
			if imp != modelPkg && !std[imp] {
				problems = append(problems, rel+": agent package imports "+imp+"; agent may import only the standard library and "+modelPkg)
			}
		default:
			if imp == modelPkg || imp == agentPkg {
				problems = append(problems, rel+": production file imports "+imp+"; no existing package may consume the public foundation before its cutover phase")
			}
		}
	}
	return problems
}

// TestDependencyRulesRejectNonStdlibDotlessImports proves the allowlists use authoritative standard-library membership, not a dot-in-path shape: the cgo pseudo-import "C" and a dotless path outside the stdlib set fail both the model and agent rows, while ordinary stdlib imports and the model dependency on agent's side still pass.
func TestDependencyRulesRejectNonStdlibDotlessImports(t *testing.T) {
	std := standardLibraryImports(t)
	for _, pkg := range []string{"model", "agent"} {
		for _, imp := range []string{"C", "notreal/pkg"} {
			if problems := checkTrackedGoFile(pkg+"/x.go", []string{imp}, std); len(problems) != 1 {
				t.Errorf("%s file importing %q: %d problems, want 1: %v", pkg, imp, len(problems), problems)
			}
		}
	}
	if problems := checkTrackedGoFile("model/x.go", []string{"fmt", "net/http"}, std); len(problems) != 0 {
		t.Errorf("stdlib imports flagged in model: %v", problems)
	}
	if problems := checkTrackedGoFile("agent/x.go", []string{"fmt", publicModule + "/model"}, std); len(problems) != 0 {
		t.Errorf("allowed agent imports flagged: %v", problems)
	}
}

// TestDependencyRulesCheckEveryTrackedDirectory proves the file-set contract has no directory-name skipping: tracked-looking paths under previously-skipped names (.hidden, _underscore, frontend, node_modules, vendor) are ordinary production files whose imports of either new package are reported.
func TestDependencyRulesCheckEveryTrackedDirectory(t *testing.T) {
	std := standardLibraryImports(t)
	for _, rel := range []string{".hidden/pkg/x.go", "_scaffold/pkg/x.go", "frontend/bindata.go", "node_modules/pkg/x.go", "vendor/pkg/x.go"} {
		for _, imp := range []string{publicModule + "/model", publicModule + "/agent"} {
			if problems := checkTrackedGoFile(rel, []string{imp}, std); len(problems) != 1 {
				t.Errorf("tracked %q importing %q: %d problems, want 1: %v", rel, imp, len(problems), problems)
			}
		}
	}
}

// trackedGoFiles returns the module-root-relative slash paths of every Git-tracked non-test Go file — the authoritative production set, independent of the worktree's untracked or ignored contents.
func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	out := bytes.Buffer{}
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("listing tracked files under %s: %v", root, err)
	}
	var files []string
	for _, rel := range strings.Split(out.String(), "\x00") {
		if rel == "" || !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		files = append(files, rel)
	}
	if len(files) == 0 {
		t.Fatalf("git tracked no Go files under %s", root)
	}
	return files
}

// standardLibraryImports returns authoritative standard-library membership from the toolchain's own package list. "C" is removed explicitly: the cgo pseudo-import must never ride either allowlist regardless of whether a given toolchain's std listing includes it.
func standardLibraryImports(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("go", "list", "std").Output()
	if err != nil {
		t.Fatalf("go list std: %v", err)
	}
	std := make(map[string]bool)
	for _, imp := range strings.Fields(string(out)) {
		std[imp] = true
	}
	delete(std, "C")
	return std
}

// moduleRoot walks up from the test's working directory to the module root (the directory holding go.mod).
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// parseFileImports returns the import paths of one file's import declarations.
func parseFileImports(path string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	imports := make([]string, 0, len(f.Imports))
	for _, spec := range f.Imports {
		imp, err := strconv.Unquote(spec.Path.Value) // the standard decoder yields the canonical path for interpreted and raw-string literals alike; manual quote trimming would leave backquotes on raw imports and mis-flag compliant files.
		if err != nil {
			return nil, fmt.Errorf("%s: undecodable import literal %s: %w", path, spec.Path.Value, err)
		}
		imports = append(imports, imp)
	}
	return imports, nil
}

// TestParseFileImportsCanonicalizesRawStringImports is the false-rejection regression: import literals must go through the standard Go string-literal decoder, so a raw-string (backquoted) import yields its canonical path and a compliant agent file written that way still passes the allowlist after extraction.
func TestParseFileImportsCanonicalizesRawStringImports(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(file, []byte("package agent\n\nimport `github.com/MMinasyan/lightcode/model`\nimport `fmt`\n"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	imports, err := parseFileImports(file)
	if err != nil {
		t.Fatalf("parseFileImports: %v", err)
	}
	want := []string{publicModule + "/model", "fmt"}
	if len(imports) != len(want) || imports[0] != want[0] || imports[1] != want[1] {
		t.Fatalf("import paths = %q, want canonical %q", imports, want)
	}
	if problems := checkTrackedGoFile("agent/x.go", imports, standardLibraryImports(t)); len(problems) != 0 {
		t.Errorf("raw-string imports rejected by the agent allowlist: %v", problems)
	}
}
