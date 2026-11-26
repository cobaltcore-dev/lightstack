package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

// DatabaseState is what a database's migration records say about it (SPEC-032 data-model §3). It is
// read before golang-migrate runs, so a database golang-migrate would misreport — or touch — is
// refused first, with the reason and the way out.
type DatabaseState int

const (
	// StateFresh records no version: no file yet (SQLite), no schema_migrations, or no row in it.
	// It migrates from 001.
	StateFresh DatabaseState = iota
	// StatePreSquash records a version but has no applied_migrations: a Lightstack from before the
	// squash migrated it (FR-011, FR-012). Refused everywhere but reset and backup.
	StatePreSquash
	// StateBaselineInFlight records version 1 as dirty without applied_migrations: another node is
	// applying the baseline right now. golang-migrate's lock decides; it is not pre-squash.
	StateBaselineInFlight
	// StateCurrent is a post-squash database at or below the newest migration it is offered.
	StateCurrent
	// StateNewer is a post-squash database above the newest migration: a newer Lightstack wrote it.
	StateNewer
	// StateDirty is a post-squash database whose last migration was interrupted.
	StateDirty
	// StateStale is a post-squash database whose record of its version does not match what the
	// migrations serve for that version now: it applied an earlier state of the open release
	// migration, or of a migration frozen since with other content (FR-019, FR-020). Refused
	// everywhere but force, reset and backup.
	StateStale
)

func (s DatabaseState) String() string {
	switch s {
	case StateFresh:
		return "fresh"
	case StatePreSquash:
		return "pre-squash"
	case StateBaselineInFlight:
		return "baseline in flight"
	case StateCurrent:
		return "current"
	case StateNewer:
		return "newer"
	case StateDirty:
		return "dirty"
	case StateStale:
		return "stale"
	}
	return fmt.Sprintf("DatabaseState(%d)", int(s))
}

// PreSquashError refuses a database a Lightstack from before the migration squash migrated. It
// names the database's version and the way out (contracts/cli-and-startup.md).
type PreSquashError struct {
	Version uint64
}

func (e *PreSquashError) Error() string {
	return fmt.Sprintf("the database is at migration version %d, from before the migration squash. "+
		`Recreate the database. See docs/UPGRADING.md, "The migration squash"`, e.Version)
}

// IsPreSquash reports whether err refuses a database migrated before the migration squash.
func IsPreSquash(err error) bool {
	var p *PreSquashError
	return errors.As(err, &p)
}

// StaleError refuses a database whose record of its version does not match what this Lightstack's
// migrations serve for that version (FR-019, FR-020): it applied an earlier state of the open
// release migration, of a migration frozen since with other content, or of the baseline — or it
// holds no record at all. The way out is to recreate it.
type StaleError struct {
	Version  uint64
	Release  string // the release Version has been frozen as since, when it has
	Baseline bool   // Version is the baseline
	NoRecord bool   // applied_migrations holds no row for Version
}

func (e *StaleError) Error() string {
	switch {
	case e.NoRecord:
		return fmt.Sprintf("the database is at migration version %d but holds no record of what it applied. "+
			"Recreate the database", e.Version)
	case e.Release != "":
		return fmt.Sprintf("the database applied an earlier state of migration %03d, which has since been frozen "+
			"as release %s with different content. Recreate the database", e.Version, e.Release)
	case e.Baseline:
		return fmt.Sprintf("the database applied an earlier state of the baseline, migration %03d, which has "+
			"changed since. Recreate the database", e.Version)
	}
	return fmt.Sprintf("the database applied an earlier state of the open release migration %03d (unreleased): "+
		"a fragment was added, changed or removed since. Recreate the database", e.Version)
}

// MissingDatabaseError reports a SQLite database file that does not exist yet. up and the server
// create it; status, down and force open must-exist and create nothing, so a mistyped path is an
// error (SPEC-028). As a type, it lets a caller tell a database not created yet from a failure:
// status reports it as NO DATABASE (SPEC-032 FR-022).
type MissingDatabaseError struct {
	Path string
}

func (e *MissingDatabaseError) Error() string {
	return fmt.Sprintf("SQLite database %s does not exist: up and the server create it at their first run. "+
		"If that is not the path you meant, correct database.url", e.Path)
}

