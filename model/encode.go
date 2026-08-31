package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ResolvedTransport is one immutable resolved input for the fixed OpenAI-compatible chat transport: target identity, endpoint and credentials, the three sidecar extra-body layers, effective wire system role, streamed-usage flag, and target protocol metadata. The caller resolves every value; this package performs no catalog access, environment lookup, configuration fallback, provider discovery, model adaptation, or credential persistence. Encode deep-copies every map, slice, and raw byte on entry, so mutating the input afterwards never affects encoding.
type ResolvedTransport struct {
	Model             ModelRef            // target model identity; must be complete (zero or partial is rejected).
	BaseURL           string              // base URL for endpoint construction; empty uses the default OpenAI v1 endpoint.
	APIKey            string              // resolved API key value; empty omits the Authorization header entirely.
	Headers           map[string]string   // resolved headers that overwrite matching built-in defaults (case-insensitively, http.Header.Set semantics) after they are set.
	ProviderExtraBody Extra               // provider-level top-level extra-body layer, merged under model and runtime layers.
	ModelExtraBody    Extra               // model-level top-level extra-body layer, merged under the runtime layer.
	WireSystemRole    string              // effective wire role canonical system messages encode as: "system", "user" or "developer"; empty defaults to "system".
	StreamedUsage     bool                // when true the body carries stream_options with include_usage:true; never derived from anything else.
	ProtocolFamily    string              // target protocol family used by the cross-model replay check only.
	MustPreserve      []string            // ordered message-extra fields a kept assistant-with-tool-calls message should carry; each missing one produces one warning in this order.
	Drop              map[string]bool     // drop key set applied at every retained scope (message, content part, tool call) whenever extras are kept.
	SourceFamilies    map[ModelRef]string // source protocol family keyed by model identity, consulted only for same-provider cross-model replay; never used for the target itself.
	WireDebugDir      string              // optional wire-debug directory consumed by the transport layer; empty disables diagnostics and Encode does not touch it.
}

// ErrReservedKeys is returned when any extra-body sidecar layer carries a key owned by this encoder's base body. The concrete error identifies every present reserved key exactly once each.
var ErrReservedKeys = errors.New("extra body carries reserved keys")

// ErrInvalidWireSystemRole is returned when the resolved wire system role is not one of system, user or developer (empty defaults to system and does not fail).
var ErrInvalidWireSystemRole = errors.New("wire system role must be one of system, user or developer")

// WarningMustPreserveMissing is the kind value for a protocol metadata warning: a replay-kept assistant message with tool calls lacks an ordered must-preserve field in its extra. The target identity and zero-based message index are diagnostic context on the same value.
const WarningMustPreserveMissing = "protocol_must_preserve_missing"

// reservedKeys is the closed set of top-level request-body keys owned by this encoder's base body; their presence in any sidecar layer rejects encoding.
var reservedKeys = []string{"model", "messages", "tools", "tool_choice", "stream", "stream_options", "max_tokens", "max_completion_tokens", "n"}

