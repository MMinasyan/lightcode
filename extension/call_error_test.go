package extension_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
)

func TestCallErrorPreservesCauseAndPartial(t *testing.T) {
	cause := fmt.Errorf("restore: %w", harness.ErrConflict)
	partial := []byte(`{"restored":["a.go"]}`)
	failure := &extension.CallError{Err: cause, Partial: partial}
	if failure.Error() != cause.Error() {
		t.Fatalf("Error() = %q, want %q", failure.Error(), cause.Error())
	}
	if !errors.Is(failure, harness.ErrConflict) {
		t.Fatalf("cause class lost: %v", failure)
	}
	if string(failure.Partial) != string(partial) {
		t.Fatalf("Partial = %q, want %q", failure.Partial, partial)
	}
}
