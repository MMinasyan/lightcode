package melta

// The fixed data host service and its Session artifact ownership are exercised
// through actual constructed native capabilities, real Melta deletion, and
// real filesystem sentinels.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// dataProbe is one Harness-scoped native service with the fixed data binding
// and, optionally, the fixed session cleanup declaration.
type dataProbe struct {
	id          string
	native      extension.Native
	capability  string
	cleanup     string
	cleanupCall func(context.Context, extension.CallContext) error

	// cleanupOutcome, when set, is returned verbatim instead of the ordinary
	// null success; it exercises declared Failure and invalid-result siblings.
	cleanupOutcome *extension.Result
}

func newDataProbe(id string, cleanup func(context.Context, extension.CallContext) error) *dataProbe {
	hostData, ok := extension.HostCapability(extension.ContractData)
	if !ok {
		panic("missing data host capability")
	}
	probe := &dataProbe{id: id, capability: "probe." + id}
	capabilities := []extension.Capability{
		extension.ServiceCapability(probe.capability, "test.probe."+id, extension.PureMethod("ping", `{"type":"object","additionalProperties":false}`, `{"type":"object","additionalProperties":false}`)),
	}
	if cleanup != nil {
		probe.cleanup = "cleanup." + id
		capabilities = append(capabilities, extension.CleanupCapability(probe.cleanup))
	}
	probe.cleanupCall = cleanup
	probe.native = testNative(id, extension.LifetimeHarness, capabilities, []extension.Binding{{
		Name: "data", Provider: extension.HostProvider, CapabilityID: extension.ContractData, Definition: hostData,
	}}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		implementations := map[string]extension.Implementation{
			probe.capability: {Service: map[string]extension.Handler{
				"ping": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					return extension.Result{Value: json.RawMessage(`{}`)}, nil
				},
			}},
		}
		if cleanup != nil {
			implementations[probe.cleanup] = extension.Implementation{Service: map[string]extension.Handler{"cleanup": func(ctx context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				if probe.cleanupCall != nil {
					if err := probe.cleanupCall(ctx, call); err != nil {
						return extension.Result{}, err
					}
				}
				if probe.cleanupOutcome != nil {
					return *probe.cleanupOutcome, nil
				}
				return extension.Result{Value: json.RawMessage(`null`)}, nil
			}}}
		}
		return extension.Instance{Capabilities: implementations}, nil
	})
	return probe
}

// dataConfig enables the supplied probes beside the ordinary supplied Agents
// definition used by Session creation.
func dataConfig(endpoint string, ids ...string) string {
	parts := []string{`"agents":{"enabled":true,"definitions":` + ownerAgentsDocument + `}`}
	for _, id := range ids {
		parts = append(parts, `"`+id+`":{"enabled":true}`)
	}
	return strings.TrimSuffix(ownerConfigDocument(endpoint), "}") + `,"extensions":{` + strings.Join(parts, ",") + `}}`
}

// dataProbeValue resolves one probe's constructed Melta-scope capability.
func dataProbeValue(t *testing.T, r *Melta, capabilityID string) *capabilityValue {
	t.Helper()
	snapshot := r.config.current()
	testassert.Require(t, snapshot != nil && snapshot.resources != nil && snapshot.resources.runtime != nil, "runtime has no published resource set")
	value := snapshot.resources.runtime.capabilities[capabilityID]
	testassert.Require(t, value != nil, "capability %q is not open in the Melta scope", capabilityID)
	return value
}

// dataView opens one actual callback view over a probe capability for the
// host-issued Session identity.
func dataView(t *testing.T, r *Melta, capabilityID, sessionID string, grant nativeGrant) (*nativeView, extension.CallContext) {
	t.Helper()
	snapshot := r.config.current()
	value := dataProbeValue(t, r, capabilityID)
	view, calls := snapshot.composition.newView(value, context.Background(), callIdentity{
		session:  harness.SessionIdentity{SessionID: sessionID},
		revision: snapshot.resources.revisionString(),
		grant:    grant,
	})
	return view, calls
}

func callData(t *testing.T, calls extension.CallContext, method string) (extension.Result, error) {
	t.Helper()
	return calls.Call(context.Background(), "data", method, json.RawMessage(`{}`))
}

func namespaceDirectory(t *testing.T, calls extension.CallContext, method string) string {
	t.Helper()
	result, err := callData(t, calls, method)
	testassert.Require(t, err == nil && len(result.Value) != 0, "%s = (%+v, %v), want a directory result", method, result, err)
	var decoded struct {
		Directory string `json:"directory"`
	}
	testassert.NoError(t, json.Unmarshal(result.Value, &decoded))
	testassert.Require(t, decoded.Directory != "", "%s result = %s, want a nonempty directory", method, result.Value)
	return decoded.Directory
}

func sessionOwners(t *testing.T, store harness.Storage, sessionID string) []string {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	testassert.NoError(t, err)
	var wire struct {
		State struct {
			Owners []string `json:"external_data_owners"`
		} `json:"state"`
	}
	testassert.NoError(t, json.Unmarshal(reg.Payload, &wire))
	return append([]string(nil), wire.State.Owners...)
}

// TestDataServiceAssignsExtensionOwnedDirectories proves the declared service
// derives the hashed Session and cache paths from host identity alone, two
// extensions stay separate, archived Sessions remain readable, cache and
// configuration survive deletion, and the deleted identity grants nothing.
func TestDataServiceAssignsExtensionOwnedDirectories(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		a := newDataProbe("probe_a", nil)
		b := newDataProbe("probe_b", nil)
		writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_a", "probe_b"))
		e.fixtureStore(store)
		ctx := context.Background()
		r, err := e.open(ctx, a.native, b.native)
		testassert.NoError(t, err)
		defer r.Close(ctx)
		workspace := filepath.Join(e.dataDir, "data-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session, err := r.createSession(ctx, workspace, "solo")
		testassert.NoError(t, err)
		id := session.Identity.SessionID

		viewA, callsA := dataView(t, r, a.capability, id, nativeGrant{})
		dirA := namespaceDirectory(t, callsA, "session_namespace")
		viewA.end()
		{
			_, err := callsA.Call(context.Background(), "data", "cache_namespace", json.RawMessage(`{}`))
			testassert.Require(t, errors.Is(err, extension.ErrClosed), "retained CallContext after callback return = %v, want the closed refusal", err)
		}
		viewB, callsB := dataView(t, r, b.capability, id, nativeGrant{})
		dirB := namespaceDirectory(t, callsB, "session_namespace")
		viewB.end()
		wantA := filepath.Join(e.dataDir, "code", id, "extensions", extensionDataHash("probe_a"))
		wantB := filepath.Join(e.dataDir, "code", id, "extensions", extensionDataHash("probe_b"))
		testassert.Require(t, dirA == wantA && dirB == wantB && dirA != dirB, "directories = (%q, %q), want the two hashed owned paths", dirA, dirB)
		testassert.NoError(t, os.WriteFile(filepath.Join(dirA, "a.sentinel"), []byte("a"), 0o600))
		testassert.NoError(t, os.WriteFile(filepath.Join(dirB, "b.sentinel"), []byte("b"), 0o600))

		cacheView, cacheCalls := dataView(t, r, a.capability, id, nativeGrant{})
		cache := namespaceDirectory(t, cacheCalls, "cache_namespace")
		cacheView.end()
		wantCache := filepath.Join(e.dataDir, "extensions", extensionDataHash("probe_a"), "cache")
		testassert.Equal(t, cache, wantCache)
		testassert.NoError(t, os.WriteFile(filepath.Join(cache, "cache.sentinel"), []byte("cache"), 0o600))

		// An archived but current Session still assigns its namespace.
		{
			_, err := r.ArchiveSession(ctx, testCaller(r), id)
			testassert.NoError(t, err)
		}
		archivedView, archivedCalls := dataView(t, r, a.capability, id, nativeGrant{})
		testassert.Equal(t, namespaceDirectory(t, archivedCalls, "session_namespace"), wantA)
		archivedView.end()

		result, err := r.DeleteSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
		testassert.Require(t, len(result.CleanupFailures) == 0, "deletion result = %+v, want complete cleanup", result)
		mustNotExist(t, filepath.Join(e.dataDir, "code", id), "the whole deleted Session tree")
		mustExist(t, filepath.Join(cache, "cache.sentinel"), "the deleted Session's extension cache")
		mustExist(t, e.configPath, "the configuration file")

		// The deleted identity grants no namespace and creates no directory.
		deletedView, deletedCalls := dataView(t, r, a.capability, id, nativeGrant{})
		{
			_, err := callData(t, deletedCalls, "session_namespace")
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "deleted Session namespace = %v, want the not-found class", err)
		}
		deletedView.end()
		mustNotExist(t, wantA, "the deleted Session's refused namespace directory")
		{
			err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.Submit(ctx, harness.SubmitRequest{
					SessionID: id, OperationID: "op-deleted", Origin: harness.InputOriginUser,
					Content: []model.ContentPart{{Kind: model.PartText, Text: "no"}}, Mode: harness.MessageModeRegular,
				})
				return err
			})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "submission for the deleted identity = %v, want not_found", err)
		}

		// A live sibling still assigns its own namespace while guest routing
		// fields are schema-invalid before the handler; a pure callback may not
		// invoke the service at all.
		other, err := r.createSession(ctx, workspace, "solo")
		testassert.NoError(t, err)
		otherID := other.Identity.SessionID
		siblingView, siblingCalls := dataView(t, r, a.capability, otherID, nativeGrant{})
		testassert.Equal(t, namespaceDirectory(t, siblingCalls, "session_namespace"), filepath.Join(e.dataDir, "code", otherID, "extensions", extensionDataHash("probe_a")))
		siblingView.end()
		routing, err := r.createSession(ctx, workspace, "solo")
		testassert.NoError(t, err)
		routingID := routing.Identity.SessionID
		foreignView, foreignCalls := dataView(t, r, a.capability, routingID, nativeGrant{})
		{
			_, err := foreignCalls.Call(context.Background(), "data", "session_namespace", json.RawMessage(`{"session_id":"`+id+`"}`))
			testassert.Require(t, errors.Is(err, ErrComposition), "foreign namespace arguments = %v, want the schema refusal", err)
		}
		foreignView.end()
		mustNotExist(t, filepath.Join(e.dataDir, "code", routingID, "extensions", extensionDataHash("probe_a")), "a refused namespace argument created a directory")
		pureView, pureCalls := dataView(t, r, a.capability, routingID, nativeGrant{pure: true})
		{
			_, err := callData(t, pureCalls, "session_namespace")
			testassert.Require(t, errors.Is(err, ErrComposition), "pure namespace call = %v, want the purity refusal", err)
		}
		pureView.end()
		mustNotExist(t, filepath.Join(e.dataDir, "code", routingID, "extensions", extensionDataHash("probe_a")), "a pure namespace call created a directory")
	})
}

