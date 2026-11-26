package database

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The baseline at 001 must equal the pre-squash history it replaces (SPEC-032 FR-003, FR-004,
// FR-005): the history and the baseline migrate two empty databases side by side, and the two are
// compared object by object. The history lives in testdata/pre-squash, moved there by the squash.

const preSquashDir = "testdata/pre-squash"

func shortID() string { return strings.ReplaceAll(uuid.NewString()[:8], "-", "") }

// freshDatabase returns the URL of an empty database on the backend LIGHTSTACK_TEST_DB_URL
// selects: a scratch PostgreSQL database, or a SQLite file in the test's temp directory.
func freshDatabase(t *testing.T, name string) (string, Backend) {
	t.Helper()
	env := os.Getenv("LIGHTSTACK_TEST_DB_URL")
	switch {
	case strings.HasPrefix(env, "postgres://"), strings.HasPrefix(env, "postgresql://"):
		return scratchPostgres(t, env, name+"_"+shortID()), Postgres
	case strings.HasPrefix(env, "sqlite://"):
		return "sqlite://" + filepath.Join(t.TempDir(), name+".db"), SQLite
	}
	t.Skip("needs LIGHTSTACK_TEST_DB_URL set to a postgres:// or sqlite:// URL")
	return "", ""
}

func introspectURL(t *testing.T, ctx context.Context, url string) []pgTable {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tables, err := introspectPostgres(ctx, conn, "public")
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	return tables
}

func openSQLiteFile(t *testing.T, ctx context.Context, path string) *sql.DB {
	t.Helper()
	db, err := openSQLiteHandle(ctx, path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestBaselineEquivalencePostgres proves the PostgreSQL baseline builds exactly the schema the
// history builds — tables, columns, constraints by name, indexes, comments, sequences — and the
// same seed rows, with values generated at install time compared as generated.
func TestBaselineEquivalencePostgres(t *testing.T) {
	admin := adminPostgresURL(t)
	ctx := context.Background()
	source := migrationsSource(t, preSquashDir, math.MaxUint64)
	suffix := shortID()
	historyURL := scratchPostgres(t, admin, "lightstack_eq_history_"+suffix)
	baselineURL := scratchPostgres(t, admin, "lightstack_eq_baseline_"+suffix)
	migrateFolderNoChecks(t, historyURL, os.DirFS(preSquashDir), 0)
	if err := MigrateUp(baselineURL, "../../migrations"); err != nil {
		t.Fatalf("migrate from the baseline: %v", err)
	}
	history, baseline := introspectURL(t, ctx, historyURL), introspectURL(t, ctx, baselineURL)

	if diffs := diffPostgresExact(normalizeSchema(baseline, "public"), normalizeSchema(history, "public"), "applied_migrations"); len(diffs) > 0 {
		t.Errorf("the baseline's schema differs from the history's in %d places:\n%s", len(diffs), strings.Join(diffs, "\n"))
	}
	// A database built from the baseline hands out the identifiers the history's would.
	if diffs := diffSequencePositions(baseline, history, "applied_migrations"); len(diffs) > 0 {
		t.Errorf("the baseline leaves sequences elsewhere than the history:\n%s", strings.Join(diffs, "\n"))
	}
	historyModel, err := postgresModel(history, source)
	if err != nil {
		t.Fatal(err)
	}
	baselineModel, err := postgresModel(baseline, source)
	if err != nil {
		t.Fatal(err)
	}
	delete(baselineModel, "applied_migrations")
	if diffs := diffModels(historyModel, baselineModel, "history", "baseline"); len(diffs) > 0 {
		t.Errorf("the baseline's seed rows or model differ from the history's in %d places:\n%s", len(diffs), strings.Join(diffs, "\n"))
	}
	if len(history) <= 50 {
		t.Errorf("%d tables compared; the comparison read nothing", len(history))
	}
}

// TestBaselineEquivalenceSQLite proves the SQLite baseline builds the schema and seed rows the
// SQLite history built — 073_baseline.sqlite plus 074 onward. The two are compared through the
// parity model, which reads the trigger-guarded NOT NULL columns of 079/080 as NOT NULL, so the
// one deliberate representation change (FR-005) compares equal.
func TestBaselineEquivalenceSQLite(t *testing.T) {
	ctx := context.Background()
	source := migrationsSource(t, preSquashDir, math.MaxUint64)
	dir := t.TempDir()
	historyPath, baselinePath := filepath.Join(dir, "history.db"), filepath.Join(dir, "baseline.db")
	migrateFolderNoChecks(t, "sqlite://"+historyPath, dropBelow(os.DirFS(preSquashDir), 73), 0)
	if err := MigrateUp("sqlite://"+baselinePath, "../../migrations"); err != nil {
		t.Fatalf("migrate from the baseline: %v", err)
	}
	historyModel, err := sqliteModel(ctx, openSQLiteFile(t, ctx, historyPath), source)
	if err != nil {
		t.Fatal(err)
	}
	baselineModel, err := sqliteModel(ctx, openSQLiteFile(t, ctx, baselinePath), source)
	if err != nil {
		t.Fatal(err)
	}
	delete(baselineModel, "applied_migrations")
	if diffs := diffModels(historyModel, baselineModel, "history", "baseline"); len(diffs) > 0 {
		t.Errorf("the SQLite baseline differs from the SQLite history in %d places:\n%s", len(diffs), strings.Join(diffs, "\n"))
	}
	if len(historyModel) <= 50 {
		t.Errorf("%d tables compared; the comparison read nothing", len(historyModel))
	}
}

// TestBaselineGeneratesInstallTimeIDs pins FR-003: seed ids the history generated at install time
// are generated at install time by the baseline too — two installs never share one.
func TestBaselineGeneratesInstallTimeIDs(t *testing.T) {
	ctx := context.Background()
	a, backend := freshDatabase(t, "lightstack_ids_a")
	b, _ := freshDatabase(t, "lightstack_ids_b")
	for _, url := range []string{a, b} {
		if err := MigrateUp(url, "../../migrations"); err != nil {
			t.Fatalf("migrate %s: %v", url, err)
		}
	}
	source := migrationsSource(t, "../../migrations", math.MaxUint64)
	idsA, idsB := generatedSeedIDs(t, ctx, a, backend, source), generatedSeedIDs(t, ctx, b, backend, source)
	if len(idsA) == 0 || len(idsB) == 0 {
		t.Fatalf("found %d and %d generated seed ids; the check would prove nothing", len(idsA), len(idsB))
	}
	for id, where := range idsA {
		if idsB[id] != "" {
			t.Errorf("both installs hold the generated id %s (%s)", id, where)
		}
	}
}

// generatedSeedIDs returns the seed values of columns that default to gen_random_uuid() and that no
// migration spells out, keyed by value, each with the table.column it was found in.
func generatedSeedIDs(t *testing.T, ctx context.Context, url string, backend Backend, source string) map[string]string {
	t.Helper()
	ids := map[string]string{}
	add := func(table, column, v string) {
		if id, err := uuid.Parse(v); err == nil && id.Version() == 4 && !strings.Contains(source, v) {
			ids[v] = table + "." + column
		}
	}
	if backend == Postgres {
		for _, tbl := range introspectURL(t, ctx, url) {
			for i, c := range tbl.columns {
				if c.def != "gen_random_uuid()" {
					continue
				}
				for _, row := range tbl.rows {
					if b, ok := row[i].([16]uint8); ok {
						add(tbl.name, c.name, uuid.UUID(b).String())
					}
				}
			}
		}
		return ids
	}
	db := openSQLiteFile(t, ctx, strings.TrimPrefix(url, "sqlite://"))
	for _, tc := range sqliteColumnsWhere(t, ctx, db, func(_, _ string, dflt sql.NullString) bool {
		return strings.Contains(dflt.String, "gen_random_uuid")
	}) {
		rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM %s`, quoteIdent(tc[1]), quoteIdent(tc[0])))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v sql.NullString
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			add(tc[0], tc[1], v.String)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	return ids
}

// sqliteColumnsWhere lists the [table, column] pairs of the SQLite database whose column matches.
func sqliteColumnsWhere(t *testing.T, ctx context.Context, db *sql.DB, match func(table, column string, dflt sql.NullString) bool) [][2]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT m.name, p.name, p.dflt_value FROM sqlite_schema m, pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' ORDER BY m.name, p.cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out [][2]string
	for rows.Next() {
		var table, column string
		var dflt sql.NullString
		if err := rows.Scan(&table, &column, &dflt); err != nil {
			t.Fatal(err)
		}
		if match(table, column, dflt) {
			out = append(out, [2]string{table, column})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestBaselineProjectIDNotNull pins FR-005's one representation change: on both backends a NULL
// project_id in the five ownership tables is a NOT NULL violation — on SQLite through the column
// constraint the baseline declares, where 079/080 used triggers that raised a trigger error.
func TestBaselineProjectIDNotNull(t *testing.T) {
	ctx := context.Background()
	url, backend := freshDatabase(t, "lightstack_notnull")
	if err := MigrateUp(url, "../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var exec func(string) error
	var columns func(string) []insertColumn
	if backend == Postgres {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		exec = func(q string) error { _, err := conn.Exec(ctx, q); return err }
		columns = func(table string) []insertColumn { return postgresRequiredColumns(t, ctx, conn, table) }
	} else {
		db := openSQLiteFile(t, ctx, strings.TrimPrefix(url, "sqlite://"))
		exec = func(q string) error { _, err := db.ExecContext(ctx, q); return err }
		columns = func(table string) []insertColumn { return sqliteRequiredColumns(t, ctx, db, table) }
	}
	for _, table := range []string{"volumes", "ports", "networks", "routers", "floating_ips"} {
		names, values := []string{quoteIdent("project_id")}, []string{"NULL"}
		for _, c := range columns(table) {
			if c.name == "project_id" {
				continue
			}
			names, values = append(names, quoteIdent(c.name)), append(values, c.dummy)
		}
		stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", quoteIdent(table), strings.Join(names, ", "), strings.Join(values, ", "))
		err := exec(stmt)
		switch {
		case err == nil:
			t.Errorf("%s accepted a NULL project_id: %s", table, stmt)
		case !IsNotNullViolation(err):
			t.Errorf("%s: a NULL project_id failed with %v; want a NOT NULL violation", table, err)
		case !strings.Contains(err.Error(), "project_id"):
			// Every other required column got a value; a violation naming another column
			// would mean the test proved nothing about project_id.
			t.Errorf("%s: the NOT NULL violation does not name project_id: %v", table, err)
		}
	}
}

// insertColumn is a NOT NULL column without a default, with a value of its type.
type insertColumn struct {
	name, dummy string
}

func postgresRequiredColumns(t *testing.T, ctx context.Context, conn *pgx.Conn, table string) []insertColumn {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT column_name, udt_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND is_nullable = 'NO' AND column_default IS NULL
		ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []insertColumn
	for rows.Next() {
		var name, udt string
		if err := rows.Scan(&name, &udt); err != nil {
			t.Fatal(err)
		}
		out = append(out, insertColumn{name: name, dummy: postgresDummy(t, table, name, udt)})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func postgresDummy(t *testing.T, table, column, udt string) string {
	t.Helper()
	switch udt {
	case "uuid":
		return "'" + uuid.NewString() + "'"
	case "varchar", "text", "bpchar":
		return "'x'"
	case "int2", "int4", "int8", "numeric", "float4", "float8":
		return "1"
	case "bool":
		return "true"
	case "timestamp", "timestamptz":
		return "'2026-01-01 00:00:00+00'"
	case "date":
		return "'2026-01-01'"
	case "json", "jsonb":
		return "'{}'"
	case "_text", "_uuid", "_varchar":
		return "'{}'"
	}
	t.Fatalf("%s.%s: no dummy value for type %s", table, column, udt)
	return ""
}

func sqliteRequiredColumns(t *testing.T, ctx context.Context, db *sql.DB, table string) []insertColumn {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT name, type FROM pragma_table_info(?) WHERE "notnull" = 1 AND dflt_value IS NULL`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []insertColumn
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatal(err)
		}
		dummy := "'x'"
		switch strings.ToUpper(typ) {
		case "INTEGER", "INT":
			dummy = "1"
		case "REAL":
			dummy = "1.0"
		case "BLOB":
			dummy = "x'00'"
		}
		out = append(out, insertColumn{name: name, dummy: dummy})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
