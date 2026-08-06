package snapshot

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/safefs"
	"golang.org/x/sys/unix"
)

// lightcodeVersion is written into each new session's meta.json. Bump
// when the on-disk layout changes in a backward-incompatible way.
const lightcodeVersion = "0.3.0"

// ErrNoSession is returned by any Store method that needs an active
// session when none is open.
var ErrNoSession = errors.New("snapshot: no session open")

// copyDirFunc is the outer-loop directory copy used by ForkInto. Tests
// override it to coordinate timing inside the fork copy loop; production
// always uses the real copyDir.
var copyDirFunc = copyDir

// forkIntoLockReleasedHook fires immediately before ForkInto releases
// s.mu. Production no-op; tests use it to detect early release of the
// lock from the fork copy region.
var forkIntoLockReleasedHook = func() {}

// mintMu serializes the session-id reservation sequence — draw, collision
// scan, claim, directory creation, and the metadata write — across every
// creation path in this process. The directory is the reservation: a second
// mint can only scan after the winning id's directory exists on disk, so it
// can never draw the same id. The hold spans only local filesystem work —
// never a call that takes an owner lock — and is released before any turn
// or loop work.
var mintMu sync.Mutex

// mintSessionIDMaxAttempts bounds how many ids the mint draws before giving
// up on a persistent collision.
const mintSessionIDMaxAttempts = 8

// mintSessionIDFunc supplies the next raw session id for the mint. Production
// draws from the crypto RNG; tests swap it to force collisions
// deterministically. Follows the copyDirFunc precedent.
var mintSessionIDFunc = newSessionID

// Store owns one session directory at a time. Its session may be
// swapped (Close + BeginNewSession / LoadSession) while tool and loop
// references to the Store remain valid — they call methods on the same
// pointer, which now transparently refer to whichever session is active.
//
// A Store with no active session rejects Snapshot / AppendMessage /
// MarkTurnComplete with ErrNoSession. BeginTurn and CurrentTurn return
// 0 in that state. This supports lazy session creation: a fresh launch
// opens a Store, and the first user prompt calls BeginNewSession.
type Store struct {
	mu sync.Mutex

	root string // ~/.lightcode/projects/<pid>/sessions, or ~/.lightcode/sessions for legacy

	// Projects-system bookkeeping. When set, TouchActivity also bumps
	// the owning project's last_activity. Both zero-value in tests that
	// use NewEmpty + BeginNewSession directly.
	projectsRoot string
	projectID    string

	active       bool
	dir          string
	snapshotsDir string
	turnsDir     string
	sessionID    string
	projectRoot  string
	projectHash  string
	currentTurn  int
	// highWaterTurn is the highest turn number this Store has issued in the
	// live session. RevertHistory and RevertCode remove turn directories, so
	// allocation from disk alone would reissue a number the session already
	// used; nextTurnLocked never allocates at or below this mark. It is
	// per-session in-memory state: cleared with the rest of the session in
	// clearLocked, so a Store that moves to another session restarts from disk.
	highWaterTurn int
	snapshotTx    map[string]*snapshotTxState
	mutationLock  map[string]*snapshotMutationLock

	// claim is the cross-process exclusion held while this process drives the
	// session. It is acquired before a mutating load or new-session
	// publication and released on Close. Nil for stores with no project
	// context (legacy/test stores that never drive across processes).
	claim *atomicfs.Lock
}

type snapshotTxState struct {
	refs int
}

type snapshotMutationLock struct {
	mu   sync.Mutex
	refs int
}

// NewForSessionsRoot returns a Store rooted at an explicit sessions
// directory (typically ~/.lightcode/projects/<pid>/sessions). The
// projectsRoot + projectID are recorded so TouchActivity can bump the
// project's last_activity.
func NewForSessionsRoot(sessionsRoot, projectsRoot, projectID string) (*Store, error) {
	if sessionsRoot != "" {
		if err := os.MkdirAll(sessionsRoot, 0o700); err != nil {
			return nil, fmt.Errorf("snapshot: create %s: %w", sessionsRoot, err)
		}
	}
	return &Store{
		root:         sessionsRoot,
		projectsRoot: projectsRoot,
		projectID:    projectID,
	}, nil
}

// NewStagingStore returns a Store rooted at stagingRoot but carrying this
// store's project context, so a candidate prepared under staging claims the
// same per-session lock namespace as this store and, once its directory is
// renamed into the real sessions root, needs no reclaim.
func (s *Store) NewStagingStore(stagingRoot string) (*Store, error) {
	s.mu.Lock()
	projectsRoot, projectID := s.projectsRoot, s.projectID
	s.mu.Unlock()
	return NewForSessionsRoot(stagingRoot, projectsRoot, projectID)
}

// AttachSessionsRoot swaps the store's sessions root and project
// bookkeeping. Used for the lazy project-creation path: a Store built
// with NewEmpty gets its real root once the first session is created.
// Must not be called while a session is active.
func (s *Store) AttachSessionsRoot(sessionsRoot, projectsRoot, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return fmt.Errorf("snapshot: cannot reroot while session %q is open", s.sessionID)
	}
	if err := os.MkdirAll(sessionsRoot, 0o700); err != nil {
		return fmt.Errorf("snapshot: create %s: %w", sessionsRoot, err)
	}
	s.root = sessionsRoot
	s.projectsRoot = projectsRoot
	s.projectID = projectID
	return nil
}

// BeginNewSession creates a fresh session directory, writes meta.json,
// and makes this Store refer to it. The Store must not already have an
// active session.
func (s *Store) BeginNewSession(projectRoot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return fmt.Errorf("snapshot: session %q already open", s.sessionID)
	}
	return s.beginNewSessionLocked(projectRoot, "")
}

// BeginNewSessionStaged creates a fresh session in an unlisted staging directory
// and atomically publishes it into the store's sessions root, so a partially
// initialized final session directory is never listed. It is the prepare-then-
// publish sequence over PrepareStagedNewSession and PublishPreparedSession. The
// Store must not have an active session; on any failure it is left inactive and
// only staging is removed. On success the store is active and rebound to the
// final location.
func (s *Store) BeginNewSessionStaged(projectRoot string) error {
	prepared, err := s.PrepareStagedNewSession(projectRoot)
	if err != nil {
		return err
	}
	return s.PublishPreparedSession(prepared)
}

