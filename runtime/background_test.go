package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// TestBackgroundBridgeMethodsForward drives each bridge method against the
// live Harness it carries: a launch returns the durable child identity, a job
// start hands its spawn the reserved completion identity, and the completion
// delivery admits the delivered content on the owning Session.
func TestBackgroundBridgeMethodsForward(t *testing.T) {
	ctx := context.Background()
	e := newOwnerEnv(t)
	store := storage.NewMemory()
	stopper := &stubStopper{events: e.events}
	r, err := e.open(ctx, e.storagePlugin(store), jobStopperPlugin(e, "jobs", "job-stopper", ScopeRuntime, stopper))
	testassert.NoError(t, err)
	bridge := &backgroundBridge{h: r.harness}

	parent, err := r.createSession(ctx, filepath.Join(e.home, "forward-parent"), "solo")
	testassert.NoError(t, err)
	res, err := bridge.LaunchChild(ctx, harness.LaunchChildRequest{
		ParentSessionID: parent.Identity.SessionID,
		AgentType:       "solo",
		Content:         []model.ContentPart{{Kind: model.PartText, Text: "child task"}},
		OperationID:     "op-child-launch",
		MaxConcurrent:   5,
		OutputLimit:     4096,
	})
	testassert.NoError(t, err)
	testassert.Require(t, res.ChildSessionID != "", "LaunchChild returned no child session id")

	jobSession, err := r.createSession(ctx, filepath.Join(e.home, "forward-job"), "solo")
	testassert.NoError(t, err)
	var completionID string
	testassert.NoError(t, bridge.StartJob(ctx, jobSession.Identity.SessionID, "0a1b2c3d", func(_ context.Context, id string) error {
		completionID = id
		return nil
	}))

	testassert.NoError(t, bridge.DeliverCompletion(ctx, jobSession.Identity.SessionID, completionID, "job report"))
	entries, err := store.ReadEntries(ctx, jobSession.Identity.SessionID, 0)
	testassert.NoError(t, err)
	admitted := false
	for _, entry := range entries {
		if entry.Kind == harness.EntryInput && strings.Contains(string(entry.Payload), "job report") {
			admitted = true
		}
	}
	testassert.Require(t, admitted, "the forwarded completion delivery left no admitted input entry")

	testassert.NoError(t, r.Close(ctx))
}

// backgroundProbe records the background services bridge of every prepared
// call the production opener builds. The probe is sequential: the test reads
// the slice only after the admitted call it observed has settled.
type backgroundProbe struct {
	seen []BackgroundServices
}

func (p *backgroundProbe) record(tc ToolContext) {
	p.seen = append(p.seen, tc.Background)
}

// backgroundProbeTool is the production-path probe: its preparation records
// the ToolContext it receives and settles immediately.
type backgroundProbeTool struct {
	probe *backgroundProbe
}

func (t backgroundProbeTool) describe(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        "bg_probe",
		Description: "records the prepared call's background services",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: true}, nil
}

func (t backgroundProbeTool) Normalize(_ ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t backgroundProbeTool) Prepare(_ context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	t.probe.record(tc)
	return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "probed"}}}
}

func backgroundProbePlugin(probe *backgroundProbe) Plugin {
	return Plugin{
		ID:       "bgprobe",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("bg_probe", backgroundProbeTool{probe: probe}.describe)},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"bg_probe": backgroundProbeTool{probe: probe}}}, nil
		},
	}
}

// TestBackgroundBridgeArmingAcrossPublication pins the arming pattern on the
// production preparation path: the bridge is created before harness.New and
// armed before the owner is published, so construction and publication
// produce no observation of it, while every prepared call afterward carries
// the complete armed bridge whose methods reach the Harness.
func TestBackgroundBridgeArmingAcrossPublication(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	server := newProductionModelServer(t, "bg_probe", "{}")
	home := t.TempDir()
	t.Setenv("HOME", home)
	isolateBundledCredentials(t)
	t.Setenv("PRODUCTION_TEST_KEY", "production-secret-1")
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, chainCommandConfigDocument(server.URL))
	writeServiceFile(t, agents.PathForConfig(configPath),
		`{"prober": {"model": "prov/m", "system_prompt": "simple", "tools": ["bg_probe"]}}`)
	workspace := filepath.Join(home, "probe-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))

	probe := &backgroundProbe{}
	r, err := Open(ctx, Options{DataDir: dataDir, ConfigPath: configPath, Plugins: []Plugin{
		coreStoragePlugin(store), backgroundProbePlugin(probe),
	}})
	testassert.NoError(t, err)
	// The unarmed-to-armed window ends before publication: construction
	// and publication observe nothing.
	testassert.Equal(t, len(probe.seen), 0)

	session, err := r.createSession(ctx, workspace, "prober")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "probe")
	awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)

	testassert.Require(t, len(probe.seen) != 0, "no prepared call observed the bridge")
	for i, background := range probe.seen {
		testassert.Require(t, background != nil, "prepared call %d carried no background services bridge", i)
		// Complete after publication: the production-armed instance
		// reaches the Harness (an unknown completion is not new work).
		testassert.NoError(t, background.DeliverCompletion(ctx, session.Identity.SessionID, "00000000000000000000000000000000", "ignored"))
	}

	testassert.NoError(t, r.Close(ctx))
}
