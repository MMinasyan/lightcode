package melta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// credentialHostNative declares the fixed credentials host binding.
func credentialHostNative(t *testing.T, id string, secrets []extension.SecretRef, lifetime extension.Lifetime, capability extension.Capability, open func(context.Context, extension.InstanceContext) (extension.Instance, error)) extension.Native {
	t.Helper()
	host, ok := extension.HostCapability(extension.ContractCredentials)
	testassert.Require(t, ok, "the credentials host contract is not defined")
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           id,
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"activation":{"type":"string"},"operation":{"type":"string"}},"required":["enabled"]}`),
			Secrets:      secrets,
			Components: []extension.Component{{
				ID:       id + "-c",
				Lifetime: lifetime,
				Provides: []string{capability.ID},
				Requires: []extension.Binding{{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: host}},
			}},
			Capabilities: []extension.Capability{capability},
		},
		Open: open,
	}
}

func credentialValue(t *testing.T, result extension.Result, err error) string {
	t.Helper()
	testassert.NoError(t, err)
	var value struct {
		Value string `json:"value"`
	}
	testassert.NoError(t, json.Unmarshal(result.Value, &value))
	return value.Value
}

func TestNativeCredentialsResolveActivationAndOperationCaptures(t *testing.T) {
	const (
		activationName = "LIGHTCODE_CRED_ACTIVATION"
		operationName  = "LIGHTCODE_CRED_OPERATION"
	)
	t.Setenv(activationName, "activation-value")
	t.Setenv(operationName, "operation-value")
	capability := extension.ServiceCapability("cred.probe", "test.cred.probe", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))

	var openActivation, openOperationErr, openMissingErr, openUndeclaredErr string
	native := credentialHostNative(t, "cred", []extension.SecretRef{
		{Field: "/activation", Lifetime: extension.SecretActivation},
		{Field: "/operation", Lifetime: extension.SecretOperation},
		{Field: "/missing", Lifetime: extension.SecretOperation},
	}, extension.LifetimeHarness, capability, func(ctx context.Context, info extension.InstanceContext) (extension.Instance, error) {
		result, err := info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/activation"}`))
		openActivation = credentialValue(t, result, err)
		_, err = info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
		testassert.Require(t, err != nil, "an operation credential resolved during a Harness Open")
		openOperationErr = err.Error()
		_, err = info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/missing"}`))
		testassert.Require(t, err != nil, "an unresolved operation field resolved")
		openMissingErr = err.Error()
		_, err = info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/undeclared"}`))
		testassert.Require(t, err != nil, "an undeclared credential field resolved")
		openUndeclaredErr = err.Error()
		return extension.Instance{Capabilities: map[string]extension.Implementation{"cred.probe": {Service: map[string]extension.Handler{"call": func(ctx context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
			return call.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
		}}}}}, nil
	})

	c := mustComposition(t, native)
	sections := map[string]json.RawMessage{
		"cred": json.RawMessage(`{"enabled":true,"activation":"` + activationName + `","operation":"` + operationName + `"}`),
	}
	declaredBindings := map[declaredField]declaredBinding{
		{extensionID: "cred", field: "/activation"}: {name: activationName, value: "activation-value"},
		{extensionID: "cred", field: "/operation"}:  {name: operationName, value: "operation-value"},
	}
	rt := mustOpenScope(t, c, context.Background(), ScopeInfo{Kind: ScopeRuntime, DataDir: "/d", sections: sections, declaredBindings: declaredBindings}, nil)
	defer func() { testassert.NoError(t, rt.close()) }()
	op := mustOpenScope(t, c, context.Background(), ScopeInfo{Kind: ScopeOperation, DataDir: "/d", Workspace: "/w", SessionID: testSessionID, OperationID: "op", sections: sections, declaredBindings: declaredBindings}, []*scope{rt})
	defer func() { testassert.NoError(t, op.close()) }()

	testassert.Equal(t, openActivation, "activation-value")
	testassert.Require(t, strings.Contains(openOperationErr, `credential "/operation" is unavailable`) && !strings.Contains(openOperationErr, "outside an Operation"), "operation read during a Harness Open = %v, want the uniform unavailable refusal", openOperationErr)
	testassert.Require(t, strings.Contains(openMissingErr, `credential "/missing" is unavailable`) && !strings.Contains(openMissingErr, "outside an Operation"), "missing operation field read = %v, want the uniform unavailable refusal", openMissingErr)
	testassert.Require(t, strings.Contains(openUndeclaredErr, "is not declared"), "undeclared field read = %v, want the declaration refusal", openUndeclaredErr)

	value := rt.capabilities["cred.probe"]
	testassert.Require(t, value != nil && value.activationSecrets["/activation"] == "activation-value", "capability = %+v, want the captured activation value", value)
	operationSecrets := map[string]map[string]string{"cred": {"/operation": "operation-value"}}
	view, call := c.newView(value, context.Background(), callIdentity{session: harness.SessionIdentity{SessionID: testSessionID}, operationID: "op", operationSecrets: operationSecrets})
	defer view.end()
	// The activation capture stays available to every later callback of the
	// component that opened it.
	result, err := call.Call(context.Background(), "credentials", "get", json.RawMessage(`{"field":"/activation"}`))
	testassert.Equal(t, credentialValue(t, result, err), "activation-value")
	// The operation reference resolves from the live Operation's capture, not
	// from a process-global current value.
	result, err = call.Call(context.Background(), "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
	testassert.Equal(t, credentialValue(t, result, err), "operation-value")
	// Another extension's capture entry is never visible through this
	// caller's own declared field.
	crossView, crossCall := c.newView(value, context.Background(), callIdentity{session: harness.SessionIdentity{SessionID: testSessionID}, operationID: "op", operationSecrets: map[string]map[string]string{"credother": {"/operation": "other-value"}}})
	defer crossView.end()
	_, err = crossCall.Call(context.Background(), "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "is unavailable"), "unrelated extension operation value = %v, want the unavailable refusal", err)
	// A guest Operation identity without a host-carried capture is not
	// authority for an Operation credential.
	nilView, nilCall := c.newView(value, context.Background(), callIdentity{session: harness.SessionIdentity{SessionID: testSessionID}, operationID: "op", operationSecrets: nil})
	defer nilView.end()
	_, err = nilCall.Call(context.Background(), "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
	testassert.Require(t, err != nil && strings.Contains(err.Error(), `credential "/operation" is unavailable`) && !strings.Contains(err.Error(), "outside an Operation"), "OperationID with no private capture = %v, want the uniform unavailable refusal", err)
}

// TestNativeCredentialsActivationAvailableInClose proves a component's Close
// callback keeps the same retained activation capture as Open and ordinary
// callbacks, and that its call view still expires when Close returns.
func TestNativeCredentialsActivationAvailableInClose(t *testing.T) {
	const activationName = "LIGHTCODE_CLOSE_ACTIVATION"
	t.Setenv(activationName, "close-activation")
	capability := extension.ServiceCapability("cred.close", "test.cred.close", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	var closeValue string
	var closeErr error
	var retained extension.CallContext
	native := credentialHostNative(t, "credclose", []extension.SecretRef{{Field: "/activation", Lifetime: extension.SecretActivation}}, extension.LifetimeHarness, capability, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"cred.close": {Service: map[string]extension.Handler{"call": testServiceHandler()}}}, Close: func(ctx context.Context, call extension.CallContext) error {
			result, err := call.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/activation"}`))
			closeValue, closeErr = credentialString(result), err
			retained = call
			return nil
		}}, nil
	})
	c := mustComposition(t, native)
	sections := map[string]json.RawMessage{"credclose": json.RawMessage(`{"enabled":true,"activation":"` + activationName + `"}`)}
	declaredBindings := map[declaredField]declaredBinding{
		{extensionID: "credclose", field: "/activation"}: {name: activationName, value: "close-activation"},
	}
	rt := mustOpenScope(t, c, context.Background(), ScopeInfo{Kind: ScopeRuntime, DataDir: "/d", sections: sections, declaredBindings: declaredBindings}, nil)
	testassert.NoError(t, rt.close())
	testassert.Require(t, closeErr == nil && closeValue == "close-activation", "Close activation get = (%q, %v), want the retained capture", closeValue, closeErr)
	_, err := retained.Call(context.Background(), "credentials", "get", json.RawMessage(`{"field":"/activation"}`))
	testassert.Require(t, errors.Is(err, extension.ErrClosed), "retained Close view call = %v, want extension.ErrClosed", err)
}