// PrepareStagedNewSession prepares a fresh session under an unlisted staging
// directory and returns a staging-bound store the caller can write to before
// publication: the session files live at <staging>/<id>, so reads and writes
// through the returned store address the candidate in that window. This store
// is marked active and holds the session claim, but its own cached paths stay
// bound to the final location under s.root, which the candidate has not reached
// yet. Nothing is written to the sessions root, so abandoning the prepared
// store without publishing leaves no trace there. On any preparation failure
// the store is left inactive and only staging is removed.
//
// Between the prepare call and the publish call the receiver is mid-transaction:
// it is bound to a session that is not yet published, and it holds that session's
// claim. The caller is responsible for serializing these two calls against any
// other use of the receiver; the store does not serialize them for you. The
// prepared store returned here carries no claim of its own — the receiver holds
// it — so a caller that intends to keep using the prepared store after publish
// must account for that.
func (s *Store) PrepareStagedNewSession(projectRoot string) (*Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return nil, fmt.Errorf("snapshot: session %q already open", s.sessionID)
	}
	stagingRoot, err := NewStagingSessionsRoot(s.root)
	if err != nil {
		return nil, err
	}
	// Prepare under staging while the cached paths stay bound to the final
	// location, then publish by atomic rename; s.root never becomes a staging
	// path, so a concurrent reader never observes an unpublished candidate.
	if err := s.beginNewSessionAtLocked(projectRoot, "", stagingRoot); err != nil {
		return nil, cleanupStagingRoot(stagingRoot, err)
	}
	// The returned store mirrors the active session with its paths rooted at
	// staging, where the candidate's files actually live. It carries no claim:
	// this store holds the claim until Close, matching the single-call behavior.
	return &Store{
		root:         stagingRoot,
		projectsRoot: s.projectsRoot,
		projectID:    s.projectID,
		active:       true,
		dir:          filepath.Join(stagingRoot, s.sessionID),
		snapshotsDir: filepath.Join(stagingRoot, s.sessionID, "snapshots"),
		turnsDir:     filepath.Join(stagingRoot, s.sessionID, "turns"),
		sessionID:    s.sessionID,
		projectRoot:  s.projectRoot,
		projectHash:  s.projectHash,
	}, nil
}

// PublishPreparedSession atomically publishes a session prepared by
// PrepareStagedNewSession: it renames the candidate from its staging root into
// this store's sessions root and rebinds the prepared store to the final
// location, so reads and writes through it keep working after publication. Its
// failure semantics are asymmetric around the rename. A failure before the
// rename (the publish step) leaves nothing published: the session state is
// dropped, both stores are left inactive, and only staging is removed, so the
// sessions root never holds a partial session. A failure after the rename (the
// relocation step) leaves the session published with the receiver still owning
// it: the receiver stays active and claim-holding on the published session, and
// only the prepared store is detached as unusable.
//
// Between the prepare call and this call the receiver is mid-transaction: it is
// bound to a session that is not yet published, and it holds that session's
// claim. The caller is responsible for serializing these two calls against any
// other use of the receiver; the store does not serialize them for you. The
// prepared store returned by the prepare step carries no claim of its own — the
// receiver holds it — so a caller that intends to keep using the prepared store
// after publish must account for that.
func (s *Store) PublishPreparedSession(prepared *Store) error {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return ErrNoSession
	}
	stagingRoot := prepared.root
	finalRoot := s.root
	if err := PublishStagedSession(stagingRoot, finalRoot, s.sessionID); err != nil {
		// The session is active but its files are still under staging; drop it.
		s.clearLocked()
		s.mu.Unlock()
		prepared.Detach()
		return cleanupStagingRoot(stagingRoot, err)
	}
	s.mu.Unlock()
	if err := prepared.RelocateActiveSessionPaths(finalRoot); err != nil {
		// The rename already committed: the session is published and the
		// receiver keeps its claim on it. Only the prepared store is broken;
		// detach it, remove the now-empty staging parent (post-commit
		// cleanup, as on the success path), and leave the receiver active.
		prepared.Detach()
		_ = os.RemoveAll(stagingRoot)
		return err
	}
	// Files now live at s.dir; the empty staging parent is post-commit cleanup.
	_ = os.RemoveAll(stagingRoot)
	return nil
}

// BeginChildSession creates a fresh session linked to a parent session.
func (s *Store) BeginChildSession(projectRoot, parentSessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return fmt.Errorf("snapshot: session %q already open", s.sessionID)
	}
	return s.beginNewSessionLocked(projectRoot, parentSessionID)
}

func (s *Store) beginNewSessionLocked(projectRoot, parentSessionID string) error {
	return s.beginNewSessionAtLocked(projectRoot, parentSessionID, s.root)
}

// beginNewSessionAtLocked creates a fresh session's directory and meta under
// createRoot but binds the store's cached paths to its permanent location under
// s.root. For an in-place begin createRoot == s.root; for staged publication
// createRoot is an unlisted staging root and the caller renames <createRoot>/<id>
// into <s.root>/<id> before any store I/O, so the cached paths (and s.root) are
// never a staging path. Caller holds s.mu.
func (s *Store) beginNewSessionAtLocked(projectRoot, parentSessionID, createRoot string) error {
	absProject, err := filepath.Abs(projectRoot)
	if err != nil {
		return fmt.Errorf("snapshot: resolve project root: %w", err)
	}
	now := time.Now().Unix()
	meta := SessionMeta{
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		ProjectPath:      absProject,
		ProjectHash:      hashString(absProject),
		LightcodeVersion: lightcodeVersion,
		State:            StateActive,
		LastActivity:     now,
		ParentSessionID:  parentSessionID,
	}
	sessionID, err := s.mintReservedSessionID(createRoot, meta, true)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.root, sessionID)
	s.active = true
	s.dir = dir
	s.snapshotsDir = filepath.Join(dir, "snapshots")
	s.turnsDir = filepath.Join(dir, "turns")
	s.sessionID = sessionID
	s.projectRoot = absProject
	s.projectHash = meta.ProjectHash
	s.currentTurn = 0
	return nil
}

