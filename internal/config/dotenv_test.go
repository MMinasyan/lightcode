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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/testassert"
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
	testassert.NoError(t, err)
	if m.Path() != path {
		t.Fatalf("Path = %q, want %q", m.Path(), path)
	}
	info, err := os.Stat(path)
	testassert.NoError(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("template mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	testassert.NoError(t, err)
	body := string(data)
	testassert.Require(t, strings.Contains(body, "#OPENAI_API_KEY=\n") && strings.Contains(body, "#MINIMAX_API_KEY=\n") && !strings.Contains(body, "***") && !strings.Contains(body, "..."), "template should contain commented empty key slots, got %q; template should not contain placeholder/credential-looking values: %q", body, body)
	if len(m.ManagedKeys()) != 0 {
		t.Fatalf("managed set should be empty for template, got %v", m.ManagedKeys())
	}
}

func TestLoadDotEnvPopulatesManagedSetForUnsetKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".lightcode")
	testassert.NoError(t, os.MkdirAll(dir, 0o700))
	key := envKeyForTest(t, "LOAD_MGD")
	path := filepath.Join(dir, ".env")
	body := "# comment\n" + key + "=from-file\nOTHER_KEY=other\n"
	testassert.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	m, err := LoadDotEnv()
	testassert.NoError(t, err)
	{
		got := os.Getenv(key)
		testassert.Require(t, got == "from-file", "env %s = %q, want %q", key, got, "from-file")
	}
	testassert.Require(t, m.IsManaged(key), "key %s should be managed after LoadDotEnv", key)
	testassert.Require(t, m.IsManaged("OTHER_KEY"), "OTHER_KEY should be managed after LoadDotEnv")
	testassert.Equal(t, os.Getenv("OTHER_KEY"), "other")
}

func TestLoadDotEnvShellKeyNotManaged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".lightcode")
	testassert.NoError(t, os.MkdirAll(dir, 0o700))
	key := envKeyForTest(t, "SHELL_EXT")
	// Pre-set in shell — LoadDotEnv must not overwrite, must not mark managed.
	t.Setenv(key, "from-shell")

	path := filepath.Join(dir, ".env")
	body := key + "=from-file\n"
	testassert.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	m, err := LoadDotEnv()
	testassert.NoError(t, err)
	{
		got := os.Getenv(key)
		testassert.Require(t, got == "from-shell", "env %s = %q, want %q (shell must win)", key, got, "from-shell")
	}
	testassert.Require(t, !m.IsManaged(key), "shell-exported key %s must not be in managed set", key)
}

