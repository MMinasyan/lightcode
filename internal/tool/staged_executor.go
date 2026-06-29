package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/safefs"
	"golang.org/x/sys/unix"
)

// AskActionFunc blocks until the user answers a staged permission prompt.
type AskActionFunc func(ctx context.Context, req permission.Request) permission.ResponseAction

// StagedExecutor executes pending edit/write calls as a batch while
// preserving normal permission and snapshot rules.
type StagedExecutor struct {
	store         SnapshotStore
	tracker       *FileTracker
	cfg           config.ToolsConfig
	check         CheckFunc
	ask           AskActionFunc
	workspaceRoot string
	options       CapabilityOptions
}

// NewStagedExecutor creates a staged batch executor.
func NewStagedExecutor(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig, check CheckFunc, ask AskActionFunc) *StagedExecutor {
	return NewStagedExecutorAtRoot(store, tracker, cfg, "", check, ask)
}

func NewStagedExecutorAtRoot(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string, check CheckFunc, ask AskActionFunc) *StagedExecutor {
	return NewStagedExecutorAtRootWithOptions(store, tracker, cfg, workspaceRoot, check, ask, CapabilityOptions{})
}

func NewStagedExecutorAtRootWithOptions(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string, check CheckFunc, ask AskActionFunc, opts CapabilityOptions) *StagedExecutor {
	return &StagedExecutor{
		store:         store,
		tracker:       tracker,
		cfg:           cfg,
		check:         check,
		ask:           ask,
		workspaceRoot: workspaceRoot,
		options:       opts,
	}
}

func (e *StagedExecutor) SetToolsConfig(cfg config.ToolsConfig) { e.cfg = cfg }