// LoadSession attaches this Store to an existing on-disk session.
// Discards any incomplete turn dirs (crash recovery).
func (s *Store) LoadSession(id string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return fmt.Errorf("snapshot: session %q already open", s.sessionID)
	}
	if verr := ValidateSessionID(id); verr != nil {
		return verr
	}
	// Claim the session before the crash-recovery mutation below, so only one
	// process ever discards this session's incomplete turns.
	if cerr := s.acquireClaimLocked(id); cerr != nil {
		return cerr
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.releaseClaimLocked(id))
		}
	}()
	dir := filepath.Join(s.root, id)
	var meta SessionMeta
	if rerr := readJSON(filepath.Join(dir, "meta.json"), &meta); rerr != nil {
		return fmt.Errorf("snapshot: load %s: %w", id, rerr)
	}
	snapshotsDir := filepath.Join(dir, "snapshots")
	turnsDir := filepath.Join(dir, "turns")
	for _, p := range []string{snapshotsDir, turnsDir} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return fmt.Errorf("snapshot: ensure %s: %w", p, err)
		}
	}
	s.active = true
	s.dir = dir
	s.snapshotsDir = snapshotsDir
	s.turnsDir = turnsDir
	s.sessionID = id
	s.projectRoot = meta.ProjectPath
	s.projectHash = meta.ProjectHash
	// Drop any turn dirs that did not reach their complete marker.
	s.discardIncompleteTurnsLocked()
	s.currentTurn = s.highestCompleteTurnLocked()
	return nil
}

// Close detaches the Store from its current session. If the session
// has no complete turns, the entire session dir is deleted (orphan
// cleanup for "opened a new session, switched away without using it").
// Returns (wasDiscarded, err). A Store with no session is a no-op.
func (s *Store) Close() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return false, nil
	}
	discarded := false
	if s.hasAnyCompleteTurnLocked() == false {
		if err := os.RemoveAll(s.dir); err != nil {
			return false, fmt.Errorf("snapshot: discard empty session %s: %w", s.sessionID, err)
		}
		discarded = true
	}
	s.clearLocked()
	return discarded, nil
}

// RelocateActiveSessionPaths rewrites the active store's cached directory
// paths to a new sessions root after its session directory has been
// atomically renamed there (staged fork/new publication). It performs no I/O:
// it recomputes root/dir/snapshotsDir/turnsDir from the known session id. The
// claim path derives from projectsRoot/projectID and is unaffected. The store
// must be active.
func (s *Store) RelocateActiveSessionPaths(finalSessionsRoot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return ErrNoSession
	}
	s.relocateActiveSessionPathsLocked(finalSessionsRoot)
	return nil
}

// relocateActiveSessionPathsLocked recomputes the cached session paths from the
// known session id and a new sessions root. Caller holds s.mu.
func (s *Store) relocateActiveSessionPathsLocked(finalSessionsRoot string) {
	s.root = finalSessionsRoot
	s.dir = filepath.Join(finalSessionsRoot, s.sessionID)
	s.snapshotsDir = filepath.Join(s.dir, "snapshots")
	s.turnsDir = filepath.Join(s.dir, "turns")
}

// Detach releases the store's claim and resets it to the no-session state
// without discarding the session directory. Used when an open fails after the
// claim was acquired but the persisted session must remain on disk (unlike
// Close, which discards a session that has no complete turns).
func (s *Store) Detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	s.clearLocked()
}

func (s *Store) clearLocked() {
	// Detach and Close do not surface the release result — no caller can act
	// on it — so a failure is left to ReleaseSessionClaim's stderr report.
	_ = s.releaseClaimLocked(s.sessionID)
	s.active = false
	s.dir = ""
	s.snapshotsDir = ""
	s.turnsDir = ""
	s.sessionID = ""
	s.projectRoot = ""
	s.projectHash = ""
	s.currentTurn = 0
	s.highWaterTurn = 0
	s.snapshotTx = nil
	s.mutationLock = nil
}

// Active reports whether a session is currently open.
func (s *Store) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// SessionID returns the session ID or "" if none is open.
func (s *Store) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Dir returns the session directory path, or "" if none is open.
func (s *Store) Dir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dir
}

// Root returns ~/.lightcode/sessions. Always valid.
func (s *Store) Root() string { return s.root }

// ProjectPath returns the project path recorded in session meta, or "".
func (s *Store) ProjectPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectRoot
}

// BeginTurn scans turn dirs (both snapshots/ and turns/), picks the next
// integer, creates empty turn dirs on both sides, and stores it as
// currentTurn. Returns 0 if no session is active.
func (s *Store) BeginTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return 0
	}
	next := s.nextTurnLocked()
	s.currentTurn = next
	_ = os.MkdirAll(filepath.Join(s.snapshotsDir, strconv.Itoa(next)), 0o700)
	_ = os.MkdirAll(filepath.Join(s.turnsDir, strconv.Itoa(next)), 0o700)
	return next
}

func (s *Store) nextTurnLocked() int {
	max := s.maxTurnLocked()
	if s.highWaterTurn > max {
		max = s.highWaterTurn
	}
	return max + 1
}

// maxTurnLocked returns the highest turn directory present on disk, across
// both the snapshots and the turns trees — the same union nextTurnLocked
// allocates from. Caller holds s.mu.
func (s *Store) maxTurnLocked() int {
	snap := readIntDirs(s.snapshotsDir)
	turn := readIntDirs(s.turnsDir)
	max := 0
	for _, n := range snap {
		if n > max {
			max = n
		}
	}
	for _, n := range turn {
		if n > max {
			max = n
		}
	}
	return max
}

// CurrentTurn returns the last BeginTurn result, 0 if unset.
func (s *Store) CurrentTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentTurn
}

// Snapshot captures the pre-turn state of absPath. First-write-wins per
// (turn, path).
func (s *Store) Snapshot(turn int, absPath string) error {
	resolved, err := pathutil.ResolveFilePath(absPath)
	if err != nil {
		return err
	}
	return s.SnapshotResolved(turn, absPath, resolved.CanonicalPath)
}

// SnapshotResolved captures canonicalPath while preserving originalPath for
// UI/history display. First-write-wins per canonical path.
func (s *Store) SnapshotResolved(turn int, originalPath, canonicalPath string) error {
	entryID, _, err := s.SnapshotResolvedEntry(turn, originalPath, canonicalPath)
	if err != nil {
		return err
	}
	s.RetainSnapshotEntry(turn, entryID)
	return nil
}

// SnapshotResolvedEntry is SnapshotResolved plus the concrete entry id and a
// created flag. Mutation callers must either retain the entry once mutation
// starts or discard their pre-mutation claim if validation fails first.
func (s *Store) SnapshotResolvedEntry(turn int, originalPath, canonicalPath string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return "", false, ErrNoSession
	}
	snapshotsDir := s.snapshotsDir
	if turn < 1 {
		return "", false, fmt.Errorf("snapshot: turn must be >= 1, got %d", turn)
	}
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
		if state := s.snapshotTxStateLocked(turn, pathHash, false); state != nil {
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
	state := s.snapshotTxStateLocked(turn, pathHash, true)
	state.refs++
	return pathHash, true, nil
}

