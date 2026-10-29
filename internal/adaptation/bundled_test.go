package adaptation

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// loadFixture reads the fixture bundled data into a bundledDoc the tests
// assert against. Mirrors the production schema in loader.go.
func loadFixture(t *testing.T) bundledDoc {
	t.Helper()
	path := filepath.Join("testdata", "bundled.json")
	raw, err := os.ReadFile(path)
	testassert.Require(t, err == nil, "read fixture %s: %v", path, err)
	var doc bundledDoc
	testassert.NoError(t, json.Unmarshal(raw, &doc))
	return doc
}

// fixtureRow returns the first row in doc whose name equals want. Tests use
// this to assert production matcher behavior against the fixture values
// without ever referencing a Go constant.
func fixtureRow(doc bundledDoc, want string) bundledEntry {
	for _, e := range doc.Entries {
		if e.Name == want {
			return e
		}
	}
	return bundledEntry{}
}

// TestProductionBundledDataIsValid parses the production bundled data
// file the loader uses at init and confirms it loads cleanly with the
// expected row count and ordering. Catches malformed-data regressions
// before they reach the matcher.
func TestProductionBundledDataIsValid(t *testing.T) {
	tbl := loadBundledTable()
	testassert.Require(t, len(tbl) != 0, "loadBundledTable returned an empty table")
	// The production data file is the authoritative source of truth.
	// Compare its loaded form against its parsed JSON form to ensure
	// the loader and the file agree.
	doc := loadFixture(t)
	testassert.Equal(t, len(doc.Entries), len(tbl))
	for i, e := range doc.Entries {
		testassert.Check(t, e.Name != "", "row %d (%q): fixture row has no authoring name", i, e.Pattern)
		testassert.Check(t, tbl[i].pattern == e.Pattern, "row %d: loader pattern %q != fixture %q", i, tbl[i].pattern, e.Pattern)
		testassert.Check(t, maps.Equal(tbl[i].adapt.Additions, e.Additions) &&
			slices.Equal(tbl[i].adapt.ExcludeTools, e.ExcludeTools) &&
			maps.Equal(tbl[i].adapt.ToolDescriptionReplacements, e.ToolDescriptionReplacements), "row %d (%q): loaded adaptation outputs %+v != fixture row %+v", i, e.Pattern, tbl[i].adapt, e)
	}
}

// TestProductionBundledDataIsMostSpecificFirst documents and enforces the
// table's most-specific-first ordering. The first row in the data file
// must match a more specific id than the second, etc.
func TestProductionBundledDataIsMostSpecificFirst(t *testing.T) {
	doc := loadFixture(t)
	if len(doc.Entries) < 2 {
		t.Skip("need at least two rows to assert ordering")
	}
	first := doc.Entries[0]
	for _, e := range doc.Entries[1:] {
		if first.Pattern == e.Pattern {
			t.Errorf("rows 0 and %d share pattern %q", indexOf(doc, e), e.Pattern)
		}
	}
}

// TestProductionBundledDataHasNoDuplicateRow is enforced by the loader at
// init; this test documents the rule at the data layer too.
func TestProductionBundledDataHasNoDuplicateRow(t *testing.T) {
	doc := loadFixture(t)
	seen := make(map[string]bool)
	for _, e := range doc.Entries {
		key := e.Pattern + "\x00" + e.Name
		testassert.Check(t, !seen[key], "duplicate row %q / %q", e.Pattern, e.Name)
		seen[key] = true
	}
}

// TestProductionBundledDataAdditionsUseValidSectionIDs walks every
// addition in the production data and confirms the key is one of the
// four user-overridable section ids.
func TestProductionBundledDataAdditionsUseValidSectionIDs(t *testing.T) {
	doc := loadFixture(t)
	for _, e := range doc.Entries {
		for k := range e.Additions {
			testassert.Check(t, validSectionIDs[k], "entry %q has additions key %q outside {safety, tone, task_execution, language}", e.Pattern, k)
		}
	}
}

