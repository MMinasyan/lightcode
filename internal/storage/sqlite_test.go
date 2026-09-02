package storage

// SQLite wiring for the shared storage conformance suite plus the
// SQLite-specific schema, open, persistence, physical-shape, and connection
// tests. Raw-SQL fixtures mutate the persisted state directly so no
// production test API is added; shared rollback, ordering, revision,
// deletion, and corruption semantics are tested only by the shared suite,
// whose reopen hook closes and durably reopens this store.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"

	"github.com/MMinasyan/lightcode/harness"
)

// TestSQLiteConformance runs the shared storage conformance suite against the
// SQLite implementation.
func TestSQLiteConformance(t *testing.T) {
	runConformance(t, sqliteBackend())
}

// newSQLiteStore opens one store on a fresh temporary file and closes it at
// test cleanup.
func newSQLiteStore(t *testing.T) *SQLite {
	t.Helper()
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "lightcode.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// sqliteBackend wires the SQLite implementation into the shared suite. Its
// reopen hook closes the pool and reopens the same file, so the suite's
// corruption and orphan cases also cover persistence across reopen. The
// fixtures execute raw SQL on one dedicated pool connection: connection-scoped
// PRAGMA state stays there and is restored before the connection returns.
func sqliteBackend() conformanceBackend {
	return conformanceBackend{
		name:     "sqlite",
		newStore: func(t *testing.T) harness.Storage { return newSQLiteStore(t) },
		reopen: func(t *testing.T, store harness.Storage) harness.Storage {
			s := store.(*SQLite)
			if err := s.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			reopened, err := OpenSQLite(s.path)
			if err != nil {
				t.Fatalf("reopen %q: %v", s.path, err)
			}
			t.Cleanup(func() { reopened.Close() })
			return reopened
		},
		corruptEntry: func(t *testing.T, store harness.Storage, sessionID string) {
			s := store.(*SQLite)
			conn := sqliteFixtureConn(t, s)
			defer conn.Close()
			var highest sql.NullInt64
			if err := conn.QueryRowContext(context.Background(), `SELECT MAX(sequence) FROM entries WHERE session_id = ?`, sessionID).Scan(&highest); err != nil {
				t.Fatalf("read highest sequence: %v", err)
			}
			sqliteRawExec(t, conn, `PRAGMA ignore_check_constraints = ON`)
			defer sqliteRawExec(t, conn, `PRAGMA ignore_check_constraints = OFF`)
			// Valid in every envelope field except the zero commit time.
			sqliteRawExec(t, conn, `INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload)
				VALUES (?, ?, 'corrupt-entry', '', ?, 0, '{"entry":"corrupt"}')`,
				sessionID, highest.Int64+1, harness.EntryInput)
		},
		corruptRegister: func(t *testing.T, store harness.Storage, sessionID string) {
			s := store.(*SQLite)
			conn := sqliteFixtureConn(t, s)
			defer conn.Close()
			sqliteRawExec(t, conn, `PRAGMA ignore_check_constraints = ON`)
			defer sqliteRawExec(t, conn, `PRAGMA ignore_check_constraints = OFF`)
			sqliteRawExec(t, conn, `UPDATE registers SET payload = '{"session":' WHERE session_id = ? AND kind = ?`,
				sessionID, harness.RegisterSession)
		},
		orphanEntry: func(t *testing.T, store harness.Storage, sessionID string) {
			s := store.(*SQLite)
			conn := sqliteFixtureConn(t, s)
			defer conn.Close()
			sqliteRawExec(t, conn, `INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload)
				VALUES (?, 1, 'orphan-entry', '', ?, ?, '{"entry":"orphan"}')`,
				sessionID, harness.EntryInput, fixtureTime.UnixNano())
		},
		orphanOperationRegister: func(t *testing.T, store harness.Storage, sessionID string) {
			s := store.(*SQLite)
			conn := sqliteFixtureConn(t, s)
			defer conn.Close()
			sqliteRawExec(t, conn, `INSERT INTO registers (session_id, kind, operation_id, revision, payload)
				VALUES (?, ?, 'orphan-op', 1, '{"operation":"orphan-op"}')`,
				sessionID, harness.RegisterOperation)
		},
		malformedSessionRegister: func(t *testing.T, store harness.Storage, sessionID string) {
			s := store.(*SQLite)
			conn := sqliteFixtureConn(t, s)
			defer conn.Close()
			// A session-register envelope carrying an operation identity is
			// not a valid parent for any dependent of the session; the
			// identity rule is bypassed like every raw-SQL fixture.
			sqliteRawExec(t, conn, `PRAGMA ignore_check_constraints = ON`)
			defer sqliteRawExec(t, conn, `PRAGMA ignore_check_constraints = OFF`)
			sqliteRawExec(t, conn, `INSERT INTO registers (session_id, kind, operation_id, revision, payload)
				VALUES (?, ?, 'forged', 1, '{"session":"forged"}')`,
				sessionID, harness.RegisterSession)
		},
		exhaustSequence: func(t *testing.T, store harness.Storage, sessionID string) {
			s := store.(*SQLite)
			conn := sqliteFixtureConn(t, s)
			defer conn.Close()
			sqliteRawExec(t, conn, `INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload)
				VALUES (?, ?, 'max-sequence-entry', '', ?, ?, '{"entry":"max"}')`,
				sessionID, int64(math.MaxInt64), harness.EntryInput, fixtureTime.UnixNano())
		},
		exhaustRevision: func(t *testing.T, store harness.Storage, sessionID string) {
			s := store.(*SQLite)
			conn := sqliteFixtureConn(t, s)
			defer conn.Close()
			sqliteRawExec(t, conn, `UPDATE registers SET revision = ? WHERE session_id = ? AND kind = ? AND operation_id = ''`,
				int64(math.MaxInt64), sessionID, harness.RegisterSession)
		},
	}
}

// sqliteFixtureConn opens one dedicated connection for raw-SQL fixtures.
func sqliteFixtureConn(t *testing.T, store *SQLite) *sql.Conn {
	t.Helper()
	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("fixture connection: %v", err)
	}
	return conn
}

