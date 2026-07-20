package agent

import (
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// EventKind identifies the type of agent event.
type EventKind int

const (
	EventTextDelta                 EventKind = iota // Streamed text chunk from the model.
	EventToolCallStart                              // Tool call begins.
	EventToolCallEnd                                // Tool call completes.
	EventUsage                                      // Token usage report from the model.
	EventTurnStart                                  // Agent starts processing a turn.
	EventTurnEnd                                    // Agent finished processing a turn.
	EventError                                      // The agentic loop returned an error.
	EventPermissionRequest                          // A tool needs user approval.
	EventCompactionStart                            // Compaction beginning.
	EventCompactionEnd                              // Compaction finished.
	EventWarning                                    // Current warning snapshot changed.
	EventSubagentStart                              // A subagent session started.
	EventBackgroundProcessComplete                  // A background process completion was delivered to the model.
	EventUserMessageDisplay                         // A user-role message was appended to history.
	EventGenericSystemSignal                        // A non-background <system-signal> was appended to history.
	EventQueueChanged                               // The backend-owned input queue changed; carries a versioned snapshot.
	EventSessionRewrite                             // Compaction rewrote the durable prefix; carries the replacement transcript state.
)

// Event is the unified event type emitted by the Agent to adapters.
type Event struct {
	Kind            EventKind
	SessionID       string
	ProjectID       string
	ParentSessionID string

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
	Metadata   map[string]any

	// Agent-level fields:
	Turn              int
	Cancelled         bool
	Error             string
	// RewritePayload (EventSessionRewrite) is the compacted session's replacement
	// transcript/token snapshot, built by the producer under the transcript lock, so
	// the adapter callback applies it without re-entering the owner.
	RewritePayload    *SessionPayload
	PermReq           *PermissionRequest
	Warnings          []PromptWarning
	SubagentSessionID string
	TaskIndex         int
	BackgroundProcess *BackgroundProcessDisplay

	// CumulativeTokens (EventUsage) is the session's absolute cumulative token
	// report computed under tokensMu, so a consumer applies it as a replacement
	// rather than querying the owner. The Cache/Input/Output fields above remain
	// this event's delta.
	CumulativeTokens *TokenReport

	// Queue fields (EventQueueChanged): a versioned snapshot of the input queue.
	Queue        []QueuedItem
	QueueVersion int

	// Seq is the transcript sequence the coordinator assigned to this event's
	// display row, set before delivery so an adapter can gate the item against a
	// capture high-water. Zero for events that produce no row.
	Seq int
}

// BackgroundProcessDisplay is the adapter-facing display payload for a
// background process completion delivered as model input.
type BackgroundProcessDisplay struct {
	ID       string `json:"id"`
	Command  string `json:"command"`
	Reason   string `json:"reason"`
	ExitCode int    `json:"exitCode"`
	Output   string `json:"output"`
}

// PromptWarning is a user-visible warning from prompt, catalog, LSP, or related systems.
type PromptWarning struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// PermissionRequest is sent to adapters when a tool needs user approval.
type PermissionRequest struct {
	ID                 string
	SessionID          string
	ProjectID          string
	ToolName           string
	Arg                string
	ResolvedArg        string
	CanAllowAll        bool
	DisableProjectSave bool
	BatchIndex         int
	BatchTotal         int
	BatchFiles         []string
	BatchResolvedFiles []string
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

// ModelListEntry is one flat picker/list entry from the catalog.
type ModelListEntry struct {
	Ref             string        `json:"ref"`
	Provider        string        `json:"provider"`
	ProviderName    string        `json:"providerName"`
	Model           string        `json:"model"`
	DisplayName     string        `json:"displayName"`
	ContextWindow   int           `json:"contextWindow"`
	MaxOutputTokens int           `json:"maxOutputTokens"`
	Cost            *catalog.Cost `json:"cost,omitempty"`
	Hidden          bool          `json:"hidden"`
	ProviderHidden  bool          `json:"providerHidden"`
	Incomplete      bool          `json:"incomplete"`
	Default         bool          `json:"default"`
	// Source is the model's provenance: "bundled", "discovered", or "user".
	// Only "user" models can be truly deleted from config; the others can be
	// hidden or have individual field overrides reset.
	Source string `json:"source"`
}

// ProviderConfigInput is the adapter-neutral payload for editing an existing
// provider's config (transport + provider-level fields). It carries only set
// fields; absent fields are left untouched. The API key value is never carried
// here — secrets flow through ConnectProvider, write-only.
type ProviderConfigInput struct {
	Name             string                    `json:"name"`
	BaseURL          string                    `json:"baseURL"`
	APIKeyEnv        string                    `json:"apiKeyEnv"`
	Headers          map[string]string         `json:"headers,omitempty"`
	Options          map[string]any            `json:"options,omitempty"`
	SystemRole       catalog.SystemRole        `json:"systemRole,omitempty"`
	UsageInStream    *bool                     `json:"usageInStream,omitempty"`
	MaxTokensField   string                    `json:"maxTokensField,omitempty"`
	ExtraBody        map[string]any            `json:"extraBody,omitempty"`
	Discovery        *bool                     `json:"discovery,omitempty"`
	ProtocolMetadata *catalog.ProtocolMetadata `json:"protocolMetadata,omitempty"`
}

// ModelConfigInput is the adapter-neutral payload for adding or editing one
// model's fields under a provider. Only set fields are written; absent fields
// are left untouched (use ResetModelField to drop an override).
type ModelConfigInput struct {
	Name             string                    `json:"name"`
	ContextWindow    int                       `json:"contextWindow"`
	MaxOutputTokens  int                       `json:"maxOutputTokens"`
	InputModalities  []catalog.Modality        `json:"inputModalities,omitempty"`
	SystemRole       catalog.SystemRole        `json:"systemRole,omitempty"`
	UsageInStream    *bool                     `json:"usageInStream,omitempty"`
	ExtraBody        map[string]any            `json:"extraBody,omitempty"`
	Cost             *catalog.Cost             `json:"cost,omitempty"`
	ProtocolMetadata *catalog.ProtocolMetadata `json:"protocolMetadata,omitempty"`
}

// ProviderConfigView is the merged, effective config of one provider for the
// config editor. Values reflect bundled+user+discovery; edits write overrides.
type ProviderConfigView struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	Builtin          bool                      `json:"builtin"`
	Connected        bool                      `json:"connected"`
	BaseURL          string                    `json:"baseURL"`
	APIKeyEnv        string                    `json:"apiKeyEnv"`
	GeneratedKeyEnv  string                    `json:"generatedKeyEnv"`
	Headers          map[string]string         `json:"headers"`
	UserHeaders      map[string]string         `json:"userHeaders"`
	Options          map[string]any            `json:"options"`
	SystemRole       string                    `json:"systemRole"`
	UsageInStream    bool                      `json:"usageInStream"`
	MaxTokensField   string                    `json:"maxTokensField"`
	ExtraBody        map[string]any            `json:"extraBody"`
	Discovery        bool                      `json:"discovery"`
	ProtocolMetadata *catalog.ProtocolMetadata `json:"protocolMetadata"`
	Models           []ModelConfigView         `json:"models"`
}

// ModelConfigView is the merged, effective config of one model for the editor.
type ModelConfigView struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	ContextWindow    int                       `json:"contextWindow"`
	MaxOutputTokens  int                       `json:"maxOutputTokens"`
	InputModalities  []catalog.Modality        `json:"inputModalities"`
	SystemRole       string                    `json:"systemRole"`
	UsageInStream    bool                      `json:"usageInStream"`
	ExtraBody        map[string]any            `json:"extraBody"`
	Cost             *catalog.Cost             `json:"cost"`
	ProtocolMetadata *catalog.ProtocolMetadata `json:"protocolMetadata"`
	Hidden           bool                      `json:"hidden"`
	Source           string                    `json:"source"`
}

