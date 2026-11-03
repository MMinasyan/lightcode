package tool

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestRunForegroundCommandTimeoutSendsTERMBeforeKILL pins the timeout
// termination order: SIGTERM reaches the process group first, so a TERM
// trap's output lands in the captured body before the timeout error.
func TestRunForegroundCommandTimeoutSendsTERMBeforeKILL(t *testing.T) {
	dir := t.TempDir()

	output, err := RunForegroundCommand(context.Background(), "trap 'echo got-term; exit 0' TERM; sleep 10", dir, 1, 0, 0, filepath.Join(dir, ".lightcode"), os.Environ())
	var exitErr *ExitError
	testassert.Require(t, errors.As(err, &exitErr), "error = %T %v, want *ExitError", err, err)
	testassert.Require(t, strings.Contains(output, "got-term") && strings.Contains(exitErr.Output, "got-term"), "output=%q exitErr.Output=%q, want SIGTERM trap output before timeout error", output, exitErr.Output)
}

func TestRunCommandTerminateProcessUsesExistingWaitChannel(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 10")
	cmd.SysProcAttr = childProcAttr()
	testassert.NoError(t, cmd.Start())
	done := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(waitDone)
	}()

	terminateProcess(cmd, waitDone)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		t.Fatal("terminateProcess did not wait for the existing cmd.Wait channel")
	}
}

// The classification oracles below drive the shared foreground body's
// completed-versus-late cases deterministically through the Wait-returned
// seam: a blocking child under a cancellable context, and a waitCommand
// replacement that releases the Wait-returned point at a chosen moment.

// TestRunForegroundCommandCancellationWhileRunningIsDeterministic cancels
// the parent while the wait is seam-blocked: the classification must be the
// parent cause (cancellation), not whichever wait channel later fires.
func TestRunForegroundCommandCancellationWhileRunningIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	origWait := waitCommand
	waitCommand = func(cmd *exec.Cmd) error {
		<-release
		return origWait(cmd)
	}
	defer func() { waitCommand = origWait }()

	errCh := make(chan error, 1)
	go func() {
		_, err := RunForegroundCommand(ctx, "sleep 5", dir, 60, 0, 0, filepath.Join(dir, ".lightcode"), os.Environ())
		errCh <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	close(release)

	select {
	case err := <-errCh:
		var exitErr *ExitError
		testassert.Require(t, errors.As(err, &exitErr) && exitErr.ExitCode == -1 && exitErr.Output == "command cancelled", "err = %v, want the exact cancellation ExitError", err)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled command did not return")
	}
}

// TestRunForegroundCommandCompletedBeatsLateEvents completes the real wait
// through the seam, holds the outcome until the late event is live, and
// releases it into the runner's late branch: the already-finished command
// must keep its real result under both a late deadline and a late
// cancellation, whatever the wait channels raced.
func TestRunForegroundCommandCompletedBeatsLateEvents(t *testing.T) {
	t.Run("late deadline", func(t *testing.T) {
		dir := t.TempDir()
		origWait := waitCommand
		waitCommand = func(cmd *exec.Cmd) error {
			err := origWait(cmd)                // the real child finishes; its output is captured
			time.Sleep(1200 * time.Millisecond) // hold the result past the 1s deadline
			return err
		}
		defer func() { waitCommand = origWait }()

		result, err := RunForegroundCommand(context.Background(), "printf ok", dir, 1, 0, 0, filepath.Join(dir, ".lightcode"), os.Environ())
		testassert.Require(t, err == nil && result == "ok", "result = (%q, %v), want the already-finished real result", result, err)
	})

	t.Run("late cancellation", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		origWait := waitCommand
		waitCommand = func(cmd *exec.Cmd) error {
			err := origWait(cmd)               // the real child finishes; its output is captured
			time.Sleep(300 * time.Millisecond) // hold the result while the cancellation fires
			return err
		}
		defer func() { waitCommand = origWait }()

		errCh := make(chan error, 1)
		resCh := make(chan string, 1)
		go func() {
			result, err := RunForegroundCommand(ctx, "printf ok", dir, 0, 0, 0, filepath.Join(dir, ".lightcode"), os.Environ())
			resCh <- result
			errCh <- err
		}()
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case result := <-resCh:
			{
				err := <-errCh
				testassert.Require(t, err == nil && result == "ok", "result = (%q, %v), want the already-finished real result", result, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("completed command did not return")
		}
	})
}

// TestRunForegroundCommandTimeoutOverflowRejectedBeforeLaunch proves the
// overflow guard converts nothing and launches nothing.
func TestRunForegroundCommandTimeoutOverflowRejectedBeforeLaunch(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "overflow-launched")

	_, err := RunForegroundCommand(context.Background(), "touch "+probe, dir, int(math.MaxInt64/int64(time.Second))+1, 0, 0, filepath.Join(dir, ".lightcode"), os.Environ())
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "overflows the seconds-to-duration conversion"), "err = %v, want the overflow rejection", err)
	{
		_, statErr := os.Stat(probe)
		testassert.Require(t, os.IsNotExist(statErr), "command launched despite the overflow rejection")
	}
}

