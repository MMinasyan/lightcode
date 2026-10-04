package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// openProjectionRuntime opens one composed Runtime over the given store with
// the controlled preparation and both agent types.
func openProjectionRuntime(t *testing.T, store harness.Storage) (*Runtime, *ownerEnv) {
	t.Helper()
	e := newOwnerEnv(t)
	writeServiceFile(t, agents.PathForConfig(e.configPath), lifecycleAgentsDocument)
	r, err := e.open(context.Background(), e.storagePlugin(store))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return r, e
}

// closeProjectionRuntime joins one test owner's shutdown. Every test defers
// it immediately inside its eachPrepStore callback, so the owner closes
// before the callback returns and the SQLite store closes after it.
func closeProjectionRuntime(r *Runtime) {
	_ = r.Close(context.Background())
}

func projectionSession(t *testing.T, r *Runtime, workspace, agentType string) harness.SessionRecord {
	t.Helper()
	rec, err := r.createSession(context.Background(), workspace, agentType)
	if err != nil {
		t.Fatalf("createSession(%s): %v", workspace, err)
	}
	return rec
}

func snapshotThroughRuntime(t *testing.T, r *Runtime, sessionID string) harness.SessionSnapshot {
	t.Helper()
	var snap harness.SessionSnapshot
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		snap, err = h.SnapshotSession(ctx, sessionID)
		return err
	})
	if err != nil {
		t.Fatalf("SnapshotSession(%s): %v", sessionID, err)
	}
	return snap
}

// headerThroughRuntime reads one Session's owned metadata header through the
// Runtime's admission gate: the narrow producer list rows and command results
// consume.
func headerThroughRuntime(t *testing.T, r *Runtime, sessionID string) harness.SessionHeader {
	t.Helper()
	var header harness.SessionHeader
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		header, err = h.ReadSessionHeader(ctx, sessionID)
		return err
	})
	if err != nil {
		t.Fatalf("ReadSessionHeader(%s): %v", sessionID, err)
	}
	return header
}

// historyThroughRuntime reads one Session's owned committed history and
// revision through the Runtime's admission gate: the narrow producer the
// live fork boundary consumes.
func historyThroughRuntime(t *testing.T, r *Runtime, sessionID string) ([]harness.HistoryFact, harness.SessionRevision) {
	t.Helper()
	var (
		facts    []harness.HistoryFact
		revision harness.SessionRevision
	)
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		facts, revision, err = h.ReadSessionHistory(ctx, sessionID)
		return err
	})
	if err != nil {
		t.Fatalf("ReadSessionHistory(%s): %v", sessionID, err)
	}
	return facts, revision
}

// headerErrorThroughRuntime reads one Session header through the Runtime's
// admission gate and returns its typed error unchanged, so closed/canceled/
// corrupt/deleted oracles share one admission-gated callback path.
func headerErrorThroughRuntime(ctx context.Context, r *Runtime, sessionID string) error {
	return r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		_, err := h.ReadSessionHeader(ctx, sessionID)
		return err
	})
}

// historyErrorThroughRuntime reads one Session history through the Runtime's
// admission gate and returns its typed error unchanged.
func historyErrorThroughRuntime(ctx context.Context, r *Runtime, sessionID string) error {
	return r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		_, _, err := h.ReadSessionHistory(ctx, sessionID)
		return err
	})
}

// submitQueuedThroughRuntime submits one buffered queued message.
func submitQueuedThroughRuntime(t *testing.T, r *Runtime, sessionID, operationID, text string) {
	t.Helper()
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		res, err := h.Submit(ctx, harness.SubmitRequest{
			SessionID:   sessionID,
			OperationID: operationID,
			Origin:      harness.InputOriginUser,
			Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
			Mode:        harness.MessageModeQueued,
		})
		if err != nil {
			return err
		}
		if res.Disposition != harness.DispositionQueued {
			return errors.New("submit " + operationID + " = " + string(res.Disposition) + ", want queued")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("queued Submit(%q): %v", operationID, err)
	}
}

