package harness

import "context"

// CallbackKind names one behavioral callback boundary: the closed set of
// Harness-issued callback identities attached to a callback's context.
type CallbackKind string

const (
	CallbackModelAttempt    CallbackKind = "model_attempt"
	CallbackNormalize       CallbackKind = "normalize"
	CallbackRequestHook     CallbackKind = "request_hook"
	CallbackArgumentHook    CallbackKind = "argument_hook"
	CallbackToolPrepare     CallbackKind = "tool_prepare"
	CallbackToolExecute     CallbackKind = "tool_execute"
	CallbackToolRelease     CallbackKind = "tool_release"
	CallbackToolOutput      CallbackKind = "tool_output"
	CallbackStrategyPlan    CallbackKind = "strategy_plan"
	CallbackStrategyConsume CallbackKind = "strategy_consume"
)

// CallbackInfo is the read-only identity of one behavioral callback, issued by
// the owner that produces the callback's actual state: the bound Operation and
// invocation, the producing request, the concrete pending call and any
// committed effect reservation, the callback's own capability identity, and —
// only at tool execution after intent publication — the accepted authorized
// permission pairs. It grants no capability; Runtime binding authority stays
// private.
type CallbackInfo struct {
	Kind                  CallbackKind
	SessionID             string
	OperationID           string
	InvocationID          string
	Purpose               InvocationPurpose
	RequestID             string
	ToolCallID            string
	EffectID              string
	CapabilityID          string
	ExtensionID           string
	AuthorizedPermissions []PermissionRequest
}

// callbackInfoKey is the private context key of the producer-issued callback
// identity.
type callbackInfoKey struct{}

// withCallbackInfo preserves the caller's context and values while attaching
// one producer-issued callback identity. It never cancels the inherited
// context: an accepted model stream's lifetime stays with its own owner.
func withCallbackInfo(ctx context.Context, info CallbackInfo) context.Context {
	return context.WithValue(ctx, callbackInfoKey{}, info)
}

// CallbackInfoFor returns the owned callback identity carried by ctx and
// whether one is present. The permission pairs are a detached copy; the getter
// grants no capability.
func CallbackInfoFor(ctx context.Context) (CallbackInfo, bool) {
	if ctx == nil {
		return CallbackInfo{}, false
	}
	info, ok := ctx.Value(callbackInfoKey{}).(CallbackInfo)
	if !ok {
		return CallbackInfo{}, false
	}
	if info.AuthorizedPermissions != nil {
		info.AuthorizedPermissions = append([]PermissionRequest(nil), info.AuthorizedPermissions...)
	}
	return info, true
}

// callbackInfo fills one callback identity's bound Operation, Session and
// invocation attribution.
func (d *executionDriver) callbackInfo(inv *openedInvocation, info CallbackInfo) CallbackInfo {
	info.SessionID = d.sessionID()
	info.OperationID = d.operationID
	if inv != nil {
		info.InvocationID = inv.request.ID
		info.Purpose = inv.request.Purpose
	}
	return info
}

// sessionID returns the bound Session identity of the admitted Operation.
func (d *executionDriver) sessionID() string {
	c := d.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.graph.Session.Identity.SessionID
}