// Encode serializes one logical request into an owned OpenAI-compatible chat body plus ordered protocol warnings for the given resolved transport input and per-call runtime extra layer. It performs no network or filesystem I/O: validation, replay filtering, sidecar merge, wire-system-role mapping, message/content/tool wire shapes (exactly reproducing the retained legacy encoder minus its persistence fields), warning collection, header construction, and endpoint resolution all happen here as pure computation over deep-copied inputs.
func Encode(in ResolvedTransport, req Request, runtimeExtras map[string]json.RawMessage) (json.RawMessage, []ProtocolWarning, error) {
	rt := cloneResolvedInput(in) // own every resolved value before reading it further.

	if !rt.Model.complete() {
		return nil, nil, fmt.Errorf("%w: target model identity must be complete", ErrInvalidModelRef)
	}
	systemRole, err := resolveWireSystemRole(rt.WireSystemRole)
	if err != nil {
		return nil, nil, err
	}

	runtimeLayer := Extra(runtimeExtras).Clone() // the per-call layer enters validation and merge as an owned copy.

	var reserved []string // every present reserved key across all three layers, one per distinct key; presence is checked in one pass before any value parsing so malformed values cannot hide a reservation.
	for _, layer := range append([]Extra{rt.ProviderExtraBody, rt.ModelExtraBody}, runtimeLayer) {
		reserved = collectReservedKeys(reserved, layer)
	}
	if len(reserved) > 0 {
		return nil, nil, &reservedKeyError{keys: reserved} // rejection produces no body bytes.
	}

	for name, layer := range map[string]Extra{"provider": rt.ProviderExtraBody, "model": rt.ModelExtraBody, "runtime": runtimeLayer} {
		if err := validateExtraValues(layer); err != nil {
			return nil, nil, fmt.Errorf("%s extra body: %w", name, err)
		}
	}

	request, err := NewRequest(req) // re-validate and own the logical request at this trust boundary.
	if err != nil {
		return nil, nil, err
	}

	bodyMessages := make([]map[string]any, 0, len(request.Messages))
	var warnings []ProtocolWarning
	for i, msg := range request.Messages {
		policy := replayPolicyFor(rt.Model, rt.SourceFamilies, rt.ProtocolFamily, rt.Drop, msg.Source)
		obj := serializeMessage(msg, systemRole, policy) // canonical keys are written after the extras so they can never be forged by an extra.
		bodyMessages = append(bodyMessages, obj)

		if !policy.keep || msg.Role != RoleAssistant || len(msg.ToolCalls) == 0 {
			continue // warnings exist only for replay-kept assistant messages that carry tool calls.
		}
		for _, field := range rt.MustPreserve { // message order first, then the ordered must-preserve list; raw extra presence suppresses per retained semantics.
			if _, ok := msg.Extra[field]; ok {
				continue
			}
			warnings = append(warnings, ProtocolWarning{
				Kind:         WarningMustPreserveMissing,
				Message:      fmt.Sprintf("%s assistant message %d has tool calls but is missing protocol metadata field %q", rt.Model.String(), i, field),
				Target:       rt.Model,
				Field:        field,
				MessageIndex: i,
			})
		}
	}

	body := map[string]any{ // base body exactly as specified; optional keys appear only with their trigger data. The wire model field is the bare target name (the endpoint's own vocabulary), not Lightcode's provider-prefixed identity rendering.
		"model":    rt.Model.Model,
		"messages": bodyMessages,
		"stream":   true,
		"n":        1,
	}
	if len(request.Tools) > 0 {
		body["tools"] = encodeTools(request.Tools)
		body["tool_choice"] = "auto" // emitted together with tools only; strict is never part of this encoder's output.
	}
	if rt.StreamedUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}

	for _, layer := range []Extra{rt.ProviderExtraBody, rt.ModelExtraBody, runtimeLayer} { // shallow merge: later layers win per key; values are cloned into the body.
		for k, v := range layer {
			if strings.HasPrefix(k, "_lightcode_") {
				continue // rule 9 at top-level scope too: private persistence fields never become wire data from any sidecar layer (reserved keys already rejected above).
			}
			body[k] = cloneRaw(v)
		}
	}

	bytesOut, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("encode chat request body: %w", err) // unreachable for the value kinds this encoder produces; kept as a defensive boundary.
	}
	return bytesOut, warnings, nil
}

// reservedKeyError carries every present reserved key found across the sidecar layers so one error identifies them all without duplicating any. Key order is diagnostic formatting only.
type reservedKeyError struct {
	keys []string // distinct offending keys in first-seen layer order.
}

func (e *reservedKeyError) Error() string {
	quoted := make([]string, len(e.keys))
	for i, key := range e.keys {
		quoted[i] = fmt.Sprintf("%q", key)
	}
	return fmt.Sprintf("%s: %s", ErrReservedKeys, strings.Join(quoted, ", "))
}

func (e *reservedKeyError) Unwrap() error { return ErrReservedKeys } // errors.Is reaches the sentinel; errors.As reaches this typed shape.

