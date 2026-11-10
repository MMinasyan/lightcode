package melta

// Native/worker boundary convergence: the shared fixture from
// extension/worker/testdata/fixture runs natively and through a real worker
// process, and every case compares the semantic observation both transports
// produce.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	workerfixture "github.com/MMinasyan/lightcode/extension/worker/testdata/fixture/native"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// parityHost is one side of the native/process comparison.
type parityHost struct {
	name   string
	worker bool
	binary string
	events string
}

// parityObservation collects the transport-independent semantic values one
// parity run produced. Keys are stable descriptions, values are digests,
// statuses, normalized paths or counts, never transport mechanics.
type parityObservation struct {
	values map[string]string
}

func newParityObservation() parityObservation {
	return parityObservation{values: map[string]string{}}
}

func (o parityObservation) set(key, value string) {
	o.values[key] = value
}

// eachParityHost builds the worker executable once and runs one case through
// the in-process native host and the real worker process. The same code
// implements both transports.
func eachParityHost(t *testing.T, run func(t *testing.T, host parityHost) parityObservation) {
	t.Helper()
	binary := buildExecutableFixture(t)
	hosts := []parityHost{{name: "native"}, {name: "worker", worker: true, binary: binary}}
	observed := make([]parityObservation, len(hosts))
	complete := true
	for index, host := range hosts {
		host := host
		index := index
		if !t.Run(host.name, func(t *testing.T) {
			host.events = filepath.Join(t.TempDir(), "fixture-events.log")
			t.Setenv(workerfixture.EventLogEnv, host.events)
			observed[index] = run(t, host)
		}) {
			complete = false
		}
	}
	if !complete {
		return
	}
	for index := 1; index < len(observed); index++ {
		if !reflect.DeepEqual(observed[0].values, observed[index].values) {
			t.Fatalf("%s semantic observation differs across transports:\nnative: %v\nworker: %v", hosts[index].name, observed[0].values, observed[index].values)
		}
	}
}

// openParity opens the fixture as a supplied native extension or as the
// executable worker serving the same declaration. The supplied Agents
// resolver composes the same registered capability universe either way.
func (e *productionEnv) openParity(ctx context.Context, host parityHost) (*Melta, error) {
	native := workerfixture.Native()
	if host.worker {
		return e.openExecutables(ctx, []executableExtension{{Declaration: native.Declaration, EntryPoint: host.binary}}, native.Declaration)
	}
	return e.openPlugins(ctx, native)
}

// configureFixture writes the fixture extension section and the Agents
// definitions the case needs into the environment's main document.
func configureFixture(t *testing.T, e *productionEnv, section, definitions string) {
	t.Helper()
	configureExtension(t, e, "fixture", section, definitions)
}

// configureExtension writes one extension section and the Agents definitions
// into the environment's main document.
func configureExtension(t *testing.T, e *productionEnv, id, section, definitions string) {
	t.Helper()
	data, err := os.ReadFile(e.configPath)
	testassert.NoError(t, err)
	var root map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(data, &root))
	var extensions map[string]json.RawMessage
	if len(root["extensions"]) != 0 {
		testassert.NoError(t, json.Unmarshal(root["extensions"], &extensions))
	}
	if extensions == nil {
		extensions = map[string]json.RawMessage{}
	}
	extensions[id] = json.RawMessage(section)
	root["extensions"], err = json.Marshal(extensions)
	testassert.NoError(t, err)
	data, err = json.Marshal(root)
	testassert.NoError(t, err)
	writeServiceFile(t, e.configPath, string(data))
	writeAgentDefinitions(t, e.configPath, definitions)
}

// paritySteps scripts one model turn sequence: one tool call per listed step,
// then a final text turn.
type parityStep struct {
	name      string
	arguments string
}

func parityScript(steps []parityStep) func(context.Context, string) []string {
	return func(_ context.Context, body string) []string {
		index := toolResultCount(body)
		if index < len(steps) {
			return toolCallTurnEvents(fmt.Sprintf("parity-call-%d", index), steps[index].name, steps[index].arguments)
		}
		return textTurnEvents("done")
	}
}

func toolResultCount(body string) int {
	var doc struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return 0
	}
	count := 0
	for _, message := range doc.Messages {
		if message.Role == "tool" {
			count++
		}
	}
	return count
}

func hashHex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func readFixtureEvents(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	testassert.NoError(t, err)
	var events []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			events = append(events, line)
		}
	}
	return events
}

