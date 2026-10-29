package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/jobs"
	"github.com/MMinasyan/lightcode/internal/prompt"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// baselineSettingsConfigDocument is the baseline-settings fixture: the
// compact capture fixture plus the top-level tools, jobs and compaction
// sections the baseline services consume directly.
func baselineSettingsConfigDocument(endpoint string) string {
	return strings.TrimSuffix(compactPrepConfigDocument(endpoint), "}") +
		`,"tools":{"read_max_lines":77,"unknown_tool_member":1},` +
		`"jobs":{"max_output_bytes":1024,"unknown_jobs_member":1},` +
		`"compaction":{"model":"prov/m2","prompt":"baseline summary"}}`
}

// baselineSettingsPrep publishes the baseline-settings fixture with an
// agents document that also overlays the selectable compact builtin.
func baselineSettingsPrep(t *testing.T, agentsDoc, endpoint string) (configurationCapture, *preparation) {
	t.Helper()
	sh := newServiceHarness(t)
	t.Setenv("PREP_COMPACT_KEY", "compact-secret")
	writeServiceFile(t, sh.configPath, baselineSettingsConfigDocument(endpoint))
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

// TestBaselineSettingsDriveToolsJobsAndCompaction proves the top-level
// baseline sections reach the actual services without any optional plugin,
// while the selectable compact Agent keeps its own independent overlay.
func TestBaselineSettingsDriveToolsJobsAndCompaction(t *testing.T) {
	agentsDoc := agentsWithCompact(`{"model":"prov/m3"}`)
	captured, prep := baselineSettingsPrep(t, agentsDoc, "https://prov.test/v1")
	snapshot := captured.snapshot

	// The five builtin Agent positions remain, and the compact definition
	// keeps its own overlay.
	var compact harness.AgentType
	roster := snapshot.agentTypes()
	var builtinNames []string
	for _, agent := range roster {
		if agent.Name == "compact" {
			compact = agent
		}
	}
	for _, builtin := range []string{"primary", "secondary", "explore", "review", "compact"} {
		found := false
		for _, agent := range roster {
			if agent.Name == builtin {
				found = true
				break
			}
		}
		if found {
			builtinNames = append(builtinNames, builtin)
		}
	}
	testassert.Equal(t, strings.Join(builtinNames, ","), "primary,secondary,explore,review,compact")
	testassert.Require(t, compact.Name == "compact" && compact.Model.String() == "prov/m3" && compact.Prompt == retainedSummarizerPrompt, "compact definition = %+v, want its independent model overlay and the builtin instructions", compact)

	invocation := Invocation{snapshot: snapshot}

	// The real read tool consumes the top-level tools section.
	byID := newStandardTools(t.TempDir(), &fakeJobs{})
	readTool := byID["read_file"]
	call, err := model.NewToolCall(model.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)})
	testassert.NoError(t, err)
	normalized, err := readTool.Normalize(ToolContext{Invocation: invocation}, call)
	testassert.NoError(t, err)
	testassert.Require(t, string(normalized) == `{"limit":77,"offset":1,"path":"a.txt"}`, "read normalization = %s, want the top-level tools limit 77", normalized)

	// The real background command path forwards the top-level jobs section.
	jobsInst := &fakeJobs{}
	tools := newStandardTools(t.TempDir(), jobsInst)
	backgroundCall, err := model.NewToolCall(model.ToolCall{ID: "call-2", Name: "run_command", Arguments: json.RawMessage(`{"command":"true","background":true}`)})
	testassert.NoError(t, err)
	plan := tools["run_command"].Prepare(context.Background(), ToolContext{
		Workspace:     t.TempDir(),
		AdmittedEntry: harness.EntryRef{SessionID: testSessionID, EntryID: testEntryID},
		Invocation:    invocation,
		Background:    scriptBackground{},
		SubprocessEnv: func() []string { return []string{} },
	}, backgroundCall)
	testassert.Require(t, plan.Immediate == nil && plan.Execute != nil, "background plan = %+v, want one executor", plan)
	outcome := plan.Execute(context.Background())
	testassert.Require(t, outcome.Result.Status == model.ResultSuccess, "background start = %+v, want success", outcome.Result)
	testassert.Require(t, len(jobsInst.started) == 1, "jobs starts = %d, want one", len(jobsInst.started))
	testassert.Equal(t, compactJSON(t, jobsInst.started[0].Config), `{"max_output_bytes":1024,"unknown_jobs_member":1}`)

	// The compact capture consumes the top-level compaction section, not the
	// selectable compact Agent's overlay.
	selection := compactPrepSelection(t, captured, "solo")
	capture, opener, err := prep.concretePrepare(context.Background(), compactPrepRequest(), selection)
	testassert.NoError(t, err)
	testassert.Require(t, opener != nil, "concretePrepare returned no opener")
	testassert.Equal(t, capture.Compact.Model.String(), "prov/m2")
	testassert.Equal(t, capture.Compact.SystemPrompt, "baseline summary")
	testassert.Require(t, capture.Compact.Pricing != nil && capture.Compact.Pricing.Input != nil && *capture.Compact.Pricing.Input == 0.5, "compact pricing = %+v, want the top-level compact model's rates", capture.Compact.Pricing)
}

