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
// with the definition warnings, the session policy, the per-plugin
// configuration values with their effective projected settings, the owned
// decoded provider user layer this same build consumed, and the captured
// global and Workspace permission inputs. Input documents are discarded once
// the snapshot is built; later consumers add their own fields when they land.
type configuration struct {
	generation           uint64
	catalog              *catalog.Catalog
	catalogWarnings      []catalog.Warning
	definitions          []agents.Resolved
	agentWarnings        []agents.Warning
	sessions             config.SessionConfig
	plugins              map[string]json.RawMessage
	settings             protocol.Settings
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
		settings:             projectSettings(sessions, doc.Plugins),
		userProviders:        doc.Providers,
		permissions:          doc.Permissions,
		workspacePermissions: workspacePermissions,
	}, nil
}

// settingsDecoderDefaults mirror the concrete plugins' own decoders: missing
// or null members keep these values, unknown members are ignored. The
// projection is presentation only — the publication's validation ran the
// plugins' real validators, and this file imports no concrete plugin.
var (
	toolsSettingsDefaults = protocol.ToolsSettings{MaxOutputBytes: 15360, ReadMaxLines: 500, ReadLineMaxChars: 5000, CommandTimeout: 120}
	jobsSettingsDefaults  = protocol.JobsSettings{MaxBackgroundProcesses: 10, MaxOutputBytes: 15360, ReadLineMaxChars: 5000}
	tasksSettingsDefaults = protocol.TasksSettings{MaxConcurrent: 4, MaxOutputBytes: 15360}
)

// projectSettings projects the effective settings view beside the raw
// sections: the parsed session policy plus each present plugin section's
// complete effective fields — the exact existing decoder defaults with the
// captured supplied values. Absent optional sections stay omitted. This is
// the named product view, not execution authority.
func projectSettings(sessions config.SessionConfig, plugins map[string]json.RawMessage) protocol.Settings {
	out := protocol.Settings{
		Sessions: protocol.SessionsSettings{
			AutoArchive:            sessions.AutoArchive,
			ArchiveAfterDays:       sessions.ArchiveAfterDays,
			DeleteAfterArchiveDays: sessions.DeleteAfterArchiveDays,
		},
		Plugins: protocol.PluginsSettings{},
	}
	if raw, ok := plugins["tools"]; ok {
		settings := toolsSettingsDefaults
		_ = json.Unmarshal(raw, &settings) // publication already validated this section
		out.Plugins.Tools = &settings
	}
	if raw, ok := plugins["jobs"]; ok {
		settings := jobsSettingsDefaults
		_ = json.Unmarshal(raw, &settings)
		out.Plugins.Jobs = &settings
	}
	if raw, ok := plugins["tasks"]; ok {
		settings := tasksSettingsDefaults
		_ = json.Unmarshal(raw, &settings)
		out.Plugins.Tasks = &settings
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

// agentTypes projects the snapshot's resolved definitions onto the Harness:
// public model identity, owned slices, the permission capability constraints
// with WriteDir trimmed once (preserving the legacy whitespace-as-unset
// behavior: no environment expansion, no new path syntax), the subagent
// eligibility and roster description, and the complete roster with no
// internal package type reaching the view.
func (c *configuration) agentTypes() []harness.AgentType {
	out := make([]harness.AgentType, 0, len(c.definitions))
	for _, def := range c.definitions {
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
