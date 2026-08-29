package engine

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/permission"
	runtimetool "github.com/MMinasyan/lightcode/internal/tool"
)

// snapshotCall records one Snapshot call: the pre-mutation state of a path
// captured at a turn. The forged-pending test uses these to prove the tool
// executed synchronously (snapshot happens during Execute, before return).
type snapshotCall struct {
	turn int
	path string
}

// recordingSnapshotStore is a SnapshotStore that records every snapshot call.
// It implements the transactional interfaces write/edit/apply_patch use so the
// forged-pending test exercises the same snapshot path production tools use.
type recordingSnapshotStore struct {
	turn  int
	calls []snapshotCall
}

func (s *recordingSnapshotStore) Snapshot(turn int, absPath string) error {
	return s.SnapshotResolved(turn, absPath, absPath)
}

func (s *recordingSnapshotStore) SnapshotResolved(turn int, originalPath, canonicalPath string) error {
	s.calls = append(s.calls, snapshotCall{turn: turn, path: canonicalPath})
	return nil
}

func (s *recordingSnapshotStore) SnapshotResolvedEntry(turn int, originalPath, canonicalPath string) (string, bool, error) {
	if err := s.SnapshotResolved(turn, originalPath, canonicalPath); err != nil {
		return "", false, err
	}
	return canonicalPath, true, nil
}

func (s *recordingSnapshotStore) DiscardSnapshotEntry(int, string) error { return nil }
func (s *recordingSnapshotStore) RetainSnapshotEntry(int, string)        {}
func (s *recordingSnapshotStore) RecordSnapshotContent(int, string, []byte) error {
	return nil
}
func (s *recordingSnapshotStore) RecordSnapshotAbsence(int, string) error { return nil }
func (s *recordingSnapshotStore) CurrentTurn() int                        { return s.turn }

// alwaysAllow is a permission check that approves every call so the test focuses
// on staging behavior rather than the ask/allow/deny gate.
func alwaysAllow(string, string) permission.Decision { return permission.DecisionAllow }

// TestDispatchRejectsForgedExecutePending closes the execute_pending matrix row
// at the dispatch layer: the tool is absent from the registry, so a forged call
// receives the normal unknown-tool result. Unlike the integration coverage in
// internal/agent, this runs untagged so the focused gate exercises it.
func TestDispatchRejectsForgedExecutePending(t *testing.T) {
	reg := runtimetool.NewRegistry()
	reg.Register(runtimetool.WrapWithPermissionAtRoot(runtimetool.NewWriteFile(nil, config.ToolsConfig{}), t.TempDir(), alwaysAllow, nil))
	lp := &Loop{registry: reg, trace: io.Discard}

	args, _ := json.Marshal(map[string]any{})
	res := lp.dispatch(context.Background(), message.ToolCall{
		ID:       "call-forged-execute",
		Type:     "function",
		Function: message.FunctionCall{Name: "execute_pending", Arguments: string(args)},
	})

	if !strings.Contains(res.Content, `unknown tool "execute_pending"`) {
		t.Fatalf("forged execute_pending result = %q, want unknown-tool result", res.Content)
	}
	if !res.IsError {
		t.Fatalf("forged execute_pending result IsError = false, want true")
	}
}

