package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/model"
)

// modelScript is the scripted model effect of the coordination fixtures: it
// records every projected request, signals each arrival, optionally parks
// every invocation until its gate closes, assembles once behind every
// output-bearing settlement, and answers from a scripted settlement list.
type modelScript struct {
	mu          sync.Mutex
	requests    []model.Request
	arrived     chan struct{}
	gate        chan struct{}
	settlements []agent.ModelSettlement
	model       agent.ModelEffect
}

func newModelScript(settlements ...agent.ModelSettlement) *modelScript {
	s := &modelScript{arrived: make(chan struct{}, 16), settlements: settlements}
	s.model = s.invoke
	return s
}

func (s *modelScript) invoke(_ context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	gate := s.gate
	s.mu.Unlock()
	s.arrived <- struct{}{}
	if gate != nil {
		<-gate
	}
	var set agent.ModelSettlement
	if len(s.settlements) > 0 {
		set = s.settlements[0]
		s.settlements = s.settlements[1:]
	} else {
		set = agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}
	}
	if set.Output != nil { // an output-bearing settlement requires exactly one assembly
		if err := assembleCompleted(assemble); err != nil {
			return agent.ModelSettlement{}, err
		}
	}
	return set, nil
}

func (s *modelScript) seen() []model.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Request(nil), s.requests...)
}

// textsOf returns one request's projected message texts.
func textsOf(req model.Request) []string {
	out := make([]string, 0, len(req.Messages))
	for _, msg := range req.Messages {
		out = append(out, msg.TextContent())
	}
	return out
}

func (s *modelScript) lastTexts() []string {
	reqs := s.seen()
	return textsOf(reqs[len(reqs)-1])
}

// releaseGate unblocks every parked invocation and stops parking later ones.
func (s *modelScript) releaseGate() {
	s.mu.Lock()
	gate := s.gate
	s.gate = nil
	s.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// settleWatch turns the fake storage's entry hook into a deterministic
// settlement barrier: one signal per committed operation-settlement entry.
type settleWatch struct {
	mu    sync.Mutex
	count int
	seen  chan struct{}
}

func watchSettlements(store *graphStorage) *settleWatch {
	w := &settleWatch{seen: make(chan struct{}, 64)}
	store.entryHook = func(draft EntryDraft) error {
		if draft.Kind == EntryOperationSettlement {
			w.mu.Lock()
			w.count++
			w.mu.Unlock()
			w.seen <- struct{}{}
		}
		return nil
	}
	return w
}

func (w *settleWatch) next() { <-w.seen }

// settlements reports how many settlement entries committed so far.
func (w *settleWatch) settlements() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

// newCancelableHarness returns a Harness over a cancelable context whose
// preparation returns the given prepared execution, or the override callback's
// result when one is supplied.
func newCancelableHarness(t *testing.T, store *graphStorage, prepared PreparedExecution, prepare func(context.Context, PreparationRequest) (PreparedExecution, error)) (*Harness, context.CancelFunc) {
	t.Helper()
	if prepare == nil {
		prepare = func(context.Context, PreparationRequest) (PreparedExecution, error) { return prepared, nil }
	}
	hctx, cancel := context.WithCancel(context.Background())
	h, err := New(hctx, Dependencies{Storage: store, Prepare: prepare})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, cancel
}

func createSession(t *testing.T, h *Harness) string {
	t.Helper()
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return session.Identity.SessionID
}

// submitText is the Submit convenience of the coordination fixtures: one text
// part from the user origin.
func submitText(t *testing.T, h *Harness, sessionID, operationID string, mode MessageMode, text string) (SubmitResult, error) {
	t.Helper()
	return h.Submit(context.Background(), SubmitRequest{
		SessionID:   sessionID,
		OperationID: operationID,
		Origin:      InputOriginUser,
		Content:     admissionContent(text),
		Mode:        mode,
	})
}

// entryTexts returns the text of every committed input entry in graph order,
// read straight from durable storage.
func entryTexts(t *testing.T, store *graphStorage, sessionID string) []string {
	t.Helper()
	graph, err := validateFixture(t, store, sessionID)
	if err != nil {
		t.Fatalf("session graph: %v", err)
	}
	out := []string{}
	for _, entry := range graph.Entries {
		if entry.Input != nil {
			out = append(out, entry.Input.Content[0].Text)
		}
	}
	return out
}

// settledOperation reads one operation register straight from durable storage:
// the store's mutex synchronizes the read with the committing transaction, so
// a settlement-watch barrier followed by this read observes terminal state
// without polling.
func settledOperation(t *testing.T, store *graphStorage, sessionID, operationID string) OperationRecord {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID})
	if err != nil {
		t.Fatalf("read operation register %q: %v", operationID, err)
	}
	rec, err := decodeOperationRegister(reg)
	if err != nil {
		t.Fatalf("decode operation register %q: %v", operationID, err)
	}
	return rec
}

