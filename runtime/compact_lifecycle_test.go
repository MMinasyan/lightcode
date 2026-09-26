package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/model"
	"github.com/pkoukk/tiktoken-go"
)

// The cross-boundary compaction verification suite: every row of the
// compaction lifecycle's final verification runs through the composed
// Runtime's public surfaces — admission, the conversation and compact model
// transports, the public manual compaction, recovery, fork, and the passive
// observation — over both the memory and SQLite stores where durable state
// matters. A controlled preparation supplies scripted conversation and
// compact transports with per-test settable capture members, so trigger
// thresholds and piece budgets are sized by arithmetic against the
// estimator's published rules rather than by accumulation.

// --- the estimator arithmetic ---

// compactEncoding loads the estimator's cl100k_base encoder once — the same
// acquisition the production estimate path's live branch performs. An
// unavailable encoder fails the fixture fast: the fixture's arithmetic must
// match the live path, and the production fallback has its own seam tests.
var (
	compactEncodingOnce sync.Once
	compactEncodingRef  *tiktoken.Tiktoken
)

func compactEncoding() *tiktoken.Tiktoken {
	compactEncodingOnce.Do(func() {
		enc, err := tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			panic(err)
		}
		compactEncodingRef = enc
	})
	return compactEncodingRef
}

// compactEstimate mirrors the harness conversation estimator's rules over
// model messages: per message, the encoded text content plus each tool
// call's name and raw argument bytes, plus the flat per-message overhead.
func compactEstimate(messages []model.Message) int {
	enc := compactEncoding()
	total := 0
	for _, m := range messages {
		total += len(enc.Encode(m.TextContent(), nil, nil))
		for _, tc := range m.ToolCalls {
			total += len(enc.Encode(tc.Name, nil, nil))
			total += len(enc.Encode(string(tc.Arguments), nil, nil))
		}
		total += 4
	}
	return total
}

// compactEncode estimates one plain text the same way.
func compactEncode(text string) int {
	return len(compactEncoding().Encode(text, nil, nil))
}

// compactTextWithTokens returns text whose encoded token count is at least
// want, built by geometrically growing repetition of the unit: the encode
// cost stays linear in the result, never a full rescan per step.
func compactTextWithTokens(unit string, want int) string {
	text := ""
	step := unit
	for compactEncode(text) < want {
		text += step
		step += step
	}
	return text
}

// sysMessageText, userMessageText, and assistantMessageText build the
// estimator's input shapes for the projection the fixture drives.
func sysMessageText(text string) model.Message {
	return model.Message{Role: model.RoleSystem, Content: []model.ContentPart{{Kind: model.PartText, Text: text}}}
}

func userMessageText(text string) model.Message {
	return model.Message{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: text}}}
}

func assistantMessageText(text string) model.Message {
	return model.Message{Role: model.RoleAssistant, Source: model.ModelRef{Provider: "prov", Model: "m"}, Content: []model.ContentPart{{Kind: model.PartText, Text: text}}}
}

// compactWindowFor sizes the conversation window against the request's
// estimate-plus-reserve threshold: one below it overflows the trigger's
// strict inequality, exactly at it leaves the request fitting.
func compactWindowFor(messages []model.Message, reserve int, overflow bool) int {
	threshold := compactEstimate(messages) + reserve
	if overflow {
		return threshold - 1
	}
	return threshold
}

// compactSeedInput is the seeded user text: enough estimate headroom that the
// summary-framed rebuilt request fits the overflowing window with the small
// output reserve the trigger rows size.
func compactSeedInput() string {
	return "hello compaction " + compactTextWithTokens("seed ", 200)
}

// --- the controlled preparation ---

// compactScript is one transport's scripted closure.
type compactScript func(ctx context.Context, req model.Request) (model.Stream, error)

// compactLifecyclePrep is the controlled preparation of the compaction suite: the
// capture's window, reserve, system prompt, and compact members are settable
// per test phase, and the opener binds the conversation and compact
// transports as separate scripted closures over the same registry.
type compactLifecyclePrep struct {
	mu sync.Mutex

	convWindow   int
	convReserve  int
	systemPrompt string
	compact      harness.CompactCapture
	retry        harness.RetryPolicy
	toolResult   string

	convScripts    map[string]compactScript
	compactScripts map[string]compactScript
}

func newCompactLifecyclePrep() *compactLifecyclePrep {
	return &compactLifecyclePrep{
		convWindow:   1 << 30,
		convReserve:  1024,
		systemPrompt: "compact-sys",
		compact: harness.CompactCapture{
			Model:         compactConversationRef,
			ContextWindow: 1 << 30,
			OutputReserve: 1024,
			SystemPrompt:  "summarize",
		},
		toolResult:     "ok",
		convScripts:    map[string]compactScript{},
		compactScripts: map[string]compactScript{},
	}
}

func (p *compactLifecyclePrep) setCapture(convWindow, convReserve int, systemPrompt string, compact harness.CompactCapture) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.convWindow, p.convReserve, p.systemPrompt, p.compact = convWindow, convReserve, systemPrompt, compact
}

func (p *compactLifecyclePrep) setRetry(retry harness.RetryPolicy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retry = retry
}

func (p *compactLifecyclePrep) setToolResult(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.toolResult = text
}

func (p *compactLifecyclePrep) setConvScript(sessionID string, fn compactScript) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.convScripts[sessionID] = fn
}

func (p *compactLifecyclePrep) setCompactScript(sessionID string, fn compactScript) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.compactScripts[sessionID] = fn
}

func (p *compactLifecyclePrep) prepare(_ context.Context, req harness.PreparationRequest, sel selection) (harness.ExecutionCapture, openExecution, error) {
	p.mu.Lock()
	convWindow, convReserve := p.convWindow, p.convReserve
	systemPrompt, compact := p.systemPrompt, p.compact
	p.mu.Unlock()
	capture := harness.ExecutionCapture{
		ConfigurationRevision: sel.invocation.Revision(),
		Model:                 sel.agent.Model,
		ContextWindow:         convWindow,
		OutputReserve:         convReserve,
		SystemPrompt:          systemPrompt,
		Tools:                 captureTools([]string{"noop"}),
		Compact:               compact,
	}
	return capture, p.opener(req.Session.Identity.SessionID), nil
}

func (p *compactLifecyclePrep) opener(sessionID string) openExecution {
	return func(_ context.Context, admission harness.OperationAdmission, _ selection) (harness.Execution, error) {
		p.mu.Lock()
		conv, compactFn, retry, toolResult := p.convScripts[sessionID], p.compactScripts[sessionID], p.retry, p.toolResult
		p.mu.Unlock()
		return harness.Execution{
			Model: func(mctx context.Context, req model.Request) (model.Stream, error) {
				if conv == nil {
					return lifecycleTextTurn("done"), nil
				}
				return conv(mctx, req)
			},
			CompactModel: func(mctx context.Context, req model.Request) (model.Stream, error) {
				if compactFn == nil {
					return compactSummaryTurn("summary", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
				}
				return compactFn(mctx, req)
			},
			Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
				return harness.PreparedTool{
					Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}},
					Immediate: &harness.ToolOutcome{Result: model.ToolResult{
						CallID: call.ID, Status: model.ResultSuccess, Content: toolResult,
					}},
				}
			},
			NormalizeTool: runtimeNormalize,
			Retry:         retry,
		}, nil
	}
}

// --- the scripted streams ---

// compactSummaryTurn is one completed summary turn carrying reported usage.
func compactSummaryTurn(text string, usage model.Usage) model.Stream {
	return &lifecycleTurn{delta: model.StreamDelta{
		HasChoice:        true,
		Role:             "assistant",
		ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: text}},
		FinishReason:     "stop",
		Usage:            &usage,
	}}
}

// compactRefusalTurn is one completed refusal-only turn.
func compactRefusalTurn(refusal string) model.Stream {
	return &lifecycleTurn{delta: model.StreamDelta{
		HasChoice:       true,
		Role:            "assistant",
		RefusalFragment: refusal,
		FinishReason:    "stop",
	}}
}

// compactPartialStream is one stream that yields a text fragment and then
// fails: the assembly finalizes an errored output retaining the partial
// payload, which continues the Operation through the failure-continuation
// signal.
type compactPartialStream struct {
	sent bool
	fail error
}

func (s *compactPartialStream) Recv() (model.StreamDelta, error) {
	if !s.sent {
		s.sent = true
		return model.StreamDelta{
			HasChoice:        true,
			Role:             "assistant",
			ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "partial answer"}},
		}, nil
	}
	return model.StreamDelta{}, s.fail
}

func (s *compactPartialStream) Close() error { return nil }

// compactRichTurn is one completed turn carrying text, an opaque reasoning
// part, a non-text part, and a tool call.
func compactRichTurn() model.Stream {
	position := 0
	return &lifecycleTurn{delta: model.StreamDelta{
		HasChoice: true,
		Role:      "assistant",
		ContentFragments: []model.ContentFragment{
			{Position: 0, Kind: model.PartText, Text: "working"},
			{Position: 1, Kind: model.PartOpaque, OpaqueWireType: "thinking"},
			{Position: 2, Kind: model.PartImageURL, URL: "https://img.test/p.png"},
		},
		ToolFragments: []model.ToolCallFragment{{Position: &position, ID: "call-rich", Name: "noop", ArgumentFragment: `{"x":1}`}},
		FinishReason:  "tool_calls",
	}}
}

// --- the fixture ---

// compactLifecycle is one composed Runtime over the controlled compaction
// preparation.
type compactLifecycle struct {
	t     *testing.T
	store harness.Storage
	prep  *compactLifecyclePrep
	r     *Runtime
}

func openCompactLifecycle(t *testing.T, store harness.Storage) *compactLifecycle {
	t.Helper()
	ctx := context.Background()
	e := newOwnerEnv(t)
	writeServiceFile(t, agents.PathForConfig(e.configPath), lifecycleAgentsDocument)
	prep := newCompactLifecyclePrep()
	opts := options{
		DataDir:    e.dataDir,
		ConfigPath: e.configPath,
		Plugins:    []Plugin{e.storagePlugin(store), jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, newLifecycleStopper(store))},
		prepare:    prep.prepare,
		// A never-ticked sweep stream keeps the automatic lifecycle sweep
		// structurally out of every row's transaction sequence.
		sweepTicks: make(chan time.Time),
	}
	r, err := open(ctx, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return &compactLifecycle{t: t, store: store, prep: prep, r: r}
}

func (f *compactLifecycle) session(name string) string {
	f.t.Helper()
	rec, err := f.r.createSession(context.Background(), "/tmp/compact-"+name, "solo")
	if err != nil {
		f.t.Fatalf("createSession(%s): %v", name, err)
	}
	return rec.Identity.SessionID
}

func (f *compactLifecycle) setCapture(convWindow, convReserve int, systemPrompt string, compact harness.CompactCapture) {
	f.t.Helper()
	f.prep.setCapture(convWindow, convReserve, systemPrompt, compact)
}

func (f *compactLifecycle) setConvScript(sessionID string, fn compactScript) {
	f.t.Helper()
	f.prep.setConvScript(sessionID, fn)
}

func (f *compactLifecycle) setCompactScript(sessionID string, fn compactScript) {
	f.t.Helper()
	f.prep.setCompactScript(sessionID, fn)
}

func (f *compactLifecycle) setToolResult(text string) {
	f.t.Helper()
	f.prep.setToolResult(text)
}

// submit submits one regular user message. When the Session sits in the
// terminal-to-retirement window of a just-settled Operation, the ordinary
// active routing buffers the message and the post-terminal drain delivers it
// — the durable admission itself is the rendezvous, so the helper waits for
// the Operation register instead of assuming the disposition.
func (f *compactLifecycle) submit(sessionID, operationID, text string) {
	f.t.Helper()
	buffered := false
	err := f.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		res, err := h.Submit(ctx, harness.SubmitRequest{
			SessionID:   sessionID,
			OperationID: operationID,
			Origin:      harness.InputOriginUser,
			Content:     []model.ContentPart{{Kind: model.PartText, Text: text}},
			Mode:        harness.MessageModeRegular,
		})
		if err != nil {
			return err
		}
		if res.Disposition == harness.DispositionAdmitted {
			return nil
		}
		buffered = true // the tolerated buffered dispositions
		return nil
	})
	if err != nil {
		f.t.Fatalf("Submit(%s): %v", operationID, err)
	}
	if !buffered {
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var admitted bool
		err := f.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ReadOperation(ctx, sessionID, operationID)
			admitted = err == nil
			return nil
		})
		if err == nil && admitted {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("submit %q stayed buffered and was never delivered", operationID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *compactLifecycle) submitMode(sessionID, operationID, text string, mode harness.MessageMode) harness.SubmitDisposition {
	f.t.Helper()
	var disposition harness.SubmitDisposition
	err := f.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
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
		f.t.Fatalf("Submit(%s): %v", operationID, err)
	}
	return disposition
}

// compact admits one manual compaction, returning the admission error as-is
// for the rejection rows.
func (f *compactLifecycle) compact(sessionID, operationID string) (harness.OperationRecord, error) {
	f.t.Helper()
	var rec harness.OperationRecord
	err := f.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		rec, err = h.Compact(ctx, harness.CompactRequest{SessionID: sessionID, OperationID: operationID})
		return err
	})
	return rec, err
}

// compactIdle admits one manual compaction on an idle Session: the
// terminal-to-retirement window clears asynchronously, and the admission
// itself is the rendezvous on the retired run — a rejection committed
// nothing, so the retry is safe.
func (f *compactLifecycle) compactIdle(sessionID, operationID string) harness.OperationRecord {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := f.compact(sessionID, operationID)
		if err == nil {
			return rec
		}
		if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "is not idle") || time.Now().After(deadline) {
			f.t.Fatalf("Compact(%s): %v", operationID, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *compactLifecycle) readSession(sessionID string) harness.SessionRecord {
	f.t.Helper()
	var rec harness.SessionRecord
	err := f.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		rec, err = h.ReadSession(ctx, sessionID)
		return err
	})
	if err != nil {
		f.t.Fatalf("ReadSession(%s): %v", sessionID, err)
	}
	return rec
}

func (f *compactLifecycle) readOperation(sessionID, operationID string) harness.OperationRecord {
	f.t.Helper()
	var rec harness.OperationRecord
	err := f.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		rec, err = h.ReadOperation(ctx, sessionID, operationID)
		return err
	})
	if err != nil {
		f.t.Fatalf("ReadOperation(%s): %v", operationID, err)
	}
	return rec
}

func (f *compactLifecycle) entries(sessionID string) []harness.Entry {
	f.t.Helper()
	entries, err := f.store.ReadEntries(context.Background(), sessionID, 0)
	if err != nil {
		f.t.Fatalf("ReadEntries(%s): %v", sessionID, err)
	}
	return entries
}

