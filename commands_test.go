package main

import (
	"bytes"
	"strings"
	"testing"
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
	for _, name := range []string{"desktop", "cli", "serve", "acp", "help"} {
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
