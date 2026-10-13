# Feature Specification: SQLite backend, part 1 — database layer and migrations

**Feature Branch**: `feature/598` (feature id: `028-sqlite-database-layer`)
**Created**: 2026-10-12
**Status**: Implemented (2026-10-12) — the real-mode host run (T105) is pending
**Input**: cobaltcore-dev/cloud-storage#598 — "[FEAT] - lightstack - Database - SQLite 1/3: database layer and migrations". Part 1 of 3 of the epic cobaltcore-dev/cloud-storage#595 (SQLite as an alternative to PostgreSQL), which holds the design, the decisions, the constraints and the accepted drawbacks. Part 2 (cloud-storage#599) adds the Helm chart and Docker Compose; part 3 (cloud-storage#600) adds backup, integrity check and restore.

## Context

Lightstack keeps all of its state in PostgreSQL. A single-node deployment — the case Lightstack is built for — still has to run, secure, back up and upgrade a separate database server. After this feature the same binary can keep its state in one SQLite file on the node instead. The database URL picks the backend. PostgreSQL stays the default and does not change.

Every service talks to PostgreSQL directly today. Measured on this branch (`278d106`, 2026-10-12, production code unless noted):

| What | Count |
|---|---|
| SQL calls through the pool or a transaction (counted as in #595's *Verify*) | 736 in 67 files |
| "No rows" checks against the PostgreSQL driver's own sentinel | 165 |
| References to the PostgreSQL pool type | 61 in 28 files |
| References to the PostgreSQL transaction type | 29 in 10 files, 23 of them in Cinder |
| Affected-row counts read from a result | 52 |
| Error-code checks for a unique violation | 2 |
| Transactions | 13 (listed under *Edge Cases*) |
| Database-backed test packages | 8 — cinder, common, console, glance, keystone, metadata, neutron, nova |
| Test files that open a pool themselves | 10, plus the benchmark suite |
| Existing migrations | 73, numbered up to `073` |

The SQL is written in PostgreSQL's dialect. Most of it runs on SQLite as it is. A known set of constructs does not — casts, list membership against an array, row and advisory locks, interval arithmetic, regular-expression matching, JSON containment, `DISTINCT ON`, `generate_series`, epoch extraction, array aggregation, and `CURRENT_TIMESTAMP`, whose SQLite format differs. The SQLite side either translates these on the way in or the statement is rewritten once, the same for both backends.

Two further facts shape the work:

- **Two transactions hold the database while storage works.** Volume provisioning (also used when a server boots from a new volume) and snapshot creation run the image copy or the storage-backend call inside the quota transaction, so that a failed insert can still reclaim the backing. On PostgreSQL that holds one project's quota lock. On SQLite it would hold the whole database's write lock for as long as an image copy takes, blocking every write of every service.
- **Nova's quota check is not atomic with the write it guards.** It commits three separate checks (instances, cores, RAM) before the instance is inserted, so two concurrent creates at the limit can both pass. Cinder's equivalent race is already closed.

## Clarifications

### Session 2026-10-12

- Q: Several Lightstack nodes can share one PostgreSQL database (multi-node). Which rows still `creating` may a node sweep at startup? → A: **Only the rows it reserved itself.** Each reservation records the node that made it, and the sweep touches only those — as upstream Cinder's volume service does per host. This adds one column to volumes and to snapshots on both backends.
- Q: `lightstack cleanup` opens SQLite must-exist and finds the file missing. What does it do? → A: **The same as for an unreachable PostgreSQL database**: it warns, purges what it can find on the host, and exits 0; anything it could only have found through the database, such as the VMs, is left for the operator. The must-exist open still matters: without it a missing file would be created empty, the purge would find nothing, and nothing would warn. This replaces #598's wording "a missing file fails the purge".
- Q: Which identity does a reservation record, so that a restarted node finds its own rows? → A: **The host name, overridable by a new environment variable `LIGHTSTACK_NODE_NAME`.** Neither existing source is stable in the chart's default pods: `compute.node_id: auto` becomes a new random id on every start, and without host networking the host name is the pod's name. Part 2's chart sets the variable from the pod's node name, and Docker Compose sets it too. Stub-mode replicas on one node share the name; their creates take milliseconds.
- Q: Which file mode does the SQLite database get? It holds recoverable EC2 secrets, console bearer tokens and password hashes, and the driver creates files 0644. → A: **0600, enforced.** Lightstack creates the database 0600 and tightens an existing database, `-wal` and `-shm` to 0600 when it opens them, logging each change. Only the process owner can read them, as only PostgreSQL's owner can read its data directory.
- Q: How do the contract tests that seed rows through `docker exec lightstack-postgres psql` run against a SQLite-backed binary? → A: **Through a seeding helper on Lightstack's own database layer.** The helper opens the database from `LIGHTSTACK_DB_URL` and runs the same inserts and deletes on both backends; the tests themselves stay as they are, and no test depends on a container name any more.
- Q: Moving Nova's quota check into the instance-insert transaction puts it after the boot volume is copied. Does the early check stay? → A: **Check and recheck, as upstream Nova does.** Today's three early checks stay as a non-binding fast refusal before any boot volume is provisioned; the binding check runs in the insert transaction. Only a create that loses a race between the two pays for a boot volume, which is then removed.
- Q: Does CI run the contract suite against a SQLite-backed binary on every pull request? → A: **Yes, from part 1.** A CI job starts the stub-mode binary directly on a SQLite file, without Compose, and runs the contract suite and the quick integration suite, which CI already runs against PostgreSQL. Part 2 may move the job onto its SQLite Compose file.
- Q: What do the database-backed unit tests do when no test database is configured? → A: **They skip, as today.** `LIGHTSTACK_TEST_DB_URL`'s scheme picks the backend of a test run; the SQLite leg runs when it is a `sqlite://` URL. CI sets one URL per leg.
- Q: What must the schema parity test compare? → A: **Everything that changes behaviour, plus indexes.** Tables, columns, nullability, defaults (normalised: current time, random UUID, literals), primary, foreign and unique keys with their delete actions, CHECK constraints, and seed rows (by natural key where ids are random); indexes by name and columns, with a documented exception list for indexes SQLite has no equivalent of.
- Q: How durable is a SQLite commit? → A: **Fully durable: `synchronous=FULL`, set on every connection and read back by the settings test.** Every commit that returned survives a power loss, as on PostgreSQL; the faster `NORMAL` could lose the last commits while the VMs and volumes they recorded still exist.
- Q: Who adds the database directory to the cleanup's node-local purge — part 1 or part 2? → A: **Part 2.** Part 1 adds only the path helper; part 2 adds the directory to the purge, removed last, together with the chart's directory list and the retention policy that decide when it may go.
- Q: What does Lightstack log about its database at startup? → A: **One structured line with the backend, the location, the schema version and — on SQLite — the settings read back from a connection** (journal mode, synchronous, foreign keys, busy wait, transaction mode). The location is the PostgreSQL host, port and database, never credentials, or the SQLite file path.
- Q: (planning) Lightstack refuses a server create over quota with `413 overLimit`, while upstream Nova answers `403`. Which refusal does this feature produce and test? → A: **`403`, as upstream, in this feature.** Nova's server-create quota refusals — instances, cores, RAM, and the boot volume's block-storage quota — answer `403 Forbidden` with upstream's message form. Cinder's own refusals keep their upstream-correct `413`. This is one of the feature's two API changes, both towards upstream (the other is FR-036a).
- Q: (analysis) Upstream Nova filters `name` inside the database with the backend's own regex operator — PostgreSQL `~`, MySQL and SQLite `REGEXP` (`nova/db/main/api.py:1990-2003`) — and only `ip` in Python (`nova/compute/api.py:3244`). Where does Lightstack match them? → A: **Like upstream.** `name` stays `name ~ $n` on PostgreSQL, unchanged; on SQLite the translation turns `~` into `REGEXP`, backed by a registered function using the application's regular expressions, as upstream's database layer does on SQLite. `ip` is matched in the application on both backends. This replaces the ticket's "both filters resolved in Go", which rested on the premise that upstream applies both in Python.
- Q: (analysis) Are the new refusals for a volume or snapshot still `creating` an API change? → A: **Yes, a second correction towards upstream (FR-036a).** Today `DeleteSnapshot` accepts any status and volumes are never visible in `creating`; afterwards delete and extend of a `creating` volume and delete of a `creating` snapshot answer `400` with upstream's messages. Documented in `docs/UPGRADING.md`.
- Q: (host run, 2026-10-14) The real-mode host run (T105) needs a Compose stack that runs either backend, and no Compose file in the repository mounts the database directory. Where does it come from? → A: **`deployments/docker-compose-kvm.yml`, changed in this feature (FR-042).** It mounts the whole state root at the same path instead of four subdirectories, so `<state_path>/db` lives on the host — and so do the config-drive ISOs, which it did not mount before. The state root and the config directory become overridable, and `LIGHTSTACK_DB_URL` passes through, so the URL picks the backend. With host networking the container carries the host's name, so the node name needs no variable there. The stub-mode SQLite Compose file stays with part 2 (cloud-storage#599).
- Q: (chart review, 2026-10-14) Does the Helm chart need changes in part 1? → A: **Two, both closing gaps this feature opens; SQLite in the chart stays part 2.** The teardown purge Job receives `LIGHTSTACK_DB_URL` from `database.existingSecret`, as the workload does (FR-044): with the URL in that Secret the rendered config holds none, and `lightstack cleanup` now refuses an empty URL, so the blocking hook would stop the uninstall where it used to purge best-effort — and that best-effort purge never read the database, so it left the release's VMs behind. And the chart sets `LIGHTSTACK_NODE_NAME` from the pod's node name (FR-043): reservations exist since this feature, and without host networking a recreated pod has a new host name. This moves the chart half of the node-name answer above forward from part 2.
- Q: (implementation, T003) Is the test strategy approved — PostgreSQL gates as the oracle; both test legs through `LIGHTSTACK_TEST_DB_URL`; RED-first unit, story, concurrency (`deferred` × `immediate`), contract (incl. the `403`), refusal and parity tests; contract, quick and Horizon suites on both backends; a real-mode host run on both backends at the end? → A: **Approved as proposed** (constitution Test-First, step 2).

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Run Lightstack on one SQLite file (Priority: P1)

An operator of a single node points Lightstack at a file — `LIGHTSTACK_DB_URL=sqlite:///var/lib/lightstack/db/lightstack.db` — and starts it. Lightstack creates the directory and the database, applies the schema and seed data, and serves every service. No PostgreSQL runs anywhere. Clients see the same API they see on PostgreSQL.

**Why this priority**: It is the feature. Everything else protects it or protects PostgreSQL from it.

**Independent Test**: On a host without PostgreSQL, start the binary with a SQLite URL whose directory does not exist yet. Run the contract suite and the quick integration suite against it.

**Acceptance Scenarios**:

1. **Given** a SQLite URL whose directory does not exist, **When** Lightstack starts, **Then** it creates the directory, creates the database, applies the schema and seed data, and every service answers.
2. **Given** that running instance, **When** the contract suite and the quick integration suite run against it, **Then** both pass.
3. **Given** a running SQLite-backed instance, **When** readiness is probed, **Then** it reports ready, having reached the database the services use.
4. **Given** Lightstack stopped and started again on the same file, **When** it starts, **Then** it finds the schema current, applies nothing, and every resource created before the stop is still there.
5. **Given** any connection the SQLite pool opens, **When** its settings are read back, **Then** write-ahead logging is on, commits are fully synchronous, foreign keys are enforced and a busy wait is set.
6. **Given** the same requests against a PostgreSQL-backed and a SQLite-backed instance, **When** a client writes and reads back timestamps, metadata documents, lists, flags and ids, **Then** it receives the same values from both.
7. **Given** a database file that group or others can read, **When** Lightstack opens it, **Then** the database, `-wal` and `-shm` are 0600 afterwards, and the log names each file it tightened.
8. **Given** a start on SQLite, **When** the operator reads the startup log, **Then** one line names the backend, the file, the schema version and the settings read back from a connection.

---

### User Story 2 — PostgreSQL deployments see no change (Priority: P1)

An operator running Lightstack on PostgreSQL upgrades and notices nothing: the same URL forms, the same pool and transaction behaviour, the same migrations, the same API — apart from two corrections towards upstream: the status of a server create refused for quota (FR-035a), and `400` for requests against a volume or snapshot still being created (FR-036a).

**Why this priority**: PostgreSQL is the default and today's production path. A regression there costs more than SQLite gains.

**Independent Test**: Record the gates before the change — unit tests including the database-backed ones, the contract suite, lint, the quick integration suite. Run them again after. Check that `git diff --diff-filter=MDR --name-only main -- migrations/` lists only the corrected down files of 010/019/028/037 (FR-025).

**Acceptance Scenarios**:

1. **Given** a `postgres://` or `postgresql://` URL that works today, **When** Lightstack starts, **Then** it connects, migrates and serves as before.
2. **Given** the existing migration files, **When** the change is complete, **Then** none of them is modified, renamed or deleted, except the corrected down files of 010/019/028/037 (FR-025).
3. **Given** a PostgreSQL database already at the newest version, **When** the new build starts, **Then** it applies only the migration this feature adds (FR-033).
4. **Given** the contract suite, **When** it runs against PostgreSQL, **Then** its results match the pre-change baseline.
5. **Given** the pool settings in the configuration, **When** the PostgreSQL pool is built, **Then** it uses them exactly as before, and transactions still take their row locks.

---

### User Story 3 — Misconfiguration is refused at startup (Priority: P2)

An operator who mistypes the URL, leaves it empty, combines SQLite with multi-node networking, or runs a deployment whose declared backend disagrees with its URL is refused at startup with a message that says what is wrong. Lightstack never half-starts and never creates a database in the wrong place.

**Why this priority**: The Helm chart (part 2) cannot read a URL kept in a Secret, so the binary is the only place a mismatch can be caught. SQLite serves one node only; a second node on its own copy would split the cloud's state without an error.

**Independent Test**: Start the binary once per refused configuration. Each run exits non-zero within a second, names the problem, and leaves no file or directory behind.

**Acceptance Scenarios**:

1. **Given** a URL with an unknown scheme, no scheme, or an empty URL, **When** Lightstack starts, **Then** it refuses and the message lists the accepted schemes: `postgres`, `postgresql`, `sqlite`.
2. **Given** a PostgreSQL keyword/value connection string (`host=… user=…`), **When** Lightstack starts, **Then** it is refused with the same message. It already fails today, later and with a less helpful message.
3. **Given** a SQLite URL whose path is not absolute, or which carries query parameters, **When** Lightstack starts, **Then** it refuses and the message shows the expected form `sqlite:///absolute/path/lightstack.db`.
4. **Given** a SQLite URL and multi-node networking enabled (`neutron.vxlan_enabled`), **When** Lightstack starts, **Then** it refuses, naming both settings.
5. **Given** `LIGHTSTACK_DB_BACKEND=postgres` with a `sqlite://` URL, or `LIGHTSTACK_DB_BACKEND=sqlite` with a PostgreSQL URL, **When** Lightstack starts, **Then** it refuses, naming both values.
6. **Given** `LIGHTSTACK_DB_BACKEND` set to anything other than `postgres` or `sqlite`, **When** Lightstack starts, **Then** it refuses, naming the accepted values.
7. **Given** `LIGHTSTACK_DB_BACKEND` unset, **When** Lightstack starts with either backend, **Then** no backend check runs.

---

### User Story 4 — No request holds the database while storage works, and quotas hold under concurrency (Priority: P2)

A project creates a volume from a large image. While the image is copied, other projects keep creating, attaching and deleting resources — on SQLite as on PostgreSQL. Concurrent creates at a quota limit never overshoot it, for volumes, snapshots and servers alike.

**Why this priority**: On SQLite, one long transaction stops every write in the cloud, and each waiting writer fails once its busy wait runs out. Nova's quota race is a correctness defect on both backends.

**Independent Test**: Concurrency tests. (a) Start a volume create from an image whose copy takes several seconds; during the copy, a write by another project completes promptly. (b) Two concurrent server creates with one instance left in the quota. (c) Concurrent volume and snapshot creates at their limits.

**Acceptance Scenarios**:

1. **Given** a volume create from an image, **When** the image is being copied, **Then** no transaction is open, and another project's writes complete without waiting for the copy.
2. **Given** a volume or snapshot reserved but not yet finished, **When** a concurrent create computes the project's usage, **Then** the reservation counts, so no more than the limit is accepted.
3. **Given** a reservation whose backing cannot be created, **When** the create ends, **Then** the reserved row and any partial backing are removed, and the client receives the same error as today.
4. **Given** Lightstack stopped on a node while a create was in progress, **When** it starts again there, **Then** the interrupted row is set to `error`, and its backing, if one exists, is reclaimed when that volume or snapshot is deleted.
5. **Given** two Lightstack nodes sharing one PostgreSQL database, each with a create in progress, **When** Lightstack restarts on one of them, **Then** it sweeps only that node's interrupted reservation, and the other node's create completes.
6. **Given** two concurrent server creates and one instance left in the project's quota, **When** both complete, **Then** exactly one receives `202` and the other `403`, on both backends.
7. **Given** a server create refused for quota after its boot volume was provisioned, **When** the request ends, **Then** that boot volume is gone.
8. **Given** a project already at its instance limit, **When** a server create from a new boot volume arrives, **Then** it is refused with `403` before any volume is provisioned.
9. **Given** volume create, boot from a new volume, and snapshot create, **When** they complete, **Then** their status codes and response bodies are those of today.

---

### User Story 5 — One migration sequence serves both backends (Priority: P2)

A contributor who changes the schema adds one migration file, or a PostgreSQL/SQLite pair under the same number when the dialects differ. Both backends migrate from the same folder. The existing PostgreSQL migrations stay as they are. A fresh SQLite database starts from a baseline that equals the current PostgreSQL schema.

**Why this priority**: Without it every later schema change forks, and the backends drift apart unnoticed.

**Independent Test**: Migrate an empty PostgreSQL database and an empty SQLite database from the folder and compare them. Start two migrators at once against one SQLite file.

**Acceptance Scenarios**:

1. **Given** an empty SQLite database, **When** it is migrated, **Then** it starts at the baseline and applies every later migration.
2. **Given** an empty PostgreSQL database, **When** it is migrated, **Then** it applies the existing migrations and every later one, and never a file meant for SQLite only.
3. **Given** a version for which a backend has its own file, **When** that backend migrates, **Then** it uses its own file instead of the plain one with the same number.
4. **Given** both databases migrated from zero, **When** the parity test compares them, **Then** tables, columns, nullability, defaults, keys, CHECK constraints, seed rows and indexes agree, apart from the documented index exceptions.
5. **Given** two migrators started at once against one SQLite file, **When** both finish, **Then** the database is intact and at the newest version, and each migration was applied once.
6. **Given** a SQLite database, **When** a migration to a version below the baseline is requested, **Then** it is refused, naming the baseline.
7. **Given** `make migrate`, **When** it runs against either backend, **Then** it migrates through Lightstack's own migration tool; `make install-tools` no longer installs the upstream migration CLI.
8. **Given** a backend's view of the folder in which an up migration has no down migration, **When** the migrations are validated, **Then** validation fails for that backend.

---

### User Story 6 — Contributors write SQL once and learn early when SQLite cannot run it (Priority: P3)

A contributor writes SQL in PostgreSQL syntax and never calls a rewrite helper. A construct SQLite cannot run makes a test fail before review, even when no test executes that query.

**Why this priority**: It keeps the backends equal after this feature ships. The feature works without it, but would erode.

**Independent Test**: Add a statement with a construct outside the supported set to any SQL string under `internal/`. The static check fails, naming the file, the line and the construct. Remove it, and the check passes.

**Acceptance Scenarios**:

1. **Given** the supported construct set documented in the database package, **When** a SQL string under `internal/` uses a construct outside it, **Then** the static check fails, naming the file, the line and the construct.
2. **Given** the database-backed test packages, **When** CI runs, **Then** they run once against PostgreSQL and once against SQLite.
3. **Given** a statement with a cast, a list membership test, a row lock, an advisory lock or the console-token expiry arithmetic, **When** it runs on SQLite, **Then** it returns what it returns on PostgreSQL, with locking governed by SQLite's transaction policy.

### Edge Cases

**Opening the database**

- **Missing directory**: created before the file is opened. SQLite does not create directories, and Docker Compose or a bare binary have nothing else that would.
- **Missing file**: the server and `lightstack-migrate` create it — a first start is a fresh install. `lightstack console-tokens` refuses, because an inspection tool must not create a database. `lightstack cleanup` warns and purges what it finds on the host, as for an unreachable PostgreSQL database; it never creates the file (FR-012).
- **A database file from another node or a stale copy**: not detectable here. Part 2 pins the pod to its node.
- **A database readable by others** — created by another tool, restored from a copy, or left by an earlier build: tightened to 0600 at open, `-wal` and `-shm` included, with a log line per file. A file Lightstack cannot change the mode of (another owner) fails the open, naming the file.
- **Busy wait exhausted**: a write that waited the full busy wait fails the way any database error fails a request today. The concurrency tests size the wait so that the tested load never reaches it.

**Translation**

- **Quoted text** containing `::`, `FOR UPDATE`, `ANY(` or `now()` is never rewritten.
- **An empty list** in a membership test matches nothing, on both backends.
- **The same statement with lists of different lengths**: the translation is reused; only the list expansion differs per call.
- **A long list**: lists are small in practice — a page of servers, a server's security groups, one node's console tokens. A list longer than SQLite's bound-parameter limit fails with an error; it is never truncated.
- **The SNAT predicate** compares the JSON text `false`. It holds on SQLite once the JSON casts are dropped, and a test proves it.
- **Regular-expression filters** on server listings follow upstream. `name` is matched by the database: PostgreSQL's `~` on PostgreSQL (unchanged), and on SQLite a registered `REGEXP` function using the application's regular expressions — as upstream uses the backend's operator, and Python's `re` on SQLite. `ip` is matched in the application on both backends, as upstream does in Python. Patterns are validated with the application's syntax first and refused with `400` when invalid, as today. A pattern whose meaning differs between the dialects — `\b` is a backspace in PostgreSQL and a word boundary in the application — follows each backend's database for `name`, as upstream's does.
- **The optimistic-concurrency checks** on `updated_at` (server state reconciliation, server updates) compare timestamps for equality. They hold on SQLite because every stored and every bound timestamp has the same fixed text format.

**Values**

- **Ids in a non-canonical spelling** (upper case, no dashes) match nothing on SQLite, as on upstream OpenStack, which stores ids as strings. On PostgreSQL behaviour stays as it is today. The exception is where Lightstack itself promises an alternate spelling: a token scope's project id and the project an S3 token resolves — the dash-free id `rgw_compat_tenant_ids` advertises — are parsed and looked up in canonical form, on both backends, as an EC2 credential's tenant already was. The canonicalising is the caller's, not the database layer's (FR-023).
- **Ordering**: ids and timestamps sort the same on both backends. The placement of NULLs and the order of mixed-case text follow the backend, as upstream's order depends on its database.

**Transactions** — the 13 transactions on this branch, 5 of which read before they write and 8 of which begin with a write:

| Area | Transactions |
|---|---|
| Quota | Nova's quota check; Cinder's shared quota helper (used by volume create and boot from a new volume, volume and snapshot manage, backup create and restore, snapshot create, extend, transfer accept) |
| Volumes | attach; detach; volume metadata replacement; snapshot metadata replacement; transfer create; transfer delete |
| Servers | create; delete; server metadata replacement |
| Networking | port allocation for a server; default security group |

A deferred SQLite transaction that reads and then writes fails at once if another write committed in between, and the busy wait does not help there. The concurrency tests decide whether transactions start immediate (FR-010).

**Reservations**

- **A delete, attach or extend request against a volume still `creating`**, or a delete of a snapshot still `creating`, is refused with `400` as upstream refuses it for that state (FR-036a).
- **Several Lightstack nodes sharing one PostgreSQL database** (multi-node): a restart on one node sweeps only that node's reservations, never another node's create in progress (FR-033).
- **A node that never comes back** — removed for good, or renamed so that its node name changes — leaves its interrupted reservations in `creating`. They keep counting towards quota until an administrator resets them, as upstream operators do for a volume stuck in a transitional state.
- **Rows that existed before the new column**: no volume or snapshot is left `creating` by the current code paths except a snapshot whose deferred status update was cut off by a stop. Such a row has no recorded node name and is not swept.

## Requirements *(mandatory)*

### Functional Requirements

**Backend selection and configuration**

- **FR-001**: The backend MUST be chosen once, at startup, from the database URL's scheme: `postgres` or `postgresql` select PostgreSQL, `sqlite` selects SQLite. Any other scheme, no scheme, or an empty URL MUST be refused, and the message MUST list the accepted schemes. This applies to every entry point that opens the database: the server, `lightstack cleanup`, `lightstack console-tokens` and `lightstack-migrate`.
- **FR-002**: A SQLite URL MUST have the form `sqlite:///<absolute path>`. A relative path or query parameters MUST be refused, with the expected form in the message. Lightstack owns the SQLite connection settings.
- **FR-003**: Every PostgreSQL URL that works today MUST keep working, with today's connection settings, pool, transactions (including row locks) and migration locking. PostgreSQL MUST receive every statement unchanged.
- **FR-004**: When the environment variable `LIGHTSTACK_DB_BACKEND` is set, it MUST be `postgres` or `sqlite`, and a database URL selecting the other backend MUST be refused, naming both values. Any other value MUST be refused, naming the accepted ones. When the variable is unset, no check runs. The check applies to every entry point listed in FR-001.
- **FR-005**: The server MUST refuse to start with a SQLite URL when multi-node networking (`neutron.vxlan_enabled`) is enabled, naming both settings.

**Database layer**

- **FR-006**: Every service, background worker, helper and database-backed test MUST reach the database through one backend-neutral contract — connect, query, query one row, execute, begin, ping, close; results with affected-row counts; row iteration and scanning; commit and rollback. No PostgreSQL driver type may appear outside the PostgreSQL implementation. Constructors keep ADR-001's shape, with the database as first parameter, and the transaction helper keeps its shape.
- **FR-007**: "No rows" MUST be reported identically by both backends, and all 165 checks MUST use that one condition. Errors MUST be classified backend-neutrally as unique, foreign-key, not-null, check, and busy/serialization violations; the 2 unique-violation checks MUST use that classification.
- **FR-008**: Every SQLite connection MUST run with write-ahead logging, fully synchronous commits (`synchronous=FULL`, so every acknowledged commit survives a power loss), a bounded busy wait and foreign-key enforcement, applied in a form the pinned driver version honours. A test MUST read the settings back from a pooled connection.
- **FR-009**: The SQLite pool MUST hold several connections: readers in parallel, one writer at a time, other writers waiting up to the busy wait. Neither a single connection (reads and readiness would queue behind writes) nor a separate reader pool (statements that write and return rows run as queries).
- **FR-010**: Whether SQLite transactions start deferred or immediate MUST be decided by the concurrency tests (FR-039) and documented with their results.
- **FR-011**: Opening SQLite MUST create the database file's parent directory (mode 0750) when it is missing. Directories missing above it MUST be created 0755: on a fresh host the state root is one of them, and QEMU, which does not run as root, must traverse it to reach the instance disks. The node-local path helper MUST gain the database directory, `<state_path>/db`. Part 2 adds it to the cleanup's node-local purge and to the chart's directory list; this feature does not purge it.
- **FR-011a**: The SQLite database and its `-wal` and `-shm` files MUST be readable and writable by the process owner only (0600). Lightstack MUST create the database with that mode and tighten an existing database, `-wal` or `-shm` with a wider mode when it opens them, logging each file it changes. A file whose mode it cannot change MUST fail the open, naming the file.
- **FR-012**: Opening SQLite MUST offer a must-exist option. The server and `lightstack-migrate` create a missing database; `lightstack console-tokens` and `lightstack cleanup` open must-exist and MUST NOT create a file. When the file is missing, `lightstack console-tokens` fails naming the path, and `lightstack cleanup` takes the path it takes today for an unreachable PostgreSQL database: it warns, naming the path, purges what it finds on the host, and exits 0.
- **FR-013**: The readiness check MUST probe the database on both backends.
- **FR-013a**: At startup Lightstack MUST log one structured line naming the backend, its location and the schema version after migration. The location is the PostgreSQL host, port and database — never a user name or password — or the SQLite file path. On SQLite the line MUST also carry the settings read back from a pooled connection — journal mode, synchronous mode, foreign-key enforcement and busy wait — and the configured transaction mode.

**SQL: written once, translated for SQLite**

- **FR-014**: Callers MUST write SQL once, in PostgreSQL syntax, and never call a rewrite helper. The SQLite implementation MUST translate on the way in, leaving quoted text untouched: text and integer casts become standard casts and every other cast is dropped; list membership against an array argument (with or without an array cast) expands to one placeholder per element, later placeholders renumbered; row locks are removed and advisory locks do nothing; the console-token expiry arithmetic becomes a registered add-seconds function, so the database clock keeps owning expiry; the regular-expression match `~` becomes `REGEXP`; a `LIMIT $n` or `OFFSET $n` placeholder becomes `COALESCE($n, -1)` or `COALESCE($n, 0)`, so a NULL bound keeps PostgreSQL's meaning of no limit and no offset. Translations MUST be cached per statement text; only list expansion runs per call.
- **FR-015**: Every SQLite connection MUST register the current-time function (returning the timestamp format of FR-019), the random-UUID function, the add-seconds function, and the `regexp` function behind `REGEXP`, which uses the application's regular expressions.
- **FR-016**: Statements without a faithful translation MUST be rewritten once, the same for both backends: tenant-usage hours computed in the application; VNI allocation as a recursive query; the two JSON-containment lookups matched in the application; the aggregate metadata merge done in the application inside a transaction; the DHCP reconciler's first subnet per network by window function; the `ip` server filter resolved in the application, which matches the scope's port addresses and selects the servers by id (this also removes the only array-unpacking call) — the `name` filter stays a database match (FR-014); the 7 `CURRENT_TIMESTAMP` uses replaced by the current-time function; the row-collecting helper replaced by a scan loop; and the floating-address array aggregation in Nova's address listing (2 statements), which yields a list on both backends and an empty list for a server without floating addresses.
- **FR-017**: The JSON field operators (`->>`, `->`) MUST stay as they are. A test MUST prove that the SNAT predicate evaluates the same on SQLite.
- **FR-018**: The database package MUST document the supported construct set. A static check MUST scan the SQL string literals under `internal/` — production and test code — and fail on any construct outside the set, naming the file, the line and the construct.

**Values on SQLite** — converted by the SQLite implementation, so callers bind and scan the same types on both backends.

- **FR-019**: Timestamps MUST be stored as text in UTC, RFC 3339 with exactly six fractional digits (`2026-10-09T08:15:30.123456Z`), so that string order equals time order and precision equals PostgreSQL's. Column defaults use the registered current-time function. Bound times MUST be formatted by the database layer and parsed back on scan.
- **FR-020**: JSON MUST be stored as text. Map and struct arguments are encoded on the way in; scans into maps and structs decode.
- **FR-021**: Lists (array columns) MUST be stored as JSON array text. A list argument in a membership test is expanded instead (FR-014).
- **FR-022**: Booleans MUST be stored as 0/1 and scan into booleans. NULL handling is unchanged: nullable columns scan into pointers.
- **FR-023**: Ids MUST be stored as text in canonical form and compared as strings. The database layer MUST NOT canonicalise values.

**Schema and migrations**

- **FR-024**: Both backends MUST migrate from the one `migrations/` folder, through a per-backend view: (1) a `.postgres` or `.sqlite` file is used only by that backend; (2) a backend's own file for a version replaces the plain file with the same number; (3) SQLite skips every version below its baseline. Down migrations follow the same rules. Validation of up/down pairs MUST run per backend view.
- **FR-025**: This feature MUST NOT modify, rename or delete an existing migration file, except the down files of `010`, `019`, `028` and `037`: they dropped what an earlier migration owned, so PostgreSQL could not migrate down past them (corrected 2026-10-13, at the user's request; below the baseline, SQLite never reads them).
- **FR-026**: The SQLite baseline MUST be the schema, seed data included, at the newest migration on `main` when the baseline is written, under that migration's number, with text ids and strict tables.
- **FR-027**: Every later schema change MUST continue the one sequence with a plain file, or a `.postgres`/`.sqlite` pair under the same number. The conventions that keep a change to one file — no `IF NOT EXISTS` on added columns, ids generated by the application, timestamp defaults through the current-time function — MUST be documented in `migrations/README.md` together with the three rules.
- **FR-028**: A CI parity test MUST migrate both backends from zero and compare: tables; columns; nullability; column defaults, normalised so that the current-time function, the random-UUID function and literals compare equal across backends (the existing schema has 113 timestamp defaults and 59 random-id defaults that inserts rely on); primary, foreign and unique keys with their delete actions; CHECK constraints (6 today); seed rows, matched by natural key where ids are random; and indexes by name and columns, partial-index predicates included. Indexes SQLite has no equivalent of — today the GIN index on image properties — are listed as documented exceptions.
- **FR-029**: `lightstack-migrate` and the startup migration MUST handle both backends. `make migrate` MUST call `lightstack-migrate up`, and `make install-tools` MUST stop installing the upstream migration CLI, which reads the folder without the view, refuses it once a SQLite file shares a version, and would apply the SQLite baseline to PostgreSQL.
- **FR-030**: SQLite migrations MUST run under an exclusive file lock next to the database for the whole run, because the migration library locks only within one process. PostgreSQL keeps its advisory lock.
- **FR-031**: On SQLite, a migration to a version below the baseline MUST be refused, naming the baseline.

**Transaction boundaries**

- **FR-032**: Volume create, boot from a new volume and snapshot create MUST reserve first: one transaction takes the quota lock, checks usage, inserts the row with status `creating` and commits; the backing is created outside any transaction; a second transaction marks the row `available` (with `bootable` and the source image where they apply), or, on failure, the row and the backing are removed. Rows in `creating` count towards quota usage.
- **FR-033**: Every reservation MUST record the node name of the Lightstack node that made it: the value of the environment variable `LIGHTSTACK_NODE_NAME` when set, the host name otherwise. At startup, Lightstack MUST set the rows reserved under its own node name that are still `creating` to `error`; it MUST NOT touch a row reserved under another node name. As upstream Cinder's cleanup does, it MUST leave their backing in place — deleting the errored volume or snapshot reclaims it — so the backing of a create that is still being built is never removed from under it. The record is one added column on volumes and on snapshots, delivered as a migration that runs on both backends.
- **FR-034**: After the change, no transaction on either backend may span a libvirt, storage, network or HTTP call. Every transaction in the inventory under *Edge Cases*, and every transaction this feature adds, is checked against that rule.
- **FR-035**: Nova's quota MUST be checked twice, as upstream Nova does. Today's early checks (instances, cores, RAM) stay as a non-binding refusal before any boot volume is provisioned. The binding check MUST take the project's quota lock — advisory, since a project on the defaults has no `quotas` row — count usage and insert the instance in one transaction, so two concurrent creates at the limit end as one `202` and one `403` on both backends. A server's boot volume keeps its own Cinder reservation before that transaction, and a server refused by the binding check MUST leave no boot volume behind.
- **FR-035a**: A server create refused for quota — instances, cores, RAM, or the block-storage quota of a new boot volume — MUST answer `403 Forbidden`, as upstream Nova does, with upstream's message form (`Quota exceeded for instances: Requested 1, but already used 10 of 10 instances`). Today it answers `413 overLimit`. Cinder's own quota refusals keep their `413`, which is upstream's.
- **FR-036**: Apart from FR-035a and FR-036a, the operations changed by FR-032 – FR-035 MUST keep their status codes and response bodies — snapshot create still answers `202` with status `creating`.
- **FR-036a**: Delete and extend of a volume, and delete of a snapshot, MUST be refused with `400` while the resource is `creating`, with upstream's messages (contract [block-storage-reservation.md](contracts/block-storage-reservation.md)). Today `DeleteSnapshot` accepts any status and volumes are never visible in `creating`, so this is the feature's second API change, towards upstream. Attach already requires `available`.

**Tests, decision record and documentation**

- **FR-037**: The 8 database-backed test packages MUST run against both backends, and CI MUST gain a SQLite leg next to the PostgreSQL one. The scheme of `LIGHTSTACK_TEST_DB_URL` picks the backend of a test run; when it is unset the database-backed tests skip, as today. Test fixtures written in PostgreSQL-only SQL MUST become backend-neutral; tests that inspect PostgreSQL internals (system catalogs, a PL/pgSQL repair block, pool statistics) run on PostgreSQL only.
- **FR-038**: The contract suite (`test/contract`) and the quick integration suite (`test/quick_test.sh`) MUST pass against a SQLite-backed binary. The 6 statements that seed rows through `docker exec lightstack-postgres psql` (5 tests in 3 files) MUST seed through a helper that opens the database from `LIGHTSTACK_DB_URL` through Lightstack's database layer, on both backends. For SQLite the suite runs on the host that holds the database file. CI MUST run both suites against a SQLite-backed binary on every pull request, in a job that starts the stub-mode binary directly on a SQLite file.
- **FR-039**: Concurrency tests on SQLite MUST cover every transaction in the inventory that reads before it writes, once with deferred and once with immediate transactions; their results decide FR-010. Further tests MUST run two migrators against one database file, prove that a volume created from an image does not block another project's writes during the copy, and run two concurrent server creates at the instance limit on both backends.
- **FR-040**: A decision record, ADR-003 "Pluggable database backend", MUST record the contract, the scheme-based selection, the pure-Go driver, the translation for SQLite and the single migrations folder, and MUST amend ADR-001's rejection of interface-based injection.
- **FR-041**: Documentation MUST describe the result: `docs/CONFIGURATION.md` (accepted schemes, the SQLite URL form, `LIGHTSTACK_DB_BACKEND`, `LIGHTSTACK_NODE_NAME`), `docs/ARCHITECTURE.md`, `CLAUDE.md` (the injection pattern and the migration rules), `migrations/README.md`, `docs/DOCKER_DEPLOYMENT.md` (`lightstack-migrate` on both backends), `docs/adr/README.md` (ADR-003 listed), and `docs/UPGRADING.md` (the upstream migration CLI no longer reads the folder; `make migrate` changed; a server create refused for quota answers `403` instead of `413`; requests against a `creating` volume or snapshot answer `400`).

**Deployment**

- **FR-042**: `deployments/docker-compose-kvm.yml` MUST run either backend unchanged. It mounts the whole node-local state root at the same path inside and outside the container, so a SQLite database in `<state_path>/db` lives on the host. The state root and the config directory are overridable through the Compose variables `LIGHTSTACK_STATE_PATH` and `LIGHTSTACK_CONFIG_DIR` — read by Compose, not by the binary, defaulting to today's `/var/lib/lightstack` and `/opt/lightstack/config` — and `LIGHTSTACK_DB_URL` passes through to the binary, empty keeping the config's URL. A render test (`make test-compose-kvm`) checks the defaults and the overrides; `deployments/README-KVM.md` documents both backends.

- **FR-043**: The Helm chart MUST set `LIGHTSTACK_NODE_NAME` on the Lightstack container from the pod's node name (`spec.nodeName`), in every mode. Without host networking — the default — the host name is the pod's name, which a recreated pod does not keep, so the startup sweep would never find the reservations its predecessor left (FR-033).
- **FR-044**: The chart's teardown purge Job MUST receive `LIGHTSTACK_DB_URL` from `database.existingSecret` exactly as the workload does. With the URL in that Secret the rendered config holds none, and `lightstack cleanup` refuses an empty URL (FR-001), which would fail the blocking pre-delete hook and stop the uninstall.

### Key Entities

- **Database backend**: PostgreSQL or SQLite. Chosen once per process from the database URL. PostgreSQL is the default; SQLite is single-node only.
- **Database URL**: the one setting that selects and locates the database. For SQLite, an absolute file path.
- **Declared backend**: the optional `LIGHTSTACK_DB_BACKEND` value a deployment sets so the binary can verify a URL it cannot otherwise see.
- **Database contract**: the backend-neutral way every component queries, writes and runs transactions, with backend-neutral "no rows" and error classes.
- **Supported construct set**: the SQL constructs both backends run — directly or through translation. Documented and enforced by the static check.
- **Migration view**: one backend's reading of the migrations folder under the three rules.
- **SQLite baseline**: the complete schema and seed data at one migration number, where every SQLite database starts.
- **Reservation**: a volume or snapshot row in `creating` that holds quota while its backing is created outside any transaction, and records the node name of the Lightstack node that made it.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a host without PostgreSQL, and in CI on every pull request, a SQLite-backed Lightstack passes the contract suite and the quick integration suite: every test that passes against PostgreSQL also passes against SQLite, with the same skips.
- **SC-002**: On PostgreSQL, every gate matches the pre-change baseline apart from the corrections of FR-035a and FR-036a and the new tests, and `git diff --diff-filter=MDR --name-only main -- migrations/` lists only the four corrected down files of FR-025.
- **SC-003**: Each refused configuration in User Story 3 exits non-zero **within 1 second**, with a message naming the accepted values or both conflicting values, and leaves **no** file or directory behind.
- **SC-004**: All **8** database-backed test packages pass on both backends in CI, and the schema parity test reports **zero** differences.
- **SC-005**: Two concurrent server creates at the instance limit end as exactly one `202` and one `403` in **20 of 20** runs on each backend. Concurrent volume and snapshot creates never take a project past its limits.
- **SC-006**: While a volume is created from an image whose copy takes at least 10 seconds on SQLite, a write by another project completes in **under 1 second**.
- **SC-007**: Two migrators started at once against one SQLite file both finish without error in **20 of 20** runs; the database passes an integrity check, sits at the newest version, and recorded each migration once.
- **SC-008**: **Zero** transactions span a libvirt, storage, network or HTTP call, checked against the 13-transaction inventory plus every transaction the feature adds.
- **SC-009**: The static construct check fails for **every** construct class outside the supported set (one seeded case per class) and passes on the finished tree.
- **SC-010**: With two Lightstack nodes on one PostgreSQL database, a restart on one changes **zero** reservations made by the other.
- **SC-011**: After any start on SQLite, the database, `-wal` and `-shm` files grant **no** access to group or others — for a fresh database and for one that was 0644 before.

## Assumptions

- **The design is fixed by cloud-storage#595 and #598** and is not reopened here: a backend-neutral contract with two implementations, selection by URL scheme, the pure-Go SQLite driver (the image builds without cgo), translation inside the SQLite implementation rather than an ORM or a repository layer, and one migrations folder read through a per-backend view.
- **The ticket's counts reproduce on this branch** (`278d106`): 165 "no rows" checks, 61 pool-type references in 28 files, 29 transaction-type references in 10 files (23 in Cinder), 52 affected-row reads, 2 unique-violation checks, 7 `CURRENT_TIMESTAMP` uses. The ticket's "10 test files that build a pool" are the 8 package test mains plus two Keystone tests; the benchmark suite opens an eleventh pool.
- **Found while specifying, not in the ticket's list**:
  - Production: the floating-address array aggregation with an empty-array default in Nova's address listing (2 statements) — added to FR-016. `IS NOT DISTINCT FROM` (2 statements) runs on SQLite 3.39 and later and joins the supported set unchanged.
  - Test fixtures: interval literals (4), `generate_series` (5 in one file), an array literal, a PL/pgSQL repair block, PostgreSQL catalog queries, a pool-statistics call, and driver-specific error values in the unique-violation test — covered by FR-037.
  - Transactions: volume transfer create and delete also read before they write and join the concurrency tests (FR-039).
  - Contract suite: 6 statements (5 tests in 3 files) seed rows through `docker exec lightstack-postgres psql` and fail without that container — covered by FR-038.
- **Only the two Cinder operations do storage work inside a transaction.** The other transaction bodies on this branch — the 11 of the 13 inventory entries, plus the quota helper's other callers — were read and call no libvirt, storage, network or HTTP operation, so FR-034 needs no change there.
- **The node name must survive a restart, which is why `LIGHTSTACK_NODE_NAME` exists.** The node registry's `auto` id is generated afresh at every start, and a chart pod without host networking (the default) has the pod's name as its host name. The chart therefore sets the variable from the pod's node name (FR-043); a deployment whose host name changes and that does not set it cannot find its own interrupted reservations, which stay `creating` until an administrator resets them. The node registry keeps its own id unchanged.
- **Connection settings form**: the driver applies `_pragma=` settings on every connection it opens. The pinned v1.60.1 also accepts and validates mattn-style shorthand keys, which v1.46.1 ignored silently, so Lightstack uses the `_pragma` form that works in both, and FR-008's read-back test is the guard. The newest driver release is pinned.
- **Regular-expression filters** stay where upstream applies them: `name` in the database (PostgreSQL unchanged; `REGEXP` through a registered function on SQLite), `ip` in the application (FR-014 – FR-016). PostgreSQL deployments see no change; on SQLite, `name` takes the application's dialect, as upstream's SQLite backend takes Python's.
- **Text order** of names and the placement of NULLs may differ between backends. Neither is part of the API's contract, and upstream OpenStack's order also depends on its database.
- **Pool settings** from the configuration apply to SQLite where they have a meaning. Those without one are ignored and documented as PostgreSQL-only.
- **No data migration** between the backends and no switching of an existing deployment (cloud-storage#595, out of scope).

## Dependencies

- cloud-storage#595 — design, decisions, accepted drawbacks.
- cloud-storage#599 (part 2) consumes `LIGHTSTACK_DB_BACKEND`, `LIGHTSTACK_NODE_NAME` (in its stub-mode Docker Compose file; the chart sets it since FR-043), the database directory helper and the must-exist open; cloud-storage#600 (part 3) builds backup, integrity check and restore on the SQLite backend.
- ADR-001 (database injection), amended by ADR-003.
- One new module, the SQLite driver (`modernc.org/sqlite`). The migration library's SQLite driver and directory-reading source are packages of a module already required.
- A PostgreSQL test database for the PostgreSQL leg (`make db-up`), and CI capacity for the SQLite leg.

## Out of Scope

- SQLite in the Helm chart, and a stub-mode Docker Compose file for SQLite (part 2, cloud-storage#599). In scope: the real-mode Compose file (FR-042), which the host run needs, and the two chart changes this feature's own behaviour needs (FR-043, FR-044).
- Removing the database directory in `lightstack cleanup`, and the chart's directory list (part 2, cloud-storage#599).
- Backup, integrity check and restore (part 3, cloud-storage#600).
- Everything cloud-storage#595 lists as out of scope — among it migrating data between backends, switching an existing deployment's backend, and SQLite for multi-node deployments.
- Any API change other than FR-035a and FR-036a.
