package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The schema model the parity and round-trip tests compare (research R11): what a
// database's schema means to Lightstack, normalised so that the two backends' spellings
// of one thing compare equal — VARCHAR(n) and TEXT with a length CHECK, a boolean and
// 0/1, now() and CURRENT_TIMESTAMP, a UUID and its text.

type modelColumn struct {
	name    string
	family  string // text, integer, real
	notNull bool
	def     string // normalised default; "" for none
	maxLen  int    // VARCHAR(n) on PostgreSQL, CHECK (length(col) <= n) on SQLite; 0 for none
}

type modelTable struct {
	columns []modelColumn
	pk      []string
	uniques []string // "a,b"
	fks     []string // "a->t(b) on delete cascade"
	checks  []string
	indexes []string // "[unique ]name(cols)[ where pred]"
	rows    []string // seed rows, normalised and sorted
}

type schemaModel map[string]*modelTable

var (
	lengthCheck = regexp.MustCompile(`^length(\w+)<=(\d+)$`) // normalised: no quotes or parentheses
	uniqueDef   = regexp.MustCompile(`^UNIQUE \((.*)\)$`)
	foreignDef  = regexp.MustCompile(`^FOREIGN KEY \((.*?)\) REFERENCES ([\w."]+)\((.*?)\)(.*)$`)
	onDelete    = regexp.MustCompile(`ON DELETE (CASCADE|SET NULL|SET DEFAULT|RESTRICT|NO ACTION)`)
)

// normalizeExpr reduces an SQL expression to a form both backends' spellings share.
func normalizeExpr(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer(" ", "", "\n", "", "\t", "", `"`, "", "(", "", ")", "").Replace(s)
}

func normalizeCols(s string) string {
	parts := strings.Split(s, ",")
	for i, p := range parts {
		parts[i] = strings.ToLower(strings.Trim(strings.TrimSpace(p), `"`))
	}
	return strings.Join(parts, ",")
}

// normalizeDefault strips the parentheses SQLite's DEFAULT expressions carry.
func normalizeDefault(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// normalizeValue renders one seed value as both backends' value of it renders. An
// install-time value — the time of the migration, or an id it generated — is masked.
func normalizeValue(v any, def, source string) string {
	if def == "now()" {
		return "<install time>"
	}
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int64, int32, int16, int:
		return fmt.Sprint(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case time.Time:
		return formatTime(x)
	case [16]uint8:
		return normalizeValue(uuid.UUID(x).String(), def, source)
	case []byte:
		return normalizeValue(string(x), def, source)
	case map[string]any, []any, []string:
		b, _ := json.Marshal(x)
		return string(b)
	case string:
		if id, err := uuid.Parse(x); err == nil && len(x) == 36 {
			if def == "gen_random_uuid()" && id.Version() == 4 && !strings.Contains(source, x) {
				return "<generated id>"
			}
			return x
		}
		if strings.HasPrefix(x, "{") || strings.HasPrefix(x, "[") {
			var doc any
			if json.Unmarshal([]byte(x), &doc) == nil {
				b, _ := json.Marshal(doc)
				return string(b)
			}
		}
		return strconv.Quote(x)
	}
	return fmt.Sprintf("%T(%v)", v, v)
}

func normalizeRow(values []any, cols []modelColumn, source string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = cols[i].name + "=" + normalizeValue(v, cols[i].def, source)
	}
	return strings.Join(parts, " ")
}

// postgresModel builds the model of an introspected PostgreSQL schema. source is the
// text of the migrations, which tells ids they spell out from ids they generate.
func postgresModel(tables []pgTable, source string) (schemaModel, error) {
	m := schemaModel{}
	for _, t := range tables {
		mt := &modelTable{pk: t.pk}
		pk := map[string]bool{}
		for _, c := range t.pk {
			pk[c] = true
		}
		for _, c := range t.columns {
			typ, err := sqliteColumnType(c)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", t.name, c.name, err)
			}
			def, err := sqliteColumnDefault(c)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", t.name, c.name, err)
			}
			col := modelColumn{name: c.name, family: strings.ToLower(typ), notNull: !c.nullable || pk[c.name], def: normalizeDefault(def)}
			if c.udt == "varchar" {
				col.maxLen = c.maxLen
			}
			mt.columns = append(mt.columns, col)
		}
		for _, u := range t.uniques {
			if mm := uniqueDef.FindStringSubmatch(u); mm != nil {
				mt.uniques = append(mt.uniques, normalizeCols(mm[1]))
			}
		}
		for _, f := range t.fks {
			mm := foreignDef.FindStringSubmatch(f)
			if mm == nil {
				return nil, fmt.Errorf("%s: foreign key %q not understood", t.name, f)
			}
			action := "no action"
			if a := onDelete.FindStringSubmatch(mm[4]); a != nil {
				action = strings.ToLower(a[1])
			}
			mt.fks = append(mt.fks, fmt.Sprintf("%s->%s(%s) on delete %s", normalizeCols(mm[1]), strings.Trim(mm[2], `"`), normalizeCols(mm[3]), action))
		}
		for _, c := range t.checks {
			if mapped, ok := sqliteChecks[c]; ok {
				c = mapped
			}
			mt.checks = append(mt.checks, normalizeExpr(strings.TrimPrefix(c, "CHECK ")))
		}
		for _, def := range t.indexes {
			mm := indexDef.FindStringSubmatch(def)
			if mm == nil {
				return nil, fmt.Errorf("%s: index %q not understood", t.name, def)
			}
			if _, skipped := skippedIndexes[mm[2]]; skipped {
				continue
			}
			mt.indexes = append(mt.indexes, modelIndex(mm[1] != "", mm[2], mm[5], mm[6]))
		}
		for _, r := range t.rows {
			mt.rows = append(mt.rows, normalizeRow(r, mt.columns, source))
		}
		m[t.name] = mt.sorted()
	}
	return m, nil
}