func countEvent(events []string, prefix string) int {
	count := 0
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			count++
		}
	}
	return count
}

// fixtureEventsWithPrefix returns the event lines carrying one prefix.
func fixtureEventsWithPrefix(events []string, prefix string) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			out = append(out, event)
		}
	}
	return out
}

func parityPlanInput() harness.PlanInput {
	return harness.PlanInput{
		Trigger:         "manual",
		Messages:        []model.Message{{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "plan input"}}}},
		BoundaryEntryID: "boundary",
		Instructions:    "instructions",
		Tools:           []model.ToolDefinition{{Name: "fixture.tool", Parameters: json.RawMessage(`{}`)}},
		ContextWindow:   4096,
		OutputReserve:   1024,
	}
}

// TestExecutableParityCorpusAndCallbacks proves the same multiline, array,
// large-number and escaped raw bytes cross tool arguments, prepared plan data
// and named-service values on both transports, and that all four hook
// boundaries plus the strategy methods run in the same order with the same
// semantic values.
func TestExecutableParityCorpusAndCallbacks(t *testing.T) {
	corpus := []struct {
		payload string
	}{
		{"{\n  \"a\": 1,\n  \"nested\": {\"b\": [true, null]}\n}"},
		{"[1, 2, 3, \"four\", {\"five\": 5}]"},
		{"{\"big\":9007199254740993,\"precise\":1.0000000000000002}"},
		{"{\"text\":\"line\\nbreak\\ttab <>& \\u2028 \\u00e9 \\\"quoted\\\"\"}"},
	}
	steps := make([]parityStep, len(corpus))
	wantResults := make([]string, len(corpus))
	wantBefore := make([]string, len(corpus))
	wantAfter := make([]string, len(corpus))
	for index, entry := range corpus {
		arguments := `{"mode":"corpus","payload":` + entry.payload + `}`
		wrapped := `{"hooked":` + strconv.Quote(arguments) + `}`
		rawContent := hashHex([]byte(wrapped)) + ":" + hashHex([]byte(arguments))
		steps[index] = parityStep{name: "fixture.tool", arguments: arguments}
		wantResults[index] = "fixture-shaped:" + hashHex([]byte(rawContent)) + ":" + rawContent
		wantBefore[index] = "hook:before_tool:in=" + strconv.Quote(arguments) + "->out=" + strconv.Quote(wrapped)
		wantAfter[index] = "hook:after_tool:in=" + strconv.Quote(rawContent) + "->out=" + strconv.Quote(wantResults[index])
	}

	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		e := newProductionEnv(t, storage.NewMemory())
		configureFixture(t, e, `{"enabled":true}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool","fixture.tool.operation"],"capabilities":["fixture.hooks"]}}`)
		e.server.setScript(parityScript(steps))
		ctx := context.Background()
		r, err := e.openParity(ctx, host)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()

		session, err := r.createSession(ctx, e.workspace("parity-ws"), "fixture_agent")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-corpus", "run")
		awaitOperation(t, r, session.Identity.SessionID, "op-corpus", harness.OperationSuccess)

		observation := newParityObservation()
		if got, want := e.server.requests(), len(steps)+1; got != want {
			t.Fatalf("model requests = %d, want %d", got, want)
		}
		preparePayload := "prov/m:fixture.tool,fixture.tool.operation"
		wantPrompt := "fixture-preparation:" + hashHex([]byte(preparePayload))
		first := decodeWireChatBody(t, e.server.bodyAt(0))
		var systemPrompt string
		{
			err := json.Unmarshal(first.Messages[0].Content, &systemPrompt)
			testassert.NoError(t, err)
		}
		testassert.Require(t, systemPrompt == wantPrompt, "prepared prompt = %q, want %q", systemPrompt, wantPrompt)
		for index := 0; index < e.server.requests(); index++ {
			doc := decodeWireChatBody(t, e.server.bodyAt(index))
			testassert.Require(t, slices.Equal(wireToolNames(doc), []string{"fixture.tool"}), "request %d tools = %v, want the request-hook visibility subset", index, wireToolNames(doc))
		}
		for index, want := range wantResults {
			body := e.server.bodyAt(index + 1)
			testassert.Require(t, strings.Contains(body, want), "tool result %d = %q, want the shaped consequence %s", index, body, want)
			observation.set(fmt.Sprintf("result-%d", index), want)
		}

		snapshot := r.config.current()
		service := snapshot.resources.runtime.capabilities["fixture.service"]
		if service == nil {
			t.Fatal("fixture service is not open")
		}
		for index, entry := range corpus {
			arguments := `{"data":` + entry.payload + `}`
			result, err := snapshot.composition.invokeProvider(ctx, service, service.capability.Methods, "echo", json.RawMessage(arguments), nil, callIdentity{revision: snapshot.resources.revisionString()}, false)
			if err != nil {
				t.Fatalf("service echo %d: %v", index, err)
			}
			testassert.Require(t, string(result.Value) == arguments, "service value %d = %q, want the exact arguments %q", index, result.Value, arguments)
			observation.set(fmt.Sprintf("service-%d", index), hashHex(result.Value))
		}

		binding, err := resolveStrategyBinding(snapshot.composition, []*scope{snapshot.resources.runtime}, harness.StrategySelection{ID: "fixture.strategy"}, context.Background())
		if err != nil {
			t.Fatalf("strategy binding: %v", err)
		}
		planInput := parityPlanInput()
		plan, err := binding.Strategy.Plan(planInput)
		if err != nil || plan.Kind != harness.PlanModel || plan.Model == nil || len(plan.Model.State) == 0 || len(plan.Model.Request.Messages) != 1 {
			t.Fatalf("strategy plan = %+v, %v, want an input-derived model plan", plan, err)
		}
		planText := messageText(plan.Model.Request.Messages[0])
		testassert.Require(t, strings.HasPrefix(planText, "fixture-plan:"), "plan message = %q, want the input-derived plan", planText)
		consumeInput := harness.ConsumeInput{
			Input:  planInput,
			State:  plan.Model.State,
			Output: model.Output{Status: model.OutputErrored, Source: model.ModelRef{Provider: "prov", Model: "m"}, Detail: "error detail"},
		}
		consume, err := binding.Strategy.Consume(consumeInput)
		if err != nil || consume.Kind != harness.PlanNone {
			t.Fatalf("strategy consume = %+v, %v", consume, err)
		}
		observation.set("strategy-plan-kind", string(plan.Kind))
		observation.set("strategy-message", planText)
		observation.set("strategy-state", string(plan.Model.State))
		observation.set("strategy-consume-kind", string(consume.Kind))

		events := readFixtureEvents(t, host.events)
		wantEvents := []string{"hook:prepare:" + preparePayload + "->" + wantPrompt}
		requestLine := "hook:request:in=fixture.tool,fixture.tool.operation->out=fixture.tool"
		for index := range steps {
			wantEvents = append(wantEvents, requestLine, wantBefore[index], wantAfter[index])
		}
		wantEvents = append(wantEvents, requestLine)
		hookEvents := fixtureEventsWithPrefix(events, "hook:")
		testassert.Require(t, slices.Equal(hookEvents, wantEvents), "hook events = %v, want %v", hookEvents, wantEvents)
		observation.set("hooks", strings.Join(hookEvents, "\n"))
		planEvents := fixtureEventsWithPrefix(events, "strategy:plan:")
		consumeEvents := fixtureEventsWithPrefix(events, "strategy:consume:")
		testassert.Require(t, len(planEvents) == 1 && len(consumeEvents) == 1, "strategy events = %v / %v", planEvents, consumeEvents)
		testassert.Require(t, strings.Contains(consumeEvents[0], "state="+string(plan.Model.State)+";output=errored;detail=error detail"), "consume event = %q, want the plan's state bytes", consumeEvents[0])
		observation.set("strategy-plan-event", planEvents[0])
		observation.set("strategy-consume-event", consumeEvents[0])
		return observation
	})
}

