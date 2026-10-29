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
	RequestKindCompact RequestKind = "compact"
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

// HookIdentity is one recorded extension hook contribution: the owning
// extension identity and the hook identity selected from it.
type HookIdentity struct {
	ExtensionID string `json:"extension_id"`
	HookID      string `json:"hook_id"`
}

// RequestEvidence is the one closed request-evidence value: the canonical
// boundary/checkpoint references of the projection actually consumed at that
// boundary, the effective prompt/context/tool-view deltas against the frozen
// capture (absent members mean unchanged), and the contributing hook
// identities in contribution order. It records changes plus canonical input
// references, never a copy of unchanged conversation history.
type RequestEvidence struct {
	CheckpointEntryID string          `json:"checkpoint_entry_id,omitempty"`
	BoundaryEntryID   string          `json:"boundary_entry_id"`
	Prompt            *string         `json:"prompt,omitempty"`
	Messages          []model.Message `json:"messages,omitempty"`
	ToolNames         []string        `json:"tool_names,omitempty"`
	HookIDs           []HookIdentity  `json:"hook_ids,omitempty"`
}

// RunningRequest is the one suspended conversation request state: the request
// identity bound before contributions, the frozen canonical
// boundary/checkpoint references, and the committed effectful request-hook
// result references in commit order. It is published and advanced only inside
// existing effect/checkpoint transactions.
type RunningRequest struct {
	RequestID         string     `json:"request_id"`
	CheckpointEntryID string     `json:"checkpoint_entry_id,omitempty"`
	BoundaryEntryID   string     `json:"boundary_entry_id"`
	HookResults       []EntryRef `json:"hook_results,omitempty"`
}

// operationRef addresses one Operation of one Session. It is a codec value
// exposed to callers through the OperationRef alias.
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

// ModelPricing is one effective model's captured USD-per-million-token rates
// taken from the catalog at admission: owned nullable rates, absent meaning
// the rate was not priced. Each rate is finite; a non-finite rate never
// reaches a durable capture.
type ModelPricing struct {
	Input     *float64 `json:"input,omitempty"`
	Output    *float64 `json:"output,omitempty"`
	CacheRead *float64 `json:"cache_read,omitempty"`
}

// Equal reports value equality of two pricing values: nil equals only nil,
// and each present rate is compared by value, never by pointer.
func (p *ModelPricing) Equal(q *ModelPricing) bool {
	if p == nil || q == nil {
		return p == nil && q == nil
	}
	return rateEqual(p.Input, q.Input) && rateEqual(p.Output, q.Output) &&
		rateEqual(p.CacheRead, q.CacheRead)
}

func rateEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Clone returns an independent owned copy of one pricing value: every
// present rate pointee is copied (a nil value clones to nil).
func (p *ModelPricing) Clone() *ModelPricing {
	if p == nil {
		return nil
	}
	out := ModelPricing{}
	if p.Input != nil {
		v := *p.Input
		out.Input = &v
	}
	if p.Output != nil {
		v := *p.Output
		out.Output = &v
	}
	if p.CacheRead != nil {
		v := *p.CacheRead
		out.CacheRead = &v
	}
	return &out
}

// ExecutionCapture is the complete non-secret configuration required to
// interpret one Operation: the stable configuration revision, the complete
// model identity, the system prompt, the advertised tool definitions in
// preserved order, the selected capability names in preserved order, the
// Agent definition's permission capability constraints, and the compaction
// configuration — the effective compact model's selection and its prompt.
// Pricing is the captured conversation model's rates; absence means the
// model's pricing was unknown. Readonly and WriteDir
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
	ContextWindow         int                    `json:"context_window"`
	OutputReserve         int                    `json:"output_reserve"`
	SystemPrompt          string                 `json:"system_prompt"`
	Tools                 []model.ToolDefinition `json:"tools"`
	Capabilities          []string               `json:"capabilities,omitempty"`
	Readonly              bool                   `json:"readonly"`
	WriteDir              string                 `json:"write_dir"`
	Pricing               *ModelPricing          `json:"pricing,omitempty"`
	Compact               CompactCapture         `json:"compact"`
	Strategy              StrategyCapture        `json:"strategy"`
}

// StrategyCapture is the resolved selected context-strategy input of one
// capture: the binding's stable identity, its non-secret settings, and the
// declared context-window requirement. It carries no strategy code.
type StrategyCapture struct {
	ID                    string          `json:"id"`
	Settings              json.RawMessage `json:"settings,omitempty"`
	RequiresContextWindow bool            `json:"requires_context_window"`
}

