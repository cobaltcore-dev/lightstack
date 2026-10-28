---
description: "Task list — SQLite backend, part 2: Helm chart and Docker Compose"
---

# Tasks: SQLite backend, part 2 — Helm chart and Docker Compose

**Input**: Design documents from `docs/specs/029-sqlite-chart-compose/`
**Prerequisites**: plan.md, spec.md, research.md (R1–R14), data-model.md, contracts/ (chart-render, teardown-retention, docker-compose)
**Branch**: `feature/599` (feature id `029-sqlite-chart-compose`)

**Tests**: INCLUDED. The spec and plan mandate test-first (Constitution Test-First; plan delivery step 2): helm-unittest and Go tests are written first and must fail (RED) before the templates/code that make them pass.

**Scope note**: This is a cohesive chart/compose change, not five independent features. The "user stories" share `charts/lightstack/templates/_helpers.tpl`, so their implementation tasks are partly sequential. The shared chart helpers are foundational (Phase 2). Part 1 (SPEC-028) already delivered the binary guards, `pkg/paths.DBDir()`, `LIGHTSTACK_NODE_NAME`, the purge-Job `LIGHTSTACK_DB_URL`, and the both-backend `docker-compose-kvm.yml` — none is re-done here.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different file, no dependency on an incomplete task)
- **[Story]**: US1–US5 (user-story phases only)

---

## Phase 1: Setup (baseline oracle)

**Purpose**: Capture the pre-change gates so "PostgreSQL renders unchanged" (FR-017) has an oracle.

- [X] T001 Record baseline gates in `docs/specs/029-sqlite-chart-compose/baseline-gates.md`: run `helm unittest charts/lightstack` and `go test ./internal/teardown/...` on the current tree and capture PASS output, plus `helm template charts/lightstack` for representative PostgreSQL values (stub + real) saved as the parity reference.

**Checkpoint**: Baseline recorded — refactors below are diffed against it.

---

## Phase 2: Foundational (shared chart helpers — BLOCKS US1/US3/US4/US5)

**Purpose**: The reusable helpers every story builds on. All edits are in `charts/lightstack/templates/_helpers.tpl` (sequential — same file). No standalone output, so these are exercised through the story tests and the T001 parity reference, not a dedicated test.

**⚠️ CRITICAL**: No user-story implementation can begin until this phase is complete.

- [X] T002 Add `lightstack.dbBackend` and `lightstack.sqliteEnabled` helpers to `charts/lightstack/templates/_helpers.tpl` — `dbBackend` returns `sqlite` when `.Values.database.url` has the `sqlite:` scheme (`hasPrefix "sqlite:"`), else `postgres`; `sqliteEnabled` renders truthy for the SQLite case (R1).
- [X] T003 Add `lightstack.dbBackendEnv` helper to `charts/lightstack/templates/_helpers.tpl` — emits `- name: LIGHTSTACK_DB_BACKEND` / `value: <lightstack.dbBackend>` (R7).
- [X] T004 Extract the pod-scheduling block (the two-branch `nodeSelector` incl. the multi-node `multiNode.nodeSelectorLabel` merge, plus `tolerations` and `affinity`) from `lightstack.podSpec` into a new `lightstack.scheduling` helper in `charts/lightstack/templates/_helpers.tpl`, and include it from `lightstack.podSpec`; output MUST be byte-identical to today (verified against T001 and by US3) (R11).

**Checkpoint**: Helpers exist; `helm template` for any values still renders as the baseline (T001) except nothing yet (no wiring). User stories can begin.

---

## Phase 3: User Story 1 — Install SQLite via the Helm chart on k3s (Priority: P1) 🎯 MVP

**Goal**: A `sqlite://` `database.url` + a node pin renders a hostPath-backed, PVC-less, `Recreate` SQLite release that migrates, starts and serves.

**Independent Test**: `helm template` SQLite values → a hostPath db-dir volume (`DirectoryOrCreate`), no PVC, `strategy: Recreate`, `LIGHTSTACK_DB_BACKEND=sqlite`; then install on single-node k3s and confirm it serves (T026).

### Tests (write FIRST — must FAIL)

- [X] T005 [P] [US1] New helm-unittest suite `charts/lightstack/tests/sqlite_storage_test.yaml` per contract **chart-render** C-RENDER-1/2/3: workload env has `LIGHTSTACK_DB_BACKEND=sqlite`; stub mode adds a hostPath volume `path: /var/lib/lightstack/db`, `type: DirectoryOrCreate`, mounted on the `lightstack` container; no `PersistentVolumeClaim`/`volumeClaimTemplates`; a custom `realMode.hostStoragePath` moves the mount and mkdir entry; real mode adds `/var/lib/lightstack/db` to the `init-host-storage` mkdir list and adds no second volume; `strategy.type: Recreate` for SQLite in stub and real mode; and the `lightstack` container sets no `securityContext.runAsUser` — it runs as root (FR-005).

