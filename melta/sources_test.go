package melta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/config"
	"github.com/Syntheresis/melta/internal/storage"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/model"
	"github.com/Syntheresis/melta/protocol"
)

// memoryCredentialSource is the in-memory credential fixture: captured values
// by name, an owned update map, and optional capture blocking/failure.
type memoryCredentialSource struct {
	mu      sync.Mutex
	values  map[string]CredentialValue
	events  *[]string
	block   chan struct{}
	entered chan struct{}
	fail    bool
}

func (s *memoryCredentialSource) Capture(ctx context.Context, names []string) (map[string]CredentialValue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	block, entered, fail := s.block, s.entered, s.fail
	out := make(map[string]CredentialValue, len(names))
	for _, name := range names {
		out[name] = s.values[name]
	}
	if s.events != nil {
		*s.events = append(*s.events, "capture")
	}
	s.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if fail {
		return nil, errors.New("injected capture failure")
	}
	return out, nil
}

func (s *memoryCredentialSource) Set(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = CredentialValue{Value: value, Defined: true, Managed: true}
	if s.events != nil {
		*s.events = append(*s.events, "credential_set")
	}
	return nil
}

func (s *memoryCredentialSource) Remove(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = CredentialValue{}
	if s.events != nil {
		*s.events = append(*s.events, "credential_remove")
	}
	return nil
}

func (s *memoryCredentialSource) SubprocessEnv(context.Context, []string) ([]string, error) {
	return []string{"KEEP=value"}, nil
}

// recordingConfigurationSource records document writes.
type recordingConfigurationSource struct {
	inner  ConfigurationSource
	mu     sync.Mutex
	events *[]string
}

func (s *recordingConfigurationSource) Read(ctx context.Context) ([]byte, error) {
	return s.inner.Read(ctx)
}

func (s *recordingConfigurationSource) Write(ctx context.Context, data []byte) error {
	if err := s.inner.Write(ctx, data); err != nil {
		return err
	}
	s.mu.Lock()
	if s.events != nil {
		*s.events = append(*s.events, "config_write")
	}
	s.mu.Unlock()
	return nil
}

// sourceConfigDocument is one complete provider document over a keyless
// endpoint and a named credential.
func sourceConfigDocument(endpoint, keyEnv, name string) []byte {
	return []byte(fmt.Sprintf(`{"providers":{"prov":{"transport":{"base_url":%q,"api_key_env":%q},"discovery":false,"models":{"m":{"name":%q,"context_window":65536}}}}}`,
		endpoint, keyEnv, name))
}

// TestRuntimeValueConfigurationAndUnusableHome constructs the owner with the
// value configuration source, an in-memory credential source, all locations
// empty and HOME pointing at a nonexistent directory; then writes the value
// document and admits with the captured credential.
func TestRuntimeValueConfigurationAndUnusableHome(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "missing-home"))
	const keyEnv = "VALUE_SOURCE_KEY"
	const key = "value-source-secret"
	server := newProductionModelServer(t, "", "")
	value := NewValueConfigurationSource(sourceConfigDocument(server.URL, keyEnv, "Before"))
	credentials := &memoryCredentialSource{values: map[string]CredentialValue{keyEnv: {Value: key, Defined: true, Managed: true}}}
	dataDir := t.TempDir()
	r, err := Open(context.Background(), Options{
		DataDir:       dataDir,
		Configuration: value,
		Credentials:   credentials,
	})
	testassert.NoError(t, err)
	defer func() {
		{
			err := r.Close(context.Background())
			testassert.NoError(t, err)
		}
	}()
	{
		data, err := value.Read(context.Background())
		testassert.NoError(t, err)
		testassert.Equal(t, string(data), string(sourceConfigDocument(server.URL, keyEnv, "Before")))
	}

	updated := sourceConfigDocument(server.URL, keyEnv, "After")
	testassert.NoError(t, value.Write(context.Background(), updated))
	{
		_, err := r.Reload(context.Background(), testCaller(r))
		testassert.NoError(t, err)
	}
	{
		data, err := value.Read(context.Background())
		testassert.NoError(t, err)
		testassert.Equal(t, string(data), string(updated))
	}

	workspace := t.TempDir()
	var record harness.SessionRecord
	testassert.NoError(t, r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		record, err = r.createSessionRecord(ctx, h, workspace, harness.AgentSelection{Inputs: &harness.AgentInputs{
			Model:        model.ModelRef{Provider: "prov", Model: "m"},
			AssemblyMode: "none",
		}})
		return err
	}))
	submitThroughRuntime(t, r, record.Identity.SessionID, "op-value-source", "hello")
	awaitOperation(t, r, record.Identity.SessionID, "op-value-source", harness.OperationSuccess)
	{
		got := server.authAt(0)
		testassert.Equal(t, got, "Bearer "+key)
	}
}

