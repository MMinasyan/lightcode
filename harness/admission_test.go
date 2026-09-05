package harness

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/agent"
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
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// validPrepared returns a prepared execution with a valid capture and
// placeholder effect functions. The placeholder model function parks forever,
// so an auto-started execution keeps its Operation current without settling
// it: admission fixtures observe a stable running state.
func validPrepared() PreparedExecution {
	return PreparedExecution{
		Capture: testCapture(),
		Model: func(context.Context, model.Request, agent.AssemblyCallback) (agent.ModelSettlement, error) {
			select {} // the fixture owns the admitted Operation's state assertions
		},
		Tool: func(context.Context, model.ToolCall) PreparedTool { return PreparedTool{} },
	}
}

// mustAdmitWithoutExecution runs the reserved admission body without
// installing the execution — the shape the effect fixtures need to drive the
// private wrappers directly on the admitted Operation.
func mustAdmitWithoutExecution(t *testing.T, h *Harness, sessionID, operationID string, content []model.ContentPart) (OperationRecord, SubmitDisposition) {
	t.Helper()
	c, err := h.coordinatorFor(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	release, err := c.reserve(context.Background())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	defer release()
	rec, prepared, disposition, err := h.admitReserved(context.Background(), c, admissionRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Origin:      InputOriginUser,
		Content:     content,
	})
	if err != nil {
		t.Fatalf("admitReserved: %v", err)
	}
	if disposition != DispositionAdmitted || prepared == nil {
		t.Fatalf("disposition %q (prepared %v), want a new admission", disposition, prepared != nil)
	}
	return rec, disposition
}

func admissionContent(text string) []model.ContentPart {
	return []model.ContentPart{{Kind: model.PartText, Text: text}}
}

func mustAdmit(t *testing.T, h *Harness, sessionID, operationID string, content []model.ContentPart) (OperationRecord, SubmitDisposition) {
	t.Helper()
	rec, disposition, err := h.admit(context.Background(), admissionRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Origin:      InputOriginUser,
		Content:     content,
	})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return rec, disposition
}

// storedSessionState reads back the store's session fixtures for one Session.
// It returns stable snapshots: an auto-started execution mutates register
// values in place, so callers never alias the store's backing arrays.
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
	if _, err := New(context.Background(), Dependencies{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("New without dependencies = %v, want ErrInvalid", err)
	}
	if _, err := New(context.Background(), Dependencies{Storage: store}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("New without preparation = %v, want ErrInvalid", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	deps := Dependencies{Storage: store, Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) { return PreparedExecution{}, nil }}
	if _, err := New(canceled, deps); !errors.Is(err, ErrInvalid) {
		t.Fatalf("New with a dead context = %v, want ErrInvalid", err)
	}
	if _, err := New(context.Background(), deps); err != nil {
		t.Fatalf("valid construction rejected: %v", err)
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
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if rec.Identity.Workspace != "/tmp/works" {
		t.Fatalf("stored workspace = %q, want the caller value unchanged", rec.Identity.Workspace)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(rec.Identity.SessionID) {
		t.Fatalf("session id %q is not 32 lowercase hex characters", rec.Identity.SessionID)
	}
	if rec.State.Lifecycle != LifecycleOpen || rec.State.CurrentAgentType != "coder" || rec.Revision != 1 {
		t.Fatalf("created session state = %+v", rec.State)
	}
	if rec.Identity.CreatedAt.IsZero() || !rec.State.LastActivity.Equal(rec.Identity.CreatedAt) {
		t.Fatalf("creation did not sample one time for created_at and last_activity: %+v", rec)
	}
	if len(rec.State.Usage.ByModel) != 0 {
		t.Fatalf("created session carries usage: %+v", rec.State.Usage)
	}
	read, err := h.ReadSession(context.Background(), rec.Identity.SessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if read.Identity.Workspace != "/tmp/works" || read.Revision != 1 {
		t.Fatalf("read session = %+v", read)
	}

	for _, workspace := range []string{"relative/path", "/tmp/works/../works", ""} {
		if _, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: workspace, AgentType: "coder"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("CreateSession(%q) = %v, want ErrInvalid", workspace, err)
		}
	}
	if _, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty agent type = %v, want ErrInvalid", err)
	}
}

// TestReadsMaterializeValidatedState proves Session and Operation reads:
// absent records use the landed not-found class, malformed identities are
// invalid input, and a created session reads back through its materialized
// coordinator.
func TestReadsMaterializeValidatedState(t *testing.T) {
	store := emptyStore(t)
	h := newTestHarness(t, store, nil)
	if _, err := h.ReadSession(context.Background(), testSessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent session = %v, want ErrNotFound", err)
	}
	if _, err := h.ReadSession(context.Background(), "nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed session id = %v, want ErrInvalid", err)
	}
	if _, err := h.ReadOperation(context.Background(), testSessionID, testOpID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent operation = %v, want ErrNotFound", err)
	}
	created, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	read, err := h.ReadSession(context.Background(), created.Identity.SessionID)
	if err != nil || read.Revision != 1 || read.Identity.SessionID != created.Identity.SessionID {
		t.Fatalf("ReadSession = %+v, err %v", read, err)
	}
	if _, err := h.ReadOperation(context.Background(), created.Identity.SessionID, "op-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent operation after creation = %v, want ErrNotFound", err)
	}
}

