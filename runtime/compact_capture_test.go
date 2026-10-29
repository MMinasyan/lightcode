package runtime

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// retainedSummarizerPrompt is the compact built-in's retained summarizer
// prompt, pinned here as an independent literal of the resolved compact
// definition.
const retainedSummarizerPrompt = `You are summarizing a coding agent conversation so the agent can continue working with reduced context. Your summary must preserve everything the agent needs to continue without re-reading files or re-asking the user.

Produce a summary with these sections:

## Goal
What the user asked for, in their words. Include exact quotes of key instructions.

## Files
Which files were read, created, or modified. For modified files: what changed and why.

## Current state
What has been done so far. What works, what doesn't.

## Unfinished work
Tasks started but not completed. Errors not yet resolved. Things the agent said it would do next.

## Decisions
Choices the user made or confirmed. Constraints they stated. Things they rejected.

Be specific — include file paths, function names, error messages, line numbers. Do not generalize. If the user said "don't use React," write that exactly, not "user has framework preferences."`

// Compaction-capture fixtures: the concrete preparation must populate the
// capture's conversation window, output reserve and compact configuration
// from one revision, with the compact selection falling back to the
// conversation model when the compact type's model does not resolve.

// compactPrepConfigDocument is the compaction-capture fixture document: the
// referenced keyed provider beside one provider whose credential stays
// absent, both pointed at the given endpoint. Every model carries its own
// captured-price fixture rate.
func compactPrepConfigDocument(endpoint string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "PREP_COMPACT_KEY",
		`{"m":{"name":"M","context_window":4096,"cost":{"input":1.5,"output":3,"cache_read":0.5,"cache_write":0.25}},`+
			`"m2":{"name":"M2","context_window":2048,"max_output_tokens":1000,"cost":{"input":0.5,"output":1,"cache_read":0.125,"cache_write":0.0625}},`+
			`"mzero":{"name":"MZ","context_window":0,"max_output_tokens":500,"cost":{"input":0.25}}}`) +
		`,"p2":` + nominalProvMember(endpoint, "PREP_COMPACT_ABSENT_KEY",
		`{"m3":{"name":"M3","context_window":1024,"max_output_tokens":200,"cost":{"input":7}}}`) + `},"sessions":{}}`
}

// agentsWithCompact appends one compact builtin overlay to the retained
// preparation agents document.
func agentsWithCompact(overlay string) string {
	return strings.TrimSuffix(prepAgentsDocument, "}") + ",\n  \"compact\": " + overlay + "}"
}

// compactPrep wires one concrete preparation (nil controlled prepare) over
// the compact fixture documents, publishes the configuration once, and
// returns that publication's own snapshot+credential capture.
func compactPrep(t *testing.T, agentsDoc, endpoint string) (configurationCapture, *preparation) {
	t.Helper()
	sh := newServiceHarness(t)
	t.Setenv("PREP_COMPACT_KEY", "compact-secret")
	writeServiceFile(t, sh.configPath, compactPrepConfigDocument(endpoint))
	writeServiceFile(t, agents.PathForConfig(sh.configPath), agentsDoc)
	owner, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	comp := mustComposition(t, compactToolsPlugin())
	runtimeScope := mustOpenScope(t, comp, owner, ScopeInfo{Kind: ScopeRuntime, DataDir: sh.dataDir}, nil)
	obs := newObservation()
	ws := newWorkspaceScopes(owner, comp, []*scope{runtimeScope}, obs)
	svc := newConfigurationService(owner, comp, sh.loader, sh.configPath, obs)
	captured, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	return captured, newPreparation(svc, comp, runtimeScope, ws, sh.home, nil, nil, nil, nil)
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

func compactPrepSelection(t *testing.T, captured configurationCapture, agentName string) selection {
	t.Helper()
	agent, err := harness.ResolveAgentType(agentName, captured.snapshot.agentTypes())
	testassert.Require(t, err == nil, "ResolveAgentType(%q): %v", agentName, err)
	return selection{agent: agent, bindings: Bindings{}, invocation: Invocation{snapshot: captured.snapshot}, credentials: captured.credentials}
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
			Model:        model.ModelRef{Provider: "prov", Model: "m2"},
			SystemPrompt: "summarize",
		},
		Strategy: harness.StrategyCapture{ID: harness.IncludedStrategyID, RequiresContextWindow: true},
	}
}

func testToolDefinitionRuntime() model.ToolDefinition {
	return model.ToolDefinition{Name: "echo", Description: "echoes", Parameters: []byte(`{"type":"object"}`)}
}

