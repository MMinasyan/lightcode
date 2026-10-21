package protocol_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.NoError(t, err)
	generated := filepath.Join(t.TempDir(), "protocol.gen.go")
	cmd := exec.Command("go", "tool", "oapi-codegen",
		"--config", "protocol/oapi-codegen.yaml",
		"-o", generated,
		"protocol/openapi.yaml")
	cmd.Dir = ".."
	{
		out, err := cmd.CombinedOutput()
		testassert.Require(t, err == nil, "regenerating protocol contracts: %v\n%s", err, out)
	}
	regenerated, err := os.ReadFile(generated)
	testassert.NoError(t, err)
	testassert.Require(t, bytes.Equal(committed, regenerated), "committed protocol/protocol.gen.go is stale: regenerate with make generate-protocol (%d committed bytes, %d regenerated)", len(committed), len(regenerated))
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
		{
			out, err := cmd.CombinedOutput()
			testassert.Require(t, err == nil, "regenerating native bindings: %v\n%s", err, out)
		}
		return readArtifact(t, source), readArtifact(t, contract)
	}
	source, contract := generate(t)
	againSource, againContract := generate(t)
	testassert.Require(t, bytes.Equal(source, againSource) && bytes.Equal(contract, againContract), "the native bindings generator is not deterministic")
	testassert.Require(t, bytes.Equal(committedSource, source), "committed internal/clientops/operations.gen.go is stale: regenerate with make generate-protocol (%d committed bytes, %d regenerated)", len(committedSource), len(source))
	testassert.Require(t, bytes.Equal(committedContract, contract), "committed frontend/src/generated/native-invocations.json is stale: regenerate with make generate-protocol (%d committed bytes, %d regenerated)", len(committedContract), len(contract))
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
	testassert.Require(t, err == nil, "reading %s: %v", path, err)
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
	testassert.NoError(t, err)

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
			testassert.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			testassert.NoError(t, os.WriteFile(path, []byte("generated"), 0o644))
		}
		// Ignored output the gates must never flag: the ignored Wails runtime
		// tree and an ignored sibling inside the checked bindings directory.
		testassert.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("frontend/client-bindings/wailsjs/runtime/\n*.ignored\n"), 0o644))
		for _, file := range []string{
			"frontend/client-bindings/wailsjs/runtime/runtime.js",
			"frontend/client-bindings/wailsjs/go/desktopclient/Host.js.ignored",
		} {
			path := filepath.Join(dir, filepath.FromSlash(file))
			testassert.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			testassert.NoError(t, os.WriteFile(path, []byte("ignored"), 0o644))
		}
		{
			out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput()
			testassert.Require(t, err == nil, "fixture git init: %v\n%s", err, out)
		}
		{
			out, err := exec.Command("git", "-C", dir, "add", ".").CombinedOutput()
			testassert.Require(t, err == nil, "fixture git add: %v\n%s", err, out)
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
		testassert.NoError(t, run(t, dir, protocolPaths...))
		testassert.NoError(t, run(t, dir, wailsPaths...))
	})

	t.Run("no path arguments fail", func(t *testing.T) {
		{
			err := run(t, fixture(t))
			testassert.Require(t, err != nil, "gate accepted an invocation without path arguments")
		}
	})

	t.Run("modified tracked output fails", func(t *testing.T) {
		for _, output := range outputs {
			dir := fixture(t)
			path := filepath.Join(dir, filepath.FromSlash(output.path))
			testassert.NoError(t, os.WriteFile(path, []byte("hand-edited"), 0o644))
			{
				err := run(t, dir, output.paths...)
				testassert.Require(t, err != nil, "gate accepted a modified tracked artifact: %s", output.path)
			}
		}
	})

	t.Run("deleted tracked output fails", func(t *testing.T) {
		for _, output := range outputs {
			dir := fixture(t)
			path := filepath.Join(dir, filepath.FromSlash(output.path))
			testassert.NoError(t, os.Remove(path))
			{
				err := run(t, dir, output.paths...)
				testassert.Require(t, err != nil, "gate accepted a deleted tracked artifact: %s", output.path)
			}
		}
	})

	t.Run("new untracked generated file fails", func(t *testing.T) {
		for _, output := range outputs {
			dir := fixture(t)
			// The staged artifact becomes untracked output again.
			{
				out, err := exec.Command("git", "-C", dir, "rm", "--cached", "-q", "--", output.path).CombinedOutput()
				testassert.Require(t, err == nil, "unstaging fixture artifact %s: %v\n%s", output.path, err, out)
			}
			{
				err := run(t, dir, output.paths...)
				testassert.Require(t, err != nil, "gate accepted a newly untracked artifact: %s", output.path)
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
			{
				err := os.WriteFile(path, []byte("generated"), 0o644)
				testassert.Require(t, err == nil, "emitting untracked fixture artifact %s: %v", output.path, err)
			}
			{
				err := run(t, dir, output.paths...)
				testassert.Require(t, err != nil, "gate accepted a newly emitted untracked artifact: %s", output.path)
			}
		}
	})

	t.Run("output outside the supplied paths passes", func(t *testing.T) {
		dir := fixture(t)
		testassert.NoError(t, os.WriteFile(filepath.Join(dir, "protocol", "protocol.gen.go"), []byte("hand-edited"), 0o644))
		testassert.NoError(t, run(t, dir, wailsPaths...))
	})

	t.Run("unrelated untracked file passes", func(t *testing.T) {
		dir := fixture(t)
		testassert.NoError(t, os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("junk"), 0o644))
		testassert.NoError(t, run(t, dir, protocolPaths...))
	})

	t.Run("ignored output passes", func(t *testing.T) {
		testassert.NoError(t, run(t, fixture(t), wailsPaths...))
	})
}

