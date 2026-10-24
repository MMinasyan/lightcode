package harness

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// prepareCall records one preparation invocation: the received context and
// request exactly as the Harness passed them.
type prepareCall struct {
	ctx context.Context
	req PreparationRequest
}

// prepareStub is the controlled preparation callback for the admission
// fixtures: it records every call, signals each arrival, optionally parks
// every call on a gate, and returns the configured result.
type prepareStub struct {
	mu        sync.Mutex
	calls     []prepareCall
	arrived   chan struct{}
	gate      chan struct{}
	ignoreCtx bool // park on the gate alone, ignoring context cancellation
	result    PreparedExecution
	err       error
}

func newPrepareStub(result PreparedExecution) *prepareStub {
	return &prepareStub{result: result, arrived: make(chan struct{}, 16)}
}

func (p *prepareStub) prepare(ctx context.Context, req PreparationRequest) (PreparedExecution, error) {
	p.mu.Lock()
	p.calls = append(p.calls, prepareCall{ctx: ctx, req: req})
	p.mu.Unlock()
	p.arrived <- struct{}{}
	if p.gate != nil {
		if p.ignoreCtx {
			<-p.gate
		} else {
			select {
			case <-p.gate:
			case <-ctx.Done():
				return PreparedExecution{}, ctx.Err()
			}
		}
	}
	return p.result, p.err
}

func (p *prepareStub) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *prepareStub) lastCall() prepareCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[len(p.calls)-1]
}

// emptyStore returns a controlled store with no durable sessions.
func emptyStore(t *testing.T) *graphStorage {
	t.Helper()
	return &graphStorage{registers: map[string][]Register{}, entries: map[string][]Entry{}}
}

// freshSessionStore returns a controlled store holding exactly one valid open
// root Session with no operations or entries.
func freshSessionStore(t *testing.T) *graphStorage {
	t.Helper()
	return (&testGraph{session: validSessionRecord()}).storage(t)
}

func newTestHarness(t *testing.T, store Storage, prepare func(context.Context, PreparationRequest) (PreparedExecution, error)) *Harness {
	t.Helper()
	if prepare == nil {
		prepare = func(context.Context, PreparationRequest) (PreparedExecution, error) {
			return PreparedExecution{}, errors.New("no preparation configured for this test")
		}
	}
	h, err := New(context.Background(), Dependencies{Storage: store, Prepare: prepare})
	testassert.NoError(t, err)
	return h
}

// validExecution returns the fixtures' execution: a model function that never
// returns, so an auto-started execution keeps its Operation current without
// settling it, a tool function returning an empty plan, and the required pure
// normalizer. Admission fixtures observe a stable running state.
func validExecution() Execution {
	return Execution{
		Model: func(context.Context, model.Request) (model.Stream, error) {
			select {} // the fixture owns the admitted Operation's state assertions
		},
		CompactModel: func(context.Context, model.Request) (model.Stream, error) {
			select {}
		},
		Tool:          func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
		NormalizeTool: objectNormalize,
	}
}

// preparedExecuting returns a prepared execution with a valid capture and an
// opener that always succeeds with the given execution.
func preparedExecuting(exec Execution) PreparedExecution {
	return PreparedExecution{
		Capture: testCapture(),
		Open:    func(context.Context, OperationAdmission) (Execution, error) { return exec, nil },
	}
}

// modelPrepared returns the fixtures' prepared execution whose opener yields
// the given physical model request function over the default tool function.
func modelPrepared(modelFn func(context.Context, model.Request) (model.Stream, error)) PreparedExecution {
	exec := validExecution()
	if modelFn != nil {
		exec.Model = modelFn
	}
	return preparedExecuting(exec)
}

// validPrepared returns a prepared execution with a valid capture and an
// opener yielding effect functions that never return, so an auto-started
// execution keeps its Operation current without settling it.
func validPrepared() PreparedExecution { return preparedExecuting(validExecution()) }

// mustAdmitWithoutExecution runs the reserved admission body without
// installing the execution — the shape the effect fixtures need to drive the
// private wrappers directly on the admitted Operation.
func mustAdmitWithoutExecution(t *testing.T, h *Harness, sessionID, operationID string, content []model.ContentPart) (OperationRecord, SubmitDisposition) {
	t.Helper()
	c := leaseCoordinator(t, h, sessionID)
	release, err := h.reserve(context.Background(), c)
	testassert.NoError(t, err)
	defer release()
	rec, prepared, disposition, err := h.admitReserved(context.Background(), c, admissionRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Kind:        RequestKindMessage,
		Origin:      InputOriginUser,
		Content:     content,
	})
	testassert.NoError(t, err)
	testassert.Require(t, disposition == DispositionAdmitted && prepared != nil, "disposition %q (prepared %v), want a new admission", disposition, prepared != nil)
	return rec, disposition
}

func admissionContent(text string) []model.ContentPart {
	return []model.ContentPart{{Kind: model.PartText, Text: text}}
}

// mustAdmit submits one regular message through public Submit and returns
// the public result's operation record and disposition.
func mustAdmit(t *testing.T, h *Harness, sessionID, operationID string, content []model.ContentPart) (OperationRecord, SubmitDisposition) {
	t.Helper()
	res, err := h.Submit(context.Background(), SubmitRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Origin:      InputOriginUser,
		Content:     content,
		Mode:        MessageModeRegular,
	})
	testassert.NoError(t, err)
	testassert.Require(t, res.Operation != nil, "Submit disposition %q carried no operation record", res.Disposition)
	return *res.Operation, res.Disposition
}

// storedSessionState reads back the store's session fixtures for one Session.
// It returns stable snapshots: an auto-started execution mutates register
// values in place, so callers never alias the store's backing arrays.
// registerLookup returns one register envelope by key from a stored state
// copy.
func registerLookup(regs []Register, key RegisterKey) (Register, bool) {
	for _, reg := range regs {
		if reg.Key == key {
			return reg, true
		}
	}
	return Register{}, false
}

func storedSessionState(s *graphStorage, sessionID string) (entries []Entry, regs []Register) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries = append([]Entry(nil), s.entries[sessionID]...)
	regs = make([]Register, len(s.registers[sessionID]))
	copy(regs, s.registers[sessionID])
	return entries, regs
}

// TestNewConstructionContract proves the constructor's input contract: a live
// context and non-nil dependencies are required, and valid construction
// performs no storage work.
func TestNewConstructionContract(t *testing.T) {
	store := emptyStore(t)
	{
		_, err := New(context.Background(), Dependencies{})
		testassert.Require(t, errors.Is(err, ErrInvalid), "New without dependencies = %v, want ErrInvalid", err)
	}
	{
		_, err := New(context.Background(), Dependencies{Storage: store})
		testassert.Require(t, errors.Is(err, ErrInvalid), "New without preparation = %v, want ErrInvalid", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	deps := Dependencies{Storage: store, Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) { return PreparedExecution{}, nil }}
	{
		_, err := New(canceled, deps)
		testassert.Require(t, errors.Is(err, ErrInvalid), "New with a dead context = %v, want ErrInvalid", err)
	}
	{
		_, err := New(context.Background(), deps)
		testassert.NoError(t, err)
	}
}

// TestCreateSessionWorkspaceIdentity proves the Workspace matrix row: the
// caller-normalized absolute value is stored unchanged, relative and unclean
// siblings are rejected without harness normalization, and created identities
// are 32 lowercase hex characters with one sampled creation time.
func TestCreateSessionWorkspaceIdentity(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, nil)

	rec, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	testassert.Require(t, rec.Identity.Workspace == "/tmp/works" && regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(rec.Identity.SessionID) && (rec.State.Lifecycle == LifecycleOpen && rec.State.CurrentAgentType == "coder" && rec.Revision == 1) && (!rec.Identity.CreatedAt.IsZero() && rec.State.LastActivity.Equal(rec.Identity.CreatedAt)) && len(rec.State.Usage.ByModel) == 0, "got %v, want %v; session id %q is not 32 lowercase hex characters; created session state = %+v; creation did not sample one time for created_at and last_activity: %+v; created session carries usage: %+v", rec.Identity.Workspace, "/tmp/works", rec.Identity.SessionID, rec.State, rec, rec.State.Usage)
	read, err := h.ReadSessionHeader(context.Background(), rec.Identity.SessionID)
	testassert.NoError(t, err)
	testassert.Require(t, read.Identity.Workspace == "/tmp/works" && read.Revision.DurableRevision == 1, "read session = %+v", read)

	for _, workspace := range []string{"relative/path", "/tmp/works/../works", ""} {
		{
			_, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: workspace, AgentType: "coder"})
			testassert.Require(t, errors.Is(err, ErrInvalid), "CreateSession(%q) = %v, want ErrInvalid", workspace, err)
		}
	}
	{
		_, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works"})
		testassert.Require(t, errors.Is(err, ErrInvalid), "empty agent type = %v, want ErrInvalid", err)
	}
}

