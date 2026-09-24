package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// compactionWrapperOverhead is the fixed token reserve for the compact
// request's wrapper text around the serialized piece: role framing and the
// Previous-summary block's fixed scaffolding.
const compactionWrapperOverhead = 64

// usageAccumulator is the orchestration's process-local piece-usage total:
// one UsageCount summed field-wise across the pieces, all of which run on the
// one compact model. A piece reporting no usage contributes nothing; the
// total stays nil until a piece first reports usage.
type usageAccumulator struct {
	total *UsageCount
}

func (a *usageAccumulator) add(piece UsageCount) error {
	if a.total == nil {
		a.total = &piece
		return nil
	}
	total, err := addUsageCount(*a.total, piece)
	if err != nil {
		return err
	}
	a.total = &total
	return nil
}

// serializeCompactionMessages renders the frozen projected conversation as
// the compaction input: one inert JSON line per message, in order. Each
// message is copied before any field is rewritten — a shallow struct copy
// shares the ToolCalls backing array, so the slice is copied too. The source
// identity is cleared on the copy only: it identifies the model for provider
// replay and is not part of the provider-visible message. Each copied tool
// call's raw argument bytes are replaced with the JSON encoding of their
// quoted byte representation, the one treatment that keeps arguments whose
// bytes are not valid JSON marshalable while retaining them verbatim; every
// other extra value is validated JSON at its accepting boundary. The graph
// and the request's owned messages are never mutated.
func serializeCompactionMessages(messages []model.Message) ([]string, error) {
	lines := make([]string, 0, len(messages))
	for i := range messages {
		copied := messages[i]
		copied.Source = model.ModelRef{}
		if len(copied.ToolCalls) > 0 {
			copied.ToolCalls = append(make([]model.ToolCall, 0, len(copied.ToolCalls)), copied.ToolCalls...)
			for j := range copied.ToolCalls {
				quoted, err := json.Marshal(fmt.Sprintf("%q", []byte(copied.ToolCalls[j].Arguments)))
				if err != nil {
					return nil, err
				}
				copied.ToolCalls[j].Arguments = quoted
			}
		}
		line, err := json.Marshal(copied)
		if err != nil {
			return nil, err
		}
		lines = append(lines, string(line))
	}
	return lines, nil
}

// compactionPieceBudget computes one piece's serialized-input token budget:
// the compact window minus the output reserve, the compact system prompt's
// plain-text estimate, the fixed wrapper overhead, and — when a previous
// summary exists — the previous summary carried as one message.
func compactionPieceBudget(capture CompactCapture, previous string) int {
	budget := capture.ContextWindow - capture.OutputReserve - estimateTokens(capture.SystemPrompt, nil) - compactionWrapperOverhead
	if previous != "" {
		budget -= estimateTokens("", []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: previous}}}})
	}
	return budget
}

