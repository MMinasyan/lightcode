package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
