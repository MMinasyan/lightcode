package extension

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// SecretName resolves one declared SecretRef field in its active section to
// the non-secret credential name the field holds. The field is a JSON pointer
// into the section. A missing, empty or non-string value names no credential.
// Metadata inspection never executes extension code.
func SecretName(section json.RawMessage, field string) (string, bool) {
	if !strings.HasPrefix(field, "/") || len(bytes.TrimSpace(section)) == 0 {
		return "", false
	}
	decoder := json.NewDecoder(bytes.NewReader(section))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	for _, raw := range strings.Split(field[1:], "/") {
		token, ok := unescapePointerToken(raw)
		if !ok {
			return "", false
		}
		switch current := value.(type) {
		case map[string]any:
			next, ok := current[token]
			if !ok {
				return "", false
			}
			value = next
		case []any:
			index, ok := pointerIndex(token)
			if !ok || index >= len(current) {
				return "", false
			}
			value = current[index]
		default:
			return "", false
		}
	}
	name, ok := value.(string)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// ValidSecretField reports whether field is a JSON Pointer whose escape
// grammar is valid. It shares the exact token interpretation SecretName uses;
// a syntactically valid field may still resolve no name in a section.
func ValidSecretField(field string) bool {
	if !strings.HasPrefix(field, "/") {
		return false
	}
	for _, token := range strings.Split(field[1:], "/") {
		if _, ok := unescapePointerToken(token); !ok {
			return false
		}
	}
	return true
}

// unescapePointerToken applies the JSON Pointer escape pair only: ~1 is a
// slash, ~0 is a tilde, and any other tilde sequence is not a valid token.
func unescapePointerToken(token string) (string, bool) {
	if !strings.Contains(token, "~") {
		return token, true
	}
	var out strings.Builder
	out.Grow(len(token))
	for i := 0; i < len(token); i++ {
		if token[i] != '~' {
			out.WriteByte(token[i])
			continue
		}
		if i+1 >= len(token) {
			return "", false
		}
		switch token[i+1] {
		case '0':
			out.WriteByte('~')
		case '1':
			out.WriteByte('/')
		default:
			return "", false
		}
		i++
	}
	return out.String(), true
}

// pointerIndex accepts only the canonical JSON Pointer array index: zero or a
// nonzero digit followed by digits. Signs, leading zeros and empty tokens are
// object-key syntax, never array indices.
func pointerIndex(token string) (int, bool) {
	if token == "" {
		return 0, false
	}
	if token == "0" {
		return 0, true
	}
	if token[0] < '1' || token[0] > '9' {
		return 0, false
	}
	for i := 1; i < len(token); i++ {
		if token[i] < '0' || token[i] > '9' {
			return 0, false
		}
	}
	index, err := strconv.Atoi(token)
	if err != nil {
		return 0, false
	}
	return index, true
}
