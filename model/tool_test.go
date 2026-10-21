package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
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
			testassert.NoError(t, err)
			testassert.Require(t, out.Name == tc.d.Name && out.Description == tc.d.Description && string(out.Parameters) == string(tc.d.Parameters), "definition not preserved: %#v", out)
		})
	}

	srcRaw := json.RawMessage(`{"type":"object"}`)
	in := ToolDefinition{Name: "x", Parameters: srcRaw}
	out, err := NewToolDefinition(in)
	testassert.NoError(t, err)
	srcRaw[1] = 'X' // corrupt caller bytes after construction.
	got := string(out.Parameters)
	testassert.Require(t, len(got) != 0 && got[:2] == `{"` && &out.Parameters[0] != &srcRaw[0], "parameters not deep copied at the accepting boundary: %s", out.Parameters)

	// Phase boundary: no JSON-Schema dialect validation. An unknown object shape is accepted.
	{
		_, err := NewToolDefinition(ToolDefinition{Name: "x", Parameters: json.RawMessage(`{"weird":true}`)})
		testassert.NoError(t, err)
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
			{
				_, err := NewToolDefinition(tc.d)
				testassert.Require(t, errors.Is(err, tc.wantIs), "error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}
}

// TestNewToolDefinitionParametersAreSyntaxOnly pins syntax-only parameter validation (no float64 decoding): a well-formed top-level object is accepted even when its numeric literals cannot be represented in Go's float64, and the bytes round-trip unchanged.
func TestNewToolDefinitionParametersAreSyntaxOnly(t *testing.T) {
	raw := `{"type":"object","properties":{"n":{"maximum":1e1000}}}`
	out, err := NewToolDefinition(ToolDefinition{Name: "big", Parameters: json.RawMessage(raw)})
	testassert.NoError(t, err)
	testassert.Require(t, string(out.Parameters) == raw, "parameters not preserved byte-for-byte: got %s want %s", out.Parameters, raw)
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
			testassert.Require(t, err == nil, "NewToolCall returned error for raw argument input %q: %v", args, err)
			testassert.Require(t, string(out.Arguments) == args, "arguments = %s, want byte-for-byte preserved %s", out.Arguments, args)

			// Corrupt the caller's bytes after construction; stored arguments must be untouched.
			testassert.Require(t, len(srcArgs) <= 0 || &out.Arguments[0] != &srcArgs[0], "%v", "stored arguments share memory with the input")
			if len(srcArgs) > 1 {
				srcArgs[1] ^= 0xFF
			}
			out2, err := NewToolCall(ToolCall{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(args)})
			testassert.Require(t, err == nil && string(out.Arguments) == args, "stored arguments changed after caller mutation: %s (err %v), want %s", out.Arguments, err, args)
			testassert.Require(t, len(srcArgs) <= 0 || &out2.Arguments[0] != &srcArgs[0], "%v", "second construction shared bytes with the corrupted input")

			out3, err := NewToolCall(ToolCall{ID: "c", Name: "f"})
			testassert.Require(t, err == nil && len(out3.Arguments) == 0, "zero arguments = %s (err %v), want empty raw value kept as-is", out3.Arguments, err)
		})
	}

	extraSrc := json.RawMessage(`{"google":{"thought_signature":"sig"}}`)
	inExtra := Extra{"extra_content": extraSrc}
	out, err := NewToolCall(ToolCall{ID: "c1", Name: "f", Arguments: json.RawMessage(``), Extra: inExtra})
	testassert.NoError(t, err)
	extraSrc[0] = 'X' // corrupt the caller's extra bytes after construction.
	got := string(out.Extra["extra_content"])
	testassert.Require(t, len(got) != 0 && got[0] == '{', "tool call extra not deep copied at accepting boundary: %s", out.Extra["extra_content"])

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
			{
				_, err := NewToolCall(tc.tc)
				testassert.Require(t, errors.Is(err, tc.wantIs), "error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}

	c1, _ := NewToolCall(ToolCall{ID: "c", Name: "f"})
	testassert.Require(t, c1.ID == "c" && c1.Name == "f", "call fields not preserved: %#v", c1)

	var tcType ToolCall
	rt := reflect.TypeOf(tcType)
	{
		_, ok := rt.FieldByName("Type")
		testassert.Require(t, !ok, "%v", "canonical tool call must not carry a wire-type field")
	}
	for _, name := range []string{"ID", "Name", "Arguments"} {
		{
			_, ok := rt.FieldByName(name)
			testassert.Require(t, ok, "ToolCall missing contract field %s: %v", name, rt)
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
			testassert.NoError(t, err)
			testassert.Require(t, out.CallID == "call_1" && out.Status == tc.status && out.Content == tc.content, "result not preserved: %#v", out)
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
			{
				_, err := NewToolResult(tc.res)
				testassert.Require(t, errors.Is(err, tc.wantIs), "error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}

	for i, status := range []string{"", "ok", "SUCCESS", "success ", string(rune('x')) + "status"} {
		{
			_, err := NewToolResult(ToolResult{CallID: "c1", Status: ToolResultStatus(status), Content: "detail"})
			testassert.Require(t, errors.Is(err, ErrInvalidStatus), "case %d status=%q error = %v, want ErrInvalidStatus", i, status, err)
		}
	}

	{
		_, err := NewToolResult(ToolResult{CallID: "", Status: ResultSuccess})
		testassert.Require(t, errors.Is(err, ErrMissingField), "empty call id error = %v, want ErrMissingField", err)
	}
}