// TestExecutableParityPreparedExecution compares the one prepared-execution
// gate on both transports: an exact plan-data round trip, an invalid plan, an
// evaluator denial and a returned result the shared validator rejects.
func TestExecutableParityPreparedExecution(t *testing.T) {
	steps := []parityStep{
		{name: "fixture.tool", arguments: `{"mode":"default"}`},
		{name: "fixture.tool", arguments: `{"mode":"invalid_plan"}`},
		{name: "fixture.tool", arguments: `{"mode":"invalid_result"}`},
	}
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		e := newProductionEnv(t, storage.NewMemory())
		configureFixture(t, e, `{"enabled":true}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool"]}}`)
		addDenyRule(t, e.configPath, harness.PermissionFileRead, filepath.Join(e.workspace("parity-ws"), "fixture.txt"))
		e.server.setScript(parityScript(steps))
		ctx := context.Background()
		r, err := e.openParity(ctx, host)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		session, err := r.createSession(ctx, e.workspace("parity-ws"), "fixture_agent")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-execution", "run")
		awaitOperation(t, r, session.Identity.SessionID, "op-execution", harness.OperationSuccess)

		observation := newParityObservation()
		// The first call is denied by the configured rule; the invalid plan
		// declares no permission pair and is refused by the same gate; the
		// invalid result reached execution but the shared validator replaced it
		// with the contract-fixed unknown outcome.
		for index, want := range []string{harness.PermissionDeniedContent, harness.PermissionDeniedContent, "Tool call failed internal validation."} {
			body := e.server.bodyAt(index + 1)
			testassert.Require(t, strings.Contains(body, want), "tool result %d = %q, want %q", index, body, want)
			if index == 2 {
				testassert.Require(t, strings.Contains(body, "Execution outcome unknown"), "invalid result kept a verified outcome: %q", body)
			}
			observation.set(fmt.Sprintf("result-%d", index), want)
		}
		events := readFixtureEvents(t, host.events)
		testassert.Equal(t, countEvent(events, "tool:execute:fixture.tool:"), 1)
		observation.set("executions", fmt.Sprint(countEvent(events, "tool:execute:fixture.tool:")))
		return observation
	})
}

