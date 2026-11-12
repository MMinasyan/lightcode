package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Syntheresis/melta/extension"
	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/model"
)

// Options are the application-supplied construction data of the shipped
// Agents extension: the registered capability and tool universes custom
// definitions validate against, and the default capability selection
// installed on the built-in primary before user overlays and inheritance.
// The application composition supplies them; the extension itself holds no
// product default.
type Options struct {
	CapabilityIDs        []string
	ToolIDs              []string
	DefaultCapabilityIDs []string
}

func (o Options) owned() Options {
	return Options{
		CapabilityIDs:        append([]string(nil), o.CapabilityIDs...),
		ToolIDs:              append([]string(nil), o.ToolIDs...),
		DefaultCapabilityIDs: append([]string(nil), o.DefaultCapabilityIDs...),
	}
}

const setModelInputSchema = `{"type":"object","required":["definition_id","model"],"properties":{"definition_id":{"type":"string","minLength":1},"model":{"type":"string"}},"additionalProperties":false}`

const setModelSuccessSchema = `{"type":"object"}`

// Native returns the Harness-scoped Agents extension: the schema-bound
// definition resolver, the supplied model configuration edit, and the
// child-session task tool, all over one construction component.
func Native(options Options) extension.Native {
	options = options.owned()
	childLaunch, _ := extension.HostCapability(extension.ContractChildLaunch)
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           ExtensionID,
			Version:      "1",
			ConfigSchema: []byte(configSchema),
			Components: []extension.Component{{
				ID:       ExtensionID,
				Lifetime: extension.LifetimeHarness,
				Provides: []string{DefinitionsCapabilityID, ConfigurationCapabilityID, TaskCapabilityID},
				Requires: []extension.Binding{{
					Name:         "child_launch",
					Provider:     extension.HostProvider,
					CapabilityID: extension.ContractChildLaunch,
					Definition:   childLaunch,
				}},
			}},
			Capabilities: []extension.Capability{
				extension.AgentDefinitionsCapability(DefinitionsCapabilityID),
				extension.ServiceCapability(ConfigurationCapabilityID, ConfigurationCapabilityID,
					extension.PureMethod("set_model", setModelInputSchema, setModelSuccessSchema)),
				extension.ToolCapability(TaskCapabilityID),
			},
			Edits: []extension.ConfigurationEdit{{
				ID:     ModelEditID,
				Method: extension.MethodKey{CapabilityID: ConfigurationCapabilityID, Method: "set_model"},
			}},
		},
		Describe: map[string]extension.ToolDescribe{TaskCapabilityID: taskTool{}.describe},
		Open: func(ctx context.Context, instance extension.InstanceContext) (extension.Instance, error) {
			return open(ctx, instance, options)
		},
	}
}

// instance is one parsed extension instance: the roster resolved once from
// the captured section, the task settings, and the constructor universes the
// configuration edit reuses.
type instance struct {
	definitions *Config
	settings    settings
	options     Options
}

func open(ctx context.Context, instanceContext extension.InstanceContext, options Options) (extension.Instance, error) {
	if err := ctx.Err(); err != nil {
		return extension.Instance{}, err
	}
	s, err := decodeSettings(instanceContext.Section)
	if err != nil {
		return extension.Instance{}, err
	}
	definitions, err := decodeDefinitions(instanceContext.Section, options)
	if err != nil {
		return extension.Instance{}, err
	}
	if warnings := definitions.Warnings(); len(warnings) != 0 {
		return extension.Instance{}, warnings[0]
	}
	value := &instance{definitions: definitions, settings: s, options: options}
	return extension.Instance{Capabilities: map[string]extension.Implementation{
		DefinitionsCapabilityID: {Service: map[string]extension.Handler{
			"list":    value.list,
			"resolve": value.resolve,
		}},
		ConfigurationCapabilityID: {Service: map[string]extension.Handler{"set_model": value.setModel}},
		TaskCapabilityID:          {Tool: taskTool{settings: s}},
	}}, nil
}

// decodeDefinitions extracts the definitions member from one active section
// and parses it through the extension's own format. An absent member is the
// empty document; a present member of any other shape is rejected by the
// parser.
func decodeDefinitions(section json.RawMessage, options Options) (*Config, error) {
	data := []byte("{}")
	if len(bytes.TrimSpace(section)) != 0 {
		var document struct {
			Definitions json.RawMessage `json:"definitions"`
		}
		if err := json.Unmarshal(section, &document); err != nil {
			return nil, fmt.Errorf("decode agent definitions: %w", err)
		}
		if len(document.Definitions) != 0 {
			data = document.Definitions
		}
	}
	parsed, err := ParseWithCapabilities(data, options.CapabilityIDs, options.ToolIDs, options.DefaultCapabilityIDs)
	if err != nil {
		return nil, fmt.Errorf("decode agent definitions: %w", err)
	}
	return parsed, nil
}

