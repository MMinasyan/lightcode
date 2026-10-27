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
	summary := harness.OperationSummary{
		OperationID: record.Admission.OperationID,
		RequestKind: record.Admission.RequestKind,
		AgentType:   record.Admission.AgentType,
		Model:       record.Admission.Execution.Model,
		AdmittedAt:  record.Admission.AdmittedAt,
		Status:      record.State.Status,
		SettledAt:   record.State.SettledAt,
		Usage:       record.State.Usage,
	}
	if record.State.Terminal != nil {
		summary.Detail = record.State.Terminal.Detail
	}
	return projectOperationSummary(summary)
}

// projectOperationSummary maps one value-only Operation summary to its
// generated header: the same projection rule the full-register adapter
// consumes.
func projectOperationSummary(summary harness.OperationSummary) protocol.Operation {
	op := protocol.Operation{
		AdmittedAt:  summary.AdmittedAt.UTC(),
		AgentType:   summary.AgentType,
		Model:       summary.Model.String(),
		OperationId: summary.OperationID,
		RequestKind: protocol.OperationRequestKind(summary.RequestKind),
		Status:      protocol.OperationStatus(summary.Status),
		Usage:       projectUsage(summary.Usage),
	}
	if summary.SettledAt != nil {
		settled := summary.SettledAt.UTC()
		op.SettledAt = &settled
	}
	if summary.Detail != "" {
		detail := summary.Detail
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

// projectConversation derives the complete client conversation from one owned
// Session's validated facts, in committed order. It consumes only the facts:
// they are already materialized, validated, and owned, so no storage payload
// is re-decoded and no model-context projection is consulted. Tool results
// never become separate items — every terminal result is absorbed into the
// assistant item that published its call, addressed by its reserved result
// entry identity alone: the validated facts guarantee that identity is
// Session-unique and bound to its publishing assistant entry and call, so
// repeating the binding here would add nothing — while an unresolved call
// keeps its assistant item without terminal members and gains them on a
// later read under the same item ID.
func projectConversation(sessionID string, facts []harness.HistoryFact) ([]protocol.ConversationItem, error) {
	results := indexToolResults(facts)
	items := make([]protocol.ConversationItem, 0, len(facts))
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

// indexToolResults maps the facts' terminal tool results by their
// reserved result entry identities — Session-unique under the validated
// facts' own reservation rule. Unmatched identities cannot exist in
// validated facts and simply never match.
func indexToolResults(facts []harness.HistoryFact) map[string]harness.ToolResultEntry {
	index := make(map[string]harness.ToolResultEntry)
	for _, fact := range facts {
		if fact.Kind != harness.EntryToolResult {
			continue
		}
		result := *fact.ToolResult
		index[result.EntryID] = result
	}
	return index
}

// projectConversationItem maps one validated fact to its generated client
// item: one item identity is derived per produced item, before its generated
// variant is constructed, and shared by the variant and the assistant/signal
// projectors. Tool-result facts are absorbed into their publishing assistant
// and produce no item of their own.
func projectConversationItem(sessionID string, fact harness.HistoryFact, results map[string]harness.ToolResultEntry) (*protocol.ConversationItem, error) {
	if fact.Kind == harness.EntryToolResult {
		return nil, nil
	}
	id := projectItemID(sessionID, fact.EntryID)
	var item protocol.ConversationItem
	var err error
	switch fact.Kind {
	case harness.EntryInput:
		content, contentErr := projectContentParts(fact.Input.Content)
		if contentErr != nil {
			return nil, fmt.Errorf("project input entry %q: %w", fact.EntryID, contentErr)
		}
		err = item.FromInputItem(protocol.InputItem{
			ItemId:      id,
			CommittedAt: fact.CommittedAt.UTC(),
			OperationId: operationAttribution(fact.OperationID),
			Origin:      protocol.InputOrigin(fact.Input.Origin),
			Content:     content,
		})
	case harness.EntryAssistant:
		assistant, assistantErr := projectAssistantItem(id, fact, results)
		if assistantErr != nil {
			return nil, assistantErr
		}
		err = item.FromAssistantItem(assistant)
	case harness.EntrySignal:
		err = item.FromSignalItem(projectSignalItem(id, fact))
	case harness.EntryCompaction:
		err = item.FromCompactionItem(protocol.CompactionItem{
			ItemId:      id,
			CommittedAt: fact.CommittedAt.UTC(),
			OperationId: operationAttribution(fact.OperationID),
			Summary:     fact.Compaction.Summary,
			Model:       fact.Compaction.Model.String(),
		})
	case harness.EntryOperationSettlement:
		err = item.FromOperationEndItem(protocol.OperationEndItem{
			ItemId:      id,
			CommittedAt: fact.CommittedAt.UTC(),
			OperationId: operationAttribution(fact.OperationID),
			Status:      protocol.OperationEndItemStatus(fact.Settlement.Status),
			Detail:      fact.Settlement.Detail,
		})
	default: // absorbed into no item
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("project %s entry %q: %w", fact.Kind, fact.EntryID, err)
	}
	return &item, nil
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
// item under its already-derived stable identity: the published calls in
// their validated order, each carrying its terminal tool result's outcome
// when that result has committed.
func projectAssistantItem(id string, fact harness.HistoryFact, results map[string]harness.ToolResultEntry) (protocol.AssistantItem, error) {
	entry := fact.Assistant
	content, err := projectContentParts(entry.Content)
	if err != nil {
		return protocol.AssistantItem{}, fmt.Errorf("project assistant entry %q: %w", fact.EntryID, err)
	}
	calls := make([]protocol.ToolCallView, 0, len(entry.ToolCalls))
	for _, call := range entry.ToolCalls {
		view, err := projectToolCall(call, results)
		if err != nil {
			return protocol.AssistantItem{}, fmt.Errorf("project assistant entry %q: %w", fact.EntryID, err)
		}
		calls = append(calls, view)
	}
	item := protocol.AssistantItem{
		ItemId:      id,
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
// result matched by the call's reserved result identity contributes its
// status, its model-visible content (an empty success content stays present),
// its preserved tool-owned metadata for every terminal status alike, and its
// committed execution evidence; a pending call omits all four and never gains
// a fabricated outcome.
func projectToolCall(call harness.ToolCallRecord, results map[string]harness.ToolResultEntry) (protocol.ToolCallView, error) {
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
	result, ok := results[call.ResultEntryID]
	if !ok {
		return view, nil
	}
	status := protocol.ToolCallStatus(result.Status)
	content := result.Content
	evidence := protocol.ExecutionEvidence(result.Evidence)
	view.Status = &status
	view.Content = &content
	view.Evidence = &evidence
	if len(result.Metadata) > 0 {
		metadata := protocol.ToolMetadata(result.Metadata)
		view.Metadata = &metadata
	}
	return view, nil
}

// projectSignalItem maps one validated signal entry to its generated item
// under its already-derived stable identity. The subtype comes from the typed
// payload kind, never from content matching.
func projectSignalItem(id string, fact harness.HistoryFact) protocol.SignalItem {
	item := protocol.SignalItem{
		ItemId:      id,
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

// historyCursor is the private opaque cursor payload: exactly these two
// members on the wire, base64url-encoded.
type historyCursor struct {
	SessionID    string `json:"session_id"`
	AnchorItemID string `json:"anchor_item_id"`
}

// encodeHistoryCursor encodes one cursor as its base64url JSON form; the
// primitive-field members' marshal cannot fail.
func encodeHistoryCursor(c historyCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

// decodeHistoryCursor decodes one opaque cursor strictly: base64url, one
// JSON document with exactly the cursor members, then the Session identity.
// Every failure wraps the shared harness.ErrInvalid sentinel; no other error
// class exists. The anchor's emptiness needs no decoder rule — no valid item
// identity is empty — the resolver rejects it uniformly with every other
// non-member.
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
	if c.SessionID != sessionID {
		return invalid("history cursor names session %q, not %q", c.SessionID, sessionID)
	}
	return c, nil
}

// headerFormsItem reports whether one committed entry kind produces a client
// conversation item; a terminal tool result is absorbed into its publishing
// assistant item instead.
func headerFormsItem(kind harness.EntryKind) bool {
	switch kind {
	case harness.EntryInput, harness.EntryAssistant, harness.EntrySignal,
		harness.EntryCompaction, harness.EntryOperationSettlement:
		return true
	default:
		return false
	}
}

// selectHistoryPage resolves one anchored page among the item-forming headers
// of one captured history view: exactly historyPageSize entry identities in
// ascending display order, the newest 50 for the nil initial cursor,
// otherwise the newest items strictly before the anchor item — whose identity
// is resolved among those same headers. The oldest selected item anchors the
// next older cursor exactly when older items remain. A nil cursor is the
// initial page; an empty supplied cursor, a malformed document, or an anchor
// that resolves to no item of this Session fails with harness.ErrInvalid.
func selectHistoryPage(sessionID string, headers []harness.HistoryEntryHeader, cursor *string) ([]string, *string, error) {
	items := make([]int, 0, len(headers))
	for i, header := range headers {
		if headerFormsItem(header.Kind) {
			items = append(items, i)
		}
	}
	end := len(items)
	if cursor != nil {
		decoded, err := decodeHistoryCursor(*cursor, sessionID)
		if err != nil {
			return nil, nil, err
		}
		end = -1
		for i, index := range items {
			if projectItemID(sessionID, headers[index].EntryID) == decoded.AnchorItemID {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, nil, fmt.Errorf("history cursor anchor %q names no item of session %q: %w", decoded.AnchorItemID, sessionID, harness.ErrInvalid)
		}
	}
	start := max(end-historyPageSize, 0)
	entryIDs := make([]string, 0, end-start)
	for _, index := range items[start:end] {
		entryIDs = append(entryIDs, headers[index].EntryID)
	}
	older := (*string)(nil)
	if start > 0 {
		encoded := encodeHistoryCursor(historyCursor{SessionID: sessionID, AnchorItemID: projectItemID(sessionID, headers[items[start]].EntryID)})
		older = &encoded
	}
	return entryIDs, older, nil
}

// historyPageFromView projects one captured history view's selected page: the
// page's entry identities are selected among the item-forming headers, their
// facts are requested by identity, and the selected assistants' reserved
// result identities are resolved to their committed results through the same
// view — including results outside the base-entry sequence range — so every
// terminal outcome attaches to its publishing call exactly as in the complete
// projection. Pending calls stay pending; results never become separate
// items.
func historyPageFromView(sessionID string, view harness.HistoryReadView, cursor *string) (protocol.HistoryPage, error) {
	entryIDs, older, err := selectHistoryPage(sessionID, view.HistoryHeaders(), cursor)
	if err != nil {
		return protocol.HistoryPage{}, err
	}
	facts := view.HistoryFacts(entryIDs)
	var resultIDs []string
	for _, fact := range facts {
		if fact.Kind != harness.EntryAssistant {
			continue
		}
		for _, call := range fact.Assistant.ToolCalls {
			if call.ResultEntryID != "" {
				resultIDs = append(resultIDs, call.ResultEntryID)
			}
		}
	}
	facts = append(facts, view.HistoryFacts(resultIDs)...)
	items, err := projectConversation(sessionID, facts)
	if err != nil {
		return protocol.HistoryPage{}, err
	}
	page := protocol.HistoryPage{
		SessionRevision: wireRevision(view.Revision),
		Items:           items, // projectConversation already returns an owned non-nil slice
		OlderCursor:     older,
	}
	return page, nil
}

// projectPending maps one composed Session view's both pending FIFOs to their
// generated shape, reusing the conversation content projection. Queue
// inspection rides hydration; no separate pending read exists.
func projectPending(view harness.SessionReadView) (protocol.PendingQueues, error) {
	steering, err := projectPendingInputs(view.Steering)
	if err != nil {
		return protocol.PendingQueues{}, err
	}
	queued, err := projectPendingInputs(view.Queued)
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

// projectSessionUsage maps one composed Session view's usage onto its two
// distinct clocks: the canonical register totals as they stand, and the
// display estimate plus the context window of the shared selection
// resolution.
func projectSessionUsage(view harness.SessionReadView, measured *harness.UsageCount, selection sessionSelection) protocol.UsageProjection {
	return protocol.UsageProjection{
		Totals:  projectUsage(view.TotalUsage),
		Context: protocol.UsageContext{UsedTokens: projectUsedTokens(measured), ContextWindow: sessionContextWindow(view, selection)},
	}
}

// projectUsedTokens is the display estimate of one measured count: the exact
// mathematical input+cached sum — mixed signs stay negative-exact and a sum
// beyond MaxInt64 stays positive-exact — with no later measurement rendering
// zero.
func projectUsedTokens(measured *harness.UsageCount) string {
	if measured == nil {
		return "0"
	}
	// The reported counts are signed int64: the display value is the exact
	// mathematical input+cached sum, so the one addition rides math/big with
	// no clamping or hand-rolled overflow cases.
	return new(big.Int).Add(big.NewInt(measured.InputTokens), big.NewInt(measured.CachedInputTokens)).String()
}

// sessionContextWindow resolves the context window: the durably running
// current Operation's captured window while one is admitted — a retiring run
// or reservation alone is not an active Operation — otherwise the shared
// selection's catalog window. An unavailable selection reports 0 rather than
// an admission error; a usable model always has a positive window, so 0
// never invents a model.
func sessionContextWindow(view harness.SessionReadView, selection sessionSelection) int {
	if view.Header.CurrentOperationID != "" {
		return view.ActiveContextWindow
	}
	return selection.window
}

// projectHydration assembles the complete Session-owned hydration body from
// one composed Session view, its already-selected newest conversation page,
// the view's newest context measurement, and one captured configuration: the
// Session header, the next-admission model selection, the sorted Operation
// headers with the active pointer into them, both pending FIFOs, background
// membership, the usage projection, and the warning presentation — every
// global group plus this Session's own prompt and protocol warnings, under
// the independently captured warning revision.
func projectHydration(view harness.SessionReadView, page protocol.ConversationPage, measured *harness.UsageCount, captured *configuration, warnings []protocol.Warning, warningsRevision uint64) (protocol.Hydration, error) {
	pending, err := projectPending(view)
	if err != nil {
		return protocol.Hydration{}, err
	}
	operations := make([]protocol.Operation, 0, len(view.Operations))
	var active *protocol.Operation
	for _, summary := range view.Operations {
		projected := projectOperationSummary(summary)
		if summary.OperationID == view.Header.CurrentOperationID {
			operation := projected
			active = &operation
		}
		operations = append(operations, projected)
	}
	background := make([]protocol.BackgroundMember, 0, len(view.Background))
	for _, member := range view.Background {
		background = append(background, protocol.BackgroundMember{Kind: protocol.BackgroundMemberKind(member.Kind), Id: member.ID})
	}
	selection := resolveSessionSelection(view.Header.CurrentAgentType, captured)
	var selectedModel *protocol.ModelRef
	if selection.model != nil {
		rendered := selection.model.String()
		selectedModel = &rendered
	}
	return protocol.Hydration{
		Session:               projectSession(view.Header),
		SessionRevision:       wireRevision(view.History.Revision),
		SelectedModel:         selectedModel,
		Operations:            operations,
		ActiveOperation:       active,
		Pending:               pending,
		Conversation:          page,
		Usage:                 projectSessionUsage(view, measured, selection),
		Background:            background,
		Warnings:              warnings, // the shared read producer returns owned non-nil slices
		WarningsRevision:      protocol.WarningsRevision{Revision: formatWarningRevision(warningsRevision)},
		ConfigurationRevision: configurationRevision(captured),
	}, nil
}

// configurationRevision derives the wire configuration revision from one
// captured configuration: the publication generation as a decimal string, with
// the empty pre-server instance identity.
func configurationRevision(captured *configuration) protocol.ConfigurationRevision {
	return protocol.ConfigurationRevision{Generation: strconv.FormatUint(captured.generation, 10)}
}

// getHistory is the private paged history read: one admitted call taking
// exactly one narrow history capture and selecting the page's items before
// requesting their facts, so an assistant and its tool result never split.
func (r *Runtime) getHistory(ctx context.Context, sessionID string, cursor *string) (protocol.HistoryPage, error) {
	var page protocol.HistoryPage
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		view, err := h.ReadHistoryView(ctx, sessionID)
		if err != nil {
			return err
		}
		page, err = historyPageFromView(sessionID, view, cursor)
		return err
	}); err != nil {
		return protocol.HistoryPage{}, err
	}
	return page, nil
}

// buildHydration is the private Session-owned hydration builder: one Harness
// admission takes the captured configuration, the composed Session view, the
// newest conversation page selected from the same view, its newest context
// measurement, and one captured warning snapshot supplying the warning
// presentation with its independently revisioned clock — the warning capture
// happens exactly once per read and is not another Session read.
func (r *Runtime) buildHydration(ctx context.Context, sessionID string) (protocol.Hydration, error) {
	var hydration protocol.Hydration
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		captured := r.config.current()
		view, err := h.ReadSessionView(ctx, sessionID)
		if err != nil {
			return err
		}
		warningsRevision, warnings := r.warnings.hydrate(sessionID)
		page, err := historyPageFromView(sessionID, view.History, nil)
		if err != nil {
			return err
		}
		hydration, err = projectHydration(view, protocol.ConversationPage{Items: page.Items, OlderCursor: page.OlderCursor}, view.History.LatestContextUsage(), captured, warnings, warningsRevision)
		return err
	}); err != nil {
		return protocol.Hydration{}, err
	}
	return hydration, nil
}

// resolveBoundaryEntry resolves one client boundary item ID against one
// captured history view's committed user-origin input headers and their
// namespace-derived projected identities, returning the private entry
// identity for the Harness Fork command. It is pure over the value metadata,
// so the admitted fork command and every test share exactly one rule. A
// foreign, non-user, or nonexistent item — and an empty boundary item — fails
// with the shared harness.ErrInvalid sentinel.
func resolveBoundaryEntry(sessionID string, headers []harness.HistoryEntryHeader, boundaryItemID string) (string, error) {
	for _, header := range headers {
		if header.Kind == harness.EntryInput && header.InputOrigin == harness.InputOriginUser &&
			projectItemID(sessionID, header.EntryID) == boundaryItemID {
			return header.EntryID, nil
		}
	}
	return "", fmt.Errorf("boundary item %q is not a committed user input of session %q: %w", boundaryItemID, sessionID, harness.ErrInvalid)
}
