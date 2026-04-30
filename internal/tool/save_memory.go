package tool

import (
	"context"
	"fmt"

	"github.com/MMinasyan/lightcode/internal/memory"
)

type SaveMemory struct {
	store       *memory.Store
	memoriesDir string
}

func NewSaveMemory(store *memory.Store, memoriesDir string) *SaveMemory {
	return &SaveMemory{store: store, memoriesDir: memoriesDir}
}

func (t *SaveMemory) Name() string { return "save_memory" }

func (t *SaveMemory) Description() string {
	return "Save a memory for cross-session access. Use when you encounter information that would be valuable in future sessions."
}

func (t *SaveMemory) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":   map[string]any{"type": "string", "description": "One sentence summary of the memory"},
			"content": map[string]any{"type": "string", "description": "The memory content"},
		},
		"required": []string{"title", "content"},
	}
}

func (t *SaveMemory) Execute(_ context.Context, params map[string]any) (string, error) {
	title, _ := params["title"].(string)
	content, _ := params["content"].(string)
	if title == "" || content == "" {
		return "error: title and content are required", nil
	}
	fp, err := t.store.SaveMemory(t.memoriesDir, title, content)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return fmt.Sprintf("Memory saved: %s (%s)", title, fp), nil
}