// forkThroughRuntime forks one source Session at the input entry of the named
// operation and returns the destination identity.
func forkThroughRuntime(t *testing.T, r *Runtime, sourceID, boundaryEntryID, operationID string) string {
	t.Helper()
	var dest string
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		res, err := h.Fork(ctx, harness.ForkRequest{
			SourceSessionID: sourceID,
			BoundaryEntryID: boundaryEntryID,
			OperationID:     operationID,
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "forked"}},
		})
		if err != nil {
			return err
		}
		dest = res.Session.Identity.SessionID
		return nil
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	return dest
}

// launchChildThroughRuntime launches one child Session of the worker type.
func launchChildThroughRuntime(t *testing.T, r *Runtime, parent, operationID string) string {
	t.Helper()
	var childID string
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		res, err := h.LaunchChildSession(ctx, harness.LaunchChildRequest{
			ParentSessionID: parent,
			AgentType:       "worker",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: "child work"}},
			OperationID:     operationID,
			MaxConcurrent:   1,
			OutputLimit:     4096,
		})
		if err != nil {
			return err
		}
		childID = res.ChildSessionID
		return nil
	})
	if err != nil {
		t.Fatalf("LaunchChildSession: %v", err)
	}
	return childID
}

// wantSessionRevision derives the expected wire revision pair from one
// snapshot: the durable register revision and the coordinator-local counter
// as decimal strings, with the empty pre-server instance identity.
func wantSessionRevision(t *testing.T, snap harness.SessionSnapshot) protocol.SessionRevision {
	t.Helper()
	return protocol.SessionRevision{
		InstanceId:      "",
		DurableRevision: strconv.FormatInt(snap.Session.Revision, 10),
		LocalRevision:   strconv.FormatUint(snap.LocalRevision, 10),
	}
}

// inputEntryID resolves one operation's committed input entry through the
// Session snapshot's validated facts.
func inputEntryID(t *testing.T, r *Runtime, sessionID, operationID string) string {
	t.Helper()
	snap := snapshotThroughRuntime(t, r, sessionID)
	for _, fact := range snap.Facts {
		if fact.Kind == harness.EntryInput && fact.OperationID == operationID {
			return fact.EntryID
		}
	}
	t.Fatalf("no input entry owned by %q in session %q", operationID, sessionID)
	return ""
}

// awaitModelArrival waits for the parked execution to reach its model effect.
func awaitModelArrival(t *testing.T, e *ownerEnv) {
	t.Helper()
	select {
	case <-e.prep.modelArrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the execution never reached its model effect")
	}
}

// assertStableHeader checks every header field that is stable while its
// subject holds still: identity, workspace, agent, lifecycle, archive
// absence, the wanted current-operation member, the snapshot times, the UTC
// zone, and the revision pair against the same fresh snapshot.
func assertStableHeader(t *testing.T, header protocol.Session, snap harness.SessionSnapshot, sessionID, workspace, agent, wantCurrent string) {
	t.Helper()
	record := snap.Session
	currentOK := (wantCurrent == "") == (header.CurrentOperationId == nil) &&
		(wantCurrent == "" || *header.CurrentOperationId == wantCurrent)
	if header.SessionId != sessionID ||
		header.Workspace != workspace ||
		header.Lifecycle != protocol.Open ||
		header.AgentType != agent ||
		header.ArchivedAt != nil ||
		!currentOK {
		t.Fatalf("header = %+v, want the open %s header at %s with current operation %q", header, sessionID, workspace, wantCurrent)
	}
	if !header.CreatedAt.Equal(record.Identity.CreatedAt) || !header.LastActivity.Equal(record.State.LastActivity) {
		t.Fatalf("header times = (%v, %v), want the snapshot times (%v, %v)", header.CreatedAt, header.LastActivity, record.Identity.CreatedAt, record.State.LastActivity)
	}
	if loc := header.CreatedAt.Location(); loc != time.UTC {
		t.Fatalf("created_at zone = %v, want UTC", loc)
	}
	if header.SessionRevision != wantSessionRevision(t, snap) {
		t.Fatalf("session_revision = %+v, want the snapshot pair %+v", header.SessionRevision, wantSessionRevision(t, snap))
	}
}

