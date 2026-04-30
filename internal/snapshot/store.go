package snapshot

import (
	"crypto/rand"
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
	"time"
)

// lightcodeVersion is written into each new session's meta.json. Bump
// when the on-disk layout changes in a backward-incompatible way.
const lightcodeVersion = "0.3.0"

// ErrNoSession is returned by any Store method that needs an active
// session when none is open.
var ErrNoSession = errors.New("snapshot: no session open")

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
	return s.beginNewSessionLocked(projectRoot)
}

func (s *Store) beginNewSessionLocked(projectRoot string) error {
	absProject, err := filepath.Abs(projectRoot)
	if err != nil {
		return fmt.Errorf("snapshot: resolve project root: %w", err)
	}
	sessionID, err := newSessionID()
	if err != nil {
		return fmt.Errorf("snapshot: generate session id: %w", err)
	}
	dir := filepath.Join(s.root, sessionID)
	snapshotsDir := filepath.Join(dir, "snapshots")
	turnsDir := filepath.Join(dir, "turns")
	for _, p := range []string{snapshotsDir, turnsDir} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return fmt.Errorf("snapshot: create %s: %w", p, err)
		}
	}
	projectHash := hashString(absProject)
	now := time.Now().Unix()
	meta := SessionMeta{
		ID:               sessionID,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		ProjectPath:      absProject,
		ProjectHash:      projectHash,
		LightcodeVersion: lightcodeVersion,
		State:            StateActive,
		LastActivity:     now,
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
		return fmt.Errorf("snapshot: write session meta: %w", err)
	}
	s.active = true
	s.dir = dir
	s.snapshotsDir = snapshotsDir
	s.turnsDir = turnsDir
	s.sessionID = sessionID
	s.projectRoot = absProject
	s.projectHash = projectHash
	s.currentTurn = 0
	return nil
}

// LoadSession attaches this Store to an existing on-disk session.
// Discards any incomplete turn dirs (crash recovery).
func (s *Store) LoadSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return fmt.Errorf("snapshot: session %q already open", s.sessionID)
	}
	dir := filepath.Join(s.root, id)
	var meta SessionMeta
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return fmt.Errorf("snapshot: load %s: %w", id, err)
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

func (s *Store) clearLocked() {
	s.active = false
	s.dir = ""
	s.snapshotsDir = ""
	s.turnsDir = ""
	s.sessionID = ""
	s.projectRoot = ""
	s.projectHash = ""
	s.currentTurn = 0
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
	return max + 1
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
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return ErrNoSession
	}
	snapshotsDir := s.snapshotsDir
	s.mu.Unlock()
	if turn < 1 {
		return fmt.Errorf("snapshot: turn must be >= 1, got %d", turn)
	}
	pathHash := hashString(absPath)
	entryDir := filepath.Join(snapshotsDir, strconv.Itoa(turn), pathHash)
	metaPath := filepath.Join(entryDir, "meta.json")
	if _, err := os.Stat(metaPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		return fmt.Errorf("snapshot: mkdir %s: %w", entryDir, err)
	}
	existed := true
	info, statErr := os.Stat(absPath)
	if statErr != nil {
		if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("snapshot: stat %s: %w", absPath, statErr)
		}
		existed = false
	} else if info.IsDir() {
		existed = false
	}
	if existed {
		if err := copyFile(absPath, filepath.Join(entryDir, "original")); err != nil {
			return fmt.Errorf("snapshot: copy %s: %w", absPath, err)
		}
	}
	meta := SnapshotMeta{OriginalPath: absPath, Existed: existed}
	if err := writeJSON(metaPath, meta); err != nil {
		return fmt.Errorf("snapshot: write meta: %w", err)
	}
	return nil
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
	turns := readIntDirs(turnsDir)
	s.mu.Unlock()
	var out []TurnMessages
	for _, n := range turns {
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
	turns := readIntDirs(turnsDir)
	s.mu.Unlock()
	var out []TurnMessages
	for _, n := range turns {
		if n <= after {
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
func (s *Store) RevertCode(toTurn int) ([]string, error) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil, ErrNoSession
	}
	snapshotsDir := s.snapshotsDir
	s.mu.Unlock()
	if toTurn < 0 {
		toTurn = 0
	}
	turns := readIntDirs(snapshotsDir)
	var affected []string
	for i := len(turns) - 1; i >= 0; i-- {
		turn := turns[i]
		if turn <= toTurn {
			break
		}
		turnDir := filepath.Join(snapshotsDir, strconv.Itoa(turn))
		paths, err := revertOneTurn(turnDir)
		if err != nil {
			return affected, fmt.Errorf("snapshot: revert turn %d: %w", turn, err)
		}
		affected = append(affected, paths...)
		if err := os.RemoveAll(turnDir); err != nil {
			return affected, fmt.Errorf("snapshot: remove %s: %w", turnDir, err)
		}
	}
	return affected, nil
}

// RevertHistory deletes message turn dirs strictly greater than toTurn
// and updates currentTurn. Files on disk are NOT touched.
func (s *Store) RevertHistory(toTurn int) error {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return ErrNoSession
	}
	turnsDir := s.turnsDir
	s.mu.Unlock()
	if toTurn < 0 {
		toTurn = 0
	}
	msgTurns := readIntDirs(turnsDir)
	for _, t := range msgTurns {
		if t > toTurn {
			_ = os.RemoveAll(filepath.Join(turnsDir, strconv.Itoa(t)))
		}
	}
	s.mu.Lock()
	if toTurn >= 0 && toTurn < s.currentTurn {
		s.currentTurn = toTurn
	}
	s.mu.Unlock()
	return nil
}

