// Package lsp declares Lightcode's Runtime-scoped LSP plugin: the
// diagnostics and workspace_symbol tools over Workspace-keyed shared
// language-server managers. It is an ordinary composition unit selected
// through static registration — the same Plugin/Instance contract an
// externally supplied capability implements, with no additional Runtime
// authority and no legacy authorization wrapper.
package lsp

import (
	"sync/atomic"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/lsp"
	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

const (
	pluginID = "lsp"

	permissionWorkspaceInspect = "workspace.inspect"

	// deniedToolResultContent is the contract-fixed model-visible content of
	// every failed canonical preparation, mirroring the Harness policy denial.
	deniedToolResultContent = "Permission denied."

	// maxDiagnosticBytes is the durable diagnostic bound for the
	// validation-class diagnostics this plugin produces, matching the
	// Harness's own tool-boundary bound.
	maxDiagnosticBytes = 512

	// maxContentBytes is the model-visible content bound of both tools: final
	// content over the bound keeps the longest complete-character prefix that
	// fits with the appended truncation marker. This is plugin-local
	// formatting applied after the shared client formatting — not a Core
	// limiter, another configuration channel or an output-spill mechanism —
	// so read failures and long diagnostic messages cannot bypass it.
	maxContentBytes = 15360

	truncationMarker = "\n[Output truncated]"

	// diagnosticsDescription says the tool checks the files changed by this
	// Session — every call recomputes the candidates, there is no
	// since-the-previous-query cursor.
	diagnosticsDescription = "Check for compilation errors in the files this Session has changed. Call after editing to verify correctness. Returns errors only (not warnings)."
	diagnosticsParameters  = `{"type":"object","properties":{}}`

	workspaceSymbolDescription = "Search for symbols by name across the project. Returns matching functions, types, variables, etc. with their file locations."
	workspaceSymbolParameters  = `{"type":"object","properties":{"query":{"type":"string","description":"Symbol name or partial name to search for."}},"required":["query"]}`
)

// newManager is the package test seam for workspace-manager construction:
// tests wrap it to count, record or redirect managers. It is never nil in
// production and simply delegates to lsp.NewManager.
var newManager = lsp.NewManager

// workspaceEntry is one workspace's shared LSP manager and its one
// detection: done closes when Detect returns, and every later caller reuses
// the manager — including one with an empty instance map — without
// re-detection. Failed detection is not retried.
type workspaceEntry struct {
	manager *lsp.Manager
	done    chan struct{}

	// closeJoins counts how far Close walked its entry joins. Nothing in
	// production reads it; with the detection provably parked, the count is
	// the observation point proving Close reached and blocks on the join.
	closeJoins atomic.Int32
}

// instance is one Open's constructed state: the runtime lifetime context,
// the resolved home directory, and the owner data root. Workspaces register
// lazily — detection starts only in an authorized tool execution; Open and
// every preparation start nothing.
type instance struct {
	ctx     context.Context
	home    string
	dataDir string

	mu     sync.Mutex
	closed bool

	// workspaces is keyed by the canonical root (symlinks resolved) rather
	// than the lexical workspace path, so a directory and its symlink alias
	// share one manager entry. This is deliberate: per-call binding
	// revalidation computes both compared values fresh and never consults
	// the entry, so lexical keying would let a retargeted workspace pass
	// revalidation and be served by the stale, old-tree entry. The
	// canonical key makes the lookup itself the staleness detector — a
	// miss after a retarget creates a correctly rooted new entry, while
	// the old entry's servers retire on the 30-minute idle timer and the
	// entry object persists until plugin close. Keying by the lexical path
	// instead requires deliberately replacing entries on binding change,
	// not a one-line key swap.
	workspaces map[string]*workspaceEntry
}

// workspace returns the canonical root's entry, creating it and launching one
// detection goroutine on first use. The lock covers only the map check, the
// creation and the insertion; detection runs outside it on the plugin's
// runtime lifetime context, so a caller's cancellation never cancels shared
// detection.
func (in *instance) workspace(canonicalRoot string) *workspaceEntry {
	in.mu.Lock()
	if in.closed {
		in.mu.Unlock()
		return nil
	}
	entry := in.workspaces[canonicalRoot]
	if entry == nil {
		manager := newManager(canonicalRoot, in.home)
		entry = &workspaceEntry{manager: manager, done: make(chan struct{})}
		in.workspaces[canonicalRoot] = entry
		lifetime := in.ctx
		go func() {
			defer close(entry.done)
			manager.Detect(lifetime)
		}()
	}
	in.mu.Unlock()
	return entry
}

// waitDetection waits for the workspace's shared detection to complete,
// bounded by the caller's context alone; cancellation returns an error and
// never cancels the shared detection.
func (in *instance) waitDetection(ctx context.Context, entry *workspaceEntry) error {
	select {
	case <-entry.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// close tears every registered workspace manager down: under the lock it
// sets closed and snapshots the entries; outside it, every manager's
// admission closes first (so a server still starting is torn down), then
// in-flight detection joins, and only then the managers shut down. The
// registry lock is never held across any wait.
func (in *instance) close() error {
	in.mu.Lock()
	in.closed = true
	entries := make([]*workspaceEntry, 0, len(in.workspaces))
	for _, entry := range in.workspaces {
		entries = append(entries, entry)
	}
	in.mu.Unlock()

	for _, entry := range entries {
		entry.manager.CloseAdmission()
	}
	for _, entry := range entries {
		entry.closeJoins.Add(1)
		<-entry.done
	}
	for _, entry := range entries {
		entry.manager.ShutdownAll()
	}
	return nil
}

// decodeCallArguments strictly decodes one call's JSON arguments into an
// owned map: UseNumber keeps every numeric lexeme exact, the decode clones
// the call data at the accepting boundary, and malformed, non-object, null
// or trailing data are argument-validation errors.
func decodeCallArguments(raw json.RawMessage) (map[string]any, error) {
	var args map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	if args == nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("arguments must be one JSON object")
	}
	return args, nil
}

// immediateError is the normalization-class immediate outcome: status error
// carrying the bounded validation diagnostic, per the tool-boundary
// validation contract.
func immediateError(callID string, cause error) harness.PreparedTool {
	return harness.PreparedTool{Immediate: &harness.ToolOutcome{
		Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: boundedDiagnostic(cause)},
	}}
}

// immediateDenied is the failed-canonical-preparation immediate outcome: the
// fixed denial, per the tool-boundary contract.
func immediateDenied(callID string) harness.PreparedTool {
	return harness.PreparedTool{Immediate: &harness.ToolOutcome{
		Result: model.ToolResult{CallID: callID, Status: model.ResultDenied, Content: deniedToolResultContent},
	}}
}

// boundedDiagnostic renders one plugin-produced validation diagnostic within
// the durable diagnostic bound.
func boundedDiagnostic(cause error) string {
	msg := cause.Error()
	if len(msg) > maxDiagnosticBytes {
		msg = strings.ToValidUTF8(msg[:maxDiagnosticBytes], "")
	}
	return msg
}

// normalizeCallArguments is the shared Normalize body for both tools: the
// strict decode, the private model-supplied `_lightcode_` field stripping,
// the tool's consumed-field validation, and the marshaled normalized object.
// Unrelated accepted members are retained.
func normalizeCallArguments(call model.ToolCall, validate func(map[string]any) error) (json.RawMessage, error) {
	args, err := decodeCallArguments(call.Arguments)
	if err != nil {
		return nil, err
	}
	clean := make(map[string]any, len(args))
	for k, v := range args {
		if strings.HasPrefix(k, "_lightcode_") {
			continue
		}
		clean[k] = v
	}
	if validate != nil {
		if err := validate(clean); err != nil {
			return nil, err
		}
	}
	return json.Marshal(clean)
}

// canonicalWorkspace resolves the lexical Workspace to its canonical root
// with the same pathutil canonicalization the shared preparation binds, so a
// symlinked lexical Workspace binds to the real root.
func canonicalWorkspace(lexical string) (string, error) {
	resolved, err := pathutil.ResolveFilePathFrom(lexical, lexical)
	if err != nil {
		return "", err
	}
	return resolved.CanonicalPath, nil
}

// capModelContent caps the final model-visible content at maxContentBytes
// UTF-8 bytes: content that fits is unchanged; otherwise the longest
// complete-character prefix that fits with the appended truncation marker is
// kept.
func capModelContent(content string) string {
	if len(content) <= maxContentBytes {
		return content
	}
	n := maxContentBytes - len(truncationMarker)
	for n > 0 && !utf8.RuneStart(content[n]) {
		n--
	}
	return content[:n] + truncationMarker
}

// toolOutcome is the one outcome-mapping rule for both tools: (content, nil)
// settles as success; an observed caller cancellation settles as interrupted
// carrying the capped partial content — the context error itself when
// nothing accumulated, since interrupted requires non-empty content; any
// other error settles as error with the capped error text. The cap applies
// to successful, partial and error content alike.
func toolOutcome(callID string, ctx context.Context, content string, err error) harness.ToolOutcome {
	switch {
	case err == nil:
		return harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultSuccess, Content: capModelContent(content)}}
	case ctx.Err() != nil:
		partial := capModelContent(content)
		if partial == "" {
			partial = capModelContent(ctx.Err().Error())
		}
		return harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultInterrupted, Content: partial}}
	default:
		return harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: capModelContent(err.Error())}}
	}
}