// TestCredentialCaptureBlockingAndFailingCandidates proves a blocking capture
// only parks its own candidate while unrelated execution proceeds, and a
// failing capture fails only that candidate with the previous revision kept.
func TestCredentialCaptureBlockingAndFailingCandidates(t *testing.T) {
	server := newProductionModelServer(t, "", "")
	const keyEnv = "SCRIPTED_CAPTURE_KEY"
	value := NewValueConfigurationSource(sourceConfigDocument(server.URL, keyEnv, "One"))
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	credentials := &memoryCredentialSource{
		values: map[string]CredentialValue{keyEnv: {Value: "scripted-secret", Defined: true, Managed: true}},
	}
	r, err := Open(context.Background(), Options{DataDir: t.TempDir(), Configuration: value, Credentials: credentials})
	testassert.NoError(t, err)
	defer func() { _ = r.Close(context.Background()) }()
	workspace := t.TempDir()
	var record harness.SessionRecord
	testassert.NoError(t, r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		var err error
		record, err = r.createSessionRecord(ctx, h, workspace, harness.AgentSelection{Inputs: &harness.AgentInputs{
			Model:        model.ModelRef{Provider: "prov", Model: "m"},
			AssemblyMode: "none",
		}})
		return err
	}))

	credentials.mu.Lock()
	credentials.block, credentials.entered = block, entered
	credentials.mu.Unlock()
	reloadDone := make(chan error, 1)
	go func() {
		_, err := r.Reload(context.Background(), testCaller(r))
		reloadDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the blocked capture never began")
	}
	submitThroughRuntime(t, r, record.Identity.SessionID, "op-during-block", "hello")
	awaitOperation(t, r, record.Identity.SessionID, "op-during-block", harness.OperationSuccess)
	select {
	case err := <-reloadDone:
		t.Fatalf("the blocked reload completed early: %v", err)
	default:
	}
	close(block)
	select {
	case err := <-reloadDone:
		testassert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the reload did not resume after the capture released")
	}

	// A failing capture fails only its candidate: the previous revision stays
	// published and unrelated execution continues.
	credentials.mu.Lock()
	credentials.fail = true
	credentials.mu.Unlock()
	before := r.config.current().generation
	_, err = r.Reload(context.Background(), testCaller(r))
	testassert.Require(t, errors.Is(err, ErrConfiguration), "failing capture reload = %v, want ErrConfiguration", err)
	testassert.Equal(t, r.config.current().generation, before)
	submitThroughRuntime(t, r, record.Identity.SessionID, "op-after-failure", "hello")
	awaitOperation(t, r, record.Identity.SessionID, "op-after-failure", harness.OperationSuccess)
}

