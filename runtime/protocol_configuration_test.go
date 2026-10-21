package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// The configuration-read suite: every read projects from one captured
// published configuration — source labels, key-source labels, the
// no-authorization header contract, the effective settings view, and the
// connected-only picker rules — with no secret bytes and full caller
// ownership of every returned map, slice, and pointer.

const configurationProvidersDocument = `{
  "providers": {
    "prov": {
      "transport": {"base_url": "https://prov.test/v1", "api_key_env": "SHELL_TEST_KEY",
                    "headers": {"X-Trace": "t1", "Authorization": "Bearer forged", "Proxy-Authorization": "forged2", "  authorization  ": "forged3"}},
      "discovery": false,
      "extra_body": {"side": "value", "big": 9007199254740993},
      "protocol_metadata": {"family": "testfam", "must_preserve": ["x_keep"], "drop": ["x_drop"]},
      "models": {
        "m": {"name": "M", "context_window": 4096, "cost": {"input": 1.5}, "extra_body": {"mside": 1}},
        "hidden_one": {"context_window": 8192, "hidden": true},
        "incomplete_one": {"name": "I"}
      }
    },
    "managedp": {"transport": {"base_url": "https://managed.test/v1", "api_key_env": "MANAGED_TEST_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}},
    "unsetp": {"transport": {"base_url": "https://unset.test/v1", "api_key_env": "UNSET_TEST_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}},
    "keyless": {"transport": {"base_url": "https://keyless.test/v1", "api_key_env": ""}, "discovery": false, "models": {"m": {"context_window": 4096}}},
    "occupied": {"transport": {"base_url": "https://occupied.test/v1", "api_key_env": "LIGHTCODE_OCCUPIED_API_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}},
    "openrouter": {"transport": {"headers": {"x-title": "user-title", "X-Custom": "keep"}}, "models": {"m": {"context_window": 4096}}}
  },
  "sessions": {"auto_archive": false, "archive_after_days": 3},
  "plugins": {"tools": {"max_output_bytes": 42}, "jobs": {"max_background_processes": 20}, "tasks": {}}
}`

// configurationAgentsDocument configures the primary model and one
// subagent with a whitespace-padded write dir.
const configurationAgentsDocument = `{
  "primary": {"model": "prov/m"},
  "worker": {"model": "prov/m", "system_prompt": "simple", "subagent": true, "description": "w", "readonly": true, "write_dir": "  /ws  ", "tools": ["prod_read"]}
}`

// settingsPlugins composes placeholder plugins with the shipped IDs the
// settings projection reads; their validators accept any object because the
// projection, not their validation, is the axis under test.
func settingsPlugins() []Plugin {
	var opens atomic.Int64 // the fixture counters are unread here
	return []Plugin{
		servicePlugin("tools", &opens, func(json.RawMessage) error { return nil }),
		servicePlugin("jobs", &opens, func(json.RawMessage) error { return nil }),
		servicePlugin("tasks", &opens, func(json.RawMessage) error { return nil }),
		rosterToolPlugin(),
	}
}

// rosterToolPlugin provides one real tool declaration so the composed tool
// universe admits the fixture's agent tool lists.
func rosterToolPlugin() Plugin {
	return Plugin{
		ID:       "prodtools",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("prod_read", fileReadTool{}.describe)},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"prod_read": fileReadTool{}}}, nil
		},
	}
}

// openConfigurationRuntime opens one composed Runtime over the given store
// with the given documents and extra plugins, using the controlled
// preparation (these tests admit nothing).
func openConfigurationRuntime(t *testing.T, store harness.Storage, configDoc, agentsDoc string, extra ...Plugin) (*Runtime, *ownerEnv) {
	t.Helper()
	e := newOwnerEnv(t)
	return openConfigurationRuntimeWithEnv(t, store, e, configDoc, agentsDoc, extra...)
}

// writeDotEnv seeds the isolated HOME's .env with one managed key so startup
// loads it into the managed set and the process environment.
func writeDotEnv(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".lightcode")
	testassert.NoError(t, os.MkdirAll(dir, 0o700))
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600))
}

// unsetenv cleans one process-environment key before and after the test:
// LoadDotEnv's real os.Setenv leaks past t.Setenv's restore, so a subtest
// must start from a clean key rather than inheriting a prior subtest's
// managed key as a pre-exported one.
func unsetenv(t *testing.T, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, key := range keys {
			_ = os.Unsetenv(key)
		}
	})
	for _, key := range keys {
		_ = os.Unsetenv(key)
	}
}

func findProvider(views []protocol.Provider, id string) protocol.Provider {
	for _, view := range views {
		if view.Id == id {
			return view
		}
	}
	return protocol.Provider{}
}

