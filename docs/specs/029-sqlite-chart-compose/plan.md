# Implementation Plan: SQLite backend, part 2 — Helm chart and Docker Compose

**Branch**: `feature/599` (feature id: `029-sqlite-chart-compose`) | **Date**: 2026-10-28 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `docs/specs/029-sqlite-chart-compose/spec.md` (cloud-storage#599, part 2 of #595)

## Summary

Part 1 (SPEC-028) made the binary speak SQLite; this part makes the Helm chart and Docker Compose deploy it. Everything here is chart templating, one new Compose file, a one-function Go change, and docs — no new Go modules, no chart dependencies, no schema change, no migration, no API change.

**Chart — SQLite storage.** The chart reads the backend from `database.url`'s scheme (R1). For a `sqlite://` URL it: mounts the database's directory (derived from the URL, R2) as a hostPath `DirectoryOrCreate` volume — scoped to `db/` in stub mode, inside the existing `host-storage` mount in real mode (R9); adds `db` to the real-mode `init-host-storage` mkdir list (R10); renders `strategy: Recreate` in both modes (R8); and sets `LIGHTSTACK_DB_BACKEND` on every binary container, always — `sqlite` here, `postgres` otherwise (R7), so a `sqlite://` URL hidden in a Secret is refused by the binary.

**Chart — render-time guards.** `lightstack.validate` gains four SQLite refusals: a path outside `realMode.hostStoragePath` (R3), no node pin (R4), a non-single-node mode or `replicaCount > 1` (R5), and a Secret-based config combined with a SQLite `database.url` (R6).

**Chart — teardown.** The pod-scheduling block (`nodeSelector`/`affinity`/`tolerations`) moves into a shared `lightstack.scheduling` helper included by `lightstack.podSpec` and by the purge Job, so the purge runs on the SQLite node — for both backends (R11). The purge Job gains the SQLite db-dir mount and the declared-backend env.

**Go — one change.** `purgeLocalStorage` gains `paths.DBDir()` as its last entry, and the enumeration database connection is closed between the network-purge and storage-purge steps of `execute`, so `db/` (with `-wal`/`-shm`) is removed last and only after the connection closes (R12). PostgreSQL has no `db/`, so no branch is needed.

**Docker Compose.** A new `deployments/docker-compose-sqlite.yml` runs the stub stack with no postgres service, the database on a named volume, and a `sqlite://` URL; the default PostgreSQL file is untouched (R13).

**Tests and docs.** helm-unittest cases for the SQLite render and the PostgreSQL-unchanged render, a Go unit test for the teardown ordering, a real-mode single-node k3s SQLite host run, and the docs/values comments including the local-POSIX-filesystem warning (R14).

## Technical Context

**Language/Version**: Helm 3.12+ chart (Go `text/template` + Sprig); JSON Schema **draft-07** (`values.schema.json`); Go 1.27.3 for the one `internal/teardown` change; Markdown docs.
**Primary Dependencies**:
- **Chart**: `charts/lightstack` (existing), the **helm-unittest** plugin (existing harness) — no new chart dependencies.
- **Go**: `internal/teardown` and the existing `pkg/paths.DBDir()` (added by SPEC-028). **No new Go modules.**
- **Compose**: Docker Compose; the real-mode `docker-compose-kvm.yml` already runs both backends (SPEC-028 FR-042).
**Storage**: node-local hostPath SQLite directory at `<realMode.hostStoragePath>/db` (`lightstack.db` + `-wal` + `-shm`), **no PVC**. External PostgreSQL unchanged. **No schema change, no migration, no persisted data owned by the chart.**
**Testing**: helm-unittest (SQLite render + PostgreSQL-unchanged render, both backends); a Go unit test for `purgeLocalStorage` ordering and `DBDir()` inclusion; `helm template`/render-failure assertions for the four refusals; a real-mode single-node k3s SQLite host run; a Docker Compose smoke check of the SQLite file.
**Target Platform**: single-node k3s (Helm) and Docker Compose. The SQLite hostPath MUST be a local POSIX filesystem (not NFS/CephFS — FR-019a).
**Project Type**: deployment/packaging (Helm chart + Docker Compose) with one supporting Go change.
**Performance Goals**: not a throughput feature. A `helm upgrade` with `Recreate` incurs a brief API outage while the old pod stops before the new one opens the database — accepted (as real mode already is).
**Constraints**:
- PostgreSQL releases render unchanged beyond the added `LIGHTSTACK_DB_BACKEND=postgres` env and the shared scheduling helper (FR-017);
- no API change; no schema change; no migration; no new Go modules or chart dependencies;
- SQLite is single-node only — the chart refuses multi-node, `replicaCount > 1`, no node pin, a path outside the storage root, and a Secret-based config (FR-002/003a/008/009);
- the spec and this plan cover this branch's chart/compose/teardown changes only; the binary-side guards come from SPEC-028.
**Scale/Scope**: ~5 chart template/values/schema files edited; 1 Go function changed (plus its `deps`/`Run`/`execute` wiring); 1 new Compose file; 2 new helm-unittest suites (`sqlite_storage`, `sqlite_refusals`) plus edits to ~4 existing suites; 2 docs files + `values.yaml` comments.

No `NEEDS CLARIFICATION` remains: the spec's Clarifications session (2026-10-28) closed the three open points, and research.md R1–R14 settle every design choice.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design (below the table).*

| Principle (`.specify/memory/constitution.md` v1.3.0) | Verdict | Why |
|---|---|---|
| I. OpenStack Flamingo API compatibility | PASS | No API surface is touched. The chart and Compose only change how the same binary is deployed. |
| II. Synchronous operations | PASS | No new transaction. The one Go change reorders an existing connection `Close` relative to an existing best-effort purge; it opens nothing new. |
| III. Fail-fast | PASS | No retry, no backoff. Render-time refusals fail the install immediately; `lightstack cleanup`'s must-exist open (SPEC-028) still fails fast on a missing file. |
| IV. Multi-mode | PASS | SQLite storage renders in **both** stub and real mode (R8/R9/R10). The purge and the Compose variant cover stub mode. |
| V. Project isolation | PASS | Unchanged — no query or scoping change. |
| VI. Single binary, SQLite single-node | PASS (v1.3.0) | This feature delivers the chart guards that enforce Article VI's "SQLite serves a single node": it refuses multi-node, extra replicas, and an unpinned pod (FR-008/009), matching the binary's own VXLAN refusal. |
| Technology constraints | PASS | No new dependency of any kind. Helm 3.12+, helm-unittest and Docker Compose are the existing tooling. |
| Forbidden patterns | PASS | No queue, worker, state machine, service discovery, or required orchestration (the chart is optional; Compose and a bare binary still run). |
| Test-first | PASS by plan | RED first: helm-unittest cases for the SQLite render and the four refusals (which pass today only because the templates don't yet branch), and the Go ordering test (`db/` is not purged today), written before the templates/code. |
| Backwards compatibility | PASS | PostgreSQL releases render unchanged (FR-017), guarded by PostgreSQL-parity helm-unittest cases. The default `docker-compose.yml` is untouched. No migration. |
| Security | PASS | The container already runs as root and the file mode (0600) is the binary's (SPEC-028 FR-011a). The stub-mode hostPath is scoped to `db/` only — least privilege (R9). The accepted drawback (PSA `baseline`/`restricted` rejects hostPath) is recorded in the spec. |
| Quality gates before merge | PASS by plan | Both backends exercised in helm-unittest; real-mode k3s SQLite host run; Docker Compose SQLite smoke; no schema/SQL change, so the migration gates are N/A; no secrets added. |

**Post-design re-check (after Phase 1)**: unchanged. The design introduces no new abstraction, no new dependency and no API change. **No deviations** — Complexity Tracking is empty. (Unlike part 1, this part needs no ADR: it consumes the SPEC-028/ADR-003 seam rather than extending it.)

## Project Structure

### Documentation (this feature)

```text
docs/specs/029-sqlite-chart-compose/
├── spec.md
├── plan.md                 # this file
├── research.md             # Phase 0: R1–R14
├── data-model.md           # Phase 1: chart value/env/volume surface; no DB entities
├── quickstart.md           # Phase 1: deploy & verify SQLite on k3s and Compose; PostgreSQL unchanged
├── contracts/
│   ├── chart-render.md      # what renders for SQLite vs PostgreSQL; the four render-time refusals
│   ├── teardown-retention.md# purge-Job scheduling parity; db/ removal order; delete/retain
│   └── docker-compose.md    # the SQLite Compose variant; default file untouched
├── checklists/requirements.md
└── tasks.md                # Phase 2 (/cl.tasks — not created here)
```

### Source Code (repository root)

```text
charts/lightstack/
├── templates/
│   ├── _helpers.tpl              # + lightstack.dbBackend / sqliteEnabled / dbBackendEnv / scheduling
│   │                             #   helpers; SQLite gates in lightstack.validate; db-dir volume+mount
│   │                             #   in lightstack.podSpec; "db" in the init-host-storage mkdir list
│   ├── deployment.yaml           # strategy: Recreate when realMode OR sqlite (L12-15)
│   └── teardown-purge-job.yaml   # include lightstack.scheduling; db-dir mount; LIGHTSTACK_DB_BACKEND
│   (daemonset.yaml: no change — SQLite is refused in multi-node)
├── values.yaml                   # database.url comment: sqlite form + single-node + local-FS warning
├── values.schema.json            # database.url description widened to document sqlite:// (no scheme reject)
└── tests/
    ├── sqlite_storage_test.yaml      # NEW: volume/mount, DB_BACKEND, strategy, db mkdir, no runAsUser
    ├── sqlite_refusals_test.yaml     # NEW: the five render-time refusals + node-pin-OK
    ├── teardown_test.yaml            # + purge-Job scheduling == Deployment; db-dir mount
    ├── init_host_storage_test.yaml   # + db subdir present (real mode)
    ├── config_render_test.yaml       # + DB_BACKEND env; PostgreSQL render unchanged
    └── db_existingsecret_test.yaml   # + sqlite-url + existingSecret refused

internal/teardown/
└── teardown.go                   # purgeLocalStorage += paths.DBDir() (last); close DB before storage purge

pkg/paths/paths.go                # unchanged — DBDir() already present (SPEC-028)

deployments/
├── docker-compose-sqlite.yml     # NEW: stub stack, no postgres, DB on a named volume, sqlite:// URL
└── docker-compose.yml            # unchanged (PostgreSQL default)

docs/
├── KUBERNETES_DEPLOYMENT.md      # + SQLite on k3s: node pin, hostPath, no PVC, Recreate, delete/retain, local-FS
└── DOCKER_DEPLOYMENT.md          # + the SQLite Compose variant
```

**Structure Decision**: This is a packaging feature. The weight is in `charts/lightstack/templates/_helpers.tpl` (helpers + validate gates + podSpec storage) and its `tests/`, with one Go function in `internal/teardown` and one new Compose file. No `src/` layout applies; the real directories are listed above.

## Delivery sequence

1. **Baseline gates** — record `helm-unittest` and `go test ./internal/teardown/...` green on `main` so the PostgreSQL-unchanged claim (FR-017) has an oracle.
2. **RED tests** — write the helm-unittest cases (SQLite render, four refusals, purge-scheduling parity, PostgreSQL-unchanged) and the Go `purgeLocalStorage` ordering test; confirm they fail.
3. **Chart helpers** — `lightstack.dbBackend` / `sqliteEnabled` / `dbBackendEnv` / `scheduling`; wire `dbBackendEnv` into the workload and purge containers; wire `scheduling` into `podSpec` and the purge Job.
4. **Chart storage** — `podSpec` db-dir volume/mount (stub narrow, real via host-storage); `db` in the mkdir list; `deployment.yaml` strategy.
5. **Chart guards** — the four SQLite refusals in `lightstack.validate`.
6. **Go teardown** — `DBDir()` in `purgeLocalStorage` (last) + close-before-storage-purge ordering.
7. **Compose** — `docker-compose-sqlite.yml`.
8. **Schema/values/docs** — `values.schema.json` description; `values.yaml` comments; `KUBERNETES_DEPLOYMENT.md`, `DOCKER_DEPLOYMENT.md`, with the local-FS warning.
9. **Green + host run** — helm-unittest and Go tests green on both backends; real-mode single-node k3s SQLite run (restart, upgrade, delete/retain); Compose SQLite smoke.

## Complexity Tracking

> No constitution deviations. This feature adds no abstraction, no dependency and no API change; it consumes the SPEC-028 seam. Table intentionally empty.
