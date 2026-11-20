package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	migratedb "github.com/golang-migrate/migrate/v4/database"
)

// noTransactionMarker, as the first line of a SQLite migration file, has the migrator run that
// file with foreign keys off (migrations/README.md, SPEC-033 research R53). It exists for one job:
// rebuilding a table that other tables reference. SQLite's DROP TABLE deletes the referencing rows
// through every ON DELETE CASCADE while foreign keys are on, and PRAGMA foreign_keys cannot change
// inside a transaction — which is where the migrator runs every other file. PostgreSQL never reads
// it: there it is a comment.
const noTransactionMarker = "-- lightstack:no-transaction"

// sqliteMigrationDriver is golang-migrate's SQLite driver with one change: a file marked with
// noTransactionMarker runs as SQLite's documented table-rebuild procedure has it — foreign keys
// turned off outside any transaction, the file in a transaction of the migrator's own, PRAGMA
// foreign_key_check run before that transaction commits and refusing the file if it finds a row
// that references nothing, then foreign keys on again. So a marked file is still all or nothing,
// and it cannot leave a dangling reference behind. Every other file runs exactly as before.
type sqliteMigrationDriver struct {
	migratedb.Driver
	db *sql.DB
}

// Run runs one migration file.
func (d *sqliteMigrationDriver) Run(migration io.Reader) error {
	body, err := io.ReadAll(migration)
	if err != nil {
		return err
	}
	if !hasNoTransactionMarker(body) {
		return d.Driver.Run(bytes.NewReader(body))
	}
	return runWithForeignKeysOff(context.Background(), d.db, string(body))
}

// hasNoTransactionMarker reports whether the file's first line is the marker.
func hasNoTransactionMarker(body []byte) bool {
	first, _, _ := bytes.Cut(body, []byte("\n"))
	return strings.TrimSpace(string(first)) == noTransactionMarker
}

// runWithForeignKeysOff runs query on one connection of db with foreign keys off, in a
// transaction that commits only if PRAGMA foreign_key_check finds nothing. Foreign keys are on
// again before the connection goes back to the migrator.
func runWithForeignKeysOff(ctx context.Context, db *sql.DB, query string) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration without the migrator's transaction: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("turning foreign keys off for the migration: %w", err)
	}
	defer func() {
		if _, onErr := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); onErr != nil {
			err = errors.Join(err, fmt.Errorf("turning foreign keys back on after the migration: %w", onErr))
		}
	}()
	// PRAGMA foreign_keys is silently a no-op inside a transaction; a connection that is in one
	// here would run the file with foreign keys on and lose the rows it exists to keep.
	var on int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on); err != nil {
		return fmt.Errorf("reading foreign_keys before the migration: %w", err)
	}
	if on != 0 {
		return errors.New("foreign keys are still on; the migration was not run")
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return &migratedb.Error{OrigErr: err, Err: "transaction start failed"}
	}
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return errors.Join(&migratedb.Error{OrigErr: err, Query: []byte(query)}, rollback(tx))
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return errors.Join(err, rollback(tx))
	}
	if err := tx.Commit(); err != nil {
		return &migratedb.Error{OrigErr: err, Err: "transaction commit failed"}
	}
	return nil
}

func rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("rolling the migration back: %w", err)
	}
	return nil
}

// checkForeignKeys runs PRAGMA foreign_key_check and returns an error naming every row it finds:
// a row whose foreign key names a parent row that does not exist.
func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("PRAGMA foreign_key_check: %w", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("PRAGMA foreign_key_check: %w", err)
		}
		row := "a row"
		if rowid.Valid {
			row = fmt.Sprintf("row %d", rowid.Int64)
		}
		found = append(found, fmt.Sprintf("%s %s references a missing %s row (foreign key %d)", table, row, parent, fkid))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("PRAGMA foreign_key_check: %w", err)
	}
	if len(found) > 0 {
		return fmt.Errorf("PRAGMA foreign_key_check after the migration found %d dangling references: %s",
			len(found), strings.Join(found, "; "))
	}
	return nil
}