// TestConfigurationViewProjectsProvidersPinsSourcesKeySourcesAndHeaders pins
// the complete provider projection: source labels (custom user, bundled
// bundled), the key-source classification (managed vs pre-exported shell vs
// unset vs keyless), the connection and operation flags, the generated env
// name over captured occupancy, and the no-authorization header contract on
// both header maps.
func TestConfigurationViewProjectsProvidersPinsSourcesKeySourcesAndHeaders(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		unsetenv(t, "SHELL_TEST_KEY", "MANAGED_TEST_KEY", "UNSET_TEST_KEY")
		t.Setenv("SHELL_TEST_KEY", "shell-secret-value") // pre-exported shell key, never managed
		e := newOwnerEnv(t)
		// A valid injected key followed by an oversized line: the scanner
		// fails after the injection. The partial load keeps the already
		// injected key in the managed set and never reclassifies the
		// pre-exported shell sibling: the loaded/external distinction
		// follows who actually set each value.
		writeDotEnv(t, e.home, "MANAGED_TEST_KEY=managed-secret-value\nHUGE="+strings.Repeat("A", bufio.MaxScanTokenSize+1)+"\n")
		r, _ := openConfigurationRuntimeWithEnv(t, store, e, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)

		view, err := r.getConfiguration(context.Background())
		testassert.NoError(t, err)
		testassert.Require(t, view.ConfigurationRevision.Generation == "1" && view.ConfigurationRevision.InstanceId == "", "configuration revision = %+v, want generation 1 with the empty pre-server instance", view.ConfigurationRevision)

		// Source labels through the projection: the custom provider's models
		// are user, and every bundled provider's shipped models are bundled.
		prov := findProvider(view.Providers, "prov")
		testassert.Require(t, prov.Id == "prov", "prov view missing: %+v", prov)
		openrouter := findProvider(view.Providers, "openrouter")
		testassert.Require(t, openrouter.Builtin, "openrouter builtin = false, want the bundled identity")
		testassert.Require(t, len(openrouter.Models) != 0 && openrouter.Models[0].Source == protocol.ModelSource("bundled"), "openrouter model sources = %+v, want bundled labels", openrouter.Models)

		// Key-source labels over the capture's own credential sample; the
		// wire view carries no mutability flags or suggested names — source
		// labels describe provenance.
		for _, row := range []struct {
			id        string
			keySource string
			connected bool
		}{
			{"prov", "external", true},
			{"managedp", "managed", true},
			{"unsetp", "none", false},
			{"keyless", "keyless", true},
		} {
			view := findProvider(view.Providers, row.id)
			testassert.Require(t, view.KeySource == protocol.ProviderKeySource(row.keySource) && view.Connected == row.connected, "%s labels = key_source %q connected %v, want %q/%v", row.id, view.KeySource, view.Connected, row.keySource, row.connected)
		}

		// The no-authorization response contract: no casing or padding of the
		// credential header names survives into either map.
		{
			_, present := prov.Headers["Authorization"]
			testassert.Require(t, !present, "effective headers returned Authorization")
		}
		for name := range prov.Headers {
			trimmed := strings.TrimSpace(name)
			testassert.Require(t, !strings.EqualFold(trimmed, "Authorization") && !strings.EqualFold(trimmed, "Proxy-Authorization"), "effective headers returned credential header %q", name)
		}
		testassert.Require(t, prov.Headers["X-Trace"] == "t1", "effective headers = %v, want the retained transport header", prov.Headers)

		// The user headers projection carries the user layer's own headers
		// exactly — no source-based stripping — so a user-layer collision
		// with a bundled header name is visible and editable; the user file
		// is never edited.
		{
			got := openrouter.UserHeaders
			testassert.Require(t, len(got) == 2 && got["x-title"] == "user-title" && got["X-Custom"] == "keep", "openrouter user_headers = %v, want the user layer's own headers", got)
		}
		testassert.Require(t, len(openrouter.Headers["X-Title"]) != 0 && openrouter.Headers["HTTP-Referer"] != "", "openrouter effective headers = %v, want the bundled headers present", openrouter.Headers)

		// No secret value leaves through any response: labels only.
		data, err := json.Marshal(view)
		testassert.NoError(t, err)
		for _, secret := range []string{"managed-secret-value", "shell-secret-value", "Bearer forged", "forged2", "forged3"} {
			testassert.Require(t, !strings.Contains(string(data), secret), "configuration view carries secret bytes %q: %s", secret, data)
		}
	})
}

// TestRuntimeSharedCredentialReference pins the shared credential
// reference: three providers naming the same managed variable resolve one
// credential — a sibling's update (including its connected rebinding away
// from the shared name) and even a referencing provider's whole deletion
// leave the variable's exact value and managed ownership and every
// surviving sibling's connection untouched. One variable, no per-provider
// leases.
func TestRuntimeSharedCredentialReference(t *testing.T) {
	unsetenv(t, "SHARED_CRED_TEST_KEY", "RESHARED_TEST_KEY")
	store := storage.NewMemory()
	e := newOwnerEnv(t)
	writeDotEnv(t, e.home, "SHARED_CRED_TEST_KEY=shared-secret-value\n")
	doc := `{"providers":{
			"shalpha": {"transport": {"base_url": "https://a.test/v1", "api_key_env": "SHARED_CRED_TEST_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}},
			"shbeta": {"transport": {"base_url": "https://b.test/v1", "api_key_env": "SHARED_CRED_TEST_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}},
			"shgamma": {"transport": {"base_url": "https://c.test/v1", "api_key_env": "SHARED_CRED_TEST_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}}}}`
	r, _ := openConfigurationRuntimeWithEnv(t, store, e, doc, `{"solo": {"model": "shalpha/m", "system_prompt": "simple"}}`)
	defer closeProjectionRuntime(r)
	ctx := context.Background()
	sub, err := r.Subscribe(8)
	testassert.NoError(t, err)
	defer sub.Close()

	// Every sibling resolves the one shared credential.
	for _, id := range []string{"shalpha", "shbeta", "shgamma"} {
		detail, err := r.getProvider(ctx, id)
		testassert.Require(t, err == nil && detail.Provider.Connected && detail.Provider.KeySource == protocol.ProviderKeySource(config.KeySourceManaged), "%s = (%v, %+v), want the shared managed connection", id, err, detail.Provider)
	}

	// The sibling's update keeps the shared reference and the variable.
	name := "Renamed Alpha"
	{
		_, err := r.updateProvider(ctx, "shalpha", protocol.ProviderEdit{Name: &name})
		testassert.NoError(t, err)
	}
	drainConnectionEvent(t, r.warnings, sub, "2")
	{
		detail, err := r.getProvider(ctx, "shbeta")
		testassert.Require(t, err == nil && detail.Provider.Connected && detail.Provider.ApiKeyEnv == "SHARED_CRED_TEST_KEY", "shbeta after the sibling update = (%v, %+v), want the shared reference untouched", err, detail.Provider)
	}
	if os.Getenv("SHARED_CRED_TEST_KEY") != "shared-secret-value" || !r.managedEnv.IsManaged("SHARED_CRED_TEST_KEY") {
		t.Fatalf("shared variable after the sibling update = (%q, %v), want the exact value and ownership", os.Getenv("SHARED_CRED_TEST_KEY"), r.managedEnv.IsManaged("SHARED_CRED_TEST_KEY"))
	}

	// The connected rebinding away from the shared name does not unset
	// the credential: the variable keeps its value and the sibling keeps
	// resolving it.
	fresh := "RESHARED_TEST_KEY"
	{
		_, err := r.updateProvider(ctx, "shalpha", protocol.ProviderEdit{ApiKeyEnv: &fresh})
		testassert.NoError(t, err)
	}
	drainConnectionEvent(t, r.warnings, sub, "3")
	if os.Getenv("SHARED_CRED_TEST_KEY") != "shared-secret-value" {
		t.Fatalf("the rebinding unset the shared credential: %q", os.Getenv("SHARED_CRED_TEST_KEY"))
	}
	{
		detail, err := r.getProvider(ctx, "shbeta")
		testassert.Require(t, err == nil && detail.Provider.Connected, "shbeta after the rebinding = (%v, %+v), want the connection through the shared variable", err, detail.Provider)
	}

	// The deletion is the strongest form: removing a provider that
	// still references the shared variable leaves the variable and every
	// surviving sibling's connection intact — the variable is not a
	// per-provider lease.
	mutation, err := r.deleteProvider(ctx, "shbeta")
	testassert.Require(t, err == nil && mutation.Result == nil, "shared-reference delete = (%v, %+v), want the null post-state", err, mutation)
	drainConnectionEvent(t, r.warnings, sub, "4")
	if os.Getenv("SHARED_CRED_TEST_KEY") != "shared-secret-value" || !r.managedEnv.IsManaged("SHARED_CRED_TEST_KEY") {
		t.Fatalf("the deletion removed the shared variable = (%q, %v)", os.Getenv("SHARED_CRED_TEST_KEY"), r.managedEnv.IsManaged("SHARED_CRED_TEST_KEY"))
	}
	{
		detail, err := r.getProvider(ctx, "shgamma")
		testassert.Require(t, err == nil && detail.Provider.Connected && detail.Provider.KeySource == protocol.ProviderKeySource(config.KeySourceManaged), "shgamma after the sibling deletion = (%v, %+v), want the surviving shared connection", err, detail.Provider)
	}
}

