package snapshot

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

const (
	durabilityProcessOpEnv      = "LIGHTCODE_DURABILITY_PROCESS_OP"
	durabilityProcessRootEnv    = "LIGHTCODE_DURABILITY_PROCESS_ROOT"
	durabilityProcessProjectEnv = "LIGHTCODE_DURABILITY_PROCESS_PROJECT"
	durabilityProcessPathEnv    = "LIGHTCODE_DURABILITY_PROCESS_PATH"
	durabilityProcessTargetEnv  = "LIGHTCODE_DURABILITY_PROCESS_TARGET"
)

// TestDurabilityProcessRows uses real child processes to place a kill between
// a namespace rename and its required parent sync. It proves hook placement
// and execution; killing a test process after the marker is not a power-loss
// simulation.
func TestDurabilityProcessRows(t *testing.T) {
	if op := os.Getenv(durabilityProcessOpEnv); op != "" {
		runDurabilityProcessChild(t, op)
		return
	}
	for _, op := range []string{"staged", "fork", "delete", "child"} {
		t.Run(op+"_rename_before_sync", func(t *testing.T) {
			root := t.TempDir()
			projectID := "p-process-" + op
			projectPath := t.TempDir()
			runDurabilityProcess(t, op, root, projectID, projectPath, filepath.Join(root, projectID, "sessions"), "parked")
			verifyDurabilityProcessState(t, op, root, projectID)
		})
	}
	t.Run("child_success_after_real_sync", func(t *testing.T) {
		root := t.TempDir()
		projectID := "p-process-child-success"
		runDurabilityProcess(t, "child-success", root, projectID, t.TempDir(), filepath.Join(root, projectID, "sessions"), "committed")
		verifyChildState(t, root, projectID)
	})
}