// TestOperationCredentialsUsePreparedOpeningCapture proves the operation
// credential value sampled at the preparation boundary is the one retained
// through the prepared opener, even when the environment changes while the
// admitted opener is parked before Operation construction.
func TestOperationCredentialsUsePreparedOpeningCapture(t *testing.T) {
	const name = "LIGHTCODE_OP_PREPARED"
	t.Setenv(name, "prepared-value")
	h := newServiceHarness(t)
	probe := &credentialGateProbe{}
	writeServiceFile(t, h.configPath, credentialServiceConfig(`{"credprobe":{"enabled":true,"activation":"`+name+`","operation":"`+name+`"}}`))
	svc := h.preparedService(context.Background(), probe.native(t))
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()

	artifacts := newArtifactIntervals()
	p := newPreparation(svc, h.locations(), "", nil, newObservationAdapter(newObservation(), newWarningStore()), artifacts, nil, nil)
	sessionID := testSessionID
	prepared, err := p.bind()(context.Background(), harness.PreparationRequest{
		Session: harness.PreparationSession{
			Identity: harness.SessionIdentity{SessionID: sessionID, Workspace: h.dataDir},
			AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{
				Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none", Tools: []string{"cred_probe"},
			}},
		},
		RequestKind: harness.RequestKindMessage,
	})
	testassert.NoError(t, err)
	defer prepared.Release()
	var provenanceSettings string
	for _, implementation := range prepared.Capture.Implementations {
		if implementation.ExtensionID == "credprobe" {
			provenanceSettings = string(implementation.Settings)
		}
	}
	testassert.Require(t, strings.Contains(provenanceSettings, name), "provenance settings = %q, want the declared reference name", provenanceSettings)
	testassert.Require(t, !strings.Contains(provenanceSettings, "prepared-value"), "provenance settings = %q, want no captured credential value", provenanceSettings)
	key := sessionArtifactDir(svc.dataDir, sessionID)
	releaseHolder, err := artifacts.acquireExecution(context.Background(), key)
	testassert.NoError(t, err)
	var execution harness.Execution
	var openErr error
	openDone := make(chan struct{})
	go func() {
		execution, openErr = prepared.Open(context.Background(), harness.OperationAdmission{SessionID: sessionID, OperationID: "op-prepared", RequestKind: harness.RequestKindMessage, AdmittedAt: time.Now()})
		close(openDone)
	}()
	awaitRegistryState(t, artifacts, key, 2)
	t.Setenv(name, "changed-value")
	releaseHolder()
	<-openDone
	testassert.NoError(t, openErr)
	defer func() { testassert.NoError(t, execution.Close()) }()

	inv, err := execution.OpenInvocation(context.Background(), harness.InvocationRequest{ID: "inv-prepared", Purpose: harness.InvocationPurposeConversation, Model: prepared.Capture.Model})
	testassert.NoError(t, err)
	defer func() { testassert.NoError(t, inv.Close()) }()
	outcome := inv.Tool(context.Background(), model.ToolCall{ID: "call-prepared", Name: "cred_probe", Arguments: json.RawMessage(`{"key":"probe"}`)}).Execute(context.Background())
	testassert.Require(t, outcome.Result.Status == model.ResultSuccess, "tool outcome = %+v, want success", outcome.Result)
	probe.mu.Lock()
	defer probe.mu.Unlock()
	testassert.Equal(t, probe.values["probe"], "prepared-value|prepared-value")
}

// TestOperationCredentialNamesShareOnePreparationCapture proves two active
// extensions resolving the same credential name derive their Operation fields
// from the same preparation capture.
func TestOperationCredentialNamesShareOnePreparationCapture(t *testing.T) {
	const name = "LIGHTCODE_OP_SHARED"
	t.Setenv(name, "shared-value")
	h := newServiceHarness(t)
	first := &credentialGateProbe{}
	second := &credentialGateProbe{}
	writeServiceFile(t, h.configPath, credentialServiceConfig(`{"creda":{"enabled":true,"activation":"`+name+`","operation":"`+name+`"},"credb":{"enabled":true,"activation":"`+name+`","operation":"`+name+`"}}`))
	svc := h.preparedService(context.Background(), first.nativeFor(t, "creda", "cred_probe_a"), second.nativeFor(t, "credb", "cred_probe_b"))
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()

	p := newPreparation(svc, h.locations(), "", nil, newObservationAdapter(newObservation(), newWarningStore()), newArtifactIntervals(), nil, nil)
	sessionID := testSessionID
	prepared, err := p.bind()(context.Background(), harness.PreparationRequest{
		Session: harness.PreparationSession{
			Identity: harness.SessionIdentity{SessionID: sessionID, Workspace: h.dataDir},
			AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{
				Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none", Tools: []string{"cred_probe_a", "cred_probe_b"},
			}},
		},
		RequestKind: harness.RequestKindMessage,
	})
	testassert.NoError(t, err)
	defer prepared.Release()

	t.Setenv(name, "changed-value")
	execution, err := prepared.Open(context.Background(), harness.OperationAdmission{SessionID: sessionID, OperationID: "op-shared", RequestKind: harness.RequestKindMessage, AdmittedAt: time.Now()})
	testassert.NoError(t, err)
	defer func() { testassert.NoError(t, execution.Close()) }()
	inv, err := execution.OpenInvocation(context.Background(), harness.InvocationRequest{ID: "inv-shared", Purpose: harness.InvocationPurposeConversation, Model: prepared.Capture.Model})
	testassert.NoError(t, err)
	defer func() { testassert.NoError(t, inv.Close()) }()
	inv.Tool(context.Background(), model.ToolCall{ID: "call-a", Name: "cred_probe_a", Arguments: json.RawMessage(`{"key":"a"}`)}).Execute(context.Background())
	inv.Tool(context.Background(), model.ToolCall{ID: "call-b", Name: "cred_probe_b", Arguments: json.RawMessage(`{"key":"b"}`)}).Execute(context.Background())
	first.mu.Lock()
	second.mu.Lock()
	defer first.mu.Unlock()
	defer second.mu.Unlock()
	testassert.Equal(t, first.values["a"], "shared-value|shared-value")
	testassert.Equal(t, second.values["b"], "shared-value|shared-value")
}

