package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	agentcfg "github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/memory"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/tool"
)

func newCatalogBackedTestAgent(t *testing.T) *Agent {
	t.Helper()
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 },
        "alt-model": { "name": "Alt Model", "context_window": 4096, "max_output_tokens": 512 },
        "hidden-model": { "name": "Hidden Model", "context_window": 2048, "max_output_tokens": 256, "hidden": true }
      }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAgentsTestConfig(t, configPath, `{"primary": {"model": "test/test-model"}}`)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

func TestAgentNewAllowsUnconfiguredAndDisconnectedDefaults(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
	}{
		{
			name: "empty config",
			cfg:  `{"providers": {}, "default_model": ""}`,
		},
		{
			name: "default provider missing key",
			cfg: `{
  "providers": {
    "test": {
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_MISSING_KEY" },
      "discovery": false,
      "models": { "test-model": { "context_window": 8192 } }
    }
  },
  "default_model": "test/test-model"
}`,
		},
		{
			name: "mistyped default model",
			cfg: `{
  "providers": {
    "test": {
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "" },
      "discovery": false,
      "models": { "test-model": { "context_window": 8192 } }
    }
  },
  "default_model": "test/missing-model"
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			projectRoot := t.TempDir()
			configPath := filepath.Join(home, ".lightcode", "config.json")
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, []byte(tc.cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}
			cur := a.CurrentModel()
			if cur.Ref != "" || cur.Provider != "" || cur.Model != "" {
				t.Fatalf("CurrentModel = %#v, want no active model", cur)
			}
		})
	}
}

func TestProviderConnectedPredicate(t *testing.T) {
	t.Setenv("LIGHTCODE_CONNECTED_KEY", "test-key")
	if providerConnected(nil) {
		t.Fatal("nil provider connected")
	}
	keyed := &catalog.Provider{Transport: catalog.Transport{APIKeyEnv: "LIGHTCODE_CONNECTED_KEY"}, Models: map[string]*catalog.Model{"m": {ContextWindow: 8192}}}
	if !providerConnected(keyed) {
		t.Fatal("keyed provider with usable model and key should be connected")
	}
	keyedMissing := &catalog.Provider{Transport: catalog.Transport{APIKeyEnv: "LIGHTCODE_MISSING_KEY"}, Models: map[string]*catalog.Model{"m": {ContextWindow: 8192}}}
	if providerConnected(keyedMissing) {
		t.Fatal("keyed provider without key should not be connected")
	}
	keyless := &catalog.Provider{Transport: catalog.Transport{BaseURL: "http://127.0.0.1:9/v1"}, Models: map[string]*catalog.Model{"m": {ContextWindow: 8192}}}
	if !providerConnected(keyless) {
		t.Fatal("keyless provider with base_url and usable model should be connected")
	}
	incomplete := &catalog.Provider{Transport: catalog.Transport{BaseURL: "http://127.0.0.1:9/v1"}, Models: map[string]*catalog.Model{"m": {ContextWindow: 0}}}
	if providerConnected(incomplete) {
		t.Fatal("provider with only incomplete models should not be connected")
	}
}

func TestAgentSetupWarningsForUnconfiguredStartup(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	configPath := filepath.Join(home, ".lightcode", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"providers": {}, "default_model": ""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	warnings := a.CurrentWarnings()
	if !hasWarningKind(warnings, "setup_no_provider") || !hasWarningKind(warnings, "setup_no_model") {
		t.Fatalf("warnings = %#v, want setup provider/model warnings", warnings)
	}
}

func TestAgentDegradedMemoryStartupUsesHomeAndKeepsToolsAvailable(t *testing.T) {
	t.Setenv("LIGHTCODE_DEGRADED_MEMORY_KEY", "test-key")
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_DEGRADED_MEMORY_KEY" },
      "discovery": false,
      "models": { "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 } }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	projectsRoot := filepath.Join(home, ".lightcode", "projects")
	absProjectRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := project.EnsureForPath(projectsRoot, absProjectRoot)
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	memoriesDir := filepath.Join(projectsRoot, proj.ID, "memories")
	if err := os.MkdirAll(memoriesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staleMemoryPath := filepath.Join(memoriesDir, "20260626-existing.md")
	if err := os.WriteFile(staleMemoryPath, []byte("---\ntitle: Existing\ncreated_at: 2026-06-26T00:00:00Z\n---\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldNewMemoryEmbedder := newMemoryEmbedder
	var gotHome string
	newMemoryEmbedder = func(homeArg string) (*memory.Embedder, error) {
		gotHome = homeArg
		return nil, fmt.Errorf("forced embedder failure")
	}
	t.Cleanup(func() {
		newMemoryEmbedder = oldNewMemoryEmbedder
	})

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if gotHome != home {
		t.Fatalf("NewEmbedder home = %q, want %q", gotHome, home)
	}
	for _, name := range []string{"save_memory", "search_memory", "search_history"} {
		if _, ok := a.registry.Get(name); !ok {
			t.Fatalf("%s not registered", name)
		}
		if !a.registry.Advertises(name, nil) {
			t.Fatalf("%s is not model-visible", name)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	if !hasWarningKind(a.CurrentWarnings(), "setup_embedder_degraded") {
		t.Fatalf("warnings = %#v, want setup_embedder_degraded", a.CurrentWarnings())
	}
	if _, err := os.Stat(strings.TrimSuffix(staleMemoryPath, ".md") + ".vec"); !os.IsNotExist(err) {
		t.Fatalf("stale memory vec stat error = %v, want not exist", err)
	}

	saveTool, _ := a.registry.Get("save_memory")
	if wrapped, ok := saveTool.(interface{ WrappedTool() tool.Tool }); ok {
		saveTool = wrapped.WrappedTool()
	}
	out, err := saveTool.Execute(context.Background(), map[string]any{"title": "New", "content": "Body"})
	if err != nil {
		t.Fatalf("save_memory Execute returned error: %v", err)
	}
	if !strings.Contains(out, "memory embedder unavailable") {
		t.Fatalf("save_memory output = %q, want unavailable error", out)
	}
	matches, err := filepath.Glob(filepath.Join(memoriesDir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != staleMemoryPath {
		t.Fatalf("memory files after save_memory = %v, want only stale memory", matches)
	}
}

func TestAgentSetDefaultModelWritesConfigAndUpdatesWarnings(t *testing.T) {
	t.Setenv("LIGHTCODE_SET_DEFAULT_KEY", "test-key")
	home := t.TempDir()
	projectRoot := t.TempDir()
	configPath := filepath.Join(home, ".lightcode", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_SET_DEFAULT_KEY" },
      "discovery": false,
      "models": { "test-model": { "name": "Test Model", "context_window": 8192 } }
    }
  },
  "compaction": { "threshold_pct": 0.9 }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	if !hasWarningKind(a.CurrentWarnings(), "setup_no_model") {
		t.Fatalf("warnings before SetDefaultModel = %#v, want setup_no_model", a.CurrentWarnings())
	}
	if err := a.SetDefaultModel("test/test-model"); err != nil {
		t.Fatalf("SetDefaultModel returned error: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "default_model") {
		t.Fatalf("config after SetDefaultModel unexpectedly contains default_model: %s", data)
	}
	agentsData, err := os.ReadFile(agentcfg.PathForConfig(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agentsData), `"model": "test/test-model"`) {
		t.Fatalf("agents config after SetDefaultModel = %s, want primary.model", agentsData)
	}
	if hasWarningKind(a.CurrentWarnings(), "setup_no_model") {
		t.Fatalf("warnings after SetDefaultModel = %#v, did not expect setup_no_model", a.CurrentWarnings())
	}
	entries := a.AllModelList()
	if len(entries) != 1 || entries[0].Ref != "test/test-model" || !entries[0].Default {
		t.Fatalf("AllModelList after SetDefaultModel = %#v, want default flag on test/test-model", entries)
	}
	cur := a.CurrentModel()
	if cur.Ref != "test/test-model" {
		t.Fatalf("CurrentModel after SetDefaultModel = %#v, want lazy default activation", cur)
	}
}

func TestAgentNewSessionUsesUpdatedPrimaryModelButExistingSessionKeepsModel(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	cur := a.CurrentModel()
	if cur.Ref != "test/test-model" {
		t.Fatalf("initial CurrentModel = %#v, want test/test-model", cur)
	}
	appendUserTurn(t, a, "first")
	firstID := a.SessionCurrent().ID
	if firstID == "" {
		t.Fatal("first session id is empty")
	}
	firstMeta, err := a.store.Meta()
	if err != nil {
		t.Fatalf("first session meta: %v", err)
	}
	if firstMeta.Provider != "test" || firstMeta.Model != "test-model" {
		t.Fatalf("first session model = %s/%s, want test/test-model", firstMeta.Provider, firstMeta.Model)
	}

	if err := a.SetDefaultModel("test/alt-model"); err != nil {
		t.Fatalf("SetDefaultModel returned error: %v", err)
	}
	if cur := a.CurrentModel(); cur.Ref != "test/test-model" {
		t.Fatalf("current session model changed after primary model write: %#v", cur)
	}

	if err := a.SessionNew(); err != nil {
		t.Fatalf("SessionNew returned error: %v", err)
	}
	appendUserTurn(t, a, "second")
	secondMeta, err := a.store.Meta()
	if err != nil {
		t.Fatalf("second session meta: %v", err)
	}
	if secondMeta.Provider != "test" || secondMeta.Model != "alt-model" {
		t.Fatalf("new session model = %s/%s, want test/alt-model", secondMeta.Provider, secondMeta.Model)
	}

	if err := a.SessionSwitch(firstID); err != nil {
		t.Fatalf("SessionSwitch first returned error: %v", err)
	}
	if cur := a.CurrentModel(); cur.Ref != "test/test-model" {
		t.Fatalf("existing session model after switch = %#v, want test/test-model", cur)
	}
}

func TestAgentModelWritesRefreshTaskToolAgentConfig(t *testing.T) {
	tests := []struct {
		name string
		set  func(*Agent) error
	}{
		{
			name: "SetDefaultModel",
			set:  func(a *Agent) error { return a.SetDefaultModel("test/alt-model") },
		},
		{
			name: "SwitchModel",
			set: func(a *Agent) error {
				appendUserTurn(t, a, "activate session")
				return a.SwitchModel("test/alt-model")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newCatalogBackedTestAgent(t)
			before, err := a.taskToolInst.resolveAgentType("secondary")
			if err != nil {
				t.Fatalf("resolve secondary before model write: %v", err)
			}
			if before.Model != "test/test-model" {
				t.Fatalf("secondary model before write = %q, want test/test-model", before.Model)
			}

			if err := tc.set(a); err != nil {
				t.Fatalf("%s returned error: %v", tc.name, err)
			}

			after, err := a.taskToolInst.resolveAgentType("secondary")
			if err != nil {
				t.Fatalf("resolve secondary after model write: %v", err)
			}
			if after.Model != "test/alt-model" {
				t.Fatalf("secondary model after %s = %q, want updated primary model test/alt-model", tc.name, after.Model)
			}
		})
	}
}

func TestAgentRuntimeConfigRoundTripWritesReloadsAndExcludesMasterBooleans(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	var root map[string]any
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	objectMap(root, "subagents")["model"] = "test/stale-model"
	if err := writeAgentConfigAtomic(a.configPath, root); err != nil {
		t.Fatal(err)
	}

	settings := a.GetRuntimeConfig()
	settings.Sessions.ArchiveAfterDays = 14
	settings.Sessions.DeleteAfterArchiveDays = 21
	settings.Compaction.ThresholdPct = 0.75
	settings.Subagents.MaxConcurrent = 3
	settings.Tools.MaxOutputBytes = 32768
	settings.Tools.ReadMaxLines = 10
	settings.Tools.ReadLineMaxChars = 7000
	settings.Tools.CommandTimeout = 90
	settings.Tools.MaxBackgroundProcesses = 1

	if err := a.SetRuntimeConfig(settings); err != nil {
		t.Fatalf("SetRuntimeConfig returned error: %v", err)
	}
	got := a.GetRuntimeConfig()
	if got.Sessions.ArchiveAfterDays != 14 || got.Sessions.DeleteAfterArchiveDays != 21 || got.Compaction.ThresholdPct != 0.75 {
		t.Fatalf("runtime config after set = %#v", got)
	}
	if got.Subagents.MaxConcurrent != 3 || got.Tools.CommandTimeout != 90 || got.Tools.MaxBackgroundProcesses != 1 {
		t.Fatalf("runtime tool/subagent config after set = %#v", got)
	}
	data, err = os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`"archive_after_days": 14`, `"delete_after_archive_days": 21`, `"threshold_pct": 0.75`, `"max_concurrent": 3`, `"command_timeout": 90`} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"summarizer_model"`) || strings.Contains(text, `"model": "test/stale-model"`) {
		t.Fatalf("config write should remove removed model fields:\n%s", text)
	}
	if strings.Contains(text, `"auto_archive"`) || strings.Contains(text, `"enabled"`) || strings.Contains(text, `"permissions"`) {
		t.Fatalf("config write should not add excluded runtime fields:\n%s", text)
	}
	defer a.procMgr.KillAll()
	if _, err := a.procMgr.Start("sleep 5", 0); err != nil {
		t.Fatalf("first background process start returned error: %v", err)
	}
	if _, err := a.procMgr.Start("sleep 5", 0); err == nil {
		t.Fatal("second background process start returned nil with updated max_background_processes=1")
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload returned error: %v", err)
	}
	if got := a.GetRuntimeConfig(); got.Tools.CommandTimeout != 90 || got.Subagents.MaxConcurrent != 3 {
		t.Fatalf("runtime config after reload = %#v", got)
	}
}

