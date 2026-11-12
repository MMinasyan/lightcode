package melta

// The owner-wide held credential-name fixtures: a prepared Session's capture
// and its live resource set keep captured provider/declared names excluded for
// every command in the Melta, including while a reload renames or rotates
// them. Names stop being protected only after the last capture/resource
// lifetime and its cleanup finish.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Syntheresis/melta/extension"
	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/storage"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/model"
)

type heldCredentialTool struct {
	fields  []string
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu     sync.Mutex
	values map[string]string
}

func (t *heldCredentialTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return call.Arguments, nil
}

func (t *heldCredentialTool) Prepare(_ context.Context, _ extension.ToolContext, call model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Permissions: []harness.PermissionRequest{{Permission: harness.PermissionCommandRun, Target: "held"}}, Data: []byte(call.ID)}
}

func (t *heldCredentialTool) Execute(ctx context.Context, tc extension.ToolContext, plan extension.ToolPlan) harness.ToolOutcome {
	callID := string(plan.Data)
	for _, field := range t.fields {
		result, err := tc.Calls.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"`+field+`"}`))
		if err != nil {
			return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: err.Error()}}
		}
		value := credentialString(result)
		t.mu.Lock()
		if t.values == nil {
			t.values = make(map[string]string)
		}
		t.values[field] = value
		t.mu.Unlock()
	}
	if t.started != nil {
		t.once.Do(func() { close(t.started) })
	}
	if t.release != nil {
		select {
		case <-t.release:
		case <-ctx.Done():
			return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: callID, Status: model.ResultError, Content: ctx.Err().Error()}}
		}
	}
	return credentialOutcome(callID, "held")
}

func (t *heldCredentialTool) value(field string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.values[field]
}

func heldCredentialNative(t *testing.T, id string, secrets []extension.SecretRef, toolID string, tool *heldCredentialTool) extension.Native {
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
				Lifetime: extension.LifetimeHarness,
				Provides: []string{toolID},
				Requires: []extension.Binding{{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: host}},
			}},
			Capabilities: []extension.Capability{extension.ToolCapability(toolID)},
		},
		Describe: map[string]extension.ToolDescribe{toolID: func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
			return extension.ToolDescription{Definition: model.ToolDefinition{Name: toolID, Parameters: json.RawMessage(`{"type":"object"}`)}, Available: true}, nil
		}},
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{toolID: {Tool: tool}}}, nil
		},
	}
}

func heldCredentialConfig(endpoint string, extensions string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, "", `{"m":{"name":"M","context_window":4096}}`) + `},"extensions":` + extensions + `}`
}

func heldProviderConfig(endpoint, keyEnv string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, keyEnv, `{"m":{"name":"M","context_window":4096}}`) + `},"extensions":{"credhold":{"enabled":true}}}`
}

func createHeldSession(t *testing.T, r *Melta, workspace string, tools []string) harness.SessionRecord {
	t.Helper()
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	var session harness.SessionRecord
	err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var createErr error
		session, createErr = r.createSessionRecord(ctx, h, workspace, harness.AgentSelection{Inputs: &harness.AgentInputs{
			Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "simple", Tools: tools,
		}})
		return createErr
	})
	testassert.NoError(t, err)
	return session
}

func awaitHeldGone(t *testing.T, svc *configurationService, names ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		held := svc.heldCredentialNames()
		gone := true
		for _, name := range names {
			if slices.Contains(held, name) {
				gone = false
				break
			}
		}
		if gone {
			return
		}
		testassert.Require(t, time.Now().Before(deadline), "held names %v still contain %v", held, names)
		time.Sleep(5 * time.Millisecond)
	}
}

func commandOutput(body string) string {
	if i := strings.LastIndex(body, "shell="); i >= 0 {
		return body[i:]
	}
	if i := strings.LastIndex(body, "old="); i >= 0 {
		return body[i:]
	}
	return ""
}

func findRequestByUser(t *testing.T, server *productionModelServer, user, role string) string {
	t.Helper()
	for i := 0; i < server.requests(); i++ {
		body := server.bodyAt(i)
		if lastUserText(body) == user && lastMessageRole(body) == role {
			return body
		}
	}
	t.Fatalf("no model request for user %q with last role %q", user, role)
	return ""
}

// awaitBackgroundCompletionBody waits for the Melta-delivered background
// completion record, then returns that complete body; content correctness is
// asserted by the caller after completion.
func awaitBackgroundCompletionBody(t *testing.T, server *productionModelServer) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for i := 0; i < server.requests(); i++ {
			body := server.bodyAt(i)
			if strings.Contains(body, "Background process") && strings.Contains(body, "finished:") {
				return body
			}
		}
		testassert.Require(t, time.Now().Before(deadline), "background completion never arrived; requests=%d", server.requests())
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSubprocessEnvironmentUsesCurrentRevision proves the child-environment
// projection reads the current provider names from the published revision and
// excludes them without a source call or a reader-visible lock.
func TestSubprocessEnvironmentUsesCurrentRevision(t *testing.T) {
	const oldName = "LIGHTCODE_COHERENCE_OLD"
	const newName = "LIGHTCODE_COHERENCE_NEW"
	t.Setenv(oldName, "old-value")
	t.Setenv(newName, "new-value")
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"providers":{"prov":`+nominalProvMember("http://prov.test/v1", oldName, `{"m":{"name":"M","context_window":4096}}`)+`}}`)
	svc := h.preparedService(context.Background())
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()
	testassert.Require(t, !slices.Contains(subprocessEnvForTest(t, svc), oldName+"=old-value"), "initial owner projection kept the current provider name")

	writeServiceFile(t, h.configPath, `{"providers":{"prov":`+nominalProvMember("http://prov.test/v1", newName, `{"m":{"name":"M","context_window":4096}}`)+`}}`)
	_, err = svc.publish(context.Background())
	testassert.NoError(t, err)
	env := subprocessEnvForTest(t, svc)
	testassert.Require(t, !slices.Contains(env, newName+"=new-value"), "owner projection kept the current provider name: %v", env)
	testassert.Require(t, slices.Contains(env, oldName+"=old-value"), "owner projection dropped the released sibling: %v", env)
}