type RuntimeConfigSettings struct {
	Sessions   RuntimeSessionsConfig   `json:"sessions"`
	Compaction RuntimeCompactionConfig `json:"compaction"`
	Subagents  RuntimeSubagentsConfig  `json:"subagents"`
	Tools      RuntimeToolsConfig      `json:"tools"`
}

type RuntimeSessionsConfig struct {
	ArchiveAfterDays       int `json:"archive_after_days"`
	DeleteAfterArchiveDays int `json:"delete_after_archive_days"`
}

type RuntimeCompactionConfig struct {
	ThresholdPct float64 `json:"threshold_pct"`
}

type RuntimeSubagentsConfig struct {
	MaxConcurrent int `json:"max_concurrent"`
}

type RuntimeToolsConfig struct {
	MaxOutputBytes         int `json:"max_output_bytes"`
	ReadMaxLines           int `json:"read_max_lines"`
	ReadLineMaxChars       int `json:"read_line_max_chars"`
	CommandTimeout         int `json:"command_timeout"`
	MaxBackgroundProcesses int `json:"max_background_processes"`
}

// Key-source classes re-exported from internal/config, which owns the
// classifier.
const (
	ProviderKeySourceNone     = config.KeySourceNone
	ProviderKeySourceKeyless  = config.KeySourceKeyless
	ProviderKeySourceManaged  = config.KeySourceManaged
	ProviderKeySourceExternal = config.KeySourceExternal
)