// DiscardSnapshotEntry releases a pre-mutation snapshot user. The entry is
// removed only if no concurrent user retained it or still depends on it.
func (s *Store) DiscardSnapshotEntry(turn int, entryID string) error {
	if turn < 1 {
		return fmt.Errorf("snapshot: turn must be >= 1, got %d", turn)
	}
	if !isLegacyEntryID(entryID) {
		return fmt.Errorf("snapshot: invalid entry id %q", entryID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return ErrNoSession
	}
	key := snapshotTxKey(turn, entryID)
	state := s.snapshotTx[key]
	if state == nil {
		return nil
	}
	if state.refs > 0 {
		state.refs--
	}
	if state.refs > 0 {
		return nil
	}
	delete(s.snapshotTx, key)
	entryDir := filepath.Join(s.snapshotsDir, strconv.Itoa(turn), entryID)
	if err := os.RemoveAll(entryDir); err != nil {
		return fmt.Errorf("snapshot: discard %s: %w", entryDir, err)
	}
	return nil
}

// RetainSnapshotEntry keeps a snapshot entry once any user starts mutating.
func (s *Store) RetainSnapshotEntry(turn int, entryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	key := snapshotTxKey(turn, entryID)
	state := s.snapshotTx[key]
	if state == nil {
		return
	}
	delete(s.snapshotTx, key)
}

// LockSnapshotMutation serializes same-session mutations for one snapshot
// entry. Callers hold the returned release function until the disk mutation
// and last-write identity record have both completed.
func (s *Store) LockSnapshotMutation(turn int, entryID string) (func(), error) {
	if turn < 1 {
		return nil, fmt.Errorf("snapshot: turn must be >= 1, got %d", turn)
	}
	if !isLegacyEntryID(entryID) {
		return nil, fmt.Errorf("snapshot: invalid entry id %q", entryID)
	}
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil, ErrNoSession
	}
	key := snapshotTxKey(turn, entryID)
	if s.mutationLock == nil {
		s.mutationLock = make(map[string]*snapshotMutationLock)
	}
	lock := s.mutationLock[key]
	if lock == nil {
		lock = &snapshotMutationLock{}
		s.mutationLock[key] = lock
	}
	lock.refs++
	s.mu.Unlock()

	lock.mu.Lock()
	released := false
	return func() {
		if released {
			return
		}
		released = true
		lock.mu.Unlock()

		s.mu.Lock()
		defer s.mu.Unlock()
		lock.refs--
		if lock.refs == 0 && s.mutationLock[key] == lock {
			delete(s.mutationLock, key)
		}
	}, nil
}

// RecordSnapshotContent records the content produced by a successful mutation.
func (s *Store) RecordSnapshotContent(turn int, entryID string, content []byte) error {
	return s.recordSnapshotIdentity(turn, entryID, SnapshotContentIdentity{Hash: hashBytes(content)})
}

// RecordSnapshotAbsence records that a successful mutation removed the file.
func (s *Store) RecordSnapshotAbsence(turn int, entryID string) error {
	return s.recordSnapshotIdentity(turn, entryID, SnapshotContentIdentity{Absent: true})
}

func (s *Store) recordSnapshotIdentity(turn int, entryID string, identity SnapshotContentIdentity) error {
	if turn < 1 {
		return fmt.Errorf("snapshot: turn must be >= 1, got %d", turn)
	}
	if !isLegacyEntryID(entryID) {
		return fmt.Errorf("snapshot: invalid entry id %q", entryID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return ErrNoSession
	}
	metaPath := filepath.Join(s.snapshotsDir, strconv.Itoa(turn), entryID, "meta.json")
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

func (s *Store) snapshotTxStateLocked(turn int, entryID string, create bool) *snapshotTxState {
	key := snapshotTxKey(turn, entryID)
	if s.snapshotTx == nil {
		if !create {
			return nil
		}
		s.snapshotTx = make(map[string]*snapshotTxState)
	}
	state := s.snapshotTx[key]
	if state == nil && create {
		state = &snapshotTxState{}
		s.snapshotTx[key] = state
	}
	return state
}

func snapshotTxKey(turn int, entryID string) string {
	return strconv.Itoa(turn) + "/" + entryID
}

// AppendMessage appends one serialized message (one JSON object + \n)
// to turns/<turn>/messages.jsonl. Creates the turn dir if needed.
func (s *Store) AppendMessage(turn int, msg []byte) error {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return ErrNoSession
	}
	turnsDir := s.turnsDir
	s.mu.Unlock()
	if turn < 1 {
		return fmt.Errorf("snapshot: turn must be >= 1, got %d", turn)
	}
	dir := filepath.Join(turnsDir, strconv.Itoa(turn))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("snapshot: mkdir %s: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "messages.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("snapshot: open messages.jsonl: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(msg); err != nil {
		return fmt.Errorf("snapshot: append message: %w", err)
	}
	if len(msg) == 0 || msg[len(msg)-1] != '\n' {
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return fmt.Errorf("snapshot: append newline: %w", err)
		}
	}
	return nil
}

// MarkTurnComplete writes the empty `complete` marker in turns/<turn>/.
func (s *Store) MarkTurnComplete(turn int) error {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return ErrNoSession
	}
	turnsDir := s.turnsDir
	s.mu.Unlock()
	if turn < 1 {
		return fmt.Errorf("snapshot: turn must be >= 1, got %d", turn)
	}
	dir := filepath.Join(turnsDir, strconv.Itoa(turn))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "complete"), nil, 0o600)
}

// TurnMessages is one turn's persisted messages, in append order.
type TurnMessages struct {
	Turn     int
	Messages [][]byte
}

// LoadCompleteTurns returns every complete turn's messages in turn
// order. Incomplete turns are deleted from disk as a side effect.
func (s *Store) LoadCompleteTurns() ([]TurnMessages, error) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil, ErrNoSession
	}
	s.discardIncompleteTurnsLocked()
	turnsDir := s.turnsDir
	s.mu.Unlock()
	return loadCompleteTurnsFromDir(turnsDir, 0, false)
}

// LoadCompleteTurnsReadOnly returns complete turns without attempting crash
// recovery. It is safe for display reads while a turn is still in progress.
func (s *Store) LoadCompleteTurnsReadOnly() ([]TurnMessages, error) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil, ErrNoSession
	}
	turnsDir := s.turnsDir
	s.mu.Unlock()
	return loadCompleteTurnsFromDir(turnsDir, 0, false)
}