// TestDataServiceRefusesPlantedSymlinks proves ancestor and leaf symlinks are
// refused without touching the outside sentinel.
func TestDataServiceRefusesPlantedSymlinks(t *testing.T) {
	e := newOwnerEnv(t)
	a := newDataProbe("probe_a", nil)
	writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_a"))
	e.fixtureStore(storage.NewMemory())
	ctx := context.Background()
	r, err := e.open(ctx, a.native)
	testassert.NoError(t, err)
	defer r.Close(ctx)
	workspace := filepath.Join(e.dataDir, "symlink-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID

	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	testassert.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	extensions := filepath.Join(e.dataDir, "code", id, "extensions")
	testassert.NoError(t, os.MkdirAll(extensions, 0o700))
	leaf := filepath.Join(extensions, extensionDataHash("probe_a"))
	testassert.NoError(t, os.Symlink(outside, leaf))
	view, calls := dataView(t, r, a.capability, id, nativeGrant{})
	{
		_, err := callData(t, calls, "session_namespace")
		testassert.Require(t, err != nil, "planted leaf symlink namespace = nil, want a refusal")
	}
	view.end()

	second, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	secondID := second.Identity.SessionID
	testassert.NoError(t, os.RemoveAll(filepath.Join(e.dataDir, "code", secondID)))
	testassert.NoError(t, os.Symlink(outside, filepath.Join(e.dataDir, "code", secondID)))
	secondView, secondCalls := dataView(t, r, a.capability, secondID, nativeGrant{})
	{
		_, err := callData(t, secondCalls, "session_namespace")
		testassert.Require(t, err != nil, "planted ancestor symlink namespace = nil, want a refusal")
	}
	secondView.end()
	data, err := os.ReadFile(sentinel)
	testassert.Require(t, err == nil && string(data) == "keep", "outside sentinel = (%q, %v), want it untouched", data, err)
}

// TestDataServiceStandaloneWriterHoldsInterval proves a standalone callback
// owns the Session interval from its first namespace call through its terminal
// response: deletion and sweep skip it, and cache-only access never takes the
// Session interval.
func TestDataServiceStandaloneWriterHoldsInterval(t *testing.T) {
	e := newOwnerEnv(t)
	a := newDataProbe("probe_a", nil)
	// Enable a sweep deletion threshold without a stale archive: the pass is
	// invoked explicitly with a future time.
	writeServiceFile(t, e.configPath, strings.TrimSuffix(dataConfig(e.server.URL, "probe_a"), "}")+`,"sessions":{"auto_archive":true,"archive_after_days":1,"delete_after_archive_days":1}}`)
	e.fixtureStore(storage.NewMemory())
	ctx := context.Background()
	r, err := e.open(ctx, a.native)
	testassert.NoError(t, err)
	defer r.Close(ctx)
	workspace := filepath.Join(e.dataDir, "writer-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID
	{
		_, err := r.ArchiveSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
	}
	view, calls := dataView(t, r, a.capability, id, nativeGrant{})
	namespace := namespaceDirectory(t, calls, "session_namespace")
	testassert.NoError(t, os.WriteFile(filepath.Join(namespace, "writing"), []byte("live"), 0o600))

	deleteDone := make(chan error, 1)
	go func() {
		_, err := r.DeleteSession(ctx, testCaller(r), id)
		deleteDone <- err
	}()
	select {
	case err := <-deleteDone:
		testassert.Require(t, errors.Is(err, harness.ErrConflict), "delete during the standalone writer = %v, want the conflict class", err)
	case <-time.After(5 * time.Second):
		t.Fatal("delete waited behind the standalone writer instead of conflicting")
	}
	r.runSweepPass(ctx, time.Now().Add(100*time.Hour))
	{
		err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ReadSessionHeader(ctx, id)
			return err
		})
		testassert.Require(t, err == nil, "sweep removed the Session under a live standalone writer: %v", err)
	}
	mustExist(t, filepath.Join(namespace, "writing"), "the writer's retained data")

	// Cache-only access on a fresh identity creates no Session interval.
	other, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	otherID := other.Identity.SessionID
	cacheView, cacheCalls := dataView(t, r, a.capability, otherID, nativeGrant{})
	namespaceDirectory(t, cacheCalls, "cache_namespace")
	release, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, otherID))
	testassert.Require(t, err == nil, "cache-only access held the Session interval: %v", err)
	release()
	cacheView.end()

	view.end()
	{
		result, err := r.DeleteSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
		testassert.Require(t, len(result.CleanupFailures) == 0, "delete after the writer released = %+v", result)
	}
	mustNotExist(t, filepath.Join(e.dataDir, "code", id), "the released Session tree")
}

// TestDataServiceExecutionLeaseIsBorrowed proves an execution's existing
// interval is shared, never reacquired, and released only by its owner.
func TestDataServiceExecutionLeaseIsBorrowed(t *testing.T) {
	e := newOwnerEnv(t)
	a := newDataProbe("probe_a", nil)
	writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_a"))
	e.fixtureStore(storage.NewMemory())
	ctx := context.Background()
	r, err := e.open(ctx, a.native)
	testassert.NoError(t, err)
	defer r.Close(ctx)
	workspace := filepath.Join(e.dataDir, "exec-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID
	releaseExecution, err := r.artifacts.acquireExecution(ctx, sessionArtifactDir(r.dataDir, id))
	testassert.NoError(t, err)
	lease := &artifactCallLease{
		intervals: r.artifacts,
		key:       sessionArtifactDir(r.dataDir, id),
		borrowed:  true,
		release:   releaseExecution,
	}
	snapshot := r.config.current()
	value := dataProbeValue(t, r, a.capability)
	view, calls := snapshot.composition.newView(value, context.Background(), callIdentity{
		session:  harness.SessionIdentity{SessionID: id},
		revision: snapshot.resources.revisionString(),
		lease:    lease,
	})
	namespaceDirectory(t, calls, "session_namespace")
	{
		_, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, id))
		testassert.Require(t, errors.Is(err, errArtifactBusy), "a nested data call lost the execution interval: %v", err)
	}
	view.end()
	{
		_, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, id))
		testassert.Require(t, errors.Is(err, errArtifactBusy), "a callback view released the execution's borrowed interval: %v", err)
	}
	releaseExecution()
	release, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, id))
	testassert.Require(t, err == nil, "the execution owner's release did not free the interval: %v", err)
	release()
}

// TestDataServiceClaimRequiresDeclaration proves the claim writes the ordered
// owner set only through a declaring extension, a repeat is a no-op, archived
// and deleted identities are refused, and no claim creates data.
func TestDataServiceClaimRequiresDeclaration(t *testing.T) {
	e := newOwnerEnv(t)
	a := newDataProbe("probe_a", func(context.Context, extension.CallContext) error { return nil })
	b := newDataProbe("probe_b", nil)
	writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_a", "probe_b"))
	store := storage.NewMemory()
	e.fixtureStore(store)
	ctx := context.Background()
	r, err := e.open(ctx, a.native, b.native)
	testassert.NoError(t, err)
	defer r.Close(ctx)
	workspace := filepath.Join(e.dataDir, "claim-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID

	viewA, callsA := dataView(t, r, a.capability, id, nativeGrant{})
	result, err := callData(t, callsA, "claim_external_data")
	testassert.Require(t, err == nil && string(result.Value) == "null", "claim = (%+v, %v), want null success", result, err)
	{
		_, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, id))
		testassert.Require(t, errors.Is(err, errArtifactBusy), "the claim released the Session interval before its terminal response: %v", err)
	}
	viewA.end()
	testassert.Require(t, len(sessionOwners(t, store, id)) == 1 && sessionOwners(t, store, id)[0] == "probe_a", "owners = %v, want probe_a", sessionOwners(t, store, id))
	mustNotExist(t, filepath.Join(e.dataDir, "code", id, "extensions", extensionDataHash("probe_a")), "a claim created a Session directory")

	viewRepeat, repeatCalls := dataView(t, r, a.capability, id, nativeGrant{})
	_, err = callData(t, repeatCalls, "claim_external_data")
	testassert.NoError(t, err)
	viewRepeat.end()
	testassert.Require(t, len(sessionOwners(t, store, id)) == 1, "repeated claim duplicated the owner set: %v", sessionOwners(t, store, id))

	viewB, callsB := dataView(t, r, b.capability, id, nativeGrant{})
	{
		_, err := callData(t, callsB, "claim_external_data")
		testassert.Require(t, err != nil, "claim without a cleanup declaration = nil, want a refusal")
	}
	viewB.end()
	testassert.Require(t, len(sessionOwners(t, store, id)) == 1, "a refused claim changed the owners: %v", sessionOwners(t, store, id))
	mustNotExist(t, filepath.Join(e.dataDir, "code", id, "extensions", extensionDataHash("probe_b")), "a refused claim created a Session directory")

	holderView, holderCalls := dataView(t, r, a.capability, id, nativeGrant{})
	namespaceDirectory(t, holderCalls, "session_namespace")
	busyView, busyCalls := dataView(t, r, a.capability, id, nativeGrant{})
	{
		_, err := callData(t, busyCalls, "claim_external_data")
		testassert.Require(t, errors.Is(err, errArtifactBusy), "claim under a live writer = %v, want the conflict class", err)
	}
	busyView.end()
	holderView.end()
	testassert.Require(t, len(sessionOwners(t, store, id)) == 1, "a busy claim changed the owners: %v", sessionOwners(t, store, id))

	{
		_, err := r.ArchiveSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
	}
	archivedView, archivedCalls := dataView(t, r, a.capability, id, nativeGrant{})
	{
		_, err := callData(t, archivedCalls, "claim_external_data")
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "archived claim = %v, want ErrInvalid", err)
	}
	archivedView.end()
}