func TestAgentSetRuntimeConfigDoesNotTriggerDiscoveryHTTP(t *testing.T) {
	var discoveryCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discoveryCalls.Add(1)
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected discovery path %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"remote-model","name":"Remote Model","context_window":12288,"max_output_tokens":3072}]}`)
	}))
	t.Cleanup(server.Close)

	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyEnv := "LIGHTCODE_RUNTIME_DISCOVERY_KEY"
	t.Setenv(keyEnv, "")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": %q, "api_key_env": %q },
      "discovery": true,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "test/test-model"
}`, server.URL+"/v1", keyEnv)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	if calls := discoveryCalls.Load(); calls != 0 {
		t.Fatalf("discovery calls during disconnected startup = %d, want 0", calls)
	}

	t.Setenv(keyEnv, "test-key")
	settings := a.GetRuntimeConfig()
	settings.Tools.CommandTimeout = 91
	if err := a.SetRuntimeConfig(settings); err != nil {
		t.Fatalf("SetRuntimeConfig returned error: %v", err)
	}
	if calls := discoveryCalls.Load(); calls != 0 {
		t.Fatalf("discovery calls during SetRuntimeConfig = %d, want 0", calls)
	}
	if got := a.GetRuntimeConfig(); got.Tools.CommandTimeout != 91 {
		t.Fatalf("runtime config after SetRuntimeConfig = %#v, want command_timeout=91", got)
	}
}

