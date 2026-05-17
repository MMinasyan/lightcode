package tool

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSaveMemoryRequiresTitleAndContent(t *testing.T) {
	tool := NewSaveMemory(&mockMemorySaver{}, "/memories")
	tests := []map[string]any{
		{},
		{"title": "title"},
		{"content": "content"},
		{"title": "", "content": "content"},
	}

	for _, params := range tests {
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute(%v) error = %v, want nil", params, err)
		}
		if result != "error: title and content are required" {
			t.Fatalf("Execute(%v) result = %q, want required-field error", params, result)
		}
	}
}

func TestSaveMemoryWritesThroughToStore(t *testing.T) {
	store := &mockMemorySaver{path: "/memories/2026-title.md"}
	tool := NewSaveMemory(store, "/memories")

	result, err := tool.Execute(context.Background(), map[string]any{
		"title":   "Useful detail",
		"content": "Remember this for later.",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "Memory saved: Useful detail (/memories/2026-title.md)" {
		t.Fatalf("Execute result = %q, want saved-memory message", result)
	}
	if store.memoriesDir != "/memories" || store.title != "Useful detail" || store.content != "Remember this for later." {
		t.Fatalf("store call = (%q, %q, %q), want memories dir/title/content", store.memoriesDir, store.title, store.content)
	}
}

func TestSaveMemoryReturnsStoreErrorAsResultString(t *testing.T) {
	store := &mockMemorySaver{err: errors.New("embed failed")}
	tool := NewSaveMemory(store, "/memories")

	result, err := tool.Execute(context.Background(), map[string]any{
		"title":   "Useful detail",
		"content": "Remember this for later.",
	})
	if err != nil {
		t.Fatalf("Execute error = %v, want nil", err)
	}
	if result != "error: embed failed" {
		t.Fatalf("Execute result = %q, want store error string", result)
	}
}

func TestSaveMemorySchemaShape(t *testing.T) {
	tool := NewSaveMemory(&mockMemorySaver{}, "/memories")
	if tool.Name() != "save_memory" {
		t.Fatalf("Name = %q, want save_memory", tool.Name())
	}
	if !strings.Contains(tool.Description(), "cross-session") {
		t.Fatalf("Description = %q, want cross-session guidance", tool.Description())
	}
	schema := tool.ParametersSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v, want map", schema["properties"])
	}
	if _, ok := props["title"]; !ok {
		t.Fatalf("schema properties = %#v, want title", props)
	}
	if _, ok := props["content"]; !ok {
		t.Fatalf("schema properties = %#v, want content", props)
	}
	if required, ok := schema["required"].([]string); !ok || !reflect.DeepEqual(required, []string{"title", "content"}) {
		t.Fatalf("schema required = %#v, want title/content", schema["required"])
	}
}

type mockMemorySaver struct {
	path        string
	err         error
	memoriesDir string
	title       string
	content     string
}

func (m *mockMemorySaver) SaveMemory(memoriesDir, title, content string) (string, error) {
	m.memoriesDir = memoriesDir
	m.title = title
	m.content = content
	return m.path, m.err
}