// compactionIDOf reads one Session register's compaction_entry_id without
// failing: the transport scripts run on execution goroutines.
func compactionIDOf(store harness.Storage, sessionID string) (string, error) {
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	if err != nil {
		return "", err
	}
	var wire struct {
		State struct {
			CompactionEntryID string `json:"compaction_entry_id"`
		} `json:"state"`
	}
	if err := json.Unmarshal(reg.Payload, &wire); err != nil {
		return "", err
	}
	return wire.State.CompactionEntryID, nil
}

// --- shared assertion helpers ---

// compactMessageShape is one model message's model-visible shape.
type compactMessageShape struct {
	role    string
	text    string
	source  model.ModelRef
	calls   int
	toolID  string
	refusal string
}

func compactShapeOf(msg model.Message) compactMessageShape {
	shape := compactMessageShape{role: string(msg.Role), text: msg.TextContent(), source: msg.Source, refusal: msg.Refusal}
	for _, call := range msg.ToolCalls {
		shape.calls++
		shape.toolID = call.ID
	}
	return shape
}

func compactShapesOf(msgs []model.Message) []compactMessageShape {
	out := make([]compactMessageShape, 0, len(msgs))
	for _, msg := range msgs {
		out = append(out, compactShapeOf(msg))
	}
	return out
}

func assertCompactShapes(t *testing.T, got, want []compactMessageShape) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("message shapes = %d messages %+v, want %d: %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("message %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// compactWirePart is one serialized content part's decoded shape; the named
// type keeps whole-slice comparisons expressible.
type compactWirePart struct {
	Kind           string `json:"Kind"`
	Text           string `json:"Text"`
	URL            string `json:"URL"`
	OpaqueWireType string `json:"OpaqueWireType"`
}

// compactWireMessage is one serialized compaction-input line's decoded shape.
type compactWireMessage struct {
	Role       string            `json:"Role"`
	Refusal    string            `json:"Refusal"`
	ToolCallID string            `json:"ToolCallID"`
	Content    []compactWirePart `json:"Content"`
	ToolCalls  []struct {
		ID        string `json:"ID"`
		Name      string `json:"Name"`
		Arguments string `json:"Arguments"`
	} `json:"ToolCalls"`
}

// compactPieceMessages decodes one piece's user text into its serialized
// message lines: every line must be one complete message object.
func compactPieceMessages(t *testing.T, userText string) []compactWireMessage {
	t.Helper()
	var out []compactWireMessage
	for _, line := range strings.Split(userText, "\n") {
		var msg compactWireMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("serialized line %q is not one complete message: %v", line, err)
		}
		out = append(out, msg)
	}
	return out
}

// compactWireRef is one durable model identity object.
type compactWireRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// compactWireEntry is one committed compaction entry's decoded payload.
type compactWireEntry struct {
	SessionID             string              `json:"session_id"`
	EntryID               string              `json:"entry_id"`
	OperationID           string              `json:"operation_id"`
	Summary               string              `json:"summary"`
	BoundaryEntryID       string              `json:"boundary_entry_id"`
	Model                 compactWireRef      `json:"model"`
	ConfigurationRevision string              `json:"configuration_revision"`
	Usage                 *harness.UsageCount `json:"usage"`
}

func compactCompactionEntriesOf(t *testing.T, store harness.Storage, sessionID string) []compactWireEntry {
	t.Helper()
	var out []compactWireEntry
	for _, entry := range entrySnapshot(t, store, sessionID) {
		if entry.Kind != harness.EntryCompaction {
			continue
		}
		var wire compactWireEntry
		if err := json.Unmarshal(entry.Payload, &wire); err != nil {
			t.Fatalf("decode compaction entry %s: %v", entry.ID, err)
		}
		out = append(out, wire)
	}
	return out
}

// compactUsageFor returns one model's total inside a register's usage.
func compactUsageFor(t *testing.T, totals harness.UsageTotals, ref model.ModelRef) harness.UsageCount {
	t.Helper()
	for _, mu := range totals.ByModel {
		if mu.Model == ref {
			return mu.Usage
		}
	}
	t.Fatalf("usage totals %+v carry no entry for model %s", totals, ref.String())
	return harness.UsageCount{}
}

// compactWireTotals is one register's decoded usage totals: the durable
// model identity is an object, not the string form model.ModelRef parses.
type compactWireTotals struct {
	ByModel []struct {
		Model compactWireRef     `json:"model"`
		Usage harness.UsageCount `json:"usage"`
	} `json:"by_model"`
}

func (w compactWireTotals) countFor(provider, name string) (harness.UsageCount, bool) {
	for _, mu := range w.ByModel {
		if mu.Model == (compactWireRef{Provider: provider, Model: name}) {
			return mu.Usage, true
		}
	}
	return harness.UsageCount{}, false
}

// compactSessionRegisterState reads one Session register's current
// Operation, projection field, and usage totals straight from the store.
func compactSessionRegisterState(t *testing.T, store harness.Storage, sessionID string) (currentOperationID, compactionEntryID string, usage compactWireTotals) {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	if err != nil {
		t.Fatalf("ReadRegister(session %s): %v", sessionID, err)
	}
	var wire struct {
		State struct {
			CurrentOperationID string            `json:"current_operation_id"`
			CompactionEntryID  string            `json:"compaction_entry_id"`
			Usage              compactWireTotals `json:"usage"`
		} `json:"state"`
	}
	if err := json.Unmarshal(reg.Payload, &wire); err != nil {
		t.Fatalf("decode session register: %v", err)
	}
	return wire.State.CurrentOperationID, wire.State.CompactionEntryID, wire.State.Usage
}

// compactOperationRegisterState reads one Operation register's status and
// usage totals straight from the store.
func compactOperationRegisterState(t *testing.T, store harness.Storage, sessionID, operationID string) (harness.OperationState, compactWireTotals, *struct {
	Detail string `json:"detail"`
}) {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID})
	if err != nil {
		t.Fatalf("ReadRegister(operation %s): %v", operationID, err)
	}
	var wire struct {
		State struct {
			Status   harness.OperationState `json:"status"`
			Usage    compactWireTotals      `json:"usage"`
			Terminal *struct {
				Detail string `json:"detail"`
			} `json:"terminal"`
		} `json:"state"`
	}
	if err := json.Unmarshal(reg.Payload, &wire); err != nil {
		t.Fatalf("decode operation register: %v", err)
	}
	return wire.State.Status, wire.State.Usage, wire.State.Terminal
}

// compactConversationRef is the fixture's conversation and compact model.
var compactConversationRef = model.ModelRef{Provider: "prov", Model: "m"}

// compactRequestRecords records one transport's seen requests with call
// counts.
type compactRequestRecords struct {
	mu   sync.Mutex
	reqs []model.Request
}

func (r *compactRequestRecords) add(req model.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
}

func (r *compactRequestRecords) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *compactRequestRecords) at(i int) model.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[i]
}

// --- the trigger rows ---

