# Migrations

One folder serves both database backends, PostgreSQL and SQLite. It holds a generated **baseline**,
one **release migration** per release, and between releases the **fragments** of the next one, in
`next/`. Each backend reads the folder through its own **view** (`internal/database/migrate_view.go`):
`lightstack` applies it at startup, and `lightstack-migrate` (or `make migrate`) by hand.
golang-migrate's own `migrate` CLI cannot read the folder: the per-backend files share version
numbers (`duplicate migration file: 001_…`), and the open release migration exists only in the view.

## The folder

```text
001_baseline.postgres.up.sql / .down.sql        the baseline of each backend (generated, frozen)
001_baseline.sqlite.up.sql   / .down.sql
NNN_v<semver>.up.sql / .down.sql                a release migration: plain, both backends
NNN_v<semver>.postgres.up.sql / .down.sql       … or one pair per backend
NNN_v<semver>.sqlite.up.sql   / .down.sql
next/<UTC YYYYMMDDhhmmss>-<ticket>-<purpose>[.postgres|.sqlite].(up|down).sql
                                                a fragment of the next release
```

Release migrations are numbered consecutively from `002`, one per release (`002_v0.8.0`,
`003_v0.9.0`, …); a release without schema changes has none. `<semver>` may carry a pre-release
(`v0.8.0-rc.1`). Nothing else may be numbered: `ValidateMigrations`, which every `lightstack-migrate`
command and the unit tests run, refuses a hand-numbered file, a gap, a version naming two releases
and a release on two versions.

Every numbered file has one shape: a header line, `-- checksum: <SHA-256 of the body>`, the body,
and the statement that records the migration in `applied_migrations`. The record commits in the
migration's own transaction, so no database ever holds a migration without its record.

## The views

1. A `.postgres` or `.sqlite` file is used only by that backend.
2. When a backend has its own file for a version, it replaces the plain file with that number,
   separately for `up` and `down`.

Every up file needs a down file, a backend's own up file a down file of its own. A backend's own
**down** file beside a plain up file is allowed: it corrects how that backend rolls the plain file
back.

The view also serves the **open release migration**: the fragments in `next/` assembled into
`NNN_open.up.sql` and `NNN_open.down.sql`, one version above the newest numbered file
(`migrate_fragments.go`). Every reader of the folder sees it.

## The baseline

