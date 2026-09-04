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

// contextSource returns the Agent context boundary of one Operation: before
// every model effect it projects fresh model.Message history from the
// committed entries plus the captured system prompt. Phase 3 accepts only full
// immutable-history projection.
func (h *Harness) contextSource(c *coordinator, operationID string) agent.ContextSource {
	return func(context.Context) ([]model.Message, error) {
		c.mu.Lock()
		op, ok := c.graph.Operation(operationID)
		if !ok {
			sessionID := c.graph.Session.Identity.SessionID
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
		}
		entries := c.graph.Entries
		systemPrompt := op.Admission.Execution.SystemPrompt
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
		for _, entry := range entries {
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
}

// projectEntry maps one committed entry to its one model message under the
// kind-to-message mapping. An operation settlement produces no model message;
// the hook_result and compaction kinds have no valid Phase 3 payload and never
// materialize.
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

// commitSteeringInput is the steering-input helper: it commits one waiting
// steering message as an Operation-owned input entry immediately before the
// next model request, advancing last activity to the entry's commit time in
// the same transaction.
func (h *Harness) commitSteeringInput(ctx context.Context, c *coordinator, operationID string, content []model.ContentPart) error {
	owned := make([]model.ContentPart, 0, len(content))
	for i, part := range content {
		validated, err := model.NewContentPart(part)
		if err != nil {
			return invalidInput("content[%d]: %v", i, err)
		}
		owned = append(owned, validated)
	}
	c.mu.Lock()
	view := c.graph.Session
	if _, ok := c.graph.Operation(operationID); !ok {
		c.mu.Unlock()
		return fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, view.Identity.SessionID)
	}
	entryID, err := newHexID()
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrStorage, err)
	}
	input := inputEntry{
		SessionID:   view.Identity.SessionID,
		EntryID:     entryID,
		OperationID: operationID,
		Origin:      InputOriginUser,
		Content:     owned,
	}
	payload, err := encodeInputEntry(input)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	var (
		committedSession SessionRecord
		inserted         Entry
	)
	err = h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		key := RegisterKey{SessionID: view.Identity.SessionID, Kind: RegisterSession}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		current, err := decodeSessionRegister(reg)
		if err != nil {
			return corruptSession(view.Identity.SessionID, "session register: %v", err)
		}
		// a violated semantic precondition outranks the conflict class: steering
		// targets the open Session's one running Operation
		if current.State.Lifecycle != LifecycleOpen {
			return invalidInput("session %q is archived; steering requires an open Session", view.Identity.SessionID)
		}
		if current.State.CurrentOperationID != operationID {
			return invalidInput("session %q runs operation %q; steering requires the target operation current", view.Identity.SessionID, current.State.CurrentOperationID)
		}
		if reg.Revision != view.Revision {
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, view.Identity.SessionID, view.Revision, reg.Revision)
		}
		inserted, err = tx.InsertEntry(EntryDraft{
			SessionID:   view.Identity.SessionID,
			ID:          entryID,
			OperationID: operationID,
			Kind:        EntryInput,
			Payload:     payload,
		})
		if err != nil {
			return err
		}
		state := current.State
		state.LastActivity = inserted.CommittedAt
		committedSession = SessionRecord{Identity: current.Identity, State: state}
		sessionPayload, err := encodeSessionRegister(committedSession)
		if err != nil {
			return err
		}
		replaced, err := tx.ReplaceRegister(key, reg.Revision, sessionPayload)
		if err != nil {
			return err
		}
		committedSession.Revision = replaced.Revision
		return nil
	})
	if err != nil {
		h.markCorrupt(view.Identity.SessionID, err)
		c.mu.Unlock()
		if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the cached view
			if rerr := h.rematerialize(ctx, c, view.Identity.SessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return rerr
			}
		}
		return err
	}
	c.graph.Entries = append(c.graph.Entries, graphEntry{Envelope: inserted, Input: &input})
	c.graph.Session = committedSession
	c.mu.Unlock()
	return nil
}