// TestSubmitRoutesIdleAndActive proves the routing row: idle regular and idle
// queued input use normal admission, active regular input enters steering and
// active queued input enters queued without preparing or publishing, and an
// existing identity resolves before routing.
func TestSubmitRoutesIdleAndActive(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	script.gate = make(chan struct{})
	prepared := validPrepared()
	prepared.Model = script.model
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)

	res, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
	if err != nil || res.Disposition != DispositionAdmitted || res.Operation == nil || res.Operation.State.Status != OperationRunning {
		t.Fatalf("idle regular submit = %+v err %v, want admitted with the record", res, err)
	}
	<-script.arrived // the execution reached its parked model boundary: the Session is active

	steered, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1")
	if err != nil || steered.Disposition != DispositionSteering || steered.Operation != nil {
		t.Fatalf("active regular submit = %+v err %v, want steering without a record", steered, err)
	}
	queued, err := submitText(t, h, session, "op-3", MessageModeQueued, "q1")
	if err != nil || queued.Disposition != DispositionQueued || queued.Operation != nil {
		t.Fatalf("active queued submit = %+v err %v, want queued without a record", queued, err)
	}
	existing, err := submitText(t, h, session, "op-1", MessageModeRegular, "retry")
	if err != nil || existing.Disposition != DispositionExisting || existing.Operation == nil ||
		existing.Operation.Admission.OperationID != "op-1" {
		t.Fatalf("active existing submit = %+v err %v, want the first record before routing", existing, err)
	}
	if entries, regs := storedSessionState(store, session); len(entries) != 1 || len(regs) != 2 {
		t.Fatalf("buffered submits published entries %v registers %v", entries, regs)
	}

	if _, err := submitText(t, h, session, "op-4", MessageMode("urgent"), "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown mode = %v, want ErrInvalid", err)
	}

	// Idle queued input uses normal admission: a fresh idle Session admits it.
	other := createSession(t, h)
	idleQueued, err := submitText(t, h, other, "idle-queued", MessageModeQueued, "queued-idle")
	if err != nil || idleQueued.Disposition != DispositionAdmitted || idleQueued.Operation == nil {
		t.Fatalf("idle queued submit = %+v err %v, want normal admission", idleQueued, err)
	}
}

// TestSteeringDrainsAtModelBoundaryInFIFOOrder proves the steering row: while
// an Operation is active, waiting steering is committed as ordinary user input
// in FIFO order at the model boundary, the next request projects it in the
// same Operation, and a ready boundary with waiting steering continues the
// Operation instead of returning.
func TestSteeringDrainsAtModelBoundaryInFIFOOrder(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	gate := make(chan struct{})
	script.gate = gate
	prepared := validPrepared()
	prepared.Model = script.model
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)

	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	<-script.arrived // parked at the first model boundary
	if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1"); err != nil {
		t.Fatalf("steering submit 1: %v", err)
	}
	if _, err := submitText(t, h, session, "op-3", MessageModeRegular, "s2"); err != nil {
		t.Fatalf("steering submit 2: %v", err)
	}

	script.releaseGate() // the first model result commits with steering waiting
	<-script.arrived
	if texts := strings.Join(script.lastTexts(), "|"); texts != "system|hello|done|s1|s2" {
		t.Fatalf("continuation projection = %q, want the drained steering after the first turn's history", texts)
	}
	watch.next()
	if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationSuccess {
		t.Fatalf("steered operation settled %q, want success", rec.State.Status)
	}
	if got := strings.Join(entryTexts(t, store, session), ","); got != "hello,s1,s2" {
		t.Fatalf("committed inputs = %q, want steering committed in FIFO order", got)
	}
}

