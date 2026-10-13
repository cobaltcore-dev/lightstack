package database_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// TestDescribe pins FR-013a: the startup log line names the backend, the location —
// never credentials — the schema version and, on SQLite, the settings read back.
func TestDescribe(t *testing.T) {
	db := openLeg(t)
	ctx := context.Background()
	url := "postgres://admin:s3cret@db.example.com:5432/lightstack?sslmode=disable"
	if db.Backend() == database.SQLite {
		url = "sqlite:///var/lib/lightstack/db/lightstack.db"
		if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER NOT NULL, dirty INTEGER NOT NULL)`); err != nil {
			t.Fatalf("schema_migrations: %v", err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO schema_migrations (version, dirty) VALUES (74, 0)`); err != nil {
			t.Fatalf("version: %v", err)
		}
	}
	d, err := database.Describe(ctx, db, url)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.Backend != db.Backend() {
		t.Fatalf("Backend = %q; want %q", d.Backend, db.Backend())
	}
	switch db.Backend() {
	case database.Postgres:
		if d.Location != "db.example.com:5432/lightstack" {
			t.Fatalf("Location = %q; want host:port/database without credentials", d.Location)
		}
	case database.SQLite:
		// Location is the real opened file (a temp leg.db here), not the display url passed in.
		if !strings.HasPrefix(d.Location, "/") || !strings.HasSuffix(d.Location, "leg.db") || d.SchemaVersion != 74 {
			t.Fatalf("Location = %q, SchemaVersion = %d", d.Location, d.SchemaVersion)
		}
		if d.JournalMode != "wal" || d.Synchronous != "full" || !d.ForeignKeys || d.BusyTimeoutMS <= 0 || d.TxLock == "" {
			t.Fatalf("SQLite settings = %+v", d)
		}
	}
}

// TestDescribeReportsZeroWhenNotMigrated: a database without a schema_migrations table is not
// migrated yet, which Describe reports as version 0 — not a failure. Only a genuine read error
// (connection, permission) surfaces, which the default branch returns. Exercised on SQLite, where
// a fresh leg has no schema_migrations table.
func TestDescribeReportsZeroWhenNotMigrated(t *testing.T) {
	db := openLeg(t)
	if db.Backend() != database.SQLite {
		t.Skip("exercised on SQLite, where a fresh leg has no schema_migrations table")
	}
	d, err := database.Describe(context.Background(), db, "sqlite:///var/lib/lightstack/db/lightstack.db")
	if err != nil {
		t.Fatalf("Describe on a not-yet-migrated database: %v; want version 0, no error", err)
	}
	if d.SchemaVersion != 0 {
		t.Fatalf("SchemaVersion = %d; want 0 before any migration", d.SchemaVersion)
	}
}
