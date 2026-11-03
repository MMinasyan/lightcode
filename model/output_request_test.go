package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// assistantMsg builds a valid assistant message on the full test source and applies an optional mutation to it.
func assistantMsg(t *testing.T, mutate func(*Message)) Message {
	t.Helper()
	m := mustMessage(t, Message{Role: RoleAssistant, Source: fullRef})
	if mutate != nil {
		mutate(&m)
	}
	return m
}

// TestNewOutputCases pins the Output accepting boundary through both entry
// points: one inventory of status, source, message and payload shapes, each
// with its original expected verdict and error class/detail where the owning
// test asserted one. The constructor's field preservation and the validator's
// non-mutation observation stay alongside the table.
func TestNewOutputCases(t *testing.T) {
	textMsg := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartText, Text: "done"}} })
	noPayloadMsg := assistantMsg(t, nil)
	userRoleMsg := mustMessage(t, Message{Role: RoleUser})
	payloadMsg := assistantMsg(t, func(m *Message) {
		m.Content = []ContentPart{{Kind: PartText, Text: "x"}}
		c, _ := NewToolCall(ToolCall{ID: "c1", Name: "f", Arguments: json.RawMessage(`{oops`)})
		m.ToolCalls = []ToolCall{c}
	})
	msgWithCalls := assistantMsg(t, func(m *Message) {
		c, _ := NewToolCall(ToolCall{ID: "c1", Name: "f"})
		m.ToolCalls = []ToolCall{c}
	})
	msgImage := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartImageURL, URL: "https://example.com/a.png"}} })
	msgOpaque := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartOpaque, OpaqueWireType: "thinking"}} })
	extraOnly := Extra{"k": json.RawMessage(`"v"`)}
	partWithExtra := ContentPart{Kind: PartText, Extra: extraOnly}
	msgExtraOnly := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{partWithExtra} })
	refusalOnly, _ := NewMessage(Message{Role: RoleAssistant, Source: fullRef, Refusal: "no"})
	mismatchedMsg := assistantMsg(t, nil)
	msgEmptyPart := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartText}} })
	nullExtraOnly := Extra{"n": json.RawMessage(`null`)}
	partNullExtra := ContentPart{Kind: PartText, Extra: nullExtraOnly}
	msgNullExtraPart := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{partNullExtra} })
	msgNameOnly := assistantMsg(t, func(m *Message) { m.Name = "worker" })

	rows := []struct {
		name      string
		out       Output
		wantErr   bool
		wantIsAny []error
		contains  string
	}{
		{"rich completed", richOutputFixture(), false, nil, ""},
		{"completed with text", Output{Status: OutputCompleted, Source: fullRef, Message: &textMsg}, false, nil, ""},
		{"tool-call-only assistant is valid completed payload", Output{Status: OutputCompleted, Source: fullRef, Message: &msgWithCalls}, false, nil, ""},
		{"errored without message", Output{Status: OutputErrored, Source: fullRef, Detail: "boom"}, false, nil, ""},
		{"interrupted with partial", Output{Status: OutputInterrupted, Source: fullRef, Message: &textMsg, Detail: "stopped"}, false, nil, ""},
		{"image url only counts as payload", Output{Status: OutputCompleted, Source: fullRef, Message: &msgImage}, false, nil, ""},
		{"opaque wire type alone counts as payload", Output{Status: OutputCompleted, Source: fullRef, Message: &msgOpaque}, false, nil, ""},
		{"part with only a finalized non-null extra counts as payload", Output{Status: OutputCompleted, Source: fullRef, Message: &msgExtraOnly}, false, nil, ""},
		{"refusal only counts as payload", Output{Status: OutputCompleted, Source: fullRef, Message: &refusalOnly}, false, nil, ""},
		{"invalid status", Output{Status: OutputStatus("done"), Source: fullRef, Detail: "d"}, true, []error{ErrInvalidStatus}, "invalid status"},
		{"empty status", Output{Status: OutputStatus(""), Source: fullRef, Detail: "d"}, true, []error{ErrInvalidStatus}, ""},
		{"uppercase status", Output{Status: OutputStatus("COMPLETED"), Source: fullRef, Detail: "d"}, true, []error{ErrInvalidStatus}, ""},
		{"zero source", Output{Status: OutputCompleted, Message: &textMsg}, true, []error{ErrMissingSource}, "complete source model identity"},
		{"partial source", Output{Status: OutputErrored, Source: ModelRef{Provider: "openai"}, Detail: "d"}, true, []error{ErrMissingSource}, ""},
		{"completed without message", Output{Status: OutputCompleted, Source: fullRef}, true, nil, "requires one assistant message"},
		{"completed without payload", Output{Status: OutputCompleted, Source: fullRef, Message: &noPayloadMsg}, true, nil, "requires an assistant payload"},
		{"empty text part with no extras is not a payload", Output{Status: OutputCompleted, Source: fullRef, Message: &msgEmptyPart}, true, nil, ""},
		{"part whose only extra finalizes to null is not a payload", Output{Status: OutputCompleted, Source: fullRef, Message: &msgNullExtraPart}, true, nil, ""},
		{"name does not count as payload", Output{Status: OutputCompleted, Source: fullRef, Message: &msgNameOnly}, true, nil, ""},
		{"completed with detail", Output{Status: OutputCompleted, Source: fullRef, Message: &textMsg, Detail: "boom"}, true, nil, "must carry empty detail"},
		{"errored without detail", Output{Status: OutputErrored, Source: fullRef}, true, []error{ErrMissingField}, "requires non-empty detail"},
		{"interrupted with tool calls", Output{Status: OutputInterrupted, Source: fullRef, Message: &payloadMsg, Detail: "stopped"}, true, nil, "must not carry tool calls"},
		{"message source mismatch", Output{Status: OutputCompleted, Source: ModelRef{Provider: "acme", Model: "other"}, Message: &mismatchedMsg}, true, []error{ErrSourceMismatch}, "differs from output source"},
		{"message source mismatch openrouter", Output{Status: OutputCompleted, Source: ModelRef{Provider: "openrouter", Model: "other"}, Message: &mismatchedMsg}, true, []error{ErrSourceMismatch}, ""},
		{"non-assistant message", Output{Status: OutputCompleted, Source: fullRef, Message: &userRoleMsg}, true, []error{ErrInvalidRole}, "carry only assistant messages"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			out, newErr := NewOutput(row.out)
			validateErr := ValidateOutput(row.out)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			if len(row.wantIsAny) > 0 {
				testassert.Require(t, matchesAnySentinel(newErr, row.wantIsAny...) && matchesAnySentinel(validateErr, row.wantIsAny...), "fixture %s: errors = %v / %v, want one of %v", row.name, newErr, validateErr, row.wantIsAny)
			}
			testassert.Require(t, row.contains == "" || (strings.Contains(newErr.Error(), row.contains) && strings.Contains(validateErr.Error(), row.contains)), "fixture %s reached the wrong rule: %v / %v, want a failure naming %q", row.name, newErr, validateErr, row.contains)
			if !row.wantErr {
				testassert.Require(t, out.Status == row.out.Status && out.Source == row.out.Source && out.Detail == row.out.Detail && (row.out.Message == nil) == (out.Message == nil), "output not preserved: %#v", out)
			}
		})
	}

	// The validator mutates nothing: an independently built value-equal
	// sibling compares equal after the call, nested reference types included.
	in := richOutputFixture()
	before := richOutputFixture()
	{
		err := ValidateOutput(in)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(in, before), "%v", "ValidateOutput mutated its input")

	var zeroUsage Usage
	outWithZeroUsage, err := NewOutput(Output{Status: OutputCompleted, Source: fullRef, Message: &textMsg, Usage: &zeroUsage})
	testassert.Require(t, err == nil && outWithZeroUsage.Usage != nil, "present all-zero usage must be retained (known zero): %#v (err %v)", outWithZeroUsage, err)

	outNoUsage := Output{Status: OutputCompleted, Source: fullRef, Message: &textMsg}
	gotOut, _ := NewOutput(outNoUsage)
	testassert.Require(t, gotOut.Usage == nil, "absent usage must stay absent (unknown/not reported): %#v", gotOut.Usage)

	negIn := Usage{InputTokens: -1, CachedInputTokens: 0, OutputTokens: -2}
	gotNeg, err := NewOutput(Output{Status: OutputErrored, Source: fullRef, Detail: "d", Usage: &negIn})
	testassert.Require(t, err == nil && gotNeg.Usage != nil && *gotNeg.Usage == negIn, "signed int usage not preserved verbatim: %#v (err %v)", gotNeg.Usage, err)

	srcRaw := json.RawMessage(`"thinking"`)
	extraMsgSrc := assistantMsg(t, func(m *Message) { m.Extra = Extra{"reasoning_content": srcRaw} })
	gotExtraOut, err := NewOutput(Output{Status: OutputErrored, Source: fullRef, Message: &extraMsgSrc, Detail: "d"})
	testassert.Require(t, err == nil && gotExtraOut.Message != nil, "NewOutput returned error or dropped message: %v", err)
	srcRaw[1] = 'X' // corrupt caller bytes after construction.
	stored := string(gotExtraOut.Message.Extra["reasoning_content"])
	testassert.Require(t, len(stored) >= 2 && stored[:2] == `"t`, "output message extra not deep copied at accepting boundary: %s", gotExtraOut.Message.Extra["reasoning_content"])
}