// TestExecutableParityDescribeBeforeOpen proves pure description is available
// before any execution scope opens on both transports, and admitted work then
// opens exactly one instance at every declared lifetime and closes it again.
func TestExecutableParityDescribeBeforeOpen(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		e := newProductionEnv(t, storage.NewMemory())
		configureFixture(t, e, `{"enabled":true}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool","fixture.tool.workspace","fixture.tool.operation","fixture.tool.invocation"]}}`)
		steps := []parityStep{
			{name: "fixture.tool", arguments: `{}`},
			{name: "fixture.tool.workspace", arguments: `{}`},
			{name: "fixture.tool.operation", arguments: `{}`},
			{name: "fixture.tool.invocation", arguments: `{}`},
		}
		e.server.setScript(parityScript(steps))
		ctx := context.Background()
		r, err := e.openParity(ctx, host)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		closed := false
		defer func() {
			if !closed {
				_ = r.Close(ctx)
			}
		}()

		observation := newParityObservation()
		snapshot := r.config.current()
		for _, id := range []string{"fixture.tool", "fixture.tool.workspace", "fixture.tool.operation", "fixture.tool.invocation"} {
			spec, ok := snapshot.composition.toolSpec(id)
			if !ok {
				t.Fatalf("tool %q is not composed", id)
			}
			description, err := spec.describe(ctx, Invocation{snapshot: snapshot}, model.ModelRef{Provider: "prov", Model: "m"}, harness.ToolConstraints{}, harness.SessionIdentity{SessionID: "describe", Workspace: "/workspace"}, harness.AgentInputs{Model: model.ModelRef{Provider: "prov", Model: "m"}})
			if err != nil {
				t.Fatalf("describe %q before open: %v", id, err)
			}
			observation.set("describe:"+id, hashHex(description.Definition.Parameters))
		}
		before := readFixtureEvents(t, host.events)
		testassert.Equal(t, countEvent(before, "open:harness:fixture.component"), 1)
		for _, lifetime := range []string{"workspace", "operation", "invocation"} {
			testassert.Equal(t, countEvent(before, "open:"+lifetime+":"), 0)
		}

		session, err := r.createSession(ctx, e.workspace("parity-ws"), "fixture_agent")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-lifetimes", "run")
		awaitOperation(t, r, session.Identity.SessionID, "op-lifetimes", harness.OperationSuccess)

		testassert.NoError(t, r.Close(ctx))
		closed = true
		events := readFixtureEvents(t, host.events)
		for _, component := range []struct{ lifetime, id string }{
			{"harness", "fixture.component"},
			{"workspace", "fixture.workspace"},
			{"operation", "fixture.operation"},
			{"invocation", "fixture.invocation"},
		} {
			opens := countEvent(events, "open:"+component.lifetime+":"+component.id)
			closes := countEvent(events, "close:"+component.lifetime+":"+component.id)
			testassert.Equal(t, opens, 1)
			testassert.Equal(t, closes, opens)
			observation.set("opens:"+component.lifetime, fmt.Sprint(opens))
			observation.set("closes:"+component.lifetime, fmt.Sprint(closes))
		}
		return observation
	})
}

