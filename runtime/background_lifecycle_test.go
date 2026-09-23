package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/model"
)

// The cross-boundary background-lifecycle suite: every row of the background
// lifecycle's final verification step runs through the composed Runtime's
// public surfaces — the admitted-call gate, the background bridge, the
// harness's public background methods, the real jobs and tools plugins, and
// the managed shutdown — over both the memory and SQLite stores where the
// durable state matters. Controlled preparations supply the fake models and
// test tools; the real-plugin rows run the production preparation against a
// scripted model server with real background processes.

// lifecycleAgentsDocument adds the child agent type beside the solo root.
const lifecycleAgentsDocument = `{
	"solo":   {"model": "prov/m", "system_prompt": "simple"},
	"worker": {"model": "prov/m", "system_prompt": "simple"}
}`

// newLifecycleID mints one fresh 32-lowercase-hex identity.
func newLifecycleID(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("random id: %v", err)
	}
	return hex.EncodeToString(buf[:])
}

// --- scripted preparation ---

// lifecycleScript is one session's or agent type's controlled execution: the
// advertised tool names, the model script, the tool dispatcher, an optional
// opener park before the execution returns, and an injected preparation
// failure.
type lifecycleScript struct {
	advertise []string
	model     func(ctx context.Context, sessionID string, attempt int, req model.Request) (model.Stream, error)
	tool      func(ctx context.Context, sessionID string, call model.ToolCall) harness.PreparedTool
	openPark  func(ctx context.Context) error
	fail      error
}

// scriptedPrep is the controlled preparation of the suite: it records every
// preparation request (the complete identity each call received), counts
// model invocations per session, and routes each session's effects through
// its installed script — by session ID first, then by agent type.
type scriptedPrep struct {
	mu             sync.Mutex
	requests       []harness.PreparationRequest
	scripts        map[string]*lifecycleScript
	typeScripts    map[string]*lifecycleScript
	modelCalls     map[string]int
	prepareGate    chan struct{}
	prepareArrived chan struct{}
}

func newScriptedPrep() *scriptedPrep {
	return &scriptedPrep{
		scripts:     make(map[string]*lifecycleScript),
		typeScripts: make(map[string]*lifecycleScript),
		modelCalls:  make(map[string]int),
	}
}

func (p *scriptedPrep) setSessionScript(sessionID string, script *lifecycleScript) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts[sessionID] = script
}

func (p *scriptedPrep) setAgentScript(agentType string, script *lifecycleScript) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.typeScripts[agentType] = script
}

func (p *scriptedPrep) scriptFor(sessionID, agentType string) *lifecycleScript {
	p.mu.Lock()
	defer p.mu.Unlock()
	if script, ok := p.scripts[sessionID]; ok {
		return script
	}
	return p.typeScripts[agentType]
}

// armPreparePark parks the next preparation inside the admission reservation
// — Submit holds its reservation across preparation — returning the arrival
// handshake and the release.
func (p *scriptedPrep) armPreparePark() (<-chan struct{}, func()) {
	arrived := make(chan struct{}, 1)
	gate := make(chan struct{})
	p.mu.Lock()
	p.prepareArrived, p.prepareGate = arrived, gate
	p.mu.Unlock()
	release := func() {
		p.mu.Lock()
		p.prepareGate = nil
		p.mu.Unlock()
		close(gate)
	}
	return arrived, release
}

func (p *scriptedPrep) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *scriptedPrep) workerRequests() []harness.PreparationRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []harness.PreparationRequest
	for _, req := range p.requests {
		if req.Session.AgentType == "worker" {
			out = append(out, req)
		}
	}
	return out
}

func (p *scriptedPrep) modelCallCount(sessionID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.modelCalls[sessionID]
}

func (p *scriptedPrep) totalModelCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, n := range p.modelCalls {
		total += n
	}
	return total
}

func (p *scriptedPrep) prepare(_ context.Context, req harness.PreparationRequest, sel selection) (harness.ExecutionCapture, openExecution, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	arrived, gate := p.prepareArrived, p.prepareGate
	p.mu.Unlock()
	if gate != nil {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-gate
	}
	script := p.scriptFor(req.Session.Identity.SessionID, req.Session.AgentType)
	if script != nil && script.fail != nil {
		return harness.ExecutionCapture{}, nil, script.fail
	}
	var advertise []string
	if script != nil {
		advertise = script.advertise
	}
	capture := harness.ExecutionCapture{
		ConfigurationRevision: sel.invocation.Revision(),
		Model:                 sel.agent.Model,
		SystemPrompt:          "prompt-" + req.Session.AgentType,
		Tools:                 captureTools(advertise),
	}
	return capture, p.opener(script), nil
}

func (p *scriptedPrep) opener(script *lifecycleScript) openExecution {
	return func(ctx context.Context, admission harness.OperationAdmission, _ selection) (harness.Execution, error) {
		if script != nil && script.openPark != nil {
			if err := script.openPark(ctx); err != nil {
				return harness.Execution{}, err
			}
		}
		return harness.Execution{
			Model: func(mctx context.Context, req model.Request) (model.Stream, error) {
				p.mu.Lock()
				p.modelCalls[admission.SessionID]++
				attempt := p.modelCalls[admission.SessionID]
				p.mu.Unlock()
				if script == nil || script.model == nil {
					return lifecycleTextTurn("done"), nil
				}
				return script.model(mctx, admission.SessionID, attempt, req)
			},
			Tool: func(tctx context.Context, call model.ToolCall) harness.PreparedTool {
				if script == nil || script.tool == nil {
					return harness.PreparedTool{
						Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}},
						Immediate: &harness.ToolOutcome{Result: model.ToolResult{
							CallID: call.ID, Status: model.ResultSuccess, Content: "ok",
						}},
					}
				}
				return script.tool(tctx, admission.SessionID, call)
			},
			NormalizeTool: runtimeNormalize,
		}, nil
	}
}

// lifecycleTurn is one single-delta model stream: a completed text or
// tool-call turn.
type lifecycleTurn struct {
	delta model.StreamDelta
	sent  bool
}

func (s *lifecycleTurn) Recv() (model.StreamDelta, error) {
	if s.sent {
		return model.StreamDelta{}, io.EOF
	}
	s.sent = true
	return s.delta, nil
}

func (s *lifecycleTurn) Close() error { return nil }

func lifecycleTextTurn(text string) model.Stream {
	return &lifecycleTurn{delta: model.StreamDelta{
		HasChoice:        true,
		Role:             "assistant",
		ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: text}},
		FinishReason:     "stop",
	}}
}

func lifecycleCallTurn(callID, name, args string) model.Stream {
	position := 0
	return &lifecycleTurn{delta: model.StreamDelta{
		HasChoice:     true,
		Role:          "assistant",
		ToolFragments: []model.ToolCallFragment{{Position: &position, ID: callID, Name: name, ArgumentFragment: args}},
		FinishReason:  "tool_calls",
	}}
}

// requestText joins every text part of every message of one model request.
func requestText(req model.Request) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		for _, part := range msg.Content {
			if part.Kind == model.PartText {
				b.WriteString(part.Text)
			}
		}
	}
	return b.String()
}

// --- the armed background bridge ---

// lifecycleBridge is the test BackgroundServices over the composed Runtime's
// live Harness, assigned once after the owner opens exactly like the
// production bridge.
type lifecycleBridge struct {
	h *harness.Harness
}

func (b *lifecycleBridge) LaunchChild(ctx context.Context, req harness.LaunchChildRequest) (harness.LaunchChildResult, error) {
	return b.h.LaunchChildSession(ctx, req)
}

func (b *lifecycleBridge) DeliverCompletion(ctx context.Context, sessionID, completionID, content string) error {
	return b.h.DeliverBackgroundCompletion(ctx, sessionID, completionID, content)
}

// --- the fixture ---

// bgLifecycle is one composed Runtime over the controlled preparation with
// the armed test bridge and a job-stop seam.
type bgLifecycle struct {
	t      *testing.T
	e      *ownerEnv
	prep   *scriptedPrep
	bridge *lifecycleBridge
	r      *Runtime
}

// openBackgroundLifecycle opens the composed Runtime with the scripted
// preparation and the given job-stop seam value. The optional tweak runs
// before construction and may rewrite the configuration document and adjust
// the private options (for example the controlled sweep tick stream).
func openBackgroundLifecycle(t *testing.T, store harness.Storage, stopper harness.JobStopper, tweak ...func(e *ownerEnv, opts *options)) *bgLifecycle {
	t.Helper()
	ctx := context.Background()
	e := newOwnerEnv(t)
	writeServiceFile(t, agents.PathForConfig(e.configPath), lifecycleAgentsDocument)
	prep := newScriptedPrep()
	bridge := &lifecycleBridge{}
	opts := options{
		DataDir:    e.dataDir,
		ConfigPath: e.configPath,
		Plugins:    []Plugin{e.storagePlugin(store), jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, stopper)},
		prepare:    prep.prepare,
	}
	for _, apply := range tweak {
		apply(e, &opts)
	}
	r, err := open(ctx, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	bridge.h = r.harness // single pre-test assignment: no synchronization needed
	return &bgLifecycle{t: t, e: e, prep: prep, bridge: bridge, r: r}
}

// session creates one root Session of the solo agent type.
func (b *bgLifecycle) session(name string) string {
	b.t.Helper()
	rec, err := b.r.createSession(context.Background(), filepath.Join(b.e.home, name), "solo")
	if err != nil {
		b.t.Fatalf("createSession(%s): %v", name, err)
	}
	return rec.Identity.SessionID
}

// launchChild launches one child Session of the worker agent type through the
// Runtime's admitted-call gate.
func (b *bgLifecycle) launchChild(parent, prompt, operationID string, maxConcurrent int) string {
	b.t.Helper()
	var childID string
	err := b.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		res, err := h.LaunchChildSession(ctx, harness.LaunchChildRequest{
			ParentSessionID: parent,
			AgentType:       "worker",
			Content:         []model.ContentPart{{Kind: model.PartText, Text: prompt}},
			OperationID:     operationID,
			MaxConcurrent:   maxConcurrent,
			OutputLimit:     4096,
		})
		if err != nil {
			return err
		}
		childID = res.ChildSessionID
		return nil
	})
	if err != nil {
		b.t.Fatalf("LaunchChildSession: %v", err)
	}
	return childID
}

// startJob starts one live job member whose spawn captures the reserved
// completion identity.
func (b *bgLifecycle) startJob(sessionID, jobID string) string {
	b.t.Helper()
	var completionID string
	err := startJobThrough(b.r, sessionID, jobID, func(_ context.Context, id string) error {
		completionID = id
		return nil
	})
	if err != nil {
		b.t.Fatalf("StartJob(%s): %v", jobID, err)
	}
	if completionID == "" {
		b.t.Fatalf("StartJob(%s) reserved no completion identity", jobID)
	}
	return completionID
}

// stop stops one Session through the Runtime's admitted-call gate.
func (b *bgLifecycle) stop(sessionID string) error {
	b.t.Helper()
	return b.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		return h.Stop(ctx, sessionID)
	})
}

// interrupt interrupts one Session through the Runtime's admitted-call gate.
func (b *bgLifecycle) interrupt(sessionID string) error {
	b.t.Helper()
	return b.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		return h.Interrupt(ctx, sessionID)
	})
}

// submitMode submits one message in the given mode and returns its
// disposition.
func (b *bgLifecycle) submitMode(sessionID, operationID, text string, mode harness.MessageMode) harness.SubmitDisposition {
	b.t.Helper()
	var disposition harness.SubmitDisposition
	err := b.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		res, err := h.Submit(ctx, harness.SubmitRequest{
			SessionID:   sessionID,
			OperationID: operationID,
			Origin:      harness.InputOriginUser,
			Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
			Mode:        mode,
		})
		if err != nil {
			return err
		}
		disposition = res.Disposition
		return nil
	})
	if err != nil {
		b.t.Fatalf("Submit(%s): %v", operationID, err)
	}
	return disposition
}