// TestSteeringContinuationAcrossOutputShapes proves the boundary decision is
// independent of output shape: steering continues the Operation across a
// completed output without calls, a completed output with calls, and an
// errored output retaining a payload — and an errored output without a
// payload settles failure while its steering drains through ordinary
// admission.
func TestSteeringContinuationAcrossOutputShapes(t *testing.T) {
	cases := []struct {
		name    string
		settle  agent.ModelSettlement
		wantEnd OperationState
	}{
		{"completed output without calls", agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()}, OperationSuccess},
		{"completed output with calls", agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith(testToolCall("call-1"))}, OperationSuccess},
		{"errored output with payload", agent.ModelSettlement{Disposition: agent.DispoContinue, Output: erroredOutputWith()}, OperationSuccess},
		{"errored output without payload", agent.ModelSettlement{Disposition: agent.DispoContinue, Output: &model.Output{
			Status: model.OutputErrored, Source: testModelRef(), Detail: "provider failure",
		}}, OperationFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := emptyStore(t)
			script := newModelScript(tc.settle)
			gate := make(chan struct{})
			script.gate = gate
			prepared := validPrepared()
			prepared.Model = script.model
			h, cancel := newCancelableHarness(t, store, prepared, nil)
			defer cancel()
			session := createSession(t, h)
			watch := watchSettlements(store)

			if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
				t.Fatalf("first submit: %v", err)
			}
			<-script.arrived
			if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer"); err != nil {
				t.Fatalf("steering submit: %v", err)
			}
			script.releaseGate()

			if tc.wantEnd == OperationSuccess { // the continuation projected the drained steering
				<-script.arrived
				if texts := strings.Join(script.lastTexts(), "|"); !strings.Contains(texts, "steer") {
					t.Fatalf("continuation projection = %q, want the drained steering", texts)
				}
				watch.next()
				if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != tc.wantEnd {
					t.Fatalf("operation settled %q, want %q", rec.State.Status, tc.wantEnd)
				}
				return
			}
			// A payload-less errored continuation settles failure: the
			// preserved steering drains through ordinary admission.
			watch.next()
			if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != tc.wantEnd {
				t.Fatalf("operation settled %q, want %q", rec.State.Status, tc.wantEnd)
			}
			<-script.arrived
			watch.next()
			if texts := strings.Join(script.lastTexts(), "|"); texts != "system|hello|steer" {
				t.Fatalf("admitted steering projection = %q, want the preserved steering as a new admission", texts)
			}
		})
	}
}

// TestPostTerminalQueuedDrain proves the queued row: queued input defers to
// the Agent turn end, then drains one item per terminal in FIFO order through
// ordinary admission.
func TestPostTerminalQueuedDrain(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	gate := make(chan struct{})
	script.gate = gate
	prepared := validPrepared()
	prepared.Model = script.model
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)

	if _, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	<-script.arrived
	if _, err := submitText(t, h, session, "turn-2", MessageModeQueued, "q1"); err != nil {
		t.Fatalf("queued submit 1: %v", err)
	}
	if _, err := submitText(t, h, session, "turn-3", MessageModeQueued, "q2"); err != nil {
		t.Fatalf("queued submit 2: %v", err)
	}

	script.releaseGate() //turn-1 settles; the drain admits q1, then q2 after its terminal
	watch.next()
	watch.next()
	if rec := settledOperation(t, store, session, "turn-2"); rec.State.Status != OperationSuccess {
		t.Fatalf("first drained turn settled %q, want success", rec.State.Status)
	}
	watch.next()
	if rec := settledOperation(t, store, session, "turn-3"); rec.State.Status != OperationSuccess {
		t.Fatalf("second drained turn settled %q, want success", rec.State.Status)
	}
	if texts := strings.Join(script.lastTexts(), "|"); texts != "system|hello|done|q1|done|q2" {
		t.Fatalf("last drained projection = %q, want FIFO order through q2", texts)
	}
	if got := strings.Join(entryTexts(t, store, session), ","); got != "hello,q1,q2" {
		t.Fatalf("committed inputs = %q, want one admitted queued item per turn", got)
	}
}

