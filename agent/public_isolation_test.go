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

// sqliteDriverPkg is the one third-party dependency the foundation permits:
// the SQLite driver is confined to internal/storage.
const sqliteDriverPkg = "github.com/mattn/go-sqlite3"

// tiktokenPkg is the one third-party dependency the harness permits: the
// conversation token estimator's cl100k_base encoder.
const tiktokenPkg = "github.com/pkoukk/tiktoken-go"

// TestPublicFoundationDependencyIsolation enforces the pre-cutover dependency baseline over the authoritative complete set of Git-tracked non-test Go files: the model package imports only the standard library; the agent package imports only the standard library and the public model package; the harness package is direct-test-only and imports only the standard library plus the public model and agent packages plus the tiktoken encoder of its conversation token estimator; internal/storage is direct-test-only, imports only the standard library plus the public harness contract plus the SQLite driver it implements the contract with, and stays one package without backend subpackages; the runtime package is direct-test-only and imports only the standard library plus the public model and harness packages plus the retained internal config, agents, catalog, atomicfs, prompt, and adaptation helpers, never internal/agent, internal/storage, any concrete plugin under internal/plugins, or the SQLite driver; the internal/plugins/sqlite plugin imports only the standard library plus the public runtime and harness contracts and the internal/storage backend it declares; the internal/plugins/adaptation plugin imports only the standard library plus the public runtime and model contracts and the internal/adaptation binding table it resolves; the plugins/jobs package imports only the standard library plus the public runtime and harness contracts and the internal/cmdoutput capture helper it composes, and stays one package without subpackages; the internal/plugins/tasks plugin imports only the standard library plus the public model, harness, and runtime contracts; the internal/plugins/builtin package imports only the standard library plus the public runtime contract and the six sibling plugins it registers; and every other tracked production file imports none of these packages, not the SQLite driver, not the target runtime or its concrete plugins — including the public plugins/ tree — whatever its directory name is. Test files are exempt in every directory — external-package test files are exactly where direct and composition tests of the new packages live — and untracked or ignored files never gate the guard. When a later phase adds a new target package that must consume model, agent, or harness, it extends the allowlist for its own package only; existing root and internal/ production packages stay forbidden until their owning cutover or deletion phase.
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

