package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/MMinasyan/lightcode/harness"
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
