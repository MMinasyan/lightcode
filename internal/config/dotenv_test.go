package config

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// envKeyForTest returns a key unlikely to collide with anything in the real
// environment, and arranges for it to be unset when the test finishes.
func envKeyForTest(t *testing.T, suffix string) string {
	t.Helper()
	key := "LIGHTCODE_TEST_" + suffix
	t.Setenv(key, "") // ensures cleanup; we'll unset properly below
	_ = os.Unsetenv(key)
	return key
}

func TestLoadDotEnvCreatesTemplateWhenMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".lightcode", ".env")

	m, err := LoadDotEnv()
	if err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if m.Path() != path {
		t.Fatalf("Path = %q, want %q", m.Path(), path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("template file not created: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("template mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "#OPENAI_API_KEY=\n") || !strings.Contains(body, "#MINIMAX_API_KEY=\n") {
		t.Fatalf("template should contain commented empty key slots, got %q", body)
	}
	if strings.Contains(body, "***") || strings.Contains(body, "...") {
		t.Fatalf("template should not contain placeholder/credential-looking values: %q", body)
	}
	if len(m.ManagedKeys()) != 0 {
		t.Fatalf("managed set should be empty for template, got %v", m.ManagedKeys())
	}
}

func TestLoadDotEnvPopulatesManagedSetForUnsetKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := envKeyForTest(t, "LOAD_MGD")
	path := filepath.Join(dir, ".env")
	body := "# comment\n" + key + "=from-file\nOTHER_KEY=other\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := LoadDotEnv()
	if err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got := os.Getenv(key); got != "from-file" {
		t.Fatalf("env %s = %q, want %q", key, got, "from-file")
	}
	if !m.IsManaged(key) {
		t.Fatalf("key %s should be managed after LoadDotEnv", key)
	}
	if !m.IsManaged("OTHER_KEY") {
		t.Fatalf("OTHER_KEY should be managed after LoadDotEnv")
	}
	if got := os.Getenv("OTHER_KEY"); got != "other" {
		t.Fatalf("OTHER_KEY env = %q, want %q", got, "other")
	}
}

func TestLoadDotEnvShellKeyNotManaged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := envKeyForTest(t, "SHELL_EXT")
	// Pre-set in shell — LoadDotEnv must not overwrite, must not mark managed.
	t.Setenv(key, "from-shell")

	path := filepath.Join(dir, ".env")
	body := key + "=from-file\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := LoadDotEnv()
	if err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got := os.Getenv(key); got != "from-shell" {
		t.Fatalf("env %s = %q, want %q (shell must win)", key, got, "from-shell")
	}
	if m.IsManaged(key) {
		t.Fatalf("shell-exported key %s must not be in managed set", key)
	}
}

func TestManagedEnvSetWritesKeyAndUpdatesProcessEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	// Seed with an unrelated line + comment to verify preservation.
	seed := "# keep me\nUNRELATED=keep\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	key := envKeyForTest(t, "SET_WRITE")
	m := NewManagedEnvForTest(path)

	if err := m.Set(key, "hello"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Process env updated.
	if got := os.Getenv(key); got != "hello" {
		t.Fatalf("env %s = %q, want %q", key, got, "hello")
	}
	// Managed set updated.
	if !m.IsManaged(key) {
		t.Fatalf("key %s should be managed after Set", key)
	}
	// File contents.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, key+"=hello") {
		t.Fatalf("file missing key line: %q", body)
	}
	if !strings.Contains(body, "# keep me") {
		t.Fatalf("comment not preserved: %q", body)
	}
	if !strings.Contains(body, "UNRELATED=keep") {
		t.Fatalf("unrelated line not preserved: %q", body)
	}
	// File mode.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestManagedEnvSetReplacesExistingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	key := envKeyForTest(t, "SET_REPLACE")
	seed := key + "=old\nOTHER=x\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManagedEnvForTest(path)
	m.managed[key] = struct{}{} // simulate that it was loaded from .env

	if err := m.Set(key, "new"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Contains(body, key+"=old") {
		t.Fatalf("old value still present: %q", body)
	}
	if !strings.Contains(body, key+"=new") {
		t.Fatalf("new value missing: %q", body)
	}
	if !strings.Contains(body, "OTHER=x") {
		t.Fatalf("unrelated line lost: %q", body)
	}
	if got := os.Getenv(key); got != "new" {
		t.Fatalf("env %s = %q, want %q", key, got, "new")
	}
}

func TestManagedEnvSetRefusesExternalKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	key := envKeyForTest(t, "EXT_REFUSE")
	t.Setenv(key, "external")

	m := NewManagedEnvForTest(path)
	err := m.Set(key, "overwrite")
	if !errors.Is(err, ErrExternalKey) {
		t.Fatalf("Set err = %v, want ErrExternalKey", err)
	}
	// Env unchanged.
	if got := os.Getenv(key); got != "external" {
		t.Fatalf("env %s = %q, want %q", key, got, "external")
	}
	// File unchanged.
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), key) {
		t.Fatalf("file should not contain key after refused Set: %q", string(data))
	}
	// Not in managed set.
	if m.IsManaged(key) {
		t.Fatalf("refused key must not be in managed set")
	}
}

func TestManagedEnvRemoveDeletesLineAndUnsets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	key := envKeyForTest(t, "RM_LINE")
	seed := "# header\n" + key + "=val\nKEEP=me\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(key, "val") // ensure cleanup

	m := NewManagedEnvForTest(path)
	m.managed[key] = struct{}{}

	if err := m.Remove(key); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, ok := os.LookupEnv(key); ok {
		t.Fatalf("env %s still set after Remove", key)
	}
	if m.IsManaged(key) {
		t.Fatalf("key %s still managed after Remove", key)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Contains(body, key+"=") {
		t.Fatalf("key line still in file: %q", body)
	}
	if !strings.Contains(body, "# header") {
		t.Fatalf("comment lost: %q", body)
	}
	if !strings.Contains(body, "KEEP=me") {
		t.Fatalf("unrelated line lost: %q", body)
	}
}

func TestManagedEnvRemoveNoopForUnmanagedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("KEEP=me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	key := envKeyForTest(t, "RM_NOOP")
	t.Setenv(key, "external")

	m := NewManagedEnvForTest(path)
	// Key is external, not managed.
	if err := m.Remove(key); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Env still set.
	if got := os.Getenv(key); got != "external" {
		t.Fatalf("env %s = %q, want %q (should be untouched)", key, got, "external")
	}
}

func TestManagedEnvRoundTripSameSession(t *testing.T) {
	// Simulates the Commit-4 scenario: key written this session is
	// immediately in the managed set and removable without restart.
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	key := envKeyForTest(t, "RT_SESSION")
	m := NewManagedEnvForTest(path)

	if err := m.Set(key, "session-val"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !m.IsManaged(key) {
		t.Fatalf("key written this session must be managed")
	}
	if err := m.Remove(key); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if m.IsManaged(key) {
		t.Fatalf("key should no longer be managed after Remove")
	}
	if _, ok := os.LookupEnv(key); ok {
		t.Fatalf("env %s still set after Remove", key)
	}
}

func TestManagedEnvSetValueWithSpacesIsQuoted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".lightcode")
	path := filepath.Join(dir, ".env")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	key := envKeyForTest(t, "QUOTED")
	m := NewManagedEnvForTest(path)
	if err := m.Set(key, "has spaces"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), key+`="has spaces"`) {
		t.Fatalf("quoted value not in file: %q", string(data))
	}

	_ = os.Unsetenv(key)
	m2, err := LoadDotEnv()
	if err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got := os.Getenv(key); got != "has spaces" {
		t.Fatalf("env %s after reload = %q, want %q", key, got, "has spaces")
	}
	if !m2.IsManaged(key) {
		t.Fatalf("key %s should be managed after quoted reload", key)
	}
}

