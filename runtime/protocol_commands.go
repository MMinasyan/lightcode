package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// createSessionHeader is the protocol-shaped root Session creation: one
// admitted call runs the shared creation core and then projects the header
// from a real SnapshotSession taken after the transition, so the revision
// pair is the coordinator's own publication. A failed fresh read propagates
// its typed error; the committed creation is never rolled back and no
// synthetic revision is invented.
func (r *Runtime) createSessionHeader(ctx context.Context, workspace, agentType string) (protocol.Session, error) {
	var header protocol.Session
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		record, err := r.createSessionRecord(ctx, h, workspace, agentType)
		if err != nil {
			return err
		}
		header, err = sessionHeaderAfter(ctx, h, record.Identity.SessionID)
		return err
	}); err != nil {
		return protocol.Session{}, err
	}
	return header, nil
}

// submitSession is the protocol-shaped Session message submission: the wire
// body is converted once, the origin is the fixed caller origin (the wire
// carries none), the mode is a plain cast so the existing Harness validator
// decides the closed set, and the outcome projects the Harness-returned
// record. A buffered item's result keeps Operation nil even after a later
// drain admits work under that identity.
func (r *Runtime) submitSession(ctx context.Context, sessionID string, req protocol.SubmitRequest) (protocol.SubmitResult, error) {
	content, err := convertContentParts(req.Content)
	if err != nil {
		return protocol.SubmitResult{}, err
	}
	var result protocol.SubmitResult
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		out, err := h.Submit(ctx, harness.SubmitRequest{
			SessionID:   sessionID,
			OperationID: req.OperationId,
			Origin:      harness.InputOriginUser,
			Content:     content,
			Mode:        harness.MessageMode(req.Mode),
		})
		if err != nil {
			return err
		}
		result = projectSubmitResult(out)
		return nil
	}); err != nil {
		return protocol.SubmitResult{}, err
	}
	return result, nil
}

// projectSubmitResult maps the Harness outcome: the disposition enum is the
// same closed set, and only an admitted or existing Operation is projected.
func projectSubmitResult(out harness.SubmitResult) protocol.SubmitResult {
	result := protocol.SubmitResult{Disposition: protocol.SubmitResultDisposition(out.Disposition)}
	if out.Operation != nil {
		operation := projectOperation(*out.Operation)
		result.Operation = &operation
	}
	return result
}

// compactSession is the protocol-shaped manual compaction over one idle
// Session: the contiguous admission discipline stays entirely with the
// Harness, and the returned Operation is its committed record.
func (r *Runtime) compactSession(ctx context.Context, sessionID string, req protocol.CompactRequest) (protocol.Operation, error) {
	var operation protocol.Operation
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		record, err := h.Compact(ctx, harness.CompactRequest{SessionID: sessionID, OperationID: req.OperationId})
		if err != nil {
			return err
		}
		operation = projectOperation(record)
		return nil
	}); err != nil {
		return protocol.Operation{}, err
	}
	return operation, nil
}

// forkSession is the protocol-shaped Fork command. One admitted call resolves
// the raw boundary entry — from an already-committed destination of this
// request when one exists, otherwise from the live source snapshot — then
// delegates to Harness.Fork with that raw identity, so first-writer/conflict
// and the normal new-fork transaction stay Harness authority. The destination
// header comes from a real post-transition snapshot.
func (r *Runtime) forkSession(ctx context.Context, sessionID string, req protocol.ForkRequest) (protocol.ForkResult, error) {
	content, err := convertContentParts(req.Content)
	if err != nil {
		return protocol.ForkResult{}, err
	}
	var result protocol.ForkResult
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		boundaryEntryID, err := forkBoundaryEntry(ctx, h, sessionID, req)
		if err != nil {
			return err
		}
		out, err := h.Fork(ctx, harness.ForkRequest{
			SourceSessionID: sessionID,
			BoundaryEntryID: boundaryEntryID,
			OperationID:     req.OperationId,
			Content:         content,
		})
		if err != nil {
			return err
		}
		header, err := sessionHeaderAfter(ctx, h, out.Session.Identity.SessionID)
		if err != nil {
			return err
		}
		result = protocol.ForkResult{Session: header, Operation: projectOperation(out.Operation)}
		return nil
	}); err != nil {
		return protocol.ForkResult{}, err
	}
	return result, nil
}

