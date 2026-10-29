package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

	"github.com/MMinasyan/lightcode/internal/selfupdate"
	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.Require(t, !gui, "unknown command must not reach the GUI path")
	testassert.Equal(t, code, 2)
	testassert.Equal(t, stdout, "")
	testassert.Require(t, strings.Contains(stderr, `lightcode: unknown command "frobnicate"`) && strings.Contains(stderr, "Run 'lightcode help' for usage."), "stderr missing unknown-command message: %q; stderr missing usage hint: %q", stderr, stderr)
}

func TestDispatchUnknownCommandEscapesControlBytes(t *testing.T) {
	_, code, _, stderr := capture(t, []string{"lightcode", "\x1b]0;evil\x07"})
	testassert.Equal(t, code, 2)
	testassert.Require(t, !strings.ContainsAny(stderr, "\x1b\x07") && strings.Contains(stderr, `\x1b`), "raw control bytes reached stderr: %q; escaped form missing from stderr: %q", stderr, stderr)
}

func TestDispatchHelpForms(t *testing.T) {
	for _, argv := range [][]string{
		{"lightcode", "help"},
		{"lightcode", "-h"},
		{"lightcode", "--help"},
	} {
		gui, code, stdout, stderr := capture(t, argv)
		testassert.Require(t, !gui && code == 0, "%v: gui=%v code=%d, want handled exit 0", argv, gui, code)
		testassert.Require(t, stderr == "", "%v: stderr must be empty, got %q", argv, stderr)
		for _, want := range []string{
			"Usage: lightcode [command]",
			"desktop", "cli", "acp", "runtime", "help",
			"(default)",
			"Exit codes: 0 success, 1 failure, 2 usage error; the terminal client exits 130 when a signal terminates it.",
		} {
			testassert.Require(t, strings.Contains(stdout, want), "%v: stdout missing %q:\n%s", argv, want, stdout)
		}
	}
}

func TestDispatchHelpForCommand(t *testing.T) {
	_, code, stdout, stderr := capture(t, []string{"lightcode", "help", "doctor"})
	testassert.Equal(t, code, 0)
	testassert.Equal(t, stderr, "")
	testassert.Require(t, strings.Contains(stdout, "Usage: lightcode doctor [flags]") && strings.Contains(stdout, "-json"), "stdout missing doctor usage: %q; stdout missing -json flag: %q", stdout, stdout)
}

func TestDispatchHelpUnknownCommand(t *testing.T) {
	_, code, stdout, stderr := capture(t, []string{"lightcode", "help", "nosuch"})
	testassert.Equal(t, code, 2)
	testassert.Equal(t, stdout, "")
	testassert.Require(t, strings.Contains(stderr, `lightcode: unknown command "nosuch"`), "stderr missing message: %q", stderr)
}

func TestDispatchHelpExtraPositional(t *testing.T) {
	_, code, _, stderr := capture(t, []string{"lightcode", "help", "a", "b"})
	testassert.Equal(t, code, 2)
	testassert.Require(t, strings.Contains(stderr, "Usage: lightcode help [command]"), "stderr missing help usage: %q", stderr)
}

func TestDispatchBareReachesGUIPath(t *testing.T) {
	gui, code, stdout, stderr := capture(t, []string{"lightcode"})
	testassert.Require(t, gui && code == 0, "gui=%v code=%d, want GUI path", gui, code)
	testassert.Require(t, stdout == "" && stderr == "", "bare invocation must print nothing, got out=%q err=%q", stdout, stderr)
}

func TestDispatchDesktopSameAsBare(t *testing.T) {
	gui, code, stdout, stderr := capture(t, []string{"lightcode", "desktop"})
	testassert.Require(t, gui && code == 0, "gui=%v code=%d, want GUI path", gui, code)
	testassert.Require(t, stdout == "" && stderr == "", "desktop must print nothing, got out=%q err=%q", stdout, stderr)
}

func TestDispatchPerCommandHelpFlag(t *testing.T) {
	for _, name := range []string{"desktop", "cli", "acp", "runtime", "doctor", "completion", "models", "config", "help"} {
		_, code, stdout, stderr := capture(t, []string{"lightcode", name, "-h"})
		testassert.Require(t, code == 0, "%s -h: exit code = %d, want 0", name, code)
		testassert.Require(t, stderr == "", "%s -h: stderr must be empty, got %q", name, stderr)
		testassert.Require(t, strings.Contains(stdout, "Usage: lightcode "+name), "%s -h: stdout missing usage: %q", name, stdout)
	}
}

func TestDispatchBadFlagPrintsUsageOnce(t *testing.T) {
	_, code, stdout, stderr := capture(t, []string{"lightcode", "doctor", "-json=abc"})
	testassert.Equal(t, code, 2)
	testassert.Equal(t, stdout, "")
	testassert.Require(t, strings.HasPrefix(stderr, "lightcode: "), "stderr missing error prefix: %q", stderr)
	{
		n := strings.Count(stderr, "Usage: lightcode doctor")
		testassert.Require(t, n == 1, "usage printed %d times, want exactly once:\n%s", n, stderr)
	}
}

func TestDispatchUnexpectedPositionals(t *testing.T) {
	for _, argv := range [][]string{
		{"lightcode", "cli", "extra"},
		{"lightcode", "acp", "extra"},
		{"lightcode", "desktop", "extra"},
		{"lightcode", "runtime", "extra"},
		{"lightcode", "doctor", "extra"},
		{"lightcode", "uninstall", "extra"},
		{"lightcode", "config", "extra"},
	} {
		gui, code, stdout, stderr := capture(t, argv)
		testassert.Require(t, !gui, "%v: must not reach the GUI path", argv)
		testassert.Require(t, code == 2, "%v: exit code = %d, want 2", argv, code)
		testassert.Require(t, stdout == "", "%v: stdout must be empty, got %q", argv, stdout)
		testassert.Require(t, strings.Contains(stderr, `unexpected argument "extra"`), "%v: stderr missing message: %q", argv, stderr)
	}
}

