package adaptation

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
)

//go:embed data/bundled.json
var bundledFS embed.FS

// bundledDoc is the wire shape of the bundled data file. It is a generic
// schema: no model ids, no vendor names, no adaptation names. Adding a new
// model adaptation is a data-only change to data/bundled.json; no Go edit.
type bundledDoc struct {
	Version int            `json:"version"`
	Entries []bundledEntry `json:"entries"`
}

type bundledEntry struct {
	Pattern                     string            `json:"pattern"`
	Name                        string            `json:"name"`
	ExcludeTools                []string          `json:"exclude_tools,omitempty"`
	Append                      []string          `json:"append,omitempty"`
	ToolDescriptionReplacements map[string]string `json:"tool_description_replacements,omitempty"`
}

// loadBundledTable reads, decodes, and validates the bundled adaptation
// data, returning the generic []entry the matcher walks. Called once at
// package init; a malformed data file is a developer bug and panics.
func loadBundledTable() []entry {
	raw, err := bundledFS.ReadFile("data/bundled.json")
	if err != nil {
		panic(fmt.Sprintf("adaptation: read bundled data: %v", err))
	}
	var doc bundledDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		panic(fmt.Sprintf("adaptation: decode bundled data: %v", err))
	}
	if doc.Version != 1 {
		panic(fmt.Sprintf("adaptation: unsupported bundled data version %d, want 1", doc.Version))
	}
	if len(doc.Entries) == 0 {
		panic("adaptation: bundled data has no entries")
	}
	out := make([]entry, 0, len(doc.Entries))
	seen := make(map[string]bool)
	for i, e := range doc.Entries {
		if err := validateBundledEntry(i, e); err != nil {
			panic(fmt.Sprintf("adaptation: bundled data: %v", err))
		}
		key := e.Pattern + "\x00" + e.Name
		if seen[key] {
			panic(fmt.Sprintf("adaptation: bundled data: duplicate row %q / %q", e.Pattern, e.Name))
		}
		seen[key] = true
		out = append(out, entry{pattern: e.Pattern, adapt: Adaptation{
			ExcludeTools:                append([]string(nil), e.ExcludeTools...),
			Append:                      append([]string(nil), e.Append...),
			ToolDescriptionReplacements: cloneStringMap(e.ToolDescriptionReplacements),
		}})
	}
	return out
}

func validateBundledEntry(i int, e bundledEntry) error {
	if e.Pattern == "" {
		return fmt.Errorf("entry %d: pattern is empty", i)
	}
	if e.Name == "" {
		return fmt.Errorf("entry %d (%q): name is empty", i, e.Pattern)
	}
	if !hasAnchor(e.Pattern) {
		return fmt.Errorf("entry %d (%q): pattern is a run of '*' (would over-match)", i, e.Pattern)
	}
	for j, t := range e.ExcludeTools {
		if t == "" {
			return fmt.Errorf("entry %d (%q): exclude_tools[%d] is empty", i, e.Pattern, j)
		}
	}
	if _, empty := e.ToolDescriptionReplacements[""]; empty {
		return fmt.Errorf("entry %d (%q): tool_description_replacements key is empty", i, e.Pattern)
	}
	return nil
}

// hasAnchor reports whether the glob pattern contains at least one
// non-'*' byte. A pattern of only '*' would over-match the entire id
// space and is almost always a typo.
func hasAnchor(pattern string) bool {
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '*' {
			return true
		}
	}
	return false
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// bundledTable is the production matcher table, populated at package init
// from the bundled data file. Generic code: the matcher does not know what
// any pattern or name says.
var bundledTable = loadBundledTable()