func TestManagedEnvSetQuotedSpecialValueRoundTrips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".lightcode")
	path := filepath.Join(dir, ".env")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	key := envKeyForTest(t, "SPECIAL")
	value := "quote=\" slash=\\ line\nbreak tab	end"
	m := NewManagedEnvForTest(path)
	if err := m.Set(key, value); err != nil {
		t.Fatalf("Set: %v", err)
	}
	data, _ := os.ReadFile(path)
	body := string(data)
	if strings.Contains(body, "line\nbreak") {
		t.Fatalf("literal newline written inside env value: %q", body)
	}
	if !strings.Contains(body, `\n`) || !strings.Contains(body, `\t`) {
		t.Fatalf("newline/tab escapes missing from quoted value: %q", body)
	}

	_ = os.Unsetenv(key)
	if _, err := LoadDotEnv(); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got := os.Getenv(key); got != value {
		t.Fatalf("env %s after reload = %q, want %q", key, got, value)
	}
}

func TestWriteDotEnvAtomicNoPartialFileOnError(t *testing.T) {
	// Use a path whose parent is a regular file so CreateTemp fails, and
	// verify no leftover .tmp files accumulate anywhere we can see.
	dir := t.TempDir()
	// Create a regular file where the .env should go; MkdirAll will fail
	// because a path component is a file.
	blocker := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(blocker, "subdir", ".env")

	err := writeDotEnvAtomic(badPath, []byte("KEY=val\n"))
	if err == nil {
		t.Fatal("expected error writing under a file-as-dir path")
	}
	// No .tmp files should remain under dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWriteDotEnvAtomicLeavesNoTempOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := writeDotEnvAtomic(path, []byte("A=1\n")); err != nil {
		t.Fatalf("writeDotEnvAtomic: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file after success: %s", e.Name())
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != "A=1\n" {
		t.Fatalf("file = %q, want %q", string(data), "A=1\n")
	}
}

func TestManagedEnvKeysSorted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManagedEnvForTest(path)
	k1 := envKeyForTest(t, "SORT_B")
	k2 := envKeyForTest(t, "SORT_A")
	_ = m.Set(k1, "1")
	_ = m.Set(k2, "2")
	keys := m.ManagedKeys()
	if len(keys) != 2 || keys[0] != k2 || keys[1] != k1 {
		t.Fatalf("ManagedKeys = %v, want [%s, %s]", keys, k2, k1)
	}
}