// TestAssistantContentAndPayloadQueries pins the two shared scalar queries against their finalization view: content is parts, refusal and surviving extras; payload adds tool calls; a nil message answers false to both; extras are read without validation.
func TestAssistantContentAndPayloadQueries(t *testing.T) {
	call, _ := NewToolCall(ToolCall{ID: "c1", Name: "f"})
	cases := []struct {
		name    string
		msg     *Message
		content bool
		payload bool
	}{
		{name: "nil message", msg: nil, content: false, payload: false},
		{name: "empty assistant", msg: &Message{Role: RoleAssistant, Source: fullRef}, content: false, payload: false},
		{name: "text part", msg: &Message{Content: []ContentPart{{Kind: PartText, Text: "x"}}}, content: true, payload: true},
		{name: "url part", msg: &Message{Content: []ContentPart{{Kind: PartImageURL, URL: "https://a/b"}}}, content: true, payload: true},
		{name: "opaque part", msg: &Message{Content: []ContentPart{{Kind: PartOpaque, OpaqueWireType: "thinking"}}}, content: true, payload: true},
		{name: "part extra only", msg: &Message{Content: []ContentPart{{Kind: PartText, Extra: Extra{"k": json.RawMessage(`1`)}}}}, content: true, payload: true},
		{name: "null-only part extras", msg: &Message{Content: []ContentPart{{Kind: PartText, Extra: Extra{"k": json.RawMessage(`null`)}}}}, content: false, payload: false},
		{name: "empty part", msg: &Message{Content: []ContentPart{{Kind: PartText}}}, content: false, payload: false},
		{name: "refusal only", msg: &Message{Refusal: "no"}, content: true, payload: true},
		{name: "message extra only", msg: &Message{Extra: Extra{"k": json.RawMessage(`"v"`)}}, content: true, payload: true},
		{name: "null-only message extras", msg: &Message{Extra: Extra{"k": json.RawMessage(`null`)}}, content: false, payload: false},
		{name: "malformed extra counts as finalization retains it", msg: &Message{Extra: Extra{"k": json.RawMessage(`{`)}}, content: true, payload: true},
		{name: "tool calls only", msg: &Message{ToolCalls: []ToolCall{call}}, content: false, payload: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			{
				got := HasAssistantContent(tc.msg)
				testassert.Equal(t, got, tc.content)
			}
			{
				got := hasAssistantPayload(tc.msg)
				testassert.Equal(t, got, tc.payload)
			}
		})
	}

	p := ContentPart{Kind: PartText, Extra: Extra{"k": json.RawMessage(`null`), "m": json.RawMessage(`0`)}}
	testassert.Require(t, ContentPartHasPayload(p), "%v", "part with one surviving extra reported empty")
	empty := ContentPart{Kind: PartText, Extra: Extra{"k": json.RawMessage(`null`)}}
	testassert.Require(t, !ContentPartHasPayload(empty), "%v", "part with null-only extras reported payload")
}

