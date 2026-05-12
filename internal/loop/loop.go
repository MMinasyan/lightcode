// Package loop implements the agentic loop: user message → model →
// text or tool calls → execute → feed result back → repeat until the
// model returns a text-only response.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/MMinasyan/lightcode/internal/editpreview"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/tool"
)

// maxIterations caps how many model→tool→model rounds a single user turn
// may perform before the loop gives up. Prevents runaway tool-call cycles.
const maxIterations = 25

// traceMaxChars is the length at which tool call arguments and results
// are truncated when written to the trace. Keeps the REPL readable when
// the agent reads large files.
const traceMaxChars = 200

// interruptedSignal is injected as a user-role message when the user
// cancels a turn. User-role works across all OpenAI-compatible providers.
const interruptedSignal = "<system-signal>Request interrupted by user</system-signal>"

// Store is the minimum surface the loop needs from the snapshot
// package: turn-scoped message persistence, turn completion, and
// activity touch. Declared here so loop has no import dependency on
// internal/snapshot. app.go wires the concrete *snapshot.Store.
type Store interface {
	AppendMessage(turn int, msg []byte) error
	MarkTurnComplete(turn int) error
	TouchActivity() error
	CurrentTurn() int
}

// PendingExecutor applies staged edit/write calls at flush time.
type PendingExecutor interface {
	ExecutePending(ctx context.Context, staged []tool.StagedCall) []tool.BatchResult
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
	Cache      int
	Input      int
	Output     int
	UsageKnown bool
}

// Loop owns the conversation history for a single session and drives
// the agentic loop on each user turn.
type Loop struct {
	client   *provider.Client
	registry *tool.Registry
	messages []openai.ChatCompletionMessage

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

	// pendingQueue holds staged edit_file/write_file calls for
	// batch execution. Created fresh per turn.
	pendingQueue *tool.PendingQueue

	pendingExecutor PendingExecutor
}

// New returns a Loop pre-seeded with the system prompt.
func New(client *provider.Client, registry *tool.Registry, systemPrompt string) *Loop {
	return &Loop{
		client:   client,
		registry: registry,
		messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
		},
		trace: io.Discard,
	}
}

// SetClient replaces the provider client used for subsequent turns.
func (l *Loop) SetClient(c *provider.Client) { l.client = c }

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

// SetPendingExecutor configures the executor used to flush staged edits.
func (l *Loop) SetPendingExecutor(exec PendingExecutor) { l.pendingExecutor = exec }

func (l *Loop) emit(ev Event) {
	if l.events == nil {
		return
	}
	select {
	case l.events <- ev:
	default:
	}
}

// Messages returns the current in-memory conversation, including the
// system prompt at index 0. Callers must not mutate the returned slice.
func (l *Loop) Messages() []openai.ChatCompletionMessage { return l.messages }

func assistantMessageHasPayload(msg openai.ChatCompletionMessage) bool {
	return msg.Content != "" || len(msg.MultiContent) > 0 || len(msg.ToolCalls) > 0
}

func normalizeAssistantToolCalls(msg openai.ChatCompletionMessage) openai.ChatCompletionMessage {
	for i := range msg.ToolCalls {
		msg.ToolCalls[i] = normalizeToolCall(msg.ToolCalls[i])
	}
	return msg
}

func normalizeToolCall(tc openai.ToolCall) openai.ToolCall {
	tc.Function.Arguments = normalizeToolCallArgs(tc.Function.Name, tc.Function.Arguments)
	return tc
}

func normalizeToolCallArgs(name, args string) string {
	if name != "sleep" {
		return args
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return args
	}
	v, ok := params["seconds"].(float64)
	if !ok {
		return args
	}
	sec := int(v)
	if sec < 1 {
		sec = 1
	}
	if sec > 300 {
		sec = 300
	}
	if float64(sec) == v {
		return args
	}
	params["seconds"] = sec
	data, err := json.Marshal(params)
	if err != nil {
		return args
	}
	return string(data)
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

func filterHistoryTurn(turn []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(turn))
	for _, msg := range turn {
		if msg.Role == openai.ChatMessageRoleAssistant && !assistantMessageHasPayload(msg) {
			continue
		}
		out = append(out, msg)
	}
	return out
}