// TestMutationCaptureRunsBeforeOwningWrites proves no fallible capture runs
// after a mutation's owning configuration or credential write.
func TestMutationCaptureRunsBeforeOwningWrites(t *testing.T) {
	server := newProductionModelServer(t, "", "")
	const keyEnv = "ORDER_CAPTURE_KEY"
	var events []string
	inner := NewValueConfigurationSource(sourceConfigDocument(server.URL, keyEnv, "One"))
	value := &recordingConfigurationSource{inner: inner, events: &events}
	credentials := &memoryCredentialSource{
		values: map[string]CredentialValue{keyEnv: {Value: "ordering-secret", Defined: true, Managed: true}},
		events: &events,
	}
	h := newServiceHarness(t)
	svc := h.service(context.Background())
	svc.configuration = value
	svc.credentials = credentials
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	if resources := svc.current().resources; resources != nil {
		defer func() { _ = resources.release() }()
	}

	key := "ordering-secret-2"
	window := 4096
	baseURL := "https://ordering.test/v1"
	_, err = svc.mutate(context.Background(), svc.editProviderCreate(context.Background(), "ordered", protocol.ProviderEdit{BaseUrl: &baseURL},
		map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key))
	testassert.NoError(t, err)
	writeAt, setAt := -1, -1
	for i, event := range events {
		switch event {
		case "config_write":
			if writeAt < 0 {
				writeAt = i
			}
		case "credential_set":
			setAt = i
		}
	}
	testassert.Require(t, writeAt >= 0 && setAt > writeAt, "events = %v, want the config write before the credential set", events)
	for _, event := range events[writeAt+1:] {
		testassert.Require(t, event != "capture", "events = %v, want no capture after the owning write", events)
	}
	{
		after := events[setAt+1:]
		for _, event := range after {
			testassert.Require(t, event != "capture", "events = %v, want no capture after the owning credential write", events)
		}
	}
}

// TestDefaultAgentInputs pins the included default inputs: full assembly and
// the seven standard tools with no model or capabilities.
func TestDefaultAgentInputs(t *testing.T) {
	inputs := DefaultAgentInputs()
	testassert.Equal(t, inputs.AssemblyMode, "full")
	testassert.Require(t, len(inputs.Tools) == 7 && inputs.Capabilities == nil && inputs.Model == (model.ModelRef{}), "default inputs = %+v, want seven tools and no model/capabilities", inputs)
	inputs.Tools[0] = "mutated"
	testassert.Require(t, DefaultAgentInputs().Tools[0] != "mutated", "DefaultAgentInputs aliases its slice")
}

// TestRuntimeOpenValidatesConstructionSelection proves explicit Agent inputs
// and a host strategy are validated at construction, before any effect.
func TestRuntimeOpenValidatesConstructionSelection(t *testing.T) {
	e := newOwnerEnv(t)
	base := e.options()
	invalidInputs := harness.AgentInputs{AssemblyMode: "bogus"}
	badStrategyID := &harness.StrategyBinding{RequiresContextWindow: true, Strategy: harness.IncludedStrategyBinding().Strategy}
	badStrategyImpl := &harness.StrategyBinding{ID: "host.strategy"}
	for _, tc := range []struct {
		name string
		opts options
	}{
		{"invalid agent inputs", func() options { o := base; o.Agent = &invalidInputs; return o }()},
		{"empty strategy id", func() options { o := base; o.Strategy = badStrategyID; return o }()},
		{"nil strategy implementation", func() options { o := base; o.Strategy = badStrategyImpl; return o }()},
	} {
		r, err := open(context.Background(), tc.opts)
		testassert.Require(t, r == nil && errors.Is(err, harness.ErrInvalid), "%s open = (%v, %v), want harness.ErrInvalid", tc.name, r, err)
		e.assertNoOwnershipSideEffects(t)
	}
}

// TestNewEvaluatorReceivesCapturedPolicyAndFailsClosed proves preparation
// calls the supplied factory with the captured policy inputs and rejects a nil
// evaluator before admission.
func TestNewEvaluatorReceivesCapturedPolicyAndFailsClosed(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, captureConfigDoc())
	svc := h.preparedService(context.Background())
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()

	var got PolicyInputs
	p := newPreparation(svc, h.locations(), "", nil, nil, newArtifactIntervals(), nil, func(inputs PolicyInputs) (harness.PermissionEvaluator, error) {
		got = inputs
		return nil, nil
	})
	_, err = p.bind()(context.Background(), harness.PreparationRequest{
		Session: harness.PreparationSession{
			Identity: harness.SessionIdentity{SessionID: testSessionID, Workspace: h.dataDir},
			AgentSelection: harness.AgentSelection{Inputs: &harness.AgentInputs{
				Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none",
			}},
		},
		RequestKind: harness.RequestKindMessage,
	})
	testassert.Require(t, err != nil && errors.Is(err, harness.ErrInvalid), "nil evaluator preparation = %v, want harness.ErrInvalid", err)
	testassert.Require(t, got.Workspace == h.dataDir && len(got.Global) != 0, "factory inputs = %+v, want the captured workspace and global policy", got)
}