func TestAgentSetRuntimeConfigRejectsInvalidValues(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	settings := a.GetRuntimeConfig()
	settings.Compaction.ThresholdPct = 1
	if err := a.SetRuntimeConfig(settings); err == nil {
		t.Fatal("SetRuntimeConfig returned nil for invalid threshold")
	}
	settings = a.GetRuntimeConfig()
	settings.Tools.CommandTimeout = 0
	if err := a.SetRuntimeConfig(settings); err == nil {
		t.Fatal("SetRuntimeConfig returned nil for invalid command timeout")
	}
}

func TestAgentSummarizerUsesCompactAgentModel(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	writeAgentsTestConfig(t, a.configPath, `{
  "primary": { "model": "test/test-model" },
  "compact": { "model": "test/alt-model" }
}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload returned error: %v", err)
	}
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	compactUnit, window, err := a.compactRunningUnitForSession(a.session)
	if err != nil {
		t.Fatalf("compactRunningUnitForSession: %v", err)
	}
	t.Cleanup(func() { _, _ = compactUnit.store.Close() })
	if got := compactUnit.currentRef; got.Provider != "test" || got.Model != "alt-model" {
		t.Fatalf("summarizer model = %#v, want test/alt-model", got)
	}
	if window != 4096 {
		t.Fatalf("summarizer window = %d, want alt-model window 4096", window)
	}
}

func TestAgentSummarizerFallsBackToActiveModelWhenCompactModelUnavailable(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	writeAgentsTestConfig(t, a.configPath, `{
  "primary": { "model": "test/test-model" },
  "compact": { "model": "test/missing-model" }
}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload returned error: %v", err)
	}
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	compactUnit, window, err := a.compactRunningUnitForSession(a.session)
	if err != nil {
		t.Fatalf("compactRunningUnitForSession: %v", err)
	}
	t.Cleanup(func() { _, _ = compactUnit.store.Close() })
	if got := compactUnit.currentRef; got.Provider != "test" || got.Model != "test-model" {
		t.Fatalf("summarizer fallback model = %#v, want active test/test-model", got)
	}
	if window != 8192 {
		t.Fatalf("summarizer fallback window = %d, want primary window 8192", window)
	}
}

