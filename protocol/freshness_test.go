package protocol_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestGeneratedContractsFresh regenerates the Go protocol artifact from the
// committed schema with the committed generator configuration and compares
// the bytes against the committed protocol/protocol.gen.go: any hand edit or
// schema change without regeneration fails. The TypeScript artifact is not
// covered here — its regeneration needs the frontend toolchain and is
// gated by the make check-protocol target.
func TestGeneratedContractsFresh(t *testing.T) {
	// go test runs the test binary with the package directory as its working
	// directory, so the module root is exactly "..".
	committed, err := os.ReadFile(filepath.Join("..", "protocol", "protocol.gen.go"))
	if err != nil {
		t.Fatalf("reading committed generated contracts: %v", err)
	}
	generated := filepath.Join(t.TempDir(), "protocol.gen.go")
	cmd := exec.Command("go", "tool", "oapi-codegen",
		"--config", "protocol/oapi-codegen.yaml",
		"-o", generated,
		"protocol/openapi.yaml")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("regenerating protocol contracts: %v\n%s", err, out)
	}
	regenerated, err := os.ReadFile(generated)
	if err != nil {
		t.Fatalf("reading regenerated contracts: %v", err)
	}
	if !bytes.Equal(committed, regenerated) {
		t.Fatalf("committed protocol/protocol.gen.go is stale: regenerate with make generate-protocol (%d committed bytes, %d regenerated)", len(committed), len(regenerated))
	}
}

// TestGeneratedNativeBindingsFresh regenerates the native operation bindings
// and the test-only native invocation contract from the committed schema with
// the committed build command and compares both byte-for-byte against the
// committed artifacts. Two runs must agree, so a hand edit, a stale artifact
// or a nondeterministic generator fails here. The TypeScript artifact is not
// covered here — its regeneration needs the frontend toolchain and is gated by
// the make check-protocol target.
func TestGeneratedNativeBindingsFresh(t *testing.T) {
	committedSource := committedArtifact(t, "internal/clientops/operations.gen.go")
	committedContract := committedArtifact(t, "frontend/src/generated/native-invocations.json")
	generate := func(t *testing.T) ([]byte, []byte) {
		t.Helper()
		dir := t.TempDir()
		source := filepath.Join(dir, "operations.gen.go")
		contract := filepath.Join(dir, "native-invocations.json")
		cmd := exec.Command("go", "run", "./internal/protocolgen",
			"-schema", "protocol/openapi.yaml",
			"-out", source,
			"-native-contract-out", contract)
		cmd.Dir = ".."
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("regenerating native bindings: %v\n%s", err, out)
		}
		return readArtifact(t, source), readArtifact(t, contract)
	}
	source, contract := generate(t)
	againSource, againContract := generate(t)
	if !bytes.Equal(source, againSource) || !bytes.Equal(contract, againContract) {
		t.Fatal("the native bindings generator is not deterministic")
	}
	if !bytes.Equal(committedSource, source) {
		t.Fatalf("committed internal/clientops/operations.gen.go is stale: regenerate with make generate-protocol (%d committed bytes, %d regenerated)", len(committedSource), len(source))
	}
	if !bytes.Equal(committedContract, contract) {
		t.Fatalf("committed frontend/src/generated/native-invocations.json is stale: regenerate with make generate-protocol (%d committed bytes, %d regenerated)", len(committedContract), len(contract))
	}
}

// committedArtifact reads one committed artifact relative to the module root.
func committedArtifact(t *testing.T, rel string) []byte {
	t.Helper()
	return readArtifact(t, filepath.Join("..", filepath.FromSlash(rel)))
}

