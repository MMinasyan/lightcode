// Package engine implements the agentic loop: user message → model →
// text or tool calls → execute → feed result back → repeat until the
// model returns a text-only response.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/engine/modelclient"
	"github.com/MMinasyan/lightcode/internal/engine/tool"
)

// maxIterations caps how many model→tool→model rounds a single user turn
// may perform before the loop gives up. Prevents runaway tool-call cycles.
const maxIterations = 25

// traceMaxChars is the length at which tool call arguments and results
// are truncated when written to the trace. Keeps the REPL readable when
// the agent reads large files.
const traceMaxChars = 200

const (
	systemSignalOpen         = "<system-signal>"
	systemSignalClose        = "</system-signal>"
	systemSignalInternalKind = "system_signal"
	toolResultErrorKind      = "tool_result_error"
)

// ErrNoModelConfigured is returned when the engine has no model client for a
// turn. Runtime/harness code owns user-facing setup guidance.
var ErrNoModelConfigured = errors.New("no model configured")

var systemSignalEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
)

// interruptedSignal is injected as a user-role message when the user
// cancels a turn. User-role works across all OpenAI-compatible providers.
var interruptedSignal = SystemSignal("Request interrupted by user")

// SystemSignal escapes payload and wraps it as a user-role system signal.
func SystemSignal(payload string) string {
	return systemSignalOpen + systemSignalEscaper.Replace(payload) + systemSignalClose
}

// NewSystemSignalMessage returns a backend-owned system signal message. The
// private marker distinguishes internal signals from literal user input.
func NewSystemSignalMessage(payload string) message.Message {
	msg := message.NewText(message.RoleUser, SystemSignal(payload))
	msg.InternalKind = systemSignalInternalKind
	return msg
}

// ParseSystemSignalMessage parses only backend-marked system-signal messages.
func ParseSystemSignalMessage(msg message.Message) (payload string, ok bool) {
	if msg.Role != message.RoleUser || msg.InternalKind != systemSignalInternalKind {
		return "", false
	}
	content := msg.TextContent()
	if !strings.HasPrefix(content, systemSignalOpen) || !strings.HasSuffix(content, systemSignalClose) {
		return "", false
	}
	inner := content[len(systemSignalOpen) : len(content)-len(systemSignalClose)]
	return html.UnescapeString(inner), true
}

// MarkToolResultError persists the live ToolCallEnd error bit on a tool result.
func MarkToolResultError(msg *message.Message) {
	if msg != nil {
		msg.InternalKind = toolResultErrorKind
	}
}

// IsToolResultErrorMessage reports whether a persisted tool result was
// live-rendered as an error.
func IsToolResultErrorMessage(msg message.Message) bool {
	return msg.Role == message.RoleTool && msg.InternalKind == toolResultErrorKind
}

// Store is the minimum surface the loop needs from the snapshot
// package: turn-scoped message persistence, turn completion, and
// activity touch. Declared here so loop has no import dependency on
// snapshot storage. app.go wires the concrete implementation.
type Store interface {
	AppendMessage(turn int, msg []byte) error
	MarkTurnComplete(turn int) error
	TouchActivity() error
	CurrentTurn() int
}

// EventKind identifies the phase of a tool call being reported.
type EventKind int

const (
	// ToolCallStart is emitted before a tool's Execute runs.
	ToolCallStart EventKind = iota
	// ToolCallEnd is emitted after a tool's Execute returns (or errors).
	ToolCallEnd
	// PermissionRequest is emitted when a tool is waiting for user approval.
	PermissionRequest
	// TextDelta carries an incremental text chunk in Event.Result.
	TextDelta
	// Usage carries token counts reported by the server for one
	// completed streaming response. Known is false when the server
	// did not return usage data.
	Usage
	// Warning carries a non-fatal runtime warning in Result.
	Warning
	// BackgroundProcessComplete carries a background process completion display item.
	BackgroundProcessComplete
	// UserMessageDisplay is emitted whenever a user-role message is appended
	// to history. Carries the persisted content and the owning turn so adapters
	// can render user messages in the same order they appear in reload.
	UserMessageDisplay
	// GenericSystemSignalDisplay is emitted whenever a non-background
	// <system-signal> entry is appended to history. Result carries the
	// already-collapsed one-line payload (no <system-signal> wrapper, no
	// HTML escapes, no newlines) so adapters can prefix and render directly.
	GenericSystemSignalDisplay
)

// Event is a structured tool-call event for UIs that want to render tool
// activity as first-class items instead of parsing the text trace.
type Event struct {
	Kind       EventKind
	ToolName   string
	ToolCallID string
	Args       string
	Result     string
	IsError    bool
	PermID     string
	PermArg    string

	// Metadata carries extra information for UI rendering (diffs, etc.).
	Metadata map[string]any

	// Usage fields (Usage kind only).
	Model      string
	ModelRef   coremodel.ModelRef
	Cache      int
	Input      int
	Output     int
	UsageKnown bool

	// Turn is set when the event belongs to a persisted conversation turn.
	Turn int

	BackgroundProcess *BackgroundProcessDisplay
}

// BackgroundProcessDisplay is the user-visible sidecar for a background
// terminal completion signal after it is added to model-visible history.
type BackgroundProcessDisplay struct {
	ID       string
	Command  string
	Reason   string
	ExitCode int
	Output   string
}

// PendingSignal is async model input owned by the loop. Wake signals ask the
// agent scheduler to start a model turn when idle. Persist controls whether
// the wrapped user-role signal is written to the current turn.
type PendingSignal struct {
	Payload           string
	Wake              bool
	Persist           bool
	BackgroundProcess *BackgroundProcessDisplay
}

// ContextTransformCheckpoint describes a model-request checkpoint where the
// caller may transform conversation history before the request is sent.
type ContextTransformCheckpoint struct {
	Turn            int
	ActiveTurnStart int
	Force           bool
}

// ContextTransformResult reports whether history was transformed and where the
// current in-progress turn now starts after the transform.
type ContextTransformResult struct {
	Transformed     bool
	ActiveTurnStart int
}

