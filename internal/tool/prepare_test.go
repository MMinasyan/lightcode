package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/permission"
)

func TestPrepareReadBindsParentDirectoryOnlyWhenLeafMissing(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "real.txt")
	if err := os.WriteFile(existing, []byte("one\ntwo\nthree"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Existing leaf: one file target, no parent-directory binding, so an
	// ordinary existing-file read requires no parent permission.
	args, err := NormalizeReadArgs(map[string]any{"path": "real.txt"}, 500)
	if err != nil {
		t.Fatalf("NormalizeReadArgs error = %v", err)
	}
	prepared, err := PrepareReadCall(root, "", config.ToolsConfig{ReadMaxLines: 500}, args)
	if err != nil {
		t.Fatalf("PrepareReadCall error = %v", err)
	}
	if len(prepared.Targets) != 1 {
		t.Fatalf("targets = %+v, want exactly the file target", prepared.Targets)
	}
	if prepared.Targets[0].CanonicalPath != existing {
		t.Fatalf("canonical target = %q, want %q", prepared.Targets[0].CanonicalPath, existing)
	}
	if prepared.Args["offset"] != json.Number("1") || prepared.Args["limit"] != json.Number("500") {
		t.Fatalf("normalized args = %+v, want default offset/limit as canonical integers 1/500", prepared.Args)
	}

	// Missing leaf: the parent directory is bound as a second declared
	// file.read target for the authorized suggestion listing.
	args, err = NormalizeReadArgs(map[string]any{"path": "missing.txt"}, 500)
	if err != nil {
		t.Fatalf("NormalizeReadArgs error = %v", err)
	}
	prepared, err = PrepareReadCall(root, "", config.ToolsConfig{ReadMaxLines: 500}, args)
	if err != nil {
		t.Fatalf("PrepareReadCall error = %v", err)
	}
	if len(prepared.Targets) != 2 {
		t.Fatalf("targets = %+v, want file plus parent directory", prepared.Targets)
	}
	if prepared.Targets[1].CanonicalPath != root {
		t.Fatalf("parent target = %+v, want directory binding on %q", prepared.Targets[1], root)
	}
}

func TestPrepareReadDisappearedLeafReturnsPlainNotFound(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gone.txt")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	args, err := NormalizeReadArgs(map[string]any{"path": "gone.txt"}, 500)
	if err != nil {
		t.Fatalf("NormalizeReadArgs error = %v", err)
	}
	prepared, err := PrepareReadCall(root, "", config.ToolsConfig{ReadMaxLines: 500}, args)
	if err != nil {
		t.Fatalf("PrepareReadCall error = %v", err)
	}
	if len(prepared.Targets) != 1 {
		t.Fatalf("targets = %+v, want the existing leaf only", prepared.Targets)
	}
	// The leaf existed during preparation but disappears before opening:
	// plain not-found, no undeclared fallback I/O, no suggestions.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	result, err := prepared.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if result != "read_file: file not found: "+filepath.Join(root, "gone.txt") {
		t.Fatalf("Execute result = %q, want plain not-found without suggestions", result)
	}
}