// TestCorruptSessionIsUnavailable proves a persisted semantic violation makes
// its Session unavailable in this Harness instance while a valid sibling
// stays usable.
func TestCorruptSessionIsUnavailable(t *testing.T) {
	fixture := validTestGraph()
	raw, err := encodeInputEntry(*fixture.entries[0].input)
	if err != nil {
		t.Fatalf("fixture encode: %v", err)
	}
	fixture.entries[0].rawOverride = setKey(raw, "bogus", json.RawMessage(`1`))
	store := fixture.storage(t)
	h := newTestHarness(t, store, nil)

	wantCorruption(t, func() error {
		_, err := h.ReadSession(context.Background(), testSessionID)
		return err
	}())
	wantCorruption(t, func() error {
		_, err := h.ReadSession(context.Background(), testSessionID)
		return err
	}())
	wantCorruption(t, func() error {
		_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: "op-9", Origin: InputOriginUser})
		return err
	}())

	sibling, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	if err != nil {
		t.Fatalf("CreateSession on a store with a corrupt sibling: %v", err)
	}
	if _, err := h.ReadSession(context.Background(), sibling.Identity.SessionID); err != nil {
		t.Fatalf("valid sibling session unreadable: %v", err)
	}
}

// TestChangeAgentType proves the retained Agent-type transition: open-only,
// identity/usage/activity preserving, and never touching an admitted
// Operation's admission section.
func TestChangeAgentType(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, nil)

	before, err := h.ReadSession(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	after, err := h.ChangeAgentType(context.Background(), testSessionID, "reviewer")
	if err != nil {
		t.Fatalf("ChangeAgentType: %v", err)
	}
	if after.State.CurrentAgentType != "reviewer" || after.Revision != before.Revision+1 {
		t.Fatalf("changed session = %+v", after.State)
	}
	if after.Identity != before.Identity || !after.State.LastActivity.Equal(before.State.LastActivity) ||
		!usageTotalsEqual(after.State.Usage, before.State.Usage) {
		t.Fatalf("agent-type change altered identity, activity or usage: %+v", after)
	}
	if _, err := h.ChangeAgentType(context.Background(), testSessionID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty agent type = %v, want ErrInvalid", err)
	}

	archived := &testGraph{session: validSessionRecord()}
	stamped := testTime
	archived.session.State.Lifecycle = LifecycleArchived
	archived.session.State.ArchivedAt = &stamped
	archivedHarness := newTestHarness(t, archived.storage(t), nil)
	if _, err := archivedHarness.ChangeAgentType(context.Background(), testSessionID, "reviewer"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("agent type on an archived session = %v, want ErrInvalid", err)
	}
}

