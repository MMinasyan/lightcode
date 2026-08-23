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
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snap
}

// run executes doctor.Run and asserts zero filesystem writes under home.
func run(t *testing.T, p Params) (Report, bool) {
	t.Helper()
	before := snapshotTree(t, p.Home)
	report, noProviders := Run(p)
	after := snapshotTree(t, p.Home)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("doctor wrote to the filesystem:\nbefore: %v\nafter:  %v", before, after)
	}
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeAgents(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if report.Failures != 0 {
		t.Fatalf("fresh home has failures: %+v", report.Checks)
	}
	if !noProviders {
		t.Fatal("fresh home must report no providers connected")
	}
	c := find(t, report, "data", "dir")
	if c.Status != StatusOK || c.Detail != "not created yet (created on first run)" {
		t.Fatalf("data/dir = %+v", c)
	}
	for _, check := range report.Checks {
		if check.Group == "data" && check.Name != "dir" {
			t.Fatalf("data group not skipped on absent dir: %+v", check)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode")); !os.IsNotExist(err) {
		t.Fatal("doctor created the data dir")
	}
}

func TestMissingConfigReportedNotCreated(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700); err != nil {
		t.Fatal(err)
	}
	report, _ := run(t, params(home))
	c := find(t, report, "data", "config")
	if c.Status != StatusOK || c.Detail != "config.json: not created yet (created on first run)" {
		t.Fatalf("data/config = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, ".lightcode", "config.json")); !os.IsNotExist(err) {
		t.Fatal("doctor created config.json")
	}
}

func TestInvalidConfigFails(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{not json`)
	report, _ := run(t, params(home))
	if find(t, report, "data", "config").Status != StatusFail {
		t.Fatalf("invalid config not FAIL: %+v", report.Checks)
	}
	if report.Failures == 0 {
		t.Fatal("invalid config must count as a failure")
	}
}

func TestEmptySkeletonIsFreshMachine(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{"providers": {}, "default_model": ""}`)
	report, noProviders := run(t, params(home))
	if report.Failures != 0 {
		t.Fatalf("empty skeleton has failures: %+v", report.Checks)
	}
	if !noProviders {
		t.Fatal("empty skeleton must report no providers connected")
	}
	if find(t, report, "setup", "provider").Status != StatusWarn {
		t.Fatal("setup/provider should warn with nothing connected")
	}
	if find(t, report, "setup", "model").Status != StatusWarn {
		t.Fatal("setup/model should warn on empty primary model")
	}
}

func TestKeySourceClasses(t *testing.T) {
	t.Run("unset key warns", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		report, _ := run(t, params(home))
		c := find(t, report, "providers", "keyedtest")
		if c.Status != StatusWarn {
			t.Fatalf("unset key = %+v, want warn", c)
		}
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
		if c.Status != StatusOK || c.Detail != "key external, connected, 1 usable models" {
			t.Fatalf("shell key = %+v", c)
		}
		if noProviders {
			t.Fatal("a connected provider must clear the fresh-install hint")
		}
		if find(t, report, "setup", "provider").Status != StatusOK {
			t.Fatal("setup/provider should be ok with a connected provider")
		}
	})

	t.Run("dotenv-only key is managed and connected", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		if err := os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte("DOCTOR_TEST_KEY=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		report, _ := run(t, params(home))
		c := find(t, report, "providers", "keyedtest")
		if c.Status != StatusOK || c.Detail != "key managed, connected, 1 usable models" {
			t.Fatalf("dotenv-only key = %+v (the false-none/false-disconnected regression)", c)
		}
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
				if err := os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte(envBody), 0o600); err != nil {
					t.Fatal(err)
				}
				report, noProviders := run(t, params(home))
				c := find(t, report, "providers", "keyedtest")
				if c.Status != StatusOK || c.Detail != "key managed, not connected, 1 usable models" {
					t.Fatalf("empty dotenv value = %+v", c)
				}
				if !noProviders {
					t.Fatal("an empty key must not count as a connected provider")
				}
				if find(t, report, "setup", "provider").Status != StatusWarn {
					t.Fatal("setup/provider must warn when the only key is empty")
				}
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
		if err := os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte("DOCTOR_TEST_KEY=\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		report, _ := run(t, params(home))
		if c := find(t, report, "setup", "model"); c.Status != StatusWarn {
			t.Fatalf("setup/model = %+v, want warn with an empty key", c)
		}
	})

	t.Run("empty shell export is none and not connected", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		p := params(home)
		p.LookupEnv = func(name string) (string, bool) { return "", name == "DOCTOR_TEST_KEY" }
		report, noProviders := run(t, p)
		c := find(t, report, "providers", "keyedtest")
		if c.Status != StatusWarn {
			t.Fatalf("empty shell export = %+v, want the no-key warn (key-source none)", c)
		}
		if !noProviders {
			t.Fatal("an empty shell export must not count as connected")
		}
	})

	t.Run("empty shell export shadows a non-empty dotenv value", func(t *testing.T) {
		// Agent parity: LoadDotEnv skips keys the shell already set, even
		// to an empty value, so the .env credential never loads.
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		if err := os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte("DOCTOR_TEST_KEY=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		p := params(home)
		p.LookupEnv = func(name string) (string, bool) { return "", name == "DOCTOR_TEST_KEY" }
		report, noProviders := run(t, p)
		c := find(t, report, "providers", "keyedtest")
		if c.Status != StatusWarn {
			t.Fatalf("shadowed dotenv key = %+v, want the no-key warn", c)
		}
		if !noProviders {
			t.Fatal("a shell-shadowed .env credential must not count as connected")
		}
	})

	t.Run("shell beats dotenv", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, keyedProviderConfig)
		if err := os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte("DOCTOR_TEST_KEY=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		p := params(home)
		p.LookupEnv = func(name string) (string, bool) { return "x", name == "DOCTOR_TEST_KEY" }
		report, _ := run(t, p)
		if c := find(t, report, "providers", "keyedtest"); c.Detail != "key external, connected, 1 usable models" {
			t.Fatalf("shell+dotenv key = %+v, want external", c)
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
		if c.Status != StatusOK || c.Detail != "key keyless, connected, 1 usable models" {
			t.Fatalf("keyless = %+v", c)
		}
		if noProviders {
			t.Fatal("keyless connected provider must count as connected")
		}
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
		if c.Detail != "key external, not connected, 0 usable models" {
			t.Fatalf("no-usable-model provider = %+v", c)
		}
		if !noProviders {
			t.Fatal("a provider without usable models must not count as connected")
		}
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
			report, _ := run(t, p)
			if c := find(t, report, "setup", "model"); c.Status != tc.wantStatus {
				t.Fatalf("setup/model = %+v, want %s", c, tc.wantStatus)
			}
		})
	}
}

