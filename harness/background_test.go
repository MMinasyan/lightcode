package harness

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// stubJobStopper satisfies the seam; Step 2 only proves a non-nil stopper is
// accepted and nil stays legal.
type stubJobStopper struct{}

func (stubJobStopper) StopJob(string, string) {}

var _ JobStopper = (*stubJobStopper)(nil)

func admitLocked(h *Harness, c *coordinator, kind memberKind, id string, cap int) (*backgroundMember, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return h.admitBackgroundMember(c, kind, id, cap)
}

func claimLocked(h *Harness, c *coordinator, completionID string) (*backgroundMember, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return h.claimBackgroundMember(c, completionID)
}

var completionIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestDependenciesAcceptJobStopper proves the job-stop seam builds: the
// optional dependency accepts a stopper, and a harness constructed without
// one keeps the nil that StartJob will reject.
func TestDependenciesAcceptJobStopper(t *testing.T) {
	withJobs := newTestHarness(t, freshSessionStore(t), nil)
	withJobs.deps.Jobs = stubJobStopper{}
	testassert.Require(t, withJobs.deps.Jobs != nil, "Jobs dependency rejected a stopper")
	withoutJobs := newTestHarness(t, freshSessionStore(t), nil)
	testassert.Require(t, withoutJobs.deps.Jobs == nil, "Jobs dependency defaults to non-nil, want legal nil")
}

// TestAdmitBackgroundMemberInitializesGroup proves group initialization on the
// coordinatorFor construction site: the first admission creates the group, the
// member carries its reserved completion identity, an open done channel, and
// the unclaimed state; the coordinator's lifecycle starts open.
func TestAdmitBackgroundMemberInitializesGroup(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	c := leaseCoordinator(t, h, testSessionID)
	testassert.Equal(t, c.bgState, bgOpen)
	first, err := admitLocked(h, c, memberChild, "child-1", 0)
	testassert.NoError(t, err)
	testassert.Require(t, c.group != nil && len(c.group.members) == 1, "first admission did not initialize the group: %+v", c.group)
	testassert.Require(t, first.kind == memberChild && first.id == "child-1", "member = %+v", first)
	testassert.Require(t, completionIDShape.MatchString(first.completionID) && !first.claimed, "completion id %q is not 32 lowercase hex; fresh member is claimed", first.completionID)
	select {
	case <-first.done:
		t.Fatalf("fresh member done channel is closed")
	default:
	}
	second, err := admitLocked(h, c, memberJob, "job-1", 0)
	testassert.NoError(t, err)
	testassert.Require(t, len(c.group.members) == 2 && (c.group.members[first.completionID] == first && c.group.members[second.completionID] == second), "got %v, want %v; members are not keyed by completion id", len(c.group.members), 2)
}

// TestCreateSessionStartsOpenLifecycle proves the CreateSession construction
// site starts the background lifecycle open.
func TestCreateSessionStartsOpenLifecycle(t *testing.T) {
	h := newTestHarness(t, emptyStore(t), nil)
	rec, err := h.CreateSession(context.Background(), CreateSessionRequest{Workspace: "/tmp/works", AgentType: "coder"})
	testassert.NoError(t, err)
	h.mu.Lock()
	c := h.sessions[rec.Identity.SessionID]
	h.mu.Unlock()
	testassert.Require(t, c != nil, "CreateSession did not cache a coordinator")
	testassert.Equal(t, c.bgState, bgOpen)
}

// TestAdmitBackgroundMemberCapRejection proves the child concurrency cap: with
// cap 2 the third member is rejected with the limit text and no member exists
// for it.
func TestAdmitBackgroundMemberCapRejection(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	c := leaseCoordinator(t, h, testSessionID)
	for i := 0; i < 2; i++ {
		{
			_, err := admitLocked(h, c, memberChild, "child", 2)
			testassert.Require(t, err == nil, "admission %d: %v", i+1, err)
		}
	}
	_, err := admitLocked(h, c, memberChild, "child", 2)
	testassert.Require(t, errors.Is(err, ErrInvalid), "third admission = %v, want ErrInvalid", err)
	want := "background group is full (2/2). Wait for a background member to complete, then retry"
	testassert.Require(t, err != nil && err.Error() == "harness: invalid storage request: "+want, "third admission error = %q, want %q", err, want)
	testassert.Equal(t, len(c.group.members), 2)
}