// TestAdmissionSessionPreconditions proves admission's session axis: the
// session must exist and be open.
func TestAdmissionSessionPreconditions(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
	if _, _, err := h.admit(context.Background(), admissionRequest{SessionID: otherSession(), OperationID: testOpID, Origin: InputOriginUser}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent session admission = %v, want ErrNotFound", err)
	}
	archived := &testGraph{session: validSessionRecord()}
	stamped := testTime
	archived.session.State.Lifecycle = LifecycleArchived
	archived.session.State.ArchivedAt = &stamped
	archivedHarness := newTestHarness(t, archived.storage(t), newPrepareStub(validPrepared()).prepare)
	if _, _, err := archivedHarness.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("archived session admission = %v, want ErrInvalid", err)
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
			_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: tc.operationID, Origin: tc.origin, Content: tc.content})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid input = %v, want ErrInvalid", err)
			}
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
	if disposition != DispositionAdmitted {
		t.Fatalf("disposition = %q, want admitted", disposition)
	}
	if rec.State.Status != OperationRunning || rec.Admission.AgentType != "coder" ||
		rec.Admission.RequestKind != RequestKindMessage || rec.Admission.SessionID != testSessionID ||
		rec.Admission.OperationID != testOpID {
		t.Fatalf("admitted operation = %+v", rec)
	}
	if rec.Admission.Execution.ConfigurationRevision != testCapture().ConfigurationRevision ||
		rec.Admission.Execution.Model != testModelRef() || len(rec.Admission.Execution.Tools) != 1 {
		t.Fatalf("durable capture = %+v", rec.Admission.Execution)
	}
	if rec.Admission.AdmittedEntry.SessionID != testSessionID || rec.Admission.AdmittedEntry.EntryID == "" {
		t.Fatalf("admitted entry reference = %+v", rec.Admission.AdmittedEntry)
	}
	if rec.State.StartedAt.IsZero() || !rec.Admission.AdmittedAt.Equal(rec.State.StartedAt) {
		t.Fatalf("operation timestamps = %+v", rec.State)
	}
	if len(rec.State.PendingToolCalls) != 0 || len(rec.State.Usage.ByModel) != 0 {
		t.Fatalf("fresh operation state = %+v", rec.State)
	}

	entries, regs := storedSessionState(store, testSessionID)
	if len(entries) != 1 || entries[0].Kind != EntryInput || entries[0].OperationID != testOpID || entries[0].Sequence != 1 {
		t.Fatalf("published entries = %+v", entries)
	}
	if _, err := decodeInputEntry(entries[0]); err != nil {
		t.Fatalf("published input entry does not decode: %v", err)
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
	if sessionReg == nil || opReg == nil {
		t.Fatalf("published registers = %+v", regs)
	}
	if opReg.Key.OperationID != testOpID {
		t.Fatalf("operation register key = %+v", opReg.Key)
	}
	session, err := decodeSessionRegister(*sessionReg)
	if err != nil {
		t.Fatalf("session register decode: %v", err)
	}
	if session.State.CurrentOperationID != testOpID || session.Revision != 2 {
		t.Fatalf("session register = revision %d, current op %q", session.Revision, session.State.CurrentOperationID)
	}
	if !session.State.LastActivity.Equal(entries[0].CommittedAt) {
		t.Fatalf("last_activity %v did not advance to the input commit time %v", session.State.LastActivity, entries[0].CommittedAt)
	}
	if _, err := validateSessionGraph(context.Background(), store, testSessionID); err != nil {
		t.Fatalf("published graph does not validate: %v", err)
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
		if _, disposition := mustAdmit(t, h, testSessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
			t.Fatalf("disposition = %q", disposition)
		}
		if got := stub.callCount(); got != 1 {
			t.Fatalf("preparation calls = %d, want exactly one", got)
		}
		req := stub.lastCall().req
		if req.RequestKind != RequestKindMessage {
			t.Fatalf("request kind = %q", req.RequestKind)
		}
		if req.Session.Identity.SessionID != testSessionID || req.Session.Identity.Workspace != "/tmp/works" ||
			req.Session.Identity.CreatedAt.IsZero() || req.Session.AgentType != "coder" {
			t.Fatalf("preparation session view = %+v", req.Session)
		}
	})

	t.Run("invalid prepared execution publishes nothing", func(t *testing.T) {
		for name, mutate := range map[string]func(*PreparedExecution){
			"nil model function": func(p *PreparedExecution) { p.Model = nil },
			"nil tool function":  func(p *PreparedExecution) { p.Tool = nil },
			"partial model ref":  func(p *PreparedExecution) { p.Capture.Model = model.ModelRef{Provider: "prov"} },
			"empty capture":      func(p *PreparedExecution) { p.Capture.ConfigurationRevision = "" },
			"duplicate tool name": func(p *PreparedExecution) {
				p.Capture.Tools = []model.ToolDefinition{testToolDefinition(), testToolDefinition()}
			},
		} {
			t.Run(name, func(t *testing.T) {
				prepared := validPrepared()
				mutate(&prepared)
				store := freshSessionStore(t)
				h := newTestHarness(t, store, newPrepareStub(prepared).prepare)
				_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("x")})
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("invalid prepared execution = %v, want ErrInvalid", err)
				}
				entries, regs := storedSessionState(store, testSessionID)
				if len(entries) != 0 || len(regs) != 1 || regs[0].Key.Kind != RegisterSession {
					t.Fatalf("invalid preparation published entries %v registers %v", entries, regs)
				}
			})
		}
	})

	t.Run("preparation error is preserved", func(t *testing.T) {
		prepErr := errors.New("preparation failed")
		stub := newPrepareStub(PreparedExecution{})
		stub.err = prepErr
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)
		_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser})
		if err != prepErr {
			t.Fatalf("preparation error = %v, want the exact callback error", err)
		}
		entries, regs := storedSessionState(store, testSessionID)
		if len(entries) != 0 || len(regs) != 1 {
			t.Fatalf("failed preparation published entries %v registers %v", entries, regs)
		}
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
			_, _, err := h.admit(ctx, admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser})
			done <- err
		}()
		<-stub.arrived // the callback is parked
		cancel()
		err := <-done
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled admission = %v, want a context error", err)
		}
		if stub.lastCall().ctx.Err() == nil {
			t.Fatalf("preparation context was not canceled with the caller")
		}
		entries, regs := storedSessionState(store, testSessionID)
		if len(entries) != 0 || len(regs) != 1 {
			t.Fatalf("canceled preparation published entries %v registers %v", entries, regs)
		}
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
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if string(rec.Admission.Execution.Tools[0].Parameters) != `{"type":"object"}` {
			t.Fatalf("durable capture shares tool bytes with the caller: %q", rec.Admission.Execution.Tools[0].Parameters)
		}
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
			if _, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("first")}); err != nil {
				t.Errorf("first admission: %v", err)
			}
			first <- struct{}{}
		}()
		<-stub.arrived // the first admission is parked in preparation

		readDone := make(chan struct{}, 1)
		go func() {
			if _, err := h.ReadSession(context.Background(), testSessionID); err != nil {
				t.Errorf("ReadSession during preparation: %v", err)
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
			rec, disposition, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("second")})
			if err != nil {
				t.Errorf("second admission: %v", err)
			}
			if disposition != DispositionExisting {
				t.Errorf("second admission disposition = %q, want existing", disposition)
			}
			if rec.Admission.AdmittedAt != firstAdmittedAt(t, h, testSessionID, testOpID) {
				t.Errorf("second admission returned a different operation")
			}
			second <- struct{}{}
		}()

		close(gate)
		<-first
		<-second
		if got := stub.callCount(); got != 1 {
			t.Fatalf("preparation calls = %d, want one: the blocked admission must not prepare again", got)
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
			_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser})
			done <- err
		}()
		<-stub.arrived

		if err := store.Transact(context.Background(), func(tx Transaction) error {
			return foreignAgentTypeChange(tx, testSessionID, "foreign")
		}); err != nil {
			t.Fatalf("foreign mutation: %v", err)
		}
		close(gate)
		err := <-done
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("raced admission = %v, want ErrConflict", err)
		}
		entries, regs := storedSessionState(store, testSessionID)
		if len(entries) != 0 || len(regs) != 1 || regs[0].Revision != 2 {
			t.Fatalf("conflicted admission published entries %v registers %v", entries, regs)
		}
	})

	t.Run("agent-type change during preparation affects later admission only", func(t *testing.T) {
		gate := make(chan struct{})
		stub := newPrepareStub(validPrepared())
		stub.gate = gate
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)

		admitted := make(chan struct{}, 1)
		go func() {
			rec, disposition, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser})
			if err != nil {
				t.Errorf("admission: %v", err)
			}
			if disposition != DispositionAdmitted || rec.Admission.AgentType != "coder" {
				t.Errorf("admitted operation = %+v disposition %q", rec, disposition)
			}
			admitted <- struct{}{}
		}()
		<-stub.arrived

		changed := make(chan struct{}, 1)
		go func() {
			rec, err := h.ChangeAgentType(context.Background(), testSessionID, "reviewer")
			if err != nil {
				t.Errorf("ChangeAgentType: %v", err)
			}
			if rec.State.CurrentAgentType != "reviewer" {
				t.Errorf("changed session = %+v", rec.State)
			}
			changed <- struct{}{}
		}()

		close(gate)
		<-admitted
		<-changed
		rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.Admission.AgentType != "coder" {
			t.Fatalf("admitted operation agent type = %q, want the capture-time value", rec.Admission.AgentType)
		}
		session, err := h.ReadSession(context.Background(), testSessionID)
		if err != nil || session.State.CurrentAgentType != "reviewer" {
			t.Fatalf("session agent type = %+v err %v", session, err)
		}
	})
}

