// Package tools declares Lightcode's native file-tools plugin: read_file,
// write_file, edit_file and apply_patch as ordinary Runtime-scoped Tool
// exports. It is an ordinary composition unit selected through static
// registration — the same Plugin/Instance contract an externally supplied
// capability implements, with no additional Runtime authority and no legacy
// authorization wrapper.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/editpreview"
	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/shellparse"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/tool"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

const (
	pluginID = "tools"

	permissionFileRead   = "file.read"
	permissionFileWrite  = "file.write"
	permissionCommandRun = "command.run"
	permissionSleep      = "sleep"

	// deniedToolResultContent is the contract-fixed model-visible content of
	// every failed canonical preparation, mirroring the Harness policy denial.
	deniedToolResultContent = "Permission denied."

	// maxDiagnosticBytes is the durable diagnostic bound for the
	// validation-class diagnostics this plugin produces, matching the
	// Harness's own tool-boundary bound.
	maxDiagnosticBytes = 512
)

// settings is the plugin's private configuration, decoded with json.Unmarshal
// from the plugins.tools section. Nil input and missing or null-valued
// members keep the defaults; every consumed limit must be positive, and
// command_timeout must fit its seconds-to-time.Duration conversion. Every
// other member is ignored, including max_background_processes regardless of
// its JSON value.
type settings struct {
	MaxOutputBytes   int `json:"max_output_bytes"`
	ReadMaxLines     int `json:"read_max_lines"`
	ReadLineMaxChars int `json:"read_line_max_chars"`
	CommandTimeout   int `json:"command_timeout"`
}

func defaultSettings() settings {
	return settings{MaxOutputBytes: 15360, ReadMaxLines: 500, ReadLineMaxChars: 5000, CommandTimeout: 120}
}

// decodeSettings decodes one supplied plugins.tools section on top of the
// defaults and validates every consumed limit.
func decodeSettings(raw json.RawMessage) (settings, error) {
	s := defaultSettings()
	if len(bytes.TrimSpace(raw)) != 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return settings{}, fmt.Errorf("tools: invalid settings: %w", err)
		}
	}
	switch {
	case s.MaxOutputBytes <= 0:
		return settings{}, errors.New("tools: max_output_bytes must be positive")
	case s.ReadMaxLines <= 0:
		return settings{}, errors.New("tools: read_max_lines must be positive")
	case s.ReadLineMaxChars <= 0:
		return settings{}, errors.New("tools: read_line_max_chars must be positive")
	case s.CommandTimeout <= 0:
		return settings{}, errors.New("tools: command_timeout must be positive")
	case int64(s.CommandTimeout) > math.MaxInt64/int64(time.Second):
		return settings{}, errors.New("tools: command_timeout overflows the seconds-to-duration conversion")
	}
	return s, nil
}

// toolsConfig projects the decoded settings onto the shared file-tool
// configuration; the command members are irrelevant to the file tools.
func (s settings) toolsConfig() config.ToolsConfig {
	return config.ToolsConfig{
		MaxOutputBytes:   s.MaxOutputBytes,
		ReadMaxLines:     s.ReadMaxLines,
		ReadLineMaxChars: s.ReadLineMaxChars,
		CommandTimeout:   s.CommandTimeout,
	}
}

// instance is one Open's constructed state: the owner data root captured from
// the scope identity. It keeps no Session-indexed state, no handle tags, no
// cached read records and no reset or eviction hooks: every mutating call
// opens its code group fresh from the calling Operation's admitted-input
// identity and drops it afterward.
type instance struct {
	dataDir string
}

func (in *instance) settings(inv runtime.Invocation) (settings, error) {
	return decodeSettings(inv.Config(pluginID))
}

// openCodeGroup opens the calling Operation's one disk code group, keyed by
// the admitted-input identity from the ToolContext — a validated Harness
// identity, never model-supplied data. One Operation has one group; steering
// shares it because the Operation's admitted input never changes. The handle
// is opened per call, shared by that call's sub-operations, and not retained.
func (in *instance) openCodeGroup(tc runtime.ToolContext) (codeGroupStore, error) {
	directory := filepath.Join(in.dataDir, "code", tc.AdmittedEntry.SessionID, tc.AdmittedEntry.EntryID)
	store, err := snapshot.OpenCodeStore(directory)
	if err != nil {
		return codeGroupStore{}, err
	}
	return codeGroupStore{CodeStore: store}, nil
}

