# Research: Squash the migrations, restart at 001, one migration per release

**Feature**: [spec.md](spec.md) · **Plan**: [plan.md](plan.md) · **Date**: 2026-11-17

Facts below were read off this branch, not assumed:

| Fact | Value |
|---|---|
| Migration folder | 82 versions (`001`–`082`), 180 SQL files |
| SQLite history | the generated `073_baseline.sqlite` plus 9 versions (`074`–`082`) |
| Last release | v0.6.1 shipped `001`–`059`. No release ever shipped SQLite. |
| Go toolchain | Go 1.27.3 (`go.mod`) |
| Modules | golang-migrate v4.19.1, pgx v5.10.0, `modernc.org/sqlite` v1.60.1, lib/pq v1.12.3 (indirect, via golang-migrate's PostgreSQL driver) |
| Object kinds in the PostgreSQL history | tables, constraints, indexes (one GIN), 3 `COMMENT ON`, 1 `SERIAL` (`host_aggregates.id`). No functions, triggers, views, types, sequences of their own, extensions or `GRANT`. |
| SQLite-only forms | `079` and `080` keep `project_id` NOT NULL on volumes, ports, networks, routers and floating_ips with 10 triggers |
| PostgreSQL history replayed over the baseline | `002` runs again and inserts the default security group's two egress rules a second time: `security_group_rules` has only a generated key, so `ON CONFLICT DO NOTHING` never fires. `003` then fails on `CREATE INDEX idx_routers_project_id`, which has no `IF NOT EXISTS`, and leaves the database dirty at `003`. |
| `make build` | builds `bin/lightstack` only; `lightstack-migrate` is built by `go run` (`make migrate`) or the Dockerfile |

---

## R1 — Where the pre-squash history goes

**Decision**: `git mv` the 180 pre-squash files to `internal/database/testdata/pre-squash/`. The baselines, the
generators and the equivalence check read the history from there.

**Rationale**:
- FR-007 asks that the files leave `migrations/`. Testdata is not shipped: the runtime image copies only
  `migrations/`.
- The move keeps `git log --follow` and blame on every file. The squash change reads as a rename, not as 180
  deletions.
- FR-002 ("the same history always generates the same files") and FR-004 (the equivalence check) stay runnable
  without git. CI's shallow checkouts hold no older commit to read the history from.
- Both inputs are frozen (FR-025), so the equivalence check never changes its verdict. It still runs in seconds.

**Alternatives considered**:
- *Delete the history.* The equivalence check would have to read it from git in CI (`fetch-depth: 0`, a pinned
  commit) and skip wherever there is no `.git`. A check that skips is not a proof.
- *Concatenate the history into `001`.* Equality would be trivial, but the five endpoint rewrites and every other
  one-time fix would stay in every install. The ticket exists to remove exactly that. It is also not "generated
  from a database" (FR-002).

## R2 — Generating the PostgreSQL baseline

**Decision**: render `001_baseline.postgres.up.sql` from the catalog of a scratch database migrated with the
history, as the SQLite generator already does.
- The introspection stays test code (`introspect_postgres_test.go`): only the generator and the equivalence check
  (R4) read it.
- It gains a schema parameter, constraint names (`pg_constraint.conname`), column and table comments, and the
  `SERIAL` pattern: a `nextval('<table>_<col>_seq'::regclass)` default on an `int4` column whose sequence the
  column owns.
- Render order:
  1. tables with their columns, defaults, primary key, `UNIQUE` and `CHECK` constraints, all named;
  2. foreign keys as `ALTER TABLE … ADD CONSTRAINT`, after every table exists;
  3. indexes from `pg_get_indexdef`, unqualified;
  4. `COMMENT ON`;
  5. seed rows, parents first.
- A seed id PostgreSQL generated at install time stays `gen_random_uuid()`, using the SQLite generator's existing
  `generatedID` rule. A seed timestamp stays `now()`.

**Rationale**: the output is deterministic and contains plain SQL only. Every construct is mapped on purpose: an
unmapped one stops the generator, as it stops the SQLite one today. Constraint names match the history's, so a
later release migration that drops a constraint by name works on every database.

**Alternatives considered**:
- *`pg_dump --schema-only` plus generated seed rows.* PostgreSQL 18's `pg_dump` writes the psql meta-commands
  `\restrict` and `\unrestrict` into plain dumps, keyed at random unless `--restrict-key` is given. This was
  verified: `pg_dump` 18.4 lists `--restrict-key` in its help. golang-migrate cannot execute meta-commands, and the
  output differs per run. The dump also resets `search_path` to empty for its session and qualifies every name, so
  the record row this feature appends (R5) would need special handling. Its data section freezes the generated
  ids.

## R3 — Generating the SQLite baseline

**Decision**: retarget `TestGenerateSQLiteBaseline`:
- its target becomes the squash point;
- it writes `001_baseline.sqlite.{up,down}.sql`;
- it appends the SQLite form of `applied_migrations` and the baseline's record row.

The five trigger-guarded `project_id` columns come out as real NOT NULL columns, because the PostgreSQL catalog has
them that way (spec FR-005 exception, Clarifications). The GIN index over image properties stays unmirrored, as
today.

**Rationale**: the generator already renders the PostgreSQL catalog as SQLite DDL. The triggers existed only
because `ALTER TABLE` could not add NOT NULL to a referenced table, and a fresh `CREATE TABLE` can. With them gone,
`IsNotNullViolation` matches on both backends. Today it is false on SQLite for those columns, because the trigger
raises a trigger error. No production code reads that classifier, so no behaviour changes.

## R4 — Proving the baseline equals the history (FR-004)

**Decision**: add `TestBaselineEquivalence`, a database-backed test on both backends that runs in CI's unit-test job.

PostgreSQL:
- Migrate two scratch databases, one with the testdata history and one with `migrations/`.
- Compare the exact model: tables; columns (type, length, nullability, default); constraints by name and
  definition; indexes; comments; sequences (type, owner, `last_value`, `is_called`); seed rows. Generated ids and
  install-time timestamps are compared as generated.
- `schema_migrations` and `applied_migrations` are excluded from the comparison.

SQLite:
- Migrate two files: the old SQLite history (`073` baseline plus `074`–`082`, through the view with versions below
  `073` dropped) and the new baseline.
- Compare the semantic model that `TestSchemaParity` already uses (`sqliteModel`, `diffModels`). It reads a
  trigger-guarded column as NOT NULL, so the FR-005 exception compares equal.
- Dropping the versions below `073` is the retired view rule 3 (R15), kept in this one test.

**Rationale**: two migrated databases compared object by object prove what a text diff of two files cannot.

## R5 — The post-squash marker and the applied-state record (FR-012, FR-019)

**Decision**: add one table, `applied_migrations (version, checksum, applied_at)`, which the baseline creates on
both backends.
- Every post-squash migration records itself in its own SQL: the baseline, each release migration and the open one.
  The up part ends with an `INSERT`; the down part begins with a `DELETE` of its row.
- The table's existence is the post-squash marker. A database with a recorded version and no such table is
  pre-squash, whatever the number.

**Rationale**: the record commits atomically with the migration it describes. This was verified in golang-migrate
v4.19.1:
- The PostgreSQL driver sends a whole file as one `ExecContext` through lib/pq's simple-query protocol. PostgreSQL
  runs a multi-statement simple query as one implicit transaction.
- The SQLite driver wraps each file in `Begin`/`Commit` (`NoTxWrap` is false).

So no crash and no second node can see a migration without its record. The version number alone cannot mark the
squash, because the restarted numbering reuses old numbers (Clarifications).

**Alternatives considered**:
- *Our code writes the record after `m.Up()` returns.* Two transactions: a crash between them, or a second node
  starting between them, sees an unrecorded migration.
- *A checksum of every applied migration kept by our code.* That re-implements a migrator. Replacing golang-migrate
  is out of scope.

## R6 — What a checksum covers

**Decision**: the checksum is the SHA-256 (stdlib) of a migration's **body**:
- for the open and release migrations, the fragments in name order, each preceded by `-- fragment: <name>`;
- for the baseline, its generated SQL.

Every up file carries `-- checksum: <hex>` in its header, and its record row stores the same value. The guard reads
the expected value from the header of the file the view serves for that version.

**Rationale**: the freeze writes exactly the body the open migration served, so the checksum is identical by
construction (FR-021, FR-023). The header lines differ between the open and frozen forms ("unreleased" and
"release v0.7.0"), which is why they stay outside the checksum. A renamed fragment changes the body, and that is
intended.

## R7 — Assembling the open release migration (FR-015, FR-017)

**Decision**: `newMigrationView` reads `migrations/next/`, validates the fragments and serves a synthetic
`NNN_open.up.sql` and `NNN_open.down.sql` from memory, as an in-memory `fs.File`. `NNN` is one above the newest
numbered migration.
- Each backend's body is the plain fragments plus its own fragments, in name order.
- The down part deletes the record row, then runs the fragments' down parts in reverse order.
- When any fragment exists, every backend gets the version. A backend without a fragment of its own gets a body of
  the plain fragments, or an empty body. Versions therefore stay aligned across backends, and a recorded version
  names the same release on both.

**Rationale**: every reader already goes through the view: startup, every `lightstack-migrate` command, the
integrity check's reference schema and the tests' `dbtest` helper. One change there satisfies FR-017.

## R8 — Folder rules and where they are enforced (FR-016, FR-026)

**Decision**: `ValidateMigrations` enforces the folder grammar
([contracts/migrations-folder.md](contracts/migrations-folder.md)):
- version `001` is exactly the two baseline pairs;
- every later numbered file is `NNN_v<semver>[.postgres|.sqlite].(up|down).sql`, with consecutive versions, one
  release per version and no release twice;
- any other numbered file fails with "schema changes go into fragments";
- fragments follow their grammar, with up and down per variant and no duplicate name for one backend.

A unit test runs `ValidateMigrations` over the real folder, so CI fails on a hand-numbered file. The view keeps
only the mechanics (rules 1 and 2, plus assembly), so it can still replay the testdata history for R4.

## R9 — Classifying a database before migrating (FR-011, FR-012, FR-020, FR-022)

**Decision**: one inspection runs before golang-migrate does. It opens a short connection, reads
`schema_migrations` and `applied_migrations`, and returns one of the states in [data-model.md](data-model.md) §3:
fresh, pre-squash, baseline in flight, current, stale, newer or dirty.
- On PostgreSQL the connection comes from `pgx`.
- On SQLite the inspection first runs `os.Stat` on the path. A file that does not exist yet is **fresh**, and the
  inspection does not create it: `MigrateUp` creates it, as it does today. An existing file opens through
  `openForInspection`, the must-exist, read-only handle `check` already uses, so inspecting changes nothing,
  not even the journal mode. Without the `Stat`, every fresh SQLite install would fail at its first start.

Refusals are typed errors, `*PreSquashError` and `*StaleError`, with the predicates `IsPreSquash` and `IsStale`
(`errors.As`). Callers branch on the type, never on message text.

These entry points call it:
- `MigrateUp`, which covers startup and `up`;
- `MigrateDown`, `MigrateToVersion` and `ForceMigrationVersion`;
- `GetMigrationStatus`;
- `IntegrityCheck`, which reports a stale state as a failure, as `status` does.

`MigrateReset` and `Backup` do not.

**Race**: the inspection runs without golang-migrate's lock.
- A dirty version `1` without `applied_migrations` is another node applying the baseline right now. It is not
  classified pre-squash; the inspection defers to golang-migrate, which waits for the lock and then finds the
  version clean.
- A dirty version is never classified stale; golang-migrate's dirty handling applies.

Every other state is final once read, because the record commits with its migration (R5).

## R10 — Re-stamp (FR-009, FR-010): dropped on 2026-11-19

**Decision** (amended 2026-11-19): no re-stamp. A database from before the squash is refused on both backends and
recreated: v0.7.0 is a fresh install (spec Clarifications, Session 2026-11-19).

**Rationale**:
- v0.6.1, the last release, shipped migrations up to `059`, so no release database can ever be at the squash point.
  Only a database built from `main` could be, and recreating those was accepted from the start (Clarifications,
  Session 2026-11-17).
- Re-stamp cost a subcommand, the schema introspection in production code, a lock key derived from golang-migrate's
  internals and a scratch-schema comparison, all for databases nobody has to keep.
- The pre-squash refusal (R9) stays. Without it, an old database would read as "written by a newer Lightstack", or,
  once a few releases exist, pass as current at a low version.

**Alternatives considered**:
- Re-stamp, the decision until 2026-11-19. `lightstack-migrate restamp` ran on PostgreSQL only, as one transaction
  under golang-migrate's advisory lock. It checked that the database recorded exactly the squash point, clean and
  without `applied_migrations`. It compared the live schema exactly with the baseline applied in a scratch schema,
  then recorded `001`. It was implemented and tested, then dropped for the reasons above.

## R11 — Freezing a release (FR-023, FR-024)

**Decision**: add the subcommand `lightstack-migrate freeze [-migrations dir] <release>`. It is dispatched before
the configuration is loaded, because it needs no database. A Makefile target wraps it:
`make migrations-freeze RELEASE=v0.7.0`.
- It writes the open migration's bodies as `NNN_<release>` files, one plain pair when every fragment is plain and
  backend pairs otherwise, then removes the fragments.
- It refuses a release name that is not `v<semver>`, and a release that already has a migration.
- With no fragments it adds nothing and says so.

A second Makefile target, `migration-fragment TICKET=… NAME=…`, creates an up/down pair named with the current UTC
time.

**Rationale**: the freeze must emit exactly what the view served (R6), so it calls the same assembly code. A
subcommand also gives it a usage text and unit tests.

**Alternatives considered**: a generator test run with `-update`, as the baselines use. That is clumsy as a step
someone runs at release time with a release name.

## R12 — CI and the tag check (FR-025, FR-027)

**Decision**:
- **Immutable once on `main`**: a step in `ci.yml`, on pull requests and pushes, fails when
  `git diff --no-renames --name-status --diff-filter=DM <base> HEAD -- 'migrations/001_baseline.*' 'migrations/[0-9][0-9][0-9]_v[0-9]*'`
  lists anything. With `--no-renames`, a rename appears as a deletion.
  - On a pull request, the base is the fetched base branch tip; on a push, it is `github.event.before`.
  - The second pattern requires a digit after `_v`, as every release name has (`002_v0.7.0…`). That digit is
    what keeps the squash change passing without an exception in code. A looser `[0-9][0-9][0-9]_v*` would also
    match two pre-squash files that the squash moves, `044_volume_groups` and `073_volume_transfer_awaiting`, and
    fail the squash change itself. This was checked against the folder: no pre-squash name matches
    `^[0-9]{3}_v[0-9]`.
- **Hand-numbered files**: the folder test (R8) runs in the existing unit-test job.
- **No fragments in a release**: a job in `security.yml`, the workflow that runs on `v*` tags, fails when
  `migrations/next/` holds a `.sql` file and names those files.

## R13 — The upgrade script (FR-013)

**Decision** (amended 2026-11-18): delete `scripts/upgrade-lightstack.sh`. Bare-metal installs are upgraded by
hand, as `docs/UPGRADING.md` describes.

**Rationale**:
- It serves one layout only: the git checkout at `/opt/lightstack` that `lightstack.service` runs, which
  `scripts/deploy-single-node.sh` creates. Docker Compose and Helm upgrade by image and never use it.
- It could not finish an upgrade. `run_migrations` called `lightstack migrate --dry-run`, but `main` has never had
  a `migrate` subcommand. Go's flag parsing stops at `migrate`, so the server started in the foreground, and the
  script waited for a line no code prints.
- `update_code` stashed local changes before the checkout and never restored them. The deploy script writes the
  node's configuration over the tracked `config/lightstack.yaml`, so the upgraded service would have started with
  the release's default configuration.
- Its backup holds the binary, the configuration and the commit, not the database, so its rollback is unsafe
  across any migration.
- The squash does not need it. The new binary refuses a pre-squash database without touching it (FR-011). No new
  code can stop an old binary, so the rollback hazard is a warning in `docs/UPGRADING.md` either way.

**Alternatives considered**:
- Two guards in the script, the decision until 2026-11-18. One built the target's `lightstack-migrate` in a
  temporary `git worktree` and stopped on a pre-squash verdict from its `status`. The other refused a `--version`
  rollback to a pre-squash build. Both were implemented and tested, together with a fix of the migration step, and
  then dropped: they kept alive a script that had never completed an upgrade, for one install layout.
- Repairing the script: restore the configuration, back up the database, and test the whole flow on a systemd
  host. That is a feature of its own, not part of the squash.

## R14 — Messages

**Decision**: the wording lives in [contracts/cli-and-startup.md](contracts/cli-and-startup.md). Every refusal names
its cause and its way out (SC-003). The pre-squash refusal replaces both golang-migrate's
`no migration found for version N` and `refuseNewerVersion`'s "written by a newer Lightstack" for this case.
`dbtest` adds the shared test database's remedy, `make db-down && make db-up`, whenever `IsPreSquash` or
`IsStale` holds.

## R15 — Retired with the history

**Decision**:
- **View and migrator**: rule 3, `baselineVersion` and `RefuseBelowBaseline` go, together with its calls in `goto`
  and `force`, and "exactly one SQLite baseline" in `ValidateMigrations`.
- **Tests**:
  - `internal/keystone/groups_domain_migration_test.go` (072) is removed;
  - `internal/database/migration_080_sqlite_test.go` is removed;
  - `test/endpoint_hostname_migration_test.sh` and `make test-endpoint-migration` (069) are removed;
  - the synthetic fixtures in `migrate_test.go` and `migrate_view_test.go` move to the new grammar.

**Rationale**: each removed test checks a one-time data conversion that no post-squash database can reach (FR-007).
The pre-squash release keeps it.

## R16 — Documentation (FR-028)

**Decision**:
- `migrations/README.md` is rewritten.
- `CLAUDE.md` gets new Database Schema and Migrations paragraphs.
- `docs/UPGRADING.md` keeps one section per release, newest first. The v0.7.0 section says to install fresh
  (FR-013).
- `docs/adr/ADR-003-pluggable-database-backend.md` gets a dated amendment note; the record stays.
- `internal/database/doc.go` is updated.
- The living documents that cite migration numbers are updated: `docs/ARCHITECTURE.md`, `docs/blueprint.md`, both
  `docs/compatibility/*_REPORT.md`, `docs/EBPF_STATUS.md`, `docs/L3_ROUTER_IMPLEMENTATION.md`,
  `docs/STUB_PLACEHOLDERS.md` and `docs/VXLAN_IMPLEMENTATION.md`.
- The 19 production comments that cite a migration number state the schema fact instead, and so do test comments
  that do the same.
- The changelog, archived specs and reports, and other features' specs stay as written. The earlier
  `UPGRADING.md` sections go: a fresh install of v0.7.0 never meets them (FR-013).

## R17 — Fragments that cannot run in a transaction

**Decision**: not supported. Every fragment of a cycle runs inside one migration, which is one transaction on both
backends (R5). The existing rule in `migrations/README.md` stands: the first change that needs a statement outside
a transaction brings the migrator change with it. Such a change would also need a version of its own. No history
file uses `CONCURRENTLY` today.