// ExecutePending applies staged calls in emission order, using a running
// buffer per file and a single final disk write per changed file.
func (e *StagedExecutor) ExecutePending(ctx context.Context, staged []StagedCall) []BatchResult {
	results := make([]BatchResult, len(staged))
	allowed := make([]bool, len(staged))
	resolvedStaged := make([]StagedCall, len(staged))
	allowAll := false
	hasApplyPatch := false
	hasFileEdit := false

	for i, call := range staged {
		results[i].ToolName = call.ToolName
		results[i].ToolCallID = call.ToolCallID
		if call.ToolName == "apply_patch" {
			hasApplyPatch = true
		}
		if call.ToolName == "edit_file" || call.ToolName == "write_file" {
			hasFileEdit = true
		}
	}
	if hasApplyPatch && hasFileEdit {
		for i := range results {
			results[i].Error = "staged apply_patch cannot be mixed with edit_file or write_file"
		}
		return results
	}
	batchFiles := stagedBatchFilesAtRoot(e.workspaceRoot, staged)

	for i, call := range staged {
		resolvedStaged[i] = call

		if call.ToolName == "write_file" {
			if _, ok := call.Params["content"].(string); !ok {
				results[i].Error = "write_file: content must be a string"
				continue
			}
		}

		execParams, err := resolveFileToolParamsAtRootWithOptions(e.workspaceRoot, call.ToolName, call.Params, e.options)
		if err != nil {
			results[i].Error = fmt.Sprintf("%s: resolve path: %v", call.ToolName, err)
			continue
		}
		resolvedStaged[i].Params = execParams
	}

	for i, call := range resolvedStaged {
		if results[i].Error != "" {
			continue
		}
		execParams := call.Params
		// apply_patch has no path param, so it uses the same multi-path target
		// plan as the immediate path. Other tools keep the per-arg check.
		var decision permission.Decision
		var targets []applyPatchTarget
		if call.ToolName == "apply_patch" {
			var err error
			targets, _, decision, err = applyPatchPermissionPlanWithOptions(e.check, e.workspaceRoot, execParams, e.options)
			if err != nil {
				results[i].Error = err.Error()
				continue
			}
		} else {
			decision = permission.DecisionAsk
			if e.check != nil {
				decision = e.check(call.ToolName, PermissionCheckArgAtRoot(e.workspaceRoot, call.ToolName, execParams))
			}
		}
		switch decision {
		case permission.DecisionAllow:
			allowed[i] = true
			if call.ToolName == "apply_patch" {
				resolvedStaged[i].Params = withApplyPatchReceipt(execParams, targets)
			}
		case permission.DecisionDeny:
			results[i].Error = "denied by user"
		default:
			if allowAll {
				allowed[i] = true
				if call.ToolName == "apply_patch" {
					resolvedStaged[i].Params = withApplyPatchReceipt(execParams, targets)
				}
				continue
			}
			if e.ask == nil {
				results[i].Error = "denied by user"
				continue
			}
			req := permissionRequestAtRoot(e.workspaceRoot, call.ToolName, staged[i].Params, execParams)
			if call.ToolName == "apply_patch" {
				req = applyPatchAskRequest(targets)
			}
			req.CanAllowAll = len(staged) > 1 && !hasApplyPatch
			req.BatchIndex = i + 1
			req.BatchTotal = len(staged)
			if call.ToolName != "apply_patch" {
				req.BatchFiles = batchFiles
			}
			action := e.ask(ctx, req)
			if action == permission.ResponseAllowAll && !req.CanAllowAll {
				action = permission.ResponseAllow
			}
			switch action {
			case permission.ResponseAllow:
				allowed[i] = true
				if call.ToolName == "apply_patch" {
					resolvedStaged[i].Params = withApplyPatchReceipt(execParams, targets)
				}
			case permission.ResponseAllowAll:
				allowed[i] = true
				allowAll = true
				if call.ToolName == "apply_patch" {
					resolvedStaged[i].Params = withApplyPatchReceipt(execParams, targets)
				}
			default:
				results[i].Error = "denied by user"
			}
		}
	}

	type fileGroup struct {
		absPath string
		indexes []int
	}
	var groups []*fileGroup
	groupIdx := map[string]*fileGroup{}
	for i, call := range resolvedStaged {
		if !allowed[i] {
			continue
		}
		if call.ToolName == "apply_patch" {
			// apply_patch has no path param; flushed through its own
			// commit below. The per-file grouping loop can't represent
			// a multi-file patch (it buckets by single path).
			continue
		}
		path, _ := call.Params["path"].(string)
		groupPath := canonicalPathFromParams(call.Params)
		if groupPath == "" {
			absPath, err := fileSecurityPathAtRoot(e.workspaceRoot, call.Params, path)
			if err != nil {
				results[i].Error = fmt.Sprintf("%s: resolve path: %v", call.ToolName, err)
				continue
			}
			groupPath = absPath
		}
		if g, ok := groupIdx[groupPath]; ok {
			g.indexes = append(g.indexes, i)
		} else {
			g := &fileGroup{absPath: groupPath, indexes: []int{i}}
			groups = append(groups, g)
			groupIdx[groupPath] = g
		}
	}

	for _, g := range groups {
		e.executeFileGroup(ctx, resolvedStaged, results, g.absPath, g.indexes)
	}

	// apply_patch staged flush. Each allowed call runs through the
	// engine (parse → FS-validate → snapshot-and-apply), which returns
	// the A/M/D summary. Partial mid-write failure returns *ExitError
	// whose Output is the committed-files summary plus the error. Use that body
	// as BatchResult.Error so the existing pending-result emitter sends it to
	// the model as an error result.
	for i, call := range resolvedStaged {
		if !allowed[i] || call.ToolName != "apply_patch" {
			continue
		}
		result, previews, err := applyPatchApplyAtRoot(ctx, e.workspaceRoot, e.store, e.tracker, call.Params)
		if err != nil {
			var exitErr *ExitError
			if errors.As(err, &exitErr) {
				results[i].Error = exitErr.Output
			} else {
				results[i].Error = err.Error()
			}
			continue
		}
		results[i].Result = result
		results[i].Metadata = applyPatchPreviewMetadata(previews)
		results[i].Success = true
	}

	return results
}

func stagedBatchFiles(staged []StagedCall) []string {
	return stagedBatchFilesAtRoot("", staged)
}

