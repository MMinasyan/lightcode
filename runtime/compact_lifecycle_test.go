package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// Native compaction rows over the real preparation: the compaction observer
// oracle through configured conversation and compact endpoints, the captured
// compaction configuration through real hooks, the raw durable-state builders
// the paging and recovery rows seed with, and the exported-surface pin.

// compactObservationConfigDocument configures the conversation model with a
// window whose trigger threshold the seed turn's reported usage overflows,
// and the top-level compaction model as a distinct catalog identity, so the
// wire distinguishes the transports by model ID.
func compactObservationConfigDocument(endpoint string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "",
		`{"m":{"name":"M","context_window":8192,"max_output_tokens":4096},`+
			`"k":{"name":"K","context_window":32768,"max_output_tokens":4096,"usage_in_stream":true}}`) + `},
		"compaction":{"model":"prov/k"}}`
}

// compactObservationAgentsDocument selects the conversation model for the
// working type.
const compactObservationAgentsDocument = `{
	"tight": {"model": "prov/m", "system_prompt": "simple"}
}`

// TestCompactLifecycleNativeCompactionObservation keeps the Runtime observer
// oracle through the native pipeline: the first turn's reported conversation
// usage overflows the captured window, the next admission's checkpoint runs
// one automatic compaction whose summary request reaches the compact model's
// transport — distinguished by its model ID, with the captured compact
// prompt and no tools — the committed summary turns the conversation
// projection, and the observation bus carries only well-scoped facts: the
// session invalidation at session granularity and the text progress at the
// real admitted Operations.
func TestCompactLifecycleNativeCompactionObservation(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, compactObservationConfigDocument(e.server.URL))
		writeAgentDefinitions(t, e.configPath, compactObservationAgentsDocument)
		e.fixtureStore(store)
		r, err := e.open(context.Background())
		testassert.NoError(t, err)
		ctx := context.Background()

		e.server.setScript(func(_ context.Context, body string) []string {
			if !strings.Contains(body, `"model":"k"`) && !strings.Contains(body, `"model": "k"`) {
				if lastUserText(body) == "first question" {
					// The seed report: {5000,10} overflows the window 8192
					// minus the reserve 4096.
					return append(textTurnEvents("seeded answer"),
						`{"usage":{"prompt_tokens":5000,"completion_tokens":10,"total_tokens":5010}}`)
				}
				return nil // conversation turns: the default completed text turn
			}
			return append(textTurnEvents("native summary one"),
				`{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
		})

		sub, err := r.Subscribe(256)
		testassert.NoError(t, err)
		session, err := r.createSession(ctx, e.dataDir, "tight")
		testassert.NoError(t, err)
		sessionID := session.Identity.SessionID

		submitThroughRuntime(t, r, sessionID, "op-1", "first question")
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess) // the seed turn commits the report
		submitThroughRuntime(t, r, sessionID, "op-2", "the overflowing checkpoint")
		op2 := awaitOperation(t, r, sessionID, "op-2", harness.OperationSuccess)
		testassert.Require(t, op2.State.Status == harness.OperationSuccess, "overflowing operation = %+v, want the compacted success", op2.State)
		submitConvergedThroughRuntime(t, r, sessionID, "op-3", "second question", harness.OperationSuccess)
		testassert.NoError(t, r.Close(ctx))

		// The wire: the seed conversation request, one compact summary on
		// the compact model, then the two conversation requests against the
		// summary projection.
		var compactSummary string
		var conversation []string
		for i := 0; i < e.server.requests(); i++ {
			body := e.server.bodyAt(i)
			doc := decodeWireChatBody(t, body)
			if doc.Model == "k" {
				compactSummary = body
				continue
			}
			testassert.Require(t, doc.Model == "m", "wire request %d names model %q, want one of the configured identities", i, doc.Model)
			conversation = append(conversation, body)
		}
		testassert.Require(t, compactSummary != "", "the compaction never reached the compact model's transport")
		testassert.Equal(t, len(conversation), 3)
		seed := decodeWireChatBody(t, conversation[0])
		{
			roles := wireRoles(seed)
			testassert.Require(t, slices.Equal(roles, []string{"system", "user"}), "seed request roles = %v, want the prompt and the first message", roles)
		}
		summary := decodeWireChatBody(t, compactSummary)
		testassert.Require(t, len(summary.Tools) == 0 && len(summary.Messages) != 0 && summary.Messages[0].Role == "system", "got %v, want %v; compact summary request = %+v, want the leading compact system prompt", len(summary.Tools), 0, summary.Messages)
		var systemText string
		{
			err := json.Unmarshal(summary.Messages[0].Content, &systemText)
			testassert.Require(t, err == nil && systemText == op2.Admission.Execution.Compact.SystemPrompt, "compact summary system = %q (err %v), want the captured compact prompt %q", systemText, err, op2.Admission.Execution.Compact.SystemPrompt)
		}
		testassert.Require(t, op2.Admission.Execution.Compact.Model == (model.ModelRef{Provider: "prov", Model: "k"}), "captured compact model = %+v, want the configured prov/k", op2.Admission.Execution.Compact.Model)
		first := decodeWireChatBody(t, conversation[1])
		{
			roles := wireRoles(first)
			testassert.Require(t, slices.Equal(roles, []string{"system", "assistant"}), "post-compaction request roles = %v, want the prompt and the summary alone", roles)
		}
		var summaryText string
		if len(first.Messages) > 1 {
			_ = json.Unmarshal(first.Messages[1].Content, &summaryText)
		}
		testassert.Require(t, strings.Contains(summaryText, "native summary one"), "post-compaction assistant = %q, want the committed summary", summaryText)
		second := decodeWireChatBody(t, conversation[2])
		{
			roles := wireRoles(second)
			testassert.Require(t, slices.Equal(roles, []string{"system", "assistant", "assistant", "user"}), "second request roles = %v, want the prompt, the summary, the first closing turn and the new message", roles)
		}

		// The durable state: exactly one committed compaction entry owned by
		// the overflowing Operation.
		entries, err := store.ReadEntries(ctx, sessionID, 0)
		testassert.NoError(t, err)
		compactions := 0
		for _, entry := range entries {
			if entry.Kind != harness.EntryCompaction {
				continue
			}
			compactions++
			var wire struct {
				OperationID string `json:"operation_id"`
				Messages    []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"messages"`
			}
			testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
			wireSummary := ""
			if len(wire.Messages) > 0 && len(wire.Messages[0].Content) > 0 {
				wireSummary = wire.Messages[0].Content[0].Text
			}
			testassert.Require(t, wire.OperationID == "op-2" && wireSummary == "[Previous conversation summary]\n\nnative summary one\n\n[End of summary. Continue from here.]", "compaction entry = %+v, want op-2's committed summary", wire)
		}
		testassert.Equal(t, compactions, 1)

		// The observation set: every session invalidation names the real
		// Session, every text progress the real running Operation, and no
		// other fact kind fires.
		sawInvalidation, sawDelta := false, false
		for event := range sub.Events() {
			switch eventKind(t, event) {
			case "session_changed":
				body, err := event.AsSessionChangedEvent()
				testassert.NoError(t, err)
				scope, err := body.Scope.AsSessionScope()
				testassert.NoError(t, err)
				testassert.Equal(t, scope.SessionId, sessionID)
				sawInvalidation = true
			case "text_delta":
				testassert.NoError(t, assertProgressScopedToOperations(t, event, sessionID, "op-1", "op-2", "op-3"))
				sawDelta = true
			case "scope_opened", "scope_closed", "warning_changed", "configuration_changed":
				// scope lifecycle and publication facts stay allowed beside the passive set
			default:
				t.Fatalf("unexpected event %s", eventJSON(t, event))
			}
		}
		testassert.Require(t, sawInvalidation && sawDelta, "observation set: invalidation %v delta %v, want both", sawInvalidation, sawDelta)
	})
}

