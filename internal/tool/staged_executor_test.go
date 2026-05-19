package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
)

func TestStagedExecutorAllowAllSkipsSubsequentPrompts(t *testing.T) {
	pathA := stagedExecutorFile(t, "a.txt", "one")
	pathB := stagedExecutorFile(t, "b.txt", "two")
	askCalls := 0
	executor := NewStagedExecutor(nil, nil, config.ToolsConfig{}, func(string, string) permission.Decision {
		return permission.DecisionAsk
	}, func(_ context.Context, req permission.Request) permission.ResponseAction {
		askCalls++
		if !req.CanAllowAll || req.BatchIndex != 1 || req.BatchTotal != 2 {
			t.Fatalf("permission request = %+v, want first request for 2-item allow-all batch", req)
		}
		if len(req.BatchFiles) != 2 {
			t.Fatalf("BatchFiles length = %d, want 2", len(req.BatchFiles))
		}
		return permission.ResponseAllowAll
	})

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedWrite(pathA, "call-a", "updated one"),
		stagedWrite(pathB, "call-b", "updated two"),
	})

	if askCalls != 1 {
		t.Fatalf("ask calls = %d, want 1", askCalls)
	}
	assertBatchSuccess(t, results, 0, "write_file", "call-a")
	assertBatchSuccess(t, results, 1, "write_file", "call-b")
	assertFileContent(t, pathA, "updated one")
	assertFileContent(t, pathB, "updated two")
}

func TestStagedExecutorDeniedCallReturnsError(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "content")
	executor := NewStagedExecutor(nil, nil, config.ToolsConfig{}, func(string, string) permission.Decision {
		return permission.DecisionDeny
	}, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedWrite(path, "call-1", "updated"),
	})

	if len(results) != 1 {
		t.Fatalf("results length = %d, want 1", len(results))
	}
	if results[0].Success {
		t.Fatalf("result success = true, want false: %+v", results[0])
	}
	if results[0].Error != "denied by user" {
		t.Fatalf("result error = %q, want denied by user", results[0].Error)
	}
	assertFileContent(t, path, "content")
}

func TestStagedExecutorAppliesEditsInEmissionOrder(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "alpha beta gamma")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	executor := NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedEdit(path, "call-1", "alpha", "one", false),
		stagedEdit(path, "call-2", "one beta", "two", false),
		stagedEdit(path, "call-3", "two gamma", "done", false),
	})

	for i, result := range results {
		assertBatchSuccess(t, results, i, "edit_file", result.ToolCallID)
	}
	assertFileContent(t, path, "done")
}

func TestStagedExecutorWriteReplacesRunningBuffer(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "before")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	executor := NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedEdit(path, "call-1", "before", "intermediate", false),
		stagedWrite(path, "call-2", "fresh target"),
		stagedEdit(path, "call-3", "target", "result", false),
	})

	for i, result := range results {
		assertBatchSuccess(t, results, i, results[i].ToolName, result.ToolCallID)
	}
	assertFileContent(t, path, "fresh result")
}

func TestStagedExecutorRejectsMalformedWriteContent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{
			name: "missing",
			params: map[string]any{
				"path": "",
			},
		},
		{
			name: "non-string",
			params: map[string]any{
				"path":    "",
				"content": 12,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := stagedExecutorFile(t, "file.txt", "before")
			tc.params["path"] = path
			askCalls := 0
			executor := NewStagedExecutor(nil, NewFileTracker(), config.ToolsConfig{}, func(string, string) permission.Decision {
				return permission.DecisionAsk
			}, func(context.Context, permission.Request) permission.ResponseAction {
				askCalls++
				return permission.ResponseAllow
			})

			results := executor.ExecutePending(context.Background(), []StagedCall{{
				ToolName:   "write_file",
				ToolCallID: "call-1",
				Params:     tc.params,
			}})

			if len(results) != 1 {
				t.Fatalf("results length = %d, want 1", len(results))
			}
			if results[0].Success || results[0].Error != "write_file: content must be a string" {
				t.Fatalf("result = %+v, want content type error", results[0])
			}
			if askCalls != 0 {
				t.Fatalf("ask calls = %d, want 0", askCalls)
			}
			assertFileContent(t, path, "before")
		})
	}
}