// TestExecutableParityMetadataOneMiB proves one 1 MiB metadata outcome keeps
// its exact bytes over the worker frame and matches the native description.
func TestExecutableParityMetadataOneMiB(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		t.Setenv(workerfixture.MetadataPaddingEnv, "1048576")
		e := newProductionEnv(t, storage.NewMemory())
		configureFixture(t, e, `{"enabled":true}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool"]}}`)
		ctx := context.Background()
		r, err := e.openParity(ctx, host)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		snapshot := r.config.current()
		spec, ok := snapshot.composition.toolSpec("fixture.tool")
		if !ok {
			t.Fatal("fixture.tool is not composed")
		}
		description, err := spec.describe(ctx, Invocation{snapshot: snapshot}, model.ModelRef{Provider: "prov", Model: "m"}, harness.ToolConstraints{}, harness.SessionIdentity{SessionID: "describe", Workspace: "/workspace"}, harness.AgentInputs{Model: model.ModelRef{Provider: "prov", Model: "m"}})
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		testassert.Require(t, len(description.Definition.Parameters) >= 1<<20, "metadata = %d bytes, want at least 1 MiB", len(description.Definition.Parameters))
		observation := newParityObservation()
		observation.set("metadata", hashHex(description.Definition.Parameters))
		observation.set("size", fmt.Sprint(len(description.Definition.Parameters)))
		return observation
	})
}

