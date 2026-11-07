package extension

import (
	"encoding/json"
	"strings"
)

// Fixed contract method schemas. Opaque tool arguments, plan data and
// serialized semantic values travel as JSONBytes: a base64 string explicitly
// identified by the fixed schema, never an embedded object the outer encoder
// would compact. Native callbacks consume the typed values directly and never
// marshal through these schemas.

func raw(value string) json.RawMessage { return json.RawMessage(value) }

// fixedDefs are the shared semantic definitions reused by the fixed tool,
// hook and strategy method schemas. They describe the actual callback values;
// opaque JSON/argument bytes remain JSONBytes and are never interpreted as
// metadata by these schemas.
const fixedDefs = `
"jsonBytes":{"type":"string","contentEncoding":"base64","pattern":"^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$"},
"modelRef":{"type":"object","required":["provider","model"],"properties":{"provider":{"type":"string","minLength":1},"model":{"type":"string","minLength":1}},"additionalProperties":false},
"usageCount":{"type":"object","properties":{"input_tokens":{"type":"integer"},"cached_input_tokens":{"type":"integer"},"output_tokens":{"type":"integer"}},"additionalProperties":false},
"extraEntry":{"type":"object","required":["key","value"],"properties":{"key":{"$ref":"#/$defs/jsonBytes"},"value":{"$ref":"#/$defs/jsonBytes"}},"additionalProperties":false},
"extra":{"type":"array","items":{"$ref":"#/$defs/extraEntry"}},
"contentPart":{"type":"object","required":["kind"],"properties":{"kind":{"type":"string","enum":["text","image_url","opaque"]},"text":{"type":"string"},"url":{"type":"string"},"opaque_wire_type":{"type":"string"},"extra":{"$ref":"#/$defs/extra"}},"additionalProperties":false},
"toolCall":{"type":"object","required":["id","name","arguments"],"properties":{"id":{"type":"string","minLength":1},"name":{"type":"string","minLength":1},"arguments":{"$ref":"#/$defs/jsonBytes"},"extra":{"$ref":"#/$defs/extra"}},"additionalProperties":false},
"message":{"type":"object","required":["role","content"],"properties":{"role":{"type":"string","enum":["system","user","assistant","tool"]},"content":{"type":"array","items":{"$ref":"#/$defs/contentPart"}},"refusal":{"type":"string"},"tool_calls":{"type":"array","items":{"$ref":"#/$defs/toolCall"}},"tool_call_id":{"type":"string"},"name":{"type":"string"},"extra":{"$ref":"#/$defs/extra"},"source":{"$ref":"#/$defs/modelRef"}},"additionalProperties":false},
"toolDefinition":{"type":"object","required":["name","parameters"],"properties":{"name":{"type":"string","minLength":1},"description":{"type":"string"},"parameters":{"$ref":"#/$defs/jsonBytes"}},"additionalProperties":false},
"toolResult":{"type":"object","required":["call_id","status","content"],"properties":{"call_id":{"type":"string","minLength":1},"status":{"type":"string","enum":["success","error","denied","interrupted"]},"content":{"type":"string"}},"additionalProperties":false},
"toolOutcome":{"type":"object","required":["result","evidence"],"properties":{"result":{"$ref":"#/$defs/toolResult"},"metadata":{"$ref":"#/$defs/jsonBytes"},"evidence":{"type":"string","enum":["","not_started","observed","unknown"]}},"additionalProperties":false},
"permission":{"type":"object","required":["permission","target"],"properties":{"permission":{"type":"string","minLength":1},"target":{"type":"string","minLength":1}},"additionalProperties":false},
"toolPlan":{"type":"object","required":["permissions"],"properties":{"permissions":{"type":"array","items":{"$ref":"#/$defs/permission"}},"canonical_workspace":{"type":"string"},"canonical_write_dir":{"type":"string"},"immediate":{"$ref":"#/$defs/toolOutcome"},"data":{"$ref":"#/$defs/jsonBytes"}},"additionalProperties":false},
"hookFailure":{"type":"object","required":["status","evidence"],"properties":{"status":{"type":"string","enum":["error","interrupted"]},"evidence":{"type":"string","enum":["not_started","observed","unknown"]}},"additionalProperties":false},
"entryRef":{"type":"object","required":["session_id","entry_id"],"properties":{"session_id":{"type":"string","minLength":1},"entry_id":{"type":"string","minLength":1}},"additionalProperties":false},
"sessionIdentity":{"type":"object","required":["session_id","workspace"],"properties":{"session_id":{"type":"string","minLength":1},"workspace":{"type":"string"},"created_at":{"type":"string"},"source_session_id":{"type":"string"},"source_boundary_entry_id":{"type":"string"},"parent_session_id":{"type":"string"}},"additionalProperties":false},
"constraints":{"type":"object","required":["readonly","write_dir"],"properties":{"readonly":{"type":"boolean"},"write_dir":{"type":"string"}},"additionalProperties":false},
"definitionRef":{"type":"object","required":["extension_id","capability_id","definition_id"],"properties":{"extension_id":{"type":"string","minLength":1},"capability_id":{"type":"string","minLength":1},"definition_id":{"type":"string","minLength":1}},"additionalProperties":false},
"callerIdentity":{"type":"object","required":["extension_id","component_id","instance_id"],"properties":{"extension_id":{"type":"string","minLength":1},"component_id":{"type":"string","minLength":1},"instance_id":{"type":"string","minLength":1}},"additionalProperties":false},
"scopeIdentity":{"type":"object","required":["kind","revision"],"properties":{"kind":{"type":"string","enum":["harness","workspace","operation","invocation"]},"revision":{"type":"string"},"workspace":{"type":"string"},"session_id":{"type":"string"},"operation_id":{"type":"string"},"invocation_id":{"type":"string"},"purpose":{"type":"string"}},"additionalProperties":false},
"callContext":{"type":"object","required":["extension_id","component_id","instance_id","lifetime","scope","revision"],"properties":{"extension_id":{"type":"string","minLength":1},"component_id":{"type":"string","minLength":1},"instance_id":{"type":"string","minLength":1},"lifetime":{"type":"string","enum":["harness","workspace","operation","invocation"]},"scope":{"$ref":"#/$defs/scopeIdentity"},"revision":{"type":"string"},"section":{"$ref":"#/$defs/jsonBytes"},"session_id":{"type":"string"},"operation_id":{"type":"string"},"invocation_id":{"type":"string"},"purpose":{"type":"string"},"request_id":{"type":"string"},"tool_call_id":{"type":"string"},"effect_id":{"type":"string"},"caller":{"$ref":"#/$defs/callerIdentity"}},"additionalProperties":false},
"agent":{"type":"object","properties":{"name":{"type":"string"},"description":{"type":"string"},"model":{"type":"string"},"tools":{"type":"array","items":{"type":"string"}},"capabilities":{"type":"array","items":{"type":"string"}},"readonly":{"type":"boolean"},"subagent":{"type":"boolean"},"write_dir":{"type":"string"},"definition":{"$ref":"#/$defs/definitionRef"}},"additionalProperties":false},
"strategySelection":{"type":"object","required":["id"],"properties":{"id":{"type":"string"},"settings_base64":{"$ref":"#/$defs/jsonBytes"}},"additionalProperties":false},
"agentInputs":{"type":"object","required":["instructions","assembly_mode","tools","capabilities","constraints","strategy"],"properties":{"model":{"$ref":"#/$defs/modelRef"},"instructions":{"type":"string"},"assembly_mode":{"type":"string","enum":["none","simple","full"]},"tools":{"type":"array","items":{"type":"string"}},"capabilities":{"type":"array","items":{"type":"string"}},"constraints":{"$ref":"#/$defs/constraints"},"strategy":{"$ref":"#/$defs/strategySelection"}},"additionalProperties":false},
"toolContext":{"type":"object","required":["session","model","revision","workspace","admitted_entry","constraints","agent_types","calls"],"properties":{"session":{"$ref":"#/$defs/sessionIdentity"},"model":{"$ref":"#/$defs/modelRef"},"revision":{"type":"string"},"workspace":{"type":"string"},"admitted_entry":{"$ref":"#/$defs/entryRef"},"constraints":{"$ref":"#/$defs/constraints"},"config":{"$ref":"#/$defs/jsonBytes"},"agent_types":{"type":"array","items":{"$ref":"#/$defs/agent"}},"calls":{"$ref":"#/$defs/callContext"}},"additionalProperties":false},
"rules":{"type":"object","properties":{"global_text":{"type":"string"},"workspace_text":{"type":"string"},"warnings":{"type":"array","items":{"$ref":"#/$defs/warning"}}},"additionalProperties":false},
"warning":{"type":"object","required":["kind","message"],"properties":{"kind":{"type":"string"},"message":{"type":"string"}},"additionalProperties":false},
"assembly":{"type":"object","required":["identity","instructions","mode"],"properties":{"identity":{"type":"string"},"instructions":{"type":"string"},"mode":{"type":"string","enum":["full","simple","none"]},"rules":{"$ref":"#/$defs/rules"},"workspace":{"type":"string"},"session_start":{"type":"string"},"os":{"type":"string"},"shell":{"type":"string"},"os_description":{"type":"string"}},"additionalProperties":false},
"preparationInput":{"type":"object","required":["session","model","constraints","assembly","prompt","catalog","tools"],"properties":{"session":{"$ref":"#/$defs/sessionIdentity"},"model":{"$ref":"#/$defs/modelRef"},"constraints":{"$ref":"#/$defs/constraints"},"assembly":{"$ref":"#/$defs/assembly"},"prompt":{"type":"string"},"catalog":{"type":"array","items":{"$ref":"#/$defs/toolDefinition"}},"tools":{"type":"array","items":{"$ref":"#/$defs/toolDefinition"}},"settings":{"$ref":"#/$defs/jsonBytes"}},"additionalProperties":false},
"preparationConsequence":{"type":"object","properties":{"prompt":{"type":"string"},"descriptions":{"type":"object","additionalProperties":{"type":"string"}},"tool_names":{"type":"array","items":{"type":"string"}}},"additionalProperties":false},
"requestHookInput":{"type":"object","required":["request_id","canonical_messages","messages","captured_prompt","prompt","catalog","tools"],"properties":{"request_id":{"type":"string","minLength":1},"canonical_messages":{"type":"array","items":{"$ref":"#/$defs/message"}},"messages":{"type":"array","items":{"$ref":"#/$defs/message"}},"checkpoint_entry_id":{"type":"string"},"boundary_entry_id":{"type":"string"},"captured_prompt":{"type":"string"},"prompt":{"type":"string"},"catalog":{"type":"array","items":{"$ref":"#/$defs/toolDefinition"}},"tools":{"type":"array","items":{"$ref":"#/$defs/toolDefinition"}}},"additionalProperties":false},
"requestConsequence":{"type":"object","properties":{"prompt":{"type":"string"},"messages":{"type":"array","items":{"$ref":"#/$defs/message"}},"tool_names":{"type":"array","items":{"type":"string"}}},"additionalProperties":false},
"hookIdentity":{"type":"object","required":["extension_id","hook_id"],"properties":{"extension_id":{"type":"string","minLength":1},"hook_id":{"type":"string","minLength":1}},"additionalProperties":false},
"requestEvidence":{"type":"object","required":["boundary_entry_id","prompt","tool_names"],"properties":{"checkpoint_entry_id":{"type":"string"},"boundary_entry_id":{"type":"string"},"prompt":{"type":"string"},"messages":{"type":"array","items":{"$ref":"#/$defs/message"}},"tool_names":{"type":"array","items":{"type":"string"}},"hook_ids":{"type":"array","items":{"$ref":"#/$defs/hookIdentity"}}},"additionalProperties":false},
"modelRequest":{"type":"object","required":["messages","tools"],"properties":{"messages":{"type":"array","items":{"$ref":"#/$defs/message"}},"tools":{"type":"array","items":{"$ref":"#/$defs/toolDefinition"}}},"additionalProperties":false},
"planInput":{"type":"object","required":["trigger","messages","boundary_entry_id","instructions","tools","context_window","output_reserve"],"properties":{"trigger":{"type":"string","enum":["automatic","manual"]},"messages":{"type":"array","items":{"$ref":"#/$defs/message"}},"checkpoint_entry_id":{"type":"string"},"boundary_entry_id":{"type":"string"},"instructions":{"type":"string"},"tools":{"type":"array","items":{"$ref":"#/$defs/toolDefinition"}},"contributions":{"$ref":"#/$defs/requestEvidence"},"measurement":{"$ref":"#/$defs/usageCount"},"context_window":{"type":"integer"},"output_reserve":{"type":"integer"},"settings":{"$ref":"#/$defs/jsonBytes"}},"additionalProperties":false},
"modelPlan":{"type":"object","required":["request"],"properties":{"request":{"$ref":"#/$defs/modelRequest"},"state":{"$ref":"#/$defs/jsonBytes"}},"additionalProperties":false},
"checkpointItem":{"oneOf":[{"type":"object","required":["retain"],"properties":{"retain":{"type":"integer","minimum":0}},"additionalProperties":false},{"type":"object","required":["context"],"properties":{"context":{"$ref":"#/$defs/message"}},"additionalProperties":false}]},
"checkpointPlan":{"type":"object","required":["boundary_entry_id","items"],"properties":{"boundary_entry_id":{"type":"string","minLength":1},"items":{"type":"array","minItems":1,"items":{"$ref":"#/$defs/checkpointItem"}}},"additionalProperties":false},
"planOutput":{"oneOf":[{"type":"object","required":["kind"],"properties":{"kind":{"const":"none"}},"additionalProperties":false},{"type":"object","required":["kind","model"],"properties":{"kind":{"const":"model"},"model":{"$ref":"#/$defs/modelPlan"}},"additionalProperties":false},{"type":"object","required":["kind","checkpoint"],"properties":{"kind":{"const":"checkpoint"},"checkpoint":{"$ref":"#/$defs/checkpointPlan"}},"additionalProperties":false}]},
"modelOutput":{"type":"object","required":["status","source","detail"],"properties":{"status":{"type":"string","enum":["completed","errored","interrupted"]},"source":{"$ref":"#/$defs/modelRef"},"message":{"$ref":"#/$defs/message"},"usage":{"$ref":"#/$defs/usageCount"},"detail":{"type":"string"}},"additionalProperties":false},
"consumeInput":{"type":"object","required":["input","output"],"properties":{"input":{"$ref":"#/$defs/planInput"},"state":{"$ref":"#/$defs/jsonBytes"},"output":{"$ref":"#/$defs/modelOutput"}},"additionalProperties":false}
`

