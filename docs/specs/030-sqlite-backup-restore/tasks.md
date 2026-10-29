# Tasks: SQLite backup, integrity check and restore

**Feature**: `030-sqlite-backup-restore` (cloud-storage#600, part 3 of #595)
**Input**: Design documents from `docs/specs/030-sqlite-backup-restore/`
**Prerequisites**: plan.md, spec.md (required); research.md, data-model.md, contracts/, quickstart.md (present)

**Tests**: INCLUDED. The Lightstack constitution makes Test-First development NON-NEGOTIABLE (Article III) and the plan's delivery sequence puts RED tests before code. Every behaviour task below has a failing test written first.

**Organization**: Grouped by the spec's user stories (US1 P1 … US5 P3), in priority order, each independently testable.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different file, no dependency on an incomplete task)
- **[Story]**: US1–US5 for story-phase tasks; Setup/Foundational/Polish carry no story label
- Every task names an exact file path

## Path conventions

Single Go project at repo root: `cmd/`, `internal/`, `pkg/`, `test/`, `docs/`. No `src/` layout (plan.md § Project Structure).

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Establish the green baseline the "no regression / PostgreSQL unchanged" claims are measured against.

- [X] T001 Record baseline gates on the branch: `go test ./...`, `make lint`, `make fmt` clean, and both binaries build (`go build ./cmd/lightstack` and `go build ./cmd/lightstack-migrate`). Capture output as the oracle; fix any pre-existing breakage before proceeding.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The shared file and the SQLite-only backend guard that both `Backup` (US1) and `IntegrityCheck` (US3) call, and the `pg_dump` refusal (US4) rides on.

**⚠️ CRITICAL**: No user story code can begin until this is in place.

- [X] T002 Create `internal/database/backup.go` with an unexported backend-guard helper that runs `database.ParseURL(dbURL)` and, for a `Postgres` URL, returns an error whose message names `pg_dump` ("… is only supported on SQLite; use pg_dump for PostgreSQL"). Both `Backup` and `IntegrityCheck` will call it first. No other logic yet.

**Checkpoint**: Shared seam ready — user stories can proceed.

---

## Phase 3: User Story 1 — Take a consistent backup while the service keeps running (Priority: P1) 🎯 MVP

**Goal**: `lightstack-migrate backup [<file>]` writes one consistent, standalone SQLite copy while Lightstack serves requests, to a timestamped default location, never overwriting an existing target, at `0600` under a `0750` directory.

**Independent Test**: Run `backup` against a running SQLite deployment; confirm a single file (no `-wal`/`-shm`), service unaffected, modes `0600`/`0750`, and a second backup to the same path refused.

### Tests for User Story 1 (write first, confirm RED)

- [X] T003 [P] [US1] Unit test for `paths.BackupsDir()` (= `<base>/backups`, sibling of `DBDir()`) in `pkg/paths/paths_test.go`.
- [X] T004 [US1] DB-backed test in `internal/database/backup_test.go`: with a SQLite database open and being written to, `database.Backup` writes a single file (assert no `<target>-wal`/`-shm`), the copy opens and reads back the committed rows, and the service handle keeps working — proves "consistent while serving" (FR-001, SC-001).
- [X] T005 [US1] Unit test in `internal/database/backup_test.go`: `database.Backup` to an existing target returns a non-nil error naming the target and writes nothing (FR-003).
- [X] T006 [US1] Unit test in `internal/database/backup_test.go`: the default target resolves under `paths.BackupsDir()` as `lightstack-<UTC timestamp>.db`; after a successful backup the directory is mode `0750` and the file mode `0600` (FR-002, FR-003a).

### Implementation for User Story 1

- [X] T007 [P] [US1] Add `BackupsDir()` to `pkg/paths/paths.go`: `return filepath.Join(base, "backups")`, with a doc note that it is a sibling of `DBDir()`, outside `db/`, and operator-owned (kept by `dataRetentionPolicy: delete`).
- [X] T008 [US1] Implement `Backup(ctx, dbURL, dest string) (string, error)` in `internal/database/backup.go`: call the backend guard (T002); resolve/escape the target (single-quote-doubled literal, or a bound parameter if modernc accepts it — research R5); for the default path create `BackupsDir()` at `0750` (reuse the `makeDatabaseDir` 0750 pattern); open a must-exist handle (`openSQLiteHandle(..., mustExist=true)`); run `VACUUM INTO <target>`; `os.Chmod(dest, 0o600)`; return the final path. Surface the existing-target error unchanged. (depends T002, T007)
- [X] T009 [US1] Add the `backup [<file>]` subcommand to `cmd/lightstack-migrate/main.go`: a `handleBackup` that calls `paths.SetBase(cfg.StatePath)`, computes the default target (`paths.BackupsDir()/lightstack-<UTC>.db`) when no arg, invokes `database.Backup`, prints the final path on success, and exits non-zero with the error message on failure; add it to the `switch` and to `printUsage()`. (depends T008)

**Checkpoint**: MVP — an operator can take a consistent, no-overwrite, correctly-permissioned backup while serving.

---

## Phase 4: User Story 2 — Restore from a backup and come back up cleanly (Priority: P1)

**Goal**: A documented restore that brings Lightstack back on a backup; an older backup migrates forward on start, a newer one is refused; the WAL-replay pitfall is prevented.

**Independent Test**: On a database that left `-wal`/`-shm` behind, follow the documented restore and confirm the service starts, passes `check`, and reads back the backup's data; and a database forced ahead of the binary refuses to start.

### Tests for User Story 2 (write first, confirm RED)

- [X] T010 [US2] Unit test in `internal/database/migrate_test.go`: `RefuseVersionAhead` returns an error when the database version (set via `ForceMigrationVersion`) is one above the highest migration the binary knows, naming both versions; returns nil at or below it (FR-008). SQLite at minimum; note it is backend-neutral.
- [X] T011 [US2] Restore round-trip test `TestRestoreProcedure` in `internal/database/backup_test.go` (a Go test, not a shell script: the stale write-ahead log an unclean stop leaves is reproducible in-process, with no `sqlite3` on CI; the real binaries are exercised by the T025 host run): create a SQLite DB with data; `Backup` it; write more data and keep a copy of the `-wal` that holds it; show that swapping the backup in **without** removing the side files reinstates post-backup data; then the correct order (remove side files, swap, `IntegrityCheck`, open) reads back the backup's data (SC-002, FR-007).

### Implementation for User Story 2

- [X] T012 [US2] Name the newer-database refusal in `internal/database/migrate.go`: `MigrateUp` calls `refuseNewerDatabase` first, which compares `m.Version()` with the newest version in the backend's migration view (`migrationView.latestVersion` in `migrate_view.go`) and refuses naming both versions and the cause. (Corrected: golang-migrate already refuses such a database, cryptically — research R8.)
- [X] T013 [US2] No wiring needed: the check lives inside `MigrateUp`, which both `cmd/lightstack/main.go` (startup) and `lightstack-migrate up` call. Confirm both paths surface the message. (depends T012)
- [X] T014 [US2] Document the restore procedure in `docs/KUBERNETES_DEPLOYMENT.md` and `docs/DOCKER_DEPLOYMENT.md` per `contracts/restore-recovery.md`: stop; delete `lightstack.db-wal`/`-shm` (mandatory, with the WAL-replay reason); swap in the backup; `lightstack-migrate check`; start; note older→forward-migrate, newer→refused. Kubernetes via a `kubectl debug node/<node>` pod (host fs under `/host`, Deployment scaled to 0, `LIGHTSTACK_DB_URL=sqlite:///host…`); Docker Compose via `docker compose run --rm --entrypoint sh`. Open the backup/restore/recovery documentation (covering T014/T021/T023) with a one-line scope statement: these procedures are for **real-mode SQLite** deployments; stub-mode deployments are not backed up (FR-004a). (depends T018 for the `check` reference)

**Checkpoint**: A backup can be restored safely on both platforms; version skew is handled.

---

## Phase 5: User Story 3 — Verify a database's integrity (Priority: P2)

**Goal**: `lightstack-migrate check` reports integrity — exit 0 intact, non-zero (listing problems) damaged — and runs online against a live database.

**Independent Test**: `check` an intact database (exit 0, no downtime while the server runs) and a deliberately damaged one (non-zero, problems listed).

### Tests for User Story 3 (write first, confirm RED)

- [X] T015 [US3] Test in `internal/database/backup_test.go`: `IntegrityCheck` returns nil for an intact database; returns a non-nil error listing problems for a database corrupted by overwriting interior pages; and returns a non-nil error when the statement itself fails with `SQLITE_CORRUPT` ("database disk image is malformed") — the two failure shapes from research R6 (FR-005).
- [X] T016 [US3] DB-backed test in `internal/database/backup_test.go`: `IntegrityCheck` succeeds against a database that is open and being written to by another handle — proves it is online (FR-005, Clarifications). (same file as T015 — sequential)

### Implementation for User Story 3

- [X] T017 [US3] Implement `IntegrityCheck(ctx, dbURL string) error` in `internal/database/backup.go`: call the backend guard (T002); open a must-exist handle; `Query` `PRAGMA integrity_check`; a single `ok` row → nil; any other rows → an error listing them; a query/scan error (e.g. `SQLITE_CORRUPT`) → that error. (depends T002)
- [X] T018 [US3] Add the `check` subcommand to `cmd/lightstack-migrate/main.go`: a `handleCheck` that calls `database.IntegrityCheck`, prints success or the listed problems, and exits 0/non-zero; add it to the `switch` and `printUsage()`. (depends T017)

**Checkpoint**: Integrity is verifiable live and is the gate used by the restore docs.

---

## Phase 6: User Story 4 — Clear SQLite-only refusal on PostgreSQL (Priority: P2)

**Goal**: `backup` and `check` on a PostgreSQL URL exit non-zero naming `pg_dump`.

**Independent Test**: Point the config/`LIGHTSTACK_DB_URL` at a `postgres://` URL; run each command; confirm non-zero exit and a message naming `pg_dump`.

### Tests for User Story 4 (write first, confirm RED)

- [X] T019 [US4] Unit test in `internal/database/backup_test.go`: `Backup` and `IntegrityCheck` on a `postgres://…` URL each return a non-nil error whose message contains `pg_dump`, and take no filesystem action (no server needed) (FR-006).

### Implementation for User Story 4

- [X] T020 [US4] Confirm both CLI handlers (`handleBackup`, `handleCheck` in `cmd/lightstack-migrate/main.go`) surface the backend-guard error to a non-zero exit with the message intact; adjust the wording of the guard in `internal/database/backup.go` (T002) if the test shows the `pg_dump` token is missing or unclear. (the guard code exists from T002/T008/T017; this closes the message/exit contract)

**Checkpoint**: PostgreSQL operators are told exactly what to use; no misleading partial result.

---

## Phase 7: User Story 5 — Recovery beyond the latest backup (Priority: P3)

**Goal**: Documented recovery guidance for damage beyond the latest backup.

**Independent Test**: The docs direct the operator to restore the latest backup and, beyond that, to salvage with `sqlite3 .recover` off the node.

### Implementation for User Story 5

- [X] T021 [US5] Add a recovery section to `docs/KUBERNETES_DEPLOYMENT.md` and `docs/DOCKER_DEPLOYMENT.md` per `contracts/restore-recovery.md`: restore the latest backup (reference the restore procedure); for damage beyond it, salvage with the `sqlite3` CLI `.recover` run **off the node** on a copy (the Lightstack image ships no `sqlite3` — research R13), then restore the recovered file.

**Checkpoint**: A documented safety net exists for corruption beyond the latest backup.

---

## Phase 8: Polish & Cross-Cutting Concerns

- [X] T022 [P] Add a test to `internal/teardown/teardown_test.go` asserting the purge list does **not** include `paths.BackupsDir()` (backups survive `dataRetentionPolicy: delete`) — guards FR-004. `teardown.go` itself is unchanged.
- [X] T023 Add the backup-taking docs to `docs/KUBERNETES_DEPLOYMENT.md` and `docs/DOCKER_DEPLOYMENT.md` (FR-011): Kubernetes `kubectl exec <pod> -- /app/lightstack-migrate backup --config /app/config/lightstack.yaml` then `kubectl cp` from `<hostStoragePath>/backups/`; Docker Compose `docker compose exec lightstack /app/lightstack-migrate backup`. (same two files as T014/T021 — sequence after them)
- [X] T024 Run the full gate on both backends: `make fmt`, `make lint`, and `go test ./...` with `LIGHTSTACK_TEST_DB_URL` set to the SQLite form and the PostgreSQL form (CLAUDE.md). All green.
- [X] T025 Host validation on k3s real-mode SQLite (FR-013): take a backup while the service serves; run the documented Kubernetes restore via a node debug pod; confirm the restored database serves requests. Reconcile `quickstart.md`/`contracts/restore-recovery.md` with any step that drifted.

- [X] T026 [US3] Host-run finding: `IntegrityCheck` in `internal/database/backup.go` also requires a recorded schema version (`schema_migrations`) and returns it; refuse an empty or foreign database (FR-005a). Test `TestIntegrityCheckRefusesANonLightstackDatabase`; CLI prints `(schema version N)`.
- [X] T027 [US5] Host-run finding: recovery docs (`docs/KUBERNETES_DEPLOYMENT.md`, `docs/DOCKER_DEPLOYMENT.md`, `contracts/restore-recovery.md`) run `.recover` with Alpine's `sqlite3` in a container — Debian/Ubuntu builds lack `sqlite_dbpage` (research R15); `HOST_TESTING.md` stage 2.6 likewise, plus an empty-file refusal check.

---

## Dependencies & Execution Order

- **Setup (T001)** → **Foundational (T002)** → user stories.
- **US1 (T003–T009)**: MVP. T007 ∥ tests; T008 needs T002+T007; T009 needs T008.
- **US2 (T010–T014)**: T012 needs no story code; T013 needs T012; T011 (restore round-trip) exercises `backup` (US1) and `check` (US3), so run it after T009 and T018; T014 references `check` (T018).
- **US3 (T015–T018)**: T017 needs T002; T018 needs T017. Independent of US1/US2 except the shared file T002.
- **US4 (T019–T020)**: needs `Backup` (T008) and `IntegrityCheck` (T017) present.
- **US5 (T021)**: docs only; after T014 so the restore reference exists.
- **Polish (T022–T025)**: T022 ∥ anytime after T007; T023 after T014/T021 (same files); T024 after all code; T025 last (needs a built image + k3s).

**Story completion order**: US1 → US3 → US2 → US4 → US5 (US3's `check` is a dependency of US2's restore test and docs, so land US3 before finishing US2), then polish.

## Parallel opportunities

- T003 (paths test) ∥ T002 (backend guard) — different files.
- T007 (`BackupsDir`) ∥ the US1 test tasks that target `backup_test.go`.
- T022 (teardown test) runs in parallel with most of the feature.
- Within a single file (`internal/database/backup.go`, `cmd/lightstack-migrate/main.go`, the two deployment docs) tasks are **sequential** — no `[P]`.

## Implementation strategy

- **MVP = US1** (T001–T009): a consistent, no-overwrite, correctly-permissioned online backup. Shippable and demonstrable on its own.
- **Increment 2 = US3** (T015–T018): the integrity check, needed as the restore gate.
- **Increment 3 = US2** (T010–T014): the documented restore + the version-ceiling guard — the backup becomes recoverable.
- **Increment 4 = US4** (T019–T020): the PostgreSQL guardrail.
- **Increment 5 = US5 + Polish** (T021–T025): recovery docs, retention test, backup docs, both-backend gate, k3s host run.

Total: **25 tasks** across 5 user stories + setup/foundational/polish.
