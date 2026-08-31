package model

import (
	"encoding/json"
	"errors"
	"io"
	"testing"
)

// TestNewStreamDeltaDeepCopyAndPositions pins delta construction rules.
func TestNewStreamDeltaDeepCopyAndPositions(t *testing.T) {
	contentSrc := json.RawMessage(`"x"`)
	pos0 := 1 // non-nil pointer position for the tool fragment.
	in := StreamDelta{
		HasChoice:       true,
		Role:            "developer", // raw wire role passes through at delta level; normalization is parser work.
		RefusalFragment: "no ",
		FinishReason:    "", // empty means absent.
		ContentFragments: []ContentFragment{
			{Position: 0, Kind: PartText, Text: "a", Extra: Extra{"k": contentSrc}},
			{Position: 1, Kind: PartImageURL, URL: "https://example.com/a.png"},
		},
		ToolFragments: []ToolCallFragment{{ID: "c1", WireType: "", Name: "f", ArgumentFragment: `{"p":`, Position: &pos0}},
	}

	out, err := NewStreamDelta(in)
	if err != nil {
		t.Fatalf("NewStreamDelta returned error: %v", err)
	}
	if !out.HasChoice || out.Role != "developer" || out.RefusalFragment != "no " || len(out.ContentFragments) != 2 || len(out.ToolFragments) != 1 {
		t.Fatalf("delta not preserved: %#v", out)
	}
	contentSrc[0] = 'X' // corrupt caller bytes after construction.
	stored := string(out.ContentFragments[0].Extra["k"])
	if stored == "" || stored[0] != '"' {
		t.Fatalf("content fragment extra not deep copied at accepting boundary: %s", out.ContentFragments[0].Extra["k"])
	}

	argSrc := json.RawMessage(`{"p":1}`)
	in2 := StreamDelta{ToolFragments: []ToolCallFragment{{ID: "c", Name: "f", ArgumentFragment: string(argSrc)}}}
	out2, err := NewStreamDelta(in2)
	if err != nil {
		t.Fatalf("NewStreamDelta returned error: %v", err)
	}
	argSrc[0] = 'X' // corrupt caller bytes after construction.
	gotArg := out2.ToolFragments[0].ArgumentFragment
	if gotArg[:1] == "X" {
		t.Fatalf("tool fragment argument shares bytes with input: %s", gotArg)
	}

	usageIn := &Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 4}
	out3, err := NewStreamDelta(StreamDelta{HasChoice: true, Usage: usageIn})
	if err != nil || out3.Usage == nil || *out3.Usage != *usageIn {
		t.Fatalf("present usage not preserved: %#v (err %v)", out3.Usage, err)
	}
	out4, _ := NewStreamDelta(StreamDelta{HasChoice: true})
	if out4.Usage != nil {
		t.Fatal("absent delta usage must stay absent")
	}

	casesErr := []struct {
		name string
		d    StreamDelta
	}{
		{name: "negative content fragment position", d: StreamDelta{ContentFragments: []ContentFragment{{Position: -1, Kind: PartText}}}},
		{name: "negative tool fragment pointer position", d: func() StreamDelta {
			p := -2
			return StreamDelta{ToolFragments: []ToolCallFragment{{ID: "c", Name: "f", Position: &p}}}
		}()},
	}
	for _, tc := range casesErr {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewStreamDelta(tc.d); !errors.Is(err, ErrInvalidPosition) {
				t.Fatalf("error = %v, want ErrInvalidPosition", err)
			}
		})
	}

	nilPos := StreamDelta{ToolFragments: []ToolCallFragment{{ID: "c", Name: "f"}}} // omitted position is optional.
	if _, err := NewStreamDelta(nilPos); err != nil {
		t.Fatalf("omitted tool fragment position must be accepted, got %v", err)
	}

	var fake streamFakeImpl
	out5, _ := NewStreamDelta(StreamDelta{HasChoice: true})
	out5.Usage = out3.Usage // value semantics on the returned delta.
	if out5.Usage == nil {
		t.Fatal("delta usage pointer lost")
	}
	_ = fake
}

type streamFakeImpl struct{}

func (streamFakeImpl) Recv() (StreamDelta, error) { return StreamDelta{}, io.EOF }
func (streamFakeImpl) Close() error               { return nil }

// TestStreamInterfaceIsSatisfied pins the exact public stream contract shape.
func TestStreamInterfaceIsSatisfied(t *testing.T) {
	var s Stream = streamFakeImpl{}
	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv error = %v, want io.EOF", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var asAny any = StreamDelta{}
	switch asAny.(type) {
	case StreamDelta:
	default:
		t.Fatal("StreamDelta value type must be addressable through the interface set")
	}
}