// collectReservedKeys appends each reserved key present in layer that has not already been reported.
func collectReservedKeys(reported []string, layer Extra) []string {
	for _, key := range reservedKeys {
		if _, ok := layer[key]; !ok || containsString(reported, key) {
			continue
		}
		reported = append(reported, key)
	}
	return reported
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// resolveWireSystemRole resolves the effective wire system role: empty defaults to "system", and any non-empty value outside the closed set is an entry error.
func resolveWireSystemRole(role string) (string, error) {
	if role == "" {
		return string(RoleSystem), nil
	}
	switch Role(role) { // developer exists only as a wire system-role value here; it can never be stored on canonical messages.
	case "system", "user", "developer":
		return role, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidWireSystemRole, role)
	}
}

// encodeTools renders tool definitions in request order as the OpenAI function shape with name, description, and parameters; strict is never emitted.
func encodeTools(tools []ToolDefinition) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, def := range tools { // descriptions stay verbatim (including empty); parameter bytes pass through as raw JSON objects.
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        def.Name,
				"description": def.Description,
				"parameters":  cloneRaw(def.Parameters),
			},
		})
	}
	return out
}

// replayPolicy is the per-message extra retention decision: whether extras are kept at all and which keys the target drop set removes from every retained scope.
type replayPolicy struct {
	keep bool            // false strips message, content-part, tool-call, and opaque parts entirely.
	drop map[string]bool // shared cloned target drop set; read-only here.
}

// replayPolicyFor applies the single replay policy to one message source identity: same provider plus same model keeps without any family lookup (field-based compare); otherwise a missing/zero source or different provider strips, and only an equal non-empty source-and-target family pair keeps across models of one provider. The target drop set rides along whenever extras are kept.
func replayPolicyFor(target ModelRef, sourceFamilies map[ModelRef]string, targetFamily string, drop map[string]bool, source ModelRef) replayPolicy {
	policy := replayPolicy{drop: drop} // every decision carries the drop set; it applies exactly when keep is true and rawExtraMap enforces that.
	if source.IsZero() || !target.complete() || source.Provider != target.Provider {
		return policy // missing or cross-provider sources never retain provider-specific extras.
	}
	if source.Model == target.Model {
		policy.keep = true // same identity: no family lookup happens at all, so absent SourceFamilies data still keeps.
		return policy
	}
	sourceFamily := sourceFamilies[source] // consulted only for the remaining case of one provider with a different model.
	if sourceFamily != "" && sourceFamily == targetFamily {
		policy.keep = true
	}
	return policy
}

// serializeMessage renders one canonical message as its wire object: retained extras first, then every canonical field overwriting any matching extra key so role/content/refusal/tool_calls/tool_call_id/name can never come from an extra. System messages encode with the resolved wire system role; all other roles are unchanged.
func serializeMessage(msg Message, systemRole string, policy replayPolicy) map[string]any {
	obj := rawExtraMap(msg.Extra, policy, messageExtraAllowed) // retained extras first; canonical fields below overwrite any matching key.

	role := msg.Role // every non-system role passes through unchanged on the wire.
	if role == RoleSystem {
		role = Role(systemRole) // only system messages adopt the resolved wire system role (system/user/developer).
	}
	obj["role"] = string(role)

	content, ok := serializeContent(msg.Content, policy)
	if ok {
		obj["content"] = content // a message with no surviving parts omits the key entirely.
	}
	if msg.Refusal != "" {
		obj["refusal"] = msg.Refusal
	}
	if len(msg.ToolCalls) > 0 { // assistant tool calls: id, explicit function type, nested name/arguments (raw JSON string).
		calls := make([]map[string]any, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			callObj := rawExtraMap(call.Extra, policy, toolCallExtraAllowed) // extras first; canonical keys overwrite below.
			if call.ID != "" {
				callObj["id"] = call.ID
			}
			callObj["type"] = "function" // canonical calls store no wire type: the function shape is always explicit on the wire (legacy emitted a stored type only when one existed).
			if call.Name != "" || len(call.Arguments) > 0 {
				callObj["function"] = map[string]any{
					"name":      call.Name,
					"arguments": string(call.Arguments), // raw argument bytes rendered as the JSON-string value this wire shape carries.
				}
			}
			calls = append(calls, callObj)
		}
		obj["tool_calls"] = calls
	}
	if msg.ToolCallID != "" {
		obj["tool_call_id"] = msg.ToolCallID // tool results carry the original call id plus optional name and content.
	}
	if msg.Name != "" {
		obj["name"] = msg.Name // canonical message name emits when present.
	}
	return obj
}

