// Package selfupdate implements the mechanics of replacing the installed
// binary from the GitHub release channel: latest-tag resolution, in-memory
// download with SHA-256 verification, tar extraction, staging in the target
// directory, a smoke check of the staged inode, and the atomic rename swap.
// The command layer owns the flow and every user-facing message.
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/version"
)

const (
	repoURL = "https://github.com/MMinasyan/lightcode"

	// AssetName is the single published binary asset per release tag.
	AssetName = "lightcode-linux-amd64.tar.gz"

	// MinInstallable is the oldest release tag upgrade will install: the
	// first tag whose binary answers --version, which the smoke check and
	// the strict dispatcher require. Older releases fall through unknown
	// arguments into a GUI launch; the installer's LIGHTCODE_VERSION pin
	// is the path to them.
	MinInstallable = "v0.0.3"
)

// LatestEndpoint answers with a redirect whose Location names the latest
// tag. Package-level so tests can point it at a fixture server.
var LatestEndpoint = repoURL + "/releases/latest"

// SmokeTimeout bounds the staged-binary check; non-termination is failure.
// Package-level so tests do not wait the full window on a hung fixture.
var SmokeTimeout = 5 * time.Second

// Geteuid is injectable for root-behavior tests; CI never runs as root.
var Geteuid = os.Geteuid

// downloadClient covers the 33 MB asset on slow links.
var downloadClient = &http.Client{Timeout: 5 * time.Minute}

// ResolveExecutable returns the running binary's resolved path and its
// directory. A package variable so tests can point the install flow at a
// fixture directory instead of the test binary.
var ResolveExecutable = resolveExecutable

// resolveExecutable checks each resolution step's own error: swallowing the
// EvalSymlinks error would let filepath.Dir("") silently point the
// writability check at the current directory.
func resolveExecutable() (path, dir string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("cannot resolve executable path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", "", fmt.Errorf("cannot resolve executable path: %w", err)
	}
	return resolved, filepath.Dir(resolved), nil
}

// DirWritable reports whether the current user can write dir.
func DirWritable(dir string) bool {
	return syscall.Access(dir, 0x2) == nil // W_OK
}

// LatestTag resolves the channel's latest release tag from the
// releases/latest redirect without following it. The Location shape is
// parsed strictly; any surprise is an error, never a guess.
func LatestTag() (string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(LatestEndpoint)
	if err != nil {
		return "", fmt.Errorf("resolve latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("resolve latest release: unexpected status %s", resp.Status)
	}
	loc := resp.Header.Get("Location")
	const marker = "/releases/tag/"
	idx := strings.LastIndex(loc, marker)
	if idx < 0 {
		return "", fmt.Errorf("resolve latest release: unexpected redirect %q", loc)
	}
	tag := loc[idx+len(marker):]
	if !version.IsRelease(tag) {
		return "", fmt.Errorf("resolve latest release: unexpected tag %q in redirect", tag)
	}
	return tag, nil
}

// AssetURLs composes the download URLs for a target tag. A non-empty base
// overrides the channel with <base>/<asset> and <base>/SHA256SUMS — no tag
// in the path, exactly the installer's LIGHTCODE_RELEASE_BASE semantics.
func AssetURLs(base, tag string) (assetURL, sumsURL string) {
	if base != "" {
		base = strings.TrimRight(base, "/")
		return base + "/" + AssetName, base + "/SHA256SUMS"
	}
	dir := repoURL + "/releases/download/" + tag
	return dir + "/" + AssetName, dir + "/SHA256SUMS"
}

// Download fetches url fully into memory, so hashing and extraction operate
// on the same bytes.
func Download(url string) ([]byte, error) {
	resp, err := downloadClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: unexpected status %s", url, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	return data, nil
}

// VerifySHA256 checks data against the SHA256SUMS manifest line whose
// filename field is exactly assetName (the manifest also lists install.sh).
func VerifySHA256(data, sums []byte, assetName string) error {
	var expected string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == assetName {
			expected = fields[0]
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("SHA256SUMS has no entry for %s", assetName)
	}
	actual := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), expected) {
		return fmt.Errorf("checksum mismatch for %s", assetName)
	}
	return nil
}

