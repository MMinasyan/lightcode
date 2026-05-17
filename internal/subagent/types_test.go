package subagent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	fm, body := parseFrontmatter("---\nname: explore\ndescription: Explore\ntools:\n  - read_file\n  - run_command\n---\nBody\n")

	if fm["name"] != "explore" || fm["description"] != "Explore" {
		t.Fatalf("frontmatter = %#v, want scalar fields", fm)
	}
	if !reflect.DeepEqual(fm["tools"], []string{"read_file", "run_command"}) {
		t.Fatalf("tools = %#v, want read_file/run_command", fm["tools"])
	}
	if body != "\nBody\n" {
		t.Fatalf("body = %q, want body after delimiter", body)
	}
}

func TestParseFrontmatterWithoutClosedBlockReturnsOriginalBody(t *testing.T) {
	inputs := []string{"plain body", "---\nname: missing-close\nbody"}
	for _, input := range inputs {
		fm, body := parseFrontmatter(input)
		if fm != nil {
			t.Fatalf("parseFrontmatter(%q) fm = %#v, want nil", input, fm)
		}
		if body != input {
			t.Fatalf("parseFrontmatter(%q) body = %q, want original", input, body)
		}
	}
}

func TestParseAgentFileRequiresNameAndParsesTools(t *testing.T) {
	got, err := parseAgentFile("---\nname: worker\ndescription: Worker\ntools:\n  - read_file\n---\nDo work\n")
	if err != nil {
		t.Fatalf("parseAgentFile valid: %v", err)
	}
	want := AgentType{Name: "worker", Description: "Worker", Tools: []string{"read_file"}, Prompt: "Do work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentType = %+v, want %+v", got, want)
	}

	if _, err := parseAgentFile("---\ndescription: missing name\n---\nBody"); err == nil {
		t.Fatal("parseAgentFile missing name error = nil, want error")
	}
}

func TestLoaderPrecedenceAndUnknownType(t *testing.T) {
	projectRoot := t.TempDir()
	home := t.TempDir()
	writeAgentFile(t, filepath.Join(home, ".lightcode", "agents"), "custom", "home custom", "home prompt")
	writeAgentFile(t, filepath.Join(projectRoot, ".lightcode", "agents"), "custom", "project custom", "project prompt")

	loader := NewLoader(projectRoot, home)
	got, err := loader.Load("custom")
	if err != nil {
		t.Fatalf("Load(custom): %v", err)
	}
	if got.Description != "project custom" || got.Prompt != "project prompt" {
		t.Fatalf("Load(custom) = %+v, want project override", got)
	}
	if _, err := loader.Load("missing"); err == nil {
		t.Fatal("Load(missing) error = nil, want error")
	}
}

func TestLoaderAllDeduplicatesProjectHomeAndBuiltin(t *testing.T) {
	projectRoot := t.TempDir()
	home := t.TempDir()
	writeAgentFile(t, filepath.Join(home, ".lightcode", "agents"), "custom", "home custom", "home prompt")
	writeAgentFile(t, filepath.Join(projectRoot, ".lightcode", "agents"), "custom", "project custom", "project prompt")

	all := NewLoader(projectRoot, home).All()
	seen := map[string]int{}
	for _, at := range all {
		seen[at.Name]++
	}
	if seen["custom"] != 1 {
		t.Fatalf("custom count = %d, want deduplicated once", seen["custom"])
	}
	if seen["explore"] != 1 {
		t.Fatalf("explore count = %d, want builtin once", seen["explore"])
	}
}

func writeAgentFile(t *testing.T, dir, name, desc, prompt string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\ntools:\n  - read_file\n---\n" + prompt + "\n"
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
