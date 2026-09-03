// SQLite implementation of the public harness storage contract. OpenSQLite
// opens the store at an explicit caller-supplied path; no production path is
// supplied or derived here, and backend lifecycle is not part of the public
// interface.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/MMinasyan/lightcode/harness"
)

const (
	sqliteDriverName = "sqlite3"

	// sqliteSchemaVersion is the only accepted schema version; it is stamped
	// into PRAGMA user_version at initialization and checked at open.
	sqliteSchemaVersion = 1

	// sqlitePoolDSN configures every physical connection of the long-lived
	// pool: write-ahead logging, full synchronous durability, immediate
	// transaction locking, and a five-second busy timeout. The driver applies
	// each value as the connection opens and translates _txlock=immediate to
	// BEGIN IMMEDIATE for every database/sql transaction.
	sqlitePoolDSN = "_journal_mode=WAL&_synchronous=FULL&_txlock=immediate&_busy_timeout=5000"

	// sqliteInitDSN configures the one temporary initialization connection: a
	// rollback-journal transaction with full synchronous durability and
	// immediate locking. No journal mode is set, so the initialized file is
	// not switched to WAL before the configured pool opens.
	sqliteInitDSN = "_synchronous=FULL&_txlock=immediate"
)

const sqliteEntriesDDL = `CREATE TABLE entries (
    session_id      TEXT    NOT NULL CHECK (session_id <> ''),
    sequence        INTEGER NOT NULL CHECK (sequence > 0),
    entry_id        TEXT    NOT NULL UNIQUE CHECK (entry_id <> ''),
    operation_id    TEXT    NOT NULL,
    kind            TEXT    NOT NULL CHECK (kind IN (
        'input', 'assistant', 'tool_result', 'signal',
        'hook_result', 'compaction', 'operation_settlement'
    )),
    committed_at_ns INTEGER NOT NULL CHECK (committed_at_ns > 0),
    payload         TEXT    NOT NULL CHECK (json_valid(payload)),
    PRIMARY KEY (session_id, sequence)
) STRICT`

const sqliteRegistersDDL = `CREATE TABLE registers (
    session_id   TEXT    NOT NULL CHECK (session_id <> ''),
    kind         TEXT    NOT NULL CHECK (kind IN ('session', 'operation')),
    operation_id TEXT    NOT NULL,
    revision     INTEGER NOT NULL CHECK (revision > 0),
    payload      TEXT    NOT NULL CHECK (json_valid(payload)),
    CHECK (
        (kind = 'session' AND operation_id = '') OR
        (kind = 'operation' AND operation_id <> '')
    ),
    PRIMARY KEY (session_id, kind, operation_id)
) STRICT`

const sqliteOperationIndexDDL = `CREATE UNIQUE INDEX operation_register_id
ON registers(operation_id) WHERE kind = 'operation'`

// sqliteSchemaObject is one user-defined schema object. Creation and
// read-only validation share the canonical set.
type sqliteSchemaObject struct {
	typ  string
	name string
	ddl  string
}

// sqliteCanonicalSchema is the complete user-defined object set of schema
// version 1: the two canonical strict tables and one explicit partial index.
// The entries primary key supplies the ordered-read index; parent
// session-register existence is checked by the storage operations, so no
// third table or trigger exists.
var sqliteCanonicalSchema = []sqliteSchemaObject{
	{"table", "entries", sqliteEntriesDDL},
	{"table", "registers", sqliteRegistersDDL},
	{"index", "operation_register_id", sqliteOperationIndexDDL},
}

// sqliteDSN builds one driver DSN for the caller path as an escaped file URI
// with the given query parameters, so reserved URI characters in the path
// cannot truncate the filename or redirect the open to a sibling database.
func sqliteDSN(path string, params string) string {
	uri := &url.URL{Path: path}
	return "file:" + uri.EscapedPath() + "?" + params
}