// IsMissingDatabase reports whether err reports a SQLite database file that does not exist yet.
func IsMissingDatabase(err error) bool {
	var m *MissingDatabaseError
	return errors.As(err, &m)
}

// IsStale reports whether err refuses a stale database.
func IsStale(err error) bool {
	var s *StaleError
	return errors.As(err, &s)
}

// migrationRecords is what a database records about its migrations: golang-migrate's version row,
// whether Lightstack's applied_migrations table exists — the post-squash marker — and, if it does,
// the checksum it records for the version.
type migrationRecords struct {
	hasVersion bool
	version    int64
	dirty      bool
	applied    bool
	record     *string // applied_migrations' checksum for version; nil when it holds no row for it
}

// classify reads the state from records. latest is the newest version the migrations folder offers.
// Only the post-squash marker tells a pre-squash database from a post-squash one, because the
// restarted numbering reuses the numbers pre-squash databases recorded (FR-012).
func classify(r migrationRecords, latest uint64) DatabaseState {
	switch {
	case !r.hasVersion, r.version < 0 && !r.dirty:
		return StateFresh
	case !r.applied && r.version == 1 && r.dirty:
		return StateBaselineInFlight
	case !r.applied:
		return StatePreSquash
	case r.dirty:
		return StateDirty
	case uint64(r.version) > latest:
		return StateNewer
	}
	return StateCurrent
}

// inspection is what inspectDatabase found.
type inspection struct {
	backend Backend
	state   DatabaseState
	version uint64
	dirty   bool
	stale   *StaleError // why, when state is StateStale
	missing string      // the SQLite file that does not exist yet; "" otherwise
}

// refusal is the error the state calls for before golang-migrate touches the database — a
// *PreSquashError or a *StaleError — or nil.
func (in inspection) refusal() error {
	switch in.state {
	case StatePreSquash:
		return &PreSquashError{Version: in.version}
	case StateStale:
		return in.stale
	}
	return nil
}

