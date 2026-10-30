package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
)

// compactionSettings is the parsed top-level compaction section: the optional
// independent summarizer model reference and the effective instruction text.
// An absent or empty model keeps the conversation-model fallback, and an
// absent prompt is the shared builtin summarizer text.
type compactionSettings struct {
	model  model.ModelRef
	prompt string
}

// defaultCompactionSettings is the absent-section value: no independent model
// and the unchanged builtin prompt.
func defaultCompactionSettings() compactionSettings {
	return compactionSettings{prompt: prompt.DefaultSummarizerPrompt}
}

// requireSettingsObject enforces the one section rule shared by the baseline
// configuration sections: absent is empty, and a present member is a JSON
// object, never null, an array, or a scalar.
func requireSettingsObject(name string, raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return fmt.Errorf("%s: settings section must be a JSON object", name)
	}
	return nil
}

// parseCompactionSettings decodes and validates one top-level compaction
// section. Unknown members are ignored, a malformed nonempty model reference
// rejects the candidate through the existing model-reference parser, and a
// present prompt must be a nonempty string whose bytes are kept exactly.
func parseCompactionSettings(raw json.RawMessage) (compactionSettings, error) {
	settings := defaultCompactionSettings()
	if err := requireSettingsObject("compaction", raw); err != nil {
		return compactionSettings{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return settings, nil
	}
	var members struct {
		Model  *string         `json:"model"`
		Prompt json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &members); err != nil {
		return compactionSettings{}, fmt.Errorf("compaction: invalid settings: %w", err)
	}
	if members.Model != nil && *members.Model != "" {
		ref, err := model.Parse(*members.Model)
		if err != nil {
			return compactionSettings{}, fmt.Errorf("compaction.model: %w", err)
		}
		settings.model = ref
	}
	if members.Prompt != nil {
		var text string
		if err := json.Unmarshal(members.Prompt, &text); err != nil || text == "" {
			return compactionSettings{}, fmt.Errorf("compaction.prompt must be a nonempty string")
		}
		settings.prompt = text
	}
	return settings, nil
}

// validateBaselineSection applies the object-when-present rule and then the
// owning baseline service's own settings validator.
func validateBaselineSection(name string, raw json.RawMessage, validate func(json.RawMessage) error) error {
	if err := requireSettingsObject(name, raw); err != nil {
		return err
	}
	return validate(raw)
}