// OpenSQLite opens the production SQLite implementation at an explicit
// caller-supplied path. A missing or zero-length file is initialized with the
// canonical schema version 1; an existing non-empty file is first validated
// through a short-lived read-only connection and only the exact canonical
// schema is accepted. Only after initialization or validation succeeds is the
// long-lived configured pool created. No migration, repair, compatibility
// reader, or old-filesystem import exists, and no rejected database is ever
// reopened through the configured writable pool.
func OpenSQLite(path string) (*SQLite, error) {
	if path == "" {
		return nil, invalidf("database path is empty")
	}
	// The input is strictly a caller-supplied filesystem path, never a SQLite
	// special DSN: resolving it to an absolute path turns relative literal
	// filenames such as `:memory:` into durable files instead of ephemeral
	// special databases.
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, storagef("resolve database path %q: %v", path, err)
	}
	path = absolute
	info, err := os.Stat(path)
	switch {
	case err == nil && info.Size() > 0:
		if err := validateSQLiteSchema(path); err != nil {
			return nil, err
		}
	case err == nil, errors.Is(err, fs.ErrNotExist):
		if err := initSQLiteSchema(path); err != nil {
			return nil, err
		}
	default:
		return nil, storagef("stat database %q: %v", path, err)
	}
	db, err := sql.Open(sqliteDriverName, sqliteDSN(path, sqlitePoolDSN))
	if err != nil {
		return nil, storagef("open pool for %q: %v", path, err)
	}
	if err := verifySQLitePool(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLite{db: db, path: path}, nil
}

