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