// TestProjectionSessionHeaderRootForkChild proves one producer's exact header
// fields for every lineage kind over both stores. Each subject is asserted
// only while it is actually stable: the root before any Operation starts,
// the fork and child parked on their running models, so no read races the
// root's later child-completion delivery.
func TestProjectionSessionHeaderRootForkChild(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		workspace := filepath.Join(e.home, "lineage")
		root := projectionSession(t, r, workspace, "solo")

		// The root header before any Operation starts.
		rootHeader := projectSession(headerThroughRuntime(t, r, root.Identity.SessionID))
		assertStableHeader(t, rootHeader, snapshotThroughRuntime(t, r, root.Identity.SessionID),
			root.Identity.SessionID, workspace, "solo", "")
		if rootHeader.ParentSessionId != nil || rootHeader.SourceSessionId != nil {
			t.Fatalf("root lineage pointers = (%v, %v), want none", rootHeader.ParentSessionId, rootHeader.SourceSessionId)
		}

		// One settled admission gives the fork its committed input boundary;
		// its terminal cleanup proves the settled idle source Fork requires.
		submitThroughRuntime(t, r, root.Identity.SessionID, "op-1", "hello")
		e.prep.awaitCleanups(1)
		boundary := inputEntryID(t, r, root.Identity.SessionID, "op-1")

		// Park both later models on the one gate: each lineage header is
		// asserted while its own Operation is the running current one.
		gate := make(chan struct{})
		e.prep.modelGate = gate
		release := sync.OnceFunc(func() { close(gate) })
		defer release() // LIFO: the gate releases before the owner close joins

		forked := forkThroughRuntime(t, r, root.Identity.SessionID, boundary, "fork-op-1")
		awaitModelArrival(t, e)
		forkHeader := projectSession(headerThroughRuntime(t, r, forked))
		assertStableHeader(t, forkHeader, snapshotThroughRuntime(t, r, forked),
			forked, workspace, "solo", "fork-op-1")
		if forkHeader.SourceSessionId == nil || *forkHeader.SourceSessionId != root.Identity.SessionID {
			t.Fatalf("fork source = %v, want %s", forkHeader.SourceSessionId, root.Identity.SessionID)
		}
		if forkHeader.ParentSessionId != nil {
			t.Fatalf("fork parent pointer = %v, want none", forkHeader.ParentSessionId)
		}

		child := launchChildThroughRuntime(t, r, root.Identity.SessionID, "child-op-1")
		awaitModelArrival(t, e)
		childHeader := projectSession(headerThroughRuntime(t, r, child))
		assertStableHeader(t, childHeader, snapshotThroughRuntime(t, r, child),
			child, workspace, "worker", "child-op-1")
		if childHeader.ParentSessionId == nil || *childHeader.ParentSessionId != root.Identity.SessionID {
			t.Fatalf("child parent = %v, want %s", childHeader.ParentSessionId, root.Identity.SessionID)
		}
		if childHeader.SourceSessionId != nil {
			t.Fatalf("child source pointer = %v, want none", childHeader.SourceSessionId)
		}

		release()
		e.prep.awaitCleanups(3)
	})
}