// ContextTransformer runs at model-request checkpoints.
type ContextTransformer interface {
	BeforeModelRequest(ctx context.Context, checkpoint ContextTransformCheckpoint) (ContextTransformResult, error)
}

// UsageRecorder observes usage synchronously before usage events are forwarded
// to asynchronous subscribers.
type UsageRecorder interface {
	RecordUsage(Event)
}

// Loop owns the conversation history for a single session and drives
// the agentic loop on each user turn.
type Loop struct {
	client             modelclient.ChatStreamer
	registry           *tool.Registry
	messages           []message.Message
	contextTransformer ContextTransformer
	usageRecorder      UsageRecorder

	// turnBoundaries records the index of each user message in
	// l.messages as it is appended. turnBoundaries[i] is the index of
	// turn (i+1)'s user message.
	turnBoundaries []int

	// store is the persistence backing for messages + turn state.
	// Tool calls and assistant messages are persisted to whichever
	// session the store currently holds. May be nil for tests.
	store Store

	trace  io.Writer
	events chan<- Event

	droppedEvents int

	// pendingQueue holds staged tool calls for batch execution. Created fresh
	// per turn.
	pendingQueue *tool.PendingQueue

	// consumedFlushResults accumulates the BatchResults of staged flushes
	// triggered during one assistant message's tool dispatch (explicit flush
	// or a flush-before-a-non-pending-tool). The Run dispatch loop persists a
	// single <staged-flush> wrapper from these after the loop and resets it.
	consumedFlushResults []tool.BatchResult

	pendingExecutor tool.PendingExecutor

	signalMu       sync.Mutex
	pendingSignals []PendingSignal
}

// New returns a Loop pre-seeded with the system prompt.
func New(client modelclient.ChatStreamer, registry *tool.Registry, systemPrompt string) *Loop {
	return &Loop{
		client:   client,
		registry: registry,
		messages: []message.Message{message.NewText(message.RoleSystem, systemPrompt)},
		trace:    io.Discard,
	}
}

// SetClient replaces the provider client used for subsequent turns.
func (l *Loop) SetClient(c modelclient.ChatStreamer) { l.client = c }

// SetStore wires a persistence store into the loop. Messages appended
// after this call are persisted via store.AppendMessage.
func (l *Loop) SetStore(s Store) { l.store = s }

// SetTrace configures the io.Writer to which tool call activity is
// written. Passing nil disables the trace.
func (l *Loop) SetTrace(w io.Writer) {
	if w == nil {
		l.trace = io.Discard
		return
	}
	l.trace = w
}

// SetEvents registers a channel to receive structured tool call events.
func (l *Loop) SetEvents(ch chan<- Event) { l.events = ch }

// SetContextTransformer configures the before-model-request transform hook.
func (l *Loop) SetContextTransformer(t ContextTransformer) { l.contextTransformer = t }

// SetUsageRecorder configures synchronous usage observation.
func (l *Loop) SetUsageRecorder(r UsageRecorder) { l.usageRecorder = r }

// SetPendingExecutor configures the executor used to flush staged calls.
func (l *Loop) SetPendingExecutor(exec tool.PendingExecutor) {
	l.pendingExecutor = exec
	if l.registry != nil {
		if coordinator, ok := l.registry.PendingCoordinator(); ok {
			if setter, ok := coordinator.(interface{ SetPendingExecutor(tool.PendingExecutor) }); ok {
				setter.SetPendingExecutor(exec)
			}
		}
	}
}

func (l *Loop) emit(ev Event) {
	if ev.Kind == Usage && l.usageRecorder != nil {
		l.usageRecorder.RecordUsage(ev)
		return
	}
	if l.events == nil {
		return
	}
	if isTranscriptEvent(ev.Kind) {
		// Transcript display events back the backend-owned display-order
		// invariant: every persisted display item must produce exactly
		// one live event in the same position. Dropping any of them
		// makes the live UI disagree with reload, so block until the
		// adapter has drained instead.
		l.flushDroppedWarningLocked()
		l.events <- ev
		return
	}
	if l.droppedEvents > 0 && ev.Kind != Warning {
		warning := Event{Kind: Warning, Result: fmt.Sprintf("dropped %d events because event channel was full", l.droppedEvents)}
		select {
		case l.events <- warning:
			l.droppedEvents = 0
		default:
		}
	}
	select {
	case l.events <- ev:
	default:
		l.droppedEvents++
	}
}

// flushDroppedWarningLocked emits the pending dropped-events warning before a
// non-droppable event so chronological ordering is preserved. The warning
// itself is still droppable.
func (l *Loop) flushDroppedWarningLocked() {
	if l.droppedEvents == 0 {
		return
	}
	warning := Event{Kind: Warning, Result: fmt.Sprintf("dropped %d events because event channel was full", l.droppedEvents)}
	select {
	case l.events <- warning:
		l.droppedEvents = 0
	default:
	}
}

// isTranscriptEvent reports whether ev.Kind contributes a row to the live or
// persisted transcript and therefore must not be dropped from the event
// stream. Keep this in sync with the projection rules in the
// TestEventOrderEqualsMessagesForFrontend invariant test.
func isTranscriptEvent(kind EventKind) bool {
	switch kind {
	case TextDelta,
		ToolCallStart,
		ToolCallEnd,
		BackgroundProcessComplete,
		UserMessageDisplay,
		GenericSystemSignalDisplay:
		return true
	}
	return false
}

// Messages returns the current in-memory conversation, including the
// system prompt at index 0. Callers must not mutate the returned slice.
func (l *Loop) Messages() []message.Message { return l.messages }

func assistantMessageHasPayload(msg message.Message) bool {
	return len(msg.Content) > 0 || msg.Refusal != "" || len(msg.ToolCalls) > 0 || len(msg.Extra) > 0
}

func assistantVisibleText(msg message.Message) string {
	if text := msg.TextContent(); text != "" {
		return text
	}
	return msg.Refusal
}

func (l *Loop) normalizeAssistantToolCalls(msg message.Message) message.Message {
	for i := range msg.ToolCalls {
		msg.ToolCalls[i] = l.normalizeToolCall(msg.ToolCalls[i])
	}
	return msg
}

