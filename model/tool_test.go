package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestNewToolDefinitionValidCases(t *testing.T) {
	cases := []struct {
		name string
		d    ToolDefinition
	}{
		{name: "object parameters", d: ToolDefinition{Name: "read_file", Description: "reads a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		{name: "empty object schema is valid JSON and an object", d: ToolDefinition{Name: "noop", Parameters: json.RawMessage(`{}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NewToolDefinition(tc.d)
			if err != nil {
				t.Fatalf("NewToolDefinition returned error: %v", err)
			}
			if out.Name != tc.d.Name || out.Description != tc.d.Description || string(out.Parameters) != string(tc.d.Parameters) {
				t.Fatalf("definition not preserved: %#v", out)
			}
		})
	}

	srcRaw := json.RawMessage(`{"type":"object"}`)
	in := ToolDefinition{Name: "x", Parameters: srcRaw}
	out, err := NewToolDefinition(in)
	if err != nil {
		t.Fatalf("NewToolDefinition returned error: %v", err)
	}
	srcRaw[1] = 'X' // corrupt caller bytes after construction.
	got := string(out.Parameters)
	if len(got) == 0 || got[:2] != `{"` || &out.Parameters[0] == &srcRaw[0] {
		t.Fatalf("parameters not deep copied at the accepting boundary: %s", out.Parameters)
	}

	// Phase boundary: no JSON-Schema dialect validation. An unknown object shape is accepted.
	if _, err := NewToolDefinition(ToolDefinition{Name: "x", Parameters: json.RawMessage(`{"weird":true}`)}); err != nil {
		t.Fatalf("schema-dialect content must not be validated here, got error %v", err)
	}
}

// TestNewToolDefinitionRejectsInvalid pins empty names and non-object or malformed schemas.
func TestNewToolDefinitionRejectsInvalid(t *testing.T) {
	cases := []struct {
		name   string
		d      ToolDefinition
		wantIs error
	}{
		{name: "empty name", d: ToolDefinition{Name: "", Parameters: json.RawMessage(`{}`)}, wantIs: ErrMissingField},
		{name: "malformed JSON parameters", d: ToolDefinition{Name: "x", Parameters: json.RawMessage(`{"type":`)}, wantIs: ErrInvalidParameters},
		{name: "array top level rejected", d: ToolDefinition{Name: "x", Parameters: json.RawMessage(`[1,2]`)}, wantIs: ErrInvalidParameters},
		{name: "string top level rejected", d: ToolDefinition{Name: "x", Parameters: json.RawMessage(`"params"`)}, wantIs: ErrInvalidParameters},
		{name: "number top level rejected", d: ToolDefinition{Name: "x", Parameters: json.RawMessage(`3`)}, wantIs: ErrInvalidParameters},
		{name: "boolean top level rejected", d: ToolDefinition{Name: "x", Parameters: json.RawMessage(`true`)}, wantIs: ErrInvalidParameters},
		{name: "null top level rejected", d: ToolDefinition{Name: "x", Parameters: json.RawMessage(`null`)}, wantIs: ErrInvalidParameters},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewToolDefinition(tc.d); !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}
}

// TestNewToolDefinitionParametersAreSyntaxOnly pins syntax-only parameter validation (no float64 decoding): a well-formed top-level object is accepted even when its numeric literals cannot be represented in Go's float64, and the bytes round-trip unchanged.
func TestNewToolDefinitionParametersAreSyntaxOnly(t *testing.T) {
	raw := `{"type":"object","properties":{"n":{"maximum":1e1000}}}`
	out, err := NewToolDefinition(ToolDefinition{Name: "big", Parameters: json.RawMessage(raw)})
	if err != nil {
		t.Fatalf("well-formed object with non-float64-representable number rejected (decoding-based validation): %v", err)
	}
	if string(out.Parameters) != raw {
		t.Fatalf("parameters not preserved byte-for-byte: got %s want %s", out.Parameters, raw)
	}
}