// TestExecutableParitySessionDataAndCleanup proves the session and cache
// namespace calls, the durable external-data claim and the post-deletion
// cleanup callback produce the same owned layout and cleanup evidence on both
// transports.
func TestExecutableParitySessionDataAndCleanup(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		e := newProductionEnv(t, storage.NewMemory())
		configureFixture(t, e, `{"enabled":true}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool"]}}`)
		steps := []parityStep{{name: "fixture.tool", arguments: `{"mode":"data"}`}}
		e.server.setScript(parityScript(steps))
		ctx := context.Background()
		r, err := e.openParity(ctx, host)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		session, err := r.createSession(ctx, e.workspace("parity-ws"), "fixture_agent")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-data", "run")
		awaitOperation(t, r, session.Identity.SessionID, "op-data", harness.OperationSuccess)
		body := e.server.bodyAt(1)
		hash := hashHex([]byte("fixture"))
		sessionDir := filepath.Join(e.dataDir, "code", session.Identity.SessionID, "extensions", hash)
		cacheDir := filepath.Join(e.dataDir, "extensions", hash, "cache")
		testassert.Require(t, strings.Contains(body, "session="+sessionDir) && strings.Contains(body, "cache="+cacheDir) && strings.Contains(body, "claim=claimed"), "data result body = %q, want the owned namespaces and the claim", body)
		testassert.Require(t, fileExists(sessionDir) && fileExists(cacheDir), "namespace directories were not created")

		observation := newParityObservation()
		observation.set("session", strings.Replace(strings.TrimPrefix(sessionDir, e.dataDir), session.Identity.SessionID, "<session>", 1))
		observation.set("cache", strings.TrimPrefix(cacheDir, e.dataDir))
		sentinel := filepath.Join(cacheDir, "cache.sentinel")
		testassert.NoError(t, os.WriteFile(sentinel, []byte("cache"), 0o600))
		{
			_, err := r.ArchiveSession(ctx, testCaller(r), session.Identity.SessionID)
			testassert.NoError(t, err)
		}
		result, err := r.DeleteSession(ctx, testCaller(r), session.Identity.SessionID)
		testassert.NoError(t, err)
		testassert.Equal(t, len(result.CleanupFailures), 0)
		testassert.Require(t, !fileExists(sessionDir), "deleted Session directory still exists")
		testassert.Require(t, fileExists(sentinel), "reusable extension cache was deleted with the Session")
		events := readFixtureEvents(t, host.events)
		cleanupLine := "cleanup:" + session.Identity.SessionID + ";call-session=\"\""
		testassert.Equal(t, countEvent(events, "cleanup:"+session.Identity.SessionID), 1)
		testassert.Require(t, slices.Contains(events, cleanupLine), "cleanup event = %v, want no Session grant on the callback context", fixtureEventsWithPrefix(events, "cleanup:"))
		observation.set("cleanup", fmt.Sprint(countEvent(events, "cleanup:"+session.Identity.SessionID)))
		return observation
	})
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestExecutableParityConcurrentOperationCredentials proves two concurrently
// admitted Operations keep their own captured Operation credential: the first
// Operation was admitted before a section change, the second after it, and
// each worker/native tool call resolves the value captured for its own
// admission. Only digests are compared.
func TestExecutableParityConcurrentOperationCredentials(t *testing.T) {
	const (
		firstName   = "PARITY_OPERATION_TOKEN_A"
		secondName  = "PARITY_OPERATION_TOKEN_B"
		firstValue  = "parity-operation-value-a"
		secondValue = "parity-operation-value-b"
	)
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		t.Setenv(firstName, firstValue)
		t.Setenv(secondName, secondValue)
		e := newProductionEnv(t, storage.NewMemory())
		configureFixture(t, e, `{"enabled":true,"token":"`+firstName+`"}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool"]}}`)
		hold := filepath.Join(t.TempDir(), "credential-hold")
		held := `{"mode":"credentials_hold","hold":` + fmt.Sprintf("%q", hold) + `}`
		script := func(_ context.Context, body string) []string {
			if lastMessageRole(body) == "tool" {
				return textTurnEvents("done")
			}
			switch lastUserText(body) {
			case "held":
				return toolCallTurnEvents("call-held", "fixture.tool", held)
			case "fresh":
				return toolCallTurnEvents("call-fresh", "fixture.tool", `{"mode":"credentials"}`)
			}
			return textTurnEvents("done")
		}
		e.server.setScript(script)
		ctx := context.Background()
		r, err := e.openParity(ctx, host)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		heldSession, err := r.createSession(ctx, e.workspace("parity-held"), "fixture_agent")
		if err != nil {
			t.Fatalf("held session: %v", err)
		}
		freshSession, err := r.createSession(ctx, e.workspace("parity-fresh"), "fixture_agent")
		if err != nil {
			t.Fatalf("fresh session: %v", err)
		}
		submitThroughRuntime(t, r, heldSession.Identity.SessionID, "op-held", "held")
		eventually(t, "the held credential read to start", func() bool {
			return countEvent(readFixtureEvents(t, host.events), "credential:entered:") == 1
		})

		configureFixture(t, e, `{"enabled":true,"token":"`+secondName+`"}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool"]}}`)
		_, err = r.Reload(ctx, testCaller(r))
		testassert.NoError(t, err)
		submitThroughRuntime(t, r, freshSession.Identity.SessionID, "op-fresh", "fresh")
		awaitOperation(t, r, freshSession.Identity.SessionID, "op-fresh", harness.OperationSuccess)
		testassert.NoError(t, os.WriteFile(hold, []byte("release"), 0o600))
		awaitOperation(t, r, heldSession.Identity.SessionID, "op-held", harness.OperationSuccess)

		firstDigest := hashHex([]byte(firstValue))
		secondDigest := hashHex([]byte(secondValue))
		if firstDigest == secondDigest {
			t.Fatal("the two captured credential values did not produce distinct digests")
		}
		heldBody := e.server.bodyAt(3)
		freshBody := e.server.bodyAt(2)
		testassert.Require(t, strings.Contains(heldBody, "credential:"+firstDigest), "held Operation did not observe its own captured credential digest")
		testassert.Require(t, strings.Contains(freshBody, "credential:"+secondDigest), "fresh Operation did not observe its own captured credential digest")
		events := readFixtureEvents(t, host.events)
		testassert.Equal(t, countEvent(events, "credential:result:"), 2)
		observation := newParityObservation()
		observation.set("held-digest", firstDigest)
		observation.set("fresh-digest", secondDigest)
		observation.set("results", fmt.Sprint(countEvent(events, "credential:result:")))
		return observation
	})
}

