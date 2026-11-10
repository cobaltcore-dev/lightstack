package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func sqliteURL(path string) string { return "sqlite://" + path }

// pinUmask sets a restrictive umask for the duration of a test and restores it, so a
// directory-mode assertion proves the modes are applied explicitly, not left to the
// umask. The package's tests do not run in parallel, so the process-global umask is safe.
func pinUmask(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o027)
	t.Cleanup(func() { syscall.Umask(old) })
}

func openTestSQLite(t *testing.T, path string, opts Options) DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := Open(ctx, sqliteURL(path), opts)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(db.Close)
	return db
}

// TestSQLiteOpenCreatesDirectoryAndFile pins FR-011 and FR-011a for a first start: the
// missing directory is created 0750 and the database 0600. A missing parent — on a fresh
// host the state root — is created 0755, because QEMU must traverse it to reach its disks.
func TestSQLiteOpenCreatesDirectoryAndFile(t *testing.T) {
	pinUmask(t)
	root := filepath.Join(t.TempDir(), "state")
	dir := filepath.Join(root, "db")
	path := filepath.Join(dir, "lightstack.db")
	db := openTestSQLite(t, path, Options{})
	if _, err := db.Exec(context.Background(), `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for p, want := range map[string]os.FileMode{root: 0o755, dir: 0o750, path: 0o600, path + "-wal": 0o600, path + "-shm": 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o; want %o", p, got, want)
		}
	}
}

// TestSQLiteOpenTightensModes pins FR-011a for a database another tool left readable by
// others: every file is tightened to 0600 and each change is logged.
func TestSQLiteOpenTightensModes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lightstack.db")
	db := openTestSQLite(t, path, Options{})
	if _, err := db.Exec(context.Background(), `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	db.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("touch %s: %v", p, err)
		}
		f.Close()
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatalf("chmod %s: %v", p, err)
		}
	}

	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	defer func() { log.Logger = prev }()

	openTestSQLite(t, path, Options{})
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue // SQLite may have removed an empty -wal/-shm on open; then nothing is readable
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode after open = %o; want 600", p, got)
		}
	}
	if n := strings.Count(buf.String(), "database file mode tightened"); n < 1 {
		t.Fatalf("no log line for a tightened file; log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), path) {
		t.Fatalf("the log line does not name the file; log:\n%s", buf.String())
	}
}

// TestSQLiteMustExist pins FR-012: a must-exist open of a missing file fails naming the
// path and creates nothing — neither the file nor its directory.
func TestSQLiteMustExist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	path := filepath.Join(dir, "lightstack.db")
	_, err := Open(context.Background(), sqliteURL(path), Options{MustExist: true})
	if err == nil {
		t.Fatal("must-exist open of a missing file succeeded")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the path", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("must-exist open created %s (stat: %v)", dir, statErr)
	}
}

// TestSQLiteOpenRefusesADirectory: a URL that names a directory — a forgotten file name,
// or sqlite:/// — must be refused with its mode untouched, not chmod'ed to 0600.
func TestSQLiteOpenRefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, mustExist := range []bool{false, true} {
		if _, err := Open(context.Background(), sqliteURL(dir), Options{MustExist: mustExist}); err == nil {
			t.Fatalf("MustExist=%v: opening a directory succeeded", mustExist)
		}
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Fatalf("MustExist=%v: directory mode is %04o; want 0755 (it was tightened)", mustExist, fi.Mode().Perm())
		}
	}
}

// beginRead begins a read-only transaction that never takes the write lock, so several can be
// open at once on different connections. Only the settings test below needs it.
func (d *sqliteDB) beginRead(ctx context.Context) (Tx, error) {
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return sqliteTx{tx}, nil
}

