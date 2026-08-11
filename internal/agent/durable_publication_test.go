package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// TestProviderEditPublishesThroughAtomicfs proves a provider edit writes
// config.json through atomicfs.Write: the publication's directory sync is
// observed through the atomicfs.SyncDirFunc hook. It fails against the old
// writeAgentConfigAtomic, which reimplemented the publication protocol with
// plain os calls and never consulted the sync hooks.
func TestProviderEditPublishesThroughAtomicfs(t *testing.T) {
	a := newProviderManagementAgent(t, editCfg)

	var synced []string
	atomicfs.SyncDirFunc = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { atomicfs.SyncDirFunc = nil })

	if err := a.SetProviderConfig("custom", ProviderConfigInput{BaseURL: "http://changed/v1"}); err != nil {
		t.Fatalf("SetProviderConfig: %v", err)
	}
	if len(synced) == 0 {
		t.Fatal("no directory sync observed: the config write did not publish through atomicfs.Write")
	}
	if want := filepath.Dir(a.configPath); synced[0] != want {
		t.Fatalf("synced dir = %q, want %q", synced[0], want)
	}
	st := providerStatusByID(t, a.ProviderList(), "custom")
	if st.BaseURL != "http://changed/v1" {
		t.Fatalf("base_url = %q, want http://changed/v1", st.BaseURL)
	}
}

// TestTokenWriteSurvivesInjectedCrash proves tokens.json is published through
// atomicfs.Write: with the temp-file sync injected to fail, the write aborts
// before publication and the previously persisted entries stay intact. It
// fails against the old persistTokensForSessionLocked, which wrote tokens.json
// in place with os.WriteFile and never consulted the sync hooks, so the
// injected failure left the new content in the file.
func TestTokenWriteSurvivesInjectedCrash(t *testing.T) {
	a := newLiveCatalogBackedTestAgent(t)
	unit := a.session
	writeTokenFile(t, unit, TokenEntry{Provider: "test", Model: "test-model", Input: 11, Output: 7, Known: true})

	injected := errors.New("injected crash mid-write")
	atomicfs.SyncFileFunc = func(*os.File) error { return injected }
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil })

	a.recordUsageForSession(unit, loop.Event{
		Model:      "test-model",
		ModelRef:   coremodel.ModelRef{Provider: "test", Model: "test-model"},
		UsageKnown: true,
		Input:      2,
	})

	data, err := os.ReadFile(filepath.Join(unit.store.Dir(), tokensFileName))
	if err != nil {
		t.Fatalf("read tokens: %v", err)
	}
	var entries []TokenEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("tokens.json is not valid JSON after the injected crash: %v", err)
	}
	if len(entries) != 1 || entries[0].Input != 11 || entries[0].Output != 7 {
		t.Fatalf("persisted entries = %#v, want the pre-crash entry intact (input 11, output 7)", entries)
	}
	dirEntries, _ := os.ReadDir(unit.store.Dir())
	for _, e := range dirEntries {
		if strings.HasPrefix(e.Name(), tokensFileName+".tmp-") {
			t.Fatalf("leftover temp %q after the aborted write", e.Name())
		}
	}
}