// TestCompactLifecycleTriggerRows proves the pre-request trigger rows through
// the composed Runtime: a fitting request sends uncompacted; an overflowing
// request compacts first and sends against the summary projection with the
// usage attributed; a later overflow in the same Operation commits a second
// entry; an automatic compaction in a launched child Session runs through the
// same shared path; and no passive event fires across a compaction.
func TestCompactLifecycleTriggerRows(t *testing.T) {
	t.Run("fitting request sends uncompacted", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			f := openCompactLifecycle(t, store)
			s := f.session("fitting")
			f.submit(s, "op-1", compactSeedInput())
			awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
			awaitIdleSession(t, f.r, s)

			messages := []model.Message{
				sysMessageText("compact-sys"),
				userMessageText(compactSeedInput()),
				assistantMessageText("done"),
				userMessageText("second question"),
			}
			f.setCapture(compactWindowFor(messages, 64, false), 64, "compact-sys", harness.CompactCapture{
				Model: compactConversationRef, ContextWindow: 1 << 20, OutputReserve: 1024, SystemPrompt: "summarize",
			})
			var sent compactRequestRecords
			var compacted compactRequestRecords
			f.setConvScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
				sent.add(req)
				return lifecycleTextTurn("done again"), nil
			})
			f.setCompactScript(s, func(context.Context, model.Request) (model.Stream, error) {
				compacted.add(model.Request{})
				return nil, errors.New("the compact transport ran for a fitting request")
			})
			f.submit(s, "op-2", "second question")
			awaitOperation(t, f.r, s, "op-2", harness.OperationSuccess)

			if sent.count() != 1 {
				t.Fatalf("conversation transport calls = %d, want exactly one", sent.count())
			}
			if compacted.count() != 0 {
				t.Fatalf("compact transport calls = %d, want none for a fitting request", compacted.count())
			}
			want := []compactMessageShape{
				{role: "system", text: "compact-sys"},
				{role: "user", text: compactSeedInput()},
				{role: "assistant", text: "done", source: compactConversationRef},
				{role: "user", text: "second question"},
			}
			assertCompactShapes(t, compactShapesOf(sent.at(0).Messages), want)
			if got := len(sent.at(0).Tools); got != 1 {
				t.Fatalf("sent tools = %d, want the advertised noop definition", got)
			}
			if ids := compactCompactionEntriesOf(t, store, s); len(ids) != 0 {
				t.Fatalf("compaction entries = %+v, want none for a fitting request", ids)
			}
			if id, err := compactionIDOf(store, s); err != nil || id != "" {
				t.Fatalf("compaction_entry_id = (%q, %v), want it empty on a fitting request", id, err)
			}
			if err := f.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})

	t.Run("overflow compacts then sends against the summary projection", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			gated := newGatedStore(store)
			f := openCompactLifecycle(t, gated)
			s := f.session("overflow")
			f.submit(s, "op-1", compactSeedInput())
			awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
			awaitIdleSession(t, f.r, s)

			messages := []model.Message{
				sysMessageText("compact-sys"),
				userMessageText(compactSeedInput()),
				assistantMessageText("done"),
				userMessageText("second question"),
			}
			f.setCapture(compactWindowFor(messages, 64, true), 64, "compact-sys", harness.CompactCapture{
				Model: compactConversationRef, ContextWindow: 1 << 20, OutputReserve: 1024, SystemPrompt: "summarize",
			})
			// The post-compaction rebuilt request and the later fitting
			// probe must both fit the overflowing window: arithmetic, not
			// accumulation.
			summarized := []model.Message{
				sysMessageText("compact-sys"),
				{Role: model.RoleAssistant, Source: compactConversationRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]"}}},
				userMessageText("tail probe"),
			}
			if fits := compactEstimate(summarized) + 64; fits > compactWindowFor(messages, 64, true) {
				t.Fatalf("fixture precondition: the summarized request %d tokens does not fit the window", fits)
			}
			var sent compactRequestRecords
			var pieces compactRequestRecords
			f.setConvScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
				// Rendezvous on the durable effect: the compaction must
				// already have committed when the request is sent.
				id, err := compactionIDOf(store, s)
				if err != nil {
					return nil, err
				}
				if id == "" {
					return nil, errors.New("the compaction had not committed when the request was sent")
				}
				sent.add(req)
				return lifecycleTextTurn("done again"), nil
			})
			// boundary records the last durable entry identity the compact
			// script read before its own commit — the boundary the commit
			// must name. The write happens before the script's park
			// notification and the read after the second park: the gated
			// store's channel chain orders both accesses.
			var boundary string
			f.setCompactScript(s, func(scriptCtx context.Context, req model.Request) (model.Stream, error) {
				pieces.add(req)
				// Derived before the commit: the last durable entry at this
				// instant is the boundary the commit must name, and the
				// entry state is stable between this snapshot and the
				// commit — the same discipline the commit itself relies on.
				committed, err := store.ReadEntries(scriptCtx, s, 0)
				if err != nil {
					return nil, err
				}
				if len(committed) == 0 {
					return nil, errors.New("no committed entries before the compaction commit")
				}
				boundary = committed[len(committed)-1].ID
				// The next two transactions are the piece's empty running
				// result and the compaction commit: park the second one
				// post-commit so the frozen instant is observable.
				gated.armPostCommit(2)
				return compactSummaryTurn("summary one", model.Usage{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}), nil
			})

			// No passive event may fire across the compaction: the protocol
			// and client surface are untouched by compaction.
			sub, err := f.r.Subscribe(16)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Close()

			f.submit(s, "op-2", "second question")
			// The first parked transaction is the piece's empty running
			// result; releasing it lets the compaction commit run and park.
			<-gated.parked
			gated.releaseGate()
			<-gated.parked
			// The frozen instant: the committed entry, the flipped
			// projection, and both usage totals are durable together, while
			// the enclosing Operation still runs — a split implementation
			// would show the entry without the register changes.
			parkedEntries := entrySnapshot(t, store, s)
			if len(parkedEntries) == 0 || parkedEntries[len(parkedEntries)-1].Kind != harness.EntryCompaction {
				t.Fatalf("parked entry tail = %+v, want the compaction entry committed last", parkedEntries)
			}
			var parkedEntry compactWireEntry
			if err := json.Unmarshal(parkedEntries[len(parkedEntries)-1].Payload, &parkedEntry); err != nil {
				t.Fatalf("decode parked compaction entry: %v", err)
			}
			if parkedEntry.BoundaryEntryID != boundary {
				t.Fatalf("parked boundary %q != the last pre-commit entry %q", parkedEntry.BoundaryEntryID, boundary)
			}
			_, parkedCompactionID, parkedSessionUsage := compactSessionRegisterState(t, store, s)
			if parkedCompactionID != parkedEntry.EntryID {
				t.Fatalf("parked compaction_entry_id = %q, want the committed entry %q", parkedCompactionID, parkedEntry.EntryID)
			}
			if got, ok := parkedSessionUsage.countFor("prov", "m"); !ok || got != (harness.UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}) {
				t.Fatalf("parked session usage = %+v, want the piece's counts", parkedSessionUsage)
			}
			parkedStatus, parkedOperationUsage, _ := compactOperationRegisterState(t, store, s, "op-2")
			if parkedStatus != harness.OperationRunning {
				t.Fatalf("parked operation status = %q, want the enclosing Operation still running", parkedStatus)
			}
			if got, ok := parkedOperationUsage.countFor("prov", "m"); !ok || got != (harness.UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}) {
				t.Fatalf("parked operation usage = %+v, want the piece's counts", parkedOperationUsage)
			}
			gated.releaseGate()
			awaitOperation(t, f.r, s, "op-2", harness.OperationSuccess)

			if pieces.count() != 1 {
				t.Fatalf("compact transport calls = %d, want exactly one piece", pieces.count())
			}
			if sent.count() != 1 {
				t.Fatalf("conversation transport calls = %d, want exactly one after the compaction", sent.count())
			}
			// The compact request is the no-Session compact Agent's shape:
			// exactly the compact prompt as system and the framed serialized
			// piece as user, with no tools.
			piece := pieces.at(0)
			if len(piece.Messages) != 2 {
				t.Fatalf("piece request messages = %d, want exactly system and user", len(piece.Messages))
			}
			if piece.Messages[0].Role != model.RoleSystem || piece.Messages[0].TextContent() != "summarize" {
				t.Fatalf("piece system message = %+v, want the compact prompt", piece.Messages[0])
			}
			if piece.Messages[1].Role != model.RoleUser {
				t.Fatalf("piece user message role = %q, want user", piece.Messages[1].Role)
			}
			if len(piece.Tools) != 0 {
				t.Fatalf("piece request tools = %d, want none: the compact agent has no tools", len(piece.Tools))
			}
			// No pruner path: the one piece carries the whole serialized
			// snapshot — every pre-boundary message including the triggering
			// input, in order, exactly once.
			pieceMessages := compactPieceMessages(t, piece.Messages[1].TextContent())
			if len(pieceMessages) != 3 {
				t.Fatalf("piece lines = %d, want the whole snapshot's three messages: %+v", len(pieceMessages), pieceMessages)
			}
			if pieceMessages[0].Role != "user" || pieceMessages[0].Content[0].Text != compactSeedInput() {
				t.Fatalf("piece line 0 = %+v, want the seeded user message", pieceMessages[0])
			}
			if pieceMessages[1].Role != "assistant" || pieceMessages[1].Content[0].Text != "done" {
				t.Fatalf("piece line 1 = %+v, want the seeded assistant message", pieceMessages[1])
			}
			if pieceMessages[2].Role != "user" || pieceMessages[2].Content[0].Text != "second question" {
				t.Fatalf("piece line 2 = %+v, want the triggering input", pieceMessages[2])
			}
			if strings.HasPrefix(piece.Messages[1].TextContent(), "Previous summary:") {
				t.Fatalf("the first piece carries a previous-summary block: %q", piece.Messages[1].TextContent())
			}
			// The sent request carries exactly the summary message and no
			// post-boundary entries — no retained tail.
			want := []compactMessageShape{
				{role: "system", text: "compact-sys"},
				{role: "assistant", text: "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]", source: compactConversationRef},
			}
			assertCompactShapes(t, compactShapesOf(sent.at(0).Messages), want)

			// The one commit is the whole-snapshot one, operation-owned, with
			// the usage attributed.
			entries := compactCompactionEntriesOf(t, store, s)
			if len(entries) != 1 {
				t.Fatalf("compaction entries = %d, want exactly one", len(entries))
			}
			entry := entries[0]
			if entry.OperationID != "op-2" || entry.Summary != "summary one" {
				t.Fatalf("compaction entry = %+v, want the Operation-owned committed summary", entry)
			}
			if entry.Model != (compactWireRef{Provider: "prov", Model: "m"}) {
				t.Fatalf("compaction entry model = %+v, want the compact model identity", entry.Model)
			}
			if entry.BoundaryEntryID != boundary {
				t.Fatalf("committed boundary %q != the last pre-commit entry %q", entry.BoundaryEntryID, boundary)
			}
			if entry.ConfigurationRevision != f.readOperation(s, "op-2").Admission.Execution.ConfigurationRevision {
				t.Fatalf("committed revision %q != the admission's recorded revision %q", entry.ConfigurationRevision, f.readOperation(s, "op-2").Admission.Execution.ConfigurationRevision)
			}
			if entry.Usage == nil || *entry.Usage != (harness.UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}) {
				t.Fatalf("compaction entry usage = %+v, want the piece's reported counts", entry.Usage)
			}
			// The projection state names the entry, and both register totals
			// agree with it.
			if got := f.readSession(s).State.CompactionEntryID; got != entry.EntryID {
				t.Fatalf("compaction_entry_id = %q, want the committed entry %q", got, entry.EntryID)
			}
			op := f.readOperation(s, "op-2")
			if got := compactUsageFor(t, op.State.Usage, compactConversationRef); got != (harness.UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}) {
				t.Fatalf("operation usage = %+v, want the entry's counts", got)
			}
			if got := compactUsageFor(t, f.readSession(s).State.Usage, compactConversationRef); got != (harness.UsageCount{InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5}) {
				t.Fatalf("session usage = %+v, want the entry's counts", got)
			}

			// A post-compaction submit carries exactly the summary message
			// and its own post-boundary entries: still no retained tail.
			f.submit(s, "op-3", "tail probe")
			awaitOperation(t, f.r, s, "op-3", harness.OperationSuccess)
			if sent.count() != 2 {
				t.Fatalf("conversation transport calls = %d, want the post-compaction probe's one call", sent.count())
			}
			want = []compactMessageShape{
				{role: "system", text: "compact-sys"},
				{role: "assistant", text: "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]", source: compactConversationRef},
				{role: "assistant", text: "done again", source: compactConversationRef},
				{role: "user", text: "tail probe"},
			}
			assertCompactShapes(t, compactShapesOf(sent.at(1).Messages), want)

			// The subscription saw only the fixed configuration and scope
			// event kinds across the compaction: no new protocol or client
			// surface fired.
			for {
				select {
				case event := <-sub.Events():
					switch event.Kind {
					case EventConfiguration, EventScopeOpened, EventScopeClosed:
					default:
						t.Fatalf("observation fired kind %q during compaction, want only the fixed configuration and scope set", event.Kind)
					}
					continue
				default:
				}
				break
			}

			if err := f.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})

	t.Run("second overflow in the same operation commits a second entry", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			f := openCompactLifecycle(t, store)
			s := f.session("reoverflow")
			f.submit(s, "op-1", compactSeedInput())
			awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
			awaitIdleSession(t, f.r, s)

			summaryMsg := model.Message{Role: model.RoleAssistant, Source: compactConversationRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]"}}}
			firstRequest := []model.Message{
				sysMessageText("compact-sys"),
				userMessageText(compactSeedInput()),
				assistantMessageText("done"),
				userMessageText("second question"),
			}
			convWindow := compactWindowFor(firstRequest, 64, true)
			// The tool boundary's tail (the call and its complete result)
			// must push the third request back over the same window, while
			// the first rebuilt request stays under it: sized by arithmetic.
			tailBase := compactEstimate([]model.Message{
				{Role: model.RoleAssistant, Source: compactConversationRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "working"}}, ToolCalls: []model.ToolCall{{ID: "call-1", Name: "noop", Arguments: json.RawMessage(`{"x":1}`)}}},
				{Role: model.RoleTool, ToolCallID: "call-1", Content: []model.ContentPart{{Kind: model.PartText, Text: "ok"}}},
			})
			needed := compactEstimate(firstRequest) - compactEstimate([]model.Message{sysMessageText("compact-sys"), summaryMsg}) - tailBase + 1
			f.setToolResult("ok " + compactTextWithTokens("tail ", max(needed, 0)+8))
			paddedResult := "ok " + compactTextWithTokens("tail ", max(needed, 0)+8)
			thirdRequest := []model.Message{
				sysMessageText("compact-sys"),
				summaryMsg,
				{Role: model.RoleAssistant, Source: compactConversationRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "working"}}, ToolCalls: []model.ToolCall{{ID: "call-1", Name: "noop", Arguments: json.RawMessage(`{"x":1}`)}}},
				{Role: model.RoleTool, ToolCallID: "call-1", Content: []model.ContentPart{{Kind: model.PartText, Text: paddedResult}}},
			}
			if fits := compactEstimate([]model.Message{sysMessageText("compact-sys"), summaryMsg}) + 64; fits > convWindow {
				t.Fatalf("fixture precondition: the first rebuilt request %d tokens does not fit the window", fits)
			}
			if thirdEstimate := compactEstimate(thirdRequest) + 64; thirdEstimate <= convWindow {
				t.Fatalf("fixture precondition: the third request %d tokens fits the window, want a re-overflow", thirdEstimate)
			}
			f.setCapture(convWindow, 64, "compact-sys", harness.CompactCapture{
				Model: compactConversationRef, ContextWindow: 1 << 20, OutputReserve: 1024, SystemPrompt: "summarize",
			})
			var sent compactRequestRecords
			var pieces compactRequestRecords
			f.setConvScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
				sent.add(req)
				if sent.count() == 1 {
					return lifecycleCallTurn("call-1", "noop", `{"x":1}`), nil
				}
				return lifecycleTextTurn("final"), nil
			})
			f.setCompactScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
				pieces.add(req)
				if pieces.count() == 1 {
					return compactSummaryTurn("summary one", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
				}
				return compactSummaryTurn("summary two", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			})
			f.submit(s, "op-2", "second question")
			awaitOperation(t, f.r, s, "op-2", harness.OperationSuccess)

			if pieces.count() != 2 {
				t.Fatalf("compact transport calls = %d, want one piece per compaction", pieces.count())
			}
			if sent.count() != 2 {
				t.Fatalf("conversation transport calls = %d, want one per boundary", sent.count())
			}
			first := compactShapesOf(sent.at(0).Messages)
			assertCompactShapes(t, first, []compactMessageShape{
				{role: "system", text: "compact-sys"},
				{role: "assistant", text: "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]", source: compactConversationRef},
			})
			second := compactShapesOf(sent.at(1).Messages)
			assertCompactShapes(t, second, []compactMessageShape{
				{role: "system", text: "compact-sys"},
				{role: "assistant", text: "[Previous conversation summary]\n\nsummary two\n\n[End of summary. Continue from here.]", source: compactConversationRef},
			})
			entries := compactCompactionEntriesOf(t, store, s)
			if len(entries) != 2 {
				t.Fatalf("compaction entries = %d, want one per overflow in the same Operation", len(entries))
			}
			for i, want := range []string{"summary one", "summary two"} {
				if entries[i].OperationID != "op-2" || entries[i].Summary != want {
					t.Fatalf("compaction entry %d = %+v, want the Operation-owned %q summary", i, entries[i], want)
				}
			}
			if got := f.readSession(s).State.CompactionEntryID; got != entries[1].EntryID {
				t.Fatalf("compaction_entry_id = %q, want the newest entry %q", got, entries[1].EntryID)
			}
			if err := f.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})

	t.Run("an automatic compaction in a launched child session", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			f := openCompactLifecycle(t, store)
			parent := f.session("child-parent")
			var childID string
			err := f.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				res, err := h.LaunchChildSession(ctx, harness.LaunchChildRequest{
					ParentSessionID: parent,
					AgentType:       "worker",
					Content:         []model.ContentPart{{Kind: model.PartText, Text: "child task"}},
					OperationID:     "op-1",
					MaxConcurrent:   5,
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
			awaitOperation(t, f.r, childID, "op-1", harness.OperationSuccess)

			// The child's next request overflows: the window is sized over
			// the child's projected messages with the huge input.
			huge := compactTextWithTokens("word ", 20000)
			childMessages := []model.Message{
				sysMessageText("compact-sys"),
				userMessageText("child task"),
				assistantMessageText("done"),
				userMessageText(huge),
			}
			f.setCapture(compactWindowFor(childMessages, 64, true), 64, "compact-sys", harness.CompactCapture{
				Model: compactConversationRef, ContextWindow: 1 << 20, OutputReserve: 1024, SystemPrompt: "summarize",
			})
			var sent compactRequestRecords
			f.setConvScript(childID, func(_ context.Context, req model.Request) (model.Stream, error) {
				sent.add(req)
				return lifecycleTextTurn("done again"), nil
			})
			f.setCompactScript(childID, func(_ context.Context, req model.Request) (model.Stream, error) {
				return compactSummaryTurn("child summary", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			})
			f.submit(childID, "op-2", huge)
			awaitOperation(t, f.r, childID, "op-2", harness.OperationSuccess)

			// The child Session carries exactly one compaction entry with
			// the register's projection field naming it.
			entries := compactCompactionEntriesOf(t, store, childID)
			if len(entries) != 1 {
				t.Fatalf("child compaction entries = %+v, want exactly one", entries)
			}
			id, rerr := compactionIDOf(store, childID)
			if rerr != nil || id != entries[0].EntryID {
				t.Fatalf("child compaction_entry_id = (%q, %v), want the committed entry %q", id, rerr, entries[0].EntryID)
			}
			// The conversation transport saw the post-summary request.
			if sent.count() != 1 {
				t.Fatalf("conversation transport calls = %d, want the post-summary request's one call", sent.count())
			}
			assertCompactShapes(t, compactShapesOf(sent.at(0).Messages), []compactMessageShape{
				{role: "system", text: "compact-sys"},
				{role: "assistant", text: "[Previous conversation summary]\n\nchild summary\n\n[End of summary. Continue from here.]", source: compactConversationRef},
			})
			if err := f.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})
}

// --- the complete-input and checkpoint rows ---

// driveRichConversation drives one Operation whose history carries an errored
// partial output continued through the failure signal, a completed turn with
// text, opaque reasoning, a non-text part and a tool call, its complete tool
// output, and a refusal turn — every model-visible shape the complete-input
// contract must preserve.
func driveRichConversation(t *testing.T, f *compactLifecycle, sessionID string) {
	t.Helper()
	// The first boundary fails after partial output; the second returns the
	// rich tool turn; the third ends the Operation with the refusal.
	var calls compactRequestRecords
	f.setConvScript(sessionID, func(_ context.Context, req model.Request) (model.Stream, error) {
		calls.add(req)
		switch calls.count() - 1 {
		case 0:
			return &compactPartialStream{fail: errors.New("stream broke")}, nil
		case 1:
			return compactRichTurn(), nil
		default:
			return compactRefusalTurn("cannot do that"), nil
		}
	})
	f.submit(sessionID, "op-1", "question one")
	awaitOperation(t, f.r, sessionID, "op-1", harness.OperationSuccess)
}

// richProjectionMessages is the model-visible projection the rich driver
// commits, in order — the estimator's input for the trigger arithmetic.
func richProjectionMessages() []model.Message {
	return []model.Message{
		userMessageText("question one"),
		assistantMessageText("partial answer"),
		userMessageText("<system-signal>The previous model response failed after partial output. Continue from the retained response.</system-signal>"),
		{Role: model.RoleAssistant, Source: compactConversationRef, Content: []model.ContentPart{
			{Kind: model.PartText, Text: "working"},
			{Kind: model.PartOpaque, OpaqueWireType: "thinking"},
			{Kind: model.PartImageURL, URL: "https://img.test/p.png"},
		}, ToolCalls: []model.ToolCall{{ID: "call-rich", Name: "noop", Arguments: json.RawMessage(`{"x":1}`)}}},
		{Role: model.RoleTool, ToolCallID: "call-rich", Content: []model.ContentPart{{Kind: model.PartText, Text: "ok"}}},
		{Role: model.RoleAssistant, Source: compactConversationRef, Refusal: "cannot do that"},
	}
}

// openTwoPieceOverflow drives the rich conversation and then submits a huge
// second message whose request overflows a window sized against it, with the
// compact budget fixed so the serialized snapshot splits into exactly two
// pieces: the six-piece first piece and the huge second message alone.
// compactSerializedLine mirrors the compaction serializer's one inert line
// for estimate arithmetic: a copy with the source cleared and each tool
// call's raw arguments replaced by their quoted byte representation. The
// fixture's messages always marshal.
func compactSerializedLine(msg model.Message) string {
	copied := msg
	copied.Source = model.ModelRef{}
	if len(copied.ToolCalls) > 0 {
		copied.ToolCalls = append(make([]model.ToolCall, 0, len(copied.ToolCalls)), copied.ToolCalls...)
		for i := range copied.ToolCalls {
			quoted, err := json.Marshal(fmt.Sprintf("%q", []byte(copied.ToolCalls[i].Arguments)))
			if err != nil {
				panic(err)
			}
			copied.ToolCalls[i].Arguments = quoted
		}
	}
	line, err := json.Marshal(copied)
	if err != nil {
		panic(err)
	}
	return string(line)
}

// compactContractHuge sizes the huge user message into the architecture's
// one-entry-fit contract window: its serialized line must exceed the
// nonempty first piece's remaining budget (closing the piece after the
// prefix messages) yet fit a fresh empty piece under the piece budget
// including the previous-summary subtraction. prefixCost is the serialized
// cost of the messages packed before it; priorSub is the previous-summary
// subtraction of the piece carrying the huge message alone. The repetition
// count is binary-searched against the serialized line's monotone cost, and
// both inequalities are asserted on the built text so a future edit cannot
// regress into the out-of-contract shape.
func compactContractHuge(t *testing.T, prefixCost, priorSub int) string {
	t.Helper()
	const budget = 4096
	if prefixCost > budget {
		t.Fatalf("fixture precondition: the prefix costs %d tokens over the %d-token piece budget", prefixCost, budget)
	}
	upper := budget - priorSub
	lower := budget - prefixCost
	if lower >= upper {
		t.Fatalf("fixture precondition: the one-entry-fit window is empty (%d >= %d)", lower, upper)
	}
	lineCost := func(reps int) int {
		return compactEncode(compactSerializedLine(userMessageText(strings.Repeat("word ", reps))))
	}
	hi := 1
	for lineCost(hi) <= upper {
		hi *= 2
	}
	lo := hi / 2
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if lineCost(mid) <= upper {
			lo = mid
		} else {
			hi = mid
		}
	}
	huge := strings.Repeat("word ", lo)
	cost := lineCost(lo)
	if cost <= lower {
		t.Fatalf("fixture precondition: the huge message's serialized cost %d does not close the first piece (prefix %d, budget %d)", cost, prefixCost, budget)
	}
	if cost > upper {
		t.Fatalf("fixture precondition: the huge message's serialized cost %d exceeds the fresh-piece budget %d", cost, upper)
	}
	return huge
}