// UpdateSystemPrompt replaces the system prompt (messages[0]).
func (l *Loop) UpdateSystemPrompt(content string) {
	if len(l.messages) > 0 && l.messages[0].Role == openai.ChatMessageRoleSystem {
		l.messages[0].Content = content
	}
}

// AppendUserMessage adds a user message to the conversation and persists
// it under the given turn. Does not run the model.
func (l *Loop) AppendUserMessage(turn int, content string) {
	l.turnBoundaries = append(l.turnBoundaries, len(l.messages))
	msg := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: content,
	}
	l.messages = append(l.messages, msg)
	l.persistMessage(turn, msg)
}

// AppendSignal appends a user-role system signal message to the
// conversation history. Not persisted and not counted as a turn boundary.
func (l *Loop) AppendSignal(content string) {
	l.messages = append(l.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: content,
	})
}

// ResetHistory drops all messages and turn boundaries, leaving only
// the system prompt. Used when switching sessions.
func (l *Loop) ResetHistory() {
	if len(l.messages) > 0 && l.messages[0].Role == openai.ChatMessageRoleSystem {
		l.messages = l.messages[:1]
	} else {
		l.messages = nil
	}
	l.turnBoundaries = nil
}

// LoadHistory restores a conversation from persisted turns. Each
// turn's messages are appended in order. The first message of each
// turn is assumed to be the user message (defines the turn boundary).
// The existing system prompt is preserved.
func (l *Loop) LoadHistory(turns [][]openai.ChatCompletionMessage) {
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
// compaction. A synthetic user message containing the summary is
// injected before any post-compaction turns.
func (l *Loop) LoadHistoryWithSummary(summary string, turns [][]openai.ChatCompletionMessage) {
	l.ResetHistory()
	summaryMsg := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: "[Previous conversation summary]\n\n" + summary + "\n\n[End of summary. Continue from here.]",
	}
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

// persistMessage serializes msg and appends it to the current turn's
// messages.jsonl via the store. Errors are traced but do not fail the
// turn — persistence is best-effort so model interaction never stalls
// on disk issues.
func (l *Loop) persistMessage(turn int, msg openai.ChatCompletionMessage) {
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

	l.turnBoundaries = append(l.turnBoundaries, len(l.messages))
	for _, input := range userInputs {
		userMsg := openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: input,
		}
		l.messages = append(l.messages, userMsg)
		l.persistMessage(turn, userMsg)
	}
	if l.store != nil {
		_ = l.store.TouchActivity()
	}
	defer func() {
		if l.store != nil && turn > 0 {
			_ = l.store.MarkTurnComplete(turn)
		}
	}()

	for iter := 0; iter < maxIterations; iter++ {
		msg, cancelled, err := l.runStream(ctx)
		if err != nil {
			return "", fmt.Errorf("chat completion: %w", err)
		}
		msg = normalizeAssistantToolCalls(msg)
		if cancelled {
			msg.ToolCalls = nil
			if assistantMessageHasPayload(msg) {
				l.messages = append(l.messages, msg)
				l.persistMessage(turn, msg)
			}
			l.pendingQueue.Discard()
			signalMsg := openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: interruptedSignal,
			}
			l.messages = append(l.messages, signalMsg)
			l.persistMessage(turn, signalMsg)
			return msg.Content, nil
		}
		l.messages = append(l.messages, msg)
		l.persistMessage(turn, msg)

		if len(msg.ToolCalls) == 0 {
			// Auto-flush pending edits at turn end.
			if l.pendingQueue.Len() > 0 {
				result := l.flushPendingQueue(ctx)
				toolMsg := openai.ChatCompletionMessage{
					Role:    openai.ChatMessageRoleUser,
					Content: result,
				}
				l.messages = append(l.messages, toolMsg)
				l.persistMessage(turn, toolMsg)
			}
			return msg.Content, nil
		}

		denied := false
		for _, tc := range msg.ToolCalls {
			result, d := l.dispatch(ctx, tc)
			toolMsg := openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			}
			l.messages = append(l.messages, toolMsg)
			l.persistMessage(turn, toolMsg)
			if d {
				denied = true
				break
			}
		}
		if denied {
			return "Tool denied by user.", nil
		}
	}

	return "", fmt.Errorf("agent loop exceeded %d iterations without a final text response; the model may be stuck in a tool-call cycle", maxIterations)
}

