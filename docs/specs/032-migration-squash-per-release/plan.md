# Implementation Plan: Squash the migrations, restart at 001, one migration per release

**Branch**: `feature/596` (feature id: `032-migration-squash-per-release`) | **Date**: 2026-11-17 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `docs/specs/032-migration-squash-per-release/spec.md` (cloud-storage#596)

## Summary

The 83 migration versions (184 files) become one generated baseline per backend at `001`, and every later schema
change reaches a database as one migration per release. The change spans `internal/database`, one new
`lightstack-migrate` subcommand, three CI checks and documentation, and it deletes the bare-metal upgrade script.
**No new Go modules and no API change.** The only schema additions are one bookkeeping table and, on SQLite, the
NOT NULL exception.

**The squash** (US1, research R1–R4):
- The pre-squash files move with `git mv` to `internal/database/testdata/pre-squash/`. History and blame survive,
  and nothing under `testdata/` is shipped.
- `001_baseline.postgres.*` is rendered from the catalog of a scratch database migrated with that history.
  - The introspection, which stays test code, learns constraint names, comments and `SERIAL`.
  - Install-time ids stay `gen_random_uuid()` and install-time timestamps stay `now()`.
  - `pg_dump` was rejected: PostgreSQL 18 writes random-keyed `\restrict` meta-commands that golang-migrate cannot
    run.
- `001_baseline.sqlite.*` comes from the existing generator, retargeted. Its five `project_id` columns become real
  NOT NULL columns, replacing the 10 triggers of `079`/`080` (FR-005 exception).
- `TestBaselineEquivalence` migrates the history and the baseline side by side on both backends and compares them
  object by object, with generated values compared as generated.

**Old databases** (US2, R5, R9, R13):
- The baseline creates `applied_migrations`. Its existence marks a post-squash database, so a recorded version
  without it is pre-squash, whatever the number.
- One classification before golang-migrate runs drives every entry point. It replaces golang-migrate's
  `no migration found` and the false "written by a newer Lightstack" for these databases.
- Nothing carries a pre-squash database over. It is refused on both backends, and the way out is to recreate it:
  v0.7.0 is a fresh install (spec Clarifications, Session 2026-11-19; research R10).
- Bare metal is upgraded by hand, as `docs/UPGRADING.md` describes. `scripts/upgrade-lightstack.sh` is deleted
  rather than given guards (spec FR-013, research R13).

**One migration per release** (US3–US5, R6–R8, R11, R12):
- Fragments live in `migrations/next/`, named by UTC creation time, ticket and purpose.
- The view assembles them into a synthetic `NNN_open` migration for each backend. Each migration's SQL records its
  own checksum in `applied_migrations`, so the record commits atomically with the migration. The guard compares
  that record with the `-- checksum:` header of the file the view serves.
- `lightstack-migrate freeze <release>` writes the same body as `NNN_v<release>` and empties `next/`.
- CI enforces three things: numbered files are immutable once on `main`; no numbered file other than the baseline
  and release migrations exists (a folder unit test); and no fragments ship in a `v*` tag (`security.yml`).

## Technical Context

**Language/Version**: Go 1.27.3 (`go.mod`), Bash (CI steps, the immutability check and its test), Markdown.

**Primary Dependencies**: golang-migrate v4.19.1 (the `iofs` source over the per-backend view, the PostgreSQL driver over lib/pq, the SQLite driver), `jackc/pgx/v5` v5.10.0 (classification; the introspection in the tests), `modernc.org/sqlite` v1.60.1, `crypto/sha256` (stdlib). **No new modules.**

**Storage**: PostgreSQL 18 and SQLite from the one `migrations/` folder, both starting at a generated `001` baseline. A new table `applied_migrations (version, checksum, applied_at)` is created by the baseline on both. On SQLite, five `project_id` columns become NOT NULL (FR-005). No other schema or data change, as FR-004 proves.

**Testing**: Go unit tests and database-backed tests on `LIGHTSTACK_TEST_DB_URL`, PostgreSQL and SQLite, as CI runs
them:
- the equivalence check;
- view assembly, checksums and validation, including a test over the real folder;
- classification of every state in data-model §3 and the refusal messages;
- `project_id` NULLs reported as NOT NULL violations on both backends (FR-005);
- freeze: output, refusals, and a byte-for-byte match with the open migration;
- the two-branch scenario (SC-002);
- startup refusals in `cmd/lightstack/startup_refusal_test.go`;
- a bash test for the immutability check.

Gates: `make lint`, both contract suites and `test/quick_test.sh`.

**Target Platform**: Linux (CI, real mode) and macOS (development, stub mode). Migrations do not depend on the mode.

**Project Type**: single Go project (`cmd/` + `internal/`), plus CI workflows, scripts and docs.

**Performance Goals**:
- A fresh database applies one migration per backend instead of 83 (PostgreSQL) or 11 (SQLite) (SC-001).
- Startup adds one inspection: two to three small queries.

**Constraints**:
- golang-migrate stays.
- Each migration file runs in one transaction (R5, R17).
- Every refusal names its cause and its way out (SC-003).
- A numbered file never changes once on `main` (FR-025).
- No PR or branch outside this one is referenced (repository convention).

**Scale/Scope**:
- New files in `internal/database`: `migration_body.go`, `migrate_fragments.go` and `migrate_state.go`, plus their
  tests (`tasks.md` lists each one). The PostgreSQL introspection the equivalence test and the generator use is
  test code.
- Changed: `migrate.go`, `migrate_view.go`, `backup.go`, `baseline_gen_test.go`, `dbtest/dbtest.go`,
  `cmd/lightstack-migrate/main.go` (the `freeze` subcommand, `status` output, usage), `Makefile` (2 targets in, 1 out),
  2 workflows, 12 Markdown documents plus `doc.go`, and 19 production code comments.
- 184 files move to testdata; 4 baseline files are generated; 3 tests and 1 script retire; the upgrade script is
  deleted.

No `NEEDS CLARIFICATION` remains. The specify, clarify and planning sessions of 2026-11-17 settled 12 questions,
recorded in the spec's Clarifications, and research R1–R17 settles every design choice.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design (below the table).*

| Principle (`.specify/memory/constitution.md` v1.4.0) | Verdict | Why |
|---|---|---|
| I. OpenStack Flamingo API compatibility | PASS | No endpoint, response or microversion changes. The SQLite NOT NULL change makes a NULL `project_id` fail the same way on both backends. No handler writes one, and none reads `IsNotNullViolation`. |
| II. Synchronous operations | PASS | Classification and freeze are one-shot and synchronous. No loop, queue or worker is added. |
| III. Fail-fast | PASS | Every refusal is immediate, with no retry or backoff. |
| IV. Multi-mode | PASS | The database layer is mode-independent. All tests run in stub mode and on macOS. |
| V. Project isolation | PASS | No query scoping changes. `applied_migrations` is deployment metadata with no `project_id`, as `schema_migrations` already is. |
| VI. Single binary | PASS | No new binary. `freeze` is a `lightstack-migrate` subcommand. The migrations folder is still read from disk, now with `next/` inside it. |
| Technology constraints | PASS | "golang-migrate migrates both from the one `migrations/` folder" still holds: fragments live inside it and the view assembles them. No new dependency. |
| Forbidden patterns | PASS | None introduced. |
| Test-first | PASS by plan | The test strategy goes to the user for approval before any test is written (Delivery sequence, step 2; tasks.md T003; the constitution's Test-First step 2). Then each phase writes its failing tests first, and each fails because its function, file or message does not exist yet. |
| Backwards compatibility ("migrations must be reversible") | PASS | The baseline's down drops everything, every fragment carries its down part, and every release migration has a down file. |
| Backwards compatibility (existing deployments) | PASS, documented break | Pre-squash databases are refused, and the way out is to recreate them: v0.7.0 is a fresh install. The spec settles that nothing deployed today has to survive and that v0.7.0 is the first post-squash release. The break is documented in `docs/UPGRADING.md`, as the project handles breaking changes. No API change, so no microversion is involved. |
| Security | PASS | No secrets, and no privilege beyond what Lightstack's own database user already has. |
| Quality gates before merge | PASS by plan | Migrations go up and down on both backends (`TestMigrationsRoundTrip`, the equivalence test). Both backends run unit and contract suites, because this change is all schema. |

**Post-design re-check (after Phase 1)**: unchanged, with no deviation and no amendment needed.
- The design adds one table, one subcommand and one view feature. Each consumes the SPEC-028 database seam rather
  than extending it.
- ADR-003 gets a dated amendment note: the per-backend view's rule 3 retires, and fragments assemble into the open
  release migration. The decision it records, one folder read through per-backend views, still stands.

## Project Structure

### Documentation (this feature)

```text
docs/specs/032-migration-squash-per-release/
├── spec.md
├── plan.md                    # this file
├── research.md                # Phase 0: R1–R17
├── data-model.md              # Phase 1: folder layout, applied_migrations, database states, body/checksum, fragment names
├── quickstart.md              # Phase 1: manual verification, one section per user story
├── contracts/
│   ├── migrations-folder.md   # grammar, validation, assembly, freeze, generators
│   ├── cli-and-startup.md     # messages, server start, lightstack-migrate commands, status
│   └── ci-and-scripts.md      # CI checks, Makefile, test scripts
├── checklists/requirements.md
└── tasks.md                   # Phase 2 (/cl.tasks, not created here)
```

### Source Code (repository root)

```text
migrations/
├── 001_baseline.postgres.{up,down}.sql   # NEW, generated (FR-001–FR-003); creates applied_migrations
├── 001_baseline.sqlite.{up,down}.sql     # NEW, generated; real NOT NULL replaces the 079/080 triggers
├── next/.gitkeep                         # NEW: fragments live here
└── README.md                             # rewritten: baseline, fragments, release freeze, immutability, guard

internal/database/
├── migration_body.go          # NEW: BODY checksum, renderUp/renderDown (header, record INSERT/DELETE),
│                              #   parseChecksumHeader
├── migrate_fragments.go       # NEW: fragment grammar, assembly per backend (synthetic NNN_open), Freeze
├── migrate_state.go           # NEW: database classification (data-model §3), refusal messages
├── migrate_view.go            # rule 3 out; serves the synthetic NNN_open files; expected-checksum lookup
├── migrate.go                 # entry points classify first; RefuseBelowBaseline and baselineVersion out;
│                              #   ValidateMigrations enforces the new grammar; FR-018 log line
├── backup.go                  # IntegrityCheck classifies (pre-squash and stale fail the check)
├── baseline_gen_test.go       # SQLite generator retargeted (001, applied_migrations and its DDL per backend);
│                              #   reads testdata/pre-squash
├── baseline_gen_postgres_test.go  # NEW: TestGeneratePostgresBaseline (-update)
├── equivalence_test.go        # NEW: TestBaselineEquivalence{Postgres,SQLite}, TestBaselineGeneratesInstallTimeIDs,
│                              #   TestBaselineProjectIDNotNull
├── migration_body_test.go     # NEW: checksum, header, record statements
├── introspect_postgres_test.go    # NEW: the PostgreSQL introspection (schema parameter, constraint names,
│                              #   comments, SERIAL/sequences), its normaliser and exact diff, and their tests
├── migrate_helpers_test.go    # NEW: migrateFolderNoChecks, dropBelow (replays the pre-squash history)
├── migrate_fragments_test.go  # NEW: grammar, assembly, the view's synthetic files
├── migrate_fragments_apply_test.go  # NEW: creation order, per-fragment replay naming the failure, FR-018 log
├── migrate_fragments_merge_test.go  # NEW: two-branch scenario (SC-002)
├── migrate_freeze_test.go     # NEW: freeze output, parity with the open migration, refusals, FR-021
├── migrate_state_test.go      # NEW: every state, every message, the stale guard
├── migrate_test.go            # fixtures to the new grammar; TestMigrationsFolder over the real folder
├── migrate_view_test.go       # fixtures to the new grammar
├── schema_introspect_test.go  # shrinks to the test helpers (scratch databases)
├── migration_080_sqlite_test.go   # REMOVED (R15)
├── testdata/pre-squash/       # NEW: the 184 pre-squash files (git mv)
└── dbtest/dbtest.go           # the shared test database's remedy appended to refusals

internal/keystone/groups_domain_migration_test.go   # REMOVED (R15)

cmd/lightstack-migrate/
├── main.go                    # freeze (before config load); status kind/state lines; pre-squash and
│                              #   stale refusals before prompts; usage
└── main_test.go               # + TestFreezeNeedsNoConfig
cmd/lightstack/
└── startup_refusal_test.go    # + pre-squash and stale startup refusals

.github/workflows/ci.yml        # + immutability step in the build job (FR-025)
.github/workflows/security.yml  # + tag-push job: no fragments in a v* tag (FR-027)

scripts/upgrade-lightstack.sh   # REMOVED (FR-013, R13): bare metal is upgraded by hand
test/endpoint_hostname_migration_test.sh   # REMOVED (R15)
Makefile                        # + migrations-freeze, migration-fragment; − test-endpoint-migration

docs/UPGRADING.md               # one section per release; v0.7.0: install fresh ("The migration squash"),
                                #   never run a pre-squash build on a post-squash database
CLAUDE.md                       # Database Schema + Migrations paragraphs
CHANGELOG.md                    # [Unreleased]: breaking — migrations squashed
docs/adr/ADR-003-pluggable-database-backend.md   # dated amendment note
docs/ARCHITECTURE.md, docs/blueprint.md, docs/compatibility/{HORIZON,TERRAFORM}_COMPATIBILITY_REPORT.md,
docs/EBPF_STATUS.md, docs/L3_ROUTER_IMPLEMENTATION.md, docs/STUB_PLACEHOLDERS.md, docs/VXLAN_IMPLEMENTATION.md,
internal/database/doc.go, and 19 production code comments plus test comments that cite a migration number
```

**Structure Decision**: single Go project. The weight is in `internal/database`, in three new files beside the
existing migrator. The PostgreSQL introspection stays test code. The CLI, CI and docs consume it. No chart template,
`values.*`, Compose file or Dockerfile changes: the binary migrates itself at start, and the image already copies the
whole `migrations/` folder.

## Delivery sequence

1. **Squash point.** Cut from `main` once every numbered migration that should be folded in has merged. Rebase.
   The baseline reproduces `main`'s newest version. If another numbered migration merges before this change does,
   rebase and regenerate (spec Dependencies).
2. **Baseline gates, approved test strategy, RED tests.** Record `make lint` and `go test ./...` green on both
   backends. Present the test strategy to the user and wait for approval (constitution Test-First, step 2). Then
   write failing tests:
   - `TestBaselineEquivalence{Postgres,SQLite}`, the install-time-id test, and the SQLite NOT NULL violation test;
   - assembly, ordering and checksum;
   - `TestMigrationsFolder` with a hand-numbered file;
   - classification and messages;
   - freeze parity;
   - startup refusals;
   - the immutability bash test.

   Confirm each fails.
3. **Move the history** to `testdata/pre-squash/` (`git mv`).
4. **Introspection**, in test code: schema parameter, constraint names, comments, sequences.
5. **Generators**: write the PostgreSQL generator and retarget the SQLite one. Generate the four baseline files,
   including `applied_migrations` and record row 1.
6. **Equivalence green** on both backends, with `TestSchemaParity` and `TestMigrationsRoundTrip` on the new folder.
7. **View and validation**: retire rule 3, `RefuseBelowBaseline` and `baselineVersion`; enforce the new grammar;
   assemble fragments into `NNN_open`; look up expected checksums.
8. **Classification and refusals** wired into `MigrateUp`, `MigrateDown`, `MigrateToVersion`,
   `ForceMigrationVersion`, `GetMigrationStatus` and `IntegrityCheck`; the `status` output; the FR-018 log line;
   the `dbtest` remedy.
9. *(Re-stamp, dropped on 2026-11-19: research R10.)*
10. **Freeze** subcommand, then the Makefile targets `migrations-freeze` and `migration-fragment`.
11. **Retire** `groups_domain_migration_test.go`, `migration_080_sqlite_test.go`,
    `endpoint_hostname_migration_test.sh` and `make test-endpoint-migration`.
12. **CI**: the immutability step in `ci.yml` and the tag-push job in `security.yml`.
13. **Upgrade script**: delete `scripts/upgrade-lightstack.sh` (FR-013, R13).
14. **Docs**:
    - `migrations/README.md`;
    - `CLAUDE.md`;
    - `docs/UPGRADING.md` (one section per release; v0.7.0 is a fresh install);
    - the ADR-003 note;
    - the living documents;
    - code and test comments.
15. **Green**: both backends' unit tests, both contract suites, `test/quick_test.sh`, lint and fmt. Then the
    quickstart, sections 1–7.

## Complexity Tracking

> No constitution deviations. The feature adds no abstraction beyond what the requirements name.
> - One bookkeeping table: golang-migrate records only `version` and `dirty`, and replacing it is out of scope.
> - One synthetic file in the existing view.
> - One subcommand.
>
> Table intentionally empty.