// compactTwoPieceCapture is the compact capture whose piece budget is fixed
// at 4096 serialized-input tokens by window/reserve arithmetic: the budget
// holds the small serialized messages and repels the huge one.
func compactTwoPieceCapture() harness.CompactCapture {
	budget := 4096
	window := compactEncode("summarize") + 64 + budget + 1024
	return harness.CompactCapture{
		Model:         compactConversationRef,
		ContextWindow: window,
		OutputReserve: window - compactEncode("summarize") - 64 - budget,
		SystemPrompt:  "summarize",
	}
}

// openTwoPieceOverflow drives the rich conversation and sizes an overflowing
// window against the submitted huge message, returning the fixture, the
// session, and the huge text itself so tests can pin the whole-message
// invariant against the exact submitted bytes.
func openTwoPieceOverflow(t *testing.T, store harness.Storage) (*compactLifecycle, string, string) {
	t.Helper()
	f := openCompactLifecycle(t, store)
	s := f.session("pieces")
	driveRichConversation(t, f, s)

	// The huge message sized into the one-entry-fit contract window: the
	// rich prefix's serialized cost and the second piece's previous-summary
	// subtraction bound it from both sides.
	prefixCost := 0
	for _, msg := range richProjectionMessages() {
		prefixCost += compactEncode(compactSerializedLine(msg))
	}
	priorSub := compactEstimate([]model.Message{userMessageText("piece one")})
	huge := compactContractHuge(t, prefixCost, priorSub)
	request := append([]model.Message{sysMessageText("compact-sys")}, append(richProjectionMessages(), userMessageText(huge))...)
	convWindow := compactWindowFor(request, 1024, true)
	if cost := compactEstimate(request); cost+1024 <= convWindow {
		t.Fatalf("fixture precondition: the overflowing request holds %d tokens under the window", cost)
	}
	f.setCapture(convWindow, 1024, "compact-sys", compactTwoPieceCapture())
	return f, s, huge
}

func TestCompactLifecycleCompleteInputPieces(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		f, s, huge := openTwoPieceOverflow(t, store)
		var pieces compactRequestRecords
		var sent compactRequestRecords
		f.setConvScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
			sent.add(req)
			return lifecycleTextTurn("done"), nil
		})
		f.setCompactScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
			pieces.add(req)
			if pieces.count() == 1 {
				return compactSummaryTurn("piece one", model.Usage{InputTokens: 4, OutputTokens: 2}), nil
			}
			return compactSummaryTurn("piece two", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
		})
		f.submit(s, "op-2", huge)
		awaitOperation(t, f.r, s, "op-2", harness.OperationSuccess)

		if pieces.count() != 2 {
			t.Fatalf("compact transport calls = %d, want exactly two pieces", pieces.count())
		}
		if sent.count() != 1 {
			t.Fatalf("conversation transport calls = %d, want exactly the post-compaction request", sent.count())
		}
		// The first piece carries the whole rich history; the second carries
		// only the huge message with the previous-summary framing.
		first := compactPieceMessages(t, pieces.at(0).Messages[1].TextContent())
		if len(first) != 6 {
			t.Fatalf("first piece lines = %d, want the six pre-boundary messages: %+v", len(first), first)
		}
		if first[0].Role != "user" || first[0].Content[0].Text != "question one" {
			t.Fatalf("first piece line 0 = %+v, want the user question", first[0])
		}
		if first[1].Role != "assistant" || first[1].Content[0].Text != "partial answer" {
			t.Fatalf("first piece line 1 = %+v, want the errored partial output", first[1])
		}
		if first[2].Role != "user" || first[2].Content[0].Text != "<system-signal>The previous model response failed after partial output. Continue from the retained response.</system-signal>" {
			t.Fatalf("first piece line 2 = %+v, want the wrapped failure signal", first[2])
		}
		if first[3].Role != "assistant" {
			t.Fatalf("first piece line 3 role = %q, want assistant", first[3].Role)
		}
		// The rich message's content parts in exact order and the tool
		// result's complete content slice: the whole-slice discipline —
		// duplicates, reorders, and extra parts cannot pass.
		wantRich := []compactWirePart{
			{Kind: "text", Text: "working"},
			{Kind: "opaque", OpaqueWireType: "thinking"},
			{Kind: "image_url", URL: "https://img.test/p.png"},
		}
		if !slices.Equal(first[3].Content, wantRich) {
			t.Fatalf("rich line content = %+v, want the exact ordered parts %+v", first[3].Content, wantRich)
		}
		if len(first[3].ToolCalls) != 1 || first[3].ToolCalls[0].ID != "call-rich" || first[3].ToolCalls[0].Name != "noop" {
			t.Fatalf("rich line tool calls = %+v, want exactly the one noop call", first[3].ToolCalls)
		}
		// The serialized arguments carry the raw bytes through the quoted
		// representation: unquoting them returns the submitted arguments
		// exactly.
		args, uerr := strconv.Unquote(first[3].ToolCalls[0].Arguments)
		if uerr != nil || args != `{"x":1}` {
			t.Fatalf("serialized tool-call arguments = %q (unquote error %v), want the submitted bytes", first[3].ToolCalls[0].Arguments, uerr)
		}
		wantResult := []compactWirePart{{Kind: "text", Text: "ok"}}
		if first[4].Role != "tool" || first[4].ToolCallID != "call-rich" || !slices.Equal(first[4].Content, wantResult) {
			t.Fatalf("first piece line 4 = %+v, want the complete tool output %+v", first[4], wantResult)
		}
		if first[5].Role != "assistant" || first[5].Refusal != "cannot do that" {
			t.Fatalf("first piece line 5 = %+v, want the refusal turn", first[5])
		}
		const framing = "Previous summary:\npiece one\n\nContinuation:\n"
		secondText := pieces.at(1).Messages[1].TextContent()
		if !strings.HasPrefix(secondText, framing) {
			t.Fatalf("second piece framing = %q, want the previous-summary continuation block", secondText[:min(60, len(secondText))])
		}
		second := compactPieceMessages(t, strings.TrimPrefix(secondText, framing))
		// The whole-message invariant: the second piece's line is the
		// COMPLETE submitted huge message — one text part carrying every
		// submitted byte — so no truncation window can pass.
		if len(second) != 1 || second[0].Role != "user" || len(second[0].Content) != 1 ||
			second[0].Content[0].Kind != "text" || second[0].Content[0].Text != huge {
			t.Fatalf("second piece lines = %+v, want exactly the submitted huge message whole (%d bytes)", second, len(huge))
		}
		// The committed summary is the last piece's output with the
		// accumulated usage; the registers agree with the entry.
		entries := compactCompactionEntriesOf(t, store, s)
		if len(entries) != 1 || entries[0].Summary != "piece two" {
			t.Fatalf("compaction entries = %+v, want the one rolling commit", entries)
		}
		if entries[0].Usage == nil || *entries[0].Usage != (harness.UsageCount{InputTokens: 5, OutputTokens: 3}) {
			t.Fatalf("compaction entry usage = %+v, want the accumulated piece totals", entries[0].Usage)
		}
		if got := compactUsageFor(t, f.readOperation(s, "op-2").State.Usage, compactConversationRef); got != (harness.UsageCount{InputTokens: 5, OutputTokens: 3}) {
			t.Fatalf("operation usage = %+v, want the entry's accumulated counts", got)
		}
		if got := compactUsageFor(t, f.readSession(s).State.Usage, compactConversationRef); got != (harness.UsageCount{InputTokens: 5, OutputTokens: 3}) {
			t.Fatalf("session usage = %+v, want the entry's accumulated counts", got)
		}
		if err := f.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

func TestCompactLifecycleCheckpointFailure(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		f, s, huge := openTwoPieceOverflow(t, store)
		before := entrySnapshot(t, store, s)
		var sent compactRequestRecords
		// Phase 1: a successful first compaction commits the prior summary
		// the failing checkpoint must leave current.
		f.setConvScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
			sent.add(req)
			return lifecycleTextTurn("done"), nil
		})
		var priorPieces compactRequestRecords
		f.setCompactScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
			priorPieces.add(req)
			if priorPieces.count() == 1 {
				return compactSummaryTurn("piece one", model.Usage{InputTokens: 2, OutputTokens: 1}), nil
			}
			return compactSummaryTurn("prior summary", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
		})
		f.submit(s, "op-2", huge)
		awaitOperation(t, f.r, s, "op-2", harness.OperationSuccess)
		prior := compactCompactionEntriesOf(t, store, s)
		if len(prior) != 1 {
			t.Fatalf("prior compaction entries = %d, want the one successful commit", len(prior))
		}
		priorID := prior[0].EntryID
		awaitIdleSession(t, f.r, s)

		// Phase 2: the failing checkpoint runs against the summarized
		// session; the window is sized against the new huge request, whose
		// serialized cost sits in the one-entry-fit window between the
		// prior-summary message's cost and the fresh-piece budget.
		priorSummaryMsg := model.Message{Role: model.RoleAssistant, Source: compactConversationRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\nprior summary\n\n[End of summary. Continue from here.]"}}}
		huge2 := compactContractHuge(t, compactEncode(compactSerializedLine(priorSummaryMsg)), compactEstimate([]model.Message{userMessageText("piece one")}))
		f.setCapture(compactWindowFor([]model.Message{
			sysMessageText("compact-sys"),
			priorSummaryMsg,
			userMessageText(huge2),
		}, 1024, true), 1024, "compact-sys", compactTwoPieceCapture())
		var failed compactRequestRecords
		f.setConvScript(s, func(context.Context, model.Request) (model.Stream, error) {
			return nil, errors.New("the conversation transport ran for a failed checkpoint")
		})
		f.setCompactScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
			failed.add(req)
			if failed.count() == 1 {
				return compactSummaryTurn("piece one", model.Usage{InputTokens: 4, OutputTokens: 2}), nil
			}
			return nil, errors.New("provider failure")
		})
		f.prep.setRetry(func(error, int) (time.Duration, bool) { return 0, false })
		f.submit(s, "op-3", huge2)
		awaitOperation(t, f.r, s, "op-3", harness.OperationFailure)

		if failed.count() != 2 {
			t.Fatalf("compact transport calls = %d, want the succeeded piece and the failed one with no retry", failed.count())
		}
		if sent.count() != 1 {
			t.Fatalf("conversation transport calls = %d, want only the first compaction's post-commit request", sent.count())
		}
		// The prior summary survives the failed checkpoint: no new compaction
		// entry committed and the register still names the prior one.
		ids := compactCompactionEntriesOf(t, store, s)
		if len(ids) != 1 || ids[0].EntryID != priorID {
			t.Fatalf("compaction entries = %+v, want only the prior entry %q", ids, priorID)
		}
		if id, err := compactionIDOf(store, s); err != nil || id != priorID {
			t.Fatalf("compaction_entry_id = (%q, %v), want the prior summary %q still current", id, err, priorID)
		}
		// Every pre-existing entry is unchanged; the tail is op-2's commit
		// records and op-3's input and failure settlement.
		after := entrySnapshot(t, store, s)
		if len(after) != len(before)+6 {
			t.Fatalf("entries = %d, want the pre-failure %d plus the two Operations' tails", len(after), len(before))
		}
		for i := range before {
			if after[i].ID != before[i].ID || after[i].Kind != before[i].Kind {
				t.Fatalf("entry %d changed through the failed checkpoint", i)
			}
		}
		wantTail := []harness.EntryKind{harness.EntryInput, harness.EntryCompaction, harness.EntryAssistant, harness.EntryOperationSettlement, harness.EntryInput, harness.EntryOperationSettlement}
		for i, kind := range wantTail {
			if after[len(before)+i].Kind != kind {
				t.Fatalf("tail entry %d = %s, want %s", i, after[len(before)+i].Kind, kind)
			}
		}
		var priorWire compactWireEntry
		if err := json.Unmarshal(after[len(before)+1].Payload, &priorWire); err != nil {
			t.Fatalf("decode prior compaction entry: %v", err)
		}
		if priorWire.EntryID != priorID || priorWire.Summary != "prior summary" {
			t.Fatalf("prior compaction entry = %+v, want the surviving summary", priorWire)
		}
		var settlementWire struct {
			OperationID string              `json:"operation_id"`
			Status      string              `json:"status"`
			Detail      string              `json:"detail"`
			Model       *compactWireRef     `json:"model"`
			Usage       *harness.UsageCount `json:"usage"`
		}
		if err := json.Unmarshal(after[len(after)-1].Payload, &settlementWire); err != nil {
			t.Fatalf("decode settlement payload: %v", err)
		}
		if settlementWire.OperationID != "op-3" || settlementWire.Status != "failure" || !strings.Contains(settlementWire.Detail, "provider failure") {
			t.Fatalf("settlement payload = %+v, want the ordinary Operation failure", settlementWire)
		}
		if settlementWire.Model == nil || *settlementWire.Model != (compactWireRef{Provider: "prov", Model: "m"}) {
			t.Fatalf("settlement model = %+v, want the compact model identity", settlementWire.Model)
		}
		if settlementWire.Usage == nil || *settlementWire.Usage != (harness.UsageCount{InputTokens: 4, OutputTokens: 2}) {
			t.Fatalf("settlement usage = %+v, want the accumulated piece usage", settlementWire.Usage)
		}
		if got := compactUsageFor(t, f.readOperation(s, "op-3").State.Usage, compactConversationRef); got != (harness.UsageCount{InputTokens: 4, OutputTokens: 2}) {
			t.Fatalf("operation usage = %+v, want the settlement's accumulated counts", got)
		}
		// The session total composes the prior commit's (3,2) with the failed
		// checkpoint's settlement (4,2).
		if got := compactUsageFor(t, f.readSession(s).State.Usage, compactConversationRef); got != (harness.UsageCount{InputTokens: 7, OutputTokens: 4}) {
			t.Fatalf("session usage = %+v, want the prior commit plus the settlement's counts", got)
		}
		if err := f.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- the steering-during-compaction row ---