func defsSchema(body string) json.RawMessage {
	return raw(`{"$defs":{` + fixedDefs + `},` + strings.TrimPrefix(body, "{"))
}

func method(name string, mode Mode, input, success, failure, partial string) Method {
	out := Method{Name: name, Mode: mode, Input: raw(input), Success: raw(success)}
	if failure != "" {
		out.Error = raw(failure)
	}
	if partial != "" {
		out.Partial = raw(partial)
	}
	return out
}

// toolMethods is the fixed melta.tool/v1 method set shared by native and
// worker implementations.
func toolMethods() []Method {
	return []Method{
		method("describe", ModePure,
			string(defsSchema(`{"type":"object","required":["model","session","constraints","agent_types","revision","inputs"],"properties":{"config":{"$ref":"#/$defs/jsonBytes"},"model":{"$ref":"#/$defs/modelRef"},"session":{"$ref":"#/$defs/sessionIdentity"},"constraints":{"$ref":"#/$defs/constraints"},"agent_types":{"type":"array","items":{"$ref":"#/$defs/agent"}},"revision":{"type":"string"},"inputs":{"$ref":"#/$defs/agentInputs"}},"additionalProperties":false}`)),
			string(defsSchema(`{"type":"object","required":["definition","available"],"properties":{"definition":{"$ref":"#/$defs/toolDefinition"},"available":{"type":"boolean"}},"additionalProperties":false}`)), "", ""),
		method("normalize", ModePure,
			string(defsSchema(`{"type":"object","required":["context","call"],"properties":{"context":{"$ref":"#/$defs/toolContext"},"call":{"$ref":"#/$defs/toolCall"}},"additionalProperties":false}`)),
			string(defsSchema(`{"type":"object","required":["arguments"],"properties":{"arguments":{"$ref":"#/$defs/jsonBytes"}},"additionalProperties":false}`)), "", ""),
		method("prepare", ModePure,
			string(defsSchema(`{"type":"object","required":["call","context"],"properties":{"call":{"$ref":"#/$defs/toolCall"},"context":{"$ref":"#/$defs/toolContext"}},"additionalProperties":false}`)),
			string(defsSchema(`{"$ref":"#/$defs/toolPlan"}`)), "", ""),
		method("execute", ModeServiceIO,
			string(defsSchema(`{"type":"object","required":["context","plan"],"properties":{"context":{"$ref":"#/$defs/toolContext"},"plan":{"$ref":"#/$defs/toolPlan"}},"additionalProperties":false}`)),
			string(defsSchema(`{"$ref":"#/$defs/toolOutcome"}`)), "", ""),
	}
}

