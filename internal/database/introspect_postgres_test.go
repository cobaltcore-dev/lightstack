package database

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Introspection of a PostgreSQL schema, for tests only. The baseline generators read it to render
// the baselines, and the schema parity and baseline equivalence tests compare it (SPEC-032
// research R2, R4).

type pgColumn struct {
	name     string
	udt      string // uuid, varchar, text, timestamp, timestamptz, bool, int4, int8, jsonb, _text, …
	maxLen   int    // varchar(n); 0 otherwise
	nullable bool
	def      string // column_default as PostgreSQL prints it; "" for none
	comment  string // COMMENT ON COLUMN; "" for none
}

// pgConstraint is one row of pg_constraint: its name, its kind (contype: p, u, f, c, and on
// PostgreSQL 18 also n for NOT NULL) and pg_get_constraintdef's text.
type pgConstraint struct {
	name string
	kind string
	def  string
}

// pgSequence is a sequence a column of the table owns, as SERIAL creates one. lastValue is nil
// until the sequence first hands out a value.
type pgSequence struct {
	name      string
	column    string
	dataType  string
	lastValue *int64
}

type pgTable struct {
	name        string
	comment     string // COMMENT ON TABLE; "" for none
	columns     []pgColumn
	pk          []string
	pkName      string
	uniques     []string       // pg_get_constraintdef of UNIQUE constraints
	fks         []string       // pg_get_constraintdef of FOREIGN KEY constraints
	parents     []string       // tables the foreign keys reference
	checks      []string       // pg_get_constraintdef of CHECK constraints
	constraints []pgConstraint // every constraint, by name
	indexes     []string       // pg_get_indexdef of indexes that back no constraint
	sequences   []pgSequence
	rows        [][]any // seed rows, ordered by primary key
}

// introspectPostgres reads every base table of schema except golang-migrate's schema_migrations,
// with its columns, constraints, indexes, comments, owned sequences and rows, in dependency order.
func introspectPostgres(ctx context.Context, conn *pgx.Conn, schema string) ([]pgTable, error) {
	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = $1 AND table_type = 'BASE TABLE' AND table_name <> 'schema_migrations'
		ORDER BY table_name`, schema)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	tables := make([]pgTable, 0, len(names))
	for _, name := range names {
		t, err := introspectTable(ctx, conn, schema, name)
		if err == nil {
			t.rows, err = introspectRows(ctx, conn, schema, t)
		}
		if err != nil {
			return nil, fmt.Errorf("introspect %s.%s: %w", schema, name, err)
		}
		tables = append(tables, t)
	}
	return dependencyOrder(tables)
}

func introspectTable(ctx context.Context, conn *pgx.Conn, schema, name string) (pgTable, error) {
	t := pgTable{name: name}
	var err error
	if t.columns, err = introspectColumns(ctx, conn, schema, name); err != nil {
		return t, err
	}
	if err := introspectConstraints(ctx, conn, schema, &t); err != nil {
		return t, err
	}
	// PostgreSQL catalog functions: introspection only ever reads a PostgreSQL schema.
	if err := conn.QueryRow(ctx, `SELECT COALESCE(obj_description(
			(quote_ident($1) || '.' || quote_ident($2))::regclass, 'pg_class'), '')`, schema, name).Scan(&t.comment); err != nil {
		return t, err
	}
	idx, err := conn.Query(ctx, `SELECT indexdef FROM pg_indexes i WHERE schemaname = $1 AND tablename = $2
		AND NOT EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
			WHERE c.conname = i.indexname AND n.nspname = i.schemaname)
		ORDER BY indexname`, schema, name)
	if err != nil {
		return t, err
	}
	if t.indexes, err = pgx.CollectRows(idx, pgx.RowTo[string]); err != nil {
		return t, err
	}
	t.sequences, err = introspectSequences(ctx, conn, schema, name)
	return t, err
}

func introspectColumns(ctx context.Context, conn *pgx.Conn, schema, name string) ([]pgColumn, error) {
	rows, err := conn.Query(ctx, `SELECT column_name, udt_name, COALESCE(character_maximum_length, 0),
			is_nullable = 'YES', COALESCE(column_default, ''),
			COALESCE(col_description((quote_ident($1) || '.' || quote_ident($2))::regclass, ordinal_position::int), '')
		FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []pgColumn
	for rows.Next() {
		var c pgColumn
		if err := rows.Scan(&c.name, &c.udt, &c.maxLen, &c.nullable, &c.def, &c.comment); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// introspectConstraints fills the constraint fields of t. A foreign key's parent is read as the
// bare relation name, so a table read from a scratch schema orders the same as one read from
// public.
func introspectConstraints(ctx context.Context, conn *pgx.Conn, schema string, t *pgTable) error {
	rows, err := conn.Query(ctx, `SELECT conname, contype::text, pg_get_constraintdef(oid),
			CASE WHEN contype = 'f' THEN (SELECT relname FROM pg_class WHERE oid = confrelid) ELSE '' END
		FROM pg_constraint WHERE conrelid = (quote_ident($1) || '.' || quote_ident($2))::regclass
		ORDER BY contype, conname`, schema, t.name)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c pgConstraint
		var parent string
		if err := rows.Scan(&c.name, &c.kind, &c.def, &parent); err != nil {
			return err
		}
		t.constraints = append(t.constraints, c)
		switch c.kind {
		case "p":
			t.pkName = c.name
			inner := strings.TrimSuffix(strings.TrimPrefix(c.def, "PRIMARY KEY ("), ")")
			for _, col := range strings.Split(inner, ",") {
				t.pk = append(t.pk, strings.Trim(strings.TrimSpace(col), `"`))
			}
		case "u":
			t.uniques = append(t.uniques, c.def)
		case "f":
			t.fks = append(t.fks, c.def)
			if parent != t.name {
				t.parents = append(t.parents, parent)
			}
		case "c":
			t.checks = append(t.checks, c.def)
		}
	}
	return rows.Err()
}