// TestProjectionOperationActiveAndTerminal proves the shared Operation
// projector for the running and settled states, including the interrupted
// terminal's required detail.
func TestProjectionOperationActiveAndTerminal(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		session := projectionSession(t, r, filepath.Join(e.home, "ops"), "solo").Identity.SessionID

		gate := make(chan struct{})
		e.prep.modelGate = gate
		submitThroughRuntime(t, r, session, "op-1", "work")
		awaitModelArrival(t, e)

		active := projectOperation(readOperation(t, r, session, "op-1"))
		if active.OperationId != "op-1" || active.RequestKind != protocol.Message ||
			active.Status != protocol.OperationStatusRunning || active.SettledAt != nil || active.Detail != nil ||
			active.AgentType != "solo" || active.Model != "prov/m" || active.AdmittedAt.IsZero() {
			t.Fatalf("active projection = %+v, want the running message op-1 with no terminal members", active)
		}

		close(gate)
		e.prep.awaitCleanups(1)
		record := readOperation(t, r, session, "op-1")
		settled := projectOperation(record)
		if settled.Status != protocol.OperationStatusSuccess || settled.SettledAt == nil || settled.Detail != nil {
			t.Fatalf("settled projection = %+v, want success with a settlement time and no detail", settled)
		}
		if !settled.AdmittedAt.Equal(record.Admission.AdmittedAt) {
			t.Fatal("settled admitted_at drifted from the register admission")
		}

		// The interrupted terminal requires its diagnostic detail, on a fresh
		// Session: the terminal cleanup of the settled row does not imply the
		// old execution's retirement, so the interruption never addresses the
		// just-settled Session.
		interruptedSession := projectionSession(t, r, filepath.Join(e.home, "ops"), "solo").Identity.SessionID
		gate = make(chan struct{})
		e.prep.modelGate = gate
		submitThroughRuntime(t, r, interruptedSession, "op-2", "again")
		awaitModelArrival(t, e)
		err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			return h.Interrupt(ctx, interruptedSession)
		})
		if err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		e.prep.awaitCleanups(2)
		interrupted := projectOperation(readOperation(t, r, interruptedSession, "op-2"))
		if interrupted.Status != protocol.OperationStatusInterruption || interrupted.SettledAt == nil ||
			interrupted.Detail == nil || *interrupted.Detail == "" {
			t.Fatalf("interrupted projection = %+v, want interruption with a required detail", interrupted)
		}
	})
}

// TestProjectionListFilters proves the one-rule filter set at the facade:
// both members required, relative roots normalized, no match an empty list.
func TestProjectionListFilters(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ws1 := filepath.Join(e.home, "first")
		ws2 := filepath.Join(e.home, "second")
		open1 := projectionSession(t, r, ws1, "solo").Identity.SessionID
		archived := projectionSession(t, r, ws2, "solo").Identity.SessionID
		err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, archived)
			return err
		})
		if err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}

		invalid := map[string]protocol.ListSessionsParams{
			"absent workspace":  {Lifecycle: "open"},
			"empty lifecycle":   {Workspace: ws1},
			"unknown lifecycle": {Workspace: ws1, Lifecycle: "paused"},
		}
		for name, params := range invalid {
			if _, err := r.listSessions(context.Background(), params); !errors.Is(err, harness.ErrInvalid) {
				t.Fatalf("%s: listSessions = %v, want harness.ErrInvalid", name, err)
			}
		}

		openList, err := r.listSessions(context.Background(), protocol.ListSessionsParams{Workspace: ws1, Lifecycle: "open"})
		if err != nil {
			t.Fatalf("listSessions open: %v", err)
		}
		if len(openList) != 1 || openList[0].SessionId != open1 {
			t.Fatalf("open list = %+v, want exactly %s", openList, open1)
		}
		archivedList, err := r.listSessions(context.Background(), protocol.ListSessionsParams{Workspace: ws2, Lifecycle: "archived"})
		if err != nil {
			t.Fatalf("listSessions archived: %v", err)
		}
		if len(archivedList) != 1 || archivedList[0].SessionId != archived || archivedList[0].Lifecycle != protocol.Archived || archivedList[0].ArchivedAt == nil {
			t.Fatalf("archived list = %+v, want exactly the archived %s", archivedList, archived)
		}
		noMatch, err := r.listSessions(context.Background(), protocol.ListSessionsParams{Workspace: ws2, Lifecycle: "open"})
		if err != nil {
			t.Fatalf("listSessions no match: %v", err)
		}
		if len(noMatch) != 0 {
			t.Fatalf("no-match list = %+v, want empty", noMatch)
		}

		// A nonempty relative root is normalized before filtering.
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		relative, err := filepath.Rel(cwd, ws1)
		if err != nil {
			t.Fatalf("Rel: %v", err)
		}
		relativeList, err := r.listSessions(context.Background(), protocol.ListSessionsParams{Workspace: relative, Lifecycle: "open"})
		if err != nil {
			t.Fatalf("listSessions relative: %v", err)
		}
		if len(relativeList) != 1 || relativeList[0].SessionId != open1 || relativeList[0].Workspace != ws1 {
			t.Fatalf("relative-root list = %+v, want exactly the normalized %s row", relativeList, ws1)
		}
	})
}

