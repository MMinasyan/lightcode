package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// steeringCaptureConfig is one real authored configuration with two ordinary
// catalog models, so a Reload can move a successor onto the other one.
func steeringCaptureConfig(endpoint string) string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "` + endpoint + `", "api_key_env": ""},
      "discovery": false,
      "models": {
        "m1": {"name": "M1", "context_window": 262144, "max_output_tokens": 4096},
        "m2": {"name": "M2", "context_window": 262144, "max_output_tokens": 4096}
      }
    }
  },
  "plugins": {"hooks": {"tag": "T1"}
  }
}`
}

// steeringCapturePolicyConfig is steeringCaptureConfig plus one real global
// file.write deny rule: the successor's revision changes the captured
// PermissionPolicy, not a capability short-circuit.
func steeringCapturePolicyConfig(endpoint string) string {
	return strings.TrimSuffix(steeringCaptureConfig(endpoint), "}") + `,"permissions":{"rules":[{"permission":"file.write","target":"*","access":"deny"}]}}`
}

// The two authored agent documents differ in model, advertised tools and
// selected hooks; both stay non-readonly so the permission capture is the
// authored policy, never a capability gate.
const (
	steeringCaptureAgentsRev1 = `{
  "capture": {"model": "prov/m1", "system_prompt": "simple", "tools": ["echo", "read"], "capabilities": ["hook.first", "arg.runtime"]}
}`
	steeringCaptureAgentsRev2 = `{
  "capture": {"model": "prov/m2", "system_prompt": "simple", "tools": ["echo"], "capabilities": ["hook.second", "arg.agent"]}
}`
)

// TestSteeringSuccessorPreparesCurrentCaptureNatively proves the current-capture
// rule over the real preparation path: while the predecessor is held, the
// authored config/agents change model, advertised tools, the permission policy
// (a real global file.write deny rule) and selected hooks, and a Reload
// publishes the complete new revision. After the release the predecessor still
// uses its original surface, policy, model and hooks — its second held request
// and its tool behavior prove it — while the successor's committed capture and
// its actual wire/tool behavior use the new revision. Both captures stay
// non-readonly: the observed divergence is the captured PermissionPolicy. The
// real Operation/Agent factory Close events precede the successor's Open
// events, proving disposal before successor construction.
func TestSteeringSuccessorPreparesCurrentCaptureNatively(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		e := newPrepEnv(t, store)
		writeServiceFile(t, e.configPath, steeringCaptureConfig(e.server.URL))
		writeServiceFile(t, agents.PathForConfig(e.configPath), steeringCaptureAgentsRev1)
		if revision, err := e.r.Reload(ctx); err != nil || revision != "2" {
			t.Fatalf("Reload = (%q, %v), want 2", revision, err)
		}
		session := e.session("capture")

		var (
			mu       sync.Mutex
			requests int
		)
		arrived1 := make(chan struct{})
		release1 := make(chan struct{})
		arrived2 := make(chan struct{})
		release2 := make(chan struct{})
		e.server.setScript(func(_ context.Context, _ string) []string {
			mu.Lock()
			requests++
			n := requests
			mu.Unlock()
			switch n {
			case 1: // the predecessor's first held turn: one write under its original policy
				close(arrived1)
				<-release1
				return toolCallTurnEvents("call-1", "echo", `{"x":1}`)
			case 2: // the predecessor's second held request, sent after the reload
				close(arrived2)
				<-release2
				return textTurnEvents("first done")
			case 3: // the successor calls the tool the new revision no longer advertises
				return toolCallTurnEvents("call-2", "read", "{}")
			case 4: // the successor writes under the new revision's deny policy
				return toolCallTurnEvents("call-3", "echo", `{"x":1}`)
			default:
				return textTurnEvents("second done")
			}
		})
		res, err := e.submit(ctx, session, "op-1", "first", harness.MessageModeRegular)
		if err != nil || res.Disposition != harness.DispositionAdmitted {
			t.Fatalf("first submit = %+v err %v, want admitted", res, err)
		}
		receiveEvent := func(from <-chan struct{}, what string) {
			t.Helper()
			select {
			case <-from:
			case <-time.After(20 * time.Second):
				t.Fatalf("%s never happened", what)
			}
		}
		receiveEvent(arrived1, "the predecessor's first request") // held before the reload

		writeServiceFile(t, e.configPath, steeringCapturePolicyConfig(e.server.URL))
		writeServiceFile(t, agents.PathForConfig(e.configPath), steeringCaptureAgentsRev2)
		if revision, err := e.r.Reload(ctx); err != nil || revision != "3" {
			t.Fatalf("Reload = (%q, %v), want 3", revision, err)
		}
		close(release1)
		receiveEvent(arrived2, "the predecessor's post-reload request") // its continuation is held again
		steered, err := e.submit(ctx, session, "op-steer", "steer-1", harness.MessageModeRegular)
		if err != nil || steered.Disposition != harness.DispositionSteering || steered.Operation != nil {
			t.Fatalf("steering submit = %+v err %v, want buffered steering", steered, err)
		}
		close(release2)

		e.awaitTerminal(session, "op-1", harness.OperationSuccess)
		e.awaitTerminal(session, "op-steer", harness.OperationSuccess)

		// the predecessor's post-reload request still carries its original
		// model, tools and selected hook
		continuation := decodeWireChatBody(t, e.server.bodyAt(1))
		if continuation.Model != "m1" {
			t.Fatalf("predecessor continuation model = %q, want the original m1", continuation.Model)
		}
		var predecessorTools []string
		for _, tool := range continuation.Tools {
			predecessorTools = append(predecessorTools, tool.Function.Name)
		}
		if !slices.Contains(predecessorTools, "read") {
			t.Fatalf("predecessor continuation tools = %v, want the original advertised set including read", predecessorTools)
		}
		if !strings.Contains(string(continuation.Messages[0].Content), "|first") ||
			strings.Contains(string(continuation.Messages[0].Content), "|second") {
			t.Fatalf("predecessor system message = %s, want only the original selected hook's suffix", continuation.Messages[0].Content)
		}

		// the successor's wire surface is the complete new revision
		successor := decodeWireChatBody(t, e.server.bodyAt(2))
		if successor.Model != "m2" {
			t.Fatalf("successor model = %q, want the new m2", successor.Model)
		}
		var successorTools []string
		for _, tool := range successor.Tools {
			successorTools = append(successorTools, tool.Function.Name)
		}
		if slices.Contains(successorTools, "read") || !slices.Contains(successorTools, "echo") {
			t.Fatalf("successor tools = %v, want exactly the new advertised set [echo]", successorTools)
		}
		if !strings.Contains(string(successor.Messages[0].Content), "|second") ||
			strings.Contains(string(successor.Messages[0].Content), "|first") {
			t.Fatalf("successor system message = %s, want only the new selected hook's suffix", successor.Messages[0].Content)
		}

		// actual tool behavior under each surface and policy
		results := chainToolResults(t, e.store, session)
		if got := results["call-1"]; got.Status != model.ResultSuccess {
			t.Fatalf("predecessor write = %+v, want success under its original policy", got)
		}
		if got := results["call-2"]; got.Status != model.ResultError || !strings.Contains(got.Content, "not available") {
			t.Fatalf("successor call to the unadvertised tool = %+v, want the unavailable-tool result", got)
		}
		if got := results["call-3"]; got.Status != model.ResultDenied {
			t.Fatalf("successor write = %+v, want the policy denial under the new revision's deny rule", got)
		}

		// selected hooks and argument hooks ran for their own admissions only
		if calls := e.hooks[0].calls(); len(calls) != 1 || calls[0].revision != "2" {
			t.Fatalf("hook.first calls = %+v, want exactly the predecessor's revision-2 preparation", calls)
		}
		if calls := e.hooks[1].calls(); len(calls) != 1 || calls[0].revision != "3" {
			t.Fatalf("hook.second calls = %+v, want exactly the successor's revision-3 preparation", calls)
		}
		if seen, _ := e.argHooks[0].received(); len(seen) != 1 || seen[0] != `{"x":1}` {
			t.Fatalf("predecessor argument-hook receives = %v, want its one original call", seen)
		}
		if seen, _ := e.argHooks[3].received(); len(seen) != 1 || seen[0] != `{"x":1}` {
			t.Fatalf("successor argument-hook receives = %v, want its one original call", seen)
		}
		_, _, echoInputs := e.echo.counters()
		if len(echoInputs) != 2 || echoInputs[0] != `{"repaired":"arg.runtime"}` || echoInputs[1] != `{"repaired":"arg.agent"}` {
			t.Fatalf("echo tool inputs = %v, want each admission's own argument-hook rewrite", echoInputs)
		}

		// committed captures: the predecessor's immutable, the successor's the
		// complete new revision; both stay non-readonly — the divergence is
		// the captured PermissionPolicy, not a capability gate
		first := readOperation(t, e.r, session, "op-1")
		if first.Admission.Execution.ConfigurationRevision != "2" || first.Admission.Execution.Model != (model.ModelRef{Provider: "prov", Model: "m1"}) ||
			first.Admission.Execution.Readonly || !strings.HasSuffix(first.Admission.Execution.SystemPrompt, "|first") {
			t.Fatalf("predecessor capture = %+v, want its immutable revision-2 surface with readonly false", first.Admission.Execution)
		}
		successorRec := readOperation(t, e.r, session, "op-steer")
		if successorRec.Admission.Execution.ConfigurationRevision != "3" || successorRec.Admission.Execution.Model != (model.ModelRef{Provider: "prov", Model: "m2"}) ||
			successorRec.Admission.Execution.Readonly || len(successorRec.Admission.Execution.Tools) != 1 ||
			successorRec.Admission.Execution.Tools[0].Name != "echo" || !strings.HasSuffix(successorRec.Admission.Execution.SystemPrompt, "|second") {
			t.Fatalf("successor capture = %+v, want the complete new revision-3 surface with readonly false", successorRec.Admission.Execution)
		}
		if successorRec.Admission.Execution.Readonly || first.Admission.Execution.Readonly {
			t.Fatalf("captures flipped readonly: predecessor=%v successor=%v, want both false with the policy carried by the deny rule",
				first.Admission.Execution.Readonly, successorRec.Admission.Execution.Readonly)
		}

		// real Operation/Agent factory disposal precedes successor construction
		events := e.events.all()
		firstClose := slices.Index(events, "op-close")
		successorOpen := slices.Index(events, "op-open:op-steer")
		firstAgClose := slices.Index(events, "ag-close")
		if firstClose < 0 || successorOpen < 0 || firstClose > successorOpen {
			t.Fatalf("factory events = %v, want the predecessor's operation close before the successor's open", events)
		}
		if firstAgClose < 0 || firstAgClose > successorOpen {
			t.Fatalf("factory events = %v, want the predecessor's agent close before the successor's construction", events)
		}
	})
}

// TestSteeringHandoffManagedShutdown proves the selected-handoff shutdown row
// over the real preparation path: the handoff settles the predecessor
// quietly, the successor's preparation parks inside the real preparation
// hooks, Runtime.Close begins, and the closed-admission/cancellation policy
// makes the parked preparation fail so no successor is admitted — the
// shutdown joins the scopes and the predecessor's success stays intact.
func TestSteeringHandoffManagedShutdown(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		e := newPrepEnv(t, store)
		session := e.session("dual") // selects hook.first, so the real preparation runs the parked hook

		e.server.setScript(func(_ context.Context, _ string) []string {
			return textTurnEvents("first done")
		})
		gate := make(chan struct{})
		e.server.setHold(gate)
		res, err := e.submit(ctx, session, "op-1", "first", harness.MessageModeRegular)
		if err != nil || res.Disposition != harness.DispositionAdmitted {
			t.Fatalf("first submit = %+v err %v, want admitted", res, err)
		}
		<-e.server.arrived
		steered, err := e.submit(ctx, session, "op-steer", "steer-1", harness.MessageModeRegular)
		if err != nil || steered.Disposition != harness.DispositionSteering || steered.Operation != nil {
			t.Fatalf("steering submit = %+v err %v, want buffered steering", steered, err)
		}

		started := make(chan struct{}, 1)
		e.hooks[0].blockUntilCanceled(started) // the successor's preparation parks in the real hook
		close(gate)

		e.awaitTerminal(session, "op-1", harness.OperationSuccess) // the handoff's silent success
		awaitHookArrival(t, started, "the successor's parked preparation")

		closeErr := make(chan error, 1)
		go func() { closeErr <- e.r.Close(context.Background()) }()
		select {
		case err := <-closeErr:
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("Runtime.Close never joined the parked handoff")
		}

		// no successor and no delivered input; the predecessor's success is
		// intact and its scopes closed before the shutdown returned
		if _, err := e.store.ReadRegister(ctx, harness.RegisterKey{SessionID: session, Kind: harness.RegisterOperation, OperationID: "op-steer"}); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("op-steer register = %v, want no successor admitted", err)
		}
		if got := e.inputEntry(session, "op-1"); got == "" {
			t.Fatalf("predecessor admitted input missing after the joined shutdown")
		}
		entries, err := e.store.ReadEntries(ctx, session, 0)
		if err != nil {
			t.Fatalf("read entries: %v", err)
		}
		for _, entry := range entries {
			if strings.Contains(string(entry.Payload), "steer-1") {
				t.Fatalf("the dropped head committed input entry %s", entry.ID)
			}
		}
		status, detail := operationRegisterTerminal(t, e.store, session, "op-1")
		if status != harness.OperationSuccess || detail != "" {
			t.Fatalf("predecessor = (%q, %q), want the silent success intact", status, detail)
		}
		events := e.events.all()
		if slices.Index(events, "op-close") < 0 || slices.Index(events, "ag-close") < 0 {
			t.Fatalf("factory events = %v, want the predecessor's operation and agent scopes closed by the joined shutdown", events)
		}
		if res, err := e.submit(ctx, session, "op-late", "late", harness.MessageModeRegular); err == nil && res.Disposition == harness.DispositionAdmitted {
			t.Fatalf("post-close submit = %+v, want the closed-admission policy to reject it", res)
		}
	})
}

// awaitHookArrival waits for one prepared-hook arrival, failing the test
// instead of hanging when the parked preparation never starts.
func awaitHookArrival(t *testing.T, from <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-from:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// TestSteeringCleanHandoffOverMountedHTTP proves the mounted projection over
// the generated HTTP client for a clean handoff — no Interrupt manufactures
// the split: the caller-ID successor, the predecessor's silent success,
// distinct input ownership and end items, coherent pending-to-adopted reads,
// and unchanged wire kinds.
func TestSteeringCleanHandoffOverMountedHTTP(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)

		var (
			requestMu sync.Mutex
			requests  int
		)
		e.server.setScript(func(_ context.Context, _ string) []string {
			requestMu.Lock()
			requests++
			n := requests
			requestMu.Unlock()
			usage := func(prompt, completion int) []string {
				return []string{
					`{"choices":[{"delta":{"role":"assistant","content":"done"},"finish_reason":null}]}`,
					`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
					fmt.Sprintf(`{"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`, prompt, completion, prompt+completion),
				}
			}
			if n == 1 {
				return usage(5, 3) // the predecessor generation
			}
			return usage(7, 4) // the successor generation
		})
		gate := make(chan struct{})
		e.server.setHold(gate)
		release := sync.OnceFunc(func() { close(gate) })
		defer release()

		created, err := client.CreateSessionWithResponse(ctx, protocol.CreateSessionRequest{Workspace: filepath.Join(e.home, "clean-handoff"), AgentType: "solo"})
		if err != nil || created.JSON201 == nil {
			t.Fatalf("create = %v status %d: %s", err, created.HTTPResponse.StatusCode, created.Body)
		}
		session := created.JSON201.SessionId

		first, err := client.SubmitSessionWithResponse(ctx, session, protocol.SubmitRequest{
			OperationId: "op-1", Mode: protocol.SubmitRequestModeRegular,
			Content: []protocol.ContentPart{textContentPart(t, "first", nil)},
		})
		if err != nil || first.JSON200 == nil || first.JSON200.Disposition != protocol.SubmitResultDispositionAdmitted {
			t.Fatalf("first submit = %v %+v, want admitted", err, first.JSON200)
		}
		awaitModelArrival(t, e)
		steered, err := client.SubmitSessionWithResponse(ctx, session, protocol.SubmitRequest{
			OperationId: "op-steer", Mode: protocol.SubmitRequestModeRegular,
			Content: []protocol.ContentPart{textContentPart(t, "steer-1", nil)},
		})
		if err != nil || steered.JSON200 == nil || steered.JSON200.Disposition != protocol.SubmitResultDispositionSteering || steered.JSON200.Operation != nil {
			t.Fatalf("steering submit = %v %+v, want buffered steering with no invented Operation", err, steered.JSON200)
		}

		// coherent pending read while the head waits
		buffered := mountedHydration(t, client, session)
		if len(buffered.Pending.Steering) != 1 || buffered.Pending.Steering[0].OperationId != "op-steer" {
			t.Fatalf("pending steering = %+v, want the caller-ID head", buffered.Pending.Steering)
		}
		for _, op := range buffered.Operations {
			if op.OperationId == "op-steer" {
				t.Fatalf("the pending head was projected as an Operation before its adoption")
			}
		}

		release()
		awaitOperation(t, r, session, "op-1", harness.OperationSuccess)
		awaitOperation(t, r, session, "op-steer", harness.OperationSuccess)

		adopted := mountedHydration(t, client, session)
		if len(adopted.Pending.Steering) != 0 || len(adopted.Pending.Queued) != 0 {
			t.Fatalf("pending after adoption = %+v / %+v, want both queues empty", adopted.Pending.Steering, adopted.Pending.Queued)
		}
		foundSuccessor := false
		for _, op := range adopted.Operations {
			if op.OperationId == "op-steer" {
				foundSuccessor = true
			}
		}
		if !foundSuccessor {
			t.Fatalf("operations = %+v, want the caller-ID successor among them", adopted.Operations)
		}
		// the totals prove only the two actual generations contributed: the
		// silent handoff settles no assistant and adds no usage
		if len(adopted.Usage.Totals.ByModel) != 1 {
			t.Fatalf("usage by_model = %+v, want exactly the one served model", adopted.Usage.Totals.ByModel)
		}
		served := adopted.Usage.Totals.ByModel[0].Usage
		if served.InputTokens != "12" || served.OutputTokens != "7" {
			t.Fatalf("usage totals = %+v, want exactly the two generations' sum (12/7)", served)
		}

		// history: exactly the two assistants, two owned inputs and two
		// silent-success end items; signals and compactions are rejected —
		// this scenario compacts nothing and interrupts nothing
		history, err := client.GetSessionHistoryWithResponse(ctx, session, &protocol.GetSessionHistoryParams{})
		if err != nil || history.JSON200 == nil {
			t.Fatalf("history = %v status %d: %s", err, history.HTTPResponse.StatusCode, history.Body)
		}
		inputOwners := map[string]int{}
		endItems := 0
		assistants := 0
		for _, item := range history.JSON200.Items {
			switch itemKind(t, item) {
			case "input":
				in, ierr := item.AsInputItem()
				if ierr != nil {
					t.Fatalf("decode input item: %v", ierr)
				}
				if in.OperationId == nil || *in.OperationId == "" {
					t.Fatalf("history input without ownership: %+v", in)
				}
				inputOwners[*in.OperationId]++
			case "assistant":
				assistants++
			case "operation_end":
				end, derr := item.AsOperationEndItem()
				if derr != nil {
					t.Fatalf("decode end item: %v", derr)
				}
				if end.Status != protocol.OperationEndItemStatusSuccess || end.Detail != "" {
					t.Fatalf("end item = %+v, want the silent success", end)
				}
				if end.OperationId == nil || (*end.OperationId != "op-1" && *end.OperationId != "op-steer") {
					t.Fatalf("end item = %+v, want exactly the two caller identities", end)
				}
				endItems++
			case "signal":
				t.Fatalf("history carried a signal item in the clean handoff scenario")
			case "compaction":
				t.Fatalf("history carried a compaction item in the clean handoff scenario")
			default:
				t.Fatalf("history carried an unknown wire kind %q", itemKind(t, item))
			}
		}
		if assistants != 2 || endItems != 2 ||
			inputOwners["op-1"] != 1 || inputOwners["op-steer"] != 1 || len(inputOwners) != 2 {
			t.Fatalf("history shape = %d assistants, %d end items, inputs %v, want exactly two/two and one owned input per caller",
				assistants, endItems, inputOwners)
		}
	})
}

