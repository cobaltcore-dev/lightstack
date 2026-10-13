package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// The SQLite implementation (research R3, R4): database/sql over the pure-Go
// modernc.org/sqlite driver, one file on the node, write-ahead logging, fully
// synchronous commits, a bounded busy wait and foreign keys on every connection.

// sqliteBusyTimeoutMS bounds how long a writer waits for the write lock. Constitution
// v1.3.0, Article III, caps it at 5 seconds; write transactions never span external
// work (Article II), so they last milliseconds.
const sqliteBusyTimeoutMS = 5000

// defaultSQLiteTxLock is how transactions begin. A deferred transaction that reads and
// then writes fails at once with SQLITE_BUSY_SNAPSHOT when another writer committed in
// between, and the busy wait does not apply to that; an immediate transaction takes the
// write lock when it begins. The concurrency matrix decided it (TestCinderConcurrencyMatrix
// and its Nova and Neutron siblings): deferred fails 5 of the 13 transactions that read
// before they write, immediate none (research R4).
const defaultSQLiteTxLock = "immediate"

const sqliteFileMode os.FileMode = 0o600

// sqlitePathFromURL returns the database file a sqlite:///absolute/path URL names.
func sqlitePathFromURL(raw string) (string, error) {
	if _, err := ParseURL(raw); err != nil {
		return "", err
	}
	u, _ := url.Parse(raw)
	return filepath.Clean(u.Path), nil
}

// sqliteDSN builds the driver's URI. mode=rw never creates the file: the create-if-
// missing entry points create it themselves with the right mode, and the must-exist
// ones must not create it at all.
func sqliteDSN(path, txlock string) string {
	// busy_timeout is set before journal_mode: switching a fresh file to WAL contends with
	// another process doing the same, and only a busy_timeout already in effect makes that
	// contention wait rather than fail at once with "database is locked (5)".
	return "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?mode=rw" +
		fmt.Sprintf("&_pragma=busy_timeout(%d)", sqliteBusyTimeoutMS) +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=" + txlock
}

// prepareSQLiteFile creates the directory and the file for a create-if-missing open,
// or checks that the file exists for a must-exist one, then tightens the modes.
func prepareSQLiteFile(path string, mustExist bool) error {
	if mustExist {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("SQLite database %s: %w", path, err)
		}
	} else {
		if err := makeDatabaseDir(filepath.Dir(path)); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, sqliteFileMode)
		switch {
		case err == nil:
			_ = f.Close()
		case !errors.Is(err, os.ErrExist):
			return fmt.Errorf("create the database file %s: %w", path, err)
		}
	}
	// A path that names a directory — a forgotten file name, or sqlite:///$EMPTY — passed
	// the existence checks above (Stat and O_EXCL both report a directory as "already
	// there"). Refuse it before tightenSQLiteModes chmods it to 0600: left 0600, a state
	// root is untraversable for QEMU, and sqlite:/// would chmod the filesystem root.
	if fi, err := os.Lstat(path); err != nil {
		return fmt.Errorf("SQLite database %s: %w", path, err)
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("SQLite database %s is not a regular file", path)
	}
	return tightenSQLiteModes(path)
}

// makeDatabaseDir creates the database directory 0750 (FR-011). Missing parents are
// created 0755, not 0750: on a fresh host the state root is one of them, and QEMU — not
// root — must traverse it to reach the instance disks.
//
// The modes are applied with an explicit Chmod, not left to Mkdir's mode-minus-umask: a
// hardened umask such as 027 would otherwise create the state root 0750 and reintroduce
// exactly that un-traversable-root defect. Only directories this call creates are
// chmod'ed; a pre-existing parent keeps its own mode.
func makeDatabaseDir(dir string) error {
	if err := mkdirAllMode(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dir), err)
	}
	switch err := os.Mkdir(dir, 0o750); {
	case errors.Is(err, os.ErrExist):
		return nil // pre-existing: leave its mode alone
	case err != nil:
		return fmt.Errorf("create the database directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		return fmt.Errorf("set the mode of %s: %w", dir, err)
	}
	return nil
}

// mkdirAllMode is os.MkdirAll with the umask undone: every directory it creates is
// chmod'ed to mode. A directory that already exists is left untouched.
func mkdirAllMode(path string, mode os.FileMode) error {
	if fi, err := os.Stat(path); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if parent := filepath.Dir(path); parent != path {
		if err := mkdirAllMode(parent, mode); err != nil {
			return err
		}
	}
	if err := os.Mkdir(path, mode); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil // created by a concurrent writer; don't touch its mode
		}
		return err
	}
	return os.Chmod(path, mode)
}

