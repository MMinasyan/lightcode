package tool

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/cmdoutput"
)

// waitCommand is the one cmd.Wait seam for foreground commands: the
// Wait-returned point of every foreground run routes through it, so tests
// drive completed-versus-late classification deterministically. It is
// nil-free in production and simply delegates to cmd.Wait.
var waitCommand = func(cmd *exec.Cmd) error { return cmd.Wait() }

// RunForegroundCommand is the one foreground execution body the target
// tools plugin composes. It runs command
// through "sh -c" with the given environment in dir (empty means the
// current directory), captures combined output within the given limits,
// spills overflow into spillDir, and converges the process group with
// SIGTERM/500ms/SIGKILL. timeoutSec at most zero disables the timeout;
// a positive value is converted to a duration only within bounds.
//
// Classification follows the cause, never which wait channel fired: a
// Wait-returned command keeps its real result, a timeout exists only when a
// timeout was configured and its own timer fired, and a parent-context
// deadline or cancellation is always cancellation.
func RunForegroundCommand(ctx context.Context, command, dir string, timeoutSec, maxBytes, maxLineChars int, spillDir string, env []string) (string, error) {
	parent := ctx
	if timeoutSec > 0 {
		if int64(timeoutSec) > int64(math.MaxInt64/time.Second) {
			return "", fmt.Errorf("run_command: timeout %d overflows the seconds-to-duration conversion", timeoutSec)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Env = env
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.SysProcAttr = childProcAttr()

	capture := cmdoutput.NewCapture(cmdoutput.Options{
		Directory:    spillDir,
		SpillPrefix:  "cmd_output_",
		MaxBytes:     maxBytes,
		MaxLineChars: maxLineChars,
	})
	cmd.Stdout = capture.Stdout()
	cmd.Stderr = capture.Stderr()

	err := cmd.Start()
	if err != nil {
		return "", fmt.Errorf("run_command: start: %w", err)
	}
	defer capture.Close()

	done := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		done <- waitCommand(cmd)
		close(waitDone)
	}()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		// A Wait that already returned has the final word, whatever raced:
		// drain it non-blocking first — its real result settles through the
		// shared Wait-returned path below, exactly as the done branch
		// settles it, descendants included.
		select {
		case waitErr = <-done:
		default:
			// SIGTERM, wait 500ms, then SIGKILL. The delivery itself
			// classifies, never the exit status and never which wait
			// channel fired: a failed SIGTERM (ESRCH) proves the process
			// group was already gone — the command finished before our kill
			// could act, and its real result settles through the shared
			// Wait-returned path below. A delivered SIGTERM terminated the
			// run, so the parent cause classifies it — the parent's deadline
			// or cancellation is always cancellation, and only this run's
			// own configured timeout is a timeout — whatever exit status the
			// trap or kill then produced.
			delivered := terminateProcess(cmd, waitDone)
			waitErr = <-done
			if delivered {
				body := capture.Format()
				if parent.Err() != nil {
					output := "command cancelled"
					if body != "" {
						output += "\n" + body
					}
					return body, &ExitError{
						Output:    output,
						ExitCode:  -1,
						Cancelled: true,
					}
				}
				return body, &ExitError{
					Output:   fmt.Sprintf("Error: Exit code -1 (timeout)\n%s", body),
					ExitCode: -1,
				}
			}
			// An already-finished command keeps its real result: the shared
			// settlement below applies the Wait outcome unchanged.
		}
	}

	// The Wait-returned point: a completed command keeps its real result —
	// a deadline or cancellation observed after this point never rewrites it.
	output := capture.Format()

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			return output, &ExitError{
				Output:   fmt.Sprintf("Error: Exit code %d\n%s", code, output),
				ExitCode: code,
			}
		}
		return output, fmt.Errorf("run_command: %w", waitErr)
	}

	if output == "" {
		return "(No output)", nil
	}
	return output, nil
}

// terminateProcess terminates the command's process group: SIGTERM, a
// 500ms grace period, then SIGKILL. It reports whether the SIGTERM was
// delivered — a failed delivery (ESRCH) proves the process group was
// already gone, so the command finished before the kill could act.
func terminateProcess(cmd *exec.Cmd, done <-chan struct{}) bool {
	if cmd.Process == nil {
		return false
	}
	// Send SIGTERM to process group.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return false
	}

	// Wait 500ms grace period.
	select {
	case <-done:
		return true
	case <-time.After(500 * time.Millisecond):
		// SIGKILL if still running.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return true
}