// TestSteeringCodeGroupsRootChildForkIndependence proves the code-group
// isolation of delivered user steering across lineages: every admitted input
// owns its own group and restore boundary, a copied fork prefix inherits no
// group, and no restore crosses a Session or copy boundary.
func TestSteeringCodeGroupsRootChildForkIndependence(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		f := openCodeRuntime(t, store)
		defer func() { _ = f.r.Close(context.Background()) }()
		ctx := context.Background()

		// the root's two delivered messages each own a group
		gate := make(chan struct{})
		f.e.server.setHold(gate)
		session := f.session("groups")
		f.submit(session, "root-1", "root work")
		awaitModelArrival(t, f.e)
		if got := f.submitMode(session, "root-2", "root steer", harness.MessageModeRegular); got != harness.DispositionSteering {
			t.Fatalf("root steering submit = %q, want steering", got)
		}
		close(gate)
		awaitOperation(t, f.r, session, "root-1", harness.OperationSuccess)
		awaitOperation(t, f.r, session, "root-2", harness.OperationSuccess)
		awaitRestorableSession(t, f.r, session)
		rootFirst := readOperation(t, f.r, session, "root-1").Admission.AdmittedEntry.EntryID
		rootSteer := readOperation(t, f.r, session, "root-2").Admission.AdmittedEntry.EntryID
		workspace := filepath.Join(f.r.dataDir, "groups-ws")
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		rootFile := filepath.Join(workspace, "root.txt")
		if err := os.WriteFile(rootFile, []byte("r0"), 0o600); err != nil {
			t.Fatalf("write root preimage: %v", err)
		}
		firstFile := filepath.Join(workspace, "first.txt")
		if err := os.WriteFile(firstFile, []byte("f0"), 0o600); err != nil {
			t.Fatalf("write first preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(f.r, session, rootFirst), firstFile, firstFile, []byte("f1"))
		captureCodeMutation(t, codeGroupRoot(f.r, session, rootSteer), rootFile, rootFile, []byte("r1"))
		if got := chainCodeGroupDirs(t, f.r.dataDir, session); len(got) != 2 || !slices.Contains(got, rootFirst) || !slices.Contains(got, rootSteer) {
			t.Fatalf("root groups = %v, want each delivered input's own group", got)
		}

		// a child session with delivered steering owns groups under its own
		// Session tree only
		childGate := make(chan struct{})
		f.e.server.setHold(childGate)
		child := launchChildThroughRuntime(t, f.r, session, "child-op-1")
		awaitModelArrival(t, f.e)
		var steerDisposition harness.SubmitDisposition
		if err := f.r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			res, err := h.Submit(ctx, harness.SubmitRequest{
				SessionID: child, OperationID: "child-op-2", Origin: harness.InputOriginUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: "child steer"}}, Mode: harness.MessageModeRegular,
			})
			if err != nil {
				return err
			}
			steerDisposition = res.Disposition
			return nil
		}); err != nil {
			t.Fatalf("child steering submit: %v", err)
		}
		if steerDisposition != harness.DispositionSteering {
			t.Fatalf("child steering submit = %q, want steering", steerDisposition)
		}
		close(childGate)
		awaitOperation(t, f.r, child, "child-op-1", harness.OperationSuccess)
		awaitOperation(t, f.r, child, "child-op-2", harness.OperationSuccess)
		awaitRestorableSession(t, f.r, child)
		childSteer := readOperation(t, f.r, child, "child-op-2").Admission.AdmittedEntry.EntryID
		childFile := filepath.Join(workspace, "child.txt")
		if err := os.WriteFile(childFile, []byte("c0"), 0o600); err != nil {
			t.Fatalf("write child preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(f.r, child, childSteer), childFile, childFile, []byte("c1"))
		if got := chainCodeGroupDirs(t, f.r.dataDir, child); len(got) == 0 || !slices.Contains(got, childSteer) {
			t.Fatalf("child groups = %v, want its delivered input's own group %q", got, childSteer)
		}
		if got := chainCodeGroupDirs(t, f.r.dataDir, session); slices.Contains(got, childSteer) {
			t.Fatalf("the child's group %q leaked into the parent's Session tree", childSteer)
		}
		childRestore, err := f.r.revertSessionCode(ctx, child, "child-op-2")
		if err != nil {
			t.Fatalf("child restore: %v", err)
		}
		if len(childRestore.Restored) != 1 || childRestore.Restored[0] != childFile {
			t.Fatalf("child restore = %v, want only its own group's file", childRestore.Restored)
		}
		if data, err := os.ReadFile(rootFile); err != nil || string(data) != "r1" {
			t.Fatalf("root file = (%q, %v), want no cross-session restore", data, err)
		}

		// a fork copies no group across the boundary: the copied prefix is
		// operationless, and a steering head delivered inside the fork owns
		// its own group beside the fork's first admitted input
		forkGate := make(chan struct{})
		f.e.server.setHold(forkGate)
		fork, err := f.r.forkSession(ctx, session, protocol.ForkRequest{
			BoundaryItemId: projectItemID(session, rootSteer),
			OperationId:    "fork-1",
			Content:        []protocol.ContentPart{textContentPart(t, "fork work", nil)},
		})
		if err != nil {
			t.Fatalf("fork: %v", err)
		}
		forkSession := fork.Session.SessionId
		awaitModelArrival(t, f.e) // the fork's first request is held
		if got := f.submitMode(forkSession, "fork-2", "fork steer", harness.MessageModeRegular); got != harness.DispositionSteering {
			t.Fatalf("fork steering submit = %q, want steering", got)
		}
		close(forkGate)
		awaitOperation(t, f.r, forkSession, "fork-1", harness.OperationSuccess)
		awaitOperation(t, f.r, forkSession, "fork-2", harness.OperationSuccess)
		awaitRestorableSession(t, f.r, forkSession)
		forkEntry := readOperation(t, f.r, forkSession, "fork-1").Admission.AdmittedEntry.EntryID
		forkSteer := readOperation(t, f.r, forkSession, "fork-2").Admission.AdmittedEntry.EntryID
		forkFile := filepath.Join(workspace, "fork.txt")
		if err := os.WriteFile(forkFile, []byte("f0"), 0o600); err != nil {
			t.Fatalf("write fork preimage: %v", err)
		}
		forkSteerFile := filepath.Join(workspace, "fork-steer.txt")
		if err := os.WriteFile(forkSteerFile, []byte("s0"), 0o600); err != nil {
			t.Fatalf("write fork-steer preimage: %v", err)
		}
		captureCodeMutation(t, codeGroupRoot(f.r, forkSession, forkEntry), forkFile, forkFile, []byte("f1"))
		captureCodeMutation(t, codeGroupRoot(f.r, forkSession, forkSteer), forkSteerFile, forkSteerFile, []byte("s1"))
		if got := chainCodeGroupDirs(t, f.r.dataDir, forkSession); len(got) != 2 || !slices.Contains(got, forkEntry) || !slices.Contains(got, forkSteer) {
			t.Fatalf("fork groups = %v, want the fork's two delivered inputs' own groups (no copied inheritance)", got)
		}
		if got := chainCodeGroupDirs(t, f.r.dataDir, forkSession); slices.Contains(got, rootFirst) || slices.Contains(got, rootSteer) {
			t.Fatalf("fork groups inherited a source group: %v", got)
		}
		forkRestore, err := f.r.revertSessionCode(ctx, forkSession, "fork-2")
		if err != nil {
			t.Fatalf("fork restore: %v", err)
		}
		if len(forkRestore.Restored) != 1 || forkRestore.Restored[0] != forkSteerFile {
			t.Fatalf("fork-steer restore = %v, want only its own group's file", forkRestore.Restored)
		}
		if data, err := os.ReadFile(forkFile); err != nil || string(data) != "f1" {
			t.Fatalf("fork-first file = (%q, %v), want untouched by the fork-steer boundary restore", data, err)
		}
		if data, err := os.ReadFile(rootFile); err != nil || string(data) != "r1" {
			t.Fatalf("source file = (%q, %v), want no cross-session restore from the fork", data, err)
		}
		if data, err := os.ReadFile(childFile); err != nil || string(data) != "c0" {
			t.Fatalf("child file = (%q, %v), want untouched by the fork's restore", data, err)
		}
	})
}
