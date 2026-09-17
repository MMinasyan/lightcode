package tool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/MMinasyan/lightcode/internal/config"
)

// ReadFile implements the read_file tool with line-numbered output,
// pagination, byte caps, binary detection, mtime tracking, and
// re-read deduplication.
type ReadFile struct {
	cfg           config.ToolsConfig
	tracker       *FileTracker
	workspaceRoot string
}

// NewReadFile creates a ReadFile tool with the given config and tracker.
func NewReadFile(cfg config.ToolsConfig, tracker *FileTracker) *ReadFile {
	return &ReadFile{cfg: cfg, tracker: tracker}
}

func NewReadFileAtRoot(cfg config.ToolsConfig, tracker *FileTracker, workspaceRoot string) *ReadFile {
	return &ReadFile{cfg: cfg, tracker: tracker, workspaceRoot: workspaceRoot}
}

func (r *ReadFile) SetToolsConfig(cfg config.ToolsConfig) { r.cfg = cfg }

func (r *ReadFile) Name() string { return "read_file" }

func (r *ReadFile) Description() string {
	return `Reads a file from disk with line numbers.
- Results are returned with line numbers: each line is prefixed with its number and a tab (e.g. "1\tpackage main"). Line numbers start at 1.
- By default reads the first 500 lines. When you already know which part of the file you need, use offset and limit to read only that part.
- If you read a file that has not changed since the last time you read it, the tool returns a short notice instead of the full content. The earlier read in this conversation is still current.
- Do not use cat, head, tail, or sed via run_command to read files. Use this tool.`
}

func (r *ReadFile) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute or relative path to the file to read.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "1-indexed line number to start reading from.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of lines to read.",
			},
		},
		"required": []string{"path"},
	}
}

func (r *ReadFile) Execute(_ context.Context, params map[string]any) (string, error) {
	prepared, err := prepareReadCall(r.workspaceRoot, r.cfg, r.tracker, params)
	if err != nil {
		return "", err
	}
	return prepared.Execute(context.Background())
}

// formatReadOutput renders the line-numbered window for the requested
// offset/limit. Read windows are clamped to the remaining lines before any
// addition, so individually valid large integers cannot overflow the
// formatting or footer arithmetic.
func formatReadOutput(data []byte, offset, limit, byteCap, charCap int) (string, int) {
	lines := splitLines(data)
	totalLen := len(lines)

	start := offset - 1
	if start >= totalLen {
		return "", totalLen
	}
	remaining := totalLen - start
	end := totalLen
	if limit < remaining {
		end = start + limit
	}

	var buf bytes.Buffer
	byteUsed := 0

	for i := start; i < end; i++ {
		lineNum := i + 1
		line := lines[i]

		// Per-line truncation.
		if charCap > 0 && utf8.RuneCountInString(line) > charCap {
			line = truncateLineToRunes(line, charCap) + "... [truncated]"
		}

		lineStr := fmt.Sprintf("%d\t%s\n", lineNum, line)
		lineLen := len(lineStr)

		if byteCap > 0 && byteUsed+lineLen > byteCap {
			// Add footer about truncation.
			buf.WriteString(fmt.Sprintf("(Output truncated at %d bytes. Use offset=%d and limit to read specific portions.)",
				byteCap, offset))
			break
		}

		buf.WriteString(lineStr)
		byteUsed += lineLen
	}

	result := buf.String()
	// Trim trailing newline.
	if len(result) > 0 && result[len(result)-1] == '\n' {
		result = result[:len(result)-1]
	}
	return result, totalLen
}

// fileSuggestion is one scored candidate name for a missing file.
type fileSuggestion struct {
	name  string
	score int // Levenshtein distance
}

// scoreFileSuggestions consumes a directory listing sorted by name (as
// os.ReadDir returns it) and returns the nearest matches for the missing
// base name: deduplicated, nearest distance first, at most three.
func scoreFileSuggestions(entries []os.DirEntry, baseName string) []fileSuggestion {
	baseStem := stripExt(baseName)

	var candidates []fileSuggestion
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		stem := stripExt(name)

		// Common prefix ratio check.
		if commonPrefixRatio(stem, baseStem) >= 0.6 {
			candidates = append(candidates, fileSuggestion{name, levenshteinDist(name, baseName)})
			continue
		}
		// Levenshtein distance check.
		dist := levenshteinDist(name, baseName)
		if dist <= 2 {
			candidates = append(candidates, fileSuggestion{name, dist})
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// Dedup and sort.
	seen := make(map[string]bool)
	var unique []fileSuggestion
	for _, c := range candidates {
		if !seen[c.name] {
			seen[c.name] = true
			unique = append(unique, c)
		}
	}
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].score < unique[j].score
	})
	if len(unique) > 3 {
		unique = unique[:3]
	}
	return unique
}

// formatFileSuggestions renders the not-found message and the suggestion
// list for the supplied entries.
func formatFileSuggestions(notFoundPath, dirName string, suggestions []fileSuggestion) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("read_file: file not found: %s\n", notFoundPath))
	sb.WriteString("Did you mean:\n")
	for _, c := range suggestions {
		sb.WriteString(fmt.Sprintf("  %s/%s\n", dirName, c.name))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func splitLines(data []byte) []string {
	s := string(data)
	var lines []string
	for {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			lines = append(lines, s)
			break
		}
		line := s[:idx]
		lines = append(lines, line)
		s = s[idx+1:]
	}
	return lines
}

func isBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	// Check for null bytes.
	if bytes.IndexByte(data, 0) >= 0 {
		return true
	}
	// Check non-printable byte ratio.
	checkLen := len(data)
	if checkLen > 8000 {
		checkLen = 8000
	}
	nonPrintable := 0
	for _, b := range data[:checkLen] {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			nonPrintable++
		}
	}
	return float64(nonPrintable)/float64(checkLen) > 0.3
}

func stripExt(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}

func commonPrefixRatio(a, b string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	match := 0
	for i := 0; i < minLen; i++ {
		if a[i] == b[i] {
			match++
		} else {
			break
		}
	}
	return float64(match) / float64(minLen)
}

func levenshteinDist(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

func truncateLineToRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}
