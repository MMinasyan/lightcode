package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
)

// AskActionFunc blocks until the user answers a staged permission prompt.
type AskActionFunc func(ctx context.Context, req permission.Request) permission.ResponseAction

// StagedExecutor executes pending edit/write calls as a batch while
// preserving the normal permission, snapshot, and read-before-edit rules.
type StagedExecutor struct {
	store   SnapshotStore
	tracker *FileTracker
	cfg     config.ToolsConfig
	check   CheckFunc
	ask     AskActionFunc
}

// NewStagedExecutor creates a staged batch executor.
func NewStagedExecutor(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig, check CheckFunc, ask AskActionFunc) *StagedExecutor {
	return &StagedExecutor{
		store:   store,
		tracker: tracker,
		cfg:     cfg,
		check:   check,
		ask:     ask,
	}
}

// ExecutePending applies staged calls in emission order, using a running
// buffer per file and a single final disk write per changed file.
func (e *StagedExecutor) ExecutePending(ctx context.Context, staged []StagedCall) []BatchResult {
	results := make([]BatchResult, len(staged))
	allowed := make([]bool, len(staged))
	resolvedStaged := make([]StagedCall, len(staged))
	allowAll := false
	batchFiles := stagedBatchFiles(staged)

	for i, call := range staged {
		resolvedStaged[i] = call
		results[i].ToolName = call.ToolName
		results[i].ToolCallID = call.ToolCallID

		execParams, err := resolveFileToolParams(call.ToolName, call.Params)
		if err != nil {
			results[i].Error = fmt.Sprintf("%s: resolve path: %v", call.ToolName, err)
			continue
		}
		resolvedStaged[i].Params = execParams

		decision := permission.DecisionAsk
		if e.check != nil {
			decision = e.check(call.ToolName, PermissionCheckArg(call.ToolName, execParams))
		}
		switch decision {
		case permission.DecisionAllow:
			allowed[i] = true
		case permission.DecisionDeny:
			results[i].Error = "denied by user"
		default:
			if allowAll {
				allowed[i] = true
				continue
			}
			if e.ask == nil {
				results[i].Error = "denied by user"
				continue
			}
			req := PermissionRequest(call.ToolName, call.Params, execParams)
			req.CanAllowAll = len(staged) > 1
			req.BatchIndex = i + 1
			req.BatchTotal = len(staged)
			req.BatchFiles = batchFiles
			action := e.ask(ctx, req)
			switch action {
			case permission.ResponseAllow:
				allowed[i] = true
			case permission.ResponseAllowAll:
				allowed[i] = true
				allowAll = true
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
		path, _ := call.Params["path"].(string)
		absPath, err := fileSecurityPath(call.Params, path)
		if err != nil {
			results[i].Error = fmt.Sprintf("%s: resolve path: %v", call.ToolName, err)
			continue
		}
		if g, ok := groupIdx[absPath]; ok {
			g.indexes = append(g.indexes, i)
		} else {
			g := &fileGroup{absPath: absPath, indexes: []int{i}}
			groups = append(groups, g)
			groupIdx[absPath] = g
		}
	}

	for _, g := range groups {
		e.executeFileGroup(ctx, resolvedStaged, results, g.absPath, g.indexes)
	}

	return results
}

func stagedBatchFiles(staged []StagedCall) []string {
	files := make([]string, 0, len(staged))
	seen := map[string]bool{}
	for _, call := range staged {
		path, _ := call.Params["path"].(string)
		if path == "" {
			continue
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

	data, readErr := os.ReadFile(absPath)
	exists := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		for _, idx := range indexes {
			results[idx].Error = fmt.Sprintf("read %s: %v", absPath, readErr)
		}
		return
	}

	if exists && e.tracker != nil {
		if err := e.tracker.WasReadCheck(absPath); err != nil {
			for _, idx := range indexes {
				results[idx].Error = err.Error()
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
			writeContent, _ := call.Params["content"].(string)
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

	if e.store != nil {
		displayPath, _ := staged[indexes[0]].Params["path"].(string)
		displayAbsPath, err := fileDisplayAbsPath(displayPath)
		if err == nil {
			err = snapshotFile(e.store, e.store.CurrentTurn(), displayAbsPath, absPath)
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

	if dir := filepath.Dir(absPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			e.failSuccessful(results, indexes, fmt.Sprintf("mkdir %s: %v", dir, err))
			return
		}
	}
	// #nosec G703 -- path is permission-gated by PermWrapped (internal/tool/permwrap.go); user approves each call before execution.
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		e.failSuccessful(results, indexes, fmt.Sprintf("write %s: %v", absPath, err))
		return
	}

	if e.tracker != nil {
		e.tracker.UpdateAfterWrite(absPath)
	}
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
