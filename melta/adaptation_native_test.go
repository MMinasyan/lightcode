package melta

// The shipped Adaptation extension's final configuration and preparation
// surface through the real owner: configured entries precede bundled rows,
// the ordered selected capabilities decide hook order for both resolved-input
// and named-definition admission, and a reload's prior capture keeps its
// entries while the next capture sees the new ones.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extensions/adaptation"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

const adaptationRuntimeAgents = `{
  "ordered": {"model": "prov/adaptive", "system_prompt": "full", "tools": ["write_file", "apply_patch"], "capabilities": ["hook.first", "model_adaptation"]},
  "reversed": {"model": "prov/adaptive", "system_prompt": "full", "tools": ["write_file", "apply_patch"], "capabilities": ["model_adaptation", "hook.first"]},
  "unselected": {"model": "prov/adaptive", "system_prompt": "full", "tools": ["read_file"], "capabilities": []},
  "modefull": {"model": "prov/adaptive", "system_prompt": "full", "tools": ["read_file"], "capabilities": ["model_adaptation"]},
  "modesimple": {"model": "prov/adaptive", "system_prompt": "simple", "tools": ["read_file"], "capabilities": ["model_adaptation"]},
  "modenone": {"model": "prov/adaptive", "system_prompt": "none", "tools": ["read_file"], "capabilities": ["model_adaptation"]},
  "bundledfull": {"model": "prov/gpt-5.5", "system_prompt": "full", "tools": ["read_file", "write_file", "edit_file", "apply_patch"], "capabilities": ["model_adaptation"]},
  "bundlednone": {"model": "prov/gpt-5.5", "system_prompt": "none", "tools": ["read_file", "write_file", "edit_file", "apply_patch"], "capabilities": ["model_adaptation"]},
  "visible": {"model": "prov/adaptive", "system_prompt": "full", "tools": ["read_file", "write_file", "apply_patch"], "capabilities": ["hook.first", "model_adaptation"]},
  "readonly": {"model": "prov/adaptive", "system_prompt": "full", "tools": ["read_file", "write_file"], "readonly": true, "capabilities": ["model_adaptation"]},
  "reload": {"model": "prov/adaptive", "system_prompt": "full", "tools": ["read_file"], "capabilities": ["park.second", "model_adaptation"]}
}`

const adaptationRuntimeConfig = `{"providers":{"prov":` + `{"transport":{"base_url":"` + "%s" + `","api_key_env":""},"discovery":false,"models":{"adaptive":{"name":"A","context_window":262144},"gpt-5.5":{"name":"G","context_window":262144}}}` + `},"extensions":{"adaptation":{"enabled":true,"entries":` + "%s" + `}}}`

func adaptationConfigDocument(endpoint, entries string) string {
	return strings.Replace(strings.Replace(adaptationRuntimeConfig, "%s", endpoint, 1), "%s", entries, 1)
}

func writeAdaptationSection(t *testing.T, configPath, section string) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	testassert.NoError(t, err)
	var root map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(data, &root))
	var extensions map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(root["extensions"], &extensions))
	extensions["adaptation"] = json.RawMessage(section)
	root["extensions"], err = json.Marshal(extensions)
	testassert.NoError(t, err)
	data, err = json.Marshal(root)
	testassert.NoError(t, err)
	writeServiceFile(t, configPath, string(data))
}

func writeAdaptationEntries(t *testing.T, configPath, entries string) {
	t.Helper()
	writeAdaptationSection(t, configPath, `{"enabled":true,"entries":`+entries+`}`)
}

// newAdaptationRuntime opens the real owner over the fixture's catalog,
// agents and configured entries, with the shipped adaptation native beside
// the suite's prompt hook and any extra plugin.
func newAdaptationRuntime(t *testing.T, entries string, extra ...extension.Native) (*productionEnv, *Melta) {
	t.Helper()
	e := newProductionEnv(t, storage.NewMemory())
	return e, openAdaptationRuntime(t, e, entries, e.hookedHook(), extra...)
}

