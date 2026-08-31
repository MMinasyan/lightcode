package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEncodeStreamOptionsPinnedByFlagOnly pins rule 3: stream_options appears exactly when the resolved flag is true, carrying precisely {"include_usage":true} and nothing else. Zero-value and explicit-false flags both omit it entirely; message count never changes its shape or occurrence. Forged sidecar copies of the key are reserved-key rejections (covered by TestReservedKeys*), not a way to turn this on.
func TestEncodeStreamOptionsPinnedByFlagOnly(t *testing.T) {
	req := Request{Messages: []Message{userText("hi")}} // no usage flag set in the baseline resolved input.

	bodyOff, _, err := Encode(testResolved(), req, nil) // false -> absent from the body entirely.
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	if strings.Contains(string(bodyOff), `"stream_options"`) {
		t.Fatal(`flag false must not emit stream_options: ` + string(bodyOff))
	}

	rtOn := testResolved() // the resolved flag is what turns it on.
	rtOn.StreamedUsage = true
	bodyOn, _, err := Encode(rtOn, req, nil)
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	var opts map[string]json.RawMessage // exactly include_usage:true and nothing else may appear inside.
	rawOpts := bodyField(t, bodyOn, "stream_options")
	if err := json.Unmarshal(rawOpts, &opts); err != nil || len(opts) != 1 {
		t.Fatalf("stream_options wrong: %s", rawOpts)
	}
	if rawInclude, ok := opts["include_usage"]; !ok || strings.TrimSpace(string(rawInclude)) != "true" {
		t.Fatalf(`stream_options = %q want {"include_usage":true}`, string(rawOpts))
	}

	bodyZero := mustEncode(t, testResolved(), Request{}) // zero-value flag with an empty request still omits the key.
	if strings.Contains(string(bodyZero), `"stream_options"`) {
		t.Fatal(`zero-value flag emitted stream_options: ` + string(bodyZero))
	}

	rtManyOn := testResolved() // flag true across multiple messages emits exactly one occurrence of the same object.
	rtManyOn.StreamedUsage = true
	bodyMulti, _, err := Encode(rtManyOn, Request{Messages: []Message{userText("a"), userText("b")}}, nil)
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	assertStreamOptionsOnce(t, bodyMulti)

	rtEmptyOn := testResolved() // flag true with zero messages emits the same single object too.
	rtEmptyOn.StreamedUsage = true
	bodyEmpty, _, err := Encode(rtEmptyOn, Request{}, nil)
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	assertStreamOptionsOnce(t, bodyEmpty)

	objOffKeys := decodeObject(t, bodyOff) // base keys stay present regardless of the flag (regression guard against accidental overwrites).
	for _, key := range []string{"model", "messages", "stream", "n"} {
		if _, ok := objOffKeys[key]; !ok {
			t.Fatalf("flag-off encode lost a base key %q: %s", key, string(bodyOff))
		}
	}
}

// assertStreamOptionsOnce decodes the body and requires exactly one stream_options object equal to {"include_usage":true}.
func assertStreamOptionsOnce(t *testing.T, body json.RawMessage) {
	t.Helper() // occurrence count plus exact shape in one check.
	if got := strings.Count(string(body), `"stream_options"`); got != 1 {
		t.Fatalf(`body carries %d stream_options keys (want exactly 1): %s`, got, string(body))
	}
	var opts map[string]json.RawMessage // the single occurrence must be precisely include_usage:true.
	rawOpts := bodyField(t, body, "stream_options")
	if err := json.Unmarshal(rawOpts, &opts); err != nil || len(opts) != 1 {
		t.Fatalf("stream_options wrong: %s", rawOpts)
	}
	if rawInclude, ok := opts["include_usage"]; !ok || strings.TrimSpace(string(rawInclude)) != "true" {
		t.Fatalf(`stream_options = %q want {"include_usage":true}`, string(rawOpts))
	}
}