// TestManagedEnvOrdinaryRows drives the ordinary Set/Remove outcomes. Each
// row carries its seed, external environment and initial managed membership,
// its Set/Remove action, and independently authored post-state predicates for
// the process environment, the managed set and the real file's present and
// absent substrings. The $KEY placeholder in a seed or fragment is the row's
// env key.
func TestManagedEnvOrdinaryRows(t *testing.T) {
	rows := []struct {
		name          string
		suffix        string
		seed          string
		externalValue string
		externalSet   bool
		managed       bool
		remove        bool
		value         string
		wantErr       error
		wantEnv       string
		wantEnvSet    bool
		wantManaged   bool
		wantFile      []string
		absentFile    []string
		wantMode      os.FileMode
	}{
		{
			name:        "SetWritesKeyAndUpdatesProcessEnv",
			suffix:      "SET_WRITE",
			seed:        "# keep me\nUNRELATED=keep\n",
			value:       "hello",
			wantEnv:     "hello",
			wantEnvSet:  true,
			wantManaged: true,
			wantFile:    []string{"$KEY=hello", "# keep me", "UNRELATED=keep"},
			wantMode:    0o600,
		},
		{
			name:        "SetReplacesExistingKey",
			suffix:      "SET_REPLACE",
			seed:        "$KEY=old\nOTHER=x\n",
			managed:     true,
			value:       "new",
			wantEnv:     "new",
			wantEnvSet:  true,
			wantManaged: true,
			wantFile:    []string{"$KEY=new", "OTHER=x"},
			absentFile:  []string{"$KEY=old"},
		},
		{
			name:          "SetRefusesExternalKey",
			suffix:        "EXT_REFUSE",
			seed:          "",
			externalValue: "external",
			externalSet:   true,
			value:         "overwrite",
			wantErr:       ErrExternalKey,
			wantEnv:       "external",
			wantEnvSet:    true,
			wantManaged:   false,
			absentFile:    []string{"$KEY"},
		},
		{
			name:          "RemoveDeletesLineAndUnsets",
			suffix:        "RM_LINE",
			seed:          "# header\n$KEY=val\nKEEP=me\n",
			externalValue: "val",
			externalSet:   true,
			managed:       true,
			remove:        true,
			wantEnvSet:    false,
			wantManaged:   false,
			wantFile:      []string{"# header", "KEEP=me"},
			absentFile:    []string{"$KEY"},
		},
		{
			name:          "RemoveNoopForUnmanagedKey",
			suffix:        "RM_NOOP",
			seed:          "KEEP=me\n",
			externalValue: "external",
			externalSet:   true,
			managed:       false,
			remove:        true,
			wantEnv:       "external",
			wantEnvSet:    true,
			wantManaged:   false,
			wantFile:      []string{"KEEP=me"},
			absentFile:    []string{"$KEY"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			key := envKeyForTest(t, row.suffix)
			path := filepath.Join(t.TempDir(), ".env")
			seed := strings.ReplaceAll(row.seed, "$KEY", key) // $KEY is the row's env key
			testassert.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
			if row.externalSet {
				t.Setenv(key, row.externalValue)
			}
			m := NewManagedEnvForTest(path)
			if row.managed {
				m.managed[key] = struct{}{}
			}

			var err error
			if row.remove {
				err = m.Remove(key)
			} else {
				err = m.Set(key, row.value)
			}
			if row.wantErr != nil {
				testassert.Require(t, errors.Is(err, row.wantErr), "%s err = %v, want %v", row.name, err, row.wantErr)
			} else {
				testassert.NoError(t, err)
			}
			if row.wantEnvSet {
				{
					got := os.Getenv(key)
					testassert.Require(t, got == row.wantEnv, "env %s = %q, want %q", key, got, row.wantEnv)
				}
			} else {
				{
					_, ok := os.LookupEnv(key)
					testassert.Require(t, !ok, "env %s still set after %s", key, row.name)
				}
			}
			testassert.Require(t, m.IsManaged(key) == row.wantManaged, "managed %s = %v after %s, want %v", key, m.IsManaged(key), row.name, row.wantManaged)

			data, err := os.ReadFile(path)
			testassert.NoError(t, err)
			body := string(data)
			for _, frag := range row.wantFile {
				frag = strings.ReplaceAll(frag, "$KEY", key)
				testassert.Require(t, strings.Contains(body, frag), "file missing %q: %q", frag, body)
			}
			for _, frag := range row.absentFile {
				frag = strings.ReplaceAll(frag, "$KEY", key)
				testassert.Require(t, !strings.Contains(body, frag), "file still contains %q: %q", frag, body)
			}
			if row.wantMode != 0 {
				info, err := os.Stat(path)
				testassert.NoError(t, err)
				testassert.Require(t, info.Mode().Perm() == row.wantMode, "file mode = %o, want %o", info.Mode().Perm(), row.wantMode)
			}
		})
	}
}

func TestManagedEnvRejectsNULValueWithoutFileEffect(t *testing.T) {
	key := envKeyForTest(t, "NUL_VALUE")
	broken := "sk-\x00-broken"
	for _, tc := range []struct {
		name    string
		attempt func(*ManagedEnv) error
	}{
		{"Set", func(m *ManagedEnv) error { return m.Set(key, broken) }},
		{"TrySet", func(m *ManagedEnv) error { return m.TrySet(key, broken) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			m := NewManagedEnvForTest(path)
			{
				err := tc.attempt(m)
				testassert.Require(t, err != nil, "a NUL value was accepted")
			}
			{
				_, err := os.Stat(path)
				testassert.Require(t, os.IsNotExist(err), "a NUL value wrote the env file (%v)", err)
			}
			{
				_, defined := os.LookupEnv(key)
				testassert.Require(t, !defined, "a NUL value reached the process environment")
			}
			testassert.Require(t, !m.IsManaged(key), "a NUL value marked the key managed")
		})
	}
}

