package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// modelAttempt is one scripted physical attempt: the stream of the accepted
// attempt, or its pre-acceptance failure.
type modelAttempt struct {
	stream model.Stream
	err    error
}

// turn scripts one accepted stream assembling to the fixture completed turn
// with the given calls.
func turn(calls ...model.ToolCall) modelAttempt {
	return modelAttempt{stream: completedTurnStream(calls...)}
}

// partialTurn scripts one accepted stream assembling to the fixture errored
// partial: text "partial" with usage, then the read failure.
func partialTurn() modelAttempt {
	return modelAttempt{stream: erroredTurnStream()}
}

// payloadlessTurn scripts one accepted stream whose assembly errors with no
// retained payload.
func payloadlessTurn() modelAttempt {
	return modelAttempt{stream: failStream(errors.New("provider failure"))}
}

// fail scripts one pre-acceptance attempt failure; its text becomes the
// settled terminal detail.
func fail(detail string) modelAttempt {
	return modelAttempt{err: errors.New(detail)}
}

// modelScript is the scripted physical model request of the coordination
// fixtures: it records every projected request, signals each arrival,
// optionally parks every invocation until its gate closes, and answers from a
// scripted attempt list.
type modelScript struct {
	mu       sync.Mutex
	requests []model.Request
	arrived  chan struct{}
	gate     chan struct{}
	attempts []modelAttempt
	model    func(context.Context, model.Request) (model.Stream, error)
}

func newModelScript(attempts ...modelAttempt) *modelScript {
	s := &modelScript{arrived: make(chan struct{}, 16), attempts: attempts}
	s.model = s.invoke
	return s
}

func (s *modelScript) invoke(_ context.Context, req model.Request) (model.Stream, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	gate := s.gate
	s.mu.Unlock()
	s.arrived <- struct{}{}
	if gate != nil {
		<-gate
	}
	var next modelAttempt
	if len(s.attempts) > 0 {
		next = s.attempts[0]
		s.attempts = s.attempts[1:]
	} else {
		next = turn()
	}
	return next.stream, next.err
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
func newCancelableHarness(t *testing.T, store Storage, prepared PreparedExecution, prepare func(context.Context, PreparationRequest) (PreparedExecution, error)) (*Harness, context.CancelFunc) {
	t.Helper()
	if prepare == nil {
		prepare = func(context.Context, PreparationRequest) (PreparedExecution, error) { return prepared, nil }
	}
	hctx, cancel := context.WithCancel(context.Background())
	h, err := New(hctx, Dependencies{Storage: store, Prepare: prepare})
	testassert.NoError(t, err)
	return h, cancel
}

func createSession(t *testing.T, h *Harness) string {
	t.Helper()
	session, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
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
	testassert.NoError(t, err)
	out := []string{}
	for _, entry := range graph.Entries {
		if entry.Input != nil {
			out = append(out, entry.Input.Content[0].Text)
		}
	}
	return out
}

// steeringInputOwner returns the owning Operation and origin of one committed
// input entry carrying the given text, read from the validated graph. Callers
// await their explicit successor terminal and Session convergence first: it is
// an ordinary raw graph reader.
func steeringInputOwner(t *testing.T, store Storage, sessionID, text string) (string, InputOrigin) {
	t.Helper()
	graph, err := validateSessionGraph(context.Background(), store, sessionID)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		if entry.Input != nil && len(entry.Input.Content) > 0 && entry.Input.Content[0].Text == text {
			return entry.Envelope.OperationID, entry.Input.Origin
		}
	}
	t.Fatalf("session %q carries no committed input %q", sessionID, text)
	return "", ""
}

// settledOperation reads one operation register straight from durable storage:
// the store's mutex synchronizes the read with the committing transaction, so
// a settlement-watch barrier followed by this read observes terminal state
// without polling.
func settledOperation(t *testing.T, store *graphStorage, sessionID, operationID string) OperationRecord {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID})
	testassert.Require(t, err == nil, "read operation register %q: %v", operationID, err)
	rec, err := decodeOperationRegister(reg)
	testassert.Require(t, err == nil, "decode operation register %q: %v", operationID, err)
	return rec
}