func TestAgentSetRuntimeConfigRefusesWhileBusy(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	a.ensureRuntime().mu.Lock()
	a.ensureRuntime().sessionLocked().busy = true
	a.ensureRuntime().mu.Unlock()
	if err := a.SetRuntimeConfig(a.GetRuntimeConfig()); err == nil {
		t.Fatal("SetRuntimeConfig returned nil while busy")
	}
}

func TestAgentSetDefaultModelRejectsInvalidRef(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.SetDefaultModel("not-a-ref"); err == nil {
		t.Fatal("SetDefaultModel returned nil for invalid ref")
	}
}

func TestAgentUsesConfiguredConfigPathForWrites(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "custom-config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": { "incomplete-model": { "name": "Incomplete Model" } }
    }
  },
  "default_model": ""
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if err := a.CompleteModelEntry("test/incomplete-model", ModelCompletion{ContextWindow: 32768}); err != nil {
		t.Fatalf("CompleteModelEntry returned error: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"context_window": 32768`) {
		t.Fatalf("custom config was not updated: %s", data)
	}
	defaultPath := filepath.Join(home, ".lightcode", "config.json")
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("default config path exists/err=%v; writer should use override path", err)
	}
}

func hasWarningKind(warnings []PromptWarning, kind string) bool {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return true
		}
	}
	return false
}

func TestAgentInitDoesNotWarnForConnectedPrimaryModel(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	warnings := a.CurrentWarnings()
	if hasWarningKind(warnings, "setup_model_unavailable") {
		t.Fatalf("warnings = %#v, did not expect model unavailable for connected primary model", warnings)
	}
	if hasWarningKind(warnings, "setup_no_provider") || hasWarningKind(warnings, "setup_no_model") {
		t.Fatalf("warnings = %#v, did not expect setup missing warnings for connected primary model", warnings)
	}
}

func TestAgentInitDoesNotActivatePrimaryModel(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Init(ctx)
	a.ensureRuntime().mu.Lock()
	current := a.currentRef
	a.ensureRuntime().mu.Unlock()
	if current.Provider != "" || current.Model != "" {
		t.Fatalf("Init activated model %#v; want lazy activation", current)
	}
}

func TestAgentCurrentModelUsesPrimaryModelRef(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	cur := a.CurrentModel()

	if cur.Ref != "test/test-model" || cur.Provider != "test" || cur.Model != "test-model" {
		t.Fatalf("CurrentModel identity = %#v, want test/test-model", cur)
	}
	if cur.DisplayName != "Test Model" || cur.ContextWindow != 8192 {
		t.Fatalf("CurrentModel metadata = %#v, want display/context from catalog", cur)
	}
}