// --- the gated transaction store ---

// gatedStore parks exactly the next armed transaction before it reaches the
// base store, signalling its arrival, and counts every transaction entry:
// the public observable of one coordinator holding the storage lock while a
// second delivery cannot enter.
type gatedStore struct {
	harness.Storage
	mu       sync.Mutex
	postLeft int
	gate     chan struct{}
	parked   chan struct{}
	total    int
}

func newGatedStore(base harness.Storage) *gatedStore {
	return &gatedStore{Storage: base, parked: make(chan struct{}, 4)}
}

// armPostCommit parks each of the next n transactions after its commit and
// before its return — the parked caller still holds its coordinator lock —
// signalling arrival per park.
func (s *gatedStore) armPostCommit(n int) {
	s.mu.Lock()
	s.postLeft = n
	s.gate = make(chan struct{})
	s.mu.Unlock()
}

func (s *gatedStore) releaseGate() {
	s.mu.Lock()
	gate := s.gate
	if s.postLeft > 0 {
		s.gate = make(chan struct{})
	} else {
		s.gate = nil
	}
	s.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (s *gatedStore) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	s.mu.Lock()
	park := s.postLeft > 0
	var gate chan struct{}
	if park {
		s.postLeft--
		gate = s.gate
	}
	s.total++
	s.mu.Unlock()
	err := s.Storage.Transact(ctx, fn)
	if park && err == nil {
		select {
		case s.parked <- struct{}{}:
		default:
		}
		<-gate
	}
	return err
}

func (s *gatedStore) transactCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// --- the delivering job-stop seam ---

// lifecycleDelivery is one completion the seam delivers when the harness
// stops its job member.
type lifecycleDelivery struct {
	sessionID    string
	completionID string
	content      string
}

// lifecycleStopper is the recording job-stop seam: on the first StopJob it
// delivers every armed completion — concurrently when asked — and records
// whether the durable signal entry was committed while the stop was still
// converging. An armed park holds the StopJob call so a test can race a start
// against the stopping window.
type lifecycleStopper struct {
	mu           sync.Mutex
	h            *harness.Harness
	store        harness.Storage
	deliveries   []lifecycleDelivery
	concurrent   bool
	park         bool
	parkArrived  chan struct{}
	parkRelease  chan struct{}
	entryEarly   bool
	stopJobs     int
	completedIDs map[string]bool
}

func newLifecycleStopper(store harness.Storage) *lifecycleStopper {
	return &lifecycleStopper{store: store, completedIDs: map[string]bool{}}
}

func (s *lifecycleStopper) arm(h *harness.Harness) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h = h
}

func (s *lifecycleStopper) addDelivery(sessionID, completionID, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries = append(s.deliveries, lifecycleDelivery{sessionID: sessionID, completionID: completionID, content: content})
}

func (s *lifecycleStopper) armPark() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.park = true
	s.parkArrived = make(chan struct{}, 1)
	s.parkRelease = make(chan struct{})
}

func (s *lifecycleStopper) releasePark() {
	s.mu.Lock()
	release := s.parkRelease
	s.mu.Unlock()
	if release != nil {
		close(release)
	}
}

func (s *lifecycleStopper) awaitParked(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	arrived := s.parkArrived
	s.mu.Unlock()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the StopJob park never arrived")
	}
}

func (s *lifecycleStopper) StopJob(sessionID, jobID string) {
	s.mu.Lock()
	s.stopJobs++
	deliveries := s.deliveries
	s.deliveries = nil
	concurrent := s.concurrent
	park, parkArrived, parkRelease := s.park, s.parkArrived, s.parkRelease
	s.park = false
	h, store := s.h, s.store
	s.mu.Unlock()
	if h == nil {
		return
	}
	if park {
		select {
		case parkArrived <- struct{}{}:
		default:
		}
		<-parkRelease
	}
	deliver := func(d lifecycleDelivery) {
		err := h.DeliverBackgroundCompletion(context.Background(), d.sessionID, d.completionID, d.content)
		s.mu.Lock()
		s.completedIDs[d.completionID] = true
		s.mu.Unlock()
		if err != nil {
			return
		}
		// The stop is still converging: the durable signal entry is already
		// committed on the closed parent.
		for _, completion := range completionsOf(store, d.sessionID) {
			if completion.content == d.content {
				s.mu.Lock()
				s.entryEarly = true
				s.mu.Unlock()
			}
		}
	}
	if concurrent {
		var wg sync.WaitGroup
		for _, d := range deliveries {
			wg.Add(1)
			go func(d lifecycleDelivery) {
				defer wg.Done()
				deliver(d)
			}(d)
		}
		wg.Wait()
		return
	}
	for _, d := range deliveries {
		deliver(d)
	}
}

func (s *lifecycleStopper) entryCommittedEarly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entryEarly
}

// completedMember reports whether one delivery's DeliverBackgroundCompletion
// call returned — on the closed path its claim-then-finish ran, so the
// member finished only after its commit.
func (s *lifecycleStopper) completedMember(completionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.completedIDs[completionID]
}

func (s *lifecycleStopper) stopJobCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopJobs
}

// --- durable-state readers ---

// lifecycleCompletion is one decoded background_completion signal entry.
type lifecycleCompletion struct {
	memberKind string
	memberID   string
	content    string
}

// completionsOf decodes one Session's background_completion signals,
// ignoring read errors: the recording seam runs on its own goroutine.
func completionsOf(store harness.Storage, sessionID string) []lifecycleCompletion {
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		return nil
	}
	return decodeLifecycleCompletions(entries)
}

