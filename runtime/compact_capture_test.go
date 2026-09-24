package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/compact"
	"github.com/MMinasyan/lightcode/model"
)

// Compaction-capture fixtures: the concrete preparation must populate the
// capture's conversation window, output reserve and compact configuration
// from one revision, with the compact selection falling back to the
// conversation model when the compact type's model does not resolve.

const compactPrepConfigTemplate = `{
  "providers": {
    "prov": {
      "transport": {"base_url": "ENDPOINT", "api_key_env": "PREP_COMPACT_KEY"},
      "discovery": false,
      "models": {
        "m": {"name": "M", "context_window": 4096},
        "m2": {"name": "M2", "context_window": 2048, "max_output_tokens": 1000},
        "mzero": {"name": "MZ", "context_window": 0, "max_output_tokens": 500}
      }
    },
    "p2": {
      "transport": {"base_url": "ENDPOINT", "api_key_env": "PREP_COMPACT_ABSENT_KEY"},
      "discovery": false,
      "models": {
        "m3": {"name": "M3", "context_window": 1024, "max_output_tokens": 200}
      }
    }
  },
  "sessions": {}
}`

// agentsWithCompact appends one compact builtin overlay to the retained
// preparation agents document.
func agentsWithCompact(overlay string) string {
	return strings.TrimSuffix(prepAgentsDocument, "}") + ",\n  \"compact\": " + overlay + "}"
}

// compactPrep wires one concrete preparation (nil controlled prepare) over
// the compact fixture documents and publishes the configuration once.
func compactPrep(t *testing.T, agentsDoc, endpoint string) (*configuration, *preparation) {
	t.Helper()
	sh := newServiceHarness(t)
	t.Setenv("PREP_COMPACT_KEY", "compact-secret")
	writeServiceFile(t, sh.configPath, strings.Replace(compactPrepConfigTemplate, "ENDPOINT", endpoint, -1))
	writeServiceFile(t, agents.PathForConfig(sh.configPath), agentsDoc)
	owner, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	comp := mustComposition(t, compactToolsPlugin())
	runtimeScope := mustOpenScope(t, comp, owner, ScopeInfo{Kind: ScopeRuntime, DataDir: sh.dataDir}, nil)
	obs := newObservation()
	ws := newWorkspaceScopes(owner, comp, []*scope{runtimeScope}, obs)
	svc := newConfigurationService(owner, comp, sh.loader, sh.configPath, obs)
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return svc.current(), newPreparation(svc, comp, runtimeScope, ws, sh.home, nil, nil)
}

func compactToolsPlugin() Plugin {
	return Plugin{
		ID:       "tools",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("echo", staticToolDescription("echo")), ToolSpec("read", staticToolDescription("read"))},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"echo": noopTool{}, "read": noopTool{}}}, nil
		},
	}
}

func compactPrepRequest() harness.PreparationRequest {
	return harness.PreparationRequest{
		Session: harness.PreparationSession{
			Identity:  harness.SessionIdentity{SessionID: "000000000000000000000000000000aa", Workspace: "/tmp/compact-ws", CreatedAt: time.Now()},
			AgentType: "solo",
		},
		RequestKind: harness.RequestKindMessage,
	}
}

func compactPrepSelection(t *testing.T, snapshot *configuration, agentName string) selection {
	t.Helper()
	agent, err := harness.ResolveAgentType(agentName, snapshot.agentTypes())
	if err != nil {
		t.Fatalf("ResolveAgentType(%q): %v", agentName, err)
	}
	return selection{agent: agent, bindings: Bindings{}, invocation: Invocation{snapshot: snapshot}}
}

// compactBaseCapture is the valid capture baseline of the hook-protection
// fixture.
func compactBaseCapture() harness.ExecutionCapture {
	return harness.ExecutionCapture{
		ConfigurationRevision: "rev-1",
		Model:                 model.ModelRef{Provider: "prov", Model: "m"},
		SystemPrompt:          "system",
		Tools:                 []model.ToolDefinition{testToolDefinitionRuntime()},
		ContextWindow:         4096,
		OutputReserve:         131072,
		Compact: harness.CompactCapture{
			Model:         model.ModelRef{Provider: "prov", Model: "m2"},
			ContextWindow: 2048,
			OutputReserve: 1000,
			SystemPrompt:  "summarize",
		},
	}
}

func testToolDefinitionRuntime() model.ToolDefinition {
	return model.ToolDefinition{Name: "echo", Description: "echoes", Parameters: []byte(`{"type":"object"}`)}
}

