package database

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SPEC-033 round 2, T061 (research R53): a SQLite migration file whose first line is
// "-- lightstack:no-transaction" runs with foreign keys off, so it can rebuild a table other
// tables reference without SQLite's DROP TABLE deleting their rows through ON DELETE CASCADE.
// PRAGMA foreign_keys cannot change inside a transaction, so the migrator turns them off first;
// PRAGMA foreign_key_check must find nothing before the file's work is kept, and foreign keys
// are on again for whatever runs next. Every other file runs as before.

const notxMarker = "-- lightstack:no-transaction"

// notxBaseline is a SQLite baseline with a parent table that a child references.
var notxBaseline = map[string]string{
	"001_baseline.sqlite.up.sql": `
CREATE TABLE parent (id TEXT PRIMARY KEY, name TEXT NOT NULL, UNIQUE (name)) STRICT;
CREATE TABLE child (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL REFERENCES parent(id) ON DELETE CASCADE) STRICT;
INSERT INTO parent (id, name) VALUES ('p1', 'a'), ('p2', 'b');
INSERT INTO child (id, parent_id) VALUES ('c1', 'p1'), ('c2', 'p2');
`,
	"001_baseline.sqlite.down.sql": "DROP TABLE child; DROP TABLE parent;",
}

// notxRebuild rebuilds parent without its UNIQUE (name), SQLite's way: new table, copy, drop,
// rename. Run in a transaction with foreign keys on, the DROP would delete every child row.
const notxRebuild = `
CREATE TABLE parent_new (id TEXT PRIMARY KEY, name TEXT NOT NULL) STRICT;
INSERT INTO parent_new (id, name) SELECT id, name FROM parent;
DROP TABLE parent;
ALTER TABLE parent_new RENAME TO parent;
`

func notxFolder(t *testing.T, files map[string]string) (dir, url, path string) {
	t.Helper()
	all := map[string]string{}
	for k, v := range notxBaseline {
		all[k] = v
	}
	for k, v := range files {
		all[k] = v
	}
	dir = writeMigrations(t, all)
	path = filepath.Join(t.TempDir(), "notx.db")
	return dir, "sqlite://" + path, path
}