func (l *Loop) normalizeToolCall(tc message.ToolCall) message.ToolCall {
	if l == nil || l.registry == nil {
		return tc
	}
	normalizer, ok := l.registry.ArgumentNormalizer(tc.Function.Name)
	if !ok {
		return tc
	}
	normalized, err := normalizer.NormalizeArguments(json.RawMessage(tc.Function.Arguments))
	if err != nil || len(normalized) == 0 {
		return tc
	}
	tc.Function.Arguments = string(normalized)
	return tc
}

func emptyAssistantResponseError(finishReason openai.FinishReason, sawChoice bool) error {
	if finishReason != "" && finishReason != openai.FinishReasonNull {
		return fmt.Errorf("empty assistant response (finish_reason=%s)", finishReason)
	}
	if !sawChoice {
		return fmt.Errorf("empty assistant response (no choices received)")
	}
	return fmt.Errorf("empty assistant response")
}

func filterHistoryTurn(turn []message.Message) []message.Message {
	out := make([]message.Message, 0, len(turn))
	for _, msg := range turn {
		if msg.Role == message.RoleAssistant && !assistantMessageHasPayload(msg) {
			continue
		}
		out = append(out, msg)
	}
	return out
}

// UpdateSystemPrompt replaces the system prompt (messages[0]).
func (l *Loop) UpdateSystemPrompt(content string) {
	if len(l.messages) > 0 && l.messages[0].Role == message.RoleSystem {
		l.messages[0] = message.NewText(message.RoleSystem, content)
	}
}

// AppendUserMessage adds a user message to the conversation and persists
// it under the given turn. Does not run the model.
func (l *Loop) AppendUserMessage(turn int, content string) {
	l.turnBoundaries = append(l.turnBoundaries, len(l.messages))
	l.persistAndEmitUserMessage(turn, content)
}

// persistAndEmitUserMessage is the single chokepoint for appending a user-role
// message into history: it appends to l.messages, persists to the store, and
// emits a UserMessageDisplay event. Callers own turnBoundaries.
func (l *Loop) persistAndEmitUserMessage(turn int, content string) {
	l.persistUserMessage(turn, content)
	l.emitUserMessageDisplay(turn, content)
}

func (l *Loop) persistUserMessage(turn int, content string) {
	msg := message.NewText(message.RoleUser, content)
	l.messages = append(l.messages, msg)
	l.persistMessage(turn, msg)
}

func (l *Loop) emitUserMessageDisplay(turn int, content string) {
	l.emit(Event{Kind: UserMessageDisplay, Turn: turn, Result: content})
}

// collapseOneLine flattens runs of whitespace (including newlines) to single
// spaces and trims leading/trailing whitespace. Used for system-signal payload
// rendering so display strings have no embedded line breaks.
func collapseOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// AddPendingSignal records async model input. The loop drains pending signals
// only when the next model request can legally include them.
func (l *Loop) AddPendingSignal(signal PendingSignal) {
	if signal.Payload == "" {
		return
	}
	l.signalMu.Lock()
	defer l.signalMu.Unlock()
	l.pendingSignals = append(l.pendingSignals, signal)
}

// HasPendingWakeSignal reports whether any pending signal should wake the model
// when no turn is active.
func (l *Loop) HasPendingWakeSignal() bool {
	l.signalMu.Lock()
	defer l.signalMu.Unlock()
	for _, signal := range l.pendingSignals {
		if signal.Wake {
			return true
		}
	}
	return false
}

// HasPendingSignal reports whether any async model input is pending.
func (l *Loop) HasPendingSignal() bool {
	l.signalMu.Lock()
	defer l.signalMu.Unlock()
	return len(l.pendingSignals) > 0
}

// DrainPendingSignalsForModel appends pending system signals to history so the
// next model request sees them. Signals do not create turn boundaries.
func (l *Loop) DrainPendingSignalsForModel(turn int) {
	l.signalMu.Lock()
	signals := append([]PendingSignal(nil), l.pendingSignals...)
	l.pendingSignals = nil
	l.signalMu.Unlock()
	for _, signal := range signals {
		l.appendSignal(signal, turn)
		if signal.BackgroundProcess != nil {
			bg := signal.BackgroundProcess
			l.emit(Event{
				Kind:              BackgroundProcessComplete,
				Result:            bg.Output,
				IsError:           !(bg.Reason == "completed" && bg.ExitCode == 0),
				Turn:              turn,
				BackgroundProcess: bg,
			})
			continue
		}
		l.emit(Event{
			Kind:   GenericSystemSignalDisplay,
			Result: collapseOneLine(signal.Payload),
			Turn:   turn,
		})
	}
}

// EnsureInterruptedSignal appends and persists the interrupted-signal entry
// idempotently. If the last message is already that signal, it returns without
// appending or emitting. Otherwise it appends, persists, and emits a
// GenericSystemSignalDisplay event so adapters can render the transcript line.
func (l *Loop) EnsureInterruptedSignal(turn int) {
	if len(l.messages) > 0 {
		last := l.messages[len(l.messages)-1]
		if last.Role == message.RoleUser && last.InternalKind == systemSignalInternalKind && last.TextContent() == interruptedSignal {
			return
		}
	}
	signalMsg := NewSystemSignalMessage("Request interrupted by user")
	l.messages = append(l.messages, signalMsg)
	l.persistMessage(turn, signalMsg)
	l.emit(Event{
		Kind:   GenericSystemSignalDisplay,
		Result: "Request interrupted by user",
		Turn:   turn,
	})
}

func (l *Loop) appendSignal(signal PendingSignal, turn int) {
	msg := NewSystemSignalMessage(signal.Payload)
	l.messages = append(l.messages, msg)
	if signal.Persist {
		l.persistMessage(turn, msg)
	}
}

func (l *Loop) clearPendingSignals() {
	l.signalMu.Lock()
	defer l.signalMu.Unlock()
	l.pendingSignals = nil
}

func (l *Loop) resetHistory(clearSignals bool) {
	if clearSignals {
		l.clearPendingSignals()
	}
	if len(l.messages) > 0 && l.messages[0].Role == message.RoleSystem {
		l.messages = l.messages[:1]
	} else {
		l.messages = nil
	}
	l.turnBoundaries = nil
}