func firstAdmittedAt(t *testing.T, h *Harness, sessionID, operationID string) time.Time {
	t.Helper()
	rec, err := h.ReadOperation(context.Background(), sessionID, operationID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
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
			_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("x")})
			if err != injected {
				t.Fatalf("admission error = %v, want the injected failure", err)
			}
			entries, regs := storedSessionState(store, testSessionID)
			if len(entries) != 0 || len(regs) != 1 || regs[0].Revision != 1 {
				t.Fatalf("partial failure published entries %v registers %v", entries, regs)
			}
			store.txHook = nil
			if _, disposition := mustAdmit(t, h, testSessionID, testOpID, admissionContent("x")); disposition != DispositionAdmitted {
				t.Fatalf("retry disposition = %q", disposition)
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
		if firstDisp != DispositionAdmitted {
			t.Fatalf("first disposition = %q", firstDisp)
		}
		retry, retryDisp := mustAdmit(t, h, testSessionID, testOpID, admissionContent("second"))
		if retryDisp != DispositionExisting {
			t.Fatalf("retry disposition = %q, want existing", retryDisp)
		}
		if retry.Admission.AdmittedEntry != first.Admission.AdmittedEntry || !retry.Admission.AdmittedAt.Equal(first.Admission.AdmittedAt) {
			t.Fatalf("retry returned a different operation: %+v vs %+v", retry, first)
		}
		entries, regs := storedSessionState(store, testSessionID)
		if len(entries) != 1 || len(regs) != 2 {
			t.Fatalf("retry published entries %v registers %v", entries, regs)
		}
		decoded, err := decodeInputEntry(entries[0])
		if err != nil || decoded.Content[0].Text != "first" {
			t.Fatalf("retry replaced the first payload: %+v err %v", decoded, err)
		}
		if got := stub.callCount(); got != 1 {
			t.Fatalf("preparation calls = %d, want one", got)
		}
	})

	t.Run("admitted race after preparation returns the first operation", func(t *testing.T) {
		gate := make(chan struct{})
		stub := newPrepareStub(validPrepared())
		stub.gate = gate
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)

		dispCh := make(chan SubmitDisposition, 1)
		go func() {
			_, disposition, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("raced")})
			if err != nil {
				t.Errorf("raced admission: %v", err)
			}
			dispCh <- disposition
		}()
		<-stub.arrived

		// A concurrent admission of the same identity wins: it publishes the
		// full input, operation, and session state directly into storage.
		if err := store.Transact(context.Background(), func(tx Transaction) error {
			return insertForeignAdmission(t, tx, testSessionID, testOpID)
		}); err != nil {
			t.Fatalf("foreign admission: %v", err)
		}
		close(gate)
		disposition := <-dispCh
		if disposition != DispositionExisting {
			t.Fatalf("raced admission disposition = %q, want existing", disposition)
		}
		entries, regs := storedSessionState(store, testSessionID)
		if len(entries) != 1 || len(regs) != 2 {
			t.Fatalf("raced admission published entries %v registers %v", entries, regs)
		}
		if got := stub.callCount(); got != 1 {
			t.Fatalf("preparation calls = %d, want one", got)
		}
		rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		if rec.Admission.Execution.ConfigurationRevision != testCapture().ConfigurationRevision {
			t.Fatalf("raced admission returned the wrong operation: %+v", rec)
		}
	})

	t.Run("global operation reuse in another session is invalid", func(t *testing.T) {
		store := freshSessionStore(t)
		h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
		mustAdmit(t, h, testSessionID, "shared-op", admissionContent("x"))
		created, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		_, _, err = h.admit(context.Background(), admissionRequest{SessionID: created.Identity.SessionID, OperationID: "shared-op", Origin: InputOriginUser, Content: admissionContent("y")})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("cross-session reuse = %v, want ErrInvalid", err)
		}
		entries, regs := storedSessionState(store, created.Identity.SessionID)
		if len(entries) != 0 || len(regs) != 1 {
			t.Fatalf("rejected reuse published entries %v registers %v", entries, regs)
		}
	})

	t.Run("second active operation is invalid", func(t *testing.T) {
		store := freshSessionStore(t)
		h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
		mustAdmit(t, h, testSessionID, testOpID, admissionContent("x"))
		_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: "op-2", Origin: InputOriginUser, Content: admissionContent("y")})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("second active operation = %v, want ErrInvalid", err)
		}
		entries, regs := storedSessionState(store, testSessionID)
		if len(entries) != 1 || len(regs) != 2 {
			t.Fatalf("rejected admission published entries %v registers %v", entries, regs)
		}
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
// preconditions are re-checked against the coordinator state the reservation
// resolved on, before preparation runs.
func TestAdmissionRechecksPreconditionsAfterReservation(t *testing.T) {
	t.Run("loser of a different-operation race returns invalid without preparing", func(t *testing.T) {
		gate := make(chan struct{})
		stub := newPrepareStub(validPrepared())
		stub.gate = gate
		store := freshSessionStore(t)
		h := newTestHarness(t, store, stub.prepare)

		first := make(chan struct{}, 1)
		go func() {
			if _, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("first")}); err != nil {
				t.Errorf("first admission: %v", err)
			}
			first <- struct{}{}
		}()
		<-stub.arrived // the first admission is parked in preparation

		lostErr := make(chan error, 1)
		go func() {
			_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: "op-2", Origin: InputOriginUser, Content: admissionContent("second")})
			lostErr <- err
		}()

		close(gate)
		<-first
		err := <-lostErr
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("raced second admission = %v, want ErrInvalid", err)
		}
		if got := stub.callCount(); got != 1 {
			t.Fatalf("preparation calls = %d, want one: the loser must not prepare", got)
		}
		entries, regs := storedSessionState(store, testSessionID)
		if len(entries) != 1 || len(regs) != 2 {
			t.Fatalf("loser published entries %v registers %v", entries, regs)
		}
	})

	t.Run("foreign archive between materialization and admission resolves at commit", func(t *testing.T) {
		store := freshSessionStore(t)
		stub := newPrepareStub(validPrepared())
		h := newTestHarness(t, store, stub.prepare)
		if _, err := h.ReadSession(context.Background(), testSessionID); err != nil { // materialize the open session
			t.Fatalf("ReadSession: %v", err)
		}
		if err := store.Transact(context.Background(), func(tx Transaction) error {
			return foreignArchiveChange(tx, testSessionID)
		}); err != nil {
			t.Fatalf("foreign archive: %v", err)
		}
		_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("admission over a foreign archive = %v, want ErrInvalid", err)
		}
		if got := stub.callCount(); got != 1 {
			t.Fatalf("preparation calls = %d, want one: preparation may run against the stale view", got)
		}
		entries, regs := storedSessionState(store, testSessionID)
		if len(entries) != 0 || len(regs) != 1 || regs[0].Revision != 2 {
			t.Fatalf("refused admission left entries %v registers %v", entries, regs)
		}
		session, err := h.ReadSession(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if session.State.Lifecycle != LifecycleArchived {
			t.Fatalf("read session kept the stale view: %+v", session.State)
		}
	})

	t.Run("foreign committed running operation refuses at commit", func(t *testing.T) {
		store := freshSessionStore(t)
		stub := newPrepareStub(validPrepared())
		h := newTestHarness(t, store, stub.prepare)
		if _, err := h.ReadSession(context.Background(), testSessionID); err != nil { // materialize the idle open session
			t.Fatalf("ReadSession: %v", err)
		}
		if err := store.Transact(context.Background(), func(tx Transaction) error {
			return insertForeignAdmission(t, tx, testSessionID, "foreign-op")
		}); err != nil {
			t.Fatalf("foreign admission: %v", err)
		}
		_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("admission over a foreign running operation = %v, want ErrInvalid", err)
		}
		if got := stub.callCount(); got != 1 {
			t.Fatalf("preparation calls = %d, want one: preparation may run against the stale view", got)
		}
		entries, regs := storedSessionState(store, testSessionID)
		if len(entries) != 1 || len(regs) != 2 {
			t.Fatalf("refused admission left entries %v registers %v", entries, regs)
		}
		session, err := h.ReadSession(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if session.State.CurrentOperationID != "foreign-op" {
			t.Fatalf("read session kept the stale view: %+v", session.State)
		}
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
			_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser})
			done <- err
		}()
		<-stub.arrived
		if err := store.Transact(context.Background(), func(tx Transaction) error {
			return foreignAgentTypeChange(tx, testSessionID, "foreign")
		}); err != nil {
			t.Fatalf("foreign mutation: %v", err)
		}
		close(gate)
		if err := <-done; !errors.Is(err, ErrConflict) {
			t.Fatalf("raced admission = %v, want ErrConflict", err)
		}
		session, err := h.ReadSession(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if session.State.CurrentAgentType != "foreign" {
			t.Fatalf("read session kept the stale view: %+v", session.State)
		}
	})

	t.Run("failed agent-type change", func(t *testing.T) {
		store := freshSessionStore(t)
		h := newTestHarness(t, store, nil)
		if _, err := h.ReadSession(context.Background(), testSessionID); err != nil { // materialize revision 1
			t.Fatalf("ReadSession: %v", err)
		}
		if err := store.Transact(context.Background(), func(tx Transaction) error {
			return foreignAgentTypeChange(tx, testSessionID, "foreign")
		}); err != nil {
			t.Fatalf("foreign mutation: %v", err)
		}
		if _, err := h.ChangeAgentType(context.Background(), testSessionID, "reviewer"); !errors.Is(err, ErrConflict) {
			t.Fatalf("raced change = %v, want ErrConflict", err)
		}
		session, err := h.ReadSession(context.Background(), testSessionID)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if session.State.CurrentAgentType != "foreign" {
			t.Fatalf("read session kept the stale view: %+v", session.State)
		}
	})
}