func TestStagedExecutorAllowsEmptyWriteContent(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "before")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	executor := NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedWrite(path, "call-1", ""),
	})

	assertBatchSuccess(t, results, 0, "write_file", "call-1")
	assertFileContent(t, path, "")
}

func TestStagedExecutorAttributesEditErrorsPerCall(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "same same unique")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	executor := NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedEdit(path, "call-1", "same", "changed", false),
		stagedEdit(path, "call-2", "unique", "done", false),
	})

	if len(results) != 2 {
		t.Fatalf("results length = %d, want 2", len(results))
	}
	if results[0].Success || !strings.Contains(results[0].Error, "old_string matches 2 locations") {
		t.Fatalf("first result = %+v, want ambiguous-match error only", results[0])
	}
	assertBatchSuccess(t, results, 1, "edit_file", "call-2")
	assertFileContent(t, path, "same same done")
}

func TestStagedExecutorFileAppearingBeforeFlushRequiresRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appeared.txt")
	call := stagedWrite(path, "call-1", "new content")
	if err := os.WriteFile(path, []byte("external content"), 0o644); err != nil {
		t.Fatal(err)
	}
	executor := NewStagedExecutor(nil, NewFileTracker(), config.ToolsConfig{}, allowStagedCall, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{call})

	if len(results) != 1 {
		t.Fatalf("results length = %d, want 1", len(results))
	}
	if results[0].Success {
		t.Fatalf("result success = true, want read-before-write error: %+v", results[0])
	}
	if !strings.Contains(results[0].Error, "has not been read yet") {
		t.Fatalf("result error = %q, want read-required error", results[0].Error)
	}
	assertFileContent(t, path, "external content")
}

func TestStagedExecutorSnapshotsBeforeFinalWrite(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "before")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	store := &recordingSnapshotStore{turn: 7, before: map[string]string{}}
	executor := NewStagedExecutor(store, tracker, config.ToolsConfig{}, allowStagedCall, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedEdit(path, "call-1", "before", "after", false),
	})

	assertBatchSuccess(t, results, 0, "edit_file", "call-1")
	if len(store.calls) != 1 {
		t.Fatalf("snapshot calls = %d, want 1", len(store.calls))
	}
	if store.calls[0].turn != 7 || store.calls[0].path != path {
		t.Fatalf("snapshot call = %+v, want turn=7 path=%q", store.calls[0], path)
	}
	if store.before[path] != "before" {
		t.Fatalf("content observed by Snapshot = %q, want pre-write content", store.before[path])
	}
	assertFileContent(t, path, "after")
}

func TestStagedExecutorGroupsAliasesInternallyAndDisplaysRequestedPaths(t *testing.T) {
	realPath := stagedExecutorFile(t, "file.txt", "alpha beta")
	dir := filepath.Dir(realPath)
	alias1 := filepath.Join(dir, "alias1.txt")
	alias2 := filepath.Join(dir, "alias2.txt")
	if err := os.Symlink(realPath, alias1); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, alias2); err != nil {
		t.Fatal(err)
	}
	tracker := NewFileTracker()
	tracker.Track(realPath, 1, 100)
	store := &recordingSnapshotStore{turn: 8, before: map[string]string{}}
	var batchFiles []string
	var request permission.Request
	executor := NewStagedExecutor(store, tracker, config.ToolsConfig{}, func(string, string) permission.Decision {
		return permission.DecisionAsk
	}, func(_ context.Context, req permission.Request) permission.ResponseAction {
		request = req
		batchFiles = append([]string(nil), req.BatchFiles...)
		return permission.ResponseAllowAll
	})

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedEdit(alias1, "call-1", "alpha", "one", false),
		stagedEdit(alias2, "call-2", "one beta", "done", false),
	})

	assertBatchSuccess(t, results, 0, "edit_file", "call-1")
	assertBatchSuccess(t, results, 1, "edit_file", "call-2")
	assertFileContent(t, realPath, "done")
	if results[0].Result != "Edited "+alias1+" (1 replacement, lines 1-1)." {
		t.Fatalf("result[0] = %q, want requested alias", results[0].Result)
	}
	if results[1].Result != "Edited "+alias2+" (1 replacement, lines 1-1)." {
		t.Fatalf("result[1] = %q, want requested alias", results[1].Result)
	}
	if len(batchFiles) != 2 || batchFiles[0] != alias1 || batchFiles[1] != alias2 {
		t.Fatalf("BatchFiles = %v, want requested aliases [%s %s]", batchFiles, alias1, alias2)
	}
	if request.Arg != alias1 {
		t.Fatalf("request arg = %q, want requested alias %q", request.Arg, alias1)
	}
	if request.ResolvedArg != realPath {
		t.Fatalf("request resolved arg = %q, want canonical %q", request.ResolvedArg, realPath)
	}
	if len(store.calls) != 1 {
		t.Fatalf("snapshot calls = %d, want 1", len(store.calls))
	}
	if store.calls[0].path != alias1 || store.calls[0].canonical != realPath {
		t.Fatalf("snapshot call = %+v, want original=%q canonical=%q", store.calls[0], alias1, realPath)
	}
}

