package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestPermissionSaveSerializesStaleReaders proves the permissions
// read-merge-write cycle serializes: several contenders each add a distinct
// rule concurrently and none is lost, and the published file is always
// complete JSON.
func TestPermissionSaveSerializesStaleReaders(t *testing.T) {
	root := t.TempDir()
	projectID := "p-serialize"

	const contenders = 4
	var wg sync.WaitGroup
	wg.Add(contenders)
	for i := 0; i < contenders; i++ {
		go func(i int) {
			defer wg.Done()
			rule := fmt.Sprintf("Bash(cmd-%d)", i)
			if err := SaveLocal(root, projectID, Rules{Allow: []string{rule}}); err != nil {
				t.Errorf("SaveLocal %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, err := LoadLocal(root, projectID)
	if err != nil {
		t.Fatalf("LoadLocal: %v", err)
	}
	if len(got.Allow) != contenders {
		t.Fatalf("Allow = %v, want %d distinct rules (lost update)", got.Allow, contenders)
	}

	// The published file is complete, parseable JSON.
	raw, err := os.ReadFile(localPath(root, projectID))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var rules Rules
	if err := json.Unmarshal(raw, &rules); err != nil {
		t.Fatalf("published permissions not complete JSON: %v", err)
	}

	// No temp artifact remains beside the record.
	entries, _ := os.ReadDir(filepath.Join(root, projectID))
	for _, e := range entries {
		if e.Name() != "permissions.json" && e.Name() != ".locks" {
			t.Fatalf("leftover artifact %q", e.Name())
		}
	}
}
