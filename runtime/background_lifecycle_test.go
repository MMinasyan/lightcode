package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/model"
)

// Shared durable-state seeders, deterministic storage barriers and the
// delivering job-stop seam for the runtime suite's recovery, protocol and
// shutdown rows. Every helper here writes through the storage contract or
// the Harness's own public methods; none constructs model behavior.

// lifecycleAgentsDocument adds the child agent type beside the solo root;
// both select the suite's preparation hook and declared tools.
const lifecycleAgentsDocument = `{
	"solo":   {"model": "prov/m", "system_prompt": "simple", "tools": ["read", "grep"], "capabilities": ["hook.first"]},
	"worker": {"model": "prov/m", "system_prompt": "simple", "tools": ["read", "grep"], "capabilities": ["hook.first"]}
}`

// backgroundLifecycleConfigDocument points the configured provider at the
// test server's keyless local endpoint and carries the hooks plugin section
// the selected preparation hook validates.
func backgroundLifecycleConfigDocument(endpoint string) string {
	return `{"providers":{"prov":{"transport":{"base_url":"` + endpoint + `","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":262144,"max_output_tokens":4096}}}},"plugins":{"hooks":{"tag":"T1"}}}`
}

// newLifecycleID mints one fresh 32-lowercase-hex identity.
func newLifecycleID(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("random id: %v", err)
	}
	return hex.EncodeToString(buf[:])
}

// --- the gated storage barrier ---

// gatedStore wraps one store and parks each of the next armed transactions
// after its commit and before its return — the parked caller still holds its
// coordinator lock — so a row observes the durable state at the frozen
// transaction boundary.
type gatedStore struct {
	harness.Storage
	mu       sync.Mutex
	postLeft int
	gate     chan struct{}
	parked   chan struct{}
}

func newGatedStore(base harness.Storage) *gatedStore {
	return &gatedStore{Storage: base, parked: make(chan struct{}, 4)}
}

// armPostCommit parks each of the next n transactions after its commit and
// before its return, signalling arrival per park.
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

// releaseAll drops any remaining armed parks and closes the current gate, so
// a failed row's cleanup can never leave a joined Close parked on the barrier.
func (s *gatedStore) releaseAll() {
	s.mu.Lock()
	s.postLeft = 0
	gate := s.gate
	s.gate = nil
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

// --- the delivering job-stop seam ---

// lifecycleDelivery is one completion the seam delivers when the harness
// stops its job member.
type lifecycleDelivery struct {
	sessionID    string
	completionID string
	content      string
}

// lifecycleStopper is the delivering job-stop seam: StopJob delivers every
// armed completion through the armed Harness, in one sequential pass.
type lifecycleStopper struct {
	mu         sync.Mutex
	h          *harness.Harness
	deliveries []lifecycleDelivery
}

func newLifecycleStopper() *lifecycleStopper {
	return &lifecycleStopper{}
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

func (s *lifecycleStopper) StopJob(sessionID, jobID string) {
	s.mu.Lock()
	deliveries := s.deliveries
	s.deliveries = nil
	h := s.h
	s.mu.Unlock()
	if h == nil {
		return
	}
	for _, d := range deliveries {
		_ = h.DeliverBackgroundCompletion(context.Background(), d.sessionID, d.completionID, d.content)
	}
}

// lifecycleCompletion is one decoded background_completion signal entry.
type lifecycleCompletion struct {
	memberKind string
	memberID   string
	content    string
}

// completionsOf decodes one Session's background_completion signals,
// ignoring read errors: the reader may observe a frozen transaction boundary.
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

// --- durable-state readers ---

// operationStatusOf reads one Operation register's status directly from the
// store, the only reader that works after the owner closed.
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

// corruptSessionRegister replaces one Session register with an invalid
// payload directly through the storage contract.
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

// --- raw durable-state seeders ---

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
			`"context_window":4096,"output_reserve":2048,`+
			`"system_prompt":"system","tools":[%s],"readonly":false,"write_dir":"",`+
			`"compact":{"model":{"provider":"cprov","model":"compact-x"},"context_window":2048,"output_reserve":1024,"system_prompt":"summarize"}},"admitted_at":%q}`,
		childID, operationID, childID, entryID, lifecycleSeedToolDefinition, now)
	operationPayload := fmt.Sprintf(
		`{"admission":%s,"state":{"status":"running","started_at":%q,"pending_tool_calls":[],"usage":{"by_model":[]}}}`,
		admission, now)
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: childID, Kind: harness.RegisterSession}, sessionPayload)
	lifecycleInsertEntry(t, store, childID, entryID, operationID, harness.EntryInput, inputPayload)
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: childID, Kind: harness.RegisterOperation, OperationID: operationID}, operationPayload)
}

