package agent

import "github.com/MMinasyan/lightcode/internal/permission"

// EventKind identifies the type of agent event.
type EventKind int

const (
	EventTextDelta         EventKind = iota // Streamed text chunk from the model.
	EventToolCallStart                      // Tool call begins.
	EventToolCallEnd                        // Tool call completes.
	EventUsage                              // Token usage report from the model.
	EventTurnStart                          // Agent starts processing a turn.
	EventTurnEnd                            // Agent finished processing a turn.
	EventError                              // The agentic loop returned an error.
	EventPermissionRequest                  // A tool needs user approval.
	EventCompactionStart                    // Compaction beginning.
	EventCompactionEnd                      // Compaction finished.
	EventWarning                            // Prompt assembly warnings.
	EventSubagentStart                      // A subagent session started.
)

// Event is the unified event type emitted by the Agent to adapters.
type Event struct {
	Kind EventKind

	// Loop-level fields (forwarded from loop.Event):
	ToolName   string
	ToolCallID string
	Args       string
	Result     string
	IsError    bool
	Model      string
	Cache      int
	Input      int
	Output     int
	UsageKnown bool

	// Agent-level fields:
	Turn               int
	Cancelled          bool
	Error              string
	PermReq            *PermissionRequest
	Warnings           []PromptWarning
	SubagentSessionID  string
	TaskIndex          int
}

// PromptWarning is a warning from the prompt assembly system.
type PromptWarning struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// PermissionRequest is sent to adapters when a tool needs user approval.
type PermissionRequest struct {
	ID       string
	ToolName string
	Arg      string
}

// TokenEntry holds accumulated token counts for one {provider, model} pair.
type TokenEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Cache    int    `json:"cache"`
	Input    int    `json:"input"`
	Output   int    `json:"output"`
	Known    bool   `json:"known"`
}

// TokenReport is the cumulative token usage for a session.
type TokenReport struct {
	Total         TokenEntry   `json:"total"`
	PerModel      []TokenEntry `json:"perModel"`
	ContextUsed   int          `json:"contextUsed"`
	ContextWindow int          `json:"contextWindow"`
}

// ProviderModels lists the models available from one provider.
type ProviderModels struct {
	Provider string   `json:"provider"`
	Models   []string `json:"models"`
}

// Snapshot describes one turn's snapshots.
type Snapshot struct {
	Turn  int            `json:"turn"`
	Files []SnapshotFile `json:"files"`
}

// SnapshotFile describes one file within a snapshot turn.
type SnapshotFile struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
}

// SessionSummary is the payload for session queries.
type SessionSummary struct {
	ID           string `json:"id"`
	CreatedAt    string `json:"createdAt"`
	LastActivity int64  `json:"lastActivity"`
	State        string `json:"state"`
	ArchivedAt   int64  `json:"archivedAt"`
	ProjectPath  string `json:"projectPath"`
}

// DisplayMessage is the pre-assembled, display-ready message returned by
// SessionMessages. Type is the discriminator: "user", "assistant", "tool", "system".
type DisplayMessage struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Turn    int    `json:"turn,omitempty"`

	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Args    string `json:"args,omitempty"`
	Done    bool   `json:"done,omitempty"`
	Success bool   `json:"success,omitempty"`
	Result  string `json:"result,omitempty"`
}

// ModelInfo holds the active provider and model.
type ModelInfo struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ProjectSummary is the payload for project queries.
type ProjectSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	CreatedAt    string `json:"createdAt"`
	LastActivity int64  `json:"lastActivity"`
}

// PermissionSuggestion re-exports permission.Suggestion so adapters
// don't need to import the permission package.
type PermissionSuggestion = permission.Suggestion