// openAdaptationRuntime opens one fixture environment's catalog, agents and
// configured entries with a caller-supplied preparation contribution placed
// before the shipped adaptation native.
func openAdaptationRuntime(t *testing.T, e *productionEnv, entries string, hook *recordHook, extra ...extension.Native) *Melta {
	t.Helper()
	writeServiceFile(t, e.configPath, adaptationConfigDocument(e.server.URL, entries))
	writeAgentDefinitions(t, e.configPath, adaptationRuntimeAgents)
	e.server.setScript(func(context.Context, string) []string { return textTurnEvents("done") })
	plugins := []extension.Native{hookPlugin(hook)}
	plugins = append(plugins, extra...)
	plugins = append(plugins, adaptation.Native())
	r, err := e.openPlugins(context.Background(), plugins...)
	testassert.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func adaptationWorkspace(t *testing.T, e *productionEnv, name string) string {
	t.Helper()
	workspace := e.workspace(name)
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	return workspace
}

func namedAdaptationOperation(t *testing.T, r *Melta, workspace, agent, operation string) harness.OperationRecord {
	t.Helper()
	session, err := r.createSession(context.Background(), workspace, agent)
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, operation, "hello")
	return awaitOperation(t, r, session.Identity.SessionID, operation, harness.OperationSuccess)
}

func suppliedAdaptationOperation(t *testing.T, r *Melta, workspace string, inputs harness.AgentInputs, operation string) harness.OperationRecord {
	t.Helper()
	var session harness.SessionRecord
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var createErr error
		session, createErr = r.createSessionRecord(ctx, h, workspace, harness.AgentSelection{Inputs: &inputs})
		return createErr
	})
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, session.Identity.SessionID, operation, "hello")
	return awaitOperation(t, r, session.Identity.SessionID, operation, harness.OperationSuccess)
}

func catalogTool(t *testing.T, record harness.OperationRecord, name string) model.ToolDefinition {
	t.Helper()
	for _, definition := range record.Admission.Execution.Tools {
		if definition.Name == name {
			return definition
		}
	}
	t.Fatalf("captured catalog has no tool %q: %+v", name, record.Admission.Execution.Tools)
	return model.ToolDefinition{}
}

// assertAdaptationWire proves one recorded model request's effective system
// prompt, advertisement and descriptions equal the admitted capture and the
// explicit expected values.
func assertAdaptationWire(t *testing.T, doc wireChatBody, record harness.OperationRecord, wantPrompt string, wantTools []string, wantDescription string) {
	t.Helper()
	capture := record.Admission.Execution
	var system string
	found := false
	for _, message := range doc.Messages {
		if message.Role == "system" {
			testassert.NoError(t, json.Unmarshal(message.Content, &system))
			found = true
			break
		}
	}
	testassert.Require(t, found, "wire request has no system message: %+v", doc.Messages)
	testassert.Equal(t, system, wantPrompt)
	testassert.Equal(t, system, capture.SystemPrompt)
	testassert.Require(t, slices.Equal(wireToolNames(doc), wantTools), "wire tools = %v, want %v", wireToolNames(doc), wantTools)
	testassert.Require(t, slices.Equal(wireToolNames(doc), capture.InitialToolNames), "wire tools = %v, want the captured advertisement %v", wireToolNames(doc), capture.InitialToolNames)
	descriptions := make(map[string]string, len(capture.Tools))
	for _, definition := range capture.Tools {
		descriptions[definition.Name] = definition.Description
	}
	testassert.Require(t, len(doc.Tools) == len(wantTools), "wire carries %d tools, want %d", len(doc.Tools), len(wantTools))
	for _, tool := range doc.Tools {
		want, ok := descriptions[tool.Function.Name]
		testassert.Require(t, ok, "wire advertised tool %q outside the captured catalog", tool.Function.Name)
		testassert.Equal(t, tool.Function.Description, want)
		if wantDescription != "" {
			testassert.Require(t, strings.Contains(tool.Function.Description, wantDescription), "wire tool %q description = %q, want %q", tool.Function.Name, tool.Function.Description, wantDescription)
		}
	}
}