func TestAgentSwitchModelTakesPrefixRef(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	_ = a.CurrentModel()
	appendUserTurn(t, a, "hello")

	if err := a.SwitchModel("test/alt-model"); err != nil {
		t.Fatalf("SwitchModel returned error: %v", err)
	}
	cur := a.CurrentModel()
	if cur.Ref != "test/alt-model" || cur.Model != "alt-model" || cur.DisplayName != "Alt Model" || cur.ContextWindow != 4096 {
		t.Fatalf("CurrentModel after switch = %#v, want alt-model metadata", cur)
	}
	agentsData, err := os.ReadFile(agentcfg.PathForConfig(a.configPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agentsData), `"model": "test/alt-model"`) {
		t.Fatalf("agents config after SwitchModel = %s, want primary.model alt-model", agentsData)
	}
	meta, err := a.store.Meta()
	if err != nil {
		t.Fatalf("store meta: %v", err)
	}
	if meta.Provider != "test" || meta.Model != "alt-model" {
		t.Fatalf("session model after SwitchModel = %s/%s, want test/alt-model", meta.Provider, meta.Model)
	}
}

func TestAgentModelListReturnsVisibleFlatCatalogEntries(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	entries := a.ModelList()

	byRef := map[string]ModelListEntry{}
	for _, entry := range entries {
		byRef[entry.Ref] = entry
	}
	if _, ok := byRef["test/hidden-model"]; ok {
		t.Fatalf("hidden model appeared in ModelList: %#v", byRef["test/hidden-model"])
	}
	entry, ok := byRef["test/test-model"]
	if !ok {
		t.Fatalf("ModelList missing test/test-model; entries=%#v", entries)
	}
	if entry.Provider != "test" || entry.ProviderName != "Test Provider" || entry.Model != "test-model" || entry.DisplayName != "Test Model" {
		t.Fatalf("ModelList entry identity/display = %#v", entry)
	}
	if entry.ContextWindow != 8192 || entry.Incomplete {
		t.Fatalf("ModelList entry completeness = %#v, want complete 8192 context", entry)
	}
}

func TestAgentSwitchModelUnknownModelDoesNotRunDiscovery(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected discovery path %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"remote-model","name":"Remote Model","context_window":12288,"max_output_tokens":3072}]}`)
	}))
	t.Cleanup(server.Close)

	a := newCatalogBackedTestAgent(t)
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.catalog.Providers["test"].Discovery = true
	delete(a.catalog.Providers["test"].Models, "remote-model")

	if err := a.SwitchModel("test/remote-model"); err == nil {
		t.Fatal("SwitchModel returned nil for unknown model")
	}
	if calls != 0 {
		t.Fatalf("discovery calls = %d, want 0", calls)
	}
}

func TestAgentRefreshDiscoveryUpdatesCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"id":"fresh-model","name":"Fresh Model","context_window":24576,"max_output_tokens":4096}]}`)
	}))
	t.Cleanup(server.Close)

	a := newCatalogBackedTestAgent(t)
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.catalog.Providers["test"].Discovery = true
	a.catalog.Providers["test"].Builtin = true

	if err := a.RefreshDiscovery("test"); err != nil {
		t.Fatalf("RefreshDiscovery returned error: %v", err)
	}
	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref == "test/fresh-model" {
			if entry.DisplayName != "Fresh Model" || entry.ContextWindow != 24576 || entry.Incomplete {
				t.Fatalf("fresh model entry = %#v", entry)
			}
			return
		}
	}
	t.Fatalf("ModelList missing test/fresh-model after RefreshDiscovery; entries=%#v", entries)
}

