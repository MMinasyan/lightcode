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
		"setCurrentSessionID", "clearRouteIfCurrent", "currentSessionID", "acceptsSubagentEventForCurrent")
	assertConfinedTo(t, "a.routeChildren",
		"setCurrentSessionID", "clearRouteIfCurrent", "acceptsSubagentEventForCurrent")
}

// TestWailsEventAcceptanceIsNavMuFree proves the event-acceptance path never takes
// navMu. handleEvent runs on the owner's event callback, and an operation may hold
// navMu while waiting on the owner, so acquiring navMu on the callback could
// deadlock; the path reads routing through the routeMu leaf lock instead.
func TestWailsEventAcceptanceIsNavMuFree(t *testing.T) {
	navMuUsers := appFuncsReferencing(t, "a.navMu")
	for _, fn := range []string{
		"handleEvent", "acceptsEvent", "acceptsSessionEvent", "acceptsSubagentEvent",
		"acceptsSubagentEventForCurrent", "liveCurrentSessionID", "currentSessionID", "clearRouteIfCurrent",
	} {
		if navMuUsers[fn] {
			t.Fatalf("%s takes navMu, but it is on the event-acceptance callback path, which must stay navMu-free", fn)
		}
	}
}