// TestAdaptationNativeToolSelectionFollowsPrepareVisibility proves a matched
// rule selects from the tools the preceding preparation hook left visible,
// never from the complete eligible catalog the rule also receives.
func TestAdaptationNativeToolSelectionFollowsPrepareVisibility(t *testing.T) {
	hider := func() *recordHook {
		return &recordHook{name: "hook.first", events: &traceLog{}, mutate: func(in extension.PreparationInput) extension.PreparationConsequence {
			visible := make([]string, 0, len(in.Tools))
			for _, definition := range in.Tools {
				if definition.Name != "write_file" {
					visible = append(visible, definition.Name)
				}
			}
			return extension.PreparationConsequence{ToolNames: visible}
		}}
	}
	for _, row := range []struct {
		name    string
		entries string
		want    []string
	}{
		{"prompt only", `[{"pattern":"*adaptive*","prompt":"ADAPTED"}]`, []string{"read_file", "apply_patch"}},
		{"exclusion", `[{"pattern":"*adaptive*","exclude_tools":["apply_patch"]}]`, []string{"read_file"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := newProductionEnv(t, storage.NewMemory())
			r := openAdaptationRuntime(t, e, row.entries, hider(), parkPlugin(&parkingHook{}))
			workspace := adaptationWorkspace(t, e, "adapt-visible")
			record := namedAdaptationOperation(t, r, workspace, "visible", "op-visible")
			testassert.Require(t, slices.Equal(record.Admission.Execution.InitialToolNames, row.want), "advertisement = %v, want %v", record.Admission.Execution.InitialToolNames, row.want)
		})
	}
}

