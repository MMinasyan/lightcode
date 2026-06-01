package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Extra stores provider-specific JSON fields as raw wire values.
type Extra map[string]json.RawMessage

// Clone returns a deep copy of Extra.
func (e Extra) Clone() Extra {
	if len(e) == 0 {
		return nil
	}
	out := make(Extra, len(e))
	for key, value := range e {
		out[key] = cloneRaw(value)
	}
	return out
}

// WithoutNulls returns a copy without JSON null values.
func (e Extra) WithoutNulls() Extra {
	if len(e) == 0 {
		return nil
	}
	out := Extra{}
	for key, value := range e {
		if isJSONNull(value) {
			continue
		}
		out[key] = cloneRaw(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ExtraAccumulator accumulates streamed Extra values by first observed JSON kind.
type ExtraAccumulator struct {
	values Extra
	kinds  map[string]jsonKind
}

// NewExtraAccumulator returns an empty accumulator.
func NewExtraAccumulator() *ExtraAccumulator {
	return &ExtraAccumulator{}
}

// Add merges one raw JSON value into key's accumulated value.
func (a *ExtraAccumulator) Add(key string, value json.RawMessage) error {
	if key == "" || len(value) == 0 {
		return nil
	}
	kind := classifyJSON(value)
	if a.values == nil {
		a.values = Extra{}
	}
	if a.kinds == nil {
		a.kinds = map[string]jsonKind{}
	}
	prevKind, exists := a.kinds[key]
	if !exists {
		a.kinds[key] = kind
		a.values[key] = cloneRaw(value)
		return nil
	}
	if prevKind != kind {
		a.kinds[key] = kind
		a.values[key] = cloneRaw(value)
		return fmt.Errorf("extra field %q changed JSON kind from %s to %s", key, prevKind, kind)
	}
	merged, err := accumulateSameKind(prevKind, a.values[key], value)
	if err != nil {
		a.values[key] = cloneRaw(value)
		return err
	}
	a.values[key] = merged
	return nil
}

// Extra returns the accumulated values with JSON null entries removed.
func (a *ExtraAccumulator) Extra() Extra {
	if a == nil {
		return nil
	}
	return a.values.WithoutNulls()
}

type jsonKind string

const (
	jsonKindNull   jsonKind = "null"
	jsonKindString jsonKind = "string"
	jsonKindArray  jsonKind = "array"
	jsonKindObject jsonKind = "object"
	jsonKindOther  jsonKind = "other"
)

func classifyJSON(raw json.RawMessage) jsonKind {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return jsonKindOther
	}
	switch trimmed[0] {
	case 'n':
		if bytes.Equal(trimmed, []byte("null")) {
			return jsonKindNull
		}
	case '"':
		return jsonKindString
	case '[':
		return jsonKindArray
	case '{':
		return jsonKindObject
	}
	return jsonKindOther
}

func accumulateSameKind(kind jsonKind, previous, next json.RawMessage) (json.RawMessage, error) {
	switch kind {
	case jsonKindString:
		var prevString, nextString string
		if err := json.Unmarshal(previous, &prevString); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(next, &nextString); err != nil {
			return nil, err
		}
		return json.Marshal(prevString + nextString)
	case jsonKindArray:
		var prevItems, nextItems []json.RawMessage
		if err := json.Unmarshal(previous, &prevItems); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(next, &nextItems); err != nil {
			return nil, err
		}
		prevItems = append(prevItems, nextItems...)
		return json.Marshal(prevItems)
	default:
		return cloneRaw(next), nil
	}
}

// CloneRaw returns a copy of a raw JSON value.
func CloneRaw(raw json.RawMessage) json.RawMessage {
	return cloneRaw(raw)
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func isLightcodeField(key string) bool {
	return strings.HasPrefix(key, "_lightcode_")
}
