package model

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParseSplitsOnFirstSlash(t *testing.T) {
	ref, err := Parse("openrouter/openai/gpt-5.4-mini")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if ref.Provider != "openrouter" || ref.Model != "openai/gpt-5.4-mini" {
		t.Fatalf("unexpected ref: %#v", ref)
	}
	if got := ref.String(); got != "openrouter/openai/gpt-5.4-mini" {
		t.Fatalf("String() = %q", got)
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
			if got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
			if got == "" {
				return
			}
			parsed, err := Parse(got)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", got, err)
			}
			if parsed != tc.ref {
				t.Fatalf("parsed = %#v, want %#v", parsed, tc.ref)
			}
		})
	}
}

func TestModelRefJSONUsesPrefixString(t *testing.T) {
	ref := ModelRef{Provider: "openai", Model: "gpt-5.4-mini"}
	b, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if string(b) != `"openai/gpt-5.4-mini"` {
		t.Fatalf("Marshal = %s", b)
	}

	var parsed ModelRef
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if parsed != ref {
		t.Fatalf("parsed = %#v, want %#v", parsed, ref)
	}
}

func TestModelRefJSONZeroMarshalsAsEmptyString(t *testing.T) {
	for _, name := range []string{"zero", "partial provider"} {
		var ref ModelRef
		if name == "partial provider" {
			ref = ModelRef{Provider: "openai"}
		}
		b, err := json.Marshal(ref)
		if err != nil {
			t.Fatalf("%s Marshal returned error: %v", name, err)
		}
		if string(b) != `""` {
			t.Fatalf("%s Marshal = %s, want \"\"", name, b)
		}
	}
}

func TestModelRefJSONUnmarshalRejectsInvalidStrings(t *testing.T) {
	for _, input := range []string{`null`, `""`, `"openai"`, `"/gpt-5.4-mini"`, `"openai/"`} {
		var ref ModelRef
		err := json.Unmarshal([]byte(input), &ref)
		if !errors.Is(err, ErrInvalidModelRef) {
			t.Fatalf("Unmarshal(%s) error = %v, want ErrInvalidModelRef", input, err)
		}
	}
}

func TestParseRejectsMissingParts(t *testing.T) {
	for _, input := range []string{"", "openai", "/gpt-5.4-mini", "openai/"} {
		if _, err := Parse(input); !errors.Is(err, ErrInvalidModelRef) {
			t.Fatalf("Parse(%q) error = %v, want ErrInvalidModelRef", input, err)
		}
	}
}

func TestModelRefIsZero(t *testing.T) {
	if !(ModelRef{}).IsZero() {
		t.Fatal("zero ref did not report IsZero")
	}
	if (ModelRef{Provider: "openai"}).IsZero() {
		t.Fatal("partial ref reported IsZero")
	}
	if (ModelRef{Provider: "openai", Model: "gpt-5.4-mini"}).IsZero() {
		t.Fatal("complete ref reported IsZero")
	}
}
