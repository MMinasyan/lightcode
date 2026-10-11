package harness

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// errSteeringBoundary is the one package-private non-cancellation context
// token: a conversation boundary returns it instead of projecting another
// request while steering waits. Agent.Run hands a non-cancellation context
// error back verbatim with a zero terminal, and the outer terminal settlement
// consumes this token by exact identity before its guards. It is produced
// only by the contextSource closure and is never persisted, returned
// publicly, or latched in Wait.
var errSteeringBoundary = errors.New("harness: steering boundary")

// contextSource returns the Agent context boundary of one Operation: the
// first call projects the newly admitted input only (first-request
// protection), and every later call selects the conversation boundary —
// when steering waits it returns errSteeringBoundary instead of projecting
// another request, leaving every head buffered for the ordinary post-terminal
// drain — otherwise it projects the fresh model.Message history. Selection
// consumes no head and performs no storage, reservation, cancellation or
// observation. Physical retries and compact-piece context callbacks never
// reach this boundary.
func (h *Harness) contextSource(c *coordinator, operationID string) agent.ContextSource {
	firstRequest := true
	return func(ctx context.Context) ([]model.Message, error) {
		if !firstRequest {
			c.mu.Lock()
			waiting := len(c.steering) > 0
			c.mu.Unlock()
			if waiting {
				return nil, errSteeringBoundary
			}
		}
		firstRequest = false
		return h.projectContext(c, operationID)
	}
}

// projectContext is the pure projection body: it snapshots the cached graph
// under the coordinator lock, releases it, and builds the messages without
// touching steering or queued buffers. When the Session register's
// CompactionEntryID is set, the projection is the system message, one
// assistant summary message built from the named compaction entry's payload,
// and the entries after the sequence of the boundary entry that the named
// entry's payload records — the named entry and every entry at or before that
// boundary never project as messages. With the field empty the full history
// projects.
func (h *Harness) projectContext(c *coordinator, operationID string) ([]model.Message, error) {
	c.mu.Lock()
	sessionID := c.graph.Session.Identity.SessionID
	op, ok := c.graph.Operation(operationID)
	if !ok {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
	}
	entries := c.graph.Entries
	systemPrompt := op.Admission.Execution.SystemPrompt
	compactionID := c.graph.Session.State.CompactionEntryID
	c.mu.Unlock()

	messages := make([]model.Message, 0, len(entries)+1)
	if systemPrompt != "" {
		msg, err := model.NewMessage(model.Message{
			Role:    model.RoleSystem,
			Content: []model.ContentPart{{Kind: model.PartText, Text: systemPrompt}},
		})
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	// The named compaction's payload supplies the one summary message and
	// its boundary target's sequence is boundarySequence, at or before which
	// no entry projects; -1 means no compaction boundary and the full
	// history projects.
	compaction, boundarySequence := findCompactionCutoff(entries, compactionID)
	if compaction != nil {
		msg, err := model.NewMessage(model.Message{
			Role:    model.RoleAssistant,
			Source:  compaction.Model,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\n" + compaction.Summary + "\n\n[End of summary. Continue from here.]"}},
		})
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	for _, entry := range entries {
		if entry.Envelope.Sequence <= boundarySequence {
			continue
		}
		msg, ok, err := projectEntry(entry)
		if err != nil {
			return nil, err
		}
		if ok {
			messages = append(messages, msg)
		}
	}
	return messages, nil
}

// projectEntry maps one committed entry to its one model message under the
// kind-to-message mapping. An operation settlement and a hook result produce
// no model message — hook results are execution evidence, never conversation
// messages — and a compaction entry never materializes either: the register's
// CompactionEntryID carries its summary into the projection instead.
func projectEntry(entry graphEntry) (model.Message, bool, error) {
	switch {
	case entry.Input != nil:
		msg, err := model.NewMessage(model.Message{
			Role:    model.RoleUser,
			Content: entry.Input.Content,
		})
		return msg, true, err
	case entry.Assistant != nil:
		calls := make([]model.ToolCall, 0, len(entry.Assistant.ToolCalls))
		for _, call := range entry.Assistant.ToolCalls {
			raw, err := base64.StdEncoding.DecodeString(call.ArgumentsBase64)
			if err != nil {
				return model.Message{}, false, fmt.Errorf("entry %s: tool call %q arguments: %w", entry.Envelope.ID, call.ID, err)
			}
			calls = append(calls, model.ToolCall{ID: call.ID, Name: call.Name, Arguments: raw, Extra: call.Extra})
		}
		msg, err := model.NewMessage(model.Message{
			Role:      model.RoleAssistant,
			Source:    entry.Assistant.Source,
			Content:   entry.Assistant.Content,
			Refusal:   entry.Assistant.Refusal,
			Extra:     entry.Assistant.Extra,
			ToolCalls: calls,
		})
		return msg, true, err
	case entry.ToolResult != nil:
		msg, err := model.NewMessage(model.Message{
			Role:       model.RoleTool,
			ToolCallID: entry.ToolResult.ToolCallID,
			Content:    []model.ContentPart{{Kind: model.PartText, Text: entry.ToolResult.Content}},
		})
		return msg, true, err
	case entry.Signal != nil:
		msg, err := model.NewMessage(model.Message{
			Role:    model.RoleUser,
			Content: []model.ContentPart{{Kind: model.PartText, Text: signalProjectedText(entry.Signal.Content)}},
		})
		return msg, true, err
	default:
		return model.Message{}, false, nil
	}
}

// signalProjectedText renders one signal's fixed content inside the retained
// system-signal text wrapper; the typed signal payload stays the canonical
// origin and subtype authority.
func signalProjectedText(content string) string {
	return "<system-signal>" + strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(content) + "</system-signal>"
}

// Interrupt interrupts one Session's execution, keyed on the durable state:
// an idle Session — no current Operation — returns nil. Otherwise both
// interrupt vehicles fire: the installed execution's context is canceled, and
// the not-yet-started executions' marker is set to the durable current
// Operation. The marker is consumed at execute's entry when it matches that
// operation — a marker for a running operation is inert for that entry and
// stays visible to the shared success guard — and canceling a retiring
// predecessor is a harmless no-op. Buffers are never discarded by an
// interrupt: unselected items drain after the interrupted Operation's
// terminal.
func (h *Harness) Interrupt(ctx context.Context, sessionID string) error {
	c, err := h.coordinatorFor(ctx, sessionID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	operationID := c.graph.Session.State.CurrentOperationID
	if operationID == "" {
		c.mu.Unlock()
		return nil
	}
	c.interruptOp = operationID
	run := c.run
	c.mu.Unlock()
	if run != nil {
		run.cancel()
	}
	return nil
}