// codeGroupStore adapts one per-call snapshot.CodeStore to the shared
// internal/tool SnapshotStore interface. The shared bodies type-assert and
// prefer the transactional path — the promoted SnapshotResolvedEntry plus the
// discard/retain/lock/identity methods — so Snapshot and CurrentTurn exist
// only to satisfy the interface; the transactional path preempts them. They
// are interface compliance, not dead code.
type codeGroupStore struct {
	*snapshot.CodeStore
}

// CurrentTurn pins the group's single turn: an Operation-backed code group
// holds one preimage set, not a session turn history.
func (codeGroupStore) CurrentTurn() int { return 1 }

// Snapshot satisfies the plain SnapshotStore surface through the resolved
// entry capture; it never runs because the transactional path preempts it.
func (s codeGroupStore) Snapshot(turn int, absPath string) error {
	_, _, err := s.SnapshotResolvedEntry(turn, absPath, absPath)
	return err
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

// canonicalPaths computes the canonical Workspace root and write-dir boundary
// with the same pathutil canonicalization the shared preparation binds
// internally, so Harness policy containment and the closures' revalidation
// agree. An empty write dir stays unconstrained.
func canonicalPaths(root, writeDir string) (workspace, boundary string, err error) {
	resolved, err := pathutil.ResolveFilePathFrom("", root)
	if err != nil {
		return "", "", err
	}
	workspace = resolved.CanonicalPath
	if writeDir == "" {
		return workspace, "", nil
	}
	writeResolved, err := pathutil.ResolveFilePathFrom(root, writeDir)
	if err != nil {
		return "", "", err
	}
	return workspace, writeResolved.CanonicalPath, nil
}

func permissionPairs(permission string, targets []tool.PreparedTarget) []harness.PermissionRequest {
	pairs := make([]harness.PermissionRequest, 0, len(targets))
	for _, target := range targets {
		pairs = append(pairs, harness.PermissionRequest{Permission: permission, Target: target.CanonicalPath})
	}
	return pairs
}

// stringOutcome runs one prepared string-result call and wraps the outcome:
// the model-visible content is the shared body's own bounded output, and an
// optional metadata converter turns the committed normalized arguments plus
// the result into the plugin-owned metadata shape. A nil metadata map or a
// failed well-formedness marshal drops the optional metadata; the result
// itself always commits.
func stringOutcome(ctx context.Context, callID string, call *tool.PreparedCall, args string, metadata func(args, result string) map[string]any) harness.ToolOutcome {
	result, err := call.Execute(ctx)
	if err != nil {
		return harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: err.Error()}}
	}
	outcome := harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultSuccess, Content: result}}
	if metadata != nil {
		if m := metadata(args, result); m != nil {
			if raw, err := json.Marshal(m); err == nil {
				outcome.Metadata = raw
			}
		}
	}
	return outcome
}

// patchOutcome runs one prepared patch call and wraps the outcome: the
// model-visible content is the engine's own summary, and the captured
// per-file previews convert into the edit_preview_files metadata shape with
// their full preview content retained.
func patchOutcome(ctx context.Context, callID string, call *tool.PreparedPatchCall) harness.ToolOutcome {
	result, err := call.Execute(ctx)
	if err != nil {
		return harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: err.Error()}}
	}
	outcome := harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultSuccess, Content: result.Result}}
	if m := tool.ApplyPatchPreviewMetadata(result.Previews); m != nil {
		if raw, err := json.Marshal(m); err == nil {
			outcome.Metadata = raw
		}
	}
	return outcome
}

