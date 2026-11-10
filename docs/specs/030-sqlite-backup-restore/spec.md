# Feature Specification: SQLite backup, integrity check and restore

**Feature Branch**: `030-sqlite-backup-restore`
**Created**: 2026-10-28
**Status**: Draft
**Input**: GitHub issue [cobaltcore-dev/cloud-storage#600](https://github.com/cobaltcore-dev/cloud-storage/issues/600) — "[FEAT] - lightstack - Database - SQLite 3/3: backup, integrity check and restore"

## Context

Part 3 of 3 of the SQLite work (parent #595). Parts 1 (the SQLite backend, #598) and 2
(Helm chart and Docker Compose, #599) already landed. SQLite is now a supported
production backend running from a single file on the node. An operator who runs
PostgreSQL has the backend's own tooling for a consistent backup, an integrity check and
a documented restore; an operator who runs SQLite has none. Today Lightstack ships no
backup support for either backend — the only trace is a few `pg_dump` lines in the Docker
docs. This feature closes that gap for SQLite: an online backup, an integrity check, and
a documented restore and recovery, so an operator who chose SQLite can protect and
recover their data.

## Clarifications

### Session 2026-10-28

- Q: What file mode should backup files and the backups directory have? → A: Mirror the live database — backup files `0600`, backups directory `0750`, owned by the process user (root in the chart).
- Q: Is `check` an online operation (live, serving database) or offline only (service stopped)? → A: Online — it is a read-only scan that may run with no downtime, serving as both a routine health check and the post-restore gate.
- Q: Should stub-mode deployments be backed up (the backups location is node-persistent only in real mode)? → A: No — backups are a real-mode operation; stub mode is a dev/test configuration and is not backed up, so the chart adds no stub-mode backups storage.

### Session 2026-10-29 (host validation)

- Q: An empty file passes SQLite's integrity check, so a failed `.recover` or a broken copy would pass the restore gate and Lightstack would start as a fresh install — should `check` accept it? → A: No. `check` also requires the migration version a Lightstack schema records, refuses a database without one, and reports the version on success (FR-005a).
- Q: The host run found that the `sqlite3` of Debian/Ubuntu cannot run `.recover` (`no such table: sqlite_dbpage`). → A: The recovery documentation runs `.recover` with Alpine's `sqlite3` in a container, verified to salvage a damaged Lightstack database (FR-012).

### Session 2026-11-05 (review)

- Q: A partial recovery can keep the migration version and the core tables while losing other tables. `check` passed such a file, and startup took its version as current, so the lost tables were never recreated and Lightstack failed later. Should `check` hold a database to its whole schema? → A: Yes. `check` compares the file with the schema its recorded version requires (the migrations applied up to that version) and refuses any missing table, index, view, trigger or column, listing them. A version newer than the migrations is refused as at startup (FR-005a).
- Q: An interrupted migration leaves its version marked dirty, which startup refuses ("Dirty database version N"), yet `check` read only the version and passed such a file. Should `check` refuse it? → A: Yes, naming the version and the repair, and before comparing schemas: on SQLite the failed migration rolls back, so the schema under a dirty version is an earlier one's, and the cause is the interrupted migration, not missing tables (FR-005a).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Take a consistent backup while the service keeps running (Priority: P1)

An operator running Lightstack on SQLite wants a trustworthy backup without taking the
service offline. They run a single backup command; Lightstack keeps serving requests; the
result is one standalone database file they can copy away and later restore.

**Why this priority**: A backup is the whole point of the feature and the foundation for
restore and recovery. Copying the live database file by hand can capture an inconsistent
state (the write-ahead log is separate), so an online, consistent backup command is the
first thing an operator needs.

**Independent Test**: Run the backup command against a running SQLite deployment, confirm
the service answered requests throughout, and confirm a single, self-contained database
file was produced with no side files.

**Acceptance Scenarios**:

1. **Given** Lightstack is running on SQLite and serving requests, **When** the operator
   runs the backup command, **Then** a single consistent database copy is written (no
   companion write-ahead-log or shared-memory side files) and the service continues to
   answer requests throughout.
2. **Given** no destination is supplied, **When** the operator runs the backup command,
   **Then** the backup is written to a default, operator-owned backups location on the
   node under a name that includes a UTC timestamp, so repeated backups never collide.
3. **Given** the chosen destination file already exists, **When** the operator runs the
   backup command, **Then** it refuses to overwrite it and exits with a non-zero status
   and a message naming the destination.
4. **Given** a deployment configured to purge node-local data on teardown, **When** the
   retention policy runs, **Then** the backups location is left untouched (backups are
   operator-owned, like a retained database).

---

### User Story 2 - Restore from a backup and come back up cleanly (Priority: P1)

After a bad change, disk loss, or an accidental deletion, an operator wants to bring
Lightstack back on a known-good backup by following a documented procedure, and have the
service come up serving the backup's data.

**Why this priority**: A backup that cannot be restored has no value. The restore has a
sharp, easy-to-miss pitfall — a leftover write-ahead log is replayed onto the restored
file and silently reintroduces post-backup data — so the procedure must be documented and
proven, for both Kubernetes and Docker Compose.

**Independent Test**: On a database that left write-ahead-log and shared-memory side files
behind, follow the documented restore, then confirm the service starts, passes the
integrity check, and reads back the backup's data (not the newer live data).

**Acceptance Scenarios**:

1. **Given** a valid backup and a stopped Lightstack, **When** the operator follows the
   documented restore (stop the service, remove the write-ahead-log and shared-memory
   side files, put the backup in place as the live database, run the integrity check,
   start the service), **Then** Lightstack starts and serves the backup's data.
2. **Given** the original database left write-ahead-log and shared-memory side files
   behind, **When** the restore is performed without removing them, **Then** the stale log
   is replayed and the restored database wrongly shows post-backup data — the documented
   procedure removes those files to prevent exactly this.
3. **Given** a backup taken from an older Lightstack version, **When** the restored service
   starts, **Then** the schema is migrated forward automatically and the service comes up.
4. **Given** a backup taken from a newer Lightstack version than the one restoring it,
   **When** the service starts, **Then** it refuses the database rather than operating on a
   schema it does not understand.
5. **Given** a Kubernetes (k3s) deployment, **When** the operator follows the documented
   Kubernetes restore procedure, **Then** the restored database serves requests.
6. **Given** a Docker Compose deployment, **When** the operator follows the documented
   Compose restore procedure, **Then** the restored database serves requests.

---

### User Story 3 - Verify a database's integrity (Priority: P2)

An operator wants to confirm a database — a live one, or a just-restored one — is intact,
and get a clear, scriptable yes/no answer.

**Why this priority**: The integrity check is the gate in the restore procedure and a
standalone health tool. It is small and supports Story 2, so it ranks just below the core
backup/restore pair.

**Independent Test**: Run the check against an intact database and confirm a success exit;
run it against a deliberately damaged database and confirm a non-zero exit that lists the
problems.

**Acceptance Scenarios**:

1. **Given** an intact SQLite database, **When** the operator runs the check, **Then** it
   exits with a success status.
2. **Given** a damaged SQLite database, **When** the operator runs the check, **Then** it
   exits non-zero and lists the problems found.
3. **Given** Lightstack is running and serving requests, **When** the operator runs the
   check, **Then** it completes against the live database with no downtime and the service
   keeps answering requests throughout.

---

### User Story 4 - Be told clearly that this is SQLite-only on a PostgreSQL deployment (Priority: P2)

An operator running the PostgreSQL backend who tries the new backup or check commands
should be told immediately that these are SQLite-only and pointed at the right tool,
rather than getting a confusing failure or a misleading partial result.

**Why this priority**: A guardrail that prevents a dangerous misunderstanding (believing a
PostgreSQL database was backed up when it was not). Cheap, and it protects data.

**Independent Test**: Point the configuration at a PostgreSQL database URL, run backup and
run check, and confirm each exits non-zero with a message naming the PostgreSQL tool to
use instead.

**Acceptance Scenarios**:

1. **Given** the database URL selects PostgreSQL, **When** the operator runs the backup
   command, **Then** it exits non-zero with a message naming `pg_dump`.
2. **Given** the database URL selects PostgreSQL, **When** the operator runs the check
   command, **Then** it exits non-zero with a message naming `pg_dump`.

---

### User Story 5 - Recover when the latest backup is not enough (Priority: P3)

When damage goes beyond what the latest backup covers, an operator wants documented
guidance to salvage as much data as possible.

**Why this priority**: A safety net below normal restore. Needed less often, and it leans
on an external tool rather than new product behaviour, so it ranks last.

**Independent Test**: Follow the documented recovery guidance on a corrupted database and
confirm it describes restoring the latest backup and, beyond that, salvaging with the
standard SQLite recovery tooling run off the node.

**Acceptance Scenarios**:

1. **Given** a corrupted database, **When** the operator consults the recovery
   documentation, **Then** it directs them to restore the latest backup, and for damage
   beyond that, to salvage with the standard SQLite recovery tooling run off the node
   (the Lightstack image ships no such tool).

---

### Edge Cases

- **Destination already present**: backup never overwrites; the operator always gets a new
  file name, and a collision is a clean non-zero refusal, not a partial write.
- **Default backups directory missing**: the first backup creates the operator-owned
  backups directory on the node.
- **Leftover write-ahead log on restore**: the single most dangerous mistake; the
  documented procedure removes the side files, and the acceptance test proves the omission
  reintroduces stale data.
- **Version skew on restore**: an older backup migrates forward on start; a backup from a
  newer Lightstack is refused rather than silently operated on.
- **Wrong backend**: backup and check on a PostgreSQL URL refuse and name the PostgreSQL
  tool, instead of appearing to succeed.
- **No in-container shell/tooling on Kubernetes**: the live container runs the binary
  directly and a scaled-down Deployment leaves nothing mounting the node storage, so the
  restore procedure uses a node debug pod rather than an exec into a running container.
- **Damage beyond the latest backup**: recovery is documented as a separate, best-effort
  path using external tooling off the node. A distribution `sqlite3` may lack `.recover`; a
  salvage that produced an empty file, or one that lost tables, is refused by `check` (FR-005a)
  instead of being restored.
- **Empty or foreign file put in place**: `check` refuses it, so the restore stops before
  Lightstack would start as a fresh install.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The migration tool MUST provide a `backup` command that produces a single,
  consistent, standalone copy of the live SQLite database — with no companion
  write-ahead-log or shared-memory side files — while Lightstack continues to serve
  requests.
- **FR-002**: When no destination is given, `backup` MUST write to a default
  operator-owned backups location on the node (a `backups` directory under the node's
  storage root), using a file name that includes a UTC timestamp so repeated backups do
  not collide.
- **FR-003**: `backup` MUST refuse to overwrite an existing destination file, exiting
  non-zero with a message that names the destination.
- **FR-003a**: Backup files and the default backups directory MUST carry the same
  least-privilege permissions as the live database — backup files `0600`, the backups
  directory `0750`, owned by the process user (root in the chart) — so a backup never
  widens access to the sensitive data it copies.
- **FR-004**: The backups location MUST be operator-owned and survive teardown/retention —
  it lies outside the node-local storage trees that a `delete` retention policy purges, so
  backups are kept exactly as a retained database would be.
- **FR-004a**: Backups are a real-mode operation. In real mode the backups location sits
  within the node-persistent host storage that is mounted at the storage root. Stub-mode
  deployments are a dev/test configuration and are NOT backed up: the chart provisions no
  stub-mode backups storage, and the backup/restore documentation targets real-mode
  deployments.
- **FR-005**: The migration tool MUST provide a `check` command that reports database
  integrity: exit success for an intact SQLite database, and exit non-zero listing the
  problems for a damaged one. `check` is a read-only operation that MUST be runnable
  against a live, serving database with no downtime, so it serves both as a routine
  health check and as the post-restore gate.
- **FR-005a**: `check` MUST also refuse a database that holds no Lightstack schema — an empty
  file, another program's database, or one without a recorded migration version — exiting
  non-zero with a message saying so, and MUST report the schema version when it passes. An
  empty file is a valid SQLite database, and restoring one would start Lightstack as a fresh
  install. `check` MUST likewise refuse a database that lacks any table, index, view, trigger
  or column the schema of its recorded version requires — what a partial recovery can leave —
  listing what is missing: such a file passes SQLite's own check, and its version tells the
  startup migration nothing is missing. The required schema is what the migrations build up
  to that version, so an older backup is held to its own version; a version newer than the
  migrations is refused as FR-008 refuses it at startup. A version marked dirty by an
  interrupted migration MUST be refused as well, naming the version and how to repair it:
  startup refuses such a database too.
- **FR-006**: On a database URL that selects PostgreSQL, both `backup` and `check` MUST
  exit non-zero with a message naming `pg_dump` — these commands are SQLite-only, and the
  operator is pointed at the backend's own tooling.
- **FR-007**: The documentation MUST describe the restore procedure: stop Lightstack,
  remove the `-wal` and `-shm` side files, put the backup in place as the live database,
  run `check`, then start. Removing the side files MUST be called out as mandatory, because
  a left-behind write-ahead log is replayed onto the restored file and reintroduces
  post-backup data.
- **FR-008**: On start after a restore, Lightstack MUST migrate an older backup's schema
  forward automatically, and MUST refuse a backup taken from a newer Lightstack version
  rather than operate on a schema it does not understand.
- **FR-009**: The documentation MUST give the Kubernetes (k3s) restore procedure using a
  node debug pod — scale the Deployment to zero, run a debug pod on the node with the host
  filesystem mounted, remove the side files and place the backup under the node's database
  directory, run `check` against that host path, remove the debug pod, and scale back up —
  because the running container runs the binary directly and a scaled-down Deployment
  leaves nothing mounting the node storage.
- **FR-010**: The documentation MUST give the Docker Compose restore procedure — stop the
  Lightstack service, perform the same file steps and `check` in a one-off run container,
  then start the service.
- **FR-011**: The documentation MUST describe how to take a backup on both platforms:
  Kubernetes (run `backup` in the pod, then copy the file out of the node's backups
  directory) and Docker Compose (run `backup` in the service container).
- **FR-012**: The documentation MUST describe recovery: restore the latest backup, and for
  damage beyond that, salvage with the standard SQLite recovery tooling run off the node —
  noting that the Lightstack image ships no such tool.
- **FR-013**: The Kubernetes restore procedure MUST be exercised once on k3s as part of
  host validation, and the restored database MUST serve requests.

### Key Entities *(include if feature involves data)*

- **Backup file**: a single standalone SQLite database file that is a consistent snapshot
  of the live database at the moment it was taken. Self-contained — it carries no
  write-ahead-log or shared-memory side files.
- **Backups directory**: an operator-owned location on the node, separate from the live
  database directory, holding timestamped backup files. Created `0750` and owned by the
  process user; its backup files are `0600` — the same least-privilege modes as the live
  database. Excluded from teardown/retention purging.
- **Write-ahead-log / shared-memory side files**: the live database's auxiliary files.
  They belong to the live database only; the restore procedure removes them before putting
  a backup in place so a stale log is not replayed onto it.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An operator can take a backup with zero downtime — Lightstack answers
  requests for the full duration of the backup, and the result is one self-contained file.
- **SC-002**: A backup restored by following the documentation alone brings the service
  back up and passes the integrity check — proven on a database that left write-ahead-log
  and shared-memory side files behind, with the restored service reading back the backup's
  data rather than newer live data.
- **SC-003**: The integrity check gives an unambiguous, scriptable result — success on an
  intact database and a non-zero, problem-listing result on a damaged one — in 100% of
  runs.
- **SC-004**: Running backup or check against a PostgreSQL deployment fails fast with
  actionable guidance (it names `pg_dump`) and never produces a misleading partial result.
- **SC-005**: An operator can complete the restore on both Kubernetes and Docker Compose
  using the documentation alone, with no source-code reading — validated at least once on
  k3s, after which the restored database serves requests.

## Assumptions

- Builds on #598 (the SQLite backend) and #599 (the node-local storage layout and chart
  wiring); both are in place. The node's storage root and its database directory already
  exist as defined there.
- The migration tool is the right home for these operator commands: it already owns the
  database URL, backend selection, and schema migration, and runs both in-cluster and as a
  standalone binary.
- "Consistent while serving" relies on SQLite's online copy facility; the operator is not
  expected to quiesce writes before taking a backup.
- Backups are kept on the same node as the live database unless the operator copies them
  elsewhere; off-node storage of backups is the operator's responsibility and is not part
  of this feature.
- Both halves of FR-008 are existing startup-migration behaviour applied to a restored
  file: an older database is migrated forward, and one from a newer Lightstack already stops
  startup, because the migrator finds no migration for its version. What that refusal lacked is
  a message an operator can act on (it read "no migration found for version N: read down …");
  this feature names the cause and both versions. (Verified during implementation; the plan's
  research had assumed the refusal was missing.)

## Out of Scope

- Scheduled/automatic backups (for example a Kubernetes CronJob) — a follow-up.
- Backup or restore for the PostgreSQL backend — unchanged; operators use the backend's
  own tooling, and the new commands refuse PostgreSQL by design.
- Moving data between backends, or any migration from PostgreSQL to SQLite or back.
- Shipping additional recovery tooling in the Lightstack image; recovery beyond the latest
  backup uses external SQLite tooling run off the node.
- Backups for stub-mode deployments. Stub mode is a dev/test configuration; its backups
  location would not be node-persistent, and the chart adds no stub-mode backups storage.
- Everything the parent issue #595 lists as out of scope.

## Dependencies

- **#598** — the SQLite backend (backend selection by URL scheme, the single-file
  database, the migration tool's backend awareness).
- **#599** — the node-local storage layout and chart wiring the Kubernetes procedure
  relies on (the node's storage root and database directory, teardown/retention behaviour).