// openConfigurationRuntimeWithEnv is openConfigurationRuntime with a
// pre-configured env (the .env must exist before Open loads it).
func openConfigurationRuntimeWithEnv(t *testing.T, store harness.Storage, e *ownerEnv, configDoc, agentsDoc string, extra ...Plugin) (*Runtime, *ownerEnv) {
	t.Helper()
	writeServiceFile(t, e.configPath, configDoc)
	writeServiceFile(t, agents.PathForConfig(e.configPath), agentsDoc)
	r, err := e.open(context.Background(), append([]Plugin{e.storagePlugin(store)}, extra...)...)
	testassert.NoError(t, err)
	return r, e
}

// TestConfigurationViewProjectsSettingsAndRoster pins the settings view: the
// parsed session policy, each owned plugin section returned as its exact
// opaque document string — no ID-chosen decoder, no injected defaults, an
// owned empty object as `{}` — absent sections omitted, and the Agent
// roster's public fields.
func TestConfigurationViewProjectsSettingsAndRoster(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)

		view, err := r.getConfiguration(context.Background())
		testassert.NoError(t, err)
		sessions := view.Settings.Sessions
		testassert.Require(t, !sessions.AutoArchive && sessions.ArchiveAfterDays == 3 && sessions.DeleteAfterArchiveDays == 7, "sessions settings = %+v, want the supplied values with the 7-day delete default", sessions)
		wantDocs := map[string]string{
			"tools": `{"max_output_bytes":42}`,
			"jobs":  `{"max_background_processes":20}`,
			"tasks": `{}`,
		}
		testassert.Require(t, len(view.Settings.Plugins) == len(wantDocs), "plugins = %v, want exactly the owned sections %v", view.Settings.Plugins, wantDocs)
		for id, wantDoc := range wantDocs {
			got, ok := view.Settings.Plugins[id]
			testassert.Require(t, ok, "plugins.%s missing, want the owned document %s", id, wantDoc)
			{
				got := compactJSON(t, []byte(got))
				testassert.Require(t, got == wantDoc, "plugins.%s = %s, want the exact owned document %s (no decoder defaults)", id, got, wantDoc)
			}
		}

		// Absent sections stay omitted: a configuration without the plugin
		// sections projects an empty complete map.
		absent, _ := openConfigurationRuntime(t, store, `{"providers":{}}`, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(absent)
		absentView, err := absent.getConfiguration(context.Background())
		testassert.NoError(t, err)
		testassert.Require(t, len(absentView.Settings.Plugins) == 0, "absent plugin sections projected = %+v, want none", absentView.Settings.Plugins)

		// The Agent roster: the primary model selection and the worker's
		// public fields with the trimmed write dir.
		var primary, worker *protocol.Agent
		for i := range view.Agents {
			switch view.Agents[i].Name {
			case "primary":
				primary = &view.Agents[i]
			case "worker":
				worker = &view.Agents[i]
			}
		}
		testassert.Require(t, primary != nil && primary.Model == "prov/m", "primary roster entry = %+v, want the configured model", primary)
		testassert.Require(t, worker != nil && worker.Subagent && worker.Readonly && worker.Description == "w" && worker.WriteDir == "/ws", "worker roster entry = %+v, want the public definition fields with the trimmed write dir", worker)
	})
}

