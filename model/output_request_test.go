package model

import (
	"encoding/json"
	"errors"
	"testing"
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

// TestNewOutputStatusAndSourceRules pins the closed statuses, source rules and detail rules.
func TestNewOutputStatusAndSourceRules(t *testing.T) {
	textMsg := assistantMsg(t, func(m *Message) {
		p, _ := NewContentPart(ContentPart{Kind: PartText, Text: "done"})
		m.Content = []ContentPart{p}
	})

	valid := []struct {
		name   string
		status OutputStatus
		msg    *Message
		detail string
	}{
		{name: "completed with text", status: OutputCompleted, msg: &textMsg, detail: ""},
		{
			name:   "tool-call-only assistant is valid completed payload",
			status: OutputCompleted,
			msg: func() *Message {
				m := assistantMsg(t, nil)
				c, _ := NewToolCall(ToolCall{ID: "c1", Name: "f"})
				m.ToolCalls = []ToolCall{c}
				return &m
			}(),
			detail: "",
		},
		{name: "errored omits message, requires detail", status: OutputErrored, msg: nil, detail: "protocol chunk parse"},
		{name: "interrupted with partial text and detail", status: OutputInterrupted, msg: &textMsg, detail: "agent interrupted"},
	}
	for _, tc := range valid {
		t.Run(tc.name+" accepts", func(t *testing.T) {
			out, err := NewOutput(Output{Status: tc.status, Source: fullRef, Message: tc.msg, Detail: tc.detail})
			if err != nil {
				t.Fatalf("NewOutput returned error: %v", err)
			}
			if out.Status != tc.status || out.Source != fullRef || out.Detail != tc.detail {
				t.Fatalf("output not preserved: %#v", out)
			}
			if (tc.msg == nil) != (out.Message == nil) {
				t.Fatal("message presence changed")
			}
		})
	}

	cases := []struct {
		name   string
		out    Output
		wantIs error // nil means any non-nil error is acceptable.
	}{
		{name: "zero source rejected", out: Output{Status: OutputCompleted, Message: &textMsg}, wantIs: ErrMissingSource},
		{name: "partial source rejected", out: Output{Status: OutputErrored, Source: ModelRef{Provider: "openai"}, Detail: "d"}, wantIs: ErrMissingSource},
		{name: "completed without message rejected", out: Output{Status: OutputCompleted, Source: fullRef}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" rejects", func(t *testing.T) {
			if _, err := NewOutput(tc.out); err == nil {
				t.Fatal("invalid output accepted")
			} else if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
		})
	}

	for i, status := range []string{"", "done", "COMPLETED"} {
		outIn := Output{Status: OutputStatus(status), Source: fullRef, Detail: "d"}
		if _, err := NewOutput(outIn); !errors.Is(err, ErrInvalidStatus) {
			t.Fatalf("case %d status=%q error = %v, want ErrInvalidStatus", i, status, err)
		}
	}

	mismatchedMsg := assistantMsg(t, nil)
	outIn := Output{Status: OutputCompleted, Source: ModelRef{Provider: "openrouter", Model: "other"}, Message: &mismatchedMsg, Detail: ""}
	if _, err := NewOutput(outIn); !errors.Is(err, ErrSourceMismatch) {
		t.Fatalf("message source != output source error = %v, want ErrSourceMismatch", err)
	}

	completedWithDetail := Output{Status: OutputCompleted, Source: fullRef, Message: &textMsg, Detail: "boom"}
	if _, err := NewOutput(completedWithDetail); err == nil {
		t.Fatal("completed with non-empty detail accepted")
	}
	erroredNoDetail := Output{Status: OutputErrored, Source: fullRef, Detail: ""}
	if _, err := NewOutput(erroredNoDetail); !errors.Is(err, ErrMissingField) {
		t.Fatalf("errored without detail error = %v, want ErrMissingField", err)
	}

	msgWithCalls := assistantMsg(t, func(m *Message) {
		c, _ := NewToolCall(ToolCall{ID: "c1", Name: "f"})
		m.ToolCalls = []ToolCall{c}
	})
	interruptedWithCalls := Output{Status: OutputInterrupted, Source: fullRef, Message: &msgWithCalls, Detail: "agent interrupted"}
	if _, err := NewOutput(interruptedWithCalls); err == nil {
		t.Fatal("interrupted output with tool calls accepted")
	}

	userRoleMsg := mustMessage(t, Message{Role: RoleUser})
	wrongRoleOut := Output{Status: OutputCompleted, Source: fullRef, Message: &userRoleMsg, Detail: ""}
	if _, err := NewOutput(wrongRoleOut); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("output message with non-assistant role error = %v, want ErrInvalidRole", err)
	}

	var zeroUsage Usage
	outWithZeroUsage, err := NewOutput(Output{Status: OutputCompleted, Source: fullRef, Message: &textMsg, Usage: &zeroUsage})
	if err != nil || outWithZeroUsage.Usage == nil {
		t.Fatalf("present all-zero usage must be retained (known zero): %#v (err %v)", outWithZeroUsage, err)
	}

	outNoUsage := Output{Status: OutputCompleted, Source: fullRef, Message: &textMsg}
	gotOut, _ := NewOutput(outNoUsage)
	if gotOut.Usage != nil {
		t.Fatalf("absent usage must stay absent (unknown/not reported): %#v", gotOut.Usage)
	}

	negIn := Usage{InputTokens: -1, CachedInputTokens: 0, OutputTokens: -2}
	gotNeg, err := NewOutput(Output{Status: OutputErrored, Source: fullRef, Detail: "d", Usage: &negIn})
	if err != nil || gotNeg.Usage == nil || *gotNeg.Usage != negIn {
		t.Fatalf("signed int usage not preserved verbatim: %#v (err %v)", gotNeg.Usage, err)
	}

	srcRaw := json.RawMessage(`"thinking"`)
	extraMsgSrc := assistantMsg(t, func(m *Message) { m.Extra = Extra{"reasoning_content": srcRaw} })
	gotExtraOut, err := NewOutput(Output{Status: OutputErrored, Source: fullRef, Message: &extraMsgSrc, Detail: "d"})
	if err != nil || gotExtraOut.Message == nil {
		t.Fatalf("NewOutput returned error or dropped message: %v", err)
	}
	srcRaw[1] = 'X' // corrupt caller bytes after construction.
	stored := string(gotExtraOut.Message.Extra["reasoning_content"])
	if len(stored) < 2 || stored[:2] != `"t` {
		t.Fatalf("output message extra not deep copied at accepting boundary: %s", gotExtraOut.Message.Extra["reasoning_content"])
	}

}