// SaveCompaction writes a compaction record to the session directory.
func (s *Store) SaveCompaction(rec CompactionRecord) error {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return ErrNoSession
	}
	dir := s.dir
	s.mu.Unlock()
	return writeJSON(filepath.Join(dir, "compaction.json"), rec)
}

// LoadCompaction reads the compaction record, or returns nil if none exists.
func (s *Store) LoadCompaction() (*CompactionRecord, error) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil, ErrNoSession
	}
	dir := s.dir
	s.mu.Unlock()
	return loadCompactionFromDir(dir)
}

// LoadCompactionForSession reads another session's compaction record without
// switching the active session.
func (s *Store) LoadCompactionForSession(id string) (*CompactionRecord, error) {
	dir, _, err := s.sessionReadDirs(id)
	if err != nil {
		return nil, err
	}
	return loadCompactionFromDir(dir)
}

func loadCompactionFromDir(dir string) (*CompactionRecord, error) {
	path := filepath.Join(dir, "compaction.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	var rec CompactionRecord
	if err := readJSON(path, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// LoadCompleteTurnsAfter returns complete turns with turn number > after.
func (s *Store) LoadCompleteTurnsAfter(after int) ([]TurnMessages, error) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil, ErrNoSession
	}
	s.discardIncompleteTurnsLocked()
	turnsDir := s.turnsDir
	s.mu.Unlock()
	return loadCompleteTurnsFromDir(turnsDir, after, true)
}

// LoadCompleteTurnsAfterReadOnly returns complete turns after the boundary
// without attempting crash recovery. It is safe for display reads while a turn
// is still in progress.
func (s *Store) LoadCompleteTurnsAfterReadOnly(after int) ([]TurnMessages, error) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil, ErrNoSession
	}
	turnsDir := s.turnsDir
	s.mu.Unlock()
	return loadCompleteTurnsFromDir(turnsDir, after, true)
}

// LoadCompleteTurnsForSessionReadOnly returns another session's complete turns
// without switching the active session or attempting crash recovery.
func (s *Store) LoadCompleteTurnsForSessionReadOnly(id string) ([]TurnMessages, error) {
	_, turnsDir, err := s.sessionReadDirs(id)
	if err != nil {
		return nil, err
	}
	return loadCompleteTurnsFromDir(turnsDir, 0, false)
}

// LoadCompleteTurnsAfterForSessionReadOnly returns another session's complete
// turns after the boundary without switching the active session or attempting
// crash recovery.
func (s *Store) LoadCompleteTurnsAfterForSessionReadOnly(id string, after int) ([]TurnMessages, error) {
	_, turnsDir, err := s.sessionReadDirs(id)
	if err != nil {
		return nil, err
	}
	return loadCompleteTurnsFromDir(turnsDir, after, true)
}

func loadCompleteTurnsFromDir(turnsDir string, after int, filterAfter bool) ([]TurnMessages, error) {
	turns := readIntDirs(turnsDir)
	var out []TurnMessages
	for _, n := range turns {
		if filterAfter && n <= after {
			continue
		}
		if _, err := os.Stat(filepath.Join(turnsDir, strconv.Itoa(n), "complete")); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(turnsDir, strconv.Itoa(n), "messages.jsonl"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("snapshot: read turn %d: %w", n, err)
		}
		lines := splitJSONL(data)
		out = append(out, TurnMessages{Turn: n, Messages: lines})
	}
	return out, nil
}

func (s *Store) sessionReadDirs(id string) (string, string, error) {
	if err := ValidateSessionID(id); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	root := s.root
	s.mu.Unlock()
	if root == "" {
		return "", "", ErrNoSession
	}
	dir := filepath.Join(root, id)
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		return "", "", fmt.Errorf("snapshot: load %s: %w", id, err)
	}
	return dir, filepath.Join(dir, "turns"), nil
}

// TurnEntry describes one snapshot turn for UI display.
type TurnEntry struct {
	Turn  int            `json:"turn"`
	Files []SnapshotMeta `json:"files"`
}

// ListTurns returns all recorded snapshot turns with their file meta.
func (s *Store) ListTurns() ([]TurnEntry, error) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil, ErrNoSession
	}
	snapshotsDir := s.snapshotsDir
	s.mu.Unlock()
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
// and turn dirs are NOT touched — conversation stays intact.
func (s *Store) RevertCode(toTurn int) (RevertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return RevertResult{}, ErrNoSession
	}
	if toTurn < 0 {
		toTurn = 0
	}
	// Record the highest turn number this session has issued before any
	// removal: the walk deletes snapshot turn dirs, so allocation from disk
	// alone would reissue numbers the session already used. The mark is raised,
	// never assigned — a later revert can scan a union maximum below it.
	if m := s.maxTurnLocked(); m > s.highWaterTurn {
		s.highWaterTurn = m
	}
	turns := readIntDirs(s.snapshotsDir)
	var result RevertResult
	skippedEntries := make(map[string]struct{})
	reportedSkippedEntries := make(map[string]struct{})
	for i := len(turns) - 1; i >= 0; i-- {
		turn := turns[i]
		if turn <= toTurn {
			break
		}
		turnDir := filepath.Join(s.snapshotsDir, strconv.Itoa(turn))
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

// RevertHistory deletes message turn dirs strictly greater than toTurn
// and updates currentTurn. Files on disk are NOT touched. The bool reports
// whether the compaction record was unlinked: it becomes true the moment
// the unlink succeeds, before the directory sync, so every return after
// that point carries it — the sync failure, a walk failure, and success
// alike, because the record is already gone from every reader. It is false
// on every path where no unlink happened: an inactive session, an
// unreadable record, a failed unlink, no record present, or a target at or
// above the boundary.
func (s *Store) RevertHistory(toTurn int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return false, ErrNoSession
	}
	if toTurn < 0 {
		toTurn = 0
	}
	// Record the highest turn number this session has issued before any
	// removal: the walk deletes message turn dirs, so allocation from disk
	// alone would reissue numbers the session already used. The mark is raised,
	// never assigned — a later revert can scan a union maximum below it.
	if m := s.maxTurnLocked(); m > s.highWaterTurn {
		s.highWaterTurn = m
	}
	// A revert below a compaction boundary empties the conversation: the
	// summary record would survive untouched and the load path would drop
	// every surviving turn behind the boundary it names. The record is
	// therefore removed before any turn, and the removal is a precondition of
	// the walk — a failed removal or a failed directory sync returns with no
	// turn removed, and so does an unreadable record, because walking on a
	// record whose survival is unknown truncates below a boundary whose
	// record may still exist, which is exactly the state this removal exists
	// to prevent.
	rec, err := loadCompactionFromDir(s.dir)
	if err != nil {
		return false, fmt.Errorf("snapshot: read compaction record: %w", err)
	}
	removedRecord := false
	if rec != nil && toTurn < rec.BoundaryTurn {
		if err := os.Remove(filepath.Join(s.dir, "compaction.json")); err != nil {
			return false, fmt.Errorf("snapshot: remove compaction record: %w", err)
		}
		removedRecord = true
		if err := atomicfs.SyncDir(s.dir); err != nil {
			return removedRecord, fmt.Errorf("snapshot: sync session dir: %w", err)
		}
	}
	// Walk descending and stop at the first failed removal, exactly like
	// RevertCode: the load path reads complete turns by scanning completion
	// markers with no upper bound, so a surviving turn directory above the
	// recorded truncation point would be re-read after a reload and the
	// reverted turn would come back. The truncation point is therefore lowered
	// only as far as removal actually reached — the failed turn survives, so
	// the recorded point stops at it.
	msgTurns := readIntDirs(s.turnsDir)
	for i := len(msgTurns) - 1; i >= 0; i-- {
		t := msgTurns[i]
		if t <= toTurn {
			break
		}
		if err := os.RemoveAll(filepath.Join(s.turnsDir, strconv.Itoa(t))); err != nil {
			if t < s.currentTurn {
				s.currentTurn = t
			}
			return removedRecord, fmt.Errorf("snapshot: revert history turn %d: %w", t, err)
		}
	}
	if toTurn >= 0 && toTurn < s.currentTurn {
		s.currentTurn = toTurn
	}
	return removedRecord, nil
}