// ResetHistory drops all messages and turn boundaries, leaving only
// the system prompt. Used when switching sessions.
func (l *Loop) ResetHistory() {
	l.resetHistory(true)
}

// LoadHistory restores a conversation from persisted turns. Each
// turn's messages are appended in order. The first message of each
// turn is assumed to be the user message (defines the turn boundary).
// The existing system prompt is preserved.
func (l *Loop) LoadHistory(turns [][]message.Message) {
	l.ResetHistory()
	for _, turn := range turns {
		turn = filterHistoryTurn(turn)
		if len(turn) == 0 {
			continue
		}
		l.turnBoundaries = append(l.turnBoundaries, len(l.messages))
		l.messages = append(l.messages, turn...)
	}
}

// LoadHistoryWithSummary restores a conversation that went through
// compaction. A synthetic assistant message containing the summary is
// injected before any post-compaction turns, attributed to the
// summarizer model so its provenance is preserved.
func (l *Loop) LoadHistoryWithSummary(summary string, summarizer coremodel.ModelRef, turns [][]message.Message) {
	l.loadHistoryWithSummary(summary, summarizer, turns, true)
}

// LoadHistoryWithSummaryPreservePending restores compacted history without
// dropping pending async model input. Used by compaction in the active turn.
func (l *Loop) LoadHistoryWithSummaryPreservePending(summary string, summarizer coremodel.ModelRef, turns [][]message.Message) {
	l.loadHistoryWithSummary(summary, summarizer, turns, false)
}

func (l *Loop) loadHistoryWithSummary(summary string, summarizer coremodel.ModelRef, turns [][]message.Message, clearSignals bool) {
	l.resetHistory(clearSignals)
	summaryMsg := message.NewText(message.RoleAssistant, "[Previous conversation summary]\n\n"+summary+"\n\n[End of summary. Continue from here.]")
	summaryMsg.Source = summarizer
	l.messages = append(l.messages, summaryMsg)
	for _, turn := range turns {
		turn = filterHistoryTurn(turn)
		if len(turn) == 0 {
			continue
		}
		l.turnBoundaries = append(l.turnBoundaries, len(l.messages))
		l.messages = append(l.messages, turn...)
	}
}

// LoadHistoryWithSummaryAndActiveTail compacts completed prior turns into a
// summary while preserving the active in-progress turn tail byte-for-byte.
// It returns the new message index where the active turn starts.
func (l *Loop) LoadHistoryWithSummaryAndActiveTail(summary string, summarizer coremodel.ModelRef, activeTurnStart int) int {
	if activeTurnStart < 0 {
		activeTurnStart = 0
	}
	if activeTurnStart > len(l.messages) {
		activeTurnStart = len(l.messages)
	}
	tail := append([]message.Message(nil), l.messages[activeTurnStart:]...)
	l.loadHistoryWithSummary(summary, summarizer, nil, false)
	newActiveTurnStart := len(l.messages)
	if len(tail) > 0 {
		l.turnBoundaries = append(l.turnBoundaries, newActiveTurnStart)
		l.messages = append(l.messages, tail...)
	}
	return newActiveTurnStart
}

// persistMessage serializes msg and appends it to the current turn's
// messages.jsonl via the store. Errors are traced but do not fail the
// turn — persistence is best-effort so model interaction never stalls
// on disk issues.
func (l *Loop) persistMessage(turn int, msg message.Message) {
	if l.store == nil {
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		fmt.Fprintf(l.trace, "  !! persist marshal: %v\n", err)
		return
	}
	if err := l.store.AppendMessage(turn, data); err != nil {
		fmt.Fprintf(l.trace, "  !! persist append: %v\n", err)
	}
}

// Run runs one full user turn to completion, returning the final
// assistant text. Conversation history is preserved across turns.
// If ctx is cancelled mid-stream, any non-empty in-flight assistant
// message is persisted, then an interrupted marker is recorded. Run
// returns cleanly (no error), because the cancel is a user action, not
// a failure.
func (l *Loop) Run(ctx context.Context, userInputs ...string) (string, error) {
	turn := 0
	if l.store != nil {
		turn = l.store.CurrentTurn()
	}

	// Fresh pending queue per turn.
	l.pendingQueue = tool.NewPendingQueue()
	l.consumedFlushResults = nil

	activeTurnStart := len(l.messages)
	l.turnBoundaries = append(l.turnBoundaries, activeTurnStart)
	l.DrainPendingSignalsForModel(turn)
	for _, input := range userInputs {
		l.persistUserMessage(turn, input)
	}
	if l.store != nil {
		_ = l.store.TouchActivity()
	}
	defer func() {
		if ctx.Err() != nil {
			l.EnsureInterruptedSignal(turn)
		}
		if l.store != nil && turn > 0 {
			_ = l.store.MarkTurnComplete(turn)
		}
	}()

	userDisplaysEmitted := false
	emitDeferredUserDisplays := func() {
		if userDisplaysEmitted {
			return
		}
		for _, input := range userInputs {
			l.emitUserMessageDisplay(turn, input)
		}
		userDisplaysEmitted = true
	}

	for iter := 0; iter < maxIterations; iter++ {
		if iter > 0 {
			l.DrainPendingSignalsForModel(turn)
		}
		if result, err := l.beforeModelRequest(ctx, turn, activeTurnStart); err != nil {
			emitDeferredUserDisplays()
			return "", err
		} else if result.Transformed {
			activeTurnStart = result.ActiveTurnStart
		}
		if iter == 0 {
			emitDeferredUserDisplays()
		}
		l.DrainPendingSignalsForModel(turn)
		msg, cancelled, err := l.runStream(ctx)
		if err != nil {
			return "", fmt.Errorf("chat completion: %w", err)
		}
		msg = l.normalizeAssistantToolCalls(msg)
		if cancelled {
			msg.ToolCalls = nil
			if assistantMessageHasPayload(msg) {
				l.messages = append(l.messages, msg)
				l.persistMessage(turn, msg)
			}
			l.pendingQueue.Discard()
			l.EnsureInterruptedSignal(turn)
			return assistantVisibleText(msg), nil
		}
		l.messages = append(l.messages, msg)
		l.persistMessage(turn, msg)

		if len(msg.ToolCalls) == 0 {
			if l.pendingQueue.Len() > 0 {
				results, err := l.flushPendingAtTurnEnd(ctx)
				if err != nil {
					return "", fmt.Errorf("flush pending at turn end: %w", err)
				}
				l.appendStagedFlushWrapper(turn, results)
			}
			if l.HasPendingWakeSignal() {
				continue
			}
			return assistantVisibleText(msg), nil
		}

		denied := false
		for _, tc := range msg.ToolCalls {
			result := l.dispatch(ctx, tc)
			toolMsg := message.NewText(message.RoleTool, result.Content)
			toolMsg.ToolCallID = tc.ID
			toolMsg.Name = tc.Function.Name
			if len(result.Metadata) > 0 {
				toolMsg.DisplayMetadata = result.Metadata
			}
			if result.IsError {
				MarkToolResultError(&toolMsg)
			}
			l.messages = append(l.messages, toolMsg)
			l.persistMessage(turn, toolMsg)
			if result.Denied {
				denied = true
				break
			}
		}
		// Persist the <staged-flush> wrapper for any flushes triggered during
		// this assistant message's dispatch, BEFORE the denied early-return:
		// a flush-before-a-denied-tool already emitted the real staged results
		// live, so the wrapper must reach history or reload would stay "Staged.".
		if len(l.consumedFlushResults) > 0 {
			l.appendStagedFlushWrapper(turn, l.consumedFlushResults)
			l.consumedFlushResults = nil
		}
		if denied {
			return "Tool denied by user.", nil
		}
	}

	return "", fmt.Errorf("agent loop exceeded %d iterations without a final text response; the model may be stuck in a tool-call cycle", maxIterations)
}