### Implementation

- [X] T006 [US1] Wire `include "lightstack.dbBackendEnv" .` into the `lightstack` container env in `lightstack.podSpec` (`charts/lightstack/templates/_helpers.tpl`, beside `lightstack.dbUrlEnv`).
- [X] T007 [US1] In `lightstack.podSpec` (`charts/lightstack/templates/_helpers.tpl`): when `lightstack.sqliteEnabled` and NOT `realMode.enabled`, add a hostPath volume (`dir` of the URL path, `type: DirectoryOrCreate`) and a matching container `volumeMount` scoped to the db directory only (R2/R9); in real mode add nothing (the directory is already inside `host-storage`).
- [X] T008 [US1] Add `"{{ .Values.realMode.hostStoragePath }}/db"` to the `init-host-storage` mkdir list in `lightstack.podSpec` (`charts/lightstack/templates/_helpers.tpl`) so the real-mode subdir list mirrors `pkg/paths/paths.go` (R10).
- [X] T009 [P] [US1] In `charts/lightstack/templates/deployment.yaml` render the `strategy: Recreate` block when `.Values.realMode.enabled` **or** `include "lightstack.sqliteEnabled" .` (lines 12-15); PostgreSQL stub keeps the default (R8).

**Checkpoint**: T005 passes. SQLite renders correct storage, strategy and backend env.

---

## Phase 4: User Story 2 — Run SQLite via Docker Compose (Priority: P1)

**Goal**: A stub-mode Compose file runs Lightstack on SQLite with no postgres service; the default file is untouched.

**Independent Test**: `docker compose -f deployments/docker-compose-sqlite.yml config` is valid and has no postgres service; `docker-compose.yml` is unchanged (contract **docker-compose**).

### Implementation

- [X] T010 [P] [US2] Create `deployments/docker-compose-sqlite.yml` per contract **docker-compose** C-COMPOSE-1: no `postgres` service and no `depends_on: postgres`; `LIGHTSTACK_DB_URL: "sqlite:///var/lib/lightstack/db/lightstack.db"`; a named volume (e.g. `lightstack_db`) mounted at `/var/lib/lightstack/db`; Redis kept as in the default file; `LIGHTSTACK_DB_BACKEND` left unset.

### Verification

- [X] T011 [US2] Verify `docker compose -f deployments/docker-compose-sqlite.yml config` parses and shows no postgres service, and that `git diff --quiet -- deployments/docker-compose.yml` (default file unchanged, C-COMPOSE-2).

**Checkpoint**: The SQLite Compose file is valid; the PostgreSQL default is byte-unchanged.

---

## Phase 5: User Story 3 — PostgreSQL deployments render exactly as before (Priority: P1)

**Goal**: The chart renders PostgreSQL releases identically, save the added `LIGHTSTACK_DB_BACKEND=postgres` env and the scheduling emitted through the shared helper.

**Independent Test**: helm-unittest PostgreSQL cases pass and `helm template` for PostgreSQL values diffs against the T001 reference with only the DB_BACKEND env added (contract **chart-render** C-RENDER-5).

### Tests (write FIRST — must FAIL for the new assertions)

- [X] T012 [P] [US3] Extend `charts/lightstack/tests/config_render_test.yaml` (and/or `db_existingsecret_test.yaml`) per C-RENDER-5: PostgreSQL values (`postgres://`, and `database.existingSecret`) render `LIGHTSTACK_DB_BACKEND=postgres` on the workload and purge containers; PostgreSQL stub has no `strategy` block; none of the SQLite refusals fires for PostgreSQL with `deploymentMode: multi-node`, `replicaCount: 3`, no node pin, or a `database.existingSecret`.

### Verification

- [X] T013 [US3] Diff `helm template charts/lightstack` for the T001 PostgreSQL reference values (stub + real) against the recorded baseline; confirm the only differences are the added `LIGHTSTACK_DB_BACKEND=postgres` env (scheduling via `lightstack.scheduling` is byte-identical). Record the diff summary in `baseline-gates.md`.

**Checkpoint**: PostgreSQL render parity proven (FR-017).

---

## Phase 6: User Story 4 — The chart refuses unsafe SQLite configurations (Priority: P2)

**Goal**: SQLite combined with a bad path, no node pin, multi-node, extra replicas, or a Secret-based config fails at render time.