// TestDeclaredCredentialRotationReplacesResources proves a candidate whose
// active sections are unchanged but whose declared binding value rotated
// prepares a replacement resource set, while an identical candidate reuses
// the current one.
func TestDeclaredCredentialRotationReplacesResources(t *testing.T) {
	const name = "LIGHTCODE_RES_ROTATE"
	t.Setenv(name, "one")
	h := newServiceHarness(t)
	var opens []string
	capability := extension.ServiceCapability("cred.res", "test.cred.res", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	native := credentialHostNative(t, "credres", []extension.SecretRef{{Field: "/activation", Lifetime: extension.SecretActivation}}, extension.LifetimeHarness, capability, func(ctx context.Context, info extension.InstanceContext) (extension.Instance, error) {
		result, err := info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/activation"}`))
		opens = append(opens, credentialValue(t, result, err))
		return extension.Instance{Capabilities: map[string]extension.Implementation{"cred.res": {Service: map[string]extension.Handler{"call": testServiceHandler()}}}}, nil
	})
	writeServiceFile(t, h.configPath, credentialServiceConfig(`{"credres":{"enabled":true,"activation":"`+name+`"}}`))
	svc := h.preparedService(context.Background(), native)
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	first := svc.current().resources
	testassert.Equal(t, strings.Join(opens, ","), "one")

	t.Setenv(name, "two")
	_, err = svc.publish(context.Background())
	testassert.NoError(t, err)
	second := svc.current().resources
	testassert.Require(t, second != first, "a rotated declared binding reused the old resource set")
	testassert.Equal(t, strings.Join(opens, ","), "one,two")

	_, err = svc.publish(context.Background())
	testassert.NoError(t, err)
	third := svc.current().resources
	testassert.Require(t, third == second, "an identical candidate replaced the resource set")
	testassert.Equal(t, strings.Join(opens, ","), "one,two")
	defer svc.current().resources.release()
}

// TestDeclaredCredentialLazyWorkspaceUsesCandidateCapture proves a Workspace
// component that opens after the candidate build uses the captured binding
// rather than sampling a later environment value.
func TestDeclaredCredentialLazyWorkspaceUsesCandidateCapture(t *testing.T) {
	const name = "LIGHTCODE_LAZY_WORKSPACE"
	t.Setenv(name, "candidate-value")
	h := newServiceHarness(t)
	var opened string
	capability := extension.ServiceCapability("cred.lazy", "test.cred.lazy", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	native := credentialHostNative(t, "credado", []extension.SecretRef{{Field: "/activation", Lifetime: extension.SecretActivation}}, extension.LifetimeWorkspace, capability, func(ctx context.Context, info extension.InstanceContext) (extension.Instance, error) {
		result, err := info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/activation"}`))
		opened = credentialValue(t, result, err)
		return extension.Instance{Capabilities: map[string]extension.Implementation{"cred.lazy": {Service: map[string]extension.Handler{"call": testServiceHandler()}}}}, nil
	})
	writeServiceFile(t, h.configPath, credentialServiceConfig(`{"credado":{"enabled":true,"activation":"`+name+`"}}`))
	svc := h.preparedService(context.Background(), native)
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()
	t.Setenv(name, "later-value")

	p := newPreparation(svc, h.locations(), "", nil, newObservationAdapter(newObservation(), newWarningStore()), nil, nil, nil)
	prepared, err := p.bind()(context.Background(), harness.PreparationRequest{
		Session: harness.PreparationSession{
			Identity:       harness.SessionIdentity{SessionID: testSessionID, Workspace: h.dataDir},
			AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none"}},
		},
		RequestKind: harness.RequestKindMessage,
	})
	testassert.NoError(t, err)
	defer prepared.Release()
	testassert.Equal(t, opened, "candidate-value")
}

// TestExternalSharedCredentialRotationKeepsProviderObservation proves an
// external rotation of a provider/extension shared name after startup, with
// no reload, keeps the provider's ordinary fresh observation and the
// preparation's Operation value from that same observation, while activation
// retains its candidate binding.
func TestExternalSharedCredentialRotationKeepsProviderObservation(t *testing.T) {
	const name = "LIGHTCODE_SHARED_PROVIDER"
	t.Setenv(name, "shared-candidate")
	h := newServiceHarness(t)
	native := credentialHostNative(t, "credshared", []extension.SecretRef{
		{Field: "/activation", Lifetime: extension.SecretActivation},
		{Field: "/operation", Lifetime: extension.SecretOperation},
	}, extension.LifetimeHarness, extension.ServiceCapability("cred.shared", "test.cred.shared", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", "")), func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"cred.shared": testServiceImplementation()}}, nil
	})
	writeServiceFile(t, h.configPath, `{"providers":{"prov":`+nominalProvMember("http://prov.test/v1", name, `{"m":{"name":"M","context_window":4096}}`)+`},"extensions":{"credshared":{"enabled":true,"activation":"`+name+`","operation":"`+name+`"}}}`)
	svc := h.preparedService(context.Background(), native)
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()
	t.Setenv(name, "shared-later")
	_, err = svc.publish(context.Background())
	testassert.NoError(t, err)

	captured, err := svc.capture(context.Background())
	testassert.NoError(t, err)
	testassert.Equal(t, captured.snapshot.credentials[name].Value, "shared-later")
	binding := captured.snapshot.declaredBindings[declaredField{extensionID: "credshared", field: "/operation"}]
	testassert.Equal(t, binding.value, "shared-later")
	activationBinding := captured.snapshot.declaredBindings[declaredField{extensionID: "credshared", field: "/activation"}]
	testassert.Equal(t, activationBinding.value, "shared-later")
	operations := captured.operationSecrets()
	testassert.Equal(t, operations["credshared"]["/operation"], "shared-later")
	settings := projectSettings(captured.snapshot.sessions, captured.snapshot.tools, captured.snapshot.jobs, captured.snapshot.compaction, captured.snapshot.extensions)
	projected := settings.Extensions["credshared"]
	testassert.Require(t, strings.Contains(projected, name), "settings extension document = %q, want the reference name", projected)
	testassert.Require(t, !strings.Contains(projected, "shared-candidate") && !strings.Contains(projected, "shared-later"), "settings extension document = %q, want no credential value", projected)
}

// TestManagedKeyEditRefreshesSharedDeclaredBinding proves a connection edit
// that writes a managed key shared with a declared reference publishes the
// new binding and replaces the resource set instead of reusing stale values.
func TestManagedKeyEditRefreshesSharedDeclaredBinding(t *testing.T) {
	const name = "LIGHTCODE_SHARED_MANAGED"
	h := newServiceHarness(t)
	t.Cleanup(func() { _ = os.Unsetenv(name) })
	managed := config.NewManagedEnv(filepath.Join(h.home, ".lightcode", ".env"))
	testassert.NoError(t, managed.TrySet(name, "old-value"))
	native := credentialHostNative(t, "credmanaged", []extension.SecretRef{
		{Field: "/activation", Lifetime: extension.SecretActivation},
		{Field: "/operation", Lifetime: extension.SecretOperation},
	}, extension.LifetimeHarness, extension.ServiceCapability("cred.managed", "test.cred.managed", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", "")), func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"cred.managed": testServiceImplementation()}}, nil
	})
	writeServiceFile(t, h.configPath, `{"providers":{"prov":`+nominalProvMember("http://prov.test/v1", name, `{"m":{"name":"M","context_window":4096}}`)+`},"extensions":{"credmanaged":{"enabled":true,"activation":"`+name+`","operation":"`+name+`"}}}`)
	svc := h.preparedService(context.Background(), native)
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	first := svc.current().resources
	provider := svc.current().catalog.Providers["prov"]
	testassert.Require(t, provider != nil, "provider fixture is missing")

	mutation, err := svc.mutate(context.Background(), svc.editProviderConnect(&connectionEffects{
		providerID: "prov", transport: provider.Transport, keyAction: keyActionSet, keyEnv: name, keyValue: "new-value",
	}))
	testassert.NoError(t, err)
	second := svc.current().resources
	testassert.Require(t, second != first, "a managed-key edit reused the stale resource set")
	binding := mutation.snapshot.declaredBindings[declaredField{extensionID: "credmanaged", field: "/operation"}]
	testassert.Equal(t, binding.value, "new-value")
	testassert.Equal(t, captureValueForTest(t, managed, name), "new-value")
	captured, err := svc.capture(context.Background())
	testassert.NoError(t, err)
	testassert.Equal(t, captured.snapshot.credentials[name].Value, "new-value")
	defer svc.current().resources.release()
}

// TestCredentialLifecycleCaptureCoversOperationAndInvocation proves Operation
// and Invocation Open/Close callbacks observe the retained Operation capture,
// not a fresh environment sample.
func TestCredentialLifecycleCaptureCoversOperationAndInvocation(t *testing.T) {
	const name = "LIGHTCODE_LIFECYCLE_BIND"
	t.Setenv(name, "lifecycle-value")
	h := newServiceHarness(t)
	openValues := make(map[string]string)
	closeValues := make(map[string]string)
	native := lifecycleCredentialNative(t, "credlife", name, openValues, closeValues, "")
	writeServiceFile(t, h.configPath, credentialServiceConfig(`{"credlife":{"enabled":true,"operation":"`+name+`"}}`))
	svc := h.preparedService(context.Background(), native)
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()
	t.Setenv(name, "later-value")
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}

	p := newPreparation(svc, h.locations(), "", nil, newObservationAdapter(newObservation(), newWarningStore()), nil, nil, nil)
	prepared, err := p.bind()(context.Background(), harness.PreparationRequest{
		Session: harness.PreparationSession{
			Identity:       harness.SessionIdentity{SessionID: testSessionID, Workspace: h.dataDir},
			AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none"}},
		},
		RequestKind: harness.RequestKindMessage,
	})
	testassert.NoError(t, err)
	defer prepared.Release()
	execution, err := prepared.Open(context.Background(), harness.OperationAdmission{SessionID: testSessionID, OperationID: "op-lifecycle", RequestKind: harness.RequestKindMessage, AdmittedAt: time.Now()})
	testassert.NoError(t, err)
	testassert.Equal(t, openValues["op"], "later-value")
	inv, err := execution.OpenInvocation(context.Background(), harness.InvocationRequest{ID: "inv-lifecycle", Purpose: harness.InvocationPurposeConversation, Model: prepared.Capture.Model})
	testassert.NoError(t, err)
	testassert.Equal(t, openValues["inv"], "later-value")
	testassert.NoError(t, inv.Close())
	testassert.NoError(t, execution.Close())
	testassert.Equal(t, closeValues["inv"], "later-value")
	testassert.Equal(t, closeValues["op"], "later-value")
}

// TestCredentialLifecycleFailedPrefixCleanupKeepsCapture proves an Operation
// component that opened before a sibling failure closes with the same
// retained Operation capture.
func TestCredentialLifecycleFailedPrefixCleanupKeepsCapture(t *testing.T) {
	const name = "LIGHTCODE_LIFECYCLE_CLEANUP"
	t.Setenv(name, "cleanup-value")
	h := newServiceHarness(t)
	openValues := make(map[string]string)
	closeValues := make(map[string]string)
	native := lifecycleCredentialNative(t, "credcleanup", name, openValues, closeValues, "op-fail")
	writeServiceFile(t, h.configPath, credentialServiceConfig(`{"credcleanup":{"enabled":true,"operation":"`+name+`"}}`))
	svc := h.preparedService(context.Background(), native)
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()
	t.Setenv(name, "later-value")
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}

	p := newPreparation(svc, h.locations(), "", nil, newObservationAdapter(newObservation(), newWarningStore()), nil, nil, nil)
	prepared, err := p.bind()(context.Background(), harness.PreparationRequest{
		Session: harness.PreparationSession{
			Identity:       harness.SessionIdentity{SessionID: testSessionID, Workspace: h.dataDir},
			AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none"}},
		},
		RequestKind: harness.RequestKindMessage,
	})
	testassert.NoError(t, err)
	defer prepared.Release()
	_, err = prepared.Open(context.Background(), harness.OperationAdmission{SessionID: testSessionID, OperationID: "op-cleanup", RequestKind: harness.RequestKindMessage, AdmittedAt: time.Now()})
	testassert.Require(t, err != nil, "the failing Operation component did not fail the open")
	testassert.Equal(t, closeValues["op"], "later-value")
}