// initSQLiteSchema initializes a missing or zero-length database file with the
// canonical tables, index, and schema version in one rollback-journal
// transaction, then closes the initialization connection.
func initSQLiteSchema(path string) error {
	db, err := sql.Open(sqliteDriverName, sqliteDSN(path, sqliteInitDSN))
	if err != nil {
		return storagef("open initialization connection for %q: %v", path, err)
	}
	defer db.Close()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return storagef("begin schema initialization of %q: %v", path, err)
	}
	for _, object := range sqliteCanonicalSchema {
		if _, err := tx.Exec(object.ddl); err != nil {
			_ = tx.Rollback()
			return storagef("create %s %s in %q: %v", object.typ, object.name, path, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", sqliteSchemaVersion)); err != nil {
		_ = tx.Rollback()
		return storagef("set schema version of %q: %v", path, err)
	}
	if err := tx.Commit(); err != nil {
		return storagef("commit schema initialization of %q: %v", path, err)
	}
	return nil
}

// validateSQLiteSchema opens an existing non-empty database through one
// short-lived read-only connection with no persistent PRAGMA configuration.
// Read-only validation may create or recover SQLite-owned -shm WAL-index
// metadata when required to inspect current -wal state; that read metadata may
// remain, but validation does not write database or WAL bytes, change
// persistent journal mode, migrate, repair, or open the configured writable
// pool before acceptance. It accepts only schema version 1 whose complete
// user-defined sqlite_schema object set and definitions match the canonical
// schema; SQLite-generated autoindexes carry no sql text and are implied by
// the canonical tables. Any other version, or any missing, changed, or
// additional table, index, view, or trigger, is incompatible.
func validateSQLiteSchema(path string) error {
	db, err := sql.Open(sqliteDriverName, sqliteDSN(path, "mode=ro"))
	if err != nil {
		return storagef("open read-only validation connection for %q: %v", path, err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return storagef("read schema version of %q: %v", path, err)
	}
	if version != sqliteSchemaVersion {
		return &harness.IncompatibleSchemaError{
			Found:     version,
			Supported: sqliteSchemaVersion,
			Detail:    fmt.Sprintf("database %q declares schema version %d", path, version),
		}
	}

	rows, err := db.Query(`SELECT type, name, sql FROM sqlite_schema WHERE sql IS NOT NULL`)
	if err != nil {
		return storagef("read schema of %q: %v", path, err)
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			return storagef("read schema of %q: %v", path, err)
		}
		found[typ+" "+name] = ddl
	}
	if err := rows.Err(); err != nil {
		return storagef("read schema of %q: %v", path, err)
	}

	want := map[string]string{}
	for _, object := range sqliteCanonicalSchema {
		want[object.typ+" "+object.name] = object.ddl
	}
	incompatible := func(detail string) error {
		return &harness.IncompatibleSchemaError{
			Found:     version,
			Supported: sqliteSchemaVersion,
			Detail:    fmt.Sprintf("database %q: %s", path, detail),
		}
	}
	for key, ddl := range found {
		canonical, ok := want[key]
		if !ok {
			return incompatible(fmt.Sprintf("unexpected %s", key))
		}
		if canonical != ddl {
			return incompatible(fmt.Sprintf("%s does not match the canonical definition", key))
		}
		delete(want, key)
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for key := range want {
			missing = append(missing, key)
		}
		sort.Strings(missing)
		return incompatible(fmt.Sprintf("missing %s", strings.Join(missing, ", ")))
	}
	return nil
}

// verifySQLitePool verifies the journal mode, synchronous mode, and busy
// timeout the pool reports before OpenSQLite returns.
func verifySQLitePool(db *sql.DB) error {
	for pragma, want := range map[string]string{
		"PRAGMA journal_mode": "wal",
		"PRAGMA synchronous":  "2",
		"PRAGMA busy_timeout": "5000",
	} {
		var got string
		if err := db.QueryRow(pragma).Scan(&got); err != nil {
			return storagef("verify %s: %v", pragma, err)
		}
		if got != want {
			return storagef("verify %s: reported %q, want %q", pragma, got, want)
		}
	}
	return nil
}

// SQLite is the production implementation of the harness storage contract on
// one SQLite database file. Transact holds the instance-local writer mutex
// for its whole callback, which serializes mutations and keeps same-instance
// SQLITE_BUSY from becoming a second conflict policy. Public reads never take
// the mutex: SQLite serves them through separate pooled connections, and WAL
// snapshot isolation makes every outside observation the complete pre-commit
// or complete post-commit state.
type SQLite struct {
	writer sync.Mutex
	db     *sql.DB
	path   string
}

var _ harness.Storage = (*SQLite)(nil)

// Close closes the database pool.
func (s *SQLite) Close() error {
	if err := s.db.Close(); err != nil {
		return storagef("close database %q: %v", s.path, err)
	}
	return nil
}

// readSnapshot runs one public read on a single pooled connection inside one
// deferred read transaction, so every statement of the read observes one
// committed snapshot even while a concurrent writer commits. The snapshot
// only reads and is always rolled back; Transact stays the only mutation
// path. Raw BEGIN/ROLLBACK statements are used because the driver begins
// every database/sql transaction with the pool's immediate write lock.
func (s *SQLite) readSnapshot(ctx context.Context, read func(ctx context.Context, q sqliteQuerier) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return sqliteDriverErr(err, "acquire read connection")
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN DEFERRED`); err != nil {
		return sqliteDriverErr(err, "begin read snapshot")
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	return read(ctx, conn)
}

func (s *SQLite) ReadEntries(ctx context.Context, sessionID string, after int64) ([]harness.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var entries []harness.Entry
	err := s.readSnapshot(ctx, func(ctx context.Context, q sqliteQuerier) error {
		var err error
		entries, err = sqliteReadEntries(ctx, q, sessionID, after)
		return err
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *SQLite) ReadRegister(ctx context.Context, key harness.RegisterKey) (harness.Register, error) {
	if err := ctx.Err(); err != nil {
		return harness.Register{}, err
	}
	var register harness.Register
	err := s.readSnapshot(ctx, func(ctx context.Context, q sqliteQuerier) error {
		var err error
		register, err = sqliteReadRegister(ctx, q, key)
		return err
	})
	if err != nil {
		return harness.Register{}, err
	}
	return register, nil
}

func (s *SQLite) ReadRegisters(ctx context.Context, sessionID string) ([]harness.Register, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var registers []harness.Register
	err := s.readSnapshot(ctx, func(ctx context.Context, q sqliteQuerier) error {
		var err error
		registers, err = sqliteReadRegisters(ctx, q, sessionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return registers, nil
}

func (s *SQLite) ListSessionIDs(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return sqliteListSessionIDs(ctx, s.db)
}

// Transact is the only mutation path. The callback runs under the writer lock
// inside one immediate database transaction; every outcome other than a
// successful commit — callback error, panic, cancellation, latched mutation
// failure — rolls the transaction back without consuming a sequence or
// revision. The commit is the publication boundary and a commit error is a
// storage-service error, not retried.
func (s *SQLite) Transact(ctx context.Context, fn func(harness.Transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sqliteDriverErr(err, "begin transaction")
	}
	txn := &sqliteTxn{ctx: ctx, tx: tx}
	completed := false
	defer func() {
		txn.closed = true
		if !completed {
			// Callback error, latched mutation failure, cancellation, or
			// panic: roll back, then let the panic propagate.
			_ = tx.Rollback()
		}
	}()
	if err := fn(txn); err != nil {
		return err
	}
	if txn.latched != nil {
		return txn.latched
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return sqliteDriverErr(err, "commit transaction")
	}
	completed = true
	return nil
}

// sqliteQuerier is satisfied by both the pool and one transaction, so every
// read and mutation helper runs unchanged on committed state and inside a
// transaction.
type sqliteQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sqliteTxn executes one immediate database transaction. A mutation failure
// of conflict or storage class latches the first such error so an ignored
// conflict or overflow still fails the transaction; every other error is the
// callback's to handle.
type sqliteTxn struct {
	ctx     context.Context
	tx      *sql.Tx
	latched error
	closed  bool
}

var _ harness.Transaction = (*sqliteTxn)(nil)

func (t *sqliteTxn) guard() error {
	if t.closed {
		return invalidf("transaction has expired")
	}
	return nil
}

func (t *sqliteTxn) latch(err error) {
	if t.latched == nil && (errors.Is(err, harness.ErrConflict) || errors.Is(err, harness.ErrStorage)) {
		t.latched = err
	}
}

func (t *sqliteTxn) ReadEntries(sessionID string, after int64) ([]harness.Entry, error) {
	if err := t.guard(); err != nil {
		return nil, err
	}
	return sqliteReadEntries(t.ctx, t.tx, sessionID, after)
}

func (t *sqliteTxn) ReadRegister(key harness.RegisterKey) (harness.Register, error) {
	if err := t.guard(); err != nil {
		return harness.Register{}, err
	}
	return sqliteReadRegister(t.ctx, t.tx, key)
}

func (t *sqliteTxn) InsertEntry(draft harness.EntryDraft) (harness.Entry, error) {
	if err := t.guard(); err != nil {
		return harness.Entry{}, err
	}
	entry, err := sqliteInsertEntry(t.ctx, t.tx, draft)
	if err != nil {
		t.latch(err)
		return harness.Entry{}, err
	}
	return entry, nil
}

func (t *sqliteTxn) InsertRegister(draft harness.RegisterDraft) (harness.Register, error) {
	if err := t.guard(); err != nil {
		return harness.Register{}, err
	}
	register, err := sqliteInsertRegister(t.ctx, t.tx, draft)
	if err != nil {
		t.latch(err)
		return harness.Register{}, err
	}
	return register, nil
}

func (t *sqliteTxn) ReplaceRegister(key harness.RegisterKey, expectedRevision int64, payload json.RawMessage) (harness.Register, error) {
	if err := t.guard(); err != nil {
		return harness.Register{}, err
	}
	register, err := sqliteReplaceRegister(t.ctx, t.tx, key, expectedRevision, payload)
	if err != nil {
		t.latch(err)
		return harness.Register{}, err
	}
	return register, nil
}

func (t *sqliteTxn) DeleteSession(sessionID string) error {
	if err := t.guard(); err != nil {
		return err
	}
	return sqliteDeleteSession(t.ctx, t.tx, sessionID)
}

// sqliteDriverErr passes context errors through untouched and wraps every
// other driver failure as ErrStorage: physical database corruption and
// connection failures carry no trustworthy Session isolation.
func sqliteDriverErr(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return storagef("%s: %v", fmt.Sprintf(format, args...), err)
}

// sqliteHasSessionRegister reports whether the canonical session register —
// the Session-register key with an empty operation identity — exists.
func sqliteHasSessionRegister(ctx context.Context, q sqliteQuerier, sessionID string) (bool, error) {
	var exists int
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registers
		WHERE session_id = ? AND kind = ? AND operation_id = '')`,
		sessionID, harness.RegisterSession).Scan(&exists)
	if err != nil {
		return false, sqliteDriverErr(err, "read session register of session %q", sessionID)
	}
	return exists != 0, nil
}