// TestBaselineCompactionAndCompactAgentEditIndependently proves the two
// owners stay separate across real mutations: editing the selectable compact
// Agent's model leaves the baseline compaction values unchanged, and a
// settings save replacing the baseline compaction section leaves the compact
// Agent definition unchanged.
func TestBaselineCompactionAndCompactAgentEditIndependently(t *testing.T) {
	sh := newServiceHarness(t)
	configDoc := strings.Replace(baselineSettingsConfigDocument("https://prov.test/v1"), `"prompt":"baseline summary"`, `"prompt":"baseline edit"`, 1)
	writeServiceFile(t, sh.configPath, configDoc)
	writeServiceFile(t, agents.PathForConfig(sh.configPath), prepAgentsDocument)
	owner, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	comp := mustComposition(t)
	svc := newConfigurationService(owner, comp, sh.loader, sh.configPath, newObservation())
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	snapshot := svc.current()
	testassert.Require(t, snapshot.compactionValues.model == (model.ModelRef{Provider: "prov", Model: "m2"}) && snapshot.compactionValues.prompt == "baseline edit", "published compaction = %+v, want the baseline section", snapshot.compactionValues)
	compactBefore := compactAgent(snapshot.agentTypes())
	testassert.Require(t, compactBefore.Model.IsZero(), "compact Agent model = %s, want the builtin fallback", compactBefore.Model.String())

	{
		_, err := svc.mutate(context.Background(), svc.editAgentModel("compact", "prov/m3"))
		testassert.NoError(t, err)
	}
	snapshot = svc.current()
	testassert.Require(t, compactAgent(snapshot.agentTypes()).Model == (model.ModelRef{Provider: "prov", Model: "m3"}), "compact Agent model = %s, want the saved definition override", compactAgent(snapshot.agentTypes()).Model.String())
	testassert.Require(t, snapshot.compactionValues.model == (model.ModelRef{Provider: "prov", Model: "m2"}) && snapshot.compactionValues.prompt == "baseline edit", "compaction after the Agent edit = %+v, want the baseline values unchanged", snapshot.compactionValues)

	settings := projectSettings(snapshot.sessions, snapshot.tools, snapshot.jobs, snapshot.compaction, snapshot.plugins)
	settings.Compaction = json.RawMessage(`{"model":"prov/mzero","prompt":"settings edit"}`)
	{
		_, err := svc.mutate(context.Background(), editSettings(settings))
		testassert.NoError(t, err)
	}
	snapshot = svc.current()
	testassert.Require(t, snapshot.compactionValues.model == (model.ModelRef{Provider: "prov", Model: "mzero"}) && snapshot.compactionValues.prompt == "settings edit", "compaction after the settings save = %+v, want the written baseline section", snapshot.compactionValues)
	testassert.Require(t, compactAgent(snapshot.agentTypes()).Model == (model.ModelRef{Provider: "prov", Model: "m3"}), "compact Agent model after the settings save = %s, want the saved override unchanged", compactAgent(snapshot.agentTypes()).Model.String())
}