func stagedBatchFilesAtRoot(root string, staged []StagedCall) []string {
	files := make([]string, 0, len(staged))
	seen := map[string]bool{}
	for _, call := range staged {
		if call.ToolName == "apply_patch" {
			// apply_patch has no path param; parse the patch and
			// contribute its touched files to the batch prompt.
			for _, p := range applyPatchFilesAtRoot(root, call.Params) {
				if seen[p] {
					continue
				}
				seen[p] = true
				files = append(files, p)
			}
			continue
		}
		path, _ := call.Params["path"].(string)
		if path == "" {
			continue
		}
		if root != "" && !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		files = append(files, path)
	}
	return files
}

func (e *StagedExecutor) executeFileGroup(ctx context.Context, staged []StagedCall, results []BatchResult, absPath string, indexes []int) {
	if ctx.Err() != nil {
		for _, idx := range indexes {
			results[idx].Error = ctx.Err().Error()
		}
		return
	}
	// re-validate canonical: read-phase guard against approved-target swap before any I/O
	if !e.validateFileGroup(staged, results, absPath, indexes) {
		return
	}

	var data []byte
	existed, shapeErr := ensureRegularExistingTarget(absPath)
	if shapeErr != nil {
		for _, idx := range indexes {
			results[idx].Error = fmt.Sprintf("read %s: %v", absPath, shapeErr)
		}
		return
	}
	if existed {
		readFile, openErr := safefs.OpenExisting(absPath, os.O_RDONLY|unix.O_NONBLOCK)
		if openErr != nil {
			for _, idx := range indexes {
				results[idx].Error = fmt.Sprintf("read %s: %v", absPath, openErr)
			}
			return
		}
		info, err := readFile.Stat()
		if err != nil {
			_ = readFile.Close()
			for _, idx := range indexes {
				results[idx].Error = fmt.Sprintf("stat %s: %v", absPath, err)
			}
			return
		}
		if err := ensureRegularFileInfo(absPath, info); err != nil {
			_ = readFile.Close()
			for _, idx := range indexes {
				results[idx].Error = fmt.Sprintf("read %s: %v", absPath, err)
			}
			return
		}
		var readErr error
		data, readErr = io.ReadAll(readFile)
		_ = readFile.Close()
		if readErr != nil {
			for _, idx := range indexes {
				results[idx].Error = fmt.Sprintf("read %s: %v", absPath, readErr)
			}
			return
		}
	}
	content := string(data)
	successes := 0
	for _, idx := range indexes {
		call := staged[idx]
		displayPath, _ := call.Params["path"].(string)
		switch call.ToolName {
		case "edit_file":
			oldStr, _ := call.Params["old_string"].(string)
			newStr, _ := call.Params["new_string"].(string)
			replaceAll, _ := call.Params["replace_all"].(bool)
			res, err := ApplyEdit(content, oldStr, newStr, replaceAll, displayPath)
			if err != nil {
				results[idx].Error = err.Error()
				continue
			}
			content = res.UpdatedContent
			results[idx].Success = true
			results[idx].Result = res.Summary
			successes++
		case "write_file":
			writeContent, ok := call.Params["content"].(string)
			if !ok {
				results[idx].Error = "write_file: content must be a string"
				continue
			}
			content = writeContent
			results[idx].Success = true
			results[idx].Result = fmt.Sprintf("Wrote %s.", displayPath)
			successes++
		default:
			results[idx].Error = fmt.Sprintf("cannot stage %q", call.ToolName)
		}
	}

	if successes == 0 {
		return
	}
	// re-validate canonical: post-edit-buffer guard before entering the snapshot block
	if !e.validateFileGroup(staged, results, absPath, indexes) {
		return
	}

	var snapshot snapshotEntry
	if e.store != nil {
		displayPath, _ := staged[indexes[0]].Params["path"].(string)
		displayAbsPath, err := fileDisplayAbsPathAtRoot(e.workspaceRoot, displayPath)
		if err == nil {
			turn := e.store.CurrentTurn()
			// re-validate canonical: pre-snapshot guard against approved-target swap
			if !e.validateFileGroup(staged, results, absPath, indexes) {
				return
			}
			if _, shapeErr := ensureRegularExistingTarget(absPath); shapeErr != nil {
				e.failSuccessful(results, indexes, fmt.Sprintf("snapshot %s: %v", absPath, shapeErr))
				return
			}
			snapshot, err = snapshotFileForMutation(e.store, turn, displayAbsPath, absPath)
			if err == nil {
				defer releaseSnapshotMutation(snapshot)
			}
		}
		if err != nil {
			for _, idx := range indexes {
				if results[idx].Success {
					results[idx].Success = false
					results[idx].Result = ""
					results[idx].Error = fmt.Sprintf("snapshot: %v", err)
				}
			}
			return
		}
	}
	// re-validate canonical: post-snapshot, pre-write guard against approved-target swap
	if !e.validateFileGroup(staged, results, absPath, indexes) {
		if discardErr := discardUnmutatedSnapshot(snapshot); discardErr != nil {
			e.failSuccessful(results, indexes, fmt.Sprintf("discard snapshot: %v", discardErr))
		}
		return
	}
	if _, shapeErr := ensureRegularExistingTarget(absPath); shapeErr != nil {
		msg := fmt.Sprintf("write %s: %v", absPath, shapeErr)
		if discardErr := discardUnmutatedSnapshot(snapshot); discardErr != nil {
			msg = fmt.Sprintf("%s; additionally failed to discard snapshot: %v", msg, discardErr)
		}
		e.failSuccessful(results, indexes, msg)
		return
	}

	writeFile, _, mutationStarted, err := openWriteTargetForMutation(absPath, e.tracker)
	if err != nil {
		if !mutationStarted {
			if discardErr := discardUnmutatedSnapshot(snapshot); discardErr != nil {
				err = fmt.Errorf("%w; additionally failed to discard snapshot: %v", err, discardErr)
			}
		} else {
			err = retainFailedMutatedSnapshot(snapshot, absPath, err)
		}
		e.failSuccessful(results, indexes, fmt.Sprintf("write %s: %v", absPath, err))
		return
	}
	defer writeFile.Close()
	retainMutatedSnapshot(snapshot)
	if err := writeFile.Truncate(0); err != nil {
		err = retainFailedMutatedSnapshot(snapshot, absPath, err)
		e.failSuccessful(results, indexes, fmt.Sprintf("truncate %s: %v", absPath, err))
		return
	}
	if _, err := writeFile.Seek(0, io.SeekStart); err != nil {
		err = retainFailedMutatedSnapshot(snapshot, absPath, err)
		e.failSuccessful(results, indexes, fmt.Sprintf("seek %s: %v", absPath, err))
		return
	}
	if _, err := writeFile.Write([]byte(content)); err != nil {
		err = retainFailedMutatedSnapshot(snapshot, absPath, err)
		e.failSuccessful(results, indexes, fmt.Sprintf("write %s: %v", absPath, err))
		return
	}
	if err := writeFile.Sync(); err != nil {
		err = retainFailedMutatedSnapshot(snapshot, absPath, err)
		e.failSuccessful(results, indexes, fmt.Sprintf("sync %s: %v", absPath, err))
		return
	}
	if err := recordMutatedSnapshotContent(snapshot, []byte(content)); err != nil {
		retainMutatedSnapshot(snapshot)
		e.failSuccessful(results, indexes, fmt.Sprintf("record snapshot identity %s: %v", absPath, err))
		return
	}
}

func (e *StagedExecutor) validateFileGroup(staged []StagedCall, results []BatchResult, absPath string, indexes []int) bool {
	for _, idx := range indexes {
		call := staged[idx]
		path, _ := call.Params["path"].(string)
		current, err := fileSecurityPathAtRoot(e.workspaceRoot, call.Params, path)
		if err == nil && current == absPath {
			continue
		}
		msg := fmt.Sprintf("approved canonical path changed for %s", path)
		if err != nil {
			msg = fmt.Sprintf("%s: %v", msg, err)
		}
		for _, failIdx := range indexes {
			if results[failIdx].Success {
				results[failIdx].Success = false
				results[failIdx].Result = ""
			}
			results[failIdx].Error = msg
		}
		return false
	}
	return true
}

func (e *StagedExecutor) failSuccessful(results []BatchResult, indexes []int, msg string) {
	for _, idx := range indexes {
		if results[idx].Success {
			results[idx].Success = false
			results[idx].Result = ""
			results[idx].Error = msg
		}
	}
}