// serializeContent renders one message's ordered parts: a single text part without surviving extras becomes the plain string; otherwise every retained part becomes an object whose own kind fields overwrite matching extra keys, with opaque parts omitted entirely whenever replay strips extras and their structural wire type plus retained extras reconstructing them when kept. An all-stripped body returns ok=false so no content key is emitted rather than empty data being invented.
func serializeContent(parts []ContentPart, policy replayPolicy) (any, bool) {
	if len(parts) == 1 && parts[0].Kind == PartText { // string form only when nothing else rides on that single part after filtering.
		extra := rawExtraMap(parts[0].Extra, policy, contentPartExtraAllowed)
		if len(extra) == 0 {
			return parts[0].Text, true
		}
	}

	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts { // order is preserved; opaque reconstruction uses the global allowlist so original wire fields survive.
		obj := rawExtraMap(part.Extra, policy, allowedPartKeys(part.Kind))
		switch part.Kind {
		case PartText:
			obj["type"] = string(PartText)
			obj["text"] = part.Text
		case PartImageURL:
			obj["type"] = string(PartImageURL)
			obj["image_url"] = map[string]any{"url": part.URL}
		default: // opaque — the closed kind set makes this arm exhaustive for validated parts.
			obj["type"] = part.OpaqueWireType // structural original wire type always wins over any colliding extra key; extras carry only the remaining fields.
		}

		if part.Kind == PartOpaque && !policy.keep {
			continue // opaque data is provider-specific: stripped messages drop these parts from the array entirely.
		}
		out = append(out, obj)
	}
	if len(out) == 0 {
		return nil, false // every part was stripped (e.g., an all-opaque message under cross-scope replay): the wire object carries no content key at all.
	}
	return out, true
}

// rawExtraMap projects one retained scope's extras through the policy: !keep contributes nothing; otherwise each surviving value is deep-copied after passing both the target drop set and the scope allowlist (which always excludes canonical keys, reserved top-level keys, and _lightcode_* private fields).
func rawExtraMap(extra Extra, policy replayPolicy, allowed func(string) bool) map[string]any {
	out := make(map[string]any)
	if !policy.keep {
		return out // stripped scopes contribute nothing to their wire object.
	}
	for key, value := range extra {
		if policy.drop[key] || !allowed(key) {
			continue
		}
		out[key] = cloneRaw(value)
	}
	return out
}

// globalExtraAllowed is the common floor for every scope: empty keys and _lightcode_* private persistence fields are never wire data, nor can any reserved top-level key come from extras. Retained legacy denylist semantics verbatim.
func globalExtraAllowed(key string) bool {
	if key == "" || strings.HasPrefix(key, "_lightcode_") {
		return false
	}
	for _, reserved := range reservedKeys { // the closed set is shared with sidecar rejection so both agree on ownership by construction.
		if key == reserved {
			return false
		}
	}
	return true
}

func messageExtraAllowed(key string) bool { // canonical object keys belong to serializeMessage's own fields, never to an extra value.
	if !globalExtraAllowed(key) {
		return false
	}
	switch key {
	case "role", "content", "refusal", "tool_calls", "tool_call_id", "name", "function_call",
		"system_fingerprint", "service_tier", "id", "created", "object": // retained legacy denylist. ("model" is already excluded as reserved.)
		return false
	default:
		return true
	}
}

func toolCallExtraAllowed(key string) bool { // canonical call fields belong to the calls loop; index is part of the retained producer's denylist even though it never appears on completed requests.
	if !globalExtraAllowed(key) {
		return false
	}
	switch key {
	case "id", "type", "function", "index":
		return false
	default:
		return true
	}
}

func contentPartExtraAllowed(key string) bool { // canonical part-shape fields belong to the kind switch.
	if !globalExtraAllowed(key) {
		return false
	}
	switch key {
	case "type", "text", "image_url":
		return false
	default:
		return true
	}
}

func allowedPartKeys(kind PartKind) func(string) bool { // opaque parts keep the global floor only so their original wire object reconstructs from extras plus structural type.
	if kind == PartOpaque {
		return globalExtraAllowed
	}
	return contentPartExtraAllowed
}