// forkBoundaryEntry resolves the raw boundary entry identity for one fork
// request inside the caller's single admission. The live source snapshot
// resolves the projected boundary first and, on success, is returned
// immediately — a valid source never falls back to a destination scan, so an
// invalid boundary item fails exactly like the single-read resolver. Only a
// typed-unavailable source (deleted or corrupt) scans an already-committed
// destination of this caller Operation ID whose durable lineage names this
// source Session, whose lineage boundary projects to the requested boundary
// item, and whose located Operation is a message Operation; that raw lineage
// boundary is returned and the Harness still decides first-writer/conflict,
// so an idempotent retry still resolves after the source Session is deleted
// or cold-corrupt. The scan reads only the public validated
// ListSessions/ReadOperation producers, never storage or an index, and an
// absent or corrupt destination is skipped without reinterpretation. With no
// valid matching lineage the original source error returns unchanged. Other
// source errors propagate unchanged. Ceiling: the unavailable-source scan is
// O(Sessions) and may validate a cold coordinator; no dedup or index state is
// added, and every normal new fork is revalidated by the Harness transaction.
func forkBoundaryEntry(ctx context.Context, h *harness.Harness, sourceID string, req protocol.ForkRequest) (string, error) {
	snap, err := h.SnapshotSession(ctx, sourceID)
	if err == nil {
		return resolveBoundaryEntry(snap, req.BoundaryItemId)
	}
	if !errors.Is(err, harness.ErrNotFound) && !errors.Is(err, harness.ErrCorrupt) {
		return "", err
	}
	sourceErr := err
	records, err := h.ListSessions(ctx)
	if err != nil {
		return "", err
	}
	for _, record := range records {
		boundary := record.Identity.SourceBoundaryEntryID
		if record.Identity.SourceSessionID != sourceID || boundary == "" {
			continue
		}
		if projectItemID(sourceID, boundary) != req.BoundaryItemId {
			continue
		}
		operation, err := h.ReadOperation(ctx, record.Identity.SessionID, req.OperationId)
		if errors.Is(err, harness.ErrNotFound) || errors.Is(err, harness.ErrCorrupt) {
			continue // not a valid matching destination
		}
		if err != nil {
			return "", err
		}
		if operation.Admission.RequestKind != harness.RequestKindMessage {
			continue
		}
		return boundary, nil
	}
	return "", sourceErr
}

// setSessionAgentType is the protocol-shaped Agent-type change: the Harness
// keeps its nonempty-only selection (an unknown type fails at the next
// preparation, not here), and the returned header is the real
// post-transition snapshot.
func (r *Runtime) setSessionAgentType(ctx context.Context, sessionID string, req protocol.SetSessionAgentTypeRequest) (protocol.Session, error) {
	var header protocol.Session
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		if _, err := h.ChangeAgentType(ctx, sessionID, req.AgentType); err != nil {
			return err
		}
		var err error
		header, err = sessionHeaderAfter(ctx, h, sessionID)
		return err
	}); err != nil {
		return protocol.Session{}, err
	}
	return header, nil
}

// archiveSession is the protocol-shaped archive: the Harness-owned
// idle/live-background gates stay in force, and the archived header is the
// real post-transition snapshot.
func (r *Runtime) archiveSession(ctx context.Context, sessionID string) (protocol.Session, error) {
	var header protocol.Session
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		if _, err := h.ArchiveSession(ctx, sessionID); err != nil {
			return err
		}
		var err error
		header, err = sessionHeaderAfter(ctx, h, sessionID)
		return err
	}); err != nil {
		return protocol.Session{}, err
	}
	return header, nil
}