// ForkInto copies turns 1..toTurn (both snapshots and messages) into a
// new session directory. The current session is untouched. Returns the
// path of the new session dir. Caller is responsible for switching to it.
func (s *Store) ForkInto(toTurn int) (string, string, error) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return "", "", ErrNoSession
	}
	srcDir := s.dir
	root := s.root
	projectRoot := s.projectRoot
	s.mu.Unlock()

	newID, err := newSessionID()
	if err != nil {
		return "", "", fmt.Errorf("snapshot: fork: generate id: %w", err)
	}
	newDir := filepath.Join(root, newID)
	newSnapshots := filepath.Join(newDir, "snapshots")
	newTurns := filepath.Join(newDir, "turns")
	for _, p := range []string{newSnapshots, newTurns} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return "", "", fmt.Errorf("snapshot: fork: mkdir %s: %w", p, err)
		}
	}

	// Copy snapshot and turn dirs for turns 1..toTurn.
	for turn := 1; turn <= toTurn; turn++ {
		ts := strconv.Itoa(turn)
		srcSnap := filepath.Join(srcDir, "snapshots", ts)
		if info, err := os.Stat(srcSnap); err == nil && info.IsDir() {
			if err := copyDir(srcSnap, filepath.Join(newSnapshots, ts)); err != nil {
				return "", "", fmt.Errorf("snapshot: fork: copy snapshots/%d: %w", turn, err)
			}
		}
		srcTurn := filepath.Join(srcDir, "turns", ts)
		if info, err := os.Stat(srcTurn); err == nil && info.IsDir() {
			if err := copyDir(srcTurn, filepath.Join(newTurns, ts)); err != nil {
				return "", "", fmt.Errorf("snapshot: fork: copy turns/%d: %w", turn, err)
			}
		}
	}

	// Copy tokens.json if present.
	srcTokens := filepath.Join(srcDir, "tokens.json")
	if _, err := os.Stat(srcTokens); err == nil {
		_ = copyFile(srcTokens, filepath.Join(newDir, "tokens.json"))
	}

	// Read source meta to carry over model fields.
	var srcMeta SessionMeta
	_ = readJSON(filepath.Join(srcDir, "meta.json"), &srcMeta)

	// Write meta.json for the new session.
	now := time.Now()
	meta := SessionMeta{
		ID:               newID,
		CreatedAt:        now.UTC().Format(time.RFC3339),
		ProjectPath:      projectRoot,
		ProjectHash:      hashString(projectRoot),
		LightcodeVersion: lightcodeVersion,
		State:            StateActive,
		LastActivity:     now.Unix(),
		Provider:         srcMeta.Provider,
		Model:            srcMeta.Model,
	}
	if err := writeJSON(filepath.Join(newDir, "meta.json"), meta); err != nil {
		return "", "", fmt.Errorf("snapshot: fork: write meta: %w", err)
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

// TouchActivity updates LastActivity in meta.json to now. Called on
// every user message. Also bumps the owning project's meta.json when
// the store is project-aware.
func (s *Store) TouchActivity() error {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return ErrNoSession
	}
	dir := s.dir
	projectsRoot := s.projectsRoot
	projectID := s.projectID
	s.mu.Unlock()
	metaPath := filepath.Join(dir, "meta.json")
	var meta SessionMeta
	if err := readJSON(metaPath, &meta); err != nil {
		return err
	}
	meta.LastActivity = time.Now().Unix()
	if err := writeJSON(metaPath, meta); err != nil {
		return err
	}
	if projectsRoot != "" && projectID != "" {
		projectMeta := filepath.Join(projectsRoot, projectID, "meta.json")
		var pm map[string]any
		if err := readJSON(projectMeta, &pm); err == nil {
			pm["last_activity"] = time.Now().Unix()
			_ = writeJSON(projectMeta, pm)
		}
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
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return ErrNoSession
	}
	dir := s.dir
	s.mu.Unlock()
	metaPath := filepath.Join(dir, "meta.json")
	var meta SessionMeta
	if err := readJSON(metaPath, &meta); err != nil {
		return err
	}
	meta.Provider = provider
	meta.Model = model
	return writeJSON(metaPath, meta)
}