func TestAgentRefreshDiscoveryPreservesUserCostFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected discovery path %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"test-model","name":"Discovered Test Model","context_window":24576,"max_output_tokens":4096,"cost":{"input":1,"output":2}}]}`)
	}))
	t.Cleanup(server.Close)

	a := newCatalogBackedTestAgent(t)
	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": {
          "name": "Test Model",
          "context_window": 8192,
          "max_output_tokens": 1024,
          "cost": {"input": 99}
        }
      }
    }
  },
  "default_model": "test/test-model"
}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload returned error: %v", err)
	}
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.catalog.Providers["test"].Discovery = true
	a.catalog.Providers["test"].Builtin = true

	if err := a.RefreshDiscovery("test"); err != nil {
		t.Fatalf("RefreshDiscovery returned error: %v", err)
	}
	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref != "test/test-model" {
			continue
		}
		if entry.Cost == nil {
			t.Fatalf("test-model cost = nil")
		}
		if entry.Cost.Output == nil || *entry.Cost.Output != 2 {
			t.Fatalf("test-model output cost = %v, want discovered value 2", entry.Cost.Output)
		}
		if entry.Cost.Input == nil || *entry.Cost.Input != 99 {
			t.Fatalf("test-model input cost = %v, want user value 99", entry.Cost.Input)
		}
		return
	}
	t.Fatalf("ModelList missing test/test-model after RefreshDiscovery; entries=%#v", entries)
}

func TestAgentRefreshDiscoveryFailureEmitsWarningEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	a := newCatalogBackedTestAgent(t)
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.catalog.Providers["test"].Discovery = true
	var warningEvent *Event
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventWarning {
			copy := ev
			warningEvent = &copy
		}
	})

	if err := a.RefreshDiscovery("test"); err == nil {
		t.Fatal("RefreshDiscovery returned nil error for HTTP 500")
	}
	if warningEvent == nil || len(warningEvent.Warnings) == 0 {
		t.Fatalf("RefreshDiscovery did not emit EventWarning; event=%#v", warningEvent)
	}
	if warningEvent.Warnings[0].Kind != "catalog_discovery_failure" {
		t.Fatalf("warning = %#v, want catalog_discovery_failure", warningEvent.Warnings[0])
	}
}

func TestAgentModelListIsSafeDuringRefreshDiscovery(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	configureChangingDiscoveryServer(t, a)

	runConcurrentDiscoveryReaders(t, a, func() {
		_ = a.ModelList()
	})
}

func TestAgentCurrentModelIsSafeDuringRefreshDiscovery(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	configureChangingDiscoveryServer(t, a)

	runConcurrentDiscoveryReaders(t, a, func() {
		_ = a.CurrentModel()
	})
}

func TestAgentReloadRebuildsCatalogAndFallsBackToDefault(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.SwitchModel("test/alt-model"); err != nil {
		t.Fatalf("SwitchModel returned error: %v", err)
	}
	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider Reloaded",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model Reloaded", "context_window": 16384, "max_output_tokens": 2048 },
        "new-model": { "name": "New Model", "context_window": 32768, "max_output_tokens": 4096 }
      }
    }
  },
  "default_model": "test/test-model"
}`)
	writeAgentsTestConfig(t, a.configPath, `{"primary": {"model": "test/test-model"}}`)

	if err := a.Reload(); err != nil {
		t.Fatalf("Reload returned error: %v", err)
	}

	cur := a.CurrentModel()
	if cur.Ref != "test/test-model" || cur.DisplayName != "Test Model Reloaded" || cur.ContextWindow != 16384 {
		t.Fatalf("CurrentModel after Reload = %#v, want fallback to reloaded default", cur)
	}
	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref == "test/new-model" && entry.DisplayName == "New Model" && entry.ContextWindow == 32768 {
			return
		}
	}
	t.Fatalf("ModelList missing test/new-model after Reload; entries=%#v", entries)
}

func TestAgentReloadRefusesWhileBusy(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	a.ensureRuntime().mu.Lock()
	a.ensureRuntime().sessionLocked().busy = true
	a.ensureRuntime().mu.Unlock()

	if err := a.Reload(); err == nil {
		t.Fatal("Reload returned nil while busy")
	}
}

func TestAgentCompleteModelEntryWritesConfigAndReloadsCatalog(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 },
        "incomplete-model": { "name": "Incomplete Model" }
      }
    }
  },
  "default_model": "test/test-model"
}`)
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload before completion returned error: %v", err)
	}

	if err := a.CompleteModelEntry("test/incomplete-model", ModelCompletion{ContextWindow: 65536, MaxOutputTokens: 8192}); err != nil {
		t.Fatalf("CompleteModelEntry returned error: %v", err)
	}

	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref == "test/incomplete-model" {
			if entry.Incomplete || entry.ContextWindow != 65536 {
				t.Fatalf("completed model entry = %#v, want complete 65536 context", entry)
			}
			return
		}
	}
	t.Fatalf("ModelList missing test/incomplete-model after CompleteModelEntry; entries=%#v", entries)
}

func TestAgentEnsureSessionReloadsExternalConfigEdit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Edited Model", "context_window": 24576, "max_output_tokens": 2048 },
        "new-session-model": { "name": "New Session Model", "context_window": 49152, "max_output_tokens": 4096 }
      }
    }
  },
  "default_model": "test/test-model"
}`)

	appendUserTurn(t, a, "hello")

	cur := a.CurrentModel()
	if cur.DisplayName != "Edited Model" || cur.ContextWindow != 24576 {
		t.Fatalf("CurrentModel after session-start reload = %#v, want edited model", cur)
	}
	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref == "test/new-session-model" {
			return
		}
	}
	t.Fatalf("ModelList missing test/new-session-model after session-start reload; entries=%#v", entries)
}

