package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

func TestRunCommandNormalization(t *testing.T) {
	byID := directTools(t.TempDir(), nil)
	tc := callToolContext(t.TempDir(), runtime.ToolConstraints{})
	runCommand := byID["run_command"]

	t.Run("applies the default timeout and strips private fields", func(t *testing.T) {
		raw, err := normalize(t, runCommand, tc, "run_command", `{"command":"ls","_lightcode_receipt":"x"}`)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("normalized bytes: %v", err)
		}
		if args["command"] != "ls" || args["timeout"] != float64(120) || args["background"] != false {
			t.Fatalf("normalized = %s, want the command with the default 120 timeout and background false", raw)
		}
		if _, ok := args["_lightcode_receipt"]; ok {
			t.Fatal("normalized arguments retained a private field")
		}
	})

	t.Run("background booleans are accepted and non-booleans are rejected", func(t *testing.T) {
		raw, err := normalize(t, runCommand, tc, "run_command", `{"command":"ls","background":true}`)
		if err != nil {
			t.Fatalf("background true: %v", err)
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("normalized bytes: %v", err)
		}
		if args["background"] != true || args["timeout"] != float64(0) {
			t.Fatalf("normalized = %s, want background true with the sub-one timeout 0", raw)
		}
		raw, err = normalize(t, runCommand, tc, "run_command", `{"command":"ls","background":false,"timeout":5}`)
		if err != nil {
			t.Fatalf("background false: %v", err)
		}
		args = nil
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("normalized bytes: %v", err)
		}
		if args["background"] != false || args["timeout"] != float64(5) {
			t.Fatalf("normalized = %s, want background false with the foreground timeout 5", raw)
		}
		raw, err = normalize(t, runCommand, tc, "run_command", `{"command":"ls","background":true,"timeout":7}`)
		if err != nil {
			t.Fatalf("background with override: %v", err)
		}
		args = nil
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("normalized bytes: %v", err)
		}
		if args["background"] != true || args["timeout"] != float64(7) {
			t.Fatalf("normalized = %s, want background true with the forwarded timeout 7", raw)
		}
		for _, args := range []string{
			`{"command":"ls","background":null}`,
			`{"command":"ls","background":0}`,
			`{"command":"ls","background":"later"}`,
		} {
			if _, err := normalize(t, runCommand, tc, "run_command", args); err == nil || !strings.Contains(err.Error(), "run_command: background must be a boolean") {
				t.Errorf("run_command normalized %s to %v, want the boolean rejection", args, err)
			}
		}
	})

	t.Run("timeout is a strict consumed integer within the duration bound", func(t *testing.T) {
		raw, err := normalize(t, runCommand, tc, "run_command", `{"command":"ls","timeout":5}`)
		if err != nil || !strings.Contains(string(raw), `"timeout":5`) {
			t.Fatalf("normalized = (%s, %v), want the override kept", raw, err)
		}
		for _, args := range []string{
			`{"command":"ls","timeout":1.5}`,
			`{"command":"ls","timeout":"many"}`,
			`{"command":"ls","timeout":true}`,
			`{"command":"ls","timeout":null}`,
			`{"command":"ls","timeout":9223372036854775808}`,
			`{"command":"ls","timeout":9223372037}`,
		} {
			if _, err := normalize(t, runCommand, tc, "run_command", args); err == nil {
				t.Errorf("run_command normalized %s, want rejection", args)
			}
		}
		// The default keeps sub-one and negative supplied values.
		raw, err = normalize(t, runCommand, tc, "run_command", `{"command":"ls","timeout":0}`)
		if err != nil || !strings.Contains(string(raw), `"timeout":120`) {
			t.Fatalf("normalized = (%s, %v), want the default for a sub-one override", raw, err)
		}
	})

	t.Run("empty command text keeps the retained argument error", func(t *testing.T) {
		for _, args := range []string{`{}`, `{"command":""}`, `{"command":null}`, `{"command":5}`} {
			if _, err := normalize(t, runCommand, tc, "run_command", args); err == nil || !strings.Contains(err.Error(), "run_command: command is required") {
				t.Errorf("run_command normalized %s to %v, want the retained required error", args, err)
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
		outcome := commandOutcome(ctx, "call-1", "kill -TERM $$", "", 0, defaultSettings(), t.TempDir(), nil)
		if outcome.Result.Status != model.ResultError || outcome.Result.Content != "Error: Exit code -1\n" {
			t.Fatalf("result = (%v, %q), want the real signal-death result as an error", outcome.Result.Status, outcome.Result.Content)
		}
	})

	t.Run("own timeout keeps its real result after the context cancels", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runForegroundCommandFn = func(ctx context.Context, command, dir string, timeoutSec, maxBytes, maxLineChars int, spillDir string, env []string) (string, error) {
			result, err := orig(ctx, command, dir, timeoutSec, maxBytes, maxLineChars, spillDir, env)
			cancel()
			return result, err
		}
		outcome := commandOutcome(ctx, "call-2", "sleep 5", "", 1, defaultSettings(), t.TempDir(), nil)
		if outcome.Result.Status != model.ResultError || !strings.HasPrefix(outcome.Result.Content, "Error: Exit code -1 (timeout)\n") {
			t.Fatalf("result = (%v, %q), want the retained timeout text as an error", outcome.Result.Status, outcome.Result.Content)
		}
	})
}