// TestCompactLifecycleSteeringDuringCompaction proves the steering row: a
// message submitted while the orchestration runs stays buffered — the rebuilt
// request carries only the compacted projection — and drains at the next
// model boundary as an Operation-owned input.
func TestCompactLifecycleSteeringDuringCompaction(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		f := openCompactLifecycle(t, store)
		s := f.session("steering")
		f.submit(s, "op-1", compactSeedInput())
		awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
		awaitIdleSession(t, f.r, s)

		messages := []model.Message{
			sysMessageText("compact-sys"),
			userMessageText(compactSeedInput()),
			assistantMessageText("done"),
			userMessageText("second question"),
		}
		f.setCapture(compactWindowFor(messages, 64, true), 64, "compact-sys", harness.CompactCapture{
			Model: compactConversationRef, ContextWindow: 1 << 20, OutputReserve: 1024, SystemPrompt: "summarize",
		})
		var sent compactRequestRecords
		f.setConvScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
			sent.add(req)
			if sent.count() == 1 {
				return lifecycleCallTurn("call-1", "noop", `{"x":1}`), nil
			}
			return lifecycleTextTurn("final"), nil
		})
		f.setCompactScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
			// Steering submitted during the orchestration: the ordinary
			// active routing buffers it.
			f.submitMode(s, "op-steer", "steered mid-compaction", harness.MessageModeRegular)
			return compactSummaryTurn("summary one", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
		})
		f.submit(s, "op-2", "second question")
		awaitOperation(t, f.r, s, "op-2", harness.OperationSuccess)

		if sent.count() != 2 {
			t.Fatalf("conversation transport calls = %d, want the rebuilt request and the drained boundary", sent.count())
		}
		// The rebuilt request carried none of the steering.
		want := []compactMessageShape{
			{role: "system", text: "compact-sys"},
			{role: "assistant", text: "[Previous conversation summary]\n\nsummary one\n\n[End of summary. Continue from here.]", source: compactConversationRef},
		}
		assertCompactShapes(t, compactShapesOf(sent.at(0).Messages), want)
		// The next boundary drained it as an Operation-owned input.
		drained := compactShapesOf(sent.at(1).Messages)
		if len(drained) != 5 || drained[4].text != "steered mid-compaction" || drained[4].role != "user" {
			t.Fatalf("next-boundary request = %+v, want the drained steering message last", drained)
		}
		inputs := lifecycleInputsOf(t, store, s)
		var steered bool
		for _, input := range inputs {
			if input.text == "steered mid-compaction" {
				steered = input.operationID == "op-2"
			}
		}
		if !steered {
			t.Fatalf("committed inputs = %+v, want the steering committed under the running Operation", inputs)
		}
		// The steering submission never became its own Operation.
		err := f.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ReadOperation(ctx, s, "op-steer")
			return err
		})
		if !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("steering submission operation = %v, want no separate admission", err)
		}
		if err := f.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- the manual compaction rows ---

func TestCompactLifecycleManualCompaction(t *testing.T) {
	t.Run("idle commits the one manual transaction and retry resolves it", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			gated := newGatedStore(store)
			f := openCompactLifecycle(t, gated)
			s := f.session("manual")
			f.submit(s, "op-1", "hello manual")
			awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
			awaitIdleSession(t, f.r, s)

			var convCalls int
			f.setConvScript(s, func(context.Context, model.Request) (model.Stream, error) {
				convCalls++
				return nil, errors.New("the conversation transport ran during manual compaction")
			})
			var pieces compactRequestRecords
			f.setCompactScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
				pieces.add(req)
				// The next two transactions are the piece's empty running
				// result and the one manual commit: park the second one
				// post-commit so the frozen instant is observable.
				gated.armPostCommit(2)
				return compactSummaryTurn("manual summary", model.Usage{InputTokens: 7, OutputTokens: 3}), nil
			})
			// The freeze-time last projectable entry — the run turn's
			// assistant — captured from the session's entries before the
			// manual compaction freezes its snapshot.
			var freezeBoundary string
			for _, entry := range f.entries(s) {
				switch entry.Kind {
				case harness.EntryInput, harness.EntryAssistant, harness.EntryToolResult, harness.EntrySignal:
					freezeBoundary = entry.ID
				}
			}
			if freezeBoundary == "" {
				t.Fatalf("no projectable entry before the manual compaction")
			}
			rec := f.compactIdle(s, "compact-op-1")
			if rec.Admission.RequestKind != harness.RequestKindCompact {
				t.Fatalf("admission kind = %q, want compact", rec.Admission.RequestKind)
			}
			if rec.Admission.AdmittedEntry != (harness.EntryRef{}) {
				t.Fatalf("admitted entry = %+v, want the zero reference: no applicable input entry", rec.Admission.AdmittedEntry)
			}
			// The first parked transaction is the piece's empty running
			// result; releasing it lets the manual commit run and park.
			<-gated.parked
			gated.releaseGate()
			<-gated.parked
			// The frozen instant: the compaction entry, the flipped
			// projection, the usage totals, and the Operation register
			// already reading terminal success are durable together — a
			// split implementation would show the entry beside a
			// still-running Operation.
			parkedEntries := entrySnapshot(t, store, s)
			var parkedEntry compactWireEntry
			var parkedEntryID string
			for _, entry := range parkedEntries {
				if entry.Kind != harness.EntryCompaction {
					continue
				}
				parkedEntryID = entry.ID
				if err := json.Unmarshal(entry.Payload, &parkedEntry); err != nil {
					t.Fatalf("decode parked compaction entry: %v", err)
				}
			}
			if parkedEntryID == "" || parkedEntry.OperationID != "compact-op-1" || parkedEntry.Summary != "manual summary" {
				t.Fatalf("parked compaction entry = (%q, %+v), want the manual commit's entry", parkedEntryID, parkedEntry)
			}
			_, parkedCompactionID, parkedSessionUsage := compactSessionRegisterState(t, store, s)
			if parkedCompactionID != parkedEntryID {
				t.Fatalf("parked compaction_entry_id = %q, want the committed entry %q", parkedCompactionID, parkedEntryID)
			}
			if got, ok := parkedSessionUsage.countFor("prov", "m"); !ok || got != (harness.UsageCount{InputTokens: 7, OutputTokens: 3}) {
				t.Fatalf("parked session usage = %+v, want the piece's counts", parkedSessionUsage)
			}
			parkedStatus, parkedOperationUsage, parkedTerminal := compactOperationRegisterState(t, store, s, "compact-op-1")
			if parkedStatus != harness.OperationSuccess || parkedTerminal == nil {
				t.Fatalf("parked operation = (%q, %+v), want the terminal success committed in the same transaction", parkedStatus, parkedTerminal)
			}
			if got, ok := parkedOperationUsage.countFor("prov", "m"); !ok || got != (harness.UsageCount{InputTokens: 7, OutputTokens: 3}) {
				t.Fatalf("parked operation usage = %+v, want the entry's counts", parkedOperationUsage)
			}
			gated.releaseGate()
			awaitOperation(t, f.r, s, "compact-op-1", harness.OperationSuccess)

			if convCalls != 0 {
				t.Fatalf("conversation transport calls = %d, want none", convCalls)
			}
			if pieces.count() != 1 {
				t.Fatalf("compact transport calls = %d, want exactly one piece", pieces.count())
			}
			piece := pieces.at(0)
			if len(piece.Messages) != 2 || piece.Messages[0].TextContent() != "summarize" || piece.Messages[1].Role != model.RoleUser {
				t.Fatalf("piece request = %+v, want exactly the compact prompt and the framed piece", piece.Messages)
			}
			if strings.HasPrefix(piece.Messages[1].TextContent(), "Previous summary:") {
				t.Fatalf("the one-piece piece carries a previous-summary block: %q", piece.Messages[1].TextContent())
			}
			lines := compactPieceMessages(t, piece.Messages[1].TextContent())
			if len(lines) != 2 || lines[0].Content[0].Text != "hello manual" || lines[1].Content[0].Text != "done" {
				t.Fatalf("piece lines = %+v, want the whole projected conversation", lines)
			}

			entries := compactCompactionEntriesOf(t, store, s)
			if len(entries) != 1 {
				t.Fatalf("compaction entries = %d, want exactly one", len(entries))
			}
			entry := entries[0]
			if entry.OperationID != "compact-op-1" || entry.Summary != "manual summary" {
				t.Fatalf("compaction entry = %+v, want the caller-generated Operation's commit", entry)
			}
			if entry.BoundaryEntryID != freezeBoundary {
				t.Fatalf("committed boundary %q, want the freeze-time last projectable entry %q", entry.BoundaryEntryID, freezeBoundary)
			}
			if entry.Usage == nil || *entry.Usage != (harness.UsageCount{InputTokens: 7, OutputTokens: 3}) {
				t.Fatalf("compaction entry usage = %+v, want the piece's reported counts", entry.Usage)
			}
			if got := f.readSession(s).State.CompactionEntryID; got != entry.EntryID {
				t.Fatalf("compaction_entry_id = %q, want the committed entry %q", got, entry.EntryID)
			}
			if got := compactUsageFor(t, f.readOperation(s, "compact-op-1").State.Usage, compactConversationRef); got != (harness.UsageCount{InputTokens: 7, OutputTokens: 3}) {
				t.Fatalf("operation usage = %+v, want the entry's counts", got)
			}
			if got := compactUsageFor(t, f.readSession(s).State.Usage, compactConversationRef); got != (harness.UsageCount{InputTokens: 7, OutputTokens: 3}) {
				t.Fatalf("session usage = %+v, want the entry's counts", got)
			}
			// The compact Operation owns exactly one compaction entry and
			// one settlement entry, and nothing of any other kind: the
			// closed entry-kind set is the union, so any kind outside the
			// two shows up in the owned map and breaks the rule.
			owned := map[harness.EntryKind]int{}
			for _, entry := range f.entries(s) {
				if entry.OperationID == "compact-op-1" {
					owned[entry.Kind]++
				}
			}
			if len(owned) != 2 || owned[harness.EntryCompaction] != 1 || owned[harness.EntryOperationSettlement] != 1 {
				t.Fatalf("the compact Operation owns %v, want exactly one compaction entry and one settlement entry and nothing of any other kind", owned)
			}
			// A retry with the same ID resolves the first compact Operation
			// without new work.
			retried, err := f.compact(s, "compact-op-1")
			if err != nil {
				t.Fatalf("retry Compact: %v", err)
			}
			if retried.Admission.OperationID != "compact-op-1" || retried.State.Status != harness.OperationSuccess {
				t.Fatalf("retry record = %+v, want the first compact Operation", retried)
			}
			if pieces.count() != 1 {
				t.Fatalf("compact transport calls after retry = %d, want no new work", pieces.count())
			}
			if err := f.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})

	t.Run("active or buffered state rejects without overtaking", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			f := openCompactLifecycle(t, store)
			s := f.session("manual-busy")
			parked := make(chan struct{}, 1)
			gate := make(chan struct{})
			f.setConvScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
				if strings.Contains(requestText(req), "start work") {
					select {
					case parked <- struct{}{}:
					default:
					}
					<-gate
					return lifecycleTextTurn("resumed"), nil
				}
				return lifecycleTextTurn("done"), nil
			})
			f.submit(s, "op-1", "start work")
			<-parked

			if got := f.submitMode(s, "op-steer", "waiting steering", harness.MessageModeRegular); got != harness.DispositionSteering {
				t.Fatalf("steering submit = %q, want steering", got)
			}
			if got := f.submitMode(s, "op-queued", "waiting queued", harness.MessageModeQueued); got != harness.DispositionQueued {
				t.Fatalf("queued submit = %q, want queued", got)
			}
			_, err := f.compact(s, "compact-busy")
			if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "session") || !strings.Contains(err.Error(), "is not idle; admission requires an idle Session") {
				t.Fatalf("Compact on a busy session = %v, want the one idle-guard rejection", err)
			}
			if _, rerr := store.ReadRegister(ctx, harness.RegisterKey{SessionID: s, Kind: harness.RegisterOperation, OperationID: "compact-busy"}); !errors.Is(rerr, harness.ErrNotFound) {
				t.Fatalf("the rejected compact admission left a register (%v), want none", rerr)
			}
			close(gate)
			awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
			awaitIdleSession(t, f.r, s)
			// The buffered steering drained into the active Operation, and
			// the queued message admits after it: Compact never overtook.
			awaitOperation(t, f.r, s, "op-queued", harness.OperationSuccess)
			var steeringCommitted, queuedCommitted bool
			for _, input := range lifecycleInputsOf(t, store, s) {
				if input.text == "waiting steering" && input.operationID == "op-1" {
					steeringCommitted = true
				}
				if input.text == "waiting queued" && input.operationID == "op-queued" {
					queuedCommitted = true
				}
			}
			if !steeringCommitted || !queuedCommitted {
				t.Fatalf("committed inputs = %+v, want the drained steering under op-1 and the queued message under its own admission", lifecycleInputsOf(t, store, s))
			}
			// After the drain converges no deferred compaction work exists:
			// zero compaction entries for the session and the rejected
			// compact-busy register still absent — a silently-queued compact
			// cannot pass.
			if ids := compactCompactionEntriesOf(t, store, s); len(ids) != 0 {
				t.Fatalf("compaction entries = %+v, want none after the drain", ids)
			}
			if _, rerr := store.ReadRegister(ctx, harness.RegisterKey{SessionID: s, Kind: harness.RegisterOperation, OperationID: "compact-busy"}); !errors.Is(rerr, harness.ErrNotFound) {
				t.Fatalf("the rejected compact admission appeared after the drain (%v), want it absent", rerr)
			}
			if err := f.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})

	t.Run("empty conversation fails nothing to compact", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			f := openCompactLifecycle(t, store)
			s := f.session("manual-empty")
			var pieces compactRequestRecords
			f.setCompactScript(s, func(context.Context, model.Request) (model.Stream, error) {
				pieces.add(model.Request{})
				return nil, errors.New("the compact transport ran for an empty conversation")
			})
			rec, err := f.compact(s, "compact-empty")
			if err != nil {
				t.Fatalf("Compact: %v", err)
			}
			if rec.Admission.RequestKind != harness.RequestKindCompact || rec.Admission.AdmittedEntry != (harness.EntryRef{}) {
				t.Fatalf("admission = %+v, want a compact admission without an input entry", rec.Admission)
			}
			awaitOperation(t, f.r, s, "compact-empty", harness.OperationFailure)
			op := f.readOperation(s, "compact-empty")
			if op.State.Terminal == nil || op.State.Terminal.Detail != "nothing to compact" {
				t.Fatalf("terminal = %+v, want the retained nothing-to-compact detail", op.State.Terminal)
			}
			if pieces.count() != 0 {
				t.Fatalf("compact transport calls = %d, want none", pieces.count())
			}
			if ids := compactCompactionEntriesOf(t, store, s); len(ids) != 0 {
				t.Fatalf("compaction entries = %+v, want none", ids)
			}
			for _, input := range lifecycleInputsOf(t, store, s) {
				if input.operationID == "compact-empty" {
					t.Fatalf("the failed compact Operation carries an input entry: %+v", input)
				}
			}
			if id, rerr := compactionIDOf(store, s); rerr != nil || id != "" {
				t.Fatalf("compaction_entry_id = (%q, %v), want the previous projection current", id, rerr)
			}
			// Subsequent messages never see a synthetic compact request.
			var sent compactRequestRecords
			f.setConvScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
				sent.add(req)
				return lifecycleTextTurn("done"), nil
			})
			f.submit(s, "op-1", "after empty")
			awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
			awaitIdleSession(t, f.r, s)
			assertCompactShapes(t, compactShapesOf(sent.at(0).Messages), []compactMessageShape{
				{role: "system", text: "compact-sys"},
				{role: "user", text: "after empty"},
			})
			if err := f.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})

	t.Run("existing summary and later messages both reach the next summary", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			f := openCompactLifecycle(t, store)
			s := f.session("manual-shape")
			f.submit(s, "op-1", "first question")
			awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
			awaitIdleSession(t, f.r, s)
			f.setCompactScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
				return compactSummaryTurn("first summary", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			})
			f.compactIdle(s, "compact-1")
			awaitOperation(t, f.r, s, "compact-1", harness.OperationSuccess)
			f.submit(s, "op-2", "later question")
			awaitOperation(t, f.r, s, "op-2", harness.OperationSuccess)
			awaitIdleSession(t, f.r, s)

			var pieces compactRequestRecords
			f.setCompactScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
				pieces.add(req)
				return compactSummaryTurn("second summary", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			})
			f.compactIdle(s, "compact-2")
			awaitOperation(t, f.r, s, "compact-2", harness.OperationSuccess)
			if pieces.count() != 1 {
				t.Fatalf("compact transport calls = %d, want one piece", pieces.count())
			}
			lines := compactPieceMessages(t, pieces.at(0).Messages[1].TextContent())
			var sawSummary, sawLater, sawTurn bool
			for _, line := range lines {
				text := ""
				if len(line.Content) > 0 {
					text = line.Content[0].Text
				}
				if strings.Contains(text, "[Previous conversation summary]") && strings.Contains(text, "first summary") {
					sawSummary = true
				}
				if text == "later question" {
					sawLater = true
				}
				if strings.Contains(text, "done") && line.Role == "assistant" {
					sawTurn = true
				}
			}
			if !sawSummary || !sawLater || !sawTurn {
				t.Fatalf("piece lines = %+v, want the existing summary, the later message, and the assistant turn together", lines)
			}
			entries := compactCompactionEntriesOf(t, store, s)
			if len(entries) != 2 || entries[1].Summary != "second summary" || entries[1].OperationID != "compact-2" {
				t.Fatalf("compaction entries = %+v, want the second manual commit", entries)
			}
			if got := f.readSession(s).State.CompactionEntryID; got != entries[1].EntryID {
				t.Fatalf("compaction_entry_id = %q, want the newest entry %q", got, entries[1].EntryID)
			}
			if err := f.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})

	t.Run("a message submitted after admission stays buffered for the post-terminal drain", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			f := openCompactLifecycle(t, store)
			s := f.session("manual-buffer")
			f.submit(s, "op-1", "hello buffered")
			awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
			awaitIdleSession(t, f.r, s)

			parked := make(chan struct{}, 1)
			gate := make(chan struct{})
			var pieces compactRequestRecords
			f.setCompactScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
				pieces.add(req)
				select {
				case parked <- struct{}{}:
				default:
				}
				<-gate
				return compactSummaryTurn("buffered summary", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
			})
			f.compactIdle(s, "compact-buffer")
			<-parked // the snapshot is frozen: the piece request is in flight
			before := entrySnapshot(t, store, s)
			if got := f.submitMode(s, "op-late", "late message", harness.MessageModeRegular); got != harness.DispositionSteering {
				t.Fatalf("late submit = %q, want the buffered steering disposition", got)
			}
			// The buffered message committed nothing and never entered the
			// manual snapshot.
			if after := entrySnapshot(t, store, s); len(after) != len(before) {
				t.Fatalf("entries changed %d -> %d while the compact Operation ran, want the buffered message to commit nothing", len(before), len(after))
			}
			if _, rerr := store.ReadRegister(ctx, harness.RegisterKey{SessionID: s, Kind: harness.RegisterOperation, OperationID: "op-late"}); !errors.Is(rerr, harness.ErrNotFound) {
				t.Fatalf("the buffered message was admitted (%v), want it buffered", rerr)
			}
			if strings.Contains(pieces.at(0).Messages[1].TextContent(), "late message") {
				t.Fatalf("the manual snapshot swallowed the post-admission message: %q", pieces.at(0).Messages[1].TextContent())
			}
			close(gate)
			awaitOperation(t, f.r, s, "compact-buffer", harness.OperationSuccess)
			awaitOperation(t, f.r, s, "op-late", harness.OperationSuccess)
			// The drained message delivered after the compaction against the
			// new projection.
			inputs := lifecycleInputsOf(t, store, s)
			var lateCommitted bool
			for _, input := range inputs {
				if input.text == "late message" && input.operationID == "op-late" {
					lateCommitted = true
				}
			}
			if !lateCommitted {
				t.Fatalf("committed inputs = %+v, want the late message under its own post-terminal admission", inputs)
			}
			if err := f.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})

	t.Run("a live interrupt of a manual compact leaves the previous projection current", func(t *testing.T) {
		eachPrepStore(t, func(t *testing.T, store harness.Storage) {
			ctx := context.Background()
			f := openCompactLifecycle(t, store)
			s := f.session("manual-interrupt")
			f.submit(s, "op-1", "hello manual")
			awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)

			// The prior compaction: one manual compact through the
			// fixture's default compact script.
			f.compactIdle(s, "compact-1")
			awaitOperation(t, f.r, s, "compact-1", harness.OperationSuccess)
			prior := compactCompactionEntriesOf(t, store, s)
			if len(prior) != 1 {
				t.Fatalf("prior compaction entries = %d, want the one seeded commit", len(prior))
			}
			priorID := prior[0].EntryID

			// The compact script is bound to the parked version before the
			// second admission: the seeding compact has already settled, so
			// the binding is deterministic.
			parked := make(chan struct{}, 1)
			f.setCompactScript(s, func(scriptCtx context.Context, req model.Request) (model.Stream, error) {
				select {
				case parked <- struct{}{}:
				default:
				}
				<-scriptCtx.Done()
				return nil, scriptCtx.Err()
			})
			f.compactIdle(s, "compact-2")
			<-parked
			if err := f.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				return h.Interrupt(ctx, s)
			}); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			awaitOperation(t, f.r, s, "compact-2", harness.OperationInterruption)

			// Exactly the prior compaction entry survives — no new entry
			// committed — and the register still names the prior summary
			// with the session's current Operation cleared.
			ids := compactCompactionEntriesOf(t, store, s)
			if len(ids) != 1 || ids[0].EntryID != priorID {
				t.Fatalf("compaction entries = %+v, want only the prior entry %q", ids, priorID)
			}
			currentOp, compactionID, _ := compactSessionRegisterState(t, store, s)
			if compactionID != priorID {
				t.Fatalf("compaction_entry_id = %q, want the prior summary %q still current", compactionID, priorID)
			}
			if currentOp != "" {
				t.Fatalf("current_operation_id = %q, want the interrupted compact Operation cleared", currentOp)
			}
			if err := f.r.Close(ctx); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})
}