func TestAdaptationNativeConfiguredEntriesThroughRuntime(t *testing.T) {
	const entries = `[{"pattern":"*adaptive*","prompt":"CONFIGURED","append":["TAIL"],"exclude_tools":["write_file"],"tool_description_replacements":{"edit_file":"apply_patch","apply_patch":"PATCH"}}]`
	e, r := newAdaptationRuntime(t, entries, parkPlugin(&parkingHook{}))
	ctx := context.Background()
	workspace := adaptationWorkspace(t, e, "adapt")

	// Capability order decides: the hook before adaptation is overwritten,
	// the hook after it sees the configured prompt and appends its own marker.
	before := e.server.requests()
	ordered := namedAdaptationOperation(t, r, workspace, "ordered", "op-ordered")
	assertAdaptationWire(t, decodeWireChatBody(t, e.server.bodyAt(before)), ordered, "CONFIGURED\n\nTAIL", []string{"apply_patch"}, "using the PATCH (V4A) patch format")
	testassert.Equal(t, ordered.Admission.Execution.SystemPrompt, "CONFIGURED\n\nTAIL")
	testassert.Require(t, slices.Equal(ordered.Admission.Execution.InitialToolNames, []string{"apply_patch"}), "ordered advertisement = %v, want the configured exclusion", ordered.Admission.Execution.InitialToolNames)
	{
		writeDescription := catalogTool(t, ordered, "write_file").Description
		testassert.Require(t, strings.Contains(writeDescription, "use apply_patch instead.") && !strings.Contains(writeDescription, "use PATCH instead."), "write_file description = %q, want one-pass edit_file→apply_patch", writeDescription)
		patchDescription := catalogTool(t, ordered, "apply_patch").Description
		testassert.Require(t, strings.Contains(patchDescription, "using the PATCH (V4A) patch format"), "apply_patch description = %q, want the original apply_patch key mapped to PATCH", patchDescription)
	}

	before = e.server.requests()
	reversed := namedAdaptationOperation(t, r, workspace, "reversed", "op-reversed")
	assertAdaptationWire(t, decodeWireChatBody(t, e.server.bodyAt(before)), reversed, "CONFIGURED\n\nTAIL|hooked", []string{"apply_patch"}, "using the PATCH (V4A) patch format")
	testassert.Equal(t, reversed.Admission.Execution.SystemPrompt, "CONFIGURED\n\nTAIL|hooked")

	// The same configured entries and capability order produce the same
	// transformation from supplied resolved inputs: no Agents extension is
	// involved, and the single resolved-capability-order rule is shared.
	before = e.server.requests()
	suppliedOrdered := suppliedAdaptationOperation(t, r, workspace, harness.AgentInputs{
		Model:        model.ModelRef{Provider: "prov", Model: "adaptive"},
		AssemblyMode: "full",
		Tools:        []string{"write_file", "apply_patch"},
		Capabilities: []string{"hook.first", "model_adaptation"},
	}, "op-supplied-ordered")
	assertAdaptationWire(t, decodeWireChatBody(t, e.server.bodyAt(before)), suppliedOrdered, "CONFIGURED\n\nTAIL", []string{"apply_patch"}, "using the PATCH (V4A) patch format")
	testassert.Equal(t, suppliedOrdered.Admission.Execution.SystemPrompt, "CONFIGURED\n\nTAIL")
	testassert.Require(t, slices.Equal(suppliedOrdered.Admission.Execution.InitialToolNames, []string{"apply_patch"}), "supplied advertisement = %v, want the configured exclusion", suppliedOrdered.Admission.Execution.InitialToolNames)
	before = e.server.requests()
	suppliedReversed := suppliedAdaptationOperation(t, r, workspace, harness.AgentInputs{
		Model:        model.ModelRef{Provider: "prov", Model: "adaptive"},
		AssemblyMode: "full",
		Tools:        []string{"write_file", "apply_patch"},
		Capabilities: []string{"model_adaptation", "hook.first"},
	}, "op-supplied-reversed")
	assertAdaptationWire(t, decodeWireChatBody(t, e.server.bodyAt(before)), suppliedReversed, "CONFIGURED\n\nTAIL|hooked", []string{"apply_patch"}, "using the PATCH (V4A) patch format")
	testassert.Equal(t, suppliedReversed.Admission.Execution.SystemPrompt, "CONFIGURED\n\nTAIL|hooked")

	// An enabled but unselected contribution has no effect on the assembled
	// surface.
	unselected := namedAdaptationOperation(t, r, workspace, "unselected", "op-unselected")
	testassert.Require(t, !strings.Contains(unselected.Admission.Execution.SystemPrompt, "CONFIGURED") && !strings.Contains(unselected.Admission.Execution.SystemPrompt, "TAIL") && slices.Equal(unselected.Admission.Execution.InitialToolNames, []string{"read_file"}), "unselected agent = prompt %.80q tools %v, want the baseline surface", unselected.Admission.Execution.SystemPrompt, unselected.Admission.Execution.InitialToolNames)

	// A configured prompt and its explicit append apply in every assembly
	// mode; equal text takes the same later transformation.
	for _, row := range []struct{ agent, operation string }{
		{"modefull", "op-mode-full"},
		{"modesimple", "op-mode-simple"},
		{"modenone", "op-mode-none"},
	} {
		got := namedAdaptationOperation(t, r, workspace, row.agent, row.operation)
		testassert.Equal(t, got.Admission.Execution.SystemPrompt, "CONFIGURED\n\nTAIL")
	}

	// Bundled guidance stays a full-mode append only.
	full := namedAdaptationOperation(t, r, workspace, "bundledfull", "op-bundled-full")
	none := namedAdaptationOperation(t, r, workspace, "bundlednone", "op-bundled-none")
	testassert.Require(t, strings.Contains(full.Admission.Execution.SystemPrompt, "Use more tool calls when they can improve the response"), "bundled full prompt = %.120q, want the guidance append", full.Admission.Execution.SystemPrompt)
	testassert.Require(t, !strings.Contains(none.Admission.Execution.SystemPrompt, "Use more tool calls when they can improve the response"), "bundled none prompt = %.120q, want no automatic guidance", none.Admission.Execution.SystemPrompt)
	testassert.Require(t, slices.Equal(full.Admission.Execution.InitialToolNames, []string{"read_file", "apply_patch"}) && slices.Equal(none.Admission.Execution.InitialToolNames, full.Admission.Execution.InitialToolNames), "bundled advertisements = %v / %v, want the bundled exclusions in both modes", full.Admission.Execution.InitialToolNames, none.Admission.Execution.InitialToolNames)

	// Constraint-ineligible tools stay unavailable: the configured catch-all
	// cannot recover the readonly-excluded write tool.
	readonly := namedAdaptationOperation(t, r, workspace, "readonly", "op-readonly")
	testassert.Require(t, slices.Equal(readonly.Admission.Execution.InitialToolNames, []string{"read_file"}) && slices.Equal(toolNames(readonly.Admission.Execution.Tools), []string{"read_file"}), "readonly adapter = tools %v catalog %v, want the constraint-eligible catalog only", readonly.Admission.Execution.InitialToolNames, readonly.Admission.Execution.Tools)
	testassert.NoError(t, r.Close(ctx))
}

