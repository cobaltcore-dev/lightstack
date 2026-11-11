package database

import (
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The static SQL check (FR-018, research R9, contracts/sql-dialect.md §3 and §5): every SQL
// string in the code must run on both backends, so a construct only PostgreSQL has fails
// here, naming file, line and construct, before any test runs the query.
//
// A string counts as SQL when it holds a statement keyword, or when it is handed to the
// SQL builder (common.SQLFilter.Cond, SQLUpdate.SetExpr). Functions are checked against an
// allow-list — the open-ended class — and syntax against a deny-list — the finite one.
// Casts, list membership, row locks and the interval form are checked by the translator
// itself. A literal that must stay PostgreSQL-only carries //sqlcheck:postgres-only on its
// line or the line above, and its test runs behind dbtest.RequirePostgres.
//
// The database package's own tests are not scanned: they exercise the dialects themselves.

var sqlKeyword = regexp.MustCompile(`\b(SELECT|INSERT|UPDATE|DELETE|WITH|FROM|WHERE|SET|VALUES|JOIN|RETURNING|ORDER BY|GROUP BY)\b`)

// portableFunctions are the functions both backends have with one meaning. The SQLite
// ones are SQLite's own or registered by sqlite_funcs.go.
var portableFunctions = map[string]bool{
	"count": true, "sum": true, "min": true, "max": true, "avg": true, "coalesce": true, "nullif": true,
	"lower": true, "upper": true, "length": true, "abs": true, "round": true, "now": true,
	"gen_random_uuid": true, "pg_advisory_xact_lock": true, "hashtext": true, "make_interval": true,
	"row_number": true, "exists": true, "cast": true, "replace": true,
}

// notFunctions are words a parenthesis may follow without a function call: keywords, and
// the name of a table or common table expression before its column list.
var notFunctions = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`IN VALUES EXISTS ANY OVER AND OR NOT ON USING AS KEY UNIQUE CHECK
		FILTER WHERE SELECT CONFLICT DEFAULT INTO JOIN FROM LIMIT OFFSET SET THEN ELSE WHEN CASE
		RETURNING BY DISTINCT ALL UNION IS LIKE BETWEEN REGEXP PRIMARY FOREIGN REFERENCES TABLE INDEX
		RECURSIVE WITH EXCEPT INTERSECT`) {
		notFunctions[w] = true
	}
}

// tableNameBefore are the keywords after which a name followed by a parenthesis is a table
// or common table expression with its column list, not a call. After FROM or JOIN it is a
// call: a table-valued function.
var tableNameBefore = map[string]bool{"INTO": true, "TABLE": true, "REFERENCES": true, "RECURSIVE": true, "WITH": true}

// messageFuncs take text for people, not SQL, however it reads.
var messageFuncs = map[string]bool{"Error": true, "Errorf": true, "Fatal": true, "Fatalf": true, "Log": true,
	"Logf": true, "Skip": true, "Skipf": true, "Msg": true, "Msgf": true, "New": true, "Print": true, "Printf": true,
	"Println": true}

// jsonbConcat is || with a jsonb operand on either side.
var jsonbConcat = regexp.MustCompile(`(?i)\|\|\s*\S*::jsonb?\b|::jsonb?\s*\|\|`)

// formatVerb is a fmt verb in an SQL template; it is replaced by a neutral stand-in before
// the template is checked.
var formatVerb = regexp.MustCompile(`%(\[\d+\])?[-+# 0-9]*[dsvq]`)

