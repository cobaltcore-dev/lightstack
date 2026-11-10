# Phase 0 Research: SQLite backend, part 2 — Helm chart and Docker Compose

**Feature**: `029-sqlite-chart-compose` · **Date**: 2026-10-28

The spec carried no `NEEDS CLARIFICATION` markers (the `/cl.clarify` pass closed the three it had). This document records the design decisions that turn each functional requirement into chart/compose/Go edits, grounded in the current sources. No new Go modules, no schema change, no migration.

## Inherited from part 1 (SPEC-028) — not re-implemented here

Verified present on this branch:

- `pkg/paths.DBDir()` → `<state_path>/db` (`pkg/paths/paths.go:61`). Wired into nothing yet.
- Binary refuses a URL whose scheme disagrees with `LIGHTSTACK_DB_BACKEND` (`database.CheckDeclaredBackend`, called from `cmd/lightstack/main.go:591` and `cmd/lightstack-migrate/main.go:38`).
- Binary refuses `sqlite://` with `neutron.vxlan_enabled` (`cmd/lightstack/main.go:594`).
- `lightstack cleanup` opens the database `MustExist: true` (`internal/teardown/teardown.go:86`), so a missing SQLite file is "unreachable", not created.
- Chart already sets `LIGHTSTACK_NODE_NAME` from `spec.nodeName` (`_helpers.tpl:323-327`) and feeds the purge Job `LIGHTSTACK_DB_URL` from `database.existingSecret` via `lightstack.dbUrlEnv` (`_helpers.tpl:187-201`, used at `teardown-purge-job.yaml:36-39`).
- `deployments/docker-compose-kvm.yml` already runs either backend (state root bind + `LIGHTSTACK_DB_URL` pass-through).

## R1 — Backend selection in the chart (FR-001, FR-003)

**Decision**: A helper `lightstack.dbBackend` returns `sqlite` when `.Values.database.url` begins with the `sqlite:` scheme, `postgres` otherwise; a companion `lightstack.sqliteEnabled` renders truthy for the SQLite case. Selection reads **only** `database.url` — the one place the chart can see the scheme.

**Rationale**: The ticket fixes this: "SQLite is selected only through `database.url` in values, because the chart has to see the scheme to render the storage." A `sqlite://` URL in a Secret is invisible to the chart and is handled by the declared-backend env + the binary (R7).

**Alternatives**: A separate `database.backend` toggle or a `sqlite.enabled` flag — rejected; the ticket and SPEC-028 both key the backend off the URL scheme, and a second source could disagree with the URL.

## R2 — Deriving the database directory from the URL (FR-004)

**Decision**: `trimPrefix "sqlite://" .Values.database.url` yields the absolute file path (`sqlite:///var/lib/lightstack/db/lightstack.db` → `/var/lib/lightstack/db/lightstack.db`); Sprig `dir` yields the mounted directory (`/var/lib/lightstack/db`). The hostPath volume mounts that directory with `type: DirectoryOrCreate`, so `lightstack.db`, `-wal` and `-shm` share it.

**Rationale**: WAL mode keeps two sidecar files beside the database; mounting the file would not expose them. SPEC-028 FR-019/021 and the ticket both require the directory, not the file.

**Alternatives**: Mount the file — rejected (sidecars invisible). Hard-code `<hostStoragePath>/db` — rejected; deriving from the URL keeps the mount and the URL in agreement and supports a non-default file name.

## R3 — Path-under-storage-root validation (FR-002)

**Decision**: Fail at render time unless the file path is under `realMode.hostStoragePath`, tested with `hasPrefix (printf "%s/" $root) $path` — the trailing slash makes `/var/lib/lightstackEVIL/x.db` fail against root `/var/lib/lightstack`.

**Rationale**: Prevents the chart mounting an arbitrary host path. The boundary slash avoids the classic prefix false-positive.

**Alternatives**: `hasPrefix $root $path` without the slash — rejected (prefix-boundary bug). Regex — unnecessary.

## R4 — Node-pin detection (FR-008)

**Decision**: SQLite requires a node pin — fail unless `.Values.nodeSelector` is non-empty **or** `.Values.affinity.nodeAffinity` is set. A `nodeSelector`/`nodeAffinity` binds the pod to a node; `podAffinity`/`podAntiAffinity` alone does not and does not satisfy the gate.

**Rationale**: A rescheduled pod finds an empty directory and SQLite creates a fresh database without error (`modernc` opens create-if-missing). The pin is the only thing keeping the data on one node.