// TestExecutableParityRetainedCallRefused proves a service that keeps one
// callback's Call and uses it after that callback returned is refused with
// the closed identity on both transports.
func TestExecutableParityRetainedCallRefused(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		e := newProductionEnv(t, storage.NewMemory())
		configureFixture(t, e, `{"enabled":true}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":[]}}`)
		ctx := context.Background()
		r, err := e.openParity(ctx, host)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		snapshot := r.config.current()
		provider := snapshot.resources.runtime.capabilities["fixture.service"]
		if provider == nil {
			t.Fatal("fixture service is not open")
		}
		invoke := func(method string, arguments string) (extension.Result, error) {
			return snapshot.composition.invokeProvider(ctx, provider, provider.capability.Methods, method, json.RawMessage(arguments), nil, callIdentity{revision: snapshot.resources.revisionString()}, false)
		}
		assembled, err := invoke("assemble", `{"identity":"fixture","instructions":"","mode":"none"}`)
		testassert.NoError(t, err)
		testassert.Require(t, assembled.Failure == nil && len(assembled.Value) != 0, "assemble result = %+v, want the prompt hostcall value", assembled)
		{
			_, err := invoke("retain", `{}`)
			testassert.NoError(t, err)
		}
		_, err = invoke("use_retained", `{}`)
		testassert.Require(t, errors.Is(err, extension.ErrClosed), "retained call error = %v, want the closed identity", err)
		observation := newParityObservation()
		observation.set("assemble", "prompt")
		observation.set("retained", "closed")
		return observation
	})
}

// TestExecutableParityChildLaunchThroughAdmission proves the child-launch
// host service works from a prepared plan that the real Session admission
// authorized, on both transports.
func TestExecutableParityChildLaunchThroughAdmission(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		e := newProductionEnv(t, storage.NewMemory())
		configureFixture(t, e, `{"enabled":true}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool"]},"child_agent":{"model":"prov/m","system_prompt":"simple","tools":[]}}`)
		script := func(_ context.Context, body string) []string {
			if lastUserText(body) == "child task" {
				return textTurnEvents("child done")
			}
			if lastMessageRole(body) == "tool" {
				return textTurnEvents("done")
			}
			return toolCallTurnEvents("call-child", "fixture.tool", `{"mode":"child"}`)
		}
		e.server.setScript(script)
		ctx := context.Background()
		r, err := e.openParity(ctx, host)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		session, err := r.createSession(ctx, e.workspace("parity-ws"), "fixture_agent")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-parent", "parent")
		awaitOperation(t, r, session.Identity.SessionID, "op-parent", harness.OperationSuccess)
		body := ""
		childID := ""
		for index := 0; index < e.server.requests(); index++ {
			candidate := e.server.bodyAt(index)
			start := strings.Index(candidate, "child=")
			if start < 0 {
				continue
			}
			rest := candidate[start+len("child="):]
			end := 0
			for end < len(rest) && ((rest[end] >= '0' && rest[end] <= '9') || (rest[end] >= 'a' && rest[end] <= 'f')) {
				end++
			}
			if end != 0 {
				body = candidate
				childID = rest[:end]
				break
			}
		}
		if childID == "" {
			t.Fatalf("no model request carried the child Session result; requests = %d", e.server.requests())
		}
		if !strings.Contains(body, "child="+childID) {
			t.Fatalf("parent follow-up lacks the child Session: %q", body)
		}
		awaitOperation(t, r, childID, "child-op", harness.OperationSuccess)
		events := readFixtureEvents(t, host.events)
		observation := newParityObservation()
		observation.set("child", fmt.Sprint(countEvent(events, "child:")))
		testassert.Equal(t, countEvent(events, "child:"), 1)
		return observation
	})
}

// TestExecutableParityCooperativeCancellation proves an admitted execution
// that observes its callback cancellation settles the same terminal on both
// transports without replaying work.
func TestExecutableParityCooperativeCancellation(t *testing.T) {
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		e := newProductionEnv(t, storage.NewMemory())
		configureFixture(t, e, `{"enabled":true}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool"]}}`)
		e.server.setScript(parityScript([]parityStep{{name: "fixture.tool", arguments: `{"mode":"cooperative"}`}}))
		ctx := context.Background()
		r, err := e.openParity(ctx, host)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		session, err := r.createSession(ctx, e.workspace("parity-ws"), "fixture_agent")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-cancel", "run")
		eventually(t, "the cooperative execution to start", func() bool {
			return countEvent(readFixtureEvents(t, host.events), "tool:cooperative") == 1
		})
		testassert.NoError(t, r.InterruptSession(ctx, testCaller(r), session.Identity.SessionID))
		record := awaitOperation(t, r, session.Identity.SessionID, "op-cancel", harness.OperationInterruption)
		events := readFixtureEvents(t, host.events)
		testassert.Equal(t, countEvent(events, "tool:execute:fixture.tool:"), 1)
		observation := newParityObservation()
		observation.set("terminal", string(record.State.Status))
		observation.set("executions", fmt.Sprint(countEvent(events, "tool:execute:fixture.tool:")))
		return observation
	})
}