// TestNewRequestValidAndOwnership pins the logical request contract through
// both entry points: one inventory of message and tool shapes with their
// original expected verdicts, the message pass ahead of the tools pass, and
// the duplicate-name rejection after the second tool's own validation, plus
// the constructor's owned-copy checks.
func TestNewRequestValidAndOwnership(t *testing.T) {
	validMessage := func() Message {
		return assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartText, Text: "hi"}} })
	}
	validTool := func(name string) ToolDefinition {
		return ToolDefinition{Name: name, Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}
	}

	rows := []struct {
		name      string
		req       Request
		wantErr   bool
		wantIsAny []error
		prefix    string // required leading text of the detail
		contains  string // required substring of the detail
	}{
		{"valid messages and tools", Request{Messages: []Message{validMessage()}, Tools: []ToolDefinition{validTool("echo")}}, false, nil, "", ""},
		{"empty request", Request{}, false, nil, "", ""},
		{"tools only capture shape", Request{Tools: []ToolDefinition{validTool("echo")}}, false, nil, "", ""},
		{"invalid message", Request{Messages: []Message{{Role: Role("bogus")}}}, true, nil, "messages[0]:", ""},
		{"invalid message part", Request{Messages: []Message{{Role: RoleUser, Content: []ContentPart{{Kind: PartText, URL: "u"}}}}}, true, nil, "messages[0]: content[0]:", ""},
		{"message error beats tool error", Request{Messages: []Message{{Role: Role("bogus")}}, Tools: []ToolDefinition{{}}}, true, nil, "messages[0]:", ""},
		{"assistant message without source", Request{Messages: []Message{{Role: RoleAssistant}}}, true, []error{ErrMissingSource}, "", ""},
		{"invalid tool", Request{Tools: []ToolDefinition{{Name: "f"}}}, true, nil, "tools[0]:", ""},
		{"second tool invalid after a valid first", Request{Tools: []ToolDefinition{validTool("a"), {Name: "f"}}}, true, nil, "tools[1]:", ""},
		{"invalid definition with empty name and array parameters", Request{Tools: []ToolDefinition{{Parameters: json.RawMessage(`[1]`)}}}, true, []error{ErrMissingField, ErrInvalidParameters}, "", ""},
		{"duplicate name", Request{Tools: []ToolDefinition{validTool("echo"), validTool("echo")}}, true, []error{ErrDuplicateToolName}, "", "duplicate tool definition name"},
		{"duplicate name t", Request{Tools: []ToolDefinition{{Name: "t", Parameters: json.RawMessage(`{}`)}, {Name: "t", Parameters: json.RawMessage(`{}`)}}}, true, []error{ErrDuplicateToolName}, "", ""},
		{"duplicate reported after the second tool's own validation", Request{Tools: []ToolDefinition{validTool("echo"), {Name: "echo", Parameters: json.RawMessage(`{oops`)}}}, true, nil, "tools[1]:", "malformed JSON parameters"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			out, newErr := NewRequest(row.req)
			validateErr := ValidateRequest(row.req)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			if len(row.wantIsAny) > 0 {
				testassert.Require(t, matchesAnySentinel(newErr, row.wantIsAny...) && matchesAnySentinel(validateErr, row.wantIsAny...), "fixture %s: errors = %v / %v, want one of %v", row.name, newErr, validateErr, row.wantIsAny)
			}
			testassert.Require(t, (row.prefix == "" || (strings.HasPrefix(newErr.Error(), row.prefix) && strings.HasPrefix(validateErr.Error(), row.prefix))) && (row.contains == "" || (strings.Contains(newErr.Error(), row.contains) && strings.Contains(validateErr.Error(), row.contains))), "fixture %s error = %q / %q, want the %q prefix; fixture %s reached the wrong rule: %v / %v, want a failure naming %q", row.name, newErr, validateErr, row.prefix, row.name, newErr, validateErr, row.contains)
			if !row.wantErr {
				testassert.Require(t, len(out.Messages) == len(row.req.Messages) && len(out.Tools) == len(row.req.Tools), "request shape not preserved: %#v", out)
				for i, msg := range row.req.Messages {
					testassert.Require(t, out.Messages[i].Role == msg.Role && out.Messages[i].Name == msg.Name && out.Messages[i].ToolCallID == msg.ToolCallID, "message %d not preserved: %#v", i, out.Messages[i])
				}
				for i, def := range row.req.Tools {
					testassert.Require(t, out.Tools[i].Name == def.Name && out.Tools[i].Description == def.Description && string(out.Tools[i].Parameters) == string(def.Parameters), "tool %d not preserved: %#v", i, out.Tools[i])
				}
			}
		})
	}

	// The validator mutates nothing: a value-equal sibling still compares
	// equal after the call, nested reference types included.
	validated := Request{Messages: []Message{validMessage()}, Tools: []ToolDefinition{validTool("echo")}}
	before := Request{Messages: []Message{validMessage()}, Tools: []ToolDefinition{validTool("echo")}}
	{
		err := ValidateRequest(validated)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(validated, before), "%v", "ValidateRequest mutated its input")

	msgs := []Message{mustMessage(t, Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "hi"}}})}
	d1, _ := NewToolDefinition(ToolDefinition{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)})
	defs := []ToolDefinition{d1}

	reqIn := Request{Messages: msgs, Tools: defs}
	out, err := NewRequest(reqIn)
	testassert.NoError(t, err)
	testassert.Require(t, len(out.Messages) == 1 && out.Messages[0].TextContent() == "hi" && (len(out.Tools) == 1 && out.Tools[0].Name == "read_file"), "messages not preserved: %#v; tools not preserved: %#v", out.Messages, out.Tools)

	msgs[0].Content = nil // corrupt caller slices after construction.
	d1.Parameters = json.RawMessage(`{}`)
	testassert.Require(t, len(out.Messages[0].Content) == 1 && out.Messages[0].TextContent() == "hi", "owned request messages must survive input mutation: %#v", out.Messages)

	defParams := json.RawMessage(`{"type":"object"}`)
	dIn := ToolDefinition{Name: "x", Parameters: defParams}
	out3, err := NewRequest(Request{Tools: []ToolDefinition{dIn}})
	testassert.NoError(t, err)
	defParams[1] = 'X' // corrupt caller bytes after construction.
	storedParams := string(out3.Tools[0].Parameters)
	testassert.Require(t, len(storedParams) != 0 && storedParams[:2] == `{"`, "request tool parameters not deep copied: %s", out3.Tools[0].Parameters)
}

