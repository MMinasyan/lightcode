package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/MMinasyan/lightcode/internal/agent"
	lcconfig "github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/selfupdate"
	"github.com/MMinasyan/lightcode/internal/server"
	"github.com/MMinasyan/lightcode/internal/version"
)

// capture swaps the dispatcher streams for one dispatch call.
func capture(t *testing.T, argv []string) (launchGUI bool, code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	oldOut, oldErr := outW, errW
	outW, errW = &out, &errb
	defer func() { outW, errW = oldOut, oldErr }()
	launchGUI, code = dispatch(argv)
	return launchGUI, code, out.String(), errb.String()
}

func TestDispatchUnknownCommand(t *testing.T) {
	gui, code, stdout, stderr := capture(t, []string{"lightcode", "frobnicate"})
	if gui {
		t.Fatal("unknown command must not reach the GUI path")
	}
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, `lightcode: unknown command "frobnicate"`) {
		t.Fatalf("stderr missing unknown-command message: %q", stderr)
	}
	if !strings.Contains(stderr, "Run 'lightcode help' for usage.") {
		t.Fatalf("stderr missing usage hint: %q", stderr)
	}
}

func TestDispatchUnknownCommandEscapesControlBytes(t *testing.T) {
	_, code, _, stderr := capture(t, []string{"lightcode", "\x1b]0;evil\x07"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if strings.ContainsAny(stderr, "\x1b\x07") {
		t.Fatalf("raw control bytes reached stderr: %q", stderr)
	}
	if !strings.Contains(stderr, `\x1b`) {
		t.Fatalf("escaped form missing from stderr: %q", stderr)
	}
}

func TestDispatchHelpForms(t *testing.T) {
	for _, argv := range [][]string{
		{"lightcode", "help"},
		{"lightcode", "-h"},
		{"lightcode", "--help"},
	} {
		gui, code, stdout, stderr := capture(t, argv)
		if gui || code != 0 {
			t.Fatalf("%v: gui=%v code=%d, want handled exit 0", argv, gui, code)
		}
		if stderr != "" {
			t.Fatalf("%v: stderr must be empty, got %q", argv, stderr)
		}
		for _, want := range []string{
			"Usage: lightcode [command]",
			"desktop", "cli", "serve", "stop", "acp", "help",
			"(default)",
			"Exit codes: 0 success, 1 failure, 2 usage error.",
		} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("%v: stdout missing %q:\n%s", argv, want, stdout)
			}
		}
	}
}

func TestRunStopNoOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	oldOut := outW
	outW = &out
	t.Cleanup(func() { outW = oldOut })
	if err := runStop(); err != nil {
		t.Fatalf("runStop: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "lightcode: no owner running") {
		t.Fatalf("runStop output = %q", got)
	}
}

func TestRunStopRemovesStaleOwnerLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := server.Write(home, server.LockFile{Port: 1, Token: "stale", PID: -1}); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	var out bytes.Buffer
	oldOut := outW
	outW = &out
	t.Cleanup(func() { outW = oldOut })
	if err := runStop(); err != nil {
		t.Fatalf("runStop: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "lightcode: removed stale owner lock") {
		t.Fatalf("runStop output = %q", got)
	}
	if _, err := server.Read(home); !os.IsNotExist(err) {
		t.Fatalf("owner lock after stale stop = %v, want removed", err)
	}
}

func TestRunStopLiveOwner(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	t.Setenv("HOME", home)
	a := newCommandTestAgent(t, home, projectRoot)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := server.New(a, server.Config{})
	_, done, err := srv.Start(ctx, home)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var out bytes.Buffer
	oldOut := outW
	outW = &out
	t.Cleanup(func() { outW = oldOut })
	if err := runStop(); err != nil {
		t.Fatalf("runStop: %v", err)
	}
	waitCommandOwnerDone(t, done)
	if got := out.String(); !strings.Contains(got, "lightcode: owner stopped") {
		t.Fatalf("runStop output = %q", got)
	}
	if _, err := server.Read(home); !os.IsNotExist(err) {
		t.Fatalf("owner lock after live stop = %v, want removed", err)
	}
}

