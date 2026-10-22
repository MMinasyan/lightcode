package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestEncodeStreamOptionsPinnedByFlagOnly pins rule 3: stream_options appears exactly when the resolved flag is true, carrying precisely {"include_usage":true} and nothing else. Zero-value and explicit-false flags both omit it entirely; message count never changes its shape or occurrence. Forged sidecar copies of the key are reserved-key rejections (covered by TestReservedKeys*), not a way to turn this on.
func TestEncodeStreamOptionsPinnedByFlagOnly(t *testing.T) {
	req := Request{Messages: []Message{userText("hi")}} // no usage flag set in the baseline resolved input.

	bodyOff, _, err := Encode(testResolved(), req, nil) // false -> absent from the body entirely.
	testassert.NoError(t, err)
	testassert.Require(t, !strings.Contains(string(bodyOff), `"stream_options"`), "%v", `flag false must not emit stream_options: `+string(bodyOff))

	rtOn := testResolved() // the resolved flag is what turns it on.
	rtOn.StreamedUsage = true
	bodyOn, _, err := Encode(rtOn, req, nil)
	testassert.NoError(t, err)
	var opts map[string]json.RawMessage // exactly include_usage:true and nothing else may appear inside.
	rawOpts := bodyField(t, bodyOn, "stream_options")
	{
		err := json.Unmarshal(rawOpts, &opts)
		testassert.Require(t, err == nil && len(opts) == 1, "stream_options wrong: %s", rawOpts)
	}
	{
		rawInclude, ok := opts["include_usage"]
		testassert.Require(t, ok && strings.TrimSpace(string(rawInclude)) == "true", `stream_options = %q want {"include_usage":true}`, string(rawOpts))
	}

	bodyZero := mustEncode(t, testResolved(), Request{}) // zero-value flag with an empty request still omits the key.
	testassert.Require(t, !strings.Contains(string(bodyZero), `"stream_options"`), "%v", `zero-value flag emitted stream_options: `+string(bodyZero))

	rtManyOn := testResolved() // flag true across multiple messages emits exactly one occurrence of the same object.
	rtManyOn.StreamedUsage = true
	bodyMulti, _, err := Encode(rtManyOn, Request{Messages: []Message{userText("a"), userText("b")}}, nil)
	testassert.NoError(t, err)
	assertStreamOptionsOnce(t, bodyMulti)

	rtEmptyOn := testResolved() // flag true with zero messages emits the same single object too.
	rtEmptyOn.StreamedUsage = true
	bodyEmpty, _, err := Encode(rtEmptyOn, Request{}, nil)
	testassert.NoError(t, err)
	assertStreamOptionsOnce(t, bodyEmpty)

	objOffKeys := decodeObject(t, bodyOff) // base keys stay present regardless of the flag (regression guard against accidental overwrites).
	for _, key := range []string{"model", "messages", "stream", "n"} {
		{
			_, ok := objOffKeys[key]
			testassert.Require(t, ok, "flag-off encode lost a base key %q: %s", key, string(bodyOff))
		}
	}
}

// assertStreamOptionsOnce decodes the body and requires exactly one stream_options object equal to {"include_usage":true}.
func assertStreamOptionsOnce(t *testing.T, body json.RawMessage) {
	t.Helper() // occurrence count plus exact shape in one check.
	{
		got := strings.Count(string(body), `"stream_options"`)
		testassert.Require(t, got == 1, `body carries %d stream_options keys (want exactly 1): %s`, got, string(body))
	}
	var opts map[string]json.RawMessage // the single occurrence must be precisely include_usage:true.
	rawOpts := bodyField(t, body, "stream_options")
	{
		err := json.Unmarshal(rawOpts, &opts)
		testassert.Require(t, err == nil && len(opts) == 1, "stream_options wrong: %s", rawOpts)
	}
	{
		rawInclude, ok := opts["include_usage"]
		testassert.Require(t, ok && strings.TrimSpace(string(rawInclude)) == "true", `stream_options = %q want {"include_usage":true}`, string(rawOpts))
	}
}
