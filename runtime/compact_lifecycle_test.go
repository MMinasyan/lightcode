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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// Native compaction rows over the real preparation: the compaction observer
// oracle through configured conversation and compact endpoints, the captured
// compaction configuration through real hooks, the raw durable-state builders
// the paging and recovery rows seed with, and the exported-surface pin.

// compactObservationConfigDocument configures the conversation model with a
// window whose trigger threshold the seed turn's reported usage overflows,
// and the compact model as a distinct catalog identity, so the wire
// distinguishes the transports by model ID.
func compactObservationConfigDocument(endpoint string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "", false,
		`{"m":{"name":"M","context_window":8192,"max_output_tokens":4096},`+
			`"k":{"name":"K","context_window":32768,"max_output_tokens":4096,"usage_in_stream":true}}`) + `}}`
}

// compactObservationAgentsDocument selects the conversation model for the
// working type and overlays the compact builtin onto the distinct compact
// model, so the two transports are distinguishable on the wire.
const compactObservationAgentsDocument = `{
	"tight": {"model": "prov/m", "system_prompt": "simple"},
	"compact": {"model": "prov/k"}
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
		writeServiceFile(t, agents.PathForConfig(e.configPath), compactObservationAgentsDocument)
		r, err := e.open(context.Background(), e.storagePlugin(store))
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
		testassert.Require(t, op2.Admission.Execution.Compact.Model == (model.ModelRef{Provider: "prov", Model: "k"}), "captured compact model = %+v, want the overlaid prov/k", op2.Admission.Execution.Compact.Model)
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
				Summary     string `json:"summary"`
			}
			testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
			testassert.Require(t, wire.OperationID == "op-2" && wire.Summary == "native summary one", "compaction entry = %+v, want op-2's committed summary", wire)
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
	return fmt.Sprintf(
		`{"session_id":%q,"operation_id":%q,"request_kind":"message",`+
			`"admitted_entry":{"session_id":%q,"entry_id":%q},"agent_type":%q,`+
			`"execution":{"configuration_revision":"rev-1","model":{"provider":%q,"model":%q},`+
			`"context_window":4096,"output_reserve":%d,`+
			`"system_prompt":%q,"tools":%s,"readonly":false,"write_dir":"",`+
			`"compact":{"model":{"provider":%q,"model":%q},"system_prompt":%q}},"admitted_at":%q}`,
		sessionID, operationID, sessionID, entryID, v.agentType,
		v.model.Provider, v.model.Model, v.outputReserve,
		v.systemPrompt, v.tools,
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
		tools:         `[{"name":"noop","description":"tool noop","parameters":{"type":"object"}}]`,
		compactModel:  usageWireRef{Provider: "prov", Model: "m"},
		compactPrompt: "summarize",
	})
}

func compactRawOperationRegister(admission, state string) string {
	return fmt.Sprintf(`{"admission":%s,"state":%s}`, admission, state)
}

func compactRawRunningModelState(now, reserved string) string {
	return fmt.Sprintf(
		`{"status":"running","started_at":%q,"active_effect":{"kind":"model","result_entry_id":%q},"pending_tool_calls":[],"usage":{"by_model":[]}}`,
		now, reserved)
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

func compactRawAssistantEntry(sessionID, entryID, operationID, text string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"status":"completed",`+
			`"source":{"provider":"prov","model":"m"},"content":[{"kind":"text","text":%q}],"tool_calls":[]}`,
		sessionID, entryID, operationID, text)
}