func TestAgentSessionSwitchReloadsExternalConfigEdit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	appendUserTurn(t, a, "first")
	firstID := a.SessionCurrent().ID
	if firstID == "" {
		t.Fatal("first session id is empty")
	}
	if err := a.SessionNew(); err != nil {
		t.Fatalf("SessionNew returned error: %v", err)
	}
	appendUserTurn(t, a, "second")
	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Switched Model", "context_window": 28672, "max_output_tokens": 2048 },
        "switch-model": { "name": "Switch Model", "context_window": 57344, "max_output_tokens": 4096 }
      }
    }
  },
  "default_model": "test/test-model"
}`)

	if err := a.SessionSwitch(firstID); err != nil {
		t.Fatalf("SessionSwitch returned error: %v", err)
	}

	cur := a.CurrentModel()
	if cur.DisplayName != "Switched Model" || cur.ContextWindow != 28672 {
		t.Fatalf("CurrentModel after SessionSwitch reload = %#v, want switched model", cur)
	}
	entries := a.ModelList()
	for _, entry := range entries {
		if entry.Ref == "test/switch-model" {
			return
		}
	}
	t.Fatalf("ModelList missing test/switch-model after SessionSwitch reload; entries=%#v", entries)
}

func TestAgentAllModelListIncludesHiddenModels(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	entries := a.AllModelList()

	byRef := map[string]ModelListEntry{}
	for _, entry := range entries {
		byRef[entry.Ref] = entry
	}
	hiddenEntry, ok := byRef["test/hidden-model"]
	if !ok {
		t.Fatalf("AllModelList missing test/hidden-model; entries=%#v", entries)
	}
	if !hiddenEntry.Hidden {
		t.Fatalf("hidden-model entry Hidden = %v, want true", hiddenEntry.Hidden)
	}
	testEntry, ok := byRef["test/test-model"]
	if !ok {
		t.Fatalf("AllModelList missing test/test-model; entries=%#v", entries)
	}
	if testEntry.Hidden {
		t.Fatalf("test-model entry Hidden = %v, want false", testEntry.Hidden)
	}
	if testEntry.Provider != "test" || testEntry.ProviderName != "Test Provider" || testEntry.Model != "test-model" || testEntry.DisplayName != "Test Model" {
		t.Fatalf("test-model entry identity/display = %#v", testEntry)
	}
	if testEntry.ContextWindow != 8192 {
		t.Fatalf("test-model entry ContextWindow = %d, want 8192", testEntry.ContextWindow)
	}
}

func TestAgentSetModelHiddenWritesConfigAndUpdatesCatalog(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	if err := a.SetModelHidden("test/test-model", true); err != nil {
		t.Fatalf("SetModelHidden returned error: %v", err)
	}
	if !a.catalog.Providers["test"].Models["test-model"].Hidden {
		t.Fatalf("in-memory catalog not updated: test-model.Hidden = %v, want true", a.catalog.Providers["test"].Models["test-model"].Hidden)
	}

	data, err := os.ReadFile(agentConfigPath(a.home))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	providers := root["providers"].(map[string]any)
	testProv := providers["test"].(map[string]any)
	models := testProv["models"].(map[string]any)
	testModel := models["test-model"].(map[string]any)
	if hidden, ok := testModel["hidden"].(bool); !ok || !hidden {
		t.Fatalf("config test-model.hidden = %v, want true", testModel["hidden"])
	}

	visibleEntries := a.ModelList()
	for _, entry := range visibleEntries {
		if entry.Ref == "test/test-model" {
			t.Fatalf("test/test-model appeared in ModelList after SetModelHidden(true)")
		}
	}

	allEntries := a.AllModelList()
	found := false
	for _, entry := range allEntries {
		if entry.Ref == "test/test-model" {
			found = true
			if !entry.Hidden {
				t.Fatalf("AllModelList test/test-model Hidden = %v, want true", entry.Hidden)
			}
			break
		}
	}
	if !found {
		t.Fatalf("AllModelList missing test/test-model after SetModelHidden(true)")
	}

	if err := a.SetModelHidden("test/test-model", false); err != nil {
		t.Fatalf("SetModelHidden(false) returned error: %v", err)
	}
	visibleEntries = a.ModelList()
	found = false
	for _, entry := range visibleEntries {
		if entry.Ref == "test/test-model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("test/test-model did not reappear in ModelList after SetModelHidden(false)")
	}
}

func TestAgentSetProviderHiddenWritesConfigAndUpdatesCatalog(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	if err := a.SetProviderHidden("test", true); err != nil {
		t.Fatalf("SetProviderHidden returned error: %v", err)
	}
	if !a.catalog.Providers["test"].Hidden {
		t.Fatalf("in-memory catalog not updated: test.Hidden = %v, want true", a.catalog.Providers["test"].Hidden)
	}

	data, err := os.ReadFile(agentConfigPath(a.home))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	providers := root["providers"].(map[string]any)
	testProv := providers["test"].(map[string]any)
	if hidden, ok := testProv["hidden"].(bool); !ok || !hidden {
		t.Fatalf("config test.hidden = %v, want true", testProv["hidden"])
	}

	visibleEntries := a.ModelList()
	for _, entry := range visibleEntries {
		if entry.Provider == "test" {
			t.Fatalf("provider test models appeared in ModelList after SetProviderHidden(true)")
		}
	}

	allEntries := a.AllModelList()
	for _, entry := range allEntries {
		if entry.Provider == "test" {
			if !entry.ProviderHidden {
				t.Fatalf("AllModelList test entry ProviderHidden = %v, want true", entry.ProviderHidden)
			}
			if !entry.Hidden {
				t.Fatalf("AllModelList test entry Hidden = %v, want true", entry.Hidden)
			}
		}
	}

	if err := a.SetProviderHidden("test", false); err != nil {
		t.Fatalf("SetProviderHidden(false) returned error: %v", err)
	}
	visibleEntries = a.ModelList()
	found := false
	for _, entry := range visibleEntries {
		if entry.Ref == "test/test-model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("test/test-model did not reappear in ModelList after SetProviderHidden(false)")
	}
	for _, entry := range visibleEntries {
		if entry.Ref == "test/hidden-model" {
			t.Fatalf("test/hidden-model appeared in ModelList after SetProviderHidden(false) (model-level hidden still true)")
		}
	}
}

func TestAgentMutateConfigRejectsMalformedShapes(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": "not-an-object"
  },
  "default_model": "test/test-model"
}`)
	if err := a.SetProviderHidden("test", true); err == nil {
		t.Fatal("SetProviderHidden returned nil for malformed provider")
	} else if !strings.Contains(err.Error(), "providers.test must be an object") {
		t.Fatalf("SetProviderHidden error = %v, want providers.test must be an object", err)
	}

	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": "not-an-object"
    }
  },
  "default_model": "test/test-model"
}`)
	if err := a.SetModelHidden("test/test-model", true); err == nil {
		t.Fatal("SetModelHidden returned nil for malformed models")
	} else if !strings.Contains(err.Error(), "providers.test.models must be an object") {
		t.Fatalf("SetModelHidden error = %v, want providers.test.models must be an object", err)
	}

	writeCatalogTestConfig(t, a.home, `{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": "not-an-object"
      }
    }
  },
  "default_model": "test/test-model"
}`)
	if err := a.SetModelHidden("test/test-model", true); err == nil {
		t.Fatal("SetModelHidden returned nil for malformed model")
	} else if !strings.Contains(err.Error(), "providers.test.models.test-model must be an object") {
		t.Fatalf("SetModelHidden error = %v, want providers.test.models.test-model must be an object", err)
	}
}

func TestAgentModelListOmitsUnconnectedProviders(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_CONNECTED_KEY", "test-key")
	// "connected" has the key set; "disconnected" references a missing env var.
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "connected": {
      "name": "Connected Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_CONNECTED_KEY" },
      "discovery": false,
      "models": {
        "vis-model": { "name": "Visible Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    },
    "disconnected": {
      "name": "Disconnected Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_NEVER_SET" },
      "discovery": false,
      "models": {
        "ghost-model": { "name": "Ghost Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "connected/vis-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	for _, entry := range a.ModelList() {
		if entry.Provider == "disconnected" {
			t.Fatalf("ModelList includes model from unconnected provider: %#v", entry)
		}
	}
	for _, entry := range a.AllModelList() {
		if entry.Provider == "disconnected" {
			t.Fatalf("AllModelList includes model from unconnected provider: %#v", entry)
		}
	}

	found := false
	for _, entry := range a.ModelList() {
		if entry.Ref == "connected/vis-model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ModelList missing connected/vis-model; entries=%#v", a.ModelList())
	}
}

func TestAgentModelListFreshInstallListsNothing(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Clear any env vars that bundled providers might pick up.
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers": {}, "default_model": ""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	if entries := a.ModelList(); len(entries) != 0 {
		t.Fatalf("fresh install ModelList = %#v, want empty", entries)
	}
	if entries := a.AllModelList(); len(entries) != 0 {
		t.Fatalf("fresh install AllModelList = %#v, want empty", entries)
	}
}

func TestHTTPAndACPSeeSameConnectedOnlyList(t *testing.T) {
	// HTTP server and ACP both call agent.ModelList(); verify that the
	// connected-only filter is applied uniformly through that single path.
	a := newCatalogBackedTestAgent(t)
	// The test agent's provider is connected (LIGHTCODE_TEST_KEY is set).
	direct := a.ModelList()
	if len(direct) == 0 {
		t.Fatal("direct ModelList empty; expected connected test provider")
	}
	// Simulate disconnecting the provider by clearing the env var.
	t.Setenv("LIGHTCODE_TEST_KEY", "")
	afterDisconnect := a.ModelList()
	if len(afterDisconnect) != 0 {
		t.Fatalf("ModelList after disconnect = %#v, want empty", afterDisconnect)
	}
}

func writeCatalogTestConfig(t *testing.T, home, content string) {
	t.Helper()
	path := filepath.Join(home, ".lightcode", "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func writeAgentsTestConfig(t *testing.T, configPath, content string) {
	t.Helper()
	path := agentcfg.PathForConfig(configPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create agents dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write agents config: %v", err)
	}
}

func configureChangingDiscoveryServer(t *testing.T, a *Agent) {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected discovery path %s", r.URL.Path)
		}
		call := calls.Add(1)
		_, _ = fmt.Fprintf(w, `{"data":[
			{"id":"test-model","name":"Test Model %d","context_window":%d,"max_output_tokens":1024},
			{"id":"fresh-model-%d","name":"Fresh Model %d","context_window":12288,"max_output_tokens":2048}
		]}`, call, 8192+call, call, call)
	}))
	t.Cleanup(server.Close)
	a.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	a.catalog.Providers["test"].Discovery = true
}

func runConcurrentDiscoveryReaders(t *testing.T, a *Agent, read func()) {
	t.Helper()
	var wg sync.WaitGroup
	errCh := make(chan error, 200)

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := a.RefreshDiscovery("test"); err != nil {
					errCh <- err
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				read()
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("RefreshDiscovery returned error: %v", err)
		}
	}
}