// checkTrackedGoFile applies the dependency baseline to one tracked production file.
func checkTrackedGoFile(rel string, imports []string, std map[string]bool) []string {
	const (
		modelPkg      = publicModule + "/model"
		agentPkg      = publicModule + "/agent"
		harnessPkg    = publicModule + "/harness"
		storagePkg    = publicModule + "/internal/storage"
		runtimePkg    = publicModule + "/runtime"
		configPkg     = publicModule + "/internal/config"
		toolPkg       = publicModule + "/internal/tool"
		snapshotPkg   = publicModule + "/internal/snapshot"
		editprePkg    = publicModule + "/internal/editpreview"
		pathutilPkg   = publicModule + "/internal/pathutil"
		agentsPkg     = publicModule + "/internal/agents"
		catalogPkg    = publicModule + "/internal/catalog"
		atomicfsPkg   = publicModule + "/internal/atomicfs"
		promptPkg     = publicModule + "/internal/prompt"
		adaptationPkg = publicModule + "/internal/adaptation"
		legacyAgent   = publicModule + "/internal/agent"
		pluginsPkg    = publicModule + "/internal/plugins"
		shellparsePkg = publicModule + "/internal/shellparse"
		lspPkg        = publicModule + "/internal/lsp"
		cmdoutputPkg  = publicModule + "/internal/cmdoutput"
		jobsPkg       = publicModule + "/plugins/jobs"
		tasksPkg      = publicModule + "/internal/plugins/tasks"
	)
	var problems []string
	dir := path.Dir(rel)
	if dir != "model" && dir != "agent" && dir != "harness" &&
		(strings.HasPrefix(dir, "model/") || strings.HasPrefix(dir, "agent/") || strings.HasPrefix(dir, "harness/")) {
		pkg := dir[:strings.IndexByte(dir, '/')]
		return []string{rel + ": " + pkg + " must remain one public package without subpackages"}
	}
	if dir != "internal/storage" && strings.HasPrefix(dir, "internal/storage/") {
		return []string{rel + ": internal/storage must remain one package without backend subpackages"}
	}
	if dir != "plugins/jobs" && strings.HasPrefix(dir, "plugins/jobs/") {
		return []string{rel + ": plugins/jobs must remain one package without subpackages"}
	}
	for _, imp := range imports {
		switch dir {
		case "model":
			if !std[imp] {
				problems = append(problems, rel+": model package imports "+imp+"; model may import only the standard library")
			}
		case "agent":
			if imp != modelPkg && !std[imp] {
				problems = append(problems, rel+": agent package imports "+imp+"; agent may import only the standard library and "+modelPkg)
			}
		case "harness":
			if imp != modelPkg && imp != agentPkg && imp != tiktokenPkg && !std[imp] {
				problems = append(problems, rel+": harness package imports "+imp+"; harness may import only the standard library, "+modelPkg+", "+agentPkg+", and "+tiktokenPkg)
			}
		case "runtime":
			if imp != modelPkg && imp != harnessPkg && imp != configPkg && imp != agentsPkg && imp != catalogPkg && imp != atomicfsPkg && imp != promptPkg && imp != adaptationPkg && !std[imp] {
				problems = append(problems, rel+": runtime package imports "+imp+"; runtime may import only the standard library, "+modelPkg+", "+harnessPkg+", and the retained "+configPkg+", "+agentsPkg+", "+catalogPkg+", "+atomicfsPkg+", "+promptPkg+", and "+adaptationPkg+" helpers, never "+legacyAgent+", "+storagePkg+", any concrete plugin under "+pluginsPkg+", or the SQLite driver")
			}
		case "internal/storage":
			if imp != harnessPkg && imp != sqliteDriverPkg && !std[imp] {
				problems = append(problems, rel+": internal/storage imports "+imp+"; internal/storage may import only the standard library, "+harnessPkg+", and "+sqliteDriverPkg)
			}
		case "internal/plugins/sqlite":
			if imp != runtimePkg && imp != harnessPkg && imp != storagePkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/sqlite imports "+imp+"; the SQLite plugin may import only the standard library, "+runtimePkg+", "+harnessPkg+", and "+storagePkg)
			}
		case "internal/plugins/tools":
			if imp != runtimePkg && imp != harnessPkg && imp != modelPkg && imp != configPkg &&
				imp != toolPkg && imp != snapshotPkg && imp != editprePkg && imp != pathutilPkg &&
				imp != shellparsePkg && imp != jobsPkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/tools imports "+imp+"; the native tools plugin may import only the standard library, "+runtimePkg+", "+harnessPkg+", "+modelPkg+", "+jobsPkg+", and the retained "+toolPkg+", "+snapshotPkg+", "+editprePkg+", "+configPkg+", "+pathutilPkg+", and "+shellparsePkg+" helpers")
			}
		case "internal/plugins/adaptation":
			if imp != runtimePkg && imp != modelPkg && imp != adaptationPkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/adaptation imports "+imp+"; the adaptation plugin may import only the standard library, "+runtimePkg+", "+modelPkg+", and "+adaptationPkg)
			}
		case "internal/plugins/lsp":
			if imp != runtimePkg && imp != harnessPkg && imp != modelPkg && imp != lspPkg &&
				imp != snapshotPkg && imp != pathutilPkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/lsp imports "+imp+"; the LSP plugin may import only the standard library, "+runtimePkg+", "+harnessPkg+", "+modelPkg+", and the retained "+lspPkg+", "+snapshotPkg+", and "+pathutilPkg+" helpers")
			}
		case "plugins/jobs":
			if imp != harnessPkg && imp != runtimePkg && imp != cmdoutputPkg && !std[imp] {
				problems = append(problems, rel+": plugins/jobs imports "+imp+"; the jobs plugin may import only the standard library, "+harnessPkg+", "+runtimePkg+", and "+cmdoutputPkg)
			}
		case "internal/plugins/tasks":
			if imp != modelPkg && imp != harnessPkg && imp != runtimePkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/tasks imports "+imp+"; the child-session tool plugin may import only the standard library, "+modelPkg+", "+harnessPkg+", and "+runtimePkg)
			}
		case "internal/plugins/builtin":
			if imp != runtimePkg && imp != pluginsPkg+"/sqlite" && imp != pluginsPkg+"/tools" && imp != pluginsPkg+"/adaptation" && imp != pluginsPkg+"/lsp" && imp != jobsPkg && imp != tasksPkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/builtin imports "+imp+"; the shipped registration set may import only the standard library, "+runtimePkg+", and the six sibling plugins it registers")
			}
		default:
			if imp == modelPkg || imp == agentPkg || imp == harnessPkg || imp == storagePkg || imp == sqliteDriverPkg ||
				imp == runtimePkg || imp == pluginsPkg || strings.HasPrefix(imp, pluginsPkg+"/") ||
				imp == publicModule+"/plugins" || strings.HasPrefix(imp, publicModule+"/plugins/") {
				problems = append(problems, rel+": production file imports "+imp+"; no existing package may consume the public foundation before its cutover phase, no legacy, root, or adapter file may import the target runtime or its concrete plugins, and the SQLite driver is confined to "+storagePkg)
			}
		}
	}
	return problems
}

