package catalog

import (
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// TestModelRefBridgeForwardsPublicIdentity pins the catalog's ModelRef
// forwarding bridge to the public model identity: the sentinel is the same
// error value, ParseModelRef errors resolve against that sentinel, and the
// alias is the public type itself (its String/JSON contracts are owned by
// model/model_ref_test.go).
func TestModelRefBridgeForwardsPublicIdentity(t *testing.T) {
	testassert.Equal(t, ErrInvalidModelRef, model.ErrInvalidModelRef)

	_, err := ParseModelRef("no-slash")
	testassert.Require(t, err != nil, "ParseModelRef(no-slash) = nil, want error")
	testassert.Require(t, errors.Is(err, model.ErrInvalidModelRef), "ParseModelRef error = %v, want errors.Is against the model sentinel", err)

	ref, err := ParseModelRef("prov/name/extra")
	testassert.NoError(t, err)
	{
		want := (model.ModelRef{Provider: "prov", Model: "name/extra"})
		testassert.Equal(t, ref, want)
	}
}