// TestNewOutputAssistantPayloadDefinition pins what counts as payload and what does not.
func TestNewOutputAssistantPayloadDefinition(t *testing.T) {
	msgText := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartText, Text: "x"}} })
	msgImage := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartImageURL, URL: "https://example.com/a.png"}} })
	msgOpaque := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartOpaque, OpaqueWireType: "thinking"}} })
	extraOnly := Extra{"k": json.RawMessage(`"v"`)}
	partWithExtra := ContentPart{Kind: PartText, Extra: extraOnly}
	msgExtraOnly := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{partWithExtra} })

	valid := []struct {
		name string
		out  Output
	}{
		{name: "non-empty text part", out: Output{Status: OutputCompleted, Source: fullRef, Message: &msgText}},
		{name: "image url only counts as payload", out: Output{Status: OutputCompleted, Source: fullRef, Message: &msgImage}},
		{name: "opaque wire type alone counts as payload", out: Output{Status: OutputCompleted, Source: fullRef, Message: &msgOpaque}},
		{name: "part with only a finalized non-null extra counts as payload", out: Output{Status: OutputCompleted, Source: fullRef, Message: &msgExtraOnly}},
	}
	for _, tc := range valid {
		t.Run(tc.name+" accepts", func(t *testing.T) {
			if _, err := NewOutput(tc.out); err != nil {
				t.Fatalf("NewOutput returned error: %v", err)
			}
		})
	}

	refusalOnly, _ := NewMessage(Message{Role: RoleAssistant, Source: fullRef, Refusal: "no"})
	if _, err := NewOutput(Output{Status: OutputCompleted, Source: fullRef, Message: &refusalOnly}); err != nil {
		t.Fatalf("non-empty refusal must count as payload: %v", err)
	}

	msgEmptyPart := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{{Kind: PartText}} })
	nullExtraOnly := Extra{"n": json.RawMessage(`null`)}
	partNullExtra := ContentPart{Kind: PartText, Extra: nullExtraOnly}
	msgNullExtraPart := assistantMsg(t, func(m *Message) { m.Content = []ContentPart{partNullExtra} })
	msgNameOnly := assistantMsg(t, func(m *Message) { m.Name = "worker" })

	noPayloadCases := []struct {
		name string
		msg  *Message
	}{
		{name: "empty text part with no extras is not a payload", msg: &msgEmptyPart},
		{name: "part whose only extra finalizes to null is not a payload", msg: &msgNullExtraPart},
		{name: "name does not count as payload", msg: &msgNameOnly},
	}
	for _, tc := range noPayloadCases {
		t.Run(tc.name+" rejects completed output", func(t *testing.T) {
			if _, err := NewOutput(Output{Status: OutputCompleted, Source: fullRef, Message: tc.msg}); err == nil {
				t.Fatal("completed output without eligible payload accepted")
			}
		})
	}

	bareAssistant := assistantMsg(t, nil)
	if _, err := NewOutput(Output{Status: OutputCompleted, Source: fullRef, Message: &bareAssistant}); err == nil {
		t.Fatal("bare assistant message (no parts/refusal/calls/extras) accepted as completed payload")
	}
}