// TestForgedPendingExecutesImmediately closes the forged-pending matrix row at
// the dispatch layer: passing pending=true to write_file, edit_file, and
// apply_patch must perform exactly one immediate settlement and never enqueue or
// flush later. It runs the table through the root registry built from the same
// CoreToolListWithOptions construction path the main agent uses, and proves, per
// case:
//   - the file is on disk with the immediate content before dispatch returns
//     (no deferred effect), and
//   - the snapshot store captured the target synchronously (one snapshot call
//     for the touched path), and
//   - exactly one ToolCallEnd is emitted for the call (one settlement, no
//     second late end), and
//   - the result is the immediate tool result, not a "Staged." settlement.
//
// Real task-child coverage for the same table lives in internal/agent
// (TestTaskToolChildForgedPendingExecutesImmediately) and drives the child
// through taskTool.buildRegistry/buildChildLoop with parent-turn snapshot scope.
func TestForgedPendingExecutesImmediately(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		seed      string
		arguments map[string]any
		want      string
	}{
		{
			name: "write_file",
			path: filepath.Join("write.txt"),
			arguments: map[string]any{
				"path":    filepath.Join("write.txt"),
				"content": "immediate-content",
				"pending": true,
			},
			want: "immediate-content",
		},
		{
			name: "edit_file",
			path: filepath.Join("edit.txt"),
			seed: "seed-text\n",
			arguments: map[string]any{
				"path":       filepath.Join("edit.txt"),
				"old_string": "seed-text",
				"new_string": "replaced-text",
				"pending":    true,
			},
			want: "replaced-text\n",
		},
		{
			name: "apply_patch",
			path: filepath.Join("patch.txt"),
			arguments: map[string]any{
				"input":   "*** Begin Patch\n*** Add File: " + filepath.Join("patch.txt") + "\n+patched-content\n*** End Patch",
				"pending": true,
			},
			want: "patched-content",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			absPath := filepath.Join(root, tc.path)
			if tc.seed != "" {
				if err := os.WriteFile(absPath, []byte(tc.seed), 0o644); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}

			store := &recordingSnapshotStore{turn: 1}
			core := runtimetool.CoreToolsWithOptions(store, runtimetool.NewFileTracker(), config.ToolsConfig{}, root, alwaysAllow, nil, runtimetool.CapabilityOptions{})
			reg := runtimetool.NewRegistry()
			for _, name := range []string{"read_file", "write_file", "edit_file", "apply_patch"} {
				if tt, ok := core[name]; ok {
					reg.Register(tt)
				}
			}
			lp := &Loop{registry: reg, trace: io.Discard}
			// apply_patch is DefaultHidden; a non-nil baseline advertisement that
			// includes it advertises all three tools through the dispatch gate.
			lp.SetActiveAdaptation(&adaptation.Adaptation{IncludeTools: []string{"apply_patch"}})
			events := make(chan Event, 8)
			lp.SetEvents(events)

			args, _ := json.Marshal(tc.arguments)
			res := lp.dispatch(context.Background(), message.ToolCall{
				ID:       "call-pending",
				Type:     "function",
				Function: message.FunctionCall{Name: tc.name, Arguments: string(args)},
			})

			if strings.Contains(res.Content, "Staged.") {
				t.Fatalf("%s with pending=true settled as staged: %q", tc.name, res.Content)
			}
			if !strings.Contains(res.Content, tc.path) {
				t.Fatalf("%s immediate result %q does not reference target path %q (staging may have been skipped)", tc.name, res.Content, tc.path)
			}
			// Immediate on-disk effect before dispatch returns: the file exists
			// with the exact immediate content (no deferred/flushed write).
			got, err := os.ReadFile(absPath)
			if err != nil {
				t.Fatalf("%s file not written before return: %v", tc.name, err)
			}
			if string(got) != tc.want {
				t.Fatalf("%s content = %q, want %q", tc.name, string(got), tc.want)
			}
			// Exactly one settlement: the tool executed once synchronously, so
			// the snapshot store saw exactly one capture of the target path.
			snapCalls := 0
			for _, c := range store.calls {
				if c.path == absPath {
					snapCalls++
				}
			}
			if snapCalls != 1 {
				t.Fatalf("%s snapshot captures of %s = %d, want exactly 1 (one immediate settlement)", tc.name, absPath, snapCalls)
			}
			// Exactly one ToolCallEnd for the forged call: a staging flush would
			// emit a second late end for the same id.
			ends := 0
			for _, ev := range drainEvents(events) {
				if ev.Kind == ToolCallEnd && ev.ToolCallID == "call-pending" {
					ends++
				}
			}
			if ends != 1 {
				t.Fatalf("%s ToolCallEnd count for call-pending = %d, want exactly 1", tc.name, ends)
			}
		})
	}
}