func TestDispatchVersion(t *testing.T) {
	setVersion(t, "v9.9.9", "abcdef1")

	for _, argv := range [][]string{
		{"lightcode", "version"},
		{"lightcode", "-v"},
		{"lightcode", "--version"},
	} {
		_, code, stdout, stderr := capture(t, argv)
		testassert.Require(t, code == 0, "%v: exit code = %d, want 0", argv, code)
		testassert.Require(t, stderr == "", "%v: stderr must be empty, got %q", argv, stderr)
		want := "lightcode v9.9.9 (" + runtime.GOOS + "/" + runtime.GOARCH + ")\n"
		testassert.Require(t, stdout == want, "%v: stdout = %q, want %q", argv, stdout, want)
	}
}

func TestDispatchVersionJSON(t *testing.T) {
	setVersion(t, "v9.9.9", "abcdef1")

	_, code, stdout, stderr := capture(t, []string{"lightcode", "version", "--json"})
	testassert.Require(t, code == 0 && stderr == "", "exit=%d stderr=%q, want clean exit 0", code, stderr)
	want := `{"version":"v9.9.9","commit":"abcdef1","os":"` + runtime.GOOS + `","arch":"` + runtime.GOARCH + `"}` + "\n"
	testassert.Equal(t, stdout, want)
}

func TestDispatchVersionExtraArgs(t *testing.T) {
	for _, argv := range [][]string{
		{"lightcode", "version", "extra"},
		{"lightcode", "-v", "extra"},
	} {
		_, code, stdout, stderr := capture(t, argv)
		testassert.Require(t, code == 2, "%v: exit code = %d, want 2", argv, code)
		testassert.Require(t, stdout == "", "%v: stdout must be empty, got %q", argv, stdout)
		testassert.Require(t, strings.Contains(stderr, `unexpected argument "extra"`), "%v: stderr missing message: %q", argv, stderr)
	}
}

func TestDispatchDoctor(t *testing.T) {
	t.Run("fresh home exits 0 with the report on stdout", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "doctor"})
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		testassert.Equal(t, stderr, "")
		testassert.Require(t, strings.Contains(stdout, "doctor: ") && strings.Contains(stdout, "no providers connected yet - this is expected on a new install") && !strings.Contains(stdout, "fail "), "stdout missing summary line: %q; stdout missing fresh-install appendix: %q; fresh home must not FAIL: %q", stdout, stdout, stdout)
		{
			_, err := os.Stat(filepath.Join(home, ".lightcode"))
			testassert.Require(t, os.IsNotExist(err), "doctor created the data dir")
		}
	})

	t.Run("json emits the report shape on stdout", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "doctor", "--json"})
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		testassert.Equal(t, stderr, "")
		var decoded struct {
			Checks []map[string]any `json:"checks"`
			OK     *int             `json:"ok"`
			Warn   *int             `json:"warnings"`
			Fail   *int             `json:"failures"`
		}
		{
			err := json.Unmarshal([]byte(stdout), &decoded)
			testassert.Require(t, err == nil, "stdout is not valid JSON: %v\n%q", err, stdout)
		}
		testassert.Require(t, decoded.OK != nil && decoded.Warn != nil && decoded.Fail != nil && len(decoded.Checks) != 0, "missing top-level fields: %s", stdout)
		for _, field := range []string{"group", "name", "status", "detail"} {
			{
				_, ok := decoded.Checks[0][field]
				testassert.Require(t, ok, "check missing %q: %v", field, decoded.Checks[0])
			}
		}
	})

	t.Run("invalid config exits 1", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		testassert.NoError(t, os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(home, ".lightcode", "config.json"), []byte("{bad json"), 0o600))
		_, code, stdout, stderr := capture(t, []string{"lightcode", "doctor"})
		testassert.Require(t, code == 1, "exit code = %d, want 1\nstdout: %s", code, stdout)
		testassert.Require(t, strings.Contains(stdout, "fail data/config:"), "stdout missing config FAIL: %q", stdout)
		testassert.Require(t, strings.HasPrefix(stderr, "lightcode: "), "stderr missing error prefix: %q", stderr)
	})

	t.Run("unresolvable home exits 1", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("LIGHTCODE_CONFIG", "")
		_, code, stdout, _ := capture(t, []string{"lightcode", "doctor"})
		testassert.Require(t, code == 1, "exit code = %d, want 1\nstdout: %s", code, stdout)
		testassert.Require(t, strings.Contains(stdout, "fail install/"), "stdout missing install-group FAIL: %q", stdout)
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
		testassert.Require(t, code == 2, "%v: exit code = %d, want 2\nstderr: %s", tc.argv, code, stderr)
		testassert.Require(t, stdout == "", "%v: stdout must be empty, got %q", tc.argv, stdout)
		testassert.Require(t, strings.Contains(stderr, "lightcode: "+tc.want), "%v: stderr = %q, want substring %q", tc.argv, stderr, tc.want)
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
		testassert.Require(t, code == 0 && stderr == "", "exit=%d stderr=%q", code, stderr)
		want := "current: v0.0.3, latest: v0.0.4 (update available; run: lightcode upgrade)\n"
		testassert.Equal(t, stdout, want)
		entries, _ := os.ReadDir(home)
		testassert.Require(t, len(entries) == 0, "--check wrote files: %v", entries)
	})

	t.Run("up to date", func(t *testing.T) {
		setVersion(t, "v0.0.4", "")
		setLatestEndpoint(t, "v0.0.4")
		_, code, stdout, _ := capture(t, []string{"lightcode", "upgrade", "--check"})
		testassert.Require(t, code == 0 && stdout == "current: v0.0.4, latest: v0.0.4 (up to date)\n", "exit=%d stdout=%q", code, stdout)
	})

	t.Run("dev build", func(t *testing.T) {
		setVersion(t, "dev", "abc1234")
		setLatestEndpoint(t, "v0.0.4")
		_, code, stdout, _ := capture(t, []string{"lightcode", "upgrade", "--check"})
		testassert.Require(t, code == 0 && stdout == "current: dev (abc1234), latest: v0.0.4\n", "exit=%d stdout=%q", code, stdout)
	})

	t.Run("below floor wins regardless of current", func(t *testing.T) {
		setVersion(t, "v0.0.3", "")
		setLatestEndpoint(t, "v0.0.2")
		_, code, stdout, _ := capture(t, []string{"lightcode", "upgrade", "--check"})
		testassert.Require(t, code == 0 && stdout == "current: v0.0.3, latest: v0.0.2 (not selfupdate-capable; use the installer)\n", "exit=%d stdout=%q", code, stdout)
	})

	t.Run("ordering beyond machine integers still decides", func(t *testing.T) {
		setVersion(t, "v1.0.0", "")
		setLatestEndpoint(t, "v18446744073709551615.0.0")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade", "--check"})
		testassert.Require(t, code == 0 && stderr == "", "exit=%d stderr=%q", code, stderr)
		want := "current: v1.0.0, latest: v18446744073709551615.0.0 (update available; run: lightcode upgrade)\n"
		testassert.Equal(t, stdout, want)
	})

	t.Run("json golden", func(t *testing.T) {
		setVersion(t, "v0.0.3", "abc1234")
		setLatestEndpoint(t, "v0.0.4")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade", "--check", "--json"})
		testassert.Require(t, code == 0 && stderr == "", "exit=%d stderr=%q", code, stderr)
		want := `{"current":"v0.0.3","latest":"v0.0.4","status":"update-available","updateAvailable":true,"installable":true}` + "\n"
		testassert.Equal(t, stdout, want)
	})

	t.Run("refuses under release base regardless of euid", func(t *testing.T) {
		t.Setenv("LIGHTCODE_RELEASE_BASE", "http://127.0.0.1:1/assets")
		old := selfupdate.Geteuid
		selfupdate.Geteuid = func() int { return 0 }
		t.Cleanup(func() { selfupdate.Geteuid = old })
		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade", "--check"})
		testassert.Require(t, code == 2 && stdout == "", "exit=%d stdout=%q", code, stdout)
		testassert.Require(t, strings.Contains(stderr, "lightcode: LIGHTCODE_RELEASE_BASE is set; --check is meaningless against a directory base"), "stderr = %q", stderr)
	})

	t.Run("resolution failure exits 1", func(t *testing.T) {
		setVersion(t, "v0.0.3", "")
		old := selfupdate.LatestEndpoint
		selfupdate.LatestEndpoint = "http://127.0.0.1:1/latest"
		t.Cleanup(func() { selfupdate.LatestEndpoint = old })
		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade", "--check"})
		testassert.Require(t, code == 1 && stdout == "", "exit=%d stdout=%q", code, stdout)
		testassert.Require(t, strings.HasPrefix(stderr, "lightcode: "), "stderr = %q", stderr)
	})
}