// SetState writes state + archived_at fields into meta.json.
func (s *Store) SetState(state string) error {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return ErrNoSession
	}
	dir := s.dir
	s.mu.Unlock()
	metaPath := filepath.Join(dir, "meta.json")
	var meta SessionMeta
	if err := readJSON(metaPath, &meta); err != nil {
		return err
	}
	meta.State = state
	if state == StateArchived {
		meta.ArchivedAt = time.Now().Unix()
	} else {
		meta.ArchivedAt = 0
	}
	return writeJSON(metaPath, meta)
}

// --- internal helpers ---

func (s *Store) discardIncompleteTurnsLocked() {
	if s.turnsDir == "" {
		return
	}
	for _, n := range readIntDirs(s.turnsDir) {
		dir := filepath.Join(s.turnsDir, strconv.Itoa(n))
		if _, err := os.Stat(filepath.Join(dir, "complete")); err != nil {
			_ = os.RemoveAll(dir)
		}
	}
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

func revertOneTurn(turnDir string) ([]string, error) {
	entries, err := os.ReadDir(turnDir)
	if err != nil {
		return nil, err
	}
	var affected []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		entryDir := filepath.Join(turnDir, e.Name())
		var meta SnapshotMeta
		if err := readJSON(filepath.Join(entryDir, "meta.json"), &meta); err != nil {
			return affected, fmt.Errorf("read snapshot meta in %s: %w", entryDir, err)
		}
		if err := restoreOne(entryDir, meta); err != nil {
			return affected, fmt.Errorf("restore %s: %w", meta.OriginalPath, err)
		}
		affected = append(affected, meta.OriginalPath)
	}
	return affected, nil
}

func restoreOne(entryDir string, meta SnapshotMeta) error {
	if !meta.Existed {
		if err := os.Remove(meta.OriginalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if parent := filepath.Dir(meta.OriginalPath); parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("mkdir parent %s: %w", parent, err)
		}
	}
	src := filepath.Join(entryDir, "original")
	return copyFile(src, meta.OriginalPath)
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

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
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
