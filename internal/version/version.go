// Package version holds the binary's build identity.
//
// Version and Commit are stamped at build time:
//
//	-ldflags "-X github.com/MMinasyan/lightcode/internal/version.Version=v0.0.3
//	          -X github.com/MMinasyan/lightcode/internal/version.Commit=abc1234"
//
// An unstamped binary is a dev build.
package version

import (
	"runtime"
	"strings"
)

var (
	Version = "dev"
	Commit  = ""
)

// IsRelease reports whether v is an exact vMAJOR.MINOR.PATCH tag. Anything
// else — "dev", empty, a hash, a git-describe shape — is a dev build.
func IsRelease(v string) bool {
	_, _, _, ok := parseTag(v)
	return ok
}

// parseTag validates the tag grammar and returns the three decimal
// components as written. Components stay digit strings, so valid sizes
// beyond the machine integer range remain orderable without a cap.
func parseTag(v string) (major, minor, patch string, ok bool) {
	if !strings.HasPrefix(v, "v") {
		return "", "", "", false
	}
	parts := strings.Split(v[1:], ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	for _, p := range parts {
		// Empty components and leading zeros ("v0.0.03") are not tag shapes.
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return "", "", "", false
		}
		for j := 0; j < len(p); j++ {
			c := p[j]
			if c < '0' || c > '9' {
				return "", "", "", false
			}
		}
	}
	return parts[0], parts[1], parts[2], true
}

// Compare orders two exact vMAJOR.MINOR.PATCH tags: -1 if a < b, 0 if
// equal, +1 if a > b. Both inputs must satisfy IsRelease; anything else is
// the caller's bug.
func Compare(a, b string) int {
	am, an, ap, _ := parseTag(a)
	bm, bn, bp, _ := parseTag(b)
	if c := compareComponent(am, bm); c != 0 {
		return c
	}
	if c := compareComponent(an, bn); c != 0 {
		return c
	}
	return compareComponent(ap, bp)
}

// compareComponent orders two validated decimal components as nonnegative
// integers. Validated components carry no leading zeros, so the longer
// digit string is always the larger number and equal lengths order
// digit-by-digit.
func compareComponent(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// String renders the identity: the tag for releases, "dev (abc1234)" or
// "dev" for everything else.
func String() string {
	if IsRelease(Version) {
		return Version
	}
	if Commit != "" {
		return "dev (" + Commit + ")"
	}
	return "dev"
}

// Line is the full human version answer; the commit joins the platform
// inside one paren group on dev builds.
func Line() string {
	plat := runtime.GOOS + "/" + runtime.GOARCH
	if IsRelease(Version) {
		return "lightcode " + Version + " (" + plat + ")"
	}
	if Commit != "" {
		return "lightcode dev (" + Commit + ", " + plat + ")"
	}
	return "lightcode dev (" + plat + ")"
}

// Info is the machine-readable shape; fields report the raw stamped
// values, empty strings when absent.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Current returns the build identity of this binary.
func Current() Info {
	return Info{Version: Version, Commit: Commit, OS: runtime.GOOS, Arch: runtime.GOARCH}
}
