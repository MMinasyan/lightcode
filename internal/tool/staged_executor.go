package tool

import (
	"context"
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

		if call.ToolName == "write_file" {
			if _, ok := call.Params["content"].(string); !ok {
				results[i].Error = "write_file: content must be a string"
				continue
			}
		}

		execParams, err := resolveFileToolParams(call.ToolName, call.Params)
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
			req := permissionRequest(call.ToolName, staged[i].Params, execParams)
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
		groupPath := canonicalPathFromParams(call.Params)
		if groupPath == "" {
			absPath, err := fileSecurityPath(call.Params, path)
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
		if readErr == nil && e.tracker != nil {
			if err := e.tracker.WasReadCheckIdentity(absPath, FileIdentityFromFileInfoAndData(info, data)); err != nil {
				_ = readFile.Close()
				for _, idx := range indexes {
					results[idx].Error = err.Error()
				}
				return
			}
		}
		_ = readFile.Close()
		if readErr != nil {
			for _, idx := range indexes {
				results[idx].Error = fmt.Sprintf("read %s: %v", absPath, readErr)
			}
			return
		}
	}
	if !existed && e.tracker != nil && e.tracker.HasRead(absPath) {
		for _, idx := range indexes {
			results[idx].Error = (&FileChangedError{Path: absPath}).Error()
		}
		return
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
	if !e.validateFileGroup(staged, results, absPath, indexes) {
		return
	}

	if e.store != nil {
		displayPath, _ := staged[indexes[0]].Params["path"].(string)
		displayAbsPath, err := fileDisplayAbsPath(displayPath)
		if err == nil {
			turn := e.store.CurrentTurn()
			if !e.validateFileGroup(staged, results, absPath, indexes) {
				return
			}
			if _, shapeErr := ensureRegularExistingTarget(absPath); shapeErr != nil {
				e.failSuccessful(results, indexes, fmt.Sprintf("snapshot %s: %v", absPath, shapeErr))
				return
			}
			err = snapshotFile(e.store, turn, displayAbsPath, absPath)
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
	if !e.validateFileGroup(staged, results, absPath, indexes) {
		return
	}
	if _, shapeErr := ensureRegularExistingTarget(absPath); shapeErr != nil {
		e.failSuccessful(results, indexes, fmt.Sprintf("write %s: %v", absPath, shapeErr))
		return
	}

	writeFile, _, err := openWriteTarget(absPath, e.tracker)
	if err != nil {
		e.failSuccessful(results, indexes, fmt.Sprintf("write %s: %v", absPath, err))
		return
	}
	defer writeFile.Close()
	if err := writeFile.Truncate(0); err != nil {
		e.failSuccessful(results, indexes, fmt.Sprintf("truncate %s: %v", absPath, err))
		return
	}
	if _, err := writeFile.Seek(0, io.SeekStart); err != nil {
		e.failSuccessful(results, indexes, fmt.Sprintf("seek %s: %v", absPath, err))
		return
	}
	if _, err := writeFile.Write([]byte(content)); err != nil {
		e.failSuccessful(results, indexes, fmt.Sprintf("write %s: %v", absPath, err))
		return
	}
	if err := writeFile.Sync(); err != nil {
		e.failSuccessful(results, indexes, fmt.Sprintf("sync %s: %v", absPath, err))
		return
	}

	if e.tracker != nil {
		if info, err := writeFile.Stat(); err == nil {
			e.tracker.UpdateAfterWriteIdentity(absPath, FileIdentityFromFileInfoAndData(info, []byte(content)))
		}
	}
}

func (e *StagedExecutor) validateFileGroup(staged []StagedCall, results []BatchResult, absPath string, indexes []int) bool {
	for _, idx := range indexes {
		call := staged[idx]
		path, _ := call.Params["path"].(string)
		current, err := fileSecurityPath(call.Params, path)
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