// TestConfigurationReadsRemainAvailableDuringParkedBuild parks a reload build
// inside a compiled plugin's validator and reads the ready revision through
// the same owner: a slow candidate build must not hold the publication/capture
// path, so the ready snapshot, its credential sample, and its revision stay
// available until the new generation publishes.
func TestConfigurationReadsRemainAvailableDuringParkedBuild(t *testing.T) {
	store := storage.NewMemory()
	var armed atomic.Bool
	gated := make(chan struct{}, 1)
	ungate := make(chan struct{})
	releaseGate := sync.OnceFunc(func() { close(ungate) })
	var opens atomic.Int64
	gate := servicePlugin("gate", &opens, func(json.RawMessage) error {
		if !armed.Load() {
			return nil
		}
		select {
		case gated <- struct{}{}:
		default:
		}
		<-ungate
		return nil
	})
	r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, append(settingsPlugins(), gate)...)
	defer func() {
		releaseGate()
		closeProjectionRuntime(r)
	}()

	armed.Store(true)
	reloadDone := make(chan error, 1)
	go func() {
		_, err := r.Reload(context.Background())
		reloadDone <- err
	}()
	select {
	case <-gated:
	case <-time.After(10 * time.Second):
		t.Fatal("the reload never reached the parked validator")
	}

	view, err := r.getConfiguration(context.Background())
	testassert.NoError(t, err)
	testassert.Equal(t, view.ConfigurationRevision.Generation, "1")
	releaseGate()
	testassert.NoError(t, <-reloadDone)
	after, err := r.getConfiguration(context.Background())
	testassert.Require(t, err == nil && after.ConfigurationRevision.Generation == "2", "read after the reload = (%v, generation %s), want generation 2", err, after.ConfigurationRevision.Generation)
}

// definedEmptyProvidersDocument is the classifier fixture: a defined-but-empty
// external key, an absent key, a managed-empty key, and one connected keyless
// sibling that proves the picker still includes the connected set.
const definedEmptyProvidersDocument = `{
  "providers": {
    "emptyext": {"transport": {"base_url": "https://empty.test/v1", "api_key_env": "EMPTY_EXT_TEST_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}},
    "absent": {"transport": {"base_url": "https://absent.test/v1", "api_key_env": "ABSENT_TEST_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}},
    "managedempty": {"transport": {"base_url": "https://managedempty.test/v1", "api_key_env": "MANAGED_EMPTY_TEST_KEY"}, "discovery": false, "models": {"m": {"context_window": 4096}}},
    "live": {"transport": {"base_url": "https://live.test/v1", "api_key_env": ""}, "discovery": false, "models": {"m": {"context_window": 4096}}}
  }
}`

// TestConfigurationDefinedEmptyExternalKeySource pins the classifier rule: a
// defined-but-empty external variable is external even though it supplies no
// credential, so the provider stays disconnected and excluded from the
// picker; the nearest siblings stay absent=none and managed-empty=managed.
func TestConfigurationDefinedEmptyExternalKeySource(t *testing.T) {
	store := storage.NewMemory()
	unsetenv(t, "EMPTY_EXT_TEST_KEY", "ABSENT_TEST_KEY", "MANAGED_EMPTY_TEST_KEY")
	t.Setenv("EMPTY_EXT_TEST_KEY", "")
	e := newOwnerEnv(t)
	writeDotEnv(t, e.home, "MANAGED_EMPTY_TEST_KEY=\n")
	r, _ := openConfigurationRuntimeWithEnv(t, store, e, definedEmptyProvidersDocument, `{"solo":{"model":"live/m"}}`, settingsPlugins()...)
	defer closeProjectionRuntime(r)
	ctx := context.Background()

	view, err := r.getConfiguration(ctx)
	testassert.NoError(t, err)
	for _, row := range []struct {
		id        string
		source    string
		connected bool
	}{
		{"emptyext", "external", false},
		{"absent", "none", false},
		{"managedempty", "managed", false},
		{"live", "keyless", true},
	} {
		prov := findProvider(view.Providers, row.id)
		testassert.Require(t, prov.Id == row.id && string(prov.KeySource) == row.source && prov.Connected == row.connected, "%s view missing: %+v; %s = key_source %q connected %v, want %q/%v", row.id, prov, row.id, prov.KeySource, prov.Connected, row.source, row.connected)
	}

	models, err := r.listModels(ctx, protocol.ListModelsParams{All: true})
	testassert.NoError(t, err)
	included := map[string]bool{}
	for _, entry := range models.Models {
		included[entry.Provider] = true
	}
	testassert.Require(t, !included["emptyext"] && !included["absent"] && !included["managedempty"] && included["live"], "the picker includes disconnected providers: %v; the picker excludes the connected keyless provider: %v", included, included)
}

// TestConfigurationModelPickerConnectedRules pins the flat picker: both the
// visible and the full list are connected-only, the visible list excludes
// hidden models and hidden providers, the full list includes them with their
// flags, and disconnected providers appear in neither.
func TestConfigurationModelPickerConnectedRules(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		unsetenv(t, "SHELL_TEST_KEY", "MANAGED_TEST_KEY", "UNSET_TEST_KEY")
		t.Setenv("SHELL_TEST_KEY", "shell-secret-value")
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)

		visible, err := r.listModels(context.Background(), protocol.ListModelsParams{All: false})
		testassert.NoError(t, err)
		full, err := r.listModels(context.Background(), protocol.ListModelsParams{All: true})
		testassert.NoError(t, err)

		// Connected providers only: unsetp's model is in neither list;
		// openrouter's isolated key keeps it disconnected in both.
		for _, list := range []struct {
			name    string
			entries []protocol.ModelListEntry
		}{{"visible", visible.Models}, {"full", full.Models}} {
			for _, entry := range list.entries {
				testassert.Require(t, entry.Provider != "unsetp" && entry.Provider != "openrouter" && entry.Ref == entry.Provider+"/"+entry.Model && entry.Source != "", "%s list contains entry from a disconnected provider: %+v; %s list ref = %q, want the provider/model spelling; %s list entry %s carries no source label", list.name, entry, list.name, entry.Ref, list.name, entry.Ref)
			}
		}
		for _, entry := range visible.Models {
			testassert.Require(t, !entry.Hidden && !entry.ProviderHidden && (entry.Provider != "prov" || entry.Model != "m" || entry.DisplayName == "M" && entry.ProviderName == "prov"), "visible list contains hidden entry %+v; prov/m entry display names = (%q, %q), want the effective model and provider names", entry, entry.DisplayName, entry.ProviderName)
		}
		var hiddenOne, incompleteOne bool
		for _, entry := range full.Models {
			switch entry.Model {
			case "hidden_one":
				hiddenOne = entry.Provider == "prov" && entry.Hidden && !entry.ProviderHidden
			case "incomplete_one":
				incompleteOne = entry.Provider == "prov" && entry.Incomplete
			}
		}
		testassert.Require(t, hiddenOne && incompleteOne, "full list misses the hidden (%v) or incomplete (%v) entry with its flag", hiddenOne, incompleteOne)

		// The per-provider model read: every model view of one provider,
		// including the incomplete and hidden entries with usable flags.
		models, err := r.listProviderModels(context.Background(), "prov")
		testassert.NoError(t, err)
		testassert.Require(t, len(models.Models) == 3 && models.ConfigurationRevision.Generation == "1", "got %v, want %v; provider models revision = %+v, want generation 1", len(models.Models), 3, models.ConfigurationRevision)
		for _, model := range models.Models {
			testassert.Require(t, model.Usable == (model.ContextWindow > 0), "model %s usable = %v with window %d", model.Id, model.Usable, model.ContextWindow)
		}

		// Identity rules: an unknown valid provider is the typed unknown
		// error; an empty identity wraps the shared invalid sentinel.
		{
			_, err := r.getProvider(context.Background(), "absent")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "unknown provider"), "getProvider(absent) = %v, want the typed unknown-provider error", err)
		}
		{
			_, err := r.getProvider(context.Background(), "")
			testassert.Require(t, strings.Contains(err.Error(), "provider id must be non-empty"), "getProvider(empty) = %v, want the shared invalid sentinel", err)
		}
		{
			_, err := r.listProviderModels(context.Background(), "")
			testassert.Require(t, strings.Contains(err.Error(), "provider id must be non-empty"), "listProviderModels(empty) = %v, want the shared invalid sentinel", err)
		}
		detail, err := r.getProvider(context.Background(), "prov")
		testassert.NoError(t, err)
		testassert.Require(t, detail.Provider.Id == "prov" && detail.ConfigurationRevision.Generation == "1", "provider detail = %+v, want the projected view at generation 1", detail.Provider)
	})
}

