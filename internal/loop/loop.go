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
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

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

// EventKind identifies the phase of a tool call being reported.
type EventKind int

const (
	// ToolCallStart is emitted before a tool's Execute runs.
	ToolCallStart EventKind = iota
	// ToolCallEnd is emitted after a tool's Execute returns (or errors).
	ToolCallEnd
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
// If ctx is cancelled mid-stream, the in-flight assistant message is
// finalized with an interrupted marker and persisted; Run returns
// cleanly (no error), because the cancel is a user action, not a
// failure.
func (l *Loop) Run(ctx context.Context, userInputs ...string) (string, error) {
	turn := 0
	if l.store != nil {
		turn = l.store.CurrentTurn()
	}

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
		if cancelled {
			msg.ToolCalls = nil
			l.messages = append(l.messages, msg)
			l.persistMessage(turn, msg)
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
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatusCode == 429 || apiErr.HTTPStatusCode >= 500
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return reqErr.HTTPStatusCode == 429 || reqErr.HTTPStatusCode >= 500
	}
	return false
}

func (l *Loop) runStream(ctx context.Context) (openai.ChatCompletionMessage, bool, error) {
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
		var stream *openai.ChatCompletionStream
		var err error
		stream, err = l.client.ChatStream(ctx, l.messages, l.registry.OpenAITools())
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

func (l *Loop) consumeStream(ctx context.Context, stream *openai.ChatCompletionStream) (openai.ChatCompletionMessage, bool, error) {

	var (
		contentBuf strings.Builder
		toolDeltas map[int]*openai.ToolCall
		role       string
		usage      *openai.Usage
		cancelled  bool
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
		delta := chunk.Choices[0].Delta

		if delta.Role != "" {
			role = delta.Role
		}
		if delta.Content != "" {
			contentBuf.WriteString(delta.Content)
			l.emit(Event{Kind: TextDelta, Result: delta.Content})
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
	msg := openai.ChatCompletionMessage{
		Role:    role,
		Content: contentBuf.String(),
	}
	if len(toolDeltas) > 0 && !cancelled {
		calls := make([]openai.ToolCall, len(toolDeltas))
		for idx, tc := range toolDeltas {
			calls[idx] = *tc
		}
		msg.ToolCalls = calls
	}
	return msg, cancelled, nil
}

// dispatch executes one tool call and returns the result string plus a
// bool indicating whether the user denied the operation.
func (l *Loop) dispatch(ctx context.Context, tc openai.ToolCall) (string, bool) {
	fmt.Fprintf(l.trace, "  → %s %s\n", tc.Function.Name, truncate(tc.Function.Arguments, traceMaxChars))
	l.emit(Event{Kind: ToolCallStart, ToolCallID: tc.ID, ToolName: tc.Function.Name, Args: tc.Function.Arguments})

	finish := func(result string, isError bool) string {
		fmt.Fprintf(l.trace, "  ← %s\n", truncate(result, traceMaxChars))
		l.emit(Event{Kind: ToolCallEnd, ToolCallID: tc.ID, ToolName: tc.Function.Name, Args: tc.Function.Arguments, Result: result, IsError: isError})
		return result
	}

	var params map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &params); err != nil {
		return finish(fmt.Sprintf("error: invalid JSON arguments: %v", err), true), false
	}
	t, ok := l.registry.Get(tc.Function.Name)
	if !ok {
		return finish(fmt.Sprintf("error: unknown tool %q", tc.Function.Name), true), false
	}
	params["_tool_call_id"] = tc.ID
	result, err := t.Execute(ctx, params)
	if err != nil {
		if errors.Is(err, tool.ErrDenied) {
			return finish("denied by user", true), true
		}
		var exitErr *tool.ExitError
		if errors.As(err, &exitErr) {
			return finish(exitErr.Output, true), false
		}
		return finish("error: "+err.Error(), true), false
	}
	return finish(result, false), false
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