// TestConcretePrepareCompactCapture pins every capture field the compaction
// configuration must carry from one revision: the conversation window, the
// output reserve with the 131072 fallback, the compact selection with the
// conversation-model default, the compact window fallback, and the compact
// prompt equal to the resolved compact type's Prompt.
func TestConcretePrepareCompactCapture(t *testing.T) {
	conversationRef := model.ModelRef{Provider: "prov", Model: "m"}
	m2Ref := model.ModelRef{Provider: "prov", Model: "m2"}

	cases := []struct {
		name            string
		agentsDoc       string
		agent           string
		wantModel       model.ModelRef
		wantWindow      int
		wantReserve     int
		wantCompactRef  model.ModelRef
		wantCompWindow  int
		wantCompReserve int
	}{
		{
			name:            "conversation model with the reserve fallback",
			agentsDoc:       prepAgentsDocument,
			agent:           "solo",
			wantModel:       conversationRef,
			wantWindow:      4096,
			wantReserve:     131072,
			wantCompactRef:  conversationRef,
			wantCompWindow:  4096,
			wantCompReserve: 131072,
		},
		{
			name:            "compact model override via the roster",
			agentsDoc:       agentsWithCompact(`{"model": "prov/m2"}`),
			agent:           "solo",
			wantModel:       conversationRef,
			wantWindow:      4096,
			wantReserve:     131072,
			wantCompactRef:  m2Ref,
			wantCompWindow:  2048,
			wantCompReserve: 1000,
		},
		{
			name:            "compact model that does not resolve falls back",
			agentsDoc:       agentsWithCompact(`{"model": "prov/absent"}`),
			agent:           "solo",
			wantModel:       conversationRef,
			wantWindow:      4096,
			wantReserve:     131072,
			wantCompactRef:  conversationRef,
			wantCompWindow:  4096,
			wantCompReserve: 131072,
		},
		{
			name:            "compact key that does not resolve falls back",
			agentsDoc:       agentsWithCompact(`{"model": "p2/m3"}`),
			agent:           "solo",
			wantModel:       conversationRef,
			wantWindow:      4096,
			wantReserve:     131072,
			wantCompactRef:  conversationRef,
			wantCompWindow:  4096,
			wantCompReserve: 131072,
		},
		{
			// catalog.Lookup rejects a non-positive window as an incomplete
			// model, so the retained window fallback fires only defensively:
			// the observable behavior for such a compact model is the
			// conversation-model default.
			name:            "compact entry with no positive window falls back",
			agentsDoc:       agentsWithCompact(`{"model": "prov/mzero"}`),
			agent:           "solo",
			wantModel:       conversationRef,
			wantWindow:      4096,
			wantReserve:     131072,
			wantCompactRef:  conversationRef,
			wantCompWindow:  4096,
			wantCompReserve: 131072,
		},
		{
			name:            "conversation reserve from the entry's max output tokens",
			agentsDoc:       strings.TrimSuffix(prepAgentsDocument, "}") + ",\n  \"reservable\": {\"model\": \"prov/m2\", \"system_prompt\": \"simple\", \"tools\": [\"echo\"]}}",
			agent:           "reservable",
			wantModel:       m2Ref,
			wantWindow:      2048,
			wantReserve:     1000,
			wantCompactRef:  m2Ref,
			wantCompWindow:  2048,
			wantCompReserve: 1000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, prep := compactPrep(t, tc.agentsDoc, "https://prov.test/v1")
			sel := compactPrepSelection(t, snapshot, tc.agent)
			capture, opener, err := prep.concretePrepare(context.Background(), compactPrepRequest(), sel)
			if err != nil {
				t.Fatalf("concretePrepare: %v", err)
			}
			if opener == nil {
				t.Fatalf("concretePrepare returned no opener")
			}
			if capture.Model != tc.wantModel {
				t.Fatalf("capture Model = %s, want %s", capture.Model.String(), tc.wantModel.String())
			}
			if capture.ContextWindow != tc.wantWindow {
				t.Fatalf("capture ContextWindow = %d, want %d", capture.ContextWindow, tc.wantWindow)
			}
			if capture.OutputReserve != tc.wantReserve {
				t.Fatalf("capture OutputReserve = %d, want %d", capture.OutputReserve, tc.wantReserve)
			}
			if capture.Compact.Model != tc.wantCompactRef {
				t.Fatalf("capture Compact.Model = %s, want %s", capture.Compact.Model.String(), tc.wantCompactRef.String())
			}
			if capture.Compact.ContextWindow != tc.wantCompWindow {
				t.Fatalf("capture Compact.ContextWindow = %d, want %d", capture.Compact.ContextWindow, tc.wantCompWindow)
			}
			if capture.Compact.OutputReserve != tc.wantCompReserve {
				t.Fatalf("capture Compact.OutputReserve = %d, want %d", capture.Compact.OutputReserve, tc.wantCompReserve)
			}
			if capture.Compact.SystemPrompt != compact.DefaultSummarizerPrompt {
				t.Fatalf("capture Compact.SystemPrompt = %q, want the compact type's Prompt", capture.Compact.SystemPrompt)
			}
		})
	}
}

// compactRecordingServer records every request's model and authorization and
// answers one text turn.
type compactRecordingServer struct {
	*httptest.Server
	models []string
	auths  []string
}

