package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/selfupdate"
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
			"desktop", "cli", "serve", "acp", "help",
			"(default)",
			"Exit codes: 0 success, 1 failure, 2 usage error.",
		} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("%v: stdout missing %q:\n%s", argv, want, stdout)
			}
		}
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
	for _, name := range []string{"desktop", "cli", "serve", "acp", "doctor", "help"} {
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