// TestProjectionListAndReadsAroundUnavailableSessions proves a fresh list
// omits deleted and corrupt Sessions without hiding valid siblings, while
// direct reads return the underlying typed errors.
func TestProjectionListAndReadsAroundUnavailableSessions(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		corruptID := "0123456789abcdef0123456789abcdef"
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ws := filepath.Join(e.home, "mixed")
		// The corrupt Session is a seeded cold register: the live Runtime never
		// warmed a coordinator for it, so the fresh scan revalidates and omits it.
		lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: corruptID, Kind: harness.RegisterSession}, `{"not":"a session register"}`)
		valid := projectionSession(t, r, ws, "solo").Identity.SessionID
		deleted := projectionSession(t, r, ws, "solo").Identity.SessionID
		err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, deleted)
			return err
		})
		if err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}
		if err := r.deleteSession(context.Background(), deleted); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}

		list, err := r.listSessions(context.Background(), protocol.ListSessionsParams{Workspace: ws, Lifecycle: "open"})
		if err != nil {
			t.Fatalf("listSessions: %v", err)
		}
		if len(list) != 1 || list[0].SessionId != valid {
			t.Fatalf("list = %+v, want exactly the valid %s", list, valid)
		}

		if err := headerErrorThroughRuntime(context.Background(), r, deleted); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("deleted read = %v, want harness.ErrNotFound", err)
		}
		err = headerErrorThroughRuntime(context.Background(), r, corruptID)
		if !errors.Is(err, harness.ErrCorrupt) {
			t.Fatalf("corrupt read = %v, want the harness.ErrCorrupt class", err)
		}
		var corruptErr *harness.CorruptionError
		if !errors.As(err, &corruptErr) {
			t.Fatal("corrupt read lost its typed CorruptionError identity")
		}
	})
}

// TestProjectionWorkspaceNavigation proves the distinct sorted roots with
// their base-name display names and no other projection.
func TestProjectionWorkspaceNavigation(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		wsA := filepath.Join(e.home, "alpha")
		wsB := filepath.Join(e.home, "beta")
		root := projectionSession(t, r, wsA, "solo").Identity.SessionID
		launchChildThroughRuntime(t, r, root, "child-op-1")
		projectionSession(t, r, wsB, "solo")

		workspaces, err := r.listWorkspaces(context.Background())
		if err != nil {
			t.Fatalf("listWorkspaces: %v", err)
		}
		want := []protocol.Workspace{
			{Root: wsA, DisplayName: filepath.Base(wsA)},
			{Root: wsB, DisplayName: filepath.Base(wsB)},
		}
		if !reflect.DeepEqual(workspaces, want) {
			t.Fatalf("workspaces = %+v, want exactly %+v", workspaces, want)
		}
	})
}

// TestProjectionCallerCopyOwnership proves a returned projection is a caller
// copy: the pre-mutation expectation is immutable and the next read is
// unchanged.
func TestProjectionCallerCopyOwnership(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ws := filepath.Join(e.home, "owned")
		session := projectionSession(t, r, ws, "solo").Identity.SessionID

		header := projectSession(headerThroughRuntime(t, r, session))
		wantHeader := header
		header.AgentType = "mutated"
		header.Workspace = "mutated"
		header.Lifecycle = protocol.Archived
		header.SessionId = "mutated"
		header.SessionRevision.DurableRevision = "0"
		header.CreatedAt = time.Time{}

		list, err := r.listSessions(context.Background(), protocol.ListSessionsParams{Workspace: ws, Lifecycle: "open"})
		if err != nil {
			t.Fatalf("listSessions: %v", err)
		}
		wantList := append([]protocol.Session(nil), list...)
		list[0].SessionId = "mutated"
		list[0].SessionRevision.LocalRevision = "0"

		workspaces, err := r.listWorkspaces(context.Background())
		if err != nil {
			t.Fatalf("listWorkspaces: %v", err)
		}
		wantWorkspaces := append([]protocol.Workspace(nil), workspaces...)
		if len(workspaces) == 1 {
			workspaces[0].Root = "mutated"
		}

		reread := projectSession(headerThroughRuntime(t, r, session))
		if reread != wantHeader {
			t.Fatalf("re-read header = %+v, want the immutable pre-mutation %+v", reread, wantHeader)
		}
		relisted, err := r.listSessions(context.Background(), protocol.ListSessionsParams{Workspace: ws, Lifecycle: "open"})
		if err != nil {
			t.Fatalf("listSessions re-read: %v", err)
		}
		if !reflect.DeepEqual(relisted, wantList) {
			t.Fatalf("re-read list = %+v, want the immutable pre-mutation %+v", relisted, wantList)
		}
		rews, err := r.listWorkspaces(context.Background())
		if err != nil {
			t.Fatalf("listWorkspaces re-read: %v", err)
		}
		if !reflect.DeepEqual(rews, wantWorkspaces) {
			t.Fatalf("re-read workspaces = %+v, want the immutable pre-mutation %+v", rews, wantWorkspaces)
		}
	})
}