func sqliteRawExec(t *testing.T, conn *sql.Conn, query string, args ...any) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// sqliteFileDigest returns the hex sha256 of one file's bytes.
func sqliteFileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// sqliteAssertNoSidecars proves no journal, WAL, or shared-memory sidecar
// exists beside the database file.
func sqliteAssertNoSidecars(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("sidecar %q exists: %v", path+suffix, err)
		}
	}
}

// sqliteFixtureDatabase builds a database file at path from raw DDL with the
// given schema version, using a plain connection that leaves the file in
// rollback-journal mode with no sidecars.
func sqliteFixtureDatabase(t *testing.T, path string, ddl []string, version int) {
	t.Helper()
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	defer db.Close()
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}

// TestSQLiteSchemaInitialization proves a missing path is initialized with the
// complete canonical schema in one shot: version 1, exactly the two canonical
// tables and the one explicit index, only the autoindexes implied by those
// tables, a well-formed image, no sidecars after close, and no partial file
// when initialization fails.
func TestSQLiteSchemaInitialization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lightcode.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	ctx := context.Background()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatalf("connection: %v", err)
	}

	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("PRAGMA user_version = %d (error %v), want 1", version, err)
	}

	objects := map[string]string{}
	rows, err := conn.QueryContext(ctx, `SELECT type, name, sql FROM sqlite_schema WHERE sql IS NOT NULL`)
	if err != nil {
		t.Fatalf("read schema objects: %v", err)
	}
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			t.Fatalf("read schema objects: %v", err)
		}
		objects[typ+" "+name] = ddl
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read schema objects: %v", err)
	}
	rows.Close()
	want := map[string]string{
		"table entries":               sqliteEntriesDDL,
		"table registers":             sqliteRegistersDDL,
		"index operation_register_id": sqliteOperationIndexDDL,
	}
	if len(objects) != len(want) {
		t.Errorf("user-defined schema objects = %v, want exactly %v", objects, want)
	}
	for key, ddl := range want {
		if objects[key] != ddl {
			t.Errorf("schema object %q = %q, want the canonical definition", key, ddl)
		}
	}

	autoindexes := map[string]bool{}
	rows, err = conn.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'index' AND sql IS NULL`)
	if err != nil {
		t.Fatalf("read autoindexes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("read autoindexes: %v", err)
		}
		autoindexes[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read autoindexes: %v", err)
	}
	wantAutoindexes := map[string]bool{
		"sqlite_autoindex_entries_1":   true,
		"sqlite_autoindex_entries_2":   true,
		"sqlite_autoindex_registers_1": true,
	}
	if len(autoindexes) != len(wantAutoindexes) {
		t.Errorf("autoindexes = %v, want exactly %v", autoindexes, wantAutoindexes)
	}
	for name := range wantAutoindexes {
		if !autoindexes[name] {
			t.Errorf("implied autoindex %q missing", name)
		}
	}

	var integrity string
	if err := conn.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Errorf("PRAGMA integrity_check = %q (error %v), want ok", integrity, err)
	}
	conn.Close()

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	sqliteAssertNoSidecars(t, path)

	// An existing zero-length file is initialized exactly like a missing one.
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("create zero-length file: %v", err)
	}
	emptyStore, err := OpenSQLite(empty)
	if err != nil {
		t.Fatalf("OpenSQLite zero-length file: %v", err)
	}
	var emptyVersion int
	if err := emptyStore.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&emptyVersion); err != nil || emptyVersion != 1 {
		t.Errorf("zero-length file user_version = %d (error %v), want 1", emptyVersion, err)
	}
	if err := emptyStore.Close(); err != nil {
		t.Fatalf("close zero-length store: %v", err)
	}
	sqliteAssertNoSidecars(t, empty)

	// A caller path holding reserved URI characters initializes exactly that
	// file, never a truncated sibling database.
	reserved := filepath.Join(dir, "weird?name#part.db")
	reservedStore, err := OpenSQLite(reserved)
	if err != nil {
		t.Fatalf("OpenSQLite reserved URI path: %v", err)
	}
	var reservedVersion int
	if err := reservedStore.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&reservedVersion); err != nil || reservedVersion != 1 {
		t.Errorf("reserved-path user_version = %d (error %v), want 1", reservedVersion, err)
	}
	if err := reservedStore.Close(); err != nil {
		t.Fatalf("close reserved-path store: %v", err)
	}
	if _, err := os.Stat(reserved); err != nil {
		t.Fatalf("reserved-path database %q missing: %v", reserved, err)
	}

	// The relative literal filename `:memory:` is a caller-supplied
	// filesystem path, never a SQLite special DSN: it resolves against the
	// working directory and opens a durable file, not an ephemeral in-memory
	// database.
	t.Chdir(dir)
	memoryStore, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite :memory: path: %v", err)
	}
	confCreateSession(t, ctx, memoryStore, "s1")
	confTxn(t, ctx, memoryStore, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("s1", "e1"))
		return err
	})
	if _, err := os.Stat(filepath.Join(dir, ":memory:")); err != nil {
		t.Fatalf("durable :memory: file missing: %v", err)
	}
	if err := memoryStore.Close(); err != nil {
		t.Fatalf("close :memory: store: %v", err)
	}
	reopenedMemory, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("reopen :memory: path: %v", err)
	}
	if entries, err := reopenedMemory.ReadEntries(ctx, "s1", 0); err != nil || len(entries) != 1 || entries[0].ID != "e1" {
		t.Errorf("durable :memory: entries = %v (error %v), want [e1]", entries, err)
	}
	if err := reopenedMemory.Close(); err != nil {
		t.Fatalf("close reopened :memory: store: %v", err)
	}

	// Every initialized file is the requested one; no sibling or truncated
	// database was created beside them.
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	initialized := map[string]bool{}
	for _, dirEntry := range dirEntries {
		initialized[dirEntry.Name()] = true
	}
	wantFiles := map[string]bool{
		"lightcode.db":       true,
		"empty.db":           true,
		"weird?name#part.db": true,
		":memory:":           true,
	}
	for name := range wantFiles {
		if !initialized[name] {
			t.Errorf("initialized file %q missing", name)
		}
	}
	for name := range initialized {
		if !wantFiles[name] {
			t.Errorf("unexpected sibling or truncated database %q was initialized", name)
		}
	}

	// A failed initialization must not leave a partial database behind.
	blocked := filepath.Join(dir, "missing", "lightcode.db")
	if _, err := OpenSQLite(blocked); err == nil {
		t.Fatal("OpenSQLite into a missing directory succeeded")
	}
	if _, err := os.Stat(blocked); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("failed initialization left %q behind: %v", blocked, err)
	}
}

// TestSQLitePhysicalShapes proves the two generic tables physically enforce
// the envelope shape: closed kinds, positive sequence and revision, non-empty
// identities, the session/operation register identity rule, and syntactically
// valid JSON — with no additional user-defined objects of any kind.
func TestSQLitePhysicalShapes(t *testing.T) {
	store := newSQLiteStore(t)
	ctx := context.Background()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	defer conn.Close()

	rejected := []struct {
		name  string
		query string
		args  []any
	}{
		{"unknown entry kind", `INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload) VALUES ('s', 1, 'e', '', 'bogus', 1, '{}')`, nil},
		{"empty entry session id", `INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload) VALUES ('', 1, 'e', '', 'input', 1, '{}')`, nil},
		{"zero sequence", `INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload) VALUES ('s', 0, 'e', '', 'input', 1, '{}')`, nil},
		{"empty entry id", `INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload) VALUES ('s', 1, '', '', 'input', 1, '{}')`, nil},
		{"zero commit time", `INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload) VALUES ('s', 1, 'e', '', 'input', 0, '{}')`, nil},
		{"malformed entry payload", `INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload) VALUES ('s', 1, 'e', '', 'input', 1, '{')`, nil},
		{"unknown register kind", `INSERT INTO registers (session_id, kind, operation_id, revision, payload) VALUES ('s', 'bogus', '', 1, '{}')`, nil},
		{"empty register session id", `INSERT INTO registers (session_id, kind, operation_id, revision, payload) VALUES ('', 'session', '', 1, '{}')`, nil},
		{"zero revision", `INSERT INTO registers (session_id, kind, operation_id, revision, payload) VALUES ('s', 'session', '', 0, '{}')`, nil},
		{"session register with operation identity", `INSERT INTO registers (session_id, kind, operation_id, revision, payload) VALUES ('s', 'session', 'op', 1, '{}')`, nil},
		{"operation register without operation identity", `INSERT INTO registers (session_id, kind, operation_id, revision, payload) VALUES ('s', 'operation', '', 1, '{}')`, nil},
		{"malformed register payload", `INSERT INTO registers (session_id, kind, operation_id, revision, payload) VALUES ('s', 'session', '', 1, '{')`, nil},
	}
	for _, tc := range rejected {
		if _, err := conn.ExecContext(ctx, tc.query, tc.args...); err == nil {
			t.Errorf("%s: physical constraint not enforced", tc.name)
		}
	}

	var userObjects int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE sql IS NOT NULL`).Scan(&userObjects); err != nil {
		t.Fatalf("count user-defined objects: %v", err)
	}
	if userObjects != 3 {
		t.Errorf("user-defined object count = %d, want 3 (two tables, one index)", userObjects)
	}
}