func modelIndex(unique bool, name, cols, where string) string {
	s := ""
	if unique {
		s = "unique "
	}
	s += strings.ToLower(name) + "(" + normalizeExpr(strings.ReplaceAll(cols, ",", "|")) + ")"
	if where != "" {
		s += " where " + normalizeExpr(where)
	}
	return s
}

func (t *modelTable) sorted() *modelTable {
	for _, l := range [][]string{t.uniques, t.fks, t.checks, t.indexes, t.rows} {
		sort.Strings(l)
	}
	return t
}

// sqliteModel builds the model of a SQLite database's schema from its catalog.
func sqliteModel(ctx context.Context, db *sql.DB, source string) (schemaModel, error) {
	type table struct{ name, sql string }
	var tables []table
	rows, err := db.QueryContext(ctx, `SELECT name, sql FROM sqlite_schema WHERE type = 'table'
		AND name NOT IN ('schema_migrations', 'sqlite_sequence') ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t table
		if err := rows.Scan(&t.name, &t.sql); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, t)
	}
	rows.Close()

	m := schemaModel{}
	for _, t := range tables {
		mt := &modelTable{}
		maxLen, checks, err := sqliteChecksOf(t.sql)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
		mt.checks = checks
		cols, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_xinfo(%s)`, quoteIdent(t.name)))
		if err != nil {
			return nil, err
		}
		pkOrder := map[int]string{}
		for cols.Next() {
			var cid, notNull, pk, hidden int
			var name, typ string
			var def sql.NullString
			if err := cols.Scan(&cid, &name, &typ, &notNull, &def, &pk, &hidden); err != nil {
				cols.Close()
				return nil, err
			}
			mt.columns = append(mt.columns, modelColumn{name: name, family: strings.ToLower(typ),
				notNull: notNull == 1 || pk > 0, def: normalizeDefault(def.String), maxLen: maxLen[name]})
			if pk > 0 {
				pkOrder[pk] = name
			}
		}
		cols.Close()
		for i := 1; i <= len(pkOrder); i++ {
			mt.pk = append(mt.pk, pkOrder[i])
		}

		fks, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA foreign_key_list(%s)`, quoteIdent(t.name)))
		if err != nil {
			return nil, err
		}
		type fk struct {
			from, to      []string
			table, action string
		}
		byID := map[int]*fk{}
		for fks.Next() {
			var id, seq int
			var parent, from, onUpdate, onDel, match string
			var to sql.NullString
			if err := fks.Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDel, &match); err != nil {
				fks.Close()
				return nil, err
			}
			f := byID[id]
			if f == nil {
				f = &fk{table: parent, action: strings.ToLower(onDel)}
				byID[id] = f
			}
			f.from = append(f.from, strings.ToLower(from))
			f.to = append(f.to, strings.ToLower(to.String))
		}
		fks.Close()
		for _, f := range byID {
			mt.fks = append(mt.fks, fmt.Sprintf("%s->%s(%s) on delete %s", strings.Join(f.from, ","), f.table, strings.Join(f.to, ","), f.action))
		}

		idx, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_list(%s)`, quoteIdent(t.name)))
		if err != nil {
			return nil, err
		}
		type index struct {
			name, origin string
			unique       bool
		}
		var indexes []index
		for idx.Next() {
			var seq, unique, partial int
			var ix index
			if err := idx.Scan(&seq, &ix.name, &unique, &ix.origin, &partial); err != nil {
				idx.Close()
				return nil, err
			}
			ix.unique = unique == 1
			indexes = append(indexes, ix)
		}
		idx.Close()
		for _, ix := range indexes {
			if ix.origin == "pk" {
				continue
			}
			cols, err := sqliteIndexColumns(ctx, db, ix.name)
			if err != nil {
				return nil, err
			}
			if ix.origin == "u" {
				mt.uniques = append(mt.uniques, strings.Join(cols, ","))
				continue
			}
			var stmt string
			if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type = 'index' AND name = ?`, ix.name).Scan(&stmt); err != nil {
				return nil, err
			}
			where := ""
			if i := strings.Index(strings.ToUpper(stmt), " WHERE "); i >= 0 {
				where = stmt[i+len(" WHERE "):]
			}
			mt.indexes = append(mt.indexes, modelIndex(ix.unique, ix.name, strings.Join(cols, ","), where))
		}

		data, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT * FROM %s`, quoteIdent(t.name)))
		if err != nil {
			return nil, err
		}
		for data.Next() {
			vals := make([]any, len(mt.columns))
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := data.Scan(ptrs...); err != nil {
				data.Close()
				return nil, err
			}
			mt.rows = append(mt.rows, normalizeRow(vals, mt.columns, source))
		}
		data.Close()
		m[t.name] = mt.sorted()
	}
	return m, nil
}

