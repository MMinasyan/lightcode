package agents

import "fmt"

const (
	SystemPromptNone   = "none"
	SystemPromptSimple = "simple"
	SystemPromptFull   = "full"
)

// StandardTools is the base registry-membership set inherited from primary.
var StandardTools = []string{
	"read_file",
	"write_file",
	"edit_file",
	"apply_patch",
	"run_command",
	"execute_pending",
	"process",
	"sleep",
	"task",
}

// Definition is the on-disk agent type shape. Pointer fields distinguish
// omitted fields from explicit zero values for inheritance.
type Definition struct {
	Model        string    `json:"model,omitempty"`
	SystemPrompt string    `json:"system_prompt,omitempty"`
	Prompt       *string   `json:"prompt,omitempty"`
	Tools        *[]string `json:"tools,omitempty"`
	LSP          *bool     `json:"lsp,omitempty"`
	Readonly     *bool     `json:"readonly,omitempty"`
	WriteDir     *string   `json:"write_dir,omitempty"`
	Description  *string   `json:"description,omitempty"`
	Subagent     *bool     `json:"subagent,omitempty"`
}

// Resolved is the concrete agent type used by runtime construction.
type Resolved struct {
	Name         string
	Model        string
	SystemPrompt string
	Prompt       string
	Tools        []string
	LSP          bool
	Readonly     bool
	WriteDir     string
	Description  string
	Subagent     bool
	Builtin      bool
}

// Warning describes a non-fatal agent config issue.
type Warning struct {
	Kind    string
	Name    string
	Message string
}

func (w Warning) Error() string {
	if w.Name == "" {
		return w.Message
	}
	return fmt.Sprintf("%s: %s", w.Name, w.Message)
}

func boolPtr(v bool) *bool { return &v }

func stringPtr(v string) *string { return &v }

func toolsPtr(v []string) *[]string {
	cp := append([]string(nil), v...)
	return &cp
}
