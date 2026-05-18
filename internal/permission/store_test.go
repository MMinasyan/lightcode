package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLocalHappyPath(t *testing.T) {
	root := t.TempDir()
	pid := "proj-1"

	// SaveLocal doesn't create directories — caller must ensure they exist.
	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}

	rules := Rules{
		Allow: []string{"write_file(/src/**)"},
		Deny:  []string{"read_file(//etc/passwd)"},
	}
	if err := SaveLocal(root, pid, rules); err != nil {
		t.Fatal("SaveLocal:", err)
	}

	path := filepath.Join(root, pid, "permissions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	var got Rules
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal("Unmarshal:", err)
	}
	if len(got.Allow) != 1 || got.Allow[0] != "write_file(/src/**)" {
		t.Fatalf("Allow = %v, want [write_file(/src/**)]", got.Allow)
	}
	if len(got.Deny) != 1 || got.Deny[0] != "read_file(//etc/passwd)" {
		t.Fatalf("Deny = %v, want [read_file(//etc/passwd)]", got.Deny)
	}
}

func TestLoadLocalHappyPath(t *testing.T) {
	root := t.TempDir()
	pid := "proj-2"

	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a known file
	permissions := Rules{
		Allow: []string{"write_file(/src/main.go)"},
		Ask:   []string{"run_command(npm run *)"},
	}
	if err := SaveLocal(root, pid, permissions); err != nil {
		t.Fatal("SaveLocal:", err)
	}

	got, err := LoadLocal(root, pid)
	if err != nil {
		t.Fatal("LoadLocal:", err)
	}
	if len(got.Allow) != 1 || got.Allow[0] != "write_file(/src/main.go)" {
		t.Fatalf("Allow = %v", got.Allow)
	}
	if len(got.Ask) != 1 || got.Ask[0] != "run_command(npm run *)" {
		t.Fatalf("Ask = %v", got.Ask)
	}
}

func TestLoadLocalMissingFile(t *testing.T) {
	root := t.TempDir()
	got, err := LoadLocal(root, "nonexistent")
	if err != nil {
		t.Fatal("LoadLocal should not error for missing file:", err)
	}
	if len(got.Allow) != 0 || len(got.Deny) != 0 || len(got.Ask) != 0 {
		t.Fatalf("expected empty rules, got %+v", got)
	}
}

func TestLoadLocalEmptyArgs(t *testing.T) {
	got, err := LoadLocal("", "pid")
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow != nil || got.Deny != nil || got.Ask != nil {
		t.Fatalf("expected nil slices, got %+v", got)
	}

	got, err = LoadLocal("root", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow != nil {
		t.Fatalf("expected nil Allow, got %v", got.Allow)
	}
}

func TestProjectIDScoping(t *testing.T) {
	root := t.TempDir()

	for _, pid := range []string{"projA", "projB"} {
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Write rules for project A
	if err := SaveLocal(root, "projA", Rules{
		Allow: []string{"write_file(/src/**)"},
	}); err != nil {
		t.Fatal(err)
	}

	// Write different rules for project B
	if err := SaveLocal(root, "projB", Rules{
		Deny: []string{"read_file(/secret/**)"},
	}); err != nil {
		t.Fatal(err)
	}

	rulesA, err := LoadLocal(root, "projA")
	if err != nil {
		t.Fatal(err)
	}
	rulesB, err := LoadLocal(root, "projB")
	if err != nil {
		t.Fatal(err)
	}

	if len(rulesA.Allow) != 1 {
		t.Fatalf("projA Allow = %v, expected 1 entry", rulesA.Allow)
	}
	if len(rulesA.Deny) != 0 {
		t.Fatalf("projA Deny = %v, expected 0", rulesA.Deny)
	}
	if len(rulesB.Deny) != 1 {
		t.Fatalf("projB Deny = %v, expected 1 entry", rulesB.Deny)
	}
	if len(rulesB.Allow) != 0 {
		t.Fatalf("projB Allow = %v, expected 0", rulesB.Allow)
	}
}

func TestSaveLocalMergesExistingRules(t *testing.T) {
	root := t.TempDir()
	pid := "proj-merge"

	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}

	// First save
	if err := SaveLocal(root, pid, Rules{
		Allow: []string{"write_file(/src/a.go)"},
		Deny:  []string{"read_file(/secret/**)"},
	}); err != nil {
		t.Fatal(err)
	}

	// Second save with overlapping + new entries
	if err := SaveLocal(root, pid, Rules{
		Allow: []string{"write_file(/src/a.go)", "write_file(/src/b.go)"},
		Deny:  []string{"read_file(/secret/**)", "read_file(//etc/shadow)"},
		Ask:   []string{"run_command(make *)"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := LoadLocal(root, pid)
	if err != nil {
		t.Fatal(err)
	}

	// Allow should have 2 (no duplicates)
	if len(got.Allow) != 2 {
		t.Fatalf("Allow = %v, expected 2 entries (no duplicates)", got.Allow)
	}
	// Deny should have 2
	if len(got.Deny) != 2 {
		t.Fatalf("Deny = %v, expected 2 entries", got.Deny)
	}
	// Ask should have 1
	if len(got.Ask) != 1 {
		t.Fatalf("Ask = %v, expected 1 entry", got.Ask)
	}
}

func TestSaveLocalMergePreservesOrder(t *testing.T) {
	root := t.TempDir()
	pid := "proj-order"

	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}

	// First save
	if err := SaveLocal(root, pid, Rules{
		Allow: []string{"a", "b"},
	}); err != nil {
		t.Fatal(err)
	}

	// Second save with b (existing) + c (new) + a (existing)
	if err := SaveLocal(root, pid, Rules{
		Allow: []string{"b", "c", "a"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := LoadLocal(root, pid)
	if err != nil {
		t.Fatal(err)
	}
	// mergeUnique preserves existing order and appends new items.
	// existing=[a,b], add=[b,c,a] → result=[a,b,c] (b deduped, c new, a deduped)
	if len(got.Allow) != 3 || got.Allow[0] != "a" || got.Allow[1] != "b" || got.Allow[2] != "c" {
		t.Fatalf("Allow = %v, want [a b c]", got.Allow)
	}
}

func TestLoadLocalMalformedFile(t *testing.T) {
	root := t.TempDir()
	pid := "proj-bad"
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "permissions.json"), []byte("NOT JSON"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadLocal(root, pid)
	if err == nil {
		t.Fatal("LoadLocal should return error for malformed file")
	}
}

func TestLoadLocalProjectIDScoping(t *testing.T) {
	root := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "projX"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Save to projX
	if err := SaveLocal(root, "projX", Rules{
		Allow: []string{"write_file(/x.go)"},
	}); err != nil {
		t.Fatal(err)
	}

	// projY should be empty (no file exists)
	got, err := LoadLocal(root, "projY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Allow) != 0 {
		t.Fatalf("projY Allow = %v, want empty", got.Allow)
	}
}