func (l *Loop) beforeModelRequest(ctx context.Context, turn, activeTurnStart int) (ContextTransformResult, error) {
	if l == nil || l.contextTransformer == nil {
		return ContextTransformResult{}, nil
	}
	return l.contextTransformer.BeforeModelRequest(ctx, ContextTransformCheckpoint{
		Turn:            turn,
		ActiveTurnStart: activeTurnStart,
	})
}

// runStream performs one streaming chat completion. Returns (msg,
// cancelled, err). On context cancellation it returns whatever partial
// text + tool deltas have accumulated with cancelled=true and err=nil,
// so the caller can persist the partial and exit the turn gracefully.
func isRetryable(err error) bool {
	var retryable interface {
		Retryable() bool
	}
	if errors.As(err, &retryable) {
		return retryable.Retryable()
	}
	return false
}

func (l *Loop) runStream(ctx context.Context) (message.Message, bool, error) {
	if l.client == nil {
		return message.Message{}, false, ErrNoModelConfigured
	}
	l.emitProtocolWarnings(l.client.ProtocolWarnings(l.messages))
	const maxRetries = 3
	backoff := 2 * time.Second

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return message.Message{Role: message.RoleAssistant}, true, nil
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		stream, err := l.client.ChatStream(ctx, modelclient.ChatRequest{
			Messages: l.messages,
			Tools:    l.registry.OpenAITools(),
		})
		if err != nil {
			if ctx.Err() != nil {
				return message.Message{Role: message.RoleAssistant}, true, nil
			}
			if isRetryable(err) && attempt < maxRetries {
				lastErr = err
				continue
			}
			return message.Message{}, false, err
		}
		msg, cancelled, err := l.consumeStream(ctx, stream)
		_ = stream.Close()
		if err != nil && isRetryable(err) && attempt < maxRetries {
			lastErr = err
			continue
		}
		return msg, cancelled, err
	}
	return message.Message{}, false, lastErr
}

func (l *Loop) emitProtocolWarnings(warnings []modelclient.ProtocolWarning) {
	for _, warning := range warnings {
		l.emit(Event{
			Kind:   Warning,
			Result: warning.Message,
			Metadata: map[string]any{
				"kind":          warning.Kind,
				"provider":      warning.Provider,
				"model":         warning.Model,
				"field":         warning.Field,
				"message_index": warning.MessageIndex,
			},
		})
	}
}