// TestNewRequest pins the logical request contract: owned messages + tools only.
func TestNewRequestValidAndOwnership(t *testing.T) {
	msgs := []Message{mustMessage(t, Message{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "hi"}}})}
	d1, _ := NewToolDefinition(ToolDefinition{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)})
	defs := []ToolDefinition{d1}

	reqIn := Request{Messages: msgs, Tools: defs}
	out, err := NewRequest(reqIn)
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].TextContent() != "hi" {
		t.Fatalf("messages not preserved: %#v", out.Messages)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "read_file" {
		t.Fatalf("tools not preserved: %#v", out.Tools)
	}

	msgs[0].Content = nil // corrupt caller slices after construction.
	d1.Parameters = json.RawMessage(`{}`)
	if len(out.Messages[0].Content) != 1 || out.Messages[0].TextContent() != "hi" {
		t.Fatalf("owned request messages must survive input mutation: %#v", out.Messages)
	}

	got, err := NewRequest(Request{})
	if err != nil || len(got.Messages) != 0 || len(got.Tools) != 0 {
		t.Fatalf("empty request = %#v (err %v), want no messages or tools", got, err)
	}

	defParams := json.RawMessage(`{"type":"object"}`)
	dIn := ToolDefinition{Name: "x", Parameters: defParams}
	out3, err := NewRequest(Request{Tools: []ToolDefinition{dIn}})
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	defParams[1] = 'X' // corrupt caller bytes after construction.
	storedParams := string(out3.Tools[0].Parameters)
	if len(storedParams) == 0 || storedParams[:2] != `{"` {
		t.Fatalf("request tool parameters not deep copied: %s", out3.Tools[0].Parameters)
	}

	badMsg := Request{Messages: []Message{{Role: RoleAssistant}}} // assistant without source.
	if _, err := NewRequest(badMsg); !errors.Is(err, ErrMissingSource) {
		t.Fatalf("request with invalid embedded message error = %v, want ErrMissingSource", err)
	}

	if _, err := NewRequest(Request{Tools: []ToolDefinition{{Name: "", Parameters: json.RawMessage(`[1]`)}}}); !errors.Is(err, ErrMissingField) && !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("request with invalid definition error = %v", err)
	}

	dupA := ToolDefinition{Name: "t", Parameters: json.RawMessage(`{}`)}
	if _, err := NewRequest(Request{Tools: []ToolDefinition{dupA, dupA}}); !errors.Is(err, ErrDuplicateToolName) {
		t.Fatalf("duplicate tool name error = %v, want ErrDuplicateToolName", err)
	}
}

// TestNewRequestNeverRetainsZeroLengthSliceBacking pins the ownership rule for request/message slices through their shared clone path (the one Encode re-runs at its trust boundary): a zero-length slice accepted with spare capacity must not retain caller-owned backing storage, so later appends on either side cannot see into each other. Representation is deliberately unpinned — nil or an independently allocated empty both satisfy the alias-free rule this row asserts behaviorally only.
func TestNewRequestNeverRetainsZeroLengthSliceBacking(t *testing.T) {
	// zero length with real spare capacity on caller-owned storage: the exact retained-capacity shape under test. A literal empty slice would not exercise it (its backing carries no slots either side's appends could share).
	zeroContent := make([]ContentPart, 0, 4)                                                 // four slots so both sides' two-part appends below fit inside one shared array if retention were buggy... (a smaller capacity would force an early reallocation and mask the aliasing this row exists to catch).
	fullCallsBase := []ToolCall{{ID: "call-a", Name: "alpha"}, {ID: "call-b", Name: "beta"}} // two seed calls so truncating below leaves at least one shared slot of headroom for both sides' appends.
	zeroCalls := fullCallsBase[:0]                                                           // zero length with that capacity now riding along on the caller's own array.

	inMsg, err := NewMessage(Message{Role: RoleAssistant, Source: fullRef, Content: zeroContent, ToolCalls: zeroCalls}) // valid assistant shape so the fixture reaches the clone path exactly as Encode's boundary would.
	if err != nil {
		t.Fatalf("NewRequest(fixture) rejected a valid message with spare-capacity empty slices: %v", err)
	}

	out, outErr := NewRequest(Request{Messages: []Message{inMsg}}) // same shared clone path the encoder invokes per call — this row judges ITS retention behavior.
	if outErr != nil {
		t.Fatalf("NewRequest returned error for valid spare-capacity empty slices: %v", outErr)
	}

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
	if _, err := NewOutput(outIn); !errors.Is(err, ErrSourceMismatch) {
		t.Fatalf("rendering-colliding identities accepted; error = %v (want ErrSourceMismatch)", err)
	}

	slashyRef := ModelRef{Provider: "prov/er", Model: "model/x"} // complete identity with slashes in both fields.
	msgIn2 := assistantMsg(t, func(m *Message) { m.Source = slashyRef; m.Content = []ContentPart{{Kind: PartText, Text: "x"}} })
	outIn2 := Output{Status: OutputCompleted, Source: slashyRef, Message: &msgIn2}
	if _, err := NewOutput(outIn2); err != nil {
		t.Fatalf("field-identical identities rejected: %v", err)
	}
}