// TestNewRequestNeverRetainsZeroLengthSliceBacking pins the ownership rule for request/message slices through their shared clone path (the one Encode re-runs at its trust boundary): a zero-length slice accepted with spare capacity must not retain caller-owned backing storage, so later appends on either side cannot see into each other. Representation is deliberately unpinned — nil or an independently allocated empty both satisfy the alias-free rule this row asserts behaviorally only.
func TestNewRequestNeverRetainsZeroLengthSliceBacking(t *testing.T) {
	// zero length with real spare capacity on caller-owned storage: the exact retained-capacity shape under test. A literal empty slice would not exercise it (its backing carries no slots either side's appends could share).
	zeroContent := make([]ContentPart, 0, 4)                                                 // four slots so both sides' two-part appends below fit inside one shared array if retention were buggy... (a smaller capacity would force an early reallocation and mask the aliasing this row exists to catch).
	fullCallsBase := []ToolCall{{ID: "call-a", Name: "alpha"}, {ID: "call-b", Name: "beta"}} // two seed calls so truncating below leaves at least one shared slot of headroom for both sides' appends.
	zeroCalls := fullCallsBase[:0]                                                           // zero length with that capacity now riding along on the caller's own array.

	inMsg, err := NewMessage(Message{Role: RoleAssistant, Source: fullRef, Content: zeroContent, ToolCalls: zeroCalls}) // valid assistant shape so the fixture reaches the clone path exactly as Encode's boundary would.
	testassert.NoError(t, err)

	out, outErr := NewRequest(Request{Messages: []Message{inMsg}}) // same shared clone path the encoder invokes per call — this row judges ITS retention behavior.
	testassert.NoError(t, outErr)

	out.Messages[0].Content = append(out.Messages[0].Content, ContentPart{Kind: PartText, Text: "out-1"}, ContentPart{Kind: PartText, Text: "out-2"}) // retained-side appends into whatever backing it was given... (must land in storage the caller never owned).
	inMsg.Content = append(inMsg.Content, ContentPart{Kind: PartText, Text: "in-1"}, ContentPart{Kind: PartText, Text: "in-2"})                       // ...and vice versa from the caller side.

	out.Messages[0].ToolCalls = append(out.Messages[0].ToolCalls, ToolCall{ID: "call-out", Name: "out-call"}) // same discipline on the calls scope... (one slot each fits inside a shared two-slot array if one existed).
	inMsg.ToolCalls = append(inMsg.ToolCalls, ToolCall{ID: "call-in", Name: "in-call"})                       // ...so both fields of the message are judged by their own row.

	if got := out.Messages[0].Content; len(got) != 2 || got[0].Text != "out-1" || got[1].Text != "out-2" {
		t.Fatalf("retained content observed caller-side appends through shared backing storage: %#v (want exactly the two retained-side parts)", got) // pre-fix this slice is the caller's own header, so its slots were clobbered by the in-*-appends above — the precise leak shape.
	} else if got := inMsg.Content; len(got) != 2 || got[0].Text != "in-1" || got[1].Text != "in-2" { // symmetric direction: the caller's view must hold exactly its own appends too.
		t.Fatalf("caller content observed retained-side appends through shared backing storage: %#v (want exactly the two caller parts)", got)
	}

	if got := out.Messages[0].ToolCalls; len(got) != 1 || got[0].ID != "call-out" { // calls scope, retained side.
		t.Fatalf("retained tool calls observed caller-side appends through shared backing storage: %#v (want exactly the one retained call)", got)
	} else if got := inMsg.ToolCalls; len(got) != 1 || got[0].ID != "call-in" { // ...and its mirror direction.
		t.Fatalf("caller tool calls observed retained-side appends through shared backing storage: %#v (want exactly the one caller call)", got)
	}
}