// TestReadsMaterializeValidatedState proves Session and Operation reads:
// absent records use the landed not-found class, malformed identities are
// invalid input, and a created session reads back through its materialized
// coordinator.
func TestReadsMaterializeValidatedState(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, nil)
	{
		_, err := h.ReadSessionHeader(context.Background(), testSessionID)
		testassert.Require(t, errors.Is(err, ErrNotFound), "absent session = %v, want ErrNotFound", err)
	}
	{
		_, err := h.ReadHistoryView(context.Background(), testSessionID)
		testassert.Require(t, errors.Is(err, ErrNotFound), "absent history = %v, want ErrNotFound", err)
	}
	{
		_, err := h.ReadSessionHeader(context.Background(), "nope")
		testassert.Require(t, errors.Is(err, ErrInvalid), "malformed session id = %v, want ErrInvalid", err)
	}
	{
		_, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		testassert.Require(t, errors.Is(err, ErrNotFound), "absent operation = %v, want ErrNotFound", err)
	}
	created, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	read, err := h.ReadSessionHeader(context.Background(), created.Identity.SessionID)
	testassert.Require(t, err == nil && read.Revision.DurableRevision == 1 && read.Identity.SessionID == created.Identity.SessionID, "ReadSession = %+v, err %v", read, err)
	{
		_, err := h.ReadOperation(context.Background(), created.Identity.SessionID, "op-1")
		testassert.Require(t, errors.Is(err, ErrNotFound), "absent operation after creation = %v, want ErrNotFound", err)
	}
}

// TestCorruptSessionIsUnavailable proves a persisted semantic violation makes
// its Session unavailable in this Harness instance while a valid sibling
// stays usable.
func TestCorruptSessionIsUnavailable(t *testing.T) {
	fixture := validTestGraph()
	raw, err := encodeInputEntry(*fixture.entries[0].input)
	testassert.NoError(t, err)
	fixture.entries[0].rawOverride = setKey(raw, "bogus", json.RawMessage(`1`))
	store := fixture.storage(t)
	h := newTestHarness(t, store, nil)

	wantCorruption(t, func() error {
		_, err := h.ReadSessionHeader(context.Background(), testSessionID)
		return err
	}())
	wantCorruption(t, func() error {
		_, err := h.ReadHistoryView(context.Background(), testSessionID)
		return err
	}())
	wantCorruption(t, func() error {
		_, err := h.ReadSessionHeader(context.Background(), testSessionID)
		return err
	}())
	wantCorruption(t, func() error {
		_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: "op-9", Origin: InputOriginUser, Mode: MessageModeRegular})
		return err
	}())

	sibling, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	{
		_, err := h.ReadSessionHeader(context.Background(), sibling.Identity.SessionID)
		testassert.NoError(t, err)
	}
}

// TestChangeAgentType proves the retained Agent-type transition: open-only,
// identity/usage/activity preserving, and never touching an admitted
// Operation's admission section.
func TestChangeAgentType(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, nil)

	before, err := h.ReadSessionHeader(context.Background(), testSessionID)
	testassert.NoError(t, err)
	beforeUsage := readSessionView(t, h, testSessionID).TotalUsage
	after, err := h.ChangeAgentType(context.Background(), testSessionID, "reviewer")
	testassert.NoError(t, err)
	testassert.Require(t, after.State.CurrentAgentType == "reviewer" && after.Revision == before.Revision.DurableRevision+1 && (after.Identity == before.Identity && after.State.LastActivity.Equal(before.LastActivity) && usageTotalsEqual(after.State.Usage, beforeUsage)), "changed session = %+v; agent-type change altered identity, activity or usage: %+v", after.State, after)
	{
		_, err := h.ChangeAgentType(context.Background(), testSessionID, "")
		testassert.Require(t, errors.Is(err, ErrInvalid), "empty agent type = %v, want ErrInvalid", err)
	}

	archived := &testGraph{session: validSessionRecord()}
	stamped := testTime
	archived.session.State.Lifecycle = LifecycleArchived
	archived.session.State.ArchivedAt = &stamped
	archivedHarness := newTestHarness(t, archived.storage(t), nil)
	{
		_, err := archivedHarness.ChangeAgentType(context.Background(), testSessionID, "reviewer")
		testassert.Require(t, errors.Is(err, ErrInvalid), "agent type on an archived session = %v, want ErrInvalid", err)
	}
}

