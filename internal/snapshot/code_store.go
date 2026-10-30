package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/safefs"
	"golang.org/x/sys/unix"
)

// CodeStore owns the code-only snapshot implementation: the snapshot-root
// state, capture, last-write identity replacement and recording, listing and
// restore. One group holds one fixed entry directory, snapshots/1. It knows
// nothing about conversation, claims, session metadata, transcript, tokens or
// compaction files — an isolated target code group is addressed through
// OpenCodeStore with no conversation state at all. One production call owns
// one handle; its sub-operations run sequentially, so the store keeps no
// cross-call claim state.
type CodeStore struct {
	snapshotsDir string
}

// OpenCodeStore returns a code store rooted at directory/snapshots. It
// creates no directories or conversation state; snapshot writes begin only
// in the allowed effect.
func OpenCodeStore(directory string) (*CodeStore, error) {
	if directory == "" {
		return nil, errors.New("snapshot: code store directory is empty")
	}
	return &CodeStore{snapshotsDir: filepath.Join(directory, "snapshots")}, nil
}

// SnapshotResolvedEntry captures the first preimage of one canonical target
// and reports the concrete entry id plus whether this call created the entry.
// A caller discards only an entry its own created flag says it created, and
// only when no mutation started; an existing first preimage survives later
// failures.
func (c *CodeStore) SnapshotResolvedEntry(originalPath, canonicalPath string) (string, bool, error) {
	snapshotsDir := c.snapshotsDir
	realPath, err := filepath.EvalSymlinks(canonicalPath)
	if err != nil {
		realPath = canonicalPath
	}
	if info, err := os.Lstat(canonicalPath); err == nil {
		if !info.Mode().IsRegular() {
			return "", false, fmt.Errorf("snapshot: non-regular file target: %s (mode %s)", canonicalPath, info.Mode())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("snapshot: stat %s: %w", canonicalPath, err)
	}
	pathHash := hashString(realPath)
	entryDir := filepath.Join(snapshotsDir, groupEntriesDir, pathHash)
	metaPath := filepath.Join(entryDir, "meta.json")
	if _, err := os.Stat(metaPath); err == nil {
		return pathHash, false, nil
	}
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		return pathHash, false, fmt.Errorf("snapshot: mkdir %s: %w", entryDir, err)
	}
	created := false
	defer func() {
		if !created {
			_ = os.RemoveAll(entryDir)
		}
	}()
	existed := true
	file, openErr := safefs.OpenExisting(canonicalPath, os.O_RDONLY|unix.O_NONBLOCK)
	if openErr != nil {
		if !errors.Is(openErr, os.ErrNotExist) {
			return pathHash, false, fmt.Errorf("snapshot: open %s: %w", canonicalPath, openErr)
		}
		existed = false
	}
	if existed {
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return pathHash, false, fmt.Errorf("snapshot: stat %s: %w", canonicalPath, err)
		}
		if !info.Mode().IsRegular() {
			return pathHash, false, fmt.Errorf("snapshot: non-regular file target: %s (mode %s)", canonicalPath, info.Mode())
		}
	}
	if existed {
		if err := copyFromFile(file, filepath.Join(entryDir, "original")); err != nil {
			return pathHash, false, fmt.Errorf("snapshot: copy %s: %w", canonicalPath, err)
		}
	}
	meta := SnapshotMeta{OriginalPath: originalPath, CanonicalPath: realPath, Existed: existed}
	if err := writeJSON(metaPath, meta); err != nil {
		return pathHash, false, fmt.Errorf("snapshot: write meta: %w", err)
	}
	created = true
	return pathHash, true, nil
}

// DiscardSnapshotEntry removes a snapshot entry. Callers only discard entries
// their own SnapshotResolvedEntry created, and only while no mutation has
// started; a created flag that is false leaves the entry untouched.
func (c *CodeStore) DiscardSnapshotEntry(entryID string) error {
	if err := validateSnapshotEntryID(entryID); err != nil {
		return err
	}
	entryDir := filepath.Join(c.snapshotsDir, groupEntriesDir, entryID)
	if err := os.RemoveAll(entryDir); err != nil {
		return fmt.Errorf("snapshot: discard %s: %w", entryDir, err)
	}
	return nil
}

// ReplaceSnapshotIdentity records a new LastWrite identity for one entry and
// returns the identity the entry owned before the replacement. ReplaceSnapshotIdentity
// with a nil identity clears the proof before a mutation starts; the returned
// prior proof is only ever written back for a proven non-mutating failure.
func (c *CodeStore) ReplaceSnapshotIdentity(entryID string, identity *SnapshotContentIdentity) (*SnapshotContentIdentity, error) {
	if err := validateSnapshotEntryID(entryID); err != nil {
		return nil, err
	}
	metaPath := filepath.Join(c.snapshotsDir, groupEntriesDir, entryID, "meta.json")
	var meta SnapshotMeta
	if err := readJSON(metaPath, &meta); err != nil {
		return nil, err
	}
	prior := meta.LastWrite
	meta.LastWrite = identity
	if err := writeJSON(metaPath, meta); err != nil {
		return prior, err
	}
	return prior, nil
}

