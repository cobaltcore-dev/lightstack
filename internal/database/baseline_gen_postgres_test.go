package database

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The PostgreSQL baseline is rendered from the catalog of a scratch database migrated with the
// pre-squash history (SPEC-032 research R2), not dumped: pg_dump writes psql meta-commands with a
// random key and freezes ids the history generates at install time. Every construct is mapped on
// purpose; one the renderer does not know stops it.

// postgresCheckSource maps CHECK definitions whose deparsed text does not reproduce itself to the
// source text that does. pg_get_constraintdef prints `protocol IN ('tcp', 'udp')` on a varchar column
// as an = ANY over a text[] cast; parsed again, that text casts each array element on its own and
// deparses differently. The source form deparses exactly as the history's constraint. A new entry
// is found by the generator's own round-trip check below, never by guessing.
var postgresCheckSource = map[string]string{
	"CHECK (((protocol)::text = ANY ((ARRAY['tcp'::character varying, 'udp'::character varying])::text[])))": "CHECK (protocol IN ('tcp', 'udp'))",
}

func TestGeneratePostgresBaseline(t *testing.T) {
	if !*updateBaseline {
		t.Skip("run with -update and a postgres:// LIGHTSTACK_TEST_DB_URL to regenerate the PostgreSQL baseline")
	}
	requireSquashPointIsNewest(t)
	admin := adminPostgresURL(t)
	ctx := context.Background()
	scratch := scratchPostgres(t, admin, "lightstack_baseline_gen_pg")
	migrateFolderNoChecks(t, scratch, os.DirFS(preSquashDir), squashPoint)
	tables := normalizeSchema(introspectURL(t, ctx, scratch), "public")
	body, downBody, err := renderPostgresBaseline(tables, migrationsSource(t, preSquashDir, squashPoint))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	header := baselineHeader(Postgres, "TestGeneratePostgresBaseline")
	up, down := renderUp(header, 1, []byte(body)), renderDown(header, 1, []byte(downBody))

	// The output must apply to an empty database and revert cleanly, run as golang-migrate runs a
	// file: one multi-statement simple query, one implicit transaction.
	verifyURL := scratchPostgres(t, admin, "lightstack_baseline_verify_pg")
	conn, err := pgx.Connect(ctx, verifyURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for i, script := range [][]byte{up, down, up} {
		if _, err := conn.Exec(ctx, string(script), pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("apply step %d of up, down, up: %v", i+1, err)
		}
	}
	// The rendered schema must read back exactly as the history's: a definition PostgreSQL does not
	// deparse the same way twice would otherwise change the schema silently (FR-005).
	rendered := normalizeSchema(introspectURL(t, ctx, verifyURL), "public")
	if diffs := diffPostgresExact(rendered, tables, "applied_migrations"); len(diffs) > 0 {
		t.Fatalf("the rendered baseline does not read back as the history's schema:\n%s", strings.Join(diffs, "\n"))
	}
	if diffs := diffSequencePositions(rendered, tables, "applied_migrations"); len(diffs) > 0 {
		t.Fatalf("the rendered baseline leaves sequences elsewhere than the history:\n%s", strings.Join(diffs, "\n"))
	}

	base := filepath.Join("..", "..", "migrations", "001_baseline.postgres")
	if err := os.WriteFile(base+".up.sql", up, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".down.sql", down, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s.{up,down}.sql: %d tables", base, len(tables))
}

// renderPostgresBaseline renders normalized tables as the bodies of the PostgreSQL baseline's up and
// down files. source is the text of the pre-squash migrations: it tells ids they spell out from ids
// generated at install time.
func renderPostgresBaseline(tables []pgTable, source string) (up, down string, err error) {
	if err := checkGeneratedIDsUnreferenced(tables, source); err != nil {
		return "", "", err
	}
	var b strings.Builder
	b.WriteString("-- Tables, in dependency order.\n")
	var fks, indexes, comments, seeds, sequences []string
	for _, t := range tables {
		ddl, err := renderPostgresTable(t)
		if err != nil {
			return "", "", err
		}
		b.WriteString(ddl)
		for _, c := range t.constraints {
			if c.kind == "f" {
				fks = append(fks, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s;", quoteIdent(t.name), quoteIdent(c.name), c.def))
			}
		}
		for _, def := range t.indexes {
			indexes = append(indexes, def+";")
		}
		if t.comment != "" {
			comments = append(comments, fmt.Sprintf("COMMENT ON TABLE %s IS %s;", quoteIdent(t.name), quoteText(t.comment)))
		}
		for _, c := range t.columns {
			if c.comment != "" {
				comments = append(comments, fmt.Sprintf("COMMENT ON COLUMN %s.%s IS %s;", quoteIdent(t.name), quoteIdent(c.name), quoteText(c.comment)))
			}
		}
		rows, err := renderPostgresSeedRows(t, source)
		if err != nil {
			return "", "", err
		}
		seeds = append(seeds, rows...)
		for _, s := range t.sequences {
			if s.lastValue != nil {
				sequences = append(sequences, fmt.Sprintf("SELECT setval(%s, %d, true);", quoteText(s.name), *s.lastValue))
			}
		}
	}
	for _, section := range []struct {
		title string
		lines []string
	}{
		{"Foreign keys, once every table exists.", fks},
		{"Indexes that back no constraint.", indexes},
		{"Comments.", comments},
		{"Seed data, parents first.", seeds},
		{"Sequence state the history left.", sequences},
	} {
		if len(section.lines) > 0 {
			b.WriteString("\n-- " + section.title + "\n" + strings.Join(section.lines, "\n") + "\n")
		}
	}
	b.WriteString("\n-- The applied-state record (SPEC-032 FR-019); its presence marks a post-squash database.\n")
	b.WriteString(appliedMigrationsDDL[Postgres])

	var d strings.Builder
	d.WriteString("DROP TABLE IF EXISTS applied_migrations;\n")
	for i := len(tables) - 1; i >= 0; i-- {
		fmt.Fprintf(&d, "DROP TABLE IF EXISTS %s;\n", quoteIdent(tables[i].name))
	}
	return b.String(), d.String(), nil
}

// renderPostgresTable renders one CREATE TABLE: its columns, then its primary key, unique and check
// constraints by name. Foreign keys, indexes and comments come after every table exists.
func renderPostgresTable(t pgTable) (string, error) {
	owned := map[string]bool{}
	for _, s := range t.sequences {
		owned[s.name] = true
	}
	var lines []string
	for _, c := range t.columns {
		line, err := renderPostgresColumn(t.name, c, owned)
		if err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	byKind := map[string][]pgConstraint{}
	for _, c := range t.constraints {
		switch c.kind {
		case "p", "u", "c":
			byKind[c.kind] = append(byKind[c.kind], c)
		case "f", "n":
			// Foreign keys follow the tables; NOT NULL is part of each column.
		default:
			return "", fmt.Errorf("%s: constraint %s of kind %q has no rendering", t.name, c.name, c.kind)
		}
	}
	for _, kind := range []string{"p", "u", "c"} {
		cs := byKind[kind]
		sort.Slice(cs, func(i, j int) bool { return cs[i].name < cs[j].name })
		for _, c := range cs {
			def := c.def
			if src, ok := postgresCheckSource[def]; ok {
				def = src
			}
			lines = append(lines, fmt.Sprintf("    CONSTRAINT %s %s", quoteIdent(c.name), def))
		}
	}
	return fmt.Sprintf("CREATE TABLE %s (\n%s\n);\n", quoteIdent(t.name), strings.Join(lines, ",\n")), nil
}

// renderPostgresColumn renders one column. A column whose default draws from the sequence it owns,
// named <table>_<column>_seq, is SERIAL: that recreates the sequence, its ownership, the default
// and NOT NULL exactly as the history's SERIAL did.
func renderPostgresColumn(table string, c pgColumn, owned map[string]bool) (string, error) {
	seq := table + "_" + c.name + "_seq"
	if owned[seq] && c.def == "nextval('"+seq+"'::regclass)" {
		serial := map[string]string{"int2": "smallserial", "int4": "serial", "int8": "bigserial"}[c.udt]
		if serial == "" {
			return "", fmt.Errorf("%s.%s: a sequence-backed %s column has no rendering", table, c.name, c.udt)
		}
		return "    " + quoteIdent(c.name) + " " + serial, nil
	}
	typ, err := postgresColumnType(c)
	if err != nil {
		return "", fmt.Errorf("%s.%s: %w", table, c.name, err)
	}
	line := "    " + quoteIdent(c.name) + " " + typ
	if !c.nullable {
		line += " NOT NULL"
	}
	if c.def != "" {
		line += " DEFAULT " + c.def
	}
	return line, nil
}

func postgresColumnType(c pgColumn) (string, error) {
	switch c.udt {
	case "varchar":
		if c.maxLen > 0 {
			return fmt.Sprintf("varchar(%d)", c.maxLen), nil
		}
		return "varchar", nil
	case "uuid", "text", "timestamptz", "timestamp", "date", "jsonb", "json", "numeric":
		return c.udt, nil
	case "bool":
		return "boolean", nil
	case "int2":
		return "smallint", nil
	case "int4":
		return "integer", nil
	case "int8":
		return "bigint", nil
	case "float4":
		return "real", nil
	case "float8":
		return "double precision", nil
	case "_text", "_uuid", "_varchar":
		return strings.TrimPrefix(c.udt, "_") + "[]", nil
	}
	return "", fmt.Errorf("column type %s has no PostgreSQL rendering", c.udt)
}

// renderPostgresSeedRows renders t's rows as INSERT statements, sorted so the output is stable.
func renderPostgresSeedRows(t pgTable, source string) ([]string, error) {
	cols := make([]string, len(t.columns))
	for i, c := range t.columns {
		cols[i] = quoteIdent(c.name)
	}
	var rows []string
	for _, row := range t.rows {
		vals := make([]string, len(t.columns))
		for i, c := range t.columns {
			lit, err := postgresSeedLiteral(row[i], c, source)
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

// postgresSeedLiteral renders one seed value. An id the history generated at install time stays
// generated, and a timestamp is the install time, as the history's seed rows get it (FR-003).
func postgresSeedLiteral(v any, c pgColumn, source string) (string, error) {
	if generatedID(v, c, source) {
		return "gen_random_uuid()", nil
	}
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		return quoteText(x), nil
	case bool:
		return strconv.FormatBool(x), nil
	case int16, int32, int64, int:
		return fmt.Sprint(x), nil
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case time.Time:
		return "now()", nil
	case [16]uint8:
		return quoteText(uuid.UUID(x).String()), nil
	case []any, []string, map[string]any:
		if strings.HasPrefix(c.udt, "_") {
			return postgresArrayLiteral(x, c)
		}
		if c.udt == "jsonb" || c.udt == "json" {
			b, err := json.Marshal(x)
			if err != nil {
				return "", fmt.Errorf("%s: %w", c.name, err)
			}
			return quoteText(string(b)), nil
		}
	}
	return "", fmt.Errorf("seed value %T in %s (%s) has no PostgreSQL rendering", v, c.name, c.udt)
}

// postgresArrayLiteral renders a text, uuid or varchar array as an ARRAY constructor.
func postgresArrayLiteral(v any, c pgColumn) (string, error) {
	var elems []string
	switch x := v.(type) {
	case []string:
		elems = x
	case []any:
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return "", fmt.Errorf("%s: array element %T has no rendering", c.name, e)
			}
			elems = append(elems, s)
		}
	default:
		return "", fmt.Errorf("%s: value %T is not an array", c.name, v)
	}
	typ, err := postgresColumnType(c)
	if err != nil {
		return "", err
	}
	quoted := make([]string, len(elems))
	for i, e := range elems {
		quoted[i] = quoteText(e)
	}
	return "ARRAY[" + strings.Join(quoted, ", ") + "]::" + typ, nil
}