`001` is the schema and seed data of the 83 migrations that existed before the squash
(`internal/database/testdata/pre-squash/`, kept as the generators' input), rendered once per backend.
It is generated, never edited by hand:

```bash
LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable' go test -count=1 ./internal/database -run 'TestGenerate(Postgres|SQLite)Baseline' -update
```

Both generators migrate a scratch PostgreSQL database with the pre-squash history and read its
catalog. The SQLite one renders `STRICT` tables with text ids and timestamps, `VARCHAR(n)` as `TEXT`
with a length `CHECK`, and defaults through the registered `now()` and `gen_random_uuid()`; a type,
default, `CHECK` or index it has no mapping for stops it. Ids and timestamps the history generates at
install time are generated at install time from the baseline too. `TestBaselineEquivalencePostgres`
and `TestBaselineEquivalenceSQLite` prove each baseline builds what the history builds.

The baseline is frozen once it is on `main`, like every numbered file.

## Writing a schema change: a fragment

A change between releases is a fragment pair in `next/`, named by its creation time, its ticket and
its purpose instead of a number:

```bash
make migration-fragment TICKET=596 NAME=add-volume-note
```

creates `next/<UTC now>-596-add-volume-note.up.sql` and `.down.sql`, both empty. Changes made in
parallel never compete for a number and merge in either order.

Fragments apply in the byte order of their names: creation time, then ticket, then purpose. A
fragment that depends on another must sort after it. When a branch is rebased onto fragments created
later than its own and depends on them, rename its fragments with a fresh time. CI replays the
fragments one by one in that order and names the first that fails (`TestFragmentsApplyInOrder`).

All fragments of a release form **one** migration, so they run in one transaction per backend.
Nothing that cannot run in a transaction (`CREATE INDEX CONCURRENTLY`) fits: the first change that
needs it brings a change to the migrator with it, and a version of its own.

Write one **plain** fragment pair only for SQL both backends spell and treat alike: `TEXT` strings
and `INTEGER`/`BIGINT` numbers, which PostgreSQL and SQLite store and compare the same way:

- `ALTER TABLE … ADD COLUMN` of a nullable `TEXT` or `INTEGER`/`BIGINT` column, or one with a
  constant default, **without** `IF NOT EXISTS`, which SQLite rejects;
- `ALTER TABLE … DROP COLUMN` of a column no index, key or `CHECK` uses;
- `CREATE INDEX` / `DROP INDEX`, partial indexes included.

Rename the pair to a `.postgres` / `.sqlite` **pair of pairs**, under the same name, whenever the
backends differ. That is every `CREATE TABLE`, and any column that is a boolean, timestamp, id/uuid,
JSON or list:

- **`CREATE TABLE`**: PostgreSQL ids are `UUID` and timestamps `TIMESTAMPTZ`, but on SQLite both are
  `TEXT`. A `TEXT` id cannot reference PostgreSQL's `UUID` keys, and pgx will not scan a `TEXT`
  column into `time.Time`. A `STRICT` table also admits only `INT`/`INTEGER`/`REAL`/`TEXT`/`BLOB`/
  `ANY`, so `BOOLEAN`, `UUID`, `JSONB` and `TIMESTAMPTZ` have to be rewritten. The `.sqlite`
  fragment renders the table as the baseline generator does: `STRICT`, `TEXT` ids and timestamps,
  `VARCHAR(n)` as `TEXT` with a length `CHECK`.
- **A boolean, timestamp, id/uuid, JSON or list column** added later: the two backends spell the
  type differently, so each needs its own `ADD COLUMN`.
- **A column with a non-constant default**: SQLite refuses `ADD COLUMN … DEFAULT (now())`. The same
  goes for `NOT NULL` without a default. Alternatively, leave the column nullable and have the
  application write it.
- **A type change, or adding or changing a `CHECK`, key or foreign key**: SQLite's `ALTER TABLE`
  cannot express these, so the `.sqlite` fragment rebuilds the table. It creates the new table,
  copies the rows, drops the old one and renames the new one.

**Never rebuild a table other tables reference in a `.sqlite` fragment.** The migration runs in a
transaction with foreign keys on, and `DROP TABLE` then deletes the rows that reference the dropped
one through every `ON DELETE CASCADE`. The migration succeeds and the data is gone. SQLite's safe
procedure turns foreign keys off, which works only outside a transaction.

One name is plain or backend-specific, never both, and every variant needs both its up and its
down part.

## The open release migration

A build between releases applies the fragments as the open release migration, logs `applied the
open release migration NNN (unreleased)`, and `lightstack-migrate status` shows `Migration: open
release migration NNN (unreleased)`. Adding, changing or removing a fragment changes it, so a
database that applied an earlier state of it is refused as stale: `applied an earlier state of the
open release migration NNN`. Recreate such a database. For the shared development database that is
`make db-down && make db-up`. Only development and test databases run unreleased builds.

## Freezing a release

When a release is cut, on `main`:

```bash
make migrations-freeze RELEASE=v0.8.0
```

`lightstack-migrate freeze`, which needs no configuration and no database, writes `NNN_v0.8.0.up.sql`
and `.down.sql`, or a pair per backend when any fragment is backend-specific, and removes the
fragments. Commit the result, then tag. The frozen files carry the open migration's body and
checksum under a release header, so a database that applied the open state carries on after the
freeze. A release without fragments adds no migration.

## Immutability and the checks

A numbered file never changes once it is on `main`. golang-migrate records only the version a
database reached, not what the file said, and `applied_migrations` records its checksum: a database
that applied a different state of its version is refused as stale.

- **Numbered migrations are immutable** (`ci.yml`, `scripts/check-migrations-immutable.sh`): a pull
  request or push that changes or removes the baseline or a release migration fails. A rename counts
  as a removal.
- **The folder grammar** (`ValidateMigrations`, `TestMigrationsFolder`): a hand-numbered file fails.
- **Release fragments** (`security.yml`): a `v*` tag that still carries fragments in `next/` fails.
  Freeze on `main`, then delete and re-cut the tag.

`TestSchemaParity` migrates an empty PostgreSQL database and an empty SQLite file from this folder and
compares tables, columns, defaults, keys, `CHECK`s, indexes and seed rows. The GIN index over image
properties is the one documented difference, and the per-backend checksums of the baseline are
masked. `TestMigrationsRoundTrip` migrates an empty database up, all the way down and up again, then
the newest migration down and up, on the backend `LIGHTSTACK_TEST_DB_URL` selects. CI runs both.

## Databases from before the squash

A database migrated before the squash, which `applied_migrations` is missing from, is refused with
the reason and the way out: recreate it. See `docs/UPGRADING.md`, "The migration squash".