**Alternatives**: Accept any non-empty `affinity` — rejected; a pod-anti-affinity-only value would pass but not pin. Require exactly `nodeSelector` — rejected; the ticket allows "a `nodeSelector` or node affinity".

## R5 — Single-instance refusals (FR-009)

**Decision**: Fail at render time when `deploymentMode != "single-node"` or `int replicaCount > 1`. (Real mode already forces one replica in `deployment.yaml:11`; the `replicaCount` gate catches stub mode, where it is honoured.)

**Rationale**: SQLite serves one writer; multi-node renders a DaemonSet and more than one replica opens more than one database.

## R6 — SQLite + Secret-based config refusal (FR-003a)

**Decision**: Fail at render time when the backend is SQLite and either `database.existingSecret` or `secrets.existingSecret` is set, naming the conflict.

**Rationale**: A Secret-injected `LIGHTSTACK_DB_URL` overrides the config at runtime, so a `sqlite://` `database.url` combined with a Secret would render SQLite storage for a URL the Secret replaces — self-contradictory. Clarified decision: fail early rather than leave the binary to refuse at startup.

## R7 — The declared-backend env (FR-003)

**Decision**: A helper `lightstack.dbBackendEnv` emits `- name: LIGHTSTACK_DB_BACKEND` / `value: <backend>` and is included on the workload container env (`_helpers.tpl` ~L319) and the purge Job container env (`teardown-purge-job.yaml` ~L36), beside `lightstack.dbUrlEnv`. It is **always** set — `sqlite` only when the chart rendered SQLite storage, `postgres` otherwise.

**Rationale**: The acceptance criterion is "every rendered container carries `LIGHTSTACK_DB_BACKEND`." Always setting `postgres` for a non-SQLite render means a `sqlite://` URL smuggled through a Secret produces a scheme mismatch the binary refuses (SPEC-028 FR-004). The quiesce Job runs `kubectl`, not the binary, and carries nothing.

## R8 — Deployment strategy (FR-007)

**Decision**: `deployment.yaml:12-15` renders `strategy: Recreate` when `realMode.enabled` **or** SQLite is selected. PostgreSQL stub mode keeps the default `RollingUpdate`.

**Rationale**: Two pods must never open one SQLite file; Recreate stops the old pod before the new one starts. Real mode already does this for its host ports.

## R9 — Stub-mode SQLite hostPath mounts (FR-004, FR-011)

**Decision**: Mounts differ by pod, because their needs differ:
- **Workload** (stub SQLite): a hostPath volume+mount **scoped to the database directory only** (`dir` of the URL path, `type: DirectoryOrCreate`) — least privilege; the workload reads and writes only its database.
- **Purge Job** (stub SQLite): the **whole state root** (`realMode.hostStoragePath`, `type: DirectoryOrCreate`), the same `host-storage` mount real mode uses. `lightstack cleanup` runs `os.RemoveAll(paths.DBDir())`; when `db/` is itself the mountpoint, `RemoveAll` empties it but cannot unlink the directory (EBUSY), so the purge fails non-zero even though the files are gone. Mounting the state root makes `db/` a removable subdirectory. The purge needs the state root regardless, since it removes every node-local tree under it.
- **Real mode** (both pods): unchanged — the database directory already sits inside the existing `host-storage` mount of the whole state root.

