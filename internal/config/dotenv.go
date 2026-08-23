package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// DotEnvPath returns the path where Lightcode expects its .env file.
func DotEnvPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lightcode", ".env"), nil
}

// envLockPath is the cross-process lock serializing read-modify-write of the
// .env file, held so two processes cannot lose each other's updates.
func envLockPath(envPath string) string {
	return filepath.Join(filepath.Dir(envPath), ".locks", "env.lock")
}

// dotEnvTemplate is written to ~/.lightcode/.env the first time Lightcode
// runs. Empty values — the user uncomments a line and pastes the raw key
// after the `=`. No placeholder prefixes, no explanatory comments.
const dotEnvTemplate = `#OPENAI_API_KEY=
#MINIMAX_API_KEY=
`

// ErrExternalKey is returned by ManagedEnv.Set when the requested key is
// already present in the process environment from outside Lightcode (a shell
// export) and is not in the managed set. Lightcode refuses to shadow it.
var ErrExternalKey = fmt.Errorf("env var is set externally; Lightcode will not override it")

// ManagedEnv tracks the set of env vars Lightcode owns — those loaded from
// ~/.lightcode/.env at startup plus any written during this session. It is
// safe for concurrent use. The managed set is the source of truth for
// deciding whether a key can be updated or removed by Lightcode: keys that
// came from the shell are never touched.
type ManagedEnv struct {
	mu      sync.Mutex
	path    string
	managed map[string]struct{}
}

// Path returns the .env file path this ManagedEnv operates on.
func (m *ManagedEnv) Path() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.path
}

// IsManaged reports whether key is owned by Lightcode (loaded from .env at
// startup, or written by Lightcode during this session). Shell-exported keys
// that Lightcode did not set are not managed.
func (m *ManagedEnv) IsManaged(key string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.managed[key]
	return ok
}

// ManagedKeys returns a sorted snapshot of the current managed key set.
func (m *ManagedEnv) ManagedKeys() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.managed))
	for k := range m.managed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Set writes key=value to the .env file (adding or replacing the line) and