func TestDispatchUpgradeGates(t *testing.T) {
	t.Setenv("LIGHTCODE_RELEASE_BASE", "")

	t.Run("dev build refuses without explicit target", func(t *testing.T) {
		setVersion(t, "dev", "abc1234")
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade"})
		testassert.Equal(t, code, 1)
		want := "lightcode: this is a development build (dev (abc1234)); pass an explicit version to upgrade anyway: lightcode upgrade v0.0.3"
		testassert.Require(t, strings.Contains(stderr, want), "stderr = %q, want substring %q", stderr, want)
	})

	t.Run("release base requires explicit target", func(t *testing.T) {
		setVersion(t, "v0.0.3", "")
		t.Setenv("LIGHTCODE_RELEASE_BASE", "http://127.0.0.1:1/assets")
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade"})
		testassert.Equal(t, code, 2)
		testassert.Require(t, strings.Contains(stderr, "lightcode: LIGHTCODE_RELEASE_BASE is set; pass an explicit version"), "stderr = %q", stderr)
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
		testassert.Equal(t, code, 1)
		testassert.Require(t, strings.Contains(stderr, "note: LIGHTCODE_RELEASE_BASE ignored when running as root") && strings.Contains(stderr, "this is a development build"), "stderr missing root note: %q; stderr = %q, want the dev refusal after the note", stderr, stderr)
	})

	t.Run("below-floor target refused with the full installer command", func(t *testing.T) {
		setVersion(t, "v1.0.0", "")
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v0.0.2"})
		testassert.Equal(t, code, 1)
		testassert.Require(t, strings.Contains(stderr, "downgrading v1.0.0 -> v0.0.2"), "stderr missing downgrade notice: %q", stderr)
		want := "lightcode: v0.0.2 predates the upgrade command; install it with the installer instead: curl -fsSL https://github.com/MMinasyan/lightcode/releases/latest/download/install.sh | LIGHTCODE_VERSION=v0.0.2 sh"
		testassert.Require(t, strings.Contains(stderr, want) && !strings.Contains(stderr, "...") && strings.Contains(stderr, "| LIGHTCODE_VERSION=v0.0.2 sh"), "stderr = %q, want substring %q; the installer command must never be elided: %q; LIGHTCODE_VERSION must sit on the sh side of the pipe: %q", stderr, want, stderr, stderr)
	})

	t.Run("dev current with explicit target prints the install notice", func(t *testing.T) {
		setVersion(t, "dev", "abc1234")
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v0.0.2"})
		testassert.Equal(t, code, 1)
		testassert.Require(t, strings.Contains(stderr, "installing v0.0.2 over dev build"), "stderr missing dev install notice: %q", stderr)
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
	testassert.NoError(t, tw.WriteHeader(&tar.Header{Name: "lightcode", Mode: 0o755, Typeflag: tar.TypeReg, Size: int64(len(script))}))
	{
		_, err := tw.Write([]byte(script))
		testassert.NoError(t, err)
	}
	testassert.NoError(t, tw.Close())
	testassert.NoError(t, gz.Close())
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
	testassert.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))
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
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		testassert.Equal(t, stdout, "")
		testassert.Require(t, strings.Contains(stderr, "upgraded v1.0.0 -> v9.9.9; restart the Runtime to apply backend changes.") && !strings.Contains(stderr, "including the GUI's own relaunch when switching projects"), "stderr missing final message: %q; stderr must not promise a GUI relaunch: %q", stderr, stderr)
		testassert.Require(t, !strings.Contains(stdout+stderr, "SMOKE_NOISE_MARKER"), "smoke child output was forwarded")
		data, err := os.ReadFile(target)
		testassert.NoError(t, err)
		testassert.Require(t, strings.Contains(string(data), "v9.9.9"), "target not replaced: %q", data)
		entries, err := os.ReadDir(dir)
		testassert.NoError(t, err)
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
		testassert.Require(t, code == 1, "exit code = %d, want 1\nstderr: %s", code, stderr)
		testassert.Require(t, strings.Contains(stderr, "checksum mismatch"), "stderr = %q", stderr)
		data, _ := os.ReadFile(target)
		testassert.Require(t, string(data) == "old binary", "target touched on checksum mismatch: %q", data)
	})

	t.Run("unwritable dir refuses before any download", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes everywhere")
		}
		setVersion(t, "v1.0.0", "")
		base, requests := upgradeFixture(t, "v9.9.9", false)
		t.Setenv("LIGHTCODE_RELEASE_BASE", base)
		dir, target := fakeInstall(t)
		testassert.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v9.9.9"})
		testassert.Equal(t, code, 1)
		want := "lightcode: binary is at " + target + " (not writable); re-run as: sudo lightcode upgrade"
		testassert.Require(t, strings.Contains(stderr, want), "stderr = %q, want substring %q", stderr, want)
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
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		testassert.Equal(t, stdout, "")
		testassert.Require(t, strings.Contains(stderr, "installing v9.9.9 over dev build") && strings.Contains(stderr, "upgraded dev (abc1234) -> v9.9.9;"), "stderr missing dev install notice: %q; stderr missing final message: %q", stderr, stderr)
		data, _ := os.ReadFile(target)
		testassert.Require(t, strings.Contains(string(data), "v9.9.9"), "target not replaced: %q", data)
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
		testassert.Equal(t, code, 1)
		testassert.Require(t, strings.Contains(stderr, "lightcode: cannot resolve executable path:"), "stderr = %q", stderr)
		if *requests != 0 {
			t.Fatalf("resolution failure must make zero requests, made %d", *requests)
		}
	})

	t.Run("implicit latest below current never downgrades", func(t *testing.T) {
		setVersion(t, "v0.0.5", "")
		setLatestEndpoint(t, "v0.0.4")
		_, target := fakeInstall(t)

		_, code, stdout, stderr := capture(t, []string{"lightcode", "upgrade"})
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		testassert.Equal(t, stdout, "")
		testassert.Require(t, strings.Contains(stderr, "already up to date (v0.0.5)") && !strings.Contains(stderr, "downgrading"), "stderr = %q; bare upgrade must never downgrade: %q", stderr, stderr)
		data, _ := os.ReadFile(target)
		testassert.Require(t, string(data) == "old binary", "target touched: %q", data)
	})

	t.Run("equal below-floor target still hits the floor gate", func(t *testing.T) {
		setVersion(t, "v0.0.2", "")
		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v0.0.2"})
		testassert.Require(t, code == 1, "exit code = %d, want 1\nstderr: %s", code, stderr)
		testassert.Require(t, !strings.Contains(stderr, "already up to date") && strings.Contains(stderr, "v0.0.2 predates the upgrade command"), "equal below-floor target must not report up to date: %q; stderr = %q", stderr, stderr)
	})

	t.Run("smoke nonzero exit with matching tag removes the staged file", func(t *testing.T) {
		setVersion(t, "v1.0.0", "")
		script := "#!/bin/sh\necho \"lightcode v9.9.9 (linux/amd64)\"\nexit 3\n"
		base, _ := upgradeFixtureScript(t, script, false)
		t.Setenv("LIGHTCODE_RELEASE_BASE", base)
		dir, target := fakeInstall(t)

		_, code, _, stderr := capture(t, []string{"lightcode", "upgrade", "v9.9.9"})
		testassert.Require(t, code == 1, "exit code = %d, want 1\nstderr: %s", code, stderr)
		testassert.Require(t, strings.Contains(stderr, "failed --version"), "stderr = %q", stderr)
		data, _ := os.ReadFile(target)
		testassert.Require(t, string(data) == "old binary", "target touched: %q", data)
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
		testassert.Require(t, code == 1, "exit code = %d, want 1\nstderr: %s", code, stderr)
		testassert.Require(t, strings.Contains(stderr, "did not exit"), "stderr = %q", stderr)
		data, _ := os.ReadFile(target)
		testassert.Require(t, string(data) == "old binary", "target touched: %q", data)
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
		testassert.Require(t, code == 1, "exit code = %d, want 1\nstderr: %s", code, stderr)
		testassert.Require(t, strings.Contains(stderr, "did not report v8.8.8") && strings.Contains(stderr, "install with the installer instead"), "stderr = %q; stderr missing installer pointer: %q", stderr, stderr)
		data, _ := os.ReadFile(target)
		testassert.Require(t, string(data) == "old binary", "target touched on smoke failure: %q", data)
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
		testassert.NoError(t, os.MkdirAll(dataDir, 0o700))
		_, target := fakeInstall(t)
		uninstallSeams(t, false, "") // works in a pipe without --yes

		_, code, stdout, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--dry-run"})
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		want := "would remove " + target + "\n" +
			"would remove " + dataDir + " (all data: API keys, sessions, snapshots)\n"
		testassert.Equal(t, stdout, want)
		{
			_, err := os.Stat(target)
			testassert.Require(t, err == nil, "dry run removed the binary")
		}
		{
			_, err := os.Stat(dataDir)
			testassert.Require(t, err == nil, "dry run removed the data dir")
		}
	})

	t.Run("dry run works against an unwritable install", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes everywhere")
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, target := fakeInstall(t)
		testassert.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		uninstallSeams(t, false, "")

		_, code, stdout, stderr := capture(t, []string{"lightcode", "uninstall", "--dry-run"})
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		testassert.Equal(t, stdout, "would remove "+target+"\n")
		testassert.Require(t, !strings.Contains(stderr, "not writable"), "dry run must short-circuit before the writability preflight: %q", stderr)
	})

	t.Run("yes removes the binary and sweeps owned staged files only", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, target := fakeInstall(t)
		staged := filepath.Join(dir, ".lightcode.tmp.stale")
		testassert.NoError(t, os.WriteFile(staged, []byte("x"), 0o755))
		link := filepath.Join(dir, ".lightcode.tmp.link")
		testassert.NoError(t, os.Symlink(target, link))
		uninstallSeams(t, false, "")

		_, code, stdout, stderr := capture(t, []string{"lightcode", "uninstall", "--yes"})
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		testassert.Equal(t, stdout, "")
		testassert.Equal(t, stderr, "removed "+target+"\n")
		{
			_, err := os.Stat(target)
			testassert.Require(t, os.IsNotExist(err), "binary not removed")
		}
		{
			_, err := os.Stat(staged)
			testassert.Require(t, os.IsNotExist(err), "owned staged file not swept")
		}
		{
			_, err := os.Lstat(link)
			testassert.Require(t, err == nil, "symlinked staged entry must survive the sweep")
		}
	})

	t.Run("foreign-owned staged file survives the sweep", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, target := fakeInstall(t)
		staged := filepath.Join(dir, ".lightcode.tmp.foreign")
		testassert.NoError(t, os.WriteFile(staged, []byte("x"), 0o755))
		oldOwner := selfupdate.FileOwner
		selfupdate.FileOwner = func(info os.FileInfo) (int, bool) { return os.Geteuid() + 1, true }
		t.Cleanup(func() { selfupdate.FileOwner = oldOwner })
		uninstallSeams(t, false, "")

		_, code, _, _ := capture(t, []string{"lightcode", "uninstall", "--yes"})
		testassert.Equal(t, code, 0)
		{
			_, err := os.Stat(staged)
			testassert.Require(t, err == nil, "foreign-owned staged file must survive the sweep")
		}
		{
			_, err := os.Stat(target)
			testassert.Require(t, os.IsNotExist(err), "binary not removed")
		}
	})

	t.Run("purge removes data before the binary", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		testassert.NoError(t, os.MkdirAll(filepath.Join(dataDir, "sessions"), 0o700))
		_, target := fakeInstall(t)
		uninstallSeams(t, false, "")

		_, code, stdout, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--yes"})
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		testassert.Equal(t, stdout, "")
		want := "removed " + dataDir + "\nremoved " + target + "\n"
		testassert.Equal(t, stderr, want)
		{
			_, err := os.Stat(dataDir)
			testassert.Require(t, os.IsNotExist(err), "data dir not removed")
		}
		{
			_, err := os.Stat(target)
			testassert.Require(t, os.IsNotExist(err), "binary not removed")
		}
	})

	t.Run("no tty and no yes refuses", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, target := fakeInstall(t)
		uninstallSeams(t, false, "")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall"})
		testassert.Equal(t, code, 1)
		testassert.Require(t, strings.Contains(stderr, "lightcode: uninstall is interactive; pass --yes to confirm non-interactively"), "stderr = %q", stderr)
		{
			_, err := os.Stat(target)
			testassert.Require(t, err == nil, "refusal must remove nothing")
		}
	})

	t.Run("decline cancels with an unprefixed status", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, target := fakeInstall(t)
		uninstallSeams(t, true, "n\n")

		_, code, stdout, stderr := capture(t, []string{"lightcode", "uninstall"})
		testassert.Equal(t, code, 1)
		testassert.Equal(t, stdout, "")
		testassert.Require(t, strings.Contains(stderr, "Remove "+target+"? [y/N] ") && strings.Contains(stderr, "cancelled\n") && !strings.Contains(stderr, "lightcode: "), "stderr missing prompt: %q; stderr missing cancelled status: %q; cancelled must not carry the error prefix: %q", stderr, stderr, stderr)
		{
			_, err := os.Stat(target)
			testassert.Require(t, err == nil, "decline must remove nothing")
		}
	})

	t.Run("empty answer defaults to no", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, target := fakeInstall(t)
		uninstallSeams(t, true, "\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall"})
		testassert.Require(t, code == 1 && strings.Contains(stderr, "cancelled"), "exit=%d stderr=%q", code, stderr)
		{
			_, err := os.Stat(target)
			testassert.Require(t, err == nil, "default answer must remove nothing")
		}
	})

	t.Run("accept via prompt removes", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		testassert.NoError(t, os.MkdirAll(dataDir, 0o700))
		_, target := fakeInstall(t)
		uninstallSeams(t, true, "y\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge"})
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		testassert.Require(t, strings.Contains(stderr, "Remove "+target+" and ALL data in "+dataDir+" (API keys, sessions, snapshots)? [y/N] "), "stderr missing purge prompt: %q", stderr)
		{
			_, err := os.Stat(target)
			testassert.Require(t, os.IsNotExist(err), "binary not removed")
		}
	})

	t.Run("purge refuses a symlinked data dir", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		real := filepath.Join(home, "realdata")
		testassert.NoError(t, os.MkdirAll(real, 0o700))
		testassert.NoError(t, os.Symlink(real, filepath.Join(home, ".lightcode")))
		_, target := fakeInstall(t)
		uninstallSeams(t, true, "y\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--yes"})
		testassert.Equal(t, code, 1)
		testassert.Require(t, strings.Contains(stderr, "lightcode: ~/.lightcode is a symlink; refusing to purge through it"), "stderr = %q", stderr)
		{
			_, err := os.Stat(real)
			testassert.Require(t, err == nil, "symlink target must be untouched")
		}
		{
			_, err := os.Stat(target)
			testassert.Require(t, err == nil, "binary must be untouched on purge refusal")
		}
	})

	t.Run("purge refuses a foreign-owned data dir", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		testassert.NoError(t, os.MkdirAll(dataDir, 0o700))
		fakeInstall(t)
		oldOwner := selfupdate.FileOwner
		selfupdate.FileOwner = func(info os.FileInfo) (int, bool) { return os.Geteuid() + 1, true }
		t.Cleanup(func() { selfupdate.FileOwner = oldOwner })

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--yes"})
		testassert.Equal(t, code, 1)
		testassert.Require(t, strings.Contains(stderr, "lightcode: ~/.lightcode is not owned by the current user; refusing to purge"), "stderr = %q", stderr)
		{
			_, err := os.Stat(dataDir)
			testassert.Require(t, err == nil, "foreign-owned data dir must be untouched")
		}
	})

	t.Run("purge with absent data dir continues as plain uninstall", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, target := fakeInstall(t)
		uninstallSeams(t, true, "y\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge"})
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		testassert.Require(t, strings.Contains(stderr, "no data directory at "+filepath.Join(home, ".lightcode")+" - nothing to purge") && strings.Contains(stderr, "Remove "+target+"? [y/N] ") && !strings.Contains(stderr, "ALL data") && !strings.Contains(stderr, "data removed"), "stderr missing nothing-to-purge note: %q; prompt must be the binary-only one: %q; purge prompt must not fire with nothing to purge: %q; the partial-flow message must never fire with nothing purged: %q", stderr, stderr, stderr, stderr)
		{
			_, err := os.Stat(target)
			testassert.Require(t, os.IsNotExist(err), "binary not removed")
		}
	})

	t.Run("unwritable binary dir refuses before any prompt", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes everywhere")
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, target := fakeInstall(t)
		testassert.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		uninstallSeams(t, true, "y\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall"})
		testassert.Equal(t, code, 1)
		testassert.Require(t, strings.Contains(stderr, "lightcode: binary is at "+target+" (not writable); re-run as: sudo lightcode uninstall") && !strings.Contains(stderr, "[y/N]"), "stderr = %q; the preflight must refuse before any prompt: %q", stderr, stderr)
	})

	t.Run("privileged partial flow purges data then points at sudo", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes everywhere")
		}
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		testassert.NoError(t, os.MkdirAll(dataDir, 0o700))
		dir, target := fakeInstall(t)
		testassert.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		uninstallSeams(t, true, "y\n")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge"})
		testassert.Require(t, code == 1, "exit code = %d, want 1\nstderr: %s", code, stderr)
		testassert.Require(t, strings.Contains(stderr, "removed "+dataDir+"\n") && strings.Contains(stderr, "lightcode: data removed; binary still installed at "+target+" - run: sudo lightcode uninstall --yes"), "stderr missing data removal line: %q; stderr = %q", stderr, stderr)
		{
			_, err := os.Stat(dataDir)
			testassert.Require(t, os.IsNotExist(err), "data dir must be purged in the partial flow")
		}
		{
			_, err := os.Stat(target)
			testassert.Require(t, err == nil, "binary must remain when its dir is unwritable")
		}
	})

	t.Run("binary-only flow works without HOME", func(t *testing.T) {
		t.Setenv("HOME", "")
		_, target := fakeInstall(t)
		uninstallSeams(t, false, "")

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--yes"})
		testassert.Require(t, code == 0, "exit code = %d, want 0\nstderr: %s", code, stderr)
		testassert.Require(t, !strings.Contains(stderr, "resolve home dir"), "the binary-only flow must not resolve the home dir: %q", stderr)
		{
			_, err := os.Stat(target)
			testassert.Require(t, os.IsNotExist(err), "binary not removed")
		}
	})

	t.Run("root purge refusal precedes home resolution", func(t *testing.T) {
		t.Setenv("HOME", "")
		fakeInstall(t)
		oldEuid := selfupdate.Geteuid
		selfupdate.Geteuid = func() int { return 0 }
		t.Cleanup(func() { selfupdate.Geteuid = oldEuid })

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--yes"})
		testassert.Equal(t, code, 1)
		testassert.Require(t, strings.Contains(stderr, "refusing to purge user data as root") && !strings.Contains(stderr, "resolve home dir"), "stderr = %q; the root refusal must fire before any home lookup: %q", stderr, stderr)
	})

	t.Run("root refuses to purge", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dataDir := filepath.Join(home, ".lightcode")
		testassert.NoError(t, os.MkdirAll(dataDir, 0o700))
		fakeInstall(t)
		oldEuid := selfupdate.Geteuid
		selfupdate.Geteuid = func() int { return 0 }
		t.Cleanup(func() { selfupdate.Geteuid = oldEuid })

		_, code, _, stderr := capture(t, []string{"lightcode", "uninstall", "--purge", "--yes"})
		testassert.Equal(t, code, 1)
		testassert.Require(t, strings.Contains(stderr, "lightcode: refusing to purge user data as root; run 'lightcode uninstall --purge' as the owning user first, then remove the binary with sudo"), "stderr = %q", stderr)
		{
			_, err := os.Stat(dataDir)
			testassert.Require(t, err == nil, "root purge refusal must remove nothing")
		}
	})
}