func (l *Loop) consumeStream(ctx context.Context, stream modelclient.ChatStream) (message.Message, bool, error) {

	var (
		contentBuf   strings.Builder
		contentParts []message.ContentPart
		refusalBuf   strings.Builder
		toolDeltas   map[int]*message.ToolCall
		toolIDs      map[string]int
		msgExtra     = message.NewExtraAccumulator()
		toolExtra    map[int]*message.ExtraAccumulator
		nextToolIdx  int
		lastToolIdx  = -1
		role         string
		finishReason openai.FinishReason
		usage        *openai.Usage
		cancelled    bool
		sawChoice    bool
	)

	for {
		delta, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var parseErr modelclient.ChunkParseError
			if errors.As(err, &parseErr) {
				fmt.Fprintf(l.trace, "  !! protocol chunk parse: %v\n", parseErr.Err)
				continue
			}
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				cancelled = true
				break
			}
			return message.Message{}, false, fmt.Errorf("stream recv: %w", err)
		}
		if delta.Usage != nil {
			usage = delta.Usage
		}
		if !delta.HasChoice {
			continue
		}
		sawChoice = true
		if delta.FinishReason != "" && delta.FinishReason != openai.FinishReasonNull {
			finishReason = delta.FinishReason
		}

		if delta.Role != "" {
			role = delta.Role
		}
		if delta.Content != "" {
			contentBuf.WriteString(delta.Content)
			l.emit(Event{Kind: TextDelta, Result: delta.Content})
		}
		for _, part := range delta.ContentParts {
			contentParts = append(contentParts, part)
			if part.Type == message.ContentPartText && part.Text != "" {
				l.emit(Event{Kind: TextDelta, Result: part.Text})
			}
		}
		if delta.Refusal != "" {
			refusalBuf.WriteString(delta.Refusal)
			l.emit(Event{Kind: TextDelta, Result: delta.Refusal})
		}
		for key, value := range delta.MessageExtra {
			if err := msgExtra.Add(key, value); err != nil {
				fmt.Fprintf(l.trace, "  !! protocol message extra: %v\n", err)
			}
		}
		for pos, tc := range delta.ToolCalls {
			if toolDeltas == nil {
				toolDeltas = make(map[int]*message.ToolCall)
			}
			idx := -1
			if tc.ID == "" && tc.Function.Name == "" {
				if len(delta.ToolCalls) == 1 && lastToolIdx >= 0 {
					idx = lastToolIdx
				} else if _, exists := toolDeltas[pos]; exists {
					idx = pos
				} else if lastToolIdx >= 0 {
					idx = lastToolIdx
				}
			} else if tc.Index != nil {
				candidate := *tc.Index
				if existing, ok := toolDeltas[candidate]; !ok || existing.ID == "" || existing.ID == tc.ID {
					idx = candidate
				}
			} else if tc.ID != "" && toolIDs != nil {
				if existing, ok := toolIDs[tc.ID]; ok {
					idx = existing
				}
			}
			if idx < 0 {
				for {
					if _, exists := toolDeltas[nextToolIdx]; !exists {
						idx = nextToolIdx
						nextToolIdx++
						break
					}
					nextToolIdx++
				}
			}
			if tc.ID != "" {
				if toolIDs == nil {
					toolIDs = make(map[string]int)
				}
				toolIDs[tc.ID] = idx
			}
			lastToolIdx = idx
			extraKey := pos
			if tc.Index != nil && !(tc.ID == "" && tc.Function.Name == "") {
				extraKey = idx
			}
			if extra := delta.ToolCallExtra[extraKey]; len(extra) > 0 {
				if toolExtra == nil {
					toolExtra = map[int]*message.ExtraAccumulator{}
				}
				acc := toolExtra[idx]
				if acc == nil {
					acc = message.NewExtraAccumulator()
					toolExtra[idx] = acc
				}
				for key, value := range extra {
					if err := acc.Add(key, value); err != nil {
						fmt.Fprintf(l.trace, "  !! protocol tool extra: %v\n", err)
					}
				}
			}
			entry, ok := toolDeltas[idx]
			if !ok {
				entry = &message.ToolCall{Type: string(tc.Type)}
				toolDeltas[idx] = entry
			}
			if tc.ID != "" {
				entry.ID = tc.ID
			}
			if tc.Function.Name != "" {
				entry.Function.Name += tc.Function.Name
			}
			entry.Function.Arguments += tc.Function.Arguments
		}
	}

	if usage != nil {
		cached := 0
		if usage.PromptTokensDetails != nil {
			cached = usage.PromptTokensDetails.CachedTokens
		}
		input := usage.PromptTokens - cached
		if input < 0 {
			input = 0
		}
		l.emit(Event{
			Kind:       Usage,
			Model:      l.client.Model(),
			ModelRef:   l.client.ModelRef(),
			UsageKnown: true,
			Cache:      cached,
			Input:      input,
			Output:     usage.CompletionTokens,
		})
	}

	if role == "" {
		role = string(message.RoleAssistant)
	}
	content := contentBuf.String()
	msg := message.Message{Role: message.Role(role), Refusal: refusalBuf.String()}
	if l.client != nil {
		msg.Source = l.client.ModelRef()
	}
	msg.AppendText(content)
	if len(contentParts) > 0 {
		msg.Content = append(msg.Content, contentParts...)
	}
	msg.Extra = msgExtra.Extra()
	if len(toolDeltas) > 0 && !cancelled {
		indices := make([]int, 0, len(toolDeltas))
		for idx := range toolDeltas {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		calls := make([]message.ToolCall, 0, len(indices))
		for _, idx := range indices {
			if tc := toolDeltas[idx]; tc != nil {
				if acc := toolExtra[idx]; acc != nil {
					tc.Extra = acc.Extra()
				}
				calls = append(calls, *tc)
			}
		}
		msg.ToolCalls = calls
	}
	if !cancelled && !assistantMessageHasPayload(msg) {
		return msg, false, emptyAssistantResponseError(finishReason, sawChoice)
	}
	return msg, cancelled, nil
}

type dispatchResult struct {
	Content  string
	Denied   bool
	IsError  bool
	Metadata map[string]any
}