// readArtifact reads one artifact or fails the calling test.
func readArtifact(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

// TestGeneratedFileSetGate exercises the shared freshness shell check in a
// disposable Git fixture with explicit path arguments: staged tracked bytes
// matching the fixture state pass, modified or deleted tracked output fails,
// a newly emitted or unstaged artifact fails, ignored output under the
// supplied paths and unrelated untracked output do not. The fixture never
// commits and never touches Git configuration; the only Git writes anywhere
// in this test are init, add and one unstaging rm inside that disposable
// fixture.
func TestGeneratedFileSetGate(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "scripts", "check-generated-files.sh"))
	if err != nil {
		t.Fatalf("resolving the freshness script: %v", err)
	}

	// The two real invocations: make check-protocol names the four protocol
	// artifacts (directories included) and make check-wails-bindings names the
	// generated Wails bindings directory.
	protocolPaths := []string{
		"protocol/protocol.gen.go",
		"frontend/src/generated/protocol",
		"internal/clientops/operations.gen.go",
		"frontend/src/generated/native-invocations.json",
	}
	wailsPaths := []string{"frontend/client-bindings/wailsjs/go"}

	// Every checked output is a tracked file in the fixture, so the matching
	// case proves each artifact path passes under its real invocation.
	outputs := []struct {
		path  string
		paths []string
	}{
		{"protocol/protocol.gen.go", protocolPaths},
		{"frontend/src/generated/protocol/types.gen.ts", protocolPaths},
		{"internal/clientops/operations.gen.go", protocolPaths},
		{"frontend/src/generated/native-invocations.json", protocolPaths},
		{"frontend/client-bindings/wailsjs/go/desktopclient/Host.js", wailsPaths},
	}

	fixture := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		for _, output := range outputs {
			path := filepath.Join(dir, filepath.FromSlash(output.path))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("creating fixture directory: %v", err)
			}
			if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
				t.Fatalf("writing fixture artifact: %v", err)
			}
		}
		// Ignored output the gates must never flag: the ignored Wails runtime
		// tree and an ignored sibling inside the checked bindings directory.
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("frontend/client-bindings/wailsjs/runtime/\n*.ignored\n"), 0o644); err != nil {
			t.Fatalf("writing fixture ignore file: %v", err)
		}
		for _, file := range []string{
			"frontend/client-bindings/wailsjs/runtime/runtime.js",
			"frontend/client-bindings/wailsjs/go/desktopclient/Host.js.ignored",
		} {
			path := filepath.Join(dir, filepath.FromSlash(file))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("creating ignored fixture directory: %v", err)
			}
			if err := os.WriteFile(path, []byte("ignored"), 0o644); err != nil {
				t.Fatalf("writing ignored fixture file: %v", err)
			}
		}
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("fixture git init: %v\n%s", err, out)
		}
		if out, err := exec.Command("git", "-C", dir, "add", ".").CombinedOutput(); err != nil {
			t.Fatalf("fixture git add: %v\n%s", err, out)
		}
		return dir
	}

	run := func(t *testing.T, dir string, paths ...string) error {
		t.Helper()
		cmd := exec.Command("sh", append([]string{script}, paths...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("exit: %v\n%s", err, out)
		}
		return nil
	}

	t.Run("matching staged bytes pass", func(t *testing.T) {
		dir := fixture(t)
		if err := run(t, dir, protocolPaths...); err != nil {
			t.Fatalf("gate rejected matching staged protocol artifacts: %v", err)
		}
		if err := run(t, dir, wailsPaths...); err != nil {
			t.Fatalf("gate rejected matching staged Wails bindings: %v", err)
		}
	})

	t.Run("no path arguments fail", func(t *testing.T) {
		if err := run(t, fixture(t)); err == nil {
			t.Fatal("gate accepted an invocation without path arguments")
		}
	})

	t.Run("modified tracked output fails", func(t *testing.T) {
		for _, output := range outputs {
			dir := fixture(t)
			path := filepath.Join(dir, filepath.FromSlash(output.path))
			if err := os.WriteFile(path, []byte("hand-edited"), 0o644); err != nil {
				t.Fatalf("modifying fixture artifact: %v", err)
			}
			if err := run(t, dir, output.paths...); err == nil {
				t.Fatalf("gate accepted a modified tracked artifact: %s", output.path)
			}
		}
	})

	t.Run("deleted tracked output fails", func(t *testing.T) {
		for _, output := range outputs {
			dir := fixture(t)
			path := filepath.Join(dir, filepath.FromSlash(output.path))
			if err := os.Remove(path); err != nil {
				t.Fatalf("deleting fixture artifact: %v", err)
			}
			if err := run(t, dir, output.paths...); err == nil {
				t.Fatalf("gate accepted a deleted tracked artifact: %s", output.path)
			}
		}
	})

	t.Run("new untracked generated file fails", func(t *testing.T) {
		for _, output := range outputs {
			dir := fixture(t)
			// The staged artifact becomes untracked output again.
			if out, err := exec.Command("git", "-C", dir, "rm", "--cached", "-q", "--", output.path).CombinedOutput(); err != nil {
				t.Fatalf("unstaging fixture artifact %s: %v\n%s", output.path, err, out)
			}
			if err := run(t, dir, output.paths...); err == nil {
				t.Fatalf("gate accepted a newly untracked artifact: %s", output.path)
			}
		}
		for _, output := range []struct {
			path  string
			paths []string
		}{
			{"frontend/src/generated/protocol/client.gen.ts", protocolPaths},
			{"frontend/client-bindings/wailsjs/go/desktopclient/New.js", wailsPaths},
		} {
			dir := fixture(t)
			path := filepath.Join(dir, filepath.FromSlash(output.path))
			if err := os.WriteFile(path, []byte("generated"), 0o644); err != nil {
				t.Fatalf("emitting untracked fixture artifact %s: %v", output.path, err)
			}
			if err := run(t, dir, output.paths...); err == nil {
				t.Fatalf("gate accepted a newly emitted untracked artifact: %s", output.path)
			}
		}
	})

	t.Run("output outside the supplied paths passes", func(t *testing.T) {
		dir := fixture(t)
		if err := os.WriteFile(filepath.Join(dir, "protocol", "protocol.gen.go"), []byte("hand-edited"), 0o644); err != nil {
			t.Fatalf("modifying fixture artifact: %v", err)
		}
		if err := run(t, dir, wailsPaths...); err != nil {
			t.Fatalf("gate flagged output outside its path arguments: %v", err)
		}
	})

	t.Run("unrelated untracked file passes", func(t *testing.T) {
		dir := fixture(t)
		if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("junk"), 0o644); err != nil {
			t.Fatalf("writing unrelated fixture file: %v", err)
		}
		if err := run(t, dir, protocolPaths...); err != nil {
			t.Fatalf("gate rejected an unrelated untracked file: %v", err)
		}
	})

	t.Run("ignored output passes", func(t *testing.T) {
		if err := run(t, fixture(t), wailsPaths...); err != nil {
			t.Fatalf("gate flagged ignored output: %v", err)
		}
	})
}