// TestBufferedItemFailureIsFinal proves the buffer-lifetime row: a failed
// delivery attempt — failed preparation, failed admission, or a failed
// delivered Operation — is final for the item, the drain advances to the next
// buffered message, and nothing is retained or retried.
func TestBufferedItemFailureIsFinal(t *testing.T) {
	t.Run("failed preparation drops the item and proceeds", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		prepared := validPrepared()
		prepared.Model = script.model
		calls := 0
		h, cancel := newCancelableHarness(t, store, prepared, func(_ context.Context, _ PreparationRequest) (PreparedExecution, error) {
			calls++
			if calls == 2 { // the first drained queued item's preparation fails
				return PreparedExecution{}, errors.New("preparation broke")
			}
			return prepared, nil
		})
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived
		if _, err := submitText(t, h, session, "turn-2", MessageModeQueued, "q1"); err != nil {
			t.Fatalf("queued submit 1: %v", err)
		}
		if _, err := submitText(t, h, session, "turn-3", MessageModeQueued, "q2"); err != nil {
			t.Fatalf("queued submit 2: %v", err)
		}
		script.releaseGate()

		watch.next()
		watch.next()
		if rec := settledOperation(t, store, session, "turn-3"); rec.State.Status != OperationSuccess {
			t.Fatalf("next buffered message settled %+v, want q2 admitted and successful", rec.State)
		}
		if calls != 3 {
			t.Fatalf("preparation calls = %d, want one per admitted turn with the failed item dropped", calls)
		}
		if got := strings.Join(entryTexts(t, store, session), ","); got != "hello,q2" {
			t.Fatalf("committed inputs = %q, want the failed item gone without retry", got)
		}
		if _, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: session, Kind: RegisterOperation, OperationID: "turn-2"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("failed item's operation register = %v, want nothing retained in storage", err)
		}
	})

	t.Run("failed admission drops the item and proceeds", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		prepared := validPrepared()
		prepared.Model = script.model
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)
		injected := false
		store.txHook = func(step string) error {
			if step == "insert_entry" && !injected && watch.settlements() >= 1 { // the first drained queued item's input publication fails
				injected = true
				return errors.New("injected admission failure")
			}
			return nil
		}

		if _, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived
		if _, err := submitText(t, h, session, "turn-2", MessageModeQueued, "q1"); err != nil {
			t.Fatalf("queued submit 1: %v", err)
		}
		if _, err := submitText(t, h, session, "turn-3", MessageModeQueued, "q2"); err != nil {
			t.Fatalf("queued submit 2: %v", err)
		}
		script.releaseGate()

		watch.next()
		watch.next()
		if rec := settledOperation(t, store, session, "turn-3"); rec.State.Status != OperationSuccess {
			t.Fatalf("next buffered message settled %+v, want q2 admitted and successful", rec.State)
		}
		if got := strings.Join(entryTexts(t, store, session), ","); got != "hello,q2" {
			t.Fatalf("committed inputs = %q, want the failed item gone without retry", got)
		}
	})

	t.Run("failed delivered operation proceeds to the next item", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript(
			agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()},  // turn-1
			agent.ModelSettlement{Disposition: agent.DispoFailure, Detail: "queued turn failed"}, // q1 fails
			agent.ModelSettlement{Disposition: agent.DispoReady, Output: completedOutputWith()},  // q2 succeeds
		)
		gate := make(chan struct{})
		script.gate = gate
		prepared := validPrepared()
		prepared.Model = script.model
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived // parked at the first model boundary: the queued items buffer before any terminal
		if _, err := submitText(t, h, session, "turn-2", MessageModeQueued, "q1"); err != nil {
			t.Fatalf("queued submit 1: %v", err)
		}
		if _, err := submitText(t, h, session, "turn-3", MessageModeQueued, "q2"); err != nil {
			t.Fatalf("queued submit 2: %v", err)
		}
		script.releaseGate()

		watch.next() // turn-1 succeeds
		watch.next() // the drain admits q1, whose Operation settles failure
		if rec := settledOperation(t, store, session, "turn-2"); rec.State.Status != OperationFailure {
			t.Fatalf("first drained turn settled %q, want failure", rec.State.Status)
		}
		watch.next() // the next buffered message proceeds
		if rec := settledOperation(t, store, session, "turn-3"); rec.State.Status != OperationSuccess {
			t.Fatalf("next buffered message settled %+v, want q2 admitted and successful", rec.State)
		}
		if got := strings.Join(entryTexts(t, store, session), ","); got != "hello,q1,q2" {
			t.Fatalf("committed inputs = %q, want both queued items delivered in order", got)
		}
	})
}

// TestFinalBoundarySerialization proves the coordinator linearizes the final
// model-result boundary against submissions: input submitted inside the model
// callback while the Operation is still current is never stranded — regular
// input continues the Operation through steering, queued input drains through
// ordinary admission after the terminal commit.
func TestFinalBoundarySerialization(t *testing.T) {
	t.Run("regular input at the boundary continues through steering", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		prepared := validPrepared()
		var (
			h       *Harness
			session string
			first   = true
		)
		inner := script.model
		script.model = func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
			if first { // submit at the model-result boundary, before the settlement returns
				first = false
				if _, err := submitText(t, h, session, "boundary", MessageModeRegular, "at-boundary"); err != nil {
					t.Errorf("boundary submit: %v", err)
				}
			}
			return inner(ctx, req, assemble)
		}
		prepared.Model = script.model
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session = createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		watch.next()
		if rec := settledOperation(t, store, session, "turn-1"); rec.State.Status != OperationSuccess {
			t.Fatalf("operation settled %q, want the boundary steering to continue it to success", rec.State.Status)
		}
		if texts := strings.Join(script.lastTexts(), "|"); texts != "system|hello|done|at-boundary" {
			t.Fatalf("final projection = %q, want the boundary steering drained in the same Operation", texts)
		}
		if got := strings.Join(entryTexts(t, store, session), ","); got != "hello,at-boundary" {
			t.Fatalf("committed inputs = %q, want the boundary input owned by the same Operation", got)
		}
	})

	t.Run("queued input at the boundary drains after the terminal", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		prepared := validPrepared()
		var (
			h       *Harness
			session string
			first   = true
		)
		inner := script.model
		script.model = func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
			if first { // defer one queued message at the final model-result boundary
				first = false
				if _, err := submitText(t, h, session, "turn-2", MessageModeQueued, "at-boundary"); err != nil {
					t.Errorf("boundary submit: %v", err)
				}
			}
			return inner(ctx, req, assemble)
		}
		prepared.Model = script.model
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session = createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		watch.next()
		watch.next()
		if rec := settledOperation(t, store, session, "turn-2"); rec.State.Status != OperationSuccess {
			t.Fatalf("drained turn settled %+v, want the boundary queued input admitted after the terminal", rec.State)
		}
		if got := strings.Join(entryTexts(t, store, session), ","); got != "hello,at-boundary" {
			t.Fatalf("committed inputs = %q, want the boundary queued input delivered", got)
		}
	})
}

