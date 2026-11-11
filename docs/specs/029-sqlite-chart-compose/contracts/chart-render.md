# Contract: Chart render (SQLite vs PostgreSQL)

**Feature**: `029-sqlite-chart-compose`

The chart's "interface" is what `helm template` produces, and which value combinations it refuses. Each clause below is a helm-unittest assertion. "SQLite values" means `database.url: sqlite:///var/lib/lightstack/db/lightstack.db` plus a node pin unless the clause is a node-pin refusal.

## C-RENDER-1 — Backend selection (FR-001, FR-003)

- Given SQLite values, the workload container env contains `LIGHTSTACK_DB_BACKEND=sqlite`.
- Given PostgreSQL values (`postgres://…`, or a `database.existingSecret`), the workload container env contains `LIGHTSTACK_DB_BACKEND=postgres`.
- The env is present on **both** the workload container and the teardown purge Job container. It is absent from the quiesce Job.

## C-RENDER-2 — SQLite storage volume (FR-004, FR-006)

- Given SQLite values in **stub** mode, the pod has a hostPath volume with `path: /var/lib/lightstack/db` and `type: DirectoryOrCreate`, and the `lightstack` container mounts it. The mount is the db directory, not the whole state root.
- Given SQLite values in **real** mode, no second db volume is added — the database directory is inside the existing `host-storage` mount — and the `init-host-storage` mkdir list contains `/var/lib/lightstack/db`.
- No `PersistentVolumeClaim` is rendered, and no `volumeClaimTemplates`, for either mode.
- A custom `realMode.hostStoragePath` (e.g. `/data/lightstack`) with a matching SQLite URL moves the mount and the mkdir entry accordingly.

## C-RENDER-3 — Strategy (FR-007)

- Given SQLite values in stub mode, the Deployment has `strategy.type: Recreate`.
- Given SQLite values in real mode, `strategy.type: Recreate` (unchanged from today).
- Given PostgreSQL stub values, the Deployment has **no** `strategy` block (default `RollingUpdate`).
- Given PostgreSQL real values, `strategy.type: Recreate` (unchanged).

## C-RENDER-4 — Render-time refusals (FR-002, FR-003a, FR-008, FR-009)

Each renders a `fail` naming the problem:

| Clause | Values | Message names |
|---|---|---|
| C-RENDER-4a | SQLite URL path **not** under `realMode.hostStoragePath` | the path and the storage root |
| C-RENDER-4b | SQLite URL, `nodeSelector: {}` and `affinity: {}` | the required node pin |
| C-RENDER-4c | SQLite URL, `deploymentMode: multi-node` | the deployment mode |
| C-RENDER-4d | SQLite URL, `replicaCount: 2` | the replica count |
| C-RENDER-4e | SQLite URL + `database.existingSecret` **or** `secrets.existingSecret` | the Secret/URL conflict |

## C-RENDER-5 — PostgreSQL unchanged (FR-017)

- Given PostgreSQL values with `deploymentMode: multi-node`, `replicaCount: 3`, no node pin, or a `database.existingSecret`, **none** of the SQLite refusals fires.
- The rendered workload, storage and strategy for PostgreSQL values are identical to the pre-feature baseline **except** the added `LIGHTSTACK_DB_BACKEND=postgres` env and the scheduling emitted through the shared helper (which must be byte-identical to today's inline block).

## C-RENDER-6 — Node-pin satisfied (FR-008)

- Given SQLite values with a non-empty `nodeSelector`, the render succeeds.
- Given SQLite values with `nodeSelector: {}` but an `affinity.nodeAffinity`, the render succeeds.