// tightenSQLiteModes makes the database, -wal and -shm readable by the process owner
// only (FR-011a): they hold recoverable credentials and console tokens.
func tightenSQLiteModes(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check the mode of %s: %w", p, err)
		}
		if fi.Mode().Perm()&0o077 == 0 {
			continue
		}
		if err := os.Chmod(p, sqliteFileMode); err != nil {
			return fmt.Errorf("restrict %s to its owner: %w", p, err)
		}
		log.Info().Str("path", p).Str("from", fmt.Sprintf("%04o", fi.Mode().Perm())).
			Str("to", fmt.Sprintf("%04o", sqliteFileMode)).Msg("database file mode tightened")
	}
	return nil
}

// openSQLiteHandle opens the database/sql handle and verifies that every setting the
// DSN requests took effect: a driver that ignores a key would otherwise leave the
// database without write-ahead logging or foreign keys, silently.
func openSQLiteHandle(ctx context.Context, path, txlock string, mustExist bool, pc PoolConfig) (*sql.DB, error) {
	if err := prepareSQLiteFile(path, mustExist); err != nil {
		return nil, err
	}
	// Switching a fresh file to WAL is an exclusive operation that can still lose a lock
	// race to another process doing the same first open, even with busy_timeout set; retry
	// over the busy window so a server starting next to a migrator does not exit on it.
	deadline := time.Now().Add(sqliteBusyTimeoutMS * time.Millisecond)
	for {
		db, err := openVerifiedSQLite(ctx, path, txlock, pc)
		if err == nil {
			return db, nil
		}
		if IsBusy(err) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return nil, err
	}
}

// openVerifiedSQLite opens one handle on path, applies the pool limits and checks every DSN
// setting took effect. openSQLiteHandle retries it on a lock race.
func openVerifiedSQLite(ctx context.Context, path, txlock string, pc PoolConfig) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path, txlock))
	if err != nil {
		return nil, fmt.Errorf("open SQLite database %s: %w", path, err)
	}
	if pc.MaxConns > 0 {
		db.SetMaxOpenConns(int(pc.MaxConns))
	}
	idle := int(pc.MinConns)
	if idle < 1 {
		idle = 1
	}
	db.SetMaxIdleConns(idle)
	db.SetConnMaxLifetime(pc.MaxConnLifetime)
	db.SetConnMaxIdleTime(pc.MaxConnIdleTime)
	if err := verifySQLiteSettings(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("SQLite database %s: %w", path, err)
	}
	return db, nil
}

type sqliteSettings struct {
	journalMode   string
	synchronous   int
	foreignKeys   int
	busyTimeoutMS int
}

func readSQLiteSettings(ctx context.Context, db *sql.DB) (sqliteSettings, error) {
	var s sqliteSettings
	for _, p := range []struct {
		pragma string
		dest   any
	}{
		{"journal_mode", &s.journalMode},
		{"synchronous", &s.synchronous},
		{"foreign_keys", &s.foreignKeys},
		{"busy_timeout", &s.busyTimeoutMS},
	} {
		if err := db.QueryRowContext(ctx, "PRAGMA "+p.pragma).Scan(p.dest); err != nil {
			return s, fmt.Errorf("read PRAGMA %s: %w", p.pragma, err)
		}
	}
	return s, nil
}

func verifySQLiteSettings(ctx context.Context, db *sql.DB) error {
	s, err := readSQLiteSettings(ctx, db)
	if err != nil {
		return err
	}
	if s.journalMode != "wal" || s.synchronous != 2 || s.foreignKeys != 1 || s.busyTimeoutMS != sqliteBusyTimeoutMS {
		return fmt.Errorf("connection settings did not take effect: journal_mode=%s synchronous=%d foreign_keys=%d busy_timeout=%d",
			s.journalMode, s.synchronous, s.foreignKeys, s.busyTimeoutMS)
	}
	return nil
}

