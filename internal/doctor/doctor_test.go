package doctor

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		snap[path] = fmt.Sprintf("%d:%d:%v", info.Size(), info.ModTime().UnixNano(), info.IsDir())
		return nil
	})
	testassert.Require(t, err == nil || os.IsNotExist(err), "snapshot %s: %v", root, err)
	return snap
}

// run executes doctor.Run and asserts zero filesystem writes under home.
func run(t *testing.T, p Params) (Report, bool) {
	t.Helper()
	before := snapshotTree(t, p.Home)
	report, noProviders := Run(p)
	after := snapshotTree(t, p.Home)
	testassert.Require(t, reflect.DeepEqual(before, after), "doctor wrote to the filesystem:\nbefore: %v\nafter:  %v", before, after)
	return report, noProviders
}

func params(home string) Params {
	return Params{
		Home:       home,
		ConfigPath: filepath.Join(home, ".lightcode", "config.json"),
		WorkDir:    "/nonexistent-workdir",
		BinaryPath: "/test/lightcode",
		Version:    "dev",
		Platform:   "linux/amd64",
		LookupEnv:  func(string) (string, bool) { return "", false },
	}
}

func find(t *testing.T, r Report, group, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Group == group && c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %s/%s in %+v", group, name, r.Checks)
	return Check{}
}

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".lightcode")
	testassert.NoError(t, os.MkdirAll(dir, 0o700))
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600))
}

func writeAgents(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".lightcode")
	testassert.NoError(t, os.MkdirAll(dir, 0o700))
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, "agents.json"), []byte(body), 0o600))
}

// keyedProviderConfig is a valid user provider with one usable model whose
// key env is DOCTOR_TEST_KEY.
const keyedProviderConfig = `{
  "providers": {
    "keyedtest": {
      "name": "Keyed Test",
      "transport": {"base_url": "https://keyed.example/v1", "api_key_env": "DOCTOR_TEST_KEY"},
      "discovery": false,
      "models": {"m1": {"context_window": 8192}}
    }
  },
  "default_model": ""
}`

func TestFreshHome(t *testing.T) {
	home := t.TempDir()
	report, noProviders := run(t, params(home))
	testassert.Require(t, report.Failures == 0, "fresh home has failures: %+v", report.Checks)
	testassert.Require(t, noProviders, "fresh home must report no providers connected")
	c := find(t, report, "data", "dir")
	testassert.Require(t, c.Status == StatusOK && c.Detail == "not created yet (created on first run)", "data/dir = %+v", c)
	for name, want := range map[string]string{
		"config": "config.json: not created yet (created on first run)",
		"agents": "agents.json: not created yet (created on first run)",
		"env":    ".env: not created yet (created on first run)",
	} {
		{
			got := find(t, report, "data", name)
			testassert.Require(t, got.Status == StatusOK && got.Detail == want, "data/%s = %+v, want %q", name, got, want)
		}
	}
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode"))
		testassert.Require(t, os.IsNotExist(err), "doctor created the data dir")
	}
}

