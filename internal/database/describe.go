package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Description is what the startup log line reports about the database (FR-013a).
type Description struct {
	Backend       Backend
	Location      string // PostgreSQL host:port/database or the SQLite file path; never credentials
	SchemaVersion uint
	// SQLite only, read back from a pooled connection:
	JournalMode   string
	Synchronous   string
	ForeignKeys   bool
	BusyTimeoutMS int
	TxLock        string
}

// Describe reads what the startup log line reports about db, opened from rawURL.
func Describe(ctx context.Context, db DB, rawURL string) (Description, error) {
	d := Description{Backend: db.Backend(), Location: location(rawURL)}
	var v int64
	switch err := db.QueryRow(ctx, `SELECT version FROM schema_migrations LIMIT 1`).Scan(&v); {
	case errors.Is(err, ErrNoRows), isMissingMigrationsTable(err):
		// Not migrated yet — the table is absent or empty — so the version stays 0. Only a
		// genuine read failure (connection, permission) is surfaced below.
	case err != nil:
		return d, fmt.Errorf("read the schema version: %w", err)
	case v > 0:
		d.SchemaVersion = uint(v)
	}
	if sq, ok := db.(*sqliteDB); ok {
		d.Location = sq.path // the real opened file, already absolute and cleaned
		s, err := readSQLiteSettings(ctx, sq.db)
		if err != nil {
			return d, err
		}
		d.JournalMode = s.journalMode
		d.Synchronous = map[int]string{0: "off", 1: "normal", 2: "full", 3: "extra"}[s.synchronous]
		d.ForeignKeys = s.foreignKeys == 1
		d.BusyTimeoutMS = s.busyTimeoutMS
		d.TxLock = sq.txlock
	}
	return d, nil
}

// isMissingMigrationsTable reports that err is a reference to a schema_migrations table that does
// not exist — the state before the first migration, which Describe reads as version 0 rather than a
// failure. PostgreSQL reports it with SQLSTATE 42P01; SQLite's "no such table" carries no distinct
// result code, so it is matched by its (fixed, English) message under a SQLite error.
func isMissingMigrationsTable(err error) bool {
	return pgCode(err) == "42P01" || (sqliteCode(err) != 0 && strings.Contains(err.Error(), "no such table"))
}

// location returns where the database is, without credentials: host:port/database for
// PostgreSQL, the file path for SQLite.
func location(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if strings.EqualFold(u.Scheme, "sqlite") {
		return u.Path
	}
	host := u.Host
	if u.Port() == "" && host != "" {
		host = net.JoinHostPort(u.Hostname(), "5432")
	}
	return host + u.Path
}