// TestCompactLifecycleNativeCompactionAgentScopeCount counts the Agent-tier
// scope constructions during one pressure-triggered submission on the native
// pipeline: the conversation Engine call and the automatic summary Engine
// call each open their own Agent-tier scope under one Operation.
func TestCompactLifecycleNativeCompactionAgentScopeCount(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, compactObservationConfigDocument(e.server.URL))
		writeAgentDefinitions(t, e.configPath, compactObservationAgentsDocument)
		enableFixturePlugin(t, e.configPath, "probe-agent")
		var agOpens atomic.Int64
		e.fixtureStore(store)
		r, err := e.open(context.Background(),
			shortScopeProbe("probe-agent", ScopeAgent, &agOpens))
		testassert.NoError(t, err)
		ctx := context.Background()

		e.server.setScript(func(_ context.Context, body string) []string {
			if !strings.Contains(body, `"model":"k"`) && !strings.Contains(body, `"model": "k"`) {
				if lastUserText(body) == "first question" {
					// The seed report: {5000,10} overflows the window 8192
					// minus the reserve 4096.
					return append(textTurnEvents("seeded answer"),
						`{"usage":{"prompt_tokens":5000,"completion_tokens":10,"total_tokens":5010}}`)
				}
				return nil // conversation turns: the default completed text turn
			}
			return append(textTurnEvents("native summary one"),
				`{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
		})

		session, err := r.createSession(ctx, e.dataDir, "tight")
		testassert.NoError(t, err)
		sessionID := session.Identity.SessionID

		submitThroughRuntime(t, r, sessionID, "op-1", "first question")
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess) // the seed turn commits the overflowing report

		before := agOpens.Load()
		submitThroughRuntime(t, r, sessionID, "op-2", "the overflowing checkpoint")
		op2 := awaitOperation(t, r, sessionID, "op-2", harness.OperationSuccess)
		testassert.Require(t, op2.State.Status == harness.OperationSuccess, "overflowing operation = %+v, want the compacted success", op2.State)
		after := agOpens.Load()
		testassert.Require(t, after-before == 2, "Agent-tier scope openings during the pressure-triggered submission = %d, want the conversation Engine call and the summary Engine call each open one", after-before)

		submitConvergedThroughRuntime(t, r, sessionID, "op-3", "second question", harness.OperationSuccess)
		testassert.NoError(t, r.Close(ctx))
	})
}

// wireRoles returns one wire request's message roles in order.
func wireRoles(doc wireChatBody) []string {
	out := make([]string, 0, len(doc.Messages))
	for _, msg := range doc.Messages {
		out = append(out, msg.Role)
	}
	return out
}

// assertProgressScopedToOperations checks one progress event carries the
// real Session and one of the named Operations.
func assertProgressScopedToOperations(t *testing.T, event Event, sessionID string, operationIDs ...string) error {
	t.Helper()
	scope := progressEventScope(t, event)
	if scopeKind(t, scope) != "operation" {
		return errors.New("progress carries a non-operation scope")
	}
	body, err := scope.AsOperationScope()
	testassert.NoError(t, err)
	if body.SessionId != sessionID {
		return errors.New("progress carries a foreign session identity")
	}
	for _, id := range operationIDs {
		if body.OperationId == id {
			return nil
		}
	}
	return fmt.Errorf("progress carries operation %q outside %v", body.OperationId, operationIDs)
}

// rawAdmissionValues carries the authored execution values of one raw
// message-kind admission seed.
type rawAdmissionValues struct {
	agentType     string
	model         usageWireRef
	outputReserve int
	systemPrompt  string
	tools         string
	compactModel  usageWireRef
	compactPrompt string
}

// rawMessageAdmission builds one message-kind admission payload from the
// caller's authored execution values.
func rawMessageAdmission(sessionID, operationID, entryID string, v rawAdmissionValues) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var defs []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal([]byte(v.tools), &defs)
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	initialNames, _ := json.Marshal(names)
	return fmt.Sprintf(
		`{"session_id":%q,"operation_id":%q,"request_kind":"message",`+
			`"admitted_entry":{"session_id":%q,"entry_id":%q},"agent_selection":{"definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":%q}},`+
			`"execution":{"configuration_revision":"rev-1","model":{"provider":%q,"model":%q},`+
			`"context_window":4096,"output_reserve":%d,`+
			`"system_prompt":%q,"tools":%s,"initial_tool_names":%s,"readonly":false,"write_dir":"",`+
			`"compact":{"model":{"provider":%q,"model":%q},"system_prompt":%q},`+
			`"strategy":{"id":"melta.summary/v1","requires_context_window":true}},"admitted_at":%q}`,
		sessionID, operationID, sessionID, entryID, v.agentType,
		v.model.Provider, v.model.Model, v.outputReserve,
		v.systemPrompt, v.tools, initialNames,
		v.compactModel.Provider, v.compactModel.Model, v.compactPrompt, now)
}

// compactRawMessageAdmission builds the compact and paging fixtures'
// message-kind admission payload.
func compactRawMessageAdmission(sessionID, operationID, entryID string) string {
	return rawMessageAdmission(sessionID, operationID, entryID, rawAdmissionValues{
		agentType:     "solo",
		model:         usageWireRef{Provider: "prov", Model: "m"},
		outputReserve: 1024,
		systemPrompt:  "compact-sys",
		tools:         `[{"name":"noop","description":"tool noop","parameters_base64":"eyJ0eXBlIjoib2JqZWN0In0="}]`,
		compactModel:  usageWireRef{Provider: "prov", Model: "m"},
		compactPrompt: "summarize",
	})
}

func compactRawOperationRegister(admission, state string) string {
	return fmt.Sprintf(`{"admission":%s,"state":%s}`, admission, state)
}

func compactRawRunningModelState(now, reserved, boundary string) string {
	return fmt.Sprintf(
		`{"status":"running","started_at":%q,`+
			`"active_effect":{"kind":"model","result_entry_id":%q,"request_id":"request-live-%s","model":{"provider":"prov","model":"m"},"purpose":"conversation","evidence":{"boundary_entry_id":%q,"prompt":"compact-sys","tool_names":[]}},`+
			`"running_request":{"request_id":"request-live-%s","boundary_entry_id":%q,"hook_results":[]},`+
			`"pending_tool_calls":[],"usage":{"by_model":[]}}`,
		now, reserved, reserved, boundary, reserved, boundary)
}

func compactRawSuccessState(now, settlementEntry, sessionID, usage string) string {
	return fmt.Sprintf(
		`{"status":"success","started_at":%q,"settled_at":%q,"pending_tool_calls":[],"usage":%s,"terminal":{"settlement_entry":{"session_id":%q,"entry_id":%q}}}`,
		now, now, usage, sessionID, settlementEntry)
}

func compactRawInputEntry(sessionID, entryID, operationID, origin, text string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"origin":%q,"content":[{"kind":"text","text":%q}]}`,
		sessionID, entryID, operationID, origin, text)
}