func TestSuggestFromBoundDirectoryReadsThroughNoFollowDescriptor(t *testing.T) {
	root := t.TempDir()
	realNames := filepath.Join(root, "real-dir")
	if err := os.Mkdir(realNames, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realNames, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherNames := filepath.Join(root, "other-dir")
	if err := os.Mkdir(otherNames, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherNames, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	parent := readTarget{displayPath: realNames, canonicalPath: realNames}
	got := suggestFromBoundDirectory(parent, filepath.Join(realNames, "confg.json"))
	if !strings.Contains(got, "Did you mean:") || !strings.Contains(got, "config.json") {
		t.Fatalf("suggestions = %q, want scored names from the bound directory", got)
	}

	// Replace the bound directory with a symlink pointing elsewhere: the
	// listing must go through the bound no-follow descriptor and fail
	// closed, never follow the replacement and list the redirected names.
	if err := os.RemoveAll(realNames); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherNames, realNames); err != nil {
		t.Fatal(err)
	}
	got = suggestFromBoundDirectory(parent, filepath.Join(realNames, "confg.json"))
	if got != "read_file: file not found: "+filepath.Join(realNames, "confg.json") {
		t.Fatalf("suggestions = %q, want plain not-found without redirected suggestions", got)
	}
}

// TestConsumedIntAtUse pins the point-of-use consumption of the canonical
// integer representation: validated lexemes parse to their int, and every
// non-canonical value class — absent, non-json.Number, non-integer lexeme,
// platform-int overflow — reads as zero without erroring, because
// normalization owns every rejection.
func TestConsumedIntAtUse(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		key  string
		want int
	}{
		{"canonical integer lexeme", map[string]any{"offset": json.Number("3")}, "offset", 3},
		{"negative lexeme", map[string]any{"offset": json.Number("-2")}, "offset", -2},
		{"absent key", map[string]any{}, "offset", 0},
		{"non-json.Number value", map[string]any{"offset": float64(3)}, "offset", 0},
		{"null value", map[string]any{"offset": nil}, "offset", 0},
		{"non-integer lexeme", map[string]any{"offset": json.Number("x")}, "offset", 0},
		{"platform-int overflow", map[string]any{"offset": json.Number("99999999999999999999")}, "offset", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := consumedIntAtUse(tc.args, tc.key); got != tc.want {
				t.Fatalf("consumedIntAtUse = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNormalizeReadArgsStrictIntegers(t *testing.T) {
	cases := []struct {
		name      string
		args      map[string]any
		wantErr   string
		wantOff   int
		wantLimit int
	}{
		{
			name:      "plain json.Number integers",
			args:      map[string]any{"path": "a.txt", "offset": json.Number("2"), "limit": json.Number("3")},
			wantOff:   2,
			wantLimit: 3,
		},
		{
			name:      "integral spellings 1.0 and 1e0",
			args:      map[string]any{"path": "a.txt", "offset": json.Number("2.0"), "limit": json.Number("1e0")},
			wantOff:   2,
			wantLimit: 1,
		},
		{
			name:      "integers beyond float64 precision stay exact",
			args:      map[string]any{"path": "a.txt", "offset": json.Number("9007199254740993"), "limit": json.Number("4")},
			wantOff:   9007199254740993,
			wantLimit: 4,
		},
		{
			name:      "long integral spelling accepted without the length cap",
			args:      map[string]any{"path": "a.txt", "offset": json.Number("1." + strings.Repeat("0", 600))},
			wantOff:   1,
			wantLimit: 500,
		},
		{
			name:      "nonpositive values take retained clamps",
			args:      map[string]any{"path": "a.txt", "offset": json.Number("-10"), "limit": json.Number("0")},
			wantOff:   1,
			wantLimit: 500,
		},
		{
			name:    "float64 rejected in the strict target path",
			args:    map[string]any{"path": "a.txt", "offset": float64(3)},
			wantErr: "must be an integer",
		},
		{
			name:      "absent keeps defaults",
			args:      map[string]any{"path": "a.txt"},
			wantOff:   1,
			wantLimit: 500,
		},
		{
			name:    "present null offset rejected",
			args:    map[string]any{"path": "a.txt", "offset": nil},
			wantErr: "must be an integer",
		},
		{
			name:    "present null limit rejected",
			args:    map[string]any{"path": "a.txt", "limit": nil},
			wantErr: "must be an integer",
		},
		{
			name:    "fraction rejected",
			args:    map[string]any{"path": "a.txt", "offset": json.Number("1.5")},
			wantErr: "must be a whole number",
		},
		{
			// A fraction so deep it rounds to 1 under 256-bit float
			// arithmetic must still be rejected by the exact lexeme parse.
			name:    "deep fraction below float rounding precision rejected",
			args:    map[string]any{"path": "a.txt", "offset": json.Number("1." + strings.Repeat("0", 80) + "1")},
			wantErr: "must be a whole number",
		},
		{
			name:    "float fraction rejected",
			args:    map[string]any{"path": "a.txt", "limit": 2.5},
			wantErr: "must be an integer",
		},
		{
			name:    "wrong type rejected",
			args:    map[string]any{"path": "a.txt", "offset": "3"},
			wantErr: "must be an integer",
		},
		{
			name:    "overflow rejected",
			args:    map[string]any{"path": "a.txt", "offset": json.Number("99999999999999999999")},
			wantErr: "is too large",
		},
		{
			name:    "missing path rejected",
			args:    map[string]any{"offset": float64(1)},
			wantErr: "path is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := NormalizeReadArgs(tc.args, 500)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("NormalizeReadArgs error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeReadArgs error = %v", err)
			}
			// Consumed integers normalize to their canonical-integer
			// json.Number lexemes — compared against the literal expected
			// lexeme, not through the producer: 2.0 becomes "2", 1e0
			// becomes "1", and a 601-digit integral spelling becomes "1".
			offsetNumber, ok := args["offset"].(json.Number)
			if !ok || string(offsetNumber) != strconv.Itoa(tc.wantOff) {
				t.Fatalf("normalized offset = %v (%T), want canonical integer lexeme %q", args["offset"], args["offset"], strconv.Itoa(tc.wantOff))
			}
			limitNumber, ok := args["limit"].(json.Number)
			if !ok || string(limitNumber) != strconv.Itoa(tc.wantLimit) {
				t.Fatalf("normalized limit = %v (%T), want canonical integer lexeme %q", args["limit"], args["limit"], strconv.Itoa(tc.wantLimit))
			}
		})
	}
}

func TestNormalizeWriteEditArgsStrictConsumedFields(t *testing.T) {
	if _, err := NormalizeWriteArgs(map[string]any{"path": "a.txt", "content": 123}); err == nil || !strings.Contains(err.Error(), "content must be a string") {
		t.Fatalf("NormalizeWriteArgs error = %v, want content type error", err)
	}
	if _, err := NormalizeWriteArgs(map[string]any{"path": "a.txt"}); err == nil || !strings.Contains(err.Error(), "content is required") {
		t.Fatalf("NormalizeWriteArgs error = %v, want content required error", err)
	}
	if _, err := NormalizeEditArgs(map[string]any{"path": "a.txt", "old_string": 1, "new_string": "x"}); err == nil || !strings.Contains(err.Error(), "old_string must be a string") {
		t.Fatalf("NormalizeEditArgs error = %v, want old_string type error", err)
	}
	if _, err := NormalizeEditArgs(map[string]any{"path": "a.txt", "old_string": "x", "new_string": true}); err == nil || !strings.Contains(err.Error(), "new_string must be a string") {
		t.Fatalf("NormalizeEditArgs error = %v, want new_string type error", err)
	}
	if _, err := NormalizeEditArgs(map[string]any{"path": "a.txt", "old_string": "x", "new_string": "y", "replace_all": "yes"}); err == nil || !strings.Contains(err.Error(), "replace_all must be a boolean") {
		t.Fatalf("NormalizeEditArgs error = %v, want replace_all type error", err)
	}
	if _, err := NormalizeEditArgs(map[string]any{"path": "a.txt", "old_string": "x", "new_string": "y", "replace_all": nil}); err == nil || !strings.Contains(err.Error(), "replace_all must be a boolean") {
		t.Fatalf("NormalizeEditArgs error = %v, want present-null replace_all rejected", err)
	}
	args, err := NormalizeEditArgs(map[string]any{"path": "a.txt", "old_string": "x", "new_string": ""})
	if err != nil {
		t.Fatalf("NormalizeEditArgs error = %v, want empty new_string accepted (deletion edit)", err)
	}
	if args["new_string"] != "" {
		t.Fatalf("normalized new_string = %v, want empty string retained", args["new_string"])
	}
}

func TestLegacyFileToolsKeepLenientArgumentParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewReadFile(config.ToolsConfig{ReadMaxLines: 500}, nil)

	// Fractional offset truncates under the retained lenient legacy parsing.
	result, err := tool.Execute(context.Background(), map[string]any{"path": path, "offset": 1.5})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.HasPrefix(result, "1\tone") {
		t.Fatalf("Execute result = %q, want read from truncated offset 1", result)
	}
	// Wrong-typed offset keeps its retained default instead of erroring.
	result, err = tool.Execute(context.Background(), map[string]any{"path": path, "offset": "3"})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.HasPrefix(result, "1\tone") {
		t.Fatalf("Execute result = %q, want read from default offset 1", result)
	}

	// Lenient write_file: missing content keeps its retained empty write.
	if _, err := NewWriteFile(nil, config.ToolsConfig{}).Execute(context.Background(), map[string]any{"path": filepath.Join(dir, "empty.txt")}); err != nil {
		t.Fatalf("write_file error = %v", err)
	}
	assertFileContent(t, filepath.Join(dir, "empty.txt"), "")
}

func TestNormalizeArgsStripsPrivateFieldsAndRetainsUnrelated(t *testing.T) {
	args, err := NormalizeWriteArgs(map[string]any{
		"path":                            "a.txt",
		"content":                         "x",
		canonicalPathParam:                "/forged/canonical",
		applyPatchReceiptParam:            map[string]any{"forged": true},
		"_lightcode_apply_patch_receipt2": "forged",
		"unrelated_metadata":              "kept",
	})
	if err != nil {
		t.Fatalf("NormalizeWriteArgs error = %v", err)
	}
	if _, forged := args[canonicalPathParam]; forged {
		t.Fatal("normalized args retained the private canonical path field")
	}
	if _, forged := args[applyPatchReceiptParam]; forged {
		t.Fatal("normalized args retained the private apply_patch receipt field")
	}
	if args["unrelated_metadata"] != "kept" {
		t.Fatalf("normalized args = %+v, want unrelated fields retained", args)
	}
	if args["path"] != "a.txt" || args["content"] != "x" {
		t.Fatalf("normalized args = %+v, want schema fields preserved", args)
	}
}

func TestReadFileClampsHugeWindowsToRemainingLines(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "lines.txt"), []byte("one\ntwo\nthree"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootCanonical, err := bindWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}

	// A MaxInt64 limit must not overflow end/last-line arithmetic: the
	// window clamps to the remaining lines. The strict normalizer
	// genuinely consumes the MaxInt64 lexeme.
	args, err := NormalizeReadArgs(map[string]any{
		"path":  "lines.txt",
		"limit": json.Number("9223372036854775807"),
	}, 500)
	if err != nil {
		t.Fatalf("NormalizeReadArgs error = %v", err)
	}
	prepared, err := PrepareReadCall(root, rootCanonical, config.ToolsConfig{ReadMaxLines: 500}, args)
	if err != nil {
		t.Fatalf("PrepareReadCall error = %v", err)
	}
	result, err := prepared.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	want := strings.Join([]string{"1\tone", "2\ttwo", "3\tthree"}, "\n")
	if result != want {
		t.Fatalf("Execute result = %q, want %q", result, want)
	}

	// Same clamp from a mid-file offset: neither the window end nor the
	// footer's offset+limit-1 may overflow.
	args, err = NormalizeReadArgs(map[string]any{
		"path":   "lines.txt",
		"offset": json.Number("2"),
		"limit":  json.Number("9223372036854775807"),
	}, 500)
	if err != nil {
		t.Fatalf("NormalizeReadArgs error = %v", err)
	}
	prepared, err = PrepareReadCall(root, rootCanonical, config.ToolsConfig{ReadMaxLines: 500}, args)
	if err != nil {
		t.Fatalf("PrepareReadCall error = %v", err)
	}
	result, err = prepared.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	want = strings.Join([]string{"2\ttwo", "3\tthree", "(Showing lines 2-3 of 3.)"}, "\n")
	if result != want {
		t.Fatalf("Execute result = %q, want %q", result, want)
	}
}