// prepareLSP is the shared Prepare body for both tools: the canonical
// workspace root bound with its one workspace.inspect pair (an unconstrained
// write dir), and the executor that revalidates the binding before any
// service start or query. No Immediate success exists: both tools execute.
func (in *instance) prepareLSP(tc runtime.ToolContext, call model.ToolCall, query func(ctx context.Context, client *lsp.Client, canonicalRoot string) harness.ToolOutcome) harness.PreparedTool {
	canonicalRoot, err := canonicalWorkspace(tc.Workspace)
	if err != nil {
		return immediateDenied(call.ID)
	}
	return harness.PreparedTool{
		Permissions:        []harness.PermissionRequest{{Permission: permissionWorkspaceInspect, Target: canonicalRoot}},
		CanonicalWorkspace: canonicalRoot,
		Execute: func(ctx context.Context) harness.ToolOutcome {
			return in.execute(call.ID, tc, canonicalRoot, ctx, query)
		},
	}
}

// execute is the shared execution path: it first revalidates the workspace
// binding — a changed binding fails with an error outcome, starts nothing,
// and never substitutes a root — then waits for the workspace's one shared
// detection (a canceled caller is interrupted and never cancels the shared
// detection), then runs the tool's query body against the shared client.
func (in *instance) execute(callID string, tc runtime.ToolContext, canonicalRoot string, ctx context.Context, query func(context.Context, *lsp.Client, string) harness.ToolOutcome) harness.ToolOutcome {
	revalidated, err := canonicalWorkspace(tc.Workspace)
	if err != nil || revalidated != canonicalRoot {
		return toolOutcome(callID, ctx, "", errors.New("workspace binding changed since the call was prepared"))
	}
	entry := in.workspace(canonicalRoot)
	if entry == nil {
		return toolOutcome(callID, ctx, "", errors.New("workspace services are closed"))
	}
	if err := in.waitDetection(ctx, entry); err != nil {
		return toolOutcome(callID, ctx, "", err)
	}
	return query(ctx, lsp.NewClient(entry.manager), canonicalRoot)
}