func TestManagedEnvRoundTripSameSession(t *testing.T) {
	// Simulates the Commit-4 scenario: key written this session is
	// immediately in the managed set and removable without restart.
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	testassert.NoError(t, os.WriteFile(path, []byte(""), 0o600))

	key := envKeyForTest(t, "RT_SESSION")
	m := NewManagedEnvForTest(path)

	testassert.NoError(t, m.Set(key, "session-val"))
	testassert.Require(t, m.IsManaged(key), "key written this session must be managed")
	testassert.NoError(t, m.Remove(key))
	testassert.Require(t, !m.IsManaged(key), "key should no longer be managed after Remove")
	{
		_, ok := os.LookupEnv(key)
		testassert.Require(t, !ok, "env %s still set after Remove", key)
	}
}

func TestManagedEnvSetValueWithSpacesIsQuoted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".lightcode")
	path := filepath.Join(dir, ".env")
	testassert.NoError(t, os.MkdirAll(dir, 0o700))
	testassert.NoError(t, os.WriteFile(path, []byte(""), 0o600))
	key := envKeyForTest(t, "QUOTED")
	m := NewManagedEnvForTest(path)
	testassert.NoError(t, m.Set(key, "has spaces"))
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), key+`="has spaces"`) {
		t.Fatalf("quoted value not in file: %q", string(data))
	}

	_ = os.Unsetenv(key)
	m2, err := LoadDotEnv()
	testassert.NoError(t, err)
	{
		got := os.Getenv(key)
		testassert.Require(t, got == "has spaces", "env %s after reload = %q, want %q", key, got, "has spaces")
	}
	testassert.Require(t, m2.IsManaged(key), "key %s should be managed after quoted reload", key)
}

func TestManagedEnvSetQuotedSpecialValueRoundTrips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".lightcode")
	path := filepath.Join(dir, ".env")
	testassert.NoError(t, os.MkdirAll(dir, 0o700))
	testassert.NoError(t, os.WriteFile(path, []byte(""), 0o600))

	key := envKeyForTest(t, "SPECIAL")
	value := "quote=\" slash=\\ line\nbreak tab	end"
	m := NewManagedEnvForTest(path)
	testassert.NoError(t, m.Set(key, value))
	data, _ := os.ReadFile(path)
	body := string(data)
	testassert.Require(t, !strings.Contains(body, "line\nbreak") && strings.Contains(body, `\n`) && strings.Contains(body, `\t`), "literal newline written inside env value: %q; newline/tab escapes missing from quoted value: %q", body, body)

	_ = os.Unsetenv(key)
	{
		_, err := LoadDotEnv()
		testassert.NoError(t, err)
	}
	{
		got := os.Getenv(key)
		testassert.Require(t, got == value, "env %s after reload = %q, want %q", key, got, value)
	}
}

