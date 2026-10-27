package harness

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// errSteeringBoundary is the one package-private non-cancellation context
// token: a conversation boundary returns it instead of supplying another
// request while steering waits. Agent.Run hands a non-cancellation context
// error back verbatim with a zero terminal, and the outer terminal settlement
// consumes this token by exact identity before its guards. It is produced
// only by the requestSource closure and is never persisted, returned
// publicly, or latched in Wait.
var errSteeringBoundary = errors.New("harness: steering boundary")

// conversationRound is the one request-source-built round an execution's next
// Engine call consumes: the frozen canonical messages and canonical
// references captured at one projection snapshot, the request identity bound
// before contributions, and the frozen contribution view applied to every
// model dispatch of the round. It is process-local composition inside the
// owning pipeline: the same frozen snapshot is consumed throughout the round,
// and no later history reread re-derives it.
type conversationRound struct {
	requestID string
	canonical []model.Message // frozen canonical conversation view, prompt stripped
	net       RequestEvidence // final effective contribution evidence of the round
}

// requestView is the live effective request view built by coalescing the
// selected request hooks over the frozen capture in memory.
type requestView struct {
	capture      ExecutionCapture
	canonical    []model.Message // frozen canonical conversation view of the round
	checkpoint   string
	boundary     string
	prompt       string // effective prompt text
	additions    []model.Message
	names        []string // effective visible tool names; nil means the full captured catalog
	contributors []requestContribution
}

// requestContribution records one accepted hook contribution in configured
// order: its identity and its mode. A pure contribution enters the list only
// when its accepted consequence actually changed the live effective view; an
// effectful contribution always enters, including a no-op. The view restores
// to the entire frozen baseline by clearing pure contributors, never by
// re-attributing dimensions to their last presenter.
type requestContribution struct {
	identity  HookIdentity
	effectful bool
}

// newRequestView freezes the captured prompt and the canonical projection
// into one live view.
func newRequestView(capture ExecutionCapture, checkpoint, boundary string, canonical []model.Message) *requestView {
	return &requestView{
		capture:    capture,
		canonical:  canonical,
		checkpoint: checkpoint,
		boundary:   boundary,
		prompt:     capture.SystemPrompt,
		additions:  nil,
		names:      nil,
	}
}

// currentCanonical renders the view's effective conversation messages for one
// hook input: the frozen canonical projection plus the contributions accepted
// so far. The captured prompt is carried separately as the rendered prompt.
func (v *requestView) currentCanonical() []model.Message {
	if len(v.additions) == 0 {
		return v.canonical
	}
	out := make([]model.Message, 0, len(v.canonical)+len(v.additions))
	out = append(out, v.canonical...)
	out = append(out, v.additions...)
	return out
}

// effectiveTools renders the view's effective tool catalog: the captured
// catalog unchanged, or its selected members in captured catalog order.
func (v *requestView) effectiveTools() []model.ToolDefinition {
	if v.names == nil {
		return cloneToolDefinitions(v.capture.Tools)
	}
	out := make([]model.ToolDefinition, 0, len(v.names))
	for _, tool := range v.capture.Tools {
		for _, name := range v.names {
			if name == tool.Name {
				out = append(out, cloneToolDefinitions([]model.ToolDefinition{tool})...)
				break
			}
		}
	}
	return out
}

// coversCaptured reports whether one already-accepted selection names every
// tool of the captured catalog. Selections are validated catalog-internal and
// canonicalized to catalog order by the accepting owner, so a selection that
// names each captured tool presents the unchanged full view.
func coversCaptured(names []string, tools []model.ToolDefinition) bool {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	for _, tool := range tools {
		if !seen[tool.Name] {
			return false
		}
	}
	return true
}

// cloneToolDefinitions returns an independent owned copy of one tool
// definition list with raw parameter bytes cloned.
func cloneToolDefinitions(tools []model.ToolDefinition) []model.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]model.ToolDefinition, len(tools))
	for i, tool := range tools {
		out[i] = tool
		out[i].Parameters = model.CloneRaw(tool.Parameters)
	}
	return out
}