func hookMethods(hooks []Hook) []Method {
	methods := make([]Method, 0, len(hooks))
	for _, hook := range hooks {
		mode := ModePure
		switch hook.Boundary {
		case BoundaryPrepare:
			methods = append(methods, method(string(hook.Boundary), ModePure,
				string(defsSchema(`{"$ref":"#/$defs/preparationInput"}`)),
				string(defsSchema(`{"$ref":"#/$defs/preparationConsequence"}`)), "", ""))
		case BoundaryRequest:
			failure := ""
			if hook.Mode == HookModeEffectful {
				mode = ModeServiceIO
				failure = string(defsSchema(`{"$ref":"#/$defs/hookFailure"}`))
			}
			methods = append(methods, method(string(hook.Boundary), mode,
				string(defsSchema(`{"$ref":"#/$defs/requestHookInput"}`)),
				string(defsSchema(`{"$ref":"#/$defs/requestConsequence"}`)), failure, ""))
		case BoundaryBeforeTool:
			failure := ""
			if hook.Mode == HookModeEffectful {
				mode = ModeServiceIO
				failure = string(defsSchema(`{"$ref":"#/$defs/hookFailure"}`))
			}
			methods = append(methods, method(string(hook.Boundary), mode,
				string(defsSchema(`{"$ref":"#/$defs/toolCall"}`)),
				string(defsSchema(`{"type":"object","required":["arguments"],"properties":{"arguments":{"$ref":"#/$defs/jsonBytes"}},"additionalProperties":false}`)), failure, ""))
		case BoundaryAfterTool:
			methods = append(methods, method(string(hook.Boundary), ModePure,
				string(defsSchema(`{"type":"object","required":["outcome","content"],"properties":{"outcome":{"$ref":"#/$defs/toolOutcome"},"content":{"type":"string"}},"additionalProperties":false}`)),
				string(defsSchema(`{"type":"object","required":["content"],"properties":{"content":{"type":"string"}},"additionalProperties":false}`)), "", ""))
		}
	}
	return methods
}

