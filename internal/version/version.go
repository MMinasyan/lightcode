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

func parseTag(v string) (major, minor, patch int, ok bool) {
	if !strings.HasPrefix(v, "v") {
		return 0, 0, 0, false
	}
	parts := strings.Split(v[1:], ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var nums [3]int
	for i, p := range parts {
		// Empty components and leading zeros ("v0.0.03") are not tag shapes.
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return 0, 0, 0, false
		}
		n := 0
		for j := 0; j < len(p); j++ {
			c := p[j]
			if c < '0' || c > '9' {
				return 0, 0, 0, false
			}
			n = n*10 + int(c-'0')
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// Compare orders two exact vMAJOR.MINOR.PATCH tags: -1 if a < b, 0 if
// equal, +1 if a > b. Both inputs must satisfy IsRelease; anything else is
// the caller's bug.
func Compare(a, b string) int {
	am, an, ap, _ := parseTag(a)
	bm, bn, bp, _ := parseTag(b)
	switch {
	case am != bm:
		return sign(am - bm)
	case an != bn:
		return sign(an - bn)
	default:
		return sign(ap - bp)
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
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