func TestDispatchCompletion(t *testing.T) {
	t.Run("registry drift guard across all shells", func(t *testing.T) {
		for _, shell := range []string{"bash", "zsh", "fish"} {
			_, code, stdout, stderr := capture(t, []string{"lightcode", "completion", shell})
			testassert.Require(t, code == 0 && stderr == "", "%s: exit=%d stderr=%q", shell, code, stderr)
			// Every registry command, every registered flag name, and the
			// top-level aliases must appear; a new command or flag that
			// misses the generator fails here.
			for i := range commands {
				cmd := &commands[i]
				testassert.Require(t, strings.Contains(stdout, cmd.name), "%s: missing command %q", shell, cmd.name)
				for _, flagName := range registeredFlagNames(cmd) {
					testassert.Require(t, strings.Contains(stdout, flagName), "%s: missing flag %q of %q", shell, flagName, cmd.name)
				}
			}
			aliases := []string{"-h", "--help", "-v", "--version"}
			if shell == "fish" {
				// fish declares flags in its native -s/-l form.
				aliases = []string{"-s h", "-l help", "-s v", "-l version"}
			}
			for _, alias := range aliases {
				testassert.Require(t, strings.Contains(stdout, alias), "%s: missing top-level alias %q", shell, alias)
			}
		}
	})

	t.Run("per-command help flags for sampled commands", func(t *testing.T) {
		_, _, stdout, _ := capture(t, []string{"lightcode", "completion", "bash"})
		for _, name := range []string{"models", "doctor", "upgrade"} {
			_, after, found := strings.Cut(stdout, name+")")
			testassert.Require(t, found, "no case arm for %q", name)
			arm, _, _ := strings.Cut(after, ";;")
			testassert.Require(t, strings.Contains(arm, "-h") && strings.Contains(arm, "--help"), "%q arm missing injected help flags: %q", name, arm)
		}
	})

	t.Run("shell enum arm is static", func(t *testing.T) {
		_, _, stdout, _ := capture(t, []string{"lightcode", "completion", "bash"})
		_, after, found := strings.Cut(stdout, "completion)")
		testassert.Require(t, found, "no case arm for completion")
		arm, _, _ := strings.Cut(after, ";;")
		for _, shell := range []string{"bash", "zsh", "fish"} {
			testassert.Require(t, strings.Contains(arm, shell), "completion arm missing %q: %q", shell, arm)
		}
	})

	t.Run("bare completion emits bash", func(t *testing.T) {
		_, code, stdout, _ := capture(t, []string{"lightcode", "completion"})
		testassert.Require(t, code == 0 && strings.HasPrefix(stdout, "# bash completion for lightcode"), "exit=%d stdout=%q", code, stdout)
	})

	t.Run("unsupported shell exits 2", func(t *testing.T) {
		_, code, stdout, stderr := capture(t, []string{"lightcode", "completion", "powershell"})
		testassert.Require(t, code == 2 && stdout == "", "exit=%d stdout=%q", code, stdout)
		testassert.Require(t, strings.Contains(stderr, `unsupported shell "powershell" (supported: bash, zsh, fish)`), "stderr = %q", stderr)
	})

	t.Run("second positional exits 2", func(t *testing.T) {
		_, code, _, stderr := capture(t, []string{"lightcode", "completion", "bash", "zsh"})
		testassert.Require(t, code == 2 && strings.Contains(stderr, `unexpected argument "zsh"`), "exit=%d stderr=%q", code, stderr)
	})
}

