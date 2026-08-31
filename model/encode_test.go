package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// testResolved is the valid resolved-transport baseline for encoder tests: target openai/gpt-test.
func testResolved() ResolvedTransport {
	return ResolvedTransport{Model: ModelRef{Provider: "openai", Model: "gpt-test"}}
}

var (
	otherSameProv = ModelRef{Provider: "openai", Model: "gpt-other"} // same provider, different model.
	foreignModel  = ModelRef{Provider: "anthropic", Model: "claude-x"}
)

// mustEncode encodes with nil runtime extras and fails the test on any error.
func mustEncode(t *testing.T, rt ResolvedTransport, req Request) json.RawMessage {
	t.Helper() // one logical request plus no per-call layer.
	body, _, err := Encode(rt, req, nil)
	if err != nil {
		t.Fatalf("Encode returned unexpected error: %v", err)
	}
	return body
}

// decodeObject parses one encoded JSON object body.
func decodeObject(t *testing.T, body json.RawMessage) map[string]json.RawMessage {
	t.Helper() // the top level is always a single object from this encoder.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("encoded body is not a JSON object: %v (body=%s)", err, string(body))
	}
	return obj
}

func bodyField(t *testing.T, body json.RawMessage, key string) []byte {
	t.Helper() // missing keys are test errors.
	obj := decodeObject(t, body)
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("body missing %q (body=%s)", key, string(body))
	}
	return raw
}

// decodeMessages parses the messages array into ordered raw objects.
func decodeMessages(t *testing.T, body json.RawMessage) []map[string]json.RawMessage {
	t.Helper() // order is preserved from the request message list.
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(bodyField(t, body, "messages"), &out); err != nil {
		t.Fatalf("body messages not an array of objects: %v", err)
	}
	return out
}

func wireString(t *testing.T, obj map[string]json.RawMessage, key string) (string, bool) {
	t.Helper() // absent keys return false without failing.
	raw, ok := obj[key]
	if !ok {
		return "", false
	}
	var s string // wrong JSON types are test errors here because the contract fixes these as strings.
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("wire value %q is not a JSON string: %s", key, raw)
	}
	return s, true
}

func mustMsg(t *testing.T, in Message) Message {
	t.Helper() // canonical fixtures go through the public constructor.
	out, err := NewMessage(in)
	if err != nil {
		t.Fatalf("NewMessage failed for fixture role %s: %v", in.Role, err)
	}
	return out
}

func mustCall(t *testing.T, id, name string, args json.RawMessage) ToolCall {
	t.Helper() // completed call fixtures go through the public constructor.
	out, err := NewToolCall(ToolCall{ID: id, Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("NewToolCall failed for %s/%s: %v", id, name, err)
	}
	return out
}

func userText(text string) Message { // the standard non-system message used across tests.
	m, _ := NewMessage(Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: text}}})
	return m
}

// TestEncodeBaseBody pins rule 1 byte-for-byte and the absence of every optional trigger without its data.
func TestEncodeBaseBody(t *testing.T) {
	req := Request{Messages: []Message{userText("hi")}} // one user message, no tools.

	body := mustEncode(t, testResolved(), req)
	want := `{"messages":[{"content":"hi","role":"user"}],"model":"gpt-test","n":1,"stream":true}` // exact wire shape including key order (map marshal).
	if string(body) != want {
		t.Fatalf("base body = %s\nwant      %s", string(body), want)
	}

	obj := decodeObject(t, body) // none of the optional top-level keys exist.
	for _, key := range []string{"tools", "tool_choice", "stream_options", "max_tokens", "max_completion_tokens"} {
		if _, ok := obj[key]; ok {
			t.Fatalf("base body must not carry %q: %s", key, string(body))
		}
	}

	bodyEmpty := mustEncode(t, testResolved(), Request{}) // zero messages and tools.
	objEmpty := decodeObject(t, bodyEmpty)
	if rawMessages, ok := objEmpty["messages"]; !ok || strings.TrimSpace(string(rawMessages)) != "[]" {
		t.Fatalf("empty request messages = %q want []", objEmpty["messages"])
	}
	for _, key := range []string{"tools", "tool_choice"} { // no tools -> neither key.
		if _, ok := objEmpty[key]; ok {
			t.Fatalf("body without tools carries %q: %s", key, string(bodyEmpty))
		}
	}
}

// TestEncodeToolsFunctionShape pins rule 2: function-shaped definitions in request order, tool_choice auto only with tools, strict never emitted.
func TestEncodeToolsFunctionShape(t *testing.T) {
	params := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`) // object schema survives verbatim.
	req := Request{Messages: []Message{userText("go")}, Tools: []ToolDefinition{{Name: "read_file", Description: "reads a file at path", Parameters: params}}}

	body, _, err := Encode(testResolved(), req, nil) // one tool definition.
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	obj := decodeObject(t, body)
	if choice, _ := wireString(t, obj, "tool_choice"); choice != "auto" {
		t.Fatalf(`tool_choice = %q want auto when tools are present`, obj["tool_choice"])
	}

	var tools []struct { // decoded into the exact OpenAI function shape.
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(bodyField(t, body, "tools"), &tools); err != nil || len(tools) != 1 {
		t.Fatalf("tools array wrong: %s", obj["tools"])
	}
	fn := tools[0].Function // every field of the function object is present.
	if tools[0].Type != "function" || fn.Name != "read_file" || fn.Description != "reads a file at path" {
		t.Fatalf("tool object wrong: %s (want type function with name/description)", obj["tools"])
	}

	var emittedParams json.RawMessage // the parameters value itself, not only its surrounding fields.
	if err := json.Unmarshal(fn.Parameters, &emittedParams); err != nil || string(emittedParams) != string(params) {
		t.Fatalf("parameters = %s want exactly the supplied schema bytes: %s", fn.Parameters, params)
	}
	var paramObj map[string]json.RawMessage // and it must decode as that object value (type/properties intact), not a re-serialized shape.
	if err := json.Unmarshal(emittedParams, &paramObj); err != nil || string(paramObj["type"]) != `"object"` {
		t.Fatalf("parameters did not survive as the supplied JSON object: %s", emittedParams)
	}

	req.Tools = append(req.Tools, ToolDefinition{Name: "write_file", Description: "", Parameters: json.RawMessage(`{}`)}) // order preserved.
	body2 := mustEncode(t, testResolved(), req)
	var two []struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(bodyField(t, body2, "tools"), &two); err != nil || len(two) != 2 || two[0].Function.Name != "read_file" || two[1].Function.Name != "write_file" {
		t.Fatalf("multiple tools lost their order or shape: %s", obj["tools"])
	}

	if strings.Contains(string(body), `"strict":`) { // strict is never emitted by this encoder.
		t.Fatalf("body carries a strict field: %s", string(body))
	}
}