// The delivery gates and the tagged producer command the workflows must wire.
const (
	protocolGateRun = "make check-protocol"
	bindingsGateRun = "make check-wails-bindings"
	wailsBuildRun   = "wails build -tags webkit2_41"
)

// deliveryGate is one required workflow gate: its exact run command and
// whether the tagged Wails build must precede it. Only the binding check
// consumes what the build produced; check-protocol regenerates on its own.
type deliveryGate struct {
	run        string
	afterWails bool
}

// workflowStep is the parsed subset of one GitHub Actions step the delivery
// gate assertions act on.
type workflowStep struct {
	Name            string `yaml:"name"`
	Run             string `yaml:"run"`
	If              string `yaml:"if"`
	ContinueOnError *bool  `yaml:"continue-on-error"`
}

// workflowJob and workflowFile are the parsed subset of the workflow files:
// only the steps of each job matter here.
type workflowJob struct {
	Steps []workflowStep `yaml:"steps"`
}

type workflowFile struct {
	Jobs map[string]*workflowJob `yaml:"jobs"`
}

// parseWorkflow reads and decodes one workflow file.
func parseWorkflow(t *testing.T, path string) workflowFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading workflow %s: %v", path, err)
	}
	var workflow workflowFile
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("decoding workflow %s: %v", path, err)
	}
	return workflow
}

// findRunStep returns the index of the first step whose trimmed run command is
// exactly want, or -1.
func findRunStep(steps []workflowStep, want string) int {
	for i, step := range steps {
		if strings.TrimSpace(step.Run) == want {
			return i
		}
	}
	return -1
}

// findContainingRunStep returns the index of the first step whose run command
// contains want, or -1.
func findContainingRunStep(steps []workflowStep, want string) int {
	for i, step := range steps {
		if strings.Contains(step.Run, want) {
			return i
		}
	}
	return -1
}

// findNamedStep returns the index of the first step named want, or -1.
func findNamedStep(steps []workflowStep, want string) int {
	for i, step := range steps {
		if step.Name == want {
			return i
		}
	}
	return -1
}

// stepBypass names the bypass on one step, or "" when it is unconditional.
func stepBypass(step workflowStep) string {
	if step.If != "" {
		return "conditionally skipped"
	}
	if step.ContinueOnError != nil {
		return "allowed to fail"
	}
	return ""
}

