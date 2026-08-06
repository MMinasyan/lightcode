package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/snapshot"
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

// TestSessionViewReadOnlyMarker proves the read-only marker's lifecycle: a
// marked current stays routed but reports not live without clearing, the
// error helper names the contention while the marker holds and no current
// session otherwise, and routing to a different session invalidates the
// marker.
func TestSessionViewReadOnlyMarker(t *testing.T) {
	svc := &countingSvc{}
	v := NewSessionView(svc)
	v.SetCurrent("A")

	// No marker: the live view resolves and the helper follows it.
	if got := v.LiveCurrent(); got != "A" {
		t.Fatalf("LiveCurrent = %q, want A", got)
	}
	if got, err := v.LiveCurrentOrErr(); err != nil || got != "A" {
		t.Fatalf("LiveCurrentOrErr = %q, %v; want A", got, err)
	}

	// A marked read-only current stays routed but reports not live, and the
	// helper names the contention instead of "no current session".
	v.SetReadOnly("A")
	if got := v.LiveCurrent(); got != "" {
		t.Fatalf("LiveCurrent over the read-only marker = %q, want empty", got)
	}
	if got := v.Current(); got != "A" {
		t.Fatalf("read-only marker cleared routing: current = %q, want A", got)
	}
	if !v.IsReadOnly("A") {
		t.Fatal("IsReadOnly(A) = false, want true")
	}
	if _, err := v.LiveCurrentOrErr(); err == nil || err.Error() != `session "A" is being driven by another process` {
		t.Fatalf("LiveCurrentOrErr over the read-only marker = %v, want the contention error", err)
	}

	// Routing to a different session invalidates the marker.
	v.SetCurrent("B")
	if v.IsReadOnly("A") {
		t.Fatal("read-only marker survived a routing change")
	}
	if got := v.LiveCurrent(); got != "B" {
		t.Fatalf("LiveCurrent after routing away = %q, want B", got)
	}

	// A live commit of the same id clears the marker: reopening the marked
	// session after the holder releases it must not stay read-only.
	v.SetCurrent("A")
	v.SetReadOnly("A")
	if !v.IsReadOnly("A") {
		t.Fatal("SetReadOnly(A) did not mark A")
	}
	v.SetCurrent("A")
	if v.IsReadOnly("A") {
		t.Fatal("read-only marker survived a live commit of the same id")
	}
	if got := v.LiveCurrent(); got != "A" {
		t.Fatalf("LiveCurrent after the same-id commit = %q, want A", got)
	}

	// With no current session the helper reports it plainly.
	v.SetCurrent("")
	if _, err := v.LiveCurrentOrErr(); err == nil || err.Error() != "no current session" {
		t.Fatalf("LiveCurrentOrErr with no current = %v, want no current session", err)
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

// stampSessionActivity rewrites a session's persisted last activity so the
// active-session listing order is deterministic instead of same-second ties.
func stampSessionActivity(t *testing.T, a *Agent, projectPath, id string, lastActivity int64) {
	t.Helper()
	proj, err := a.ensureProjectForPath(projectPath)
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	metaPath := filepath.Join(a.projects.SessionsRoot(proj.ID), id, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var meta snapshot.SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	meta.LastActivity = lastActivity
	out, err := json.Marshal(&meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(metaPath, out, 0o600); err != nil {
		t.Fatalf("rewrite meta: %v", err)
	}
}

// listingFailSvc embeds a real owner but fails the active-session listing, so
// a caller that reads the listing result must surface the error instead of
// silently creating a session.
type listingFailSvc struct {
	AdapterService
	listErr error
}

func (f *listingFailSvc) SessionListForProjectPath(projectPath, state string) ([]SessionSummary, error) {
	return nil, f.listErr
}

// TestOpenOrCreateSessionSkipsContendedNewestSession proves
// OpenOrCreateSession opens the newest candidate whose claim is acquirable:
// the newest session is held by another owner, so the older one opens.
func TestOpenOrCreateSessionSkipsContendedNewestSession(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	projectPath := t.TempDir()
	olderID, err := second.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath older: %v", err)
	}
	newestID, err := first.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath newest: %v", err)
	}
	stampSessionActivity(t, second, projectPath, olderID, 1)
	stampSessionActivity(t, first, projectPath, newestID, 2)

	scope := NewAdapterScope(second, projectPath)
	summary, err := scope.OpenOrCreateSession(projectPath)
	if err != nil {
		t.Fatalf("OpenOrCreateSession over a contended newest session: %v", err)
	}
	if summary.ID != olderID {
		t.Fatalf("opened session = %q, want the older session %q", summary.ID, olderID)
	}
}

// TestOpenOrCreateSessionCreatesWhenEveryCandidateContended proves
// OpenOrCreateSession falls through to creating a new session when every
// active candidate is held by another owner.
func TestOpenOrCreateSessionCreatesWhenEveryCandidateContended(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	projectPath := t.TempDir()
	olderID, err := first.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath older: %v", err)
	}
	newestID, err := first.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath newest: %v", err)
	}
	stampSessionActivity(t, first, projectPath, olderID, 1)
	stampSessionActivity(t, first, projectPath, newestID, 2)

	scope := NewAdapterScope(second, projectPath)
	summary, err := scope.OpenOrCreateSession(projectPath)
	if err != nil {
		t.Fatalf("OpenOrCreateSession with every candidate contended: %v", err)
	}
	if summary.ID == "" || summary.ID == olderID || summary.ID == newestID {
		t.Fatalf("opened session = %q, want a newly created session", summary.ID)
	}
}