func strategyMethods() []Method {
	return []Method{
		method("plan", ModePure,
			string(defsSchema(`{"$ref":"#/$defs/planInput"}`)),
			string(defsSchema(`{"$ref":"#/$defs/planOutput"}`)), "", ""),
		method("consume", ModePure,
			string(defsSchema(`{"$ref":"#/$defs/consumeInput"}`)),
			string(defsSchema(`{"$ref":"#/$defs/planOutput"}`)), "", ""),
	}
}

// ToolCapability constructs the fixed melta.tool/v1 capability with the one
// model-visible tool identity.
func ToolCapability(id string) Capability {
	return Capability{ID: id, ContractID: ContractTool, ContractVersion: ContractVersion, Kind: KindTool, Methods: toolMethods()}
}

// HookCapability constructs the fixed melta.hook/v1 capability for one or more
// declared boundaries. At most one hook per boundary is allowed; prepare and
// after_tool must use HookModePure.
func HookCapability(id string, hooks ...Hook) Capability {
	return Capability{ID: id, ContractID: ContractHook, ContractVersion: ContractVersion, Kind: KindHook, Methods: hookMethods(hooks), Hooks: append([]Hook(nil), hooks...)}
}

// SingleHook builds one Hook value for HookCapability.
func SingleHook(boundary HookBoundary, mode HookMode) Hook {
	return Hook{Boundary: boundary, Mode: mode}
}