**Independent Test**: `helm template` each refused configuration → render failure naming the problem (contract **chart-render** C-RENDER-4/6).

### Tests (write FIRST — must FAIL)

- [X] T014 [P] [US4] New helm-unittest suite `charts/lightstack/tests/sqlite_refusals_test.yaml` per C-RENDER-4/6 (template `templates/service.yaml`, which invokes `lightstack.validate`): `failedTemplate` with a message naming the problem for each of — path outside `realMode.hostStoragePath`; `nodeSelector: {}` and `affinity: {}`; `deploymentMode: multi-node`; `replicaCount: 2`; `database.existingSecret` set; `secrets.existingSecret` set. Plus: a non-empty `nodeSelector` renders OK, and `nodeSelector: {}` with an `affinity.nodeAffinity` renders OK.

### Implementation

- [X] T015 [US4] Add the SQLite render-time refusals to `lightstack.validate` in `charts/lightstack/templates/_helpers.tpl` (gated on `lightstack.sqliteEnabled`, outside the `secrets.existingSecret` branch): FR-002 path under `realMode.hostStoragePath` via `hasPrefix (printf "%s/" $root) $path` (R3); FR-008 node pin — non-empty `nodeSelector` or `affinity.nodeAffinity` (R4); FR-009 `deploymentMode == single-node` and `replicaCount <= 1` (R5); FR-003a refuse when `database.existingSecret` or `secrets.existingSecret` is set (R6). Each `fail` names the conflict.

**Checkpoint**: T014 passes; all five refusals fire, node-pinned renders succeed.

---

## Phase 7: User Story 5 — Uninstall and retention protect the SQLite database (Priority: P2)

**Goal**: The purge runs on the SQLite node (parity with the Deployment), reads the database, and removes `db/` last; retain leaves it; a reinstall resumes.

**Independent Test**: helm-unittest shows the purge Job's scheduling equals the Deployment's and it mounts the db dir; the Go test shows `db/` purged last after the connection closes; the host run shows delete/retain behaviour (contract **teardown-retention**).

### Tests (write FIRST — must FAIL)

- [X] T016 [P] [US5] Extend `charts/lightstack/tests/teardown_test.yaml` per C-TEARDOWN-1/2: the purge Job's `nodeSelector`/`affinity`/`tolerations` equal the Deployment's for the same values on both backends (incl. a set `nodeSelector`/`affinity`/`tolerations`, and multi-node PostgreSQL); the purge Job carries `LIGHTSTACK_DB_BACKEND`; stub-mode SQLite purge mounts the db-dir hostPath.
- [X] T017 [P] [US5] Add a Go test in `internal/teardown/teardown_test.go` per C-TEARDOWN-3: `purgeLocalStorage`'s directory list includes `paths.DBDir()` as the **last** entry; and in `execute` the database connection is closed before the storage-purge step runs (assert ordering via the `deps` fakes / a close probe). Also add a case for C-TEARDOWN-4 (FR-012): `Run`/`cleanup` pointed at a missing SQLite file takes the "database unreachable" path — it does not create the file, runs host-only cleanup best-effort, and returns success (exit 0).

### Implementation

- [X] T018 [P] [US5] In `charts/lightstack/templates/teardown-purge-job.yaml`: include `lightstack.scheduling` in the pod spec; add `include "lightstack.dbBackendEnv" .` to the purge container env; and for stub-mode SQLite add the db-dir hostPath volume + container mount (real mode keeps `host-storage`) (FR-010/011).
- [X] T019 [P] [US5] In `internal/teardown/teardown.go`: append `paths.DBDir()` as the last element of the `purgeLocalStorage` slice (L208-210); add `closeDB func() error` to `deps`; in `Run` set `d.closeDB` to a once-guarded `pool.Close` instead of `defer pool.Close()`; in `execute` call `d.closeDB` after the network-purge step and before the storage-purge step (R12/FR-013). No backend branch — PostgreSQL has no `db/` and `RemoveAll` of an absent path is nil.

**Checkpoint**: T016 and T017 pass; purge is node-pinned and `db/` is removed last after close.

---

## Phase 8: Polish & cross-cutting (docs, schema, values, host validation)

**Purpose**: Documentation, the local-filesystem warning, and the real-host/compose validation.

