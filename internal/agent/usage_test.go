package agent

import (
	"testing"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
)

func TestRecordUsagePrefersEventModelRef(t *testing.T) {
	parentRef := coremodel.ModelRef{Provider: "parent-provider", Model: "parent-model"}
	childRef := coremodel.ModelRef{Provider: "child-provider", Model: "child-model"}
	a := &Agent{session: &session{currentRef: parentRef}}

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

// TestRecordUsageCarriesCumulativeReport verifies each EventUsage carries the
// session's absolute cumulative token report (a replacement a consumer applies
// without querying the owner) while the delta fields still carry only that
// event's contribution.
func TestRecordUsageCarriesCumulativeReport(t *testing.T) {
	a := newCatalogBackedTestAgent(t)

	a.session.tokensMu.Lock()
	a.session.tokens = nil
	a.session.tokensMu.Unlock()

	var usage []Event
	a.SetEventHandler(func(ev Event) {
		if ev.Kind == EventUsage {
			usage = append(usage, ev)
		}
	})

	ref := coremodel.ModelRef{Provider: "p", Model: "m"}
	a.recordUsageForSession(a.session, loop.Event{Model: "m", ModelRef: ref, UsageKnown: true, Cache: 1, Input: 2, Output: 3})
	a.recordUsageForSession(a.session, loop.Event{Model: "m", ModelRef: ref, UsageKnown: true, Cache: 0, Input: 4, Output: 5})

	if len(usage) != 2 {
		t.Fatalf("captured %d usage events, want 2", len(usage))
	}
	last := usage[1]
	if last.CumulativeTokens == nil {
		t.Fatal("EventUsage missing cumulative token report")
	}
	if tot := last.CumulativeTokens.Total; tot.Cache != 1 || tot.Input != 6 || tot.Output != 8 {
		t.Fatalf("cumulative total = cache %d input %d output %d, want 1/6/8", tot.Cache, tot.Input, tot.Output)
	}
	if last.Input != 4 || last.Output != 5 || last.Cache != 0 {
		t.Fatalf("delta fields = cache %d input %d output %d, want 0/4/5", last.Cache, last.Input, last.Output)
	}
}
