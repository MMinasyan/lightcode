package harness

import "fmt"

// memberKind classifies one background ownership-group member.
type memberKind string // "child", "job"

const (
	memberChild memberKind = "child"
	memberJob   memberKind = "job"
)

// bgState is the architecture's one group lifecycle.
type bgState string // "open", "stopping", "closed"

const (
	bgOpen     bgState = "open"
	bgStopping bgState = "stopping"
	bgClosed   bgState = "closed"
)

// backgroundMember is one registered member of a Session's background
// ownership group.
type backgroundMember struct {
	kind         memberKind
	id           string        // child session ID or job ID
	completionID string        // reserved, session-unique, first-writer-wins
	claimed      bool          // set by the first-winner claim; a second claim is a no-op
	done         chan struct{} // closed when the member finishes
}

// backgroundGroup is one Session's set of live background members.
type backgroundGroup struct {
	members map[string]*backgroundMember // keyed by completionID; claimed members remain until finished
}

// launchInfo is one child Session's reserved completion identity.
type launchInfo struct {
	completionID string // the child member's reserved completion identity
	outputLimit  int
}

// stopInterval is one stop's join handle: err is written before done closes
// (the workspaceAttempt pattern, runtime/composition.go:673-677).
type stopInterval struct {
	done chan struct{}
	err  error
}

// admitBackgroundMember registers one background member on the Session's
// group. It runs under the caller-held c.mu and rejects a non-open Session
// lifecycle, harness cancellation (the raw error, Submit's entry-gate shape),
// and a group that is stopping or closed; with a positive cap the kind's live
// count must stay below it. The returned member is the caller's handle —
// member.completionID is the completion identity every later step consumes.
func (h *Harness) admitBackgroundMember(c *coordinator, kind memberKind, id string, cap int) (*backgroundMember, error) {
	if c.graph.Session.State.Lifecycle != LifecycleOpen {
		return nil, invalidInput("session %q is archived; admission requires an open Session", c.graph.Session.Identity.SessionID)
	}
	if err := h.ctx.Err(); err != nil {
		return nil, err
	}
	if c.bgState != bgOpen {
		return nil, invalidInput("background group is closed or stopping")
	}
	if c.group == nil {
		c.group = &backgroundGroup{members: map[string]*backgroundMember{}}
	}
	if cap > 0 {
		count := 0
		for _, m := range c.group.members {
			if m.kind == kind {
				count++
			}
		}
		if count >= cap {
			return nil, invalidInput("background group is full (%d/%d). Wait for a background member to complete, then retry", count, cap)
		}
	}
	completionID, err := newHexID()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	m := &backgroundMember{kind: kind, id: id, completionID: completionID, done: make(chan struct{})}
	c.group.members[completionID] = m
	return m, nil
}

// claimBackgroundMember marks one member claimed and returns it; the member
// stays in the map — visible to Stop's snapshot and the archive gate while
// its settlement is in flight. A missing or already-claimed ID returns
// (nil, false). It runs under the caller-held c.mu.
func (h *Harness) claimBackgroundMember(c *coordinator, completionID string) (*backgroundMember, bool) {
	if c.group == nil {
		return nil, false
	}
	m := c.group.members[completionID]
	if m == nil || m.claimed {
		return nil, false
	}
	m.claimed = true
	return m, true
}

// finishBackgroundMember performs the terminal transition: it removes the
// member if still present and closes done. An already-removed member is a
// no-op. Every member-ending path runs claim-then-finish. It takes c.mu
// itself.
func (h *Harness) finishBackgroundMember(c *coordinator, m *backgroundMember) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.group == nil {
		return
	}
	if _, ok := c.group.members[m.completionID]; !ok {
		return
	}
	delete(c.group.members, m.completionID)
	close(m.done)
}
