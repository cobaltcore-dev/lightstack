package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/rs/zerolog/log"
)

// MigrationInfo contains migration status information: the version golang-migrate records, whether
// it is dirty, and what that version is. A refused state comes back as GetMigrationStatus's error.
type MigrationInfo struct {
	Version uint
	Dirty   bool
	// Kind says what the recorded version is (FR-022): "none (empty database)", the baseline, a
	// release migration ("release v0.8.0") or the open release migration of a build between
	// releases. It is empty for a version the migrations do not hold: a pre-squash database, or
	// one a newer Lightstack migrated.
	Kind string
}

// withMigrator runs fn with a migrator for the backend dbURL selects, reading the
// migrations folder through that backend's view (migrate_view.go), and closes it after.
//
// PostgreSQL migrates through golang-migrate's postgres driver and its advisory lock, as
// before. SQLite migrates through a connection of its own, opened with the service pool's
// settings — never the pool itself, because closing the migrator closes its connection —
// and under the migration file lock for the whole run (FR-030), since golang-migrate's
// SQLite driver locks within one process only.
// withMigrator opens the migrator for dbURL and runs fn. create tells the SQLite side
// whether to create a missing database: true for commands that establish a schema (up,
// goto, reset), false for commands that inspect or roll one back (status, down, force), so
// a mistyped path is an error rather than a new empty database stamped with a version.
func withMigrator(dbURL, migrationsPath string, create bool, fn func(m *migrate.Migrate) error) error {
	backend, err := ParseURL(dbURL)
	if err != nil {
		return err
	}
	src, err := newMigrationSource(migrationsPath, backend)
	if err != nil {
		return err
	}
	m, cleanup, err := newMigrator(backend, dbURL, src, create)
	if err != nil {
		_ = src.Close() // the migrator never took ownership; close the source here
		return err
	}
	defer cleanup()
	return fn(m)
}

// newMigrationSource reads the migrations folder through backend's view (migrate_view.go) as a
// golang-migrate source.
func newMigrationSource(migrationsPath string, backend Backend) (source.Driver, error) {
	view, err := newMigrationView(os.DirFS(migrationsPath), backend)
	if err != nil {
		return nil, err
	}
	src, err := iofs.New(view, ".")
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations: %w", err)
	}
	return src, nil
}

// newMigrator builds a migrator for backend over src and returns a cleanup that closes it (and, on
// SQLite, releases the migration lock). On error src is left for the caller to close.
func newMigrator(backend Backend, dbURL string, src source.Driver, create bool) (*migrate.Migrate, func(), error) {
	switch backend {
	case Postgres:
		m, err := migrate.NewWithSourceInstance("iofs", src, dbURL)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create migrator: %w", err)
		}
		return m, func() { closeMigrator(m) }, nil
	case SQLite:
		return newSQLiteMigrator(dbURL, src, create)
	default:
		return nil, nil, fmt.Errorf("database backend %q is not supported", backend)
	}
}