// TestBlockedCleanupDoesNotHoldProviderNames proves a cleanup resource
// reservation is not an admission: while cleanup is blocked, a provider
// rename from SHELL must leave ordinary SHELL available to a new Session's
// prompt and command, while the current provider name remains excluded.
func TestBlockedCleanupDoesNotHoldProviderNames(t *testing.T) {
	const (
		shellName    = "SHELL"
		shellValue   = "/bin/cleanup-shell"
		newProvider  = "LIGHTCODE_CLEANUP_NEW_PROVIDER"
		declaredName = "LIGHTCODE_CLEANUP_DECLARED"
	)
	t.Setenv(shellName, shellValue)
	t.Setenv(newProvider, "new-provider-value")
	t.Setenv(declaredName, "declared-value")
	e := newOwnerEnv(t)
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	once := sync.OnceFunc(func() { close(release) })
	defer once()
	probe := newDataProbe("probe_park", func(context.Context, extension.CallContext) error {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		return nil
	})
	e.fixtureStore(storage.NewMemory())
	writeServiceFile(t, e.configPath, cleanupProviderConfig(e.server.URL, shellName, declaredName))
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		if lastUserText(body) == "command" {
			args, _ := json.Marshal(map[string]any{"command": `printf 'shell=%s|provider=%s|declared=%s' "$SHELL" "$` + newProvider + `" "$` + declaredName + `"`})
			return toolCallTurnEvents("cleanup-call", "run_command", string(args))
		}
		return textTurnEvents("done")
	})
	ctx := context.Background()
	r, err := e.open(ctx, probe.native)
	testassert.NoError(t, err)
	defer func() {
		once()
		_ = r.Close(context.Background())
	}()

	oldSession, err := r.createSession(ctx, filepath.Join(e.dataDir, "cleanup-old"), "runner")
	testassert.NoError(t, err)
	view, calls := dataView(t, r, probe.capability, oldSession.Identity.SessionID, nativeGrant{})
	_, err = callData(t, calls, "claim_external_data")
	testassert.NoError(t, err)
	view.end()
	_, err = r.ArchiveSession(ctx, testCaller(r), oldSession.Identity.SessionID)
	testassert.NoError(t, err)

	deleteDone := make(chan error, 1)
	go func() {
		_, deleteErr := r.DeleteSession(ctx, testCaller(r), oldSession.Identity.SessionID)
		deleteDone <- deleteErr
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup callback never started")
	}
	testassert.Require(t, !slices.Contains(r.config.heldCredentialNames(), shellName), "cleanup resource reservation held provider name %q: %v", shellName, r.config.heldCredentialNames())

	writeServiceFile(t, e.configPath, cleanupProviderConfig(e.server.URL, newProvider, declaredName))
	_, err = r.Reload(ctx, testCaller(r))
	testassert.NoError(t, err)

	workspace := filepath.Join(e.dataDir, "cleanup-new")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	newSession, err := r.createSession(ctx, workspace, "runner")
	testassert.NoError(t, err)
	submitThroughRuntime(t, r, newSession.Identity.SessionID, "op-cleanup", "command")
	awaitOperation(t, r, newSession.Identity.SessionID, "op-cleanup", harness.OperationSuccess)
	testassert.Require(t, strings.Contains(findRequestByUser(t, e.server, "command", "user"), "Shell: "+shellValue), "new Session prompt did not preserve ordinary SHELL")
	output := commandOutput(findRequestByUser(t, e.server, "command", "tool"))
	testassert.Require(t, strings.Contains(output, "shell="+shellValue+"|provider=|declared="), "new Session command = %q, want ordinary SHELL visible and current provider/declared names excluded", output)

	once()
	select {
	case deleteErr := <-deleteDone:
		testassert.NoError(t, deleteErr)
	case <-time.After(10 * time.Second):
		t.Fatal("blocked cleanup did not finish after release")
	}
}

