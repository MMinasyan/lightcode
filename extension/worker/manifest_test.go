package worker_test

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
)

func TestEncodeManifestCarriesDeclarationContractAndEntryPoint(t *testing.T) {
	declaration := extension.Declaration{
		ID:           "manifest",
		Version:      "1.2.3",
		ConfigSchema: json.RawMessage(`{"type":"object"}`),
		Components: []extension.Component{{
			ID:       "component",
			Lifetime: extension.LifetimeHarness,
			Provides: []string{"manifest.tool"},
		}},
		Capabilities: []extension.Capability{{
			ID:              "manifest.tool",
			ContractID:      extension.ContractTool,
			ContractVersion: extension.ContractVersion,
			Kind:            extension.KindTool,
			Methods:         []extension.Method{{Name: "execute"}},
		}},
	}
	encoded, err := worker.EncodeManifest(declaration, "bin/worker")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	var manifest worker.Manifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.ContractVersion != extension.ContractVersion || manifest.EntryPoint != "bin/worker" {
		t.Fatalf("manifest envelope = %+v", manifest)
	}
	if got := manifest.Declaration.Value(); got.ID != declaration.ID || got.Version != declaration.Version {
		t.Fatalf("manifest declaration = %+v", got)
	}
}