// ForkInto copies turns 1..toTurn (both snapshots and messages) from the
// current session into a new session directory under destSessionsRoot. The
// current session is untouched. Returns the new session id and dir. The caller
// loads the copy through a store rooted at destSessionsRoot; for staged
// publication destSessionsRoot is an unlisted staging root later renamed
// atomically into the session namespace.
func (s *Store) ForkInto(toTurn int, destSessionsRoot string) (string, string, error) {
	s.mu.Lock()
	defer func() {
		forkIntoLockReleasedHook()
		s.mu.Unlock()
	}()
	if !s.active {
		return "", "", ErrNoSession
	}
	srcDir := s.dir
	projectRoot := s.projectRoot

	// Read source meta first so the mint can write the full fork meta under
	// the reservation lock; a read error fails the fork rather than
	// publishing a candidate with blank metadata.
	var srcMeta SessionMeta
	if err := readJSON(filepath.Join(srcDir, "meta.json"), &srcMeta); err != nil {
		return "", "", fmt.Errorf("snapshot: fork: read source meta: %w", err)
	}

	now := time.Now()
	meta := SessionMeta{
		CreatedAt:        now.UTC().Format(time.RFC3339),
		ProjectPath:      projectRoot,
		ProjectHash:      hashString(projectRoot),
		LightcodeVersion: lightcodeVersion,
		State:            StateActive,
		LastActivity:     now.Unix(),
		Provider:         srcMeta.Provider,
		Model:            srcMeta.Model,
		ActiveAgentType:  srcMeta.ActiveAgentType,
	}
	newID, err := s.mintReservedSessionID(destSessionsRoot, meta, false)
	if err != nil {
		return "", "", fmt.Errorf("snapshot: fork: %w", err)
	}
	newDir := filepath.Join(destSessionsRoot, newID)
	newSnapshots := filepath.Join(newDir, "snapshots")
	newTurns := filepath.Join(newDir, "turns")

	// Copy snapshot and turn dirs for turns 1..toTurn. A missing turn dir is
	// skipped; any other stat or copy error fails the fork so a partial or
	// silently truncated candidate is never published.
	for turn := 1; turn <= toTurn; turn++ {
		ts := strconv.Itoa(turn)
		srcSnap := filepath.Join(srcDir, "snapshots", ts)
		if info, err := os.Stat(srcSnap); err == nil {
			if !info.IsDir() {
				return "", "", fmt.Errorf("snapshot: fork: snapshots/%d is not a directory", turn)
			}
			if err := copyDirFunc(srcSnap, filepath.Join(newSnapshots, ts)); err != nil {
				return "", "", fmt.Errorf("snapshot: fork: copy snapshots/%d: %w", turn, err)
			}
		} else if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("snapshot: fork: stat snapshots/%d: %w", turn, err)
		}
		srcTurn := filepath.Join(srcDir, "turns", ts)
		if info, err := os.Stat(srcTurn); err == nil {
			if !info.IsDir() {
				return "", "", fmt.Errorf("snapshot: fork: turns/%d is not a directory", turn)
			}
			if err := copyDirFunc(srcTurn, filepath.Join(newTurns, ts)); err != nil {
				return "", "", fmt.Errorf("snapshot: fork: copy turns/%d: %w", turn, err)
			}
		} else if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("snapshot: fork: stat turns/%d: %w", turn, err)
		}
	}

	// Copy tokens.json if present; a real read/copy error fails the fork.
	srcTokens := filepath.Join(srcDir, "tokens.json")
	if _, err := os.Stat(srcTokens); err == nil {
		if err := copyFile(srcTokens, filepath.Join(newDir, "tokens.json")); err != nil {
			return "", "", fmt.Errorf("snapshot: fork: copy tokens: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("snapshot: fork: stat tokens: %w", err)
	}

	// Copy compaction.json only when its boundary exists in the forked history.
	srcCompaction := filepath.Join(srcDir, "compaction.json")
	if _, err := os.Stat(srcCompaction); err == nil {
		var rec CompactionRecord
		if err := readJSON(srcCompaction, &rec); err != nil {
			return "", "", fmt.Errorf("snapshot: fork: read compaction: %w", err)
		}
		if rec.BoundaryTurn <= toTurn {
			if err := copyFile(srcCompaction, filepath.Join(newDir, "compaction.json")); err != nil {
				return "", "", fmt.Errorf("snapshot: fork: copy compaction: %w", err)
			}
		}
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("snapshot: fork: stat compaction: %w", err)
	}

	return newID, newDir, nil
}

// copyDir recursively copies src directory to dst.
func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateMeta reads, mutates, and rewrites the session meta.json while
// holding the store lock, so two concurrent field updates cannot lose each
// other, and the rewrite is atomic so a reader always sees old-or-new
// complete JSON.
func (s *Store) updateMeta(mutate func(*SessionMeta)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return ErrNoSession
	}
	metaPath := filepath.Join(s.dir, "meta.json")
	var meta SessionMeta
	if err := readJSON(metaPath, &meta); err != nil {
		return err
	}
	mutate(&meta)
	return writeJSON(metaPath, meta)
}

