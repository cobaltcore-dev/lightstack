# Implementation Plan: Teardown via Helm Pre-Delete Hooks (Clean, No State Leakage)

**Branch**: `010-helm-teardown` (work branch: `feature/53-teardown-via-helm-pre-delete-hooks-clean-no-state-leakage`) | **Date**: 2026-06-15 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/010-helm-teardown/spec.md`
**Tracking issue**: [#53](https://github.com/cobaltcore-dev/lightstack/issues/53)

> **Superseded in part by the 2026-06-24 scope change** (see `spec.md` → Clarifications scope note): the **DB reset was dropped** (database is operator-owned; never reset — `MigrateReset`/`--migrations` references below are obsolete), and the storage/network purge was **widened to the full host inventory** (all `/var/lib/lightstack` trees; full host network state via `neutron.PurgeRealModeNetworkState`). This file is the point-in-time design; `spec.md` is authoritative.

## Summary

The destructive half of SPEC-008's uninstall, gated behind the existing `dataRetentionPolicy: delete` value. When `delete` is selected, `helm uninstall` runs a **blocking pre-delete hook** that purges everything the release created and leaves a **pristine baseline** for an immediate redeploy. `retain` (default) is unchanged — Helm removes only the Kubernetes workloads.

Unlike SPEC-008 (a chart with *no* product code changes), this feature has **two surfaces**:

1. **Product (Go)** — a new `lightstack cleanup` subcommand backed by a small `internal/teardown` package that **reuses the existing per-resource deletes** (`VMManager.DeleteVM`, `NetworkNamespaceManager.DeleteNamespace`, `RouterManager.DeleteRouterNamespace`, bridge/TAP/volume/image deletes) and the `database.MigrateReset()` library function, enumerating what to purge from the database. Plus the **FR-020 fix**: Cinder local volumes currently write to `~/.lightstack/volumes` (off the chart's `hostPath` mount); they move under the configured host storage path, mirroring Glance.
2. **Chart (Helm)** — pre-delete hook Jobs + a release-scoped RBAC triad, rendered only when `dataRetentionPolicy: delete`.

**Technical approach**: the binary stays **Kubernetes-API-free** (preserving lightstack's deliberate K8s-agnosticism — SPEC-008 FR-009c). The teardown is split by responsibility across ordered pre-delete hooks: a **kubectl-image Job** (operator-supplied, no implicit pull) handles the K8s-API steps — scale the Deployment to 0, wait for pods to terminate (the quiesce of FR-019), then delete release-owned PVCs; a **privileged `lightstack cleanup` Job** (same image + host mounts as a real-mode instance) does the host purge (libvirt domains, netns/routes/bridges/TAP/iptables/VXLAN), the node-local storage purge, and the DB schema reset **last**. The purge Job is the blocking, fail-closed gate (`activeDeadlineSeconds` bounds it — FR-016); `helm uninstall --no-hooks` is the documented escape hatch (C5). Single-node is the validated MVP; multi-node host purge is **deferred to issue #52** (FR-014, SC-007) — the pointer to the needed multi-node teardown work is already posted there ([#52 comment](https://github.com/cobaltcore-dev/lightstack/issues/52#issuecomment-4706996521)).

## Technical Context

**Language/Version**: Go 1.26 (the existing single binary — new `cleanup` subcommand + `internal/teardown` package) **and** a Helm 3 chart (Go templating) extending `charts/lightstack/`
**Primary Dependencies**: Helm ≥3.12; Kubernetes ≥1.25; the existing Lightstack image (privileged real-mode pod for host purge); an operator-supplied **kubectl image** (required only when `dataRetentionPolicy: delete`; no implicit pull — FR-018 pattern). **No new Go module dependency** — `internal/teardown` reuses `pkg/hypervisor`, `pkg/networking`, `pkg/storage`, `internal/database`; the binary gains **no Kubernetes client** (deliberate)
**Storage**: external PostgreSQL (reset via `database.MigrateReset()` on `delete`); node-local `hostPath` (default `/var/lib/lightstack`, purged); networked RBD/S3 left operator-owned (C4, not purged)
**Testing**: Go unit tests for `internal/teardown` (stub mode — enumeration, ordering, idempotency, DB-unreachable fallback); helm-unittest (hooks + RBAC render on `delete`, **absent** on `retain`; least-privilege RBAC); a single-node scenario test (`test/helm_teardown_test.sh`: `delete` purge → assert zero residue → immediate reinstall → identity check) extending `test/helm_upgrade_uninstall_test.sh`
**Target Platform**: vanilla Kubernetes + k3s (OpenShift via SCC, documented); Linux worker node for real-mode host purge
**Project Type**: Deployment-artifact feature (Helm chart) **plus** a minimal product CLI/cleanup change over the existing single-binary modular monolith
**Performance Goals**: SC-002 — an install immediately after a `delete` teardown reaches a healthy identity endpoint within the first-install budget (SPEC-008 SC-001), no collisions/residual-state errors; teardown bounded by `teardown.timeoutSeconds` (default 600, FR-016)
**Constraints**: destructive path strictly opt-in (`delete`); `retain` must not regress SPEC-008 SC-006; binary remains K8s-API-free and standalone-runnable (the `cleanup` subcommand's K8s steps live in the chart's kubectl Job, not the binary); fail-closed blocking with a bounded timeout and a `--no-hooks` escape; release-scoped (FR-007); idempotent (FR-008)
**Scale/Scope**: ~3 new chart templates (RBAC, quiesce+PVC Job, purge Job) + new `teardown.*` values; 1 new Go package (`internal/teardown`) + a `cleanup` subcommand in `cmd/lightstack/main.go`; 1 Go fix (`pkg/storage/ceph.go`)

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

| Principle | Assessment |
|-----------|------------|
| **I. Flamingo API compatibility (NON-NEGOTIABLE)** | PASS — teardown adds no API surface and changes no endpoint; `lightstack cleanup` is an operational subcommand, not an HTTP route. Existing CLI/Terraform/contract suites unaffected. |
| **II. Synchronous operations** | PASS — the cleanup is a synchronous batch (enumerate → delete → reset), no queues/async/state-machine. The hook blocks until it completes (FR-001). |
| **III. Fail-fast design** | PASS — fail-closed with a bounded `activeDeadlineSeconds` (FR-016); a missing artifact is success (idempotent, FR-008); an unreachable DB is ignored, not retried/hung (C9). |
| **IV. Multi-mode architecture** | PASS — `lightstack cleanup` works in stub (DB reset + no-op host purge — CI-testable, unprivileged) and real (full host purge). Stub is the validated default. |
| **V. Project isolation** | PASS — the purge is release-scoped and enumerates per-project netns/domains; touches no co-resident release (FR-007). |
| **VI. Single binary deployment** | PASS — `cleanup` is a subcommand of the existing `lightstack` binary (FR-010), reusing its packages; **no Kubernetes client added** → binary stays lean and standalone-runnable. K8s-API steps live in the chart's kubectl hook Job, not the binary. |
| Tech constraint: *"no container orchestration required"* | PASS (additive) — the Helm hooks are k8s-only sugar; `lightstack cleanup` runs standalone (Compose/bare-metal) doing host+DB purge without any k8s. |
| Tech constraint: *no message queues / async workers / state machines* | PASS — none introduced. |
| DB migrations reversible | N/A — adds no migration; reuses `MigrateReset` (drop + re-`Up`) on existing migrations. |
| SPEC-008 FR-009c (*product must not require the K8s API*) | UPHELD — the binary gains no K8s client; the chart's kubectl Job (not the product) performs scale/PVC-delete. |

**Result**: All gates PASS. The one judgment call (K8s-API-free binary + a kubectl hook Job vs. embedding `client-go`) is resolved in `research.md` (Decision R1) in favor of the K8s-free binary; logged in Complexity Tracking.

**Post-design re-check (after Phase 1)**: Still PASS. The design upholds every gate — R1 keeps the binary K8s-free (VI, FR-009c), R3/R9 keep the purge synchronous and fail-fast (II/III), R8 keeps stub-mode coverage (IV), R7 keeps RBAC least-privilege and release-scoped (V). No new violations; Complexity Tracking unchanged.

## Project Structure

### Documentation (this feature)

```text
docs/specs/010-helm-teardown/
├── spec.md              # Feature spec (9 clarifications, 3 rounds, 20 FRs)
├── plan.md              # This file
├── research.md          # Phase 0 — the design decisions (R1–R9)
├── data-model.md        # Phase 1 — new values + enumerated entities + rendered resources
├── quickstart.md        # Phase 1 — operator runbook: delete teardown + pristine-redeploy verify
├── contracts/           # Phase 1
│   ├── cli-cleanup.md       # `lightstack cleanup` command contract (flags, exit codes, modes)
│   ├── teardown-hooks.md    # rendered pre-delete Job + RBAC resource contract
│   └── values.md            # new teardown.* values contract
└── checklists/          # (later — /speckit.checklist)
```

### Source Code (repository root)

```text
cmd/lightstack/main.go                       # add a `cleanup` subcommand dispatch ahead of the server path
internal/teardown/
├── teardown.go                       # NEW — orchestration: order, release-scoped enumeration from DB,
│                                     #       host/storage purge, MigrateReset last, fail-closed + idempotent,
│                                     #       DB-unreachable host-prefix fallback
└── teardown_test.go                  # NEW — stub-mode unit tests (enumeration, ordering, idempotency)
pkg/storage/ceph.go                   # FR-020 — Cinder local path → <hostStoragePath>/volumes (mirror Glance)
internal/database/migrate.go          # reused (MigrateReset) — no change