func decodeLifecycleCompletions(entries []harness.Entry) []lifecycleCompletion {
	var completions []lifecycleCompletion
	for _, entry := range entries {
		if entry.Kind != harness.EntrySignal {
			continue
		}
		var wire struct {
			Signal        string `json:"signal"`
			Content       string `json:"content"`
			RelatedMember *struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"related_member"`
		}
		if err := json.Unmarshal(entry.Payload, &wire); err != nil || wire.Signal != "background_completion" {
			continue
		}
		completion := lifecycleCompletion{content: wire.Content}
		if wire.RelatedMember != nil {
			completion.memberKind = wire.RelatedMember.Kind
			completion.memberID = wire.RelatedMember.ID
		}
		completions = append(completions, completion)
	}
	return completions
}

// lifecycleCompletionsOf reads and decodes one Session's background
// completion signals, failing the test on a read error.
func lifecycleCompletionsOf(t *testing.T, store harness.Storage, sessionID string) []lifecycleCompletion {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEntries(%s): %v", sessionID, err)
	}
	return decodeLifecycleCompletions(entries)
}

// lifecycleInput is one decoded input entry.
type lifecycleInput struct {
	operationID string
	origin      string
	text        string
}

func lifecycleInputsOf(t *testing.T, store harness.Storage, sessionID string) []lifecycleInput {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEntries(%s): %v", sessionID, err)
	}
	var inputs []lifecycleInput
	for _, entry := range entries {
		if entry.Kind != harness.EntryInput {
			continue
		}
		var wire struct {
			OperationID string `json:"operation_id"`
			Origin      string `json:"origin"`
			Content     []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(entry.Payload, &wire); err != nil {
			t.Fatalf("decode input payload %s: %v", entry.Payload, err)
		}
		input := lifecycleInput{operationID: wire.OperationID, origin: wire.Origin}
		for _, part := range wire.Content {
			if part.Kind == "text" {
				input.text += part.Text
			}
		}
		inputs = append(inputs, input)
	}
	return inputs
}

// awaitSettlementEntry polls one Session's entries until the given
// Operation's settlement entry commits — the storage-level rendezvous for a
// child's terminal commit.
func awaitSettlementEntry(t *testing.T, store harness.Storage, sessionID, operationID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := store.ReadEntries(context.Background(), sessionID, 0)
		if err == nil {
			for _, entry := range entries {
				if entry.Kind != harness.EntryOperationSettlement {
					continue
				}
				var wire struct {
					OperationID string `json:"operation_id"`
				}
				if json.Unmarshal(entry.Payload, &wire) == nil && wire.OperationID == operationID {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the settlement entry of operation %q never committed on session %s", operationID, sessionID)
}

// awaitRuntimeInput polls one Session for a runtime-origin input entry
// whose text contains want: a cross-session completion lands after the
// settling operation's terminal commit.
func awaitRuntimeInput(t *testing.T, store harness.Storage, sessionID, want string) lifecycleInput {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, input := range lifecycleInputsOf(t, store, sessionID) {
			if input.origin == "runtime" && strings.Contains(input.text, want) {
				return input
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no runtime-origin input containing %q on session %s", want, sessionID)
	return lifecycleInput{}
}

// findRuntimeInput returns the first runtime-origin input entry whose text
// contains want, failing when absent.
func findRuntimeInput(t *testing.T, store harness.Storage, sessionID, want string) lifecycleInput {
	t.Helper()
	for _, input := range lifecycleInputsOf(t, store, sessionID) {
		if input.origin == "runtime" && strings.Contains(input.text, want) {
			return input
		}
	}
	t.Fatalf("no runtime-origin input containing %q on session %s", want, sessionID)
	return lifecycleInput{}
}

// hasRuntimeInput reports whether one Session holds a runtime-origin input
// whose text contains want.
func hasRuntimeInput(t *testing.T, store harness.Storage, sessionID, want string) bool {
	t.Helper()
	for _, input := range lifecycleInputsOf(t, store, sessionID) {
		if input.origin == "runtime" && strings.Contains(input.text, want) {
			return true
		}
	}
	return false
}

// lifecycleToolResultsOf decodes every committed tool result of one Session,
// keyed by tool call ID.
func lifecycleToolResultsOf(t *testing.T, store harness.Storage, sessionID string) map[string]string {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEntries(%s): %v", sessionID, err)
	}
	results := make(map[string]string)
	for _, entry := range entries {
		if entry.Kind != harness.EntryToolResult {
			continue
		}
		var wire struct {
			ToolCallID string `json:"tool_call_id"`
			Content    string `json:"content"`
		}
		if err := json.Unmarshal(entry.Payload, &wire); err != nil {
			t.Fatalf("decode tool_result payload: %v", err)
		}
		results[wire.ToolCallID] = wire.Content
	}
	return results
}

// operationStatusOf reads one Operation register's status without failing.
func operationStatusOf(store harness.Storage, sessionID, operationID string) (harness.OperationState, error) {
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID})
	if err != nil {
		return "", err
	}
	var wire struct {
		State struct {
			Status harness.OperationState `json:"status"`
		} `json:"state"`
	}
	if err := json.Unmarshal(reg.Payload, &wire); err != nil {
		return "", err
	}
	return wire.State.Status, nil
}

// pollOperationStatus polls one Operation's register status until terminal
// or the deadline, returning the terminal status.
func pollOperationStatus(store harness.Storage, sessionID, operationID string, deadline time.Time) (harness.OperationState, error) {
	for {
		status, err := operationStatusOf(store, sessionID, operationID)
		if err == nil {
			switch status {
			case harness.OperationSuccess, harness.OperationFailure, harness.OperationInterruption:
				return status, nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", err
			}
			return status, fmt.Errorf("operation %q never settled (status %q)", operationID, status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sessionCurrentOperation reads one Session register's current Operation.
func sessionCurrentOperation(t *testing.T, store harness.Storage, sessionID string) string {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	if err != nil {
		t.Fatalf("ReadRegister(session %s): %v", sessionID, err)
	}
	var wire struct {
		State struct {
			CurrentOperationID string `json:"current_operation_id"`
		} `json:"state"`
	}
	if err := json.Unmarshal(reg.Payload, &wire); err != nil {
		t.Fatalf("decode session register: %v", err)
	}
	return wire.State.CurrentOperationID
}

// corruptSessionRegister replaces one Session register's payload with an
// undecodable document at its current revision.
func corruptSessionRegister(t *testing.T, store harness.Storage, sessionID string) {
	t.Helper()
	key := harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}
	reg, err := store.ReadRegister(context.Background(), key)
	if err != nil {
		t.Fatalf("ReadRegister(%s): %v", sessionID, err)
	}
	err = store.Transact(context.Background(), func(tx harness.Transaction) error {
		_, err := tx.ReplaceRegister(key, reg.Revision, json.RawMessage(`{"not":"a session register"}`))
		return err
	})
	if err != nil {
		t.Fatalf("corrupt register: %v", err)
	}
}

// registerSnapshot captures one Session's registers for byte-identity
// comparison.
func registerSnapshot(t *testing.T, store harness.Storage, sessionID string) []harness.Register {
	t.Helper()
	registers, err := store.ReadRegisters(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadRegisters(%s): %v", sessionID, err)
	}
	return registers
}

// entrySnapshot captures one Session's full entry list for byte-identity
// comparison.
func entrySnapshot(t *testing.T, store harness.Storage, sessionID string) []harness.Entry {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEntries(%s): %v", sessionID, err)
	}
	return entries
}

func assertEntriesUnchanged(t *testing.T, before, after []harness.Entry) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("entry count changed %d -> %d", len(before), len(after))
	}
	for i := range before {
		b, a := before[i], after[i]
		if b.ID != a.ID || b.Kind != a.Kind || b.Sequence != a.Sequence || string(b.Payload) != string(a.Payload) {
			t.Fatalf("entry %s changed across the second recovery (sequence %d -> %d)", b.ID, b.Sequence, a.Sequence)
		}
	}
}

func assertRegistersUnchanged(t *testing.T, before, after []harness.Register) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("register count changed %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Key != after[i].Key || before[i].Revision != after[i].Revision || string(before[i].Payload) != string(after[i].Payload) {
			t.Fatalf("register %v changed (revision %d -> %d)", before[i].Key, before[i].Revision, after[i].Revision)
		}
	}
}

// --- fixtures end; tests follow ---

// --- R4 active: launch -> child executes -> steering delivery ---

// TestBackgroundLifecycleLaunchChildSteeringDelivery proves the active row:
// a prepared call launches one child through the background bridge, the
// child executes on the fake model and settles, and its completion is
// delivered as steering into the parent's running Operation: the completion
// text reaches the parent's next model call as a runtime-origin input entry
// of that Operation, and no signal entry exists for the receivable parent.
func TestBackgroundLifecycleLaunchChildSteeringDelivery(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
		parent := bg.session("steering-parent")
		launchOp := newLifecycleID(t)

		var seenMu sync.Mutex
		completionAtBoundary := ""
		bg.prep.setSessionScript(parent, &lifecycleScript{
			advertise: []string{"spawn_child", "noop"},
			model: func(_ context.Context, _ string, attempt int, req model.Request) (model.Stream, error) {
				if strings.Contains(requestText(req), "child done") {
					seenMu.Lock()
					completionAtBoundary = requestText(req)
					seenMu.Unlock()
					return lifecycleTextTurn("parent done"), nil
				}
				if attempt == 1 {
					return lifecycleCallTurn("call-spawn", "spawn_child", `{"prompt":"child work"}`), nil
				}
				if attempt >= 8 {
					// Fail the boundary assertion instead of looping forever.
					return lifecycleTextTurn("no completion arrived"), nil
				}
				return lifecycleCallTurn(fmt.Sprintf("call-noop-%d", attempt), "noop", `{}`), nil
			},
			tool: func(toolCtx context.Context, sessionID string, call model.ToolCall) harness.PreparedTool {
				if call.Name != "spawn_child" {
					return harness.PreparedTool{
						Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}},
						Immediate: &harness.ToolOutcome{Result: model.ToolResult{
							CallID: call.ID, Status: model.ResultSuccess, Content: "ok",
						}},
					}
				}
				var args struct {
					Prompt string `json:"prompt"`
				}
				_ = json.Unmarshal(call.Arguments, &args)
				return harness.PreparedTool{
					Permissions: []harness.PermissionRequest{{Permission: "child.launch", Target: "worker"}},
					Execute: func(execCtx context.Context) harness.ToolOutcome {
						res, err := bg.bridge.LaunchChild(execCtx, harness.LaunchChildRequest{
							ParentSessionID: sessionID,
							AgentType:       "worker",
							Content:         []model.ContentPart{{Kind: model.PartText, Text: args.Prompt}},
							OperationID:     launchOp,
							MaxConcurrent:   5,
							OutputLimit:     4096,
						})
						if err != nil {
							return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: err.Error()}}
						}
						if _, err := pollOperationStatus(store, res.ChildSessionID, launchOp, time.Now().Add(15*time.Second)); err != nil {
							return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: err.Error()}}
						}
						return harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "spawned"}}
					},
				}
			},
		})
		bg.prep.setAgentScript("worker", &lifecycleScript{
			model: func(_ context.Context, _ string, _ int, _ model.Request) (model.Stream, error) {
				return lifecycleTextTurn("child done"), nil
			},
		})

		submitThroughRuntime(t, bg.r, parent, "op-1", "run the child")
		{
			deadline := time.Now().Add(5 * time.Second)
			var status harness.OperationState
			var detail string
			for time.Now().Before(deadline) {
				status, detail = operationRegisterTerminal(t, store, parent, "op-1")
				if status != harness.OperationRunning {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if status != harness.OperationSuccess {
				t.Fatalf("parent operation settled %q detail %q", status, detail)
			}
		}

		// The child executed and settled on the fake model, with the launch
		// lineage durable.
		ids, err := store.ListSessionIDs(ctx)
		if err != nil {
			t.Fatalf("ListSessionIDs: %v", err)
		}
		child := ""
		for _, id := range ids {
			if id != parent {
				child = id
			}
		}
		if child == "" {
			t.Fatal("no durable child session exists")
		}
		if got := sessionCurrentOperation(t, store, child); got != "" {
			t.Fatalf("child current operation = %q, want cleared after settlement", got)
		}
		if status, err := operationStatusOf(store, child, launchOp); err != nil || status != harness.OperationSuccess {
			t.Fatalf("child operation status = (%q, %v), want success", status, err)
		}
		childInputs := lifecycleInputsOf(t, store, child)
		if len(childInputs) != 1 || childInputs[0].origin != "plugin" || childInputs[0].text != "child work" {
			t.Fatalf("child inputs = %+v, want the plugin-origin launch prompt", childInputs)
		}

		// The completion reached the parent's model boundary as steering.
		seenMu.Lock()
		atBoundary := completionAtBoundary
		seenMu.Unlock()
		if !strings.Contains(atBoundary, "child done") {
			t.Fatalf("the parent's model boundary never saw the child completion (last request text %q)", atBoundary)
		}
		steering := findRuntimeInput(t, store, parent, "child done")
		if steering.operationID != "op-1" {
			t.Fatalf("completion input operation = %q, want the parent's running op-1", steering.operationID)
		}
		if completions := lifecycleCompletionsOf(t, store, parent); len(completions) != 0 {
			t.Fatalf("receivable parent recorded %d signal entries, want none", len(completions))
		}
		if err := bg.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- R4 idle: admission under the completion identity and the reservation race ---

// TestBackgroundLifecycleIdleAdmissionAndReservationRace proves the idle row:
// a completion delivered to an idle parent admits one Operation under the
// completion identity with a runtime-origin input entry, and the reservation
// race re-routes to steering — a submit holding the admission reservation
// while the delivery parks in its own reserve lands the completion as the
// winning Operation's steering input, never as its own Operation and never
// dropped.
func TestBackgroundLifecycleIdleAdmissionAndReservationRace(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()

		t.Run("idle admission under the completion identity", func(t *testing.T) {
			bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
			session := bg.session("idle-parent")
			completionID := bg.startJob(session, "aa000001")

			if err := bg.bridge.DeliverCompletion(ctx, session, completionID, "job report"); err != nil {
				t.Fatalf("DeliverCompletion: %v", err)
			}
			rec := awaitOperation(t, bg.r, session, completionID, harness.OperationSuccess)
			if rec.Admission.OperationID != completionID {
				t.Fatalf("admitted operation = %q, want the completion identity %q", rec.Admission.OperationID, completionID)
			}
			input := findRuntimeInput(t, store, session, "job report")
			if input.operationID != completionID {
				t.Fatalf("completion input operation = %q, want %q", input.operationID, completionID)
			}
			if completions := lifecycleCompletionsOf(t, store, session); len(completions) != 0 {
				t.Fatalf("receivable parent recorded %d signal entries, want none", len(completions))
			}
			if got := bg.prep.modelCallCount(session); got != 1 {
				t.Fatalf("parent model calls = %d, want exactly the completion operation's one call", got)
			}
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})

		t.Run("reservation race re-routes to steering", func(t *testing.T) {
			bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
			session := bg.session("race-parent")
			completionID := bg.startJob(session, "bb000002")
			// Two model-call gates make "run installed" structural across
			// the delivery's entire classification window: the first call
			// parks on firstCallGate (the run cannot settle before the
			// delivery has classified), and the steering-carrying call
			// parks on modelGate (the drain committed the input entry
			// before this call, so the outcome assertions run against a
			// settled-structure state under every scheduling).
			modelParked := make(chan struct{}, 1)
			modelGate := make(chan struct{})
			firstCallGate := make(chan struct{})
			bg.prep.setSessionScript(session, &lifecycleScript{
				advertise: []string{"noop"},
				model: func(_ context.Context, _ string, attempt int, req model.Request) (model.Stream, error) {
					if strings.Contains(requestText(req), "job report") {
						select {
						case modelParked <- struct{}{}:
						default:
						}
						<-modelGate
						return lifecycleTextTurn("done"), nil
					}
					if attempt == 1 {
						<-firstCallGate // installed run held: settle is impossible while this gate is closed
					}
					return lifecycleCallTurn(fmt.Sprintf("call-noop-%d", attempt), "noop", `{}`), nil
				},
			})

			// The test-side hold of the admission reservation: Submit holds
			// it across preparation, so parking preparation holds the
			// reservation with nothing committed and no run installed.
			prepArrived, releasePrep := bg.prep.armPreparePark()
			submitDone := make(chan error, 1)
			go func() {
				submitDone <- bg.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
					res, err := h.Submit(ctx, harness.SubmitRequest{
						SessionID:   session,
						OperationID: "op-race",
						Origin:      harness.InputOriginUser,
						Content:     []model.ContentPart{{Kind: model.PartText, Text: "hello"}},
						Mode:        harness.MessageModeRegular,
					})
					if err != nil {
						return err
					}
					if res.Disposition != harness.DispositionAdmitted {
						return fmt.Errorf("submit = %+v, want admitted", res.Disposition)
					}
					return nil
				})
			}()
			<-prepArrived

			deliveryDone := make(chan error, 1)
			go func() {
				deliveryDone <- bg.bridge.DeliverCompletion(context.Background(), session, completionID, "job report")
			}()

			// The submit installs its run — held by the parked first call,
			// it cannot settle — and only then releases the reservation, so
			// the delivery classifies steering either way: before the
			// release (re-evaluated under the reservation) or after it
			// (seeing the installed run). The settle-then-idle path is
			// structurally impossible while firstCallGate is closed.
			releasePrep()
			if err := <-submitDone; err != nil {
				t.Fatalf("racing submit: %v", err)
			}
			if err := <-deliveryDone; err != nil {
				t.Fatalf("delivery: %v", err)
			}
			close(firstCallGate)

			// The run's next boundary drains the steering, commits the
			// input entry, and parks the steering-carrying call.
			select {
			case <-modelParked:
			case <-time.After(10 * time.Second):
				t.Fatal("the steering-carrying model call never arrived")
			}

			// The reroute: the completion is steering input to the run that
			// was installed after the delivery started, and the still-idle
			// admission is the forbidden sibling.
			steering := findRuntimeInput(t, store, session, "job report")
			if steering.operationID != "op-race" {
				t.Fatalf("completion input operation = %q, want the winning submit's op-race", steering.operationID)
			}
			if _, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: session, Kind: harness.RegisterOperation, OperationID: completionID}); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("a separate completion operation was admitted (%v), want the re-routed steering only", err)
			}
			if completions := lifecycleCompletionsOf(t, store, session); len(completions) != 0 {
				t.Fatalf("receivable parent recorded %d signal entries, want none", len(completions))
			}

			close(modelGate)
			awaitOperation(t, bg.r, session, "op-race", harness.OperationSuccess)
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})
}

// --- R2 + R4 closed: a closed parent records the signal entry ---

// TestBackgroundLifecycleClosedParentRecording proves the closed row: a
// member completion delivered to a closed parent — a permanently stopped
// child, and a parent under harness cancellation — is recorded as a durable
// background_completion signal entry on that parent while the stop is still
// converging, and no signal entry exists for a receivable parent.
func TestBackgroundLifecycleClosedParentRecording(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()

		t.Run("a permanently stopped child records its member's completion", func(t *testing.T) {
			stopper := newLifecycleStopper(store)
			bg := openBackgroundLifecycle(t, store, stopper)
			stopper.arm(bg.r.harness)
			root := bg.session("closed-root")
			bg.prep.setAgentScript("worker", &lifecycleScript{
				model: func(_ context.Context, _ string, _ int, _ model.Request) (model.Stream, error) {
					return lifecycleTextTurn("child finished"), nil
				},
			})
			childOp := newLifecycleID(t)
			child := bg.launchChild(root, "child task", childOp, 5)
			awaitOperation(t, bg.r, child, childOp, harness.OperationSuccess)

			jobCompletion := bg.startJob(child, "cc000003")
			stopper.addDelivery(child, jobCompletion, "job report")

			if err := bg.stop(child); err != nil {
				t.Fatalf("Stop(child): %v", err)
			}
			if !stopper.entryCommittedEarly() {
				t.Fatal("the job completion signal entry was not committed while the stop still converged")
			}
			completions := lifecycleCompletionsOf(t, store, child)
			if len(completions) != 1 {
				t.Fatalf("stopped child recorded %d completion entries, want one", len(completions))
			}
			if completions[0].memberKind != "job" || completions[0].memberID != "cc000003" || completions[0].content != "job report" {
				t.Fatalf("recorded completion = %+v, want the job member cc000003 with its report", completions[0])
			}
			// The permanent closure rejects agent work.
			err := bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.Submit(ctx, harness.SubmitRequest{
					SessionID: child, OperationID: "op-after-stop", Origin: harness.InputOriginUser,
					Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}}, Mode: harness.MessageModeRegular,
				})
				return err
			})
			if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "session is closed to agent work") {
				t.Fatalf("Submit on the stopped child = %v, want the permanent-closure rejection", err)
			}
			// The child's own completion still reaches the receivable root.
			if !hasRuntimeInput(t, store, root, "child finished") {
				t.Fatal("the root never received the stopped child's own completion")
			}
			if completions := lifecycleCompletionsOf(t, store, root); len(completions) != 0 {
				t.Fatalf("receivable root recorded %d signal entries, want none", len(completions))
			}
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})

		t.Run("harness cancellation records the signal entry", func(t *testing.T) {
			stopper := newLifecycleStopper(store)
			bg := openBackgroundLifecycle(t, store, stopper)
			stopper.arm(bg.r.harness)
			root := bg.session("cancel-root")
			completionID := bg.startJob(root, "dd000004")
			stopper.addDelivery(root, completionID, "job report")

			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !stopper.entryCommittedEarly() {
				t.Fatal("the job completion signal entry was not committed while the stop still converged")
			}
			completions := lifecycleCompletionsOf(t, store, root)
			if len(completions) != 1 {
				t.Fatalf("canceled parent recorded %d completion entries, want one", len(completions))
			}
			if completions[0].memberKind != "job" || completions[0].memberID != "dd000004" {
				t.Fatalf("recorded completion = %+v, want the job member dd000004", completions[0])
			}
		})
	})
}

// --- R4 concurrency: two concurrent distinct closed completions ---

// TestBackgroundLifecycleConcurrentClosedCompletions proves the concurrency
// row: two distinct completions delivered together to a parent closed by
// harness cancellation serialize through one coordinator transaction — while
// the first holds the coordinator lock across its parked transaction, the
// second cannot enter storage — and after the release both distinct entries
// are committed and both members finish.
func TestBackgroundLifecycleConcurrentClosedCompletions(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		gated := newGatedStore(store)
		stopper := newLifecycleStopper(gated)
		stopper.concurrent = true
		bg := openBackgroundLifecycle(t, gated, stopper)
		stopper.arm(bg.r.harness)
		root := bg.session("concurrent-root")
		first := bg.startJob(root, "ee000005")
		second := bg.startJob(root, "ee000006")
		stopper.addDelivery(root, first, "first report")
		stopper.addDelivery(root, second, "second report")

		// Each closed delivery's transaction is gated at its commit: while
		// the first holds the coordinator lock across its parked return the
		// second cannot enter storage.
		gated.armPostCommit(2)
		baseline := gated.transactCount()
		closeDone := make(chan error, 1)
		go func() { closeDone <- bg.r.Close(context.Background()) }()
		select {
		case <-gated.parked:
		case <-time.After(10 * time.Second):
			t.Fatal("the first closed-delivery transaction never committed")
		}
		firstEntries := lifecycleCompletionsOf(t, store, root)
		if len(firstEntries) != 1 {
			t.Fatalf("completion entries while the first was parked = %d, want exactly its own commit", len(firstEntries))
		}
		if got := gated.transactCount() - baseline; got != 1 {
			t.Fatalf("transaction entries while the first held the coordinator lock = %d, want exactly its own", got)
		}
		// Per-member commit-before-finish: the parked delivery's entry is
		// durable while that member's own delivery call has not returned.
		idOf := map[string]string{"first report": first, "second report": second}
		parkedID, ok := idOf[firstEntries[0].content]
		if !ok {
			t.Fatalf("parked completion content = %q, want one of the two reports", firstEntries[0].content)
		}
		if stopper.completedMember(parkedID) {
			t.Fatalf("member %s completed before its delivery returned", parkedID)
		}

		gated.releaseGate()
		select {
		case <-gated.parked:
		case <-time.After(10 * time.Second):
			t.Fatal("the second closed-delivery transaction never committed")
		}
		bothEntries := lifecycleCompletionsOf(t, store, root)
		if len(bothEntries) != 2 {
			t.Fatalf("completion entries after both commits = %d, want the two distinct completions", len(bothEntries))
		}
		// The second-parked member is the one whose entry appeared since
		// the first park — present in bothEntries but absent from
		// firstEntries. Its identity-diff entry is durable while its own
		// delivery goroutine is parked inside its call: structurally it
		// cannot be marked finished.
		var secondID string
		for _, completion := range bothEntries {
			if completion.memberID == firstEntries[0].memberID {
				continue
			}
			id, ok := idOf[completion.content]
			if !ok {
				t.Fatalf("second-parked completion content = %q, want one of the two reports", completion.content)
			}
			secondID = id
		}
		if secondID == "" {
			t.Fatalf("no member appeared since the first park: %+v", bothEntries)
		}
		if stopper.completedMember(secondID) {
			t.Fatalf("member %s completed while its delivery was parked", secondID)
		}

		gated.releaseGate()
		if err := <-closeDone; err != nil {
			t.Fatalf("Close: %v", err)
		}
		// After the full release both deliveries returned — on the closed
		// path that means both members finished after their commits — with
		// the durable order distinct across both.
		for content, id := range idOf {
			if !stopper.completedMember(id) {
				t.Fatalf("member %s (%s) never completed after the release", id, content)
			}
		}
		entries, err := store.ReadEntries(ctx, root, 0)
		if err != nil {
			t.Fatalf("ReadEntries: %v", err)
		}
		durable := decodeLifecycleCompletions(entries)
		if len(durable) != 2 {
			t.Fatalf("durable completion sequence = %+v, want the two commits in sequence order", durable)
		}
		if (durable[0].content == durable[1].content) || (durable[0].memberID == durable[1].memberID) {
			t.Fatalf("durable completion sequence = %+v, want two distinct contents and members", durable)
		}
		for _, completion := range durable {
			if completion.memberKind != "job" || (completion.memberID != "ee000005" && completion.memberID != "ee000006") {
				t.Fatalf("recorded completion = %+v, want one of the two job members", completion)
			}
		}
	})
}

// --- R3: a duplicate delivery changes nothing ---

// TestBackgroundLifecycleDuplicateDeliveryChangesNothing proves the
// duplicate row: a second delivery of one completion is a no-op — the same
// entries, the same operation registers, and no second model invocation.
func TestBackgroundLifecycleDuplicateDeliveryChangesNothing(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
		session := bg.session("duplicate-parent")
		completionID := bg.startJob(session, "ff000007")

		if err := bg.bridge.DeliverCompletion(ctx, session, completionID, "job report"); err != nil {
			t.Fatalf("first DeliverCompletion: %v", err)
		}
		awaitOperation(t, bg.r, session, completionID, harness.OperationSuccess)
		registersBefore := registerSnapshot(t, store, session)
		entriesBefore := lifecycleInputsOf(t, store, session)

		if err := bg.bridge.DeliverCompletion(ctx, session, completionID, "job report"); err != nil {
			t.Fatalf("duplicate DeliverCompletion = %v, want the no-op nil", err)
		}
		assertRegistersUnchanged(t, registersBefore, registerSnapshot(t, store, session))
		if got := lifecycleInputsOf(t, store, session); len(got) != len(entriesBefore) {
			t.Fatalf("input entries after the duplicate = %d, want the unchanged %d", len(got), len(entriesBefore))
		}
		if got := bg.prep.modelCallCount(session); got != 1 {
			t.Fatalf("parent model calls = %d, want the one completion operation call", got)
		}
		if completions := lifecycleCompletionsOf(t, store, session); len(completions) != 0 {
			t.Fatalf("receivable parent recorded %d signal entries, want none", len(completions))
		}
		if err := bg.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- R4 + R8: a child with a live job delivers only after it finishes ---

// TestBackgroundLifecycleChildWaitsForLiveJob proves the deferral row: a
// child whose live job member keeps its group non-empty defers its own
// completion, and after the job's completion settles as the child's
// successor Operation the child's completion delivers to the parent carrying
// the successor's own final answer.
func TestBackgroundLifecycleChildWaitsForLiveJob(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
		root := bg.session("job-child-root")
		firstOp := newLifecycleID(t)
		childStarted := make(chan struct{}, 1)
		childRelease := make(chan struct{})
		bg.prep.setAgentScript("worker", &lifecycleScript{
			model: func(ctx context.Context, _ string, _ int, req model.Request) (model.Stream, error) {
				text := requestText(req)
				if strings.Contains(text, "job report") {
					return lifecycleTextTurn("successor done"), nil
				}
				if strings.Contains(text, "child work") {
					select { // the launch turn parks so the job exists first
					case childStarted <- struct{}{}:
					default:
					}
					<-childRelease
					return lifecycleTextTurn("child first turn"), nil
				}
				return lifecycleTextTurn("done"), nil
			},
		})
		child := bg.launchChild(root, "child work", firstOp, 5)
		<-childStarted
		// The live job joins the child's group before the launch turn settles.
		jobCompletion := bg.startJob(child, "99000009")
		close(childRelease)
		// The child-settlement rendezvous: observe the child's settlement
		// entry via storage while the job gate — its completion delivery —
		// is still held. The deferral is structural: a settled child whose
		// group is non-empty cannot deliver, so a single absence check pins
		// it without any timing window.
		awaitSettlementEntry(t, store, child, firstOp)
		if status, _ := operationStatusOf(store, child, firstOp); status != harness.OperationSuccess {
			t.Fatalf("child operation status = %q, want success", status)
		}
		if hasRuntimeInput(t, store, root, "child first turn") || hasRuntimeInput(t, store, root, "successor done") {
			t.Fatal("the parent received a completion while the child's live job was still held")
		}

		// Release the held job completion: it settles as the child's
		// successor Operation.
		if err := bg.bridge.DeliverCompletion(ctx, child, jobCompletion, "job report"); err != nil {
			t.Fatalf("job completion delivery: %v", err)
		}
		awaitOperation(t, bg.r, child, jobCompletion, harness.OperationSuccess)
		jobInput := findRuntimeInput(t, store, child, "job report")
		if jobInput.operationID != jobCompletion {
			t.Fatalf("job completion input operation = %q, want %q", jobInput.operationID, jobCompletion)
		}

		// The child's completion arrives only now, carrying the successor's
		// own final answer.
		completion := awaitRuntimeInput(t, store, root, "successor done")
		if completion.text != "successor done" {
			t.Fatalf("parent completion text = %q, want the successor's final answer", completion.text)
		}
		awaitOperation(t, bg.r, root, completion.operationID, harness.OperationSuccess)
		if hasRuntimeInput(t, store, root, "child first turn") {
			t.Fatal("the parent received the first turn's answer, want the successor's only")
		}
		if err := bg.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- R7: interrupt windows ---

// TestBackgroundLifecycleInterruptWindows proves the interrupt row through
// the Runtime's admitted-call gate: interrupting a blocked model attempt
// settles the contract interruption and the queued input submitted before
// the interrupt drains afterwards; interrupting a channel-blocked foreground
// tool settles its real interrupted result while the buffered steering
// survives and drains as its own admission; and an interrupt between the
// admission commit and the model effect settles the durable Operation with
// zero model attempts.
func TestBackgroundLifecycleInterruptWindows(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()

		t.Run("blocked model settles interruption and the queued input drains", func(t *testing.T) {
			bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
			session := bg.session("interrupt-model")
			started := make(chan struct{}, 1)
			bg.prep.setSessionScript(session, &lifecycleScript{
				model: func(ctx context.Context, _ string, attempt int, _ model.Request) (model.Stream, error) {
					if attempt == 1 {
						started <- struct{}{}
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return lifecycleTextTurn("done"), nil
				},
			})
			submitThroughRuntime(t, bg.r, session, "op-1", "hello")
			<-started
			if got := bg.submitMode(session, "op-q", "queued", harness.MessageModeQueued); got != harness.DispositionQueued {
				t.Fatalf("queued submit = %q, want queued", got)
			}
			if err := bg.interrupt(session); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			rec := awaitOperation(t, bg.r, session, "op-1", harness.OperationInterruption)
			if rec.State.Terminal == nil || rec.State.Terminal.Detail != "agent interrupted" {
				t.Fatalf("interrupted operation terminal = %+v, want the contract detail", rec.State.Terminal)
			}
			awaitOperation(t, bg.r, session, "op-q", harness.OperationSuccess)
			if got := bg.prep.modelCallCount(session); got != 2 {
				t.Fatalf("model calls = %d, want the parked attempt and the drained queued operation", got)
			}
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})

		t.Run("blocked foreground tool settles its interrupted result and steering drains", func(t *testing.T) {
			bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
			session := bg.session("interrupt-tool")
			var seenMu sync.Mutex
			steeringAtBoundary := ""
			toolStarted := make(chan struct{}, 1)
			bg.prep.setSessionScript(session, &lifecycleScript{
				advertise: []string{"blocking"},
				model: func(_ context.Context, _ string, attempt int, req model.Request) (model.Stream, error) {
					if attempt > 1 {
						seenMu.Lock()
						steeringAtBoundary = requestText(req)
						seenMu.Unlock()
						return lifecycleTextTurn("done"), nil
					}
					return lifecycleCallTurn("call-block", "blocking", `{}`), nil
				},
				tool: func(_ context.Context, _ string, call model.ToolCall) harness.PreparedTool {
					return harness.PreparedTool{
						Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "blocking"}},
						Execute: func(toolCtx context.Context) harness.ToolOutcome {
							toolStarted <- struct{}{}
							<-toolCtx.Done()
							return harness.ToolOutcome{Result: model.ToolResult{
								CallID: call.ID, Status: model.ResultInterrupted, Content: "tool cancelled by test",
							}}
						},
					}
				},
			})
			submitThroughRuntime(t, bg.r, session, "op-tool", "hello")
			<-toolStarted
			if got := bg.submitMode(session, "op-tool-steer", "steer this", harness.MessageModeRegular); got != harness.DispositionSteering {
				t.Fatalf("steering submit = %q, want steering", got)
			}
			if err := bg.interrupt(session); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			awaitOperation(t, bg.r, session, "op-tool", harness.OperationInterruption)
			// The buffered steering survives the interrupt and drains after
			// the interrupted terminal as its own admission.
			awaitOperation(t, bg.r, session, "op-tool-steer", harness.OperationSuccess)
			seenMu.Lock()
			atBoundary := steeringAtBoundary
			seenMu.Unlock()
			if !strings.Contains(atBoundary, "steer this") {
				t.Fatalf("the drained steering never reached a model boundary (last request text %q)", atBoundary)
			}
			results := lifecycleToolResultsOf(t, store, session)
			if got := results["call-block"]; got != "tool cancelled by test" {
				t.Fatalf("blocked tool result = %q, want the tool's real interrupted result", got)
			}
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})

		t.Run("interrupt between the admission commit and the model settles without a model attempt", func(t *testing.T) {
			bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
			session := bg.session("interrupt-window")
			openArrived := make(chan struct{}, 1)
			openRelease := make(chan struct{})
			bg.prep.setSessionScript(session, &lifecycleScript{
				openPark: func(ctx context.Context) error {
					openArrived <- struct{}{}
					select {
					case <-openRelease:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
			submitThroughRuntime(t, bg.r, session, "op-window", "hello")
			select {
			case <-openArrived:
			case <-time.After(10 * time.Second):
				t.Fatal("the execution never reached its opener")
			}
			if err := bg.interrupt(session); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			rec := awaitOperation(t, bg.r, session, "op-window", harness.OperationInterruption)
			if rec.State.Terminal == nil || rec.State.Terminal.Detail != "agent interrupted" {
				t.Fatalf("interrupted operation terminal = %+v, want the contract detail", rec.State.Terminal)
			}
			if got := bg.prep.modelCallCount(session); got != 0 {
				t.Fatalf("model calls = %d, want none: the interrupt settled the operation before the model effect", got)
			}
			close(openRelease)
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})
}

// --- R8 + R5: stop behaviors ---

// TestBackgroundLifecycleStopBehaviors proves the stop row through the
// Runtime's admitted-call gate: a root stop settles the stopped child as
// interruption, kills the job with its completion delivered as steering
// while the root's own Operation survives, joins the stopping window so a
// racing start rejects without spawning, reopens the group, and drains the
// queued buffer afterwards; and a delivery-preventing child failure
// converges through the first-writer claim.
func TestBackgroundLifecycleStopBehaviors(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()

		t.Run("root stop settles members, reopens, and never interrupts the root operation", func(t *testing.T) {
			stopper := newLifecycleStopper(store)
			bg := openBackgroundLifecycle(t, store, stopper)
			stopper.arm(bg.r.harness)
			root := bg.session("stop-root")
			// The child's launch turn parks so the root stop interrupts it.
			bg.prep.setAgentScript("worker", &lifecycleScript{
				model: func(ctx context.Context, _ string, _ int, _ model.Request) (model.Stream, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				},
			})
			modelStarted := make(chan struct{}, 1)
			modelRelease := make(chan struct{})
			var seenMu sync.Mutex
			steeringAtBoundary := ""
			bg.prep.setSessionScript(root, &lifecycleScript{
				advertise: []string{"noop"},
				model: func(_ context.Context, _ string, attempt int, req model.Request) (model.Stream, error) {
					text := requestText(req)
					if strings.Contains(text, "queued message") {
						return lifecycleTextTurn("done"), nil // the drained queued input
					}
					if attempt == 1 {
						modelStarted <- struct{}{}
						<-modelRelease
						return lifecycleCallTurn("call-noop-first", "noop", `{}`), nil
					}
					seenMu.Lock()
					steeringAtBoundary = text
					seenMu.Unlock()
					if strings.Contains(text, "job report") {
						return lifecycleTextTurn("done"), nil
					}
					if attempt >= 8 {
						return lifecycleTextTurn("no steering drained"), nil
					}
					return lifecycleCallTurn(fmt.Sprintf("call-noop-%d", attempt), "noop", `{}`), nil
				},
			})
			childOp := newLifecycleID(t)
			child := bg.launchChild(root, "child task", childOp, 5)
			jobCompletion := bg.startJob(root, "ab000011")
			stopper.addDelivery(root, jobCompletion, "job report")
			stopper.armPark()

			submitThroughRuntime(t, bg.r, root, "op-1", "run")
			<-modelStarted
			if got := bg.submitMode(root, "op-q", "queued message", harness.MessageModeQueued); got != harness.DispositionQueued {
				t.Fatalf("queued submit = %q, want queued", got)
			}

			stopDone := make(chan error, 1)
			go func() { stopDone <- bg.stop(root) }()
			stopper.awaitParked(t)

			// The racing start rejects without spawning while the stop holds
			// the stopping window.
			var spawned bool
			err := startJobThrough(bg.r, root, "ab000012", func(context.Context, string) error {
				spawned = true
				return nil
			})
			if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "background group is closed or stopping") {
				t.Fatalf("racing StartJob = %v, want the stopping rejection", err)
			}
			if spawned {
				t.Fatal("the racing start's spawn ran while the group was stopping")
			}

			stopper.releasePark()
			if err := <-stopDone; err != nil {
				t.Fatalf("Stop: %v", err)
			}
			// The stopped child's Operation settles as interruption.
			awaitOperation(t, bg.r, child, childOp, harness.OperationInterruption)
			// The root's own Operation survives the stop and still runs.
			if status, err := operationStatusOf(store, root, "op-1"); err != nil || status != harness.OperationRunning {
				t.Fatalf("root operation after the stop = (%q, %v), want still running", status, err)
			}
			// Release: the steering completion reaches the model boundary,
			// the root's Operation succeeds, and the queued buffer drains.
			close(modelRelease)
			awaitOperation(t, bg.r, root, "op-1", harness.OperationSuccess)
			seenMu.Lock()
			atBoundary := steeringAtBoundary
			seenMu.Unlock()
			if !strings.Contains(atBoundary, "job report") {
				t.Fatalf("the root's model boundary never saw the stopped job's completion (last request text %q)", atBoundary)
			}
			awaitOperation(t, bg.r, root, "op-q", harness.OperationSuccess)
			// The group reopened: new background work starts and completes.
			reopened := bg.startJob(root, "ab000013")
			if err := bg.bridge.DeliverCompletion(ctx, root, reopened, "after report"); err != nil {
				t.Fatalf("delivery after reopen: %v", err)
			}
			awaitOperation(t, bg.r, root, reopened, harness.OperationSuccess)
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})

		t.Run("stop joins the child admission publish/install window", func(t *testing.T) {
			gated := newGatedStore(store)
			bg := openBackgroundLifecycle(t, gated, newLifecycleStopper(gated))
			root := bg.session("window-root")
			launchOp := newLifecycleID(t)
			bg.prep.setAgentScript("worker", &lifecycleScript{})

			// The launch transaction's return is gated: the child register
			// and admission are durable while the handoff, materialization,
			// and execution installation have not started.
			before := map[string]bool{}
			if ids, err := store.ListSessionIDs(ctx); err == nil {
				for _, id := range ids {
					before[id] = true
				}
			}
			gated.armPostCommit(1)
			type launchResult struct {
				child string
				err   error
			}
			launchDone := make(chan launchResult, 1)
			go func() {
				var res launchResult
				res.err = bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
					out, err := h.LaunchChildSession(ctx, harness.LaunchChildRequest{
						ParentSessionID: root,
						AgentType:       "worker",
						Content:         []model.ContentPart{{Kind: model.PartText, Text: "child work"}},
						OperationID:     launchOp,
						MaxConcurrent:   5,
						OutputLimit:     4096,
					})
					if err == nil {
						res.child = out.ChildSessionID
					}
					return err
				})
				launchDone <- res
			}()
			select {
			case <-gated.parked:
			case <-time.After(10 * time.Second):
				t.Fatal("the child launch transaction never committed")
			}
			ids, err := store.ListSessionIDs(ctx)
			if err != nil {
				t.Fatalf("ListSessionIDs: %v", err)
			}
			child := ""
			for _, id := range ids {
				if !before[id] {
					child = id
				}
			}
			if child == "" {
				t.Fatal("the published child has no durable identity")
			}

			// The stop joins the window: it captures the parent membership
			// and cannot return before the launch installs and settles.
			stopDone := make(chan error, 1)
			go func() { stopDone <- bg.stop(child) }()

			// Deterministic handshake for Stop's critical section: the
			// canceled-context Submit takes the free reservation, reaches
			// the permanent-closure check before the routing gate, and
			// publishes nothing in either state — it reports
			// "session is closed to agent work" only once the stop's
			// section has run. A context.Canceled answer means the
			// section has not run yet; yield and probe again.
			probeDeadline := time.Now().Add(10 * time.Second)
			for {
				var probeErr error
				err := bg.r.withHarness(context.Background(), func(_ context.Context, h *harness.Harness) error {
					probeCtx, cancelProbe := context.WithCancel(context.Background())
					cancelProbe()
					_, probeErr = h.Submit(probeCtx, harness.SubmitRequest{
						SessionID:   child,
						OperationID: "probe-close",
						Origin:      harness.InputOriginUser,
						Content:     []model.ContentPart{{Kind: model.PartText, Text: "probe"}},
						Mode:        harness.MessageModeRegular,
					})
					return nil
				})
				if err != nil {
					t.Fatalf("closure probe gate: %v", err)
				}
				if probeErr != nil && strings.Contains(probeErr.Error(), "session is closed to agent work") {
					break // the stop's section set the permanent closure
				}
				if time.Now().After(probeDeadline) {
					t.Fatalf("the stop's closure never appeared (probe error %v)", probeErr)
				}
				runtime.Gosched()
			}
			select {
			case err := <-stopDone:
				t.Fatalf("Stop returned (%v) inside the publish/install window", err)
			default:
			}

			gated.releaseGate()
			res := <-launchDone
			if res.err != nil {
				t.Fatalf("LaunchChildSession: %v", res.err)
			}
			if res.child != child {
				t.Fatalf("launched child = %q, want the published %q", res.child, child)
			}
			if err := <-stopDone; err != nil {
				t.Fatalf("Stop(child): %v", err)
			}
			// The closed child settles at the entry interruption path and
			// its parent member finishes, unblocking the parent lifecycle.
			status, detail := operationRegisterTerminal(t, gated, child, launchOp)
			if status != harness.OperationInterruption || detail != "agent interrupted" {
				t.Fatalf("child operation = (%q, %q), want the entry interruption", status, detail)
			}
			// The entry guard settles before the model effect ever starts.
			if got := bg.prep.modelCallCount(child); got != 0 {
				t.Fatalf("child model calls = %d, want none before the entry interruption", got)
			}
			if err := bg.stop(root); err != nil {
				t.Fatalf("Stop(root) = %v, want the parent group quiet after the member finished", err)
			}
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})

		t.Run("a delivery-preventing child failure converges through the first-writer claim", func(t *testing.T) {
			stopper := newLifecycleStopper(store)
			bg := openBackgroundLifecycle(t, store, stopper)
			stopper.arm(bg.r.harness)
			root := bg.session("corrupt-root")
			childOp := newLifecycleID(t)
			child := bg.launchChild(root, "child task", childOp, 5)
			awaitOperation(t, bg.r, child, childOp, harness.OperationSuccess)

			jobCompletion := bg.startJob(child, "cd000014")
			stopper.addDelivery(child, jobCompletion, "job report")
			corruptSessionRegister(t, store, child)

			if err := bg.stop(root); err != nil {
				t.Fatalf("Stop: %v, want convergence despite the corrupt child", err)
			}
			// No completion was recorded anywhere: the corruption prevented
			// the natural delivery and no compensating write followed.
			if completions := lifecycleCompletionsOf(t, store, child); len(completions) != 0 {
				t.Fatalf("corrupt child recorded %d completion entries, want none", len(completions))
			}
			if completions := lifecycleCompletionsOf(t, store, root); len(completions) != 0 {
				t.Fatalf("root recorded %d completion entries, want none", len(completions))
			}
			if hasRuntimeInput(t, store, root, "child task") {
				t.Fatal("the root received a completion despite the prevented delivery")
			}
			// The corrupted register is unchanged and the root holds no live
			// member: its lifecycle is unblocked.
			reg, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: child, Kind: harness.RegisterSession})
			if err != nil || string(reg.Payload) != `{"not":"a session register"}` {
				t.Fatalf("corrupted register changed: (%s, %v)", reg.Payload, err)
			}
			err = bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ArchiveSession(ctx, root)
				return err
			})
			if err != nil {
				t.Fatalf("ArchiveSession after the converged stop = %v, want no live member left", err)
			}
			// The shutdown converges; the corrupt child's stop error is
			// collected and joined, never an early exit.
			if err := bg.r.Close(ctx); err != nil && !strings.Contains(err.Error(), "corrupt") {
				t.Fatalf("Close = %v, want convergence with at most the joined corruption report", err)
			}
		})
	})
}

// --- R9: archive gate and the sweep's skip ---

// TestBackgroundLifecycleArchiveGateAndSweepSkip proves the archive row: a
// live — claimed but unfinished — background member rejects the manual
// archive with the gate text and succeeds after the member finishes; the
// automatic sweep skips live background work and archives once the group is
// quiet; and no background start reaches a non-open Session.
func TestBackgroundLifecycleArchiveGateAndSweepSkip(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()

		t.Run("archive rejects a live member and succeeds after it finishes", func(t *testing.T) {
			bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
			session := bg.session("archive-parent")
			completionID := bg.startJob(session, "ef000015")

			err := bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ArchiveSession(ctx, session)
				return err
			})
			if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "session has live background work; stop it first") {
				t.Fatalf("ArchiveSession with a live member = %v, want the live-work rejection", err)
			}

			if err := bg.bridge.DeliverCompletion(ctx, session, completionID, "job report"); err != nil {
				t.Fatalf("delivery: %v", err)
			}
			awaitOperation(t, bg.r, session, completionID, harness.OperationSuccess)
			err = bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ArchiveSession(ctx, session)
				return err
			})
			if err != nil {
				t.Fatalf("ArchiveSession after the member finished = %v, want success", err)
			}
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})

		t.Run("the sweep skips live work and no start reaches a non-open session", func(t *testing.T) {
			wrapped := newSweepStore(store)
			ticks := make(chan time.Time)
			bg := openBackgroundLifecycle(t, wrapped, newLifecycleStopper(wrapped), func(e *ownerEnv, opts *options) {
				writeServiceFile(t, e.configPath, ownerSweepDocument(`{"archive_after_days":1,"delete_after_archive_days":1}`))
				opts.sweepTicks = ticks
			})
			created, err := bg.r.createSession(ctx, filepath.Join(bg.e.home, "sweep-session"), "solo")
			if err != nil {
				t.Fatalf("createSession: %v", err)
			}
			session := created.Identity.SessionID
			completionID := bg.startJob(session, "ef000016")

			// A pass past the archive boundary skips the live member; the
			// next send's rendezvous proves the pass converged. The
			// boundary sits past any LastActivity advance the later
			// completion admission commits, so the quiet pass stays past
			// the threshold too.
			boundary := created.State.LastActivity.Add(48 * time.Hour)
			sendTick(t, ticks, boundary)
			sendTick(t, ticks, boundary)
			if got := sessionLifecycle(t, store, session); got != harness.LifecycleOpen {
				t.Fatalf("session after the sweep over live work = %q, want open", got)
			}

			// The member finishes; the next quiet pass archives. Durable
			// writes from the completion's admission and settlement are
			// drained first so waitWritten observes the sweep's own commit,
			// and a bounded retry absorbs the retirement tail still holding
			// the coordinator when the first pass samples it.
			if err := bg.bridge.DeliverCompletion(ctx, session, completionID, "job report"); err != nil {
				t.Fatalf("delivery: %v", err)
			}
			awaitOperation(t, bg.r, session, completionID, harness.OperationSuccess)
			archived := false
			for attempt := 0; attempt < 3 && !archived; attempt++ {
				drainWritten(wrapped)
				sendTick(t, ticks, boundary)
				sendTick(t, ticks, boundary) // rendezvous: the previous pass converged
				if sessionLifecycle(t, store, session) == harness.LifecycleArchived {
					archived = true
					break
				}
			}
			if !archived {
				t.Fatalf("session after the quiet sweeps = %q, want archived", sessionLifecycle(t, store, session))
			}

			// No background start reaches the non-open Session.
			var spawned bool
			err = startJobThrough(bg.r, session, "ef000019", func(context.Context, string) error {
				spawned = true
				return nil
			})
			if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "is archived; admission requires an open Session") {
				t.Fatalf("StartJob on the archived session = %v, want the archived rejection", err)
			}
			if spawned {
				t.Fatal("the archived-session start's spawn ran")
			}
			err = bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, lerr := h.LaunchChildSession(ctx, harness.LaunchChildRequest{
					ParentSessionID: session,
					AgentType:       "worker",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "x"}},
					OperationID:     newLifecycleID(t),
					MaxConcurrent:   5,
					OutputLimit:     4096,
				})
				return lerr
			})
			if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "is archived; admission requires an open Session") {
				t.Fatalf("LaunchChildSession on the archived session = %v, want the archived rejection", err)
			}
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})
}

// drainWritten discards every pending sweep-write notification so the next
// waitWritten observes only a later commit.
func drainWritten(store *sweepStore) {
	for {
		select {
		case <-store.written:
		default:
			return
		}
	}
}

// sessionLifecycle reads one Session register's lifecycle directly.
func sessionLifecycle(t *testing.T, store harness.Storage, sessionID string) harness.SessionLifecycle {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	if err != nil {
		t.Fatalf("ReadRegister(%s): %v", sessionID, err)
	}
	var wire struct {
		State struct {
			Lifecycle harness.SessionLifecycle `json:"lifecycle"`
		} `json:"state"`
	}
	if err := json.Unmarshal(reg.Payload, &wire); err != nil {
		t.Fatalf("decode session register: %v", err)
	}
	return wire.State.Lifecycle
}

// --- R14: runtime-loss recovery ---

// lifecycleSeedToolDefinition is the one tool definition every seeded
// recovery fixture captures.
const lifecycleSeedToolDefinition = `{"name":"echo","description":"echoes","parameters":{"type":"object"}}`

// lifecycleInsertRegister inserts one register payload directly through the
// storage contract.
func lifecycleInsertRegister(t *testing.T, store harness.Storage, key harness.RegisterKey, payload string) {
	t.Helper()
	if err := store.Transact(context.Background(), func(tx harness.Transaction) error {
		_, err := tx.InsertRegister(harness.RegisterDraft{Key: key, Payload: json.RawMessage(payload)})
		return err
	}); err != nil {
		t.Fatalf("insert register %v: %v", key, err)
	}
}

// lifecycleInsertEntry inserts one entry payload directly through the
// storage contract. The addressed Session must already exist.
func lifecycleInsertEntry(t *testing.T, store harness.Storage, sessionID, id, operationID string, kind harness.EntryKind, payload string) {
	t.Helper()
	if err := store.Transact(context.Background(), func(tx harness.Transaction) error {
		_, err := tx.InsertEntry(harness.EntryDraft{SessionID: sessionID, ID: id, OperationID: operationID, Kind: kind, Payload: json.RawMessage(payload)})
		return err
	}); err != nil {
		t.Fatalf("insert entry %s: %v", id, err)
	}
}

// seedLifecycleRoot inserts one idle open root Session register directly
// through the storage contract.
func seedLifecycleRoot(t *testing.T, store harness.Storage, sessionID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload := fmt.Sprintf(
		`{"identity":{"session_id":%q,"workspace":"/tmp/works","created_at":%q},`+
			`"state":{"lifecycle":"open","current_agent_type":"solo","usage":{"by_model":[]},"last_activity":%q}}`,
		sessionID, now, now)
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}, payload)
}

// seedLifecycleChild inserts one valid child Session with a running
// Operation directly through the storage contract — the quiescent durable
// state a process loss leaves behind, with no live Harness or goroutine
// retaining write access.
func seedLifecycleChild(t *testing.T, store harness.Storage, parentID, childID, operationID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	sessionPayload := fmt.Sprintf(
		`{"identity":{"session_id":%q,"workspace":"/tmp/works","created_at":%q,"parent_session_id":%q},`+
			`"state":{"lifecycle":"open","current_agent_type":"worker","current_operation_id":%q,"usage":{"by_model":[]},"last_activity":%q}}`,
		childID, now, parentID, operationID, now)
	entryID := newLifecycleID(t)
	inputPayload := fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"origin":"plugin","content":[{"kind":"text","text":"child work"}]}`,
		childID, entryID, operationID)
	admission := fmt.Sprintf(
		`{"session_id":%q,"operation_id":%q,"request_kind":"message",`+
			`"admitted_entry":{"session_id":%q,"entry_id":%q},"agent_type":"worker",`+
			`"execution":{"configuration_revision":"rev-1","model":{"provider":"prov","model":"gpt-x"},`+
			`"system_prompt":"system","tools":[%s],"readonly":false,"write_dir":""},"admitted_at":%q}`,
		childID, operationID, childID, entryID, lifecycleSeedToolDefinition, now)
	operationPayload := fmt.Sprintf(
		`{"admission":%s,"state":{"status":"running","started_at":%q,"pending_tool_calls":[],"usage":{"by_model":[]}}}`,
		admission, now)
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: childID, Kind: harness.RegisterSession}, sessionPayload)
	lifecycleInsertEntry(t, store, childID, entryID, operationID, harness.EntryInput, inputPayload)
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: childID, Kind: harness.RegisterOperation, OperationID: operationID}, operationPayload)
}

