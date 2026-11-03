// Package toolargs is the shared strict decoder for plugin tool-call
// arguments. It owns only the JSON-object data domain: exactly one complete
// document that must be a non-null object, decoded with UseNumber so every
// numeric lexeme survives beyond float64. It carries no settings, registry,
// normalization policy, error/outcome formatting or permission knowledge.
package toolargs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Decode strictly decodes one call's JSON arguments into an owned map:
// UseNumber keeps every numeric lexeme exact, the decode clones the call
// data at the accepting boundary, and malformed, non-object, null or
// trailing data are argument-validation errors.
func Decode(raw json.RawMessage) (map[string]any, error) {
	var args map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	if args == nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("arguments must be one JSON object")
	}
	return args, nil
}