// sqliteSessionOrphaned reports whether any entry or register envelope exists
// for one session whose canonical register is absent. Such state is
// corruption, never an empty or absent session.
func sqliteSessionOrphaned(ctx context.Context, q sqliteQuerier, sessionID string) (bool, error) {
	var orphaned int
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM entries WHERE session_id = ?)
		OR EXISTS(SELECT 1 FROM registers WHERE session_id = ?)`,
		sessionID, sessionID).Scan(&orphaned)
	if err != nil {
		return false, sqliteDriverErr(err, "read dependents of session %q", sessionID)
	}
	return orphaned != 0, nil
}

func sqliteReadEntries(ctx context.Context, q sqliteQuerier, sessionID string, after int64) ([]harness.Entry, error) {
	if sessionID == "" {
		return nil, invalidf("session identity is empty")
	}
	if after < 0 {
		return nil, invalidf("after %d is negative", after)
	}
	hasSession, err := sqliteHasSessionRegister(ctx, q, sessionID)
	if err != nil {
		return nil, err
	}
	if !hasSession {
		orphaned, err := sqliteSessionOrphaned(ctx, q, sessionID)
		if err != nil {
			return nil, err
		}
		if orphaned {
			return nil, orphanCorruption(sessionID)
		}
		return nil, notfoundf("session %q has no envelopes", sessionID)
	}
	rows, err := q.QueryContext(ctx, `SELECT entry_id, sequence, operation_id, kind, committed_at_ns, payload
		FROM entries WHERE session_id = ? AND sequence > ? ORDER BY sequence`, sessionID, after)
	if err != nil {
		return nil, sqliteDriverErr(err, "read entries of session %q", sessionID)
	}
	defer rows.Close()
	entries := []harness.Entry{}
	for rows.Next() {
		var (
			entry         harness.Entry
			kind          string
			committedAtNS int64
			payload       string
		)
		if err := rows.Scan(&entry.ID, &entry.Sequence, &entry.OperationID, &kind, &committedAtNS, &payload); err != nil {
			return nil, sqliteDriverErr(err, "read entries of session %q", sessionID)
		}
		entry.SessionID = sessionID
		entry.Kind = harness.EntryKind(kind)
		if committedAtNS <= 0 {
			return nil, &harness.CorruptionError{SessionID: sessionID, Detail: fmt.Sprintf("stored entry %q commit time %d is not positive", entry.ID, committedAtNS)}
		}
		entry.CommittedAt = time.Unix(0, committedAtNS).UTC()
		entry.Payload = json.RawMessage(payload)
		if err := validateStoredEntry(entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, sqliteDriverErr(err, "read entries of session %q", sessionID)
	}
	return entries, nil
}

func sqliteReadRegister(ctx context.Context, q sqliteQuerier, key harness.RegisterKey) (harness.Register, error) {
	if err := validateRegisterKey(key); err != nil {
		return harness.Register{}, err
	}
	hasSession, err := sqliteHasSessionRegister(ctx, q, key.SessionID)
	if err != nil {
		return harness.Register{}, err
	}
	if !hasSession {
		orphaned, err := sqliteSessionOrphaned(ctx, q, key.SessionID)
		if err != nil {
			return harness.Register{}, err
		}
		if orphaned {
			return harness.Register{}, orphanCorruption(key.SessionID)
		}
		return harness.Register{}, notfoundf("register %v does not exist", key)
	}
	var (
		register harness.Register
		payload  string
	)
	register.Key = key
	err = q.QueryRowContext(ctx, `SELECT revision, payload FROM registers
		WHERE session_id = ? AND kind = ? AND operation_id = ?`,
		key.SessionID, key.Kind, key.OperationID).Scan(&register.Revision, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return harness.Register{}, notfoundf("register %v does not exist", key)
	}
	if err != nil {
		return harness.Register{}, sqliteDriverErr(err, "read register %v", key)
	}
	register.Payload = json.RawMessage(payload)
	if err := validateStoredRegister(register); err != nil {
		return harness.Register{}, err
	}
	return register, nil
}

func sqliteReadRegisters(ctx context.Context, q sqliteQuerier, sessionID string) ([]harness.Register, error) {
	if sessionID == "" {
		return nil, invalidf("session identity is empty")
	}
	sessionRegister, err := sqliteReadRegister(ctx, q, harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT operation_id, revision, payload FROM registers
		WHERE session_id = ? AND kind = ? ORDER BY operation_id`, sessionID, harness.RegisterOperation)
	if err != nil {
		return nil, sqliteDriverErr(err, "read operation registers of session %q", sessionID)
	}
	defer rows.Close()
	registers := []harness.Register{sessionRegister}
	var registered int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM registers WHERE session_id = ?`, sessionID).Scan(&registered); err != nil {
		return nil, sqliteDriverErr(err, "read operation registers of session %q", sessionID)
	}
	for rows.Next() {
		var (
			register harness.Register
			payload  string
		)
		register.Key = harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterOperation}
		if err := rows.Scan(&register.Key.OperationID, &register.Revision, &payload); err != nil {
			return nil, sqliteDriverErr(err, "read operation registers of session %q", sessionID)
		}
		register.Payload = json.RawMessage(payload)
		if err := validateStoredRegister(register); err != nil {
			return nil, err
		}
		registers = append(registers, register)
	}
	if err := rows.Err(); err != nil {
		return nil, sqliteDriverErr(err, "read operation registers of session %q", sessionID)
	}
	if registered != len(registers) {
		// A register envelope of the wrong kind for its identity is malformed
		// persisted state: ReadRegisters surfaces it instead of silently
		// skipping it.
		return nil, &harness.CorruptionError{SessionID: sessionID, Detail: "stored session register carries an operation identity"}
	}
	return registers, nil
}

func sqliteListSessionIDs(ctx context.Context, q sqliteQuerier) ([]string, error) {
	// The identity-only scan keeps orphaned and corrupt sessions discoverable;
	// only physical failure of the enumeration is a storage error.
	rows, err := q.QueryContext(ctx, `SELECT session_id FROM entries
		UNION SELECT session_id FROM registers ORDER BY session_id`)
	if err != nil {
		return nil, sqliteDriverErr(err, "enumerate session identities")
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, sqliteDriverErr(err, "enumerate session identities")
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, sqliteDriverErr(err, "enumerate session identities")
	}
	return ids, nil
}

func sqliteInsertEntry(ctx context.Context, q sqliteQuerier, draft harness.EntryDraft) (harness.Entry, error) {
	if err := validateEntryDraft(draft); err != nil {
		return harness.Entry{}, err
	}
	hasSession, err := sqliteHasSessionRegister(ctx, q, draft.SessionID)
	if err != nil {
		return harness.Entry{}, err
	}
	if !hasSession {
		orphaned, err := sqliteSessionOrphaned(ctx, q, draft.SessionID)
		if err != nil {
			return harness.Entry{}, err
		}
		if orphaned {
			return harness.Entry{}, orphanCorruption(draft.SessionID)
		}
		return harness.Entry{}, notfoundf("session %q does not exist", draft.SessionID)
	}
	var duplicate int
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM entries WHERE entry_id = ?)`, draft.ID).Scan(&duplicate); err != nil {
		return harness.Entry{}, sqliteDriverErr(err, "read entry identity %q", draft.ID)
	}
	if duplicate != 0 {
		return harness.Entry{}, conflictf("entry id %q already exists", draft.ID)
	}
	// Sequence assignment uses the committed per-Session maximum inside the
	// immediate transaction, so a rollback leaves no committed gap and no
	// sequence side table is needed.
	var highest sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MAX(sequence) FROM entries WHERE session_id = ?`, draft.SessionID).Scan(&highest); err != nil {
		return harness.Entry{}, sqliteDriverErr(err, "read committed sequence of session %q", draft.SessionID)
	}
	if highest.Int64 == math.MaxInt64 {
		return harness.Entry{}, storagef("session %q entry sequence would overflow", draft.SessionID)
	}
	entry := harness.Entry{
		SessionID:   draft.SessionID,
		ID:          draft.ID,
		Sequence:    highest.Int64 + 1,
		OperationID: draft.OperationID,
		Kind:        draft.Kind,
		CommittedAt: time.Now().UTC(),
		Payload:     clonePayload(draft.Payload),
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.SessionID, entry.Sequence, entry.ID, entry.OperationID, entry.Kind, entry.CommittedAt.UnixNano(), string(entry.Payload)); err != nil {
		return harness.Entry{}, sqliteDriverErr(err, "insert entry %q", draft.ID)
	}
	return entry, nil
}

func sqliteInsertRegister(ctx context.Context, q sqliteQuerier, draft harness.RegisterDraft) (harness.Register, error) {
	if err := validateRegisterKey(draft.Key); err != nil {
		return harness.Register{}, err
	}
	if !json.Valid(draft.Payload) {
		return harness.Register{}, invalidf("register payload is not valid JSON")
	}
	hasSession, err := sqliteHasSessionRegister(ctx, q, draft.Key.SessionID)
	if err != nil {
		return harness.Register{}, err
	}
	switch draft.Key.Kind {
	case harness.RegisterOperation:
		if !hasSession {
			orphaned, err := sqliteSessionOrphaned(ctx, q, draft.Key.SessionID)
			if err != nil {
				return harness.Register{}, err
			}
			if orphaned {
				return harness.Register{}, orphanCorruption(draft.Key.SessionID)
			}
			return harness.Register{}, notfoundf("session %q does not exist", draft.Key.SessionID)
		}
		var duplicate int
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registers WHERE kind = ? AND operation_id = ?)`,
			harness.RegisterOperation, draft.Key.OperationID).Scan(&duplicate); err != nil {
			return harness.Register{}, sqliteDriverErr(err, "read operation identity %q", draft.Key.OperationID)
		}
		if duplicate != 0 {
			return harness.Register{}, conflictf("operation %q already exists", draft.Key.OperationID)
		}
	case harness.RegisterSession:
		if hasSession {
			return harness.Register{}, conflictf("register %v already exists", draft.Key)
		}
		orphaned, err := sqliteSessionOrphaned(ctx, q, draft.Key.SessionID)
		if err != nil {
			return harness.Register{}, err
		}
		if orphaned {
			// Inserting the missing session register must not repair or hide
			// persisted dependents that outlived their register.
			return harness.Register{}, orphanCorruption(draft.Key.SessionID)
		}
	}
	register := harness.Register{Key: draft.Key, Revision: 1, Payload: clonePayload(draft.Payload)}
	if _, err := q.ExecContext(ctx, `INSERT INTO registers (session_id, kind, operation_id, revision, payload)
		VALUES (?, ?, ?, 1, ?)`,
		register.Key.SessionID, register.Key.Kind, register.Key.OperationID, string(register.Payload)); err != nil {
		return harness.Register{}, sqliteDriverErr(err, "insert register %v", draft.Key)
	}
	return register, nil
}