func compactAgent(roster []harness.AgentType) harness.AgentType {
	for _, agent := range roster {
		if agent.Name == "compact" {
			return agent
		}
	}
	return harness.AgentType{}
}

// TestCompactionSettingsResetRestoresFallbackDefaults proves a settings save
// that resets the whole compaction object to {} applies the documented
// defaults at the next admission: the conversation-model fallback and the
// unchanged builtin prompt.
func TestCompactionSettingsResetRestoresFallbackDefaults(t *testing.T) {
	sh := newServiceHarness(t)
	t.Setenv("PREP_COMPACT_KEY", "compact-secret")
	writeServiceFile(t, sh.configPath, baselineSettingsConfigDocument("https://prov.test/v1"))
	writeServiceFile(t, agents.PathForConfig(sh.configPath), prepAgentsDocument)
	owner, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	comp := mustComposition(t, compactToolsPlugin())
	runtimeScope := mustOpenScope(t, comp, owner, ScopeInfo{Kind: ScopeRuntime, DataDir: sh.dataDir}, nil)
	obs := newObservation()
	ws := newWorkspaceScopes(owner, comp, []*scope{runtimeScope}, obs)
	svc := newConfigurationService(owner, comp, sh.loader, sh.configPath, obs)
	captured, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	prep := newPreparation(svc, comp, runtimeScope, ws, sh.home, nil, nil, nil, nil)

	prepare := func(t *testing.T, captured configurationCapture) harness.ExecutionCapture {
		t.Helper()
		selection := compactPrepSelection(t, captured, "solo")
		capture, opener, err := prep.concretePrepare(context.Background(), compactPrepRequest(), selection)
		testassert.NoError(t, err)
		testassert.Require(t, opener != nil, "concretePrepare returned no opener")
		return capture
	}
	before := prepare(t, captured)
	testassert.Require(t, before.Compact.Model.String() == "prov/m2" && before.Compact.SystemPrompt == "baseline summary", "custom compaction capture = %+v, want the configured model and prompt", before.Compact)

	settings := projectSettings(svc.current().sessions, svc.current().tools, svc.current().jobs, svc.current().compaction, svc.current().plugins)
	settings.Compaction = json.RawMessage(`{}`)
	{
		_, err := svc.mutate(context.Background(), editSettings(settings))
		testassert.NoError(t, err)
	}
	reset, err := svc.capture(context.Background())
	testassert.NoError(t, err)
	after := prepare(t, reset)
	testassert.Require(t, after.Compact.Model.String() == "prov/m" && after.Compact.SystemPrompt == retainedSummarizerPrompt, "reset compaction capture = %+v, want the conversation model and unchanged builtin prompt", after.Compact)
	testassert.Require(t, after.Compact.Pricing != nil && after.Compact.Pricing.Input != nil && *after.Compact.Pricing.Input == 1.5, "reset compact pricing = %+v, want the conversation model's rates", after.Compact.Pricing)
}

// newBaselineSettingsService publishes one baseline configuration over the
// compact fixture and returns its harness, service, preparation and first
// capture.
func newBaselineSettingsService(t *testing.T, configDoc string) (*serviceHarness, *configurationService, *preparation, configurationCapture) {
	t.Helper()
	sh := newServiceHarness(t)
	t.Setenv("PREP_COMPACT_KEY", "compact-secret")
	writeServiceFile(t, sh.configPath, configDoc)
	writeServiceFile(t, agents.PathForConfig(sh.configPath), prepAgentsDocument)
	owner, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	comp := mustComposition(t, compactToolsPlugin())
	runtimeScope := mustOpenScope(t, comp, owner, ScopeInfo{Kind: ScopeRuntime, DataDir: sh.dataDir}, nil)
	obs := newObservation()
	ws := newWorkspaceScopes(owner, comp, []*scope{runtimeScope}, obs)
	svc := newConfigurationService(owner, comp, sh.loader, sh.configPath, obs)
	captured, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	return sh, svc, newPreparation(svc, comp, runtimeScope, ws, sh.home, nil, nil, nil, nil), captured
}

