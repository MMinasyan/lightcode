package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// withSessionSnapshot runs one projection callback inside a single admitted
// call: the captured configuration snapshot is taken exactly once before the
// one Harness snapshot, and the callback consumes both before the admission
// releases — the whole projection lifetime stays joined to the call. Reads
// that need no configuration ignore it.
func (r *Runtime) withSessionSnapshot(ctx context.Context, sessionID string, callback func(harness.SessionSnapshot, *configuration) error) error {
	return r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		captured := r.config.current()
		snap, err := h.SnapshotSession(ctx, sessionID)
		if err != nil {
			return err
		}
		return callback(snap, captured)
	})
}

// listSessions projects Session headers under one required workspace and
// lifecycle filter set, inside a single admitted call. Absent, empty, or
// unknown filter members and a failing workspace normalization wrap the
// shared harness.ErrInvalid sentinel; a nonempty relative workspace is
// normalized with filepath.Abs before filtering. The scan is the Harness's
// own metadata listing: every row is a fresh header and an unavailable
// Session is already omitted there, while any other failure propagates with
// no partial list.
func (r *Runtime) listSessions(ctx context.Context, params protocol.ListSessionsParams) ([]protocol.Session, error) {
	if params.Workspace == "" {
		return nil, fmt.Errorf("workspace must be non-empty: %w", harness.ErrInvalid)
	}
	if !params.Lifecycle.Valid() {
		return nil, fmt.Errorf("lifecycle must be open or archived: %w", harness.ErrInvalid)
	}
	workspace, err := filepath.Abs(params.Workspace)
	if err != nil {
		return nil, fmt.Errorf("normalize workspace: %v: %w", err, harness.ErrInvalid)
	}
	var headers []protocol.Session
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		rows, err := h.ListSessions(ctx)
		if err != nil {
			return err
		}
		headers = make([]protocol.Session, 0, len(rows))
		for _, row := range rows {
			if row.Identity.Workspace != workspace {
				continue // workspace identity is immutable under every supported mutation
			}
			if row.Lifecycle != harness.SessionLifecycle(params.Lifecycle) {
				continue
			}
			headers = append(headers, projectSession(row))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return headers, nil
}

// listWorkspaces projects the distinct Workspace roots of every validated
// Session from one fresh metadata scan, sorted by root, inside a single
// admitted call. It is derived navigation: no Project record, count, or
// owning revision.
func (r *Runtime) listWorkspaces(ctx context.Context) ([]protocol.Workspace, error) {
	var out []protocol.Workspace
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		rows, err := h.ListSessions(ctx)
		if err != nil {
			return err
		}
		roots := make([]string, 0, len(rows))
		seen := make(map[string]bool, len(rows))
		for _, row := range rows {
			root := row.Identity.Workspace
			if !seen[root] {
				seen[root] = true
				roots = append(roots, root)
			}
		}
		sort.Strings(roots)
		out = make([]protocol.Workspace, 0, len(roots))
		for _, root := range roots {
			out = append(out, protocol.Workspace{Root: root, DisplayName: filepath.Base(root)})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// snapshotRevision derives the value revision pair from one full snapshot:
// the durable register revision and the coordinator-local counter.
func snapshotRevision(snap harness.SessionSnapshot) harness.SessionRevision {
	return harness.SessionRevision{DurableRevision: snap.Session.Revision, LocalRevision: snap.LocalRevision}
}

// snapshotHeader adapts one owned full snapshot to the metadata value the
// narrow header producer returns, for projections that already hold the
// complete snapshot (hydration).
func snapshotHeader(snap harness.SessionSnapshot) harness.SessionHeader {
	header := harness.SessionHeader{
		Identity:           snap.Session.Identity,
		Lifecycle:          snap.Session.State.Lifecycle,
		CurrentAgentType:   snap.Session.State.CurrentAgentType,
		CurrentOperationID: snap.Session.State.CurrentOperationID,
		LastActivity:       snap.Session.State.LastActivity,
		Revision:           snapshotRevision(snap),
	}
	if snap.Session.State.ArchivedAt != nil {
		stamped := *snap.Session.State.ArchivedAt
		header.ArchivedAt = &stamped
	}
	return header
}

// projectSession maps one owned metadata header to its generated Session
// header. Every field comes from the header's validated values; timestamps
// are UTC. The instance identifier stays empty in these pre-server values:
// the protocol server supplies it before wire emission.
func projectSession(header harness.SessionHeader) protocol.Session {
	out := protocol.Session{
		AgentType:       header.CurrentAgentType,
		CreatedAt:       header.Identity.CreatedAt.UTC(),
		LastActivity:    header.LastActivity.UTC(),
		Lifecycle:       protocol.SessionLifecycle(header.Lifecycle),
		SessionId:       header.Identity.SessionID,
		SessionRevision: wireRevision(header.Revision),
		Workspace:       header.Identity.Workspace,
	}
	if header.ArchivedAt != nil {
		archived := header.ArchivedAt.UTC()
		out.ArchivedAt = &archived
	}
	if header.CurrentOperationID != "" {
		current := header.CurrentOperationID
		out.CurrentOperationId = &current
	}
	if header.Identity.ParentSessionID != "" {
		parent := header.Identity.ParentSessionID
		out.ParentSessionId = &parent
	}
	if header.Identity.SourceSessionID != "" {
		source := header.Identity.SourceSessionID
		out.SourceSessionId = &source
	}
	return out
}

// projectOperation maps one owned Operation register to its generated header:
// admission identity, the captured Agent type and model ref, the terminal
// status and detail, and the canonical register totals as decimal strings.
func projectOperation(record harness.OperationRecord) protocol.Operation {
	op := protocol.Operation{
		AdmittedAt:  record.Admission.AdmittedAt.UTC(),
		AgentType:   record.Admission.AgentType,
		Model:       record.Admission.Execution.Model.String(),
		OperationId: record.Admission.OperationID,
		RequestKind: protocol.OperationRequestKind(record.Admission.RequestKind),
		Status:      protocol.OperationStatus(record.State.Status),
		Usage:       projectUsage(record.State.Usage),
	}
	if record.State.SettledAt != nil {
		settled := record.State.SettledAt.UTC()
		op.SettledAt = &settled
	}
	if record.State.Terminal != nil && record.State.Terminal.Detail != "" {
		detail := record.State.Terminal.Detail
		op.Detail = &detail
	}
	return op
}

// projectUsage maps the canonical register totals to their generated shape:
// one nonempty-able by-model array whose counts are decimal strings.
func projectUsage(totals harness.UsageTotals) protocol.UsageTotals {
	out := protocol.UsageTotals{ByModel: make([]protocol.ModelUsage, len(totals.ByModel))}
	for i, row := range totals.ByModel {
		out.ByModel[i] = protocol.ModelUsage{
			Model: row.Model.String(),
			Usage: protocol.UsageCount{
				InputTokens:       strconv.FormatInt(row.Usage.InputTokens, 10),
				CachedInputTokens: strconv.FormatInt(row.Usage.CachedInputTokens, 10),
				OutputTokens:      strconv.FormatInt(row.Usage.OutputTokens, 10),
			},
		}
	}
	return out
}

// itemIDDomain is the item identity's domain separator: the protocol item ID
// binds the Session identity and the entry identity, never a payload value.
const itemIDDomain = "lightcode-item\x00"

// projectItemID derives one stable client item identity: the unpadded
// base64url of the first 16 bytes of SHA-256 over the Session/entry binding.
// A copied fork's new entry identities derive independent item IDs.
func projectItemID(sessionID, entryID string) string {
	sum := sha256.Sum256([]byte(itemIDDomain + sessionID + "\x00" + entryID))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// projectedItem is one generated conversation item alongside the stable
// identity its source fact produced: the identity travels with the item from
// its source, so paging anchors and cursors never re-decode a union variant.
type projectedItem struct {
	id   string
	item protocol.ConversationItem
}

// projectConversation derives the complete client conversation from one owned
// Session's validated facts, in committed order. It consumes only the facts:
// they are already materialized, validated, and owned, so no storage payload
// is re-decoded and no model-context projection is consulted. Tool results
// never become separate items — every terminal result is absorbed into the
// assistant item that published its call, matched by the full assistant-entry
// reference, the reserved result entry identity, and the call ID — while an
// unresolved call keeps its assistant item without terminal members and gains
// them on a later read under the same item ID.
func projectConversation(sessionID string, facts []harness.HistoryFact) ([]projectedItem, error) {
	results := indexToolResults(facts)
	items := make([]projectedItem, 0, len(facts))
	for _, fact := range facts {
		item, err := projectConversationItem(sessionID, fact, results)
		if err != nil {
			return nil, err
		}
		if item != nil {
			items = append(items, *item)
		}
	}
	return items, nil
}

// toolResultKey addresses one terminal tool result by the full
// assistant-entry reference (including the Session), its reserved result
// entry identity, and the call ID — the exact validated binding.
type toolResultKey struct {
	session   string
	assistant string
	result    string
	call      string
}

// indexToolResults maps the snapshot's terminal tool results by their
// validated assistant/call binding. Unmatched results cannot exist in
// validated facts and simply never match.
func indexToolResults(facts []harness.HistoryFact) map[toolResultKey]harness.ToolResultEntry {
	index := make(map[toolResultKey]harness.ToolResultEntry)
	for _, fact := range facts {
		if fact.Kind != harness.EntryToolResult {
			continue
		}
		result := *fact.ToolResult
		index[toolResultKey{
			session:   result.AssistantEntry.SessionID,
			assistant: result.AssistantEntry.EntryID,
			result:    result.EntryID,
			call:      result.ToolCallID,
		}] = result
	}
	return index
}

// projectConversationItem maps one validated fact to its generated client
// item and source-known stable identity. Tool-result facts are absorbed into
// their publishing assistant and produce no item of their own.
func projectConversationItem(sessionID string, fact harness.HistoryFact, results map[toolResultKey]harness.ToolResultEntry) (*projectedItem, error) {
	var item protocol.ConversationItem
	var err error
	switch fact.Kind {
	case harness.EntryInput:
		content, contentErr := projectContentParts(fact.Input.Content)
		if contentErr != nil {
			return nil, fmt.Errorf("project input entry %q: %w", fact.EntryID, contentErr)
		}
		err = item.FromInputItem(protocol.InputItem{
			ItemId:      projectItemID(sessionID, fact.EntryID),
			CommittedAt: fact.CommittedAt.UTC(),
			OperationId: operationAttribution(fact.OperationID),
			Origin:      protocol.InputOrigin(fact.Input.Origin),
			Content:     content,
		})
	case harness.EntryAssistant:
		assistant, assistantErr := projectAssistantItem(sessionID, fact, results)
		if assistantErr != nil {
			return nil, assistantErr
		}
		err = item.FromAssistantItem(assistant)
	case harness.EntrySignal:
		err = item.FromSignalItem(projectSignalItem(sessionID, fact))
	case harness.EntryCompaction:
		err = item.FromCompactionItem(protocol.CompactionItem{
			ItemId:      projectItemID(sessionID, fact.EntryID),
			CommittedAt: fact.CommittedAt.UTC(),
			OperationId: operationAttribution(fact.OperationID),
			Summary:     fact.Compaction.Summary,
			Model:       fact.Compaction.Model.String(),
		})
	case harness.EntryOperationSettlement:
		err = item.FromOperationEndItem(protocol.OperationEndItem{
			ItemId:      projectItemID(sessionID, fact.EntryID),
			CommittedAt: fact.CommittedAt.UTC(),
			OperationId: operationAttribution(fact.OperationID),
			Status:      protocol.OperationEndItemStatus(fact.Settlement.Status),
			Detail:      fact.Settlement.Detail,
		})
	default: // a tool result: absorbed into its publishing assistant
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("project %s entry %q: %w", fact.Kind, fact.EntryID, err)
	}
	return &projectedItem{id: projectItemID(sessionID, fact.EntryID), item: item}, nil
}

// operationAttribution points at a fact's owning Operation when it has one;
// a copied fork prefix carries no Operation ownership at all.
func operationAttribution(operationID string) *string {
	if operationID == "" {
		return nil
	}
	return &operationID
}

// projectAssistantItem maps one validated assistant entry to its generated
// item: the published calls in their validated order, each carrying its
// terminal tool result's outcome when that result has committed.
func projectAssistantItem(sessionID string, fact harness.HistoryFact, results map[toolResultKey]harness.ToolResultEntry) (protocol.AssistantItem, error) {
	entry := fact.Assistant
	content, err := projectContentParts(entry.Content)
	if err != nil {
		return protocol.AssistantItem{}, fmt.Errorf("project assistant entry %q: %w", fact.EntryID, err)
	}
	calls := make([]protocol.ToolCallView, 0, len(entry.ToolCalls))
	for _, call := range entry.ToolCalls {
		view, err := projectToolCall(sessionID, fact.EntryID, call, results)
		if err != nil {
			return protocol.AssistantItem{}, fmt.Errorf("project assistant entry %q: %w", fact.EntryID, err)
		}
		calls = append(calls, view)
	}
	item := protocol.AssistantItem{
		ItemId:      projectItemID(sessionID, fact.EntryID),
		CommittedAt: fact.CommittedAt.UTC(),
		OperationId: operationAttribution(fact.OperationID),
		Status:      protocol.AssistantItemStatus(entry.Status),
		Source:      entry.Source.String(),
		Content:     content,
		ToolCalls:   calls,
		Extra:       extraObject(entry.Extra),
	}
	if entry.Refusal != "" {
		refusal := entry.Refusal
		item.Refusal = &refusal
	}
	return item, nil
}

// projectToolCall maps one validated published call to its generated view.
// The wire arguments are the decoded raw argument text: the canonical base64
// never leaves the facts, and the raw bytes need no JSON validity gate —
// the native JSON string encoding supplies the UTF-8 replacement. A terminal
// result matched by the reserved result identity and call ID contributes its
// status, its model-visible content (an empty success content stays present),
// and its preserved tool-owned metadata for every terminal status alike; a
// pending call omits all three and never gains a fabricated outcome.
func projectToolCall(sessionID, assistantID string, call harness.ToolCallRecord, results map[toolResultKey]harness.ToolResultEntry) (protocol.ToolCallView, error) {
	arguments, err := base64.StdEncoding.Strict().DecodeString(call.ArgumentsBase64)
	if err != nil {
		return protocol.ToolCallView{}, fmt.Errorf("tool call %q arguments: %v", call.ID, err)
	}
	view := protocol.ToolCallView{
		Id:        call.ID,
		Name:      call.Name,
		Arguments: string(arguments),
		Extra:     extraObject(call.Extra),
	}
	if len(call.NormalizedArguments) > 0 {
		normalized := protocol.JSONObject{}
		if err := json.Unmarshal(call.NormalizedArguments, &normalized); err != nil {
			return protocol.ToolCallView{}, fmt.Errorf("tool call %q normalized arguments: %v", call.ID, err)
		}
		view.NormalizedArguments = &normalized
	}
	result, ok := results[toolResultKey{session: sessionID, assistant: assistantID, result: call.ResultEntryID, call: call.ID}]
	if !ok {
		return view, nil
	}
	status := protocol.ToolCallStatus(result.Status)
	content := result.Content
	view.Status = &status
	view.Content = &content
	if len(result.Metadata) > 0 {
		metadata := protocol.ToolMetadata(result.Metadata)
		view.Metadata = &metadata
	}
	return view, nil
}

// projectSignalItem maps one validated signal entry to its generated item.
// The subtype comes from the typed payload kind, never from content matching.
func projectSignalItem(sessionID string, fact harness.HistoryFact) protocol.SignalItem {
	item := protocol.SignalItem{
		ItemId:      projectItemID(sessionID, fact.EntryID),
		CommittedAt: fact.CommittedAt.UTC(),
		OperationId: operationAttribution(fact.OperationID),
		Subtype:     protocol.SignalItemSubtype(fact.Signal.Signal),
		Content:     fact.Signal.Content,
	}
	if member := fact.Signal.RelatedMember; member != nil {
		item.RelatedMember = &protocol.BackgroundMember{Kind: protocol.BackgroundMemberKind(member.Kind), Id: member.ID}
	}
	return item
}

// projectContentParts maps one validated content list to its generated
// union parts, each carrying its own validated extras.
func projectContentParts(parts []model.ContentPart) ([]protocol.ContentPart, error) {
	out := make([]protocol.ContentPart, 0, len(parts))
	for _, part := range parts {
		var wire protocol.ContentPart
		var err error
		switch part.Kind {
		case model.PartText:
			err = wire.FromTextPart(protocol.TextPart{Kind: protocol.TextPartKindText, Text: part.Text, Extra: extraObject(part.Extra)})
		case model.PartImageURL:
			err = wire.FromImageURLPart(protocol.ImageURLPart{Kind: protocol.ImageUrl, Url: part.URL, Extra: extraObject(part.Extra)})
		case model.PartOpaque:
			err = wire.FromOpaquePart(protocol.OpaquePart{Kind: protocol.Opaque, OpaqueWireType: part.OpaqueWireType, Extra: extraObject(part.Extra)})
		default:
			return nil, fmt.Errorf("content part kind %q is not one of text, image_url or opaque", part.Kind)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, wire)
	}
	return out, nil
}

// extraObject maps one validated raw extra set to the generated raw-valued
// object — the same map shape, converted directly; the generated From*
// constructor serializes it into the union's own bytes before returning, so
// no reference escapes. Absent extras stay omitted.
func extraObject(extra model.Extra) *protocol.JSONObject {
	if len(extra) == 0 {
		return nil
	}
	out := protocol.JSONObject(extra)
	return &out
}

// wireRevision derives the wire revision pair from one value pair: the
// durable register revision and the coordinator-local counter as decimal
// strings, with the empty pre-server instance identity.
func wireRevision(revision harness.SessionRevision) protocol.SessionRevision {
	return protocol.SessionRevision{
		DurableRevision: strconv.FormatInt(revision.DurableRevision, 10),
		LocalRevision:   strconv.FormatUint(revision.LocalRevision, 10),
	}
}

// historyPageSize is the exact page size in indivisible items.
const historyPageSize = 50

// historyCursor is the private opaque cursor payload: exactly these four
// members on the wire, base64url-encoded.
type historyCursor struct {
	Version      int    `json:"version"`
	SessionID    string `json:"session_id"`
	AnchorItemID string `json:"anchor_item_id"`
	Direction    string `json:"direction"`
}

// encodeHistoryCursor encodes one cursor as its base64url JSON form; the
// primitive-field members' marshal cannot fail.
func encodeHistoryCursor(c historyCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

// decodeHistoryCursor decodes one opaque cursor strictly: base64url, one
// JSON document with exactly the cursor members, then the exact version,
// Session identity, and direction. Every failure wraps the shared
// harness.ErrInvalid sentinel; no other error class exists. The anchor's
// emptiness needs no decoder rule — no valid item identity is empty — the
// resolver rejects it uniformly with every other non-member.
func decodeHistoryCursor(raw, sessionID string) (historyCursor, error) {
	invalid := func(format string, args ...any) (historyCursor, error) {
		return historyCursor{}, fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), harness.ErrInvalid)
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return invalid("malformed history cursor: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var c historyCursor
	if err := decoder.Decode(&c); err != nil {
		return invalid("malformed history cursor: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return invalid("malformed history cursor: trailing content")
	}
	switch {
	case c.Version != 1:
		return invalid("history cursor version %d is not 1", c.Version)
	case c.SessionID != sessionID:
		return invalid("history cursor names session %q, not %q", c.SessionID, sessionID)
	case c.Direction != "older":
		return invalid("history cursor direction %q is not %q", c.Direction, "older")
	}
	return c, nil
}

// projectHistoryPage slices one owned history capture's complete projected
// conversation into one anchored page: exactly historyPageSize indivisible
// items in ascending display order, the newest 50 for the nil initial
// cursor, otherwise the newest items strictly before the anchor item —
// whose identity is resolved among this same projection's source-known item
// identities. The oldest returned item anchors the next older cursor exactly
// when older items remain. A nil cursor is the initial page; an empty
// supplied cursor, a malformed document, or an anchor that resolves to no
// item of this Session fails with harness.ErrInvalid.
func projectHistoryPage(sessionID string, facts []harness.HistoryFact, revision harness.SessionRevision, cursor *string) (protocol.HistoryPage, error) {
	items, err := projectConversation(sessionID, facts)
	if err != nil {
		return protocol.HistoryPage{}, err
	}
	end := len(items)
	if cursor != nil {
		decoded, err := decodeHistoryCursor(*cursor, sessionID)
		if err != nil {
			return protocol.HistoryPage{}, err
		}
		end = -1
		for i, item := range items {
			if item.id == decoded.AnchorItemID {
				end = i
				break
			}
		}
		if end < 0 {
			return protocol.HistoryPage{}, fmt.Errorf("history cursor anchor %q names no item of session %q: %w", decoded.AnchorItemID, sessionID, harness.ErrInvalid)
		}
	}
	start := max(end-historyPageSize, 0)
	page := protocol.HistoryPage{
		SessionRevision: wireRevision(revision),
		Items:           make([]protocol.ConversationItem, end-start),
	}
	for i, item := range items[start:end] {
		page.Items[i] = item.item
	}
	if start > 0 {
		encoded := encodeHistoryCursor(historyCursor{Version: 1, SessionID: sessionID, AnchorItemID: items[start].id, Direction: "older"})
		page.OlderCursor = &encoded
	}
	return page, nil
}

// projectPending maps one owned snapshot's both pending FIFOs to their
// generated shape, reusing the conversation content projection. Queue
// inspection rides hydration; no separate pending read exists.
func projectPending(snap harness.SessionSnapshot) (protocol.PendingQueues, error) {
	steering, err := projectPendingInputs(snap.Steering)
	if err != nil {
		return protocol.PendingQueues{}, err
	}
	queued, err := projectPendingInputs(snap.Queued)
	if err != nil {
		return protocol.PendingQueues{}, err
	}
	return protocol.PendingQueues{Steering: steering, Queued: queued}, nil
}

// projectPendingInputs maps one FIFO's members: the operation identity, the
// origin, and the projected content parts — kind and identity only.
func projectPendingInputs(items []harness.PendingInput) ([]protocol.PendingInput, error) {
	out := make([]protocol.PendingInput, 0, len(items))
	for _, item := range items {
		content, err := projectContentParts(item.Content)
		if err != nil {
			return nil, fmt.Errorf("project pending input %q: %w", item.OperationID, err)
		}
		out = append(out, protocol.PendingInput{OperationId: item.OperationID, Origin: protocol.InputOrigin(item.Origin), Content: content})
	}
	return out, nil
}

// sessionSelection is the shared pure resolution of one Session's current
// Agent type under one captured configuration: the configured model identity
// when the resolved definition names one, and that exact model's catalog
// context window when it is usable there. It resolves the selected
// definition once through the captured roster and the captured catalog and
// joins nothing else.
type sessionSelection struct {
	model  *model.ModelRef
	window int
}

// resolveSessionSelection resolves the current Agent type's model selection
// and window. An unknown, duplicate, or empty-name definition and a
// definition with no configured model yield no selection. A configured model
// that is absent from the captured catalog, or whose entry has no positive
// window, stays identifiable with a zero window: admission still refuses it
// and no fallback model is invented.
func resolveSessionSelection(agentType string, captured *configuration) sessionSelection {
	agent, err := harness.ResolveAgentType(agentType, captured.agentTypes())
	if err != nil {
		return sessionSelection{}
	}
	if agent.Model.IsZero() {
		return sessionSelection{}
	}
	selected := agent.Model
	_, entry, err := captured.catalog.Lookup(catalog.ModelRef{Provider: agent.Model.Provider, Model: agent.Model.Model})
	if err != nil || entry.ContextWindow <= 0 {
		return sessionSelection{model: &selected}
	}
	return sessionSelection{model: &selected, window: entry.ContextWindow}
}

// projectSessionUsage maps one owned snapshot's usage onto its two distinct
// clocks: the canonical register totals as they stand, and the display
// estimate plus the context window of the shared selection resolution.
func projectSessionUsage(snap harness.SessionSnapshot, selection sessionSelection) protocol.UsageProjection {
	return protocol.UsageProjection{
		Totals:  projectUsage(snap.Session.State.Usage),
		Context: protocol.UsageContext{UsedTokens: projectUsedTokens(snap.Facts, snap.Session.State.CompactionEntryID), ContextWindow: sessionContextWindow(snap, selection)},
	}
}

// projectUsedTokens is the display estimate: the latest measured
// input+cached count among the validated facts strictly after the current
// compaction boundary, considering only usage-bearing assistant and
// settlement entries — compaction usage stays totals-only, an unknown usage
// is transparent, and no later measurement means zero.
func projectUsedTokens(facts []harness.HistoryFact, compactionEntryID string) string {
	start := 0
	if compactionEntryID != "" {
		for i, fact := range facts {
			if fact.Kind == harness.EntryCompaction && fact.EntryID == compactionEntryID {
				start = i + 1
				break
			}
		}
	}
	var measured *harness.UsageCount
	for _, fact := range facts[start:] {
		var candidate *harness.UsageCount
		switch {
		case fact.Assistant != nil:
			candidate = fact.Assistant.Usage
		case fact.Settlement != nil:
			candidate = fact.Settlement.Usage
		}
		if candidate != nil {
			measured = candidate
		}
	}
	if measured == nil {
		return "0"
	}
	// The reported counts are signed int64: the display value is the exact
	// mathematical input+cached sum — mixed signs stay negative-exact and a
	// sum beyond MaxInt64 stays positive-exact — so the one addition rides
	// math/big with no clamping or hand-rolled overflow cases.
	return new(big.Int).Add(big.NewInt(measured.InputTokens), big.NewInt(measured.CachedInputTokens)).String()
}

// sessionContextWindow resolves the context window: the durably running
// current Operation's captured window while one is admitted — a retiring run
// or reservation alone is not an active Operation — otherwise the shared
// selection's catalog window. An unavailable selection reports 0 rather than
// an admission error; a usable model always has a positive window, so 0
// never invents a model.
func sessionContextWindow(snap harness.SessionSnapshot, selection sessionSelection) int {
	if opID := snap.Session.State.CurrentOperationID; opID != "" {
		for _, op := range snap.Operations {
			if op.Admission.OperationID == opID {
				return op.Admission.Execution.ContextWindow
			}
		}
	}
	return selection.window
}

// projectHydration assembles the complete Session-owned hydration body from
// one owned snapshot and one captured configuration: the newest conversation
// page, one Session header, the next-admission model selection, the sorted
// Operation headers with the active pointer into them, both pending FIFOs,
// background membership, the usage projection, and the warning presentation —
// every global group plus this Session's own prompt and protocol warnings,
// under the independently captured warning revision.
func projectHydration(snap harness.SessionSnapshot, captured *configuration, warnings []protocol.Warning, warningsRevision uint64) (protocol.Hydration, error) {
	revision := snapshotRevision(snap)
	page, err := projectHistoryPage(snap.Session.Identity.SessionID, snap.Facts, revision, nil)
	if err != nil {
		return protocol.Hydration{}, err
	}
	pending, err := projectPending(snap)
	if err != nil {
		return protocol.Hydration{}, err
	}
	operations := make([]protocol.Operation, 0, len(snap.Operations))
	var active *protocol.Operation
	for _, record := range snap.Operations {
		projected := projectOperation(record)
		if record.Admission.OperationID == snap.Session.State.CurrentOperationID {
			operation := projected
			active = &operation
		}
		operations = append(operations, projected)
	}
	background := make([]protocol.BackgroundMember, 0, len(snap.Background))
	for _, member := range snap.Background {
		background = append(background, protocol.BackgroundMember{Kind: protocol.BackgroundMemberKind(member.Kind), Id: member.ID})
	}
	selection := resolveSessionSelection(snap.Session.State.CurrentAgentType, captured)
	var selectedModel *protocol.ModelRef
	if selection.model != nil {
		rendered := selection.model.String()
		selectedModel = &rendered
	}
	return protocol.Hydration{
		Session:               projectSession(snapshotHeader(snap)),
		SessionRevision:       wireRevision(revision),
		SelectedModel:         selectedModel,
		Operations:            operations,
		ActiveOperation:       active,
		Pending:               pending,
		Conversation:          protocol.ConversationPage{Items: page.Items, OlderCursor: page.OlderCursor},
		Usage:                 projectSessionUsage(snap, selection),
		Background:            background,
		Warnings:              warnings, // the shared read producer returns owned non-nil slices
		WarningsRevision:      protocol.WarningsRevision{Revision: formatWarningRevision(warningsRevision)},
		ConfigurationRevision: configurationRevision(captured),
	}, nil
}

// configurationRevision derives the wire configuration revision from one
// captured snapshot: the publication generation as a decimal string, with
// the empty pre-server instance identity.
func configurationRevision(captured *configuration) protocol.ConfigurationRevision {
	return protocol.ConfigurationRevision{Generation: strconv.FormatUint(captured.generation, 10)}
}

// getHistory is the private paged history read: one admitted call taking
// exactly one owned history capture — the committed facts and revision, no
// Operations, FIFOs, or usage — and projecting the whole conversation before
// slicing, so an assistant and its tool result never split.
func (r *Runtime) getHistory(ctx context.Context, sessionID string, cursor *string) (protocol.HistoryPage, error) {
	var page protocol.HistoryPage
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		facts, revision, err := h.ReadSessionHistory(ctx, sessionID)
		if err != nil {
			return err
		}
		page, err = projectHistoryPage(sessionID, facts, revision, cursor)
		return err
	}); err != nil {
		return protocol.HistoryPage{}, err
	}
	return page, nil
}

// buildHydration is the private Session-owned hydration builder: one Harness
// snapshot and one captured configuration assemble every body member under
// that snapshot revision, and one captured warning snapshot supplies the
// warning presentation with its independently revisioned clock — the warning
// capture happens exactly once per read and is not another Session read.
func (r *Runtime) buildHydration(ctx context.Context, sessionID string) (protocol.Hydration, error) {
	var hydration protocol.Hydration
	if err := r.withSessionSnapshot(ctx, sessionID, func(snap harness.SessionSnapshot, captured *configuration) error {
		warningsRevision, warnings := r.warnings.hydrate(sessionID)
		var err error
		hydration, err = projectHydration(snap, captured, warnings, warningsRevision)
		return err
	}); err != nil {
		return protocol.Hydration{}, err
	}
	return hydration, nil
}

// resolveBoundaryEntry resolves one client boundary item ID against one owned
// history capture's committed user-origin input facts and their
// namespace-derived projected identities, returning the private entry
// identity for the Harness Fork command. It is pure over the capture, so the
// admitted fork command and every test share exactly one rule. A foreign,
// non-user, or nonexistent item — and an empty boundary item — fails with the
// shared harness.ErrInvalid sentinel.
func resolveBoundaryEntry(sessionID string, facts []harness.HistoryFact, boundaryItemID string) (string, error) {
	for _, fact := range facts {
		if fact.Kind == harness.EntryInput && fact.Input.Origin == harness.InputOriginUser &&
			projectItemID(sessionID, fact.EntryID) == boundaryItemID {
			return fact.EntryID, nil
		}
	}
	return "", fmt.Errorf("boundary item %q is not a committed user input of session %q: %w", boundaryItemID, sessionID, harness.ErrInvalid)
}
