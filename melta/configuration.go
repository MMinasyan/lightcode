// Package melta composes the inactive managed Melta around the public
// Harness. This file owns the private immutable configuration snapshot: one
// owned input set decoded from captured configuration bytes, consumed once
// and never reread, with no exported configuration, catalog, document-editing
// or warning API.
package melta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Syntheresis/melta/extension"
	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/catalog"
	"github.com/Syntheresis/melta/internal/config"
	"github.com/Syntheresis/melta/internal/jobs"
	"github.com/Syntheresis/melta/protocol"
)

// configuration is the immutable snapshot of one effective input set: the
// assembled catalog with its build warnings and each model's source label,
// the immutable Agent roster projection supplied by the active definition
// resolvers, the session policy, every compiled plugin's owned raw
// configuration section, the owned decoded provider user layer this same
// build consumed, and the captured global and Workspace permission inputs.
// Input documents are discarded once the snapshot is built; later consumers
// add their own fields when they land.
type configuration struct {
	generation           uint64
	resources            *resourceSet
	composition          *composition
	catalog              *catalog.Catalog
	catalogWarnings      []catalog.Warning
	definitions          []agentDefinition
	sessions             config.SessionConfig
	tools                json.RawMessage
	jobs                 json.RawMessage
	compaction           json.RawMessage
	compactionValues     compactionSettings
	extensions           map[string]json.RawMessage
	secrets              []declaredSecret
	declaredBindings     map[declaredField]declaredBinding
	credentials          map[string]CredentialValue
	credentialNames      []string
	userProviders        map[string]any
	permissions          json.RawMessage
	workspacePermissions map[string]json.RawMessage
}

// capturedConfigDocument is the private single-pass decode of one captured
// main configuration document: the catalog, sessions, the top-level baseline
// tools/jobs/compaction objects, and the plugin sections are consumed while
// the raw permissions member is captured unchanged for policy resolution,
// numbers stay exact through the UseNumber decoder, and no legacy
// whole-document shape or permission rule is enforced.
type capturedConfigDocument struct {
	Providers   map[string]any             `json:"providers"`
	Sessions    json.RawMessage            `json:"sessions"`
	Tools       json.RawMessage            `json:"tools"`
	Jobs        json.RawMessage            `json:"jobs"`
	Compaction  json.RawMessage            `json:"compaction"`
	Extensions  map[string]json.RawMessage `json:"extensions"`
	Permissions json.RawMessage            `json:"permissions"`
}

// declaredSecret is one active extension SecretRef resolved to the non-secret
// credential name its section field holds. Only the reference metadata is
// captured here; values live in the private activation/Operation resources
// that resolve them.
type declaredSecret struct {
	extensionID string
	field       string
	lifetime    extension.SecretLifetime
	name        string
}

// declaredField identifies one active SecretRef field in its extension.
type declaredField struct {
	extensionID string
	field       string
}

// declaredBinding is one candidate's private capture of an active declared
// field: the resolved non-secret credential name and the value captured from
// the one candidate environment observation. It is never durable.
type declaredBinding struct {
	name  string
	value string
}

// declaredSecretNames returns the unique non-secret credential names every
// active declaration references. The live producer unions them with the
// current provider names; the preparation's one credential observation
// resolves the same names for its captured values and exclusions.
func (c *configuration) declaredSecretNames() []string {
	if c == nil || len(c.secrets) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(c.secrets))
	names := make([]string, 0, len(c.secrets))
	for _, secret := range c.secrets {
		if seen[secret.name] {
			continue
		}
		seen[secret.name] = true
		names = append(names, secret.name)
	}
	return names
}

// resolveDeclaredSecrets inspects the SecretRef metadata of every active
// installed declaration against its captured section and records the
// non-secret credential names without executing extension code.
func resolveDeclaredSecrets(declarations []extension.Native, sections map[string]json.RawMessage) []declaredSecret {
	var out []declaredSecret
	for _, native := range declarations {
		declaration := native.Declaration
		if len(declaration.Secrets) == 0 {
			continue
		}
		section := sections[declaration.ID]
		if !sectionEnabled(section) {
			continue
		}
		for _, secret := range declaration.Secrets {
			name, ok := extension.SecretName(section, secret.Field)
			if !ok {
				continue
			}
			out = append(out, declaredSecret{extensionID: declaration.ID, field: secret.Field, lifetime: secret.Lifetime, name: name})
		}
	}
	return out
}