// TestNewToolCallPreservesArgumentsRaw pins byte-for-byte argument preservation without JSON validation.
func TestNewToolCallPreservesArgumentsRaw(t *testing.T) {
	cases := []string{
		`{"path":"foo.txt"}`, // object; schema validity is caller-owned tool preparation.
		``,                   // empty arguments stay raw input.
		`not json`,           // malformed stays raw input for caller-owned validation.
		`null`,               // null stays raw input.
		`[1,2]`,              // array stays raw input.
		`"primitive"`,        // primitive stays raw input.
	}
	for _, args := range cases {
		t.Run("args", func(t *testing.T) {
			srcArgs := json.RawMessage(args)
			out, err := NewToolCall(ToolCall{ID: "call_1", Name: "read_file", Arguments: srcArgs})
			if err != nil {
				t.Fatalf("NewToolCall returned error for raw argument input %q: %v", args, err)
			}
			if string(out.Arguments) != args {
				t.Fatalf("arguments = %s, want byte-for-byte preserved %s", out.Arguments, args)
			}

			// Corrupt the caller's bytes after construction; stored arguments must be untouched.
			if len(srcArgs) > 0 && &out.Arguments[0] == &srcArgs[0] {
				t.Fatal("stored arguments share memory with the input")
			}
			if len(srcArgs) > 1 {
				srcArgs[1] ^= 0xFF
			}
			out2, err := NewToolCall(ToolCall{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(args)})
			if err != nil || string(out.Arguments) != args {
				t.Fatalf("stored arguments changed after caller mutation: %s (err %v), want %s", out.Arguments, err, args)
			}
			if len(srcArgs) > 0 && &out2.Arguments[0] == &srcArgs[0] {
				t.Fatal("second construction shared bytes with the corrupted input")
			}

			out3, err := NewToolCall(ToolCall{ID: "c", Name: "f"})
			if err != nil || len(out3.Arguments) != 0 {
				t.Fatalf("zero arguments = %s (err %v), want empty raw value kept as-is", out3.Arguments, err)
			}
		})
	}

	extraSrc := json.RawMessage(`{"google":{"thought_signature":"sig"}}`)
	inExtra := Extra{"extra_content": extraSrc}
	out, err := NewToolCall(ToolCall{ID: "c1", Name: "f", Arguments: json.RawMessage(``), Extra: inExtra})
	if err != nil {
		t.Fatalf("NewToolCall returned error: %v", err)
	}
	extraSrc[0] = 'X' // corrupt the caller's extra bytes after construction.
	got := string(out.Extra["extra_content"])
	if len(got) == 0 || got[0] != '{' {
		t.Fatalf("tool call extra not deep copied at accepting boundary: %s", out.Extra["extra_content"])
	}

	casesErr := []struct {
		name   string
		tc     ToolCall
		wantIs error
	}{
		{name: "empty id rejected", tc: ToolCall{ID: "", Name: "f"}, wantIs: ErrMissingField},
		{name: "empty name rejected", tc: ToolCall{ID: "c1", Name: ""}, wantIs: ErrMissingField},
	}
	for _, tc := range casesErr {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewToolCall(tc.tc); !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}

	c1, _ := NewToolCall(ToolCall{ID: "c", Name: "f"})
	if c1.ID != "c" || c1.Name != "f" {
		t.Fatalf("call fields not preserved: %#v", c1)
	}

	var tcType ToolCall
	rt := reflect.TypeOf(tcType)
	if _, ok := rt.FieldByName("Type"); ok {
		t.Fatal("canonical tool call must not carry a wire-type field")
	}
	for _, name := range []string{"ID", "Name", "Arguments"} {
		if _, ok := rt.FieldByName(name); !ok {
			t.Fatalf("ToolCall missing contract field %s: %v", name, rt)
		}
	}
}

// TestNewToolResultStatusMatrix pins the closed status set and content rules.
func TestNewToolResultStatusMatrix(t *testing.T) {
	valid := []struct {
		name    string
		status  ToolResultStatus
		content string
	}{
		{name: "success empty", status: ResultSuccess, content: ""},
		{name: "success non-empty", status: ResultSuccess, content: "file body"},
		{name: "error non-empty", status: ResultError, content: "boom"},
		{name: "denied non-empty", status: ResultDenied, content: "user denied"},
		{name: "interrupted non-empty", status: ResultInterrupted, content: "stopped"},
	}
	for _, tc := range valid {
		t.Run(tc.name+" accepts", func(t *testing.T) {
			out, err := NewToolResult(ToolResult{CallID: "call_1", Status: tc.status, Content: tc.content})
			if err != nil {
				t.Fatalf("NewToolResult returned error: %v", err)
			}
			if out.CallID != "call_1" || out.Status != tc.status || out.Content != tc.content {
				t.Fatalf("result not preserved: %#v", out)
			}
		})
	}

	cases := []struct {
		name   string
		res    ToolResult
		wantIs error
	}{
		{name: "error empty content rejected", res: ToolResult{CallID: "c1", Status: ResultError, Content: ""}, wantIs: ErrMissingField},
		{name: "denied empty content rejected", res: ToolResult{CallID: "c1", Status: ResultDenied}, wantIs: ErrMissingField},
		{name: "interrupted empty content rejected", res: ToolResult{CallID: "c1", Status: ResultInterrupted, Content: ""}, wantIs: ErrMissingField},
	}
	for _, tc := range cases {
		t.Run(tc.name+" rejects", func(t *testing.T) {
			if _, err := NewToolResult(tc.res); !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}

	for i, status := range []string{"", "ok", "SUCCESS", "success ", string(rune('x')) + "status"} {
		if _, err := NewToolResult(ToolResult{CallID: "c1", Status: ToolResultStatus(status), Content: "detail"}); !errors.Is(err, ErrInvalidStatus) {
			t.Fatalf("case %d status=%q error = %v, want ErrInvalidStatus", i, status, err)
		}
	}

	if _, err := NewToolResult(ToolResult{CallID: "", Status: ResultSuccess}); !errors.Is(err, ErrMissingField) {
		t.Fatalf("empty call id error = %v, want ErrMissingField", err)
	}
}