// TestAdmissionSessionPreconditions proves admission's session axis: the
// session must exist and be open.
func TestAdmissionSessionPreconditions(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
	{
		_, err := h.Submit(context.Background(), SubmitRequest{SessionID: otherSession(), OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
		testassert.Require(t, errors.Is(err, ErrNotFound), "absent session admission = %v, want ErrNotFound", err)
	}
	archived := &testGraph{session: validSessionRecord()}
	stamped := testTime
	archived.session.State.Lifecycle = LifecycleArchived
	archived.session.State.ArchivedAt = &stamped
	archivedHarness := newTestHarness(t, archived.storage(t), newPrepareStub(validPrepared()).prepare)
	{
		_, err := archivedHarness.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
		testassert.Require(t, errors.Is(err, ErrInvalid), "archived session admission = %v, want ErrInvalid", err)
	}
}

// TestAdmissionInputValidation proves the input axis of admission: a
// non-empty opaque operation identity and a closed input origin.
func TestAdmissionInputValidation(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
	for _, tc := range []struct {
		name        string
		operationID string
		origin      InputOrigin
		content     []model.ContentPart
	}{
		{"empty operation id", "", InputOriginUser, admissionContent("x")},
		{"unknown origin", testOpID, InputOrigin("agent"), admissionContent("x")},
		{"invalid content part", testOpID, InputOriginUser, []model.ContentPart{{Kind: model.PartKind("bogus")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: tc.operationID, Origin: tc.origin, Content: tc.content, Mode: MessageModeRegular})
			testassert.Require(t, errors.Is(err, ErrInvalid), "invalid input = %v, want ErrInvalid", err)
		})
	}
}

// TestAdmissionPublishesRunningOperation proves the admission row's positive
// contract: one transaction publishes the input entry, the running Operation
// register, and the Session current-Operation state, and the updated graph
// still validates.
func TestAdmissionPublishesRunningOperation(t *testing.T) {
	store := freshSessionStore(t)
	stub := newPrepareStub(validPrepared())
	h := newTestHarness(t, store, stub.prepare)

	rec, disposition := mustAdmit(t, h, testSessionID, testOpID, admissionContent("hello"))
	testassert.Equal(t, disposition, DispositionAdmitted)
	testassert.Require(t, rec.State.Status == OperationRunning && rec.Admission.AgentType == "coder" && rec.Admission.RequestKind == RequestKindMessage && rec.Admission.SessionID == testSessionID && rec.Admission.OperationID == testOpID && (rec.Admission.Execution.ConfigurationRevision == testCapture().ConfigurationRevision && rec.Admission.Execution.Model == testModelRef() && len(rec.Admission.Execution.Tools) == 1) && (rec.Admission.AdmittedEntry.SessionID == testSessionID && rec.Admission.AdmittedEntry.EntryID != "") && (!rec.State.StartedAt.IsZero() && rec.Admission.AdmittedAt.Equal(rec.State.StartedAt)) && (len(rec.State.PendingToolCalls) == 0 && len(rec.State.Usage.ByModel) == 0), "admitted operation = %+v; durable capture = %+v; admitted entry reference = %+v; operation timestamps = %+v; fresh operation state = %+v", rec, rec.Admission.Execution, rec.Admission.AdmittedEntry, rec.State, rec.State)

	entries, regs := storedSessionState(store, testSessionID)
	testassert.Require(t, len(entries) == 1 && entries[0].Kind == EntryInput && entries[0].OperationID == testOpID && entries[0].Sequence == 1, "published entries = %+v", entries)
	{
		_, err := decodeInputEntry(entries[0])
		testassert.NoError(t, err)
	}
	var sessionReg, opReg *Register
	for i := range regs {
		switch regs[i].Key.Kind {
		case RegisterSession:
			sessionReg = &regs[i]
		case RegisterOperation:
			opReg = &regs[i]
		}
	}
	testassert.Require(t, sessionReg != nil && opReg != nil, "published registers = %+v", regs)
	testassert.Require(t, opReg.Key.OperationID == testOpID, "operation register key = %+v", opReg.Key)
	session, err := decodeSessionRegister(*sessionReg)
	testassert.NoError(t, err)
	testassert.Require(t, session.State.CurrentOperationID == testOpID && session.Revision == 2, "session register = revision %d, current op %q", session.Revision, session.State.CurrentOperationID)
	testassert.Require(t, session.State.LastActivity.Equal(entries[0].CommittedAt), "last_activity %v did not advance to the input commit time %v", session.State.LastActivity, entries[0].CommittedAt)
	{
		_, err := validateSessionGraph(context.Background(), store, testSessionID)
		testassert.NoError(t, err)
	}
}

// TestAdmissionPreparationContract proves the preparation matrix row: exactly
// one preparation call with the revision-free owned Session view, invalid
// prepared executions publish nothing and use ErrInvalid, preparation errors
// are preserved, caller cancellation reaches the callback, and the Harness
// owns the returned capture.
func TestAdmissionPreparationContract(t *testing.T) {
	t.Run("one call with the owned session view", func(t *testing.T) {
		store := freshSessionStore(t)
		stub := newPrepareStub(validPrepared())
		h := newTestHarness(t, store, stub.prepare)
		{
			_, disposition := mustAdmit(t, h, testSessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		{
			got := stub.callCount()
			testassert.Equal(t, got, 1)
		}
		req := stub.lastCall().req
		testassert.Require(t, req.RequestKind == RequestKindMessage && (req.Session.Identity.SessionID == testSessionID && req.Session.Identity.Workspace == "/tmp/works" && !req.Session.Identity.CreatedAt.IsZero() && req.Session.AgentType == "coder"), "got %v, want %v; preparation session view = %+v", req.RequestKind, RequestKindMessage, req.Session)
	})

	t.Run("invalid prepared execution publishes nothing", func(t *testing.T) {
		for name, mutate := range map[string]func(*PreparedExecution){
			"nil opener":        func(p *PreparedExecution) { p.Open = nil },
			"partial model ref": func(p *PreparedExecution) { p.Capture.Model = model.ModelRef{Provider: "prov"} },
			"empty capture":     func(p *PreparedExecution) { p.Capture.ConfigurationRevision = "" },
			"duplicate tool name": func(p *PreparedExecution) {
				p.Capture.Tools = []model.ToolDefinition{testToolDefinition(), testToolDefinition()}
			},
		} {
			t.Run(name, func(t *testing.T) {
				prepared := validPrepared()
				mutate(&prepared)
				store := freshSessionStore(t)
				h := newTestHarness(t, store, newPrepareStub(prepared).prepare)
				_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("x"), Mode: MessageModeRegular})
				testassert.Require(t, errors.Is(err, ErrInvalid), "invalid prepared execution = %v, want ErrInvalid", err)
				entries, regs := storedSessionState(store, testSessionID)
				testassert.Require(t, len(entries) == 0 && len(regs) == 1 && regs[0].Key.Kind == RegisterSession, "invalid preparation published entries %v registers %v", entries, regs)
			})
		}
	})

	t.Run("preparation error is preserved", func(t *testing.T) {
		prepErr := errors.New("preparation failed")
		stub := newPrepareStub(PreparedExecution{})
		stub.err = prepErr
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)
		_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
		testassert.Equal(t, err, prepErr)
		entries, regs := storedSessionState(store, testSessionID)
		testassert.Require(t, len(entries) == 0 && len(regs) == 1, "failed preparation published entries %v registers %v", entries, regs)
	})

	t.Run("caller cancellation stops preparation", func(t *testing.T) {
		gate := make(chan struct{})
		stub := newPrepareStub(validPrepared())
		stub.gate = gate
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := h.Submit(ctx, SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
			done <- err
		}()
		<-stub.arrived // the callback is parked
		cancel()
		err := <-done
		testassert.Require(t, err != nil && errors.Is(err, context.Canceled), "canceled admission = %v, want a context error", err)
		testassert.Require(t, stub.lastCall().ctx.Err() != nil, "preparation context was not canceled with the caller")
		entries, regs := storedSessionState(store, testSessionID)
		testassert.Require(t, len(entries) == 0 && len(regs) == 1, "canceled preparation published entries %v registers %v", entries, regs)
		close(gate)
	})

	t.Run("preparation owns the returned capture", func(t *testing.T) {
		prepared := validPrepared()
		tools := []model.ToolDefinition{testToolDefinition()}
		prepared.Capture.Tools = tools
		store := freshSessionStore(t)
		h := newTestHarness(t, store, newPrepareStub(prepared).prepare)
		mustAdmit(t, h, testSessionID, testOpID, admissionContent("x"))
		tools[0].Parameters = json.RawMessage(`{"type":"string"}`)
		rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, string(rec.Admission.Execution.Tools[0].Parameters) == `{"type":"object"}`, "durable capture shares tool bytes with the caller: %q", rec.Admission.Execution.Tools[0].Parameters)
	})
}