// compactModelEffect encloses one compact piece's model request in one
// logical model-effect intent/result pair — a sibling of modelEffect closing
// over the compact transport and the orchestration's usage accumulator. The
// shared active-effect intent transaction commits the same durable intent;
// the in-memory intent's expected identity is the compact model, the assembly
// source the agent's callback validates and the terminal usage model, because
// the pieces run on the compact transport while the durable active-effect
// state is model-agnostic. The standard attempt-retry loop runs through the
// compact transport, and each piece's reported usage joins the accumulator
// before its settlement. A completed output with non-empty summary text
// settles by the empty running result — no entry, the active effect cleared,
// both registers replaced with no usage, the reserved identity unused — and
// returns the ready settlement whose output carries the piece summary. A
// textless completed output, a piece failure, and an interruption settle
// through the effect's own terminal settlement with the accumulated usage
// keyed by the compact model.
func (h *Harness) compactModelEffect(c *coordinator, operationID string, exec Execution, capture ExecutionCapture, accumulated *usageAccumulator) agent.ModelEffect {
	attempt := exec.CompactModel
	retry := exec.Retry
	if retry == nil {
		retry = standardRetryPolicy
	}
	return func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		intent, err := h.beginModelEffect(ctx, c, operationID)
		if err != nil {
			// An intent transaction aborted by cancellation settles the
			// cancellation outcome: the run never sees a cancellation-shaped
			// error out of an active effect.
			if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() != nil {
				if _, cerr := h.commitEffectResult(context.WithoutCancel(h.ctx), c, operationID, nil, modelResult{
					terminal:   OperationInterruption,
					detail:     executionInterruptedDetail,
					usage:      accumulated.total,
					usageModel: capture.Compact.Model,
				}); cerr != nil {
					return agent.ModelSettlement{}, cerr
				}
				return agent.ModelSettlement{Disposition: agent.DispoInterruption, Detail: executionInterruptedDetail}, nil
			}
			return agent.ModelSettlement{}, err
		}
		// The durable intent transaction is the conversation model effect's
		// own; only the in-memory intent's expected identity is the compact
		// model's.
		intent.expected = capture.Compact.Model
		// Execution cancellation stops new callbacks: a context that died
		// between the committed intent and the callback settles the Operation
		// as terminal interruption without ever starting the callback.
		if ctx.Err() != nil {
			return h.interruptModelEffect(c, operationID, intent, accumulated.total)
		}
		// Result and terminal transactions run without cancellation so an
		// already-produced result or required terminal settlement publishes.
		settleCtx := context.WithoutCancel(h.ctx)
		settle := func(cause error) (agent.ModelSettlement, error) {
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
				terminal: OperationFailure,
				detail:   cause.Error(),
				usage:    accumulated.total,
			}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return agent.ModelSettlement{}, cause
		}
		// The one attempt loop lives inside the committed intent, identical
		// to the conversation model effect's but running through the compact
		// transport.
		var output model.Output
		for failed := 1; ; failed++ {
			if err := ctx.Err(); err != nil { // observed cancellation before an attempt interrupts; no output and no assembly call
				return h.interruptModelEffect(c, operationID, intent, accumulated.total)
			}
			stream, attemptErr := attempt(ctx, req)
			if attemptErr == nil && stream != nil {
				output, attemptErr = assemble(intent.expected, stream) // exactly one assembly after acceptance
				if attemptErr != nil {
					return settle(attemptErr)
				}
				break
			}
			if attemptErr == nil || stream != nil { // exactly one stream or one error must be returned; a supplied stream closes before the boundary failure
				if stream != nil {
					_ = stream.Close()
				}
				return settle(&agent.ProtocolError{Boundary: "model", Detail: "physical model request returned neither exactly one stream nor one error"})
			}
			// An attempt failure observed under a done execution context
			// settles the interruption outcome before any classification.
			if ctx.Err() != nil {
				return h.interruptModelEffect(c, operationID, intent, accumulated.total)
			}
			delay, again := retry(attemptErr, failed)
			if !again || delay < 0 {
				committed := agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: attemptErr.Error()}
				if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
					terminal: OperationFailure,
					detail:   attemptErr.Error(),
					usage:    accumulated.total,
				}); err != nil {
					return agent.ModelSettlement{}, err
				}
				return committed, nil
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done(): // cancellation during backoff interrupts; no attempt starts
				return h.interruptModelEffect(c, operationID, intent, accumulated.total)
			}
		}
		// The piece's reported usage joins the accumulator before its
		// settlement; a piece reporting no usage contributes nothing.
		if output.Usage != nil {
			if err := accumulated.add(UsageCount{
				InputTokens:       int64(output.Usage.InputTokens),
				CachedInputTokens: int64(output.Usage.CachedInputTokens),
				OutputTokens:      int64(output.Usage.OutputTokens),
			}); err != nil {
				return agent.ModelSettlement{}, err
			}
		}
		if output.Status == model.OutputCompleted {
			// The ready path applies only after verifying the completed
			// output carries non-empty summary text; a textless completed
			// output — including refusal-only — fails the piece as an
			// ordinary model failure with no retry.
			if output.Message == nil || output.Message.TextContent() == "" {
				if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
					terminal: OperationFailure,
					detail:   "compaction summary is empty",
					usage:    accumulated.total,
				}); err != nil {
					return agent.ModelSettlement{}, err
				}
				return agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "compaction summary is empty"}, nil
			}
			// The empty running result inserts no entry, clears the active
			// effect, and replaces both registers with no usage; the reserved
			// result identity stays unused.
			if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{}); err != nil {
				return agent.ModelSettlement{}, err
			}
			return agent.ModelSettlement{Disposition: agent.DispoReady, Output: &output}, nil
		}
		// A piece failure and an interruption settle through the effect's own
		// terminal settlement, consuming the reserved identity.
		terminal := OperationFailure
		disposition := agent.DispoFailure
		if output.Status == model.OutputInterrupted {
			terminal = OperationInterruption
			disposition = agent.DispoInterruption
		}
		if _, err := h.commitEffectResult(settleCtx, c, operationID, &intent, modelResult{
			terminal: terminal,
			detail:   output.Detail,
			usage:    accumulated.total,
		}); err != nil {
			return agent.ModelSettlement{}, err
		}
		return agent.ModelSettlement{Disposition: disposition, Detail: output.Detail}, nil
	}
}