// TestAgentTypeChangeDuringBlockedEffect proves the Agent-type row: the change
// during a running Operation affects later admission only, and the drained
// queued admission resolves the Session's then-current type.
func TestAgentTypeChangeDuringBlockedEffect(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	gate := make(chan struct{})
	script.gate = gate
	prepared := validPrepared()
	prepared.Model = script.model
	var (
		prepareMu   sync.Mutex
		prepareReqs []PreparationRequest
	)
	h, cancel := newCancelableHarness(t, store, prepared, func(_ context.Context, req PreparationRequest) (PreparedExecution, error) {
		prepareMu.Lock()
		prepareReqs = append(prepareReqs, req)
		prepareMu.Unlock()
		return prepared, nil
	})
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)

	if _, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	<-script.arrived
	if _, err := submitText(t, h, session, "turn-2", MessageModeQueued, "q1"); err != nil {
		t.Fatalf("queued submit: %v", err)
	}
	if _, err := h.ChangeAgentType(context.Background(), session, "reviewer"); err != nil {
		t.Fatalf("ChangeAgentType during the blocked effect: %v", err)
	}

	script.releaseGate()
	watch.next()
	watch.next()
	if rec := settledOperation(t, store, session, "turn-2"); rec.State.Status != OperationSuccess {
		t.Fatalf("drained turn settled %+v, want success", rec.State)
	}
	prepareMu.Lock()
	defer prepareMu.Unlock()
	if len(prepareReqs) != 2 {
		t.Fatalf("preparation calls = %d, want the drained admission to prepare again", len(prepareReqs))
	}
	if prepareReqs[0].Session.AgentType != "coder" || prepareReqs[1].Session.AgentType != "reviewer" {
		t.Fatalf("preparation agent types = %q then %q, want the capture-time value then the changed one",
			prepareReqs[0].Session.AgentType, prepareReqs[1].Session.AgentType)
	}
}

