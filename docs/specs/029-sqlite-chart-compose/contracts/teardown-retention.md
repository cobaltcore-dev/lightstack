# Contract: Teardown and retention

**Feature**: `029-sqlite-chart-compose`

Covers the purge Job's scheduling, the order in which `db/` is removed, and the `delete`/`retain` behaviour. Clauses marked **[unit]** are helm-unittest; **[go]** are `internal/teardown` Go tests; **[host]** are the k3s host run.

## C-TEARDOWN-1 — Purge-Job scheduling parity (FR-010) **[unit]**

- The teardown purge Job's `spec.template.spec.nodeSelector`, `affinity` and `tolerations` equal the Deployment's, for the same values — on **both** backends.
- Setting `nodeSelector`, `affinity` or `tolerations` changes both the Deployment and the purge Job identically.
- `deploymentMode: multi-node` (PostgreSQL) still renders the merged node-selector label on the purge Job as on the workload (the shared helper preserves the two-branch logic).

## C-TEARDOWN-2 — Purge-Job SQLite db access (FR-011) **[unit]**

- Given SQLite values in stub mode and `dataRetentionPolicy: delete`, the purge Job mounts the node-local **state root** (`host-storage` at `/var/lib/lightstack`, `DirectoryOrCreate`) — **not** the `db/` directory directly. `lightstack cleanup` RemoveAll's `db/`, which cannot be unlinked when it is the mountpoint, so `db/` must be a subdirectory of the mount.
- Given SQLite values in real mode, the purge Job mounts `host-storage` (unchanged) — the db directory is inside it.
- The purge Job container carries `LIGHTSTACK_DB_BACKEND` (C-RENDER-1).

## C-TEARDOWN-3 — `db/` removed last, after the connection closes (FR-013) **[go]**

- `purgeLocalStorage` removes `paths.DBDir()` and it is the **last** directory removed (after `images/`, `volumes/`, `instances/`, `cloud-init/`, `dhcp/`, `console-tokens/`, `console-sockets/`).
- The enumeration database connection is closed **before** the storage-purge step runs: in `execute`, `closeDB` is called after the network purge and before the storage purge. A test asserts the close happens before `purgeStorage` is invoked (e.g. via an ordering probe in the `deps` fakes).
- For a PostgreSQL release, `db/` is absent and its removal is a no-op (`os.RemoveAll` on an absent path is nil) — no backend branch exists in the code.

## C-TEARDOWN-4 — Unreachable database on the wrong node (FR-012) **[go]/[host]**

- Given a SQLite purge whose must-exist open fails (no database on this node), `lightstack cleanup` takes the existing "database unreachable" path: it warns naming the path, purges the node's host state, and exits 0. It never creates the file.
- The uninstall completes; VMs on the correct node keep running (host).

## C-TEARDOWN-5 — Retention policies (FR-013, FR-014) **[host]**

- `dataRetentionPolicy: delete`: after uninstall, the release's VMs are gone and `<hostStoragePath>/db` (with `lightstack.db`, `-wal`, `-shm`) is removed, `db/` last.
- `dataRetentionPolicy: retain`: after uninstall, nothing ran and `<hostStoragePath>/db` is intact; a reinstall on the same node opens the database and resumes (replaying any `-wal`/`-shm`).
- The three db files are never moved or copied individually.
