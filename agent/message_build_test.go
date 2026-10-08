package agent

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// TestCompleteCallCountMatchesBuildCalls compares the scalar completion count with the owning construction over complete and incomplete tool-slot fixtures — the query must equal buildCalls' length without sorting, argument conversion or extra finalization — and pins that reading the count leaves the built order and verbatim raw arguments/extras untouched.
func TestCompleteCallCountMatchesBuildCalls(t *testing.T) {
	cases := []struct {
		name  string
		frags []model.ToolCallFragment
		want  int
	}{
		{name: "no slots", frags: nil, want: 0},
		{name: "one complete", frags: []model.ToolCallFragment{{Position: posI(0), ID: "a", Name: "fn", ArgumentFragment: `{"x":1}`}}, want: 1},
		{name: "many complete out of arrival order", frags: []model.ToolCallFragment{
			{Position: posI(2), ID: "c", Name: "fnC"},
			{Position: posI(0), ID: "a", Name: "fnA", ArgumentFragment: `{"x":1}`},
			{Position: posI(1), ID: "b", Name: "fnB", Extra: model.Extra{"meta": json.RawMessage(`{"k":1}`)}},
		}, want: 3},
		{name: "id only is incomplete", frags: []model.ToolCallFragment{{ID: "a"}}, want: 0},
		{name: "name only is incomplete", frags: []model.ToolCallFragment{{Name: "n"}}, want: 0},
		{name: "anonymous args only is incomplete", frags: []model.ToolCallFragment{{ArgumentFragment: `{}`}}, want: 0},
		{name: "mixed complete and incomplete", frags: []model.ToolCallFragment{
			{Position: posI(0), ID: "a", Name: "fn"},
			{Position: posI(1), ID: "b"},
			{Position: posI(2), ID: "c", Name: "gn", ArgumentFragment: `[]`},
		}, want: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newAssemblyState(testRef)
			st.applyToolEvent(tc.frags)

			if got := st.completeCallCount(); got != tc.want {
				t.Fatalf("completeCallCount = %d, want %d", got, tc.want)
			}

			// The scalar reads above must not alter the eventual owning construction.
			calls := st.buildCalls()
			if len(calls) != tc.want {
				t.Fatalf("buildCalls length = %d, want %d", len(calls), tc.want)
			}
			if st.completeCallCount() != len(calls) {
				t.Fatalf("completeCallCount = %d, want len(buildCalls()) = %d", st.completeCallCount(), len(calls))
			}
		})
	}

	// Ordered raw fidelity over the multi-call fixture: ascending position order and exact raw arguments/extras.
	st := newAssemblyState(testRef)
	st.applyToolEvent([]model.ToolCallFragment{
		{Position: posI(2), ID: "c", Name: "fnC", ArgumentFragment: `{"z":`},
		{Position: posI(0), ID: "a", Name: "fnA", ArgumentFragment: `{"x":1}`},
		{Position: posI(1), ID: "b", Name: "fnB", Extra: model.Extra{"meta": json.RawMessage(`{"k":1}`)}},
		{Position: posI(3), ID: "d"}, // incomplete sibling must not disturb the built order or values.
	})
	if st.completeCallCount() != 3 {
		t.Fatalf("completeCallCount = %d, want 3", st.completeCallCount())
	}
	calls := st.buildCalls()
	want := []struct {
		id, name, args string
		extra          string
	}{
		{id: "a", name: "fnA", args: `{"x":1}`},
		{id: "b", name: "fnB", args: ``},
		{id: "c", name: "fnC", args: `{"z":`},
	}
	if len(calls) != len(want) {
		t.Fatalf("built calls = %d, want %d", len(calls), len(want))
	}
	for i, w := range want {
		if calls[i].ID != w.id || calls[i].Name != w.name || string(calls[i].Arguments) != w.args {
			t.Fatalf("call[%d] = %#v, want id=%s name=%s args=%s", i, calls[i], w.id, w.name, w.args)
		}
	}
	if got := string(calls[1].Extra["meta"]); got != `{"k":1}` {
		t.Fatalf("call extras not finalized verbatim: %s", got)
	}
}