func lifecycleCredentialNative(t *testing.T, id, credentialName string, openValues, closeValues map[string]string, failComponent string) extension.Native {
	t.Helper()
	host, ok := extension.HostCapability(extension.ContractCredentials)
	testassert.Require(t, ok, "the credentials host contract is not defined")
	binding := extension.Binding{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: host}
	components := []extension.Component{
		{ID: "op", Lifetime: extension.LifetimeOperation, Requires: []extension.Binding{binding}},
		{ID: "inv", Lifetime: extension.LifetimeInvocation, Requires: []extension.Binding{binding}},
	}
	if failComponent != "" {
		components = append(components, extension.Component{ID: "op-fail", Lifetime: extension.LifetimeOperation, Requires: []extension.Binding{binding}})
	}
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           id,
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"operation":{"type":"string"}},"required":["enabled"]}`),
			Secrets:      []extension.SecretRef{{Field: "/operation", Lifetime: extension.SecretOperation}},
			Components:   components,
		},
		Open: func(ctx context.Context, info extension.InstanceContext) (extension.Instance, error) {
			if info.ComponentID == failComponent {
				return extension.Instance{}, errors.New("component open failure")
			}
			result, err := info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
			if err != nil {
				return extension.Instance{}, err
			}
			key := "op"
			if info.ComponentID == "inv" {
				key = "inv"
			}
			openValues[key] = credentialString(result)
			return extension.Instance{Close: func(ctx context.Context, call extension.CallContext) error {
				result, err := call.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
				if err != nil {
					return err
				}
				closeValues[key] = credentialString(result)
				return nil
			}}, nil
		},
	}
}

// TestCredentialEffectfulHookUsesOperationCapture proves an effectful
// before_tool hook receives the retained Operation capture through the same
// callback handoff as tools.
func TestCredentialEffectfulHookUsesOperationCapture(t *testing.T) {
	const name = "LIGHTCODE_EFFECTFUL_HOOK"
	t.Setenv(name, "hook-value")
	h := newServiceHarness(t)
	host, ok := extension.HostCapability(extension.ContractCredentials)
	testassert.Require(t, ok, "the credentials host contract is not defined")
	var hookValue string
	native := extension.Native{
		Declaration: extension.Declaration{
			ID:           "credhook",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"operation":{"type":"string"}},"required":["enabled"]}`),
			Secrets:      []extension.SecretRef{{Field: "/operation", Lifetime: extension.SecretOperation}},
			Components: []extension.Component{{
				ID: "credhook-c", Lifetime: extension.LifetimeHarness, Provides: []string{"cred_tool", "cred_effect_hook"},
				Requires: []extension.Binding{{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: host}},
			}},
			Capabilities: []extension.Capability{
				extension.ToolCapability("cred_tool"),
				extension.HookCapability("cred_effect_hook", extension.SingleHook(extension.BoundaryBeforeTool, extension.HookModeEffectful)),
			},
		},
		Describe: map[string]extension.ToolDescribe{"cred_tool": func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
			return extension.ToolDescription{Definition: model.ToolDefinition{Name: "cred_tool", Parameters: json.RawMessage(`{"type":"object"}`)}, Available: true}, nil
		}},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				"cred_tool": {Tool: noopNativeTool{}},
				"cred_effect_hook": {BeforeTool: func(ctx context.Context, call extension.CallContext, toolCall model.ToolCall) (json.RawMessage, error) {
					result, err := call.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
					if err != nil {
						return nil, err
					}
					hookValue = credentialString(result)
					return toolCall.Arguments, nil
				}},
			}}, nil
		},
	}
	writeServiceFile(t, h.configPath, credentialServiceConfig(`{"credhook":{"enabled":true,"operation":"`+name+`"}}`))
	svc := h.preparedService(context.Background(), native)
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()
	t.Setenv(name, "later-value")
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}

	p := newPreparation(svc, h.locations(), "", nil, newObservationAdapter(newObservation(), newWarningStore()), nil, nil, nil)
	prepared, err := p.bind()(context.Background(), harness.PreparationRequest{
		Session: harness.PreparationSession{
			Identity: harness.SessionIdentity{SessionID: testSessionID, Workspace: h.dataDir},
			AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{
				Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none", Tools: []string{"cred_tool"}, Capabilities: []string{"cred_effect_hook"},
			}},
		},
		RequestKind: harness.RequestKindMessage,
	})
	testassert.NoError(t, err)
	defer prepared.Release()
	execution, err := prepared.Open(context.Background(), harness.OperationAdmission{SessionID: testSessionID, OperationID: "op-hook", RequestKind: harness.RequestKindMessage, AdmittedAt: time.Now()})
	testassert.NoError(t, err)
	defer func() { testassert.NoError(t, execution.Close()) }()
	inv, err := execution.OpenInvocation(context.Background(), harness.InvocationRequest{ID: "inv-hook", Purpose: harness.InvocationPurposeConversation, Model: prepared.Capture.Model})
	testassert.NoError(t, err)
	defer func() { testassert.NoError(t, inv.Close()) }()
	testassert.Require(t, len(inv.ToolHooks) == 1, "tool hooks = %d, want one effectful hook", len(inv.ToolHooks))
	_, err = inv.ToolHooks[0].Run(context.Background(), model.ToolCall{ID: "call-hook", Name: "cred_tool", Arguments: json.RawMessage(`{}`)})
	testassert.NoError(t, err)
	testassert.Equal(t, hookValue, "later-value")
}

// credentialValueStrategy records the Operation value it obtained at its
// legitimate Open.
type credentialValueStrategy struct{ value string }

func (s *credentialValueStrategy) Plan(harness.PlanInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

func (s *credentialValueStrategy) Consume(harness.ConsumeInput) (harness.PlanOutput, error) {
	return harness.PlanOutput{Kind: harness.PlanNone}, nil
}

func TestCredentialStrategyCapturesAtItsOperationOpen(t *testing.T) {
	const name = "LIGHTCODE_STRATEGY_BIND"
	t.Setenv(name, "strategy-value")
	h := newServiceHarness(t)
	host, ok := extension.HostCapability(extension.ContractCredentials)
	testassert.Require(t, ok, "the credentials host contract is not defined")
	var strategy *credentialValueStrategy
	native := extension.Native{
		Declaration: extension.Declaration{
			ID:           "credstrat",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"operation":{"type":"string"}},"required":["enabled"]}`),
			Secrets:      []extension.SecretRef{{Field: "/operation", Lifetime: extension.SecretOperation}},
			Components: []extension.Component{{
				ID: "credstrat-c", Lifetime: extension.LifetimeOperation, Provides: []string{"cred_strategy"},
				Requires: []extension.Binding{{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: host}},
			}},
			Capabilities: []extension.Capability{extension.StrategyCapability("cred_strategy", false)},
		},
		Open: func(ctx context.Context, info extension.InstanceContext) (extension.Instance, error) {
			result, err := info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
			if err != nil {
				return extension.Instance{}, err
			}
			strategy = &credentialValueStrategy{value: credentialString(result)}
			return extension.Instance{Capabilities: map[string]extension.Implementation{"cred_strategy": {Strategy: strategy}}}, nil
		},
	}
	writeServiceFile(t, h.configPath, credentialServiceConfig(`{"credstrat":{"enabled":true,"operation":"`+name+`"}}`))
	svc := h.preparedService(context.Background(), native)
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()
	t.Setenv(name, "later-value")
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}

	p := newPreparation(svc, h.locations(), "", nil, newObservationAdapter(newObservation(), newWarningStore()), nil, nil, nil)
	prepared, err := p.bind()(context.Background(), harness.PreparationRequest{
		Session: harness.PreparationSession{
			Identity: harness.SessionIdentity{SessionID: testSessionID, Workspace: h.dataDir},
			AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{
				Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none", Strategy: harness.StrategySelection{ID: "cred_strategy"},
			}},
		},
		RequestKind: harness.RequestKindMessage,
	})
	testassert.NoError(t, err)
	defer prepared.Release()
	execution, err := prepared.Open(context.Background(), harness.OperationAdmission{SessionID: testSessionID, OperationID: "op-strategy", RequestKind: harness.RequestKindMessage, AdmittedAt: time.Now()})
	testassert.NoError(t, err)
	defer func() { testassert.NoError(t, execution.Close()) }()
	testassert.Require(t, execution.Strategy != nil && execution.Strategy.ID == "cred_strategy" && strategy != nil, "strategy binding = %+v, want the opened strategy", execution.Strategy)
	if _, err := execution.Strategy.Strategy.Plan(harness.PlanInput{}); err != nil {
		t.Fatalf("strategy Plan: %v", err)
	}
	testassert.Equal(t, strategy.value, "later-value")
}

// credentialNestedTool is one caller tool whose nested provider call reads
// the provider's own declared credential field.
type credentialNestedTool struct{}

func (credentialNestedTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}
func (credentialNestedTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{}
}
func (credentialNestedTool) Execute(ctx context.Context, tc extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	own, err := tc.Calls.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/caller"}`))
	if err != nil {
		return credentialOutcome("call", "caller error: "+err.Error())
	}
	nested, err := tc.Calls.Call(ctx, "provider", "call", json.RawMessage(`{}`))
	if err != nil {
		return credentialOutcome("call", "provider error: "+err.Error())
	}
	return credentialOutcome("call", "caller="+credentialString(own)+"|provider="+credentialString(nested))
}