// TestConfigurationReadsOwnReturnedValues pins deep caller ownership against
// an immutable serialized baseline: mutating every returned map, slice, and
// pointer never reaches the captured configuration.
func TestConfigurationReadsOwnReturnedValues(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		unsetenv(t, "SHELL_TEST_KEY", "MANAGED_TEST_KEY", "UNSET_TEST_KEY")
		t.Setenv("SHELL_TEST_KEY", "shell-secret-value")
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)

		baseline, err := r.getConfiguration(context.Background())
		testassert.NoError(t, err)
		expected, err := json.Marshal(baseline)
		testassert.NoError(t, err)

		// Mutate everything reachable through the returned view.
		prov := findProvider(baseline.Providers, "prov")
		prov.Headers["X-Trace"] = "mutated"
		prov.UserHeaders["X-Trace"] = "mutated"
		var model *protocol.ModelView
		for i := range prov.Models {
			if prov.Models[i].Id == "m" {
				model = &prov.Models[i]
			}
		}
		testassert.Require(t, model != nil && model.Cost != nil && model.Cost.Input != nil && model.ExtraBody != nil && len(model.InputModalities) != 0, "prov/m view missing expected references: %+v", model)
		*model.Cost.Input = 999
		(*model.ExtraBody)["side"] = "mutated"
		model.InputModalities[0] = "mutated"
		baseline.Settings.Sessions.ArchiveAfterDays = 999
		baseline.Settings.Plugins["tools"] = `{"mutated":true}`
		for i := range baseline.Agents {
			if baseline.Agents[i].Name == "worker" && len(baseline.Agents[i].Tools) > 0 {
				baseline.Agents[i].Tools[0] = "mutated"
			}
		}

		fresh, err := r.getConfiguration(context.Background())
		testassert.NoError(t, err)
		got, err := json.Marshal(fresh)
		testassert.NoError(t, err)
		testassert.Require(t, string(got) == string(expected), "a mutation of the returned view reached the captured configuration:\n%s\nwant\n%s", got, expected)

		// The model list and provider detail own their values the same way.
		list, err := r.listModels(context.Background(), protocol.ListModelsParams{All: true})
		testassert.NoError(t, err)
		listExpected, err := json.Marshal(list)
		testassert.NoError(t, err)
		if len(list.Models) > 0 {
			list.Models[0].DisplayName = "mutated"
			list.Models = nil
		}
		detail, err := r.getProvider(context.Background(), "prov")
		testassert.NoError(t, err)
		detailExpected, err := json.Marshal(detail)
		testassert.NoError(t, err)
		detail.Provider.Models[0].Name = "mutated"
		detail.Provider.Headers["X-Trace"] = "mutated"

		freshList, err := r.listModels(context.Background(), protocol.ListModelsParams{All: true})
		testassert.NoError(t, err)
		gotList, err := json.Marshal(freshList)
		testassert.NoError(t, err)
		testassert.Require(t, string(gotList) == string(listExpected), "a mutation of the returned list reached the captured configuration")
		freshDetail, err := r.getProvider(context.Background(), "prov")
		testassert.NoError(t, err)
		gotDetail, err := json.Marshal(freshDetail)
		testassert.NoError(t, err)
		testassert.Require(t, string(gotDetail) == string(detailExpected), "a mutation of the returned detail reached the captured configuration")
	})
}

// TestConfigurationFailedReloadLeavesClocksUnchanged pins the failed-reload
// rule over both clocks: a failed publication advances neither the
// configuration generation nor the warning revision, and the global warning
// groups stay the published candidate's.
func TestConfigurationFailedReloadLeavesClocksUnchanged(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, e := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)

		before, err := r.getConfiguration(context.Background())
		testassert.NoError(t, err)
		warningsBefore, err := r.getWarnings(context.Background())
		testassert.NoError(t, err)

		writeServiceFile(t, e.configPath, "{not json")
		{
			_, err := r.Reload(context.Background())
			testassert.Require(t, err != nil, "Reload over a malformed configuration succeeded, want failure")
		}

		after, err := r.getConfiguration(context.Background())
		testassert.NoError(t, err)
		testassert.Equal(t, after.ConfigurationRevision.Generation, before.ConfigurationRevision.Generation)
		warningsAfter, err := r.getWarnings(context.Background())
		testassert.NoError(t, err)
		testassert.Equal(t, warningsAfter.WarningsRevision.Revision, warningsBefore.WarningsRevision.Revision)
	})
}

