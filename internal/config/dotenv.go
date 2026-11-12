package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Syntheresis/melta/internal/atomicfs"
)

// envLockPath is the cross-process lock serializing read-modify-write of the
// .env file, held so two processes cannot lose each other's updates.
func envLockPath(envPath string) string {
	return filepath.Join(filepath.Dir(envPath), ".locks", "env.lock")
}

// ErrExternalKey is returned by ManagedEnv.TrySet and TryRemove when the
// requested key is already present in the process environment from outside
// Lightcode (a shell export). Lightcode refuses to shadow it.
var ErrExternalKey = fmt.Errorf("env var is set externally; Lightcode will not override it")

// CredentialValue is one source-coherent observation of an environment
// variable: its current value, whether the source defines it at all, and
// whether Lightcode's managed file owns it. Infrastructure data only — it is
// never a protocol DTO, log value, or durable capture.
type CredentialValue struct {
	Value   string
	Defined bool
	Managed bool
}

// ManagedEnv is one explicit dotenv file. It never mutates the process
// environment: Capture reads the file on every call with the process
// environment taking precedence, and TrySet/TryRemove update only this file
// under its cross-process lock.
type ManagedEnv struct {
	path string
}

// NewManagedEnv constructs a managed environment bound to path.
func NewManagedEnv(path string) *ManagedEnv {
	return &ManagedEnv{path: path}
}

// Path returns the .env file path this ManagedEnv operates on.
func (m *ManagedEnv) Path() string {
	if m == nil {
		return ""
	}
	return m.path
}

// Capture returns one owned observation of each requested name. A name
// defined in the process environment wins and is unmanaged; otherwise a file
// entry is defined and managed; otherwise the name is undefined. The file is
// read on every call, so an external edit is observed by the next capture.
func (m *ManagedEnv) Capture(names []string) (map[string]CredentialValue, error) {
	file, malformed, err := readDotEnvFile(m.Path())
	if err != nil {
		return nil, err
	}
	for _, message := range malformed {
		fmt.Fprintf(os.Stderr, "lightcode: .env %s: skipping %s\n", m.Path(), message)
	}
	environment := parseEnviron(os.Environ())
	out := make(map[string]CredentialValue, len(names))
	for _, name := range names {
		if value, defined := environment[name]; defined {
			out[name] = CredentialValue{Value: value, Defined: true}
			continue
		}
		if value, managed := file[name]; managed {
			out[name] = CredentialValue{Value: value, Defined: true, Managed: true}
			continue
		}
		out[name] = CredentialValue{}
	}
	return out, nil
}

// parseEnviron splits one os.Environ snapshot into its name → value map. A
// defined-but-empty variable keeps its empty value with Defined true.
func parseEnviron(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		out[name] = value
	}
	return out
}

// TrySet writes key=value to the .env file (adding or replacing the line)
// in one nonblocking lock attempt, so a shutting-down owner never parks on a
// foreign env-lock holder. If key is already set in the process env, TrySet
// returns ErrExternalKey and makes no change — shell precedence is preserved
// on every write path. On leaf contention it returns an operation error and
// mutates neither the file nor the process environment.
//
// The file write is atomic: a temp file in the same directory is written
// with mode 0600 and renamed over the target, so a crash mid-write cannot
// leave a partial .env.
func (m *ManagedEnv) TrySet(key, value string) error {
	if m == nil {
		return fmt.Errorf("managed env is not initialized")
	}
	if err := validateEnvKeyValue(key, value); err != nil {
		return err
	}
	if _, external := os.LookupEnv(key); external {
		return ErrExternalKey
	}
	ok, err := atomicfs.TryWithLock(envLockPath(m.path), func() error {
		return writeDotEnvLinePayload(m.path, key, value)
	})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("managed env lock is busy; retry")
	}
	return nil
}

