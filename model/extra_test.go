package model

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestExtraAccumulatorConcatenatesStrings(t *testing.T) {
	acc := NewExtraAccumulator()
	{
		err := acc.Add("reasoning_content", json.RawMessage(`"hello "`))
		testassert.NoError(t, err)
	}
	{
		err := acc.Add("reasoning_content", json.RawMessage(`"world"`))
		testassert.NoError(t, err)
	}
	{
		got := string(acc.Finalize()["reasoning_content"])
		testassert.Equal(t, got, `"hello world"`)
	}
}

func TestExtraAccumulatorAppendsArrays(t *testing.T) {
	acc := NewExtraAccumulator()
	{
		err := acc.Add("reasoning_details", json.RawMessage(`[{"type":"text","text":"a"}]`))
		testassert.NoError(t, err)
	}
	{
		err := acc.Add("reasoning_details", json.RawMessage(`[{"type":"encrypted","data":"b"}]`))
		testassert.NoError(t, err)
	}
	{
		got := string(acc.Finalize()["reasoning_details"])
		testassert.Equal(t, got, `[{"type":"text","text":"a"},{"type":"encrypted","data":"b"}]`)
	}
}

func TestExtraAccumulatorObjectLastWriteWins(t *testing.T) {
	acc := NewExtraAccumulator()
	{
		err := acc.Add("reasoning", json.RawMessage(`{"first":true}`))
		testassert.NoError(t, err)
	}
	{
		err := acc.Add("reasoning", json.RawMessage(`{"second":true}`))
		testassert.NoError(t, err)
	}
	{
		got := string(acc.Finalize()["reasoning"])
		testassert.Equal(t, got, `{"second":true}`)
	}
}

func TestExtraAccumulatorNumberAndBooleanAreOtherKind(t *testing.T) {
	pairs := [][2]string{{`1`, `2`}, {"true", "false"}} // numbers and booleans share the other kind; same-kind replaces never error.
	for _, pair := range pairs {
		t.Run(pair[0]+" to "+pair[1], func(t *testing.T) {
			acc := NewExtraAccumulator()
			{
				err := acc.Add("n", json.RawMessage(pair[0]))
				testassert.Require(t, err == nil, "Add(%s) returned error: %v", pair[0], err)
			}
			{
				err := acc.Add("n", json.RawMessage(pair[1]))
				testassert.Require(t, err == nil, "%s to %s must replace within the other kind without error, got %v", pair[0], pair[1], err)
			}
			{
				got := string(acc.Finalize()["n"])
				testassert.Equal(t, got, pair[1])
			}
		})
	}

	mixed := NewExtraAccumulator() // number to boolean is still the same other kind: replace without error.
	{
		err := mixed.Add("m", json.RawMessage(`5`))
		testassert.NoError(t, err)
	}
	if err := mixed.Add("m", json.RawMessage(`true`)); err != nil {
		t.Fatalf("number to boolean must replace within the other kind without error, got %v", err)
	} else if got := string(mixed.Finalize()["m"]); got != `true` {
		t.Fatalf("m = %s, want latest value kept", got)
	}

	toOther := NewExtraAccumulator() // string to number crosses kinds: error plus keep latest.
	{
		err := toOther.Add("o", json.RawMessage(`"text"`))
		testassert.NoError(t, err)
	}
	if err := toOther.Add("o", json.RawMessage(`7`)); err == nil {
		t.Fatal("string to number must return a kind-change accumulator error")
	} else if got := string(toOther.Finalize()["o"]); got != `7` {
		t.Fatalf("o = %s, want latest value kept after kind change", got)
	}

	fromNull := NewExtraAccumulator()
	{
		err := fromNull.Add("z", json.RawMessage(`null`))
		testassert.NoError(t, err)
	}
	if err := fromNull.Add("z", json.RawMessage(`"s"`)); err == nil {
		t.Fatal("null to string must return a kind-change accumulator error")
	} else if _, ok := fromNull.Finalize()["z"]; !ok {
		t.Fatalf("latest value after null->string change not kept: %#v", fromNull.Finalize())
	}
	{
		got, present := fromNull.Finalize()["z"]
		testassert.Require(t, present && string(got) == `"s"`, "finalized z = %q (present=%v), want \"s\"", got, present)
	}
}