func TestCatalogWarningSeverities(t *testing.T) {
	t.Run("user_config_skip is a failure", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{"providers": {"droppedprovider": 42}, "default_model": ""}`)
		report, _ := run(t, params(home))
		c := find(t, report, "catalog", "droppedprovider")
		if c.Status != StatusFail {
			t.Fatalf("user_config_skip = %+v, want fail", c)
		}
		if report.Failures == 0 {
			t.Fatal("a dropped user provider must fail the run")
		}
	})

	t.Run("empty provider warns", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{
  "providers": {"emptytest": {"transport": {"base_url": "https://e.example/v1", "api_key_env": ""}, "discovery": false, "models": {}}},
  "default_model": ""
}`)
		report, _ := run(t, params(home))
		if c := find(t, report, "catalog", "emptytest"); c.Status != StatusWarn {
			t.Fatalf("empty_provider = %+v, want warn", c)
		}
		if report.Failures != 0 {
			t.Fatal("empty_provider must not fail the run")
		}
	})

	t.Run("incomplete model warns", func(t *testing.T) {
		home := t.TempDir()
		writeConfig(t, home, `{
  "providers": {"incompletetest": {"transport": {"base_url": "https://i.example/v1", "api_key_env": ""}, "discovery": false, "models": {"m1": {}}}},
  "default_model": ""
}`)
		report, _ := run(t, params(home))
		if c := find(t, report, "catalog", "incompletetest"); c.Status != StatusWarn {
			t.Fatalf("incomplete_model = %+v, want warn", c)
		}
	})

	t.Run("corrupt discovery cache warns", func(t *testing.T) {
		home := t.TempDir()
		dir := filepath.Join(home, ".lightcode", "cache", "discovery")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cacheprov.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		report, _ := run(t, params(home))
		if c := find(t, report, "catalog", "cacheprov"); c.Status != StatusWarn {
			t.Fatalf("discovery_failure = %+v, want warn", c)
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
		if len(checks) != 2 || checks[0].Status != StatusWarn || checks[1].Status != StatusWarn {
			t.Fatalf("checks = %+v", checks)
		}
		if want := `unknown warning kind "never_seen_kind": novel`; checks[1].Detail != want {
			t.Fatalf("unknown-kind detail = %q, want %q", checks[1].Detail, want)
		}
	})
}