// recordingJobs is the fresh recording fake Jobs instance: the Runtime calls
// only StopJob, and the discarded-versus-fresh probe needs Reserve and Read.
// It spawns no OS process.
type recordingJobs struct {
	mu      sync.Mutex
	calls   int
	records map[string]string
}

func newRecordingJobs() *recordingJobs {
	return &recordingJobs{records: make(map[string]string)}
}

func (j *recordingJobs) callCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls
}

func (j *recordingJobs) Reserve() (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls++
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf[:])
	j.records[id] = "reserved"
	return id, nil
}

func (j *recordingJobs) Read(_, jobID string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls++
	if j.records[jobID] == "" {
		return "", fmt.Errorf("process: no process with ID %q", jobID)
	}
	return "recorded output", nil
}

func (j *recordingJobs) StopJob(string, string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls++
}

// TestBackgroundLifecycleRuntimeLossRecovery proves the runtime-loss row
// through a quiescent durable fixture, never managed cancellation: recovery
// settles the durable running child Operation as interruption with the
// runtime-loss detail, retains the child's entries, records no parent
// completion, invokes no model, tool, or job during recovery, reconstructs
// no member blocking the parent lifecycle, admits the parent normally
// afterwards, and a re-run changes nothing. A fresh fake Jobs instance holds
// no records for a discarded instance's identities and performs no discovery
// or adoption calls.
func TestBackgroundLifecycleRuntimeLossRecovery(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		parent := newLifecycleID(t)
		child := newLifecycleID(t)
		childOp := newLifecycleID(t)
		seedLifecycleRoot(t, store, parent)
		seedLifecycleChild(t, store, parent, child, childOp)
		childEntriesBefore := snapshotEntryIDs(t, store, child)
		parentRegistersBefore := registerSnapshot(t, store, parent)

		if err := harness.Recover(ctx, store); err != nil {
			t.Fatalf("Recover: %v", err)
		}

		// The child's running Operation settled as the runtime-loss
		// interruption; its current Operation cleared.
		status, detail := operationRegisterTerminal(t, store, child, childOp)
		if status != harness.OperationInterruption || detail != "Operation interrupted by Runtime loss." {
			t.Fatalf("recovered child operation = (%q, %q), want the runtime-loss interruption", status, detail)
		}
		if got := sessionCurrentOperation(t, store, child); got != "" {
			t.Fatalf("recovered child current operation = %q, want cleared", got)
		}
		if ids := snapshotEntryIDs(t, store, child); !containsAll(ids, childEntriesBefore) {
			t.Fatalf("recovered child entries %v lost the seeded prefix %v", ids, childEntriesBefore)
		}
		// The parent recorded no completion and its registers are unchanged.
		if completions := lifecycleCompletionsOf(t, store, parent); len(completions) != 0 {
			t.Fatalf("parent recorded %d completion entries, want none", len(completions))
		}
		assertRegistersUnchanged(t, parentRegistersBefore, registerSnapshot(t, store, parent))

		// A fresh Harness over the recovered state with a fresh recording
		// fake Jobs instance: recovery-time composition invokes nothing.
		fresh := newRecordingJobs()
		bg := openBackgroundLifecycle(t, store, fresh)
		if got := bg.prep.requestCount(); got != 0 {
			t.Fatalf("preparation calls during recovery = %d, want none", got)
		}
		if got := bg.prep.totalModelCalls(); got != 0 {
			t.Fatalf("model invocations during recovery = %d, want none", got)
		}
		if got := fresh.callCount(); got != 0 {
			t.Fatalf("jobs invocations during recovery = %d, want none", got)
		}

		// No reconstructed member blocks the parent lifecycle, and the
		// parent admits new work normally.
		submitThroughRuntime(t, bg.r, parent, "op-after-loss", "continue")
		awaitOperation(t, bg.r, parent, "op-after-loss", harness.OperationSuccess)
		err := bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ArchiveSession(ctx, parent)
			return err
		})
		if err != nil {
			t.Fatalf("ArchiveSession on the recovered parent = %v, want no reconstructed member", err)
		}

		// A re-run of recovery changes nothing: registers AND the full
		// entry list stay byte-identical on both sessions.
		childRegistersBefore := registerSnapshot(t, store, child)
		childEntriesSnapshot := entrySnapshot(t, store, child)
		parentRegistersAfterAdmission := registerSnapshot(t, store, parent)
		parentEntriesSnapshot := entrySnapshot(t, store, parent)
		if err := harness.Recover(ctx, store); err != nil {
			t.Fatalf("second Recover: %v", err)
		}
		assertRegistersUnchanged(t, childRegistersBefore, registerSnapshot(t, store, child))
		assertEntriesUnchanged(t, childEntriesSnapshot, entrySnapshot(t, store, child))
		assertRegistersUnchanged(t, parentRegistersAfterAdmission, registerSnapshot(t, store, parent))
		assertEntriesUnchanged(t, parentEntriesSnapshot, entrySnapshot(t, store, parent))

		if err := bg.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// A fresh instance has no records for a discarded instance's
		// identities and performs no discovery or adoption calls.
		discarded := newRecordingJobs()
		if _, err := discarded.Reserve(); err != nil {
			t.Fatalf("discarded Reserve: %v", err)
		}
		discardedID := ""
		discarded.mu.Lock()
		for id := range discarded.records {
			discardedID = id
		}
		discarded.mu.Unlock()
		if _, err := fresh.Read(parent, discardedID); err == nil || !strings.Contains(err.Error(), "no process with ID") {
			t.Fatalf("fresh Read of the discarded identity = %v, want the unknown record", err)
		}
		if got := fresh.callCount(); got != 1 { // the one Read above is the test's own probe
			t.Fatalf("fresh instance calls = %d, want only the test's own probe", got)
		}
	})
}