// applyConsequence transforms the live view with one already-validated
// consequence: a present Prompt replaces the prompt text, Messages append to
// the conversation view, and a present ToolNames replaces visibility with
// those names rebuilt from the captured catalog in catalog order. A selection
// covering the captured catalog normalizes the live view to the full catalog.
// It reports whether the consequence actually changed the live view.
func (v *requestView) applyConsequence(con RequestConsequence) bool {
	prompt, additions, names := v.prompt, len(v.additions), v.names
	if con.Prompt != nil {
		v.prompt = *con.Prompt
	}
	if len(con.Messages) > 0 {
		v.additions = append(v.additions, con.Messages...)
	}
	if con.ToolNames != nil {
		if coversCaptured(con.ToolNames, v.capture.Tools) {
			v.names = nil
		} else {
			ordered := make([]string, 0, len(con.ToolNames))
			for _, tool := range v.capture.Tools {
				for _, name := range con.ToolNames {
					if name == tool.Name {
						ordered = append(ordered, tool.Name)
						break
					}
				}
			}
			v.names = ordered
		}
	}
	return prompt != v.prompt || additions != len(v.additions) ||
		(names == nil) != (v.names == nil) || !slices.Equal(names, v.names)
}

// recordPure applies one accepted pure consequence and records its identity
// only when the applied view actually changed. When the view returns to the
// entire frozen baseline — captured prompt, no additions, full catalog — pure
// contributors clear, matching the argument chain's restore rule.
func (v *requestView) recordPure(identity HookIdentity, con RequestConsequence) {
	changed := v.applyConsequence(con)
	if changed {
		v.contributors = append(v.contributors, requestContribution{identity: identity})
	}
	v.clearPureContributorsAtBaseline()
}

// recordEffectful applies the codec-adopted committed consequence of one
// settled effectful hook and records its identity unconditionally, including
// for a reasserted no-op.
func (v *requestView) recordEffectful(identity HookIdentity, con RequestConsequence) {
	v.applyConsequence(con)
	v.contributors = append(v.contributors, requestContribution{identity: identity, effectful: true})
	v.clearPureContributorsAtBaseline()
}

func (v *requestView) clearPureContributorsAtBaseline() {
	if v.prompt != v.capture.SystemPrompt || len(v.additions) > 0 || v.names != nil {
		return
	}
	kept := v.contributors[:0]
	for _, c := range v.contributors {
		if c.effectful {
			kept = append(kept, c)
		}
	}
	v.contributors = kept
}

// evidenceFor renders one evidence value over the frozen capture baseline.
// The delta fields and pure provenance are baseline-relative at every consumed
// boundary: while the view net-changed the baseline, every recorded pure
// contributor renders in hook order beside every effectful one, and effectful
// contributions retain their identities even when the view rests at the
// baseline.
func (v *requestView) evidenceFor() RequestEvidence {
	evidence := RequestEvidence{
		CheckpointEntryID: v.checkpoint,
		BoundaryEntryID:   v.boundary,
	}
	promptChanged := v.prompt != v.capture.SystemPrompt
	if promptChanged {
		prompt := v.prompt
		evidence.Prompt = &prompt
	}
	if len(v.additions) > 0 {
		evidence.Messages = cloneRequestMessages(v.additions)
	}
	namesChanged := v.names != nil && !coversCaptured(v.names, v.capture.Tools)
	if namesChanged {
		names := make([]string, len(v.names))
		copy(names, v.names)
		evidence.ToolNames = names
	}
	netChanged := promptChanged || len(v.additions) > 0 || namesChanged
	for _, c := range v.contributors {
		if c.effectful || netChanged {
			evidence.HookIDs = append(evidence.HookIDs, c.identity)
		}
	}
	return evidence
}