// TestDataServiceOwnersDeletionAndCleanupFailureTokens proves the shared
// post-commit producer reports local and external failures separately, keeps
// the canonical deletion, and only the committing deletion can report owners.
func TestDataServiceOwnersDeletionAndCleanupFailureTokens(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes as root")
	}
	e := newOwnerEnv(t)
	var goodCalls, badCalls atomic.Int64
	var goodSawSession atomic.Bool
	var goodNamespaceRefused atomic.Bool
	good := newDataProbe("probe_good", func(ctx context.Context, call extension.CallContext) error {
		goodCalls.Add(1)
		goodSawSession.Store(call.SessionID != "")
		if _, err := call.Call(context.Background(), "data", "session_namespace", json.RawMessage(`{}`)); err != nil {
			goodNamespaceRefused.Store(true)
		}
		return nil
	})
	bad := newDataProbe("probe_bad", func(context.Context, extension.CallContext) error {
		badCalls.Add(1)
		return errors.New("guest secret detail must not escape")
	})
	writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_good", "probe_bad"))
	e.fixtureStore(storage.NewMemory())
	ctx := context.Background()
	r, err := e.open(ctx, good.native, bad.native)
	testassert.NoError(t, err)
	defer r.Close(ctx)
	workspace := filepath.Join(e.dataDir, "cleanup-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID
	for _, probe := range []*dataProbe{good, bad} {
		view, calls := dataView(t, r, probe.capability, id, nativeGrant{})
		_, err := callData(t, calls, "claim_external_data")
		testassert.NoError(t, err)
		view.end()
	}
	{
		_, err := r.DeleteSession(ctx, testCaller(r), id) // open Session: refused before any cleanup
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "open Session delete = %v, want the refusal", err)
	}
	testassert.Require(t, goodCalls.Load() == 0 && badCalls.Load() == 0, "a refused deletion ran cleanup callbacks")
	plantSessionCode(t, e.dataDir, id)
	cache := filepath.Join(e.dataDir, "extensions", extensionDataHash("probe_good"), "cache")
	testassert.NoError(t, os.MkdirAll(cache, 0o700))
	testassert.NoError(t, os.WriteFile(filepath.Join(cache, "keep"), []byte("cache"), 0o600))
	{
		_, err := r.ArchiveSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
	}
	codeRoot := filepath.Join(e.dataDir, "code")
	testassert.NoError(t, os.Chmod(codeRoot, 0o555))
	t.Cleanup(func() { os.Chmod(codeRoot, 0o755) })

	result, err := r.DeleteSession(ctx, testCaller(r), id)
	testassert.NoError(t, err)
	testassert.Require(t, len(result.CleanupFailures) == 2, "cleanup failures = %+v, want the local storage failure and the external unavailable failure", result.CleanupFailures)
	testassert.Require(t, result.CleanupFailures[0].ExtensionId == nil && result.CleanupFailures[0].Code == "storage", "local failure = %+v, want the storage class with no extension id", result.CleanupFailures[0])
	testassert.Require(t, result.CleanupFailures[1].ExtensionId != nil && *result.CleanupFailures[1].ExtensionId == "probe_bad" && result.CleanupFailures[1].Code == "unavailable" && result.CleanupFailures[1].Message == cleanupUnavailableMessage, "external failure = %+v, want the fixed unavailable detail", result.CleanupFailures[1])
	testassert.Require(t, !strings.Contains(result.CleanupFailures[1].Message, "guest secret"), "external failure forwarded guest error detail")
	testassert.Require(t, goodCalls.Load() == 1 && badCalls.Load() == 1, "cleanup calls = (%d, %d), want both owners attempted", goodCalls.Load(), badCalls.Load())
	testassert.Require(t, !goodSawSession.Load() && goodNamespaceRefused.Load(), "cleanup gained a Session binding or namespace grant")
	{
		err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ReadSessionHeader(ctx, id)
			return err
		})
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "canonical deletion = %v, want it committed", err)
	}
	mustExist(t, filepath.Join(cache, "keep"), "the external cache after partial cleanup")
	mustExist(t, e.configPath, "the configuration after partial cleanup")
	testassert.NoError(t, os.Chmod(codeRoot, 0o755))
	{
		repeated, err := r.DeleteSession(ctx, testCaller(r), id)
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "repeated delete = (%+v, %v), want the ordinary not-found", repeated, err)
		testassert.Require(t, len(repeated.CleanupFailures) == 0, "repeated delete produced a cleanup result: %+v", repeated)
		testassert.Require(t, goodCalls.Load() == 1 && badCalls.Load() == 1, "repeated delete replayed owner cleanup: calls = (%d, %d)", goodCalls.Load(), badCalls.Load())
	}
	mustExist(t, filepath.Join(e.dataDir, "code", id), "the unremoved local tree after the absent deletion")
}

// TestDataServiceDeletionResultOnlyFromCommittingDelete proves only the
// deletion that commits produces a cleanup result: a repeated delete reports
// the ordinary not-found and replays no external cleanup.
func TestDataServiceDeletionResultOnlyFromCommittingDelete(t *testing.T) {
	e := newOwnerEnv(t)
	var cleanupCalls atomic.Int64
	bad := newDataProbe("probe_bad", func(context.Context, extension.CallContext) error {
		cleanupCalls.Add(1)
		return errors.New("guest secret detail must not escape")
	})
	writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_bad"))
	e.fixtureStore(storage.NewMemory())
	ctx := context.Background()
	r, err := e.open(ctx, bad.native)
	testassert.NoError(t, err)
	defer r.Close(ctx)
	workspace := filepath.Join(e.dataDir, "delete-once-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID
	view, calls := dataView(t, r, bad.capability, id, nativeGrant{})
	_, err = callData(t, calls, "claim_external_data")
	testassert.NoError(t, err)
	view.end()
	{
		_, err := r.ArchiveSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
	}

	result, err := r.DeleteSession(ctx, testCaller(r), id)
	testassert.NoError(t, err)
	testassert.Require(t, len(result.CleanupFailures) == 1 && result.CleanupFailures[0].ExtensionId != nil && *result.CleanupFailures[0].ExtensionId == "probe_bad" && result.CleanupFailures[0].Code == protocol.CleanupFailureCodeUnavailable, "first deletion failures = %+v, want the owner's unavailable failure", result.CleanupFailures)
	testassert.Require(t, cleanupCalls.Load() == 1, "cleanup calls = %d, want the committing deletion's one attempt", cleanupCalls.Load())

	repeated, err := r.DeleteSession(ctx, testCaller(r), id)
	testassert.Require(t, errors.Is(err, harness.ErrNotFound), "repeated delete = (%+v, %v), want the ordinary not-found", repeated, err)
	testassert.Require(t, len(repeated.CleanupFailures) == 0, "repeated delete produced a cleanup result: %+v", repeated)
	testassert.Require(t, cleanupCalls.Load() == 1, "repeated delete replayed external cleanup: calls = %d", cleanupCalls.Load())
}

// TestDataServiceCleanupDisabledAndRemovedOwners proves a disabled or removed
// owner is an unavailable failure while the canonical deletion commits.
func TestDataServiceCleanupDisabledAndRemovedOwners(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		e := newOwnerEnv(t)
		var goodCalls, badCalls atomic.Int64
		good := newDataProbe("probe_good", func(context.Context, extension.CallContext) error { goodCalls.Add(1); return nil })
		bad := newDataProbe("probe_bad", func(context.Context, extension.CallContext) error { badCalls.Add(1); return nil })
		writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_good", "probe_bad"))
		e.fixtureStore(storage.NewMemory())
		ctx := context.Background()
		r, err := e.open(ctx, good.native, bad.native)
		testassert.NoError(t, err)
		defer r.Close(ctx)
		workspace := filepath.Join(e.dataDir, "disabled-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session, err := r.createSession(ctx, workspace, "solo")
		testassert.NoError(t, err)
		id := session.Identity.SessionID
		for _, probe := range []*dataProbe{good, bad} {
			view, calls := dataView(t, r, probe.capability, id, nativeGrant{})
			_, err := callData(t, calls, "claim_external_data")
			testassert.NoError(t, err)
			view.end()
		}
		writeServiceFile(t, e.configPath, strings.TrimSuffix(ownerConfigDocument(e.server.URL), "}")+`,"extensions":{"agents":{"enabled":true,"definitions":`+ownerAgentsDocument+`},"probe_good":{"enabled":true},"probe_bad":{"enabled":false}}}`)
		_, err = r.Reload(ctx, testCaller(r))
		testassert.NoError(t, err)
		{
			_, err := r.ArchiveSession(ctx, testCaller(r), id)
			testassert.NoError(t, err)
		}
		result, err := r.DeleteSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
		testassert.Require(t, len(result.CleanupFailures) == 1 && result.CleanupFailures[0].ExtensionId != nil && *result.CleanupFailures[0].ExtensionId == "probe_bad" && result.CleanupFailures[0].Code == "unavailable", "disabled owner result = %+v, want the unavailable failure", result.CleanupFailures)
		testassert.Require(t, goodCalls.Load() == 1 && badCalls.Load() == 0, "cleanup calls = (%d, %d), want only the installed owner invoked", goodCalls.Load(), badCalls.Load())
	})

	t.Run("removed", func(t *testing.T) {
		e := newOwnerEnv(t)
		var goodCalls atomic.Int64
		good := newDataProbe("probe_good", func(context.Context, extension.CallContext) error { goodCalls.Add(1); return nil })
		writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_good"))
		e.fixtureStore(storage.NewMemory())
		ctx := context.Background()
		r, err := e.open(ctx, good.native)
		testassert.NoError(t, err)
		defer r.Close(ctx)
		workspace := filepath.Join(e.dataDir, "removed-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session, err := r.createSession(ctx, workspace, "solo")
		testassert.NoError(t, err)
		id := session.Identity.SessionID
		// The owner was recorded by an earlier installation and is no longer
		// part of the active composition.
		testassert.NoError(t, r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			return h.ClaimExternalData(ctx, id, "probe_removed")
		}))
		{
			_, err := r.ArchiveSession(ctx, testCaller(r), id)
			testassert.NoError(t, err)
		}
		result, err := r.DeleteSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
		testassert.Require(t, len(result.CleanupFailures) == 1 && result.CleanupFailures[0].ExtensionId != nil && *result.CleanupFailures[0].ExtensionId == "probe_removed" && result.CleanupFailures[0].Code == "unavailable", "removed owner result = %+v, want the unavailable failure", result.CleanupFailures)
		testassert.Require(t, goodCalls.Load() == 0, "an unrelated owner cleanup ran")
	})
}

// TestDataServiceSweepReportsCleanupFailures proves the sweep's one producer
// joins external cleanup failures into its retained pass diagnostic while the
// committed deletion stays committed.
func TestDataServiceSweepReportsCleanupFailures(t *testing.T) {
	e := newOwnerEnv(t)
	bad := newDataProbe("probe_bad", func(context.Context, extension.CallContext) error { return errors.New("guest failure") })
	writeServiceFile(t, e.configPath, strings.TrimSuffix(dataConfig(e.server.URL, "probe_bad"), "}")+`,"sessions":{"auto_archive":true,"archive_after_days":1,"delete_after_archive_days":1}}`)
	e.fixtureStore(storage.NewMemory())
	ctx := context.Background()
	r, _, err := e.openSwept(ctx, bad.native)
	testassert.NoError(t, err)
	defer r.Close(ctx)
	workspace := filepath.Join(e.dataDir, "sweep-cleanup-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID
	view, calls := dataView(t, r, bad.capability, id, nativeGrant{})
	_, err = callData(t, calls, "claim_external_data")
	testassert.NoError(t, err)
	view.end()
	{
		_, err := r.ArchiveSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
	}
	stderr := captureSweepStderr(t)
	r.runSweepPass(ctx, time.Now().Add(100*time.Hour))
	out := stderr()
	testassert.Require(t, strings.Contains(out, cleanupUnavailableMessage) && strings.Contains(out, "probe_bad"), "sweep diagnostic = %q, want the external cleanup failure", out)
	{
		err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ReadSessionHeader(ctx, id)
			return err
		})
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "swept canonical deletion = %v, want it committed", err)
	}
}