func TestAdaptationNativeSelectionRefusalAndReloadCapture(t *testing.T) {
	const entries = `[{"pattern":"*adaptive*","prompt":"FIRST"}]`
	park := &parkingHook{}
	e, r := newAdaptationRuntime(t, entries, parkPlugin(park))
	ctx := context.Background()
	workspace := adaptationWorkspace(t, e, "adapt-reload")

	// A duplicate selected capability is refused at Session creation; an
	// undeclared one is refused at admission before any Operation exists.
	supplied := func(capabilities []string) harness.AgentInputs {
		return harness.AgentInputs{
			Model:        model.ModelRef{Provider: "prov", Model: "adaptive"},
			AssemblyMode: "full",
			Tools:        []string{"read_file"},
			Capabilities: capabilities,
		}
	}
	{
		inputs := supplied([]string{"model_adaptation", "model_adaptation"})
		err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, createErr := r.createSessionRecord(ctx, h, workspace, harness.AgentSelection{Inputs: &inputs})
			return createErr
		})
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "duplicate capability create error = %v, want harness.ErrInvalid", err)
	}
	{
		inputs := supplied([]string{"model_adaptation", "missing.capability"})
		session := suppliedSession(t, r, workspace, inputs)
		_, err := submitRawThroughRuntime(ctx, r, session.Identity.SessionID, "op-missing", "hello", harness.MessageModeRegular)
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "unavailable capability submit error = %v, want harness.ErrInvalid", err)
		var rec harness.OperationRecord
		readErr := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			var readErr error
			rec, readErr = h.ReadOperation(ctx, session.Identity.SessionID, "op-missing")
			return readErr
		})
		testassert.Require(t, errors.Is(readErr, harness.ErrNotFound) && rec.State.Status == "", "unavailable capability left an Operation: %+v (%v)", rec, readErr)
	}

	// One in-flight preparation keeps revision 1's configured prompt while
	// the reload publishes revision 2 for the next capture.
	session, err := r.createSession(ctx, workspace, "reload")
	testassert.NoError(t, err)
	release, arrived := make(chan struct{}), make(chan struct{}, 1)
	park.arm(release, arrived)
	done := make(chan error, 1)
	go func() {
		_, submitErr := submitRawThroughRuntime(ctx, r, session.Identity.SessionID, "op-first", "hello", harness.MessageModeRegular)
		done <- submitErr
	}()
	<-arrived
	writeAdaptationEntries(t, e.configPath, `[{"pattern":"*adaptive*","prompt":"SECOND"}]`)
	_, reloadErr := r.Reload(ctx, testCaller())
	testassert.NoError(t, reloadErr)
	close(release)
	testassert.NoError(t, <-done)
	first := awaitOperation(t, r, session.Identity.SessionID, "op-first", harness.OperationSuccess)
	testassert.Equal(t, first.Admission.Execution.SystemPrompt, "FIRST")

	submitThroughRuntime(t, r, session.Identity.SessionID, "op-second", "hello")
	second := awaitOperation(t, r, session.Identity.SessionID, "op-second", harness.OperationSuccess)
	testassert.Equal(t, second.Admission.Execution.SystemPrompt, "SECOND")
	testassert.Require(t, first.Admission.Execution.ConfigurationRevision != second.Admission.Execution.ConfigurationRevision, "revision = %q vs %q, want the reload's revision to reach only the next capture", first.Admission.Execution.ConfigurationRevision, second.Admission.Execution.ConfigurationRevision)
	testassert.NoError(t, r.Close(ctx))
}

func suppliedSession(t *testing.T, r *Melta, workspace string, inputs harness.AgentInputs) harness.SessionRecord {
	t.Helper()
	var session harness.SessionRecord
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var createErr error
		session, createErr = r.createSessionRecord(ctx, h, workspace, harness.AgentSelection{Inputs: &inputs})
		return createErr
	})
	testassert.NoError(t, err)
	return session
}