// awaitOperationTerminal polls one operation register until it settles
// terminally with the wanted status, tolerating the not-yet-admitted window.
func awaitOperationTerminal(t *testing.T, store *graphStorage, sessionID, operationID string, want OperationState) OperationRecord {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		reg, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID})
		if err == nil {
			rec, derr := decodeOperationRegister(reg)
			testassert.Require(t, derr == nil, "decode operation register %q: %v", operationID, derr)
			if rec.State.Terminal != nil {
				testassert.Require(t, rec.State.Status == want, "operation %q status = %s, want %s", operationID, rec.State.Status, want)
				return rec
			}
		} else if !errors.Is(err, ErrNotFound) {
			t.Fatalf("read operation register %q: %v", operationID, err)
		}
		testassert.Require(t, !time.Now().After(deadline), "operation %q never settled as %s", operationID, want)
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSubmitRoutesIdleAndActive proves the routing row: idle regular and idle
// queued input use normal admission, active regular input enters steering and
// active queued input enters queued without preparing or publishing, and an
// existing identity resolves before routing.
func TestSubmitRoutesIdleAndActive(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript()
	script.gate = make(chan struct{})
	prepared := modelPrepared(script.model)
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)

	res, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
	testassert.Require(t, err == nil && res.Disposition == DispositionAdmitted && res.Operation != nil && res.Operation.State.Status == OperationRunning, "idle regular submit = %+v err %v, want admitted with the record", res, err)
	<-script.arrived // the execution reached its parked model boundary: the Session is active

	steered, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1")
	testassert.Require(t, err == nil && steered.Disposition == DispositionSteering && steered.Operation == nil, "active regular submit = %+v err %v, want steering without a record", steered, err)
	queued, err := submitText(t, h, session, "op-3", MessageModeQueued, "q1")
	testassert.Require(t, err == nil && queued.Disposition == DispositionQueued && queued.Operation == nil, "active queued submit = %+v err %v, want queued without a record", queued, err)
	existing, err := submitText(t, h, session, "op-1", MessageModeRegular, "retry")
	testassert.Require(t, err == nil && existing.Disposition == DispositionExisting && existing.Operation != nil && existing.Operation.Admission.OperationID == "op-1", "active existing submit = %+v err %v, want the first record before routing", existing, err)
	{
		entries, regs := storedSessionState(store, session)
		testassert.Require(t, len(entries) == 1 && len(regs) == 2, "buffered submits published entries %v registers %v", entries, regs)
	}

	{
		_, err := submitText(t, h, session, "op-4", MessageMode("urgent"), "x")
		testassert.Require(t, errors.Is(err, ErrInvalid), "unknown mode = %v, want ErrInvalid", err)
	}

	// Idle queued input uses normal admission: a fresh idle Session admits it.
	other := createSession(t, h)
	idleQueued, err := submitText(t, h, other, "idle-queued", MessageModeQueued, "queued-idle")
	testassert.Require(t, err == nil && idleQueued.Disposition == DispositionAdmitted && idleQueued.Operation != nil, "idle queued submit = %+v err %v, want normal admission", idleQueued, err)
}