// TestProjectionConcurrentReadDelete proves a read racing one deletion
// converges to a complete-body outcome: the full old header or the typed
// not-found, never a partial projection.
func TestProjectionConcurrentReadDelete(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		blocked := newTargetedBlockStore(store)
		r, e := openProjectionRuntime(t, blocked)
		defer closeProjectionRuntime(r)
		ws := filepath.Join(e.home, "race")
		session := projectionSession(t, r, ws, "solo").Identity.SessionID
		err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, session)
			return err
		})
		if err != nil {
			t.Fatalf("ArchiveSession: %v", err)
		}

		// The complete old outcome: a read that finishes before the deletion
		// commit returns the full header.
		before := projectSession(headerThroughRuntime(t, r, session))
		if before.SessionId != session || before.Lifecycle != protocol.Archived {
			t.Fatalf("pre-delete header = %+v, want the complete archived header", before)
		}

		// The complete new outcome: a read parked behind the deletion
		// transaction converges to the typed not-found with no header.
		blocked.block(session)
		// The shared wrapper's releaseBlock is not idempotent: one OnceFunc
		// release serves both the success path and failure cleanup, deferred
		// after the owner close so a failing test releases before the close
		// joins the parked calls.
		release := sync.OnceFunc(blocked.releaseBlock)
		defer release()
		deleteDone := make(chan error, 1)
		go func() {
			deleteDone <- r.deleteSession(context.Background(), session)
		}()
		select {
		case <-blocked.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the deletion never reached its transaction")
		}
		readDone := make(chan error, 1)
		go func() {
			readDone <- headerErrorThroughRuntime(context.Background(), r, session)
		}()
		release()
		if err := <-deleteDone; err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		select {
		case err := <-readDone:
			if !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("read racing the delete = %v, want the typed not-found", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the racing read never converged")
		}
	})
}

// TestProjectionQueuedLocalRevisionWithoutDurableAdvance proves a queued
// buffer publication advances only the coordinator-local counter.
func TestProjectionQueuedLocalRevisionWithoutDurableAdvance(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		session := projectionSession(t, r, filepath.Join(e.home, "queued"), "solo").Identity.SessionID

		gate := make(chan struct{})
		e.prep.modelGate = gate
		submitThroughRuntime(t, r, session, "op-1", "running")
		awaitModelArrival(t, e)
		before := projectSession(headerThroughRuntime(t, r, session))

		submitQueuedThroughRuntime(t, r, session, "op-2", "queued")

		after := projectSession(headerThroughRuntime(t, r, session))
		if after.SessionRevision.DurableRevision != before.SessionRevision.DurableRevision {
			t.Fatalf("durable revision = %s, want the unchanged %s", after.SessionRevision.DurableRevision, before.SessionRevision.DurableRevision)
		}
		beforeLocal, err := strconv.ParseUint(before.SessionRevision.LocalRevision, 10, 64)
		if err != nil {
			t.Fatalf("parse before local revision: %v", err)
		}
		afterLocal, err := strconv.ParseUint(after.SessionRevision.LocalRevision, 10, 64)
		if err != nil {
			t.Fatalf("parse after local revision: %v", err)
		}
		if afterLocal <= beforeLocal {
			t.Fatalf("local revision %d did not advance past %d after the queued publication", afterLocal, beforeLocal)
		}

		close(gate)
		e.prep.awaitCleanups(1)
	})
}