// snapshotEntryIDs captures one Session's committed entry identities.
func snapshotEntryIDs(t *testing.T, store harness.Storage, sessionID string) []string {
	t.Helper()
	entries, err := store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("ReadEntries(%s): %v", sessionID, err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	return ids
}

func containsAll(ids, want []string) bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	for _, id := range want {
		if !set[id] {
			return false
		}
	}
	return true
}

// --- R15: managed shutdown ---

// TestBackgroundLifecycleManagedShutdown proves the managed-shutdown row:
// Close interrupts the active work, the live job is stopped through StopJob
// with its completion recorded as the durable signal entry while the shutdown
// still converges, and the queued input buffered before the shutdown starts
// produces no second Agent or model call.
func TestBackgroundLifecycleManagedShutdown(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		t.Run("close interrupts active work and stops the live job", func(t *testing.T) {
			stopper := newLifecycleStopper(store)
			bg := openBackgroundLifecycle(t, store, stopper)
			stopper.arm(bg.r.harness)
			session := bg.session("shutdown-session")
			modelStarted := make(chan struct{}, 1)
			bg.prep.setSessionScript(session, &lifecycleScript{
				model: func(ctx context.Context, _ string, _ int, _ model.Request) (model.Stream, error) {
					modelStarted <- struct{}{}
					<-ctx.Done()
					return nil, ctx.Err()
				},
			})
			submitThroughRuntime(t, bg.r, session, "op-1", "run")
			<-modelStarted
			if got := bg.submitMode(session, "op-q", "queued message", harness.MessageModeQueued); got != harness.DispositionQueued {
				t.Fatalf("queued submit = %q, want queued", got)
			}
			completionID := bg.startJob(session, "12340001")
			stopper.addDelivery(session, completionID, "job report")

			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// The active work settled as the contract interruption.
			status, detail := operationRegisterTerminal(t, store, session, "op-1")
			if status != harness.OperationInterruption || detail != "agent interrupted" {
				t.Fatalf("shutdown operation = (%q, %q), want the interrupted settlement", status, detail)
			}
			// The queued input produced no second model call and no Operation.
			if got := bg.prep.modelCallCount(session); got != 1 {
				t.Fatalf("model calls after shutdown = %d, want only the interrupted attempt", got)
			}
			if _, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: session, Kind: harness.RegisterOperation, OperationID: "op-q"}); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("the queued input was admitted (%v), want no work started after shutdown began", err)
			}
			// The job was stopped through StopJob with its completion recorded.
			if got := stopper.stopJobCount(); got != 1 {
				t.Fatalf("StopJob calls = %d, want the live job stopped once", got)
			}
			if !stopper.entryCommittedEarly() {
				t.Fatal("the job completion signal entry was not committed while the shutdown still converged")
			}
			completions := lifecycleCompletionsOf(t, store, session)
			if len(completions) != 1 || completions[0].memberKind != "job" || completions[0].memberID != "12340001" {
				t.Fatalf("shutdown completion entries = %+v, want the killed job's one entry", completions)
			}
		})

		t.Run("the stopped job's completion commits before the shutdown wait converges", func(t *testing.T) {
			gated := newGatedStore(store)
			stopper := newLifecycleStopper(gated)
			bg := openBackgroundLifecycle(t, gated, stopper)
			stopper.arm(bg.r.harness)
			root := bg.session("convergence-root")
			modelParked := make(chan struct{}, 1)
			modelGate := make(chan struct{})
			// The successor run parks its model call outside the shutdown
			// context: cancelWork does not retire it, so only the test's
			// gate ends the run — the wait must block until then.
			bg.prep.setSessionScript(root, &lifecycleScript{
				model: func(ctx context.Context, _ string, _ int, _ model.Request) (model.Stream, error) {
					select {
					case modelParked <- struct{}{}:
					default:
					}
					<-modelGate
					return nil, ctx.Err()
				},
			})
			j1 := bg.startJob(root, "12340011")
			if err := bg.bridge.DeliverCompletion(ctx, root, j1, "first report"); err != nil {
				t.Fatalf("J1 delivery: %v", err)
			}
			select { // the idle admission installed successor execution O1 and parked it
			case <-modelParked:
			case <-time.After(10 * time.Second):
				t.Fatal("the successor execution never installed its model call")
			}

			j2 := bg.startJob(root, "12340012")
			stopper.addDelivery(root, j2, "second report")

			closeDone := make(chan error, 1)
			go func() { closeDone <- bg.r.Close(ctx) }()

			// StopAll stops J2 through StopJob: the closed completion
			// commits while O1's run stays behind the parked model.
			deadline := time.Now().Add(10 * time.Second)
			for {
				committed := false
				for _, completion := range completionsOf(gated, root) {
					if completion.content == "second report" {
						committed = true
					}
				}
				if committed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the stopped job's completion never committed before the shutdown wait")
				}
				time.Sleep(5 * time.Millisecond)
			}

			// O1's terminal settlement is the one armed post-commit park:
			// its committed settlement cannot return, so the run stays
			// unconverged and the shutdown cannot pass harness.Wait.
			gated.armPostCommit(1)
			close(modelGate)
			select {
			case <-gated.parked:
			case <-time.After(10 * time.Second):
				t.Fatal("the successor's terminal settlement never committed")
			}

			// While held: the J2 completion is durably committed, Close has
			// not returned, and no scope has closed — only a
			// StopAll-before-Wait ordering reaches this state.
			committed := false
			for _, completion := range completionsOf(gated, root) {
				if completion.content == "second report" {
					committed = true
				}
			}
			if !committed {
				t.Fatal("the J2 completion entry is not durably committed while the settlement is parked")
			}
			select {
			case err := <-closeDone:
				t.Fatalf("Close returned (%v) while the successor run was unconverged", err)
			default:
			}
			if closed := eventsNamedList(bg.e.events.all(), "close"); len(closed) != 0 {
				t.Fatalf("scope closures fired while the shutdown was held: %v", closed)
			}

			gated.releaseGate()
			select {
			case err := <-closeDone:
				if err != nil {
					t.Fatalf("Close: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Close never converged after the settlement was released")
			}
			closed := eventsNamedList(bg.e.events.all(), "close")
			if !orderedSubset(closed, "close:jobs", "close:core") {
				t.Fatalf("close events = %v, want the jobs capability closing before storage", closed)
			}
			status, detail := operationRegisterTerminal(t, gated, root, j1)
			if status != harness.OperationInterruption || detail != "agent interrupted" {
				t.Fatalf("successor operation = (%q, %q), want the shutdown interruption", status, detail)
			}
		})
	})
}

