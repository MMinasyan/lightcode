package agent

import (
	"fmt"
	"testing"
)

// countingSvc embeds AdapterService so that any owner call other than the
// overridden SessionSummaryForSession panics on the nil interface, and counts
// every liveness probe. A non-querying delivery filter must leave the count at
// zero and never trip the panic.
type countingSvc struct {
	AdapterService
	summaryCalls int
	summaryErr   error
}

func (c *countingSvc) SessionSummaryForSession(id string) (SessionSummary, error) {
	c.summaryCalls++
	if c.summaryErr != nil {
		return SessionSummary{}, c.summaryErr
	}
	return SessionSummary{ID: id}, nil
}

func TestAcceptsEventForCurrentDoesNotQueryOwner(t *testing.T) {
	svc := &countingSvc{summaryErr: fmt.Errorf("owner probe not allowed in delivery")}
	v := NewSessionView(svc)
	v.SetCurrent("A")

	if !v.AcceptsEventForCurrent("A", Event{Kind: EventTextDelta, SessionID: "A"}) {
		t.Fatal("session-tagged event for the current session was rejected")
	}
	if v.AcceptsEventForCurrent("A", Event{Kind: EventTextDelta, SessionID: "B"}) {
		t.Fatal("session-tagged event for a different session was accepted")
	}
	if !v.AcceptsEventForCurrent("A", Event{Kind: EventTextDelta}) {
		t.Fatal("global (untagged) event was rejected")
	}
	if svc.summaryCalls != 0 {
		t.Fatalf("delivery filter queried the owner %d times, want 0", svc.summaryCalls)
	}
}

func TestAcceptsEventForCurrentFiltersLateSourceSession(t *testing.T) {
	v := NewSessionView(&countingSvc{})

	// Presentation current advanced A -> B; a late A event must not render,
	// while a B event does.
	if !v.AcceptsEventForCurrent("B", Event{Kind: EventTextDelta, SessionID: "B"}) {
		t.Fatal("event for the current session was rejected")
	}
	if v.AcceptsEventForCurrent("B", Event{Kind: EventTextDelta, SessionID: "A"}) {
		t.Fatal("late event for the previous session was accepted")
	}
	// No current session drops every session-tagged event (startup window).
	if v.AcceptsEventForCurrent("", Event{Kind: EventTextDelta, SessionID: "A"}) {
		t.Fatal("session-tagged event accepted while no session is current")
	}
}

func TestAcceptsEventForCurrentSubagentChildren(t *testing.T) {
	v := NewSessionView(&countingSvc{})
	v.SetCurrent("root")

	// No current session rejects subagent events.
	if v.AcceptsEventForCurrent("", Event{SubagentSessionID: "child", ParentSessionID: "root"}) {
		t.Fatal("subagent event accepted with no current session")
	}
	// A direct child of current registers and is accepted.
	if !v.AcceptsEventForCurrent("root", Event{SubagentSessionID: "child", ParentSessionID: "root"}) {
		t.Fatal("direct child of the current session was rejected")
	}
	// A subsequent event from the registered child is accepted via the set.
	if !v.AcceptsEventForCurrent("root", Event{SubagentSessionID: "child"}) {
		t.Fatal("registered child event was rejected")
	}
	// A child of a different parent is rejected.
	if v.AcceptsEventForCurrent("root", Event{SubagentSessionID: "other", ParentSessionID: "elsewhere"}) {
		t.Fatal("child of a different parent was accepted")
	}
}