// validateRequestConsequence is the accepting gate for one consequence at the
// request boundary: the shared message rule (tool execution members excluded),
// and tool names validated against and canonicalized to the captured catalog.
// A present selection may restore any captured eligible tool, including the
// full catalog; selecting outside the captured catalog rejects before the next
// hook or dispatch consumes the value.
func validateRequestConsequence(capture ExecutionCapture, con RequestConsequence) (RequestConsequence, error) {
	owned := RequestConsequence{}
	if con.Prompt != nil {
		prompt := *con.Prompt
		owned.Prompt = &prompt
	}
	for i, m := range con.Messages {
		validated, err := validateContributionMessage(m)
		if err != nil {
			return RequestConsequence{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		owned.Messages = append(owned.Messages, validated)
	}
	if con.ToolNames != nil {
		seen := make(map[string]bool, len(con.ToolNames))
		for _, name := range con.ToolNames {
			if name == "" {
				return RequestConsequence{}, errors.New("consequence tool_names carries an empty name")
			}
			if seen[name] {
				return RequestConsequence{}, fmt.Errorf("consequence tool_names has duplicate name %q", name)
			}
			found := false
			for _, tool := range capture.Tools {
				if tool.Name == name {
					found = true
					break
				}
			}
			if !found {
				return RequestConsequence{}, fmt.Errorf("consequence tool name %q is outside the captured tool catalog", name)
			}
			seen[name] = true
		}
		ordered := make([]string, 0, len(con.ToolNames))
		for _, tool := range capture.Tools {
			if seen[tool.Name] {
				ordered = append(ordered, tool.Name)
			}
		}
		owned.ToolNames = ordered
	}
	return owned, nil
}

// buildDispatchRequest materializes one model dispatch request from the
// evidence adopted at the committed intent over the round's frozen canonical
// messages, never from a reread history or replayed hooks. The materialized
// view is the one the recovery path rebuilds with materializeRequestView:
// the actual request carries the captured catalog, and a narrowed selection
// (present-empty advertises none) narrows the request's tool entries, which
// are the advertisement the Engine gates on and the assistant records.
func buildDispatchRequest(capture ExecutionCapture, canonical []model.Message, evidence RequestEvidence) (model.Request, []string) {
	prompt := capture.SystemPrompt
	if evidence.Prompt != nil {
		prompt = *evidence.Prompt
	}
	messages := make([]model.Message, 0, 1+len(canonical)+len(evidence.Messages))
	if prompt != "" {
		messages = append(messages, model.Message{
			Role:    model.RoleSystem,
			Content: []model.ContentPart{{Kind: model.PartText, Text: prompt}},
		})
	}
	messages = append(messages, canonical...)
	messages = append(messages, evidence.Messages...)
	tools := capture.Tools
	if evidence.ToolNames != nil {
		tools = (&requestView{capture: capture, names: evidence.ToolNames}).effectiveTools()
	}
	names := make([]string, len(tools))
	for i, def := range tools {
		names[i] = def.Name
	}
	return model.Request{Messages: messages, Tools: tools}, names
}

// cloneRequestMessages deep-copies one message list at every ownership
// boundary: content parts, their extras, raw extras values, and the complete
// permitted message fields.
func cloneRequestMessages(messages []model.Message) []model.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]model.Message, len(messages))
	for i, m := range messages {
		out[i] = cloneMessageValue(m)
	}
	return out
}

// cloneMessageValue returns an independent deep copy of one message value:
// the content parts' extras and the message extras are cloned, and every
// scalar field is carried unchanged.
func cloneMessageValue(m model.Message) model.Message {
	out := m
	out.Content = ownContentParts(m.Content)
	out.Extra = m.Extra.Clone()
	if m.ToolCalls != nil {
		out.ToolCalls = make([]model.ToolCall, len(m.ToolCalls))
		for i, call := range m.ToolCalls {
			call.Arguments = model.CloneRaw(call.Arguments)
			call.Extra = call.Extra.Clone()
			out.ToolCalls[i] = call
		}
	}
	return out
}

// requestSource returns the Agent request boundary of one Operation. Its
// order is the owning pipeline's: first steering, then the frozen canonical
// projection and request identity, then the selected request contributions,
// then the automatic maintenance orchestration over canonical-only input,
// then — after a checkpoint — the rebuilt projection with the frozen
// contributions applied exactly once, and finally the model dispatch request.
// The first call projects the newly admitted input only (first-request
// protection), and every later call selects the conversation boundary — when
// steering waits it returns errSteeringBoundary instead of supplying another
// request, leaving every head buffered for the ordinary post-terminal drain.
// Selection consumes no head and performs no cancellation or observation.
// Physical retries and the compact Agent's own request callback never reach
// this boundary. The supplied round carries the built identity and evidence
// to the model effect of the same execution.
func (h *Harness) requestSource(c *coordinator, operationID string, capture ExecutionCapture, exec Execution, inv InvocationExecution, round *conversationRound) agent.RequestSource {
	firstRequest := true
	return func(ctx context.Context) (model.Request, error) {
		if !firstRequest {
			c.mu.Lock()
			waiting := len(c.steering) > 0
			c.mu.Unlock()
			if waiting {
				return model.Request{}, errSteeringBoundary
			}
		}
		firstRequest = false
		if err := ctx.Err(); err != nil { // the source never starts work under a canceled execution
			return model.Request{}, err
		}
		canonical, checkpointID, boundaryID, err := h.projectCanonical(c, operationID)
		if err != nil {
			return model.Request{}, err
		}
		requestID, err := newHexID() // the request identity is minted in memory and bound before contributions
		if err != nil {
			return model.Request{}, fmt.Errorf("%w: %v", ErrStorage, err)
		}
		view := newRequestView(capture, checkpointID, boundaryID, canonical)
		if err := h.runRequestHooks(ctx, c, operationID, inv, requestID, view); err != nil {
			return model.Request{}, err
		}
		if conversationOverflows(c, capture) {
			snapshot := cloneRequestMessages(canonical)
			facts := &maintenanceFacts{
				consumed: view.evidenceFor(), // the maintenance procedure consumes the frozen contribution evidence
			}
			suspended := RunningRequest{
				RequestID:         requestID,
				CheckpointEntryID: checkpointID,
				BoundaryEntryID:   boundaryID,
			}
			summary, usage, maintenanceID, _, closeMaintenance, err := h.runCompaction(ctx, c, operationID, exec, capture, snapshot, &suspended, facts)
			if err != nil {
				closeMaintenance()          // the failure terminal already settled inside the orchestration: dispose now
				return model.Request{}, err // the original cause stands; the outer settled guard preserves the existing terminal
			}
			if err := h.commitCompaction(c, operationID, capture, maintenanceID, facts, summary, len(snapshot), usage, false); err != nil {
				closeMaintenance()
				return model.Request{}, err // a failed checkpoint commit retains the actual state and error for recovery
			}
			closeMaintenance() // the committed checkpoint precedes the maintenance stage's disposal
			canonical, checkpointID, boundaryID, err = h.projectCanonical(c, operationID)
			if err != nil {
				return model.Request{}, err
			}
			view.checkpoint, view.boundary = checkpointID, boundaryID
		}
		// The frozen contribution view is applied exactly once to the rebuilt
		// projection; its references now name the new checkpoint and tail.
		evidence := view.evidenceFor()
		req, _ := buildDispatchRequest(capture, canonical, evidence)
		*round = conversationRound{
			requestID: requestID,
			canonical: canonical,
			net:       evidence,
		}
		return req, nil
	}
}

