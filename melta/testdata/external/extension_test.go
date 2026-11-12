package external_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/melta"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

const (
	externalExtensionID    = "external.ext"
	externalCapabilityID   = "external.ext.api"
	externalContractID     = "external.ext.api/v1"
	externalFailureCode    = "external_declared"
	externalFailureMessage = "line one\nline two \"quoted\""

	externalFailureData = `{
  "big": 9007199254740993,
  "escaped": "line\n\t\"quote\" \\ slash"
}`

	externalFailurePartial = `["partial",
  9007199254740993,
  {"escaped":"line\n\t<>&"}]`

	externalRawValue = "{\n  \"big\": 9007199254740993,\n  \"escaped\": \"line\\n\\t\\\"quote\\\" \\\\ slash\"\n}"
)

// externalCalls observes how often the native implementation is constructed.
type externalCalls struct {
	opens atomic.Int32
}

// externalExtension is the public native extension of this module: one
// Harness-scoped service with the fixed data and command host bindings. Its
// work method is a concrete Session-target session_work method; fail and raw
// are concrete Harness-target queries.
func externalExtension(calls *externalCalls) extension.Native {
	data, _ := extension.HostCapability(extension.ContractData)
	commands, _ := extension.HostCapability(extension.ContractCommands)
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           externalExtensionID,
			Version:      "1.0.0",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"additionalProperties":false}`),
			Components: []extension.Component{{
				ID:       "external.ext.component",
				Lifetime: extension.LifetimeHarness,
				Provides: []string{externalCapabilityID},
				Requires: []extension.Binding{
					{Name: "data", Provider: extension.HostProvider, CapabilityID: extension.ContractData, Definition: data},
					{Name: "commands", Provider: extension.HostProvider, CapabilityID: extension.ContractCommands, Definition: commands},
				},
			}},
			Capabilities: []extension.Capability{extension.ServiceCapability(externalCapabilityID, externalContractID,
				extension.ServiceMethod("work", extension.ModeServiceIO,
					`{"type":"object","required":["submit"],"properties":{"submit":{"type":"object"}},"additionalProperties":false}`,
					`{"type":"object"}`, "", ""),
				extension.ServiceMethod("fail", extension.ModeServiceIO, `{}`, `{}`, `{}`, `{}`),
				extension.ServiceMethod("raw", extension.ModeServiceIO, `{}`, `{}`, "", ""),
			)},
			Exposed: []extension.ExposedMethod{
				{Method: extension.MethodKey{CapabilityID: externalCapabilityID, Method: "work"}, Kind: extension.ExposedSessionWork, Target: extension.ExposedTargetSession},
				{Method: extension.MethodKey{CapabilityID: externalCapabilityID, Method: "fail"}, Kind: extension.ExposedQuery, Target: extension.ExposedTargetHarness},
				{Method: extension.MethodKey{CapabilityID: externalCapabilityID, Method: "raw"}, Kind: extension.ExposedQuery, Target: extension.ExposedTargetHarness},
			},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			calls.opens.Add(1)
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				externalCapabilityID: {Service: map[string]extension.Handler{
					"work": externalWork,
					"fail": externalFail,
					"raw":  externalRaw,
				}},
			}}, nil
		},
	}
}

// externalWork obtains the extension's own Session and cache directories
// through the declared host data service, writes one sentinel into each, and
// submits ordinary admitted work through the fixed command bridge.
func externalWork(ctx context.Context, call extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
	var request struct {
		Submit json.RawMessage `json:"submit"`
	}
	if err := json.Unmarshal(arguments, &request); err != nil {
		return extension.Result{}, err
	}
	if len(request.Submit) == 0 {
		return extension.Result{}, errors.New("external extension: missing submit request")
	}
	sessionDirectory, err := externalNamespace(ctx, call, "session_namespace")
	if err != nil {
		return extension.Result{}, err
	}
	cacheDirectory, err := externalNamespace(ctx, call, "cache_namespace")
	if err != nil {
		return extension.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(sessionDirectory, "session.sentinel"), []byte("session"), 0o600); err != nil {
		return extension.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(cacheDirectory, "cache.sentinel"), []byte("cache"), 0o600); err != nil {
		return extension.Result{}, err
	}
	submitted, err := call.Call(ctx, "commands", "submit", request.Submit)
	if err != nil {
		return extension.Result{}, err
	}
	if submitted.Failure != nil {
		return submitted, nil
	}
	value, err := json.Marshal(struct {
		SessionDirectory string          `json:"session_directory"`
		CacheDirectory   string          `json:"cache_directory"`
		Submit           json.RawMessage `json:"submit"`
	}{SessionDirectory: sessionDirectory, CacheDirectory: cacheDirectory, Submit: submitted.Value})
	if err != nil {
		return extension.Result{}, err
	}
	return extension.Result{Value: value}, nil
}

func externalNamespace(ctx context.Context, call extension.CallContext, method string) (string, error) {
	result, err := call.Call(ctx, "data", method, json.RawMessage(`{}`))
	if err != nil {
		return "", err
	}
	if result.Failure != nil {
		return "", fmt.Errorf("external extension: data %s failed: %s", method, result.Failure.Message)
	}
	var value struct {
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(result.Value, &value); err != nil {
		return "", fmt.Errorf("external extension: data %s: %w", method, err)
	}
	if value.Directory == "" {
		return "", fmt.Errorf("external extension: data %s returned no directory", method)
	}
	return value.Directory, nil
}

func externalFail(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
	return extension.Result{Failure: &extension.Failure{
		Code:    externalFailureCode,
		Message: externalFailureMessage,
		Data:    json.RawMessage(externalFailureData),
		Partial: json.RawMessage(externalFailurePartial),
	}}, nil
}

func externalRaw(_ context.Context, _ extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
	return extension.Result{Value: append(json.RawMessage(nil), arguments...)}, nil
}

func externalExtensionConfiguration(endpoint string) []byte {
	var document map[string]any
	if err := json.Unmarshal(configurationDocument(endpoint), &document); err != nil {
		panic(err)
	}
	document["extensions"] = map[string]any{externalExtensionID: map[string]any{"enabled": true}}
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return encoded
}

func externalRequest(capabilityID, contractID, method string, target protocol.ExtensionInvocationTarget, arguments, requestID string) protocol.ExtensionInvocationRequest {
	rawArguments := json.RawMessage(arguments)
	return protocol.ExtensionInvocationRequest{
		ExtensionId:     externalExtensionID,
		CapabilityId:    capabilityID,
		ContractId:      contractID,
		ContractVersion: extension.ContractVersion,
		Method:          method,
		Target:          target,
		Arguments:       &rawArguments,
		RequestId:       requestID,
	}
}

func externalHarnessTarget() protocol.ExtensionInvocationTarget {
	return protocol.ExtensionInvocationTarget{Kind: protocol.ExtensionInvocationTargetKindHarness}
}

func externalSessionTarget(sessionID string) protocol.ExtensionInvocationTarget {
	return protocol.ExtensionInvocationTarget{Kind: protocol.ExtensionInvocationTargetKindSession, SessionId: &sessionID}
}

func externalWorkInput(t *testing.T, page protocol.HistoryPage, operationID string) protocol.InputItem {
	t.Helper()
	for _, item := range page.Items {
		input, err := item.AsInputItem()
		if err != nil {
			continue
		}
		if input.OperationId != nil && *input.OperationId == operationID {
			return input
		}
	}
	t.Fatalf("history has no input for operation %q", operationID)
	return protocol.InputItem{}
}

// TestPublicExtensionIntegration exercises the final extension surface from an
// independent module: public native construction, discovery, one concrete
// Session-target invocation with ordinary admitted work attributed to the
// extension, exact declared failure data/partial and raw success values, then
// implementation removal on a deliberate reopen with persisted history,
// refused new invocation, and Session-data cleanup that preserves the extension
// cache and configuration bytes.
func TestPublicExtensionIntegration(t *testing.T) {
	server := newScriptedModel(t)
	dataDir := t.TempDir()
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, externalExtensionConfiguration(server.URL), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	credentials := &memoryCredentials{values: map[string]melta.CredentialValue{
		externalKeyEnv: {Value: externalKey, Defined: true, Managed: true},
	}}
	inputs := melta.DefaultAgentInputs()
	inputs.Model = model.ModelRef{Provider: "prov", Model: "m"}
	inputs.Instructions = "The external extension module."
	calls := &externalCalls{}
	options := melta.Options{
		DataDir:       dataDir,
		Configuration: melta.NewFileConfigurationSource(configPath),
		Credentials:   credentials,
		Locations: melta.Locations{
			GlobalRulesDir:     filepath.Join(t.TempDir(), "rules"),
			DiscoveryCacheDir:  filepath.Join(t.TempDir(), "discovery"),
			WorkspacePolicyDir: filepath.Join(t.TempDir(), "projects"),
		},
		Identity:   "external extension application",
		Agent:      &inputs,
		Extensions: []extension.Native{externalExtension(calls)},
	}
	ctx := context.Background()
	owner := openOwner(t, options)
	defer func() { _ = owner.Close(ctx) }()
	bound := caller(t, owner)
	session, err := owner.CreateSession(ctx, bound, workspace, nil)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sessionID := session.SessionId
	sessionCaller, err := owner.Caller(melta.CallerContext{
		Origin: harness.InputOriginUser,
		Scope:  melta.CallerScope{Kind: melta.CallerScopeSession, SessionID: sessionID},
	})
	if err != nil {
		t.Fatalf("Session Caller: %v", err)
	}

	catalog, err := owner.ListExtensionCapabilities(ctx, bound)
	if err != nil {
		t.Fatalf("ListExtensionCapabilities: %v", err)
	}
	var entry *protocol.ExtensionCapabilityCatalog
	for i := range catalog {
		if catalog[i].ExtensionId == externalExtensionID {
			entry = &catalog[i]
			break
		}
	}
	if entry == nil {
		t.Fatalf("catalog omitted the enabled external extension: %+v", catalog)
	}
	if entry.Version != "1.0.0" || !strings.Contains(entry.Settings, `"enabled":true`) {
		t.Fatalf("catalog entry = version %q settings %q", entry.Version, entry.Settings)
	}
	var discoveredCapability bool
	for _, capability := range entry.Capabilities {
		if capability.CapabilityId != externalCapabilityID {
			continue
		}
		discoveredCapability = capability.ContractId == externalContractID && capability.ContractVersion == extension.ContractVersion && capability.Kind == protocol.ExtensionCapabilityKindService
	}
	if !discoveredCapability {
		t.Fatalf("catalog capabilities = %+v, want the declared service capability", entry.Capabilities)
	}
	var discoveredWork, discoveredFail, discoveredRaw bool
	for _, exposed := range entry.Exposed {
		switch exposed.Method {
		case "work":
			discoveredWork = exposed.CapabilityId == externalCapabilityID && exposed.Kind == protocol.SessionWork && exposed.Target == protocol.ExtensionExposureTargetSession
		case "fail":
			discoveredFail = exposed.Kind == protocol.Query && exposed.Target == protocol.ExtensionExposureTargetHarness
		case "raw":
			discoveredRaw = exposed.Kind == protocol.Query && exposed.Target == protocol.ExtensionExposureTargetHarness
		}
	}
	if !discoveredWork || !discoveredFail || !discoveredRaw {
		t.Fatalf("catalog exposures = %+v, want the work/fail/raw entries", entry.Exposed)
	}

	raw, err := owner.InvokeExtension(ctx, bound, externalRequest(externalCapabilityID, externalContractID, "raw", externalHarnessTarget(), externalRawValue, "req-external-raw"))
	if err != nil {
		t.Fatalf("raw invocation: %v", err)
	}
	if raw.Failure != nil || string(raw.Value) != externalRawValue {
		t.Fatalf("raw result = %+v, want the exact %q", raw, externalRawValue)
	}

	failure, err := owner.InvokeExtension(ctx, bound, externalRequest(externalCapabilityID, externalContractID, "fail", externalHarnessTarget(), `{}`, "req-external-fail"))
	if err != nil {
		t.Fatalf("declared failure invocation = %v, want a nil Go error", err)
	}
	if failure.Failure == nil || failure.Failure.Code != externalFailureCode || failure.Failure.Message != externalFailureMessage || string(failure.Failure.Data) != externalFailureData || string(failure.Failure.Partial) != externalFailurePartial || failure.Value != nil {
		t.Fatalf("declared failure = %+v, want exact code/message/data/partial", failure.Failure)
	}

	workArguments := `{"submit":{"operation_id":"op-external-extension","mode":"regular","content":[{"kind":"text","text":"external work"}]}}`
	work, err := owner.InvokeExtension(ctx, sessionCaller, externalRequest(externalCapabilityID, externalContractID, "work", externalSessionTarget(sessionID), workArguments, "req-external-work"))
	if err != nil {
		t.Fatalf("Session-target work invocation: %v", err)
	}
	if work.Failure != nil {
		t.Fatalf("Session-target work failure = %+v", work.Failure)
	}
	var workResult struct {
		SessionDirectory string `json:"session_directory"`
		CacheDirectory   string `json:"cache_directory"`
	}
	if err := json.Unmarshal(work.Value, &workResult); err != nil || workResult.SessionDirectory == "" || workResult.CacheDirectory == "" {
		t.Fatalf("work result = %s (%v), want the declared data namespaces", work.Value, err)
	}
	sessionSentinel := filepath.Join(workResult.SessionDirectory, "session.sentinel")
	cacheSentinel := filepath.Join(workResult.CacheDirectory, "cache.sentinel")
	if data, err := os.ReadFile(sessionSentinel); err != nil || string(data) != "session" {
		t.Fatalf("session sentinel = (%q, %v)", data, err)
	}
	if data, err := os.ReadFile(cacheSentinel); err != nil || string(data) != "cache" {
		t.Fatalf("cache sentinel = (%q, %v)", data, err)
	}
	awaitOperation(t, owner, bound, sessionID, "op-external-extension", protocol.OperationStatusSuccess)

	page, err := owner.GetHistory(ctx, bound, sessionID, nil)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	input := externalWorkInput(t, page, "op-external-extension")
	if input.Origin != protocol.InputOriginPlugin || input.ExtensionId == nil || *input.ExtensionId != externalExtensionID {
		t.Fatalf("admitted input attribution = origin %q extension %v, want plugin/%s", input.Origin, input.ExtensionId, externalExtensionID)
	}
	if len(input.Content) == 0 {
		t.Fatalf("admitted input carries no content: %+v", input)
	}
	requestsBeforeReopen, _ := server.requests()
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read configuration: %v", err)
	}
	if err := owner.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Remove the native implementation and reopen the same store and
	// configuration: the extension cannot replay, yet its admitted history
	// remains readable.
	options.Extensions = nil
	reopened := openOwner(t, options)
	defer func() { _ = reopened.Close(ctx) }()
	reopenedCaller := caller(t, reopened)
	catalog, err = reopened.ListExtensionCapabilities(ctx, reopenedCaller)
	if err != nil {
		t.Fatalf("reopened ListExtensionCapabilities: %v", err)
	}
	for _, candidate := range catalog {
		if candidate.ExtensionId == externalExtensionID {
			t.Fatalf("removed extension is still cataloged: %+v", candidate)
		}
	}
	if got := calls.opens.Load(); got != 1 {
		t.Fatalf("extension opened %d times after removal, want no replay", got)
	}
	_, err = reopened.InvokeExtension(ctx, reopenedCaller, externalRequest(externalCapabilityID, externalContractID, "work", externalSessionTarget(sessionID), workArguments, "req-external-removed"))
	if !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("invocation after removal = %v, want not-found", err)
	}
	requestsAfterReopen, _ := server.requests()
	if len(requestsAfterReopen) != len(requestsBeforeReopen) {
		t.Fatalf("removed extension replayed model work: %d requests, want %d", len(requestsAfterReopen), len(requestsBeforeReopen))
	}
	page, err = reopened.GetHistory(ctx, reopenedCaller, sessionID, nil)
	if err != nil {
		t.Fatalf("reopened GetHistory: %v", err)
	}
	input = externalWorkInput(t, page, "op-external-extension")
	if input.ExtensionId == nil || *input.ExtensionId != externalExtensionID {
		t.Fatalf("reopened input lost extension attribution: %+v", input)
	}
	if data, err := os.ReadFile(sessionSentinel); err != nil || string(data) != "session" {
		t.Fatalf("Session sentinel after removal = (%q, %v), want preserved", data, err)
	}

	if _, err := reopened.ArchiveSession(ctx, reopenedCaller, sessionID); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}
	deletion, err := reopened.DeleteSession(ctx, reopenedCaller, sessionID)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if len(deletion.CleanupFailures) != 0 {
		t.Fatalf("deletion cleanup failures = %+v", deletion.CleanupFailures)
	}
	if _, err := os.Stat(workResult.SessionDirectory); !os.IsNotExist(err) {
		t.Fatalf("deleted Session namespace = %v, want absent", err)
	}
	if data, err := os.ReadFile(cacheSentinel); err != nil || string(data) != "cache" {
		t.Fatalf("cache after Session deletion = (%q, %v), want preserved", data, err)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || string(configAfter) != string(configBefore) {
		t.Fatalf("configuration after Session deletion = (%q, %v), want preserved", configAfter, err)
	}
}