// newSQLiteMigrator opens a dedicated SQLite connection under the migration lock and builds a
// migrator over it. create tells whether a missing file is created (up/goto/reset) or an error
// (status/down/force), so a mistyped path is not stamped with a version.
func newSQLiteMigrator(dbURL string, src source.Driver, create bool) (*migrate.Migrate, func(), error) {
	path, err := sqlitePathFromURL(dbURL)
	if err != nil {
		return nil, nil, err
	}
	// A must-exist command on a missing file is an error, checked before lockMigrations creates
	// the directory: status/down/force must not conjure an empty database.
	if !create {
		if _, statErr := os.Stat(path); statErr != nil {
			return nil, nil, fmt.Errorf("SQLite database %s: %w", path, statErr)
		}
	}
	unlock, err := lockMigrations(path)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := openSQLiteHandle(ctx, path, defaultSQLiteTxLock, !create, PoolConfig{MaxConns: 1, MinConns: 1})
	if err != nil {
		unlock()
		return nil, nil, err
	}
	drv, err := migratesqlite.WithInstance(db, &migratesqlite.Config{})
	if err != nil {
		_ = db.Close()
		unlock()
		return nil, nil, fmt.Errorf("failed to create migrator: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", drv)
	if err != nil {
		_ = drv.Close()
		unlock()
		return nil, nil, fmt.Errorf("failed to create migrator: %w", err)
	}
	return m, func() { closeMigrator(m); unlock() }, nil
}

// closeMigrator closes m, logging a close error (the run already succeeded or failed on its own).
func closeMigrator(m *migrate.Migrate) {
	if sourceErr, dbErr := m.Close(); sourceErr != nil || dbErr != nil {
		log.Error().Err(sourceErr).AnErr("db_err", dbErr).Msg("error closing migrator")
	}
}

// refuseNewerDatabase refuses a database whose version is above the newest migration in the
// folder. golang-migrate stops on one too, but as "no migration found for version N: read down
// …", which does not tell an operator who has just restored a backup what went wrong.
func refuseNewerDatabase(m *migrate.Migrate, dbURL, migrationsPath string) error {
	current, _, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get migration version: %w", err)
	}
	backend, err := ParseURL(dbURL)
	if err != nil {
		return err
	}
	return refuseNewerVersion(uint64(current), migrationsPath, backend)
}

// refuseNewerVersion refuses a database version above the newest migration in backend's view of the
// folder, naming both: the database was written by a newer Lightstack (SPEC-030 FR-008).
func refuseNewerVersion(version uint64, migrationsPath string, backend Backend) error {
	view, err := newMigrationView(os.DirFS(migrationsPath), backend)
	if err != nil {
		return err
	}
	if latest := view.latestVersion(); version > latest {
		return fmt.Errorf("the database is at migration version %d, but this Lightstack knows versions up to %d: "+
			"it was written by a newer Lightstack. Run that version, or restore a backup this one wrote", version, latest)
	}
	return nil
}

// GetMigrationStatus returns the current migration version, dirty state and database state. For a
// database migrated before the squash it returns both the info and the *PreSquashError, without
// opening a migrator, so status can show the version and say why it cannot go on (FR-011, FR-022).
func GetMigrationStatus(dbURL, migrationsPath string) (*MigrationInfo, error) {
	in, err := inspectDatabase(dbURL, migrationsPath)
	if err != nil {
		return nil, err
	}
	switch {
	case in.state == StatePreSquash:
		// golang-migrate would read its version against the restarted numbering; the version and the
		// refusal are all there is to report.
		return &MigrationInfo{Version: uint(in.version), Dirty: in.dirty}, in.refusal()
	case in.missing != "":
		// status opens must-exist and creates nothing; it reports the file it did not find.
		return &MigrationInfo{}, &MissingDatabaseError{Path: in.missing}
	}
	var info *MigrationInfo
	err = withMigrator(dbURL, migrationsPath, false, func(m *migrate.Migrate) error {
		version, dirty, err := m.Version()
		if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
			return fmt.Errorf("failed to get migration version: %w", err)
		}
		info = &MigrationInfo{Version: version, Dirty: dirty}
		return nil
	})
	if err != nil {
		return info, err
	}
	view, err := newMigrationView(os.DirFS(migrationsPath), in.backend)
	if err != nil {
		return info, err
	}
	info.Kind = view.kind(uint64(info.Version))
	// A stale database is reported with its version, what that version is, and the refusal.
	return info, in.refusal()
}

