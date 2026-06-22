package adaptation

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed data/bundled.json
var bundledFS embed.FS

// bundledDoc is the wire shape of the bundled data file. It is a generic
// schema: no model ids, no vendor names, no adaptation names. Adding a new
// model adaptation is a data-only change to data/bundled.json; no Go edit.
type bundledDoc struct {
	Version  int             `json:"version"`
	Defaults bundledDefaults `json:"defaults"`
	Entries  []bundledEntry  `json:"entries"`
}

type bundledDefaults struct {
	ToolDescriptionReplacements map[string]string `json:"tool_description_replacements,omitempty"`
}

type bundledEntry struct {
	Pattern                     string            `json:"pattern"`
	Name                        string            `json:"name"`
	ExcludeTools                []string          `json:"exclude_tools,omitempty"`
	IncludeTools                []string          `json:"include_tools,omitempty"`
	Additions                   map[string]string `json:"additions,omitempty"`
	ToolDescriptionReplacements map[string]string `json:"tool_description_replacements,omitempty"`
}

// validSectionIDs lists the user-overridable section ids the prompt
// assembler accepts. An additions key outside this set is rejected at
// startup so a typo in the data file is caught immediately.
var validSectionIDs = map[string]bool{
	"safety":         true,
	"tone":           true,
	"task_execution": true,
	"language":       true,
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
	if err := validateBundledDefaults(doc.Defaults); err != nil {
		panic(fmt.Sprintf("adaptation: bundled data: %v", err))
	}
	defaultToolDescriptionReplacements = cloneStringMap(doc.Defaults.ToolDescriptionReplacements)
	out := make([]entry, 0, len(doc.Entries))
	seen := make(map[string]bool)
	for i, e := range doc.Entries {
		if err := validateBundledEntry(i, e, doc.Defaults); err != nil {
			panic(fmt.Sprintf("adaptation: bundled data: %v", err))
		}
		key := e.Pattern + "\x00" + e.Name
		if seen[key] {
			panic(fmt.Sprintf("adaptation: bundled data: duplicate row %q / %q", e.Pattern, e.Name))
		}
		seen[key] = true
		out = append(out, entry{pattern: e.Pattern, adapt: Adaptation{
			Name:                        e.Name,
			ExcludeTools:                append([]string(nil), e.ExcludeTools...),
			IncludeTools:                append([]string(nil), e.IncludeTools...),
			Additions:                   cloneStringMap(e.Additions),
			ToolDescriptionReplacements: cloneStringMap(e.ToolDescriptionReplacements),
		}})
	}
	return out
}

func validateBundledDefaults(d bundledDefaults) error {
	for k, v := range d.ToolDescriptionReplacements {
		if err := validatePlaceholderKey(k); err != nil {
			return fmt.Errorf("defaults.tool_description_replacements: %w", err)
		}
		if v == "" {
			return fmt.Errorf("defaults.tool_description_replacements[%q] is empty", k)
		}
	}
	return nil
}

func validateBundledEntry(i int, e bundledEntry, defaults bundledDefaults) error {
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
	for j, t := range e.IncludeTools {
		if t == "" {
			return fmt.Errorf("entry %d (%q): include_tools[%d] is empty", i, e.Pattern, j)
		}
	}
	keys := make([]string, 0, len(e.Additions))
	for k := range e.Additions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !validSectionIDs[k] {
			return fmt.Errorf("entry %d (%q): additions key %q is not a user-overridable section id (want safety/tone/task_execution/language)", i, e.Pattern, k)
		}
		if e.Additions[k] == "" {
			return fmt.Errorf("entry %d (%q): additions[%q] is empty", i, e.Pattern, k)
		}
	}
	keys = keys[:0]
	for k := range e.ToolDescriptionReplacements {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := validatePlaceholderKey(k); err != nil {
			return fmt.Errorf("entry %d (%q): tool_description_replacements key %q is invalid: %w", i, e.Pattern, k, err)
		}
		if _, ok := defaults.ToolDescriptionReplacements[k]; !ok {
			return fmt.Errorf("entry %d (%q): tool_description_replacements key %q has no default replacement", i, e.Pattern, k)
		}
	}
	return nil
}

func validatePlaceholderKey(k string) error {
	if k == "" {
		return fmt.Errorf("placeholder key is empty")
	}
	if len(k) < 3 || k[0] != '<' || k[len(k)-1] != '>' {
		return fmt.Errorf("placeholder must be wrapped in <...>")
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
