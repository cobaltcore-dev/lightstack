# Implementation Plan: SQLite backup, integrity check and restore

**Branch**: `feature/600` (feature id: `030-sqlite-backup-restore`) | **Date**: 2026-10-28 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `docs/specs/030-sqlite-backup-restore/spec.md` (cloud-storage#600, part 3 of #595)

## Summary

Parts 1 (SPEC-028) and 2 (SPEC-029) made the binary and the chart speak SQLite. This part gives an operator who chose SQLite what PostgreSQL has by default: a consistent online backup, an integrity check, and a documented restore and recovery. It is two new `lightstack-migrate` subcommands, a small `internal/database` addition, one `pkg/paths` helper, a clearer message for an existing startup refusal, and docs. **No new Go modules, no chart change, no schema change, no migration, no API change.**

**Backup (`lightstack-migrate backup [<file>]`).** Opens its own SQLite connection (must-exist) to the live database and runs `VACUUM INTO <target>` — a consistent snapshot taken under a read transaction, so Lightstack keeps serving writes throughout, and the output is a single standalone file with no `-wal`/`-shm` (R2, verified). `VACUUM INTO` refuses an existing target with a non-zero exit, which is the no-overwrite guarantee (R2). With no `<file>`, the target defaults to `paths.BackupsDir()/lightstack-<UTC timestamp>.db`; the CLI must call `paths.SetBase(cfg.StatePath)` first (R3). The backups directory is created `0750` and the backup file tightened to `0600` — the live database's modes (R4, FR-003a).

**Integrity (`lightstack-migrate check`).** Runs `PRAGMA integrity_check` on the database the URL names. A single `ok` row is success; any other rows are listed and the command exits non-zero; a hard `SQLITE_CORRUPT` ("database disk image is malformed") from the statement itself is also a non-zero failure (R6, verified — the bare `sqlite3` CLI exits 0 on problem rows, so our code interprets the result). It is read-only and runs against a live, serving database with no downtime (R6, spec Clarifications).

**PostgreSQL guard.** Both commands resolve the backend with the existing `database.ParseURL`; on a PostgreSQL URL each exits non-zero with a message naming `pg_dump` (R7, FR-006). The refusal lives inside the two `internal/database` functions, so the CLI handler stays thin and the behaviour is unit-testable without a database.

**Newer-version refusal (corrected during implementation).** Planning claimed FR-008's refusal was missing; a probe showed `golang-migrate` already stops on a database ahead of the binary (`no migration found for version N: read down …`), which startup treats as fatal. The change is the message: `MigrateUp` refuses such a database first, naming both versions and the cause, so startup and `lightstack-migrate up` share it with no extra wiring (research R8). Forward-migrating an *older* backup is the existing `MigrateUp` (R9).

**Restore & recovery (documented).** The restore procedure — stop, delete `-wal`/`-shm`, swap in the backup, `check`, start — with the WAL-replay pitfall called out (R10, verified: skipping the delete reinstates post-backup data). Kubernetes uses a `kubectl debug node/<node>` pod because the chart runs `/app/lightstack` directly and a scaled-down Deployment unmounts the host storage (R11); Docker Compose uses a one-off run container (R12). Recovery beyond the latest backup is the `sqlite3` CLI `.recover` run off-node — the image ships no `sqlite3` (R13, confirmed in the Dockerfile).

**Scope boundaries.** Backups are a real-mode operation under the already-mounted `host-storage` path, outside `db/` and already absent from `purgeLocalStorage`, so `dataRetentionPolicy: delete` keeps them with **no chart change** (R14). Stub-mode deployments are not backed up (spec FR-004a). A teardown test locks the exclusion.

## Technical Context

**Language/Version**: Go 1.27.3 (module pins `go 1.26`; Constitution requires 1.26+). Markdown docs.
**Primary Dependencies**:
- `jackc/pgx/v5` + `pgxpool` (PostgreSQL, unchanged) and `modernc.org/sqlite` (pure-Go SQLite, SPEC-028) — **no new modules**.
- `golang-migrate` v4 (`m.Version()`, already used) and the migration view, for naming the newer-database refusal.
- `pkg/paths` (gains `BackupsDir()`), `internal/common` (`cfg.StatePath`), `cmd/lightstack-migrate`.
**Storage**: the operator's SQLite database file and a sibling `backups/` directory under the node storage root (`state_path` / `realMode.hostStoragePath`, default `/var/lib/lightstack`). **No schema change, no migration, no persisted data owned by the chart.** PostgreSQL is untouched except the backend-neutral ceiling check.
**Testing**: Go unit + DB-backed tests on `LIGHTSTACK_TEST_DB_URL` (SQLite and PostgreSQL, per CLAUDE.md). Backup refuses an existing target; `backup`/`check` on a PostgreSQL URL exit non-zero naming `pg_dump`; default path/name; `check` passes intact and fails damaged (problem-row path and `SQLITE_CORRUPT` path); a restore round-trip that leaves `-wal`/`-shm` behind and reads back the backup's data; a database forced ahead of the binary is refused at start; `paths.BackupsDir()`; teardown does not purge `BackupsDir()`. One k3s real-mode host run of the Kubernetes restore (FR-013).
**Target Platform**: single-node SQLite deployments — k3s (Helm) and Docker Compose; Linux real mode. The CLI commands are file operations and run on macOS/dev too (Article IV).
**Project Type**: single Go project (CLI + internal library) plus docs. No `src/` layout.
**Performance Goals**: not a throughput feature. `backup` runs a `VACUUM INTO` whose duration scales with database size but does not block writers (WAL read snapshot); `check` is a read-only scan with no downtime.
**Constraints**:
- SQLite-only commands; PostgreSQL refused with a `pg_dump` message (FR-006);
- backup output is a single consistent file, never overwriting an existing target (FR-001/003);
- backup file `0600`, backups directory `0750`, root-owned (FR-003a);
- no new Go modules, no chart change, no schema change, no migration, no API change;
- `check` is online (FR-005); the newer-database refusal is backend-neutral and fires only when the database is ahead of the binary (FR-008).
**Scale/Scope**: ~1 new Go file (`internal/database/backup.go`), ~1 helper in `pkg/paths/paths.go`, ~1 helper in `internal/database/migrate.go` (version ceiling) wired into startup, 2 subcommands + handlers + usage in `cmd/lightstack-migrate/main.go`, 1 teardown test, ~6 unit/DB tests, 2 docs files. No chart templates, no `values.*`.

No `NEEDS CLARIFICATION` remains: the spec's Clarifications session (2026-10-28) closed backup permissions, the online nature of `check`, and the stub-mode boundary; research.md R1–R14 settle every design choice, including R8, corrected during implementation: the newer-database refusal already existed; only its message changes.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design (below the table).*

| Principle (`.specify/memory/constitution.md` v1.3.0) | Verdict | Why |
|---|---|---|
| I. OpenStack Flamingo API compatibility | PASS | No API surface is touched. These are operator CLI commands and documented procedures; no endpoint, response, or microversion changes. |
| II. Synchronous operations | PASS | `backup` and `check` are one-shot CLI operations, each a single statement; no queue, worker, or state machine. The ceiling check is a startup read. `VACUUM INTO` runs outside any app transaction and holds no write lock on the live database. |
| III. Fail-fast | PASS | Wrong backend, missing file, existing target, and an ahead-of-binary database all fail immediately with a clear message. No retries or backoff are added; the backup connection reuses SPEC-028's existing bounded WAL-open. |
| IV. Multi-mode | PASS | The CLI commands operate on a SQLite file regardless of service mode and are testable without Linux (macOS dev). Backups-as-a-deployment-operation are real-mode-scoped (FR-004a); that is a deployment boundary, not a code gate, and stub mode still runs the commands in tests. |
| V. Project isolation | PASS | No query or scoping change. A full-database backup is a node-admin operation, not a tenant-scoped API; it never crosses an API boundary. |
| VI. Single binary, SQLite single-node | PASS (v1.3.0) | Backup/check are SQLite-only and single-node, consistent with Article VI. PostgreSQL is directed to its own tooling. |
| Technology constraints | PASS | No new dependency of any kind. `modernc.org/sqlite`, `golang-migrate`, `pkg/paths` and the `sqlite3`/`pg_dump` CLIs (off-node, operator-side) are existing or external-to-image. |
| Forbidden patterns | PASS | No queue, worker, state machine, service discovery, or required orchestration. |
| Test-first | PASS by plan | RED first: unit tests for the `pg_dump` refusal, existing-target refusal, `check` intact/damaged, the newer-database refusal's message, and the restore round-trip — all written before the code, failing because the commands do not exist yet (the refusal test failed on its message). |
| Backwards compatibility | PASS | PostgreSQL deployments are unaffected except the backend-neutral ceiling check, which triggers only on a database genuinely ahead of the binary (a downgrade or a newer restore) — the correct, safe refusal. No migration; nothing to reverse. |
| Security | PASS | A backup copies the whole database (credential hashes, all tenant data); it is created `0600` under a `0750` directory, root-owned — the live database's least-privilege modes (FR-003a). No secrets added. |
| Quality gates before merge | PASS by plan | Both backends exercised: the `pg_dump` refusal runs on a PostgreSQL URL; backup/check/restore/ceiling on SQLite. No SQL or schema change, so the migration up/down gates are N/A. k3s real-mode host run for FR-013. |

**Post-design re-check (after Phase 1)**: unchanged. The design adds no abstraction and no dependency; the named refusal is a small addition inside the existing `MigrateUp`, not a new seam. **No deviations** — Complexity Tracking is empty. Like SPEC-029, this part consumes the SPEC-028/ADR-003 database seam rather than extending it, so it needs no ADR.

## Project Structure

### Documentation (this feature)

```text
docs/specs/030-sqlite-backup-restore/
├── spec.md
├── plan.md                 # this file
├── research.md             # Phase 0: R1–R14
├── data-model.md           # Phase 1: artifacts & paths (no DB entities, no schema change)
├── quickstart.md           # Phase 1: take a backup; restore on k3s and Compose; verify
├── contracts/
│   ├── cli-commands.md      # backup & check: inputs, exit codes, messages, backend guard
│   └── restore-recovery.md  # the documented restore (K8s + Compose) and recovery procedures
├── checklists/requirements.md
└── tasks.md                # Phase 2 (/cl.tasks — not created here)
```

### Source Code (repository root)

```text
cmd/lightstack-migrate/
└── main.go                 # + `backup [<file>]` and `check` subcommands, handlers, usage;
                            #   backup handler calls paths.SetBase(cfg.StatePath) before the default

internal/database/
├── backup.go               # NEW: Backup(ctx, dbURL, dest) and IntegrityCheck(ctx, dbURL);
│                           #   both guard the backend via ParseURL (PostgreSQL → pg_dump error);
│                           #   SQLite opens a must-exist handle, runs VACUUM INTO / integrity_check,
│                           #   tightens the backup file to 0600
├── migrate.go              # MigrateUp names the newer-database refusal (refuseNewerDatabase)
├── migrate_view.go         # + migrationView.latestVersion
└── backup_test.go          # NEW: unit/DB tests for both functions + the ceiling guard

pkg/paths/
├── paths.go                # + BackupsDir() => <base>/backups  (sibling of DBDir(), outside db/)
└── paths_test.go           # + BackupsDir()

cmd/lightstack/
└── main.go                 # unchanged — startup already calls MigrateUp

internal/teardown/
└── teardown_test.go        # + assert purgeLocalStorage() does NOT include BackupsDir() (FR-004)
                            #   (teardown.go itself unchanged — backups are already outside the list)

docs/
├── KUBERNETES_DEPLOYMENT.md # + backup (exec + kubectl cp), restore (node debug pod), recovery
└── DOCKER_DEPLOYMENT.md     # + backup (compose exec), restore (compose run), recovery
```

**Structure Decision**: Single Go project. The weight is in `internal/database/backup.go` and the two `cmd/lightstack-migrate` subcommands, with one supporting `pkg/paths` helper, one backend-neutral startup guard in `internal/database/migrate.go`, a teardown test, and two docs files. No chart templates and no `values.*` are touched — backups live under the existing real-mode `host-storage` mount, outside `db/`, and are already excluded from the purge.

## Delivery sequence

1. **Baseline gates** — record `go test ./...`, `make lint`, `make fmt` green on the branch so later claims have an oracle; confirm the migrate CLI builds for both backends.
2. **RED tests** — `internal/database/backup_test.go` (pg_dump refusal, existing-target refusal, default-path shape, `check` intact/damaged incl. the `SQLITE_CORRUPT` path, newer-database refusal message, restore round-trip with a stale `-wal`), `pkg/paths` `BackupsDir()`, and the teardown-exclusion assertion; confirm they fail.
3. **paths** — `BackupsDir()`.
4. **database** — `backup.go`: `Backup` (VACUUM INTO + 0600 tighten + 0750 dir for the default) and `IntegrityCheck` (result interpretation), both with the `ParseURL`→`pg_dump` guard.
5. **newer-database refusal** — `refuseNewerDatabase` inside `MigrateUp` (covers startup and `lightstack-migrate up`).
6. **CLI** — `backup`/`check` subcommands, handlers (default path via `paths.SetBase(cfg.StatePath)`), usage text.
7. **Green** — unit/DB tests green on SQLite and PostgreSQL; lint/fmt clean.
8. **Docs** — `KUBERNETES_DEPLOYMENT.md` and `DOCKER_DEPLOYMENT.md`: backup, restore (WAL-delete pitfall, node debug pod / compose run), recovery (`.recover` off-node).
9. **Host run** — on k3s real-mode SQLite: take a backup while serving, run the documented Kubernetes restore through a node debug pod, confirm the restored database serves requests (FR-013).

## Complexity Tracking

> No constitution deviations. This feature adds no abstraction, no dependency, no API change, no schema change, and no chart change; it consumes the SPEC-028 database seam. The newer-database refusal already existed; this feature only names its cause, inside `MigrateUp`. Table intentionally empty.
