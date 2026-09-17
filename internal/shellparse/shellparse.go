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

// ParseSimple recognizes the restricted simple-command grammar: complete
// simple commands — a command word followed by literal words, joined by
// ; newline && || | or |& — and nothing else. Comments, command-position
// reserved words, assignment prefixes, parentheses/braces, expansions and
// substitutions, heredocs/here-strings and every redirection are outside
// the grammar. It shares the existing scanner in a private restricted mode;
// legacy Parse keeps its behavior.
//
// ok reports that the whole input belongs to the grammar. Segment.Text
// preserves source-token boundaries: only unquoted separating blanks are
// trimmed, never quoted or escaped trailing whitespace. Any input outside
// the grammar — or without at least one command — returns no segments, so
// no partial segment list ever escapes.
func ParseSimple(command string) ([]Segment, bool) {
	p := parser{runes: []rune(command), restricted: true, firstWordStart: -1, lastWordEnd: -1}
	if err := p.scan(); err != nil {
		return nil, false
	}
	if p.simpleRejected || len(p.segments) == 0 {
		return nil, false
	}
	switch last := p.segments[len(p.segments)-1].Separator; last {
	case "", ";", "\n":
	default:
		return nil, false // dangling &&, ||, | or |& at end of input
	}
	return p.segments, true
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

	// Restricted-mode state. restricted selects the grammar; simpleRejected
	// marks input outside it and is never reset within one scan. The word
	// indices are byte offsets into raw: firstWordStart/lastWordEnd bound
	// the segment's source-faithful Text.
	restricted     bool
	simpleRejected bool
	firstWordStart int
	lastWordEnd    int
	firstWordDone  bool
}