// --- the raw durable fixture for recovery and corruption ---

// compactRawSessionRegister builds one open root Session register payload
// with empty usage; currentOp and compactionEntryID are omitted when empty.
func compactRawSessionRegister(sessionID, currentOp, compactionEntryID string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	current := ""
	if currentOp != "" {
		current = fmt.Sprintf(`,"current_operation_id":%q`, currentOp)
	}
	compaction := ""
	if compactionEntryID != "" {
		compaction = fmt.Sprintf(`,"compaction_entry_id":%q`, compactionEntryID)
	}
	return fmt.Sprintf(
		`{"identity":{"session_id":%q,"workspace":"/tmp/compact-works","created_at":%q},`+
			`"state":{"lifecycle":"open","current_agent_type":"solo"%s%s,"usage":{"by_model":[]},"last_activity":%q}}`,
		sessionID, now, current, compaction, now)
}

// compactRawMessageAdmission builds one message-kind admission payload.
func compactRawMessageAdmission(sessionID, operationID, entryID string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf(
		`{"session_id":%q,"operation_id":%q,"request_kind":"message",`+
			`"admitted_entry":{"session_id":%q,"entry_id":%q},"agent_type":"solo",`+
			`"execution":{"configuration_revision":"rev-1","model":{"provider":"prov","model":"m"},`+
			`"context_window":4096,"output_reserve":1024,`+
			`"system_prompt":"compact-sys","tools":[{"name":"noop","description":"tool noop","parameters":{"type":"object"}}],"readonly":false,"write_dir":"",`+
			`"compact":{"model":{"provider":"prov","model":"m"},"context_window":2048,"output_reserve":1024,"system_prompt":"summarize"}},"admitted_at":%q}`,
		sessionID, operationID, sessionID, entryID, now)
}

// compactRawCompactAdmission builds one compact-kind admission payload with
// the admitted input member omitted.
func compactRawCompactAdmission(sessionID, operationID string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf(
		`{"session_id":%q,"operation_id":%q,"request_kind":"compact",`+
			`"agent_type":"solo",`+
			`"execution":{"configuration_revision":"rev-1","model":{"provider":"prov","model":"m"},`+
			`"context_window":4096,"output_reserve":1024,`+
			`"system_prompt":"compact-sys","tools":[{"name":"noop","description":"tool noop","parameters":{"type":"object"}}],"readonly":false,"write_dir":"",`+
			`"compact":{"model":{"provider":"prov","model":"m"},"context_window":2048,"output_reserve":1024,"system_prompt":"summarize"}},"admitted_at":%q}`,
		sessionID, operationID, now)
}

func compactRawOperationRegister(admission, state string) string {
	return fmt.Sprintf(`{"admission":%s,"state":%s}`, admission, state)
}

func compactRawRunningModelState(now, reserved string) string {
	return fmt.Sprintf(
		`{"status":"running","started_at":%q,"active_effect":{"kind":"model","result_entry_id":%q},"pending_tool_calls":[],"usage":{"by_model":[]}}`,
		now, reserved)
}

func compactRawSuccessState(now, settlementEntry, sessionID string) string {
	return fmt.Sprintf(
		`{"status":"success","started_at":%q,"settled_at":%q,"pending_tool_calls":[],"usage":{"by_model":[]},"terminal":{"settlement_entry":{"session_id":%q,"entry_id":%q}}}`,
		now, now, sessionID, settlementEntry)
}

func compactRawInputEntry(sessionID, entryID, operationID, text string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"origin":"user","content":[{"kind":"text","text":%q}]}`,
		sessionID, entryID, operationID, text)
}

func compactRawAssistantEntry(sessionID, entryID, operationID, text string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"status":"completed",`+
			`"source":{"provider":"prov","model":"m"},"content":[{"kind":"text","text":%q}],"tool_calls":[]}`,
		sessionID, entryID, operationID, text)
}

func compactRawSettlementEntry(sessionID, entryID, operationID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"status":"success"}`,
		sessionID, entryID, operationID)
}

func compactRawCompactionEntry(sessionID, entryID, operationID, summary, boundaryID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"summary":%q,"boundary_entry_id":%q,`+
			`"model":{"provider":"prov","model":"m"},"configuration_revision":"rev-1"}`,
		sessionID, entryID, operationID, summary, boundaryID)
}

// compactRawSuccessOperation seeds one settled success message Operation:
// its input and assistant entries plus the settlement entry are inserted in
// order and the register carries the matching terminal section. It returns
// the assistant entry's identity; the input and settlement identities stay
// local to the seeding.
func compactRawSuccessOperation(t *testing.T, store harness.Storage, sessionID, operationID, inputText, assistantText string) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	inputID, assistantID, settlementID := newLifecycleID(t), newLifecycleID(t), newLifecycleID(t)
	lifecycleInsertEntry(t, store, sessionID, inputID, operationID, harness.EntryInput, compactRawInputEntry(sessionID, inputID, operationID, inputText))
	lifecycleInsertEntry(t, store, sessionID, assistantID, operationID, harness.EntryAssistant, compactRawAssistantEntry(sessionID, assistantID, operationID, assistantText))
	lifecycleInsertEntry(t, store, sessionID, settlementID, operationID, harness.EntryOperationSettlement, compactRawSettlementEntry(sessionID, settlementID, operationID))
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID},
		compactRawOperationRegister(compactRawMessageAdmission(sessionID, operationID, inputID), compactRawSuccessState(now, settlementID, sessionID)))
	return assistantID
}