func TestWriteDotEnvAtomicNoPartialFileOnError(t *testing.T) {
	// Use a path whose parent is a regular file so CreateTemp fails, and
	// verify no leftover .tmp files accumulate anywhere we can see.
	dir := t.TempDir()
	// Create a regular file where the .env should go; MkdirAll will fail
	// because a path component is a file.
	blocker := filepath.Join(dir, "blocked")
	testassert.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	badPath := filepath.Join(blocker, "subdir", ".env")

	err := writeDotEnvAtomic(badPath, []byte("KEY=val\n"))
	testassert.Require(t, err != nil, "expected error writing under a file-as-dir path")
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
	testassert.NoError(t, writeDotEnvAtomic(path, []byte("A=1\n")))
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
	testassert.NoError(t, os.WriteFile(path, []byte(""), 0o600))
	m := NewManagedEnvForTest(path)
	k1 := envKeyForTest(t, "SORT_B")
	k2 := envKeyForTest(t, "SORT_A")
	_ = m.Set(k1, "1")
	_ = m.Set(k2, "2")
	keys := m.ManagedKeys()
	testassert.Require(t, len(keys) == 2 && keys[0] == k2 && keys[1] == k1, "ManagedKeys = %v, want [%s, %s]", keys, k2, k1)
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
		{
			got := isValidEnvKey(k)
			testassert.Check(t, got == want, "isValidEnvKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestReadDotEnvKeysAbsentFileCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	path := filepath.Join(dir, ".env")
	keys, malformed, err := ReadDotEnvKeys(path)
	testassert.NoError(t, err)
	testassert.Require(t, len(keys) == 0, "keys = %v, want empty", keys)
	testassert.Require(t, len(malformed) == 0, "malformed = %v, want empty", malformed)
	{
		_, statErr := os.Stat(dir)
		testassert.Require(t, os.IsNotExist(statErr), "parent dir was created: stat err = %v", statErr)
	}
	{
		_, statErr := os.Stat(path)
		testassert.Require(t, os.IsNotExist(statErr), ".env was created: stat err = %v", statErr)
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
	testassert.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	keys, malformed, err := ReadDotEnvKeys(path)
	testassert.NoError(t, err)
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
	testassert.Require(t, len(keys) == 3, "keys = %v, want exactly 3 names", keys)
	testassert.Require(t, len(malformed) == 2, "malformed = %v, want 2 entries", malformed)
	for _, m := range malformed {
		testassert.Check(t, strings.Contains(m, path+":"), "malformed entry %q does not name the file and line", m)
	}

	// The process environment is untouched: nothing was Setenv'd.
	{
		_, set := os.LookupEnv("PLAIN_KEY")
		testassert.Require(t, !set, "ReadDotEnvKeys set PLAIN_KEY in the process env")
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
	testassert.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	keys, _, err := ReadDotEnvKeys(path)
	testassert.NoError(t, err)
	testassert.Require(t, !keys["EMPTY_FIRST"], "EMPTY_FIRST: the empty first occurrence must win")
	testassert.Require(t, keys["VALUE_FIRST"], "VALUE_FIRST: the non-empty first occurrence must win")
}

// Quoted empty values unwrap to empty, exactly as LoadDotEnv parses them.
func TestReadDotEnvKeysQuotedEmptyValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "DOUBLE_QUOTED_EMPTY=\"\"\n" +
		"SINGLE_QUOTED_EMPTY=''\n" +
		"QUOTED_VALUE=\"x\"\n"
	testassert.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	keys, malformed, err := ReadDotEnvKeys(path)
	testassert.NoError(t, err)
	testassert.Require(t, len(malformed) == 0, "malformed = %v, want none", malformed)
	for _, name := range []string{"DOUBLE_QUOTED_EMPTY", "SINGLE_QUOTED_EMPTY"} {
		nonEmpty, ok := keys[name]
		testassert.Require(t, ok && !nonEmpty, "%s: present=%v nonEmpty=%v, want defined and empty", name, ok, nonEmpty)
	}
	testassert.Require(t, keys["QUOTED_VALUE"], "QUOTED_VALUE: a quoted non-empty value must resolve")
}

// TestManagedEnvTrySetRows covers the one-attempt TrySet contract: success
// writes the file, process env, and managed set; contention returns an
// operation error and mutates nothing; the external-key refusal is preserved.
func TestManagedEnvTrySetRows(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		testassert.NoError(t, os.WriteFile(path, []byte("# keep\n"), 0o600))
		key := envKeyForTest(t, "TRYSET_OK")
		m := NewManagedEnvForTest(path)
		testassert.NoError(t, m.TrySet(key, "hello"))
		{
			got := os.Getenv(key)
			testassert.Require(t, got == "hello", "env %s = %q, want hello", key, got)
		}
		testassert.Require(t, m.IsManaged(key), "key not managed after TrySet")
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		testassert.Require(t, strings.Contains(string(data), key+"=hello") && strings.Contains(string(data), "# keep"), "file = %q, want the key line and the preserved comment", data)
	})
	t.Run("contention_mutates_nothing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		testassert.NoError(t, os.WriteFile(path, []byte("KEEP=me\n"), 0o600))
		key := envKeyForTest(t, "TRYSET_CONTEND")
		_ = os.Unsetenv(key)
		m := NewManagedEnvForTest(path)
		holder, ok, err := atomicfs.TryAcquire(envLockPath(path))
		testassert.Require(t, err == nil && ok, "seed TryAcquire: (%v, %v)", ok, err)
		defer holder.Release()
		{
			err := m.TrySet(key, "secret")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "retry"), "TrySet under contention = %v, want a retryable error", err)
		}
		{
			_, set := os.LookupEnv(key)
			testassert.Require(t, !set, "process env %s was set under contention", key)
		}
		testassert.Require(t, !m.IsManaged(key), "key was added to the managed set under contention")
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		testassert.Require(t, !strings.Contains(string(data), key), "file mutated under contention: %q", data)
	})
	t.Run("external_key_refused", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		testassert.NoError(t, os.WriteFile(path, []byte(""), 0o600))
		key := envKeyForTest(t, "TRYSET_EXTERNAL")
		t.Setenv(key, "external")
		m := NewManagedEnvForTest(path)
		err := m.TrySet(key, "overwrite")
		testassert.Require(t, errors.Is(err, ErrExternalKey), "TrySet err = %v, want ErrExternalKey", err)
		{
			got := os.Getenv(key)
			testassert.Require(t, got == "external", "env %s = %q, want the external value untouched", key, got)
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
		testassert.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
		t.Setenv(key, "val")
		m := NewManagedEnvForTest(path)
		m.managed[key] = struct{}{}
		testassert.NoError(t, m.TryRemove(key))
		{
			_, ok := os.LookupEnv(key)
			testassert.Require(t, !ok, "env %s still set after TryRemove", key)
		}
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		body := string(data)
		testassert.Require(t, !strings.Contains(body, key+"=") && strings.Contains(body, "KEEP=me"), "key line still in file: %q; unrelated line lost: %q", body, body)
	})
	t.Run("contention_mutates_nothing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		key := envKeyForTest(t, "TRYRM_CONTEND")
		seed := "# header\n" + key + "=val\nKEEP=me\n"
		testassert.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
		t.Setenv(key, "val")
		m := NewManagedEnvForTest(path)
		m.managed[key] = struct{}{}
		holder, ok, err := atomicfs.TryAcquire(envLockPath(path))
		testassert.Require(t, err == nil && ok, "seed TryAcquire: (%v, %v)", ok, err)
		defer holder.Release()
		{
			err := m.TryRemove(key)
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "retry"), "TryRemove under contention = %v, want a retryable error", err)
		}
		{
			got := os.Getenv(key)
			testassert.Require(t, got == "val", "env %s = %q, want val untouched", key, got)
		}
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		testassert.Require(t, strings.Contains(string(data), key+"=val"), "file mutated under contention: %q", data)
	})
	t.Run("unmanaged_noop", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		key := envKeyForTest(t, "TRYRM_NOOP")
		testassert.NoError(t, os.WriteFile(path, []byte("KEEP=me\n"), 0o600))
		t.Setenv(key, "external")
		m := NewManagedEnvForTest(path)
		testassert.NoError(t, m.TryRemove(key))
		{
			got := os.Getenv(key)
			testassert.Require(t, got == "external", "external env %s was touched: %q", key, got)
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
		testassert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Set did not complete after the holder released the env lock")
	}
	{
		got := os.Getenv(key)
		testassert.Require(t, got == "hello", "env %s = %q, want hello", key, got)
	}
}