// TestExecutableParityPureCallbacksCannotDispatchServiceIO proves a fixture
// Normalize or Prepare that attempts a service-I/O hostcall is refused on both
// transports, with the fixed refusal reaching the model.
func TestExecutableParityPureCallbacksCannotDispatchServiceIO(t *testing.T) {
	steps := []parityStep{
		{name: "fixture.tool", arguments: `{"mode":"normalize_io"}`},
		{name: "fixture.tool", arguments: `{"mode":"prepare_io"}`},
	}
	eachParityHost(t, func(t *testing.T, host parityHost) parityObservation {
		e := newProductionEnv(t, storage.NewMemory())
		configureFixture(t, e, `{"enabled":true}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":["fixture.tool"]}}`)
		e.server.setScript(parityScript(steps))
		ctx := context.Background()
		r, err := e.openParity(ctx, host)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { testassert.NoError(t, r.Close(ctx)) }()
		session, err := r.createSession(ctx, e.workspace("parity-ws"), "fixture_agent")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		submitThroughRuntime(t, r, session.Identity.SessionID, "op-pure-io", "run")
		awaitOperation(t, r, session.Identity.SessionID, "op-pure-io", harness.OperationSuccess)
		for index := range steps {
			body := e.server.bodyAt(index + 1)
			testassert.Require(t, strings.Contains(body, "fixture: service I/O refused"), "tool result %d = %q, want the fixed service-I/O refusal", index, body)
		}
		events := readFixtureEvents(t, host.events)
		testassert.Equal(t, countEvent(events, "tool:normalize:io-refused:fixture.tool"), 1)
		testassert.Equal(t, countEvent(events, "tool:prepare:io-refused:fixture.tool"), 1)
		testassert.Equal(t, countEvent(events, "io-granted"), 0)
		observation := newParityObservation()
		observation.set("normalize-io", fmt.Sprint(countEvent(events, "tool:normalize:io-refused:fixture.tool")))
		observation.set("prepare-io", fmt.Sprint(countEvent(events, "tool:prepare:io-refused:fixture.tool")))
		return observation
	})
}

// TestExecutableParityWorkerEnvironment proves the real worker process
// inherits neither a declared nor a provider secret name while an ordinary
// marker survives; the fixture reports names only.
func TestExecutableParityWorkerEnvironment(t *testing.T) {
	const (
		declaredName  = "PARITY_ENV_DECLARED_SECRET"
		declaredValue = "parity-declared-value-never-logged"
		markerName    = "PARITY_ENV_ORDINARY_MARKER"
		markerValue   = "parity-marker-value"
		providerName  = "PRODUCTION_TEST_KEY"
		providerValue = "production-secret-1"
	)
	t.Setenv(declaredName, declaredValue)
	t.Setenv(markerName, markerValue)
	binary := buildExecutableFixture(t)
	e := newProductionEnv(t, storage.NewMemory())
	configureExtension(t, e, "fixture", `{"enabled":true,"token":"`+declaredName+`"}`, `{"fixture_agent":{"model":"prov/m","system_prompt":"simple","tools":[]}}`)
	eventPath := filepath.Join(t.TempDir(), "worker-env-events.log")
	t.Setenv(workerfixture.EventLogEnv, eventPath)
	ctx := context.Background()
	r, err := e.openParity(ctx, parityHost{name: "worker", worker: true, binary: binary, events: eventPath})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { testassert.NoError(t, r.Close(ctx)) }()
	eventually(t, "the worker environment report", func() bool {
		return countEvent(readFixtureEvents(t, eventPath), "env:"+markerName) == 1
	})
	data, err := os.ReadFile(eventPath)
	testassert.NoError(t, err)
	events := readFixtureEvents(t, eventPath)
	for _, name := range []string{declaredName, providerName} {
		testassert.Require(t, !slices.Contains(events, "env:"+name), "secret name %q reached the worker environment", name)
	}
	testassert.Require(t, slices.Contains(events, "env:"+markerName), "ordinary marker %q is missing from the worker environment", markerName)
	testassert.Require(t, !strings.Contains(string(data), declaredValue) && !strings.Contains(string(data), providerValue), "a secret value was written to the event log")
}