// RecordSnapshotContent records the content produced by a completed mutation.
func (c *CodeStore) RecordSnapshotContent(entryID string, content []byte) error {
	_, err := c.ReplaceSnapshotIdentity(entryID, &SnapshotContentIdentity{Hash: hashBytes(content)})
	return err
}

// RecordSnapshotAbsence records that a completed mutation removed the file.
func (c *CodeStore) RecordSnapshotAbsence(entryID string) error {
	_, err := c.ReplaceSnapshotIdentity(entryID, &SnapshotContentIdentity{Absent: true})
	return err
}

// validateSnapshotEntryID is the shared argument check for the entry and
// identity methods.
func validateSnapshotEntryID(entryID string) error {
	if !isEntryID(entryID) {
		return fmt.Errorf("snapshot: invalid entry id %q", entryID)
	}
	return nil
}

// groupEntriesDir is the one fixed entry directory of a group's snapshots
// root: every capture of one code group lives under snapshots/1.
const groupEntriesDir = "1"

// ListEntries returns every recorded snapshot entry of one group in the
// fixed entry directory, in the directory's own lexical entry order.
func (c *CodeStore) ListEntries() ([]SnapshotMeta, error) {
	entriesDir := filepath.Join(c.snapshotsDir, groupEntriesDir)
	dirEntries, err := os.ReadDir(entriesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapshot: read entries dir %s: %w", entriesDir, err)
	}
	var metas []SnapshotMeta
	for _, de := range dirEntries {
		if !de.IsDir() {
			continue
		}
		var meta SnapshotMeta
		if err := readJSON(filepath.Join(entriesDir, de.Name(), "meta.json"), &meta); err != nil {
			continue
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// RevertCodeGroups restores every snapshotted file across the supplied
// directories under ONE traversal ownership: the skip and reported-skip
// ledgers span every group, so an identity skipped in a newer group is never
// restored by an older one. Each group's fixed entries directory is visited
// exactly once in the caller's order. A failure returns the result
// accumulated so far with the error; the restore work that already succeeded
// stays honest.
func RevertCodeGroups(directories []string) (RevertResult, error) {
	var result RevertResult
	skippedEntries := make(map[string]struct{})
	reportedSkippedEntries := make(map[string]struct{})
	for _, directory := range directories {
		entriesDir := filepath.Join(directory, "snapshots", groupEntriesDir)
		groupResult, err := revertEntriesDir(entriesDir, skippedEntries, reportedSkippedEntries)
		result.Restored = append(result.Restored, groupResult.Restored...)
		result.Skipped = append(result.Skipped, groupResult.Skipped...)
		if err != nil {
			return result, fmt.Errorf("snapshot: revert %s: %w", directory, err)
		}
		if err := os.Remove(entriesDir); err != nil && !errors.Is(err, os.ErrNotExist) && !isDirNotEmpty(err) {
			return result, fmt.Errorf("snapshot: remove %s: %w", entriesDir, err)
		}
	}
	return result, nil
}

// --- shared restore/identity/path/file-copy helpers ---

func revertEntriesDir(entriesDir string, skippedEntries, reportedSkippedEntries map[string]struct{}) (RevertResult, error) {
	entries, err := os.ReadDir(entriesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RevertResult{}, nil
		}
		return RevertResult{}, err
	}
	var result RevertResult
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		entryDir := filepath.Join(entriesDir, e.Name())
		var meta SnapshotMeta
		if err := readJSON(filepath.Join(entryDir, "meta.json"), &meta); err != nil {
			return result, fmt.Errorf("read snapshot meta in %s: %w", entryDir, err)
		}
		entryID := filepath.Base(entryDir)
		if _, blocked := skippedEntries[entryID]; blocked {
			continue
		}
		restored, skipped, err := restoreOne(entryDir, meta)
		if err != nil {
			return result, fmt.Errorf("restore %s: %w", meta.OriginalPath, err)
		}
		if skipped != nil {
			skippedEntries[entryID] = struct{}{}
			if _, reported := reportedSkippedEntries[entryID]; !reported {
				result.Skipped = append(result.Skipped, *skipped)
				reportedSkippedEntries[entryID] = struct{}{}
			}
			continue
		}
		if restored {
			result.Restored = append(result.Restored, meta.OriginalPath)
			if err := os.RemoveAll(entryDir); err != nil {
				return result, fmt.Errorf("remove restored snapshot entry %s: %w", entryDir, err)
			}
		}
	}
	return result, nil
}

func restoreOne(entryDir string, meta SnapshotMeta) (bool, *SkippedRevert, error) {
	entryID := filepath.Base(entryDir)
	restorePath, skipped, err := validateRestorePath(entryID, meta)
	if err != nil {
		return false, nil, err
	}
	if skipped != nil {
		return false, skipped, nil
	}
	if ok, reason, err := currentMatchesLastWrite(restorePath, meta.LastWrite); err != nil {
		return false, nil, err
	} else if !ok {
		return false, &SkippedRevert{Path: meta.OriginalPath, Reason: reason}, nil
	}
	if !meta.Existed {
		if err := safefs.RemoveLeaf(restorePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, nil, err
		}
		return true, nil, nil
	}
	src := filepath.Join(entryDir, "original")
	return true, nil, restoreFile(src, restorePath)
}

func currentMatchesLastWrite(path string, last *SnapshotContentIdentity) (bool, string, error) {
	if last == nil {
		return false, "snapshot entry has no recorded post-write identity", nil
	}
	if last.Absent {
		if _, err := os.Lstat(path); err == nil {
			return false, "file exists but this session last left it absent", nil
		} else if errors.Is(err, os.ErrNotExist) {
			return true, "", nil
		} else {
			return false, "", fmt.Errorf("stat current target %s: %w", path, err)
		}
	}
	got, exists, reason, err := hashCurrentFile(path)
	if err != nil {
		return false, "", err
	}
	if reason != "" {
		return false, reason, nil
	}
	if !exists {
		return false, "file is missing but this session last wrote content", nil
	}
	if got != last.Hash {
		return false, "file content changed since this session last wrote it", nil
	}
	return true, "", nil
}

func hashCurrentFile(path string) (string, bool, string, error) {
	f, err := safefs.OpenExisting(path, os.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, "", nil
		}
		if errors.Is(err, safefs.ErrNonRegular) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENOTDIR) {
			hardlinked, statErr := hardlinkedRegularOrSymlinkLeaf(path)
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return "", false, "", statErr
			}
			if hardlinked {
				return "", false, "", fmt.Errorf("%w (link count > 1): %s", safefs.ErrHardlink, path)
			}
			return "", true, "file is no longer a regular file", nil
		}
		return "", false, "", fmt.Errorf("open current target %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", false, "", fmt.Errorf("read current target %s: %w", path, err)
	}
	return hashBytes(data), true, "", nil
}