// TestDataServiceCleanupJoinsShutdown proves an admitted cleanup callback is
// joined by Melta shutdown.
func TestDataServiceCleanupJoinsShutdown(t *testing.T) {
	e := newOwnerEnv(t)
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	cleanup := sync.OnceFunc(func() { close(release) })
	defer cleanup()
	parked := newDataProbe("probe_park", func(_ context.Context, _ extension.CallContext) error {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		return nil
	})
	writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_park"))
	e.fixtureStore(storage.NewMemory())
	ctx := context.Background()
	r, err := e.open(ctx, parked.native)
	testassert.NoError(t, err)
	workspace := filepath.Join(e.dataDir, "shutdown-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID
	view, calls := dataView(t, r, parked.capability, id, nativeGrant{})
	_, err = callData(t, calls, "claim_external_data")
	testassert.NoError(t, err)
	view.end()
	{
		_, err := r.ArchiveSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
	}
	deleteDone := make(chan error, 1)
	go func() {
		_, err := r.DeleteSession(context.Background(), testCaller(r), id)
		deleteDone <- err
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup callback never started")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	{
		err := r.Close(closeCtx)
		testassert.Require(t, errors.Is(err, context.DeadlineExceeded), "Close during parked cleanup = %v, want it to wait", err)
	}
	cleanup()
	{
		select {
		case err := <-deleteDone:
			testassert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("delete never returned after cleanup release")
		}
	}
	closeCtx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	testassert.NoError(t, r.Close(closeCtx2))
}

// operationDataTool is one native tool that reaches the data service inside an
// admitted Operation and parks before its terminal response.
type operationDataTool struct {
	arrived  chan string
	release  chan struct{}
	err      chan error
	sentinel string
}

func (t *operationDataTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

func (t *operationDataTool) Prepare(_ context.Context, tool extension.ToolContext, _ model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{
		Permissions:        []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: filepath.Join(tool.Workspace, "probe.txt")}},
		CanonicalWorkspace: tool.Workspace,
	}
}

func (t *operationDataTool) Execute(_ context.Context, tool extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	claim, err := tool.Calls.Call(context.Background(), "data", "claim_external_data", json.RawMessage(`{}`))
	if err != nil || string(claim.Value) != "null" {
		t.err <- fmt.Errorf("claim = (%+v, %v)", claim, err)
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "claim failed"}}
	}
	result, err := tool.Calls.Call(context.Background(), "data", "session_namespace", json.RawMessage(`{}`))
	if err != nil {
		t.err <- err
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "namespace failed"}}
	}
	var decoded struct {
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(result.Value, &decoded); err != nil {
		t.err <- err
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "namespace invalid"}}
	}
	if err := os.WriteFile(t.sentinel, []byte("external"), 0o600); err != nil {
		t.err <- err
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "external data failed"}}
	}
	t.arrived <- decoded.Directory
	<-t.release
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: "wrote"}}
}

// TestDataServiceOperationClaimAndCleanup proves an actual admitted Operation
// can claim external data while running, create it after the claim, and hold
// the execution's borrowed interval through its callback terminal response; a
// later canonical deletion invokes the real cleanup declaration on both
// stores and never replays it.
func TestDataServiceOperationClaimAndCleanup(t *testing.T) {
	for _, lifetime := range []extension.Lifetime{extension.LifetimeHarness, extension.LifetimeWorkspace, extension.LifetimeOperation, extension.LifetimeInvocation} {
		t.Run(string(lifetime), func(t *testing.T) {
			eachPrepStore(t, func(t *testing.T, store harness.Storage) {
				e := newOwnerEnv(t)
				sentinel := filepath.Join(e.dataDir, "external-sentinel")
				tool := &operationDataTool{arrived: make(chan string, 1), release: make(chan struct{}), err: make(chan error, 1), sentinel: sentinel}
				releaseTool := sync.OnceFunc(func() { close(tool.release) })
				defer releaseTool()
				var cleanupCalls atomic.Int64
				hostData, ok := extension.HostCapability(extension.ContractData)
				testassert.Require(t, ok, "host data capability is missing")
				native := extension.Native{
					Declaration: extension.Declaration{
						ID:           "probe_tool",
						Version:      "1",
						ConfigSchema: json.RawMessage(testConfigSchema),
						Components: []extension.Component{
							{ID: "probe-cleanup", Lifetime: extension.LifetimeHarness, Provides: []string{"cleanup.probe_tool"}},
							{ID: "probe-tool", Lifetime: lifetime, Provides: []string{"probe_tool"}, Requires: []extension.Binding{{
								Name: "data", Provider: extension.HostProvider, CapabilityID: extension.ContractData, Definition: hostData,
							}}},
						},
						Capabilities: []extension.Capability{extension.CleanupCapability("cleanup.probe_tool"), extension.ToolCapability("probe_tool")},
					},
					Describe: map[string]extension.ToolDescribe{"probe_tool": testDescribeTool("probe_tool")},
					Open: func(_ context.Context, info extension.InstanceContext) (extension.Instance, error) {
						switch info.ComponentID {
						case "probe-cleanup":
							return extension.Instance{Capabilities: map[string]extension.Implementation{"cleanup.probe_tool": {Service: map[string]extension.Handler{
								"cleanup": func(_ context.Context, _ extension.CallContext, _ json.RawMessage) (extension.Result, error) {
									cleanupCalls.Add(1)
									if err := os.Remove(sentinel); err != nil && !os.IsNotExist(err) {
										return extension.Result{}, err
									}
									return extension.Result{Value: json.RawMessage(`null`)}, nil
								},
							}}}}, nil
						case "probe-tool":
							return extension.Instance{Capabilities: map[string]extension.Implementation{"probe_tool": {Tool: tool}}}, nil
						}
						return extension.Instance{}, fmt.Errorf("unexpected component %q", info.ComponentID)
					},
				}
				writeServiceFile(t, e.configPath, strings.TrimSuffix(dataConfig(e.server.URL, "probe_tool"), "}")+`,"sessions":{"auto_archive":true,"archive_after_days":1,"delete_after_archive_days":1}}`)
				writeAgentDefinitions(t, e.configPath, `{"data_agent":{"model":"prov/m","system_prompt":"full","tools":["probe_tool"]}}`)
				e.fixtureStore(store)
				ctx := context.Background()
				r, err := e.open(ctx, native)
				testassert.NoError(t, err)
				defer r.Close(ctx)
				workspace := filepath.Join(e.dataDir, "operation-data-ws")
				testassert.NoError(t, os.MkdirAll(workspace, 0o700))
				session, err := r.createSession(ctx, workspace, "data_agent")
				testassert.NoError(t, err)
				id := session.Identity.SessionID
				var calls atomic.Int64
				e.server.setScript(func(context.Context, string) []string {
					if calls.Add(1) == 1 {
						return toolCallTurnEvents("call-1", "probe_tool", `{}`)
					}
					return textTurnEvents("done")
				})
				submitThroughRuntime(t, r, id, "op-data", "run")
				var directory string
				select {
				case directory = <-tool.arrived:
				case err := <-tool.err:
					t.Fatalf("tool data call: %v", err)
				case <-time.After(10 * time.Second):
					rec := readOperation(t, r, id, "op-data")
					t.Fatalf("tool never reached the data service: state=%+v", rec.State)
				}
				want := filepath.Join(e.dataDir, "code", id, "extensions", extensionDataHash("probe_tool"))
				testassert.Require(t, directory == want, "tool namespace = %q, want %q", directory, want)
				mustExist(t, sentinel, "the external sentinel created after the claim")
				{
					rec := readOperation(t, r, id, "op-data")
					testassert.Require(t, rec.State.Status == harness.OperationRunning, "operation state during the claim callback = %s, want running", rec.State.Status)
				}
				{
					owners := sessionOwners(t, store, id)
					testassert.Require(t, len(owners) == 1 && owners[0] == "probe_tool", "durable owners during the active Operation = %v, want [probe_tool]", owners)
				}
				{
					_, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, id))
					testassert.Require(t, errors.Is(err, errArtifactBusy), "the Operation's claim did not retain its borrowed execution interval: %v", err)
				}
				{
					_, err := r.DeleteSession(ctx, testCaller(r), id)
					testassert.Require(t, errors.Is(err, harness.ErrConflict), "delete during the active claim callback = %v, want the interval conflict", err)
				}
				r.runSweepPass(ctx, time.Now().Add(100*time.Hour))
				{
					err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
						_, err := h.ReadSessionHeader(ctx, id)
						return err
					})
					testassert.Require(t, err == nil, "sweep removed the Session under the active claim callback: %v", err)
				}
				releaseTool()
				awaitOperation(t, r, id, "op-data", harness.OperationSuccess)
				deadline := time.Now().Add(10 * time.Second)
				for {
					release, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, id))
					if err == nil {
						release()
						break
					}
					if !errors.Is(err, errArtifactBusy) || time.Now().After(deadline) {
						t.Fatalf("the completed execution never released its interval: %v", err)
					}
					time.Sleep(10 * time.Millisecond)
				}
				{
					_, err := r.ArchiveSession(ctx, testCaller(r), id)
					testassert.NoError(t, err)
				}
				result, err := r.DeleteSession(ctx, testCaller(r), id)
				testassert.NoError(t, err)
				testassert.Require(t, len(result.CleanupFailures) == 0, "delete after the active claim = %+v, want complete cleanup", result)
				testassert.Require(t, cleanupCalls.Load() == 1, "cleanup calls = %d, want the owner's actual cleanup once", cleanupCalls.Load())
				mustNotExist(t, sentinel, "the owner-cleaned external sentinel")
				{
					err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
						_, err := h.ReadSessionHeader(ctx, id)
						return err
					})
					testassert.Require(t, errors.Is(err, harness.ErrNotFound), "canonical deletion = %v, want it committed", err)
				}
				{
					repeated, err := r.DeleteSession(ctx, testCaller(r), id)
					testassert.Require(t, errors.Is(err, harness.ErrNotFound), "repeated delete = (%+v, %v), want the ordinary not-found", repeated, err)
					testassert.Require(t, cleanupCalls.Load() == 1, "repeated delete replayed cleanup: calls = %d", cleanupCalls.Load())
				}
			})
		})
	}
}

