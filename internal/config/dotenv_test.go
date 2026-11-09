package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestManagedEnvOrdinaryRows drives the ordinary TrySet/TryRemove outcomes. Each
// row carries its seed, external environment, its TrySet/TryRemove action, and
// independently authored post-state predicates for the file and the captured
// observation. The $KEY placeholder in a seed or fragment is the row's env
// key. The process environment is never mutated by a managed write.
func TestManagedEnvOrdinaryRows(t *testing.T) {
	rows := []struct {
		name          string
		suffix        string
		seed          string
		externalValue string
		externalSet   bool
		remove        bool
		value         string
		wantErr       error
		wantDefined   bool
		wantManaged   bool
		wantValue     string
		wantFile      []string
		absentFile    []string
		wantMode      os.FileMode
	}{
		{
			name:        "SetWritesKey",
			suffix:      "SET_WRITE",
			seed:        "# keep me\nUNRELATED=keep\n",
			value:       "hello",
			wantDefined: true,
			wantManaged: true,
			wantValue:   "hello",
			wantFile:    []string{"$KEY=hello", "# keep me", "UNRELATED=keep"},
			wantMode:    0o600,
		},
		{
			name:        "SetReplacesExistingKey",
			suffix:      "SET_REPLACE",
			seed:        "$KEY=old\nOTHER=x\n",
			value:       "new",
			wantDefined: true,
			wantManaged: true,
			wantValue:   "new",
			wantFile:    []string{"$KEY=new", "OTHER=x"},
			absentFile:  []string{"$KEY=old"},
		},
		{
			name:          "SetRefusesExternalKey",
			suffix:        "EXT_REFUSE",
			externalValue: "external",
			externalSet:   true,
			value:         "overwrite",
			wantErr:       ErrExternalKey,
			wantDefined:   true,
			wantManaged:   false,
			wantValue:     "external",
			absentFile:    []string{"$KEY"},
		},
		{
			name:        "RemoveDeletesLine",
			suffix:      "RM_LINE",
			seed:        "# header\n$KEY=val\nKEEP=me\n",
			remove:      true,
			wantDefined: false,
			wantFile:    []string{"# header", "KEEP=me"},
			absentFile:  []string{"$KEY"},
		},
		{
			name:        "RemoveNoopForAbsentKey",
			suffix:      "RM_NOOP",
			seed:        "KEEP=me\n",
			remove:      true,
			wantDefined: false,
			wantFile:    []string{"KEEP=me"},
			absentFile:  []string{"$KEY"},
		},
		{
			name:          "RemoveRefusesExternalKey",
			suffix:        "RM_EXT",
			seed:          "$KEY=file\n",
			externalValue: "external",
			externalSet:   true,
			remove:        true,
			wantErr:       ErrExternalKey,
			wantDefined:   true,
			wantManaged:   false,
			wantValue:     "external",
			wantFile:      []string{"$KEY=file"},
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
			m := NewManagedEnv(path)

			var err error
			if row.remove {
				err = m.TryRemove(key)
			} else {
				err = m.TrySet(key, row.value)
			}
			if row.wantErr != nil {
				testassert.Require(t, errors.Is(err, row.wantErr), "%s err = %v, want %v", row.name, err, row.wantErr)
			} else {
				testassert.NoError(t, err)
			}

			captured, captureErr := m.Capture([]string{key})
			testassert.NoError(t, captureErr)
			got := captured[key]
			testassert.Require(t, got.Defined == row.wantDefined && got.Managed == row.wantManaged && got.Value == row.wantValue, "capture after %s = %+v, want defined=%v managed=%v value=%q", row.name, got, row.wantDefined, row.wantManaged, row.wantValue)

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

// TestSubprocessEnvExcludesManagedNames proves the child-environment
// projection drops every name the managed file defines — even when the shell
// also defines it — in addition to the supplied excluded names, and that an
// unreadable managed file is an error rather than an ambient fallback.
func TestSubprocessEnvExcludesManagedNames(t *testing.T) {
	managedName := envKeyForTest(t, "SUBPROCESS_MANAGED")
	overlapName := envKeyForTest(t, "SUBPROCESS_OVERLAP")
	suppliedName := envKeyForTest(t, "SUBPROCESS_SUPPLIED")
	keepName := envKeyForTest(t, "SUBPROCESS_KEEP")
	path := filepath.Join(t.TempDir(), ".env")
	testassert.NoError(t, os.WriteFile(path, []byte(managedName+"=file\n"+overlapName+"=file\n"), 0o600))
	t.Setenv(overlapName, "shell")
	t.Setenv(suppliedName, "supplied")
	t.Setenv(keepName, "keep")
	m := NewManagedEnv(path)

	env, err := m.SubprocessEnv([]string{suppliedName})
	testassert.NoError(t, err)
	kept := false
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case managedName, overlapName, suppliedName:
			t.Fatalf("subprocess environment kept excluded name %q: %q", name, entry)
		case keepName:
			kept = true
			testassert.Require(t, entry == keepName+"=keep", "ordinary entry = %q, want the shell value", entry)
		}
	}
	testassert.Require(t, kept, "the ordinary shell entry was dropped from the projection")

	blocked := filepath.Join(t.TempDir(), "dir")
	testassert.NoError(t, os.Mkdir(blocked, 0o700))
	{
		_, err := NewManagedEnv(blocked).SubprocessEnv(nil)
		testassert.Require(t, err != nil, "an unreadable managed file produced an ambient environment")
	}
}

// TestManagedEnvTwoExplicitLocationsIndependent proves two sources over
// different explicit dotenv files hold independent values for the same name in
// one process, while a real shell value has precedence in both and managed
// writes change neither the other file nor the parent environment.
func TestManagedEnvTwoExplicitLocationsIndependent(t *testing.T) {
	key := envKeyForTest(t, "TWO_LOCATIONS")
	firstPath := filepath.Join(t.TempDir(), ".env")
	secondPath := filepath.Join(t.TempDir(), ".env")
	testassert.NoError(t, os.WriteFile(firstPath, []byte(key+"=first\n"), 0o600))
	testassert.NoError(t, os.WriteFile(secondPath, []byte(key+"=second\n"), 0o600))

	first := NewManagedEnv(firstPath)
	second := NewManagedEnv(secondPath)
	firstCapture, err := first.Capture([]string{key})
	testassert.NoError(t, err)
	secondCapture, err := second.Capture([]string{key})
	testassert.NoError(t, err)
	testassert.Require(t, firstCapture[key].Value == "first" && firstCapture[key].Managed, "first source = %+v, want its own managed value", firstCapture[key])
	testassert.Require(t, secondCapture[key].Value == "second" && secondCapture[key].Managed, "second source = %+v, want its own managed value", secondCapture[key])

	testassert.NoError(t, first.TrySet(key, "first-updated"))
	secondCapture, err = second.Capture([]string{key})
	testassert.NoError(t, err)
	testassert.Require(t, secondCapture[key].Value == "second", "second source observed the first source's write: %+v", secondCapture[key])
	{
		_, defined := os.LookupEnv(key)
		testassert.Require(t, !defined, "a managed write reached the parent environment")
	}

	t.Setenv(key, "shell")
	for _, source := range []*ManagedEnv{first, second} {
		captured, err := source.Capture([]string{key})
		testassert.NoError(t, err)
		testassert.Require(t, captured[key].Value == "shell" && captured[key].Defined && !captured[key].Managed, "source under shell precedence = %+v, want the unmanaged shell value", captured[key])
		{
			err := source.TrySet(key, "shadow")
			testassert.Require(t, errors.Is(err, ErrExternalKey), "Set under shell precedence = %v, want ErrExternalKey", err)
		}
		{
			err := source.TryRemove(key)
			testassert.Require(t, errors.Is(err, ErrExternalKey), "Remove under shell precedence = %v, want ErrExternalKey", err)
		}
	}
	{
		got := os.Getenv(key)
		testassert.Require(t, got == "shell", "parent environment = %q, want the shell value untouched", got)
	}
}

func TestManagedEnvCaptureClassifiesEmptyAndAbsent(t *testing.T) {
	key := envKeyForTest(t, "EMPTY_CLASS")
	path := filepath.Join(t.TempDir(), ".env")
	testassert.NoError(t, os.WriteFile(path, []byte(key+"=\n"), 0o600))
	m := NewManagedEnv(path)
	captured, err := m.Capture([]string{key})
	testassert.NoError(t, err)
	got := captured[key]
	testassert.Require(t, got.Defined && got.Managed && got.Value == "", "managed-empty capture = %+v, want defined managed empty", got)

	t.Setenv(key, "")
	captured, err = m.Capture([]string{key})
	testassert.NoError(t, err)
	got = captured[key]
	testassert.Require(t, got.Defined && !got.Managed && got.Value == "", "external-empty capture = %+v, want defined unmanaged empty", got)
	_ = os.Unsetenv(key)

	testassert.NoError(t, os.WriteFile(path, nil, 0o600))
	captured, err = m.Capture([]string{key})
	testassert.NoError(t, err)
	testassert.Require(t, !captured[key].Defined, "absent capture = %+v, want undefined", captured[key])
}

func TestManagedEnvRejectsNULValueWithoutFileEffect(t *testing.T) {
	key := envKeyForTest(t, "NUL_VALUE")
	broken := "sk-\x00-broken"
	for _, tc := range []struct {
		name    string
		attempt func(*ManagedEnv) error
	}{
		{"TrySet", func(m *ManagedEnv) error { return m.TrySet(key, broken) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			m := NewManagedEnv(path)
			{
				err := tc.attempt(m)
				testassert.Require(t, err != nil, "a NUL value was accepted")
			}
			{
				_, err := os.Stat(path)
				testassert.Require(t, os.IsNotExist(err), "a NUL value wrote the env file (%v)", err)
			}
			captured, err := m.Capture([]string{key})
			testassert.NoError(t, err)
			testassert.Require(t, !captured[key].Defined, "a NUL value defined the key")
		})
	}
}

func TestManagedEnvSetValueWithSpacesIsQuoted(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	testassert.NoError(t, os.WriteFile(path, []byte(""), 0o600))
	key := envKeyForTest(t, "QUOTED")
	m := NewManagedEnv(path)
	testassert.NoError(t, m.TrySet(key, "has spaces"))
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), key+`="has spaces"`) {
		t.Fatalf("quoted value not in file: %q", string(data))
	}
	captured, err := m.Capture([]string{key})
	testassert.NoError(t, err)
	testassert.Require(t, captured[key].Value == "has spaces" && captured[key].Managed, "quoted reload = %+v, want the managed value", captured[key])
}