func sqliteIndexColumns(ctx context.Context, db *sql.DB, index string) ([]string, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_xinfo(%s)`, quoteIdent(index)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var seq, cid, desc, key int
		var name sql.NullString
		var coll string
		if err := rows.Scan(&seq, &cid, &name, &desc, &coll, &key); err != nil {
			return nil, err
		}
		if key == 0 {
			continue
		}
		c := strings.ToLower(name.String)
		if !name.Valid {
			c = "<expression>"
		}
		if desc == 1 {
			c += " desc"
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// sqliteChecksOf reads the CHECK constraints of a CREATE TABLE statement. A length check
// on one column is that column's length limit; the others are returned normalised.
func sqliteChecksOf(createTable string) (maxLen map[string]int, checks []string, err error) {
	toks, err := lexSQL(createTable)
	if err != nil {
		return nil, nil, err
	}
	maxLen = map[string]int{}
	for i := 0; i < len(toks); i++ {
		if !toks[i].isIdent("CHECK") {
			continue
		}
		j := next(toks, i+1)
		if j >= len(toks) || !toks[j].is(tkPunct, "(") {
			return nil, nil, fmt.Errorf("CHECK without an expression")
		}
		depth, k := 0, j
		var b strings.Builder
		for ; k < len(toks); k++ {
			switch {
			case toks[k].is(tkPunct, "("):
				depth++
			case toks[k].is(tkPunct, ")"):
				depth--
			}
			b.WriteString(toks[k].text)
			if depth == 0 {
				break
			}
		}
		expr := normalizeExpr(b.String())
		if mm := lengthCheck.FindStringSubmatch(expr); mm != nil {
			n, _ := strconv.Atoi(mm[2])
			maxLen[mm[1]] = n
		} else {
			checks = append(checks, expr)
		}
		i = k
	}
	return maxLen, checks, nil
}

// diffModels lists how b differs from a.
func diffModels(a, b schemaModel, aName, bName string) []string {
	var out []string
	for name := range a {
		if b[name] == nil {
			out = append(out, fmt.Sprintf("table %s: only on %s", name, aName))
		}
	}
	for name := range b {
		if a[name] == nil {
			out = append(out, fmt.Sprintf("table %s: only on %s", name, bName))
		}
	}
	for name, ta := range a {
		tb := b[name]
		if tb == nil {
			continue
		}
		if len(ta.columns) != len(tb.columns) {
			out = append(out, fmt.Sprintf("table %s: %d columns on %s, %d on %s", name, len(ta.columns), aName, len(tb.columns), bName))
		} else {
			for i := range ta.columns {
				if ta.columns[i] != tb.columns[i] {
					out = append(out, fmt.Sprintf("table %s column %d: %+v on %s, %+v on %s", name, i, ta.columns[i], aName, tb.columns[i], bName))
				}
			}
		}
		for _, l := range []struct {
			what   string
			la, lb []string
		}{
			{"primary key", ta.pk, tb.pk}, {"unique", ta.uniques, tb.uniques}, {"foreign keys", ta.fks, tb.fks},
			{"checks", ta.checks, tb.checks}, {"indexes", ta.indexes, tb.indexes}, {"seed rows", ta.rows, tb.rows},
		} {
			if strings.Join(l.la, "\n") != strings.Join(l.lb, "\n") {
				out = append(out, fmt.Sprintf("table %s %s:\n  %s: %q\n  %s: %q", name, l.what, aName, l.la, bName, l.lb))
			}
		}
	}
	sort.Strings(out)
	return out
}