// TestConcretePrepareCompactCapture pins every capture field the compaction
// configuration must carry from one revision: the conversation window, the
// output reserve with the 131072 fallback, the compact selection with the
// conversation-model default, the compact prompt equal to the resolved
// compact type's Prompt, and the captured pricing of the final effective
// conversation and compact models — the conversation model's own rates
// exactly when the fallback kept that model and its transport, and the
// fallback candidate's rates never reaching a capture it did not select.
func TestConcretePrepareCompactCapture(t *testing.T) {
	conversationRef := model.ModelRef{Provider: "prov", Model: "m"}
	m2Ref := model.ModelRef{Provider: "prov", Model: "m2"}
	fpr := func(v float64) *float64 { return &v }
	mPricing := &harness.ModelPricing{Input: fpr(1.5), Output: fpr(3), CacheRead: fpr(0.5)}
	m2Pricing := &harness.ModelPricing{Input: fpr(0.5), Output: fpr(1), CacheRead: fpr(0.125)}

	cases := []struct {
		name               string
		agentsDoc          string
		agent              string
		wantModel          model.ModelRef
		wantWindow         int
		wantReserve        int
		wantCompactRef     model.ModelRef
		wantPricing        *harness.ModelPricing
		wantCompactPricing *harness.ModelPricing
	}{
		{
			name:               "conversation model with the reserve fallback",
			agentsDoc:          prepAgentsDocument,
			agent:              "solo",
			wantModel:          conversationRef,
			wantWindow:         4096,
			wantReserve:        131072,
			wantCompactRef:     conversationRef,
			wantPricing:        mPricing,
			wantCompactPricing: mPricing,
		},
		{
			name:               "compact model override via the roster",
			agentsDoc:          agentsWithCompact(`{"model": "prov/m2"}`),
			agent:              "solo",
			wantModel:          conversationRef,
			wantWindow:         4096,
			wantReserve:        131072,
			wantCompactRef:     m2Ref,
			wantPricing:        mPricing,
			wantCompactPricing: m2Pricing,
		},
		{
			name:               "compact model that does not resolve falls back",
			agentsDoc:          agentsWithCompact(`{"model": "prov/absent"}`),
			agent:              "solo",
			wantModel:          conversationRef,
			wantWindow:         4096,
			wantReserve:        131072,
			wantCompactRef:     conversationRef,
			wantPricing:        mPricing,
			wantCompactPricing: mPricing,
		},
		{
			name:               "compact key that does not resolve falls back",
			agentsDoc:          agentsWithCompact(`{"model": "p2/m3"}`),
			agent:              "solo",
			wantModel:          conversationRef,
			wantWindow:         4096,
			wantReserve:        131072,
			wantCompactRef:     conversationRef,
			wantPricing:        mPricing,
			wantCompactPricing: mPricing,
		},
		{
			// catalog.Lookup rejects the zero-window model, so the
			// conversation-model default applies.
			name:               "compact entry with no positive window falls back",
			agentsDoc:          agentsWithCompact(`{"model": "prov/mzero"}`),
			agent:              "solo",
			wantModel:          conversationRef,
			wantWindow:         4096,
			wantReserve:        131072,
			wantCompactRef:     conversationRef,
			wantPricing:        mPricing,
			wantCompactPricing: mPricing,
		},
		{
			name:               "conversation reserve from the entry's max output tokens",
			agentsDoc:          strings.TrimSuffix(prepAgentsDocument, "}") + ",\n  \"reservable\": {\"model\": \"prov/m2\", \"system_prompt\": \"simple\", \"tools\": [\"echo\"]}}",
			agent:              "reservable",
			wantModel:          m2Ref,
			wantWindow:         2048,
			wantReserve:        1000,
			wantCompactRef:     m2Ref,
			wantPricing:        m2Pricing,
			wantCompactPricing: m2Pricing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured, prep := compactPrep(t, tc.agentsDoc, "https://prov.test/v1")
			sel := compactPrepSelection(t, captured, tc.agent)
			capture, opener, err := prep.concretePrepare(context.Background(), compactPrepRequest(), sel)
			testassert.NoError(t, err)
			testassert.Require(t, opener != nil, "concretePrepare returned no opener")
			if capture.Model != tc.wantModel {
				t.Fatalf("capture Model = %s, want %s", capture.Model.String(), tc.wantModel.String())
			}
			testassert.Require(t, capture.ContextWindow == tc.wantWindow && capture.OutputReserve == tc.wantReserve, "got %v, want %v; got %v, want %v", capture.ContextWindow, tc.wantWindow, capture.OutputReserve, tc.wantReserve)
			if capture.Compact.Model != tc.wantCompactRef {
				t.Fatalf("capture Compact.Model = %s, want %s", capture.Compact.Model.String(), tc.wantCompactRef.String())
			}
			testassert.Equal(t, capture.Compact.SystemPrompt, retainedSummarizerPrompt)
			testassert.Require(t, capture.Pricing.Equal(tc.wantPricing), "capture pricing = %+v, want the conversation entry's catalog rates", capture.Pricing)
			testassert.Require(t, capture.Compact.Pricing.Equal(tc.wantCompactPricing), "capture compact pricing = %+v, want the final effective compact model's catalog rates", capture.Compact.Pricing)
		})
	}
}

