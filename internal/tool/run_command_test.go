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

	"github.com/MMinasyan/lightcode/internal/config"
)

func TestRunCommandRequiresCommand(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 1}, t.TempDir(), nil)

	for _, params := range []map[string]any{{}, {"command": ""}} {
		_, err := tool.Execute(context.Background(), params)
		if err == nil || !strings.Contains(err.Error(), "command is required") {
			t.Fatalf("Execute(%v) error = %v, want command-required error", params, err)
		}
	}
}

func TestRunCommandExecutesShellCommandWithInheritedEnv(t *testing.T) {
	t.Setenv("LIGHTCODE_RUN_COMMAND_TEST", "env-value")
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil)

	result, err := tool.Execute(context.Background(), map[string]any{
		"command": "printf '%s' \"$LIGHTCODE_RUN_COMMAND_TEST\"",
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "env-value" {
		t.Fatalf("Execute result = %q, want inherited env output", result)
	}
}

func TestRunCommandEachCallStartsFreshShell(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil)

	first, err := tool.Execute(context.Background(), map[string]any{"command": "cd / && pwd"})
	if err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	second, err := tool.Execute(context.Background(), map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("second Execute error = %v", err)
	}
	if strings.TrimSpace(first) != "/" {
		t.Fatalf("first pwd = %q, want /", first)
	}
	if strings.TrimSpace(second) == "/" {
		t.Fatalf("second pwd = %q, want fresh shell not affected by prior cd", second)
	}
}

func TestRunCommandReturnsNoOutputForSuccessfulSilentCommand(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil)

	result, err := tool.Execute(context.Background(), map[string]any{"command": "true"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "(No output)" {
		t.Fatalf("Execute result = %q, want no-output marker", result)
	}
}

func TestRunCommandNonZeroExitReturnsExitErrorWithOutput(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 2}, t.TempDir(), nil)

	output, err := tool.Execute(context.Background(), map[string]any{"command": "printf fail && exit 7"})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
	}
	if exitErr.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", exitErr.ExitCode)
	}
	if output != "fail" || !strings.Contains(exitErr.Output, "Error: Exit code 7\nfail") {
		t.Fatalf("output=%q exitErr.Output=%q, want failed command output", output, exitErr.Output)
	}
}

func TestRunCommandNonZeroExitLargeOutputIsBounded(t *testing.T) {
	home := t.TempDir()
	tool := NewRunCommand(config.ToolsConfig{
		CommandTimeout:   2,
		MaxOutputBytes:   12,
		ReadLineMaxChars: 5,
	}, home, nil)

	output, err := tool.Execute(context.Background(), map[string]any{"command": "printf 'abcdefg\\n1234567\\n' && exit 7"})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
	}
	if output == "abcdefg\n1234567\n" {
		t.Fatalf("output = %q, want bounded output", output)
	}
	if !strings.Contains(exitErr.Output, "Error: Exit code 7\n") {
		t.Fatalf("ExitError.Output = %q, missing exit header", exitErr.Output)
	}
	if !strings.Contains(exitErr.Output, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_") {
		t.Fatalf("ExitError.Output = %q, want spill marker", exitErr.Output)
	}
}

func TestRunCommandTimeoutUsesOverrideAndReturnsExitError(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 5}, t.TempDir(), nil)
	start := time.Now()

	_, err := tool.Execute(context.Background(), map[string]any{
		"command": "sleep 2",
		"timeout": float64(1),
	})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
	}
	if exitErr.ExitCode != -1 || !strings.Contains(exitErr.Output, "timeout") {
		t.Fatalf("ExitError = %+v, want timeout with exit code -1", exitErr)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("timeout elapsed %s, want command killed close to override", elapsed)
	}
}

