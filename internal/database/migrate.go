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

// MigrationInfo contains migration status information
type MigrationInfo struct {
	Version uint
	Dirty   bool
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

// RefuseBelowBaseline refuses a SQLite target version below the baseline, where the SQLite schema
// does not exist (contracts/migrations.md). Exported so lightstack-migrate can refuse goto/force
// before it prompts for a destructive confirmation.
func RefuseBelowBaseline(dbURL, migrationsPath string, version int) error {
	if backend, err := ParseURL(dbURL); err != nil || backend != SQLite {
		return err
	}
	baseline, err := baselineVersion(os.DirFS(migrationsPath))
	if err != nil {
		return err
	}
	if version >= 0 && uint64(version) < baseline {
		return fmt.Errorf("SQLite starts at its baseline %03d; version %d is below it", baseline, version)
	}
	return nil
}

// GetMigrationStatus returns current migration version and dirty state
func GetMigrationStatus(dbURL, migrationsPath string) (*MigrationInfo, error) {
	var info *MigrationInfo
	err := withMigrator(dbURL, migrationsPath, false, func(m *migrate.Migrate) error {
		version, dirty, err := m.Version()
		if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
			return fmt.Errorf("failed to get migration version: %w", err)
		}
		info = &MigrationInfo{Version: version, Dirty: dirty}
		return nil
	})
	return info, err
}

// MigrateUp applies all pending migrations
func MigrateUp(dbURL, migrationsPath string) error {
	return withMigrator(dbURL, migrationsPath, true, func(m *migrate.Migrate) error {
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
		return nil
	})
}

// MigrateDown rolls back one migration. On SQLite the last step down runs the baseline's
// down file and leaves an empty database.
func MigrateDown(dbURL, migrationsPath string) error {
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

// MigrateToVersion migrates to a specific version. On SQLite a version below the
// baseline is refused.
func MigrateToVersion(dbURL, migrationsPath string, targetVersion uint) error {
	if err := RefuseBelowBaseline(dbURL, migrationsPath, int(targetVersion)); err != nil {
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
// to fix dirty state issues. On SQLite a version below the baseline is refused.
func ForceMigrationVersion(dbURL, migrationsPath string, version int) error {
	if err := RefuseBelowBaseline(dbURL, migrationsPath, version); err != nil {
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

// ValidateMigrations checks both backends' views of the migrations folder
// (contracts/migrations.md): every up migration has its down migration and the reverse,
// the folder holds exactly one SQLite baseline, and no version appears twice in a view.
func ValidateMigrations(migrationsPath string) error {
	if _, err := os.Stat(migrationsPath); os.IsNotExist(err) {
		return fmt.Errorf("migrations directory does not exist: %s", migrationsPath)
	}
	src := os.DirFS(migrationsPath)
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