// inspectDatabase reads the database's state over one short connection, before any migrator runs
// (research R9). A SQLite file that does not exist yet is fresh and is not created: up creates it,
// and a must-exist command refuses it on its own.
func inspectDatabase(dbURL, migrationsPath string) (inspection, error) {
	backend, err := ParseURL(dbURL)
	if err != nil {
		return inspection{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec, missing, err := readRecords(ctx, backend, dbURL)
	switch {
	case err != nil:
		return inspection{}, err
	case missing != "":
		return inspection{backend: backend, state: StateFresh, missing: missing}, nil
	}
	return inspectRecords(rec, migrationsPath, backend)
}

// readRecords reads the migration records of the database at dbURL over one short connection. For
// a SQLite file that does not exist yet it returns the file's path as missing, and creates nothing.
func readRecords(ctx context.Context, backend Backend, dbURL string) (rec migrationRecords, missing string, err error) {
	switch backend {
	case SQLite:
		path, err := sqlitePathFromURL(dbURL)
		if err != nil {
			return rec, "", err
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return rec, path, nil
		}
		db, err := openForInspection(ctx, path)
		if err != nil {
			return rec, "", err
		}
		defer func() { _ = db.Close() }()
		rec, err = readSQLiteRecords(ctx, db)
		return rec, "", err
	case Postgres:
		conn, err := pgx.Connect(ctx, dbURL)
		if err != nil {
			return rec, "", fmt.Errorf("inspect the database: %w", err)
		}
		defer func() { _ = conn.Close(context.Background()) }()
		rec, err = readPostgresRecords(ctx, conn)
		return rec, "", err
	}
	return rec, "", fmt.Errorf("database backend %q is not supported", backend)
}

// inspectRecords classifies the records read from a database against backend's view of
// migrationsPath.
func inspectRecords(rec migrationRecords, migrationsPath string, backend Backend) (inspection, error) {
	state, stale, err := stateOf(rec, migrationsPath, backend)
	if err != nil {
		return inspection{}, err
	}
	in := inspection{backend: backend, state: state, dirty: rec.dirty, stale: stale}
	if rec.version > 0 {
		in.version = uint64(rec.version)
	}
	return in, nil
}

// stateOf classifies rec against backend's view of migrationsPath. A current database whose record
// of its version does not match what the view serves for that version now is stale; the
// *StaleError says why (FR-019, FR-020).
func stateOf(rec migrationRecords, migrationsPath string, backend Backend) (DatabaseState, *StaleError, error) {
	view, err := newMigrationView(os.DirFS(migrationsPath), backend)
	if err != nil {
		return 0, nil, err
	}
	state := classify(rec, view.latestVersion())
	if state != StateCurrent || rec.version <= 0 {
		return state, nil, nil
	}
	stale, err := view.staleness(uint64(rec.version), rec.record)
	if err != nil || stale == nil {
		return state, nil, err
	}
	return StateStale, stale, nil
}

func readSQLiteRecords(ctx context.Context, db *sql.DB) (migrationRecords, error) {
	var rec migrationRecords
	var hasTable bool
	if err := db.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migrations'),
		EXISTS (SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'applied_migrations')`).Scan(&hasTable, &rec.applied); err != nil {
		return rec, fmt.Errorf("inspect the database: %w", err)
	}
	if !hasTable {
		return rec, nil
	}
	switch err := db.QueryRowContext(ctx, "SELECT version, dirty FROM schema_migrations LIMIT 1").Scan(&rec.version, &rec.dirty); {
	case errors.Is(err, sql.ErrNoRows):
		return rec, nil
	case err != nil:
		return rec, fmt.Errorf("inspect the database: %w", err)
	}
	rec.hasVersion = true
	if !rec.applied {
		return rec, nil
	}
	var sum string
	switch err := db.QueryRowContext(ctx, "SELECT checksum FROM applied_migrations WHERE version = ?", rec.version).Scan(&sum); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return rec, fmt.Errorf("inspect the database: %w", err)
	default:
		rec.record = &sum
	}
	return rec, nil
}

// readPostgresRecords reads the records inspectDatabase classifies. Each query returns at most one
// row; no row means the record is absent.
func readPostgresRecords(ctx context.Context, conn *pgx.Conn) (migrationRecords, error) {
	var rec migrationRecords
	var hasTable bool
	// to_regclass resolves through search_path, where golang-migrate keeps schema_migrations.
	//sqlcheck:postgres-only
	if err := conn.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL,
		to_regclass('applied_migrations') IS NOT NULL`).Scan(&hasTable, &rec.applied); err != nil {
		return rec, fmt.Errorf("inspect the database: %w", err)
	}
	if !hasTable {
		return rec, nil
	}
	switch err := conn.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations LIMIT 1").Scan(&rec.version, &rec.dirty); {
	case errors.Is(err, pgx.ErrNoRows):
		return rec, nil
	case err != nil:
		return rec, fmt.Errorf("inspect the database: %w", err)
	}
	rec.hasVersion = true
	if !rec.applied {
		return rec, nil
	}
	var sum string
	switch err := conn.QueryRow(ctx, "SELECT checksum FROM applied_migrations WHERE version = $1", rec.version).Scan(&sum); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return rec, fmt.Errorf("inspect the database: %w", err)
	default:
		rec.record = &sum
	}
	return rec, nil
}

// CheckDatabaseState returns the refusal a database's state calls for before golang-migrate touches
// it, or nil: a *PreSquashError for a database migrated before the squash (FR-011), a *StaleError
// for one that applied an earlier state of its version (FR-020), each with the way out. The
// migrating entry points call it first; a command that prompts calls it before prompting too, so
// nobody confirms what cannot run. Reset and backup do not: reset is how such a database is
// recreated.
func CheckDatabaseState(dbURL, migrationsPath string) error {
	in, err := inspectDatabase(dbURL, migrationsPath)
	if err != nil {
		return err
	}
	return in.refusal()
}

// CheckForceState is CheckDatabaseState for force: a stale database is not refused, because force is
// the repair tool for a dirty version, and a stale database may be dirty too (FR-020).
func CheckForceState(dbURL, migrationsPath string) error {
	if err := CheckDatabaseState(dbURL, migrationsPath); err != nil && !IsStale(err) {
		return err
	}
	return nil
}
