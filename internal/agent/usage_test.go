package agent

import (
	"testing"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
)

func TestRecordUsagePrefersEventModelRef(t *testing.T) {
	parentRef := coremodel.ModelRef{Provider: "parent-provider", Model: "parent-model"}
	childRef := coremodel.ModelRef{Provider: "child-provider", Model: "child-model"}
	a := &Agent{currentRef: parentRef}

	a.recordUsage(loop.Event{
		Kind:       loop.Usage,
		Model:      childRef.Model,
		ModelRef:   childRef,
		UsageKnown: true,
		Cache:      2,
		Input:      3,
		Output:     5,
	})

	if _, ok := a.tokens[parentRef.String()]; ok {
		t.Fatalf("usage was recorded under parent ref %s", parentRef.String())
	}
	entry := a.tokens[childRef.String()]
	if entry == nil {
		t.Fatalf("usage was not recorded under child ref %s", childRef.String())
	}
	if entry.Provider != childRef.Provider || entry.Model != childRef.Model {
		t.Fatalf("entry identity = %s/%s, want %s", entry.Provider, entry.Model, childRef.String())
	}
	if entry.Cache != 2 || entry.Input != 3 || entry.Output != 5 {
		t.Fatalf("entry usage = cache %d input %d output %d", entry.Cache, entry.Input, entry.Output)
	}
}
