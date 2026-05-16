package message

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
	if got := string(acc.Extra()["reasoning_content"]); got != `"hello world"` {
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
	if got := string(acc.Extra()["reasoning_details"]); got != `[{"type":"text","text":"a"},{"type":"encrypted","data":"b"}]` {
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
	if got := string(acc.Extra()["reasoning"]); got != `{"second":true}` {
		t.Fatalf("reasoning = %s", got)
	}
}

func TestExtraAccumulatorKindChangeReturnsErrorAndKeepsLastValue(t *testing.T) {
	acc := NewExtraAccumulator()
	if err := acc.Add("reasoning", json.RawMessage(`"text"`)); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if err := acc.Add("reasoning", json.RawMessage(`[{"type":"text"}]`)); err == nil {
		t.Fatalf("Add returned nil error for kind change")
	}
	if got := string(acc.Extra()["reasoning"]); got != `[{"type":"text"}]` {
		t.Fatalf("reasoning = %s", got)
	}
}

func TestExtraWithoutNulls(t *testing.T) {
	extra := Extra{
		"drop": json.RawMessage(`null`),
		"keep": json.RawMessage(`""`),
	}
	clean := extra.WithoutNulls()
	if _, ok := clean["drop"]; ok {
		t.Fatalf("null value was kept: %#v", clean)
	}
	if got := string(clean["keep"]); got != `""` {
		t.Fatalf("keep = %s", got)
	}
}
