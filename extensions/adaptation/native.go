package adaptation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/prompt"
)

// CapabilityID is the extension's one declared preparation capability.
const CapabilityID = "model_adaptation"

const configSchema = `{"type":"object","required":["enabled"],"properties":{"enabled":{"type":"boolean"},"entries":{"type":"array","items":{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string"},"prompt":{"type":"string"},"append":{"type":"array","items":{"type":"string"}},"exclude_tools":{"type":"array","items":{"type":"string"}},"tool_description_replacements":{"type":"object","propertyNames":{"minLength":1},"additionalProperties":{"type":"string"}}},"additionalProperties":true}}},"additionalProperties":true}`

// Native returns the Harness-scoped native whose single contribution is the
// prepare-boundary hook. It has no dependencies and no settings of its own
// beyond the captured user entries.
func Native() extension.Native {
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           "adaptation",
			Version:      "1",
			ConfigSchema: []byte(configSchema),
			Components: []extension.Component{{
				ID:       "adaptation",
				Lifetime: extension.LifetimeHarness,
				Provides: []string{CapabilityID},
			}},
			Capabilities: []extension.Capability{
				extension.HookCapability(CapabilityID, extension.SingleHook(extension.BoundaryPrepare, extension.HookModePure)),
			},
		},
		Open: open,
	}
}

// configuredEntry is one user-supplied match entry. It accepts the same
// pattern/append/exclude_tools/tool_description_replacements fields as the
// bundled rows plus an optional complete prompt replacement. Unknown fields
// are uninterpreted.
type configuredEntry struct {
	Pattern                     string            `json:"pattern"`
	Prompt                      *string           `json:"prompt"`
	Append                      []string          `json:"append"`
	ExcludeTools                []string          `json:"exclude_tools"`
	ToolDescriptionReplacements map[string]string `json:"tool_description_replacements"`
}

// hook applies the first matching configured or bundled entry for the input's
// model component. A configured entry's prompt replacement and append
// paragraphs apply in every assembly mode; a bundled row's append paragraphs
// apply only to full-mode prompts. Exclusions and description replacements
// apply in every mode.
type hook struct {
	entries []entry
}

func (h hook) Prepare(_ context.Context, _ extension.CallContext, input extension.PreparationInput) (extension.PreparationConsequence, error) {
	matched := matchEntryIn(h.entries, input.Model.Model)
	if matched == nil {
		return extension.PreparationConsequence{}, nil
	}
	var consequence extension.PreparationConsequence
	text := input.Prompt
	replaced := false
	if matched.adapt.Prompt != nil {
		text = *matched.adapt.Prompt
		replaced = true
	}
	appendParts := matched.adapt.Append
	if matched.bundled && input.Assembly.Mode != prompt.ModeFull {
		appendParts = nil
	}
	if len(appendParts) != 0 {
		text = TransformPrompt(text, &Adaptation{Append: appendParts})
		replaced = true
	}
	if replaced {
		consequence.Prompt = &text
	}

	excluded := make(map[string]bool, len(matched.adapt.ExcludeTools))
	for _, name := range matched.adapt.ExcludeTools {
		excluded[name] = true
	}
	visible := make([]string, 0, len(input.Catalog))
	for _, definition := range input.Catalog {
		if !excluded[definition.Name] {
			visible = append(visible, definition.Name)
		}
	}
	consequence.ToolNames = visible

	replacer := newToolDescriptionReplacer(matched.adapt.ToolDescriptionReplacements)
	descriptions := make(map[string]string)
	for _, definition := range input.Catalog {
		rendered := replacer.Replace(definition.Description)
		if rendered != definition.Description {
			descriptions[definition.Name] = rendered
		}
	}
	if len(descriptions) != 0 {
		consequence.Descriptions = descriptions
	}
	return consequence, nil
}

func open(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
	if err := ctx.Err(); err != nil {
		return extension.Instance{}, err
	}
	entries, err := decodeEntries(instance.Section)
	if err != nil {
		return extension.Instance{}, err
	}
	return extension.Instance{Capabilities: map[string]extension.Implementation{
		CapabilityID: {Preparation: hook{entries: entries}.Prepare},
	}}, nil
}

// decodeEntries parses and owns the configured entries once from the captured
// section, then places them before the bundled rows. Only the exact
// schema-recognized member names are consumed: differently cased or unknown
// members stay uninterpreted even when their JSON types are invalid, and a
// lower-cased member cannot be overridden by a differently cased sibling.
func decodeEntries(section json.RawMessage) ([]entry, error) {
	var members map[string]json.RawMessage
	if len(bytes.TrimSpace(section)) != 0 {
		if err := json.Unmarshal(section, &members); err != nil {
			return nil, fmt.Errorf("decode adaptation section: %w", err)
		}
	}
	var rawEntries []json.RawMessage
	if raw, ok := members["entries"]; ok {
		if err := json.Unmarshal(raw, &rawEntries); err != nil {
			return nil, fmt.Errorf("decode adaptation entries: %w", err)
		}
	}
	entries := make([]entry, 0, len(rawEntries)+len(bundledTable))
	for i, raw := range rawEntries {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("decode adaptation entries[%d]: %w", i, err)
		}
		var configured configuredEntry
		for _, member := range []struct {
			name   string
			target any
		}{
			{"pattern", &configured.Pattern},
			{"prompt", &configured.Prompt},
			{"append", &configured.Append},
			{"exclude_tools", &configured.ExcludeTools},
			{"tool_description_replacements", &configured.ToolDescriptionReplacements},
		} {
			if value, ok := fields[member.name]; ok {
				if err := json.Unmarshal(value, member.target); err != nil {
					return nil, fmt.Errorf("decode adaptation entries[%d].%s: %w", i, member.name, err)
				}
			}
		}
		entries = append(entries, entry{
			pattern: configured.Pattern,
			adapt: Adaptation{
				Prompt:                      configured.Prompt,
				ExcludeTools:                append([]string(nil), configured.ExcludeTools...),
				Append:                      append([]string(nil), configured.Append...),
				ToolDescriptionReplacements: cloneStringMap(configured.ToolDescriptionReplacements),
			},
		})
	}
	return append(entries, bundledTable...), nil
}
