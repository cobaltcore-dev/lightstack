package database

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The SQLite baseline is generated, not written by hand (research R11): a scratch
// PostgreSQL database is migrated to the baseline version, introspected, and rendered
// as SQLite DDL plus its seed rows. The generated files are committed; regenerate them
// only when moving the baseline to a newer migration on main:
//
//	LIGHTSTACK_TEST_DB_URL=postgres://… go test -count=1 ./internal/database -run TestGenerateSQLiteBaseline -update

var updateBaseline = flag.Bool("update", false, "regenerate migrations/NNN_baseline.sqlite.*.sql from PostgreSQL")

// baselineTarget is the newest migration on main when the baseline was written.
const baselineTarget = 73

// sqliteChecks maps PostgreSQL's normalised CHECK definitions to SQLite. A CHECK the
// map does not know fails the generator, so a new one is translated deliberately.
var sqliteChecks = map[string]string{
	"CHECK ((domain_id > 0))":                                                                                "CHECK (domain_id > 0)",
	"CHECK ((socket ~~ '/%'::text))":                                                                         "CHECK (socket LIKE '/%')",
	"CHECK (((vni >= 1000) AND (vni <= 16777215)))":                                                          "CHECK (vni >= 1000 AND vni <= 16777215)",
	"CHECK (((external_port >= 1) AND (external_port <= 65535)))":                                            "CHECK (external_port >= 1 AND external_port <= 65535)",
	"CHECK (((internal_port >= 1) AND (internal_port <= 65535)))":                                            "CHECK (internal_port >= 1 AND internal_port <= 65535)",
	"CHECK (((protocol)::text = ANY ((ARRAY['tcp'::character varying, 'udp'::character varying])::text[])))": "CHECK (protocol IN ('tcp', 'udp'))",
}

// skippedIndexes have no SQLite equivalent; the parity test lists them as documented
// exceptions.
var skippedIndexes = map[string]string{
	"idx_images_properties": "GIN index over JSONB",
}

var (
	castLiteral = regexp.MustCompile(`^'((?:[^']|'')*)'::([a-z ]+(\[\])?)$`)
	numberLit   = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
	indexDef    = regexp.MustCompile(`^CREATE (UNIQUE )?INDEX (\S+) ON public\.(\S+) USING (\w+) \((.*?)\)(?: WHERE (.*))?$`)
)

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteText(s string) string  { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

func sqliteColumnType(c pgColumn) (string, error) {
	switch c.udt {
	case "uuid", "varchar", "text", "bpchar", "timestamp", "timestamptz", "date", "jsonb", "json", "_text", "_uuid", "_varchar":
		return "TEXT", nil
	case "bool", "int2", "int4", "int8":
		return "INTEGER", nil
	case "float4", "float8", "numeric":
		return "REAL", nil
	}
	return "", fmt.Errorf("column type %s has no SQLite mapping", c.udt)
}

func isNowDefault(def string) bool { return def == "now()" || def == "CURRENT_TIMESTAMP" }

func sqliteColumnDefault(c pgColumn) (string, error) {
	d := c.def
	switch {
	case d == "", strings.HasPrefix(d, "nextval("):
		return "", nil
	case d == "gen_random_uuid()":
		return "(gen_random_uuid())", nil
	case isNowDefault(d):
		return "(now())", nil
	case d == "true":
		return "1", nil
	case d == "false":
		return "0", nil
	case d == "ARRAY[]::text[]":
		return "'[]'", nil
	case numberLit.MatchString(d):
		return d, nil
	}
	if m := castLiteral.FindStringSubmatch(d); m != nil {
		if strings.HasSuffix(m[2], "[]") {
			if m[1] == "{}" {
				return "'[]'", nil
			}
			return "", fmt.Errorf("array default %q has no SQLite mapping", d)
		}
		return "'" + m[1] + "'", nil
	}
	return "", fmt.Errorf("default %q has no SQLite mapping", d)
}

// generatedID reports a seed id PostgreSQL generated while migrating: a version-4 UUID
// in a gen_random_uuid() column that no migration spells out. Every installation gets
// its own, so the baseline generates it too instead of freezing one run's value.
func generatedID(v any, c pgColumn, source string) bool {
	b, ok := v.([16]uint8)
	if !ok || c.def != "gen_random_uuid()" {
		return false
	}
	id := uuid.UUID(b)
	return id.Version() == 4 && !strings.Contains(source, id.String())
}

func sqliteSeedLiteral(v any, c pgColumn, source string) (string, error) {
	if generatedID(v, c, source) {
		return "gen_random_uuid()", nil
	}
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		return quoteText(x), nil
	case bool:
		if x {
			return "1", nil
		}
		return "0", nil
	case int16, int32, int64, int:
		return fmt.Sprint(x), nil
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case time.Time:
		if isNowDefault(c.def) {
			return "now()", nil // the install time, as PostgreSQL's seed gets
		}
		return quoteText(formatTime(x)), nil
	case [16]uint8:
		return quoteText(uuid.UUID(x).String()), nil
	case map[string]any, []any:
		b, err := json.Marshal(x)
		if err != nil {
			return "", err
		}
		return quoteText(string(b)), nil
	}
	return "", fmt.Errorf("seed value %T in %s has no SQLite mapping", v, c.name)
}