// TestSQLiteRejectsNonEmptyVersionZero proves a non-empty version-zero
// database is rejected as incompatible without any file, journal-mode, or
// sidecar change.
func TestSQLiteRejectsNonEmptyVersionZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	sqliteFixtureDatabase(t, path, []string{`CREATE TABLE legacy (id INTEGER PRIMARY KEY)`}, 0)
	before := sqliteFileDigest(t, path)

	store, err := OpenSQLite(path)
	if !errors.Is(err, harness.ErrIncompatible) {
		t.Fatalf("error = %v, want ErrIncompatible", err)
	}
	var incompatible *harness.IncompatibleSchemaError
	if !errors.As(err, &incompatible) {
		t.Fatalf("error = %T, want *harness.IncompatibleSchemaError", err)
	}
	if incompatible.Found != 0 || incompatible.Supported != 1 {
		t.Errorf("recovered %+v, want found 0 supported 1", incompatible)
	}
	if store != nil {
		t.Error("rejected database returned a store")
	}
	if after := sqliteFileDigest(t, path); after != before {
		t.Error("rejected version-zero database file changed")
	}
	sqliteAssertNoSidecars(t, path)
}

// TestSQLiteRejectsUnsupportedVersion proves a database with a schema version
// beyond 1 is rejected without any file or sidecar change.
func TestSQLiteRejectsUnsupportedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	sqliteFixtureDatabase(t, path, []string{sqliteEntriesDDL, sqliteRegistersDDL, sqliteOperationIndexDDL}, 2)
	before := sqliteFileDigest(t, path)

	_, err := OpenSQLite(path)
	if !errors.Is(err, harness.ErrIncompatible) {
		t.Fatalf("error = %v, want ErrIncompatible", err)
	}
	var incompatible *harness.IncompatibleSchemaError
	if !errors.As(err, &incompatible) || incompatible.Found != 2 || incompatible.Supported != 1 {
		t.Errorf("recovered %+v, want found 2 supported 1", incompatible)
	}
	if after := sqliteFileDigest(t, path); after != before {
		t.Error("rejected version-2 database file changed")
	}
	sqliteAssertNoSidecars(t, path)

	// Unsupported hot-WAL sibling: version 2 lives only in an uncheckpointed
	// WAL beside a checkpointed version 1 main file, with no SHM file — the
	// crashed-writer state. Read-only validation must see the WAL-carried
	// version and reject the copy while leaving the database and WAL bytes
	// unchanged; a SQLite-owned -shm WAL index may appear and remain.
	source := filepath.Join(t.TempDir(), "wal-source.db")
	live, err := sql.Open(sqliteDriverName, sqliteDSN(source, "_journal_mode=WAL&_synchronous=FULL"))
	if err != nil {
		t.Fatalf("open WAL source: %v", err)
	}
	if _, err := live.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("checkpointed version: %v", err)
	}
	if _, err := live.Exec(`PRAGMA wal_checkpoint(FULL)`); err != nil {
		t.Fatalf("checkpoint version 1: %v", err)
	}
	if _, err := live.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("hot version: %v", err)
	}
	mainBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("copy source main file: %v", err)
	}
	walBytes, err := os.ReadFile(source + "-wal")
	if err != nil {
		t.Fatalf("copy source WAL: %v", err)
	}
	target := filepath.Join(t.TempDir(), "wal-crashed.db")
	if err := os.WriteFile(target, mainBytes, 0o644); err != nil {
		t.Fatalf("materialize target main file: %v", err)
	}
	if err := os.WriteFile(target+"-wal", walBytes, 0o644); err != nil {
		t.Fatalf("materialize target WAL: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("close WAL source: %v", err)
	}
	beforeMain := sqliteFileDigest(t, target)
	beforeWAL := sqliteFileDigest(t, target+"-wal")

	crashedStore, err := OpenSQLite(target)
	if !errors.Is(err, harness.ErrIncompatible) {
		t.Fatalf("error = %v, want ErrIncompatible", err)
	}
	var walIncompatible *harness.IncompatibleSchemaError
	if !errors.As(err, &walIncompatible) || walIncompatible.Found != 2 || walIncompatible.Supported != 1 {
		t.Errorf("recovered %+v, want the WAL-carried version found 2 supported 1", walIncompatible)
	}
	if crashedStore != nil {
		t.Error("rejected WAL-mode database returned a store")
	}
	if after := sqliteFileDigest(t, target); after != beforeMain {
		t.Error("rejected WAL-mode database file changed")
	}
	if after := sqliteFileDigest(t, target+"-wal"); after != beforeWAL {
		t.Error("rejected WAL-mode database WAL changed")
	}
	if _, err := os.Stat(target + "-journal"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("rejected WAL-mode database created a journal sidecar")
	}
}