// nestedProviderNative is one nested service that reaches the data service;
// nestedCallerNative calls it through an ordinary declared binding.
func nestedProviderNative() extension.Native {
	hostData, _ := extension.HostCapability(extension.ContractData)
	return testNative("nested_provider", extension.LifetimeHarness, []extension.Capability{
		extension.ServiceCapability("nested.provider", "test.nested.provider", extension.ServiceIOMethod("namespace", `{"type":"object","additionalProperties":false}`, `{"type":"object","required":["directory"],"properties":{"directory":{"type":"string"}},"additionalProperties":false}`)),
	}, []extension.Binding{{Name: "data", Provider: extension.HostProvider, CapabilityID: extension.ContractData, Definition: hostData}}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"nested.provider": {Service: map[string]extension.Handler{
			"namespace": func(ctx context.Context, calls extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				return calls.Call(ctx, "data", "session_namespace", json.RawMessage(`{}`))
			},
		}}}}, nil
	})
}

func nestedCallerNative(provider extension.Capability) extension.Native {
	return testNative("nested_caller", extension.LifetimeHarness, []extension.Capability{
		extension.ServiceCapability("nested.caller", "test.nested.caller", extension.ServiceIOMethod("run", `{"type":"object","additionalProperties":false}`, `{"type":"object","required":["directory"],"properties":{"directory":{"type":"string"}},"additionalProperties":false}`)),
	}, []extension.Binding{{Name: "provider", Provider: "nested_provider/nested_provider#component", CapabilityID: provider.ID, Definition: provider.Definition()}}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"nested.caller": {Service: map[string]extension.Handler{
			"run": func(ctx context.Context, calls extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				return calls.Call(ctx, "provider", "namespace", json.RawMessage(`{}`))
			},
		}}}}, nil
	})
}

// TestDataServiceNestedCallBorrowsExecutionLease proves a nested declared
// provider call inherits the root callback's Session interval instead of
// attempting a second acquisition.
func TestDataServiceNestedCallBorrowsExecutionLease(t *testing.T) {
	e := newOwnerEnv(t)
	provider := nestedProviderNative()
	providerCapability := provider.Declaration.Capabilities[0]
	caller := nestedCallerNative(providerCapability)
	writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "nested_provider", "nested_caller"))
	e.fixtureStore(storage.NewMemory())
	ctx := context.Background()
	r, err := e.open(ctx, provider, caller)
	testassert.NoError(t, err)
	defer r.Close(ctx)
	workspace := filepath.Join(e.dataDir, "nested-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID
	releaseExecution, err := r.artifacts.acquireExecution(ctx, sessionArtifactDir(r.dataDir, id))
	testassert.NoError(t, err)
	lease := &artifactCallLease{intervals: r.artifacts, key: sessionArtifactDir(r.dataDir, id), borrowed: true, release: releaseExecution}
	snapshot := r.config.current()
	value := dataProbeValue(t, r, "nested.caller")
	view, calls := snapshot.composition.newView(value, context.Background(), callIdentity{
		session:  harness.SessionIdentity{SessionID: id},
		revision: snapshot.resources.revisionString(),
		lease:    lease,
	})
	result, err := calls.Call(context.Background(), "provider", "namespace", json.RawMessage(`{}`))
	testassert.Require(t, err == nil, "nested data call = %v, want the borrowed execution lease", err)
	var decoded struct {
		Directory string `json:"directory"`
	}
	testassert.NoError(t, json.Unmarshal(result.Value, &decoded))
	testassert.Require(t, decoded.Directory == filepath.Join(e.dataDir, "code", id, "extensions", extensionDataHash("nested_provider")), "nested directory = %q", decoded.Directory)
	view.end()
	releaseExecution()
}

// TestDataServiceCleanupCancellationReportsUnavailable proves caller
// cancellation during an external callback still commits the canonical
// deletion and reports the fixed unavailable failure.
func TestDataServiceCleanupCancellationReportsUnavailable(t *testing.T) {
	e := newOwnerEnv(t)
	arrived := make(chan struct{}, 1)
	probe := newDataProbe("probe_cancel", func(ctx context.Context, _ extension.CallContext) error {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	})
	writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_cancel"))
	e.fixtureStore(storage.NewMemory())
	r, err := e.open(context.Background(), probe.native)
	testassert.NoError(t, err)
	defer r.Close(context.Background())
	workspace := filepath.Join(e.dataDir, "cancel-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(context.Background(), workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID
	view, calls := dataView(t, r, probe.capability, id, nativeGrant{})
	_, err = callData(t, calls, "claim_external_data")
	testassert.NoError(t, err)
	view.end()
	{
		_, err := r.ArchiveSession(context.Background(), testCaller(r), id)
		testassert.NoError(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan protocol.DeletionResult, 1)
	go func() {
		result, _ := r.DeleteSession(ctx, testCaller(r), id)
		done <- result
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup callback never started")
	}
	cancel()
	select {
	case result := <-done:
		testassert.Require(t, len(result.CleanupFailures) == 1 && result.CleanupFailures[0].ExtensionId != nil && *result.CleanupFailures[0].ExtensionId == "probe_cancel" && result.CleanupFailures[0].Code == "unavailable", "canceled cleanup result = %+v, want one unavailable failure", result.CleanupFailures)
	case <-time.After(10 * time.Second):
		t.Fatal("delete never returned after caller cancellation")
	}
	{
		err := r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
			_, err := h.ReadSessionHeader(ctx, id)
			return err
		})
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "canonical deletion after canceled cleanup = %v, want it committed", err)
	}
}

// TestDataServiceClaimDeletedSessionRefused proves a deleted identity grants
// no claim and creates no directory, including beside the sibling Session that
// remains current.
func TestDataServiceClaimDeletedSessionRefused(t *testing.T) {
	e := newOwnerEnv(t)
	probe := newDataProbe("probe_a", func(context.Context, extension.CallContext) error { return nil })
	writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_a"))
	e.fixtureStore(storage.NewMemory())
	ctx := context.Background()
	r, err := e.open(ctx, probe.native)
	testassert.NoError(t, err)
	defer r.Close(ctx)
	workspace := filepath.Join(e.dataDir, "deleted-claim-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	session, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	id := session.Identity.SessionID
	sibling, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	siblingID := sibling.Identity.SessionID
	{
		_, err := r.ArchiveSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
	}
	{
		result, err := r.DeleteSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
		testassert.Require(t, len(result.CleanupFailures) == 0, "deletion = %+v", result)
	}
	view, calls := dataView(t, r, probe.capability, id, nativeGrant{})
	{
		_, err := callData(t, calls, "claim_external_data")
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "deleted claim = %v, want not_found", err)
	}
	view.end()
	mustNotExist(t, filepath.Join(e.dataDir, "code", id, "extensions", extensionDataHash("probe_a")), "the deleted identity's refused claim directory")
	siblingView, siblingCalls := dataView(t, r, probe.capability, siblingID, nativeGrant{})
	_, err = callData(t, siblingCalls, "claim_external_data")
	testassert.NoError(t, err)
	siblingView.end()
}

// TestDataServiceCleanupResultSiblings proves the producer treats a declared
// domain Failure and an invalid success exactly like a Go error: mixed owners
// are all attempted, each failure becomes the fixed unavailable detail, the
// canonical deletion stays committed, and no later result replays them.
func TestDataServiceCleanupResultSiblings(t *testing.T) {
	buildProbes := func() (good, domain, goerror, invalid *dataProbe, calls map[string]*atomic.Int64) {
		calls = map[string]*atomic.Int64{"good": {}, "domain": {}, "goerror": {}, "invalid": {}}
		good = newDataProbe("probe_good", func(context.Context, extension.CallContext) error {
			calls["good"].Add(1)
			return nil
		})
		domain = newDataProbe("probe_domain", func(context.Context, extension.CallContext) error {
			calls["domain"].Add(1)
			return nil
		})
		domain.cleanupOutcome = &extension.Result{Failure: &extension.Failure{Code: "domain", Message: "guest secret detail"}}
		goerror = newDataProbe("probe_goerror", func(context.Context, extension.CallContext) error {
			calls["goerror"].Add(1)
			return errors.New("guest go error")
		})
		invalid = newDataProbe("probe_invalid", func(context.Context, extension.CallContext) error {
			calls["invalid"].Add(1)
			return nil
		})
		invalid.cleanupOutcome = &extension.Result{Value: json.RawMessage(`{}`)}
		return good, domain, goerror, invalid, calls
	}
	claimAll := func(t *testing.T, r *Melta, store harness.Storage, id string, probes ...*dataProbe) {
		t.Helper()
		for _, probe := range probes {
			view, calls := dataView(t, r, probe.capability, id, nativeGrant{})
			_, err := callData(t, calls, "claim_external_data")
			testassert.NoError(t, err)
			view.end()
		}
		owners := sessionOwners(t, store, id)
		testassert.Require(t, len(owners) == len(probes), "owners = %v, want the four claimed extensions", owners)
	}

	t.Run("direct", func(t *testing.T) {
		e := newOwnerEnv(t)
		good, domain, goerror, invalid, calls := buildProbes()
		writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_good", "probe_domain", "probe_goerror", "probe_invalid"))
		store := storage.NewMemory()
		e.fixtureStore(store)
		ctx := context.Background()
		r, err := e.open(ctx, good.native, domain.native, goerror.native, invalid.native)
		testassert.NoError(t, err)
		defer r.Close(ctx)
		workspace := filepath.Join(e.dataDir, "result-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session, err := r.createSession(ctx, workspace, "solo")
		testassert.NoError(t, err)
		id := session.Identity.SessionID
		claimAll(t, r, store, id, good, domain, goerror, invalid)
		{
			_, err := r.ArchiveSession(ctx, testCaller(r), id)
			testassert.NoError(t, err)
		}
		result, err := r.DeleteSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
		testassert.Require(t, len(result.CleanupFailures) == 3, "failures = %+v, want domain, Go error and invalid-result siblings", result.CleanupFailures)
		for _, failure := range result.CleanupFailures {
			testassert.Require(t, failure.ExtensionId != nil && failure.Code == protocol.CleanupFailureCodeUnavailable && failure.Message == cleanupUnavailableMessage, "failure = %+v, want the fixed external detail", failure)
			testassert.Require(t, !strings.Contains(failure.Message, "guest"), "failure forwarded guest text: %+v", failure)
		}
		seen := map[string]bool{}
		for _, failure := range result.CleanupFailures {
			seen[*failure.ExtensionId] = true
		}
		testassert.Require(t, seen["probe_domain"] && seen["probe_goerror"] && seen["probe_invalid"] && !seen["probe_good"], "failure owners = %v, want the three failing siblings attempted", seen)
		for name, counter := range calls {
			testassert.Require(t, counter.Load() == 1, "%s cleanup calls = %d, want one attempt", name, counter.Load())
		}
		{
			err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ReadSessionHeader(ctx, id)
				return err
			})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "canonical deletion = %v, want it committed", err)
		}
		repeated, err := r.DeleteSession(ctx, testCaller(r), id)
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "repeated delete = (%+v, %v), want the ordinary not-found", repeated, err)
		testassert.Require(t, len(repeated.CleanupFailures) == 0, "repeated delete produced a cleanup result: %+v", repeated)
		for name, counter := range calls {
			testassert.Require(t, counter.Load() == 1, "%s cleanup calls after repeat = %d", name, counter.Load())
		}
	})

	t.Run("sweep", func(t *testing.T) {
		e := newOwnerEnv(t)
		good, domain, goerror, invalid, calls := buildProbes()
		writeServiceFile(t, e.configPath, strings.TrimSuffix(dataConfig(e.server.URL, "probe_good", "probe_domain", "probe_goerror", "probe_invalid"), "}")+`,"sessions":{"auto_archive":true,"archive_after_days":1,"delete_after_archive_days":1}}`)
		store := storage.NewMemory()
		e.fixtureStore(store)
		ctx := context.Background()
		r, _, err := e.openSwept(ctx, good.native, domain.native, goerror.native, invalid.native)
		testassert.NoError(t, err)
		defer r.Close(ctx)
		workspace := filepath.Join(e.dataDir, "sweep-result-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session, err := r.createSession(ctx, workspace, "solo")
		testassert.NoError(t, err)
		id := session.Identity.SessionID
		claimAll(t, r, store, id, good, domain, goerror, invalid)
		{
			_, err := r.ArchiveSession(ctx, testCaller(r), id)
			testassert.NoError(t, err)
		}
		stderr := captureSweepStderr(t)
		r.runSweepPass(ctx, time.Now().Add(100*time.Hour))
		out := stderr()
		for _, owner := range []string{"probe_domain", "probe_goerror", "probe_invalid"} {
			testassert.Require(t, strings.Contains(out, cleanupUnavailableMessage) && strings.Contains(out, owner), "sweep diagnostic = %q, want the unavailable failure for %s", out, owner)
		}
		for name, counter := range calls {
			testassert.Require(t, counter.Load() == 1, "%s cleanup calls = %d, want one attempt", name, counter.Load())
		}
		{
			err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
				_, err := h.ReadSessionHeader(ctx, id)
				return err
			})
			testassert.Require(t, errors.Is(err, harness.ErrNotFound), "swept canonical deletion = %v, want it committed", err)
		}
	})
}

