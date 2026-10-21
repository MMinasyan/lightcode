package model

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestNewStreamDeltaDeepCopyAndPositions pins the StreamDelta accepting rules
// through both entry points: one inventory of positioned fragments, closed
// content kinds and Extra scopes with their original expected verdicts, plus
// the constructor's deep-copy/field-preservation probes and the validator's
// non-mutation observation.
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
	testassert.NoError(t, err)
	testassert.Require(t, out.HasChoice && out.Role == "developer" && out.RefusalFragment == "no " && len(out.ContentFragments) == 2 && len(out.ToolFragments) == 1, "delta not preserved: %#v", out)
	contentSrc[0] = 'X' // corrupt caller bytes after construction.
	stored := string(out.ContentFragments[0].Extra["k"])
	testassert.Require(t, stored != "" && stored[0] == '"', "content fragment extra not deep copied at accepting boundary: %s", out.ContentFragments[0].Extra["k"])

	argSrc := json.RawMessage(`{"p":1}`)
	in2 := StreamDelta{ToolFragments: []ToolCallFragment{{ID: "c", Name: "f", ArgumentFragment: string(argSrc)}}}
	out2, err := NewStreamDelta(in2)
	testassert.NoError(t, err)
	argSrc[0] = 'X' // corrupt caller bytes after construction.
	gotArg := out2.ToolFragments[0].ArgumentFragment
	testassert.Require(t, gotArg[:1] != "X", "tool fragment argument shares bytes with input: %s", gotArg)

	usageIn := &Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 4}
	out3, err := NewStreamDelta(StreamDelta{HasChoice: true, Usage: usageIn})
	testassert.Require(t, err == nil && out3.Usage != nil && *out3.Usage == *usageIn, "present usage not preserved: %#v (err %v)", out3.Usage, err)
	out4, _ := NewStreamDelta(StreamDelta{HasChoice: true})
	testassert.Require(t, out4.Usage == nil, "%v", "absent delta usage must stay absent")

	neg := -1
	negTwo := -2
	rows := []struct {
		name      string
		delta     StreamDelta
		wantErr   bool
		wantIsAny []error
	}{
		{"rich choice-bearing", richDeltaFixture(), false, nil},
		{"empty delta", StreamDelta{HasChoice: true}, false, nil},
		{"usage only", StreamDelta{HasChoice: true, Usage: &Usage{InputTokens: 1}}, false, nil},
		{"nil tool pointer position", StreamDelta{ToolFragments: []ToolCallFragment{{ID: "c", Name: "f"}}}, false, nil},
		{"transient empty text at position zero", StreamDelta{ContentFragments: []ContentFragment{{Position: 0, Kind: PartText}}}, false, nil},
		{"transient image URL later", StreamDelta{ContentFragments: []ContentFragment{{Position: 1, Kind: PartImageURL}}}, false, nil},
		{"transient opaque without wire type", StreamDelta{ContentFragments: []ContentFragment{{Position: 2, Kind: PartOpaque}}}, false, nil},
		{"negative content position", StreamDelta{HasChoice: true, ContentFragments: []ContentFragment{{Position: -1, Kind: PartText, Text: "x"}}}, true, nil},
		{"negative content fragment position", StreamDelta{ContentFragments: []ContentFragment{{Position: -1, Kind: PartText}}}, true, []error{ErrInvalidPosition}},
		{"cross-kind fragment", StreamDelta{HasChoice: true, ContentFragments: []ContentFragment{{Position: 0, Kind: PartText, Text: "x", URL: "u"}}}, true, nil},
		{"unknown kind", StreamDelta{HasChoice: true, ContentFragments: []ContentFragment{{Position: 0, Kind: PartKind("audio"), Text: "x"}}}, true, []error{ErrForbiddenField, ErrMissingField}},
		{"empty content kind", StreamDelta{ContentFragments: []ContentFragment{{Position: 0, Text: "x"}}}, true, []error{ErrForbiddenField, ErrMissingField}},
		{"text fragment carrying a URL", StreamDelta{ContentFragments: []ContentFragment{{Position: 0, Kind: PartText, Text: "a", URL: "b"}}}, true, []error{ErrForbiddenField, ErrMissingField}},
		{"image fragment carrying an opaque wire type", StreamDelta{ContentFragments: []ContentFragment{{Position: 1, Kind: PartImageURL, OpaqueWireType: "thinking"}}}, true, []error{ErrForbiddenField, ErrMissingField}},
		{"negative tool pointer position", StreamDelta{HasChoice: true, ToolFragments: []ToolCallFragment{{ID: "c", Name: "f", Position: &neg}}}, true, nil},
		{"negative tool fragment pointer position", StreamDelta{ToolFragments: []ToolCallFragment{{ID: "c", Name: "f", Position: &negTwo}}}, true, []error{ErrInvalidPosition}},
		{"malformed tool extra", StreamDelta{HasChoice: true, ToolFragments: []ToolCallFragment{{ID: "c", Name: "f", Extra: Extra{"k": json.RawMessage(`{oops`)}}}}, true, nil},
		{"malformed message extra", StreamDelta{HasChoice: true, MessageExtra: Extra{"m": json.RawMessage(`[1`)}}, true, nil},
		{"valid-looking text with an invalid sibling", StreamDelta{HasChoice: true, RefusalFragment: "no", ContentFragments: []ContentFragment{{Position: -1, Kind: PartText, Text: "x"}}}, true, nil},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got, newErr := NewStreamDelta(row.delta)
			validateErr := ValidateStreamDelta(row.delta)
			requireEqualConstructorVerdict(t, row.name, newErr, validateErr)
			testassert.Require(t, (newErr != nil) == row.wantErr, "fixture %s: error = %v, wantErr = %v", row.name, newErr, row.wantErr)
			if len(row.wantIsAny) > 0 {
				testassert.Require(t, matchesAnySentinel(newErr, row.wantIsAny...) && matchesAnySentinel(validateErr, row.wantIsAny...), "fixture %s: errors = %v / %v, want one of %v", row.name, newErr, validateErr, row.wantIsAny)
			}
			if !row.wantErr {
				testassert.Require(t, reflect.DeepEqual(got, row.delta), "delta not preserved: %#v", got)
			}
		})
	}

	// The validator mutates nothing, nested fragments and extras included.
	validated := richDeltaFixture()
	before := richDeltaFixture()
	{
		err := ValidateStreamDelta(validated)
		testassert.NoError(t, err)
	}
	testassert.Require(t, reflect.DeepEqual(validated, before), "%v", "ValidateStreamDelta mutated its input")

	var fake streamFakeImpl
	out5, _ := NewStreamDelta(StreamDelta{HasChoice: true})
	out5.Usage = out3.Usage // value semantics on the returned delta.
	testassert.Require(t, out5.Usage != nil, "%v", "delta usage pointer lost")
	_ = fake
}

