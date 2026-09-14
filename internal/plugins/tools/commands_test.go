package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/tool"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/runtime"
)

func TestRunCommandNormalization(t *testing.T) {
	byID := openTools(t, t.TempDir())
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
		if args["command"] != "ls" || args["timeout"] != float64(120) {
			t.Fatalf("normalized = %s, want the command with the default 120 timeout", raw)
		}
		if _, ok := args["_lightcode_receipt"]; ok {
			t.Fatal("normalized arguments retained a private field")
		}
	})

	t.Run("rejects any present background member", func(t *testing.T) {
		for _, args := range []string{
			`{"command":"ls","background":true}`,
			`{"command":"ls","background":false}`,
			`{"command":"ls","background":null}`,
			`{"command":"ls","background":0}`,
			`{"command":"ls","background":"later"}`,
		} {
			if _, err := normalize(t, runCommand, tc, "run_command", args); err == nil {
				t.Errorf("run_command normalized %s, want rejection", args)
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

func TestRunCommandPrepareTargets(t *testing.T) {
	dataDir := t.TempDir()
	byID := openTools(t, dataDir)
	ws := t.TempDir()

	t.Run("conclusive simple commands declare one target per segment", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{}), "run_command", `{"command":"echo one; echo two && echo three"}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want an executor plan", plan)
		}
		if len(plan.Permissions) != 3 {
			t.Fatalf("pairs = %+v, want one command.run pair per segment", plan.Permissions)
		}
		want := []string{"echo one", "echo two", "echo three"}
		for i, pair := range plan.Permissions {
			if pair.Permission != "command.run" || pair.Target != want[i] {
				t.Fatalf("pair %d = %+v, want command.run on %q", i, pair, want[i])
			}
		}
	})

	t.Run("everything else falls back to the one complete target", func(t *testing.T) {
		for _, tc := range []struct{ args, want string }{
			{`{"command":"printf ok # ; rm -rf scratch"}`, "printf ok # ; rm -rf scratch"},
			{`{"command":"echo a > b"}`, "echo a > b"},
			{`{"command":"cat <<EOF"}`, "cat <<EOF"},
			{`{"command":"FOO=bar ls"}`, "FOO=bar ls"},
			{`{"command":"echo a &&"}`, "echo a &&"},
			// Leading ASCII blanks/newlines are trimmed and every remaining
			// byte — the heredoc body and the trailing newline — preserved.
			{"{\"command\":\"  \\ncat <<EOF\\nbody\\nEOF\\n\"}", "cat <<EOF\nbody\nEOF\n"},
		} {
			plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{}), "run_command", tc.args)
			if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "command.run" {
				t.Fatalf("%s: pairs = %+v, want the single complete-command target", tc.args, plan.Permissions)
			}
			if plan.Permissions[0].Target != tc.want {
				t.Fatalf("%s: target = %q, want %q", tc.args, plan.Permissions[0].Target, tc.want)
			}
		}
	})

	t.Run("all-blank text takes the whole-command fallback with the original input", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{}), "run_command", `{"command":"   "}`)
		if len(plan.Permissions) != 1 || plan.Permissions[0].Target != "   " {
			t.Fatalf("pairs = %+v, want the unchanged original blank input as the one target", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "(No output)" {
			t.Fatalf("result = %+v, want the retained no-op run", outcome.Result)
		}
	})

	t.Run("empty command text is an immediate argument error", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{}), "run_command", `{"command":""}`)
		if plan.Execute != nil || plan.Immediate == nil {
			t.Fatalf("plan = %+v, want one immediate outcome", plan)
		}
		if plan.Immediate.Result.Status != model.ResultError || plan.Immediate.Result.Content != "run_command: command is required" {
			t.Fatalf("immediate = %+v, want the retained required error", plan.Immediate)
		}
	})

	t.Run("readonly declares the rewritten command and executes it", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{Readonly: true}), "run_command", `{"command":"git status --short"}`)
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "command.run" {
			t.Fatalf("pairs = %+v, want one command.run pair", plan.Permissions)
		}
		target := plan.Permissions[0].Target
		if !strings.HasPrefix(target, "git --no-pager --no-optional-locks") || strings.Contains(target, "status --short") == false {
			t.Fatalf("target = %q, want the rewritten read-only git command", target)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError {
			t.Fatalf("result = %+v, want the git failure settled as an error result", outcome.Result)
		}
	})

	t.Run("readonly plain command runs in the workspace", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{Readonly: true}), "run_command", `{"command":"pwd"}`)
		if len(plan.Permissions) != 1 || plan.Permissions[0].Target != "pwd" {
			t.Fatalf("pairs = %+v, want the unchanged pwd target", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || strings.TrimSpace(outcome.Result.Content) != ws {
			t.Fatalf("result = %+v, want pwd in the workspace", outcome.Result)
		}
	})

	t.Run("readonly rejection is an immediate error with the fixed text", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{Readonly: true}), "run_command", `{"command":"curl example.com"}`)
		if plan.Execute != nil || plan.Immediate == nil {
			t.Fatalf("plan = %+v, want one immediate outcome", plan)
		}
		if plan.Immediate.Result.Status != model.ResultError || !strings.Contains(plan.Immediate.Result.Content, "read-only agent") || strings.Contains(plan.Immediate.Result.Content, "curl") {
			t.Fatalf("immediate = %+v, want the fixed rejection that never echoes the command", plan.Immediate)
		}
	})

	t.Run("readonly blank and empty commands settle the fixed rejection", func(t *testing.T) {
		for _, args := range []string{`{"command":""}`, `{"command":"   "}`, "{\"command\":\" \\n\\t \"}"} {
			plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{Readonly: true}), "run_command", args)
			if plan.Execute != nil || plan.Immediate == nil {
				t.Fatalf("%s: plan = %+v, want one immediate outcome", args, plan)
			}
			if plan.Immediate.Result.Status != model.ResultError || plan.Immediate.Result.Content != tool.ReadOnlyRunCommandRejected {
				t.Fatalf("%s: immediate = %+v, want the fixed rejection that never echoes the command", args, plan.Immediate)
			}
		}
	})

	t.Run("readonly ignores a model timeout override", func(t *testing.T) {
		// cat on a FIFO blocks until the test's writer opens it at two
		// seconds: honoring the 1s override would time out, while the
		// retained captured default lets the command finish.
		fifo := filepath.Join(ws, "pipe")
		if err := exec.Command("mkfifo", fifo).Run(); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
		go func() {
			time.Sleep(2 * time.Second)
			f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
			if err == nil {
				_, _ = f.WriteString("done")
				_ = f.Close()
			}
		}()
		start := time.Now()
		plan := prepare(t, byID["run_command"], callToolContext(ws, runtime.ToolConstraints{Readonly: true}), "run_command", `{"command":"cat pipe","timeout":1}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "done" {
			t.Fatalf("result = %+v, want the default timeout to let the command finish", outcome.Result)
		}
		if elapsed := time.Since(start); elapsed < 2*time.Second {
			t.Fatalf("elapsed %s, want the 1s override ignored in favor of the captured default", elapsed)
		}
	})
}

func TestRunCommandExecuteOutcomes(t *testing.T) {
	dataDir := t.TempDir()
	byID := openTools(t, dataDir)
	ws := t.TempDir()
	tc := callToolContext(ws, runtime.ToolConstraints{})

	t.Run("success carries the captured output", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"printf ok"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "ok" {
			t.Fatalf("result = %+v, want the command output", outcome.Result)
		}
	})

	t.Run("nonzero exit settles exactly as the legacy engine settles the ExitError", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"printf fail && exit 7"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError || outcome.Result.Content != "Error: Exit code 7\nfail" {
			t.Fatalf("result = %+v, want the engine's error settlement with the ExitError output", outcome.Result)
		}
	})

	t.Run("a configured timeout settles as an error result with the retained text", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"sleep 5","timeout":1}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultError || !strings.HasPrefix(outcome.Result.Content, "Error: Exit code -1 (timeout)\n") {
			t.Fatalf("result = %+v, want the retained timeout text as an error result", outcome.Result)
		}
	})

	t.Run("cancellation settles as an interrupted result with the retained text", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"sleep 5"}`)
		outcomeCh := make(chan struct {
			Status model.ToolResultStatus
			Body   string
		}, 1)
		go func() {
			outcome := plan.Execute(ctx)
			outcomeCh <- struct {
				Status model.ToolResultStatus
				Body   string
			}{outcome.Result.Status, outcome.Result.Content}
		}()
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case outcome := <-outcomeCh:
			if outcome.Status != model.ResultInterrupted || outcome.Body != "command cancelled" {
				t.Fatalf("outcome = (%v, %q), want the retained cancellation text as interrupted", outcome.Status, outcome.Body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled command did not return")
		}
	})

	t.Run("output spills under the calling session's output directory", func(t *testing.T) {
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"yes overflowing-output | head -c 20000"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("result = %+v, want success with the spill marker", outcome.Result)
		}
		wantPrefix := filepath.Join(dataDir, "code", testSessionID, "output", "cmd_output_")
		if !strings.Contains(outcome.Result.Content, "saved to: "+wantPrefix) {
			t.Fatalf("result = %q, want the spill marker under %q", outcome.Result.Content, wantPrefix)
		}
		marker := "saved to: "
		idx := strings.LastIndex(outcome.Result.Content, marker)
		path := outcome.Result.Content[idx+len(marker):]
		if end := strings.IndexAny(path, "]\n"); end >= 0 {
			path = path[:end]
		}
		data, err := os.ReadFile(strings.TrimSpace(path))
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		if len(data) != 20000 {
			t.Fatalf("spill size = %d, want the full 20000 output bytes", len(data))
		}
	})

	t.Run("the inherited environment and workspace cwd are retained", func(t *testing.T) {
		t.Setenv("LIGHTCODE_TOOLS_PLUGIN_TEST", "env-value")
		plan := prepare(t, byID["run_command"], tc, "run_command", `{"command":"printf '%s' \"$LIGHTCODE_TOOLS_PLUGIN_TEST\"; pwd"}`)
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess {
			t.Fatalf("result = %+v, want success", outcome.Result)
		}
		if !strings.Contains(outcome.Result.Content, "env-value") || !strings.Contains(outcome.Result.Content, ws) {
			t.Fatalf("result = %q, want the inherited env value and the workspace cwd", outcome.Result.Content)
		}
	})
}

func TestSleepTool(t *testing.T) {
	byID := openTools(t, t.TempDir())
	tc := callToolContext(t.TempDir(), runtime.ToolConstraints{})
	sleepTool := byID["sleep"]

	t.Run("normalization strips private fields, requires strict integers and clamps to 1..300", func(t *testing.T) {
		for _, args := range []string{
			`{"seconds":1.5}`,                  // fraction
			`{"seconds":"5"}`,                  // wrong type
			`{"seconds":true}`,                 // wrong type
			`{"seconds":null}`,                 // present null
			`null`,                             // whole-null arguments
			`{"seconds":99999999999999999999}`, // too large
		} {
			if _, err := normalize(t, sleepTool, tc, "sleep", args); err == nil {
				t.Errorf("sleep normalized %s, want rejection", args)
			}
		}
		raw, err := normalize(t, sleepTool, tc, "sleep", `{"seconds":2,"_lightcode_receipt":"x"}`)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if !strings.Contains(string(raw), `"seconds":2`) || strings.Contains(string(raw), "_lightcode_receipt") {
			t.Fatalf("normalized = %s, want the clamped value with private fields stripped", raw)
		}
		raw, err = normalize(t, sleepTool, tc, "sleep", `{"seconds":5000}`)
		if err != nil || !strings.Contains(string(raw), `"seconds":300`) {
			t.Fatalf("normalized = (%s, %v), want the 300 clamp", raw, err)
		}
		for _, args := range []string{`{}`, `{"seconds":0}`, `{"seconds":-5}`} {
			raw, err := normalize(t, sleepTool, tc, "sleep", args)
			if err != nil || !strings.Contains(string(raw), `"seconds":1`) {
				t.Fatalf("normalized = (%s, %v), want the retained default 1 for %s", raw, err, args)
			}
		}
	})

	t.Run("prepare declares the fixed sleep target", func(t *testing.T) {
		plan := prepare(t, sleepTool, tc, "sleep", `{"seconds":1}`)
		if plan.Immediate != nil || plan.Execute == nil {
			t.Fatalf("plan = %+v, want an executor plan", plan)
		}
		if len(plan.Permissions) != 1 || plan.Permissions[0].Permission != "sleep" || plan.Permissions[0].Target != "*" {
			t.Fatalf("pairs = %+v, want the fixed sleep target *", plan.Permissions)
		}
		outcome := plan.Execute(context.Background())
		if outcome.Result.Status != model.ResultSuccess || outcome.Result.Content != "Slept for 1 seconds." {
			t.Fatalf("result = %+v, want the retained sleep result", outcome.Result)
		}
	})

	t.Run("cancellation settles as an interrupted result", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		plan := prepare(t, sleepTool, tc, "sleep", `{"seconds":30}`)
		type outcome struct {
			status model.ToolResultStatus
			body   string
		}
		outcomeCh := make(chan outcome, 1)
		go func() {
			result := plan.Execute(ctx)
			outcomeCh <- outcome{status: result.Result.Status, body: result.Result.Content}
		}()
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case got := <-outcomeCh:
			if got.status != model.ResultInterrupted || got.body == "" {
				t.Fatalf("outcome = (%v, %q), want a nonempty interrupted result", got.status, got.body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled sleep did not return")
		}
	})
}