// dispatch executes one tool call and returns the result string plus display
// state that must be persisted with the tool response.
func (l *Loop) dispatch(ctx context.Context, tc message.ToolCall) dispatchResult {
	tc = l.normalizeToolCall(tc)
	fmt.Fprintf(l.trace, "  → %s %s\n", tc.Function.Name, truncate(tc.Function.Arguments, traceMaxChars))
	l.emit(Event{Kind: ToolCallStart, ToolCallID: tc.ID, ToolName: tc.Function.Name, Args: tc.Function.Arguments})

	var params map[string]any
	parseErr := json.Unmarshal([]byte(tc.Function.Arguments), &params)

	finish := func(result string, isError bool, metadataArgs string) dispatchResult {
		fmt.Fprintf(l.trace, "  ← %s\n", truncate(result, traceMaxChars))
		ev := Event{Kind: ToolCallEnd, ToolCallID: tc.ID, ToolName: tc.Function.Name, Args: tc.Function.Arguments, Result: result, IsError: isError}
		var metadata map[string]any
		if !isError {
			metadata = l.displayMetadata(ctx, tc.Function.Name, metadataArgs, result)
			ev.Metadata = metadata
		}
		l.emit(ev)
		return dispatchResult{Content: result, IsError: isError, Metadata: metadata}
	}

	if parseErr != nil {
		return finish(fmt.Sprintf("error: invalid JSON arguments: %v", parseErr), true, tc.Function.Arguments)
	}

	metadataArgs := tc.Function.Arguments

	if coordinator, ok := l.pendingCoordinatorForTool(tc.Function.Name); ok {
		act, _ := params["action"].(string)
		if act == "discard" {
			l.pendingQueue.Discard()
			return finish("Discarded all staged edits.", false, metadataArgs)
		}
		results, err := l.flushPendingQueue(ctx, coordinator)
		if err != nil {
			return finish("error: "+err.Error(), true, metadataArgs)
		}
		if len(results) == 0 {
			return finish("No pending edits to execute.", false, metadataArgs)
		}
		// The per-staged ToolCallEnd events (emitted by flushPendingQueue)
		// carry the real results live; the <staged-flush> wrapper (persisted
		// from consumedFlushResults after the dispatch loop) reconstructs them
		// on reload. This tool's own result is a short summary.
		l.consumedFlushResults = append(l.consumedFlushResults, results...)
		summary, isError := stagedFlushSummary(results)
		return finish(summary, isError, metadataArgs)
	}

	t, ok := l.registry.Get(tc.Function.Name)
	if !ok {
		return finish(fmt.Sprintf("error: unknown tool %q", tc.Function.Name), true, metadataArgs)
	}

	params["_tool_call_id"] = tc.ID
	if data, err := json.Marshal(params); err == nil {
		metadataArgs = string(data)
	}

	pending, _ := params["pending"].(bool)
	if pending {
		stageable, ok := l.stageableTool(tc.Function.Name)
		if ok {
			if err := stageable.ValidateStaged(ctx, json.RawMessage(tc.Function.Arguments)); err != nil {
				return finish(err.Error(), true, metadataArgs)
			}
			l.pendingQueue.Stage(tool.StagedCall{
				ToolName:   tc.Function.Name,
				ToolCallID: tc.ID,
				Args:       tc.Function.Arguments,
				Params:     params,
			})
			return finish(stageable.StagedResultMessage(), false, metadataArgs)
		}
	}

	if results, flushed, err := l.autoFlushBefore(ctx, tc); err != nil {
		return finish("error: "+err.Error(), true, metadataArgs)
	} else if flushed {
		l.consumedFlushResults = append(l.consumedFlushResults, results...)
	}

	result, err := t.Execute(ctx, params)
	if err != nil {
		if errors.Is(err, tool.ErrDenied) {
			result := finish("denied by user", true, metadataArgs)
			result.Denied = true
			return result
		}
		var exitErr *tool.ExitError
		if errors.As(err, &exitErr) {
			return finish(exitErr.Output, true, metadataArgs)
		}
		return finish("error: "+err.Error(), true, metadataArgs)
	}
	return finish(result, false, metadataArgs)
}

func (l *Loop) stageableTool(name string) (tool.StageableTool, bool) {
	if l == nil || l.registry == nil {
		return nil, false
	}
	return l.registry.StageableTool(name)
}

func (l *Loop) pendingCoordinator() tool.PendingCoordinator {
	if l != nil && l.registry != nil {
		if coordinator, ok := l.registry.PendingCoordinator(); ok {
			return coordinator
		}
	}
	return tool.NewPendingCoordinator("", l.pendingExecutorOrFallback())
}

func (l *Loop) pendingCoordinatorForTool(name string) (tool.PendingCoordinator, bool) {
	coordinator := l.pendingCoordinator()
	if coordinator == nil || coordinator.ExplicitFlushToolName() != name {
		return nil, false
	}
	return coordinator, true
}

func (l *Loop) pendingExecutorOrFallback() tool.PendingExecutor {
	if l.pendingExecutor != nil {
		return l.pendingExecutor
	}
	return loopPendingExecutor{loop: l}
}

type loopPendingExecutor struct {
	loop *Loop
}

func (e loopPendingExecutor) ExecutePending(ctx context.Context, staged []tool.StagedCall) []tool.BatchResult {
	if e.loop == nil {
		return nil
	}
	return e.loop.executePendingSequential(ctx, staged)
}

func (l *Loop) autoFlushBefore(ctx context.Context, tc message.ToolCall) ([]tool.BatchResult, bool, error) {
	if l.pendingQueue == nil || l.pendingQueue.Len() == 0 {
		return nil, false, nil
	}
	coordinator := l.pendingCoordinator()
	if coordinator == nil {
		return nil, false, nil
	}
	staged := l.pendingQueue.Staged()
	results, flushed, err := coordinator.AutoFlushBefore(ctx, tool.ToolCall{
		Name:      tc.Function.Name,
		ID:        tc.ID,
		Arguments: json.RawMessage(tc.Function.Arguments),
	}, l.pendingQueue)
	if err != nil || !flushed {
		return results, flushed, err
	}
	l.emitPendingResults(ctx, results, staged)
	return results, true, nil
}

func (l *Loop) flushPendingAtTurnEnd(ctx context.Context) ([]tool.BatchResult, error) {
	if l.pendingQueue == nil || l.pendingQueue.Len() == 0 {
		return nil, nil
	}
	staged := l.pendingQueue.Staged()
	results, flushed, err := l.pendingCoordinator().FlushAtTurnEnd(ctx, l.pendingQueue)
	if err != nil || !flushed {
		return results, err
	}
	l.emitPendingResults(ctx, results, staged)
	return results, nil
}

func (l *Loop) displayMetadata(ctx context.Context, name, args, result string) map[string]any {
	if l == nil || l.registry == nil {
		return nil
	}
	provider, ok := l.registry.DisplayMetadataProvider(name)
	if !ok {
		return nil
	}
	return provider.DisplayMetadata(ctx, json.RawMessage(args), result)
}

func (l *Loop) flushPendingQueue(ctx context.Context, coordinator tool.PendingCoordinator) ([]tool.BatchResult, error) {
	if coordinator == nil {
		coordinator = l.pendingCoordinator()
	}
	if l.pendingQueue == nil || l.pendingQueue.Len() == 0 {
		return nil, nil
	}
	staged := l.pendingQueue.Staged()
	results, flushed, err := coordinator.FlushPending(ctx, l.pendingQueue)
	if err != nil || !flushed {
		return results, err
	}
	l.emitPendingResults(ctx, results, staged)
	return results, nil
}