// diagnosticsTool is the diagnostics export: every call lists the calling
// session's unique canonical changed paths afresh — no cursor, no
// group-shaped result, no previous/current distinction and no checked-group
// set — filters them to the bound workspace, and queries the shared client
// through the canonical read path.
type diagnosticsTool struct{ inst *instance }

func (diagnosticsTool) describe(_ runtime.Invocation, _ runtime.ToolConstraints, _ harness.SessionIdentity) (runtime.ToolDescription, error) {
	return runtime.ToolDescription{
		Definition: model.ToolDefinition{
			Name:        "diagnostics",
			Description: diagnosticsDescription,
			Parameters:  json.RawMessage(diagnosticsParameters),
		},
		Available: true,
	}, nil
}

func (diagnosticsTool) Normalize(_ runtime.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return normalizeCallArguments(call, nil)
}

func (t diagnosticsTool) Prepare(_ context.Context, tc runtime.ToolContext, call model.ToolCall) harness.PreparedTool {
	return t.inst.prepareLSP(tc, call, func(ctx context.Context, client *lsp.Client, canonicalRoot string) harness.ToolOutcome {
		paths, err := snapshot.ListChangedFiles(t.inst.dataDir, tc.AdmittedEntry.SessionID)
		if err != nil {
			// The retained listing-error content, settled as success with
			// the cap applied; a listing error discards all partial data.
			return toolOutcome(call.ID, ctx, "error: could not list changed files", nil)
		}
		paths = filterInsideRoot(paths, canonicalRoot)
		if len(paths) == 0 {
			return toolOutcome(call.ID, ctx, "No files have been modified.", nil)
		}
		content, err := client.GetDiagnosticsCanonical(ctx, paths)
		return toolOutcome(call.ID, ctx, content, err)
	})
}