// TestWaitConvergence proves the Harness-lifetime row: Wait returns only after
// cancellation has closed admission and every blocked preparation, execution,
// and required settlement has converged, and a first storage failure that
// stopped admitted work is retained for Wait while later failures never
// replace it.
func TestWaitConvergence(t *testing.T) {
	t.Run("blocked model converges after cancellation", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		prepared := validPrepared()
		prepared.Model = script.model
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived
		waited := make(chan error, 1)
		go func() { waited <- h.Wait(context.Background()) }() // Wait begins before cancellation

		cancel()             // Harness loss closes admission and ends the run
		script.releaseGate() // the already-started callback publishes, then the run interrupts
		if err := <-waited; err != nil {
			t.Fatalf("Wait: %v", err)
		}
		watch.next()
		if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationInterruption {
			t.Fatalf("converged operation = %q, want interruption", rec.State.Status)
		}
	})

	t.Run("blocked preparation converges without publication", func(t *testing.T) {
		store := emptyStore(t)
		hctx, cancel := context.WithCancel(context.Background())
		gate := make(chan struct{})
		arrived := make(chan struct{}, 1)
		h, err := New(hctx, Dependencies{Storage: store, Prepare: func(prepCtx context.Context, _ PreparationRequest) (PreparedExecution, error) {
			arrived <- struct{}{}
			<-gate
			return PreparedExecution{}, prepCtx.Err() // the dead harness context fails the parked preparation
		}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		session := createSession(t, h)
		admitted := make(chan error, 1)
		go func() {
			_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
			admitted <- err
		}()
		<-arrived

		waited := make(chan error, 1)
		go func() { waited <- h.Wait(context.Background()) }()
		cancel()    // the Harness context dies while preparation is parked
		close(gate) // the parked preparation fails on the dead context

		if err := <-admitted; err == nil {
			t.Fatalf("preparation over a lost Harness context succeeded, want an error")
		}
		if err := <-waited; err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if _, regs := storedSessionState(store, session); len(regs) != 1 {
			t.Fatalf("converged store holds %d registers, want the session register only", len(regs))
		}
	})

	t.Run("blocked tool settles before Wait returns", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript(agent.ModelSettlement{
			Disposition: agent.DispoReady,
			Output:      completedOutputWith(testToolCall("call-1")),
		})
		modelGate := make(chan struct{})
		script.gate = modelGate
		toolGate := make(chan struct{})
		toolArrived := make(chan struct{}, 1)
		prepared := validPrepared()
		prepared.Model = script.model
		prepared.Tool = func(context.Context, model.ToolCall) PreparedTool {
			toolArrived <- struct{}{}
			<-toolGate
			return PreparedTool{Execute: func(context.Context) model.ToolResult {
				return model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}
			}}
		}
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		session := createSession(t, h)
		watch := watchSettlements(store)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived
		script.releaseGate() // the completed output with one call dispatches the tool
		<-toolArrived        // the tool callback is parked

		waited := make(chan error, 1)
		go func() { waited <- h.Wait(context.Background()) }()
		cancel()
		close(toolGate) // the produced result publishes, the run interrupts

		if err := <-waited; err != nil {
			t.Fatalf("Wait: %v", err)
		}
		watch.next()
		rec := settledOperation(t, store, session, "op-1")
		if rec.State.Status != OperationInterruption || len(rec.State.PendingToolCalls) != 0 {
			t.Fatalf("converged operation = %+v, want interruption with the produced call settled", rec.State)
		}
	})

	t.Run("the first storage failure that stopped admitted work is retained", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		prepared := validPrepared()
		prepared.Model = script.model
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		replaces := 0
		firstErr := fmt.Errorf("first storage failure: %w", ErrStorage)
		laterErr := fmt.Errorf("later storage failure: %w", ErrStorage)
		failed := make(chan struct{}, 1)
		store.txHook = func(step string) error {
			if step != "replace_register" {
				return nil
			}
			replaces++
			switch {
			case replaces == 1: // session A's admission publication
				return nil
			case replaces == 2: // the first execution's model-effect intent fails
				failed <- struct{}{}
				return firstErr
			default:
				return laterErr // every later publication fails with the later error
			}
		}
		sessionA := createSession(t, h)
		if _, err := submitText(t, h, sessionA, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-failed // the admitted work stopped at the intent publication

		sessionB := createSession(t, h)
		if _, err := submitText(t, h, sessionB, "op-1", MessageModeRegular, "hello"); err == nil {
			t.Fatalf("later storage failure did not surface from Submit")
		}

		cancel()
		if err := h.Wait(context.Background()); !errors.Is(err, firstErr) {
			t.Fatalf("Wait = %v, want the first retained storage error (later: %v)", err, laterErr)
		}
		if rec := settledOperation(t, store, sessionA, "op-1"); rec.State.Status != OperationRunning {
			t.Fatalf("stopped operation = %q, want the last committed running state for recovery", rec.State.Status)
		}
	})
}

// TestWaitDiscardsBuffersOfStuckSessions proves the lifetime row's loss axis
// on a Session whose admitted work stopped at a storage publication failure:
// the Operation stays running with both buffers present, and cancellation +
// Wait clears every coordinator's FIFOs while returning the retained storage
// error.
func TestWaitDiscardsBuffersOfStuckSessions(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	prepared := validPrepared()
	prepared.Model = script.model
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	session := createSession(t, h)
	replaces := 0
	firstErr := fmt.Errorf("first storage failure: %w", ErrStorage)
	failed := make(chan struct{}, 1)
	store.txHook = func(step string) error {
		if step != "replace_register" {
			return nil
		}
		replaces++
		switch {
		case replaces == 1: // the admission publication
			return nil
		case replaces == 2: // the execution's model-effect intent fails to publish
			failed <- struct{}{}
			return firstErr
		default:
			return fmt.Errorf("later storage failure: %w", ErrStorage)
		}
	}

	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	<-failed // the admitted work stopped; the Operation stays running for recovery
	c, err := h.coordinatorFor(context.Background(), session)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	// The stuck Operation keeps the Session active: both submits buffer.
	if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer"); err != nil {
		t.Fatalf("steering submit: %v", err)
	}
	if _, err := submitText(t, h, session, "op-3", MessageModeQueued, "queued-1"); err != nil {
		t.Fatalf("queued submit: %v", err)
	}
	c.mu.Lock()
	buffered := len(c.steering) + len(c.queued)
	c.mu.Unlock()
	if buffered != 2 {
		t.Fatalf("buffered items = %d, want both FIFOs present before cancellation", buffered)
	}

	cancel()
	if err := h.Wait(context.Background()); !errors.Is(err, firstErr) {
		t.Fatalf("Wait = %v, want the retained storage error", err)
	}
	c.mu.Lock()
	left := len(c.steering) + len(c.queued)
	c.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d buffered items survived cancellation + Wait, want both FIFOs cleared", left)
	}
	if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationRunning {
		t.Fatalf("stopped operation = %q, want the last committed running state for recovery", rec.State.Status)
	}
}