// TestDependencyRulesRejectNonStdlibDotlessImports proves the allowlists use authoritative standard-library membership, not a dot-in-path shape: the cgo pseudo-import "C" and a dotless path outside the stdlib set fail every package row, while ordinary stdlib imports, the model dependency on agent's side, and internal/storage's dependency on harness still pass. The foundation's own reverse and skipping edges — model or agent reaching harness, any target package reaching internal/storage, and the driver reaching a target package — are each rejected exactly once.
func TestDependencyRulesRejectNonStdlibDotlessImports(t *testing.T) {
	std := standardLibraryImports(t)
	for _, rel := range []string{"model/x.go", "agent/x.go", "harness/x.go", "internal/storage/x.go"} {
		for _, imp := range []string{"C", "notreal/pkg"} {
			if problems := checkTrackedGoFile(rel, []string{imp}, std); len(problems) != 1 {
				t.Errorf("%s importing %q: %d problems, want 1: %v", rel, imp, len(problems), problems)
			}
		}
	}
	if problems := checkTrackedGoFile("model/x.go", []string{"fmt", "net/http"}, std); len(problems) != 0 {
		t.Errorf("stdlib imports flagged in model: %v", problems)
	}
	if problems := checkTrackedGoFile("agent/x.go", []string{"fmt", publicModule + "/model"}, std); len(problems) != 0 {
		t.Errorf("allowed agent imports flagged: %v", problems)
	}
	if problems := checkTrackedGoFile("harness/x.go", []string{"fmt", "encoding/json"}, std); len(problems) != 0 {
		t.Errorf("stdlib imports flagged in harness: %v", problems)
	}
	if problems := checkTrackedGoFile("harness/x.go", []string{"fmt", publicModule + "/model"}, std); len(problems) != 0 {
		t.Errorf("allowed harness imports flagged: %v", problems)
	}
	if problems := checkTrackedGoFile("harness/x.go", []string{"fmt", publicModule + "/agent"}, std); len(problems) != 0 {
		t.Errorf("allowed harness-agent imports flagged: %v", problems)
	}
	if problems := checkTrackedGoFile("harness/x.go", []string{"fmt", tiktokenPkg}, std); len(problems) != 0 {
		t.Errorf("allowed harness tiktoken imports flagged: %v", problems)
	}
	if problems := checkTrackedGoFile("internal/storage/x.go", []string{"fmt", "context", publicModule + "/harness", sqliteDriverPkg}, std); len(problems) != 0 {
		t.Errorf("allowed internal/storage imports flagged: %v", problems)
	}
	for _, rel := range []string{"model/x.go", "agent/x.go", "harness/x.go"} {
		if problems := checkTrackedGoFile(rel, []string{sqliteDriverPkg}, std); len(problems) != 1 {
			t.Errorf("%s importing the sqlite driver: %d problems, want 1: %v", rel, len(problems), problems)
		}
	}
	for _, rel := range []string{"model/x.go", "agent/x.go"} {
		if problems := checkTrackedGoFile(rel, []string{tiktokenPkg}, std); len(problems) != 1 {
			t.Errorf("%s importing the tiktoken encoder: %d problems, want 1: %v", rel, len(problems), problems)
		}
	}
	for _, imp := range []string{publicModule + "/model", publicModule + "/agent"} {
		if problems := checkTrackedGoFile("internal/storage/x.go", []string{imp}, std); len(problems) != 1 {
			t.Errorf("internal/storage importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The SQLite plugin declares the ordinary Runtime-scoped plugin over the
	// public runtime and harness contracts and the storage backend it opens;
	// the legacy owner, the driver, and sibling plugins never enter it.
	if problems := checkTrackedGoFile("internal/plugins/sqlite/x.go", []string{"context", "path/filepath", publicModule + "/runtime", publicModule + "/harness", publicModule + "/internal/storage"}, std); len(problems) != 0 {
		t.Errorf("allowed internal/plugins/sqlite imports flagged: %v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/internal/agent", sqliteDriverPkg, publicModule + "/internal/plugins/memory"} {
		if problems := checkTrackedGoFile("internal/plugins/sqlite/x.go", []string{imp}, std); len(problems) != 1 {
			t.Errorf("internal/plugins/sqlite importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The native tools plugin declares the four file tools plus run_command,
	// process, and sleep over the public runtime and harness contracts, the
	// public model types, the jobs capability it requires, and the retained
	// shared file-preparation, snapshot, preview, config, path, and
	// shell-parse helpers it composes; the legacy owner, the driver, storage,
	// and the other sibling plugins never enter it.
	if problems := checkTrackedGoFile("internal/plugins/tools/x.go", []string{
		"bytes", "context", "encoding/json", "errors", "fmt", "io", "math", "path/filepath", "strings", "time",
		publicModule + "/model", publicModule + "/harness", publicModule + "/runtime",
		publicModule + "/internal/config", publicModule + "/internal/tool", publicModule + "/internal/snapshot",
		publicModule + "/internal/editpreview", publicModule + "/internal/pathutil",
		publicModule + "/internal/shellparse", publicModule + "/plugins/jobs",
	}, std); len(problems) != 0 {
		t.Errorf("allowed internal/plugins/tools imports flagged: %v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/plugins/memory", publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins"} {
		if problems := checkTrackedGoFile("internal/plugins/tools/x.go", []string{imp}, std); len(problems) != 1 {
			t.Errorf("internal/plugins/tools importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The adaptation plugin declares the single Runtime-scoped ModelAdaptation
	// export over the public runtime and model contracts and the bundled
	// internal/adaptation binding table it resolves; the legacy owner, the
	// prompt assembler, storage, the driver, and sibling plugins never enter it.
	if problems := checkTrackedGoFile("internal/plugins/adaptation/x.go", []string{"context", publicModule + "/model", publicModule + "/runtime", publicModule + "/internal/adaptation"}, std); len(problems) != 0 {
		t.Errorf("allowed internal/plugins/adaptation imports flagged: %v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/prompt", publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins/tools"} {
		if problems := checkTrackedGoFile("internal/plugins/adaptation/x.go", []string{imp}, std); len(problems) != 1 {
			t.Errorf("internal/plugins/adaptation importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The LSP plugin declares the diagnostics and workspace_symbol tools over
	// the public runtime, harness, and model contracts and the retained
	// internal/lsp, snapshot, and path helpers it composes; the legacy owner,
	// the legacy tools, storage, the driver, and sibling plugins never enter it.
	if problems := checkTrackedGoFile("internal/plugins/lsp/x.go", []string{
		"context", "encoding/json", "errors", "os", "path/filepath", "strings", "sync",
		publicModule + "/model", publicModule + "/harness", publicModule + "/runtime",
		publicModule + "/internal/lsp", publicModule + "/internal/snapshot", publicModule + "/internal/pathutil",
	}, std); len(problems) != 0 {
		t.Errorf("allowed internal/plugins/lsp imports flagged: %v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/tool", publicModule + "/internal/prompt", publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins/tools", publicModule + "/internal/plugins/adaptation", publicModule + "/internal/plugins"} {
		if problems := checkTrackedGoFile("internal/plugins/lsp/x.go", []string{imp}, std); len(problems) != 1 {
			t.Errorf("internal/plugins/lsp importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The jobs plugin declares the background jobs capability over the public
	// runtime and harness contracts and the internal/cmdoutput capture helper
	// it composes; the legacy owner, the process manager, storage, the driver,
	// and sibling plugins never enter it.
	if problems := checkTrackedGoFile("plugins/jobs/x.go", []string{
		"bytes", "context", "crypto/rand", "encoding/hex", "encoding/json", "fmt", "os", "os/exec",
		"path/filepath", "sort", "sync", "syscall", "time",
		publicModule + "/harness", publicModule + "/runtime", publicModule + "/internal/cmdoutput",
	}, std); len(problems) != 0 {
		t.Errorf("allowed plugins/jobs imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/internal/process", publicModule + "/internal/tool", publicModule + "/model",
		publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage",
		sqliteDriverPkg, publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins",
		publicModule + "/plugins/tasks",
	} {
		if problems := checkTrackedGoFile("plugins/jobs/x.go", []string{imp}, std); len(problems) != 1 {
			t.Errorf("plugins/jobs importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The child-session tool plugin declares the task tool over the public
	// model, harness, and runtime contracts; the legacy owner, the agents
	// loader, storage, the driver, and sibling plugins never enter it.
	if problems := checkTrackedGoFile("internal/plugins/tasks/x.go", []string{
		"encoding/json", "fmt",
		publicModule + "/model", publicModule + "/harness", publicModule + "/runtime",
	}, std); len(problems) != 0 {
		t.Errorf("allowed internal/plugins/tasks imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/agents",
		publicModule + "/internal/tool", publicModule + "/internal/storage", sqliteDriverPkg,
		publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins",
		publicModule + "/plugins/jobs",
	} {
		if problems := checkTrackedGoFile("internal/plugins/tasks/x.go", []string{imp}, std); len(problems) != 1 {
			t.Errorf("internal/plugins/tasks importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The runtime target layer consumes the public model and harness
	// contracts and the retained internal configuration helpers only; the
	// legacy owner, durable storage, concrete plugins, the public agent
	// package and the driver never enter it.
	if problems := checkTrackedGoFile("runtime/x.go", []string{"fmt", "encoding/json", publicModule + "/model", publicModule + "/harness", publicModule + "/internal/config", publicModule + "/internal/agents", publicModule + "/internal/catalog", publicModule + "/internal/atomicfs", publicModule + "/internal/prompt", publicModule + "/internal/adaptation"}, std); len(problems) != 0 {
		t.Errorf("allowed runtime imports flagged: %v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins/tasks"} {
		if problems := checkTrackedGoFile("runtime/x.go", []string{imp}, std); len(problems) != 1 {
			t.Errorf("runtime importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The deferred target packages never enter the runtime: internal/project
	// and internal/provider keep their own consumers until their owning
	// cutover or deletion phase.
	for _, imp := range []string{publicModule + "/internal/project", publicModule + "/internal/provider"} {
		if problems := checkTrackedGoFile("runtime/x.go", []string{imp}, std); len(problems) != 1 {
			t.Errorf("runtime importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The shipped registration set imports exactly the runtime contract and
	// the six sibling plugins it registers; every sibling helper, the legacy
	// owner, storage, the driver, and the public agent package never enter it.
	if problems := checkTrackedGoFile("internal/plugins/builtin/x.go", []string{
		"context", publicModule + "/runtime",
		publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins/tools",
		publicModule + "/internal/plugins/adaptation", publicModule + "/internal/plugins/lsp",
		publicModule + "/plugins/jobs", publicModule + "/internal/plugins/tasks",
	}, std); len(problems) != 0 {
		t.Errorf("allowed internal/plugins/builtin imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage",
		sqliteDriverPkg, publicModule + "/internal/tool", publicModule + "/internal/prompt",
		publicModule + "/internal/adaptation", publicModule + "/internal/lsp", publicModule + "/internal/plugins",
	} {
		if problems := checkTrackedGoFile("internal/plugins/builtin/x.go", []string{imp}, std); len(problems) != 1 {
			t.Errorf("internal/plugins/builtin importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// Legacy, root, and adapter production files never reach into the target
	// runtime or its concrete plugins.
	for _, row := range []struct{ rel, imp string }{
		{"internal/agent/x.go", publicModule + "/runtime"},
		{"main.go", publicModule + "/runtime"},
		{"app.go", publicModule + "/runtime"},
		{"internal/acp/x.go", publicModule + "/runtime"},
		{"internal/config/x.go", publicModule + "/runtime"},
		{"internal/anything/x.go", publicModule + "/internal/plugins/sqlite"},
		{"internal/anything/x.go", publicModule + "/internal/plugins"},
	} {
		if problems := checkTrackedGoFile(row.rel, []string{row.imp}, std); len(problems) != 1 {
			t.Errorf("%s importing %q: %d problems, want 1: %v", row.rel, row.imp, len(problems), problems)
		}
	}
	// The final foundation graph is model <- agent <- harness with
	// internal/storage beneath harness only: every reverse or skipping edge
	// between the foundation packages is rejected, and no foundation package
	// reaches the target runtime.
	for _, row := range []struct{ rel, imp string }{
		{"model/x.go", publicModule + "/agent"},
		{"model/x.go", publicModule + "/harness"},
		{"model/x.go", publicModule + "/internal/storage"},
		{"agent/x.go", publicModule + "/harness"},
		{"agent/x.go", publicModule + "/internal/storage"},
		{"harness/x.go", publicModule + "/internal/storage"},
		{"model/x.go", publicModule + "/runtime"},
		{"agent/x.go", publicModule + "/runtime"},
		{"harness/x.go", publicModule + "/runtime"},
	} {
		if problems := checkTrackedGoFile(row.rel, []string{row.imp}, std); len(problems) != 1 {
			t.Errorf("%s importing %q: %d problems, want 1: %v", row.rel, row.imp, len(problems), problems)
		}
	}
}

// TestDependencyRulesCheckEveryTrackedDirectory proves the file-set contract has no directory-name skipping: tracked-looking paths under previously-skipped names (.hidden, _underscore, frontend, node_modules, vendor), root files, and arbitrary internal/ paths are ordinary production files whose imports of any foundation package or the SQLite driver are reported, and internal/storage subpackages are rejected like public-package ones.
func TestDependencyRulesCheckEveryTrackedDirectory(t *testing.T) {
	std := standardLibraryImports(t)
	for _, rel := range []string{".hidden/pkg/x.go", "_scaffold/pkg/x.go", "frontend/bindata.go", "node_modules/pkg/x.go", "vendor/pkg/x.go", "main.go", "internal/anything/x.go"} {
		for _, imp := range []string{publicModule + "/model", publicModule + "/agent", publicModule + "/harness", publicModule + "/internal/storage", publicModule + "/runtime", publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins/tools", publicModule + "/internal/plugins/adaptation", publicModule + "/internal/plugins/lsp", publicModule + "/internal/plugins/builtin", publicModule + "/plugins/jobs", sqliteDriverPkg} {
			if problems := checkTrackedGoFile(rel, []string{imp}, std); len(problems) != 1 {
				t.Errorf("tracked %q importing %q: %d problems, want 1: %v", rel, imp, len(problems), problems)
			}
		}
	}
	for _, rel := range []string{"model/sub/x.go", "agent/sub/x.go", "harness/sub/x.go", "internal/storage/sub/x.go", "plugins/jobs/sub/x.go"} {
		if problems := checkTrackedGoFile(rel, []string{"fmt"}, std); len(problems) != 1 {
			t.Errorf("subpackage file %q: %d problems, want 1: %v", rel, len(problems), problems)
		}
	}
	if problems := checkTrackedGoFile("internal/model/x.go", []string{"fmt"}, std); len(problems) != 0 {
		t.Errorf("unrelated internal/model file wrongly treated as a foundation subpackage: %v", problems)
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