- [X] T020 [P] Widen the `database.url` description in `charts/lightstack/values.schema.json` (L54-57) to document the `sqlite:///<absolute path>` form; keep the scheme unconstrained (no rejecting pattern) (FR-001).
- [X] T021 [P] Update the `database.url` comments in `charts/lightstack/values.yaml` (L24-27): the SQLite URL form, the single-node constraints (node pin, single-node, one replica, path under `hostStoragePath`), and the warning that `realMode.hostStoragePath` must be a local POSIX filesystem (not NFS/CephFS) (FR-001/019a).
- [X] T022 [P] Update `docs/KUBERNETES_DEPLOYMENT.md`: SQLite on k3s — the SQLite URL, the required node pin, the hostPath (no PVC), `Recreate`, the delete/retain behaviour, and the local-POSIX-filesystem warning (FR-019/019a).
- [X] T023 [P] Update `docs/DOCKER_DEPLOYMENT.md`: the SQLite Compose variant and how to run it (FR-019), including a one-line note that the SQLite database must sit on a local POSIX filesystem — the named volume's host path must not be NFS/CephFS (FR-019a).
- [X] T024 Run the full gates green: `helm unittest charts/lightstack` and `go test ./internal/teardown/...` both pass; confirm no regression vs the T001 baseline beyond the recorded additions (FR-017, SC-006).
- [ ] T025 Real-mode single-node k3s SQLite host run (quickstart A/C): install with a SQLite URL + node pin; verify migrate/start/serve, pod restart, `helm upgrade` (data survives), and `dataRetentionPolicy` delete (VMs + `db/` gone, `db/` last) and retain (db stays, reinstall resumes) (FR-018, SC-001/004/005/007). Also verify SC-003 — a `sqlite://` URL supplied via `database.existingSecret` while `database.url` is `postgres://` makes the pod crash-loop with the binary's scheme-mismatch message (chart renders `LIGHTSTACK_DB_BACKEND=postgres`) — and FR-012 end to end: a purge that cannot reach the database warns, purges host state, and the uninstall still completes.
- [ ] T026 Docker Compose SQLite smoke on a host (quickstart D): `up` → `openstack token issue` → `down` → `up` resumes from the named volume; confirm no postgres container (SC-001).

---

## Dependencies & completion order

- **Setup (T001)** → **Foundational (T002→T003→T004, sequential, same file)** → stories.
- **US1 (T005→{T006→T007→T008 sequential}, T009 [P])**: needs T002–T004. The MVP.
- **US2 (T010→T011)**: independent of the chart; needs only Setup.
- **US3 (T012→T013)**: its assertions depend on US1 (T006, workload DB_BACKEND) and US4 (T015, refusals not firing); run after US1 and US4.
- **US4 (T014→T015)**: needs T002 (`sqliteEnabled`); T015 edits `_helpers.tpl` after US1's `_helpers.tpl` edits (same file — sequential).
- **US5 (T016/T017 [P] → T018/T019 [P])**: needs T003 (`dbBackendEnv`) and T004 (`scheduling`); T018 (yaml) and T019 (Go) are different files.
- **Polish (T020–T023 [P]; T024 after all code; T025/T026 host)**: T024 gates the whole change; T025/T026 need a real host.

```
T001 ─► T002 ─► T003 ─► T004 ─┬─► US1: T005 ─► T006 ─► T007 ─► T008 ; T009[P]
                              ├─► US2: T010 ─► T011                 (independent)
                              ├─► US4: T014 ─► T015
                              ├─► US5: T016[P],T017[P] ─► T018[P],T019[P]
                              └─► US3: T012 ─► T013  (after US1 + US4)
All code ─► T024 ─► T025, T026
```

## Parallel execution examples

- **After Foundational**: start US2 (T010) and US1's test (T005) and US5's tests (T016, T017) together — all different files.
- **Within US1**: T009 (`deployment.yaml`) runs parallel to the `_helpers.tpl` chain T006→T007→T008.
- **Within US5**: T018 (`teardown-purge-job.yaml`) and T019 (`teardown.go`) run in parallel.
- **Polish**: T020–T023 (schema, values, two docs) all parallel.

## Implementation strategy

- **MVP = US1** (+ its Foundational prerequisites): a SQLite release that renders and serves on k3s. Demo with quickstart A.
- **Increment 2**: US2 (Compose) — the local-dev path.
- **Increment 3**: US3 + US4 — prove PostgreSQL is untouched and the guards fire.
- **Increment 4**: US5 — safe uninstall/retention.
- **Close-out**: Polish docs + the host run (T025) and Compose smoke (T026), which exercise SC-001/004/005/007 end to end.

## Format validation

All tasks carry a checkbox, a sequential `Txxx` id, a `[P]` marker where parallelizable, a `[USn]` label in the story phases (none in Setup/Foundational/Polish), and an exact file path. Tests (T005, T012, T014, T016, T017) precede their implementation within each story.