// TestConfigurationSetupWarningsLifecycle pins the setup group's retained
// rules and its refresh at the shared publication path: the disconnected
// state reports no_provider and the primary model's unavailability, the
// connected state clears them, a catalog warning appears and clears, and the
// unfiltered read orders the actual stored groups lexically.
func TestConfigurationSetupWarningsLifecycle(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		unsetenv(t, "UNSET_SETUP_KEY")
		e := newOwnerEnv(t)
		disconnectedDoc := `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":"UNSET_SETUP_KEY"},"discovery":false,"models":{"m":{"context_window":4096}}}}}`
		r, _ := openConfigurationRuntimeWithEnv(t, store, e, disconnectedDoc, configurationAgentsDocument)
		defer closeProjectionRuntime(r)
		setupSub, err := r.Subscribe(64)
		testassert.NoError(t, err)
		t.Cleanup(setupSub.Close)

		// Disconnected: no_provider plus the primary model's unavailability.
		snap, err := r.getWarnings(context.Background())
		testassert.NoError(t, err)
		var setupKinds []string
		for _, warning := range snap.Warnings {
			if warning.Source == "runtime:setup" {
				setupKinds = append(setupKinds, warning.Kind)
			}
		}
		testassert.Require(t, len(setupKinds) == 2 && setupKinds[0] == "setup_no_provider" && setupKinds[1] == "setup_model_unavailable", "setup warnings = %v, want no_provider then model_unavailable", setupKinds)

		// Connect (the managed key writes through the retained path) and
		// reload: the setup warnings clear and the publication carries its
		// warning hint — one configuration event then one runtime-scoped
		// warning event with the advanced revision.
		testassert.NoError(t, r.managedEnv.Set("UNSET_SETUP_KEY", "setup-secret"))
		{
			_, err := r.Reload(context.Background())
			testassert.NoError(t, err)
		}
		clearingConfig, ok := nextEvent(t, setupSub)
		if !ok || eventKind(t, clearingConfig) != "configuration_changed" {
			t.Fatalf("clearing reload's configuration event = %s (ok=%v)", eventJSON(t, clearingConfig), ok)
		}
		clearingWarning, ok := nextEvent(t, setupSub)
		if !ok || eventKind(t, clearingWarning) != "warning_changed" {
			t.Fatalf("clearing reload's warning event = %s (ok=%v), want the required warning hint after the configuration event", eventJSON(t, clearingWarning), ok)
		}
		clearingBody, err := clearingWarning.AsWarningChangedEvent()
		testassert.NoError(t, err)
		testassert.Require(t, scopeKind(t, clearingBody.Scope) == "runtime", "clearing reload's warning scope = %+v, want the runtime scope", clearingBody.Scope)
		clear, err := r.getWarnings(context.Background())
		testassert.NoError(t, err)
		for _, warning := range clear.Warnings {
			testassert.Require(t, warning.Source != "runtime:setup", "setup warning survived the connected reload: %+v", warning)
		}
		revision, err := strconv.ParseUint(clear.WarningsRevision.Revision, 10, 64)
		testassert.Require(t, err == nil && revision != 0, "warning revision after the clearing reload = %v, want an advanced nonzero counter", clear.WarningsRevision.Revision)

		testassert.Equal(t, clearingBody.WarningsRevision.Revision, clear.WarningsRevision.Revision)

		// Idempotent reports advance nothing: reload again with no change —
		// exactly one configuration event and NO warning hint.
		{
			_, err := r.Reload(context.Background())
			testassert.NoError(t, err)
		}
		idempotentConfig, ok := nextEvent(t, setupSub)
		if !ok || eventKind(t, idempotentConfig) != "configuration_changed" {
			t.Fatalf("unchanged reload's configuration event = %s (ok=%v)", eventJSON(t, idempotentConfig), ok)
		}
		assertNoEvent(t, setupSub)
		idempotent, err := r.getWarnings(context.Background())
		testassert.NoError(t, err)
		testassert.Equal(t, idempotent.WarningsRevision.Revision, clear.WarningsRevision.Revision)

		// A catalog warning rides the published candidate; reads enumerate
		// the actual stored groups deterministically — global groups ordered
		// lexically by source, never a fixed source list — and the broken
		// provider also restores the setup group.
		brokenDoc := strings.Replace(disconnectedDoc, `"base_url":"https://prov.test/v1"`, `"base_url":""`, 1)
		writeServiceFile(t, e.configPath, brokenDoc)
		{
			_, err := r.Reload(context.Background())
			testassert.NoError(t, err)
		}
		broken, err := r.getWarnings(context.Background())
		testassert.NoError(t, err)
		var sources []string
		var catalogSeen, setupSeen bool
		for _, warning := range broken.Warnings {
			sources = append(sources, string(warning.Source))
			switch warning.Source {
			case "runtime:catalog":
				catalogSeen = true
			case "runtime:setup":
				setupSeen = true
			}
		}
		testassert.Require(t, slices.IsSorted(sources) && len(sources) >= 2 && sources[0] == "runtime:agents", "broken-reload warning order = %v, want the lexical global groups with the agents group first", sources)
		testassert.Require(t, catalogSeen && setupSeen, "broken-reload warnings = %+v, want both the catalog and setup groups", broken.Warnings)
	})
}