// TestAdmissionReservationAndRaces proves the preparation row's race axis:
// the reservation blocks same-session preparation without holding a state
// lock across the callback, a foreign revision change is a conflict that
// publishes nothing, and an agent-type change during preparation affects
// later admission only.
func TestAdmissionReservationAndRaces(t *testing.T) {
	t.Run("reservation blocks the same session without a lock across the callback", func(t *testing.T) {
		gate := make(chan struct{})
		stub := newPrepareStub(validPrepared())
		stub.gate = gate
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)

		first := make(chan struct{}, 1)
		go func() {
			{
				_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("first"), Mode: MessageModeRegular})
				testassert.Check(t, err == nil, "first admission: %v", err)
			}
			first <- struct{}{}
		}()
		<-stub.arrived // the first admission is parked in preparation

		readDone := make(chan struct{}, 1)
		go func() {
			{
				_, err := h.ReadSessionHeader(context.Background(), testSessionID)
				testassert.Check(t, err == nil, "ReadSession during preparation: %v", err)
			}
			readDone <- struct{}{}
		}()
		select {
		case <-readDone:
		case <-time.After(5 * time.Second):
			t.Fatalf("ReadSession blocked while preparation was in flight")
		}

		second := make(chan struct{}, 1)
		go func() {
			res, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("second"), Mode: MessageModeRegular})
			testassert.Check(t, err == nil, "second admission: %v", err)
			testassert.Check(t, res.Disposition == DispositionExisting && res.Operation != nil, "second admission disposition = %q, want existing", res.Disposition)
			testassert.Check(t, res.Operation == nil || res.Operation.Admission.AdmittedAt == firstAdmittedAt(t, h, testSessionID, testOpID), "second admission returned a different operation")
			second <- struct{}{}
		}()

		close(gate)
		<-first
		<-second
		{
			got := stub.callCount()
			testassert.Equal(t, got, 1)
		}
	})

	t.Run("foreign revision change during preparation is a conflict", func(t *testing.T) {
		gate := make(chan struct{})
		stub := newPrepareStub(validPrepared())
		stub.gate = gate
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)

		done := make(chan error, 1)
		go func() {
			_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
			done <- err
		}()
		<-stub.arrived

		{
			err := store.Transact(context.Background(), func(tx Transaction) error {
				return foreignAgentTypeChange(tx, testSessionID, "foreign")
			})
			testassert.NoError(t, err)
		}
		close(gate)
		err := <-done
		testassert.Require(t, errors.Is(err, ErrConflict), "raced admission = %v, want ErrConflict", err)
		entries, regs := storedSessionState(store, testSessionID)
		testassert.Require(t, len(entries) == 0 && len(regs) == 1 && regs[0].Revision == 2, "conflicted admission published entries %v registers %v", entries, regs)
	})

	t.Run("agent-type change during preparation affects later admission only", func(t *testing.T) {
		gate := make(chan struct{})
		stub := newPrepareStub(validPrepared())
		stub.gate = gate
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)

		admitted := make(chan struct{}, 1)
		go func() {
			res, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
			testassert.Check(t, err == nil, "admission: %v", err)
			testassert.Check(t, res.Disposition == DispositionAdmitted && res.Operation != nil && res.Operation.Admission.AgentType == "coder", "admitted operation = %+v disposition %q", res.Operation, res.Disposition)
			admitted <- struct{}{}
		}()
		<-stub.arrived

		changed := make(chan struct{}, 1)
		go func() {
			rec, err := h.ChangeAgentType(context.Background(), testSessionID, "reviewer")
			testassert.Check(t, err == nil, "ChangeAgentType: %v", err)
			testassert.Check(t, rec.State.CurrentAgentType == "reviewer", "changed session = %+v", rec.State)
			changed <- struct{}{}
		}()

		close(gate)
		<-admitted
		<-changed
		rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Equal(t, rec.Admission.AgentType, "coder")
		session, err := h.ReadSessionHeader(context.Background(), testSessionID)
		testassert.Require(t, err == nil && session.CurrentAgentType == "reviewer", "session agent type = %+v err %v", session, err)
	})
}

func firstAdmittedAt(t *testing.T, h *Harness, sessionID, operationID string) time.Time {
	t.Helper()
	rec, err := h.ReadOperation(context.Background(), sessionID, operationID)
	testassert.NoError(t, err)
	return rec.Admission.AdmittedAt
}

// foreignAgentTypeChange replaces one session register's agent type directly
// in storage, as a foreign writer would.
func foreignAgentTypeChange(tx Transaction, sessionID, agentType string) error {
	key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
	reg, err := tx.ReadRegister(key)
	if err != nil {
		return err
	}
	current, err := decodeSessionRegister(reg)
	if err != nil {
		return err
	}
	current.State.CurrentAgentType = agentType
	payload, err := encodeSessionRegister(SessionRecord{Identity: current.Identity, State: current.State})
	if err != nil {
		return err
	}
	_, err = tx.ReplaceRegister(key, reg.Revision, payload)
	return err
}

// TestAdmissionPartialFailurePublishesNothing proves the admission row's
// partial-admission sibling: a storage failure at any producer step leaves
// the previous committed state and the session usable.
func TestAdmissionPartialFailurePublishesNothing(t *testing.T) {
	for _, step := range []string{"insert_entry", "insert_register", "replace_register"} {
		t.Run(step, func(t *testing.T) {
			store := freshSessionStore(t)
			injected := errors.New("injected storage failure at " + step)
			store.txHook = func(hooked string) error {
				if hooked == step {
					return injected
				}
				return nil
			}
			h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
			_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("x"), Mode: MessageModeRegular})
			testassert.Equal(t, err, injected)
			entries, regs := storedSessionState(store, testSessionID)
			testassert.Require(t, len(entries) == 0 && len(regs) == 1 && regs[0].Revision == 1, "partial failure published entries %v registers %v", entries, regs)
			store.txHook = nil
			{
				_, disposition := mustAdmit(t, h, testSessionID, testOpID, admissionContent("x"))
				testassert.Equal(t, disposition, DispositionAdmitted)
			}
		})
	}
}