// checkGeneratedIDsUnreferenced fails when one seed row holds an id PostgreSQL generated
// for another: generating the id afresh would leave the reference dangling.
func checkGeneratedIDsUnreferenced(tables []pgTable, source string) error {
	generated := map[string]string{}
	for _, t := range tables {
		for _, row := range t.rows {
			for i, c := range t.columns {
				if generatedID(row[i], c, source) {
					generated[uuid.UUID(row[i].([16]uint8)).String()] = t.name + "." + c.name
				}
			}
		}
	}
	for _, t := range tables {
		for _, row := range t.rows {
			for i, c := range t.columns {
				if generatedID(row[i], c, source) {
					continue
				}
				text := fmt.Sprint(row[i])
				if b, ok := row[i].([16]uint8); ok {
					text = uuid.UUID(b).String()
				}
				for id, owner := range generated {
					if strings.Contains(text, id) {
						return fmt.Errorf("%s.%s refers to %s, generated for %s", t.name, c.name, id, owner)
					}
				}
			}
		}
	}
	return nil
}

// renderSQLiteBaseline renders tables as SQLite DDL and seed rows. source is the text of the
// PostgreSQL migrations, used to tell ids they spell out from ids PostgreSQL made up.
func renderSQLiteBaseline(tables []pgTable, source string) (up, down string, err error) {
	var b strings.Builder
	fmt.Fprintf(&b, "-- SQLite baseline: the schema and seed data of the PostgreSQL migrations up to %03d.\n", baselineTarget)
	b.WriteString("-- Generated by TestGenerateSQLiteBaseline (internal/database/baseline_gen_test.go); do not edit by hand.\n")
	b.WriteString("-- Mapping: research R11 and data-model §3 of SPEC-028. Not mirrored: ")
	var skipped []string
	for name, why := range skippedIndexes {
		skipped = append(skipped, name+" ("+why+")")
	}
	sort.Strings(skipped) // a map's order is random; the generated header must be stable
	b.WriteString(strings.Join(skipped, ", ") + ".\n\n")
	b.WriteString("PRAGMA defer_foreign_keys = ON;\n")

	if err := checkGeneratedIDsUnreferenced(tables, source); err != nil {
		return "", "", err
	}
	var indexes, seeds []string
	for _, t := range tables {
		ddl, idx, rows, err := renderTable(t, source)
		if err != nil {
			return "", "", err
		}
		b.WriteString(ddl)
		indexes = append(indexes, idx...)
		seeds = append(seeds, rows...)
	}
	b.WriteString("\n" + strings.Join(indexes, "\n") + "\n\n-- Seed data\n" + strings.Join(seeds, "\n") + "\n")
	return b.String(), renderBaselineDown(tables), nil
}

// renderTable renders one table's CREATE TABLE and returns its index and seed-row statements.
func renderTable(t pgTable, source string) (ddl string, indexes, seeds []string, err error) {
	serial := ""
	for _, c := range t.columns {
		if strings.HasPrefix(c.def, "nextval(") {
			serial = c.name
		}
	}
	if serial != "" && (len(t.pk) != 1 || t.pk[0] != serial) {
		return "", nil, nil, fmt.Errorf("%s: sequence column %s is not the whole primary key", t.name, serial)
	}
	var lines []string
	for _, c := range t.columns {
		line, err := renderColumn(t, c, serial)
		if err != nil {
			return "", nil, nil, err
		}
		lines = append(lines, line)
	}
	if len(t.pk) > 0 && serial == "" {
		quoted := make([]string, len(t.pk))
		for i, c := range t.pk {
			quoted[i] = quoteIdent(c)
		}
		lines = append(lines, "    PRIMARY KEY ("+strings.Join(quoted, ", ")+")")
	}
	for _, u := range t.uniques {
		lines = append(lines, "    "+u)
	}
	for _, f := range t.fks {
		lines = append(lines, "    "+f)
	}
	for _, c := range t.checks {
		s, ok := sqliteChecks[c]
		if !ok {
			return "", nil, nil, fmt.Errorf("%s: CHECK %q has no SQLite mapping in sqliteChecks", t.name, c)
		}
		lines = append(lines, "    "+s)
	}
	ddl = fmt.Sprintf("\nCREATE TABLE %s (\n%s\n) STRICT;\n", quoteIdent(t.name), strings.Join(lines, ",\n"))
	if indexes, err = renderIndexes(t); err != nil {
		return "", nil, nil, err
	}
	if seeds, err = renderSeedRows(t, source); err != nil {
		return "", nil, nil, err
	}
	return ddl, indexes, seeds, nil
}

