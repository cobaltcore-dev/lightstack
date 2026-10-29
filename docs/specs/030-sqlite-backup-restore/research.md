# Research: SQLite backup, integrity check and restore

**Feature**: `030-sqlite-backup-restore` (cloud-storage#600) | **Date**: 2026-10-28
**Method**: read the SPEC-028/029 code (`internal/database`, `pkg/paths`, `cmd/lightstack-migrate`, `cmd/lightstack`, `internal/teardown`, `build/package/Dockerfile`, `charts/lightstack`), plus empirical SQLite checks with the host `sqlite3` 3.45.1 (behaviours below are SQLite-version-stable; modernc ships 3.53.4).

Each item: **Decision / Rationale / Alternatives**.

---

## R1 — Where the commands live and how the backend is chosen

**Decision**: Add `backup [<file>]` and `check` as subcommands of `lightstack-migrate` (`cmd/lightstack-migrate/main.go`). The command dispatch already resolves the backend at the top via `database.ParseURL(cfg.Database.URL)` and runs `CheckDeclaredBackend`; the two handlers call new `internal/database` functions that themselves re-check the backend.

**Rationale**: `lightstack-migrate` already owns the database URL, backend selection, config loading, and schema migration, and runs both in-cluster (`kubectl exec`) and standalone (`docker compose exec`, bare binary). The ticket names exactly these invocations. Keeping the backend guard inside the `internal/database` functions (not only in the CLI) makes the `pg_dump` refusal unit-testable without a live database.

**Alternatives**: a separate `lightstack-backup` binary (more image surface, another `CMD`); an in-server HTTP endpoint (crosses an API boundary, needs auth/scoping — rejected, backups are a node-admin operation, not a tenant API).

## R2 — Online backup: `VACUUM INTO` (consistent, single-file, no-overwrite)

**Decision**: `backup` opens its own must-exist SQLite handle to the live database and runs `VACUUM INTO <target>`. The result is one standalone file; `VACUUM INTO` refuses an existing target.

**Rationale (verified)**: with a WAL database carrying committed data,
- `VACUUM INTO 'backup1.db'` → exit 0, produces **one** file (`backup1.db`, no `-wal`/`-shm`);
- a second `VACUUM INTO 'backup1.db'` → `Error: stepping, output file already exists`, **exit 1**;
- the copy reads back the source's committed rows.

`VACUUM INTO` reads the source under a transaction and writes a fresh, defragmented database to the new file; in WAL mode a reader does not block writers and sees a consistent committed snapshot, so Lightstack keeps serving requests throughout (FR-001). `VACUUM INTO` is SQLite ≥ 3.27 (2019); modernc 3.53.4 has it. The no-overwrite refusal **is** FR-003 — no separate existence check is required, though the handler still reports it with a clear message naming the target.

**Alternatives**: copy the live file + `-wal`/`-shm` (can capture a torn state — the exact inconsistency the ticket warns against); the SQLite Online Backup API (not exposed by `database/sql`/modernc as a first-class call; `VACUUM INTO` is the supported pure-Go path); `.dump` to SQL text (slower, larger, not a ready-to-use database file).

## R3 — Default target path and name

**Decision**: With no `<file>`, write to `paths.BackupsDir()/lightstack-<UTC timestamp>.db`, where `BackupsDir()` is new = `filepath.Join(base, "backups")`. The `backup` handler must call `paths.SetBase(cfg.StatePath)` before computing it — the migrate CLI does **not** set the base today (only `cmd/lightstack/main.go:478` does). Timestamp format `20060102T150405Z` (UTC, basic ISO-8601), e.g. `lightstack-20261028T143000Z.db`.

**Rationale**: `cfg.StatePath` is resolved by `common.LoadConfig` and equals the chart's `realMode.hostStoragePath` (default `/var/lib/lightstack`), so in real mode `BackupsDir()` is under the already-mounted `host-storage` path and is node-persistent. A UTC timestamp gives collision-free names across backups; two backups within the same second collide and the second is refused by `VACUUM INTO` (R2) — acceptable, the operator reruns (FR-003 covers the refusal).

**Alternatives**: sub-second/counter-suffixed names (unneeded — the refusal is a safe guard, not a failure mode worth engineering around); a fixed `lightstack-backup.db` (would always collide after the first).

## R4 — Backup file and directory permissions

**Decision**: The default backups directory is created `0750`; every backup file is tightened to `0600` after `VACUUM INTO`. Reuse the existing `makeDatabaseDir` pattern (0750 dir, 0755 parents) for the directory; for the file, a direct `os.Chmod(dest, 0600)` — the backup has no `-wal`/`-shm`, so the `tightenSQLiteModes` three-file loop is unnecessary (a one-file chmod is clearer).

**Rationale**: FR-003a / spec Clarifications — a backup holds the same sensitive data as the live database (bcrypt password hashes, all tenant data), which is `0600` under `0750` (`sqliteFileMode = 0o600`, `makeDatabaseDir` = 0o750). Least privilege must not widen at the copy. Root (the chart's user) still reads `0600` for `kubectl cp` / `docker cp`.

**Alternatives**: world-readable backups for non-root off-node copy (rejected in clarify — root copies fine); leaving the mode to umask (non-deterministic, could be 0644).

## R5 — `VACUUM INTO` target argument

**Decision**: Pass the target as a single-quoted SQL string literal with embedded `'` doubled: `VACUUM INTO '<escaped>'`. Confirm at implementation whether modernc accepts a bound parameter (`VACUUM INTO ?`); prefer it if so, else the escaped literal.

**Rationale**: `VACUUM INTO` takes a text expression; the operator supplies the path, so it must not be interpolated raw. Doubling `'` is the minimal correct escaping for a SQLite string literal. The default path (timestamped, under the state root) contains no quotes; an explicit `<file>` could, so escaping is required regardless.

**Alternatives**: raw interpolation (unsafe); shelling out to the `sqlite3` CLI (the image ships none — R13).

## R6 — Integrity check: `PRAGMA integrity_check`

**Decision**: `check` opens a must-exist handle and runs `PRAGMA integrity_check`. Interpret the result: a single row equal to `ok` → success (exit 0); any other row(s) → print them and exit non-zero; if the statement itself returns an error (e.g. `SQLITE_CORRUPT`, "database disk image is malformed") → print it and exit non-zero. It is read-only and may run against a live, serving database with no downtime.

**Rationale (verified)**: on an intact database `PRAGMA integrity_check` returns the single row `ok`. On a corrupted one it either returns problem rows (e.g. `Tree 2 page 8: btreeInitPage() returns error code 11`) **or**, when corruption is severe, the statement fails outright with `database disk image is malformed (11)`. Crucially, the bare `sqlite3` CLI exits **0** even when it prints problem rows — so the non-zero exit is **our** code's job, derived from the result, not from a child process. `integrity_check` is a read; in WAL mode it does not block writers, so `check` is online (spec Clarifications, FR-005).

**Alternatives**: `PRAGMA quick_check` (faster, less thorough — the ticket specifies `integrity_check`); trusting a child `sqlite3` exit code (wrong — it is 0 on problem rows, and the image has no `sqlite3`).

## R7 — PostgreSQL guard (`pg_dump`)

**Decision**: Both `Backup` and `IntegrityCheck` begin by resolving the backend with `database.ParseURL`. On `Postgres` they return an error whose message names `pg_dump` ("… is only supported on SQLite; use pg_dump for PostgreSQL"); the CLI prints it and exits non-zero.

**Rationale**: FR-006 and the ticket's acceptance criterion require both commands to exit non-zero naming `pg_dump` on a PostgreSQL URL. Centralising the guard in the functions keeps the CLI handlers thin and lets a unit test assert the refusal with just a `postgres://` URL — no server needed. The message names `pg_dump` for both commands exactly as the ticket specifies (a single external tool the operator already uses for PostgreSQL), even though `pg_dump` is strictly a dump tool — this matches the ticket verbatim and is the operator's known entry point.

**Alternatives**: gating only in the CLI (not unit-testable without wiring); naming different tools per command (the ticket fixes `pg_dump` for both).

## R8 — Newer-version refusal: exists already; the message did not

**Correction (implementation, 2026-10-29)**: the planning version of this item claimed the refusal was missing, reasoning that `Up()` returns `ErrNoChange` on an ahead database. A probe disproved it: with a SQLite database forced to version 75 (binary's newest: 74), `MigrateUp` fails with `failed to run migrations: no migration found for version 75: read down for version 75 .: file does not exist` — golang-migrate looks the current version up in the source before reading forward, and startup treats the error as fatal. FR-008's refusal therefore already held.

**Decision**: keep the behaviour; replace the message. `MigrateUp` checks first, via `refuseNewerDatabase`, whether the recorded version exceeds the newest migration in the backend's view (`migrationView.latestVersion`), and refuses naming both versions and the cause ("written by a newer Lightstack"). Because the check sits inside `MigrateUp`, startup and `lightstack-migrate up` both get it with no wiring; the planned separate `RefuseVersionAhead` + call sites are not needed. A test pins the refusal so it cannot quietly disappear with a golang-migrate upgrade.

**Rationale**: the operator meeting this error has just restored a backup; "read down for version 75" points nowhere. Backend-neutral version arithmetic, so a PostgreSQL downgrade gets the same message.

**Alternatives**: leave golang-migrate's message (correct, unhelpful); a separate exported guard wired into two entry points (more surface for the same effect).

## R9 — Forward-migrating an older backup

**Decision**: No new code. Restoring an older backup and starting runs the existing startup `MigrateUp`, which applies the pending migrations forward.

**Rationale**: FR-008's first half is the normal migrate-on-start path (`cmd/lightstack/main.go` → `database.MigrateUp`), already proven by the SPEC-028 migration tests. An older SQLite backup sits at or above the generated baseline (073) and migrates forward like any under-version database; `RefuseBelowBaseline` already guards a below-baseline target.

**Alternatives**: none needed.

## R10 — Restore procedure and the WAL-replay pitfall

**Decision**: Document: stop Lightstack; delete `lightstack.db-wal` and `lightstack.db-shm`; put the backup in place as `lightstack.db`; run `lightstack-migrate check`; start. Call out deleting the side files as **mandatory**.

**Rationale (verified)**: a leftover `-wal` is replayed onto whatever file sits at `lightstack.db` the next time a connection opens, reinstating post-backup data onto the restored snapshot — the ticket's tested failure ("the 'restored' database returned the post-backup data"). The correct sequence (remove `-wal`/`-shm`, then swap in the backup) reads back the backup's data. A backup produced by `VACUUM INTO` has no side files of its own (R2), so only the *old* database's side files are the hazard.

**Alternatives**: checkpoint-and-copy the live database instead of restoring a backup (not a restore); leaving the side files and trusting a checkpoint (unsafe — the replay is the whole bug).

## R11 — Kubernetes restore via a node debug pod

**Decision**: Document the k3s restore as: `kubectl scale deployment … --replicas=0`; `kubectl debug node/<node> -it --image=<lightstack image>` (host filesystem under `/host`); delete `-wal`/`-shm` and copy the backup to `/host<hostStoragePath>/db/lightstack.db`; run `/app/lightstack-migrate check` with `LIGHTSTACK_DB_URL=sqlite:///host<hostStoragePath>/db/lightstack.db`; delete the debug pod; `kubectl scale … --replicas=<n>`. Backups are taken with `kubectl exec <pod> -- /app/lightstack-migrate backup …` then `kubectl cp` from `<hostStoragePath>/backups/`.

**Rationale**: the chart runs `/app/lightstack` directly (`_helpers.tpl` container command `["/app/lightstack", "--config", …]`), so there is no shell in the running container, and a scaled-down Deployment leaves no pod mounting the node storage — the file surgery cannot be done via `kubectl exec`. A node debug pod mounts the host filesystem under `/host`, giving direct access to `<hostStoragePath>` on the node. The debug pod uses the Lightstack image so `/app/lightstack-migrate check` is present. Backing up, by contrast, happens while the service runs, so it is a plain `kubectl exec` into the live pod.

**Alternatives**: a sidecar/Job mounting the hostPath (more chart surface for a rare operation — out of scope); `kubectl cp` into a stopped pod (there is none when scaled to 0).

## R12 — Docker Compose restore

**Decision**: Document: `docker compose stop lightstack`; delete `-wal`/`-shm`, place the backup, and run `check` inside `docker compose run --rm --entrypoint sh lightstack`; `docker compose start lightstack`. Backups: `docker compose exec lightstack /app/lightstack-migrate backup …`.

**Rationale**: Compose has a shell available via `run --entrypoint sh`, and the database directory is a bind/named volume shared with the service, so the one-off container sees the same files. Stopping the service first avoids a second writer during the swap.

**Alternatives**: editing files on the host volume directly (works for bind mounts, not named volumes; the run-container form covers both).

## R13 — Recovery beyond the latest backup

**Decision**: Document recovery as: restore the latest good backup (R10/R11/R12); for damage beyond that, salvage with the `sqlite3` CLI `.recover` run **off the node** (on a copy of the damaged file), because the Lightstack image ships no `sqlite3`.

**Rationale (verified)**: `build/package/Dockerfile` installs `qemu-img` and `cdrkit` (genisoimage) but **no** `sqlite3`, and the binary is `CGO_ENABLED=0` pure Go — there is no SQLite shell in the image. `.recover` is the standard best-effort salvage for a corrupt SQLite database and runs anywhere the operator has the `sqlite3` CLI. Recovery is a rare, best-effort path, so leaning on external tooling (rather than shipping `sqlite3`) keeps the image small (Article VI).

**Alternatives**: ship `sqlite3` in the image (image bloat for a rare operation); a built-in `recover` subcommand wrapping the C `.recover` logic (large, and modernc does not expose it — out of scope). Not every `sqlite3` can run `.recover` — see R15.

## R15 — Host-run findings (2026-10-29)

**Empty file passes `check`.** An empty file is a valid, empty SQLite database; `integrity_check` answers `ok`. The host run's failed `.recover` left a 0-byte `recovered.db`, and `check` passed it — the restore gate would have let Lightstack start as a fresh install. **Decision**: `check` also reads the migration version from `schema_migrations` and refuses a database without one (no table, or no row), and reports the version on success (FR-005a). The table's presence is read from `sqlite_schema`, not inferred from driver error text.

**Ubuntu's `sqlite3` cannot run `.recover`.** Ubuntu 24.04's `sqlite3` 3.45.1 answers `.recover` with `sql error: no such table: sqlite_dbpage (1)`: the distribution builds the shell without the `sqlite_dbpage` virtual table. Alpine's `sqlite` 3.53.4 (in `alpine:3.24`, the image's base) runs it, and salvaged the run's three-page-damaged copy into a 1.4 MB database with `schema_migrations` at 74 that passes `integrity_check`. **Decision**: the recovery docs run `.recover` in an Alpine container.

## R14 — Scope boundaries (no chart change, no migration, no new modules)

**Decision**: This feature touches Go (`cmd/lightstack-migrate`, `internal/database`, `pkg/paths`, a startup wire in `cmd/lightstack`), a teardown **test**, and docs. It changes **no** chart template or `values.*`, adds **no** migration or schema change, and adds **no** Go module. Stub-mode deployments are not backed up (FR-004a).

**Rationale**: in real mode `BackupsDir()` (= `<hostStoragePath>/backups`) is already under the mounted `host-storage` volume, sits outside `db/`, and is already absent from `purgeLocalStorage` (`internal/teardown/teardown.go` lists `images/ volumes/ instances/ cloud-init/ dhcp/ console-tokens/ console-sockets/ db/` — not `backups/`), so `dataRetentionPolicy: delete` keeps backups with no chart or teardown code change; a unit test locks the exclusion (FR-004). Pre-creating `backups/` in the chart's `init-host-storage` mkdir list would create it `0755`, conflicting with FR-003a's `0750`, so the backup command creates it itself. Stub mode mounts only `db/` as a hostPath (SPEC-029), so `<hostStoragePath>/backups` would not be node-persistent there — hence FR-004a scopes backups to real mode and no stub-mode mount is added.

**Alternatives**: add a `backups` entry to the chart mkdir list (mode conflict, and unnecessary); a stub-mode backups mount (out of scope per FR-004a).

---

## Summary of decisions

| # | Decision | Net-new code? |
|---|----------|---------------|
| R1 | `backup`/`check` as `lightstack-migrate` subcommands; backend guard inside the functions | yes (CLI) |
| R2 | Online, consistent, single-file backup via `VACUUM INTO`; no-overwrite is the refusal | yes |
| R3 | Default `BackupsDir()/lightstack-<UTC>.db`; CLI sets `paths.SetBase(cfg.StatePath)` | yes (`BackupsDir`, SetBase wire) |
| R4 | Backup file `0600`, backups dir `0750`, root-owned | yes (chmod/mkdir) |
| R5 | `VACUUM INTO` target as an escaped literal (or bound param if accepted) | yes |
| R6 | `check` = `PRAGMA integrity_check`; our code maps result → exit; online | yes |
| R7 | PostgreSQL → both refuse naming `pg_dump` | yes |
| R8 | Newer-database refusal already exists; `MigrateUp` now names the cause (research corrected) | yes (message) |
| R9 | Forward-migrate older backup = existing `MigrateUp` | no |
| R10 | Restore procedure + mandatory `-wal`/`-shm` delete | docs |
| R11 | Kubernetes restore via node debug pod; backup via exec + `kubectl cp` | docs |
| R12 | Docker Compose restore via `run --rm --entrypoint sh`; backup via exec | docs |
| R13 | Recovery via `sqlite3 .recover` off-node; image ships no `sqlite3` | docs |
| R14 | No chart change, no migration, no new modules; stub mode not backed up | no (test only) |

No `NEEDS CLARIFICATION` remains.