// TestSQLiteRejectsMissingChangedAndExtraSchemaObjects proves the read-only
// open phase accepts only the exact canonical object set: every user-defined
// table and index must be present with its canonical definition, and any
// additional table, index, view, or trigger is incompatible.
func TestSQLiteRejectsMissingChangedAndExtraSchemaObjects(t *testing.T) {
	changedEntriesDDL := strings.Replace(sqliteEntriesDDL, ") STRICT", ")", 1)
	changedRegistersDDL := strings.Replace(sqliteRegistersDDL, ") STRICT", ")", 1)
	changedIndexDDL := `CREATE INDEX operation_register_id
ON registers(operation_id) WHERE kind = 'operation'`
	canonical := []string{sqliteEntriesDDL, sqliteRegistersDDL, sqliteOperationIndexDDL}

	for name, ddl := range map[string][]string{
		"missing entries table":     {sqliteRegistersDDL, sqliteOperationIndexDDL},
		"missing registers table":   {sqliteEntriesDDL},
		"missing operation index":   {sqliteEntriesDDL, sqliteRegistersDDL},
		"changed entries table":     {changedEntriesDDL, sqliteRegistersDDL, sqliteOperationIndexDDL},
		"changed registers table":   {sqliteEntriesDDL, changedRegistersDDL, sqliteOperationIndexDDL},
		"changed operation index":   {sqliteEntriesDDL, sqliteRegistersDDL, changedIndexDDL},
		"additional table":          append(append([]string{}, canonical...), `CREATE TABLE extra (x TEXT)`),
		"additional index":          append(append([]string{}, canonical...), `CREATE INDEX extra_index ON entries(operation_id)`),
		"additional view":           append(append([]string{}, canonical...), `CREATE VIEW extra_view AS SELECT 1`),
		"additional trigger":        append(append([]string{}, canonical...), `CREATE TRIGGER extra_trigger AFTER INSERT ON entries BEGIN SELECT 1; END`),
		"missing and extra objects": {sqliteEntriesDDL, sqliteRegistersDDL, sqliteOperationIndexDDL, `CREATE TABLE extra (x TEXT)`},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.db")
			sqliteFixtureDatabase(t, path, ddl, 1)
			store, err := OpenSQLite(path)
			if !errors.Is(err, harness.ErrIncompatible) {
				t.Fatalf("error = %v, want ErrIncompatible", err)
			}
			var incompatible *harness.IncompatibleSchemaError
			if !errors.As(err, &incompatible) {
				t.Fatalf("error = %T, want *harness.IncompatibleSchemaError", err)
			}
			if incompatible.Found != 1 || incompatible.Supported != 1 {
				t.Errorf("recovered %+v, want found 1 supported 1", incompatible)
			}
			if store != nil {
				t.Error("rejected database returned a store")
			}
		})
	}
}

