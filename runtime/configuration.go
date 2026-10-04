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
	"github.com/MMinasyan/lightcode/protocol"
)

// configuration is the immutable snapshot of one effective input set: the
// assembled catalog with its build warnings and each model's source label,
// the complete resolved agent definitions (retaining their private fields)
// with the definition warnings, the session policy, every compiled plugin's
// owned raw configuration section, the owned decoded provider user layer this
// same build consumed, and the captured global and Workspace permission
// inputs. Input documents are discarded once the snapshot is built; later
// consumers add their own fields when they land.
type configuration struct {
	generation           uint64
	catalog              *catalog.Catalog
	catalogWarnings      []catalog.Warning
	definitions          []agents.Resolved
	agentWarnings        []agents.Warning
	sessions             config.SessionConfig
	plugins              map[string]json.RawMessage
	userProviders        map[string]any
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
// the default capability selection derived from the composition (empty, or
// the single composed ModelAdaptation export ID), and
// carries the given publication generation. Plugin-section validation belongs
// to the publisher, not here.
func newConfiguration(generation uint64, doc capturedConfigDocument, built catalog.BuildResult, agentsData []byte, capabilityIDs, toolIDs, defaultCapabilityIDs []string, workspacePermissions map[string]json.RawMessage) (*configuration, error) {
	sessions, err := config.ParseSessions(doc.Sessions)
	if err != nil {
		return nil, fmt.Errorf("captured configuration sessions: %w", err)
	}
	definitions, err := agents.ParseWithCapabilities(agentsData, capabilityIDs, toolIDs, defaultCapabilityIDs)
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
		userProviders:        doc.Providers,
		permissions:          doc.Permissions,
		workspacePermissions: workspacePermissions,
	}, nil
}

// projectSettings projects one captured revision's settings view: the parsed
// session policy beside every owned raw plugin section as its opaque JSON
// document string. This is a lossless transport projection, not an
// interpreted settings authority — each document is returned as captured and
// the selected declaration's validator owns interpretation. An owned empty
// object is an empty document, absent sections are absent map members, and
// unowned numbers survive as their exact lexemes inside the strings.
func projectSettings(sessions config.SessionConfig, plugins map[string]json.RawMessage) protocol.Settings {
	out := protocol.Settings{
		Sessions: protocol.SessionsSettings{
			AutoArchive:            sessions.AutoArchive,
			ArchiveAfterDays:       sessions.ArchiveAfterDays,
			DeleteAfterArchiveDays: sessions.DeleteAfterArchiveDays,
		},
		Plugins: make(protocol.PluginsSettings, len(plugins)),
	}
	for id, raw := range plugins {
		out.Plugins[id] = string(raw)
	}
	return out
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

// agentTypes projects the snapshot's resolved definitions onto the Harness
// view.
func (c *configuration) agentTypes() []harness.AgentType {
	return projectAgentTypes(c.definitions)
}

// projectAgentTypes projects resolved definitions onto the Harness view: the
// shared producer admission and the Agent-model edit's known-type check both
// resolve through — public model identity, owned slices, the permission
// capability constraints with WriteDir trimmed once (preserving the legacy
// whitespace-as-unset behavior: no environment expansion, no new path
// syntax), the subagent eligibility and roster description, and the complete
// roster with no internal package type reaching the view.
func projectAgentTypes(defs []agents.Resolved) []harness.AgentType {
	out := make([]harness.AgentType, 0, len(defs))
	for _, def := range defs {
		at := harness.AgentType{
			Name:         def.Name,
			SystemPrompt: def.SystemPrompt,
			Prompt:       def.Prompt,
			Readonly:     def.Readonly,
			WriteDir:     strings.TrimSpace(def.WriteDir),
			Subagent:     def.Subagent,
			Description:  def.Description,
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
