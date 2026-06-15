# Data Model: Teardown via Helm Pre-Delete Hooks (Phase 1)

**Feature**: SPEC-009 | **Date**: 2026-06-15 | **Input**: [spec.md](./spec.md), [research.md](./research.md)

This feature adds no database tables. Its "data model" is three things: (1) the new/extended **configuration values**, (2) the **entities the cleanup enumerates and purges**, and (3) the **Kubernetes resources the chart renders** for teardown.

---

## 1. Configuration values (`charts/o3k/values.yaml`)

### Existing — semantics finalized here

| Value | Type | Default | Applies | Notes |
|-------|------|---------|---------|-------|
| `dataRetentionPolicy` | enum `retain`\|`delete` | `retain` | all | **The master gate.** `retain` = SPEC-008 behavior (remove K8s workloads only; no hooks rendered). `delete` = render the pre-delete teardown hooks (FR-002). Irreversible warning documented (FR-018). |

### New — `teardown.*` (effective only when `dataRetentionPolicy: delete`)

| Value | Type | Default | Required | Notes |
|-------|------|---------|----------|-------|
| `teardown.kubectlImage` | string | `""` | **yes, when `delete`** | Operator-supplied kubectl image for the quiesce/PVC Job (R1). No implicit pull — template `fail`s if empty under `delete` (FR-018 pattern). |
| `teardown.timeoutSeconds` | int | `600` | no | Purge Job `activeDeadlineSeconds` — bounds the blocking hook (FR-016). |
| `teardown.deletePVCs` | bool | `true` | no | Delete release-owned PVCs during teardown (FR-005). PVCs are "if present" today (chart uses `hostPath`). |

**Validation (`values.schema.json`)**: `dataRetentionPolicy ∈ {retain, delete}`; `teardown.timeoutSeconds` integer ≥ 1; `teardown.kubectlImage` string. The "required when `delete`" check is a template `fail` (schema can't express conditional-required cleanly), mirroring the chart's existing image/secret fail-fast gates.

**Not added** (decided in spec): no networked-backend (RBD/S3) purge toggle (C4 — operator-owned); no `hostPath`→PVC conversion (C3); no multi-node host-purge values (C2/#52).

---

## 2. Enumerated entities (what `o3k cleanup` purges)

The cleanup reads these from the database (source of truth), maps each to its host artifact, and deletes via the existing per-resource functions. Names/locations are the product's verified conventions.

| Entity | DB source | Host artifact / locator | Delete via | Class |
|--------|-----------|-------------------------|------------|-------|
| **Instance domain** | `instances.libvirt_domain_id` (the domain UUID `DeleteVM` parses) | libvirt domain (host `libvirtd`) | `VMManager.DeleteVM(uuid)` (destroy + undefine) | compute |
| **Project namespace** | `networks.project_id` | netns `light-ns-<project_id>` | `NetworkNamespaceManager.DeleteNamespace` | network |
| **Router namespace** | `routers.id` | netns `qrouter-<router_id[:11]>` | `RouterManager.DeleteRouterNamespace` | network |
| **Bridge / TAP** | `ports` / network records | host/in-ns links | `BridgeManager.DeleteBridge`, `TAPDeviceManager.DeleteTAPDevice` | network |
| **Routes / iptables / VXLAN** | router/SNAT/FIP/VXLAN records | host netfilter & overlay ifaces | existing router/VXLAN teardown calls | network |
| **Local volume file** | `volumes` (local-backed) | `<hostStoragePath>/volumes/volume-<id>.qcow2` (FR-020) | file delete (`CephClient.DeleteVolume` local path) | storage |
| **Local image file** | `images` (local-backed) | `<hostStoragePath>/images/<id>` | file delete (`ImageStore.DeleteImage` local path) | storage |
| **Orphaned storage** | *absence* of a DB record | file under `<hostStoragePath>/{volumes,images}` with no matching row | file delete (FR-006) | storage |
| **Release PVC** | n/a (K8s) | PVCs labeled `app.kubernetes.io/instance=<release>` | kubectl Job `delete pvc -l …` | storage (K8s) |
| **DB schema** | n/a | the release's configured database | `database.MigrateReset` (last) | database |

**Release scoping (FR-007)**: each release owns its database, so "everything in this DB" *is* this release's set; netns/domain names derive from that DB's project/router ids. The purge never wildcards across a co-resident release (R3); the node-local storage tree under `hostStoragePath` is single-release by FR-003b (real releases can't co-reside on a node; stub has no `hostPath`), so its orphan sweep is release-safe. DB-unreachable fallback enumerates host artifacts by the `light-ns-`/`qrouter-` prefixes, the libvirt domain list, and a wholesale purge of `hostStoragePath/{volumes,images}` (C9).

---

## 3. Rendered Kubernetes resources (chart)

All carry `helm.sh/hook: pre-delete` and render **only when `dataRetentionPolicy: delete`**. Names are release-scoped via `o3k.fullname` (e.g. `<release>-o3k-teardown-*`); labels reuse `o3k.labels`.

| Resource | Hook weight | Purpose | Key spec |
|----------|------------:|---------|----------|
| **ServiceAccount** `…-teardown` | -20 | identity for the kubectl Job | — |
| **Role** `…-teardown` | -20 | least-privilege (R7): `deployments` get/list/patch; `pods` get/list; `persistentvolumeclaims` get/list/delete | namespaced |
| **RoleBinding** `…-teardown` | -20 | bind SA → Role | namespaced |
| **Job** `…-teardown-quiesce` | -10 | `kubectlImage`; scale Deployment→0, `wait --for=delete pod`, then `delete pvc -l instance=<release>` (if `deletePVCs`) | `backoffLimit: 0` |
| **Job** `…-teardown-purge` | 0 | o3k image, privileged + host mounts + config Secret; runs `o3k cleanup`; **blocking, fail-closed** | `activeDeadlineSeconds: {{ timeoutSeconds }}`, `backoffLimit: 0`, `restartPolicy: Never` |

`hook-delete-policy: before-hook-creation,hook-succeeded` on all (re-runnable; succeeded hooks are cleaned up; a failed purge Job is retained for its logs).

### State / sequence

```text
helm uninstall (dataRetentionPolicy: delete)
        │  (workload + Service + Secret still live)
        ▼
[-20] RBAC created
[-10] quiesce Job:  scale Deployment→0 ─► wait pods gone ─► delete release PVCs
[  0] purge Job (o3k cleanup, blocking):
          enumerate from DB ─► compute ─► network ─► storage ─► DB reset (last)
          (DB unreachable → ignore DB; host-prefix enumeration + wholesale hostStoragePath purge; skip reset)
          success ─────────────► hook passes
          any required class fails / timeout ─► non-zero ─► uninstall ABORTS (FR-001)
        ▼  (all pre-delete hooks succeeded)
Helm deletes the release's resources  ─►  pristine baseline (SC-001/002)
```

Escape hatch: `helm uninstall --no-hooks` skips the purge entirely (C5).

### `retain` (default) — unchanged

No teardown resources render. `helm uninstall` removes only the release workloads/Service/Secret/Ingress, exactly as SPEC-008 today (SC-004 / SC-006). No DB reset, no host purge, no PVC deletion.