// TestHarnessCancellationGatesPublication proves publication never starts
// once the Harness context is done: nothing is published and the Harness
// context error is returned while the caller context stays live.
func TestHarnessCancellationGatesPublication(t *testing.T) {
	harnessCtx, cancelHarness := context.WithCancel(context.Background())
	defer cancelHarness()
	gate := make(chan struct{})
	stub := newPrepareStub(validPrepared())
	stub.gate = gate
	stub.ignoreCtx = true // preparation completes despite the canceled harness context
	store := freshSessionStore(t)
	h, err := New(harnessCtx, Dependencies{Storage: store, Prepare: stub.prepare})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser})
		done <- err
	}()
	<-stub.arrived
	cancelHarness()
	close(gate)
	err = <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("admission over a canceled harness context = %v, want a context error", err)
	}
	entries, regs := storedSessionState(store, testSessionID)
	if len(entries) != 0 || len(regs) != 1 || regs[0].Revision != 1 {
		t.Fatalf("canceled publication left entries %v registers %v", entries, regs)
	}
}

// TestReadOperationReturnsOwnedRecords proves every pointer field of a
// returned Operation record is an independent copy of the coordinator state.
func TestReadOperationReturnsOwnedRecords(t *testing.T) {
	t.Run("active effect pointer", func(t *testing.T) {
		fixture := validTestGraph()
		fixture.ops[0].State.ActiveEffect = &ActiveEffect{Kind: EffectModel, ResultEntryID: hexID(3)}
		h := newTestHarness(t, fixture.storage(t), nil)
		rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		rec.State.ActiveEffect.Kind = EffectTool
		rec.State.ActiveEffect.ResultEntryID = hexID(9)
		again, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if again.State.ActiveEffect == nil || again.State.ActiveEffect.Kind != EffectModel || again.State.ActiveEffect.ResultEntryID != hexID(3) {
			t.Fatalf("returned record shares the active effect with the coordinator view: %+v", again.State.ActiveEffect)
		}
	})

	t.Run("terminal pointers", func(t *testing.T) {
		h := newTestHarness(t, terminalTestGraph().storage(t), nil)
		rec, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		if err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		other := testTime.Add(time.Hour)
		*rec.State.SettledAt = other
		rec.State.Terminal.Detail = "tampered"
		rec.State.Terminal.SettlementEntry.EntryID = hexID(9)
		again, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if again.State.SettledAt == nil || !again.State.SettledAt.Equal(testTime) ||
			again.State.Terminal == nil || again.State.Terminal.Detail != "" ||
			again.State.Terminal.SettlementEntry.EntryID != hexID(3) {
			t.Fatalf("returned record shares terminal pointers with the coordinator view: %+v", again.State)
		}
	})
}

