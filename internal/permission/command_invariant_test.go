package permission

import "testing"

func TestRunCommandEffectiveDenyCannotBeHidden(t *testing.T) {
	rules := Rules{
		Allow: []string{"run_command(*)"},
		Deny:  []string{"run_command(rm *)"},
	}
	for _, command := range extractedRMCommands() {
		t.Run(command, func(t *testing.T) {
			assertEvaluateDecision(t, rules, command, DecisionDeny)
		})
	}
}

func TestRunCommandLocalPrecedenceUsesEffectiveAnalysis(t *testing.T) {
	for _, command := range extractedRMCommands() {
		t.Run("ask/"+command, func(t *testing.T) {
			got := Check(Rules{Ask: []string{"run_command(rm *)"}}, Rules{Allow: []string{"run_command(*)"}}, "run_command", command, "/project", "/home/user", "/project")
			if got != DecisionAsk {
				t.Fatalf("Check(%q) = %d, want DecisionAsk", command, got)
			}
		})
		t.Run("deny/"+command, func(t *testing.T) {
			got := Check(Rules{Deny: []string{"run_command(rm *)"}}, Rules{Allow: []string{"run_command(*)"}}, "run_command", command, "/project", "/home/user", "/project")
			if got != DecisionDeny {
				t.Fatalf("Check(%q) = %d, want DecisionDeny", command, got)
			}
		})
	}
}

func TestRunCommandUnknownExecutionSemanticsAsk(t *testing.T) {
	rules := Rules{Allow: []string{"run_command(*)"}}
	for _, command := range askOnlyShellForms() {
		t.Run(command, func(t *testing.T) {
			assertEvaluateDecision(t, rules, command, DecisionAsk)
		})
	}
}

func TestRunCommandCheckFailsClosedBeforeGlobalAllow(t *testing.T) {
	global := Rules{Allow: []string{"run_command(*)"}}
	for _, command := range askOnlyShellForms() {
		t.Run("global/"+command, func(t *testing.T) {
			got := Check(Rules{}, global, "run_command", command, "/project", "/home/user", "/project")
			if got != DecisionAsk {
				t.Fatalf("Check(%q) = %d, want DecisionAsk", command, got)
			}
		})
		t.Run("local-broad/"+command, func(t *testing.T) {
			got := Check(Rules{Allow: []string{"run_command(*)"}}, global, "run_command", command, "/project", "/home/user", "/project")
			if got != DecisionAsk {
				t.Fatalf("Check with local broad allow(%q) = %d, want DecisionAsk", command, got)
			}
		})
	}
}

func TestRunCommandExistingRawRulesAndSafeFDDupStillWork(t *testing.T) {
	assertEvaluateDecision(t, Rules{Allow: []string{"run_command(npm run build)"}}, "npm run build", DecisionAllow)
	assertEvaluateDecision(t, Rules{Allow: []string{"run_command(echo *)"}}, "echo ok 2>&1", DecisionAllow)
	assertEvaluateDecision(t, Rules{Allow: []string{"run_command(echo *)"}}, "echo ok > file", DecisionAsk)
}

func extractedRMCommands() []string {
	return []string{
		`r\m -rf x`,
		"VAR=1 rm -rf x",
		"VAR=1 VAR2=2 rm -rf x",
		"command rm -rf x",
		"command -- rm -rf x",
		"env VAR=1 rm -rf x",
		"sh -c 'rm -rf x'",
		"bash -c 'rm -rf x'",
		"dash -c 'rm -rf x'",
		"zsh -c 'rm -rf x'",
		"env sh -c 'rm -rf x'",
		"env bash -c 'rm -rf x'",
		"env dash -c 'rm -rf x'",
		"env zsh -c 'rm -rf x'",
	}
}

func askOnlyShellForms() []string {
	return []string{
		"busybox sh -c 'rm -rf x'",
		"eval rm -rf x",
		"exec rm -rf x",
		"time rm -rf x",
		". ./script.sh",
		"source ./script.sh",
		"builtin rm -rf x",
		"trap 'rm -rf x' EXIT",
		"nohup rm -rf x",
		"nice rm -rf x",
		"timeout 1 rm -rf x",
		"setsid rm -rf x",
		"chroot /tmp rm -rf x",
		"unshare rm -rf x",
		"sudo rm -rf x",
		"doas rm -rf x",
		"env -i rm -rf x",
		"env -u PATH rm -rf x",
		"env -C /tmp rm -rf x",
		"env -S 'rm -rf x'",
		"command -p rm -rf x",
		"(rm -rf x)",
		"{ rm -rf x; }",
		"! rm -rf x",
		"if true; then rm -rf x; fi",
		`for f in x; do rm -rf "$f"; done`,
		"while true; do rm -rf x; done",
		"case x in x) rm -rf x;; esac",
		"echo $(rm -rf x)",
		"echo `rm -rf x`",
		"cat <(rm -rf x)",
	}
}

func assertEvaluateDecision(t *testing.T, rules Rules, command string, want Decision) {
	t.Helper()
	got := Evaluate(rules, "run_command", command, "/project", "/home/user", "/project")
	if got != want {
		t.Fatalf("Evaluate(%q) = %d, want %d", command, got, want)
	}
}