func TestEscapeSingleQuoted(t *testing.T) {
	testassert.Equal(t, escapeSingleQuoted("a'b"), `a'\''b`)
}

// TestDispatchRuntimeUsage pins the runtime command's argument contract at
// the dispatch layer: exactly --data-dir and --config, both required and
// nonempty, and no positional operands — every violation takes the existing
// usage/exit-2 path before any owner work starts.
func TestDispatchRuntimeUsage(t *testing.T) {
	for _, argv := range [][]string{
		{"lightcode", "runtime"},
		{"lightcode", "runtime", "--data-dir", "/tmp/lightcode-runtime-usage-data"},
		{"lightcode", "runtime", "--config", "/tmp/lightcode-runtime-usage-config.json"},
		{"lightcode", "runtime", "--data-dir", "", "--config", "/tmp/lightcode-runtime-usage-config.json"},
		{"lightcode", "runtime", "--data-dir", "/tmp/lightcode-runtime-usage-data", "--config", ""},
		{"lightcode", "runtime", "--data-dir", "/tmp/lightcode-runtime-usage-data", "--config", "/tmp/lightcode-runtime-usage-config.json", "extra"},
		{"lightcode", "runtime", "--bogus"},
	} {
		gui, code, stdout, stderr := capture(t, argv)
		testassert.Require(t, !gui, "%v: must not reach the GUI path", argv)
		testassert.Require(t, code == 2, "%v: exit code = %d, want 2", argv, code)
		testassert.Require(t, stdout == "", "%v: stdout must be empty, got %q", argv, stdout)
		testassert.Require(t, strings.HasPrefix(stderr, "lightcode: ") && strings.Contains(stderr, "Usage: lightcode runtime"), "%v: stderr missing error prefix: %q; %v: stderr missing runtime usage line: %q", argv, stderr, argv, stderr)
	}
}

