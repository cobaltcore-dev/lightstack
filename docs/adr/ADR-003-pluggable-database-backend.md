# ADR-003: Pluggable Database Backend

**Status**: Accepted
**Date**: 2026-10-12
**Deciders**: Lightstack Core Team
**Spec**: [SPEC-028](../specs/028-sqlite-database-layer/spec.md)
**Amends**: [ADR-001](ADR-001-database-dependency-injection.md), which rejected
interface-based injection as premature, for no immediate benefit — a second backend is that benefit.

## Context

Every Lightstack deployment needs a PostgreSQL server, including the single-node ones: a lab, an edge
site, a development machine. For those, a separate database service is the largest moving part of
the installation. SQLite, embedded in the binary, removes it.

Two backends need one call-site syntax. ADR-001 injected the concrete `*pgxpool.Pool` and rejected an
interface as premature, with no immediate benefit. SQLite is that benefit.

## Decision

**One contract, `database.DB`, with a PostgreSQL and a SQLite implementation, chosen by the URL.**

- **Contract**: `internal/database` defines `DB`, `Tx`, `Rows`, `Row` and `Result` with the method set
  the code already calls on pgx — `Query`, `QueryRow`, `Exec`, `Begin`, `Scan`, `RowsAffected` — plus
  `ErrNoRows` and backend-neutral error classes (`IsUniqueViolation`, `IsBusy`, …). Services receive a
  `database.DB` through their constructors, as ADR-001 established; nothing else about injection
  changes.
- **Selection**: the scheme of `database.url` — `postgres://`/`postgresql://` or
  `sqlite:///absolute/path`. Chosen once, at startup. `LIGHTSTACK_DB_BACKEND` may declare the
  expected backend, and a mismatch refuses to start.
- **PostgreSQL**: pgx and pgxpool, unchanged — same pool settings, same types, same advisory locks.
- **SQLite**: `modernc.org/sqlite`, pure Go, through `database/sql`. No cgo, so the binary stays one
  static file. Write-ahead logging, `synchronous=FULL`, a 5 s busy wait, foreign keys on, and
  transactions that begin `IMMEDIATE` — decided by a concurrency matrix, in which deferred
  transactions failed the 5 of the 13 that read before they write; the other 8 start with a
  write. The file is `0600`, its directory `0750`. SQLite serves a single node: with VXLAN
  enabled the server refuses to start.
- **Dialect**: callers write PostgreSQL syntax once. The SQLite implementation translates the few
  constructs SQLite lacks on the way in — casts, `= ANY($n)` over a slice, `LIMIT`/`OFFSET` bound to
  NULL, `FOR UPDATE`, the one interval form, `~` — and registers `now()`, `gen_random_uuid()` and a
  `regexp()`. Constructs neither translation nor registration covers (jsonb operators, `ARRAY_AGG`,
  `DISTINCT ON`, …) were rewritten once — in portable SQL, or in Go where SQL has no portable form —
  and a static check (`TestSQLConstructs`) fails the build on new ones. The supported set is the
  package documentation of `internal/database`.
- **Migrations**: one folder. Each backend reads it through a view: backend-suffixed files are used by
  that backend only and replace the plain file of their number; SQLite starts at a baseline
  generated from PostgreSQL's schema at 073. A parity test compares both backends' schemas after
  migrating from the folder. A file lock serialises SQLite migration runs across processes.
- **No transaction around external work**: SQLite has one writer for the whole file, so a
  transaction held across a storage call would stop every project's writes. Volume and snapshot
  creates reserve their row, do the backing work outside any transaction, then finalise; a startup
  sweep resolves the reservations a stopped node left behind.

## Alternatives Considered

### `pgx/v5/stdlib` and `database/sql` for both backends
One standard interface, no contract of our own.

**Rejected because**: it changes PostgreSQL's pool and its type mapping for every query — UUIDs,
arrays and JSON scan differently through `database/sql` — so the backend that runs production would
change to admit the one that does not.

### An ORM or a query builder
Write queries once in a builder that renders each dialect.

**Rejected because**: 736 SQL statements would be rewritten, and the builder becomes the abstraction
CLAUDE.md's Article VIII warns against. Translating the handful of constructs that differ is smaller.

### SQLite through cgo (`mattn/go-sqlite3`)
The most used driver.

**Rejected because**: cgo breaks the static single binary and cross-compilation (constitution Article VI).

### Two migration folders
One per backend.

**Rejected because**: every schema change would be written twice with nothing to show the two agree.
One folder with per-backend files only where the dialects differ, and a parity test, keeps them
equal.

### A hand-written SQLite schema
**Rejected because**: 72 tables, 150 indexes and their seed data drift when transcribed. The baseline
is generated, and the generator fails on a construct it has no mapping for.

## Consequences

### Positive
- A single node runs on one binary and one file; no database service to install or operate.
- The production PostgreSQL path is unchanged: same driver, pool and types.
- Contract tests run unchanged against either backend, so drift between them shows up as a failing
  test.
- No transaction is held across storage work, on either backend.

### Negative
- `database.DB` is an interface: ADR-001's concern about indirection is accepted, at the boundary
  only — services still write SQL directly.
- The translator is code to maintain, and a construct it does not cover must be rewritten in portable
  SQL. The static check makes that a build failure rather than a production one.
- SQLite is single-node. Multi-node deployments stay on PostgreSQL.
- A regular expression in a server-name filter follows each backend's own dialect, as upstream's
  database filter does.