// prepareCompactCapture runs one concrete preparation over a selection built
// from the supplied capture.
func prepareCompactCapture(t *testing.T, prep *preparation, captured configurationCapture) harness.ExecutionCapture {
	t.Helper()
	selection := compactPrepSelection(t, captured, "solo")
	capture, opener, err := prep.concretePrepare(context.Background(), compactPrepRequest(), selection)
	testassert.NoError(t, err)
	testassert.Require(t, opener != nil, "concretePrepare returned no opener")
	return capture
}

// startBackgroundForTest runs the real run_command tool's background path and
// returns the one forwarded StartRequest.
func startBackgroundForTest(t *testing.T, invocation Invocation) jobs.StartRequest {
	t.Helper()
	recorder := &fakeJobs{}
	values := newStandardTools(t.TempDir(), recorder)
	call, err := model.NewToolCall(model.ToolCall{ID: "call-bg", Name: "run_command", Arguments: json.RawMessage(`{"command":"true","background":true}`)})
	testassert.NoError(t, err)
	plan := values["run_command"].Prepare(context.Background(), ToolContext{
		Workspace:     t.TempDir(),
		AdmittedEntry: harness.EntryRef{SessionID: testSessionID, EntryID: testEntryID},
		Invocation:    invocation,
		Background:    scriptBackground{},
		SubprocessEnv: func() []string { return []string{} },
	}, call)
	testassert.Require(t, plan.Immediate == nil && plan.Execute != nil, "background plan = %+v, want one executor", plan)
	outcome := plan.Execute(context.Background())
	testassert.Require(t, outcome.Result.Status == model.ResultSuccess && len(recorder.started) == 1, "background start = %+v with %d starts, want one success", outcome.Result, len(recorder.started))
	return recorder.started[0]
}