// TestAdmissionOwnsInputContent proves the committed view entry carries the
// validated owned content copies, not the caller's slice.
func TestAdmissionOwnsInputContent(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)
	content := []model.ContentPart{{Kind: model.PartText, Text: "hello", Extra: model.Extra{"k": json.RawMessage(`"v"`)}}}
	if _, disposition := mustAdmit(t, h, testSessionID, testOpID, content); disposition != DispositionAdmitted {
		t.Fatalf("disposition = %q", disposition)
	}
	content[0].Text = "tampered"
	content[0].Extra["k"] = json.RawMessage(`"tampered"`)
	h.mu.Lock()
	c := h.sessions[testSessionID]
	h.mu.Unlock()
	c.mu.Lock()
	entry := *c.graph.Entries[0].Input
	c.mu.Unlock()
	if entry.Content[0].Text != "hello" || string(entry.Content[0].Extra["k"]) != `"v"` {
		t.Fatalf("coordinator view shares input content with the caller: %+v", entry.Content)
	}
}

// TestReadOperationValidatesIdentity proves an empty operation identity is
// invalid input while a well-formed absent one stays not-found.
func TestReadOperationValidatesIdentity(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, nil)
	if _, err := h.ReadOperation(context.Background(), testSessionID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty operation id = %v, want ErrInvalid", err)
	}
	if _, err := h.ReadOperation(context.Background(), testSessionID, "op-ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent operation id = %v, want ErrNotFound", err)
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
	if _, disposition := mustAdmit(t, h, testSessionID, testOpID, admissionContent("first")); disposition != DispositionAdmitted {
		t.Fatalf("first disposition = %q", disposition)
	}
	h.mu.Lock()
	c := h.sessions[testSessionID]
	h.mu.Unlock()
	stamped := testTime
	c.mu.Lock()
	c.graph.Session.State.Lifecycle = LifecycleArchived
	c.graph.Session.State.ArchivedAt = &stamped
	c.mu.Unlock()

	_, disposition, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser, Content: admissionContent("retry")})
	if err != nil {
		t.Fatalf("retry on an archived session = %v, want the existing Operation", err)
	}
	if disposition != DispositionExisting {
		t.Fatalf("retry disposition = %q, want existing", disposition)
	}
}

