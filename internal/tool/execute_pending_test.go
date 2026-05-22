package tool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
)

func TestExecutePendingFlushesMultipleFileGroups(t *testing.T) {
	first := stagedExecutorFile(t, "first.txt", "one")
	second := stagedExecutorFile(t, "second.txt", "two")
	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, first, 1, 100)
	trackIdentityForPath(t, tracker, second, 1, 100)
	executor := NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedEdit(first, "edit-1", "one", "ONE", false),
		stagedEdit(second, "edit-2", "two", "TWO", false),
	})

	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	assertBatchSuccess(t, results, 0, "edit_file", "edit-1")
	assertBatchSuccess(t, results, 1, "edit_file", "edit-2")
	assertFileContent(t, first, "ONE")
	assertFileContent(t, second, "TWO")
}

func TestExecutePendingReturnsPartialSuccessForEditError(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "same\nsame\nlast")
	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, path, 1, 100)
	executor := NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedEdit(path, "bad-edit", "same", "done", false),
		stagedEdit(path, "good-edit", "last", "done", false),
	})

	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	if results[0].Success || !strings.Contains(results[0].Error, "old_string matches 2 locations") {
		t.Fatalf("result[0] = %+v, want ambiguous-match error", results[0])
	}
	assertBatchSuccess(t, results, 1, "edit_file", "good-edit")
	assertFileContent(t, path, "same\nsame\ndone")
}

func TestExecutePendingUnknownToolNameReturnsPerCallError(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "before")
	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, path, 1, 100)
	executor := NewStagedExecutor(nil, tracker, config.ToolsConfig{}, allowStagedCall, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{{
		ToolName:   "delete_file",
		ToolCallID: "unknown-1",
		Params: map[string]any{
			"path": path,
		},
	}})

	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Success || results[0].ToolName != "delete_file" || results[0].ToolCallID != "unknown-1" || !strings.Contains(results[0].Error, "cannot stage \"delete_file\"") {
		t.Fatalf("result[0] = %+v, want unknown-tool error with identity", results[0])
	}
	assertFileContent(t, path, "before")
}

func TestExecutePendingSnapshotErrorMarksAllSuccessfulCallsFailed(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "before")
	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, path, 1, 100)
	store := &failingSnapshotStore{err: errors.New("snapshot failed")}
	executor := NewStagedExecutor(store, tracker, config.ToolsConfig{}, allowStagedCall, nil)

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedEdit(path, "edit-1", "before", "middle", false),
		stagedEdit(path, "edit-2", "middle", "after", false),
	})

	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	for i, result := range results {
		if result.Success || !strings.Contains(result.Error, "snapshot failed") {
			t.Fatalf("result[%d] = %+v, want snapshot failure", i, result)
		}
	}
	assertFileContent(t, path, "before")
}

func TestExecutePendingSingleAskCannotAllowAll(t *testing.T) {
	path := stagedExecutorFile(t, "file.txt", "before")
	tracker := NewFileTracker()
	trackIdentityForPath(t, tracker, path, 1, 100)
	var gotCanAllowAll bool
	executor := NewStagedExecutor(nil, tracker, config.ToolsConfig{}, func(string, string) permission.Decision {
		return permission.DecisionAsk
	}, func(_ context.Context, req permission.Request) permission.ResponseAction {
		gotCanAllowAll = req.CanAllowAll
		return permission.ResponseAllow
	})

	results := executor.ExecutePending(context.Background(), []StagedCall{
		stagedEdit(path, "edit-1", "before", "after", false),
	})

	if gotCanAllowAll {
		t.Fatalf("single pending call CanAllowAll = true, want false")
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	assertBatchSuccess(t, results, 0, "edit_file", "edit-1")
	assertFileContent(t, path, "after")
}

type failingSnapshotStore struct {
	err error
}

func (s *failingSnapshotStore) Snapshot(int, string) error {
	return s.err
}

func (s *failingSnapshotStore) CurrentTurn() int {
	return 0
}