func (p *parser) scan() error {
	for i := 0; i < len(p.runes); i++ {
		r := p.runes[i]

		if p.inSingle {
			p.writeWordRune(r)
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
				if p.restricted && i+1 < len(p.runes) && p.runes[i+1] == '\r' {
					p.simpleRejected = true
					continue
				}
				if p.skipLineContinuation(&i) {
					continue
				}
				p.writeWordRune(r)
				if i+1 >= len(p.runes) {
					p.arg.WriteRune(r)
					p.hadArg = true
					continue
				}
				next := p.runes[i+1]
				if isDoubleQuoteEscapedRune(next) {
					i++
					p.writeWordRune(next)
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
				p.writeWordRune(r)
				p.inDouble = false
				p.hadArg = true
			case '`':
				if p.restricted {
					p.simpleRejected = true
					continue
				}
				return fmt.Errorf("backtick command substitution not allowed")
			case '$':
				if p.restricted {
					p.simpleRejected = true
					continue
				}
				p.raw.WriteRune(r)
				if i+1 < len(p.runes) && p.runes[i+1] == '(' {
					return fmt.Errorf("$() command substitution not allowed")
				}
				p.segment.UnsafeExpansion = true
				p.arg.WriteRune(r)
				p.hadArg = true
			default:
				p.writeWordRune(r)
				p.arg.WriteRune(r)
				p.hadArg = true
			}
			continue
		}

		if p.restricted {
			p.scanRestrictedRune(&i)
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
			if i+1 < len(p.runes) {
				next := p.runes[i+1]
				if (r == '&' && next == '&') || (r == '|' && (next == '|' || next == '&')) {
					separator += string(next)
					i++
				}
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
			next, err := p.consumeRedirection(i)
			if err != nil {
				return err
			}
			i = next
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
	if p.restricted && p.hadArg && !p.firstWordDone {
		// The open word is the segment's first: its raw source spans from
		// firstWordStart to the current raw end, so quote or escape
		// provenance in the source disqualifies the reserved and assignment
		// spellings.
		word := p.raw.String()[p.firstWordStart:]
		p.firstWordDone = true
		if reservedCommandWords[word] || isAssignmentPrefix(word) {
			p.simpleRejected = true
		}
	}
	if !p.hadArg {
		return
	}
	p.segment.Argv = append(p.segment.Argv, p.arg.String())
	p.arg.Reset()
	p.hadArg = false
}

func (p *parser) emitSegment(separator string) {
	if p.restricted {
		p.emitSegmentRestricted(separator)
		return
	}
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

// emitSegmentRestricted closes one segment under the restricted grammar.
// Its Text is the source span from the segment's first word character to
// its last — only unquoted separating blanks are trimmed. A blank segment
// is ignored when separated by a newline (blank line) or the end of input,
// and outside the grammar otherwise (empty semicolon or conditional/pipeline
// command).
func (p *parser) emitSegmentRestricted(separator string) {
	text := ""
	if p.firstWordStart >= 0 {
		raw := p.raw.String()
		text = raw[p.firstWordStart:p.lastWordEnd]
	}
	if text != "" {
		p.segment.Text = text
		p.segment.Normalized = strings.Join(p.segment.Argv, " ")
		p.segment.Separator = separator
		p.segments = append(p.segments, p.segment)
	} else if separator != "\n" && separator != "" {
		p.simpleRejected = true
	}
	p.segment = Segment{}
	p.raw.Reset()
	p.arg.Reset()
	p.hadArg = false
	p.firstWordStart = -1
	p.lastWordEnd = -1
	p.firstWordDone = false
}

// writeWordRune writes one word rune to the segment source, tracking word
// boundaries in restricted mode so Segment.Text can be cut from the source
// without trimming quoted or escaped content.
func (p *parser) writeWordRune(r rune) {
	if p.restricted {
		if p.firstWordStart < 0 {
			p.firstWordStart = p.raw.Len()
		}
	}
	p.raw.WriteRune(r)
	if p.restricted {
		p.lastWordEnd = p.raw.Len()
	}
}

// scanRestrictedRune consumes one unquoted rune under the restricted
// simple-command grammar, marking anything outside it via simpleRejected.
// Recognized shell whitespace is ASCII-only: space and tab separate words,
// newline separates commands, and carriage return or other Unicode
// whitespace uses fallback. Any unquoted redirection operator, comment
// start, parenthesis/brace, reserved syntax or expansion marks the input
// out of the grammar without partial parsing.
func (p *parser) scanRestrictedRune(i *int) {
	r := p.runes[*i]
	switch {
	case r == '\\':
		if *i+1 >= len(p.runes) {
			p.simpleRejected = true // trailing lone backslash
			return
		}
		next := p.runes[*i+1]
		switch next {
		case '\n':
			*i++ // line continuation keeps the current word open
		case '\r':
			p.simpleRejected = true
		default:
			p.writeWordRune(r)
			p.writeWordRune(next)
			p.arg.WriteRune(next)
			p.hadArg = true
			*i++
		}
	case r == '\'' || r == '"':
		p.writeWordRune(r)
		if r == '\'' {
			p.inSingle = true
		} else {
			p.inDouble = true
		}
		p.hadArg = true
	case r == ' ' || r == '\t':
		p.emitArg()
		p.raw.WriteRune(r)
	case r == '\n':
		p.emitArg()
		p.emitSegment("\n")
	case unicode.IsSpace(r):
		p.simpleRejected = true // carriage return and other Unicode whitespace
	case r == '#':
		if p.hadArg {
			// Mid-word # is an ordinary word character.
			p.writeWordRune(r)
			p.arg.WriteRune(r)
			p.hadArg = true
		} else {
			p.simpleRejected = true // comment start
		}
	case r == ';':
		p.emitArg()
		p.emitSegment(";")
	case r == '(' || r == ')' || r == '{' || r == '}':
		p.simpleRejected = true
	case r == '<' || r == '>':
		p.simpleRejected = true // any unquoted redirection
	case r == '&' && *i+1 < len(p.runes) && p.runes[*i+1] == '&':
		p.emitArg()
		*i++
		p.emitSegment("&&")
	case r == '&':
		p.simpleRejected = true // bare & is never conclusive
	case r == '|':
		p.emitArg()
		separator := "|"
		if *i+1 < len(p.runes) && (p.runes[*i+1] == '|' || p.runes[*i+1] == '&') {
			separator += string(p.runes[*i+1])
			*i++
		}
		p.emitSegment(separator)
	case r == '`' || r == '$' || r == '*' || r == '?' || r == '[':
		p.simpleRejected = true // substitution or expansion
	case r == '~' && !p.hadArg:
		p.simpleRejected = true // tilde expansion
	default:
		p.writeWordRune(r)
		p.arg.WriteRune(r)
		p.hadArg = true
	}
}

// reservedCommandWords are the shell reserved words that fall outside the
// restricted grammar when they form an unquoted command word. The check
// runs on the word's raw source, so quoted or escaped spellings — which the
// shell reads as ordinary command names — stay inside the grammar.
var reservedCommandWords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true,
	"case": true, "esac": true, "in": true, "function": true,
	"select": true, "coproc": true, "time": true, "!": true,
}

// isAssignmentPrefix reports whether a raw command word is a variable
// assignment prefix (NAME=...) at command position: an unquoted identifier
// followed by an unquoted =. A quote anywhere inside the identifier-or-equals
// prefix breaks the form, leaving an ordinary command word.
func isAssignmentPrefix(word string) bool {
	for i := 0; i < len(word); i++ {
		c := word[i]
		if c == '=' {
			return i > 0
		}
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return false
}

func (p *parser) consumeRedirection(i int) (int, error) {
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
		if p.inSingle {
			p.raw.WriteRune(r)
			if r == '\'' {
				p.inSingle = false
			}
			targetEnd++
			continue
		}
		if p.inDouble {
			switch r {
			case '"':
				p.inDouble = false
			case '`':
				return targetEnd, fmt.Errorf("backtick command substitution not allowed")
			case '$':
				if targetEnd+1 < len(p.runes) && p.runes[targetEnd+1] == '(' {
					return targetEnd, fmt.Errorf("$() command substitution not allowed")
				}
				p.segment.UnsafeExpansion = true
			}
			p.raw.WriteRune(r)
			targetEnd++
			continue
		}
		if unicode.IsSpace(r) || isBoundaryStart(p.runes, targetEnd) {
			break
		}
		if r == '\'' {
			p.inSingle = true
			p.raw.WriteRune(r)
			targetEnd++
			continue
		}
		if r == '"' {
			p.inDouble = true
			p.raw.WriteRune(r)
			targetEnd++
			continue
		}
		if r == '`' {
			return targetEnd, fmt.Errorf("backtick command substitution not allowed")
		}
		if r == '$' && targetEnd+1 < len(p.runes) && p.runes[targetEnd+1] == '(' {
			return targetEnd, fmt.Errorf("$() command substitution not allowed")
		}
		if isUnsafeExpansionRune(r) || r == '$' || (r == '~' && targetEnd == targetStart) {
			p.segment.UnsafeExpansion = true
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
	return targetEnd - 1, nil
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
