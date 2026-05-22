package shellparse

import (
	"fmt"
	"strings"
	"unicode"
)

// Segment is one shell command segment split on control operators.
type Segment struct {
	Text            string
	Normalized      string
	Separator       string
	Argv            []string
	Redirections    []Redirection
	UnsafeExpansion bool
}

// Redirection is a shell redirection operator seen outside quotes.
type Redirection struct {
	Raw       string
	SafeFDDup bool
}

// Parse scans a /bin/sh-style command line into command segments.
// It intentionally models only the shell surface Lightcode needs for
// permission checks: command boundaries, argv normalization, substitutions,
// expansions, and redirection operators.
func Parse(command string) ([]Segment, error) {
	p := parser{runes: []rune(command)}
	if err := p.scan(); err != nil {
		return nil, err
	}
	return p.segments, nil
}

type parser struct {
	runes []rune

	segments []Segment
	segment  Segment
	raw      strings.Builder
	arg      strings.Builder

	inSingle bool
	inDouble bool
	hadArg   bool
}

func (p *parser) scan() error {
	for i := 0; i < len(p.runes); i++ {
		r := p.runes[i]

		if p.inSingle {
			p.raw.WriteRune(r)
			if r == '\'' {
				p.inSingle = false
				p.hadArg = true
				continue
			}
			p.arg.WriteRune(r)
			p.hadArg = true
			continue
		}

		if p.inDouble {
			if r == '\\' {
				if p.skipLineContinuation(&i) {
					continue
				}
				p.raw.WriteRune(r)
				if i+1 >= len(p.runes) {
					p.arg.WriteRune(r)
					p.hadArg = true
					continue
				}
				next := p.runes[i+1]
				if isDoubleQuoteEscapedRune(next) {
					i++
					p.raw.WriteRune(next)
					p.arg.WriteRune(next)
					p.hadArg = true
					continue
				}
				p.arg.WriteRune(r)
				p.hadArg = true
				continue
			}

			switch r {
			case '"':
				p.raw.WriteRune(r)
				p.inDouble = false
				p.hadArg = true
			case '`':
				return fmt.Errorf("backtick command substitution not allowed")
			case '$':
				p.raw.WriteRune(r)
				if i+1 < len(p.runes) && p.runes[i+1] == '(' {
					return fmt.Errorf("$() command substitution not allowed")
				}
				p.segment.UnsafeExpansion = true
				p.arg.WriteRune(r)
				p.hadArg = true
			default:
				p.raw.WriteRune(r)
				p.arg.WriteRune(r)
				p.hadArg = true
			}
			continue
		}

		if r == '\\' {
			if p.skipLineContinuation(&i) {
				continue
			}
			p.raw.WriteRune(r)
			if i+1 >= len(p.runes) {
				p.arg.WriteRune(r)
				p.hadArg = true
				continue
			}
			i++
			next := p.runes[i]
			p.raw.WriteRune(next)
			p.arg.WriteRune(next)
			p.hadArg = true
			continue
		}

		switch {
		case r == '\'':
			p.raw.WriteRune(r)
			p.inSingle = true
			p.hadArg = true
		case r == '"':
			p.raw.WriteRune(r)
			p.inDouble = true
			p.hadArg = true
		case unicode.IsSpace(r):
			if r == '\n' || r == '\r' {
				p.emitArg()
				p.emitSegment(string(r))
				continue
			}
			p.raw.WriteRune(r)
			p.emitArg()
		case isBoundaryStart(p.runes, i):
			p.emitArg()
			separator := string(r)
			if i+1 < len(p.runes) && ((r == '&' && p.runes[i+1] == '&') || (r == '|' && p.runes[i+1] == '|')) {
				separator += string(p.runes[i+1])
				i++
			}
			p.emitSegment(separator)
		case r == '`':
			return fmt.Errorf("backtick command substitution not allowed")
		case r == '$':
			if i+1 < len(p.runes) && p.runes[i+1] == '(' {
				return fmt.Errorf("$() command substitution not allowed")
			}
			p.segment.UnsafeExpansion = true
			p.raw.WriteRune(r)
			p.arg.WriteRune(r)
			p.hadArg = true
		case isRedirectionStart(p.runes, i):
			i = p.consumeRedirection(i)
		default:
			if isUnsafeExpansionRune(r) || (r == '~' && !p.hadArg) {
				p.segment.UnsafeExpansion = true
			}
			p.raw.WriteRune(r)
			p.arg.WriteRune(r)
			p.hadArg = true
		}
	}

	if p.inSingle || p.inDouble {
		return fmt.Errorf("unterminated quote")
	}
	p.emitArg()
	p.emitSegment("")
	return nil
}