// TestExternalConfigDiagnosedWithoutDataDir pins that the addressed config
// path, not the default data directory, governs the read-only diagnosis: an
// external config.json (and its adjacent agents.json) drives the
// config/agents/provider/model checks while HOME/.lightcode stays absent.
func TestExternalConfigDiagnosedWithoutDataDir(t *testing.T) {
	writeExternal := func(t *testing.T, dir, name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		testassert.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}

	t.Run("existing config and agents", func(t *testing.T) {
		home := t.TempDir()
		external := t.TempDir()
		configPath := writeExternal(t, external, "config.json", keyedProviderConfig)
		agentsPath := writeExternal(t, external, "agents.json", `{"primary": {"model": "keyedtest/m1"}}`)
		p := params(home)
		p.ConfigPath = configPath
		p.LookupEnv = func(name string) (string, bool) { return "x", name == "DOCTOR_TEST_KEY" }
		externalBefore := snapshotTree(t, external)
		report, noProviders := run(t, p)
		testassert.Require(t, reflect.DeepEqual(externalBefore, snapshotTree(t, external)), "doctor wrote to the addressed external directory")
		{
			c := find(t, report, "data", "dir")
			testassert.Require(t, c.Status == StatusOK && c.Detail == "not created yet (created on first run)", "data/dir = %+v", c)
		}
		{
			c := find(t, report, "data", "config")
			testassert.Require(t, c.Status == StatusOK && c.Detail == configPath, "data/config = %+v, want the addressed external path", c)
		}
		{
			c := find(t, report, "data", "agents")
			testassert.Require(t, c.Status == StatusOK && c.Detail == agentsPath, "data/agents = %+v, want the addressed adjacent path", c)
		}
		{
			c := find(t, report, "providers", "keyedtest")
			testassert.Require(t, c.Status == StatusOK && c.Detail == "key external, connected, 1 usable models", "providers/keyedtest = %+v, want the external config's provider diagnosed", c)
		}
		{
			c := find(t, report, "setup", "model")
			testassert.Require(t, c.Status == StatusOK && c.Detail == "keyedtest/m1", "setup/model = %+v, want the external agents.json model diagnosed", c)
		}
		testassert.Require(t, !noProviders, "a connected external provider must clear the fresh-install hint")
		testassert.Require(t, report.Failures == 0, "external config has failures: %+v", report.Checks)
		{
			_, err := os.Stat(filepath.Join(home, ".lightcode"))
			testassert.Require(t, os.IsNotExist(err), "doctor created the data dir")
		}
	})

	t.Run("invalid config fails", func(t *testing.T) {
		p := params(t.TempDir())
		p.ConfigPath = writeExternal(t, t.TempDir(), "config.json", `{not json`)
		report, _ := run(t, p)
		{
			c := find(t, report, "data", "config")
			testassert.Require(t, c.Status == StatusFail, "data/config = %+v, want the addressed external file's parse failure", c)
		}
		testassert.Require(t, report.Failures != 0, "an invalid external config must count as a failure")
	})

	t.Run("missing config and agents report not created", func(t *testing.T) {
		p := params(t.TempDir())
		p.ConfigPath = filepath.Join(t.TempDir(), "config.json")
		report, _ := run(t, p)
		{
			c := find(t, report, "data", "config")
			testassert.Require(t, c.Status == StatusOK && c.Detail == "config.json: not created yet (created on first run)", "data/config = %+v", c)
		}
		{
			c := find(t, report, "data", "agents")
			testassert.Require(t, c.Status == StatusOK && c.Detail == "agents.json: not created yet (created on first run)", "data/agents = %+v", c)
		}
		testassert.Require(t, report.Failures == 0, "missing external files must not fail: %+v", report.Checks)
	})

	t.Run("directory error keeps its own diagnostic", func(t *testing.T) {
		// HOME exists as a regular file, so Lstat(home/.lightcode) fails
		// with ENOTDIR. The dir diagnostic must report that error and the
		// addressed external config must still be diagnosed.
		homeFile := filepath.Join(t.TempDir(), "home")
		testassert.NoError(t, os.WriteFile(homeFile, []byte("not a directory"), 0o600))
		p := params(homeFile)
		p.ConfigPath = writeExternal(t, t.TempDir(), "config.json", keyedProviderConfig)
		report, _ := run(t, p)
		{
			c := find(t, report, "data", "dir")
			testassert.Require(t, c.Status == StatusFail, "data/dir = %+v, want the Lstat error as FAIL", c)
		}
		{
			c := find(t, report, "data", "config")
			testassert.Require(t, c.Status == StatusOK && c.Detail == p.ConfigPath, "data/config = %+v, want the addressed config diagnosed despite the dir error", c)
		}
	})
}

func TestMissingConfigReportedNotCreated(t *testing.T) {
	home := t.TempDir()
	testassert.NoError(t, os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700))
	report, _ := run(t, params(home))
	c := find(t, report, "data", "config")
	testassert.Require(t, c.Status == StatusOK && c.Detail == "config.json: not created yet (created on first run)", "data/config = %+v", c)
	{
		_, err := os.Stat(filepath.Join(home, ".lightcode", "config.json"))
		testassert.Require(t, os.IsNotExist(err), "doctor created config.json")
	}
}