// introspectSequences lists the sequences the table's columns own (SERIAL), with their state.
func introspectSequences(ctx context.Context, conn *pgx.Conn, schema, name string) ([]pgSequence, error) {
	rows, err := conn.Query(ctx, `SELECT s.relname, a.attname, format_type(sq.seqtypid, NULL), ps.last_value
		FROM pg_class t
		JOIN pg_namespace n ON n.oid = t.relnamespace
		JOIN pg_depend d ON d.refclassid = 'pg_class'::regclass AND d.refobjid = t.oid AND d.deptype = 'a'
		JOIN pg_class s ON s.oid = d.objid AND d.classid = 'pg_class'::regclass AND s.relkind = 'S'
		JOIN pg_namespace sn ON sn.oid = s.relnamespace
		JOIN pg_sequence sq ON sq.seqrelid = s.oid
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
		JOIN pg_sequences ps ON ps.schemaname = sn.nspname AND ps.sequencename = s.relname
		WHERE n.nspname = $1 AND t.relname = $2
		ORDER BY s.relname`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seqs []pgSequence
	for rows.Next() {
		var s pgSequence
		if err := rows.Scan(&s.name, &s.column, &s.dataType, &s.lastValue); err != nil {
			return nil, err
		}
		seqs = append(seqs, s)
	}
	return seqs, rows.Err()
}

// introspectRows reads the table's rows, ordered by its primary key (or the first column).
func introspectRows(ctx context.Context, conn *pgx.Conn, schema string, t pgTable) ([][]any, error) {
	order := "1"
	if len(t.pk) > 0 {
		quoted := make([]string, len(t.pk))
		for i, c := range t.pk {
			quoted[i] = pgx.Identifier{c}.Sanitize()
		}
		order = strings.Join(quoted, ", ")
	}
	// SELECT *'s result type is the table's current shape. Describe it on every call instead of
	// reusing a cached prepared statement, which PostgreSQL refuses once the table has changed
	// ("cached plan must not change result type").
	rows, err := conn.Query(ctx, fmt.Sprintf(`SELECT * FROM %s ORDER BY %s`, pgx.Identifier{schema, t.name}.Sanitize(), order),
		pgx.QueryExecModeDescribeExec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

// dependencyOrder sorts tables so that every table follows the tables its foreign keys
// reference, alphabetically among tables that are free to go. Creating and seeding in
// this order and dropping in reverse keeps each statement's referenced tables present.
func dependencyOrder(tables []pgTable) ([]pgTable, error) {
	byName := map[string]pgTable{}
	for _, t := range tables {
		byName[t.name] = t
	}
	done := map[string]bool{}
	var out []pgTable
	for len(out) < len(tables) {
		var ready []string
		for _, t := range tables {
			if done[t.name] {
				continue
			}
			free := true
			for _, p := range t.parents {
				if _, known := byName[p]; known && !done[p] {
					free = false
				}
			}
			if free {
				ready = append(ready, t.name)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("foreign keys form a cycle among the remaining %d tables", len(tables)-len(out))
		}
		sort.Strings(ready)
		for _, n := range ready {
			done[n] = true
			out = append(out, byName[n])
		}
	}
	return out, nil
}

// normalizeSchema returns a copy of tables with every qualifier naming schema removed — from column
// defaults (nextval('schema.seq'::regclass)), constraint definitions and index definitions — so the
// same objects read from two schemas compare equal (research R10). The qualifier is matched as a
// whole identifier, plain or quoted: a schema named a leaves the "a." inside data.x alone.
func normalizeSchema(tables []pgTable, schema string) []pgTable {
	q := regexp.QuoteMeta(schema)
	qualifier := regexp.MustCompile(`(?:"` + q + `"|\b` + q + `)\.`)
	strip := func(s string) string { return qualifier.ReplaceAllString(s, "") }
	stripAll := func(in []string) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = strip(s)
		}
		return out
	}
	out := make([]pgTable, len(tables))
	for i, t := range tables {
		n := t
		n.columns = make([]pgColumn, len(t.columns))
		for j, c := range t.columns {
			c.def = strip(c.def)
			n.columns[j] = c
		}
		n.constraints = make([]pgConstraint, len(t.constraints))
		for j, c := range t.constraints {
			c.def = strip(c.def)
			n.constraints[j] = c
		}
		n.uniques, n.fks, n.checks, n.indexes = stripAll(t.uniques), stripAll(t.fks), stripAll(t.checks), stripAll(t.indexes)
		out[i] = n
	}
	return out
}

// diffPostgresExact lists every way the live schema differs from ref, the reference: a missing
// object is in ref but not in live, an extra one in live but not in ref, a different one in both
// with another definition. It compares tables; columns (type, length, nullability, default,
// comment, order); constraints by name and definition, the primary key among them; indexes by name
// and definition; table comments; and owned sequences by type and owning column. Data is not
// compared: neither rows nor sequence positions (diffSequencePositions). Tables named in exclude
// are skipped. Both sides must already be normalized (normalizeSchema).
func diffPostgresExact(live, ref []pgTable, exclude ...string) []string {
	liveByName, refByName := tablesByName(live, exclude), tablesByName(ref, exclude)
	var out []string
	for _, name := range slices.Sorted(maps.Keys(refByName)) {
		l, ok := liveByName[name]
		if !ok {
			out = append(out, "missing table "+name)
			continue
		}
		out = append(out, diffTable(l, refByName[name])...)
	}
	for _, name := range slices.Sorted(maps.Keys(liveByName)) {
		if _, ok := refByName[name]; !ok {
			out = append(out, "extra table "+name)
		}
	}
	return out
}

// diffSequencePositions lists the owned sequences whose position differs between live and ref: the
// value they last handed out, or "unused". The baseline equivalence compares positions, so a
// database built from the baseline hands out the identifiers the history would; diffPostgresExact
// does not, since a position is data. Tables named in exclude are skipped; a sequence on one side
// only is diffPostgresExact's to report.
func diffSequencePositions(live, ref []pgTable, exclude ...string) []string {
	liveByName, refByName := tablesByName(live, exclude), tablesByName(ref, exclude)
	var out []string
	for _, name := range slices.Sorted(maps.Keys(refByName)) {
		positions := map[string]string{}
		for _, s := range liveByName[name].sequences {
			positions[s.name] = sequencePosition(s)
		}
		for _, r := range refByName[name].sequences {
			if l, ok := positions[r.name]; ok && l != sequencePosition(r) {
				out = append(out, fmt.Sprintf("different sequence position %s.%s: %s ≠ %s", name, r.name, l, sequencePosition(r)))
			}
		}
	}
	return out
}

func sequencePosition(s pgSequence) string {
	if s.lastValue == nil {
		return "unused"
	}
	return fmt.Sprint(*s.lastValue)
}

func diffTable(live, ref pgTable) []string {
	t := ref.name
	var out []string
	if live.comment != ref.comment {
		out = append(out, fmt.Sprintf("different comment %s: %q ≠ %q", t, live.comment, ref.comment))
	}
	out = append(out, diffColumns(t, live.columns, ref.columns)...)
	out = append(out, diffNamed("constraint", t, constraintDefs(live), constraintDefs(ref))...)
	out = append(out, diffNamed("index", t, indexDefs(live.indexes), indexDefs(ref.indexes))...)
	out = append(out, diffNamed("sequence", t, sequenceDefs(live), sequenceDefs(ref))...)
	return out
}

func diffColumns(t string, live, ref []pgColumn) []string {
	liveByName, refByName := map[string]pgColumn{}, map[string]pgColumn{}
	for _, c := range live {
		liveByName[c.name] = c
	}
	for _, c := range ref {
		refByName[c.name] = c
	}
	var out []string
	for _, r := range ref {
		l, ok := liveByName[r.name]
		if !ok {
			out = append(out, fmt.Sprintf("missing column %s.%s", t, r.name))
			continue
		}
		col := t + "." + r.name
		if a, b := columnType(l), columnType(r); a != b {
			out = append(out, fmt.Sprintf("different column %s: type %s ≠ %s", col, a, b))
		}
		if l.nullable != r.nullable {
			out = append(out, fmt.Sprintf("different column %s: %s ≠ %s", col, nullability(l), nullability(r)))
		}
		if l.def != r.def {
			out = append(out, fmt.Sprintf("different column %s: default %q ≠ %q", col, l.def, r.def))
		}
		if l.comment != r.comment {
			out = append(out, fmt.Sprintf("different column %s: comment %q ≠ %q", col, l.comment, r.comment))
		}
	}
	for _, l := range live {
		if _, ok := refByName[l.name]; !ok {
			out = append(out, fmt.Sprintf("extra column %s.%s", t, l.name))
		}
	}
	if a, b := sharedColumnOrder(live, refByName), sharedColumnOrder(ref, liveByName); !slices.Equal(a, b) {
		out = append(out, fmt.Sprintf("different column order %s: %s ≠ %s", t, strings.Join(a, ", "), strings.Join(b, ", ")))
	}
	return out
}

// sharedColumnOrder is the order of cols' columns that also exist in other.
func sharedColumnOrder(cols []pgColumn, other map[string]pgColumn) []string {
	var names []string
	for _, c := range cols {
		if _, ok := other[c.name]; ok {
			names = append(names, c.name)
		}
	}
	return names
}

func columnType(c pgColumn) string {
	if c.maxLen > 0 {
		return fmt.Sprintf("%s(%d)", c.udt, c.maxLen)
	}
	return c.udt
}

func nullability(c pgColumn) string {
	if c.nullable {
		return "NULL"
	}
	return "NOT NULL"
}

// diffNamed compares two name → definition maps of one kind of object of table t.
func diffNamed(kind, t string, live, ref map[string]string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(ref)) {
		l, ok := live[name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("missing %s %s.%s: %s", kind, t, name, ref[name]))
		case l != ref[name]:
			out = append(out, fmt.Sprintf("different %s %s.%s: %s ≠ %s", kind, t, name, l, ref[name]))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(live)) {
		if _, ok := ref[name]; !ok {
			out = append(out, fmt.Sprintf("extra %s %s.%s: %s", kind, t, name, live[name]))
		}
	}
	return out
}

func constraintDefs(t pgTable) map[string]string {
	defs := map[string]string{}
	for _, c := range t.constraints {
		defs[c.name] = c.def
	}
	return defs
}

var indexName = regexp.MustCompile(`^CREATE (?:UNIQUE )?INDEX (\S+) ON `)

func indexDefs(indexes []string) map[string]string {
	defs := map[string]string{}
	for _, def := range indexes {
		name := def
		if m := indexName.FindStringSubmatch(def); m != nil {
			name = m[1]
		}
		defs[name] = def
	}
	return defs
}

// sequenceDefs is each owned sequence's definition: its type and the column owning it. Its
// position is data, diffSequencePositions' to compare.
func sequenceDefs(t pgTable) map[string]string {
	defs := map[string]string{}
	for _, s := range t.sequences {
		defs[s.name] = fmt.Sprintf("%s owned by %s", s.dataType, s.column)
	}
	return defs
}

// tablesByName indexes tables by name, leaving out those named in exclude.
func tablesByName(tables []pgTable, exclude []string) map[string]pgTable {
	byName := map[string]pgTable{}
	for _, t := range tables {
		if !slices.Contains(exclude, t.name) {
			byName[t.name] = t
		}
	}
	return byName
}

// introspectSchemaDDL creates, in the current search_path, one of every construct the exact
// comparison covers: named constraints of each kind, a SERIAL sequence, a partial and a plain
// index, defaults, a varchar length and comments.
const introspectSchemaDDL = `
CREATE TABLE parent (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT parent_name_unique UNIQUE (name)
);
CREATE TABLE child (
    id        SERIAL PRIMARY KEY,
    parent_id UUID NOT NULL REFERENCES parent(id) ON DELETE CASCADE,
    status    TEXT NOT NULL DEFAULT 'new',
    note      TEXT,
    CONSTRAINT child_status_check CHECK (status IN ('new', 'done'))
);
CREATE INDEX idx_child_parent ON child (parent_id);
CREATE INDEX idx_child_open ON child (status) WHERE status = 'new';
COMMENT ON TABLE child IS 'children of a parent';
COMMENT ON COLUMN child.note IS 'free text';
INSERT INTO parent (name) VALUES ('p1');
`

func createIntrospectSchema(t *testing.T, ctx context.Context, conn *pgx.Conn, schema string) {
	t.Helper()
	for _, stmt := range []string{
		"CREATE SCHEMA " + schema,
		"SET search_path TO " + schema,
		introspectSchemaDDL,
		"RESET search_path",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("schema %s: %v", schema, err)
		}
	}
}

