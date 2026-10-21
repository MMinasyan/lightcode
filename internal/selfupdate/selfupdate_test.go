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

	"github.com/MMinasyan/lightcode/internal/testassert"
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
				testassert.Require(t, err != nil && strings.Contains(err.Error(), tc.wantErr), "err = %v, want substring %q", err, tc.wantErr)
				return
			}
			testassert.NoError(t, err)
			testassert.Equal(t, tag, tc.wantTag)
		})
	}
}

func TestAssetURLs(t *testing.T) {
	asset, sums := AssetURLs("", "v0.0.4")
	testassert.Equal(t, asset, "https://github.com/MMinasyan/lightcode/releases/download/v0.0.4/"+AssetName)
	testassert.Equal(t, sums, "https://github.com/MMinasyan/lightcode/releases/download/v0.0.4/SHA256SUMS")

	// A base override composes <base>/<asset> with no tag in the path,
	// matching the installer's semantics.
	asset, sums = AssetURLs("http://example.test/dir/", "v0.0.4")
	testassert.Equal(t, asset, "http://example.test/dir/"+AssetName)
	testassert.Equal(t, sums, "http://example.test/dir/SHA256SUMS")
	testassert.Require(t, !strings.Contains(asset, "v0.0.4"), "base URL must not contain the tag")
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

	testassert.NoError(t, VerifySHA256(data, manifest(good), AssetName))
	{
		err := VerifySHA256(data, manifest(strings.Repeat("0", 64)), AssetName)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "checksum mismatch"), "mismatch err = %v", err)
	}
	{
		err := VerifySHA256(data, []byte("deadbeef  other-file\n"), AssetName)
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "no entry"), "missing entry err = %v", err)
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
		testassert.NoError(t, tw.WriteHeader(hdr))
		if m.typeflag == tar.TypeReg {
			{
				_, err := tw.Write([]byte(m.body))
				testassert.NoError(t, err)
			}
		}
	}
	testassert.NoError(t, tw.Close())
	testassert.NoError(t, gz.Close())
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
		testassert.NoError(t, err)
		testassert.Require(t, string(data) == "BINARY", "extracted %q", data)
	})

	t.Run("missing member", func(t *testing.T) {
		tb := buildTarball(t, map[string]member{"otherfile": {body: "X", typeflag: tar.TypeReg}})
		{
			_, err := ExtractBinary(tb)
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "no lightcode member"), "err = %v", err)
		}
	})

	t.Run("symlink member rejected", func(t *testing.T) {
		tb := buildTarball(t, map[string]member{"lightcode": {typeflag: tar.TypeSymlink, linkname: "/bin/sh"}})
		{
			_, err := ExtractBinary(tb)
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "not a regular file"), "err = %v", err)
		}
	})

	t.Run("dir member rejected", func(t *testing.T) {
		tb := buildTarball(t, map[string]member{"lightcode": {typeflag: tar.TypeDir}})
		{
			_, err := ExtractBinary(tb)
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "not a regular file"), "err = %v", err)
		}
	})

	t.Run("short body", func(t *testing.T) {
		tb := buildTarball(t, map[string]member{"lightcode": {body: "BINARY", typeflag: tar.TypeReg}})
		{
			_, err := ExtractBinary(tb[:len(tb)/2])
			testassert.Require(t, err != nil, "truncated archive must error")
		}
	})

	t.Run("not gzip", func(t *testing.T) {
		{
			_, err := ExtractBinary([]byte("plain bytes"))
			testassert.Require(t, err != nil, "non-gzip input must error")
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
		testassert.NoError(t, os.WriteFile(target, []byte("old"), 0o755))
		staged, err := Stage(dir, smokeScript(`echo "lightcode v9.9.9 (linux/amd64)"`))
		testassert.NoError(t, err)
		testassert.Require(t, strings.HasPrefix(filepath.Base(staged), ".lightcode.tmp."), "staged name = %q", staged)
		info, err := os.Stat(staged)
		testassert.NoError(t, err)
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("staged mode = %v", info.Mode())
		}
		stagedIno := info.Sys().(*syscall.Stat_t).Ino

		testassert.NoError(t, SmokeCheck(staged, "v9.9.9"))
		testassert.NoError(t, Replace(staged, target))
		finalInfo, err := os.Stat(target)
		testassert.NoError(t, err)
		testassert.Require(t, finalInfo.Sys().(*syscall.Stat_t).Ino == stagedIno, "the promoted inode is not the smoke-checked inode")
	})

	t.Run("stale staged files do not block", func(t *testing.T) {
		dir := t.TempDir()
		testassert.NoError(t, os.WriteFile(filepath.Join(dir, ".lightcode.tmp.stale"), []byte("x"), 0o755))
		{
			_, err := Stage(dir, []byte("new"))
			testassert.NoError(t, err)
		}
	})

	t.Run("smoke fails on wrong tag", func(t *testing.T) {
		dir := t.TempDir()
		staged, err := Stage(dir, smokeScript(`echo "lightcode v0.0.1 (linux/amd64)"`))
		testassert.NoError(t, err)
		{
			err := SmokeCheck(staged, "v9.9.9")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "did not report"), "err = %v", err)
		}
	})

	t.Run("smoke fails on nonzero exit even with matching tag", func(t *testing.T) {
		dir := t.TempDir()
		staged, err := Stage(dir, smokeScript(`echo "lightcode v9.9.9 (linux/amd64)"; exit 3`))
		testassert.NoError(t, err)
		{
			err := SmokeCheck(staged, "v9.9.9")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "failed --version"), "err = %v", err)
		}
	})

	t.Run("smoke fails on hang", func(t *testing.T) {
		dir := t.TempDir()
		staged, err := Stage(dir, smokeScript("sleep 60"))
		testassert.NoError(t, err)
		old := SmokeTimeout
		SmokeTimeout = 200 * time.Millisecond
		defer func() { SmokeTimeout = old }()
		{
			err := SmokeCheck(staged, "v9.9.9")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "did not exit"), "err = %v", err)
		}
	})

	t.Run("smoke rejects foreign-owned staged file", func(t *testing.T) {
		dir := t.TempDir()
		staged, err := Stage(dir, smokeScript("echo v9.9.9"))
		testassert.NoError(t, err)
		old := Geteuid
		Geteuid = func() int { return os.Geteuid() + 1 }
		defer func() { Geteuid = old }()
		{
			err := SmokeCheck(staged, "v9.9.9")
			testassert.Require(t, err != nil && strings.Contains(err.Error(), "not owned"), "err = %v", err)
		}
	})

	t.Run("smoke strips LIGHTCODE_ env from the child", func(t *testing.T) {
		t.Setenv("LIGHTCODE_SMOKE_CANARY", "leaked")
		dir := t.TempDir()
		// The script fails the tag check when the canary leaks through.
		staged, err := Stage(dir, smokeScript(`if [ -n "$LIGHTCODE_SMOKE_CANARY" ]; then echo leaked; else echo lightcode v9.9.9; fi`))
		testassert.NoError(t, err)
		testassert.NoError(t, SmokeCheck(staged, "v9.9.9"))
	})
}