func openNotx(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := openSQLiteHandle(context.Background(), path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func notxCount(t *testing.T, path, query string) int {
	t.Helper()
	db := openNotx(t, path)
	defer db.Close()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestNoTransactionMarkerRebuildsAReferencedTable(t *testing.T) {
	dir, url, path := notxFolder(t, map[string]string{
		"002_rebuild.sqlite.up.sql":   notxMarker + "\n" + notxRebuild,
		"002_rebuild.sqlite.down.sql": notxMarker + "\n" + strings.ReplaceAll(notxRebuild, "name TEXT NOT NULL)", "name TEXT NOT NULL, UNIQUE (name))"),
	})
	if err := MigrateUp(url, dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	if n := notxCount(t, path, `SELECT count(*) FROM child`); n != 2 {
		t.Fatalf("%d child rows after the rebuild; want both kept (foreign keys off while it ran)", n)
	}
	if n := notxCount(t, path, `SELECT count(*) FROM parent`); n != 2 {
		t.Fatalf("%d parent rows after the rebuild; want 2", n)
	}
	// The rebuild took effect: the name is no longer unique.
	db := openNotx(t, path)
	if _, err := db.Exec(`INSERT INTO parent (id, name) VALUES ('p3', 'a')`); err != nil {
		t.Errorf("a repeated name after the rebuild: %v; want it accepted", err)
	}
	db.Close()

	// Down rebuilds it again, the same way, and keeps the children too.
	db = openNotx(t, path)
	if _, err := db.Exec(`DELETE FROM parent WHERE id = 'p3'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := MigrateDown(url, dir); err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := notxCount(t, path, `SELECT count(*) FROM child`); n != 2 {
		t.Fatalf("%d child rows after the down rebuild; want both kept", n)
	}
}

func TestNoTransactionMarkerTurnsForeignKeysBackOn(t *testing.T) {
	dir, url, path := notxFolder(t, map[string]string{
		"002_rebuild.sqlite.up.sql":   notxMarker + "\n" + notxRebuild,
		"002_rebuild.sqlite.down.sql": notxMarker + "\n" + notxRebuild,
		// A plain file after it, on the migrator's same connection: with foreign keys on again,
		// a child naming a missing parent is refused.
		"003_orphan.sqlite.up.sql":   `INSERT INTO child (id, parent_id) VALUES ('c3', 'missing');`,
		"003_orphan.sqlite.down.sql": `DELETE FROM child WHERE id = 'c3';`,
	})
	err := MigrateUp(url, dir)
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("up through a file inserting an orphan = %v; want a foreign-key failure, since foreign keys are on again", err)
	}
	if n := notxCount(t, path, `SELECT count(*) FROM child WHERE id = 'c3'`); n != 0 {
		t.Errorf("the orphan was stored; want it refused")
	}
}

func TestNoTransactionMarkerChecksForeignKeys(t *testing.T) {
	dir, url, path := notxFolder(t, map[string]string{
		// With foreign keys off, deleting p1 leaves c1 without its parent.
		"002_orphans.sqlite.up.sql":   notxMarker + "\nDELETE FROM parent WHERE id = 'p1';\n",
		"002_orphans.sqlite.down.sql": notxMarker + "\nSELECT 1;\n",
	})
	err := MigrateUp(url, dir)
	if err == nil {
		t.Fatal("up through a file that leaves a foreign-key violation succeeded; want it refused")
	}
	for _, want := range []string{"foreign_key_check", "child", "parent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	// Nothing the file did is kept: p1 is still there, and so is its child.
	if n := notxCount(t, path, `SELECT count(*) FROM parent WHERE id = 'p1'`); n != 1 {
		t.Errorf("p1 is gone after the refused migration; want the file's work rolled back")
	}
	if n := notxCount(t, path, `SELECT count(*) FROM child`); n != 2 {
		t.Errorf("%d child rows after the refused migration; want 2", n)
	}
}

// Files without the marker run as before: in a transaction, with foreign keys on (lock).
func TestMigrationsWithoutTheMarkerRunInATransaction(t *testing.T) {
	t.Run("a failing file leaves nothing", func(t *testing.T) {
		dir, url, path := notxFolder(t, map[string]string{
			"002_half.sqlite.up.sql":   "CREATE TABLE half (id TEXT) STRICT;\nINSERT INTO no_such_table (id) VALUES ('x');\n",
			"002_half.sqlite.down.sql": "DROP TABLE half;",
		})
		if err := MigrateUp(url, dir); err == nil {
			t.Fatal("up through a failing file succeeded")
		}
		if n := notxCount(t, path, `SELECT count(*) FROM sqlite_schema WHERE name = 'half'`); n != 0 {
			t.Errorf("the failing file's first statement was kept; want the whole file rolled back")
		}
	})
	t.Run("foreign keys are on, so a drop cascades", func(t *testing.T) {
		// The hazard migrations/README.md warns of: the same rebuild without the marker loses
		// every child row.
		dir, url, path := notxFolder(t, map[string]string{
			"002_rebuild.sqlite.up.sql":   notxRebuild,
			"002_rebuild.sqlite.down.sql": notxRebuild,
		})
		if err := MigrateUp(url, dir); err != nil {
			t.Fatalf("up: %v", err)
		}
		if n := notxCount(t, path, `SELECT count(*) FROM child`); n != 0 {
			t.Errorf("%d child rows after a rebuild without the marker; want 0 — foreign keys on, as before", n)
		}
	})
}

// PostgreSQL ignores the marker: the file runs as every PostgreSQL file does, in one
// transaction, so a failing statement takes the whole file with it.
func TestNoTransactionMarkerIgnoredByPostgres(t *testing.T) {
	env := os.Getenv("LIGHTSTACK_TEST_DB_URL")
	if !strings.HasPrefix(env, "postgres://") && !strings.HasPrefix(env, "postgresql://") {
		t.Skip("needs LIGHTSTACK_TEST_DB_URL set to a postgres:// URL")
	}
	url := scratchPostgres(t, env, "lightstack_notx_"+strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	dir := writeMigrations(t, map[string]string{
		"001_init.up.sql":   notxMarker + "\nCREATE TABLE notx_ok (id TEXT PRIMARY KEY);\n",
		"001_init.down.sql": "DROP TABLE notx_ok;",
		"002_half.up.sql":   notxMarker + "\nCREATE TABLE notx_half (id TEXT);\nCREATE TABLE notx_half (id TEXT);\n",
		"002_half.down.sql": "DROP TABLE notx_half;",
	})
	if err := MigrateToVersion(url, dir, 1); err != nil {
		t.Fatalf("a PostgreSQL file with the marker: %v; want it run as any other", err)
	}
	if err := MigrateUp(url, dir); err == nil {
		t.Fatal("up through a failing PostgreSQL file succeeded")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var ok, half bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('notx_ok') IS NOT NULL, to_regclass('notx_half') IS NOT NULL`).Scan(&ok, &half); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("notx_ok is missing; want 001 applied")
	}
	if half {
		t.Error("notx_half exists after its file failed; want the whole file rolled back, as without the marker")
	}
}