// TestCapturedPricingDropsNonFiniteCatalogRates pins the catalog-to-capture
// rate copy: a nil cost means unknown pricing, present finite rates are
// copied independently, and a non-finite catalog rate becomes an absent
// rate rather than a preparation failure.
func TestCapturedPricingDropsNonFiniteCatalogRates(t *testing.T) {
	fpr := func(v float64) *float64 { return &v }
	if capturedPricing(nil) != nil {
		t.Fatal("capturedPricing(nil) is not unknown pricing")
	}
	cost := &catalog.Cost{
		Input:      fpr(math.NaN()),
		Output:     fpr(math.Inf(1)),
		CacheRead:  fpr(0.5),
		CacheWrite: fpr(0.25),
	}
	got := capturedPricing(cost)
	// The catalog's cache-write metadata is not a captured rate: only the
	// finite input, output, and cache-read rates are carried.
	testassert.Require(t, got != nil && got.Input == nil && got.Output == nil && got.CacheRead != nil && *got.CacheRead == 0.5,
		"capturedPricing(%+v) = %+v, want only the finite captured rate kept", cost, got)
	*cost.CacheRead = 99 // the capture owns its rate pointees
	testassert.Require(t, *got.CacheRead == 0.5, "mutating the catalog rate reached the captured pricing: %v", *got.CacheRead)
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

// TestConcreteOpenerCompactModel proves the invocation factory's maintenance
// Model callback reaches the compact model's transport — its catalog model and
// resolved credential — and that when the effective compact model is the
// conversation model the closure is built identically over the same transport.
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
			captured, prep := compactPrep(t, tc.agentsDoc, server.URL)
			sel := compactPrepSelection(t, captured, "solo")
			capture, opener, err := prep.concretePrepare(context.Background(), compactPrepRequest(), sel)
			testassert.NoError(t, err)
			admission := harness.OperationAdmission{
				SessionID:   "000000000000000000000000000000aa",
				OperationID: "op-1",
				AgentType:   "solo",
				Execution:   capture,
				AdmittedAt:  time.Now(),
			}
			inv, err := opener(context.Background(), admission, sel, harness.InvocationRequest{
				ID:      "inv-maintenance",
				Purpose: harness.InvocationPurposeMaintenance,
				Model:   capture.Compact.Model,
			})
			testassert.NoError(t, err)
			testassert.Require(t, inv.Model != nil, "opened maintenance invocation carries no Model callback")
			req, err := model.NewRequest(model.Request{Messages: []model.Message{{
				Role:    "user",
				Content: []model.ContentPart{{Kind: model.PartText, Text: "summarize"}},
			}}})
			testassert.NoError(t, err)
			stream, err := inv.Model(context.Background(), req)
			testassert.NoError(t, err)
			stream.Close()
			testassert.Require(t, len(server.models) == 1 && server.models[0] == tc.wantCompactOn && len(server.auths) == 1 && server.auths[0] == "Bearer compact-secret", "maintenance Model wire models = %v, want exactly one request on %q; maintenance Model authorization = %v, want the compact transport's resolved credential", server.models, tc.wantCompactOn, server.auths)

			// The conversation invocation's callback rides its own transport;
			// when the compact model is the conversation model the two closures
			// are built identically over the one shared transport, so both hit
			// the same wire model and credential.
			conv, err := opener(context.Background(), admission, sel, harness.InvocationRequest{
				ID:      "inv-conversation",
				Purpose: harness.InvocationPurposeConversation,
				Model:   capture.Model,
			})
			testassert.NoError(t, err)
			stream, err = conv.Model(context.Background(), req)
			testassert.NoError(t, err)
			stream.Close()
			testassert.Require(t, len(server.models) == 2 && server.models[1] == "m", "conversation Model wire models = %v, want the conversation model second", server.models)
			testassert.Require(t, capture.Compact.Model != conversationRef || server.auths[1] == server.auths[0], "conversation authorization = %q differs from the compact one %q on the shared-model case", server.auths[1], server.auths[0])
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
	testassert.NoError(t, validateHookedCapture(base, prompted))
	// The tool-definition replacement freedom still holds: same names, new
	// description.
	tooled := base
	tooled.Tools = append([]model.ToolDefinition(nil), base.Tools...)
	tooled.Tools[0].Description = "replaced description"
	testassert.NoError(t, validateHookedCapture(base, tooled))

	for _, mutation := range []struct {
		name   string
		mutate func(*harness.ExecutionCapture)
	}{
		{"context window", func(c *harness.ExecutionCapture) { c.ContextWindow++ }},
		{"output reserve", func(c *harness.ExecutionCapture) { c.OutputReserve++ }},
		{"compact model", func(c *harness.ExecutionCapture) { c.Compact.Model = model.ModelRef{Provider: "other", Model: "m"} }},
		{"compact system prompt", func(c *harness.ExecutionCapture) { c.Compact.SystemPrompt = "changed" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			next := base
			mutation.mutate(&next)
			{
				err := validateHookedCapture(base, next)
				testassert.Require(t, err != nil, "hook changing the %s accepted, want rejection", mutation.name)
			}
		})
	}
}