// deliveryGateProblems reports every delivery-gate wiring problem in one
// workflow job: each gate must exist as an exact, unconditional run step, a
// gate marked afterWails must follow the tagged Wails build, and each follower
// step (packaging and upload) must come after every gate. Job-level conditions
// are out of scope: the release build job's repository guard is not a step
// bypass.
func deliveryGateProblems(workflow workflowFile, job string, gates []deliveryGate, followers []string) []string {
	parsed, ok := workflow.Jobs[job]
	if !ok {
		return []string{"workflow has no " + job + " job"}
	}
	steps := parsed.Steps
	var problems []string
	wails := findContainingRunStep(steps, wailsBuildRun)
	if wails < 0 {
		problems = append(problems, "missing the tagged Wails build command")
	} else if bypass := stepBypass(steps[wails]); bypass != "" {
		problems = append(problems, "Wails build is "+bypass)
	}
	for _, gate := range gates {
		index := findRunStep(steps, gate.run)
		if index < 0 {
			problems = append(problems, "missing the exact run step "+gate.run)
			continue
		}
		if bypass := stepBypass(steps[index]); bypass != "" {
			problems = append(problems, gate.run+" is "+bypass)
			continue
		}
		if gate.afterWails && wails >= 0 && index < wails {
			problems = append(problems, gate.run+" runs before the Wails build")
		}
	}
	for _, follower := range followers {
		index := findNamedStep(steps, follower)
		if index < 0 {
			problems = append(problems, "missing the "+follower+" step")
			continue
		}
		for _, gate := range gates {
			if gateIndex := findRunStep(steps, gate.run); gateIndex >= 0 && gateIndex > index {
				problems = append(problems, gate.run+" does not precede "+follower)
			}
		}
	}
	return problems
}

// removeRunStep drops the step with the exact run command.
func removeRunStep(workflow *workflowFile, job, run string) {
	steps := workflow.Jobs[job].Steps
	if index := findRunStep(steps, run); index >= 0 {
		workflow.Jobs[job].Steps = append(steps[:index], steps[index+1:]...)
	}
}

// mutateRunStep applies mutate to the step with the exact run command.
func mutateRunStep(workflow *workflowFile, job, run string, mutate func(*workflowStep)) {
	steps := workflow.Jobs[job].Steps
	if index := findRunStep(steps, run); index >= 0 {
		mutate(&steps[index])
	}
}

// mutateContainingRunStep applies mutate to the step whose run command
// contains want.
func mutateContainingRunStep(workflow *workflowFile, job, want string, mutate func(*workflowStep)) {
	steps := workflow.Jobs[job].Steps
	if index := findContainingRunStep(steps, want); index >= 0 {
		mutate(&steps[index])
	}
}

// swapRunSteps swaps the exact run step with the containing run step.
func swapRunSteps(workflow *workflowFile, job, run, containing string) {
	steps := workflow.Jobs[job].Steps
	first, second := findRunStep(steps, run), findContainingRunStep(steps, containing)
	if first >= 0 && second >= 0 {
		steps[first], steps[second] = steps[second], steps[first]
	}
}

// moveRunStepToEnd moves the exact run step to the end of its job.
func moveRunStepToEnd(workflow *workflowFile, job, run string) {
	steps := workflow.Jobs[job].Steps
	if index := findRunStep(steps, run); index >= 0 {
		step := steps[index]
		workflow.Jobs[job].Steps = append(append(steps[:index], steps[index+1:]...), step)
	}
}

// moveRunStepBefore moves the exact run step to just before the containing run
// step.
func moveRunStepBefore(workflow *workflowFile, job, run, containing string) {
	steps := workflow.Jobs[job].Steps
	index, target := findRunStep(steps, run), findContainingRunStep(steps, containing)
	if index < 0 || target < 0 {
		return
	}
	step := steps[index]
	steps = append(steps[:index], steps[index+1:]...)
	if index < target {
		target--
	}
	workflow.Jobs[job].Steps = append(steps[:target], append([]workflowStep{step}, steps[target:]...)...)
}

// boolPtr returns a pointer to value for optional workflow keys.
func boolPtr(value bool) *bool {
	return &value
}

