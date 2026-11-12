package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Syntheresis/melta/extension"
	"github.com/Syntheresis/melta/extension/worker"
	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/model"
	"github.com/Syntheresis/melta/protocol"
)

func buildFixture(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "fixture-worker")
	build := exec.Command("go", "build", "-o", binary, "./testdata/fixture")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building fixture: %v\n%s", err, output)
	}
	return binary
}

func TestFixtureWorkerActivateAndDescribeOverPipes(t *testing.T) {
	binary := buildFixture(t)
	command := exec.Command(binary)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("opening stdin: %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("opening stdout: %v", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("starting fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	writer := worker.NewWriter(stdin)
	reader := worker.NewReader(stdout)
	params, err := json.Marshal(worker.ActivateParams{ContractVersion: extension.ContractVersion})
	if err != nil {
		t.Fatalf("encoding activation params: %v", err)
	}
	if err := writer.WriteFrame(worker.Frame{Kind: worker.KindRequest, ID: "activate", Method: worker.MethodActivate, Params: params}); err != nil {
		t.Fatalf("writing activation: %v", err)
	}
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("reading activation: %v\nstderr: %s", err, stderr.String())
	}
	if frame.Error != nil {
		t.Fatalf("activation failed: %v", frame.Error)
	}
	var activation worker.ActivateResult
	if err := json.Unmarshal(frame.Result, &activation); err != nil {
		t.Fatalf("decoding activation: %v", err)
	}
	if activation.Declaration.ID != "fixture" || activation.Declaration.Version != "1.0.0" {
		t.Fatalf("activation declaration = %+v", activation.Declaration)
	}
	if len(activation.Declaration.ConfigSchema) == 0 {
		t.Fatal("fixture declaration has no config schema")
	}
	declaration := activation.Declaration.Value()
	effectful := false
	for _, capability := range declaration.Capabilities {
		if capability.Kind != extension.KindHook {
			continue
		}
		for _, hook := range capability.Hooks {
			if hook.Mode == extension.HookModeEffectful {
				effectful = true
			}
		}
	}
	if !effectful {
		t.Fatal("fixture declaration carries no effectful hook")
	}
	describes := map[string]extension.ToolDescribe{}
	for _, capability := range declaration.Capabilities {
		if capability.Kind != extension.KindTool {
			continue
		}
		describes[capability.ID] = func(context.Context, extension.DescribeInput) (extension.ToolDescription, error) {
			return extension.ToolDescription{
				Definition: model.ToolDefinition{Name: capability.ID, Parameters: json.RawMessage(`{"type":"object"}`)},
				Available:  true,
			}, nil
		}
	}
	if _, err := extension.Compile([]extension.Native{{
		Declaration: declaration,
		Describe:    describes,
		Open: func(context.Context, extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{}, nil
		},
	}}); err != nil {
		t.Fatalf("fixture declaration does not compile: %v", err)
	}

	input := extension.DescribeInput{
		Session:     harness.SessionIdentity{SessionID: "session-1", Workspace: "/workspace", CreatedAt: time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)},
		Model:       model.ModelRef{Provider: "provider", Model: "model"},
		Constraints: harness.ToolConstraints{Readonly: true, WriteDir: "/workspace/write"},
		Revision:    "rev-1",
		AgentTypes:  []protocol.Agent{testAgent()},
		Inputs: harness.AgentInputs{
			Model:        model.ModelRef{Provider: "provider", Model: "model"},
			Instructions: "instructions",
			AssemblyMode: "full",
			Tools:        []string{"fixture.tool"},
			Capabilities: []string{"fixture.tool"},
			Constraints:  harness.ToolConstraints{Readonly: true, WriteDir: "/workspace/write"},
			Strategy:     harness.StrategySelection{ID: "strategy"},
		},
	}
	arguments, err := worker.EncodeDescribeParams(input)
	if err != nil {
		t.Fatalf("encoding describe input: %v", err)
	}
	call, err := json.Marshal(worker.CallParams{
		Target: worker.Target{Kind: worker.TargetMetadata, CapabilityID: "fixture.tool"},
		Context: worker.NewContext("metadata-token", extension.CallContext{
			ExtensionID: "fixture", ComponentID: "fixture.component", InstanceID: "metadata",
			Lifetime: extension.LifetimeHarness, Scope: extension.ScopeIdentity{Kind: extension.LifetimeHarness, Revision: "rev-1"}, Revision: "rev-1",
		}),
		Method:    "describe",
		Arguments: worker.JSONBytes(arguments),
	})
	if err != nil {
		t.Fatalf("encoding describe call: %v", err)
	}
	if err := writer.WriteFrame(worker.Frame{Kind: worker.KindRequest, ID: "describe-1", Method: worker.MethodCall, Params: call}); err != nil {
		t.Fatalf("writing describe call: %v", err)
	}
	frame, err = reader.ReadFrame()
	if err != nil {
		t.Fatalf("reading describe: %v\nstderr: %s", err, stderr.String())
	}
	if frame.Error != nil {
		t.Fatalf("describe failed: %v", frame.Error)
	}
	description := decodeFixed(t, frame.Result, worker.DecodeDescribeResult)
	if description.Definition.Name != "fixture.tool" || !description.Available {
		t.Fatalf("description = %+v", description)
	}

	stdin.Close()
	if err := command.Wait(); err != nil {
		t.Fatalf("fixture exit: %v\nstderr: %s", err, stderr.String())
	}
}
