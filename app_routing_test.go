package main

import (
	"os"
	"regexp"
	"testing"
)

// appFuncsReferencing returns the names of top-level app.go functions whose body
// contains the given code token (e.g. "a.routeProjectPath"). Matching the receiver
// form keeps comments that merely name a field from counting as an access.
func appFuncsReferencing(t *testing.T, token string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("read app.go: %v", err)
	}
	s := string(src)
	decl := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)\(`)
	locs := decl.FindAllStringSubmatchIndex(s, -1)
	// Match the token as a whole identifier so "a.routeProjectPath" does not also
	// match "a.routeProjectPathBounded".
	ref := regexp.MustCompile(regexp.QuoteMeta(token) + `\b`)
	out := map[string]bool{}
	for i, loc := range locs {
		name := s[loc[2]:loc[3]]
		end := len(s)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if ref.MatchString(s[loc[1]:end]) {
			out[name] = true
		}
	}
	return out
}

// TestWailsUsageCallbackDoesNotQueryOwner proves the usage owner-query is confined to
// the bound request method; the event callback (handleEvent) applies the event's
// cumulative report as a replacement instead of querying, so it never re-enters the
// owner while it emits under tokensMu.
func TestWailsUsageCallbackDoesNotQueryOwner(t *testing.T) {
	assertConfinedTo(t, "a.tokenUsage", "TokenUsage")
}

func assertConfinedTo(t *testing.T, token string, allowed ...string) {
	t.Helper()
	allow := map[string]bool{}
	for _, a := range allowed {
		allow[a] = true
	}
	for name := range appFuncsReferencing(t, token) {
		if !allow[name] {
			t.Fatalf("%s is accessed in %s, which is not one of its guarded accessors %v", token, name, allowed)
		}
	}
}

// TestWailsRoutingStateIsEncapsulated proves the adapter's routing-current state is
// touched only through its guarded accessors: the project path only under navMu,
// the session id and subagent child set only under the routeMu leaf lock. A bare
// access anywhere else would read or write routing off its lock.
func TestWailsRoutingStateIsEncapsulated(t *testing.T) {
	assertConfinedTo(t, "a.routeProjectPath",
		"routeProjectPathCaptured", "routeProjectPathBounded", "ProjectSwitch", "SessionNew", "startup")
	assertConfinedTo(t, "a.routeCurrent",
		"setCurrentSessionID", "clearRouteIfCurrent", "currentSessionID")
}

// TestWailsTitleChangesOnlyThroughOrderedBoundary proves no operation sets the
// native window title directly: WindowSetTitle appears only in the drainer's title
// choke point, so a project switch's title changes only when its boundary is
// consumed — a stalled boundary keeps the title at the prior project.
func TestWailsTitleChangesOnlyThroughOrderedBoundary(t *testing.T) {
	for name := range appFuncsReferencing(t, "WindowSetTitle") {
		if name != "startDelivery" {
			t.Fatalf("WindowSetTitle is called in %s; the title must change only through the ordered delivery drainer", name)
		}
	}
}

// TestWailsEventDeliveryIsNavMuFree proves the event callback and the sole drainer
// never take navMu. handleEvent runs on the owner's event callback, and an
// operation may hold navMu while waiting on the owner, so acquiring navMu on the
// callback or drainer could deadlock; the callback only appends frames and the
// drainer filters them against presentation current under the deliveryMu leaf.
func TestWailsEventDeliveryIsNavMuFree(t *testing.T) {
	navMuUsers := appFuncsReferencing(t, "a.navMu")
	for _, fn := range []string{
		"handleEvent", "enqueueFrame", "emitFrame", "emitSessionFrame",
		"emitSubagentFrame", "presentAcceptsLocked", "seedPresented", "emitResyncBoundary",
		"sessionChangedPayload", "currentSessionID", "clearRouteIfCurrent",
	} {
		if navMuUsers[fn] {
			t.Fatalf("%s takes navMu, but it is on the event callback or delivery path, which must stay navMu-free", fn)
		}
	}
}

// TestWailsHandleEventDoesNotQueryOwnerForDelivery proves the delivery filter is
// owner-query-free: the callback appends frames without probing session liveness,
// and the drainer decides delivery against a drainer-owned presentation current, so
// no path here re-enters the owner while it may hold an event-producing lock.
func TestWailsHandleEventDoesNotQueryOwnerForDelivery(t *testing.T) {
	for _, probe := range []struct {
		fn     string
		tokens []string
	}{
		{"handleEvent", []string{"a.acceptsEvent", "a.liveCurrentSessionID", "a.currentSessionID"}},
		{"presentAcceptsLocked", []string{"a.svc", "a.liveCurrentSessionID"}},
		{"emitSessionFrame", []string{"a.svc", "a.liveCurrentSessionID"}},
		{"emitSubagentFrame", []string{"a.svc", "a.liveCurrentSessionID"}},
		{"enqueueFrame", []string{"a.svc", "a.liveCurrentSessionID"}},
	} {
		for _, token := range probe.tokens {
			if appFuncsReferencing(t, token)[probe.fn] {
				t.Fatalf("%s references %s, but the delivery filter must not query the owner", probe.fn, token)
			}
		}
	}
}