// captureStderr redirects os.Stderr into a temp file for the test's duration
// and returns a func that reads what was written.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "stderr-*")
	testassert.NoError(t, err)
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

		testassert.NoError(t, m.Set(key, "hello"))
		// The committed callback result is preserved: the write landed and
		// the process env was updated despite the release failure.
		{
			got := os.Getenv(key)
			testassert.Require(t, got == "hello", "env %s = %q, want hello", key, got)
		}
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		testassert.Require(t, strings.Contains(string(data), key+"=hello"), "file missing key line: %q", data)
		{
			out := stderr()
			testassert.Require(t, out == "lightcode: release lock "+lockPath+": "+injected.Error()+"\n", "write stderr = %q, want the one exact release diagnostic", out)
		}
	})

	t.Run("remove", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		lockPath := filepath.Join(dir, ".locks", "env.lock")
		key := envKeyForTest(t, "RLF_REMOVE")
		seed := "# header\n" + key + "=val\nKEEP=me\n"
		testassert.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
		_ = os.Setenv(key, "val")
		m := NewManagedEnvForTest(path)
		m.managed[key] = struct{}{}
		stderr := captureStderr(t)

		testassert.NoError(t, m.Remove(key))
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		body := string(data)
		testassert.Require(t, !strings.Contains(body, key+"=") && strings.Contains(body, "KEEP=me"), "key line still in file: %q; unrelated line lost: %q", body, body)
		{
			out := stderr()
			testassert.Require(t, out == "lightcode: release lock "+lockPath+": "+injected.Error()+"\n", "remove stderr = %q, want the one exact release diagnostic", out)
		}
	})
}