// CompactCapture is the effective compaction configuration of one capture:
// the model the compaction summarizer runs on, the compact type's system
// prompt, and that final effective model's captured pricing — the
// conversation model's pricing exactly when the fallback kept the
// conversation model and its transport. Equality of two compact captures is
// value equality through the pricing pointer and its rate pointees, never
// pointer identity.
type CompactCapture struct {
	Model        model.ModelRef `json:"model"`
	SystemPrompt string         `json:"system_prompt"`
	Pricing      *ModelPricing  `json:"pricing,omitempty"`
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
// Operation. CheckpointEntryID, when present, names the in-session context
// checkpoint entry whose messages define the model-visible context. ArchivedAt is
// present exactly when the lifecycle is archived.
type SessionState struct {
	Lifecycle          SessionLifecycle `json:"lifecycle"`
	ArchivedAt         *time.Time       `json:"archived_at,omitempty"`
	CurrentAgentType   string           `json:"current_agent_type"`
	CurrentOperationID string           `json:"current_operation_id,omitempty"`
	CheckpointEntryID  string           `json:"checkpoint_entry_id,omitempty"`
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
// entry reserved for its result. A model effect carries its request identity,
// actual model, and purpose; a conversation model effect also carries the
// effective request evidence it consumes, a maintenance effect never does. A
// tool effect requires the tool-call ID, addresses the matching first pending
// call, and carries the accepted normalized selection plus the contributing
// argument-hook provenance. A hook effect carries its extension and hook
// identities plus its consumed input under exactly one binding: the tool-call
// ID of the first pending call it runs for, or the request identity of a
// request-bound hook.
type ActiveEffect struct {
	Kind          EffectKind `json:"kind"`
	ResultEntryID string     `json:"result_entry_id"`
	ToolCallID    string     `json:"tool_call_id,omitempty"`
	HookID        string     `json:"hook_id,omitempty"`

	RequestID            string            `json:"request_id,omitempty"`
	Model                *model.ModelRef   `json:"model,omitempty"`
	Purpose              InvocationPurpose `json:"purpose,omitempty"`
	Evidence             *RequestEvidence  `json:"evidence,omitempty"`
	ExtensionID          string            `json:"hook_extension_id,omitempty"`
	InputArgumentsBase64 string            `json:"input_arguments_base64,omitempty"`
	FinalArguments       json.RawMessage   `json:"final_arguments,omitempty"`
	ContributionIDs      []HookIdentity    `json:"hook_ids,omitempty"`
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
// Request, when present, is the one suspended conversation request state
// published with the first existing effect/checkpoint transition and cleared
// at the conversation result without a pending batch, the final pending tool
// result, or the terminal settlement. UnreportedMaintenance records the
// knowledge that a completed maintenance model intent's facts are not yet
// durably recorded: set when the empty running result clears a completed
// maintenance intent, preserved across every later transition and recovery,
// and cleared only by the successful checkpoint or terminal publication that
// records those facts.
type OperationCurrentState struct {
	Status                OperationState     `json:"status"`
	StartedAt             time.Time          `json:"started_at"`
	SettledAt             *time.Time         `json:"settled_at,omitempty"`
	ActiveEffect          *ActiveEffect      `json:"active_effect,omitempty"`
	Request               *RunningRequest    `json:"running_request,omitempty"`
	PendingToolCalls      []PendingToolCall  `json:"pending_tool_calls"`
	Usage                 UsageTotals        `json:"usage"`
	Terminal              *OperationTerminal `json:"terminal,omitempty"`
	UnreportedMaintenance bool               `json:"unreported_maintenance,omitempty"`
}

// OperationRecord is one decoded Operation register: the payload's admission
// and state plus the storage envelope revision, which is metadata and never
// part of a payload.
type OperationRecord struct {
	Revision  int64
	Admission OperationAdmission
	State     OperationCurrentState
}

// The entry-payload structs below are the codec's validated payload values.
// Every payload repeats the envelope Session and entry identity; normal
// entries repeat their owning Operation identity, while independently copied
// fork-prefix entries omit it. The exported aliases below make the exact
// struct shapes reachable as the HistoryFact payload members; no duplicate
// field schema exists.

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
// Exactly one of NormalizedArguments and NormalizationFailure is present for
// an advertised call and neither for an unadvertised one: the successful
// normalization of the original, or the bounded failure diagnostic its one
// normalization produced, both committed by the producer. Later dispatch
// consumes the committed member and never normalizes the original again.
type toolCallRecord struct {
	ID                   string          `json:"id"`
	Ordinal              int64           `json:"ordinal"`
	Name                 string          `json:"name"`
	ArgumentsBase64      string          `json:"arguments_base64"`
	Extra                model.Extra     `json:"extra,omitempty"`
	NormalizedArguments  json.RawMessage `json:"normalized_arguments,omitempty"`
	NormalizationFailure string          `json:"normalization_failure,omitempty"`
	ResultEntryID        string          `json:"result_entry_id"`
}

// assistantEntry is one finalized model output: status, complete source
// identity, eligible model-visible payload, reported usage when present, and
// the producing request identity with its final effective request evidence
// and advertised names. The evidence is not conversation text.
type assistantEntry struct {
	SessionID       string              `json:"session_id"`
	EntryID         string              `json:"entry_id"`
	OperationID     string              `json:"operation_id,omitempty"`
	Status          model.OutputStatus  `json:"status"`
	Source          model.ModelRef      `json:"source"`
	Content         []model.ContentPart `json:"content"`
	Refusal         string              `json:"refusal,omitempty"`
	Extra           model.Extra         `json:"extra,omitempty"`
	ToolCalls       []toolCallRecord    `json:"tool_calls"`
	Usage           *UsageCount         `json:"usage,omitempty"`
	RequestID       string              `json:"request_id"`
	AdvertisedNames []string            `json:"advertised_names"`
	Evidence        *RequestEvidence    `json:"evidence"`
}

// toolResultEntry is one settled tool result answering a published call by
// its reserved identity. Metadata is the tool-owned raw-JSON member the
// Harness preserves opaquely within its well-formedness and size bound; it is
// absent on synthetic results and never reaches the model-visible result.
// NormalizedArguments and HookIDs carry the final effective argument selection
// and its contributing ordered hook provenance actually consumed by that
// call's settlement; results that never accepted a value carry neither.
// Evidence is the producer-assigned execution knowledge of the settled call,
// independent of status: proven unstarted work is not_started, a produced
// terminal outcome is observed, and a possibly-executed-but-unverified
// external outcome is unknown. Content is the final model-facing string built
// once by the shared result producer after pure output shaping and evidence
// rendering; OriginalContent is that producer's unshaped content, and
// ShapingFailure the bounded processing failure of an aborted shaping chain.
type toolResultEntry struct {
	SessionID           string                 `json:"session_id"`
	EntryID             string                 `json:"entry_id"`
	OperationID         string                 `json:"operation_id,omitempty"`
	AssistantEntry      EntryRef               `json:"assistant_entry"`
	ToolCallID          string                 `json:"tool_call_id"`
	Status              model.ToolResultStatus `json:"status"`
	Content             string                 `json:"content"`
	OriginalContent     string                 `json:"original_content"`
	ShapingFailure      string                 `json:"shaping_failure,omitempty"`
	Evidence            ExecutionEvidence      `json:"evidence"`
	Metadata            json.RawMessage        `json:"metadata,omitempty"`
	NormalizedArguments json.RawMessage        `json:"normalized_arguments,omitempty"`
	HookIDs             []HookIdentity         `json:"hook_ids,omitempty"`
}

// hookResultStatus is the closed status of one settled effectful hook
// execution.
type hookResultStatus string

const (
	hookSucceeded   hookResultStatus = "success"
	hookFailed      hookResultStatus = "error"
	hookInterrupted hookResultStatus = "interrupted"
)

// hookResultEntry is one settled effectful-hook execution: the independent
// immutable historical evidence of one hook's outcome under the executing
// effect's own reserved identity — never a conversation message. Its binding
// is exactly one of a tool call (argument hook) or a request (request hook),
// never both. Every entry carries the extension and hook identities, the
// consumed input evidence the hook actually received before I/O, and the
// execution knowledge independently of status. A successful tool-bound result
// carries the one normalized replacement object; a successful request-bound
// result carries the closed consequence (an empty consequence is a valid
// no-op); every other status carries a non-empty error and no consequence.
type hookResultEntry struct {
	SessionID   string            `json:"session_id"`
	EntryID     string            `json:"entry_id"`
	OperationID string            `json:"operation_id"`
	HookID      string            `json:"hook_id"`
	ExtensionID string            `json:"extension_id"`
	ToolCallID  string            `json:"tool_call_id,omitempty"`
	RequestID   string            `json:"request_id,omitempty"`
	Status      hookResultStatus  `json:"status"`
	Evidence    ExecutionEvidence `json:"evidence"`
	Arguments   json.RawMessage   `json:"arguments,omitempty"`
	Error       string            `json:"error,omitempty"`

	InputArgumentsBase64 string              `json:"input_arguments_base64,omitempty"`
	InputEvidence        *RequestEvidence    `json:"input_evidence,omitempty"`
	Consequence          *RequestConsequence `json:"consequence,omitempty"`
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

// maintenanceFact is one ordered maintenance procedure fact: the producer's
// own request identity, the complete model identity that consumed it, the
// actual tool-free logical Request messages the effect consumed, and the
// optional reported usage — an absent usage differs from a known zero. Facts
// settle once on the successful checkpoint or on the terminal settlement,
// never both.
type maintenanceFact struct {
	RequestID string          `json:"request_id"`
	Model     model.ModelRef  `json:"model"`
	Input     []model.Message `json:"input"`
	Usage     *UsageCount     `json:"usage,omitempty"`
}

// operationSettlementEntry publishes one terminal Operation outcome. It is
// never operationless; the settlement's usage requires its model identity,
// while the model identity alone additionally marks one no-output terminal
// model producer whose usage report was absent. CompactionUsage marks one
// settlement as the
// maintenance procedure's own provenance: it identifies the maintenance
// family directly and its ordered Facts carry the procedure's per-request
// usage. RequestID is the
// settlement's own request witness — the answered conversation request, a live
// model intent's identity, or a maintenance settlement's producer identity;
// ConsumedRequestID names the conversation request whose frozen view an
// automatic maintenance settlement consumed, present only beside maintenance
// evidence and distinct from that producer. Evidence is the final effective
// request evidence owned by a no-output settlement, and Facts are the actual
// tool-free logical maintenance requests the procedure reached with their
// producing models and reported usage. Maintenance facts settle once on the
// successful checkpoint or on the terminal settlement, never both.
type operationSettlementEntry struct {
	SessionID         string            `json:"session_id"`
	EntryID           string            `json:"entry_id"`
	OperationID       string            `json:"operation_id"`
	Status            OperationState    `json:"status"`
	Detail            string            `json:"detail,omitempty"`
	Model             *model.ModelRef   `json:"model,omitempty"`
	Usage             *UsageCount       `json:"usage,omitempty"`
	CompactionUsage   bool              `json:"compaction_usage,omitempty"`
	RequestID         string            `json:"request_id,omitempty"`
	ConsumedRequestID string            `json:"consumed_request_id,omitempty"`
	Evidence          *RequestEvidence  `json:"evidence,omitempty"`
	Facts             []maintenanceFact `json:"facts,omitempty"`
}

// contextCheckpointEntry is one immutable context checkpoint commit: the
// shared-schema messages defining the new model-visible context over the
// frozen conversation up to its covered boundary entry, the selected strategy
// identity that produced them, the governing configuration revision, and the
// procedure's ordered maintenance facts — the producing request identities,
// models, actual tool-free inputs and optional reported usage.
// ConsumedRequestID names the conversation request whose frozen contribution
// view the automatic procedure consumed (manual maintenance omits it), and
// Evidence is the effective contribution evidence consumed at that frozen
// canonical boundary.
// Independently copied fork-prefix entries omit the owning Operation identity
// and carry no source usage.
type contextCheckpointEntry struct {
	SessionID             string            `json:"session_id"`
	EntryID               string            `json:"entry_id"`
	OperationID           string            `json:"operation_id,omitempty"`
	StrategyID            string            `json:"strategy_id"`
	Messages              []model.Message   `json:"messages"`
	BoundaryEntryID       string            `json:"boundary_entry_id"`
	ConfigurationRevision string            `json:"configuration_revision"`
	Facts                 []maintenanceFact `json:"facts"`
	ConsumedRequestID     string            `json:"consumed_request_id,omitempty"`
	Evidence              *RequestEvidence  `json:"evidence"`
}

// Exported aliases of the validated entry payloads: the HistoryFact union
// exposes these exact structs, their JSON tags, and their nested members. A
// hook_result payload stays execution evidence and has no exported alias.
type (
	InputEntry               = inputEntry
	AssistantEntry           = assistantEntry
	ToolResultEntry          = toolResultEntry
	SignalEntry              = signalEntry
	OperationSettlementEntry = operationSettlementEntry
	ContextCheckpointEntry   = contextCheckpointEntry
	MaintenanceFact          = maintenanceFact
	ToolCallRecord           = toolCallRecord
	RelatedMember            = relatedMember
	OperationRef             = operationRef
)
