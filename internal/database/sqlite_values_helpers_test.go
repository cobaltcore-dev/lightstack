package database

import (
	"database/sql"
	"sync"
	"testing"
)

var (
	memOnce sync.Once
	memDB   *sql.DB
)

// scanInto scans src, as SQLite returns it to database/sql, into dest through the
// SQLite implementation's conversion — the same path sqliteRows.Scan takes.
func scanInto(src any, dest any) error {
	memOnce.Do(func() {
		var err error
		if memDB, err = sql.Open("sqlite", "file::memory:"); err != nil {
			panic(err)
		}
		memDB.SetMaxOpenConns(1)
	})
	rows, err := memDB.Query("SELECT ?", src)
	if err != nil {
		return err
	}
	defer rows.Close()
	rows.Next()
	return scanConverted(rows.Scan, []any{dest})
}

var _ = testing.Short
