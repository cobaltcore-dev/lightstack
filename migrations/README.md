# Migrations

One folder serves both database backends, PostgreSQL and SQLite. Each backend reads it through its
own **view** (`internal/database/migrate_view.go`); `lightstack` applies the view at startup and
`lightstack-migrate` (or `make migrate`) applies it by hand. golang-migrate's `migrate` CLI cannot read
the folder: reading it through iofs, it rejects the two files that share a version
(`duplicate migration file: 073_…`), because `073_baseline.sqlite` and PostgreSQL's `073` collide.

## File names

```text
NNN_<name>.up.sql / NNN_<name>.down.sql                 plain: both backends
NNN_<name>.postgres.up.sql / .postgres.down.sql         PostgreSQL only
NNN_<name>.sqlite.up.sql / .sqlite.down.sql             SQLite only
073_baseline.sqlite.up.sql / .down.sql                  where SQLite starts (generated, see below)
```

Versions are three digits and only ever grow. golang-migrate records only the version a database
has reached, not what the file said, so an edited file never reaches a database that already ran it:
a deployed schema changes through a new version.

## The three rules of a view

1. A `.postgres` or `.sqlite` file is used only by that backend.
2. When a backend has its own file for a version, it replaces the plain file with that number —
   separately for `up` and `down`.
3. SQLite skips every version below its baseline, `NNN_baseline.sqlite.up.sql`.

`ValidateMigrations` (run by `lightstack-migrate` before every command) checks both views: every up
file has a down file and the reverse, a backend's own up file has a down file of its own, there is
exactly one SQLite baseline, and no version appears twice in a view.

A backend's own **down** file beside a plain up file is allowed: it corrects how that backend rolls
the plain file back. Files below the SQLite baseline are PostgreSQL's alone, so a wrong down file
there is simply corrected in place — as the down files of `010`, `019`, `028` and `037` were: they
dropped tables and columns an earlier migration owned, so PostgreSQL could not migrate down past
them.

## The SQLite baseline

SQLite starts at `073_baseline.sqlite.up.sql`: the schema and seed data of PostgreSQL's migrations
001–073, rendered as SQLite DDL (`STRICT` tables, text ids and timestamps, `VARCHAR(n)` as `TEXT` with
a length `CHECK`, defaults through the registered `now()` and `gen_random_uuid()`). It is generated,
never edited by hand:

```bash
LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable' \
  go test -count=1 ./internal/database -run TestGenerateSQLiteBaseline -update
```

The generator migrates a scratch PostgreSQL database to 073, reads its catalog and writes both files.
A column type, default, `CHECK` or index it has no mapping for stops it, so a new construct is mapped
on purpose (`internal/database/baseline_gen_test.go`). Ids PostgreSQL's seed generates at install time
are generated at install time on SQLite too.

Regenerate `073` only before the **first** SQLite release. Once a deployed SQLite database exists the
baseline is frozen: moving it to a newer migration would strand every database below the new number,
because rule 3 hides versions below the baseline — a database at 074 under a baseline moved to 075
can no longer be migrated (`no migration found for version 74`). From then on, schema changes are new
migrations above the baseline, never a regenerated one.

Below the baseline SQLite has no versions: `goto` and `force` to one are refused, and `down` past the
baseline runs its down file and leaves an empty database.

## Writing a migration

Write one **plain** file pair only for SQL both backends spell and treat alike — `TEXT` strings and
`INTEGER`/`BIGINT` numbers, which PostgreSQL and SQLite store and compare the same way:

- `ALTER TABLE … ADD COLUMN` — **without** `IF NOT EXISTS`, which SQLite rejects — of a nullable
  `TEXT` or `INTEGER`/`BIGINT` column, or one with a constant default;
- `ALTER TABLE … DROP COLUMN` of a column no index, key or `CHECK` uses;
- `CREATE INDEX` / `DROP INDEX`, partial indexes included.

`074_reservation_node` (a nullable `TEXT` column) is an example.

Write a `.postgres` / `.sqlite` **pair** under one number whenever the backends differ — which is
every `CREATE TABLE`, and any column that is a boolean, timestamp, id/uuid, JSON or list:

- **`CREATE TABLE`**: PostgreSQL ids are `UUID` and timestamps `TIMESTAMPTZ`, but on SQLite both are
  `TEXT` (a `TEXT` id cannot reference PostgreSQL's `UUID` keys, and pgx will not scan a `TEXT`
  column into `time.Time`); a `STRICT` table also admits only `INT`/`INTEGER`/`REAL`/`TEXT`/`BLOB`/
  `ANY`, so `BOOLEAN`, `UUID`, `JSONB` and `TIMESTAMPTZ` have to be rewritten. The `.sqlite` file
  renders the table as the baseline generator does (`STRICT`, `TEXT` ids and timestamps,
  `VARCHAR(n)` as `TEXT` with a length `CHECK`);
- a **boolean, timestamp, id/uuid, JSON or list** column added later: the two backends spell the
  type differently, so each needs its own `ADD COLUMN`;
- a column added with a non-constant default (SQLite refuses `ADD COLUMN … DEFAULT (now())`) or
  `NOT NULL` without a default — or leave it nullable and have the application write it;
- a type change, or adding or changing a `CHECK`, key or foreign key: SQLite's `ALTER TABLE` cannot
  express these, and the `.sqlite` file rebuilds the table — create the new table, copy the rows,
  drop the old one, rename the new one.

**Never rebuild a table other tables reference in a `.sqlite` file.** Each file runs in a
transaction with foreign keys on, and `DROP TABLE` then deletes the rows that reference the dropped
one through every `ON DELETE CASCADE` — the migration succeeds and the data is gone. SQLite's safe
procedure turns foreign keys off, which works only outside a transaction, and the migrator gives
every file one. The first migration that needs such a rebuild comes with a change to the migrator
that lets one file run outside a transaction.

## Checking

- `TestSchemaParity` (`internal/database/parity_test.go`, needs PostgreSQL) migrates an empty
  PostgreSQL database and an empty SQLite file from this folder and compares tables, columns,
  defaults, keys, `CHECK`s, indexes and seed rows. The GIN index over image properties is the one
  documented difference. CI runs it.
- `TestMigrationsRoundTrip` migrates an empty database up, all the way down and up again, then the
  newest migration down and up, and requires the schema of the first up each time — on the backend
  `LIGHTSTACK_TEST_DB_URL` selects. CI runs it on both.