func TestInvalidConfigFails(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{not json`)
	report, _ := run(t, params(home))
	testassert.Require(t, find(t, report, "data", "config").Status == StatusFail, "invalid config not FAIL: %+v", report.Checks)
	testassert.Require(t, report.Failures != 0, "invalid config must count as a failure")
}

func TestEmptySkeletonIsFreshMachine(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{"providers": {}, "default_model": ""}`)
	report, noProviders := run(t, params(home))
	testassert.Require(t, report.Failures == 0, "empty skeleton has failures: %+v", report.Checks)
	testassert.Require(t, noProviders, "empty skeleton must report no providers connected")
	testassert.Require(t, find(t, report, "setup", "provider").Status == StatusWarn, "setup/provider should warn with nothing connected")
	testassert.Require(t, find(t, report, "setup", "model").Status == StatusWarn, "setup/model should warn on empty primary model")
}

func TestKeySourceClasses(t *testing.T) {
	t.Run("unset key warns", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		report, _ := run(t, params(home))
		c := find(t, report, "providers", "keyedtest")
		testassert.Require(t, c.Status == StatusWarn, "unset key = %+v, want warn", c)
	})

	t.Run("shell env key is external", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		p := params(home)
		p.LookupEnv = func(name string) (string, bool) {
			if name == "DOCTOR_TEST_KEY" {
				return "x", true
			}
			return "", false
		}
		report, noProviders := run(t, p)
		c := find(t, report, "providers", "keyedtest")
		testassert.Require(t, c.Status == StatusOK && c.Detail == "key external, connected, 1 usable models", "shell key = %+v", c)
		testassert.Require(t, !noProviders, "a connected provider must clear the fresh-install hint")
		testassert.Require(t, find(t, report, "setup", "provider").Status == StatusOK, "setup/provider should be ok with a connected provider")
	})

	t.Run("dotenv-only key is managed and connected", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		testassert.NoError(t, os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte("DOCTOR_TEST_KEY=x\n"), 0o600))
		report, _ := run(t, params(home))
		c := find(t, report, "providers", "keyedtest")
		testassert.Require(t, c.Status == StatusOK && c.Detail == "key managed, connected, 1 usable models", "dotenv-only key = %+v (the false-none/false-disconnected regression)", c)
	})

	t.Run("empty dotenv value is managed but not connected", func(t *testing.T) {
		// The template-style state: the user uncommented `KEY=` but never
		// pasted a key. The agent marks it managed yet not connected;
		// doctor must agree instead of reporting a false connection.
		for name, envBody := range map[string]string{
			"bare":          "DOCTOR_TEST_KEY=\n",
			"double quoted": "DOCTOR_TEST_KEY=\"\"\n",
			"single quoted": "DOCTOR_TEST_KEY=''\n",
		} {
			t.Run(name, func(t *testing.T) {
				home := t.TempDir()
				writeConfig(t, home, keyedProviderConfig)
				testassert.NoError(t, os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte(envBody), 0o600))
				report, noProviders := run(t, params(home))
				c := find(t, report, "providers", "keyedtest")
				testassert.Require(t, c.Status == StatusOK && c.Detail == "key managed, not connected, 1 usable models", "empty dotenv value = %+v", c)
				testassert.Require(t, noProviders, "an empty key must not count as a connected provider")
				testassert.Require(t, find(t, report, "setup", "provider").Status == StatusWarn, "setup/provider must warn when the only key is empty")
			})
		}
	})

	t.Run("empty dotenv value leaves the default model unavailable", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{
  "providers": {
    "keyedtest": {
      "transport": {"base_url": "https://keyed.example/v1", "api_key_env": "DOCTOR_TEST_KEY"},
      "discovery": false,
      "models": {"m1": {"context_window": 8192}}
    }
  },
  "default_model": "keyedtest/m1"
}`)
		writeAgents(t, home, `{"primary": {"model": "keyedtest/m1"}}`)
		testassert.NoError(t, os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte("DOCTOR_TEST_KEY=\n"), 0o600))
		report, _ := run(t, params(home))
		{
			c := find(t, report, "setup", "model")
			testassert.Require(t, c.Status == StatusWarn, "setup/model = %+v, want warn with an empty key", c)
		}
	})

	t.Run("empty shell export is none and not connected", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		p := params(home)
		p.LookupEnv = func(name string) (string, bool) { return "", name == "DOCTOR_TEST_KEY" }
		report, noProviders := run(t, p)
		c := find(t, report, "providers", "keyedtest")
		testassert.Require(t, c.Status == StatusWarn, "empty shell export = %+v, want the no-key warn (key-source none)", c)
		testassert.Require(t, noProviders, "an empty shell export must not count as connected")
	})

	t.Run("empty shell export shadows a non-empty dotenv value", func(t *testing.T) {
		// Agent parity: LoadDotEnv skips keys the shell already set, even
		// to an empty value, so the .env credential never loads.
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		testassert.NoError(t, os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte("DOCTOR_TEST_KEY=x\n"), 0o600))
		p := params(home)
		p.LookupEnv = func(name string) (string, bool) { return "", name == "DOCTOR_TEST_KEY" }
		report, noProviders := run(t, p)
		c := find(t, report, "providers", "keyedtest")
		testassert.Require(t, c.Status == StatusWarn, "shadowed dotenv key = %+v, want the no-key warn", c)
		testassert.Require(t, noProviders, "a shell-shadowed .env credential must not count as connected")
	})

	t.Run("shell beats dotenv", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		testassert.NoError(t, os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte("DOCTOR_TEST_KEY=x\n"), 0o600))
		p := params(home)
		p.LookupEnv = func(name string) (string, bool) { return "x", name == "DOCTOR_TEST_KEY" }
		report, _ := run(t, p)
		{
			c := find(t, report, "providers", "keyedtest")
			testassert.Require(t, c.Detail == "key external, connected, 1 usable models", "shell+dotenv key = %+v, want external", c)
		}
	})
}

func TestConnectionSemantics(t *testing.T) {
	t.Run("keyless with base url and usable model connects", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{
  "providers": {
    "keylesstest": {
      "transport": {"base_url": "http://localhost:11434/v1", "api_key_env": ""},
      "discovery": false,
      "models": {"m1": {"context_window": 8192}}
    }
  },
  "default_model": ""
}`)
		report, noProviders := run(t, params(home))
		c := find(t, report, "providers", "keylesstest")
		testassert.Require(t, c.Status == StatusOK && c.Detail == "key keyless, connected, 1 usable models", "keyless = %+v", c)
		testassert.Require(t, !noProviders, "keyless connected provider must count as connected")
	})

	t.Run("keyed with env set but no usable model stays disconnected", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{
  "providers": {
    "nousable": {
      "transport": {"base_url": "https://x.example/v1", "api_key_env": "DOCTOR_TEST_KEY"},
      "discovery": false,
      "models": {"m1": {}}
    }
  },
  "default_model": ""
}`)
		p := params(home)
		p.LookupEnv = func(name string) (string, bool) { return "x", name == "DOCTOR_TEST_KEY" }
		report, noProviders := run(t, p)
		c := find(t, report, "providers", "nousable")
		testassert.Require(t, c.Detail == "key external, not connected, 0 usable models", "no-usable-model provider = %+v", c)
		testassert.Require(t, noProviders, "a provider without usable models must not count as connected")
	})
}

func TestPrimaryModelChecks(t *testing.T) {
	configWithModel := func(modelBody string) string {
		return `{
  "providers": {
    "keyedtest": {
      "transport": {"base_url": "https://keyed.example/v1", "api_key_env": "DOCTOR_TEST_KEY"},
      "discovery": false,
      "models": {"m1": ` + modelBody + `}
    }
  }
}`
	}
	envSet := func(name string) (string, bool) { return "x", name == "DOCTOR_TEST_KEY" }

	cases := []struct {
		name       string
		config     string
		agents     string
		env        func(string) (string, bool)
		wantStatus string
	}{
		{"available primary model", configWithModel(`{"context_window": 8192}`), `{"primary": {"model": "keyedtest/m1"}}`, envSet, StatusOK},
		{"missing primary model", configWithModel(`{"context_window": 8192}`), `{"primary": {"model": "keyedtest/nosuch"}}`, envSet, StatusWarn},
		{"incomplete primary model", configWithModel(`{}`), `{"primary": {"model": "keyedtest/m1"}}`, envSet, StatusWarn},
		{"provider not connected", configWithModel(`{"context_window": 8192}`), `{"primary": {"model": "keyedtest/m1"}}`, nil, StatusWarn},
		{"hidden but complete primary model", configWithModel(`{"context_window": 8192, "hidden": true}`), `{"primary": {"model": "keyedtest/m1"}}`, envSet, StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeConfig(t, home, tc.config)
			writeAgents(t, home, tc.agents)
			p := params(home)
			if tc.env != nil {
				p.LookupEnv = tc.env
			}
			baseline, _ := run(t, p)
			{
				c := find(t, baseline, "setup", "model")
				testassert.Require(t, c.Status == tc.wantStatus, "setup/model = %+v, want %s", c, tc.wantStatus)
			}

			// Custom definitions that the agents parser drops with its own
			// warnings must not leak into the doctor check stream: the
			// complete report for the augmented document must equal the
			// baseline report. Each definition carries a valid model and
			// fails only at its intended boundary (unavailable capability,
			// unavailable tool, wrong-typed capabilities member).
			var doc map[string]json.RawMessage
			testassert.NoError(t, json.Unmarshal([]byte(tc.agents), &doc))
			doc["custom_ghost_capability"] = json.RawMessage(`{"model": "test/ghost", "capabilities": ["not-declared"]}`)
			doc["custom_ghost_tool"] = json.RawMessage(`{"model": "test/ghost", "tools": ["not-compiled"]}`)
			doc["custom_broken_capabilities"] = json.RawMessage(`{"model": "test/ghost", "capabilities": "not-a-list"}`)
			augmented, err := json.Marshal(doc)
			testassert.NoError(t, err)
			writeAgents(t, home, string(augmented))
			report, _ := run(t, p)
			testassert.Require(t, reflect.DeepEqual(report, baseline), "custom definitions changed the doctor report:\nbaseline:  %+v\naugmented: %+v", baseline, report)
		})
	}
}

func TestCatalogWarningSeverities(t *testing.T) {
	t.Run("user_config_skip is a failure", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{"providers": {"droppedprovider": 42}, "default_model": ""}`)
		report, _ := run(t, params(home))
		c := find(t, report, "catalog", "droppedprovider")
		testassert.Require(t, c.Status == StatusFail, "user_config_skip = %+v, want fail", c)
		testassert.Require(t, report.Failures != 0, "a dropped user provider must fail the run")
	})

	t.Run("empty provider warns", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{
  "providers": {"emptytest": {"transport": {"base_url": "https://e.example/v1", "api_key_env": ""}, "discovery": false, "models": {}}},
  "default_model": ""
}`)
		report, _ := run(t, params(home))
		{
			c := find(t, report, "catalog", "emptytest")
			testassert.Require(t, c.Status == StatusWarn, "empty_provider = %+v, want warn", c)
		}
		testassert.Require(t, report.Failures == 0, "empty_provider must not fail the run")
	})

	t.Run("incomplete model warns", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{
  "providers": {"incompletetest": {"transport": {"base_url": "https://i.example/v1", "api_key_env": ""}, "discovery": false, "models": {"m1": {}}}},
  "default_model": ""
}`)
		report, _ := run(t, params(home))
		{
			c := find(t, report, "catalog", "incompletetest")
			testassert.Require(t, c.Status == StatusWarn, "incomplete_model = %+v, want warn", c)
		}
	})

	t.Run("corrupt discovery cache warns", func(t *testing.T) {
		home := t.TempDir()
		dir := filepath.Join(home, ".lightcode", "cache", "discovery")
		testassert.NoError(t, os.MkdirAll(dir, 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(dir, "cacheprov.json"), []byte("{not json"), 0o600))
		report, _ := run(t, params(home))
		{
			c := find(t, report, "catalog", "cacheprov")
			testassert.Require(t, c.Status == StatusWarn, "discovery_failure = %+v, want warn", c)
		}
	})

	t.Run("severity mapping covers bundled skip and unknown kinds", func(t *testing.T) {
		var checks []Check
		add := func(group, name, status, detail string) {
			checks = append(checks, Check{Group: group, Name: name, Status: status, Detail: detail})
		}
		catalogChecks([]catalog.Warning{
			{Kind: "bundled_config_skip", Provider: "bp", Message: "broken bundled"},
			{Kind: "never_seen_kind", Provider: "np", Message: "novel"},
		}, 0, add)
		testassert.Require(t, len(checks) == 2 && checks[0].Status == StatusWarn && checks[1].Status == StatusWarn, "checks = %+v", checks)
		{
			want := `unknown warning kind "never_seen_kind": novel`
			testassert.Equal(t, checks[1].Detail, want)
		}
	})
}

