package catalog

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// TestReservedKeyValidationCoversEveryOwnerKey pins the consumer side of the
// shared reserved set against an independent literal list: every owned key is
// reserved for protocol field names, and clean unknown fields are not.
func TestReservedKeyValidationCoversEveryOwnerKey(t *testing.T) {
	want := []string{
		"model",
		"messages",
		"tools",
		"tool_choice",
		"stream",
		"stream_options",
		"max_tokens",
		"max_completion_tokens",
		"n",
	}
	for _, name := range want {
		testassert.Check(t, protocolFieldReserved(name), "protocolFieldReserved(%q) = false, want the shared reserved key rejected", name)
	}
	for _, name := range []string{"temperature", "reasoning_effort"} {
		testassert.Check(t, !protocolFieldReserved(name), "protocolFieldReserved(%q) = true, want a clean unknown field accepted", name)
	}
}

func TestCheckReservedKeysReturnsEmptyForCleanBody(t *testing.T) {
	got := CheckReservedKeys(map[string]any{
		"temperature":      0.2,
		"reasoning_effort": "high",
	})
	testassert.Require(t, len(got) == 0, "CheckReservedKeys returned %#v, want empty", got)
}

func TestCheckReservedKeysReturnsReservedKeysInStableOrder(t *testing.T) {
	got := CheckReservedKeys(map[string]any{
		"n":              2,
		"model":          "bad",
		"stream_options": map[string]any{"include_usage": true},
		"temperature":    0.2,
	})
	want := []string{"model", "stream_options", "n"}
	testassert.Require(t, len(got) == len(want), "reserved keys = %#v, want %#v", got, want)
	for i := range want {
		testassert.Require(t, got[i] == want[i], "reserved keys = %#v, want %#v", got, want)
	}
}

// TestReservedKeyPresenceIsDiagnosticAndRejection pins the two boundaries
// apart for the same reserved-key presence: configuration validation reports
// its own diagnostic by presence alone — whatever the value and beside an
// unrelated document violation — while transport acceptance rejects the same
// presence with its own typed error and no body bytes. Neither boundary's
// outcome substitutes for the other.
func TestReservedKeyPresenceIsDiagnosticAndRejection(t *testing.T) {
	raw := validRawProvider()
	raw["extra_body"] = map[string]any{"model": map[string]any{"nested": true}, "temperature": "hot"}
	delete(raw["transport"].(map[string]any), "base_url")

	errs := ValidateRaw("openai", raw, true)
	testassert.Require(t, hasValidationError(errs, "extra_body.model") && !hasValidationError(errs, "extra_body.temperature") && hasValidationError(errs, "transport.base_url"), "ValidateRaw errors = %#v, want the reserved extra_body.model diagnostic for any value shape; ValidateRaw errors = %#v, want the clean sibling value to stay unrestricted; ValidateRaw errors = %#v, want the unrelated violation beside the reserved-key diagnostic", errs, errs, errs)

	rt := model.ResolvedTransport{
		Model:             model.ModelRef{Provider: "openai", Model: "gpt-test"},
		ProviderExtraBody: model.Extra{"model": json.RawMessage(`"forged"`)},
	}
	body, _, err := model.Encode(rt, model.Request{}, nil)
	var reserved *model.ReservedKeyError
	testassert.Require(t, errors.As(err, &reserved) && errors.Is(err, model.ErrReservedKeys), "Encode error = %v, want the typed reserved-key rejection", err)
	testassert.Require(t, len(reserved.Keys) == 1 && reserved.Keys[0] == "model", "rejected keys = %#v, want [model]", reserved.Keys)
	if len(body) != 0 {
		t.Fatalf("rejection produced a partial body: %s", string(body))
	}
}