**Rationale**: least privilege where it is safe (the workload), the state root where the operation requires it (the purge's `RemoveAll`).

**History**: the clarify pass first scoped **both** pods to the narrow db directory; host testing (SPEC-029, 2026-10-28) showed the purge then fails — `db/ … total 0` (files removed) with the Job reporting `BackoffLimitExceeded`. Corrected to mount the state root in the purge Job.

## R10 — init-host-storage mkdir list (FR-004)

**Decision**: Add `"{{ $root }}/db"` to the `init-host-storage` mkdir list (`_helpers.tpl:295-302`). Real mode only (that initContainer runs only in real mode). Stub mode relies on `DirectoryOrCreate` plus the binary's `MkdirAll` (SPEC-028 FR-011) to create `db/`.

**Rationale**: Keeps the pre-created subdir list a mirror of `pkg/paths/paths.go`, which now includes `DBDir()`.

## R11 — Shared scheduling helper (FR-010)

**Decision**: Extract the pod-scheduling block — the two-branch `nodeSelector` (multi-node label merge + `.Values.nodeSelector`; single-node `.Values.nodeSelector`), `tolerations`, and `affinity` — out of `lightstack.podSpec` into a new `lightstack.scheduling` helper. `podSpec` includes it (producing byte-identical output), and the purge Job includes it too. A helm-unittest case asserts the purge Job's `nodeSelector`/`affinity`/`tolerations` equal the Deployment's.

**Rationale**: The purge Job carries no node pin today, so a SQLite purge could land on a node without the database. Moving the block into a shared helper pins both from one source, for both backends. For PostgreSQL this is a harmless correctness fix (the ticket requires it regardless).

**Risk**: the helper must reproduce the existing `nodeSelector` two-branch logic exactly, including the `multiNode.nodeSelectorLabel` split, so PostgreSQL releases render unchanged (FR-017). The helm-unittest PostgreSQL-parity cases guard this.

## R12 — Teardown ordering for `db/` removal (FR-013)

**Decision**: Add `paths.DBDir()` to the `purgeLocalStorage` slice (`internal/teardown/teardown.go:208-210`) as the **last** element, and close the enumeration database connection **before** the storage purge runs. Implementation: `deps` gains `closeDB func() error`; `Run` assigns it the pool's `Close` (guarded to run once) instead of the current `defer pool.Close()`; `execute` calls `closeDB` after the network-purge step and before the storage-purge step.

**Rationale**: The network purge (`neutron.PurgeRealModeNetworkState`) runs queries on the pool and executes *before* storage purge in `execute`, so the pool cannot close until after the network step. `db/` must be removed only after the connection closes, or SQLite's `-wal`/`-shm` would be unlinked from an open connection. For PostgreSQL the `db/` directory is absent and `os.RemoveAll` on an absent path is nil (existing comment) — so no backend branch is needed; only the ordering changes. A deferred `closeDB` (via `sync.Once`) remains as a safety net if `execute` is not reached.

**Alternatives**: Remove `db/` outside `purgeLocalStorage` as a separate final step — rejected; the ticket puts `db/` in the fixed subdir list. Keep `defer pool.Close()` and append `DBDir()` without reordering — rejected; that removes `db/` while the pool is open.

## R13 — Docker Compose SQLite variant (FR-015)

**Decision**: New file `deployments/docker-compose-sqlite.yml`: no `postgres` service and no `depends_on: postgres`; Redis kept as the default file has it; a named volume (e.g. `lightstack_db`) mounted at the database directory (`/var/lib/lightstack/db`); `LIGHTSTACK_DB_URL: "sqlite:///var/lib/lightstack/db/lightstack.db"`. `LIGHTSTACK_DB_BACKEND` left unset (SPEC-028: unset means no check). The default `deployments/docker-compose.yml` is untouched.

**Rationale**: The ticket: "A SQLite variant of `deployments/docker-compose.yml`: no postgres service, the database file on a named volume. The PostgreSQL file stays the default." The real-mode KVM stack already handles both backends (SPEC-028 FR-042), so only the stub-mode file is added.

**Alternatives**: Parameterise the existing file with a profile — rejected; the ticket asks for a separate variant and keeping the default PostgreSQL file unchanged is a stated acceptance criterion.

## R14 — Documentation and values comments (FR-019, FR-019a)

**Decision**: `docs/KUBERNETES_DEPLOYMENT.md` gains a SQLite-on-k3s section (URL, required node pin, hostPath, no PVC, `Recreate`, delete/retain behaviour); `docs/DOCKER_DEPLOYMENT.md` documents the SQLite Compose variant; `charts/lightstack/values.yaml` comments document the SQLite URL form and the single-node constraints. Both the docs and the `values.yaml` comment carry the **local-POSIX-filesystem** warning: `hostStoragePath` must not be NFS or CephFS, whose locking can corrupt SQLite.

**Rationale**: FR-019/019a. The filesystem constraint (from #595's verified risks) cannot be enforced at render time, so the warning is the mitigation.

## Constitution alignment

- **Article IV (Multi-mode)**: SQLite storage renders in both stub and real mode — satisfied by R8/R9/R10.
- **Article VI (Single binary / SQLite single-node)**: this feature delivers the chart guards (R4, R5) that enforce it, matching the amended Article VI and the binary's own VXLAN refusal.
- **No new transactions**; the one Go change (R12) only reorders an existing close relative to an existing purge — Articles II and III unaffected.
- **Quality gate "both backends exercised"**: met by the helm-unittest SQLite + PostgreSQL cases (R1–R11) and the real-mode k3s SQLite run. No SQL or schema change, so the migration gates are N/A.