func TestAdaptationNativeConfigurationPublication(t *testing.T) {
	ctx := context.Background()
	invalid := `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}},"extensions":{"adaptation":{"enabled":true,"entries":[{"pattern":1}]}}}`
	e := newProductionEnv(t, storage.NewMemory())
	writeServiceFile(t, e.configPath, invalid)
	writeAgentDefinitions(t, e.configPath, `{}`)
	_, err := e.openPlugins(ctx, adaptation.Native())
	testassert.Require(t, errors.Is(err, ErrConfiguration), "active invalid adaptation section open error = %v, want ErrConfiguration", err)

	// The same invalid bytes are uninterpreted while the section is inactive,
	// including across a reload.
	inactive := strings.Replace(invalid, `{"enabled":true,"entries":[{"pattern":1}]}`, `{"enabled":false,"entries":[{"pattern":1}]}`, 1)
	writeServiceFile(t, e.configPath, inactive)
	r, err := e.openPlugins(ctx, adaptation.Native())
	testassert.NoError(t, err)
	writeAdaptationSection(t, e.configPath, `{"enabled":false,"entries":[{"pattern":2}]}`)
	_, err = r.Reload(ctx, testCaller())
	testassert.NoError(t, err)
	data, err := os.ReadFile(e.configPath)
	testassert.Require(t, err == nil && strings.Contains(string(data), `"enabled":false,"entries":[{"pattern":2}]`), "inactive reload changed or dropped the retained section: %s (%v)", data, err)
	testassert.NoError(t, r.Close(ctx))

	// An enabled section of an uninstalled extension is uninterpreted data.
	writeAdaptationSection(t, e.configPath, `{"enabled":true,"entries":[{"pattern":1}]}`)
	uninstalled, err := e.openPlugins(ctx)
	testassert.NoError(t, err)
	testassert.NoError(t, uninstalled.Close(ctx))

	// Melta publication accepts the exact lowercase members beside a
	// differently cased sibling; the hook consumes only the lowercase values.
	cased := newProductionEnv(t, storage.NewMemory())
	writeServiceFile(t, cased.configPath, `{"providers":{"prov":`+nominalProvMember(cased.server.URL, "", `{"adaptive":{"name":"A","context_window":262144}}`)+`},"extensions":{"adaptation":{"enabled":true,"entries":[{"pattern":"*adaptive*","prompt":"LOWER","append":["TAIL"]}],"Entries":[{"pattern":"*adaptive*","prompt":"UPPER","append":["UPPER_TAIL"]}],"Unrelated":true}}}`)
	writeAgentDefinitions(t, cased.configPath, `{"modefull": {"model": "prov/adaptive", "system_prompt": "full", "tools": ["read_file"], "capabilities": ["model_adaptation"]}}`)
	cased.server.setScript(func(context.Context, string) []string { return textTurnEvents("done") })
	runtimeCased, err := cased.openPlugins(ctx, adaptation.Native())
	testassert.NoError(t, err)
	casedWorkspace := adaptationWorkspace(t, cased, "adapt-cased")
	casedRecord := namedAdaptationOperation(t, runtimeCased, casedWorkspace, "modefull", "op-cased")
	testassert.Equal(t, casedRecord.Admission.Execution.SystemPrompt, "LOWER\n\nTAIL")
	assertAdaptationWire(t, decodeWireChatBody(t, cased.server.bodyAt(0)), casedRecord, "LOWER\n\nTAIL", []string{"read_file"}, "")
	testassert.NoError(t, runtimeCased.Close(ctx))

	// The settings writer preserves the configured section and rejects an
	// active document whose consumed members have the wrong shape without
	// changing the owning file.
	validEntries := `[{"pattern":"*","unknown":true}]`
	e2, r2 := newAdaptationRuntime(t, validEntries, parkPlugin(&parkingHook{}))
	view, err := r2.GetConfiguration(ctx, testCaller())
	testassert.NoError(t, err)
	settings := view.Settings
	_, err = r2.UpdateSettings(ctx, testCaller(), protocol.UpdateSettingsRequest{Settings: &settings})
	testassert.NoError(t, err)
	written, err := r2.GetConfiguration(ctx, testCaller())
	testassert.NoError(t, err)
	beforeGeneration := written.ConfigurationRevision.Generation
	data, err = os.ReadFile(e2.configPath)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(string(data), `"entries":[{"pattern":"*","unknown":true}]`), "settings write lost the configured entries: %s", data)

	before, err := os.ReadFile(e2.configPath)
	testassert.NoError(t, err)
	settings = written.Settings
	settings.Extensions["adaptation"] = `{"enabled":true,"entries":[{"pattern":"*","prompt":1}]}`
	_, err = r2.UpdateSettings(ctx, testCaller(), protocol.UpdateSettingsRequest{Settings: &settings})
	testassert.Require(t, err != nil, "settings write accepted an invalid active adaptation section")
	after, err := os.ReadFile(e2.configPath)
	testassert.Require(t, err == nil && string(before) == string(after), "a rejected settings write changed the owning file")
	fresh, err := r2.GetConfiguration(ctx, testCaller())
	testassert.NoError(t, err)
	testassert.Equal(t, fresh.ConfigurationRevision.Generation, beforeGeneration)
	testassert.NoError(t, r2.Close(ctx))
}
