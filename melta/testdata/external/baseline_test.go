// Package external_test is an independent Go module that consumes only the
// public packages of the module it replaces to: its acceptance proves the
// public construction is usable without the repository's internal helpers.
package external_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/melta"
	"github.com/Syntheresis/melta/model"
	"github.com/Syntheresis/melta/protocol"
)

const (
	externalKeyEnv   = "MELTA_EXTERNAL_KEY"
	externalKey      = "sk-external-baseline"
	externalStrategy = "external.summary/v1"
	externalFile     = "composed.txt"
)

// memoryCredentials is the in-memory public CredentialSource of this module:
// captured values by name, owned updates, and one stable child environment.
type memoryCredentials struct {
	mu     sync.Mutex
	values map[string]melta.CredentialValue
}

func (s *memoryCredentials) Capture(ctx context.Context, names []string) (map[string]melta.CredentialValue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]melta.CredentialValue, len(names))
	for _, name := range names {
		out[name] = s.values[name]
	}
	return out, nil
}

func (s *memoryCredentials) Set(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = melta.CredentialValue{Value: value, Defined: true, Managed: true}
	return nil
}

func (s *memoryCredentials) Remove(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = melta.CredentialValue{}
	return nil
}

func (s *memoryCredentials) SubprocessEnv(context.Context, []string) ([]string, error) {
	return []string{"PATH=" + os.Getenv("PATH")}, nil
}

// externalSummaryStrategy is the host-selected context strategy: the manual
// trigger plans one tool-free model pass and folds its completed text into the
// one summary checkpoint at the frozen boundary.
type externalSummaryStrategy struct{}

func (externalSummaryStrategy) Plan(in harness.PlanInput) (harness.PlanOutput, error) {
	if in.Trigger != "manual" {
		return harness.PlanOutput{Kind: harness.PlanNone}, nil
	}
	return harness.PlanOutput{Kind: harness.PlanModel, Model: &harness.ModelPlan{
		Request: model.Request{Messages: []model.Message{{
			Role:    model.RoleUser,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "summarize"}},
		}}},
	}}, nil
}

func (externalSummaryStrategy) Consume(in harness.ConsumeInput) (harness.PlanOutput, error) {
	summary := "external summary"
	if in.Output.Message != nil && in.Output.Message.TextContent() != "" {
		summary = in.Output.Message.TextContent()
	}
	message, err := model.NewMessage(model.Message{
		Role:    model.RoleAssistant,
		Source:  in.Output.Source,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\n" + summary + "\n\n[End of summary. Continue from here.]"}},
	})
	if err != nil {
		return harness.PlanOutput{}, err
	}
	return harness.PlanOutput{Kind: harness.PlanCheckpoint, Checkpoint: &harness.CheckpointPlan{
		BoundaryEntryID: in.Input.BoundaryEntryID,
		Items:           []harness.CheckpointItem{{Context: &message}},
	}}, nil
}

// recordingEvaluator is the custom permission factory's evaluator: it allows
// the two declared baseline permissions and records every evaluated pair.
type recordingEvaluator struct {
	mu   sync.Mutex
	seen []harness.PermissionRequest
}

func (e *recordingEvaluator) Evaluate(_ context.Context, action harness.PreparedAction) (harness.PermissionDecision, error) {
	e.mu.Lock()
	e.seen = append(e.seen, action.Permissions...)
	e.mu.Unlock()
	for _, request := range action.Permissions {
		switch request.Permission {
		case harness.PermissionFileWrite, harness.PermissionCommandRun:
		default:
			return harness.PermissionDeny, nil
		}
	}
	return harness.PermissionAllow, nil
}

func (e *recordingEvaluator) recorded() []harness.PermissionRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]harness.PermissionRequest(nil), e.seen...)
}

// scriptedModel is the OpenAI-compatible SSE endpoint the included transport
// reaches: the last user text selects the scripted turn, and a tool result
// always closes with plain text.
type scriptedModel struct {
	*httptest.Server

	mu     sync.Mutex
	bodies []string
	auth   []string
}