// TestSteeringAdmitsHeadsInFIFOOrder proves the steering row: while an
// Operation is active, every delivered head is admitted as its own caller-ID
// Operation in FIFO order at the conversation boundary — the predecessor
// settles quiet success and each head's own Operation makes the next first
// request carrying it — a head buffered from the predecessor's Open never
// preempts its first receiving request, one call ID may recur across
// successor Operations, and a queued follower trails the steering heads.
func TestSteeringAdmitsHeadsInFIFOOrder(t *testing.T) {
	store := emptyStore(t)
	script := newModelScript(turn(), turn(testToolCall("call-1")), turn(testToolCall("call-1")), turn())
	gate := make(chan struct{})
	script.gate = gate
	openerArrived := make(chan struct{})
	openerGate := make(chan struct{})
	var once sync.Once
	exec := validExecution()
	exec.Model = script.model
	exec.Tool = func(_ context.Context, call model.ToolCall) PreparedTool {
		return PreparedTool{Permissions: fixturePermission, Immediate: &ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ok"}}}
	}
	prepared := PreparedExecution{
		Capture: testCapture(),
		Open: func(context.Context, OperationAdmission) (Execution, error) {
			once.Do(func() { // only the predecessor's Open parks before its first request
				close(openerArrived)
				<-openerGate
			})
			return exec, nil
		},
	}
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)

	{
		_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	receiveBounded(t, openerArrived, "the predecessor's Open")
	{
		_, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1")
		testassert.NoError(t, err)
	}
	close(openerGate) // the next head is buffered from the predecessor's Open, before its first request
	receiveBounded(t, script.arrived, "op-1's first model request")
	{
		_, err := h.Submit(context.Background(), SubmitRequest{
			SessionID: session, OperationID: "op-3", Origin: InputOriginRuntime,
			Content: admissionContent("s2"), Mode: MessageModeRegular,
		})
		testassert.NoError(t, err)
	}
	{
		_, err := h.Submit(context.Background(), SubmitRequest{
			SessionID: session, OperationID: "op-4", Origin: InputOriginPlugin,
			Content: admissionContent("q1"), Mode: MessageModeQueued,
		})
		testassert.NoError(t, err)
	}

	script.releaseGate() // the first model result settles with steering waiting: the boundary exits through the token
	for _, op := range []string{"op-1", "op-2", "op-3", "op-4"} {
		receiveBounded(t, watch.seen, "the split Operations' settlements")
		if rec := settledOperation(t, store, session, op); rec.State.Status != OperationSuccess {
			t.Fatalf("operation %q settled %q, want success", op, rec.State.Status)
		} else if rec.State.Terminal == nil || rec.State.Terminal.Detail != "" {
			t.Fatalf("operation %q terminal = %+v, want quiet success with empty detail", op, rec.State.Terminal)
		}
	}
	reqs := script.seen()
	testassert.Equal(t, len(reqs), 5)
	{
		texts := strings.Join(textsOf(reqs[0]), "|")
		testassert.Equal(t, texts, "system|hello")
	}
	{
		texts := strings.Join(textsOf(reqs[1]), "|")
		testassert.Equal(t, texts, "system|hello|done|s1")
	}
	{
		texts := strings.Join(textsOf(reqs[2]), "|")
		testassert.Equal(t, texts, "system|hello|done|s1|done|ok|s2")
	}
	{
		texts := strings.Join(textsOf(reqs[3]), "|")
		testassert.Equal(t, texts, "system|hello|done|s1|done|ok|s2|done|ok")
	}
	{
		texts := strings.Join(textsOf(reqs[4]), "|")
		testassert.Equal(t, texts, "system|hello|done|s1|done|ok|s2|done|ok|done|q1")
	}
	awaitOperationTerminal(t, store, session, "op-4", OperationSuccess) // the explicit successor rendezvous
	awaitHarnessQuiet(t, h, session)
	{
		got := strings.Join(entryTexts(t, store, session), ",")
		testassert.Equal(t, got, "hello,s1,s2,q1")
	}
	{
		owner, origin := steeringInputOwner(t, store, session, "s1")
		testassert.Require(t, owner == "op-2" && origin == InputOriginUser, "steering input s1 owner/origin = %q/%v, want op-2/user", owner, origin)
	}
	{
		owner, origin := steeringInputOwner(t, store, session, "s2")
		testassert.Require(t, owner == "op-3" && origin == InputOriginRuntime, "steering input s2 owner/origin = %q/%v, want op-3/runtime", owner, origin)
	}
	{
		owner, origin := steeringInputOwner(t, store, session, "q1")
		testassert.Require(t, owner == "op-4" && origin == InputOriginPlugin, "queued input q1 owner/origin = %q/%v, want op-4/plugin", owner, origin)
	}
	graph, err := validateFixture(t, store, session)
	testassert.NoError(t, err)
	for _, entry := range graph.Entries {
		testassert.Require(t, entry.Signal == nil, "entry %s added a signal behind a boundary exit", entry.Envelope.ID)
	}
}

// TestSteeringContinuationAcrossOutputShapes proves the boundary decision is
// independent of output shape: a completed output without calls, a completed
// output with calls, and an errored output retaining a payload all hand the
// Operation off at its next boundary so the steering arrives through its own
// successor admission — and an errored output without a payload settles
// failure while its steering drains through ordinary admission.
func TestSteeringContinuationAcrossOutputShapes(t *testing.T) {
	cases := []struct {
		name    string
		attempt modelAttempt
		wantEnd OperationState
	}{
		{"completed output without calls", turn(), OperationSuccess},
		{"completed output with calls", turn(testToolCall("call-1")), OperationSuccess},
		{"errored output with payload", partialTurn(), OperationSuccess},
		{"errored output without payload", payloadlessTurn(), OperationFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := emptyStore(t)
			script := newModelScript(tc.attempt)
			gate := make(chan struct{})
			script.gate = gate
			prepared := modelPrepared(script.model)
			h, cancel := newCancelableHarness(t, store, prepared, nil)
			defer cancel()
			session := createSession(t, h)
			watch := watchSettlements(store)

			{
				_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
				testassert.NoError(t, err)
			}
			receiveBounded(t, script.arrived, "op-1's first model request")
			{
				_, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer")
				testassert.NoError(t, err)
			}
			script.releaseGate()

			receiveBounded(t, watch.seen, "op-1's terminal settlement")
			{
				rec := settledOperation(t, store, session, "op-1")
				testassert.Equal(t, rec.State.Status, tc.wantEnd)
			}
			receiveBounded(t, watch.seen, "the steering successor's settlement")
			{
				texts := strings.Join(script.lastTexts(), "|")
				testassert.Require(t, strings.Contains(texts, "steer"), "delivered projection = %q, want the steering carried by its own Operation's request", texts)
			}
			awaitOperationTerminal(t, store, session, "op-2", OperationSuccess) // the explicit successor rendezvous
			awaitHarnessQuiet(t, h, session)
			{
				owner, _ := steeringInputOwner(t, store, session, "steer")
				testassert.Equal(t, owner, "op-2")
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
	prepared := modelPrepared(script.model)
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	session := createSession(t, h)
	watch := watchSettlements(store)

	{
		_, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-script.arrived
	{
		_, err := submitText(t, h, session, "turn-2", MessageModeQueued, "q1")
		testassert.NoError(t, err)
	}
	{
		_, err := submitText(t, h, session, "turn-3", MessageModeQueued, "q2")
		testassert.NoError(t, err)
	}

	script.releaseGate() //turn-1 settles; the drain admits q1, then q2 after its terminal
	watch.next()
	watch.next()
	{
		rec := settledOperation(t, store, session, "turn-2")
		testassert.Equal(t, rec.State.Status, OperationSuccess)
	}
	watch.next()
	{
		rec := settledOperation(t, store, session, "turn-3")
		testassert.Equal(t, rec.State.Status, OperationSuccess)
	}
	{
		texts := strings.Join(script.lastTexts(), "|")
		testassert.Equal(t, texts, "system|hello|done|q1|done|q2")
	}
	{
		got := strings.Join(entryTexts(t, store, session), ",")
		testassert.Equal(t, got, "hello,q1,q2")
	}
}

// TestBufferedItemFailureIsFinal proves the buffer-lifetime row: a failed
// delivery attempt — failed preparation, failed admission, or a failed
// delivered Operation — is final for the item, the drain advances to the next
// buffered message, and nothing is retained or retried.
func TestBufferedItemFailureIsFinal(t *testing.T) {
	for _, mode := range []MessageMode{MessageModeQueued, MessageModeRegular} {
		t.Run(string(mode), func(t *testing.T) {
			t.Run("failed preparation drops the item and proceeds", func(t *testing.T) {
				store := emptyStore(t)
				script := newModelScript()
				gate := make(chan struct{})
				script.gate = gate
				prepared := modelPrepared(script.model)
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

				{
					_, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello")
					testassert.NoError(t, err)
				}
				<-script.arrived
				{
					_, err := submitText(t, h, session, "turn-2", mode, "q1")
					testassert.NoError(t, err)
				}
				{
					_, err := submitText(t, h, session, "turn-3", mode, "q2")
					testassert.NoError(t, err)
				}
				script.releaseGate()

				watch.next()
				watch.next()
				{
					rec := settledOperation(t, store, session, "turn-3")
					testassert.Require(t, rec.State.Status == OperationSuccess, "next buffered message settled %+v, want q2 admitted and successful", rec.State)
				}
				testassert.Equal(t, calls, 3)
				{
					got := strings.Join(entryTexts(t, store, session), ",")
					testassert.Equal(t, got, "hello,q2")
				}
				{
					_, err := store.ReadRegister(context.Background(), RegisterKey{SessionID: session, Kind: RegisterOperation, OperationID: "turn-2"})
					testassert.Require(t, errors.Is(err, ErrNotFound), "failed item's operation register = %v, want nothing retained in storage", err)
				}
			})

			t.Run("failed admission drops the item and proceeds", func(t *testing.T) {
				store := emptyStore(t)
				script := newModelScript()
				gate := make(chan struct{})
				script.gate = gate
				prepared := modelPrepared(script.model)
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

				{
					_, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello")
					testassert.NoError(t, err)
				}
				<-script.arrived
				{
					_, err := submitText(t, h, session, "turn-2", mode, "q1")
					testassert.NoError(t, err)
				}
				{
					_, err := submitText(t, h, session, "turn-3", mode, "q2")
					testassert.NoError(t, err)
				}
				script.releaseGate()

				watch.next()
				watch.next()
				{
					rec := settledOperation(t, store, session, "turn-3")
					testassert.Require(t, rec.State.Status == OperationSuccess, "next buffered message settled %+v, want q2 admitted and successful", rec.State)
				}
				{
					got := strings.Join(entryTexts(t, store, session), ",")
					testassert.Equal(t, got, "hello,q2")
				}
			})

			t.Run("failed delivered operation proceeds to the next item", func(t *testing.T) {
				store := emptyStore(t)
				script := newModelScript(
					turn(),                     // turn-1
					fail("queued turn failed"), // q1 fails
					turn(),                     // q2 succeeds
				)
				gate := make(chan struct{})
				script.gate = gate
				prepared := modelPrepared(script.model)
				h, cancel := newCancelableHarness(t, store, prepared, nil)
				defer cancel()
				session := createSession(t, h)
				watch := watchSettlements(store)

				{
					_, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello")
					testassert.NoError(t, err)
				}
				<-script.arrived // parked at the first model boundary: the queued items buffer before any terminal
				{
					_, err := submitText(t, h, session, "turn-2", mode, "q1")
					testassert.NoError(t, err)
				}
				{
					_, err := submitText(t, h, session, "turn-3", mode, "q2")
					testassert.NoError(t, err)
				}
				script.releaseGate()

				watch.next() // turn-1 succeeds
				watch.next() // the drain admits q1, whose Operation settles failure
				{
					rec := settledOperation(t, store, session, "turn-2")
					testassert.Equal(t, rec.State.Status, OperationFailure)
				}
				watch.next() // the next buffered message proceeds
				{
					rec := settledOperation(t, store, session, "turn-3")
					testassert.Require(t, rec.State.Status == OperationSuccess, "next buffered message settled %+v, want q2 admitted and successful", rec.State)
				}
				{
					got := strings.Join(entryTexts(t, store, session), ",")
					testassert.Equal(t, got, "hello,q1,q2")
				}
			})
		})
	}
}

// TestFinalBoundarySerialization proves the coordinator linearizes the final
// model-result boundary against submissions: regular input submitted inside
// the model callback while the Operation is still current is never stranded —
// it exits at the natural-success boundary and arrives through its own
// successor admission — and queued input drains through ordinary admission
// after the terminal commit.
func TestFinalBoundarySerialization(t *testing.T) {
	t.Run("regular input at the boundary arrives through its own Operation", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		var (
			h       *Harness
			session string
			first   = true
		)
		inner := script.model
		script.model = func(ctx context.Context, req model.Request) (model.Stream, error) {
			if first { // submit at the model-result boundary, before the settlement returns
				first = false
				{
					_, err := submitText(t, h, session, "boundary", MessageModeRegular, "at-boundary")
					testassert.Check(t, err == nil, "boundary submit: %v", err)
				}
			}
			return inner(ctx, req)
		}
		prepared := modelPrepared(script.model)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session = createSession(t, h)
		watch := watchSettlements(store)

		{
			_, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		receiveBounded(t, watch.seen, "turn-1's quiet success settlement") // the natural-success boundary exit
		receiveBounded(t, watch.seen, "the boundary input's own settlement")
		{
			rec := settledOperation(t, store, session, "turn-1")
			testassert.Equal(t, rec.State.Status, OperationSuccess)
		}
		{
			rec := settledOperation(t, store, session, "boundary")
			testassert.Equal(t, rec.State.Status, OperationSuccess)
		}
		{
			texts := strings.Join(script.lastTexts(), "|")
			testassert.Equal(t, texts, "system|hello|done|at-boundary")
		}
		awaitHarnessQuiet(t, h, session) // raw graph readers await Session convergence after the successor's terminal
		{
			got := strings.Join(entryTexts(t, store, session), ",")
			testassert.Equal(t, got, "hello,at-boundary")
		}
		{
			owner, _ := steeringInputOwner(t, store, session, "at-boundary")
			testassert.Equal(t, owner, "boundary")
		}
	})

	t.Run("queued input at the boundary drains after the terminal", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		var (
			h       *Harness
			session string
			first   = true
		)
		inner := script.model
		script.model = func(ctx context.Context, req model.Request) (model.Stream, error) {
			if first { // defer one queued message at the final model-result boundary
				first = false
				{
					_, err := submitText(t, h, session, "turn-2", MessageModeQueued, "at-boundary")
					testassert.Check(t, err == nil, "boundary submit: %v", err)
				}
			}
			return inner(ctx, req)
		}
		prepared := modelPrepared(script.model)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session = createSession(t, h)
		watch := watchSettlements(store)

		{
			_, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		watch.next()
		watch.next()
		{
			rec := settledOperation(t, store, session, "turn-2")
			testassert.Require(t, rec.State.Status == OperationSuccess, "drained turn settled %+v, want the boundary queued input admitted after the terminal", rec.State)
		}
		{
			got := strings.Join(entryTexts(t, store, session), ",")
			testassert.Equal(t, got, "hello,at-boundary")
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
	prepared := modelPrepared(script.model)
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

	{
		_, err := submitText(t, h, session, "turn-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-script.arrived
	{
		_, err := submitText(t, h, session, "turn-2", MessageModeQueued, "q1")
		testassert.NoError(t, err)
	}
	{
		_, err := h.ChangeAgentType(context.Background(), session, "reviewer")
		testassert.NoError(t, err)
	}

	script.releaseGate()
	watch.next()
	watch.next()
	{
		rec := settledOperation(t, store, session, "turn-2")
		testassert.Require(t, rec.State.Status == OperationSuccess, "drained turn settled %+v, want success", rec.State)
	}
	prepareMu.Lock()
	defer prepareMu.Unlock()
	testassert.Equal(t, len(prepareReqs), 2)
	testassert.Require(t, prepareReqs[0].Session.AgentType == "coder" && prepareReqs[1].Session.AgentType == "reviewer", "preparation agent types = %q then %q, want the capture-time value then the changed one", prepareReqs[0].Session.AgentType, prepareReqs[1].Session.AgentType)
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
		prepared := modelPrepared(script.model)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		session := createSession(t, h)
		watch := watchSettlements(store)

		{
			_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived
		waited := make(chan error, 1)
		go func() { waited <- h.Wait(context.Background()) }() // Wait begins before cancellation

		cancel()             // Harness loss closes admission and ends the run
		script.releaseGate() // the already-started callback publishes, then the run interrupts
		{
			err := <-waited
			testassert.NoError(t, err)
		}
		watch.next()
		{
			rec := settledOperation(t, store, session, "op-1")
			testassert.Equal(t, rec.State.Status, OperationInterruption)
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
		testassert.NoError(t, err)
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

		{
			err := <-admitted
			testassert.Require(t, err != nil, "preparation over a lost Harness context succeeded, want an error")
		}
		{
			err := <-waited
			testassert.NoError(t, err)
		}
		{
			_, regs := storedSessionState(store, session)
			testassert.Equal(t, len(regs), 1)
		}
	})

	t.Run("blocked tool settles before Wait returns", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript(turn(testToolCall("call-1")))
		modelGate := make(chan struct{})
		script.gate = modelGate
		toolGate := make(chan struct{})
		toolArrived := make(chan struct{}, 1)
		exec := validExecution()
		exec.Model = script.model
		exec.Tool = func(context.Context, model.ToolCall) PreparedTool {
			toolArrived <- struct{}{}
			<-toolGate
			return PreparedTool{Permissions: fixturePermission, Execute: func(context.Context) ToolOutcome {
				return ToolOutcome{Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}}
			}}
		}
		prepared := preparedExecuting(exec)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		session := createSession(t, h)
		watch := watchSettlements(store)

		{
			_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived
		script.releaseGate() // the completed output with one call dispatches the tool
		<-toolArrived        // the tool callback is parked

		waited := make(chan error, 1)
		go func() { waited <- h.Wait(context.Background()) }()
		cancel()
		close(toolGate) // the produced result publishes, the run interrupts

		{
			err := <-waited
			testassert.NoError(t, err)
		}
		watch.next()
		rec := settledOperation(t, store, session, "op-1")
		testassert.Require(t, rec.State.Status == OperationInterruption && len(rec.State.PendingToolCalls) == 0, "converged operation = %+v, want interruption with the produced call settled", rec.State)
	})

	t.Run("the first storage failure that stopped admitted work is retained", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		prepared := modelPrepared(script.model)
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
		{
			_, err := submitText(t, h, sessionA, "op-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-failed // the admitted work stopped at the intent publication

		sessionB := createSession(t, h)
		{
			_, err := submitText(t, h, sessionB, "op-1", MessageModeRegular, "hello")
			testassert.Require(t, err != nil, "later storage failure did not surface from Submit")
		}

		cancel()
		{
			err := h.Wait(context.Background())
			testassert.Require(t, errors.Is(err, firstErr), "Wait = %v, want the first retained storage error (later: %v)", err, laterErr)
		}
		{
			rec := settledOperation(t, store, sessionA, "op-1")
			testassert.Equal(t, rec.State.Status, OperationRunning)
		}
	})

	t.Run("a failed terminal publication and a failed cleanup are both retained and joined", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		firstErr := fmt.Errorf("settlement storage failure: %w", ErrStorage)
		cleanupErr := errors.New("first execution cleanup failure")
		laterCleanupErr := errors.New("later execution cleanup failure")
		closedA := make(chan struct{}, 1)
		closedB := make(chan struct{}, 1)
		open := func(_ context.Context, adm OperationAdmission) (Execution, error) {
			exec := validExecution()
			exec.Model = script.model
			if adm.OperationID == "op-a" {
				exec.Close = func() error { closedA <- struct{}{}; return cleanupErr }
			} else {
				exec.Close = func() error { closedB <- struct{}{}; return laterCleanupErr }
			}
			return exec, nil
		}
		prepared := PreparedExecution{Capture: testCapture(), Open: open}
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		replaces := 0
		store.txHook = func(step string) error {
			if step != "replace_register" {
				return nil
			}
			replaces++
			if replaces == 5 { // Session A's outer terminal settlement fails
				return firstErr
			}
			return nil
		}
		sessionA := createSession(t, h)
		{
			_, err := submitText(t, h, sessionA, "op-a", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-closedA // the cleanup ran after the failed terminal-settlement attempt
		sessionB := createSession(t, h)
		{
			_, err := submitText(t, h, sessionB, "op-b", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-closedB

		cancel()
		err := h.Wait(context.Background())
		testassert.Require(t, errors.Is(err, firstErr) && errors.Is(err, cleanupErr) && !errors.Is(err, laterCleanupErr), "Wait = %v, want the join of the retained storage failure and the retained cleanup failure; Wait = %v, want the first retained cleanup failure not replaced by a later one", err, err)
		{
			rec := settledOperation(t, store, sessionA, "op-a")
			testassert.Equal(t, rec.State.Status, OperationRunning)
		}
		{
			rec := settledOperation(t, store, sessionB, "op-b")
			testassert.Equal(t, rec.State.Status, OperationSuccess)
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
	prepared := modelPrepared(script.model)
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

	{
		_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-failed // the admitted work stopped; the Operation stays running for recovery
	c := leaseCoordinator(t, h, session)
	// The stuck Operation keeps the Session active: both submits buffer.
	{
		_, err := submitText(t, h, session, "op-2", MessageModeRegular, "steer")
		testassert.NoError(t, err)
	}
	{
		_, err := submitText(t, h, session, "op-3", MessageModeQueued, "queued-1")
		testassert.NoError(t, err)
	}
	c.mu.Lock()
	buffered := len(c.steering) + len(c.queued)
	c.mu.Unlock()
	testassert.Equal(t, buffered, 2)

	cancel()
	{
		err := h.Wait(context.Background())
		testassert.Require(t, errors.Is(err, firstErr), "Wait = %v, want the retained storage error", err)
	}
	c.mu.Lock()
	left := len(c.steering) + len(c.queued)
	c.mu.Unlock()
	testassert.Equal(t, left, 0)
	{
		rec := settledOperation(t, store, session, "op-1")
		testassert.Equal(t, rec.State.Status, OperationRunning)
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
	prepared := modelPrepared(script.model)
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	session := createSession(t, h)
	watch := watchSettlements(store)

	{
		_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
		testassert.NoError(t, err)
	}
	<-script.arrived
	{
		_, err := submitText(t, h, session, "op-2", MessageModeRegular, "s1")
		testassert.NoError(t, err)
	}
	{
		_, err := submitText(t, h, session, "op-3", MessageModeQueued, "q1")
		testassert.NoError(t, err)
	}

	cancel() // Harness loss
	{
		_, err := submitText(t, h, session, "op-4", MessageModeRegular, "late")
		testassert.Require(t, errors.Is(err, context.Canceled), "submit after Harness loss = %v, want a context error", err)
	}
	script.releaseGate() // the produced result publishes, the run interrupts, the buffers discard
	{
		err := h.Wait(context.Background())
		testassert.NoError(t, err)
	}
	watch.next()
	{
		rec := settledOperation(t, store, session, "op-1")
		testassert.Equal(t, rec.State.Status, OperationInterruption)
	}
	{
		got := strings.Join(entryTexts(t, store, session), ",")
		testassert.Equal(t, got, "hello")
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
		prepared := modelPrepared(script.model)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)

		{
			_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived // the Session is active

		c := leaseCoordinator(t, h, session)
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
			testassert.Require(t, errors.Is(err, context.Canceled), "gated submit = %v, want a context error", err)
		case res := <-done:
			t.Fatalf("gated submit returned %+v, want a context error", res)
		}
		c.mu.Lock()
		buffered := len(c.steering) + len(c.queued)
		c.mu.Unlock()
		testassert.Equal(t, buffered, 0)
		script.releaseGate()
	})

	t.Run("an enqueue that wins the gate delivers after later caller cancellation", func(t *testing.T) {
		store := emptyStore(t)
		script := newModelScript()
		gate := make(chan struct{})
		script.gate = gate
		prepared := modelPrepared(script.model)
		h, cancel := newCancelableHarness(t, store, prepared, nil)
		defer cancel()
		session := createSession(t, h)

		{
			_, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello")
			testassert.NoError(t, err)
		}
		<-script.arrived // the Session is active
		submitCtx, cancelSubmit := context.WithCancel(context.Background())
		res, err := h.Submit(submitCtx, SubmitRequest{SessionID: session, OperationID: "op-2", Origin: InputOriginUser, Content: admissionContent("late"), Mode: MessageModeRegular})
		testassert.Require(t, err == nil && res.Disposition == DispositionSteering, "steering submit = %+v err %v, want the enqueue to win", res, err)
		cancelSubmit() // later caller cancellation does not own the item

		script.releaseGate()
		<-script.arrived
		{
			got := strings.Join(script.lastTexts(), "|")
			testassert.Require(t, strings.Contains(got, "late"), "continuation projection = %q, want the item delivered despite the later cancellation", got)
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
		testassert.NoError(t, err)
		{
			_, disposition := mustAdmitWithoutExecution(t, h, session.Identity.SessionID, testOpID, admissionContent("hello"))
			testassert.Equal(t, disposition, DispositionAdmitted)
		}
		c := leaseCoordinator(t, h, session.Identity.SessionID)
		{
			_, err := h.commitEffectResult(context.Background(), c, testOpID, nil, modelResult{terminal: OperationSuccess})
			testassert.NoError(t, err)
		}
		return h, c
	}

	t.Run("an empty drain retires its own run slot", func(t *testing.T) {
		h, c := newIdleCoordinator(t)
		run := &activeExecution{done: make(chan struct{})}
		c.mu.Lock()
		c.run = run
		c.mu.Unlock()

		release, err := h.reserve(h.ctx, c) // direct drain tests own the reservation around the body
		testassert.NoError(t, err)
		h.drainBuffers(c, run) // synchronous: the empty FIFOs retire the slot in the scan's critical section
		release()

		c.mu.Lock()
		got := c.run
		c.mu.Unlock()
		testassert.Require(t, got == nil, "empty drain left a run slot installed: %+v", got)
	})

	t.Run("an empty drain never clears a replacement run", func(t *testing.T) {
		h, c := newIdleCoordinator(t)
		oldRun := &activeExecution{done: make(chan struct{})}
		replacement := &activeExecution{done: make(chan struct{})}
		c.mu.Lock()
		c.run = replacement // the current slot belongs to another execution
		c.mu.Unlock()

		release, err := h.reserve(h.ctx, c)
		testassert.NoError(t, err)
		h.drainBuffers(c, oldRun)
		release()

		c.mu.Lock()
		got := c.run
		c.mu.Unlock()
		testassert.Equal(t, got, replacement)
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
	exec := validExecution()
	firstModel, secondModel := first.model, second.model
	exec.Model = func(ctx context.Context, req model.Request) (model.Stream, error) {
		texts := textsOf(req)
		if texts[len(texts)-1] == "first" {
			return firstModel(ctx, req)
		}
		return secondModel(ctx, req)
	}
	prepared := preparedExecuting(exec)
	h, cancel := newCancelableHarness(t, store, prepared, nil)
	defer cancel()
	sessionA := createSession(t, h)
	sessionB := createSession(t, h)
	watch := watchSettlements(store)

	{
		_, err := submitText(t, h, sessionA, "op-a", MessageModeRegular, "first")
		testassert.NoError(t, err)
	}
	<-first.arrived // session A parked at its model boundary

	{
		_, err := submitText(t, h, sessionB, "op-b", MessageModeRegular, "second")
		testassert.NoError(t, err)
	}
	<-second.arrived // session B reached its model boundary while A stays parked
	watch.next()
	{
		rec := settledOperation(t, store, sessionB, "op-b")
		testassert.Equal(t, rec.State.Status, OperationSuccess)
	}

	first.releaseGate()
	watch.next()
	{
		rec := settledOperation(t, store, sessionA, "op-a")
		testassert.Equal(t, rec.State.Status, OperationSuccess)
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
	testassert.NoError(t, err)
	c := leaseCoordinator(t, h, session.Identity.SessionID)

	c.mu.Lock() // a coordinator mutex held across the snapshot call
	got := h.snapshotCoordinators()
	c.mu.Unlock()
	testassert.Require(t, len(got) == 1 && got[0] == c, "snapshot = %d coordinators, want exactly the materialized one", len(got))
	// the registry mutex is released once the snapshot returns
	testassert.Require(t, h.mu.TryLock(), "snapshotCoordinators retained the registry mutex")
	h.mu.Unlock()
}