func TestScoreFileSuggestionsConsumesSuppliedEntries(t *testing.T) {
	entries := []os.DirEntry{
		fsDirEntry("main.go", false),
		fsDirEntry("sub", true),
		fsDirEntry("config.json", false),
		fsDirEntry("config.local.json", false),
		fsDirEntry("unrelated.md", false),
	}
	got := scoreFileSuggestions(entries, "confg.json")
	var names []string
	for _, s := range got {
		names = append(names, s.name)
	}
	// Prefix-ratio and small-distance matches only; directories skipped;
	// nearest distance first.
	if len(got) != 2 || names[0] != "config.json" || names[1] != "config.local.json" {
		t.Fatalf("suggestions = %v, want the two config matches nearest-first", names)
	}
	if got := scoreFileSuggestions(entries, "main.go"); len(got) != 1 || got[0].name != "main.go" {
		t.Fatalf("suggestions = %+v, want only the exact stem match", got)
	}
}

func fsDirEntry(name string, isDir bool) os.DirEntry {
	var mode os.FileMode = 0o644
	if isDir {
		mode = os.ModeDir
	}
	return fsFakeEntry{name: name, mode: mode}
}

type fsFakeEntry struct {
	name string
	mode os.FileMode
}

func (e fsFakeEntry) Name() string               { return e.name }
func (e fsFakeEntry) IsDir() bool                { return e.mode.IsDir() }
func (e fsFakeEntry) Type() os.FileMode          { return e.mode.Type() }
func (e fsFakeEntry) Info() (os.FileInfo, error) { return nil, os.ErrNotExist }

func TestPrepareReadRevalidatesRepointedWorkspaceRoot(t *testing.T) {
	base := t.TempDir()
	realA := filepath.Join(base, "realA")
	realB := filepath.Join(base, "realB")
	for _, dir := range []string{realA, realB} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(realA, "f.txt"), []byte("A-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realB, "f.txt"), []byte("B-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "w")
	if err := os.Symlink(realA, root); err != nil {
		t.Fatal(err)
	}

	// The target is addressed directly under the real directory, so its
	// canonical path is independent of the root symlink; only the bound
	// Workspace root canonical can detect the repoint.
	args, err := NormalizeReadArgs(map[string]any{"path": filepath.Join(realA, "f.txt")}, 500)
	if err != nil {
		t.Fatalf("NormalizeReadArgs error = %v", err)
	}
	prepared, err := PrepareReadCall(root, "", config.ToolsConfig{ReadMaxLines: 500}, args)
	if err != nil {
		t.Fatalf("PrepareReadCall error = %v", err)
	}
	// Baseline: with the binding intact the read succeeds.
	if result, err := prepared.Execute(context.Background()); err != nil || !strings.Contains(result, "A-content") {
		t.Fatalf("Execute = %q, %v; want A-content", result, err)
	}

	// Repoint the Workspace root after preparation: the closure must refuse
	// the changed root binding instead of reading a substituted tree.
	args, err = NormalizeReadArgs(map[string]any{"path": filepath.Join(realA, "f.txt")}, 500)
	if err != nil {
		t.Fatalf("NormalizeReadArgs error = %v", err)
	}
	prepared, err = PrepareReadCall(root, "", config.ToolsConfig{ReadMaxLines: 500}, args)
	if err != nil {
		t.Fatalf("PrepareReadCall error = %v", err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realB, root); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "workspace root changed") {
		t.Fatalf("Execute error = %v, want workspace root change refusal", err)
	}
	assertFileContent(t, filepath.Join(realA, "f.txt"), "A-content")
	assertFileContent(t, filepath.Join(realB, "f.txt"), "B-content")
}

