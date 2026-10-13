# Contract: database configuration and startup

What an operator configures, what every entry point that opens the database checks, and what it reports. Covers FR-001 – FR-005, FR-011 – FR-013a, FR-033 (node name) and SC-003.

## Entry points

| Entry point | Opens the database | On a refused configuration | On a missing SQLite file |
|---|---|---|---|
| `lightstack` (server) | create-if-missing | exits 1 (fatal log), before any file or directory is created | creates directory (0750, missing parents 0755) and file (0600), migrates — a fresh install |
| `lightstack cleanup` | must-exist | exits 2 (its config-error code), before teardown starts | warns naming the path and takes today's "database unreachable" path: host-only purge, exit 0 (clarification 2026-10-12) |
| `lightstack console-tokens` | must-exist | exits 2 (its config-error code) | exits 1 naming the path (its connect-failure code) |
| `lightstack-migrate` | create-if-missing | exits 1 | creates directory and file, as the server does |
| `lightstack healthcheck` | does not open the database | — | — |

Every entry point runs the checks below **right after loading its configuration**, in this order, before opening anything. A refusal therefore never leaves a file or directory behind (SC-003).

## 1. URL (FR-001, FR-002)

| `database.url` (after the `LIGHTSTACK_DB_URL` override) | Backend |
|---|---|
| `postgres://…`, `postgresql://…` — every form that works today | PostgreSQL, passed to pgx and golang-migrate unchanged |
| `sqlite:///<absolute path>` — no host, no query, no fragment | SQLite |

Refused, with these messages (`<url>` is printed with any password replaced by `xxxxx`):

| Input | Message |
|---|---|
| empty | `database.url is empty; accepted schemes: postgres, postgresql, sqlite` |
| no scheme, incl. keyword/value (`host=db user=…`) | `database.url has no scheme; accepted schemes: postgres, postgresql, sqlite` |
| any other scheme (`mysql://…`, `file:…`, `sqlite3://…`) | `database.url scheme "<scheme>" is not supported; accepted schemes: postgres, postgresql, sqlite` |
| `sqlite://relative/x.db`, `sqlite://host/x.db`, `sqlite:///x.db?mode=ro`, `sqlite:x.db` | `database.url "<url>" is not a valid SQLite URL; expected sqlite:///absolute/path/lightstack.db (absolute path, no host, no query parameters)` |

## 2. Declared backend (FR-004)

| `LIGHTSTACK_DB_BACKEND` | URL selects | Result |
|---|---|---|
| unset (or empty) | either | no check |
| `postgres` | PostgreSQL | accepted |
| `sqlite` | SQLite | accepted |
| `postgres` | SQLite | refused: `LIGHTSTACK_DB_BACKEND=postgres but database.url selects sqlite` |
| `sqlite` | PostgreSQL | refused: `LIGHTSTACK_DB_BACKEND=sqlite but database.url selects postgres` |
| anything else | either | refused: `LIGHTSTACK_DB_BACKEND="<value>" is not valid; accepted values: postgres, sqlite` |

## 3. Multi-node guard — server only (FR-005)

`database.url` selects SQLite **and** `neutron.vxlan_enabled: true` → refused: `database.url selects sqlite, but neutron.vxlan_enabled is true; SQLite serves a single node only`.

## 4. Node name (FR-033)

`LIGHTSTACK_NODE_NAME` when set and non-empty; otherwise `os.Hostname()`. The Helm chart sets it to the pod's node name (FR-043). Resolved once at server start. A failure to determine either is fatal at startup. The value is recorded in `volumes.reserved_by` / `snapshots.reserved_by` and selects the rows the startup sweep may touch. It is independent of `compute.node_id`.

## 5. Opening SQLite (FR-008, FR-009, FR-011, FR-011a, FR-012)

1. Create-if-missing entry points create the parent directory (0750) and the database file (0600) when missing. Directories missing above the parent are created 0755, so QEMU can still traverse the state root. Must-exist entry points stat the file and fail naming the path when it is missing; they never create it.
2. Tighten the database, `-wal` and `-shm` files to 0600 when any has group or other bits, logging one line per file (`database file mode tightened` with `path`, `from`, `to`). A file whose mode cannot be changed fails the open, naming it.
3. Open with the connection settings of research R3 (write-ahead logging, `synchronous=FULL`, busy wait, foreign keys, the transaction mode of FR-010) applied to **every** pooled connection.
4. Read the settings back from a pooled connection and fail the open if any differs from what was requested — the guard against a driver that ignores a setting silently.

## 6. Startup log line (FR-013a)

One structured line after migrations, at `info`:

```json
{"level":"info","backend":"sqlite","location":"/var/lib/lightstack/db/lightstack.db",
 "schema_version":74,"node":"node-a","journal_mode":"wal","synchronous":"full","foreign_keys":true,
 "busy_timeout_ms":5000,"txlock":"immediate","time":"2026-10-12T10:00:00Z","message":"database ready"}
{"level":"info","backend":"postgres","location":"db.example.com:5432/lightstack",
 "schema_version":74,"node":"node-a","time":"2026-10-12T10:00:00Z","message":"database ready"}
```

`location` never contains a user name or password. The SQLite settings are the values read back in step 5.4, not the requested ones — except `txlock`, the configured transaction mode, which no connection reports.