func introspectNormalized(t *testing.T, ctx context.Context, conn *pgx.Conn, schema string) []pgTable {
	t.Helper()
	tables, err := introspectPostgres(ctx, conn, schema)
	if err != nil {
		t.Fatalf("introspect %s: %v", schema, err)
	}
	if len(tables) != 2 {
		t.Fatalf("introspect %s: %d tables; want 2", schema, len(tables))
	}
	return normalizeSchema(tables, schema)
}

// TestDiffPostgresExact pins the exact comparison the equivalence test relies on (research R4): the
// same objects read from two schemas compare equal once each side's schema qualifier is stripped,
// and every difference is named.
func TestDiffPostgresExact(t *testing.T) {
	admin := adminPostgresURL(t)
	ctx := context.Background()
	url := scratchPostgres(t, admin, "lightstack_introspect_"+strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	createIntrospectSchema(t, ctx, conn, "a")
	createIntrospectSchema(t, ctx, conn, "b")

	a := introspectNormalized(t, ctx, conn, "a")
	if diffs := diffPostgresExact(a, introspectNormalized(t, ctx, conn, "b")); len(diffs) > 0 {
		t.Fatalf("identical schemas differ:\n%s", strings.Join(diffs, "\n"))
	}

	// State is not schema: a row drawn from b's sequence moves its position, which only
	// diffSequencePositions reports.
	if _, err := conn.Exec(ctx, "INSERT INTO b.child (parent_id) SELECT id FROM b.parent"); err != nil {
		t.Fatal(err)
	}
	used := introspectNormalized(t, ctx, conn, "b")
	if diffs := diffPostgresExact(used, a); len(diffs) > 0 {
		t.Errorf("a used sequence reads as a schema difference:\n%s", strings.Join(diffs, "\n"))
	}
	if diffs := diffSequencePositions(used, a); len(diffs) != 1 || !strings.Contains(diffs[0], "different sequence position child.child_id_seq: 1 ≠ unused") {
		t.Errorf("diffSequencePositions = %q; want child.child_id_seq at 1 against unused", diffs)
	}

	for _, stmt := range []string{
		"ALTER TABLE b.child ALTER COLUMN status SET DEFAULT 'done'",
		"CREATE INDEX idx_child_extra ON b.child (parent_id, status)",
		"ALTER TABLE b.child DROP COLUMN note",
		"ALTER TABLE b.parent DROP CONSTRAINT parent_name_unique",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	diffs := diffPostgresExact(introspectNormalized(t, ctx, conn, "b"), a)
	joined := strings.Join(diffs, "\n")
	for _, want := range []string{
		"different column child.status: default",
		"extra index child.idx_child_extra",
		"missing column child.note",
		"missing constraint parent.parent_name_unique",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("diff does not name %q:\n%s", want, joined)
		}
	}

	// Excluded tables are not compared at all.
	if diffs := diffPostgresExact(introspectNormalized(t, ctx, conn, "b"), a, "child", "parent"); len(diffs) > 0 {
		t.Errorf("excluded tables still differ:\n%s", strings.Join(diffs, "\n"))
	}
	if diffs := diffSequencePositions(used, a, "child"); len(diffs) > 0 {
		t.Errorf("an excluded table's sequence still differs:\n%s", strings.Join(diffs, "\n"))
	}
}

// TestNormalizeSchemaStripsOnlyTheSchemaQualifier checks the qualifier is removed as a whole
// identifier: a schema named a must not eat the "a." inside data.x.
func TestNormalizeSchemaStripsOnlyTheSchemaQualifier(t *testing.T) {
	in := []pgTable{{
		name:        "t",
		columns:     []pgColumn{{name: "id", def: "nextval('a.t_id_seq'::regclass)"}},
		indexes:     []string{"CREATE INDEX i ON a.t USING btree (x) WHERE (data.x > 0)"},
		constraints: []pgConstraint{{name: "f", kind: "f", def: `FOREIGN KEY (p) REFERENCES "a".parent(id)`}},
		fks:         []string{"FOREIGN KEY (p) REFERENCES a.parent(id)"},
	}}
	out := normalizeSchema(in, "a")
	if got := out[0].columns[0].def; got != "nextval('t_id_seq'::regclass)" {
		t.Errorf("default = %q", got)
	}
	if got := out[0].indexes[0]; got != "CREATE INDEX i ON t USING btree (x) WHERE (data.x > 0)" {
		t.Errorf("index = %q", got)
	}
	if got := out[0].constraints[0].def; got != "FOREIGN KEY (p) REFERENCES parent(id)" {
		t.Errorf("constraint = %q", got)
	}
	if got := out[0].fks[0]; got != "FOREIGN KEY (p) REFERENCES parent(id)" {
		t.Errorf("fk = %q", got)
	}
	if in[0].indexes[0] != "CREATE INDEX i ON a.t USING btree (x) WHERE (data.x > 0)" {
		t.Errorf("normalizeSchema changed its input")
	}
}