func TestDispatchHelpForCommand(t *testing.T) {
	_, code, stdout, stderr := capture(t, []string{"lightcode", "help", "serve"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stderr != "" {
		t.Fatalf("stderr must be empty, got %q", stderr)
	}
	if !strings.Contains(stdout, "Usage: lightcode serve [flags]") {
		t.Fatalf("stdout missing serve usage: %q", stdout)
	}
	if !strings.Contains(stdout, "-port") {
		t.Fatalf("stdout missing -port flag: %q", stdout)
	}
}

func newCommandTestAgent(t *testing.T, home string, projectRoot string) *agentpkg.Agent {
	t.Helper()
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
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 }
      }
    }
  },
  "default_model": "test/test-model"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lightcodeDir, "agents.json"), []byte(`{"primary": {"model": "test/test-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := lcconfig.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := agentpkg.New(agentpkg.Config{Cfg: cfg, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

func waitCommandOwnerDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("owner shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not shut down")
	}
}

func TestDispatchHelpUnknownCommand(t *testing.T) {
	_, code, stdout, stderr := capture(t, []string{"lightcode", "help", "nosuch"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, `lightcode: unknown command "nosuch"`) {
		t.Fatalf("stderr missing message: %q", stderr)
	}
}

func TestDispatchHelpExtraPositional(t *testing.T) {
	_, code, _, stderr := capture(t, []string{"lightcode", "help", "a", "b"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage: lightcode help [command]") {
		t.Fatalf("stderr missing help usage: %q", stderr)
	}
}

func TestDispatchBareReachesGUIPath(t *testing.T) {
	gui, code, stdout, stderr := capture(t, []string{"lightcode"})
	if !gui || code != 0 {
		t.Fatalf("gui=%v code=%d, want GUI path", gui, code)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("bare invocation must print nothing, got out=%q err=%q", stdout, stderr)
	}
}

func TestDispatchDesktopSameAsBare(t *testing.T) {
	gui, code, stdout, stderr := capture(t, []string{"lightcode", "desktop"})
	if !gui || code != 0 {
		t.Fatalf("gui=%v code=%d, want GUI path", gui, code)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("desktop must print nothing, got out=%q err=%q", stdout, stderr)
	}
}

func TestShouldDetachRespectsEnv(t *testing.T) {
	t.Setenv("LIGHTCODE_DETACHED", "1")
	if shouldDetach() {
		t.Fatal("LIGHTCODE_DETACHED=1 must take the Wails path, not detach again")
	}
	t.Setenv("LIGHTCODE_DETACHED", "")
	if !shouldDetach() {
		t.Fatal("without LIGHTCODE_DETACHED=1 the process must detach")
	}
}

func TestDispatchPerCommandHelpFlag(t *testing.T) {
	for _, name := range []string{"desktop", "cli", "serve", "acp", "doctor", "completion", "models", "config", "help"} {
		_, code, stdout, stderr := capture(t, []string{"lightcode", name, "-h"})
		if code != 0 {
			t.Fatalf("%s -h: exit code = %d, want 0", name, code)
		}
		if stderr != "" {
			t.Fatalf("%s -h: stderr must be empty, got %q", name, stderr)
		}
		if !strings.Contains(stdout, "Usage: lightcode "+name) {
			t.Fatalf("%s -h: stdout missing usage: %q", name, stdout)
		}
	}
}

func TestDispatchBadFlagPrintsUsageOnce(t *testing.T) {
	_, code, stdout, stderr := capture(t, []string{"lightcode", "serve", "-port=abc"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	if !strings.HasPrefix(stderr, "lightcode: ") {
		t.Fatalf("stderr missing error prefix: %q", stderr)
	}
	if n := strings.Count(stderr, "Usage: lightcode serve"); n != 1 {
		t.Fatalf("usage printed %d times, want exactly once:\n%s", n, stderr)
	}
}

func TestDispatchUnexpectedPositionals(t *testing.T) {
	for _, argv := range [][]string{
		{"lightcode", "cli", "extra"},
		{"lightcode", "acp", "extra"},
		{"lightcode", "desktop", "extra"},
		{"lightcode", "serve", "extra"},
		{"lightcode", "doctor", "extra"},
		{"lightcode", "uninstall", "extra"},
		{"lightcode", "config", "extra"},
	} {
		gui, code, stdout, stderr := capture(t, argv)
		if gui {
			t.Fatalf("%v: must not reach the GUI path", argv)
		}
		if code != 2 {
			t.Fatalf("%v: exit code = %d, want 2", argv, code)
		}
		if stdout != "" {
			t.Fatalf("%v: stdout must be empty, got %q", argv, stdout)
		}
		if !strings.Contains(stderr, `unexpected argument "extra"`) {
			t.Fatalf("%v: stderr missing message: %q", argv, stderr)
		}
	}
}

func TestDispatchVersion(t *testing.T) {
	oldV, oldC := version.Version, version.Commit
	version.Version, version.Commit = "v9.9.9", "abcdef1"
	defer func() { version.Version, version.Commit = oldV, oldC }()

	for _, argv := range [][]string{
		{"lightcode", "version"},
		{"lightcode", "-v"},
		{"lightcode", "--version"},
	} {
		_, code, stdout, stderr := capture(t, argv)
		if code != 0 {
			t.Fatalf("%v: exit code = %d, want 0", argv, code)
		}
		if stderr != "" {
			t.Fatalf("%v: stderr must be empty, got %q", argv, stderr)
		}
		want := "lightcode v9.9.9 (" + runtime.GOOS + "/" + runtime.GOARCH + ")\n"
		if stdout != want {
			t.Fatalf("%v: stdout = %q, want %q", argv, stdout, want)
		}
	}
}

func TestDispatchVersionJSON(t *testing.T) {
	oldV, oldC := version.Version, version.Commit
	version.Version, version.Commit = "v9.9.9", "abcdef1"
	defer func() { version.Version, version.Commit = oldV, oldC }()

	_, code, stdout, stderr := capture(t, []string{"lightcode", "version", "--json"})
	if code != 0 || stderr != "" {
		t.Fatalf("exit=%d stderr=%q, want clean exit 0", code, stderr)
	}
	want := `{"version":"v9.9.9","commit":"abcdef1","os":"` + runtime.GOOS + `","arch":"` + runtime.GOARCH + `"}` + "\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestDispatchVersionExtraArgs(t *testing.T) {
	for _, argv := range [][]string{
		{"lightcode", "version", "extra"},
		{"lightcode", "-v", "extra"},
	} {
		_, code, stdout, stderr := capture(t, argv)
		if code != 2 {
			t.Fatalf("%v: exit code = %d, want 2", argv, code)
		}
		if stdout != "" {
			t.Fatalf("%v: stdout must be empty, got %q", argv, stdout)
		}
		if !strings.Contains(stderr, `unexpected argument "extra"`) {
			t.Fatalf("%v: stderr missing message: %q", argv, stderr)
		}
	}
}

func TestDispatchDoctor(t *testing.T) {
	t.Run("fresh home exits 0 with the report on stdout", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "doctor"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if stderr != "" {
			t.Fatalf("stderr must be empty, got %q", stderr)
		}
		if !strings.Contains(stdout, "doctor: ") {
			t.Fatalf("stdout missing summary line: %q", stdout)
		}
		if !strings.Contains(stdout, "no providers connected yet - this is expected on a new install") {
			t.Fatalf("stdout missing fresh-install appendix: %q", stdout)
		}
		if strings.Contains(stdout, "fail ") {
			t.Fatalf("fresh home must not FAIL: %q", stdout)
		}
		if _, err := os.Stat(filepath.Join(home, ".lightcode")); !os.IsNotExist(err) {
			t.Fatal("doctor created the data dir")
		}
	})

	t.Run("json emits the report shape on stdout", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "doctor", "--json"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if stderr != "" {
			t.Fatalf("stderr must be empty, got %q", stderr)
		}
		var decoded struct {
			Checks []map[string]any `json:"checks"`
			OK     *int             `json:"ok"`
			Warn   *int             `json:"warnings"`
			Fail   *int             `json:"failures"`
		}
		if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
			t.Fatalf("stdout is not valid JSON: %v\n%q", err, stdout)
		}
		if decoded.OK == nil || decoded.Warn == nil || decoded.Fail == nil || len(decoded.Checks) == 0 {
			t.Fatalf("missing top-level fields: %s", stdout)
		}
		for _, field := range []string{"group", "name", "status", "detail"} {
			if _, ok := decoded.Checks[0][field]; !ok {
				t.Fatalf("check missing %q: %v", field, decoded.Checks[0])
			}
		}
	})

	t.Run("invalid config exits 1", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		if err := os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".lightcode", "config.json"), []byte("{bad json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, code, stdout, stderr := capture(t, []string{"lightcode", "doctor"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstdout: %s", code, stdout)
		}
		if !strings.Contains(stdout, "fail data/config:") {
			t.Fatalf("stdout missing config FAIL: %q", stdout)
		}
		if !strings.HasPrefix(stderr, "lightcode: ") {
			t.Fatalf("stderr missing error prefix: %q", stderr)
		}
	})

	t.Run("unresolvable home exits 1", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("LIGHTCODE_CONFIG", "")
		_, code, stdout, _ := capture(t, []string{"lightcode", "doctor"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstdout: %s", code, stdout)
		}
		if !strings.Contains(stdout, "fail install/") {
			t.Fatalf("stdout missing install-group FAIL: %q", stdout)
		}
	})
}

func setVersion(t *testing.T, v, commit string) {
	t.Helper()
	oldV, oldC := version.Version, version.Commit
	version.Version, version.Commit = v, commit
	t.Cleanup(func() { version.Version, version.Commit = oldV, oldC })
}

func setLatestEndpoint(t *testing.T, tag string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://github.com/MMinasyan/lightcode/releases/tag/"+tag)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	old := selfupdate.LatestEndpoint
	selfupdate.LatestEndpoint = srv.URL
	t.Cleanup(func() { selfupdate.LatestEndpoint = old })
}

func TestDispatchUpgradeUsageErrors(t *testing.T) {
	t.Setenv("LIGHTCODE_RELEASE_BASE", "")
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"lightcode", "upgrade", "v1.0.0", "v2.0.0"}, `unexpected argument "v2.0.0"`},
		{[]string{"lightcode", "upgrade", "foo"}, `invalid version "foo" (expected vMAJOR.MINOR.PATCH)`},
		{[]string{"lightcode", "upgrade", "v0.0.4", "--check"}, "--check takes no version argument"},
		{[]string{"lightcode", "upgrade", "--check", "v0.0.4"}, "--check takes no version argument"},
		{[]string{"lightcode", "upgrade", "--json"}, "--json requires --check"},
	}
	for _, tc := range cases {
		_, code, stdout, stderr := capture(t, tc.argv)
		if code != 2 {
			t.Fatalf("%v: exit code = %d, want 2\nstderr: %s", tc.argv, code, stderr)
		}
		if stdout != "" {
			t.Fatalf("%v: stdout must be empty, got %q", tc.argv, stdout)
		}
		if !strings.Contains(stderr, "lightcode: "+tc.want) {
			t.Fatalf("%v: stderr = %q, want substring %q", tc.argv, stderr, tc.want)
		}
	}
}

func TestDispatchUpgradeCheck(t *testing.T) {
	t.Setenv("LIGHTCODE_RELEASE_BASE", "")

	t.Run("update available", func(t *testing.T) {
		setVersion(t, "v0.0.3", "abc1234")
		setLatestEndpoint(t, "v0.0.4")
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade", "--check"})
		if code != 0 || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		want := "current: v0.0.3, latest: v0.0.4 (update available; run: lightcode upgrade)\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		entries, _ := os.ReadDir(home)
		if len(entries) != 0 {
			t.Fatalf("--check wrote files: %v", entries)
		}
	})

	t.Run("up to date", func(t *testing.T) {
		setVersion(t, "v0.0.4", "")
		setLatestEndpoint(t, "v0.0.4")
		_, code, stdout, _ := capture(t, []string{"lightcode", "upgrade", "--check"})
		if code != 0 || stdout != "current: v0.0.4, latest: v0.0.4 (up to date)\n" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
	})

	t.Run("dev build", func(t *testing.T) {
		setVersion(t, "dev", "abc1234")
		setLatestEndpoint(t, "v0.0.4")
		_, code, stdout, _ := capture(t, []string{"lightcode", "upgrade", "--check"})
		if code != 0 || stdout != "current: dev (abc1234), latest: v0.0.4\n" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
	})

	t.Run("below floor wins regardless of current", func(t *testing.T) {
		setVersion(t, "v0.0.3", "")
		setLatestEndpoint(t, "v0.0.2")
		_, code, stdout, _ := capture(t, []string{"lightcode", "upgrade", "--check"})
		if code != 0 || stdout != "current: v0.0.3, latest: v0.0.2 (not selfupdate-capable; use the installer)\n" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
	})

	t.Run("json golden", func(t *testing.T) {
		setVersion(t, "v0.0.3", "abc1234")
		setLatestEndpoint(t, "v0.0.4")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade", "--check", "--json"})
		if code != 0 || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		want := `{"current":"v0.0.3","latest":"v0.0.4","status":"update-available","updateAvailable":true,"installable":true}` + "\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("refuses under release base regardless of euid", func(t *testing.T) {
		t.Setenv("LIGHTCODE_RELEASE_BASE", "http://127.0.0.1:1/assets")
		old := selfupdate.Geteuid
		selfupdate.Geteuid = func() int { return 0 }
		t.Cleanup(func() { selfupdate.Geteuid = old })
		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade", "--check"})
		if code != 2 || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.Contains(stderr, "lightcode: LIGHTCODE_RELEASE_BASE is set; --check is meaningless against a directory base") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("resolution failure exits 1", func(t *testing.T) {
		setVersion(t, "v0.0.3", "")
		old := selfupdate.LatestEndpoint
		selfupdate.LatestEndpoint = "http://127.0.0.1:1/latest"
		t.Cleanup(func() { selfupdate.LatestEndpoint = old })
		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade", "--check"})
		if code != 1 || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.HasPrefix(stderr, "lightcode: ") {
			t.Fatalf("stderr = %q", stderr)
		}
	})
}

func TestDispatchUpgradeGates(t *testing.T) {
	t.Setenv("LIGHTCODE_RELEASE_BASE", "")

	t.Run("dev build refuses without explicit target", func(t *testing.T) {
		setVersion(t, "dev", "abc1234")
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		want := "lightcode: this is a development build (dev (abc1234)); pass an explicit version to upgrade anyway: lightcode upgrade v0.0.3"
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want substring %q", stderr, want)
		}
	})

	t.Run("release base requires explicit target", func(t *testing.T) {
		setVersion(t, "v0.0.3", "")
		t.Setenv("LIGHTCODE_RELEASE_BASE", "http://127.0.0.1:1/assets")
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade"})
		if code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
		if !strings.Contains(stderr, "lightcode: LIGHTCODE_RELEASE_BASE is set; pass an explicit version") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("root drops release base with a note", func(t *testing.T) {
		setVersion(t, "dev", "abc1234")
		t.Setenv("LIGHTCODE_RELEASE_BASE", "http://127.0.0.1:1/assets")
		old := selfupdate.Geteuid
		selfupdate.Geteuid = func() int { return 0 }
		t.Cleanup(func() { selfupdate.Geteuid = old })
		// With the override dropped, the dev-build refusal fires next -
		// not the explicit-target requirement the base would demand.
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "note: LIGHTCODE_RELEASE_BASE ignored when running as root") {
			t.Fatalf("stderr missing root note: %q", stderr)
		}
		if !strings.Contains(stderr, "this is a development build") {
			t.Fatalf("stderr = %q, want the dev refusal after the note", stderr)
		}
	})

	t.Run("below-floor target refused with the full installer command", func(t *testing.T) {
		setVersion(t, "v1.0.0", "")
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v0.0.2"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "downgrading v1.0.0 -> v0.0.2") {
			t.Fatalf("stderr missing downgrade notice: %q", stderr)
		}
		want := "lightcode: v0.0.2 predates the upgrade command; install it with the installer instead: curl -fsSL https://github.com/MMinasyan/lightcode/releases/latest/download/install.sh | LIGHTCODE_VERSION=v0.0.2 sh"
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want substring %q", stderr, want)
		}
		if strings.Contains(stderr, "...") {
			t.Fatalf("the installer command must never be elided: %q", stderr)
		}
		if !strings.Contains(stderr, "| LIGHTCODE_VERSION=v0.0.2 sh") {
			t.Fatalf("LIGHTCODE_VERSION must sit on the sh side of the pipe: %q", stderr)
		}
	})

	t.Run("dev current with explicit target prints the install notice", func(t *testing.T) {
		setVersion(t, "dev", "abc1234")
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v0.0.2"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "installing v0.0.2 over dev build") {
			t.Fatalf("stderr missing dev install notice: %q", stderr)
		}
	})
}

// upgradeFixture serves a release base directory with a fake binary whose
// smoke output reports tag.
func upgradeFixture(t *testing.T, tag string, corruptSums bool) (baseURL string, requests *int) {
	t.Helper()
	script := "#!/bin/sh\necho \"lightcode " + tag + " (linux/amd64)\"\necho SMOKE_NOISE_MARKER\n"
	return upgradeFixtureScript(t, script, corruptSums)
}

// upgradeFixtureScript is the same fixture with a caller-built fake binary.
func upgradeFixtureScript(t *testing.T, script string, corruptSums bool) (baseURL string, requests *int) {
	t.Helper()
	var tarBuf bytes.Buffer
	gz := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "lightcode", Mode: 0o755, Typeflag: tar.TypeReg, Size: int64(len(script))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tarball := tarBuf.Bytes()
	sum := sha256.Sum256(tarball)
	digest := hex.EncodeToString(sum[:])
	if corruptSums {
		digest = strings.Repeat("0", 64)
	}
	sums := digest + "  " + selfupdate.AssetName + "\n"

	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		switch r.URL.Path {
		case "/" + selfupdate.AssetName:
			_, _ = w.Write(tarball)
		case "/SHA256SUMS":
			_, _ = w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &count
}

// fakeInstall points ResolveExecutable at a temp install dir with a dummy
// target binary.
func fakeInstall(t *testing.T) (dir, target string) {
	t.Helper()
	dir = t.TempDir()
	target = filepath.Join(dir, "lightcode")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := selfupdate.ResolveExecutable
	selfupdate.ResolveExecutable = func() (string, string, error) { return target, dir, nil }
	t.Cleanup(func() { selfupdate.ResolveExecutable = old })
	return dir, target
}

func TestDispatchUpgradeInstallFlow(t *testing.T) {
	t.Run("full swap against a fixture base", func(t *testing.T) {
		setVersion(t, "v1.0.0", "")
		base, _ := upgradeFixture(t, "v9.9.9", false)
		t.Setenv("LIGHTCODE_RELEASE_BASE", base)
		dir, target := fakeInstall(t)

		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade", "v9.9.9"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout must stay empty on install, got %q", stdout)
		}
		if !strings.Contains(stderr, "upgraded v1.0.0 -> v9.9.9; takes effect the next time lightcode starts - including the GUI's own relaunch when switching projects.") {
			t.Fatalf("stderr missing final message: %q", stderr)
		}
		if strings.Contains(stdout+stderr, "SMOKE_NOISE_MARKER") {
			t.Fatal("smoke child output was forwarded")
		}
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "v9.9.9") {
			t.Fatalf("target not replaced: %q", data)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".lightcode.tmp.") {
				t.Fatalf("staged file left behind: %s", e.Name())
			}
		}
	})

	t.Run("checksum mismatch leaves the target untouched", func(t *testing.T) {
		setVersion(t, "v1.0.0", "")
		base, _ := upgradeFixture(t, "v9.9.9", true)
		t.Setenv("LIGHTCODE_RELEASE_BASE", base)
		_, target := fakeInstall(t)

		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v9.9.9"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "checksum mismatch") {
			t.Fatalf("stderr = %q", stderr)
		}
		data, _ := os.ReadFile(target)
		if string(data) != "old binary" {
			t.Fatalf("target touched on checksum mismatch: %q", data)
		}
	})

	t.Run("unwritable dir refuses before any download", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes everywhere")
		}
		setVersion(t, "v1.0.0", "")
		base, requests := upgradeFixture(t, "v9.9.9", false)
		t.Setenv("LIGHTCODE_RELEASE_BASE", base)
		dir, target := fakeInstall(t)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v9.9.9"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		want := "lightcode: binary is at " + target + " (not writable); re-run as: sudo lightcode upgrade"
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want substring %q", stderr, want)
		}
		if *requests != 0 {
			t.Fatalf("preflight refusal must make zero requests, made %d", *requests)
		}
	})

	t.Run("dev current with explicit target installs over dev build", func(t *testing.T) {
		setVersion(t, "dev", "abc1234")
		base, _ := upgradeFixture(t, "v9.9.9", false)
		t.Setenv("LIGHTCODE_RELEASE_BASE", base)
		_, target := fakeInstall(t)

		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade", "v9.9.9"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout must stay empty, got %q", stdout)
		}
		if !strings.Contains(stderr, "installing v9.9.9 over dev build") {
			t.Fatalf("stderr missing dev install notice: %q", stderr)
		}
		if !strings.Contains(stderr, "upgraded dev (abc1234) -> v9.9.9;") {
			t.Fatalf("stderr missing final message: %q", stderr)
		}
		data, _ := os.ReadFile(target)
		if !strings.Contains(string(data), "v9.9.9") {
			t.Fatalf("target not replaced: %q", data)
		}
	})

	t.Run("executable resolution failure refuses before any download", func(t *testing.T) {
		setVersion(t, "v1.0.0", "")
		base, requests := upgradeFixture(t, "v9.9.9", false)
		t.Setenv("LIGHTCODE_RELEASE_BASE", base)
		old := selfupdate.ResolveExecutable
		selfupdate.ResolveExecutable = func() (string, string, error) {
			return "", "", fmt.Errorf("cannot resolve executable path: boom")
		}
		t.Cleanup(func() { selfupdate.ResolveExecutable = old })

		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v9.9.9"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "lightcode: cannot resolve executable path:") {
			t.Fatalf("stderr = %q", stderr)
		}
		if *requests != 0 {
			t.Fatalf("resolution failure must make zero requests, made %d", *requests)
		}
	})

	t.Run("implicit latest below current never downgrades", func(t *testing.T) {
		setVersion(t, "v0.0.5", "")
		setLatestEndpoint(t, "v0.0.4")
		_, target := fakeInstall(t)

		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout must stay empty, got %q", stdout)
		}
		if !strings.Contains(stderr, "already up to date (v0.0.5)") {
			t.Fatalf("stderr = %q", stderr)
		}
		if strings.Contains(stderr, "downgrading") {
			t.Fatalf("bare upgrade must never downgrade: %q", stderr)
		}
		data, _ := os.ReadFile(target)
		if string(data) != "old binary" {
			t.Fatalf("target touched: %q", data)
		}
	})

	t.Run("equal below-floor target still hits the floor gate", func(t *testing.T) {
		setVersion(t, "v0.0.2", "")
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v0.0.2"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr: %s", code, stderr)
		}
		if strings.Contains(stderr, "already up to date") {
			t.Fatalf("equal below-floor target must not report up to date: %q", stderr)
		}
		if !strings.Contains(stderr, "v0.0.2 predates the upgrade command") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("smoke nonzero exit with matching tag removes the staged file", func(t *testing.T) {
		setVersion(t, "v1.0.0", "")
		script := "#!/bin/sh\necho \"lightcode v9.9.9 (linux/amd64)\"\nexit 3\n"
		base, _ := upgradeFixtureScript(t, script, false)
		t.Setenv("LIGHTCODE_RELEASE_BASE", base)
		dir, target := fakeInstall(t)

		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v9.9.9"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "failed --version") {
			t.Fatalf("stderr = %q", stderr)
		}
		data, _ := os.ReadFile(target)
		if string(data) != "old binary" {
			t.Fatalf("target touched: %q", data)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".lightcode.tmp.") {
				t.Fatalf("staged file left behind: %s", e.Name())
			}
		}
	})

	t.Run("hung smoke fixture removes the staged file", func(t *testing.T) {
		setVersion(t, "v1.0.0", "")
		script := "#!/bin/sh\nsleep 60\n"
		base, _ := upgradeFixtureScript(t, script, false)
		t.Setenv("LIGHTCODE_RELEASE_BASE", base)
		dir, target := fakeInstall(t)
		oldTimeout := selfupdate.SmokeTimeout
		selfupdate.SmokeTimeout = 200 * time.Millisecond
		t.Cleanup(func() { selfupdate.SmokeTimeout = oldTimeout })

		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v9.9.9"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "did not exit") {
			t.Fatalf("stderr = %q", stderr)
		}
		data, _ := os.ReadFile(target)
		if string(data) != "old binary" {
			t.Fatalf("target touched: %q", data)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".lightcode.tmp.") {
				t.Fatalf("staged file left behind after hang: %s", e.Name())
			}
		}
	})

	t.Run("smoke failure removes the staged file and keeps the target", func(t *testing.T) {
		setVersion(t, "v1.0.0", "")
		// The fixture binary reports v9.9.9; ask for v8.8.8 so the smoke
		// tag match fails after a verified download.
		base, _ := upgradeFixture(t, "v9.9.9", false)
		t.Setenv("LIGHTCODE_RELEASE_BASE", base)
		dir, target := fakeInstall(t)

		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v8.8.8"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "did not report v8.8.8") {
			t.Fatalf("stderr = %q", stderr)
		}
		if !strings.Contains(stderr, "install with the installer instead") {
			t.Fatalf("stderr missing installer pointer: %q", stderr)
		}
		data, _ := os.ReadFile(target)
		if string(data) != "old binary" {
			t.Fatalf("target touched on smoke failure: %q", data)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".lightcode.tmp.") {
				t.Fatalf("staged file left behind: %s", e.Name())
			}
		}
	})
}

// uninstallSeams pins the interactive seams to deterministic values and
// restores them after the test.
func uninstallSeams(t *testing.T, tty bool, input string) {
	t.Helper()
	oldTTY, oldIn := selfupdate.IsTerminal, selfupdate.PromptInput
	selfupdate.IsTerminal = func() bool { return tty }
	selfupdate.PromptInput = strings.NewReader(input)
	t.Cleanup(func() { selfupdate.IsTerminal, selfupdate.PromptInput = oldTTY, oldIn })
}

func TestDispatchUninstall(t *testing.T) {
	t.Run("dry run lists targets and removes nothing", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		_, target := fakeInstall(t)
		uninstallSeams(t, false, "") // works in a pipe without --yes

		_, code, stdout, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--dry-run"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		want := "would remove " + target + "\n" +
			"would remove " + dataDir + " (all data: API keys, sessions, snapshots)\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("dry run removed the binary")
		}
		if _, err := os.Stat(dataDir); err != nil {
			t.Fatal("dry run removed the data dir")
		}
	})

	t.Run("dry run works against an unwritable install", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes everywhere")
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, target := fakeInstall(t)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		uninstallSeams(t, false, "")

		_, code, stdout, stderr := capture(t, []string{"lightcode", "uninstall", "--dry-run"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if stdout != "would remove "+target+"\n" {
			t.Fatalf("stdout = %q", stdout)
		}
		if strings.Contains(stderr, "not writable") {
			t.Fatalf("dry run must short-circuit before the writability preflight: %q", stderr)
		}
	})

	t.Run("yes removes the binary and sweeps owned staged files only", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, target := fakeInstall(t)
		staged := filepath.Join(dir, ".lightcode.tmp.stale")
		if err := os.WriteFile(staged, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, ".lightcode.tmp.link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		uninstallSeams(t, false, "")

		_, code, stdout, stderr := capture(t, []string{"lightcode", "uninstall", "--yes"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout must be empty, got %q", stdout)
		}
		if stderr != "removed "+target+"\n" {
			t.Fatalf("stderr = %q", stderr)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("binary not removed")
		}
		if _, err := os.Stat(staged); !os.IsNotExist(err) {
			t.Fatal("owned staged file not swept")
		}
		if _, err := os.Lstat(link); err != nil {
			t.Fatal("symlinked staged entry must survive the sweep")
		}
	})

	t.Run("foreign-owned staged file survives the sweep", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, target := fakeInstall(t)
		staged := filepath.Join(dir, ".lightcode.tmp.foreign")
		if err := os.WriteFile(staged, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		oldOwner := selfupdate.FileOwner
		selfupdate.FileOwner = func(info os.FileInfo) (int, bool) { return os.Geteuid() + 1, true }
		t.Cleanup(func() { selfupdate.FileOwner = oldOwner })
		uninstallSeams(t, false, "")

		_, code, _, _ := capture(t, []string{"lightcode", "uninstall", "--yes"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		if _, err := os.Stat(staged); err != nil {
			t.Fatal("foreign-owned staged file must survive the sweep")
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("binary not removed")
		}
	})

	t.Run("purge removes data before the binary", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		if err := os.MkdirAll(filepath.Join(dataDir, "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, target := fakeInstall(t)
		uninstallSeams(t, false, "")

		_, code, stdout, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--yes"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if stdout != "" {
			t.Fatalf("stdout must be empty, got %q", stdout)
		}
		want := "removed " + dataDir + "\nremoved " + target + "\n"
		if stderr != want {
			t.Fatalf("stderr = %q, want %q (data line first)", stderr, want)
		}
		if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
			t.Fatal("data dir not removed")
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("binary not removed")
		}
	})

	t.Run("no tty and no yes refuses", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, target := fakeInstall(t)
		uninstallSeams(t, false, "")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "lightcode: uninstall is interactive; pass --yes to confirm non-interactively") {
			t.Fatalf("stderr = %q", stderr)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("refusal must remove nothing")
		}
	})

	t.Run("decline cancels with an unprefixed status", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, target := fakeInstall(t)
		uninstallSeams(t, true, "n\n")

		_, code, stdout, stderr := capture(t, []string{"lightcode", "uninstall"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if stdout != "" {
			t.Fatalf("stdout must be empty, got %q", stdout)
		}
		if !strings.Contains(stderr, "Remove "+target+"? [y/N] ") {
			t.Fatalf("stderr missing prompt: %q", stderr)
		}
		if !strings.Contains(stderr, "cancelled\n") {
			t.Fatalf("stderr missing cancelled status: %q", stderr)
		}
		if strings.Contains(stderr, "lightcode: ") {
			t.Fatalf("cancelled must not carry the error prefix: %q", stderr)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("decline must remove nothing")
		}
	})

	t.Run("empty answer defaults to no", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, target := fakeInstall(t)
		uninstallSeams(t, true, "\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall"})
		if code != 1 || !strings.Contains(stderr, "cancelled") {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("default answer must remove nothing")
		}
	})

	t.Run("accept via prompt removes", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		_, target := fakeInstall(t)
		uninstallSeams(t, true, "y\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "Remove "+target+" and ALL data in "+dataDir+" (API keys, sessions, snapshots)? [y/N] ") {
			t.Fatalf("stderr missing purge prompt: %q", stderr)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("binary not removed")
		}
	})

	t.Run("purge refuses a symlinked data dir", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		real := filepath.Join(home, "realdata")
		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(home, ".lightcode")); err != nil {
			t.Fatal(err)
		}
		_, target := fakeInstall(t)
		uninstallSeams(t, true, "y\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--yes"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "lightcode: ~/.lightcode is a symlink; refusing to purge through it") {
			t.Fatalf("stderr = %q", stderr)
		}
		if _, err := os.Stat(real); err != nil {
			t.Fatal("symlink target must be untouched")
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("binary must be untouched on purge refusal")
		}
	})

	t.Run("purge refuses a foreign-owned data dir", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		fakeInstall(t)
		oldOwner := selfupdate.FileOwner
		selfupdate.FileOwner = func(info os.FileInfo) (int, bool) { return os.Geteuid() + 1, true }
		t.Cleanup(func() { selfupdate.FileOwner = oldOwner })

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--yes"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "lightcode: ~/.lightcode is not owned by the current user; refusing to purge") {
			t.Fatalf("stderr = %q", stderr)
		}
		if _, err := os.Stat(dataDir); err != nil {
			t.Fatal("foreign-owned data dir must be untouched")
		}
	})

	t.Run("purge with absent data dir continues as plain uninstall", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, target := fakeInstall(t)
		uninstallSeams(t, true, "y\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "no data directory at "+filepath.Join(home, ".lightcode")+" - nothing to purge") {
			t.Fatalf("stderr missing nothing-to-purge note: %q", stderr)
		}
		if !strings.Contains(stderr, "Remove "+target+"? [y/N] ") {
			t.Fatalf("prompt must be the binary-only one: %q", stderr)
		}
		if strings.Contains(stderr, "ALL data") {
			t.Fatalf("purge prompt must not fire with nothing to purge: %q", stderr)
		}
		if strings.Contains(stderr, "data removed") {
			t.Fatalf("the partial-flow message must never fire with nothing purged: %q", stderr)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("binary not removed")
		}
	})

	t.Run("unwritable binary dir refuses before any prompt", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes everywhere")
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, target := fakeInstall(t)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		uninstallSeams(t, true, "y\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "lightcode: binary is at "+target+" (not writable); re-run as: sudo lightcode uninstall") {
			t.Fatalf("stderr = %q", stderr)
		}
		if strings.Contains(stderr, "[y/N]") {
			t.Fatalf("the preflight must refuse before any prompt: %q", stderr)
		}
	})

	t.Run("privileged partial flow purges data then points at sudo", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes everywhere")
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		dir, target := fakeInstall(t)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		uninstallSeams(t, true, "y\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "removed "+dataDir+"\n") {
			t.Fatalf("stderr missing data removal line: %q", stderr)
		}
		if !strings.Contains(stderr, "lightcode: data removed; binary still installed at "+target+" - run: sudo lightcode uninstall --yes") {
			t.Fatalf("stderr = %q", stderr)
		}
		if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
			t.Fatal("data dir must be purged in the partial flow")
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("binary must remain when its dir is unwritable")
		}
	})

	t.Run("binary-only flow works without HOME", func(t *testing.T) {
		t.Setenv("HOME", "")
		_, target := fakeInstall(t)
		uninstallSeams(t, false, "")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--yes"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
		}
		if strings.Contains(stderr, "resolve home dir") {
			t.Fatalf("the binary-only flow must not resolve the home dir: %q", stderr)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("binary not removed")
		}
	})

	t.Run("root purge refusal precedes home resolution", func(t *testing.T) {
		t.Setenv("HOME", "")
		fakeInstall(t)
		oldEuid := selfupdate.Geteuid
		selfupdate.Geteuid = func() int { return 0 }
		t.Cleanup(func() { selfupdate.Geteuid = oldEuid })

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--yes"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "refusing to purge user data as root") {
			t.Fatalf("stderr = %q", stderr)
		}
		if strings.Contains(stderr, "resolve home dir") {
			t.Fatalf("the root refusal must fire before any home lookup: %q", stderr)
		}
	})

	t.Run("root refuses to purge", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		fakeInstall(t)
		oldEuid := selfupdate.Geteuid
		selfupdate.Geteuid = func() int { return 0 }
		t.Cleanup(func() { selfupdate.Geteuid = oldEuid })

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--yes"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(stderr, "lightcode: refusing to purge user data as root; run 'lightcode uninstall --purge' as the owning user first, then remove the binary with sudo") {
			t.Fatalf("stderr = %q", stderr)
		}
		if _, err := os.Stat(dataDir); err != nil {
			t.Fatal("root purge refusal must remove nothing")
		}
	})
}

func TestDispatchCompletion(t *testing.T) {
	t.Run("registry drift guard across all shells", func(t *testing.T) {
		for _, shell := range []string{"bash", "zsh", "fish"} {
			_, code, stdout, stderr := capture(t, []string{"lightcode", "completion", shell})
			if code != 0 || stderr != "" {
				t.Fatalf("%s: exit=%d stderr=%q", shell, code, stderr)
			}
			// Every registry command, every registered flag name, and the
			// top-level aliases must appear; a new command or flag that
			// misses the generator fails here.
			for i := range commands {
				cmd := &commands[i]
				if !strings.Contains(stdout, cmd.name) {
					t.Fatalf("%s: missing command %q", shell, cmd.name)
				}
				for _, flagName := range registeredFlagNames(cmd) {
					if !strings.Contains(stdout, flagName) {
						t.Fatalf("%s: missing flag %q of %q", shell, flagName, cmd.name)
					}
				}
			}
			aliases := []string{"-h", "--help", "-v", "--version"}
			if shell == "fish" {
				// fish declares flags in its native -s/-l form.
				aliases = []string{"-s h", "-l help", "-s v", "-l version"}
			}
			for _, alias := range aliases {
				if !strings.Contains(stdout, alias) {
					t.Fatalf("%s: missing top-level alias %q", shell, alias)
				}
			}
		}
	})

	t.Run("per-command help flags for sampled commands", func(t *testing.T) {
		_, _, stdout, _ := capture(t, []string{"lightcode", "completion", "bash"})
		for _, name := range []string{"models", "doctor", "upgrade"} {
			_, after, found := strings.Cut(stdout, name+")")
			if !found {
				t.Fatalf("no case arm for %q", name)
			}
			arm, _, _ := strings.Cut(after, ";;")
			if !strings.Contains(arm, "-h") || !strings.Contains(arm, "--help") {
				t.Fatalf("%q arm missing injected help flags: %q", name, arm)
			}
		}
	})

	t.Run("shell enum arm is static", func(t *testing.T) {
		_, _, stdout, _ := capture(t, []string{"lightcode", "completion", "bash"})
		_, after, found := strings.Cut(stdout, "completion)")
		if !found {
			t.Fatal("no case arm for completion")
		}
		arm, _, _ := strings.Cut(after, ";;")
		for _, shell := range []string{"bash", "zsh", "fish"} {
			if !strings.Contains(arm, shell) {
				t.Fatalf("completion arm missing %q: %q", shell, arm)
			}
		}
	})

	t.Run("bare completion emits bash", func(t *testing.T) {
		_, code, stdout, _ := capture(t, []string{"lightcode", "completion"})
		if code != 0 || !strings.HasPrefix(stdout, "# bash completion for lightcode") {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
	})

	t.Run("unsupported shell exits 2", func(t *testing.T) {
		_, code, stdout, stderr := capture(t, []string{"lightcode", "completion", "powershell"})
		if code != 2 || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.Contains(stderr, `unsupported shell "powershell" (supported: bash, zsh, fish)`) {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("second positional exits 2", func(t *testing.T) {
		_, code, _, stderr := capture(t, []string{"lightcode", "completion", "bash", "zsh"})
		if code != 2 || !strings.Contains(stderr, `unexpected argument "zsh"`) {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
	})
}

func TestEscapeSingleQuoted(t *testing.T) {
	if got := escapeSingleQuoted("a'b"); got != `a'\''b` {
		t.Fatalf("escaped = %q", got)
	}
}

// modelsFixtureConfig defines providers covering every models state:
// modeltest is connected with a visible, a hidden, and an incomplete model;
// disc is keyed but never connected; allinc has only incomplete models.
const modelsFixtureConfig = `{
  "providers": {
    "modeltest": {
      "transport": {"base_url": "https://mt.example/v1", "api_key_env": "MODELS_TEST_KEY"},
      "discovery": false,
      "models": {
        "vis": {"context_window": 8192, "max_output_tokens": 1024},
        "hid": {"context_window": 16384, "hidden": true},
        "inc": {}
      }
    },
    "disc": {
      "transport": {"base_url": "https://d.example/v1", "api_key_env": "MODELS_DISC_KEY"},
      "discovery": false,
      "models": {"m": {"context_window": 4096}}
    },
    "allinc": {
      "transport": {"base_url": "https://ai.example/v1", "api_key_env": "MODELS_TEST_KEY"},
      "discovery": false,
      "models": {"m": {}}
    }
  },
  "default_model": "modeltest/vis"
}`

func modelsFixtureHome(t *testing.T, body string) string {
	return modelsFixtureHomeWithPrimary(t, body, "")
}

func modelsFixtureHomeWithPrimary(t *testing.T, body, primaryModel string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LIGHTCODE_CONFIG", "")
	dir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if primaryModel != "" {
		if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(`{"primary": {"model": "`+primaryModel+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestDispatchModels(t *testing.T) {
	t.Run("default shows visible complete connected models", func(t *testing.T) {
		modelsFixtureHomeWithPrimary(t, modelsFixtureConfig, "modeltest/vis")
		t.Setenv("MODELS_TEST_KEY", "x")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "models"})
		if code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		want := "modeltest/vis\t8192\t1024\tdefault\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("all includes hidden and incomplete with markers", func(t *testing.T) {
		modelsFixtureHomeWithPrimary(t, modelsFixtureConfig, "modeltest/vis")
		t.Setenv("MODELS_TEST_KEY", "x")
		_, code, stdout, _ := capture(t, []string{"lightcode", "models", "--all"})
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		want := "modeltest/hid\t16384\t0\thidden\n" +
			"modeltest/inc\t0\t0\tincomplete\n" +
			"modeltest/vis\t8192\t1024\tdefault\n"
		if stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("provider filter returns only that provider's rows", func(t *testing.T) {
		modelsFixtureHome(t, `{
  "providers": {
    "modeltest": {
      "transport": {"base_url": "https://mt.example/v1", "api_key_env": "MODELS_TEST_KEY"},
      "discovery": false,
      "models": {"vis": {"context_window": 8192, "max_output_tokens": 1024}}
    },
    "other": {
      "transport": {"base_url": "http://localhost:11434/v1", "api_key_env": ""},
      "discovery": false,
      "models": {"m": {"context_window": 4096}}
    }
  },
  "default_model": ""
}`)
		t.Setenv("MODELS_TEST_KEY", "x")
		// Both providers are connected; unfiltered output shows both.
		_, code, stdout, _ := capture(t, []string{"lightcode", "models"})
		if code != 0 || !strings.Contains(stdout, "modeltest/vis") || !strings.Contains(stdout, "other/m") {
			t.Fatalf("unfiltered exit=%d stdout=%q", code, stdout)
		}
		// The filter keeps exactly the named provider's rows.
		_, code, stdout, stderr := capture(t, []string{"lightcode", "models", "modeltest"})
		if code != 0 || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		if stdout != "modeltest/vis\t8192\t1024\t-\n" {
			t.Fatalf("filtered stdout = %q", stdout)
		}
	})

	t.Run("unknown provider exits 2 with known ids", func(t *testing.T) {
		modelsFixtureHome(t, modelsFixtureConfig)
		t.Setenv("MODELS_TEST_KEY", "x")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "models", "nosuch"})
		if code != 2 || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.Contains(stderr, `unknown provider "nosuch"`) || !strings.Contains(stderr, "modeltest") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("disconnected provider hints on stderr", func(t *testing.T) {
		modelsFixtureHome(t, modelsFixtureConfig)
		t.Setenv("MODELS_TEST_KEY", "x")
		for _, argv := range [][]string{
			{"lightcode", "models", "disc"},
			{"lightcode", "models", "disc", "--all"},
		} {
			_, code, stdout, stderr := capture(t, argv)
			if code != 0 || stdout != "" {
				t.Fatalf("%v: exit=%d stdout=%q", argv, code, stdout)
			}
			if !strings.Contains(stderr, `provider "disc" is not connected - connect it in Settings first`) {
				t.Fatalf("%v: stderr = %q", argv, stderr)
			}
		}
	})

	t.Run("all-incomplete provider counts as disconnected", func(t *testing.T) {
		modelsFixtureHome(t, modelsFixtureConfig)
		t.Setenv("MODELS_TEST_KEY", "x")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "models", "allinc"})
		if code != 0 || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.Contains(stderr, `provider "allinc" is not connected - connect it in Settings first`) {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("connected provider with nothing visible hints rerun with all", func(t *testing.T) {
		modelsFixtureHome(t, `{
  "providers": {
    "hiddenonly": {
      "transport": {"base_url": "https://h.example/v1", "api_key_env": "MODELS_TEST_KEY"},
      "discovery": false,
      "models": {"h": {"context_window": 8192, "hidden": true}, "hi": {"hidden": true}}
    }
  },
  "default_model": ""
}`)
		t.Setenv("MODELS_TEST_KEY", "x")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "models", "hiddenonly"})
		if code != 0 || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.Contains(stderr, `no visible models for "hiddenonly" - rerun with --all`) {
			t.Fatalf("stderr = %q", stderr)
		}

		// --all shows both, including the hidden,incomplete multi-marker.
		_, code, stdout, _ = capture(t, []string{"lightcode", "models", "hiddenonly", "--all"})
		if code != 0 {
			t.Fatalf("--all exit=%d", code)
		}
		want := "hiddenonly/h\t8192\t0\thidden\n" +
			"hiddenonly/hi\t0\t0\thidden,incomplete\n"
		if stdout != want {
			t.Fatalf("--all stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("global empty distinguishes connect-first from rerun-with-all", func(t *testing.T) {
		modelsFixtureHome(t, modelsFixtureConfig)
		// No key set anywhere: nothing connected.
		_, code, stdout, stderr := capture(t, []string{"lightcode", "models"})
		if code != 0 || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.Contains(stderr, "no models available - connect a provider in Settings first") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("global rerun-with-all when connected but nothing visible", func(t *testing.T) {
		modelsFixtureHome(t, `{
  "providers": {
    "hiddenonly": {
      "transport": {"base_url": "https://h.example/v1", "api_key_env": "MODELS_TEST_KEY"},
      "discovery": false,
      "models": {"h": {"context_window": 8192, "hidden": true}}
    }
  },
  "default_model": ""
}`)
		t.Setenv("MODELS_TEST_KEY", "x")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "models"})
		if code != 0 || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.Contains(stderr, "no visible models - rerun with --all") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("json is always valid and hints stay on stderr", func(t *testing.T) {
		modelsFixtureHome(t, modelsFixtureConfig)
		// Empty state: stdout must be [] and the hint on stderr.
		_, code, stdout, stderr := capture(t, []string{"lightcode", "models", "--json"})
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		if strings.TrimSpace(stdout) != "[]" {
			t.Fatalf("empty --json stdout = %q, want []", stdout)
		}
		if !strings.Contains(stderr, "no models available") {
			t.Fatalf("hint missing from stderr: %q", stderr)
		}
	})

	t.Run("json golden fields", func(t *testing.T) {
		modelsFixtureHomeWithPrimary(t, modelsFixtureConfig, "modeltest/vis")
		t.Setenv("MODELS_TEST_KEY", "x")
		_, code, stdout, _ := capture(t, []string{"lightcode", "models", "--json"})
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
			t.Fatalf("stdout not JSON: %v\n%q", err, stdout)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %v", rows)
		}
		row := rows[0]
		for field, want := range map[string]any{
			"ref":             "modeltest/vis",
			"provider":        "modeltest",
			"providerName":    "modeltest",
			"model":           "vis",
			"displayName":     "vis",
			"contextWindow":   float64(8192),
			"maxOutputTokens": float64(1024),
			"hidden":          false,
			"providerHidden":  false,
			"incomplete":      false,
			"default":         true,
			"source":          "user",
		} {
			if row[field] != want {
				t.Fatalf("row[%q] = %v, want %v", field, row[field], want)
			}
		}
		// cost is omitempty and the fixture sets none.
		if _, present := row["cost"]; present {
			t.Fatalf("cost must be omitted when unset: %v", row)
		}
	})

	t.Run("two positionals exit 2", func(t *testing.T) {
		_, code, _, stderr := capture(t, []string{"lightcode", "models", "p1", "p2"})
		if code != 2 || !strings.Contains(stderr, `unexpected argument "p2"`) {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
	})
}

// snapshotHome records every entry under root with size and mtime so a
// before/after comparison proves zero filesystem writes.
func snapshotHome(t *testing.T, root string) map[string]string {
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

func TestDispatchConfig(t *testing.T) {
	t.Run("path prints the resolved path even when absent", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "config", "--path"})
		if code != 0 || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		if stdout != filepath.Join(home, ".lightcode", "config.json")+"\n" {
			t.Fatalf("stdout = %q", stdout)
		}
	})

	t.Run("path honors LIGHTCODE_CONFIG", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "custom.json")
		t.Setenv("LIGHTCODE_CONFIG", custom)
		_, code, stdout, _ := capture(t, []string{"lightcode", "config", "--path"})
		if code != 0 || stdout != custom+"\n" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
	})

	t.Run("default is byte passthrough even for invalid json", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		body := "{not json at all"
		if err := os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".lightcode", "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, code, stdout, stderr := capture(t, []string{"lightcode", "config"})
		if code != 0 || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		if stdout != body {
			t.Fatalf("stdout = %q, want byte-exact %q", stdout, body)
		}
	})

	t.Run("env override notes the path on stderr", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "custom.json")
		if err := os.WriteFile(custom, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("LIGHTCODE_CONFIG", custom)
		_, code, stdout, stderr := capture(t, []string{"lightcode", "config"})
		if code != 0 || stdout != "{}" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.Contains(stderr, custom) {
			t.Fatalf("stderr missing the resolved path: %q", stderr)
		}
	})

	t.Run("missing file errors without creating it", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		path := filepath.Join(home, ".lightcode", "config.json")
		before := snapshotHome(t, home)
		_, code, stdout, stderr := capture(t, []string{"lightcode", "config"})
		if code != 1 || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.Contains(stderr, "lightcode: config not created yet (run lightcode once to initialize): "+path) {
			t.Fatalf("stderr = %q", stderr)
		}
		if after := snapshotHome(t, home); !reflect.DeepEqual(before, after) {
			t.Fatalf("config wrote to the filesystem:\nbefore: %v\nafter:  %v", before, after)
		}
	})

	t.Run("normal run performs zero writes", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		if err := os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".lightcode", "config.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotHome(t, home)
		_, code, stdout, _ := capture(t, []string{"lightcode", "config"})
		if code != 0 || stdout != "{}" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if after := snapshotHome(t, home); !reflect.DeepEqual(before, after) {
			t.Fatalf("config wrote to the filesystem:\nbefore: %v\nafter:  %v", before, after)
		}
	})

	t.Run("resolve failure exits 1", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("LIGHTCODE_CONFIG", "")
		_, code, _, stderr := capture(t, []string{"lightcode", "config", "--path"})
		if code != 1 || !strings.HasPrefix(stderr, "lightcode: ") {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
	})
}