// TestDispatchModelsUtilityRouting pins the root's models argument checks at
// the dispatch layer for the rows that need no configuration read: the
// remainder fold keeps bare trailing flags out of the operand list, and the
// zero/one provider check reports an extra operand through the root's
// usageErrorf so dispatch prints the usage diagnostic with exit code 2.
func TestDispatchModelsUtilityRouting(t *testing.T) {
	t.Run("two positionals exit 2 as a usage error", func(t *testing.T) {
		_, code, stdout, stderr := capture(t, []string{"lightcode", "models", "p1", "p2"})
		testassert.Require(t, code == 2 && stdout == "", "exit=%d stdout=%q", code, stdout)
		testassert.Require(t, strings.HasPrefix(stderr, "lightcode: ") && strings.Contains(stderr, `unexpected argument "p2"`) && strings.Contains(stderr, "Usage: lightcode models"), "stderr missing translated usage diagnostic: %q; stderr missing models usage line: %q", stderr, stderr)
	})

	t.Run("terminator keeps operands out of the flag parse", func(t *testing.T) {
		// The root flag parse stops at `--`; a second positionals
		// violation proves the terminated operands reached the provider
		// check instead of being parsed as flags.
		_, code, stdout, stderr := capture(t, []string{"lightcode", "models", "--", "p1", "p2"})
		testassert.Require(t, code == 2 && stdout == "", "exit=%d stdout=%q", code, stdout)
		testassert.Require(t, strings.Contains(stderr, `unexpected argument "p2"`), "stderr missing translated usage diagnostic: %q", stderr)
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
	testassert.Require(t, err == nil || os.IsNotExist(err), "snapshot %s: %v", root, err)
	return snap
}

func TestDispatchConfig(t *testing.T) {
	t.Run("path prints the resolved path even when absent", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		_, code, stdout, stderr := capture(t, []string{"lightcode", "config", "--path"})
		testassert.Require(t, code == 0 && stderr == "", "exit=%d stderr=%q", code, stderr)
		testassert.Require(t, stdout == filepath.Join(home, ".lightcode", "config.json")+"\n", "stdout = %q", stdout)
	})

	t.Run("path honors LIGHTCODE_CONFIG", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "custom.json")
		t.Setenv("LIGHTCODE_CONFIG", custom)
		_, code, stdout, _ := capture(t, []string{"lightcode", "config", "--path"})
		testassert.Require(t, code == 0 && stdout == custom+"\n", "exit=%d stdout=%q", code, stdout)
	})

	t.Run("default is byte passthrough even for invalid json", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		body := "{not json at all"
		testassert.NoError(t, os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(home, ".lightcode", "config.json"), []byte(body), 0o600))
		_, code, stdout, stderr := capture(t, []string{"lightcode", "config"})
		testassert.Require(t, code == 0 && stderr == "", "exit=%d stderr=%q", code, stderr)
		testassert.Equal(t, stdout, body)
	})

	t.Run("env override notes the path on stderr", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "custom.json")
		testassert.NoError(t, os.WriteFile(custom, []byte("{}"), 0o600))
		t.Setenv("LIGHTCODE_CONFIG", custom)
		_, code, stdout, stderr := capture(t, []string{"lightcode", "config"})
		testassert.Require(t, code == 0 && stdout == "{}", "exit=%d stdout=%q", code, stdout)
		testassert.Require(t, strings.Contains(stderr, custom), "stderr missing the resolved path: %q", stderr)
	})

	t.Run("missing file errors without creating it", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		path := filepath.Join(home, ".lightcode", "config.json")
		before := snapshotHome(t, home)
		_, code, stdout, stderr := capture(t, []string{"lightcode", "config"})
		testassert.Require(t, code == 1 && stdout == "", "exit=%d stdout=%q", code, stdout)
		testassert.Require(t, strings.Contains(stderr, "lightcode: config not created yet (run lightcode once to initialize): "+path), "stderr = %q", stderr)
		{
			after := snapshotHome(t, home)
			testassert.Require(t, reflect.DeepEqual(before, after), "config wrote to the filesystem:\nbefore: %v\nafter:  %v", before, after)
		}
	})

	t.Run("normal run performs zero writes", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIGHTCODE_CONFIG", "")
		testassert.NoError(t, os.MkdirAll(filepath.Join(home, ".lightcode"), 0o700))
		testassert.NoError(t, os.WriteFile(filepath.Join(home, ".lightcode", "config.json"), []byte("{}"), 0o600))
		before := snapshotHome(t, home)
		_, code, stdout, _ := capture(t, []string{"lightcode", "config"})
		testassert.Require(t, code == 0 && stdout == "{}", "exit=%d stdout=%q", code, stdout)
		{
			after := snapshotHome(t, home)
			testassert.Require(t, reflect.DeepEqual(before, after), "config wrote to the filesystem:\nbefore: %v\nafter:  %v", before, after)
		}
	})

	t.Run("resolve failure exits 1", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("LIGHTCODE_CONFIG", "")
		_, code, _, stderr := capture(t, []string{"lightcode", "config", "--path"})
		testassert.Require(t, code == 1 && strings.HasPrefix(stderr, "lightcode: "), "exit=%d stderr=%q", code, stderr)
	})
}