func TestPrepareWriteRevalidatesRepointedWorkspaceRoot(t *testing.T) {
	base := t.TempDir()
	realA := filepath.Join(base, "realA")
	realB := filepath.Join(base, "realB")
	for _, dir := range []string{realA, realB} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(realA, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "w")
	if err := os.Symlink(realA, root); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}

	args, err := NormalizeWriteArgs(map[string]any{"path": filepath.Join(realA, "f.txt"), "content": "y"})
	if err != nil {
		t.Fatalf("NormalizeWriteArgs error = %v", err)
	}
	prepared, err := PrepareWriteCall(root, "", "", CapabilityOptions{}, store, args)
	if err != nil {
		t.Fatalf("PrepareWriteCall error = %v", err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realB, root); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "workspace root changed") {
		t.Fatalf("Execute error = %v, want workspace root change refusal", err)
	}
	// No mutation happened under either tree.
	assertFileContent(t, filepath.Join(realA, "f.txt"), "x")
	if _, err := os.Stat(filepath.Join(realB, "f.txt")); !os.IsNotExist(err) {
		t.Fatalf("realB f.txt stat err = %v, want absent (no substituted mutation)", err)
	}
}

func TestPreparePatchRevalidatesRepointedWorkspaceRoot(t *testing.T) {
	base := t.TempDir()
	realA := filepath.Join(base, "realA")
	realB := filepath.Join(base, "realB")
	for _, dir := range []string{realA, realB} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(realA, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "w")
	if err := os.Symlink(realA, root); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	input := applyPatchInput(t, "*** Update File: "+filepath.Join(realA, "f.txt")+"\n@@\n-hi\n+bye")

	args, err := NormalizePatchArgs(map[string]any{"input": input})
	if err != nil {
		t.Fatalf("NormalizePatchArgs error = %v", err)
	}
	rootCanonical, err := bindWorkspaceRoot(root)
	if err != nil {
		t.Fatalf("bindWorkspaceRoot error = %v", err)
	}
	prepared, err := PreparePatchCall(root, rootCanonical, "", CapabilityOptions{}, store, args)
	if err != nil {
		t.Fatalf("PreparePatchCall error = %v", err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realB, root); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "workspace root changed") {
		t.Fatalf("Execute error = %v, want workspace root change refusal", err)
	}
	assertFileContent(t, filepath.Join(realA, "f.txt"), "hi")
}

func TestPrepareTargetNilTrackerRepeatedReadsReturnContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("one\ntwo"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, err := NormalizeReadArgs(map[string]any{"path": path}, 500)
	if err != nil {
		t.Fatalf("NormalizeReadArgs error = %v", err)
	}
	prepared, err := PrepareReadCall(dir, "", config.ToolsConfig{ReadMaxLines: 500}, args)
	if err != nil {
		t.Fatalf("PrepareReadCall error = %v", err)
	}
	for i := 0; i < 2; i++ {
		result, err := prepared.Execute(context.Background())
		if err != nil {
			t.Fatalf("read %d error = %v", i+1, err)
		}
		// Target reads never return a cached-read notice.
		if !strings.Contains(result, "1\tone") || strings.Contains(result, "File unchanged") {
			t.Fatalf("read %d result = %q, want full content", i+1, result)
		}
	}
}

func TestPrepareReadTargetsExpressParentAllowDeny(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("parent denied denies missing-leaf suggestions", func(t *testing.T) {
		// Missing leaf: the parent directory is a declared target, so a
		// parent denial is expressible before any enumeration.
		args, err := NormalizeReadArgs(map[string]any{"path": "confg.json"}, 500)
		if err != nil {
			t.Fatalf("NormalizeReadArgs error = %v", err)
		}
		prepared, err := PrepareReadCall(dir, "", config.ToolsConfig{ReadMaxLines: 500}, args)
		if err != nil {
			t.Fatalf("PrepareReadCall error = %v", err)
		}
		if len(prepared.Targets) != 2 {
			t.Fatalf("targets = %+v, want file plus parent directory", prepared.Targets)
		}
		// The Step-9 conversion checks every target; a parent denial must
		// refuse the call before execution, so no enumeration runs.
		allowed := true
		for _, target := range prepared.Targets {
			if target.CanonicalPath == dir {
				allowed = false
				break
			}
		}
		if allowed {
			t.Fatal("parent denial was not expressible through the prepared targets")
		}
		if _, err := prepared.Execute(context.Background()); err != nil {
			t.Fatalf("closure execution itself failed = %v, want refusal to come from the permission layer", err)
		}
	})

	t.Run("parent denied allows existing-file read", func(t *testing.T) {
		// Existing leaf: only the file target is declared, so a parent
		// denial does not block an allowed existing-file read.
		args, err := NormalizeReadArgs(map[string]any{"path": "config.json"}, 500)
		if err != nil {
			t.Fatalf("NormalizeReadArgs error = %v", err)
		}
		prepared, err := PrepareReadCall(dir, "", config.ToolsConfig{ReadMaxLines: 500}, args)
		if err != nil {
			t.Fatalf("PrepareReadCall error = %v", err)
		}
		if len(prepared.Targets) != 1 || prepared.Targets[0].CanonicalPath != filepath.Join(dir, "config.json") {
			t.Fatalf("targets = %+v, want only the file target (parent not required)", prepared.Targets)
		}
		result, err := prepared.Execute(context.Background())
		if err != nil || !strings.Contains(result, "{}") {
			t.Fatalf("Execute = %q, %v; want file content (parent permission not required)", result, err)
		}
	})
}

func TestPreparedEditAndWritePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "file.txt")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}

	// Prepared write through the exported entry (no tracker, nil-tracker
	// target path) overwrites content.
	args, err := NormalizeWriteArgs(map[string]any{"path": path, "content": "changed"})
	if err != nil {
		t.Fatalf("NormalizeWriteArgs error = %v", err)
	}
	prepared, err := PrepareWriteCall(dir, "", "", CapabilityOptions{}, store, args)
	if err != nil {
		t.Fatalf("PrepareWriteCall error = %v", err)
	}
	if len(prepared.Targets) != 1 || prepared.Targets[0].CanonicalPath != path {
		t.Fatalf("targets = %+v, want the single canonical write target", prepared.Targets)
	}
	if result, err := prepared.Execute(context.Background()); err != nil || result != "Wrote "+path+"." {
		t.Fatalf("Execute = %q, %v; want write result", result, err)
	}
	assertFileContent(t, path, "changed")

	// Prepared edit through the exported entry applies the replacement.
	editArgs, err := NormalizeEditArgs(map[string]any{"path": path, "old_string": "changed", "new_string": "final"})
	if err != nil {
		t.Fatalf("NormalizeEditArgs error = %v", err)
	}
	editPrepared, err := PrepareEditCall(dir, "", "", CapabilityOptions{}, store, editArgs)
	if err != nil {
		t.Fatalf("PrepareEditCall error = %v", err)
	}
	if len(editPrepared.Targets) != 1 || editPrepared.Targets[0].CanonicalPath != path {
		t.Fatalf("edit targets = %+v, want the single canonical edit target", editPrepared.Targets)
	}
	if result, err := editPrepared.Execute(context.Background()); err != nil || !strings.Contains(result, "lines 1-") {
		t.Fatalf("Execute = %q, %v; want edit summary", result, err)
	}
	assertFileContent(t, path, "final")

	// A prepared edit whose old_string is missing fails inside the closure
	// without mutating.
	missing, err := NormalizeEditArgs(map[string]any{"path": path, "old_string": "absent", "new_string": "x"})
	if err != nil {
		t.Fatalf("NormalizeEditArgs error = %v", err)
	}
	missingPrepared, err := PrepareEditCall(dir, "", "", CapabilityOptions{}, store, missing)
	if err != nil {
		t.Fatalf("PrepareEditCall error = %v", err)
	}
	if _, err := missingPrepared.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Execute error = %v, want missing match error", err)
	}
	assertFileContent(t, path, "final")

	// A prepared write to a missing nested path creates the required
	// parent directories as part of the single file.write target.
	nested := filepath.Join(dir, "created", "deep", "new.txt")
	nestedArgs, err := NormalizeWriteArgs(map[string]any{"path": nested, "content": "made"})
	if err != nil {
		t.Fatalf("NormalizeWriteArgs error = %v", err)
	}
	nestedPrepared, err := PrepareWriteCall(dir, "", "", CapabilityOptions{}, store, nestedArgs)
	if err != nil {
		t.Fatalf("PrepareWriteCall error = %v", err)
	}
	if _, err := nestedPrepared.Execute(context.Background()); err != nil {
		t.Fatalf("Execute error = %v, want required parent creation", err)
	}
	assertFileContent(t, nested, "made")
}

// TestPrepareWriteAndEditDiscardSnapshotClaimOnPostSnapshotRevalidationFailure
// extends the post-snapshot repoint window: when the post-snapshot binding
// revalidation refuses a repointed target, the unmutated snapshot claim must
// be discarded exactly like any other pre-mutation failure (the retained
// legacy discard-before-mutation behavior), not left claimed.
func TestPrepareWriteAndEditDiscardSnapshotClaimOnPostSnapshotRevalidationFailure(t *testing.T) {
	dir := t.TempDir()
	real1 := filepath.Join(dir, "real1.txt")
	real2 := filepath.Join(dir, "real2.txt")
	for _, p := range []string{real1, real2} {
		if err := os.WriteFile(p, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link.txt")
	repointTo := func(target string) {
		t.Helper()
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(real1, link); err != nil {
		t.Fatal(err)
	}

	// Write path: the snapshot hook flips the approved symlink after
	// capture, exactly in the snapshot-to-mutation window. The execution
	// must fail on the changed canonical target AND discard the snapshot
	// claim; neither file may be mutated.
	repointTo(real1)
	store := &applyPatchStore{turn: 1, onSnapshot: func(call int) {
		if call == 1 {
			repointTo(real2)
		}
	}}
	args, err := NormalizeWriteArgs(map[string]any{"path": link, "content": "after"})
	if err != nil {
		t.Fatalf("NormalizeWriteArgs error = %v", err)
	}
	prepared, err := PrepareWriteCall(dir, "", "", CapabilityOptions{}, store, args)
	if err != nil {
		t.Fatalf("PrepareWriteCall error = %v", err)
	}
	if _, err := prepared.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("write Execute error = %v, want post-snapshot canonical change refusal", err)
	}
	if len(store.discards) != 1 || store.discards[0] != real1 {
		t.Fatalf("write discards = %v, want the unmutated snapshot claim for %s discarded once", store.discards, real1)
	}
	assertFileContent(t, real1, "before")
	assertFileContent(t, real2, "before")

	// Edit path: same window, same discard-before-mutation behavior.
	repointTo(real1)
	store2 := &applyPatchStore{turn: 1, onSnapshot: func(call int) {
		if call == 1 {
			repointTo(real2)
		}
	}}
	editArgs, err := NormalizeEditArgs(map[string]any{"path": link, "old_string": "before", "new_string": "after"})
	if err != nil {
		t.Fatalf("NormalizeEditArgs error = %v", err)
	}
	editPrepared, err := PrepareEditCall(dir, "", "", CapabilityOptions{}, store2, editArgs)
	if err != nil {
		t.Fatalf("PrepareEditCall error = %v", err)
	}
	if _, err := editPrepared.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("edit Execute error = %v, want post-snapshot canonical change refusal", err)
	}
	if len(store2.discards) != 1 || store2.discards[0] != real1 {
		t.Fatalf("edit discards = %v, want the unmutated snapshot claim for %s discarded once", store2.discards, real1)
	}
	assertFileContent(t, real1, "before")
	assertFileContent(t, real2, "before")
}

func TestPreparePatchExecutesParsedPatchWithoutReparse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	input := applyPatchInput(t, "*** Update File: a.txt\n@@\n-hi\n+bye")

	args := map[string]any{"input": input}
	prepared, err := PreparePatchCall(dir, "", "", CapabilityOptions{}, store, args)
	if err != nil {
		t.Fatalf("PreparePatchCall error = %v", err)
	}
	if len(prepared.Targets) != 1 || prepared.Targets[0].CanonicalPath != filepath.Join(dir, "a.txt") {
		t.Fatalf("targets = %+v, want the single canonical patch target", prepared.Targets)
	}

	// Swapping the arguments after preparation must not leak into the
	// execution closure: the patch was parsed exactly once and the closure
	// consumes that parsed plan without redecoding or reparsing.
	args["input"] = applyPatchInput(t, "*** Update File: a.txt\n@@\n-hi\n+TAMPERED")
	result, err := prepared.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if !strings.Contains(result.Result, "Success. Updated the following files:") {
		t.Fatalf("Execute result = %q, want success summary", result.Result)
	}
	// The structured result carries the captured preview data.
	if len(result.Previews) != 1 || result.Previews[0].Op != "M" || result.Previews[0].Path != "a.txt" {
		t.Fatalf("previews = %+v, want the applied M preview for a.txt", result.Previews)
	}
	assertFileContent(t, filepath.Join(dir, "a.txt"), "bye")
}

