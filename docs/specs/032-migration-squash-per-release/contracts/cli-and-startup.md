# Contract: `lightstack-migrate`, server start and their messages

What an operator or developer sees. `NNN` stands for a three-digit version.

## Messages

Every refusal names its cause and its way out (SC-003). The texts below are templates. Tests match their stable
parts, shown in bold. Error texts carry no final period, as Go error strings do not; the CLI prints them after
a prefix such as `Migration failed: `.

**Pre-squash** (FR-011), the same on both backends:

> the database is at migration version V, **from before the migration squash**. Recreate the database. See
> docs/UPGRADING.md, "The migration squash"

**Stale open release migration** (FR-020):

> the database **applied an earlier state of the open release migration** NNN (unreleased): a fragment was added,
> changed or removed since. Recreate the database.

**Stale after freeze** (FR-020):

> the database **applied an earlier state of migration** NNN, which has since been frozen as release vX.Y.Z with
> different content. Recreate the database.

**No record** (stale, defensive):

> the database is at migration version V but **holds no record of what it applied**. Recreate the database.

**Newer Lightstack**: unchanged (SPEC-030 FR-008). It is now reached only by a post-squash database whose version
is above the newest one in the view.

`dbtest` adds this line to a pre-squash or stale refusal on the shared PostgreSQL test database,
recognised with `IsPreSquash(err)` or `IsStale(err)` and never by text:
`(the test database: make db-down && make db-up)`.

## Server start

`cmd/lightstack/main.go` is unchanged. `MigrateUp` classifies the database first (research R9), then migrates.

| Database state | Start |
|---|---|
| fresh | migrates from 001, then serves |
| current | migrates up, logging `applied the open release migration NNN (unreleased)` when it applied one (FR-018), then serves |
| pre-squash, stale, no record, newer | `Failed to run migrations: <message>`, exit 1, database untouched |
| dirty | golang-migrate's refusal, as today |

## `lightstack-migrate` commands

| Command | Pre-squash database | Stale database | Otherwise |
|---|---|---|---|
| `up` | refused, exit 1 | refused, exit 1 | as today, plus the FR-018 log line |
| `down` | refused before the prompt, exit 1 | refused before the prompt, exit 1 | as today. Rolling back the open migration rolls back all its fragments and deletes its record row. |
| `goto <v>` | refused before the prompt, exit 1 | refused before the prompt, exit 1 | as today |
| `force <v>` | refused before the prompt, exit 1 | allowed (the repair tool for a dirty version) | as today |
| `status` | prints the state and message, exit 1 | prints the state and message, exit 1 | prints version and kind, exit 0 |
| `check` (SQLite) | `Check failed: <pre-squash message>`, exit 1 | `Check failed: <stale message>`, exit 1 | as today |
| `reset` | allowed: it recreates the database | allowed | as today |
| `backup` | allowed: it copies the file | allowed | as today |
| `freeze <release>` | needs no database | — | [migrations-folder.md](migrations-folder.md) |

Removed: the below-baseline refusal of `goto` and `force`. Nothing lies below `001`.

### `status` output (FR-022)

```text
Database Migration Status
=========================
Current Version: 2
Migration: open release migration 002 (unreleased)
Status: Clean
```

The `Migration:` line reads one of:
- `baseline`
- `release vX.Y.Z`
- `open release migration NNN (unreleased)`
- `none (empty database)`

A pre-squash database prints `Status: PRE-SQUASH`, and a stale one `Status: STALE`. A SQLite database file that does
not exist yet prints `Status: NO DATABASE`: still an error, as SPEC-028 makes a mistyped path one, and nothing is
created, but reported as a status rather than as a failure. Each is followed by a blank line, the message, and
exit 1.

### Usage text addition

```text
  freeze <release>      Freeze migrations/next/ into the release migration NNN_<release>; needs no database
```
