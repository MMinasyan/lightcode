package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// getSession projects one Session's header from one coherent Harness
// snapshot inside a single admitted call. A deleted or corrupt Session
// returns the Harness's own typed error unchanged; the projection adds no
// validation of already-validated facts.
func (r *Runtime) getSession(ctx context.Context, sessionID string) (protocol.Session, error) {
	var header protocol.Session
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		snap, err := h.SnapshotSession(ctx, sessionID)
		if err != nil {
			return err
		}
		header = projectSession(snap)
		return nil
	}); err != nil {
		return protocol.Session{}, err
	}
	return header, nil
}

// listSessions projects Session headers under one required workspace and
// lifecycle filter set, inside a single admitted call. Absent, empty, or
// unknown filter members and a failing workspace normalization wrap the
// shared harness.ErrInvalid sentinel; a nonempty relative workspace is
// normalized with filepath.Abs before filtering. The scan is fresh: every
// candidate is re-read through SnapshotSession, so a candidate that became
// deleted or corrupt is skipped while its valid siblings stay listed; any
// other failure propagates with no partial list.
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
		records, err := h.ListSessions(ctx)
		if err != nil {
			return err
		}
		headers = make([]protocol.Session, 0, len(records))
		for _, record := range records {
			if record.Identity.Workspace != workspace {
				continue // workspace identity is immutable under every supported mutation
			}
			snap, err := h.SnapshotSession(ctx, record.Identity.SessionID)
			if err != nil {
				if errors.Is(err, harness.ErrNotFound) || errors.Is(err, harness.ErrCorrupt) {
					continue // the candidate became unavailable: absent, like the fresh scan's own rule
				}
				return err
			}
			if snap.Session.State.Lifecycle != harness.SessionLifecycle(params.Lifecycle) {
				continue
			}
			headers = append(headers, projectSession(snap))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return headers, nil
}

// listWorkspaces projects the distinct Workspace roots of every validated
// Session from one fresh scan, sorted by root, inside a single admitted call.
// It is derived navigation: no Project record, count, or owning revision.
func (r *Runtime) listWorkspaces(ctx context.Context) ([]protocol.Workspace, error) {
	var out []protocol.Workspace
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		records, err := h.ListSessions(ctx)
		if err != nil {
			return err
		}
		roots := make([]string, 0, len(records))
		seen := make(map[string]bool, len(records))
		for _, record := range records {
			root := record.Identity.Workspace
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

// projectSession maps one owned snapshot to its generated Session header.
// Every field comes from the snapshot's validated record and coordinator
// counter; timestamps are UTC. The instance identifier stays empty in these
// pre-server values: the protocol server supplies it before wire emission.
func projectSession(snap harness.SessionSnapshot) protocol.Session {
	record := snap.Session
	header := protocol.Session{
		AgentType:       record.State.CurrentAgentType,
		CreatedAt:       record.Identity.CreatedAt.UTC(),
		LastActivity:    record.State.LastActivity.UTC(),
		Lifecycle:       protocol.SessionLifecycle(record.State.Lifecycle),
		SessionId:       record.Identity.SessionID,
		SessionRevision: protocol.SessionRevision{DurableRevision: strconv.FormatInt(record.Revision, 10), LocalRevision: strconv.FormatUint(snap.LocalRevision, 10)},
		Workspace:       record.Identity.Workspace,
	}
	if record.State.ArchivedAt != nil {
		archived := record.State.ArchivedAt.UTC()
		header.ArchivedAt = &archived
	}
	if record.State.CurrentOperationID != "" {
		current := record.State.CurrentOperationID
		header.CurrentOperationId = &current
	}
	if record.Identity.ParentSessionID != "" {
		parent := record.Identity.ParentSessionID
		header.ParentSessionId = &parent
	}
	if record.Identity.SourceSessionID != "" {
		source := record.Identity.SourceSessionID
		header.SourceSessionId = &source
	}
	return header
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

// projectConversation derives the complete client conversation from one owned
// Session snapshot's validated facts, in committed order. It consumes only
// the snapshot: the facts are already materialized, validated, and owned, so
// no storage payload is re-decoded and no model-context projection is
// consulted. Tool results never become separate items — every terminal
// result is absorbed into the assistant item that published its call, matched
// by the full assistant-entry reference, the reserved result entry identity,
// and the call ID — while an unresolved call keeps its assistant item without
// terminal members and gains them on a later read under the same item ID.
func projectConversation(snap harness.SessionSnapshot) ([]protocol.ConversationItem, error) {
	sessionID := snap.Session.Identity.SessionID
	results := indexToolResults(snap.Facts)
	items := make([]protocol.ConversationItem, 0, len(snap.Facts))
	for _, fact := range snap.Facts {
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
// item. Tool-result facts are absorbed into their publishing assistant and
// produce no item of their own.
func projectConversationItem(sessionID string, fact harness.HistoryFact, results map[toolResultKey]harness.ToolResultEntry) (*protocol.ConversationItem, error) {
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