// cloneResolvedInput deep-copies every reference-typed field of one resolved transport input: scalar fields move with the struct value copy itself, and every non-nil map or slice is cloned even when empty so retained collections never alias caller storage (nil inputs stay nil). Both accepting boundaries — direct encoder invocation and transport construction — share this helper.
func cloneResolvedInput(in ResolvedTransport) ResolvedTransport {
	out := in // identity strings, endpoint/key values, flags, role, debug dir.
	if out.Headers != nil {
		headers := make(map[string]string, len(out.Headers)) // string values are immutable; copying the entries IS the deep copy here (an empty map still gets its own storage).
		for k, v := range out.Headers {
			headers[k] = v
		}
		out.Headers = headers
	}
	if in.ProviderExtraBody != nil {
		out.ProviderExtraBody = in.ProviderExtraBody.Clone() // owned deep copy or the package's canonical empty form — never a shared reference.
	}
	if in.ModelExtraBody != nil {
		out.ModelExtraBody = in.ModelExtraBody.Clone()
	}
	if out.MustPreserve != nil {
		fields := make([]string, len(out.MustPreserve)) // ordered list: element order is contract-relevant for warning emission (empty lists cloned too).
		copy(fields, out.MustPreserve)
		out.MustPreserve = fields
	}
	if out.Drop != nil {
		dropSet := make(map[string]bool, len(out.Drop)) // a key drops exactly when its boolean value is true (the retained check shape; empty sets cloned too).
		for k, v := range out.Drop {
			dropSet[k] = v
		}
		out.Drop = dropSet
	}
	if in.SourceFamilies != nil {
		fams := make(map[ModelRef]string, len(in.SourceFamilies)) // string values are immutable.
		for k, v := range in.SourceFamilies {
			fams[k] = v
		}
		out.SourceFamilies = fams
	}
	return out
}

// BuildHeaders returns the chat request headers for one resolved transport, built by a single helper path over an http.Header so every entry carries exactly its Set semantics: MIME-canonical storage and case-insensitive matching of existing entries. It sets JSON content type and SSE accept first, Bearer authorization only when a key is present, then applies each resolved header — which therefore overwrites any default (or earlier entry) it matches regardless of spelling, collapsing to one canonical-form entry per name rather than coexisting as separate case variants.
func BuildHeaders(in ResolvedTransport) map[string]string { // pure helper shared by the encoder contract's tests; Commit 3's transport calls it for its physical requests too.
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	if in.APIKey != "" { // empty key omits the Authorization header entirely rather than emitting an empty bearer value.
		headers.Set("Authorization", "Bearer "+in.APIKey)
	}
	for k, v := range in.Headers { // resolved entries win over every matching default or prior entry; Set matches case-insensitively and stores each name once in canonical form (last write wins among same-name inputs).
		headers.Set(k, v)
	}
	out := make(map[string]string, len(headers))
	for key, values := range headers { // one value per retained header by construction of the Set path above.
		out[key] = values[0]
	}
	return out
}

// ChatEndpoint resolves the chat-completions endpoint: trailing slashes removed from a non-empty base plus /chat/completions; the default OpenAI v1 URL is used only when the original BaseURL itself was empty, so every other input — however degenerate after trimming — resolves through trim-plus-suffix. A trailing-slash base such as "https://host/" yields exactly one slash before the path segment (no double-slash).
func ChatEndpoint(in ResolvedTransport) string { // pure helper; Commit 3's transport calls it for its physical requests too.
	if in.BaseURL == "" { // only a truly empty original base takes the default endpoint — no post-trim special case exists or may be added here.
		return defaultOpenAIChatEndpoint
	}
	return strings.TrimRight(in.BaseURL, "/") + chatCompletionsPathSuffix // one or more trailing slashes collapse away before the fixed suffix is appended (a slash-only base trims to an empty host part and keeps exactly that).
}

const (
	defaultOpenAIChatEndpoint = "https://api.openai.com/v1/chat/completions" // used exactly when no usable base is configured; the full URL, not just a host.
	chatCompletionsPathSuffix = "/chat/completions"                          // appended to every non-empty base after slash trimming.
)