// TestAdmitDeadContextSkipsPreparation proves a dead combined context never
// invokes Prepare: nothing publishes and the applicable context error returns.
func TestAdmitDeadContextSkipsPreparation(t *testing.T) {
	harnessCtx, cancelHarness := context.WithCancel(context.Background())
	stub := newPrepareStub(validPrepared())
	store := freshSessionStore(t)
	h, err := New(harnessCtx, Dependencies{Storage: store, Prepare: stub.prepare})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cancelHarness() // the harness context dies after construction, before admission
	_, _, admitErr := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser})
	if !errors.Is(admitErr, context.Canceled) {
		t.Fatalf("admission over a dead harness context = %v, want a context error", admitErr)
	}
	if got := stub.callCount(); got != 0 {
		t.Fatalf("preparation calls = %d, want zero: a dead context must not invoke Prepare", got)
	}
	entries, regs := storedSessionState(store, testSessionID)
	if len(entries) != 0 || len(regs) != 1 {
		t.Fatalf("dead-context admission left entries %v registers %v", entries, regs)
	}
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
		if _, err := h.ReadSession(context.Background(), testSessionID); err != nil { // materialize revision 1
			t.Fatalf("ReadSession: %v", err)
		}
		if err := store.Transact(context.Background(), func(tx Transaction) error { return foreignCorruptRevision(tx, testSessionID) }); err != nil {
			t.Fatalf("foreign corrupt revision: %v", err)
		}
		_, _, err := h.admit(context.Background(), admissionRequest{SessionID: testSessionID, OperationID: testOpID, Origin: InputOriginUser})
		var corrupt *CorruptionError
		if !errors.As(err, &corrupt) {
			t.Fatalf("admission over a corrupt foreign revision = %v, want the discovered CorruptionError", err)
		}
		if _, rerr := h.ReadSession(context.Background(), testSessionID); !errors.As(rerr, &corrupt) {
			t.Fatalf("subsequent read = %v, want the marked CorruptionError", rerr)
		}
	})

	t.Run("agent-type change", func(t *testing.T) {
		store := freshSessionStore(t)
		h := newTestHarness(t, store, nil)
		if _, err := h.ReadSession(context.Background(), testSessionID); err != nil { // materialize revision 1
			t.Fatalf("ReadSession: %v", err)
		}
		if err := store.Transact(context.Background(), func(tx Transaction) error { return foreignCorruptRevision(tx, testSessionID) }); err != nil {
			t.Fatalf("foreign corrupt revision: %v", err)
		}
		_, err := h.ChangeAgentType(context.Background(), testSessionID, "reviewer")
		var corrupt *CorruptionError
		if !errors.As(err, &corrupt) {
			t.Fatalf("agent-type change over a corrupt foreign revision = %v, want the discovered CorruptionError", err)
		}
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
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if rec.Admission.Execution.Tools != nil {
		t.Fatalf("admitted capture retained the caller's zero-length backing: %+v", rec.Admission.Execution.Tools)
	}
}

// TestAdmittedOperationCarriesRegisterRevision proves the admitted Operation
// record carries the revision storage assigned to its register at insert, in
// both the returned value and the coordinator view a later read observes.
func TestAdmittedOperationCarriesRegisterRevision(t *testing.T) {
	store := freshSessionStore(t)
	h := newTestHarness(t, store, newPrepareStub(validPrepared()).prepare)

	rec, disposition := mustAdmit(t, h, testSessionID, testOpID, admissionContent("hello"))
	if disposition != DispositionAdmitted {
		t.Fatalf("disposition = %q, want admitted", disposition)
	}
	if rec.Revision != 1 {
		t.Fatalf("admitted operation revision = %d, want the register revision assigned at insert", rec.Revision)
	}
	viewed, err := h.ReadOperation(context.Background(), testSessionID, testOpID)
	if err != nil {
		t.Fatalf("ReadOperation: %v", err)
	}
	if viewed.Revision != 1 {
		t.Fatalf("coordinator-view operation revision = %d, want the register revision assigned at insert", viewed.Revision)
	}
}

// corruptGateStore wraps one Storage and parks its first ReadRegisters call
// until released, serving it a scripted register set; every other call passes
// through unchanged. It forces one deterministic interleave of two concurrent
// first materializations: the parked caller reads storage before the other
// caller installs its coordinator and sees the scripted registers after.
type corruptGateStore struct {
	Storage
	mu      sync.Mutex
	seen    bool
	entered chan struct{}
	release chan struct{}
	served  []Register
}

func (s *corruptGateStore) ReadRegisters(ctx context.Context, sessionID string) ([]Register, error) {
	s.mu.Lock()
	first := !s.seen
	s.seen = true
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

// TestConcurrentMaterializationSticksDiscoveredCorruption proves sticky
// per-Session corruption isolation across two concurrent first
// materializations of one Session: storage serves a valid graph to the caller
// that installs its coordinator and a persisted violation to the parked
// caller that discovers it afterwards; every later read of that Session then
// returns the corruption while a valid sibling stays usable.
func TestConcurrentMaterializationSticksDiscoveredCorruption(t *testing.T) {
	store := freshSessionStore(t)
	validRaw, err := encodeSessionRegister(validSessionRecord())
	mustEncode(t, err)
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
		_, err := h.ReadSession(context.Background(), testSessionID)
		corruptErr <- err
	}()
	<-gate.entered // the parked materialization saw no installed coordinator

	validDone := make(chan SessionRecord, 1)
	go func() {
		rec, err := h.ReadSession(context.Background(), testSessionID)
		if err != nil {
			t.Errorf("valid materialization: %v", err)
			validDone <- SessionRecord{}
			return
		}
		validDone <- rec
	}()
	validRec := <-validDone // the valid coordinator is installed before the discovery
	close(gate.release)

	wantCorruption(t, <-corruptErr)
	if validRec.Identity.SessionID != testSessionID || validRec.State.Lifecycle != LifecycleOpen {
		t.Fatalf("valid materialization = %+v", validRec)
	}
	wantCorruption(t, func() error {
		_, err := h.ReadSession(context.Background(), testSessionID)
		return err
	}())
	wantCorruption(t, func() error {
		_, err := h.ReadOperation(context.Background(), testSessionID, "op-any")
		return err
	}())

	sibling, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	if err != nil {
		t.Fatalf("CreateSession with a corrupt sibling: %v", err)
	}
	if _, err := h.ReadSession(context.Background(), sibling.Identity.SessionID); err != nil {
		t.Fatalf("valid sibling session unreadable: %v", err)
	}
}