// TestRunForegroundCommandParentDeadlineIsCancellation pins that a
// parent-context deadline — as opposed to the run's own configured timeout —
// classifies as cancellation.
func TestRunForegroundCommandParentDeadlineIsCancellation(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(1*time.Second))
	defer cancel()

	_, err := RunForegroundCommand(ctx, "sleep 5", dir, 0, 0, 0, filepath.Join(dir, ".lightcode"), os.Environ())
	var exitErr *ExitError
	testassert.Require(t, errors.As(err, &exitErr) && exitErr.ExitCode == -1 && strings.HasPrefix(exitErr.Output, "command cancelled"), "err = %v, want the cancellation classification for a parent deadline", err)
}

// TestRunForegroundCommandNonZeroExitLargeOutputIsBounded pins the nonzero
// exit settlement over large output: the captured body is bounded and the
// full output spills beside the given spill directory, with the spill
// marker inside ExitError.Output.
func TestRunForegroundCommandNonZeroExitLargeOutputIsBounded(t *testing.T) {
	home := t.TempDir()
	spillDir := filepath.Join(home, ".lightcode")

	output, err := RunForegroundCommand(context.Background(), "printf 'abcdefg\\n1234567\\n' && exit 7", "", 2, 12, 5, spillDir, os.Environ())
	var exitErr *ExitError
	testassert.Require(t, errors.As(err, &exitErr), "error = %T %v, want *ExitError", err, err)
	testassert.Require(t, output != "abcdefg\n1234567\n", "output = %q, want bounded output", output)
	testassert.Require(t, strings.Contains(exitErr.Output, "Error: Exit code 7\n") && strings.Contains(exitErr.Output, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_"), "ExitError.Output = %q, missing exit header; ExitError.Output = %q, want spill marker", exitErr.Output, exitErr.Output)
}

// TestRunForegroundCommandTimeoutLargeOutputIsBounded pins the timeout
// settlement over large output: the timeout header plus the bounded body
// and the spill marker inside ExitError.Output.
func TestRunForegroundCommandTimeoutLargeOutputIsBounded(t *testing.T) {
	home := t.TempDir()
	spillDir := filepath.Join(home, ".lightcode")

	output, err := RunForegroundCommand(context.Background(), "printf 'abcdefg\\n1234567\\n'; sleep 5", "", 1, 12, 5, spillDir, os.Environ())
	var exitErr *ExitError
	testassert.Require(t, errors.As(err, &exitErr), "error = %T %v, want *ExitError", err, err)
	testassert.Require(t, output != "abcdefg\n1234567\n", "output = %q, want bounded output", output)
	testassert.Require(t, strings.Contains(exitErr.Output, "Error: Exit code -1 (timeout)\n") && strings.Contains(exitErr.Output, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_"), "ExitError.Output = %q, missing timeout header; ExitError.Output = %q, want spill marker", exitErr.Output, exitErr.Output)
}

// TestRunForegroundCommandTimeoutKillsNearConfiguredDeadline pins that a
// configured timeout kills the run close to its deadline and settles the
// retained timeout ExitError.
func TestRunForegroundCommandTimeoutKillsNearConfiguredDeadline(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()

	_, err := RunForegroundCommand(context.Background(), "sleep 2", dir, 1, 0, 0, filepath.Join(dir, ".lightcode"), os.Environ())
	var exitErr *ExitError
	testassert.Require(t, errors.As(err, &exitErr), "error = %T %v, want *ExitError", err, err)
	testassert.Require(t, exitErr.ExitCode == -1 && strings.Contains(exitErr.Output, "timeout"), "ExitError = %+v, want timeout with exit code -1", exitErr)
	{
		elapsed := time.Since(start)
		testassert.Require(t, elapsed <= 1500*time.Millisecond, "timeout elapsed %s, want command killed close to the configured deadline", elapsed)
	}
}

// TestRunForegroundCommandCancellationOutput pins the delivered-cancellation
// output shape with buffered output: the cancellation header plus bounded
// output and the spill marker. The empty-output "command cancelled" marker is
// owned by TestRunForegroundCommandCancellationWhileRunningIsDeterministic.
func TestRunForegroundCommandCancellationOutput(t *testing.T) {
	t.Run("non-empty", func(t *testing.T) {
		home := t.TempDir()
		ready := home + "/ready"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		errCh := make(chan error, 1)
		outCh := make(chan string, 1)
		command := "printf 'abcdefg\\n1234567\\n'; touch " + ready + "; sleep 5"
		go func() {
			output, err := RunForegroundCommand(ctx, command, "", 5, 12, 5, filepath.Join(home, ".lightcode"), os.Environ())
			outCh <- output
			errCh <- err
		}()
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		{
			_, err := os.Stat(ready)
			testassert.NoError(t, err)
		}
		cancel()
		var exitErr *ExitError
		select {
		case err := <-errCh:
			testassert.Require(t, errors.As(err, &exitErr), "error = %T %v, want *ExitError", err, err)
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled command did not return")
		}
		output := <-outCh
		testassert.Require(t, strings.HasPrefix(exitErr.Output, "command cancelled\n") && strings.Contains(exitErr.Output, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_"), "ExitError.Output = %q, want cancellation header plus output; ExitError.Output = %q, want spill marker", exitErr.Output, exitErr.Output)
		testassert.Require(t, output != "abcdefg\n1234567\n", "output = %q, want bounded output", output)
	})
}

func TestNormalizeRunCommandArgs(t *testing.T) {
	decode := func(t *testing.T, raw string) map[string]any {
		t.Helper()
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var args map[string]any
		{
			err := decoder.Decode(&args)
			testassert.Require(t, err == nil, "decode %s: %v", raw, err)
		}
		return args
	}
	assertNormalized := func(t *testing.T, raw string, wantTimeout int) {
		t.Helper()
		normalized, err := NormalizeRunCommandArgs(decode(t, raw), 120)
		testassert.Require(t, err == nil, "NormalizeRunCommandArgs(%s) = %v, want accepted", raw, err)
		testassert.Equal(t, normalized["command"], "ls")
		{
			got, ok := normalized["timeout"].(json.Number)
			testassert.Require(t, ok && got.String() == strconv.Itoa(wantTimeout), "timeout = %v, want the canonical lexeme %d", normalized["timeout"], wantTimeout)
		}
	}

	assertNormalized(t, `{"command":"ls"}`, 120)
	assertNormalized(t, `{"command":"ls","timeout":5}`, 5)
	assertNormalized(t, `{"command":"ls","timeout":0}`, 120)
	assertNormalized(t, `{"command":"ls","timeout":-3}`, 120)
	assertNormalized(t, `{"command":"ls","timeout":1.0}`, 1)
	// The exact positive seconds-to-duration bound is accepted, one over is not.
	assertNormalized(t, `{"command":"ls","timeout":9223372036}`, 9223372036)

	for _, raw := range []string{
		`{}`,
		`{"command":""}`,
		`{"command":null}`,
		`{"command":5}`,
		`{"command":"ls","timeout":1.5}`,
		`{"command":"ls","timeout":"5"}`,
		`{"command":"ls","timeout":true}`,
		`{"command":"ls","timeout":null}`,
		`{"command":"ls","timeout":9223372036854775808}`,
		`{"command":"ls","timeout":9223372037}`,
	} {
		{
			_, err := NormalizeRunCommandArgs(decode(t, raw), 120)
			testassert.Check(t, err != nil, "NormalizeRunCommandArgs(%s) accepted, want rejection", raw)
		}
	}
	for _, raw := range []string{
		`{"command":"ls","background":null}`,
		`{"command":"ls","background":0}`,
		`{"command":"ls","background":"later"}`,
	} {
		{
			_, err := NormalizeRunCommandArgs(decode(t, raw), 120)
			testassert.Check(t, err != nil && err.Error() == "run_command: background must be a boolean", "NormalizeRunCommandArgs(%s) = %v, want the boolean-consumption error", raw, err)
		}
	}

	// Present background booleans are preserved, an absent one is written as
	// false, and the background timeout rule (absent/sub-one -> 0) applies
	// only when background is true.
	backgroundCases := []struct {
		raw            string
		wantBackground bool
		wantTimeout    int
	}{
		{`{"command":"ls"}`, false, 120},
		{`{"command":"ls","background":false}`, false, 120},
		{`{"command":"ls","background":false,"timeout":5}`, false, 5},
		{`{"command":"ls","background":true}`, true, 0},
		{`{"command":"ls","background":true,"timeout":0}`, true, 0},
		{`{"command":"ls","background":true,"timeout":-3}`, true, 0},
		{`{"command":"ls","background":true,"timeout":5}`, true, 5},
	}
	for _, tc := range backgroundCases {
		normalized, err := NormalizeRunCommandArgs(decode(t, tc.raw), 120)
		if err != nil {
			t.Errorf("NormalizeRunCommandArgs(%s) = %v, want accepted", tc.raw, err)
			continue
		}
		{
			got, ok := normalized["background"].(bool)
			testassert.Check(t, ok && got == tc.wantBackground, "NormalizeRunCommandArgs(%s) background = %v, want %v", tc.raw, normalized["background"], tc.wantBackground)
		}
		{
			got, ok := normalized["timeout"].(json.Number)
			testassert.Check(t, ok && got.String() == strconv.Itoa(tc.wantTimeout), "NormalizeRunCommandArgs(%s) timeout = %v, want the canonical lexeme %d", tc.raw, normalized["timeout"], tc.wantTimeout)
		}
	}

	normalized, err := NormalizeRunCommandArgs(decode(t, `{"command":"ls","_lightcode_receipt":"x"}`), 120)
	testassert.NoError(t, err)
	{
		_, ok := normalized["_lightcode_receipt"]
		testassert.Require(t, !ok, "normalized arguments retained a private field")
	}
}