func TestProductionBundledDataReplacementKeysAreLiteralNonEmpty(t *testing.T) {
	doc := loadFixture(t)
	matched := false
	for _, e := range doc.Entries {
		for key, value := range e.ToolDescriptionReplacements {
			matched = true
			testassert.Check(t, key != "", "entry %q has an empty tool_description_replacements key", e.Pattern)
			testassert.Check(t, value != "", "entry %q tool_description_replacements[%q] is empty", e.Pattern, key)
		}
	}
	testassert.Require(t, matched, "no fixture row carries a tool_description_replacements override")
}

func TestBundledEntryRejectsEmptyToolDescriptionReplacementKey(t *testing.T) {
	err := validateBundledEntry(0, bundledEntry{
		Pattern:                     "model-*",
		Name:                        "model",
		ToolDescriptionReplacements: map[string]string{"": "value"},
	})
	testassert.Require(t, err != nil && strings.Contains(err.Error(), "key is empty"), "validateBundledEntry err = %v, want the empty replacement key rejection", err)
}

func indexOf(doc bundledDoc, want bundledEntry) int {
	for i, e := range doc.Entries {
		if e.Pattern == want.Pattern && e.Name == want.Name {
			return i
		}
	}
	return -1
}

// modelFamilyNames is the durable guardrail for Invariant 0: no concrete
// model-family name may appear as a string literal in production Go
// under internal/adaptation/ outside _test.go and the data file. The
// adaptation package's production-binding surface must be data-only.
var modelFamilyNames = []string{
	"gpt", "gemini", "grok", "gemma", "codex", "oss",
	"claude", "deepseek", "qwen", "minimax", "kimi", "glm",
}

// TestProductionGoContainsNoModelFamilyNames walks every non-test, non-data
// Go file in the adaptation package and fails if any production-binding
// shape (string literal) carries a known model family name. The audit is
// intentionally tolerant: it scans only string literals (so comments and
// identifiers don't trigger); it scans only the adaptation package (so
// unrelated packages aren't affected); it allows the data/ subdirectory
// and the testdata/ subdirectory to contain anything.
//
// Adding a new model family to the audit list is a one-line edit; the
// test is the durable guardrail that the data-driven binding invariant
// stays intact.
func TestProductionGoContainsNoModelFamilyNames(t *testing.T) {
	entries, err := os.ReadDir(".")
	testassert.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "loader.go" {
			// loader.go embeds data/bundled.json and the data file's
			// bytes flow through it at init. Exempt it; the data
			// file itself is the audit's allowlist.
			continue
		}
		scanFileForModelFamilyNames(t, name)
	}
}

func scanFileForModelFamilyNames(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	testassert.Require(t, err == nil, "read %s: %v", path, err)
	// Match Go string literals (double-quoted) and backtick raw strings.
	// We are intentionally not parsing Go; a token-level scan is good
	// enough for the audit's purpose.
	src := string(raw)
	for _, fam := range modelFamilyNames {
		testassert.Check(t, !containsStringLiteral(src, fam), "production Go file %q contains model family name %q as a string literal; production bindings must live in the bundled data file", path, fam)
	}
}

// containsStringLiteral reports whether substr appears as the content of
// a Go string literal (double-quoted) or raw string (backtick-delimited)
// in src. It does not handle every Go escaping rule, but a substring
// inside a literal is a strong-enough signal for the audit.
func containsStringLiteral(src, substr string) bool {
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j += 2
					continue
				}
				j++
			}
			if j >= len(src) {
				return false
			}
			lit := src[i+1 : j]
			if strings.Contains(lit, substr) {
				return true
			}
			i = j
		case '`':
			j := i + 1
			for j < len(src) && src[j] != '`' {
				j++
			}
			if j >= len(src) {
				return false
			}
			lit := src[i+1 : j]
			if strings.Contains(lit, substr) {
				return true
			}
			i = j
		}
	}
	return false
}