func compactRawAssistantEntry(sessionID, entryID, operationID, boundaryID, text string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"status":"completed",`+
			`"source":{"provider":"prov","model":"m"},"content":[{"kind":"text","text":%q}],"tool_calls":[],"request_id":%q,"evidence":{"boundary_entry_id":%q,"prompt":"compact-sys","tool_names":[]}}`,
		sessionID, entryID, operationID, text, "request-a-"+entryID, boundaryID)
}

func compactRawSettlementEntry(sessionID, entryID, operationID, extra string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"status":"success"%s}`,
		sessionID, entryID, operationID, extra)
}

func compactRawCompactionEntry(sessionID, entryID, operationID, summary, boundaryID string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"strategy_id":"melta.summary/v1",`+
			`"messages":[{"role":"assistant","source":{"provider":"prov","model":"m"},"content":[{"kind":"text","text":%q}]}],`+
			`"boundary_entry_id":%q,"configuration_revision":"rev-1",`+
			`"facts":[{"request_id":%q,"model":{"provider":"prov","model":"m"},"input":[{"role":"user","content":[{"kind":"text","text":"transcript seed"}]}]}],`+
			`"consumed_request_id":%q,"evidence":{"boundary_entry_id":%q,"prompt":"compact-sys","tool_names":[]}}`,
		sessionID, entryID, operationID, "[Previous conversation summary]\n\n"+summary+"\n\n[End of summary. Continue from here.]", boundaryID, "maintainer-"+entryID, "request-"+operationID, boundaryID)
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
	lifecycleInsertEntry(t, store, sessionID, inputID, operationID, harness.EntryInput, compactRawInputEntry(sessionID, inputID, operationID, "user", inputText))
	lifecycleInsertEntry(t, store, sessionID, assistantID, operationID, harness.EntryAssistant, compactRawAssistantEntry(sessionID, assistantID, operationID, inputID, assistantText))
	lifecycleInsertEntry(t, store, sessionID, settlementID, operationID, harness.EntryOperationSettlement, compactRawSettlementEntry(sessionID, settlementID, operationID, ""))
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID},
		compactRawOperationRegister(compactRawMessageAdmission(sessionID, operationID, inputID), compactRawSuccessState(now, settlementID, sessionID, `{"by_model":[]}`)))
	return assistantID
}

// TestCompactLifecycleCapturedCompactionConfiguration proves the R6 row end
// to end over the real Open path: the prepared capture carries the compact
// configuration from one revision, and the prompt and tool-description
// replacement freedoms still hold through the closed contribution
// consequence. The compact members are Core-owned capture fields a
// contribution cannot name at all.
func TestCompactLifecycleCapturedCompactionConfiguration(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		var seenMu sync.Mutex
		var seenPrompt string
		hook := &recordHook{name: "hook.first", events: &traceLog{}, mutate: func(in extension.PreparationInput) extension.PreparationConsequence {
			seenMu.Lock()
			seenPrompt = in.Prompt
			seenMu.Unlock()
			return extension.PreparationConsequence{}
		}}
		// The freed phase runs on its own hook instance, so the first
		// instance's visible contribution is never overwritten.
		freedHook := &recordHook{name: "hook.freed", events: &traceLog{}}
		writeAgentDefinitions(t, e.configPath, productionAgentsWithFreedHook())
		r, err := e.openWithPlugins(ctx, hook, &parkingHook{}, freedHookPlugin(freedHook))
		testassert.NoError(t, err)
		defer r.Close(ctx)
		session, err := r.createSession(ctx, e.workspace("compact-capture"), "worker")
		testassert.NoError(t, err)
		sessionID := session.Identity.SessionID

		// The capture carries the compact configuration from one revision.
		submitThroughRuntime(t, r, sessionID, "op-1", "please write")
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		awaitIdleSession(t, r, sessionID)
		durable := readOperation(t, r, sessionID, "op-1").Admission.Execution
		seenMu.Lock()
		seen := seenPrompt
		seenMu.Unlock()
		testassert.Equal(t, seen, durable.SystemPrompt)
		testassert.Require(t, durable.Compact.Model.Provider != "" && durable.Compact.Model.Model != "" && durable.Compact.SystemPrompt != "", "captured compact configuration incomplete: %+v", durable.Compact)

		// The prompt and tool-description replacement freedoms hold on a
		// fresh session under the second hook instance.
		freedHook.set(nil, func(in extension.PreparationInput) extension.PreparationConsequence {
			prompt := in.Prompt + "|freed"
			descriptions := make(map[string]string, len(in.Catalog))
			for _, tool := range in.Catalog {
				descriptions[tool.Name] = "freed description"
			}
			return extension.PreparationConsequence{Prompt: &prompt, Descriptions: descriptions}
		})
		freedSession, err := r.createSession(ctx, e.workspace("compact-freed"), "freedworker")
		testassert.NoError(t, err)
		freedID := freedSession.Identity.SessionID
		submitThroughRuntime(t, r, freedID, "op-3", "please write")
		awaitOperation(t, r, freedID, "op-3", harness.OperationSuccess)
		rec := readOperation(t, r, freedID, "op-3")
		testassert.Require(t, strings.Contains(rec.Admission.Execution.SystemPrompt, "|freed"), "capture prompt = %q, want the hook's replacement", rec.Admission.Execution.SystemPrompt)
		for _, tool := range rec.Admission.Execution.Tools {
			testassert.Require(t, tool.Description == "freed description", "tool %q description = %q, want the hook's replacement", tool.Name, tool.Description)
		}
	})
}

// freedHookPlugin composes the second hook instance: its own plugin
// identity and capability spec, so only the agent type selecting this spec
// runs the instance's mutation and the first instance's mutation is never
// overwritten by the freed phase.
func freedHookPlugin(hook *recordHook) extension.Native {
	capability := extension.HookCapability("hook.freed", extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure))
	return testNative("freedhooks", extension.LifetimeHarness, []extension.Capability{capability}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"hook.freed": {Preparation: hook.Prepare}}}, nil
	})
}

// productionAgentsWithFreedHook extends the production agents document with
// one agent type selecting the second hook instance's capability spec.
func productionAgentsWithFreedHook() string {
	return strings.TrimSuffix(productionAgentsDocument, "}") + `,
  "freedworker": {"model": "prov/m", "system_prompt": "simple", "tools": ["prod_write", "prod_read"], "capabilities": ["hook.freed"]}}`
}

type observedInvocation struct {
	instanceID   string
	invocationID string
	purpose      string
	operationID  string
	resource     string
}

type observedScopeFixture struct {
	mu                         sync.Mutex
	owner                      *Runtime
	operationOpen              int
	operationClose             int
	operationID                string
	operationInst              string
	operationSawTerminal       bool
	operationSawCheckpoint     bool
	invocations                []observedInvocation
	invocationOpen             int
	invocationClose            int
	conversationOpen           int
	maintenanceOpen            int
	conversationSawCheckpoint  bool
	conversationSawTerminal    bool
	maintenanceSawCheckpoint   bool
	maintenanceSawRunning      bool
	maintenanceSawConversation bool
	dependencyOK               bool
	dependencyCalls            int
	operationTerminal          atomic.Bool
	operationCheckpoint        atomic.Bool
	resources                  map[string]string
	resourceByInstance         map[string]string
	providerResource           string
	resourceSeq                int64
	maintenanceRefusedSibling  bool
	maintenanceRefusedProvider bool
	maintenanceFreedOwn        bool
	maintenanceOwn             string
	maintenanceReleaseErr      string
	conversationUsedResource   bool
	providerResourceAtClose    bool
	parkInvocation             bool
	invocationArrived          chan struct{}
}

func (f *observedScopeFixture) native(store harness.Storage) extension.Native {
	resourceInput := `{"type":"object","required":["resource"],"properties":{"resource":{"type":"string","minLength":1}},"additionalProperties":false}`
	resourceFailure := `{"type":"object","required":["reason"],"properties":{"reason":{"type":"string"}},"additionalProperties":false}`
	opCapability := extension.ServiceCapability("op.scope", "test.op.scope",
		extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""),
		extension.ServiceMethod("allocate", extension.ModeServiceIO, `{}`, `{"type":"object","required":["resource"],"properties":{"resource":{"type":"string","minLength":1}},"additionalProperties":false}`, "", ""),
		extension.ServiceMethod("release", extension.ModeServiceIO, resourceInput, `{}`, resourceFailure, ""),
		extension.ServiceMethod("use", extension.ModeServiceIO, resourceInput, `{}`, resourceFailure, ""),
	)
	dep := extension.Binding{Name: "dep", Provider: "scopes/op-c", CapabilityID: "op.scope", Definition: opCapability.Definition()}
	invCapability := extension.ServiceCapability("inv.scope", "test.inv.scope", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	handler := func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
		return extension.Result{Value: json.RawMessage(`{}`)}, nil
	}
	hasCompaction := func(sessionID string) bool {
		entries, err := store.ReadEntries(context.Background(), sessionID, 0)
		if err != nil {
			return false
		}
		for _, entry := range entries {
			if entry.Kind == harness.EntryCompaction {
				return true
			}
		}
		return false
	}
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           "scopes",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"]}`),
			Components: []extension.Component{
				{ID: "op-c", Lifetime: extension.LifetimeOperation, Provides: []string{"op.scope"}},
				{ID: "inv-c", Lifetime: extension.LifetimeInvocation, Provides: []string{"inv.scope"}, Requires: []extension.Binding{dep}},
			},
			Capabilities: []extension.Capability{opCapability, invCapability},
		},
		Open: func(ctx context.Context, info extension.InstanceContext) (extension.Instance, error) {
			switch info.ComponentID {
			case "op-c":
				f.mu.Lock()
				if f.resources == nil {
					f.resources = map[string]string{}
					f.resourceByInstance = map[string]string{}
				}
				f.operationOpen++
				f.operationID = info.Scope.OperationID
				f.operationInst = info.InstanceID
				f.resourceSeq++
				providerResource := fmt.Sprintf("provider-%d-%s", f.resourceSeq, info.InstanceID)
				f.providerResource = providerResource
				f.resources[providerResource] = info.InstanceID
				f.resourceByInstance[info.InstanceID] = providerResource
				f.mu.Unlock()
				resourceFailure := func(reason string) (extension.Result, error) {
					return extension.Result{Failure: &extension.Failure{Code: "forbidden", Message: reason}}, nil
				}
				ownerOf := func(call extension.CallContext) string { return call.Caller.InstanceID }
				return extension.Instance{Capabilities: map[string]extension.Implementation{"op.scope": {Service: map[string]extension.Handler{
					"call": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
						f.mu.Lock()
						f.dependencyCalls++
						f.mu.Unlock()
						return extension.Result{Value: json.RawMessage(`{}`)}, nil
					},
					"allocate": func(_ context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
						f.mu.Lock()
						f.resourceSeq++
						resource := fmt.Sprintf("resource-%d", f.resourceSeq)
						f.resources[resource] = ownerOf(call)
						f.mu.Unlock()
						value, _ := json.Marshal(struct {
							Resource string `json:"resource"`
						}{Resource: resource})
						return extension.Result{Value: value}, nil
					},
					"release": func(_ context.Context, call extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
						var request struct {
							Resource string `json:"resource"`
						}
						if err := json.Unmarshal(arguments, &request); err != nil {
							return extension.Result{}, err
						}
						f.mu.Lock()
						defer f.mu.Unlock()
						owner, ok := f.resources[request.Resource]
						if !ok || owner != ownerOf(call) {
							return resourceFailure("resource is not owned by the calling instance")
						}
						delete(f.resources, request.Resource)
						return extension.Result{Value: json.RawMessage(`{}`)}, nil
					},
					"use": func(_ context.Context, call extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
						var request struct {
							Resource string `json:"resource"`
						}
						if err := json.Unmarshal(arguments, &request); err != nil {
							return extension.Result{}, err
						}
						f.mu.Lock()
						defer f.mu.Unlock()
						owner, ok := f.resources[request.Resource]
						if !ok || owner != ownerOf(call) {
							return resourceFailure("resource is not owned by the calling instance")
						}
						return extension.Result{Value: json.RawMessage(`{}`)}, nil
					},
				}}}, Close: func(ctx context.Context, calls extension.CallContext) error {
					rec, err := f.owner.harness.ReadOperation(context.Background(), info.Scope.SessionID, info.Scope.OperationID)
					f.mu.Lock()
					resource := f.providerResource
					_, f.providerResourceAtClose = f.resources[resource]
					f.operationClose++
					f.operationSawTerminal = err == nil && rec.State.Status == harness.OperationSuccess
					f.operationSawCheckpoint = err == nil && hasCompaction(info.Scope.SessionID)
					f.operationTerminal.Store(f.operationSawTerminal)
					f.operationCheckpoint.Store(f.operationSawCheckpoint)
					f.mu.Unlock()
					if resource != "" {
						// The provider frees its own resource through its own
						// private store: its Close view has no self-binding.
						f.mu.Lock()
						delete(f.resources, resource)
						f.mu.Unlock()
					}
					return nil
				}}, nil
			case "inv-c":
				if f.parkInvocation && info.Scope.Purpose == string(harness.InvocationPurposeConversation) {
					if f.invocationArrived != nil {
						select {
						case f.invocationArrived <- struct{}{}:
						default:
						}
					}
					<-ctx.Done()
					return extension.Instance{}, ctx.Err()
				}
				f.mu.Lock()
				f.invocationOpen++
				switch info.Scope.Purpose {
				case string(harness.InvocationPurposeConversation):
					f.conversationOpen++
				case string(harness.InvocationPurposeMaintenance):
					f.maintenanceOpen++
				}
				f.invocations = append(f.invocations, observedInvocation{instanceID: info.InstanceID, invocationID: info.Scope.InvocationID, purpose: info.Scope.Purpose, operationID: info.Scope.OperationID})
				f.mu.Unlock()
				allocated, err := info.Call(ctx, "dep", "allocate", json.RawMessage(`{}`))
				if err != nil {
					return extension.Instance{}, err
				}
				var resourceValue struct {
					Resource string `json:"resource"`
				}
				if err := json.Unmarshal(allocated.Value, &resourceValue); err != nil {
					return extension.Instance{}, err
				}
				f.mu.Lock()
				f.resourceByInstance[info.InstanceID] = resourceValue.Resource
				f.invocations[len(f.invocations)-1].resource = resourceValue.Resource
				f.mu.Unlock()
				return extension.Instance{Capabilities: map[string]extension.Implementation{"inv.scope": {Service: map[string]extension.Handler{"call": handler}}}, Close: func(ctx context.Context, calls extension.CallContext) error {
					result, err := calls.Call(ctx, "dep", "call", json.RawMessage(`{}`))
					rec, readErr := f.owner.harness.ReadOperation(context.Background(), info.Scope.SessionID, info.Scope.OperationID)
					f.mu.Lock()
					own := f.resourceByInstance[info.InstanceID]
					conversationResource := ""
					for _, invocation := range f.invocations {
						if invocation.purpose == string(harness.InvocationPurposeConversation) {
							conversationResource = f.resourceByInstance[invocation.instanceID]
						}
					}
					providerResource := f.providerResource
					f.invocationClose++
					f.dependencyOK = err == nil && result.Failure == nil
					if info.Scope.Purpose == string(harness.InvocationPurposeConversation) {
						f.conversationSawCheckpoint = readErr == nil && hasCompaction(info.Scope.SessionID)
						f.conversationSawTerminal = readErr == nil && rec.State.Status == harness.OperationSuccess
					} else {
						f.maintenanceSawCheckpoint = readErr == nil && hasCompaction(info.Scope.SessionID)
						f.maintenanceSawRunning = readErr == nil && rec.State.Status == harness.OperationRunning
						f.maintenanceSawConversation = f.conversationOpen == 1 && f.invocationClose < f.invocationOpen
					}
					f.mu.Unlock()
					if own == "" {
						return fmt.Errorf("invocation %s allocated no resource", info.InstanceID)
					}
					if info.Scope.Purpose == string(harness.InvocationPurposeConversation) {
						// The resumed conversation still owns its resource after
						// maintenance disposal and uses it through the same
						// Operation provider before terminal disposal.
						used, callErr := calls.Call(ctx, "dep", "use", resourceArguments(own))
						f.mu.Lock()
						f.conversationUsedResource = callErr == nil && used.Failure == nil
						f.mu.Unlock()
						if callErr != nil || used.Failure != nil {
							return fmt.Errorf("conversation resource use: %v %+v", callErr, used.Failure)
						}
					} else {
						// Maintenance must not release either its sibling
						// conversation's resource or the provider's own resource;
						// both stay intact while its own resource is freed.
						var sibling extension.Result
						var siblingErr error
						if conversationResource != "" {
							sibling, siblingErr = calls.Call(ctx, "dep", "release", resourceArguments(conversationResource))
						}
						provider, providerErr := calls.Call(ctx, "dep", "release", resourceArguments(providerResource))
						f.mu.Lock()
						f.maintenanceRefusedSibling = conversationResource == "" || (siblingErr == nil && sibling.Failure != nil && f.resources[conversationResource] != "")
						f.maintenanceRefusedProvider = providerErr == nil && provider.Failure != nil && f.resources[providerResource] != ""
						f.mu.Unlock()
						if !f.maintenanceRefusedSibling || !f.maintenanceRefusedProvider {
							return fmt.Errorf("maintenance release refusal: sibling %+v/%v provider %+v/%v", sibling.Failure, siblingErr, provider.Failure, providerErr)
						}
					}
					released, releaseErr := calls.Call(ctx, "dep", "release", resourceArguments(own))
					f.mu.Lock()
					if info.Scope.Purpose != string(harness.InvocationPurposeConversation) {
						f.maintenanceFreedOwn = releaseErr == nil && released.Failure == nil && f.resources[own] == ""
						f.maintenanceOwn = own
						if releaseErr != nil {
							f.maintenanceReleaseErr = releaseErr.Error()
						} else if released.Failure != nil {
							f.maintenanceReleaseErr = released.Failure.Message
						}
					}
					delete(f.resourceByInstance, info.InstanceID)
					f.mu.Unlock()
					if releaseErr != nil || released.Failure != nil {
						return fmt.Errorf("invocation resource release: %v %+v", releaseErr, released.Failure)
					}
					return nil
				}}, nil
			default:
				return extension.Instance{}, fmt.Errorf("unknown component %q", info.ComponentID)
			}
		},
	}
}

