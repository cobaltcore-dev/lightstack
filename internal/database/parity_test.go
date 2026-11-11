package database

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSchemaParity pins FR-028 (research R11): a PostgreSQL database and a SQLite file,
// each migrated from the one migrations folder, hold the same schema and seed data —
// tables, columns, nullability, defaults, keys with their delete actions, CHECKs, indexes
// with their predicates, and seed rows. The GIN index over image properties has no SQLite
// counterpart and is the one documented exception (skippedIndexes).
func TestSchemaParity(t *testing.T) {
	admin := adminPostgresURL(t)
	ctx := context.Background()
	source := migrationsSource(t, math.MaxUint64)

	pgURL := scratchPostgres(t, admin, "lightstack_parity_"+strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	if err := MigrateUp(pgURL, "../../migrations"); err != nil {
		t.Fatalf("migrate PostgreSQL: %v", err)
	}
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		t.Fatal(err)
	}
	pgTables, err := introspectPostgres(ctx, conn)
	conn.Close(ctx)
	if err != nil {
		t.Fatalf("introspect PostgreSQL: %v", err)
	}
	pg, err := postgresModel(pgTables, source)
	if err != nil {
		t.Fatalf("model PostgreSQL: %v", err)
	}

	path := filepath.Join(t.TempDir(), "parity.db")
	if err := MigrateUp("sqlite://"+path, "../../migrations"); err != nil {
		t.Fatalf("migrate SQLite: %v", err)
	}
	db, err := openSQLiteHandle(ctx, path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sq, err := sqliteModel(ctx, db, source)
	if err != nil {
		t.Fatalf("model SQLite: %v", err)
	}

	if diffs := diffModels(pg, sq, "postgres", "sqlite"); len(diffs) > 0 {
		t.Errorf("the backends' schemas differ in %d places:\n%s", len(diffs), strings.Join(diffs, "\n"))
	}
	if len(pg) < 50 {
		t.Errorf("%d tables compared; the comparison read nothing", len(pg))
	}
}
