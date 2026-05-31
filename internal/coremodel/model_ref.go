// Package coremodel contains model identity types that are independent of the
// provider catalog.
package coremodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidModelRef is returned when a provider-prefixed model reference is malformed.
var ErrInvalidModelRef = errors.New("catalog: invalid model ref")

// ModelRef is the canonical internal identity of a model.
type ModelRef struct {
	Provider string
	Model    string
}

// String returns the provider-prefixed model reference.
func (r ModelRef) String() string {
	if r.Provider == "" || r.Model == "" {
		return ""
	}
	return r.Provider + "/" + r.Model
}

// IsZero reports whether the model reference has no provider or model.
func (r ModelRef) IsZero() bool {
	return r.Provider == "" && r.Model == ""
}

// MarshalJSON serializes ModelRef as the provider-prefixed string form.
func (r ModelRef) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

// UnmarshalJSON parses a provider-prefixed model reference from JSON.
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

// Parse parses a provider-prefixed model reference.
func Parse(s string) (ModelRef, error) {
	provider, model, ok := strings.Cut(s, "/")
	if !ok || provider == "" || model == "" {
		return ModelRef{}, fmt.Errorf("%w: %q", ErrInvalidModelRef, s)
	}
	return ModelRef{Provider: provider, Model: model}, nil
}
