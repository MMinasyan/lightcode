package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestNewToolDefinitionCases pins the ToolDefinition accepting rules through
// both entry points: one inventory of object-shaped schemas, syntax-only
// huge-number input, unknown object shapes, and every invalid name/top-level
// shape, each with its original expected verdict.
func TestNewToolDefinitionCases(t *testing.T) {
	rows := []struct {
		name      string
		def       ToolDefinition
		wantErr   bool
		wantIsAny []error
	}{
		{"object parameters", ToolDefinition{Name: "read_file", Description: "reads a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}, false, nil},
		{"empty object schema is valid JSON and an object", ToolDefinition{Name: "noop", Parameters: json.RawMessage(`{}`)}, false, nil},
		{"object parameters with whitespace", ToolDefinition{Name: "f", Parameters: json.RawMessage("  {\"a\": 1}  ")}, false, nil},
		{"huge number literal is syntax-valid", ToolDefinition{Name: "f", Parameters: json.RawMessage(`{"n":1e1000}`)}, false, nil},
		{"syntax-only schema with a huge maximum", ToolDefinition{Name: "big", Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"maximum":1e1000}}}`)}, false, nil},
		{"unknown object shape is not dialect-validated", ToolDefinition{Name: "x", Parameters: json.RawMessage(`{"weird":true}`)}, false, nil},
		{"empty name", ToolDefinition{Parameters: json.RawMessage(`{}`)}, true, []error{ErrMissingField}},
		{"empty name precedes malformed parameters", ToolDefinition{Parameters: json.RawMessage(`{oops`)}, true, nil},
		{"empty parameters", ToolDefinition{Name: "f"}, true, nil},
		{"null parameters", ToolDefinition{Name: "f", Parameters: json.RawMessage(`null`)}, true, []error{ErrInvalidParameters}},
		{"array parameters", ToolDefinition{Name: "f", Parameters: json.RawMessage(`[1]`)}, true, nil},
		{"two-item array parameters", ToolDefinition{Name: "x", Parameters: json.RawMessage(`[1,2]`)}, true, []error{ErrInvalidParameters}},
		{"string top level", ToolDefinition{Name: "x", Parameters: json.RawMessage(`"params"`)}, true, []error{ErrInvalidParameters}},
		{"number top level", ToolDefinition{Name: "x", Parameters: json.RawMessage(`3`)}, true, []error{ErrInvalidParameters}},
		{"boolean top level", ToolDefinition{Name: "x", Parameters: json.RawMessage(`true`)}, true, []error{ErrInvalidParameters}},
		{"malformed JSON parameters", ToolDefinition{Name: "x", Parameters: json.RawMessage(`{"type":`)}, true, []error{ErrInvalidParameters}},
		{"truncated JSON parameters", ToolDefinition{Name: "f", Parameters: json.RawMessage(`{oops`)}, true, nil},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			out, newErr := NewToolDefinition(row.def)
			validateErr := ValidateToolDefinition(row.def)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			if len(row.wantIsAny) > 0 {
				testassert.Require(t, matchesAnySentinel(newErr, row.wantIsAny...) && matchesAnySentinel(validateErr, row.wantIsAny...), "fixture %s: errors = %v / %v, want one of %v", row.name, newErr, validateErr, row.wantIsAny)
			}
			if !row.wantErr {
				testassert.Require(t, out.Name == row.def.Name && out.Description == row.def.Description && string(out.Parameters) == string(row.def.Parameters), "definition not preserved: %#v", out)
			}
		})
	}

	// The validator mutates nothing, parameter bytes included.
	validated := ToolDefinition{Name: "echo", Description: "d", Parameters: json.RawMessage(`{"deep":{"a":[1,2]}}`)}
	before := ToolDefinition{Name: "echo", Description: "d", Parameters: json.RawMessage(`{"deep":{"a":[1,2]}}`)}
	{
		err := ValidateToolDefinition(validated)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(validated, before), "%v", "ValidateToolDefinition mutated its input")

	srcRaw := json.RawMessage(`{"type":"object"}`)
	in := ToolDefinition{Name: "x", Parameters: srcRaw}
	out, err := NewToolDefinition(in)
	testassert.NoError(t, err)
	srcRaw[1] = 'X' // corrupt caller bytes after construction.
	got := string(out.Parameters)
	testassert.Require(t, len(got) != 0 && got[:2] == `{"` && &out.Parameters[0] != &srcRaw[0], "parameters not deep copied at the accepting boundary: %s", out.Parameters)
}

// TestNewToolCallPreservesArgumentsRaw pins the ToolCall accepting rules
// through both entry points: raw arguments of every JSON shape (including
// malformed bytes) stay accepted and byte-for-byte owned, id/name failures
// precede Extra validation, and each row carries its original expected verdict.
func TestNewToolCallPreservesArgumentsRaw(t *testing.T) {
	rows := []struct {
		name      string
		call      ToolCall
		wantErr   bool
		wantIsAny []error
		contains  string
	}{
		{"rich", richToolCallFixture(), false, nil, ""},
		{"object arguments", ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`{"path":"foo.txt"}`)}, false, nil, ""}, // schema validity is caller-owned tool preparation.
		{"empty arguments", ToolCall{ID: "c", Name: "f"}, false, nil, ""},                                                    // empty arguments stay raw input.
		{"malformed arguments", ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`not json`)}, false, nil, ""},        // malformed stays raw input for caller-owned validation.
		{"null arguments", ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`null`)}, false, nil, ""},                 // null stays raw input.
		{"array arguments", ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`[1,2]`)}, false, nil, ""},               // array stays raw input.
		{"primitive arguments", ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`"x"`)}, false, nil, ""},             // primitive stays raw input.
		{"empty id", ToolCall{Name: "f"}, true, []error{ErrMissingField}, ""},
		{"empty name", ToolCall{ID: "c"}, true, nil, ""},
		{"empty name with id", ToolCall{ID: "c1", Name: ""}, true, []error{ErrMissingField}, ""},
		{"malformed extra", ToolCall{ID: "c", Name: "f", Extra: Extra{"k": json.RawMessage(`{oops`)}}, true, nil, ""},
		{"id precedence over malformed extra", ToolCall{Name: "f", Extra: Extra{"k": json.RawMessage(`{oops`)}}, true, nil, "non-empty id and name"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			out, newErr := NewToolCall(row.call)
			validateErr := ValidateToolCall(row.call)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			if len(row.wantIsAny) > 0 {
				testassert.Require(t, matchesAnySentinel(newErr, row.wantIsAny...) && matchesAnySentinel(validateErr, row.wantIsAny...), "fixture %s: errors = %v / %v, want one of %v", row.name, newErr, validateErr, row.wantIsAny)
			}
			testassert.Require(t, row.contains == "" || (strings.Contains(newErr.Error(), row.contains) && strings.Contains(validateErr.Error(), row.contains)), "fixture %s reached the wrong rule: %v / %v, want a failure naming %q", row.name, newErr, validateErr, row.contains)
			if !row.wantErr {
				testassert.Require(t, out.ID == row.call.ID && out.Name == row.call.Name && string(out.Arguments) == string(row.call.Arguments), "call fields not preserved: %#v", out)
				testassert.Require(t, len(out.Arguments) == 0 || &out.Arguments[0] != &row.call.Arguments[0], "%v", "stored arguments share memory with the input")
			}
		})
	}

	// The validator mutates nothing, argument bytes and extras included.
	validated := richToolCallFixture()
	before := richToolCallFixture()
	{
		err := ValidateToolCall(validated)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(validated, before), "%v", "ValidateToolCall mutated its input")

	// Corrupt one representative caller input after construction; the stored
	// bytes must survive the mutation.
	srcArgs := json.RawMessage(`{"path":"foo.txt"}`)
	copied, err := NewToolCall(ToolCall{ID: "call_1", Name: "read_file", Arguments: srcArgs})
	testassert.NoError(t, err)
	srcArgs[1] ^= 0xFF
	testassert.Require(t, string(copied.Arguments) == `{"path":"foo.txt"}`, "stored arguments changed after caller mutation: %s", copied.Arguments)

	extraSrc := json.RawMessage(`{"google":{"thought_signature":"sig"}}`)
	inExtra := Extra{"extra_content": extraSrc}
	out, err := NewToolCall(ToolCall{ID: "c1", Name: "f", Arguments: json.RawMessage(``), Extra: inExtra})
	testassert.NoError(t, err)
	extraSrc[0] = 'X' // corrupt the caller's extra bytes after construction.
	got := string(out.Extra["extra_content"])
	testassert.Require(t, len(got) != 0 && got[0] == '{', "tool call extra not deep copied at accepting boundary: %s", out.Extra["extra_content"])

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