// sqlIssues lists what in sql only PostgreSQL runs.
func sqlIssues(sql string) []string {
	sql = formatVerb.ReplaceAllStringFunc(sql, func(v string) string {
		if strings.HasSuffix(v, "d") {
			return "1"
		}
		return "x"
	})
	toks, err := lexSQL(sql)
	if err != nil {
		return []string{err.Error()}
	}
	var issues []string
	if _, err := translateTokens(toks); err != nil {
		issues = append(issues, err.Error())
	}

	var code strings.Builder
	var sig []token // significant tokens: no whitespace or comments
	for _, t := range toks {
		if t.kind == tkSpace {
			code.WriteByte(' ')
			continue
		}
		sig = append(sig, t)
		if t.kind != tkString && t.kind != tkQuoted {
			code.WriteString(t.text)
		}
	}
	for _, op := range []string{"@>", "<@", "?|", "?&", "#>"} {
		if strings.Contains(code.String(), op) {
			issues = append(issues, fmt.Sprintf("the %s operator is not supported on SQLite", op))
		}
	}
	// || merges JSON documents on PostgreSQL and concatenates text on SQLite: the statement
	// runs on both and means two things. A jsonb operand beside it gives it away.
	if jsonbConcat.MatchString(code.String()) {
		issues = append(issues, "|| on jsonb merges documents on PostgreSQL but concatenates text on SQLite")
	}

	word := func(i int) string {
		if i < 0 || i >= len(sig) || sig[i].kind != tkIdent {
			return ""
		}
		return strings.ToUpper(sig[i].text)
	}
	for i, t := range sig {
		w := word(i)
		switch {
		case w == "ILIKE", w == "LATERAL", w == "TRUNCATE", w == "LISTEN", w == "NOTIFY", w == "NOWAIT",
			w == "CURRENT_TIMESTAMP", w == "INTERVAL":
			issues = append(issues, w+" is not supported on SQLite")
		case w == "SIMILAR" && word(i+1) == "TO", w == "DISTINCT" && word(i+1) == "ON", w == "SKIP" && word(i+1) == "LOCKED":
			issues = append(issues, w+" "+word(i+1)+" is not supported on SQLite")
		case w == "ARRAY" && i+1 < len(sig) && sig[i+1].is(tkPunct, "["):
			issues = append(issues, "ARRAY[…] is not supported on SQLite")
		case w == "CONFLICT" && word(i+1) == "ON" && word(i+2) == "CONSTRAINT":
			issues = append(issues, "ON CONFLICT ON CONSTRAINT is not supported on SQLite")
		case w == "DELETE" && word(i+1) == "FROM" && word(i+3) == "USING":
			issues = append(issues, "DELETE … USING is not supported on SQLite")
		case (w == "DELETE" && word(i+1) == "FROM" && aliasWithoutAs(word(i+3))) ||
			(w == "UPDATE" && word(i-1) != "DO" && aliasWithoutAs(word(i+2)) && word(i+3) == "SET"):
			issues = append(issues, "a table alias without AS in "+w+" is not supported on SQLite")
		case t.kind == tkIdent && i+1 < len(sig) && sig[i+1].is(tkPunct, "(") && adjacent(toks, t, sig[i+1]) &&
			!notFunctions[w] && !tableNameBefore[word(i-1)] && !indexedTable(word, i) && !portableFunctions[strings.ToLower(t.text)]:
			issues = append(issues, fmt.Sprintf("the function %s is not available on SQLite", strings.ToLower(t.text)))
		}
	}
	return issues
}

// indexedTable reports the table of CREATE INDEX … ON t(cols) at position i.
func indexedTable(word func(int) string, i int) bool {
	if word(i-1) != "ON" {
		return false
	}
	for j := i - 2; j >= i-6; j-- {
		if word(j) == "INDEX" {
			return true
		}
	}
	return false
}

// aliasWithoutAs reports a word in the place of a table alias: any name that is not the
// keyword that may follow the table.
func aliasWithoutAs(w string) bool {
	return w != "" && w != "WHERE" && w != "USING" && w != "RETURNING" && w != "AS" && w != "SET"
}

// adjacent reports whether b directly follows a in toks, with no whitespace between.
func adjacent(toks []token, a, b token) bool {
	for i := 0; i+1 < len(toks); i++ {
		if toks[i] == a && toks[i+1] == b {
			return true
		}
	}
	return false
}

// sqlFinding is one construct the check refuses, where it is.
type sqlFinding struct {
	pos   gotoken.Position
	issue string
}

func (f sqlFinding) String() string {
	return fmt.Sprintf("%s:%d: %s", f.pos.Filename, f.pos.Line, f.issue)
}

// checkGoFile reports the refused constructs in the SQL strings of one Go file.
func checkGoFile(fset *gotoken.FileSet, file *ast.File) []sqlFinding {
	exempt := map[int]bool{}
	for _, group := range file.Comments {
		for _, c := range group.List {
			if strings.Contains(c.Text, "sqlcheck:postgres-only") {
				line := fset.Position(c.Pos()).Line
				exempt[line], exempt[line+1] = true, true
			}
		}
	}
	var findings []sqlFinding
	report := func(at ast.Node, sql string, builder bool) {
		pos := fset.Position(at.Pos())
		if exempt[pos.Line] || (!builder && !sqlKeyword.MatchString(sql)) {
			return
		}
		for _, issue := range sqlIssues(sql) {
			findings = append(findings, sqlFinding{pos, issue})
		}
	}
	seen := map[ast.Node]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			name := ""
			switch fn := x.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			if messageFuncs[name] {
				for _, a := range x.Args {
					markSeen(a, seen)
				}
			}
			if (name == "Cond" || name == "SetExpr") && len(x.Args) == 1 && !seen[x.Args[0]] {
				if s, ok := foldString(x.Args[0]); ok {
					report(x.Args[0], s, true)
					markSeen(x.Args[0], seen)
				}
			}
		case *ast.BinaryExpr:
			if seen[x] || x.Op != gotoken.ADD {
				return true
			}
			if s, ok := foldString(x); ok {
				report(x, s, false)
				markSeen(x, seen)
				return false
			}
		case *ast.BasicLit:
			if !seen[x] && x.Kind == gotoken.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil {
					report(x, s, false)
				}
			}
		}
		return true
	})
	return findings
}

// foldString folds a string literal, or a + of strings, to its text. A part that is not a
// literal stands in as what it renders at run time: the SQL builder's Arg as a placeholder,
// anything else — a variable, an alias — as the name x, so the statement around it still
// parses. At least one part must be a literal.
func foldString(e ast.Expr) (string, bool) {
	s, literal := fold(e)
	return s, literal
}