// ProviderStatus describes a provider's setup state for adapter-neutral settings surfaces.
type ProviderStatus struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Builtin         bool   `json:"builtin"`
	Connected       bool   `json:"connected"`
	KeySource       string `json:"keySource"`
	Disconnectable  bool   `json:"disconnectable"`
	Removable       bool   `json:"removable"`
	APIKeyEnv       string `json:"apiKeyEnv"`
	GeneratedKeyEnv string `json:"generatedKeyEnv,omitempty"`
	BaseURL         string `json:"baseURL"`
	Discovery       bool   `json:"discovery"`
	ModelCount      int    `json:"modelCount"`
	UsableModels    int    `json:"usableModels"`
}

// DiscoveryModelCandidate is model metadata returned by connect-time discovery.
type DiscoveryModelCandidate struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	ContextWindow   int           `json:"contextWindow"`
	MaxOutputTokens int           `json:"maxOutputTokens"`
	Cost            *catalog.Cost `json:"cost,omitempty"`
	Usable          bool          `json:"usable"`
}

// CustomProviderModelInput is one model selected by the user for a custom provider.
type CustomProviderModelInput struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	ContextWindow    int                       `json:"contextWindow"`
	MaxOutputTokens  int                       `json:"maxOutputTokens"`
	InputModalities  []catalog.Modality        `json:"inputModalities,omitempty"`
	SystemRole       catalog.SystemRole        `json:"systemRole,omitempty"`
	UsageInStream    *bool                     `json:"usageInStream,omitempty"`
	ExtraBody        map[string]any            `json:"extraBody,omitempty"`
	Cost             *catalog.Cost             `json:"cost,omitempty"`
	ProtocolMetadata *catalog.ProtocolMetadata `json:"protocolMetadata,omitempty"`
	Hidden           bool                      `json:"hidden,omitempty"`
}