// TestCandidateResourceHoldsNamesBeforeOpen proves a candidate resource set
// registers its declared names before any author Open, retains them through
// failed-prefix cleanup, and releases them when the candidate unwinds without
// publication.
func TestCandidateResourceHoldsNamesBeforeOpen(t *testing.T) {
	const name = "LIGHTCODE_CANDIDATE_OPEN"
	t.Setenv(name, "candidate-open-value")
	h := newServiceHarness(t)
	var parkOpen, failSibling, parkCleanup atomic.Bool
	var parkedValue string
	var openEntered, cleanupEntered = make(chan struct{}), make(chan struct{})
	var openRelease, cleanupRelease = make(chan struct{}), make(chan struct{})
	var releaseOpenOnce, releaseCleanupOnce sync.Once
	releaseOpen := func() { releaseOpenOnce.Do(func() { close(openRelease) }) }
	releaseCleanup := func() { releaseCleanupOnce.Do(func() { close(cleanupRelease) }) }
	t.Cleanup(func() {
		parkCleanup.Store(false)
		releaseOpen()
		releaseCleanup()
	})
	host, ok := extension.HostCapability(extension.ContractCredentials)
	testassert.Require(t, ok, "the credentials host contract is not defined")
	native := extension.Native{
		Declaration: extension.Declaration{
			ID:           "credlife",
			Version:      "1",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"activation":{"type":"string"}},"required":["enabled"]}`),
			Secrets:      []extension.SecretRef{{Field: "/activation", Lifetime: extension.SecretActivation}},
			Components: []extension.Component{
				{ID: "cred-open", Lifetime: extension.LifetimeHarness, Requires: []extension.Binding{{Name: "credentials", Provider: extension.HostProvider, CapabilityID: extension.ContractCredentials, Definition: host}}},
				{ID: "cred-fail", Lifetime: extension.LifetimeHarness},
			},
		},
		Open: func(ctx context.Context, info extension.InstanceContext) (extension.Instance, error) {
			switch info.ComponentID {
			case "cred-open":
				if parkOpen.Load() {
					result, err := info.Call(ctx, "credentials", "get", json.RawMessage(`{"field":"/activation"}`))
					if err != nil {
						return extension.Instance{}, err
					}
					parkedValue = credentialString(result)
					close(openEntered)
					<-openRelease
				}
				return extension.Instance{Close: func(context.Context, extension.CallContext) error {
					if parkCleanup.Load() {
						close(cleanupEntered)
						<-cleanupRelease
					}
					return nil
				}}, nil
			case "cred-fail":
				if failSibling.Load() {
					return extension.Instance{}, errors.New("candidate sibling open failure")
				}
			}
			return extension.Instance{}, nil
		},
	}
	svc := h.preparedService(context.Background(), native)
	writeServiceFile(t, h.configPath, credentialServiceConfig(`{"credlife":{"enabled":true}}`))
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()
	testassert.Require(t, slices.Contains(subprocessEnvForTest(t, svc), name+"=candidate-open-value"), "initial projection did not expose the ordinary name")

	parkOpen.Store(true)
	failSibling.Store(true)
	parkCleanup.Store(true)
	buildDone := make(chan error, 1)
	go func() {
		_, buildErr := svc.buildCaptured(context.Background(), 2, []byte(credentialServiceConfig(`{"credlife":{"enabled":true,"activation":"`+name+`"}}`)), nil, nil)
		buildDone <- buildErr
	}()
	select {
	case <-openEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("candidate author Open never entered")
	}
	testassert.Equal(t, parkedValue, "candidate-open-value")
	testassert.Require(t, !slices.Contains(subprocessEnvForTest(t, svc), name+"=candidate-open-value"), "name was not held while the candidate author Open was parked")
	releaseOpen()
	select {
	case <-cleanupEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("candidate prefix cleanup never entered")
	}
	testassert.Require(t, !slices.Contains(subprocessEnvForTest(t, svc), name+"=candidate-open-value"), "name was not held during candidate prefix cleanup")
	releaseCleanup()
	parkCleanup.Store(false)
	testassert.Require(t, <-buildDone != nil, "candidate with a failing sibling succeeded")
	testassert.Require(t, slices.Contains(subprocessEnvForTest(t, svc), name+"=candidate-open-value"), "name stayed held after an unpublished candidate unwound")
}

func cleanupProviderConfig(endpoint, keyEnv, declaredName string) string {
	return `{"providers":{"prov":` + nominalProvMember(endpoint, keyEnv, `{"m":{"name":"M","context_window":4096}}`) + `},"extensions":{"agents":{"enabled":true,"definitions":{"runner":{"model":"prov/m","system_prompt":"simple","tools":["run_command"]}}},"probe_park":{"enabled":true,"value":"` + declaredName + `"}}}`
}

func TestHeldCredentialNamesProtectOtherSessionsAfterReload(t *testing.T) {
	const (
		oldName    = "LIGHTCODE_HELD_OLD"
		newName    = "LIGHTCODE_HELD_NEW"
		oldShell   = "SHELL"
		newShell   = "LIGHTCODE_HELD_NEW_SHELL"
		unrelated  = "LIGHTCODE_HELD_UNRELATED"
		shellValue = "/bin/held-shell"
		oldValue   = "held-old-value"
		newValue   = "held-new-value"
		unrelatedV = "unrelated-value"
	)
	t.Setenv(oldShell, shellValue)
	t.Setenv(oldName, oldValue)
	t.Setenv(newName, newValue)
	t.Setenv(unrelated, unrelatedV)

	e := newProductionEnv(t, storage.NewMemory())
	hold := &heldCredentialTool{fields: []string{"/activation", "/operation"}, started: make(chan struct{}), release: make(chan struct{})}
	native := heldCredentialNative(t, "credhold", []extension.SecretRef{
		{Field: "/activation", Lifetime: extension.SecretActivation},
		{Field: "/operation", Lifetime: extension.SecretOperation},
	}, "cred_hold", hold)
	writeServiceFile(t, e.configPath, heldCredentialConfig(e.server.URL, `{"credhold":{"enabled":true,"activation":"`+oldShell+`","operation":"`+oldName+`"}}`))
	command := `printf 'shell=%s|old=%s|new=%s|unrelated=%s' "$` + oldShell + `" "$` + oldName + `" "$` + newName + `" "$` + unrelated + `"`
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		switch lastUserText(body) {
		case "hold":
			return toolCallTurnEvents("hold-call", "cred_hold", `{}`)
		case "bg":
			args, _ := json.Marshal(map[string]any{"command": command, "background": true})
			return toolCallTurnEvents("bg-call", "run_command", string(args))
		case "fg", "after":
			args, _ := json.Marshal(map[string]any{"command": command})
			return toolCallTurnEvents("fg-call", "run_command", string(args))
		default:
			return textTurnEvents("done")
		}
	})
	r, err := e.openPlugins(context.Background(), native)
	testassert.NoError(t, err)
	defer func() { _ = r.Close(context.Background()) }()

	first := createHeldSession(t, r, e.workspace("held-a"), []string{"cred_hold"})
	submitThroughRuntime(t, r, first.Identity.SessionID, "op-hold", "hold")
	select {
	case <-hold.started:
	case <-time.After(5 * time.Second):
		t.Fatalf("held tool never started; requests=%d bodies=%v", e.server.requests(), func() []string {
			var out []string
			for i := 0; i < e.server.requests(); i++ {
				out = append(out, e.server.bodyAt(i))
			}
			return out
		}())
	}

	writeServiceFile(t, e.configPath, heldCredentialConfig(e.server.URL, `{"credhold":{"enabled":true,"activation":"`+newShell+`","operation":"`+newName+`"}}`))
	_, err = r.Reload(context.Background(), testCaller(r))
	testassert.NoError(t, err)

	second := createHeldSession(t, r, e.workspace("held-b"), []string{"run_command"})
	submitThroughRuntime(t, r, second.Identity.SessionID, "op-foreground", "fg")
	awaitOperation(t, r, second.Identity.SessionID, "op-foreground", harness.OperationSuccess)
	testassert.Require(t, strings.Contains(findRequestByUser(t, e.server, "fg", "user"), "Shell: unknown"), "foreground prompt, want the held SHELL filtered; held=%v", r.config.heldCredentialNames())
	foregroundOutput := commandOutput(findRequestByUser(t, e.server, "fg", "tool"))
	testassert.Require(t, strings.Contains(foregroundOutput, "shell=|old=|new=|unrelated="+unrelatedV), "foreground output = %q, want old/new/shell absent and the unrelated sibling preserved", foregroundOutput)

	backgroundSession := createHeldSession(t, r, e.workspace("held-bg"), []string{"run_command"})
	submitThroughRuntime(t, r, backgroundSession.Identity.SessionID, "op-background", "bg")
	awaitOperation(t, r, backgroundSession.Identity.SessionID, "op-background", harness.OperationSuccess)
	testassert.Require(t, strings.Contains(findRequestByUser(t, e.server, "bg", "user"), "Shell: unknown"), "background prompt, want the held SHELL filtered")
	backgroundOutput := awaitBackgroundCompletionBody(t, e.server)
	testassert.Require(t, strings.Contains(backgroundOutput, "shell=|old=|new=|unrelated="+unrelatedV), "background completion = %q, want old/new/shell absent and the unrelated sibling preserved", backgroundOutput)

	close(hold.release)
	awaitOperation(t, r, first.Identity.SessionID, "op-hold", harness.OperationSuccess)
	testassert.Equal(t, hold.value("/operation"), oldValue)
	testassert.Equal(t, hold.value("/activation"), shellValue)
	awaitHeldGone(t, r.config, oldShell, oldName)

	after := createHeldSession(t, r, e.workspace("held-c"), []string{"run_command"})
	submitThroughRuntime(t, r, after.Identity.SessionID, "op-after", "after")
	awaitOperation(t, r, after.Identity.SessionID, "op-after", harness.OperationSuccess)
	testassert.Require(t, strings.Contains(findRequestByUser(t, e.server, "after", "user"), "Shell: "+shellValue), "after prompt, want the released SHELL visible")
	afterOutput := commandOutput(findRequestByUser(t, e.server, "after", "tool"))
	testassert.Require(t, strings.Contains(afterOutput, "shell="+shellValue+"|old="+oldValue+"|new=|unrelated="+unrelatedV), "after output = %q, want released old/shell visible and the still-current new name excluded", afterOutput)
}

func TestProviderOnlyRenameUsesAdoptedResourcesAndHeldNames(t *testing.T) {
	const (
		oldProvider = "LIGHTCODE_HELD_PROVIDER_OLD"
		newProvider = "LIGHTCODE_HELD_PROVIDER_NEW"
		unrelated   = "LIGHTCODE_HELD_PROVIDER_UNRELATED"
	)
	t.Setenv(oldProvider, "old-provider-value")
	t.Setenv(newProvider, "new-provider-value")
	t.Setenv(unrelated, "provider-unrelated")

	e := newProductionEnv(t, storage.NewMemory())
	hold := &heldCredentialTool{started: make(chan struct{}), release: make(chan struct{})}
	native := heldCredentialNative(t, "credhold", nil, "cred_hold", hold)
	writeServiceFile(t, e.configPath, heldProviderConfig(e.server.URL, oldProvider))
	command := `printf 'shell=%s|old=%s|new=%s|unrelated=%s' "$SHELL" "$` + oldProvider + `" "$` + newProvider + `" "$` + unrelated + `"`
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return textTurnEvents("done")
		}
		if lastUserText(body) == "hold" {
			return toolCallTurnEvents("hold-call", "cred_hold", `{}`)
		}
		args, _ := json.Marshal(map[string]any{"command": command})
		return toolCallTurnEvents("fg-call", "run_command", string(args))
	})
	r, err := e.openPlugins(context.Background(), native)
	testassert.NoError(t, err)
	defer func() { _ = r.Close(context.Background()) }()

	first := createHeldSession(t, r, e.workspace("provider-a"), []string{"cred_hold"})
	submitThroughRuntime(t, r, first.Identity.SessionID, "op-hold", "hold")
	select {
	case <-hold.started:
	case <-time.After(5 * time.Second):
		t.Fatalf("held tool never started; requests=%d bodies=%v", e.server.requests(), func() []string {
			var out []string
			for i := 0; i < e.server.requests(); i++ {
				out = append(out, e.server.bodyAt(i))
			}
			return out
		}())
	}
	resources := r.config.current().resources

	writeServiceFile(t, e.configPath, heldProviderConfig(e.server.URL, newProvider))
	_, err = r.Reload(context.Background(), testCaller(r))
	testassert.NoError(t, err)
	testassert.Require(t, r.config.current().resources == resources, "provider-only rename did not adopt the existing resource set")

	second := createHeldSession(t, r, e.workspace("provider-b"), []string{"run_command"})
	submitThroughRuntime(t, r, second.Identity.SessionID, "op-provider", "fg")
	awaitOperation(t, r, second.Identity.SessionID, "op-provider", harness.OperationSuccess)
	output := commandOutput(findRequestByUser(t, e.server, "fg", "tool"))
	testassert.Require(t, strings.Contains(output, "|old=|new=|unrelated=provider-unrelated"), "provider-only output = %q, want old/new absent and the unrelated sibling preserved", output)

	close(hold.release)
	awaitOperation(t, r, first.Identity.SessionID, "op-hold", harness.OperationSuccess)
	awaitHeldGone(t, r.config, oldProvider)

	after := createHeldSession(t, r, e.workspace("provider-c"), []string{"run_command"})
	submitThroughRuntime(t, r, after.Identity.SessionID, "op-provider-after", "after")
	awaitOperation(t, r, after.Identity.SessionID, "op-provider-after", harness.OperationSuccess)
	afterOutput := commandOutput(findRequestByUser(t, e.server, "after", "tool"))
	testassert.Require(t, strings.Contains(afterOutput, "|old=old-provider-value|new=|unrelated=provider-unrelated"), "provider-only after output = %q, want the released provider name visible", afterOutput)
}

func TestFailedPreparationReleasesHeldCredentialNames(t *testing.T) {
	const name = "LIGHTCODE_FAILED_CAPTURE"
	t.Setenv(name, "failed-value")
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"providers":{"prov":`+nominalProvMember("http://prov.test/v1", name, `{"m":{"name":"M","context_window":4096}}`)+`}}`)
	svc := h.preparedService(context.Background())
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()
	testassert.Require(t, !slices.Contains(svc.heldCredentialNames(), name), "published provider name was held without a reserving capture")

	p := newPreparation(svc, h.locations(), "", nil, newObservationAdapter(newObservation(), newWarningStore()), nil, nil, nil)
	_, err = p.bind()(context.Background(), harness.PreparationRequest{Session: harness.PreparationSession{Identity: harness.SessionIdentity{SessionID: testSessionID, Workspace: h.dataDir}}})
	testassert.Require(t, err != nil, "discarded preparation without a selection succeeded")
	testassert.Require(t, !slices.Contains(svc.heldCredentialNames(), name), "discarded preparation leaked held name %q: %v", name, svc.heldCredentialNames())
}