// normalizeCallArguments is the one shared Normalize body for every tool:
// the strict decode of the call arguments, the tool's single argument
// normalization, and the marshaled normalized object.
func normalizeCallArguments(call model.ToolCall, normalize func(map[string]any) (map[string]any, error)) (json.RawMessage, error) {
	args, err := decodeCallArguments(call.Arguments)
	if err != nil {
		return nil, err
	}
	normalized, err := normalize(args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// readTool is the read_file export: always available, no metadata, the
// nil-tracker read path that always returns the bounded requested content.
type readTool struct{ inst *instance }

func (readTool) describe(in runtime.Invocation, _ runtime.ToolConstraints) (runtime.ToolDescription, error) {
	if _, err := decodeSettings(in.Config(pluginID)); err != nil {
		return runtime.ToolDescription{}, err
	}
	return runtime.ToolDescription{
		Definition: model.ToolDefinition{
			Name:        "read_file",
			Description: readFileDescription,
			Parameters:  json.RawMessage(readFileParameters),
		},
		Available: true,
	}, nil
}

func (t readTool) Normalize(tc runtime.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	s, err := t.inst.settings(tc.Invocation)
	if err != nil {
		return nil, err
	}
	return normalizeCallArguments(call, func(args map[string]any) (map[string]any, error) {
		return tool.NormalizeReadArgs(args, s.ReadMaxLines)
	})
}

func (t readTool) Prepare(_ context.Context, tc runtime.ToolContext, call model.ToolCall) harness.PreparedTool {
	s, err := t.inst.settings(tc.Invocation)
	if err != nil {
		return immediateError(call.ID, err)
	}
	// Preparation accepts only the committed normalized arguments: one
	// strict decode-and-pass, never a second normalization.
	args, err := decodeCallArguments(call.Arguments)
	if err != nil {
		return immediateError(call.ID, err)
	}
	workspace, writeDirCanonical, err := canonicalPaths(tc.Workspace, tc.Constraints.WriteDir)
	if err != nil {
		return immediateDenied(call.ID)
	}
	prepared, err := tool.PrepareReadCall(tc.Workspace, s.toolsConfig(), args)
	if err != nil {
		return immediateDenied(call.ID)
	}
	return harness.PreparedTool{
		Permissions:        permissionPairs(permissionFileRead, prepared.Targets),
		CanonicalWorkspace: workspace,
		CanonicalWriteDir:  writeDirCanonical,
		Execute: func(ctx context.Context) harness.ToolOutcome {
			return stringOutcome(ctx, call.ID, prepared, string(call.Arguments), nil)
		},
	}
}

// mutationPrepared is one mutation tool's prepared call pieces: the
// declared canonical targets and the outcome-wrapping executor.
type mutationPrepared struct {
	targets []tool.PreparedTarget
	execute func(context.Context) harness.ToolOutcome
}

// prepareMutation is the one shared mutation-Prepare body: the plugin
// settings validation, the strict decode-and-pass of the committed
// normalized arguments, the canonical root/write-dir binding, the per-call
// code group, and the assembled executor plan with its file.write pairs.
// The per-tool callback runs the shared preparation and wraps the outcome;
// its error is a failed canonical preparation.
func (in *instance) prepareMutation(tc runtime.ToolContext, call model.ToolCall, run func(root string, opts tool.CapabilityOptions, group codeGroupStore, args map[string]any) (mutationPrepared, error)) harness.PreparedTool {
	if _, err := in.settings(tc.Invocation); err != nil {
		return immediateError(call.ID, err)
	}
	args, err := decodeCallArguments(call.Arguments)
	if err != nil {
		return immediateError(call.ID, err)
	}
	workspace, writeDirCanonical, err := canonicalPaths(tc.Workspace, tc.Constraints.WriteDir)
	if err != nil {
		return immediateDenied(call.ID)
	}
	group, err := in.openCodeGroup(tc)
	if err != nil {
		return immediateDenied(call.ID)
	}
	prepared, err := run(tc.Workspace, tool.CapabilityOptions{WriteDir: tc.Constraints.WriteDir}, group, args)
	if err != nil {
		return immediateDenied(call.ID)
	}
	return harness.PreparedTool{
		Permissions:        permissionPairs(permissionFileWrite, prepared.targets),
		CanonicalWorkspace: workspace,
		CanonicalWriteDir:  writeDirCanonical,
		Execute:            prepared.execute,
	}
}

// writeTool is the write_file export.
type writeTool struct{ inst *instance }

func (t writeTool) describe(in runtime.Invocation, constraints runtime.ToolConstraints) (runtime.ToolDescription, error) {
	return mutationDescription("write_file", writeFileDescription, writeFileParameters, false, in, constraints)
}

func (t writeTool) Normalize(tc runtime.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	if _, err := t.inst.settings(tc.Invocation); err != nil {
		return nil, err
	}
	return normalizeCallArguments(call, tool.NormalizeWriteArgs)
}

func (t writeTool) Prepare(_ context.Context, tc runtime.ToolContext, call model.ToolCall) harness.PreparedTool {
	return t.inst.prepareMutation(tc, call, func(root string, opts tool.CapabilityOptions, group codeGroupStore, args map[string]any) (mutationPrepared, error) {
		prepared, err := tool.PrepareWriteCall(root, opts, group, args)
		if err != nil {
			return mutationPrepared{}, err
		}
		return mutationPrepared{
			targets: prepared.Targets,
			execute: func(ctx context.Context) harness.ToolOutcome {
				return stringOutcome(ctx, call.ID, prepared, string(call.Arguments), nil)
			},
		}, nil
	})
}

// editTool is the edit_file export; its metadata carries the edit_preview
// diff built from the committed normalized arguments and the result.
type editTool struct{ inst *instance }

func (t editTool) describe(in runtime.Invocation, constraints runtime.ToolConstraints) (runtime.ToolDescription, error) {
	return mutationDescription("edit_file", editFileDescription, editFileParameters, false, in, constraints)
}

func (t editTool) Normalize(tc runtime.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	if _, err := t.inst.settings(tc.Invocation); err != nil {
		return nil, err
	}
	return normalizeCallArguments(call, tool.NormalizeEditArgs)
}

func (t editTool) Prepare(_ context.Context, tc runtime.ToolContext, call model.ToolCall) harness.PreparedTool {
	return t.inst.prepareMutation(tc, call, func(root string, opts tool.CapabilityOptions, group codeGroupStore, args map[string]any) (mutationPrepared, error) {
		prepared, err := tool.PrepareEditCall(root, opts, group, args)
		if err != nil {
			return mutationPrepared{}, err
		}
		return mutationPrepared{
			targets: prepared.Targets,
			execute: func(ctx context.Context) harness.ToolOutcome {
				return stringOutcome(ctx, call.ID, prepared, string(call.Arguments), editpreview.MetadataFromArgs)
			},
		}, nil
	})
}

// patchTool is the apply_patch export; it is DefaultHidden like its legacy
// counterpart, so only model adaptations that include it can see it.
type patchTool struct{ inst *instance }

func (t patchTool) describe(in runtime.Invocation, constraints runtime.ToolConstraints) (runtime.ToolDescription, error) {
	return mutationDescription("apply_patch", applyPatchDescription, applyPatchParameters, true, in, constraints)
}

func (t patchTool) Normalize(tc runtime.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	if _, err := t.inst.settings(tc.Invocation); err != nil {
		return nil, err
	}
	return normalizeCallArguments(call, tool.NormalizePatchArgs)
}

func (t patchTool) Prepare(_ context.Context, tc runtime.ToolContext, call model.ToolCall) harness.PreparedTool {
	return t.inst.prepareMutation(tc, call, func(root string, opts tool.CapabilityOptions, group codeGroupStore, args map[string]any) (mutationPrepared, error) {
		prepared, err := tool.PreparePatchCall(root, opts, group, args)
		if err != nil {
			return mutationPrepared{}, err
		}
		return mutationPrepared{
			targets: prepared.Targets,
			execute: func(ctx context.Context) harness.ToolOutcome {
				return patchOutcome(ctx, call.ID, prepared)
			},
		}, nil
	})
}

// runCommandTool is the run_command export: always available; the calling
// Agent's readonly constraint selects the implementation. The readonly
// variant authorizes and executes the read-only allowlist's rewritten
// command with the captured default timeout — a model timeout override is
// ignored, and write_dir never grants unrestricted command execution. The
// unconstrained variant decomposes conclusive simple commands into one
// command.run target per complete segment, falls back to one complete
// target for everything else, and always executes through the shell. There
// is no background path: normalization rejects any supplied background
// member, and the description never advertises one.
type runCommandTool struct{ inst *instance }

func (runCommandTool) describe(in runtime.Invocation, _ runtime.ToolConstraints) (runtime.ToolDescription, error) {
	if _, err := decodeSettings(in.Config(pluginID)); err != nil {
		return runtime.ToolDescription{}, err
	}
	return runtime.ToolDescription{
		Definition: model.ToolDefinition{
			Name:        "run_command",
			Description: runCommandDescription,
			Parameters:  json.RawMessage(runCommandParameters),
		},
		Available: true,
	}, nil
}

func (t runCommandTool) Normalize(tc runtime.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	s, err := t.inst.settings(tc.Invocation)
	if err != nil {
		return nil, err
	}
	return normalizeCallArguments(call, func(args map[string]any) (map[string]any, error) {
		return tool.NormalizeRunCommandArgs(args, s.CommandTimeout)
	})
}

func (t runCommandTool) Prepare(_ context.Context, tc runtime.ToolContext, call model.ToolCall) harness.PreparedTool {
	s, err := t.inst.settings(tc.Invocation)
	if err != nil {
		return immediateError(call.ID, err)
	}
	// Preparation accepts only the committed normalized arguments: one
	// strict decode-and-pass, never a second normalization.
	args, err := decodeCallArguments(call.Arguments)
	if err != nil {
		return immediateError(call.ID, err)
	}
	command, _ := args["command"].(string)
	timeoutSec := s.CommandTimeout
	var targets []string
	if tc.Constraints.Readonly {
		// The readonly path retains the trim-and-reject first: empty and
		// all-blank input settles the fixed rejection like any other
		// non-allowlisted command, never echoing the raw form.
		rewritten, err := tool.ReadOnlyCommand(command)
		if err != nil {
			return immediateError(call.ID, errors.New(tool.ReadOnlyRunCommandRejected))
		}
		targets = []string{rewritten}
		command = rewritten
	} else {
		if command == "" {
			return immediateError(call.ID, errors.New("run_command: command is required"))
		}
		if v, ok := args["timeout"].(json.Number); ok {
			if n, err := v.Int64(); err == nil {
				timeoutSec = int(n)
			}
		}
		if segments, ok := shellparse.ParseSimple(command); ok {
			for _, segment := range segments {
				targets = append(targets, segment.Text)
			}
		}
		if len(targets) == 0 {
			// One complete-command fallback: the input with only leading
			// ASCII blanks/newlines removed, or the complete original input
			// when nothing remains. The exact text is both target and the
			// script the shell runs.
			fallback := strings.TrimLeft(command, " \t\n")
			if fallback == "" {
				fallback = command
			}
			targets = []string{fallback}
			command = fallback
		}
	}
	pairs := make([]harness.PermissionRequest, 0, len(targets))
	for _, target := range targets {
		pairs = append(pairs, harness.PermissionRequest{Permission: permissionCommandRun, Target: target})
	}
	spillDir := filepath.Join(t.inst.dataDir, "code", tc.AdmittedEntry.SessionID, "output")
	return harness.PreparedTool{
		Permissions: pairs,
		Execute: func(ctx context.Context) harness.ToolOutcome {
			return commandOutcome(ctx, call.ID, command, tc.Workspace, timeoutSec, s, spillDir)
		},
	}
}

// commandOutcome runs one prepared foreground command through the shared
// runner and maps the retained outcomes onto the model-visible result: a
// completed run settles as success or — for a nonzero exit — the same
// ExitError output the legacy engine reports as an error; a configured
// timeout settles as an error result with the retained timeout text; a
// parent-context cancellation or deadline settles as an interrupted result
// with the retained cancellation text.
func commandOutcome(ctx context.Context, callID, command, dir string, timeoutSec int, s settings, spillDir string) harness.ToolOutcome {
	result, err := tool.RunForegroundCommand(ctx, command, dir, timeoutSec, s.MaxOutputBytes, s.ReadLineMaxChars, spillDir)
	var exitErr *tool.ExitError
	if errors.As(err, &exitErr) {
		status := model.ResultError
		if exitErr.ExitCode == -1 && ctx.Err() != nil {
			status = model.ResultInterrupted
		}
		return harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: status, Content: exitErr.Output}}
	}
	if err != nil {
		return harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: err.Error()}}
	}
	return harness.ToolOutcome{Result: model.ToolResult{CallID: callID, Status: model.ResultSuccess, Content: result}}
}