func TestIsValidEnvKey(t *testing.T) {
	cases := map[string]bool{
		"OPENAI_API_KEY": true,
		"_leading":       true,
		"a123":           true,
		"":               false,
		"1bad":           false,
		"has-dash":       false,
		"has space":      false,
		"has.dot":        false,
	}
	for k, want := range cases {
		if got := isValidEnvKey(k); got != want {
			t.Errorf("isValidEnvKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestReadDotEnvKeysAbsentFileCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	path := filepath.Join(dir, ".env")
	keys, malformed, err := ReadDotEnvKeys(path)
	if err != nil {
		t.Fatalf("ReadDotEnvKeys returned error: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("keys = %v, want empty", keys)
	}
	if len(malformed) != 0 {
		t.Fatalf("malformed = %v, want empty", malformed)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("parent dir was created: stat err = %v", statErr)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf(".env was created: stat err = %v", statErr)
	}
}

func TestReadDotEnvKeysParsesNamesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "# comment\n" +
		"\n" +
		"PLAIN_KEY=secret-value\n" +
		"export EXPORTED_KEY=\"quoted value\"\n" +
		"COMMENTED_OUT_IN_TEMPLATE_STYLE=\n" +
		"no equals sign here\n" +
		"BAD_QUOTE_KEY=\"bad \\x escape\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	keys, malformed, err := ReadDotEnvKeys(path)
	if err != nil {
		t.Fatalf("ReadDotEnvKeys returned error: %v", err)
	}
	for name, wantNonEmpty := range map[string]bool{
		"PLAIN_KEY":                       true,
		"EXPORTED_KEY":                    true,
		"COMMENTED_OUT_IN_TEMPLATE_STYLE": false,
	} {
		got, ok := keys[name]
		if !ok {
			t.Errorf("keys missing %q: %v", name, keys)
		} else if got != wantNonEmpty {
			t.Errorf("keys[%q] = %v, want %v (value emptiness)", name, got, wantNonEmpty)
		}
	}
	if len(keys) != 3 {
		t.Fatalf("keys = %v, want exactly 3 names", keys)
	}
	if len(malformed) != 2 {
		t.Fatalf("malformed = %v, want 2 entries", malformed)
	}
	for _, m := range malformed {
		if !strings.Contains(m, path+":") {
			t.Errorf("malformed entry %q does not name the file and line", m)
		}
	}

	// The process environment is untouched: nothing was Setenv'd.
	if _, set := os.LookupEnv("PLAIN_KEY"); set {
		t.Fatal("ReadDotEnvKeys set PLAIN_KEY in the process env")
	}
}

// Duplicate keys: the first occurrence wins, exactly as LoadDotEnv behaves
// (after its first Setenv the key is already set and later lines skip).
func TestReadDotEnvKeysFirstOccurrenceWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "EMPTY_FIRST=\n" +
		"EMPTY_FIRST=value\n" +
		"VALUE_FIRST=value\n" +
		"VALUE_FIRST=\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, _, err := ReadDotEnvKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if keys["EMPTY_FIRST"] {
		t.Fatal("EMPTY_FIRST: the empty first occurrence must win")
	}
	if !keys["VALUE_FIRST"] {
		t.Fatal("VALUE_FIRST: the non-empty first occurrence must win")
	}
}

// Quoted empty values unwrap to empty, exactly as LoadDotEnv parses them.
func TestReadDotEnvKeysQuotedEmptyValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "DOUBLE_QUOTED_EMPTY=\"\"\n" +
		"SINGLE_QUOTED_EMPTY=''\n" +
		"QUOTED_VALUE=\"x\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, malformed, err := ReadDotEnvKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(malformed) != 0 {
		t.Fatalf("malformed = %v, want none", malformed)
	}
	for _, name := range []string{"DOUBLE_QUOTED_EMPTY", "SINGLE_QUOTED_EMPTY"} {
		nonEmpty, ok := keys[name]
		if !ok || nonEmpty {
			t.Fatalf("%s: present=%v nonEmpty=%v, want defined and empty", name, ok, nonEmpty)
		}
	}
	if !keys["QUOTED_VALUE"] {
		t.Fatal("QUOTED_VALUE: a quoted non-empty value must resolve")
	}
}

// TestManagedEnvTrySetRows covers the one-attempt TrySet contract: success
// writes the file, process env, and managed set; contention returns an
// operation error and mutates nothing; the external-key refusal is preserved.
func TestManagedEnvTrySetRows(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte("# keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		key := envKeyForTest(t, "TRYSET_OK")
		m := NewManagedEnvForTest(path)
		if err := m.TrySet(key, "hello"); err != nil {
			t.Fatalf("TrySet: %v", err)
		}
		if got := os.Getenv(key); got != "hello" {
			t.Fatalf("env %s = %q, want hello", key, got)
		}
		if !m.IsManaged(key) {
			t.Fatal("key not managed after TrySet")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), key+"=hello") || !strings.Contains(string(data), "# keep") {
			t.Fatalf("file = %q, want the key line and the preserved comment", data)
		}
	})
	t.Run("contention_mutates_nothing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte("KEEP=me\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		key := envKeyForTest(t, "TRYSET_CONTEND")
		_ = os.Unsetenv(key)
		m := NewManagedEnvForTest(path)
		holder, ok, err := atomicfs.TryAcquire(envLockPath(path))
		if err != nil || !ok {
			t.Fatalf("seed TryAcquire: (%v, %v)", ok, err)
		}
		defer holder.Release()
		if err := m.TrySet(key, "secret"); err == nil || !strings.Contains(err.Error(), "retry") {
			t.Fatalf("TrySet under contention = %v, want a retryable error", err)
		}
		if _, set := os.LookupEnv(key); set {
			t.Fatalf("process env %s was set under contention", key)
		}
		if m.IsManaged(key) {
			t.Fatal("key was added to the managed set under contention")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), key) {
			t.Fatalf("file mutated under contention: %q", data)
		}
	})
	t.Run("external_key_refused", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
		key := envKeyForTest(t, "TRYSET_EXTERNAL")
		t.Setenv(key, "external")
		m := NewManagedEnvForTest(path)
		err := m.TrySet(key, "overwrite")
		if !errors.Is(err, ErrExternalKey) {
			t.Fatalf("TrySet err = %v, want ErrExternalKey", err)
		}
		if got := os.Getenv(key); got != "external" {
			t.Fatalf("env %s = %q, want the external value untouched", key, got)
		}
	})
}