// TestSQLiteSettingsOnEveryConnection pins FR-008: write-ahead logging, fully
// synchronous commits, the busy wait and foreign keys hold on every pooled connection.
func TestSQLiteSettingsOnEveryConnection(t *testing.T) {
	db := openTestSQLite(t, filepath.Join(t.TempDir(), "s.db"), Options{Pool: PoolConfig{MaxConns: 4, MinConns: 1}})
	ctx := context.Background()
	// Hold three connections at once so the checks below run on different ones.
	var txs []Tx
	for i := 0; i < 3; i++ {
		tx, err := db.(*sqliteDB).beginRead(ctx)
		if err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		txs = append(txs, tx)
	}
	for i, tx := range txs {
		var journal string
		var sync, fk, busy int
		if err := tx.QueryRow(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
			t.Errorf("conn %d journal_mode = %q, %v; want wal", i, journal, err)
		}
		if err := tx.QueryRow(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil || sync != 2 {
			t.Errorf("conn %d synchronous = %d, %v; want 2 (FULL)", i, sync, err)
		}
		if err := tx.QueryRow(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
			t.Errorf("conn %d foreign_keys = %d, %v; want 1", i, fk, err)
		}
		if err := tx.QueryRow(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil || busy != sqliteBusyTimeoutMS {
			t.Errorf("conn %d busy_timeout = %d, %v; want %d", i, busy, err, sqliteBusyTimeoutMS)
		}
	}
	for _, tx := range txs {
		_ = tx.Rollback(ctx)
	}
}

// TestSQLiteReadsDuringAWrite pins FR-009: with write-ahead logging, a reader is not
// blocked by an open write transaction on another connection.
func TestSQLiteReadsDuringAWrite(t *testing.T) {
	db := openTestSQLite(t, filepath.Join(t.TempDir(), "r.db"), Options{})
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	writer, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var n int
	if err := db.QueryRow(readCtx, `SELECT count(*) FROM t`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("read during an open write = %d, %v; want 0 without waiting", n, err)
	}
}

// TestSQLiteFunctions pins FR-015: the functions callers' SQL relies on exist on every
// connection.
func TestSQLiteFunctions(t *testing.T) {
	db := openTestSQLite(t, filepath.Join(t.TempDir(), "f.db"), Options{})
	ctx := context.Background()
	canonical := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`)

	var now string
	if err := db.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil || !canonical.MatchString(now) {
		t.Fatalf("now() = %q, %v; want the canonical timestamp", now, err)
	}
	var id string
	if err := db.QueryRow(ctx, `SELECT gen_random_uuid()`).Scan(&id); err != nil || len(id) != 36 || strings.ToLower(id) != id {
		t.Fatalf("gen_random_uuid() = %q, %v", id, err)
	}
	var later string
	if err := db.QueryRow(ctx, `SELECT ls_add_seconds('2026-10-09T08:15:30.000000Z', 90)`).Scan(&later); err != nil || later != "2026-10-09T08:17:00.000000Z" {
		t.Fatalf("ls_add_seconds = %q, %v", later, err)
	}
	var matched int
	if err := db.QueryRow(ctx, `SELECT 'web-01' REGEXP '^web-\d+$'`).Scan(&matched); err != nil || matched != 1 {
		t.Fatalf("REGEXP match = %d, %v; want 1", matched, err)
	}
	if err := db.QueryRow(ctx, `SELECT 'db-01' REGEXP '^web'`).Scan(&matched); err != nil || matched != 0 {
		t.Fatalf("REGEXP mismatch = %d, %v; want 0", matched, err)
	}
	if _, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "cinder_quota:p"); err != nil {
		t.Fatalf("advisory lock no-op: %v", err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE d (id TEXT PRIMARY KEY DEFAULT (gen_random_uuid()), at TEXT NOT NULL DEFAULT (now())) STRICT`); err != nil {
		t.Fatalf("defaults through registered functions: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO d DEFAULT VALUES`); err != nil {
		t.Fatalf("insert relying on the defaults: %v", err)
	}
}

// TestRegexpCacheIsBounded proves the compiled-pattern cache never grows past its cap.
// Patterns come from the API (the `name` server filter), so an unbounded cache would be
// a tenant-reachable memory leak.
func TestRegexpCacheIsBounded(t *testing.T) {
	db := openTestSQLite(t, filepath.Join(t.TempDir(), "re.db"), Options{})
	ctx := context.Background()
	for i := 0; i < regexpCacheCap*3; i++ {
		var matched int
		if err := db.QueryRow(ctx, `SELECT 'x' REGEXP $1`, fmt.Sprintf("^p%d$", i)).Scan(&matched); err != nil {
			t.Fatalf("REGEXP with pattern %d: %v", i, err)
		}
	}
	if n := regexpCacheLen(); n > regexpCacheCap {
		t.Fatalf("regexp cache holds %d patterns; want at most %d", n, regexpCacheCap)
	}
}

// TestSQLiteContract runs the same contract checks as the PostgreSQL implementation:
// ErrNoRows, RowsAffected, transactions and every error class (FR-006, FR-007).
func TestSQLiteContract(t *testing.T) {
	db := openTestSQLite(t, filepath.Join(t.TempDir(), "c.db"), Options{})
	ctx := context.Background()
	for _, s := range []string{
		`CREATE TABLE parent (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, size INTEGER CHECK (size >= 0)) STRICT`,
		`CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id)) STRICT`,
		`INSERT INTO parent (id, name, size) VALUES (1, 'a', 1)`,
	} {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	var n int
	if err := db.QueryRow(ctx, `SELECT 1 FROM parent WHERE id = 99`).Scan(&n); !errors.Is(err, ErrNoRows) {
		t.Fatalf("no rows = %v; want ErrNoRows", err)
	}
	res, err := db.Exec(ctx, `UPDATE parent SET size = size + 1 WHERE id >= $1`, 1)
	if err != nil || res.RowsAffected() != 1 {
		t.Fatalf("RowsAffected = %v, %v; want 1", res, err)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO parent (id, name) VALUES (2, 'rolled-back')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM parent`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after rollback = %d, %v; want 1", n, err)
	}

	cases := []struct {
		name string
		sql  string
		is   func(error) bool
	}{
		{"unique (primary key)", `INSERT INTO parent (id, name) VALUES (1, 'b')`, IsUniqueViolation},
		{"unique (constraint)", `INSERT INTO parent (id, name) VALUES (3, 'a')`, IsUniqueViolation},
		{"foreign key", `INSERT INTO child (id, parent_id) VALUES (1, 99)`, IsForeignKeyViolation},
		{"not null", `INSERT INTO parent (id, name) VALUES (4, NULL)`, IsNotNullViolation},
		{"check", `INSERT INTO parent (id, name, size) VALUES (5, 'c', -1)`, IsCheckViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(ctx, tc.sql)
			if err == nil || !tc.is(fmt.Errorf("wrapped: %w", err)) {
				t.Fatalf("%s: err = %v; classifier did not recognise it", tc.sql, err)
			}
		})
	}
	if err := HealthCheck(ctx, db); err != nil || db.Backend() != SQLite {
		t.Fatalf("HealthCheck = %v, Backend = %q", err, db.Backend())
	}
}

// TestSQLiteBusyIsClassified: a deferred transaction that read, then writes after
// another connection committed, fails at once with SQLITE_BUSY_SNAPSHOT (research R4).
// It is reported as busy, so it is told apart from a constraint violation.
func TestSQLiteBusyIsClassified(t *testing.T) {
	db := openTestSQLite(t, filepath.Join(t.TempDir(), "b.db"), Options{TxLock: "deferred"})
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO t (id) VALUES (2)`)
	if err == nil {
		t.Fatal("the stale deferred transaction wrote; want SQLITE_BUSY_SNAPSHOT")
	}
	if !IsBusy(fmt.Errorf("wrapped: %w", err)) || IsUniqueViolation(err) {
		t.Fatalf("%v is not classified as busy (only)", err)
	}
}

// TestConcurrentFirstOpenOnAFreshFile: several processes opening a brand-new file at once each
// switch it to WAL, an exclusive step that lost a lock race before busy_timeout was ordered ahead
// of journal_mode in the DSN. Every open must now succeed.
func TestConcurrentFirstOpenOnAFreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db", "fresh.db")
	const n = 24
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			db, err := openSQLiteHandle(ctx, path, defaultSQLiteTxLock, false, PoolConfig{MaxConns: 1})
			if err == nil {
				_ = db.Close()
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("open %d of %d failed: %v", i, n, err)
		}
	}
}
