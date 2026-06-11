package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/version"
)

func TestLatestTag(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		location string
		wantTag  string
		wantErr  string
	}{
		{"redirect with tag", http.StatusFound, "https://github.com/MMinasyan/lightcode/releases/tag/v0.0.2", "v0.0.2", ""},
		{"malformed location", http.StatusFound, "https://github.com/MMinasyan/lightcode/releases", "", "unexpected redirect"},
		{"non-semver tag", http.StatusFound, "https://github.com/MMinasyan/lightcode/releases/tag/nightly", "", "unexpected tag"},
		{"non-redirect", http.StatusOK, "", "", "unexpected status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.location != "" {
					w.Header().Set("Location", tc.location)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			old := LatestEndpoint
			LatestEndpoint = srv.URL
			defer func() { LatestEndpoint = old }()

			tag, err := LatestTag()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tag != tc.wantTag {
				t.Fatalf("tag = %q, want %q", tag, tc.wantTag)
			}
		})
	}
}

func TestAssetURLs(t *testing.T) {
	asset, sums := AssetURLs("", "v0.0.4")
	if asset != "https://github.com/MMinasyan/lightcode/releases/download/v0.0.4/"+AssetName {
		t.Fatalf("channel asset URL = %q", asset)
	}
	if sums != "https://github.com/MMinasyan/lightcode/releases/download/v0.0.4/SHA256SUMS" {
		t.Fatalf("channel sums URL = %q", sums)
	}

	// A base override composes <base>/<asset> with no tag in the path,
	// matching the installer's semantics.
	asset, sums = AssetURLs("http://example.test/dir/", "v0.0.4")
	if asset != "http://example.test/dir/"+AssetName {
		t.Fatalf("base asset URL = %q", asset)
	}
	if sums != "http://example.test/dir/SHA256SUMS" {
		t.Fatalf("base sums URL = %q", sums)
	}
	if strings.Contains(asset, "v0.0.4") {
		t.Fatal("base URL must not contain the tag")
	}
}

func TestVerifySHA256(t *testing.T) {
	data := []byte("release bytes")
	sum := sha256.Sum256(data)
	good := hex.EncodeToString(sum[:])

	manifest := func(assetSum string) []byte {
		// The real manifest lists install.sh first; selection is by exact
		// filename field.
		return []byte("0123456789abcdef  install.sh\n" + assetSum + "  " + AssetName + "\n")
	}

	if err := VerifySHA256(data, manifest(good), AssetName); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if err := VerifySHA256(data, manifest(strings.Repeat("0", 64)), AssetName); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("mismatch err = %v", err)
	}
	if err := VerifySHA256(data, []byte("deadbeef  other-file\n"), AssetName); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("missing entry err = %v", err)
	}
}

