package worker

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
	"github.com/MMinasyan/lightcode/protocol"
)

// ActivateParams is the first request's input: the exact host contract version.
type ActivateParams struct {
	ContractVersion string `json:"contract_version"`
}

// ActivateResult is the activation response: the wire declaration and the
// sorted IDs of the declared tool capabilities with a metadata callback.
type ActivateResult struct {
	Declaration Declaration `json:"declaration"`
	Described   []string    `json:"described"`
}

// NewContext encodes one host-issued callback context and its opaque token.
func NewContext(token string, call extension.CallContext) Context {
	return Context{Token: token, callContextJSON: callContextFrom(call)}
}

// NewInstanceContext encodes one open or close callback view and its token.
func NewInstanceContext(token string, instance extension.InstanceContext) Context {
	return Context{
		Token: token,
		callContextJSON: callContextJSON{
			ExtensionID:  instance.ExtensionID,
			ComponentID:  instance.ComponentID,
			InstanceID:   instance.InstanceID,
			Lifetime:     instance.Lifetime,
			Scope:        instance.Scope,
			Revision:     instance.Revision,
			Section:      JSONBytes(instance.Section),
			SessionID:    instance.Scope.SessionID,
			OperationID:  instance.Scope.OperationID,
			InvocationID: instance.Scope.InvocationID,
			Purpose:      instance.Scope.Purpose,
		},
	}
}

// Caller is one caller identity with the fixed callerIdentity member names.
type Caller struct {
	ExtensionID string `json:"extension_id"`
	ComponentID string `json:"component_id"`
	InstanceID  string `json:"instance_id"`
}

func (c *Caller) value() extension.CallerIdentity {
	if c == nil {
		return extension.CallerIdentity{}
	}
	return extension.CallerIdentity{ExtensionID: c.ExtensionID, ComponentID: c.ComponentID, InstanceID: c.InstanceID}
}

// Context is one host-issued callback routing context: the shared call
// identities plus the opaque token that alone grants binding access.
type Context struct {
	Token string `json:"token"`
	callContextJSON
}

func (c Context) callContext() extension.CallContext {
	return callContextValue(c.callContextJSON)
}

// Target addresses a metadata capability or one opened instance capability.
type Target struct {
	Kind         string `json:"kind"`
	CapabilityID string `json:"capability_id"`
	InstanceID   string `json:"instance_id,omitempty"`
}

// OpenParams is one open request's input.
type OpenParams struct {
	InstanceID  string                  `json:"instance_id"`
	ComponentID string                  `json:"component_id"`
	Scope       extension.ScopeIdentity `json:"scope"`
	Context     Context                 `json:"context"`
	Section     JSONBytes               `json:"section"`
}

// DisposeParams is one dispose request's input.
type DisposeParams struct {
	InstanceID string  `json:"instance_id"`
	Context    Context `json:"context"`
}

// CallParams is one call request's input.
type CallParams struct {
	Target    Target    `json:"target"`
	Context   Context   `json:"context"`
	Method    string    `json:"method"`
	Arguments JSONBytes `json:"arguments"`
}

// HostcallParams is one hostcall request's input.
type HostcallParams struct {
	Context   Context   `json:"context"`
	Binding   string    `json:"binding"`
	Method    string    `json:"method"`
	Arguments JSONBytes `json:"arguments"`
}

type modelRefJSON struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// modelRefFrom renders one model identity as the fixed schema's required
// {provider,model} object. A zero or partial identity has no schema form and
// fails the call.
func modelRefFrom(v model.ModelRef) (modelRefJSON, error) {
	if v.Provider == "" || v.Model == "" {
		return modelRefJSON{}, fmt.Errorf("worker: model reference %q is not complete", v.String())
	}
	return modelRefJSON{Provider: v.Provider, Model: v.Model}, nil
}

// optionalModelRef renders one optional model identity; a zero value stays
// absent.
func optionalModelRef(v model.ModelRef) (*modelRefJSON, error) {
	if v.IsZero() {
		return nil, nil
	}
	encoded, err := modelRefFrom(v)
	if err != nil {
		return nil, err
	}
	return &encoded, nil
}

func modelRefValue(v modelRefJSON) model.ModelRef {
	return model.ModelRef{Provider: v.Provider, Model: v.Model}
}

type extraJSON struct {
	Key   JSONBytes `json:"key"`
	Value JSONBytes `json:"value"`
}

func extraFrom(v model.Extra) []extraJSON {
	if len(v) == 0 {
		return nil
	}
	keys := make([]string, 0, len(v))
	for key := range v {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]extraJSON, 0, len(v))
	for _, key := range keys {
		out = append(out, extraJSON{Key: JSONBytes(key), Value: JSONBytes(v[key])})
	}
	return out
}

func extraValue(v []extraJSON) model.Extra {
	if len(v) == 0 {
		return nil
	}
	out := make(model.Extra, len(v))
	for _, entry := range v {
		out[string(entry.Key)] = append(json.RawMessage(nil), entry.Value...)
	}
	return out
}

type contentPartJSON struct {
	Kind           string      `json:"kind"`
	Text           string      `json:"text,omitempty"`
	URL            string      `json:"url,omitempty"`
	OpaqueWireType string      `json:"opaque_wire_type,omitempty"`
	Extra          []extraJSON `json:"extra,omitempty"`
}

func contentFrom(v []model.ContentPart) []contentPartJSON {
	out := make([]contentPartJSON, len(v))
	for i, part := range v {
		out[i] = contentPartJSON{
			Kind:           string(part.Kind),
			Text:           part.Text,
			URL:            part.URL,
			OpaqueWireType: part.OpaqueWireType,
			Extra:          extraFrom(part.Extra),
		}
	}
	return out
}

func contentValue(v []contentPartJSON) []model.ContentPart {
	out := make([]model.ContentPart, len(v))
	for i, part := range v {
		out[i] = model.ContentPart{
			Kind:           model.PartKind(part.Kind),
			Text:           part.Text,
			URL:            part.URL,
			OpaqueWireType: part.OpaqueWireType,
			Extra:          extraValue(part.Extra),
		}
	}
	return out
}

type toolCallJSON struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Arguments JSONBytes   `json:"arguments"`
	Extra     []extraJSON `json:"extra,omitempty"`
}

func toolCallFrom(v model.ToolCall) toolCallJSON {
	return toolCallJSON{ID: v.ID, Name: v.Name, Arguments: JSONBytes(v.Arguments), Extra: extraFrom(v.Extra)}
}

