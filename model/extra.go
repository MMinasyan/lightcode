package model

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Extra stores provider-specific JSON fields as raw wire values. Every map and
// byte slice in an Extra is owned by the holder: accepting boundaries deep-copy it.
type Extra map[string]json.RawMessage

// Clone returns a deep copy of the extra, or nil for an empty one.
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

// Finalize returns a copy without JSON null entries: finalized extras never carry nulls. An all-null extra finalizes to nil.
func (e Extra) Finalize() Extra {
	if len(e) == 0 {
		return nil
	}
	out := make(Extra, len(e))
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

// CloneRaw returns a copy of one raw JSON value, or an empty value for nil.
func CloneRaw(raw json.RawMessage) json.RawMessage {
	return cloneRaw(raw)
}

// validateExtraValues rejects Extra values whose raw bytes are not complete, syntactically valid JSON — including empty byte slices, which are absent data rather than a value and must never reach stored state where finalization could count them as payload. Every accepting boundary that takes an Extra map (content parts, messages and their tool calls, stream-delta fragments and message extras) runs this check before cloning; the null literal is complete JSON and passes construction (finalization drops it later). Tool-call argument bytes remain the only exception to complete-JSON requirements: they are caller-owned raw input retained verbatim by NewToolCall.
func validateExtraValues(e Extra) error {
	for key, value := range e {
		if json.Valid(value) {
			continue
		}
		return fmt.Errorf("extra field %q is not valid JSON", key)
	}
	return nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}

// ExtraAccumulator accumulates streamed extra values per key by first observed JSON kind. It retains the five kinds null, string, array, object, and other (numbers and booleans): strings concatenate, arrays append, objects and other values replace with the latest same-kind value. A key changing among those kinds stores the latest value and returns an accumulator error.
type ExtraAccumulator struct {
	values Extra
	kinds  map[string]extraKind
}

// NewExtraAccumulator returns an empty accumulator.
func NewExtraAccumulator() *ExtraAccumulator {
	return &ExtraAccumulator{}
}

// Add merges one raw JSON value into key's accumulated value, deep-copying the input bytes on retention. Empty keys and values are ignored. An error is returned only for a kind change or an unmergeable same-kind pair; in both cases the latest value is kept and accumulation continues with it as the new baseline.
func (a *ExtraAccumulator) Add(key string, value json.RawMessage) error {
	if key == "" || len(value) == 0 {
		return nil
	}
	kind := classifyJSON(value)
	if a.values == nil {
		a.values = Extra{}
	}
	if a.kinds == nil {
		a.kinds = map[string]extraKind{}
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

// Finalize returns the accumulated values with JSON null entries removed. A nil accumulator finalizes to nil.
func (a *ExtraAccumulator) Finalize() Extra {
	if a == nil {
		return nil
	}
	return a.values.Finalize()
}

type extraKind string

const (
	extraKindNull   extraKind = "null"
	extraKindString extraKind = "string"
	extraKindArray  extraKind = "array"
	extraKindObject extraKind = "object"
	extraKindOther  extraKind = "other" // numbers and booleans.
)

func classifyJSON(raw json.RawMessage) extraKind {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return extraKindOther
	}
	switch trimmed[0] {
	case 'n':
		if bytes.Equal(trimmed, []byte("null")) {
			return extraKindNull
		}
	case '"':
		return extraKindString
	case '[':
		return extraKindArray
	case '{':
		return extraKindObject
	}
	return extraKindOther
}

func accumulateSameKind(kind extraKind, previous, next json.RawMessage) (json.RawMessage, error) {
	switch kind {
	case extraKindString:
		var prevString, nextString string
		if err := json.Unmarshal(previous, &prevString); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(next, &nextString); err != nil {
			return nil, err
		}
		return json.Marshal(prevString + nextString)
	case extraKindArray:
		var prevItems, nextItems []json.RawMessage
		if err := json.Unmarshal(previous, &prevItems); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(next, &nextItems); err != nil {
			return nil, err
		}
		prevItems = append(prevItems, nextItems...)
		return json.Marshal(prevItems)
	default: // object and other replace with the latest same-kind value.
		return cloneRaw(next), nil
	}
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
