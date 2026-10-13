// Package database is Lightstack's access to its database: PostgreSQL, or one embedded
// SQLite file for a single node (SPEC-028, ADR-003).
//
// # Choosing the backend
//
// The scheme of database.url chooses it once, at startup (ParseURL): postgres:// or
// postgresql:// for PostgreSQL, sqlite:///absolute/path/lightstack.db for SQLite. Open
// returns a DB; services receive it through their constructors and never ask which backend
// they run on. A SQLite file is opened with write-ahead logging, synchronous=FULL, a busy
// wait of 5 s, foreign keys on and immediate transactions, and the settings are read back.
//
// # The SQL callers write
//
// Callers write PostgreSQL syntax once. PostgreSQL receives it unchanged; SQLite
// translates it on the way in. A statement may use:
//
//   - SELECT, INSERT, UPDATE and DELETE; joins and subqueries; EXISTS; IN with a literal
//     list; CASE; GROUP BY, HAVING, ORDER BY, LIMIT and OFFSET; RETURNING; UPDATE … FROM;
//     WITH and WITH RECURSIVE; ON CONFLICT (cols) DO NOTHING or DO UPDATE SET … =
//     excluded.…; ROW_NUMBER() OVER (PARTITION BY … ORDER BY …); IS [NOT] DISTINCT FROM;
//     TRUE and FALSE;
//   - the functions count, sum, min, max, avg (with DISTINCT and FILTER (WHERE …)),
//     coalesce, nullif, lower, upper, length, abs, round, replace, now, gen_random_uuid,
//     and pg_advisory_xact_lock(hashtext($1)) — a no-op on SQLite, whose immediate
//     transactions serialise writers;
//   - || on text, LIKE for prefix filters (SQLite's LIKE ignores ASCII case), ~ for a
//     regular-expression match (each backend's own dialect), and ->> and -> on JSON;
//   - $1 … $N placeholders.
//
// Translated for SQLite, and written the same for both:
//
//   - casts: ::text, ::varchar and ::char become CAST(… AS TEXT); ::int, ::integer,
//     ::bigint and ::smallint become CAST(… AS INTEGER); ::uuid, ::jsonb, ::json,
//     ::timestamp, ::timestamptz, ::boolean and array casts are dropped, since ids, JSON,
//     lists and times are text on SQLite;
//   - col = ANY($n), with or without an array cast, where $n binds a slice: one
//     placeholder per element;
//   - LIMIT $n and OFFSET $n bound to NULL mean no limit and no offset, as on PostgreSQL;
//   - FOR UPDATE is removed;
//   - now() + make_interval(secs => $n), the console tokens' expiry.
//
// Not supported, on either backend: ~* and !~, ILIKE, SIMILAR TO, the jsonb operators (@>,
// <@, ?|, ?&, #>, #>>, and || on jsonb), jsonb_* and json_build_* functions, ARRAY_AGG,
// STRING_AGG, ARRAY[…], DISTINCT ON, generate_series, EXTRACT, date_trunc, to_char,
// INTERVAL '…', CURRENT_TIMESTAMP (use now()), FOR SHARE, SKIP LOCKED, NOWAIT, LATERAL,
// ON CONFLICT ON CONSTRAINT, DELETE … USING, a table alias without AS in UPDATE or DELETE,
// dollar quoting, TRUNCATE, LISTEN and NOTIFY, a cast to any other type, and any other
// form of ANY. TestSQLConstructs (sqlcheck_test.go) finds them in the code, naming file,
// line and construct; a PostgreSQL-only test literal carries //sqlcheck:postgres-only and
// runs behind dbtest.RequirePostgres.
//
// # Values
//
// Bind times as time.Time: SQLite stores them as UTC text with six fractional digits,
// which sorts in time order. Bind JSON as a map, struct or json.RawMessage and lists as
// []string; scan them into the same types. A nil slice binds NULL on both backends. Ids
// are text in canonical form and compared as text: the layer never canonicalises a value,
// so a caller that accepts another spelling parses it first.
//
// # Errors
//
// ErrNoRows replaces each driver's own. IsUniqueViolation, IsForeignKeyViolation,
// IsNotNullViolation, IsCheckViolation and IsBusy classify a failure on either backend.
//
// # Migrations
//
// One migrations folder serves both backends, each through its own view; see
// migrations/README.md. MigrateUp and the other Migrate functions pick the backend from the
// URL, as Open does.
package database