// --- the native background fixture ---

// lifecycleTools is the background suite's declared tool pair: pure object
// normalization and one per-test-configured Prepare dispatcher, so the real
// model's tool calls exercise real prepared outcomes.
type lifecycleTools struct {
	mu      sync.Mutex
	prepare func(ctx context.Context, call model.ToolCall) harness.PreparedTool
}

func (t *lifecycleTools) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t *lifecycleTools) Prepare(ctx context.Context, _ ToolContext, call model.ToolCall) harness.PreparedTool {
	t.mu.Lock()
	prepare := t.prepare
	t.mu.Unlock()
	if prepare != nil {
		return prepare(ctx, call)
	}
	// The fixture's default: one immediate success under the declared
	// command.run pair, the settled result the plain rows expect.
	return harness.PreparedTool{
		Permissions: []harness.PermissionRequest{{Permission: "command.run", Target: "test"}},
		Immediate:   &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "ok"}},
	}
}

// setPrepare installs one per-test prepared-outcome dispatcher.
func (t *lifecycleTools) setPrepare(prepare func(ctx context.Context, call model.ToolCall) harness.PreparedTool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prepare = prepare
}

// lifecycleToolsPlugin declares the suite's read and grep tools over the one
// shared dispatcher; both are always available so the concrete surface
// advertises them and the model's calls reach the real dispatcher.
func lifecycleToolsPlugin(tools *lifecycleTools) Plugin {
	return Plugin{
		ID:       "bgtools",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("read", availableToolDescription("read")), ToolSpec("grep", availableToolDescription("grep"))},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"read": tools, "grep": tools}}, nil
		},
	}
}

// bgRuntime is the background suite's native owner: the solo and worker agent
// types over the local model endpoint, the declared read/grep tools, the
// selected preparation hook, and the job-stop seam armed on construction.
type bgRuntime struct {
	t     *testing.T
	e     *ownerEnv
	r     *Runtime
	hook  *recordHook
	tools *lifecycleTools
}

func openBackgroundRuntime(t *testing.T, store harness.Storage, stopper harness.JobStopper) *bgRuntime {
	t.Helper()
	e := newOwnerEnv(t)
	writeServiceFile(t, e.configPath, backgroundLifecycleConfigDocument(e.server.URL))
	writeServiceFile(t, agents.PathForConfig(e.configPath), lifecycleAgentsDocument)
	hook := &recordHook{name: "hook.first", events: &traceLog{}}
	tools := &lifecycleTools{}
	r, err := e.open(context.Background(),
		e.storagePlugin(store),
		jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, stopper),
		hookPlugin(hook),
		lifecycleToolsPlugin(tools))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if s, ok := stopper.(*lifecycleStopper); ok {
		s.arm(r.harness)
	}
	return &bgRuntime{t: t, e: e, r: r, hook: hook, tools: tools}
}

// session creates one root Session of the solo agent type.
func (b *bgRuntime) session(name string) string {
	b.t.Helper()
	rec, err := b.r.createSession(context.Background(), filepath.Join(b.e.home, name), "solo")
	if err != nil {
		b.t.Fatalf("createSession(%s): %v", name, err)
	}
	return rec.Identity.SessionID
}

// launchChild launches one child Session of the worker agent type through the
// Runtime's admitted-call gate.
func (b *bgRuntime) launchChild(parent, prompt, operationID string, maxConcurrent int) string {
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
func (b *bgRuntime) startJob(sessionID, jobID string) string {
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
func (b *bgRuntime) stop(sessionID string) error {
	b.t.Helper()
	return b.r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		return h.Stop(ctx, sessionID)
	})
}

// submitMode submits one message in the given mode and returns its
// disposition.
func (b *bgRuntime) submitMode(sessionID, operationID, text string, mode harness.MessageMode) harness.SubmitDisposition {
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