// TestDeliveryGateWorkflowWiring parses the repository's Test and Release
// workflows and requires the delivery gates to stay wired to their real
// producers: the post-build Wails binding check and the protocol freshness
// check are exact, unconditional run steps, the binding check follows the
// tagged Wails build (check-protocol is free to precede it in Test), and the
// release checks both follow the build and precede packaging and upload.
// Mutation rows prove removed, conditionally skipped, allowed-to-fail,
// suppressed and reordered gates are rejected, and that the accepted Test
// order with the protocol check before the build is not over-constrained.
// This verifies repository wiring, not GitHub branch protection or hosted
// execution.
func TestDeliveryGateWorkflowWiring(t *testing.T) {
	testPath := filepath.Join("..", ".github", "workflows", "test.yml")
	releasePath := filepath.Join("..", ".github", "workflows", "release.yml")
	// Only the binding check consumes the Wails build output; check-protocol
	// regenerates on its own and may run before the build in Test. Release
	// keeps both checks after the build.
	testGates := []deliveryGate{{protocolGateRun, false}, {bindingsGateRun, true}}
	releaseGates := []deliveryGate{{protocolGateRun, true}, {bindingsGateRun, true}}
	releaseFollowers := []string{"Package release assets", "Upload release assets"}

	if problems := deliveryGateProblems(parseWorkflow(t, testPath), "test", testGates, nil); len(problems) != 0 {
		t.Errorf("test workflow: %s", strings.Join(problems, "; "))
	}
	if problems := deliveryGateProblems(parseWorkflow(t, releasePath), "build", releaseGates, releaseFollowers); len(problems) != 0 {
		t.Errorf("release workflow: %s", strings.Join(problems, "; "))
	}

	for _, mutation := range []struct {
		name      string
		path      string
		job       string
		gates     []deliveryGate
		followers []string
		accepted  bool
		mutate    func(*workflowFile)
	}{
		{"protocol gate before the Wails build in Test is accepted", testPath, "test", testGates, nil, true, func(workflow *workflowFile) {
			moveRunStepBefore(workflow, "test", protocolGateRun, wailsBuildRun)
		}},
		{"protocol gate before the Wails build in Release is forbidden", releasePath, "build", releaseGates, releaseFollowers, false, func(workflow *workflowFile) {
			moveRunStepBefore(workflow, "build", protocolGateRun, wailsBuildRun)
		}},
		{"removed binding gate", testPath, "test", testGates, nil, false, func(workflow *workflowFile) {
			removeRunStep(workflow, "test", bindingsGateRun)
		}},
		{"conditional binding gate", testPath, "test", testGates, nil, false, func(workflow *workflowFile) {
			mutateRunStep(workflow, "test", bindingsGateRun, func(step *workflowStep) { step.If = "${{ false }}" })
		}},
		{"continue-on-error binding gate", testPath, "test", testGates, nil, false, func(workflow *workflowFile) {
			mutateRunStep(workflow, "test", bindingsGateRun, func(step *workflowStep) { step.ContinueOnError = boolPtr(true) })
		}},
		{"suppressed binding gate", testPath, "test", testGates, nil, false, func(workflow *workflowFile) {
			mutateRunStep(workflow, "test", bindingsGateRun, func(step *workflowStep) { step.Run += " || true" })
		}},
		{"removed protocol gate", testPath, "test", testGates, nil, false, func(workflow *workflowFile) {
			removeRunStep(workflow, "test", protocolGateRun)
		}},
		{"binding gate before the Wails build", testPath, "test", testGates, nil, false, func(workflow *workflowFile) {
			swapRunSteps(workflow, "test", bindingsGateRun, wailsBuildRun)
		}},
		{"removed release binding gate", releasePath, "build", releaseGates, releaseFollowers, false, func(workflow *workflowFile) {
			removeRunStep(workflow, "build", bindingsGateRun)
		}},
		{"continue-on-error release protocol gate", releasePath, "build", releaseGates, releaseFollowers, false, func(workflow *workflowFile) {
			mutateRunStep(workflow, "build", protocolGateRun, func(step *workflowStep) { step.ContinueOnError = boolPtr(true) })
		}},
		{"suppressed release protocol gate", releasePath, "build", releaseGates, releaseFollowers, false, func(workflow *workflowFile) {
			mutateRunStep(workflow, "build", protocolGateRun, func(step *workflowStep) { step.Run += " || true" })
		}},
		{"release binding gate after packaging", releasePath, "build", releaseGates, releaseFollowers, false, func(workflow *workflowFile) {
			moveRunStepToEnd(workflow, "build", bindingsGateRun)
		}},
		{"release binding gate before the Wails build", releasePath, "build", releaseGates, releaseFollowers, false, func(workflow *workflowFile) {
			swapRunSteps(workflow, "build", bindingsGateRun, wailsBuildRun)
		}},
		{"release Wails build allowed to fail", releasePath, "build", releaseGates, releaseFollowers, false, func(workflow *workflowFile) {
			mutateContainingRunStep(workflow, "build", wailsBuildRun, func(step *workflowStep) { step.ContinueOnError = boolPtr(true) })
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			workflow := parseWorkflow(t, mutation.path)
			mutation.mutate(&workflow)
			problems := deliveryGateProblems(workflow, mutation.job, mutation.gates, mutation.followers)
			if mutation.accepted {
				if len(problems) != 0 {
					t.Fatalf("accepted workflow mutation reported problems: %s", strings.Join(problems, "; "))
				}
				return
			}
			if len(problems) == 0 {
				t.Fatal("mutated workflow was accepted")
			}
		})
	}
}
