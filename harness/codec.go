package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// durableTimeLayout is the landed RFC3339Nano layout of every durable
// register timestamp. Encoding normalizes through UTC; decoding rejects any
// other zone offset.
const durableTimeLayout = time.RFC3339Nano

// invalidInput wraps caller-side codec rejections in the landed invalid-input
// class. Persisted violations are never created here: the graph validator
// surfaces them as CorruptionError instead.
func invalidInput(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// decodePayloadObject requires exactly one complete top-level JSON value that
// is an object, rejecting null, other containers, and trailing content.
func decodePayloadObject(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}
	if obj == nil {
		return nil, errors.New("payload must be a JSON object, not null")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("payload must contain exactly one complete JSON value: %v", err)
	}
	return obj, nil
}

// rejectUnknownMembers enforces the exact closed key set of one payload
// object: every key must be one of the known exact case-sensitive names.
func rejectUnknownMembers(obj map[string]json.RawMessage, known ...string) error {
	allowed := make(map[string]bool, len(known))
	for _, k := range known {
		allowed[k] = true
	}
	for k := range obj {
		if !allowed[k] {
			return fmt.Errorf("unknown member %q", k)
		}
	}
	return nil
}

// member returns one raw member value, rejecting missing required members and
// explicit nulls for both required and optional members: absence encodes by
// omission, never as null.
func member(obj map[string]json.RawMessage, key string, required bool) (json.RawMessage, error) {
	raw, ok := obj[key]
	if !ok {
		if required {
			return nil, fmt.Errorf("required member %q is missing", key)
		}
		return nil, nil
	}
	if trimmed := bytes.TrimSpace(raw); bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("member %q must not be null", key)
	}
	return raw, nil
}

func stringMember(obj map[string]json.RawMessage, key string, required bool) (string, error) {
	raw, err := member(obj, key, required)
	if err != nil || raw == nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("member %q must be a JSON string: %w", key, err)
	}
	return s, nil
}

func int64Member(obj map[string]json.RawMessage, key string, required bool) (int64, error) {
	raw, err := member(obj, key, required)
	if err != nil || raw == nil {
		return 0, err
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("member %q must be a JSON number: %w", key, err)
	}
	return n, nil
}

// objectMember returns one required-or-optional object member decoded with
// the same strictness as a top-level payload object.
func objectMember(obj map[string]json.RawMessage, key string, required bool) (map[string]json.RawMessage, error) {
	raw, err := member(obj, key, required)
	if err != nil || raw == nil {
		return nil, err
	}
	inner, err := decodePayloadObject(raw)
	if err != nil {
		return nil, fmt.Errorf("member %q: %w", key, err)
	}
	return inner, nil
}

// arrayMember returns one array member as raw element values. Empty arrays
// are valid and non-null.
func arrayMember(obj map[string]json.RawMessage, key string, required bool) ([]json.RawMessage, error) {
	raw, err := member(obj, key, required)
	if err != nil || raw == nil {
		return nil, err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("member %q must be a JSON array: %w", key, err)
	}
	return items, nil
}

// extraMember decodes one opaque model.Extra member. Extra contents stay raw
// JSON preserved verbatim: no Harness key rules apply inside them.
func extraMember(obj map[string]json.RawMessage, key string, required bool) (model.Extra, error) {
	raw, err := member(obj, key, required)
	if err != nil || raw == nil {
		return nil, err
	}
	inner, err := decodePayloadObject(raw)
	if err != nil {
		return nil, fmt.Errorf("member %q: %w", key, err)
	}
	extra := make(model.Extra, len(inner))
	for k, v := range inner {
		if !json.Valid(v) {
			return nil, fmt.Errorf("member %q: extra field %q is not valid JSON", key, k)
		}
		extra[k] = model.CloneRaw(v)
	}
	return extra, nil
}

// rawJSONMember decodes one opaque raw-JSON member, returning an owned copy
// of the exact stored bytes.
func rawJSONMember(obj map[string]json.RawMessage, key string, required bool) (json.RawMessage, error) {
	raw, err := member(obj, key, required)
	if err != nil || raw == nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("member %q is not valid JSON", key)
	}
	return model.CloneRaw(raw), nil
}

