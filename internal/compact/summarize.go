package compact

import (
	"context"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/message"
)

// DefaultSummarizerPrompt is the default system prompt for the summarizer.
const DefaultSummarizerPrompt = `You are summarizing a coding agent conversation so the agent can continue working with reduced context. Your summary must preserve everything the agent needs to continue without re-reading files or re-asking the user.

Produce a summary with these sections:

## Goal
What the user asked for, in their words. Include exact quotes of key instructions.

## Files
Which files were read, created, or modified. For modified files: what changed and why.

## Current state
What has been done so far. What works, what doesn't.

## Unfinished work
Tasks started but not completed. Errors not yet resolved. Things the agent said it would do next.

## Decisions
Choices the user made or confirmed. Constraints they stated. Things they rejected.

Be specific — include file paths, function names, error messages, line numbers. Do not generalize. If the user said "don't use React," write that exactly, not "user has framework preferences."`

const safetyMargin = 1.25
const summaryBudget = 20_000

// Run prunes messages and produces a summary, using single-shot or
// iterative summarization depending on whether the pruned conversation
// fits in the summarizer's context window.
func Run(ctx context.Context, messages []message.Message, cfg Config) (Result, error) {
	pruned := Prune(messages)
	prunedTokens := CountTokens(pruned)

	prompt := cfg.SummarizerPrompt
	if prompt == "" {
		prompt = DefaultSummarizerPrompt
	}

	if float64(prunedTokens)*safetyMargin+summaryBudget < float64(cfg.ContextWindow) {
		summary, err := summarizeOnce(ctx, cfg, prompt, "", pruned)
		if err != nil {
			return Result{}, err
		}
		return Result{Summary: summary, SummarizerModel: cfg.SummarizerClient.Model(), SummarizerRef: cfg.SummarizerClient.ModelRef()}, nil
	}

	summary, err := summarizeIterative(ctx, cfg, prompt, pruned)
	if err != nil {
		return Result{}, err
	}
	return Result{Summary: summary, SummarizerModel: cfg.SummarizerClient.Model(), SummarizerRef: cfg.SummarizerClient.ModelRef()}, nil
}

func summarizeOnce(ctx context.Context, cfg Config, systemPrompt, previousSummary string, messages []message.Message) (string, error) {
	userContent := ""
	if previousSummary != "" {
		userContent = "Previous summary:\n" + previousSummary + "\n\nContinuation:\n"
	}
	userContent += serializeMessages(messages)

	resp, err := cfg.SummarizerClient.Chat(ctx, []message.Message{
		message.NewText(message.RoleSystem, systemPrompt),
		message.NewText(message.RoleUser, userContent),
	}, nil)
	if err != nil {
		return "", fmt.Errorf("summarizer call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("summarizer returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

func summarizeIterative(ctx context.Context, cfg Config, systemPrompt string, pruned []message.Message) (string, error) {
	maxPieceTokens := int(float64(cfg.ContextWindow-summaryBudget*2) / safetyMargin)
	if maxPieceTokens < 1000 {
		maxPieceTokens = 1000
	}
	pieces := splitIntoPieces(pruned, maxPieceTokens)

	var summary string
	for _, piece := range pieces {
		var err error
		summary, err = summarizeOnce(ctx, cfg, systemPrompt, summary, piece)
		if err != nil {
			return "", err
		}
	}
	return summary, nil
}

// splitIntoPieces splits messages into groups that fit within maxTokens.
// Never splits between an assistant message with ToolCalls and the
// subsequent tool result messages.
func splitIntoPieces(messages []message.Message, maxTokens int) [][]message.Message {
	groups := groupByToolPairs(messages)
	var pieces [][]message.Message
	var current []message.Message
	currentTokens := 0

	for _, group := range groups {
		groupTokens := CountTokens(group)
		if len(current) > 0 && currentTokens+groupTokens > maxTokens {
			pieces = append(pieces, current)
			current = nil
			currentTokens = 0
		}
		current = append(current, group...)
		currentTokens += groupTokens
	}
	if len(current) > 0 {
		pieces = append(pieces, current)
	}
	return pieces
}

// groupByToolPairs groups messages so that an assistant message with
// ToolCalls and its subsequent tool result messages are never separated.
func groupByToolPairs(messages []message.Message) [][]message.Message {
	var groups [][]message.Message
	i := 0
	for i < len(messages) {
		m := messages[i]
		if m.Role == message.RoleAssistant && len(m.ToolCalls) > 0 {
			group := []message.Message{m}
			i++
			for i < len(messages) && messages[i].Role == message.RoleTool {
				group = append(group, messages[i])
				i++
			}
			groups = append(groups, group)
		} else {
			groups = append(groups, []message.Message{m})
			i++
		}
	}
	return groups
}

func serializeMessages(messages []message.Message) string {
	var b strings.Builder
	for _, m := range messages {
		switch m.Role {
		case message.RoleUser:
			fmt.Fprintf(&b, "User: %s\n\n", m.TextContent())
		case message.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				if content := m.TextContent(); content != "" {
					fmt.Fprintf(&b, "Assistant: %s\n", content)
				}
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "Assistant [tool_call]: %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
				}
				b.WriteString("\n")
			} else {
				fmt.Fprintf(&b, "Assistant: %s\n\n", m.TextContent())
			}
		case message.RoleTool:
			name := m.Name
			if name == "" {
				name = "tool"
			}
			fmt.Fprintf(&b, "Tool [%s]: %s\n\n", name, m.TextContent())
		}
	}
	return b.String()
}
