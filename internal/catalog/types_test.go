package catalog

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestParseModelRefSplitsOnFirstSlash(t *testing.T) {
	ref, err := ParseModelRef("openrouter/openai/gpt-5.4-mini")
	testassert.NoError(t, err)
	testassert.Require(t, ref.Provider == "openrouter" && ref.Model == "openai/gpt-5.4-mini", "unexpected ref: %#v", ref)
	testassert.Equal(t, ref.String(), "openrouter/openai/gpt-5.4-mini")
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
			parsed, err := ParseModelRef(got)
			testassert.Require(t, err == nil, "ParseModelRef(%q) returned error: %v", got, err)
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
	testassert.NoError(t, json.Unmarshal(b, &parsed))
	testassert.Equal(t, parsed, ref)
}

func TestParseModelRefRejectsMissingParts(t *testing.T) {
	for _, input := range []string{"", "openai", "/gpt-5.4-mini", "openai/"} {
		{
			_, err := ParseModelRef(input)
			testassert.Require(t, err != nil, "ParseModelRef(%q) succeeded, want error", input)
		}
	}
}