func sqliteReplaceRegister(ctx context.Context, q sqliteQuerier, key harness.RegisterKey, expectedRevision int64, payload json.RawMessage) (harness.Register, error) {
	if err := validateRegisterKey(key); err != nil {
		return harness.Register{}, err
	}
	if expectedRevision <= 0 {
		return harness.Register{}, invalidf("expected revision %d is not positive", expectedRevision)
	}
	if !json.Valid(payload) {
		return harness.Register{}, invalidf("register payload is not valid JSON")
	}
	var (
		currentRevision int64
		currentPayload  string
	)
	err := q.QueryRowContext(ctx, `SELECT revision, payload FROM registers
		WHERE session_id = ? AND kind = ? AND operation_id = ?`,
		key.SessionID, key.Kind, key.OperationID).Scan(&currentRevision, &currentPayload)
	if errors.Is(err, sql.ErrNoRows) {
		orphaned, err := sqliteSessionOrphaned(ctx, q, key.SessionID)
		if err != nil {
			return harness.Register{}, err
		}
		if orphaned {
			return harness.Register{}, orphanCorruption(key.SessionID)
		}
		return harness.Register{}, notfoundf("register %v does not exist", key)
	}
	if err != nil {
		return harness.Register{}, sqliteDriverErr(err, "read register %v", key)
	}
	if hasSession, err := sqliteHasSessionRegister(ctx, q, key.SessionID); err != nil {
		return harness.Register{}, err
	} else if !hasSession {
		// An extant register without its session register is an orphan:
		// replacing it must not update dependent state.
		return harness.Register{}, orphanCorruption(key.SessionID)
	}
	if currentRevision != expectedRevision {
		return harness.Register{}, conflictf("register %v revision %d does not match expected %d", key, currentRevision, expectedRevision)
	}
	if currentRevision == math.MaxInt64 {
		return harness.Register{}, storagef("register %v revision would overflow", key)
	}
	// One conditional update on key and expected revision replaces the whole
	// payload; zero affected rows would be a stale revision, which cannot
	// occur under the writer lock and stays a conflict.
	result, err := q.ExecContext(ctx, `UPDATE registers SET revision = ?, payload = ?
		WHERE session_id = ? AND kind = ? AND operation_id = ? AND revision = ?`,
		expectedRevision+1, string(payload), key.SessionID, key.Kind, key.OperationID, expectedRevision)
	if err != nil {
		return harness.Register{}, sqliteDriverErr(err, "replace register %v", key)
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return harness.Register{}, conflictf("register %v revision %d does not match expected %d", key, currentRevision, expectedRevision)
	}
	return harness.Register{Key: key, Revision: expectedRevision + 1, Payload: clonePayload(payload)}, nil
}

func sqliteDeleteSession(ctx context.Context, q sqliteQuerier, sessionID string) error {
	if sessionID == "" {
		return invalidf("session identity is empty")
	}
	hasSession, err := sqliteHasSessionRegister(ctx, q, sessionID)
	if err != nil {
		return err
	}
	if !hasSession {
		orphaned, err := sqliteSessionOrphaned(ctx, q, sessionID)
		if err != nil {
			return err
		}
		if orphaned {
			return orphanCorruption(sessionID)
		}
		return notfoundf("session %q does not exist", sessionID)
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM entries WHERE session_id = ?`, sessionID); err != nil {
		return sqliteDriverErr(err, "delete entries of session %q", sessionID)
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM registers WHERE session_id = ?`, sessionID); err != nil {
		return sqliteDriverErr(err, "delete registers of session %q", sessionID)
	}
	return nil
}
