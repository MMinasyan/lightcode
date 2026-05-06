package cli

import (
	"strings"
)

// keyMsg represents a key event from the terminal.
type keyMsg struct {
	Rune    rune
	Special keySpecial
}

type keySpecial int

const (
	keyNone keySpecial = iota
	keyEnter
	keyBackspace
	keyTab
	keyEscape
	keyUp
	keyDown
	keyLeft
	keyRight
	keyCtrlC
	keyCtrlD
	keyCtrlT
	keyDelete
	keyHome
	keyEnd
)

func parseInputBytes(b []byte) []keyMsg {
	var keys []keyMsg
	i := 0
	for i < len(b) {
		switch {
		case b[i] == 0x03:
			keys = append(keys, keyMsg{Special: keyCtrlC})
			i++
		case b[i] == 0x04:
			keys = append(keys, keyMsg{Special: keyCtrlD})
			i++
		case b[i] == 0x14:
			keys = append(keys, keyMsg{Special: keyCtrlT})
			i++
		case b[i] == '\r' || b[i] == '\n':
			keys = append(keys, keyMsg{Special: keyEnter})
			i++
		case b[i] == 0x7f || b[i] == 0x08:
			keys = append(keys, keyMsg{Special: keyBackspace})
			i++
		case b[i] == '\t':
			keys = append(keys, keyMsg{Special: keyTab})
			i++
		case b[i] == 0x1b:
			if i+2 < len(b) && b[i+1] == '[' {
				consumed := 3
				switch b[i+2] {
				case 'A':
					keys = append(keys, keyMsg{Special: keyUp})
				case 'B':
					keys = append(keys, keyMsg{Special: keyDown})
				case 'C':
					keys = append(keys, keyMsg{Special: keyRight})
				case 'D':
					keys = append(keys, keyMsg{Special: keyLeft})
				case 'H':
					keys = append(keys, keyMsg{Special: keyHome})
				case 'F':
					keys = append(keys, keyMsg{Special: keyEnd})
				case '3':
					if i+3 < len(b) && b[i+3] == '~' {
						keys = append(keys, keyMsg{Special: keyDelete})
						consumed = 4
					} else {
						keys = append(keys, keyMsg{Special: keyEscape})
					}
				default:
					keys = append(keys, keyMsg{Special: keyEscape})
				}
				i += consumed
			} else {
				keys = append(keys, keyMsg{Special: keyEscape})
				i++
			}
		default:
			keys = append(keys, keyMsg{Rune: rune(b[i])})
			i++
		}
	}
	return keys
}

// inputLine manages a single-line text input with cursor.
type inputLine struct {
	text   []rune
	cursor int
}

func newInputLine() *inputLine {
	return &inputLine{}
}

func (il *inputLine) String() string {
	return string(il.text)
}

func (il *inputLine) CursorText() string {
	if il.cursor <= 0 {
		return ""
	}
	if il.cursor >= len(il.text) {
		return string(il.text)
	}
	return string(il.text[:il.cursor])
}

func (il *inputLine) Insert(r rune) {
	il.text = append(il.text[:il.cursor], append([]rune{r}, il.text[il.cursor:]...)...)
	il.cursor++
}

func (il *inputLine) DeleteBack() bool {
	if il.cursor == 0 {
		return false
	}
	il.text = append(il.text[:il.cursor-1], il.text[il.cursor:]...)
	il.cursor--
	return true
}

func (il *inputLine) DeleteForward() bool {
	if il.cursor >= len(il.text) {
		return false
	}
	il.text = append(il.text[:il.cursor], il.text[il.cursor+1:]...)
	return true
}

func (il *inputLine) MoveLeft() bool {
	if il.cursor == 0 {
		return false
	}
	il.cursor--
	return true
}

func (il *inputLine) MoveRight() bool {
	if il.cursor >= len(il.text) {
		return false
	}
	il.cursor++
	return true
}

func (il *inputLine) MoveHome() {
	il.cursor = 0
}

func (il *inputLine) MoveEnd() {
	il.cursor = len(il.text)
}

func (il *inputLine) Set(text string) {
	il.text = []rune(text)
	il.cursor = len(il.text)
}

func (il *inputLine) Clear() {
	il.text = nil
	il.cursor = 0
}

type inputHistory struct {
	entries []string
	pos     int
	draft   string
}

func newInputHistory() inputHistory {
	return inputHistory{pos: 0}
}

func (h *inputHistory) Reset(entries []string) {
	h.entries = h.entries[:0]
	for _, e := range entries {
		if strings.TrimSpace(e) != "" {
			h.entries = append(h.entries, e)
		}
	}
	h.pos = len(h.entries)
	h.draft = ""
}

func (h *inputHistory) Add(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if len(h.entries) == 0 || h.entries[len(h.entries)-1] != text {
		h.entries = append(h.entries, text)
	}
	h.pos = len(h.entries)
	h.draft = ""
}

func (h *inputHistory) Prev(current string) (string, bool) {
	if len(h.entries) == 0 || h.pos == 0 {
		return "", false
	}
	if h.pos == len(h.entries) {
		h.draft = current
	}
	h.pos--
	return h.entries[h.pos], true
}

func (h *inputHistory) Next() (string, bool) {
	if h.pos >= len(h.entries) {
		return "", false
	}
	h.pos++
	if h.pos == len(h.entries) {
		return h.draft, true
	}
	return h.entries[h.pos], true
}

func (h *inputHistory) Edited() {
	h.pos = len(h.entries)
	h.draft = ""
}

func completeSlashCommand(text string) string {
	commands := []string{
		"/help", "/clear", "/model", "/session", "/project", "/new", "/resume",
		"/revert", "/fork", "/context", "/compact", "/copy", "/exit",
	}

	if !strings.HasPrefix(text, "/") {
		return text
	}

	var matches []string
	for _, cmd := range commands {
		if strings.HasPrefix(cmd, text) {
			matches = append(matches, cmd)
		}
	}

	if len(matches) == 1 {
		return matches[0] + " "
	}

	if len(matches) > 1 {
		lcp := longestCommonPrefix(matches)
		if len(lcp) > len(text) {
			return lcp
		}
	}

	return text
}

func longestCommonPrefix(strs []string) string {
	if len(strs) == 0 {
		return ""
	}
	prefix := strs[0]
	for _, s := range strs[1:] {
		for !strings.HasPrefix(s, prefix) {
			prefix = prefix[:len(prefix)-1]
			if prefix == "" {
				return ""
			}
		}
	}
	return prefix
}