// filterInsideRoot keeps the paths at or under the canonical root, derived
// with filepath.Rel; outside-root paths are dropped, never read.
func filterInsideRoot(paths []string, canonicalRoot string) []string {
	filtered := make([]string, 0, len(paths))
	for _, p := range paths {
		if rel, err := filepath.Rel(canonicalRoot, p); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// workspaceSymbolTool is the workspace_symbol export: the legacy description,
// schema and outcomes over the shared detection.
type workspaceSymbolTool struct{ inst *instance }

func (workspaceSymbolTool) describe(_ runtime.Invocation, _ runtime.ToolConstraints, _ harness.SessionIdentity) (runtime.ToolDescription, error) {
	return runtime.ToolDescription{
		Definition: model.ToolDefinition{
			Name:        "workspace_symbol",
			Description: workspaceSymbolDescription,
			Parameters:  json.RawMessage(workspaceSymbolParameters),
		},
		Available: true,
	}, nil
}

// validateQueryArgs consumes the tool's one argument: absent, null and
// wrong-typed query values are argument-validation errors; a present empty
// string passes normalization and is rejected at execution with the
// retained text.
func validateQueryArgs(args map[string]any) error {
	v, present := args["query"]
	if !present || v == nil {
		return errors.New("workspace_symbol: query is required")
	}
	if _, ok := v.(string); !ok {
		return errors.New("workspace_symbol: query must be a string")
	}
	return nil
}

func (workspaceSymbolTool) Normalize(_ runtime.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return normalizeCallArguments(call, validateQueryArgs)
}

func (t workspaceSymbolTool) Prepare(_ context.Context, tc runtime.ToolContext, call model.ToolCall) harness.PreparedTool {
	args, err := decodeCallArguments(call.Arguments)
	if err != nil {
		return immediateError(call.ID, err)
	}
	query, _ := args["query"].(string)
	return t.inst.prepareLSP(tc, call, func(ctx context.Context, client *lsp.Client, _ string) harness.ToolOutcome {
		if query == "" {
			return toolOutcome(call.ID, ctx, "error: query is required", nil)
		}
		content, err := client.WorkspaceSymbol(ctx, query)
		return toolOutcome(call.ID, ctx, content, err)
	})
}

// Plugin returns the Runtime-scoped plugin whose two exports are the LSP
// diagnostics and workspace_symbol tools. It has no settings — the
// nil-validator no-settings rule applies — and no dependencies. Open resolves
// the home directory once, returning any error through ordinary failed
// construction; the retained language-server cache stays under
// home/.cache/lightcode/lsp independently of DataDir.
func Plugin() runtime.Plugin {
	return runtime.Plugin{
		ID:    pluginID,
		Scope: runtime.ScopeRuntime,
		Provides: []runtime.CapabilitySpec{
			runtime.ToolSpec("diagnostics", diagnosticsTool{}.describe),
			runtime.ToolSpec("workspace_symbol", workspaceSymbolTool{}.describe),
		},
		Open: open,
	}
}

// open checks the scope context, resolves home once, and captures the runtime
// lifetime context and the owner data root. No manager, workspace or server
// exists at construction.
func open(ctx context.Context, info runtime.ScopeInfo, _ runtime.Bindings) (runtime.Instance, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Instance{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return runtime.Instance{}, fmt.Errorf("lsp: resolve home directory: %w", err)
	}
	inst := &instance{
		ctx:        ctx,
		home:       home,
		dataDir:    info.DataDir,
		workspaces: make(map[string]*workspaceEntry),
	}
	return runtime.Instance{
		Values: map[string]any{
			"diagnostics":      diagnosticsTool{inst: inst},
			"workspace_symbol": workspaceSymbolTool{inst: inst},
		},
		Close: inst.close,
	}, nil
}
