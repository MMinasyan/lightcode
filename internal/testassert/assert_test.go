package testassert

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// selfExecEnv switches the running test binary into the child mode of the
// assertion falsification regression.
const selfExecEnv = "LIGHTCODE_TESTASSERT_SELF_EXEC"

// selfExecMarker is printed after the assertion calls of one child mode. The
// parent requires it exactly where the call must not stop the test.
const selfExecMarker = "testassert self-exec: after assertions"

// TestAssertionsSelfExec proves the shared assertion owner still fails when it
// must: positive calls pass, false Require, NoError and Equal stop the test
// before the marker, and false Check fails while reaching it.
func TestAssertionsSelfExec(t *testing.T) {
	if mode := os.Getenv(selfExecEnv); mode != "" {
		runSelfExec(t, mode)
		return
	}
	cases := []struct {
		mode       string
		wantFailed bool
		wantMarker bool
	}{
		{"pass", false, true},
		{"require", true, false},
		{"check", true, true},
		{"noerror", true, false},
		{"equal", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.timeout=120s")
			cmd.Env = append(os.Environ(), selfExecEnv+"="+tc.mode)
			out, err := cmd.CombinedOutput()
			testassertRun(t, tc.mode, tc.wantFailed, tc.wantMarker, err, string(out))
		})
	}
}

// testassertRun checks one child mode's exit status and marker in the parent:
// t.Name() in the child includes the subtest, so the run filter selects it
// while the environment selects the subject.
func testassertRun(t *testing.T, mode string, wantFailed, wantMarker bool, err error, out string) {
	t.Helper()
	if wantFailed {
		if err == nil {
			t.Fatalf("%s child passed, want a failing exit status:\n%s", mode, out)
		}
	} else if err != nil {
		t.Fatalf("%s child failed: %v\n%s", mode, err, out)
	}
	if got := strings.Contains(out, selfExecMarker); got != wantMarker {
		t.Fatalf("%s child marker present = %v, want %v:\n%s", mode, got, wantMarker, out)
	}
}

// runSelfExec is the child body: the named assertion call and the marker that
// follows it when the call does not stop the test.
func runSelfExec(t *testing.T, mode string) {
	switch mode {
	case "pass":
		Require(t, true, "require true")
		Check(t, true, "check true")
		NoError(t, nil)
		Equal(t, "same", "same")
	case "require":
		Require(t, false, "require false")
	case "check":
		Check(t, false, "check false")
	case "noerror":
		NoError(t, errors.New("noerror failure"))
	case "equal":
		Equal(t, 1, 2)
	}
	fmt.Println(selfExecMarker)
}