// TryRemove deletes key from the .env file in one nonblocking lock attempt,
// so a shutting-down owner never parks on a foreign env-lock holder. A key
// defined by the process environment is refused with ErrExternalKey; an
// absent unmanaged name is a successful no-op; on leaf contention it returns
// an operation error and mutates neither the file nor the process
// environment.
func (m *ManagedEnv) TryRemove(key string) error {
	if m == nil {
		return fmt.Errorf("managed env is not initialized")
	}
	if _, external := os.LookupEnv(key); external {
		return ErrExternalKey
	}
	file, _, err := readDotEnvFile(m.path)
	if err != nil {
		return err
	}
	if _, present := file[key]; !present {
		return nil
	}
	ok, err := atomicfs.TryWithLock(envLockPath(m.path), func() error {
		return removeDotEnvLinePayload(m.path, key)
	})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("managed env lock is busy; retry")
	}
	return nil
}

// SubprocessEnv returns the process environment with every supplied excluded
// name and every name the managed file defines removed, shell-supplied and
// managed overlaps included. The managed file's values never enter this
// projection; an unreadable file is an error rather than an ambient fallback.
func (m *ManagedEnv) SubprocessEnv(excluded []string) ([]string, error) {
	managed, _, err := ReadDotEnvKeys(m.Path())
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(excluded)+len(managed))
	names = append(names, excluded...)
	for name := range managed {
		names = append(names, name)
	}
	return EnvWithoutKeys(os.Environ(), names), nil
}

// readDotEnvFile reads the file and returns parsed values plus human-readable
// malformed-line descriptions. A malformed line is skipped.
func readDotEnvFile(path string) (map[string]string, []string, error) {
	values := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return values, nil, nil
		}
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var malformed []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
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
			malformed = append(malformed, fmt.Sprintf("skipping malformed line at %d", lineNum))
			continue
		}

		key := strings.TrimSpace(line[:eq])
		value, err := parseEnvValue(strings.TrimSpace(line[eq+1:]))
		if err != nil {
			malformed = append(malformed, fmt.Sprintf("skipping malformed quoted value at %d: %v", lineNum, err))
			continue
		}
		if _, seen := values[key]; !seen {
			values[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	return values, malformed, nil
}

// EnvWithoutKeys returns env with every entry whose name is in keys removed.
// Order otherwise preserved; nil keys returns env unchanged.
func EnvWithoutKeys(env []string, keys []string) []string {
	if len(keys) == 0 {
		return env
	}
	drop := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		drop[k] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if _, ok := drop[name]; ok {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// ReadDotEnvKeys reads the .env file at path and returns the key names it
// defines, each mapped to whether its parsed value is non-empty — a
// template-style `KEY=` line defines the name but resolves no credential.
// It also returns human-readable descriptions of any malformed lines. An
// absent file yields an empty map without creating anything, values are
// parsed only to detect malformed quoting and emptiness and never retained,
// the first occurrence of a duplicated key wins, and the process environment
// is not touched.
func ReadDotEnvKeys(path string) (map[string]bool, []string, error) {
	values, malformed, err := readDotEnvFile(path)
	if err != nil {
		return nil, nil, err
	}
	keys := make(map[string]bool, len(values))
	for key, value := range values {
		keys[key] = value != ""
	}
	return keys, malformed, nil
}

func validateEnvKeyValue(key, value string) error {
	if !isValidEnvKey(key) {
		return fmt.Errorf("invalid env key %q", key)
	}
	return validateEnvValue(value)
}

// validateEnvValue rejects native environment values the OS refuses before
// any owning env-file write, so an ordinary Setenv input refusal cannot follow
// persistence. NUL is the one invalid byte in an environment value.
func validateEnvValue(value string) error {
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("invalid env value: contains NUL")
	}
	return nil
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
// runs under the env leaf lock held by the caller, so it never reacquires the
// leaf.
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
// removed. It runs under the env leaf lock held by the caller, so it never
// reacquires the leaf.
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
// On any failure the temp file is removed so no partial artifact remains.
func writeDotEnvAtomic(path string, data []byte) error {
	return atomicfs.Write(path, data, 0o600)
}