type streamFakeImpl struct{}

func (streamFakeImpl) Recv() (StreamDelta, error) { return StreamDelta{}, io.EOF }
func (streamFakeImpl) Close() error               { return nil }

func TestNewStreamDeltaNeverRetainsZeroLengthFragmentBacking(t *testing.T) {
	zeroContent := make([]ContentFragment, 0, 1)
	zeroTools := make([]ToolCallFragment, 0, 1)
	out, err := NewStreamDelta(StreamDelta{HasChoice: true, ContentFragments: zeroContent, ToolFragments: zeroTools})
	testassert.NoError(t, err)

	out.ContentFragments = append(out.ContentFragments, ContentFragment{Kind: PartText, Text: "out"})
	zeroContent = append(zeroContent, ContentFragment{Kind: PartText, Text: "in"})
	testassert.Require(t, out.ContentFragments[0].Text == "out" && zeroContent[0].Text == "in", "content slices share backing: out=%#v in=%#v", out.ContentFragments, zeroContent)

	out.ToolFragments = append(out.ToolFragments, ToolCallFragment{ID: "out", Name: "out"})
	zeroTools = append(zeroTools, ToolCallFragment{ID: "in", Name: "in"})
	testassert.Require(t, out.ToolFragments[0].ID == "out" && zeroTools[0].ID == "in", "tool slices share backing: out=%#v in=%#v", out.ToolFragments, zeroTools)
}

// TestStreamInterfaceIsSatisfied pins the exact public stream contract shape.
func TestStreamInterfaceIsSatisfied(t *testing.T) {
	var s Stream = streamFakeImpl{}
	{
		_, err := s.Recv()
		testassert.Require(t, errors.Is(err, io.EOF), "Recv error = %v, want io.EOF", err)
	}
	{
		err := s.Close()
		testassert.NoError(t, err)
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
	testassert.NoError(t, err)
	testassert.Require(t, out.Kind == in.Kind && out.Message == in.Message && out.Target == fullRef && out.Field == "reasoning_content" && out.MessageIndex == 1, "warning not preserved: %#v", out)

	neg := ProtocolWarning{Kind: "k", Message: "m"} // zero index is valid (zero-based).
	{
		_, err := NewProtocolWarning(neg)
		testassert.NoError(t, err)
	}

	badIdx := -1
	negWarn := ProtocolWarning{Kind: "k", Message: "m", MessageIndex: badIdx}
	{
		_, err := NewProtocolWarning(negWarn)
		testassert.Require(t, errors.Is(err, ErrInvalidPosition), "negative message index error = %v, want ErrInvalidPosition", err)
	}

	var zeroTarget ProtocolWarning // target identity is diagnostic context; no nonzero requirement.
	outZero, err := NewProtocolWarning(zeroTarget)
	testassert.Require(t, err == nil && outZero.Target.IsZero(), "zero-target warning = %#v (err %v), want accepted with the zero target kept", outZero, err)

	var p ProtocolWarning // warnings carry exactly their contract fields.
	got := structFieldNames(p)
	for _, name := range []string{"Kind", "Message", "Target", "Field", "MessageIndex"} {
		testassert.Require(t, got[name], "ProtocolWarning missing field %s: %#v", name, got)
		delete(got, name)
	}
	for extra := range got {
		t.Fatalf("ProtocolWarning has non-contract field %q", extra)
	}
}