// TestAdmissionIdempotency proves the idempotency row's exercisable axis: an
// admitted identity returns the first Operation even with a changed payload,
// without comparing content, preparing again, or publishing a second entry; a
// same-Session identity that lost a concurrent race resolves to the first
// Operation; and a global identity conflict in another Session is invalid
// reuse.
func TestAdmissionIdempotency(t *testing.T) {
	t.Run("changed-payload retry returns the first operation", func(t *testing.T) {
		store := freshSessionStore(t)
		stub := newPrepareStub(validPrepared())
		h := newTestHarness(t, store, stub.prepare)
		first, firstDisp := mustAdmit(t, h, testSessionID, testOpID, admissionContent("first"))
		testassert.Equal(t, firstDisp, DispositionAdmitted)
		retry, retryDisp := mustAdmit(t, h, testSessionID, testOpID, admissionContent("second"))
		testassert.Equal(t, retryDisp, DispositionExisting)
		testassert.Require(t, retry.Admission.AdmittedEntry == first.Admission.AdmittedEntry && retry.Admission.AdmittedAt.Equal(first.Admission.AdmittedAt), "retry returned a different operation: %+v vs %+v", retry, first)
		entries, regs := storedSessionState(store, testSessionID)
		testassert.Require(t, len(entries) == 1 && len(regs) == 2, "retry published entries %v registers %v", entries, regs)
		decoded, err := decodeInputEntry(entries[0])
		testassert.Require(t, err == nil && decoded.Content[0].Text == "first", "retry replaced the first payload: %+v err %v", decoded, err)
		{
			got := stub.callCount()
			testassert.Equal(t, got, 1)
		}
	})

	// One local winner table over the shared parked-preparation driver: each
	// case keeps its own winner construction and result/entry/revision/
	// ownership assertions.
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, store *graphStorage, h *Harness, other string)
		check func(t *testing.T, store *graphStorage, h *Harness, other string, baseRevision int64, res SubmitResult, err error)
	}{
		{
			name: "admitted race after preparation returns the first operation",
			seed: func(t *testing.T, store *graphStorage, _ *Harness, _ string) {
				// A concurrent admission of the same identity wins: it
				// publishes the full input, operation, and session state
				// directly into storage.
				{
					err := store.Transact(context.Background(), func(tx Transaction) error {
						return insertForeignAdmission(t, tx, testSessionID, testOpID)
					})
					testassert.NoError(t, err)
				}
			},
			check: func(t *testing.T, store *graphStorage, h *Harness, _ string, _ int64, res SubmitResult, err error) {
				testassert.NoError(t, err)
				testassert.Equal(t, res.Disposition, DispositionExisting)
				entries, regs := storedSessionState(store, testSessionID)
				testassert.Require(t, len(entries) == 1 && len(regs) == 2, "raced admission published entries %v registers %v", entries, regs)
				rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
				testassert.NoError(t, err)
				testassert.Require(t, rec.Admission.Execution.ConfigurationRevision == testCapture().ConfigurationRevision, "raced admission returned the wrong operation: %+v", rec)
			},
		},
		{
			name: "same-session terminal compact winner rejects without alias",
			seed: func(t *testing.T, store *graphStorage, _ *Harness, _ string) {
				// The winner is a terminal compact Operation under the same
				// caller ID, with its typed settlement and no Session-register
				// change.
				settlementID := hexID(60)
				{
					err := store.Transact(context.Background(), func(tx Transaction) error {
						settlement, err := encodeOperationSettlementEntry(operationSettlementEntry{
							SessionID: testSessionID, EntryID: settlementID, OperationID: testOpID, Status: OperationSuccess,
						})
						if err != nil {
							return err
						}
						if _, err := tx.InsertEntry(EntryDraft{SessionID: testSessionID, ID: settlementID, OperationID: testOpID, Kind: EntryOperationSettlement, Payload: settlement}); err != nil {
							return err
						}
						winner := compactOperationRecord(testOpID)
						stamped := testTime
						winner.State.Status = OperationSuccess
						winner.State.SettledAt = &stamped
						winner.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: settlementID}}
						payload, err := encodeOperationRegister(winner)
						if err != nil {
							return err
						}
						_, err = tx.InsertRegister(RegisterDraft{Key: RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID}, Payload: payload})
						return err
					})
					testassert.NoError(t, err)
				}
			},
			check: func(t *testing.T, store *graphStorage, _ *Harness, _ string, _ int64, _ SubmitResult, err error) {
				testassert.Require(t, errors.Is(err, ErrInvalid), "raced admission = %v, want the wrong-kind rejection", err)
				entries, regs := storedSessionState(store, testSessionID)
				testassert.Require(t, len(entries) == 1 && entries[0].ID == hexID(60), "raced admission published %v, want no extra input beside the winner's settlement", entries)
				testassert.Equal(t, len(regs), 2)
				wire, ok := registerLookup(regs, RegisterKey{SessionID: testSessionID, Kind: RegisterOperation, OperationID: testOpID})
				testassert.Require(t, ok, "the compact winner's register vanished")
				rec, err := decodeOperationRegister(wire)
				testassert.Require(t, err == nil && rec.Admission.RequestKind == RequestKindCompact && rec.State.Terminal != nil && rec.State.Terminal.SettlementEntry.EntryID == hexID(60), "winner rewritten = %+v err %v, want the compact terminal untouched", rec, err)
			},
		},
		{
			name: "foreign-session winner rejects without alias",
			seed: func(t *testing.T, store *graphStorage, _ *Harness, other string) {
				// The winner lands in its own Session and changes only that
				// Session.
				{
					err := store.Transact(context.Background(), func(tx Transaction) error {
						return insertForeignAdmission(t, tx, other, testOpID)
					})
					testassert.NoError(t, err)
				}
			},
			check: func(t *testing.T, store *graphStorage, _ *Harness, other string, baseRevision int64, _ SubmitResult, err error) {
				testassert.Require(t, errors.Is(err, ErrInvalid), "raced admission = %v, want the foreign-identity rejection", err)
				entries, _ := storedSessionState(store, testSessionID)
				testassert.Require(t, len(entries) == 0, "raced admission published %v, want no extra input", entries)
				_, otherRegs := storedSessionState(store, other)
				otherWire, ok := registerLookup(otherRegs, RegisterKey{SessionID: other, Kind: RegisterOperation, OperationID: testOpID})
				testassert.Require(t, ok, "the foreign winner is not durable in its own Session")
				{
					otherRec, err := decodeOperationRegister(otherWire)
					testassert.Require(t, err == nil && otherRec.Admission.SessionID == other, "foreign winner = %+v err %v, want it owned by its own Session", otherRec, err)
				}
				_, regs := storedSessionState(store, testSessionID)
				wire, ok := registerLookup(regs, RegisterKey{SessionID: testSessionID, Kind: RegisterSession})
				testassert.Require(t, ok, "the session register vanished")
				current, err := decodeSessionRegister(wire)
				testassert.Require(t, err == nil && current.Revision == baseRevision, "this Session changed = %+v err %v (revision %d, was %d), want the foreign winner confined to its own Session", current, err, current.Revision, baseRevision)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := make(chan struct{})
			stub := newPrepareStub(validPrepared())
			stub.gate = gate
			store := freshSessionStore(t)
			h := newTestHarness(t, store, stub.prepare)
			created, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
			testassert.NoError(t, err)
			other := created.Identity.SessionID

			type outcome struct {
				res SubmitResult
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				res, err := h.Submit(context.Background(), SubmitRequest{
					SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser,
					Content: admissionContent("raced"), Mode: MessageModeRegular,
				})
				done <- outcome{res, err}
			}()
			<-stub.arrived // the admission is parked in preparation

			_, baseRegs := storedSessionState(store, testSessionID)
			baseWire, ok := registerLookup(baseRegs, RegisterKey{SessionID: testSessionID, Kind: RegisterSession})
			testassert.Require(t, ok, "the session register vanished")
			base, err := decodeSessionRegister(baseWire)
			testassert.NoError(t, err)

			tc.seed(t, store, h, other)
			close(gate)
			out := <-done
			{
				got := stub.callCount()
				testassert.Equal(t, got, 1)
			}
			tc.check(t, store, h, other, base.Revision, out.res, out.err)
		})
	}

	t.Run("global operation reuse in another session is invalid", func(t *testing.T) {
		store := freshSessionStore(t)
		h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
		mustAdmit(t, h, testSessionID, "shared-op", admissionContent("x"))
		created, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
		testassert.NoError(t, err)
		_, err = h.Submit(context.Background(), SubmitRequest{SessionID: created.Identity.SessionID, OperationID: "shared-op", Origin: InputOriginUser, Content: admissionContent("y"), Mode: MessageModeRegular})
		testassert.Require(t, errors.Is(err, ErrInvalid), "cross-session reuse = %v, want ErrInvalid", err)
		entries, regs := storedSessionState(store, created.Identity.SessionID)
		testassert.Require(t, len(entries) == 0 && len(regs) == 1, "rejected reuse published entries %v registers %v", entries, regs)
	})

}