// renderColumn renders one column's line of a CREATE TABLE. serial is the AUTOINCREMENT column.
func renderColumn(t pgTable, c pgColumn, serial string) (string, error) {
	typ, err := sqliteColumnType(c)
	if err != nil {
		return "", fmt.Errorf("%s.%s: %w", t.name, c.name, err)
	}
	line := "    " + quoteIdent(c.name) + " " + typ
	if c.name == serial {
		line += " PRIMARY KEY AUTOINCREMENT"
	}
	if !c.nullable && c.name != serial {
		line += " NOT NULL"
	}
	def, err := sqliteColumnDefault(c)
	if err != nil {
		return "", fmt.Errorf("%s.%s: %w", t.name, c.name, err)
	}
	if def != "" {
		line += " DEFAULT " + def
	}
	if c.udt == "varchar" && c.maxLen > 0 {
		line += fmt.Sprintf(" CHECK (length(%s) <= %d)", quoteIdent(c.name), c.maxLen)
	}
	return line, nil
}

// renderIndexes renders t's btree indexes, skipping those listed in skippedIndexes.
func renderIndexes(t pgTable) ([]string, error) {
	var indexes []string
	for _, def := range t.indexes {
		m := indexDef.FindStringSubmatch(def)
		if m == nil {
			return nil, fmt.Errorf("index %q has no SQLite mapping", def)
		}
		if m[4] != "btree" {
			if _, ok := skippedIndexes[m[2]]; !ok {
				return nil, fmt.Errorf("index %s uses %s; add it to skippedIndexes or map it", m[2], m[4])
			}
			continue
		}
		s := fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)", m[1], quoteIdent(m[2]), quoteIdent(m[3]), m[5])
		if m[6] != "" {
			s += " WHERE " + m[6]
		}
		indexes = append(indexes, s+";")
	}
	return indexes, nil
}

// renderSeedRows renders the INSERT statements for t's seed rows, sorted (generated ids no longer
// order them).
func renderSeedRows(t pgTable, source string) ([]string, error) {
	var rows []string
	for _, row := range t.rows {
		cols := make([]string, len(t.columns))
		vals := make([]string, len(t.columns))
		for i, c := range t.columns {
			cols[i] = quoteIdent(c.name)
			lit, err := sqliteSeedLiteral(row[i], c, source)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", t.name, err)
			}
			vals[i] = lit
		}
		rows = append(rows, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s);", quoteIdent(t.name), strings.Join(cols, ", "), strings.Join(vals, ", ")))
	}
	sort.Strings(rows)
	return rows, nil
}

// renderBaselineDown renders the baseline's down file: drop every table it created, in reverse.
func renderBaselineDown(tables []pgTable) string {
	var d strings.Builder
	fmt.Fprintf(&d, "-- Reverts the SQLite baseline (%03d): drops every table it created.\n", baselineTarget)
	d.WriteString("-- Generated by TestGenerateSQLiteBaseline (internal/database/baseline_gen_test.go); do not edit by hand.\n\n")
	d.WriteString("PRAGMA defer_foreign_keys = ON;\n")
	for i := len(tables) - 1; i >= 0; i-- {
		fmt.Fprintf(&d, "DROP TABLE IF EXISTS %s;\n", quoteIdent(tables[i].name))
	}
	return d.String()
}

// migrationsSource returns the text of every PostgreSQL up migration up to version.
func migrationsSource(t *testing.T, version uint64) string {
	t.Helper()
	view, err := newMigrationView(os.DirFS("../../migrations"), Postgres)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range view.entries {
		f, _ := parseMigrationFile(e.Name())
		if f.direction != "up" || f.version > version {
			continue
		}
		data, err := fs.ReadFile(view, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}

func TestGenerateSQLiteBaseline(t *testing.T) {
	if !*updateBaseline {
		t.Skip("run with -update and a postgres:// LIGHTSTACK_TEST_DB_URL to regenerate the SQLite baseline")
	}
	admin := adminPostgresURL(t)
	scratch := scratchPostgres(t, admin, "lightstack_baseline_gen")
	if err := MigrateToVersion(scratch, "../../migrations", baselineTarget); err != nil {
		t.Fatalf("migrate the scratch database to %03d: %v", baselineTarget, err)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, scratch)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	tables, err := introspectPostgres(ctx, conn)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	up, down, err := renderSQLiteBaseline(tables, migrationsSource(t, baselineTarget))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// The output must apply to an empty SQLite database and revert cleanly.
	db, err := openSQLiteHandle(ctx, filepath.Join(t.TempDir(), "baseline.db"), defaultSQLiteTxLock, false, PoolConfig{MaxConns: 1})
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	defer db.Close()
	for _, script := range []string{up, down, up} {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, script); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply the generated script: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit the generated script: %v", err)
		}
	}

	base := filepath.Join("..", "..", "migrations", fmt.Sprintf("%03d_baseline.sqlite", baselineTarget))
	if err := os.WriteFile(base+".up.sql", []byte(up), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".down.sql", []byte(down), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s.{up,down}.sql: %d tables", base, len(tables))
}