func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	testassert.Require(t, DirWritable(dir), "temp dir must be writable")
	locked := filepath.Join(dir, "locked")
	testassert.NoError(t, os.Mkdir(locked, 0o500))
	testassert.Require(t, os.Geteuid() == 0 || !DirWritable(locked), "0500 dir must not be writable")
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
		// Ordering past the machine integer range decides the upgrade status.
		{"update available beyond machine integers", "v1.0.0", "v18446744073709551615.0.0", StatusUpdateAvailable, true, true},
		{"current beyond machine integers newer than latest", "v18446744073709551615.0.0", "v1.0.0", StatusUpToDate, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(tc.current, tc.current, tc.latest)
			testassert.Require(t, got.Status == tc.wantStatus && got.UpdateAvailable == tc.wantUpdate && got.Installable == tc.wantInstallable, "Check = %+v, want status=%s update=%v installable=%v", got, tc.wantStatus, tc.wantUpdate, tc.wantInstallable)
		})
	}
}

func TestMinInstallableShape(t *testing.T) {
	testassert.Require(t, version.IsRelease(MinInstallable), "MinInstallable %q is not an exact vMAJOR.MINOR.PATCH tag - a placeholder cannot ship", MinInstallable)
}

// TestFloorWithinReleaseTag is the release-workflow gate: it runs against
// the tag being released and fails when the floor exceeds it.
func TestFloorWithinReleaseTag(t *testing.T) {
	tag := os.Getenv("LIGHTCODE_RELEASE_TAG")
	if tag == "" {
		t.Skip("LIGHTCODE_RELEASE_TAG not set")
	}
	testassert.Require(t, version.IsRelease(MinInstallable), "floor %q is not an exact vMAJOR.MINOR.PATCH tag", MinInstallable)
	testassert.Require(t, version.IsRelease(tag), "release tag %q is not an exact vMAJOR.MINOR.PATCH tag", tag)
	testassert.Require(t, version.Compare(MinInstallable, tag) <= 0, "floor %s exceeds the tag being released (%s)", MinInstallable, tag)
}

func TestEnvWithoutLightcode(t *testing.T) {
	t.Setenv("LIGHTCODE_TEST_STRIP", "1")
	t.Setenv("KEEP_THIS_VAR", "1")
	env := envWithoutLightcode()
	joined := strings.Join(env, "\n")
	testassert.Require(t, !strings.Contains(joined, "LIGHTCODE_"), "LIGHTCODE_ vars not stripped: %s", joined)
	testassert.Require(t, strings.Contains(joined, "KEEP_THIS_VAR=1"), "non-LIGHTCODE vars must be kept")
}

func TestIgnoreReleaseBaseAsRoot(t *testing.T) {
	old := Geteuid
	defer func() { Geteuid = old }()
	Geteuid = func() int { return 0 }
	testassert.Require(t, IgnoreReleaseBaseAsRoot(), "euid 0 must ignore the base override")
	Geteuid = func() int { return 1000 }
	testassert.Require(t, !IgnoreReleaseBaseAsRoot(), "non-root must keep the base override")
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
	testassert.Require(t, err == nil && string(data) == "payload", "data=%q err=%v", data, err)
	{
		_, err := Download(srv.URL + "/missing")
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "unexpected status"), "err = %v", err)
	}
}