// --- R16: shared preparation ---

// TestBackgroundLifecycleSharedPreparation proves the shared-preparation
// row: an injected preparation failure publishes neither the child nor the
// parent member, and a successful launch's child admission carries the
// sentinel capture with the complete child identity through the one
// preparation path. Child parents refuse further launches by lineage.
func TestBackgroundLifecycleSharedPreparation(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()

		t.Run("injected Prepare failure publishes no child or member", func(t *testing.T) {
			bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
			root := bg.session("prepare-fail-root")
			injected := errors.New("test child preparation failure")
			bg.prep.setAgentScript("worker", &lifecycleScript{fail: injected})

			err := bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.LaunchChildSession(ctx, harness.LaunchChildRequest{
					ParentSessionID: root,
					AgentType:       "worker",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "child work"}},
					OperationID:     newLifecycleID(t),
					MaxConcurrent:   5,
					OutputLimit:     4096,
				})
				return err
			})
			if !errors.Is(err, injected) {
				t.Fatalf("LaunchChildSession = %v, want the injected preparation failure", err)
			}
			ids, err := store.ListSessionIDs(ctx)
			if err != nil {
				t.Fatalf("ListSessionIDs: %v", err)
			}
			if len(ids) != 1 || ids[0] != root {
				t.Fatalf("sessions after the failed launch = %v, want only the root", ids)
			}
			// No member was published: the root's lifecycle is unblocked.
			err = bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ArchiveSession(ctx, root)
				return err
			})
			if err != nil {
				t.Fatalf("ArchiveSession after the failed launch = %v, want no published member", err)
			}
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})

		t.Run("successful child admission carries the sentinel capture and complete identity", func(t *testing.T) {
			bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
			created, err := bg.r.createSession(ctx, filepath.Join(bg.e.home, "prepare-ok-root"), "solo")
			if err != nil {
				t.Fatalf("createSession: %v", err)
			}
			root := created.Identity.SessionID
			childOp := newLifecycleID(t)
			child := bg.launchChild(root, "child work", childOp, 5)

			requests := bg.prep.workerRequests()
			if len(requests) != 1 {
				t.Fatalf("worker preparations = %d, want exactly the one shared path", len(requests))
			}
			req := requests[0]
			if req.Session.Identity.SessionID != child || req.Session.Identity.ParentSessionID != root {
				t.Fatalf("prepared child identity = %+v, want the durable child with its lineage", req.Session.Identity)
			}
			if req.Session.Identity.Workspace != created.Identity.Workspace {
				t.Fatalf("prepared child identity = %+v, want the parent's workspace %q", req.Session.Identity, created.Identity.Workspace)
			}
			var childIdentity harness.SessionIdentity
			if err := bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				rec, err := h.ReadSession(ctx, child)
				if err != nil {
					return err
				}
				childIdentity = rec.Identity
				return nil
			}); err != nil {
				t.Fatalf("ReadSession(child): %v", err)
			}
			if !req.Session.Identity.CreatedAt.Equal(childIdentity.CreatedAt) {
				t.Fatalf("prepared CreatedAt = %v, want the durable child identity's %v", req.Session.Identity.CreatedAt, childIdentity.CreatedAt)
			}
			if req.Session.Identity.ParentSessionID != childIdentity.ParentSessionID {
				t.Fatalf("prepared ParentSessionID = %q, want the durable child identity's %q", req.Session.Identity.ParentSessionID, childIdentity.ParentSessionID)
			}
			if req.Session.AgentType != "worker" || req.RequestKind != harness.RequestKindMessage {
				t.Fatalf("prepared request = %+v, want the worker message kind", req)
			}
			// The sentinel capture is the durable child admission.
			rec := awaitOperation(t, bg.r, child, childOp, harness.OperationSuccess)
			if rec.Admission.Execution.SystemPrompt != "prompt-worker" {
				t.Fatalf("child admission capture prompt = %q, want the sentinel prompt-worker", rec.Admission.Execution.SystemPrompt)
			}
			if rec.Admission.SessionID != child || rec.Admission.OperationID != childOp {
				t.Fatalf("child admission identity = %+v, want the launched child", rec.Admission)
			}
			// Child parents refuse further launches by lineage.
			err = bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.LaunchChildSession(ctx, harness.LaunchChildRequest{
					ParentSessionID: child,
					AgentType:       "worker",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "grandchild"}},
					OperationID:     newLifecycleID(t),
					MaxConcurrent:   5,
					OutputLimit:     4096,
				})
				return err
			})
			if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "child sessions cannot launch child sessions") {
				t.Fatalf("launch from a child parent = %v, want the lineage refusal", err)
			}
			if err := bg.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})
}

