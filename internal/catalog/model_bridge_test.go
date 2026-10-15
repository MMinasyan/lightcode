package catalog

import (
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// TestModelRefBridgeForwardsPublicIdentity pins the catalog's ModelRef
// forwarding bridge to the public model identity: the sentinel is the same
// error value, ParseModelRef errors resolve against that sentinel, and the
// alias is the public type itself (its String/JSON contracts are owned by
// model/model_ref_test.go).
func TestModelRefBridgeForwardsPublicIdentity(t *testing.T) {
	if ErrInvalidModelRef != model.ErrInvalidModelRef {
		t.Fatalf("ErrInvalidModelRef = %v, want the model sentinel %v", ErrInvalidModelRef, model.ErrInvalidModelRef)
	}

	_, err := ParseModelRef("no-slash")
	if err == nil {
		t.Fatal("ParseModelRef(no-slash) = nil, want error")
	}
	if !errors.Is(err, model.ErrInvalidModelRef) {
		t.Fatalf("ParseModelRef error = %v, want errors.Is against the model sentinel", err)
	}

	ref, err := ParseModelRef("prov/name/extra")
	if err != nil {
		t.Fatalf("ParseModelRef: %v", err)
	}
	if want := (model.ModelRef{Provider: "prov", Model: "name/extra"}); ref != want {
		t.Fatalf("ParseModelRef = %+v, want %+v", ref, want)
	}
}
