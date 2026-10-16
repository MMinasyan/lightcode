package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestIsRelease(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"v0.0.3", true},
		{"v1.2.3", true},
		{"v10.20.30", true},
		{"v0.0.0", true},
		// Valid components carry no size limit; only the grammar decides.
		{"v9223372036854775808.0.0", true},
		{"v18446744073709551615.0.0", true},
		{"v" + strings.Repeat("9", 40) + "." + strings.Repeat("9", 40) + "." + strings.Repeat("9", 40), true},
		{"dev", false},
		{"", false},
		{"df5eab6-dirty", false},
		{"v0.0.2-5-gabc1234-dirty", false},
		{"v0.1", false},
		{"v0.0.3.4", false},
		{"0.0.3", false},
		{"v0.0.03", false},
		{"v0.0.", false},
		{"v..3", false},
		{"v0.0.3 ", false},
	}
	for _, c := range cases {
		if got := IsRelease(c.v); got != c.want {
			t.Errorf("IsRelease(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.0.3", "v0.0.3", 0},
		{"v0.0.3", "v0.0.4", -1},
		{"v0.0.4", "v0.0.3", 1},
		{"v0.0.10", "v0.0.9", 1},
		{"v0.9.0", "v0.10.0", -1},
		{"v1.0.0", "v0.99.99", 1},
		{"v2.0.0", "v10.0.0", -1},
		// Components past the machine integer range keep their
		// mathematical order; signed accumulation wraps and inverts them.
		{"v18446744073709551615.0.0", "v1.0.0", 1},
		{"v1.0.0", "v18446744073709551615.0.0", -1},
		{"v0.18446744073709551615.0", "v0.1.0", 1},
		{"v0.1.0", "v0.18446744073709551615.0", -1},
		{"v0.0.18446744073709551615", "v0.0.1", 1},
		{"v0.0.1", "v0.0.18446744073709551615", -1},
		{"v9223372036854775807.0.0", "v9223372036854775808.0.0", -1},
		{"v9223372036854775808.0.0", "v9223372036854775807.0.0", 1},
		{"v9223372036854775808.0.0", "v9223372036854775808.0.0", 0},
		{"v0.0.9223372036854775808", "v0.0.9223372036854775807", 1},
		// Long decimal strings: equal lengths order digit-by-digit and a
		// longer component is always the larger number.
		{"v" + strings.Repeat("9", 40) + ".0.0", "v" + strings.Repeat("9", 40) + ".0.0", 0},
		{"v" + strings.Repeat("9", 40) + ".0.0", "v" + strings.Repeat("8", 40) + ".0.0", 1},
		{"v" + strings.Repeat("8", 40) + ".0.0", "v" + strings.Repeat("9", 40) + ".0.0", -1},
		{"v1" + strings.Repeat("0", 39) + ".0.0", "v" + strings.Repeat("9", 39) + ".0.0", 1},
		// Major outranks an extreme minor/patch and vice versa.
		{"v1.18446744073709551615.18446744073709551615", "v2.0.0", -1},
		{"v2.0.0", "v1.18446744073709551615.18446744073709551615", 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func setIdentity(t *testing.T, v, commit string) {
	t.Helper()
	oldV, oldC := Version, Commit
	Version, Commit = v, commit
	t.Cleanup(func() { Version, Commit = oldV, oldC })
}

func TestStringClassification(t *testing.T) {
	cases := []struct {
		v, commit string
		want      string
	}{
		{"v0.0.3", "abc1234", "v0.0.3"},
		{"v0.0.3", "", "v0.0.3"},
		{"dev", "abc1234", "dev (abc1234)"},
		{"dev", "", "dev"},
		{"", "", "dev"},
		{"df5eab6-dirty", "df5eab6", "dev (df5eab6)"},
		{"v0.0.2-5-gabc1234-dirty", "abc1234", "dev (abc1234)"},
	}
	for _, c := range cases {
		setIdentity(t, c.v, c.commit)
		if got := String(); got != c.want {
			t.Errorf("String() with Version=%q Commit=%q = %q, want %q", c.v, c.commit, got, c.want)
		}
	}
}

func TestLineShapes(t *testing.T) {
	plat := runtime.GOOS + "/" + runtime.GOARCH
	cases := []struct {
		v, commit string
		want      string
	}{
		{"v0.0.3", "abc1234", "lightcode v0.0.3 (" + plat + ")"},
		{"dev", "abc1234", "lightcode dev (abc1234, " + plat + ")"},
		{"dev", "", "lightcode dev (" + plat + ")"},
	}
	for _, c := range cases {
		setIdentity(t, c.v, c.commit)
		if got := Line(); got != c.want {
			t.Errorf("Line() with Version=%q Commit=%q = %q, want %q", c.v, c.commit, got, c.want)
		}
	}
}

func TestCurrentReportsRawValues(t *testing.T) {
	setIdentity(t, "v9.9.9", "abcdef1")
	info := Current()
	if info.Version != "v9.9.9" || info.Commit != "abcdef1" {
		t.Fatalf("Current() = %+v, want raw stamped values", info)
	}
	if info.OS == "" || info.Arch == "" {
		t.Fatalf("Current() must fill os/arch, got %+v", info)
	}

	setIdentity(t, "dev", "")
	info = Current()
	if info.Version != "dev" || info.Commit != "" {
		t.Fatalf("Current() = %+v, want dev with empty commit", info)
	}
}
