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

	"github.com/MMinasyan/lightcode/internal/testassert"
)

const publicModule = "github.com/MMinasyan/lightcode"

// sqliteDriverPkg is the one third-party dependency the foundation permits:
// the SQLite driver is confined to internal/storage.
const sqliteDriverPkg = "github.com/mattn/go-sqlite3"

// oapiRuntimePkg is the one third-party dependency the protocol permits:
// generated HTTP parameter binding for its standard-library net/http server
// and client.
const oapiRuntimePkg = "github.com/oapi-codegen/runtime"

// kinOpenAPIPkg is the one third-party dependency the private protocol build
// command permits: the pinned schema loader that derives the native operation
// bindings and their invocation contract from the committed schema.
const kinOpenAPIPkg = "github.com/getkin/kin-openapi/openapi3"

// retiredPackageRoots is the one uniform rejection rule for the retired legacy
// package roots selected by this modernization: no tracked production file may
// live under one of these internal package trees or import one. The list is not
// a census of every historical directory name; the generic role rules still
// reject arbitrary new packages that try to compose owner or client
// authorities. Matching is on complete path segments, so the retained
// internal/agents, internal/acpclient and internal/cliclient packages are never
// confused with their retired neighbours, and the public model, agent and
// client packages never match.
var retiredPackageRoots = []string{
	"internal/agent",
	"internal/engine",
	"internal/provider",
	"internal/process",
	"internal/compact",
	"internal/cli",
	"internal/acp",
	"internal/project",
	"internal/permission",
	"internal/memory",
}

// retiredRoot reports the retired legacy package tree that one module-relative
// directory path or module-relative import suffix lives under or names, or ""
// when it names no retired tree.
func retiredRoot(p string) string {
	for _, root := range retiredPackageRoots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return root
		}
	}
	return ""
}

// The Wails framework packages the native desktop host is allowed to reach.
const (
	wailsPkg        = "github.com/wailsapp/wails/v2"
	wailsRuntimePkg = "github.com/wailsapp/wails/v2/pkg/runtime"
	wailsOptionsPkg = "github.com/wailsapp/wails/v2/pkg/options"
	wailsAssetPkg   = "github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsLinuxPkg   = "github.com/wailsapp/wails/v2/pkg/options/linux"
)

// TestPublicFoundationDependencyIsolation applies the one dependency baseline
// to the authoritative complete set of Git-tracked non-test Go files: every
// removed legacy package tree is rejected uniformly (a file living under one or
// importing one, before any role handling), and each retained package is pinned
// to the imports its role allows — the public model/agent/harness foundation,
// the runtime owner and its retained helpers, the shipped plugins, the client
// hosts, the generated contracts, and the independent utilities. The tracked
// set must hold both public packages, every problem is reported as its own
// error, and no file-set shortcut may skip a tracked path.
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
	testassert.Require(t, modelFiles != 0 && agentFiles != 0, "tracked set holds %d model and %d agent production files from %s; both packages must be present", modelFiles, agentFiles, root)
}