// list returns the resolver's roster: unique displayed IDs and descriptions
// in the parser's retained order.
func (i *instance) list(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
	resolved := i.definitions.All()
	summaries := make([]extension.AgentDefinitionSummary, 0, len(resolved))
	for _, definition := range resolved {
		summaries = append(summaries, extension.AgentDefinitionSummary{ID: definition.Name, Description: definition.Description})
	}
	value, err := json.Marshal(summaries)
	if err != nil {
		return extension.Result{}, err
	}
	return extension.Result{Value: value}, nil
}

// resolve returns one definition's complete resolved inputs and its
// extension-owned display/task eligibility metadata.
func (i *instance) resolve(_ context.Context, _ extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
	var request struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(arguments, &request); err != nil {
		return extension.Result{}, err
	}
	resolved, err := i.definitions.Resolve(request.ID)
	if err != nil {
		return extension.Result{}, err
	}
	value, err := json.Marshal(extension.AgentDefinitionResolution{
		Inputs:      resolvedInputs(resolved),
		Description: resolved.Description,
		Subagent:    resolved.Subagent,
	})
	if err != nil {
		return extension.Result{}, err
	}
	return extension.Result{Value: value}, nil
}

// resolvedInputs projects one resolved definition onto the shared
// AgentInputs document. WriteDir is trimmed exactly once here, preserving
// the legacy whitespace-as-unset behavior.
func resolvedInputs(resolved Resolved) harness.AgentInputs {
	var ref model.ModelRef
	if resolved.Model != "" {
		if parsed, err := model.Parse(resolved.Model); err == nil {
			ref = parsed
		}
	}
	return harness.AgentInputs{
		Model:        ref,
		Instructions: resolved.Prompt,
		AssemblyMode: resolved.SystemPrompt,
		Tools:        append([]string(nil), resolved.Tools...),
		Capabilities: append([]string(nil), resolved.Capabilities...),
		Constraints:  harness.ToolConstraints{Readonly: resolved.Readonly, WriteDir: strings.TrimSpace(resolved.WriteDir)},
	}.Owned()
}

// setModel transforms the latest owning section through the extension's own
// authoring format: a nonempty model replaces the definition's user override,
// an empty model clears it, and every other section member and definition
// field is preserved. The known-definition and model parsing failures are the
// retained invalid-input errors.
func (i *instance) setModel(_ context.Context, call extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
	var request struct {
		DefinitionID string `json:"definition_id"`
		Model        string `json:"model"`
	}
	if err := json.Unmarshal(arguments, &request); err != nil {
		return extension.Result{}, err
	}
	if request.Model != "" {
		if _, err := model.Parse(request.Model); err != nil {
			return extension.Result{}, fmt.Errorf("model ref %q: %v: %w", request.Model, err, harness.ErrInvalid)
		}
	}
	section, err := decodeObject(call.Section, "agents section")
	if err != nil {
		return extension.Result{}, err
	}
	definitions := map[string]json.RawMessage{}
	if len(section["definitions"]) != 0 {
		definitions, err = decodeObject(section["definitions"], "agents definitions")
		if err != nil {
			return extension.Result{}, err
		}
	}
	data, err := json.Marshal(definitions)
	if err != nil {
		return extension.Result{}, err
	}
	parsed, err := ParseWithCapabilities(data, i.options.CapabilityIDs, i.options.ToolIDs, i.options.DefaultCapabilityIDs)
	if err != nil {
		return extension.Result{}, fmt.Errorf("decode agent definitions: %w", err)
	}
	if _, err := parsed.Resolve(request.DefinitionID); err != nil {
		return extension.Result{}, fmt.Errorf("%v: %w", err, harness.ErrInvalid)
	}
	var fields map[string]json.RawMessage
	if raw, ok := definitions[request.DefinitionID]; ok {
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return extension.Result{}, fmt.Errorf("agents.%s must be an object: %w", request.DefinitionID, harness.ErrInvalid)
		}
	} else {
		fields = map[string]json.RawMessage{}
	}
	if request.Model == "" {
		delete(fields, "model")
	} else {
		if fields["model"], err = json.Marshal(request.Model); err != nil {
			return extension.Result{}, err
		}
	}
	if definitions[request.DefinitionID], err = json.Marshal(fields); err != nil {
		return extension.Result{}, err
	}
	if section["definitions"], err = json.Marshal(definitions); err != nil {
		return extension.Result{}, err
	}
	transformed, err := json.Marshal(section)
	if err != nil {
		return extension.Result{}, err
	}
	return extension.Result{Value: transformed}, nil
}

// decodeObject decodes one section document into a raw-member object; null is
// the empty object and any other shape is invalid input.
func decodeObject(raw json.RawMessage, label string) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("%s must be an object: %w", label, harness.ErrInvalid)
	}
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	return object, nil
}