func (p *parser) emitArg() {
	if !p.hadArg {
		return
	}
	p.segment.Argv = append(p.segment.Argv, p.arg.String())
	p.arg.Reset()
	p.hadArg = false
}

func (p *parser) emitSegment(separator string) {
	text := strings.TrimSpace(p.raw.String())
	if text != "" || len(p.segment.Argv) > 0 || len(p.segment.Redirections) > 0 || separator != "" {
		p.segment.Text = text
		p.segment.Normalized = strings.Join(p.segment.Argv, " ")
		p.segment.Separator = separator
		p.segments = append(p.segments, p.segment)
	}
	p.segment = Segment{}
	p.raw.Reset()
	p.arg.Reset()
	p.hadArg = false
}

func (p *parser) consumeRedirection(i int) int {
	if p.hadArg {
		if allDigits(p.arg.String()) {
			p.arg.Reset()
			p.hadArg = false
		} else {
			p.emitArg()
		}
	}

	start := i
	opEnd := redirectionOpEnd(p.runes, i)
	for j := i; j < opEnd; j++ {
		p.raw.WriteRune(p.runes[j])
	}
	targetStart := opEnd
	for targetStart < len(p.runes) && (p.runes[targetStart] == ' ' || p.runes[targetStart] == '\t') {
		p.raw.WriteRune(p.runes[targetStart])
		targetStart++
	}
	targetEnd := targetStart
	for targetEnd < len(p.runes) {
		r := p.runes[targetEnd]
		if unicode.IsSpace(r) || isBoundaryStart(p.runes, targetEnd) {
			break
		}
		p.raw.WriteRune(r)
		targetEnd++
	}

	raw := string(p.runes[start:opEnd])
	if targetEnd > targetStart {
		raw += string(p.runes[targetStart:targetEnd])
	}
	p.segment.Redirections = append(p.segment.Redirections, Redirection{
		Raw:       raw,
		SafeFDDup: isSafeFDDup(raw),
	})
	return targetEnd - 1
}

func isBoundaryStart(runes []rune, i int) bool {
	r := runes[i]
	if r == ';' || r == '|' {
		return true
	}
	if r == '&' {
		if i+1 < len(runes) && runes[i+1] == '>' {
			return false
		}
		return true
	}
	return false
}

func isRedirectionStart(runes []rune, i int) bool {
	r := runes[i]
	if r == '<' || r == '>' {
		return true
	}
	return r == '&' && i+1 < len(runes) && runes[i+1] == '>'
}

func redirectionOpEnd(runes []rune, i int) int {
	switch runes[i] {
	case '&':
		if i+1 < len(runes) && runes[i+1] == '>' {
			if i+2 < len(runes) && runes[i+2] == '>' {
				return i + 3
			}
			return i + 2
		}
	case '>':
		if i+1 < len(runes) {
			switch runes[i+1] {
			case '>', '|', '&':
				return i + 2
			}
		}
	case '<':
		if i+1 < len(runes) {
			if runes[i+1] == '<' {
				if i+2 < len(runes) && runes[i+2] == '<' {
					return i + 3
				}
				return i + 2
			}
			if runes[i+1] == '>' {
				return i + 2
			}
		}
	}
	return i + 1
}

func isSafeFDDup(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	idx := strings.Index(raw, ">&")
	if idx < 0 {
		return false
	}
	prefix := raw[:idx]
	target := raw[idx+2:]
	if prefix != "" && !allDigits(prefix) {
		return false
	}
	return target == "-" || allDigits(target)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isUnsafeExpansionRune(r rune) bool {
	switch r {
	case '*', '?', '[', '{':
		return true
	default:
		return false
	}
}

func isDoubleQuoteEscapedRune(r rune) bool {
	switch r {
	case '$', '`', '"', '\\':
		return true
	default:
		return false
	}
}

func (p *parser) skipLineContinuation(i *int) bool {
	if *i+1 >= len(p.runes) {
		return false
	}
	next := p.runes[*i+1]
	switch next {
	case '\n':
		*i = *i + 1
		return true
	case '\r':
		*i = *i + 1
		if *i+1 < len(p.runes) && p.runes[*i+1] == '\n' {
			*i = *i + 1
		}
		return true
	default:
		return false
	}
}
