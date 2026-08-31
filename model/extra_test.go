package model

import (
	"encoding/json"
	"testing"
)

func TestExtraAccumulatorConcatenatesStrings(t *testing.T) {
	acc := NewExtraAccumulator()
	if err := acc.Add("reasoning_content", json.RawMessage(`"hello "`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if err := acc.Add("reasoning_content", json.RawMessage(`"world"`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if got := string(acc.Finalize()["reasoning_content"]); got != `"hello world"` {
		t.Fatalf("reasoning_content = %s", got)
	}
}

func TestExtraAccumulatorAppendsArrays(t *testing.T) {
	acc := NewExtraAccumulator()
	if err := acc.Add("reasoning_details", json.RawMessage(`[{"type":"text","text":"a"}]`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if err := acc.Add("reasoning_details", json.RawMessage(`[{"type":"encrypted","data":"b"}]`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if got := string(acc.Finalize()["reasoning_details"]); got != `[{"type":"text","text":"a"},{"type":"encrypted","data":"b"}]` {
		t.Fatalf("reasoning_details = %s", got)
	}
}

func TestExtraAccumulatorObjectLastWriteWins(t *testing.T) {
	acc := NewExtraAccumulator()
	if err := acc.Add("reasoning", json.RawMessage(`{"first":true}`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if err := acc.Add("reasoning", json.RawMessage(`{"second":true}`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if got := string(acc.Finalize()["reasoning"]); got != `{"second":true}` {
		t.Fatalf("reasoning = %s", got)
	}
}

func TestExtraAccumulatorNumberAndBooleanAreOtherKind(t *testing.T) {
	pairs := [][2]string{{`1`, `2`}, {"true", "false"}} // numbers and booleans share the other kind; same-kind replaces never error.
	for _, pair := range pairs {
		t.Run(pair[0]+" to "+pair[1], func(t *testing.T) {
			acc := NewExtraAccumulator()
			if err := acc.Add("n", json.RawMessage(pair[0])); err != nil {
				t.Fatalf("Add(%s) returned error: %v", pair[0], err)
			}
			if err := acc.Add("n", json.RawMessage(pair[1])); err != nil {
				t.Fatalf("%s to %s must replace within the other kind without error, got %v", pair[0], pair[1], err)
			}
			if got := string(acc.Finalize()["n"]); got != pair[1] {
				t.Fatalf("n = %s, want latest value replacing the previous same-kind one (%s)", got, pair[1])
			}
		})
	}

	mixed := NewExtraAccumulator() // number to boolean is still the same other kind: replace without error.
	if err := mixed.Add("m", json.RawMessage(`5`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if err := mixed.Add("m", json.RawMessage(`true`)); err != nil {
		t.Fatalf("number to boolean must replace within the other kind without error, got %v", err)
	} else if got := string(mixed.Finalize()["m"]); got != `true` {
		t.Fatalf("m = %s, want latest value kept", got)
	}

	toOther := NewExtraAccumulator() // string to number crosses kinds: error plus keep latest.
	if err := toOther.Add("o", json.RawMessage(`"text"`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if err := toOther.Add("o", json.RawMessage(`7`)); err == nil {
		t.Fatal("string to number must return a kind-change accumulator error")
	} else if got := string(toOther.Finalize()["o"]); got != `7` {
		t.Fatalf("o = %s, want latest value kept after kind change", got)
	}

	fromNull := NewExtraAccumulator()
	if err := fromNull.Add("z", json.RawMessage(`null`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if err := fromNull.Add("z", json.RawMessage(`"s"`)); err == nil {
		t.Fatal("null to string must return a kind-change accumulator error")
	} else if _, ok := fromNull.Finalize()["z"]; !ok {
		t.Fatalf("latest value after null->string change not kept: %#v", fromNull.Finalize())
	}
	if got, present := fromNull.Finalize()["z"]; !present || string(got) != `"s"` {
		t.Fatalf("finalized z = %q (present=%v), want \"s\"", got, present)
	}
}

func TestExtraAccumulatorKindChangeReturnsErrorAndKeepsLastValue(t *testing.T) {
	acc := NewExtraAccumulator()
	if err := acc.Add("reasoning", json.RawMessage(`"text"`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if err := acc.Add("reasoning", json.RawMessage(`[{"type":"text"}]`)); err == nil {
		t.Fatal("Add returned nil error for kind change")
	}
	if got := string(acc.Finalize()["reasoning"]); got != `[{"type":"text"}]` {
		t.Fatalf("reasoning = %s", got)
	}
}

func TestExtraAccumulatorOmitsNullValuesOnFinalize(t *testing.T) {
	acc := NewExtraAccumulator()
	if err := acc.Add("drop", json.RawMessage(`null`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if err := acc.Add("keep", json.RawMessage(`""`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	out := acc.Finalize()
	if _, ok := out["drop"]; ok {
		t.Fatalf("null value was kept: %#v", out)
	}
	if got := string(out["keep"]); got != `""` {
		t.Fatalf("keep = %s", got)
	}

	replacedNull := NewExtraAccumulator()
	if err := replacedNull.Add("k", json.RawMessage(`"a"`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if err := replacedNull.Add("k", json.RawMessage(`null`)); err == nil {
		t.Fatal("string to null must return a kind-change accumulator error")
	}
	if _, ok := replacedNull.Finalize()["k"]; ok {
		t.Fatalf("latest value is null and must be omitted: %#v", replacedNull.Finalize())
	}
}

func TestExtraAccumulatorIgnoresEmptyKeyAndValue(t *testing.T) {
	acc := NewExtraAccumulator()
	if err := acc.Add("", json.RawMessage(`"x"`)); err != nil {
		t.Fatalf("Add empty key returned error: %v", err)
	}
	if err := acc.Add("k", json.RawMessage(nil)); err != nil {
		t.Fatalf("Add empty value returned error: %v", err)
	}
	if got := acc.Finalize(); len(got) != 0 {
		t.Fatalf("Finalize = %#v, want empty", got)
	}
}

func TestExtraCloneDeepCopiesValues(t *testing.T) {
	srcRaw := json.RawMessage(`"hello"`)
	in := Extra{"a": srcRaw} // direct assignment shares bytes by caller choice; Clone is the accepting boundary.
	out := in.Clone()
	if string(out["a"]) != `"hello"` {
		t.Fatalf("clone a = %s", out["a"])
	}
	if len(srcRaw) > 0 && &out["a"][0] == &srcRaw[0] {
		t.Fatal("Clone shares bytes with its input")
	}
	out["a"][1] = 'H' // corrupt the clone's retained copy.
	if got := string(in["a"]); got != `"hello"` {
		t.Fatalf("original extra mutated through a Clone result: %s", in["a"])
	}

	var nilExtra Extra
	if got := nilExtra.Clone(); got != nil {
		t.Fatalf("nil Clone = %#v, want nil", got)
	}
	if len(out) != 1 {
		t.Fatalf("clone of one-entry extra wrong: %#v", out)
	}
}

func TestExtraFinalizeDropsNullsAndDeepCopies(t *testing.T) {
	extra := Extra{
		"drop": json.RawMessage(`null`),
		"keep": json.RawMessage(`""`),
	}
	clean := extra.Finalize()
	if _, ok := clean["drop"]; ok {
		t.Fatalf("null value was kept: %#v", clean)
	}
	if got := string(clean["keep"]); got != `""` {
		t.Fatalf("keep = %s", got)
	}

	srcRaw := json.RawMessage(`"x"`)
	withNull := Extra{"n": json.RawMessage(`null`), "k": srcRaw}
	out := withNull.Finalize()
	if out["k"] == nil || &out["k"][0] == &srcRaw[0] {
		t.Fatal("finalize did not deep copy retained values")
	}

	var zero Extra
	if z := zero.Finalize(); z != nil {
		t.Fatalf("zero Finalize = %#v, want nil", z)
	}
	allNull := Extra{"only": json.RawMessage(`null`)}
	if len(allNull.Finalize()) != 0 {
		t.Fatalf("all-null finalize not empty: %#v", allNull.Finalize())
	}
}

func TestCloneRawCopiesBytes(t *testing.T) {
	src := json.RawMessage(`"abc"`)
	dst := CloneRaw(src)
	if string(dst) != `"abc"` || len(dst) == 0 || &dst[0] == &src[0] {
		t.Fatalf("CloneRaw = %s, want independent copy", dst)
	}
	var nilSrc json.RawMessage
	if got := CloneRaw(nilSrc); len(got) != 0 {
		t.Fatalf("CloneRaw(nil) = %#v, want empty", got)
	}
	dst[1] = 'x'
	if string(src) != `"abc"` {
		t.Fatal("mutating the copy changed the source")
	}
}

func TestExtraAccumulatorAddDeepCopiesInput(t *testing.T) {
	srcRaw := json.RawMessage(`"first"`)
	acc := NewExtraAccumulator()
	if err := acc.Add("k", srcRaw); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	srcRaw[1] = 'X'
	got := acc.Finalize()["k"]
	if string(got)[:1] == `"X` || &got[0] == &srcRaw[0] {
		t.Fatalf("accumulator stored caller bytes: %s", got)
	}

	out := acc.Finalize()
	src2 := json.RawMessage(`"second"`)
	in := Extra{"k": src2}
	clonedExtra := in.Clone()
	if &clonedExtra["k"][0] == &src2[0] {
		t.Fatal("Clone shared bytes with input")
	}
	_ = out
}