func TestMalformedDotEnvLineWarns(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{"providers": {}, "default_model": ""}`)
	testassert.NoError(t, os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte("no equals sign\nGOOD_KEY=v\n"), 0o600))
	report, _ := run(t, params(home))
	warned := false
	for _, c := range report.Checks {
		if c.Group == "data" && c.Name == "env" && c.Status == StatusWarn {
			warned = true
		}
	}
	testassert.Require(t, warned, "no warn for the malformed .env line: %+v", report.Checks)
}

func TestPermissionWarnings(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".lightcode")
	testassert.NoError(t, os.MkdirAll(dir, 0o755))
	testassert.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("K=v\n"), 0o644))
	report, _ := run(t, params(home))
	{
		c := find(t, report, "data", "dir")
		testassert.Require(t, c.Status == StatusWarn, "0755 data dir = %+v, want warn", c)
	}
	envWarned := false
	for _, c := range report.Checks {
		if c.Group == "data" && c.Name == "env" && c.Status == StatusWarn {
			envWarned = true
		}
	}
	testassert.Require(t, envWarned, "0644 .env should warn")
}

func TestSymlinkDataDirWarns(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, "realdata")
	testassert.NoError(t, os.MkdirAll(real, 0o700))
	testassert.NoError(t, os.Symlink(real, filepath.Join(home, ".lightcode")))
	report, _ := run(t, params(home))
	c := find(t, report, "data", "dir")
	testassert.Require(t, c.Status == StatusWarn, "symlinked data dir = %+v, want warn", c)
}

func TestResolveErrorsFail(t *testing.T) {
	t.Run("config path error", func(t *testing.T) {
		p := params(t.TempDir())
		p.ConfigPathErr = fmt.Errorf("no home")
		report, _ := run(t, p)
		testassert.Require(t, find(t, report, "install", "config-path").Status == StatusFail, "config-path resolution error must FAIL")
		testassert.Require(t, report.Failures != 0, "resolution failure must count")
	})

	t.Run("home error stops home-dependent groups", func(t *testing.T) {
		p := Params{HomeErr: fmt.Errorf("no home"), BinaryPath: "b", Version: "dev", Platform: "linux/amd64"}
		report, noProviders := Run(p)
		testassert.Require(t, find(t, report, "install", "home").Status == StatusFail, "home resolution error must FAIL")
		testassert.Require(t, !noProviders, "fresh-install hint must not show on a resolution failure")
		for _, c := range report.Checks {
			testassert.Require(t, c.Group == "install", "home-dependent group ran without a home: %+v", c)
		}
	})
}

func TestReportJSONShape(t *testing.T) {
	home := t.TempDir()
	report, _ := run(t, params(home))
	b, err := json.Marshal(report)
	testassert.NoError(t, err)
	var decoded struct {
		Checks []map[string]any `json:"checks"`
		OK     *int             `json:"ok"`
		Warn   *int             `json:"warnings"`
		Fail   *int             `json:"failures"`
	}
	testassert.NoError(t, json.Unmarshal(b, &decoded))
	testassert.Require(t, decoded.OK != nil && decoded.Warn != nil && decoded.Fail != nil && len(decoded.Checks) != 0, "missing top-level fields: %s", b)
	for _, c := range decoded.Checks {
		for _, field := range []string{"group", "name", "status", "detail"} {
			{
				_, ok := c[field]
				testassert.Require(t, ok, "check missing %q: %v", field, c)
			}
		}
	}
}