// TestFileConfigurationMissingReadReportsNotExist proves the file adapter
// never creates a missing document: the read error wraps fs.ErrNotExist.
func TestFileConfigurationMissingReadReportsNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	_, err := NewFileConfigurationSource(path).Read(context.Background())
	testassert.Require(t, errors.Is(err, fs.ErrNotExist), "missing read = %v, want fs.ErrNotExist", err)
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("file read created the document: %v", statErr)
	}
}

// TestReservationLoadRetainUsesHeldMu proves a retaining published-pointer
// load runs under heldMu, the same mutex the publication Store takes.
func TestReservationLoadRetainUsesHeldMu(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, captureConfigDoc())
	svc := h.preparedService(context.Background())
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	defer svc.current().resources.release()

	svc.heldMu.Lock()
	done := make(chan configurationCapture, 1)
	go func() {
		captured, _ := svc.captureFor(context.Background(), true)
		done <- captured
	}()
	select {
	case captured := <-done:
		svc.heldMu.Unlock()
		if captured.snapshot != nil && captured.snapshot.resources != nil {
			_ = captured.snapshot.resources.release()
		}
		t.Fatal("a retaining capture loaded and retained without heldMu")
	case <-time.After(100 * time.Millisecond):
	}
	svc.heldMu.Unlock()
	select {
	case captured := <-done:
		testassert.Require(t, captured.snapshot != nil, "reserving capture returned no snapshot")
		_ = captured.snapshot.resources.release()
	case <-time.After(10 * time.Second):
		t.Fatal("reserving capture did not resume after heldMu released")
	}
}

// TestNewEvaluatorFailurePublishesNoOperation proves an evaluator factory
// error or nil evaluator rejects preparation before durable admission.
func TestNewEvaluatorFailurePublishesNoOperation(t *testing.T) {
	const factoryErr = "injected evaluator failure"
	for _, tc := range []struct {
		name      string
		evaluator func(PolicyInputs) (harness.PermissionEvaluator, error)
	}{
		{"factory error", func(PolicyInputs) (harness.PermissionEvaluator, error) { return nil, errors.New(factoryErr) }},
		{"nil evaluator", func(PolicyInputs) (harness.PermissionEvaluator, error) { return nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newOwnerEnv(t)
			e.fixtureStore(storage.NewMemory())
			opts := e.options()
			opts.NewEvaluator = tc.evaluator
			r, err := open(context.Background(), opts)
			testassert.NoError(t, err)
			defer func() { _ = r.Close(context.Background()) }()
			var record harness.SessionRecord
			testassert.NoError(t, r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
				var createErr error
				record, createErr = r.createSessionRecord(ctx, h, t.TempDir(), harness.AgentSelection{Inputs: &harness.AgentInputs{
					Model: model.ModelRef{Provider: "prov", Model: "m"}, AssemblyMode: "none",
				}})
				return createErr
			}))
			err = submitExpectFailure(r, record.Identity.SessionID, "op-evaluator", "hello")
			testassert.Require(t, err != nil, "evaluator failure submit succeeded")
			{
				readErr := readOperationError(context.Background(), r, record.Identity.SessionID, "op-evaluator")
				testassert.Require(t, errors.Is(readErr, harness.ErrNotFound), "evaluator failure admitted an Operation: %v", readErr)
			}
		})
	}
}

// contextRecordingConfiguration records the write context and fails if the
// caller's context is already canceled.
type contextRecordingConfiguration struct {
	inner        ConfigurationSource
	mu           sync.Mutex
	err          error
	writeEntered chan struct{}
	writeRelease chan struct{}
}

func (s *contextRecordingConfiguration) Read(ctx context.Context) ([]byte, error) {
	return s.inner.Read(ctx)
}