func openSQLite(ctx context.Context, rawURL string, opts Options, pc PoolConfig) (DB, error) {
	path, err := sqlitePathFromURL(rawURL)
	if err != nil {
		return nil, err
	}
	txlock := opts.TxLock
	switch txlock {
	case "":
		txlock = defaultSQLiteTxLock
	case "deferred", "immediate":
	default:
		return nil, fmt.Errorf("SQLite transaction mode %q is not supported; use deferred or immediate", txlock)
	}
	db, err := openSQLiteHandle(ctx, path, txlock, opts.MustExist, pc)
	if err != nil {
		return nil, err
	}
	return &sqliteDB{db: db, path: path, txlock: txlock}, nil
}

type sqliteDB struct {
	db     *sql.DB
	path   string
	txlock string
}

// prepareSQLite translates a statement and converts its arguments for SQLite.
func prepareSQLite(query string, args []any) (string, []any, error) {
	text, expanded, err := rewrite(query, args)
	if err != nil {
		return "", nil, err
	}
	for i, a := range expanded {
		if expanded[i], err = bindValue(a); err != nil {
			return "", nil, err
		}
	}
	return text, expanded, nil
}

type sqlQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func sqliteQuery(ctx context.Context, q sqlQuerier, query string, args []any) (Rows, error) {
	text, a, err := prepareSQLite(query, args)
	if err != nil {
		return errRows{err}, err
	}
	rows, err := q.QueryContext(ctx, text, a...)
	if err != nil {
		return errRows{err}, err
	}
	return sqliteRows{rows}, nil
}

func sqliteQueryRow(ctx context.Context, q sqlQuerier, query string, args []any) Row {
	text, a, err := prepareSQLite(query, args)
	if err != nil {
		return sqliteRow{err: err}
	}
	return sqliteRow{row: q.QueryRowContext(ctx, text, a...)}
}

func sqliteExec(ctx context.Context, q sqlQuerier, query string, args []any) (Result, error) {
	text, a, err := prepareSQLite(query, args)
	if err != nil {
		return sqliteResult{}, err
	}
	res, err := q.ExecContext(ctx, text, a...)
	return sqliteResult{res}, err
}

func (d *sqliteDB) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	return sqliteQuery(ctx, d.db, query, args)
}

func (d *sqliteDB) QueryRow(ctx context.Context, query string, args ...any) Row {
	return sqliteQueryRow(ctx, d.db, query, args)
}

func (d *sqliteDB) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	return sqliteExec(ctx, d.db, query, args)
}

func (d *sqliteDB) Begin(ctx context.Context) (Tx, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return sqliteTx{tx}, nil
}

func (d *sqliteDB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }
func (d *sqliteDB) Close()                         { _ = d.db.Close() }

// InUse is the number of connections checked out of the pool, for the test harness's
// connection-leak check (dbtest.Close). database/sql's Close returns even with a leaked
// Rows, so this is the only signal of a leak on SQLite.
func (d *sqliteDB) InUse() int       { return d.db.Stats().InUse }
func (d *sqliteDB) Backend() Backend { return SQLite }

type sqliteTx struct{ tx *sql.Tx }

func (t sqliteTx) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	return sqliteQuery(ctx, t.tx, query, args)
}

func (t sqliteTx) QueryRow(ctx context.Context, query string, args ...any) Row {
	return sqliteQueryRow(ctx, t.tx, query, args)
}

func (t sqliteTx) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	return sqliteExec(ctx, t.tx, query, args)
}

func (t sqliteTx) Commit(context.Context) error   { return t.tx.Commit() }
func (t sqliteTx) Rollback(context.Context) error { return t.tx.Rollback() }

type sqliteRows struct{ rows *sql.Rows }

func (r sqliteRows) Next() bool             { return r.rows.Next() }
func (r sqliteRows) Scan(dest ...any) error { return scanConverted(r.rows.Scan, dest) }
func (r sqliteRows) Err() error             { return r.rows.Err() }
func (r sqliteRows) Close()                 { _ = r.rows.Close() }

type sqliteRow struct {
	row *sql.Row
	err error
}

func (r sqliteRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	err := scanConverted(r.row.Scan, dest)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoRows
	}
	return err
}

type sqliteResult struct{ res sql.Result }

func (r sqliteResult) RowsAffected() int64 {
	if r.res == nil {
		return 0
	}
	n, err := r.res.RowsAffected()
	if err != nil {
		return 0
	}
	return n
}

// errRows is the Rows of a query that failed before it ran.
type errRows struct{ err error }

func (r errRows) Next() bool        { return false }
func (r errRows) Scan(...any) error { return r.err }
func (r errRows) Err() error        { return r.err }
func (r errRows) Close()            {}