func TestRunCommandTimeoutLargeOutputIsBounded(t *testing.T) {
	home := t.TempDir()
	tool := NewRunCommand(config.ToolsConfig{
		CommandTimeout:   5,
		MaxOutputBytes:   12,
		ReadLineMaxChars: 5,
	}, home, nil)

	output, err := tool.Execute(context.Background(), map[string]any{
		"command": "printf 'abcdefg\\n1234567\\n'; sleep 5",
		"timeout": float64(1),
	})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
	}
	if output == "abcdefg\n1234567\n" {
		t.Fatalf("output = %q, want bounded output", output)
	}
	if !strings.Contains(exitErr.Output, "Error: Exit code -1 (timeout)\n") {
		t.Fatalf("ExitError.Output = %q, missing timeout header", exitErr.Output)
	}
	if !strings.Contains(exitErr.Output, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_") {
		t.Fatalf("ExitError.Output = %q, want spill marker", exitErr.Output)
	}
}

func TestRunCommandCancellationOutput(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 5}, t.TempDir(), nil)
		errCh := make(chan error, 1)
		go func() {
			_, err := tool.Execute(ctx, map[string]any{"command": "sleep 5"})
			errCh <- err
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		var exitErr *ExitError
		select {
		case err := <-errCh:
			if !errors.As(err, &exitErr) {
				t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled command did not return")
		}
		if exitErr.Output != "command cancelled" {
			t.Fatalf("ExitError.Output = %q, want exact cancellation marker", exitErr.Output)
		}
	})

	t.Run("non-empty", func(t *testing.T) {
		home := t.TempDir()
		ready := home + "/ready"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tool := NewRunCommand(config.ToolsConfig{
			CommandTimeout:   5,
			MaxOutputBytes:   12,
			ReadLineMaxChars: 5,
		}, home, nil)
		errCh := make(chan error, 1)
		outCh := make(chan string, 1)
		command := "printf 'abcdefg\\n1234567\\n'; touch " + ready + "; sleep 5"
		go func() {
			output, err := tool.Execute(ctx, map[string]any{"command": command})
			outCh <- output
			errCh <- err
		}()
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := os.Stat(ready); err != nil {
			t.Fatalf("command did not reach ready marker: %v", err)
		}
		cancel()
		var exitErr *ExitError
		select {
		case err := <-errCh:
			if !errors.As(err, &exitErr) {
				t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled command did not return")
		}
		output := <-outCh
		if !strings.HasPrefix(exitErr.Output, "command cancelled\n") {
			t.Fatalf("ExitError.Output = %q, want cancellation header plus output", exitErr.Output)
		}
		if !strings.Contains(exitErr.Output, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_") {
			t.Fatalf("ExitError.Output = %q, want spill marker", exitErr.Output)
		}
		if output == "abcdefg\n1234567\n" {
			t.Fatalf("output = %q, want bounded output", output)
		}
	})
}

func TestRunCommandBackgroundDelegatesToProcessManager(t *testing.T) {
	procMgr := &recordingProcessManager{id: "proc-2", activeIDs: []string{"proc-1", "proc-2"}}
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 3}, t.TempDir(), procMgr)

	result, err := tool.Execute(context.Background(), map[string]any{
		"command":    "sleep 10",
		"background": true,
		"timeout":    float64(4),
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	want := "Command running in the background with ID: `proc-2`. You will be notified when it finishes. If your next steps do not depend on its output, continue with them; otherwise use sleep to wait and process to read the output.\nRunning in the background: `proc-1, proc-2`."
	if result != want {
		t.Fatalf("Execute result = %q, want background id", result)
	}
	if procMgr.command != "sleep 10" || procMgr.timeoutSec != 4 {
		t.Fatalf("ProcessManager call = (%q, %d), want command and timeout override", procMgr.command, procMgr.timeoutSec)
	}
}

func TestRunCommandBackgroundUsesEmptyActiveListFromLister(t *testing.T) {
	procMgr := &recordingProcessManager{id: "proc-1", activeIDs: []string{}}
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 3}, t.TempDir(), procMgr)

	result, err := tool.Execute(context.Background(), map[string]any{
		"command":    "true",
		"background": true,
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	want := "Command running in the background with ID: `proc-1`. You will be notified when it finishes. If your next steps do not depend on its output, continue with them; otherwise use sleep to wait and process to read the output.\nRunning in the background: ``."
	if result != want {
		t.Fatalf("Execute result = %q, want empty active list", result)
	}
}

func TestRunCommandBackgroundWithoutTimeoutDoesNotUseForegroundDefault(t *testing.T) {
	procMgr := &recordingProcessManager{id: "proc-1"}
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 3}, t.TempDir(), procMgr)

	if _, err := tool.Execute(context.Background(), map[string]any{
		"command":    "sleep 10",
		"background": true,
	}); err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if procMgr.timeoutSec != 0 {
		t.Fatalf("background timeout = %d, want no implicit foreground default", procMgr.timeoutSec)
	}
}

