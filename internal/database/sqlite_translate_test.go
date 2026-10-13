package database

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestPrepareSQLiteDoesNotMutateCallerArgs: prepareSQLite converts arguments in place, so the
// no-list expand path must hand it a clone — a caller reusing its slice keeps its own values.
func TestPrepareSQLiteDoesNotMutateCallerArgs(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	args := []any{ts}
	_, out, err := prepareSQLite(`SELECT $1::text`, args)
	if err != nil {
		t.Fatalf("prepareSQLite: %v", err)
	}
	if _, ok := args[0].(time.Time); !ok {
		t.Fatalf("caller argument was mutated to %T; want it left a time.Time", args[0])
	}
	if _, ok := out[0].(string); !ok {
		t.Fatalf("returned argument = %T; want the bound string", out[0])
	}
}

// TestTranslate pins the PostgreSQL → SQLite rewrite rules of research R5 and
// contracts/sql-dialect.md §2. Each case gives the statement as callers write it, the
// arguments, and the exact SQLite text and arguments that reach the driver.
func TestTranslate(t *testing.T) {
	ids := []string{"a", "b", "c"}
	cases := []struct {
		name     string
		sql      string
		args     []any
		wantSQL  string
		wantArgs []any
	}{
		{"plain statement passes through",
			`SELECT id, name FROM users WHERE id = $1`, []any{"u"},
			`SELECT id, name FROM users WHERE id = $1`, []any{"u"}},
		{"text cast on a column",
			`SELECT id::text FROM users WHERE id::text = $1`, []any{"u"},
			`SELECT CAST(id AS TEXT) FROM users WHERE CAST(id AS TEXT) = $1`, []any{"u"}},
		{"text cast on a qualified column and a placeholder",
			`SELECT p.device_id::text FROM ports p WHERE p.network_id = $1::text`, []any{"n"},
			`SELECT CAST(p.device_id AS TEXT) FROM ports p WHERE p.network_id = CAST($1 AS TEXT)`, []any{"n"}},
		{"varchar cast becomes text",
			`SELECT $1::varchar(36)`, []any{"x"},
			`SELECT CAST($1 AS TEXT)`, []any{"x"}},
		{"integer casts",
			`SELECT $1::int, $2::bigint, count(*)::integer FROM t`, []any{1, 2},
			`SELECT CAST($1 AS INTEGER), CAST($2 AS INTEGER), CAST(count(*) AS INTEGER) FROM t`, []any{1, 2}},
		{"other casts are dropped",
			`SELECT '{}'::jsonb, $1::uuid, $2::timestamptz, NULLIF($3, '')::uuid`, []any{"u", "t", ""},
			`SELECT '{}', $1, $2, NULLIF($3, '')`, []any{"u", "t", ""}},
		{"a cast on a parenthesised expression",
			`SELECT (a || b)::text FROM t`, nil,
			`SELECT CAST((a || b) AS TEXT) FROM t`, nil},
		{"= ANY expands a list, later placeholders renumbered",
			`SELECT id FROM t WHERE id = ANY($1) AND project_id = $2`, []any{ids, "p"},
			`SELECT id FROM t WHERE id IN ($1, $2, $3) AND project_id = $4`, []any{"a", "b", "c", "p"}},
		{"= ANY with an array cast expands the same way",
			`DELETE FROM a WHERE volume_id = ANY($1::uuid[])`, []any{ids[:2]},
			`DELETE FROM a WHERE volume_id IN ($1, $2)`, []any{"a", "b"}},
		{"a list after a scalar",
			`SELECT 1 FROM t WHERE x = $1 AND y = ANY($2)`, []any{"x", []string{"q"}},
			`SELECT 1 FROM t WHERE x = $1 AND y IN ($2)`, []any{"x", "q"}},
		{"an empty list matches nothing",
			`SELECT id FROM t WHERE id = ANY($1)`, []any{[]string{}},
			`SELECT id FROM t WHERE id IN ()`, []any{}},
		{"a placeholder used twice keeps one argument",
			`SELECT 1 FROM t WHERE a = $1 OR b = $1`, []any{"v"},
			`SELECT 1 FROM t WHERE a = $1 OR b = $1`, []any{"v"}},
		{"a LIMIT or OFFSET placeholder reads NULL as PostgreSQL does",
			`SELECT id FROM t ORDER BY id LIMIT $1 OFFSET $2`, []any{nil, 0},
			`SELECT id FROM t ORDER BY id LIMIT COALESCE($1, -1) OFFSET COALESCE($2, 0)`, []any{nil, 0}},
		{"a literal LIMIT is left alone",
			`SELECT id FROM t LIMIT 1`, nil,
			`SELECT id FROM t LIMIT 1`, nil},
		{"FOR UPDATE is removed",
			"SELECT hard_limit FROM quotas WHERE project_id = $1 FOR UPDATE", []any{"p"},
			"SELECT hard_limit FROM quotas WHERE project_id = $1", []any{"p"}},
		{"console expiry arithmetic",
			`INSERT INTO c (a, expires_at) VALUES ($1, now() + make_interval(secs => $2))`, []any{"x", 30},
			`INSERT INTO c (a, expires_at) VALUES ($1, ls_add_seconds(now(), $2))`, []any{"x", 30}},
		{"regular-expression match becomes REGEXP",
			`SELECT id FROM instances WHERE name ~ $1`, []any{"^web"},
			`SELECT id FROM instances WHERE name REGEXP $1`, []any{"^web"}},
		{"= ANY with no surrounding spaces keeps a boundary",
			`SELECT id FROM t WHERE id=ANY($1)`, []any{ids},
			`SELECT id FROM t WHERE id IN ($1, $2, $3)`, []any{"a", "b", "c"}},
		{"REGEXP with no surrounding spaces keeps a boundary",
			`SELECT id FROM t WHERE name~$1`, []any{"^x"},
			`SELECT id FROM t WHERE name REGEXP $1`, []any{"^x"}},
		{"quoted text is never rewritten",
			`SELECT 'a::text FOR UPDATE = ANY($1) ~ now()', "x::int" FROM t WHERE c = $1`, []any{"v"},
			`SELECT 'a::text FOR UPDATE = ANY($1) ~ now()', "x::int" FROM t WHERE c = $1`, []any{"v"}},
		{"an escaped quote inside a string",
			`SELECT 'it''s ::text' WHERE a = $1::text`, []any{"v"},
			`SELECT 'it''s ::text' WHERE a = CAST($1 AS TEXT)`, []any{"v"}},
		{"comments are never rewritten",
			"SELECT 1 -- id::text = ANY($9)\nWHERE a = $1 /* FOR UPDATE */", []any{"v"},
			"SELECT 1 -- id::text = ANY($9)\nWHERE a = $1 /* FOR UPDATE */", []any{"v"}},
		{"advisory lock statement runs as written",
			`SELECT pg_advisory_xact_lock(hashtext($1))`, []any{"k"},
			`SELECT pg_advisory_xact_lock(hashtext($1))`, []any{"k"}},
		{"JSON operators stay",
			`SELECT external_gateway_info->>'network_id', external_gateway_info->'enable_snat' FROM routers`, nil,
			`SELECT external_gateway_info->>'network_id', external_gateway_info->'enable_snat' FROM routers`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSQL, gotArgs, err := rewrite(tc.sql, tc.args)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if gotSQL != tc.wantSQL {
				t.Errorf("SQL:\n got  %s\n want %s", gotSQL, tc.wantSQL)
			}
			if len(gotArgs) == 0 && len(tc.wantArgs) == 0 {
				return
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Errorf("args = %#v; want %#v", gotArgs, tc.wantArgs)
			}
		})
	}
}