// insertForeignAdmission publishes one complete admission of the given
// identity directly into storage, as a concurrent winner would.
func insertForeignAdmission(t *testing.T, tx Transaction, sessionID, operationID string) error {
	t.Helper()
	sessionKey := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
	reg, err := tx.ReadRegister(sessionKey)
	if err != nil {
		return err
	}
	current, err := decodeSessionRegister(reg)
	if err != nil {
		return err
	}
	input := inputEntry{
		SessionID:   sessionID,
		EntryID:     hexID(50),
		OperationID: operationID,
		Origin:      InputOriginUser,
		Content:     admissionContent("won"),
	}
	payload, err := encodeInputEntry(input)
	if err != nil {
		return err
	}
	inserted, err := tx.InsertEntry(EntryDraft{SessionID: sessionID, ID: input.EntryID, OperationID: operationID, Kind: EntryInput, Payload: payload})
	if err != nil {
		return err
	}
	record := OperationRecord{
		Admission: OperationAdmission{
			SessionID:     sessionID,
			OperationID:   operationID,
			RequestKind:   RequestKindMessage,
			AdmittedEntry: EntryRef{SessionID: sessionID, EntryID: input.EntryID},
			AgentType:     current.State.CurrentAgentType,
			Execution:     testCapture(),
			AdmittedAt:    inserted.CommittedAt,
		},
		State: OperationCurrentState{
			Status:           OperationRunning,
			StartedAt:        inserted.CommittedAt,
			PendingToolCalls: []PendingToolCall{},
			Usage:            UsageTotals{},
		},
	}
	opPayload, err := encodeOperationRegister(record)
	if err != nil {
		return err
	}
	if _, err := tx.InsertRegister(RegisterDraft{Key: RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID}, Payload: opPayload}); err != nil {
		return err
	}
	state := current.State
	state.CurrentOperationID = operationID
	state.LastActivity = inserted.CommittedAt
	sessionPayload, err := encodeSessionRegister(SessionRecord{Identity: current.Identity, State: state})
	if err != nil {
		return err
	}
	_, err = tx.ReplaceRegister(sessionKey, reg.Revision, sessionPayload)
	return err
}

// terminalTestGraph returns a valid graph whose single Operation settled
// successfully with a no-output settlement entry.
func terminalTestGraph() *testGraph {
	fixture := validTestGraph()
	session := &fixture.session
	session.State.CurrentOperationID = ""
	op := &fixture.ops[0]
	stamped := testTime
	op.State.Status = OperationSuccess
	op.State.SettledAt = &stamped
	op.State.Terminal = &OperationTerminal{SettlementEntry: EntryRef{SessionID: testSessionID, EntryID: hexID(3)}}
	ref := testModelRef()
	settlement := operationSettlementEntry{
		SessionID:   testSessionID,
		EntryID:     hexID(3),
		OperationID: testOpID,
		Status:      OperationSuccess,
		Model:       &ref,
		Usage:       testUsage(5),
	}
	fixture.entries = append(fixture.entries, testEntry{
		env:        Entry{SessionID: testSessionID, ID: hexID(3), OperationID: testOpID, Kind: EntryOperationSettlement, Sequence: 3, CommittedAt: testTime},
		settlement: &settlement,
	})
	totals := UsageTotals{ByModel: []ModelUsage{{Model: ref, Usage: *testUsage(5)}}}
	op.State.Usage = totals
	session.State.Usage = totals
	return fixture
}

// foreignArchiveChange replaces one session register's lifecycle with archived
// directly in storage, as a foreign writer would.
func foreignArchiveChange(tx Transaction, sessionID string) error {
	key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
	reg, err := tx.ReadRegister(key)
	if err != nil {
		return err
	}
	current, err := decodeSessionRegister(reg)
	if err != nil {
		return err
	}
	stamped := time.Now().UTC()
	current.State.Lifecycle = LifecycleArchived
	current.State.ArchivedAt = &stamped
	payload, err := encodeSessionRegister(SessionRecord{Identity: current.Identity, State: current.State})
	if err != nil {
		return err
	}
	_, err = tx.ReplaceRegister(key, reg.Revision, payload)
	return err
}

// TestAdmissionRechecksPreconditionsAfterReservation proves the open and idle
// preconditions are re-checked against the durable state at the admission
// commit: preparation may run against the stale view the reservation resolved
// on, and a foreign archive or a committed foreign Operation refuses the
// publication and refreshes the coordinator view.
func TestAdmissionRechecksPreconditionsAfterReservation(t *testing.T) {
	t.Run("foreign archive between materialization and admission resolves at commit", func(t *testing.T) {
		store := freshSessionStore(t)
		stub := newPrepareStub(validPrepared())
		h := newTestHarness(t, store, stub.prepare)
		leaseCoordinator(t, h, testSessionID) // the controlled warm view the foreign write races
		// a real history use under the held lease
		{
			_, err := h.ReadHistoryView(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		{
			err := store.Transact(context.Background(), func(tx Transaction) error {
				return foreignArchiveChange(tx, testSessionID)
			})
			testassert.NoError(t, err)
		}
		_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
		testassert.Require(t, errors.Is(err, ErrInvalid), "admission over a foreign archive = %v, want ErrInvalid", err)
		{
			got := stub.callCount()
			testassert.Equal(t, got, 1)
		}
		entries, regs := storedSessionState(store, testSessionID)
		testassert.Require(t, len(entries) == 0 && len(regs) == 1 && regs[0].Revision == 2, "refused admission left entries %v registers %v", entries, regs)
		session, err := h.ReadSessionHeader(context.Background(), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, session.Lifecycle == LifecycleArchived, "read session kept the stale view: %+v", session)
	})

	t.Run("foreign committed running operation refuses at commit", func(t *testing.T) {
		store := freshSessionStore(t)
		stub := newPrepareStub(validPrepared())
		h := newTestHarness(t, store, stub.prepare)
		leaseCoordinator(t, h, testSessionID) // the controlled warm view the foreign write races
		// a real history use under the held lease
		{
			_, err := h.ReadHistoryView(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		{
			err := store.Transact(context.Background(), func(tx Transaction) error {
				return insertForeignAdmission(t, tx, testSessionID, "foreign-op")
			})
			testassert.NoError(t, err)
		}
		_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
		testassert.Require(t, errors.Is(err, ErrInvalid), "admission over a foreign running operation = %v, want ErrInvalid", err)
		{
			got := stub.callCount()
			testassert.Equal(t, got, 1)
		}
		entries, regs := storedSessionState(store, testSessionID)
		testassert.Require(t, len(entries) == 1 && len(regs) == 2, "refused admission left entries %v registers %v", entries, regs)
		session, err := h.ReadSessionHeader(context.Background(), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, session.CurrentOperationID == "foreign-op", "read session kept the stale view: %+v", session)
	})
}

// TestForeignChangeRefreshesTheView proves a foreign revision change that
// fails an admission or agent-type transaction revalidates the coordinator
// view before the error returns, so later reads observe the foreign state.
func TestForeignChangeRefreshesTheView(t *testing.T) {
	t.Run("failed admission", func(t *testing.T) {
		gate := make(chan struct{})
		stub := newPrepareStub(validPrepared())
		stub.gate = gate
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)

		done := make(chan error, 1)
		go func() {
			_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
			done <- err
		}()
		<-stub.arrived
		{
			err := store.Transact(context.Background(), func(tx Transaction) error {
				return foreignAgentTypeChange(tx, testSessionID, "foreign")
			})
			testassert.NoError(t, err)
		}
		close(gate)
		{
			err := <-done
			testassert.Require(t, errors.Is(err, ErrConflict), "raced admission = %v, want ErrConflict", err)
		}
		session, err := h.ReadSessionHeader(context.Background(), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, session.CurrentAgentType == "foreign", "read session kept the stale view: %+v", session)
	})

	t.Run("failed agent-type change", func(t *testing.T) {
		store := freshSessionStore(t)
		h := newTestHarness(t, store, nil)
		leaseCoordinator(t, h, testSessionID) // the controlled warm view the foreign write races
		// a real history use under the held lease
		{
			_, err := h.ReadHistoryView(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		{
			err := store.Transact(context.Background(), func(tx Transaction) error {
				return foreignAgentTypeChange(tx, testSessionID, "foreign")
			})
			testassert.NoError(t, err)
		}
		{
			_, err := h.ChangeAgentType(context.Background(), testSessionID, "reviewer")
			testassert.Require(t, errors.Is(err, ErrConflict), "raced change = %v, want ErrConflict", err)
		}
		session, err := h.ReadSessionHeader(context.Background(), testSessionID)
		testassert.NoError(t, err)
		testassert.Require(t, session.CurrentAgentType == "foreign", "read session kept the stale view: %+v", session)
	})
}

// TestHarnessCancellationGatesPublication proves publication never starts
// once the Harness context is done: nothing is published, the Harness
// context error is returned while the caller context stays live, and the
// discarded prepared values are never opened.
func TestHarnessCancellationGatesPublication(t *testing.T) {
	harnessCtx, cancelHarness := context.WithCancel(context.Background())
	defer cancelHarness()
	gate := make(chan struct{})
	opens := 0
	prepared := PreparedExecution{
		Capture: testCapture(),
		Open: func(context.Context, OperationAdmission) (Execution, error) {
			opens++
			return validExecution(), nil
		},
	}
	stub := newPrepareStub(prepared)
	stub.gate = gate
	stub.ignoreCtx = true // preparation completes despite the canceled harness context
	store := freshSessionStore(t)
	h, err := New(harnessCtx, Dependencies{Storage: store, Prepare: stub.prepare})
	testassert.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
		done <- err
	}()
	<-stub.arrived
	cancelHarness()
	close(gate)
	err = <-done
	testassert.Require(t, errors.Is(err, context.Canceled), "admission over a canceled harness context = %v, want a context error", err)
	testassert.Equal(t, opens, 0)
	entries, regs := storedSessionState(store, testSessionID)
	testassert.Require(t, len(entries) == 0 && len(regs) == 1 && regs[0].Revision == 1, "canceled publication left entries %v registers %v", entries, regs)
}

// TestReadOperationReturnsOwnedRecords proves every pointer field of a
// returned Operation record is an independent copy of the coordinator state.
func TestReadOperationReturnsOwnedRecords(t *testing.T) {
	t.Run("active effect pointer", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.ops[0].State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(3), RequestID: hexID(99)}
		h := newTestHarness(t, fixture.storage(t), nil)
		rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		testassert.NoError(t, err)
		rec.State.ActiveEffect.Kind = EffectTool
		rec.State.ActiveEffect.ResultEntryID = hexID(9)
		again, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, again.State.ActiveEffect != nil && again.State.ActiveEffect.Kind == EffectModel && again.State.ActiveEffect.ResultEntryID == hexID(3), "returned record shares the active effect with the coordinator view: %+v", again.State.ActiveEffect)
	})

	t.Run("terminal pointers", func(t *testing.T) {
		h := newTestHarness(t, terminalTestGraph().storage(t), nil)
		rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		testassert.NoError(t, err)
		other := testTime.Add(time.Hour)
		*rec.State.SettledAt = other
		rec.State.Terminal.Detail = "tampered"
		rec.State.Terminal.SettlementEntry.EntryID = hexID(9)
		again, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, again.State.SettledAt != nil && again.State.SettledAt.Equal(testTime) && again.State.Terminal != nil && again.State.Terminal.Detail == "" && again.State.Terminal.SettlementEntry.EntryID == hexID(3), "returned record shares terminal pointers with the coordinator view: %+v", again.State)
	})
}