func resourceArguments(resource string) json.RawMessage {
	value, _ := json.Marshal(struct {
		Resource string `json:"resource"`
	}{Resource: resource})
	return value
}

func TestCompactLifecycleNativePressureScopesAndResources(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, compactObservationConfigDocument(e.server.URL))
		writeAgentDefinitions(t, e.configPath, compactObservationAgentsDocument)
		enableFixturePlugin(t, e.configPath, "scopes")
		e.fixtureStore(store)
		fixture := &observedScopeFixture{}
		native := fixture.native(store)
		r, err := e.open(context.Background(), native)
		testassert.NoError(t, err)
		fixture.owner = r
		ctx := context.Background()

		e.server.setScript(func(_ context.Context, body string) []string {
			if !strings.Contains(body, `"model":"k"`) && !strings.Contains(body, `"model": "k"`) {
				if lastUserText(body) == "first question" {
					return append(textTurnEvents("seeded answer"), `{"usage":{"prompt_tokens":5000,"completion_tokens":10,"total_tokens":5010}}`)
				}
				return nil
			}
			return append(textTurnEvents("native summary one"), `{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
		})

		session, err := r.createSession(ctx, e.dataDir, "tight")
		testassert.NoError(t, err)
		sessionID := session.Identity.SessionID
		submitThroughRuntime(t, r, sessionID, "op-1", "first question")
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		deadline := time.Now().Add(10 * time.Second)
		for {
			fixture.mu.Lock()
			closed := fixture.operationClose == 1
			fixture.mu.Unlock()
			if closed {
				break
			}
			testassert.Require(t, time.Now().Before(deadline), "op-1 Operation scope never closed")
			time.Sleep(10 * time.Millisecond)
		}
		fixture.mu.Lock()
		fixture.operationOpen, fixture.operationClose = 0, 0
		fixture.invocations = nil
		fixture.invocationOpen, fixture.invocationClose = 0, 0
		fixture.conversationOpen, fixture.maintenanceOpen = 0, 0
		fixture.operationSawTerminal, fixture.operationSawCheckpoint = false, false
		fixture.conversationSawCheckpoint, fixture.conversationSawTerminal = false, false
		fixture.maintenanceSawCheckpoint, fixture.maintenanceSawRunning, fixture.maintenanceSawConversation = false, false, false
		fixture.dependencyOK = false
		fixture.mu.Unlock()

		submitThroughRuntime(t, r, sessionID, "op-2", "the overflowing checkpoint")
		op2 := awaitOperation(t, r, sessionID, "op-2", harness.OperationSuccess)
		testassert.Require(t, op2.State.Status == harness.OperationSuccess, "overflowing operation = %+v", op2.State)
		deadline = time.Now().Add(10 * time.Second)
		for {
			fixture.mu.Lock()
			closed := fixture.operationClose == 1 && fixture.invocationClose == 2
			fixture.mu.Unlock()
			if closed {
				break
			}
			testassert.Require(t, time.Now().Before(deadline), "scoped disposals never completed")
			time.Sleep(10 * time.Millisecond)
		}
		fixture.mu.Lock()
		testassert.Equal(t, fixture.operationOpen, 1)
		testassert.Equal(t, fixture.invocationOpen, 2)
		testassert.Require(t, len(fixture.invocations) == 2, "invocations = %+v, want conversation and maintenance", fixture.invocations)
		testassert.Require(t, fixture.invocations[0].invocationID != fixture.invocations[1].invocationID && fixture.invocations[0].instanceID != fixture.invocations[1].instanceID, "invocations = %+v, want distinct IDs and instances", fixture.invocations)
		testassert.Require(t, fixture.invocations[0].operationID == "op-2" && fixture.invocations[1].operationID == "op-2", "invocation Operation IDs = %+v, want the shared Operation", fixture.invocations)
		testassert.Require(t, fixture.invocations[0].resource != "" && fixture.invocations[1].resource != "" && fixture.invocations[0].resource != fixture.invocations[1].resource, "invocation resources = %+v, want distinct owned resources", fixture.invocations)
		purposes := []string{fixture.invocations[0].purpose, fixture.invocations[1].purpose}
		testassert.Require(t, slices.Contains(purposes, string(harness.InvocationPurposeConversation)) && slices.Contains(purposes, string(harness.InvocationPurposeMaintenance)), "purposes = %v, want conversation and maintenance", purposes)
		testassert.Require(t, fixture.maintenanceSawCheckpoint && fixture.maintenanceSawRunning && fixture.maintenanceSawConversation, "maintenance close = checkpoint %v running %v conversation-open %v, want settlement before disposal", fixture.maintenanceSawCheckpoint, fixture.maintenanceSawRunning, fixture.maintenanceSawConversation)
		testassert.Require(t, fixture.conversationSawCheckpoint && fixture.conversationSawTerminal, "conversation close = checkpoint %v terminal %v, want settlement before disposal", fixture.conversationSawCheckpoint, fixture.conversationSawTerminal)
		testassert.Require(t, fixture.dependencyOK && fixture.dependencyCalls > 0, "invocation cleanup did not use the still-open Operation provider")
		testassert.Require(t, fixture.maintenanceRefusedSibling && fixture.maintenanceRefusedProvider, "maintenance release refusals = sibling %v provider %v, want both foreign resources refused and intact", fixture.maintenanceRefusedSibling, fixture.maintenanceRefusedProvider)
		testassert.Require(t, fixture.maintenanceFreedOwn, "maintenance did not free its own resource %q (release %q, resources %v)", fixture.maintenanceOwn, fixture.maintenanceReleaseErr, fixture.resources)
		testassert.Require(t, fixture.conversationUsedResource, "the resumed conversation did not use its surviving resource through the Operation provider")
		testassert.Require(t, fixture.providerResourceAtClose, "the Operation provider's own resource was not intact at Operation Close")
		testassert.Require(t, len(fixture.resources) == 0, "resources after scoped disposal = %v, want every owned resource freed", fixture.resources)
		fixture.mu.Unlock()
		testassert.NoError(t, r.Close(ctx))
		testassert.Require(t, fixture.operationTerminal.Load() && fixture.operationCheckpoint.Load(), "Operation close = terminal %v checkpoint %v, want committed terminal and checkpoint", fixture.operationTerminal.Load(), fixture.operationCheckpoint.Load())
	})
}

func TestCompactLifecycleManualMaintenanceOpensOnlyMaintenanceInstance(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, compactObservationConfigDocument(e.server.URL))
		writeAgentDefinitions(t, e.configPath, compactObservationAgentsDocument)
		enableFixturePlugin(t, e.configPath, "scopes")
		e.fixtureStore(store)
		fixture := &observedScopeFixture{}
		r, err := e.open(context.Background(), fixture.native(store))
		testassert.NoError(t, err)
		fixture.owner = r
		ctx := context.Background()
		e.server.setScript(func(_ context.Context, body string) []string {
			if strings.Contains(body, `"model":"k"`) || strings.Contains(body, `"model": "k"`) {
				return textTurnEvents("manual summary")
			}
			return nil
		})
		session, err := r.createSession(ctx, e.dataDir, "tight")
		testassert.NoError(t, err)
		sessionID := session.Identity.SessionID
		submitThroughRuntime(t, r, sessionID, "op-seed", "seed")
		awaitOperation(t, r, sessionID, "op-seed", harness.OperationSuccess)
		deadline := time.Now().Add(10 * time.Second)
		for {
			fixture.mu.Lock()
			done := fixture.operationClose == 1
			fixture.mu.Unlock()
			if done {
				break
			}
			testassert.Require(t, time.Now().Before(deadline), "seed Operation never closed")
			time.Sleep(10 * time.Millisecond)
		}
		fixture.mu.Lock()
		fixture.operationOpen, fixture.operationClose = 0, 0
		fixture.invocations = nil
		fixture.invocationOpen, fixture.invocationClose = 0, 0
		fixture.conversationOpen, fixture.maintenanceOpen = 0, 0
		fixture.mu.Unlock()

		_, err = r.compactSession(ctx, sessionID, protocol.CompactRequest{OperationId: "compact-1"})
		testassert.NoError(t, err)
		awaitOperation(t, r, sessionID, "compact-1", harness.OperationSuccess)
		deadline = time.Now().Add(10 * time.Second)
		for {
			fixture.mu.Lock()
			done := fixture.operationClose == 1 && fixture.invocationClose == 1
			fixture.mu.Unlock()
			if done {
				break
			}
			testassert.Require(t, time.Now().Before(deadline), "manual maintenance scopes never closed")
			time.Sleep(10 * time.Millisecond)
		}
		fixture.mu.Lock()
		testassert.Equal(t, fixture.operationOpen, 1)
		testassert.Equal(t, fixture.conversationOpen, 0)
		testassert.Equal(t, fixture.maintenanceOpen, 1)
		testassert.Require(t, len(fixture.invocations) == 1 && fixture.invocations[0].purpose == string(harness.InvocationPurposeMaintenance), "invocations = %+v, want only the maintenance instance", fixture.invocations)
		fixture.mu.Unlock()
		testassert.NoError(t, r.Close(ctx))
	})
}

func TestCompactLifecycleFailedSummaryDisposesMaintenanceInstance(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"summary rejected"}}`))
		}))
		t.Cleanup(bad.Close)
		writeServiceFile(t, e.configPath, `{"providers":{"prov":`+nominalProvMember(e.server.URL, "", `{"m":{"name":"M","context_window":8192,"max_output_tokens":4096}}`)+`,"bad":`+nominalProvMember(bad.URL, "", `{"k":{"name":"K","context_window":32768,"max_output_tokens":4096}}`)+`},"compaction":{"model":"bad/k"}}`)
		writeAgentDefinitions(t, e.configPath, compactObservationAgentsDocument)
		enableFixturePlugin(t, e.configPath, "scopes")
		e.fixtureStore(store)
		fixture := &observedScopeFixture{}
		r, err := e.open(context.Background(), fixture.native(store))
		testassert.NoError(t, err)
		fixture.owner = r
		ctx := context.Background()
		e.server.setScript(func(_ context.Context, body string) []string {
			if lastUserText(body) == "first question" {
				return append(textTurnEvents("seeded answer"), `{"usage":{"prompt_tokens":5000,"completion_tokens":10,"total_tokens":5010}}`)
			}
			return nil
		})
		session, err := r.createSession(ctx, e.dataDir, "tight")
		testassert.NoError(t, err)
		sessionID := session.Identity.SessionID
		submitThroughRuntime(t, r, sessionID, "op-seed", "first question")
		awaitOperation(t, r, sessionID, "op-seed", harness.OperationSuccess)
		deadline := time.Now().Add(10 * time.Second)
		for {
			fixture.mu.Lock()
			done := fixture.operationClose == 1
			fixture.mu.Unlock()
			if done {
				break
			}
			testassert.Require(t, time.Now().Before(deadline), "seed Operation never closed")
			time.Sleep(10 * time.Millisecond)
		}
		fixture.mu.Lock()
		fixture.operationOpen, fixture.operationClose = 0, 0
		fixture.invocations = nil
		fixture.invocationOpen, fixture.invocationClose = 0, 0
		fixture.mu.Unlock()

		submitThroughRuntime(t, r, sessionID, "op-failed", "the overflowing checkpoint")
		awaitOperation(t, r, sessionID, "op-failed", harness.OperationFailure)
		deadline = time.Now().Add(10 * time.Second)
		for {
			fixture.mu.Lock()
			done := fixture.invocationOpen == 2 && fixture.invocationClose == 2 && fixture.operationClose == 1
			fixture.mu.Unlock()
			if done {
				break
			}
			testassert.Require(t, time.Now().Before(deadline), "failed-summary scopes never disposed")
			time.Sleep(10 * time.Millisecond)
		}
		fixture.mu.Lock()
		testassert.Require(t, len(fixture.invocations) == 2, "invocations = %+v, want conversation and failed maintenance", fixture.invocations)
		conversation, maintenance := 0, 0
		for _, invocation := range fixture.invocations {
			switch invocation.purpose {
			case string(harness.InvocationPurposeConversation):
				conversation++
			case string(harness.InvocationPurposeMaintenance):
				maintenance++
			}
		}
		testassert.Require(t, conversation == 1 && maintenance == 1, "invocation purposes = %+v", fixture.invocations)
		testassert.Require(t, !fixture.maintenanceSawCheckpoint, "failed summary left a committed checkpoint")
		fixture.mu.Unlock()
		testassert.NoError(t, r.Close(ctx))
	})
}