// reopenSession is the protocol-shaped reopen: an already open Session is the
// Harness no-write success, and the header is the real post-transition
// snapshot.
func (r *Runtime) reopenSession(ctx context.Context, sessionID string) (protocol.Session, error) {
	var header protocol.Session
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		if _, err := h.ReopenSession(ctx, sessionID); err != nil {
			return err
		}
		var err error
		header, err = sessionHeaderAfter(ctx, h, sessionID)
		return err
	}); err != nil {
		return protocol.Session{}, err
	}
	return header, nil
}

// interruptSession delegates to the Harness interrupt and returns with it:
// the control is issued immediately and this call never waits for the
// Operation's later terminal commit.
func (r *Runtime) interruptSession(ctx context.Context, sessionID string) error {
	return r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		return h.Interrupt(ctx, sessionID)
	})
}

// stopSession delegates to the full background convergence wait owned by
// Harness.Stop; the root active Operation and pending buffers are not touched
// by this facade.
func (r *Runtime) stopSession(ctx context.Context, sessionID string) error {
	return r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		return h.Stop(ctx, sessionID)
	})
}

// sessionHeaderAfter projects one Session header from a fresh SnapshotSession
// taken inside the caller's admission after a committed transition. A failing
// fresh read returns its typed error; no synthetic revision is invented.
func sessionHeaderAfter(ctx context.Context, h *harness.Harness, sessionID string) (protocol.Session, error) {
	snap, err := h.SnapshotSession(ctx, sessionID)
	if err != nil {
		return protocol.Session{}, err
	}
	return projectSession(snap), nil
}

// convertContentParts maps one generated content union list to canonical
// model parts through the generated accessors. It performs union decoding
// only: the Harness accepting boundary validates and clones every part
// through model.NewContentPart before any admission or effect, for both
// Submit and Fork. Unknown discriminator and union decode failures wrap the
// shared harness.ErrInvalid sentinel.
func convertContentParts(parts []protocol.ContentPart) ([]model.ContentPart, error) {
	out := make([]model.ContentPart, 0, len(parts))
	for i, wire := range parts {
		part, err := convertContentPart(wire)
		if err != nil {
			return nil, fmt.Errorf("content[%d]: %w", i, err)
		}
		out = append(out, part)
	}
	return out, nil
}

// convertContentPart maps one generated union member to its canonical kind
// with exactly that kind's field; extras ride the identical raw-valued map
// conversion, so numbers and opaque extras are preserved. It adds no
// model-semantic validation: the Harness boundary owns that rule.
func convertContentPart(wire protocol.ContentPart) (model.ContentPart, error) {
	invalid := func(format string, args ...any) (model.ContentPart, error) {
		return model.ContentPart{}, fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), harness.ErrInvalid)
	}
	kind, err := wire.Discriminator()
	if err != nil {
		return invalid("decode content part: %v", err)
	}
	var part model.ContentPart
	switch kind {
	case "text":
		value, err := wire.AsTextPart()
		if err != nil {
			return invalid("decode text part: %v", err)
		}
		part = model.ContentPart{Kind: model.PartText, Text: value.Text, Extra: modelExtra(value.Extra)}
	case "image_url":
		value, err := wire.AsImageURLPart()
		if err != nil {
			return invalid("decode image_url part: %v", err)
		}
		part = model.ContentPart{Kind: model.PartImageURL, URL: value.Url, Extra: modelExtra(value.Extra)}
	case "opaque":
		value, err := wire.AsOpaquePart()
		if err != nil {
			return invalid("decode opaque part: %v", err)
		}
		part = model.ContentPart{Kind: model.PartOpaque, OpaqueWireType: value.OpaqueWireType, Extra: modelExtra(value.Extra)}
	default:
		return invalid("content part kind %q is not one of text, image_url or opaque", kind)
	}
	return part, nil
}

// modelExtra converts one generated raw-valued object to the canonical extra
// set by the identical direct map conversion; the Harness accepting boundary
// clones it through model.NewContentPart, so no reference to the wire value
// escapes.
func modelExtra(extra *protocol.JSONObject) model.Extra {
	if extra == nil {
		return nil
	}
	return model.Extra(*extra)
}