// TestCompactLifecycleRecoverRunningCompaction proves the recovery row on the
// quiescent durable fixture: a running compact Operation with a committed
// model-effect intent and no settlement — seeded raw, recovered before any
// live Harness — settles as the runtime-loss interruption with no compaction
// entry and the previous projection current, idempotently, and the next
// admission works.
func TestCompactLifecycleRecoverRunningCompaction(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		sessionID := newLifecycleID(t)
		compactionID := newLifecycleID(t)

		// The pre-loss state: two settled message Operations whose second
		// turn followed a committed compaction entry, plus the running
		// compact Operation holding a model-effect intent with no settlement.
		lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession}, compactRawSessionRegister(sessionID, "compact-op", compactionID))
		assistant1 := compactRawSuccessOperation(t, store, sessionID, "op-1", "first question", "first answer")
		now := time.Now().UTC().Format(time.RFC3339Nano)
		lifecycleInsertEntry(t, store, sessionID, compactionID, "op-1", harness.EntryCompaction,
			compactRawCompactionEntry(sessionID, compactionID, "op-1", "seeded summary", assistant1))
		compactRawSuccessOperation(t, store, sessionID, "op-2", "after the summary", "later answer")
		reserved := newLifecycleID(t)
		lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "compact-op"},
			compactRawOperationRegister(compactRawCompactAdmission(sessionID, "compact-op"), compactRawRunningModelState(now, reserved)))

		preEntries := entrySnapshot(t, store, sessionID)

		if err := harness.Recover(ctx, store); err != nil {
			t.Fatalf("Recover: %v", err)
		}

		// The running compact Operation settled as the runtime-loss
		// interruption with no active effect.
		reg, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "compact-op"})
		if err != nil {
			t.Fatalf("ReadRegister(compact-op): %v", err)
		}
		var opWire struct {
			State struct {
				Status       harness.OperationState `json:"status"`
				ActiveEffect *harness.ActiveEffect  `json:"active_effect"`
				Terminal     *struct {
					Detail string `json:"detail"`
				} `json:"terminal"`
			} `json:"state"`
		}
		if err := json.Unmarshal(reg.Payload, &opWire); err != nil {
			t.Fatalf("decode operation register: %v", err)
		}
		if opWire.State.Status != harness.OperationInterruption || opWire.State.ActiveEffect != nil || opWire.State.Terminal == nil {
			t.Fatalf("recovered operation state = %+v, want terminal interruption with no active effect", opWire.State)
		}
		if opWire.State.Terminal.Detail != "Operation interrupted by Runtime loss." {
			t.Fatalf("recovered detail = %q, want the runtime-loss interruption detail", opWire.State.Terminal.Detail)
		}
		// No compaction entry was committed, and the previous projection
		// stays current.
		if ids := compactCompactionEntriesOf(t, store, sessionID); len(ids) != 1 || ids[0].EntryID != compactionID {
			t.Fatalf("compaction entries = %+v, want only the seeded one", ids)
		}
		var sessionWire struct {
			State struct {
				CurrentOperationID string `json:"current_operation_id"`
				CompactionEntryID  string `json:"compaction_entry_id"`
			} `json:"state"`
		}
		sreg, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
		if err != nil {
			t.Fatalf("ReadRegister(session): %v", err)
		}
		if err := json.Unmarshal(sreg.Payload, &sessionWire); err != nil {
			t.Fatalf("decode session register: %v", err)
		}
		if sessionWire.State.CurrentOperationID != "" || sessionWire.State.CompactionEntryID != compactionID {
			t.Fatalf("recovered session state = %+v, want the cleared current Operation and the previous projection", sessionWire.State)
		}
		// The seeded entries stayed byte-identical and the recovery committed
		// only the signal and the settlement.
		afterEntries := entrySnapshot(t, store, sessionID)
		if len(afterEntries) != len(preEntries)+2 {
			t.Fatalf("recovered entries = %d, want the seeded %d plus the signal and the settlement", len(afterEntries), len(preEntries))
		}
		for i := range preEntries {
			if afterEntries[i].ID != preEntries[i].ID || afterEntries[i].Kind != preEntries[i].Kind || string(afterEntries[i].Payload) != string(preEntries[i].Payload) {
				t.Fatalf("seeded entry %d changed through recovery", i)
			}
		}
		if afterEntries[len(afterEntries)-2].Kind != harness.EntrySignal || afterEntries[len(afterEntries)-1].Kind != harness.EntryOperationSettlement {
			t.Fatalf("recovery tail kinds = (%s, %s), want the signal and the settlement",
				afterEntries[len(afterEntries)-2].Kind, afterEntries[len(afterEntries)-1].Kind)
		}

		// A re-run of recovery writes nothing.
		beforeRerun := registerSnapshot(t, store, sessionID)
		if err := harness.Recover(ctx, store); err != nil {
			t.Fatalf("second Recover: %v", err)
		}
		assertRegistersUnchanged(t, beforeRerun, registerSnapshot(t, store, sessionID))
		assertEntriesUnchanged(t, afterEntries, entrySnapshot(t, store, sessionID))

		// The next admission works over the recovered state: the fresh
		// request projects the previous summary and the post-boundary
		// entries, and fits the sized window.
		f := openCompactLifecycle(t, store)
		messages := []model.Message{
			sysMessageText("compact-sys"),
			{Role: model.RoleAssistant, Source: compactConversationRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\nseeded summary\n\n[End of summary. Continue from here.]"}}},
			userMessageText("after the summary"),
			assistantMessageText("later answer"),
			userMessageText("<system-signal>Operation interrupted.</system-signal>"),
			userMessageText("next admission"),
		}
		f.setCapture(compactWindowFor(messages, 64, false), 64, "compact-sys", harness.CompactCapture{
			Model: compactConversationRef, ContextWindow: 1 << 20, OutputReserve: 1024, SystemPrompt: "summarize",
		})
		var sent compactRequestRecords
		f.setConvScript(sessionID, func(_ context.Context, req model.Request) (model.Stream, error) {
			sent.add(req)
			return lifecycleTextTurn("done"), nil
		})
		f.submit(sessionID, "op-next", "next admission")
		awaitOperation(t, f.r, sessionID, "op-next", harness.OperationSuccess)
		assertCompactShapes(t, compactShapesOf(sent.at(0).Messages), []compactMessageShape{
			{role: "system", text: "compact-sys"},
			{role: "assistant", text: "[Previous conversation summary]\n\nseeded summary\n\n[End of summary. Continue from here.]", source: compactConversationRef},
			{role: "user", text: "after the summary"},
			{role: "assistant", text: "later answer", source: compactConversationRef},
			{role: "user", text: "<system-signal>Operation interrupted.</system-signal>"},
			{role: "user", text: "next admission"},
		})
		if err := f.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestCompactLifecycleCancelMidCompaction proves the live-cancellation row:
// a piece parked inside the compact transport settles the Operation as the
// interruption with no compaction entry when the execution is canceled.
func TestCompactLifecycleCancelMidCompaction(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		f := openCompactLifecycle(t, store)
		s := f.session("cancel")
		f.submit(s, "op-1", compactSeedInput())
		awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
		awaitIdleSession(t, f.r, s)

		messages := []model.Message{
			sysMessageText("compact-sys"),
			userMessageText(compactSeedInput()),
			assistantMessageText("done"),
			userMessageText("second question"),
		}
		f.setCapture(compactWindowFor(messages, 64, true), 64, "compact-sys", harness.CompactCapture{
			Model: compactConversationRef, ContextWindow: 1 << 20, OutputReserve: 1024, SystemPrompt: "summarize",
		})
		// Phase 1: a successful first compaction commits the prior summary
		// the interrupted compaction must leave current.
		var sent compactRequestRecords
		f.setConvScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
			sent.add(req)
			return lifecycleTextTurn("done"), nil
		})
		f.setCompactScript(s, func(context.Context, model.Request) (model.Stream, error) {
			return compactSummaryTurn("prior summary", model.Usage{InputTokens: 1, OutputTokens: 1}), nil
		})
		f.submit(s, "op-2", "second question")
		awaitOperation(t, f.r, s, "op-2", harness.OperationSuccess)
		prior := compactCompactionEntriesOf(t, store, s)
		if len(prior) != 1 {
			t.Fatalf("prior compaction entries = %d, want the one successful commit", len(prior))
		}
		priorID := prior[0].EntryID
		awaitIdleSession(t, f.r, s)

		// Phase 2: the interrupted compaction runs against the summarized
		// session; the window is sized against the new huge request.
		huge := compactTextWithTokens("word ", 20000)
		f.setCapture(compactWindowFor([]model.Message{
			sysMessageText("compact-sys"),
			{Role: model.RoleAssistant, Source: compactConversationRef, Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\nprior summary\n\n[End of summary. Continue from here.]"}}},
			userMessageText(huge),
		}, 64, true), 64, "compact-sys", harness.CompactCapture{
			Model: compactConversationRef, ContextWindow: 1 << 20, OutputReserve: 1024, SystemPrompt: "summarize",
		})
		f.setConvScript(s, func(context.Context, model.Request) (model.Stream, error) {
			return nil, errors.New("the conversation transport ran for an interrupted compaction")
		})
		parked := make(chan struct{}, 1)
		f.setCompactScript(s, func(scriptCtx context.Context, req model.Request) (model.Stream, error) {
			select {
			case parked <- struct{}{}:
			default:
			}
			<-scriptCtx.Done()
			return nil, scriptCtx.Err()
		})
		f.submit(s, "op-3", huge)
		<-parked
		if err := f.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			return h.Interrupt(ctx, s)
		}); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		awaitOperation(t, f.r, s, "op-3", harness.OperationInterruption)

		if sent.count() != 1 {
			t.Fatalf("conversation transport calls = %d, want only the first compaction's post-commit request", sent.count())
		}
		// The prior summary survives the cancellation: no compaction entry
		// committed for the interrupted Operation and the register still
		// names the prior one.
		ids := compactCompactionEntriesOf(t, store, s)
		if len(ids) != 1 || ids[0].EntryID != priorID {
			t.Fatalf("compaction entries = %+v, want only the prior entry %q", ids, priorID)
		}
		if id, err := compactionIDOf(store, s); err != nil || id != priorID {
			t.Fatalf("compaction_entry_id = (%q, %v), want the prior summary %q still current", id, err, priorID)
		}
		if err := f.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestCompactLifecycleCorruptCompactionEntry proves the corruption row: a
// compaction entry with a malformed payload, inserted raw, makes the Session
// unavailable through public reads while a valid sibling stays usable.
func TestCompactLifecycleCorruptCompactionEntry(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		corrupt := newLifecycleID(t)
		sibling := newLifecycleID(t)
		lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: corrupt, Kind: harness.RegisterSession}, compactRawSessionRegister(corrupt, "", ""))
		lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: sibling, Kind: harness.RegisterSession}, compactRawSessionRegister(sibling, "", ""))
		// The otherwise-valid Session's one poisoned record: a compaction
		// entry whose payload decodes nothing.
		lifecycleInsertEntry(t, store, corrupt, newLifecycleID(t), "", harness.EntryCompaction, `{"bogus":1}`)

		f := openCompactLifecycle(t, store)
		err := f.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ReadSession(ctx, corrupt)
			return err
		})
		var corruptErr *harness.CorruptionError
		if !errors.As(err, &corruptErr) || !errors.Is(err, harness.ErrCorrupt) {
			t.Fatalf("ReadSession(corrupt) = %v, want a corruption error", err)
		}
		if got := f.readSession(sibling).Identity.SessionID; got != sibling {
			t.Fatalf("sibling session read = %q, want %q usable", got, sibling)
		}
		if err := f.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestCompactLifecycleForkAcrossCompaction proves the fork row: after an