func runDurabilityProcess(t *testing.T, op, root, projectID, projectPath, target, marker string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDurabilityProcessRows$")
	cmd.Env = append(os.Environ(),
		durabilityProcessOpEnv+"="+op,
		durabilityProcessRootEnv+"="+root,
		durabilityProcessProjectEnv+"="+projectID,
		durabilityProcessPathEnv+"="+projectPath,
		durabilityProcessTargetEnv+"="+target,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	markerSeen := make(chan struct{})
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		for scanner := bufio.NewScanner(stdout); scanner.Scan(); {
			if scanner.Text() == marker {
				close(markerSeen)
				return
			}
		}
	}()
	select {
	case <-markerSeen:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		<-scanDone
		t.Fatalf("child %s did not print %q\n%s", op, marker, stderr.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child %s: %v", op, err)
	}
	_ = cmd.Wait()
	<-scanDone
}

func runDurabilityProcessChild(t *testing.T, op string) {
	root := os.Getenv(durabilityProcessRootEnv)
	projectID := os.Getenv(durabilityProcessProjectEnv)
	projectPath := os.Getenv(durabilityProcessPathEnv)
	sessionsRoot := filepath.Join(root, projectID, "sessions")
	switch op {
	case "staged":
		store, err := NewForSessionsRoot(sessionsRoot, root, projectID)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := store.PrepareStagedNewSession(projectPath)
		if err != nil {
			t.Fatal(err)
		}
		parkSync(t, prepared.Root())
		if err := store.PublishPreparedSession(prepared); err != nil {
			t.Fatal(err)
		}
	case "fork":
		store, err := NewForSessionsRoot(sessionsRoot, root, projectID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.BeginNewSession(projectPath); err != nil {
			t.Fatal(err)
		}
		sourceID := store.SessionID()
		turn := store.BeginTurn()
		if err := store.AppendMessage(turn, []byte(`{"role":"user","content":"fork"}`)); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkTurnComplete(turn); err != nil {
			t.Fatal(err)
		}
		stagingRoot, err := NewStagingSessionsRoot(sessionsRoot)
		if err != nil {
			t.Fatal(err)
		}
		id, _, err := store.ForkInto(turn, stagingRoot)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := store.NewStagingStore(stagingRoot)
		if err != nil {
			t.Fatal(err)
		}
		if err := candidate.LoadSession(id); err != nil {
			t.Fatal(err)
		}
		if err := candidate.SetModel("provider", "model"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, projectID, ".durability-fork-source"), []byte(sourceID), 0o600); err != nil {
			t.Fatal(err)
		}
		parkSync(t, stagingRoot)
		if err := PublishStagedSession(stagingRoot, sessionsRoot, id); err != nil {
			t.Fatal(err)
		}
	case "delete":
		store, err := NewForSessionsRoot(sessionsRoot, root, projectID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.BeginNewSession(projectPath); err != nil {
			t.Fatal(err)
		}
		id := store.SessionID()
		store.Detach()
		parkAt := os.Getenv(durabilityProcessTargetEnv)
		parkSync(t, parkAt)
		if err := DeleteSession(sessionsRoot, id); err != nil {
			t.Fatal(err)
		}
	case "child", "child-success":
		parent, err := NewForSessionsRoot(sessionsRoot, root, projectID)
		if err != nil {
			t.Fatal(err)
		}
		if err := parent.BeginNewSession(projectPath); err != nil {
			t.Fatal(err)
		}
		parentID := parent.SessionID()
		parent.Detach()
		child, err := NewForSessionsRoot(sessionsRoot, root, projectID)
		if err != nil {
			t.Fatal(err)
		}
		if op == "child" {
			parkSync(t, os.Getenv(durabilityProcessTargetEnv))
		}
		if err := child.BeginChildSession(projectPath, parentID); err != nil {
			t.Fatal(err)
		}
		if op == "child-success" {
			dir, err := os.Open(child.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if err := dir.Sync(); err != nil {
				dir.Close()
				t.Fatal(err)
			}
			if err := dir.Close(); err != nil {
				t.Fatal(err)
			}
			fmt.Println("committed")
			select {}
		}
	default:
		t.Fatalf("unknown durability operation %q", op)
	}
}

func parkSync(t *testing.T, target string) {
	atomicfs.SyncDirFunc = func(dir string) error {
		if dir == target {
			fmt.Println("parked")
			select {}
		}
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })
}

func verifyDurabilityProcessState(t *testing.T, op, root, projectID string) {
	t.Helper()
	sessionsRoot := filepath.Join(root, projectID, "sessions")
	infos, err := List(sessionsRoot, "", "")
	if err != nil {
		t.Fatal(err)
	}
	switch op {
	case "delete":
		if len(infos) != 0 {
			t.Fatalf("deleted sessions listed after rename-before-sync kill: %+v", infos)
		}
		entries, err := os.ReadDir(filepath.Join(root, projectID, ".deleting", "sessions"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("delete residue = %v err=%v, want one renamed destination", entries, err)
		}
		deletedID := entries[0].Name()
		for i := 0; i < len(deletedID); i++ {
			if deletedID[i] == '-' {
				deletedID = deletedID[:i]
				break
			}
		}
		claim, ok, err := AcquireSessionClaim(root, projectID, deletedID)
		if err != nil || !ok {
			t.Fatalf("delete claim namespace: ok=%v err=%v", ok, err)
		}
		_ = claim.Release()
	case "fork":
		sourceData, err := os.ReadFile(filepath.Join(root, projectID, ".durability-fork-source"))
		if err != nil {
			t.Fatal(err)
		}
		sourceID := strings.TrimSpace(string(sourceData))
		if sourceID == "" {
			t.Fatal("fork source marker is empty")
		}
		if len(infos) != 2 {
			t.Fatalf("fork sessions after rename-before-sync kill = %+v, want source and destination", infos)
		}
		var destination *SessionInfo
		sourceFound := false
		for i := range infos {
			info := infos[i]
			store, err := NewForSessionsRoot(sessionsRoot, root, projectID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.LoadSession(info.ID); err != nil {
				t.Fatalf("fresh load %s: %v", info.ID, err)
			}
			turns, err := store.LoadCompleteTurnsReadOnly()
			if err != nil {
				t.Fatalf("fresh turns %s: %v", info.ID, err)
			}
			store.Detach()
			claim, ok, err := AcquireSessionClaim(root, projectID, info.ID)
			if err != nil || !ok {
				t.Fatalf("claim %s after fork reap: ok=%v err=%v", info.ID, ok, err)
			}
			_ = claim.Release()
			if info.ID == sourceID {
				sourceFound = true
			} else {
				if len(turns) != 1 || len(turns[0].Messages) != 1 || !strings.Contains(string(turns[0].Messages[0]), "fork") {
					t.Fatalf("fork destination %s turns = %+v, want copied source turn", info.ID, turns)
				}
				destination = &info
			}
		}
		if !sourceFound || destination == nil {
			t.Fatalf("fork source/destination after reap: sourceFound=%v destination=%v infos=%+v", sourceFound, destination != nil, infos)
		}
	case "child":
		verifyChildState(t, root, projectID)
	case "staged":
		if len(infos) != 1 {
			t.Fatalf("staged sessions after rename-before-sync kill = %+v, want one destination", infos)
		}
		info := infos[0]
		store, err := NewForSessionsRoot(sessionsRoot, root, projectID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.LoadSession(info.ID); err != nil {
			t.Fatalf("fresh load staged destination %s: %v", info.ID, err)
		}
		store.Detach()
		claim, ok, err := AcquireSessionClaim(root, projectID, info.ID)
		if err != nil || !ok {
			t.Fatalf("claim staged destination %s after reap: ok=%v err=%v", info.ID, ok, err)
		}
		_ = claim.Release()
	default:
		if len(infos) == 0 {
			t.Fatalf("%s published no visible session after rename-before-sync kill", op)
		}
		for _, info := range infos {
			store, err := NewForSessionsRoot(sessionsRoot, root, projectID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.LoadSession(info.ID); err != nil {
				t.Fatalf("fresh load %s: %v", info.ID, err)
			}
			store.Detach()
			claim, ok, err := AcquireSessionClaim(root, projectID, info.ID)
			if err != nil || !ok {
				t.Fatalf("claim %s after child reap: ok=%v err=%v", info.ID, ok, err)
			}
			_ = claim.Release()
		}
	}
}

func verifyChildState(t *testing.T, root, projectID string) {
	t.Helper()
	sessionsRoot := filepath.Join(root, projectID, "sessions")
	infos, err := List(sessionsRoot, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("child success sessions = %+v, want parent and child", infos)
	}
	var child *SessionInfo
	var parentID string
	for i := range infos {
		if infos[i].ParentSessionID != "" {
			child = &infos[i]
		} else if parentID == "" {
			parentID = infos[i].ID
		} else {
			t.Fatalf("multiple parent sessions in child durability row: %+v", infos)
		}
	}
	if child == nil || parentID == "" || child.ParentSessionID != parentID {
		t.Fatalf("child parent/destination relationship = parent %q child=%+v, want one linked child", parentID, child)
	}
	for _, info := range infos {
		store, err := NewForSessionsRoot(sessionsRoot, root, projectID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.LoadSession(info.ID); err != nil {
			t.Fatalf("fresh session load %s: %v", info.ID, err)
		}
		store.Detach()
		claim, ok, err := AcquireSessionClaim(root, projectID, info.ID)
		if err != nil || !ok {
			t.Fatalf("session claim %s after reap: ok=%v err=%v", info.ID, ok, err)
		}
		_ = claim.Release()
	}
	if child.ID == "" {
		t.Fatal("child success session has empty id")
	}
}