// TestRemovedOwnerTransportSymbols proves the removed daemon transport cannot
// silently return: serve and stop resolve as unknown commands through
// findCommand's nil path, and zero references to the transport's package and
// symbols exist anywhere in the tree.
func TestRemovedOwnerTransportSymbols(t *testing.T) {
	for _, name := range []string{"serve", "stop"} {
		{
			cmd := findCommand(name)
			testassert.Require(t, cmd == nil, "%q must resolve as an unknown command, got %#v", name, cmd)
		}
	}

	// The full removed surface: the owner lock file and its liveness probes,
	// the shutdown endpoint and its trigger, the adapter lease/attachment
	// machinery, the HTTP service and its handlers, the serve/stop command
	// bodies, and the transport package with its client. Unqualified Client
	// and NewClient stay out of the list: they name the surviving generated
	// protocol clients, which have nothing to do with this transport.
	symbols := []string{
		"owner.lock",
		"IsStale",
		"WaitForOwnerExit",
		"WaitForOwnerExitContext",
		"LockFile",
		"RequestShutdown",
		"/v1/owner/shutdown",
		"AttachLocalAdapter",
		"DetachAdapter",
		"adapterLease",
		"hasLocalAdapterLeaseLocked",
		"newAdapterID",
		"LocalService",
		"handleAdapterSSE",
		"streamEvents",
		"rpcHandlers",
		"attachAdapter",
		"adapterRPCRequest",
		"rpcEnvelope",
		"runServe",
		"runStop",
		"internal/server",
		"server.NewClient",
		"AttachAdapter",
		"WaitOwner",
		"/v1/adapter",
	}
	// The scan covers every code file in the product tree — Go, the
	// frontend's Svelte/TS/JS/JSON/HTML — so a reference added anywhere in
	// the repo (including the frontend) fails. Dot-directories are the
	// repo's private/local material (.git, .office, .claude, .opencode),
	// and node_modules/dist/build are dependencies and build output; none
	// is product code. Test files may carry these symbols as the data of
	// absence assertions, which is not a reference to the removed transport.
	codeExts := map[string]bool{
		".go": true, ".svelte": true, ".ts": true, ".js": true,
		".mjs": true, ".cjs": true, ".json": true, ".html": true,
	}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name != "." && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist" || name == "build") {
				return filepath.SkipDir
			}
			return nil
		}
		if !codeExts[filepath.Ext(path)] || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, symbol := range symbols {
			testassert.Check(t, !strings.Contains(string(b), symbol), "%s references removed owner transport symbol %q", path, symbol)
		}
		return nil
	})
	testassert.NoError(t, err)
}