// TestNewProtocolWarning pins warning construction and position validation.
func TestNewProtocolWarning(t *testing.T) {
	in := ProtocolWarning{Kind: "must_preserve", Message: "field x missing on assistant message 2", Target: fullRef, Field: "reasoning_content", MessageIndex: 1}
	out, err := NewProtocolWarning(in)
	if err != nil {
		t.Fatalf("NewProtocolWarning returned error: %v", err)
	}
	if out.Kind != in.Kind || out.Message != in.Message || out.Target != fullRef || out.Field != "reasoning_content" || out.MessageIndex != 1 {
		t.Fatalf("warning not preserved: %#v", out)
	}

	neg := ProtocolWarning{Kind: "k", Message: "m"} // zero index is valid (zero-based).
	if _, err := NewProtocolWarning(neg); err != nil {
		t.Fatalf("zero message index rejected: %v", err)
	}

	badIdx := -1
	negWarn := ProtocolWarning{Kind: "k", Message: "m", MessageIndex: badIdx}
	if _, err := NewProtocolWarning(negWarn); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("negative message index error = %v, want ErrInvalidPosition", err)
	}

	var zeroTarget ProtocolWarning // target identity is diagnostic context; no nonzero requirement.
	outZero, err := NewProtocolWarning(zeroTarget)
	if err != nil || !outZero.Target.IsZero() {
		t.Fatalf("zero-target warning = %#v (err %v), want accepted with the zero target kept", outZero, err)
	}

	var p ProtocolWarning // warnings carry exactly their contract fields.
	got := structFieldNames(p)
	for _, name := range []string{"Kind", "Message", "Target", "Field", "MessageIndex"} {
		if !got[name] {
			t.Fatalf("ProtocolWarning missing field %s: %#v", name, got)
		}
		delete(got, name)
	}
	for extra := range got {
		t.Fatalf("ProtocolWarning has non-contract field %q", extra)
	}
}

// TestNewStreamDeltaContentFragmentValidation pins the closed content-kind and field-exclusivity contract at the stream-delta accepting boundary: fragments are validated like content values, while still permitting empty transient text/URL/opaque-type pieces (including a transiently absent opaque wire type — only canonical parts require it).
func TestNewStreamDeltaContentFragmentValidation(t *testing.T) {
	rejected := []struct {
		name string
		frag ContentFragment
	}{
		{name: "unknown content kind rejected", frag: ContentFragment{Position: 0, Kind: PartKind("audio")}},
		{name: "empty content kind rejected", frag: ContentFragment{Position: 0, Text: "x"}},
		{name: "text fragment carrying a URL rejected", frag: ContentFragment{Position: 0, Kind: PartText, Text: "a", URL: "b"}},
		{name: "image_url fragment carrying an opaque wire type rejected", frag: ContentFragment{Position: 1, Kind: PartImageURL, OpaqueWireType: "thinking"}},
	}
	for _, tc := range rejected {
		t.Run(tc.name+" rejects at the delta boundary", func(t *testing.T) {
			if _, err := NewStreamDelta(StreamDelta{ContentFragments: append([]ContentFragment{}, tc.frag)}); !errors.Is(err, ErrForbiddenField) && !errors.Is(err, ErrMissingField) {
				t.Fatalf("fragment %#v error = %v", tc.frag, err)
			}
		})
	}

	transient := []struct {
		name string
		frag ContentFragment
	}{
		{name: "empty text piece at position zero allowed transitively", frag: ContentFragment{Position: 0, Kind: PartText}},
		{name: "image URL may arrive in a later delta", frag: ContentFragment{Position: 1, Kind: PartImageURL}},
	}
	for _, tc := range transient {
		t.Run(tc.name+" accepted at the delta boundary", func(t *testing.T) {
			gotOut, err := NewStreamDelta(StreamDelta{ContentFragments: append([]ContentFragment{}, tc.frag)})
			if err != nil || len(gotOut.ContentFragments) != 1 || gotOut.ContentFragments[0].Kind != tc.frag.Kind || gotOut.ContentFragments[0].Position != tc.frag.Position {
				t.Fatalf("transient fragment %#v = %#v (err %v)", tc.frag, gotOut.ContentFragments, err)
			}
		})
	}

	opaqueTransient := ContentFragment{Position: 2, Kind: PartOpaque} // wire type absent for now.
	gotOpaq, e2 := NewStreamDelta(StreamDelta{ContentFragments: append([]ContentFragment{}, opaqueTransient)})
	if e2 != nil || gotOpaq.ContentFragments[0].Kind != PartOpaque {
		t.Fatalf("transiently empty opaque wire type must be permitted on fragments (err %v): %#v", e2, gotOpaq)
	}

	partLevelStillRequiresWireType := ContentPart{Kind: PartOpaque} // the SAME shape is invalid as a canonical part.
	if _, pErr := NewContentPart(partLevelStillRequiresWireType); !errors.Is(pErr, ErrMissingField) {
		t.Fatalf("canonical opaque part without wire type error = %v, want ErrMissingField (fragment allowance does not loosen parts)", pErr)
	}
}