// TestAdmitBackgroundMemberClosedAndArchivedRejection proves the lifecycle
// rejections: a non-open session uses Submit's archived error for both kinds,
// and a stopping or closed group rejects uniformly.
func TestAdmitBackgroundMemberClosedAndArchivedRejection(t *testing.T) {
	archived := validSessionRecord()
	now := time.Now().UTC()
	archived.State.Lifecycle = LifecycleArchived
	archived.State.ArchivedAt = &now
	h := newTestHarness(t, (&testGraph{session: archived}).storage(t), nil)
	c := leaseCoordinator(t, h, testSessionID)
	for _, kind := range []memberKind{memberChild, memberJob} {
		_, err := admitLocked(h, c, kind, "member", 0)
		want := invalidInput("session %q is archived; admission requires an open Session", testSessionID)
		testassert.Require(t, err != nil && err.Error() == want.Error(), "%s admission on archived session = %v, want %v", kind, err, want)
		testassert.Require(t, c.group == nil, "rejected admission created a group")
	}

	h2 := newTestHarness(t, freshSessionStore(t), nil)
	c2 := leaseCoordinator(t, h2, testSessionID)
	for _, state := range []bgState{bgStopping, bgClosed} {
		c2.mu.Lock()
		c2.bgState = state
		c2.mu.Unlock()
		_, err := admitLocked(h2, c2, memberChild, "child", 0)
		testassert.Require(t, err != nil && err.Error() == "harness: invalid storage request: background group is closed or stopping", "admission in state %q = %v, want the closed-or-stopping rejection", state, err)
		testassert.Require(t, c2.group == nil, "rejected admission created a group")
	}
}

// TestAdmitBackgroundMemberAfterHarnessCancellation proves a start after
// harness cancellation is rejected with the raw context error before any
// member exists.
func TestAdmitBackgroundMemberAfterHarnessCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h, err := New(ctx, Dependencies{
		Storage: freshSessionStore(t),
		Prepare: func(context.Context, PreparationRequest) (PreparedExecution, error) {
			return PreparedExecution{}, errors.New("no preparation configured for this test")
		},
	})
	testassert.NoError(t, err)
	cancel()
	c := leaseCoordinator(t, h, testSessionID)
	c.mu.Lock()
	_, admitErr := h.admitBackgroundMember(c, memberChild, "child", 0)
	c.mu.Unlock()
	testassert.Require(t, errors.Is(admitErr, context.Canceled) && admitErr.Error() == "context canceled", "admission after cancellation = %v, want the raw context.Canceled", admitErr)
	testassert.Require(t, c.group == nil, "rejected admission created a member")
}

// TestClaimBackgroundMember proves the first-writer claim: the first claim
// marks the member and keeps it in the map; a second claim or an unknown id
// returns not-claimed.
func TestClaimBackgroundMember(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	c := leaseCoordinator(t, h, testSessionID)
	{
		m, ok := claimLocked(h, c, "absent")
		testassert.Require(t, m == nil && !ok, "unknown claim = (%v, %v), want (nil, false)", m, ok)
	}
	m, err := admitLocked(h, c, memberJob, "job-1", 0)
	testassert.NoError(t, err)
	got, ok := claimLocked(h, c, m.completionID)
	testassert.Require(t, ok && got == m, "first claim = (%v, %v), want the member", got, ok)
	testassert.Require(t, m.claimed, "claimed member is not marked claimed")
	c.mu.Lock()
	_, present := c.group.members[m.completionID]
	c.mu.Unlock()
	testassert.Require(t, present, "claimed member left the group before finishing")
	{
		got, ok := claimLocked(h, c, m.completionID)
		testassert.Require(t, got == nil && !ok, "second claim = (%v, %v), want (nil, false)", got, ok)
	}
}

// TestFinishBackgroundMember proves the terminal transition: the first finish
// removes the member and closes done; a second finish is a no-op.
func TestFinishBackgroundMember(t *testing.T) {
	h := newTestHarness(t, freshSessionStore(t), nil)
	c := leaseCoordinator(t, h, testSessionID)
	m, err := admitLocked(h, c, memberChild, "child-1", 0)
	testassert.NoError(t, err)
	h.finishBackgroundMember(c, m)
	select {
	case <-m.done:
	default:
		t.Fatalf("finish did not close done")
	}
	c.mu.Lock()
	_, present := c.group.members[m.completionID]
	empty := len(c.group.members) == 0
	c.mu.Unlock()
	testassert.Require(t, !present, "finished member is still in the group")
	testassert.Require(t, empty, "group is not empty after finish")
	h.finishBackgroundMember(c, m) // must not panic or double-close
	c.mu.Lock()
	_, present = c.group.members[m.completionID]
	c.mu.Unlock()
	testassert.Require(t, !present, "no-op finish re-inserted the member")
}
