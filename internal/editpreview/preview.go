package editpreview

import (
	"encoding/json"
	"strconv"
	"strings"
)

const (
	KindContext = "context"
	KindRemove  = "remove"
	KindAdd     = "add"
)

type Preview struct {
	Hunks []Hunk `json:"hunks"`
}

type Hunk struct {
	Rows []Row `json:"rows"`
}

type Row struct {
	Kind    string `json:"kind"`
	OldLine int    `json:"oldLine,omitempty"`
	NewLine int    `json:"newLine,omitempty"`
	Text    string `json:"text"`
}

func MetadataFromArgs(args, result string) map[string]any {
	var params map[string]any
	if json.Unmarshal([]byte(args), &params) != nil {
		return nil
	}
	return MetadataFromParams(params, result)
}

func MetadataFromParams(params map[string]any, result string) map[string]any {
	oldString, _ := params["old_string"].(string)
	newString, _ := params["new_string"].(string)
	if (oldString == "" && newString == "") || oldString == newString {
		return nil
	}

	if preview := Build(oldString, newString, result); preview != nil {
		return map[string]any{"edit_preview": preview}
	}
	return nil
}

func FromMetadata(metadata map[string]any) (*Preview, bool) {
	if metadata == nil {
		return nil, false
	}
	value, ok := metadata["edit_preview"]
	if !ok || value == nil {
		return nil, false
	}

	switch p := value.(type) {
	case *Preview:
		return p, p != nil && len(p.Hunks) > 0
	case Preview:
		return &p, len(p.Hunks) > 0
	}

	data, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var preview Preview
	if json.Unmarshal(data, &preview) != nil || len(preview.Hunks) == 0 {
		return nil, false
	}
	return &preview, true
}

func Build(oldString, newString, result string) *Preview {
	starts := parseStartLines(result)
	if len(starts) == 0 {
		return nil
	}

	oldLines := splitLines(oldString)
	newLines := splitLines(newString)
	if len(oldLines) == 0 && len(newLines) == 0 {
		return nil
	}

	preview := &Preview{Hunks: make([]Hunk, 0, len(starts))}
	for _, start := range starts {
		rows := diffRows(oldLines, newLines, start)
		if len(rows) > 0 {
			preview.Hunks = append(preview.Hunks, Hunk{Rows: rows})
		}
	}
	if len(preview.Hunks) == 0 {
		return nil
	}
	return preview
}

func DisplayLine(row Row) int {
	if row.Kind == KindRemove {
		return row.OldLine
	}
	return row.NewLine
}

func Marker(row Row) string {
	switch row.Kind {
	case KindRemove:
		return "-"
	case KindAdd:
		return "+"
	default:
		return " "
	}
}

// parseStartLines depends on edit_file's O(1) result string format:
// "Edited <path> (<n> replacement(s), lines <ranges>)."
// Keep this in sync with tool.ApplyEdit's summary format.
func parseStartLines(result string) []int {
	idx := strings.LastIndex(result, "lines ")
	if idx == -1 {
		return nil
	}
	tail := result[idx+len("lines "):]
	if end := strings.Index(tail, ")"); end >= 0 {
		tail = tail[:end]
	}
	tail = strings.TrimSuffix(strings.TrimSpace(tail), ".")

	var starts []int
	for _, part := range strings.Split(tail, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if dash := strings.Index(part, "-"); dash >= 0 {
			part = part[:dash]
		}
		start, err := strconv.Atoi(part)
		if err == nil && start > 0 {
			starts = append(starts, start)
		}
	}
	return starts
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if strings.HasSuffix(s, "\n") {
		lines = lines[:len(lines)-1]
	}
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

func diffRows(oldLines, newLines []string, startLine int) []Row {
	n := len(oldLines)
	m := len(newLines)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	rows := make([]Row, 0, n+m)
	oldLine := startLine
	newLine := startLine
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && oldLines[i] == newLines[j]:
			rows = append(rows, Row{Kind: KindContext, OldLine: oldLine, NewLine: newLine, Text: oldLines[i]})
			i++
			j++
			oldLine++
			newLine++
		case i < n && (j == m || dp[i+1][j] >= dp[i][j+1]):
			rows = append(rows, Row{Kind: KindRemove, OldLine: oldLine, Text: oldLines[i]})
			i++
			oldLine++
		case j < m:
			rows = append(rows, Row{Kind: KindAdd, NewLine: newLine, Text: newLines[j]})
			j++
			newLine++
		}
	}
	return rows
}