// MigrateUp applies all pending migrations. A database newer than every migration this binary
// carries — one restored from a newer Lightstack's backup — is refused by name (SPEC-030 FR-008).
func MigrateUp(dbURL, migrationsPath string) error {
	if err := CheckDatabaseState(dbURL, migrationsPath); err != nil {
		return err
	}
	backend, err := ParseURL(dbURL)
	if err != nil {
		return err
	}
	open, err := assembleOpen(os.DirFS(migrationsPath), backend)
	if err != nil {
		return err
	}
	return withMigrator(dbURL, migrationsPath, true, func(m *migrate.Migrate) error {
		if err := refuseNewerDatabase(m, dbURL, migrationsPath); err != nil {
			return err
		}
		log.Info().Msg("Running database migrations...")
		if err := m.Up(); err != nil {
			if errors.Is(err, migrate.ErrNoChange) {
				log.Info().Msg("Database is already up to date")
				return nil
			}
			return fmt.Errorf("failed to run migrations: %w", err)
		}
		version, _, _ := m.Version()
		log.Info().Uint("version", version).Msg("Migrations applied successfully")
		// A build between releases applied fragments no release has frozen yet (FR-018): when they
		// change, the database has to be recreated.
		if open != nil && uint64(version) == open.version {
			log.Warn().Uint("version", version).Msgf("applied the open release migration %03d (unreleased)", version)
		}
		return nil
	})
}

// MigrateDown rolls back one migration. The last step down runs the baseline's down file and
// leaves an empty database.
func MigrateDown(dbURL, migrationsPath string) error {
	if err := CheckDatabaseState(dbURL, migrationsPath); err != nil {
		return err
	}
	return withMigrator(dbURL, migrationsPath, false, func(m *migrate.Migrate) error {
		currentVersion, _, err := m.Version()
		if err != nil {
			if errors.Is(err, migrate.ErrNilVersion) {
				return fmt.Errorf("no migrations to roll back")
			}
			return fmt.Errorf("failed to get current version: %w", err)
		}
		log.Warn().Uint("from_version", currentVersion).Msg("Rolling back one migration")
		if err := m.Steps(-1); err != nil {
			return fmt.Errorf("failed to rollback migration: %w", err)
		}
		newVersion, _, _ := m.Version()
		log.Info().Uint("to_version", newVersion).Msg("Migration rolled back successfully")
		return nil
	})
}

// MigrateToVersion migrates to a specific version.
func MigrateToVersion(dbURL, migrationsPath string, targetVersion uint) error {
	if err := CheckDatabaseState(dbURL, migrationsPath); err != nil {
		return err
	}
	return withMigrator(dbURL, migrationsPath, true, func(m *migrate.Migrate) error {
		currentVersion, _, err := m.Version()
		if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
			return fmt.Errorf("failed to get current version: %w", err)
		}
		log.Info().
			Uint("current_version", currentVersion).
			Uint("target_version", targetVersion).
			Msg("Migrating to specific version")
		if err := m.Migrate(targetVersion); err != nil {
			if errors.Is(err, migrate.ErrNoChange) {
				log.Info().Msg("Already at target version")
				return nil
			}
			return fmt.Errorf("failed to migrate to version %d: %w", targetVersion, err)
		}
		log.Info().Uint("version", targetVersion).Msg("Migrated to target version")
		return nil
	})
}

