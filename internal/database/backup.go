package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
)

// Backup and integrity check for the SQLite backend (SPEC-030). Both open a connection of their
// own on the database file, so they run beside a serving Lightstack, and both run their statement
// on the raw handle: the SQLite translation would rewrite a "~" or "::" in a file name. check also
// reads the migrations folder: the schema a recorded version requires is what those migrations
// build. PostgreSQL has pg_dump and its own checks; on a PostgreSQL URL both refuse and say so.

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

// openForMaintenance opens one read-write connection on an existing database file. Backup uses it:
// VACUUM INTO runs on the source, and the source is the live database, already in WAL mode.
func openForMaintenance(ctx context.Context, path string) (*sql.DB, error) {
	return openSQLiteHandle(ctx, path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1, MinConns: 1})
}

// openForInspection opens one must-exist, read-only connection for check. It alters the file in no
// way — not its journal mode, not its permissions — where the server opener would open mode=rw,
// convert the file to WAL and could tighten its mode. So check inspects a live database (reading
// its live WAL), a standalone backup, and a backup on read-only media alike (SPEC-030).
func openForInspection(ctx context.Context, path string) (*sql.DB, error) {
	// mode=ro already fails a missing file, but Stat gives the plain "no such file" rather than
	// SQLITE_CANTOPEN, and rejects a directory before the driver does.
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("SQLite database %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("SQLite database %s is not a regular file", path)
	}
	db, err := sql.Open("sqlite", sqliteInspectDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open SQLite database %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// sql.Open is lazy; force the open now so an unreadable or non-database file fails here
	// rather than inside the integrity check with a misleading message.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open SQLite database %s: %w", path, err)
	}
	return db, nil
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
// schema version only when the database is intact, the version is not marked dirty by an
// interrupted migration, and the database holds the whole Lightstack schema of that version. It
// opens a read-only connection that alters nothing — not the journal mode, not the permissions —
// so it runs beside a serving Lightstack and inspects a standalone backup, even one on read-only
// media, without rewriting it. A database damaged badly enough fails the statement itself; that is
// reported as is.
//
// An empty file is a valid, empty SQLite database and passes integrity_check. Restored in place
// of the database, it would start Lightstack as a fresh install, so check also requires the
// migration version Lightstack's schema records — and, since a partial recovery can keep the
// version while losing tables, the schema that version requires, as the migrations in
// migrationsPath build it (SPEC-030 FR-005a).
func IntegrityCheck(ctx context.Context, dbURL, migrationsPath string) (uint64, error) {
	path, err := requireSQLite("check", dbURL)
	if err != nil {
		return 0, err
	}
	db, err := openForInspection(ctx, path)
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
	version, err := schemaVersion(ctx, db, path)
	if err != nil {
		return 0, err
	}
	if err := requireCompleteSchema(ctx, db, path, migrationsPath, version); err != nil {
		return 0, err
	}
	return version, nil
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

// lightstackCoreTables are tables migration 001 creates and every later version keeps; the SQLite
// baseline (073) has them too, so every Lightstack database carries all of them. Their presence —
// not golang-migrate's own schema_migrations table, which any golang-migrate database has — is what
// marks a file as holding Lightstack's schema.
var lightstackCoreTables = []string{"users", "projects", "instances", "flavors"}

// schemaVersion reads the migration version golang-migrate records, refusing a database that is not
// a restorable Lightstack one: an empty file, another program's golang-migrate database, a partial
// .recover that kept only the version row, or one migrated all the way down. A recorded version is
// generic golang-migrate metadata on its own, so it also requires Lightstack's own core tables to
// be present (SPEC-030 FR-005a).
//
// It also refuses a version marked dirty. golang-migrate commits that mark before it runs a
// migration and clears it after, so a dirty version means an interrupted migration, and startup
// refuses the database ("Dirty database version N"). On SQLite the failed migration rolls back,
// leaving an earlier version's schema under the dirty one, so the mark is read here, before the
// schema comparison would report that as missing tables.
func schemaVersion(ctx context.Context, db *sql.DB, path string) (uint64, error) {
	notOurs := fmt.Errorf("%s holds no Lightstack schema: it is empty, an incomplete recovery, or not a Lightstack database", path)
	for _, name := range append([]string{"schema_migrations"}, lightstackCoreTables...) {
		switch ok, err := tableExists(ctx, db, name); {
		case err != nil:
			return 0, fmt.Errorf("read the schema version of %s: %w", path, err)
		case !ok:
			return 0, notOurs
		}
	}
	var version uint64
	var dirty bool
	switch err := db.QueryRowContext(ctx, "SELECT version, dirty FROM schema_migrations LIMIT 1").Scan(&version, &dirty); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, notOurs
	case err != nil:
		return 0, fmt.Errorf("read the schema version of %s: %w", path, err)
	case dirty:
		return 0, fmt.Errorf("%s is marked dirty at migration version %d: a migration was interrupted, and Lightstack "+
			"will not start on it. Restore another backup, or force the version whose schema the file holds (%d, or "+
			"the one before it if that migration rolled back) and run check again", path, version, version)
	}
	return version, nil
}

// requireCompleteSchema refuses a database that lacks any table, index, view, trigger or column its
// schema version requires — what a partial .recover can leave. integrity_check does not see it, and
// MigrateUp would take the version as current and never recreate the missing part, so Lightstack
// would start and fail on it later. What a version requires is the migrations applied up to it: an
// older backup is held to its own version and migrated forward on start, and a version newer than
// the migrations is refused as startup refuses it (FR-008). Extra objects, such as the
// lost_and_found table .recover may add, do not stop Lightstack and are not refused.
func requireCompleteSchema(ctx context.Context, db *sql.DB, path, migrationsPath string, version uint64) error {
	if err := refuseNewerVersion(version, migrationsPath, SQLite); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	want, err := referenceSchema(ctx, migrationsPath, version)
	if err != nil {
		return fmt.Errorf("build the schema of version %d to compare %s with: %w", version, path, err)
	}
	have, err := schemaElements(ctx, db)
	if err != nil {
		return fmt.Errorf("read the schema of %s: %w", path, err)
	}
	if missing := missingElements(want, have); len(missing) > 0 {
		return fmt.Errorf("%s lacks part of the schema version %d requires (an incomplete recovery or an altered database):\n  %s",
			path, version, strings.Join(missing, "\n  "))
	}
	return nil
}

// referenceSchema returns the schema elements a SQLite database at version holds: the migrations
// folder's SQLite view applied up to version on a scratch in-memory database, then read back.
func referenceSchema(ctx context.Context, migrationsPath string, version uint64) (map[string]bool, error) {
	src, err := newMigrationSource(migrationsPath, SQLite)
	if err != nil {
		return nil, err
	}
	scratch, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	scratch.SetMaxOpenConns(1) // an in-memory database lives and dies with its one connection
	drv, err := migratesqlite.WithInstance(scratch, &migratesqlite.Config{})
	if err != nil {
		_ = scratch.Close()
		_ = src.Close()
		return nil, err
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", drv)
	if err != nil {
		_ = drv.Close()
		_ = src.Close()
		return nil, err
	}
	defer closeMigrator(m)
	if err := m.Migrate(uint(version)); err != nil {
		return nil, err
	}
	return schemaElements(ctx, scratch)
}

// schemaElements lists what a SQLite schema is made of, each element as "<kind> <name>": its tables,
// indexes, views and triggers, and every table column as "column <table>.<column>". SQLite's own
// sqlite_% objects are left out.
func schemaElements(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT type, name FROM sqlite_schema
		WHERE type IN ('table', 'index', 'view', 'trigger') AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	elements := map[string]bool{}
	var tables []string
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		elements[kind+" "+name] = true
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, table := range tables {
		columns, err := tableColumns(ctx, db, table)
		if err != nil {
			return nil, err
		}
		for _, column := range columns {
			elements["column "+table+"."+column] = true
		}
	}
	return elements, nil
}

// tableColumns returns the column names of table. It runs PRAGMA table_info rather than the
// pragma_table_info function: the static SQL check (TestSQLConstructs) refuses, in any query, a
// function PostgreSQL lacks, and PRAGMA statements, SQLite's own like the integrity check, are
// outside it.
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info("`+strings.ReplaceAll(table, `"`, `""`)+`")`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		// table_info answers cid, name, type, notnull, dflt_value and pk; only the name is needed.
		var name string
		var ignored any
		if err := rows.Scan(&ignored, &name, &ignored, &ignored, &ignored, &ignored); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// missingElements lists, sorted, the elements of want that have lacks. The columns of a missing
// table are left out: the table is listed, and its columns would only repeat it.
func missingElements(want, have map[string]bool) []string {
	var missing []string
	for e := range want {
		if have[e] {
			continue
		}
		if column, ok := strings.CutPrefix(e, "column "); ok {
			if table, _, _ := strings.Cut(column, "."); !have["table "+table] {
				continue
			}
		}
		missing = append(missing, e)
	}
	slices.Sort(missing)
	return missing
}

// tableExists reports whether the SQLite database has a table of the given name.
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", name).Scan(&n)
	return n > 0, err
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