func (s *contextRecordingConfiguration) Write(ctx context.Context, data []byte) error {
	s.mu.Lock()
	s.err = ctx.Err()
	entered, release := s.writeEntered, s.writeRelease
	err := s.err
	s.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if err != nil {
		return err
	}
	return s.inner.Write(ctx, data)
}

func (s *contextRecordingConfiguration) writeError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// contextRecordingCredentials records the owning operation's context and
// fails if it is already canceled; Remove can park until the caller cancels.
type contextRecordingCredentials struct {
	inner         CredentialSource
	mu            sync.Mutex
	setErr        error
	removeErr     error
	removeEntered chan struct{}
	removeRelease chan struct{}
}

func (s *contextRecordingCredentials) Capture(ctx context.Context, names []string) (map[string]CredentialValue, error) {
	return s.inner.Capture(ctx, names)
}

func (s *contextRecordingCredentials) Set(ctx context.Context, name, value string) error {
	s.mu.Lock()
	s.setErr = ctx.Err()
	err := s.setErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.inner.Set(ctx, name, value)
}

func (s *contextRecordingCredentials) Remove(ctx context.Context, name string) error {
	s.mu.Lock()
	entered, release := s.removeEntered, s.removeRelease
	s.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	s.mu.Lock()
	s.removeErr = ctx.Err()
	err := s.removeErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.inner.Remove(ctx, name)
}

func (s *contextRecordingCredentials) SubprocessEnv(ctx context.Context, excluded []string) ([]string, error) {
	return s.inner.SubprocessEnv(ctx, excluded)
}

// TestMutationWritesUseWithoutCancel proves configuration Write and credential
// Set/Remove after the final cancellation check run with a non-canceled
// context, including a cross-source rollback-capable create.
func TestMutationWritesUseWithoutCancel(t *testing.T) {
	t.Run("configuration write and credential set", func(t *testing.T) {
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, `{"providers":{}}`)
		svc := h.service(context.Background())
		writeEntered := make(chan struct{}, 1)
		writeRelease := make(chan struct{})
		configuration := &contextRecordingConfiguration{inner: NewFileConfigurationSource(h.configPath), writeEntered: writeEntered, writeRelease: writeRelease}
		credentials := &contextRecordingCredentials{inner: &memoryCredentialSource{values: map[string]CredentialValue{}}}
		svc.configuration = configuration
		svc.credentials = credentials
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
		if resources := svc.current().resources; resources != nil {
			defer func() { _ = resources.release() }()
		}

		key := "sk-without-cancel"
		window := 4096
		baseURL := "https://without-cancel.test/v1"
		keyEnv := "WITHOUT_CANCEL_KEY"
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := svc.mutate(ctx, svc.editProviderCreate(context.Background(), "withoutcancel", protocol.ProviderEdit{BaseUrl: &baseURL, ApiKeyEnv: &keyEnv},
				map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key))
			done <- err
		}()
		select {
		case <-writeEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("the configuration write never began")
		}
		cancel()
		close(writeRelease)
		testassert.NoError(t, <-done)
		testassert.Require(t, configuration.writeError() == nil, "configuration Write context = %v, want non-canceled", configuration.writeError())
		credentials.mu.Lock()
		setErr := credentials.setErr
		credentials.mu.Unlock()
		testassert.Require(t, setErr == nil, "credential Set context = %v, want non-canceled", setErr)
	})

	t.Run("credential remove", func(t *testing.T) {
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, `{"providers":{"prov":`+nominalProvMember("https://remove.test/v1", "WITHOUT_CANCEL_REMOVE", `{"m":{"name":"M","context_window":4096}}`)+`}}`)
		svc := h.service(context.Background())
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		credentials := &contextRecordingCredentials{
			inner:         &memoryCredentialSource{values: map[string]CredentialValue{"WITHOUT_CANCEL_REMOVE": {Value: "remove-secret", Defined: true, Managed: true}}},
			removeEntered: entered,
			removeRelease: release,
		}
		svc.credentials = credentials
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
		if resources := svc.current().resources; resources != nil {
			defer func() { _ = resources.release() }()
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := svc.mutate(ctx, svc.editProviderDisconnect("prov", &connectionEffects{}))
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("credential Remove never began")
		}
		cancel()
		close(release)
		testassert.NoError(t, <-done)
		credentials.mu.Lock()
		removeErr := credentials.removeErr
		credentials.mu.Unlock()
		testassert.Require(t, removeErr == nil, "credential Remove context = %v, want non-canceled", removeErr)
	})
}