type hookClaimProbe struct {
	lifetime extension.Lifetime
	boundary extension.HookBoundary
	sentinel string
	arrived  chan string
	release  chan struct{}
	err      chan error
}

func (p *hookClaimProbe) native(cleanup func() error) extension.Native {
	hostData, _ := extension.HostCapability(extension.ContractData)
	hookCapability := extension.HookCapability("hook.probe", extension.SingleHook(p.boundary, extension.HookModeEffectful))
	cleanupCapability := extension.CleanupCapability("cleanup.hook.probe")
	claimAndHold := func(ctx context.Context, call extension.CallContext) (string, error) {
		claimed, err := call.Call(ctx, "data", "claim_external_data", json.RawMessage(`{}`))
		if err != nil || string(claimed.Value) != "null" {
			return "", fmt.Errorf("claim = (%+v, %v)", claimed, err)
		}
		namespace, err := call.Call(ctx, "data", "session_namespace", json.RawMessage(`{}`))
		if err != nil {
			return "", err
		}
		var decoded struct {
			Directory string `json:"directory"`
		}
		if err := json.Unmarshal(namespace.Value, &decoded); err != nil {
			return "", err
		}
		if err := os.WriteFile(p.sentinel, []byte("external"), 0o600); err != nil {
			return "", err
		}
		p.arrived <- decoded.Directory
		<-p.release
		return decoded.Directory, nil
	}
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           "probe_hook",
			Version:      "1",
			ConfigSchema: json.RawMessage(testConfigSchema),
			Components: []extension.Component{
				{ID: "probe-cleanup", Lifetime: extension.LifetimeHarness, Provides: []string{"cleanup.hook.probe"}},
				{ID: "probe-hook", Lifetime: p.lifetime, Provides: []string{"hook.probe"}, Requires: []extension.Binding{{
					Name: "data", Provider: extension.HostProvider, CapabilityID: extension.ContractData, Definition: hostData,
				}}},
			},
			Capabilities: []extension.Capability{cleanupCapability, hookCapability},
		},
		Open: func(_ context.Context, info extension.InstanceContext) (extension.Instance, error) {
			switch info.ComponentID {
			case "probe-cleanup":
				return extension.Instance{Capabilities: map[string]extension.Implementation{"cleanup.hook.probe": {Service: map[string]extension.Handler{
					"cleanup": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
						if err := cleanup(); err != nil {
							return extension.Result{}, err
						}
						return extension.Result{Value: json.RawMessage(`null`)}, nil
					},
				}}}}, nil
			case "probe-hook":
				implementation := extension.Implementation{}
				switch p.boundary {
				case extension.BoundaryBeforeTool:
					implementation.BeforeTool = func(ctx context.Context, call extension.CallContext, toolCall model.ToolCall) (json.RawMessage, error) {
						if _, err := claimAndHold(ctx, call); err != nil {
							p.err <- err
							return nil, err
						}
						return toolCall.Arguments, nil
					}
				case extension.BoundaryRequest:
					implementation.Request = func(ctx context.Context, call extension.CallContext, _ harness.RequestHookInput) (harness.RequestConsequence, error) {
						if _, err := claimAndHold(ctx, call); err != nil {
							p.err <- err
							return harness.RequestConsequence{}, err
						}
						return harness.RequestConsequence{}, nil
					}
				}
				return extension.Instance{Capabilities: map[string]extension.Implementation{"hook.probe": implementation}}, nil
			}
			return extension.Instance{}, fmt.Errorf("unexpected component %q", info.ComponentID)
		},
	}
}

type immediateErrorTool struct{}

func (immediateErrorTool) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}
func (immediateErrorTool) Prepare(context.Context, extension.ToolContext, model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{Immediate: &harness.ToolOutcome{Result: model.ToolResult{Status: model.ResultError, Content: "hooked"}}}
}
func (immediateErrorTool) Execute(context.Context, extension.ToolContext, extension.ToolPlan) harness.ToolOutcome {
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "hooked"}}
}

// TestDataServiceRootHookClaimAcrossLifetimes proves effectful before_tool and
// request hooks implemented at Harness or Workspace lifetime receive the
// actual execution's borrowed interval, can claim and create external data,
// hold the interval through their response, and expose the claim to later
// shared cleanup.
func TestDataServiceRootHookClaimAcrossLifetimes(t *testing.T) {
	for _, boundary := range []extension.HookBoundary{extension.BoundaryBeforeTool, extension.BoundaryRequest} {
		for _, lifetime := range []extension.Lifetime{extension.LifetimeHarness, extension.LifetimeWorkspace} {
			t.Run(string(boundary)+"/"+string(lifetime), func(t *testing.T) {
				eachPrepStore(t, func(t *testing.T, store harness.Storage) {
					e := newOwnerEnv(t)
					sentinel := filepath.Join(e.dataDir, "hook-external-sentinel")
					probe := &hookClaimProbe{lifetime: lifetime, boundary: boundary, sentinel: sentinel, arrived: make(chan string, 1), release: make(chan struct{}), err: make(chan error, 1)}
					releaseHook := sync.OnceFunc(func() { close(probe.release) })
					defer releaseHook()
					var cleanupCalls atomic.Int64
					ids := []string{"probe_hook", "probe_tool"}
					writeServiceFile(t, e.configPath, dataConfig(e.server.URL, ids...))
					writeAgentDefinitions(t, e.configPath, `{"hook_agent":{"model":"prov/m","system_prompt":"full","tools":["probe_tool"],"capabilities":["hook.probe"]}}`)
					e.fixtureStore(store)
					tool := testToolNative("probe_tool", extension.LifetimeHarness, immediateErrorTool{}, testDescribeTool("probe_tool"))
					native := probe.native(func() error {
						cleanupCalls.Add(1)
						if err := os.Remove(sentinel); err != nil && !os.IsNotExist(err) {
							return err
						}
						return nil
					})
					ctx := context.Background()
					r, err := e.open(ctx, native, tool)
					testassert.NoError(t, err)
					defer r.Close(ctx)
					workspace := filepath.Join(e.dataDir, "hook-ws")
					testassert.NoError(t, os.MkdirAll(workspace, 0o700))
					session, err := r.createSession(ctx, workspace, "hook_agent")
					testassert.NoError(t, err)
					id := session.Identity.SessionID
					var calls atomic.Int64
					e.server.setScript(func(_ context.Context, body string) []string {
						if boundary == extension.BoundaryBeforeTool && calls.Add(1) == 1 {
							return toolCallTurnEvents("call-hook", "probe_tool", `{}`)
						}
						return textTurnEvents("done")
					})
					submitThroughRuntime(t, r, id, "op-hook", "run")
					var directory string
					select {
					case directory = <-probe.arrived:
					case err := <-probe.err:
						t.Fatalf("hook data call: %v", err)
					case <-time.After(10 * time.Second):
						rec := readOperation(t, r, id, "op-hook")
						t.Fatalf("hook never reached the data service: state=%+v", rec.State)
					}
					want := filepath.Join(e.dataDir, "code", id, "extensions", extensionDataHash("probe_hook"))
					testassert.Require(t, directory == want, "hook namespace = %q, want %q", directory, want)
					{
						rec := readOperation(t, r, id, "op-hook")
						testassert.Require(t, rec.State.Status == harness.OperationRunning, "operation state during hook = %s, want running", rec.State.Status)
					}
					mustExist(t, sentinel, "the external sentinel created by the hook after its claim")
					{
						owners := sessionOwners(t, store, id)
						testassert.Require(t, len(owners) == 1 && owners[0] == "probe_hook", "owners during the hook = %v, want [probe_hook]", owners)
					}
					{
						_, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, id))
						testassert.Require(t, errors.Is(err, errArtifactBusy), "the %s/%s hook lost the execution interval: %v", boundary, lifetime, err)
					}
					{
						_, err := r.DeleteSession(ctx, testCaller(r), id)
						testassert.Require(t, errors.Is(err, harness.ErrConflict), "delete during the %s/%s hook = %v, want the conflict", boundary, lifetime, err)
					}
					r.runSweepPass(ctx, time.Now().Add(100*time.Hour))
					{
						err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
							_, err := h.ReadSessionHeader(ctx, id)
							return err
						})
						testassert.Require(t, err == nil, "sweep removed the Session under the %s/%s hook", boundary, lifetime)
					}
					releaseHook()
					awaitOperation(t, r, id, "op-hook", harness.OperationSuccess)
					deadline := time.Now().Add(10 * time.Second)
					for {
						release, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, id))
						if err == nil {
							release()
							break
						}
						if !errors.Is(err, errArtifactBusy) || time.Now().After(deadline) {
							t.Fatalf("the completed hook execution never released its interval: %v", err)
						}
						time.Sleep(10 * time.Millisecond)
					}
					{
						_, err := r.ArchiveSession(ctx, testCaller(r), id)
						testassert.NoError(t, err)
					}
					result, err := r.DeleteSession(ctx, testCaller(r), id)
					testassert.NoError(t, err)
					testassert.Require(t, len(result.CleanupFailures) == 0, "delete after the hook = %+v, want complete cleanup", result)
					testassert.Require(t, cleanupCalls.Load() == 1, "cleanup calls = %d, want the hook owner's actual cleanup once", cleanupCalls.Load())
					mustNotExist(t, sentinel, "the owner-cleaned hook sentinel")
				})
			})
		}
	}
}