// TestConfigurationRosterRequiredArrays pins the Agent roster over the real
// current definitions — the builtins (including compact, which declares no
// capabilities and no tools) beside a nonempty custom sibling: every
// required array member serializes as a present [] rather than null, the
// selected model identity and trimmed write dir come from the existing
// normalization producer, and no prompt, LSP, or other private definition
// field is exposed.
func TestConfigurationRosterRequiredArrays(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)

		view, err := r.getConfiguration(context.Background())
		testassert.NoError(t, err)
		type rosterProbe struct {
			Name         string          `json:"name"`
			Model        string          `json:"model"`
			WriteDir     string          `json:"write_dir"`
			Tools        json.RawMessage `json:"tools"`
			Capabilities json.RawMessage `json:"capabilities"`
			Description  string          `json:"description"`
			Subagent     bool            `json:"subagent"`
			Readonly     bool            `json:"readonly"`
		}
		data, err := json.Marshal(view.Agents)
		testassert.NoError(t, err)
		var probes []rosterProbe
		testassert.NoError(t, json.Unmarshal(data, &probes))
		byName := make(map[string]rosterProbe, len(probes))
		for _, probe := range probes {
			for _, field := range []struct {
				name  string
				value json.RawMessage
			}{{"tools", probe.Tools}, {"capabilities", probe.Capabilities}} {
				testassert.Require(t, field.value != nil && string(field.value) != "null", "agent %q's required %s member = %q, want a present array", probe.Name, field.name, field.value)
			}
			byName[probe.Name] = probe
		}
		compact, ok := byName["compact"]
		if !ok {
			t.Fatalf("roster misses the compact builtin: %v", probeNames(probes, func(p rosterProbe) string { return p.Name }))
		}
		testassert.Require(t, string(compact.Tools) == "[]" && string(compact.Capabilities) == "[]", "compact arrays = tools %s capabilities %s, want the required empty []", compact.Tools, compact.Capabilities)
		// The retained resolution chain: compact inherits the primary's model
		// through secondary; the projection shows the resolved selection.
		testassert.Equal(t, compact.Model, "prov/m")
		worker, ok := byName["worker"]
		if !ok {
			t.Fatalf("roster misses the custom sibling: %v", probeNames(probes, func(p rosterProbe) string { return p.Name }))
		}
		testassert.Require(t, string(worker.Tools) == `["prod_read"]` && string(worker.Capabilities) == "[]" && worker.WriteDir == "/ws" && worker.Subagent && worker.Readonly && worker.Description == "w", "worker arrays = tools %s capabilities %s, want the nonempty tools and empty [] capabilities; worker public fields = %+v, want the normalized definition values", worker.Tools, worker.Capabilities, worker)
		testassert.Require(t, !strings.Contains(string(data), "system_prompt") && !strings.Contains(string(data), "lsp") && !strings.Contains(string(data), "prompt\":"), "roster exposes private definition fields: %s", data)
	})
}

func probeNames[T any](probes []T, name func(T) string) []string {
	out := make([]string, 0, len(probes))
	for _, probe := range probes {
		out = append(out, name(probe))
	}
	return out
}

// TestConfigurationProviderExtrasExactAndOwned pins the provider view's
// sidecar members through all three shared read paths: the captured
// extra_body with its exact side value and raw big-integer lexeme, the
// transport options, and the protocol metadata — each present with real
// values (no conditional skips), at the same generation, and owned against
// an independent serialized baseline.
func TestConfigurationProviderExtrasExactAndOwned(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		unsetenv(t, "SHELL_TEST_KEY", "MANAGED_TEST_KEY", "UNSET_TEST_KEY")
		t.Setenv("SHELL_TEST_KEY", "shell-secret-value")
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)

		// exactJSONMember marshals one decoded JSON value and compares its
		// exact lexeme — json.Number values must never round through float64.
		exactJSONMember := func(where, field string, value any, want string) {
			t.Helper()
			raw, err := json.Marshal(value)
			testassert.Require(t, err == nil, "%s: marshal %s: %v", where, field, err)
			testassert.Require(t, string(raw) == want, "%s: %s = %s, want the exact lexeme %s", where, field, raw, want)
		}
		checkExtras := func(where string, view protocol.Provider) {
			t.Helper()
			testassert.Require(t, view.ExtraBody != nil, "%s: prov extra_body missing, want the captured sidecar layer", where)
			{
				got := (*view.ExtraBody)["side"]
				testassert.Require(t, got == "value", "%s: prov extra_body side = %v, want the captured value", where, got)
			}
			exactJSONMember(where, "extra_body.big", (*view.ExtraBody)["big"], "9007199254740993")
			testassert.Require(t, view.ProtocolMetadata != nil && view.ProtocolMetadata.Family != nil && *view.ProtocolMetadata.Family == "testfam", "%s: prov protocol metadata family = %+v, want testfam", where, view.ProtocolMetadata)
			testassert.Require(t, view.ProtocolMetadata.MustPreserve != nil && slices.Equal(*view.ProtocolMetadata.MustPreserve, []string{"x_keep"}) && view.ProtocolMetadata.Drop != nil && slices.Equal(*view.ProtocolMetadata.Drop, []string{"x_drop"}), "%s: prov must_preserve = %v, want [x_keep]; %s: prov drop = %v, want [x_drop]", where, view.ProtocolMetadata.MustPreserve, where, view.ProtocolMetadata.Drop)
		}

		configView, err := r.getConfiguration(context.Background())
		testassert.NoError(t, err)
		prov := findProvider(configView.Providers, "prov")
		checkExtras("getConfiguration", prov)
		// The remaining provider/model fields the sidecar fixture carries:
		// the effective system role and the streamed-usage flag.
		testassert.Require(t, prov.SystemRole == protocol.SystemRole("system") && prov.UsageInStream, "prov system_role = %q usage_in_stream = %v, want the effective defaults", prov.SystemRole, prov.UsageInStream)
		for _, model := range prov.Models {
			testassert.Require(t, model.SystemRole == protocol.SystemRole("system") && model.UsageInStream, "prov/%s system_role = %q usage_in_stream = %v, want the effective defaults", model.Id, model.SystemRole, model.UsageInStream)
		}
		list, err := r.listProviders(context.Background())
		testassert.NoError(t, err)
		checkExtras("listProviders", findProvider(list.Providers, "prov"))
		detail, err := r.getProvider(context.Background(), "prov")
		testassert.NoError(t, err)
		checkExtras("getProvider", detail.Provider)
		testassert.Require(t, configView.ConfigurationRevision.Generation == list.ConfigurationRevision.Generation && list.ConfigurationRevision.Generation == detail.ConfigurationRevision.Generation, "the three read paths disagree on the generation: %q/%q/%q", configView.ConfigurationRevision.Generation, list.ConfigurationRevision.Generation, detail.ConfigurationRevision.Generation)

		// Ownership: the sidecar members are owned against an independent
		// serialized baseline — nested member mutations never reach the
		// captured configuration.
		baseline, err := json.Marshal(detail.Provider)
		testassert.NoError(t, err)
		(*detail.Provider.ExtraBody)["side"] = "mutated"
		(*detail.Provider.ExtraBody)["big"] = nil
		*detail.Provider.ProtocolMetadata.Family = "mutated"
		(*detail.Provider.ProtocolMetadata.MustPreserve)[0] = "mutated"
		(*detail.Provider.ProtocolMetadata.Drop)[0] = "mutated"
		fresh, err := r.getProvider(context.Background(), "prov")
		testassert.NoError(t, err)
		got, err := json.Marshal(fresh.Provider)
		testassert.NoError(t, err)
		testassert.Require(t, string(got) == string(baseline), "a mutation of the returned provider's sidecar members reached the captured configuration:\n%s\nwant\n%s", got, baseline)

		// The model rows keep their own owned cost/extra/modality values with
		// raw number lexemes.
		var model *protocol.ModelView
		for i := range fresh.Provider.Models {
			if fresh.Provider.Models[i].Id == "m" {
				model = &fresh.Provider.Models[i]
			}
		}
		testassert.Require(t, model != nil && model.Cost != nil && model.Cost.Input != nil && model.ExtraBody != nil, "prov/m view missing expected sidecar references: %+v", model)
		if *model.Cost.Input != 1.5 {
			t.Fatalf("prov/m cost input = %v, want the captured price", *model.Cost.Input)
		}
		testassert.Require(t, model.MaxOutputTokens == 0 && model.ProtocolMetadata != nil && model.ProtocolMetadata.Family != nil && *model.ProtocolMetadata.Family == "testfam", "got %v, want %v; prov/m protocol metadata = %+v, want the effective model-over-provider merge", model.MaxOutputTokens, 0, model.ProtocolMetadata)
		exactJSONMember("getProvider model", "extra_body.mside", (*model.ExtraBody)["mside"], "1")
	})
}