func TestExtraAccumulatorKindChangeReturnsErrorAndKeepsLastValue(t *testing.T) {
	acc := NewExtraAccumulator()
	{
		err := acc.Add("reasoning", json.RawMessage(`"text"`))
		testassert.NoError(t, err)
	}
	{
		err := acc.Add("reasoning", json.RawMessage(`[{"type":"text"}]`))
		testassert.Require(t, err != nil, "%v", "Add returned nil error for kind change")
	}
	{
		got := string(acc.Finalize()["reasoning"])
		testassert.Equal(t, got, `[{"type":"text"}]`)
	}
}

func TestExtraAccumulatorOmitsNullValuesOnFinalize(t *testing.T) {
	acc := NewExtraAccumulator()
	{
		err := acc.Add("drop", json.RawMessage(`null`))
		testassert.NoError(t, err)
	}
	{
		err := acc.Add("keep", json.RawMessage(`""`))
		testassert.NoError(t, err)
	}
	out := acc.Finalize()
	{
		_, ok := out["drop"]
		testassert.Require(t, !ok, "null value was kept: %#v", out)
	}
	{
		got := string(out["keep"])
		testassert.Equal(t, got, `""`)
	}

	replacedNull := NewExtraAccumulator()
	{
		err := replacedNull.Add("k", json.RawMessage(`"a"`))
		testassert.NoError(t, err)
	}
	{
		err := replacedNull.Add("k", json.RawMessage(`null`))
		testassert.Require(t, err != nil, "%v", "string to null must return a kind-change accumulator error")
	}
	if _, ok := replacedNull.Finalize()["k"]; ok {
		t.Fatalf("latest value is null and must be omitted: %#v", replacedNull.Finalize())
	}
}

func TestExtraAccumulatorIgnoresEmptyKeyAndValue(t *testing.T) {
	acc := NewExtraAccumulator()
	{
		err := acc.Add("", json.RawMessage(`"x"`))
		testassert.NoError(t, err)
	}
	{
		err := acc.Add("k", json.RawMessage(nil))
		testassert.NoError(t, err)
	}
	{
		got := acc.Finalize()
		testassert.Require(t, len(got) == 0, "Finalize = %#v, want empty", got)
	}
}

func TestExtraCloneDeepCopiesValues(t *testing.T) {
	srcRaw := json.RawMessage(`"hello"`)
	in := Extra{"a": srcRaw} // direct assignment shares bytes by caller choice; Clone is the accepting boundary.
	out := in.Clone()
	testassert.Require(t, string(out["a"]) == `"hello"`, "clone a = %s", out["a"])
	testassert.Require(t, len(srcRaw) <= 0 || &out["a"][0] != &srcRaw[0], "%v", "Clone shares bytes with its input")
	out["a"][1] = 'H' // corrupt the clone's retained copy.
	{
		got := string(in["a"])
		testassert.Require(t, got == `"hello"`, "original extra mutated through a Clone result: %s", in["a"])
	}

	var nilExtra Extra
	{
		got := nilExtra.Clone()
		testassert.Require(t, got == nil, "nil Clone = %#v, want nil", got)
	}
	testassert.Require(t, len(out) == 1, "clone of one-entry extra wrong: %#v", out)
}

func TestExtraFinalizeDropsNullsAndDeepCopies(t *testing.T) {
	extra := Extra{
		"drop": json.RawMessage(`null`),
		"keep": json.RawMessage(`""`),
	}
	clean := extra.Finalize()
	{
		_, ok := clean["drop"]
		testassert.Require(t, !ok, "null value was kept: %#v", clean)
	}
	{
		got := string(clean["keep"])
		testassert.Equal(t, got, `""`)
	}

	srcRaw := json.RawMessage(`"x"`)
	withNull := Extra{"n": json.RawMessage(`null`), "k": srcRaw}
	out := withNull.Finalize()
	testassert.Require(t, out["k"] != nil && &out["k"][0] != &srcRaw[0], "%v", "finalize did not deep copy retained values")

	var zero Extra
	{
		z := zero.Finalize()
		testassert.Require(t, z == nil, "zero Finalize = %#v, want nil", z)
	}
	allNull := Extra{"only": json.RawMessage(`null`)}
	if len(allNull.Finalize()) != 0 {
		t.Fatalf("all-null finalize not empty: %#v", allNull.Finalize())
	}
}