// TouchActivity updates LastActivity in meta.json to now. Called on
// every user message. Also advances the owning project's meta.json when
// the store is project-aware.
func (s *Store) TouchActivity() error {
	if err := s.SetLastActivity(time.Now().Unix()); err != nil {
		return err
	}
	return s.TouchProjectActivity()
}

// SetLastActivity stamps only the session meta's LastActivity to ts. It does not touch
// project activity, so a caller can report the session's committed LastActivity in a
// boundary summary based solely on this write's success.
func (s *Store) SetLastActivity(ts int64) error {
	return s.updateMeta(func(m *SessionMeta) {
		m.LastActivity = ts
	})
}

// TouchProjectActivity records project-level recent activity; it never affects the
// session's own LastActivity, so its failure does not make a boundary summary that
// already reported the session's LastActivity inconsistent with disk.
func (s *Store) TouchProjectActivity() error {
	s.mu.Lock()
	projectsRoot := s.projectsRoot
	projectID := s.projectID
	s.mu.Unlock()
	if projectsRoot != "" && projectID != "" {
		return project.TouchActivity(projectsRoot, projectID)
	}
	return nil
}

// Meta reads the session's meta.json.
func (s *Store) Meta() (SessionMeta, error) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return SessionMeta{}, ErrNoSession
	}
	dir := s.dir
	s.mu.Unlock()
	var m SessionMeta
	err := readJSON(filepath.Join(dir, "meta.json"), &m)
	return m, err
}

// SetModel writes provider + model fields into meta.json.
func (s *Store) SetModel(provider, model string) error {
	return s.updateMeta(func(m *SessionMeta) {
		m.Provider = provider
		m.Model = model
	})
}

// SetActiveAgentType writes the active_agent_type field into meta.json.
func (s *Store) SetActiveAgentType(agentType string) error {
	return s.updateMeta(func(m *SessionMeta) {
		m.ActiveAgentType = agentType
	})
}

// SetState writes state + archived_at fields into meta.json.
func (s *Store) SetState(state string) error {
	return s.updateMeta(func(m *SessionMeta) {
		m.State = state
		if state == StateArchived {
			m.ArchivedAt = time.Now().Unix()
		} else {
			m.ArchivedAt = 0
		}
	})
}

// --- internal helpers ---

func (s *Store) discardIncompleteTurnsLocked() {
	if s.turnsDir == "" {
		return
	}
	for _, n := range readIntDirs(s.turnsDir) {
		dir := filepath.Join(s.turnsDir, strconv.Itoa(n))
		if _, err := os.Stat(filepath.Join(dir, "complete")); err == nil {
			continue
		}
		// Attempt crash recovery.
		recovered := recoverTurnMessages(dir)
		if len(recovered) == 0 {
			_ = os.RemoveAll(dir)
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), recovered, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			continue
		}
		_ = os.WriteFile(filepath.Join(dir, "complete"), nil, 0o600)
	}
}

// recoverTurnMessages attempts to recover complete iterations from a
// partially-written messages.jsonl. Returns the recovered JSONL content
// or nil if nothing can be recovered.
func recoverTurnMessages(dir string) []byte {
	data, err := os.ReadFile(filepath.Join(dir, "messages.jsonl"))
	if err != nil {
		return nil
	}
	lines := splitJSONL(data)

	if len(lines) == 0 {
		return nil
	}

	// Skip malformed last line (crash mid-write guard).
	var lastCheck map[string]any
	if json.Unmarshal(lines[len(lines)-1], &lastCheck) != nil {
		lines = lines[:len(lines)-1]
	}

	if len(lines) == 0 {
		return nil
	}

	// Parse each line into a structured form.
	type msgInfo struct {
		raw        []byte
		role       string
		content    string
		toolCallID string   // for tool messages
		toolCalls  []string // tool call IDs from assistant messages
	}
	var msgs []msgInfo
	for _, line := range lines {
		var raw map[string]any
		if json.Unmarshal(line, &raw) != nil {
			continue
		}
		info := msgInfo{raw: line}
		info.role, _ = raw["role"].(string)
		info.content, _ = raw["content"].(string)
		info.toolCallID, _ = raw["tool_call_id"].(string)
		if tcs, ok := raw["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				if tcMap, ok := tc.(map[string]any); ok {
					if id, ok := tcMap["id"].(string); ok {
						info.toolCalls = append(info.toolCalls, id)
					}
				}
			}
		}
		msgs = append(msgs, info)
	}

	// Identify complete iterations: an assistant message with tool calls
	// followed by matching tool result messages.
	// Walk through and mark which assistant messages are complete.
	type iterMark struct {
		msgIdx   int
		complete bool
		endIdx   int // last tool result index for this iteration
	}
	var iters []iterMark
	i := 0
	for i < len(msgs) {
		m := msgs[i]
		if m.role == "assistant" && len(m.toolCalls) > 0 {
			expected := make(map[string]bool, len(m.toolCalls))
			for _, id := range m.toolCalls {
				expected[id] = true
			}
			seen := make(map[string]bool)
			endIdx := i
			for j := i + 1; j < len(msgs); j++ {
				if msgs[j].role == "tool" {
					seen[msgs[j].toolCallID] = true
					endIdx = j
				} else {
					break
				}
			}
			allFound := true
			for id := range expected {
				if !seen[id] {
					allFound = false
					break
				}
			}
			iters = append(iters, iterMark{msgIdx: i, complete: allFound, endIdx: endIdx})
			i = endIdx + 1
		} else {
			i++
		}
	}

	// Discard last incomplete iteration.
	if len(iters) > 0 && !iters[len(iters)-1].complete {
		iters = iters[:len(iters)-1]
	}

	if len(iters) == 0 {
		return nil
	}

	// Rebuild: keep user messages and complete iterations, preserving order.
	// Build a set of indices to keep.
	keep := make(map[int]bool)
	for _, it := range iters {
		if it.complete {
			keep[it.msgIdx] = true
			for k := it.msgIdx + 1; k <= it.endIdx; k++ {
				keep[k] = true
			}
		}
	}
	for idx, m := range msgs {
		if m.role == "user" {
			keep[idx] = true
		}
	}
	// Also keep final text-only assistant messages (no tool calls).
	for idx := len(msgs) - 1; idx >= 0; idx-- {
		if !keep[idx] {
			break
		}
	}
	// Find the last assistant message without tool calls and keep it if it's after the last iteration.
	if len(iters) > 0 {
		lastIterEnd := iters[len(iters)-1].endIdx
		for idx := lastIterEnd + 1; idx < len(msgs); idx++ {
			if msgs[idx].role == "assistant" && len(msgs[idx].toolCalls) == 0 && msgs[idx].content != "" {
				keep[idx] = true
			}
		}
	} else {
		// No iterations — keep user messages and any text-only responses.
		for idx := range msgs {
			if msgs[idx].role == "assistant" && len(msgs[idx].toolCalls) == 0 && msgs[idx].content != "" {
				keep[idx] = true
			}
		}
	}

	var rebuild []byte
	for idx, m := range msgs {
		if keep[idx] {
			rebuild = append(rebuild, m.raw...)
			rebuild = append(rebuild, '\n')
		}
	}

	if len(rebuild) == 0 {
		return nil
	}
	return rebuild
}