func TestMalformedDotEnvLineWarns(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `{"providers": {}, "default_model": ""}`)
	if err := os.WriteFile(filepath.Join(home, ".lightcode", ".env"), []byte("no equals sign\nGOOD_KEY=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, _ := run(t, params(home))
	warned := false
	for _, c := range report.Checks {
		if c.Group == "data" && c.Name == "env" && c.Status == StatusWarn {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warn for the malformed .env line: %+v", report.Checks)
	}
}

func TestPermissionWarnings(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("K=v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, _ := run(t, params(home))
	if c := find(t, report, "data", "dir"); c.Status != StatusWarn {
		t.Fatalf("0755 data dir = %+v, want warn", c)
	}
	envWarned := false
	for _, c := range report.Checks {
		if c.Group == "data" && c.Name == "env" && c.Status == StatusWarn {
			envWarned = true
		}
	}
	if !envWarned {
		t.Fatal("0644 .env should warn")
	}
}

func TestSymlinkDataDirWarns(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, "realdata")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(home, ".lightcode")); err != nil {
		t.Fatal(err)
	}
	report, _ := run(t, params(home))
	c := find(t, report, "data", "dir")
	if c.Status != StatusWarn {
		t.Fatalf("symlinked data dir = %+v, want warn", c)
	}
}

func TestResolveErrorsFail(t *testing.T) {
	t.Run("config path error", func(t *testing.T) {
		p := params(t.TempDir())
		p.ConfigPathErr = fmt.Errorf("no home")
		report, _ := run(t, p)
		if find(t, report, "install", "config-path").Status != StatusFail {
			t.Fatal("config-path resolution error must FAIL")
		}
		if report.Failures == 0 {
			t.Fatal("resolution failure must count")
		}
	})

	t.Run("home error stops home-dependent groups", func(t *testing.T) {
		p := Params{HomeErr: fmt.Errorf("no home"), BinaryPath: "b", Version: "dev", Platform: "linux/amd64"}
		report, noProviders := Run(p)
		if find(t, report, "install", "home").Status != StatusFail {
			t.Fatal("home resolution error must FAIL")
		}
		if noProviders {
			t.Fatal("fresh-install hint must not show on a resolution failure")
		}
		for _, c := range report.Checks {
			if c.Group != "install" {
				t.Fatalf("home-dependent group ran without a home: %+v", c)
			}
		}
	})
}

func TestReportJSONShape(t *testing.T) {
	home := t.TempDir()
	report, _ := run(t, params(home))
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Checks []map[string]any `json:"checks"`
		OK     *int             `json:"ok"`
		Warn   *int             `json:"warnings"`
		Fail   *int             `json:"failures"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.OK == nil || decoded.Warn == nil || decoded.Fail == nil || len(decoded.Checks) == 0 {
		t.Fatalf("missing top-level fields: %s", b)
	}
	for _, c := range decoded.Checks {
		for _, field := range []string{"group", "name", "status", "detail"} {
			if _, ok := c[field]; !ok {
				t.Fatalf("check missing %q: %v", field, c)
			}
		}
	}
}