// sleepTool is the sleep export: normalization clamps to integer seconds
// 1..300, the fixed target * is the one the built-in policy allows, and
// execution observes cancellation as an interrupted result.
type sleepTool struct{ inst *instance }

func (sleepTool) describe(in runtime.Invocation, _ runtime.ToolConstraints) (runtime.ToolDescription, error) {
	if _, err := decodeSettings(in.Config(pluginID)); err != nil {
		return runtime.ToolDescription{}, err
	}
	return runtime.ToolDescription{
		Definition: model.ToolDefinition{
			Name:        "sleep",
			Description: sleepDescription,
			Parameters:  json.RawMessage(sleepParameters),
		},
		Available: true,
	}, nil
}

func (t sleepTool) Normalize(tc runtime.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	if _, err := t.inst.settings(tc.Invocation); err != nil {
		return nil, err
	}
	return normalizeCallArguments(call, tool.NormalizeSleepArgs)
}

func (t sleepTool) Prepare(_ context.Context, tc runtime.ToolContext, call model.ToolCall) harness.PreparedTool {
	if _, err := t.inst.settings(tc.Invocation); err != nil {
		return immediateError(call.ID, err)
	}
	args, err := decodeCallArguments(call.Arguments)
	if err != nil {
		return immediateError(call.ID, err)
	}
	seconds := 1.0
	if v, ok := args["seconds"].(json.Number); ok {
		if n, err := v.Int64(); err == nil {
			seconds = float64(n)
		}
	}
	return harness.PreparedTool{
		Permissions: []harness.PermissionRequest{{Permission: permissionSleep, Target: "*"}},
		Execute: func(ctx context.Context) harness.ToolOutcome {
			result, err := tool.Sleep{}.Execute(ctx, map[string]any{"seconds": seconds})
			if err != nil {
				status := model.ResultError
				if ctx.Err() != nil {
					status = model.ResultInterrupted
				}
				return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: status, Content: err.Error()}}
			}
			return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: result}}
		},
	}
}

