package database_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// openPostgres opens the PostgreSQL test database, or skips when the test leg is not
// PostgreSQL. These tests exercise the contract's PostgreSQL implementation directly, so
// they never run against SQLite.
func openPostgres(t *testing.T) database.DB {
	t.Helper()
	url := os.Getenv("LIGHTSTACK_TEST_DB_URL")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("set LIGHTSTACK_TEST_DB_URL to a postgres:// URL to run the PostgreSQL contract tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := database.Open(ctx, url, database.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// scratchTables creates a parent/child pair with every constraint class the error
// classifiers recognise, under names unique to the test, and drops them afterwards.
func scratchTables(t *testing.T, db database.DB) (parent, child string) {
	t.Helper()
	ctx := context.Background()
	suffix := strings.ReplaceAll(strings.ToLower(t.Name()), "/", "_")
	parent, child = "dbtest_parent_"+suffix, "dbtest_child_"+suffix
	stmts := []string{
		fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, child, parent),
		fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, size INTEGER CHECK (size >= 0))`, parent),
		fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES %s(id))`, child, parent),
	}
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s, %s`, child, parent))
	})
	return parent, child
}

func TestPostgresQueryRowNoRowsIsErrNoRows(t *testing.T) {
	db := openPostgres(t)
	var n int
	err := db.QueryRow(context.Background(), `SELECT 1 WHERE false`).Scan(&n)
	if !errors.Is(err, database.ErrNoRows) {
		t.Fatalf("Scan on no rows = %v; want database.ErrNoRows", err)
	}
	if !errors.Is(fmt.Errorf("lookup: %w", err), database.ErrNoRows) {
		t.Fatal("a wrapped ErrNoRows is no longer recognised")
	}
}

func TestPostgresExecRowsAffected(t *testing.T) {
	db := openPostgres(t)
	parent, _ := scratchTables(t, db)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, err := db.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, name, size) VALUES ($1, $2, 1)`, parent), i, fmt.Sprintf("n%d", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	res, err := db.Exec(ctx, fmt.Sprintf(`UPDATE %s SET size = size + 1 WHERE id >= $1`, parent), 2)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := res.RowsAffected(); got != 2 {
		t.Fatalf("RowsAffected = %d; want 2", got)
	}
}

func TestPostgresQueryIteratesRows(t *testing.T) {
	db := openPostgres(t)
	rows, err := db.Query(context.Background(), `SELECT g FROM generate_series(1, 3) AS g ORDER BY g`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var g int
		if err := rows.Scan(&g); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("rows = %v; want [1 2 3]", got)
	}
}

func TestPostgresTransactionCommitAndRollback(t *testing.T) {
	db := openPostgres(t)
	parent, _ := scratchTables(t, db)
	ctx := context.Background()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, name) VALUES (1, 'rolled-back')`, parent)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, name) VALUES (2, 'committed')`, parent)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var inside int
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, parent)).Scan(&inside); err != nil || inside != 1 {
		t.Fatalf("count inside the transaction = %d, %v; want 1", inside, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var names []string
	rows, err := db.Query(ctx, fmt.Sprintf(`SELECT name FROM %s ORDER BY id`, parent))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	if fmt.Sprint(names) != "[committed]" {
		t.Fatalf("rows after commit and rollback = %v; want [committed]", names)
	}
}

func TestPostgresWithTxRollsBackOnError(t *testing.T) {
	db := openPostgres(t)
	parent, _ := scratchTables(t, db)
	ctx := context.Background()
	boom := errors.New("boom")
	err := database.WithTx(ctx, db, func(tx database.Tx) error {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, name) VALUES (1, 'x')`, parent)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx = %v; want the closure's error", err)
	}
	var n int
	if err := db.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, parent)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after a failed WithTx = %d, %v; want 0", n, err)
	}
}

func TestPostgresErrorClasses(t *testing.T) {
	db := openPostgres(t)
	parent, child := scratchTables(t, db)
	ctx := context.Background()
	if _, err := db.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, name, size) VALUES (1, 'a', 1)`, parent)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cases := []struct {
		name string
		sql  string
		is   func(error) bool
	}{
		{"unique (primary key)", fmt.Sprintf(`INSERT INTO %s (id, name) VALUES (1, 'b')`, parent), database.IsUniqueViolation},
		{"unique (constraint)", fmt.Sprintf(`INSERT INTO %s (id, name) VALUES (2, 'a')`, parent), database.IsUniqueViolation},
		{"foreign key", fmt.Sprintf(`INSERT INTO %s (id, parent_id) VALUES (1, 99)`, child), database.IsForeignKeyViolation},
		{"not null", fmt.Sprintf(`INSERT INTO %s (id, name) VALUES (3, NULL)`, parent), database.IsNotNullViolation},
		{"check", fmt.Sprintf(`INSERT INTO %s (id, name, size) VALUES (4, 'c', -1)`, parent), database.IsCheckViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(ctx, tc.sql)
			if err == nil {
				t.Fatal("statement succeeded; want a constraint violation")
			}
			if !tc.is(fmt.Errorf("wrapped: %w", err)) {
				t.Fatalf("classifier did not recognise %v", err)
			}
		})
	}
}

func TestPostgresHealthCheck(t *testing.T) {
	db := openPostgres(t)
	if err := database.HealthCheck(context.Background(), db); err != nil {
		t.Fatalf("HealthCheck = %v", err)
	}
	if got := db.Backend(); got != database.Postgres {
		t.Fatalf("Backend = %q; want %q", got, database.Postgres)
	}
}

// TestPostgresStatementsPassThrough pins FR-003: PostgreSQL receives statements as
// written. A row lock still blocks a second transaction, and an array argument still
// reaches `= ANY($1::uuid[])` as an array.
func TestPostgresStatementsPassThrough(t *testing.T) {
	db := openPostgres(t)
	parent, _ := scratchTables(t, db)
	ctx := context.Background()
	if _, err := db.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, name) VALUES (1, 'locked')`, parent)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	holder, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var id int
	if err := holder.QueryRow(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE id = 1 FOR UPDATE`, parent)).Scan(&id); err != nil {
		t.Fatalf("lock: %v", err)
	}

	waiter, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = waiter.Rollback(ctx) }()
	if _, err := waiter.Exec(ctx, `SET LOCAL lock_timeout = '200ms'`); err != nil {
		t.Fatalf("lock_timeout: %v", err)
	}
	_, err = waiter.Exec(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE id = 1 FOR UPDATE`, parent))
	if !database.IsBusy(err) {
		t.Fatalf("second FOR UPDATE = %v; want a lock timeout (the row lock reached PostgreSQL)", err)
	}

	ids := []string{"3f1b4b52-4d3a-4f0e-9a51-0d6a0f6b1a01", "3f1b4b52-4d3a-4f0e-9a51-0d6a0f6b1a02"}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM unnest($1::uuid[]) AS u WHERE u = ANY($1::uuid[])`, ids).Scan(&n); err != nil || n != 2 {
		t.Fatalf("= ANY($1::uuid[]) = %d, %v; want 2", n, err)
	}
}
