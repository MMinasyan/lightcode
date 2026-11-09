package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
	"github.com/MMinasyan/lightcode/protocol"
)

// ErrClosed reports a callback view whose boundary has returned or a supplying
// scope that is closing, closed or canceled.
var ErrClosed = errors.New("extension: closed")

// ScopeIdentity is the host-issued immutable identity of one constructed
// scope: its lifetime, the revision that built the adopted resource set, and
// only the attribution that lifetime carries.
type ScopeIdentity struct {
	Kind         Lifetime `json:"kind"`
	Revision     string   `json:"revision"`
	Workspace    string   `json:"workspace,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	OperationID  string   `json:"operation_id,omitempty"`
	InvocationID string   `json:"invocation_id,omitempty"`
	Purpose      string   `json:"purpose,omitempty"`
}

// Native is one statically registered extension implementation.
type Native struct {
	Declaration Declaration
	Describe    map[string]ToolDescribe
	Open        func(context.Context, InstanceContext) (Instance, error)
}

// Instance is one component's complete constructed contribution: exactly the
// component's declared capability IDs.
type Instance struct {
	Capabilities map[string]Implementation
	Close        func(context.Context, CallContext) error
}

// Implementation is one capability's fixed typed contribution. Exactly the
// fields matching the declaration's kind and declared hook boundaries are
// non-nil.
type Implementation struct {
	Service     map[string]Handler
	Tool        Tool
	Preparation PreparationHook
	Request     RequestHook
	BeforeTool  BeforeToolHook
	AfterTool   AfterToolHook
	Strategy    harness.ContextStrategy
}

func (impl Implementation) validate(capability *CompiledCapability) error {
	fail := func(format string, args ...any) error {
		return invalid("capability %q: %s", capability.ID, fmt.Sprintf(format, args...))
	}
	switch capability.Kind {
	case KindService:
		if impl.Tool != nil || impl.Preparation != nil || impl.Request != nil || impl.BeforeTool != nil || impl.AfterTool != nil || impl.Strategy != nil {
			return fail("service carries a non-service binding")
		}
		if len(impl.Service) != len(capability.Methods) {
			return fail("service supplies %d methods, want %d", len(impl.Service), len(capability.Methods))
		}
		for name := range impl.Service {
			if _, ok := capability.Methods[name]; !ok {
				return fail("service supplies undeclared method %q", name)
			}
		}
		for name := range capability.Methods {
			if impl.Service[name] == nil {
				return fail("service is missing method %q", name)
			}
		}
	case KindTool:
		if impl.Tool == nil {
			return fail("tool binding is nil")
		}
		if impl.Service != nil || impl.Preparation != nil || impl.Request != nil || impl.BeforeTool != nil || impl.AfterTool != nil || impl.Strategy != nil {
			return fail("tool carries another kind's binding")
		}
	case KindHook:
		if impl.Service != nil || impl.Tool != nil || impl.Strategy != nil {
			return fail("hook carries another kind's binding")
		}
		declared := make(map[HookBoundary]bool, len(capability.Hooks))
		for _, hook := range capability.Hooks {
			declared[hook.Boundary] = true
		}
		if declared[BoundaryPrepare] != (impl.Preparation != nil) ||
			declared[BoundaryRequest] != (impl.Request != nil) ||
			declared[BoundaryBeforeTool] != (impl.BeforeTool != nil) ||
			declared[BoundaryAfterTool] != (impl.AfterTool != nil) {
			return fail("hook callbacks do not match the declared boundaries")
		}
	case KindStrategy:
		if impl.Strategy == nil {
			return fail("strategy binding is nil")
		}
		if impl.Service != nil || impl.Tool != nil || impl.Preparation != nil || impl.Request != nil || impl.BeforeTool != nil || impl.AfterTool != nil {
			return fail("strategy carries another kind's binding")
		}
	}
	return nil
}

// ToolDescribe is one declaration-level pure metadata callback. It runs before
// any Operation or invocation component opens and receives no instance,
// binding or resource.
type ToolDescribe func(context.Context, DescribeInput) (ToolDescription, error)

// DescribeInput is one tool describe callback's owned captured input.
type DescribeInput struct {
	Session     harness.SessionIdentity
	Model       model.ModelRef
	Constraints harness.ToolConstraints
	Revision    string
	Config      json.RawMessage
	AgentTypes  []protocol.Agent
	Inputs      harness.AgentInputs
}

// ToolDescription is one static tool definition and its hard-constraint
// eligibility.
type ToolDescription struct {
	Definition model.ToolDefinition
	Available  bool
}

// ToolContext carries one tool callback's owned captured values and the
// host-issued call binding for that step.
type ToolContext struct {
	Session       harness.SessionIdentity
	Model         model.ModelRef
	Revision      string
	Workspace     string
	AdmittedEntry harness.EntryRef
	Constraints   harness.ToolConstraints
	Config        json.RawMessage
	AgentTypes    []protocol.Agent
	Calls         CallContext
}

// Tool is one native extension tool contribution: pure normalization and
// preparation plus the authorized execution.
type Tool interface {
	Normalize(ToolContext, model.ToolCall) (json.RawMessage, error)
	Prepare(context.Context, ToolContext, model.ToolCall) ToolPlan
	Execute(context.Context, ToolContext, ToolPlan) harness.ToolOutcome
}

// ToolPlan is one prepared tool call. Data is opaque bytes owned by the tool
// and returned to Execute unchanged; the host never interprets it.
type ToolPlan struct {
	Permissions        []harness.PermissionRequest
	CanonicalWorkspace string
	CanonicalWriteDir  string
	Immediate          *harness.ToolOutcome
	Data               []byte
}

// Owned returns an independent copy of the plan and its byte fields.
func (p ToolPlan) Owned() ToolPlan {
	out := p
	out.Permissions = append([]harness.PermissionRequest(nil), p.Permissions...)
	if p.Immediate != nil {
		immediate := *p.Immediate
		immediate.Metadata = append(json.RawMessage(nil), p.Immediate.Metadata...)
		immediate.Result.Content = p.Immediate.Result.Content
		out.Immediate = &immediate
	}
	out.Data = append([]byte(nil), p.Data...)
	return out
}

// PreparationInput is one selected preparation contribution's complete owned
// input.
type PreparationInput struct {
	Session     harness.SessionIdentity
	Model       model.ModelRef
	Constraints harness.ToolConstraints
	Assembly    prompt.AssemblyInput
	Prompt      string
	Catalog     []model.ToolDefinition
	Tools       []model.ToolDefinition
	Settings    json.RawMessage
}

// PreparationConsequence is one contribution's closed accepted effect.
type PreparationConsequence struct {
	Prompt       *string
	Descriptions map[string]string
	ToolNames    []string
}

// PreparationHook is one pure preparation contribution.
type PreparationHook func(context.Context, CallContext, PreparationInput) (PreparationConsequence, error)

// RequestHook is one request-boundary contribution over the Harness input.
type RequestHook func(context.Context, CallContext, harness.RequestHookInput) (harness.RequestConsequence, error)

// BeforeToolHook is one argument-repair contribution.
type BeforeToolHook func(context.Context, CallContext, model.ToolCall) (json.RawMessage, error)

// AfterToolHook is one pure model-facing output-shaping contribution.
type AfterToolHook func(context.Context, CallContext, harness.ToolOutputInput) (string, error)

// Handler is one named-service method.
type Handler func(context.Context, CallContext, json.RawMessage) (Result, error)

// Result is one named-service result: exactly a schema-valid success value or
// one declared failure.
type Result struct {
	Value   json.RawMessage `json:"value,omitempty"`
	Failure *Failure        `json:"failure,omitempty"`
}

// Failure is the common domain-failure arm: nonempty Code and Message are
// always valid, while Data and Partial are independent optional payloads
// governed by their method's own declared Error and Partial schemas. A
// zero-length payload is absent; any nonempty bytes, whitespace included, are
// a present payload and must satisfy the declared schema.
type Failure struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	Partial json.RawMessage `json:"partial,omitempty"`
}

// InstanceContext is one Native.Open call's host-issued view: component and
// scope identity, the captured section, and a Call that ends when Open
// returns.
type InstanceContext struct {
	ExtensionID   string
	ComponentID   string
	Version       string
	InstanceID    string
	Lifetime      Lifetime
	Scope         ScopeIdentity
	Revision      string
	Section       json.RawMessage
	DataDir       string
	ReportWarning func(kind, message string)

	Call func(context.Context, string, string, json.RawMessage) (Result, error)
}

// CallerIdentity names the instance that originated one nested call. Provider
// resource checks use this host-issued identity, never guest-edited fields.
type CallerIdentity struct {
	ExtensionID string
	ComponentID string
	InstanceID  string
}

// CallContext is one callback's host-issued view: immutable identities of the
// originating activation/call, the captured call revision and section, and
// its own Call that ends when the callback returns. Execution grants and
// purity are private host state.
type CallContext struct {
	ExtensionID string
	ComponentID string
	InstanceID  string
	Lifetime    Lifetime
	Scope       ScopeIdentity
	Revision    string
	Section     json.RawMessage

	SessionID    string
	OperationID  string
	InvocationID string
	Purpose      string
	RequestID    string
	ToolCallID   string
	EffectID     string

	Caller CallerIdentity

	Call func(context.Context, string, string, json.RawMessage) (Result, error)
}
