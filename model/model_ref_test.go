package model

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestParseSplitsOnFirstSlash(t *testing.T) {
	ref, err := Parse("openrouter/openai/gpt-5.4-mini")
	testassert.NoError(t, err)
	testassert.Require(t, ref.Provider == "openrouter" && ref.Model == "openai/gpt-5.4-mini", "unexpected ref: %#v", ref)
	{
		got := ref.String()
		testassert.Equal(t, got, "openrouter/openai/gpt-5.4-mini")
	}
}

func TestModelRefStringIsParseableWhenNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		ref  ModelRef
		want string
	}{
		{name: "zero", ref: ModelRef{}, want: ""},
		{name: "provider only", ref: ModelRef{Provider: "openai"}, want: ""},
		{name: "model only", ref: ModelRef{Model: "gpt-5.4-mini"}, want: ""},
		{name: "model with slash", ref: ModelRef{Provider: "openrouter", Model: "openai/gpt-5.4-mini"}, want: "openrouter/openai/gpt-5.4-mini"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ref.String()
			testassert.Equal(t, got, tc.want)
			if got == "" {
				return
			}
			parsed, err := Parse(got)
			testassert.Require(t, err == nil, "Parse(%q) returned error: %v", got, err)
			testassert.Equal(t, parsed, tc.ref)
		})
	}
}

func TestModelRefJSONUsesPrefixString(t *testing.T) {
	ref := ModelRef{Provider: "openai", Model: "gpt-5.4-mini"}
	b, err := json.Marshal(ref)
	testassert.NoError(t, err)
	testassert.Require(t, string(b) == `"openai/gpt-5.4-mini"`, "Marshal = %s", b)

	var parsed ModelRef
	{
		err := json.Unmarshal(b, &parsed)
		testassert.NoError(t, err)
	}
	testassert.Equal(t, parsed, ref)
}

func TestModelRefJSONZeroMarshalsAsEmptyString(t *testing.T) {
	for _, name := range []string{"zero", "partial provider"} {
		var ref ModelRef
		if name == "partial provider" {
			ref = ModelRef{Provider: "openai"}
		}
		b, err := json.Marshal(ref)
		testassert.Require(t, err == nil, "%s Marshal returned error: %v", name, err)
		testassert.Require(t, string(b) == `""`, "%s Marshal = %s, want \"\"", name, b)
	}
}

func TestModelRefJSONUnmarshalRejectsInvalidStrings(t *testing.T) {
	for _, input := range []string{`null`, `""`, `"openai"`, `"/gpt-5.4-mini"`, `"openai/"`} {
		var ref ModelRef
		err := json.Unmarshal([]byte(input), &ref)
		testassert.Require(t, errors.Is(err, ErrInvalidModelRef), "Unmarshal(%s) error = %v, want ErrInvalidModelRef", input, err)
	}
}

func TestParseRejectsMissingParts(t *testing.T) {
	for _, input := range []string{"", "openai", "/gpt-5.4-mini", "openai/"} {
		{
			_, err := Parse(input)
			testassert.Require(t, errors.Is(err, ErrInvalidModelRef), "Parse(%q) error = %v, want ErrInvalidModelRef", input, err)
		}
	}
}

func TestModelRefIsZero(t *testing.T) {
	testassert.Require(t, (ModelRef{}).IsZero(), "%v", "zero ref did not report IsZero")
	testassert.Require(t, !(ModelRef{Provider: "openai"}).IsZero(), "%v", "partial ref reported IsZero")
	testassert.Require(t, !(ModelRef{Provider: "openai", Model: "gpt-5.4-mini"}).IsZero(), "%v", "complete ref reported IsZero")
}