// TestHarnessLossDiscardsBuffers proves the buffer-lifetime row's loss axis:
// cancellation closes admission, and after the in-flight execution converges
// both buffers are discarded without delivery attempts.
func TestHarnessLossDiscardsBuffers(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	gate := make(chan struct{})
	script.gate = gate
	prepared := validPrepared()
	prepared.Model = script.model
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	session := createSession(t, h)
	watch := watchSettlements(store)

	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	<-script.arrived
	if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1"); err != nil {
		t.Fatalf("steering submit: %v", err)
	}
	if _, err := submitText(t, h, session, "op-3", MessageModeQueued, "q1"); err != nil {
		t.Fatalf("queued submit: %v", err)
	}

	cancel() // Harness loss
	if _, err := submitText(t, h, session, "op-4", MessageModeRegular, "late"); !errors.Is(err, context.Canceled) {
		t.Fatalf("submit after Harness loss = %v, want a context error", err)
	}
	script.releaseGate() // the produced result publishes, the run interrupts, the buffers discard
	if err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	watch.next()
	if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationInterruption {
		t.Fatalf("converged operation = %q, want interruption", rec.State.Status)
	}
	if got := strings.Join(entryTexts(t, store, session), ","); got != "hello" {
		t.Fatalf("committed inputs = %q, want both buffers discarded without delivery", got)
	}
}

// TestSubmitCancellationGate proves the routing decision under the
// coordinator mutex is the cancellation publication gate: a caller or
// Harness cancellation observed there publishes nothing — no buffer item, no
// admission — and an enqueue that wins the gate still delivers after the
// caller's later cancellation.
func TestSubmitCancellationGate(t *testing.T) {
	t.Run("cancellation observed at the gate publishes nothing", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		prepared := validPrepared()
		prepared.Model = script.model
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived // the Session is active

		c, err := h.coordinatorFor(context.Background(), session)
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
		submitCtx, cancelSubmit := context.WithCancel(context.Background())
		cancelSubmit() // the caller context dies before the gate resolves
		done := make(chan SubmitResult, 1)
		errCh := make(chan error, 1)
		c.mu.Lock() // hold the routing decision open so the submit parks on it
		go func() {
			res, err := h.Submit(submitCtx, SubmitRequest{SessionID: session, OperationID: "op-2", Origin: InputOriginUser, Content: admissionContent("late"), Mode: MessageModeRegular})
			if err != nil {
				errCh <- err
				return
			}
			done <- res
		}()
		c.mu.Unlock() // the submit now reaches the gate with a dead caller context

		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("gated submit = %v, want a context error", err)
			}
		case res := <-done:
			t.Fatalf("gated submit returned %+v, want a context error", res)
		}
		c.mu.Lock()
		buffered := len(c.steering) + len(c.queued)
		c.mu.Unlock()
		if buffered != 0 {
			t.Fatalf("%d items buffered over a canceled context, want nothing published", buffered)
		}
		script.releaseGate()
	})

	t.Run("an enqueue that wins the gate delivers after later caller cancellation", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		prepared := validPrepared()
		prepared.Model = script.model
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)

		if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
			t.Fatalf("first submit: %v", err)
		}
		<-script.arrived // the Session is active
		submitCtx, cancelSubmit := context.WithCancel(context.Background())
		res, err := h.Submit(submitCtx, SubmitRequest{SessionID: session, OperationID: "op-2", Origin: InputOriginUser, Content: admissionContent("late"), Mode: MessageModeRegular})
		if err != nil || res.Disposition != DispositionSteering {
			t.Fatalf("steering submit = %+v err %v, want the enqueue to win", res, err)
		}
		cancelSubmit() // later caller cancellation does not own the item

		script.releaseGate()
		<-script.arrived
		if got := strings.Join(script.lastTexts(), "|"); !strings.Contains(got, "late") {
			t.Fatalf("continuation projection = %q, want the item delivered despite the later cancellation", got)
		}
	})
}

