package melta

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/protocol"
)

func TestErrorValueProjectsClassCauseAndOwnedPartial(t *testing.T) {
	partial := json.RawMessage("{\n  \"big\": 9007199254740993\n}")
	failure := &extension.CallError{Err: fmt.Errorf("command: %w", harness.ErrStorage), Partial: partial}
	value := ErrorValue(failure)
	if value.Code != protocol.ErrorCodeStorage || value.Message != failure.Error() {
		t.Fatalf("value = %+v, want the original storage class and message", value)
	}
	if value.PartialResult == nil || string(*value.PartialResult) != string(partial) {
		t.Fatalf("partial = %+v, want %q", value.PartialResult, partial)
	}
	partial[2] = 'X'
	if string(*value.PartialResult) != "{\n  \"big\": 9007199254740993\n}" {
		t.Fatalf("value retained the caller's partial buffer: %q", *value.PartialResult)
	}

	corrupt := &harness.CorruptionError{SessionID: "0123456789abcdef0123456789abcdef", Detail: "rows"}
	value = ErrorValue(corrupt)
	if value.Code != protocol.ErrorCodeCorrupt || value.SessionId == nil || *value.SessionId != corrupt.SessionID {
		t.Fatalf("corrupt value = %+v", value)
	}
	if value.PartialResult != nil {
		t.Fatalf("unrelated error invented a partial: %+v", value.PartialResult)
	}
}