// checkTrackedGoFile applies the dependency baseline to one tracked production file.
func checkTrackedGoFile(rel string, imports []string, std map[string]bool) []string {
	const (
		modelPkg       = publicModule + "/model"
		agentPkg       = publicModule + "/agent"
		harnessPkg     = publicModule + "/harness"
		storagePkg     = publicModule + "/internal/storage"
		runtimePkg     = publicModule + "/runtime"
		configPkg      = publicModule + "/internal/config"
		toolPkg        = publicModule + "/internal/tool"
		snapshotPkg    = publicModule + "/internal/snapshot"
		editprePkg     = publicModule + "/internal/editpreview"
		pathutilPkg    = publicModule + "/internal/pathutil"
		safefsPkg      = publicModule + "/internal/safefs"
		agentsPkg      = publicModule + "/internal/agents"
		catalogPkg     = publicModule + "/internal/catalog"
		atomicfsPkg    = publicModule + "/internal/atomicfs"
		promptPkg      = publicModule + "/internal/prompt"
		adaptationPkg  = publicModule + "/internal/adaptation"
		pluginsPkg     = publicModule + "/internal/plugins"
		shellparsePkg  = publicModule + "/internal/shellparse"
		lspPkg         = publicModule + "/internal/lsp"
		cmdoutputPkg   = publicModule + "/internal/cmdoutput"
		jobsPkg        = publicModule + "/plugins/jobs"
		tasksPkg       = publicModule + "/internal/plugins/tasks"
		toolargsPkg    = publicModule + "/internal/toolargs"
		testassertPkg  = publicModule + "/internal/testassert"
		protocolPkg    = publicModule + "/protocol"
		clientPkg      = publicModule + "/client"
		clienthostPkg  = publicModule + "/internal/clienthost"
		clientviewPkg  = publicModule + "/internal/clientview"
		clientopsPkg   = publicModule + "/internal/clientops"
		acpclientPkg   = publicModule + "/internal/acpclient"
		desktopPkg     = publicModule + "/internal/desktopclient"
		cliclientPkg   = publicModule + "/internal/cliclient"
		hostprocessPkg = publicModule + "/internal/hostprocess"
		versionPkg     = publicModule + "/internal/version"
		doctorPkg      = publicModule + "/internal/doctor"
		selfupdatePkg  = publicModule + "/internal/selfupdate"
		builtinPkg     = publicModule + "/internal/plugins/builtin"
	)
	var problems []string
	dir := path.Dir(rel)
	// Two disjoint checks share the retired-tree rule. The source-path check
	// runs before any role handling and rejects a tracked production file that
	// lives under a retired package tree, exactly like the subpackage guards
	// below: no file in a retired tree is ever classified. The import check
	// lives in the per-import loop, so a retired import is reported once and
	// independent violations of other imports still surface. Both trim or
	// bound on the module path / segment boundaries, so the retained
	// internal/agents, internal/acpclient and internal/cliclient packages stay
	// unaffected.
	if root := retiredRoot(dir); root != "" {
		return []string{rel + ": the " + root + " package tree is retired; no tracked production file may live under it"}
	}
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
		// The retired-tree import classification runs before exact roles: one
		// problem per offending import, and that import never enters role
		// handling, so independent violations of the file's other imports
		// still surface.
		if strings.HasPrefix(imp, publicModule+"/") {
			if root := retiredRoot(strings.TrimPrefix(imp, publicModule+"/")); root != "" {
				problems = append(problems, rel+": imports "+imp+"; the "+root+" package tree is retired and may not be imported by any production file")
				continue
			}
		}
		// Exact roles for the active root entry files: the root entry
		// composes the shipped clients, the command dispatcher reaches the
		// shared startup helper and its independent utilities, and the
		// runtime command alone composes the production Runtime owner over
		// the public runtime contract and the shipped plugin set.
		switch rel {
		case "main.go":
			if imp != clienthostPkg && imp != desktopPkg && imp != cliclientPkg && imp != acpclientPkg && !std[imp] {
				problems = append(problems, rel+": main.go imports "+imp+"; the root entry may import only the standard library and "+clienthostPkg+", "+desktopPkg+", "+cliclientPkg+", and "+acpclientPkg)
			}
			continue
		case "commands.go":
			if imp != clienthostPkg && imp != configPkg && imp != doctorPkg && imp != selfupdatePkg && imp != versionPkg && !std[imp] {
				problems = append(problems, rel+": commands.go imports "+imp+"; the command dispatcher may import only the standard library and "+clienthostPkg+", "+configPkg+", "+doctorPkg+", "+selfupdatePkg+", and "+versionPkg)
			}
			continue
		case "runtime_command.go":
			if imp != runtimePkg && imp != builtinPkg && !std[imp] {
				problems = append(problems, rel+": runtime_command.go imports "+imp+"; the runtime command may import only the standard library, "+runtimePkg+", and "+builtinPkg)
			}
			continue
		}
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
			if imp != modelPkg && imp != agentPkg && !std[imp] {
				problems = append(problems, rel+": harness package imports "+imp+"; harness may import only the standard library, "+modelPkg+", and "+agentPkg)
			}
		case "runtime":
			if imp != modelPkg && imp != harnessPkg && imp != protocolPkg && imp != configPkg && imp != agentsPkg && imp != catalogPkg && imp != atomicfsPkg && imp != promptPkg && imp != adaptationPkg && imp != snapshotPkg && imp != safefsPkg && imp != pathutilPkg && !std[imp] {
				problems = append(problems, rel+": runtime package imports "+imp+"; runtime may import only the standard library, "+modelPkg+", "+harnessPkg+", "+protocolPkg+", and the retained "+configPkg+", "+agentsPkg+", "+catalogPkg+", "+atomicfsPkg+", "+promptPkg+", "+adaptationPkg+", "+snapshotPkg+", "+safefsPkg+", and "+pathutilPkg+" helpers, never "+storagePkg+", any concrete plugin under "+pluginsPkg+", or the SQLite driver")
			}
		case "internal/catalog":
			if imp != modelPkg && imp != atomicfsPkg && !std[imp] {
				problems = append(problems, rel+": internal/catalog imports "+imp+"; the catalog may import only the standard library, "+atomicfsPkg+", and the public "+modelPkg+" identity it forwards")
			}
		case "internal/agents":
			if imp != modelPkg && !std[imp] {
				problems = append(problems, rel+": internal/agents imports "+imp+"; the agents definitions may import only the standard library and the public "+modelPkg+" identity parsing")
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
				imp != shellparsePkg && imp != jobsPkg && imp != toolargsPkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/tools imports "+imp+"; the native tools plugin may import only the standard library, "+runtimePkg+", "+harnessPkg+", "+modelPkg+", "+jobsPkg+", "+toolargsPkg+", and the retained "+toolPkg+", "+snapshotPkg+", "+editprePkg+", "+configPkg+", "+pathutilPkg+", and "+shellparsePkg+" helpers")
			}
		case "internal/plugins/adaptation":
			if imp != runtimePkg && imp != modelPkg && imp != adaptationPkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/adaptation imports "+imp+"; the adaptation plugin may import only the standard library, "+runtimePkg+", "+modelPkg+", and "+adaptationPkg)
			}
		case "internal/plugins/lsp":
			if imp != runtimePkg && imp != harnessPkg && imp != modelPkg && imp != lspPkg &&
				imp != snapshotPkg && imp != pathutilPkg && imp != toolargsPkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/lsp imports "+imp+"; the LSP plugin may import only the standard library, "+runtimePkg+", "+harnessPkg+", "+modelPkg+", "+toolargsPkg+", and the retained "+lspPkg+", "+snapshotPkg+", and "+pathutilPkg+" helpers")
			}
		case "plugins/jobs":
			if imp != harnessPkg && imp != runtimePkg && imp != cmdoutputPkg && !std[imp] {
				problems = append(problems, rel+": plugins/jobs imports "+imp+"; the jobs plugin may import only the standard library, "+harnessPkg+", "+runtimePkg+", and "+cmdoutputPkg)
			}
		case "internal/plugins/tasks":
			if imp != modelPkg && imp != harnessPkg && imp != runtimePkg && imp != toolargsPkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/tasks imports "+imp+"; the child-session tool plugin may import only the standard library, "+modelPkg+", "+harnessPkg+", "+runtimePkg+", and "+toolargsPkg)
			}
		case "internal/toolargs":
			if !std[imp] {
				problems = append(problems, rel+": internal/toolargs imports "+imp+"; the shared tool-argument decoder may import only the standard library")
			}
		case "internal/testassert":
			if !std[imp] {
				problems = append(problems, rel+": internal/testassert imports "+imp+"; the shared test assertion helper may import only the standard library")
			}
		case "internal/plugins/builtin":
			if imp != runtimePkg && imp != pluginsPkg+"/sqlite" && imp != pluginsPkg+"/tools" && imp != pluginsPkg+"/adaptation" && imp != pluginsPkg+"/lsp" && imp != jobsPkg && imp != tasksPkg && !std[imp] {
				problems = append(problems, rel+": internal/plugins/builtin imports "+imp+"; the shipped registration set may import only the standard library, "+runtimePkg+", and the six sibling plugins it registers")
			}
		case "protocol":
			if imp != oapiRuntimePkg && !std[imp] {
				problems = append(problems, rel+": protocol package imports "+imp+"; protocol may import only the standard library and "+oapiRuntimePkg)
			}
		case "client":
			if imp != protocolPkg && !std[imp] {
				problems = append(problems, rel+": client package imports "+imp+"; client may import only the standard library and "+protocolPkg)
			}
		case "internal/terminal":
			if imp != editprePkg && !std[imp] {
				problems = append(problems, rel+": internal/terminal imports "+imp+"; terminal may import only the standard library and "+editprePkg)
			}
		case "internal/hostprocess":
			if !std[imp] {
				problems = append(problems, rel+": internal/hostprocess imports "+imp+"; hostprocess may import only the standard library")
			}
		case "internal/clienthost":
			if imp != clientPkg && imp != protocolPkg && imp != hostprocessPkg && imp != clientopsPkg && !std[imp] {
				problems = append(problems, rel+": internal/clienthost imports "+imp+"; clienthost may import only the standard library, "+clientPkg+", "+protocolPkg+", "+hostprocessPkg+", and "+clientopsPkg)
			}
		case "internal/clientview":
			if imp != protocolPkg && !std[imp] {
				problems = append(problems, rel+": internal/clientview imports "+imp+"; clientview may import only the standard library and "+protocolPkg)
			}
		case "internal/clientops":
			if imp != clientPkg && imp != protocolPkg && !std[imp] {
				problems = append(problems, rel+": internal/clientops imports "+imp+"; the shared typed invocation helper may import only the standard library, "+clientPkg+", and "+protocolPkg)
			}
		case "internal/protocolgen":
			if imp != kinOpenAPIPkg && !std[imp] {
				problems = append(problems, rel+": internal/protocolgen imports "+imp+"; the private protocol build command may import only the standard library and "+kinOpenAPIPkg)
			}
		case "internal/acpclient":
			if imp != clientPkg && imp != protocolPkg && imp != clienthostPkg && imp != clientopsPkg && imp != versionPkg && !std[imp] {
				problems = append(problems, rel+": internal/acpclient imports "+imp+"; the target ACP bridge may import only the standard library, "+clientPkg+", "+protocolPkg+", "+clienthostPkg+", "+clientopsPkg+", and the native version helper")
			}
		case "internal/cliclient":
			if imp != clientPkg && imp != protocolPkg && imp != clienthostPkg && imp != clientviewPkg && imp != clientopsPkg && imp != publicModule+"/internal/terminal" && imp != "golang.org/x/term" && !std[imp] {
				problems = append(problems, rel+": internal/cliclient imports "+imp+"; the terminal client may import only stdlib, client, protocol, clienthost, clientview, clientops, terminal, and golang.org/x/term")
			}
		case "internal/desktopclient":
			if imp != clientPkg && imp != protocolPkg && imp != clienthostPkg && imp != clientopsPkg && imp != versionPkg &&
				imp != wailsPkg && imp != wailsRuntimePkg && imp != wailsOptionsPkg && imp != wailsAssetPkg && imp != wailsLinuxPkg && !std[imp] {
				problems = append(problems, rel+": internal/desktopclient imports "+imp+"; the desktop host may import only the standard library, "+clientPkg+", "+protocolPkg+", "+clienthostPkg+", "+clientopsPkg+", the Wails framework, and the native version helper")
			}
		default:
			if imp == clienthostPkg || imp == clientviewPkg || imp == clientopsPkg || imp == acpclientPkg || imp == desktopPkg || imp == cliclientPkg {
				problems = append(problems, rel+": production file imports "+imp+"; the shared startup, scalar projection, and typed invocation helpers and the desktop host and ACP bridge are reserved for their exact target consumers, and no legacy, root, or adapter file may start a Runtime or adopt target view or invocation rules before the coordinated cutover")
			} else if imp == modelPkg || imp == agentPkg || imp == harnessPkg || imp == storagePkg || imp == sqliteDriverPkg ||
				imp == runtimePkg || imp == pluginsPkg || strings.HasPrefix(imp, pluginsPkg+"/") ||
				imp == publicModule+"/plugins" || strings.HasPrefix(imp, publicModule+"/plugins/") ||
				imp == toolargsPkg || imp == protocolPkg || imp == testassertPkg {
				problems = append(problems, rel+": production file imports "+imp+"; no existing package may consume the public foundation before its cutover phase, no legacy, root, or adapter file may import the target runtime, its concrete plugins, or the protocol contracts, the SQLite driver is confined to "+storagePkg+", the shared tool-argument decoder is confined to its three plugin consumers, and the shared test assertion helper is confined to test files")
			}
		}
	}
	return problems
}