func TestEnvWithoutKeys(t *testing.T) {
	t.Run("managed name filtered case-sensitively", func(t *testing.T) {
		env := []string{"MANAGED_KEY=1", "managed_key=2", "PATH=/bin"}
		got := EnvWithoutKeys(env, []string{"MANAGED_KEY"})
		want := []string{"managed_key=2", "PATH=/bin"}
		testassert.Require(t, slices.Equal(got, want), "EnvWithoutKeys = %v, want %v", got, want)
	})

	t.Run("external name retained", func(t *testing.T) {
		env := []string{"EXTERNAL_TOKEN=abc", "MANAGED_TOKEN=xyz"}
		got := EnvWithoutKeys(env, []string{"MANAGED_TOKEN"})
		want := []string{"EXTERNAL_TOKEN=abc"}
		testassert.Require(t, slices.Equal(got, want), "EnvWithoutKeys = %v, want %v", got, want)
	})

	t.Run("nil inputs", func(t *testing.T) {
		{
			got := EnvWithoutKeys(nil, nil)
			testassert.Require(t, len(got) == 0, "EnvWithoutKeys(nil, nil) = %v, want empty", got)
		}
		env := []string{"A=1", "B=2"}
		{
			got := EnvWithoutKeys(env, nil)
			testassert.Require(t, slices.Equal(got, env), "EnvWithoutKeys(env, nil) = %v, want %v", got, env)
		}
		{
			got := EnvWithoutKeys(nil, []string{"A"})
			testassert.Require(t, len(got) == 0, "EnvWithoutKeys(nil, keys) = %v, want empty", got)
		}
	})
}

// A scanner error after valid lines must not discard the keys already
// injected: the partial managed set still covers them.
func TestLoadDotEnvScannerErrorReturnsPartialManagedSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".lightcode")
	testassert.NoError(t, os.MkdirAll(dir, 0o700))
	key := envKeyForTest(t, "SCANNER_PARTIAL")
	path := filepath.Join(dir, ".env")
	body := key + "=injected\nHUGE=" + strings.Repeat("A", bufio.MaxScanTokenSize+1) + "\n"
	testassert.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	m, err := LoadDotEnv()
	testassert.Require(t, err != nil, "LoadDotEnv: want scanner error from the oversized line, got nil")
	testassert.Require(t, m != nil, "LoadDotEnv: want a partial managed set alongside the scanner error, got nil")
	{
		got := os.Getenv(key)
		testassert.Require(t, got == "injected", "env %s = %q, want the value injected before the scanner error", key, got)
	}
	{
		keys := m.ManagedKeys()
		testassert.Require(t, slices.Contains(keys, key), "ManagedKeys = %v, want to contain %s", keys, key)
	}
}