func newScriptedModel(t *testing.T) *scriptedModel {
	t.Helper()
	s := &scriptedModel{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *scriptedModel) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := string(raw)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	s.mu.Unlock()
	if wireLastRole(body) == "tool" {
		writeTextTurn(w, "done")
		return
	}
	switch wireLastUserText(body) {
	case "write v1":
		writeToolTurn(w, "call-write-1", "write_file", `{"path":"`+externalFile+`","content":"v1"}`)
	case "start job":
		writeToolTurn(w, "call-job", "run_command", `{"command":"sleep 30","background":true}`)
	case "summarize":
		writeTextTurn(w, "external summary")
	default:
		writeTextTurn(w, "done")
	}
}

func (s *scriptedModel) requests() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...), append([]string(nil), s.auth...)
}

func writeTextTurn(w http.ResponseWriter, text string) {
	writeSSE(w,
		`{"choices":[{"delta":{"role":"assistant","content":`+strconv.Quote(text)+`},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
}

func writeToolTurn(w http.ResponseWriter, id, name, arguments string) {
	writeSSE(w,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":`+strconv.Quote(id)+`,"type":"function","function":{"name":`+strconv.Quote(name)+`,"arguments":`+strconv.Quote(arguments)+`}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
}

func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		fmt.Fprintf(w, "data: %s\n\n", event)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func wireLastRole(body string) string {
	var doc struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if json.Unmarshal([]byte(body), &doc) != nil || len(doc.Messages) == 0 {
		return ""
	}
	return doc.Messages[len(doc.Messages)-1].Role
}

func wireLastUserText(body string) string {
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal([]byte(body), &doc) != nil {
		return ""
	}
	for i := len(doc.Messages) - 1; i >= 0; i-- {
		if doc.Messages[i].Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(doc.Messages[i].Content, &text) == nil {
			return text
		}
		return ""
	}
	return ""
}

func textPart(t *testing.T, text string) protocol.ContentPart {
	t.Helper()
	var part protocol.ContentPart
	if err := part.FromTextPart(protocol.TextPart{Text: text}); err != nil {
		t.Fatalf("text part: %v", err)
	}
	return part
}

func awaitOperation(t *testing.T, owner *melta.Melta, caller melta.Caller, sessionID, operationID string, want protocol.OperationStatus) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	var lastStatus protocol.OperationStatus
	for {
		hydration, err := owner.GetHydration(context.Background(), caller, sessionID)
		lastErr = err
		if err == nil {
			for _, operation := range hydration.Operations {
				if operation.OperationId != operationID {
					continue
				}
				lastStatus = operation.Status
				if operation.Status == want {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %q never reached %q (last status %q, last err %v)", operationID, want, lastStatus, lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func revertWhenIdle(t *testing.T, owner *melta.Melta, caller melta.Caller, sessionID, boundary string) protocol.CodeRevertResult {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		result, err := owner.RevertSessionCode(context.Background(), caller, sessionID, boundary)
		if err == nil {
			return result
		}
		if !errors.Is(err, harness.ErrConflict) {
			t.Fatalf("RevertSessionCode: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("RevertSessionCode never left the busy artifact interval: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func configurationDocument(endpoint string) []byte {
	document := map[string]any{
		"providers": map[string]any{
			"prov": map[string]any{
				"transport": map[string]any{"base_url": endpoint, "api_key_env": externalKeyEnv},
				"discovery": false,
				"models": map[string]any{"m": map[string]any{
					"name": "M", "context_window": 65536,
				}},
			},
		},
		"compaction": map[string]any{"model": "prov/m", "prompt": "external summary"},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return encoded
}

func openOwner(t *testing.T, options melta.Options) *melta.Melta {
	t.Helper()
	owner, err := melta.Open(context.Background(), options)
	if err != nil {
		t.Fatalf("melta.Open: %v", err)
	}
	return owner
}

func caller(t *testing.T, owner *melta.Melta) melta.Caller {
	t.Helper()
	bound, err := owner.Caller(melta.CallerContext{
		Origin: harness.InputOriginUser,
		Scope:  melta.CallerScope{Kind: melta.CallerScopeHarness},
	})
	if err != nil {
		t.Fatalf("Caller: %v", err)
	}
	return bound
}

func submit(t *testing.T, owner *melta.Melta, caller melta.Caller, sessionID, operationID, text string) {
	t.Helper()
	result, err := owner.SubmitSession(context.Background(), caller, sessionID, protocol.SubmitRequest{
		OperationId: operationID,
		Mode:        protocol.SubmitRequestModeRegular,
		Content:     []protocol.ContentPart{textPart(t, text)},
	})
	if err != nil {
		t.Fatalf("SubmitSession(%q): %v", operationID, err)
	}
	if result.Operation == nil || result.Operation.OperationId != operationID {
		t.Fatalf("SubmitSession(%q) = %+v, want the admitted operation", operationID, result)
	}
	awaitOperation(t, owner, caller, sessionID, operationID, protocol.OperationStatusSuccess)
}

// TestPublicBaselineEndToEnd exercises the complete public owner from an
// independent module: value configuration, a custom credential source, the
// included model transport against a local SSE server, a standard file
// mutation and background command, snapshot inspection and rewind, manual
// compaction over a custom strategy, and close/reopen history.
func TestPublicBaselineEndToEnd(t *testing.T) {
	homeBlocker := filepath.Join(t.TempDir(), "home-blocker")
	if err := os.WriteFile(homeBlocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write Home blocker: %v", err)
	}
	t.Setenv("HOME", filepath.Join(homeBlocker, "home"))
	server := newScriptedModel(t)
	dataDir := t.TempDir()
	workspace := t.TempDir()
	locations := melta.Locations{
		GlobalRulesDir:     filepath.Join(t.TempDir(), "rules"),
		DiscoveryCacheDir:  filepath.Join(t.TempDir(), "discovery"),
		WorkspacePolicyDir: filepath.Join(t.TempDir(), "projects"),
	}
	credentials := &memoryCredentials{values: map[string]melta.CredentialValue{
		externalKeyEnv: {Value: externalKey, Defined: true, Managed: true},
	}}
	inputs := melta.DefaultAgentInputs()
	inputs.Model = model.ModelRef{Provider: "prov", Model: "m"}
	inputs.Instructions = "The external baseline module."
	evaluator := &recordingEvaluator{}
	options := melta.Options{
		DataDir:       dataDir,
		Configuration: melta.NewValueConfigurationSource(configurationDocument(server.URL)),
		Credentials:   credentials,
		Locations:     locations,
		Identity:      "external baseline application",
		Agent:         &inputs,
		Strategy: &harness.StrategyBinding{
			ID:                    externalStrategy,
			RequiresContextWindow: false,
			Strategy:              externalSummaryStrategy{},
		},
		NewEvaluator: func(melta.PolicyInputs) (harness.PermissionEvaluator, error) {
			return evaluator, nil
		},
	}
	owner := openOwner(t, options)
	t.Cleanup(func() {
		_ = owner.Close(context.Background())
	})
	ctx := context.Background()
	bound := caller(t, owner)

	session, err := owner.CreateSession(ctx, bound, workspace, nil)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if session.Workspace != workspace || session.AgentSelection.Inputs == nil || session.AgentSelection.Inputs.Model != "prov/m" {
		t.Fatalf("created session = %+v, want the fixed Workspace and resolved input model", session)
	}
	sessionID := session.SessionId

	if err := os.WriteFile(filepath.Join(workspace, externalFile), []byte("v0"), 0o600); err != nil {
		t.Fatalf("seed mutation target: %v", err)
	}
	submit(t, owner, bound, sessionID, "op-write-1", "write v1")
	{
		data, err := os.ReadFile(filepath.Join(workspace, externalFile))
		if err != nil || string(data) != "v1" {
			t.Fatalf("mutation = (%q, %v), want v1", data, err)
		}
	}

	groups, err := owner.ListSessionCodeSnapshots(ctx, bound, sessionID)
	if err != nil {
		t.Fatalf("ListSessionCodeSnapshots: %v", err)
	}
	recorded := map[string]bool{}
	for _, group := range groups {
		for _, file := range group.Files {
			if strings.HasSuffix(file.Path, externalFile) {
				recorded[group.OperationId] = true
			}
		}
	}
	if !recorded["op-write-1"] {
		t.Fatalf("snapshot groups = %+v, want the mutation operation recording %q", groups, externalFile)
	}

	reverted := revertWhenIdle(t, owner, bound, sessionID, "op-write-1")
	if reverted.Error != nil || len(reverted.Restored) != 1 || !strings.HasSuffix(reverted.Restored[0], externalFile) {
		t.Fatalf("revert = %+v, want the one restored file", reverted)
	}
	{
		data, err := os.ReadFile(filepath.Join(workspace, externalFile))
		if err != nil || string(data) != "v0" {
			t.Fatalf("reverted mutation = (%q, %v), want v0", data, err)
		}
	}

	submit(t, owner, bound, sessionID, "op-job", "start job")
	hydration, err := owner.GetHydration(ctx, bound, sessionID)
	if err != nil {
		t.Fatalf("GetHydration after the background command: %v", err)
	}
	if len(hydration.Background) != 1 {
		t.Fatalf("background members = %+v, want the started job", hydration.Background)
	}

	compactDeadline := time.Now().Add(20 * time.Second)
	for {
		_, err := owner.CompactSession(ctx, bound, sessionID, protocol.CompactRequest{OperationId: "op-compact"})
		if err == nil {
			break
		}
		if !errors.Is(err, harness.ErrInvalid) || !strings.Contains(err.Error(), "is not idle; admission requires an idle Session") {
			t.Fatalf("CompactSession: %v", err)
		}
		if time.Now().After(compactDeadline) {
			t.Fatalf("CompactSession never admitted within the wait bound: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	awaitOperation(t, owner, bound, sessionID, "op-compact", protocol.OperationStatusSuccess)

	page, err := owner.GetHistory(ctx, bound, sessionID, nil)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	var checkpoints int
	for _, item := range page.Items {
		checkpoint, err := item.AsCheckpointItem()
		if err != nil || checkpoint.Kind != protocol.ContextCheckpoint {
			continue
		}
		checkpoints++
		if checkpoint.Strategy != externalStrategy || len(checkpoint.Messages) != 1 || checkpoint.Messages[0].Content == nil || len(*checkpoint.Messages[0].Content) != 1 {
			t.Fatalf("checkpoint = %+v, want the custom strategy's one summary message", checkpoint)
		}
		part, err := (*checkpoint.Messages[0].Content)[0].AsTextPart()
		if err != nil || !strings.Contains(part.Text, "external summary") {
			t.Fatalf("checkpoint text = (%+v, %v), want the scripted summary", part, err)
		}
	}
	if checkpoints != 1 {
		t.Fatalf("checkpoint items = %d, want exactly one", checkpoints)
	}

	permissions := evaluator.recorded()
	var wrote, commanded bool
	for _, permission := range permissions {
		wrote = wrote || permission.Permission == harness.PermissionFileWrite
		commanded = commanded || permission.Permission == harness.PermissionCommandRun
	}
	if !wrote || !commanded {
		t.Fatalf("evaluated permissions = %+v, want the custom evaluator authorizing the file and command mutations", permissions)
	}
	{
		_, auth := server.requests()
		if len(auth) == 0 || auth[0] != "Bearer "+externalKey {
			t.Fatalf("model authorization = %q, want the captured credential", auth)
		}
	}

	if err := owner.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openOwner(t, options)
	defer func() {
		_ = reopened.Close(ctx)
	}()
	reopenedCaller := caller(t, reopened)
	sessions, err := reopened.ListSessions(ctx, reopenedCaller, protocol.ListSessionsParams{Workspace: workspace, Lifecycle: protocol.Open})
	if err != nil || len(sessions) != 1 || sessions[0].SessionId != sessionID {
		t.Fatalf("reopened ListSessions = (%+v, %v), want the persisted Session", sessions, err)
	}
	reopenedPage, err := reopened.GetHistory(ctx, reopenedCaller, sessionID, nil)
	if err != nil {
		t.Fatalf("reopened GetHistory: %v", err)
	}
	var inputsSeen, checkpointsSeen int
	for _, item := range reopenedPage.Items {
		if input, err := item.AsInputItem(); err == nil && input.Kind == protocol.Input {
			for _, content := range input.Content {
				if part, err := content.AsTextPart(); err == nil {
					switch part.Text {
					case "write v1", "start job":
						inputsSeen++
					}
				}
			}
		}
		if checkpoint, err := item.AsCheckpointItem(); err == nil && checkpoint.Kind == protocol.ContextCheckpoint && checkpoint.Strategy == externalStrategy {
			checkpointsSeen++
		}
	}
	if inputsSeen != 2 || checkpointsSeen != 1 {
		t.Fatalf("reopened history = (%d inputs, %d checkpoints), want the persisted two inputs and one checkpoint", inputsSeen, checkpointsSeen)
	}
}