// TestDependencyRulesRejectNonStdlibDotlessImports proves the allowlists use authoritative standard-library membership, not a dot-in-path shape: the cgo pseudo-import "C" and a dotless path outside the stdlib set fail every package row, while ordinary stdlib imports, the model dependency on agent's side, and internal/storage's dependency on harness still pass. The foundation's own reverse and skipping edges — model or agent reaching harness, any target package reaching internal/storage, and the driver reaching a target package — are each rejected exactly once.
func TestDependencyRulesRejectNonStdlibDotlessImports(t *testing.T) {
	std := standardLibraryImports(t)
	for _, rel := range []string{"model/x.go", "agent/x.go", "harness/x.go", "internal/storage/x.go", "internal/toolargs/x.go", "internal/testassert/x.go", "protocol/x.go", "client/x.go", "internal/terminal/x.go", "internal/hostprocess/x.go", "internal/clienthost/x.go", "internal/clientview/x.go", "internal/clientops/x.go", "internal/protocolgen/x.go", "internal/acpclient/x.go", "internal/cliclient/x.go", "internal/desktopclient/x.go"} {
		for _, imp := range []string{"C", "notreal/pkg"} {
			{
				problems := checkTrackedGoFile(rel, []string{imp}, std)
				testassert.Check(t, len(problems) == 1, "%s importing %q: %d problems, want 1: %v", rel, imp, len(problems), problems)
			}
		}
	}
	{
		problems := checkTrackedGoFile("model/x.go", []string{"fmt", "net/http"}, std)
		testassert.Check(t, len(problems) == 0, "stdlib imports flagged in model: %v", problems)
	}
	{
		problems := checkTrackedGoFile("agent/x.go", []string{"fmt", publicModule + "/model"}, std)
		testassert.Check(t, len(problems) == 0, "allowed agent imports flagged: %v", problems)
	}
	{
		problems := checkTrackedGoFile("harness/x.go", []string{"fmt", "encoding/json"}, std)
		testassert.Check(t, len(problems) == 0, "stdlib imports flagged in harness: %v", problems)
	}
	{
		problems := checkTrackedGoFile("harness/x.go", []string{"fmt", publicModule + "/model"}, std)
		testassert.Check(t, len(problems) == 0, "allowed harness imports flagged: %v", problems)
	}
	{
		problems := checkTrackedGoFile("harness/x.go", []string{"fmt", publicModule + "/agent"}, std)
		testassert.Check(t, len(problems) == 0, "allowed harness-agent imports flagged: %v", problems)
	}
	{
		problems := checkTrackedGoFile("internal/storage/x.go", []string{"fmt", "context", publicModule + "/harness", sqliteDriverPkg}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/storage imports flagged: %v", problems)
	}
	for _, rel := range []string{"model/x.go", "agent/x.go", "harness/x.go"} {
		{
			problems := checkTrackedGoFile(rel, []string{sqliteDriverPkg}, std)
			testassert.Check(t, len(problems) == 1, "%s importing the sqlite driver: %d problems, want 1: %v", rel, len(problems), problems)
		}
	}
	for _, imp := range []string{publicModule + "/model", publicModule + "/agent"} {
		{
			problems := checkTrackedGoFile("internal/storage/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/storage importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The SQLite plugin declares the ordinary Runtime-scoped plugin over the
	// public runtime and harness contracts and the storage backend it opens;
	// the legacy owner, the driver, and sibling plugins never enter it.
	{
		problems := checkTrackedGoFile("internal/plugins/sqlite/x.go", []string{"context", "path/filepath", publicModule + "/runtime", publicModule + "/harness", publicModule + "/internal/storage"}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/plugins/sqlite imports flagged: %v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/internal/agent", sqliteDriverPkg, publicModule + "/internal/plugins/memory"} {
		{
			problems := checkTrackedGoFile("internal/plugins/sqlite/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/plugins/sqlite importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The native tools plugin declares the four file tools plus run_command,
	// process, and sleep over the public runtime and harness contracts, the
	// public model types, the jobs capability it requires, and the retained
	// shared file-preparation, snapshot, preview, config, path, and
	// shell-parse helpers it composes; the legacy owner, the driver, storage,
	// and the other sibling plugins never enter it.
	{
		problems := checkTrackedGoFile("internal/plugins/tools/x.go", []string{
			"bytes", "context", "encoding/json", "errors", "fmt", "io", "math", "path/filepath", "strings", "time",
			publicModule + "/model", publicModule + "/harness", publicModule + "/runtime",
			publicModule + "/internal/config", publicModule + "/internal/tool", publicModule + "/internal/snapshot",
			publicModule + "/internal/editpreview", publicModule + "/internal/pathutil",
			publicModule + "/internal/shellparse", publicModule + "/internal/toolargs", publicModule + "/plugins/jobs",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/plugins/tools imports flagged: %v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/plugins/memory", publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins"} {
		{
			problems := checkTrackedGoFile("internal/plugins/tools/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/plugins/tools importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The adaptation plugin declares the single Runtime-scoped ModelAdaptation
	// export over the public runtime and model contracts and the bundled
	// internal/adaptation binding table it resolves; the legacy owner, the
	// prompt assembler, storage, the driver, and sibling plugins never enter it.
	{
		problems := checkTrackedGoFile("internal/plugins/adaptation/x.go", []string{"context", publicModule + "/model", publicModule + "/runtime", publicModule + "/internal/adaptation"}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/plugins/adaptation imports flagged: %v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/prompt", publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins/tools"} {
		{
			problems := checkTrackedGoFile("internal/plugins/adaptation/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/plugins/adaptation importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The LSP plugin declares the diagnostics and workspace_symbol tools over
	// the public runtime, harness, and model contracts and the retained
	// internal/lsp, snapshot, and path helpers it composes; the legacy owner,
	// the legacy tools, storage, the driver, and sibling plugins never enter it.
	{
		problems := checkTrackedGoFile("internal/plugins/lsp/x.go", []string{
			"context", "encoding/json", "errors", "os", "path/filepath", "strings", "sync",
			publicModule + "/model", publicModule + "/harness", publicModule + "/runtime",
			publicModule + "/internal/lsp", publicModule + "/internal/snapshot", publicModule + "/internal/pathutil",
			publicModule + "/internal/toolargs",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/plugins/lsp imports flagged: %v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/tool", publicModule + "/internal/prompt", publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins/tools", publicModule + "/internal/plugins/adaptation", publicModule + "/internal/plugins"} {
		{
			problems := checkTrackedGoFile("internal/plugins/lsp/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/plugins/lsp importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The jobs plugin declares the background jobs capability over the public
	// runtime and harness contracts and the internal/cmdoutput capture helper
	// it composes; the legacy owner, the process manager, storage, the driver,
	// and sibling plugins never enter it.
	{
		problems := checkTrackedGoFile("plugins/jobs/x.go", []string{
			"bytes", "context", "crypto/rand", "encoding/hex", "encoding/json", "fmt", "os", "os/exec",
			"path/filepath", "sort", "sync", "syscall", "time",
			publicModule + "/harness", publicModule + "/runtime", publicModule + "/internal/cmdoutput",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed plugins/jobs imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/internal/process", publicModule + "/internal/tool", publicModule + "/model",
		publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage",
		sqliteDriverPkg, publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins",
		publicModule + "/plugins/tasks",
	} {
		{
			problems := checkTrackedGoFile("plugins/jobs/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "plugins/jobs importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The child-session tool plugin declares the task tool over the public
	// model, harness, and runtime contracts; the legacy owner, the agents
	// loader, storage, the driver, and sibling plugins never enter it.
	{
		problems := checkTrackedGoFile("internal/plugins/tasks/x.go", []string{
			"encoding/json", "fmt",
			publicModule + "/model", publicModule + "/harness", publicModule + "/runtime",
			publicModule + "/internal/toolargs",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/plugins/tasks imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/agents",
		publicModule + "/internal/tool", publicModule + "/internal/storage", sqliteDriverPkg,
		publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins",
		publicModule + "/plugins/jobs",
	} {
		{
			problems := checkTrackedGoFile("internal/plugins/tasks/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/plugins/tasks importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The shared tool-argument decoder is the strict JSON-object data-domain
	// helper over the standard library only; the foundation contracts, the
	// target runtime and harness, the plugin tree, its sibling helpers, and
	// durable storage never enter it, and its only consumers are the tools,
	// tasks and LSP plugins.
	{
		problems := checkTrackedGoFile("internal/toolargs/x.go", []string{"bytes", "encoding/json", "errors", "fmt", "io"}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/toolargs imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/agent", publicModule + "/harness", publicModule + "/runtime",
		publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/tool",
		publicModule + "/internal/plugins", publicModule + "/internal/plugins/tools",
	} {
		{
			problems := checkTrackedGoFile("internal/toolargs/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/toolargs importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The tools, tasks and LSP plugin rows hold the only allowed edges into
	// the shared decoder; the nearest forbidden siblings — the other
	// concrete plugins, the runtime owner, and any unrelated package —
	// importing it stay rejected exactly once.
	for _, rel := range []string{
		"internal/plugins/sqlite/x.go", "internal/plugins/adaptation/x.go", "plugins/jobs/x.go",
		"internal/plugins/builtin/x.go", "runtime/x.go", "internal/anything/x.go",
	} {
		{
			problems := checkTrackedGoFile(rel, []string{publicModule + "/internal/toolargs"}, std)
			testassert.Check(t, len(problems) == 1, "%s importing the shared tool-argument decoder: %d problems, want 1: %v", rel, len(problems), problems)
		}
	}
	// The shared assertion helper admits only standard-library imports; the
	// production foundation packages are its nearest forbidden siblings and
	// never enter it outside test files.
	{
		problems := checkTrackedGoFile("internal/testassert/x.go", []string{"testing", "fmt"}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/testassert imports flagged: %v", problems)
	}
	for _, rel := range []string{"model/x.go", "agent/x.go", "harness/x.go", "runtime/x.go"} {
		{
			problems := checkTrackedGoFile(rel, []string{publicModule + "/internal/testassert"}, std)
			testassert.Check(t, len(problems) == 1, "%s importing the shared test assertion helper: %d problems, want 1: %v", rel, len(problems), problems)
		}
	}
	// The retained catalog and agents helpers carry the one allowed edge
	// into the public model identity; the catalog keeps its atomic file
	// helper, the agents definitions do not; the foundation layers above
	// model, the target runtime, storage, the driver and the plugin tree
	// are the nearest forbidden siblings and never enter them.
	{
		problems := checkTrackedGoFile("internal/catalog/x.go", []string{"fmt", "encoding/json", publicModule + "/model", publicModule + "/internal/atomicfs"}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/catalog imports flagged: %v", problems)
	}
	{
		problems := checkTrackedGoFile("internal/agents/x.go", []string{"fmt", "encoding/json", publicModule + "/model"}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/agents imports flagged: %v", problems)
	}
	for _, rel := range []string{"internal/catalog/x.go", "internal/agents/x.go"} {
		for _, imp := range []string{publicModule + "/agent", publicModule + "/harness", publicModule + "/runtime", publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/plugins"} {
			{
				problems := checkTrackedGoFile(rel, []string{imp}, std)
				testassert.Check(t, len(problems) == 1, "%s importing %q: %d problems, want 1: %v", rel, imp, len(problems), problems)
			}
		}
	}
	{
		problems := checkTrackedGoFile("internal/agents/x.go", []string{publicModule + "/internal/atomicfs"}, std)
		testassert.Check(t, len(problems) == 1, "internal/agents importing the atomic file helper: %d problems, want 1: %v", len(problems), problems)
	}
	// The runtime target layer consumes the public model, harness, and
	// protocol contracts and the retained internal configuration, snapshot,
	// safefs and path helpers only; the removed legacy trees (already
	// rejected uniformly above), durable storage, concrete plugins, the
	// public agent package and the driver never enter it.
	{
		problems := checkTrackedGoFile("runtime/x.go", []string{"fmt", "encoding/json", publicModule + "/model", publicModule + "/harness", publicModule + "/protocol", publicModule + "/internal/config", publicModule + "/internal/agents", publicModule + "/internal/catalog", publicModule + "/internal/atomicfs", publicModule + "/internal/prompt", publicModule + "/internal/adaptation", publicModule + "/internal/snapshot", publicModule + "/internal/safefs", publicModule + "/internal/pathutil"}, std)
		testassert.Check(t, len(problems) == 0, "allowed runtime imports flagged: %v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins/tasks"} {
		{
			problems := checkTrackedGoFile("runtime/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "runtime importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The shipped registration set imports exactly the runtime contract and
	// the six sibling plugins it registers; every sibling helper, the legacy
	// owner, storage, the driver, and the public agent package never enter it.
	{
		problems := checkTrackedGoFile("internal/plugins/builtin/x.go", []string{
			"context", publicModule + "/runtime",
			publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins/tools",
			publicModule + "/internal/plugins/adaptation", publicModule + "/internal/plugins/lsp",
			publicModule + "/plugins/jobs", publicModule + "/internal/plugins/tasks",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/plugins/builtin imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/agent", publicModule + "/internal/agent", publicModule + "/internal/storage",
		sqliteDriverPkg, publicModule + "/internal/tool", publicModule + "/internal/prompt",
		publicModule + "/internal/adaptation", publicModule + "/internal/lsp", publicModule + "/internal/plugins",
	} {
		{
			problems := checkTrackedGoFile("internal/plugins/builtin/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/plugins/builtin importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The generated protocol package carries only the schema-produced
	// contracts over the standard library and the oapi-codegen runtime
	// helper its generated parameter binding needs; the model, agent, and
	// harness contracts, the target runtime, and the legacy owner never
	// enter it, and the runtime's protocol import is the one permitted
	// consumption edge of the generated contracts.
	{
		problems := checkTrackedGoFile("protocol/x.go", []string{
			"context", "encoding/json", "fmt", "net/http", "net/url", "strings",
			oapiRuntimePkg,
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed protocol imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/agent", publicModule + "/harness",
		publicModule + "/runtime", publicModule + "/internal/agent",
		publicModule + "/internal/storage", sqliteDriverPkg,
	} {
		{
			problems := checkTrackedGoFile("protocol/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "protocol importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The client wrapper consumes the generated protocol contracts and the
	// standard library only; the Runtime, Harness, model, agent, and
	// internal helper packages never enter it.
	{
		problems := checkTrackedGoFile("client/x.go", []string{
			"bufio", "bytes", "context", "encoding/json", "errors", "fmt", "io",
			"net/http", "net/url", "os", "strconv", "strings",
			publicModule + "/protocol",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed client imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/agent", publicModule + "/harness",
		publicModule + "/runtime", publicModule + "/internal/agent",
		publicModule + "/internal/config", publicModule + "/internal/storage",
		sqliteDriverPkg, oapiRuntimePkg, publicModule + "/internal/plugins",
	} {
		{
			problems := checkTrackedGoFile("client/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "client importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The shared terminal presentation package consumes the standard library
	// and the retained edit-preview helper only; the legacy owner, the
	// foundation contracts, the target runtime and the generated protocol
	// contracts never enter it.
	{
		problems := checkTrackedGoFile("internal/terminal/x.go", []string{
			"bytes", "context", "encoding/base64", "encoding/json", "fmt", "io",
			"os/exec", "strings", "sync", "unicode", publicModule + "/internal/editpreview",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/terminal imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/agent", publicModule + "/harness",
		publicModule + "/runtime", publicModule + "/internal/agent", publicModule + "/internal/cli",
		publicModule + "/internal/storage", sqliteDriverPkg, oapiRuntimePkg,
	} {
		{
			problems := checkTrackedGoFile("internal/terminal/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/terminal importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// The host process helper is the standard library only; the legacy owner,
	// the terminal presentation package, and every foundation or target
	// contract never enter it.
	{
		problems := checkTrackedGoFile("internal/hostprocess/x.go", []string{"os/exec", "syscall"}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/hostprocess imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/internal/cli", publicModule + "/internal/terminal",
		publicModule + "/internal/storage", sqliteDriverPkg, oapiRuntimePkg,
	} {
		{
			problems := checkTrackedGoFile("internal/hostprocess/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/hostprocess importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// Root and retained helper files cannot reach the Runtime, concrete plugins,
	// or protocol contracts outside their declared roles.
	for _, row := range []struct{ rel, imp string }{
		{"main.go", publicModule + "/runtime"},
		{"app.go", publicModule + "/runtime"},
		{"internal/config/x.go", publicModule + "/runtime"},
		{"main.go", publicModule + "/protocol"},
		{"app.go", publicModule + "/protocol"},
		{"internal/config/x.go", publicModule + "/protocol"},
		{"internal/anything/x.go", publicModule + "/internal/plugins/sqlite"},
		{"internal/anything/x.go", publicModule + "/internal/plugins"},
	} {
		{
			problems := checkTrackedGoFile(row.rel, []string{row.imp}, std)
			testassert.Check(t, len(problems) == 1, "%s importing %q: %d problems, want 1: %v", row.rel, row.imp, len(problems), problems)
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
		{
			problems := checkTrackedGoFile(row.rel, []string{row.imp}, std)
			testassert.Check(t, len(problems) == 1, "%s importing %q: %d problems, want 1: %v", row.rel, row.imp, len(problems), problems)
		}
	}
}

// TestDependencyRulesRejectRetiredPackages exercises the one uniform rule for
// the selected retired package roots. The expected root list is literal on
// purpose: it is independent of the implementation's own table, so a deleted or
// misspelled retiredPackageRoots entry fails here. Every root must refuse one
// generic bare import and one file that lives under its own source path; the
// subpath branch and the mixed allowed/forbidden reporting are exercised once
// with one representative root; and the real retained neighbour source paths
// (internal/agents, internal/acpclient, internal/cliclient) are never treated as
// retired by the segment-bounded matching.
func TestDependencyRulesRejectRetiredPackages(t *testing.T) {
	std := standardLibraryImports(t)
	for _, root := range []string{
		"internal/agent",
		"internal/engine",
		"internal/provider",
		"internal/process",
		"internal/compact",
		"internal/cli",
		"internal/acp",
		"internal/project",
		"internal/permission",
		"internal/memory",
	} {
		imp := publicModule + "/" + root
		{
			problems := checkTrackedGoFile("internal/anything/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "generic file importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
		{
			problems := checkTrackedGoFile(root+"/x.go", []string{"fmt"}, std)
			testassert.Check(t, len(problems) == 1, "file under %s: %d problems, want 1: %v", root, len(problems), problems)
		}
	}
	// One representative root covers the subpath branch and the mixed rows:
	// beside an allowed import the refusal is exactly one problem, and beside a
	// forbidden import (main.go may import fmt but not the public model
	// package) both offending imports surface.
	const representative = "internal/agent"
	rep := publicModule + "/" + representative
	{
		problems := checkTrackedGoFile("internal/anything/x.go", []string{rep + "/sub/deep"}, std)
		testassert.Check(t, len(problems) == 1, "generic file importing %q subpackage: %d problems, want 1: %v", rep, len(problems), problems)
	}
	{
		problems := checkTrackedGoFile("main.go", []string{rep, "fmt"}, std)
		testassert.Check(t, len(problems) == 1, "main.go importing %q beside an allowed import: %d problems, want 1: %v", rep, len(problems), problems)
	}
	{
		problems := checkTrackedGoFile("main.go", []string{rep, publicModule + "/model"}, std)
		testassert.Check(t, len(problems) == 2, "main.go importing %q beside the forbidden model package: %d problems, want 2: %v", rep, len(problems), problems)
	}
	// Segment boundaries: none of the real retained neighbour source paths is
	// caught by the segment-bounded matching.
	for _, rel := range []string{"internal/agents/x.go", "internal/acpclient/x.go", "internal/cliclient/x.go"} {
		{
			problems := checkTrackedGoFile(rel, []string{"fmt"}, std)
			testassert.Check(t, len(problems) == 0, "%s wrongly treated as a retired tree: %v", rel, problems)
		}
	}
}

// TestDependencyRulesClientHostStartupIsolation enforces the shared startup
// helper's exact allowlist. The reverse edge is covered by the directory matrix.
func TestDependencyRulesClientHostStartupIsolation(t *testing.T) {
	std := standardLibraryImports(t)
	{
		problems := checkTrackedGoFile("internal/clienthost/x.go", []string{
			"context", "errors", "fmt", "os", "os/exec", "path/filepath", "time",
			publicModule + "/client", publicModule + "/protocol", publicModule + "/internal/hostprocess",
			publicModule + "/internal/clientops",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/clienthost imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/agent", publicModule + "/harness",
		publicModule + "/runtime", publicModule + "/internal/agent", publicModule + "/internal/config",
		publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/plugins",
		publicModule + "/internal/terminal", publicModule + "/internal/cli",
		publicModule + "/internal/desktopclient", publicModule + "/internal/clientview",
		publicModule + "/internal/acpclient", publicModule + "/internal/version",
	} {
		{
			problems := checkTrackedGoFile("internal/clienthost/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/clienthost importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
}

// TestDependencyRulesClientViewIsolation enforces the scalar projection
// helper's exact allowlist over the standard library and the generated
// protocol contracts. The reverse edge is covered by the directory matrix.
func TestDependencyRulesClientViewIsolation(t *testing.T) {
	std := standardLibraryImports(t)
	{
		problems := checkTrackedGoFile("internal/clientview/x.go", []string{
			"encoding/json", "errors", "fmt", "strconv",
			publicModule + "/protocol",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/clientview imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/agent", publicModule + "/harness",
		publicModule + "/runtime", publicModule + "/internal/agent", publicModule + "/internal/config",
		publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/plugins",
		publicModule + "/internal/terminal", publicModule + "/internal/cli",
		publicModule + "/client", publicModule + "/internal/clienthost",
		publicModule + "/internal/desktopclient", publicModule + "/internal/clientops",
	} {
		{
			problems := checkTrackedGoFile("internal/clientview/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/clientview importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
}

// TestDependencyRulesProtocolGenIsolation enforces the private protocol build
// command's exact allowlist: the standard library and the pinned schema loader
// only. The foundation, the Runtime, the target clients and the generated
// contracts are its nearest forbidden siblings, and another subpackage of the
// same loader module is not an allowed edge either.
func TestDependencyRulesProtocolGenIsolation(t *testing.T) {
	std := standardLibraryImports(t)
	{
		problems := checkTrackedGoFile("internal/protocolgen/x.go", []string{
			"context", "encoding/json", "flag", "fmt", "go/format", "os", "sort", "strconv", "strings", "text/template",
			kinOpenAPIPkg,
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/protocolgen imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/agent", publicModule + "/harness",
		publicModule + "/runtime", publicModule + "/internal/agent", publicModule + "/internal/config",
		publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/plugins",
		publicModule + "/internal/terminal", publicModule + "/internal/cli",
		publicModule + "/internal/clienthost", publicModule + "/internal/clientview",
		publicModule + "/internal/clientops", publicModule + "/internal/desktopclient",
		publicModule + "/internal/acpclient", publicModule + "/internal/cliclient",
		publicModule + "/client", publicModule + "/protocol",
		oapiRuntimePkg, "github.com/getkin/kin-openapi/openapi3filter",
	} {
		{
			problems := checkTrackedGoFile("internal/protocolgen/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/protocolgen importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
}

// TestDependencyRulesClientOpsIsolation enforces the shared typed invocation
// helper's exact allowlist. The reverse edge is covered by the directory
// matrix.
func TestDependencyRulesClientOpsIsolation(t *testing.T) {
	std := standardLibraryImports(t)
	{
		problems := checkTrackedGoFile("internal/clientops/x.go", []string{
			"bytes", "context", "encoding/json", "errors", "fmt", "io", "net/http",
			publicModule + "/client", publicModule + "/protocol",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/clientops imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/agent", publicModule + "/harness",
		publicModule + "/runtime", publicModule + "/internal/agent", publicModule + "/internal/config",
		publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/plugins",
		publicModule + "/internal/terminal", publicModule + "/internal/cli",
		publicModule + "/internal/clienthost", publicModule + "/internal/clientview",
		publicModule + "/internal/desktopclient", publicModule + "/internal/acpclient",
		publicModule + "/internal/version", publicModule + "/internal/hostprocess",
	} {
		{
			problems := checkTrackedGoFile("internal/clientops/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/clientops importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
}

// TestDependencyRulesACPClientIsolation enforces the target ACP bridge's
// exact allowlist and proves the new helpers stay reserved for their exact
// consumers.
func TestDependencyRulesACPClientIsolation(t *testing.T) {
	std := standardLibraryImports(t)
	{
		problems := checkTrackedGoFile("internal/acpclient/x.go", []string{
			"bufio", "bytes", "context", "encoding/json", "errors", "fmt", "io", "os", "os/signal",
			"path/filepath", "slices", "strings", "sync", "syscall", "time",
			publicModule + "/client", publicModule + "/protocol", publicModule + "/internal/clienthost",
			publicModule + "/internal/clientops", publicModule + "/internal/version",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/acpclient imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/agent", publicModule + "/harness",
		publicModule + "/runtime", publicModule + "/internal/agent", publicModule + "/internal/acp",
		publicModule + "/internal/config", publicModule + "/internal/storage", sqliteDriverPkg,
		publicModule + "/internal/plugins", publicModule + "/internal/terminal", publicModule + "/internal/cli",
		publicModule + "/internal/clientview", publicModule + "/internal/desktopclient",
		publicModule + "/internal/hostprocess",
	} {
		{
			problems := checkTrackedGoFile("internal/acpclient/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/acpclient importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
	// Root files and other sibling client hosts cannot reach
	// the typed invocation helper; shared startup and the desktop host may use
	// it, but none of them can reach the ACP bridge.
	for _, rel := range []string{"rootmisc.go", "app.go", "internal/clienthost/x.go", "internal/desktopclient/x.go", "internal/clientview/x.go"} {
		for _, imp := range []string{publicModule + "/internal/clientops", publicModule + "/internal/acpclient"} {
			if imp == publicModule+"/internal/clientops" && (rel == "internal/clienthost/x.go" || rel == "internal/desktopclient/x.go") {
				continue
			}
			{
				problems := checkTrackedGoFile(rel, []string{imp}, std)
				testassert.Check(t, len(problems) == 1, "%s importing %q: %d problems, want 1: %v", rel, imp, len(problems), problems)
			}
		}
	}
}

// TestDependencyRulesDesktopHostIsolation enforces the native desktop host's
// exact allowlist. The reverse edge and the deleted preview entry's exact and
// subdirectory paths are covered by the directory matrix and the generic
// fallback rows.
func TestDependencyRulesDesktopHostIsolation(t *testing.T) {
	std := standardLibraryImports(t)
	{
		problems := checkTrackedGoFile("internal/desktopclient/x.go", []string{
			"bytes", "context", "encoding/json", "errors", "fmt", "io/fs", "path/filepath", "strconv", "strings", "sync",
			publicModule + "/client", publicModule + "/protocol", publicModule + "/internal/clienthost",
			publicModule + "/internal/clientops", publicModule + "/internal/version",
			wailsPkg, wailsRuntimePkg, wailsOptionsPkg, wailsAssetPkg, wailsLinuxPkg,
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed internal/desktopclient imports flagged: %v", problems)
	}
	for _, imp := range []string{
		publicModule + "/model", publicModule + "/agent", publicModule + "/harness",
		publicModule + "/runtime", publicModule + "/internal/agent", publicModule + "/internal/config",
		publicModule + "/internal/storage", sqliteDriverPkg, publicModule + "/internal/plugins",
		publicModule + "/internal/terminal", publicModule + "/internal/cli",
		publicModule + "/internal/clientview",
	} {
		{
			problems := checkTrackedGoFile("internal/desktopclient/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "internal/desktopclient importing %q: %d problems, want 1: %v", imp, len(problems), problems)
		}
	}
}

func TestDependencyRulesCliclientIsolation(t *testing.T) {
	std := standardLibraryImports(t)
	allowed := []string{"context", "encoding/json", "os", "sync", "syscall", "golang.org/x/term"}
	for _, pkg := range []string{"client", "protocol", "internal/clienthost", "internal/clientview", "internal/clientops", "internal/terminal"} {
		allowed = append(allowed, publicModule+"/"+pkg)
	}
	{
		problems := checkTrackedGoFile("internal/cliclient/x.go", allowed, std)
		testassert.Require(t, len(problems) == 0, "%v", problems)
	}
	for _, imp := range []string{publicModule + "/agent", publicModule + "/harness", publicModule + "/runtime", publicModule + "/internal/agent", publicModule + "/internal/cli", publicModule + "/internal/storage", publicModule + "/internal/plugins/tools", publicModule + "/internal/hostprocess", publicModule + "/internal/desktopclient", publicModule + "/internal/acpclient", publicModule + "/internal/version", sqliteDriverPkg, "golang.org/x/sys/unix"} {
		{
			problems := checkTrackedGoFile("internal/cliclient/x.go", []string{imp}, std)
			testassert.Check(t, len(problems) == 1, "forbidden %s: %v", imp, problems)
		}
	}
	for _, rel := range []string{"rootmisc.go", "app.go", "commands.go", "internal/clienthost/x.go", "internal/clientview/x.go", "internal/clientops/x.go", "internal/desktopclient/x.go", "internal/acpclient/x.go", "internal/cliclient/sub/x.go", "cmd/desktopclient/x.go"} {
		{
			problems := checkTrackedGoFile(rel, []string{publicModule + "/internal/cliclient"}, std)
			testassert.Check(t, len(problems) == 1, "reverse %s: %v", rel, problems)
		}
	}
}

// TestDependencyRulesRootEntryRoles enforces the active root entry files'
// exact import roles: the root entry composes the shipped clients, the
// dispatcher reaches the shared startup helper and its independent
// utilities, and the runtime command alone holds the Runtime composition
// exception over the public runtime contract and the shipped plugins. The
// nearest wrong roles, an arbitrary root file, and adapter edges into the
// Runtime, plugins and storage stay rejected exactly once.
func TestDependencyRulesRootEntryRoles(t *testing.T) {
	std := standardLibraryImports(t)
	{
		problems := checkTrackedGoFile("main.go", []string{
			"context", "embed", "errors", "fmt", "log/slog", "os", "path/filepath",
			publicModule + "/internal/clienthost", publicModule + "/internal/desktopclient",
			publicModule + "/internal/cliclient", publicModule + "/internal/acpclient",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed main.go imports flagged: %v", problems)
	}
	{
		problems := checkTrackedGoFile("commands.go", []string{
			"bufio", "context", "encoding/json", "errors", "flag", "fmt", "io", "os", "os/signal",
			"path/filepath", "runtime", "strings", "syscall",
			publicModule + "/internal/clienthost", publicModule + "/internal/config",
			publicModule + "/internal/doctor", publicModule + "/internal/selfupdate",
			publicModule + "/internal/version",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed commands.go imports flagged: %v", problems)
	}
	{
		problems := checkTrackedGoFile("runtime_command.go", []string{
			"context", "errors", "path/filepath",
			publicModule + "/runtime", publicModule + "/internal/plugins/builtin",
		}, std)
		testassert.Check(t, len(problems) == 0, "allowed runtime_command.go imports flagged: %v", problems)
	}
	for _, row := range []struct{ rel, imp string }{
		// The Runtime composition exception belongs only to the runtime
		// command, and no active entry file reaches the direct model,
		// Harness, storage or SQLite-driver edges; removed owners are
		// rejected by the retired-source guard before role handling.
		{"main.go", publicModule + "/runtime"},
		{"main.go", publicModule + "/internal/plugins/builtin"},
		{"main.go", publicModule + "/internal/config"},
		{"main.go", publicModule + "/model"},
		{"main.go", publicModule + "/harness"},
		{"main.go", publicModule + "/internal/storage"},
		{"main.go", sqliteDriverPkg},
		{"main.go", publicModule + "/internal/agent"},
		{"main.go", publicModule + "/internal/cli"},
		{"main.go", publicModule + "/internal/acp"},
		{"commands.go", publicModule + "/internal/desktopclient"},
		{"commands.go", publicModule + "/internal/cliclient"},
		{"commands.go", publicModule + "/internal/acpclient"},
		{"commands.go", publicModule + "/runtime"},
		{"commands.go", publicModule + "/internal/plugins/builtin"},
		{"commands.go", publicModule + "/internal/agent"},
		{"commands.go", publicModule + "/internal/cli"},
		{"commands.go", publicModule + "/internal/acp"},
		{"commands.go", publicModule + "/harness"},
		{"commands.go", sqliteDriverPkg},
		{"runtime_command.go", publicModule + "/internal/clienthost"},
		{"runtime_command.go", publicModule + "/internal/cliclient"},
		{"runtime_command.go", publicModule + "/internal/desktopclient"},
		{"runtime_command.go", publicModule + "/internal/acpclient"},
		{"runtime_command.go", publicModule + "/internal/config"},
		{"runtime_command.go", publicModule + "/internal/doctor"},
		{"runtime_command.go", publicModule + "/model"},
		{"runtime_command.go", publicModule + "/harness"},
		{"runtime_command.go", publicModule + "/internal/storage"},
		{"runtime_command.go", sqliteDriverPkg},
		{"runtime_command.go", publicModule + "/internal/agent"},
		{"runtime_command.go", publicModule + "/internal/cli"},
		{"runtime_command.go", publicModule + "/internal/acp"},
		// A new arbitrary root file keeps the generic fallback: no role and
		// no reserved edge.
		{"rootmisc.go", publicModule + "/runtime"},
		{"rootmisc.go", publicModule + "/internal/clienthost"},
		{"rootmisc.go", publicModule + "/internal/plugins/builtin"},
		{"rootmisc.go", publicModule + "/internal/storage"},
	} {
		{
			problems := checkTrackedGoFile(row.rel, []string{row.imp}, std)
			testassert.Check(t, len(problems) == 1, "%s importing %q: %d problems, want 1: %v", row.rel, row.imp, len(problems), problems)
		}
	}
}

// TestDependencyRulesCheckEveryTrackedDirectory proves the file-set contract has no directory-name skipping: tracked-looking paths under previously-skipped names (.hidden, _underscore, frontend, node_modules, vendor), root files, and arbitrary internal/ paths are ordinary production files whose imports of any foundation package or the SQLite driver are reported, and internal/storage subpackages are rejected like public-package ones.
func TestDependencyRulesCheckEveryTrackedDirectory(t *testing.T) {
	std := standardLibraryImports(t)
	for _, rel := range []string{".hidden/pkg/x.go", "_scaffold/pkg/x.go", "frontend/bindata.go", "node_modules/pkg/x.go", "vendor/pkg/x.go", "rootmisc.go", "internal/anything/x.go", "cmd/desktopclient/x.go", "cmd/desktopclient/sub/x.go"} {
		for _, imp := range []string{publicModule + "/model", publicModule + "/agent", publicModule + "/harness", publicModule + "/internal/storage", publicModule + "/runtime", publicModule + "/internal/plugins/sqlite", publicModule + "/internal/plugins/tools", publicModule + "/internal/plugins/adaptation", publicModule + "/internal/plugins/lsp", publicModule + "/internal/plugins/builtin", publicModule + "/plugins/jobs", publicModule + "/internal/toolargs", publicModule + "/protocol", publicModule + "/internal/clienthost", publicModule + "/internal/clientview", publicModule + "/internal/clientops", publicModule + "/internal/acpclient", publicModule + "/internal/cliclient", publicModule + "/internal/desktopclient", sqliteDriverPkg} {
			{
				problems := checkTrackedGoFile(rel, []string{imp}, std)
				testassert.Check(t, len(problems) == 1, "tracked %q importing %q: %d problems, want 1: %v", rel, imp, len(problems), problems)
			}
		}
	}
	for _, rel := range []string{"model/sub/x.go", "agent/sub/x.go", "harness/sub/x.go", "internal/storage/sub/x.go", "plugins/jobs/sub/x.go"} {
		{
			problems := checkTrackedGoFile(rel, []string{"fmt"}, std)
			testassert.Check(t, len(problems) == 1, "subpackage file %q: %d problems, want 1: %v", rel, len(problems), problems)
		}
	}
	{
		problems := checkTrackedGoFile("internal/model/x.go", []string{"fmt"}, std)
		testassert.Check(t, len(problems) == 0, "unrelated internal/model file wrongly treated as a foundation subpackage: %v", problems)
	}
}

// trackedGoFiles returns the module-root-relative slash paths of every Git-tracked non-test Go file — the authoritative production set, independent of the worktree's untracked or ignored contents.
func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	out := bytes.Buffer{}
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	cmd.Stdout = &out
	{
		err := cmd.Run()
		testassert.Require(t, err == nil, "listing tracked files under %s: %v", root, err)
	}
	var files []string
	for _, rel := range strings.Split(out.String(), "\x00") {
		if rel == "" || !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		files = append(files, rel)
	}
	testassert.Require(t, len(files) != 0, "git tracked no Go files under %s", root)
	return files
}

// standardLibraryImports returns authoritative standard-library membership from the toolchain's own package list. "C" is removed explicitly: the cgo pseudo-import must never ride either allowlist regardless of whether a given toolchain's std listing includes it.
func standardLibraryImports(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("go", "list", "std").Output()
	testassert.NoError(t, err)
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
	testassert.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		testassert.Require(t, parent != dir, "no go.mod found above %s", dir)
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
	{
		err := os.WriteFile(file, []byte("package agent\n\nimport `github.com/MMinasyan/lightcode/model`\nimport `fmt`\n"), 0o644)
		testassert.NoError(t, err)
	}
	imports, err := parseFileImports(file)
	testassert.NoError(t, err)
	want := []string{publicModule + "/model", "fmt"}
	testassert.Require(t, len(imports) == len(want) && imports[0] == want[0] && imports[1] == want[1], "import paths = %q, want canonical %q", imports, want)
	{
		problems := checkTrackedGoFile("agent/x.go", imports, standardLibraryImports(t))
		testassert.Check(t, len(problems) == 0, "raw-string imports rejected by the agent allowlist: %v", problems)
	}
}