func TestPreparePatchClosureRevalidatesRepointedTarget(t *testing.T) {
	dir := t.TempDir()
	real1 := filepath.Join(dir, "real1.txt")
	real2 := filepath.Join(dir, "real2.txt")
	if err := os.WriteFile(real1, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real2, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real1, link); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	input := applyPatchInput(t, "*** Update File: link.txt\n@@\n-hi\n+bye")

	prepared, err := PreparePatchCall(dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	if err != nil {
		t.Fatalf("PreparePatchCall error = %v", err)
	}
	// Repoint the approved leaf after preparation: the closure must refuse
	// the changed canonical target instead of silently substituting it.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real2, link); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical path changed") {
		t.Fatalf("Execute error = %v, want canonical change refusal", err)
	}
	assertFileContent(t, real1, "hi")
	assertFileContent(t, real2, "hi")
}

func TestPreparePatchPartialFailureReportsCommittedFiles(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	if err := os.WriteFile(first, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}
	// After the first op commits its snapshot, tamper with the second op's
	// file: the per-op current-content check must refuse it and the partial
	// summary must report the committed first file.
	store.onSnapshot = func(call int) {
		if call == 1 {
			if err := os.WriteFile(second, []byte("tampered"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	input := applyPatchInput(t, "*** Update File: first.txt\n@@\n-one\n+ONE\n*** Update File: second.txt\n@@\n-two\n+TWO")

	prepared, err := PreparePatchCall(dir, "", "", CapabilityOptions{}, store, map[string]any{"input": input})
	if err != nil {
		t.Fatalf("PreparePatchCall error = %v", err)
	}
	_, err = prepared.Execute(context.Background())
	if err == nil {
		t.Fatal("Execute succeeded, want partial-failure error")
	}
	if !strings.Contains(err.Error(), "M first.txt") || !strings.Contains(err.Error(), "changed after validation") {
		t.Fatalf("Execute error = %v, want committed-files summary plus validation failure", err)
	}
	assertFileContent(t, first, "ONE")
	assertFileContent(t, second, "tampered")
}

func TestPrepareWriteRevalidatesWriteDirWitness(t *testing.T) {
	root := t.TempDir()
	realA := filepath.Join(root, "realA")
	realB := filepath.Join(root, "realB")
	if err := os.Mkdir(realA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(realB, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := filepath.Join(root, "wd")
	if err := os.Symlink(realA, wd); err != nil {
		t.Fatal(err)
	}
	store := &applyPatchStore{turn: 1}

	// The target is addressed directly through the real directory while the
	// write-dir witness goes through the symlink; the target canonical path
	// is inside the witness boundary at preparation.
	args, err := NormalizeWriteArgs(map[string]any{"path": filepath.Join(realA, "file.txt"), "content": "x"})
	if err != nil {
		t.Fatalf("NormalizeWriteArgs error = %v", err)
	}
	prepared, err := PrepareWriteCall(root, "", "", CapabilityOptions{WriteDir: wd}, store, args)
	if err != nil {
		t.Fatalf("PrepareWriteCall error = %v", err)
	}
	if _, err := prepared.Execute(context.Background()); err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	assertFileContent(t, filepath.Join(realA, "file.txt"), "x")

	// Prepare a second write while the witness still resolves to realA,
	// then flip the symlink to the other directory before execution: the
	// target canonical path is unchanged, but the closure must refuse it:
	// the bound canonical write-dir boundary changed.
	args, err = NormalizeWriteArgs(map[string]any{"path": filepath.Join(realA, "file.txt"), "content": "y"})
	if err != nil {
		t.Fatalf("NormalizeWriteArgs error = %v", err)
	}
	prepared, err = PrepareWriteCall(root, "", "", CapabilityOptions{WriteDir: wd}, store, args)
	if err != nil {
		t.Fatalf("PrepareWriteCall error = %v", err)
	}
	if err := os.Remove(wd); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realB, wd); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical write_dir changed") {
		t.Fatalf("Execute error = %v, want write-dir revalidation refusal", err)
	}
	assertFileContent(t, filepath.Join(realA, "file.txt"), "x")

	// A repointed write-dir symlink fails even when the target would sit
	// inside both the old and the new directory: the canonical boundary
	// comparison catches it where a fresh containment check would pass.
	// Restore the witness first so preparation binds the old boundary.
	if err := os.Remove(wd); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realA, wd); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(realA, "B")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	args, err = NormalizeWriteArgs(map[string]any{"path": filepath.Join(nested, "f.txt"), "content": "n"})
	if err != nil {
		t.Fatalf("NormalizeWriteArgs error = %v", err)
	}
	prepared, err = PrepareWriteCall(root, "", "", CapabilityOptions{WriteDir: wd}, store, args)
	if err != nil {
		t.Fatalf("PrepareWriteCall error = %v", err)
	}
	// wd still points at realA during preparation: boundary realA, target
	// realA/B/f.txt inside it. Flip wd to the nested realA/B — the target
	// stays inside both boundaries.
	if err := os.Remove(wd); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nested, wd); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical write_dir changed") {
		t.Fatalf("Execute error = %v, want write-dir canonical change refusal (target inside both boundaries)", err)
	}
	if _, err := os.Stat(filepath.Join(nested, "f.txt")); !os.IsNotExist(err) {
		t.Fatalf("nested f.txt stat err = %v, want absent (no mutation)", err)
	}
}

func TestNormalizePatchArgsStrictInputType(t *testing.T) {
	if _, err := NormalizePatchArgs(map[string]any{"input": 42}); err == nil || !strings.Contains(err.Error(), "apply_patch: input must be a string") {
		t.Fatalf("NormalizePatchArgs error = %v, want input type error", err)
	}
	if _, err := NormalizePatchArgs(map[string]any{"input": nil}); err == nil || !strings.Contains(err.Error(), "apply_patch: input must be a string") {
		t.Fatalf("NormalizePatchArgs error = %v, want present-null input rejected", err)
	}
	args, err := NormalizePatchArgs(map[string]any{"input": "*** Begin Patch\n*** End Patch", "unrelated": "kept"})
	if err != nil {
		t.Fatalf("NormalizePatchArgs error = %v", err)
	}
	if args["unrelated"] != "kept" {
		t.Fatalf("normalized args = %+v, want unrelated fields retained", args)
	}
	// Missing input stays a later patch-syntax error, not a normalization one.
	args, err = NormalizePatchArgs(map[string]any{})
	if err != nil {
		t.Fatalf("NormalizePatchArgs error = %v, want missing input tolerated at normalization", err)
	}
	if _, err := PreparePatchCall(t.TempDir(), "", "", CapabilityOptions{}, &applyPatchStore{turn: 1}, args); err == nil || !strings.Contains(err.Error(), "input is empty") {
		t.Fatalf("PreparePatchCall error = %v, want patch-syntax error", err)
	}
}

// patchWriteDirWitnessFixture builds the repointed-write-dir scenario shared
// by the preparation and receipt oracles: the target realA/B/a.txt sits
// inside both the original boundary (realA) and the repointed one (realA/B),
// so a fresh containment check would pass and only the bound canonical
// write-dir witness can refuse the change.
type patchWriteDirWitnessFixture struct {
	root   string
	target string
	wd     string
	flip   func()
}

func newPatchWriteDirWitnessFixture(t *testing.T) patchWriteDirWitnessFixture {
	t.Helper()
	root := t.TempDir()
	realA := filepath.Join(root, "realA")
	nested := filepath.Join(realA, "B")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(nested, "a.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd := filepath.Join(root, "wd")
	if err := os.Symlink(realA, wd); err != nil {
		t.Fatal(err)
	}
	return patchWriteDirWitnessFixture{
		root:   root,
		target: target,
		wd:     wd,
		flip: func() {
			if err := os.Remove(wd); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(nested, wd); err != nil {
				t.Fatal(err)
			}
		},
	}
}

func patchWriteDirWitnessInput(t *testing.T, target string) string {
	t.Helper()
	return applyPatchInput(t, "*** Update File: "+target+"\n@@\n-x\n+bye")
}

// TestPreparePatchRevalidatesWriteDirWitness proves apply_patch obeys the
// same canonical write-dir binding rule as the file tools: a write-dir
// symlink repointed after preparation fails execution with the
// binding-change error even when the repointed directory still contains the
// target — and nothing is mutated.
func TestPreparePatchRevalidatesWriteDirWitness(t *testing.T) {
	t.Run("prepared_closure", func(t *testing.T) {
		f := newPatchWriteDirWitnessFixture(t)
		store := &applyPatchStore{turn: 1}
		rootCanonical, err := bindWorkspaceRoot(f.root)
		if err != nil {
			t.Fatalf("bindWorkspaceRoot error = %v", err)
		}
		writeDirCanonical, err := bindWriteDir(f.root, "apply_patch", f.wd)
		if err != nil {
			t.Fatalf("bindWriteDir error = %v", err)
		}
		prepared, err := PreparePatchCall(f.root, rootCanonical, writeDirCanonical, CapabilityOptions{WriteDir: f.wd}, store, map[string]any{"input": patchWriteDirWitnessInput(t, f.target)})
		if err != nil {
			t.Fatalf("PreparePatchCall error = %v", err)
		}
		f.flip()
		if _, err := prepared.Execute(context.Background()); err == nil || !strings.Contains(err.Error(), "approved canonical write_dir changed") {
			t.Fatalf("Execute error = %v, want write-dir canonical change refusal", err)
		}
		assertFileContent(t, f.target, "x")
	})

	// The approval receipt binds the canonical write-dir at authorization;
	// repointing the symlink inside the approval callback must fail the
	// same binding-change refusal at execution.
	t.Run("receipt", func(t *testing.T) {
		f := newPatchWriteDirWitnessFixture(t)
		store := &applyPatchStore{turn: 1}
		tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, f.root)
		check := rulesCheck(f.root, permission.Rules{Ask: []string{"**"}})
		ask := func(_ context.Context, _ permission.Request) permission.ResponseAction {
			f.flip()
			return permission.ResponseAllow
		}
		wrapped := WrapWithPermissionAtRootWithOptions(tool, f.root, check, ask, CapabilityOptions{WriteDir: f.wd})
		_, err := wrapped.Execute(context.Background(), map[string]any{"input": patchWriteDirWitnessInput(t, f.target)})
		if err == nil || !strings.Contains(err.Error(), "approved canonical write_dir changed") {
			t.Fatalf("Execute error = %v, want write-dir canonical change refusal through the receipt", err)
		}
		assertFileContent(t, f.target, "x")
	})
}

func TestPreparePatchRejectsOutsideWriteDirAtPreparation(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "in")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inside, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootCanonical, err := bindWorkspaceRoot(root)
	if err != nil {
		t.Fatalf("bindWorkspaceRoot error = %v", err)
	}
	writeDirCanonical, err := bindWriteDir(root, "apply_patch", inside)
	if err != nil {
		t.Fatalf("bindWriteDir error = %v", err)
	}
	outside := filepath.Join(root, "out")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A patch destination outside write_dir is rejected at preparation,
	// before authorization: no targets are declared and nothing runs.
	input := applyPatchInput(t, "*** Add File: "+filepath.Join(outside, "new.txt")+"\n+data")
	args, err := NormalizePatchArgs(map[string]any{"input": input})
	if err != nil {
		t.Fatalf("NormalizePatchArgs error = %v", err)
	}
	prepared, err := PreparePatchCall(root, rootCanonical, writeDirCanonical, CapabilityOptions{WriteDir: inside}, &applyPatchStore{turn: 1}, args)
	if err == nil || !strings.Contains(err.Error(), "outside write_dir") {
		t.Fatalf("PreparePatchCall error = %v, want outside-write_dir rejection at preparation", err)
	}
	if prepared != nil {
		t.Fatalf("prepared call = %+v, want nil after preparation rejection", prepared)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside new.txt stat err = %v, want absent", err)
	}

	// A source outside write_dir is likewise rejected at preparation.
	input = applyPatchInput(t, "*** Update File: "+filepath.Join(outside, "b.txt")+"\n@@\n-y\n+z")
	args, err = NormalizePatchArgs(map[string]any{"input": input})
	if err != nil {
		t.Fatalf("NormalizePatchArgs error = %v", err)
	}
	if _, err := PreparePatchCall(root, rootCanonical, writeDirCanonical, CapabilityOptions{WriteDir: inside}, &applyPatchStore{turn: 1}, args); err == nil || !strings.Contains(err.Error(), "outside write_dir") {
		t.Fatalf("PreparePatchCall error = %v, want outside-write_dir source rejection at preparation", err)
	}

	// An inside patch prepares and applies normally.
	input = applyPatchInput(t, "*** Update File: "+filepath.Join(inside, "a.txt")+"\n@@\n-x\n+fixed")
	args, err = NormalizePatchArgs(map[string]any{"input": input})
	if err != nil {
		t.Fatalf("NormalizePatchArgs error = %v", err)
	}
	prepared, err = PreparePatchCall(root, rootCanonical, writeDirCanonical, CapabilityOptions{WriteDir: inside}, &applyPatchStore{turn: 1}, args)
	if err != nil {
		t.Fatalf("PreparePatchCall error = %v, want inside-write_dir patch prepared", err)
	}
	if _, err := prepared.Execute(context.Background()); err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	assertFileContent(t, filepath.Join(inside, "a.txt"), "fixed")
}