// TestOpenOrCreateSessionSurfacesListingFailure proves OpenOrCreateSession
// reports a session-listing failure instead of silently creating a session.
func TestOpenOrCreateSessionSurfacesListingFailure(t *testing.T) {
	_, second := newSharedHomeAgentPair(t)
	projectPath := t.TempDir()
	listErr := fmt.Errorf("session listing failed")
	scope := NewAdapterScope(&listingFailSvc{AdapterService: second, listErr: listErr}, projectPath)
	_, err := scope.OpenOrCreateSession(projectPath)
	if err == nil {
		t.Fatal("OpenOrCreateSession with a failing listing = nil error, want the listing failure")
	}
	if !errors.Is(err, listErr) {
		t.Fatalf("OpenOrCreateSession error = %v, want the listing failure", err)
	}
}

// TestOpenOrCreateSessionSurfacesCorruptCandidate proves OpenOrCreateSession
// surfaces an open failure other than contention instead of skipping the
// candidate: this is user-initiated, so a corrupt session must not be passed
// over silently. The contended newest candidate is skipped first, so the
// corruption error can only be the scan reaching the corrupt candidate.
func TestOpenOrCreateSessionSurfacesCorruptCandidate(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	projectPath := t.TempDir()
	// The newest candidate is driven by the other owner; the older one is
	// corrupt: valid meta, broken compaction record.
	heldID, err := first.NewSessionForProjectPath(projectPath, "primary")
	if err != nil {
		t.Fatalf("NewSessionForProjectPath held: %v", err)
	}
	proj, err := second.ensureProjectForPath(projectPath)
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	const corruptID = "corrupt-session"
	sessionDir := filepath.Join(second.projects.SessionsRoot(proj.ID), corruptID)
	if err := os.MkdirAll(filepath.Join(sessionDir, "turns"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"id":%q,"state":"active","project_path":%q,"last_activity":%d}`+"\n",
		corruptID, projectPath, time.Now().Unix()-100)
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "compaction.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	scope := NewAdapterScope(second, projectPath)
	summary, err := scope.OpenOrCreateSession(projectPath)
	if err == nil {
		t.Fatalf("OpenOrCreateSession over a corrupt candidate = %#v, nil error; want the corruption surfaced", summary)
	}
	if !strings.Contains(err.Error(), "compaction.json") {
		t.Fatalf("OpenOrCreateSession error = %v, want the corrupt candidate's load failure (held candidate = %q)", err, heldID)
	}
}