// runStream performs one streaming chat completion. Returns (msg,
// cancelled, err). On context cancellation it returns whatever partial
// text + tool deltas have accumulated with cancelled=true and err=nil,
// so the caller can persist the partial and exit the turn gracefully.
func isRetryable(err error) bool {
	var statusErr *provider.HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Retryable()
	}
	return false
}

func (l *Loop) runStream(ctx context.Context) (openai.ChatCompletionMessage, bool, error) {
	if l.client == nil {
		return openai.ChatCompletionMessage{}, false, fmt.Errorf("no model configured — set default_model in ~/.lightcode/config.json and an API key in ~/.lightcode/.env")
	}
	const maxRetries = 3
	backoff := 2 * time.Second

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant}, true, nil
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		var stream *provider.Stream
		var err error
		stream, err = l.client.ChatStream(ctx, l.messages, l.registry.OpenAITools(), nil)
		if err != nil {
			if ctx.Err() != nil {
				return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant}, true, nil
			}
			if isRetryable(err) && attempt < maxRetries {
				lastErr = err
				continue
			}
			return openai.ChatCompletionMessage{}, false, err
		}
		msg, cancelled, err := l.consumeStream(ctx, stream)
		stream.Close()
		if err != nil && isRetryable(err) && attempt < maxRetries {
			lastErr = err
			continue
		}
		return msg, cancelled, err
	}
	return openai.ChatCompletionMessage{}, false, lastErr
}