// ExtractBinary returns the bytes of the archive member named exactly
// "lightcode". Only a regular-file member is accepted; symlinks,
// directories, and anything else are rejected.
func ExtractBinary(tarball []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, fmt.Errorf("read release archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read release archive: %w", err)
		}
		if hdr.Name != "lightcode" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("release archive member lightcode is not a regular file")
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read release archive: %w", err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("release archive has no lightcode member")
}

// Stage writes the verified binary into the target directory under a random
// .lightcode.tmp.* name, mode 0755. Staging next to the target gives the
// same filesystem for the atomic rename, an exec-capable location, and the
// guarantee that the smoke check executes the same inode the rename
// promotes. A crash strands one dotfile; uninstall sweeps them.
func Stage(dir string, binary []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".lightcode.tmp.*")
	if err != nil {
		return "", fmt.Errorf("stage new binary: %w", err)
	}
	name := f.Name()
	if _, err := f.Write(binary); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("stage new binary: %w", err)
	}
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("stage new binary: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("stage new binary: %w", err)
	}
	return name, nil
}

// SmokeCheck executes the staged binary with --version before the swap,
// output captured and never forwarded, LIGHTCODE_* stripped from the child
// env. Pass requires both an exit-0 child and the target tag in the
// captured output; non-termination within the timeout is failure.
func SmokeCheck(staged, tag string) error {
	info, err := os.Lstat(staged)
	if err != nil {
		return fmt.Errorf("staged binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("staged binary is not a regular file")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != Geteuid() {
		return fmt.Errorf("staged binary is not owned by the current user")
	}

	ctx, cancel := context.WithTimeout(context.Background(), SmokeTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, staged, "--version")
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Env = envWithoutLightcode()
	// Without WaitDelay, a grandchild inheriting the output pipe keeps
	// Wait blocked long after the timeout killed the staged binary.
	cmd.WaitDelay = 100 * time.Millisecond
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("staged binary did not exit within %s", SmokeTimeout)
	}
	if err != nil {
		return fmt.Errorf("staged binary failed --version: %v", err)
	}
	if !strings.Contains(out.String(), tag) {
		return fmt.Errorf("staged binary did not report %s", tag)
	}
	return nil
}

func envWithoutLightcode() []string {
	env := os.Environ()
	kept := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "LIGHTCODE_") {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

// Replace promotes the staged file over the target via rename(2) — the
// installer's non-destructive swap. The old inode stays alive for running
// processes.
func Replace(staged, target string) error {
	if err := os.Rename(staged, target); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}

// Check statuses, in precedence order: a below-floor latest wins regardless
// of the current build (deterministic on dev builds and today's real
// channel), then a dev current, then the tag comparison.
const (
	StatusBelowFloor      = "below-floor"
	StatusDevBuild        = "dev-build"
	StatusUpdateAvailable = "update-available"
	StatusUpToDate        = "up-to-date"
)

// CheckResult is the upgrade --check --json shape.
type CheckResult struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	Status          string `json:"status"`
	UpdateAvailable bool   `json:"updateAvailable"`
	Installable     bool   `json:"installable"`
}

// Check classifies latest against the current build identity.
// currentVersion is the raw stamped version; currentDisplay is its human
// rendering (the tag, or "dev (abc1234)").
func Check(currentVersion, currentDisplay, latest string) CheckResult {
	result := CheckResult{Current: currentDisplay, Latest: latest}
	result.Installable = version.IsRelease(latest) && version.Compare(latest, MinInstallable) >= 0
	switch {
	case !result.Installable:
		result.Status = StatusBelowFloor
	case !version.IsRelease(currentVersion):
		result.Status = StatusDevBuild
	case version.Compare(latest, currentVersion) > 0:
		result.Status = StatusUpdateAvailable
		result.UpdateAvailable = true
	default:
		result.Status = StatusUpToDate
	}
	return result
}

// IgnoreReleaseBaseAsRoot reports whether a set LIGHTCODE_RELEASE_BASE must
// be dropped: on the install path under euid 0 the override is ignored —
// custom-base into a root-owned directory is unsupported; use the
// installer. The --check refusal applies regardless of euid and is the
// caller's gate.
func IgnoreReleaseBaseAsRoot() bool {
	return Geteuid() == 0
}
