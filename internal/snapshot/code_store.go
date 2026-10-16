package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"

	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/safefs"
	"golang.org/x/sys/unix"
)

// CodeStore owns the code-only snapshot implementation: the snapshot-root
// state, capture references, mutation locks, first-write and last-write
// identities, listing and restore. It knows nothing about conversation,
// claims, session metadata, transcript, tokens or compaction files — an
// isolated target code group is addressed through OpenCodeStore with no
// conversation state at all.
type CodeStore struct {
	mu sync.Mutex

	snapshotsDir string

	snapshotTx   map[string]*snapshotTxState
	mutationLock map[string]*snapshotMutationLock
}

// OpenCodeStore returns a code store rooted at directory/snapshots. It
// creates no directories, conversation, turn or claim; snapshot writes begin
// only in the allowed effect.
func OpenCodeStore(directory string) (*CodeStore, error) {
	if directory == "" {
		return nil, errors.New("snapshot: code store directory is empty")
	}
	return &CodeStore{snapshotsDir: filepath.Join(directory, "snapshots")}, nil
}

// SnapshotResolvedEntry is SnapshotResolved plus the concrete entry id and a
// created flag. Mutation callers must either retain the entry once mutation
// starts or discard their pre-mutation claim if validation fails first.
func (c *CodeStore) SnapshotResolvedEntry(turn int, originalPath, canonicalPath string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateSnapshotTurn(turn); err != nil {
		return "", false, err
	}
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
	entryDir := filepath.Join(snapshotsDir, strconv.Itoa(turn), pathHash)
	metaPath := filepath.Join(entryDir, "meta.json")
	if _, err := os.Stat(metaPath); err == nil {
		if state := c.snapshotTxStateLocked(turn, pathHash, false); state != nil {
			state.refs++
		}
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
	state := c.snapshotTxStateLocked(turn, pathHash, true)
	state.refs++
	return pathHash, true, nil
}

// DiscardSnapshotEntry releases a pre-mutation snapshot user. The entry is
// removed only if no concurrent user retained it or still depends on it.
func (c *CodeStore) DiscardSnapshotEntry(turn int, entryID string) error {
	if err := validateSnapshotTurn(turn); err != nil {
		return err
	}
	if err := validateSnapshotEntryID(entryID); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := snapshotTxKey(turn, entryID)
	state := c.snapshotTx[key]
	if state == nil {
		return nil
	}
	if state.refs > 0 {
		state.refs--
	}
	if state.refs > 0 {
		return nil
	}
	delete(c.snapshotTx, key)
	entryDir := filepath.Join(c.snapshotsDir, strconv.Itoa(turn), entryID)
	if err := os.RemoveAll(entryDir); err != nil {
		return fmt.Errorf("snapshot: discard %s: %w", entryDir, err)
	}
	return nil
}

// RetainSnapshotEntry keeps a snapshot entry once any user starts mutating.
func (c *CodeStore) RetainSnapshotEntry(turn int, entryID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := snapshotTxKey(turn, entryID)
	state := c.snapshotTx[key]
	if state == nil {
		return
	}
	delete(c.snapshotTx, key)
}

// LockSnapshotMutation serializes same-session mutations for one snapshot
// entry. Callers hold the returned release function until the disk mutation
// and last-write identity record have both completed.
func (c *CodeStore) LockSnapshotMutation(turn int, entryID string) (func(), error) {
	if err := validateSnapshotTurn(turn); err != nil {
		return nil, err
	}
	if err := validateSnapshotEntryID(entryID); err != nil {
		return nil, err
	}
	c.mu.Lock()
	key := snapshotTxKey(turn, entryID)
	if c.mutationLock == nil {
		c.mutationLock = make(map[string]*snapshotMutationLock)
	}
	lock := c.mutationLock[key]
	if lock == nil {
		lock = &snapshotMutationLock{}
		c.mutationLock[key] = lock
	}
	lock.refs++
	c.mu.Unlock()

	lock.mu.Lock()
	released := false
	return func() {
		if released {
			return
		}
		released = true
		lock.mu.Unlock()

		c.mu.Lock()
		defer c.mu.Unlock()
		lock.refs--
		if lock.refs == 0 && c.mutationLock[key] == lock {
			delete(c.mutationLock, key)
		}
	}, nil
}

// RecordSnapshotContent records the content produced by a successful mutation.
func (c *CodeStore) RecordSnapshotContent(turn int, entryID string, content []byte) error {
	return c.recordSnapshotIdentity(turn, entryID, SnapshotContentIdentity{Hash: hashBytes(content)})
}

// RecordSnapshotAbsence records that a successful mutation removed the file.
func (c *CodeStore) RecordSnapshotAbsence(turn int, entryID string) error {
	return c.recordSnapshotIdentity(turn, entryID, SnapshotContentIdentity{Absent: true})
}

func (c *CodeStore) recordSnapshotIdentity(turn int, entryID string, identity SnapshotContentIdentity) error {
	if err := validateSnapshotTurn(turn); err != nil {
		return err
	}
	if err := validateSnapshotEntryID(entryID); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	metaPath := filepath.Join(c.snapshotsDir, strconv.Itoa(turn), entryID, "meta.json")
	var meta SnapshotMeta
	if err := readJSON(metaPath, &meta); err != nil {
		return err
	}
	meta.LastWrite = &identity
	if err := writeJSON(metaPath, meta); err != nil {
		return err
	}
	return nil
}

func (c *CodeStore) snapshotTxStateLocked(turn int, entryID string, create bool) *snapshotTxState {
	key := snapshotTxKey(turn, entryID)
	if c.snapshotTx == nil {
		if !create {
			return nil
		}
		c.snapshotTx = make(map[string]*snapshotTxState)
	}
	state := c.snapshotTx[key]
	if state == nil && create {
		state = &snapshotTxState{}
		c.snapshotTx[key] = state
	}
	return state
}

type snapshotTxState struct {
	refs int
}

type snapshotMutationLock struct {
	mu   sync.Mutex
	refs int
}

func snapshotTxKey(turn int, entryID string) string {
	return strconv.Itoa(turn) + "/" + entryID
}

// validateSnapshotTurn and validateSnapshotEntryID are the shared argument
// checks for the transaction and identity methods.
func validateSnapshotTurn(turn int) error {
	if turn < 1 {
		return fmt.Errorf("snapshot: turn must be >= 1, got %d", turn)
	}
	return nil
}

func validateSnapshotEntryID(entryID string) error {
	if !isLegacyEntryID(entryID) {
		return fmt.Errorf("snapshot: invalid entry id %q", entryID)
	}
	return nil
}

// TurnEntry describes one snapshot turn for UI display.
type TurnEntry struct {
	Turn  int            `json:"turn"`
	Files []SnapshotMeta `json:"files"`
}

// ListTurns returns all recorded snapshot turns with their file meta.
func (c *CodeStore) ListTurns() ([]TurnEntry, error) {
	c.mu.Lock()
	snapshotsDir := c.snapshotsDir
	c.mu.Unlock()
	turns := readIntDirs(snapshotsDir)
	var entries []TurnEntry
	for _, turn := range turns {
		turnDir := filepath.Join(snapshotsDir, strconv.Itoa(turn))
		dirEntries, err := os.ReadDir(turnDir)
		if err != nil {
			return entries, fmt.Errorf("snapshot: read turn %d: %w", turn, err)
		}
		var files []SnapshotMeta
		for _, de := range dirEntries {
			if !de.IsDir() {
				continue
			}
			var meta SnapshotMeta
			if err := readJSON(filepath.Join(turnDir, de.Name(), "meta.json"), &meta); err != nil {
				continue
			}
			files = append(files, meta)
		}
		entries = append(entries, TurnEntry{Turn: turn, Files: files})
	}
	return entries, nil
}

// RevertCodeGroups restores every file snapshotted in turns > afterTurn
// across the supplied directories, latest-first, under ONE traversal
// ownership: the skip and reported-skip ledgers span every group and every
// descending turn, so an identity skipped in a newer directory is never
// restored by an older one. Turn numbering, the after-turn floor and turn-dir
// removal stay per directory. A failure returns the result accumulated so far
// with the error; the restore work that already succeeded stays honest.
func RevertCodeGroups(directories []string, afterTurn int) (RevertResult, error) {
	if afterTurn < 0 {
		afterTurn = 0
	}
	var result RevertResult
	skippedEntries := make(map[string]struct{})
	reportedSkippedEntries := make(map[string]struct{})
	for _, directory := range directories {
		snapshotsDir := filepath.Join(directory, "snapshots")
		turns := readIntDirs(snapshotsDir)
		for i := len(turns) - 1; i >= 0; i-- {
			turn := turns[i]
			if turn <= afterTurn {
				break
			}
			turnDir := filepath.Join(snapshotsDir, strconv.Itoa(turn))
			turnResult, err := revertOneTurn(turnDir, skippedEntries, reportedSkippedEntries)
			result.Restored = append(result.Restored, turnResult.Restored...)
			result.Skipped = append(result.Skipped, turnResult.Skipped...)
			if err != nil {
				return result, fmt.Errorf("snapshot: revert %s turn %d: %w", directory, turn, err)
			}
			if err := os.Remove(turnDir); err != nil && !errors.Is(err, os.ErrNotExist) && !isDirNotEmpty(err) {
				return result, fmt.Errorf("snapshot: remove %s: %w", turnDir, err)
			}
		}
	}
	return result, nil
}

// RevertCode restores every file snapshotted in turns > toTurn to its
// pre-turn state and deletes those snapshot turn dirs. The traversal is the
// shared whole-invocation one under this store's instance ownership.
func (c *CodeStore) RevertCode(toTurn int) (RevertResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return RevertCodeGroups([]string{filepath.Dir(c.snapshotsDir)}, toTurn)
}

// --- shared restore/identity/path/file-copy helpers ---

func revertOneTurn(turnDir string, skippedEntries, reportedSkippedEntries map[string]struct{}) (RevertResult, error) {
	entries, err := os.ReadDir(turnDir)
	if err != nil {
		return RevertResult{}, err
	}
	var result RevertResult
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		entryDir := filepath.Join(turnDir, e.Name())
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

func validateRestorePath(entryID string, meta SnapshotMeta) (string, *SkippedRevert, error) {
	if !meta.Existed {
		restorePath, err := validateDeletePath(entryID, meta)
		return restorePath, nil, err
	}
	if meta.CanonicalPath == "" {
		restorePath, err := validateLegacyRestorePath(entryID, meta.OriginalPath)
		return restorePath, nil, err
	}
	return validateModernRestorePath(entryID, meta)
}

func validateModernRestorePath(entryID string, meta SnapshotMeta) (string, *SkippedRevert, error) {
	if hashString(meta.CanonicalPath) != entryID {
		return "", nil, fmt.Errorf("modern restore refused: meta canonical path %q does not match entry id %s", meta.CanonicalPath, entryID)
	}
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

func validateDeletePath(entryID string, meta SnapshotMeta) (string, error) {
	if meta.CanonicalPath != "" {
		return validateModernDeletePath(entryID, meta)
	}
	return "", fmt.Errorf("legacy delete refused: %s has no canonical proof", meta.OriginalPath)
}

func validateModernDeletePath(entryID string, meta SnapshotMeta) (string, error) {
	if hashString(meta.CanonicalPath) != entryID {
		return "", fmt.Errorf("modern delete refused: meta canonical path %q does not match entry id %s", meta.CanonicalPath, entryID)
	}
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

func validateLegacyRestorePath(entryID, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("legacy restore path is empty")
	}
	if !isLegacyEntryID(entryID) {
		return "", fmt.Errorf("legacy snapshot entry id %q is invalid", entryID)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve legacy restore path %s: %w", path, err)
	}

	resolved, err := pathutil.ResolveFilePath(absPath)
	if err == nil {
		if got := hashString(resolved.CanonicalPath); got == entryID {
			return resolved.CanonicalPath, nil
		} else if resolved.LeafExists {
			return "", fmt.Errorf("legacy snapshot target hash changed from %s to %s", entryID, got)
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve legacy restore path %s: %w", path, err)
	}

	cleanAbs := filepath.Clean(absPath)
	if got := hashString(cleanAbs); got == entryID {
		return cleanAbs, nil
	}
	return "", fmt.Errorf("legacy snapshot target missing and cannot be proven for %s", path)
}

func isLegacyEntryID(id string) bool {
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

func readIntDirs(dir string) []int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 1 {
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyFromFile(in, dst)
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