func TestCredentialNestedProviderUsesItsOwnDeclaredField(t *testing.T) {
	const callerName = "LIGHTCODE_NESTED_CALLER"
	const providerName = "LIGHTCODE_NESTED_PROVIDER"
	t.Setenv(callerName, "caller-value")
	t.Setenv(providerName, "provider-value")
	h := newServiceHarness(t)
	host, ok := extension.HostCapability(extension.ContractCredentials)
	testassert.Require(t, ok, "the credentials host contract is not defined")
	providerCapability := extension.ServiceCapability("cred_provider", "test.cred.provider", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
	providerNative := extension.Native{
		Declaration: extension.Declaration{
			ID:           "credprovider",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"activation":{"type":"string"}},"required":["enabled"]}`),
			Secrets:      []extension.SecretRef{{Field: "/provider", Lifetime: extension.SecretActivation}},
			Components: []extension.Component{{
				ID: "credprovider-c", Lifetime: extension.LifetimeHarness, Provides: []string{"cred_provider"},
				Requires: []extension.Binding{{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: host}},
			}},
			Capabilities: []extension.Capability{providerCapability},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"cred_provider": {Service: map[string]extension.Handler{"call": func(ctx context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				return call.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/provider"}`))
			}}}}}, nil
		},
	}
	binding := extension.Binding{Name: "provider", Provider: "credprovider/credprovider-c", CapabilityID: "cred_provider", Definition: providerCapability.Definition()}
	callerCapability := extension.ToolCapability("cred_caller")
	callerNative := extension.Native{
		Declaration: extension.Declaration{
			ID:           "credcaller",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"activation":{"type":"string"}},"required":["enabled"]}`),
			Secrets:      []extension.SecretRef{{Field: "/caller", Lifetime: extension.SecretActivation}},
			Components: []extension.Component{{
				ID: "credcaller-c", Lifetime: extension.LifetimeHarness, Provides: []string{"cred_caller"},
				Requires: []extension.Binding{
					{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: host},
					binding,
				},
			}},
			Capabilities: []extension.Capability{callerCapability},
		},
		Describe: map[string]extension.ToolDescribe{"cred_caller": func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
			return extension.ToolDescription{Definition: model.ToolDefinition{Name: "cred_caller", Parameters: json.RawMessage(`{"type":"object"}`)}, Available: true}, nil
		}},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"cred_caller": {Tool: credentialNestedTool{}}}}, nil
		},
	}
	writeServiceFile(t, h.configPath, credentialServiceConfig(`{"credprovider":{"enabled":true,"provider":"`+providerName+`"},"credcaller":{"enabled":true,"caller":"`+callerName+`"}}`))
	svc := h.preparedService(context.Background(), providerNative, callerNative)
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()

	p := newPreparation(svc, h.locations(), "", nil, newObservationAdapter(newObservation(), newWarningStore()), nil, nil, nil)
	prepared, err := p.bind()(context.Background(), harness.PreparationRequest{
		Session: harness.PreparationSession{
			Identity: harness.SessionIdentity{SessionID: testSessionID, Workspace: h.dataDir},
			AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{
				Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none", Tools: []string{"cred_caller"},
			}},
		},
		RequestKind: harness.RequestKindMessage,
	})
	testassert.NoError(t, err)
	defer prepared.Release()
	execution, err := prepared.Open(context.Background(), harness.OperationAdmission{SessionID: testSessionID, OperationID: "op-nested", RequestKind: harness.RequestKindMessage, AdmittedAt: time.Now()})
	testassert.NoError(t, err)
	defer func() { testassert.NoError(t, execution.Close()) }()
	inv, err := execution.OpenInvocation(context.Background(), harness.InvocationRequest{ID: "inv-nested", Purpose: harness.InvocationPurposeConversation, Model: prepared.Capture.Model})
	testassert.NoError(t, err)
	defer func() { testassert.NoError(t, inv.Close()) }()
	outcome := inv.Tool(context.Background(), model.ToolCall{ID: "call-nested", Name: "cred_caller", Arguments: json.RawMessage(`{}`)}).Execute(context.Background())
	testassert.Require(t, outcome.Result.Status == model.ResultSuccess, "nested outcome = %+v, want success", outcome.Result)
	testassert.Equal(t, outcome.Result.Content, "caller=caller-value|provider=provider-value")
}

// TestCustomProviderCreateRefreshesSharedDeclaredBinding proves a custom
// provider create with a supplied key shared by an active extension carries
// that pending key into the candidate binding, publishes the managed key, and
// replaces the resource set for both an absent and a previously managed key.
func TestCustomProviderCreateRefreshesSharedDeclaredBinding(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		envName     string
		seedManaged bool
	}{
		{name: "previously absent", envName: "LIGHTCODE_CREATE_ABSENT"},
		{name: "previously managed", envName: "LIGHTCODE_CREATE_MANAGED", seedManaged: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newServiceHarness(t)
			t.Cleanup(func() { _ = os.Unsetenv(testCase.envName) })
			managed := config.NewManagedEnv(filepath.Join(h.home, ".lightcode", ".env"))
			if testCase.seedManaged {
				testassert.NoError(t, managed.TrySet(testCase.envName, "old-managed"))
			}
			var opens atomic.Int64
			capability := extension.ServiceCapability("cred.create", "test.cred.create", extension.ServiceMethod("call", extension.ModeServiceIO, `{}`, `{"type":"object"}`, "", ""))
			native := credentialHostNative(t, "credcreate", []extension.SecretRef{
				{Field: "/activation", Lifetime: extension.SecretActivation},
				{Field: "/operation", Lifetime: extension.SecretOperation},
			}, extension.LifetimeHarness, capability, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
				opens.Add(1)
				return extension.Instance{Capabilities: map[string]extension.Implementation{"cred.create": {Service: map[string]extension.Handler{"call": testServiceHandler()}}}}, nil
			})
			writeServiceFile(t, h.configPath, credentialServiceConfig(`{"credcreate":{"enabled":true,"activation":"`+testCase.envName+`","operation":"`+testCase.envName+`"}}`))
			svc := h.preparedService(context.Background(), native)
			_, err := svc.publish(context.Background())
			testassert.NoError(t, err)
			firstResources := svc.current().resources
			testassert.Equal(t, opens.Load(), int64(1))

			const keyValue = "supplied-create-key"
			baseURL := "https://created.test/v1"
			window := 4096
			key := keyValue
			mutation, err := svc.mutate(context.Background(), svc.editProviderCreate(context.Background(), "newp", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &testCase.envName}, map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key))
			testassert.NoError(t, err)
			testassert.Require(t, svc.current().resources != firstResources, "custom create reused the stale resource set")
			testassert.Equal(t, opens.Load(), int64(2))
			binding := mutation.snapshot.declaredBindings[declaredField{extensionID: "credcreate", field: "/operation"}]
			testassert.Equal(t, binding.value, keyValue)
			provider := mutation.snapshot.catalog.Providers["newp"]
			testassert.Require(t, provider != nil && provider.Models["m"] != nil && provider.Models["m"].ContextWindow == window, "created provider = %+v, want the ready model", provider)
			captured, err := svc.capture(context.Background())
			testassert.NoError(t, err)
			observed := captured.snapshot.credentials[testCase.envName]
			testassert.Require(t, observed.Defined && observed.Managed && observed.Value == keyValue, "provider credential = %+v, want the supplied managed key", observed)
			testassert.Require(t, catalog.ProviderConnected(provider, capturedEnvIsSet(captured.snapshot.credentials)), "created provider = %+v, want the ready authenticated provider", provider)
			testassert.Equal(t, captured.operationSecrets()["credcreate"]["/operation"], keyValue)
			testassert.Equal(t, captureValueForTest(t, managed, testCase.envName), keyValue)
			defer svc.current().resources.release()
		})
	}
}

// TestDefinedEmptyExternalSharedKeyRefusesSuppliedConnect proves a shared
// declared binding does not reclassify a defined-empty external provider key:
// the external observation survives and a supplied-key connect refuses before
// any config, cache or managed-source effect.
func TestDefinedEmptyExternalSharedKeyRefusesSuppliedConnect(t *testing.T) {
	const name = "LIGHTCODE_DEFINED_EMPTY_SHARED"
	e := newProductionEnv(t, storage.NewMemory())
	t.Setenv(name, "")
	writeServiceFile(t, e.configPath, `{"providers":{"prov":`+nominalProvMember(e.server.URL, name, `{"m":{"name":"M","context_window":4096}}`)+`},"extensions":{"credexternal":{"enabled":true,"value":"`+name+`"}}}`)
	native := extension.Native{
		Declaration: extension.Declaration{
			ID:           "credexternal",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"value":{"type":"string"}},"required":["enabled"]}`),
			Secrets:      []extension.SecretRef{{Field: "/value", Lifetime: extension.SecretActivation}},
			Components:   []extension.Component{{ID: "credexternal-c", Lifetime: extension.LifetimeHarness}},
		},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{}, nil
		},
	}
	r, err := e.openPlugins(context.Background(), native)
	testassert.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	before, err := os.ReadFile(e.configPath)
	testassert.NoError(t, err)
	generation := r.config.current().generation
	captured, err := r.config.capture(context.Background())
	testassert.NoError(t, err)
	observed := captured.snapshot.credentials[name]
	testassert.Require(t, observed.Defined && !observed.Managed && observed.Value == "", "shared external observation = %+v, want the unchanged defined-empty external record", observed)
	testassert.Equal(t, classifyKeySource(name, captured.snapshot.credentials), config.KeySourceExternal)
	testassert.Require(t, !capturedEnvIsSet(captured.snapshot.credentials)(name), "defined-empty external shared key reported as set")

	key := "supplied-key-must-refuse"
	if _, err := r.ConnectProvider(context.Background(), testCaller(), "prov", &key); err == nil || !strings.Contains(err.Error(), "set externally") {
		t.Fatalf("connectProvider with a defined-empty external shared key = %v, want the external refusal", err)
	}
	after, err := os.ReadFile(e.configPath)
	testassert.NoError(t, err)
	testassert.Require(t, string(after) == string(before), "refused connect rewrote the config document")
	testassert.Require(t, !testManaged(r, name), "refused connect managed the external shared key")
	testassert.Require(t, r.config.current().generation == generation, "refused connect consumed a configuration generation")
}

