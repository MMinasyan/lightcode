package toolargs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestDecodeValidDomain proves the accepted side: one complete JSON object
// with every member retained, member-level null kept, and every numeric
// lexeme preserved exactly as a json.Number beyond float64.
func TestDecodeValidDomain(t *testing.T) {
	args, err := Decode(json.RawMessage(` {"a":"x","b":true,"c":null,"d":{"e":[1,2]}} `))
	testassert.NoError(t, err)
	testassert.Require(t, args["a"] == "x" && args["b"] == true && args["c"] == nil, "decoded = %#v, want the members preserved", args)
	if nested, ok := args["d"].(map[string]any); !ok {
		t.Fatalf("decoded = %#v, want the nested object preserved", args)
	} else if list, ok := nested["e"].([]any); !ok || len(list) != 2 {
		t.Fatalf("decoded = %#v, want the nested array preserved", args)
	}
	{
		empty, err := Decode(json.RawMessage(`{}`))
		testassert.Require(t, err == nil && len(empty) == 0 && empty != nil, "Decode({}) = %#v, %v, want an empty owned map", empty, err)
	}
	nums, err := Decode(json.RawMessage(`{"big":123456789012345678901234567890,"exp":1e400,"neg":-0.0}`))
	testassert.NoError(t, err)
	for key, want := range map[string]string{"big": "123456789012345678901234567890", "exp": "1e400", "neg": "-0.0"} {
		got, ok := nums[key].(json.Number)
		testassert.Check(t, ok && got.String() == want, "decoded %s = %#v, want the exact lexeme %q", key, nums[key], want)
	}
}

// TestDecodeInvalidDomain proves the refused side uniformly: empty, null,
// array, primitive, malformed and trailing documents are all argument
// validation errors under the retained diagnostics.
func TestDecodeInvalidDomain(t *testing.T) {
	exact := map[string]string{
		`null`:             "arguments must be a JSON object",
		`{"a":1}{"b":2}`:   "arguments must be one JSON object",
		`{"a":1} trailing`: "arguments must be one JSON object",
		`{} {}`:            "arguments must be one JSON object",
		`{"a":1}null`:      "arguments must be one JSON object",
		"{}\n{}":           "arguments must be one JSON object",
	}
	prefix := map[string]string{
		``:         "arguments must be a JSON object",
		`not json`: "arguments must be a JSON object",
		`[1]`:      "arguments must be a JSON object",
		`"str"`:    "arguments must be a JSON object",
		`123`:      "arguments must be a JSON object",
		`true`:     "arguments must be a JSON object",
		`{`:        "arguments must be a JSON object",
		`{"a":}`:   "arguments must be a JSON object",
		`{"a":1,}`: "arguments must be a JSON object",
	}
	for raw, want := range exact {
		_, err := Decode(json.RawMessage(raw))
		testassert.Check(t, err != nil && err.Error() == want, "Decode(%q) error = %v, want exactly %q", raw, err, want)
	}
	for raw, want := range prefix {
		_, err := Decode(json.RawMessage(raw))
		testassert.Check(t, err != nil && strings.HasPrefix(err.Error(), want), "Decode(%q) error = %v, want prefix %q", raw, err, want)
	}
}

// TestDecodeRetainsNoCallerBytes proves the owned-map boundary: mutating the
// caller's raw slice after the decode leaves every returned value unchanged.
func TestDecodeRetainsNoCallerBytes(t *testing.T) {
	raw := []byte(`{"s":"keep","n":123}`)
	args, err := Decode(raw)
	testassert.NoError(t, err)
	for i := range raw {
		raw[i] = 'x'
	}
	testassert.Require(t, args["s"] == "keep" && args["n"].(json.Number).String() == "123", "decoded = %#v, want values independent of the mutated caller bytes", args)
}