// TestSQLitePersistenceAcrossCloseReopen proves exact-version reopen: the
// pool closes and the same file reopens with every committed envelope, its
// sequences, revisions, and commit times intact.
func TestSQLitePersistenceAcrossCloseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lightcode.db")
	ctx := context.Background()

	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	confCreateSession(t, ctx, store, "s1")
	var first harness.Entry
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		if _, err := txn.InsertRegister(confOpRegister("s1", "op1")); err != nil {
			return err
		}
		if _, err := txn.ReplaceRegister(confSessionKey("s1"), 1, rawJSON(`{"state":"replaced"}`)); err != nil {
			return err
		}
		var err error
		first, err = txn.InsertEntry(harness.EntryDraft{SessionID: "s1", ID: "e1", Kind: harness.EntryToolResult, OperationID: "op1", Payload: rawJSON(`{"entry":"e1"}`)})
		return err
	})
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("exact-version reopen: %v", err)
	}
	defer reopened.Close()

	ids, err := reopened.ListSessionIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "s1" {
		t.Fatalf("ListSessionIDs after reopen = %v (error %v), want [s1]", ids, err)
	}
	entries, err := reopened.ReadEntries(ctx, "s1", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadEntries after reopen = %v (error %v), want [e1]", entries, err)
	}
	entry := entries[0]
	if entry.ID != "e1" || entry.Sequence != 1 || entry.Kind != harness.EntryToolResult || entry.OperationID != "op1" || string(entry.Payload) != `{"entry":"e1"}` {
		t.Errorf("reopened entry = %+v, want the committed entry %#v", entry, first)
	}
	if !entry.CommittedAt.Equal(first.CommittedAt) || entry.CommittedAt.Location() != time.UTC {
		t.Errorf("reopened commit time = %v, want the committed %v in UTC", entry.CommittedAt, first.CommittedAt)
	}
	register, err := reopened.ReadRegister(ctx, confSessionKey("s1"))
	if err != nil || register.Revision != 2 || string(register.Payload) != `{"state":"replaced"}` {
		t.Errorf("reopened session register = %+v (error %v), want revision 2", register, err)
	}
	operation, err := reopened.ReadRegister(ctx, confOpKey("s1", "op1"))
	if err != nil || operation.Revision != 1 {
		t.Errorf("reopened operation register = %+v (error %v), want revision 1", operation, err)
	}

	// Current hot-WAL sibling: the canonical schema, schema version, and
	// committed data live only in an uncheckpointed WAL. The source database
	// and WAL bytes are copied while the source owner is open and the SHM
	// file is omitted, leaving a crash state with no live owner. Read-only
	// validation must see the WAL-carried version and schema and accept the
	// copy without writing the database or WAL; a SQLite-owned -shm WAL index
	// may appear and remain.
	source := filepath.Join(t.TempDir(), "wal-current-source.db")
	live, err := sql.Open(sqliteDriverName, sqliteDSN(source, "_journal_mode=WAL&_synchronous=FULL"))
	if err != nil {
		t.Fatalf("open hot-WAL source: %v", err)
	}
	for _, stmt := range []string{
		sqliteEntriesDDL,
		sqliteRegistersDDL,
		sqliteOperationIndexDDL,
		`PRAGMA user_version = 1`,
		`INSERT INTO registers (session_id, kind, operation_id, revision, payload) VALUES ('s1', 'session', '', 1, '{"session":"s1"}')`,
		`INSERT INTO entries (session_id, sequence, entry_id, operation_id, kind, committed_at_ns, payload) VALUES ('s1', 1, 'e1', '', 'input', 1700000000000000000, '{"entry":"e1"}')`,
	} {
		if _, err := live.Exec(stmt); err != nil {
			t.Fatalf("build hot-WAL source: %v", err)
		}
	}
	mainBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("copy source main file: %v", err)
	}
	walBytes, err := os.ReadFile(source + "-wal")
	if err != nil {
		t.Fatalf("copy source WAL: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("close hot-WAL source: %v", err)
	}

	crashed := filepath.Join(t.TempDir(), "wal-current.db")
	if err := os.WriteFile(crashed, mainBytes, 0o644); err != nil {
		t.Fatalf("materialize crash copy: %v", err)
	}
	if err := os.WriteFile(crashed+"-wal", walBytes, 0o644); err != nil {
		t.Fatalf("materialize crash WAL: %v", err)
	}
	beforeMain := sqliteFileDigest(t, crashed)
	beforeWAL := sqliteFileDigest(t, crashed+"-wal")

	crashStore, err := OpenSQLite(crashed)
	if err != nil {
		t.Fatalf("read-only validation of the WAL-carried state: %v", err)
	}
	ids, err = crashStore.ListSessionIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "s1" {
		t.Errorf("WAL-carried session discovery = %v (error %v), want [s1]", ids, err)
	}
	if walEntries, err := crashStore.ReadEntries(ctx, "s1", 0); err != nil || len(walEntries) != 1 || walEntries[0].ID != "e1" {
		t.Errorf("WAL-carried entries = %v (error %v), want [e1]", walEntries, err)
	}
	if after := sqliteFileDigest(t, crashed); after != beforeMain {
		t.Error("validated crash-copy database changed")
	}
	if after := sqliteFileDigest(t, crashed+"-wal"); after != beforeWAL {
		t.Error("validated crash-copy WAL changed")
	}
	if _, err := os.Stat(crashed + "-journal"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("validated crash copy created a journal sidecar")
	}
	if err := crashStore.Close(); err != nil {
		t.Fatalf("close crash copy: %v", err)
	}
}