func (l *Loop) consumeStream(ctx context.Context, stream *provider.Stream) (openai.ChatCompletionMessage, bool, error) {

	var (
		contentBuf   strings.Builder
		refusalBuf   strings.Builder
		toolDeltas   map[int]*openai.ToolCall
		role         string
		finishReason openai.FinishReason
		usage        *openai.Usage
		cancelled    bool
		sawChoice    bool
	)

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				cancelled = true
				break
			}
			return openai.ChatCompletionMessage{}, false, fmt.Errorf("stream recv: %w", err)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		sawChoice = true
		if choice.FinishReason != "" && choice.FinishReason != openai.FinishReasonNull {
			finishReason = choice.FinishReason
		}
		delta := choice.Delta

		if delta.Role != "" {
			role = delta.Role
		}
		if delta.Content != "" {
			contentBuf.WriteString(delta.Content)
			l.emit(Event{Kind: TextDelta, Result: delta.Content})
		}
		if delta.Refusal != "" {
			refusalBuf.WriteString(delta.Refusal)
			l.emit(Event{Kind: TextDelta, Result: delta.Refusal})
		}
		for _, tc := range delta.ToolCalls {
			if tc.Index == nil {
				continue
			}
			idx := *tc.Index
			if toolDeltas == nil {
				toolDeltas = make(map[int]*openai.ToolCall)
			}
			entry, ok := toolDeltas[idx]
			if !ok {
				entry = &openai.ToolCall{Index: tc.Index, Type: tc.Type}
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
			UsageKnown: true,
			Cache:      cached,
			Input:      input,
			Output:     usage.CompletionTokens,
		})
	}

	if role == "" {
		role = openai.ChatMessageRoleAssistant
	}
	content := contentBuf.String()
	if content == "" && refusalBuf.Len() > 0 {
		content = refusalBuf.String()
	}
	msg := openai.ChatCompletionMessage{
		Role:    role,
		Content: content,
	}
	if len(toolDeltas) > 0 && !cancelled {
		indices := make([]int, 0, len(toolDeltas))
		for idx := range toolDeltas {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		calls := make([]openai.ToolCall, 0, len(indices))
		for _, idx := range indices {
			if tc := toolDeltas[idx]; tc != nil {
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

// dispatch executes one tool call and returns the result string plus a
// bool indicating whether the user denied the operation.
func (l *Loop) dispatch(ctx context.Context, tc openai.ToolCall) (string, bool) {
	tc = normalizeToolCall(tc)
	fmt.Fprintf(l.trace, "  → %s %s\n", tc.Function.Name, truncate(tc.Function.Arguments, traceMaxChars))
	l.emit(Event{Kind: ToolCallStart, ToolCallID: tc.ID, ToolName: tc.Function.Name, Args: tc.Function.Arguments})

	var params map[string]any
	parseErr := json.Unmarshal([]byte(tc.Function.Arguments), &params)

	finish := func(result string, isError bool) string {
		fmt.Fprintf(l.trace, "  ← %s\n", truncate(result, traceMaxChars))
		ev := Event{Kind: ToolCallEnd, ToolCallID: tc.ID, ToolName: tc.Function.Name, Args: tc.Function.Arguments, Result: result, IsError: isError}
		if !isError && tc.Function.Name == "edit_file" && params != nil {
			ev.Metadata = editpreview.MetadataFromParams(params, result)
		}
		l.emit(ev)
		return result
	}

	if parseErr != nil {
		return finish(fmt.Sprintf("error: invalid JSON arguments: %v", parseErr), true), false
	}

	// Handle execute_pending directly to use the current turn's queue.
	if tc.Function.Name == "execute_pending" {
		act, _ := params["action"].(string)
		if act == "discard" {
			l.pendingQueue.Discard()
			return finish("Discarded all staged edits.", false), false
		}
		result := l.flushPendingQueue(ctx)
		return finish(result, false), false
	}

	t, ok := l.registry.Get(tc.Function.Name)
	if !ok {
		return finish(fmt.Sprintf("error: unknown tool %q", tc.Function.Name), true), false
	}

	params["_tool_call_id"] = tc.ID

	// Check for pending flag.
	pending, _ := params["pending"].(bool)
	if pending && (tc.Function.Name == "edit_file" || tc.Function.Name == "write_file") {
		if err := validateStagedCall(tc.Function.Name, params); err != nil {
			return finish(err.Error(), true), false
		}
		l.pendingQueue.Stage(tool.StagedCall{
			ToolName:   tc.Function.Name,
			ToolCallID: tc.ID,
			Params:     params,
		})
		return finish("Staged.", false), false
	}

	// Before executing a non-pending tool, flush the queue.
	var prefix string
	if l.pendingQueue.Len() > 0 && tc.Function.Name != "execute_pending" {
		prefix = l.flushPendingQueue(ctx) + "\n\n"
	}

	result, err := t.Execute(ctx, params)
	if err != nil {
		if errors.Is(err, tool.ErrDenied) {
			return finish(prefix+"denied by user", true), true
		}
		var exitErr *tool.ExitError
		if errors.As(err, &exitErr) {
			return finish(prefix+exitErr.Output, true), false
		}
		return finish(prefix+"error: "+err.Error(), true), false
	}
	return finish(prefix+result, false), false
}

// flushPendingQueue executes all staged edits/writes and returns the
// batch result string. Individual ToolCallEnd events are emitted for each
// staged call so the UI shows per-edit results.
func (l *Loop) flushPendingQueue(ctx context.Context) string {
	staged := l.pendingQueue.Staged()
	l.pendingQueue.Discard()

	if len(staged) == 0 {
		return "No pending edits to execute."
	}

	var results []tool.BatchResult
	if l.pendingExecutor != nil {
		results = l.pendingExecutor.ExecutePending(ctx, staged)
	} else {
		results = l.executePendingSequential(ctx, staged)
	}

	// Emit individual ToolCallEnd events for each staged call.
	for i, r := range results {
		result := r.Result
		isError := false
		if r.Error != "" {
			result = r.Error
			isError = true
		}
		var metadata map[string]any
		if !isError && r.ToolName == "edit_file" && i < len(staged) {
			metadata = editpreview.MetadataFromParams(staged[i].Params, result)
		}
		l.emit(Event{
			Kind:       ToolCallEnd,
			ToolCallID: r.ToolCallID,
			ToolName:   r.ToolName,
			Result:     result,
			IsError:    isError,
			Metadata:   metadata,
		})
	}

	return tool.FormatBatchResult(results)
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

// validateStagedCall performs lightweight validation on a pending edit/write
// before staging, so the model gets immediate error feedback.
func validateStagedCall(toolName string, params map[string]any) error {
	path, _ := params["path"].(string)
	if path == "" {
		return fmt.Errorf("%s: path is required", toolName)
	}
	if toolName == "edit_file" {
		oldStr, _ := params["old_string"].(string)
		newStr, _ := params["new_string"].(string)
		if oldStr == "" {
			return fmt.Errorf("edit_file: old_string must not be empty")
		}
		if oldStr == newStr {
			return fmt.Errorf("edit_file: old_string and new_string are identical")
		}
	}
	return nil
}
