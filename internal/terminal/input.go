package terminal

import "strings"

// KeyMsg represents a key event from the terminal.
type KeyMsg struct {
	Rune    rune
	Special KeySpecial
}

type KeySpecial int

const (
	KeyNone KeySpecial = iota
	KeyEnter
	KeyBackspace
	KeyTab
	KeyEscape
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyCtrlC
	KeyCtrlD
	KeyCtrlT
	KeyDelete
	KeyHome
	KeyEnd
)

func ParseInputBytes(b []byte) []KeyMsg {
	var keys []KeyMsg
	i := 0
	for i < len(b) {
		switch {
		case b[i] == 0x03:
			keys = append(keys, KeyMsg{Special: KeyCtrlC})
			i++
		case b[i] == 0x04:
			keys = append(keys, KeyMsg{Special: KeyCtrlD})
			i++
		case b[i] == 0x14:
			keys = append(keys, KeyMsg{Special: KeyCtrlT})
			i++
		case b[i] == '\r' || b[i] == '\n':
			keys = append(keys, KeyMsg{Special: KeyEnter})
			i++
		case b[i] == 0x7f || b[i] == 0x08:
			keys = append(keys, KeyMsg{Special: KeyBackspace})
			i++
		case b[i] == '\t':
			keys = append(keys, KeyMsg{Special: KeyTab})
			i++
		case b[i] == 0x1b:
			if i+2 < len(b) && b[i+1] == '[' {
				consumed := 3
				switch b[i+2] {
				case 'A':
					keys = append(keys, KeyMsg{Special: KeyUp})
				case 'B':
					keys = append(keys, KeyMsg{Special: KeyDown})
				case 'C':
					keys = append(keys, KeyMsg{Special: KeyRight})
				case 'D':
					keys = append(keys, KeyMsg{Special: KeyLeft})
				case 'H':
					keys = append(keys, KeyMsg{Special: KeyHome})
				case 'F':
					keys = append(keys, KeyMsg{Special: KeyEnd})
				case '3':
					if i+3 < len(b) && b[i+3] == '~' {
						keys = append(keys, KeyMsg{Special: KeyDelete})
						consumed = 4
					} else {
						keys = append(keys, KeyMsg{Special: KeyEscape})
					}
				default:
					keys = append(keys, KeyMsg{Special: KeyEscape})
				}
				i += consumed
			} else {
				keys = append(keys, KeyMsg{Special: KeyEscape})
				i++
			}
		default:
			keys = append(keys, KeyMsg{Rune: rune(b[i])})
			i++
		}
	}
	return keys
}

// InputLine manages a single-line text input with cursor.
type InputLine struct {
	text   []rune
	cursor int
}

func NewInputLine() *InputLine {
	return &InputLine{}
}

func (il *InputLine) String() string {
	return string(il.text)
}

func (il *InputLine) CursorText() string {
	if il.cursor <= 0 {
		return ""
	}
	if il.cursor >= len(il.text) {
		return string(il.text)
	}
	return string(il.text[:il.cursor])
}

func (il *InputLine) Insert(r rune) {
	il.text = append(il.text[:il.cursor], append([]rune{r}, il.text[il.cursor:]...)...)
	il.cursor++
}

func (il *InputLine) DeleteBack() bool {
	if il.cursor == 0 {
		return false
	}
	il.text = append(il.text[:il.cursor-1], il.text[il.cursor:]...)
	il.cursor--
	return true
}

func (il *InputLine) DeleteForward() bool {
	if il.cursor >= len(il.text) {
		return false
	}
	il.text = append(il.text[:il.cursor], il.text[il.cursor+1:]...)
	return true
}

func (il *InputLine) MoveLeft() bool {
	if il.cursor == 0 {
		return false
	}
	il.cursor--
	return true
}

func (il *InputLine) MoveRight() bool {
	if il.cursor >= len(il.text) {
		return false
	}
	il.cursor++
	return true
}

func (il *InputLine) MoveHome() {
	il.cursor = 0
}

func (il *InputLine) MoveEnd() {
	il.cursor = len(il.text)
}

func (il *InputLine) Set(text string) {
	il.text = []rune(text)
	il.cursor = len(il.text)
}

func (il *InputLine) Clear() {
	il.text = nil
	il.cursor = 0
}

type InputHistory struct {
	entries []string
	pos     int
	draft   string
}

func NewInputHistory() InputHistory {
	return InputHistory{pos: 0}
}

func (h *InputHistory) Reset(entries []string) {
	h.entries = h.entries[:0]
	for _, e := range entries {
		if strings.TrimSpace(e) != "" {
			h.entries = append(h.entries, e)
		}
	}
	h.pos = len(h.entries)
	h.draft = ""
}

func (h *InputHistory) Add(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if len(h.entries) == 0 || h.entries[len(h.entries)-1] != text {
		h.entries = append(h.entries, text)
	}
	h.pos = len(h.entries)
	h.draft = ""
}

func (h *InputHistory) Prev(current string) (string, bool) {
	if len(h.entries) == 0 || h.pos == 0 {
		return "", false
	}
	if h.pos == len(h.entries) {
		h.draft = current
	}
	h.pos--
	return h.entries[h.pos], true
}

func (h *InputHistory) Next() (string, bool) {
	if h.pos >= len(h.entries) {
		return "", false
	}
	h.pos++
	if h.pos == len(h.entries) {
		return h.draft, true
	}
	return h.entries[h.pos], true
}

func (h *InputHistory) Edited() {
	h.pos = len(h.entries)
	h.draft = ""
}

// CompleteSlashCommand completes text against the caller-supplied command
// list. The list is the caller's vocabulary; this helper holds none.
func CompleteSlashCommand(text string, commands []string) string {
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
