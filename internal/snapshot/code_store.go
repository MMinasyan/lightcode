package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/MMinasyan/lightcode/internal/safefs"
	"golang.org/x/sys/unix"
)

// CodeStore owns the code-only half of the snapshot implementation: the
// snapshot-root state, capture references, mutation locks, first-write and
// last-write identities, listing and restore. It knows nothing about the
// Project record, claim, Session metadata, transcript, tokens or compaction
// file — an isolated target code group is addressed through OpenCodeStore
// with no conversation state at all. The legacy Store owns session/turn
// allocation (CurrentTurn, high-water) and delegates code operations here;
// lock order is Store then CodeStore, never the reverse.
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

// rebind points the code store's snapshot root at a relocated session
// directory. The legacy Store calls it under its own lock whenever the
// active session's snapshots directory moves; lock order is Store then
// CodeStore, never the reverse.
func (c *CodeStore) rebind(snapshotsDir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshotsDir = snapshotsDir
}

// reset drops all capture references and mutation locks and clears the
// snapshot root, matching the legacy Store's session detach.
func (c *CodeStore) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshotsDir = ""
	c.snapshotTx = nil
	c.mutationLock = nil
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
// checks for the transaction and identity methods. The legacy Store wrappers
// run them before the session check to preserve their error precedence; the
// CodeStore methods run them again for direct target-tool callers.
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

// RevertCode restores every file snapshotted in turns > toTurn to its
// pre-turn state and deletes those snapshot turn dirs. Message history
// and turn dirs are NOT touched — conversation stays intact. Turn-number
// allocation remains the legacy Store's concern: the high-water mark is
// recorded there before this rewind is delegated.
func (c *CodeStore) RevertCode(toTurn int) (RevertResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if toTurn < 0 {
		toTurn = 0
	}
	turns := readIntDirs(c.snapshotsDir)
	var result RevertResult
	skippedEntries := make(map[string]struct{})
	reportedSkippedEntries := make(map[string]struct{})
	for i := len(turns) - 1; i >= 0; i-- {
		turn := turns[i]
		if turn <= toTurn {
			break
		}
		turnDir := filepath.Join(c.snapshotsDir, strconv.Itoa(turn))
		turnResult, err := revertOneTurn(turnDir, skippedEntries, reportedSkippedEntries)
		result.Restored = append(result.Restored, turnResult.Restored...)
		result.Skipped = append(result.Skipped, turnResult.Skipped...)
		if err != nil {
			return result, fmt.Errorf("snapshot: revert turn %d: %w", turn, err)
		}
		if err := os.Remove(turnDir); err != nil && !errors.Is(err, os.ErrNotExist) && !isDirNotEmpty(err) {
			return result, fmt.Errorf("snapshot: remove %s: %w", turnDir, err)
		}
	}
	return result, nil
}