func TestCompactLifecycleCanceledInvocationOpeningDisposesOperation(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, compactObservationConfigDocument(e.server.URL))
		writeAgentDefinitions(t, e.configPath, compactObservationAgentsDocument)
		enableFixturePlugin(t, e.configPath, "scopes")
		e.fixtureStore(store)
		fixture := &observedScopeFixture{parkInvocation: true, invocationArrived: make(chan struct{}, 1)}
		r, err := e.open(context.Background(), fixture.native(store))
		testassert.NoError(t, err)
		fixture.owner = r
		ctx := context.Background()
		e.server.setScript(func(context.Context, string) []string { return nil })
		session, err := r.createSession(ctx, e.dataDir, "tight")
		testassert.NoError(t, err)
		sessionID := session.Identity.SessionID
		submitThroughRuntime(t, r, sessionID, "op-canceled", "cancel me")
		select {
		case <-fixture.invocationArrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the invocation opening never parked")
		}
		testassert.NoError(t, r.interruptSession(context.Background(), sessionID))
		awaitOperation(t, r, sessionID, "op-canceled", harness.OperationInterruption)
		deadline := time.Now().Add(10 * time.Second)
		for {
			fixture.mu.Lock()
			done := fixture.operationClose == 1
			fixture.mu.Unlock()
			if done {
				break
			}
			testassert.Require(t, time.Now().Before(deadline), "canceled opening never disposed the Operation")
			time.Sleep(10 * time.Millisecond)
		}
		fixture.mu.Lock()
		testassert.Equal(t, fixture.invocationClose, 0)
		testassert.Require(t, !fixture.operationSawTerminal, "canceled opening retained a success terminal")
		testassert.Require(t, !fixture.operationCheckpoint.Load(), "canceled opening retained a checkpoint")
		fixture.mu.Unlock()
		testassert.NoError(t, r.Close(ctx))
	})
}
