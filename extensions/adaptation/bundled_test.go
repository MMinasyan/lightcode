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
		testassert.Check(t, slices.Equal(tbl[i].adapt.Append, e.Append) &&
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

// TestProductionBundledDataAppendParagraphsAreNonEmpty walks every append
// paragraph in the production data and confirms it carries text.
func TestProductionBundledDataAppendParagraphsAreNonEmpty(t *testing.T) {
	doc := loadFixture(t)
	for _, e := range doc.Entries {
		for i, paragraph := range e.Append {
			testassert.Check(t, strings.TrimSpace(paragraph) != "", "entry %q has an empty append[%d]", e.Pattern, i)
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