// TestManagedEnvTryRemoveRows covers the one-attempt TryRemove contract:
// success removes the line and unsets the key; contention returns an operation
// error and mutates nothing; an unmanaged key is a no-op.
func TestManagedEnvTryRemoveRows(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		key := envKeyForTest(t, "TRYRM_OK")
		seed := "# header\n" + key + "=val\nKEEP=me\n"
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, "val")
		m := NewManagedEnvForTest(path)
		m.managed[key] = struct{}{}
		if err := m.TryRemove(key); err != nil {
			t.Fatalf("TryRemove: %v", err)
		}
		if _, ok := os.LookupEnv(key); ok {
			t.Fatalf("env %s still set after TryRemove", key)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(data)
		if strings.Contains(body, key+"=") {
			t.Fatalf("key line still in file: %q", body)
		}
		if !strings.Contains(body, "KEEP=me") {
			t.Fatalf("unrelated line lost: %q", body)
		}
	})
	t.Run("contention_mutates_nothing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		key := envKeyForTest(t, "TRYRM_CONTEND")
		seed := "# header\n" + key + "=val\nKEEP=me\n"
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, "val")
		m := NewManagedEnvForTest(path)
		m.managed[key] = struct{}{}
		holder, ok, err := atomicfs.TryAcquire(envLockPath(path))
		if err != nil || !ok {
			t.Fatalf("seed TryAcquire: (%v, %v)", ok, err)
		}
		defer holder.Release()
		if err := m.TryRemove(key); err == nil || !strings.Contains(err.Error(), "retry") {
			t.Fatalf("TryRemove under contention = %v, want a retryable error", err)
		}
		if got := os.Getenv(key); got != "val" {
			t.Fatalf("env %s = %q, want val untouched", key, got)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), key+"=val") {
			t.Fatalf("file mutated under contention: %q", data)
		}
	})
	t.Run("unmanaged_noop", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		key := envKeyForTest(t, "TRYRM_NOOP")
		if err := os.WriteFile(path, []byte("KEEP=me\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, "external")
		m := NewManagedEnvForTest(path)
		if err := m.TryRemove(key); err != nil {
			t.Fatalf("TryRemove unmanaged: %v", err)
		}
		if got := os.Getenv(key); got != "external" {
			t.Fatalf("external env %s was touched: %q", key, got)
		}
	})
}

// envBlockHolderEnv selects the child half of
// TestManagedEnvSetBlocksUntilForeignLockReleases and names the env lock path
// the child holds.
const envBlockHolderEnv = "LIGHTCODE_CONFIG_ENV_BLOCK_HOLDER"

