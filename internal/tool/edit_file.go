package tool

import (
	"fmt"
	"os"
	"strings"
)

type editResult struct {
	Result         string
	UpdatedContent string
	LineRanges     string
	Count          int
}

func preflightEditSnapshotTarget(absPath string) error {
	if _, err := ensureRegularExistingTarget(absPath); err != nil {
		return fmt.Errorf("edit_file: %w", err)
	}
	f, err := openExistingMutationFile(absPath, os.O_RDWR)
	if err != nil {
		return fmt.Errorf("edit_file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("edit_file: stat: %w", err)
	}
	if err := ensureRegularFileInfo(absPath, info); err != nil {
		return fmt.Errorf("edit_file: %w", err)
	}
	return nil
}

// EditBufferResult holds the result of an edit applied to a string buffer.
type EditBufferResult struct {
	UpdatedContent string
	Summary        string
	LineRanges     string
	Count          int
}

// ApplyEdit performs a string replacement on content without touching the
// filesystem. It is the pure-buffer helper edit_file uses to compute the
// updated content and per-replacement line ranges before the write.
func ApplyEdit(content, oldString, newString string, replaceAll bool, path string) (*EditBufferResult, error) {
	n := strings.Count(content, oldString)
	if n == 0 {
		return nil, fmt.Errorf("edit_file: old_string not found in %s", path)
	}
	if n > 1 && !replaceAll {
		return nil, fmt.Errorf("edit_file: old_string matches %d locations in %s; use replace_all=true to replace all, or provide more context to make it unique", n, path)
	}

	var updated string
	var replCount int
	var lineRanges []string

	if replaceAll {
		updated = strings.ReplaceAll(content, oldString, newString)
		replCount = n
		remaining := content
		shift := 0
		for strings.Contains(remaining, oldString) {
			idx := strings.Index(remaining, oldString)
			absIdx := shift + idx
			startLine := countNewlinesUpTo(content, absIdx) + 1
			endLine := startLine + countNewlinesIn(newString)
			lineRanges = append(lineRanges, fmt.Sprintf("%d-%d", startLine, endLine))
			shift = shift + idx + len(oldString)
			remaining = content[shift:]
		}
	} else {
		idx := strings.Index(content, oldString)
		startLine := countNewlinesUpTo(content, idx) + 1
		endLine := startLine + countNewlinesIn(newString)
		updated = strings.Replace(content, oldString, newString, 1)
		replCount = 1
		lineRanges = append(lineRanges, fmt.Sprintf("%d-%d", startLine, endLine))
	}

	summary := fmt.Sprintf("Edited %s (%d replacement, lines %s).", path, replCount, strings.Join(lineRanges, ", "))
	if replCount > 1 {
		summary = fmt.Sprintf("Edited %s (%d replacements, lines %s).", path, replCount, strings.Join(lineRanges, ", "))
	}

	return &EditBufferResult{
		UpdatedContent: updated,
		Summary:        summary,
		LineRanges:     strings.Join(lineRanges, ", "),
		Count:          replCount,
	}, nil
}

func countNewlinesUpTo(s string, idx int) int {
	count := 0
	for i := 0; i < idx && i < len(s); i++ {
		if s[i] == '\n' {
			count++
		}
	}
	return count
}

func countNewlinesIn(s string) int {
	count := 0
	for _, c := range s {
		if c == '\n' {
			count++
		}
	}
	return count
}
