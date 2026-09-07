// Package runtime composes the inactive managed Runtime around the public
// Harness. This file owns the private immutable configuration snapshot: one
// owned input set decoded from captured configuration bytes, consumed once
// and never reread, with no exported configuration, catalog, document-editing
// or warning API.
package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/model"
)

// configuration is the immutable snapshot of one effective input set: the
// assembled catalog with its build warnings, the complete resolved agent
// definitions (retaining their private fields) with the definition warnings,
// the session policy, the per-plugin configuration values, and the
// publication generation assigned by the publisher. Input documents and
// unconsumed sections are discarded once the snapshot is built; later
// consumers add their own fields when they land.
type configuration struct {
	generation      uint64
	catalog         *catalog.Catalog
	catalogWarnings []catalog.Warning
	definitions     []agents.Resolved
	agentWarnings   []agents.Warning
	sessions        config.SessionConfig
	plugins         map[string]json.RawMessage
}

// capturedConfigDocument is the private single-pass decode of one captured
// main configuration document: only the catalog, sessions, and plugin
// sections are consumed, numbers stay exact through the UseNumber decoder,
// and no legacy whole-document shape or permission rule is enforced.
type capturedConfigDocument struct {
	Providers map[string]any             `json:"providers"`
	Sessions  json.RawMessage            `json:"sessions"`
	Plugins   map[string]json.RawMessage `json:"plugins"`
}

// newConfiguration decodes the captured main-configuration and
// agent-definition bytes once, assembles the effective catalog from the
// supplied layers plus the captured providers (no entry point rereads the
// main configuration), and returns the complete snapshot carrying the given
// publication generation.
func newConfiguration(generation uint64, configData, agentsData []byte, layers catalog.BuildInputs, capabilityIDs []string) (*configuration, error) {
	var doc capturedConfigDocument
	decoder := json.NewDecoder(bytes.NewReader(configData))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode captured configuration: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode captured configuration: unexpected trailing JSON value")
		}
		return nil, fmt.Errorf("decode captured configuration: %w", err)
	}
	sessions, err := config.ParseSessions(doc.Sessions)
	if err != nil {
		return nil, fmt.Errorf("captured configuration sessions: %w", err)
	}
	layers.UserRaw = doc.Providers
	built := catalog.Build(layers)
	definitions, err := agents.ParseWithCapabilities(agentsData, capabilityIDs)
	if err != nil {
		return nil, fmt.Errorf("decode captured agent definitions: %w", err)
	}
	return &configuration{
		generation:      generation,
		catalog:         built.Catalog,
		catalogWarnings: built.Warnings,
		definitions:     definitions.All(),
		agentWarnings:   definitions.Warnings(),
		sessions:        sessions,
		plugins:         doc.Plugins,
	}, nil
}

// agentTypes projects the snapshot's resolved definitions onto the Harness:
// public model identity, owned slices, and the complete roster with no
// internal package type reaching the view.
func (c *configuration) agentTypes() []harness.AgentType {
	out := make([]harness.AgentType, 0, len(c.definitions))
	for _, def := range c.definitions {
		at := harness.AgentType{
			Name:         def.Name,
			SystemPrompt: def.SystemPrompt,
			Prompt:       def.Prompt,
		}
		if def.Model != "" {
			if ref, err := model.Parse(def.Model); err == nil {
				at.Model = ref
			}
		}
		at.Tools = append([]string(nil), def.Tools...)
		at.Capabilities = append([]string(nil), def.Capabilities...)
		out = append(out, at)
	}
	return out
}