// TestAdmissionOwnsInputContent proves the committed view entry carries the
// validated owned content copies, not the caller's slice.
func TestAdmissionOwnsInputContent(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
	content := []model.ContentPart{{Kind: model.PartText, Text: "hello", Extra: model.Extra{"k": json.RawMessage(`"v"`)}}}
	{
		_, disposition := mustAdmit(t, h, testSessionID, testOpID, content)
		testassert.Equal(t, disposition, DispositionAdmitted)
	}
	content[0].Text = "tampered"
	content[0].Extra["k"] = json.RawMessage(`"tampered"`)
	h.mu.Lock()
	c := h.sessions[testSessionID]
	h.mu.Unlock()
	c.mu.Lock()
	entry := *c.graph.Entries[0].Input
	c.mu.Unlock()
	testassert.Require(t, entry.Content[0].Text == "hello" && string(entry.Content[0].Extra["k"]) == `"v"`, "coordinator view shares input content with the caller: %+v", entry.Content)
}

// TestReadOperationValidatesIdentity proves an empty operation identity is
// invalid input while a well-formed absent one stays not-found.
func TestReadOperationValidatesIdentity(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, nil)
	{
		_, err := h.ReadOperation(context.Background(), testSessionID, "")
		testassert.Require(t, errors.Is(err, ErrInvalid), "empty operation id = %v, want ErrInvalid", err)
	}
	{
		_, err := h.ReadOperation(context.Background(), testSessionID, "op-ghost")
		testassert.Require(t, errors.Is(err, ErrNotFound), "absent operation id = %v, want ErrNotFound", err)
	}
}

// TestAdmitExistingBeforeLifecycle proves the fast-path ordering: retrying an
// already-admitted Operation ID on an archived Session returns the existing
// Operation, never the archived precondition error — existing precedes
// lifecycle. The settle-free archive is applied to the validated view the
// fast path consults (a genuinely archived Session with an unsettled running
// Operation is graph-invalid, so it cannot be produced through storage).
func TestAdmitExistingBeforeLifecycle(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
	{
		_, disposition := mustAdmit(t, h, testSessionID, testOpID, admissionContent("first"))
		testassert.Equal(t, disposition, DispositionAdmitted)
	}
	h.mu.Lock()
	c := h.sessions[testSessionID]
	h.mu.Unlock()
	stamped := testTime
	c.mu.Lock()
	c.graph.Session.State.Lifecycle = LifecycleArchived
	c.graph.Session.State.ArchivedAt = &stamped
	c.mu.Unlock()

	res, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("retry"), Mode: MessageModeRegular})
	testassert.NoError(t, err)
	testassert.Require(t, res.Disposition == DispositionExisting && res.Operation != nil, "retry disposition = %q, want existing", res.Disposition)
}

// TestAdmitDeadContextSkipsPreparation proves a dead combined context never
// invokes Prepare: nothing publishes and the applicable context error returns.
func TestAdmitDeadContextSkipsPreparation(t *testing.T) {
	harnessCtx, cancelHarness := context.WithCancel(context.Background())
	stub := newPrepareStub(validPrepared())
	store := freshSessionStore(t)
	h, err := New(harnessCtx, Dependencies{Storage: store, Prepare: stub.prepare})
	testassert.NoError(t, err)
	cancelHarness() // the harness context dies after construction, before admission
	_, admitErr := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
	testassert.Require(t, errors.Is(admitErr, context.Canceled), "admission over a dead harness context = %v, want a context error", admitErr)
	{
		got := stub.callCount()
		testassert.Equal(t, got, 0)
	}
	entries, regs := storedSessionState(store, testSessionID)
	testassert.Require(t, len(entries) == 0 && len(regs) == 1, "dead-context admission left entries %v registers %v", entries, regs)
}

// foreignCorruptRevision bumps one Session register to a new valid revision and
// plants a stored entry whose kind has no durable payload: a plain register
// read still decodes, but a full graph revalidation rejects the Session.
func foreignCorruptRevision(tx Transaction, sessionID string) error {
	key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
	reg, err := tx.ReadRegister(key)
	if err != nil {
		return err
	}
	current, err := decodeSessionRegister(reg)
	if err != nil {
		return err
	}
	current.State.CurrentAgentType = "foreign"
	payload, err := encodeSessionRegister(SessionRecord{Identity: current.Identity, State: current.State})
	if err != nil {
		return err
	}
	if _, err := tx.ReplaceRegister(key, reg.Revision, payload); err != nil {
		return err
	}
	_, err = tx.InsertEntry(EntryDraft{SessionID: sessionID, ID: hexID(77), Kind: EntryHookResult, Payload: json.RawMessage(`{}`)})
	return err
}