// TestManagedEnvSetBlocksUntilForeignLockReleases proves the blocking Set
// wrapper stays blocking for startup/non-owner callers: with a foreign process
// holding the env leaf lock, Set stays parked and completes once the holder
// releases. It is the positive control beside the one-attempt TrySet. The
// holder is a self-exec child of this test binary, so the contention is a real
// cross-process flock, not a same-process open.
func TestManagedEnvSetBlocksUntilForeignLockReleases(t *testing.T) {
	if lockPath := os.Getenv(envBlockHolderEnv); lockPath != "" {
		l, err := atomicfs.Acquire(lockPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		if err := l.Release(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		os.Exit(0)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	key := envKeyForTest(t, "BLOCK_SET")
	_ = os.Unsetenv(key)
	lockPath := envLockPath(path)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedEnvSetBlocksUntilForeignLockReleases$")
	cmd.Env = append(os.Environ(), envBlockHolderEnv+"="+lockPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("child stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("child stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start child: %v", err)
	}
	ready := make(chan struct{})
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				close(ready)
				return
			}
		}
	}()
	reap := func() error {
		_ = stdin.Close()
		err := cmd.Wait()
		<-scannerDone
		return err
	}
	fail := func() string {
		cancel()
		_ = reap()
		return stderr.String()
	}
	t.Cleanup(func() { cancel(); _ = reap() })
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatalf("child never held the env lock within %v: %v\n%s", 30*time.Second, ctx.Err(), fail())
	}

	m := NewManagedEnvForTest(path)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- m.Set(key, "hello")
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("Set returned %v while the foreign process held the env lock; the blocking wrapper must stay blocked", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := reap(); err != nil {
		t.Fatalf("child after release: %v\n%s", err, stderr.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Set after the holder released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Set did not complete after the holder released the env lock")
	}
	if got := os.Getenv(key); got != "hello" {
		t.Fatalf("env %s = %q, want hello", key, got)
	}
}

// captureStderr redirects os.Stderr into a temp file for the test's duration
// and returns a func that reads what was written.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = old
		f.Close()
	})
	return func() string {
		data, _ := os.ReadFile(f.Name())
		return string(data)
	}
}

// TestDotEnvWritesReportLockReleaseFailure proves both public dotenv write
// paths — ManagedEnv.Set (writeDotEnvLine) and ManagedEnv.Remove
// (removeDotEnvLine) — serialize through atomicfs.WithLock: an injected
// release failure keeps the committed callback result (the write still lands
// and the public call still succeeds) and prints exactly one
// `lightcode: release lock <lockPath>: <error>` diagnostic. It fails against
// the old direct Acquire/Release sites, which ignored the release error
// silently.
func TestDotEnvWritesReportLockReleaseFailure(t *testing.T) {
	injected := errors.New("injected release failure")

	atomicfs.ReleaseFunc = func(*atomicfs.Lock) error { return injected }
	t.Cleanup(func() { atomicfs.ReleaseFunc = nil })

	// Each subtest uses its own directory: the injected release never drops
	// the underlying flock (ReleaseFunc replaces the real unlock), so a second
	// blocking Acquire on the same lock path would wait forever.
	t.Run("write", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		lockPath := filepath.Join(dir, ".locks", "env.lock")
		key := envKeyForTest(t, "RLF_WRITE")
		m := NewManagedEnvForTest(path)
		stderr := captureStderr(t)

		if err := m.Set(key, "hello"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		// The committed callback result is preserved: the write landed and
		// the process env was updated despite the release failure.
		if got := os.Getenv(key); got != "hello" {
			t.Fatalf("env %s = %q, want hello", key, got)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), key+"=hello") {
			t.Fatalf("file missing key line: %q", data)
		}
		if out := stderr(); out != "lightcode: release lock "+lockPath+": "+injected.Error()+"\n" {
			t.Fatalf("write stderr = %q, want the one exact release diagnostic", out)
		}
	})

	t.Run("remove", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		lockPath := filepath.Join(dir, ".locks", "env.lock")
		key := envKeyForTest(t, "RLF_REMOVE")
		seed := "# header\n" + key + "=val\nKEEP=me\n"
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Setenv(key, "val")
		m := NewManagedEnvForTest(path)
		m.managed[key] = struct{}{}
		stderr := captureStderr(t)

		if err := m.Remove(key); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(data)
		if strings.Contains(body, key+"=") {
			t.Fatalf("key line still in file: %q", body)
		}
		if !strings.Contains(body, "KEEP=me") {
			t.Fatalf("unrelated line lost: %q", body)
		}
		if out := stderr(); out != "lightcode: release lock "+lockPath+": "+injected.Error()+"\n" {
			t.Fatalf("remove stderr = %q, want the one exact release diagnostic", out)
		}
	})
}
