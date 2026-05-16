package catalog

import (
	"encoding/json"
	"testing"
)

func TestParseModelRefSplitsOnFirstSlash(t *testing.T) {
	ref, err := ParseModelRef("openrouter/openai/gpt-5.4-mini")
	if err != nil {
		t.Fatalf("ParseModelRef returned error: %v", err)
	}
	if ref.Provider != "openrouter" || ref.Model != "openai/gpt-5.4-mini" {
		t.Fatalf("unexpected ref: %#v", ref)
	}
	if got := ref.String(); got != "openrouter/openai/gpt-5.4-mini" {
		t.Fatalf("String() = %q", got)
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

func TestParseModelRefRejectsMissingParts(t *testing.T) {
	for _, input := range []string{"", "openai", "/gpt-5.4-mini", "openai/"} {
		if _, err := ParseModelRef(input); err == nil {
			t.Fatalf("ParseModelRef(%q) succeeded, want error", input)
		}
	}
}
