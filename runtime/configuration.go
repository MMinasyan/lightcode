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
	"strings"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/model"
)

// configuration is the immutable snapshot of one effective input set: the
// assembled catalog with its build warnings, the complete resolved agent
// definitions (retaining their private fields) with the definition warnings,
// the session policy, the per-plugin configuration values, and the captured
// global and Workspace permission inputs. Input documents and unconsumed
// sections are discarded once the snapshot is built; later consumers add
// their own fields when they land.
type configuration struct {
	generation           uint64
	catalog              *catalog.Catalog
	catalogWarnings      []catalog.Warning
	definitions          []agents.Resolved
	agentWarnings        []agents.Warning
	sessions             config.SessionConfig
	plugins              map[string]json.RawMessage
	permissions          json.RawMessage
	workspacePermissions map[string]json.RawMessage
}

// capturedConfigDocument is the private single-pass decode of one captured
// main configuration document: only the catalog, sessions, and plugin
// sections are consumed while the raw permissions member is captured
// unchanged for policy resolution, numbers stay exact through the UseNumber
// decoder, and no legacy whole-document shape or permission rule is
// enforced.
type capturedConfigDocument struct {
	Providers   map[string]any             `json:"providers"`
	Sessions    json.RawMessage            `json:"sessions"`
	Plugins     map[string]json.RawMessage `json:"plugins"`
	Permissions json.RawMessage            `json:"permissions"`
}

// decodeCapturedConfig decodes one captured main configuration document in a
// single pass: only the catalog, sessions, and plugin sections are consumed,
// numbers stay exact through the UseNumber decoder, and no legacy
// whole-document shape or permission rule is enforced.
func decodeCapturedConfig(configData []byte) (capturedConfigDocument, error) {
	var doc capturedConfigDocument
	decoder := json.NewDecoder(bytes.NewReader(configData))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode captured configuration: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return doc, fmt.Errorf("decode captured configuration: unexpected trailing JSON value")
		}
		return doc, fmt.Errorf("decode captured configuration: %w", err)
	}
	return doc, nil
}

// newConfiguration assembles the complete snapshot from one decoded captured
// document, the provider assembly already performed for the captured
// providers layer (a catalog entry never rereads the main configuration), the
// captured agent-definition bytes, and the captured Workspace permission
// bytes: it calls ParseSessions and agents.ParseWithCapabilities against the
// supplied ordinary visible export IDs and the compiled tool universe, with
// empty capability defaults until a later composition phase derives them, and
// carries the given publication generation. Plugin-section validation belongs
// to the publisher, not here.
func newConfiguration(generation uint64, doc capturedConfigDocument, built catalog.BuildResult, agentsData []byte, capabilityIDs, toolIDs []string, workspacePermissions map[string]json.RawMessage) (*configuration, error) {
	sessions, err := config.ParseSessions(doc.Sessions)
	if err != nil {
		return nil, fmt.Errorf("captured configuration sessions: %w", err)
	}
	definitions, err := agents.ParseWithCapabilities(agentsData, capabilityIDs, toolIDs, nil)
	if err != nil {
		return nil, fmt.Errorf("decode captured agent definitions: %w", err)
	}
	return &configuration{
		generation:           generation,
		catalog:              built.Catalog,
		catalogWarnings:      built.Warnings,
		definitions:          definitions.All(),
		agentWarnings:        definitions.Warnings(),
		sessions:             sessions,
		plugins:              doc.Plugins,
		permissions:          doc.Permissions,
		workspacePermissions: workspacePermissions,
	}, nil
}

// permissionPolicy resolves this revision's automatic policy for one
// Workspace from the captured global permissions member and the captured
// Workspace permission bytes keyed by directory ID; neither input is reread.
// An absent Workspace capture is an absent Workspace policy level, and any
// malformed block follows the built-in fallback posture inside
// ResolvePermissionPolicy.
func (c *configuration) permissionPolicy(workspace string) harness.PermissionPolicy {
	var workspaceRaw json.RawMessage
	if id, err := config.WorkspacePermissionDirID(workspace); err == nil {
		workspaceRaw = c.workspacePermissions[id]
	}
	return harness.ResolvePermissionPolicy(c.permissions, workspaceRaw)
}

// agentTypes projects the snapshot's resolved definitions onto the Harness:
// public model identity, owned slices, the permission capability constraints
// with WriteDir trimmed once (preserving the legacy whitespace-as-unset
// behavior: no environment expansion, no new path syntax), and the complete
// roster with no internal package type reaching the view.
func (c *configuration) agentTypes() []harness.AgentType {
	out := make([]harness.AgentType, 0, len(c.definitions))
	for _, def := range c.definitions {
		at := harness.AgentType{
			Name:         def.Name,
			SystemPrompt: def.SystemPrompt,
			Prompt:       def.Prompt,
			Readonly:     def.Readonly,
			WriteDir:     strings.TrimSpace(def.WriteDir),
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