// The delivery gates and the tagged producer command the workflows must wire.
const (
	protocolGateRun = "make check-protocol"
	bindingsGateRun = "make check-wails-bindings"
	wailsBuildRun   = "wails build -tags webkit2_41"
)

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
	testassert.Require(t, err == nil, "reading workflow %s: %v", path, err)
	var workflow workflowFile
	{
		err := yaml.Unmarshal(data, &workflow)
		testassert.Require(t, err == nil, "decoding workflow %s: %v", path, err)
	}
	return workflow
}

// parsedSteps returns one workflow job's parsed steps, failing the test when
// the job is missing.
func parsedSteps(t *testing.T, path, job string) []workflowStep {
	t.Helper()
	workflow := parseWorkflow(t, path)
	parsed, ok := workflow.Jobs[job]
	testassert.Require(t, ok, "workflow %s has no %s job", path, job)
	return parsed.Steps
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

// checkDeliveryGates checks the fixed delivery-gate wiring in one job's steps
// and returns the first violation: make check-protocol and
// make check-wails-bindings must be exact, unconditional run steps, the tagged
// Wails build must be an unconditional step before the binding check, and both
// gates must precede every Package release assets or Upload release assets step
// the job declares. The protocol check has no ordering constraint against the
// Wails build, and a job without publication steps (Test) skips that last rule.
func checkDeliveryGates(steps []workflowStep) error {
	wails := findContainingRunStep(steps, wailsBuildRun)
	if wails < 0 {
		return fmt.Errorf("missing the tagged Wails build command")
	}
	if bypass := stepBypass(steps[wails]); bypass != "" {
		return fmt.Errorf("Wails build is %s", bypass)
	}
	bindings := findRunStep(steps, bindingsGateRun)
	if bindings < 0 {
		return fmt.Errorf("missing the exact run step %s", bindingsGateRun)
	}
	if bypass := stepBypass(steps[bindings]); bypass != "" {
		return fmt.Errorf("%s is %s", bindingsGateRun, bypass)
	}
	if bindings < wails {
		return fmt.Errorf("%s runs before the Wails build", bindingsGateRun)
	}
	protocol := findRunStep(steps, protocolGateRun)
	if protocol < 0 {
		return fmt.Errorf("missing the exact run step %s", protocolGateRun)
	}
	if bypass := stepBypass(steps[protocol]); bypass != "" {
		return fmt.Errorf("%s is %s", protocolGateRun, bypass)
	}
	for _, publication := range []string{"Package release assets", "Upload release assets"} {
		index := findNamedStep(steps, publication)
		if index < 0 {
			continue
		}
		if protocol > index {
			return fmt.Errorf("%s does not precede %s", protocolGateRun, publication)
		}
		if bindings > index {
			return fmt.Errorf("%s does not precede %s", bindingsGateRun, publication)
		}
	}
	return nil
}

// TestDeliveryGateWorkflowWiring parses the repository's Test and Release
// workflows and requires the delivery gates to stay wired to their real
// producers: make check-protocol and make check-wails-bindings are exact,
// unconditional run steps, the binding check follows the tagged Wails build,
// and the Release checks both precede packaging and upload. Neither workflow
// constrains the protocol check against the Wails build, and the Release
// workflow must still declare its two publication steps. Mutation rows prove
// removed, conditionally skipped, allowed-to-fail, suppressed and reordered
// gates are rejected, and that a protocol check before the build is accepted
// in both workflows. This verifies repository wiring, not GitHub branch
// protection or hosted execution.
func TestDeliveryGateWorkflowWiring(t *testing.T) {
	testPath := filepath.Join("..", ".github", "workflows", "test.yml")
	releasePath := filepath.Join("..", ".github", "workflows", "release.yml")

	testSteps := parsedSteps(t, testPath, "test")
	{
		err := checkDeliveryGates(testSteps)
		testassert.Check(t, err == nil, "test workflow: %v", err)
	}
	releaseSteps := parsedSteps(t, releasePath, "build")
	{
		err := checkDeliveryGates(releaseSteps)
		testassert.Check(t, err == nil, "release workflow: %v", err)
	}
	for _, publication := range []string{"Package release assets", "Upload release assets"} {
		testassert.Check(t, findNamedStep(releaseSteps, publication) >= 0, "release workflow is missing the %s step", publication)
	}

	// protocolBeforeWails moves the exact protocol gate step to just before the
	// tagged Wails build: the order both workflows must accept.
	protocolBeforeWails := func(steps []workflowStep) []workflowStep {
		from := findRunStep(steps, protocolGateRun)
		to := findContainingRunStep(steps, wailsBuildRun)
		step := steps[from]
		steps = slices.Delete(steps, from, from+1)
		if from < to {
			to--
		}
		return slices.Insert(steps, to, step)
	}
	allowedToFail := true

	for _, mutation := range []struct {
		name   string
		path   string
		job    string
		accept bool
		mutate func([]workflowStep) []workflowStep
	}{
		{"protocol gate before the Wails build in Test is accepted", testPath, "test", true, protocolBeforeWails},
		{"protocol gate before the Wails build in Release is accepted", releasePath, "build", true, protocolBeforeWails},
		{"removed binding gate", testPath, "test", false, func(steps []workflowStep) []workflowStep {
			index := findRunStep(steps, bindingsGateRun)
			return slices.Delete(steps, index, index+1)
		}},
		{"conditional binding gate", testPath, "test", false, func(steps []workflowStep) []workflowStep {
			steps[findRunStep(steps, bindingsGateRun)].If = "${{ false }}"
			return steps
		}},
		{"continue-on-error binding gate", testPath, "test", false, func(steps []workflowStep) []workflowStep {
			steps[findRunStep(steps, bindingsGateRun)].ContinueOnError = &allowedToFail
			return steps
		}},
		{"suppressed binding gate", testPath, "test", false, func(steps []workflowStep) []workflowStep {
			steps[findRunStep(steps, bindingsGateRun)].Run += " || true"
			return steps
		}},
		{"removed protocol gate", testPath, "test", false, func(steps []workflowStep) []workflowStep {
			index := findRunStep(steps, protocolGateRun)
			return slices.Delete(steps, index, index+1)
		}},
		{"binding gate before the Wails build", testPath, "test", false, func(steps []workflowStep) []workflowStep {
			bindings, wails := findRunStep(steps, bindingsGateRun), findContainingRunStep(steps, wailsBuildRun)
			steps[bindings], steps[wails] = steps[wails], steps[bindings]
			return steps
		}},
		{"removed release binding gate", releasePath, "build", false, func(steps []workflowStep) []workflowStep {
			index := findRunStep(steps, bindingsGateRun)
			return slices.Delete(steps, index, index+1)
		}},
		{"continue-on-error release protocol gate", releasePath, "build", false, func(steps []workflowStep) []workflowStep {
			steps[findRunStep(steps, protocolGateRun)].ContinueOnError = &allowedToFail
			return steps
		}},
		{"suppressed release protocol gate", releasePath, "build", false, func(steps []workflowStep) []workflowStep {
			steps[findRunStep(steps, protocolGateRun)].Run += " || true"
			return steps
		}},
		{"release binding gate after packaging", releasePath, "build", false, func(steps []workflowStep) []workflowStep {
			index := findRunStep(steps, bindingsGateRun)
			step := steps[index]
			return append(slices.Delete(steps, index, index+1), step)
		}},
		{"release binding gate before the Wails build", releasePath, "build", false, func(steps []workflowStep) []workflowStep {
			bindings, wails := findRunStep(steps, bindingsGateRun), findContainingRunStep(steps, wailsBuildRun)
			steps[bindings], steps[wails] = steps[wails], steps[bindings]
			return steps
		}},
		{"release Wails build allowed to fail", releasePath, "build", false, func(steps []workflowStep) []workflowStep {
			steps[findContainingRunStep(steps, wailsBuildRun)].ContinueOnError = &allowedToFail
			return steps
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			steps := mutation.mutate(parsedSteps(t, mutation.path, mutation.job))
			err := checkDeliveryGates(steps)
			if mutation.accept {
				testassert.NoError(t, err)
				return
			}
			testassert.Require(t, err != nil, "mutated workflow was accepted")
		})
	}
}