// TestProjectionClosedAndCanceledCallers proves every facade read holds the
// admission gate: closed owners reject with ErrClosed and canceled callers
// with their own context error.
func TestProjectionClosedAndCanceledCallers(t *testing.T) {
	r, _ := openProjectionRuntime(t, storage.NewMemory())
	defer closeProjectionRuntime(r)
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed := []func() error{
		func() error {
			return headerErrorThroughRuntime(context.Background(), r, "0123456789abcdef0123456789abcdef")
		},
		func() error {
			_, err := r.listSessions(context.Background(), protocol.ListSessionsParams{Workspace: "/tmp/x", Lifecycle: "open"})
			return err
		},
		func() error { _, err := r.listWorkspaces(context.Background()); return err },
	}
	for i, call := range closed {
		if err := call(); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed caller %d = %v, want ErrClosed", i, err)
		}
	}

	r2, _ := openProjectionRuntime(t, storage.NewMemory())
	defer closeProjectionRuntime(r2)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceledCalls := []func() error{
		func() error {
			return headerErrorThroughRuntime(canceled, r2, "0123456789abcdef0123456789abcdef")
		},
		func() error {
			_, err := r2.listSessions(canceled, protocol.ListSessionsParams{Workspace: "/tmp/x", Lifecycle: "open"})
			return err
		},
		func() error { _, err := r2.listWorkspaces(canceled); return err },
	}
	for i, call := range canceledCalls {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller %d = %v, want context.Canceled", i, err)
		}
	}
}

// TestProjectionHeaderCarriesNoInternalRepresentation proves the projected
// headers' exact field sets: no storage register, system prompt, execution
// capture, or key material leaves through a Session or Operation header.
func TestProjectionHeaderCarriesNoInternalRepresentation(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ws := filepath.Join(e.home, "wire")
		session := projectionSession(t, r, ws, "solo").Identity.SessionID
		submitThroughRuntime(t, r, session, "op-1", "work")
		e.prep.awaitCleanups(1)

		header := projectSession(headerThroughRuntime(t, r, session))
		assertExactJSONKeys(t, header, []string{
			"agent_type", "created_at", "last_activity", "lifecycle", "session_id",
			"session_revision", "session_revision.durable_revision", "session_revision.instance_id",
			"session_revision.local_revision", "workspace",
		})
		snap := snapshotThroughRuntime(t, r, session)
		operationJSON := assertExactJSONKeys(t, projectOperation(snap.Operations[0]), []string{
			"admitted_at", "agent_type", "model", "operation_id", "request_kind",
			"settled_at", "status", "usage", "usage.by_model",
		})
		if !strings.Contains(operationJSON, `"by_model":[]`) {
			t.Fatalf("empty usage = %s, want the non-nil [] array", operationJSON)
		}
		// The canonical register totals project as decimal strings.
		usage := projectUsage(harness.UsageTotals{ByModel: []harness.ModelUsage{{
			Model: model.ModelRef{Provider: "prov", Model: "m"},
			Usage: harness.UsageCount{InputTokens: 11, CachedInputTokens: 3, OutputTokens: 5},
		}}})
		wantUsage := protocol.UsageTotals{ByModel: []protocol.ModelUsage{{
			Model: "prov/m",
			Usage: protocol.UsageCount{InputTokens: "11", CachedInputTokens: "3", OutputTokens: "5"},
		}}}
		if !reflect.DeepEqual(usage, wantUsage) {
			t.Fatalf("usage projection = %+v, want %+v", usage, wantUsage)
		}
	})
}

// assertExactJSONKeys marshals v and checks its flattened object key set
// against the expected members, then returns the marshaled body. Decoding
// keeps exact numbers, so opaque values like 1e1000 survive the inspection.
func assertExactJSONKeys(t *testing.T, v any, want []string) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var top map[string]any
	if err := decoder.Decode(&top); err != nil {
		t.Fatalf("unmarshal %T: %v", v, err)
	}
	var got []string
	for key, value := range top {
		got = append(got, key)
		if nested, ok := value.(map[string]any); ok {
			for nestedKey := range nested {
				got = append(got, key+"."+nestedKey)
			}
		}
	}
	sort.Strings(got)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(got, wantSorted) {
		t.Fatalf("wire keys = %v, want exactly %v", got, wantSorted)
	}
	return string(data)
}