// TestFileToolsRootWitnessSurvivesApproval repoints the Workspace root
// symlink inside the approval callback: the authorization-time root
// witness must make the inner preparation fail on change instead of
// re-binding a fresh root, with no execution and no mutation.
func TestFileToolsRootWitnessSurvivesApproval(t *testing.T) {
	base := t.TempDir()
	realA := filepath.Join(base, "realA")
	realB := filepath.Join(base, "realB")
	for _, dir := range []string{realA, realB} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(realA, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "w")
	if err := os.Symlink(realA, root); err != nil {
		t.Fatal(err)
	}

	// reset makes every subtest self-contained: an earlier subtest's
	// approval callback repoints the root symlink and may mutate the
	// fixture file, so each subtest re-binds w -> realA, restores the
	// fixture content and clears any residue in realB.
	reset := func() {
		t.Helper()
		if err := os.Remove(root); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realA, root); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realA, "f.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(realB, "f.txt")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	flipRoot := func() {
		if err := os.Remove(root); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realB, root); err != nil {
			t.Fatal(err)
		}
	}
	askAll := func(t *testing.T, hook func()) (CheckFunc, AskFunc) {
		check := rulesCheck(base, permission.Rules{Ask: []string{"**"}})
		ask := func(_ context.Context, req permission.Request) permission.ResponseAction {
			hook()
			return permission.ResponseAllow
		}
		return check, ask
	}

	t.Run("write_file", func(t *testing.T) {
		store := &applyPatchStore{turn: 1}
		reset()

		tool := NewWriteFileWithSnapshotAtRoot(store, nil, config.ToolsConfig{}, root)
		check, ask := askAll(t, flipRoot)
		wrapped := WrapWithPermissionAtRootWithOptions(tool, root, check, ask, CapabilityOptions{})

		_, err := wrapped.Execute(context.Background(), map[string]any{
			"path":    filepath.Join(realA, "f.txt"),
			"content": "y",
		})
		if err == nil || !strings.Contains(err.Error(), "workspace root changed") {
			t.Fatalf("Execute error = %v, want root witness change refusal after approval", err)
		}
		assertFileContent(t, filepath.Join(realA, "f.txt"), "x")
		if _, err := os.Stat(filepath.Join(realB, "f.txt")); !os.IsNotExist(err) {
			t.Fatalf("realB f.txt stat err = %v, want absent (no execution)", err)
		}
	})

	t.Run("edit_file", func(t *testing.T) {
		store := &applyPatchStore{turn: 1}
		reset()

		tool := NewEditFileWithSnapshotAtRoot(store, nil, config.ToolsConfig{}, root)
		check, ask := askAll(t, flipRoot)
		wrapped := WrapWithPermissionAtRootWithOptions(tool, root, check, ask, CapabilityOptions{})

		_, err := wrapped.Execute(context.Background(), map[string]any{
			"path":       filepath.Join(realA, "f.txt"),
			"old_string": "x",
			"new_string": "y",
		})
		if err == nil || !strings.Contains(err.Error(), "workspace root changed") {
			t.Fatalf("Execute error = %v, want root witness change refusal after approval", err)
		}
		assertFileContent(t, filepath.Join(realA, "f.txt"), "x")
	})

	t.Run("apply_patch", func(t *testing.T) {
		store := &applyPatchStore{turn: 1}
		reset()

		tool := NewApplyPatchWithSnapshotAtRoot(store, NewFileTracker(), config.ToolsConfig{}, root)
		check, ask := askAll(t, flipRoot)
		wrapped := WrapWithPermissionAtRoot(tool, root, check, ask)

		input := applyPatchInput(t, "*** Update File: "+filepath.Join(realA, "f.txt")+"\n@@\n-x\n+patched")
		_, err := wrapped.Execute(context.Background(), map[string]any{"input": input})
		if err == nil || !strings.Contains(err.Error(), "workspace root changed") {
			t.Fatalf("Execute error = %v, want root witness change refusal after approval", err)
		}
		assertFileContent(t, filepath.Join(realA, "f.txt"), "x")
	})
}