// nestedClaimCaller is one Workspace-lifetime root tool that delegates its
// claim to a different-lifetime Harness provider service.
type nestedClaimCaller struct {
	release chan struct{}
}

func (t *nestedClaimCaller) Normalize(_ extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}
func (t *nestedClaimCaller) Prepare(_ context.Context, tool extension.ToolContext, _ model.ToolCall) extension.ToolPlan {
	return extension.ToolPlan{
		Permissions:        []harness.PermissionRequest{{Permission: harness.PermissionFileRead, Target: filepath.Join(tool.Workspace, "nested.txt")}},
		CanonicalWorkspace: tool.Workspace,
	}
}
func (t *nestedClaimCaller) Execute(ctx context.Context, tool extension.ToolContext, _ extension.ToolPlan) harness.ToolOutcome {
	result, err := tool.Calls.Call(ctx, "provider", "run", json.RawMessage(`{}`))
	if err != nil {
		return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultError, Content: "provider failed"}}
	}
	<-t.release
	return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{Status: model.ResultSuccess, Content: string(result.Value)}}
}

// TestDataServiceNestedClaimAcrossLifetime proves the actual execution lease
// survives a declared hop from a Workspace-lifetime root tool to a
// Harness-lifetime provider that claims and holds external data.
func TestDataServiceNestedClaimAcrossLifetime(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		sentinel := filepath.Join(e.dataDir, "nested-external-sentinel")
		arrived := make(chan struct{}, 1)
		releaseProvider := make(chan struct{})
		releaseProviderOnce := sync.OnceFunc(func() { close(releaseProvider) })
		defer releaseProviderOnce()
		var cleanupCalls atomic.Int64
		hostData, _ := extension.HostCapability(extension.ContractData)
		provider := testNative("nested_provider", extension.LifetimeHarness, []extension.Capability{
			extension.ServiceCapability("nested.provider", "test.nested.provider", extension.ServiceIOMethod("run", `{"type":"object","additionalProperties":false}`, `{"type":"object","additionalProperties":false}`)),
			extension.CleanupCapability("cleanup.nested_provider"),
		}, []extension.Binding{{Name: "data", Provider: extension.HostProvider, CapabilityID: extension.ContractData, Definition: hostData}}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{
				"nested.provider": {Service: map[string]extension.Handler{"run": func(ctx context.Context, calls extension.CallContext, _ json.RawMessage) (extension.Result, error) {
					claimed, err := calls.Call(ctx, "data", "claim_external_data", json.RawMessage(`{}`))
					if err != nil || string(claimed.Value) != "null" {
						return extension.Result{}, fmt.Errorf("claim = (%+v, %v)", claimed, err)
					}
					if _, err := calls.Call(ctx, "data", "session_namespace", json.RawMessage(`{}`)); err != nil {
						return extension.Result{}, err
					}
					if err := os.WriteFile(sentinel, []byte("external"), 0o600); err != nil {
						return extension.Result{}, err
					}
					select {
					case arrived <- struct{}{}:
					default:
					}
					<-releaseProvider
					return extension.Result{Value: json.RawMessage(`{}`)}, nil
				}}},
				"cleanup.nested_provider": {Service: map[string]extension.Handler{"cleanup": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					cleanupCalls.Add(1)
					if err := os.Remove(sentinel); err != nil && !os.IsNotExist(err) {
						return extension.Result{}, err
					}
					return extension.Result{Value: json.RawMessage(`null`)}, nil
				}}},
			}}, nil
		})
		providerCapability := provider.Declaration.Capabilities[0]
		callerTool := &nestedClaimCaller{release: make(chan struct{})}
		releaseCaller := sync.OnceFunc(func() { close(callerTool.release) })
		defer releaseCaller()
		caller := testNative("nested_caller", extension.LifetimeWorkspace, []extension.Capability{extension.ToolCapability("nested_tool")}, []extension.Binding{{
			Name: "provider", Provider: "nested_provider/nested_provider#component", CapabilityID: providerCapability.ID, Definition: providerCapability.Definition(),
		}}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"nested_tool": {Tool: callerTool}}}, nil
		})
		caller.Describe["nested_tool"] = testDescribeTool("nested_tool")
		writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "nested_provider", "nested_caller"))
		writeAgentDefinitions(t, e.configPath, `{"nested_agent":{"model":"prov/m","system_prompt":"full","tools":["nested_tool"]}}`)
		e.fixtureStore(store)
		ctx := context.Background()
		r, err := e.open(ctx, provider, caller)
		testassert.NoError(t, err)
		defer r.Close(ctx)
		workspace := filepath.Join(e.dataDir, "nested-tool-ws")
		testassert.NoError(t, os.MkdirAll(workspace, 0o700))
		session, err := r.createSession(ctx, workspace, "nested_agent")
		testassert.NoError(t, err)
		id := session.Identity.SessionID
		var calls atomic.Int64
		e.server.setScript(func(context.Context, string) []string {
			if calls.Add(1) == 1 {
				return toolCallTurnEvents("call-nested", "nested_tool", `{}`)
			}
			return textTurnEvents("done")
		})
		submitThroughRuntime(t, r, id, "op-nested", "run")
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			rec := readOperation(t, r, id, "op-nested")
			t.Fatalf("provider never reached the data service: state=%+v", rec.State)
		}
		mustExist(t, sentinel, "the nested provider's external sentinel")
		{
			owners := sessionOwners(t, store, id)
			testassert.Require(t, len(owners) == 1 && owners[0] == "nested_provider", "owners during the nested provider = %v, want [nested_provider]", owners)
		}
		{
			_, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, id))
			testassert.Require(t, errors.Is(err, errArtifactBusy), "the nested provider lost the execution interval: %v", err)
		}
		{
			_, err := r.DeleteSession(ctx, testCaller(r), id)
			testassert.Require(t, errors.Is(err, harness.ErrConflict), "delete during the nested provider = %v, want the conflict", err)
		}
		releaseProviderOnce()
		releaseCaller()
		awaitOperation(t, r, id, "op-nested", harness.OperationSuccess)
		deadline := time.Now().Add(10 * time.Second)
		for {
			release, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, id))
			if err == nil {
				release()
				break
			}
			if !errors.Is(err, errArtifactBusy) || time.Now().After(deadline) {
				t.Fatalf("the nested execution never released its interval: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		{
			_, err := r.ArchiveSession(ctx, testCaller(r), id)
			testassert.NoError(t, err)
		}
		result, err := r.DeleteSession(ctx, testCaller(r), id)
		testassert.NoError(t, err)
		testassert.Require(t, len(result.CleanupFailures) == 0, "delete after the nested claim = %+v, want complete cleanup", result)
		testassert.Require(t, cleanupCalls.Load() == 1, "cleanup calls = %d, want the nested provider owner once", cleanupCalls.Load())
		mustNotExist(t, sentinel, "the owner-cleaned nested sentinel")
	})
}

// TestDataServiceCleanupHasNoSessionOrWorkAuthority proves a real deletion
// cleanup callback runs without Session/Operation identity, its declared data
// Session namespace and host child-launch bindings refuse, and submitting to
// the deleted identity creates no child or Operation. It has no chmod/root
// dependency.
func TestDataServiceCleanupHasNoSessionOrWorkAuthority(t *testing.T) {
	e := newOwnerEnv(t)
	store := storage.NewMemory()
	e.fixtureStore(store)
	hostData, _ := extension.HostCapability(extension.ContractData)
	hostLaunch, _ := extension.HostCapability(extension.ContractChildLaunch)
	var (
		observedSession  string
		observedOp       string
		dataErr          error
		launchErr        error
		cleanupCompleted atomic.Bool
	)
	native := testNative("probe_authority", extension.LifetimeHarness, []extension.Capability{extension.CleanupCapability("cleanup.probe_authority")}, []extension.Binding{
		{Name: "data", Provider: extension.HostProvider, CapabilityID: extension.ContractData, Definition: hostData},
		{Name: "launch", Provider: extension.HostProvider, CapabilityID: extension.ContractChildLaunch, Definition: hostLaunch},
	}, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"cleanup.probe_authority": {Service: map[string]extension.Handler{
			"cleanup": func(_ context.Context, call extension.CallContext, _ json.RawMessage) (extension.Result, error) {
				observedSession, observedOp = call.SessionID, call.OperationID
				if _, err := call.Call(context.Background(), "data", "session_namespace", json.RawMessage(`{}`)); err != nil {
					dataErr = err
				}
				if _, err := call.Call(context.Background(), "launch", "launch", json.RawMessage(`{"definition":{"extension_id":"agents","capability_id":"agents.definitions","definition_id":"solo"},"prompt":"no","operation_id":"child-op","max_concurrent":1,"output_limit":1}`)); err != nil {
					launchErr = err
				}
				cleanupCompleted.Store(true)
				return extension.Result{Value: json.RawMessage(`null`)}, nil
			},
		}}}}, nil
	})
	writeServiceFile(t, e.configPath, dataConfig(e.server.URL, "probe_authority"))
	ctx := context.Background()
	r, err := e.open(ctx, native)
	testassert.NoError(t, err)
	defer r.Close(ctx)
	workspace := filepath.Join(e.dataDir, "authority-ws")
	testassert.NoError(t, os.MkdirAll(workspace, 0o700))
	target, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	targetID := target.Identity.SessionID
	sibling, err := r.createSession(ctx, workspace, "solo")
	testassert.NoError(t, err)
	siblingID := sibling.Identity.SessionID
	{
		err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			return h.ClaimExternalData(ctx, targetID, "probe_authority")
		})
		testassert.NoError(t, err)
	}
	{
		_, err := r.ArchiveSession(ctx, testCaller(r), targetID)
		testassert.NoError(t, err)
	}
	result, err := r.DeleteSession(ctx, testCaller(r), targetID)
	testassert.NoError(t, err)
	testassert.Require(t, len(result.CleanupFailures) == 0, "cleanup result = %+v, want success", result)
	testassert.Require(t, cleanupCompleted.Load(), "cleanup callback did not complete")
	testassert.Require(t, observedSession == "" && observedOp == "", "cleanup identity = session %q operation %q, want empty", observedSession, observedOp)
	testassert.Require(t, errors.Is(dataErr, ErrClosed), "cleanup Session namespace error = %v, want the closed refusal", dataErr)
	testassert.Require(t, errors.Is(launchErr, ErrDenied), "cleanup child-launch error = %v, want the denied/unauthorized refusal", launchErr)
	{
		err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
			_, err := h.Submit(ctx, harness.SubmitRequest{
				SessionID: targetID, OperationID: "op-deleted", Origin: harness.InputOriginUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: "no"}}, Mode: harness.MessageModeRegular,
			})
			return err
		})
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "submit to the deleted identity = %v, want not_found", err)
	}
	{
		ids, err := store.ListSessionIDs(ctx)
		testassert.NoError(t, err)
		testassert.Require(t, len(ids) == 1 && ids[0] == siblingID, "sessions after cleanup/submit = %v, want only the sibling", ids)
	}
	{
		_, err := store.ReadRegisters(ctx, targetID)
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "deleted identity registers = %v, want absent", err)
	}
}