func fold(e ast.Expr) (text string, literal bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == gotoken.STRING {
			if s, err := strconv.Unquote(x.Value); err == nil {
				return s, true
			}
		}
	case *ast.ParenExpr:
		return fold(x.X)
	case *ast.BinaryExpr:
		if x.Op == gotoken.ADD {
			a, la := fold(x.X)
			b, lb := fold(x.Y)
			return a + b, la || lb
		}
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Arg" {
			return "$1", false
		}
	}
	return "x", false
}

func markSeen(n ast.Node, seen map[ast.Node]bool) {
	ast.Inspect(n, func(m ast.Node) bool {
		if m != nil {
			seen[m] = true
		}
		return true
	})
}

// TestSQLConstructs runs the check over the tree (SC-009).
func TestSQLConstructs(t *testing.T) {
	fset := gotoken.NewFileSet()
	var findings []sqlFinding
	for _, root := range []string{"../../internal", "../../test", "../../cmd", "../../pkg"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if filepath.Dir(path) == filepath.Clean("../../internal/database") && strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			findings = append(findings, checkGoFile(fset, file)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	for _, f := range findings {
		lines = append(lines, strings.TrimPrefix(f.String(), "../../"))
	}
	sort.Strings(lines)
	if len(lines) > 0 {
		t.Errorf("%d SQL constructs SQLite cannot run (contracts/sql-dialect.md §3):\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// TestSQLConstructsSeeded keeps the check honest (SC-009): one construct of every refused
// class, each in a file of its own, must be reported with its file, line and construct.
func TestSQLConstructsSeeded(t *testing.T) {
	for construct, sql := range map[string]string{
		"~*":                  `SELECT id FROM t WHERE name ~* $1`,
		"!~":                  `SELECT id FROM t WHERE name !~ $1`,
		"ILIKE":               `SELECT id FROM t WHERE name ILIKE $1`,
		"@>":                  `SELECT id FROM ports WHERE fixed_ips @> $1::jsonb`,
		"jsonb_build_object":  `SELECT jsonb_build_object('a', 1) FROM t`,
		"array_agg":           `SELECT ARRAY_AGG(name) FROM t`,
		"ARRAY[":              `INSERT INTO t (tags) VALUES (ARRAY['a'])`,
		"DISTINCT ON":         `SELECT DISTINCT ON (network_id) id FROM subnets`,
		"generate_series":     `SELECT g FROM generate_series(1, 3) AS g`,
		"extract":             `SELECT EXTRACT(EPOCH FROM now()) FROM t`,
		"INTERVAL":            `UPDATE t SET at = now() - interval '1 minute'`,
		"CURRENT_TIMESTAMP":   `UPDATE t SET at = CURRENT_TIMESTAMP WHERE id = $1`,
		"FOR SHARE":           `SELECT id FROM t WHERE id = $1 FOR SHARE`,
		"dollar quoting":      `SELECT $$text$$ FROM t`,
		"cast to regclass":    `SELECT 'groups'::regclass FROM t`,
		"ANY":                 `SELECT id FROM t WHERE id = ANY(ARRAY[$1])`,
		"date_trunc":          `SELECT date_trunc('day', created_at) FROM t`,
		"alias without AS":    `DELETE FROM instance_actions a WHERE a.id = $1`,
		"ON CONFLICT ON":      `INSERT INTO t (id) VALUES ($1) ON CONFLICT ON CONSTRAINT t_pkey DO NOTHING`,
		"builder fragment @>": `f.Cond("p.fixed_ips @> $1")`,
		"jsonb ||":            `UPDATE host_aggregates SET metadata = metadata || $1::jsonb WHERE uuid = $2`,
	} {
		t.Run(construct, func(t *testing.T) {
			src := "package seed\n\nfunc seeded() {\n\tq := `" + sql + "`\n\t_ = q\n}\n"
			if strings.HasPrefix(sql, "f.Cond(") {
				src = "package seed\n\nfunc seeded() {\n\t" + sql + "\n}\n"
			}
			fset := gotoken.NewFileSet()
			file, err := parser.ParseFile(fset, "seed_"+strings.ReplaceAll(construct, " ", "_")+".go", src, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			findings := checkGoFile(fset, file)
			if len(findings) == 0 {
				t.Fatalf("%q was not reported", sql)
			}
			if f := findings[0]; f.pos.Line != 4 || !strings.HasPrefix(f.pos.Filename, "seed_") {
				t.Errorf("reported at %s; want the seeded file, line 4", f)
			}
		})
	}
	// And the exemption holds.
	src := "package seed\n\nfunc seeded() {\n\t//sqlcheck:postgres-only\n\tq := `SELECT 'groups'::regclass FROM t`\n\t_ = q\n}\n"
	fset := gotoken.NewFileSet()
	file, err := parser.ParseFile(fset, "seed_exempt.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if findings := checkGoFile(fset, file); len(findings) != 0 {
		t.Errorf("an exempt literal was reported: %v", findings)
	}
}