// TestNewOutputSourceIdentityComparedByFields pins that output source identity is compared by struct fields (Provider and Model separately), not via the lossy String() rendering: two complete identities whose rendered forms collide at the first-slash split must be rejected, while identical field values are accepted even when a provider value contains slashes.
func TestNewOutputSourceIdentityComparedByFields(t *testing.T) {
	colliding := ModelRef{Provider: "openrouter", Model: "other/model"}  // renders as openrouter/other/model.
	splitOther := ModelRef{Provider: "openrouter/other", Model: "model"} // same rendering, different field split.

	msgIn := assistantMsg(t, func(m *Message) { m.Source = splitOther; m.Content = []ContentPart{{Kind: PartText, Text: "x"}} })
	outIn := Output{Status: OutputCompleted, Source: colliding, Message: &msgIn}
	{
		_, err := NewOutput(outIn)
		testassert.Require(t, errors.Is(err, ErrSourceMismatch), "rendering-colliding identities accepted; error = %v (want ErrSourceMismatch)", err)
	}

	slashyRef := ModelRef{Provider: "prov/er", Model: "model/x"} // complete identity with slashes in both fields.
	msgIn2 := assistantMsg(t, func(m *Message) { m.Source = slashyRef; m.Content = []ContentPart{{Kind: PartText, Text: "x"}} })
	outIn2 := Output{Status: OutputCompleted, Source: slashyRef, Message: &msgIn2}
	{
		_, err := NewOutput(outIn2)
		testassert.NoError(t, err)
	}
}