// TestConfigurationModelSourceExactThroughCapturedCatalog pins the EXACT
// source labels of every effective model through the Runtime projection,
// with the captured catalog produced from the same accepted layers: a
// bundled provider's shipped models are bundled, its user-layer model is
// user, and a model arriving through a discovery-cache record bound to the
// merged transport — written with the existing cache helper, read once by
// the reload build, never per read — is discovered.
func TestConfigurationModelSourceExactThroughCapturedCatalog(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		unsetenv(t, "SHELL_TEST_KEY", "MANAGED_TEST_KEY", "UNSET_TEST_KEY")
		t.Setenv("SHELL_TEST_KEY", "shell-secret-value")
		r, e := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)

		// Before any cache record: the bundled provider's shipped models are
		// bundled and the user-layer model is user; nothing is discovered.
		before, err := r.getConfiguration(context.Background())
		testassert.NoError(t, err)
		openrouter := findProvider(before.Providers, "openrouter")
		testassert.Require(t, openrouter.Builtin, "openrouter builtin = false, want the bundled identity")
		wantBefore := map[string]string{"m": "user"}
		for _, model := range openrouter.Models {
			want := wantBefore[model.Id]
			if want == "" {
				want = "bundled" // every shipped catalog model
			}
			testassert.Require(t, model.Source == protocol.ModelSource(want), "openrouter/%s source = %q, want %q", model.Id, model.Source, want)
		}

		// Seed a bound cache record from the effective merged transport the
		// first build produced, then reload: the rebuild adopts the discovered
		// model and labels it from the same accepted record.
		transport := catalog.Transport{BaseURL: openrouter.BaseUrl, APIKeyEnv: openrouter.ApiKeyEnv, Headers: openrouter.Headers}
		testassert.NoError(t, catalog.WriteDiscoveryCache(e.home, "openrouter", transport,
			catalog.DiscoveredProvider{Models: map[string]catalog.DiscoveredModel{"found-via-cache": {Name: "Found Via Cache", ContextWindow: 4096}}},
			time.Now().UTC()))
		{
			_, err := r.Reload(context.Background())
			testassert.NoError(t, err)
		}
		after, err := r.getConfiguration(context.Background())
		testassert.NoError(t, err)
		reloaded := findProvider(after.Providers, "openrouter")
		testassert.Require(t, reloaded.Id == "openrouter", "reloaded openrouter view missing")
		wantAfter := map[string]string{"m": "user", "found-via-cache": "discovered"}
		for _, model := range reloaded.Models {
			want := wantAfter[model.Id]
			if want == "" {
				want = "bundled"
			}
			testassert.Require(t, model.Source == protocol.ModelSource(want), "openrouter/%s source = %q, want %q", model.Id, model.Source, want)
		}
		discovered := reloaded.Models[0]
		for i := range reloaded.Models {
			if reloaded.Models[i].Id == "found-via-cache" {
				discovered = reloaded.Models[i]
			}
		}
		testassert.Require(t, discovered.Name == "Found Via Cache" && discovered.ContextWindow == 4096 && discovered.Usable, "discovered model view = %+v, want the record's adopted identity", discovered)

		// The picker carries the same exact labels from the captured catalog,
		// still connected-only: the disconnected openrouter appears in neither
		// list, and the connected custom provider's model is user.
		list, err := r.listModels(context.Background(), protocol.ListModelsParams{All: true})
		testassert.NoError(t, err)
		pickerLabels := map[string]string{}
		for _, entry := range list.Models {
			testassert.Require(t, entry.Provider != "openrouter", "the picker listed a disconnected provider's model: %+v", entry)
			pickerLabels[entry.Provider+"/"+entry.Model] = string(entry.Source)
		}
		testassert.Require(t, pickerLabels["prov/m"] == "user", "picker labels = %v, want the exact user label for the connected provider's model", pickerLabels)
	})
}