func (l *Loop) emitPendingResults(ctx context.Context, results []tool.BatchResult, staged []tool.StagedCall) {
	stagedByID := make(map[string]tool.StagedCall, len(staged))
	for _, call := range staged {
		stagedByID[call.ToolCallID] = call
	}

	for i := range results {
		r := &results[i]
		result := r.Result
		isError := false
		if r.Error != "" {
			result = r.Error
			isError = true
		}
		stagedCall := stagedByID[r.ToolCallID]
		metadata := r.Metadata
		if !isError {
			if metadata == nil {
				metadata = l.displayMetadata(ctx, r.ToolName, stagedCall.Args, result)
			}
			r.Metadata = metadata
		} else {
			r.Metadata = nil
		}
		l.emit(Event{
			Kind:       ToolCallEnd,
			ToolCallID: r.ToolCallID,
			ToolName:   r.ToolName,
			Args:       stagedCall.Args,
			Result:     result,
			IsError:    isError,
			Metadata:   metadata,
		})
	}
}

func stagedFlushSummary(results []tool.BatchResult) (string, bool) {
	failed := 0
	for _, r := range results {
		if r.Error != "" {
			failed++
		}
	}
	applied := len(results) - failed
	if failed == 0 {
		return fmt.Sprintf("Applied %d staged edits.", applied), false
	}
	if applied == 0 {
		return fmt.Sprintf("Failed to apply %d staged edits.", failed), true
	}
	return fmt.Sprintf("Applied %d staged edits; %d failed.", applied, failed), true
}

const (
	stagedFlushOpen         = "<staged-flush>"
	stagedFlushClose        = "</staged-flush>"
	stagedFlushInternalKind = "staged_flush"
)

// StagedFlushEntry is one staged tool's final result, carried in the
// <staged-flush> reload wrapper.
type StagedFlushEntry struct {
	ID       string         `json:"id"`
	Result   string         `json:"result"`
	IsError  bool           `json:"isError"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type stagedFlushPayload struct {
	Results []StagedFlushEntry `json:"results"`
}

// BuildStagedFlush renders the <staged-flush> wrapper content for the given
// flush results, mirroring the live ToolCallEnd semantics (error string wins,
// IsError set when there was an error). Returns "" when there are no results.
func BuildStagedFlush(results []tool.BatchResult) string {
	if len(results) == 0 {
		return ""
	}
	payload := stagedFlushPayload{Results: make([]StagedFlushEntry, 0, len(results))}
	for _, r := range results {
		e := StagedFlushEntry{ID: r.ToolCallID, Result: r.Result, Metadata: r.Metadata}
		if r.Error != "" {
			e.Result = r.Error
			e.IsError = true
			e.Metadata = nil
		}
		payload.Results = append(payload.Results, e)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return stagedFlushOpen + string(data) + stagedFlushClose
}

// NewStagedFlushMessage renders a backend-owned staged-flush wrapper message.
// The private marker distinguishes internal wrappers from literal user input
// with the same text.
func NewStagedFlushMessage(results []tool.BatchResult) (message.Message, bool) {
	wrapper := BuildStagedFlush(results)
	if wrapper == "" {
		return message.Message{}, false
	}
	msg := message.NewText(message.RoleUser, wrapper)
	msg.InternalKind = stagedFlushInternalKind
	return msg, true
}

// ParseStagedFlush reports whether trusted content is a <staged-flush> wrapper
// and, if so, returns its entries. The bool is true for any content bounded by
// the markers, including malformed inner JSON where entries is nil.
func ParseStagedFlush(content string) (entries []StagedFlushEntry, isWrapper bool) {
	if !strings.HasPrefix(content, stagedFlushOpen) || !strings.HasSuffix(content, stagedFlushClose) {
		return nil, false
	}
	inner := content[len(stagedFlushOpen) : len(content)-len(stagedFlushClose)]
	var payload stagedFlushPayload
	if json.Unmarshal([]byte(inner), &payload) != nil {
		return nil, true
	}
	return payload.Results, true
}

// ParseStagedFlushMessage parses only backend-marked staged-flush messages.
func ParseStagedFlushMessage(msg message.Message) (entries []StagedFlushEntry, isWrapper bool) {
	if msg.Role != message.RoleUser || msg.InternalKind != stagedFlushInternalKind {
		return nil, false
	}
	return ParseStagedFlush(msg.TextContent())
}

// appendStagedFlushWrapper persists exactly one <staged-flush> wrapper for the
// given flush results: it appends a RoleUser message to in-memory history AND
// persists it. The in-memory append is required so the running loop includes
// the structured results in its next provider request (not only after reload).
// No display event is emitted — the per-tool ToolCallEnd events already carried
// the results to the live display; the wrapper is for reload + model context.
func (l *Loop) appendStagedFlushWrapper(turn int, results []tool.BatchResult) {
	msg, ok := NewStagedFlushMessage(results)
	if !ok {
		return
	}
	l.messages = append(l.messages, msg)
	l.persistMessage(turn, msg)
}

func (l *Loop) executePendingSequential(ctx context.Context, staged []tool.StagedCall) []tool.BatchResult {
	results := make([]tool.BatchResult, 0, len(staged))
	for _, call := range staged {
		r := tool.BatchResult{ToolName: call.ToolName, ToolCallID: call.ToolCallID}
		t, ok := l.registry.Get(call.ToolName)
		if !ok {
			r.Error = fmt.Sprintf("unknown tool %q", call.ToolName)
			results = append(results, r)
			continue
		}
		result, err := t.Execute(ctx, call.Params)
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Success = true
			r.Result = result
		}
		results = append(results, r)
	}
	return results
}

// TurnCount returns the number of completed user turns in this session.
func (l *Loop) TurnCount() int { return len(l.turnBoundaries) }

// TruncateHistory drops every message from turn keepThrough+1 onward.
func (l *Loop) TruncateHistory(keepThrough int) error {
	if keepThrough < 0 || keepThrough > len(l.turnBoundaries) {
		return fmt.Errorf("truncate history: keepThrough %d out of range [0, %d]", keepThrough, len(l.turnBoundaries))
	}
	if keepThrough == len(l.turnBoundaries) {
		return nil
	}
	cut := l.turnBoundaries[keepThrough]
	l.messages = l.messages[:cut]
	l.turnBoundaries = l.turnBoundaries[:keepThrough]
	return nil
}

func truncate(s string, max int) string {
	flat := strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(flat) <= max {
		return flat
	}
	return flat[:max] + fmt.Sprintf("... (%d bytes total)", len(s))
}
