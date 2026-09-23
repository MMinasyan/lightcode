package harness

import (
	"encoding/json"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// SessionLifecycle is the closed lifecycle of a durable Session. No other
// value is durable: pending, preparing, settling, recovering, cancelling, and
// stopping states are process-local transitions that never reach storage.
type SessionLifecycle string

const (
	LifecycleOpen     SessionLifecycle = "open"
	LifecycleArchived SessionLifecycle = "archived"
)

// RequestKind is the closed kind of one admitted Operation request.
type RequestKind string

const (
	RequestKindMessage RequestKind = "message"
)

// InputOrigin is the closed origin of one admitted input entry.
type InputOrigin string

const (
	InputOriginUser    InputOrigin = "user"
	InputOriginRuntime InputOrigin = "runtime"
	InputOriginPlugin  InputOrigin = "plugin"
)

// MessageMode is the closed delivery mode of one submitted Session message.
type MessageMode string

const (
	MessageModeRegular MessageMode = "regular"
	MessageModeQueued  MessageMode = "queued"
)

// SubmitDisposition is the closed outcome kind of one public Submit.
type SubmitDisposition string

const (
	DispositionAdmitted SubmitDisposition = "admitted"
	DispositionSteering SubmitDisposition = "steering"
	DispositionQueued   SubmitDisposition = "queued"
	DispositionExisting SubmitDisposition = "existing"
)

// OperationState is the closed state of one durable Operation. Running is the
// only non-terminal state; success, failure, and interruption are terminal
// and never replaced.
type OperationState string

const (
	OperationRunning      OperationState = "running"
	OperationSuccess      OperationState = "success"
	OperationFailure      OperationState = "failure"
	OperationInterruption OperationState = "interruption"
)

// EffectKind is the closed kind of one in-flight Operation effect.
type EffectKind string

const (
	EffectModel EffectKind = "model"
	EffectTool  EffectKind = "tool"
	EffectHook  EffectKind = "hook"
)

// SignalKind is the closed kind of one durable signal entry. The signal kind
// fixes the entry's model-visible content exactly.
type SignalKind string

const (
	SignalInterruption             SignalKind = "interruption"
	SignalModelFailureContinuation SignalKind = "model_failure_continuation"

	// SignalBackgroundCompletion records one background member's completion.
	// Its content is the producer-bounded completion text, not a fixed
	// contract string, and its envelope is always operationless.
	SignalBackgroundCompletion SignalKind = "background_completion"
)

// Fixed signal contents, one per signal kind. The model-visible wording of a
// durable signal is contract, not free producer text.
const (
	signalInterruptionContent             = "Operation interrupted."
	signalModelFailureContinuationContent = "The previous model response failed after partial output. Continue from the retained response."
)

// EntryRef addresses one committed entry of one Session.
type EntryRef struct {
	SessionID string `json:"session_id"`
	EntryID   string `json:"entry_id"`
}

// operationRef addresses one Operation of one Session. It is a private codec
// value, not public API.
type operationRef struct {
	SessionID   string `json:"session_id"`
	OperationID string `json:"operation_id"`
}

// UsageCount carries the signed token counts reported by one model response.
// A present all-zero value is known zero and nil means unreported; the
// distinction is preserved everywhere usage is optional.
type UsageCount struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

// ModelUsage pairs one complete model identity with its accumulated usage.
type ModelUsage struct {
	Model model.ModelRef `json:"model"`
	Usage UsageCount     `json:"usage"`
}

// UsageTotals accumulates usage per model. ByModel is unique and sorted
// lexicographically by provider, then model, in every constructed or decoded
// value.
type UsageTotals struct {
	ByModel []ModelUsage `json:"by_model"`
}

// ExecutionCapture is the complete non-secret configuration required to
// interpret one Operation: the stable configuration revision, the complete
// model identity, the system prompt, the advertised tool definitions in
// preserved order, the selected capability names in preserved order, and the
// Agent definition's permission capability constraints. Readonly and WriteDir
// are the configured lexical constraint copied from the one Harness-selected
// definition; WriteDir is already trimmed at projection and stays unchanged in
// the capture. The captured ConfigurationRevision also identifies the
// governing permission policy; no rules are durable. Capability names are
// validated for shape only: no plugin is loaded to read a historical capture.
// No resolved secret, callback, plugin value, permission representation,
// retry policy, or arbitrary plugin JSON is durable.
type ExecutionCapture struct {
	ConfigurationRevision string                 `json:"configuration_revision"`
	Model                 model.ModelRef         `json:"model"`
	SystemPrompt          string                 `json:"system_prompt"`
	Tools                 []model.ToolDefinition `json:"tools"`
	Capabilities          []string               `json:"capabilities,omitempty"`
	Readonly              bool                   `json:"readonly"`
	WriteDir              string                 `json:"write_dir"`
}

// SessionIdentity is the immutable identity section of one Session register.
// A root omits every lineage field; a fork requires both source fields; a
// child carries parent_session_id and neither source field.
type SessionIdentity struct {
	SessionID             string    `json:"session_id"`
	Workspace             string    `json:"workspace"`
	CreatedAt             time.Time `json:"created_at"`
	SourceSessionID       string    `json:"source_session_id,omitempty"`
	SourceBoundaryEntryID string    `json:"source_boundary_entry_id,omitempty"`
	ParentSessionID       string    `json:"parent_session_id,omitempty"`
}

// SessionState is the mutable state section of one Session register.
// CurrentOperationID, when present, names the Session's one running
// Operation. ArchivedAt is present exactly when the lifecycle is archived.
type SessionState struct {
	Lifecycle          SessionLifecycle `json:"lifecycle"`
	ArchivedAt         *time.Time       `json:"archived_at,omitempty"`
	CurrentAgentType   string           `json:"current_agent_type"`
	CurrentOperationID string           `json:"current_operation_id,omitempty"`
	Usage              UsageTotals      `json:"usage"`
	LastActivity       time.Time        `json:"last_activity"`
}

// SessionRecord is one decoded Session register: the payload's identity and
// state plus the storage envelope revision, which is metadata and never part
// of a payload.
type SessionRecord struct {
	Revision int64
	Identity SessionIdentity
	State    SessionState
}

// OperationAdmission is the immutable admission section of one Operation
// register. It is the sole durable Agent-type field for its Operation.
type OperationAdmission struct {
	SessionID     string           `json:"session_id"`
	OperationID   string           `json:"operation_id"`
	RequestKind   RequestKind      `json:"request_kind"`
	AdmittedEntry EntryRef         `json:"admitted_entry"`
	AgentType     string           `json:"agent_type"`
	Execution     ExecutionCapture `json:"execution"`
	AdmittedAt    time.Time        `json:"admitted_at"`
}

// ActiveEffect names the one in-flight effect of a running Operation and the
// entry reserved for its result. A model effect omits the tool-call and hook
// IDs; a tool effect requires the tool-call ID and addresses the matching
// first pending call; a hook effect requires both the hook ID and the
// tool-call ID of the first pending call it runs for.
type ActiveEffect struct {
	Kind          EffectKind `json:"kind"`
	ResultEntryID string     `json:"result_entry_id"`
	ToolCallID    string     `json:"tool_call_id,omitempty"`
	HookID        string     `json:"hook_id,omitempty"`
}

// PendingToolCall is one unresolved published tool call: the assistant entry
// that published it and the result entry reserved for it. Pending calls
// preserve unresolved assistant order and have unique reserved results.
type PendingToolCall struct {
	AssistantEntry EntryRef `json:"assistant_entry"`
	CallID         string   `json:"call_id"`
	ResultEntryID  string   `json:"result_entry_id"`
}

// OperationTerminal is the terminal section of one settled Operation: the
// settlement entry that published the outcome and its diagnostic detail.
// Success has empty detail; failure and interruption require one.
type OperationTerminal struct {
	SettlementEntry EntryRef `json:"settlement_entry"`
	Detail          string   `json:"detail,omitempty"`
}

// OperationCurrentState is the mutable state section of one Operation
// register. Running forbids the settlement time and terminal section;
// terminal states require both and forbid active and pending effects.
type OperationCurrentState struct {
	Status           OperationState     `json:"status"`
	StartedAt        time.Time          `json:"started_at"`
	SettledAt        *time.Time         `json:"settled_at,omitempty"`
	ActiveEffect     *ActiveEffect      `json:"active_effect,omitempty"`
	PendingToolCalls []PendingToolCall  `json:"pending_tool_calls"`
	Usage            UsageTotals        `json:"usage"`
	Terminal         *OperationTerminal `json:"terminal,omitempty"`
}

// OperationRecord is one decoded Operation register: the payload's admission
// and state plus the storage envelope revision, which is metadata and never
// part of a payload.
type OperationRecord struct {
	Revision  int64
	Admission OperationAdmission
	State     OperationCurrentState
}

// The six entry-payload structs below are private codec values, not public
// API. Every payload repeats the envelope Session and entry identity; normal
// entries repeat their owning Operation identity, while independently copied
// fork-prefix entries omit it.

// inputEntry is one admitted user, runtime, or plugin input.
type inputEntry struct {
	SessionID   string              `json:"session_id"`
	EntryID     string              `json:"entry_id"`
	OperationID string              `json:"operation_id,omitempty"`
	Origin      InputOrigin         `json:"origin"`
	Content     []model.ContentPart `json:"content"`
}

// toolCallRecord is one published assistant tool call: opaque call identity,
// zero-based ordinal, raw base64 arguments, and the reserved result entry.
type toolCallRecord struct {
	ID                  string          `json:"id"`
	Ordinal             int64           `json:"ordinal"`
	Name                string          `json:"name"`
	ArgumentsBase64     string          `json:"arguments_base64"`
	Extra               model.Extra     `json:"extra,omitempty"`
	NormalizedArguments json.RawMessage `json:"normalized_arguments,omitempty"`
	ResultEntryID       string          `json:"result_entry_id"`
}

// assistantEntry is one finalized model output: status, complete source
// identity, eligible model-visible payload, and reported usage when present.
type assistantEntry struct {
	SessionID   string              `json:"session_id"`
	EntryID     string              `json:"entry_id"`
	OperationID string              `json:"operation_id,omitempty"`
	Status      model.OutputStatus  `json:"status"`
	Source      model.ModelRef      `json:"source"`
	Content     []model.ContentPart `json:"content"`
	Refusal     string              `json:"refusal,omitempty"`
	Extra       model.Extra         `json:"extra,omitempty"`
	ToolCalls   []toolCallRecord    `json:"tool_calls"`
	Usage       *UsageCount         `json:"usage,omitempty"`
}

// toolResultEntry is one settled tool result answering a published call by
// its reserved identity. Metadata is the tool-owned raw-JSON member the
// Harness preserves opaquely within its well-formedness and size bound; it is
// absent on synthetic results and never reaches the model-visible result.
type toolResultEntry struct {
	SessionID      string                 `json:"session_id"`
	EntryID        string                 `json:"entry_id"`
	OperationID    string                 `json:"operation_id,omitempty"`
	AssistantEntry EntryRef               `json:"assistant_entry"`
	ToolCallID     string                 `json:"tool_call_id"`
	Status         model.ToolResultStatus `json:"status"`
	Content        string                 `json:"content"`
	Metadata       json.RawMessage        `json:"metadata,omitempty"`
}

// hookResultStatus is the closed status of one settled argument-hook
// execution.
type hookResultStatus string

const (
	hookSucceeded   hookResultStatus = "success"
	hookFailed      hookResultStatus = "error"
	hookInterrupted hookResultStatus = "interrupted"
)

// hookResultEntry is one settled argument-hook execution: the independent
// immutable historical evidence of one hook's outcome for one tool call,
// under the executing effect's own reserved identity — never a conversation
// message. Success carries the one normalized replacement object and no
// error; every other status carries a non-empty error and no arguments.
type hookResultEntry struct {
	SessionID   string           `json:"session_id"`
	EntryID     string           `json:"entry_id"`
	OperationID string           `json:"operation_id"`
	HookID      string           `json:"hook_id"`
	ToolCallID  string           `json:"tool_call_id"`
	Status      hookResultStatus `json:"status"`
	Arguments   json.RawMessage  `json:"arguments,omitempty"`
	Error       string           `json:"error,omitempty"`
}

// relatedMember addresses the background member whose completion one
// background_completion signal records. It is informational history, never
// resolved across Sessions.
type relatedMember struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// signalEntry is one durable control signal. Its related source Operation is
// typed history: informational in a copied fork prefix, resolving inside the
// owning Session otherwise. A background_completion signal instead carries a
// related background member and no Operation at all.
type signalEntry struct {
	SessionID        string         `json:"session_id"`
	EntryID          string         `json:"entry_id"`
	OperationID      string         `json:"operation_id,omitempty"`
	Signal           SignalKind     `json:"signal"`
	RelatedOperation *operationRef  `json:"related_operation,omitempty"`
	RelatedMember    *relatedMember `json:"related_member,omitempty"`
	Content          string         `json:"content"`
}

// operationSettlementEntry publishes one terminal Operation outcome. It is
// never operationless; the model identity is present exactly when the
// settlement carries usage.
type operationSettlementEntry struct {
	SessionID   string          `json:"session_id"`
	EntryID     string          `json:"entry_id"`
	OperationID string          `json:"operation_id"`
	Status      OperationState  `json:"status"`
	Detail      string          `json:"detail,omitempty"`
	Model       *model.ModelRef `json:"model,omitempty"`
	Usage       *UsageCount     `json:"usage,omitempty"`
}
