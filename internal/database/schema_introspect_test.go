package database

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Introspection of a PostgreSQL schema, shared by the SQLite baseline generator and the
// schema parity test (research R11).

type pgColumn struct {
	name     string
	udt      string // uuid, varchar, text, timestamp, timestamptz, bool, int4, int8, jsonb, _text, …
	maxLen   int    // varchar(n); 0 otherwise
	nullable bool
	def      string // column_default as PostgreSQL prints it; "" for none
}

type pgTable struct {
	name    string
	columns []pgColumn
	pk      []string
	uniques []string // pg_get_constraintdef of UNIQUE constraints
	fks     []string // pg_get_constraintdef of FOREIGN KEY constraints
	parents []string // tables the foreign keys reference
	checks  []string // pg_get_constraintdef of CHECK constraints
	indexes []string // pg_get_indexdef of indexes that back no constraint
	rows    [][]any  // seed rows, ordered by primary key
}

// adminPostgresURL returns the PostgreSQL test database URL, or skips.
func adminPostgresURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("LIGHTSTACK_TEST_DB_URL")
	if !strings.HasPrefix(u, "postgres://") && !strings.HasPrefix(u, "postgresql://") {
		t.Skip("needs LIGHTSTACK_TEST_DB_URL set to a postgres:// URL")
	}
	return u
}

// scratchPostgres creates an empty database next to the test database, returns its URL
// and drops it when the test ends.
func scratchPostgres(t *testing.T, adminURL, name string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		t.Fatalf("drop scratch database: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func introspectPostgres(ctx context.Context, conn *pgx.Conn) ([]pgTable, error) {
	var tables []pgTable
	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name <> 'schema_migrations'
		ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		t := pgTable{name: name}
		cols, err := conn.Query(ctx, `SELECT column_name, udt_name, COALESCE(character_maximum_length, 0),
				is_nullable = 'YES', COALESCE(column_default, '')
			FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1
			ORDER BY ordinal_position`, name)
		if err != nil {
			return nil, err
		}
		for cols.Next() {
			var c pgColumn
			if err := cols.Scan(&c.name, &c.udt, &c.maxLen, &c.nullable, &c.def); err != nil {
				cols.Close()
				return nil, err
			}
			t.columns = append(t.columns, c)
		}
		cols.Close()

		cons, err := conn.Query(ctx, `SELECT contype::text, pg_get_constraintdef(oid),
				CASE WHEN contype = 'f' THEN confrelid::regclass::text ELSE '' END FROM pg_constraint
			WHERE conrelid = ('public.' || quote_ident($1))::regclass ORDER BY contype, conname`, name)
		if err != nil {
			return nil, err
		}
		for cons.Next() {
			var kind, def, parent string
			if err := cons.Scan(&kind, &def, &parent); err != nil {
				cons.Close()
				return nil, err
			}
			switch kind {
			case "p":
				inner := strings.TrimSuffix(strings.TrimPrefix(def, "PRIMARY KEY ("), ")")
				for _, c := range strings.Split(inner, ",") {
					t.pk = append(t.pk, strings.Trim(strings.TrimSpace(c), `"`))
				}
			case "u":
				t.uniques = append(t.uniques, def)
			case "f":
				t.fks = append(t.fks, def)
				if parent != name {
					t.parents = append(t.parents, parent)
				}
			case "c":
				t.checks = append(t.checks, def)
			}
		}
		cons.Close()

		idx, err := conn.Query(ctx, `SELECT indexdef FROM pg_indexes i WHERE schemaname = 'public' AND tablename = $1
			AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conname = i.indexname) ORDER BY indexname`, name)
		if err != nil {
			return nil, err
		}
		if t.indexes, err = pgx.CollectRows(idx, pgx.RowTo[string]); err != nil {
			return nil, err
		}

		order := "1"
		if len(t.pk) > 0 {
			quoted := make([]string, len(t.pk))
			for i, c := range t.pk {
				quoted[i] = pgx.Identifier{c}.Sanitize()
			}
			order = strings.Join(quoted, ", ")
		}
		data, err := conn.Query(ctx, fmt.Sprintf(`SELECT * FROM %s ORDER BY %s`, pgx.Identifier{name}.Sanitize(), order))
		if err != nil {
			return nil, err
		}
		for data.Next() {
			vals, err := data.Values()
			if err != nil {
				data.Close()
				return nil, err
			}
			t.rows = append(t.rows, vals)
		}
		data.Close()
		tables = append(tables, t)
	}
	return dependencyOrder(tables)
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
