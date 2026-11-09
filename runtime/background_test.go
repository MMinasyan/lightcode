package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// backgroundProbe records the native call view of every prepared call.
type backgroundProbe struct {
	seen []extension.CallContext
}

func (p *backgroundProbe) record(tc extension.ToolContext) {
	p.seen = append(p.seen, tc.Calls)
}

type backgroundProbeTool struct {
	probe *backgroundProbe
}

func (t backgroundProbeTool) describe(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
	return extension.ToolDescription{Definition: model.ToolDefinition{Name: "bg_probe", Description: "records the prepared call view", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}, Available: true}, nil
}

func (t backgroundProbeTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t backgroundProbeTool) Prepare(_ context.Context, tc extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	t.probe.record(tc)
	return extension.ToolPlan{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "probed"}}}
}

func (t backgroundProbeTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "probed"}}
}

func backgroundProbePlugin(probe *backgroundProbe) extension.Native {
	tool := backgroundProbeTool{probe: probe}
	native := testNative("bgprobe", extension.LifetimeHarness, []extension.Capability{extension.ToolCapability("bg_probe")}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"bg_probe": {Tool: tool}}}, nil
	})
	native.Describe["bg_probe"] = tool.describe
	return native
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
	writeAgentDefinitions(t, configPath,
		`{"prober": {"model": "prov/m", "system_prompt": "simple", "tools": ["bg_probe"]}}`)
	enableFixturePlugin(t, configPath, "bgprobe")
	workspace := filepath.Join(home, "probe-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))

	probe := &backgroundProbe{}
	useFixtureStore(t, store)
	r, err := Open(ctx, Options{DataDir: dataDir, ConfigPath: configPath, Plugins: []extension.Native{
		backgroundProbePlugin(probe), fixtureAgentPlugin(backgroundProbePlugin(probe)),
	}})
	testassert.NoError(t, err)
	// The unarmed-to-armed window ends before publication: construction
	// and publication observe nothing.
	testassert.Equal(t, len(probe.seen), 0)

	session, err := r.createSession(ctx, workspace, "prober")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, "op-1", "probe")
	awaitOperation(t, r, session.Identity.SessionID, "op-1", harness.OperationSuccess)

	testassert.Require(t, len(probe.seen) != 0, "no prepared call observed the background services object")
	for i, call := range probe.seen {
		testassert.Require(t, call.Call != nil, "prepared call %d carried no call binding", i)
	}

	testassert.NoError(t, r.Close(ctx))
}

func TestNativeToolPlansDoNotExecuteBeforeAuthorization(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		stopper := newLifecycleStopper()
		bg := openBackgroundRuntime(t, store, stopper)
		defer closeProjectionRuntime(bg.r)
		session := bg.session("plan-gate")
		bg.e.server.setScript(func(_ context.Context, body string) []string {
			if lastMessageRole(body) == "tool" {
				return nil
			}
			calls := []string{`{"index":0,"id":"call-invalid","type":"function","function":{"name":"read","arguments":"{}"}}`,
				`{"index":1,"id":"call-immediate","type":"function","function":{"name":"read","arguments":"{}"}}`,
				`{"index":2,"id":"call-valid","type":"function","function":{"name":"read","arguments":"{}"}}`}
			return []string{
				`{"choices":[{"delta":{"role":"assistant","tool_calls":[` + strings.Join(calls, ",") + `]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			}
		})
		var executes atomic.Int64
		bg.tools.setPrepare(func(_ context.Context, call model.ToolCall) extension.ToolPlan {
			permissions := []harness.PermissionRequest{{Permission: "command.run", Target: "test"}}
			switch call.ID {
			case "call-invalid":
				return extension.ToolPlan{}
			case "call-immediate":
				return extension.ToolPlan{Permissions: permissions, Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "immediate"}}}
			default:
				return extension.ToolPlan{Permissions: permissions}
			}
		})
		bg.tools.setExecute(func(context.Context, extension.ToolPlan) harness.ToolOutcome {
			executes.Add(1)
			return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "valid"}}
		})
		submitThroughRuntime(t, bg.r, session, "op-gate", "run")
		awaitOperation(t, bg.r, session, "op-gate", harness.OperationSuccess)
		testassert.Equal(t, executes.Load(), int64(1))
	})
}