// TestDrainRetiresItsOwnRunSlot proves the empty-drain slot retirement is
// atomic with the FIFO scan: an empty drain clears its own run slot in the
// same critical section — leaving no window where the Session looks active
// without a pending drain — and never clears a replacement run installed by
// another execution.
func TestDrainRetiresItsOwnRunSlot(t *testing.T) {
	// newIdleCoordinator returns one coordinator whose admitted Operation is
	// already settled: the idle, terminal-committed state an empty drain sees.
	newIdleCoordinator := func(t *testing.T) (*Harness, *coordinator) {
		t.Helper()
		store := emptyStore(t)
		h := newTestHarness(t, store, func(context.Context, PreparationRequest) (PreparedExecution, error) {
			return validPrepared(), nil
		})
		session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if _, disposition := mustAdmitWithoutExecution(t, h, session.Identity.SessionID, testOpID, admissionContent("hello")); disposition != DispositionAdmitted {
			t.Fatalf("disposition %q, want admitted", disposition)
		}
		c, err := h.coordinatorFor(context.Background(), session.Identity.SessionID)
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
		if _, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationSuccess}); err != nil {
			t.Fatalf("settle the admitted operation: %v", err)
		}
		return h, c
	}

	t.Run("an empty drain retires its own run slot", func(t *testing.T) {
		h, c := newIdleCoordinator(t)
		run := &activeExecution{done: make(chan struct{})}
		c.mu.Lock()
		c.run = run
		c.mu.Unlock()

		h.drainBuffers(c, run) // synchronous: the empty FIFOs retire the slot in the scan's critical section

		c.mu.Lock()
		got := c.run
		c.mu.Unlock()
		if got != nil {
			t.Fatalf("empty drain left a run slot installed: %+v", got)
		}
	})

	t.Run("an empty drain never clears a replacement run", func(t *testing.T) {
		h, c := newIdleCoordinator(t)
		oldRun := &activeExecution{done: make(chan struct{})}
		replacement := &activeExecution{done: make(chan struct{})}
		c.mu.Lock()
		c.run = replacement // the current slot belongs to another execution
		c.mu.Unlock()

		h.drainBuffers(c, oldRun)

		c.mu.Lock()
		got := c.run
		c.mu.Unlock()
		if got != replacement {
			t.Fatalf("empty drain cleared a replacement run: got %+v", got)
		}
	})
}

// TestSessionsExecuteConcurrently proves the coordination row's concurrency
// axis: two Sessions execute at the same time — the second Session's model
// boundary arrives and settles while the first Session's execution stays
// parked — with no global execution lane.
func TestSessionsExecuteConcurrently(t *testing.T) {
	store := emptyStore(t)
	first := newModelScript()
	firstGate := make(chan struct{})
	first.gate = firstGate
	second := newModelScript()
	prepared := validPrepared()
	firstModel, secondModel := first.model, second.model
	prepared.Model = func(ctx context.Context, req model.Request, assemble agent.AssemblyCallback) (agent.ModelSettlement, error) {
		texts := textsOf(req)
		if texts[len(texts)-1] == "first" {
			return firstModel(ctx, req, assemble)
		}
		return secondModel(ctx, req, assemble)
	}
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	sessionA := createSession(t, h)
	sessionB := createSession(t, h)
	watch := watchSettlements(store)

	if _, err := submitText(t, h, sessionA, "op-a", MessageModeRegular, "first"); err != nil {
		t.Fatalf("first session submit: %v", err)
	}
	<-first.arrived // session A parked at its model boundary

	if _, err := submitText(t, h, sessionB, "op-b", MessageModeRegular, "second"); err != nil {
		t.Fatalf("second session submit: %v", err)
	}
	<-second.arrived // session B reached its model boundary while A stays parked
	watch.next()
	if rec := settledOperation(t, store, sessionB, "op-b"); rec.State.Status != OperationSuccess {
		t.Fatalf("second session settled %q while the first is parked, want concurrent progress", rec.State.Status)
	}

	first.releaseGate()
	watch.next()
	if rec := settledOperation(t, store, sessionA, "op-a"); rec.State.Status != OperationSuccess {
		t.Fatalf("first session settled %q, want success after its release", rec.State.Status)
	}
}

// TestSnapshotCoordinatorsReleasesRegistryMutex proves the snapshot
// postcondition deterministically: snapshotCoordinators returns with the
// registry mutex released — observed with TryLock — including while a
// coordinator mutex is held across the snapshot call, which also proves the
// snapshot never takes the coordinator mutex.
func TestSnapshotCoordinatorsReleasesRegistryMutex(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	c, err := h.coordinatorFor(context.Background(), session.Identity.SessionID)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}

	c.mu.Lock() // a coordinator mutex held across the snapshot call
	got := h.snapshotCoordinators()
	c.mu.Unlock()
	if len(got) != 1 || got[0] != c {
		t.Fatalf("snapshot = %d coordinators, want exactly the materialized one", len(got))
	}
	if !h.mu.TryLock() { // the registry mutex is released once the snapshot returns
		t.Fatalf("snapshotCoordinators retained the registry mutex")
	}
	h.mu.Unlock()
}
