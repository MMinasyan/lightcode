// Package model defines provider-neutral values for Lightcode's agent
// foundation: model identities, conversation roles and messages, content
// parts, tool definitions, calls and results, logical requests, usage counts,
// protocol warnings, stream deltas, and final outputs.
//
// Values are plain Go data with closed enums validated at their constructors;
// every accepting constructor returns an independent deep copy of its input.
// Messages have no general JSON wire or persistence contract: provider wire
// encoding is produced by the OpenAI-compatible encoder in this package's
// transport layer, not by these values.
package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidModelRef is returned when a provider-prefixed model reference is malformed or incomplete where a complete identity is required for parsing.
var ErrInvalidModelRef = errors.New("invalid model ref")

// ModelRef is the canonical internal identity of a model: a provider and a
// model name, rendered as "provider/model".
type ModelRef struct {
	Provider string
	Model    string
}

// String returns the provider-prefixed model reference. A zero or partial
// reference renders as an empty string.
func (r ModelRef) String() string {
	if r.Provider == "" || r.Model == "" {
		return ""
	}
	return r.Provider + "/" + r.Model
}

// IsZero reports whether the model reference has no provider and no model. A
// partially populated identity is not zero but is still incomplete: use String
// to test renderability, or keep a complete ref wherever one is required.
func (r ModelRef) IsZero() bool {
	return r.Provider == "" && r.Model == ""
}

// complete reports whether both identity parts are present. A partially populated ref is not zero but still incomplete and invalid wherever a nonzero identity is required.
func (r ModelRef) complete() bool { return r.Provider != "" && r.Model != "" }

// MarshalJSON serializes the reference as its provider-prefixed string form; a
// zero or partial identity marshals to the JSON string "".
func (r ModelRef) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

// UnmarshalJSON parses a provider-prefixed model reference from JSON. It always
// uses normal parsing: "", null, missing-part and empty identities are invalid.
func (r *ModelRef) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	ref, err := Parse(s)
	if err != nil {
		return err
	}
	*r = ref
	return nil
}

// Parse parses a provider-prefixed model reference. The string is split at the
// first "/"; both parts must be non-empty.
func Parse(s string) (ModelRef, error) {
	provider, model, ok := strings.Cut(s, "/")
	if !ok || provider == "" || model == "" {
		return ModelRef{}, fmt.Errorf("%w: %q", ErrInvalidModelRef, s)
	}
	return ModelRef{Provider: provider, Model: model}, nil
}