type retainedCleanupProbe struct {
	id        string
	first     bool
	arrived   chan struct{}
	release   chan struct{}
	secondTag chan string
	openOld   *atomic.Int64
	openNew   *atomic.Int64
	closeOld  *atomic.Int64
	closeNew  *atomic.Int64

	closedOld     atomic.Int64
	closeObserved atomic.Int64
}

func (p *retainedCleanupProbe) native() extension.Native {
	hostData, _ := extension.HostCapability(extension.ContractData)
	return testNative(p.id, extension.LifetimeHarness, []extension.Capability{
		extension.ServiceCapability("probe."+p.id, "test.retained", extension.PureMethod("tag", `{"type":"object","additionalProperties":false}`, `{"type":"object","required":["tag"],"properties":{"tag":{"type":"string"}},"additionalProperties":false}`)),
		extension.CleanupCapability("cleanup." + p.id),
	}, []extension.Binding{{Name: "data", Provider: extension.HostProvider, CapabilityID: extension.ContractData, Definition: hostData}}, func(_ context.Context, info extension.InstanceContext) (extension.Instance, error) {
		var section struct {
			Tag string `json:"tag"`
		}
		if err := json.Unmarshal(info.Section, &section); err != nil {
			return extension.Instance{}, err
		}
		tag := section.Tag
		if tag == "old" {
			p.openOld.Add(1)
		} else {
			p.openNew.Add(1)
		}
		return extension.Instance{
			Capabilities: map[string]extension.Implementation{
				"probe." + p.id: {Service: map[string]extension.Handler{"tag": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					value, err := json.Marshal(struct {
						Tag string `json:"tag"`
					}{Tag: tag})
					return extension.Result{Value: value}, err
				}}},
				"cleanup." + p.id: {Service: map[string]extension.Handler{"cleanup": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
					if p.first {
						select {
						case p.arrived <- struct{}{}:
						default:
						}
						<-p.release
					} else {
						p.closeObserved.Store(p.closeOld.Load())
						select {
						case p.secondTag <- tag:
						default:
						}
					}
					return extension.Result{Value: json.RawMessage(`null`)}, nil
				}}},
			},
			Close: func(context.Context, extension.CallContext) error {
				if tag == "old" {
					p.closedOld.Add(1)
					p.closeOld.Add(1)
				} else {
					p.closeNew.Add(1)
				}
				return nil
			},
		}, nil
	})
}

func taggedDataConfig(endpoint, aTag string, aEnabled bool, bTag string, bEnabled bool) string {
	enabled := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	return strings.TrimSuffix(ownerConfigDocument(endpoint), "}") + `,"extensions":{"agents":{"enabled":true,"definitions":` + ownerAgentsDocument + `},"probe_a":{"enabled":` + enabled(aEnabled) + `,"tag":` + fmt.Sprintf("%q", aTag) + `},"probe_b":{"enabled":` + enabled(bEnabled) + `,"tag":` + fmt.Sprintf("%q", bTag) + `}},"sessions":{"auto_archive":true,"archive_after_days":1,"delete_after_archive_days":1}}`
}

func currentProbeTag(t *testing.T, r *Melta, capabilityID string) string {
	t.Helper()
	snapshot := r.config.current()
	value := snapshot.resources.runtime.capabilities[capabilityID]
	testassert.Require(t, value != nil, "current capability %q is not open", capabilityID)
	result, err := snapshot.composition.invokeProvider(context.Background(), value, value.capability.Methods, "tag", json.RawMessage(`{}`), nil, callIdentity{revision: snapshot.resources.revisionString()}, false)
	testassert.NoError(t, err)
	var decoded struct {
		Tag string `json:"tag"`
	}
	testassert.NoError(t, json.Unmarshal(result.Value, &decoded))
	return decoded.Tag
}

// TestDataServiceCleanupBatchRetainsOriginalResourcesAcrossReload proves one
// cleanup producer's batch reservation keeps the original owner instances and
// configuration alive across a replacing/disabling reload: the first owner
// parks, canonical deletion is already committed, the second owner still runs
// the original instance after release, old resources retire once only after
// the batch, and current availability is the reloaded set.
func TestDataServiceCleanupBatchRetainsOriginalResourcesAcrossReload(t *testing.T) {
	for _, mode := range []string{"direct", "sweep"} {
		t.Run(mode, func(t *testing.T) {
			eachPrepStore(t, func(t *testing.T, store harness.Storage) {
				e := newOwnerEnv(t)
				var openOld, openNew, closeOld, closeNew atomic.Int64
				first := &retainedCleanupProbe{id: "probe_a", first: true, arrived: make(chan struct{}, 1), release: make(chan struct{}), secondTag: make(chan string, 1), openOld: &openOld, openNew: &openNew, closeOld: &closeOld, closeNew: &closeNew}
				second := &retainedCleanupProbe{id: "probe_b", secondTag: make(chan string, 1), openOld: &openOld, openNew: &openNew, closeOld: &closeOld, closeNew: &closeNew}
				releaseFirst := sync.OnceFunc(func() { close(first.release) })
				defer releaseFirst()
				writeServiceFile(t, e.configPath, taggedDataConfig(e.server.URL, "old", true, "old", true))
				e.fixtureStore(store)
				ctx := context.Background()
				var r *Melta
				var err error
				if mode == "sweep" {
					r, _, err = e.openSwept(ctx, first.native(), second.native())
				} else {
					r, err = e.open(ctx, first.native(), second.native())
				}
				testassert.NoError(t, err)
				defer r.Close(ctx)
				workspace := filepath.Join(e.dataDir, "retained-ws")
				testassert.NoError(t, os.MkdirAll(workspace, 0o700))
				session, err := r.createSession(ctx, workspace, "solo")
				testassert.NoError(t, err)
				id := session.Identity.SessionID
				for _, capability := range []string{"probe.probe_a", "probe.probe_b"} {
					view, calls := dataView(t, r, capability, id, nativeGrant{})
					_, err := callData(t, calls, "claim_external_data")
					testassert.NoError(t, err)
					view.end()
				}
				owners := sessionOwners(t, store, id)
				testassert.Require(t, len(owners) == 2 && owners[0] == "probe_a" && owners[1] == "probe_b", "owners = %v, want [probe_a probe_b]", owners)
				{
					_, err := r.ArchiveSession(ctx, testCaller(r), id)
					testassert.NoError(t, err)
				}

				triggerDone := make(chan struct{})
				go func() {
					defer close(triggerDone)
					if mode == "sweep" {
						r.runSweepPass(ctx, time.Now().Add(100*time.Hour))
						return
					}
					_, _ = r.DeleteSession(ctx, testCaller(r), id)
				}()
				select {
				case <-first.arrived:
				case <-time.After(10 * time.Second):
					t.Fatal("first owner cleanup never parked")
				}
				{
					_, err := store.ReadRegister(ctx, harness.RegisterKey{SessionID: id, Kind: harness.RegisterSession})
					testassert.Require(t, errors.Is(err, harness.ErrNotFound), "canonical deletion while parked = %v, want committed absence", err)
				}
				testassert.Require(t, closeOld.Load() == 0 && closeNew.Load() == 0, "resources closed before the parked batch: old=%d new=%d", closeOld.Load(), closeNew.Load())
				writeServiceFile(t, e.configPath, taggedDataConfig(e.server.URL, "new", true, "old", false))
				_, err = r.Reload(ctx, testCaller(r))
				testassert.NoError(t, err)
				testassert.Require(t, openOld.Load() == 2 && openNew.Load() == 1, "open instances = old %d new %d, want two original and one reloaded probe", openOld.Load(), openNew.Load())
				{
					select {
					case tag := <-second.secondTag:
						t.Fatalf("second owner ran (%q) before the first callback released", tag)
					default:
					}
				}
				testassert.Require(t, closeOld.Load() == 0, "old resources closed during the reload hold: %d", closeOld.Load())
				testassert.Require(t, currentProbeTag(t, r, "probe.probe_a") == "new", "current probe_a tag after reload = %q, want new", currentProbeTag(t, r, "probe.probe_a"))
				snapshot := r.config.current()
				testassert.Require(t, snapshot.resources.runtime.capabilities["probe.probe_b"] == nil, "disabled probe_b is still available in the reloaded set")
				releaseFirst()
				select {
				case <-triggerDone:
				case <-time.After(10 * time.Second):
					t.Fatal("cleanup batch never completed after release")
				}
				testassert.Require(t, closeOld.Load() == 2 && closeNew.Load() == 0, "old closes = %d new closes = %d, want the two original instances retired once", closeOld.Load(), closeNew.Load())
				testassert.Require(t, first.closedOld.Load() == 1 && second.closedOld.Load() == 1, "per-instance old closes = (%d, %d), want exactly one each", first.closedOld.Load(), second.closedOld.Load())
				testassert.Require(t, second.closeObserved.Load() == 0, "old resources closed before the second callback ran: %d", second.closeObserved.Load())
				select {
				case tag := <-second.secondTag:
					testassert.Require(t, tag == "old", "second owner cleanup tag = %q, want the captured original instance", tag)
				default:
					t.Fatal("second owner cleanup never ran")
				}
				testassert.Require(t, closeNew.Load() == 0, "reloaded instances closed during the batch")
				testassert.Require(t, currentProbeTag(t, r, "probe.probe_a") == "new", "current probe_a tag after batch = %q, want new", currentProbeTag(t, r, "probe.probe_a"))
			})
		})
	}
}