func (s *Store) hasAnyCompleteTurnLocked() bool {
	if s.turnsDir == "" {
		return false
	}
	for _, n := range readIntDirs(s.turnsDir) {
		if _, err := os.Stat(filepath.Join(s.turnsDir, strconv.Itoa(n), "complete")); err == nil {
			return true
		}
	}
	return false
}

func (s *Store) highestCompleteTurnLocked() int {
	if s.turnsDir == "" {
		return 0
	}
	max := 0
	for _, n := range readIntDirs(s.turnsDir) {
		if _, err := os.Stat(filepath.Join(s.turnsDir, strconv.Itoa(n), "complete")); err == nil {
			if n > max {
				max = n
			}
		}
	}
	return max
}

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

func splitJSONL(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			line := data[start:i]
			if len(line) > 0 {
				cp := make([]byte, len(line))
				copy(cp, line)
				out = append(out, cp)
			}
			start = i + 1
		}
	}
	if start < len(data) {
		line := data[start:]
		if len(line) > 0 {
			cp := make([]byte, len(line))
			copy(cp, line)
			out = append(out, cp)
		}
	}
	return out
}

func newSessionID() (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// mintReservedSessionID draws a fresh session id and reserves it on disk
// while holding mintMu: it scans both namespaces of every project under the
// store's projects root for an existing use of the drawn id, retrying on a
// collision, and on a clean scan claims the id (when claim is set), creates
// its directory under createRoot, and writes its meta. The directory is the
// reservation, so the lock must span its creation: if it were released
// before the winning id's directory existed, a second mint could scan the
// same roots, see no collision, and draw the same id. A collided attempt
// creates nothing, so no cleanup is needed between retries. The hold covers
// local filesystem work only and is released before any turn or loop work.
func (s *Store) mintReservedSessionID(createRoot string, meta SessionMeta, claim bool) (string, error) {
	mintMu.Lock()
	defer mintMu.Unlock()
	for attempt := 0; attempt < mintSessionIDMaxAttempts; attempt++ {
		id, err := mintSessionIDFunc()
		if err != nil {
			return "", fmt.Errorf("snapshot: generate session id: %w", err)
		}
		used, err := s.sessionIDExistsAnywhere(id)
		if err != nil {
			return "", fmt.Errorf("snapshot: scan session id %s: %w", id, err)
		}
		if used {
			continue
		}
		if claim {
			if cerr := s.acquireClaimLocked(id); cerr != nil {
				return "", cerr
			}
		}
		createDir := filepath.Join(createRoot, id)
		for _, p := range []string{filepath.Join(createDir, "snapshots"), filepath.Join(createDir, "turns")} {
			if err := os.MkdirAll(p, 0o700); err != nil {
				if claim {
					err = errors.Join(err, s.releaseClaimLocked(id))
				}
				return "", fmt.Errorf("snapshot: create %s: %w", p, err)
			}
		}
		meta.ID = id
		if err := writeJSON(filepath.Join(createDir, "meta.json"), meta); err != nil {
			if claim {
				err = errors.Join(err, s.releaseClaimLocked(id))
			}
			return "", fmt.Errorf("snapshot: write session meta: %w", err)
		}
		return id, nil
	}
	return "", fmt.Errorf("snapshot: could not mint a unique session id in %d attempts", mintSessionIDMaxAttempts)
}

// sessionIDExistsAnywhere reports whether id is already present on disk in
// either namespace of any project the store can see: the visible sessions
// directories under each project and the staged candidates under
// <project>/.staging/sessions/<nonce>. Staged candidates are deliberately
// invisible to session enumeration but are already minted, so a scan of only
// the visible directories cannot see them; the nonce is not known, only
// enumerated. Only a store without a projects root (legacy and test stores)
// falls back to scanning its own root instead.
func (s *Store) sessionIDExistsAnywhere(id string) (bool, error) {
	if s.projectsRoot != "" {
		entries, err := os.ReadDir(s.projectsRoot)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			projectDir := filepath.Join(s.projectsRoot, e.Name())
			used, err := sessionIDInNamespaces(filepath.Join(projectDir, "sessions"), projectDir, id)
			if used || err != nil {
				return used, err
			}
		}
		return false, nil
	}
	if s.root != "" {
		return sessionIDInNamespaces(s.root, filepath.Dir(s.root), id)
	}
	return false, nil
}

// scanPathMissing reports whether err means the scanned path cannot hold a
// session id: the parent is missing, or a component is not a directory (Go
// no longer folds ENOTDIR into os.IsNotExist). Either way the id is absent.
func scanPathMissing(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)
}

// sessionIDInNamespaces reports whether id exists under visibleRoot (the
// visible sessions namespace) or under any staged candidate at
// <projectDir>/.staging/sessions/<nonce>/<id> — one level deeper in the same
// per-project walk the lifecycle sweep performs.
func sessionIDInNamespaces(visibleRoot, projectDir, id string) (bool, error) {
	if _, err := os.Stat(filepath.Join(visibleRoot, id)); err == nil {
		return true, nil
	} else if !scanPathMissing(err) {
		return false, err
	}
	stagingRoot := filepath.Join(projectDir, ".staging", "sessions")
	nonces, err := os.ReadDir(stagingRoot)
	if err != nil {
		if scanPathMissing(err) {
			return false, nil
		}
		return false, err
	}
	for _, n := range nonces {
		if !n.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(stagingRoot, n.Name(), id)); err == nil {
			return true, nil
		} else if !scanPathMissing(err) {
			return false, err
		}
	}
	return false, nil
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