// mutationDescription is the shared mutation-tool description: the write
// tools are unavailable only to a readonly Agent without a configured write
// dir — with a nonempty write dir the Harness confines every write to it, so
// the tool stays available. The description consumes the captured Invocation
// and the hard constraints only; no execution instance is constructed.
func mutationDescription(name, description, parameters string, defaultHidden bool, in runtime.Invocation, constraints runtime.ToolConstraints) (runtime.ToolDescription, error) {
	if _, err := decodeSettings(in.Config(pluginID)); err != nil {
		return runtime.ToolDescription{}, err
	}
	return runtime.ToolDescription{
		Definition: model.ToolDefinition{
			Name:        name,
			Description: description,
			Parameters:  json.RawMessage(parameters),
		},
		Available:     !constraints.Readonly || constraints.WriteDir != "",
		DefaultHidden: defaultHidden,
	}, nil
}

// Plugin returns the Runtime-scoped plugin whose six exports are the native
// file tools plus the foreground command and sleep tools. It has no
// dependencies. ValidateConfig validates the owned plugins.tools section;
// Open captures the owner data root the mutating calls derive their code
// groups from and the command spills their output directory from.
func Plugin() runtime.Plugin {
	return runtime.Plugin{
		ID:    pluginID,
		Scope: runtime.ScopeRuntime,
		Provides: []runtime.CapabilitySpec{
			runtime.ToolSpec("read_file", readTool{}.describe),
			runtime.ToolSpec("write_file", writeTool{}.describe),
			runtime.ToolSpec("edit_file", editTool{}.describe),
			runtime.ToolSpec("apply_patch", patchTool{}.describe),
			runtime.ToolSpec("run_command", runCommandTool{}.describe),
			runtime.ToolSpec("sleep", sleepTool{}.describe),
		},
		ValidateConfig: func(raw json.RawMessage) error {
			_, err := decodeSettings(raw)
			return err
		},
		Open: open,
	}
}

// open checks the scope context and captures the scope identity's owner data
// root. Every tool value shares that one instance; no per-Session state is
// created here.
func open(ctx context.Context, info runtime.ScopeInfo, _ runtime.Bindings) (runtime.Instance, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Instance{}, err
	}
	inst := &instance{dataDir: info.DataDir}
	return runtime.Instance{Values: map[string]any{
		"read_file":   readTool{inst},
		"write_file":  writeTool{inst},
		"edit_file":   editTool{inst},
		"apply_patch": patchTool{inst},
		"run_command": runCommandTool{inst},
		"sleep":       sleepTool{inst},
	}}, nil
}