// decodeCapturedConfig decodes one captured main configuration document in a
// single pass: the catalog, sessions, baseline and plugin sections are
// consumed, numbers stay exact through the UseNumber decoder, and no legacy
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
// providers layer (a catalog entry never rereads the main configuration), and
// the captured Workspace permission bytes. Agent definitions are not parsed
// here: the active resolver extensions supply the roster projection after
// their Harness resources are ready. Plugin-section validation belongs to the
// publisher, not here.
func newConfiguration(generation uint64, doc capturedConfigDocument, built catalog.BuildResult, workspacePermissions map[string]json.RawMessage) (*configuration, error) {
	sessions, err := config.ParseSessions(doc.Sessions)
	if err != nil {
		return nil, fmt.Errorf("captured configuration sessions: %w", err)
	}
	if err := validateBaselineSection("tools", doc.Tools, validateToolsSettings); err != nil {
		return nil, fmt.Errorf("captured configuration tools: %w", err)
	}
	if err := validateBaselineSection("jobs", doc.Jobs, jobs.ValidateSettings); err != nil {
		return nil, fmt.Errorf("captured configuration jobs: %w", err)
	}
	compaction, err := parseCompactionSettings(doc.Compaction)
	if err != nil {
		return nil, fmt.Errorf("captured configuration compaction: %w", err)
	}
	return &configuration{
		generation:           generation,
		catalog:              built.Catalog,
		catalogWarnings:      built.Warnings,
		sessions:             sessions,
		tools:                doc.Tools,
		jobs:                 doc.Jobs,
		compaction:           doc.Compaction,
		compactionValues:     compaction,
		extensions:           doc.Extensions,
		userProviders:        doc.Providers,
		permissions:          doc.Permissions,
		workspacePermissions: workspacePermissions,
	}, nil
}

// projectSettings projects one captured revision's settings view: the parsed
// session policy beside the top-level baseline sections and every owned raw
// plugin section. The baseline sections are copied as their exact captured
// JSON objects, with an absent section projected as the empty object so its
// owning service applies its defaults at consumption. Every plugin section is
// an opaque JSON document string. This is a lossless transport projection, not
// an interpreted settings authority — each section is returned as captured
// and the owning declaration's validator owns interpretation. An owned empty
// object is an empty document, absent plugin sections are absent map members,
// and unowned numbers survive as their exact lexemes inside the copied bytes.
func projectSettings(sessions config.SessionConfig, tools, jobs, compaction json.RawMessage, plugins map[string]json.RawMessage) protocol.Settings {
	out := protocol.Settings{
		Sessions: protocol.SessionsSettings{
			AutoArchive:            sessions.AutoArchive,
			ArchiveAfterDays:       sessions.ArchiveAfterDays,
			DeleteAfterArchiveDays: sessions.DeleteAfterArchiveDays,
		},
		Tools:      projectSettingsObject(tools),
		Jobs:       projectSettingsObject(jobs),
		Compaction: projectSettingsObject(compaction),
		Extensions: make(protocol.ExtensionsSettings, len(plugins)),
	}
	for id, raw := range plugins {
		out.Extensions[id] = string(raw)
	}
	return out
}

// projectSettingsObject returns one owned copy of a top-level baseline
// section; an absent section is the empty object its consuming service
// defaults from.
func projectSettingsObject(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	return append(json.RawMessage(nil), raw...)
}

// policyInputs resolves this revision's captured permission inputs for one
// Workspace from the captured global permissions member and the captured
// Workspace permission bytes keyed by directory ID; neither input is reread.
// An absent Workspace capture is an absent Workspace policy level.
func (c *configuration) policyInputs(workspace string) PolicyInputs {
	var workspaceRaw json.RawMessage
	if id, err := config.WorkspacePermissionDirID(workspace); err == nil {
		workspaceRaw = c.workspacePermissions[id]
	}
	return PolicyInputs{
		Workspace:       workspace,
		Global:          append(json.RawMessage(nil), c.permissions...),
		WorkspacePolicy: append(json.RawMessage(nil), workspaceRaw...),
	}
}

// permissionPolicy resolves this revision's automatic policy for one
// Workspace from its captured inputs. Any malformed block follows the
// built-in fallback posture inside ResolvePermissionPolicy.
func (c *configuration) permissionPolicy(workspace string) harness.PermissionPolicy {
	inputs := c.policyInputs(workspace)
	return harness.ResolvePermissionPolicy(inputs.Global, inputs.WorkspacePolicy)
}