// MigrateReset empties the database and re-runs all migrations. On PostgreSQL golang-migrate's
// Drop removes every table, as before. On SQLite that drop is unusable — it would try to drop
// SQLite's own sqlite_sequence and drops tables in name order, parents before the tables that
// reference them — and m.Down() refuses to run on a dirty database, which is exactly the state a
// reset exists to recover from. So the SQLite schema is emptied directly: every table, view and
// trigger that is not SQLite's own is dropped (including schema_migrations, so the re-run starts
// from the baseline), with foreign keys off so drop order does not matter.
func MigrateReset(dbURL, migrationsPath string) error {
	backend, err := ParseURL(dbURL)
	if err != nil {
		return err
	}
	log.Warn().Msg("Resetting database - this will DROP ALL TABLES")
	if backend == SQLite {
		if err := emptySQLiteSchema(dbURL); err != nil {
			return err
		}
	} else if err := withMigrator(dbURL, migrationsPath, true, func(m *migrate.Migrate) error {
		if err := m.Drop(); err != nil {
			return fmt.Errorf("failed to drop database: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	log.Info().Msg("Database emptied, re-running migrations")
	return MigrateUp(dbURL, migrationsPath)
}

// emptySQLiteSchema drops every table, view and trigger of the SQLite database at dbURL that is
// not SQLite's own, so a reset leaves an empty file that MigrateUp re-populates from the baseline.
// It holds the migration lock for the drop, and unlike m.Down() it works on a dirty database. A
// missing file is left for MigrateUp to create (reset is create-if-missing).
func emptySQLiteSchema(dbURL string) error {
	path, err := sqlitePathFromURL(dbURL)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil // nothing to empty; MigrateUp creates the file
	} else if err != nil {
		return fmt.Errorf("SQLite database %s: %w", path, err)
	}
	unlock, err := lockMigrations(path)
	if err != nil {
		return err
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := openSQLiteHandle(ctx, path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1, MinConns: 1})
	if err != nil {
		return err
	}
	defer db.Close()
	return dropSQLiteObjects(ctx, db)
}

// dropSQLiteObjects drops every table, view and trigger that is not SQLite's own (the sqlite_%
// names it reserves). Foreign keys are disabled so parents can be dropped before the tables that
// reference them; the objects are listed before any is dropped, since dropping while iterating
// sqlite_schema would disturb the cursor.
func dropSQLiteObjects(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("disabling foreign keys for reset: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT type, name FROM sqlite_schema
		WHERE type IN ('table', 'view', 'trigger') AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return fmt.Errorf("listing objects to drop: %w", err)
	}
	type object struct{ kind, name string }
	var objects []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.kind, &o.name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("listing objects to drop: %w", err)
		}
		objects = append(objects, o)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("listing objects to drop: %w", err)
	}
	for _, o := range objects {
		// kind is SQLite's own object type; name comes from sqlite_schema. IF EXISTS tolerates an
		// object a prior DROP already took (a table drops its own triggers and indexes).
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP %s IF EXISTS "%s"`, o.kind, o.name)); err != nil {
			return fmt.Errorf("dropping %s %s: %w", o.kind, o.name, err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return fmt.Errorf("re-enabling foreign keys after reset: %w", err)
	}
	return nil
}

// ForceMigrationVersion sets the migration version without running migrations. Use this
// to fix dirty state issues.
func ForceMigrationVersion(dbURL, migrationsPath string, version int) error {
	if err := CheckForceState(dbURL, migrationsPath); err != nil {
		return err
	}
	return withMigrator(dbURL, migrationsPath, false, func(m *migrate.Migrate) error {
		log.Warn().Int("version", version).Msg("Forcing migration version (use with caution)")
		if err := m.Force(version); err != nil {
			return fmt.Errorf("failed to force version: %w", err)
		}
		log.Info().Int("version", version).Msg("Migration version forced")
		return nil
	})
}

// ValidateMigrations checks the migrations folder (SPEC-032 contracts/migrations-folder.md): its
// numbered files follow the grammar — the baseline pair at 001, release migrations numbered
// consecutively from 002, nothing else (validateFolderNames) — and in each backend's view every up
// migration has its down migration and the reverse, with no version twice.
func ValidateMigrations(migrationsPath string) error {
	if _, err := os.Stat(migrationsPath); os.IsNotExist(err) {
		return fmt.Errorf("migrations directory does not exist: %s", migrationsPath)
	}
	src := os.DirFS(migrationsPath)
	if err := validateFolderNames(src); err != nil {
		return err
	}
	if _, err := readFragments(src); err != nil {
		return err
	}
	for _, b := range []Backend{Postgres, SQLite} {
		view, err := newMigrationView(src, b)
		if err != nil {
			return err
		}
		n, err := validateView(view, b)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no migration files found in %s", migrationsPath)
		}
		log.Info().Str("backend", string(b)).Int("migrations", n).Str("path", migrationsPath).Msg("Migration files validated")
	}
	return nil
}