// applies it to the process environment. If key is already set in the
// process env and is not in the managed set, Set returns ErrExternalKey and
// makes no change — shell precedence is preserved on every write path.
//
// The file write is atomic: a temp file in the same directory is written
// with mode 0600 and renamed over the target, so a crash mid-write cannot
// leave a partial .env.
func (m *ManagedEnv) Set(key, value string) error {
	if m == nil {
		return fmt.Errorf("managed env is not initialized")
	}
	if !isValidEnvKey(key) {
		return fmt.Errorf("invalid env key %q", key)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Shell precedence: if the key is set in the process env but Lightcode
	// did not put it there, refuse to overwrite.
	if _, managed := m.managed[key]; !managed {
		if _, external := os.LookupEnv(key); external {
			return ErrExternalKey
		}
	}

	return atomicfs.WithLock(envLockPath(m.path), func() error {
		return m.setEnvPayloadLocked(key, value)
	})
}

// TrySet is the one-attempt counterpart of Set for owner paths that must not
// block a shutting-down owner on a foreign env-lock holder. It applies the
// same external-key checks and the same payload as Set; on leaf contention it
// returns an operation error and mutates neither the file, the process env,
// nor the managed set.
func (m *ManagedEnv) TrySet(key, value string) error {
	if m == nil {
		return fmt.Errorf("managed env is not initialized")
	}
	if !isValidEnvKey(key) {
		return fmt.Errorf("invalid env key %q", key)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, managed := m.managed[key]; !managed {
		if _, external := os.LookupEnv(key); external {
			return ErrExternalKey
		}
	}

	ok, err := atomicfs.TryWithLock(envLockPath(m.path), func() error {
		return m.setEnvPayloadLocked(key, value)
	})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("managed env lock is busy; retry")
	}
	return nil
}

// setEnvPayloadLocked rewrites the .env file, updates the process env, and
// records the key as managed. It runs under the env leaf lock held by the
// caller (the blocking WithLock in Set or the one-attempt TryWithLock in
// TrySet), so it never reacquires the leaf. The caller holds m.mu.
func (m *ManagedEnv) setEnvPayloadLocked(key, value string) error {
	if err := writeDotEnvLinePayload(m.path, key, value); err != nil {
		return err
	}
	if err := os.Setenv(key, value); err != nil {
		return fmt.Errorf("setenv %s: %w", key, err)
	}
	if m.managed == nil {
		m.managed = map[string]struct{}{}
	}
	m.managed[key] = struct{}{}
	return nil
}

// Remove deletes key from the .env file and unsets it in the process
// environment. If key is not managed (e.g. it came from the shell), Remove
// is a no-op and returns nil — Lightcode never mutates env it did not set.
func (m *ManagedEnv) Remove(key string) error {
	if m == nil {
		return fmt.Errorf("managed env is not initialized")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, managed := m.managed[key]; !managed {
		return nil
	}
	return atomicfs.WithLock(envLockPath(m.path), func() error {
		return m.removeEnvPayloadLocked(key)
	})
}

// TryRemove is the one-attempt counterpart of Remove for owner paths that must
// not block a shutting-down owner on a foreign env-lock holder. It applies the
// same unmanaged no-op and the same payload as Remove; on leaf contention it
// returns an operation error and mutates neither the file, the process env,
// nor the managed set.
func (m *ManagedEnv) TryRemove(key string) error {
	if m == nil {
		return fmt.Errorf("managed env is not initialized")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, managed := m.managed[key]; !managed {
		return nil
	}
	ok, err := atomicfs.TryWithLock(envLockPath(m.path), func() error {
		return m.removeEnvPayloadLocked(key)
	})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("managed env lock is busy; retry")
	}
	return nil
}

// removeEnvPayloadLocked removes every line defining key from the .env file,
// unsets the key in the process env, and drops it from the managed set. It
// runs under the env leaf lock held by the caller (the blocking WithLock in
// Remove or the one-attempt TryWithLock in TryRemove), so it never reacquires
// the leaf. The caller holds m.mu.
func (m *ManagedEnv) removeEnvPayloadLocked(key string) error {
	if err := removeDotEnvLinePayload(m.path, key); err != nil {
		return err
	}
	_ = os.Unsetenv(key)
	delete(m.managed, key)
	return nil
}

// LoadDotEnv reads ~/.lightcode/.env, populates the process environment
// with KEY=value entries that are not already set, and returns a ManagedEnv
// that tracks which keys Lightcode actually loaded. Existing process env
// vars always win — a shell `export` takes precedence, and such keys are
// not added to the managed set.
//
// On first run the file does not exist; LoadDotEnv creates the parent
// directory (mode 0700) and writes a commented template (mode 0600), then
// proceeds. The user only ever has to edit that one file.
//
// File format:
//
//	# full-line comments are allowed
//	KEY=value
//	KEY="quoted value"
//	export KEY=value   # shell-compat, "export " prefix is stripped
//
// Blank lines and full-line comments (#) are skipped. Malformed lines
// print a warning to stderr and are ignored.
func LoadDotEnv() (*ManagedEnv, error) {
	path, err := DotEnvPath()
	if err != nil {
		return &ManagedEnv{path: "", managed: map[string]struct{}{}}, nil
	}

	// Auto-create the file with a template if it does not exist.
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: could not create %s: %v\n", filepath.Dir(path), err)
			return &ManagedEnv{path: path, managed: map[string]struct{}{}}, nil
		}
		if _, err := atomicfs.CreateExclusive(path, []byte(dotEnvTemplate), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: could not create %s: %v\n", path, err)
			return &ManagedEnv{path: path, managed: map[string]struct{}{}}, nil
		}
	}

	m := &ManagedEnv{path: path, managed: map[string]struct{}{}}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			fmt.Fprintf(os.Stderr, "lightcode: .env %s:%d: skipping malformed line\n", path, lineNum)
			continue
		}

		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])

		value, err = parseEnvValue(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: .env %s:%d: skipping malformed quoted value: %v\n", path, lineNum, err)
			continue
		}

		if _, alreadySet := os.LookupEnv(key); alreadySet {
			// Shell (or prior setenv) wins; do not mark as managed.
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: .env: setenv %s failed: %v\n", key, err)
			continue
		}
		m.managed[key] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return m, nil
}

// ReadDotEnvKeys reads the .env file at path and returns the key names it
// defines, each mapped to whether its parsed value is non-empty — a
// template-style `KEY=` line defines the name but resolves no credential.
// It also returns human-readable descriptions of any malformed lines. It is
// the read-only counterpart of LoadDotEnv: an absent file yields an empty
// map without creating anything, values are parsed only to detect malformed
// quoting and emptiness and never retained, the first occurrence of a
// duplicated key wins (as in LoadDotEnv), and the process environment is
// not touched.
func ReadDotEnvKeys(path string) (map[string]bool, []string, error) {
	keys := map[string]bool{}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return keys, nil, nil
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return keys, nil, nil
		}
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var malformed []string
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			malformed = append(malformed, fmt.Sprintf("%s:%d: malformed line", path, lineNum))
			continue
		}

		key := strings.TrimSpace(line[:eq])
		value, err := parseEnvValue(strings.TrimSpace(line[eq+1:]))
		if err != nil {
			malformed = append(malformed, fmt.Sprintf("%s:%d: malformed quoted value: %v", path, lineNum, err))
			continue
		}
		if _, seen := keys[key]; !seen {
			keys[key] = value != ""
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	return keys, malformed, nil
}