func TestManagedEnvSetQuotedSpecialValueRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	testassert.NoError(t, os.WriteFile(path, []byte(""), 0o600))
	key := envKeyForTest(t, "SPECIAL")
	value := "quote=\" slash=\\ line\nbreak tab	end"
	m := NewManagedEnv(path)
	testassert.NoError(t, m.TrySet(key, value))
	data, _ := os.ReadFile(path)
	body := string(data)
	testassert.Require(t, !strings.Contains(body, "line\nbreak") && strings.Contains(body, `\n`) && strings.Contains(body, `\t`), "literal newline written inside env value: %q; newline/tab escapes missing from quoted value: %q", body, body)

	captured, err := m.Capture([]string{key})
	testassert.NoError(t, err)
	testassert.Require(t, captured[key].Value == value, "value after reload = %q, want %q", captured[key].Value, value)
}

func TestWriteDotEnvAtomicNoPartialFileOnError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocked")
	testassert.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	badPath := filepath.Join(blocker, "subdir", ".env")

	err := writeDotEnvAtomic(badPath, []byte("KEY=val\n"))
	testassert.Require(t, err != nil, "expected error writing under a file-as-dir path")
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
		testassert.Check(t, strings.Contains(m, "malformed"), "malformed entry %q does not describe the problem", m)
	}
	{
		_, set := os.LookupEnv("PLAIN_KEY")
		testassert.Require(t, !set, "ReadDotEnvKeys set PLAIN_KEY in the process env")
	}
}

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
// writes the file; contention returns an operation error and mutates nothing;
// the external-key refusal is preserved.
func TestManagedEnvTrySetRows(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		testassert.NoError(t, os.WriteFile(path, []byte("# keep\n"), 0o600))
		key := envKeyForTest(t, "TRYSET_OK")
		m := NewManagedEnv(path)
		testassert.NoError(t, m.TrySet(key, "hello"))
		captured, err := m.Capture([]string{key})
		testassert.NoError(t, err)
		testassert.Require(t, captured[key].Managed && captured[key].Value == "hello", "capture after TrySet = %+v", captured[key])
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		testassert.Require(t, strings.Contains(string(data), key+"=hello") && strings.Contains(string(data), "# keep"), "file = %q, want the key line and the preserved comment", data)
	})
	t.Run("contention_mutates_nothing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		testassert.NoError(t, os.WriteFile(path, []byte("KEEP=me\n"), 0o600))
		key := envKeyForTest(t, "TRYSET_CONTEND")
		m := NewManagedEnv(path)
		holder, ok, err := atomicfs.TryAcquire(envLockPath(path))
		testassert.Require(t, err == nil && ok, "seed TryAcquire: (%v, %v)", ok, err)
		defer holder.Release()
		{
			err := m.TrySet(key, "secret")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "retry"), "TrySet under contention = %v, want a retryable error", err)
		}
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
		m := NewManagedEnv(path)
		err := m.TrySet(key, "overwrite")
		testassert.Require(t, errors.Is(err, ErrExternalKey), "TrySet err = %v, want ErrExternalKey", err)
		{
			got := os.Getenv(key)
			testassert.Require(t, got == "external", "env %s = %q, want the external value untouched", key, got)
		}
	})
}

