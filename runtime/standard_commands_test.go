package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

func TestRunCommandNormalization(t *testing.T) {
	byID := directTools(t.TempDir())
	tc := callToolContext(t.TempDir(), ToolConstraints{})
	runCommand := byID["run_command"]

	t.Run("applies the default timeout and strips private fields", func(t *testing.T) {
		raw, err := normalize(t, runCommand, tc, "run_command", `{"command":"ls","_lightcode_receipt":"x"}`)
		testassert.NoError(t, err)
		var args map[string]any
		testassert.NoError(t, json.Unmarshal(raw, &args))
		testassert.Require(t, args["command"] == "ls" && args["timeout"] == float64(120) && args["background"] == false, "normalized = %s, want the command with the default 120 timeout and background false", raw)
		{
			_, ok := args["_lightcode_receipt"]
			testassert.Require(t, !ok, "normalized arguments retained a private field")
		}
	})

	t.Run("background booleans are accepted and non-booleans are rejected", func(t *testing.T) {
		raw, err := normalize(t, runCommand, tc, "run_command", `{"command":"ls","background":true}`)
		testassert.NoError(t, err)
		var args map[string]any
		testassert.NoError(t, json.Unmarshal(raw, &args))
		testassert.Require(t, args["background"] == true && args["timeout"] == float64(0), "normalized = %s, want background true with the sub-one timeout 0", raw)
		raw, err = normalize(t, runCommand, tc, "run_command", `{"command":"ls","background":false,"timeout":5}`)
		testassert.NoError(t, err)
		args = nil
		testassert.NoError(t, json.Unmarshal(raw, &args))
		testassert.Require(t, args["background"] == false && args["timeout"] == float64(5), "normalized = %s, want background false with the foreground timeout 5", raw)
		raw, err = normalize(t, runCommand, tc, "run_command", `{"command":"ls","background":true,"timeout":7}`)
		testassert.NoError(t, err)
		args = nil
		testassert.NoError(t, json.Unmarshal(raw, &args))
		testassert.Require(t, args["background"] == true && args["timeout"] == float64(7), "normalized = %s, want background true with the forwarded timeout 7", raw)
		for _, args := range []string{
			`{"command":"ls","background":null}`,
			`{"command":"ls","background":0}`,
			`{"command":"ls","background":"later"}`,
		} {
			{
				_, err := normalize(t, runCommand, tc, "run_command", args)
				testassert.Check(t, err != nil && strings.Contains(err.Error(), "run_command: background must be a boolean"), "run_command normalized %s to %v, want the boolean rejection", args, err)
			}
		}
	})

	t.Run("timeout is a strict consumed integer within the duration bound", func(t *testing.T) {
		raw, err := normalize(t, runCommand, tc, "run_command", `{"command":"ls","timeout":5}`)
		testassert.Require(t, err == nil && strings.Contains(string(raw), `"timeout":5`), "normalized = (%s, %v), want the override kept", raw, err)
		for _, args := range []string{
			`{"command":"ls","timeout":1.5}`,
			`{"command":"ls","timeout":"many"}`,
			`{"command":"ls","timeout":true}`,
			`{"command":"ls","timeout":null}`,
			`{"command":"ls","timeout":9223372036854775808}`,
			`{"command":"ls","timeout":9223372037}`,
		} {
			{
				_, err := normalize(t, runCommand, tc, "run_command", args)
				testassert.Check(t, err != nil, "run_command normalized %s, want rejection", args)
			}
		}
		// The default keeps sub-one and negative supplied values.
		raw, err = normalize(t, runCommand, tc, "run_command", `{"command":"ls","timeout":0}`)
		testassert.Require(t, err == nil && strings.Contains(string(raw), `"timeout":120`), "normalized = (%s, %v), want the default for a sub-one override", raw, err)
	})

	t.Run("empty command text keeps the retained argument error", func(t *testing.T) {
		for _, args := range []string{`{}`, `{"command":""}`, `{"command":null}`, `{"command":5}`} {
			{
				_, err := normalize(t, runCommand, tc, "run_command", args)
				testassert.Check(t, err != nil && strings.Contains(err.Error(), "run_command: command is required"), "run_command normalized %s to %v, want the retained required error", args, err)
			}
		}
	})
}

// TestCommandOutcomeClassifiesByDeliveredCause pins the causal settlement of
// the runner's settled ExitError shapes: the classification reads the
// delivered cause carried on the error, never the context after the runner
// has settled the real result. The seam cancels the context inside the seam
// after the runner returns — the exact post-settlement window.
func TestCommandOutcomeClassifiesByDeliveredCause(t *testing.T) {
	orig := runForegroundCommandFn
	defer func() { runForegroundCommandFn = orig }()

	t.Run("external-signal death keeps its real result after the context cancels", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runForegroundCommandFn = func(ctx context.Context, command, dir string, timeoutSec, maxBytes, maxLineChars int, spillDir string, env []string) (string, error) {
			result, err := orig(ctx, command, dir, timeoutSec, maxBytes, maxLineChars, spillDir, env)
			cancel()
			return result, err
		}
		outcome := commandOutcome(ctx, "call-1", "kill -TERM $$", "", 0, defaultToolSettings(), t.TempDir(), nil)
		testassert.Require(t, outcome.Result.Status == model.ResultError && outcome.Result.Content == "Error: Exit code -1\n", "result = (%v, %q), want the real signal-death result as an error", outcome.Result.Status, outcome.Result.Content)
	})

	t.Run("own timeout keeps its real result after the context cancels", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runForegroundCommandFn = func(ctx context.Context, command, dir string, timeoutSec, maxBytes, maxLineChars int, spillDir string, env []string) (string, error) {
			result, err := orig(ctx, command, dir, timeoutSec, maxBytes, maxLineChars, spillDir, env)
			cancel()
			return result, err
		}
		outcome := commandOutcome(ctx, "call-2", "sleep 5", "", 1, defaultToolSettings(), t.TempDir(), nil)
		testassert.Require(t, outcome.Result.Status == model.ResultError && strings.HasPrefix(outcome.Result.Content, "Error: Exit code -1 (timeout)\n"), "result = (%v, %q), want the retained timeout text as an error", outcome.Result.Status, outcome.Result.Content)
	})
}