// TestPreparationUsesCurrentCredentialObservationAfterStartup proves a
// preparation started after an external shared-name rotation uses the current
// provider/Operation observation, while an already prepared Operation keeps
// its earlier provider, Operation and activation values frozen.
func TestPreparationUsesCurrentCredentialObservationAfterStartup(t *testing.T) {
	const name = "LIGHTCODE_CURRENT_OBSERVATION"
	t.Setenv(name, "shared-candidate")
	h := newServiceHarness(t)
	server := newProductionModelServer(t, "", "")
	probe := &credentialGateProbe{}
	writeServiceFile(t, h.configPath, `{"providers":{"prov":`+nominalProvMember(server.URL, name, `{"m":{"name":"M","context_window":4096}}`)+`},"extensions":{"credprobe":{"enabled":true,"activation":"`+name+`","operation":"`+name+`"}}}`)
	svc := h.preparedService(context.Background(), probe.native(t))
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()

	p := newPreparation(svc, h.locations(), "", nil, newObservationAdapter(newObservation(), newWarningStore()), newArtifactIntervals(), nil, nil)
	request := func(session string) harness.PreparationRequest {
		return harness.PreparationRequest{
			Session: harness.PreparationSession{
				Identity: harness.SessionIdentity{SessionID: session, Workspace: h.dataDir},
				AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{
					Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none", Tools: []string{"cred_probe"},
				}},
			},
			RequestKind: harness.RequestKindMessage,
		}
	}
	oldPrepared, err := p.bind()(context.Background(), request("session-old"))
	testassert.NoError(t, err)
	defer oldPrepared.Release()

	t.Setenv(name, "shared-later")
	_, err = svc.publish(context.Background())
	testassert.NoError(t, err)
	newPrepared, err := p.bind()(context.Background(), request("session-new"))
	testassert.NoError(t, err)
	defer newPrepared.Release()

	run := func(prepared harness.PreparedExecution, session, operation, invocation, key string) {
		t.Helper()
		execution, err := prepared.Open(context.Background(), harness.OperationAdmission{SessionID: session, OperationID: operation, RequestKind: harness.RequestKindMessage, AdmittedAt: time.Now()})
		testassert.NoError(t, err)
		defer func() { testassert.NoError(t, execution.Close()) }()
		inv, err := execution.OpenInvocation(context.Background(), harness.InvocationRequest{ID: invocation, Purpose: harness.InvocationPurposeConversation, Model: prepared.Capture.Model})
		testassert.NoError(t, err)
		defer func() { testassert.NoError(t, inv.Close()) }()
		before := server.requests()
		stream, err := inv.Model(context.Background(), model.Request{})
		testassert.NoError(t, err)
		if stream != nil {
			_ = stream.Close()
		}
		testassert.Require(t, server.requests() == before+1 && server.authAt(before) == "Bearer "+key, "provider authorization = %q, want %q", server.authAt(before), "Bearer "+key)
		outcome := inv.Tool(context.Background(), model.ToolCall{ID: "call-" + key, Name: "cred_probe", Arguments: json.RawMessage(`{"key":"` + key + `"}`)}).Execute(context.Background())
		testassert.Require(t, outcome.Result.Status == model.ResultSuccess, "tool outcome = %+v, want success", outcome.Result)
	}
	run(newPrepared, "session-new", "op-new", "inv-new", "shared-later")
	run(oldPrepared, "session-old", "op-old", "inv-old", "shared-candidate")
	probe.mu.Lock()
	defer probe.mu.Unlock()
	testassert.Equal(t, probe.values["shared-later"], "shared-later|shared-later")
	testassert.Equal(t, probe.values["shared-candidate"], "shared-candidate|shared-candidate")
}

func credentialServiceConfig(extensions string) string {
	return `{"providers":{"prov":` + nominalProvMember("http://prov.test/v1", "", `{"m":{"name":"M","context_window":4096}}`) + `},"extensions":` + extensions + `}`
}

// credentialProbe is the runtime acceptance extension: one Harness-scoped
// tool and preparation hook over the declared credentials host binding.
type credentialProbe struct {
	mu sync.Mutex

	openActivation   string
	openOperationErr string
	openUndeclared   string
	hookShell        string
	hookCalls        int

	toolValues []string
}

func (p *credentialProbe) native(t *testing.T) extension.Native {
	t.Helper()
	host, ok := extension.HostCapability(extension.ContractCredentials)
	testassert.Require(t, ok, "the credentials host contract is not defined")
	toolCapability := extension.ToolCapability("cred_probe")
	hookCapability := extension.HookCapability("cred_hook", extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure))
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           "credprobe",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"activation":{"type":"string"},"operation":{"type":"string"}},"required":["enabled"]}`),
			Secrets: []extension.SecretRef{
				{Field: "/activation", Lifetime: extension.SecretActivation},
				{Field: "/operation", Lifetime: extension.SecretOperation},
			},
			Components: []extension.Component{{
				ID:       "credprobe-c",
				Lifetime: extension.LifetimeHarness,
				Provides: []string{"cred_probe", "cred_hook"},
				Requires: []extension.Binding{{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: host}},
			}},
			Capabilities: []extension.Capability{toolCapability, hookCapability},
		},
		Describe: map[string]extension.ToolDescribe{"cred_probe": func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
			return extension.ToolDescription{Definition: model.ToolDefinition{Name: "cred_probe", Description: "reads declared credentials", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}, Available: true}, nil
		}},
		Open: func(ctx context.Context, info extension.InstanceContext) (extension.Instance, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			result, err := info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/activation"}`))
			p.openActivation = credentialValue(t, result, err)
			_, err = info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
			if err != nil {
				p.openOperationErr = err.Error()
			}
			_, err = info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/missing"}`))
			if err != nil {
				p.openUndeclared = err.Error()
			}
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				"cred_probe": {Tool: &credentialTool{probe: p}},
				"cred_hook":  {Preparation: p.prepare},
			}}, nil
		},
	}
}

func (p *credentialProbe) prepare(_ context.Context, _ extension.CallContext, input extension.PreparationInput) (extension.PreparationConsequence, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hookShell = input.Assembly.Shell
	p.hookCalls++
	return extension.PreparationConsequence{}, nil
}

type credentialTool struct{ probe *credentialProbe }

func (t *credentialTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func (t *credentialTool) Prepare(_ context.Context, _ extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "cred_probe"}}, Data: []byte(call.ID)}
}