// TestManagedEnvTryRemoveRows covers the one-attempt TryRemove contract:
// success removes the line; contention returns an operation error and mutates
// nothing; an absent key is a no-op; an externally defined key is refused.
func TestManagedEnvTryRemoveRows(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		key := envKeyForTest(t, "TRYRM_OK")
		seed := "# header\n" + key + "=val\nKEEP=me\n"
		testassert.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
		m := NewManagedEnv(path)
		testassert.NoError(t, m.TryRemove(key))
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
		m := NewManagedEnv(path)
		holder, ok, err := atomicfs.TryAcquire(envLockPath(path))
		testassert.Require(t, err == nil && ok, "seed TryAcquire: (%v, %v)", ok, err)
		defer holder.Release()
		{
			err := m.TryRemove(key)
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "retry"), "TryRemove under contention = %v, want a retryable error", err)
		}
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		testassert.Require(t, strings.Contains(string(data), key+"=val"), "file mutated under contention: %q", data)
	})
	t.Run("absent_noop", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		key := envKeyForTest(t, "TRYRM_NOOP")
		testassert.NoError(t, os.WriteFile(path, []byte("KEEP=me\n"), 0o600))
		m := NewManagedEnv(path)
		testassert.NoError(t, m.TryRemove(key))
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		testassert.Require(t, string(data) == "KEEP=me\n", "absent removal changed the file: %q", data)
	})
	t.Run("external_refused", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		key := envKeyForTest(t, "TRYRM_EXT")
		testassert.NoError(t, os.WriteFile(path, []byte(key+"=file\n"), 0o600))
		t.Setenv(key, "external")
		m := NewManagedEnv(path)
		err := m.TryRemove(key)
		testassert.Require(t, errors.Is(err, ErrExternalKey), "TryRemove err = %v, want ErrExternalKey", err)
		data, readErr := os.ReadFile(path)
		testassert.NoError(t, readErr)
		testassert.Require(t, strings.Contains(string(data), key+"=file"), "external removal changed the file: %q", data)
	})
}

// envBlockHolderEnv selects the child half of
// TestManagedEnvSetBlocksUntilForeignLockReleases and names the env lock path
// the child holds.