// TestTokenWriteFailureLeavesTotalsAndEmitsWarning proves a failed token
// publication changes nothing durable or live and reports once: the in-memory
// totals keep their pre-failure values, the on-disk file keeps its pre-failure
// entries, no usage event is emitted for the failed delta, exactly one
// protocol warning is added, and exactly one stderr line names the session and
// the error. It then proves the failure leaves no pending delta: the next
// successful event applies only its own contribution. The inactive_store
// subtest proves the same contract when the unit's store is not active:
// persistence refuses with snapshot.ErrNoSession, disk/memory/context stay
// unchanged, no usage event is emitted, the report is one warning plus one
// stderr line, and later events still apply nothing. It fails against the old
// memory-before-write mutation, which applied the delta to live totals and
// emitted the usage event before the write, so live totals diverged from disk
// and the failure was silent.
func TestTokenWriteFailureLeavesTotalsAndEmitsWarning(t *testing.T) {
	a := newLiveCatalogBackedTestAgent(t)
	unit := a.session
	writeTokenFile(t, unit, TokenEntry{Provider: "test", Model: "test-model", Input: 11, Output: 7, Known: true})
	a.loadTokensFromDiskForSession(unit)

	var usage []Event
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventUsage {
			usage = append(usage, ev)
		}
	})

	injected := errors.New("injected token write failure")
	atomicfs.SyncFileFunc = func(*os.File) error { return injected }
	t.Cleanup(func() { atomicfs.SyncFileFunc = nil })
	stderr := captureStderr(t)
	sessionID := unit.store.SessionID()

	a.recordUsageForSession(unit, loop.Event{
		Model:      "test-model",
		ModelRef:   coremodel.ModelRef{Provider: "test", Model: "test-model"},
		UsageKnown: true,
		Input:      2,
	})

	// Live totals unchanged.
	unit.tokensMu.Lock()
	entry := unit.tokens["test/test-model"]
	unit.tokensMu.Unlock()
	if entry == nil || entry.Input != 11 || entry.Output != 7 || entry.Cache != 0 {
		t.Fatalf("live totals after failed write = %#v, want the pre-failure 11/7", entry)
	}
	// Durable totals unchanged.
	var persisted []TokenEntry
	data, err := os.ReadFile(filepath.Join(unit.store.Dir(), tokensFileName))
	if err != nil {
		t.Fatalf("read tokens: %v", err)
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("tokens.json invalid after the failed write: %v", err)
	}
	if len(persisted) != 1 || persisted[0].Input != 11 || persisted[0].Output != 7 {
		t.Fatalf("persisted entries = %#v, want the pre-failure 11/7", persisted)
	}
	// No usage event for the failed delta.
	if len(usage) != 0 {
		t.Fatalf("captured %d usage events for a failed write, want 0", len(usage))
	}
	// Exactly one protocol warning naming the persistence failure.
	var warned bool
	for _, w := range a.CurrentWarnings() {
		if w.Kind == "protocol_warning" && strings.Contains(w.Message, "failed to persist token usage") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("warnings = %#v, want one failed-to-persist protocol warning", a.CurrentWarnings())
	}
	// Exactly one stderr line naming the session and the error.
	lines := strings.Split(strings.TrimSuffix(stderr(), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "failed to persist token usage for session "+sessionID) || !strings.Contains(lines[0], "injected token write failure") {
		t.Fatalf("stderr = %q, want exactly one failed-to-persist diagnostic for session %s", lines, sessionID)
	}

	// The failed delta is not retried: the next successful event applies only
	// its own contribution (11 + 3, not 11 + 2 + 3).
	atomicfs.SyncFileFunc = nil
	a.recordUsageForSession(unit, loop.Event{
		Model:      "test-model",
		ModelRef:   coremodel.ModelRef{Provider: "test", Model: "test-model"},
		UsageKnown: true,
		Input:      3,
	})
	unit.tokensMu.Lock()
	entry = unit.tokens["test/test-model"]
	unit.tokensMu.Unlock()
	if entry.Input != 14 || entry.Output != 7 {
		t.Fatalf("live totals after the next delta = %#v, want 14/7 (own delta only)", entry)
	}
	data, err = os.ReadFile(filepath.Join(unit.store.Dir(), tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].Input != 14 || persisted[0].Output != 7 {
		t.Fatalf("persisted entries after the next delta = %#v, want 14/7", persisted)
	}
	if len(usage) != 1 {
		t.Fatalf("usage events = %d, want 1 for the successful delta only", len(usage))
	}
}

// TestTokenInactiveStoreRefusesAndLeavesTotals proves the inactive-store
// publication contract: persistence refuses with snapshot.ErrNoSession, so a
// usage record against a unit whose store is no longer active leaves
// disk/memory/context unchanged, emits no usage event, reports exactly one
// protocol warning and one stderr line, and retains no pending delta — a later
// event on the same inactive store applies nothing either.
func TestTokenInactiveStoreRefusesAndLeavesTotals(t *testing.T) {
	a := newLiveCatalogBackedTestAgent(t)
	unit := a.session
	writeTokenFile(t, unit, TokenEntry{Provider: "test", Model: "test-model", Input: 11, Output: 7, Known: true})
	a.loadTokensFromDiskForSession(unit)
	tokensPath := filepath.Join(unit.store.Dir(), tokensFileName)

	var usage []Event
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventUsage {
			usage = append(usage, ev)
		}
	})

	unit.tokensMu.Lock()
	unit.lastContextUsed = 77
	unit.tokensMu.Unlock()
	unit.store.Detach()
	stderr := captureStderr(t)

	a.recordUsageForSession(unit, loop.Event{
		Model:      "test-model",
		ModelRef:   coremodel.ModelRef{Provider: "test", Model: "test-model"},
		UsageKnown: true,
		Input:      2,
	})

	// Memory and context unchanged.
	unit.tokensMu.Lock()
	entry := unit.tokens["test/test-model"]
	context := unit.lastContextUsed
	unit.tokensMu.Unlock()
	if entry == nil || entry.Input != 11 || entry.Output != 7 || entry.Cache != 0 {
		t.Fatalf("live totals after inactive-store record = %#v, want the pre-failure 11/7", entry)
	}
	if context != 77 {
		t.Fatalf("lastContextUsed after inactive-store record = %d, want 77", context)
	}
	// Disk unchanged.
	data, err := os.ReadFile(tokensPath)
	if err != nil {
		t.Fatalf("read tokens: %v", err)
	}
	var persisted []TokenEntry
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("tokens.json invalid after the inactive-store record: %v", err)
	}
	if len(persisted) != 1 || persisted[0].Input != 11 || persisted[0].Output != 7 {
		t.Fatalf("persisted entries = %#v, want the pre-failure 11/7", persisted)
	}
	// No usage event.
	if len(usage) != 0 {
		t.Fatalf("captured %d usage events for an inactive store, want 0", len(usage))
	}
	// Exactly one protocol warning naming the persistence failure.
	var warned bool
	for _, w := range a.CurrentWarnings() {
		if w.Kind == "protocol_warning" && strings.Contains(w.Message, "failed to persist token usage") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("warnings = %#v, want one failed-to-persist protocol warning", a.CurrentWarnings())
	}
	// Exactly one stderr line naming the refusal.
	lines := strings.Split(strings.TrimSuffix(stderr(), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "failed to persist token usage for session") || !strings.Contains(lines[0], "no session open") {
		t.Fatalf("stderr = %q, want exactly one inactive-store diagnostic", lines)
	}

	// No pending delta or retry: a later event on the same inactive store
	// also refuses and applies nothing.
	a.recordUsageForSession(unit, loop.Event{
		Model:      "test-model",
		ModelRef:   coremodel.ModelRef{Provider: "test", Model: "test-model"},
		UsageKnown: true,
		Input:      3,
	})
	unit.tokensMu.Lock()
	entry = unit.tokens["test/test-model"]
	unit.tokensMu.Unlock()
	if entry == nil || entry.Input != 11 || entry.Output != 7 {
		t.Fatalf("live totals after the second inactive-store record = %#v, want 11/7 (no accumulated delta)", entry)
	}
	data, err = os.ReadFile(tokensPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].Input != 11 || persisted[0].Output != 7 {
		t.Fatalf("persisted entries after the second inactive-store record = %#v, want 11/7", persisted)
	}
}
