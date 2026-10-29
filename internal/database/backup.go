package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Backup and integrity check for the SQLite backend (SPEC-030). Both open a connection of their
// own on the database file, so they run beside a serving Lightstack, and both run their statement
// on the raw handle: the SQLite translation would rewrite a "~" or "::" in a file name.
// PostgreSQL has pg_dump and its own checks; on a PostgreSQL URL both refuse and say so.

// requireSQLite returns the database file a SQLite URL names, or the refusal for any other.
func requireSQLite(op, dbURL string) (string, error) {
	backend, err := ParseURL(dbURL)
	if err != nil {
		return "", err
	}
	if backend != SQLite {
		return "", fmt.Errorf("%s is only for SQLite; database.url selects %s — use pg_dump", op, backend)
	}
	return sqlitePathFromURL(dbURL)
}

// openForMaintenance opens one connection on an existing database file.
func openForMaintenance(ctx context.Context, path string) (*sql.DB, error) {
	return openSQLiteHandle(ctx, path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1, MinConns: 1})
}

// Backup writes a consistent copy of the SQLite database dbURL names to dest with VACUUM INTO:
// one standalone file, no -wal or -shm, taken in a read transaction while other connections go
// on writing. dest must not exist — a backup never overwrites — and is created 0600 before the
// copy starts, so it is never readable by others, not even half-written. A directory it has to
// create is made 0750, the database directory's mode.
func Backup(ctx context.Context, dbURL, dest string) error {
	src, err := requireSQLite("backup", dbURL)
	if err != nil {
		return err
	}
	// Open the source first: a missing database must not leave an empty target behind.
	db, err := openForMaintenance(ctx, src)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := makeDatabaseDir(filepath.Dir(dest)); err != nil {
		return err
	}
	// VACUUM INTO fills an empty file and refuses any other, so creating the target here,
	// exclusively, is both the no-overwrite check and what makes it private from the start.
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sqliteFileMode)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("backup target %s already exists; each backup needs a new file", dest)
	}
	if err != nil {
		return fmt.Errorf("create backup target %s: %w", dest, err)
	}
	// The mode passed to OpenFile is filtered by the umask; set it outright.
	err = f.Chmod(sqliteFileMode)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		_, err = db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(dest, "'", "''")+"'")
	}
	if err != nil {
		_ = os.Remove(dest) // ours: created above, and not a usable backup
		return fmt.Errorf("back up %s to %s: %w", src, dest, err)
	}
	return nil
}

// IntegrityCheck runs PRAGMA integrity_check on the SQLite database dbURL names and returns its
// schema version only when the database is intact and is a Lightstack database. It only reads, so
// it runs beside a serving Lightstack. A database damaged badly enough fails the statement itself;
// that is reported as is.
//
// An empty file is a valid, empty SQLite database and passes integrity_check. Restored in place
// of the database, it would start Lightstack as a fresh install, so check also requires the
// migration version Lightstack's schema records (SPEC-030 FR-005a).
func IntegrityCheck(ctx context.Context, dbURL string) (uint64, error) {
	path, err := requireSQLite("check", dbURL)
	if err != nil {
		return 0, err
	}
	db, err := openForMaintenance(ctx, path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()

	result, err := integrityCheck(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("integrity check of %s: %w", path, err)
	}
	if err := integrityResult(result); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return schemaVersion(ctx, db, path)
}

// integrityCheck returns the rows PRAGMA integrity_check answers with.
func integrityCheck(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		result = append(result, line)
	}
	return result, rows.Err()
}

// schemaVersion reads the migration version golang-migrate records, refusing a database that
// has none: an empty file, another program's database, or one migrated all the way down.
func schemaVersion(ctx context.Context, db *sql.DB, path string) (uint64, error) {
	notOurs := fmt.Errorf("%s holds no Lightstack schema (no migration version): it is empty or not a Lightstack database", path)
	var tables int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migrations'").Scan(&tables); err != nil {
		return 0, fmt.Errorf("read the schema version of %s: %w", path, err)
	}
	if tables == 0 {
		return 0, notOurs
	}
	var version uint64
	switch err := db.QueryRowContext(ctx, "SELECT version FROM schema_migrations LIMIT 1").Scan(&version); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, notOurs
	case err != nil:
		return 0, fmt.Errorf("read the schema version of %s: %w", path, err)
	}
	return version, nil
}

// integrityResult reads integrity_check's answer: a single "ok" row is a pass, anything else
// lists the problems found.
func integrityResult(rows []string) error {
	if len(rows) == 1 && rows[0] == "ok" {
		return nil
	}
	if len(rows) == 0 {
		return errors.New("integrity check returned no result")
	}
	return fmt.Errorf("integrity check failed:\n  %s", strings.Join(rows, "\n  "))
}
