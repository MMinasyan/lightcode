package melta

import (
	"errors"
	"testing"

	"github.com/Syntheresis/melta/extension"
	"github.com/Syntheresis/melta/harness"
)

func TestOwnCallErrorValidatesAndOwnsPartial(t *testing.T) {
	partial := []byte(`{"big":9007199254740993}`)
	ownedErr := ownCallError(&extension.CallError{Err: harness.ErrStorage, Partial: partial})
	var callErr *extension.CallError
	if !errors.As(ownedErr, &callErr) || !errors.Is(ownedErr, harness.ErrStorage) {
		t.Fatalf("owned error = %v, want the call error chain", ownedErr)
	}
	partial[2] = 'X'
	if string(callErr.Partial) != `{"big":9007199254740993}` {
		t.Fatalf("owned partial = %q, want an independent copy", callErr.Partial)
	}
	if empty := ownCallError(&extension.CallError{Err: harness.ErrStorage}); empty == nil {
		t.Fatal("absent partial was rejected")
	}
	for _, invalid := range [][]byte{[]byte("{"), []byte("not json"), []byte(`{"a":}`)} {
		err := ownCallError(&extension.CallError{Err: harness.ErrStorage, Partial: invalid})
		if !errors.Is(err, extension.ErrInvalid) {
			t.Fatalf("invalid partial %q error = %v, want the invalid-output sentinel", invalid, err)
		}
		if errors.As(err, &callErr) {
			t.Fatalf("invalid partial %q was exposed as CallError", invalid)
		}
	}
	if null := ownCallError(&extension.CallError{Err: harness.ErrStorage, Partial: []byte("null")}); null == nil {
		t.Fatal("JSON null partial was rejected")
	}
}