// --- R6 negative: children are never queued at the cap ---

// TestBackgroundLifecycleChildCapNeverQueues proves the cap row: at the
// parent's child cap the next launch rejects with the exact limit text
// before any child or member exists, nothing is queued, and after the live
// child completes a new launch succeeds.
func TestBackgroundLifecycleChildCapNeverQueues(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		bg := openBackgroundLifecycle(t, store, newLifecycleStopper(store))
		root := bg.session("cap-root")
		childStarted := make(chan struct{}, 1)
		bg.prep.setAgentScript("worker", &lifecycleScript{
			model: func(ctx context.Context, _ string, _ int, _ model.Request) (model.Stream, error) {
				childStarted <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			},
		})
		firstOp := newLifecycleID(t)
		first := bg.launchChild(root, "child one", firstOp, 1)
		<-childStarted

		err := bg.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, lerr := h.LaunchChildSession(ctx, harness.LaunchChildRequest{
				ParentSessionID: root,
				AgentType:       "worker",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "child two"}},
				OperationID:     newLifecycleID(t),
				MaxConcurrent:   1,
				OutputLimit:     4096,
			})
			return lerr
		})
		if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "background group is full (1/1). Wait for a background member to complete, then retry") {
			t.Fatalf("launch at the cap = %v, want the exact cap rejection", err)
		}
		ids, err := store.ListSessionIDs(ctx)
		if err != nil {
			t.Fatalf("ListSessionIDs: %v", err)
		}
		if len(ids) != 2 {
			t.Fatalf("sessions after the capped launch = %v, want only the root and the one child", ids)
		}
		// The live child completes; a new launch succeeds — the group never
		// queued the rejected work.
		if err := bg.interrupt(first); err != nil {
			t.Fatalf("Interrupt(child): %v", err)
		}
		awaitOperation(t, bg.r, first, firstOp, harness.OperationInterruption)
		interrupted := awaitRuntimeInput(t, store, root, "Task interrupted.")
		awaitOperation(t, bg.r, root, interrupted.operationID, harness.OperationSuccess)
		thirdOp := newLifecycleID(t)
		if third := bg.launchChild(root, "child three", thirdOp, 1); third == first {
			t.Fatal("the relaunch minted the same child identity")
		}
		<-childStarted
		if err := bg.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}