// encodeTime renders one durable timestamp: nonzero, normalized to UTC in the
// landed RFC3339Nano layout, with a year inside the four-digit RFC3339 range
// so encode never emits what decode refuses.
func encodeTime(t time.Time) (string, error) {
	if t.IsZero() {
		return "", invalidInput("timestamp must not be zero")
	}
	utc := t.UTC()
	if year := utc.Year(); year < 0 || year > 9999 {
		return "", invalidInput("timestamp year %d is outside the four-digit RFC3339 range", year)
	}
	return utc.Format(durableTimeLayout), nil
}

// decodeTime parses one durable timestamp, rejecting zero values and any
// zone offset other than UTC.
func decodeTime(s string) (time.Time, error) {
	t, err := time.Parse(durableTimeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not UTC RFC3339Nano: %w", s, err)
	}
	if t.IsZero() {
		return time.Time{}, fmt.Errorf("timestamp %q must not be zero", s)
	}
	if _, offset := t.Zone(); offset != 0 {
		return time.Time{}, fmt.Errorf("timestamp %q carries zone offset %d; only UTC is durable", s, offset)
	}
	return t, nil
}

// validateHexID enforces the durable identity shape: exactly 32 lowercase
// hexadecimal characters.
func validateHexID(s, what string) error {
	if len(s) != 32 {
		return fmt.Errorf("%s %q is not 32 hexadecimal characters", what, s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%s %q is not lowercase hexadecimal", what, s)
		}
	}
	return nil
}

// validateOperationIdentity enforces the durable Operation identity shape: a
// non-empty opaque caller identity.
func validateOperationIdentity(s, what string) error {
	if s == "" {
		return fmt.Errorf("%s must be non-empty", what)
	}
	return nil
}

// validExtraValues reports whether every Extra value is complete valid JSON;
// empty values are absent data, never stored content.
func validExtraValues(e model.Extra) bool {
	for _, v := range e {
		if !json.Valid(v) {
			return false
		}
	}
	return true
}

// encodeModelRef renders the durable two-field model identity object
// {"provider":...,"model":...}. The combined-string ModelRef JSON form is
// never durable: it is lossy on first-slash splits.
func encodeModelRef(r model.ModelRef) (json.RawMessage, error) {
	if r.Provider == "" || r.Model == "" {
		return nil, invalidInput("durable model reference %q must be complete (non-empty provider and model)", r.String())
	}
	wire, err := json.Marshal(struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}{Provider: r.Provider, Model: r.Model})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// decodeModelRef reads one durable model identity object with exact keys and
// non-empty fields; zero, partial, and combined-string references are all
// rejected.
func decodeModelRef(obj map[string]json.RawMessage) (model.ModelRef, error) {
	if err := rejectUnknownMembers(obj, "provider", "model"); err != nil {
		return model.ModelRef{}, err
	}
	provider, err := stringMember(obj, "provider", true)
	if err != nil {
		return model.ModelRef{}, err
	}
	name, err := stringMember(obj, "model", true)
	if err != nil {
		return model.ModelRef{}, err
	}
	if provider == "" || name == "" {
		return model.ModelRef{}, fmt.Errorf("durable model reference must be complete, got provider %q model %q", provider, name)
	}
	return model.ModelRef{Provider: provider, Model: name}, nil
}