func (t *credentialTool) Execute(ctx context.Context, tc extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	activation, err := tc.Calls.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/activation"}`))
	if err != nil {
		return credentialOutcome(string(plan.Data), "activation error: "+err.Error())
	}
	operation, err := tc.Calls.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
	if err != nil {
		return credentialOutcome(string(plan.Data), "operation error: "+err.Error())
	}
	t.probe.mu.Lock()
	combined := "activation=" + credentialString(activation) + "|operation=" + credentialString(operation)
	t.probe.toolValues = append(t.probe.toolValues, combined)
	t.probe.mu.Unlock()
	return credentialOutcome(string(plan.Data), combined)
}

func credentialString(result extension.Result) string {
	var value struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(result.Value, &value)
	return value.Value
}

func credentialOutcome(callID, content string) harness.ToolOutcome {
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: callID, Status: model.ResultSuccess, Content: content}}
}

func credentialConfigDocument(endpoint, activation, operation string) string {
	return `{"providers":{"prov":{"transport":{"base_url":"` + endpoint + `","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":262144}}}},"extensions":{"credprobe":{"enabled":true,"activation":` + fmt.Sprintf("%q", activation) + `,"operation":` + fmt.Sprintf("%q", operation) + `}}}`
}

func TestProjectedShellUsesTheOwnerEnvironmentProjection(t *testing.T) {
	t.Setenv("SHELL", "/bin/projected")
	t.Setenv("EXTENSION_SECRET", "secret")
	t.Setenv("KEEP", "value")
	h := newServiceHarness(t)
	svc := h.service(context.Background())

	svc.published.Store(&configuration{catalog: &catalog.Catalog{}})
	if got := projectedShell(context.Background(), svc.subprocessEnvironment); got != "/bin/projected" {
		t.Fatalf("projectedShell(owner projection) = %q, want the projected shell", got)
	}
	svc.published.Store(&configuration{catalog: &catalog.Catalog{Providers: map[string]*catalog.Provider{
		"p": {Transport: catalog.Transport{APIKeyEnv: "SHELL"}},
	}}})
	if got := projectedShell(context.Background(), svc.subprocessEnvironment); got != "unknown" {
		t.Fatalf("projectedShell(provider name) = %q, want unknown", got)
	}
	svc.published.Store(&configuration{catalog: &catalog.Catalog{}, secrets: []declaredSecret{{name: "EXTENSION_SECRET"}}})
	projected := subprocessEnvForTest(t, svc)
	testassert.Require(t, !slices.Contains(projected, "EXTENSION_SECRET=secret") && slices.Contains(projected, "SHELL=/bin/projected") && slices.Contains(projected, "KEEP=value"), "owner projection = %v, want the declared name removed and siblings kept", projected)
	svc.published.Store(&configuration{catalog: &catalog.Catalog{}})
	svc.holdCredentialNames([]string{"SHELL"})
	if got := projectedShell(context.Background(), svc.subprocessEnvironment); got != "unknown" {
		t.Fatalf("projectedShell(held name) = %q, want unknown", got)
	}
	svc.releaseCredentialNames([]string{"SHELL"})
	if got := projectedShell(context.Background(), svc.subprocessEnvironment); got != "/bin/projected" {
		t.Fatalf("projectedShell(released held name) = %q, want the projected shell", got)
	}
	t.Setenv("SHELL", "")
	if got := projectedShell(context.Background(), svc.subprocessEnvironment); got != "unknown" {
		t.Fatalf("projectedShell(empty shell) = %q, want unknown", got)
	}
}

func TestResolveDeclaredSecretsOnlyReadsActiveSections(t *testing.T) {
	native := extension.Native{Declaration: extension.Declaration{
		ID:      "ext",
		Version: "1",
		Secrets: []extension.SecretRef{
			{Field: "/activation", Lifetime: extension.SecretActivation},
			{Field: "/operation", Lifetime: extension.SecretOperation},
			{Field: "/absent", Lifetime: extension.SecretOperation},
		},
	}}
	resolved := resolveDeclaredSecrets([]extension.Native{native}, map[string]json.RawMessage{
		"ext": json.RawMessage(`{"enabled":true,"activation":"ACTIVATION_NAME","operation":"OPERATION_NAME"}`),
	})
	testassert.Require(t, len(resolved) == 2, "resolved secrets = %+v, want the two present fields", resolved)
	testassert.Equal(t, resolved[0].name, "ACTIVATION_NAME")
	testassert.Equal(t, resolved[1].name, "OPERATION_NAME")
	testassert.Require(t, len((&configuration{secrets: resolved}).declaredSecretNames()) == 2, "declared names did not dedupe the resolved references")
	if got := resolveDeclaredSecrets([]extension.Native{native}, map[string]json.RawMessage{
		"ext": json.RawMessage(`{"enabled":false,"activation":"ACTIVATION_NAME","operation":"OPERATION_NAME"}`),
	}); len(got) != 0 {
		t.Fatalf("inactive section resolved %+v, want nothing", got)
	}
	if got := resolveDeclaredSecrets([]extension.Native{native}, nil); len(got) != 0 {
		t.Fatalf("absent section resolved %+v, want nothing", got)
	}
}

func TestDeclaredCredentialsPromptAndChildEnvironment(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		activationName string
		shell          string
		wantPrompt     string
		wantChild      string
	}{
		{
			name:           "declared shell collision",
			activationName: "SHELL",
			shell:          "/bin/collision-shell",
			wantPrompt:     "unknown",
			wantChild:      "shell=|secret=|other=extension-only-secret",
		},
		{
			name:           "ordinary shell sibling",
			activationName: "LIGHTCODE_EXT_ONLY",
			shell:          "/bin/ordinary-shell",
			wantPrompt:     "/bin/ordinary-shell",
			wantChild:      "shell=/bin/ordinary-shell|secret=|other=extension-only-secret",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			e := newProductionEnv(t, storage.NewMemory())
			const (
				operationName = "LIGHTCODE_EXT_OPERATION"
				secretValue   = "extension-only-secret"
			)
			writeServiceFile(t, e.configPath, credentialConfigDocument(e.server.URL, testCase.activationName, operationName))
			t.Setenv("SHELL", testCase.shell)
			t.Setenv(testCase.activationName, secretValue)
			t.Setenv(operationName, "operation-secret")
			// The fixture's ordinary sibling carries the same bytes as the
			// secret value: exclusion is by declared name, never by scanning
			// arbitrary payload bytes.
			t.Setenv("LIGHTCODE_UNRELATED_PAYLOAD", secretValue)

			probe := &credentialProbe{}
			var steps atomic.Int64
			e.server.setScript(func(context.Context, string) []string {
				switch steps.Add(1) {
				case 1:
					return toolCallTurnEvents("cred-call", "cred_probe", `{}`)
				case 2:
					command := `printf 'shell=%s|secret=%s|other=%s' "$SHELL" "$` + testCase.activationName + `" "$LIGHTCODE_UNRELATED_PAYLOAD"`
					args, err := json.Marshal(map[string]any{"command": command})
					testassert.NoError(t, err)
					return toolCallTurnEvents("cmd-call", "run_command", string(args))
				default:
					return textTurnEvents("done")
				}
			})

			r, err := e.openPlugins(context.Background(), probe.native(t))
			testassert.NoError(t, err)
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			workspace := e.workspace("credentials-workspace")
			testassert.NoError(t, os.MkdirAll(workspace, 0o700))
			var session harness.SessionRecord
			err = r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
				var createErr error
				session, createErr = r.createSessionRecord(ctx, h, workspace, harness.AgentSelection{Inputs: &harness.AgentInputs{
					Model:        model.ModelRef{Provider: "prov", Model: "m"},
					AssemblyMode: "full",
					Tools:        []string{"cred_probe", "run_command"},
					Capabilities: []string{"cred_hook"},
				}})
				return createErr
			})
			testassert.NoError(t, err)
			submitThroughRuntime(t, r, session.Identity.SessionID, "op-cred", "hello")
			awaitOperation(t, r, session.Identity.SessionID, "op-cred", harness.OperationSuccess)

			probe.mu.Lock()
			defer probe.mu.Unlock()
			testassert.Equal(t, probe.openActivation, secretValue)
			testassert.Require(t, strings.Contains(probe.openOperationErr, "is unavailable"), "operation read during Open = %v, want the unavailable refusal", probe.openOperationErr)
			testassert.Require(t, strings.Contains(probe.openUndeclared, "is not declared"), "undeclared field read during Open = %v, want the declaration refusal", probe.openUndeclared)
			testassert.Require(t, probe.hookCalls == 1 && probe.hookShell == testCase.wantPrompt, "preparation hook = (%d, %q), want one call seeing %q", probe.hookCalls, probe.hookShell, testCase.wantPrompt)
			testassert.Require(t, len(probe.toolValues) == 1, "tool credential reads = %v, want one", probe.toolValues)
			testassert.Equal(t, probe.toolValues[0], "activation="+secretValue+"|operation=operation-secret")

			testassert.Require(t, e.server.requests() == 3, "model requests = %d, want tool, command and final turns", e.server.requests())
			for i := 0; i < e.server.requests(); i++ {
				body := e.server.bodyAt(i)
				testassert.Require(t, strings.Contains(body, "Shell: "+testCase.wantPrompt), "request %d prompt = %q, want the projected shell %q", i, body, testCase.wantPrompt)
			}
			testassert.Require(t, strings.Contains(e.server.bodyAt(1), "activation="+secretValue+"|operation=operation-secret"), "credential tool result = %q, want the activation and operation values", e.server.bodyAt(1))
			testassert.Require(t, strings.Contains(e.server.bodyAt(2), testCase.wantChild), "command result = %q, want %q", e.server.bodyAt(2), testCase.wantChild)
		})
	}
}