// runCompaction summarizes the frozen projected conversation through the
// no-Session compact agent. The snapshot — the complete projected
// conversation minus the leading system message, supplied by the caller — is
// serialized into inert transcript lines and packed whole-message into
// pieces that each fit the compact budget; a message that exceeds a nonempty
// piece's remaining budget starts the next piece, and no message, reasoning
// part, tool call, or tool result is ever split. Each piece runs one
// agent.Run invocation: the fixed compact prompt as the system message, the
// framed piece as the user message — the Previous summary block only when a
// previous piece already produced a summary — the compact model effect over
// the compact transport, and a tool boundary that rejects any hallucinated
// dispatch. Each piece's request budget accounts for the previous summary
// known when that piece is packed. Intermediate summaries stay process-local;
// the final piece's output is the summary candidate returned to the caller
// together with the accumulated piece usage — the commit is the caller's. A
// piece failure or interruption settles durably inside its own effect; a
// run-level terminal or any other failure — a pre-run cancellation, a
// cancellation observed after a settled piece, an empty snapshot, an error
// after a settled piece — settles directly on the quiet running Operation
// with the accumulated usage keyed by the compact model. No compaction entry
// is written on any path.
func (h *Harness) runCompaction(execCtx context.Context, c *coordinator, operationID string, exec Execution, capture ExecutionCapture, snapshot []model.Message) (string, *UsageCount, error) {
	if len(snapshot) == 0 {
		return "", nil, h.settleCompactionFailure(c, operationID, OperationFailure, errors.New("nothing to compact"), capture, nil)
	}
	lines, err := serializeCompactionMessages(snapshot)
	if err != nil {
		return "", nil, h.settleCompactionFailure(c, operationID, OperationFailure, err, capture, nil)
	}
	accumulated := &usageAccumulator{}
	effect := h.compactModelEffect(c, operationID, exec, capture, accumulated)
	previous := ""
	index := 0
	for index < len(lines) {
		budget := compactionPieceBudget(capture.Compact, previous)
		var piece []string
		used := 0
		for index < len(lines) {
			cost := estimateTokens(lines[index], nil)
			if len(piece) > 0 && used+cost > budget {
				break
			}
			piece = append(piece, lines[index])
			used += cost
			index++
		}
		requestText := ""
		if previous != "" {
			requestText = "Previous summary:\n" + previous + "\n\nContinuation:\n"
		}
		requestText += strings.Join(piece, "\n")
		res, runErr := agent.Run(execCtx, agent.Invocation{
			ExpectedModel: capture.Compact.Model,
			Tools:         nil,
			Context: func(context.Context) ([]model.Message, error) {
				return []model.Message{
					{Role: model.RoleSystem, Content: []model.ContentPart{{Kind: model.PartText, Text: capture.Compact.SystemPrompt}}},
					{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: requestText}}},
				}, nil
			},
			ModelEffect: effect,
			ToolEffect: func(context.Context, model.ToolCall) (model.ToolResult, error) {
				return model.ToolResult{}, &agent.ProtocolError{Boundary: "tool", Detail: "compact agent has no tools"}
			},
		})
		if runErr != nil {
			return "", accumulated.total, h.settleCompactionFailure(c, operationID, OperationFailure, runErr, capture, accumulated.total)
		}
		switch res.Status {
		case agent.TerminalSuccess:
			previous = res.LastOutput.Message.TextContent()
		default:
			// A run-level terminal — a pre-run cancellation, or a
			// cancellation observed after a settled piece before the run's
			// success return — leaves the Operation running and quiet with
			// no settlement; the direct settlement covers it, and takes no
			// compensating write when a piece effect already settled the
			// same terminal.
			terminal := OperationFailure
			if res.Status == agent.TerminalInterruption {
				terminal = OperationInterruption
			}
			return "", accumulated.total, h.settleCompactionFailure(c, operationID, terminal, errors.New(res.Detail), capture, accumulated.total)
		}
	}
	return previous, accumulated.total, nil
}

// settleCompactionFailure is the direct terminal settlement of one
// orchestration failure or run-level terminal on the quiet running Operation:
// the given terminal state and detail commit with the accumulated usage keyed
// by the compact model, and the cause returns to the caller. An Operation
// already settled by a piece effect, or one whose committed intent state a
// publication failure left for recovery, takes no compensating write.
func (h *Harness) settleCompactionFailure(c *coordinator, operationID string, terminal OperationState, cause error, capture ExecutionCapture, usage *UsageCount) error {
	c.mu.Lock()
	op, ok := c.graph.Operation(operationID)
	if !ok {
		sessionID := c.graph.Session.Identity.SessionID
		c.mu.Unlock()
		return fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	settled := op.State.Status != OperationRunning
	quiet := op.State.ActiveEffect == nil
	c.mu.Unlock()
	if settled || !quiet {
		return cause
	}
	if _, err := h.commitEffectResult(context.WithoutCancel(h.ctx), c, operationID, nil, modelResult{
		terminal:   terminal,
		detail:     cause.Error(),
		usage:      usage,
		usageModel: capture.Compact.Model,
	}); err != nil {
		return err
	}
	return cause
}
