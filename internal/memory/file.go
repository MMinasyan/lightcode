package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type Section struct {
	Name    string
	Content string
}

func WriteMemoryFile(dir, title, content string) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	ts := now.Format("20060102-150405")
	slug := slugify(title)
	if slug == "" {
		slug = "untitled"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	name := fmt.Sprintf("%s-%s.md", ts, slug)
	fp := filepath.Join(dir, name)

	createdAt := now.Format(time.RFC3339)
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", yamlQuote(title))
	fmt.Fprintf(&b, "created_at: %s\n", createdAt)
	b.WriteString("---\n\n")
	b.WriteString(content)
	b.WriteString("\n")

	if err := os.WriteFile(fp, []byte(b.String()), 0644); err != nil {
		return "", err
	}
	return fp, nil
}

func ReadMemoryFile(path string) (title, content, createdAt string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", err
	}
	s := string(data)

	if !strings.HasPrefix(s, "---\n") {
		return "", s, "", nil
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		return "", s, "", nil
	}
	frontmatter := s[4 : 4+end]
	body := strings.TrimLeft(s[4+end+5:], "\n")

	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "title:") {
			title = strings.TrimSpace(strings.TrimPrefix(line, "title:"))
			title = strings.Trim(title, `"'`)
		} else if strings.HasPrefix(line, "created_at:") {
			createdAt = strings.TrimSpace(strings.TrimPrefix(line, "created_at:"))
		}
	}
	return title, body, createdAt, nil
}

var sectionRe = regexp.MustCompile(`(?m)^## (.+)$`)

func SplitSummary(summary string) []Section {
	locs := sectionRe.FindAllStringSubmatchIndex(summary, -1)
	if len(locs) == 0 {
		if s := strings.TrimSpace(summary); s != "" {
			return []Section{{Name: "Summary", Content: s}}
		}
		return nil
	}

	var sections []Section
	for i, loc := range locs {
		name := summary[loc[2]:loc[3]]
		start := loc[1]
		var end int
		if i+1 < len(locs) {
			end = locs[i+1][0]
		} else {
			end = len(summary)
		}
		content := strings.TrimSpace(summary[start:end])
		if content != "" {
			sections = append(sections, Section{Name: name, Content: content})
		}
	}
	return sections
}

func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func yamlQuote(s string) string {
	if strings.ContainsAny(s, ":#{}[]|>&*!%@`\"'\n\r\\") {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		s = strings.ReplaceAll(s, "\n", `\n`)
		s = strings.ReplaceAll(s, "\r", `\r`)
		return `"` + s + `"`
	}
	return s
}
