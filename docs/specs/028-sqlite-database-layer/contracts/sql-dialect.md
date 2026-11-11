# Contract: the SQL both backends run

Callers write PostgreSQL syntax once. PostgreSQL receives it unchanged; the SQLite implementation translates it on the way in (FR-014 – FR-018; research R5 – R9). This file is the supported construct set that `internal/database` documents and the static check enforces.

## 1. Shared as written

Plain `SELECT`/`INSERT`/`UPDATE`/`DELETE`, joins, subqueries, `EXISTS`, `IN (…)` with literal lists, `CASE`, `COALESCE`, `NULLIF`, `LOWER`/`UPPER`/`LENGTH`/`ABS`/`ROUND`, `COUNT`/`SUM`/`MIN`/`MAX`/`AVG` incl. `COUNT(DISTINCT …)` and `FILTER (WHERE …)`, `GROUP BY`/`HAVING`/`ORDER BY`/`LIMIT`/`OFFSET`, `ON CONFLICT (cols) DO NOTHING | DO UPDATE SET … = excluded.…`, `RETURNING`, `UPDATE … FROM`, `WITH` and `WITH RECURSIVE`, `ROW_NUMBER() OVER (PARTITION BY … ORDER BY …)`, `IS [NOT] DISTINCT FROM`, `TRUE`/`FALSE`, `||` on text, `LIKE` (prefix filters only — SQLite's `LIKE` ignores ASCII case), JSON field access `->>` and `->`, `$1 … $N` placeholders (bound by number on both backends).

## 2. Translated for SQLite

| PostgreSQL | SQLite | Notes |
|---|---|---|
| `x::text`, `::varchar`, `::char` | `CAST(x AS TEXT)` | operand: placeholder, literal, identifier chain or parenthesised/function group |
| `x::int`, `::integer`, `::bigint`, `::smallint` | `CAST(x AS INTEGER)` | |
| `x::jsonb`, `::json`, `::uuid`, `::uuid[]`, `::text[]`, `::timestamp`, `::timestamptz`, `::boolean` | `x` | ids, JSON and times are text on SQLite |
| `col = ANY($n)`, `col = ANY($n::uuid[])` | `col IN ($k, $k+1, …)` | `$n` must bind a slice; one placeholder per element; later placeholders renumbered; empty slice → matches nothing |
| `LIMIT $n`, `OFFSET $n` | `LIMIT COALESCE($n, -1)`, `OFFSET COALESCE($n, 0)` | PostgreSQL reads a NULL there as no limit / no offset, SQLite refuses it; the list handlers bind a nil limit when the client sends none |
| `… FOR UPDATE` | `…` | serialisation comes from immediate transactions (research R4) |
| `now() + make_interval(secs => $n)` | `ls_add_seconds(now(), $n)` | console-token expiry keeps the database clock |
| `x ~ y` | `x REGEXP y` | the registered `regexp` uses the application's regular expressions — as upstream's SQLite backend uses Python's |
| `now()`, `NOW()`, `gen_random_uuid()`, `regexp()` | registered functions | `now()` returns `2006-01-02T15:04:05.000000Z` |
| `SELECT pg_advisory_xact_lock(hashtext($1))` | same text; both functions registered as no-ops | keeps the placeholder `database/sql` counts |

Quoted strings, quoted identifiers and comments are never rewritten.

## 3. Not supported — rewritten once for both backends, and refused by the static check

`~*`, `!~`, `ILIKE`, `SIMILAR TO`, `@>`, `<@`, `?|`, `?&`, `#>`, `#>>`, `||` on `jsonb` (it concatenates text on SQLite), `jsonb_*`/`json_build_*` functions, `jsonb_array_elements`, `ARRAY_AGG`, `STRING_AGG`, `ARRAY[…]`, `DISTINCT ON`, `generate_series`, `EXTRACT`, `date_trunc`, `to_char`, `INTERVAL '…'`, `CURRENT_TIMESTAMP`, `FOR SHARE`, `SKIP LOCKED`, `NOWAIT`, `LATERAL`, `ON CONFLICT ON CONSTRAINT`, `DELETE … USING`, a table alias without `AS` in `UPDATE`/`DELETE` (`DELETE FROM t a` → `DELETE FROM t AS a`), dollar-quoted strings, `TRUNCATE`, `LISTEN`/`NOTIFY`, any function outside the allow-list, any cast to a type outside §2, any `ANY(` not of the form `= ANY($n)`. Research R8 lists the ten rewrite sites of this feature.

## 4. Values crossing the boundary on SQLite

| Go type | Bound as | Scanned from |
|---|---|---|
| `time.Time`, `*time.Time` | text `2006-01-02T15:04:05.000000Z` (UTC) | that text only — anything else is an error |
| `[]byte`, `json.RawMessage` | text (a STRICT `TEXT` column refuses BLOBs) | text |
| map, struct | JSON text | JSON text → `json.Unmarshal` |
| slice (not `[]byte`) outside `= ANY` | JSON array text | JSON array text → `json.Unmarshal` |
| `bool` | 0/1 | 0/1 → `bool` |
| `driver.Valuer` (e.g. `uuid.UUID`) | `Value()`, then the rules above | — |

## 5. Static check

`go test ./internal/database -run TestSQLConstructs` scans the SQL string literals (and constant concatenations, and fragments passed to `internal/common/sqlbuilder.go`) of every `.go` file under `internal`, `test`, `cmd` and `pkg` and fails naming file, line and construct for anything in §3. A test literal that must stay PostgreSQL-only carries `//sqlcheck:postgres-only` and runs behind `dbtest.RequirePostgres`. A seeded case per construct class keeps the check honest (SC-009).