// encodeContentPart renders one content part with exactly the kind-owned keys
// plus optional extra. Per-kind field ownership means a part carries only its
// own field: a text part never carries a URL or opaque wire type.
func encodeContentPart(p model.ContentPart) (json.RawMessage, error) {
	if _, err := model.NewContentPart(p); err != nil {
		return nil, invalidInput("content part: %v", err)
	}
	obj := map[string]json.RawMessage{}
	var err error
	if obj["kind"], err = json.Marshal(string(p.Kind)); err != nil {
		return nil, err
	}
	switch p.Kind {
	case model.PartText:
		if obj["text"], err = json.Marshal(p.Text); err != nil {
			return nil, err
		}
	case model.PartImageURL:
		if obj["url"], err = json.Marshal(p.URL); err != nil {
			return nil, err
		}
	case model.PartOpaque:
		if obj["opaque_wire_type"], err = json.Marshal(p.OpaqueWireType); err != nil {
			return nil, err
		}
	}
	if len(p.Extra) > 0 {
		raw, err := marshalExtra(p.Extra)
		if err != nil {
			return nil, err
		}
		obj["extra"] = raw
	}
	wire, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// decodeContentPart reads one content part with exact per-kind keys and
// reconstructs it through the landed model constructor, which re-enforces
// per-kind field ownership and returns an owned copy.
func decodeContentPart(raw json.RawMessage) (model.ContentPart, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return model.ContentPart{}, fmt.Errorf("content part: %w", err)
	}
	if err := rejectUnknownMembers(obj, "kind", "text", "url", "opaque_wire_type", "extra"); err != nil {
		return model.ContentPart{}, err
	}
	kind, err := stringMember(obj, "kind", true)
	if err != nil {
		return model.ContentPart{}, err
	}
	var part model.ContentPart
	part.Kind = model.PartKind(kind)
	forbidden := func(owned string, others ...string) error {
		for _, other := range others {
			if _, present := obj[other]; present {
				return fmt.Errorf("%s content part must not carry %q", owned, other)
			}
		}
		return nil
	}
	switch part.Kind {
	case model.PartText:
		if err := forbidden("text", "url", "opaque_wire_type"); err != nil {
			return model.ContentPart{}, err
		}
		if part.Text, err = stringMember(obj, "text", true); err != nil {
			return model.ContentPart{}, err
		}
	case model.PartImageURL:
		if err := forbidden("image_url", "text", "opaque_wire_type"); err != nil {
			return model.ContentPart{}, err
		}
		if part.URL, err = stringMember(obj, "url", true); err != nil {
			return model.ContentPart{}, err
		}
	case model.PartOpaque:
		if err := forbidden("opaque", "text", "url"); err != nil {
			return model.ContentPart{}, err
		}
		if part.OpaqueWireType, err = stringMember(obj, "opaque_wire_type", true); err != nil {
			return model.ContentPart{}, err
		}
	default:
		return model.ContentPart{}, fmt.Errorf("content part kind %q is not one of text, image_url or opaque", kind)
	}
	if part.Extra, err = extraMember(obj, "extra", false); err != nil {
		return model.ContentPart{}, err
	}
	return model.NewContentPart(part)
}

// marshalExtra renders one Extra as a JSON object after checking every value
// is complete valid JSON, so the raw value bytes pass through verbatim.
func marshalExtra(e model.Extra) (json.RawMessage, error) {
	if !validExtraValues(e) {
		return nil, invalidInput("extra values must be complete valid JSON")
	}
	wire, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// encodeToolDefinition renders one tool definition with exact keys name,
// description, and object-valued parameters.
func encodeToolDefinition(d model.ToolDefinition) (json.RawMessage, error) {
	if _, err := model.NewToolDefinition(d); err != nil {
		return nil, invalidInput("tool definition: %v", err)
	}
	obj := map[string]json.RawMessage{}
	var err error
	if obj["name"], err = json.Marshal(d.Name); err != nil {
		return nil, err
	}
	if obj["description"], err = json.Marshal(d.Description); err != nil {
		return nil, err
	}
	obj["parameters"] = model.CloneRaw(d.Parameters)
	return json.Marshal(obj)
}

// decodeToolDefinition reads one tool definition, preserves its parameter
// bytes verbatim, and re-validates it through the landed constructor.
func decodeToolDefinition(raw json.RawMessage) (model.ToolDefinition, error) {
	obj, err := decodePayloadObject(raw)
	if err != nil {
		return model.ToolDefinition{}, fmt.Errorf("tool definition: %w", err)
	}
	if err := rejectUnknownMembers(obj, "name", "description", "parameters"); err != nil {
		return model.ToolDefinition{}, err
	}
	var d model.ToolDefinition
	if d.Name, err = stringMember(obj, "name", true); err != nil {
		return model.ToolDefinition{}, err
	}
	if d.Description, err = stringMember(obj, "description", true); err != nil {
		return model.ToolDefinition{}, err
	}
	params, err := member(obj, "parameters", true)
	if err != nil {
		return model.ToolDefinition{}, err
	}
	if _, err := decodePayloadObject(params); err != nil {
		return model.ToolDefinition{}, fmt.Errorf("member %q: %w", "parameters", err)
	}
	d.Parameters = model.CloneRaw(params)
	if _, err := model.NewToolDefinition(d); err != nil {
		return model.ToolDefinition{}, err
	}
	return d, nil
}

// validOutputStatus reports whether the status is one of the landed closed
// output statuses.
func validOutputStatus(s model.OutputStatus) bool {
	switch s {
	case model.OutputCompleted, model.OutputErrored, model.OutputInterrupted:
		return true
	default:
		return false
	}
}