// TestRefreshReturnsDiscoveredCorruption proves refresh-error precedence: when
// the stale-view rematerialization discovers corruption, its error — not the
// original conflict — is returned, at both call sites.
func TestRefreshReturnsDiscoveredCorruption(t *testing.T) {
	t.Run("admission", func(t *testing.T) {
		store := freshSessionStore(t)
		h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
		// materialize revision 1
		{
			_, err := h.ReadSessionHeader(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		{
			err := store.Transact(context.Background(), func(tx Transaction) error { return foreignCorruptRevision(tx, testSessionID) })
			testassert.NoError(t, err)
		}
		_, err := h.Submit(context.Background(), SubmitRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Mode: MessageModeRegular})
		var corrupt *CorruptionError
		testassert.Require(t, errors.As(err, &corrupt), "admission over a corrupt foreign revision = %v, want the discovered CorruptionError", err)
		{
			_, rerr := h.ReadSessionHeader(context.Background(), testSessionID)
			testassert.Require(t, errors.As(rerr, &corrupt), "subsequent read = %v, want the marked CorruptionError", rerr)
		}
	})

	t.Run("agent-type change", func(t *testing.T) {
		store := freshSessionStore(t)
		h := newTestHarness(t, store, nil)
		// materialize revision 1
		{
			_, err := h.ReadSessionHeader(context.Background(), testSessionID)
			testassert.NoError(t, err)
		}
		{
			err := store.Transact(context.Background(), func(tx Transaction) error { return foreignCorruptRevision(tx, testSessionID) })
			testassert.NoError(t, err)
		}
		_, err := h.ChangeAgentType(context.Background(), testSessionID, "reviewer")
		var corrupt *CorruptionError
		testassert.Require(t, errors.As(err, &corrupt), "agent-type change over a corrupt foreign revision = %v, want the discovered CorruptionError", err)
	})
}

// TestOwnCaptureDropsZeroLengthBacking proves a zero-length tool list never
// retains the caller's backing, even with spare capacity.
func TestOwnCaptureDropsZeroLengthBacking(t *testing.T) {
	prepared := validPrepared()
	tools := make([]model.ToolDefinition, 0, 1) // zero length with spare capacity
	prepared.Capture.Tools = tools
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(prepared).prepare)
	mustAdmit(t, h, testSessionID, testOpID, admissionContent("x"))
	_ = append(tools, testToolDefinition()) // the caller reuses its backing after admission
	rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.Admission.Execution.Tools == nil, "admitted capture retained the caller's zero-length backing: %+v", rec.Admission.Execution.Tools)
}

// TestOwnCaptureOwnsCapabilitySelection proves the admitted capture keeps the
// prepared capability selection in its own storage: a zero-length selection
// normalizes to nil and never keeps the caller's backing, and a caller
// mutation after admission reaches neither the durable capture nor a later
// read of the same prepared value.
func TestOwnCaptureOwnsCapabilitySelection(t *testing.T) {
	t.Run("zero-length backing", func(t *testing.T) {
		prepared := validPrepared()
		capabilities := make([]string, 0, 1) // zero length with spare capacity
		prepared.Capture.Capabilities = capabilities
		store := freshSessionStore(t)
		h := newTestHarness(t, store, newPrepareStub(prepared).prepare)
		mustAdmit(t, h, testSessionID, testOpID, admissionContent("x"))
		_ = append(capabilities, "ghost") // the caller reuses its backing after admission
		rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, rec.Admission.Execution.Capabilities == nil, "admitted capture retained the caller's zero-length capability backing: %+v", rec.Admission.Execution.Capabilities)
	})

	t.Run("caller mutation after admission", func(t *testing.T) {
		prepared := validPrepared()
		capabilities := []string{"cap-a"}
		prepared.Capture.Capabilities = capabilities
		h := newTestHarness(t, freshSessionStore(t), newPrepareStub(prepared).prepare)
		mustAdmit(t, h, testSessionID, testOpID, admissionContent("x"))
		capabilities[0] = "tampered"
		rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		testassert.NoError(t, err)
		testassert.Require(t, len(rec.Admission.Execution.Capabilities) == 1 && rec.Admission.Execution.Capabilities[0] == "cap-a", "admitted capture aliases the caller's capability selection: %+v", rec.Admission.Execution.Capabilities)
	})
}

// TestAdmittedOperationCarriesRegisterRevision proves the admitted Operation
// record carries the revision storage assigned to its register at insert, in
// both the returned value and the coordinator view a later read observes.
func TestAdmittedOperationCarriesRegisterRevision(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)

	rec, disposition := mustAdmitWithoutExecution(t, h, testSessionID, testOpID, admissionContent("hello"))
	testassert.Equal(t, disposition, DispositionAdmitted)
	testassert.Equal(t, rec.Revision, 1)
	viewed, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Equal(t, viewed.Revision, 1)
}

// corruptGateStore wraps one Storage and parks its first ReadRegisters call
// until released, serving it a scripted register set; every other call passes
// through unchanged. It forces one deterministic interleave: the parked
// caller's load holds the Session's one stable cell while the concurrent
// caller waits on it, and the parked caller reads the scripted registers. It
// counts the calls, so a row can prove that concurrent first acquisitions
// share one load.
type corruptGateStore struct {
	Storage
	mu      sync.Mutex
	reads   int
	entered chan struct{}
	release chan struct{}
	served  []Register
}

func (s *corruptGateStore) ReadRegisters(ctx context.Context, sessionID string) ([]Register, error) {
	s.mu.Lock()
	s.reads++
	first := s.reads == 1
	s.mu.Unlock()
	if !first {
		return s.Storage.ReadRegisters(ctx, sessionID)
	}
	select {
	case s.entered <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.served, nil
}

// readCount reports how many register reads the store served.
func (s *corruptGateStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// TestConcurrentMaterializationSticksDiscoveredCorruption proves sticky
// per-Session corruption isolation across two concurrent first acquisitions of
// one Session: both share the one stable cell and its one load, the parked
// caller discovers the persisted violation the store serves it, and every
// later read of that Session — including the waiter that was held behind the
// load — returns the corruption while a valid sibling stays usable.
func TestConcurrentMaterializationSticksDiscoveredCorruption(t *testing.T) {
	store := freshSessionStore(t)
	validRaw, err := encodeSessionRegister(validSessionRecord())
	testassert.NoError(t, err)
	gate := &corruptGateStore{
		Storage: store,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		served: []Register{{
			Key:      RegisterKey{SessionID: testSessionID, Kind: RegisterSession},
			Revision: 1,
			Payload:  setKey(validRaw, "bogus", json.RawMessage(`1`)),
		}},
	}
	h := newTestHarness(t, gate, nil)

	corruptErr := make(chan error, 1)
	go func() {
		_, err := h.ReadSessionHeader(context.Background(), testSessionID)
		corruptErr <- err
	}()
	<-gate.entered // the parked load holds the Session's one stable cell

	sharedErr := make(chan error, 1)
	go func() {
		_, err := h.ReadSessionHeader(context.Background(), testSessionID)
		sharedErr <- err
	}()

	close(gate.release)

	wantCorruption(t, <-corruptErr)
	// The concurrent acquirer never started a second load: it waited on the
	// stable cell and observed the same load's outcome.
	wantCorruption(t, <-sharedErr)
	{
		got := gate.readCount()
		testassert.Equal(t, got, 1)
	}
	wantCorruption(t, func() error {
		_, err := h.ReadSessionHeader(context.Background(), testSessionID)
		return err
	}())
	wantCorruption(t, func() error {
		_, err := h.ReadOperation(context.Background(), testSessionID, "op-any")
		return err
	}())

	sibling, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	{
		_, err := h.ReadSessionHeader(context.Background(), sibling.Identity.SessionID)
		testassert.NoError(t, err)
	}
}