// StrategyCapability constructs the fixed melta.context_strategy/v1 capability
// with its pre-admission metadata.
func StrategyCapability(id string, requiresContextWindow bool) Capability {
	return Capability{ID: id, ContractID: ContractContextStrategy, ContractVersion: ContractVersion, Kind: KindStrategy, Methods: strategyMethods(), Strategy: &StrategyDecl{RequiresContextWindow: requiresContextWindow}}
}

// ServiceCapability constructs one author-defined named service capability.
func ServiceCapability(id, contractID string, methods ...Method) Capability {
	return Capability{ID: id, ContractID: contractID, ContractVersion: ContractVersion, Kind: KindService, Methods: methods}
}

// ServiceMethod constructs one named-service method declaration.
func ServiceMethod(name string, mode Mode, input, success, failure, partial string) Method {
	return method(name, mode, input, success, failure, partial)
}

// PureMethod and ServiceIOMethod are the two shorthand constructors for the
// common no-failure method shape.
func PureMethod(name, input, success string) Method {
	return method(name, ModePure, input, success, "", "")
}

func ServiceIOMethod(name, input, success string) Method {
	return method(name, ModeServiceIO, input, success, "", "")
}

// cleanupMethods is the fixed melta.session_cleanup/v1 method set: one
// idempotent service call over the deleted Session identity.
func cleanupMethods() []Method {
	return []Method{method("cleanup", ModeServiceIO,
		`{"type":"object","required":["session_id"],"properties":{"session_id":{"type":"string","minLength":1}},"additionalProperties":false}`,
		`{"type":"null"}`, "", "")}
}

