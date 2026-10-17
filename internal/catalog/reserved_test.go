package catalog

import (
	"encoding/json"
	"errors"
	"testing"

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
		if !protocolFieldReserved(name) {
			t.Errorf("protocolFieldReserved(%q) = false, want the shared reserved key rejected", name)
		}
	}
	for _, name := range []string{"temperature", "reasoning_effort"} {
		if protocolFieldReserved(name) {
			t.Errorf("protocolFieldReserved(%q) = true, want a clean unknown field accepted", name)
		}
	}
}

func TestCheckReservedKeysReturnsEmptyForCleanBody(t *testing.T) {
	got := CheckReservedKeys(map[string]any{
		"temperature":      0.2,
		"reasoning_effort": "high",
	})
	if len(got) != 0 {
		t.Fatalf("CheckReservedKeys returned %#v, want empty", got)
	}
}

func TestCheckReservedKeysReturnsReservedKeysInStableOrder(t *testing.T) {
	got := CheckReservedKeys(map[string]any{
		"n":              2,
		"model":          "bad",
		"stream_options": map[string]any{"include_usage": true},
		"temperature":    0.2,
	})
	want := []string{"model", "stream_options", "n"}
	if len(got) != len(want) {
		t.Fatalf("reserved keys = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reserved keys = %#v, want %#v", got, want)
		}
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
	if !hasValidationError(errs, "extra_body.model") {
		t.Fatalf("ValidateRaw errors = %#v, want the reserved extra_body.model diagnostic for any value shape", errs)
	}
	if hasValidationError(errs, "extra_body.temperature") {
		t.Fatalf("ValidateRaw errors = %#v, want the clean sibling value to stay unrestricted", errs)
	}
	if !hasValidationError(errs, "transport.base_url") {
		t.Fatalf("ValidateRaw errors = %#v, want the unrelated violation beside the reserved-key diagnostic", errs)
	}

	rt := model.ResolvedTransport{
		Model:             model.ModelRef{Provider: "openai", Model: "gpt-test"},
		ProviderExtraBody: model.Extra{"model": json.RawMessage(`"forged"`)},
	}
	body, _, err := model.Encode(rt, model.Request{}, nil)
	var reserved *model.ReservedKeyError
	if !errors.As(err, &reserved) || !errors.Is(err, model.ErrReservedKeys) {
		t.Fatalf("Encode error = %v, want the typed reserved-key rejection", err)
	}
	if len(reserved.Keys) != 1 || reserved.Keys[0] != "model" {
		t.Fatalf("rejected keys = %#v, want [model]", reserved.Keys)
	}
	if len(body) != 0 {
		t.Fatalf("rejection produced a partial body: %s", string(body))
	}
}