// TestDeclaredCredentialCapturesStayPerOperation proves two concurrent
// Operations keep the credential values captured at their own admission, and
// a reload that renames the declared field does not rewrite the old capture
// or lift its child-environment exclusion.
func TestDeclaredCredentialCapturesStayPerOperation(t *testing.T) {
	e := newProductionEnv(t, storage.NewMemory())
	const first = "LIGHTCODE_EXT_FIRST"
	const second = "LIGHTCODE_EXT_SECOND"
	writeServiceFile(t, e.configPath, credentialConfigDocument(e.server.URL, first, first))
	t.Setenv("SHELL", "/bin/capture-shell")
	t.Setenv(first, "first-secret")
	t.Setenv(second, "second-secret")

	release := make(chan struct{})
	probe := &credentialGateProbe{release: release}
	var steps atomic.Int64
	e.server.setScript(func(context.Context, string) []string {
		switch steps.Add(1) {
		case 1:
			return toolCallTurnEvents("cred-call", "cred_probe", `{"key":"first","park":true}`)
		case 2:
			return toolCallTurnEvents("cred-call", "cred_probe", `{"key":"changed","park":false}`)
		case 4:
			return toolCallTurnEvents("cred-call", "cred_probe", `{"key":"third","park":false}`)
		case 6:
			return toolCallTurnEvents("cmd-call", "run_command", `{"command":"printf 'first=%s|second=%s' \"$LIGHTCODE_EXT_FIRST\" \"$LIGHTCODE_EXT_SECOND\""}`)
		default:
			return textTurnEvents("done")
		}
	})
	r, err := e.openPlugins(context.Background(), probe.native(t))
	testassert.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	workspace := e.workspace("capture-workspace")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))

	create := func() harness.SessionRecord {
		t.Helper()
		var session harness.SessionRecord
		err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			var createErr error
			session, createErr = r.createSessionRecord(ctx, h, workspace, harness.AgentSelection{Inputs: &harness.AgentInputs{
				Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none",
				Tools: []string{"cred_probe", "run_command"}, Capabilities: []string{},
			}})
			return createErr
		})
		testassert.NoError(t, err)
		return session
	}
	firstSession := create()
	submitThroughRuntime(t, r, firstSession.Identity.SessionID, "op-first", "hello")
	firstResources := r.config.current().resources
	deadline := time.Now().Add(10 * time.Second)
	for {
		probe.mu.Lock()
		parked := probe.parked
		probe.mu.Unlock()
		if parked {
			break
		}
		testassert.Require(t, time.Now().Before(deadline), "the first credential read never parked")
		time.Sleep(5 * time.Millisecond)
	}

	// A changed value under the same name is observed at the next build.
	t.Setenv(first, "changed-secret")
	_, err = r.Reload(context.Background(), testCaller())
	testassert.NoError(t, err)
	secondSession := create()
	submitThroughRuntime(t, r, secondSession.Identity.SessionID, "op-second", "hello")
	awaitOperation(t, r, secondSession.Identity.SessionID, "op-second", harness.OperationSuccess)

	// Rename the declared field and retain the old variable: a reload's new
	// resource set captures the new name while the parked Operation keeps the
	// old name and value.
	writeServiceFile(t, e.configPath, credentialConfigDocument(e.server.URL, second, second))
	revision, err := r.Reload(context.Background(), testCaller())
	testassert.Require(t, err == nil && revision.Generation == "3", "Reload = (%q, %v), want 3", revision, err)
	testassert.Require(t, r.config.current().resources != firstResources, "the rotated declared binding reused the pre-reload resource set")
	thirdSession := create()
	submitThroughRuntime(t, r, thirdSession.Identity.SessionID, "op-third", "hello")
	awaitOperation(t, r, thirdSession.Identity.SessionID, "op-third", harness.OperationSuccess)
	close(release)
	awaitOperation(t, r, firstSession.Identity.SessionID, "op-first", harness.OperationSuccess)

	probe.mu.Lock()
	defer probe.mu.Unlock()
	// The first Operation's activation and Operation captures predate both the
	// value change and the rename. The second Operation's revision captures the
	// changed value; the third captures the reloaded name.
	testassert.Equal(t, probe.values["first"], "first-secret|first-secret")
	testassert.Equal(t, probe.values["changed"], "changed-secret|changed-secret")
	testassert.Equal(t, probe.values["third"], "second-secret|second-secret")
	// The first Operation's prepared exclusion still names the old variable
	// after the reload renamed it, and the live revision adds the new one.
	testassert.Require(t, e.server.requests() == 7, "model requests = %d, want three probes and four final turns", e.server.requests())
	testassert.Require(t, strings.Contains(e.server.bodyAt(6), "first=|second="), "renamed credential command = %q, want both declared names excluded", e.server.bodyAt(6))
}

// credentialGateProbe is one Harness-scoped tool that captures the declared
// credentials and can park before reading them.
type credentialGateProbe struct {
	mu      sync.Mutex
	release chan struct{}

	parked bool
	values map[string]string
}

func (p *credentialGateProbe) native(t *testing.T) extension.Native {
	return p.nativeFor(t, "credprobe", "cred_probe")
}

func (p *credentialGateProbe) nativeFor(t *testing.T, id, toolID string) extension.Native {
	t.Helper()
	host, ok := extension.HostCapability(extension.ContractCredentials)
	testassert.Require(t, ok, "the credentials host contract is not defined")
	toolCapability := extension.ToolCapability(toolID)
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           id,
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"activation":{"type":"string"},"operation":{"type":"string"}},"required":["enabled"]}`),
			Secrets: []extension.SecretRef{
				{Field: "/activation", Lifetime: extension.SecretActivation},
				{Field: "/operation", Lifetime: extension.SecretOperation},
			},
			Components: []extension.Component{{
				ID:       id + "-c",
				Lifetime: extension.LifetimeHarness,
				Provides: []string{toolID},
				Requires: []extension.Binding{{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: host}},
			}},
			Capabilities: []extension.Capability{toolCapability},
		},
		Describe: map[string]extension.ToolDescribe{toolID: func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
			return extension.ToolDescription{Definition: model.ToolDefinition{Name: toolID, Description: "reads the captured credentials", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}, Available: true}, nil
		}},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{toolID: {Tool: &credentialGateTool{probe: p}}}}, nil
		},
	}
}

type credentialGateTool struct{ probe *credentialGateProbe }

func (t *credentialGateTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func (t *credentialGateTool) Prepare(_ context.Context, _ extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	data, _ := json.Marshal(struct {
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
	}{CallID: call.ID, Arguments: call.Arguments})
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "cred_probe"}}, Data: data}
}

func (t *credentialGateTool) Execute(ctx context.Context, tc extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	var prepared struct {
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(plan.Data, &prepared)
	var request struct {
		Key  string `json:"key"`
		Park bool   `json:"park"`
	}
	_ = json.Unmarshal(prepared.Arguments, &request)
	if request.Park {
		t.probe.mu.Lock()
		t.probe.parked = true
		t.probe.mu.Unlock()
		<-t.probe.release
	}
	activation, err := tc.Calls.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/activation"}`))
	if err != nil {
		return credentialOutcome(prepared.CallID, "activation error: "+err.Error())
	}
	operation, err := tc.Calls.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/operation"}`))
	if err != nil {
		return credentialOutcome(prepared.CallID, "operation error: "+err.Error())
	}
	t.probe.mu.Lock()
	if t.probe.values == nil {
		t.probe.values = make(map[string]string)
	}
	t.probe.values[request.Key] = credentialString(activation) + "|" + credentialString(operation)
	t.probe.mu.Unlock()
	return credentialOutcome(prepared.CallID, "probed")
}

// captureValueForTest returns one managed file value.
func captureValueForTest(t *testing.T, managed *config.ManagedEnv, name string) string {
	t.Helper()
	captured, err := managed.Capture([]string{name})
	testassert.NoError(t, err)
	return captured[name].Value
}