charts/lightstack/
├── values.yaml                       # expand dataRetentionPolicy doc + new teardown.* block
├── values.schema.json                # teardown.* schema; kubectlImage required-when-delete gate
├── templates/
│   ├── teardown-rbac.yaml            # NEW — SA + Role + RoleBinding (pre-delete hook, weight -20, gated on delete)
│   ├── teardown-quiesce-job.yaml     # NEW — kubectl Job: scale Deployment→0, wait, delete release PVCs (weight -10)
│   ├── teardown-purge-job.yaml       # NEW — privileged lightstack cleanup Job: host+storage+DB reset (weight 0, blocking)
│   └── _helpers.tpl                  # add teardown names/labels + kubectl-image helper
├── tests/
│   └── teardown_test.yaml            # NEW — helm-unittest: render-on-delete / absent-on-retain; RBAC least-privilege
└── README.md                         # FR-018 — reconcile the Uninstall section (supersede "clean manually when delete")

test/
└── helm_teardown_test.sh             # NEW — single-node delete purge + pristine redeploy scenario

docs/specs/008-helm-charts/           # FR-018 — reconcile the deferred-purge descriptions with what ships
├── spec.md                           #   FR-014: purge now also quiesces + resets the DB schema
├── data-model.md                     #   uninstall / dataRetentionPolicy lifecycle entry (delete path)
├── tasks.md                          #   T027/T029 deferral notes → "done in #53"
└── quickstart.md                     #   Uninstall section: add the `delete` path
```

**Structure Decision**: Extend the existing `charts/lightstack/` chart (no new chart) and the existing `lightstack` binary (no new binary — a subcommand, FR-010). The destructive behavior is rendered only under `dataRetentionPolicy: delete`, so `retain` installs are byte-for-byte unchanged (SC-004). Multi-node host-purge templates/validation are **not** authored here — deferred to issue #52 (FR-014); single-node is the validated done-line, mirroring SPEC-008's gating.

## Complexity Tracking

| Item | Why needed | Simpler alternative rejected because |
|------|------------|--------------------------------------|
| Two hook Jobs (kubectl + privileged lightstack) instead of one | The K8s-API steps (scale, PVC delete) and the host/DB purge need different images/privileges; splitting keeps the binary K8s-free (R1) | One Job embedding `client-go` in the Lightstack binary → couples the product to the K8s API (against SPEC-008 FR-009c) and bloats the ~35MB binary |
| A required, operator-supplied `kubectlImage` when `delete` | Lets the chart drive scale/PVC-delete without a K8s client in the product, matching the chart's "no implicit image pull" posture (FR-018) | Bundling kubectl into the Lightstack image → bloats every deployment for a teardown-only need |
| New `internal/teardown` package + `lightstack cleanup` subcommand | FR-010 (cleanup is an operation of the existing binary); reuses existing deletes + `MigrateReset` | A standalone teardown script shelling per-resource → no reuse of the tested delete paths; drift risk |
| FR-020 Cinder-path change ships in this feature | The storage purge is meaningless if local volumes sit off-mount; also fixes a latent SPEC-008 FR-012a persistence defect | Deferring it → teardown's storage purge can't find Cinder volumes; redeploy not pristine for volume data |