func compactRawSettlementEntry(sessionID, entryID, operationID, extra string) string {
	return fmt.Sprintf(
		`{"session_id":%q,"entry_id":%q,"operation_id":%q,"status":"success"%s}`,
		sessionID, entryID, operationID, extra)
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
	lifecycleInsertEntry(t, store, sessionID, inputID, operationID, harness.EntryInput, compactRawInputEntry(sessionID, inputID, operationID, "user", inputText))
	lifecycleInsertEntry(t, store, sessionID, assistantID, operationID, harness.EntryAssistant, compactRawAssistantEntry(sessionID, assistantID, operationID, assistantText))
	lifecycleInsertEntry(t, store, sessionID, settlementID, operationID, harness.EntryOperationSettlement, compactRawSettlementEntry(sessionID, settlementID, operationID, ""))
	lifecycleInsertRegister(t, store, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: operationID},
		compactRawOperationRegister(compactRawMessageAdmission(sessionID, operationID, inputID), compactRawSuccessState(now, settlementID, sessionID, `{"by_model":[]}`)))
	return assistantID
}

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
		seen := seenCompact
		revision := seenRevision
		seenMu.Unlock()
		testassert.Equal(t, seen, durable.Compact)
		testassert.Equal(t, revision, durable.ConfigurationRevision)
		testassert.Require(t, seen.Model.Provider != "" && seen.Model.Model != "" && seen.SystemPrompt != "", "captured compact configuration incomplete: %+v", seen)

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
		rejected, err := submitRawThroughRuntime(ctx, r, sessionID, "op-2", "please write", harness.MessageModeRegular)
		testassert.Require(t, err != nil || rejected.Disposition != harness.DispositionAdmitted, "a hook changing the compact system prompt was accepted (disposition %q), want rejection", rejected.Disposition)
		{
			_, rerr := e.store.ReadRegister(ctx, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation, OperationID: "op-2"})
			testassert.Require(t, errors.Is(rerr, harness.ErrNotFound), "the rejected admission left a register (%v), want none", rerr)
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
// exported declaration set against the fixed current set — top-level
// exported declarations under their bare names and exported methods on
// exported receiver types under receiver-qualified names, with pointer and
// value receivers distinguished in the key the same way the exported check
// distinguishes them — so an added (*Runtime) method cannot hide behind the
// top-level-only scan and same-named methods on different receivers cannot
// collide: the mounted server transport exposes only the Runtime's
// OpenProtocol and the ProtocolServer's endpoint and discovery publication,
// while the generated handler methods live on the private transport receiver
// and are not public surface. A source scan of the package's production
// files must find exactly the pinned names and no others.
func TestCompactLifecycleRuntimeSurfaceUnchanged(t *testing.T) {
	want := map[string]bool{
		"(*ProtocolServer).Endpoint":         true,
		"(*ProtocolServer).PublishDiscovery": true,
		"(*Runtime).Close":                   true,
		"(*Runtime).OpenProtocol":            true,
		"(*Runtime).Reload":                  true,
		"(*Runtime).Subscribe":               true,
		"(*Subscription).Close":              true,
		"(*Subscription).Events":             true,
		"(Invocation).AgentTypes":            true,
		"(Invocation).Config":                true,
		"(Invocation).Revision":              true,
		"Adaptation":                         true,
		"BackgroundServices":                 true,
		"Bind":                               true,
		"Bindings":                           true,
		"CapabilitySpec":                     true,
		"ErrClosed":                          true,
		"ErrComposition":                     true,
		"ErrConfiguration":                   true,
		"ErrOwned":                           true,
		"Event":                              true,
		"Instance":                           true,
		"Invocation":                         true,
		"ModelAdaptation":                    true,
		"Open":                               true,
		"Options":                            true,
		"Plugin":                             true,
		"PreparationHook":                    true,
		"ProtocolServer":                     true,
		"Runtime":                            true,
		"ScopeAgent":                         true,
		"ScopeInfo":                          true,
		"ScopeKind":                          true,
		"ScopeOperation":                     true,
		"ScopeRuntime":                       true,
		"ScopeWorkspace":                     true,
		"Spec":                               true,
		"Subscription":                       true,
		"Tool":                               true,
		"ToolArgumentsHook":                  true,
		"ToolConstraints":                    true,
		"ToolContext":                        true,
		"ToolDescription":                    true,
		"ToolSpec":                           true,
	}
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	testassert.NoError(t, err)
	got := map[string]string{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		testassert.Require(t, err == nil, "parse %s: %v", file, err)
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
	testassert.Require(t, len(files) != 0, "the package source scan found no production files")
	for name := range got {
		if !want[name] {
			t.Fatalf("exported declaration %q (%s) is outside the fixed pre-compaction surface", name, got[name])
		}
	}
	for name := range want {
		{
			_, ok := got[name]
			testassert.Require(t, ok, "pinned exported declaration %q missing from the package source scan", name)
		}
	}
}
