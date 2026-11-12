package worker

import (
	"encoding/json"

	"github.com/Syntheresis/melta/extension"
)

// Manifest is one installed executable package's manifest: the worker's wire
// declaration, the exact host-contract version it serves, and its
// package-relative entry point. Installation accepts only a contained regular
// executable entry point; this value mirrors that package layout.
type Manifest struct {
	ContractVersion string      `json:"contract_version"`
	EntryPoint      string      `json:"entry_point"`
	Declaration     Declaration `json:"declaration"`
}

// EncodeManifest renders one package manifest for a Go worker author. The
// contract version is the one accepted host contract version; the entry point
// is relative to the package directory.
func EncodeManifest(declaration extension.Declaration, entryPoint string) ([]byte, error) {
	return json.Marshal(Manifest{
		ContractVersion: extension.ContractVersion,
		EntryPoint:      entryPoint,
		Declaration:     NewDeclaration(declaration),
	})
}