func hardlinkedRegularOrSymlinkLeaf(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	mode := info.Mode()
	if !mode.IsRegular() && mode&os.ModeSymlink == 0 {
		return false, nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return st.Nlink > 1, nil
}

func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, unix.ENOTEMPTY)
}

// validateRestorePath proves one restore target through the entry's
// capture-time canonical witness. An entry without that witness is refused:
// the restore never reconstructs a target from a path guess.
func validateRestorePath(entryID string, meta SnapshotMeta) (string, *SkippedRevert, error) {
	if meta.CanonicalPath == "" {
		return "", nil, fmt.Errorf("canonical proof refused: snapshot entry for %s carries no capture-time canonical path", meta.OriginalPath)
	}
	if hashString(meta.CanonicalPath) != entryID {
		return "", nil, fmt.Errorf("canonical proof refused: meta canonical path %q does not match entry id %s", meta.CanonicalPath, entryID)
	}
	if !meta.Existed {
		restorePath, err := validateCanonicalDeletePath(meta)
		return restorePath, nil, err
	}
	return validateCanonicalRestorePath(meta)
}

func validateCanonicalRestorePath(meta SnapshotMeta) (string, *SkippedRevert, error) {
	resolved, err := pathutil.ResolveFilePath(meta.OriginalPath)
	if err != nil {
		return "", nil, fmt.Errorf("resolve restore path %s: %w", meta.OriginalPath, err)
	}
	if resolved.CanonicalPath != meta.CanonicalPath {
		return "", &SkippedRevert{
			Path:   meta.OriginalPath,
			Reason: fmt.Sprintf("canonical path changed from %s to %s", meta.CanonicalPath, resolved.CanonicalPath),
		}, nil
	}
	return meta.CanonicalPath, nil, nil
}

func validateCanonicalDeletePath(meta SnapshotMeta) (string, error) {
	absOriginal, err := filepath.Abs(meta.OriginalPath)
	if err != nil {
		return "", fmt.Errorf("resolve delete path %s: %w", meta.OriginalPath, err)
	}
	resolvedParent, _, err := pathutil.ResolveAbsPath(filepath.Dir(absOriginal))
	if err != nil {
		return "", fmt.Errorf("resolve delete parent %s: %w", filepath.Dir(absOriginal), err)
	}
	resolvedPath := filepath.Join(resolvedParent, filepath.Base(absOriginal))
	if resolvedPath != meta.CanonicalPath {
		return "", fmt.Errorf("canonical path changed from %s to %s", meta.CanonicalPath, resolvedPath)
	}
	return meta.CanonicalPath, nil
}

func isEntryID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func copyFromFile(in *os.File, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func restoreFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if info, err := os.Lstat(dst); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("restore target is non-regular: %s (mode %s)", dst, info.Mode())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat restore target %s: %w", dst, err)
	}

	out, _, err := safefs.OpenForWrite(dst, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Truncate(0); err != nil {
		return err
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