// definedOnlyCredentials is the sparse credential source: Capture returns
// only the requested names that are actually defined.
type definedOnlyCredentials struct {
	CredentialSource
}

func (s definedOnlyCredentials) Capture(ctx context.Context, names []string) (map[string]CredentialValue, error) {
	captured, err := s.CredentialSource.Capture(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]CredentialValue, len(captured))
	for name, value := range captured {
		if value.Defined {
			out[name] = value
		}
	}
	return out, nil
}

// TestSparseCredentialCaptureHoldsRequestedNames proves a preparation holds
// every name its build requested — not only the sparse source's returned keys
// — so a later revision that no longer references the name cannot expose it to
// child processes while the preparation is live.
func TestSparseCredentialCaptureHoldsRequestedNames(t *testing.T) {
	const name = "LIGHTCODE_SPARSE_REQUESTED"
	t.Setenv(name, "")
	_ = os.Unsetenv(name)
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"providers":{"prov":`+nominalProvMember("http://prov.test/v1", name, `{"m":{"name":"M","context_window":4096}}`)+`}}`)
	svc := h.preparedService(context.Background())
	svc.credentials = definedOnlyCredentials{CredentialSource: svc.credentials}
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()

	captured, err := svc.capturePreparation(context.Background())
	testassert.NoError(t, err)
	defer captured.releaseHeldNames()
	testassert.Require(t, slices.Contains(svc.heldCredentialNames(), name), "sparse capture left requested name %q unheld: %v", name, svc.heldCredentialNames())

	t.Setenv(name, "later-value")
	writeServiceFile(t, h.configPath, `{}`)
	_, err = svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()
	env := subprocessEnvForTest(t, svc)
	testassert.Require(t, !slices.Contains(env, name+"=later-value"), "a later revision exposed the held name to children: %v", env)
}

// subprocessEnvForTest resolves the owner's child-environment projection.
func subprocessEnvForTest(t *testing.T, svc *configurationService) []string {
	t.Helper()
	env, err := svc.subprocessEnvironment(context.Background())
	testassert.NoError(t, err)
	return env
}