// automatic compaction, forking the idle compacted source through the public
// method copies the compaction entry without usage and operation ownership,
// rewrites its boundary within the copied prefix, recreates the destination
// projection at the copied identity, and leaves the source unchanged.
func TestCompactLifecycleForkAcrossCompaction(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		f := openCompactLifecycle(t, store)
		s := f.session("fork-source")
		f.submit(s, "op-1", compactSeedInput())
		awaitOperation(t, f.r, s, "op-1", harness.OperationSuccess)
		awaitIdleSession(t, f.r, s)

		messages := []model.Message{
			sysMessageText("compact-sys"),
			userMessageText(compactSeedInput()),
			assistantMessageText("done"),
			userMessageText("second question"),
		}
		f.setCapture(compactWindowFor(messages, 64, true), 64, "compact-sys", harness.CompactCapture{
			Model: compactConversationRef, ContextWindow: 1 << 20, OutputReserve: 1024, SystemPrompt: "summarize",
		})
		f.setCompactScript(s, func(_ context.Context, req model.Request) (model.Stream, error) {
			return compactSummaryTurn("summary one", model.Usage{InputTokens: 10, OutputTokens: 5}), nil
		})
		f.submit(s, "op-2", "second question")
		awaitOperation(t, f.r, s, "op-2", harness.OperationSuccess)
		awaitIdleSession(t, f.r, s)
		// The fork boundary must sit after the compaction entry so the
		// copied prefix carries it: one more fitting turn.
		f.submit(s, "op-3", "fork boundary")
		awaitOperation(t, f.r, s, "op-3", harness.OperationSuccess)
		awaitIdleSession(t, f.r, s)

		sourceEntriesBefore := entrySnapshot(t, store, s)
		sourceRegistersBefore := registerSnapshot(t, store, s)
		sourceCompactions := compactCompactionEntriesOf(t, store, s)
		if len(sourceCompactions) != 1 {
			t.Fatalf("source compaction entries = %d, want the one automatic commit", len(sourceCompactions))
		}
		boundaryID := ""
		// The fork boundary is op-3's user-origin input entry, committed
		// after the compaction entry.
		for _, entry := range sourceEntriesBefore {
			if entry.Kind != harness.EntryInput {
				continue
			}
			var wire struct {
				OperationID string `json:"operation_id"`
				Content     []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(entry.Payload, &wire); err == nil && wire.OperationID == "op-3" {
				boundaryID = entry.ID
			}
		}
		if boundaryID == "" {
			t.Fatalf("no op-3 input entry found on the source: %+v", sourceEntriesBefore)
		}

		// The destination's fresh admission must not compact: size the
		// capture for the forked work, not the source's overflowing window.
		f.setCapture(1<<30, 1024, "compact-sys", harness.CompactCapture{
			Model: compactConversationRef, ContextWindow: 1 << 20, OutputReserve: 1024, SystemPrompt: "summarize",
		})
		var result harness.ForkResult
		if err := f.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			var err error
			result, err = h.Fork(ctx, harness.ForkRequest{
				SourceSessionID: s,
				BoundaryEntryID: boundaryID,
				OperationID:     "fork-op-1",
				Content:         []model.ContentPart{{Kind: model.PartText, Text: "forked"}},
			})
			return err
		}); err != nil {
			t.Fatalf("Fork: %v", err)
		}
		dest := result.Session.Identity.SessionID
		awaitOperation(t, f.r, dest, "fork-op-1", harness.OperationSuccess)

		// The destination projection is recreated at the copied identity.
		if got := f.readSession(dest).State.CompactionEntryID; got == "" || got == sourceCompactions[0].EntryID {
			t.Fatalf("destination compaction_entry_id = %q, want a fresh copied identity", got)
		}
		// The copied compaction entry: fresh identity, no operation
		// ownership, no usage, the boundary rewritten within the copied
		// prefix, the revision kept.
		boundaryIndex := -1
		for i, entry := range sourceEntriesBefore {
			if entry.ID == boundaryID {
				boundaryIndex = i
			}
		}
		if boundaryIndex < 0 {
			t.Fatalf("the fork boundary %q is not among the source entries", boundaryID)
		}
		destEntries := f.entries(dest)
		var compactions []*compactWireEntry
		var compactionIDs []string
		for i := range destEntries {
			if destEntries[i].Kind != harness.EntryCompaction {
				continue
			}
			var wire compactWireEntry
			if err := json.Unmarshal(destEntries[i].Payload, &wire); err != nil {
				t.Fatalf("decode copied compaction entry: %v", err)
			}
			compactions = append(compactions, &wire)
			compactionIDs = append(compactionIDs, destEntries[i].ID)
		}
		if len(compactions) != 1 {
			t.Fatalf("destination compaction entries = %d, want exactly the copied one", len(compactions))
		}
		copied, copiedEntryID := compactions[0], compactionIDs[0]
		if copiedEntryID == sourceCompactions[0].EntryID {
			t.Fatalf("the copied entry kept the source identity %q", copiedEntryID)
		}
		if copied.OperationID != "" {
			t.Fatalf("copied entry operation = %q, want no operation ownership", copied.OperationID)
		}
		if copied.Usage != nil {
			t.Fatalf("copied entry usage = %+v, want none", copied.Usage)
		}
		if copied.Summary != "summary one" {
			t.Fatalf("copied summary = %q, want the source summary", copied.Summary)
		}
		// The copied prefix preserves the source's order: the destination's
		// first entries map one-to-one onto the source's strict-before-
		// boundary prefix, excluding the kinds the copy drops (Operation
		// settlements). The copied boundary must be exactly the mapped
		// copy of the source's boundary entry.
		var expectedCopied []harness.Entry
		for _, entry := range sourceEntriesBefore[:boundaryIndex] {
			switch entry.Kind {
			case harness.EntryInput, harness.EntryAssistant, harness.EntryToolResult, harness.EntrySignal, harness.EntryCompaction:
				expectedCopied = append(expectedCopied, entry)
			}
		}
		if len(destEntries) <= len(expectedCopied) || destEntries[len(expectedCopied)].Kind != harness.EntryInput {
			t.Fatalf("destination entries = %+v, want the copied prefix followed by the fork's own input", destEntries)
		}
		idMap := map[string]string{}
		for i, entry := range expectedCopied {
			if destEntries[i].Kind != entry.Kind {
				t.Fatalf("copied prefix %d = %s, want the source %s's %s", i, destEntries[i].Kind, entry.ID, entry.Kind)
			}
			idMap[entry.ID] = destEntries[i].ID
		}
		expectedBoundary, ok := idMap[sourceCompactions[0].BoundaryEntryID]
		if !ok {
			t.Fatalf("the source boundary %q is not inside the copied prefix", sourceCompactions[0].BoundaryEntryID)
		}
		if copied.BoundaryEntryID != expectedBoundary {
			t.Fatalf("copied boundary %q != the mapped copy of the source boundary %q", copied.BoundaryEntryID, expectedBoundary)
		}
		if copied.ConfigurationRevision != sourceCompactions[0].ConfigurationRevision {
			t.Fatalf("copied revision = %q, want the kept source revision", copied.ConfigurationRevision)
		}
		// The copied entry contributes no usage to the destination registers.
		if totals := f.readSession(dest).State.Usage; len(totals.ByModel) != 0 {
			t.Fatalf("destination usage = %+v, want no inherited source usage", totals)
		}
		// The source is unchanged.
		assertEntriesUnchanged(t, sourceEntriesBefore, entrySnapshot(t, store, s))
		assertRegistersUnchanged(t, sourceRegistersBefore, registerSnapshot(t, store, s))

		if err := f.r.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// --- the captured configuration row over the real preparation ---

// TestCompactLifecycleCapturedCompactionConfiguration proves the R6 row end
// to end over the real Open path: the prepared capture carries the compact
// configuration from one revision — the durable admission records exactly
// what the hook saw — a preparation hook attempting to change a compact
// member is rejected at admission, and the prompt and tool-definition
// replacement freedoms still hold.
func TestCompactLifecycleCapturedCompactionConfiguration(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		var seenMu sync.Mutex
		var seenCompact harness.CompactCapture
		var seenRevision string
		hook := &recordHook{name: "hook.first", events: &traceLog{}, mutate: func(c harness.ExecutionCapture) harness.ExecutionCapture {
			seenMu.Lock()
			seenCompact, seenRevision = c.Compact, c.ConfigurationRevision
			seenMu.Unlock()
			return c
		}}
		// The freed phase runs on its own hook instance: one mutation phase
		// per instance removes the temporal-assignment race, because the
		// first instance's COMPACT mutation is never overwritten.
		freedHook := &recordHook{name: "hook.freed", events: &traceLog{}}
		writeServiceFile(t, agents.PathForConfig(e.configPath), productionAgentsWithFreedHook())
		r, err := e.openWithPlugins(ctx, hook, &parkingHook{}, freedHookPlugin(freedHook))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("compact-capture"), "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sessionID := session.Identity.SessionID

		// The capture carries the compact configuration from one revision.
		submitThroughRuntime(t, r, sessionID, "op-1", "please write")
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		awaitIdleSession(t, r, sessionID)
		var durable harness.ExecutionCapture
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			rec, err := h.ReadOperation(ctx, sessionID, "op-1")
			if err != nil {
				return err
			}
			durable = rec.Admission.Execution
			return nil
		}); err != nil {
			t.Fatalf("ReadOperation: %v", err)
		}
		seenMu.Lock()
		seen := seenCompact
		revision := seenRevision
		seenMu.Unlock()
		if seen != durable.Compact {
			t.Fatalf("hook-saw compact capture %+v differs from the durable %+v", seen, durable.Compact)
		}
		if revision != durable.ConfigurationRevision {
			t.Fatalf("hook-saw revision %q differs from the durable %q", revision, durable.ConfigurationRevision)
		}
		if seen.Model.Provider == "" || seen.Model.Model == "" || seen.ContextWindow <= 0 || seen.OutputReserve <= 0 || seen.SystemPrompt == "" {
			t.Fatalf("captured compact configuration incomplete: %+v", seen)
		}

		// A hook attempting to change a compact member is rejected. The
		// mutation stays installed on this session for the phase's whole
		// life and is never overwritten before op-2's preparation consumes
		// it — synchronous at the submit, or drain-deferred inside the
		// retiring run's one delivery — so op-2 is rejected and dropped in
		// every interleaving and its register never exists.
		hook.set(nil, func(c harness.ExecutionCapture) harness.ExecutionCapture {
			c.Compact.SystemPrompt = "changed"
			return c
		})
		// The convergent-outcome tolerance: a direct rejection fails the
		// Submit, and a submit landing in the retiring-run window is
		// buffered as steering whose drain re-prepares with the mutated
		// capture, whose admission rejects and whose item is dropped — the
		// never-admitted check below pins every path, so only an accepted
		// admission fails the row.
		var rejected harness.SubmitResult
		err = r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			var serr error
			rejected, serr = h.Submit(ctx, harness.SubmitRequest{
				SessionID:   sessionID,
				OperationID: "op-2",
				Origin:      harness.InputOriginUser,
				Content:     []model.ContentPart{{Kind: model.PartText, Text: "please write"}},
				Mode:        harness.MessageModeRegular,
			})
			return serr
		})
		if err == nil && rejected.Disposition == harness.DispositionAdmitted {
			t.Fatalf("a hook changing the compact system prompt was accepted (disposition %q), want rejection", rejected.Disposition)
		}
		if _, rerr := e.store.ReadRegister(ctx, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "op-2"}); !errors.Is(rerr, harness.ErrNotFound) {
			t.Fatalf("the rejected admission left a register (%v), want none", rerr)
		}

		// The prompt and tool-definition replacement freedoms hold on a
		// fresh session under the second hook instance: the FREED mutation
		// is installed on that instance before op-3's submit and never
		// overwritten, so op-3's preparation — direct, on a fresh session
		// with no retiring run — always reads it and admits in every
		// interleaving, while session A's drain-deferred op-2 preparation
		// keeps reading the first instance's COMPACT mutation.
		freedHook.set(nil, func(c harness.ExecutionCapture) harness.ExecutionCapture {
			c.SystemPrompt += "|freed"
			c.Tools = append([]model.ToolDefinition(nil), c.Tools...)
			for i := range c.Tools {
				c.Tools[i].Description = "freed description"
			}
			return c
		})
		freedSession, err := r.createSession(ctx, e.workspace("compact-freed"), "freedworker")
		if err != nil {
			t.Fatalf("createSession(freed): %v", err)
		}
		freedID := freedSession.Identity.SessionID
		var freed harness.SubmitResult
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			var serr error
			freed, serr = h.Submit(ctx, harness.SubmitRequest{
				SessionID:   freedID,
				OperationID: "op-3",
				Origin:      harness.InputOriginUser,
				Content:     []model.ContentPart{{Kind: model.PartText, Text: "please write"}},
				Mode:        harness.MessageModeRegular,
			})
			return serr
		}); err != nil {
			t.Fatalf("submit op-3: %v", err)
		}
		if freed.Disposition != harness.DispositionAdmitted {
			t.Fatalf("submit op-3 disposition = %q, want admission on the fresh session", freed.Disposition)
		}
		awaitOperation(t, r, freedID, "op-3", harness.OperationSuccess)
		if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			rec, err := h.ReadOperation(ctx, freedID, "op-3")
			if err != nil {
				return err
			}
			if !strings.Contains(rec.Admission.Execution.SystemPrompt, "|freed") {
				t.Fatalf("capture prompt = %q, want the hook's replacement", rec.Admission.Execution.SystemPrompt)
			}
			for _, tool := range rec.Admission.Execution.Tools {
				if tool.Description != "freed description" {
					t.Fatalf("tool %q description = %q, want the hook's replacement", tool.Name, tool.Description)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("ReadOperation(op-3): %v", err)
		}
	})
}

// freedHookPlugin composes the second hook instance: its own plugin
// identity and capability spec, so only the agent type selecting this spec
// runs the instance's mutation and the first instance's mutation is never
// overwritten by the freed phase.
func freedHookPlugin(hook *recordHook) Plugin {
	return Plugin{
		ID:       "freedhooks",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[PreparationHook]("hook.freed")},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"hook.freed": hook}}, nil
		},
	}
}

// productionAgentsWithFreedHook extends the production agents document with
// one agent type selecting the second hook instance's capability spec.
func productionAgentsWithFreedHook() string {
	return strings.TrimSuffix(productionAgentsDocument, "}") + `,
  "freedworker": {"model": "prov/m", "system_prompt": "simple", "tools": ["prod_write", "prod_read"], "capabilities": ["hook.freed"]}}`
}

// --- the protocol and client surface pin ---

// TestCompactLifecycleRuntimeSurfaceUnchanged pins the runtime package's
// exported declaration set against the fixed pre-compaction set — top-level
// exported declarations under their bare names and exported methods on
// exported receiver types under receiver-qualified names, with pointer and
// value receivers distinguished in the key the same way the exported check
// distinguishes them — so an added (*Runtime) method cannot hide behind the
// top-level-only scan and same-named methods on different receivers cannot
// collide: the compaction lifecycle added no runtime-level protocol or
// client surface, and a source scan of the package's production files must
// find exactly the pinned names and no others.
func TestCompactLifecycleRuntimeSurfaceUnchanged(t *testing.T) {
	want := map[string]bool{
		"(*Runtime).Close":        true,
		"(*Runtime).Reload":       true,
		"(*Runtime).Subscribe":    true,
		"(*Subscription).Close":   true,
		"(*Subscription).Events":  true,
		"(Invocation).AgentTypes": true,
		"(Invocation).Config":     true,
		"(Invocation).Revision":   true,
		"Adaptation":              true,
		"BackgroundServices":      true,
		"Bind":                    true,
		"Bindings":                true,
		"CapabilitySpec":          true,
		"ErrClosed":               true,
		"ErrComposition":          true,
		"ErrConfiguration":        true,
		"ErrOwned":                true,
		"Event":                   true,
		"EventConfiguration":      true,
		"EventKind":               true,
		"EventScopeClosed":        true,
		"EventScopeOpened":        true,
		"Instance":                true,
		"Invocation":              true,
		"ModelAdaptation":         true,
		"Open":                    true,
		"Options":                 true,
		"Plugin":                  true,
		"PreparationHook":         true,
		"Runtime":                 true,
		"ScopeAgent":              true,
		"ScopeInfo":               true,
		"ScopeKind":               true,
		"ScopeOperation":          true,
		"ScopeRuntime":            true,
		"ScopeWorkspace":          true,
		"Spec":                    true,
		"Subscription":            true,
		"Tool":                    true,
		"ToolArgumentsHook":       true,
		"ToolConstraints":         true,
		"ToolContext":             true,
		"ToolDescription":         true,
		"ToolSpec":                true,
	}
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	got := map[string]string{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil { // exported methods count, keyed receiver-qualified
					receiver := ""
					pointer := false
					switch recv := d.Recv.List[0].Type.(type) {
					case *ast.StarExpr:
						if ident, ok := recv.X.(*ast.Ident); ok {
							receiver, pointer = ident.Name, true
						}
					case *ast.Ident:
						receiver = recv.Name
					}
					if ast.IsExported(d.Name.Name) && ast.IsExported(receiver) {
						if pointer {
							got["(*"+receiver+")."+d.Name.Name] = file
						} else {
							got["("+receiver+")."+d.Name.Name] = file
						}
					}
				} else if ast.IsExported(d.Name.Name) {
					got[d.Name.Name] = file
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if ast.IsExported(s.Name.Name) {
							got[s.Name.Name] = file
						}
					case *ast.ValueSpec:
						for _, name := range s.Names {
							if ast.IsExported(name.Name) {
								got[name.Name] = file
							}
						}
					}
				}
			}
		}
	}
	if len(files) == 0 {
		t.Fatal("the package source scan found no production files")
	}
	for name := range got {
		if !want[name] {
			t.Fatalf("exported declaration %q (%s) is outside the fixed pre-compaction surface", name, got[name])
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("pinned exported declaration %q missing from the package source scan", name)
		}
	}
}