func TestManagedEnvSubprocessEnvFiltersCurrentManagedSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	testassert.NoError(t, os.WriteFile(path, []byte(""), 0o600))
	m := NewManagedEnvForTest(path)
	external := envKeyForTest(t, "SUBPROC_EXTERNAL")
	managed := envKeyForTest(t, "SUBPROC_MANAGED")
	t.Setenv(external, "sibling-value")

	// Before any managed write: everything passes through.
	env := m.SubprocessEnv()
	testassert.Require(t, env != nil, "SubprocessEnv = nil, want a non-nil slice")
	testassert.Require(t, slices.Contains(env, external+"=sibling-value") && !slices.Contains(env, managed+"="), "SubprocessEnv = %v, want the external sibling present; SubprocessEnv = %v, want the unset managed name absent", env, env)

	testassert.NoError(t, m.TrySet(managed, "managed-secret"))
	env = m.SubprocessEnv()
	testassert.Require(t, !slices.Contains(env, managed+"=managed-secret") && slices.Contains(env, external+"=sibling-value"), "SubprocessEnv = %v, want the managed key scrubbed; SubprocessEnv = %v, want the external sibling still present", env, env)

	// The snapshot is one owned copy: mutating it never reaches the manager
	// or the process env.
	if len(env) > 0 {
		env[0] = "MUTED=1"
	}
	testassert.Require(t, slices.Contains(m.SubprocessEnv(), external+"=sibling-value"), "a mutated snapshot changed the manager's view")

	testassert.NoError(t, m.TryRemove(managed))
	testassert.Require(t, !slices.Contains(m.SubprocessEnv(), managed+"=managed-secret"), "the removed key is back in the snapshot")
}

func TestManagedEnvSubprocessEnvNilReceiver(t *testing.T) {
	var m *ManagedEnv
	env := m.SubprocessEnv()
	testassert.Require(t, env != nil, "nil receiver returned nil, want a non-nil empty slice")
	testassert.Equal(t, len(env), 0)
}

func TestSubprocessEnvConcurrentWithManagedWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	testassert.NoError(t, os.WriteFile(path, []byte(""), 0o600))
	m := NewManagedEnvForTest(path)
	key := envKeyForTest(t, "SUBPROC_RACE")
	t.Setenv(key, "")
	_ = os.Unsetenv(key)

	// One synchronous managed write proves the writer path works before the
	// consumer loop, so a failed or unscheduled writer cannot pass vacuously.
	testassert.NoError(t, m.TrySet(key, "managed-secret"))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopWriter := func() {
		stopOnce.Do(func() { close(stop) })
	}
	// A failing assertion stops and joins the writer before unwinding, so no
	// leaked goroutine keeps mutating the process env or the temp .env.
	defer func() {
		stopWriter()
		wg.Wait()
	}()
	writerErrs := make(chan error, 1)
	wg.Add(1)
	go func() { // concurrent managed writes share SubprocessEnv's critical section
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			var err error
			if i%2 == 0 {
				err = m.TrySet(key, "managed-secret")
			} else {
				err = m.TryRemove(key)
			}
			if err != nil {
				select {
				case writerErrs <- err:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		env := m.SubprocessEnv()
		// The managed key is filtered while owned and unset while unmanaged:
		// no key= prefix may ever appear in the snapshot, not even the exact
		// managed secret.
		for _, entry := range env {
			testassert.Require(t, !strings.HasPrefix(entry, key+"="), "snapshot leaked a managed key entry %q", entry)
		}
	}
	stopWriter()
	wg.Wait()
	select {
	case err := <-writerErrs:
		t.Fatalf("concurrent managed writer: %v", err)
	default:
	}
	// After the final state settles, the snapshot reflects it exactly.
	testassert.NoError(t, m.TrySet(key, "managed-secret"))
	for _, entry := range m.SubprocessEnv() {
		testassert.Require(t, !strings.HasPrefix(entry, key+"="), "the connected key leaked into the snapshot: %q", entry)
	}
}