func TestRunCommandBackgroundWithoutProcessManagerErrors(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 3}, t.TempDir(), nil)

	_, err := tool.Execute(context.Background(), map[string]any{
		"command":    "sleep 10",
		"background": true,
	})
	if err == nil || !strings.Contains(err.Error(), "background processes not available") {
		t.Fatalf("Execute error = %v, want background unavailable error", err)
	}
}

func TestRunCommandTruncatesLargeOutputAndSpillsFullOutput(t *testing.T) {
	home := t.TempDir()
	tool := NewRunCommand(config.ToolsConfig{
		CommandTimeout:   2,
		MaxOutputBytes:   12,
		ReadLineMaxChars: 5,
	}, home, nil)

	result, err := tool.Execute(context.Background(), map[string]any{"command": "printf 'abcdefg\\n1234567\\n'"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "abcde... [truncated 7 chars]") {
		t.Fatalf("Execute result = %q, want per-line truncation", result)
	}
	if !strings.Contains(result, "Full output (16 bytes) saved to: "+home+"/.lightcode/cmd_output_") {
		t.Fatalf("Execute result = %q, want spill path under home", result)
	}
}

func TestRunCommandTruncatesManyLinesAndSpillsFullOutput(t *testing.T) {
	home := t.TempDir()
	tool := NewRunCommand(config.ToolsConfig{
		CommandTimeout: 2,
		MaxOutputBytes: 100,
	}, home, nil)

	var b strings.Builder
	for i := 1; i <= 25; i++ {
		b.WriteString("line ")
		b.WriteString(string(rune('A' + i - 1)))
		b.WriteByte('\n')
	}
	fullOutput := b.String()

	result, err := tool.Execute(context.Background(), map[string]any{"command": "cat <<'EOF'\n" + fullOutput + "EOF"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result, "line A") || !strings.Contains(result, "line Y") {
		t.Fatalf("Execute result = %q, want first and last lines", result)
	}
	spillPath := extractSpillPath(t, result)
	if !strings.HasPrefix(spillPath, home+"/.lightcode/cmd_output_") {
		t.Fatalf("spill path = %q, want command spill path under home %q", spillPath, home)
	}
	data, err := os.ReadFile(spillPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", spillPath, err)
	}
	if string(data) != fullOutput {
		t.Fatalf("spill file content = %q, want full output %q", string(data), fullOutput)
	}
}

func TestRunCommandTimeoutSendsTERMBeforeKILL(t *testing.T) {
	tool := NewRunCommand(config.ToolsConfig{CommandTimeout: 5}, t.TempDir(), nil)

	output, err := tool.Execute(context.Background(), map[string]any{
		"command": "trap 'echo got-term; exit 0' TERM; sleep 10",
		"timeout": float64(1),
	})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Execute error = %T %v, want *ExitError", err, err)
	}
	if !strings.Contains(output, "got-term") || !strings.Contains(exitErr.Output, "got-term") {
		t.Fatalf("output=%q exitErr.Output=%q, want SIGTERM trap output before timeout error", output, exitErr.Output)
	}
}

func TestRunCommandTerminateProcessUsesExistingWaitChannel(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 10")
	cmd.SysProcAttr = childProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start error = %v", err)
	}
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

type recordingProcessManager struct {
	id         string
	err        error
	command    string
	timeoutSec int
	activeIDs  []string
}

func (m *recordingProcessManager) Start(command string, timeoutSec int) (string, error) {
	m.command = command
	m.timeoutSec = timeoutSec
	return m.id, m.err
}

func (m *recordingProcessManager) ActiveIDs() []string {
	return append([]string(nil), m.activeIDs...)
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
		if !errors.As(err, &exitErr) || exitErr.ExitCode != -1 || exitErr.Output != "command cancelled" {
			t.Fatalf("err = %v, want the exact cancellation ExitError", err)
		}
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
		if err != nil || result != "ok" {
			t.Fatalf("result = (%q, %v), want the already-finished real result", result, err)
		}
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
			if err := <-errCh; err != nil || result != "ok" {
				t.Fatalf("result = (%q, %v), want the already-finished real result", result, err)
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
	if err == nil || !strings.Contains(err.Error(), "overflows the seconds-to-duration conversion") {
		t.Fatalf("err = %v, want the overflow rejection", err)
	}
	if _, statErr := os.Stat(probe); !os.IsNotExist(statErr) {
		t.Fatalf("command launched despite the overflow rejection")
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
	if !errors.As(err, &exitErr) || exitErr.ExitCode != -1 || !strings.HasPrefix(exitErr.Output, "command cancelled") {
		t.Fatalf("err = %v, want the cancellation classification for a parent deadline", err)
	}
}

func TestNormalizeRunCommandArgs(t *testing.T) {
	decode := func(t *testing.T, raw string) map[string]any {
		t.Helper()
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var args map[string]any
		if err := decoder.Decode(&args); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return args
	}
	assertNormalized := func(t *testing.T, raw string, wantTimeout int) {
		t.Helper()
		normalized, err := NormalizeRunCommandArgs(decode(t, raw), 120)
		if err != nil {
			t.Fatalf("NormalizeRunCommandArgs(%s) = %v, want accepted", raw, err)
		}
		if normalized["command"] != "ls" {
			t.Fatalf("command = %v, want ls", normalized["command"])
		}
		if got, ok := normalized["timeout"].(json.Number); !ok || got.String() != strconv.Itoa(wantTimeout) {
			t.Fatalf("timeout = %v, want the canonical lexeme %d", normalized["timeout"], wantTimeout)
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
		if _, err := NormalizeRunCommandArgs(decode(t, raw), 120); err == nil {
			t.Errorf("NormalizeRunCommandArgs(%s) accepted, want rejection", raw)
		}
	}
	for _, raw := range []string{
		`{"command":"ls","background":null}`,
		`{"command":"ls","background":0}`,
		`{"command":"ls","background":"later"}`,
	} {
		if _, err := NormalizeRunCommandArgs(decode(t, raw), 120); err == nil || err.Error() != "run_command: background must be a boolean" {
			t.Errorf("NormalizeRunCommandArgs(%s) = %v, want the boolean-consumption error", raw, err)
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
		if got, ok := normalized["background"].(bool); !ok || got != tc.wantBackground {
			t.Errorf("NormalizeRunCommandArgs(%s) background = %v, want %v", tc.raw, normalized["background"], tc.wantBackground)
		}
		if got, ok := normalized["timeout"].(json.Number); !ok || got.String() != strconv.Itoa(tc.wantTimeout) {
			t.Errorf("NormalizeRunCommandArgs(%s) timeout = %v, want the canonical lexeme %d", tc.raw, normalized["timeout"], tc.wantTimeout)
		}
	}

	normalized, err := NormalizeRunCommandArgs(decode(t, `{"command":"ls","_lightcode_receipt":"x"}`), 120)
	if err != nil {
		t.Fatalf("private-field strip: %v", err)
	}
	if _, ok := normalized["_lightcode_receipt"]; ok {
		t.Fatal("normalized arguments retained a private field")
	}
}

func extractSpillPath(t *testing.T, result string) string {
	t.Helper()
	marker := "saved to: "
	idx := strings.LastIndex(result, marker)
	if idx < 0 {
		t.Fatalf("result = %q, missing spill marker", result)
	}
	path := result[idx+len(marker):]
	if end := strings.IndexAny(path, "]\n"); end >= 0 {
		path = path[:end]
	}
	return strings.TrimSpace(path)
}