// CustomProviderRequest is the adapter-neutral payload for adding a custom provider.
type CustomProviderRequest struct {
	ID               string                     `json:"id"`
	Name             string                     `json:"name"`
	BaseURL          string                     `json:"baseURL"`
	APIKeyEnv        string                     `json:"apiKeyEnv"`
	APIKey           string                     `json:"apiKey"`
	Headers          map[string]string          `json:"headers,omitempty"`
	Options          map[string]any             `json:"options,omitempty"`
	SystemRole       catalog.SystemRole         `json:"systemRole,omitempty"`
	UsageInStream    *bool                      `json:"usageInStream,omitempty"`
	MaxTokensField   string                     `json:"maxTokensField,omitempty"`
	ExtraBody        map[string]any             `json:"extraBody,omitempty"`
	ProtocolMetadata *catalog.ProtocolMetadata  `json:"protocolMetadata,omitempty"`
	Hidden           bool                       `json:"hidden,omitempty"`
	Discovery        bool                       `json:"discovery"`
	Models           []CustomProviderModelInput `json:"models"`
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
	ID              string `json:"id"`
	CreatedAt       string `json:"createdAt"`
	LastActivity    int64  `json:"lastActivity"`
	State           string `json:"state"`
	ArchivedAt      int64  `json:"archivedAt"`
	ProjectPath     string `json:"projectPath"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
}

// DisplayMessage is the pre-assembled, display-ready message returned by
// SessionMessages. Type is the discriminator: "user", "assistant", "tool", "system".
type DisplayMessage struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Turn    int    `json:"turn,omitempty"`

	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Args     string         `json:"args,omitempty"`
	Done     bool           `json:"done,omitempty"`
	Success  bool           `json:"success,omitempty"`
	Result   string         `json:"result,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`

	SubagentSessionIDs []SubagentSessionLink     `json:"subagentSessionIds,omitempty"`
	BackgroundProcess  *BackgroundProcessDisplay `json:"backgroundProcess,omitempty"`
}

type SubagentSessionLink struct {
	Index     int    `json:"index"`
	SessionID string `json:"sessionId"`
}

const (
	TurnActionRevertCode    = "revert_code"
	TurnActionRevertHistory = "revert_history"
	TurnActionFork          = "fork"
)

// QueuedItem is one user message awaiting backend drain. ID is stable per
// session (for keyed rendering); the queue is in-memory and volatile.
type QueuedItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// QueueState is a versioned snapshot of the input queue, returned by
// QueueSnapshot for hydration. Version is monotonic for the agent's lifetime;
// adapters drop snapshots whose version <= the last they applied.
type QueueState struct {
	Items   []QueuedItem `json:"items"`
	Version int          `json:"version"`
}

// SubmitResult reports whether submitted input started a turn or was queued.
type SubmitResult struct {
	Started bool         `json:"started"`
	Turn    int          `json:"turn,omitempty"`
	Queue   []QueuedItem `json:"queue"`
	Version int          `json:"version"`
}

// SessionPayload is the adapter-facing state snapshot for one session.
type SessionPayload struct {
	Session  SessionSummary   `json:"session"`
	Messages []DisplayMessage `json:"messages"`
	Tokens   TokenReport      `json:"tokens"`
}

// TurnActionResult is returned after a user-message revert/fork action.
type TurnActionResult struct {
	Action         string                   `json:"action"`
	Turn           int                      `json:"turn"`
	TargetTurn     int                      `json:"targetTurn"`
	SessionChanged bool                     `json:"sessionChanged"`
	Prefill        string                   `json:"prefill,omitempty"`
	RestoredFiles  []string                 `json:"restoredFiles,omitempty"`
	SkippedFiles   []snapshot.SkippedRevert `json:"skippedFiles,omitempty"`
	Session        SessionSummary           `json:"session"`
	Messages       []DisplayMessage         `json:"messages,omitempty"`
	Tokens         TokenReport              `json:"tokens"`
}

// ModelInfo holds the active model identity and catalog metadata.
type ModelInfo struct {
	Ref           string        `json:"ref"`
	Provider      string        `json:"provider"`
	Model         string        `json:"model"`
	DisplayName   string        `json:"displayName"`
	ContextWindow int           `json:"contextWindow"`
	Cost          *catalog.Cost `json:"cost,omitempty"`
	Incomplete    bool          `json:"incomplete"`
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