// TestTranslateRefuses pins the constructs the translation refuses rather than passing
// through: SQLite would either reject them or, worse, run them with another meaning.
func TestTranslateRefuses(t *testing.T) {
	cases := []struct {
		name, sql string
		args      []any
		mention   string
	}{
		{"dollar quoting", `DO $$ BEGIN END $$`, nil, "dollar"},
		{"case-insensitive regex", `SELECT 1 FROM t WHERE a ~* $1`, []any{"x"}, "~*"},
		{"negated regex", `SELECT 1 FROM t WHERE a !~ $1`, []any{"x"}, "!~"},
		{"a list argument that is not a slice", `SELECT 1 FROM t WHERE a = ANY($1)`, []any{"x"}, "ANY"},
		{"an unknown cast target", `SELECT $1::inet`, []any{"x"}, "inet"},
		{"an argument the statement never uses", `SELECT $1`, []any{"a", "b"}, "argument"},
		{"unterminated quoted text", `SELECT 'oops`, nil, "unterminated quoted text"},
		{"unterminated block comment", `SELECT 1 /* oops`, nil, "unterminated comment"},
		{"unbalanced parentheses before a cast", `SELECT )::text`, nil, "unbalanced parentheses"},
		{"ANY without a parenthesised argument", `SELECT 1 FROM t WHERE a = ANY $1`, []any{[]string{"x"}}, "parenthesised"},
		{"ANY of a literal, not a placeholder", `SELECT 1 FROM t WHERE a = ANY (1)`, nil, "list argument"},
		{"FOR SHARE", `SELECT 1 FROM t FOR SHARE`, nil, "FOR SHARE"},
		{"FOR UPDATE SKIP LOCKED", `SELECT 1 FROM t FOR UPDATE SKIP LOCKED`, nil, "SKIP"},
		{"a cast without a type", `SELECT a::`, nil, "cast without a type"},
		{"ANY not after =", `SELECT 1 FROM t WHERE ANY($1)`, []any{[]string{"x"}}, "= ANY($n)"},
		{"a placeholder beyond the arguments", `SELECT $2`, []any{"only one"}, "argument"},
		{"make_interval in another form", `SELECT make_interval(days => $1)`, []any{1}, "make_interval"},
		{"a date cast would truncate, not tag", `SELECT created_at::date FROM t`, nil, "date"},
		{"a multi-word type cast", `SELECT $1::character varying`, []any{"x"}, "character varying"},
		{"a time-zone type cast", `SELECT $1::timestamp with time zone`, []any{"x"}, "timestamp with"},
		{"the $0 placeholder", `SELECT $0`, nil, "$0"},
		{"a cast straight after END", `SELECT CASE WHEN a THEN 1 ELSE 2 END::text FROM t`, nil, "END"},
		{"a LIMIT placeholder in arithmetic", `SELECT id FROM t LIMIT $1 + 1`, []any{nil}, "LIMIT"},
		{"an OFFSET placeholder in arithmetic", `SELECT id FROM t LIMIT 5 OFFSET $1 - 1`, []any{nil}, "OFFSET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := rewrite(tc.sql, tc.args)
			if err == nil {
				t.Fatalf("rewrite(%q) succeeded; want an error", tc.sql)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Fatalf("error %q does not name %q", err, tc.mention)
			}
		})
	}
}

// TestTranslateCachesTemplates: the parse runs once per statement text; only the list
// expansion runs per call, so the same text with lists of different lengths reuses the
// cached template and still expands correctly.
func TestTranslateCachesTemplates(t *testing.T) {
	resetTranslateCache()
	sql := `SELECT id FROM t WHERE id = ANY($1) AND x = $2`
	for _, n := range []int{1, 3, 2} {
		list := make([]string, n)
		for i := range list {
			list[i] = string(rune('a' + i))
		}
		got, args, err := rewrite(sql, []any{list, "x"})
		if err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		if strings.Count(got, "$") != n+1 || len(args) != n+1 {
			t.Fatalf("n=%d: got %q with %d args", n, got, len(args))
		}
	}
	if translateCacheLen() != 1 {
		t.Fatalf("cache holds %d templates; want 1", translateCacheLen())
	}
}
