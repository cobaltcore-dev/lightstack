package database_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// TestValuesRoundTrip pins US1 AS6 and FR-019 – FR-023: the same Go values written and
// read back through the contract come back equal on both backends.
func TestValuesRoundTrip(t *testing.T) {
	db := openLeg(t)
	ctx := context.Background()
	table := "dbtest_roundtrip_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	ddl := map[database.Backend]string{
		database.Postgres: `CREATE TABLE %s (id UUID PRIMARY KEY, at TIMESTAMP NOT NULL, doc JSONB, tags TEXT[], flag BOOLEAN NOT NULL, note TEXT)`,
		database.SQLite:   `CREATE TABLE %s (id TEXT PRIMARY KEY, at TEXT NOT NULL, doc TEXT, tags TEXT, flag INTEGER NOT NULL, note TEXT) STRICT`,
	}[db.Backend()]
	if _, err := db.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(ctx, fmt.Sprintf(ddl, table)); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)) })

	id := "3f1b4b52-4d3a-4f0e-9a51-0d6a0f6b1a01"
	at := time.Date(2026, 10, 9, 8, 15, 30, 123456789, time.UTC)
	doc := map[string]any{"enable_snat": false, "network_id": "n-1"}
	tags := []string{"10.0.0.0/8", "192.0.2.1"}
	if _, err := db.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, at, doc, tags, flag, note) VALUES ($1, $2, $3, $4, $5, $6)`, table),
		id, at, doc, tags, true, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var (
		gotID   string
		gotAt   time.Time
		gotDoc  map[string]any
		gotTags []string
		gotFlag bool
		gotNote *string
	)
	err := db.QueryRow(ctx, fmt.Sprintf(`SELECT id::text, at, doc, tags, flag, note FROM %s WHERE id::text = $1`, table), id).
		Scan(&gotID, &gotAt, &gotDoc, &gotTags, &gotFlag, &gotNote)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if gotID != id || !gotAt.Equal(at.Truncate(time.Microsecond)) || !reflect.DeepEqual(gotDoc, doc) ||
		!reflect.DeepEqual(gotTags, tags) || !gotFlag || gotNote != nil {
		t.Fatalf("round trip on %s:\n id=%q at=%v doc=%v tags=%v flag=%v note=%v", db.Backend(), gotID, gotAt, gotDoc, gotTags, gotFlag, gotNote)
	}

	// String order is time order on SQLite only because every timestamp has one width
	// and one zone (FR-019); the comparison below holds on both backends.
	var later int
	if err := db.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE at > $1`, table), at.Add(-time.Second)).Scan(&later); err != nil || later != 1 {
		t.Fatalf("time comparison = %d, %v; want 1", later, err)
	}
}

// TestNullLimitIsNoLimit pins the pagination idiom of the list handlers: a nil limit
// binds LIMIT NULL, which PostgreSQL reads as no limit and SQLite would refuse.
func TestNullLimitIsNoLimit(t *testing.T) {
	db := openLeg(t)
	ctx := context.Background()
	rows, err := db.Query(ctx, `SELECT n FROM (SELECT 1 AS n UNION ALL SELECT 2 UNION ALL SELECT 3) AS t ORDER BY n LIMIT $1 OFFSET $2`, nil, 1)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("LIMIT NULL OFFSET 1 on %s = %v; want [2 3]", db.Backend(), got)
	}
}