// TestSQLitePhysicalCorruption proves a database whose schema image is
// physically malformed fails the open with ErrStorage, is never misread as an
// incompatible schema, and is left byte-identical by the rejected open.
func TestSQLitePhysicalCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lightcode.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	confCreateSession(t, context.Background(), store, "s1")
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Destroy the b-tree page type of the schema root while keeping the
	// database header intact, so the file is still recognizable as SQLite
	// but its schema image is unreadable.
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := file.WriteAt([]byte{0x00}, 100); err != nil {
		t.Fatalf("corrupt schema page: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close after corruption: %v", err)
	}
	before := sqliteFileDigest(t, path)

	opened, err := OpenSQLite(path)
	if !errors.Is(err, harness.ErrStorage) {
		t.Fatalf("error = %v, want ErrStorage", err)
	}
	if errors.Is(err, harness.ErrIncompatible) {
		t.Errorf("physical corruption misclassified as incompatible schema: %v", err)
	}
	if opened != nil {
		t.Error("corrupt database returned a store")
	}
	if after := sqliteFileDigest(t, path); after != before {
		t.Error("rejected corrupt database file changed")
	}
}

// TestSQLiteConnectionConfiguration forces fresh pooled connections and proves
// each physical connection reports the configured write-ahead journal, full
// synchronous mode, and busy timeout, that a transaction opened through the
// pool holds the write lock immediately before any statement runs, and that
// one public read's statements share one deferred read snapshot that stays
// pre-commit across a concurrent writer's commit.
func TestSQLiteConnectionConfiguration(t *testing.T) {
	store := newSQLiteStore(t)
	store.db.SetMaxIdleConns(0) // every acquired connection is physically fresh
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		for pragma, want := range map[string]string{
			"PRAGMA journal_mode": "wal",
			"PRAGMA synchronous":  "2",
			"PRAGMA busy_timeout": "5000",
		} {
			var got string
			if err := conn.QueryRowContext(ctx, pragma).Scan(&got); err != nil {
				t.Fatalf("connection %d, %s: %v", i, pragma, err)
			}
			if got != want {
				t.Errorf("connection %d, %s = %q, want %q", i, pragma, got, want)
			}
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("connection %d close: %v", i, err)
		}
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	other, err := sql.Open(sqliteDriverName, sqliteDSN(store.path, "_busy_timeout=10"))
	if err != nil {
		t.Fatalf("open competing connection: %v", err)
	}
	defer other.Close()
	if _, err := other.Exec(`BEGIN IMMEDIATE`); err == nil {
		t.Error("BEGIN IMMEDIATE succeeded while the pool transaction holds the write lock; the pool transaction did not use BEGIN IMMEDIATE")
	} else {
		var busy sqlite3.Error
		if !errors.As(err, &busy) || busy.Code != sqlite3.ErrBusy {
			t.Errorf("BEGIN IMMEDIATE error = %v, want SQLITE_BUSY", err)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("release pool transaction: %v", err)
	}

	// One public read's statements share one deferred read snapshot on a
	// single pooled connection: a concurrent writer commits mid-read, and the
	// statements of the read still observe the complete pre-commit state.
	confCreateSession(t, ctx, store, "s1")
	confTxn(t, ctx, store, func(txn harness.Transaction) error {
		_, err := txn.InsertEntry(confEntry("s1", "e1"))
		return err
	})
	err = store.readSnapshot(ctx, func(ctx context.Context, q sqliteQuerier) error {
		before, err := sqliteReadEntries(ctx, q, "s1", 0)
		if err != nil {
			return err
		}
		confTxn(t, ctx, store, func(txn harness.Transaction) error {
			_, err := txn.InsertEntry(confEntry("s1", "e2"))
			return err
		})
		after, err := sqliteReadEntries(ctx, q, "s1", 0)
		if err != nil {
			return err
		}
		if len(before) != 1 || before[0].ID != "e1" || len(after) != 1 || after[0].ID != "e1" {
			t.Errorf("read snapshot observed a concurrent commit: before %v, after %v, want the same pre-commit state [e1]", before, after)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if entries, err := store.ReadEntries(ctx, "s1", 0); err != nil || len(entries) != 2 || entries[1].ID != "e2" {
		t.Errorf("post-snapshot read = %v (error %v), want the committed transition [e1 e2]", entries, err)
	}
}