// NewManagedEnvForTest constructs a ManagedEnv bound to path with an empty
// managed set. Intended for tests that want to exercise Set/Remove against a
// specific file without going through LoadDotEnv.
func NewManagedEnvForTest(path string) *ManagedEnv {
	return &ManagedEnv{path: path, managed: map[string]struct{}{}}
}

// isValidEnvKey reports whether key is a plausible env var name: non-empty,
// starts with a letter or underscore, contains only [A-Za-z0-9_].
func isValidEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_':
			continue
		case r >= 'A' && r <= 'Z':
			continue
		case r >= 'a' && r <= 'z':
			continue
		case r >= '0' && r <= '9' && i > 0:
			continue
		default:
			return false
		}
	}
	return true
}

// writeDotEnvLinePayload rewrites the .env file with key=value applied. It
// runs under the env leaf lock held by the caller (the blocking WithLock in
// ManagedEnv.Set or the one-attempt TryWithLock in ManagedEnv.TrySet), so it
// never reacquires the leaf.
func writeDotEnvLinePayload(path, key, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	existing, readErr := os.ReadFile(path)
	var lines []string
	if readErr == nil {
		lines = strings.Split(string(existing), "\n")
		// A trailing newline produces an empty final element; keep it so we
		// can reproduce the file faithfully.
	} else if !os.IsNotExist(readErr) {
		return readErr
	}

	encoded := key + "=" + quoteEnvValue(value)
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "export ")
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(trimmed[:eq])
		if k == key {
			lines[i] = encoded
			replaced = true
			// Keep going only if we want to replace the first occurrence;
			// .env convention is first-wins for duplicates, but we replace
			// all occurrences of the same key for simplicity.
		}
	}
	if !replaced {
		// Append. If the file ended with a newline, the split left an empty
		// trailing element; insert before it so we don't get a blank line
		// between the last real line and the new entry.
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = append(lines[:len(lines)-1], encoded, "")
		} else {
			lines = append(lines, encoded)
		}
	}

	body := strings.Join(lines, "\n")
	return writeDotEnvAtomic(path, []byte(body))
}

// removeDotEnvLinePayload rewrites the .env file with every line defining key
// removed. It runs under the env leaf lock held by the caller (the blocking
// WithLock in ManagedEnv.Remove or the one-attempt TryWithLock in
// ManagedEnv.TryRemove), so it never reacquires the leaf.
func removeDotEnvLinePayload(path, key string) error {
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	lines := strings.Split(string(existing), "\n")
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		probe := strings.TrimPrefix(trimmed, "export ")
		if strings.HasPrefix(probe, "#") || trimmed == "" {
			kept = append(kept, line)
			continue
		}
		eq := strings.IndexByte(probe, '=')
		if eq <= 0 {
			kept = append(kept, line)
			continue
		}
		k := strings.TrimSpace(probe[:eq])
		if k == key {
			continue // drop
		}
		kept = append(kept, line)
	}

	body := strings.Join(kept, "\n")
	return writeDotEnvAtomic(path, []byte(body))
}

// quoteEnvValue returns value wrapped in double quotes if it contains
// characters that would be ambiguous in a shell-style .env file (spaces,
// quotes, hashes, newlines). Plain values are returned bare, matching the
// existing template style.
func quoteEnvValue(value string) string {
	if value == "" {
		return ""
	}
	needsQuote := false
	for _, r := range value {
		switch r {
		case ' ', '\t', '"', '\'', '#', '\\', '\n', '\r':
			needsQuote = true
		}
		if needsQuote {
			break
		}
	}
	if !needsQuote {
		return value
	}
	// Escape characters that cannot safely appear literally inside one
	// shell-style .env line.
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	escaped = strings.ReplaceAll(escaped, "\r", `\r`)
	escaped = strings.ReplaceAll(escaped, "\t", `\t`)
	return `"` + escaped + `"`
}

func parseEnvValue(value string) (string, error) {
	if len(value) < 2 {
		return value, nil
	}
	first := value[0]
	last := value[len(value)-1]
	if first == '\'' && last == '\'' {
		return value[1 : len(value)-1], nil
	}
	if first != '"' || last != '"' {
		return value, nil
	}

	var b strings.Builder
	escaped := false
	for _, r := range value[1 : len(value)-1] {
		if escaped {
			switch r {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '\\', '"':
				b.WriteRune(r)
			default:
				return "", fmt.Errorf("unsupported escape \\%c", r)
			}
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		return "", fmt.Errorf("trailing escape")
	}
	return b.String(), nil
}

// writeDotEnvAtomic writes data to path via a temp file + rename, mode 0600.
// Mirrors the pattern used by writeAgentConfigAtomic. On any failure the
// temp file is removed so no partial artifact remains.
func writeDotEnvAtomic(path string, data []byte) error {
	return atomicfs.Write(path, data, 0o600)
}