// CleanupCapability constructs the fixed melta.session_cleanup/v1 capability.
// It is the one cleanup declaration a claiming extension supplies.
func CleanupCapability(id string) Capability {
	return Capability{ID: id, ContractID: ContractSessionCleanup, ContractVersion: ContractVersion, Kind: KindService, Methods: cleanupMethods()}
}

// hostDataDefinition is the fixed melta.data/v1 host service: the extension's
// own Session and cache namespaces plus the durable external-data claim.
func hostDataDefinition() CapabilityDefinition {
	return CapabilityDefinition{
		ContractID:      ContractData,
		ContractVersion: ContractVersion,
		Kind:            KindService,
		Methods: []Method{
			method("session_namespace", ModeServiceIO,
				`{"type":"object","additionalProperties":false}`, `{"type":"object","required":["directory"],"properties":{"directory":{"type":"string","minLength":1}},"additionalProperties":false}`, "", ""),
			method("cache_namespace", ModeServiceIO,
				`{"type":"object","additionalProperties":false}`, `{"type":"object","required":["directory"],"properties":{"directory":{"type":"string","minLength":1}},"additionalProperties":false}`, "", ""),
			method("claim_external_data", ModeServiceIO,
				`{"type":"object","additionalProperties":false}`, `{"type":"null"}`, "", ""),
		},
	}
}

// hostPromptDefinition is the fixed melta.prompt/v1 host service: one pure
// assembler over captured values.
func hostPromptDefinition() CapabilityDefinition {
	return CapabilityDefinition{
		ContractID:      ContractPrompt,
		ContractVersion: ContractVersion,
		Kind:            KindService,
		Methods: []Method{method("assemble", ModePure,
			string(defsSchema(`{"$ref":"#/$defs/assembly"}`)),
			`{"type":"string"}`, "", "")},
	}
}

// hostChildLaunchDefinition is the fixed melta.child_launch/v1 host service:
// data-only child launch whose parent and permission pairs come from the
// host-issued CallContext, never from arguments.
func hostChildLaunchDefinition() CapabilityDefinition {
	return CapabilityDefinition{
		ContractID:      ContractChildLaunch,
		ContractVersion: ContractVersion,
		Kind:            KindService,
		Methods: []Method{method("launch", ModeServiceIO,
			string(defsSchema(`{"type":"object","required":["definition","prompt","operation_id","max_concurrent","output_limit"],"properties":{"definition":{"$ref":"#/$defs/definitionRef"},"prompt":{"type":"string"},"operation_id":{"type":"string","minLength":1},"max_concurrent":{"type":"integer","minimum":1},"output_limit":{"type":"integer","minimum":1}},"additionalProperties":false}`)),
			`{"type":"object","required":["child_session_id"],"properties":{"child_session_id":{"type":"string","minLength":1}},"additionalProperties":false}`, "", "")},
	}
}
