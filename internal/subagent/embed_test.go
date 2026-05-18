package subagent

import "testing"

func TestBuiltinExploreLoads(t *testing.T) {
	loader := NewLoader(t.TempDir(), t.TempDir())

	got, err := loader.Load("explore")
	if err != nil {
		t.Fatalf("Load(explore): %v", err)
	}
	if got.Name != "explore" {
		t.Fatalf("Name = %q, want explore", got.Name)
	}
	if got.Description == "" {
		t.Fatal("Description is empty")
	}
	if got.Prompt == "" {
		t.Fatal("Prompt is empty")
	}
	if len(got.Tools) == 0 {
		t.Fatal("Tools is empty")
	}
}

func TestLoaderAllIncludesBuiltinExplore(t *testing.T) {
	loader := NewLoader(t.TempDir(), t.TempDir())

	for _, got := range loader.All() {
		if got.Name == "explore" {
			if got.Description == "" || got.Prompt == "" || len(got.Tools) == 0 {
				t.Fatalf("builtin explore = %+v, want non-empty description/prompt/tools", got)
			}
			return
		}
	}
	t.Fatal("All() did not include builtin explore")
}