// buildTarball assembles a gzip tarball with the given members.
func buildTarball(t *testing.T, members map[string]struct {
	body     string
	typeflag byte
	linkname string
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, m := range members {
		hdr := &tar.Header{Name: name, Mode: 0o755, Typeflag: m.typeflag, Linkname: m.linkname}
		if m.typeflag == tar.TypeReg {
			hdr.Size = int64(len(m.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if m.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(m.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type member = struct {
	body     string
	typeflag byte
	linkname string
}

func TestExtractBinary(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		tb := buildTarball(t, map[string]member{"lightcode": {body: "BINARY", typeflag: tar.TypeReg}})
		data, err := ExtractBinary(tb)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "BINARY" {
			t.Fatalf("extracted %q", data)
		}
	})

	t.Run("missing member", func(t *testing.T) {
		tb := buildTarball(t, map[string]member{"otherfile": {body: "X", typeflag: tar.TypeReg}})
		if _, err := ExtractBinary(tb); err == nil || !strings.Contains(err.Error(), "no lightcode member") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("symlink member rejected", func(t *testing.T) {
		tb := buildTarball(t, map[string]member{"lightcode": {typeflag: tar.TypeSymlink, linkname: "/bin/sh"}})
		if _, err := ExtractBinary(tb); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("dir member rejected", func(t *testing.T) {
		tb := buildTarball(t, map[string]member{"lightcode": {typeflag: tar.TypeDir}})
		if _, err := ExtractBinary(tb); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("short body", func(t *testing.T) {
		tb := buildTarball(t, map[string]member{"lightcode": {body: "BINARY", typeflag: tar.TypeReg}})
		if _, err := ExtractBinary(tb[:len(tb)/2]); err == nil {
			t.Fatal("truncated archive must error")
		}
	})

	t.Run("not gzip", func(t *testing.T) {
		if _, err := ExtractBinary([]byte("plain bytes")); err == nil {
			t.Fatal("non-gzip input must error")
		}
	})
}

// writeScript stages a shell script as the fake binary.
func smokeScript(body string) []byte {
	return []byte("#!/bin/sh\n" + body + "\n")
}

func TestStageAndSmokeAndReplace(t *testing.T) {
	t.Run("single inode from stage through replace", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "lightcode")
		if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		staged, err := Stage(dir, smokeScript(`echo "lightcode v9.9.9 (linux/amd64)"`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(filepath.Base(staged), ".lightcode.tmp.") {
			t.Fatalf("staged name = %q", staged)
		}
		info, err := os.Stat(staged)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("staged mode = %v", info.Mode())
		}
		stagedIno := info.Sys().(*syscall.Stat_t).Ino

		if err := SmokeCheck(staged, "v9.9.9"); err != nil {
			t.Fatalf("smoke: %v", err)
		}
		if err := Replace(staged, target); err != nil {
			t.Fatal(err)
		}
		finalInfo, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if finalInfo.Sys().(*syscall.Stat_t).Ino != stagedIno {
			t.Fatal("the promoted inode is not the smoke-checked inode")
		}
	})

	t.Run("stale staged files do not block", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".lightcode.tmp.stale"), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Stage(dir, []byte("new")); err != nil {
			t.Fatalf("pre-existing stale temp file blocked staging: %v", err)
		}
	})

	t.Run("smoke fails on wrong tag", func(t *testing.T) {
		dir := t.TempDir()
		staged, err := Stage(dir, smokeScript(`echo "lightcode v0.0.1 (linux/amd64)"`))
		if err != nil {
			t.Fatal(err)
		}
		if err := SmokeCheck(staged, "v9.9.9"); err == nil || !strings.Contains(err.Error(), "did not report") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("smoke fails on nonzero exit even with matching tag", func(t *testing.T) {
		dir := t.TempDir()
		staged, err := Stage(dir, smokeScript(`echo "lightcode v9.9.9 (linux/amd64)"; exit 3`))
		if err != nil {
			t.Fatal(err)
		}
		if err := SmokeCheck(staged, "v9.9.9"); err == nil || !strings.Contains(err.Error(), "failed --version") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("smoke fails on hang", func(t *testing.T) {
		dir := t.TempDir()
		staged, err := Stage(dir, smokeScript("sleep 60"))
		if err != nil {
			t.Fatal(err)
		}
		old := SmokeTimeout
		SmokeTimeout = 200 * time.Millisecond
		defer func() { SmokeTimeout = old }()
		if err := SmokeCheck(staged, "v9.9.9"); err == nil || !strings.Contains(err.Error(), "did not exit") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("smoke rejects foreign-owned staged file", func(t *testing.T) {
		dir := t.TempDir()
		staged, err := Stage(dir, smokeScript("echo v9.9.9"))
		if err != nil {
			t.Fatal(err)
		}
		old := Geteuid
		Geteuid = func() int { return os.Geteuid() + 1 }
		defer func() { Geteuid = old }()
		if err := SmokeCheck(staged, "v9.9.9"); err == nil || !strings.Contains(err.Error(), "not owned") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("smoke strips LIGHTCODE_ env from the child", func(t *testing.T) {
		t.Setenv("LIGHTCODE_SMOKE_CANARY", "leaked")
		dir := t.TempDir()
		// The script fails the tag check when the canary leaks through.
		staged, err := Stage(dir, smokeScript(`if [ -n "$LIGHTCODE_SMOKE_CANARY" ]; then echo leaked; else echo lightcode v9.9.9; fi`))
		if err != nil {
			t.Fatal(err)
		}
		if err := SmokeCheck(staged, "v9.9.9"); err != nil {
			t.Fatalf("LIGHTCODE_ env leaked into the smoke child: %v", err)
		}
	})
}

func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	if !DirWritable(dir) {
		t.Fatal("temp dir must be writable")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 && DirWritable(locked) {
		t.Fatal("0500 dir must not be writable")
	}
}

func TestCheckStatusPrecedence(t *testing.T) {
	cases := []struct {
		name            string
		current         string
		latest          string
		wantStatus      string
		wantUpdate      bool
		wantInstallable bool
	}{
		{"update available", "v0.0.3", "v0.0.4", StatusUpdateAvailable, true, true},
		{"up to date", "v0.0.4", "v0.0.4", StatusUpToDate, false, true},
		{"current newer than latest", "v0.0.5", "v0.0.4", StatusUpToDate, false, true},
		{"dev build", "dev", "v0.0.4", StatusDevBuild, false, true},
		{"below floor beats comparison", "v0.0.3", "v0.0.2", StatusBelowFloor, false, false},
		{"below floor beats dev", "dev", "v0.0.2", StatusBelowFloor, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(tc.current, tc.current, tc.latest)
			if got.Status != tc.wantStatus || got.UpdateAvailable != tc.wantUpdate || got.Installable != tc.wantInstallable {
				t.Fatalf("Check = %+v, want status=%s update=%v installable=%v", got, tc.wantStatus, tc.wantUpdate, tc.wantInstallable)
			}
		})
	}
}

func TestMinInstallableShape(t *testing.T) {
	if !version.IsRelease(MinInstallable) {
		t.Fatalf("MinInstallable %q is not an exact vMAJOR.MINOR.PATCH tag - a placeholder cannot ship", MinInstallable)
	}
}

// TestFloorWithinReleaseTag is the release-workflow gate: it runs against
// the tag being released and fails when the floor exceeds it.
func TestFloorWithinReleaseTag(t *testing.T) {
	tag := os.Getenv("LIGHTCODE_RELEASE_TAG")
	if tag == "" {
		t.Skip("LIGHTCODE_RELEASE_TAG not set")
	}
	if !version.IsRelease(MinInstallable) {
		t.Fatalf("floor %q is not an exact vMAJOR.MINOR.PATCH tag", MinInstallable)
	}
	if !version.IsRelease(tag) {
		t.Fatalf("release tag %q is not an exact vMAJOR.MINOR.PATCH tag", tag)
	}
	if version.Compare(MinInstallable, tag) > 0 {
		t.Fatalf("floor %s exceeds the tag being released (%s)", MinInstallable, tag)
	}
}

func TestEnvWithoutLightcode(t *testing.T) {
	t.Setenv("LIGHTCODE_TEST_STRIP", "1")
	t.Setenv("KEEP_THIS_VAR", "1")
	env := envWithoutLightcode()
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "LIGHTCODE_") {
		t.Fatalf("LIGHTCODE_ vars not stripped: %s", joined)
	}
	if !strings.Contains(joined, "KEEP_THIS_VAR=1") {
		t.Fatal("non-LIGHTCODE vars must be kept")
	}
}

func TestIgnoreReleaseBaseAsRoot(t *testing.T) {
	old := Geteuid
	defer func() { Geteuid = old }()
	Geteuid = func() int { return 0 }
	if !IgnoreReleaseBaseAsRoot() {
		t.Fatal("euid 0 must ignore the base override")
	}
	Geteuid = func() int { return 1000 }
	if IgnoreReleaseBaseAsRoot() {
		t.Fatal("non-root must keep the base override")
	}
}

// Sanity: Download surfaces non-200 and short reads as errors.
func TestDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, "payload")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	data, err := Download(srv.URL + "/ok")
	if err != nil || string(data) != "payload" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if _, err := Download(srv.URL + "/missing"); err == nil || !strings.Contains(err.Error(), "unexpected status") {
		t.Fatalf("err = %v", err)
	}
}