func TestCloneRawCopiesBytes(t *testing.T) {
	src := json.RawMessage(`"abc"`)
	dst := CloneRaw(src)
	testassert.Require(t, string(dst) == `"abc"` && len(dst) != 0 && &dst[0] != &src[0], "CloneRaw = %s, want independent copy", dst)
	var nilSrc json.RawMessage
	{
		got := CloneRaw(nilSrc)
		testassert.Require(t, len(got) == 0, "CloneRaw(nil) = %#v, want empty", got)
	}
	dst[1] = 'x'
	testassert.Require(t, string(src) == `"abc"`, "%v", "mutating the copy changed the source")
}

func TestExtraAccumulatorAddDeepCopiesInput(t *testing.T) {
	srcRaw := json.RawMessage(`"first"`)
	acc := NewExtraAccumulator()
	{
		err := acc.Add("k", srcRaw)
		testassert.NoError(t, err)
	}
	srcRaw[1] = 'X'
	got := acc.Finalize()["k"]
	testassert.Require(t, string(got)[:1] != `"X` && &got[0] != &srcRaw[0], "accumulator stored caller bytes: %s", got)

	out := acc.Finalize()
	src2 := json.RawMessage(`"second"`)
	in := Extra{"k": src2}
	clonedExtra := in.Clone()
	testassert.Require(t, &clonedExtra["k"][0] != &src2[0], "%v", "Clone shared bytes with input")
	_ = out
}

// TestExtraHasValuesMatchesFinalizedRetention compares the scalar query with the finalization view it stands for: HasValues must equal "Finalize retains at least one value" over null, non-null, empty and malformed raw values, for the plain map and the accumulator alike.
func TestExtraHasValuesMatchesFinalizedRetention(t *testing.T) {
	extras := []Extra{
		nil,
		{},
		{"only": json.RawMessage(`null`)},
		{"padded": json.RawMessage(`  null  `)},
		{"mixed": json.RawMessage(`null`), "keep": json.RawMessage(`1`)},
		{"empty": json.RawMessage(``)},
		{"malformed": json.RawMessage(`{`)},
		{"zero": json.RawMessage(`0`)},
		{"false": json.RawMessage(`false`)},
		{"emptystr": json.RawMessage(`""`)},
		{"array": json.RawMessage(`[]`)},
		{"object": json.RawMessage(`{}`)},
	}
	for _, e := range extras {
		{
			got, want := e.HasValues(), len(e.Finalize()) > 0
			testassert.Require(t, got == want, "Extra %#v: HasValues = %v, want %v", map[string]json.RawMessage(e), got, want)
		}
	}

	var nilAcc *ExtraAccumulator
	testassert.Require(t, !nilAcc.HasValues(), "%v", "nil accumulator reports values")

	type add struct {
		key   string
		value json.RawMessage
	}
	scripts := [][]add{
		{},
		{{"k", json.RawMessage(`null`)}},
		{{"k", json.RawMessage(`null`)}, {"k", json.RawMessage(`5`)}}, // kind change keeps the latest value.
		{{"k", json.RawMessage(`5`)}, {"k", json.RawMessage(`null`)}},
		{{"k", json.RawMessage(`"a"`)}, {"k", json.RawMessage(`"b"`)}},
		{{"", json.RawMessage(`1`)}, {"k", json.RawMessage(``)}}, // ignored key and value leave nothing retained.
		{{"a", json.RawMessage(`null`)}, {"b", json.RawMessage(`false`)}},
	}
	for _, script := range scripts {
		acc := NewExtraAccumulator()
		for _, step := range script {
			_ = acc.Add(step.key, step.value) // accumulation errors keep the latest value; the query sees the same retained state either way.
		}
		{
			got, want := acc.HasValues(), len(acc.Finalize()) > 0
			testassert.Require(t, got == want, "accumulator %#v: HasValues = %v, want %v", script, got, want)
		}
	}
}