// TestBaselineCapturesStayFrozenAcrossSettingsPublication proves a saved
// settings revision reaches only later admissions: the retained Invocation
// keeps its tools/jobs/compaction values while the fresh capture observes the
// new ones, for the real read and background-command paths and for compact
// preparation.
func TestBaselineCapturesStayFrozenAcrossSettingsPublication(t *testing.T) {
	sh, svc, prep, oldCaptured := newBaselineSettingsService(t, baselineSettingsConfigDocument("https://prov.test/v1"))
	oldInvocation := Invocation{snapshot: oldCaptured.snapshot}
	workspace := t.TempDir()
	lines := make([]string, 120)
	for i := range lines {
		lines[i] = strings.Repeat("x", 10)
	}
	testassert.NoError(t, os.WriteFile(filepath.Join(workspace, "big.txt"), []byte(strings.Join(lines, "\n")), 0o644))
	readCall, err := model.NewToolCall(model.ToolCall{ID: "call-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"big.txt"}`)})
	testassert.NoError(t, err)
	readTool := newStandardTools(sh.dataDir, &fakeJobs{})["read_file"]
	read := func(t *testing.T, invocation Invocation) string {
		t.Helper()
		normalized, err := readTool.Normalize(ToolContext{Invocation: invocation}, readCall)
		testassert.NoError(t, err)
		call, err := model.NewToolCall(model.ToolCall{ID: "call-read", Name: "read_file", Arguments: normalized})
		testassert.NoError(t, err)
		plan := readTool.Prepare(context.Background(), ToolContext{Workspace: workspace, Invocation: invocation}, call)
		testassert.Require(t, plan.Immediate == nil && plan.Execute != nil, "read plan = %+v, want one executor", plan)
		return plan.Execute(context.Background()).Result.Content
	}
	oldRead := read(t, oldInvocation)
	testassert.Require(t, strings.Contains(oldRead, "(Showing lines 1-77 of 120."), "old read = %q, want the captured 77-line window", oldRead)
	oldJob := startBackgroundForTest(t, oldInvocation)
	testassert.Equal(t, compactJSON(t, oldJob.Config), `{"max_output_bytes":1024,"unknown_jobs_member":1}`)
	oldCompact := prepareCompactCapture(t, prep, oldCaptured)
	testassert.Require(t, oldCompact.Compact.Model.String() == "prov/m2" && oldCompact.Compact.SystemPrompt == "baseline summary", "old compact = %+v, want the retained revision", oldCompact.Compact)

	settings := projectSettings(oldCaptured.snapshot.sessions, oldCaptured.snapshot.tools, oldCaptured.snapshot.jobs, oldCaptured.snapshot.compaction, oldCaptured.snapshot.plugins)
	settings.Tools = json.RawMessage(`{"read_max_lines":88}`)
	settings.Jobs = json.RawMessage(`{"max_output_bytes":2048}`)
	settings.Compaction = json.RawMessage(`{"model":"prov/m","prompt":"fresh prompt"}`)
	settings.Plugins = nil
	{
		_, err := svc.mutate(context.Background(), editSettings(settings))
		testassert.NoError(t, err)
	}
	freshCaptured, err := svc.capture(context.Background())
	testassert.NoError(t, err)
	freshInvocation := Invocation{snapshot: freshCaptured.snapshot}
	freshRead := read(t, freshInvocation)
	testassert.Require(t, strings.Contains(freshRead, "(Showing lines 1-88 of 120."), "fresh read = %q, want the new 88-line window", freshRead)
	freshJob := startBackgroundForTest(t, freshInvocation)
	testassert.Equal(t, compactJSON(t, freshJob.Config), `{"max_output_bytes":2048}`)
	freshCompact := prepareCompactCapture(t, prep, freshCaptured)
	testassert.Require(t, freshCompact.Compact.Model.String() == "prov/m" && freshCompact.Compact.SystemPrompt == "fresh prompt", "fresh compact = %+v, want the newly published revision", freshCompact.Compact)

	// The frozen capture still consumes the old values after publication.
	againOld := read(t, oldInvocation)
	testassert.Require(t, strings.Contains(againOld, "(Showing lines 1-77 of 120."), "retained read after publication = %q, want the old window", againOld)
	testassert.Equal(t, compactJSON(t, startBackgroundForTest(t, oldInvocation).Config), `{"max_output_bytes":1024,"unknown_jobs_member":1}`)
	againOldCompact := prepareCompactCapture(t, prep, oldCaptured)
	testassert.Require(t, againOldCompact.Compact.Model.String() == "prov/m2" && againOldCompact.Compact.SystemPrompt == "baseline summary", "retained compact after publication = %+v, want the old revision", againOldCompact.Compact)
}

// TestBaselinePartialObjectSettingsUseAbsentDefaults proves raw-document
// edits that omit one member leave that member's documented default at the
// consuming service: an omitted tools read limit, an omitted jobs output
// bound, and an omitted compaction prompt/model.
func TestBaselinePartialObjectSettingsUseAbsentDefaults(t *testing.T) {
	sh, svc, prep, captured := newBaselineSettingsService(t, baselineSettingsConfigDocument("https://prov.test/v1"))
	settings := projectSettings(captured.snapshot.sessions, captured.snapshot.tools, captured.snapshot.jobs, captured.snapshot.compaction, captured.snapshot.plugins)
	settings.Tools = json.RawMessage(`{"max_output_bytes":2048}`)
	settings.Jobs = json.RawMessage(`{"max_background_processes":5}`)
	settings.Compaction = json.RawMessage(`{"model":"prov/m2"}`)
	settings.Plugins = nil
	{
		_, err := svc.mutate(context.Background(), editSettings(settings))
		testassert.NoError(t, err)
	}
	fresh, err := svc.capture(context.Background())
	testassert.NoError(t, err)
	invocation := Invocation{snapshot: fresh.snapshot}

	readTool := newStandardTools(sh.dataDir, &fakeJobs{})["read_file"]
	call, err := model.NewToolCall(model.ToolCall{ID: "call-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)})
	testassert.NoError(t, err)
	normalized, err := readTool.Normalize(ToolContext{Invocation: invocation}, call)
	testassert.NoError(t, err)
	testassert.Require(t, string(normalized) == `{"limit":500,"offset":1,"path":"a.txt"}`, "omitted read_max_lines normalization = %s, want the absent-member default 500", normalized)

	// The real jobs manager consumes the forwarded object and applies its own
	// absent-member max_output_bytes default.
	manager := jobs.New(t.TempDir())
	t.Cleanup(func() { _ = manager.Close() })
	jobID, err := manager.Reserve()
	testassert.NoError(t, err)
	exited := make(chan jobs.ExitResult, 1)
	testassert.NoError(t, manager.Start(jobs.StartRequest{
		JobID: jobID, SessionID: testSessionID, Workspace: sh.dataDir, Command: "printf ok", Env: []string{},
		Config: invocation.jobsSettings(), OnExit: func(result jobs.ExitResult) { exited <- result },
	}))
	select {
	case result := <-exited:
		testassert.Equal(t, result.MaxOutputBytes, 15360)
	case <-time.After(15 * time.Second):
		t.Fatal("the omitted-max-output job never exited")
	}

	compact := prepareCompactCapture(t, prep, fresh)
	testassert.Require(t, compact.Compact.Model.String() == "prov/m2" && compact.Compact.SystemPrompt == prompt.DefaultSummarizerPrompt, "omitted compaction prompt capture = %+v, want the builtin default", compact.Compact)

	// Omit model independently while retaining a custom prompt.
	settings = projectSettings(fresh.snapshot.sessions, fresh.snapshot.tools, fresh.snapshot.jobs, fresh.snapshot.compaction, fresh.snapshot.plugins)
	settings.Compaction = json.RawMessage(`{"prompt":"custom retained"}`)
	settings.Plugins = nil
	{
		_, err := svc.mutate(context.Background(), editSettings(settings))
		testassert.NoError(t, err)
	}
	fresh, err = svc.capture(context.Background())
	testassert.NoError(t, err)
	compact = prepareCompactCapture(t, prep, fresh)
	testassert.Require(t, compact.Compact.Model.String() == "prov/m" && compact.Compact.SystemPrompt == "custom retained", "omitted compaction model capture = %+v, want the conversation fallback with the custom prompt", compact.Compact)
	testassert.Require(t, compact.Compact.Pricing != nil && compact.Compact.Pricing.Input != nil && *compact.Compact.Pricing.Input == 1.5, "omitted compaction model pricing = %+v, want the conversation model's rates", compact.Compact.Pricing)
}

// TestConcretePrepareCompactionWithoutRoster proves compaction preparation
// consumes only the captured compaction settings: the selection's Invocation
// points at an owned configuration copy whose definitions are nil, while the
// published snapshot and its backing roster remain untouched.
func TestConcretePrepareCompactionWithoutRoster(t *testing.T) {
	endpoint := "https://prov.test/v1"
	for _, tc := range []struct {
		name       string
		configDoc  string
		wantModel  string
		wantPrompt string
		wantInput  float64
	}{
		{"configured settings", baselineSettingsConfigDocument(endpoint), "prov/m2", "baseline summary", 0.5},
		{"unavailable model fallback", compactPrepConfigDocumentWith(endpoint, `{"model":"prov/absent"}`), "prov/m", prompt.DefaultSummarizerPrompt, 1.5},
		{"absent compaction", compactPrepConfigDocument(endpoint), "prov/m", prompt.DefaultSummarizerPrompt, 1.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, prep, captured := newBaselineSettingsService(t, tc.configDoc)
			publishedRoster := len(captured.snapshot.agentTypes())
			testassert.Require(t, publishedRoster != 0, "published roster is empty before the owned copy")

			owned := *captured.snapshot
			owned.definitions = nil
			testassert.Equal(t, len(owned.agentTypes()), 0)
			selection := compactPrepSelection(t, captured, "solo")
			selection.invocation = Invocation{snapshot: &owned}
			capture, opener, err := prep.concretePrepare(context.Background(), compactPrepRequest(), selection)
			testassert.NoError(t, err)
			testassert.Require(t, opener != nil, "concretePrepare returned no opener")
			testassert.Require(t, capture.Compact.Model.String() == tc.wantModel && capture.Compact.SystemPrompt == tc.wantPrompt, "rosterless compact capture = %+v, want %s with %q", capture.Compact, tc.wantModel, tc.wantPrompt)
			testassert.Require(t, capture.Compact.Pricing != nil && capture.Compact.Pricing.Input != nil && *capture.Compact.Pricing.Input == tc.wantInput, "rosterless compact pricing = %+v, want input %v", capture.Compact.Pricing, tc.wantInput)
			testassert.Equal(t, len(captured.snapshot.agentTypes()), publishedRoster)
		})
	}
}