func toolCallValue(v toolCallJSON) model.ToolCall {
	return model.ToolCall{ID: v.ID, Name: v.Name, Arguments: append(json.RawMessage(nil), v.Arguments...), Extra: extraValue(v.Extra)}
}

type messageJSON struct {
	Role       string            `json:"role"`
	Content    []contentPartJSON `json:"content"`
	Refusal    string            `json:"refusal,omitempty"`
	ToolCalls  []toolCallJSON    `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	Name       string            `json:"name,omitempty"`
	Extra      []extraJSON       `json:"extra,omitempty"`
	Source     *modelRefJSON     `json:"source,omitempty"`
}

func messageFrom(v model.Message) (messageJSON, error) {
	out := messageJSON{
		Role:       string(v.Role),
		Content:    contentFrom(v.Content),
		Refusal:    v.Refusal,
		ToolCallID: v.ToolCallID,
		Name:       v.Name,
		Extra:      extraFrom(v.Extra),
	}
	if len(v.ToolCalls) != 0 {
		out.ToolCalls = make([]toolCallJSON, len(v.ToolCalls))
		for i, call := range v.ToolCalls {
			out.ToolCalls[i] = toolCallFrom(call)
		}
	}
	source, err := optionalModelRef(v.Source)
	if err != nil {
		return messageJSON{}, err
	}
	out.Source = source
	return out, nil
}

func messageValue(v messageJSON) model.Message {
	out := model.Message{
		Role:       model.Role(v.Role),
		Content:    contentValue(v.Content),
		Refusal:    v.Refusal,
		ToolCallID: v.ToolCallID,
		Name:       v.Name,
		Extra:      extraValue(v.Extra),
	}
	if len(v.ToolCalls) != 0 {
		out.ToolCalls = make([]model.ToolCall, len(v.ToolCalls))
		for i, call := range v.ToolCalls {
			out.ToolCalls[i] = toolCallValue(call)
		}
	}
	if v.Source != nil {
		out.Source = modelRefValue(*v.Source)
	}
	return out
}

func messagesFrom(v []model.Message) ([]messageJSON, error) {
	if v == nil {
		return nil, nil
	}
	out := make([]messageJSON, len(v))
	for i, message := range v {
		encoded, err := messageFrom(message)
		if err != nil {
			return nil, err
		}
		out[i] = encoded
	}
	return out, nil
}

func messagesRequired(v []model.Message) ([]messageJSON, error) {
	out := make([]messageJSON, len(v))
	for i, message := range v {
		encoded, err := messageFrom(message)
		if err != nil {
			return nil, err
		}
		out[i] = encoded
	}
	return out, nil
}

func messagesValue(v []messageJSON) []model.Message {
	out := make([]model.Message, len(v))
	for i, message := range v {
		out[i] = messageValue(message)
	}
	return out
}

type toolDefinitionJSON struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Parameters  JSONBytes `json:"parameters"`
}

func toolDefinitionFrom(v model.ToolDefinition) toolDefinitionJSON {
	return toolDefinitionJSON{Name: v.Name, Description: v.Description, Parameters: JSONBytes(v.Parameters)}
}

func toolDefinitionValue(v toolDefinitionJSON) model.ToolDefinition {
	return model.ToolDefinition{Name: v.Name, Description: v.Description, Parameters: append(json.RawMessage(nil), v.Parameters...)}
}

func toolDefinitionsFrom(v []model.ToolDefinition) []toolDefinitionJSON {
	out := make([]toolDefinitionJSON, len(v))
	for i, definition := range v {
		out[i] = toolDefinitionFrom(definition)
	}
	return out
}

func toolDefinitionsValue(v []toolDefinitionJSON) []model.ToolDefinition {
	out := make([]model.ToolDefinition, len(v))
	for i, definition := range v {
		out[i] = toolDefinitionValue(definition)
	}
	return out
}

type toolResultJSON struct {
	CallID  string `json:"call_id"`
	Status  string `json:"status"`
	Content string `json:"content"`
}

func toolResultFrom(v model.ToolResult) toolResultJSON {
	return toolResultJSON{CallID: v.CallID, Status: string(v.Status), Content: v.Content}
}

func toolResultValue(v toolResultJSON) model.ToolResult {
	return model.ToolResult{CallID: v.CallID, Status: model.ToolResultStatus(v.Status), Content: v.Content}
}

type toolOutcomeJSON struct {
	Result   toolResultJSON `json:"result"`
	Metadata JSONBytes      `json:"metadata,omitempty"`
	Evidence string         `json:"evidence"`
}

func toolOutcomeFrom(v harness.ToolOutcome) toolOutcomeJSON {
	return toolOutcomeJSON{
		Result:   toolResultFrom(v.Result),
		Metadata: JSONBytes(v.Metadata),
		Evidence: string(v.Evidence),
	}
}

func toolOutcomeValue(v toolOutcomeJSON) harness.ToolOutcome {
	return harness.ToolOutcome{
		Result:   toolResultValue(v.Result),
		Metadata: append(json.RawMessage(nil), v.Metadata...),
		Evidence: harness.ExecutionEvidence(v.Evidence),
	}
}

type permissionJSON struct {
	Permission string `json:"permission"`
	Target     string `json:"target"`
}

type toolPlanJSON struct {
	Permissions        []permissionJSON `json:"permissions"`
	CanonicalWorkspace string           `json:"canonical_workspace,omitempty"`
	CanonicalWriteDir  string           `json:"canonical_write_dir,omitempty"`
	Immediate          *toolOutcomeJSON `json:"immediate,omitempty"`
	Data               JSONBytes        `json:"data,omitempty"`
}

func toolPlanFrom(v extension.ToolPlan) toolPlanJSON {
	out := toolPlanJSON{
		Permissions:        make([]permissionJSON, len(v.Permissions)),
		CanonicalWorkspace: v.CanonicalWorkspace,
		CanonicalWriteDir:  v.CanonicalWriteDir,
		Data:               JSONBytes(v.Data),
	}
	for i, permission := range v.Permissions {
		out.Permissions[i] = permissionJSON{Permission: permission.Permission, Target: permission.Target}
	}
	if v.Immediate != nil {
		immediate := toolOutcomeFrom(*v.Immediate)
		out.Immediate = &immediate
	}
	return out
}

func toolPlanValue(v toolPlanJSON) extension.ToolPlan {
	out := extension.ToolPlan{
		Permissions:        make([]harness.PermissionRequest, len(v.Permissions)),
		CanonicalWorkspace: v.CanonicalWorkspace,
		CanonicalWriteDir:  v.CanonicalWriteDir,
		Data:               append([]byte(nil), v.Data...),
	}
	for i, permission := range v.Permissions {
		out.Permissions[i] = harness.PermissionRequest{Permission: permission.Permission, Target: permission.Target}
	}
	if v.Immediate != nil {
		immediate := toolOutcomeValue(*v.Immediate)
		out.Immediate = &immediate
	}
	return out
}

type callContextJSON struct {
	ExtensionID  string                  `json:"extension_id"`
	ComponentID  string                  `json:"component_id"`
	InstanceID   string                  `json:"instance_id"`
	Lifetime     extension.Lifetime      `json:"lifetime"`
	Scope        extension.ScopeIdentity `json:"scope"`
	Revision     string                  `json:"revision"`
	Section      JSONBytes               `json:"section,omitempty"`
	SessionID    string                  `json:"session_id,omitempty"`
	OperationID  string                  `json:"operation_id,omitempty"`
	InvocationID string                  `json:"invocation_id,omitempty"`
	Purpose      string                  `json:"purpose,omitempty"`
	RequestID    string                  `json:"request_id,omitempty"`
	ToolCallID   string                  `json:"tool_call_id,omitempty"`
	EffectID     string                  `json:"effect_id,omitempty"`
	Caller       *Caller                 `json:"caller,omitempty"`
}

func callContextFrom(v extension.CallContext) callContextJSON {
	out := callContextJSON{
		ExtensionID:  v.ExtensionID,
		ComponentID:  v.ComponentID,
		InstanceID:   v.InstanceID,
		Lifetime:     v.Lifetime,
		Scope:        v.Scope,
		Revision:     v.Revision,
		Section:      JSONBytes(v.Section),
		SessionID:    v.SessionID,
		OperationID:  v.OperationID,
		InvocationID: v.InvocationID,
		Purpose:      v.Purpose,
		RequestID:    v.RequestID,
		ToolCallID:   v.ToolCallID,
		EffectID:     v.EffectID,
	}
	if v.Caller != (extension.CallerIdentity{}) {
		caller := Caller{ExtensionID: v.Caller.ExtensionID, ComponentID: v.Caller.ComponentID, InstanceID: v.Caller.InstanceID}
		out.Caller = &caller
	}
	return out
}

func callContextValue(v callContextJSON) extension.CallContext {
	out := extension.CallContext{
		ExtensionID:  v.ExtensionID,
		ComponentID:  v.ComponentID,
		InstanceID:   v.InstanceID,
		Lifetime:     v.Lifetime,
		Scope:        v.Scope,
		Revision:     v.Revision,
		Section:      append(json.RawMessage(nil), v.Section...),
		SessionID:    v.SessionID,
		OperationID:  v.OperationID,
		InvocationID: v.InvocationID,
		Purpose:      v.Purpose,
		RequestID:    v.RequestID,
		ToolCallID:   v.ToolCallID,
		EffectID:     v.EffectID,
	}
	if v.Caller != nil {
		out.Caller = extension.CallerIdentity{ExtensionID: v.Caller.ExtensionID, ComponentID: v.Caller.ComponentID, InstanceID: v.Caller.InstanceID}
	}
	return out
}

type toolContextJSON struct {
	Session       harness.SessionIdentity `json:"session"`
	Model         modelRefJSON            `json:"model"`
	Revision      string                  `json:"revision"`
	Workspace     string                  `json:"workspace"`
	AdmittedEntry harness.EntryRef        `json:"admitted_entry"`
	Constraints   harness.ToolConstraints `json:"constraints"`
	Config        JSONBytes               `json:"config,omitempty"`
	AgentTypes    []agentJSON             `json:"agent_types"`
	Calls         callContextJSON         `json:"calls"`
}

func toolContextFrom(v extension.ToolContext) (toolContextJSON, error) {
	model, err := modelRefFrom(v.Model)
	if err != nil {
		return toolContextJSON{}, err
	}
	agents, err := agentsFrom(v.AgentTypes)
	if err != nil {
		return toolContextJSON{}, err
	}
	return toolContextJSON{
		Session:       v.Session,
		Model:         model,
		Revision:      v.Revision,
		Workspace:     v.Workspace,
		AdmittedEntry: v.AdmittedEntry,
		Constraints:   v.Constraints,
		Config:        JSONBytes(v.Config),
		AgentTypes:    agents,
		Calls:         callContextFrom(v.Calls),
	}, nil
}

func toolContextValue(v toolContextJSON) extension.ToolContext {
	return extension.ToolContext{
		Session:       v.Session,
		Model:         modelRefValue(v.Model),
		Revision:      v.Revision,
		Workspace:     v.Workspace,
		AdmittedEntry: v.AdmittedEntry,
		Constraints:   v.Constraints,
		Config:        append(json.RawMessage(nil), v.Config...),
		AgentTypes:    agentsValue(v.AgentTypes),
		Calls:         callContextValue(v.Calls),
	}
}

type definitionRefJSON struct {
	ExtensionID  string `json:"extension_id"`
	CapabilityID string `json:"capability_id"`
	DefinitionID string `json:"definition_id"`
}

type agentJSON struct {
	Name         string             `json:"name,omitempty"`
	Description  string             `json:"description,omitempty"`
	Model        string             `json:"model,omitempty"`
	Tools        []string           `json:"tools,omitempty"`
	Capabilities []string           `json:"capabilities,omitempty"`
	Readonly     bool               `json:"readonly,omitempty"`
	Subagent     bool               `json:"subagent,omitempty"`
	WriteDir     string             `json:"write_dir,omitempty"`
	Definition   *definitionRefJSON `json:"definition,omitempty"`
}

func agentFrom(v protocol.Agent) (agentJSON, error) {
	out := agentJSON{
		Name:         v.Name,
		Description:  v.Description,
		Model:        v.Model,
		Tools:        append([]string(nil), v.Tools...),
		Capabilities: append([]string(nil), v.Capabilities...),
		Readonly:     v.Readonly,
		Subagent:     v.Subagent,
		WriteDir:     v.WriteDir,
	}
	if v.Definition != (protocol.DefinitionRef{}) {
		if v.Definition.ExtensionId == "" || v.Definition.CapabilityId == "" || v.Definition.DefinitionId == "" {
			return agentJSON{}, fmt.Errorf("worker: agent definition reference is not complete")
		}
		out.Definition = &definitionRefJSON{
			ExtensionID:  v.Definition.ExtensionId,
			CapabilityID: v.Definition.CapabilityId,
			DefinitionID: v.Definition.DefinitionId,
		}
	}
	return out, nil
}

func agentValue(v agentJSON) protocol.Agent {
	out := protocol.Agent{
		Name:         v.Name,
		Description:  v.Description,
		Model:        v.Model,
		Tools:        append([]string(nil), v.Tools...),
		Capabilities: append([]string(nil), v.Capabilities...),
		Readonly:     v.Readonly,
		Subagent:     v.Subagent,
		WriteDir:     v.WriteDir,
	}
	if v.Definition != nil {
		out.Definition = protocol.DefinitionRef{
			ExtensionId:  v.Definition.ExtensionID,
			CapabilityId: v.Definition.CapabilityID,
			DefinitionId: v.Definition.DefinitionID,
		}
	}
	return out
}

func agentsFrom(v []protocol.Agent) ([]agentJSON, error) {
	if v == nil {
		return []agentJSON{}, nil
	}
	out := make([]agentJSON, len(v))
	for i, agent := range v {
		encoded, err := agentFrom(agent)
		if err != nil {
			return nil, err
		}
		out[i] = encoded
	}
	return out, nil
}

func agentsValue(v []agentJSON) []protocol.Agent {
	if v == nil {
		return nil
	}
	out := make([]protocol.Agent, len(v))
	for i, agent := range v {
		out[i] = agentValue(agent)
	}
	return out
}

type describeParamsJSON struct {
	Config      JSONBytes               `json:"config,omitempty"`
	Model       modelRefJSON            `json:"model"`
	Session     harness.SessionIdentity `json:"session"`
	Constraints harness.ToolConstraints `json:"constraints"`
	AgentTypes  []agentJSON             `json:"agent_types"`
	Revision    string                  `json:"revision"`
	Inputs      harness.AgentInputs     `json:"inputs"`
}

// EncodeDescribeParams encodes one tool describe input.
func EncodeDescribeParams(v extension.DescribeInput) (json.RawMessage, error) {
	model, err := modelRefFrom(v.Model)
	if err != nil {
		return nil, err
	}
	agents, err := agentsFrom(v.AgentTypes)
	if err != nil {
		return nil, err
	}
	return marshalValue(describeParamsJSON{
		Config:      JSONBytes(v.Config),
		Model:       model,
		Session:     v.Session,
		Constraints: v.Constraints,
		AgentTypes:  agents,
		Revision:    v.Revision,
		Inputs:      v.Inputs,
	})
}

// DecodeDescribeParams decodes one tool describe input.
func DecodeDescribeParams(raw json.RawMessage) (extension.DescribeInput, error) {
	var wire describeParamsJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return extension.DescribeInput{}, err
	}
	return extension.DescribeInput{
		Session:     wire.Session,
		Model:       modelRefValue(wire.Model),
		Constraints: wire.Constraints,
		Revision:    wire.Revision,
		Config:      append(json.RawMessage(nil), wire.Config...),
		AgentTypes:  agentsValue(wire.AgentTypes),
		Inputs:      wire.Inputs,
	}, nil
}

type toolDescriptionJSON struct {
	Definition toolDefinitionJSON `json:"definition"`
	Available  bool               `json:"available"`
}

// EncodeDescribeResult encodes one tool description.
func EncodeDescribeResult(v extension.ToolDescription) (json.RawMessage, error) {
	return marshalValue(toolDescriptionJSON{Definition: toolDefinitionFrom(v.Definition), Available: v.Available})
}

// DecodeDescribeResult decodes one tool description.
func DecodeDescribeResult(raw json.RawMessage) (extension.ToolDescription, error) {
	var wire toolDescriptionJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return extension.ToolDescription{}, err
	}
	return extension.ToolDescription{Definition: toolDefinitionValue(wire.Definition), Available: wire.Available}, nil
}

type normalizeParamsJSON struct {
	Context toolContextJSON `json:"context"`
	Call    toolCallJSON    `json:"call"`
}

// EncodeNormalizeParams encodes one normalize input.
func EncodeNormalizeParams(context extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	encoded, err := toolContextFrom(context)
	if err != nil {
		return nil, err
	}
	return marshalValue(normalizeParamsJSON{Context: encoded, Call: toolCallFrom(call)})
}

// DecodeNormalizeParams decodes one normalize input.
func DecodeNormalizeParams(raw json.RawMessage) (extension.ToolContext, model.ToolCall, error) {
	var wire normalizeParamsJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return extension.ToolContext{}, model.ToolCall{}, err
	}
	return toolContextValue(wire.Context), toolCallValue(wire.Call), nil
}

type argumentsJSON struct {
	Arguments JSONBytes `json:"arguments"`
}

// EncodeArguments encodes the shared {arguments} result of normalize and the
// before-tool hook.
func EncodeArguments(v json.RawMessage) (json.RawMessage, error) {
	return marshalValue(argumentsJSON{Arguments: JSONBytes(v)})
}

// DecodeArguments decodes the shared {arguments} result of normalize and the
// before-tool hook.
func DecodeArguments(raw json.RawMessage) (json.RawMessage, error) {
	var wire argumentsJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), wire.Arguments...), nil
}

type prepareParamsJSON struct {
	Call    toolCallJSON    `json:"call"`
	Context toolContextJSON `json:"context"`
}

// EncodePrepareParams encodes one prepare input.
func EncodePrepareParams(context extension.ToolContext, call model.ToolCall) (json.RawMessage, error) {
	encoded, err := toolContextFrom(context)
	if err != nil {
		return nil, err
	}
	return marshalValue(prepareParamsJSON{Call: toolCallFrom(call), Context: encoded})
}

// DecodePrepareParams decodes one prepare input.
func DecodePrepareParams(raw json.RawMessage) (extension.ToolContext, model.ToolCall, error) {
	var wire prepareParamsJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return extension.ToolContext{}, model.ToolCall{}, err
	}
	return toolContextValue(wire.Context), toolCallValue(wire.Call), nil
}

// EncodePrepareResult encodes one prepared tool plan.
func EncodePrepareResult(v extension.ToolPlan) (json.RawMessage, error) {
	return marshalValue(toolPlanFrom(v))
}

// DecodePrepareResult decodes one prepared tool plan.
func DecodePrepareResult(raw json.RawMessage) (extension.ToolPlan, error) {
	var wire toolPlanJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return extension.ToolPlan{}, err
	}
	return toolPlanValue(wire), nil
}

type executeParamsJSON struct {
	Context toolContextJSON `json:"context"`
	Plan    toolPlanJSON    `json:"plan"`
}

// EncodeExecuteParams encodes one execute input.
func EncodeExecuteParams(context extension.ToolContext, plan extension.ToolPlan) (json.RawMessage, error) {
	encoded, err := toolContextFrom(context)
	if err != nil {
		return nil, err
	}
	return marshalValue(executeParamsJSON{Context: encoded, Plan: toolPlanFrom(plan)})
}

// DecodeExecuteParams decodes one execute input.
func DecodeExecuteParams(raw json.RawMessage) (extension.ToolContext, extension.ToolPlan, error) {
	var wire executeParamsJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return extension.ToolContext{}, extension.ToolPlan{}, err
	}
	return toolContextValue(wire.Context), toolPlanValue(wire.Plan), nil
}

// EncodeExecuteResult encodes one tool execution outcome.
func EncodeExecuteResult(v harness.ToolOutcome) (json.RawMessage, error) {
	return marshalValue(toolOutcomeFrom(v))
}

// DecodeExecuteResult decodes one tool execution outcome.
func DecodeExecuteResult(raw json.RawMessage) (harness.ToolOutcome, error) {
	var wire toolOutcomeJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return harness.ToolOutcome{}, err
	}
	return toolOutcomeValue(wire), nil
}

type preparationInputJSON struct {
	Session     harness.SessionIdentity `json:"session"`
	Model       modelRefJSON            `json:"model"`
	Constraints harness.ToolConstraints `json:"constraints"`
	Assembly    prompt.AssemblyInput    `json:"assembly"`
	Prompt      string                  `json:"prompt"`
	Catalog     []toolDefinitionJSON    `json:"catalog"`
	Tools       []toolDefinitionJSON    `json:"tools"`
	Settings    JSONBytes               `json:"settings,omitempty"`
}

// EncodePrepareHookParams encodes one preparation hook input.
func EncodePrepareHookParams(v extension.PreparationInput) (json.RawMessage, error) {
	model, err := modelRefFrom(v.Model)
	if err != nil {
		return nil, err
	}
	return marshalValue(preparationInputJSON{
		Session:     v.Session,
		Model:       model,
		Constraints: v.Constraints,
		Assembly:    v.Assembly,
		Prompt:      v.Prompt,
		Catalog:     toolDefinitionsFrom(v.Catalog),
		Tools:       toolDefinitionsFrom(v.Tools),
		Settings:    JSONBytes(v.Settings),
	})
}

// DecodePrepareHookParams decodes one preparation hook input.
func DecodePrepareHookParams(raw json.RawMessage) (extension.PreparationInput, error) {
	var wire preparationInputJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return extension.PreparationInput{}, err
	}
	return extension.PreparationInput{
		Session:     wire.Session,
		Model:       modelRefValue(wire.Model),
		Constraints: wire.Constraints,
		Assembly:    wire.Assembly,
		Prompt:      wire.Prompt,
		Catalog:     toolDefinitionsValue(wire.Catalog),
		Tools:       toolDefinitionsValue(wire.Tools),
		Settings:    append(json.RawMessage(nil), wire.Settings...),
	}, nil
}

type preparationConsequenceJSON struct {
	Prompt       *string           `json:"prompt,omitempty"`
	Descriptions map[string]string `json:"descriptions,omitempty"`
	ToolNames    *[]string         `json:"tool_names,omitempty"`
}

// EncodePrepareHookResult encodes one preparation consequence. An absent tool
// set remains absent; a present empty set stays present and empty.
func EncodePrepareHookResult(v extension.PreparationConsequence) (json.RawMessage, error) {
	out := preparationConsequenceJSON{Prompt: v.Prompt, Descriptions: v.Descriptions}
	if v.ToolNames != nil {
		toolNames := v.ToolNames
		out.ToolNames = &toolNames
	}
	return marshalValue(out)
}

// DecodePrepareHookResult decodes one preparation consequence.
func DecodePrepareHookResult(raw json.RawMessage) (extension.PreparationConsequence, error) {
	var wire preparationConsequenceJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return extension.PreparationConsequence{}, err
	}
	out := extension.PreparationConsequence{Prompt: wire.Prompt, Descriptions: wire.Descriptions}
	if wire.ToolNames != nil {
		out.ToolNames = *wire.ToolNames
	}
	return out, nil
}

type requestHookInputJSON struct {
	RequestID         string               `json:"request_id"`
	CanonicalMessages []messageJSON        `json:"canonical_messages"`
	Messages          []messageJSON        `json:"messages"`
	CheckpointEntryID string               `json:"checkpoint_entry_id,omitempty"`
	BoundaryEntryID   string               `json:"boundary_entry_id,omitempty"`
	CapturedPrompt    string               `json:"captured_prompt"`
	Prompt            string               `json:"prompt"`
	Catalog           []toolDefinitionJSON `json:"catalog"`
	Tools             []toolDefinitionJSON `json:"tools"`
}

// EncodeRequestHookParams encodes one request hook input.
func EncodeRequestHookParams(v harness.RequestHookInput) (json.RawMessage, error) {
	canonical, err := messagesRequired(v.CanonicalMessages)
	if err != nil {
		return nil, err
	}
	messages, err := messagesRequired(v.Messages)
	if err != nil {
		return nil, err
	}
	return marshalValue(requestHookInputJSON{
		RequestID:         v.RequestID,
		CanonicalMessages: canonical,
		Messages:          messages,
		CheckpointEntryID: v.CheckpointEntryID,
		BoundaryEntryID:   v.BoundaryEntryID,
		CapturedPrompt:    v.CapturedPrompt,
		Prompt:            v.Prompt,
		Catalog:           toolDefinitionsFrom(v.Catalog),
		Tools:             toolDefinitionsFrom(v.Tools),
	})
}

// DecodeRequestHookParams decodes one request hook input.
func DecodeRequestHookParams(raw json.RawMessage) (harness.RequestHookInput, error) {
	var wire requestHookInputJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return harness.RequestHookInput{}, err
	}
	return harness.RequestHookInput{
		RequestID:         wire.RequestID,
		CanonicalMessages: messagesValue(wire.CanonicalMessages),
		Messages:          messagesValue(wire.Messages),
		CheckpointEntryID: wire.CheckpointEntryID,
		BoundaryEntryID:   wire.BoundaryEntryID,
		CapturedPrompt:    wire.CapturedPrompt,
		Prompt:            wire.Prompt,
		Catalog:           toolDefinitionsValue(wire.Catalog),
		Tools:             toolDefinitionsValue(wire.Tools),
	}, nil
}

type requestConsequenceJSON struct {
	Prompt    *string       `json:"prompt,omitempty"`
	Messages  []messageJSON `json:"messages,omitempty"`
	ToolNames *[]string     `json:"tool_names,omitempty"`
}

// EncodeRequestHookResult encodes one request consequence. An absent tool set
// remains absent; a present empty set stays present and empty.
func EncodeRequestHookResult(v harness.RequestConsequence) (json.RawMessage, error) {
	messages, err := messagesFrom(v.Messages)
	if err != nil {
		return nil, err
	}
	out := requestConsequenceJSON{Prompt: v.Prompt, Messages: messages}
	if v.ToolNames != nil {
		toolNames := v.ToolNames
		out.ToolNames = &toolNames
	}
	return marshalValue(out)
}

// DecodeRequestHookResult decodes one request consequence.
func DecodeRequestHookResult(raw json.RawMessage) (harness.RequestConsequence, error) {
	var wire requestConsequenceJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return harness.RequestConsequence{}, err
	}
	out := harness.RequestConsequence{Prompt: wire.Prompt, Messages: messagesValue(wire.Messages)}
	if wire.ToolNames != nil {
		out.ToolNames = *wire.ToolNames
	}
	return out, nil
}

// EncodeBeforeToolHookParams encodes one before-tool hook input.
func EncodeBeforeToolHookParams(v model.ToolCall) (json.RawMessage, error) {
	return marshalValue(toolCallFrom(v))
}

// DecodeBeforeToolHookParams decodes one before-tool hook input.
func DecodeBeforeToolHookParams(raw json.RawMessage) (model.ToolCall, error) {
	var wire toolCallJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return model.ToolCall{}, err
	}
	return toolCallValue(wire), nil
}

type toolOutputInputJSON struct {
	Outcome toolOutcomeJSON `json:"outcome"`
	Content string          `json:"content"`
}

// EncodeAfterToolHookParams encodes one after-tool hook input.
func EncodeAfterToolHookParams(v harness.ToolOutputInput) (json.RawMessage, error) {
	return marshalValue(toolOutputInputJSON{Outcome: toolOutcomeFrom(v.Outcome), Content: v.Content})
}

// DecodeAfterToolHookParams decodes one after-tool hook input.
func DecodeAfterToolHookParams(raw json.RawMessage) (harness.ToolOutputInput, error) {
	var wire toolOutputInputJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return harness.ToolOutputInput{}, err
	}
	return harness.ToolOutputInput{Outcome: toolOutcomeValue(wire.Outcome), Content: wire.Content}, nil
}

type contentJSON struct {
	Content string `json:"content"`
}

// EncodeAfterToolHookResult encodes one after-tool hook result.
func EncodeAfterToolHookResult(v string) (json.RawMessage, error) {
	return marshalValue(contentJSON{Content: v})
}

// DecodeAfterToolHookResult decodes one after-tool hook result.
func DecodeAfterToolHookResult(raw json.RawMessage) (string, error) {
	var wire contentJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return "", err
	}
	return wire.Content, nil
}

type requestEvidenceJSON struct {
	CheckpointEntryID string                 `json:"checkpoint_entry_id,omitempty"`
	BoundaryEntryID   string                 `json:"boundary_entry_id"`
	Prompt            string                 `json:"prompt"`
	Messages          []messageJSON          `json:"messages,omitempty"`
	ToolNames         []string               `json:"tool_names"`
	HookIDs           []harness.HookIdentity `json:"hook_ids,omitempty"`
}

func requestEvidenceFrom(v *harness.RequestEvidence) (*requestEvidenceJSON, error) {
	if v == nil {
		return nil, nil
	}
	messages, err := messagesFrom(v.Messages)
	if err != nil {
		return nil, err
	}
	tools := v.ToolNames
	if tools == nil {
		tools = []string{}
	}
	return &requestEvidenceJSON{
		CheckpointEntryID: v.CheckpointEntryID,
		BoundaryEntryID:   v.BoundaryEntryID,
		Prompt:            v.Prompt,
		Messages:          messages,
		ToolNames:         tools,
		HookIDs:           append([]harness.HookIdentity(nil), v.HookIDs...),
	}, nil
}

func requestEvidenceValue(v *requestEvidenceJSON) *harness.RequestEvidence {
	if v == nil {
		return nil
	}
	return &harness.RequestEvidence{
		CheckpointEntryID: v.CheckpointEntryID,
		BoundaryEntryID:   v.BoundaryEntryID,
		Prompt:            v.Prompt,
		Messages:          messagesValue(v.Messages),
		ToolNames:         v.ToolNames,
		HookIDs:           append([]harness.HookIdentity(nil), v.HookIDs...),
	}
}

type planInputJSON struct {
	Trigger           string               `json:"trigger"`
	Messages          []messageJSON        `json:"messages"`
	CheckpointEntryID string               `json:"checkpoint_entry_id,omitempty"`
	BoundaryEntryID   string               `json:"boundary_entry_id"`
	Instructions      string               `json:"instructions"`
	Tools             []toolDefinitionJSON `json:"tools"`
	Contributions     *requestEvidenceJSON `json:"contributions,omitempty"`
	Measurement       *harness.UsageCount  `json:"measurement,omitempty"`
	ContextWindow     int                  `json:"context_window"`
	OutputReserve     int                  `json:"output_reserve"`
	Settings          JSONBytes            `json:"settings,omitempty"`
}

func planInputFrom(v harness.PlanInput) (planInputJSON, error) {
	messages, err := messagesRequired(v.Messages)
	if err != nil {
		return planInputJSON{}, err
	}
	contributions, err := requestEvidenceFrom(v.Contributions)
	if err != nil {
		return planInputJSON{}, err
	}
	return planInputJSON{
		Trigger:           v.Trigger,
		Messages:          messages,
		CheckpointEntryID: v.CheckpointEntryID,
		BoundaryEntryID:   v.BoundaryEntryID,
		Instructions:      v.Instructions,
		Tools:             toolDefinitionsFrom(v.Tools),
		Contributions:     contributions,
		Measurement:       v.Measurement,
		ContextWindow:     v.ContextWindow,
		OutputReserve:     v.OutputReserve,
		Settings:          JSONBytes(v.Settings),
	}, nil
}

func planInputValue(v planInputJSON) harness.PlanInput {
	return harness.PlanInput{
		Trigger:           v.Trigger,
		Messages:          messagesValue(v.Messages),
		CheckpointEntryID: v.CheckpointEntryID,
		BoundaryEntryID:   v.BoundaryEntryID,
		Instructions:      v.Instructions,
		Tools:             toolDefinitionsValue(v.Tools),
		Contributions:     requestEvidenceValue(v.Contributions),
		Measurement:       v.Measurement,
		ContextWindow:     v.ContextWindow,
		OutputReserve:     v.OutputReserve,
		Settings:          append(json.RawMessage(nil), v.Settings...),
	}
}

type modelRequestJSON struct {
	Messages []messageJSON        `json:"messages"`
	Tools    []toolDefinitionJSON `json:"tools"`
}

type modelPlanJSON struct {
	Request modelRequestJSON `json:"request"`
	State   JSONBytes        `json:"state,omitempty"`
}

type checkpointItemJSON struct {
	Retain  *int         `json:"retain,omitempty"`
	Context *messageJSON `json:"context,omitempty"`
}

type checkpointPlanJSON struct {
	BoundaryEntryID string               `json:"boundary_entry_id"`
	Items           []checkpointItemJSON `json:"items"`
}

type planOutputJSON struct {
	Kind       string              `json:"kind"`
	Model      *modelPlanJSON      `json:"model,omitempty"`
	Checkpoint *checkpointPlanJSON `json:"checkpoint,omitempty"`
}

func planOutputFrom(v harness.PlanOutput) (planOutputJSON, error) {
	out := planOutputJSON{Kind: string(v.Kind)}
	if v.Model != nil {
		messages, err := messagesRequired(v.Model.Request.Messages)
		if err != nil {
			return planOutputJSON{}, err
		}
		modelPlan := modelPlanJSON{
			Request: modelRequestJSON{
				Messages: messages,
				Tools:    toolDefinitionsFrom(v.Model.Request.Tools),
			},
			State: JSONBytes(v.Model.State),
		}
		out.Model = &modelPlan
	}
	if v.Checkpoint != nil {
		checkpoint := checkpointPlanJSON{
			BoundaryEntryID: v.Checkpoint.BoundaryEntryID,
			Items:           make([]checkpointItemJSON, len(v.Checkpoint.Items)),
		}
		for i, item := range v.Checkpoint.Items {
			checkpoint.Items[i] = checkpointItemJSON{Retain: item.Retain}
			if item.Context != nil {
				message, err := messageFrom(*item.Context)
				if err != nil {
					return planOutputJSON{}, err
				}
				checkpoint.Items[i].Context = &message
			}
		}
		out.Checkpoint = &checkpoint
	}
	return out, nil
}

func planOutputValue(v planOutputJSON) harness.PlanOutput {
	out := harness.PlanOutput{Kind: harness.PlanKind(v.Kind)}
	if v.Model != nil {
		plan := harness.ModelPlan{
			Request: model.Request{
				Messages: messagesValue(v.Model.Request.Messages),
				Tools:    toolDefinitionsValue(v.Model.Request.Tools),
			},
			State: append(json.RawMessage(nil), v.Model.State...),
		}
		out.Model = &plan
	}
	if v.Checkpoint != nil {
		checkpoint := harness.CheckpointPlan{
			BoundaryEntryID: v.Checkpoint.BoundaryEntryID,
			Items:           make([]harness.CheckpointItem, len(v.Checkpoint.Items)),
		}
		for i, item := range v.Checkpoint.Items {
			checkpoint.Items[i] = harness.CheckpointItem{Retain: item.Retain}
			if item.Context != nil {
				message := messageValue(*item.Context)
				checkpoint.Items[i].Context = &message
			}
		}
		out.Checkpoint = &checkpoint
	}
	return out
}

// EncodePlanParams encodes one context strategy plan input.
func EncodePlanParams(v harness.PlanInput) (json.RawMessage, error) {
	encoded, err := planInputFrom(v)
	if err != nil {
		return nil, err
	}
	return marshalValue(encoded)
}

// DecodePlanParams decodes one context strategy plan input.
func DecodePlanParams(raw json.RawMessage) (harness.PlanInput, error) {
	var wire planInputJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return harness.PlanInput{}, err
	}
	return planInputValue(wire), nil
}

type modelOutputJSON struct {
	Status  string              `json:"status"`
	Source  modelRefJSON        `json:"source"`
	Message *messageJSON        `json:"message,omitempty"`
	Usage   *harness.UsageCount `json:"usage,omitempty"`
	Detail  string              `json:"detail"`
}

func modelOutputFrom(v model.Output) (modelOutputJSON, error) {
	source, err := modelRefFrom(v.Source)
	if err != nil {
		return modelOutputJSON{}, err
	}
	out := modelOutputJSON{
		Status: string(v.Status),
		Source: source,
		Usage:  usageFrom(v.Usage),
		Detail: v.Detail,
	}
	if v.Message != nil {
		message, err := messageFrom(*v.Message)
		if err != nil {
			return modelOutputJSON{}, err
		}
		out.Message = &message
	}
	return out, nil
}

func modelOutputValue(v modelOutputJSON) model.Output {
	out := model.Output{
		Status: model.OutputStatus(v.Status),
		Source: modelRefValue(v.Source),
		Usage:  usageValue(v.Usage),
		Detail: v.Detail,
	}
	if v.Message != nil {
		message := messageValue(*v.Message)
		out.Message = &message
	}
	return out
}

type consumeInputJSON struct {
	Input  planInputJSON   `json:"input"`
	State  JSONBytes       `json:"state,omitempty"`
	Output modelOutputJSON `json:"output"`
}

// EncodeConsumeParams encodes one context strategy consume input.
func EncodeConsumeParams(v harness.ConsumeInput) (json.RawMessage, error) {
	input, err := planInputFrom(v.Input)
	if err != nil {
		return nil, err
	}
	output, err := modelOutputFrom(v.Output)
	if err != nil {
		return nil, err
	}
	return marshalValue(consumeInputJSON{Input: input, State: JSONBytes(v.State), Output: output})
}

// DecodeConsumeParams decodes one context strategy consume input.
func DecodeConsumeParams(raw json.RawMessage) (harness.ConsumeInput, error) {
	var wire consumeInputJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return harness.ConsumeInput{}, err
	}
	return harness.ConsumeInput{
		Input:  planInputValue(wire.Input),
		State:  append(json.RawMessage(nil), wire.State...),
		Output: modelOutputValue(wire.Output),
	}, nil
}

// EncodePlanResult encodes one context strategy plan or consume result.
func EncodePlanResult(v harness.PlanOutput) (json.RawMessage, error) {
	encoded, err := planOutputFrom(v)
	if err != nil {
		return nil, err
	}
	return marshalValue(encoded)
}

// DecodePlanResult decodes one context strategy plan or consume result.
func DecodePlanResult(raw json.RawMessage) (harness.PlanOutput, error) {
	var wire planOutputJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return harness.PlanOutput{}, err
	}
	return planOutputValue(wire), nil
}

type failureJSON struct {
	Code    string     `json:"code"`
	Message string     `json:"message"`
	Data    *JSONBytes `json:"data,omitempty"`
	Partial *JSONBytes `json:"partial,omitempty"`
}

type serviceResultJSON struct {
	Value   *JSONBytes   `json:"value,omitempty"`
	Failure *failureJSON `json:"failure,omitempty"`
}

// EncodeServiceResult encodes one result envelope. Exactly one of the success
// value and the failure arm is present; failure data and partial payloads use
// the common zero-length-is-absent rule.
func EncodeServiceResult(v extension.Result) (json.RawMessage, error) {
	if err := extension.ValidateResultEnvelope(v); err != nil {
		return nil, fmt.Errorf("worker: result: %w", err)
	}
	out := serviceResultJSON{}
	if v.Value != nil {
		value := JSONBytes(v.Value)
		out.Value = &value
	}
	if v.Failure != nil {
		failure := &failureJSON{Code: v.Failure.Code, Message: v.Failure.Message}
		if len(v.Failure.Data) != 0 {
			data := JSONBytes(v.Failure.Data)
			failure.Data = &data
		}
		if len(v.Failure.Partial) != 0 {
			partial := JSONBytes(v.Failure.Partial)
			failure.Partial = &partial
		}
		out.Failure = failure
	}
	return marshalValue(out)
}

// DecodeServiceResult decodes one result envelope. Failure data and partial
// payloads use the common zero-length-is-absent rule. Content validity belongs
// to the accepting boundary.
func DecodeServiceResult(raw json.RawMessage) (extension.Result, error) {
	var wire serviceResultJSON
	if err := unmarshalValue(raw, &wire); err != nil {
		return extension.Result{}, err
	}
	out := extension.Result{}
	if wire.Value != nil {
		out.Value = append(json.RawMessage{}, (*wire.Value)...)
	}
	if wire.Failure != nil {
		out.Failure = &extension.Failure{Code: wire.Failure.Code, Message: wire.Failure.Message}
		if wire.Failure.Data != nil && len(*wire.Failure.Data) != 0 {
			out.Failure.Data = append(json.RawMessage{}, (*wire.Failure.Data)...)
		}
		if wire.Failure.Partial != nil && len(*wire.Failure.Partial) != 0 {
			out.Failure.Partial = append(json.RawMessage{}, (*wire.Failure.Partial)...)
		}
	}
	if err := extension.ValidateResultEnvelope(out); err != nil {
		return extension.Result{}, fmt.Errorf("worker: result: %w", err)
	}
	return out, nil
}

// HookStatus is one effectful hook failure's closed status.
type HookStatus string

// The closed effectful hook failure statuses.
const (
	HookStatusError       HookStatus = "error"
	HookStatusInterrupted HookStatus = "interrupted"
)

// HookFailure is one effectful hook's typed failure arm: the closed status, the
// author's execution knowledge, and the non-secret message the host turns into
// the native EffectError knowledge mapping.
type HookFailure struct {
	Status   HookStatus
	Evidence harness.ExecutionEvidence
	Message  string
}

type hookFailureJSON struct {
	Status   HookStatus                `json:"status"`
	Evidence harness.ExecutionEvidence `json:"evidence"`
}

// DecodeHookResult decodes one effectful hook result envelope: either the
// success value or a typed failure with its status, evidence and message.
func DecodeHookResult(raw json.RawMessage) (json.RawMessage, *HookFailure, error) {
	result, err := DecodeServiceResult(raw)
	if err != nil {
		return nil, nil, err
	}
	if result.Failure == nil {
		return result.Value, nil, nil
	}
	var data hookFailureJSON
	if err := unmarshalValue(result.Failure.Data, &data); err != nil {
		return nil, nil, fmt.Errorf("worker: hook failure data: %w", err)
	}
	return nil, &HookFailure{Status: data.Status, Evidence: data.Evidence, Message: result.Failure.Message}, nil
}

// DecodeRequestHookOutcome decodes one effectful request hook's result
// envelope into its typed consequence or typed failure.
func DecodeRequestHookOutcome(raw json.RawMessage) (harness.RequestConsequence, *HookFailure, error) {
	value, failure, err := DecodeHookResult(raw)
	if err != nil || failure != nil {
		return harness.RequestConsequence{}, failure, err
	}
	consequence, err := DecodeRequestHookResult(value)
	if err != nil {
		return harness.RequestConsequence{}, nil, err
	}
	return consequence, nil, nil
}

// DecodeBeforeToolHookOutcome decodes one effectful before-tool hook's result
// envelope into its typed argument replacement or typed failure.
func DecodeBeforeToolHookOutcome(raw json.RawMessage) (json.RawMessage, *HookFailure, error) {
	value, failure, err := DecodeHookResult(raw)
	if err != nil || failure != nil {
		return nil, failure, err
	}
	arguments, err := DecodeArguments(value)
	if err != nil {
		return nil, nil, err
	}
	return arguments, nil, nil
}

// HookFailureFromError maps one effectful hook author error through the
// harness's closed interruption/knowledge predicates, so a worker hook settles
// exactly like the same native hook.
func HookFailureFromError(err error) *HookFailure {
	if err == nil {
		return nil
	}
	status := HookStatusError
	if harness.EffectInterrupted(err) {
		status = HookStatusInterrupted
	}
	return &HookFailure{Status: status, Evidence: harness.DeclaredExecutionKnowledge(err), Message: err.Error()}
}

func usageFrom(v *model.Usage) *harness.UsageCount {
	if v == nil {
		return nil
	}
	return &harness.UsageCount{InputTokens: int64(v.InputTokens), CachedInputTokens: int64(v.CachedInputTokens), OutputTokens: int64(v.OutputTokens)}
}

func usageValue(v *harness.UsageCount) *model.Usage {
	if v == nil {
		return nil
	}
	return &model.Usage{InputTokens: int(v.InputTokens), CachedInputTokens: int(v.CachedInputTokens), OutputTokens: int(v.OutputTokens)}
}

func marshalValue(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func unmarshalValue(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return fmt.Errorf("worker: missing JSON value")
	}
	if err := rejectNullTokens(raw); err != nil {
		return fmt.Errorf("worker: invalid JSON value: %w", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("worker: invalid JSON value: %w", err)
	}
	return nil
}