// oneShotEntriesStore wraps one store: the first ReadEntries after arming
// parks before delegating to the backend, signals its arrival, and never
// parks again — the owner's post-deletion generation bump may revalidate the
// survivor, so a second read must pass straight through.
type oneShotEntriesStore struct {
	harness.Storage
	mu      sync.Mutex
	release chan struct{} // armed gate: closed once by releaseGate
	spent   bool          // the one park is taken
	closed  bool          // the gate was released
	arrived chan struct{}
}

func newOneShotEntriesStore(base harness.Storage) *oneShotEntriesStore {
	return &oneShotEntriesStore{Storage: base, arrived: make(chan struct{}, 1)}
}

func (s *oneShotEntriesStore) arm() {
	s.mu.Lock()
	s.release = make(chan struct{})
	s.spent = false
	s.closed = false
	s.mu.Unlock()
}

func (s *oneShotEntriesStore) releaseGate() {
	s.mu.Lock()
	release := s.release
	closed := s.closed
	s.closed = true
	s.mu.Unlock()
	if release != nil && !closed {
		close(release)
	}
}

func (s *oneShotEntriesStore) ReadEntries(ctx context.Context, sessionID string, after int64) ([]harness.Entry, error) {
	s.mu.Lock()
	release, park := s.release, !s.spent
	s.spent = true
	s.mu.Unlock()
	if release != nil && park {
		select {
		case s.arrived <- struct{}{}:
		default:
		}
		<-release
	}
	return s.Storage.ReadEntries(ctx, sessionID, after)
}

// TestProjectionListCandidateDeletionDuringScan proves the fresh-scan skip
// against a real interleaving: a candidate deleted while the scan's snapshot
// read is parked leaves exactly the complete survivor listed, and the direct
// victim read is the typed not-found — never a propagated not-found partial
// list.
func TestProjectionListCandidateDeletionDuringScan(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		gated := newOneShotEntriesStore(store)
		r, e := openProjectionRuntime(t, gated)
		defer closeProjectionRuntime(r)
		ws := filepath.Join(e.home, "scan")
		// Two archived Sessions seeded after Open: the lower sorted identity
		// is the cold survivor whose parked materialization lets the deletion
		// land first; the higher sorted identity is the victim copy that a
		// missed gone check would still list.
		first := seedStaleArchivedSession(t, store, ws, 0)
		second := seedStaleArchivedSession(t, store, ws, 0)
		survivor, victim := first, second
		if survivor > victim {
			survivor, victim = victim, survivor
		}
		// Warm the victim before arming: the scan reaches it only after the
		// parked survivor, so a missed gone check would return its cached
		// header instead of omitting it.
		if err := headerErrorThroughRuntime(context.Background(), r, victim); err != nil {
			t.Fatalf("warm ReadSessionHeader: %v", err)
		}

		gated.arm()
		defer gated.releaseGate() // failure cleanup: runs before the owner close below
		listDone := make(chan error, 1)
		var list []protocol.Session
		go func() {
			var err error
			list, err = r.listSessions(context.Background(), protocol.ListSessionsParams{Workspace: ws, Lifecycle: "archived"})
			listDone <- err
		}()
		select {
		case <-gated.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the scan never reached the cold survivor's entry read")
		}
		// While the survivor's read is parked, the already-copied victim is
		// deleted: the scan must observe it as absent, not fail.
		if err := r.deleteSession(context.Background(), victim); err != nil {
			t.Fatalf("delete during scan: %v", err)
		}
		gated.releaseGate()
		select {
		case err := <-listDone:
			if err != nil {
				t.Fatalf("listSessions: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the scan never converged")
		}
		if len(list) != 1 || list[0].SessionId != survivor || list[0].Lifecycle != protocol.Archived || list[0].ArchivedAt == nil {
			t.Fatalf("list = %+v, want exactly the complete survivor %s header", list, survivor)
		}
		if err := headerErrorThroughRuntime(context.Background(), r, victim); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("victim read = %v, want harness.ErrNotFound", err)
		}
	})
}