func newCompactRecordingServer(t *testing.T) *compactRecordingServer {
	t.Helper()
	s := &compactRecordingServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var wire struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &wire) // malformed bodies record an empty model
		s.models = append(s.models, wire.Model)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		writeTextTurn(w, "done")
	}))
	t.Cleanup(s.Close)
	return s
}

// TestConcreteOpenerCompactModel proves the opener's CompactModel callback
// reaches the compact model's transport — its catalog model and resolved
// credential — and that when the effective compact model is the conversation
// model the closure is built identically over the same transport.
func TestConcreteOpenerCompactModel(t *testing.T) {
	conversationRef := model.ModelRef{Provider: "prov", Model: "m"}
	cases := []struct {
		name          string
		agentsDoc     string
		wantCompactOn string
	}{
		{"compact model override", agentsWithCompact(`{"model": "prov/m2"}`), "m2"},
		{"conversation model default", prepAgentsDocument, "m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newCompactRecordingServer(t)
			snapshot, prep := compactPrep(t, tc.agentsDoc, server.URL)
			sel := compactPrepSelection(t, snapshot, "solo")
			capture, opener, err := prep.concretePrepare(context.Background(), compactPrepRequest(), sel)
			if err != nil {
				t.Fatalf("concretePrepare: %v", err)
			}
			exec, err := opener(context.Background(), harness.OperationAdmission{
				SessionID:   "000000000000000000000000000000aa",
				OperationID: "op-1",
				AgentType:   "solo",
				Execution:   capture,
				AdmittedAt:  time.Now(),
			}, sel)
			if err != nil {
				t.Fatalf("opener: %v", err)
			}
			if exec.CompactModel == nil {
				t.Fatalf("opened execution carries no CompactModel callback")
			}
			req, err := model.NewRequest(model.Request{Messages: []model.Message{{
				Role:    "user",
				Content: []model.ContentPart{{Kind: model.PartText, Text: "summarize"}},
			}}})
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			stream, err := exec.CompactModel(context.Background(), req)
			if err != nil {
				t.Fatalf("CompactModel: %v", err)
			}
			stream.Close()
			if len(server.models) != 1 || server.models[0] != tc.wantCompactOn {
				t.Fatalf("CompactModel wire models = %v, want exactly one request on %q", server.models, tc.wantCompactOn)
			}
			if len(server.auths) != 1 || server.auths[0] != "Bearer compact-secret" {
				t.Fatalf("CompactModel authorization = %v, want the compact transport's resolved credential", server.auths)
			}

			// The conversation callback rides its own transport in the same
			// execution; when the compact model is the conversation model the
			// two closures are built identically over the one shared
			// transport, so both hit the same wire model and credential.
			stream, err = exec.Model(context.Background(), req)
			if err != nil {
				t.Fatalf("Model: %v", err)
			}
			stream.Close()
			if len(server.models) != 2 || server.models[1] != "m" {
				t.Fatalf("Model wire models = %v, want the conversation model second", server.models)
			}
			if capture.Compact.Model == conversationRef && server.auths[1] != server.auths[0] {
				t.Fatalf("conversation authorization = %q differs from the compact one %q on the shared-model case", server.auths[1], server.auths[0])
			}
		})
	}
}

// TestValidateHookedCaptureProtectsCompaction proves a preparation hook
// cannot change the compact members or the window and reserve: the three new
// members join the same prohibition family as model and revision, while the
// prompt and tool-definition replacement freedoms still hold.
func TestValidateHookedCaptureProtectsCompaction(t *testing.T) {
	base := compactBaseCapture()

	// The prompt freedom still holds.
	prompted := base
	prompted.SystemPrompt = "replaced prompt"
	if err := validateHookedCapture(base, prompted); err != nil {
		t.Fatalf("prompt replacement rejected: %v", err)
	}
	// The tool-definition replacement freedom still holds: same names, new
	// description.
	tooled := base
	tooled.Tools = append([]model.ToolDefinition(nil), base.Tools...)
	tooled.Tools[0].Description = "replaced description"
	if err := validateHookedCapture(base, tooled); err != nil {
		t.Fatalf("tool definition replacement rejected: %v", err)
	}

	for _, mutation := range []struct {
		name   string
		mutate func(*harness.ExecutionCapture)
	}{
		{"context window", func(c *harness.ExecutionCapture) { c.ContextWindow++ }},
		{"output reserve", func(c *harness.ExecutionCapture) { c.OutputReserve++ }},
		{"compact model", func(c *harness.ExecutionCapture) { c.Compact.Model = model.ModelRef{Provider: "other", Model: "m"} }},
		{"compact context window", func(c *harness.ExecutionCapture) { c.Compact.ContextWindow++ }},
		{"compact output reserve", func(c *harness.ExecutionCapture) { c.Compact.OutputReserve++ }},
		{"compact system prompt", func(c *harness.ExecutionCapture) { c.Compact.SystemPrompt = "changed" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			next := base
			mutation.mutate(&next)
			if err := validateHookedCapture(base, next); err == nil {
				t.Fatalf("hook changing the %s accepted, want rejection", mutation.name)
			}
		})
	}
}