// projectContext is the pure projection body: it snapshots the resident graph
// under the coordinator lock, releases it, and builds the messages without
// touching steering or queued buffers, returning the messages and the same-
// snapshot canonical checkpoint and boundary references. When the Session
// register's CompactionEntryID is set, the projection is the system message,
// one assistant summary message built from the named compaction entry's
// payload, and the entries after the sequence of the boundary entry that the
// named entry's payload records — the named entry and every entry at or
// before that boundary never project as messages. With the field empty the
// full history projects.
func (h *Harness) projectContext(c *coordinator, operationID string) ([]model.Message, error) {
	messages, _, _, err := h.projectContextRefs(c, operationID)
	return messages, err
}

// projectContextRefs projects the conversation and reports the named
// checkpoint and the canonical boundary entry of the same projection
// snapshot. The boundary is the newest conversation-bearing entry the
// projection carried — including the named checkpoint entry itself, whose
// summary rides the projection as its message — so a rebuilt projection
// always names a complete checkpoint-and-tail pair; an uncompacted
// projection with no projectable entry names none.
func (h *Harness) projectContextRefs(c *coordinator, operationID string) ([]model.Message, string, string, error) {
	c.mu.Lock()
	sessionID := c.graph.Session.Identity.SessionID
	op, ok := c.graph.Operation(operationID)
	if !ok {
		c.mu.Unlock()
		return nil, "", "", fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
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
			return nil, "", "", err
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
			return nil, "", "", err
		}
		messages = append(messages, msg)
	}
	boundary := ""
	for _, entry := range entries {
		if entry.Envelope.Sequence <= boundarySequence {
			continue
		}
		// The named checkpoint entry itself is conversation-bearing: a
		// rebuilt projection whose tail is the checkpoint names the
		// checkpoint entry as its canonical boundary, exactly as the
		// committed advance publishes it.
		if conversationBearingEntryKind(entry) {
			boundary = entry.Envelope.ID
		}
		msg, ok, err := projectEntry(entry)
		if err != nil {
			return nil, "", "", err
		}
		if ok {
			messages = append(messages, msg)
		}
	}
	return messages, compactionID, boundary, nil
}

// projectCanonical is the frozen canonical conversation view one request
// round consumes: the projected messages with the leading captured system
// prompt stripped (the prompt is the request's rendered text, carried
// separately), together with the same-snapshot canonical references.
func (h *Harness) projectCanonical(c *coordinator, operationID string) ([]model.Message, string, string, error) {
	messages, checkpointID, boundaryID, err := h.projectContextRefs(c, operationID)
	if err != nil {
		return nil, "", "", err
	}
	if len(messages) > 0 && messages[0].Role == model.RoleSystem {
		messages = messages[1:]
	}
	return messages, checkpointID, boundaryID, nil
}

// projectEntry maps one committed entry to its one model message under the
// kind-to-message mapping. An operation settlement and a hook result produce
// no model message — hook results are execution evidence, never conversation
// messages — and a compaction entry never materializes either: the register's
// CompactionEntryID carries its summary into the projection instead. Request
// evidence is likewise never conversation text: it lives on entry payloads and
// register state, never in a projected message.
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
	c, release, err := h.acquireCoordinator(ctx, sessionID)
	if err != nil {
		return err
	}
	defer release()
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