func TestStagedExecutorCancelledContextFailsAllCalls(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "before")
	tracker := NewFileTracker()
	tracker.Track(path, 1, 100)
	executor := NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results := executor.ExecutePending(ctx, []StagedCall{
		stagedEdit(path, "call-1", "before", "after", false),
		stagedWrite(path, "call-2", "other"),
	})

	if len(results) != 2 {
		t.Fatalf("results length = %d, want 2", len(results))
	}
	for i, result := range results {
		if result.Success || result.Error != context.Canceled.Error() {
			t.Fatalf("result[%d] = %+v, want context canceled error", i, result)
		}
	}
	assertFileContent(t, path, "before")
}

type snapshotCall struct {
	turn      int
	path      string
	canonical string
}

type recordingSnapshotStore struct {
	turn   int
	calls  []snapshotCall
	before map[string]string
}

func (s *recordingSnapshotStore) Snapshot(turn int, absPath string) error {
	return s.SnapshotResolved(turn, absPath, absPath)
}

func (s *recordingSnapshotStore) SnapshotResolved(turn int, originalPath, canonicalPath string) error {
	data, err := os.ReadFile(canonicalPath)
	if err != nil {
		return err
	}
	s.calls = append(s.calls, snapshotCall{turn: turn, path: originalPath, canonical: canonicalPath})
	s.before[canonicalPath] = string(data)
	return nil
}

func (s *recordingSnapshotStore) CurrentTurn() int {
	return s.turn
}

func allowStagedCall(string, string) permission.Decision {
	return permission.DecisionAllow
}

func stagedExecutorFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func stagedWrite(path, id, content string) StagedCall {
	return StagedCall{
		ToolName:   "write_file",
		ToolCallID: id,
		Params: map[string]any{
			"path":    path,
			"content": content,
		},
	}
}

func stagedEdit(path, id, oldString, newString string, replaceAll bool) StagedCall {
	return StagedCall{
		ToolName:   "edit_file",
		ToolCallID: id,
		Params: map[string]any{
			"path":        path,
			"old_string":  oldString,
			"new_string":  newString,
			"replace_all": replaceAll,
		},
	}
}

func assertBatchSuccess(t *testing.T, results []BatchResult, idx int, toolName, toolCallID string) {
	t.Helper()
	if idx >= len(results) {
		t.Fatalf("result index %d out of range len=%d", idx, len(results))
	}
	result := results[idx]
	if !result.Success || result.Error != "" {
		t.Fatalf("result[%d] = %+v, want success", idx, result)
	}
	if result.ToolName != toolName || result.ToolCallID != toolCallID {
		t.Fatalf("result[%d] identity = (%q, %q), want (%q, %q)", idx, result.ToolName, result.ToolCallID, toolName, toolCallID)
	}
	if result.Result == "" {
		t.Fatalf("result[%d] has empty Result", idx)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != want {
		t.Fatalf("content of %s = %q, want %q", path, got, want)
	}
}