// TestDisconnectDecidesFromMutationCapture proves disconnect reads the
// mutation build's latest-document capture: a managed key added to the source
// after publication is removed without a reload.
func TestDisconnectDecidesFromMutationCapture(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"providers":{"prov":`+nominalProvMember("https://capture.test/v1", "MUTATION_CAPTURE_KEY", `{"m":{"name":"M","context_window":4096}}`)+`}}`)
	memory := &memoryCredentialSource{values: map[string]CredentialValue{}}
	svc := h.service(context.Background())
	svc.credentials = memory
	_, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	if resources := svc.current().resources; resources != nil {
		defer func() { _ = resources.release() }()
	}

	memory.mu.Lock()
	memory.values["MUTATION_CAPTURE_KEY"] = CredentialValue{Value: "outside-change", Defined: true, Managed: true}
	memory.mu.Unlock()
	mutation, err := svc.mutate(context.Background(), svc.editProviderDisconnect("prov", &connectionEffects{}))
	testassert.NoError(t, err)
	observed := mutation.snapshot.credentials["MUTATION_CAPTURE_KEY"]
	testassert.Require(t, !observed.Managed && observed.Value == "", "disconnect capture = %+v, want the removed key from the mutation build", observed)
	memory.mu.Lock()
	after := memory.values["MUTATION_CAPTURE_KEY"]
	memory.mu.Unlock()
	testassert.Require(t, !after.Managed && after.Value == "", "disconnect left the managed key in the source: %+v", after)
}

// TestProjectedShellUsesPreparationContext proves the prompt projection passes
// the preparation context to the child-environment source.
func TestProjectedShellUsesPreparationContext(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "preparation")
	got := projectedShell(ctx, func(ctx context.Context) ([]string, error) {
		if ctx.Value(key{}) != "preparation" {
			return nil, errors.New("producer received the wrong context")
		}
		return []string{"SHELL=/bin/prepared"}, nil
	})
	testassert.Equal(t, got, "/bin/prepared")
}

// TestPolicyInputsOwnRevisionBytes proves evaluator inputs do not alias the
// published revision's permission bytes.
func TestPolicyInputsOwnRevisionBytes(t *testing.T) {
	global := json.RawMessage(`{"rules":[]}`)
	workspace := json.RawMessage(`{"default":"allow"}`)
	c := &configuration{permissions: global}
	id, err := config.WorkspacePermissionDirID("/workspace")
	testassert.NoError(t, err)
	c.workspacePermissions = map[string]json.RawMessage{id: workspace}
	inputs := c.policyInputs("/workspace")
	global[0] = 'x'
	workspace[0] = 'x'
	testassert.Require(t, inputs.Global[0] == '{' && inputs.WorkspacePolicy[0] == '{', "policy inputs alias revision bytes: %+v", inputs)
}

// TestStrategyBindingIsCopiedAtOpen proves the host strategy pointer is owned
// by the owner after construction.
func TestStrategyBindingIsCopiedAtOpen(t *testing.T) {
	e := newOwnerEnv(t)
	binding := &harness.StrategyBinding{ID: "host.strategy", RequiresContextWindow: false, Strategy: harness.IncludedStrategyBinding().Strategy}
	opts := e.options()
	opts.Strategy = binding
	e.fixtureStore(storage.NewMemory())
	r, err := open(context.Background(), opts)
	testassert.NoError(t, err)
	defer func() { _ = r.Close(context.Background()) }()
	binding.ID = "mutated"
	testassert.Equal(t, r.config.current().composition.defaultStrategy.ID, "host.strategy")
}

// contextBlockingCredentials waits for request cancellation on selected
// captures.
type contextBlockingCredentials struct {
	inner   CredentialSource
	mu      sync.Mutex
	block   bool
	entered chan struct{}
}

func (s *contextBlockingCredentials) Capture(ctx context.Context, names []string) (map[string]CredentialValue, error) {
	s.mu.Lock()
	block, entered := s.block, s.entered
	s.mu.Unlock()
	if block {
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.inner.Capture(ctx, names)
}

func (s *contextBlockingCredentials) Set(ctx context.Context, name, value string) error {
	return s.inner.Set(ctx, name, value)
}

func (s *contextBlockingCredentials) Remove(ctx context.Context, name string) error {
	return s.inner.Remove(ctx, name)
}

func (s *contextBlockingCredentials) SubprocessEnv(ctx context.Context, excluded []string) ([]string, error) {
	return s.inner.SubprocessEnv(ctx, excluded)
}

// TestProviderCreateCredentialCaptureUsesRequestContext proves a provider
// create's generated-name and explicit-binding captures use the mutation
// request context: cancelling the request unblocks the waiting source, leaves
// the revision and document unchanged, and releases the build serialization.
func TestProviderCreateCredentialCaptureUsesRequestContext(t *testing.T) {
	for _, tc := range []struct {
		name   string
		patch  func(env string) protocol.ProviderEdit
		keyEnv string
	}{
		{"generated name", func(string) protocol.ProviderEdit { return protocol.ProviderEdit{} }, ""},
		{"explicit binding", func(env string) protocol.ProviderEdit { return protocol.ProviderEdit{ApiKeyEnv: &env} }, "REQUEST_CONTEXT_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newServiceHarness(t)
			writeServiceFile(t, h.configPath, `{"providers":{}}`)
			blocking := &contextBlockingCredentials{inner: &memoryCredentialSource{values: map[string]CredentialValue{}}, entered: make(chan struct{}, 1)}
			svc := h.service(context.Background())
			svc.credentials = blocking
			_, err := svc.publish(context.Background())
			testassert.NoError(t, err)
			if resources := svc.current().resources; resources != nil {
				defer func() { _ = resources.release() }()
			}
			before, err := os.ReadFile(h.configPath)
			testassert.NoError(t, err)
			generation := svc.current().generation

			blocking.mu.Lock()
			blocking.block = true
			blocking.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			key := "sk-request-context"
			window := 4096
			baseURL := "https://request-context.test/v1"
			done := make(chan error, 1)
			go func() {
				_, err := svc.mutate(ctx, svc.editProviderCreate(ctx, "requestctx", protocol.ProviderEdit{
					BaseUrl:   &baseURL,
					ApiKeyEnv: tc.patch(tc.keyEnv).ApiKeyEnv,
				}, map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &key))
				done <- err
			}()
			select {
			case <-blocking.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the credential capture never began")
			}
			cancel()
			select {
			case err := <-done:
				testassert.Require(t, errors.Is(err, context.Canceled), "canceled create = %v, want context.Canceled", err)
			case <-time.After(5 * time.Second):
				t.Fatal("cancelling the create did not unblock the credential capture")
			}
			testassert.Equal(t, svc.current().generation, generation)
			after, err := os.ReadFile(h.configPath)
			testassert.NoError(t, err)
			testassert.Require(t, string(after) == string(before), "a canceled create changed the owning document")

			// The build serialization is released for the next mutation.
			blocking.mu.Lock()
			blocking.block = false
			blocking.mu.Unlock()
			nextKey := "sk-next-create"
			nextEnv := "NEXT_CREATE_KEY"
			_, err = svc.mutate(context.Background(), svc.editProviderCreate(context.Background(), "nextcreate", protocol.ProviderEdit{
				BaseUrl: &baseURL, ApiKeyEnv: &nextEnv,
			}, map[string]protocol.ModelEdit{"m": {ContextWindow: &window}}, &nextKey))
			testassert.NoError(t, err)
			testassert.Equal(t, svc.current().generation, generation+1)
		})
	}
}
